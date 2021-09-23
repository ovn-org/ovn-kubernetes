package cni

import (
	"fmt"
	"net"

	"github.com/containernetworking/cni/pkg/types/current"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
)

var (
	minRsrc           = resource.MustParse("1k")
	maxRsrc           = resource.MustParse("1P")
	BandwidthNotFound = &notFoundError{}
)

type direction int

func (d direction) String() string {
	if d == Egress {
		return "egress"
	}
	return "ingress"
}

const (
	Egress direction = iota
	Ingress
)

type notFoundError struct{}

func (*notFoundError) Error() string {
	return "not found"
}

func validateBandwidthIsReasonable(rsrc *resource.Quantity) error {
	if rsrc.Value() < minRsrc.Value() {
		return fmt.Errorf("resource is unreasonably small (< 1kbit)")
	}
	if rsrc.Value() > maxRsrc.Value() {
		return fmt.Errorf("resoruce is unreasonably large (> 1Pbit)")
	}
	return nil
}

func extractPodBandwidth(podAnnotations map[string]string, dir direction) (int64, error) {
	annotation := "kubernetes.io/ingress-bandwidth"
	if dir == Egress {
		annotation = "kubernetes.io/egress-bandwidth"
	}

	str, found := podAnnotations[annotation]
	if !found {
		return 0, BandwidthNotFound
	}
	bwVal, err := resource.ParseQuantity(str)
	if err != nil {
		return 0, err
	}
	if err := validateBandwidthIsReasonable(&bwVal); err != nil {
		return 0, err
	}
	return bwVal.Value(), nil
}

func (pr *PodRequest) String() string {
	return fmt.Sprintf("[%s/%s %s]", pr.PodNamespace, pr.PodName, pr.SandboxID)
}

// checkOrUpdatePodUID validates the given pod UID against the request's existing
// pod UID. If the existing UID is empty the runtime did not support passing UIDs
// and the best we can do is use the given UID for the duration of the request.
// But if the existing UID is valid and does not match the given UID then the
// sandbox request is for a different pod instance and should be terminated.
func (pr *PodRequest) checkOrUpdatePodUID(podUID string) error {
	if pr.PodUID == "" {
		// Runtime didn't pass UID, use the one we got from the pod object
		pr.PodUID = podUID
	} else if podUID != pr.PodUID {
		// Exit early if the pod was deleted and recreated already
		return fmt.Errorf("pod deleted before sandbox %v operation began", pr.Command)
	}
	return nil
}

func (pr *PodRequest) cmdAdd(kubeAuth *KubeAPIAuth, podLister corev1listers.PodLister, useOVSExternalIDs bool, kclient kubernetes.Interface) (*Response, error) {
	namespace := pr.PodNamespace
	podName := pr.PodName
	if namespace == "" || podName == "" {
		return nil, fmt.Errorf("required CNI variable missing")
	}

	kubecli := &kube.Kube{KClient: kclient}
	annotCondFn := isOvnReady

	if pr.IsSmartNIC {
		// Add Smart-NIC connection-details annotation so ovnkube-node running on smart-NIC
		// performs the needed network plumbing.
		if err := pr.addSmartNICConnectionDetailsAnnot(kubecli); err != nil {
			return nil, err
		}
		annotCondFn = isSmartNICReady
	}
	// Get the IP address and MAC address of the pod
	// for Smart-Nic, ensure connection-details is present
	podUID, annotations, err := GetPodAnnotations(pr.ctx, podLister, kclient, namespace, podName, annotCondFn)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod annotation: %v", err)
	}
	if err := pr.checkOrUpdatePodUID(podUID); err != nil {
		return nil, err
	}

	podInterfaceInfo, err := PodAnnotation2PodInfo(annotations, useOVSExternalIDs, pr.IsSmartNIC)
	if err != nil {
		return nil, err
	}

	response := &Response{KubeAuth: kubeAuth}
	if !config.UnprivilegedMode {
		response.Result, err = pr.getCNIResult(podLister, kclient, podInterfaceInfo)
		if err != nil {
			return nil, err
		}
	} else {
		response.PodIFInfo = podInterfaceInfo
	}

	return response, nil
}

func (pr *PodRequest) cmdDel() error {
	if pr.IsSmartNIC {
		// nothing to do
		return nil
	}

	if err := pr.PlatformSpecificCleanup(); err != nil {
		return err
	}
	return nil
}

func (pr *PodRequest) cmdCheck(podLister corev1listers.PodLister, useOVSExternalIDs bool, kclient kubernetes.Interface) error {
	// noop...CMD check is not considered useful, and has a considerable performance impact
	// to pod bring up times with CRIO. This is due to the fact that CRIO currently calls check
	// after CNI ADD before it finishes bringing the container up
	return nil
}

// HandleCNIRequest is the callback for all the requests
// coming to the cniserver after being processed into PodRequest objects
// Argument '*PodRequest' encapsulates all the necessary information
// kclient is passed in so that clientset can be reused from the server
// Return value is the actual bytes to be sent back without further processing.
func HandleCNIRequest(request *PodRequest, podLister corev1listers.PodLister, useOVSExternalIDs bool, kclient kubernetes.Interface, kubeAuth *KubeAPIAuth) ([]byte, error) {
	var result, resultForLogging []byte
	var response *Response
	var err, err1 error

	klog.Infof("%s %s starting CNI request %+v", request, request.Command, request)
	switch request.Command {
	case CNIAdd:
		response, err = request.cmdAdd(kubeAuth, podLister, useOVSExternalIDs, kclient)
	case CNIDel:
		err = request.cmdDel()
	case CNICheck:
		err = request.cmdCheck(podLister, useOVSExternalIDs, kclient)
	default:
	}

	if response != nil {
		if result, err1 = response.Marshal(); err1 != nil {
			return nil, fmt.Errorf("%s %s CNI request %+v failed to marshal result: %v",
				request, request.Command, request, err)
		}
		if resultForLogging, err1 = response.MarshalForLogging(); err1 != nil {
			klog.Errorf("%s %s CNI request %+v, %v", request, request.Command, request, err1)
		}
	}

	klog.Infof("%s %s finished CNI request %+v, result %q, err %v",
		request, request.Command, request, string(resultForLogging), err)

	if err != nil {
		// Prefix errors with request info for easier failure debugging
		return nil, fmt.Errorf("%s %v", request, err)
	}
	return result, nil
}

// getCNIResult get result from pod interface info.
func (pr *PodRequest) getCNIResult(podLister corev1listers.PodLister, kclient kubernetes.Interface, podInterfaceInfo *PodInterfaceInfo) (*current.Result, error) {
	interfacesArray, err := pr.ConfigureInterface(podLister, kclient, podInterfaceInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to configure pod interface: %v", err)
	}

	gateways := map[string]net.IP{}
	for _, gw := range podInterfaceInfo.Gateways {
		if gw.To4() != nil && gateways["4"] == nil {
			gateways["4"] = gw
		} else if gw.To4() == nil && gateways["6"] == nil {
			gateways["6"] = gw
		}
	}

	// Build the result structure to pass back to the runtime
	ips := []*current.IPConfig{}
	for _, ipcidr := range podInterfaceInfo.IPs {
		ip := &current.IPConfig{
			Interface: current.Int(1),
			Address:   *ipcidr,
		}
		if utilnet.IsIPv6CIDR(ipcidr) {
			ip.Version = "6"
		} else {
			ip.Version = "4"
		}
		ip.Gateway = gateways[ip.Version]
		ips = append(ips, ip)
	}

	return &current.Result{
		Interfaces: interfacesArray,
		IPs:        ips,
	}, nil
}

package cni

import (
	"encoding/json"
	"fmt"
	"net"

	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/pkg/errors"
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

func (pr *PodRequest) cmdAdd(podLister corev1listers.PodLister, useOVSExternalIDs bool, kclient kubernetes.Interface) ([]byte, error) {
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
	annotations, err := GetPodAnnotations(pr.ctx, podLister, kclient, namespace, podName, annotCondFn)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod annotation: %v", err)
	}

	podInterfaceInfo, err := PodAnnotation2PodInfo(annotations, useOVSExternalIDs, pr.IsSmartNIC)
	if err != nil {
		return nil, err
	}

	response := &Response{}
	if !config.UnprivilegedMode {
		response.Result, err = pr.getCNIResult(podInterfaceInfo)
		if err != nil {
			return nil, err
		}
	} else {
		response.PodIFInfo = podInterfaceInfo
	}

	responseBytes, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal pod request response: %v", err)
	}

	return responseBytes, nil
}

func (pr *PodRequest) cmdDel() ([]byte, error) {
	if pr.IsSmartNIC {
		// nothing to do
		return []byte{}, nil
	}

	if err := pr.PlatformSpecificCleanup(); err != nil {
		return nil, err
	}
	return []byte{}, nil
}

func (pr *PodRequest) cmdCheck(podLister corev1listers.PodLister, useOVSExternalIDs bool, kclient kubernetes.Interface) ([]byte, error) {
	namespace := pr.PodNamespace
	podName := pr.PodName
	if namespace == "" || podName == "" {
		return nil, fmt.Errorf("required CNI variable missing")
	}

	// Get the IP address and MAC address of the pod
	annotCondFn := isOvnReady
	if pr.IsSmartNIC {
		annotCondFn = isSmartNICReady
	}
	annotations, err := GetPodAnnotations(pr.ctx, podLister, kclient, pr.PodNamespace, pr.PodName, annotCondFn)
	if err != nil {
		return nil, err
	}

	if pr.CNIConf.PrevResult != nil {
		result, err := current.NewResultFromResult(pr.CNIConf.PrevResult)
		if err != nil {
			return nil, fmt.Errorf("could not convert result to current version: %v", err)
		}
		hostIfaceName := ""
		for _, interf := range result.Interfaces {
			if len(interf.Sandbox) == 0 {
				hostIfaceName = interf.Name
				break
			}
		}
		if len(hostIfaceName) == 0 {
			return nil, fmt.Errorf("could not find host interface in the prevResult: %v", result)
		}
		ifaceID := fmt.Sprintf("%s_%s", namespace, podName)
		ofPort, err := getIfaceOFPort(hostIfaceName)
		if err != nil {
			return nil, err
		}
		for _, ip := range result.IPs {
			if err = waitForPodInterface(pr.ctx, result.Interfaces[*ip.Interface].Mac, []*net.IPNet{&ip.Address},
				hostIfaceName, ifaceID, ofPort, useOVSExternalIDs); err != nil {
				return nil, fmt.Errorf("error while waiting on OVN pod interface: %s ip: %v, error: %v", ifaceID, ip, err)
			}
		}

		for _, direction := range []direction{Ingress, Egress} {
			annotationBandwith, annotationErr := extractPodBandwidth(annotations, direction)
			ovnBandwith, ovnErr := getOvsPortBandwidth(hostIfaceName, direction)
			if errors.Is(annotationErr, BandwidthNotFound) && errors.Is(ovnErr, BandwidthNotFound) {
				continue
			}
			if annotationErr != nil {
				return nil, errors.Wrapf(err, "Failed to get bandwith from annotations of pod %s %s", podName, direction)
			}
			if ovnErr != nil {
				return nil, errors.Wrapf(err, "Failed to get pod %s %s bandwith from ovn", direction, podName)
			}
			if annotationBandwith != ovnBandwith {
				return nil, fmt.Errorf("defined %s bandwith restriction %d is not equal to the set one %d", direction, annotationBandwith, ovnBandwith)
			}
		}
	}
	return []byte{}, nil
}

// HandleCNIRequest is the callback for all the requests
// coming to the cniserver after being procesed into PodRequest objects
// Argument '*PodRequest' encapsulates all the necessary information
// kclient is passed in so that clientset can be reused from the server
// Return value is the actual bytes to be sent back without further processing.
func HandleCNIRequest(request *PodRequest, podLister corev1listers.PodLister, useOVSExternalIDs bool, kclient kubernetes.Interface) ([]byte, error) {
	var result []byte
	var err error

	klog.Infof("%s %s starting CNI request %+v", request, request.Command, request)
	switch request.Command {
	case CNIAdd:
		result, err = request.cmdAdd(podLister, useOVSExternalIDs, kclient)
	case CNIDel:
		result, err = request.cmdDel()
	case CNICheck:
		result, err = request.cmdCheck(podLister, useOVSExternalIDs, kclient)
	default:
	}
	klog.Infof("%s %s finished CNI request %+v, result %q, err %v", request, request.Command, request, string(result), err)

	if err != nil {
		// Prefix errors with request info for easier failure debugging
		return nil, fmt.Errorf("%s %v", request, err)
	}
	return result, nil
}

// getCNIResult get result from pod interface info.
func (pr *PodRequest) getCNIResult(podInterfaceInfo *PodInterfaceInfo) (*current.Result, error) {
	interfacesArray, err := pr.ConfigureInterface(pr.PodNamespace, pr.PodName, podInterfaceInfo)
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

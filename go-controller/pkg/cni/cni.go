package cni

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/containernetworking/cni/pkg/types/current"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var minRsrc = resource.MustParse("1k")
var maxRsrc = resource.MustParse("1P")

func validateBandwidthIsReasonable(rsrc *resource.Quantity) error {
	if rsrc.Value() < minRsrc.Value() {
		return fmt.Errorf("resource is unreasonably small (< 1kbit)")
	}
	if rsrc.Value() > maxRsrc.Value() {
		return fmt.Errorf("resoruce is unreasonably large (> 1Pbit)")
	}
	return nil
}

func extractPodBandwidthResources(podAnnotations map[string]string) (int64, int64, error) {
	ingress := int64(-1)
	egress := int64(-1)
	str, found := podAnnotations["kubernetes.io/ingress-bandwidth"]
	if found {
		ingressVal, err := resource.ParseQuantity(str)
		if err != nil {
			return -1, -1, err
		}
		if err := validateBandwidthIsReasonable(&ingressVal); err != nil {
			return -1, -1, err
		}
		ingress = ingressVal.Value()
	}
	str, found = podAnnotations["kubernetes.io/egress-bandwidth"]
	if found {
		egressVal, err := resource.ParseQuantity(str)
		if err != nil {
			return -1, -1, err
		}
		if err := validateBandwidthIsReasonable(&egressVal); err != nil {
			return -1, -1, err
		}
		egress = egressVal.Value()
	}
	return ingress, egress, nil
}

func (pr *PodRequest) cmdAdd() ([]byte, error) {
	namespace := pr.PodNamespace
	podName := pr.PodName
	if namespace == "" || podName == "" {
		return nil, fmt.Errorf("required CNI variable missing (namespace: %q, name %q)", namespace, podName)
	}

	clientset, err := util.NewClientset(&config.Kubernetes)
	if err != nil {
		return nil, fmt.Errorf("Could not create kubernetes clientset: %v", err)
	}
	kubecli := &kube.Kube{KClient: clientset}

	// Get the IP address and MAC address from the API server.
	// Exponential back off ~32 seconds + 7* t(api call)
	var annotationBackoff = wait.Backoff{Duration: 1 * time.Second, Steps: 7, Factor: 1.5, Jitter: 0.1}
	var annotations map[string]string
	var ovnAnnotation string
	if err = wait.ExponentialBackoff(annotationBackoff, func() (bool, error) {
		annotations, err = kubecli.GetAnnotationsOnPod(namespace, podName)
		if err != nil {
			// TODO: check if err is non recoverable
			logrus.Warningf("error getting pod annotations: %v", err)
			return false, nil
		}
		if ovnAnnotation = annotations["ovn"]; ovnAnnotation != "" {
			return true, nil
		}
		return false, nil
	}); err != nil {
		return nil, fmt.Errorf("failed to get pod annotation: %v", err)
	}

	podInfo, err := util.UnmarshalPodAnnotation(ovnAnnotation)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal ovn annotation: %v", err)
	}

	ingress, egress, err := extractPodBandwidthResources(annotations)
	if err != nil {
		return nil, fmt.Errorf("failed to parse bandwidth request: %v", err)
	}
	podInterfaceInfo := &PodInterfaceInfo{
		PodAnnotation: *podInfo,
		MTU:           config.Default.MTU,
		Ingress:       ingress,
		Egress:        egress,
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
	if err := pr.PlatformSpecificCleanup(); err != nil {
		return nil, err
	}
	return []byte{}, nil
}

// HandleCNIRequest is the callback for all the requests
// coming to the cniserver after being procesed into PodRequest objects
// Argument '*PodRequest' encapsulates all the necessary information
// Return value is the actual bytes to be sent back without further processing.
func HandleCNIRequest(request *PodRequest) ([]byte, error) {
	logrus.Infof("[%s/%s] dispatching pod network request %v", request.PodNamespace, request.PodName, request)
	var result []byte
	var err error
	switch request.Command {
	case CNIAdd:
		result, err = request.cmdAdd()
	case CNIDel:
		result, err = request.cmdDel()
	default:
	}
	logrus.Infof("[%s/%s] CNI request %v, result %q, err %v", request.PodNamespace, request.PodName, request, string(result), err)
	return result, err
}

// getCNIResult get result from pod interface info.
func (pr *PodRequest) getCNIResult(podInterfaceInfo *PodInterfaceInfo) (*current.Result, error) {
	interfacesArray, err := pr.ConfigureInterface(pr.PodNamespace, pr.PodName, podInterfaceInfo)
	if err != nil {
		return nil, fmt.Errorf("Failed to configure interface in pod: %v", err)
	}

	// Build the result structure to pass back to the runtime
	ipVersion := "6"
	if podInterfaceInfo.IP.IP.To4() != nil {
		ipVersion = "4"
	}
	return &current.Result{
		Interfaces: interfacesArray,
		IPs: []*current.IPConfig{
			{
				Version:   ipVersion,
				Interface: current.Int(1),
				Address:   *podInterfaceInfo.IP,
				Gateway:   podInterfaceInfo.GW,
			},
		},
	}, nil
}

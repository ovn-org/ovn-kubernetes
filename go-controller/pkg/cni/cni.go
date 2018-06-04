package cni

import (
	"encoding/json"
	"net"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/containernetworking/cni/pkg/types/current"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
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

func (pr *PodRequest) cmdAdd() *PodResult {
	namespace := pr.PodNamespace
	podName := pr.PodName
	if namespace == "" || podName == "" {
		logrus.Errorf("required CNI variable missing")
		return nil
	}

	clientset, err := util.NewClientset(&config.Kubernetes)
	if err != nil {
		logrus.Errorf("Could not create clientset for kubernetes: %v", err)
		return nil
	}
	kubecli := &kube.Kube{KClient: clientset}

	// Get the IP address and MAC address from the API server.
	// Exponential back off ~32 seconds + 7* t(api call)
	var annotationBackoff = wait.Backoff{Duration: 1 * time.Second, Steps: 7, Factor: 1.5, Jitter: 0.1}
	var annotation map[string]string
	if err = wait.ExponentialBackoff(annotationBackoff, func() (bool, error) {
		annotation, err = kubecli.GetAnnotationsOnPod(namespace, podName)
		if err != nil {
			// TODO: check if err is non recoverable
			logrus.Warningf("Error while obtaining pod annotations - %v", err)
			return false, nil
		}
		if _, ok := annotation["ovn"]; ok {
			return true, nil
		}
		return false, nil
	}); err != nil {
		logrus.Errorf("failed to get pod annotation - %v", err)
		return nil
	}

	ovnAnnotation, ok := annotation["ovn"]
	if !ok {
		logrus.Errorf("failed to get ovn annotation from pod")
		return nil
	}

	var ovnAnnotatedMap map[string]string
	err = json.Unmarshal([]byte(ovnAnnotation), &ovnAnnotatedMap)
	if err != nil {
		logrus.Errorf("unmarshal ovn annotation failed")
		return nil
	}

	ipAddress := ovnAnnotatedMap["ip_address"]
	macAddress := ovnAnnotatedMap["mac_address"]
	gatewayIP := ovnAnnotatedMap["gateway_ip"]

	if ipAddress == "" || macAddress == "" || gatewayIP == "" {
		logrus.Errorf("failed in pod annotation key extract")
		return nil
	}

	ingress, egress, err := extractPodBandwidthResources(annotation)
	if err != nil {
		return fmt.Errorf("failed to parse bandwidth request: %v", err)
	}

	var interfacesArray []*current.Interface
	interfacesArray, err = pr.ConfigureInterface(namespace, podName, macAddress, ipAddress, gatewayIP, config.Default.MTU, ingress, egress)
	if err != nil {
		return nil
	}

	// Build the result structure to pass back to the runtime
	addr, addrNet, err := net.ParseCIDR(ipAddress)
	if err != nil {
		logrus.Errorf("failed to parse IP address %q: %v", ipAddress, err)
		return nil
	}
	ipVersion := "6"
	if addr.To4() != nil {
		ipVersion = "4"
	}
	result := &current.Result{
		Interfaces: interfacesArray,
		IPs: []*current.IPConfig{
			{
				Version:   ipVersion,
				Interface: current.Int(1),
				Address:   net.IPNet{IP: addr, Mask: addrNet.Mask},
				Gateway:   net.ParseIP(gatewayIP),
			},
		},
	}

	podResult := &PodResult{}
	versionedResult, _ := result.GetAsVersion(pr.CNIVersion)
	podResult.Response, _ = json.Marshal(versionedResult)
	return podResult
}

func (pr *PodRequest) cmdDel() *PodResult {
	pr.PlatformSpecificCleanup()
	return &PodResult{}
}

func HandleCNIRequest(request *PodRequest) ([]byte, error) {
	logrus.Infof("Dispatching pod network request %v", request)
	var result *PodResult
	switch request.Command {
	case CNI_ADD:
		result = request.cmdAdd()
	case CNI_DEL:
		result = request.cmdDel()
	default:
	}
	logrus.Infof("Returning pod network request %v, result %s err %v", request, string(result.Response), result.Err)
	return result.Response, result.Err
}

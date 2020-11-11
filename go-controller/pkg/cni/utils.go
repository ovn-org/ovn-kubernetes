package cni

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Mellanox/sriovnet"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const SmartNicConnectionDetails = "k8s.ovn.org/smartnic.connection-details"
const SmartNicConnetionStatus = "k8s.ovn.org/smartnic.connection-status"

func GetPodAnnotationsWithBackoff(kubecli kube.Interface, namespace string, podName string, isSmartNic bool) (annotations map[string]string, err error) {
	// Get the IP address and MAC address from the API server.
	// Exponential back off ~32 seconds + 7* t(api call)
	var annotationBackoff = wait.Backoff{Duration: 1 * time.Second, Steps: 7, Factor: 1.5, Jitter: 0.1}
	if err = wait.ExponentialBackoff(annotationBackoff, func() (bool, error) {
		annotations, err = kubecli.GetAnnotationsOnPod(namespace, podName)
		if err != nil {
			if errors.IsNotFound(err) {
				// Pod not found; don't bother waiting longer
				return false, err
			}
			klog.Warningf("Error getting pod annotations: %v", err)
			return false, nil
		}
		if _, ok := annotations[util.OvnPodAnnotationName]; ok {
			if isSmartNic {
				if _, ok := annotations[SmartNicConnetionStatus]; ok {
					return true, nil
				}
				return false, nil
			}
			return true, nil
		}
		return false, nil
	}); err != nil {
		return nil, fmt.Errorf("failed to get pod annotation: %v", err)
	}

	return annotations, nil
}

func PodAnnotation2PodInfo(podAnnotationMap map[string]string) (*PodInterfaceInfo, error) {
	podAnnotation, err := util.UnmarshalPodAnnotation(podAnnotationMap)
	if err != nil {
		return nil, err
	}
	ingress, egress, err := extractPodBandwidthResources(podAnnotationMap)
	if err != nil {
		return nil, err
	}
	podInterfaceInfo := &PodInterfaceInfo{
		PodAnnotation: *podAnnotation,
		MTU:           config.Default.MTU,
		Ingress:       ingress,
		Egress:        egress,
		IsSmartNic:    true,
	}
	return podInterfaceInfo, nil
}

//Move to sriovnet
func GetPfPciFromVfPci(vfPciAddress string) (string, error) {
	pfPath := filepath.Join(sriovnet.PciSysDir, vfPciAddress, "physfn")
	pciDevDir, err := os.Readlink(pfPath)
	if len(pciDevDir) <= 3 {
		return "", fmt.Errorf("could not find PCI Address")
	}
	return pciDevDir[3:], err
}

package cni

import (
	"context"
	"fmt"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// wait on a certain pod annotation related condition
type podAnnotWaitCond func(podAnnotation map[string]string) bool

// isOvnReady is a wait condition for OVN master to set pod-networks annotation
func isOvnReady(podAnnotation map[string]string) bool {
	if _, ok := podAnnotation[util.OvnPodAnnotationName]; ok {
		return true
	}
	return false
}

// isSmartNICReady is a wait condition smart-NIC: wait for OVN master to set pod-networks annotation and
// ovnkube running on Smart-NIC to set connection-status pod annotation and its status is Ready
func isSmartNICReady(podAnnotation map[string]string) bool {
	if isOvnReady(podAnnotation) {
		// check Smart-NIC connection status
		status := util.SmartNICConnectionStatus{}
		if err := status.FromPodAnnotation(podAnnotation); err == nil {
			if status.Status == util.SmartNicConnectionStatusReady {
				return true
			}
		}
	}
	return false
}

// GetPodAnnotations obtains the pod annotation from the cache
func GetPodAnnotations(ctx context.Context, podLister corev1listers.PodLister, namespace, name string, annotCond podAnnotWaitCond) (map[string]string, error) {
	timeout := time.After(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("canceled waiting for annotations")
		case <-timeout:
			return nil, fmt.Errorf("timed out waiting for annotations")
		default:
			pod, err := podLister.Pods(namespace).Get(name)
			if err != nil {
				return nil, fmt.Errorf("failed to get annotations: %v", err)
			}
			annotations := pod.ObjectMeta.Annotations
			if annotCond(annotations) {
				return annotations, nil
			}
			// try again later
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// PodAnnotation2PodInfo creates PodInterfaceInfo from Pod annotations and additional attributes
func PodAnnotation2PodInfo(podAnnotation map[string]string, checkExtIDs bool, isSmartNic bool) (
	*PodInterfaceInfo, error) {
	podAnnotSt, err := util.UnmarshalPodAnnotation(podAnnotation)
	if err != nil {
		return nil, err
	}
	ingress, egress, err := extractPodBandwidthResources(podAnnotation)
	if err != nil {
		return nil, err
	}
	podInterfaceInfo := &PodInterfaceInfo{
		PodAnnotation: *podAnnotSt,
		MTU:           config.Default.MTU,
		Ingress:       ingress,
		Egress:        egress,
		CheckExtIDs:   checkExtIDs,
		IsSmartNic:    isSmartNic,
	}
	return podInterfaceInfo, nil
}

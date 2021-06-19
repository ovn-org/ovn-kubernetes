package cni

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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

// getPod returns a pod from the informer cache or (if that fails) the apiserver
func getPod(podLister corev1listers.PodLister, kclient kubernetes.Interface, namespace, name string) (*kapi.Pod, error) {
	pod, err := podLister.Pods(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		// If the pod wasn't in our local cache, ask for it directly
		pod, err = kclient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	}
	return pod, err
}

// GetPodAnnotations obtains the pod annotation from the cache
func GetPodAnnotations(ctx context.Context, podLister corev1listers.PodLister, kclient kubernetes.Interface, namespace, name string, annotCond podAnnotWaitCond) (map[string]string, error) {
	var notFoundCount uint

	timeout := time.After(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("canceled waiting for annotations")
		case <-timeout:
			return nil, fmt.Errorf("timed out waiting for annotations")
		default:
			pod, err := getPod(podLister, kclient, namespace, name)
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return nil, fmt.Errorf("failed to get pod for annotations: %v", err)
				}
				// Allow up to 1 second for pod to be found
				notFoundCount++
				if notFoundCount >= 5 {
					return nil, fmt.Errorf("timed out waiting for pod after 1s: %v", err)
				}
				// drop through to try again
			} else if pod != nil {
				annotations := pod.ObjectMeta.Annotations
				if annotCond(annotations) {
					return annotations, nil
				}
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
	ingress, err := extractPodBandwidth(podAnnotation, Ingress)
	if err != nil && !errors.Is(err, BandwidthNotFound) {
		return nil, err
	}
	egress, err := extractPodBandwidth(podAnnotation, Egress)
	if err != nil && !errors.Is(err, BandwidthNotFound) {
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

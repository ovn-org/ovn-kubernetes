package cni

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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

// isDPUReady is a wait condition which waits for OVN master to set pod-networks annotation and
// ovnkube running on DPU to set connection-status pod annotation and its status is Ready
func isDPUReady(podAnnotation map[string]string) bool {
	if isOvnReady(podAnnotation) {
		// check DPU connection status
		status := util.DPUConnectionStatus{}
		if err := status.FromPodAnnotation(podAnnotation); err == nil {
			if status.Status == util.DPUConnectionStatusReady {
				return true
			}
		}
	}
	return false
}

// getPod tries to read a Pod object from the informer cache, or if the pod
// doesn't exist there, the apiserver. If neither a list or a kube client is
// given, returns no pod and no error
func getPod(podLister corev1listers.PodLister, kclient kubernetes.Interface, namespace, name string) (*kapi.Pod, error) {
	var pod *kapi.Pod
	var err error

	if podLister != nil {
		pod, err = podLister.Pods(namespace).Get(name)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		// drop through
	}

	if pod == nil && kclient != nil {
		// If the pod wasn't in our local cache, ask for it directly
		pod, err = kclient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	}

	return pod, err
}

// GetPodAnnotations obtains the pod UID and annotation from the cache or apiserver
func GetPodAnnotations(ctx context.Context, podLister corev1listers.PodLister, kclient kubernetes.Interface, namespace, name string, annotCond podAnnotWaitCond) (string, map[string]string, error) {
	var notFoundCount uint

	for {
		select {
		case <-ctx.Done():
			detail := "timed out"
			if ctx.Err() == context.Canceled {
				detail = "canceled while"
			}
			return "", nil, fmt.Errorf("%s waiting for annotations: %w", detail, ctx.Err())
		default:
			pod, err := getPod(podLister, kclient, namespace, name)
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return "", nil, fmt.Errorf("failed to get pod for annotations: %v", err)
				}
				// Allow up to 1 second for pod to be found
				notFoundCount++
				if notFoundCount >= 5 {
					return "", nil, fmt.Errorf("timed out waiting for pod after 1s: %v", err)
				}
				// drop through to try again
			} else if pod != nil {
				if annotCond(pod.Annotations) {
					return string(pod.UID), pod.Annotations, nil
				}
			}

			// try again later
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// PodAnnotation2PodInfo creates PodInterfaceInfo from Pod annotations and additional attributes
func PodAnnotation2PodInfo(podAnnotation map[string]string, checkExtIDs bool, podUID string) (
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
		RoutableMTU:   config.Default.RoutableMTU,
		Ingress:       ingress,
		Egress:        egress,
		CheckExtIDs:   checkExtIDs,
		IsDPUHostMode: config.OvnKubeNode.Mode == types.NodeModeDPUHost,
		PodUID:        podUID,
	}
	return podInterfaceInfo, nil
}

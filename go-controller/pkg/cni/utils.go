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
)

// wait on a certain pod annotation related condition
type podAnnotWaitCond func(map[string]string, string) (*util.PodAnnotation, bool)

// isOvnReady is a wait condition for OVN master to set pod-networks annotation
func isOvnReady(podAnnotation map[string]string, nadName string) (*util.PodAnnotation, bool) {
	podNADAnnotation, err := util.UnmarshalPodAnnotation(podAnnotation, nadName)
	return podNADAnnotation, err == nil
}

// isDPUReady is a wait condition which waits for OVN master to set pod-networks annotation and
// ovnkube running on DPU to set connection-status pod annotation and its status is Ready
func isDPUReady(podAnnotation map[string]string, nadName string) (*util.PodAnnotation, bool) {
	podNADAnnotation, ready := isOvnReady(podAnnotation, nadName)
	if ready {
		// check DPU connection status
		if status, err := util.UnmarshalPodDPUConnStatus(podAnnotation, nadName); err == nil {
			if status.Status == util.DPUConnectionStatusReady {
				return podNADAnnotation, true
			}
		}
	}
	return nil, false
}

// getPod tries to read a Pod object from the informer cache, or if the pod
// doesn't exist there, the apiserver. If neither a list or a kube client is
// given, returns no pod and no error
func (c *ClientSet) getPod(namespace, name string) (*kapi.Pod, error) {
	var pod *kapi.Pod
	var err error

	pod, err = c.podLister.Pods(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}

	if pod == nil {
		// If the pod wasn't in our local cache, ask for it directly
		pod, err = c.kclient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	}

	return pod, err
}

// GetPodAnnotations obtains the pod UID and annotation from the cache or apiserver
func GetPodAnnotations(ctx context.Context, getter PodInfoGetter,
	namespace, name, nadName string, annotCond podAnnotWaitCond) (string, map[string]string, *util.PodAnnotation, error) {
	var notFoundCount uint

	for {
		select {
		case <-ctx.Done():
			detail := "timed out"
			if ctx.Err() == context.Canceled {
				detail = "canceled while"
			}
			return "", nil, nil, fmt.Errorf("%s waiting for annotations: %w", detail, ctx.Err())
		default:
			pod, err := getter.getPod(namespace, name)
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return "", nil, nil, fmt.Errorf("failed to get pod for annotations: %v", err)
				}
				// Allow up to 1 second for pod to be found
				notFoundCount++
				if notFoundCount >= 5 {
					return "", nil, nil, fmt.Errorf("timed out waiting for pod after 1s: %v", err)
				}
				// drop through to try again
			} else if pod != nil {
				podNADAnnotation, ready := annotCond(pod.Annotations, nadName)
				if ready {
					return string(pod.UID), pod.Annotations, podNADAnnotation, nil
				}
			}

			// try again later
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// PodAnnotation2PodInfo creates PodInterfaceInfo from Pod annotations and additional attributes
func PodAnnotation2PodInfo(podAnnotation map[string]string, podNADAnnotation *util.PodAnnotation, checkExtIDs bool, podUID,
	vfNetdevname, nadName string, netName string, mtu int) (*PodInterfaceInfo, error) {
	var err error
	// get pod's annotation of the given NAD if it is not available
	if podNADAnnotation == nil {
		podNADAnnotation, err = util.UnmarshalPodAnnotation(podAnnotation, nadName)
		if err != nil {
			return nil, err
		}
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
		PodAnnotation:        *podNADAnnotation,
		MTU:                  mtu,
		RoutableMTU:          config.Default.RoutableMTU, // TBD, configurable for secondary network?
		Ingress:              ingress,
		Egress:               egress,
		CheckExtIDs:          checkExtIDs,
		IsDPUHostMode:        config.OvnKubeNode.Mode == types.NodeModeDPUHost,
		PodUID:               podUID,
		VfNetdevName:         vfNetdevname,
		NetName:              netName,
		NADName:              nadName,
		EnableUDPAggregation: config.Default.EnableUDPAggregation,
	}
	return podInterfaceInfo, nil
}

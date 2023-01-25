package cni

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	podnetworkapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/podnetwork/v1"

	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// wait on a certain pod annotation and/or PodNetwork related condition
type podWaitCondition func(podAnnotation map[string]string, podNetwork *podnetworkapi.PodNetwork, nadName string) bool

// isOvnReady is a wait condition for OVN master to set pod-networks annotation
func isOvnReady(podAnnotation map[string]string, podNetwork *podnetworkapi.PodNetwork, nadName string) bool {
	_, err := util.ParsePodNetwork(podNetwork, nadName)
	return err == nil
}

// isDPUReady is a wait condition which waits for OVN master to set pod-networks annotation and
// ovnkube running on DPU to set connection-status pod annotation and its status is Ready
func isDPUReady(podAnnotation map[string]string, podNetwork *podnetworkapi.PodNetwork, nadName string) bool {
	if isOvnReady(podAnnotation, podNetwork, nadName) {
		// check DPU connection status
		if status, err := util.UnmarshalPodDPUConnStatus(podAnnotation, nadName); err == nil {
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

// getPodNetwork tries to read a PodNetwork object from the informer cache, or if the PodNetwork
// doesn't exist there, the apiserver.
func (c *ClientSet) getPodNetwork(pod *kapi.Pod) (*podnetworkapi.PodNetwork, error) {
	podNetwork, err := c.podNetLister.PodNetworks(pod.Namespace).Get(util.GetPodNetworkName(pod))
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}

	if podNetwork == nil {
		// If the pod network wasn't in our local cache, ask for it directly
		podNetwork, err = c.podNetClient.K8sV1().PodNetworks(pod.Namespace).Get(context.TODO(), util.GetPodNetworkName(pod), metav1.GetOptions{})
	}

	return podNetwork, err
}

// GetPodNetworkInfo obtains the pod UID, pod annotation, and PodNetwork from the cache or apiserver
func GetPodNetworkInfo(ctx context.Context, getter PodInfoGetter, namespace, name, nadName string,
	podWaitCond podWaitCondition) (string, map[string]string, *podnetworkapi.PodNetwork, error) {
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
			}
			var podNetworks *podnetworkapi.PodNetwork
			if pod != nil {
				podNetworks, err = getter.getPodNetwork(pod)
				if err != nil {
					if !apierrors.IsNotFound(err) {
						return "", nil, nil, fmt.Errorf("failed to get podNetwork: %v", err)
					}
					// Allow up to 1 second for pod network to be found
					notFoundCount++
					if notFoundCount >= 5 {
						return "", nil, nil, fmt.Errorf("timed out waiting for podNetwork after 1s: %v", err)
					}
					// drop through to try again
				}
			}
			if pod != nil && podNetworks != nil {
				if podWaitCond(pod.Annotations, podNetworks, nadName) {
					return string(pod.UID), pod.Annotations, podNetworks, nil
				}
			}

			// try again later
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// PodNetworkAndAnnotation2PodInfo creates PodInterfaceInfo from Pod annotations and PodNetwork object
func PodNetworkAndAnnotation2PodInfo(podAnnotation map[string]string, podNetworks *podnetworkapi.PodNetwork,
	checkExtIDs bool, podUID, vfNetdevname, nadName, netName string, mtu int) (*PodInterfaceInfo, error) {
	nadNetwork, err := util.ParsePodNetwork(podNetworks, nadName)
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
		SinglePodNetwork:     *nadNetwork,
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

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const podAnnotationPollInterval = 200 * time.Millisecond

// wait on a certain pod annotation related condition
// return error to abort the retry attempt
type podAnnotWaitCond = func(*corev1.Pod, string) (*util.PodAnnotation, bool, error)

// isOvnReady is a wait condition for the pod-networks annotation to be set.
func isOvnReady(pod *corev1.Pod, nadKey string) (*util.PodAnnotation, bool, error) {
	podNADAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return podNADAnnotation, true, nil
}

// isDPUReady is a wait condition for the pod-networks annotation and the DPU
// connection-status pod annotation to be set to Ready.
func isDPUReady(annotCondFn podAnnotWaitCond, nadKey string) podAnnotWaitCond {
	return func(pod *corev1.Pod, nad string) (annotation *util.PodAnnotation, ready bool, err error) {
		if annotCondFn != nil {
			annotation, ready, err = annotCondFn(pod, nad)
			if err != nil || !ready {
				return
			}
		}
		// check DPU connection status of the given nad name
		status, err := util.UnmarshalPodDPUConnStatus(pod.Annotations, nadKey)
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				return annotation, false, nil
			}
			return annotation, false, err
		}
		if status.Status == util.DPUConnectionStatusReady {
			return annotation, true, nil
		}
		return annotation, false, fmt.Errorf("DPU plumb failed: connection status is %v", status.Status)
	}
}

// getPod reads a Pod object from the local informer cache. The CNI server is
// started only after this informer has synced, so bypassing it on a cache miss
// would amplify API load precisely when the watch is lagging.
func (c *ClientSet) getPod(namespace, name string) (*corev1.Pod, error) {
	return c.podLister.Pods(namespace).Get(name)
}

// getPodWithAnnotations optionally validates expectedPodUID when the Pod is
// found. podAlreadyObserved distinguishes an initial informer cache miss from
// deletion of a Pod that this CNI ADD previously observed.
func getPodWithAnnotations(ctx context.Context, getter PodInfoGetter,
	namespace, name, nadName, expectedPodUID string, podAlreadyObserved bool,
	annotCond podAnnotWaitCond) (*corev1.Pod, map[string]string, *util.PodAnnotation, error) {
	var pod *corev1.Pod
	var podNADAnnotation *util.PodAnnotation
	podUID := expectedPodUID
	podObserved := podAlreadyObserved

	getReadyPod := func() (*corev1.Pod, *util.PodAnnotation, bool, error) {
		pod, err := getter.getPod(namespace, name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				if podObserved {
					return nil, nil, false, fmt.Errorf("pod %s/%s with UID %s was deleted while waiting for annotations",
						namespace, name, podUID)
				}
				return nil, nil, false, nil
			}
			return nil, nil, false, fmt.Errorf("failed to get pod for annotations: %v", err)
		}
		if podUID != "" && string(pod.UID) != podUID {
			// A runtime identifies a static pod by its config hash, which does
			// not match the API UID of its mirror Pod. Accept that mismatch only
			// on the initial observation, then pin subsequent reads to the mirror.
			if podObserved || !IsStaticPod(pod) {
				return nil, nil, false, fmt.Errorf("pod %s/%s with UID %s was replaced by UID %s while waiting for annotations",
					namespace, name, podUID, pod.UID)
			}
		}
		podUID = string(pod.UID)
		podObserved = true
		annotation, ready, err := annotCond(pod, nadName)
		return pod, annotation, ready, err
	}

	poll := wait.Backoff{Duration: 2 * time.Millisecond, Factor: 2, Steps: 8, Cap: podAnnotationPollInterval}
	err := poll.DelayFunc().Until(ctx, true, true,
		func(context.Context) (bool, error) {
			var ready bool
			var err error
			pod, podNADAnnotation, ready, err = getReadyPod()
			return ready, err
		})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			detail := "timed out"
			if errors.Is(err, context.Canceled) {
				detail = "canceled while"
			}
			return nil, nil, nil, fmt.Errorf("%s waiting for annotations: %w", detail, err)
		}
		return nil, nil, nil, err
	}
	return pod, pod.Annotations, podNADAnnotation, nil
}

// PodAnnotation2PodInfo creates PodInterfaceInfo from Pod annotations and additional attributes
func PodAnnotation2PodInfo(podAnnotation map[string]string, podNADAnnotation *util.PodAnnotation, podUID,
	netdevname, nadKey, netName string, mtu int) (*PodInterfaceInfo, error) {
	var err error
	// get pod's annotation of the given NAD if it is not available
	if podNADAnnotation == nil {
		podNADAnnotation, err = util.UnmarshalPodAnnotation(podAnnotation, nadKey)
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
		RoutableMTU:          config.Default.RoutableMTU, // TBD, configurable for UDNs?
		Ingress:              ingress,
		Egress:               egress,
		IsDPUHostMode:        config.IsModeDPUHost(),
		IsSimulatedDPU:       util.IsSimulatedDPU(),
		PodUID:               podUID,
		NetdevName:           netdevname,
		NetName:              netName,
		NADKey:               nadKey,
		EnableUDPAggregation: config.Default.EnableUDPAggregation,
	}
	return podInterfaceInfo, nil
}

// GetPodIfNamesForNAD gets the pod's all interface names of the given secondary NAD name
//
// Note that the names of secondary UDN pod interfaces are determined by multus: it is either specified by
// network selection itself, or it is net<index> (<index> is determined by order of the pod interface,
// started from 1 for the 1st non-default interface).
func GetPodIfNamesForNAD(pod *corev1.Pod, nadName string) ([]string, error) {
	networks, err := util.GetK8sPodAllNetworkSelections(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to getting network selection elements for pod %s/%s: %v",
			pod.Namespace, pod.Name, err)
	}
	ifNames := make([]string, 0, len(networks))
	for idx, network := range networks {
		nad := util.GetNADName(network.Namespace, network.Name)
		if nad != nadName {
			continue
		}
		if network.InterfaceRequest != "" {
			ifNames = append(ifNames, network.InterfaceRequest)
		} else {
			ifNames = append(ifNames, fmt.Sprintf("net%d", idx+1))
		}
	}
	return ifNames, nil
}

// GetCNINADKey gets the pod's nadKey (nadName with index in case there are multiple same NADs in the pod)
// Based on the given ifName, find out which number of this CNI request is for the given nadName, then
// determine its associated NAD key.
func GetCNINADKey(pod *corev1.Pod, ifName, nadName string) (string, error) {
	ifNames, err := GetPodIfNamesForNAD(pod, nadName)
	if err != nil {
		return "", err
	}
	for idx, name := range ifNames {
		if ifName == name {
			return util.GetIndexedNADKey(nadName, idx), nil
		}
	}
	return "", fmt.Errorf("failed to find NAD key associated with CNI request for pod %s/%s with ifName %s",
		pod.Namespace, pod.Name, ifName)
}

// START taken from https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/types/pod_update.go
const (
	ConfigSourceAnnotationKey = "kubernetes.io/config.source"
	// ApiserverSource identifies updates from Kubernetes API Server.
	ApiserverSource = "api"
)

// GetPodSource returns the source of the pod based on the annotation.
func GetPodSource(pod *corev1.Pod) (string, error) {
	if pod.Annotations != nil {
		if source, ok := pod.Annotations[ConfigSourceAnnotationKey]; ok {
			return source, nil
		}
	}
	return "", fmt.Errorf("cannot get source of pod %q", pod.UID)
}

// IsStaticPod returns true if the pod is a static pod.
func IsStaticPod(pod *corev1.Pod) bool {
	source, err := GetPodSource(pod)
	return err == nil && source != ApiserverSource
}

//END taken from https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/types/pod_update.go

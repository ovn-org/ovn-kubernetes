// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// DefaultNetworkMigrationEndpoints returns a private view of default-network
// EndpointSlices that keeps a VM's retained address eligible during its launcher
// handoff. Kubernetes-owned slices and informer objects are never modified.
// Local traffic policies retain Kubernetes readiness because they require an
// endpoint on the serving node, not merely a reachable shared VM address.
func DefaultNetworkMigrationEndpoints(slices []*discovery.EndpointSlice, service *corev1.Service, pods corelisters.PodLister) ([]*discovery.EndpointSlice, error) {
	if service == nil || pods == nil || util.ServiceExternalTrafficPolicyLocal(service) || util.ServiceInternalTrafficPolicyLocal(service) {
		return slices, nil
	}
	result := append([]*discovery.EndpointSlice(nil), slices...)
	for i, slice := range slices {
		// Mirrored UDN slices already have their own migration handling.
		if slice.Labels[discovery.LabelServiceName] != service.Name || slice.Namespace != service.Namespace ||
			slice.Annotations[types.UserDefinedNetworkEndpointSliceAnnotation] != "" {
			continue
		}
		for j, endpoint := range slice.Endpoints {
			if util.IsEndpointReady(endpoint) || endpoint.TargetRef == nil || endpoint.TargetRef.Kind != "Pod" || endpoint.TargetRef.Namespace != slice.Namespace {
				continue
			}
			pod, err := pods.Pods(slice.Namespace).Get(endpoint.TargetRef.Name)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if pod.UID != endpoint.TargetRef.UID || !IsPodLiveMigratable(pod) || !IsPodOwnedByVirtualMachine(pod) {
				continue
			}
			status, err := DiscoverLiveMigrationStatus(pods, pod)
			if err != nil {
				return nil, fmt.Errorf("discover migration for endpoint pod %s/%s: %w", pod.Namespace, pod.Name, err)
			}
			if status == nil || status.State == LiveMigrationFailed || !runningLauncher(status.SourcePod) || !runningLauncher(status.TargetPod) {
				continue
			}
			if pod.UID != status.SourcePod.UID && pod.UID != status.TargetPod.UID {
				continue
			}
			// VM names can be reused. Only a handoff within one VMI may override
			// readiness; a replacement VM is not an ongoing connection owner.
			sourceOwner := metav1.GetControllerOf(status.SourcePod)
			targetOwner := metav1.GetControllerOf(status.TargetPod)
			if sourceOwner == nil || targetOwner == nil || sourceOwner.Kind != "VirtualMachineInstance" ||
				sourceOwner.APIVersion != "kubevirt.io/v1" || sourceOwner.UID == "" || sourceOwner.UID != targetOwner.UID ||
				sourceOwner.Kind != targetOwner.Kind || sourceOwner.APIVersion != targetOwner.APIVersion {
				continue
			}
			if !endpointUsesPodIPs(endpoint, status.SourcePod) || !endpointUsesPodIPs(endpoint, status.TargetPod) {
				continue
			}
			if result[i] == slice {
				result[i] = slice.DeepCopy()
			}
			result[i].Endpoints[j].Conditions = discovery.EndpointConditions{
				Ready: ptr.To(true), Serving: ptr.To(true), Terminating: ptr.To(false),
			}
		}
	}
	return result, nil
}

func runningLauncher(pod *corev1.Pod) bool {
	return pod != nil && pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil && IsPodLiveMigratable(pod)
}

func endpointUsesPodIPs(endpoint discovery.Endpoint, pod *corev1.Pod) bool {
	annotation, err := util.UnmarshalPodAnnotation(pod.Annotations, types.DefaultNetworkName)
	if err != nil || len(endpoint.Addresses) == 0 {
		return false
	}
	for _, address := range endpoint.Addresses {
		found := false
		for _, ip := range annotation.IPs {
			if ip.IP.Equal(net.ParseIP(address)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestDefaultNetworkMigrationEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*corev1.Pod, *corev1.Pod, *corev1.Service, *discovery.EndpointSlice)
		keep   bool
	}{
		{name: "handoff with both launchers unready", keep: true},
		{name: "target domain ready before PodReady", keep: true, change: func(_, target *corev1.Pod, _ *corev1.Service, _ *discovery.EndpointSlice) {
			target.Annotations[kubevirtv1.MigrationTargetReadyTimestamp] = time.Now().Format(time.RFC3339Nano)
		}},
		{name: "ordinary pods", change: func(source, target *corev1.Pod, _ *corev1.Service, _ *discovery.EndpointSlice) {
			source.Labels = nil
			target.Labels = nil
		}},
		{name: "unannotated VM", change: func(source, target *corev1.Pod, _ *corev1.Service, _ *discovery.EndpointSlice) {
			delete(source.Annotations, kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation)
			delete(target.Annotations, kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation)
		}},
		{name: "different VMI incarnation", change: func(_, target *corev1.Pod, _ *corev1.Service, _ *discovery.EndpointSlice) {
			target.OwnerReferences[0].UID = "another-vmi"
		}},
		{name: "failed target", change: func(_, target *corev1.Pod, _ *corev1.Service, _ *discovery.EndpointSlice) {
			target.Status.Phase = corev1.PodFailed
		}},
		{name: "completed source must not mask later target failure", change: func(source, target *corev1.Pod, _ *corev1.Service, _ *discovery.EndpointSlice) {
			source.Status.Phase = corev1.PodSucceeded
			target.Annotations[kubevirtv1.MigrationTargetReadyTimestamp] = time.Now().Format(time.RFC3339Nano)
		}},
		{name: "VM deletion", change: func(source, target *corev1.Pod, _ *corev1.Service, _ *discovery.EndpointSlice) {
			source.DeletionTimestamp = ptr.To(metav1.Now())
			target.DeletionTimestamp = ptr.To(metav1.Now())
		}},
		{name: "different infrastructure address", change: func(_, target *corev1.Pod, _ *corev1.Service, _ *discovery.EndpointSlice) {
			target.Annotations[types.OvnPodAnnotationName] = `{"default":{"ip_addresses":["10.244.2.9/24"],"mac_address":"0a:58:0a:f4:02:09"}}`
		}},
		{name: "malformed annotation", change: func(_, target *corev1.Pod, _ *corev1.Service, _ *discovery.EndpointSlice) {
			target.Annotations[types.OvnPodAnnotationName] = "invalid"
		}},
		{name: "stale endpoint UID", change: func(_, _ *corev1.Pod, _ *corev1.Service, slice *discovery.EndpointSlice) {
			for i := range slice.Endpoints {
				slice.Endpoints[i].TargetRef.UID = "old-uid"
			}
		}},
		{name: "ETP Local", change: func(_, _ *corev1.Pod, svc *corev1.Service, _ *discovery.EndpointSlice) {
			svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
		}},
		{name: "ITP Local", change: func(_, _ *corev1.Pod, svc *corev1.Service, _ *discovery.EndpointSlice) {
			svc.Spec.InternalTrafficPolicy = ptr.To(corev1.ServiceInternalTrafficPolicyLocal)
		}},
		{name: "mirrored UDN", change: func(_, _ *corev1.Pod, _ *corev1.Service, slice *discovery.EndpointSlice) {
			slice.Annotations = map[string]string{types.UserDefinedNetworkEndpointSliceAnnotation: "udn"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, address := range []string{"10.244.1.4", "fd00:10:244:1::4"} {
				pod := func(name string, created time.Time) *corev1.Pod {
					return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
						Name: name, Namespace: "test", UID: ktypes.UID(name), CreationTimestamp: metav1.NewTime(created),
						Labels: map[string]string{kubevirtv1.AppLabel: "virt-launcher", kubevirtv1.VirtualMachineNameLabel: "vm"},
						Annotations: map[string]string{kubevirtv1.DomainAnnotation: "vm", kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
							types.OvnPodAnnotationName: `{"default":{"ip_addresses":["10.244.1.4/24","fd00:10:244:1::4/64"],"mac_address":"0a:58:0a:f4:01:04"}}`},
						OwnerReferences: []metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: "vm", UID: "vmi-uid", Controller: ptr.To(true)}},
					}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
				}
				source := pod("source", time.Now().Add(-time.Minute))
				target := pod("target", time.Now())
				svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "test"}}
				slice := &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "slice", Namespace: "test", Labels: map[string]string{discovery.LabelServiceName: "svc"}}}
				for _, p := range []*corev1.Pod{source, target} {
					slice.Endpoints = append(slice.Endpoints, discovery.Endpoint{Addresses: []string{address}, NodeName: ptr.To(p.Name + "-node"),
						TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, UID: p.UID},
						Conditions: discovery.EndpointConditions{Ready: ptr.To(false), Serving: ptr.To(false), Terminating: ptr.To(false)}})
				}
				if tc.change != nil {
					tc.change(source, target, svc, slice)
				}
				indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
				require.NoError(t, indexer.Add(source))
				require.NoError(t, indexer.Add(target))
				before := slice.DeepCopy()
				adjusted, err := DefaultNetworkMigrationEndpoints([]*discovery.EndpointSlice{slice}, svc, corelisters.NewPodLister(indexer))
				require.NoError(t, err)
				require.Equal(t, before, slice, "informer slice must not be mutated")
				for i, endpoint := range adjusted[0].Endpoints {
					require.Equal(t, tc.keep, *endpoint.Conditions.Ready, address)
					require.Equal(t, tc.keep, *endpoint.Conditions.Serving, address)
					require.Equal(t, before.Endpoints[i].NodeName, endpoint.NodeName)
				}
				// Removing the source ends the exception, even if the target's
				// domain-ready annotation remains from a successful migration.
				require.NoError(t, indexer.Delete(source))
				adjusted, err = DefaultNetworkMigrationEndpoints([]*discovery.EndpointSlice{slice}, svc, corelisters.NewPodLister(indexer))
				require.NoError(t, err)
				require.Equal(t, before, adjusted[0])
			}
		})
	}
}

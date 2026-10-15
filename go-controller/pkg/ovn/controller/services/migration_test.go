// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestDefaultNetworkMigrationServiceBackends(t *testing.T) {
	for _, mode := range []config.GatewayMode{config.GatewayModeShared, config.GatewayModeLocal} {
		t.Run(string(mode), func(t *testing.T) {
			require.NoError(t, config.PrepareTestConfig())
			config.Gateway.Mode = mode
			config.IPv4Mode, config.IPv6Mode = true, true
			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
				{CIDR: ovntest.MustParseIPNet("10.244.0.0/16"), HostSubnetLength: 24},
				{CIDR: ovntest.MustParseIPNet("fd00:10:244::/48"), HostSubnetLength: 64},
			}
			c, err := newControllerWithDBSetupForNetwork(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
				lbGroup(types.ClusterLBGroupName), lbGroup(types.ClusterSwitchLBGroupName), lbGroup(types.ClusterRouterLBGroupName),
			}}, &util.DefaultNetInfo{}, "test")
			require.NoError(t, err)
			defer c.close()
			svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "test"}, Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.2", ClusterIPs: []string{"10.96.0.2", "fd00:10:96::2"},
				Ports: []corev1.ServicePort{{Protocol: corev1.ProtocolTCP, Port: 5201, TargetPort: intstr.FromInt32(5201)}},
			}}
			require.NoError(t, c.serviceStore.Add(svc))
			source := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: "test", UID: "source",
				CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
				Labels:            map[string]string{kubevirtv1.AppLabel: "virt-launcher", kubevirtv1.VirtualMachineNameLabel: "vm"},
				Annotations: map[string]string{kubevirtv1.DomainAnnotation: "vm", kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
					types.OvnPodAnnotationName: `{"default":{"ip_addresses":["10.244.1.4/24","fd00:10:244:1::4/64"],"mac_address":"0a:58:0a:f4:01:04"}}`},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: "vm", UID: "vmi", Controller: ptr.To(true)}},
			}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			target := source.DeepCopy()
			target.Name, target.UID, target.CreationTimestamp = "target", "target", metav1.Now()
			target.Annotations[kubevirtv1.MigrationTargetReadyTimestamp] = time.Now().Format(time.RFC3339Nano)
			podStore := c.podInformer.Informer().GetStore()
			require.NoError(t, podStore.Add(source))
			require.NoError(t, podStore.Add(target))
			for _, family := range []discovery.AddressType{discovery.AddressTypeIPv4, discovery.AddressTypeIPv6} {
				ip := "10.244.1.4"
				if family == discovery.AddressTypeIPv6 {
					ip = "fd00:10:244:1::4"
				}
				slice := &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: string(family), Namespace: "test", Labels: map[string]string{discovery.LabelServiceName: "svc"}},
					AddressType: family, Ports: []discovery.EndpointPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(int32(5201))}}}
				for _, pod := range []*corev1.Pod{source, target} {
					slice.Endpoints = append(slice.Endpoints, discovery.Endpoint{Addresses: []string{ip},
						TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: "test", Name: pod.Name, UID: pod.UID},
						Conditions: discovery.EndpointConditions{Ready: ptr.To(false), Serving: ptr.To(false)}})
				}
				require.NoError(t, c.endpointSliceStore.Add(slice))
			}
			key := scopedServiceQueueKey(types.DefaultNetworkName, "test/svc")
			syncAndCheck := func(want4, want6 string) {
				t.Helper()
				require.NoError(t, c.syncService(key))
				var lbs []nbdb.LoadBalancer
				require.NoError(t, c.nbClient.List(context.Background(), &lbs))
				require.Len(t, lbs, 1)
				require.Equal(t, want4, lbs[0].Vips["10.96.0.2:5201"])
				require.Equal(t, want6, lbs[0].Vips["[fd00:10:96::2]:5201"])
			}
			syncAndCheck("10.244.1.4:5201", "[fd00:10:244:1::4]:5201")
			for _, item := range c.endpointSliceStore.List() {
				require.False(t, *item.(*discovery.EndpointSlice).Endpoints[0].Conditions.Ready)
			}
			// The EndpointSlices remain unchanged; source completion must still
			// enqueue a reconciliation and restore normal readiness semantics.
			setServiceControllerStartupDone(c.Controller, true)
			drainServiceQueue(c.Controller)
			source = source.DeepCopy()
			source.Status.Phase = corev1.PodSucceeded
			require.NoError(t, podStore.Update(source))
			c.onMigrationPodChange(source)
			require.Contains(t, drainServiceQueue(c.Controller), key)
			syncAndCheck("", "")
			require.NoError(t, podStore.Delete(source))
			c.onMigrationPodChange(cache.DeletedFinalStateUnknown{Obj: source})
			require.Contains(t, drainServiceQueue(c.Controller), key)
		})
	}
}

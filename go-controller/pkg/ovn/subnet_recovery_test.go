// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"testing"

	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	logicalswitchmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestAllocatePodIPsAfterSubnetChange(t *testing.T) {
	for _, networkName := range []string{"default", "blue"} {
		for _, tc := range []struct {
			name     string
			phase    corev1.PodPhase
			ips      []string
			reserved []string
			wantErr  bool
		}{
			{name: "evicted pod with obsolete IPv4", phase: corev1.PodFailed, ips: []string{"172.18.1.3/24"}},
			{name: "succeeded pod with obsolete IPv6", phase: corev1.PodSucceeded, ips: []string{"fd00:1::3/64"}},
			{name: "terminal pod retains current reservations", phase: corev1.PodFailed, ips: []string{"172.18.2.3/24", "fd00:2::3/64"}, reserved: []string{"172.18.2.3/24", "fd00:2::3/64"}},
			{name: "terminal pod retains IPv6 when IPv4 changed", phase: corev1.PodFailed, ips: []string{"172.18.1.3/24", "fd00:2::3/64"}, reserved: []string{"fd00:2::3/64"}},
			{name: "terminal pod retains IPv4 when IPv6 changed", phase: corev1.PodSucceeded, ips: []string{"172.18.2.3/24", "fd00:1::3/64"}, reserved: []string{"172.18.2.3/24"}},
			{name: "running pod mismatch remains an error", phase: corev1.PodRunning, ips: []string{"172.18.1.3/24"}, wantErr: true},
			{name: "pending pod mismatch remains an error", phase: corev1.PodPending, ips: []string{"172.18.1.3/24"}, wantErr: true},
		} {
			t.Run(fmt.Sprintf("%s/%s", networkName, tc.name), func(t *testing.T) {
				g := gomega.NewWithT(t)
				g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
				t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed()) })
				config.IPv4Mode, config.IPv6Mode = true, true
				var netInfo util.NetInfo = &util.DefaultNetInfo{}
				nadKey := "default"
				if networkName != "default" {
					nad := ovntest.GenerateNAD(networkName, networkName, "ns1", types.Layer3Topology, "172.18.0.0/16/24,fd00::/16/64", types.NetworkRolePrimary)
					var err error
					netInfo, err = util.ParseNADInfo(nad)
					g.Expect(err).NotTo(gomega.HaveOccurred())
					nadKey = "ns1/" + networkName
				}
				switchName := netInfo.GetNetworkScopedSwitchName("node1")
				client := util.GetOVNClientset().GetOVNKubeControllerClientset()
				wf, err := factory.NewOVNKubeControllerWatchFactory(client, "node1")
				g.Expect(err).NotTo(gomega.HaveOccurred())
				t.Cleanup(wf.Shutdown)
				g.Expect(wf.NodeCoreInformer().Informer().GetStore().Add(&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{Name: "node1"},
				})).To(gomega.Succeed())
				lsm := logicalswitchmanager.NewLogicalSwitchManager()
				g.Expect(lsm.AddOrUpdateSwitch(switchName, ovntest.MustParseIPNets("172.18.2.0/24", "fd00:2::/64"), nil, nil)).To(gomega.Succeed())
				bnc := &BaseNetworkController{
					CommonNetworkControllerInfo: CommonNetworkControllerInfo{watchFactory: wf},
					ReconcilableNetInfo:         util.NewReconcilableNetInfo(netInfo),
					lsManager:                   lsm,
				}
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1", UID: "pod1"},
					Spec:       corev1.PodSpec{NodeName: "node1"},
					Status:     corev1.PodStatus{Phase: tc.phase},
				}
				annotation := &util.PodAnnotation{IPs: ovntest.MustParseIPNets(tc.ips...)}
				port, err := bnc.allocatePodIPsOnSwitch(pod, annotation.IPs, nadKey, switchName)
				if tc.wantErr {
					g.Expect(err).To(gomega.HaveOccurred(), "live pod inconsistencies must not be hidden")
					return
				}
				g.Expect(err).NotTo(gomega.HaveOccurred(), "obsolete terminal-pod addresses must not prevent startup")
				g.Expect(port).To(gomega.Equal(bnc.GetLogicalPortName(pod, nadKey)), "retain the port for normal cleanup")
				g.Expect(annotation.IPs).To(gomega.Equal(ovntest.MustParseIPNets(tc.ips...)), "do not rewrite pod annotations")
				for _, ip := range tc.reserved {
					ips := ovntest.MustParseIPNets(ip)
					g.Expect(lsm.OwnsIPs(switchName, bnc.podIPOwner(pod, nadKey), ips)).To(gomega.BeTrue(), "current addresses must retain the completed pod's owner")
					g.Expect(lsm.AllocateIPs(switchName, "other-pod", ips)).To(gomega.MatchError(ipallocator.ErrAllocatedByOther), "current addresses must stay reserved until cleanup")
				}
			})
		}
	}
}

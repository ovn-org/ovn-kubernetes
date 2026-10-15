// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

	ipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	apbroutecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	logicalswitchmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestBaseNetworkController_GetLocalNode(t *testing.T) {
	g := gomega.NewWithT(t)
	clientSet := util.GetOVNClientset(&corev1.NodeList{Items: []corev1.Node{{
		ObjectMeta: metav1.ObjectMeta{Name: "node1"},
	}}}).GetOVNKubeControllerClientset()
	watchFactory, err := factory.NewOVNKubeControllerWatchFactory(clientSet, "test-node")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(watchFactory.Start()).To(gomega.Succeed())
	t.Cleanup(watchFactory.Shutdown)

	bnc := &BaseNetworkController{CommonNetworkControllerInfo: CommonNetworkControllerInfo{
		watchFactory: watchFactory,
		nodeName:     "node1",
	}}
	node, err := bnc.GetLocalNode()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(node.Name).To(gomega.Equal("node1"))

	bnc.nodeName = "missing-node"
	_, err = bnc.GetLocalNode()
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
}

func TestPodIPOwner(t *testing.T) {
	g := gomega.NewWithT(t)
	bnc := &BaseNetworkController{ReconcilableNetInfo: &util.DefaultNetInfo{}}
	nad := ovntypes.DefaultNetworkName
	pod := ovntest.NewPod("namespace", "pod", "node", "")
	replacement := pod.DeepCopy()
	replacement.UID = "replacement"
	g.Expect(bnc.podIPOwner(pod, nad)).NotTo(gomega.Equal(bnc.podIPOwner(replacement, nad)))
	g.Expect(bnc.podIPOwner(pod, nad)).NotTo(gomega.Equal(bnc.podIPOwner(pod, "other-nad")))

	pod.Labels = map[string]string{kubevirtv1.AppLabel: "virt-launcher"}
	pod.Annotations = map[string]string{
		kubevirtv1.DomainAnnotation:                             "vm",
		kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
	}
	target := pod.DeepCopy()
	target.UID, target.Name, target.Spec.NodeName = "target", "target", "other-node"
	g.Expect(bnc.podIPOwner(pod, nad)).To(gomega.Equal(bnc.podIPOwner(target, nad)))
	g.Expect(bnc.podIPOwner(pod, nad)).NotTo(gomega.Equal(bnc.podIPOwner(pod, "other-nad")))
	target.Namespace = "other-namespace"
	g.Expect(bnc.podIPOwner(pod, nad)).NotTo(gomega.Equal(bnc.podIPOwner(target, nad)))
	target.Namespace = pod.Namespace
	target.Annotations[kubevirtv1.DomainAnnotation] = "other-vm"
	g.Expect(bnc.podIPOwner(pod, nad)).NotTo(gomega.Equal(bnc.podIPOwner(target, nad)))
}

func TestVMPodIPOwnershipOnStartup(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	nad := ovntypes.DefaultNetworkName
	ips := ovntest.MustParseIPNets("10.0.0.3/24", "fd00::3/64")
	source := ovntest.NewPod("namespace", "source", "node", "")
	source.Labels = map[string]string{kubevirtv1.AppLabel: "virt-launcher"}
	source.Annotations = map[string]string{
		kubevirtv1.DomainAnnotation:                             "vm",
		kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
	}
	var err error
	source.Annotations, err = util.MarshalPodAnnotation(source.Annotations, &util.PodAnnotation{
		IPs: ips, MAC: util.IPAddrToHWAddr(ips[0].IP),
	}, nad)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	source.Status.Phase = corev1.PodSucceeded
	source.CreationTimestamp = metav1.NewTime(time.Unix(100, 0))
	target := source.DeepCopy()
	target.Name, target.UID = "target", "target"
	target.Spec.NodeName = "other-node"
	target.Status.Phase = corev1.PodRunning
	target.CreationTimestamp = metav1.NewTime(time.Unix(200, 0))
	stale := source.DeepCopy()
	stale.Name, stale.UID = "stale", "stale"
	stale.Annotations[kubevirtv1.DomainAnnotation] = "old-vm"
	clients := util.GetOVNClientset(source, target, stale, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node"},
	}).GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(clients, "node")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(wf.Start()).To(gomega.Succeed())
	t.Cleanup(wf.Shutdown)
	nbClient, dbContext, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(dbContext.Cleanup)
	oc := &DefaultNetworkController{BaseNetworkController: BaseNetworkController{
		CommonNetworkControllerInfo: CommonNetworkControllerInfo{watchFactory: wf, nodeName: "node", nbClient: nbClient},
		ReconcilableNetInfo:         &util.DefaultNetInfo{},
		lsManager:                   logicalswitchmanager.NewLogicalSwitchManager(),
	}}
	g.Expect(oc.createNodeLogicalSwitch("node", ovntest.MustParseIPNets("10.0.0.0/24", "fd00::/64"), "", "")).To(gomega.Succeed())
	g.Expect(oc.lsManager.OwnsIPs("node", oc.podIPOwner(target, nad), ips)).To(gomega.BeTrue())

	// Restore the remote target's reservation on its original source switch.
	vms := map[ktypes.NamespacedName]bool{}
	for _, pod := range []*corev1.Pod{stale, source, target} {
		vms, _, _, err = oc.allocateSyncMigratablePodIPsOnZone(vms, pod)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}
	g.Expect(oc.lsManager.OwnsIPs("node", oc.podIPOwner(target, nad), ips)).To(gomega.BeTrue())
	g.Expect(oc.canCleanupPodIPResources(source, "node", ips)).To(gomega.BeFalse(), "the migrated VM is still running")
	g.Expect(oc.canCleanupPodIPResources(stale, "node", ips)).To(gomega.BeFalse(), "another VM owns these IPs")
	g.Expect(oc.removeRemoteZonePod(target)).To(gomega.Succeed())
	g.Expect(oc.lsManager.OwnsIPs("node", oc.podIPOwner(target, nad), ips)).To(gomega.BeTrue())

	target.Status.Phase = corev1.PodSucceeded
	target, err = clients.KubeClient.CoreV1().Pods(target.Namespace).UpdateStatus(context.Background(), target, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() bool {
		pod, err := wf.GetPod(target.Namespace, target.Name)
		return err == nil && util.PodCompleted(pod)
	}).Should(gomega.BeTrue())
	g.Expect(oc.canCleanupPodIPResources(source, "node", ips)).To(gomega.BeTrue())
	g.Expect(oc.removeRemoteZonePod(target)).To(gomega.Succeed())
	g.Expect(oc.lsManager.OwnsIPs("node", oc.podIPOwner(target, nad), ips)).To(gomega.BeFalse())

	ordinary := ovntest.NewPod("namespace", "ordinary", "node", "")
	g.Expect(oc.lsManager.AllocateIPs("node", oc.podIPOwner(ordinary, nad), ips)).To(gomega.Succeed())
	g.Expect(oc.canCleanupPodIPResources(source, "node", ips)).To(gomega.BeFalse())
	g.Expect(oc.removeRemoteZonePod(target)).To(gomega.Succeed())
	g.Expect(oc.lsManager.OwnsIPs("node", oc.podIPOwner(ordinary, nad), ips)).To(gomega.BeTrue())
}

func TestMigratedVMPodIPOwnershipOnSubnetReuse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		subnets  []string
		localIPs []string
	}{
		{"both subnets reused", []string{"10.0.0.0/24", "fd00::/64"}, []string{"10.0.0.3/24", "fd00::3/64"}},
		{"IPv4 subnet reused", []string{"10.0.0.0/24", "fd01::/64"}, []string{"10.0.0.3/24"}},
		{"IPv6 subnet reused", []string{"10.1.0.0/24", "fd00::/64"}, []string{"fd00::3/64"}},
		{"neither subnet reused", []string{"10.1.0.0/24", "fd01::/64"}, nil},
	} {
		for _, podNode := range []string{"target-node", "replacement-node"} {
			t.Run(tc.name+"/"+podNode, func(t *testing.T) {
				g := gomega.NewWithT(t)
				g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
				nad := ovntypes.DefaultNetworkName
				ips := ovntest.MustParseIPNets("10.0.0.3/24", "fd00::3/64")
				localIPs := ovntest.MustParseIPNets(tc.localIPs...)
				pod := ovntest.NewPod("namespace", "migrated-vm", podNode, "10.0.0.3 fd00::3")
				pod.Status.Phase = corev1.PodRunning
				pod.Labels = map[string]string{kubevirtv1.AppLabel: "virt-launcher"}
				pod.Annotations = map[string]string{
					kubevirtv1.DomainAnnotation:                             "vm",
					kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
				}
				var err error
				pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations,
					&util.PodAnnotation{IPs: ips, MAC: util.IPAddrToHWAddr(ips[0].IP)}, nad)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				clients := util.GetOVNClientset(pod,
					&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "replacement-node"}},
					&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "target-node"}},
				).GetOVNKubeControllerClientset()
				wf, err := factory.NewOVNKubeControllerWatchFactory(clients, "replacement-node")
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(wf.Start()).To(gomega.Succeed())
				t.Cleanup(wf.Shutdown)
				nbClient, dbContext, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				t.Cleanup(dbContext.Cleanup)
				stopChan := make(chan struct{})
				t.Cleanup(func() { close(stopChan) })
				oc := &DefaultNetworkController{
					BaseNetworkController: BaseNetworkController{
						CommonNetworkControllerInfo: CommonNetworkControllerInfo{watchFactory: wf, nodeName: "replacement-node", nbClient: nbClient},
						ReconcilableNetInfo:         &util.DefaultNetInfo{},
						lsManager:                   logicalswitchmanager.NewLogicalSwitchManager(),
						logicalPortCache:            NewPortCache(stopChan),
					},
					externalGatewayRouteInfo: apbroutecontroller.NewExternalGatewayRouteInfoCache(),
				}
				// The source node is gone; this node may inherit either or both of its subnets.
				g.Expect(oc.createNodeLogicalSwitch("replacement-node", ovntest.MustParseIPNets(tc.subnets...), "", "")).To(gomega.Succeed())
				for range 2 {
					g.Expect(oc.syncPods([]interface{}{pod})).To(gomega.Succeed())
				}
				_, _, annotation, err := oc.allocateSyncMigratablePodIPsOnZone(map[ktypes.NamespacedName]bool{}, pod)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(annotation.IPs).To(gomega.Equal(ips), "filtering reservations must not change the VM's annotation")
				annotation, release, err := oc.allocatePodAnnotation(pod, nil, "namespace/migrated-vm", nad, nil, ovntypes.NetworkRolePrimary)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(annotation.IPs).To(gomega.Equal(ips))
				g.Expect(release).To(gomega.BeFalse())
				if oc.isPodScheduledOnLocalNode(pod) && len(localIPs) != 0 {
					other := pod.DeepCopy()
					other.UID, other.Name = "other", "other"
					other.Annotations[kubevirtv1.DomainAnnotation] = "other-vm"
					_, _, err = oc.allocatePodAnnotation(other, nil, "namespace/other", nad, nil, ovntypes.NetworkRolePrimary)
					g.Expect(errors.Is(err, ipallocator.ErrAllocatedByOther)).To(gomega.BeTrue())
				}
				owner := oc.podIPOwner(pod, nad)
				ownedIPs := []*net.IPNet{}
				for _, ip := range ips {
					if oc.lsManager.OwnsIPs("replacement-node", owner, []*net.IPNet{ip}) {
						ownedIPs = append(ownedIPs, ip)
					}
				}
				g.Expect(ownedIPs).To(gomega.Equal(localIPs))
				var portInfo *lpInfo
				if oc.isPodScheduledOnLocalNode(pod) {
					lsp := &nbdb.LogicalSwitchPort{
						Name:      oc.GetLogicalPortName(pod, nad),
						Addresses: []string{annotation.MAC.String() + " " + util.JoinIPNetIPs(ips, " ")},
					}
					g.Expect(libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(nbClient,
						&nbdb.LogicalSwitch{Name: podNode}, lsp)).To(gomega.Succeed())
					portInfo = oc.logicalPortCache.add(pod, podNode, nad, lsp.UUID, annotation.MAC, ips)
				}
				g.Expect(oc.removePod(pod, portInfo)).To(gomega.Succeed())
				if portInfo != nil {
					g.Eventually(func() ([]*nbdb.LogicalSwitchPort, error) {
						return libovsdbops.FindLogicalSwitchPortWithPredicate(nbClient, func(lsp *nbdb.LogicalSwitchPort) bool {
							return lsp.Name == portInfo.name
						})
					}).Should(gomega.BeEmpty())
				}
				for _, ip := range localIPs {
					g.Expect(ipallocator.IsErrAllocated(oc.lsManager.AllocateIPs("replacement-node", "next-owner", []*net.IPNet{ip}))).To(gomega.BeTrue())
				}

				pod.Status.Phase = corev1.PodSucceeded
				pod, err = clients.KubeClient.CoreV1().Pods(pod.Namespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Eventually(func() bool {
					current, err := wf.GetPod(pod.Namespace, pod.Name)
					return err == nil && util.PodCompleted(current)
				}).Should(gomega.BeTrue())
				g.Expect(oc.removePod(pod, portInfo)).To(gomega.Succeed())
				if len(localIPs) != 0 {
					g.Expect(oc.lsManager.AllocateIPs("replacement-node", "next-owner", localIPs)).To(gomega.Succeed())
					g.Expect(oc.removePod(pod, portInfo)).To(gomega.Succeed())
					g.Expect(oc.lsManager.OwnsIPs("replacement-node", "next-owner", localIPs)).To(gomega.BeTrue())
				}
			})
		}
	}
}

func TestPodIPCleanupOwnership(t *testing.T) {
	g := gomega.NewWithT(t)
	// No informer is needed: the allocator, not a pod annotation, proves ownership.
	oc := &DefaultNetworkController{BaseNetworkController: BaseNetworkController{
		ReconcilableNetInfo: &util.DefaultNetInfo{},
		lsManager:           logicalswitchmanager.NewLogicalSwitchManager(),
	}}
	pod := ovntest.NewPod("namespace", "pod", "node1", "")
	replacement := pod.DeepCopy()
	replacement.UID = "replacement"
	nad := ovntypes.DefaultNetworkName
	ips := ovntest.MustParseIPNets("10.128.0.3/24", "fd00::3/64")
	portInfo := &lpInfo{logicalSwitch: "node1", ips: ips}
	g.Expect(oc.lsManager.AddOrUpdateSwitch("node1", ovntest.MustParseIPNets("10.128.0.0/24", "fd00::/64"), nil, nil)).To(gomega.Succeed())
	g.Expect(oc.canCleanupPodIPResources(pod, "node1", nil)).To(gomega.BeTrue())
	g.Expect(oc.lsManager.AllocateIPs("node1", oc.podIPOwner(pod, nad), ips)).To(gomega.Succeed())
	g.Expect(oc.canCleanupPodIPResources(pod, "node1", ips)).To(gomega.BeTrue())
	g.Expect(oc.releasePodIPs(pod, nad, portInfo)).To(gomega.Succeed())
	g.Expect(oc.canCleanupPodIPResources(pod, "node1", ips)).To(gomega.BeFalse())

	g.Expect(oc.lsManager.AllocateIPs("node1", oc.podIPOwner(replacement, nad), ips)).To(gomega.Succeed())
	g.Expect(oc.canCleanupPodIPResources(pod, "node1", ips)).To(gomega.BeFalse())
	g.Expect(oc.releasePodIPs(pod, nad, portInfo)).To(gomega.Succeed())
	g.Expect(oc.lsManager.OwnsIPs("node1", oc.podIPOwner(replacement, nad), ips)).To(gomega.BeTrue())
}

// TestBaseNetworkController_allocatesPodAnnotation pins who writes the
// pod-networks annotation per topology/IPAM combination. The DHCP row is the
// single-writer contract: the CNI picks the MAC and reports the DHCP-learned
// IPs, so this controller must never allocate (nor overwrite) the entry.
func TestBaseNetworkController_allocatesPodAnnotation(t *testing.T) {
	tests := []struct {
		name     string
		netconf  *ovncnitypes.NetConf
		expected bool
	}{
		{
			name: "localnet with subnets (OVN-K IPAM): cluster manager allocates",
			netconf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "localnet-ipam"},
				Topology: ovntypes.LocalnetTopology,
				NADName:  "default/localnet-ipam",
				Subnets:  "10.128.0.0/16",
			},
			expected: false,
		},
		{
			name: "localnet without subnets (ipam-less): this controller allocates the MAC-only entry",
			netconf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "localnet-ipamless"},
				Topology: ovntypes.LocalnetTopology,
				NADName:  "default/localnet-ipamless",
			},
			expected: true,
		},
		{
			name: "localnet with DHCP IPAM: the CNI is the single writer",
			netconf: &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: "localnet-dhcp",
					IPAM: cnitypes.IPAM{Type: ovntypes.IPAMTypeDHCP},
				},
				Topology: ovntypes.LocalnetTopology,
				NADName:  "default/localnet-dhcp",
			},
			expected: false,
		},
		{
			name: "layer2: cluster manager allocates",
			netconf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2"},
				Topology: ovntypes.Layer2Topology,
				NADName:  "default/l2",
				Subnets:  "10.129.0.0/16",
			},
			expected: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := util.NewNetInfo(tt.netconf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			bnc := &BaseNetworkController{ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo)}

			g.Expect(bnc.allocatesPodAnnotation()).To(gomega.Equal(tt.expected))
		})
	}
}

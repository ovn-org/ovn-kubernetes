// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"sigs.k8s.io/knftables"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// TestAdvertisedServiceRoutingTransitions covers mark removal and restoration
// across overlay, no-overlay, and EVPN routing decisions.
func TestAdvertisedServiceRoutingTransitions(t *testing.T) {
	for _, transport := range []string{"", types.NetworkTransportNoOverlay, types.NetworkTransportEVPN} {
		t.Run("transport="+transport, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "reset test configuration")
			t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "reset test configuration") })
			netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{Name: "blue"}, Role: types.NetworkRolePrimary, Topology: types.Layer3Topology, Transport: transport,
			})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "create transport-specific network info")
			nft := nodenft.SetFakeNFTablesHelper()
			tx := nft.NewTransaction()
			tx.Add(&knftables.Chain{Name: GetUDNMarkChain("4099")})
			tx.Add(&knftables.Map{Name: nftablesUDNMarkNodePortsMap, Type: "inet_proto . inet_service : verdict"})
			tx.Add(&knftables.Map{Name: nftablesUDNMarkExternalIPsV4Map, Type: "ipv4_addr . inet_proto . inet_service : verdict"})
			tx.Add(&knftables.Map{Name: nftablesUDNMarkExternalIPsV6Map, Type: "ipv6_addr . inet_proto . inet_service : verdict"})
			g.Expect(nft.Run(context.Background(), tx)).To(gomega.Succeed(), "create nftables maps and mark chains")
			service := &corev1.Service{Spec: corev1.ServiceSpec{
				Type:        corev1.ServiceTypeNodePort,
				Ports:       []corev1.ServicePort{{Protocol: corev1.ProtocolTCP, Port: 80, NodePort: 30080}},
				ExternalIPs: []string{"192.0.2.1", "2001:db8::1"},
			}}
			bridge := &bridgeconfig.BridgeUDNConfiguration{PktMark: "4099"}
			// Seed legacy marks so the EVPN case also verifies their removal.
			g.Expect(nodenft.AddObjects(getUDNNFTRules(service, bridge))).To(gomega.Succeed(), "seed legacy service marks")
			nc := &UserDefinedNodeNetworkController{}
			nc.name = "node"
			old := util.NewMutableNetInfo(netInfo)
			previousMarks := shouldAddUDNServiceMarkRules(old, "node")
			for _, vrfs := range [][]string{nil, {types.DefaultNetworkName}, {"blue"}, nil, {types.DefaultNetworkName}} {
				current := util.NewMutableNetInfo(netInfo)
				current.SetPodNetworkAdvertisedVRFs(map[string][]string{"node": vrfs})
				expected := transport == "" && len(vrfs) == 0 || transport != types.NetworkTransportEVPN && len(vrfs) > 0 && vrfs[0] == types.DefaultNetworkName
				g.Expect(shouldAddUDNServiceMarkRules(current, "node")).To(gomega.Equal(expected), "mark eligibility for VRFs %v", vrfs)
				if previousMarks != expected {
					g.Expect(nc.shouldReconcileNetworkChange(old, current)).To(gomega.BeTrue(), "service mark changes must trigger gateway reconciliation")
				}
				if transport == types.NetworkTransportEVPN {
					g.Expect(nc.shouldReconcileNetworkChange(old, current)).To(gomega.BeFalse(), "EVPN's routing decision is independent of the mapping")
				}
				g.Expect(reconcileUDNServiceMarkRules(service, current, "node", bridge)).To(gomega.Succeed(), "reconcile marks for VRFs %v", vrfs)
				for _, name := range []string{nftablesUDNMarkNodePortsMap, nftablesUDNMarkExternalIPsV4Map, nftablesUDNMarkExternalIPsV6Map} {
					elements, err := nft.ListElements(context.Background(), "map", name)
					g.Expect(err).NotTo(gomega.HaveOccurred(), "list service mark map")
					count := 0
					if expected {
						count = 1
					}
					g.Expect(elements).To(gomega.HaveLen(count), "map %s, target VRFs %v", name, vrfs)
				}
				old = current
				previousMarks = expected
			}
		})
	}
}

// TestExistingServiceRefreshesAdvertisementMarks exercises the retry add path
// after an advertisement change without changing Service or EndpointSlice fields.
func TestExistingServiceRefreshesAdvertisementMarks(t *testing.T) {
	// Third-party fake clients cannot serve WatchList bookmarks.
	previousWatchList := utilfeature.DefaultFeatureGate.Enabled("WatchListClient")
	if err := utilfeature.DefaultMutableFeatureGate.SetFromMap(map[string]bool{"WatchListClient": false}); err != nil {
		t.Fatalf("failed to disable WatchListClient feature gate: %v", err)
	}
	t.Cleanup(func() {
		if err := utilfeature.DefaultMutableFeatureGate.SetFromMap(map[string]bool{"WatchListClient": previousWatchList}); err != nil {
			t.Errorf("failed to restore WatchListClient feature gate: %v", err)
		}
	})
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "reset test configuration")
	t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "reset test configuration") })
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.IPv4Mode = true
	config.IPv6Mode = false
	util.SetFakeIPTablesHelpers()
	nft := nodenft.SetFakeNFTablesHelper()
	g.Expect(initGatewayNFTables()).To(gomega.Succeed(), "initialize gateway nftables")
	g.Expect(configureUDNServicesNFTables()).To(gomega.Succeed(), "initialize UDN service nftables")
	clients := util.GetOVNClientset()
	wf, err := factory.NewNodeWatchFactory(clients.GetNodeClientset(), "node")
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create node watch factory")
	t.Cleanup(wf.Shutdown)
	g.Expect(wf.Start()).To(gomega.Succeed(), "start node watch factory")
	info, err := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: cnitypes.NetConf{Name: "blue"}, Role: types.NetworkRolePrimary, Topology: types.Layer3Topology})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create blue network info")
	mutable := util.NewMutableNetInfo(info)
	npw := initFakeNodePortWatcher()
	npw.watchFactory = wf
	npw.networkManager = &mockNetworkManagerWithActiveUDN{netInfo: mutable}
	npw.nodeIPManager = newAddressManagerInternal("node", &kube.Kube{KClient: clients.KubeClient}, nil, wf, nil, nil, false)
	g.Expect(npw.ofm.defaultBridge.AddNetworkConfig(mutable, nil, nil, 3, 4099, nil, nil)).To(gomega.Succeed(), "add blue network bridge configuration")
	bridge := npw.ofm.defaultBridge.GetNetworkConfig("blue")
	bridge.OfPortPatch = "2"
	tx := nft.NewTransaction()
	tx.Add(&knftables.Chain{Name: GetUDNMarkChain(bridge.PktMark)})
	g.Expect(nft.Run(context.Background(), tx)).To(gomega.Succeed(), "create nftables maps and mark chains")
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"}, Spec: corev1.ServiceSpec{
		Type: corev1.ServiceTypeNodePort, ClusterIP: "10.96.0.1", ClusterIPs: []string{"10.96.0.1"},
		Ports: []corev1.ServicePort{{Protocol: corev1.ProtocolTCP, Port: 80, NodePort: 30080}},
	}}
	for _, vrfs := range [][]string{nil, {"blue"}, {types.DefaultNetworkName}, {"blue"}, nil} {
		mutable.SetPodNetworkAdvertisedVRFs(map[string][]string{"node": vrfs})
		g.Expect(npw.AddService(service)).To(gomega.Succeed(), "refresh unchanged service for VRFs %v", vrfs)
		elems, err := nft.ListElements(context.Background(), "map", nftablesUDNMarkNodePortsMap)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "list NodePort mark map")
		expected := 1
		if len(vrfs) > 0 && vrfs[0] == "blue" {
			expected = 0
		}
		g.Expect(elems).To(gomega.HaveLen(expected), "target VRFs %v", vrfs)
	}
}

// TestServiceMarkCleanupPreservesOtherNetworks covers overlapping ExternalIP and
// LoadBalancer tuples when an unrelated network loses mark eligibility.
func TestServiceMarkCleanupPreservesOtherNetworks(t *testing.T) {
	g := gomega.NewWithT(t)
	nft := nodenft.SetFakeNFTablesHelper()
	g.Expect(configureUDNServicesNFTables()).To(gomega.Succeed(), "initialize shared service maps")
	bridges := []*bridgeconfig.BridgeUDNConfiguration{{PktMark: "4099"}, {PktMark: "4100"}}
	tx := nft.NewTransaction()
	for _, bridge := range bridges {
		tx.Add(&knftables.Chain{Name: GetUDNMarkChain(bridge.PktMark)})
	}
	g.Expect(nft.Run(context.Background(), tx)).To(gomega.Succeed(), "create network mark chains")
	info, err := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: cnitypes.NetConf{Name: "blue"}, Role: types.NetworkRolePrimary, Topology: types.Layer3Topology})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create blue network info")
	blue := util.NewMutableNetInfo(info)
	info, err = util.NewNetInfo(&ovncnitypes.NetConf{NetConf: cnitypes.NetConf{Name: "red"}, Role: types.NetworkRolePrimary, Topology: types.Layer3Topology})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create red network info")
	red := util.NewMutableNetInfo(info)
	red.SetPodNetworkAdvertisedVRFs(map[string][]string{"node": {"red"}})
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "blue"}, Spec: corev1.ServiceSpec{
		Type:        corev1.ServiceTypeLoadBalancer,
		Ports:       []corev1.ServicePort{{Protocol: corev1.ProtocolTCP, Port: 80, NodePort: 30080}},
		ExternalIPs: []string{"192.0.2.1", "2001:db8::1"},
	}, Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{
		{IP: "192.0.2.1"}, {IP: "2001:db8::1"}, {IP: "192.0.2.2"}, {IP: "2001:db8::2"},
	}}}}
	other := service.DeepCopy()
	other.Namespace = "red"
	other.Spec.Ports[0].NodePort = 30081
	g.Expect(reconcileUDNServiceMarkRules(service, blue, "node", bridges[0])).To(gomega.Succeed(), "program blue marks")
	for i := 0; i < 2; i++ {
		g.Expect(reconcileUDNServiceMarkRules(other, red, "node", bridges[1])).To(gomega.Succeed(), "refresh ineligible red service, attempt %d", i)
		for _, name := range []string{nftablesUDNMarkExternalIPsV4Map, nftablesUDNMarkExternalIPsV6Map} {
			elems, err := nft.ListElements(context.Background(), "map", name)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "list %s", name)
			g.Expect(elems).To(gomega.HaveLen(2), "preserve blue's ExternalIP and LoadBalancer marks in %s", name)
			for _, elem := range elems {
				g.Expect(elem.Value).To(gomega.Equal([]string{"jump " + GetUDNMarkChain(bridges[0].PktMark)}), "retain blue ownership in %s", name)
			}
		}
	}
	blue.SetPodNetworkAdvertisedVRFs(map[string][]string{"node": {"blue"}})
	for i := 0; i < 2; i++ {
		g.Expect(reconcileUDNServiceMarkRules(service, blue, "node", bridges[0])).To(gomega.Succeed(), "remove own marks idempotently, attempt %d", i)
	}
	for _, name := range []string{nftablesUDNMarkExternalIPsV4Map, nftablesUDNMarkExternalIPsV6Map} {
		elems, err := nft.ListElements(context.Background(), "map", name)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "list cleaned map %s", name)
		g.Expect(elems).To(gomega.BeEmpty(), "remove only the owner's marks in %s", name)
	}
}

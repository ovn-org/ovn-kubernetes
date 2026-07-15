// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package managementport

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/stretchr/testify/mock"
	"github.com/urfave/cli/v2"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/knftables"
	anpfake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressipv1fake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressservicefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	networkqosfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var tmpDir string

var _ = AfterSuite(func() {
	err := os.RemoveAll(tmpDir)
	Expect(err).NotTo(HaveOccurred())
})

type managementPortTestConfig struct {
	family int

	clusterCIDR string
	serviceCIDR string
	nodeSubnet  string

	expectedManagementPortIP string
	expectedGatewayIP        string

	isRoutingAdvertised bool
	isNoOverlay         bool
}

func (mptc *managementPortTestConfig) GetNodeSubnetCIDR() *net.IPNet {
	return ovntest.MustParseIPNet(mptc.nodeSubnet)
}

func (mptc *managementPortTestConfig) GetMgtPortAddr() *netlink.Addr {
	mpCIDR := &net.IPNet{
		IP:   ovntest.MustParseIP(mptc.expectedManagementPortIP),
		Mask: mptc.GetNodeSubnetCIDR().Mask,
	}
	mgtPortAddrs, err := netlink.ParseAddr(mpCIDR.String())
	Expect(err).NotTo(HaveOccurred())
	return mgtPortAddrs
}

// checkMgmtPortTestNFTables validates nftables rules for management port
func checkMgmtPortTestNFTables(configs []managementPortTestConfig, mgmtPortName string) {
	nft, err := nodenft.GetNFTablesHelper()
	Expect(err).NotTo(HaveOccurred())
	rules, err := nft.ListRules(context.Background(), nftMgmtPortChain)
	Expect(err).NotTo(HaveOccurred())

	var returnRule, snatV4Rule, snatV6Rule string
	var wantReturnRule, wantSNATV4Rule, wantSNATV6Rule bool
	var returnNonLocalV4Rule, returnNonLocalV6Rule, returnMgmtIPV4Rule, returnMgmtIPV6Rule string
	var wantReturnNonLocalV4Rule, wantReturnNonLocalV6Rule, wantReturnMgmtIPV4Rule, wantReturnMgmtIPV6Rule bool

	returnRule = fmt.Sprintf("oifname != %s return", mgmtPortName)
	wantReturnRule = true

	for _, cfg := range configs {
		isPodNetworkAdvertised := cfg.isRoutingAdvertised || cfg.isNoOverlay
		if cfg.family == netlink.FAMILY_V4 {
			snatV4Rule = "snat ip to " + cfg.expectedManagementPortIP
			wantSNATV4Rule = true
			returnNonLocalV4Rule = "meta nfproto ipv4 fib saddr type != local"
			wantReturnNonLocalV4Rule = isPodNetworkAdvertised
			returnMgmtIPV4Rule = "meta nfproto ipv4 ip saddr " + cfg.expectedManagementPortIP
			wantReturnMgmtIPV4Rule = true
		} else {
			snatV6Rule = "snat ip6 to " + cfg.expectedManagementPortIP
			wantSNATV6Rule = true
			returnNonLocalV6Rule = "meta nfproto ipv6 fib saddr type != local"
			wantReturnNonLocalV6Rule = isPodNetworkAdvertised
			returnMgmtIPV6Rule = "meta nfproto ipv6 ip6 saddr " + cfg.expectedManagementPortIP
			wantReturnMgmtIPV6Rule = true
		}
	}

	for _, rule := range rules {
		if wantReturnRule && strings.Contains(rule.Rule, returnRule) {
			wantReturnRule = false
		} else if wantSNATV4Rule && strings.Contains(rule.Rule, snatV4Rule) {
			wantSNATV4Rule = false
		} else if wantSNATV6Rule && strings.Contains(rule.Rule, snatV6Rule) {
			wantSNATV6Rule = false
		} else if wantReturnNonLocalV4Rule && strings.Contains(rule.Rule, returnNonLocalV4Rule) {
			wantReturnNonLocalV4Rule = false
		} else if wantReturnNonLocalV6Rule && strings.Contains(rule.Rule, returnNonLocalV6Rule) {
			wantReturnNonLocalV6Rule = false
		} else if wantReturnMgmtIPV4Rule && strings.Contains(rule.Rule, returnMgmtIPV4Rule) {
			wantReturnMgmtIPV4Rule = false
		} else if wantReturnMgmtIPV6Rule && strings.Contains(rule.Rule, returnMgmtIPV6Rule) {
			wantReturnMgmtIPV6Rule = false
		}
	}

	Expect(wantReturnRule).To(BeFalse(), "did not find rule with %q", returnRule)
	Expect(wantSNATV4Rule).To(BeFalse(), "did not find rule with %q", snatV4Rule)
	Expect(wantSNATV6Rule).To(BeFalse(), "did not find rule with %q", snatV6Rule)
	Expect(wantReturnNonLocalV4Rule).To(BeFalse(), "did not find rule with %q", returnNonLocalV4Rule)
	Expect(wantReturnNonLocalV6Rule).To(BeFalse(), "did not find rule with %q", returnNonLocalV6Rule)
	Expect(wantReturnMgmtIPV4Rule).To(BeFalse(), "did not find rule with %q", returnMgmtIPV4Rule)
	Expect(wantReturnMgmtIPV6Rule).To(BeFalse(), "did not find rule with %q", returnMgmtIPV6Rule)
}

// checkMgmtTestPortIpsAndRoutes checks IPs and Routes of the management port
func checkMgmtTestPortIpsAndRoutes(
	g Gomega,
	configs []managementPortTestConfig,
	mgmtPortName string,
	mgtPortAddrs []*netlink.Addr,
	expectedLRPMAC string,
) {
	mgmtPortLink, err := netlink.LinkByName(mgmtPortName)
	g.Expect(err).NotTo(HaveOccurred())
	for i, cfg := range configs {
		// Check whether IP has been added
		addrs, err := netlink.AddrList(mgmtPortLink, cfg.family)
		g.Expect(err).NotTo(HaveOccurred())
		var foundAddr bool
		for _, a := range addrs {
			if a.IP.Equal(mgtPortAddrs[i].IP) && bytes.Equal(a.Mask, mgtPortAddrs[i].Mask) {
				foundAddr = true
				break
			}
		}
		g.Expect(foundAddr).To(BeTrue(), "did not find expected management port IP %s", mgtPortAddrs[i].String())

		// Check whether the routes have been added
		gatewayIP := ovntest.MustParseIP(cfg.expectedGatewayIP)
		subnets := []string{cfg.clusterCIDR}
		for _, subnet := range subnets {
			dstIPnet := ovntest.MustParseIPNet(subnet)
			route := &netlink.Route{Dst: dstIPnet}
			filterMask := netlink.RT_FILTER_DST
			foundRoute := false
			routes, err := netlink.RouteListFiltered(cfg.family, route, filterMask)
			g.Expect(err).ToNot(HaveOccurred())
			for _, r := range routes {
				if r.Gw.Equal(gatewayIP) && r.LinkIndex == mgmtPortLink.Attrs().Index {
					foundRoute = true
					break
				}
			}
			g.Expect(foundRoute).To(BeTrue(), "did not find expected route to %s", subnet)
			if cfg.isNoOverlay {
				route := &netlink.Route{Dst: dstIPnet, Table: ovnkubeMgmPortRT}
				filterMask := netlink.RT_FILTER_DST | netlink.RT_FILTER_TABLE
				foundRoute := false
				routes, err := netlink.RouteListFiltered(cfg.family, route, filterMask)
				g.Expect(err).ToNot(HaveOccurred())
				for _, r := range routes {
					if r.Gw.Equal(gatewayIP) && r.LinkIndex == mgmtPortLink.Attrs().Index {
						foundRoute = true
						break
					}
				}
				g.Expect(foundRoute).To(BeTrue(), "did not find expected no-overlay table %d route to %s", ovnkubeMgmPortRT, subnet)

				rule := netlink.NewRule()
				rule.Priority = ovnkubeNoOverlayPodRulePriority
				rule.Table = ovnkubeMgmPortRT
				rule.IifName = "lo"
				rule.Dst = dstIPnet
				rule.Family = cfg.family
				foundRule, err := noOverlayPodRoutingRuleExists(rule)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(foundRule).To(BeTrue(), "did not find expected no-overlay pod routing rule to %s", subnet)
			}
		}

		// Check whether router IP has been added in the arp entry for mgmt port
		neighbours, err := netlink.NeighList(mgmtPortLink.Attrs().Index, cfg.family)
		g.Expect(err).NotTo(HaveOccurred())
		var foundNeighbour bool
		for _, neighbour := range neighbours {
			if neighbour.IP.Equal(gatewayIP) && (neighbour.HardwareAddr.String() == expectedLRPMAC) {
				foundNeighbour = true
				break
			}
		}
		g.Expect(foundNeighbour).To(BeTrue())
	}
}

func testManagementPort(ctx *cli.Context, fexec *ovntest.FakeExec, testNS ns.NetNS,
	configs []managementPortTestConfig, expectedLRPMAC string, legacy bool) {
	const (
		nodeName      string = "node1"
		mgtPort       string = types.K8sMgmtIntfName
		legacyMgtPort string = types.K8sPrefix + nodeName
		mtu           string = "1400"
	)

	mgmtPortMAC := util.IPAddrToHWAddr(net.ParseIP(configs[0].expectedManagementPortIP))
	if legacy {
		mgmtPortMAC, _ = net.ParseMAC("00:11:22:33:44:55")
	}

	// generic setup
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgtPort,
		Output: "internal," + mgtPort,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgtPort + "_0",
		Output: "internal," + mgtPort + "_0",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 -- --if-exists del-port br-int " + legacyMgtPort + " -- --may-exist add-port br-int " + mgtPort + " -- set interface " + mgtPort + " mac=\"" + mgmtPortMAC.String() + "\"" + " type=internal mtu_request=" + mtu + " external-ids:iface-id=" + legacyMgtPort,
	})
	var isRoutingAdvertised, isNoOverlay bool
	for _, cfg := range configs {
		// We do not enable per-interface forwarding for IPv6 for this test suite
		if cfg.family == netlink.FAMILY_V4 {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
				Output: "net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
			})
		}
		isRoutingAdvertised = isRoutingAdvertised || cfg.isRoutingAdvertised
		isNoOverlay = isNoOverlay || cfg.isNoOverlay
	}

	err := util.SetExec(fexec)
	Expect(err).NotTo(HaveOccurred())

	nodeSubnetCIDRs := make([]*net.IPNet, len(configs))
	mgtPortAddrs := make([]*netlink.Addr, len(configs))
	netInfo := &multinetworkmocks.NetInfo{}

	for i, cfg := range configs {
		nodeSubnetCIDRs[i] = cfg.GetNodeSubnetCIDR()
		mgtPortAddrs[i] = cfg.GetMgtPortAddr()
		netInfo.On("GetNodeGatewayIP", nodeSubnetCIDRs[i]).Return(util.GetNodeGatewayIfAddr(nodeSubnetCIDRs[i]))
		netInfo.On("GetNodeManagementIP", nodeSubnetCIDRs[i]).Return(util.GetNodeManagementIfAddr(nodeSubnetCIDRs[i]))
	}

	existingNode := corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: nodeName,
	}}

	if legacy {
		existingNode.Annotations = map[string]string{
			util.OvnNodeManagementPortMacAddresses: fmt.Sprintf("{\"default\":%q}", mgmtPortMAC)}
	}

	fakeClient := fake.NewSimpleClientset(&corev1.NodeList{
		Items: []corev1.Node{existingNode},
	})
	fakeNodeClient := &util.OVNNodeClientset{
		KubeClient: fakeClient,
	}

	transport := ""
	if isNoOverlay {
		transport = types.NetworkTransportNoOverlay
	}
	netInfo.On("Transport").Return(transport)
	if isRoutingAdvertised {
		netInfo.On("GetPodNetworkAdvertisedOnNodeVRFs", nodeName).Return([]string{"vrf"})
	} else {
		netInfo.On("GetPodNetworkAdvertisedOnNodeVRFs", nodeName).Return(nil)
	}
	_, err = config.InitConfig(ctx, fexec, nil)
	Expect(err).NotTo(HaveOccurred())
	if isNoOverlay {
		config.Default.Transport = types.NetworkTransportNoOverlay
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		config.NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled
		config.NoOverlay.Routing = config.NoOverlayRoutingManaged
	}
	kubeInterface := &kube.KubeOVN{Kube: kube.Kube{KClient: fakeClient}, ANPClient: anpfake.NewSimpleClientset(),
		EIPClient: egressipv1fake.NewSimpleClientset(), EgressFirewallClient: &egressfirewallfake.Clientset{},
		EgressServiceClient: &egressservicefake.Clientset{}, NetworkQoSClient: &networkqosfake.Clientset{}}
	nodeAnnotator := kube.NewNodeAnnotator(kubeInterface, existingNode.Name)
	watchFactory, err := factory.NewNodeWatchFactory(fakeNodeClient, nodeName)
	Expect(err).NotTo(HaveOccurred())
	Expect(watchFactory.Start()).To(Succeed())
	wg := &sync.WaitGroup{}
	rm := routemanager.NewController()
	stopCh := make(chan struct{})
	defer func() {
		close(stopCh)
		wg.Wait()
	}()
	wg.Add(1)
	go func() {
		defer GinkgoRecover()
		defer wg.Done()
		err := testNS.Do(func(ns.NetNS) error {
			rm.Run(stopCh, 10*time.Second)
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
	}()

	err = testNS.Do(func(ns.NetNS) error {
		defer GinkgoRecover()

		netdevName, rep := "", ""

		mgmtPortController, err := NewManagementPortController(nil, &existingNode, nodeSubnetCIDRs, netdevName, rep, rm, netInfo)
		Expect(err).NotTo(HaveOccurred())
		stop := make(chan struct{})
		err = mgmtPortController.Start(stop)
		Expect(err).NotTo(HaveOccurred())
		defer close(stop)
		Eventually(checkMgmtTestPortIpsAndRoutes).WithArguments(configs, mgtPort, mgtPortAddrs, expectedLRPMAC).Should(Succeed())
		if isNoOverlay {
			checkNoOverlayPodRouteLookup(configs, mgtPort)
		}
		return nil
	})
	Expect(err).NotTo(HaveOccurred())

	err = nodeAnnotator.Run()
	Expect(err).NotTo(HaveOccurred())

	checkMgmtPortTestNFTables(configs, mgtPort)

	Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
}

func checkNoOverlayPodRouteLookup(configs []managementPortTestConfig, mgmtPortName string) {
	mgmtPortLink, err := netlink.LinkByName(mgmtPortName)
	Expect(err).NotTo(HaveOccurred())

	bgpLink := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{Name: "bgp0"},
	}
	Expect(netlink.LinkAdd(bgpLink)).To(Succeed())
	bgpRouteLink, err := netlink.LinkByName("bgp0")
	Expect(err).NotTo(HaveOccurred())
	Expect(netlink.LinkSetUp(bgpRouteLink)).To(Succeed())

	for _, cfg := range configs {
		if !cfg.isNoOverlay {
			continue
		}
		remoteNodeSubnet := ovntest.MustParseIPNet("10.1.2.0/24")
		if cfg.family == netlink.FAMILY_V6 {
			remoteNodeSubnet = ovntest.MustParseIPNet("fda6:0:0:2::/64")
		}
		gatewayIP := ovntest.MustParseIP(cfg.expectedGatewayIP)

		Expect(netlink.RouteAdd(&netlink.Route{
			LinkIndex: bgpRouteLink.Attrs().Index,
			Dst:       remoteNodeSubnet,
			Scope:     netlink.SCOPE_LINK,
		})).To(Succeed())

		mainRoute := &netlink.Route{Dst: remoteNodeSubnet}
		routes, err := netlink.RouteListFiltered(cfg.family, mainRoute, netlink.RT_FILTER_DST)
		Expect(err).NotTo(HaveOccurred())
		Expect(routes).NotTo(BeEmpty())
		Expect(routes[0].LinkIndex).To(Equal(bgpRouteLink.Attrs().Index))

		podSubnet := ovntest.MustParseIPNet(cfg.clusterCIDR)
		tableRoute := &netlink.Route{Dst: podSubnet, Table: ovnkubeMgmPortRT}
		routes, err = netlink.RouteListFiltered(cfg.family, tableRoute, netlink.RT_FILTER_DST|netlink.RT_FILTER_TABLE)
		Expect(err).NotTo(HaveOccurred())
		Expect(routes).NotTo(BeEmpty())
		Expect(routes[0].LinkIndex).To(Equal(mgmtPortLink.Attrs().Index))
		Expect(routes[0].Gw.Equal(gatewayIP)).To(BeTrue(), "expected route gateway %s, got %s", gatewayIP, routes[0].Gw)
	}
}

func testManagementPortDPU(ctx *cli.Context, fexec *ovntest.FakeExec, testNS ns.NetNS,
	configs []managementPortTestConfig, mgmtPortNetdev string) {
	const (
		nodeName   string = "node1"
		mgtPortMAC string = "0a:58:0a:01:01:02"
		mgtPort    string = types.K8sMgmtIntfName
		mtu        int    = 1400
	)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
	}

	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgtPort,
		"ovs-vsctl --timeout=15 --if-exists get bridge br-int datapath_type",
		fmt.Sprintf("ovs-vsctl --timeout=15 -- --may-exist add-port br-int %s -- set interface %s "+
			"external-ids:iface-id=%s external-ids:%s=%s external-ids:ovn-orig-mgmt-port-rep-name=%s",
			mgtPort, mgtPort, "k8s-"+nodeName, types.OvnManagementPortNameExternalID, types.K8sMgmtIntfName, mgmtPortNetdev),
	})

	err := util.SetExec(fexec)
	Expect(err).NotTo(HaveOccurred())

	nodeSubnetCIDRs := make([]*net.IPNet, len(configs))
	netInfo := &multinetworkmocks.NetInfo{}
	for i, cfg := range configs {
		nodeSubnetCIDRs[i] = cfg.GetNodeSubnetCIDR()
		netInfo.On("GetNodeGatewayIP", nodeSubnetCIDRs[i]).Return(util.GetNodeGatewayIfAddr(nodeSubnetCIDRs[i]))
		netInfo.On("GetNodeManagementIP", nodeSubnetCIDRs[i]).Return(util.GetNodeManagementIfAddr(nodeSubnetCIDRs[i]))
	}

	existingNode := corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: nodeName,
	}}

	fakeClient := fake.NewSimpleClientset(&corev1.NodeList{
		Items: []corev1.Node{existingNode},
	})
	fakeNodeClient := &util.OVNNodeClientset{
		KubeClient: fakeClient,
	}

	netInfo.On("Transport").Return("")
	netInfo.On("GetPodNetworkAdvertisedOnNodeVRFs", nodeName).Return(nil)

	_, err = config.InitConfig(ctx, fexec, nil)
	Expect(err).NotTo(HaveOccurred())

	kubeInterface := &kube.KubeOVN{Kube: kube.Kube{KClient: fakeClient}, ANPClient: anpfake.NewSimpleClientset(), EIPClient: egressipv1fake.NewSimpleClientset(), EgressFirewallClient: &egressfirewallfake.Clientset{}, EgressServiceClient: &egressservicefake.Clientset{}, NetworkQoSClient: &networkqosfake.Clientset{}}
	nodeAnnotator := kube.NewNodeAnnotator(kubeInterface, existingNode.Name)
	watchFactory, err := factory.NewNodeWatchFactory(fakeNodeClient, nodeName)
	Expect(err).NotTo(HaveOccurred())
	Expect(watchFactory.Start()).To(Succeed())
	wg := &sync.WaitGroup{}
	rm := routemanager.NewController()
	stopCh := make(chan struct{})
	wg.Add(1)
	go func() {
		defer GinkgoRecover()
		defer wg.Done()
		err := testNS.Do(func(ns.NetNS) error {
			rm.Run(stopCh, 10*time.Second)
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
	}()
	defer func() {
		close(stopCh)
		wg.Wait()
	}()

	err = testNS.Do(func(ns.NetNS) error {
		defer GinkgoRecover()

		netdevName, rep := "pf0vf0", "pf0vf0"

		mgmtPortController, err := NewManagementPortController(nil, node, nodeSubnetCIDRs, netdevName, rep, rm, netInfo)
		Expect(err).NotTo(HaveOccurred())
		stop := make(chan struct{})
		err = mgmtPortController.Start(stop)
		Expect(err).NotTo(HaveOccurred())
		defer close(stop)
		Eventually(func(g Gomega) {
			l, err := netlink.LinkByName(mgtPort)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(l.Attrs().MTU).To(Equal(mtu))
			g.Expect(l.Attrs().Flags & net.FlagUp).To(Equal(net.FlagUp))
		}).Should(Succeed())
		return nil
	})
	Expect(err).NotTo(HaveOccurred())

	err = nodeAnnotator.Run()
	Expect(err).NotTo(HaveOccurred())
	Eventually(fexec.CalledMatchesExpected).Should(BeTrue(), fexec.ErrorDesc)
}

func testManagementPortDPUHost(ctx *cli.Context, fexec *ovntest.FakeExec, testNS ns.NetNS, configs []managementPortTestConfig) {
	const (
		nodeName   string = "node1"
		mgtPortMAC string = "0a:58:0a:01:01:02"
		mgtPort    string = types.K8sMgmtIntfName
		mtu        int    = 1400
		deviceID   string = "0000:03:00.0"
	)

	origFsOps := util.GetFileSystemOps()
	defer util.SetFileSystemOps(origFsOps)
	mockFsOps := &utilMocks.FileSystemOps{}
	mockFsOps.On("Readlink", mock.AnythingOfType("string")).Return("../../"+deviceID, nil)
	util.SetFileSystemOps(mockFsOps)

	origSriovOps := util.GetSriovnetOps()
	defer util.SetSriovnetOpsInst(origSriovOps)
	sriovMock := &utilMocks.SriovnetOps{}
	sriovMock.On("GetNetDevicesFromPci", deviceID).Return([]string{"pf0vf0"}, nil)
	util.SetSriovnetOpsInst(sriovMock)

	origVdpaOps := util.GetVdpaOps()
	defer util.SetVdpaOpsInst(origVdpaOps)
	vdpaMock := &utilMocks.VdpaOps{}
	vdpaMock.On("GetVdpaDeviceByPci", deviceID).Return(nil, fmt.Errorf("no vdpa"))
	util.SetVdpaOpsInst(vdpaMock)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
	}

	// OVS cmd setup
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgtPort,
	})

	for _, cfg := range configs {
		// We do not enable per-interface forwarding for IPv6
		if cfg.family == netlink.FAMILY_V4 {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
				Output: "net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
			})
		}
	}

	err := util.SetExec(fexec)
	Expect(err).NotTo(HaveOccurred())

	nodeSubnetCIDRs := make([]*net.IPNet, len(configs))
	mgtPortAddrs := make([]*netlink.Addr, len(configs))
	netInfo := &multinetworkmocks.NetInfo{}
	for i, cfg := range configs {
		nodeSubnetCIDRs[i] = cfg.GetNodeSubnetCIDR()
		mgtPortAddrs[i] = cfg.GetMgtPortAddr()
		netInfo.On("GetNodeGatewayIP", nodeSubnetCIDRs[i]).Return(util.GetNodeGatewayIfAddr(nodeSubnetCIDRs[i]))
		netInfo.On("GetNodeManagementIP", nodeSubnetCIDRs[i]).Return(util.GetNodeManagementIfAddr(nodeSubnetCIDRs[i]))
	}

	netInfo.On("Transport").Return("")
	netInfo.On("GetPodNetworkAdvertisedOnNodeVRFs", nodeName).Return(nil)

	_, err = config.InitConfig(ctx, fexec, nil)
	Expect(err).NotTo(HaveOccurred())
	wg := &sync.WaitGroup{}
	rm := routemanager.NewController()
	stopCh := make(chan struct{})
	wg.Add(1)
	go func() {
		defer GinkgoRecover()
		defer wg.Done()
		err := testNS.Do(func(ns.NetNS) error {
			rm.Run(stopCh, 10*time.Second)
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
	}()
	defer func() {
		close(stopCh)
		wg.Wait()
	}()
	err = testNS.Do(func(ns.NetNS) error {
		defer GinkgoRecover()

		netdevName, rep := "pf0vf0", ""

		mgmtPortController, err := NewManagementPortController(nil, node, nodeSubnetCIDRs, netdevName, rep, rm, netInfo)
		Expect(err).NotTo(HaveOccurred())
		stop := make(chan struct{})
		err = mgmtPortController.Start(stop)
		Expect(err).NotTo(HaveOccurred())
		defer close(stop)
		Eventually(func(g Gomega) {
			l, err := netlink.LinkByName(mgtPort)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(l.Attrs().HardwareAddr.String()).To(Equal(mgtPortMAC))
			g.Expect(l.Attrs().MTU).To(Equal(mtu))
			g.Expect(l.Attrs().Flags & net.FlagUp).To(Equal(net.FlagUp))
		}).Should(Succeed())

		return nil
	})
	Expect(err).NotTo(HaveOccurred())

	checkMgmtPortTestNFTables(configs, mgtPort)

	Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
}

var _ = Describe("Management Port tests", func() {
	Describe("Syncing management port", func() {
		var netlinkOpsMock *utilMocks.NetLinkOps
		var execMock *ovntest.FakeExec

		const (
			repName    = "enp3s0f0_0"
			netdevName = "enp3s0f0v0"
		)

		t := GinkgoT()
		origNetlinkOps := util.GetNetLinkOps()
		mgmtPortName := types.K8sMgmtIntfName
		netlinkMockErr := fmt.Errorf("netlink mock error")
		fakeExecErr := fmt.Errorf("face exec error")
		linkMock := &mocks.Link{}
		var (
			ovsClient  libovsdbclient.Client
			ovsCleanup *libovsdbtest.Context
		)

		BeforeEach(func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
			util.ResetRunner()

			netlinkOpsMock = &utilMocks.NetLinkOps{}
			execMock = ovntest.NewFakeExec()
			err := util.SetExec(execMock)
			Expect(err).NotTo(HaveOccurred())
			util.SetNetLinkOpMockInst(netlinkOpsMock)
			// Restore global netlink ops unconditionally — registered here so a
			// failing assertion in AfterEach cannot leak the mock to other specs.
			DeferCleanup(func() { util.SetNetLinkOpMockInst(origNetlinkOps) })
			nodenft.SetFakeNFTablesHelper()

			ovsClient, ovsCleanup, err = libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs"},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { ovsCleanup.Cleanup() })
		})

		AfterEach(func() {
			netlinkOpsMock.AssertExpectations(t)
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
		})

		Context("Reconciling no-overlay pod routing", func() {
			It("ignores no-overlay pod routing rules for other input interfaces", func() {
				podSubnet := ovntest.MustParseIPNet("10.1.0.0/16")
				rule := newNoOverlayPodRoutingRule(podSubnet)
				otherRule := *rule
				otherRule.IifName = "eth0"
				ruleFilterMask := netlink.RT_FILTER_PRIORITY | netlink.RT_FILTER_TABLE | netlink.RT_FILTER_DST
				netlinkOpsMock.On("RuleListFiltered", netlink.FAMILY_V4, rule, ruleFilterMask).Return([]netlink.Rule{otherRule}, nil)

				exists, err := noOverlayPodRoutingRuleExists(rule)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeFalse())
			})

			It("removes stale no-overlay pod routing in local gateway mode", func() {
				config.Default.Transport = types.NetworkTransportNoOverlay
				config.Gateway.Mode = config.GatewayModeLocal

				podSubnet := ovntest.MustParseIPNet("10.1.0.0/16")
				cfg := &managementPortIPFamilyConfig{
					podSubnets: []*net.IPNet{podSubnet},
				}
				rule := newNoOverlayPodRoutingRule(podSubnet)
				ruleFilterMask := netlink.RT_FILTER_PRIORITY | netlink.RT_FILTER_TABLE | netlink.RT_FILTER_DST
				netlinkOpsMock.On("RuleListFiltered", netlink.FAMILY_V4, rule, ruleFilterMask).Return([]netlink.Rule{*rule}, nil)
				netlinkOpsMock.On("RuleDel", rule).Return(nil)

				routeFilter := &netlink.Route{Dst: podSubnet, Table: ovnkubeMgmPortRT}
				routeFilterMask := netlink.RT_FILTER_DST | netlink.RT_FILTER_TABLE
				staleRoute := netlink.Route{
					LinkIndex: 10,
					Gw:        ovntest.MustParseIP("10.1.1.1"),
					Dst:       podSubnet,
					Table:     ovnkubeMgmPortRT,
				}
				netlinkOpsMock.On("RouteListFiltered", netlink.FAMILY_V4, routeFilter, routeFilterMask).Return([]netlink.Route{staleRoute}, nil)
				netlinkOpsMock.On("RouteDel", &staleRoute).Return(nil)

				err := reconcileNoOverlayPodRouteViaManagementPort(linkMock, cfg, routemanager.NewController())
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("Syncing netdevice interface", func() {
			It("Fails to lookup netdevice link", func() {
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
				})
				netlinkOpsMock.On("LinkByName", mgmtPortName).Return(nil, netlinkMockErr)
				netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(false)

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).To(HaveOccurred())
			})

			It("Fails to teardown IP configuration", func() {
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
				})
				netlinkOpsMock.On("LinkByName", mgmtPortName).Return(linkMock, nil)
				linkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: mgmtPortName})
				netlinkOpsMock.On("AddrList", linkMock, netlink.FAMILY_ALL).Return([]netlink.Addr{}, netlinkMockErr)

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).To(HaveOccurred())
			})

			It("Fails to set netdevice link down", func() {
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
				})
				// ovn-orig-mgmt-port-netdev-name is now read via libovsdb;
				// seed it on the harness instead of the fexec.
				Expect(ovsops.UpdateOpenvSwitchExternalIDs(ovsClient, map[string]string{
					"ovn-orig-mgmt-port-netdev-name": netdevName,
				})).To(Succeed())
				netlinkOpsMock.On("LinkByName", netdevName).Return(nil, netlinkMockErr)
				netlinkOpsMock.On("LinkByName", mgmtPortName).Return(linkMock, nil)
				netlinkOpsMock.On("AddrList", linkMock, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
				netlinkOpsMock.On("RouteList", linkMock, netlink.FAMILY_ALL).Return([]netlink.Route{}, nil)
				netlinkOpsMock.On("LinkSetDown", linkMock).Return(netlinkMockErr)

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).To(HaveOccurred())
			})

			It("Fails to rename netdevice link", func() {
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
				})
				// ovn-orig-mgmt-port-netdev-name is now read via libovsdb;
				// seed it on the harness instead of the fexec.
				Expect(ovsops.UpdateOpenvSwitchExternalIDs(ovsClient, map[string]string{
					"ovn-orig-mgmt-port-netdev-name": netdevName,
				})).To(Succeed())
				netlinkOpsMock.On("LinkByName", netdevName).Return(nil, netlinkMockErr)
				netlinkOpsMock.On("LinkByName", mgmtPortName).Return(linkMock, nil)
				linkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: mgmtPortName})
				netlinkOpsMock.On("AddrList", linkMock, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
				netlinkOpsMock.On("RouteList", linkMock, netlink.FAMILY_ALL).Return([]netlink.Route{}, nil)
				netlinkOpsMock.On("LinkSetDown", linkMock).Return(nil)
				netlinkOpsMock.On("LinkSetName", linkMock, netdevName).Return(netlinkMockErr)

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).To(HaveOccurred())
			})
			It("Unconfigures old management port netdevice", func() {
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
				})
				// ovn-orig-mgmt-port-netdev-name is now read via libovsdb;
				// seed it on the harness instead of the fexec.
				Expect(ovsops.UpdateOpenvSwitchExternalIDs(ovsClient, map[string]string{
					"ovn-orig-mgmt-port-netdev-name": netdevName,
				})).To(Succeed())
				netlinkOpsMock.On("LinkByName", netdevName).Return(nil, netlinkMockErr)
				netlinkOpsMock.On("LinkByName", mgmtPortName).Return(linkMock, nil)
				netlinkOpsMock.On("AddrList", linkMock, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
				netlinkOpsMock.On("RouteList", linkMock, netlink.FAMILY_ALL).Return([]netlink.Route{}, nil)
				netlinkOpsMock.On("LinkSetDown", linkMock).Return(nil)
				netlinkOpsMock.On("LinkSetName", linkMock, netdevName).Return(nil)

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("Syncing when old management port is OVS internal port", func() {
			It("Internal port found, but new one supposed to be an internal port", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
					Output: "internal," + mgmtPortName,
				})

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, true)
				Expect(err).ToNot(HaveOccurred())
			})
			It("Fails to remove port from the bridge", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
					Output: "internal," + mgmtPortName,
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 --if-exists del-port br-int " + mgmtPortName,
					Err: fakeExecErr,
				})

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).To(HaveOccurred())
			})

			It("Removes internal port from the bridge", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
					Output: "internal," + mgmtPortName,
				})
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --if-exists del-port br-int " + mgmtPortName,
				})

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("Syncing representor interface", func() {
			It("Fails to delete representor from the bridge", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
					Output: "," + mgmtPortName,
				})
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --if-exists get Interface " + mgmtPortName + " external-ids:ovn-orig-mgmt-port-rep-name",
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 --if-exists del-port br-int " + mgmtPortName,
					Err: fakeExecErr,
				})

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).To(HaveOccurred())
			})

			It("Fails to get representor original name and fallback to generic one", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
					Output: "," + mgmtPortName,
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 --if-exists get Interface " + mgmtPortName + " external-ids:ovn-orig-mgmt-port-rep-name",
					Err: fakeExecErr,
				})
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --if-exists del-port br-int " + mgmtPortName,
				})

				// Return error here, so we know that function didn't returned earlier
				netlinkOpsMock.On("LinkByName", mgmtPortName).Return(nil, netlinkMockErr)
				netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(false)
				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).To(HaveOccurred())
			})

			It("Fails to get representor link", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
					Output: "," + mgmtPortName,
				})
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --if-exists get Interface " + mgmtPortName + " external-ids:ovn-orig-mgmt-port-rep-name",
					"ovs-vsctl --timeout=15 --if-exists del-port br-int " + mgmtPortName,
				})
				netlinkOpsMock.On("LinkByName", mgmtPortName).Return(nil, netlinkMockErr)
				netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(false)

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).To(HaveOccurred())
			})

			It("Succeeds when representor link is already removed", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
					Output: "," + mgmtPortName,
				})
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --if-exists get Interface " + mgmtPortName + " external-ids:ovn-orig-mgmt-port-rep-name",
					"ovs-vsctl --timeout=15 --if-exists del-port br-int " + mgmtPortName,
				})
				netlinkOpsMock.On("LinkByName", mgmtPortName).Return(nil, netlinkMockErr)
				netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).ToNot(HaveOccurred())
			})

			It("Fails to set representor link down", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
					Output: "," + mgmtPortName,
				})
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --if-exists get Interface " + mgmtPortName + " external-ids:ovn-orig-mgmt-port-rep-name",
					"ovs-vsctl --timeout=15 --if-exists del-port br-int " + mgmtPortName,
				})
				netlinkOpsMock.On("LinkByName", mgmtPortName).Return(linkMock, nil)
				linkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: mgmtPortName})
				netlinkOpsMock.On("AddrList", linkMock, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
				netlinkOpsMock.On("LinkSetDown", linkMock).Return(netlinkMockErr)

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).To(HaveOccurred())
			})

			It("Fails to rename representor link", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
					Output: "," + mgmtPortName,
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get Interface " + mgmtPortName + " external-ids:ovn-orig-mgmt-port-rep-name",
					Output: repName,
				})
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --if-exists del-port br-int " + mgmtPortName,
				})
				netlinkOpsMock.On("LinkByName", repName).Return(nil, netlinkMockErr)
				netlinkOpsMock.On("LinkByName", mgmtPortName).Return(linkMock, nil)
				netlinkOpsMock.On("AddrList", linkMock, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
				netlinkOpsMock.On("LinkSetDown", linkMock).Return(nil)
				netlinkOpsMock.On("LinkSetName", linkMock, repName).Return(netlinkMockErr)

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).To(HaveOccurred())
			})

			It("Removes representor from the bridge", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
					Output: "," + mgmtPortName,
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get Interface " + mgmtPortName + " external-ids:ovn-orig-mgmt-port-rep-name",
					Output: repName,
				})
				execMock.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --if-exists del-port br-int " + mgmtPortName,
				})
				netlinkOpsMock.On("LinkByName", repName).Return(nil, netlinkMockErr)
				netlinkOpsMock.On("LinkByName", mgmtPortName).Return(linkMock, nil)
				netlinkOpsMock.On("AddrList", linkMock, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
				netlinkOpsMock.On("LinkSetDown", linkMock).Return(nil)
				netlinkOpsMock.On("LinkSetName", linkMock, repName).Return(nil)

				err := syncMgmtPortInterfaceWithExec(ovsClient, mgmtPortName, false)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("Syncing nftables rules", func() {
			It("removes stale nftables rules while keeping remaining intact", func() {
				nft := nodenft.SetFakeNFTablesHelper()

				netInfo := &multinetworkmocks.NetInfo{}
				nodeNet := ovntest.MustParseIPNet("10.1.1.0/24")

				netInfo.On("Transport").Return("")
				netInfo.On("GetPodNetworkAdvertisedOnNodeVRFs", "").Return(nil)
				netInfo.On("GetNodeGatewayIP", nodeNet).Return(util.GetNodeGatewayIfAddr(nodeNet))
				netInfo.On("GetNodeManagementIP", nodeNet).Return(util.GetNodeManagementIfAddr(nodeNet))
				// Make a fake MgmtPortConfig with only the fields we care about
				fakeMgmtPortIPFamilyConfig := managementPortIPFamilyConfig{
					ifAddr: nodeNet,
				}
				fakeMgmtPortConfig := managementPortConfig{
					ipv4:    &fakeMgmtPortIPFamilyConfig,
					netInfo: netInfo,
				}
				err := SetupManagementPortNFTSets()
				Expect(err).NotTo(HaveOccurred())
				err = setupManagementPortNFTChain(types.K8sMgmtIntfName, &fakeMgmtPortConfig)
				Expect(err).NotTo(HaveOccurred())

				finalExpectedNFT := nft.Dump()

				// Inject rules into SNAT MGMT chain that shouldn't exist and should be cleared on a restore, even if the chain has no rules
				tx := nft.NewTransaction()
				tx.Add(&knftables.Chain{
					Name:    nftMgmtPortChain,
					Comment: knftables.PtrTo("OVN SNAT to Management Port"),

					Type:     knftables.PtrTo(knftables.NATType),
					Hook:     knftables.PtrTo(knftables.PostroutingHook),
					Priority: knftables.PtrTo(knftables.SNATPriority),
				})
				tx.Add(&knftables.Rule{
					Chain: nftMgmtPortChain,
					Rule:  "blah blah blah",
				})
				Expect(nft.Run(context.Background(), tx)).To(Succeed())

				expectedNFT := finalExpectedNFT + "\nadd rule inet ovn-kubernetes mgmtport-snat blah blah blah\n"
				err = nodenft.MatchNFTRules(expectedNFT, nft.Dump())
				Expect(err).NotTo(HaveOccurred())

				err = setupManagementPortNFTChain(types.K8sMgmtIntfName, &fakeMgmtPortConfig)
				Expect(err).NotTo(HaveOccurred())
				err = nodenft.MatchNFTRules(finalExpectedNFT, nft.Dump())
				Expect(err).NotTo(HaveOccurred())
			})
		})
	})

	Describe("Management port controller start", func() {
		var tmpErr error
		var app *cli.App
		var testNS ns.NetNS
		var fexec *ovntest.FakeExec

		tmpDir, tmpErr = os.MkdirTemp("", "clusternodetest_certdir")
		if tmpErr != nil {
			GinkgoT().Errorf("failed to create tempdir: %v", tmpErr)
		}

		BeforeEach(func() {
			var err error
			// Restore global default values before each testcase
			Expect(config.PrepareTestConfig()).To(Succeed())

			app = cli.NewApp()
			app.Name = "test"
			app.Flags = config.Flags

			testNS, err = testutils.NewNS()
			Expect(err).NotTo(HaveOccurred())
			fexec = ovntest.NewFakeExec()
		})

		AfterEach(func() {
			Expect(testNS.Close()).To(Succeed())
			Expect(testutils.UnmountNS(testNS)).To(Succeed())
		})

		const (
			v4clusterCIDR string = "10.1.0.0/16"
			v4nodeSubnet  string = "10.1.1.0/24"
			v4gwIP        string = "10.1.1.1"
			v4mgtPortIP   string = "10.1.1.2"
			v4serviceCIDR string = "172.16.1.0/24"
			v4lrpMAC      string = "0a:58:0a:01:01:01"

			v6clusterCIDR string = "fda6::/48"
			v6nodeSubnet  string = "fda6:0:0:1::/64"
			v6gwIP        string = "fda6:0:0:1::1"
			v6mgtPortIP   string = "fda6:0:0:1::2"
			v6serviceCIDR string = "fc95::/64"
			// generated from util.IPAddrToHWAddr(net.ParseIP("fda6:0:0:1::1")).String()
			v6lrpMAC string = "0a:58:23:5a:40:f1"

			mgmtPortNetdev string = "pf0vf0"
		)

		Context("Management Port, ovnkube node mode full", func() {

			BeforeEach(func() {
				// Set up a fake k8sMgmt interface
				err := testNS.Do(func(ns.NetNS) error {
					defer GinkgoRecover()
					ovntest.AddLink(types.K8sMgmtIntfName)
					return nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("sets up the management port for IPv4 clusters", func() {
				app.Action = func(ctx *cli.Context) error {
					testManagementPort(ctx, fexec, testNS,
						[]managementPortTestConfig{
							{
								family: netlink.FAMILY_V4,

								clusterCIDR: v4clusterCIDR,
								nodeSubnet:  v4nodeSubnet,

								expectedManagementPortIP: v4mgtPortIP,
								expectedGatewayIP:        v4gwIP,
							},
						}, v4lrpMAC, false)
					return nil
				}
				err := app.Run([]string{
					app.Name,
					"--cluster-subnets=" + v4clusterCIDR,
				})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("sets up the management port for IPv4 clusters with legacy annotation", func() {
				app.Action = func(ctx *cli.Context) error {
					testManagementPort(ctx, fexec, testNS,
						[]managementPortTestConfig{
							{
								family: netlink.FAMILY_V4,

								clusterCIDR: v4clusterCIDR,
								nodeSubnet:  v4nodeSubnet,

								expectedManagementPortIP: v4mgtPortIP,
								expectedGatewayIP:        v4gwIP,
							},
						}, v4lrpMAC, true)
					return nil
				}
				err := app.Run([]string{
					app.Name,
					"--cluster-subnets=" + v4clusterCIDR,
				})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("sets up the management port for BGP advertised IPv4 clusters", func() {
				app.Action = func(ctx *cli.Context) error {
					testManagementPort(ctx, fexec, testNS,
						[]managementPortTestConfig{
							{
								family: netlink.FAMILY_V4,

								clusterCIDR: v4clusterCIDR,
								nodeSubnet:  v4nodeSubnet,

								expectedManagementPortIP: v4mgtPortIP,
								expectedGatewayIP:        v4gwIP,

								isRoutingAdvertised: true,
							},
						}, v4lrpMAC, true)
					return nil
				}
				err := app.Run([]string{
					app.Name,
					"--cluster-subnets=" + v4clusterCIDR,
				})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("sets up no-overlay pod routing through the management port for IPv4 clusters", func() {
				app.Action = func(ctx *cli.Context) error {
					testManagementPort(ctx, fexec, testNS,
						[]managementPortTestConfig{
							{
								family: netlink.FAMILY_V4,

								clusterCIDR: v4clusterCIDR,
								nodeSubnet:  v4nodeSubnet,

								expectedManagementPortIP: v4mgtPortIP,
								expectedGatewayIP:        v4gwIP,

								isNoOverlay: true,
							},
						}, v4lrpMAC, false)
					return nil
				}
				err := app.Run([]string{
					app.Name,
					"--cluster-subnets=" + v4clusterCIDR,
					"--gateway-mode=" + string(config.GatewayModeShared),
				})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("sets up no-overlay pod routing through the management port for dual-stack clusters", func() {
				app.Action = func(ctx *cli.Context) error {
					testManagementPort(ctx, fexec, testNS,
						[]managementPortTestConfig{
							{
								family: netlink.FAMILY_V4,

								clusterCIDR: v4clusterCIDR,
								serviceCIDR: v4serviceCIDR,
								nodeSubnet:  v4nodeSubnet,

								expectedManagementPortIP: v4mgtPortIP,
								expectedGatewayIP:        v4gwIP,

								isNoOverlay: true,
							},
							{
								family: netlink.FAMILY_V6,

								clusterCIDR: v6clusterCIDR,
								serviceCIDR: v6serviceCIDR,
								nodeSubnet:  v6nodeSubnet,

								expectedManagementPortIP: v6mgtPortIP,
								expectedGatewayIP:        v6gwIP,

								isNoOverlay: true,
							},
						}, v4lrpMAC, false)
					return nil
				}
				err := app.Run([]string{
					app.Name,
					"--cluster-subnets=" + v4clusterCIDR + "," + v6clusterCIDR,
					"--k8s-service-cidr=" + v4serviceCIDR + "," + v6serviceCIDR,
					"--gateway-mode=" + string(config.GatewayModeShared),
				})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("sets up the management port for IPv6 clusters", func() {
				app.Action = func(ctx *cli.Context) error {
					testManagementPort(ctx, fexec, testNS,
						[]managementPortTestConfig{
							{
								family: netlink.FAMILY_V6,

								clusterCIDR: v6clusterCIDR,
								serviceCIDR: v6serviceCIDR,
								nodeSubnet:  v6nodeSubnet,

								expectedManagementPortIP: v6mgtPortIP,
								expectedGatewayIP:        v6gwIP,
							},
						}, v6lrpMAC, false)
					return nil
				}
				err := app.Run([]string{
					app.Name,
					"--cluster-subnets=" + v6clusterCIDR,
					"--k8s-service-cidr=" + v6serviceCIDR,
				})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("sets up the management port for BGP advertised IPv6 clusters", func() {
				app.Action = func(ctx *cli.Context) error {
					testManagementPort(ctx, fexec, testNS,
						[]managementPortTestConfig{
							{
								family: netlink.FAMILY_V6,

								clusterCIDR: v6clusterCIDR,
								serviceCIDR: v6serviceCIDR,
								nodeSubnet:  v6nodeSubnet,

								expectedManagementPortIP: v6mgtPortIP,
								expectedGatewayIP:        v6gwIP,

								isRoutingAdvertised: true,
							},
						}, v6lrpMAC, true)
					return nil
				}
				err := app.Run([]string{
					app.Name,
					"--cluster-subnets=" + v6clusterCIDR,
					"--k8s-service-cidr=" + v6serviceCIDR,
				})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("sets up the management port for dual-stack clusters", func() {
				app.Action = func(ctx *cli.Context) error {
					testManagementPort(ctx, fexec, testNS,
						[]managementPortTestConfig{
							{
								family: netlink.FAMILY_V4,

								clusterCIDR: v4clusterCIDR,
								serviceCIDR: v4serviceCIDR,
								nodeSubnet:  v4nodeSubnet,

								expectedManagementPortIP: v4mgtPortIP,
								expectedGatewayIP:        v4gwIP,
							},
							{
								family: netlink.FAMILY_V6,

								clusterCIDR: v6clusterCIDR,
								serviceCIDR: v6serviceCIDR,
								nodeSubnet:  v6nodeSubnet,

								expectedManagementPortIP: v6mgtPortIP,
								expectedGatewayIP:        v6gwIP,
							},
						}, v4lrpMAC, false)
					return nil
				}
				err := app.Run([]string{
					app.Name,
					"--cluster-subnets=" + v4clusterCIDR + "," + v6clusterCIDR,
					"--k8s-service-cidr=" + v4serviceCIDR + "," + v6serviceCIDR,
				})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("sets up the management port for BGP advertised dual-stack clusters", func() {
				app.Action = func(ctx *cli.Context) error {
					testManagementPort(ctx, fexec, testNS,
						[]managementPortTestConfig{
							{
								family: netlink.FAMILY_V4,

								clusterCIDR: v4clusterCIDR,
								serviceCIDR: v4serviceCIDR,
								nodeSubnet:  v4nodeSubnet,

								expectedManagementPortIP: v4mgtPortIP,
								expectedGatewayIP:        v4gwIP,

								isRoutingAdvertised: true,
							},
							{
								family: netlink.FAMILY_V6,

								clusterCIDR: v6clusterCIDR,
								serviceCIDR: v6serviceCIDR,
								nodeSubnet:  v6nodeSubnet,

								expectedManagementPortIP: v6mgtPortIP,
								expectedGatewayIP:        v6gwIP,

								isRoutingAdvertised: true,
							},
						}, v4lrpMAC, true)
					return nil
				}
				err := app.Run([]string{
					app.Name,
					"--cluster-subnets=" + v4clusterCIDR + "," + v6clusterCIDR,
					"--k8s-service-cidr=" + v4serviceCIDR + "," + v6serviceCIDR,
				})
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("Management Port, ovnkube node mode dpu", func() {

			BeforeEach(func() {
				// Set up a fake k8sMgmt interface
				err := testNS.Do(func(ns.NetNS) error {
					defer GinkgoRecover()
					ovntest.AddLink(mgmtPortNetdev)
					return nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("sets up the management port for IPv4 dpu clusters", func() {
				app.Action = func(ctx *cli.Context) error {
					testManagementPortDPU(ctx, fexec, testNS,
						[]managementPortTestConfig{
							{
								family: netlink.FAMILY_V4,

								clusterCIDR: v4clusterCIDR,
								serviceCIDR: v4serviceCIDR,
								nodeSubnet:  v4nodeSubnet,

								expectedManagementPortIP: v4mgtPortIP,
								expectedGatewayIP:        v4gwIP,
							},
						}, mgmtPortNetdev)
					return nil
				}
				err := app.Run([]string{
					app.Name,
					"--cluster-subnets=" + v4clusterCIDR,
					"--k8s-service-cidr=" + v4serviceCIDR,
					"--ovnkube-node-mode=" + types.NodeModeDPU,
				})
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("Management Port, ovnkube node mode dpu-host", func() {
			BeforeEach(func() {
				// Set up a fake k8sMgmt interface
				err := testNS.Do(func(ns.NetNS) error {
					defer GinkgoRecover()
					ovntest.AddLink(mgmtPortNetdev)
					return nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("sets up the management port for IPv4 dpu-host clusters", func() {
				app.Action = func(ctx *cli.Context) error {
					testManagementPortDPUHost(ctx, fexec, testNS,
						[]managementPortTestConfig{
							{
								family: netlink.FAMILY_V4,

								clusterCIDR: v4clusterCIDR,
								serviceCIDR: v4serviceCIDR,
								nodeSubnet:  v4nodeSubnet,

								expectedManagementPortIP: v4mgtPortIP,
								expectedGatewayIP:        v4gwIP,
							},
						})
					return nil
				}
				err := app.Run([]string{
					app.Name,
					"--cluster-subnets=" + v4clusterCIDR,
					"--k8s-service-cidr=" + v4serviceCIDR,
					"--ovnkube-node-mode=" + types.NodeModeDPUHost,
					"--ovnkube-node-mgmt-port-netdev=" + mgmtPortNetdev,
				})
				Expect(err).NotTo(HaveOccurred())
			})
		})
	})

	Context("NewManagementPortController creates a controller according to config.OvnKubeNode.Mode", func() {
		var origFsOps util.FileSystemOps

		BeforeEach(func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
			origFsOps = util.GetFileSystemOps()
			mockFsOps := &utilMocks.FileSystemOps{}
			mockFsOps.On("Readlink", mock.AnythingOfType("string")).Return("../../0000:03:00.2", nil)
			util.SetFileSystemOps(mockFsOps)
		})

		AfterEach(func() {
			util.SetFileSystemOps(origFsOps)
		})

		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-node",
				Annotations: map[string]string{
					util.OvnNodeManagementPortMacAddresses: "{\"default\": \"00:11:22:33:44:55\"}",
				},
			},
		}
		hostSubnets := []*net.IPNet{ovntest.MustParseIPNet("10.1.1.0/24")}
		netdevName, rep := "ens1f0v0", "ens1f0_0"
		netInfo := &multinetworkmocks.NetInfo{}
		netInfo.On("Transport").Return("")
		netInfo.On("GetPodNetworkAdvertisedOnNodeVRFs", "worker-node").Return(nil)
		netInfo.On("GetNodeGatewayIP", hostSubnets[0]).Return(util.GetNodeGatewayIfAddr(hostSubnets[0]))
		netInfo.On("GetNodeManagementIP", hostSubnets[0]).Return(util.GetNodeManagementIfAddr(hostSubnets[0]))
		It("Creates managementPort by default", func() {
			mgmtPort, err := NewManagementPortController(nil, node, hostSubnets, netdevName, rep, nil, netInfo)
			Expect(err).NotTo(HaveOccurred())
			mgmtPortImpl := mgmtPort.(*managementPortController)
			Expect(mgmtPortImpl.ports[ovsPort]).ToNot(BeNil())
			Expect(mgmtPortImpl.ports[netdevPort]).To(BeNil())
			Expect(mgmtPortImpl.ports[representorPort]).To(BeNil())
		})
		It("Creates managementPortRepresentor for Ovnkube Node mode dpu", func() {
			config.OvnKubeNode.Mode = types.NodeModeDPU
			mgmtPort, err := NewManagementPortController(nil, node, hostSubnets, netdevName, rep, nil, netInfo)
			Expect(err).NotTo(HaveOccurred())
			mgmtPortImpl := mgmtPort.(*managementPortController)
			Expect(mgmtPortImpl.ports[ovsPort]).To(BeNil())
			Expect(mgmtPortImpl.ports[netdevPort]).To(BeNil())
			Expect(mgmtPortImpl.ports[representorPort]).ToNot(BeNil())
			repImpl := mgmtPortImpl.ports[representorPort].(*managementPortRepresentor)
			Expect(repImpl.repDevName).To(Equal(rep))
		})
		It("Creates managementPortNetdev for dpu-host using device ID from node annotation", func() {
			config.OvnKubeNode.Mode = types.NodeModeDPUHost
			nodeWithAnnotation := node.DeepCopy()
			nodeWithAnnotation.Annotations[util.OvnNodeManagementPort] = `{"default":{"DeviceId":"0000:05:00.7","PfId":1,"FuncId":3}}`
			mgmtPort, err := NewManagementPortController(nil, nodeWithAnnotation, hostSubnets, netdevName, rep, nil, netInfo)
			Expect(err).NotTo(HaveOccurred())
			mgmtPortImpl := mgmtPort.(*managementPortController)
			Expect(mgmtPortImpl.ports[netdevPort]).ToNot(BeNil())
			netdevImpl := mgmtPortImpl.ports[netdevPort].(*managementPortNetdev)
			Expect(netdevImpl.deviceID).To(Equal("0000:05:00.7"))
		})
		It("Creates managementPortNetdev for dpu-host falling back to sysfs when annotation is missing", func() {
			config.OvnKubeNode.Mode = types.NodeModeDPUHost
			mgmtPort, err := NewManagementPortController(nil, node, hostSubnets, netdevName, rep, nil, netInfo)
			Expect(err).NotTo(HaveOccurred())
			mgmtPortImpl := mgmtPort.(*managementPortController)
			Expect(mgmtPortImpl.ports[ovsPort]).To(BeNil())
			Expect(mgmtPortImpl.ports[netdevPort]).ToNot(BeNil())
			Expect(mgmtPortImpl.ports[representorPort]).To(BeNil())
			netdevImpl := mgmtPortImpl.ports[netdevPort].(*managementPortNetdev)
			Expect(netdevImpl.deviceID).To(Equal("0000:03:00.2"))
		})
		It("Creates managementPortNetdev for dpu-host in simulated DPU when annotation is missing", func() {
			config.OvnKubeNode.Mode = types.NodeModeDPUHost
			config.OvnKubeNode.SimulateDPU = true
			simNetdev := "eth0-1"
			mgmtPort, err := NewManagementPortController(nil, node, hostSubnets, simNetdev, rep, nil, netInfo)
			Expect(err).NotTo(HaveOccurred())
			mgmtPortImpl := mgmtPort.(*managementPortController)
			netdevImpl := mgmtPortImpl.ports[netdevPort].(*managementPortNetdev)
			Expect(netdevImpl.deviceID).To(Equal(simNetdev))
		})
		It("Fails to create managementPortNetdev when PCI resolution fails and annotation is missing", func() {
			config.OvnKubeNode.Mode = types.NodeModeDPUHost
			util.SetFileSystemOps(origFsOps)
			mockFsOpsFail := &utilMocks.FileSystemOps{}
			mockFsOpsFail.On("Readlink", mock.AnythingOfType("string")).Return("", fmt.Errorf("no such file"))
			util.SetFileSystemOps(mockFsOpsFail)

			_, err := NewManagementPortController(nil, node, hostSubnets, netdevName, rep, nil, netInfo)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get PCI device ID"))
		})
		It("Creates managementPortNetdev and managementPortRepresentor for full mode using annotation", func() {
			config.OvnKubeNode.MgmtPortNetdev = netdevName
			nodeWithAnnotation := node.DeepCopy()
			nodeWithAnnotation.Annotations[util.OvnNodeManagementPort] = `{"default":{"DeviceId":"0000:05:00.7","PfId":1,"FuncId":3}}`
			mgmtPort, err := NewManagementPortController(nil, nodeWithAnnotation, hostSubnets, netdevName, rep, nil, netInfo)
			Expect(err).NotTo(HaveOccurred())
			mgmtPortImpl := mgmtPort.(*managementPortController)
			Expect(mgmtPortImpl.ports[ovsPort]).To(BeNil())
			Expect(mgmtPortImpl.ports[netdevPort]).ToNot(BeNil())
			Expect(mgmtPortImpl.ports[representorPort]).ToNot(BeNil())
			netdevImpl := mgmtPortImpl.ports[netdevPort].(*managementPortNetdev)
			Expect(netdevImpl.deviceID).To(Equal("0000:05:00.7"))
			repImpl := mgmtPortImpl.ports[representorPort].(*managementPortRepresentor)
			Expect(repImpl.repDevName).To(Equal(rep))
		})
		It("Creates managementPortNetdev and managementPortRepresentor for full mode falling back to sysfs", func() {
			config.OvnKubeNode.MgmtPortNetdev = netdevName
			mgmtPort, err := NewManagementPortController(nil, node, hostSubnets, netdevName, rep, nil, netInfo)
			Expect(err).NotTo(HaveOccurred())
			mgmtPortImpl := mgmtPort.(*managementPortController)
			Expect(mgmtPortImpl.ports[ovsPort]).To(BeNil())
			Expect(mgmtPortImpl.ports[netdevPort]).ToNot(BeNil())
			Expect(mgmtPortImpl.ports[representorPort]).ToNot(BeNil())
			netdevImpl := mgmtPortImpl.ports[netdevPort].(*managementPortNetdev)
			Expect(netdevImpl.deviceID).To(Equal("0000:03:00.2"))
			repImpl := mgmtPortImpl.ports[representorPort].(*managementPortRepresentor)
			Expect(repImpl.repDevName).To(Equal(rep))
		})
	})
})

package node

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	. "github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	kubeMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func getCreationFakeOVSCommands(fexec *ovntest.FakeExec, mgtPort, mgtPortMAC, netName, nodeName string, mtu int) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 -- --if-exists del-port br-int " + mgtPort +
			" -- --may-exist add-port br-int " + mgtPort +
			" -- set interface " + mgtPort +
			" type=internal mtu_request=" + fmt.Sprintf("%d", mtu) +
			" external-ids:iface-id=" + types.K8sPrefix + netName + "_" + nodeName,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface " + mgtPort + " mac_in_use",
		Output: mgtPortMAC,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 set interface " + mgtPort + " " + fmt.Sprintf("mac=%s", strings.ReplaceAll(mgtPortMAC, ":", "\\:")),
	})
}

func getDeletionFakeOVSCommands(fexec *ovntest.FakeExec, mgtPort string) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 -- --if-exists del-port br-int " + mgtPort,
	})
}

func getVRFCreationFakeOVSCommands(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 port-to-br eth0",
		Output: "breth0",
	})
}

var _ = Describe("UserDefinedNetworkGateway", func() {
	var (
		netName                  = "bluenet"
		netID                    = "3"
		nodeName          string = "worker1"
		mgtPortMAC        string = "00:00:00:55:66:77"
		fexec             *ovntest.FakeExec
		testNS            ns.NetNS
		nodeAnnotatorMock *kubeMocks.Annotator
		vrf               *vrfmanager.Controller
		v4NodeSubnet      = "10.128.0.0/24"
		v6NodeSubnet      = "ae70::66/112"
		mgtPort           = fmt.Sprintf("%s%s", types.K8sMgmtIntfNamePrefix, netID)
	)
	BeforeEach(func() {
		err := config.PrepareTestConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// Set up a fake vsctl command mock interface
		fexec = ovntest.NewFakeExec()
		err = util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())
		// Set up a fake k8sMgmt interface
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ovntest.AddLink(mgtPort)
			ovntest.AddLink("breth0")
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		nodeAnnotatorMock = &kubeMocks.Annotator{}
		wg := &sync.WaitGroup{}
		vrf = vrfmanager.NewController()
		stopCh := make(chan struct{})
		defer func() {
			close(stopCh)
			wg.Wait()
		}()
		wg.Add(1)
		go testNS.Do(func(netNS ns.NetNS) error {
			defer wg.Done()
			defer GinkgoRecover()
			vrf.Run(stopCh, wg)
			return nil
		})
	})
	AfterEach(func() {
		Expect(testNS.Close()).To(Succeed())
		Expect(testutils.UnmountNS(testNS)).To(Succeed())
	})
	ovntest.OnSupportedPlatformsIt("should create management port for a L3 user defined network", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]string{netName: mgtPortMAC}).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		udnGateway := NewUserDefinedNetworkGateway(netInfo, 3, node, nodeAnnotatorMock, vrf, nil)
		Expect(err).NotTo(HaveOccurred())
		getCreationFakeOVSCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			err = udnGateway.addUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			return nil
		})

		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should delete management port for a L3 user defined network", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway := NewUserDefinedNetworkGateway(netInfo, 3, node, nil, vrf, nil)
		Expect(err).NotTo(HaveOccurred())
		getDeletionFakeOVSCommands(fexec, mgtPort)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			err = udnGateway.deleteUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			return nil
		})

		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should create management port for a L2 user defined network", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids": fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]string{netName: mgtPortMAC}).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		udnGateway := NewUserDefinedNetworkGateway(netInfo, 3, node, nodeAnnotatorMock, vrf, nil)
		Expect(err).NotTo(HaveOccurred())
		getCreationFakeOVSCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			err = udnGateway.addUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			return nil
		})

		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should delete management port for a L2 user defined network", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids": fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway := NewUserDefinedNetworkGateway(netInfo, 3, node, nil, vrf, nil)
		Expect(err).NotTo(HaveOccurred())
		getDeletionFakeOVSCommands(fexec, mgtPort)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			err = udnGateway.deleteUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			return nil
		})

		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should compute correct masquerade reply traffic routes for a user defined network", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.V6MasqueradeSubnet = "fd69::/112"
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway := NewUserDefinedNetworkGateway(netInfo, 3, node, nil, vrf, nil)
		Expect(err).NotTo(HaveOccurred())
		getVRFCreationFakeOVSCommands(fexec)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			bridgelink, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)

			routes, err := udnGateway.computeRoutesForUDN(vrfTableId, mplink)
			Expect(err).NotTo(HaveOccurred())
			klog.Info(len(routes))
			Expect(len(routes)).To(Equal(3))
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[0].Dst).To(Equal(*ovntest.MustParseIPNet("172.16.1.0/24"))) // default service subnet
			Expect(routes[0].LinkIndex).To(Equal(bridgelink.Attrs().Index))
			Expect(routes[0].Gw).To(Equal(config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP))
			cidr, err := util.GetIPNetFullMask("169.254.0.16")
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[1].Dst).To(Equal(*cidr))
			Expect(routes[1].LinkIndex).To(Equal(mplink.Attrs().Index))
			cidr, err = util.GetIPNetFullMask("fd69::10")
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[2].Dst).To(Equal(*cidr))
			Expect(routes[2].LinkIndex).To(Equal(mplink.Attrs().Index))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should compute correct service routes for a user defined network", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.V6MasqueradeSubnet = "fd69::/112"
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("10.96.0.0/16", "fd00:10:96::/112")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway := NewUserDefinedNetworkGateway(netInfo, 3, node, nil, vrf, nil)
		Expect(err).NotTo(HaveOccurred())
		getVRFCreationFakeOVSCommands(fexec)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			link, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())

			mplink, err := netlink.LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)

			routes, err := udnGateway.computeRoutesForUDN(vrfTableId, mplink)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(routes)).To(Equal(4))
			Expect(err).NotTo(HaveOccurred())
			// 1st and 2nd routes are the service routes from user-provided config value
			Expect(*routes[0].Dst).To(Equal(*config.Kubernetes.ServiceCIDRs[0]))
			Expect(routes[0].LinkIndex).To(Equal(link.Attrs().Index))
			Expect(*routes[1].Dst).To(Equal(*config.Kubernetes.ServiceCIDRs[1]))
			Expect(routes[1].LinkIndex).To(Equal(link.Attrs().Index))
			cidr, err := util.GetIPNetFullMask("169.254.0.16")
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[2].Dst).To(Equal(*cidr))
			Expect(routes[2].LinkIndex).To(Equal(mplink.Attrs().Index))
			cidr, err = util.GetIPNetFullMask("fd69::10")
			Expect(err).NotTo(HaveOccurred())
			Expect(*routes[3].Dst).To(Equal(*cidr))
			Expect(routes[3].LinkIndex).To(Equal(mplink.Attrs().Index))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should set rp filer to loose mode for management port interface", func() {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp3.rp_filter=2",
			Output: "net.ipv4.conf.ovn-k8s-mp3.rp_filter = 2",
		})
		err := testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			err := addRPFilterLooseModeForManagementPort(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
})

func TestGetUDNVRFIPRules(t *testing.T) {
	type testRule struct {
		priority int
		family   int
		table    int
		dst      net.IPNet
	}
	type testConfig struct {
		desc          string
		vrftableID    int
		v4mode        bool
		v6mode        bool
		expectedRules []testRule
	}

	tests := []testConfig{
		{
			desc:          "empty rules test",
			vrftableID:    1007,
			expectedRules: nil,
		},
		{
			desc:       "v4 rule test",
			vrftableID: 1007,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1007,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("169.254.0.16")),
				},
			},
			v4mode: true,
		},
		{
			desc:       "v6 rule test",
			vrftableID: 1009,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1009,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("fd69::10")),
				},
			},
			v6mode: true,
		},
		{
			desc:       "dualstack rule test",
			vrftableID: 1010,
			expectedRules: []testRule{
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V4,
					table:    1010,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("169.254.0.16")),
				},
				{
					priority: UDNMasqueradeIPRulePriority,
					family:   netlink.FAMILY_V6,
					table:    1010,
					dst:      *util.GetIPNetFullMaskFromIP(ovntest.MustParseIP("fd69::10")),
				},
			},
			v4mode: true,
			v6mode: true,
		},
	}
	config.Gateway.V6MasqueradeSubnet = "fd69::/112"
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			config.IPv4Mode = test.v4mode
			config.IPv6Mode = test.v6mode
			nad := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16/24,ae70::66/60", types.NetworkRolePrimary)
			netInfo, err := util.ParseNADInfo(nad)
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway := NewUserDefinedNetworkGateway(netInfo, 3, nil, nil, nil, nil)
			g.Expect(err).NotTo(HaveOccurred())
			rules, err := udnGateway.getUDNVRFIPRules(test.vrftableID)
			g.Expect(err).To(gomega.BeNil())
			for i, rule := range rules {
				g.Expect(rule.Priority).To(gomega.Equal(test.expectedRules[i].priority))
				g.Expect(rule.Table).To(gomega.Equal(test.expectedRules[i].table))
				g.Expect(rule.Family).To(gomega.Equal(test.expectedRules[i].family))
				g.Expect(*rule.Dst).To(gomega.Equal(test.expectedRules[i].dst))
			}
		})
	}
}

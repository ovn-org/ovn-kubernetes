package node

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	udnfakeclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	factoryMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory/mocks"
	kubemocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	coreinformermocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	v1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func getCreationFakeCommands(fexec *ovntest.FakeExec, mgtPort, mgtPortMAC, netName, nodeName string, mtu int) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15" +
			" -- --may-exist add-port br-int " + mgtPort +
			" -- set interface " + mgtPort +
			fmt.Sprintf(" mac=\"%s\"", mgtPortMAC) +
			" type=internal mtu_request=" + fmt.Sprintf("%d", mtu) +
			" external-ids:iface-id=" + types.K8sPrefix + netName + "_" + nodeName,
	})

	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "sysctl -w net.ipv4.conf." + mgtPort + ".forwarding=1",
		Output: "net.ipv4.conf." + mgtPort + ".forwarding = 1",
	})
}

func getVRFCreationFakeOVSCommands(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 port-to-br eth0",
		Output: "breth0",
	})
}

func getRPFilterLooseModeFakeCommands(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp3.rp_filter=2",
		Output: "net.ipv4.conf.ovn-k8s-mp3.rp_filter = 2",
	})
}

func getDeletionFakeOVSCommands(fexec *ovntest.FakeExec, mgtPort string) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 -- --if-exists del-port br-int " + mgtPort,
	})
}

func setUpGatewayFakeOVSCommands(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 port-to-br eth0",
		Output: "breth0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 port-to-br breth0",
		Output: "breth0",
	})
	// getIntfName
	// GetNicName
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 list-ports breth0",
		Output: "breth0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get Port breth0 Interfaces",
		Output: "breth0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get Interface breth0 Type",
		Output: "system",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 mac_in_use",
		Output: "00:00:00:55:66:99",
	})
	if config.IPv4Mode {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "sysctl -w net.ipv4.conf.breth0.forwarding=1",
			Output: "net.ipv4.conf.breth0.forwarding = 1",
		})
	}
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
		Output: "",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-bridge-mappings=" + types.PhysicalNetworkName + ":breth0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
		Output: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-appctl --timeout=15 dpif/show-dp-features breth0",
		Output: "Check pkt length action: Yes",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . other_config:hw-offload",
		Output: "false",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get Interface patch-breth0_worker1-to-br-int ofport",
		Output: "5",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get interface breth0 ofport",
		Output: "7",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get Open_vSwitch . external_ids:ovn-encap-ip",
		Output: "192.168.1.10",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip route replace table 7 172.16.1.0/24 via 100.128.0.1 dev ovn-k8s-mp0",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip -4 rule",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip -4 rule add fwmark 0x1745ec lookup 7 prio 30",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip -6 rule",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ip -6 rule add fwmark 0x1745ec lookup 7 prio 30",
		Output: "0",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.rp_filter=2",
		Output: "net.ipv4.conf.ovn-k8s-mp0.rp_filter = 2",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 ofport",
		Output: "7",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -",
	})
}

func setUpUDNOpenflowManagerFakeOVSCommands(fexec *ovntest.FakeExec) {
	// UDN patch port
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 get Interface patch-breth0_bluenet_worker1-to-br-int ofport",
		Output: "15",
	})
}

func setUpUDNOpenflowManagerCheckPortsFakeOVSCommands(fexec *ovntest.FakeExec) {
	// Default and UDN patch port
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Interface patch-breth0_bluenet_worker1-to-br-int ofport",
		Output: "15",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Interface patch-breth0_worker1-to-br-int ofport",
		Output: "5",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 ofport",
		Output: "7",
	})

	// After simulated deletion.
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Interface patch-breth0_bluenet_worker1-to-br-int ofport",
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Interface patch-breth0_worker1-to-br-int ofport",
		Output: "5",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 ofport",
		Output: "7",
	})
}

func openflowManagerCheckPorts(ofMgr *openflowManager) {
	netConfigs, uplink, ofPortPhys := ofMgr.getDefaultBridgePortConfigurations()
	sort.SliceStable(netConfigs, func(i, j int) bool {
		return netConfigs[i].patchPort < netConfigs[j].patchPort
	})
	checkPorts(netConfigs, uplink, ofPortPhys)
}

func checkDefaultSvcIsolationOVSFlows(flows []string, defaultConfig *bridgeUDNConfiguration, ofPortHost, bridgeMAC string, svcCIDR *net.IPNet) {
	By(fmt.Sprintf("Checking default service isolation flows for %s", svcCIDR.String()))

	var masqIP string
	var masqSubnet string
	var protoPrefix string
	if utilnet.IsIPv4CIDR(svcCIDR) {
		protoPrefix = "ip"
		masqIP = config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()
		masqSubnet = config.Gateway.V4MasqueradeSubnet
	} else {
		protoPrefix = "ip6"
		masqIP = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String()
		masqSubnet = config.Gateway.V6MasqueradeSubnet
	}

	var nTable0DefaultFlows int
	var nTable0UDNMasqFlows int
	var nTable2Flows int
	for _, flow := range flows {
		if strings.Contains(flow, fmt.Sprintf("priority=500, in_port=%s, %s, %s_dst=%s, actions=ct(commit,zone=%d,nat(src=%s),table=2)",
			ofPortHost, protoPrefix, protoPrefix, svcCIDR, config.Default.HostMasqConntrackZone,
			masqIP)) {
			nTable0DefaultFlows++
		} else if strings.Contains(flow, fmt.Sprintf("priority=550, in_port=%s, %s, %s_src=%s, %s_dst=%s, actions=ct(commit,zone=%d,table=2)",
			ofPortHost, protoPrefix, protoPrefix, masqSubnet, protoPrefix, svcCIDR, config.Default.HostMasqConntrackZone)) {
			nTable0UDNMasqFlows++
		} else if strings.Contains(flow, fmt.Sprintf("priority=100, table=2, actions=set_field:%s->eth_dst,output:%s",
			bridgeMAC, defaultConfig.ofPortPatch)) {
			nTable2Flows++
		}
	}

	Expect(nTable0DefaultFlows).To(Equal(1))
	Expect(nTable0UDNMasqFlows).To(Equal(1))
	Expect(nTable2Flows).To(Equal(1))
}

func checkUDNSvcIsolationOVSFlows(flows []string, netConfig *bridgeUDNConfiguration, netName, bridgeMAC string, svcCIDR *net.IPNet, expectedNFlows int) {
	By(fmt.Sprintf("Checking UDN %s service isolation flows for %s; expected %d flows",
		netName, svcCIDR.String(), expectedNFlows))

	var mgmtMasqIP string
	var protoPrefix string
	if utilnet.IsIPv4CIDR(svcCIDR) {
		mgmtMasqIP = netConfig.v4MasqIPs.ManagementPort.IP.String()
		protoPrefix = "ip"
	} else {
		mgmtMasqIP = netConfig.v6MasqIPs.ManagementPort.IP.String()
		protoPrefix = "ip6"
	}

	var nFlows int
	for _, flow := range flows {
		if strings.Contains(flow, fmt.Sprintf("priority=200, table=2, %s, %s_src=%s, actions=set_field:%s->eth_dst,output:%s",
			protoPrefix, protoPrefix, mgmtMasqIP, bridgeMAC, netConfig.ofPortPatch)) {
			nFlows++
		}
	}

	Expect(nFlows).To(Equal(expectedNFlows))
}

var _ = Describe("UserDefinedNetworkGateway", func() {
	var (
		netName               = "bluenet"
		netID                 = "3"
		nodeName       string = "worker1"
		mgtPortMAC     string = "00:00:00:55:66:77" // dummy MAC used for fake commands
		fexec          *ovntest.FakeExec
		testNS         ns.NetNS
		factoryMock    factoryMocks.NodeWatchFactory
		kubeMock       kubemocks.Interface
		nodeLister     v1mocks.NodeLister
		vrf            *vrfmanager.Controller
		rm             *routemanager.Controller
		ipRulesManager *iprulemanager.Controller
		wg             sync.WaitGroup
		stopCh         chan struct{}
		v4NodeSubnet   = "100.128.0.0/24"
		v6NodeSubnet   = "ae70::/64"
		mgtPort        = fmt.Sprintf("%s%s", types.K8sMgmtIntfNamePrefix, netID)
		v4NodeIP       = "192.168.1.10/24"
		v6NodeIP       = "fc00:f853:ccd:e793::3/64"
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		err := config.PrepareTestConfig()
		Expect(err).NotTo(HaveOccurred())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		// Use a larger masq subnet to allow OF manager to allocate IPs for UDNs.
		config.Gateway.V6MasqueradeSubnet = "fd69::/112"
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/17"
		// Set up a fake vsctl command mock interface
		fexec = ovntest.NewFakeExec()
		Expect(util.SetExec(fexec)).To(Succeed())
		// Set up a fake k8sMgmt interface
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			// given the netdevice is created using add-port command in OVS
			// we need to mock create a dummy link for things to work in unit tests
			ovntest.AddLink(mgtPort)
			link := ovntest.AddLink("breth0")
			addr, _ := netlink.ParseAddr(v4NodeIP)
			err = netlink.AddrAdd(link, addr)
			if err != nil {
				return err
			}
			addr, _ = netlink.ParseAddr(v6NodeIP)
			return netlink.AddrAdd(link, addr)
		})
		Expect(err).NotTo(HaveOccurred())
		factoryMock = factoryMocks.NodeWatchFactory{}
		nodeInformer := coreinformermocks.NodeInformer{}
		factoryMock.On("NodeCoreInformer").Return(&nodeInformer)
		nodeLister = v1mocks.NodeLister{}
		nodeInformer.On("Lister").Return(&nodeLister)
		kubeMock = kubemocks.Interface{}
		wg = sync.WaitGroup{}
		stopCh = make(chan struct{})
		rm = routemanager.NewController()
		vrf = vrfmanager.NewController(rm)
		wg.Add(1)
		go testNS.Do(func(netNS ns.NetNS) error {
			defer wg.Done()
			defer GinkgoRecover()
			err = vrf.Run(stopCh, &wg)
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
		ipRulesManager = iprulemanager.NewController(true, true)
		wg.Add(1)
		go testNS.Do(func(netNS ns.NetNS) error {
			defer wg.Done()
			ipRulesManager.Run(stopCh, 4*time.Minute)
			return nil
		})
	})
	AfterEach(func() {
		close(stopCh)
		wg.Wait()
		Expect(testNS.Close()).To(Succeed())
		Expect(testutils.UnmountNS(testNS)).To(Succeed())
	})
	ovntest.OnSupportedPlatformsIt("should create management port for a L3 user defined network", func() {
		config.IPv6Mode = true
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
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, factoryMock.NodeCoreInformer().Lister(),
			&kubeMock, vrf, nil, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNode", "worker1").Return(node, nil)

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			mpLink, err := udnGateway.addUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			Expect(mpLink).NotTo(BeNil())
			Expect(udnGateway.addUDNManagementPortIPs(mpLink)).Should(Succeed())
			exists, err := util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("100.128.0.2/24"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			exists, err = util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("ae70::2/64"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
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
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		// must be defined so that the primary user defined network can match the ip families of the underlying cluster
		config.IPv4Mode = true
		config.IPv6Mode = true
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, factoryMock.NodeCoreInformer().Lister(),
			&kubeMock, vrf, nil, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNode", "worker1").Return(node, nil)
		cnode := node.DeepCopy()
		kubeMock.On("UpdateNodeStatus", cnode).Return(nil) // check if network key gets deleted from annotation
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			Expect(udnGateway.deleteUDNManagementPort()).To(Succeed())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should create management port for a L2 user defined network", func() {
		config.IPv6Mode = true
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
			types.Layer2Topology, "100.128.0.0/16,ae70::/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, factoryMock.NodeCoreInformer().Lister(),
			&kubeMock, vrf, nil, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNode", "worker1").Return(node, nil)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			mpLink, err := udnGateway.addUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			Expect(mpLink).NotTo(BeNil())
			Expect(udnGateway.addUDNManagementPortIPs(mpLink)).Should(Succeed())
			exists, err := util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("100.128.0.2/16"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			exists, err = util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("ae70::2/60"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should delete management port for a L2 user defined network", func() {
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
			types.Layer2Topology, "100.128.0.0/16,ae70::/60", types.NetworkRolePrimary)
		// must be defined so that the primary user defined network can match the ip families of the underlying cluster
		config.IPv4Mode = true
		config.IPv6Mode = true
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, factoryMock.NodeCoreInformer().Lister(),
			&kubeMock, vrf, nil, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNode", "worker1").Return(node, nil)
		cnode := node.DeepCopy()
		kubeMock.On("UpdateNodeStatus", cnode).Return(nil) // check if network key gets deleted from annotation
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			Expect(udnGateway.deleteUDNManagementPort()).To(Succeed())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should create and delete correct openflows on breth0 for a L3 user defined network", func() {
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.Interface = "eth0"
		config.Gateway.NodeportEnable = true
		ifAddrs := ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":[\"%s\"],\"%s\":[\"%s\", \"%s\"]}", v4NodeSubnet, netName, v4NodeSubnet, v6NodeSubnet),
					"k8s.ovn.org/host-cidrs":   fmt.Sprintf("[\"%s\", \"%s\"]", v4NodeIP, v6NodeIP),
				},
			},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: strings.Split(v4NodeIP, "/")[0]},
				{Type: corev1.NodeInternalIP, Address: strings.Split(v6NodeIP, "/")[0]}},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		setUpGatewayFakeOVSCommands(fexec)
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		getVRFCreationFakeOVSCommands(fexec)
		getRPFilterLooseModeFakeCommands(fexec)
		setUpUDNOpenflowManagerFakeOVSCommands(fexec)
		setUpUDNOpenflowManagerCheckPortsFakeOVSCommands(fexec)
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{*node},
			},
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient:               kubeFakeClient,
			NetworkAttchDefClient:    nadfake.NewSimpleClientset(),
			UserDefinedNetworkClient: udnfakeclient.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			close(stop)
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		_, _ = util.SetFakeIPTablesHelpers()
		_ = nodenft.SetFakeNFTablesHelper()

		// Make a fake MgmtPortConfig with only the fields we care about
		fakeMgmtPortV4IPFamilyConfig := managementPortIPFamilyConfig{
			ifAddr: ovntest.MustParseIPNet(v4NodeSubnet),
			gwIP:   net.IP(v4NodeSubnet),
		}
		fakeMgmtPortV6IPFamilyConfig := managementPortIPFamilyConfig{
			ifAddr: ovntest.MustParseIPNet(v6NodeSubnet),
			gwIP:   net.IP(v6NodeSubnet),
		}

		fakeMgmtPortConfig := managementPortConfig{
			ifName:    nodeName,
			link:      nil,
			routerMAC: nil,
			ipv4:      &fakeMgmtPortV4IPFamilyConfig,
			ipv6:      &fakeMgmtPortV6IPFamilyConfig,
		}
		err = setupManagementPortNFTables(&fakeMgmtPortConfig)
		Expect(err).NotTo(HaveOccurred())

		nodeAnnotatorMock := &kubemocks.Annotator{}
		nodeAnnotatorMock.On("Delete", mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]*util.L3GatewayConfig{
			types.DefaultNetworkName: {
				ChassisID:   "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				BridgeID:    "breth0",
				InterfaceID: "breth0_worker1",
				MACAddress:  ovntest.MustParseMAC("00:00:00:55:66:99"),
				IPAddresses: ifAddrs,
				VLANID:      ptr.To(uint(0)),
			}}).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/node-masquerade-subnet": "{\"ipv4\":\"169.254.0.0/17\",\"ipv6\":\"fd69::/112\"}",
		}).Return(nil)

		wg.Add(1)
		go testNS.Do(func(netNS ns.NetNS) error {
			defer GinkgoRecover()
			rm.Run(stop, 10*time.Second)
			wg.Done()
			return nil
		})
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())

			// make preparations for creating openflow manager in DNCC which can be used for SNCC
			localGw, err := newGateway(
				nodeName,
				ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet),
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotatorMock,
				&fakeMgmtPortConfig,
				&kubeMock,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeLocal,
			)
			Expect(err).NotTo(HaveOccurred())
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			err = localGw.initFunc()
			Expect(err).NotTo(HaveOccurred())
			Expect(localGw.Init(stop, wg)).To(Succeed())
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, wf.NodeCoreInformer().Lister(),
				&kubeMock, vrf, ipRulesManager, localGw)
			Expect(err).NotTo(HaveOccurred())
			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			localGw.openflowManager.syncFlows()
			flowMap := udnGateway.gateway.openflowManager.flowCache

			Expect(len(flowMap["DEFAULT"])).To(Equal(46))
			Expect(udnGateway.masqCTMark).To(Equal(udnGateway.masqCTMark))
			var udnFlows int
			for _, flows := range flowMap {
				for _, flow := range flows {
					mark := fmt.Sprintf("0x%x", udnGateway.masqCTMark)
					if strings.Contains(flow, mark) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))
			Expect(len(udnGateway.openflowManager.defaultBridge.netConfig)).To(Equal(1)) // only default network

			Expect(udnGateway.AddNetwork()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.flowCache
			Expect(len(flowMap["DEFAULT"])).To(Equal(64))                                // 18 UDN Flows are added by default
			Expect(len(udnGateway.openflowManager.defaultBridge.netConfig)).To(Equal(2)) // default network + UDN network
			defaultUdnConfig := udnGateway.openflowManager.defaultBridge.netConfig["default"]
			bridgeUdnConfig := udnGateway.openflowManager.defaultBridge.netConfig["bluenet"]
			bridgeMAC := udnGateway.openflowManager.defaultBridge.macAddress.String()
			ofPortHost := udnGateway.openflowManager.defaultBridge.ofPortHost
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					} else if strings.Contains(flow, fmt.Sprintf("in_port=%s", bridgeUdnConfig.ofPortPatch)) {
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(14))
			openflowManagerCheckPorts(udnGateway.openflowManager)

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				checkDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect exactly one flow per UDN for table 2 for service isolation.
				checkUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", bridgeMAC, svcCIDR, 1)
			}

			// The second call to checkPorts() will return no ofPort for the UDN - simulating a deletion that already was
			// processed by ovn-northd/ovn-controller.  We should not be panicking on that.
			// See setUpUDNOpenflowManagerCheckPortsFakeOVSCommands() for the order of ofPort query results.
			openflowManagerCheckPorts(udnGateway.openflowManager)

			cnode := node.DeepCopy()
			kubeMock.On("UpdateNodeStatus", cnode).Return(nil) // check if network key gets deleted from annotation
			Expect(udnGateway.DelNetwork()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.flowCache
			Expect(len(flowMap["DEFAULT"])).To(Equal(46))                                // only default network flows are present
			Expect(len(udnGateway.openflowManager.defaultBridge.netConfig)).To(Equal(1)) // default network only
			udnFlows = 0
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				checkDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect no more flows per UDN for table 2 for service isolation.
				checkUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", bridgeMAC, svcCIDR, 0)
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should create and delete correct openflows on breth0 for a L2 user defined network", func() {
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Gateway.Interface = "eth0"
		config.Gateway.NodeportEnable = true
		ifAddrs := ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":[\"%s\"],\"%s\":[\"%s\", \"%s\"]}", v4NodeSubnet, netName, v4NodeSubnet, v6NodeSubnet),
					"k8s.ovn.org/host-cidrs":   fmt.Sprintf("[\"%s\", \"%s\"]", v4NodeIP, v6NodeIP),
				},
			},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: strings.Split(v4NodeIP, "/")[0]},
				{Type: corev1.NodeInternalIP, Address: strings.Split(v6NodeIP, "/")[0]}},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::/64", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		setUpGatewayFakeOVSCommands(fexec)
		getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		getVRFCreationFakeOVSCommands(fexec)
		getRPFilterLooseModeFakeCommands(fexec)
		setUpUDNOpenflowManagerFakeOVSCommands(fexec)
		setUpUDNOpenflowManagerCheckPortsFakeOVSCommands(fexec)
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{*node},
			},
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient:               kubeFakeClient,
			NetworkAttchDefClient:    nadfake.NewSimpleClientset(),
			UserDefinedNetworkClient: udnfakeclient.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		wg := &sync.WaitGroup{}
		defer func() {
			close(stop)
			wf.Shutdown()
			wg.Wait()
		}()
		err = wf.Start()

		_, _ = util.SetFakeIPTablesHelpers()
		_ = nodenft.SetFakeNFTablesHelper()

		Expect(err).NotTo(HaveOccurred())

		// Make a fake MgmtPortConfig with only the fields we care about
		fakeMgmtPortV4IPFamilyConfig := managementPortIPFamilyConfig{
			ifAddr: ovntest.MustParseIPNet(v4NodeSubnet),
			gwIP:   net.IP(v4NodeSubnet),
		}
		fakeMgmtPortV6IPFamilyConfig := managementPortIPFamilyConfig{
			ifAddr: ovntest.MustParseIPNet(v6NodeSubnet),
			gwIP:   net.IP(v6NodeSubnet),
		}

		fakeMgmtPortConfig := managementPortConfig{
			ifName:    nodeName,
			link:      nil,
			routerMAC: nil,
			ipv4:      &fakeMgmtPortV4IPFamilyConfig,
			ipv6:      &fakeMgmtPortV6IPFamilyConfig,
		}
		err = setupManagementPortNFTables(&fakeMgmtPortConfig)
		Expect(err).NotTo(HaveOccurred())

		nodeAnnotatorMock := &kubemocks.Annotator{}
		nodeAnnotatorMock.On("Delete", mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]*util.L3GatewayConfig{
			types.DefaultNetworkName: {
				ChassisID:   "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				BridgeID:    "breth0",
				InterfaceID: "breth0_worker1",
				MACAddress:  ovntest.MustParseMAC("00:00:00:55:66:99"),
				IPAddresses: ifAddrs,
				VLANID:      ptr.To(uint(0)),
			}}).Return(nil)
		nodeAnnotatorMock.On("Set", mock.Anything, mock.Anything).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/node-masquerade-subnet": "{\"ipv4\":\"169.254.0.0/17\",\"ipv6\":\"fd69::/112\"}",
		}).Return(nil)

		wg.Add(1)
		go testNS.Do(func(netNS ns.NetNS) error {
			defer GinkgoRecover()
			rm.Run(stop, 10*time.Second)
			wg.Done()
			return nil
		})
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())
			// make preparations for creating openflow manager in DNCC which can be used for SNCC
			localGw, err := newGateway(
				nodeName,
				ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet),
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotatorMock,
				&fakeMgmtPortConfig,
				&kubeMock,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeLocal,
			)
			Expect(err).NotTo(HaveOccurred())
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			Expect(localGw.initFunc()).To(Succeed())
			Expect(localGw.Init(stop, wg)).To(Succeed())
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, wf.NodeCoreInformer().Lister(),
				&kubeMock, vrf, ipRulesManager, localGw)
			Expect(err).NotTo(HaveOccurred())
			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			localGw.openflowManager.syncFlows()
			flowMap := udnGateway.gateway.openflowManager.flowCache
			Expect(len(flowMap["DEFAULT"])).To(Equal(46))
			Expect(udnGateway.masqCTMark).To(Equal(udnGateway.masqCTMark))
			var udnFlows int
			for _, flows := range flowMap {
				for _, flow := range flows {
					mark := fmt.Sprintf("0x%x", udnGateway.masqCTMark)
					if strings.Contains(flow, mark) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))
			Expect(len(udnGateway.openflowManager.defaultBridge.netConfig)).To(Equal(1)) // only default network

			Expect(udnGateway.AddNetwork()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.flowCache
			Expect(len(flowMap["DEFAULT"])).To(Equal(64))                                // 18 UDN Flows are added by default
			Expect(len(udnGateway.openflowManager.defaultBridge.netConfig)).To(Equal(2)) // default network + UDN network
			defaultUdnConfig := udnGateway.openflowManager.defaultBridge.netConfig["default"]
			bridgeUdnConfig := udnGateway.openflowManager.defaultBridge.netConfig["bluenet"]
			bridgeMAC := udnGateway.openflowManager.defaultBridge.macAddress.String()
			ofPortHost := udnGateway.openflowManager.defaultBridge.ofPortHost
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					} else if strings.Contains(flow, fmt.Sprintf("in_port=%s", bridgeUdnConfig.ofPortPatch)) {
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(14))
			openflowManagerCheckPorts(udnGateway.openflowManager)

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				checkDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect exactly one flow per UDN for tables 0 and 2 for service isolation.
				checkUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", bridgeMAC, svcCIDR, 1)
			}

			// The second call to checkPorts() will return no ofPort for the UDN - simulating a deletion that already was
			// processed by ovn-northd/ovn-controller.  We should not be panicking on that.
			// See setUpUDNOpenflowManagerCheckPortsFakeOVSCommands() for the order of ofPort query results.
			openflowManagerCheckPorts(udnGateway.openflowManager)

			cnode := node.DeepCopy()
			kubeMock.On("UpdateNodeStatus", cnode).Return(nil) // check if network key gets deleted from annotation
			Expect(udnGateway.DelNetwork()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.flowCache
			Expect(len(flowMap["DEFAULT"])).To(Equal(46))                                // only default network flows are present
			Expect(len(udnGateway.openflowManager.defaultBridge.netConfig)).To(Equal(1)) // default network only
			udnFlows = 0
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(0))

			for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
				// Check flows for default network service CIDR.
				checkDefaultSvcIsolationOVSFlows(flowMap["DEFAULT"], defaultUdnConfig, ofPortHost, bridgeMAC, svcCIDR)

				// Expect no more flows per UDN for tables 0 and 2 for service isolation.
				checkUDNSvcIsolationOVSFlows(flowMap["DEFAULT"], bridgeUdnConfig, "bluenet", bridgeMAC, svcCIDR, 0)
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should compute correct masquerade reply traffic routes for a user defined network", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, nil, nil, vrf, nil, &gateway{})
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
			Expect(len(routes)).To(Equal(7))
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

			// IPv4 ETP=Local service masquerade IP route
			Expect(*routes[3].Dst).To(Equal(*ovntest.MustParseIPNet("169.254.169.3/32"))) // ETP=Local svc masq IP
			Expect(routes[3].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[3].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv4 cluster subnet route
			Expect(*routes[4].Dst).To(Equal(*ovntest.MustParseIPNet("100.128.0.0/16"))) // cluster subnet route
			Expect(routes[4].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[4].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv6 ETP=Local service masquerade IP route
			Expect(*routes[5].Dst).To(Equal(*ovntest.MustParseIPNet("fd69::3/128"))) // ETP=Local svc masq IP
			Expect(routes[5].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[5].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())

			// IPv6 cluster subnet route
			Expect(*routes[6].Dst).To(Equal(*ovntest.MustParseIPNet("ae70::/60"))) // cluster subnet route
			Expect(routes[6].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[6].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should compute correct service routes for a user defined network", func() {
		config.Gateway.Interface = "eth0"
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("10.96.0.0/16", "fd00:10:96::/112")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, nil, nil, vrf, nil, &gateway{})
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
			Expect(len(routes)).To(Equal(8))
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

			// IPv4 ETP=Local service masquerade IP route
			Expect(*routes[4].Dst).To(Equal(*ovntest.MustParseIPNet("169.254.169.3/32"))) // ETP=Local svc masq IP
			Expect(routes[4].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[4].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv4 cluster subnet route
			Expect(*routes[5].Dst).To(Equal(*ovntest.MustParseIPNet("100.128.0.0/16"))) // cluster subnet route
			Expect(routes[5].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[5].Gw.Equal(ovntest.MustParseIP("100.128.0.1"))).To(BeTrue())

			// IPv6 ETP=Local service masquerade IP route
			Expect(*routes[6].Dst).To(Equal(*ovntest.MustParseIPNet("fd69::3/128"))) // ETP=Local svc masq IP
			Expect(routes[6].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[6].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())

			// IPv6 cluster subnet route
			Expect(*routes[7].Dst).To(Equal(*ovntest.MustParseIPNet("ae70::/60"))) // cluster subnet route
			Expect(routes[7].LinkIndex).To(Equal(mplink.Attrs().Index))
			Expect(routes[7].Gw.Equal(ovntest.MustParseIP("ae70::1"))).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should set rp filer to loose mode for management port interface", func() {
		getRPFilterLooseModeFakeCommands(fexec)
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

func TestConstructUDNVRFIPRules(t *testing.T) {
	type testRule struct {
		priority int
		family   int
		table    int
		mark     int
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
					mark:     0x1003,
				},
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
					mark:     0x1003,
				},
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
					mark:     0x1003,
				},
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
					mark:     0x1003,
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
			g := NewWithT(t)
			config.IPv4Mode = test.v4mode
			config.IPv6Mode = test.v6mode
			cidr := ""
			if config.IPv4Mode {
				cidr = "100.128.0.0/16/24"

			}
			if config.IPv4Mode && config.IPv6Mode {
				cidr += ",ae70::/60/64"
			} else if config.IPv6Mode {
				cidr = "ae70::/60/64"
			}
			nad := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
				types.Layer3Topology, cidr, types.NetworkRolePrimary)
			netInfo, err := util.ParseNADInfo(nad)
			g.Expect(err).NotTo(HaveOccurred())
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, nil, nil, nil, nil, nil, &gateway{})
			g.Expect(err).NotTo(HaveOccurred())
			rules, err := udnGateway.constructUDNVRFIPRules(test.vrftableID)
			g.Expect(err).To(BeNil())
			for i, rule := range rules {
				g.Expect(rule.Priority).To(Equal(test.expectedRules[i].priority))
				g.Expect(rule.Table).To(Equal(test.expectedRules[i].table))
				g.Expect(rule.Family).To(Equal(test.expectedRules[i].family))
				if rule.Dst != nil {
					g.Expect(*rule.Dst).To(Equal(test.expectedRules[i].dst))
				} else {
					g.Expect(rule.Mark).To(Equal(test.expectedRules[i].mark))
				}
			}
		})
	}
}

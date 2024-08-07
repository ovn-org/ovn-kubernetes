package node

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	factoryMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory/mocks"
	kubemocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	coreinformermocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	v1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func getCreationFakeOVSCommands(fexec *ovntest.FakeExec, mgtPort, mgtPortMAC, netName, nodeName string, mtu int) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15" +
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
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface patch-breth0_worker1-to-br-int ofport",
		Output: "5",
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

var _ = Describe("UserDefinedNetworkGateway", func() {
	var (
		netName             = "bluenet"
		netID               = "3"
		nodeName     string = "worker1"
		mgtPortMAC   string = "00:00:00:55:66:77" // dummy MAC used for fake commands
		fexec        *ovntest.FakeExec
		testNS       ns.NetNS
		factoryMock  factoryMocks.NodeWatchFactory
		kubeMock     kubemocks.Interface
		nodeLister   v1mocks.NodeLister
		vrf          *vrfmanager.Controller
		wg           sync.WaitGroup
		stopCh       chan struct{}
		v4NodeSubnet = "100.128.0.0/24"
		v6NodeSubnet = "ae70::66/112"
		mgtPort      = fmt.Sprintf("%s%s", types.K8sMgmtIntfNamePrefix, netID)
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		// Set up a fake vsctl command mock interface
		fexec = ovntest.NewFakeExec()
		Expect(util.SetExec(fexec)).To(Succeed())
		// Set up a fake k8sMgmt interface
		var err error
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			// given the netdevice is created using add-port command in OVS
			// we need to mock create a dummy link for things to work in unit tests
			ovntest.AddLink(mgtPort)
			ovntest.AddLink("breth0")
			return nil
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
		vrf = vrfmanager.NewController()
		wg.Add(1)
		go testNS.Do(func(netNS ns.NetNS) error {
			defer wg.Done()
			defer GinkgoRecover()
			err = vrf.Run(stopCh, &wg)
			Expect(err).NotTo(HaveOccurred())
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
			types.Layer3Topology, "100.128.0.0/16/24,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, factoryMock.NodeCoreInformer().Lister(), &kubeMock, vrf, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		getCreationFakeOVSCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNode", "worker1").Return(node, nil)
		cnode := node.DeepCopy()
		cnode.Annotations[util.OvnNodeManagementPortMacAddresses] = `{"bluenet":"00:00:00:55:66:77"}`
		kubeMock.On("UpdateNodeStatus", cnode).Return(nil)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			mpLink, err := udnGateway.addUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			Expect(mpLink).NotTo(BeNil())
			exists, err := util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("100.128.0.2/24"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			exists, err = util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("ae70::2/112"))
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
			types.Layer3Topology, "100.128.0.0/16/24,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, factoryMock.NodeCoreInformer().Lister(), &kubeMock, vrf, &gateway{})
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
					"k8s.ovn.org/network-ids": fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, factoryMock.NodeCoreInformer().Lister(), &kubeMock, vrf, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		getCreationFakeOVSCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNode", "worker1").Return(node, nil)
		cnode := node.DeepCopy()
		cnode.Annotations[util.OvnNodeManagementPortMacAddresses] = `{"bluenet":"00:00:00:55:66:77"}`
		kubeMock.On("UpdateNodeStatus", cnode).Return(nil)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			mpLink, err := udnGateway.addUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			Expect(mpLink).NotTo(BeNil())
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
					"k8s.ovn.org/network-ids": fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, factoryMock.NodeCoreInformer().Lister(), &kubeMock, vrf, &gateway{})
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
		config.IPv6Mode = true
		config.Gateway.Interface = "eth0"
		config.Gateway.NodeportEnable = true
		ifAddrs := ovntest.MustParseIPNets("192.168.1.10/24", "fc00:f853:ccd:e793::3/64")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":[\"%s\"],\"%s\":[\"%s\", \"%s\"]}", v4NodeSubnet, netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		setUpGatewayFakeOVSCommands(fexec)
		getCreationFakeOVSCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		setUpUDNOpenflowManagerFakeOVSCommands(fexec)
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{*node},
			},
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient: kubeFakeClient,
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
		Expect(err).NotTo(HaveOccurred())
		cnode := node.DeepCopy()
		cnode.Annotations[util.OvnNodeManagementPortMacAddresses] = `{"bluenet":"00:00:00:55:66:77"}`
		kubeMock.On("UpdateNodeStatus", cnode).Return(nil)
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
			"k8s.ovn.org/node-masquerade-subnet": "{\"ipv4\":\"169.254.169.0/29\",\"ipv6\":\"fd69::/125\"}",
		}).Return(nil)

		rm := routemanager.NewController()
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
			localGw, err := newLocalGateway(nodeName, ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet), gatewayNextHops,
				gatewayIntf, "", ifAddrs, nodeAnnotatorMock, &fakeMgmtPortConfig, &kubeMock, wf, rm)
			Expect(err).NotTo(HaveOccurred())
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			Expect(localGw.Init(stop, wg)).To(Succeed())
			udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, wf.NodeCoreInformer().Lister(), &kubeMock, vrf, localGw)
			Expect(err).NotTo(HaveOccurred())
			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			localGw.openflowManager.syncFlows()
			flowMap := udnGateway.gateway.openflowManager.flowCache
			Expect(len(flowMap["DEFAULT"])).To(Equal(45))
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
			Expect(len(flowMap["DEFAULT"])).To(Equal(59))                                // 14 UDN Flows are added by default
			Expect(len(udnGateway.openflowManager.defaultBridge.netConfig)).To(Equal(2)) // default network + UDN network
			for _, flows := range flowMap {
				for _, flow := range flows {
					if strings.Contains(flow, fmt.Sprintf("0x%x", udnGateway.masqCTMark)) {
						// UDN Flow
						udnFlows++
					} else if strings.Contains(flow, fmt.Sprintf("in_port=%s", udnGateway.openflowManager.defaultBridge.netConfig["bluenet"].ofPortPatch)) {
						udnFlows++
					}
				}
			}
			Expect(udnFlows).To(Equal(14))

			cnode := node.DeepCopy()
			kubeMock.On("UpdateNodeStatus", cnode).Return(nil) // check if network key gets deleted from annotation
			Expect(udnGateway.DelNetwork()).To(Succeed())
			flowMap = udnGateway.gateway.openflowManager.flowCache
			Expect(len(flowMap["DEFAULT"])).To(Equal(45))                                // only default network flows are present
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
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
})

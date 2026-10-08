// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
	uplinkinformerfactory "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/informers/externalversions"
	udnfakeclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	factoryMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory/mocks"
	kubemocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	coreinformermocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	v1mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("UserDefinedNodeNetworkController", func() {
	var (
		networkID = "3"
		nad       = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		fexec      *ovntest.FakeExec
		mgtPortMAC string = "00:00:00:55:66:77" // dummy MAC used for fake commands
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		// Use a larger masq subnet to allow OF manager to allocate IPs for UDNs.
		config.Gateway.V6MasqueradeSubnet = "fd69::/112"
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/17"
		// Set up a fake vsctl command mock interface
		fexec = ovntest.NewFakeExec()
		// Setup mock filesystem for ovs-vswitchd.pid file needed by ovs-appctl commands
		Expect(util.SetupMockOVSPidFile()).To(Succeed())
		Expect(util.SetExec(fexec)).To(Succeed())
		ovntest.AnnotateNADWithNetworkID(networkID, nad)
		ovntest.AddLink("breth0")
	})
	AfterEach(func() {
		util.ResetRunner()
		ovntest.DelLink("breth0")
	})

	ovntest.OnSupportedPlatformsIt("ensure UDNGateway is not invoked when feature gate is OFF", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = false
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		factoryMock := factoryMocks.NodeWatchFactory{}
		nodeList := []*corev1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
		}
		cnnci := CommonNodeNetworkControllerInfo{name: "worker1", watchFactory: &factoryMock}
		factoryMock.On("GetNode", "worker1").Return(nodeList[0], nil)
		factoryMock.On("GetNodes").Return(nodeList, nil)
		NetInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		controller, err := NewUserDefinedNodeNetworkController(&cnnci, NetInfo, nil, nil, nil, nil, &gateway{}, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		err = controller.Start(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(controller.gateway).To(BeNil())
	})
	ovntest.OnSupportedPlatformsIt("ensure UDNGateway is invoked for Primary UDNs when feature gate is ON", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		factoryMock := factoryMocks.NodeWatchFactory{}
		nodeList := []*corev1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
		}
		cnnci := CommonNodeNetworkControllerInfo{name: "worker1", watchFactory: &factoryMock}
		factoryMock.On("GetNode", "worker1").Return(nodeList[0], nil)
		factoryMock.On("GetNodes").Return(nodeList, nil)
		nodeInformer := coreinformermocks.NodeInformer{}
		factoryMock.On("NodeCoreInformer").Return(&nodeInformer)
		nodeLister := v1mocks.NodeLister{}
		nodeInformer.On("Lister").Return(&nodeLister)
		uplinkFactory := uplinkinformerfactory.NewSharedInformerFactory(
			uplinkfake.NewSimpleClientset(),
			time.Second,
		)
		factoryMock.On("UplinkStateInformer").Return(uplinkFactory.K8s().V1alpha1().UplinkStates())
		NetInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		getCreationFakeCommands(fexec, "ovn-k8s-mp3", mgtPortMAC, NetInfo.GetNetworkName(), "worker1", NetInfo.MTU())
		ofm := getDummyOpenflowManager()
		controller, err := NewUserDefinedNodeNetworkController(
			&cnnci,
			NetInfo,
			nil,
			nil,
			nil,
			nil,
			&gateway{openflowManager: ofm},
			nil,
			nil,
		)
		Expect(err).NotTo(HaveOccurred())
		err = controller.Start(context.Background())
		Expect(err).To(HaveOccurred()) // we don't have the gateway pieces setup so its expected to fail here
		Expect(err.Error()).To(ContainSubstring("could not create management port"), err.Error())
		Expect(controller.gateway).To(Not(BeNil()))
	})
	ovntest.OnSupportedPlatformsIt("ensure UDNGateway is not invoked for Primary UDNs when feature gate is ON but network is not Primary", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		factoryMock := factoryMocks.NodeWatchFactory{}
		nodeList := []*corev1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
		}
		cnnci := CommonNodeNetworkControllerInfo{name: "worker1", watchFactory: &factoryMock}
		factoryMock.On("GetNode", "worker1").Return(nodeList[0], nil)
		factoryMock.On("GetNodes").Return(nodeList, nil)
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		NetInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		controller, err := NewUserDefinedNodeNetworkController(&cnnci, NetInfo, nil, nil, nil, nil, &gateway{}, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		err = controller.Start(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(controller.gateway).To(BeNil())
	})
})

var _ = Describe("UserDefinedNodeNetworkController: UserDefinedPrimaryNetwork Gateway functionality", func() {
	var (
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		netName                 = "bluenet"
		netID                   = 3
		nodeName         string = "worker1"
		mgtPortMAC       string = "00:00:00:55:66:77"
		fexec            *ovntest.FakeExec
		testNS           ns.NetNS
		vrf              *vrfmanager.Controller
		routeManager     *routemanager.Controller
		ipRulesManager   *iprulemanager.Controller
		v4NodeSubnet     = "100.128.0.0/24"
		v6NodeSubnet     = "ae70::/112"
		mgtPort          = fmt.Sprintf("%s%d", types.K8sMgmtIntfNamePrefix, netID)
		gatewayInterface = "eth0"
		gatewayBridge    = "breth0"
		stopCh           chan struct{}
		wg               *sync.WaitGroup
		kubeMock         kubemocks.Interface
		v4NodeIP         = "192.168.1.10/24"
		v6NodeIP         = "fc00:f853:ccd:e793::3/64"
		ovsClient        libovsdbclient.Client
		ovsCleanup       *libovsdbtest.Context
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		// Skip the encap-update path inside addressManager.sync() — these tests
		// don't fake ovn-appctl and aren't exercising encap reconciliation.
		config.Default.EncapIP = "test-encap-ip"
		var ovsErr error
		ovsClient, ovsCleanup, ovsErr = libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"breth0-uuid"}},
				&vswitchd.Bridge{
					UUID:  "breth0-uuid",
					Name:  "breth0",
					Ports: []string{"breth0-port-uuid", "eth0-port-uuid"},
				},
				// Local port of the bridge (hidden by GetPortBridge / port-to-br).
				&vswitchd.Port{UUID: "breth0-port-uuid", Name: "breth0", Interfaces: []string{"breth0-iface-uuid"}},
				&vswitchd.Interface{UUID: "breth0-iface-uuid", Name: "breth0", Type: "internal", Ofport: ptr.To(65534)},
				// Physical uplink: system-typed so GetNicName resolves eth0.
				&vswitchd.Port{UUID: "eth0-port-uuid", Name: "eth0", Interfaces: []string{"eth0-iface-uuid"}},
				&vswitchd.Interface{UUID: "eth0-iface-uuid", Name: "eth0", Type: "system", Ofport: ptr.To(7)},
			},
		})
		Expect(ovsErr).NotTo(HaveOccurred())
		// Use a larger masq subnet to allow OF manager to allocate IPs for UDNs.
		config.Gateway.V6MasqueradeSubnet = "fd69::/112"
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/17"
		// Set up a fake vsctl command mock interface
		kubeMock = kubemocks.Interface{}
		fexec = ovntest.NewFakeExec()
		// Setup mock filesystem for ovs-vswitchd.pid file needed by ovs-appctl commands
		Expect(util.SetupMockOVSPidFile()).To(Succeed())
		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())
		// Set up a fake k8sMgmt interface
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ovntest.AddLink(gatewayInterface)
			link := ovntest.AddLink(gatewayBridge)
			ovntest.AddLink(mgtPort)
			addr, _ := netlink.ParseAddr("169.254.169.2/29")
			err = netlink.AddrAdd(link, addr)
			if err != nil {
				return err
			}
			addr, _ = netlink.ParseAddr(v4NodeIP)
			err = netlink.AddrAdd(link, addr)
			if err != nil {
				return err
			}
			addr, _ = netlink.ParseAddr(v6NodeIP)
			err = netlink.AddrAdd(link, addr)
			if err != nil {
				return err
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		wg = &sync.WaitGroup{}
		stopCh = make(chan struct{})
		routeManager = routemanager.NewController()
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				routeManager.Run(stopCh, 2*time.Minute)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		ipRulesManager = iprulemanager.NewController(true, true)
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				ipRulesManager.Run(stopCh, 4*time.Minute)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		vrf = vrfmanager.NewController(routeManager)
		wg2 := &sync.WaitGroup{}
		defer func() {
			wg2.Wait()
		}()
		wg2.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg2.Done()
			err := testNS.Do(func(ns.NetNS) error {
				return vrf.Run(stopCh, wg)
			})
			Expect(err).NotTo(HaveOccurred())
		}()
	})
	AfterEach(func() {
		close(stopCh)
		wg.Wait()
		ovsCleanup.Cleanup()
		Expect(testNS.Close()).To(Succeed())
		Expect(testutils.UnmountNS(testNS)).To(Succeed())
		util.ResetRunner()
	})

	ovntest.OnSupportedPlatformsIt("ensure UDNGateway and VRFManager and IPRulesManager are invoked for Primary UDNs when feature gate is ON", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.Gateway.NextHop = "192.168.1.13"
		config.Gateway.Interface = gatewayInterface
		config.Gateway.V6MasqueradeSubnet = "fd69::/112"
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/17"
		config.IPv6Mode = true
		config.IPv4Mode = true
		config.Gateway.NodeportEnable = true
		config.Kubernetes.ServiceCIDRs = ovntest.MustParseIPNets("172.16.1.0/24", "fd02::/112")
		ifAddrs := ovntest.MustParseIPNets(v4NodeIP, v6NodeIP)

		By("creating necessary mocks")
		factoryMock := factoryMocks.NodeWatchFactory{}
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":       fmt.Sprintf("{\"%s\": \"%d\"}", netName, netID),
					"k8s.ovn.org/node-subnets":      fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
					"k8s.ovn.org/host-cidrs":        fmt.Sprintf("[\"%s\", \"%s\"]", v4NodeIP, v6NodeIP),
					"k8s.ovn.org/l3-gateway-config": "{\"default\": {}}",
				},
			},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: strings.Split(v4NodeIP, "/")[0]},
				{Type: corev1.NodeInternalIP, Address: strings.Split(v6NodeIP, "/")[0]}},
			},
		}
		nodeList := []*corev1.Node{node}
		factoryMock.On("GetNode", nodeName).Return(nodeList[0], nil)
		factoryMock.On("GetNodes").Return(nodeList, nil)
		nodeInformer := coreinformermocks.NodeInformer{}
		factoryMock.On("NodeCoreInformer").Return(&nodeInformer)
		nodeLister := v1mocks.NodeLister{}
		nodeInformer.On("Lister").Return(&nodeLister)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		uplinkFactory := uplinkinformerfactory.NewSharedInformerFactory(
			uplinkfake.NewSimpleClientset(),
			time.Second,
		)
		factoryMock.On("UplinkStateInformer").Return(
			uplinkFactory.K8s().V1alpha1().UplinkStates())

		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{*node},
			},
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient:               kubeFakeClient,
			NetworkAttchDefClient:    nadfake.NewSimpleClientset(),
			UplinkClient:             uplinkfake.NewSimpleClientset(),
			UserDefinedNetworkClient: udnfakeclient.NewSimpleClientset(),
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
			"k8s.ovn.org/node-masquerade-subnet": "{\"ipv4\":\"169.254.0.0/17\",\"ipv6\":\"fd69::/112\"}",
		}).Return(nil)
		kubeMock.On("SetAnnotationsOnNode", node.Name, map[string]interface{}{
			"k8s.ovn.org/host-cidrs":          "[\"192.168.1.10/24\",\"fc00:f853:ccd:e793::3/64\"]",
			"k8s.ovn.org/l3-gateway-config":   "{\"default\":{\"mode\":\"\"}}",
			"k8s.ovn.org/node-primary-ifaddr": "{\"ipv4\":\"192.168.1.10/24\",\"ipv6\":\"fc00:f853:ccd:e793::3/64\"}",
		}).Return(nil)

		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		wg := &sync.WaitGroup{}
		defer func() {
			wf.Shutdown()
			wg.Wait()
		}()
		err = wf.Start()

		Expect(err).NotTo(HaveOccurred())

		By("creating NAD for primary UDN")
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		ovntest.AnnotateNADWithNetworkID(strconv.Itoa(netID), nad)
		NetInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		_, ipNet, err := net.ParseCIDR(v4NodeSubnet)
		Expect(err).NotTo(HaveOccurred())
		mgtPortMAC = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipNet).IP).String()
		// Make Management port
		nodeSubnets := ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet)
		mp, err := managementport.NewManagementPortController(ovsClient, node, nodeSubnets, "", "", routeManager, NetInfo)
		Expect(err).NotTo(HaveOccurred())

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			setManagementPortFakeCommands(fexec, nodeName)
			setUpGatewayFakeOVSCommands(fexec)
			deleteStaleManagementPortFakeCommands(fexec, mgtPort)
			getCreationFakeCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, NetInfo.MTU())
			setUpUDNOpenflowManagerFakeOVSCommands(fexec)
			getDeletionFakeOVSCommands(fexec, mgtPort)

			gatewayNextHops, gatewayIntf, err := getGatewayNextHops(ovsClient)
			Expect(err).NotTo(HaveOccurred())

			// create dummy management interface
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: types.K8sMgmtIntfName,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			err = mp.Start(stopCh)
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
				mp,
				&kubeMock,
				wf,
				routeManager,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeLocal,
				ovsClient,
			)
			Expect(err).NotTo(HaveOccurred())
			stop := make(chan struct{})
			wg := &sync.WaitGroup{}
			err = localGw.initFunc()
			Expect(err).NotTo(HaveOccurred())
			Expect(localGw.Init(stop, wg)).To(Succeed())
			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			err = localGw.openflowManager.updateBridgeFlowCache(localGw.nodeIPManager.ListAddresses())
			Expect(err).NotTo(HaveOccurred())
			localGw.openflowManager.syncFlows()

			By("creating a UDN controller for user-defined primary network")
			cnnci := CommonNodeNetworkControllerInfo{name: nodeName, watchFactory: &factoryMock}
			controller, err := NewUserDefinedNodeNetworkController(
				&cnnci,
				NetInfo,
				nil,
				vrf,
				ipRulesManager,
				nil,
				localGw,
				nil,
				nil,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.gateway).To(Not(BeNil()))
			Expect(controller.gateway.ruleManager).To(Not(BeNil()))
			controller.gateway.kubeInterface = &kubeMock

			By("starting UDN controller for user-defined primary network")
			addOVSPatchPortInterface(ovsClient, "breth0", "patch-breth0_bluenet_worker1-to-br-int", 15)
			err = controller.Start(context.Background())
			Expect(err).NotTo(HaveOccurred())

			By("check management interface and VRF device is created for the network")
			vrfDeviceName := util.GetNetworkVRFName(NetInfo)
			vrfLink, err := util.GetNetLinkOps().LinkByName(vrfDeviceName)
			Expect(err).NotTo(HaveOccurred())
			Expect(vrfLink.Type()).To(Equal("vrf"))
			vrfDev, ok := vrfLink.(*netlink.Vrf)
			Expect(ok).To(BeTrue())
			mplink, err := util.GetNetLinkOps().LinkByName(mgtPort)
			Expect(err).NotTo(HaveOccurred())
			vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
			Expect(vrfDev.Table).To(Equal(uint32(vrfTableId)))

			By("delete VRF device explicitly and ensure VRF Manager reconciles it")
			err = util.GetNetLinkOps().LinkDelete(vrfLink)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := util.GetNetLinkOps().LinkByName(vrfDeviceName)
				return err
			}).WithTimeout(120 * time.Second).Should(Succeed())

			By("check iprules are created for the network")
			rulesFound, err := netlink.RuleList(netlink.FAMILY_ALL)
			Expect(err).NotTo(HaveOccurred())
			var udnRules []netlink.Rule
			for _, rule := range rulesFound {
				if rule.Priority == UDNMasqueradeIPRulePriority {
					udnRules = append(udnRules, rule)
				}
			}
			Expect(udnRules).To(HaveLen(4))
			var v4PacketMarkRule, v6PacketMarkRule, v4MasqIPRule, v6MasqIPRule bool
			for _, rule := range udnRules {
				switch {
				case rule.Family == netlink.FAMILY_V4 && rule.Mark != 0:
					v4PacketMarkRule = true
				case rule.Family == netlink.FAMILY_V6 && rule.Mark != 0:
					v6PacketMarkRule = true
				case rule.Family == netlink.FAMILY_V4 && rule.Dst != nil:
					v4MasqIPRule = true
				case rule.Family == netlink.FAMILY_V6 && rule.Dst != nil:
					v6MasqIPRule = true
				}
			}
			Expect(v4PacketMarkRule).To(BeTrue())
			Expect(v6PacketMarkRule).To(BeTrue())
			Expect(v4MasqIPRule).To(BeTrue())
			Expect(v6MasqIPRule).To(BeTrue())

			By("delete the network and ensure its associated VRF device is also deleted")
			controller.Stop()
			err = controller.Cleanup()
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := util.GetNetLinkOps().LinkByName(vrfDeviceName)
				return err
			}).WithTimeout(120 * time.Second).ShouldNot(Succeed())

			By("check masquerade iprules are deleted for the network")
			rulesFound, err = netlink.RuleList(netlink.FAMILY_ALL)
			Expect(err).NotTo(HaveOccurred())
			udnRules = []netlink.Rule{} // reset
			for _, rule := range rulesFound {
				if rule.Priority == UDNMasqueradeIPRulePriority {
					udnRules = append(udnRules, rule)
				}
			}
			Expect(udnRules).To(BeEmpty())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		expectMgmtPortSysctls(Default, types.K8sMgmtIntfName)
		expectForwardingSysctls(Default, "breth0")
		expectMgmtPortSysctls(Default, mgtPort)
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
})

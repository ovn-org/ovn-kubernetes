package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type secondaryNetInfo struct {
	netName   string
	nadName   string
	subnets   string
	topology  string
	isPrimary bool
}

const (
	dummyMACAddr         = "02:03:04:05:06:07"
	nadName              = "blue-net"
	ns                   = "namespace1"
	secondaryNetworkName = "isolatednet"
)

type testConfiguration struct {
	configToOverride   *config.OVNKubernetesFeatureConfig
	expectationOptions []option
}

var _ = Describe("OVN Multi-Homed pod operations", func() {
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdbtest.TestSetup
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed()) // reset defaults

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true)
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: nodeName,
				},
			},
		}

		config.OVNKubernetesFeature = *minimalFeatureConfig()
		config.Gateway.V4MasqueradeSubnet = dummyMasqueradeSubnet().String()
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	table.DescribeTable(
		"reconciles a new",
		func(netInfo secondaryNetInfo, testConfig testConfiguration) {
			podInfo := dummyTestPod(ns, netInfo)
			if testConfig.configToOverride != nil {
				config.OVNKubernetesFeature = *testConfig.configToOverride
			}
			app.Action = func(ctx *cli.Context) error {
				nad, err := newNetworkAttachmentDefinition(
					ns,
					nadName,
					*netInfo.netconf(),
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(netInfo.setupOVNDependencies(&initialDB)).To(Succeed())

				if netInfo.isPrimary {
					initialDB.NBData = append(
						initialDB.NBData,
						&nbdb.LogicalSwitch{
							Name: fmt.Sprintf("%s_join", netInfo.netName),
						},
						&nbdb.LogicalRouter{
							Name: fmt.Sprintf("%s_ovn_cluster_router", netInfo.netName),
						},
						&nbdb.LogicalRouterPort{
							Name: fmt.Sprintf("rtos-%s_%s", netInfo.netName, nodeName),
						},
					)
				}

				const nodeIPv4CIDR = "192.168.126.202/24"
				testNode, err := newNodeWithSecondaryNets(nodeName, nodeIPv4CIDR, netInfo)
				Expect(err).NotTo(HaveOccurred())
				fakeOvn.startWithDBSetup(
					initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							*newNamespace(ns),
						},
					},
					&v1.NodeList{
						Items: []v1.Node{*testNode},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newMultiHomedPod(podInfo.namespace, podInfo.podName, podInfo.nodeName, podInfo.podIP, netInfo),
						},
					},
					&nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					},
				)
				podInfo.populateLogicalSwitchCache(fakeOvn)

				// pod exists, networks annotations don't
				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.Background(), podInfo.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				Expect(fakeOvn.controller.WatchNamespaces()).NotTo(HaveOccurred())
				Expect(fakeOvn.controller.WatchPods()).NotTo(HaveOccurred())
				secondaryNetController, ok := fakeOvn.secondaryControllers[secondaryNetworkName]
				Expect(ok).To(BeTrue())

				secondaryNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
				podInfo.populateSecondaryNetworkLogicalSwitchCache(fakeOvn, secondaryNetController)
				Expect(secondaryNetController.bnc.WatchNodes()).To(Succeed())
				Expect(secondaryNetController.bnc.WatchPods()).To(Succeed())

				// check that after start networks annotations and nbdb will be updated
				Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, podInfo.namespace, podInfo.podName)
				}).WithTimeout(2 * time.Second).Should(MatchJSON(podInfo.getAnnotationsJson()))

				defaultNetExpectations := getDefaultNetExpectedPodsAndSwitches([]testPod{podInfo}, []string{nodeName})
				expectationOptions := testConfig.expectationOptions
				if netInfo.isPrimary {
					defaultNetExpectations = emptyDefaultClusterNetworkNodeSwitch(podInfo.nodeName)
					gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
					Expect(err).NotTo(HaveOccurred())
					Expect(gwConfig.NextHops).NotTo(BeEmpty())
					expectationOptions = append(expectationOptions, withGatewayConfig(gwConfig))
				}
				Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(
						append(
							defaultNetExpectations,
							newSecondaryNetworkExpectationMachine(
								fakeOvn,
								[]testPod{podInfo},
								expectationOptions...,
							).expectedLogicalSwitchesAndPorts()...)))

				return nil
			}

			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		table.Entry("pod on a user defined secondary network",
			dummySecondaryUserDefinedNetwork("192.168.0.0/16"),
			nonICClusterTestConfiguration(),
		),
		table.Entry("pod on a user defined primary network",
			dummyPrimaryUserDefinedNetwork("192.168.0.0/16"),
			nonICClusterTestConfiguration(),
		),
		table.Entry("pod on a user defined secondary network on an interconnect cluster",
			dummySecondaryUserDefinedNetwork("192.168.0.0/16"),
			icClusterTestConfiguration(),
		),
		table.Entry("pod on a user defined primary network on an interconnect cluster",
			dummyPrimaryUserDefinedNetwork("192.168.0.0/16"),
			icClusterTestConfiguration(),
		),
	)

})

func newPodWithPrimaryUDN(
	nodeName, nodeSubnet, nodeMgtIP, nodeGWIP, podName, podIPs, podMAC, namespace string,
	primaryUDNConfig secondaryNetInfo,
) testPod {
	pod := newTPod(nodeName, nodeSubnet, nodeMgtIP, "", podName, podIPs, podMAC, namespace)
	if primaryUDNConfig.isPrimary {
		pod.networkRole = "infrastructure-locked"
		pod.routes = append(
			pod.routes,
			util.PodRoute{
				Dest:    testing.MustParseIPNet("10.128.0.0/14"),
				NextHop: testing.MustParseIP("10.128.1.1"),
			},
			util.PodRoute{
				Dest:    testing.MustParseIPNet("100.64.0.0/16"),
				NextHop: testing.MustParseIP("10.128.1.1"),
			},
		)
	}
	pod.addNetwork(
		primaryUDNConfig.netName,
		primaryUDNConfig.nadName,
		primaryUDNConfig.subnets,
		"",
		nodeGWIP,
		"192.168.0.3/16",
		"0a:58:c0:a8:00:03",
		"primary",
		0,
		[]util.PodRoute{
			{
				Dest:    testing.MustParseIPNet("192.168.0.0/16"),
				NextHop: testing.MustParseIP("192.168.0.1"),
			},
			{
				Dest:    testing.MustParseIPNet("172.16.1.0/24"),
				NextHop: testing.MustParseIP("192.168.0.1"),
			},
			{
				Dest:    testing.MustParseIPNet("100.65.0.0/16"),
				NextHop: testing.MustParseIP("192.168.0.1"),
			},
		},
	)
	return pod
}

func newMultiHomedPod(namespace, name, node, podIP string, multiHomingConfigs ...secondaryNetInfo) *v1.Pod {
	pod := newPod(namespace, name, node, podIP)
	var secondaryNetworks []nadapi.NetworkSelectionElement
	for _, multiHomingConf := range multiHomingConfigs {
		if multiHomingConf.isPrimary {
			continue // these will be automatically plugged in
		}
		nadNamePair := strings.Split(multiHomingConf.nadName, "/")
		ns := pod.Namespace
		attachmentName := multiHomingConf.nadName
		if len(nadNamePair) > 1 {
			ns = nadNamePair[0]
			attachmentName = nadNamePair[1]
		}
		nse := nadapi.NetworkSelectionElement{
			Name:      attachmentName,
			Namespace: ns,
		}
		secondaryNetworks = append(secondaryNetworks, nse)
	}
	serializedNetworkSelectionElements, _ := json.Marshal(secondaryNetworks)
	pod.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: string(serializedNetworkSelectionElements)}
	return pod
}

func namespacedName(ns, name string) string { return fmt.Sprintf("%s/%s", ns, name) }

func (sni *secondaryNetInfo) setupOVNDependencies(dbData *libovsdbtest.TestSetup) error {
	netInfo, err := util.NewNetInfo(sni.netconf())
	if err != nil {
		return err
	}

	switch sni.topology {
	case ovntypes.Layer2Topology:
		dbData.NBData = append(dbData.NBData, &nbdb.LogicalSwitch{
			Name:        netInfo.GetNetworkScopedName(ovntypes.OVNLayer2Switch),
			UUID:        netInfo.GetNetworkScopedName(ovntypes.OVNLayer2Switch) + "_UUID",
			ExternalIDs: map[string]string{ovntypes.NetworkExternalID: sni.netName},
		})
	case ovntypes.Layer3Topology:
		dbData.NBData = append(dbData.NBData, &nbdb.LogicalSwitch{
			Name:        netInfo.GetNetworkScopedName(nodeName),
			UUID:        netInfo.GetNetworkScopedName(nodeName) + "_UUID",
			ExternalIDs: map[string]string{ovntypes.NetworkExternalID: sni.netName},
		})
	case ovntypes.LocalnetTopology:
		dbData.NBData = append(dbData.NBData, &nbdb.LogicalSwitch{
			Name:        netInfo.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch),
			UUID:        netInfo.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch) + "_UUID",
			ExternalIDs: map[string]string{ovntypes.NetworkExternalID: sni.netName},
		})
	default:
		return fmt.Errorf("missing topology in the network configuration: %v", sni)
	}
	return nil
}

func (sni *secondaryNetInfo) netconf() *ovncnitypes.NetConf {
	const plugin = "ovn-k8s-cni-overlay"

	role := ovntypes.NetworkRoleSecondary
	if sni.isPrimary {
		role = ovntypes.NetworkRolePrimary
	}
	return &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: sni.netName,
			Type: plugin,
		},
		Topology: sni.topology,
		NADName:  sni.nadName,
		Subnets:  sni.subnets,
		Role:     role,
	}
}

func dummyTestPod(nsName string, info secondaryNetInfo) testPod {
	const nodeSubnet = "10.128.1.0/24"
	if info.isPrimary {
		return newPodWithPrimaryUDN(
			nodeName,
			nodeSubnet,
			"10.128.1.2",
			"192.168.0.1",
			"myPod",
			"10.128.1.3",
			"0a:58:0a:80:01:03",
			nsName,
			info,
		)
	}
	pod := newTPod(nodeName, nodeSubnet, "10.128.1.2", "10.128.1.1", podName, "10.128.1.3", "0a:58:0a:80:01:03", nsName)
	pod.addNetwork(
		info.netName,
		info.nadName,
		info.subnets,
		"",
		"",
		"192.168.0.3/16",
		"0a:58:c0:a8:00:03",
		"secondary",
		0,
		[]util.PodRoute{
			{
				Dest:    testing.MustParseIPNet("192.168.0.0/16"),
				NextHop: testing.MustParseIP("192.168.0.1"),
			},
		},
	)
	return pod
}

func dummySecondaryUserDefinedNetwork(subnets string) secondaryNetInfo {
	return secondaryNetInfo{
		netName:  secondaryNetworkName,
		nadName:  namespacedName(ns, nadName),
		topology: ovntypes.Layer3Topology,
		subnets:  subnets,
	}
}

func dummyPrimaryUserDefinedNetwork(subnets string) secondaryNetInfo {
	secondaryNet := dummySecondaryUserDefinedNetwork(subnets)
	secondaryNet.isPrimary = true
	return secondaryNet
}

func (sni *secondaryNetInfo) String() string {
	return fmt.Sprintf("%q: %q", sni.netName, sni.subnets)
}

func newNodeWithSecondaryNets(nodeName string, nodeIPv4CIDR string, netInfos ...secondaryNetInfo) (*v1.Node, error) {
	var nodeSubnetInfo []string
	for _, info := range netInfos {
		nodeSubnetInfo = append(nodeSubnetInfo, info.String())
	}

	nodeIP, nodeCIDR, err := net.ParseCIDR(nodeIPv4CIDR)
	if err != nil {
		return nil, err
	}
	nextHopIP := util.GetNodeGatewayIfAddr(nodeCIDR).IP
	nodeCIDR.IP = nodeIP

	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"k8s.ovn.org/node-primary-ifaddr":      fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4CIDR, ""),
				"k8s.ovn.org/node-subnets":             fmt.Sprintf("{\"default\":\"%s\", %s}", v4Node1Subnet, strings.Join(nodeSubnetInfo, ",")),
				util.OVNNodeHostCIDRs:                  fmt.Sprintf("[\"%s\"]", nodeIPv4CIDR),
				"k8s.ovn.org/zone-name":                "global",
				"k8s.ovn.org/l3-gateway-config":        fmt.Sprintf("{\"default\":{\"mode\":\"shared\",\"bridge-id\":\"breth0\",\"interface-id\":\"breth0_ovn-worker\",\"mac-address\":%q,\"ip-addresses\":[%[2]q],\"ip-address\":%[2]q,\"next-hops\":[%[3]q],\"next-hop\":%[3]q,\"node-port-enable\":\"true\",\"vlan-id\":\"0\"}}", util.IPAddrToHWAddr(nodeIP), nodeCIDR, nextHopIP),
				util.OvnNodeChassisID:                  "abdcef",
				"k8s.ovn.org/network-ids":              "{\"default\":\"0\",\"isolatednet\":\"2\"}",
				util.OvnNodeManagementPortMacAddresses: fmt.Sprintf("{\"isolatednet\":%q}", dummyMACAddr),
				util.OVNNodeGRLRPAddrs:                 fmt.Sprintf("{\"isolatednet\":{\"ipv4\":%q}}", gwRouterIPAddress()),
			},
			Labels: map[string]string{
				"k8s.ovn.org/egress-assignable": "",
			},
		},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{
				{
					Type:   v1.NodeReady,
					Status: v1.ConditionTrue,
				},
			},
		},
	}, nil
}

func dummyJoinIPs() []*net.IPNet {
	return []*net.IPNet{dummyJoinIP()}
}

func dummyJoinIP() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("169.254.169.13"),
		Mask: net.CIDRMask(24, 32),
	}
}
func dummyMasqueradeSubnet() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("169.254.169.0"),
		Mask: net.CIDRMask(24, 32),
	}
}

func emptyDefaultClusterNetworkNodeSwitch(nodeName string) []libovsdbtest.TestData {
	switchUUID := nodeName + "-UUID"
	return []libovsdbtest.TestData{&nbdb.LogicalSwitch{UUID: switchUUID, Name: nodeName}}
}

func expectedGWEntities(nodeName string, netInfo util.NetInfo, gwConfig util.L3GatewayConfig) []libovsdbtest.TestData {
	gwRouterName := fmt.Sprintf("GR_%s_%s", netInfo.GetNetworkName(), nodeName)

	expectedEntities := append(
		expectedGWRouterPlusNATAndStaticRoutes(nodeName, gwRouterName, netInfo, gwConfig),
		expectedGRToJoinSwitchLRP(gwRouterName, gwRouterIPAddress(), netInfo),
		expectedGRToExternalSwitchLRP(gwRouterName, netInfo, nodePhysicalIPAddress(), udnGWSNATAddress()),
		expectedGatewayChassis(nodeName, netInfo, gwConfig),
		expectedStaticMACBinding(gwRouterName, nextHopMasqueradeIP()),
	)
	for _, entity := range expectedExternalSwitchAndLSPs(netInfo, gwConfig, nodeName) {
		expectedEntities = append(expectedEntities, entity)
	}
	for _, entity := range expectedJoinSwitchAndLSPs(netInfo, nodeName) {
		expectedEntities = append(expectedEntities, entity)
	}
	return expectedEntities
}

func expectedGWRouterPlusNATAndStaticRoutes(
	nodeName, gwRouterName string,
	netInfo util.NetInfo,
	gwConfig util.L3GatewayConfig,
) []libovsdbtest.TestData {
	gwRouterToExtLRPUUID := fmt.Sprintf("%s%s-UUID", ovntypes.GWRouterToExtSwitchPrefix, gwRouterName)
	gwRouterToJoinLRPUUID := fmt.Sprintf("%s%s-UUID", ovntypes.GWRouterToJoinSwitchPrefix, gwRouterName)

	var hostIPs []string
	for _, ip := range append(gwConfig.IPAddresses, dummyJoinIP()) {
		hostIPs = append(hostIPs, ip.IP.String())
	}

	var hostPhysicalIP string
	if len(gwConfig.IPAddresses) > 0 {
		hostPhysicalIP = gwConfig.IPAddresses[0].IP.String()
	}

	const (
		nat1             = "abc-UUID"
		nat2             = "cba-UUID"
		staticRoute1     = "srA-UUID"
		staticRoute2     = "srB-UUID"
		staticRoute3     = "srC-UUID"
		ipv4DefaultRoute = "0.0.0.0/0"
	)

	staticRouteOutputPort := ovntypes.GWRouterToExtSwitchPrefix + netInfo.GetNetworkScopedGWRouterName(nodeName)
	nextHopIP := gwConfig.NextHops[0].String()
	ipv4Subnet := networkSubnet(netInfo)
	nextHopMasqIP := nextHopMasqueradeIP().String()
	masqSubnet := config.Gateway.V4MasqueradeSubnet
	return []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			Name: gwRouterName,
			UUID: gwRouterName + "-UUID",
			ExternalIDs: map[string]string{
				ovntypes.NetworkExternalID:  netInfo.GetNetworkName(),
				ovntypes.TopologyExternalID: netInfo.TopologyType(),
				"physical_ip":               hostPhysicalIP,
				"physical_ips":              strings.Join(hostIPs, ","),
			},
			Options:      gwRouterOptions(gwConfig),
			Ports:        []string{gwRouterToJoinLRPUUID, gwRouterToExtLRPUUID},
			Nat:          []string{nat1, nat2},
			StaticRoutes: []string{staticRoute1, staticRoute2, staticRoute3},
		},
		newNATEntry(nat1, dummyJoinIP().IP.String(), gwRouterIPAddress().IP.String()),
		newNATEntry(nat2, dummyJoinIP().IP.String(), networkSubnet(netInfo)),
		expectedGRStaticRoute(staticRoute1, ipv4Subnet, dummyJoinIP().IP.String(), nil, nil),
		expectedGRStaticRoute(staticRoute2, ipv4DefaultRoute, nextHopIP, nil, &staticRouteOutputPort),
		expectedGRStaticRoute(staticRoute3, masqSubnet, nextHopMasqIP, nil, &staticRouteOutputPort),
	}
}

func expectedStaticMACBinding(gwRouterName string, ip net.IP) *nbdb.StaticMACBinding {
	lrpName := fmt.Sprintf("%s%s", ovntypes.GWRouterToExtSwitchPrefix, gwRouterName)
	return &nbdb.StaticMACBinding{
		UUID:               lrpName + "static-mac-binding-UUID",
		IP:                 ip.String(),
		LogicalPort:        lrpName,
		MAC:                util.IPAddrToHWAddr(nextHopMasqueradeIP()).String(),
		OverrideDynamicMAC: true,
	}
}

func expectedGatewayChassis(nodeName string, netInfo util.NetInfo, gwConfig util.L3GatewayConfig) *nbdb.GatewayChassis {
	gwChassisName := fmt.Sprintf("%s%s_%s-%s", ovntypes.RouterToSwitchPrefix, netInfo.GetNetworkName(), nodeName, gwConfig.ChassisID)
	return &nbdb.GatewayChassis{UUID: gwChassisName + "-UUID", Name: gwChassisName, Priority: 1, ChassisName: gwConfig.ChassisID}
}

func expectedGRToJoinSwitchLRP(gatewayRouterName string, gwRouterLRPIP *net.IPNet, netInfo util.NetInfo) *nbdb.LogicalRouterPort {
	lrpName := fmt.Sprintf("%s%s", ovntypes.GWRouterToJoinSwitchPrefix, gatewayRouterName)
	options := map[string]string{"gateway_mtu": fmt.Sprintf("%d", 1400)}
	return expectedLogicalRouterPort(lrpName, netInfo, options, gwRouterLRPIP)
}

func expectedGRToExternalSwitchLRP(gatewayRouterName string, netInfo util.NetInfo, joinSwitchIPs ...*net.IPNet) *nbdb.LogicalRouterPort {
	lrpName := fmt.Sprintf("%s%s", ovntypes.GWRouterToExtSwitchPrefix, gatewayRouterName)
	return expectedLogicalRouterPort(lrpName, netInfo, nil, joinSwitchIPs...)
}

func expectedLogicalRouterPort(lrpName string, netInfo util.NetInfo, options map[string]string, routerNetworks ...*net.IPNet) *nbdb.LogicalRouterPort {
	var ips []string
	for _, ip := range routerNetworks {
		ips = append(ips, ip.String())
	}
	var mac string
	if len(routerNetworks) > 0 {
		ipToGenMacFrom := routerNetworks[0]
		mac = util.IPAddrToHWAddr(ipToGenMacFrom.IP).String()
	}
	return &nbdb.LogicalRouterPort{
		UUID:     lrpName + "-UUID",
		Name:     lrpName,
		Networks: ips,
		MAC:      mac,
		Options:  options,
		ExternalIDs: map[string]string{
			"k8s.ovn.org/topology": netInfo.TopologyType(),
			"k8s.ovn.org/network":  netInfo.GetNetworkName(),
		},
	}
}

func expectedLayer3EgressEntities(netInfo util.NetInfo, gwConfig util.L3GatewayConfig) []libovsdbtest.TestData {
	const (
		routerPolicyUUID1 = "lrpol1-UUID"
		routerPolicyUUID2 = "lrpol2-UUID"
		staticRouteUUID1  = "sr1-UUID"
		staticRouteUUID2  = "sr2-UUID"
	)
	joinIPAddr := dummyJoinIP().IP.String()
	clusterRouterName := fmt.Sprintf("%s_ovn_cluster_router", netInfo.GetNetworkName())
	rtosLRPName := fmt.Sprintf("%s%s", ovntypes.RouterToSwitchPrefix, netInfo.GetNetworkScopedName(nodeName))
	rtosLRPUUID := rtosLRPName + "-UUID"
	nodeIP := gwConfig.IPAddresses[0].IP.String()
	networkIPv4Subnet := networkSubnet(netInfo)

	gatewayChassisUUID := fmt.Sprintf("%s-%s-UUID", rtosLRPName, gwConfig.ChassisID)
	expectedEntities := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			Name:         clusterRouterName,
			UUID:         clusterRouterName + "-UUID",
			Ports:        []string{rtosLRPUUID},
			StaticRoutes: []string{staticRouteUUID1, staticRouteUUID2},
			Policies:     []string{routerPolicyUUID1, routerPolicyUUID2},
		},
		&nbdb.LogicalRouterPort{UUID: rtosLRPUUID, Name: rtosLRPName, Networks: []string{"192.168.0.1/16"}, MAC: "0a:58:c0:a8:00:01", GatewayChassis: []string{gatewayChassisUUID}},
		expectedGRStaticRoute(staticRouteUUID1, networkIPv4Subnet, gwRouterIPAddress().IP.String(), &nbdb.LogicalRouterStaticRoutePolicySrcIP, nil),
		expectedGRStaticRoute(staticRouteUUID2, gwRouterIPAddress().IP.String(), gwRouterIPAddress().IP.String(), nil, nil),
		expectedLogicalRouterPolicy(routerPolicyUUID1, netInfo, nodeName, nodeIP, managementPortIP().String()),
		expectedLogicalRouterPolicy(routerPolicyUUID2, netInfo, nodeName, joinIPAddr, managementPortIP().String()),
	}
	return expectedEntities
}

func expectedLogicalRouterPolicy(routerPolicyUUID1 string, netInfo util.NetInfo, nodeName, destIP, nextHop string) *nbdb.LogicalRouterPolicy {
	const (
		priority      = 1004
		rerouteAction = "reroute"
	)
	networkScopedNodeName := netInfo.GetNetworkScopedName(nodeName)
	lrpName := fmt.Sprintf("%s%s", ovntypes.RouterToSwitchPrefix, networkScopedNodeName)
	return &nbdb.LogicalRouterPolicy{
		UUID:        routerPolicyUUID1,
		Action:      rerouteAction,
		ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
		Match:       fmt.Sprintf("inport == %q && ip4.dst == %s /* %s */", lrpName, destIP, networkScopedNodeName),
		Nexthops:    []string{nextHop},
		Priority:    priority,
	}
}

func expectedGRStaticRoute(uuid, ipPrefix, nextHop string, policy *nbdb.LogicalRouterStaticRoutePolicy, outputPort *string) *nbdb.LogicalRouterStaticRoute {
	return &nbdb.LogicalRouterStaticRoute{
		UUID:       uuid,
		IPPrefix:   ipPrefix,
		OutputPort: outputPort,
		Nexthop:    nextHop,
		Policy:     policy,
		ExternalIDs: map[string]string{
			"k8s.ovn.org/network":  "isolatednet",
			"k8s.ovn.org/topology": "layer3",
		},
	}
}

func allowAllFromMgmtPort(aclUUID string, mgmtPortIP string) *nbdb.ACL {
	meterName := "acl-logging"
	return &nbdb.ACL{
		UUID:      aclUUID,
		Action:    "allow-related",
		Direction: "to-lport",
		ExternalIDs: map[string]string{
			"k8s.ovn.org/name":             "isolatednet_test-node",
			"ip":                           mgmtPortIP,
			"k8s.ovn.org/id":               fmt.Sprintf("isolatednet-network-controller:NetpolNode:isolatednet_test-node:%s", mgmtPortIP),
			"k8s.ovn.org/owner-controller": "isolatednet-network-controller",
			"k8s.ovn.org/owner-type":       "NetpolNode",
		},
		Match:    fmt.Sprintf("ip4.src==%s", mgmtPortIP),
		Meter:    &meterName,
		Priority: 1001,
		Tier:     2,
	}
}

func nodePhysicalIPAddress() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("192.168.126.202"),
		Mask: net.CIDRMask(24, 32),
	}
}

func udnGWSNATAddress() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("169.254.169.13"),
		Mask: net.CIDRMask(24, 32),
	}
}

func newNATEntry(uuid string, externalIP string, logicalIP string) *nbdb.NAT {
	return &nbdb.NAT{
		UUID:       uuid,
		ExternalIP: externalIP,
		LogicalIP:  logicalIP,
		Type:       "snat",
		Options:    map[string]string{"stateless": "false"},
	}
}

func expectedExternalSwitchAndLSPs(netInfo util.NetInfo, gwConfig util.L3GatewayConfig, nodeName string) []libovsdbtest.TestData {
	const (
		port1UUID = "port1-UUID"
		port2UUID = "port2-UUID"
	)
	gwRouterName := netInfo.GetNetworkScopedGWRouterName(nodeName)
	return []libovsdbtest.TestData{
		&nbdb.LogicalSwitch{
			UUID:        "ext-UUID",
			Name:        netInfo.GetNetworkScopedExtSwitchName(nodeName),
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
			Ports:       []string{port1UUID, port2UUID},
		},
		&nbdb.LogicalSwitchPort{
			UUID:        port1UUID,
			Name:        netInfo.GetNetworkScopedExtPortName(gwConfig.BridgeID, nodeName),
			Addresses:   []string{"unknown"},
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
			Options:     map[string]string{"network_name": "physnet"},
			Type:        ovntypes.LocalnetTopology,
		},
		&nbdb.LogicalSwitchPort{
			UUID:        port2UUID,
			Name:        ovntypes.EXTSwitchToGWRouterPrefix + gwRouterName,
			Addresses:   []string{gwConfig.MACAddress.String()},
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
			Options:     externalSwitchRouterPortOptions(gwRouterName),
			Type:        "router",
		},
	}
}

func externalSwitchRouterPortOptions(gatewayRouterName string) map[string]string {
	return map[string]string{
		"nat-addresses":             "router",
		"exclude-lb-vips-from-garp": "true",
		"router-port":               ovntypes.GWRouterToExtSwitchPrefix + gatewayRouterName,
	}
}

func expectedJoinSwitchAndLSPs(netInfo util.NetInfo, nodeName string) []libovsdbtest.TestData {
	const joinToGRLSPUUID = "port3-UUID"
	gwRouterName := netInfo.GetNetworkScopedGWRouterName(nodeName)
	expectedData := []libovsdbtest.TestData{
		&nbdb.LogicalSwitch{
			UUID:  "join-UUID",
			Name:  netInfo.GetNetworkScopedJoinSwitchName(),
			Ports: []string{joinToGRLSPUUID},
		},
		&nbdb.LogicalSwitchPort{
			UUID:        joinToGRLSPUUID,
			Name:        ovntypes.JoinSwitchToGWRouterPrefix + gwRouterName,
			Addresses:   []string{"router"},
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
			Options:     map[string]string{"router-port": ovntypes.GWRouterToJoinSwitchPrefix + gwRouterName},
			Type:        "router",
		},
	}
	return expectedData
}

func nextHopMasqueradeIP() net.IP {
	return net.ParseIP("169.254.169.4")
}

func gwRouterIPAddress() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("100.65.0.4"),
		Mask: net.CIDRMask(16, 32),
	}
}

func managementPortIP() net.IP {
	return net.ParseIP("192.168.0.2")
}

func networkSubnet(netInfo util.NetInfo) string {
	return strings.TrimSuffix(subnetsAsString(netInfo.Subnets())[0], "/24")
}

func gwRouterOptions(gwConfig util.L3GatewayConfig) map[string]string {
	return map[string]string{
		"lb_force_snat_ip":              "router_ip",
		"snat-ct-zone":                  "0",
		"mac_binding_age_threshold":     "300",
		"chassis":                       gwConfig.ChassisID,
		"always_learn_from_arp_request": "false",
		"dynamic_neigh_routers":         "true",
	}
}

func standardNonDefaultNetworkExtIDs(netInfo util.NetInfo) map[string]string {
	return map[string]string{
		"k8s.ovn.org/topology": netInfo.TopologyType(),
		"k8s.ovn.org/network":  netInfo.GetNetworkName(),
	}
}

func minimalFeatureConfig() *config.OVNKubernetesFeatureConfig {
	return &config.OVNKubernetesFeatureConfig{
		EnableNetworkSegmentation: true,
		EnableMultiNetwork:        true,
	}
}

func enableICFeatureConfig() *config.OVNKubernetesFeatureConfig {
	featConfig := minimalFeatureConfig()
	featConfig.EnableInterconnect = true
	return featConfig
}

func icClusterTestConfiguration() testConfiguration {
	return testConfiguration{
		configToOverride:   enableICFeatureConfig(),
		expectationOptions: []option{withInterconnectCluster()},
	}
}

func nonICClusterTestConfiguration() testConfiguration {
	return testConfiguration{}
}

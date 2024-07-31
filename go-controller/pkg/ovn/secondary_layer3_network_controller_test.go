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

var _ = Describe("OVN Multi-Homed pod operations", func() {
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdbtest.TestSetup

		isNetworkSegmentationOriginallyEnabled bool
		isMultiNetworkOriginallyEnabled        bool
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

		isNetworkSegmentationOriginallyEnabled = config.OVNKubernetesFeature.EnableNetworkSegmentation
		isMultiNetworkOriginallyEnabled = config.OVNKubernetesFeature.EnableMultiNetwork
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
	})

	AfterEach(func() {
		fakeOvn.shutdown()
		config.OVNKubernetesFeature.EnableNetworkSegmentation = isNetworkSegmentationOriginallyEnabled
		config.OVNKubernetesFeature.EnableMultiNetwork = isMultiNetworkOriginallyEnabled
	})

	table.DescribeTable(
		"reconciles a new",
		func(netInfo secondaryNetInfo, expectationOptions ...option) {
			podInfo := dummyTestPod(ns, netInfo)
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

				fakeOvn.startWithDBSetup(
					initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							*newNamespace(ns),
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*newNodeWithSecondaryNets(nodeName, "192.168.126.202/24", netInfo),
						},
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

				secondaryNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyIPs()
				podInfo.populateSecondaryNetworkLogicalSwitchCache(fakeOvn, secondaryNetController)
				Expect(secondaryNetController.bnc.WatchNodes()).To(Succeed())
				Expect(secondaryNetController.bnc.WatchPods()).To(Succeed())

				// check that after start networks annotations and nbdb will be updated
				Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, podInfo.namespace, podInfo.podName)
				}).WithTimeout(2 * time.Second).Should(MatchJSON(podInfo.getAnnotationsJson()))

				defaultNetExpectations := getExpectedDataPodsAndSwitches([]testPod{podInfo}, []string{nodeName})
				if netInfo.isPrimary {
					defaultNetExpectations = emptyDefaultClusterNetworkNodeSwitch(podInfo.nodeName)
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
		),

		table.Entry("pod on a user defined primary network",
			dummyPrimaryUserDefinedNetwork("192.168.0.0/16"),
			withPrimaryUDN(),
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

func newNodeWithSecondaryNets(nodeName string, nodeIPv4CIDR string, netInfos ...secondaryNetInfo) *v1.Node {
	var nodeSubnetInfo []string
	for _, info := range netInfos {
		nodeSubnetInfo = append(nodeSubnetInfo, info.String())
	}

	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"k8s.ovn.org/node-primary-ifaddr":    fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4CIDR, ""),
				"k8s.ovn.org/node-subnets":           fmt.Sprintf("{\"default\":\"%s\", %s}", v4Node1Subnet, strings.Join(nodeSubnetInfo, ",")),
				util.OVNNodeHostCIDRs:                fmt.Sprintf("[\"%s\"]", nodeIPv4CIDR),
				"k8s.ovn.org/zone-name":              "global",
				"k8s.ovn.org/l3-gateway-config":      "{\"default\":{\"mode\":\"shared\",\"bridge-id\":\"breth0\",\"interface-id\":\"breth0_ovn-worker\",\"mac-address\":\"ea:2e:6c:f2:f5:d4\",\"ip-addresses\":[\"10.89.0.14/24\"],\"ip-address\":\"10.89.0.14/24\",\"next-hops\":[\"10.89.0.1\"],\"next-hop\":\"10.89.0.1\",\"node-port-enable\":\"true\",\"vlan-id\":\"0\"}}",
				util.OvnNodeChassisID:                "abdcef",
				"k8s.ovn.org/network-ids":            "{\"default\":\"0\",\"isolatednet\":\"2\"}",
				util.OvnNodeManagementPortMacAddress: dummyMACAddr,
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
	}
}

func dummyIPs() []*net.IPNet {
	return []*net.IPNet{
		{
			IP:   net.ParseIP("10.10.10.0"),
			Mask: net.CIDRMask(24, 32),
		},
	}
}

func emptyDefaultClusterNetworkNodeSwitch(nodeName string) []libovsdbtest.TestData {
	switchUUID := nodeName + "-UUID"
	return []libovsdbtest.TestData{&nbdb.LogicalSwitch{UUID: switchUUID, Name: nodeName}}
}

func expectedGWEntities(nodeName string, networkName string) []libovsdbtest.TestData {
	gwRouterName := fmt.Sprintf("GR_%s_%s", networkName, nodeName)
	var staticRouteOutputPort = "rtoe-GR_isolatednet_test-node"
	expectedEntities := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			Name: gwRouterName,
			UUID: gwRouterName + "-UUID",
			ExternalIDs: map[string]string{
				ovntypes.NetworkExternalID:  networkName,
				ovntypes.TopologyExternalID: "layer3",
				"physical_ip":               "10.89.0.14",
				"physical_ips":              "10.89.0.14,169.254.169.13",
			},
			Options: map[string]string{
				"lb_force_snat_ip":              "router_ip",
				"snat-ct-zone":                  "0",
				"mac_binding_age_threshold":     "300",
				"chassis":                       "abdcef",
				"always_learn_from_arp_request": "false",
				"dynamic_neigh_routers":         "true",
			},
			Ports:        []string{"rtoj-GR_isolatednet_test-node-UUID", "rtoe-GR_isolatednet_test-node-UUID"},
			Nat:          []string{"abc-UUID", "cba-UUID"},
			StaticRoutes: []string{"srA-UUID", "srB-UUID", "srC-UUID"},
		},
		&nbdb.LogicalRouterPort{UUID: "rtoj-GR_isolatednet_test-node-UUID", Name: "rtoj-GR_isolatednet_test-node", Networks: []string{"10.10.10.0/24"}, MAC: "0a:58:0a:0a:0a:00", Options: map[string]string{"gateway_mtu": "1400"}, ExternalIDs: map[string]string{"k8s.ovn.org/topology": "layer3", "k8s.ovn.org/network": "isolatednet"}},
		&nbdb.LogicalRouterPort{UUID: "rtoe-GR_isolatednet_test-node-UUID", Name: "rtoe-GR_isolatednet_test-node", Networks: []string{"10.89.0.14/24", "169.254.169.13/24"}, MAC: "ea:2e:6c:f2:f5:d4", ExternalIDs: map[string]string{"k8s.ovn.org/topology": "layer3", "k8s.ovn.org/network": "isolatednet"}},
		&nbdb.NAT{UUID: "abc-UUID", ExternalIP: "169.254.169.13", LogicalIP: "10.10.10.0", Options: map[string]string{"stateless": "false"}, Type: "snat"},
		&nbdb.NAT{UUID: "cba-UUID", ExternalIP: "169.254.169.13", LogicalIP: "192.168.0.0/16", Options: map[string]string{"stateless": "false"}, Type: "snat"},
		expectedGRStaticRoute("srA-UUID", "192.168.0.0/16", "10.10.10.0", nil, nil),
		expectedGRStaticRoute("srB-UUID", "0.0.0.0/0", "10.89.0.1", nil, &staticRouteOutputPort),
		expectedGRStaticRoute("srC-UUID", "169.254.169.0/24", "169.254.169.4", nil, &staticRouteOutputPort),
		&nbdb.GatewayChassis{UUID: "rtos-isolatednet_test-node-abdcef-UUID", Name: "rtos-isolatednet_test-node-abdcef", Priority: 1, ChassisName: "abdcef"},
		&nbdb.LogicalSwitch{UUID: "ext-UUID", Name: "ext_isolatednet_test-node", ExternalIDs: map[string]string{"k8s.ovn.org/topology": "layer3", "k8s.ovn.org/network": "isolatednet"}, Ports: []string{"port1-UUID", "port2-UUID"}},
		&nbdb.LogicalSwitch{UUID: "join-UUID", Name: "isolatednet_join", Ports: []string{"port3-UUID"}},
		&nbdb.LogicalSwitchPort{UUID: "port1-UUID", Name: "breth0_isolatednet_test-node", Addresses: []string{"unknown"}, ExternalIDs: map[string]string{"k8s.ovn.org/topology": "layer3", "k8s.ovn.org/network": "isolatednet"}, Options: map[string]string{"network_name": "isolatednet"}, Type: "localnet"},
		&nbdb.LogicalSwitchPort{UUID: "port2-UUID", Name: "etor-GR_isolatednet_test-node", Addresses: []string{"ea:2e:6c:f2:f5:d4"}, ExternalIDs: map[string]string{"k8s.ovn.org/topology": "layer3", "k8s.ovn.org/network": "isolatednet"}, Options: map[string]string{"nat-addresses": "router", "exclude-lb-vips-from-garp": "true", "router-port": "rtoe-GR_isolatednet_test-node"}, Type: "router"},
		&nbdb.LogicalSwitchPort{UUID: "port3-UUID", Name: "jtor-GR_isolatednet_test-node", Addresses: []string{"router"}, ExternalIDs: map[string]string{"k8s.ovn.org/topology": "layer3", "k8s.ovn.org/network": "isolatednet"}, Options: map[string]string{"router-port": "rtoj-GR_isolatednet_test-node"}, Type: "router"},
		&nbdb.StaticMACBinding{UUID: "mb1-UUID", IP: "169.254.169.4", LogicalPort: "rtoe-GR_isolatednet_test-node", MAC: "0a:58:a9:fe:a9:04", OverrideDynamicMAC: true},
	}
	return expectedEntities
}

func expectedLayer3EgressEntities(networkName string) []libovsdbtest.TestData {
	clusterRouterName := fmt.Sprintf("%s_ovn_cluster_router", networkName)
	expectedEntities := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			Name:         clusterRouterName,
			UUID:         clusterRouterName + "-UUID",
			Ports:        []string{"rtos-isolatednet_test-node-UUID"},
			StaticRoutes: []string{"sr1-UUID", "sr2-UUID"},
			Policies:     []string{"lrpol1-UUID", "lrpol2-UUID"},
		},
		&nbdb.LogicalRouterPort{UUID: "rtos-isolatednet_test-node-UUID", Name: "rtos-isolatednet_test-node", Networks: []string{"192.168.0.1/16"}, MAC: "0a:58:c0:a8:00:01", GatewayChassis: []string{"rtos-isolatednet_test-node-abdcef-UUID"}},
		expectedGRStaticRoute("sr1-UUID", "192.168.0.0/16", "10.10.10.0", &nbdb.LogicalRouterStaticRoutePolicySrcIP, nil),
		expectedGRStaticRoute("sr2-UUID", "10.10.10.0", "10.10.10.0", nil, nil),
		&nbdb.LogicalRouterPolicy{UUID: "lrpol1-UUID", Action: "reroute", ExternalIDs: map[string]string{"k8s.ovn.org/topology": "layer3", "k8s.ovn.org/network": "isolatednet"}, Match: "inport == \"rtos-isolatednet_test-node\" && ip4.dst == 10.89.0.14 /* isolatednet_test-node */", Nexthops: []string{"192.168.0.2"}, Priority: 1004},
		&nbdb.LogicalRouterPolicy{UUID: "lrpol2-UUID", Action: "reroute", ExternalIDs: map[string]string{"k8s.ovn.org/topology": "layer3", "k8s.ovn.org/network": "isolatednet"}, Match: "inport == \"rtos-isolatednet_test-node\" && ip4.dst == 169.254.169.13 /* isolatednet_test-node */", Nexthops: []string{"192.168.0.2"}, Priority: 1004},
	}
	return expectedEntities
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

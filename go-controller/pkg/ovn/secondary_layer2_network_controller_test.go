package ovn

import (
	"context"
	"fmt"
	"net"
	"time"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("OVN Multi-Homed pod operations for layer2 network", func() {
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
			NBData: []libovsdbtest.TestData{},
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
			podInfo := dummyL2TestPod(ns, netInfo)
			if testConfig.configToOverride != nil {
				config.OVNKubernetesFeature = *testConfig.configToOverride
				if testConfig.gatewayConfig != nil {
					config.Gateway.DisableSNATMultipleGWs = testConfig.gatewayConfig.DisableSNATMultipleGWs
				}
			}
			app.Action = func(ctx *cli.Context) error {
				By(fmt.Sprintf("creating a network attachment definition for network: %s", netInfo.netName))
				nad, err := newNetworkAttachmentDefinition(
					ns,
					nadName,
					*netInfo.netconf(),
				)
				Expect(err).NotTo(HaveOccurred())
				By("setting up the OVN DB without any entities in it")
				Expect(netInfo.setupOVNDependencies(&initialDB)).To(Succeed())

				const nodeIPv4CIDR = "192.168.126.202/24"
				By(fmt.Sprintf("Creating a node named %q, with IP: %s", nodeName, nodeIPv4CIDR))
				testNode, err := newNodeWithSecondaryNets(nodeName, nodeIPv4CIDR, netInfo)
				Expect(err).NotTo(HaveOccurred())
				fakeOvn.startWithDBSetup(
					initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							*newNamespace(ns),
						},
					},
					&v1.NodeList{Items: []v1.Node{*testNode}},
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

				// on IC, the test itself spits out the pod with the
				// annotations set, since on production it would be the
				// clustermanager to annotate the pod.
				if !config.OVNKubernetesFeature.EnableInterconnect {
					By("asserting the pod originally does *not* feature the OVN pod networks annotation")
					// pod exists, networks annotations don't
					pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.Background(), podInfo.podName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					_, ok := pod.Annotations[util.OvnPodAnnotationName]
					Expect(ok).To(BeFalse())
				}

				Expect(fakeOvn.controller.WatchNamespaces()).NotTo(HaveOccurred())
				Expect(fakeOvn.controller.WatchPods()).NotTo(HaveOccurred())
				By("asserting the pod (once reconciled) *features* the OVN pod networks annotation")
				secondaryNetController, ok := fakeOvn.secondaryControllers[secondaryNetworkName]
				Expect(ok).To(BeTrue())

				secondaryNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
				podInfo.populateSecondaryNetworkLogicalSwitchCache(fakeOvn, secondaryNetController)
				Expect(secondaryNetController.bnc.WatchNodes()).To(Succeed())
				Expect(secondaryNetController.bnc.WatchPods()).To(Succeed())

				// for layer2 on interconnect, it is the cluster manager that
				// allocates the OVN annotation; on unit tests, this just
				// doesn't happen, and we create the pod with these annotations
				// set. Hence, no point checking they're the expected ones.
				// TODO: align the mocked annotations with the production code
				//   - currently missing setting the routes.
				if !config.OVNKubernetesFeature.EnableInterconnect {
					By("asserting the pod OVN pod networks annotation are the expected ones")
					// check that after start networks annotations and nbdb will be updated
					Eventually(func() string {
						return getPodAnnotations(fakeOvn.fakeClient.KubeClient, podInfo.namespace, podInfo.podName)
					}).WithTimeout(2 * time.Second).Should(MatchJSON(podInfo.getAnnotationsJson()))
				}

				expectationOptions := testConfig.expectationOptions
				if netInfo.isPrimary {
					By("configuring the expectation machine with the GW related configuration")
					gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
					Expect(err).NotTo(HaveOccurred())
					Expect(gwConfig.NextHops).NotTo(BeEmpty())
					expectationOptions = append(expectationOptions, withGatewayConfig(gwConfig))
				}
				By("asserting the OVN entities provisioned in the NBDB are the expected ones")
				Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(
						newSecondaryNetworkExpectationMachine(
							fakeOvn,
							[]testPod{podInfo},
							expectationOptions...,
						).expectedLogicalSwitchesAndPorts(netInfo.isPrimary)...))

				return nil
			}

			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		table.Entry("pod on a user defined secondary network",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
		),

		table.Entry("pod on a user defined primary network on an IC cluster",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
		),

		table.Entry("pod on a user defined secondary network",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
		),

		table.Entry("pod on a user defined primary network on an IC cluster",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
		),
		table.Entry("pod on a user defined primary network on an IC cluster",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterWithDisableSNATTestConfiguration(),
		),
	)

	table.DescribeTable(
		"the gateway is properly cleaned up",
		func(netInfo secondaryNetInfo, testConfig testConfiguration) {
			podInfo := dummyTestPod(ns, netInfo)
			if testConfig.configToOverride != nil {
				config.OVNKubernetesFeature = *testConfig.configToOverride
				if testConfig.gatewayConfig != nil {
					config.Gateway.DisableSNATMultipleGWs = testConfig.gatewayConfig.DisableSNATMultipleGWs
				}
			}
			app.Action = func(ctx *cli.Context) error {
				netConf := netInfo.netconf()
				networkConfig, err := util.NewNetInfo(netConf)
				Expect(err).NotTo(HaveOccurred())

				nad, err := newNetworkAttachmentDefinition(
					ns,
					nadName,
					*netConf,
				)
				Expect(err).NotTo(HaveOccurred())

				const nodeIPv4CIDR = "192.168.126.202/24"
				testNode, err := newNodeWithSecondaryNets(nodeName, nodeIPv4CIDR, netInfo)
				Expect(err).NotTo(HaveOccurred())

				gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
				Expect(err).NotTo(HaveOccurred())
				Expect(gwConfig.NextHops).NotTo(BeEmpty())

				if netInfo.isPrimary {
					gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
					Expect(err).NotTo(HaveOccurred())
					initialDB.NBData = append(
						initialDB.NBData,
						expectedLayer2EgressEntities(networkConfig, *gwConfig, nodeName)...)
				}

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

				Expect(netInfo.setupOVNDependencies(&initialDB)).To(Succeed())

				podInfo.populateLogicalSwitchCache(fakeOvn)

				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.Background(), podInfo.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				// on IC, the test itself spits out the pod with the
				// annotations set, since on production it would be the
				// clustermanager to annotate the pod.
				if !config.OVNKubernetesFeature.EnableInterconnect {
					// pod exists, networks annotations don't
					_, ok := pod.Annotations[util.OvnPodAnnotationName]
					Expect(ok).To(BeFalse())
				}

				Expect(fakeOvn.controller.WatchNamespaces()).To(Succeed())
				Expect(fakeOvn.controller.WatchPods()).To(Succeed())
				secondaryNetController, ok := fakeOvn.secondaryControllers[secondaryNetworkName]
				Expect(ok).To(BeTrue())

				secondaryNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
				podInfo.populateSecondaryNetworkLogicalSwitchCache(fakeOvn, secondaryNetController)
				Expect(secondaryNetController.bnc.WatchNodes()).To(Succeed())
				Expect(secondaryNetController.bnc.WatchPods()).To(Succeed())

				Expect(fakeOvn.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
				Expect(fakeOvn.fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Delete(context.Background(), nad.Name, metav1.DeleteOptions{})).To(Succeed())

				// we must access the layer2 controller to be able to issue its cleanup function (to remove the GW related stuff).
				Expect(
					newSecondaryLayer2NetworkController(
						&secondaryNetController.bnc.CommonNetworkControllerInfo,
						networkConfig,
						nodeName,
					).Cleanup()).To(Succeed())
				Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{}))

				return nil
			}
			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		table.Entry("pod on a user defined primary network",
			dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16"),
			nonICClusterTestConfiguration(),
		),
		table.Entry("pod on a user defined primary network on an interconnect cluster",
			dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16"),
			icClusterTestConfiguration(),
		),
		table.Entry("pod on a user defined primary network on an interconnect cluster",
			dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16"),
			icClusterWithDisableSNATTestConfiguration(),
		),
	)

})

func dummySecondaryLayer2UserDefinedNetwork(subnets string) secondaryNetInfo {
	return secondaryNetInfo{
		netName:  secondaryNetworkName,
		nadName:  namespacedName(ns, nadName),
		topology: ovntypes.Layer2Topology,
		subnets:  subnets,
	}
}

func dummyPrimaryLayer2UserDefinedNetwork(subnets string) secondaryNetInfo {
	secondaryNet := dummySecondaryLayer2UserDefinedNetwork(subnets)
	secondaryNet.isPrimary = true
	return secondaryNet
}

func dummyL2TestPod(nsName string, info secondaryNetInfo) testPod {
	const nodeSubnet = "10.128.1.0/24"
	if info.isPrimary {
		pod := newTPod(nodeName, nodeSubnet, "10.128.1.2", "", "myPod", "10.128.1.3", "0a:58:0a:80:01:03", nsName)
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
		pod.addNetwork(
			info.netName,
			info.nadName,
			info.subnets,
			"",
			"100.200.0.1",
			"100.200.0.3/16",
			"0a:58:64:c8:00:03",
			"primary",
			0,
			[]util.PodRoute{
				{
					Dest:    testing.MustParseIPNet("172.16.1.0/24"),
					NextHop: testing.MustParseIP("100.200.0.1"),
				},
				{
					Dest:    testing.MustParseIPNet("100.65.0.0/16"),
					NextHop: testing.MustParseIP("100.200.0.1"),
				},
			},
		)
		return pod
	}
	pod := newTPod(nodeName, nodeSubnet, "10.128.1.2", "10.128.1.1", podName, "10.128.1.3", "0a:58:0a:80:01:03", nsName)
	pod.addNetwork(
		info.netName,
		info.nadName,
		info.subnets,
		"",
		"",
		"100.200.0.1/16",
		"0a:58:64:c8:00:01",
		"secondary",
		0,
		[]util.PodRoute{},
	)
	return pod
}

func dummyL2TestPodAdditionalNetworkIP() string {
	secNetInfo := dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16")
	return dummyL2TestPod(ns, secNetInfo).getNetworkPortInfo(secNetInfo.netName, secNetInfo.nadName).podIP
}

func expectedLayer2EgressEntities(netInfo util.NetInfo, gwConfig util.L3GatewayConfig, nodeName string) []libovsdbtest.TestData {
	const (
		nat1              = "nat1-UUID"
		nat2              = "nat2-UUID"
		nat3              = "nat3-UUID"
		perPodSNAT        = "pod-snat-UUID"
		sr1               = "sr1-UUID"
		sr2               = "sr2-UUID"
		routerPolicyUUID1 = "lrp1-UUID"
	)
	gwRouterName := fmt.Sprintf("GR_%s_test-node", netInfo.GetNetworkName())
	staticRouteOutputPort := ovntypes.GWRouterToExtSwitchPrefix + gwRouterName
	gwRouterToNetworkSwitchPortName := ovntypes.GWRouterToJoinSwitchPrefix + gwRouterName
	gwRouterToExtSwitchPortName := fmt.Sprintf("%s%s", ovntypes.GWRouterToExtSwitchPrefix, gwRouterName)

	var nat []string
	if config.Gateway.DisableSNATMultipleGWs {
		nat = append(nat, perPodSNAT)
	} else {
		nat = append(nat, nat1, nat2, nat3)
	}
	expectedEntities := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			Name:         gwRouterName,
			UUID:         gwRouterName + "-UUID",
			Nat:          nat,
			Ports:        []string{gwRouterToNetworkSwitchPortName + "-UUID", gwRouterToExtSwitchPortName + "-UUID"},
			StaticRoutes: []string{sr1, sr2},
			ExternalIDs:  gwRouterExternalIDs(netInfo, gwConfig),
			Options:      gwRouterOptions(gwConfig),
			Policies:     []string{routerPolicyUUID1},
		},
		expectedGWToNetworkSwitchRouterPort(gwRouterToNetworkSwitchPortName, netInfo, gwRouterIPAddress(), layer2SubnetGWAddr()),
		expectedGRStaticRoute(sr1, dummyMasqueradeSubnet().String(), nextHopMasqueradeIP().String(), nil, &staticRouteOutputPort, netInfo),
		expectedGRStaticRoute(sr2, ipv4DefaultRoute().String(), nodeGateway().IP.String(), nil, &staticRouteOutputPort, netInfo),
		expectedGRToExternalSwitchLRP(gwRouterName, netInfo, nodePhysicalIPAddress(), udnGWSNATAddress()),
		expectedStaticMACBinding(gwRouterName, nextHopMasqueradeIP()),

		expectedLogicalRouterPolicy(routerPolicyUUID1, netInfo, nodeName, nodeIP().IP.String(), managementPortIP(layer2Subnet()).String()),
	}

	for _, entity := range expectedExternalSwitchAndLSPs(netInfo, gwConfig, nodeName) {
		expectedEntities = append(expectedEntities, entity)
	}
	if config.Gateway.DisableSNATMultipleGWs {
		expectedEntities = append(expectedEntities, newNATEntry(perPodSNAT, dummyJoinIP().IP.String(), dummyL2TestPodAdditionalNetworkIP(), nil))
	} else {
		expectedEntities = append(expectedEntities, newNATEntry(nat1, dummyJoinIP().IP.String(), gwRouterIPAddress().IP.String(), standardNonDefaultNetworkExtIDs(netInfo)))
		expectedEntities = append(expectedEntities, newNATEntry(nat2, dummyJoinIP().IP.String(), layer2Subnet().String(), standardNonDefaultNetworkExtIDs(netInfo)))
		expectedEntities = append(expectedEntities, newNATEntry(nat3, dummyJoinIP().IP.String(), layer2SubnetGWAddr().IP.String(), standardNonDefaultNetworkExtIDs(netInfo)))
	}
	return expectedEntities
}

func expectedGWToNetworkSwitchRouterPort(name string, netInfo util.NetInfo, networks ...*net.IPNet) *nbdb.LogicalRouterPort {
	options := map[string]string{"gateway_mtu": fmt.Sprintf("%d", 1400)}
	return expectedLogicalRouterPort(name, netInfo, options, networks...)
}

func layer2Subnet() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("100.200.0.0"),
		Mask: net.CIDRMask(16, 32),
	}
}

func layer2SubnetGWAddr() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("100.200.0.1"),
		Mask: net.CIDRMask(16, 32),
	}
}

func nodeGateway() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("192.168.126.1"),
		Mask: net.CIDRMask(24, 32),
	}
}

func ipv4DefaultRoute() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("0.0.0.0"),
		Mask: net.CIDRMask(0, 32),
	}
}

func dummyLayer2SecondaryUserDefinedNetwork(subnets string) secondaryNetInfo {
	return secondaryNetInfo{
		netName:  secondaryNetworkName,
		nadName:  namespacedName(ns, nadName),
		topology: ovntypes.Layer2Topology,
		subnets:  subnets,
	}
}

func dummyLayer2PrimaryUserDefinedNetwork(subnets string) secondaryNetInfo {
	secondaryNet := dummyLayer2SecondaryUserDefinedNetwork(subnets)
	secondaryNet.isPrimary = true
	return secondaryNet
}

func newSecondaryLayer2NetworkController(cnci *CommonNetworkControllerInfo, netInfo util.NetInfo, nodeName string) *SecondaryLayer2NetworkController {
	layer2NetworkController := NewSecondaryLayer2NetworkController(cnci, netInfo)
	layer2NetworkController.gatewayManagers.Store(
		nodeName,
		newDummyGatewayManager(cnci.kube, cnci.nbClient, netInfo, cnci.watchFactory, nodeName),
	)
	return layer2NetworkController
}

func nodeIP() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("192.168.126.202"),
		Mask: net.CIDRMask(24, 32),
	}
}

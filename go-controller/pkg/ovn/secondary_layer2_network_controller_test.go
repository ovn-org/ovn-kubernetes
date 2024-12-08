package ovn

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/urfave/cli/v2"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	knet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	networkAttachDefController "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/nad"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type lspEnableValue *bool

var (
	lspEnableNotSpecified    lspEnableValue = nil
	lspEnableExplicitlyTrue  lspEnableValue = ptr.To(true)
	lspEnableExplicitlyFalse lspEnableValue = ptr.To(false)
)

type liveMigrationPodInfo struct {
	podPhase           v1.PodPhase
	annotation         map[string]string
	creationTimestamp  metav1.Time
	expectedLspEnabled lspEnableValue
}

type liveMigrationInfo struct {
	vmName        string
	sourcePodInfo liveMigrationPodInfo
	targetPodInfo liveMigrationPodInfo
}

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

	DescribeTable(
		"reconciles a new",
		func(netInfo secondaryNetInfo, testConfig testConfiguration, gatewayMode config.GatewayMode) {
			const podIdx = 0
			podInfo := dummyL2TestPod(ns, netInfo, podIdx)
			setupConfig(netInfo, testConfig, gatewayMode)
			app.Action = func(ctx *cli.Context) error {
				pod := newMultiHomedPod(podInfo, netInfo)

				const nodeIPv4CIDR = "192.168.126.202/24"
				By(fmt.Sprintf("Creating a node named %q, with IP: %s", nodeName, nodeIPv4CIDR))
				testNode, err := newNodeWithSecondaryNets(nodeName, nodeIPv4CIDR, netInfo)
				Expect(err).NotTo(HaveOccurred())

				Expect(setupFakeOvnForLayer2Topology(fakeOvn, initialDB, netInfo, testNode, podInfo, pod)).To(Succeed())

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
					expectationOptions = append(expectationOptions, withClusterPortGroup())
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
		Entry("pod on a user defined secondary network",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
			config.GatewayModeShared,
		),

		Entry("pod on a user defined primary network",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
			config.GatewayModeShared,
		),

		Entry("pod on a user defined secondary network on an IC cluster",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			config.GatewayModeShared,
		),

		Entry("pod on a user defined primary network on an IC cluster",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			config.GatewayModeShared,
		),

		Entry("pod on a user defined primary network on an IC cluster; LGW",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			config.GatewayModeLocal,
		),

		Entry("pod on a user defined primary network on an IC cluster with per-pod SNATs enabled",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(func(testConfig *testConfiguration) {
				testConfig.gatewayConfig = &config.GatewayConfig{DisableSNATMultipleGWs: true}
			}),
			config.GatewayModeShared,
		),
		/** FIXME: tests do not support ipv6 yet
		Entry("pod on a IPv6 user defined primary network on an IC cluster with per-pod SNATs enabled",
			dummyPrimaryLayer2UserDefinedNetwork("2001:db8:abcd:0012::/64"),
			icClusterWithDisableSNATTestConfiguration(),
			config.GatewayModeShared,
		),
		*/
	)

	DescribeTable(
		"reconciles a new kubevirt-related pod during its live-migration phases",
		func(netInfo secondaryNetInfo, testConfig testConfiguration, migrationInfo *liveMigrationInfo) {
			const (
				sourcePodInfoIdx = 0
				targetPodInfoIdx = 1
			)
			sourcePodInfo := dummyL2TestPod(ns, netInfo, sourcePodInfoIdx)
			setupConfig(netInfo, testConfig, config.GatewayModeShared)
			app.Action = func(ctx *cli.Context) error {
				sourcePod := newMultiHomedKubevirtPod(
					migrationInfo.vmName,
					migrationInfo.sourcePodInfo,
					sourcePodInfo,
					netInfo)

				const nodeIPv4CIDR = "192.168.126.202/24"
				By(fmt.Sprintf("Creating a node named %q, with IP: %s", nodeName, nodeIPv4CIDR))
				testNode, err := newNodeWithSecondaryNets(nodeName, nodeIPv4CIDR, netInfo)
				Expect(err).NotTo(HaveOccurred())

				Expect(setupFakeOvnForLayer2Topology(fakeOvn, initialDB, netInfo, testNode, sourcePodInfo, sourcePod)).To(Succeed())

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
						return getPodAnnotations(fakeOvn.fakeClient.KubeClient, sourcePodInfo.namespace, sourcePodInfo.podName)
					}).WithTimeout(2 * time.Second).Should(MatchJSON(sourcePodInfo.getAnnotationsJson()))
				}

				expectationOptions := testConfig.expectationOptions
				if netInfo.isPrimary {
					By("configuring the expectation machine with the GW related configuration")
					gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
					Expect(err).NotTo(HaveOccurred())
					Expect(gwConfig.NextHops).NotTo(BeEmpty())
					expectationOptions = append(expectationOptions, withGatewayConfig(gwConfig))
					expectationOptions = append(expectationOptions, withClusterPortGroup())
				}
				By("asserting the OVN entities provisioned in the NBDB are the expected ones before migration started")
				Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(
						newSecondaryNetworkExpectationMachine(
							fakeOvn,
							[]testPod{sourcePodInfo},
							expectationOptions...,
						).expectedLogicalSwitchesAndPorts(netInfo.isPrimary)...))

				targetPodInfo := dummyL2TestPod(ns, netInfo, targetPodInfoIdx)
				targetKvPod := newMultiHomedKubevirtPod(
					migrationInfo.vmName,
					migrationInfo.targetPodInfo,
					targetPodInfo,
					netInfo)

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(targetKvPod.Namespace).Create(context.Background(), targetKvPod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("asserting the OVN entities provisioned in the NBDB are the expected ones after migration")
				expectedPodLspEnabled := map[string]*bool{}
				expectedPodLspEnabled[sourcePodInfo.podName] = migrationInfo.sourcePodInfo.expectedLspEnabled

				testPods := []testPod{sourcePodInfo}
				if !util.PodCompleted(targetKvPod) {
					testPods = append(testPods, targetPodInfo)
					expectedPodLspEnabled[targetPodInfo.podName] = migrationInfo.targetPodInfo.expectedLspEnabled
				}
				Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(
						newSecondaryNetworkExpectationMachine(
							fakeOvn,
							testPods,
							expectationOptions...,
						).expectedLogicalSwitchesAndPortsWithLspEnabled(netInfo.isPrimary, expectedPodLspEnabled)...))
				return nil
			}

			Expect(app.Run([]string{app.Name})).To(Succeed())
		},

		Entry("on a layer2 topology with user defined secondary network, when target pod is not yet ready",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
			notReadyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined secondary network, when target pod is ready",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
			readyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined secondary network and an IC cluster, when target pod is not yet ready",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			notReadyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined secondary network and an IC cluster, when target pod is ready",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			readyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined secondary network and an IC cluster, when target pod failed",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			failedMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined primary network, when target pod is not yet ready",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
			notReadyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined primary network, when target pod is ready",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
			readyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined primary network and an IC cluster, when target pod is not yet ready",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			notReadyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined primary network and an IC cluster, when target pod is ready",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			readyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined primary network and an IC cluster, when target pod failed",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			failedMigrationInfo(),
		),
	)

	DescribeTable(
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

				nadController := &nad.FakeNADController{
					PrimaryNetworks: map[string]util.NetInfo{},
				}
				nadController.PrimaryNetworks[ns] = networkConfig
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
				nbZone := &nbdb.NBGlobal{Name: ovntypes.OvnDefaultZone, UUID: ovntypes.OvnDefaultZone}

				if netInfo.isPrimary {
					gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
					Expect(err).NotTo(HaveOccurred())
					initialDB.NBData = append(
						initialDB.NBData,
						expectedLayer2EgressEntities(networkConfig, *gwConfig, nodeName)...)
				}
				initialDB.NBData = append(initialDB.NBData, nbZone)

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
							*newMultiHomedPod(podInfo, netInfo),
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
						nadController,
					).Cleanup()).To(Succeed())
				Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{nbZone}))

				return nil
			}
			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		Entry("pod on a user defined primary network",
			dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16"),
			nonICClusterTestConfiguration(),
		),
		Entry("pod on a user defined primary network on an IC cluster",
			dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16"),
			icClusterTestConfiguration(),
		),
		Entry("pod on a user defined primary network on an IC cluster with per-pod SNATs enabled",
			dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16"),
			icClusterTestConfiguration(func(testConfig *testConfiguration) {
				testConfig.gatewayConfig = &config.GatewayConfig{DisableSNATMultipleGWs: true}
			}),
		),
	)

})

func dummyLocalnetWithSecondaryUserDefinedNetwork(subnets string) secondaryNetInfo {
	return secondaryNetInfo{
		netName:        secondaryNetworkName,
		nadName:        namespacedName(ns, nadName),
		topology:       ovntypes.LocalnetTopology,
		clustersubnets: subnets,
	}
}

func dummySecondaryLayer2UserDefinedNetwork(subnets string) secondaryNetInfo {
	return secondaryNetInfo{
		netName:        secondaryNetworkName,
		nadName:        namespacedName(ns, nadName),
		topology:       ovntypes.Layer2Topology,
		clustersubnets: subnets,
	}
}

func dummyPrimaryLayer2UserDefinedNetwork(subnets string) secondaryNetInfo {
	secondaryNet := dummySecondaryLayer2UserDefinedNetwork(subnets)
	secondaryNet.isPrimary = true
	return secondaryNet
}

func dummyL2TestPod(nsName string, info secondaryNetInfo, podIdx int) testPod {
	const nodeSubnet = "10.128.1.0/24"

	if info.isPrimary {
		pod := newTPod(nodeName, nodeSubnet, "10.128.1.2", "", fmt.Sprintf("myPod-%d", podIdx), fmt.Sprintf("10.128.1.%d", podIdx+3), fmt.Sprintf("0a:58:0a:80:01:%0.2d", podIdx+3), nsName)
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
			info.clustersubnets,
			"",
			"100.200.0.1",
			fmt.Sprintf("100.200.0.%d/16", podIdx+3),
			fmt.Sprintf("0a:58:64:c8:00:%0.2d", podIdx+3),
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
	pod := newTPod(nodeName, nodeSubnet, "10.128.1.2", "10.128.1.1", fmt.Sprintf("%s-%d", podName, podIdx), fmt.Sprintf("10.128.1.%d", podIdx+3), fmt.Sprintf("0a:58:0a:80:01:%0.2d", podIdx+3), nsName)
	pod.addNetwork(
		info.netName,
		info.nadName,
		info.clustersubnets,
		"",
		"",
		fmt.Sprintf("100.200.0.%d/16", podIdx+1),
		fmt.Sprintf("0a:58:64:c8:00:%0.2d", podIdx+1),
		"secondary",
		0,
		[]util.PodRoute{},
	)
	return pod
}

func dummyL2TestPodAdditionalNetworkIP() string {
	const podIdx = 0
	secNetInfo := dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16")
	return dummyL2TestPod(ns, secNetInfo, podIdx).getNetworkPortInfo(secNetInfo.netName, secNetInfo.nadName).podIP
}

func expectedLayer2EgressEntities(netInfo util.NetInfo, gwConfig util.L3GatewayConfig, nodeName string) []libovsdbtest.TestData {
	const (
		nat1               = "nat1-UUID"
		nat2               = "nat2-UUID"
		nat3               = "nat3-UUID"
		perPodSNAT         = "pod-snat-UUID"
		sr1                = "sr1-UUID"
		sr2                = "sr2-UUID"
		lrsr1              = "lrsr1-UUID"
		routerPolicyUUID1  = "lrp1-UUID"
		hostCIDRPolicyUUID = "host-cidr-policy-UUID"
		masqSNATUUID1      = "masq-snat1-UUID"
	)
	gwRouterName := fmt.Sprintf("GR_%s_test-node", netInfo.GetNetworkName())
	staticRouteOutputPort := ovntypes.GWRouterToExtSwitchPrefix + gwRouterName
	gwRouterToNetworkSwitchPortName := ovntypes.RouterToSwitchPrefix + netInfo.GetNetworkScopedName(ovntypes.OVNLayer2Switch)
	gwRouterToExtSwitchPortName := fmt.Sprintf("%s%s", ovntypes.GWRouterToExtSwitchPrefix, gwRouterName)
	masqSNAT := newMasqueradeManagementNATEntry(masqSNATUUID1, "169.254.169.14", layer2Subnet().String(), netInfo)

	var nat []string
	if config.Gateway.DisableSNATMultipleGWs {
		nat = append(nat, nat1, nat3, perPodSNAT, masqSNATUUID1)
	} else {
		nat = append(nat, nat1, nat2, nat3, masqSNATUUID1)
	}
	gr := &nbdb.LogicalRouter{
		Name:         gwRouterName,
		UUID:         gwRouterName + "-UUID",
		Nat:          nat,
		Ports:        []string{gwRouterToNetworkSwitchPortName + "-UUID", gwRouterToExtSwitchPortName + "-UUID"},
		StaticRoutes: []string{sr1, sr2},
		ExternalIDs:  gwRouterExternalIDs(netInfo, gwConfig),
		Options:      gwRouterOptions(gwConfig),
		Policies:     []string{routerPolicyUUID1},
	}
	gr.Options["lb_force_snat_ip"] = gwRouterJoinIPAddress().IP.String()
	expectedEntities := []libovsdbtest.TestData{
		gr,
		expectedGWToNetworkSwitchRouterPort(gwRouterToNetworkSwitchPortName, netInfo, gwRouterJoinIPAddress(), layer2SubnetGWAddr()),
		expectedGRStaticRoute(sr1, dummyMasqueradeSubnet().String(), nextHopMasqueradeIP().String(), nil, &staticRouteOutputPort, netInfo),
		expectedGRStaticRoute(sr2, ipv4DefaultRoute().String(), nodeGateway().IP.String(), nil, &staticRouteOutputPort, netInfo),
		expectedGRToExternalSwitchLRP(gwRouterName, netInfo, nodePhysicalIPAddress(), udnGWSNATAddress()),
		masqSNAT,
		expectedLogicalRouterPolicy(routerPolicyUUID1, netInfo, nodeName, nodeIP().IP.String(), managementPortIP(layer2Subnet()).String()),
	}

	expectedEntities = append(expectedEntities, expectedStaticMACBindings(gwRouterName, staticMACBindingIPs())...)

	if config.Gateway.Mode == config.GatewayModeLocal {
		l2LGWLRP := expectedLogicalRouterPolicy(hostCIDRPolicyUUID, netInfo, nodeName, nodeCIDR().String(), managementPortIP(layer2Subnet()).String())
		l2LGWLRP.Match = fmt.Sprintf(`ip4.dst == %s && ip4.src == %s`, nodeCIDR().String(), layer2Subnet().String())
		l2LGWLRP.Priority, _ = strconv.Atoi(ovntypes.UDNHostCIDRPolicyPriority)
		expectedEntities = append(expectedEntities, l2LGWLRP)
		gr.Policies = append(gr.Policies, hostCIDRPolicyUUID)
		lrsr := expectedGRStaticRoute(lrsr1, layer2Subnet().String(), managementPortIP(layer2Subnet()).String(),
			&nbdb.LogicalRouterStaticRoutePolicySrcIP, nil, netInfo)
		expectedEntities = append(expectedEntities, lrsr)
		gr.StaticRoutes = append(gr.StaticRoutes, lrsr1)
	}

	expectedEntities = append(expectedEntities, expectedExternalSwitchAndLSPs(netInfo, gwConfig, nodeName)...)
	if config.Gateway.DisableSNATMultipleGWs {
		expectedEntities = append(expectedEntities, newNATEntry(nat1, dummyMasqueradeIP().IP.String(), gwRouterJoinIPAddress().IP.String(), standardNonDefaultNetworkExtIDs(netInfo), ""))
		expectedEntities = append(expectedEntities, newNATEntry(nat3, dummyMasqueradeIP().IP.String(), layer2SubnetGWAddr().IP.String(), standardNonDefaultNetworkExtIDs(netInfo), ""))
		expectedEntities = append(expectedEntities, newNATEntry(perPodSNAT, dummyMasqueradeIP().IP.String(), dummyL2TestPodAdditionalNetworkIP(), nil, fmt.Sprintf("outport == %q", gwRouterToExtSwitchPortName)))
	} else {
		expectedEntities = append(expectedEntities, newNATEntry(nat1, dummyMasqueradeIP().IP.String(), gwRouterJoinIPAddress().IP.String(), standardNonDefaultNetworkExtIDs(netInfo), ""))
		expectedEntities = append(expectedEntities, newNATEntry(nat2, dummyMasqueradeIP().IP.String(), layer2Subnet().String(), standardNonDefaultNetworkExtIDs(netInfo), fmt.Sprintf("outport == %q", gwRouterToExtSwitchPortName)))
		expectedEntities = append(expectedEntities, newNATEntry(nat3, dummyMasqueradeIP().IP.String(), layer2SubnetGWAddr().IP.String(), standardNonDefaultNetworkExtIDs(netInfo), ""))
	}
	return expectedEntities
}

func expectedGWToNetworkSwitchRouterPort(name string, netInfo util.NetInfo, networks ...*net.IPNet) *nbdb.LogicalRouterPort {
	options := map[string]string{"gateway_mtu": fmt.Sprintf("%d", 1400)}
	lrp := expectedLogicalRouterPort(name, netInfo, options, networks...)

	if config.IPv6Mode {
		lrp.Ipv6RaConfigs = map[string]string{
			"address_mode":      "dhcpv6_stateful",
			"mtu":               "1400",
			"send_periodic":     "true",
			"max_interval":      "900",
			"min_interval":      "300",
			"router_preference": "LOW",
		}
	}
	return lrp
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
		netName:        secondaryNetworkName,
		nadName:        namespacedName(ns, nadName),
		topology:       ovntypes.Layer2Topology,
		clustersubnets: subnets,
	}
}

func dummyLayer2PrimaryUserDefinedNetwork(subnets string) secondaryNetInfo {
	secondaryNet := dummyLayer2SecondaryUserDefinedNetwork(subnets)
	secondaryNet.isPrimary = true
	return secondaryNet
}

func newSecondaryLayer2NetworkController(cnci *CommonNetworkControllerInfo, netInfo util.NetInfo, nodeName string,
	nadController networkAttachDefController.NADController) *SecondaryLayer2NetworkController {
	layer2NetworkController, _ := NewSecondaryLayer2NetworkController(cnci, netInfo, nadController)
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

func nodeCIDR() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("192.168.126.0"),
		Mask: net.CIDRMask(24, 32),
	}
}

func setupFakeOvnForLayer2Topology(fakeOvn *FakeOVN, initialDB libovsdbtest.TestSetup, netInfo secondaryNetInfo, testNode *v1.Node, podInfo testPod, pod *v1.Pod) error {
	By(fmt.Sprintf("creating a network attachment definition for network: %s", netInfo.netName))
	By(fmt.Sprintf("creating a network attachment definition for network: %s", netInfo.netName))
	nad, err := newNetworkAttachmentDefinition(
		ns,
		nadName,
		*netInfo.netconf(),
	)
	Expect(err).NotTo(HaveOccurred())
	By("setting up the OVN DB without any entities in it")
	Expect(netInfo.setupOVNDependencies(&initialDB)).To(Succeed())

	if netInfo.isPrimary {
		networkConfig, err := util.NewNetInfo(netInfo.netconf())
		Expect(err).NotTo(HaveOccurred())

		initialDB.NBData = append(
			initialDB.NBData,
			&nbdb.LogicalRouter{
				Name:        fmt.Sprintf("GR_%s_%s", networkConfig.GetNetworkName(), nodeName),
				ExternalIDs: standardNonDefaultNetworkExtIDs(networkConfig),
			},
			newNetworkClusterPortGroup(networkConfig),
		)
	}

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
				*pod,
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
		if err != nil {
			return err
		}
		_, ok := pod.Annotations[util.OvnPodAnnotationName]
		if ok {
			return fmt.Errorf("expected pod annotation %q", util.OvnPodAnnotationName)
		}
	}
	if err = fakeOvn.controller.nadController.Start(); err != nil {
		return err
	}

	if err = fakeOvn.controller.WatchNamespaces(); err != nil {
		return err
	}
	if err = fakeOvn.controller.WatchPods(); err != nil {
		return err
	}
	By("asserting the pod (once reconciled) *features* the OVN pod networks annotation")
	secondaryNetController, doesControllerExist := fakeOvn.secondaryControllers[secondaryNetworkName]
	if !doesControllerExist {
		return fmt.Errorf("expected secondary network controller to exist")
	}

	secondaryNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
	podInfo.populateSecondaryNetworkLogicalSwitchCache(fakeOvn, secondaryNetController)
	if err = secondaryNetController.bnc.WatchNodes(); err != nil {
		return err
	}
	if err = secondaryNetController.bnc.WatchPods(); err != nil {
		return err
	}

	return nil
}

func setupConfig(netInfo secondaryNetInfo, testConfig testConfiguration, gatewayMode config.GatewayMode) {
	if testConfig.configToOverride != nil {
		config.OVNKubernetesFeature = *testConfig.configToOverride
		if testConfig.gatewayConfig != nil {
			config.Gateway.DisableSNATMultipleGWs = testConfig.gatewayConfig.DisableSNATMultipleGWs
		}
	}
	config.Gateway.Mode = gatewayMode
	if knet.IsIPv6CIDRString(netInfo.clustersubnets) {
		config.IPv6Mode = true
		// tests dont support dualstack yet
		config.IPv4Mode = false
	}
}

func notReadyMigrationInfo() *liveMigrationInfo {
	const vmName = "my-vm"
	return &liveMigrationInfo{
		vmName: vmName,
		sourcePodInfo: liveMigrationPodInfo{
			podPhase:           v1.PodRunning,
			creationTimestamp:  metav1.NewTime(time.Now().Add(-time.Hour)),
			expectedLspEnabled: lspEnableNotSpecified,
		},
		targetPodInfo: liveMigrationPodInfo{
			podPhase:           v1.PodRunning,
			creationTimestamp:  metav1.NewTime(time.Now()),
			expectedLspEnabled: lspEnableExplicitlyFalse,
		},
	}
}

func readyMigrationInfo() *liveMigrationInfo {
	const vmName = "my-vm"
	return &liveMigrationInfo{
		vmName: vmName,
		sourcePodInfo: liveMigrationPodInfo{
			podPhase:           v1.PodRunning,
			creationTimestamp:  metav1.NewTime(time.Now().Add(-time.Hour)),
			expectedLspEnabled: lspEnableExplicitlyFalse,
		},
		targetPodInfo: liveMigrationPodInfo{
			podPhase:           v1.PodRunning,
			creationTimestamp:  metav1.NewTime(time.Now()),
			annotation:         map[string]string{kubevirtv1.MigrationTargetReadyTimestamp: "some-timestamp"},
			expectedLspEnabled: lspEnableExplicitlyTrue,
		},
	}
}

func failedMigrationInfo() *liveMigrationInfo {
	const vmName = "my-vm"
	return &liveMigrationInfo{
		vmName: vmName,
		sourcePodInfo: liveMigrationPodInfo{
			podPhase:           v1.PodRunning,
			creationTimestamp:  metav1.NewTime(time.Now().Add(-time.Hour)),
			expectedLspEnabled: lspEnableExplicitlyTrue,
		},
		targetPodInfo: liveMigrationPodInfo{
			podPhase:          v1.PodFailed,
			creationTimestamp: metav1.NewTime(time.Now()),
		},
	}
}

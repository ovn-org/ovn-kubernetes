package ovn

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	. "github.com/onsi/gomega"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	knet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	testnm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	networkingv1 "k8s.io/api/networking/v1"
)

type secondaryNetInfo struct {
	netName        string
	nadName        string
	clustersubnets string
	hostsubnets    string // not used in layer2 tests
	topology       string
	isPrimary      bool
}

const (
	nadName              = "blue-net"
	ns                   = "namespace1"
	secondaryNetworkName = "isolatednet"
	denyPolicyName       = "deny-all-policy"
	denyPG               = "deny-port-group"
)

type testConfiguration struct {
	configToOverride   *config.OVNKubernetesFeatureConfig
	gatewayConfig      *config.GatewayConfig
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

	DescribeTable(
		"reconciles a new",
		func(netInfo secondaryNetInfo, testConfig testConfiguration, gwMode config.GatewayMode) {
			podInfo := dummyTestPod(ns, netInfo)
			if testConfig.configToOverride != nil {
				config.OVNKubernetesFeature = *testConfig.configToOverride
				if testConfig.gatewayConfig != nil {
					config.Gateway.DisableSNATMultipleGWs = testConfig.gatewayConfig.DisableSNATMultipleGWs
				}
			}
			config.Gateway.Mode = gwMode
			if knet.IsIPv6CIDRString(netInfo.clustersubnets) {
				config.IPv6Mode = true
				// tests dont support dualstack yet
				config.IPv4Mode = false
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
					networkConfig, err := util.NewNetInfo(netInfo.netconf())
					Expect(err).NotTo(HaveOccurred())
					initialDB.NBData = append(
						initialDB.NBData,
						&nbdb.LogicalSwitch{
							Name:        fmt.Sprintf("%s_join", netInfo.netName),
							ExternalIDs: standardNonDefaultNetworkExtIDs(networkConfig),
						},
						&nbdb.LogicalRouter{
							Name:        fmt.Sprintf("%s_ovn_cluster_router", netInfo.netName),
							ExternalIDs: standardNonDefaultNetworkExtIDs(networkConfig),
						},
						&nbdb.LogicalRouterPort{
							Name: fmt.Sprintf("rtos-%s_%s", netInfo.netName, nodeName),
						},
					)
					initialDB.NBData = append(initialDB.NBData, getHairpinningACLsV4AndPortGroup()...)
					initialDB.NBData = append(initialDB.NBData, getHairpinningACLsV4AndPortGroupForNetwork(networkConfig, nil)...)
				}

				const nodeIPv4CIDR = "192.168.126.202/24"
				testNode, err := newNodeWithSecondaryNets(nodeName, nodeIPv4CIDR, netInfo)
				Expect(err).NotTo(HaveOccurred())
				networkPolicy := getMatchLabelsNetworkPolicy(denyPolicyName, ns, "", "", false, false)
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
					&networkingv1.NetworkPolicyList{
						Items: []networkingv1.NetworkPolicy{*networkPolicy},
					},
				)
				podInfo.populateLogicalSwitchCache(fakeOvn)

				// pod exists, networks annotations don't
				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.Background(), podInfo.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				Expect(fakeOvn.networkManager.Start()).NotTo(HaveOccurred())
				defer fakeOvn.networkManager.Stop()

				Expect(fakeOvn.controller.WatchNamespaces()).NotTo(HaveOccurred())
				Expect(fakeOvn.controller.WatchPods()).NotTo(HaveOccurred())
				if netInfo.isPrimary {
					Expect(fakeOvn.controller.WatchNetworkPolicy()).NotTo(HaveOccurred())
				}
				secondaryNetController, ok := fakeOvn.secondaryControllers[secondaryNetworkName]
				Expect(ok).To(BeTrue())

				secondaryNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
				podInfo.populateSecondaryNetworkLogicalSwitchCache(fakeOvn, secondaryNetController)
				Expect(secondaryNetController.bnc.WatchNodes()).To(Succeed())
				Expect(secondaryNetController.bnc.WatchPods()).To(Succeed())

				if netInfo.isPrimary {
					Expect(secondaryNetController.bnc.WatchNetworkPolicy()).To(Succeed())
					ninfo, err := fakeOvn.networkManager.Interface().GetActiveNetworkForNamespace(ns)
					Expect(err).NotTo(HaveOccurred())
					Expect(ninfo.GetNetworkName()).To(Equal(netInfo.netName))
				}

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
					if testConfig.configToOverride != nil && testConfig.configToOverride.EnableEgressFirewall {
						defaultNetExpectations = append(defaultNetExpectations,
							buildNamespacedPortGroup(podInfo.namespace, DefaultNetworkControllerName))
						secNetPG := buildNamespacedPortGroup(podInfo.namespace, secondaryNetController.bnc.controllerName)
						portName := util.GetSecondaryNetworkLogicalPortName(podInfo.namespace, podInfo.podName, netInfo.nadName) + "-UUID"
						secNetPG.Ports = []string{portName}
						defaultNetExpectations = append(defaultNetExpectations, secNetPG)
					}
					networkConfig, err := util.NewNetInfo(netInfo.netconf())
					Expect(err).NotTo(HaveOccurred())
					// Add NetPol hairpin ACLs and PGs for the validation.
					mgmtPortName := managementPortName(secondaryNetController.bnc.GetNetworkScopedName(nodeName))
					mgmtPortUUID := mgmtPortName + "-UUID"
					defaultNetExpectations = append(defaultNetExpectations, getHairpinningACLsV4AndPortGroup()...)
					defaultNetExpectations = append(defaultNetExpectations, getHairpinningACLsV4AndPortGroupForNetwork(networkConfig,
						[]string{mgmtPortUUID})...)
					// Add Netpol deny policy ACLs and PGs for the validation.
					podLPortName := util.GetSecondaryNetworkLogicalPortName(podInfo.namespace, podInfo.podName, netInfo.nadName) + "-UUID"
					dataParams := newNetpolDataParams(networkPolicy).withLocalPortUUIDs(podLPortName).withNetInfo(networkConfig)
					defaultDenyExpectedData := getDefaultDenyData(dataParams)
					pgDbIDs := getNetworkPolicyPortGroupDbIDs(ns, secondaryNetController.bnc.controllerName, denyPolicyName)
					ingressPG := libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil)
					ingressPG.UUID = denyPG
					ingressPG.Ports = []string{podLPortName}
					defaultNetExpectations = append(defaultNetExpectations, ingressPG)
					defaultNetExpectations = append(defaultNetExpectations, defaultDenyExpectedData...)
				}
				Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(
						append(
							defaultNetExpectations,
							newSecondaryNetworkExpectationMachine(
								fakeOvn,
								[]testPod{podInfo},
								expectationOptions...,
							).expectedLogicalSwitchesAndPorts(netInfo.isPrimary)...)))

				return nil
			}

			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		Entry("pod on a user defined secondary network",
			dummySecondaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			nonICClusterTestConfiguration(),
			config.GatewayModeShared,
		),
		Entry("pod on a user defined primary network",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			nonICClusterTestConfiguration(),
			config.GatewayModeShared,
		),
		Entry("pod on a user defined secondary network on an IC cluster",
			dummySecondaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(),
			config.GatewayModeShared,
		),
		Entry("pod on a user defined primary network on an IC cluster",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(),
			config.GatewayModeShared,
		),
		Entry("pod on a user defined primary network on an IC cluster; LGW",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(),
			config.GatewayModeLocal,
		),
		Entry("pod on a user defined primary network on an IC cluster with per-pod SNATs enabled",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(func(testConfig *testConfiguration) {
				testConfig.gatewayConfig = &config.GatewayConfig{DisableSNATMultipleGWs: true}
			}),
			config.GatewayModeShared,
		),
		Entry("pod on a user defined primary network on an IC cluster with EgressFirewall enabled",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(func(config *testConfiguration) {
				config.configToOverride.EnableEgressFirewall = true
			}),
			config.GatewayModeShared,
		),
	)

	DescribeTable(
		"the gateway is properly cleaned up",
		func(netInfo secondaryNetInfo, testConfig testConfiguration) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
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

				mutableNetworkConfig := util.NewMutableNetInfo(networkConfig)
				mutableNetworkConfig.SetNADs(util.GetNADName(nad.Namespace, nad.Name))
				networkConfig = mutableNetworkConfig

				fakeNetworkManager := &testnm.FakeNetworkManager{
					PrimaryNetworks: make(map[string]util.NetInfo),
				}
				fakeNetworkManager.PrimaryNetworks[ns] = networkConfig

				const nodeIPv4CIDR = "192.168.126.202/24"
				testNode, err := newNodeWithSecondaryNets(nodeName, nodeIPv4CIDR, netInfo)
				Expect(err).NotTo(HaveOccurred())

				nbZone := &nbdb.NBGlobal{Name: ovntypes.OvnDefaultZone, UUID: ovntypes.OvnDefaultZone}
				defaultNetExpectations := emptyDefaultClusterNetworkNodeSwitch(podInfo.nodeName)
				defaultNetExpectations = append(defaultNetExpectations, nbZone)
				gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
				Expect(err).NotTo(HaveOccurred())
				Expect(gwConfig.NextHops).NotTo(BeEmpty())

				if netInfo.isPrimary {
					gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
					Expect(err).NotTo(HaveOccurred())
					initialDB.NBData = append(
						initialDB.NBData,
						expectedGWEntities(podInfo.nodeName, netInfo.hostsubnets, networkConfig, *gwConfig)...)
					initialDB.NBData = append(
						initialDB.NBData,
						expectedLayer3EgressEntities(networkConfig, *gwConfig, testing.MustParseIPNet(netInfo.hostsubnets))...)
					initialDB.NBData = append(initialDB.NBData,
						newNetworkClusterPortGroup(networkConfig),
					)
					if testConfig.configToOverride != nil && testConfig.configToOverride.EnableEgressFirewall {
						defaultNetExpectations = append(defaultNetExpectations,
							buildNamespacedPortGroup(podInfo.namespace, DefaultNetworkControllerName))
					}
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

				// pod exists, networks annotations don't
				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.Background(), podInfo.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				Expect(fakeOvn.networkManager.Start()).NotTo(HaveOccurred())
				defer fakeOvn.networkManager.Stop()

				Expect(fakeOvn.controller.WatchNamespaces()).To(Succeed())
				Expect(fakeOvn.controller.WatchPods()).To(Succeed())
				secondaryNetController, ok := fakeOvn.secondaryControllers[secondaryNetworkName]
				Expect(ok).To(BeTrue())

				secondaryNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
				podInfo.populateSecondaryNetworkLogicalSwitchCache(fakeOvn, secondaryNetController)
				Expect(secondaryNetController.bnc.WatchNodes()).To(Succeed())
				Expect(secondaryNetController.bnc.WatchPods()).To(Succeed())

				if netInfo.isPrimary {
					Expect(secondaryNetController.bnc.WatchNetworkPolicy()).To(Succeed())
				}

				Expect(fakeOvn.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
				Expect(fakeOvn.fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Delete(context.Background(), nad.Name, metav1.DeleteOptions{})).To(Succeed())

				// we must access the layer3 controller to be able to issue its cleanup function (to remove the GW related stuff).
				Expect(
					newSecondaryLayer3NetworkController(
						&secondaryNetController.bnc.CommonNetworkControllerInfo,
						networkConfig,
						nodeName,
						fakeNetworkManager,
						nil,
						NewPortCache(ctx.Done()),
					).Cleanup()).To(Succeed())
				Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(defaultNetExpectations))

				return nil
			}
			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		Entry("pod on a user defined primary network",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			nonICClusterTestConfiguration(),
		),
		Entry("pod on a user defined primary network on an IC cluster",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(),
		),
		Entry("pod on a user defined primary network on an IC cluster with per-pod SNATs enabled",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(func(testConfig *testConfiguration) {
				testConfig.gatewayConfig = &config.GatewayConfig{DisableSNATMultipleGWs: true}
			}),
		),
		Entry("pod on a user defined primary network on an IC cluster with EgressFirewall enabled",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(func(config *testConfiguration) {
				config.configToOverride.EnableEgressFirewall = true
			}),
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
		primaryUDNConfig.hostsubnets,
		"",
		nodeGWIP,
		"192.168.1.3/24",
		"0a:58:c0:a8:01:03",
		"primary",
		0,
		[]util.PodRoute{
			{
				Dest:    testing.MustParseIPNet("192.168.0.0/16"),
				NextHop: testing.MustParseIP("192.168.1.1"),
			},
			{
				Dest:    testing.MustParseIPNet("172.16.1.0/24"),
				NextHop: testing.MustParseIP("192.168.1.1"),
			},
			{
				Dest:    testing.MustParseIPNet("100.65.0.0/16"),
				NextHop: testing.MustParseIP("192.168.1.1"),
			},
		},
	)
	return pod
}

func namespacedName(ns, name string) string { return fmt.Sprintf("%s/%s", ns, name) }

func (sni *secondaryNetInfo) getNetworkRole() string {
	return util.GetUserDefinedNetworkRole(sni.isPrimary)
}

func getNetworkRole(netInfo util.NetInfo) string {
	return util.GetUserDefinedNetworkRole(netInfo.IsPrimaryNetwork())
}

func (sni *secondaryNetInfo) setupOVNDependencies(dbData *libovsdbtest.TestSetup) error {
	netInfo, err := util.NewNetInfo(sni.netconf())
	if err != nil {
		return err
	}

	externalIDs := map[string]string{
		types.NetworkExternalID:     sni.netName,
		types.NetworkRoleExternalID: sni.getNetworkRole(),
	}
	switch sni.topology {
	case types.Layer2Topology:
		dbData.NBData = append(dbData.NBData, &nbdb.LogicalSwitch{
			Name:        netInfo.GetNetworkScopedName(types.OVNLayer2Switch),
			UUID:        netInfo.GetNetworkScopedName(types.OVNLayer2Switch) + "_UUID",
			ExternalIDs: externalIDs,
		})
	case types.Layer3Topology:
		dbData.NBData = append(dbData.NBData, &nbdb.LogicalSwitch{
			Name:        netInfo.GetNetworkScopedName(nodeName),
			UUID:        netInfo.GetNetworkScopedName(nodeName) + "_UUID",
			ExternalIDs: externalIDs,
		})
	case types.LocalnetTopology:
		dbData.NBData = append(dbData.NBData, &nbdb.LogicalSwitch{
			Name:        netInfo.GetNetworkScopedName(types.OVNLocalnetSwitch),
			UUID:        netInfo.GetNetworkScopedName(types.OVNLocalnetSwitch) + "_UUID",
			ExternalIDs: externalIDs,
		})
	default:
		return fmt.Errorf("missing topology in the network configuration: %v", sni)
	}
	return nil
}

func (sni *secondaryNetInfo) netconf() *ovncnitypes.NetConf {
	const plugin = "ovn-k8s-cni-overlay"

	role := types.NetworkRoleSecondary
	if sni.isPrimary {
		role = types.NetworkRolePrimary
	}
	return &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: sni.netName,
			Type: plugin,
		},
		Topology: sni.topology,
		NADName:  sni.nadName,
		Subnets:  sni.clustersubnets,
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
			"192.168.1.1",
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
		info.hostsubnets,
		"",
		"",
		"192.168.1.3/24",
		"0a:58:c0:a8:01:03",
		"secondary",
		0,
		[]util.PodRoute{
			{
				Dest:    testing.MustParseIPNet(info.clustersubnets),
				NextHop: testing.MustParseIP("192.168.1.1"),
			},
		},
	)
	return pod
}

func dummyTestPodAdditionalNetworkIP() string {
	secNetInfo := dummyPrimaryLayer2UserDefinedNetwork("192.168.0.0/16")
	return dummyTestPod(ns, secNetInfo).getNetworkPortInfo(secNetInfo.netName, secNetInfo.nadName).podIP
}

func dummySecondaryLayer3UserDefinedNetwork(clustersubnets, hostsubnets string) secondaryNetInfo {
	return secondaryNetInfo{
		netName:        secondaryNetworkName,
		nadName:        namespacedName(ns, nadName),
		topology:       types.Layer3Topology,
		clustersubnets: clustersubnets,
		hostsubnets:    hostsubnets,
	}
}

func dummyPrimaryLayer3UserDefinedNetwork(clustersubnets, hostsubnets string) secondaryNetInfo {
	secondaryNet := dummySecondaryLayer3UserDefinedNetwork(clustersubnets, hostsubnets)
	secondaryNet.isPrimary = true
	return secondaryNet
}

// This util is returning a network-name/hostSubnet for the node's node-subnets annotation
func (sni *secondaryNetInfo) String() string {
	return fmt.Sprintf("%q: %q", sni.netName, sni.hostsubnets)
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
				"k8s.ovn.org/node-primary-ifaddr":                           fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4CIDR, ""),
				"k8s.ovn.org/node-subnets":                                  fmt.Sprintf("{\"default\":\"%s\", %s}", v4Node1Subnet, strings.Join(nodeSubnetInfo, ",")),
				util.OVNNodeHostCIDRs:                                       fmt.Sprintf("[\"%s\"]", nodeIPv4CIDR),
				"k8s.ovn.org/zone-name":                                     "global",
				"k8s.ovn.org/l3-gateway-config":                             fmt.Sprintf("{\"default\":{\"mode\":\"shared\",\"bridge-id\":\"breth0\",\"interface-id\":\"breth0_ovn-worker\",\"mac-address\":%q,\"ip-addresses\":[%[2]q],\"ip-address\":%[2]q,\"next-hops\":[%[3]q],\"next-hop\":%[3]q,\"node-port-enable\":\"true\",\"vlan-id\":\"0\"}}", util.IPAddrToHWAddr(nodeIP), nodeCIDR, nextHopIP),
				util.OvnNodeChassisID:                                       "abdcef",
				"k8s.ovn.org/network-ids":                                   "{\"default\":\"0\",\"isolatednet\":\"2\"}",
				util.OVNNodeGRLRPAddrs:                                      fmt.Sprintf("{\"isolatednet\":{\"ipv4\":%q}}", gwRouterJoinIPAddress()),
				"k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids": "{\"isolatednet\":\"25\"}",
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
	return []*net.IPNet{dummyMasqueradeIP()}
}

func dummyMasqueradeIP() *net.IPNet {
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

func expectedGWEntities(nodeName, nodeSubnet string, netInfo util.NetInfo, gwConfig util.L3GatewayConfig) []libovsdbtest.TestData {
	gwRouterName := fmt.Sprintf("GR_%s_%s", netInfo.GetNetworkName(), nodeName)

	expectedEntities := append(
		expectedGWRouterPlusNATAndStaticRoutes(nodeName, gwRouterName, netInfo, gwConfig),
		expectedGRToJoinSwitchLRP(gwRouterName, gwRouterJoinIPAddress(), netInfo),
		expectedGRToExternalSwitchLRP(gwRouterName, netInfo, nodePhysicalIPAddress(), udnGWSNATAddress()),
		expectedGatewayChassis(nodeName, netInfo, gwConfig),
	)
	expectedEntities = append(expectedEntities, expectedStaticMACBindings(gwRouterName, staticMACBindingIPs())...)
	expectedEntities = append(expectedEntities, expectedExternalSwitchAndLSPs(netInfo, gwConfig, nodeName)...)
	expectedEntities = append(expectedEntities, expectedJoinSwitchAndLSPs(netInfo, nodeName)...)
	return expectedEntities
}

func expectedGWRouterPlusNATAndStaticRoutes(
	nodeName, gwRouterName string,
	netInfo util.NetInfo,
	gwConfig util.L3GatewayConfig,
) []libovsdbtest.TestData {
	gwRouterToExtLRPUUID := fmt.Sprintf("%s%s-UUID", types.GWRouterToExtSwitchPrefix, gwRouterName)
	gwRouterToJoinLRPUUID := fmt.Sprintf("%s%s-UUID", types.GWRouterToJoinSwitchPrefix, gwRouterName)

	const (
		nat1             = "abc-UUID"
		nat2             = "cba-UUID"
		perPodSNAT       = "pod-snat-UUID"
		staticRoute1     = "srA-UUID"
		staticRoute2     = "srB-UUID"
		staticRoute3     = "srC-UUID"
		ipv4DefaultRoute = "0.0.0.0/0"
	)

	staticRouteOutputPort := types.GWRouterToExtSwitchPrefix + netInfo.GetNetworkScopedGWRouterName(nodeName)
	nextHopIP := gwConfig.NextHops[0].String()
	nextHopMasqIP := nextHopMasqueradeIP().String()
	masqSubnet := config.Gateway.V4MasqueradeSubnet
	var nat []string
	if config.Gateway.DisableSNATMultipleGWs {
		nat = append(nat, nat1, perPodSNAT)
	} else {
		nat = append(nat, nat1, nat2)
	}
	expectedEntities := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			Name:         gwRouterName,
			UUID:         gwRouterName + "-UUID",
			ExternalIDs:  gwRouterExternalIDs(netInfo, gwConfig),
			Options:      gwRouterOptions(gwConfig),
			Ports:        []string{gwRouterToJoinLRPUUID, gwRouterToExtLRPUUID},
			Nat:          nat,
			StaticRoutes: []string{staticRoute1, staticRoute2, staticRoute3},
		},
		expectedGRStaticRoute(staticRoute1, netInfo.Subnets()[0].CIDR.String(), dummyMasqueradeIP().IP.String(), nil, nil, netInfo),
		expectedGRStaticRoute(staticRoute2, ipv4DefaultRoute, nextHopIP, nil, &staticRouteOutputPort, netInfo),
		expectedGRStaticRoute(staticRoute3, masqSubnet, nextHopMasqIP, nil, &staticRouteOutputPort, netInfo),
	}
	if config.Gateway.DisableSNATMultipleGWs {
		expectedEntities = append(expectedEntities, newNATEntry(nat1, dummyMasqueradeIP().IP.String(), gwRouterJoinIPAddress().IP.String(), standardNonDefaultNetworkExtIDs(netInfo), ""))
		expectedEntities = append(expectedEntities, newNATEntry(perPodSNAT, dummyMasqueradeIP().IP.String(), dummyTestPodAdditionalNetworkIP(), nil, ""))
	} else {
		expectedEntities = append(expectedEntities, newNATEntry(nat1, dummyMasqueradeIP().IP.String(), gwRouterJoinIPAddress().IP.String(), standardNonDefaultNetworkExtIDs(netInfo), ""))
		expectedEntities = append(expectedEntities, newNATEntry(nat2, dummyMasqueradeIP().IP.String(), netInfo.Subnets()[0].CIDR.String(), standardNonDefaultNetworkExtIDs(netInfo), ""))
	}
	return expectedEntities
}

func expectedStaticMACBindings(gwRouterName string, ips []net.IP) []libovsdbtest.TestData {
	lrpName := fmt.Sprintf("%s%s", ovntypes.GWRouterToExtSwitchPrefix, gwRouterName)
	var bindings []libovsdbtest.TestData
	for _, ip := range ips {
		bindings = append(bindings, &nbdb.StaticMACBinding{
			UUID:               fmt.Sprintf("%sstatic-mac-binding-UUID(%s)", lrpName, ip.String()),
			IP:                 ip.String(),
			LogicalPort:        lrpName,
			MAC:                util.IPAddrToHWAddr(ip).String(),
			OverrideDynamicMAC: true,
		})
	}
	return bindings
}

func expectedGatewayChassis(nodeName string, netInfo util.NetInfo, gwConfig util.L3GatewayConfig) *nbdb.GatewayChassis {
	gwChassisName := fmt.Sprintf("%s%s_%s-%s", types.RouterToSwitchPrefix, netInfo.GetNetworkName(), nodeName, gwConfig.ChassisID)
	return &nbdb.GatewayChassis{UUID: gwChassisName + "-UUID", Name: gwChassisName, Priority: 1, ChassisName: gwConfig.ChassisID}
}

func expectedGRToJoinSwitchLRP(gatewayRouterName string, gwRouterLRPIP *net.IPNet, netInfo util.NetInfo) *nbdb.LogicalRouterPort {
	lrpName := fmt.Sprintf("%s%s", types.GWRouterToJoinSwitchPrefix, gatewayRouterName)
	options := map[string]string{"gateway_mtu": fmt.Sprintf("%d", 1400)}
	return expectedLogicalRouterPort(lrpName, netInfo, options, gwRouterLRPIP)
}

func expectedGRToExternalSwitchLRP(gatewayRouterName string, netInfo util.NetInfo, joinSwitchIPs ...*net.IPNet) *nbdb.LogicalRouterPort {
	lrpName := fmt.Sprintf("%s%s", types.GWRouterToExtSwitchPrefix, gatewayRouterName)
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
			types.TopologyExternalID: netInfo.TopologyType(),
			types.NetworkExternalID:  netInfo.GetNetworkName(),
		},
	}
}

func expectedLayer3EgressEntities(netInfo util.NetInfo, gwConfig util.L3GatewayConfig, nodeSubnet *net.IPNet) []libovsdbtest.TestData {
	const (
		routerPolicyUUID1 = "lrpol1-UUID"
		routerPolicyUUID2 = "lrpol2-UUID"
		staticRouteUUID1  = "sr1-UUID"
		staticRouteUUID2  = "sr2-UUID"
		staticRouteUUID3  = "sr3-UUID"
		masqSNATUUID1     = "masq-snat1-UUID"
	)
	masqIPAddr := dummyMasqueradeIP().IP.String()
	clusterRouterName := fmt.Sprintf("%s_ovn_cluster_router", netInfo.GetNetworkName())
	rtosLRPName := fmt.Sprintf("%s%s", types.RouterToSwitchPrefix, netInfo.GetNetworkScopedName(nodeName))
	rtosLRPUUID := rtosLRPName + "-UUID"
	nodeIP := gwConfig.IPAddresses[0].IP.String()
	masqSNAT := newNATEntry(masqSNATUUID1, "169.254.169.14", nodeSubnet.String(), standardNonDefaultNetworkExtIDs(netInfo), "")
	masqSNAT.Match = getMasqueradeManagementIPSNATMatch(util.IPAddrToHWAddr(managementPortIP(nodeSubnet)).String())
	masqSNAT.LogicalPort = ptr.To(fmt.Sprintf("rtos-%s_%s", netInfo.GetNetworkName(), nodeName))
	if !config.OVNKubernetesFeature.EnableInterconnect {
		masqSNAT.GatewayPort = ptr.To(fmt.Sprintf("rtos-%s_%s", netInfo.GetNetworkName(), nodeName) + "-UUID")
	}

	gatewayChassisUUID := fmt.Sprintf("%s-%s-UUID", rtosLRPName, gwConfig.ChassisID)
	lrsrNextHop := gwRouterJoinIPAddress().IP.String()
	if config.Gateway.Mode == config.GatewayModeLocal {
		lrsrNextHop = managementPortIP(nodeSubnet).String()
	}
	expectedEntities := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			Name:         clusterRouterName,
			UUID:         clusterRouterName + "-UUID",
			Ports:        []string{rtosLRPUUID},
			StaticRoutes: []string{staticRouteUUID1, staticRouteUUID2},
			Policies:     []string{routerPolicyUUID1, routerPolicyUUID2},
			ExternalIDs:  standardNonDefaultNetworkExtIDs(netInfo),
			Nat:          []string{masqSNATUUID1},
		},
		&nbdb.LogicalRouterPort{UUID: rtosLRPUUID, Name: rtosLRPName, Networks: []string{"192.168.1.1/24"}, MAC: "0a:58:c0:a8:01:01", GatewayChassis: []string{gatewayChassisUUID}},
		expectedGRStaticRoute(staticRouteUUID1, nodeSubnet.String(), lrsrNextHop, &nbdb.LogicalRouterStaticRoutePolicySrcIP, nil, netInfo),
		expectedGRStaticRoute(staticRouteUUID2, gwRouterJoinIPAddress().IP.String(), gwRouterJoinIPAddress().IP.String(), nil, nil, netInfo),
		expectedLogicalRouterPolicy(routerPolicyUUID1, netInfo, nodeName, nodeIP, managementPortIP(nodeSubnet).String()),
		expectedLogicalRouterPolicy(routerPolicyUUID2, netInfo, nodeName, masqIPAddr, managementPortIP(nodeSubnet).String()),
		masqSNAT,
	}
	return expectedEntities
}

func expectedLogicalRouterPolicy(routerPolicyUUID1 string, netInfo util.NetInfo, nodeName, destIP, nextHop string) *nbdb.LogicalRouterPolicy {
	const (
		priority      = 1004
		rerouteAction = "reroute"
	)
	networkScopedSwitchName := netInfo.GetNetworkScopedSwitchName(nodeName)
	lrpName := fmt.Sprintf("%s%s", types.RouterToSwitchPrefix, networkScopedSwitchName)

	return &nbdb.LogicalRouterPolicy{
		UUID:        routerPolicyUUID1,
		Action:      rerouteAction,
		ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
		Match:       fmt.Sprintf("inport == %q && ip4.dst == %s /* %s */", lrpName, destIP, networkScopedSwitchName),
		Nexthops:    []string{nextHop},
		Priority:    priority,
	}
}

func expectedGRStaticRoute(uuid, ipPrefix, nextHop string, policy *nbdb.LogicalRouterStaticRoutePolicy, outputPort *string, netInfo util.NetInfo) *nbdb.LogicalRouterStaticRoute {
	return &nbdb.LogicalRouterStaticRoute{
		UUID:       uuid,
		IPPrefix:   ipPrefix,
		OutputPort: outputPort,
		Nexthop:    nextHop,
		Policy:     policy,
		ExternalIDs: map[string]string{
			types.NetworkExternalID:  "isolatednet",
			types.TopologyExternalID: netInfo.TopologyType(),
		},
	}
}

func allowAllFromMgmtPort(aclUUID string, mgmtPortIP string, switchName string) *nbdb.ACL {
	meterName := "acl-logging"
	return &nbdb.ACL{
		UUID:      aclUUID,
		Action:    "allow-related",
		Direction: "to-lport",
		ExternalIDs: map[string]string{
			"k8s.ovn.org/name":             switchName,
			"ip":                           mgmtPortIP,
			"k8s.ovn.org/id":               fmt.Sprintf("isolatednet-network-controller:NetpolNode:%s:%s", switchName, mgmtPortIP),
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

func newMasqueradeManagementNATEntry(uuid string, netInfo util.NetInfo) *nbdb.NAT {
	masqSNAT := newNATEntry(
		uuid,
		"169.254.169.14",
		layer2Subnet().String(),
		standardNonDefaultNetworkExtIDs(netInfo),
		getMasqueradeManagementIPSNATMatch(util.IPAddrToHWAddr(managementPortIP(layer2Subnet())).String()),
	)
	masqSNAT.LogicalPort = ptr.To(fmt.Sprintf("rtoj-GR_%s_%s", netInfo.GetNetworkName(), nodeName))
	return masqSNAT
}

func newNATEntry(uuid string, externalIP string, logicalIP string, extIDs map[string]string, match string) *nbdb.NAT {
	return &nbdb.NAT{
		UUID:        uuid,
		ExternalIP:  externalIP,
		LogicalIP:   logicalIP,
		Match:       match,
		Type:        "snat",
		Options:     map[string]string{"stateless": "false"},
		ExternalIDs: extIDs,
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
			ExternalIDs: standardNonDefaultNetworkExtIDsForLogicalSwitch(netInfo),
			Ports:       []string{port1UUID, port2UUID},
		},
		&nbdb.LogicalSwitchPort{
			UUID:        port1UUID,
			Name:        netInfo.GetNetworkScopedExtPortName(gwConfig.BridgeID, nodeName),
			Addresses:   []string{"unknown"},
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
			Options:     map[string]string{"network_name": "physnet"},
			Type:        types.LocalnetTopology,
		},
		&nbdb.LogicalSwitchPort{
			UUID:        port2UUID,
			Name:        types.EXTSwitchToGWRouterPrefix + gwRouterName,
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
		"router-port":               types.GWRouterToExtSwitchPrefix + gatewayRouterName,
	}
}

func expectedJoinSwitchAndLSPs(netInfo util.NetInfo, nodeName string) []libovsdbtest.TestData {
	const joinToGRLSPUUID = "port3-UUID"
	gwRouterName := netInfo.GetNetworkScopedGWRouterName(nodeName)
	expectedData := []libovsdbtest.TestData{
		&nbdb.LogicalSwitch{
			UUID:        "join-UUID",
			Name:        netInfo.GetNetworkScopedJoinSwitchName(),
			Ports:       []string{joinToGRLSPUUID},
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
		},
		&nbdb.LogicalSwitchPort{
			UUID:        joinToGRLSPUUID,
			Name:        types.JoinSwitchToGWRouterPrefix + gwRouterName,
			Addresses:   []string{"router"},
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
			Options:     map[string]string{"router-port": types.GWRouterToJoinSwitchPrefix + gwRouterName},
			Type:        "router",
		},
	}
	return expectedData
}

func nextHopMasqueradeIP() net.IP {
	return net.ParseIP("169.254.169.4")
}

func staticMACBindingIPs() []net.IP {
	return []net.IP{net.ParseIP("169.254.169.4"), net.ParseIP("169.254.169.2")}
}

func gwRouterJoinIPAddress() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("100.65.0.4"),
		Mask: net.CIDRMask(16, 32),
	}
}

func gwRouterOptions(gwConfig util.L3GatewayConfig) map[string]string {
	return map[string]string{
		"lb_force_snat_ip":              "router_ip",
		"mac_binding_age_threshold":     "300",
		"chassis":                       gwConfig.ChassisID,
		"always_learn_from_arp_request": "false",
		"dynamic_neigh_routers":         "true",
	}
}

func standardNonDefaultNetworkExtIDs(netInfo util.NetInfo) map[string]string {
	return map[string]string{
		types.TopologyExternalID: netInfo.TopologyType(),
		types.NetworkExternalID:  netInfo.GetNetworkName(),
	}
}

func standardNonDefaultNetworkExtIDsForLogicalSwitch(netInfo util.NetInfo) map[string]string {
	externalIDs := standardNonDefaultNetworkExtIDs(netInfo)
	externalIDs[types.NetworkRoleExternalID] = getNetworkRole(netInfo)
	return externalIDs
}

func newSecondaryLayer3NetworkController(
	cnci *CommonNetworkControllerInfo,
	netInfo util.NetInfo,
	nodeName string,
	networkManager networkmanager.Interface,
	eIPController *EgressIPController,
	portCache *PortCache,
) *SecondaryLayer3NetworkController {
	layer3NetworkController, err := NewSecondaryLayer3NetworkController(cnci, netInfo, networkManager, eIPController, portCache)
	Expect(err).NotTo(HaveOccurred())
	layer3NetworkController.gatewayManagers.Store(
		nodeName,
		newDummyGatewayManager(cnci.kube, cnci.nbClient, netInfo, cnci.watchFactory, nodeName),
	)
	return layer3NetworkController
}

func buildNamespacedPortGroup(namespace, controller string) *nbdb.PortGroup {
	pgIDs := getNamespacePortGroupDbIDs(namespace, controller)
	pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
	pg.UUID = pg.Name + "-UUID"
	return pg
}

func getNetworkPolicyPortGroupDbIDs(namespace, controllerName, name string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetworkPolicy, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: libovsdbops.BuildNamespaceNameKey(namespace, name),
		})
}

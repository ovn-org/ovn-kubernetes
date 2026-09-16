// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	knet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	zoneic "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type userDefinedNetInfo struct {
	netName            string
	nadName            string
	clustersubnets     string
	hostsubnets        string // not used in layer2 tests
	topology           string
	isPrimary          bool
	allowPersistentIPs bool
	ipamClaimReference string
	hasEVPN            bool
	transport          string // e.g., types.NetworkTransportNoOverlay
	outboundSNAT       string // e.g., types.NoOverlaySNATEnabled
}

const (
	nadName                = "blue-net"
	ns                     = "namespace1"
	userDefinedNetworkName = "isolatednet"
	userDefinedNetworkID   = "2"
	denyPolicyName         = "deny-all-policy"
	denyPG                 = "deny-port-group"
)

type testConfiguration struct {
	configToOverride   *config.OVNKubernetesFeatureConfig
	gatewayConfig      *config.GatewayConfig
	withRemoteNode     bool
	withRemotePod      bool
	expectationOptions []option
}

var _ = Describe("OVN Multi-Homed pod operations for layer 3 network", func() {
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

		useFakeAddressSets := false
		fakeOvn = NewFakeOVN(useFakeAddressSets, nodeName)
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
		func(netInfo userDefinedNetInfo, testConfig testConfiguration, gwMode config.GatewayMode) {
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
			app.Action = func(*cli.Context) error {
				nad, err := newNetworkAttachmentDefinition(
					ns,
					nadName,
					*netInfo.netconf(),
				)
				Expect(err).NotTo(HaveOccurred())
				if netInfo.isPrimary && config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
					nad.OwnerReferences = []metav1.OwnerReference{makeCUDNOwnerRef("dynamic-cudn")}
				}

				n := testing.NewNamespace(ns)
				if netInfo.isPrimary {
					n = newUDNNamespace(ns)
				}

				const nodeIPv4CIDR = "192.168.126.202/24"
				testNode, err := newNodeWithUserDefinedNetworks(nodeName, nodeIPv4CIDR, netInfo)
				Expect(err).NotTo(HaveOccurred())
				const netpolPodLabel = "pod-role"
				const netpolPodLabelValue = "server"
				const netpolPodUpdatedLabelValue = "client"
				pod := newMultiHomedPod(podInfo, netInfo)
				pod.Labels[netpolPodLabel] = netpolPodLabelValue
				networkPolicy := &networkingv1.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      denyPolicyName,
						Namespace: ns,
					},
					Spec: networkingv1.NetworkPolicySpec{
						PodSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								netpolPodLabel: netpolPodLabelValue,
							},
						},
						PolicyTypes: []networkingv1.PolicyType{
							networkingv1.PolicyTypeIngress,
						},
					},
				}
				nodes := []corev1.Node{*testNode}
				if testConfig.withRemoteNode {
					By("adding a remote node")
					testNode2, err := newNodeWithUserDefinedNetworks("test-node2", "192.168.127.202/24", netInfo)
					Expect(err).NotTo(HaveOccurred())
					nodes = append(nodes, *testNode2)
				}

				var expectedNBData []libovsdbtest.TestData
				if netInfo.hasEVPN {
					// emulate address sets handled by other controllers, needed for EVPN SNATs
					nodeIPsAS4, _, err := buildClusterNodeIPsAddressSetsForNodes(nodes)
					Expect(err).NotTo(HaveOccurred())
					udnEnabledSvcAS4, _ := buildUDNEnabledSvcAddressSets(nil)
					initialDB.NBData = append(
						initialDB.NBData,
						nodeIPsAS4,
						udnEnabledSvcAS4,
					)
					expectedNBData = append(expectedNBData, nodeIPsAS4, udnEnabledSvcAS4)
				}

				fakeOvn.startWithDBSetup(
					initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							*n,
						},
					},
					&corev1.NodeList{
						Items: nodes,
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*pod,
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
				initialPod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.Background(), podInfo.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, ok := initialPod.Annotations[types.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				// succeed the check for Load_Balancer_Group support
				fexec := testing.NewFakeExec()
				fexec.AddFakeCmdsNoOutputNoError([]string{"ovn-nbctl --timeout=15 --columns=_uuid list Load_Balancer_Group"})
				Expect(util.SetExec(fexec)).To(Succeed())

				fullL3UDNController := fakeOvn.fullL3UDNControllers[userDefinedNetworkName]
				Expect(fullL3UDNController).ToNot(BeNil())
				Expect(fullL3UDNController.init()).To(Succeed())

				Expect(fakeOvn.controller.WatchNamespaces()).NotTo(HaveOccurred())
				Expect(fakeOvn.controller.WatchPods()).NotTo(HaveOccurred())

				userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
				Expect(ok).To(BeTrue())

				userDefinedNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
				podInfo.populateUserDefinedNetworkLogicalSwitchCache(userDefinedNetController)
				Expect(fakeOvn.registerUDNNodeHandler(userDefinedNetworkName)).To(Succeed())
				Expect(userDefinedNetController.bnc.WatchNamespaces()).To(Succeed())
				Expect(fullL3UDNController.waitForLocalZoneNodeLogicalSwitches()).To(Succeed())
				Expect(userDefinedNetController.bnc.WatchPods()).To(Succeed())

				if netInfo.isPrimary {
					Expect(userDefinedNetController.bnc.WatchNetworkPolicy()).To(Succeed())
					Eventually(func() string {
						ninfo, err := fakeOvn.networkManager.Interface().GetActiveNetworkForNamespace(ns)
						if err != nil {
							return ""
						}
						if ninfo == nil {
							return ""
						}
						return ninfo.GetNetworkName()
					}).WithTimeout(3 * time.Second).To(Equal(netInfo.netName))
				}

				// check that after start networks annotations and nbdb will be updated
				Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, podInfo.namespace, podInfo.podName)
				}).WithTimeout(2 * time.Second).Should(MatchJSON(podInfo.getAnnotationsJson()))

				defaultNetExpectations := getDefaultNetExpectedPodsAndSwitches([]testPod{podInfo}, []string{nodeName})
				expectationOptions := testConfig.expectationOptions
				gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
				expectationOptions = append(expectationOptions, withGatewayConfig(gwConfig))
				if netInfo.isPrimary {
					defaultNetExpectations = emptyDefaultClusterNetworkNodeSwitch(podInfo.nodeName)
					Expect(err).NotTo(HaveOccurred())
					Expect(gwConfig.NextHops).NotTo(BeEmpty())
					if testConfig.configToOverride != nil && testConfig.configToOverride.EnableEgressFirewall {
						defaultNetExpectations = append(defaultNetExpectations,
							buildNamespacedPortGroup(podInfo.namespace, types.DefaultNetworkControllerName))
						secNetPG := buildNamespacedPortGroup(podInfo.namespace, userDefinedNetController.bnc.controllerName)
						portName := util.GetUserDefinedNetworkLogicalPortName(podInfo.namespace, podInfo.podName, netInfo.nadName) + "-UUID"
						secNetPG.Ports = []string{portName}
						defaultNetExpectations = append(defaultNetExpectations, secNetPG)
					}
					networkConfig, err := util.NewNetInfo(netInfo.netconf())
					Expect(err).NotTo(HaveOccurred())
					// Add NetPol hairpin ACLs and PGs for the validation.
					mgmtPortName := managementPortName(userDefinedNetController.bnc.GetNetworkScopedName(nodeName))
					mgmtPortUUID := mgmtPortName + "-UUID"
					defaultNetExpectations = append(defaultNetExpectations, getHairpinningACLsV4AndPortGroupForNetwork(networkConfig,
						[]string{mgmtPortUUID})...)
					// Add Netpol deny policy ACLs and PGs for the validation.
					podLPortName := util.GetUserDefinedNetworkLogicalPortName(podInfo.namespace, podInfo.podName, netInfo.nadName) + "-UUID"
					dataParams := newNetpolDataParams(networkPolicy).withLocalPortUUIDs(podLPortName).withNetInfo(networkConfig)
					defaultDenyExpectedData := getDefaultDenyData(dataParams)
					pgDbIDs := getNetworkPolicyPortGroupDbIDs(ns, userDefinedNetController.bnc.controllerName, denyPolicyName)
					ingressPG := libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil)
					ingressPG.UUID = denyPG
					ingressPG.Ports = []string{podLPortName}
					defaultNetExpectations = append(defaultNetExpectations, ingressPG)
					defaultNetExpectations = append(defaultNetExpectations, defaultDenyExpectedData...)
				}
				By("asserting the OVN entities provisioned in the NBDB are the expected ones")
				expectedNBData = append(expectedNBData, defaultNetExpectations...)
				expectedNBData = append(
					expectedNBData,
					newUserDefinedNetworkExpectationMachine(fakeOvn, []testPod{podInfo}, expectationOptions...).expectedLogicalSwitchesAndPorts(nodeName)...,
				)
				Eventually(fakeOvn.nbClient).WithTimeout(5 * time.Second).Should(libovsdbtest.HaveData(expectedNBData))
				if netInfo.isPrimary {
					By("reconciling UDN network policy membership on a label-only pod update")
					updatedPod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.Background(), podInfo.podName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					updatedPod.Labels[netpolPodLabel] = netpolPodUpdatedLabelValue
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Update(context.Background(), updatedPod, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					networkPolicyPGName := libovsdbutil.GetPortGroupName(getNetworkPolicyPortGroupDbIDs(ns, userDefinedNetController.bnc.controllerName, denyPolicyName))
					ingressDefaultDenyPGName := userDefinedNetController.bnc.defaultDenyPortGroupName(ns, libovsdbutil.ACLIngress)
					Eventually(func(g Gomega) {
						networkPolicyPG, err := libovsdbops.GetPortGroup(fakeOvn.nbClient, &nbdb.PortGroup{Name: networkPolicyPGName})
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(networkPolicyPG.Ports).To(BeEmpty())

						ingressDefaultDenyPG, err := libovsdbops.GetPortGroup(fakeOvn.nbClient, &nbdb.PortGroup{Name: ingressDefaultDenyPGName})
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(ingressDefaultDenyPG.Ports).To(BeEmpty())
					}).WithTimeout(5 * time.Second).Should(Succeed())
				}

				return nil
			}

			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
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
		Entry("with dynamic UDN allocation, a remote node with no NAD is ignored",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(func(config *testConfiguration) {
				config.configToOverride.EnableDynamicUDNAllocation = true
				config.configToOverride.EnableNetworkSegmentation = true
				config.withRemoteNode = true
			}),
			config.GatewayModeShared,
		),
		Entry("pod on a CUDN configured with EVPN",
			dummyPrimaryLayer3EVPNCUDN("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(func(config *testConfiguration) {
				config.withRemoteNode = true
			}),
			config.GatewayModeLocal,
		),
	)

	It("deletes OVN pod state on a cache miss when the OVN pod network annotation is missing", func() {
		app.Action = func(*cli.Context) error {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			config.Gateway.Mode = config.GatewayModeShared
			netInfo := dummySecondaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24")
			podInfo := dummyTestPod(ns, netInfo)
			netConf := netInfo.netconf()
			networkConfig, err := util.NewNetInfo(netConf)
			Expect(err).NotTo(HaveOccurred())
			nad, err := newNetworkAttachmentDefinition(ns, nadName, *netConf)
			Expect(err).NotTo(HaveOccurred())
			testNode, err := newNodeWithUserDefinedNetworks(nodeName, "192.168.126.202/24", netInfo)
			Expect(err).NotTo(HaveOccurred())

			initialPod := newMultiHomedPod(podInfo, netInfo)
			delete(initialPod.Annotations, types.OvnPodAnnotationName)

			udnPortInfo := podInfo.udnPodInfos[netInfo.netName].allportInfo[netInfo.nadName]
			ovnPodInfo := podInfo
			ovnPodInfo.portUUID = udnPortInfo.portUUID
			ovnPodInfo.podIP = udnPortInfo.podIP
			ovnPodInfo.podMAC = udnPortInfo.podMAC
			initialNBData := append([]libovsdbtest.TestData{}, initialDB.NBData...)
			initialNBData = append(initialNBData, getExpectedPodsAndSwitches(networkConfig, []testPod{ovnPodInfo}, []string{nodeName}, netInfo.nadName)...)
			portName := util.GetUserDefinedNetworkLogicalPortName(podInfo.namespace, podInfo.podName, netInfo.nadName)
			portUUID := portName + "-UUID"
			nsPG := buildNamespacedPortGroup(podInfo.namespace, getNetworkControllerName(netInfo.netName))
			nsPG.Ports = []string{portUUID}
			initialNBData = append(initialNBData, nsPG)

			fakeOvn.startWithDBSetup(
				libovsdbtest.TestSetup{NBData: initialNBData},
				&corev1.NamespaceList{Items: []corev1.Namespace{*testing.NewNamespace(ns)}},
				&corev1.NodeList{Items: []corev1.Node{*testNode}},
				&corev1.PodList{Items: []corev1.Pod{*initialPod}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)

			Expect(fakeOvn.NewUserDefinedNetworkController(nad)).To(Succeed())
			userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
			Expect(ok).To(BeTrue())
			podInfo.populateUserDefinedNetworkLogicalSwitchCache(userDefinedNetController)

			Expect(userDefinedNetController.bnc.getPortInfoForUserDefinedNetwork(initialPod)).To(BeNil())

			userDefinedNetController.bnc.networkManager = networkmanager.Default().Interface()
			Expect(userDefinedNetController.bnc.removePodForUserDefinedNetwork(initialPod, nil)).To(Succeed())

			_, err = libovsdbops.GetLogicalSwitchPort(fakeOvn.nbClient, &nbdb.LogicalSwitchPort{Name: portName})
			Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())

			pg, err := libovsdbops.GetPortGroup(fakeOvn.nbClient, &nbdb.PortGroup{Name: nsPG.Name})
			Expect(err).NotTo(HaveOccurred())
			Expect(pg.Ports).To(BeEmpty())

			return nil
		}

		Expect(app.Run([]string{app.Name})).To(Succeed())
	})

	It("deletes OVN pod state on a cache miss when the OVN pod network annotation is malformed", func() {
		app.Action = func(*cli.Context) error {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			config.Gateway.Mode = config.GatewayModeShared

			netInfo := dummySecondaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24")
			podInfo := dummyTestPod(ns, netInfo)
			netConf := netInfo.netconf()
			networkConfig, err := util.NewNetInfo(netConf)
			Expect(err).NotTo(HaveOccurred())
			nad, err := newNetworkAttachmentDefinition(ns, nadName, *netConf)
			Expect(err).NotTo(HaveOccurred())
			testNode, err := newNodeWithUserDefinedNetworks(nodeName, "192.168.126.202/24", netInfo)
			Expect(err).NotTo(HaveOccurred())

			initialPod := newMultiHomedPod(podInfo, netInfo)
			// Malformed delete annotations must not block applied-state teardown.
			initialPod.Annotations[types.OvnPodAnnotationName] = "not-a-json-annotation"

			udnPortInfo := podInfo.udnPodInfos[netInfo.netName].allportInfo[netInfo.nadName]
			ovnPodInfo := podInfo
			ovnPodInfo.portUUID = udnPortInfo.portUUID
			ovnPodInfo.podIP = udnPortInfo.podIP
			ovnPodInfo.podMAC = udnPortInfo.podMAC
			initialNBData := append([]libovsdbtest.TestData{}, initialDB.NBData...)
			initialNBData = append(initialNBData, getExpectedPodsAndSwitches(networkConfig, []testPod{ovnPodInfo}, []string{nodeName}, netInfo.nadName)...)
			portName := util.GetUserDefinedNetworkLogicalPortName(podInfo.namespace, podInfo.podName, netInfo.nadName)
			portUUID := portName + "-UUID"
			nsPG := buildNamespacedPortGroup(podInfo.namespace, getNetworkControllerName(netInfo.netName))
			nsPG.Ports = []string{portUUID}
			initialNBData = append(initialNBData, nsPG)

			fakeOvn.startWithDBSetup(
				libovsdbtest.TestSetup{NBData: initialNBData},
				&corev1.NamespaceList{Items: []corev1.Namespace{*testing.NewNamespace(ns)}},
				&corev1.NodeList{Items: []corev1.Node{*testNode}},
				&corev1.PodList{Items: []corev1.Pod{*initialPod}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)

			Expect(fakeOvn.NewUserDefinedNetworkController(nad)).To(Succeed())
			userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
			Expect(ok).To(BeTrue())
			podInfo.populateUserDefinedNetworkLogicalSwitchCache(userDefinedNetController)

			Expect(userDefinedNetController.bnc.getPortInfoForUserDefinedNetwork(initialPod)).To(BeNil())

			userDefinedNetController.bnc.networkManager = networkmanager.Default().Interface()
			Expect(userDefinedNetController.bnc.removePodForUserDefinedNetwork(initialPod, nil)).To(Succeed())

			_, err = libovsdbops.GetLogicalSwitchPort(fakeOvn.nbClient, &nbdb.LogicalSwitchPort{Name: portName})
			Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())

			pg, err := libovsdbops.GetPortGroup(fakeOvn.nbClient, &nbdb.PortGroup{Name: nsPG.Name})
			Expect(err).NotTo(HaveOccurred())
			Expect(pg.Ports).To(BeEmpty())

			return nil
		}

		Expect(app.Run([]string{app.Name})).To(Succeed())
	})

	DescribeTable(
		"the gateway is properly cleaned up",
		func(netInfo userDefinedNetInfo, testConfig testConfiguration) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			podInfo := dummyTestPod(ns, netInfo)
			if testConfig.configToOverride != nil {
				config.OVNKubernetesFeature = *testConfig.configToOverride
				if testConfig.gatewayConfig != nil {
					config.Gateway.DisableSNATMultipleGWs = testConfig.gatewayConfig.DisableSNATMultipleGWs
				}
			}
			app.Action = func(_ *cli.Context) error {
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

				fakeNetworkManager := &networkmanager.FakeNetworkManager{
					PrimaryNetworks: make(map[string]util.NetInfo),
				}
				fakeNetworkManager.PrimaryNetworks[ns] = networkConfig

				const nodeIPv4CIDR = "192.168.126.202/24"
				testNode, err := newNodeWithUserDefinedNetworks(nodeName, nodeIPv4CIDR, netInfo)
				Expect(err).NotTo(HaveOccurred())

				defaultNetExpectations := emptyDefaultClusterNetworkNodeSwitch(podInfo.nodeName)
				defaultNetExpectations = append(defaultNetExpectations, generateUDNPostInitDB([]libovsdbtest.TestData{})...)
				gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
				Expect(err).NotTo(HaveOccurred())
				Expect(gwConfig.NextHops).NotTo(BeEmpty())

				if netInfo.isPrimary {
					gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
					Expect(err).NotTo(HaveOccurred())
					initialDB.NBData = append(
						initialDB.NBData,
						expectedGWEntities(podInfo.nodeName, networkConfig, *gwConfig)...)
					initialDB.NBData = append(
						initialDB.NBData,
						expectedLayer3EgressEntities(networkConfig, *gwConfig, testing.MustParseIPNet(netInfo.hostsubnets))...)
					initialDB.NBData = append(initialDB.NBData,
						newNetworkClusterPortGroup(networkConfig),
						newLoadBalancerGroup(networkConfig.GetNetworkScopedLoadBalancerGroupName(types.ClusterLBGroupName)),
						newLoadBalancerGroup(networkConfig.GetNetworkScopedLoadBalancerGroupName(types.ClusterSwitchLBGroupName)),
						newLoadBalancerGroup(networkConfig.GetNetworkScopedLoadBalancerGroupName(types.ClusterRouterLBGroupName)),
					)
					if testConfig.configToOverride != nil && testConfig.configToOverride.EnableEgressFirewall {
						defaultNetExpectations = append(defaultNetExpectations,
							buildNamespacedPortGroup(podInfo.namespace, types.DefaultNetworkControllerName))
					}
				}

				fakeOvn.startWithDBSetup(
					initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							*newUDNNamespace(ns),
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{*testNode},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*newMultiHomedPod(podInfo, netInfo),
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
				_, ok := pod.Annotations[types.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				// succeed the check for Load_Balancer_Group support
				fexec := testing.NewFakeExec()
				fexec.AddFakeCmdsNoOutputNoError([]string{"ovn-nbctl --timeout=15 --columns=_uuid list Load_Balancer_Group"})
				Expect(util.SetExec(fexec)).To(Succeed())

				fullL3UDNController := fakeOvn.fullL3UDNControllers[userDefinedNetworkName]
				Expect(fullL3UDNController).ToNot(BeNil())
				Expect(fullL3UDNController.init()).To(Succeed())

				Expect(fakeOvn.controller.WatchNamespaces()).To(Succeed())
				Expect(fakeOvn.controller.WatchPods()).To(Succeed())
				userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
				Expect(ok).To(BeTrue())

				userDefinedNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
				podInfo.populateUserDefinedNetworkLogicalSwitchCache(userDefinedNetController)
				Expect(fakeOvn.registerUDNNodeHandler(userDefinedNetworkName)).To(Succeed())
				Expect(userDefinedNetController.bnc.WatchNamespaces()).To(Succeed())
				Expect(fullL3UDNController.waitForLocalZoneNodeLogicalSwitches()).To(Succeed())
				Expect(userDefinedNetController.bnc.WatchPods()).To(Succeed())

				if netInfo.isPrimary {
					Expect(userDefinedNetController.bnc.WatchNetworkPolicy()).To(Succeed())
				}

				// we must access the layer3 controller to be able to issue its cleanup function (to remove the GW related stuff).
				Eventually(func() bool {
					_, exists := fullL3UDNController.gatewayManagers.Load(nodeName)
					return exists
				}).Should(BeTrue())
				// Avoid node reconcile while pod/NAD deletes and cleanup are
				// tearing down network entities.
				fullL3UDNController.DeregisterNodeHandler()
				Expect(fakeOvn.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
				Expect(fakeOvn.fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Delete(context.Background(), nad.Name, metav1.DeleteOptions{})).To(Succeed())
				Expect(fullL3UDNController.Cleanup()).To(Succeed())
				Eventually(fakeOvn.nbClient).WithTimeout(5 * time.Second).Should(libovsdbtest.HaveData(defaultNetExpectations))

				return nil
			}
			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
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

	It("primary Layer 3 UDN: controller creates entities via init/watchers, then dummy Cleanup() removes them", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		netInfo := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24")
		app.Action = func(ctx *cli.Context) error {
			netConf := netInfo.netconf()
			networkConfig, err := util.NewNetInfo(netConf)
			Expect(err).NotTo(HaveOccurred())
			// For cleanup we use a copy with InvalidID so the dummy controller treats the network as stale.
			mutableNetInfoCleanup := util.NewMutableNetInfo(networkConfig)
			mutableNetInfoCleanup.SetNetworkID(types.InvalidID)

			nad, err := newNetworkAttachmentDefinition(ns, nadName, *netConf)
			Expect(err).NotTo(HaveOccurred())
			// Dummy controller only runs Cleanup(), which does not use the network manager; empty fake is enough.
			fakeNetworkManager := &networkmanager.FakeNetworkManager{
				PrimaryNetworks: make(map[string]util.NetInfo),
			}

			const nodeIPv4CIDR = "192.168.126.202/24"
			testNode, err := newNodeWithUserDefinedNetworks(nodeName, nodeIPv4CIDR, netInfo)
			Expect(err).NotTo(HaveOccurred())

			// Post-cleanup DB: default net node switch + global entities (Copp, meters) as in Layer2 test.
			defaultNetExpectations := generateUDNPostInitDB(emptyDefaultClusterNetworkNodeSwitch(nodeName))

			// Minimal initialDB: default net node switch, no UDN entities. The UDN controller's Start()
			// runs init() which creates cluster router and join switch; then node sync creates per-node entities.
			fakeOvn.startWithDBSetup(
				initialDB,
				&corev1.NamespaceList{Items: []corev1.Namespace{*newUDNNamespace(ns)}},
				&corev1.NodeList{Items: []corev1.Node{*testNode}},
				&corev1.PodList{Items: []corev1.Pod{}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)

			// Mock ovn-nbctl list Load_Balancer_Group (used by UDN controller init; default controller init is not run in this test).
			fexec := util.GetExec().(*testing.FakeExec)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --columns=_uuid list Load_Balancer_Group",
			})

			// networkManager is already started by startWithDBSetup (via init()) and stopped by AfterEach (shutdown).
			Expect(fakeOvn.controller.WatchNamespaces()).To(Succeed())
			Expect(fakeOvn.controller.WatchPods()).To(Succeed())

			// Run init() to create cluster-level entities (cluster router, join switch, LB groups, etc.),
			// then start watchers so node sync creates per-node entities (node LS, GW router, etc.).
			l3Controller, ok := fakeOvn.fullL3UDNControllers[userDefinedNetworkName]
			Expect(ok).To(BeTrue())
			Expect(l3Controller.init()).To(Succeed())
			Expect(l3Controller.RegisterNodeHandler()).To(Succeed())
			Expect(l3Controller.WatchNamespaces()).To(Succeed())
			Expect(l3Controller.waitForLocalZoneNodeLogicalSwitches()).To(Succeed())
			Expect(l3Controller.WatchPods()).To(Succeed())
			Expect(l3Controller.WatchNetworkPolicy()).To(Succeed())

			// Wait for the controller to create UDN entities: assert any switches and routers exist with this network's external-ids,
			// and that the gateway router for this node exists.
			networkName := networkConfig.GetNetworkName()
			gwRouterName := networkConfig.GetNetworkScopedGWRouterName(nodeName)
			Eventually(func(g Gomega) {
				switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(fakeOvn.nbClient, func(ls *nbdb.LogicalSwitch) bool {
					return ls.ExternalIDs != nil && ls.ExternalIDs[types.NetworkExternalID] == networkName
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(switches).NotTo(BeEmpty(), "at least one LogicalSwitch for network %q should exist", networkName)
			}).WithTimeout(10 * time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				routers, err := libovsdbops.FindLogicalRoutersWithPredicate(fakeOvn.nbClient, func(lr *nbdb.LogicalRouter) bool {
					return lr.ExternalIDs != nil && lr.ExternalIDs[types.NetworkExternalID] == networkName
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(routers).NotTo(BeEmpty(), "at least one LogicalRouter for network %q should exist", networkName)
			}).WithTimeout(10 * time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				routers, err := libovsdbops.FindLogicalRoutersWithPredicate(fakeOvn.nbClient, func(lr *nbdb.LogicalRouter) bool {
					return lr.Name == gwRouterName
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(routers).NotTo(BeEmpty(), "gateway router %q should exist", gwRouterName)
			}).WithTimeout(10 * time.Second).Should(Succeed())

			// Deregister the active controller's node handler before stale cleanup
			// to avoid concurrent node reconciliation re-creating entities while cleanup runs.
			l3Controller.DeregisterNodeHandler()

			// Do NOT delete the NAD. Simulate CleanupStaleNetworks(no valid networks): dummy controller
			// with InvalidID runs Cleanup() so our network is treated as stale and all its entities are removed.
			dummyController, err := NewLayer3UserDefinedNetworkController(
				&l3Controller.CommonNetworkControllerInfo,
				mutableNetInfoCleanup,
				fakeNetworkManager,
				nil,
				nil,
				NewPortCache(ctx.Done()),
				nil,
				nil,
				nil,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(dummyController.Cleanup()).To(Succeed())

			Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(defaultNetExpectations))
			return nil
		}
		Expect(app.Run([]string{app.Name})).To(Succeed())
	})

	Describe("Dynamic UDN allocation with remote node", func() {
		It("activates a remote node when a NAD becomes active and cleans it up when inactive", func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"

			// Basic UDN setup
			netInfo := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24")
			n := newUDNNamespace(ns)
			nad, err := newNetworkAttachmentDefinition(ns, nadName, *netInfo.netconf())
			Expect(err).NotTo(HaveOccurred())
			nad.OwnerReferences = []metav1.OwnerReference{makeCUDNOwnerRef("dynamic-cudn")}

			// Local node and remote node with NAD
			localNode, err := newNodeWithUserDefinedNetworks(nodeName, "192.168.126.202/24", netInfo)
			Expect(err).NotTo(HaveOccurred())
			localNode.Annotations[util.OvnTransitSwitchPortAddr] = `{"ipv4":"100.88.0.3/16"}`

			remoteNode, err := newNodeWithUserDefinedNetworks("remoteNode", "192.168.127.202/24", netInfo)
			Expect(err).NotTo(HaveOccurred())
			remoteNode.Annotations[util.OvnTransitSwitchPortAddr] = `{"ipv4":"100.88.0.4/16"}`

			remotePod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "remote-pod",
					Namespace: ns,
				},
				Spec: corev1.PodSpec{
					NodeName:   remoteNode.Name,
					Containers: []corev1.Container{{Name: "c", Image: "scratch"}},
				},
			}

			localPod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-pod",
					Namespace: ns,
				},
				Spec: corev1.PodSpec{
					NodeName:   localNode.Name,
					Containers: []corev1.Container{{Name: "c", Image: "scratch"}},
				},
			}

			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{}, &corev1.NamespaceList{Items: []corev1.Namespace{*n}},
				&corev1.NodeList{Items: []corev1.Node{*localNode, *remoteNode}},
				&corev1.PodList{Items: []corev1.Pod{localPod}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}})

			userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
			Expect(ok).To(BeTrue())
			userDefinedNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
			l3Controller, ok := fakeOvn.fullL3UDNControllers[netInfo.netName]
			Expect(ok).To(BeTrue())
			mutableNetInfo := util.NewMutableNetInfo(l3Controller.GetNetInfo())
			mutableNetInfo.SetNetworkID(2)
			err = util.ReconcileNetInfo(l3Controller.ReconcilableNetInfo, mutableNetInfo)
			Expect(err).NotTo(HaveOccurred())
			err = l3Controller.init()
			Expect(err).NotTo(HaveOccurred())
			Expect(fakeOvn.registerUDNNodeHandler(netInfo.netName)).To(Succeed())

			By("Remote node should not have a port on transit subnet before activation")
			Consistently(func() bool {
				p := func(item *nbdb.LogicalSwitchPort) bool {
					return item.ExternalIDs["node"] == remoteNode.Name
				}
				portList, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOvn.nbClient, p)
				return err == nil && len(portList) > 0
			}).WithTimeout(500 * time.Millisecond).Should(BeFalse())

			By("Creating a pod on the remote node should activate it")
			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(ns).Create(context.TODO(), &remotePod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				return fakeOvn.networkManager.Interface().NodeHasNetwork(remoteNode.Name, netInfo.netName)
			}).WithTimeout(3 * time.Second).Should(BeTrue())
			By("Triggering networkRefChange callback after updating remote node as active on NAD")
			l3Controller.HandleNetworkRefChange(remoteNode.Name, true)

			By("Remote node should have a port created on transit subnet")
			Eventually(func() bool {
				p := func(item *nbdb.LogicalSwitchPort) bool {
					return item.ExternalIDs["node"] == remoteNode.Name
				}
				portList, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOvn.nbClient, p)
				if err == nil && len(portList) > 0 {
					return true
				}
				return false
			}).Should(BeTrue())

			By("Deleting a pod on the remote node should set it as inactive")
			err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(ns).Delete(context.TODO(), remotePod.Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				return fakeOvn.networkManager.Interface().NodeHasNetwork(remoteNode.Name, netInfo.netName)
			}).WithTimeout(3 * time.Second).Should(BeFalse())
			By("Triggering networkRefChange callback after updating remote node as inactive on NAD")
			l3Controller.HandleNetworkRefChange(remoteNode.Name, false)
			By("Remote node should not have a port on transit subnet")
			Eventually(func() bool {
				p := func(item *nbdb.LogicalSwitchPort) bool {
					return item.ExternalIDs["node"] == remoteNode.Name
				}
				portList, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOvn.nbClient, p)
				if err == nil && len(portList) > 0 {
					return true
				}
				return false
			}).WithTimeout(3 * time.Second).Should(BeFalse())

			By("Verifying core gateway router and cluster router are intact after remote node removal")
			hasPort := func(ports []string, target string) bool {
				for _, port := range ports {
					if port == target {
						return true
					}
				}
				return false
			}
			hasRouterPorts := func(routerName string, portNames ...string) bool {
				routers, err := libovsdbops.FindLogicalRoutersWithPredicate(fakeOvn.nbClient, func(router *nbdb.LogicalRouter) bool {
					return router.Name == routerName
				})
				if err != nil || len(routers) == 0 {
					return false
				}
				router := routers[0]
				for _, portName := range portNames {
					ports, err := libovsdbops.FindLogicalRouterPortWithPredicate(fakeOvn.nbClient, func(port *nbdb.LogicalRouterPort) bool {
						return port.Name == portName
					})
					if err != nil || len(ports) == 0 {
						return false
					}
					if !hasPort(router.Ports, ports[0].UUID) {
						return false
					}
				}
				return true
			}
			hasSwitchPorts := func(switchName string, portNames ...string) bool {
				switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(fakeOvn.nbClient, func(sw *nbdb.LogicalSwitch) bool {
					return sw.Name == switchName
				})
				if err != nil || len(switches) == 0 {
					return false
				}
				sw := switches[0]
				for _, portName := range portNames {
					ports, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOvn.nbClient, func(port *nbdb.LogicalSwitchPort) bool {
						return port.Name == portName
					})
					if err != nil || len(ports) == 0 {
						return false
					}
					if !hasPort(sw.Ports, ports[0].UUID) {
						return false
					}
				}
				return true
			}
			Eventually(func() bool {
				return hasRouterPorts(
					"GR_isolatednet_test-node",
					"rtoj-GR_isolatednet_test-node",
					"rtoe-GR_isolatednet_test-node",
				) &&
					hasRouterPorts(
						"isolatednet_ovn_cluster_router",
						"rtoj-isolatednet_ovn_cluster_router",
						"rtos-isolatednet_test-node",
						"isolatednet_rtots-test-node",
					) &&
					hasSwitchPorts(
						"isolatednet_join",
						"jtor-GR_isolatednet_test-node",
						"jtor-isolatednet_ovn_cluster_router",
					) &&
					hasSwitchPorts(
						"isolatednet_transit_switch",
						"isolatednet_tstor-test-node",
					)
			}).WithTimeout(3 * time.Second).Should(BeTrue())
		})

		It("activates a remote node when a CUDN NAD becomes active in another namespace", func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"

			const (
				cudnName   = "cudn-shared"
				nsA        = "namespace-a"
				nsB        = "namespace-b"
				nadAName   = "cudn-nad-a"
				nadBName   = "cudn-nad-b"
				remoteName = "remoteNode"
			)

			netName := util.GenerateCUDNNetworkName(cudnName)
			netInfoA := userDefinedNetInfo{
				netName:        netName,
				nadName:        namespacedName(nsA, nadAName),
				topology:       types.Layer3Topology,
				clustersubnets: "192.168.0.0/16",
				hostsubnets:    "192.168.1.0/24",
				isPrimary:      true,
			}
			netInfoB := userDefinedNetInfo{
				netName:        netName,
				nadName:        namespacedName(nsB, nadBName),
				topology:       types.Layer3Topology,
				clustersubnets: "192.168.0.0/16",
				hostsubnets:    "192.168.2.0/24",
				isPrimary:      true,
			}

			nsObjA := newUDNNamespace(nsA)
			nsObjB := newUDNNamespace(nsB)
			nadA, err := newNetworkAttachmentDefinition(nsA, nadAName, *netInfoA.netconf())
			Expect(err).NotTo(HaveOccurred())
			nadA.OwnerReferences = []metav1.OwnerReference{makeCUDNOwnerRef(cudnName)}
			nadB, err := newNetworkAttachmentDefinition(nsB, nadBName, *netInfoB.netconf())
			Expect(err).NotTo(HaveOccurred())
			nadB.OwnerReferences = []metav1.OwnerReference{makeCUDNOwnerRef(cudnName)}

			localNode, err := newNodeWithUserDefinedNetworks(nodeName, "192.168.126.202/24", netInfoA)
			Expect(err).NotTo(HaveOccurred())
			localNode.Annotations[util.OvnTransitSwitchPortAddr] = `{"ipv4":"100.88.0.3/16"}`

			remoteNode, err := newNodeWithUserDefinedNetworks(remoteName, "192.168.127.202/24", netInfoB)
			Expect(err).NotTo(HaveOccurred())
			remoteNode.Annotations[util.OvnTransitSwitchPortAddr] = `{"ipv4":"100.88.0.4/16"}`

			localPod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-pod",
					Namespace: nsA,
				},
				Spec: corev1.PodSpec{
					NodeName:   localNode.Name,
					Containers: []corev1.Container{{Name: "c", Image: "scratch"}},
				},
			}

			remotePod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "remote-pod",
					Namespace: nsB,
				},
				Spec: corev1.PodSpec{
					NodeName:   remoteNode.Name,
					Containers: []corev1.Container{{Name: "c", Image: "scratch"}},
				},
			}

			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{}, &corev1.NamespaceList{Items: []corev1.Namespace{*nsObjA, *nsObjB}},
				&corev1.NodeList{Items: []corev1.Node{*localNode, *remoteNode}},
				&corev1.PodList{Items: []corev1.Pod{localPod}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nadA, *nadB}},
			)

			userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[netName]
			Expect(ok).To(BeTrue())
			userDefinedNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
			l3Controller, ok := fakeOvn.fullL3UDNControllers[netName]
			Expect(ok).To(BeTrue())

			mutableNetInfo := util.NewMutableNetInfo(l3Controller.GetNetInfo())
			mutableNetInfo.SetNetworkID(2)
			err = util.ReconcileNetInfo(l3Controller.ReconcilableNetInfo, mutableNetInfo)
			Expect(err).NotTo(HaveOccurred())
			Expect(l3Controller.init()).To(Succeed())
			Expect(fakeOvn.registerUDNNodeHandler(netName)).To(Succeed())

			By("Remote node should not have a port on transit subnet before activation")
			Consistently(func() bool {
				p := func(item *nbdb.LogicalSwitchPort) bool {
					return item.ExternalIDs["node"] == remoteNode.Name
				}
				portList, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOvn.nbClient, p)
				return err == nil && len(portList) > 0
			}).WithTimeout(500 * time.Millisecond).Should(BeFalse())

			By("Creating a pod on the remote node in another namespace should activate it")
			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nsB).Create(context.TODO(), &remotePod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				return fakeOvn.networkManager.Interface().NodeHasNetwork(remoteNode.Name, netName)
			}).WithTimeout(3 * time.Second).Should(BeTrue())
			By("Triggering networkRefChange callback after updating remote node as active on NAD")
			l3Controller.HandleNetworkRefChange(remoteNode.Name, true)

			By("Remote node should have a port created on transit subnet")
			Eventually(func() bool {
				p := func(item *nbdb.LogicalSwitchPort) bool {
					return item.ExternalIDs["node"] == remoteNode.Name
				}
				portList, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOvn.nbClient, p)
				return err == nil && len(portList) > 0
			}).Should(BeTrue())
		})
	})

})

var _ = Describe("Layer3 UDN Transport Mode - Interconnect", func() {
	const (
		node1Name  = "node1"
		node2Name  = "node2"
		udnName    = "test-udn"
		udnNadName = "default/test-nad"
	)

	var (
		app       *cli.App
		initialDB libovsdbtest.TestSetup
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{},
			SBData: []libovsdbtest.TestData{},
		}
	})

	createUDNNetConf := func(transport string) *ovncnitypes.NetConf {
		return &ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				Name: udnName,
				Type: "ovn-k8s-cni-overlay",
			},
			Topology:  types.Layer3Topology,
			NADName:   udnNadName,
			Subnets:   "192.168.0.0/16",
			Role:      types.NetworkRolePrimary,
			Transport: transport,
		}
	}

	createTestNode := func(nodeName, nodeID string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets":                    `{"default":"10.128.1.0/24","` + udnName + `":"192.168.1.0/24"}`,
					"k8s.ovn.org/node-id":                         nodeID,
					"k8s.ovn.org/node-chassis-id":                 "test-chassis-" + nodeName,
					"k8s.ovn.org/node-transit-switch-port-ifaddr": `{"ipv4":"100.88.0.1/16"}`,
				},
			},
		}
	}

	Context("with no-overlay transport", func() {
		It("should not create interconnect resources when addUpdateLocalNodeEvent is called", func() {
			app.Action = func(_ *cli.Context) error {
				netConf := createUDNNetConf(types.NetworkTransportNoOverlay)
				netInfo, err := util.NewNetInfo(netConf)
				Expect(err).NotTo(HaveOccurred())
				Expect(netInfo.Transport()).To(Equal(types.NetworkTransportNoOverlay))

				node1 := createTestNode(node1Name, "1")
				clusterRouterName := netInfo.GetNetworkScopedClusterRouterName()
				transitSwitchName := netInfo.GetNetworkScopedName(types.TransitSwitch)

				// Setup NBDB with cluster router
				initialDB.NBData = []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name: clusterRouterName,
						UUID: clusterRouterName + "-UUID",
						ExternalIDs: map[string]string{
							types.NetworkExternalID:  udnName,
							types.TopologyExternalID: types.Layer3Topology,
						},
					},
					&nbdb.LogicalSwitch{
						Name: netInfo.GetNetworkScopedName(node1Name),
						UUID: netInfo.GetNetworkScopedName(node1Name) + "-UUID",
						ExternalIDs: map[string]string{
							types.NetworkExternalID:     udnName,
							types.NetworkRoleExternalID: types.NetworkRolePrimary,
						},
						OtherConfig: map[string]string{"subnet": "192.168.1.0/24"},
					},
				}

				nbClient, sbClient, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(initialDB)
				Expect(err).NotTo(HaveOccurred())
				defer libovsdbCleanup.Cleanup()

				// Create the controller
				watcher := &factory.WatchFactory{}
				zoneICHandler := zoneic.NewZoneInterconnectHandler(netInfo, nbClient, sbClient, watcher)

				cnci := &CommonNetworkControllerInfo{
					nbClient:     nbClient,
					sbClient:     sbClient,
					watchFactory: watcher,
				}

				controller := &Layer3UserDefinedNetworkController{
					BaseUserDefinedNetworkController: BaseUserDefinedNetworkController{
						BaseNetworkController: BaseNetworkController{
							CommonNetworkControllerInfo: *cnci,
							ReconcilableNetInfo:         util.NewReconcilableNetInfo(netInfo),
							zoneICHandler:               zoneICHandler,
						},
					},
				}

				// Call addUpdateLocalNodeEvent with syncZoneIC=true ONLY
				// By setting only syncZoneIC=true (not syncNode), we skip addNode() and jump
				// straight to the IC check at line 833. This tests the IC check in isolation.
				nSyncs := &nodeSyncs{syncZoneIC: true}
				err = controller.addUpdateLocalNodeEvent(node1, nSyncs)
				Expect(err).NotTo(HaveOccurred(), "addUpdateLocalNodeEvent should succeed when only syncZoneIC is set")

				// Verify no transit switch was created
				ts := &nbdb.LogicalSwitch{Name: transitSwitchName}
				_, err = libovsdbops.GetLogicalSwitch(nbClient, ts)
				Expect(err).To(HaveOccurred(), "Transit switch should not exist for no-overlay transport")

				// Verify no transit switch port was created
				transitSwitchPortName := netInfo.GetNetworkScopedName(types.TransitSwitchToRouterPrefix + node1Name)
				ports, err := libovsdbops.FindLogicalSwitchPortWithPredicate(nbClient, func(lsp *nbdb.LogicalSwitchPort) bool {
					return lsp.Name == transitSwitchPortName
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(ports).To(BeEmpty(), "No transit switch port should exist for no-overlay transport")

				// Verify no router port was created
				routerPortName := netInfo.GetNetworkScopedName(types.RouterToTransitSwitchPrefix + node1Name)
				rports, err := libovsdbops.FindLogicalRouterPortWithPredicate(nbClient, func(lrp *nbdb.LogicalRouterPort) bool {
					return lrp.Name == routerPortName
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(rports).To(BeEmpty(), "No router port should exist for no-overlay transport")

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not create interconnect resources when addUpdateRemoteNodeEvent is called", func() {
			app.Action = func(_ *cli.Context) error {
				netConf := createUDNNetConf(types.NetworkTransportNoOverlay)
				netInfo, err := util.NewNetInfo(netConf)
				Expect(err).NotTo(HaveOccurred())
				Expect(netInfo.Transport()).To(Equal(types.NetworkTransportNoOverlay))

				node2 := createTestNode(node2Name, "2")
				clusterRouterName := netInfo.GetNetworkScopedClusterRouterName()
				transitSwitchName := netInfo.GetNetworkScopedName(types.TransitSwitch)

				// Setup NBDB with cluster router
				initialDB.NBData = []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name: clusterRouterName,
						UUID: clusterRouterName + "-UUID",
						ExternalIDs: map[string]string{
							types.NetworkExternalID:  udnName,
							types.TopologyExternalID: types.Layer3Topology,
						},
					},
				}

				nbClient, sbClient, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(initialDB)
				Expect(err).NotTo(HaveOccurred())
				defer libovsdbCleanup.Cleanup()

				// Create the controller
				watcher := &factory.WatchFactory{}
				zoneICHandler := zoneic.NewZoneInterconnectHandler(netInfo, nbClient, sbClient, watcher)

				cnci := &CommonNetworkControllerInfo{
					nbClient:     nbClient,
					sbClient:     sbClient,
					watchFactory: watcher,
				}

				controller := &Layer3UserDefinedNetworkController{
					BaseUserDefinedNetworkController: BaseUserDefinedNetworkController{
						BaseNetworkController: BaseNetworkController{
							CommonNetworkControllerInfo: *cnci,
							ReconcilableNetInfo:         util.NewReconcilableNetInfo(netInfo),
							zoneICHandler:               zoneICHandler,
						},
					},
				}

				// Call addUpdateRemoteNodeEvent with syncZoneIc=true
				err = controller.addUpdateRemoteNodeEvent(node2, true)
				Expect(err).NotTo(HaveOccurred(), "addUpdateRemoteNodeEvent should succeed for no-overlay (skips IC operations)")

				// Verify no transit switch was created
				ts := &nbdb.LogicalSwitch{Name: transitSwitchName}
				_, err = libovsdbops.GetLogicalSwitch(nbClient, ts)
				Expect(err).To(HaveOccurred(), "Transit switch should not exist for no-overlay transport")

				// Verify no remote ports were created
				remotePortName := netInfo.GetNetworkScopedName(types.TransitSwitchToRouterPrefix + node2Name)
				ports, err := libovsdbops.FindLogicalSwitchPortWithPredicate(nbClient, func(lsp *nbdb.LogicalSwitchPort) bool {
					return lsp.Name == remotePortName
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(ports).To(BeEmpty(), "No remote port should exist for no-overlay transport")

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("with overlay transport", func() {
		It("should create interconnect resources when addUpdateLocalNodeEvent is called", func() {
			app.Action = func(_ *cli.Context) error {
				// Empty transport = overlay/geneve
				netConf := createUDNNetConf("")
				netInfo, err := util.NewNetInfo(netConf)
				Expect(err).NotTo(HaveOccurred())
				Expect(netInfo.Transport()).NotTo(Equal(types.NetworkTransportNoOverlay))

				node1 := createTestNode(node1Name, "1")
				clusterRouterName := netInfo.GetNetworkScopedClusterRouterName()
				transitSwitchName := netInfo.GetNetworkScopedName(types.TransitSwitch)

				// Setup NBDB with cluster router and transit switch
				initialDB.NBData = []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name: clusterRouterName,
						UUID: clusterRouterName + "-UUID",
						ExternalIDs: map[string]string{
							types.NetworkExternalID:  udnName,
							types.TopologyExternalID: types.Layer3Topology,
						},
					},
					&nbdb.LogicalSwitch{
						Name: netInfo.GetNetworkScopedName(node1Name),
						UUID: netInfo.GetNetworkScopedName(node1Name) + "-UUID",
						ExternalIDs: map[string]string{
							types.NetworkExternalID:     udnName,
							types.NetworkRoleExternalID: types.NetworkRolePrimary,
						},
						OtherConfig: map[string]string{"subnet": "192.168.1.0/24"},
					},
					// Pre-create transit switch for overlay
					&nbdb.LogicalSwitch{
						Name: transitSwitchName,
						UUID: transitSwitchName + "-UUID",
						ExternalIDs: map[string]string{
							types.NetworkExternalID: udnName,
						},
					},
				}

				nbClient, sbClient, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(initialDB)
				Expect(err).NotTo(HaveOccurred())
				defer libovsdbCleanup.Cleanup()

				// Create the controller
				watcher := &factory.WatchFactory{}
				zoneICHandler := zoneic.NewZoneInterconnectHandler(netInfo, nbClient, sbClient, watcher)

				cnci := &CommonNetworkControllerInfo{
					nbClient:     nbClient,
					sbClient:     sbClient,
					watchFactory: watcher,
				}

				controller := &Layer3UserDefinedNetworkController{
					BaseUserDefinedNetworkController: BaseUserDefinedNetworkController{
						BaseNetworkController: BaseNetworkController{
							CommonNetworkControllerInfo: *cnci,
							ReconcilableNetInfo:         util.NewReconcilableNetInfo(netInfo),
							zoneICHandler:               zoneICHandler,
						},
					},
				}

				// Call addUpdateLocalNodeEvent with syncZoneIC=true
				nSyncs := &nodeSyncs{syncZoneIC: true}
				_ = controller.addUpdateLocalNodeEvent(node1, nSyncs)
				// Error may occur due to incomplete setup, but IC operations should have been attempted

				// Verify transit switch still exists
				ts := &nbdb.LogicalSwitch{Name: transitSwitchName}
				_, err = libovsdbops.GetLogicalSwitch(nbClient, ts)
				Expect(err).NotTo(HaveOccurred(), "Transit switch should exist for overlay transport")

				// Verify transit switch port was created
				transitSwitchPortName := netInfo.GetNetworkScopedName(types.TransitSwitchToRouterPrefix + node1Name)
				ports, err := libovsdbops.FindLogicalSwitchPortWithPredicate(nbClient, func(lsp *nbdb.LogicalSwitchPort) bool {
					return lsp.Name == transitSwitchPortName
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(ports).To(HaveLen(1), "Transit switch port should be created for overlay transport")

				// Verify router port was created
				routerPortName := netInfo.GetNetworkScopedName(types.RouterToTransitSwitchPrefix + node1Name)
				rports, err := libovsdbops.FindLogicalRouterPortWithPredicate(nbClient, func(lrp *nbdb.LogicalRouterPort) bool {
					return lrp.Name == routerPortName
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(rports).To(HaveLen(1), "Router port should be created for overlay transport")

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

var _ = Describe("Layer3 CUDN OutboundSNAT for no-overlay mode", func() {
	var (
		fakeOvn *FakeOVN
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		fakeOvn = NewFakeOVN(false, nodeName)
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	// Test 1: Verify SNAT with exempted_ext_ips is created on ovn_cluster_router in LOCAL gateway mode
	It("creates SNAT with exempted address set in local gateway mode on ovn-cluster-router", func() {
		// Step 1: Set config.Gateway.Mode = config.GatewayModeLocal
		By("Step 1: Setting up LOCAL gateway mode configuration")
		config.Gateway.Mode = config.GatewayModeLocal
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16" // Broader subnet to accommodate network ID 2

		app := cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		// Step 2: Create a Layer3 CUDN with no-overlay transport and outboundSNAT enabled
		By("Step 2: Creating Layer3 CUDN with no-overlay transport and outboundSNAT enabled")
		cudnNetInfo := userDefinedNetInfo{
			netName:        userDefinedNetworkName,
			nadName:        namespacedName(ns, nadName),
			topology:       types.Layer3Topology,
			clustersubnets: "10.100.0.0/16",
			hostsubnets:    "10.100.1.0/24",
			isPrimary:      true,
			transport:      types.NetworkTransportNoOverlay,
			outboundSNAT:   string(types.NoOverlaySNATEnabled),
		}

		app.Action = func(*cli.Context) error {
			// Create NAD with no-overlay transport and outboundSNAT enabled
			netConf := cudnNetInfo.netconf()

			nad, err := newNetworkAttachmentDefinition(ns, nadName, *netConf)
			Expect(err).NotTo(HaveOccurred())

			n := newUDNNamespace(ns)
			const nodeIPv4CIDR = "192.168.126.202/24"
			testNode, err := newNodeWithUserDefinedNetworks(nodeName, nodeIPv4CIDR, cudnNetInfo)
			Expect(err).NotTo(HaveOccurred())

			podInfo := dummyTestPod(ns, cudnNetInfo)

			By("Starting fakeOvn with database setup (without node initially)")
			fakeOvn.startWithDBSetup(
				libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitch{Name: nodeName},
					},
				},
				&corev1.NamespaceList{Items: []corev1.Namespace{*n}},
				&corev1.NodeList{Items: []corev1.Node{}}, // Empty - will add node after watch starts
				&corev1.PodList{Items: []corev1.Pod{*newMultiHomedPod(podInfo, cudnNetInfo)}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)
			podInfo.populateLogicalSwitchCache(fakeOvn)

			// Step 3: Initialize the CUDN controller
			By("Step 3: Initializing CUDN controller")
			fexec := util.GetExec().(*testing.FakeExec)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --columns=_uuid list Load_Balancer_Group",
			})
			fullL3UDNController := fakeOvn.fullL3UDNControllers[userDefinedNetworkName]
			Expect(fullL3UDNController).ToNot(BeNil())
			Expect(fullL3UDNController.init()).To(Succeed())

			userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
			Expect(ok).To(BeTrue())

			userDefinedNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
			podInfo.populateUserDefinedNetworkLogicalSwitchCache(userDefinedNetController)

			Expect(ConfigureAdvertisedNetworkIsolation(fakeOvn.nbClient)).To(Succeed())

			// Step 4: Start watching nodes to trigger addUpdateLocalNodeEvent()
			By("Starting node watch on L3 UDN controller")
			Expect(fakeOvn.registerUDNNodeHandler(userDefinedNetworkName)).To(Succeed())

			By("Adding node to trigger addUpdateLocalNodeEvent()")
			// Add the node AFTER starting the watch so the watch catches the ADD event
			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(), testNode, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			By("Node created successfully")

			// Wait for node processing to complete
			By("Waiting for NAT entries to be created on ovn_cluster_router")
			Eventually(func() error {
				// Step 5: Query the OVN NB database to find NAT entries on the CUDN's ovn_cluster_router
				clusterRouterName := userDefinedNetController.bnc.GetNetworkScopedClusterRouterName()

				lr := &nbdb.LogicalRouter{Name: clusterRouterName}
				lr, err = libovsdbops.GetLogicalRouter(fakeOvn.nbClient, lr)
				if err != nil {
					return fmt.Errorf("failed to get logical router %s: %v", clusterRouterName, err)
				}

				if len(lr.Nat) == 0 {
					return fmt.Errorf("no NAT entries found on router %s", clusterRouterName)
				}

				// Step 6: Verify NAT entries have exempted_ext_ips set
				foundSNATWithExemption := false
				for _, natUUID := range lr.Nat {
					nat := &nbdb.NAT{UUID: natUUID}
					nat, err = libovsdbops.GetNAT(fakeOvn.nbClient, nat)
					if err != nil {
						continue
					}

					if nat.Type == nbdb.NATTypeSNAT && nat.ExemptedExtIPs != nil && *nat.ExemptedExtIPs != "" {
						foundSNATWithExemption = true

						// Get the address set to verify UUID and contents
						dbIDs := libovsdbops.NewDbObjectIDs(
							libovsdbops.AddressSetNoOverlaySNATExemption,
							userDefinedNetController.bnc.controllerName,
							map[libovsdbops.ExternalIDKey]string{
								libovsdbops.ObjectNameKey: "no-overlay-snat-exemption",
								libovsdbops.NetworkKey:    userDefinedNetworkName,
							},
						)
						as, err := fakeOvn.controller.addressSetFactory.GetAddressSet(dbIDs)
						if err != nil {
							return fmt.Errorf("failed to get address set: %v", err)
						}

						// Verify the NAT's ExemptedExtIPs field contains the actual address set UUID
						v4UUID, v6UUID := as.GetASUUID()
						expectedUUID := v4UUID // For IPv4 mode
						if config.IPv6Mode && !config.IPv4Mode {
							expectedUUID = v6UUID
						}
						if *nat.ExemptedExtIPs != expectedUUID {
							return fmt.Errorf("NAT ExemptedExtIPs (%s) does not match address set UUID (%s)",
								*nat.ExemptedExtIPs, expectedUUID)
						}

						// Verify the address set contains both cluster subnet and node host IP
						ipv4Addrs, ipv6Addrs := as.GetAddresses()
						allAddrs := append(ipv4Addrs, ipv6Addrs...)
						expectedAddrs := []string{"10.100.0.0/16", "192.168.126.202"}
						for _, expectedAddr := range expectedAddrs {
							if !slices.Contains(allAddrs, expectedAddr) {
								return fmt.Errorf("address set does not contain %s, has: %v", expectedAddr, allAddrs)
							}
						}
						break
					}
				}

				if !foundSNATWithExemption {
					return fmt.Errorf("no SNAT with ExemptedExtIPs found on router %s", clusterRouterName)
				}

				return nil
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

			return nil
		}

		Expect(app.Run([]string{app.Name})).To(Succeed())
	})

	// The exemption address set must be synced before any SNAT that references
	// it is created; if the sync fails, no SNAT may be created at all.
	It("does not create SNAT rules when the exemption address set cannot be synced", func() {
		By("Setting up LOCAL gateway mode configuration")
		config.Gateway.Mode = config.GatewayModeLocal
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"

		app := cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		cudnNetInfo := userDefinedNetInfo{
			netName:        userDefinedNetworkName,
			nadName:        namespacedName(ns, nadName),
			topology:       types.Layer3Topology,
			clustersubnets: "10.100.0.0/16",
			hostsubnets:    "10.100.1.0/24",
			isPrimary:      true,
			transport:      types.NetworkTransportNoOverlay,
			outboundSNAT:   string(types.NoOverlaySNATEnabled),
		}

		app.Action = func(*cli.Context) error {
			netConf := cudnNetInfo.netconf()
			nad, err := newNetworkAttachmentDefinition(ns, nadName, *netConf)
			Expect(err).NotTo(HaveOccurred())

			n := newUDNNamespace(ns)
			const nodeIPv4CIDR = "192.168.126.202/24"
			testNode, err := newNodeWithUserDefinedNetworks(nodeName, nodeIPv4CIDR, cudnNetInfo)
			Expect(err).NotTo(HaveOccurred())
			By("Corrupting the host-cidrs annotation so that the exemption address set sync fails")
			testNode.Annotations[util.OVNNodeHostCIDRs] = "not-a-json-list"

			fakeOvn.startWithDBSetup(
				libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitch{Name: nodeName},
					},
				},
				&corev1.NamespaceList{Items: []corev1.Namespace{*n}},
				&corev1.NodeList{Items: []corev1.Node{}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)

			By("Initializing CUDN controller")
			fexec := util.GetExec().(*testing.FakeExec)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --columns=_uuid list Load_Balancer_Group",
			})
			fullL3UDNController := fakeOvn.fullL3UDNControllers[userDefinedNetworkName]
			Expect(fullL3UDNController).ToNot(BeNil())
			Expect(fullL3UDNController.init()).To(Succeed())

			By("Starting node watch on L3 UDN controller")
			Expect(fakeOvn.registerUDNNodeHandler(userDefinedNetworkName)).To(Succeed())

			By("Adding the node to trigger addUpdateLocalNodeEvent()")
			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(), testNode, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Expecting the node sync to fail and be deferred for retry")
			Eventually(func() bool {
				_, failed := fullL3UDNController.gatewaysFailed.Load(nodeName)
				return failed
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())
			Eventually(func() bool {
				_, failed := fullL3UDNController.addNodeFailed.Load(nodeName)
				return failed
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())

			By("Expecting no SNAT to have been created")
			Consistently(func() []*nbdb.NAT {
				nats, err := libovsdbops.FindNATsWithPredicate(fakeOvn.nbClient, func(*nbdb.NAT) bool { return true })
				Expect(err).NotTo(HaveOccurred())
				return nats
			}).WithTimeout(2 * time.Second).WithPolling(200 * time.Millisecond).Should(BeEmpty())

			return nil
		}

		Expect(app.Run([]string{app.Name})).To(Succeed())
	})

	// Test 2: Verify SNAT with exempted_ext_ips is created on GR_<node> in SHARED gateway mode
	It("creates SNAT with exempted address set in shared gateway mode on gateway router", func() {
		// Step 1: Set config.Gateway.Mode = config.GatewayModeShared
		config.Gateway.Mode = config.GatewayModeShared
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16" // Broader subnet to accommodate network ID 2

		app := cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		// Step 2: Create a Layer3 CUDN with no-overlay transport and outboundSNAT enabled
		cudnNetInfo := userDefinedNetInfo{
			netName:        userDefinedNetworkName,
			nadName:        namespacedName(ns, nadName),
			topology:       types.Layer3Topology,
			clustersubnets: "10.100.0.0/16",
			hostsubnets:    "10.100.1.0/24",
			isPrimary:      true,
			transport:      types.NetworkTransportNoOverlay,
			outboundSNAT:   string(types.NoOverlaySNATEnabled),
		}

		app.Action = func(*cli.Context) error {
			// Create NAD with no-overlay transport and outboundSNAT enabled
			netConf := cudnNetInfo.netconf()

			nad, err := newNetworkAttachmentDefinition(ns, nadName, *netConf)
			Expect(err).NotTo(HaveOccurred())

			n := newUDNNamespace(ns)
			const nodeIPv4CIDR = "192.168.126.202/24"
			testNode, err := newNodeWithUserDefinedNetworks(nodeName, nodeIPv4CIDR, cudnNetInfo)
			Expect(err).NotTo(HaveOccurred())

			podInfo := dummyTestPod(ns, cudnNetInfo)

			fakeOvn.startWithDBSetup(
				libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitch{Name: nodeName},
					},
				},
				&corev1.NamespaceList{Items: []corev1.Namespace{*n}},
				&corev1.NodeList{Items: []corev1.Node{}}, // Empty - will add node after watch starts
				&corev1.PodList{Items: []corev1.Pod{*newMultiHomedPod(podInfo, cudnNetInfo)}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)
			podInfo.populateLogicalSwitchCache(fakeOvn)

			// Step 3: Initialize the CUDN controller
			fexec := util.GetExec().(*testing.FakeExec)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --columns=_uuid list Load_Balancer_Group",
			})
			fullL3UDNController := fakeOvn.fullL3UDNControllers[userDefinedNetworkName]
			Expect(fullL3UDNController).ToNot(BeNil())
			Expect(fullL3UDNController.init()).To(Succeed())

			userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
			Expect(ok).To(BeTrue())

			userDefinedNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
			podInfo.populateUserDefinedNetworkLogicalSwitchCache(userDefinedNetController)

			Expect(ConfigureAdvertisedNetworkIsolation(fakeOvn.nbClient)).To(Succeed())

			// Step 4: Create and advertise a pod on the CUDN to trigger gateway SNAT creation
			By("Starting node watch on L3 UDN controller for shared gateway mode")
			Expect(fakeOvn.registerUDNNodeHandler(userDefinedNetworkName)).To(Succeed())

			By("Adding node to trigger gateway creation with SNAT+exemption")
			// Add the node AFTER starting the watch so the watch catches the ADD event
			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(), testNode, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := userDefinedNetController.bnc.GetLocalNode()
				return err
			}).WithTimeout(5 * time.Second).Should(Succeed())
			Expect(userDefinedNetController.bnc.WatchPods()).To(Succeed())
			By("Node created, gateway should be initialized")

			// Wait for pod and gateway processing to complete
			Eventually(func() error {
				// Step 5: Query the OVN NB database to find NAT entries on GR_<nodeName>
				gwRouterName := userDefinedNetController.bnc.GetNetInfo().GetNetworkScopedGWRouterName(nodeName)
				lr := &nbdb.LogicalRouter{Name: gwRouterName}
				lr, err = libovsdbops.GetLogicalRouter(fakeOvn.nbClient, lr)
				if err != nil {
					return fmt.Errorf("failed to get gateway router %s: %v", gwRouterName, err)
				}

				if len(lr.Nat) == 0 {
					return fmt.Errorf("no NAT entries found on gateway router %s", gwRouterName)
				}

				// Step 6: Verify NAT entries have exempted_ext_ips set
				foundSNATWithExemption := false
				for _, natUUID := range lr.Nat {
					nat := &nbdb.NAT{UUID: natUUID}
					nat, err = libovsdbops.GetNAT(fakeOvn.nbClient, nat)
					if err != nil {
						continue
					}

					if nat.Type == nbdb.NATTypeSNAT && nat.ExemptedExtIPs != nil && *nat.ExemptedExtIPs != "" {
						foundSNATWithExemption = true

						// Get the address set to verify UUID and contents
						dbIDs := libovsdbops.NewDbObjectIDs(
							libovsdbops.AddressSetNoOverlaySNATExemption,
							userDefinedNetController.bnc.controllerName,
							map[libovsdbops.ExternalIDKey]string{
								libovsdbops.ObjectNameKey: "no-overlay-snat-exemption",
								libovsdbops.NetworkKey:    userDefinedNetController.bnc.GetNetworkName(),
							},
						)
						as, err := fakeOvn.controller.addressSetFactory.GetAddressSet(dbIDs)
						if err != nil {
							return fmt.Errorf("failed to get address set: %v", err)
						}

						// Verify the NAT's ExemptedExtIPs field contains the actual address set UUID
						v4UUID, v6UUID := as.GetASUUID()
						expectedUUID := v4UUID // For IPv4 mode
						if config.IPv6Mode && !config.IPv4Mode {
							expectedUUID = v6UUID
						}
						if *nat.ExemptedExtIPs != expectedUUID {
							return fmt.Errorf("NAT ExemptedExtIPs (%s) does not match address set UUID (%s)",
								*nat.ExemptedExtIPs, expectedUUID)
						}

						// Verify the address set contains both cluster subnet and node host IP
						ipv4Addrs, ipv6Addrs := as.GetAddresses()
						allAddrs := append(ipv4Addrs, ipv6Addrs...)
						expectedAddrs := []string{"10.100.0.0/16", "192.168.126.202"}
						for _, expectedAddr := range expectedAddrs {
							if !slices.Contains(allAddrs, expectedAddr) {
								return fmt.Errorf("address set does not contain %s, has: %v", expectedAddr, allAddrs)
							}
						}
						break
					}
				}

				if !foundSNATWithExemption {
					return fmt.Errorf("no SNAT with ExemptedExtIPs found on gateway router %s", gwRouterName)
				}

				return nil
			}).WithTimeout(10 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

			return nil
		}

		Expect(app.Run([]string{app.Name})).To(Succeed())
	})
})

func newPodWithPrimaryUDN(
	nodeName, nodeSubnet, nodeMgtIP, nodeGWIP, podName, podIPs, podMAC, namespace string,
	primaryUDNConfig userDefinedNetInfo,
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

func makeCUDNOwnerRef(name string) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: userdefinednetworkv1.SchemeGroupVersion.String(),
		Kind:       "ClusterUserDefinedNetwork",
		Name:       name,
		Controller: &controller,
	}
}

func getNetworkRole(netInfo util.NetInfo) string {
	return util.GetUserDefinedNetworkRole(netInfo.IsPrimaryNetwork())
}

func (sni *userDefinedNetInfo) netconf() *ovncnitypes.NetConf {
	const plugin = "ovn-k8s-cni-overlay"

	role := types.NetworkRoleSecondary
	transitSubnet := ""
	if sni.isPrimary {
		role = types.NetworkRolePrimary
		if sni.topology == types.Layer2Topology {
			transitSubnets := []string{}
			for _, clusterSubnet := range strings.Split(sni.clustersubnets, ",") {
				_, cidr, err := net.ParseCIDR(clusterSubnet)
				Expect(err).NotTo(HaveOccurred())
				if knet.IsIPv4CIDR(cidr) {
					transitSubnets = append(transitSubnets, config.ClusterManager.V4TransitSubnet)
				} else {
					transitSubnets = append(transitSubnets, config.ClusterManager.V6TransitSubnet)
				}
			}
			transitSubnet = strings.Join(transitSubnets, ",")
		}
	}

	netconf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: sni.netName,
			Type: plugin,
		},
		Topology:           sni.topology,
		NADName:            sni.nadName,
		Subnets:            sni.clustersubnets,
		Role:               role,
		AllowPersistentIPs: sni.allowPersistentIPs,
		TransitSubnet:      transitSubnet,
	}

	if sni.hasEVPN {
		netconf.Transport = types.NetworkTransportEVPN
	}
	if sni.transport != "" {
		netconf.Transport = sni.transport
	}
	if sni.outboundSNAT != "" {
		netconf.OutboundSNAT = sni.outboundSNAT
	}

	return netconf
}

func dummyTestPod(nsName string, info userDefinedNetInfo) testPod {
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

func dummySecondaryLayer3UserDefinedNetwork(clustersubnets, hostsubnets string) userDefinedNetInfo {
	return userDefinedNetInfo{
		netName:        userDefinedNetworkName,
		nadName:        namespacedName(ns, nadName),
		topology:       types.Layer3Topology,
		clustersubnets: clustersubnets,
		hostsubnets:    hostsubnets,
	}
}

func dummyPrimaryLayer3UserDefinedNetwork(clustersubnets, hostsubnets string) userDefinedNetInfo {
	secondaryNet := dummySecondaryLayer3UserDefinedNetwork(clustersubnets, hostsubnets)
	secondaryNet.isPrimary = true
	return secondaryNet
}

func dummyPrimaryLayer3EVPNCUDN(clustersubnets, hostsubnets string) userDefinedNetInfo {
	udnNetInfo := dummyPrimaryLayer3UserDefinedNetwork(clustersubnets, hostsubnets)
	udnNetInfo.hasEVPN = true
	return udnNetInfo
}

// This util is returning a network-name/hostSubnet for the node's node-subnets annotation
func (sni *userDefinedNetInfo) String() string {
	return fmt.Sprintf("%q: %q", sni.netName, sni.hostsubnets)
}

func buildClusterNodeIPsAddressSetsForNodes(nodes []corev1.Node) (*nbdb.AddressSet, *nbdb.AddressSet, error) {
	nodePtrs := make([]*corev1.Node, 0, len(nodes))
	for i := range nodes {
		nodePtrs = append(nodePtrs, &nodes[i])
	}
	v4NodeAddrs, v6NodeAddrs, err := util.GetNodeAddresses(config.IPv4Mode, config.IPv6Mode, nodePtrs...)
	if err != nil {
		return nil, nil, err
	}
	nodeAddrs := append(v4NodeAddrs, v6NodeAddrs...)
	nodeIPsAS4, nodeIPsAS6 := buildEgressIPNodeAddressSets(util.StringSlice(nodeAddrs))
	return nodeIPsAS4, nodeIPsAS6, nil
}

func newNodeWithUserDefinedNetworks(nodeName string, nodeIPv4CIDR string, netInfos ...userDefinedNetInfo) (*corev1.Node, error) {
	var nodeSubnetInfo []string
	for _, info := range netInfos {
		nodeSubnetInfo = append(nodeSubnetInfo, info.String())
	}

	parsedNodeSubnets := fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet)
	if len(nodeSubnetInfo) > 0 {
		parsedNodeSubnets = fmt.Sprintf("{\"default\":\"%s\", %s}", v4Node1Subnet, strings.Join(nodeSubnetInfo, ","))
	}

	nodeIP, nodeCIDR, err := net.ParseCIDR(nodeIPv4CIDR)
	if err != nil {
		return nil, err
	}
	nextHopIP := util.GetNodeGatewayIfAddr(nodeCIDR).IP
	nodeCIDR.IP = nodeIP

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				util.Layer2TopologyVersion:                                  util.TransitRouterTopoVersion,
				"k8s.ovn.org/node-primary-ifaddr":                           fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4CIDR, ""),
				"k8s.ovn.org/node-subnets":                                  parsedNodeSubnets,
				util.OVNNodeHostCIDRs:                                       fmt.Sprintf("[\"%s\"]", nodeIPv4CIDR),
				"k8s.ovn.org/l3-gateway-config":                             fmt.Sprintf("{\"default\":{\"mode\":\"shared\",\"bridge-id\":\"breth0\",\"interface-id\":\"breth0_ovn-worker\",\"mac-address\":%q,\"ip-addresses\":[%[2]q],\"ip-address\":%[2]q,\"next-hops\":[%[3]q],\"next-hop\":%[3]q,\"node-port-enable\":\"true\",\"vlan-id\":\"0\"}}", util.IPAddrToHWAddr(nodeIP), nodeCIDR, nextHopIP),
				util.OvnNodeChassisID:                                       chassisIDForNode(nodeName),
				"k8s.ovn.org/network-ids":                                   fmt.Sprintf("{\"default\":\"0\",\"isolatednet\":\"%s\"}", userDefinedNetworkID),
				util.OvnNodeID:                                              "4",
				"k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids": "{\"isolatednet\":\"25\"}",
				"k8s.ovn.org/node-transit-switch-port-ifaddr":               "{\"ipv4\":\"100.88.0.3/16\"}",
			},
			Labels: map[string]string{
				"k8s.ovn.org/egress-assignable": "",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
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

func expectedGWEntities(nodeName string, netInfo util.NetInfo, gwConfig util.L3GatewayConfig) []libovsdbtest.TestData {
	gwRouterName := fmt.Sprintf("GR_%s_%s", netInfo.GetNetworkName(), nodeName)

	expectedEntities := append(
		expectedGWRouterPlusNATAndStaticRoutes(nodeName, gwRouterName, netInfo, gwConfig),
		expectedGRToJoinSwitchLRP(gwRouterName, gwRouterJoinIPAddress(), netInfo),
		expectedGRToExternalSwitchLRP(gwRouterName, netInfo, nodePhysicalIPAddress(), udnGWSNATAddress()),
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

	const (
		nat1             = "abc-UUID"
		nat2             = "cba-UUID"
		staticRoute1     = "srA-UUID"
		staticRoute2     = "srB-UUID"
		staticRoute3     = "srC-UUID"
		ipv4DefaultRoute = "0.0.0.0/0"
	)

	staticRouteOutputPort := types.GWRouterToExtSwitchPrefix + netInfo.GetNetworkScopedGWRouterName(nodeName)
	gwRouterLRPUUID := fmt.Sprintf("%s%s-UUID", types.GWRouterToJoinSwitchPrefix, gwRouterName)
	grOptions := gwRouterOptions(gwConfig)
	sr1 := expectedGRStaticRoute(staticRoute1, netInfo.Subnets()[0].CIDR.String(), dummyMasqueradeIP().IP.String(), nil, nil, netInfo)
	if netInfo.TopologyType() == types.Layer2Topology {
		gwRouterLRPUUID = fmt.Sprintf("%s%s-UUID", types.RouterToTransitRouterPrefix, gwRouterName)
		grOptions["lb_force_snat_ip"] = gwRouterJoinIPAddress().IP.String()
		transitRouteOutputPort := types.RouterToTransitRouterPrefix + netInfo.GetNetworkScopedGWRouterName(nodeName)
		trInfo := getTestTransitRouterInfo(netInfo)
		sr1 = expectedGRStaticRoute(staticRoute1, netInfo.Subnets()[0].CIDR.String(), trInfo.transitRouterNets[0].IP.String(), nil, &transitRouteOutputPort, netInfo)
	}
	nextHopIP := gwConfig.NextHops[0].String()
	nextHopMasqIP := nextHopMasqueradeIP().String()
	masqSubnet := config.Gateway.V4MasqueradeSubnet
	var nat []string
	nat = append(nat, nat1, nat2)
	expectedEntities := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			Name:         gwRouterName,
			UUID:         gwRouterName + "-UUID",
			ExternalIDs:  gwRouterExternalIDs(netInfo, gwConfig),
			Options:      grOptions,
			Ports:        []string{gwRouterLRPUUID, gwRouterToExtLRPUUID},
			Nat:          nat,
			StaticRoutes: []string{staticRoute1, staticRoute2, staticRoute3},
			Copp:         ptr.To(string(coppUUID)),
			LoadBalancerGroup: []string{
				netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterLBGroupName) + "-UUID",
				netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterRouterLBGroupName) + "-UUID",
			},
		},
		sr1,
		expectedGRStaticRoute(staticRoute2, ipv4DefaultRoute, nextHopIP, nil, &staticRouteOutputPort, netInfo),
		expectedGRStaticRoute(staticRoute3, masqSubnet, nextHopMasqIP, nil, &staticRouteOutputPort, netInfo),
	}
	var udnSubnetMasqMatch string
	if netInfo.Transport() == types.NetworkTransportEVPN {
		nodeIPV4ASHashName, _ := addressset.GetHashNamesForAS(getClusterNodeIPsAddrSetDbIDsForTest())
		udnSubnetMasqMatch = fmt.Sprintf("ip4.dst == $%s", nodeIPV4ASHashName)
	}
	expectedEntities = append(expectedEntities, newNATEntry(nat1, dummyMasqueradeIP().IP.String(), gwRouterJoinIPAddress().IP.String(), standardNonDefaultNetworkExtIDs(netInfo), ""))
	expectedEntities = append(expectedEntities, newNATEntry(nat2, dummyMasqueradeIP().IP.String(), netInfo.Subnets()[0].CIDR.String(), standardNonDefaultNetworkExtIDs(netInfo), udnSubnetMasqMatch))
	return expectedEntities
}

func expectedStaticMACBindings(gwRouterName string, ips []net.IP) []libovsdbtest.TestData {
	lrpName := fmt.Sprintf("%s%s", types.GWRouterToExtSwitchPrefix, gwRouterName)
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
	options := map[string]string{libovsdbops.GatewayMTU: fmt.Sprintf("%d", 1400)}
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
		masqSNATUUID1     = "masq-snat1-UUID"
	)
	masqIPAddr := dummyMasqueradeIP().IP.String()
	clusterRouterName := fmt.Sprintf("%s_ovn_cluster_router", netInfo.GetNetworkName())
	rtosLRPName := fmt.Sprintf("%s%s", types.RouterToSwitchPrefix, netInfo.GetNetworkScopedName(nodeName))
	rtosLRPUUID := rtosLRPName + "-UUID"

	gatewayChassisUUID := fmt.Sprintf("%s-%s-UUID", rtosLRPName, gwConfig.ChassisID)
	lrsrNextHop := gwRouterJoinIPAddress().IP.String()
	if config.Gateway.Mode == config.GatewayModeLocal {
		lrsrNextHop = managementPortIP(nodeSubnet).String()
	}

	clusterRouterExternalIDs := standardNonDefaultNetworkExtIDs(netInfo)
	clusterRouterExternalIDs["k8s-cluster-router"] = "yes"

	clusterRouterOptions := map[string]string{"always_learn_from_arp_request": "false"}
	if config.Gateway.Mode == config.GatewayModeLocal {
		clusterRouterOptions["ct-commit-all"] = "true"
	}

	clusterRouter := &nbdb.LogicalRouter{
		Name:        clusterRouterName,
		UUID:        clusterRouterName + "-UUID",
		Ports:       []string{rtosLRPUUID},
		ExternalIDs: clusterRouterExternalIDs,
		Copp:        ptr.To(string(coppUUID)),
		Options:     clusterRouterOptions,
	}

	expectedEntities := []libovsdbtest.TestData{
		expectedGatewayChassis(nodeName, netInfo, gwConfig),
		clusterRouter,
		&nbdb.LogicalRouterPort{
			UUID:           rtosLRPUUID,
			Name:           rtosLRPName,
			Networks:       []string{"192.168.1.1/24"},
			MAC:            "0a:58:c0:a8:01:01",
			GatewayChassis: []string{gatewayChassisUUID},
			Options:        map[string]string{libovsdbops.GatewayMTU: "1400"},
		},
	}

	hasEVPN := netInfo.Transport() == types.NetworkTransportEVPN
	if !hasEVPN {
		rtotsLRPName := netInfo.GetNetworkScopedName(types.RouterToTransitSwitchPrefix + nodeName)
		rtotsLRPUUID := rtotsLRPName + "-UUID"
		expectedEntities = append(expectedEntities,
			&nbdb.LogicalRouterPort{
				UUID:     rtotsLRPUUID,
				Name:     rtotsLRPName,
				Networks: []string{"100.88.0.3/16"},
				MAC:      "0a:58:64:58:00:03",
				Options:  map[string]string{"mcast_flood": "true"},
			},
		)
		clusterRouter.Ports = append(clusterRouter.Ports, rtotsLRPUUID)
	}

	if netInfo.IsPrimaryNetwork() {
		rtojLRPName := types.GWRouterToJoinSwitchPrefix + clusterRouterName
		rtojLRPUUID := rtojLRPName + "-UUID"
		nodeIP := gwConfig.IPAddresses[0].IP.String()
		masqSNAT := newNATEntry(masqSNATUUID1, "169.254.169.14", nodeSubnet.String(), standardNonDefaultNetworkExtIDs(netInfo), "")
		masqSNAT.LogicalPort = ptr.To(fmt.Sprintf("rtos-%s_%s", netInfo.GetNetworkName(), nodeName))
		masqSNAT.Match = getMasqueradeManagementIPSNATMatch(util.IPAddrToHWAddr(managementPortIP(nodeSubnet)).String())
		if hasEVPN {
			nodeIPV4ASHashName, _ := addressset.GetHashNamesForAS(getClusterNodeIPsAddrSetDbIDsForTest())
			udnEnabledSvcV4ASHashName, _ := addressset.GetHashNamesForAS(udnenabledsvc.GetAddressSetDBIDs())
			masqSNAT.Match += fmt.Sprintf(" && (ip4.dst == $%s || ip4.dst == $%s)", nodeIPV4ASHashName, udnEnabledSvcV4ASHashName)
		}
		expectedEntities = append(expectedEntities,
			&nbdb.LogicalRouterPort{
				UUID:        rtojLRPUUID,
				Name:        rtojLRPName,
				Networks:    []string{"100.65.0.1/16"},
				MAC:         "0a:58:64:41:00:01",
				ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
			},
			expectedGRStaticRoute(staticRouteUUID1, nodeSubnet.String(), lrsrNextHop, &nbdb.LogicalRouterStaticRoutePolicySrcIP, nil, netInfo),
			expectedGRStaticRoute(staticRouteUUID2, gwRouterJoinIPAddress().IP.String(), gwRouterJoinIPAddress().IP.String(), nil, nil, netInfo),
			expectedLogicalRouterPolicy(routerPolicyUUID1, netInfo, nodeName, nodeIP, managementPortIP(nodeSubnet).String()),
			expectedLogicalRouterPolicy(routerPolicyUUID2, netInfo, nodeName, masqIPAddr, managementPortIP(nodeSubnet).String()),
			masqSNAT,
		)
		clusterRouter.Ports = append(clusterRouter.Ports, rtojLRPUUID)
		clusterRouter.Nat = []string{masqSNATUUID1}
		clusterRouter.Policies = []string{routerPolicyUUID1, routerPolicyUUID2}
		clusterRouter.StaticRoutes = []string{staticRouteUUID1, staticRouteUUID2}
	}
	return expectedEntities
}

func expectedLogicalRouterPolicy(routerPolicyUUID1 string, netInfo util.NetInfo, nodeName, destIP, nextHop string) *nbdb.LogicalRouterPolicy {
	const (
		priority      = 1004
		rerouteAction = "reroute"
	)
	inport := netInfo.GetNetworkScopedRouterToSwitchPortName(nodeName)
	networkScopedSwitchName := netInfo.GetNetworkScopedSwitchName(nodeName)

	return &nbdb.LogicalRouterPolicy{
		UUID:        routerPolicyUUID1,
		Action:      rerouteAction,
		ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
		Match:       fmt.Sprintf("inport == %q && ip4.dst == %s /* %s */", inport, destIP, networkScopedSwitchName),
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
		Mask: net.CIDRMask(32, 32),
	}
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
		libovsdbops.RouterPort: types.GWRouterToExtSwitchPrefix + gatewayRouterName,
	}
}

func expectedJoinSwitchAndLSPs(netInfo util.NetInfo, nodeName string) []libovsdbtest.TestData {
	gwRouterName := netInfo.GetNetworkScopedGWRouterName(nodeName)
	clusterRouterName := netInfo.GetNetworkScopedClusterRouterName()
	joinToGRLSPName := types.JoinSwitchToGWRouterPrefix + gwRouterName
	joinToCRLSPName := types.JoinSwitchToGWRouterPrefix + clusterRouterName
	joinToGRLSPUUID := joinToGRLSPName + "-UUID"
	joinToCRLSPUUID := joinToCRLSPName + "-UUID"
	expectedData := []libovsdbtest.TestData{
		&nbdb.LogicalSwitch{
			UUID:        "join-UUID",
			Name:        netInfo.GetNetworkScopedJoinSwitchName(),
			Ports:       []string{joinToGRLSPUUID, joinToCRLSPUUID},
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
		},
		&nbdb.LogicalSwitchPort{
			UUID:        joinToGRLSPUUID,
			Name:        joinToGRLSPName,
			Addresses:   []string{"router"},
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
			Options:     map[string]string{libovsdbops.RouterPort: types.GWRouterToJoinSwitchPrefix + gwRouterName},
			Type:        "router",
		},
		&nbdb.LogicalSwitchPort{
			UUID:      joinToCRLSPUUID,
			Name:      joinToCRLSPName,
			Addresses: []string{"router"},
			Options:   map[string]string{libovsdbops.RouterPort: types.GWRouterToJoinSwitchPrefix + clusterRouterName},
			Type:      "router",
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

	dynamicNeighRouters := "false"

	return map[string]string{
		"lb_force_snat_ip":              "router_ip",
		"mac_binding_age_threshold":     "300",
		"chassis":                       gwConfig.ChassisID,
		"always_learn_from_arp_request": "false",
		"dynamic_neigh_routers":         dynamicNeighRouters,
		"disable_garp_rarp":             "true",
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

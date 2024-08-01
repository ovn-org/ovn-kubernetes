package ovn

import (
	"context"
	"fmt"
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
		func(netInfo secondaryNetInfo) {
			podInfo := dummyL2TestPod(ns, netInfo)
			app.Action = func(ctx *cli.Context) error {
				nad, err := newNetworkAttachmentDefinition(
					ns,
					nadName,
					*netInfo.netconf(),
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(netInfo.setupOVNDependencies(&initialDB)).To(Succeed())

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
				if netInfo.isPrimary {
					defaultNetExpectations = emptyDefaultClusterNetworkNodeSwitch(podInfo.nodeName)
				}

				var expectationOptions []option
				if netInfo.isPrimary {
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
			secondaryNetInfo{
				netName:  secondaryNetworkName,
				nadName:  namespacedName(ns, nadName),
				topology: ovntypes.Layer2Topology,
				subnets:  "100.200.0.0/16",
			},
		),

		table.Entry("pod on a user defined primary network",
			secondaryNetInfo{
				netName:   secondaryNetworkName,
				nadName:   namespacedName(ns, nadName),
				topology:  ovntypes.Layer2Topology,
				subnets:   "100.200.0.0/16",
				isPrimary: true,
			},
		),
	)

})

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

func expectedLayer2EgressEntities(networkName string) []libovsdbtest.TestData {
	clusterRouterName := fmt.Sprintf("GR_%s_test-node", networkName)
	expectedEntities := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			Name:         clusterRouterName,
			UUID:         clusterRouterName + "-UUID",
			Ports:        []string{"rtoj-isolatednet_ovn_layer2_switch-UUID"},
			StaticRoutes: []string{"sr1-UUID", "sr2-UUID"},
			Policies:     []string{"lrpol1-UUID", "lrpol2-UUID"},
		},
		&nbdb.LogicalRouterPort{UUID: "rtoj-isolatednet_ovn_layer2_switch-UUID", Name: "rtoj-isolatednet_ovn_layer2_switch", Networks: []string{"100.200.0.1/16"}, MAC: "0a:58:64:c8:00:03", GatewayChassis: []string{"rtos-isolatednet_test-node-abdcef-UUID"}},
		expectedGRStaticRoute("sr1-UUID", "192.168.0.0/16", "10.10.10.0", &nbdb.LogicalRouterStaticRoutePolicySrcIP, nil),
		expectedGRStaticRoute("sr2-UUID", "10.10.10.0", "10.10.10.0", nil, nil),
		&nbdb.LogicalRouterPolicy{UUID: "lrpol1-UUID", Action: "reroute", ExternalIDs: map[string]string{"k8s.ovn.org/topology": "layer3", "k8s.ovn.org/network": "isolatednet"}, Match: "inport == \"rtos-isolatednet_test-node\" && ip4.dst == 10.89.0.14 /* isolatednet_test-node */", Nexthops: []string{"192.168.0.2"}, Priority: 1004},
		&nbdb.LogicalRouterPolicy{UUID: "lrpol2-UUID", Action: "reroute", ExternalIDs: map[string]string{"k8s.ovn.org/topology": "layer3", "k8s.ovn.org/network": "isolatednet"}, Match: "inport == \"rtos-isolatednet_test-node\" && ip4.dst == 169.254.169.13 /* isolatednet_test-node */", Nexthops: []string{"192.168.0.2"}, Priority: 1004},
	}
	return expectedEntities
}

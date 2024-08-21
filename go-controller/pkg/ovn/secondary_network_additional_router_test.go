package ovn

import (
	"context"
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
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type podSecondaryNetworkInfo struct {
	secondaryNetInfo
	podIP    string
	podMAC   string
	podRoute []util.PodRoute
}

var _ = Describe("OVN pod operations on NADs with additional routes", func() {
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
		"reconcile a new POD on a NAD with additional destination configuraiton",
		func(podInfo podSecondaryNetworkInfo) {
			tpod := newTPod(nodeName, "10.128.1.0/24", "10.128.1.2", "10.128.1.1", podName, "10.128.1.3", "0a:58:0a:80:01:03", ns)
			tpod.addNetwork(
				podInfo.netName,
				podInfo.nadName,
				podInfo.subnets,
				"",
				"",
				podInfo.podIP,
				podInfo.podMAC,
				podInfo.Role(),
				0,
				podInfo.podRoute,
			)

			app.Action = func(ctx *cli.Context) error {
				nad, err := newNetworkAttachmentDefinition(
					ns,
					nadName,
					*podInfo.netconf(),
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(podInfo.setupOVNDependencies(&initialDB)).To(Succeed())

				const nodeIPv4CIDR = "192.168.126.202/24"
				testNode, err := newNodeWithSecondaryNets(nodeName, nodeIPv4CIDR, podInfo.secondaryNetInfo)
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
							*newMultiHomedPod(tpod.namespace, tpod.podName, tpod.nodeName, tpod.podIP, podInfo.secondaryNetInfo),
						},
					},
					&nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					},
				)
				tpod.populateLogicalSwitchCache(fakeOvn)

				// pod exists, networks annotations don't
				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tpod.namespace).Get(context.Background(), tpod.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				Expect(fakeOvn.controller.WatchNamespaces()).NotTo(HaveOccurred())
				Expect(fakeOvn.controller.WatchPods()).NotTo(HaveOccurred())
				secondaryNetController, ok := fakeOvn.secondaryControllers[secondaryNetworkName]
				Expect(ok).To(BeTrue())

				secondaryNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
				tpod.populateSecondaryNetworkLogicalSwitchCache(fakeOvn, secondaryNetController)
				Expect(secondaryNetController.bnc.WatchNodes()).To(Succeed())
				Expect(secondaryNetController.bnc.WatchPods()).To(Succeed())

				// check that after start networks annotations and nbdb will be updated
				Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, tpod.namespace, tpod.podName)
				}).WithTimeout(2 * time.Second).Should(MatchJSON(tpod.getAnnotationsJson()))

				defaultNetExpectations := getDefaultNetExpectedPodsAndSwitches([]testPod{tpod}, []string{nodeName})
				Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(
						append(
							defaultNetExpectations,
							newSecondaryNetworkExpectationMachine(
								fakeOvn,
								[]testPod{tpod},
							).expectedLogicalSwitchesAndPorts()...)))

				return nil
			}

			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		table.Entry("pod on a layer3 secondary network",
			podSecondaryNetworkInfo{
				secondaryNetInfo: secondaryNetInfo{
					netName:   secondaryNetworkName,
					nadName:   namespacedName(ns, nadName),
					topology:  types.Layer3Topology,
					subnets:   "192.168.0.0/16",
					extraDest: "192.169.0.0/16,192.170.0.0/16",
				},
				podIP:  "192.168.0.3/16",
				podMAC: "0a:58:c0:a8:00:03",
				podRoute: []util.PodRoute{
					{
						Dest:    testing.MustParseIPNet("192.168.0.0/16"),
						NextHop: testing.MustParseIP("192.168.0.1"),
					},
					{
						Dest:    testing.MustParseIPNet("192.169.0.0/16"),
						NextHop: testing.MustParseIP("192.168.0.1"),
					},
					{
						Dest:    testing.MustParseIPNet("192.170.0.0/16"),
						NextHop: testing.MustParseIP("192.168.0.1"),
					},
				},
			},
		),
		table.Entry("pod on a localnet secondary network",
			podSecondaryNetworkInfo{
				secondaryNetInfo: secondaryNetInfo{
					netName:        secondaryNetworkName,
					nadName:        namespacedName(ns, nadName),
					topology:       types.LocalnetTopology,
					subnets:        "10.129.0.0/16",
					extraDest:      "192.169.0.0/16,192.170.0.0/16",
					excludeSubnets: "10.129.0.1/32",
					gateway:        "10.129.0.1",
				},
				podIP:  "10.129.0.2/16",
				podMAC: "0a:58:0a:81:00:02",
				podRoute: []util.PodRoute{
					{
						Dest:    testing.MustParseIPNet("192.169.0.0/16"),
						NextHop: testing.MustParseIP("10.129.0.1"),
					},
					{
						Dest:    testing.MustParseIPNet("192.170.0.0/16"),
						NextHop: testing.MustParseIP("10.129.0.1"),
					},
				},
			},
		),
	)
})

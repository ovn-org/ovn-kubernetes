// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
)

const (
	uplinkPoll                      = time.Second
	uplinkShortTimeout              = 60 * time.Second
	uplinkConditionStableWindow     = 15 * time.Second
	uplinkTimeout                   = 240 * time.Second
	uplinkCurlMaxTime               = 1
	uplinkDPUGatewayNetworkEnv      = "OVN_TEST_DPU_UPLINK_NETWORK"
	uplinkDPUHostInterfaceNameEnv   = "OVN_TEST_DPU_UPLINK_HOST_INTERFACE"
	uplinkDPUExpectedBridgeEnv      = "OVN_TEST_DPU_UPLINK_EXPECTED_BRIDGE"
	uplinkDPUResourceNameEnv        = "OVN_TEST_DPU_UPLINK_RESOURCE_NAME"
	uplinkDPUResourceNameAnnotation = "k8s.v1.cni.cncf.io/resourceName"
	uplinkDPUHostAddrAnnotation     = "k8s.ovn.org/primary-dpu-host-addr"
	uplinkDPUHostNodeLabel          = "k8s.ovn.org/dpu-host"
	uplinkBGPServerIPv4CIDREnv      = "OVN_TEST_BGP_SERVER_NET_SUBNET_IPV4"
	uplinkBGPServerIPv6CIDREnv      = "OVN_TEST_BGP_SERVER_NET_SUBNET_IPV6"
	uplinkDefaultDPUResourceName    = "dpusim.io/vf"
	uplinkDefaultBGPServerIPv4CIDR  = "172.29.0.0/16"
	uplinkDefaultBGPServerIPv6CIDR  = "fc00:f853:ccd:e797::/64"
	// uplinkPreservedIPv[4|6]CIDR/IP define a destination that is reachable
	// only through the default route pre-installed on the Uplink interface
	// and never advertised over BGP, mimicking a platform where routing state
	// comes from another agent (e.g. a DHCP client) and the BGP session
	// toward the node carries no routes.
	uplinkPreservedIPv4CIDR = "203.0.113.0/24"
	uplinkPreservedIPv4IP   = "203.0.113.1"
	uplinkPreservedIPv6CIDR = "2001:db8:cafe::/64"
	uplinkPreservedIPv6IP   = "2001:db8:cafe::1"
)

// errUplinkStateNotFound distinguishes "the UplinkState does not exist" from
// transient API errors in getUplinkState results.
var errUplinkStateNotFound = errors.New("no matching UplinkState")

// uplinkRouteStableWindow must span at least one VRF manager reconcile period
// (60s) so that holding an assertion for this long proves periodic
// reconciliation does not undo the observed routing state.
const uplinkRouteStableWindow = 90 * time.Second

var uplinkGVR = schema.GroupVersionResource{
	Group:    "k8s.ovn.org",
	Version:  "v1alpha1",
	Resource: "uplinks",
}

var uplinkStateGVR = schema.GroupVersionResource{
	Group:    "k8s.ovn.org",
	Version:  "v1alpha1",
	Resource: "uplinkstates",
}

var uplinkFRRConfigurationGVR = schema.GroupVersionResource{
	Group:    "frrk8s.metallb.io",
	Version:  "v1beta1",
	Resource: "frrconfigurations",
}

type dpuHostAddrAnnotation struct {
	IPv4 string `json:"ipv4"`
	IPv6 string `json:"ipv6"`
}

var _ = ginkgo.Describe("UplinkState API validation", feature.Uplink, func() {
	f := wrappedTestFramework("uplink-validation")
	f.SkipNamespaceCreation = true

	ginkgo.It("accepts up to 256 default gateways across both IP families", func(ctx ginkgo.SpecContext) {
		for _, count := range []int{0, 256, 257} {
			ginkgo.By(fmt.Sprintf("validating %d default gateways with the API server", count))
			gateways := make([]string, 0, count)
			for i := 0; i < count; i++ {
				if i%2 == 0 {
					gateways = append(gateways, fmt.Sprintf("192.0.2.%d", i/2+1))
				} else {
					gateways = append(gateways, fmt.Sprintf("2001:db8::%x", i/2+1))
				}
			}
			state := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "k8s.ovn.org/v1alpha1",
				"kind":       "UplinkState",
				"metadata":   map[string]interface{}{"generateName": "uplink-validation-"},
				"spec":       map[string]interface{}{"uplinkName": "validation", "nodeName": "validation"},
			}}
			gomega.Expect(unstructured.SetNestedStringSlice(state.Object, gateways, "status", "defaultGateways")).To(gomega.Succeed())
			// Dry-run exercises the real CRD schema without publishing synthetic
			// discovery data for the node controllers to consume.
			created, err := f.DynamicClient.Resource(uplinkStateGVR).Create(ctx, state,
				metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			if count > 256 {
				gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "expected a CRD validation error: %v", err)
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("status.defaultGateways"))
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("256"))
				continue
			}
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			actual, found, err := unstructured.NestedStringSlice(created.Object, "status", "defaultGateways")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(found).To(gomega.BeTrue())
			gomega.Expect(actual).To(gomega.Equal(gateways))
		}
	})
})

var _ = ginkgo.Describe("Network Segmentation Uplink default-VRF egress", feature.NetworkSegmentation, feature.Uplink, func() {
	f := wrappedTestFramework("uplink-default")
	f.SkipNamespaceCreation = true

	var ictx infraapi.Context
	var ipFamilySet sets.Set[utilnet.IPFamily]
	var testSuffix string

	ginkgo.BeforeEach(func() {
		if IsGatewayModeLocal(f.ClientSet) {
			e2eskipper.Skipf("Uplink CUDN gateway plumbing is only supported in shared gateway mode")
		}
		if isDPUUplinkE2E() {
			e2eskipper.Skipf("default-VRF Uplink e2e uses regular KIND bridge provisioning")
		}
		ipFamilySet = sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
		ictx = infraprovider.Get().NewTestContext()
		testSuffix = framework.RandomSuffix()
	})

	ginkgo.It("maps multiple CUDNs to the same Uplink bridge", func(ctx ginkgo.SpecContext) {
		nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		schedulableNodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 2)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(schedulableNodes.Items).NotTo(gomega.BeEmpty())

		uplinkAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		uplinkNetwork, nodeIfaces := setupUplinkNetwork(
			ictx,
			nodes.Items,
			ipFamilySet,
			"upnet"+testSuffix,
			[]string{uplinkAlloc.BGPPeerSubnet, uplinkAlloc.BGPPeerSubnet6},
		)

		bridgeName := uplinkBridgeName("updef" + testSuffix)
		gomega.Expect(configureUplinkBridge(ctx, f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())
		gomega.Expect(configureUplinkBridgeDefaultRoutes(
			ictx,
			bridgeName,
			nodeIfaces,
		)).To(gomega.Succeed())

		uplinkName := "uplink" + testSuffix
		createUplink(ctx, f, ictx, uplinkName, nodes.Items, nodeIfaces, bridgeName)
		waitForUplinkStatesResolved(ctx, f, uplinkName, bridgeName, nodes.Items)
		waitForUplinkStatesDefaultGateways(ctx, f, uplinkName, nodes.Items, ipFamilySet)

		serverName := "upsrv" + testSuffix
		server, err := ictx.CreateExternalContainer(infraapi.ExternalContainer{
			Name:    serverName,
			Image:   images.AgnHost(),
			CmdArgs: []string{"netexec"},
			Network: uplinkNetwork,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for i, networkName := range []string{"updefa" + testSuffix, "updefb" + testSuffix} {
			namespace := setupUplinkLayer3CUDN(ctx, f, ictx, ipFamilySet, networkName, uplinkName)

			ginkgo.By("verifying the derived VRF name is published on the CUDN status")
			gomega.Expect(waitForCUDNVRFName(f, networkName)).To(gomega.Equal(networkName),
				"expected the CUDN name to be used as the VRF name since it fits the device name length limit")

			pod := createUplinkNetexecPod(
				ctx,
				f,
				namespace.Name,
				"client-"+networkName,
				schedulableNodes.Items[i%len(schedulableNodes.Items)].Name,
			)
			for _, family := range ipFamilySet.UnsortedList() {
				serverIP := getFirstIPStringOfFamily(family, []string{server.IPv4, server.IPv6})
				gomega.Expect(serverIP).NotTo(gomega.BeEmpty())
				nodeIface, ok := nodeIfaces[pod.Spec.NodeName]
				gomega.Expect(ok).To(gomega.BeTrue(), "expected Uplink interface for node %s", pod.Spec.NodeName)
				expectedSourceIP := getFirstIPStringOfFamily(family, []string{nodeIface.IPv4, nodeIface.IPv6})
				gomega.Expect(expectedSourceIP).NotTo(gomega.BeEmpty())
				uplinkPodToClientIPAndExpect(pod, serverIP, expectedSourceIP)
			}

			// the VRF device is checked on the node running the network's pod:
			// with Dynamic UDN allocation the network is only rendered there
			ginkgo.By("verifying the published VRF name matches a VRF device on the node running the network's pod")
			gomega.Eventually(ctx, func() error {
				return nodeVRFDeviceExists(pod.Spec.NodeName, networkName)
			}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.Succeed(),
				"expected VRF device %s on node %s", networkName, pod.Spec.NodeName)
		}

		ginkgo.By("creating a CUDN whose name exceeds the VRF device name length limit")
		longNetworkName := "updef-with-a-long-name" + testSuffix
		longNamespace := setupUplinkLayer3CUDN(ctx, f, ictx, ipFamilySet, longNetworkName, uplinkName)

		ginkgo.By("verifying the ID-derived VRF name is published on the CUDN status")
		vrfName := waitForCUDNVRFName(f, longNetworkName)
		networkID := getNADNetworkID(ctx, f, longNamespace.Name, longNetworkName)
		gomega.Expect(vrfName).To(gomega.Equal(
			fmt.Sprintf("%s%s%s", ovntypes.UDNVRFDevicePrefix, networkID, ovntypes.UDNVRFDeviceSuffix)),
			"expected the ID-derived VRF name since the CUDN name exceeds the device name length limit")

		ginkgo.By("verifying the ID-derived VRF name matches a VRF device on the node running the network's pod")
		longNamePod := createUplinkNetexecPod(
			ctx,
			f,
			longNamespace.Name,
			"client-"+longNetworkName,
			schedulableNodes.Items[0].Name,
		)
		gomega.Eventually(ctx, func() error {
			return nodeVRFDeviceExists(longNamePod.Spec.NodeName, vrfName)
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.Succeed(),
			"expected VRF device %s on node %s", vrfName, longNamePod.Spec.NodeName)
	})

	ginkgo.It("publishes the heaviest next hops of the lowest-metric default routes", func(ctx ginkgo.SpecContext) {
		nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		uplinkAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, nodeIfaces := setupUplinkNetwork(ictx, nodes.Items, ipFamilySet,
			"upnet"+testSuffix, []string{uplinkAlloc.BGPPeerSubnet, uplinkAlloc.BGPPeerSubnet6})
		bridgeName := uplinkBridgeName("upecmp" + testSuffix)
		gomega.Expect(configureUplinkBridge(ctx, f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())

		gateways := map[utilnet.IPFamily][]string{
			utilnet.IPv4: {"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"},
			utilnet.IPv6: {"2001:db8::1", "2001:db8::2", "2001:db8::3", "2001:db8::4"},
		}
		// Per family: the preferred (metric 50000) route carries a heavy and a
		// light next hop, the backup (metric 50001) route two more. Only the
		// heavy next hop of the preferred route must be published.
		var expectedGateways []string
		for family := range ipFamilySet {
			expectedGateways = append(expectedGateways, gateways[family][0])
		}
		ginkgo.By("installing unequal-weight and higher-metric default routes on the Uplink bridge")
		for _, node := range nodes.Items {
			for family := range ipFamilySet {
				ipCmd := "ip"
				if family == utilnet.IPv6 {
					ipCmd = "ip -6"
				}
				nextHops := gateways[family]
				// These routes test discovery only; deleting the test bridge removes them.
				_, err := execNodeCommand(node.Name,
					"%s route replace default metric 50000 nexthop via %s dev %s weight 4 onlink nexthop via %s dev %s weight 1 onlink",
					ipCmd, nextHops[0], bridgeName, nextHops[1], bridgeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = execNodeCommand(node.Name,
					"%s route replace default metric 50001 nexthop via %s dev %s weight 4 onlink nexthop via %s dev %s weight 1 onlink",
					ipCmd, nextHops[2], bridgeName, nextHops[3], bridgeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
		}

		ginkgo.By("publishing only the heaviest next hops of the lowest-metric defaults")
		uplinkName := "uplink" + testSuffix
		createUplink(ctx, f, ictx, uplinkName, nodes.Items, nodeIfaces, bridgeName)
		waitForUplinkStatesResolved(ctx, f, uplinkName, bridgeName, nodes.Items)
		for _, node := range nodes.Items {
			state, err := getUplinkState(ctx, f, uplinkName, node.Name)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			actual, _, err := unstructured.NestedStringSlice(state.Object, "status", "defaultGateways")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(actual).To(gomega.ConsistOf(expectedGateways))
		}
	})

	ginkgo.It("runs a CUDN without Uplink default gateways and adds its default routes when they appear", func(ctx ginkgo.SpecContext) {
		nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		schedulableNodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 2)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(len(schedulableNodes.Items)).To(gomega.BeNumerically(">=", 2),
			"the east-west check needs pods on two different nodes")

		uplinkAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		uplinkNetwork, nodeIfaces := setupUplinkNetwork(
			ictx,
			nodes.Items,
			ipFamilySet,
			"upnet"+testSuffix,
			[]string{uplinkAlloc.BGPPeerSubnet, uplinkAlloc.BGPPeerSubnet6},
		)

		// Provision the Uplink bridge WITHOUT its default routes: discovery
		// must still resolve the Uplink and publish no default gateways.
		bridgeName := uplinkBridgeName("upgw" + testSuffix)
		gomega.Expect(configureUplinkBridge(ctx, f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())

		uplinkName := "uplink" + testSuffix
		createUplink(ctx, f, ictx, uplinkName, nodes.Items, nodeIfaces, bridgeName)
		waitForUplinkStatesResolved(ctx, f, uplinkName, bridgeName, nodes.Items)
		waitForUplinkStatesNoDefaultGateways(ctx, f, uplinkName, nodes.Items)

		serverName := "upsrv" + testSuffix
		server, err := ictx.CreateExternalContainer(infraapi.ExternalContainer{
			Name:    serverName,
			Image:   images.AgnHost(),
			CmdArgs: []string{"netexec"},
			Network: uplinkNetwork,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		networkName := "upgw" + testSuffix
		bgpAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		networkSpec := uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6)
		namespace, err := createUplinkNamespace(
			ctx,
			f,
			ictx,
			"uplink-default",
			networkName,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(createUplinkCUDN(
			ctx,
			f,
			ictx,
			namespace,
			networkName,
			networkSpec,
			nil,
			uplinkName,
		)).To(gomega.Succeed())

		// The network must come up and carry east-west traffic even without
		// the default gateways: only the default route is missing.
		clientPod := createUplinkNetexecPod(
			ctx,
			f,
			namespace.Name,
			"client-"+networkName,
			schedulableNodes.Items[0].Name,
		)
		serverPod := createUplinkNetexecPod(
			ctx,
			f,
			namespace.Name,
			"server-"+networkName,
			schedulableNodes.Items[1].Name,
		)
		for _, family := range ipFamilySet.UnsortedList() {
			serverPodIP, err := getPodAnnotationIPsForAttachmentByIPFamily(
				f.ClientSet,
				namespace.Name,
				serverPod.Name,
				namespacedName(namespace.Name, networkName),
				family,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(serverPodIP).NotTo(gomega.BeEmpty())
			uplinkPodToHostnameAndExpect(clientPod, serverPodIP, serverPod.Name)
		}

		// A default gateway is not required: an uplink without one is valid
		// (BGP-imported routes can drive external reachability), so no
		// condition degrades. Its absence is visible only as the empty
		// status.defaultGateways field.
		for _, pod := range []*corev1.Pod{clientPod, serverPod} {
			waitForUplinkStateGatewayCondition(
				ctx,
				f,
				uplinkName,
				pod.Spec.NodeName,
				metav1.ConditionTrue,
				uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
			)
		}
		waitForCUDNUplinksReady(ctx, f, networkName)

		// The gateways the restored default routes will carry, per node and
		// family; the gateway router and the managed host VRF default route
		// must not exist before the restore and must appear after it.
		vrfName := waitForCUDNVRFName(f, networkName)
		type nodeGateway struct {
			node, cidr, gateway string
		}
		var nodeGateways []nodeGateway
		for _, pod := range []*corev1.Pod{clientPod, serverPod} {
			iface := nodeIfaces[pod.Spec.NodeName]
			for _, family := range ipFamilySet.UnsortedList() {
				gateway, ip, prefix := iface.IPv4Gateway, iface.IPv4, iface.IPv4Prefix
				if family == utilnet.IPv6 {
					gateway, ip, prefix = iface.IPv6Gateway, iface.IPv6, iface.IPv6Prefix
				}
				gateway, err = interfaceGateway(gateway, ip, prefix)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(gateway).NotTo(gomega.BeEmpty(),
					"expected a %s gateway for node %s", family, pod.Spec.NodeName)
				nodeGateways = append(nodeGateways, nodeGateway{pod.Spec.NodeName, defaultRouteCIDR(family), gateway})
			}
		}
		for _, ng := range nodeGateways {
			gomega.Expect(cudnGRHasRoute(f.ClientSet, networkName, ng.node, ng.cidr, ng.gateway)).To(gomega.BeFalse(),
				"expected no default route via %s on the gateway router of CUDN %s for node %s before the restore",
				ng.gateway, networkName, ng.node)
			gomega.Expect(uplinkRouteShownIn(ng.node, "vrf "+vrfName, ng.cidr, ng.gateway)).NotTo(gomega.Succeed(),
				"expected no default route via %s in host VRF %s on node %s before the restore",
				ng.gateway, vrfName, ng.node)
		}

		// Restore the default routes: discovery re-polls and publishes the
		// gateways, and each network is reprogrammed from the changed
		// UplinkState with its default routes.
		gomega.Expect(configureUplinkBridgeDefaultRoutes(
			ictx,
			bridgeName,
			nodeIfaces,
		)).To(gomega.Succeed())
		waitForUplinkStatesDefaultGateways(ctx, f, uplinkName, nodes.Items, ipFamilySet)
		for _, ng := range nodeGateways {
			ng := ng
			gomega.Eventually(ctx, func() (bool, error) {
				return cudnGRHasRoute(f.ClientSet, networkName, ng.node, ng.cidr, ng.gateway)
			}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.BeTrue(),
				"expected the gateway router of CUDN %s for node %s to acquire a default route via %s",
				networkName, ng.node, ng.gateway)
			gomega.Eventually(ctx, func() error {
				return uplinkRouteShownIn(ng.node, "vrf "+vrfName, ng.cidr, ng.gateway)
			}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.Succeed(),
				"expected the managed default route via %s in host VRF %s on node %s",
				ng.gateway, vrfName, ng.node)
		}

		for _, family := range ipFamilySet.UnsortedList() {
			serverIP := getFirstIPStringOfFamily(family, []string{server.IPv4, server.IPv6})
			gomega.Expect(serverIP).NotTo(gomega.BeEmpty())
			nodeIface, ok := nodeIfaces[clientPod.Spec.NodeName]
			gomega.Expect(ok).To(gomega.BeTrue(), "expected Uplink interface for node %s", clientPod.Spec.NodeName)
			expectedSourceIP := getFirstIPStringOfFamily(family, []string{nodeIface.IPv4, nodeIface.IPv6})
			gomega.Expect(expectedSourceIP).NotTo(gomega.BeEmpty())
			uplinkPodToClientIPAndExpect(clientPod, serverIP, expectedSourceIP)
		}
	})

	ginkgo.It("recreates an UplinkState deleted out of band", func(ctx ginkgo.SpecContext) {
		env := provisionUplinkWithActiveCUDN(ctx, f, ictx, ipFamilySet, testSuffix, "updel")
		node, uplinkName, bridgeName, networkName := env.node, env.uplinkName, env.bridgeName, env.networkName

		ginkgo.By("deleting the UplinkState out of band")
		state, err := getUplinkState(ctx, f, uplinkName, node.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deletedUID := state.GetUID()
		gomega.Expect(f.DynamicClient.Resource(uplinkStateGVR).Delete(
			ctx,
			state.GetName(),
			metav1.DeleteOptions{},
		)).To(gomega.Succeed())

		ginkgo.By("waiting for a new UplinkState without restarting ovnkube-node")
		gomega.Eventually(ctx, func() error {
			state, err := getUplinkState(ctx, f, uplinkName, node.Name)
			if err != nil {
				return err
			}
			if state.GetUID() == deletedUID {
				return fmt.Errorf("UplinkState %s still carries the deleted object's UID %s",
					state.GetName(), deletedUID)
			}
			return nil
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected the UplinkState for uplink %q on node %q to be recreated",
			uplinkName,
			node.Name,
		)

		ginkgo.By("waiting for the recreated UplinkState to recover discovery and gateway readiness")
		waitForUplinkStatesResolved(ctx, f, uplinkName, bridgeName, []corev1.Node{node})
		waitForUplinkStateGatewayCondition(
			ctx,
			f,
			uplinkName,
			node.Name,
			metav1.ConditionTrue,
			uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
		)
		waitForCUDNUplinksReady(ctx, f, networkName)
	})

	// An out-of-band deletion requeues the UDN, which can preserve unchanged
	// dataplane while reporting against the new UplinkState. Intentional
	// deselection withdraws that dataplane. In both cases, a recreated object
	// must receive a newly evaluated condition rather than inherit the old one.
	ginkgo.It("does not restore gateway readiness on an UplinkState recreated after deselection", func(ctx ginkgo.SpecContext) {
		env := provisionUplinkWithActiveCUDN(ctx, f, ictx, ipFamilySet, testSuffix, "updesel")
		node, uplinkName, bridgeName := env.node, env.uplinkName, env.bridgeName
		hostname, ok := node.Labels[corev1.LabelHostname]
		gomega.Expect(ok).To(gomega.BeTrue(), "expected node %s to have label %q", node.Name, corev1.LabelHostname)

		state, err := getUplinkState(ctx, f, uplinkName, node.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deselectedUID := state.GetUID()
		gatewayReady, err := uplinkStateCondition(state, uplinkv1alpha1.UplinkStateConditionGatewayReady)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		previousTransition := gatewayReady.LastTransitionTime

		ginkgo.By("deselecting the node from the Uplink")
		deselectedHostname := "deselected-" + testSuffix
		setUplinkNodeConfigHostname(ctx, f, uplinkName, hostname, deselectedHostname)
		gomega.Eventually(ctx, func() error {
			_, err := getUplinkState(ctx, f, uplinkName, node.Name)
			if errors.Is(err, errUplinkStateNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			return fmt.Errorf("UplinkState for uplink %q on node %q still exists", uplinkName, node.Name)
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected the UplinkState for uplink %q on node %q to be deleted after deselection",
			uplinkName,
			node.Name,
		)

		ginkgo.By("reselecting the node and waiting for a new UplinkState to resolve")
		setUplinkNodeConfigHostname(ctx, f, uplinkName, deselectedHostname, hostname)
		gomega.Eventually(ctx, func() error {
			state, err := getUplinkState(ctx, f, uplinkName, node.Name)
			if err != nil {
				return err
			}
			if state.GetUID() == deselectedUID {
				return fmt.Errorf("UplinkState %s still carries the deselected object's UID %s",
					state.GetName(), deselectedUID)
			}
			return nil
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected the UplinkState for uplink %q on node %q to be recreated after reselection",
			uplinkName,
			node.Name,
		)
		waitForUplinkStatesResolved(ctx, f, uplinkName, bridgeName, []corev1.Node{node})

		ginkgo.By("verifying the recreated UplinkState does not inherit the previous GatewayReady")
		gomega.Consistently(ctx, func() error {
			state, err := getUplinkState(ctx, f, uplinkName, node.Name)
			if err != nil {
				return err
			}
			conditions, err := getConditions(state)
			if err != nil {
				return err
			}
			for _, condition := range conditions {
				if condition.Type != uplinkv1alpha1.UplinkStateConditionGatewayReady ||
					condition.Status != metav1.ConditionTrue {
					continue
				}
				// Any GatewayReady=True evaluated for the recreated object
				// transitions after the deselection.
				if !condition.LastTransitionTime.After(previousTransition.Time) {
					return fmt.Errorf(
						"UplinkState %s carries GatewayReady=True from before the deselection (transitioned %s)",
						state.GetName(),
						condition.LastTransitionTime,
					)
				}
			}
			return nil
		}).WithTimeout(uplinkConditionStableWindow).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected the recreated UplinkState not to inherit gateway readiness from the previous lifecycle",
		)
	})

	ginkgo.It("reprograms an active CUDN when the selected Uplink configuration changes", func(ctx ginkgo.SpecContext) {
		env := provisionUplinkWithActiveCUDN(ctx, f, ictx, ipFamilySet, testSuffix, "upchange")
		node, uplinkName, networkName := env.node, env.uplinkName, env.networkName
		hostname, ok := node.Labels[corev1.LabelHostname]
		gomega.Expect(ok).To(gomega.BeTrue(), "expected node %s to have label %q", node.Name, corev1.LabelHostname)

		state, err := getUplinkState(ctx, f, uplinkName, node.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(),
			"expected to get UplinkState for Uplink %s on node %s", uplinkName, node.Name)
		initialUID := state.GetUID()
		expectSingleUplinkLocalnetPort(ctx, f, node.Name, networkName, env.bridgeName)

		ginkgo.By("verifying egress through the initial Uplink bridge")
		initialServer, err := ictx.CreateExternalContainer(infraapi.ExternalContainer{
			Name:    "upchangesrva" + testSuffix,
			Image:   images.AgnHost(),
			CmdArgs: []string{"netexec"},
			Network: env.uplinkNetwork,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred(),
			"expected to create the initial Uplink egress test server")
		initialNodeIface, ok := env.nodeIfaces[node.Name]
		gomega.Expect(ok).To(gomega.BeTrue(), "expected initial Uplink interface for node %s", node.Name)
		for _, family := range ipFamilySet.UnsortedList() {
			serverIP := getFirstIPStringOfFamily(family, []string{initialServer.IPv4, initialServer.IPv6})
			gomega.Expect(serverIP).NotTo(gomega.BeEmpty(),
				"expected initial Uplink server address for IP family %s", family)
			expectedSourceIP := getFirstIPStringOfFamily(family, []string{initialNodeIface.IPv4, initialNodeIface.IPv6})
			gomega.Expect(expectedSourceIP).NotTo(gomega.BeEmpty(),
				"expected initial Uplink source address for IP family %s", family)
			uplinkPodToClientIPAndExpect(env.pod, serverIP, expectedSourceIP)
		}

		ginkgo.By("provisioning a replacement Uplink bridge and underlay")
		replacementAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(),
			"expected to allocate the replacement Uplink underlay")
		replacementNetwork, replacementIfaces := setupUplinkNetwork(
			ictx,
			[]corev1.Node{node},
			ipFamilySet,
			"upchangenet"+testSuffix,
			[]string{replacementAlloc.BGPPeerSubnet, replacementAlloc.BGPPeerSubnet6},
		)
		replacementBridge := uplinkBridgeName("upchangerepl" + testSuffix)
		gomega.Expect(configureUplinkBridge(ctx, f, ictx, replacementBridge, replacementIfaces)).To(
			gomega.Succeed(), "expected to configure replacement Uplink bridge %s", replacementBridge)
		gomega.Expect(configureUplinkBridgeDefaultRoutes(
			ictx,
			replacementBridge,
			replacementIfaces,
		)).To(gomega.Succeed(), "expected to configure default routes on replacement Uplink bridge %s",
			replacementBridge)
		replacementServer, err := ictx.CreateExternalContainer(infraapi.ExternalContainer{
			Name:    "upchangesrvb" + testSuffix,
			Image:   images.AgnHost(),
			CmdArgs: []string{"netexec"},
			Network: replacementNetwork,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred(),
			"expected to create the replacement Uplink egress test server")

		ginkgo.By("changing the selected nodeConfig without deselecting the node")
		setUplinkNodeConfigHostInterfaceName(ctx, f, uplinkName, hostname, replacementBridge)
		waitForUplinkStatesResolved(ctx, f, uplinkName, replacementBridge, []corev1.Node{node})
		expectSingleUplinkLocalnetPort(ctx, f, node.Name, networkName, replacementBridge)

		ginkgo.By("verifying egress uses the replacement Uplink interface")
		replacementNodeIface, ok := replacementIfaces[node.Name]
		gomega.Expect(ok).To(gomega.BeTrue(), "expected replacement Uplink interface for node %s", node.Name)
		for _, family := range ipFamilySet.UnsortedList() {
			serverIP := getFirstIPStringOfFamily(family, []string{replacementServer.IPv4, replacementServer.IPv6})
			gomega.Expect(serverIP).NotTo(gomega.BeEmpty(),
				"expected replacement Uplink server address for IP family %s", family)
			expectedSourceIP := getFirstIPStringOfFamily(family, []string{replacementNodeIface.IPv4, replacementNodeIface.IPv6})
			gomega.Expect(expectedSourceIP).NotTo(gomega.BeEmpty(),
				"expected replacement Uplink source address for IP family %s", family)
			uplinkPodToClientIPAndExpect(env.pod, serverIP, expectedSourceIP)
		}

		ginkgo.By("verifying gateway readiness remains published after reprogramming")
		gomega.Eventually(ctx, func() error {
			state, err := getUplinkState(ctx, f, uplinkName, node.Name)
			if err != nil {
				return err
			}
			if state.GetUID() != initialUID {
				return fmt.Errorf("UplinkState %s was unexpectedly recreated", state.GetName())
			}
			gatewayReady, err := uplinkStateCondition(state, uplinkv1alpha1.UplinkStateConditionGatewayReady)
			if err != nil {
				return err
			}
			if gatewayReady.Status != metav1.ConditionTrue ||
				gatewayReady.Reason != uplinkv1alpha1.UplinkStateReasonGatewayConfigured {
				return fmt.Errorf("UplinkState %s GatewayReady is %s/%s, expected %s/%s",
					state.GetName(),
					gatewayReady.Status,
					gatewayReady.Reason,
					metav1.ConditionTrue,
					uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
				)
			}
			return nil
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected gateway readiness to remain published after replacement Uplink programming",
		)
		waitForCUDNUplinksReady(ctx, f, networkName)
	})
})

// uplinkRecoveryEnv is the provisioning shared by the UplinkState recovery
// tests: one Uplink resolved on every node, and one CUDN activated on a single
// schedulable node with gateway readiness published for it.
type uplinkRecoveryEnv struct {
	node          corev1.Node
	uplinkName    string
	bridgeName    string
	networkName   string
	uplinkNetwork infraapi.Network
	nodeIfaces    map[string]infraapi.NetworkInterface
	pod           *corev1.Pod
}

func provisionUplinkWithActiveCUDN(
	ctx context.Context,
	f *framework.Framework,
	ictx infraapi.Context,
	ipFamilySet sets.Set[utilnet.IPFamily],
	testSuffix string,
	prefix string,
) uplinkRecoveryEnv {
	ginkgo.GinkgoHelper()

	nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	schedulableNodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 1)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(schedulableNodes.Items).NotTo(gomega.BeEmpty())
	node := schedulableNodes.Items[0]

	uplinkAlloc, err := allocators.AllocateBGP(f, ictx)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	uplinkNetwork, nodeIfaces := setupUplinkNetwork(
		ictx,
		nodes.Items,
		ipFamilySet,
		"upnet"+testSuffix,
		[]string{uplinkAlloc.BGPPeerSubnet, uplinkAlloc.BGPPeerSubnet6},
	)

	bridgeName := uplinkBridgeName(prefix + testSuffix)
	gomega.Expect(configureUplinkBridge(ctx, f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())
	gomega.Expect(configureUplinkBridgeDefaultRoutes(
		ictx,
		bridgeName,
		nodeIfaces,
	)).To(gomega.Succeed())

	uplinkName := prefix + testSuffix
	createUplink(ctx, f, ictx, uplinkName, nodes.Items, nodeIfaces, bridgeName)
	waitForUplinkStatesResolved(ctx, f, uplinkName, bridgeName, nodes.Items)

	ginkgo.By("activating a CUDN on the Uplink so GatewayReady is published")
	networkName := prefix + "net" + testSuffix
	namespace := setupUplinkLayer3CUDN(ctx, f, ictx, ipFamilySet, networkName, uplinkName)
	pod := createUplinkNetexecPod(ctx, f, namespace.Name, "client-"+networkName, node.Name)
	waitForUplinkStateGatewayCondition(
		ctx,
		f,
		uplinkName,
		node.Name,
		metav1.ConditionTrue,
		uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
	)
	waitForCUDNUplinksReady(ctx, f, networkName)

	return uplinkRecoveryEnv{
		node:          node,
		uplinkName:    uplinkName,
		bridgeName:    bridgeName,
		networkName:   networkName,
		uplinkNetwork: uplinkNetwork,
		nodeIfaces:    nodeIfaces,
		pod:           pod,
	}
}

var _ = ginkgo.Describe("Network Segmentation Uplink DPU VF lifecycle", feature.NetworkSegmentation, feature.Uplink, func() {
	f := wrappedTestFramework("uplink-dpu-vf")
	f.SkipNamespaceCreation = true

	var ictx infraapi.Context
	var ipFamilySet sets.Set[utilnet.IPFamily]
	var testSuffix string

	ginkgo.BeforeEach(func() {
		if !isDPUUplinkE2E() {
			e2eskipper.Skipf("test requires the simulated DPU Uplink environment")
		}
		if IsGatewayModeLocal(f.ClientSet) {
			e2eskipper.Skipf("Uplink CUDN gateway plumbing is only supported in shared gateway mode")
		}
		ipFamilySet = sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
		ictx = infraprovider.Get().NewTestContext()
		testSuffix = framework.RandomSuffix()
	})

	// Simulated VFs are veths: an attachment netdevice that is not returned to
	// the host before the pod namespace is destroyed is lost together with its
	// DPU-side representor peer, permanently breaking any future pod assigned
	// the same VF. Assert the CNI DEL contract directly: after pod deletion,
	// every VF netdevice recorded in the pod's DPU connection-details (the
	// default network's and the primary UDN's) is present again on the host.
	ginkgo.It("returns simulated VF netdevices to the host when the pod is deleted", func(ctx ginkgo.SpecContext) {
		schedulableNodes, err := e2enode.GetReadySchedulableNodes(ctx, f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodes := filterNodesByLabel(schedulableNodes.Items, uplinkDPUHostNodeLabel)
		gomega.Expect(nodes).NotTo(gomega.BeEmpty(), "test requires a DPU host node")
		nodes = nodes[:1]
		node := nodes[0]

		nodeIfaces := collectDPUHostUplinkInterfaces(nodes)
		bridgeName := os.Getenv(uplinkDPUExpectedBridgeEnv)
		gomega.Expect(bridgeName).NotTo(gomega.BeEmpty(), "expected the DPU Uplink bridge name")

		uplinkName := "upvf" + testSuffix
		createUplink(ctx, f, ictx, uplinkName, nodes, nodeIfaces, "")
		waitForUplinkStatesResolved(ctx, f, uplinkName, bridgeName, nodes)

		networkName := "upvfnet" + testSuffix
		bgpAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		namespace, err := createUplinkNamespace(ctx, f, ictx, "uplink-dpu-vf", networkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(createUplinkCUDN(
			ctx,
			f,
			ictx,
			namespace,
			networkName,
			uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6),
			nil,
			uplinkName,
		)).To(gomega.Succeed())

		pod := createUplinkNetexecPod(ctx, f, namespace.Name, "client-"+networkName, node.Name)

		pod, err = f.ClientSet.CoreV1().Pods(namespace.Name).Get(ctx, pod.Name, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		connDetails := pod.Annotations["k8s.ovn.org/dpu.connection-details"]
		gomega.Expect(connDetails).NotTo(gomega.BeEmpty(), "expected the DPU connection-details annotation on the pod")
		var attachments map[string]struct {
			VfNetdevName string `json:"vfNetdevName"`
		}
		gomega.Expect(json.Unmarshal([]byte(connDetails), &attachments)).To(gomega.Succeed())
		gomega.Expect(attachments).To(gomega.HaveLen(2),
			"expected connection details for the default network and the primary UDN, got: %s", connDetails)
		netdevs := make([]string, 0, len(attachments))
		for nadKey, attachment := range attachments {
			gomega.Expect(attachment.VfNetdevName).NotTo(gomega.BeEmpty(),
				"expected a VF netdevice name for NAD %s", nadKey)
			netdevs = append(netdevs, attachment.VfNetdevName)
		}

		ginkgo.By(fmt.Sprintf("deleting the pod and expecting VF netdevices %v to return to node %s", netdevs, node.Name))
		gomega.Expect(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)).To(gomega.Succeed())

		for _, netdev := range netdevs {
			gomega.Eventually(ctx, func() error {
				_, err := infraprovider.Get().ExecK8NodeCommand(node.Name, []string{"ip", "link", "show", "dev", netdev})
				return err
			}).WithTimeout(uplinkShortTimeout).WithPolling(uplinkPoll).Should(gomega.Succeed(),
				"expected simulated VF netdevice %s to be returned to the host on node %s", netdev, node.Name)
		}
	})

	// The first ovnkube-node start renames the management port VF to the
	// canonical management port name, so a restart must resolve the device
	// through its link alias: with name-based resolution only (simulated
	// devices), the restarted ovnkube crashloops and the pod never becomes
	// Ready again. No CI lane restarts ovnkube on a DPU host otherwise, which
	// is how that regression previously went unnoticed.
	ginkgo.It("recovers the management port device after an ovnkube-node restart", ginkgo.Serial, func(ctx ginkgo.SpecContext) {
		schedulableNodes, err := e2enode.GetReadySchedulableNodes(ctx, f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodes := filterNodesByLabel(schedulableNodes.Items, uplinkDPUHostNodeLabel)
		gomega.Expect(nodes).NotTo(gomega.BeEmpty(), "test requires a DPU host node")
		node := nodes[0]

		// A primary UDN active on the node gives the restarted ovnkube a
		// second management port to re-resolve besides the default network's;
		// its netdevice was also renamed by the first instance, and a failure
		// here takes the node controller down for all networks.
		networkName := "updnrst" + testSuffix
		bgpAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		namespace, err := createUplinkNamespace(ctx, f, ictx, "uplink-dpu-restart", networkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(createUplinkPrimaryUDN(
			ctx,
			f,
			namespace.Name,
			networkName,
			uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6),
		)).To(gomega.Succeed())
		udnPod := createUplinkNetexecPod(ctx, f, namespace.Name, "client-"+networkName, node.Name)

		ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
		listOnNode := metav1.ListOptions{
			LabelSelector: "app=ovnkube-node-dpu-host",
			FieldSelector: "spec.nodeName=" + node.Name,
		}
		pods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(ctx, listOnNode)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pods.Items).To(gomega.HaveLen(1), "expected one ovnkube-node-dpu-host pod on node %s", node.Name)
		oldPod := pods.Items[0]

		ginkgo.By(fmt.Sprintf("restarting ovnkube-node pod %s on node %s", oldPod.Name, node.Name))
		gomega.Expect(e2epod.DeletePodWithWait(ctx, f.ClientSet, &oldPod)).To(gomega.Succeed())

		gomega.Eventually(ctx, func() error {
			pods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(ctx, listOnNode)
			if err != nil {
				return err
			}
			if len(pods.Items) != 1 {
				return fmt.Errorf("expected one ovnkube-node-dpu-host pod on node %s, got %d", node.Name, len(pods.Items))
			}
			pod := pods.Items[0]
			if pod.UID == oldPod.UID {
				return fmt.Errorf("pod %s not recreated yet", pod.Name)
			}
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					return nil
				}
			}
			return fmt.Errorf("pod %s (restarts: %d) is not ready", pod.Name, podTotalRestarts(&pod))
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.Succeed(),
			"expected the restarted ovnkube-node pod on node %s to become ready", node.Name)

		udnPod, err = f.ClientSet.CoreV1().Pods(namespace.Name).Get(ctx, udnPod.Name, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(udnPod.Status.Phase).To(gomega.Equal(corev1.PodRunning),
			"expected the UDN pod to survive the ovnkube-node restart")
	})
})

var _ = ginkgo.Describe("Network Segmentation Uplink route advertisements", feature.NetworkSegmentation, feature.RouteAdvertisements, feature.Uplink, func() {
	f := wrappedTestFramework("uplink-bgp")
	f.SkipNamespaceCreation = true

	var ictx infraapi.Context
	var ipFamilySet sets.Set[utilnet.IPFamily]
	var testSuffix string

	ginkgo.BeforeEach(func() {
		if IsGatewayModeLocal(f.ClientSet) {
			e2eskipper.Skipf("Uplink CUDN gateway plumbing is only supported in shared gateway mode")
		}
		ipFamilySet = sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
		ictx = infraprovider.Get().NewTestContext()
		testSuffix = framework.RandomSuffix()
	})

	// The NetworkManager addressing shape is not exercised here: on full-mode
	// clusters the route-preservation table covers both addressing shapes.
	ginkgo.It("uses the Uplink interface as the targetVRF auto BGP peering path", func(ctx ginkgo.SpecContext) {
		if isDPUUplinkE2E() {
			e2eskipper.Skipf("full-mode Uplink bridge provisioning; the split DPU mode variant covers DPU")
		}
		nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		schedulableNodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 2)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(schedulableNodes.Items).NotTo(gomega.BeEmpty())

		bgpAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		networkName := "upvrf" + testSuffix
		serverName := networkName + "-srv"
		serverNetworkName := serverName
		peerCIDRs := []string{bgpAlloc.BGPPeerSubnet, bgpAlloc.BGPPeerSubnet6}
		serverCIDRs := []string{bgpAlloc.IPVRFSubnet, bgpAlloc.IPVRFSubnet6}

		gomega.Expect(runBGPNetworkAndServer(
			f,
			ictx,
			ipFamilySet,
			networkName,
			serverName,
			serverNetworkName,
			peerCIDRs,
			serverCIDRs,
		)).To(gomega.Succeed())

		peerNetwork, err := infraprovider.Get().GetNetwork(networkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeIfaces := collectNodeNetworkInterfaces(nodes.Items, peerNetwork)

		bridgeName := uplinkBridgeName("upvrf" + testSuffix)
		gomega.Expect(configureUplinkBridge(ctx, f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())

		createUplink(ctx, f, ictx, networkName, nodes.Items, nodeIfaces, bridgeName)
		waitForUplinkStatesResolved(ctx, f, networkName, bridgeName, nodes.Items)

		networkSpec := uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6)
		namespace, err := createUplinkAdvertisedCUDN(
			ctx,
			f,
			ictx,
			networkName,
			networkSpec,
			networkName,
			"auto",
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		var pods []*corev1.Pod
		for _, node := range schedulableNodes.Items {
			pods = append(pods, createUplinkNetexecPod(
				ctx,
				f,
				namespace.Name,
				"client-"+networkName+"-"+node.Name,
				node.Name,
			))
		}
		pod := pods[0]
		serverNetwork, err := infraprovider.Get().GetNetwork(serverNetworkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		serverIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
			infraapi.ExternalContainer{Name: serverName},
			serverNetwork,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		frrIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
			infraapi.ExternalContainer{Name: networkName + "-frr"},
			peerNetwork,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, family := range ipFamilySet.UnsortedList() {
			serverCIDR := getFirstCIDRStringOfFamily(family, serverCIDRs)
			gomega.Expect(serverCIDR).NotTo(gomega.BeEmpty())
			frrIP := getFirstIPStringOfFamily(family, []string{frrIface.IPv4, frrIface.IPv6})
			gomega.Expect(frrIP).NotTo(gomega.BeEmpty())
			for _, node := range schedulableNodes.Items {
				gomega.Eventually(ctx, func() (bool, error) {
					return hasRouteInCUDNVRF(node, networkName, serverCIDR, bgpNextHopsForPeer(family, frrIface)...)
				}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
					gomega.BeTrue(),
					"expected node %s to learn %s via %s in CUDN VRF %s",
					node.Name,
					serverCIDR,
					frrIP,
					networkName,
				)
			}

			serverIP := getFirstIPStringOfFamily(family, []string{serverIface.IPv4, serverIface.IPv6})
			gomega.Expect(serverIP).NotTo(gomega.BeEmpty())
			uplinkPodToHostnameAndExpect(pod, serverIP, serverName)
			podIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
				f.ClientSet,
				pod.Namespace,
				pod.Name,
				networkName,
				family,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(podIP).NotTo(gomega.BeEmpty())
			uplinkPodToClientIPAndExpect(pod, serverIP, podIP)
		}
	})

	// Both addressing shapes of the host-side Uplink VF are covered:
	// kernel-owned, and NetworkManager-style (noprefixroute addresses with
	// userspace-installed proto kernel prefix routes), which the VF has when
	// NetworkManager manages it.
	ginkgo.DescribeTable("uses the Uplink interface as the targetVRF auto BGP peering path in split DPU mode", func(ctx ginkgo.SpecContext, nmStyleAddressing bool) {
		if !isDPUUplinkE2E() {
			e2eskipper.Skipf("requires a split DPU deployment")
		}
		schedulableNodes, err := e2enode.GetReadySchedulableNodes(ctx, f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(schedulableNodes.Items).NotTo(gomega.BeEmpty())
		runDPUUplinkVRFLiteRouteAdvertisements(
			ctx,
			f,
			ictx,
			schedulableNodes.Items,
			ipFamilySet,
			testSuffix,
			nmStyleAddressing,
		)
	},
		ginkgo.Entry("with kernel-owned interface addressing", false),
		ginkgo.Entry("with NetworkManager-style interface addressing", true),
	)

	// The two addressing shapes cover both route-preservation regimes: with
	// kernel-owned addressing the kernel regenerates the interface prefix
	// routes in the target table itself and the migration tolerates them,
	// while with NetworkManager-style addressing (noprefixroute, prefix
	// routes installed from userspace as proto kernel) only the migration
	// can carry the prefix routes across, and the default routes depend on
	// them for their gateway resolution.
	ginkgo.DescribeTable("preserves pre-existing Uplink interface routes across VRF enslavement and release", func(ctx ginkgo.SpecContext, nmStyleAddressing bool) {
		if isDPUUplinkE2E() {
			e2eskipper.Skipf("preserved-route Uplink e2e uses regular KIND bridge provisioning")
		}
		nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		schedulableNodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 2)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(schedulableNodes.Items).NotTo(gomega.BeEmpty())

		bgpAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		networkName := "uppre" + testSuffix
		serverName := networkName + "-srv"
		serverNetworkName := serverName
		peerCIDRs := []string{bgpAlloc.BGPPeerSubnet, bgpAlloc.BGPPeerSubnet6}
		serverCIDRs := []string{bgpAlloc.IPVRFSubnet, bgpAlloc.IPVRFSubnet6}

		gomega.Expect(runBGPNetworkAndServer(
			f,
			ictx,
			ipFamilySet,
			networkName,
			serverName,
			serverNetworkName,
			peerCIDRs,
			serverCIDRs,
		)).To(gomega.Succeed())

		peerNetwork, err := infraprovider.Get().GetNetwork(networkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeIfaces := collectNodeNetworkInterfaces(nodes.Items, peerNetwork)

		bridgeName := uplinkBridgeName("uppre" + testSuffix)
		gomega.Expect(configureUplinkBridge(ctx, f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())

		nodeNames := make([]string, 0, len(nodeIfaces))
		for nodeName := range nodeIfaces {
			nodeNames = append(nodeNames, nodeName)
		}

		if nmStyleAddressing {
			ginkgo.By("reshaping the Uplink bridge addressing to NetworkManager form")
			bridgeByNode := make(map[string]string, len(nodeIfaces))
			for nodeName := range nodeIfaces {
				bridgeByNode[nodeName] = bridgeName
			}
			gomega.Expect(configureUplinkNMStyleAddressing(ictx, bridgeByNode)).To(gomega.Succeed())
		}

		server := infraapi.ExternalContainer{Name: serverName}
		frr := infraapi.ExternalContainer{Name: networkName + "-frr"}
		serverNetwork, err := infraprovider.Get().GetNetwork(serverNetworkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		serverIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(server, serverNetwork)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		frrIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(frr, peerNetwork)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		preservedFor := func(family utilnet.IPFamily) (cidr, ip, frrIP string) {
			cidr, ip = uplinkPreservedIPv4CIDR, uplinkPreservedIPv4IP
			if family == utilnet.IPv6 {
				cidr, ip = uplinkPreservedIPv6CIDR, uplinkPreservedIPv6IP
			}
			frrIP = getFirstIPStringOfFamily(family, []string{frrIface.IPv4, frrIface.IPv6})
			gomega.Expect(frrIP).NotTo(gomega.BeEmpty())
			return cidr, ip, frrIP
		}

		prefixCIDRFor := func(iface infraapi.NetworkInterface, family utilnet.IPFamily) string {
			ip, prefix := iface.IPv4, iface.IPv4Prefix
			if family == utilnet.IPv6 {
				ip, prefix = iface.IPv6, iface.IPv6Prefix
			}
			gomega.Expect(ip).NotTo(gomega.BeEmpty())
			_, ipNet, err := net.ParseCIDR(ip + "/" + prefix)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return ipNet.String()
		}

		ginkgo.By("serving a prefix from the BGP server without advertising it and installing default routes on the Uplink bridge")
		// Serve the preserved prefix from the BGP server without advertising
		// it over BGP, and install a default route through the FRR peer on
		// the Uplink bridge before any enslavement. The default route is the
		// state another agent (e.g. a DHCP client) would have configured on
		// the interface, and the only path toward the preserved prefix.
		for _, family := range ipFamilySet.UnsortedList() {
			preservedCIDR, preservedIP, frrIP := preservedFor(family)
			serverIP := getFirstIPStringOfFamily(family, []string{serverIface.IPv4, serverIface.IPv6})
			gomega.Expect(serverIP).NotTo(gomega.BeEmpty())
			hostMask := "/32"
			ipCmd := "ip"
			if family == utilnet.IPv6 {
				hostMask = "/128"
				ipCmd = "ip -6"
			}
			_, err = infraprovider.Get().ExecExternalContainerCommand(server, []string{
				"sh", "-c", fmt.Sprintf("%s addr add %s%s dev lo", ipCmd, preservedIP, hostMask),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = infraprovider.Get().ExecExternalContainerCommand(frr, []string{
				"sh", "-c", fmt.Sprintf("%s route add %s via %s", ipCmd, preservedCIDR, serverIP),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(configureUplinkStaticRoute(
				ictx,
				bridgeName,
				nodeNames,
				defaultRouteCIDR(family),
				frrIP,
				false,
				"",
			)).To(gomega.Succeed())
		}

		ginkgo.By("creating the Uplink and waiting for gateway discovery")
		uplinkName := networkName
		createUplink(ctx, f, ictx, uplinkName, nodes.Items, nodeIfaces, bridgeName)
		waitForUplinkStatesResolved(ctx, f, uplinkName, bridgeName, nodes.Items)
		// gateway discovery consumed the pre-existing default route
		waitForUplinkStatesDefaultGateways(ctx, f, uplinkName, nodes.Items, ipFamilySet)

		ginkgo.By("creating the advertised CUDN backed by the Uplink")
		networkLabels := map[string]string{"advertise": networkName}
		networkSpec := uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6)
		namespace, err := createUplinkNamespace(ctx, f, ictx, "uplink-bgp", networkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(createUplinkCUDN(
			ctx,
			f,
			ictx,
			namespace,
			networkName,
			networkSpec,
			networkLabels,
			uplinkName,
		)).To(gomega.Succeed())
		if isDynamicUDNEnabled() {
			ginkgo.By("activating the dynamic CUDN on the nodes under test")
			for i, node := range schedulableNodes.Items {
				createUplinkNetexecPod(
					ctx,
					f,
					namespace.Name,
					fmt.Sprintf("activate-%s-%d", networkName, i),
					node.Name,
				)
			}
		}
		gomega.Expect(createRouteAdvertisements(
			f,
			ictx,
			networkName,
			"auto",
			networkLabels,
			map[string]string{"network": networkName},
		)).To(gomega.Succeed())

		ginkgo.By("waiting for the pre-existing default routes to be migrated into the CUDN VRF")
		// Enslaving the Uplink interface into the CUDN VRF must migrate its
		// pre-existing default route into the VRF routing table instead of
		// letting the kernel discard it.
		for _, family := range ipFamilySet.UnsortedList() {
			_, _, frrIP := preservedFor(family)
			for _, node := range schedulableNodes.Items {
				node := node
				gomega.Eventually(ctx, func() error {
					return uplinkRouteShownIn(node.Name, "vrf "+networkName, defaultRouteCIDR(family), frrIP)
				}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
					gomega.Succeed(),
					"expected preserved default route via %s in CUDN VRF %s on node %s",
					frrIP,
					networkName,
					node.Name,
				)
			}
		}

		ginkgo.By("waiting for the interface prefix routes to be present in the CUDN VRF")
		// With kernel-owned addressing the kernel regenerates the prefix
		// routes in the VRF table itself; with NetworkManager-style
		// addressing only the migration can put them there. Either way the
		// default routes above depend on them for their gateway resolution.
		for _, family := range ipFamilySet.UnsortedList() {
			for _, node := range schedulableNodes.Items {
				node := node
				prefixCIDR := prefixCIDRFor(nodeIfaces[node.Name], family)
				gomega.Eventually(ctx, func() error {
					return uplinkKernelRouteShownIn(node.Name, "vrf "+networkName, prefixCIDR)
				}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
					gomega.Succeed(),
					"expected prefix route %s in CUDN VRF %s on node %s",
					prefixCIDR,
					networkName,
					node.Name,
				)
			}
		}

		ginkgo.By("verifying VRF connectivity through the preserved default routes")
		// Host traffic in the CUDN VRF rides the preserved default route:
		// nothing advertises the preserved prefix over BGP, matching a
		// platform where the BGP session toward the node carries no routes.
		for _, family := range ipFamilySet.UnsortedList() {
			_, preservedIP, _ := preservedFor(family)
			for _, node := range schedulableNodes.Items {
				node := node
				gomega.Eventually(ctx, func() error {
					return uplinkHostVRFTCPProbe(node.Name, networkName, preservedIP, netexecPort)
				}).WithTimeout(uplinkShortTimeout).WithPolling(uplinkPoll).Should(
					gomega.Succeed(),
					"expected node %s to reach %s through the preserved default route in VRF %s",
					node.Name,
					preservedIP,
					networkName,
				)
			}
		}

		ginkgo.By("holding the preserved routes through a full reconcile period")
		// The migrated route is not owned by any ovnkube manager: make sure
		// periodic reconciliation does not clean it up. The window spans at
		// least one full VRF manager reconcile period.
		gomega.Consistently(ctx, func() error {
			for _, family := range ipFamilySet.UnsortedList() {
				_, _, frrIP := preservedFor(family)
				for _, node := range schedulableNodes.Items {
					if err := uplinkRouteShownIn(node.Name, "vrf "+networkName, defaultRouteCIDR(family), frrIP); err != nil {
						return err
					}
				}
			}
			return nil
		}).WithTimeout(uplinkRouteStableWindow).WithPolling(5*time.Second).Should(
			gomega.Succeed(),
			"expected preserved default route to remain in CUDN VRF %s",
			networkName,
		)

		ginkgo.By("verifying discovery still publishes the default gateways from the VRF routing table")
		// Rediscovery after enslavement reads the interface's routes from
		// the CUDN VRF's routing table, where the preserved default routes
		// now live: the published default gateways must not be dropped.
		waitForUplinkStatesDefaultGateways(ctx, f, uplinkName, schedulableNodes.Items, ipFamilySet)

		ginkgo.By("deleting the RouteAdvertisements and waiting for the routes to return to the main table")
		// Deleting the RouteAdvertisements moves the network back to the
		// default routing domain: releasing the Uplink interface from the VRF
		// must migrate its routes back to the main table.
		raClient, err := raclientset.NewForConfig(f.ClientConfig())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(raClient.K8sV1().RouteAdvertisements().Delete(
			ctx,
			networkName,
			metav1.DeleteOptions{},
		)).To(gomega.Succeed())
		for _, family := range ipFamilySet.UnsortedList() {
			_, _, frrIP := preservedFor(family)
			for _, node := range schedulableNodes.Items {
				node := node
				gomega.Eventually(ctx, func() error {
					return uplinkRouteShownIn(node.Name, "", defaultRouteCIDR(family), frrIP)
				}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
					gomega.Succeed(),
					"expected preserved default route via %s back in the main table on node %s",
					frrIP,
					node.Name,
				)
				prefixCIDR := prefixCIDRFor(nodeIfaces[node.Name], family)
				gomega.Eventually(ctx, func() error {
					return uplinkKernelRouteShownIn(node.Name, "", prefixCIDR)
				}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
					gomega.Succeed(),
					"expected prefix route %s back in the main table on node %s",
					prefixCIDR,
					node.Name,
				)
			}
		}
	},
		ginkgo.Entry("with kernel-owned interface addressing", false),
		ginkgo.Entry("with NetworkManager-style interface addressing", true),
	)

	ginkgo.It("discovers a default gateway that appears in the CUDN VRF after enslavement", func(ctx ginkgo.SpecContext) {
		if isDPUUplinkE2E() {
			e2eskipper.Skipf("full-mode Uplink bridge provisioning; the split DPU mode variant covers DPU")
		}
		nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		schedulableNodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 2)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(schedulableNodes.Items).NotTo(gomega.BeEmpty())

		bgpAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		networkName := "uplate" + testSuffix
		serverName := networkName + "-srv"
		peerCIDRs := []string{bgpAlloc.BGPPeerSubnet, bgpAlloc.BGPPeerSubnet6}
		serverCIDRs := []string{bgpAlloc.IPVRFSubnet, bgpAlloc.IPVRFSubnet6}
		gomega.Expect(runBGPNetworkAndServer(
			f,
			ictx,
			ipFamilySet,
			networkName,
			serverName,
			serverName,
			peerCIDRs,
			serverCIDRs,
		)).To(gomega.Succeed())

		peerNetwork, err := infraprovider.Get().GetNetwork(networkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeIfaces := collectNodeNetworkInterfaces(nodes.Items, peerNetwork)

		ginkgo.By("provisioning the Uplink bridge without default routes and creating the Uplink")
		bridgeName := uplinkBridgeName("uplate" + testSuffix)
		gomega.Expect(configureUplinkBridge(ctx, f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())
		uplinkName := networkName
		createUplink(ctx, f, ictx, uplinkName, nodes.Items, nodeIfaces, bridgeName)
		waitForUplinkStatesResolved(ctx, f, uplinkName, bridgeName, nodes.Items)
		waitForUplinkStatesNoDefaultGateways(ctx, f, uplinkName, nodes.Items)

		ginkgo.By("creating the advertised CUDN backed by the Uplink")
		networkLabels := map[string]string{"advertise": networkName}
		networkSpec := uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6)
		namespace, err := createUplinkNamespace(ctx, f, ictx, "uplink-bgp", networkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(createUplinkCUDN(
			ctx,
			f,
			ictx,
			namespace,
			networkName,
			networkSpec,
			networkLabels,
			uplinkName,
		)).To(gomega.Succeed())
		if isDynamicUDNEnabled() {
			ginkgo.By("activating the dynamic CUDN on the nodes under test")
			for i, node := range schedulableNodes.Items {
				createUplinkNetexecPod(
					ctx,
					f,
					namespace.Name,
					fmt.Sprintf("activate-%s-%d", networkName, i),
					node.Name,
				)
			}
		}

		gomega.Expect(createRouteAdvertisements(
			f,
			ictx,
			networkName,
			"auto",
			networkLabels,
			map[string]string{"network": networkName},
		)).To(gomega.Succeed())

		ginkgo.By("waiting for the Uplink bridge to be enslaved to the CUDN VRF")
		for _, node := range schedulableNodes.Items {
			node := node
			gomega.Eventually(ctx, func() error {
				_, err := execNodeCommand(node.Name,
					"ip -o link show dev %s | grep -q 'master %s'", bridgeName, networkName)
				return err
			}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
				gomega.Succeed(),
				"expected Uplink bridge %s enslaved to VRF %s on node %s",
				bridgeName,
				networkName,
				node.Name,
			)
		}

		nodeNames := make([]string, 0, len(schedulableNodes.Items))
		for _, node := range schedulableNodes.Items {
			nodeNames = append(nodeNames, node.Name)
		}

		waitForUplinkStatesNoDefaultGateways(ctx, f, uplinkName, schedulableNodes.Items)
		// A change of the published default gateways must not rebuild the
		// gateway: the CUDN VRF keeps its interface index and the Uplink
		// bridge stays enslaved to it.
		vrfLinkIndexes := map[string]string{}
		for _, node := range schedulableNodes.Items {
			vrfLinkIndexes[node.Name] = uplinkLinkIndex(node.Name, networkName)
		}
		ginkgo.By("adding default routes explicitly to the CUDN VRF routing table")
		frr := infraapi.ExternalContainer{Name: networkName + "-frr"}
		frrIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(frr, peerNetwork)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		for _, family := range ipFamilySet.UnsortedList() {
			frrIP := getFirstIPStringOfFamily(family, []string{frrIface.IPv4, frrIface.IPv6})
			gomega.Expect(frrIP).NotTo(gomega.BeEmpty())
			hasDefaultRoute := func(nodeName string) (bool, error) {
				return cudnGRHasRoute(f.ClientSet, networkName, nodeName, defaultRouteCIDR(family), frrIP)
			}
			for _, node := range schedulableNodes.Items {
				gomega.Expect(hasDefaultRoute(node.Name)).To(gomega.BeFalse(),
					"expected no default route via %s on the gateway router of CUDN %s for node %s before discovery",
					frrIP, networkName, node.Name)
			}
			gomega.Expect(configureUplinkStaticRoute(
				ictx,
				bridgeName,
				nodeNames,
				defaultRouteCIDR(family),
				frrIP,
				false,
				networkName,
			)).To(gomega.Succeed())
			for _, node := range schedulableNodes.Items {
				gomega.Expect(uplinkRouteShownIn(node.Name, "vrf "+networkName, defaultRouteCIDR(family), frrIP)).
					To(gomega.Succeed(),
						"expected the added default route via %s in CUDN VRF %s on node %s",
						frrIP, networkName, node.Name)
				gomega.Eventually(ctx, func() (bool, error) { return hasDefaultRoute(node.Name) }).
					WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.BeTrue(),
					"expected CUDN %s gateway router on node %s to acquire default route via %s",
					networkName, node.Name, frrIP)
			}
		}

		ginkgo.By("waiting for discovery to publish the default gateways from the VRF routing table")
		waitForUplinkStatesDefaultGateways(ctx, f, uplinkName, schedulableNodes.Items, ipFamilySet)
		for _, node := range schedulableNodes.Items {
			waitForUplinkStateGatewayCondition(
				ctx,
				f,
				uplinkName,
				node.Name,
				metav1.ConditionTrue,
				uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
			)
		}
		ginkgo.By("checking that the gateway change did not rebuild the CUDN VRF")
		for _, node := range schedulableNodes.Items {
			gomega.Expect(uplinkLinkIndex(node.Name, networkName)).To(gomega.Equal(vrfLinkIndexes[node.Name]),
				"expected CUDN VRF %s on node %s to keep its interface index across the default gateway change",
				networkName, node.Name)
			_, err := execNodeCommand(node.Name,
				"ip -o link show dev %s | grep -q 'master %s'", bridgeName, networkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(),
				"expected Uplink bridge %s to stay enslaved to VRF %s on node %s across the default gateway change",
				bridgeName, networkName, node.Name)
		}
	})
})

var _ = ginkgo.Describe("Uplink route advertisements with Dynamic UDN allocation", feature.RouteAdvertisementsDynamicUDN, feature.Uplink, func() {
	f := wrappedTestFramework("uplink-dynamic-bgp")
	f.SkipNamespaceCreation = true

	var ictx infraapi.Context
	var ipFamilySet sets.Set[utilnet.IPFamily]
	var testSuffix string

	ginkgo.BeforeEach(func() {
		if IsGatewayModeLocal(f.ClientSet) {
			e2eskipper.Skipf("Uplink CUDN gateway plumbing is only supported in shared gateway mode")
		}
		ipFamilySet = sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
		ictx = infraprovider.Get().NewTestContext()
		testSuffix = framework.RandomSuffix()
	})

	ginkgo.It("allows node-disjoint Dynamic CUDNs to share a targetVRF auto Uplink and rejects overlap", func(ctx ginkgo.SpecContext) {
		if !isDynamicUDNEnabled() {
			e2eskipper.Skipf("test requires Dynamic UDN allocation")
		}

		schedulableNodes, err := e2enode.GetReadySchedulableNodes(ctx, f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		availableNodes := schedulableNodes.Items
		if isDPUUplinkE2E() {
			availableNodes = filterNodesByLabel(availableNodes, uplinkDPUHostNodeLabel)
		}
		if len(availableNodes) < 2 {
			e2eskipper.Skipf("test requires at least two ready schedulable nodes")
		}
		nodes := availableNodes[:2]

		var nodeIfaces map[string]infraapi.NetworkInterface
		frrNeighborIPsByNode := map[string][]string{}
		var bridgeName, hostInterfaceName string
		if isDPUUplinkE2E() {
			nodeIfaces = collectDPUHostUplinkInterfaces(nodes)
			bridgeName = os.Getenv(uplinkDPUExpectedBridgeEnv)
			gomega.Expect(bridgeName).NotTo(gomega.BeEmpty(), "expected the DPU Uplink bridge name")

			dpuGatewayNetwork, err := infraprovider.Get().GetNetwork(os.Getenv(uplinkDPUGatewayNetworkEnv))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			frrIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
				infraapi.ExternalContainer{Name: routerContainerName},
				dpuGatewayNetwork,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			frrNeighborIPs := matchIPStringsByIPFamilySet([]string{frrIface.IPv4, frrIface.IPv6}, ipFamilySet)
			gomega.Expect(frrNeighborIPs).NotTo(gomega.BeEmpty(), "expected an external FRR address on the DPU Uplink network")
			for _, node := range nodes {
				frrNeighborIPsByNode[node.Name] = frrNeighborIPs
			}
		} else {
			uplinkAlloc, err := allocators.AllocateBGP(f, ictx)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, nodeIfaces = setupUplinkNetwork(
				ictx,
				nodes,
				ipFamilySet,
				"updyn"+testSuffix,
				[]string{uplinkAlloc.BGPPeerSubnet, uplinkAlloc.BGPPeerSubnet6},
			)
			bridgeName = uplinkBridgeName("updyn" + testSuffix)
			hostInterfaceName = bridgeName
			gomega.Expect(configureUplinkBridge(ctx, f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())
			for _, node := range nodes {
				iface := nodeIfaces[node.Name]
				ipv4Gateway, err := interfaceGateway(iface.IPv4Gateway, iface.IPv4, iface.IPv4Prefix)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ipv6Gateway, err := interfaceGateway(iface.IPv6Gateway, iface.IPv6, iface.IPv6Prefix)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				frrNeighborIPsByNode[node.Name] = matchIPStringsByIPFamilySet(
					[]string{ipv4Gateway, ipv6Gateway},
					ipFamilySet,
				)
			}
		}

		uplinkName := "updyn" + testSuffix
		createUplink(ctx, f, ictx, uplinkName, nodes, nodeIfaces, hostInterfaceName)
		waitForUplinkStatesResolved(ctx, f, uplinkName, bridgeName, nodes)

		type networkOnNode struct {
			name      string
			namespace string
			node      corev1.Node
		}
		networks := []networkOnNode{
			{name: "upda" + testSuffix, node: nodes[0]},
			{name: "updb" + testSuffix, node: nodes[1]},
		}
		ginkgo.By(fmt.Sprintf(
			"creating node-disjoint Dynamic CUDNs %s on %s and %s on %s using Uplink %s with targetVRF auto",
			networks[0].name,
			networks[0].node.Name,
			networks[1].name,
			networks[1].node.Name,
			uplinkName,
		))
		for i := range networks {
			network := &networks[i]
			bgpAlloc, err := allocators.AllocateBGP(f, ictx)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			networkLabels := map[string]string{"advertise": network.name}
			namespace, err := createUplinkNamespace(ctx, f, ictx, "uplink-bgp", network.name)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			network.namespace = namespace.Name
			gomega.Expect(createUplinkCUDN(
				ctx,
				f,
				ictx,
				namespace,
				network.name,
				uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6),
				networkLabels,
				uplinkName,
			)).To(gomega.Succeed())

			createUplinkNetexecPod(ctx, f, namespace.Name, "client-"+network.name, network.node.Name)
			gomega.Expect(createNodeScopedUplinkFRRConfiguration(
				ctx,
				f,
				ictx,
				network.name,
				network.name,
				network.node,
				frrNeighborIPsByNode[network.node.Name],
			)).To(gomega.Succeed())
			gomega.Expect(createRouteAdvertisements(
				f,
				ictx,
				network.name,
				"auto",
				networkLabels,
				map[string]string{"network": network.name},
			)).To(gomega.Succeed())
		}

		ginkgo.By("verifying both node-disjoint Dynamic CUDNs use the shared Uplink without conflict")
		for _, network := range networks {
			gomega.Eventually(ctx, func() (string, error) {
				return getUplinkBridgeVRF(network.node.Name, bridgeName)
			}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
				gomega.Equal(network.name),
				"expected Uplink bridge %s on node %s to be attached to CUDN VRF %s",
				bridgeName,
				network.node.Name,
				network.name,
			)
			waitForCUDNUplinksReady(ctx, f, network.name)
		}

		owner := networks[0]
		conflicting := networks[1]
		ginkgo.By(fmt.Sprintf(
			"making CUDN %s active and advertised on node %s already used by CUDN %s",
			conflicting.name,
			owner.node.Name,
			owner.name,
		))
		createUplinkNetexecPod(
			ctx,
			f,
			conflicting.namespace,
			"client-"+conflicting.name+"-overlap",
			owner.node.Name,
		)
		gomega.Expect(createNodeScopedUplinkFRRConfiguration(
			ctx,
			f,
			ictx,
			conflicting.name+"-overlap",
			conflicting.name,
			owner.node,
			frrNeighborIPsByNode[owner.node.Name],
		)).To(gomega.Succeed())

		waitForUplinkStateGatewayCondition(
			ctx,
			f,
			uplinkName,
			owner.node.Name,
			metav1.ConditionFalse,
			"UplinkConfigurationConflict",
		)
		waitForCUDNUplinksCondition(
			ctx,
			f,
			conflicting.name,
			metav1.ConditionFalse,
			"UplinkConfigurationConflict",
		)
		gomega.Eventually(ctx, func() (string, error) {
			return getUplinkBridgeVRF(owner.node.Name, bridgeName)
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Equal(owner.name),
			"expected conflicting CUDN %s not to move Uplink bridge %s on node %s from VRF %s",
			conflicting.name,
			bridgeName,
			owner.node.Name,
			owner.name,
		)
	})
})

var _ = ginkgo.Describe("Network Segmentation Uplink route advertisements", feature.NetworkSegmentation, feature.RouteAdvertisements, feature.Uplink, func() {
	f := wrappedTestFramework("uplink-bgp")
	f.SkipNamespaceCreation = true

	var ictx infraapi.Context
	var ipFamilySet sets.Set[utilnet.IPFamily]
	var testSuffix string

	ginkgo.BeforeEach(func() {
		if IsGatewayModeLocal(f.ClientSet) {
			e2eskipper.Skipf("Uplink CUDN gateway plumbing is only supported in shared gateway mode")
		}
		ipFamilySet = sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
		ictx = infraprovider.Get().NewTestContext()
		testSuffix = framework.RandomSuffix()
	})

	ginkgo.It("uses the default VRF as the BGP peering path", func(ctx ginkgo.SpecContext) {
		if isDPUUplinkE2E() {
			e2eskipper.Skipf("default-VRF Uplink route advertisements use regular KIND bridge provisioning")
		}

		nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		schedulableNodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 2)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(schedulableNodes.Items).NotTo(gomega.BeEmpty())

		bgpAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		networkName := "updefra" + testSuffix
		serverName := networkName + "-srv"
		serverNetworkName := serverName
		peerCIDRs := []string{bgpAlloc.BGPPeerSubnet, bgpAlloc.BGPPeerSubnet6}
		serverCIDRs := []string{bgpAlloc.IPVRFSubnet, bgpAlloc.IPVRFSubnet6}

		gomega.Expect(runBGPNetworkAndServerWithFRRVRF(
			f,
			ictx,
			ipFamilySet,
			networkName,
			serverName,
			serverNetworkName,
			peerCIDRs,
			serverCIDRs,
			"",
		)).To(gomega.Succeed())

		peerNetwork, err := infraprovider.Get().GetNetwork(networkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeIfaces := collectNodeNetworkInterfaces(nodes.Items, peerNetwork)

		bridgeName := uplinkBridgeName("updefra" + testSuffix)
		gomega.Expect(configureUplinkBridge(ctx, f, ictx, bridgeName, nodeIfaces)).To(gomega.Succeed())

		createUplink(ctx, f, ictx, networkName, nodes.Items, nodeIfaces, bridgeName)
		waitForUplinkStatesResolved(ctx, f, networkName, bridgeName, nodes.Items)

		networkSpec := uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6)
		namespace, err := createUplinkAdvertisedCUDN(
			ctx,
			f,
			ictx,
			networkName,
			networkSpec,
			networkName,
			"",
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		pod := createUplinkNetexecPod(
			ctx,
			f,
			namespace.Name,
			"client-"+networkName,
			schedulableNodes.Items[0].Name,
		)
		cudnNodes := schedulableNodes.Items
		if isDynamicUDNEnabled() {
			// With dynamic UDN allocation, the CUDN and its VRF only exist on
			// nodes running workloads attached to the network.
			cudnNodes = []corev1.Node{schedulableNodes.Items[0]}
		}

		serverNetwork, err := infraprovider.Get().GetNetwork(serverNetworkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		serverIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
			infraapi.ExternalContainer{Name: serverName},
			serverNetwork,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		frrIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
			infraapi.ExternalContainer{Name: networkName + "-frr"},
			peerNetwork,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, node := range nodes.Items {
			uplinkBridgeVRF, err := getNodeInterfaceVRF(node.Name, bridgeName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(uplinkBridgeVRF).To(gomega.BeEmpty(),
				"expected Uplink bridge %s on node %s to remain in the default VRF",
				bridgeName,
				node.Name,
			)
		}

		for _, family := range ipFamilySet.UnsortedList() {
			serverCIDR := getFirstCIDRStringOfFamily(family, serverCIDRs)
			gomega.Expect(serverCIDR).NotTo(gomega.BeEmpty())
			frrIP := getFirstIPStringOfFamily(family, []string{frrIface.IPv4, frrIface.IPv6})
			gomega.Expect(frrIP).NotTo(gomega.BeEmpty())
			for _, node := range schedulableNodes.Items {
				gomega.Eventually(ctx, func() (bool, error) {
					return hasRouteInDefaultVRF(node, serverCIDR, bgpNextHopsForPeer(family, frrIface)...)
				}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
					gomega.BeTrue(),
					"expected node %s to learn %s via %s in the default VRF",
					node.Name,
					serverCIDR,
					frrIP,
				)
			}
			for _, node := range cudnNodes {
				gomega.Eventually(ctx, func() (bool, error) {
					return hasRouteInCUDNVRF(node, networkName, serverCIDR, bgpNextHopsForPeer(family, frrIface)...)
				}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
					gomega.BeTrue(),
					"expected node %s to leak %s via %s into CUDN VRF %s",
					node.Name,
					serverCIDR,
					frrIP,
					networkName,
				)
			}

			serverIP := getFirstIPStringOfFamily(family, []string{serverIface.IPv4, serverIface.IPv6})
			gomega.Expect(serverIP).NotTo(gomega.BeEmpty())
			uplinkPodToHostnameAndExpect(pod, serverIP, serverName)
			podIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
				f.ClientSet,
				pod.Namespace,
				pod.Name,
				networkName,
				family,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(podIP).NotTo(gomega.BeEmpty())
			uplinkPodToClientIPAndExpect(pod, serverIP, podIP)
			uplinkExternalToPodAndExpect(
				ctx,
				infraapi.ExternalContainer{Name: serverName},
				podIP,
				pod.Name,
			)
		}

		servicePod := createUplinkServicePod(
			ctx,
			f,
			namespace.Name,
			"server-"+networkName,
			schedulableNodes.Items[0].Name,
		)
		service := createUplinkNodePortService(ctx, f, namespace.Name, servicePod.Labels)
		primaryNetwork, err := infraprovider.Get().PrimaryNetwork()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		primaryIface, err := infraprovider.Get().GetK8NodeNetworkInterface(
			servicePod.Spec.NodeName,
			primaryNetwork,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		primaryClient, err := ictx.CreateExternalContainer(infraapi.ExternalContainer{
			Name:    "upclient" + testSuffix,
			Image:   images.AgnHost(),
			Network: primaryNetwork,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		peerClient, err := ictx.CreateExternalContainer(infraapi.ExternalContainer{
			Name:    "uppeer" + testSuffix,
			Image:   images.AgnHost(),
			Network: peerNetwork,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		defaultNamespace, err := createUplinkPlainNamespace(
			ctx,
			f,
			ictx,
			"uplink-bgp",
			"default-"+networkName,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defaultServicePod := createUplinkServicePod(
			ctx,
			f,
			defaultNamespace.Name,
			"default-server-"+networkName,
			schedulableNodes.Items[0].Name,
		)
		defaultService := createUplinkNodePortService(
			ctx,
			f,
			defaultNamespace.Name,
			defaultServicePod.Labels,
		)

		for _, family := range ipFamilySet.UnsortedList() {
			nodeIP := getFirstIPStringOfFamily(family, []string{primaryIface.IPv4, primaryIface.IPv6})
			if nodeIP != "" {
				uplinkExternalToNodePortAndExpect(
					ctx,
					primaryClient,
					nodeIP,
					service.Spec.Ports[0].NodePort,
					servicePod.Name,
				)
			}

			uplinkNodeIface, ok := nodeIfaces[servicePod.Spec.NodeName]
			gomega.Expect(ok).To(gomega.BeTrue(), "expected Uplink interface for node %s", servicePod.Spec.NodeName)
			nodeIP = getFirstIPStringOfFamily(family, []string{uplinkNodeIface.IPv4, uplinkNodeIface.IPv6})
			if nodeIP != "" {
				uplinkExternalToNodePortAndExpect(
					ctx,
					peerClient,
					nodeIP,
					service.Spec.Ports[0].NodePort,
					servicePod.Name,
				)
			}

			defaultUplinkNodeIface, ok := nodeIfaces[defaultServicePod.Spec.NodeName]
			gomega.Expect(ok).To(gomega.BeTrue(), "expected Uplink interface for node %s", defaultServicePod.Spec.NodeName)
			nodeIP = getFirstIPStringOfFamily(family, []string{defaultUplinkNodeIface.IPv4, defaultUplinkNodeIface.IPv6})
			if nodeIP != "" {
				uplinkExternalToNodePortAndExpect(
					ctx,
					peerClient,
					nodeIP,
					defaultService.Spec.Ports[0].NodePort,
					defaultServicePod.Name,
				)
			}
		}
	})
})

var _ = ginkgo.Describe("Network Segmentation Uplink split DPU status conditions", feature.NetworkSegmentation, feature.Uplink, func() {
	f := wrappedTestFramework("uplink-conditions")
	f.SkipNamespaceCreation = true

	var ictx infraapi.Context
	var ipFamilySet sets.Set[utilnet.IPFamily]
	var testSuffix string

	ginkgo.BeforeEach(func() {
		if IsGatewayModeLocal(f.ClientSet) {
			e2eskipper.Skipf("Uplink CUDN gateway plumbing is only supported in shared gateway mode")
		}
		if !isDPUUplinkE2E() {
			e2eskipper.Skipf("split DPU status condition e2e requires the DPU simulator environment")
		}
		ipFamilySet = sets.New(getSupportedIPFamiliesSlice(f.ClientSet)...)
		ictx = infraprovider.Get().NewTestContext()
		testSuffix = framework.RandomSuffix()
	})

	ginkgo.It("keeps one writer per condition and recovers a missing host interface", func(ctx ginkgo.SpecContext) {
		schedulableNodes, err := e2enode.GetReadySchedulableNodes(ctx, f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		dpuHostNodes := filterNodesByLabel(schedulableNodes.Items, uplinkDPUHostNodeLabel)
		gomega.Expect(dpuHostNodes).NotTo(gomega.BeEmpty(), "expected at least one ready schedulable DPU host node")
		nodeIfaces := collectDPUHostUplinkInterfaces(dpuHostNodes)

		ginkgo.By("resolving an Uplink on the provisioned host interface")
		uplinkName := "upcond" + testSuffix
		createUplink(ctx, f, ictx, uplinkName, dpuHostNodes, nodeIfaces, "")
		waitForUplinkStatesResolved(ctx, f, uplinkName, os.Getenv(uplinkDPUExpectedBridgeEnv), dpuHostNodes)

		ginkgo.By("verifying the DPU-host reported its discovery on HostDataReady")
		for _, node := range dpuHostNodes {
			state, err := getUplinkState(ctx, f, uplinkName, node.Name)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(checkUplinkStateCondition(state,
				uplinkv1alpha1.UplinkStateConditionHostDataReady,
				metav1.ConditionTrue,
				uplinkv1alpha1.UplinkStateReasonHostDataDiscovered,
			)).To(gomega.Succeed())
		}

		ginkgo.By("pointing an Uplink at a host interface that does not exist")
		node := dpuHostNodes[0]
		missingIface := "upe" + testSuffix
		missingUplink := "upmiss" + testSuffix
		createUplink(ctx, f, ictx, missingUplink, []corev1.Node{node}, nodeIfaces, missingIface)

		gomega.Eventually(ctx, func() error {
			state, err := getUplinkState(ctx, f, missingUplink, node.Name)
			if err != nil {
				return err
			}
			if err := checkUplinkStateCondition(state,
				uplinkv1alpha1.UplinkStateConditionHostDataReady,
				metav1.ConditionFalse,
				uplinkv1alpha1.UplinkStateReasonHostInterfaceNotFound,
			); err != nil {
				return err
			}
			return checkUplinkStateCondition(state,
				uplinkv1alpha1.UplinkStateConditionResolved,
				metav1.ConditionFalse,
				uplinkv1alpha1.UplinkStateReasonWaitingForDPUHost,
			)
		}).WithTimeout(uplinkShortTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected the DPU-host to report the missing interface on HostDataReady and the DPU to wait on Resolved",
		)
		// Each condition must keep a single writer in the failure window
		// too: assert both conditions stay stable, including the transition
		// timestamps, which a competing writer would churn.
		snapshotConditions := func() (map[string]metav1.Condition, error) {
			state, err := getUplinkState(ctx, f, missingUplink, node.Name)
			if err != nil {
				return nil, err
			}
			conditions := map[string]metav1.Condition{}
			for _, conditionType := range []string{
				uplinkv1alpha1.UplinkStateConditionHostDataReady,
				uplinkv1alpha1.UplinkStateConditionResolved,
			} {
				condition, err := uplinkStateCondition(state, conditionType)
				if err != nil {
					return nil, err
				}
				conditions[conditionType] = *condition
			}
			return conditions, nil
		}
		conditions, err := snapshotConditions()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Consistently(ctx, snapshotConditions).WithTimeout(uplinkConditionStableWindow).WithPolling(uplinkPoll).Should(
			gomega.Equal(conditions),
			"expected stable conditions while unresolved: reason, message and lastTransitionTime unchanged",
		)

		ginkgo.By("creating the host interface and waiting for host discovery to recover")
		ictx.AddCleanUpFn(func() error {
			_, err := execNodeCommand(node.Name, "ip link del %s || true", missingIface)
			return err
		})
		createIface := fmt.Sprintf(
			"ip link add %[1]s type dummy && ip addr add 192.0.2.1/24 dev %[1]s", missingIface)
		if ipFamilySet.Has(utilnet.IPv6) {
			createIface += fmt.Sprintf(" && ip addr add 2001:db8:e2e::1/64 dev %s", missingIface)
		}
		createIface += fmt.Sprintf(" && ip link set %s up", missingIface)
		gomega.Expect(execNodeCommand(node.Name, "%s", createIface)).Error().To(gomega.Succeed())

		gomega.Eventually(ctx, func() error {
			state, err := getUplinkState(ctx, f, missingUplink, node.Name)
			if err != nil {
				return err
			}
			if err := checkUplinkStateCondition(state,
				uplinkv1alpha1.UplinkStateConditionHostDataReady,
				metav1.ConditionTrue,
				uplinkv1alpha1.UplinkStateReasonHostDataDiscovered,
			); err != nil {
				return err
			}
			// The DPU consumed the recovered host data: it moved past
			// WaitingForDPUHost to a bridge discovery verdict of its own.
			// Which verdict depends on the DPU-side environment (a dummy
			// interface MAC matches no DPU bridge), so only the transition
			// is asserted.
			resolved, err := uplinkStateCondition(state, uplinkv1alpha1.UplinkStateConditionResolved)
			if err != nil {
				return err
			}
			if resolved.Status != metav1.ConditionFalse ||
				resolved.Reason == uplinkv1alpha1.UplinkStateReasonWaitingForDPUHost {
				return fmt.Errorf("UplinkState %s Resolved is %s/%s, expected a DPU-side discovery verdict",
					state.GetName(), resolved.Status, resolved.Reason)
			}
			return nil
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected host discovery to recover without a restart and the DPU to consume the new data",
		)

		ginkgo.By("removing the host interface and waiting for the published host data to be pruned")
		// Unlike the missing-interface phase above, host data has been
		// published by now and must be withdrawn, not just flagged: a stale
		// macAddress would keep feeding the DPU's bridge matching. Link
		// deletion generates no Kubernetes events and a successful side does
		// not retry, so force a reconcile with a node label change.
		gomega.Expect(execNodeCommand(node.Name, "ip link del %s", missingIface)).Error().To(gomega.Succeed())
		pokeLabel := "e2e.k8s.ovn.org/uplink-poke"
		e2enode.AddOrUpdateLabelOnNode(f.ClientSet, node.Name, pokeLabel, testSuffix)
		ginkgo.DeferCleanup(e2enode.RemoveLabelOffNode, f.ClientSet, node.Name, pokeLabel)
		gomega.Eventually(ctx, func() error {
			state, err := getUplinkState(ctx, f, missingUplink, node.Name)
			if err != nil {
				return err
			}
			if err := checkUplinkStateCondition(state,
				uplinkv1alpha1.UplinkStateConditionHostDataReady,
				metav1.ConditionFalse,
				uplinkv1alpha1.UplinkStateReasonHostInterfaceNotFound,
			); err != nil {
				return err
			}
			// Server-side apply removes the host-owned data fields once the
			// DPU-host stops asserting them.
			macAddress, _, err := unstructured.NestedString(state.Object, "status", "macAddress")
			if err != nil {
				return err
			}
			if macAddress != "" {
				return fmt.Errorf("UplinkState %s still reports macAddress %q for the removed interface",
					state.GetName(), macAddress)
			}
			return nil
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected the host-owned data fields to be pruned once discovery fails",
		)
	})

	ginkgo.It("recreates an UplinkState deleted out of band", func(ctx ginkgo.SpecContext) {
		schedulableNodes, err := e2enode.GetReadySchedulableNodes(ctx, f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		dpuHostNodes := filterNodesByLabel(schedulableNodes.Items, uplinkDPUHostNodeLabel)
		gomega.Expect(dpuHostNodes).NotTo(gomega.BeEmpty(), "expected at least one ready schedulable DPU host node")
		nodeIfaces := collectDPUHostUplinkInterfaces(dpuHostNodes)
		node := dpuHostNodes[0]

		ginkgo.By("resolving an Uplink on the provisioned host interface")
		uplinkName := "updel" + testSuffix
		createUplink(ctx, f, ictx, uplinkName, []corev1.Node{node}, nodeIfaces, "")
		waitForUplinkStatesResolved(ctx, f, uplinkName, os.Getenv(uplinkDPUExpectedBridgeEnv), []corev1.Node{node})

		ginkgo.By("deleting the UplinkState out of band")
		state, err := getUplinkState(ctx, f, uplinkName, node.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deletedUID := state.GetUID()
		gomega.Expect(f.DynamicClient.Resource(uplinkStateGVR).Delete(
			ctx,
			state.GetName(),
			metav1.DeleteOptions{},
		)).To(gomega.Succeed())

		ginkgo.By("waiting for both DPU sides to republish their status on a recreated UplinkState")
		gomega.Eventually(ctx, func() error {
			state, err := getUplinkState(ctx, f, uplinkName, node.Name)
			if err != nil {
				return err
			}
			// A new UID proves recreation rather than a surviving object.
			if state.GetUID() == deletedUID {
				return fmt.Errorf("UplinkState %s still carries the deleted object's UID %s",
					state.GetName(), deletedUID)
			}
			// The DPU-host republished the host interface data on the new
			// object...
			if err := checkUplinkStateCondition(state,
				uplinkv1alpha1.UplinkStateConditionHostDataReady,
				metav1.ConditionTrue,
				uplinkv1alpha1.UplinkStateReasonHostDataDiscovered,
			); err != nil {
				return err
			}
			// ...and the DPU consumed it and re-resolved its bridge, having
			// survived the create race between the two sides.
			return checkUplinkStateCondition(state,
				uplinkv1alpha1.UplinkStateConditionResolved,
				metav1.ConditionTrue,
				uplinkv1alpha1.UplinkStateReasonResolved,
			)
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected the deleted UplinkState to be recreated and re-resolved without a restart",
		)
		// Full status recovery: the resolved bridge and the host IPs were
		// republished on the recreated object, not just the conditions.
		waitForUplinkStatesResolved(ctx, f, uplinkName, os.Getenv(uplinkDPUExpectedBridgeEnv), []corev1.Node{node})
	})

	ginkgo.It("resolves the bridge by host function and falls back to host MAC", func(ctx ginkgo.SpecContext) {
		nodesCtx, cancel := context.WithTimeout(ctx, uplinkShortTimeout)
		defer cancel()
		schedulableNodes, err := e2enode.GetReadySchedulableNodes(nodesCtx, f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		dpuHostNodes := filterNodesByLabel(schedulableNodes.Items, uplinkDPUHostNodeLabel)
		gomega.Expect(dpuHostNodes).NotTo(gomega.BeEmpty(), "expected at least one ready schedulable DPU host node")
		nodeIfaces := collectDPUHostUplinkInterfaces(dpuHostNodes)
		expectedBridge := os.Getenv(uplinkDPUExpectedBridgeEnv)

		ginkgo.By("resolving an Uplink through published host function")
		deviceUplink := "updd" + testSuffix
		createUplink(ctx, f, ictx, deviceUplink, dpuHostNodes, nodeIfaces, "")
		waitForUplinkStatesResolved(ctx, f, deviceUplink, expectedBridge, dpuHostNodes)
		for _, node := range dpuHostNodes {
			node := node
			gomega.Eventually(ctx, func() error {
				state, err := getUplinkState(ctx, f, deviceUplink, node.Name)
				if err != nil {
					return err
				}
				if err := checkUplinkStateHostFunction(state, nodeIfaces[node.Name].InfName); err != nil {
					return err
				}
				return checkUplinkStateResolvedVia(state, "host function")
			}).WithTimeout(uplinkShortTimeout).WithPolling(uplinkPoll).Should(
				gomega.Succeed(),
				"expected the DPU-host on node %q to publish host function and the DPU to resolve through it",
				node.Name,
			)
		}

		ginkgo.By("resolving an Uplink by host MAC when no host function can be published")
		// A dummy interface is not an SR-IOV function, so the DPU-host
		// publishes host data without host function and the DPU must fall
		// back to matching the published MAC against the representor peer
		// MACs on its bridges. Cloning the provisioned uplink interface MAC
		// steers that match to the same representor and bridge.
		node := dpuHostNodes[0]
		hostInterfaceName := nodeIfaces[node.Name].InfName
		hostMAC, err := infraprovider.Get().ExecK8NodeCommand(node.Name, []string{
			"cat", "/sys/class/net/" + hostInterfaceName + "/address",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		hostMAC = strings.TrimSpace(hostMAC)
		gomega.Expect(hostMAC).NotTo(gomega.BeEmpty(),
			"expected a MAC address on the provisioned uplink interface %s", hostInterfaceName)

		macIface := "upm" + testSuffix
		ictx.AddCleanUpFn(func() error {
			_, err := execNodeCommand(node.Name, "ip link del %s || true", macIface)
			return err
		})
		createIface := fmt.Sprintf(
			"ip link add %[1]s address %[2]s type dummy && ip addr add 192.0.2.10/24 dev %[1]s",
			macIface, hostMAC)
		if ipFamilySet.Has(utilnet.IPv6) {
			createIface += fmt.Sprintf(" && ip addr add 2001:db8:e2e::10/64 dev %s", macIface)
		}
		createIface += fmt.Sprintf(" && ip link set %s up", macIface)
		gomega.Expect(execNodeCommand(node.Name, "%s", createIface)).Error().To(gomega.Succeed())

		macUplink := "upmac" + testSuffix
		createUplink(ctx, f, ictx, macUplink, []corev1.Node{node}, nodeIfaces, macIface)
		waitForUplinkStatesResolved(ctx, f, macUplink, expectedBridge, []corev1.Node{node})
		state, err := getUplinkState(ctx, f, macUplink, node.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, hasHostFunction, err := unstructured.NestedMap(state.Object, "status", "hostFunction")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(hasHostFunction).To(gomega.BeFalse(),
			"a non SR-IOV host interface must resolve by MAC, without host function")
		gomega.Expect(checkUplinkStateResolvedVia(state, "host MAC")).To(gomega.Succeed())
	})

	ginkgo.It("runs a CUDN without Uplink default gateways and publishes them when they appear", func(ctx ginkgo.SpecContext) {
		schedulableNodes, err := e2enode.GetReadySchedulableNodes(ctx, f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		dpuHostNodes := filterNodesByLabel(schedulableNodes.Items, uplinkDPUHostNodeLabel)
		gomega.Expect(dpuHostNodes).NotTo(gomega.BeEmpty(), "expected at least one ready schedulable DPU host node")
		nodeIfaces := collectDPUHostUplinkInterfaces(dpuHostNodes)

		ginkgo.By("ensuring the host interfaces carry no default routes")
		// The DPU simulator provisions the host interfaces with addresses
		// but no default routes; the removal only normalizes environments
		// that do provision them.
		for _, node := range dpuHostNodes {
			gomega.Expect(removeNodeInterfaceDefaultRoutes(ictx, node.Name, nodeIfaces[node.Name].InfName)).
				To(gomega.Succeed())
		}

		ginkgo.By("resolving an Uplink that discovers no default gateways")
		uplinkName := "upgw" + testSuffix
		createUplink(ctx, f, ictx, uplinkName, dpuHostNodes, nodeIfaces, "")
		waitForUplinkStatesResolved(ctx, f, uplinkName, os.Getenv(uplinkDPUExpectedBridgeEnv), dpuHostNodes)
		waitForUplinkStatesNoDefaultGateways(ctx, f, uplinkName, dpuHostNodes)

		ginkgo.By("running a CUDN on the Uplink with no degraded condition")
		// A default gateway is not required: no condition degrades, and its
		// absence is visible only as the empty status.defaultGateways field.
		networkName := "upgw" + testSuffix
		bgpAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		networkSpec := uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6)
		namespace, err := createUplinkNamespace(ctx, f, ictx, "uplink-conditions", networkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(createUplinkCUDN(
			ctx,
			f,
			ictx,
			namespace,
			networkName,
			networkSpec,
			nil,
			uplinkName,
		)).To(gomega.Succeed())
		// The network is rendered, and its gateway router created, only on
		// the node running a pod attached to it.
		podNode := dpuHostNodes[0]
		createUplinkNetexecPod(ctx, f, namespace.Name, "client-"+networkName, podNode.Name)
		waitForUplinkStateGatewayCondition(ctx, f, uplinkName, podNode.Name,
			metav1.ConditionTrue, uplinkv1alpha1.UplinkStateReasonGatewayConfigured)
		waitForUplinkStateConditionOfType(ctx, f, uplinkName, podNode.Name,
			uplinkv1alpha1.UplinkStateConditionHostGatewayReady,
			metav1.ConditionTrue, uplinkv1alpha1.UplinkStateReasonGatewayConfigured)
		waitForCUDNUplinksReady(ctx, f, networkName)

		ginkgo.By("adding default routes and waiting for the gateways to be published and programmed")
		for _, node := range dpuHostNodes {
			node := node
			gateways, err := addNodeInterfaceDefaultRoutes(ictx, node.Name, nodeIfaces[node.Name].InfName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(gateways).NotTo(gomega.BeEmpty(),
				"expected the host interface of node %s to carry addresses to derive default gateways from", node.Name)
			waitForUplinkStatesDefaultGateways(ctx, f, uplinkName, []corev1.Node{node}, sets.KeySet(gateways))
			if node.Name != podNode.Name {
				continue
			}
			for family, gateway := range gateways {
				gomega.Eventually(ctx, func() (bool, error) {
					return dpuCUDNGatewayRouterHasDefaultRoute(node.Name, networkName, defaultRouteCIDR(family), gateway)
				}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.BeTrue(),
					"expected the gateway router of CUDN %s for node %s to acquire a default route via %s",
					networkName, node.Name, gateway)
			}
		}
	})

	ginkgo.It("discovers a default gateway that appears in the host-side CUDN VRF after enslavement", func(ctx ginkgo.SpecContext) {
		if !ipFamilySet.Has(utilnet.IPv4) {
			e2eskipper.Skipf("DPU Uplink e2e requires IPv4 on the DPU simulator gateway network")
		}
		schedulableNodes, err := e2enode.GetReadySchedulableNodes(ctx, f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		dpuHostNodes := filterNodesByLabel(schedulableNodes.Items, uplinkDPUHostNodeLabel)
		gomega.Expect(dpuHostNodes).NotTo(gomega.BeEmpty(), "expected at least one ready schedulable DPU host node")
		nodeIfaces := collectDPUHostUplinkInterfaces(dpuHostNodes)

		ginkgo.By("ensuring the host interfaces carry no default routes")
		for _, node := range dpuHostNodes {
			gomega.Expect(removeNodeInterfaceDefaultRoutes(ictx, node.Name, nodeIfaces[node.Name].InfName)).
				To(gomega.Succeed())
		}

		ginkgo.By("resolving an Uplink that discovers no default gateways")
		uplinkName := "uplate" + testSuffix
		createUplink(ctx, f, ictx, uplinkName, dpuHostNodes, nodeIfaces, "")
		waitForUplinkStatesResolved(ctx, f, uplinkName, os.Getenv(uplinkDPUExpectedBridgeEnv), dpuHostNodes)
		waitForUplinkStatesNoDefaultGateways(ctx, f, uplinkName, dpuHostNodes)

		ginkgo.By("running a targetVRF auto advertised CUDN on the Uplink on every DPU host node")
		// Only networks advertised to a non-default VRF enslave the host
		// interface into the host-side CUDN VRF, hence the advertisement. A
		// pod per node renders the network there.
		networkName := uplinkName
		dpuGatewayNetwork, err := infraprovider.Get().GetNetwork(os.Getenv(uplinkDPUGatewayNetworkEnv))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		frrIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
			infraapi.ExternalContainer{Name: routerContainerName},
			dpuGatewayNetwork,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(frrIface.IPv4).NotTo(gomega.BeEmpty())
		applyUplinkFRRK8sConfiguration(
			ictx,
			networkName,
			[]string{frrIface.IPv4},
			[]string{envOrDefault(uplinkBGPServerIPv4CIDREnv, uplinkDefaultBGPServerIPv4CIDR)},
		)
		bgpAlloc, err := allocators.AllocateBGP(f, ictx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		networkSpec := uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6)
		namespace, err := createUplinkAdvertisedCUDN(
			ctx,
			f,
			ictx,
			networkName,
			networkSpec,
			uplinkName,
			"auto",
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		for i, node := range dpuHostNodes {
			createUplinkNetexecPod(ctx, f, namespace.Name, fmt.Sprintf("client-%s-%d", networkName, i), node.Name)
		}
		waitForCUDNUplinksReady(ctx, f, networkName)

		ginkgo.By("waiting for the host interfaces to be enslaved to the CUDN VRF")
		vrfName := waitForCUDNVRFName(f, networkName)
		for _, node := range dpuHostNodes {
			node := node
			gomega.Eventually(ctx, func() (string, error) {
				return getNodeInterfaceVRF(node.Name, nodeIfaces[node.Name].InfName)
			}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
				gomega.Equal(vrfName),
				"expected host interface %s on node %s to be enslaved to VRF %s",
				nodeIfaces[node.Name].InfName,
				node.Name,
				vrfName,
			)
		}
		waitForUplinkStatesNoDefaultGateways(ctx, f, uplinkName, dpuHostNodes)

		ginkgo.By("adding default routes explicitly to the host-side CUDN VRF routing table")
		// The enslaved interface keeps its routes in the VRF table, so the
		// late default gateway must be discovered there and reach the OVN
		// gateway router that the DPU programs for the host node.
		for _, node := range dpuHostNodes {
			node := node
			gateways, err := nodeInterfaceDefaultGateways(node.Name, nodeIfaces[node.Name].InfName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(gateways).NotTo(gomega.BeEmpty(),
				"expected the host interface of node %s to carry addresses to derive default gateways from", node.Name)
			for family, gateway := range gateways {
				cidr := defaultRouteCIDR(family)
				gomega.Expect(dpuCUDNGatewayRouterHasDefaultRoute(node.Name, networkName, cidr, gateway)).To(gomega.BeFalse(),
					"expected no default route via %s on the gateway router of CUDN %s for node %s before discovery",
					gateway, networkName, node.Name)
				gomega.Expect(configureUplinkStaticRoute(
					ictx,
					nodeIfaces[node.Name].InfName,
					[]string{node.Name},
					cidr,
					gateway,
					false,
					vrfName,
				)).To(gomega.Succeed())
				gomega.Expect(uplinkRouteShownIn(node.Name, "vrf "+vrfName, cidr, gateway)).To(gomega.Succeed(),
					"expected the added default route via %s in host-side CUDN VRF %s on node %s",
					gateway, vrfName, node.Name)
			}

			waitForUplinkStatesDefaultGateways(ctx, f, uplinkName, []corev1.Node{node}, sets.KeySet(gateways))
			for family, gateway := range gateways {
				gomega.Eventually(ctx, func() (bool, error) {
					return dpuCUDNGatewayRouterHasDefaultRoute(node.Name, networkName, defaultRouteCIDR(family), gateway)
				}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.BeTrue(),
					"expected the gateway router of CUDN %s for node %s to acquire a default route via %s",
					networkName, node.Name, gateway)
			}
			waitForUplinkStateGatewayCondition(ctx, f, uplinkName, node.Name,
				metav1.ConditionTrue, uplinkv1alpha1.UplinkStateReasonGatewayConfigured)
		}
	})
})

// removeNodeInterfaceDefaultRoutes removes the default routes of the node
// interface, if any, registering a cleanup that restores them.
func removeNodeInterfaceDefaultRoutes(ictx infraapi.Context, nodeName, ifName string) error {
	// Registered before the first removal so that a later failure still
	// restores the routes already removed.
	var restoreCommands []string
	ictx.AddCleanUpFn(func() error {
		var errs []error
		for _, restoreCommand := range restoreCommands {
			if _, err := execNodeCommand(nodeName, "%s", restoreCommand); err != nil {
				errs = append(errs, fmt.Errorf("failed to restore default route on %s/%s: %w", nodeName, ifName, err))
			}
		}
		return errors.Join(errs...)
	})
	for _, ipCmd := range []string{"ip", "ip -6"} {
		out, err := execNodeCommand(nodeName, "%s route show default dev %s", ipCmd, ifName)
		if err != nil {
			return fmt.Errorf("failed to list default routes on %s/%s: %w", nodeName, ifName, err)
		}
		for _, route := range strings.Split(strings.TrimSpace(out), "\n") {
			route = strings.TrimSpace(route)
			if route == "" {
				continue
			}
			restoreCommands = append(restoreCommands,
				fmt.Sprintf("%s route replace %s dev %s", ipCmd, route, ifName))
			if _, err := execNodeCommand(nodeName, "%s route del %s dev %s", ipCmd, route, ifName); err != nil {
				return fmt.Errorf("failed to remove default route %q on %s/%s: %w",
					route, nodeName, ifName, err)
			}
		}
	}
	return nil
}

// uplinkMasqueradeCIDRs are the OVN-Kubernetes masquerade subnets. ovnkube
// adds a host masquerade address from them to the Uplink gateway interface,
// which must not be mistaken for the interface's uplink subnet.
var uplinkMasqueradeCIDRs = []string{"169.254.0.0/17", "fd69::/112"}

// nodeInterfaceDefaultGateways derives a default gateway per IP family the
// node interface has a global address for, skipping the ovnkube masquerade
// addresses: the first usable IP of the address's subnet, or the next one
// when the interface holds the first usable IP itself. firstUsableIP does
// not apply here: it rejects the self-IP case that this helper supports.
func nodeInterfaceDefaultGateways(nodeName, ifName string) (map[utilnet.IPFamily]string, error) {
	masqueradeNets := make([]*net.IPNet, 0, len(uplinkMasqueradeCIDRs))
	for _, cidr := range uplinkMasqueradeCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, err
		}
		masqueradeNets = append(masqueradeNets, ipNet)
	}
	isMasquerade := func(ip net.IP) bool {
		for _, ipNet := range masqueradeNets {
			if ipNet.Contains(ip) {
				return true
			}
		}
		return false
	}
	out, err := execNodeCommand(nodeName, "ip -j addr show dev %s", ifName)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses of %s/%s: %w", nodeName, ifName, err)
	}
	links, err := parseIPAddrJSON(out)
	if err != nil {
		return nil, fmt.Errorf("failed to parse addresses of %s/%s: %w", nodeName, ifName, err)
	}

	gateways := map[utilnet.IPFamily]string{}
	for _, link := range links {
		for _, addr := range link.AddrInfo {
			family := utilnet.IPv4
			if addr.Family == "inet6" {
				family = utilnet.IPv6
			}
			if _, found := gateways[family]; addr.Scope != "global" || found {
				continue
			}
			ip, ipNet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", addr.Local, addr.PrefixLen))
			if err != nil {
				return nil, fmt.Errorf("failed to parse %s/%d on %s/%s: %w",
					addr.Local, addr.PrefixLen, nodeName, ifName, err)
			}
			if isMasquerade(ip) {
				continue
			}
			if ipv4 := ip.To4(); ipv4 != nil {
				ip = ipv4
				ipNet.IP = ipNet.IP.To4()
			}
			gatewayIP := append(net.IP(nil), ipNet.IP...)
			incrementUplinkIP(gatewayIP)
			if gatewayIP.Equal(ip) {
				incrementUplinkIP(gatewayIP)
			}
			if !ipNet.Contains(gatewayIP) {
				return nil, fmt.Errorf("failed to derive a gateway from %s/%d on %s/%s",
					addr.Local, addr.PrefixLen, nodeName, ifName)
			}
			gateways[family] = gatewayIP.String()
		}
	}
	return gateways, nil
}

// addNodeInterfaceDefaultRoutes installs a default route on the node
// interface for each IP family it has a global address for, using the
// gateways derived by nodeInterfaceDefaultGateways, and returns them by
// family. A cleanup removing the routes is registered.
func addNodeInterfaceDefaultRoutes(
	ictx infraapi.Context,
	nodeName, ifName string,
) (map[utilnet.IPFamily]string, error) {
	gateways, err := nodeInterfaceDefaultGateways(nodeName, ifName)
	if err != nil {
		return nil, err
	}

	// Registered before the first addition so that a later failure still
	// removes the routes already added.
	var deleteCommands []string
	ictx.AddCleanUpFn(func() error {
		var errs []error
		for _, deleteCommand := range deleteCommands {
			if _, err := execNodeCommand(nodeName, "%s", deleteCommand); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
	for family, gateway := range gateways {
		ipCmd := "ip"
		if family == utilnet.IPv6 {
			ipCmd = "ip -6"
		}
		if _, err := execNodeCommand(nodeName,
			"%s route replace default via %s dev %s metric 50000", ipCmd, gateway, ifName); err != nil {
			return nil, fmt.Errorf("failed to add default route via %s on %s/%s: %w",
				gateway, nodeName, ifName, err)
		}
		deleteCommands = append(deleteCommands,
			fmt.Sprintf("%s route del default via %s dev %s metric 50000 2>/dev/null || true",
				ipCmd, gateway, ifName))
	}
	return gateways, nil
}

// defaultRouteCIDR returns the default route destination of the IP family.
func defaultRouteCIDR(family utilnet.IPFamily) string {
	if family == utilnet.IPv6 {
		return "::/0"
	}
	return "0.0.0.0/0"
}

// cudnGRHasRoute reports whether the OVN gateway router of the CUDN on the
// node has a static route to cidr via nextHop.
func cudnGRHasRoute(k8sClient kubernetes.Interface, cudnName, nodeName, cidr, nextHop string) (bool, error) {
	routes, err := cudnGRRoutesForNode(k8sClient, cudnName, nodeName)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(routes, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == cidr && fields[1] == nextHop {
			return true, nil
		}
	}
	return false, nil
}

func uplinkStateCondition(state *unstructured.Unstructured, conditionType string) (*metav1.Condition, error) {
	conditions, err := getConditions(state)
	if err != nil {
		return nil, err
	}
	condition := meta.FindStatusCondition(conditions, conditionType)
	if condition == nil {
		return nil, fmt.Errorf("UplinkState %s has no %s condition", state.GetName(), conditionType)
	}
	return condition, nil
}

// checkUplinkStateResolvedVia verifies which resolution method produced the
// resolved bridge, as reported by the Resolved condition message: the
// methods resolve the same bridge, so nothing else distinguishes a live
// host function path from a silent fallback to the host MAC scan.
func checkUplinkStateResolvedVia(state *unstructured.Unstructured, method string) error {
	resolved, err := uplinkStateCondition(state, uplinkv1alpha1.UplinkStateConditionResolved)
	if err != nil {
		return err
	}
	expected := "Uplink DPU bridge discovery succeeded via " + method
	if resolved.Message != expected {
		return fmt.Errorf("UplinkState %s Resolved message is %q, expected %q",
			state.GetName(), resolved.Message, expected)
	}
	return nil
}

// uplinkSimNetdevRe extracts the PF and function indices from a DPU simulator
// netdev name (<prefix><pfID>-<funcID>, e.g. eth0-16).
var uplinkSimNetdevRe = regexp.MustCompile(`^\D*(\d+)-(\d+)$`)

// checkUplinkStateHostFunction verifies that status.hostFunction was
// published, and that it matches the PF and VF indices encoded in the
// provisioned host interface name when it follows the simulator convention.
func checkUplinkStateHostFunction(state *unstructured.Unstructured, hostInterfaceName string) error {
	_, found, err := unstructured.NestedMap(state.Object, "status", "hostFunction")
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("UplinkState %s has no host function", state.GetName())
	}
	matches := uplinkSimNetdevRe.FindStringSubmatch(hostInterfaceName)
	if len(matches) != 3 {
		return nil
	}
	expectedPF, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return err
	}
	expectedVF, err := strconv.ParseInt(matches[2], 10, 64)
	if err != nil {
		return err
	}
	pfID, _, err := unstructured.NestedInt64(state.Object, "status", "hostFunction", "pfID")
	if err != nil {
		return err
	}
	vfID, vfFound, err := unstructured.NestedInt64(state.Object, "status", "hostFunction", "vfID")
	if err != nil {
		return err
	}
	if !vfFound {
		return fmt.Errorf("UplinkState %s host function for host interface %s has no vfID",
			state.GetName(), hostInterfaceName)
	}
	if pfID != expectedPF || vfID != expectedVF {
		return fmt.Errorf("UplinkState %s host function are pf%dvf%d, expected pf%dvf%d for %s",
			state.GetName(), pfID, vfID, expectedPF, expectedVF, hostInterfaceName)
	}
	return nil
}

func checkUplinkStateCondition(
	state *unstructured.Unstructured,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
) error {
	condition, err := uplinkStateCondition(state, conditionType)
	if err != nil {
		return err
	}
	if condition.Status != status || condition.Reason != reason {
		return fmt.Errorf("UplinkState %s condition %s is %s/%s, expected %s/%s",
			state.GetName(), conditionType,
			condition.Status, condition.Reason, status, reason)
	}
	return nil
}

func runDPUUplinkVRFLiteRouteAdvertisements(
	ctx context.Context,
	f *framework.Framework,
	ictx infraapi.Context,
	schedulableNodes []corev1.Node,
	ipFamilySet sets.Set[utilnet.IPFamily],
	testSuffix string,
	nmStyleAddressing bool,
) {
	ginkgo.GinkgoHelper()

	if !ipFamilySet.Has(utilnet.IPv4) {
		e2eskipper.Skipf("DPU Uplink e2e requires IPv4 on the DPU simulator gateway network")
	}

	networkName := "upvrf" + testSuffix
	dpuGatewayNetworkName := os.Getenv(uplinkDPUGatewayNetworkEnv)
	dpuGatewayNetwork, err := infraprovider.Get().GetNetwork(dpuGatewayNetworkName)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	dpuHostNodes := filterNodesByLabel(schedulableNodes, uplinkDPUHostNodeLabel)
	gomega.Expect(dpuHostNodes).NotTo(gomega.BeEmpty(), "expected at least one ready schedulable DPU host node")
	nodeIfaces := collectDPUHostUplinkInterfaces(dpuHostNodes)

	if nmStyleAddressing {
		// The host-side Uplink VF can be managed by NetworkManager, which
		// sets noprefixroute on the addresses it manages and installs their
		// prefix routes itself, tagged proto kernel: reshape the host
		// interface the same way, so that the enslavement must migrate those
		// routes into the host-side CUDN VRF for the gateway programming to
		// succeed.
		hostInterfaceByNode := make(map[string]string, len(dpuHostNodes))
		for _, node := range dpuHostNodes {
			hostInterfaceByNode[node.Name] = nodeIfaces[node.Name].InfName
		}
		gomega.Expect(configureUplinkNMStyleAddressing(ictx, hostInterfaceByNode)).To(gomega.Succeed())
	}

	// Install a route on each DPU-host Uplink interface before any
	// enslavement, standing in for routing state another agent (e.g. a DHCP
	// client) configured there: it must survive the enslavement into the
	// host-side CUDN VRF.
	const preservedGateway = "198.51.100.254"
	for _, node := range dpuHostNodes {
		gomega.Expect(configureUplinkStaticRoute(
			ictx,
			nodeIfaces[node.Name].InfName,
			[]string{node.Name},
			uplinkPreservedIPv4CIDR,
			preservedGateway,
			true,
			"",
		)).To(gomega.Succeed())
	}

	createUplink(ctx, f, ictx, networkName, dpuHostNodes, nodeIfaces, "")
	waitForUplinkStatesResolved(ctx, f, networkName, os.Getenv(uplinkDPUExpectedBridgeEnv), dpuHostNodes)

	serverCIDRs := []string{
		envOrDefault(uplinkBGPServerIPv4CIDREnv, uplinkDefaultBGPServerIPv4CIDR),
		envOrDefault(uplinkBGPServerIPv6CIDREnv, uplinkDefaultBGPServerIPv6CIDR),
	}
	frrIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
		infraapi.ExternalContainer{Name: routerContainerName},
		dpuGatewayNetwork,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(frrIface.IPv4).NotTo(gomega.BeEmpty())
	applyUplinkFRRK8sConfiguration(
		ictx,
		networkName,
		[]string{frrIface.IPv4},
		[]string{serverCIDRs[0]},
	)

	bgpAlloc, err := allocators.AllocateBGP(f, ictx)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	networkSpec := uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6)
	namespace, err := createUplinkAdvertisedCUDN(
		ctx,
		f,
		ictx,
		networkName,
		networkSpec,
		networkName,
		"auto",
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	var pods []*corev1.Pod
	for _, node := range dpuHostNodes {
		pods = append(pods, createUplinkNetexecPod(
			ctx,
			f,
			namespace.Name,
			"client-"+networkName+"-"+node.Name,
			node.Name,
		))
	}

	// Each side of the split deployment reports its share of the gateway
	// programming on its own condition, and cluster manager aggregates both
	// into the CUDN's UplinksReady.
	for _, node := range dpuHostNodes {
		waitForUplinkStateGatewayCondition(ctx, f, networkName, node.Name,
			metav1.ConditionTrue, uplinkv1alpha1.UplinkStateReasonGatewayConfigured)
		waitForUplinkStateConditionOfType(ctx, f, networkName, node.Name,
			uplinkv1alpha1.UplinkStateConditionHostGatewayReady,
			metav1.ConditionTrue, uplinkv1alpha1.UplinkStateReasonGatewayConfigured)
	}
	waitForCUDNUplinksReady(ctx, f, networkName)

	// The published VRF name is the value FRRConfiguration authors put in the
	// routers 'vrf' field; it must match the VRF the DPU-side bridge is
	// enslaved to. With dynamic network allocation the network is only
	// rendered on a node once a pod attached to it runs there, so this can
	// only be asserted after the pods exist.
	ginkgo.By("verifying the published VRF name matches the VRF enslaving the DPU-side bridge")
	vrfName := waitForCUDNVRFName(f, networkName)
	dpuBridgeName := os.Getenv(uplinkDPUExpectedBridgeEnv)
	for _, node := range dpuHostNodes {
		gomega.Eventually(ctx, func() (string, error) {
			return getUplinkBridgeVRF(node.Name, dpuBridgeName)
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Equal(vrfName),
			"expected the DPU-side bridge %s for host node %s to be enslaved to the published VRF %s",
			dpuBridgeName,
			node.Name,
			vrfName,
		)
	}

	// The pre-existing route was migrated into the host-side CUDN VRF on
	// enslavement.
	for _, node := range dpuHostNodes {
		node := node
		gomega.Eventually(ctx, func() error {
			return uplinkRouteShownIn(node.Name, "vrf "+vrfName, uplinkPreservedIPv4CIDR, preservedGateway)
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected preserved route %s via %s in host-side CUDN VRF %s on node %s",
			uplinkPreservedIPv4CIDR,
			preservedGateway,
			vrfName,
			node.Name,
		)
	}

	// The host interface's prefix routes are present in the host-side CUDN
	// VRF for every enabled IP family: with kernel-owned addressing the
	// kernel regenerates them there itself, while with NetworkManager-style
	// noprefixroute addressing only the migration can put them there.
	for _, family := range ipFamilySet.UnsortedList() {
		for _, node := range dpuHostNodes {
			node := node
			prefixCIDR := nodeInterfacePrefixCIDR(node.Name, nodeIfaces[node.Name].InfName, family)
			gomega.Eventually(ctx, func() error {
				return uplinkKernelRouteShownIn(node.Name, "vrf "+vrfName, prefixCIDR)
			}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
				gomega.Succeed(),
				"expected prefix route %s in host-side CUDN VRF %s on node %s",
				prefixCIDR,
				vrfName,
				node.Name,
			)
		}
	}

	pod := pods[0]
	serverNetwork, err := infraprovider.Get().GetNetwork(bgpExternalNetworkName)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	serverIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(
		infraapi.ExternalContainer{Name: bgpExternalServerContainerName},
		serverNetwork,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	serverCIDR := getFirstCIDRStringOfFamily(utilnet.IPv4, serverCIDRs)
	gomega.Expect(serverCIDR).NotTo(gomega.BeEmpty())
	for _, node := range dpuHostNodes {
		gomega.Eventually(ctx, func() (bool, error) {
			return hasRouteInDPUCUDNVRF(node, vrfName, serverCIDR, frrIface.IPv4)
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.BeTrue(),
			"expected DPU for host node %s to learn %s via %s in CUDN VRF %s",
			node.Name,
			serverCIDR,
			frrIface.IPv4,
			vrfName,
		)
	}

	gomega.Expect(serverIface.IPv4).NotTo(gomega.BeEmpty())
	uplinkPodToHostnameAndExpectReachable(pod, serverIface.IPv4)
	podIP, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(
		f.ClientSet,
		pod.Namespace,
		pod.Name,
		networkName,
		utilnet.IPv4,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(podIP).NotTo(gomega.BeEmpty())
	uplinkPodToClientIPAndExpect(pod, serverIface.IPv4, podIP)
}

func applyUplinkFRRK8sConfiguration(
	ictx infraapi.Context,
	networkName string,
	neighborIPs []string,
	receiveNetworks []string,
) {
	ginkgo.GinkgoHelper()

	frrK8sConfig, err := generateFRRk8sConfiguration(networkName, neighborIPs, receiveNetworks)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ictx.AddCleanUpFn(func() error { return os.RemoveAll(frrK8sConfig) })
	_, err = e2ekubectl.RunKubectl(
		deploymentconfig.Get().FRRK8sNamespace(),
		"create",
		"-f",
		frrK8sConfig,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ictx.AddCleanUpFn(func() error {
		_, err = e2ekubectl.RunKubectl(
			deploymentconfig.Get().FRRK8sNamespace(),
			"delete",
			"-f",
			frrK8sConfig,
		)
		return err
	})
}

func setupUplinkNetwork(
	ictx infraapi.Context,
	nodes []corev1.Node,
	ipFamilySet sets.Set[utilnet.IPFamily],
	fallbackNetworkName string,
	fallbackSubnets []string,
) (infraapi.Network, map[string]infraapi.NetworkInterface) {
	ginkgo.GinkgoHelper()

	network, err := ictx.CreateNetwork(
		fallbackNetworkName,
		matchCIDRStringsByIPFamilySet(fallbackSubnets, ipFamilySet)...,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	nodeIfaces := map[string]infraapi.NetworkInterface{}
	for _, node := range nodes {
		iface, err := ictx.AttachNetwork(network, node.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeIfaces[node.Name] = iface
	}
	return network, nodeIfaces
}

func collectNodeNetworkInterfaces(nodes []corev1.Node, network infraapi.Network) map[string]infraapi.NetworkInterface {
	ginkgo.GinkgoHelper()

	nodeIfaces := map[string]infraapi.NetworkInterface{}
	for _, node := range nodes {
		iface, err := infraprovider.Get().GetK8NodeNetworkInterface(node.Name, network)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeIfaces[node.Name] = iface
	}
	return nodeIfaces
}

func filterNodesByLabel(nodes []corev1.Node, label string) []corev1.Node {
	out := make([]corev1.Node, 0, len(nodes))
	for _, node := range nodes {
		if _, ok := node.Labels[label]; ok {
			out = append(out, node)
		}
	}
	return out
}

func collectDPUHostUplinkInterfaces(nodes []corev1.Node) map[string]infraapi.NetworkInterface {
	ginkgo.GinkgoHelper()

	nodeIfaces := map[string]infraapi.NetworkInterface{}
	if hostInterfaceName := os.Getenv(uplinkDPUHostInterfaceNameEnv); hostInterfaceName != "" {
		for _, node := range nodes {
			nodeIfaces[node.Name] = infraapi.NetworkInterface{InfName: hostInterfaceName}
		}
		return nodeIfaces
	}

	for _, node := range nodes {
		rawAnnotation := node.Annotations[uplinkDPUHostAddrAnnotation]
		gomega.Expect(rawAnnotation).NotTo(
			gomega.BeEmpty(),
			"expected node %s to have annotation %s",
			node.Name,
			uplinkDPUHostAddrAnnotation,
		)
		var dpuHostAddr dpuHostAddrAnnotation
		gomega.Expect(json.Unmarshal([]byte(rawAnnotation), &dpuHostAddr)).To(gomega.Succeed())

		var iface infraapi.NetworkInterface
		if dpuHostAddr.IPv4 != "" {
			ip, prefix, cidr := parseDPUHostCIDR(dpuHostAddr.IPv4)
			iface.InfName = findNodeInterfaceByCIDR(node.Name, cidr)
			iface.IPv4 = ip
			iface.IPv4Prefix = prefix
		}
		if dpuHostAddr.IPv6 != "" {
			ip, prefix, cidr := parseDPUHostCIDR(dpuHostAddr.IPv6)
			if iface.InfName == "" {
				iface.InfName = findNodeInterfaceByCIDR(node.Name, cidr)
			}
			iface.IPv6 = ip
			iface.IPv6Prefix = prefix
		}
		gomega.Expect(iface.InfName).NotTo(
			gomega.BeEmpty(),
			"expected node %s annotation %s to resolve to a host interface",
			node.Name,
			uplinkDPUHostAddrAnnotation,
		)
		nodeIfaces[node.Name] = iface
	}
	return nodeIfaces
}

func parseDPUHostCIDR(cidr string) (string, string, string) {
	ginkgo.GinkgoHelper()

	ip, ipNet, err := net.ParseCIDR(cidr)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	prefix, _ := ipNet.Mask.Size()
	ipString := ip.String()
	prefixString := fmt.Sprint(prefix)
	return ipString, prefixString, ipString + "/" + prefixString
}

func findNodeInterfaceByCIDR(nodeName, cidr string) string {
	ginkgo.GinkgoHelper()

	out, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{
		"sh",
		"-c",
		fmt.Sprintf("ip -o addr show | awk '$4 == \"%s\" {print $2; exit}'", cidr),
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return strings.TrimSpace(out)
}

func configureUplinkBridge(
	ctx context.Context,
	f *framework.Framework,
	ictx infraapi.Context,
	bridgeName string,
	nodeIfaces map[string]infraapi.NetworkInterface,
) error {
	ginkgo.GinkgoHelper()

	ovsPods, err := uplinkOVSPodsByNode(ctx, f)
	if err != nil {
		return err
	}
	// Cleanups run after the spec context is done.
	cleanupUplinkBridge := func() error {
		var errs []error
		ovsPods, err := uplinkOVSPodsByNode(context.Background(), f)
		if err != nil {
			return err
		}
		for nodeName := range nodeIfaces {
			ovsPod, ok := ovsPods[nodeName]
			if !ok {
				errs = append(errs, fmt.Errorf("failed to find ovnkube-node pod on node %q", nodeName))
				continue
			}
			if iface, ok := nodeIfaces[nodeName]; ok {
				if _, err := execNodeCommand(
					nodeName,
					"if ip link show dev %[1]s >/dev/null 2>&1 && "+
						"ip link show dev %[2]s >/dev/null 2>&1; then "+
						"for addr in $(ip -o -4 addr show dev %[1]s scope global | awk '{print $4}'); do "+
						"ip addr add $addr dev %[2]s 2>/dev/null || true; "+
						"ip addr del $addr dev %[1]s 2>/dev/null || true; done; "+
						"for addr in $(ip -o -6 addr show dev %[1]s scope global | awk '{print $4}'); do "+
						"ip addr add $addr dev %[2]s 2>/dev/null || true; "+
						"ip addr del $addr dev %[1]s 2>/dev/null || true; done; fi",
					bridgeName,
					iface.InfName,
				); err != nil {
					errs = append(errs, err)
				}
			}
			if err := runOVSCommand(
				ovsPod,
				"if ovs-vsctl --timeout=15 br-exists %[1]s; then "+
					"ovs-vsctl --timeout=15 del-br %[1]s; fi",
				bridgeName,
			); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	ictx.AddCleanUpFn(cleanupUplinkBridge)
	cleanupOnError := func(setupErr error) error {
		return errors.Join(setupErr, cleanupUplinkBridge())
	}

	for nodeName, iface := range nodeIfaces {
		ovsPod, ok := ovsPods[nodeName]
		if !ok {
			return cleanupOnError(fmt.Errorf("failed to find ovnkube-node pod on node %q", nodeName))
		}
		if err := runOVSCommand(ovsPod, "ovs-vsctl --timeout=15 --may-exist add-br %s", bridgeName); err != nil {
			return cleanupOnError(err)
		}
		if err := runOVSCommand(
			ovsPod,
			"ovs-vsctl --timeout=15 --may-exist add-port %s %s",
			bridgeName,
			iface.InfName,
		); err != nil {
			return cleanupOnError(err)
		}
		if err := runOVSCommand(
			ovsPod,
			"ovs-vsctl --timeout=15 br-set-external-id %s bridge-uplink %s",
			bridgeName,
			iface.InfName,
		); err != nil {
			return cleanupOnError(err)
		}
		if _, err := execNodeCommand(
			nodeName,
			"ip link set dev %[1]s up; "+
				"for addr in $(ip -o -4 addr show dev %[2]s scope global | awk '{print $4}'); do "+
				"ip addr add $addr dev %[1]s 2>/dev/null || true; "+
				"ip addr del $addr dev %[2]s 2>/dev/null || true; done; "+
				"for addr in $(ip -o -6 addr show dev %[2]s scope global | awk '{print $4}'); do "+
				"ip addr add $addr dev %[1]s 2>/dev/null || true; "+
				"ip addr del $addr dev %[2]s 2>/dev/null || true; done",
			bridgeName,
			iface.InfName,
		); err != nil {
			return cleanupOnError(err)
		}
	}
	return nil
}

func configureUplinkBridgeDefaultRoutes(
	ictx infraapi.Context,
	bridgeName string,
	nodeIfaces map[string]infraapi.NetworkInterface,
) error {
	ginkgo.GinkgoHelper()

	for nodeName, iface := range nodeIfaces {
		ipv4Gateway, err := interfaceGateway(
			iface.IPv4Gateway,
			iface.IPv4,
			iface.IPv4Prefix,
		)
		if err != nil {
			return fmt.Errorf("failed to resolve IPv4 gateway for node %s: %w",
				nodeName,
				err,
			)
		}
		if ipv4Gateway != "" {
			if _, err := execNodeCommand(
				nodeName,
				"ip route replace default via %s dev %s metric 50000",
				ipv4Gateway,
				bridgeName,
			); err != nil {
				return err
			}
			if _, err := execNodeCommand(
				nodeName,
				"ip route show default via %s dev %s metric 50000 | grep -q .",
				ipv4Gateway,
				bridgeName,
			); err != nil {
				return fmt.Errorf("failed to verify IPv4 default route via %s on %s/%s: %w",
					ipv4Gateway,
					nodeName,
					bridgeName,
					err,
				)
			}
		}
		ipv6Gateway, err := interfaceGateway(
			iface.IPv6Gateway,
			iface.IPv6,
			iface.IPv6Prefix,
		)
		if err != nil {
			return fmt.Errorf("failed to resolve IPv6 gateway for node %s: %w",
				nodeName,
				err,
			)
		}
		if ipv6Gateway != "" {
			if _, err := execNodeCommand(
				nodeName,
				"ip -6 route replace default via %s dev %s metric 50000",
				ipv6Gateway,
				bridgeName,
			); err != nil {
				return err
			}
			if _, err := execNodeCommand(
				nodeName,
				"ip -6 route show default via %s dev %s metric 50000 | grep -q .",
				ipv6Gateway,
				bridgeName,
			); err != nil {
				return fmt.Errorf("failed to verify IPv6 default route via %s on %s/%s: %w",
					ipv6Gateway,
					nodeName,
					bridgeName,
					err,
				)
			}
		}
	}

	ictx.AddCleanUpFn(func() error {
		var errs []error
		for nodeName, iface := range nodeIfaces {
			ipv4Gateway, err := interfaceGateway(
				iface.IPv4Gateway,
				iface.IPv4,
				iface.IPv4Prefix,
			)
			if err != nil {
				errs = append(errs, err)
			}
			if ipv4Gateway != "" {
				if _, err := execNodeCommand(
					nodeName,
					"ip route del default via %s dev %s metric 50000 2>/dev/null || true",
					ipv4Gateway,
					bridgeName,
				); err != nil {
					errs = append(errs, err)
				}
			}
			ipv6Gateway, err := interfaceGateway(
				iface.IPv6Gateway,
				iface.IPv6,
				iface.IPv6Prefix,
			)
			if err != nil {
				errs = append(errs, err)
			}
			if ipv6Gateway != "" {
				if _, err := execNodeCommand(
					nodeName,
					"ip -6 route del default via %s dev %s metric 50000 2>/dev/null || true",
					ipv6Gateway,
					bridgeName,
				); err != nil {
					errs = append(errs, err)
				}
			}
		}
		return errors.Join(errs...)
	})
	return nil
}

// configureUplinkStaticRoute installs a static route on the Uplink interface
// of the given nodes, mimicking routing state installed by another agent
// (e.g. a DHCP client). An empty vrfName selects the main table; otherwise
// the route is installed in that VRF. With onlink the route is accepted
// regardless of the interface's addressing.
func configureUplinkStaticRoute(
	ictx infraapi.Context,
	devName string,
	nodeNames []string,
	cidr string,
	via string,
	onlink bool,
	vrfName string,
) error {
	ginkgo.GinkgoHelper()

	family := ""
	if utilnet.IsIPv6CIDRString(cidr) {
		family = "-6 "
	}
	onlinkFlag := ""
	if onlink {
		onlinkFlag = "onlink "
	}
	tableSelector := "table main"
	if vrfName != "" {
		tableSelector = "vrf " + vrfName
	}
	ictx.AddCleanUpFn(func() error {
		var errs []error
		for _, nodeName := range nodeNames {
			// The route may sit in the main table or may already be gone with
			// the bridge or the VRF, so deletion is best effort.
			if _, err := execNodeCommand(
				nodeName,
				"ip %sroute del %s via %s dev %s %s metric 50000 2>/dev/null || true",
				family,
				cidr,
				via,
				devName,
				tableSelector,
			); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
	for _, nodeName := range nodeNames {
		// Keep test default routes below the node's management default route.
		if _, err := execNodeCommand(
			nodeName,
			"ip %sroute replace %s via %s dev %s %s%s metric 50000",
			family,
			cidr,
			via,
			devName,
			onlinkFlag,
			tableSelector,
		); err != nil {
			return err
		}
	}
	return nil
}

// configureUplinkNMStyleAddressing reshapes the interfaces' global addresses
// into NetworkManager form: noprefixroute addresses whose proto kernel prefix
// routes the kernel does not regenerate across master changes. Cleanup hands
// the prefix routes back to the kernel. Exotic routes (expires, linkdown,
// ECMP) fail the reshape loudly.
func configureUplinkNMStyleAddressing(ictx infraapi.Context, devByNode map[string]string) error {
	ginkgo.GinkgoHelper()

	// One script, both directions: the noprefixroute flag marks ownership,
	// so forward reshapes unflagged addresses and cleanup undoes flagged
	// ones. The OVN-Kubernetes masquerade addresses (169.254.0.0/17,
	// fd69::/112) and their routes are excluded: ovnkube owns them.
	reshape := func(nodeName, devName, flag, addrFilter, replayTolerance string) error {
		_, err := execNodeCommand(nodeName, `
set -e
dev=%[1]s
# Snapshot the routes the address changes below purge; masquerade routes
# stay out (replaying one can EEXIST against another interface's twin).
v4routes=$(ip -4 route show table all dev $dev | grep -Ev 'table local|169\.254\.' || true)
# v6 risks only the kernel prefix routes (see the flag change below).
v6routes=$(ip -6 route show table all dev $dev proto kernel | grep -Ev '^fe80|table local|fd69:' || true)
# v4 flags cannot change in place: delete and re-add, purging v4 routes.
for addr in $(ip -o -4 addr show dev $dev scope global | grep -Ev ' 169\.254\.' | %[3]s | awk '{print $4}'); do
	ip addr del $addr dev $dev
	ip addr add $addr dev $dev%[2]s
done
# v6 flags change in place: only the address's own prefix route is lost.
for addr in $(ip -o -6 addr show dev $dev scope global | grep -Ev ' fd69:' | %[3]s | awk '{print $4}'); do
	ip addr change $addr dev $dev%[2]s
done
# The dev-filtered capture omits the dev token: re-state it.
replay4() { while read -r route; do if [ -n "$route" ]; then ip -4 route replace $route dev $dev%[4]s; fi; done; }
# Gateway-less routes first: the gateway routes resolve against them.
echo "$v4routes" | grep -v via | replay4
echo "$v4routes" | grep via | replay4
# iproute2 omits the filtered proto from the capture: re-state it.
echo "$v6routes" | while read -r route; do
	if [ -n "$route" ]; then ip -6 route replace $route proto kernel dev $dev%[4]s; fi
done`,
			devName,
			flag,
			addrFilter,
			replayTolerance,
		)
		return err
	}
	// Registered before touching any node so a partial reshape still gets
	// undone; replay is best-effort, ovnkube may be tearing down concurrently.
	ictx.AddCleanUpFn(func() error {
		var errs []error
		for nodeName, devName := range devByNode {
			if err := reshape(nodeName, devName, "", "grep noprefixroute", " || true"); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
	for nodeName, devName := range devByNode {
		if err := reshape(nodeName, devName, " noprefixroute", "grep -v noprefixroute", ""); err != nil {
			return err
		}
	}
	return nil
}

// uplinkKernelRouteShownIn returns nil when a proto kernel route toward the
// given CIDR is present in the given routing table on the node. The CIDR
// selects the address family.
func uplinkKernelRouteShownIn(nodeName, tableSelector, cidr string) error {
	return uplinkRouteMatchShownIn(nodeName, tableSelector, cidr, "proto kernel")
}

// nodeInterfacePrefixCIDR returns the network prefix of the first global
// address of the given family on the node's interface.
func nodeInterfacePrefixCIDR(nodeName, devName string, family utilnet.IPFamily) string {
	ginkgo.GinkgoHelper()

	familyFlag := "-4"
	if family == utilnet.IPv6 {
		familyFlag = "-6"
	}
	out, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{
		"ip", "-j", familyFlag, "addr", "show", "dev", devName, "scope", "global",
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	parsed, err := parseIPAddrJSON(out)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(),
		"failed to parse ip -j output for %s on node %s", devName, nodeName)
	gomega.Expect(parsed).NotTo(gomega.BeEmpty())
	gomega.Expect(parsed[0].AddrInfo).NotTo(gomega.BeEmpty(),
		"expected a global %s address on %s of node %s", family, devName, nodeName)

	_, ipNet, err := net.ParseCIDR(
		fmt.Sprintf("%s/%d", parsed[0].AddrInfo[0].Local, parsed[0].AddrInfo[0].PrefixLen))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return ipNet.String()
}

// uplinkRouteShownIn returns nil when the given route is present in the given
// routing table on the node. tableSelector is an `ip route show` scope
// selector such as "vrf <name>", or empty for the main table.
func uplinkRouteShownIn(nodeName, tableSelector, cidr, via string) error {
	return uplinkRouteMatchShownIn(nodeName, tableSelector, cidr, "via "+via)
}

func uplinkRouteMatchShownIn(nodeName, tableSelector, routeSelector, matchSelector string) error {
	family := ""
	if utilnet.IsIPv6CIDRString(routeSelector) {
		family = "-6 "
	}
	_, err := execNodeCommand(
		nodeName,
		"ip %sroute show %s %s %s | grep -q .",
		family,
		tableSelector,
		routeSelector,
		matchSelector,
	)
	return err
}

// uplinkHostVRFTCPProbe returns nil when a TCP connection from the node's
// network context of the given VRF succeeds toward ip:port, proving the VRF
// routing table holds a working route toward the destination. The host VRF
// context is the consumer of the kernel VRF routing table; pod egress goes
// through the OVN gateway router instead, which derives its routes from
// UplinkState and route import rather than from this table.
func uplinkHostVRFTCPProbe(nodeName, vrfName, ip string, port int) error {
	_, err := execNodeCommand(
		nodeName,
		"timeout 3 ip vrf exec %s bash -c 'exec 3<>/dev/tcp/%s/%d'",
		vrfName,
		ip,
		port,
	)
	return err
}

func interfaceGateway(gateway, ip, prefix string) (string, error) {
	if gateway != "" || ip == "" {
		return gateway, nil
	}
	if prefix == "" {
		return "", fmt.Errorf("interface has IP %s without a prefix", ip)
	}
	return firstUsableIP(fmt.Sprintf("%s/%s", ip, prefix))
}

func firstUsableIP(cidr string) (string, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		ipNet.IP = ipNet.IP.To4()
	}
	gateway := append(net.IP(nil), ipNet.IP...)
	incrementUplinkIP(gateway)
	if !ipNet.Contains(gateway) || gateway.Equal(ip) {
		return "", fmt.Errorf("failed to derive gateway from %s", cidr)
	}
	return gateway.String(), nil
}

func incrementUplinkIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			return
		}
	}
}

// expectSingleUplinkLocalnetPort asserts that the CUDN's external switch on
// nodeName carries exactly one localnet port and OVS exactly one br-int patch
// port for the network, both belonging to bridgeName. Both names embed the
// bridge, so a bridge change must replace them: ovn-controller forwards
// through a single localnet port per datapath, and a leftover from the
// previous bridge keeps egress on a patch port that the replacement bridge's
// flows do not handle.
func expectSingleUplinkLocalnetPort(ctx context.Context, f *framework.Framework, nodeName, cudnName, bridgeName string) {
	ginkgo.GinkgoHelper()

	ovsPods, err := uplinkOVSPodsByNode(ctx, f)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "expected to list ovnkube-node pods")
	ovsPod, ok := ovsPods[nodeName]
	gomega.Expect(ok).To(gomega.BeTrue(), "expected an ovnkube-node pod on node %s", nodeName)

	networkName := ovntypes.CUDNPrefix + cudnName
	scopedNode := util.GetUserDefinedNetworkPrefix(networkName) + nodeName
	expectedLocalnet := util.GetExtPortName(bridgeName, scopedNode)
	expectedPatch := util.GetPatchPortName(bridgeName, scopedNode)

	gomega.Eventually(ctx, func() error {
		out, err := e2epodoutput.RunHostCmd(ovsPod.Namespace, ovsPod.Name, fmt.Sprintf(
			"ovn-nbctl --timeout=15 --bare --columns=name find logical_switch_port type=localnet options:network_name=%s",
			networkName))
		if err != nil {
			return err
		}
		if got := strings.Fields(out); len(got) != 1 || got[0] != expectedLocalnet {
			return fmt.Errorf("network %s on node %s has localnet ports %v, expected exactly [%s]",
				networkName, nodeName, got, expectedLocalnet)
		}
		out, err = e2epodoutput.RunHostCmd(ovsPod.Namespace, ovsPod.Name, fmt.Sprintf(
			"ovs-vsctl --timeout=15 --bare --columns=name find interface type=patch | grep -F -- '_%s%s' || true",
			scopedNode, ovntypes.PatchPortSuffix))
		if err != nil {
			return err
		}
		if got := strings.Fields(out); len(got) != 1 || got[0] != expectedPatch {
			return fmt.Errorf("network %s on node %s has br-int patch ports %v, expected exactly [%s]",
				networkName, nodeName, got, expectedPatch)
		}
		return nil
	}).WithTimeout(uplinkShortTimeout).WithPolling(uplinkPoll).Should(gomega.Succeed(),
		"expected a single localnet port and patch port for network %s on bridge %s", networkName, bridgeName)
}

func uplinkOVSPodsByNode(ctx context.Context, f *framework.Framework) (map[string]corev1.Pod, error) {
	pods, err := f.ClientSet.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(
		ctx,
		metav1.ListOptions{LabelSelector: "app=ovnkube-node"},
	)
	if err != nil {
		return nil, err
	}
	byNode := map[string]corev1.Pod{}
	for _, pod := range pods.Items {
		byNode[pod.Spec.NodeName] = pod
	}
	return byNode, nil
}

func runOVSCommand(pod corev1.Pod, format string, args ...any) error {
	ginkgo.GinkgoHelper()
	cmd := fmt.Sprintf(format, args...)
	_, err := e2epodoutput.RunHostCmdWithRetries(
		pod.Namespace,
		pod.Name,
		cmd,
		uplinkPoll,
		uplinkShortTimeout,
	)
	return err
}

// execNodeCommand runs a shell command on the node through the
// provider-agnostic node exec API, returning the command output.
// uplinkLinkIndex returns the interface index of a link on the node.
func uplinkLinkIndex(nodeName, linkName string) string {
	ginkgo.GinkgoHelper()
	out, err := execNodeCommand(nodeName, "cat /sys/class/net/%s/ifindex", linkName)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "expected link %s on node %s", linkName, nodeName)
	return strings.TrimSpace(out)
}

func execNodeCommand(nodeName, format string, args ...any) (string, error) {
	ginkgo.GinkgoHelper()
	return infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"sh", "-c", fmt.Sprintf(format, args...)})
}

func createUplink(
	ctx context.Context,
	f *framework.Framework,
	ictx infraapi.Context,
	name string,
	nodes []corev1.Node,
	nodeIfaces map[string]infraapi.NetworkInterface,
	hostInterfaceName string,
) {
	ginkgo.GinkgoHelper()

	nodeConfigs := make([]interface{}, 0, len(nodes))
	for _, node := range nodes {
		iface, ok := nodeIfaces[node.Name]
		gomega.Expect(ok).To(gomega.BeTrue(), "expected Uplink interface for node %s", node.Name)
		hostname, ok := node.Labels[corev1.LabelHostname]
		gomega.Expect(ok).To(gomega.BeTrue(), "expected node %s to have label %q", node.Name, corev1.LabelHostname)
		nodeHostInterfaceName := hostInterfaceName
		if nodeHostInterfaceName == "" {
			nodeHostInterfaceName = iface.InfName
		}
		nodeConfigs = append(nodeConfigs, map[string]interface{}{
			"type": "OVSBridge",
			"nodeSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					corev1.LabelHostname: hostname,
				},
			},
			"hostInterfaceName": nodeHostInterfaceName,
		})
	}

	_, err := f.DynamicClient.Resource(uplinkGVR).Create(
		ctx,
		&unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "k8s.ovn.org/v1alpha1",
				"kind":       "Uplink",
				"metadata": map[string]interface{}{
					"name": name,
				},
				"spec": map[string]interface{}{
					"nodeConfigs": nodeConfigs,
				},
			},
		},
		metav1.CreateOptions{},
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	// Cleanups run after the spec context is done.
	ictx.AddCleanUpFn(func() error {
		return f.DynamicClient.Resource(uplinkGVR).Delete(context.Background(), name, metav1.DeleteOptions{})
	})
}

func waitForUplinkStatesResolved(ctx context.Context, f *framework.Framework, uplinkName string, bridgeName string, nodes []corev1.Node) {
	ginkgo.GinkgoHelper()

	for _, node := range nodes {
		node := node
		gomega.Eventually(ctx, func() error {
			state, err := getUplinkState(ctx, f, uplinkName, node.Name)
			if err != nil {
				return err
			}
			conditions, err := getConditions(state)
			if err != nil {
				return err
			}
			var resolved *metav1.Condition
			for i := range conditions {
				if conditions[i].Type == uplinkv1alpha1.UplinkStateConditionResolved {
					resolved = &conditions[i]
					break
				}
			}
			if resolved == nil || resolved.Status != metav1.ConditionTrue {
				return fmt.Errorf("UplinkState %s is not resolved: %#v", state.GetName(), resolved)
			}
			resolvedBridge, _, err := unstructured.NestedString(state.Object, "status", "ovsBridge", "name")
			if err != nil {
				return err
			}
			if bridgeName != "" && resolvedBridge != bridgeName {
				return fmt.Errorf("UplinkState %s resolved bridge %q, expected %q",
					state.GetName(),
					resolvedBridge,
					bridgeName,
				)
			}
			if bridgeName == "" && resolvedBridge == "" {
				return fmt.Errorf("UplinkState %s has no resolved bridge", state.GetName())
			}
			ipAddresses, _, err := unstructured.NestedStringSlice(state.Object, "status", "ipAddresses")
			if err != nil {
				return err
			}
			if len(ipAddresses) == 0 {
				return fmt.Errorf("UplinkState %s has no IP addresses", state.GetName())
			}
			return nil
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected UplinkState for uplink %q on node %q to become resolved",
			uplinkName,
			node.Name,
		)
	}
}

func waitForUplinkStatesDefaultGateways(
	ctx context.Context,
	f *framework.Framework,
	uplinkName string,
	nodes []corev1.Node,
	ipFamilySet sets.Set[utilnet.IPFamily],
) {
	ginkgo.GinkgoHelper()

	for _, node := range nodes {
		node := node
		gomega.Eventually(ctx, func() error {
			state, err := getUplinkState(ctx, f, uplinkName, node.Name)
			if err != nil {
				return err
			}
			defaultGateways, _, err := unstructured.NestedStringSlice(
				state.Object,
				"status",
				"defaultGateways",
			)
			if err != nil {
				return err
			}
			families := sets.New[utilnet.IPFamily]()
			for _, defaultGateway := range defaultGateways {
				ip := net.ParseIP(defaultGateway)
				if ip == nil {
					return fmt.Errorf(
						"UplinkState %s has invalid default gateway %q",
						state.GetName(),
						defaultGateway,
					)
				}
				if utilnet.IsIPv6(ip) {
					families.Insert(utilnet.IPv6)
				} else {
					families.Insert(utilnet.IPv4)
				}
			}
			for family := range ipFamilySet {
				if !families.Has(family) {
					return fmt.Errorf(
						"UplinkState %s default gateways %v are missing %s",
						state.GetName(),
						defaultGateways,
						family,
					)
				}
			}
			return nil
		}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected UplinkState for uplink %q on node %q to publish default gateways",
			uplinkName,
			node.Name,
		)
	}
}

// waitForUplinkStatesNoDefaultGateways verifies that the UplinkStates keep
// publishing no default gateways: the host interfaces have no default route,
// and discovery must report that truthfully rather than invent one.
func waitForUplinkStatesNoDefaultGateways(
	ctx context.Context,
	f *framework.Framework,
	uplinkName string,
	nodes []corev1.Node,
) {
	ginkgo.GinkgoHelper()

	for _, node := range nodes {
		node := node
		gomega.Consistently(ctx, func() error {
			state, err := getUplinkState(ctx, f, uplinkName, node.Name)
			if err != nil {
				return err
			}
			defaultGateways, _, err := unstructured.NestedStringSlice(
				state.Object,
				"status",
				"defaultGateways",
			)
			if err != nil {
				return err
			}
			if len(defaultGateways) > 0 {
				return fmt.Errorf(
					"UplinkState %s unexpectedly published default gateways %v",
					state.GetName(),
					defaultGateways,
				)
			}
			return nil
		}).WithTimeout(uplinkConditionStableWindow).WithPolling(uplinkPoll).Should(
			gomega.Succeed(),
			"expected UplinkState for uplink %q on node %q to publish no default gateways",
			uplinkName,
			node.Name,
		)
	}
}

func getUplinkState(ctx context.Context, f *framework.Framework, uplinkName string, nodeName string) (*unstructured.Unstructured, error) {
	fieldSelector := fields.AndSelectors(
		fields.OneTermEqualSelector("spec.uplinkName", uplinkName),
		fields.OneTermEqualSelector("spec.nodeName", nodeName),
	).String()
	stateList, err := f.DynamicClient.Resource(uplinkStateGVR).List(
		ctx,
		metav1.ListOptions{FieldSelector: fieldSelector},
	)
	if err != nil {
		return nil, err
	}
	if len(stateList.Items) == 0 {
		return nil, fmt.Errorf("failed to find UplinkState for uplink %q on node %q: %w",
			uplinkName, nodeName, errUplinkStateNotFound)
	}
	return &stateList.Items[0], nil
}

// setUplinkNodeConfigHostname rewrites the hostname selector of the Uplink
// nodeConfig currently selecting fromHostname, selecting (or deselecting) the
// matching node without touching the other nodeConfigs.
func setUplinkNodeConfigHostname(ctx context.Context, f *framework.Framework, uplinkName, fromHostname, toHostname string) {
	ginkgo.GinkgoHelper()

	gomega.Eventually(ctx, func() error {
		return updateUplinkNodeConfig(ctx, f, uplinkName, fromHostname, func(nodeConfig map[string]interface{}) error {
			return unstructured.SetNestedField(
				nodeConfig, toHostname, "nodeSelector", "matchLabels", corev1.LabelHostname,
			)
		})
	}).WithTimeout(uplinkShortTimeout).WithPolling(uplinkPoll).Should(
		gomega.Succeed(),
		"expected to update Uplink %q nodeConfig selector from %q to %q",
		uplinkName,
		fromHostname,
		toHostname,
	)
}

// setUplinkNodeConfigHostInterfaceName changes the interface selected for one
// node without changing which nodes the Uplink selects.
func setUplinkNodeConfigHostInterfaceName(
	ctx context.Context,
	f *framework.Framework,
	uplinkName, hostname, hostInterfaceName string,
) {
	ginkgo.GinkgoHelper()

	gomega.Eventually(ctx, func() error {
		return updateUplinkNodeConfig(ctx, f, uplinkName, hostname, func(nodeConfig map[string]interface{}) error {
			return unstructured.SetNestedField(nodeConfig, hostInterfaceName, "hostInterfaceName")
		})
	}).WithTimeout(uplinkShortTimeout).WithPolling(uplinkPoll).Should(
		gomega.Succeed(),
		"expected to update Uplink %q nodeConfig for hostname %q to interface %q",
		uplinkName,
		hostname,
		hostInterfaceName,
	)
}

func updateUplinkNodeConfig(
	ctx context.Context,
	f *framework.Framework,
	uplinkName, hostname string,
	update func(map[string]interface{}) error,
) error {
	getCtx, cancelGet := context.WithTimeout(ctx, uplinkShortTimeout)
	uplink, err := f.DynamicClient.Resource(uplinkGVR).Get(
		getCtx, uplinkName, metav1.GetOptions{})
	cancelGet()
	if err != nil {
		return err
	}
	nodeConfigs, _, err := unstructured.NestedSlice(uplink.Object, "spec", "nodeConfigs")
	if err != nil {
		return err
	}
	found := false
	for i := range nodeConfigs {
		nodeConfig, ok := nodeConfigs[i].(map[string]interface{})
		if !ok {
			continue
		}
		selectedHostname, _, err := unstructured.NestedString(
			nodeConfig,
			"nodeSelector",
			"matchLabels",
			corev1.LabelHostname,
		)
		if err != nil {
			return err
		}
		if selectedHostname != hostname {
			continue
		}
		if err := update(nodeConfig); err != nil {
			return err
		}
		nodeConfigs[i] = nodeConfig
		found = true
	}
	if !found {
		return fmt.Errorf("uplink %s has no nodeConfig selecting hostname %q", uplinkName, hostname)
	}
	if err := unstructured.SetNestedSlice(uplink.Object, nodeConfigs, "spec", "nodeConfigs"); err != nil {
		return err
	}
	updateCtx, cancelUpdate := context.WithTimeout(ctx, uplinkShortTimeout)
	_, err = f.DynamicClient.Resource(uplinkGVR).Update(
		updateCtx, uplink, metav1.UpdateOptions{})
	cancelUpdate()
	return err
}

func createNodeScopedUplinkFRRConfiguration(
	ctx context.Context,
	f *framework.Framework,
	ictx infraapi.Context,
	configurationName string,
	networkName string,
	node corev1.Node,
	neighborIPs []string,
) error {
	hostname := node.Labels[corev1.LabelHostname]
	if hostname == "" {
		return fmt.Errorf("node %s has no %s label", node.Name, corev1.LabelHostname)
	}
	if len(neighborIPs) == 0 {
		return fmt.Errorf("no BGP neighbor addresses found for node %s", node.Name)
	}
	neighbors := make([]interface{}, 0, len(neighborIPs))
	for _, neighborIP := range neighborIPs {
		neighbors = append(neighbors, map[string]interface{}{
			"address": neighborIP,
			"asn":     int64(64512),
		})
	}
	client := f.DynamicClient.Resource(uplinkFRRConfigurationGVR).Namespace(
		deploymentconfig.Get().FRRK8sNamespace(),
	)
	_, err := client.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1",
		"kind":       "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name":   configurationName,
			"labels": map[string]interface{}{"network": networkName},
		},
		"spec": map[string]interface{}{
			"nodeSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{corev1.LabelHostname: hostname},
			},
			"bgp": map[string]interface{}{
				"routers": []interface{}{map[string]interface{}{
					"asn":       int64(64512),
					"vrf":       networkName,
					"neighbors": neighbors,
				}},
			},
		},
	}}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create node-scoped FRRConfiguration %s: %w", configurationName, err)
	}
	// Cleanups run after the spec context is done.
	ictx.AddCleanUpFn(func() error {
		return client.Delete(context.Background(), configurationName, metav1.DeleteOptions{})
	})
	return nil
}

func waitForUplinkStateGatewayCondition(
	ctx context.Context,
	f *framework.Framework,
	uplinkName string,
	nodeName string,
	expectedStatus metav1.ConditionStatus,
	expectedReason string,
) {
	ginkgo.GinkgoHelper()
	waitForUplinkStateConditionOfType(ctx, f, uplinkName, nodeName,
		uplinkv1alpha1.UplinkStateConditionGatewayReady, expectedStatus, expectedReason)
}

func waitForUplinkStateConditionOfType(
	ctx context.Context,
	f *framework.Framework,
	uplinkName string,
	nodeName string,
	conditionType string,
	expectedStatus metav1.ConditionStatus,
	expectedReason string,
) {
	ginkgo.GinkgoHelper()

	gomega.Eventually(ctx, func() error {
		state, err := getUplinkState(ctx, f, uplinkName, nodeName)
		if err != nil {
			return err
		}
		conditions, err := getConditions(state)
		if err != nil {
			return err
		}
		for _, condition := range conditions {
			if condition.Type != conditionType {
				continue
			}
			if condition.Status == expectedStatus && condition.Reason == expectedReason {
				return nil
			}
			return fmt.Errorf("UplinkState %s %s condition is %s/%s, expected %s/%s",
				state.GetName(), conditionType, condition.Status, condition.Reason, expectedStatus, expectedReason)
		}
		return fmt.Errorf("UplinkState %s has no %s condition", state.GetName(), conditionType)
	}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.Succeed())
}

func waitForCUDNUplinksReady(ctx context.Context, f *framework.Framework, cudnName string) {
	ginkgo.GinkgoHelper()

	waitForCUDNUplinksCondition(
		ctx,
		f,
		cudnName,
		metav1.ConditionTrue,
		"UplinksReady",
	)
}

func waitForCUDNUplinksCondition(
	ctx context.Context,
	f *framework.Framework,
	cudnName string,
	expectedStatus metav1.ConditionStatus,
	expectedReason string,
) {
	ginkgo.GinkgoHelper()

	client := f.DynamicClient.Resource(clusterUDNGVR)
	gomega.Eventually(ctx, func() error {
		cudn, err := client.Get(ctx, cudnName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		conditions, err := getConditions(cudn)
		if err != nil {
			return err
		}
		for _, condition := range conditions {
			if condition.Type != "UplinksReady" {
				continue
			}
			if condition.Status == expectedStatus && condition.Reason == expectedReason {
				return nil
			}
			return fmt.Errorf("CUDN %s UplinksReady condition is %s/%s (%s), expected %s/%s",
				cudnName, condition.Status, condition.Reason, condition.Message, expectedStatus, expectedReason)
		}
		return fmt.Errorf("CUDN %s has no UplinksReady condition", cudnName)
	}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.Succeed())
}

// waitForCUDNVRFName waits until the CUDN publishes the derived VRF device
// name on status.vrfName and returns it.
func waitForCUDNVRFName(f *framework.Framework, cudnName string) string {
	ginkgo.GinkgoHelper()

	return waitForNetworkVRFName(f.DynamicClient.Resource(clusterUDNGVR), cudnName, uplinkTimeout, uplinkPoll)
}

// setupUplinkLayer3CUDN allocates a BGP subnet, creates a namespace and a
// Layer3 primary CUDN named networkName attached to the given Uplink, and
// returns the namespace.
func setupUplinkLayer3CUDN(
	ctx context.Context,
	f *framework.Framework,
	ictx infraapi.Context,
	ipFamilySet sets.Set[utilnet.IPFamily],
	networkName string,
	uplinkName string,
) *corev1.Namespace {
	ginkgo.GinkgoHelper()

	bgpAlloc, err := allocators.AllocateBGP(f, ictx)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	namespace, err := createUplinkNamespace(
		ctx,
		f,
		ictx,
		"uplink-default",
		networkName,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(createUplinkCUDN(
		ctx,
		f,
		ictx,
		namespace,
		networkName,
		uplinkLayer3NetworkSpec(ipFamilySet, bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6),
		nil,
		uplinkName,
	)).To(gomega.Succeed())
	return namespace
}

// getNADNetworkID returns the network ID annotated on the given
// NetworkAttachmentDefinition, waiting for the annotation to be set.
func getNADNetworkID(ctx context.Context, f *framework.Framework, namespace, nadName string) string {
	ginkgo.GinkgoHelper()

	nadClient, err := nadclient.NewForConfig(f.ClientConfig())
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	var networkID string
	gomega.Eventually(ctx, func() error {
		ctx, cancel := context.WithTimeout(ctx, uplinkShortTimeout)
		defer cancel()
		nad, err := nadClient.NetworkAttachmentDefinitions(namespace).Get(ctx, nadName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		networkID = nad.Annotations[ovntypes.OvnNetworkIDAnnotation]
		if networkID == "" {
			return fmt.Errorf("NAD %s/%s has no network-id annotation", namespace, nadName)
		}
		return nil
	}).WithTimeout(uplinkShortTimeout).WithPolling(uplinkPoll).Should(gomega.Succeed(),
		"expected NAD %s/%s to be annotated with a network ID", namespace, nadName)
	return networkID
}

// nodeVRFDeviceExists verifies a VRF device with the given name exists on the node.
func nodeVRFDeviceExists(nodeName, vrfName string) error {
	out, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "-d", "-j", "link", "show", "dev", vrfName})
	if err != nil {
		return fmt.Errorf("failed to get device %s on node %s: %w", vrfName, nodeName, err)
	}
	link, err := parseSingleLink(out, vrfName)
	if err != nil {
		return err
	}
	if link.LinkInfo.InfoKind != "vrf" {
		return fmt.Errorf("device %s on node %s is not a VRF; output: %s", vrfName, nodeName, out)
	}
	return nil
}

func createUplinkAdvertisedCUDN(
	ctx context.Context,
	f *framework.Framework,
	ictx infraapi.Context,
	networkName string,
	networkSpec *udnv1.NetworkSpec,
	uplinkName string,
	targetVRF string,
) (*corev1.Namespace, error) {
	ginkgo.GinkgoHelper()

	networkLabels := map[string]string{"advertise": networkName}
	namespace, err := createUplinkNamespace(ctx, f, ictx, "uplink-bgp", networkName)
	if err != nil {
		return nil, err
	}
	if err := createUplinkCUDN(ctx, f, ictx, namespace, networkName, networkSpec, networkLabels, uplinkName); err != nil {
		return nil, err
	}
	if err := createRouteAdvertisements(
		f,
		ictx,
		networkName,
		targetVRF,
		networkLabels,
		map[string]string{"network": networkName},
	); err != nil {
		return nil, err
	}
	return namespace, nil
}

func createUplinkNamespace(
	ctx context.Context,
	f *framework.Framework,
	ictx infraapi.Context,
	testName string,
	networkName string,
) (*corev1.Namespace, error) {
	nsLabels := map[string]string{
		"e2e-framework":           testName,
		RequiredUDNNamespaceLabel: "",
	}
	namespace, err := f.CreateNamespace(ctx, networkName, nsLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create namespace: %w", err)
	}
	// Cleanups run after the spec context is done.
	ictx.AddCleanUpFn(func() error {
		return f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), namespace.Name, metav1.DeleteOptions{})
	})
	return namespace, nil
}

func createUplinkPlainNamespace(
	ctx context.Context,
	f *framework.Framework,
	ictx infraapi.Context,
	testName string,
	networkName string,
) (*corev1.Namespace, error) {
	nsLabels := map[string]string{
		"e2e-framework": testName,
	}
	namespace, err := f.CreateNamespace(ctx, networkName, nsLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create namespace: %w", err)
	}
	// Cleanups run after the spec context is done.
	ictx.AddCleanUpFn(func() error {
		return f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), namespace.Name, metav1.DeleteOptions{})
	})
	return namespace, nil
}

func createUplinkCUDN(
	ctx context.Context,
	f *framework.Framework,
	ictx infraapi.Context,
	namespace *corev1.Namespace,
	name string,
	networkSpec *udnv1.NetworkSpec,
	networkLabels map[string]string,
	uplinkName string,
) error {
	networkSpecMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(networkSpec)
	if err != nil {
		return fmt.Errorf("failed to convert network spec to unstructured: %w", err)
	}
	metadata := map[string]interface{}{"name": name}
	if len(networkLabels) > 0 {
		metadata["labels"] = stringMapToInterfaceMap(networkLabels)
	}
	if isDPUUplinkE2E() {
		metadata["annotations"] = map[string]interface{}{
			uplinkDPUResourceNameAnnotation: dpuUplinkResourceName(),
		}
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.ovn.org/v1",
		"kind":       "ClusterUserDefinedNetwork",
		"metadata":   metadata,
		"spec": map[string]interface{}{
			"namespaceSelector": map[string]interface{}{
				"matchExpressions": []interface{}{map[string]interface{}{
					"key":      "kubernetes.io/metadata.name",
					"operator": "In",
					"values":   []interface{}{namespace.Name},
				}},
			},
			"network": networkSpecMap,
			"uplinks": []interface{}{uplinkName},
		},
	}}
	client := f.DynamicClient.Resource(clusterUDNGVR)
	if _, err := client.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create CUDN %s: %w", name, err)
	}
	// Cleanups run after the spec context is done.
	ictx.AddCleanUpFn(func() error {
		return client.Delete(context.Background(), name, metav1.DeleteOptions{})
	})
	gomega.Eventually(ctx, networkReadyFunc(ctx, client, name)).
		WithTimeout(uplinkTimeout).
		WithPolling(uplinkPoll).
		Should(gomega.Succeed(), "expected CUDN %s to become ready", name)
	return nil
}

func stringMapToInterfaceMap(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func uplinkLayer3NetworkSpec(
	ipFamilySet sets.Set[utilnet.IPFamily],
	ipv4Subnet string,
	ipv6Subnet string,
) *udnv1.NetworkSpec {
	return &udnv1.NetworkSpec{
		Topology: udnv1.NetworkTopologyLayer3,
		Layer3: &udnv1.Layer3Config{
			Role: udnv1.NetworkRolePrimary,
			Subnets: matchL3SubnetsByIPFamilies(
				ipFamilySet,
				udnv1.Layer3Subnet{CIDR: udnv1.CIDR(ipv4Subnet)},
				udnv1.Layer3Subnet{CIDR: udnv1.CIDR(ipv6Subnet)},
			),
		},
	}
}

func createUplinkNetexecPod(ctx context.Context, f *framework.Framework, namespace string, name string, nodeName string) *corev1.Pod {
	ginkgo.GinkgoHelper()

	pod := e2epod.NewAgnhostPod(namespace, name, nil, nil, nil)
	pod.Spec.Containers[0].Args = []string{"netexec"}
	if nodeName != "" {
		pod.Spec.NodeName = nodeName
	}
	addDPUUplinkResourceRequest(pod)
	return e2epod.PodClientNS(f, namespace).CreateSync(ctx, pod)
}

func createUplinkServicePod(ctx context.Context, f *framework.Framework, namespace string, name string, nodeName string) *corev1.Pod {
	ginkgo.GinkgoHelper()

	labels := map[string]string{"app": name}
	pod := e2epod.NewAgnhostPod(
		namespace,
		name,
		nil,
		nil,
		[]corev1.ContainerPort{{ContainerPort: netexecPort}},
		"netexec",
	)
	pod.Labels = labels
	pod.Spec.NodeName = nodeName
	addDPUUplinkResourceRequest(pod)
	return e2epod.PodClientNS(f, namespace).CreateSync(ctx, pod)
}

func addDPUUplinkResourceRequest(pod *corev1.Pod) {
	ginkgo.GinkgoHelper()

	if !isDPUUplinkE2E() {
		return
	}

	resourceName := corev1.ResourceName(dpuUplinkResourceName())
	quantity := resource.MustParse("2")
	container := &pod.Spec.Containers[0]
	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}
	container.Resources.Requests[resourceName] = quantity
	container.Resources.Limits[resourceName] = quantity
}

func createUplinkNodePortService(ctx context.Context, f *framework.Framework, namespace string, selector map[string]string) *corev1.Service {
	ginkgo.GinkgoHelper()

	service := e2eservice.CreateServiceSpec("server", "", false, selector)
	service.Spec.Type = corev1.ServiceTypeNodePort
	preferDualStack := corev1.IPFamilyPolicyPreferDualStack
	service.Spec.IPFamilyPolicy = &preferDualStack
	service.Spec.Ports[0].Port = netexecPort
	service.Spec.Ports[0].TargetPort = intstr.FromInt32(netexecPort)
	service, err := f.ClientSet.CoreV1().Services(namespace).Create(
		ctx,
		service,
		metav1.CreateOptions{},
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return service
}

func uplinkPodToHostnameAndExpect(src *corev1.Pod, dstIP, expect string) {
	ginkgo.GinkgoHelper()

	hostname, err := e2epodoutput.RunHostCmdWithRetries(
		src.Namespace,
		src.Name,
		fmt.Sprintf("curl --max-time %d -g -q -s http://%s/hostname", uplinkCurlMaxTime, net.JoinHostPort(dstIP, fmt.Sprintf("%d", netexecPort))),
		uplinkPoll,
		uplinkTimeout,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(hostname).To(gomega.Equal(expect))
}

func uplinkPodToHostnameAndExpectReachable(src *corev1.Pod, dstIP string) {
	ginkgo.GinkgoHelper()

	hostname, err := e2epodoutput.RunHostCmdWithRetries(
		src.Namespace,
		src.Name,
		fmt.Sprintf("curl --max-time %d -g -q -s http://%s/hostname", uplinkCurlMaxTime, net.JoinHostPort(dstIP, fmt.Sprintf("%d", netexecPort))),
		uplinkPoll,
		uplinkTimeout,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(strings.TrimSpace(hostname)).NotTo(gomega.BeEmpty())
}

func uplinkPodToClientIPAndExpect(src *corev1.Pod, dstIP, expect string) {
	ginkgo.GinkgoHelper()

	ip, err := e2epodoutput.RunHostCmdWithRetries(
		src.Namespace,
		src.Name,
		fmt.Sprintf("curl --max-time %d -g -q -s http://%s/clientip", uplinkCurlMaxTime, net.JoinHostPort(dstIP, fmt.Sprintf("%d", netexecPort))),
		uplinkPoll,
		uplinkTimeout,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ip, _, err = net.SplitHostPort(ip)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(ip).To(gomega.Equal(expect))
}

func uplinkExternalToNodePortAndExpect(
	ctx context.Context,
	container infraapi.ExternalContainer,
	dstIP string,
	nodePort int32,
	expect string,
) {
	ginkgo.GinkgoHelper()

	target := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(dstIP, fmt.Sprint(nodePort)))
	gomega.Eventually(ctx, func() (string, error) {
		return infraprovider.Get().ExecExternalContainerCommand(container, []string{
			"curl",
			"--max-time",
			fmt.Sprint(uplinkCurlMaxTime),
			"-g",
			"-q",
			"-s",
			target,
		})
	}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.Equal(expect))
}

func uplinkExternalToPodAndExpect(ctx context.Context, container infraapi.ExternalContainer, dstIP string, expect string) {
	ginkgo.GinkgoHelper()

	target := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(dstIP, fmt.Sprint(netexecPort)))
	gomega.Eventually(ctx, func() (string, error) {
		return infraprovider.Get().ExecExternalContainerCommand(container, []string{
			"curl",
			"--max-time",
			fmt.Sprint(uplinkCurlMaxTime),
			"-g",
			"-q",
			"-s",
			target,
		})
	}).WithTimeout(uplinkTimeout).WithPolling(uplinkPoll).Should(gomega.Equal(expect))
}

func getFirstCIDRStringOfFamily(family utilnet.IPFamily, cidrs []string) string {
	for _, cidr := range cidrs {
		if utilnet.IPFamilyOfCIDRString(cidr) == family {
			return cidr
		}
	}
	return ""
}

func getNodeInterfaceVRF(nodeName, interfaceName string) (string, error) {
	out, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "-j", "link", "show", "dev", interfaceName})
	if err != nil {
		return "", fmt.Errorf("failed to get interface %s on node %s: %w", interfaceName, nodeName, err)
	}
	return interfaceMaster(out, interfaceName)
}

func getUplinkBridgeVRF(hostNodeName, bridgeName string) (string, error) {
	if !isDPUUplinkE2E() {
		return getNodeInterfaceVRF(hostNodeName, bridgeName)
	}

	dpuNodeName, err := dpuNodeNameForHostNode(hostNodeName)
	if err != nil {
		return "", err
	}
	out, err := ForContainer(dpuNodeName).Exec("ip", "-j", "link", "show", "dev", bridgeName)
	if err != nil {
		return "", fmt.Errorf("failed to get bridge %s on DPU node %s: %w", bridgeName, dpuNodeName, err)
	}
	return interfaceMaster(out, bridgeName)
}

// nodeLink is the subset of `ip -j link show` output the tests inspect; the
// linkinfo details are only present when the dump is taken with `-d`.
type nodeLink struct {
	Master   string `json:"master"`
	LinkInfo struct {
		InfoKind string `json:"info_kind"`
	} `json:"linkinfo"`
}

func parseSingleLink(output, interfaceName string) (*nodeLink, error) {
	var links []nodeLink
	if err := json.Unmarshal([]byte(output), &links); err != nil {
		return nil, fmt.Errorf("failed to parse interface %s: %w; output: %s", interfaceName, err, output)
	}
	if len(links) != 1 {
		return nil, fmt.Errorf("expected one link for interface %s, got %d; output: %s", interfaceName, len(links), output)
	}
	return &links[0], nil
}

func interfaceMaster(output, interfaceName string) (string, error) {
	link, err := parseSingleLink(output, interfaceName)
	if err != nil {
		return "", err
	}
	return link.Master, nil
}

// bgpNextHopsForPeer returns the next hops a BGP route learned from the peer
// may carry in the kernel: the peer's address of the given family and, for
// IPv6, its EUI-64 link-local, since a directly connected peer may advertise
// IPv6 routes with its link-local as next hop rather than its global address.
func bgpNextHopsForPeer(family utilnet.IPFamily, peer infraapi.NetworkInterface) []string {
	nextHops := []string{getFirstIPStringOfFamily(family, []string{peer.IPv4, peer.IPv6})}
	if family != utilnet.IPv6 {
		return nextHops
	}
	if ll := ipv6LinkLocalFromMAC(peer.MAC); ll != "" {
		nextHops = append(nextHops, ll)
	}
	return nextHops
}

// ipv6LinkLocalFromMAC returns the EUI-64 IPv6 link-local address derived
// from the given MAC, or an empty string if the MAC cannot be parsed.
func ipv6LinkLocalFromMAC(mac string) string {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return ""
	}
	return net.IP{
		0xfe, 0x80, 0, 0, 0, 0, 0, 0,
		hw[0] ^ 0x02, hw[1], hw[2], 0xff, 0xfe, hw[3], hw[4], hw[5],
	}.String()
}

func hasRouteInDefaultVRF(node corev1.Node, cidr string, nextHops ...string) (bool, error) {
	routeCommand := []string{"ip", "--json", "route", "show", cidr}
	if utilnet.IsIPv6CIDRString(cidr) {
		routeCommand = []string{"ip", "-6", "--json", "route", "show", cidr}
	}

	out, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), routeCommand)
	if err != nil {
		return false, fmt.Errorf("failed to get routes on node %s: %w", node.Name, err)
	}
	routes := []kernelRoute{}
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		return false, fmt.Errorf("failed to parse routes on node %s: %w; output: %s", node.Name, err, out)
	}
	framework.Logf("Routes in default VRF on node %s for %s: %s", node.Name, cidr, out)
	return hasBGPRoute(routes, cidr, nextHops...), nil
}

func hasRouteInDPUCUDNVRF(hostNode corev1.Node, vrfName, cidr, nextHop string) (bool, error) {
	dpuNodeName, err := dpuNodeNameForHostNode(hostNode.Name)
	if err != nil {
		return false, err
	}

	routeCommand := []string{"ip", "--json", "route", "show", "vrf", vrfName, cidr}
	if utilnet.IsIPv6CIDRString(cidr) {
		routeCommand = []string{"ip", "-6", "--json", "route", "show", "vrf", vrfName, cidr}
	}

	out, err := ForContainer(dpuNodeName).Exec(routeCommand[0], routeCommand[1:]...)
	if err != nil {
		return false, fmt.Errorf(
			"failed to get routes from CUDN VRF %s on DPU node %s for host node %s: %w, output: %s",
			vrfName,
			dpuNodeName,
			hostNode.Name,
			err,
			out,
		)
	}
	routes := []kernelRoute{}
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		return false, fmt.Errorf(
			"failed to parse routes from CUDN VRF %s on DPU node %s for host node %s: %w; output: %s",
			vrfName,
			dpuNodeName,
			hostNode.Name,
			err,
			out,
		)
	}
	framework.Logf(
		"Routes in CUDN VRF %s on DPU node %s for host node %s and %s: %s",
		vrfName,
		dpuNodeName,
		hostNode.Name,
		cidr,
		out,
	)
	return hasBGPRoute(routes, cidr, nextHop), nil
}

// dpuCUDNGatewayRouterHasDefaultRoute reports whether the OVN gateway router
// of the CUDN for the host node has a default static route via nextHop. The
// DPU's ovnkube programs that router in the DPU cluster, which the test has
// no API access to: the query runs through the DPU kind node container, with
// crictl, in the nb-ovsdb container of the DPU ovnkube pod.
func dpuCUDNGatewayRouterHasDefaultRoute(hostNodeName, cudnName, cidr, nextHop string) (bool, error) {
	dpuNodeName, err := dpuNodeNameForHostNode(hostNodeName)
	if err != nil {
		return false, err
	}
	out, err := ForContainer(dpuNodeName).Exec("crictl", "ps", "-q", "--name", "nb-ovsdb")
	if err != nil {
		return false, fmt.Errorf("failed to find the nb-ovsdb container on DPU node %s: %w, output: %s",
			dpuNodeName, err, out)
	}
	containerIDs := strings.Fields(out)
	if len(containerIDs) == 0 {
		return false, fmt.Errorf("no nb-ovsdb container running on DPU node %s", dpuNodeName)
	}
	out, err = ForContainer(dpuNodeName).Exec("crictl", "exec", containerIDs[0],
		"ovn-nbctl", "--bare", "--columns=nexthop", "find", "logical_router_static_route",
		"ip_prefix="+cidr,
		"external_ids:"+ovntypes.NetworkExternalID+"="+ovntypes.CUDNPrefix+cudnName)
	if err != nil {
		return false, fmt.Errorf("failed to list default routes of CUDN %s on DPU node %s: %w, output: %s",
			cudnName, dpuNodeName, err, out)
	}
	framework.Logf("Default routes %s of CUDN %s on DPU node %s for host node %s: %s",
		cidr, cudnName, dpuNodeName, hostNodeName, strings.TrimSpace(out))
	for _, programmed := range strings.Fields(out) {
		if programmed == nextHop {
			return true, nil
		}
	}
	return false, nil
}

func dpuNodeNameForHostNode(hostNodeName string) (string, error) {
	if !strings.Contains(hostNodeName, "-host-") {
		return "", fmt.Errorf("failed to derive DPU node name from host node %q", hostNodeName)
	}
	return strings.Replace(hostNodeName, "-host-", "-dpu-", 1), nil
}

// createUplinkPrimaryUDN creates a namespaced primary UserDefinedNetwork with
// the DPU resourceName annotation, so that pods in the namespace attach to it
// through a simulated VF. The namespace teardown deletes it.
func createUplinkPrimaryUDN(
	ctx context.Context,
	f *framework.Framework,
	namespace string,
	name string,
	networkSpec *udnv1.NetworkSpec,
) error {
	networkSpecMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(networkSpec)
	if err != nil {
		return fmt.Errorf("failed to convert network spec to unstructured: %w", err)
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.ovn.org/v1",
		"kind":       "UserDefinedNetwork",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
			"annotations": map[string]interface{}{
				uplinkDPUResourceNameAnnotation: dpuUplinkResourceName(),
			},
		},
		"spec": networkSpecMap,
	}}
	client := f.DynamicClient.Resource(udnGVR).Namespace(namespace)
	if _, err := client.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create UserDefinedNetwork %s/%s: %w", namespace, name, err)
	}
	gomega.Eventually(ctx, networkReadyFunc(ctx, client, name)).
		WithTimeout(uplinkTimeout).
		WithPolling(uplinkPoll).
		Should(gomega.Succeed(), "expected UserDefinedNetwork %s/%s to become ready", namespace, name)
	return nil
}

// podTotalRestarts sums the container restart counts of a pod.
func podTotalRestarts(pod *corev1.Pod) int {
	restarts := 0
	for _, containerStatus := range pod.Status.ContainerStatuses {
		restarts += int(containerStatus.RestartCount)
	}
	return restarts
}

func isDPUUplinkE2E() bool {
	return os.Getenv(uplinkDPUGatewayNetworkEnv) != ""
}

func dpuUplinkResourceName() string {
	return envOrDefault(uplinkDPUResourceNameEnv, uplinkDefaultDPUResourceName)
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func uplinkBridgeName(name string) string {
	name = strings.ReplaceAll(name, "-", "")
	if len(name) > 13 {
		name = name[len(name)-13:]
	}
	return "u" + name
}

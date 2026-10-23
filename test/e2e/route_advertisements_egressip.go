// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

// BGP EgressIP RouteAdvertisements e2e exercises five ordered scenarios on Kind/OpenShift BGP
// clusters (ENABLE_ROUTE_ADVERTISEMENTS=true), test the following
// test scenario 1 & 2: BGP egressIP on default network with and without RouteAdvertisements.spec.nodeSelector (egress-nodes=bgp) for EgressIP
// test scenario 3 & 4: BGP egressIP on L3 primary CUDN with and without RouteAdvertisements.spec.nodeSelector (egress-nodes=bgp) for EgressIP
// test scenario 5: BGP egressIP on L2 CUDN is correctly rejected.
// For scenario 1-2 & 3-4, verify egressIP advertised/withdrawl on the external router during egressIP assignment and failover, and
// verify pod egress SNAT to egressIP address.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
)

const (
	// egressBGPExportLabelKey/Value label nodes that match RouteAdvertisements.spec.nodeSelector for EgressIP.
	egressBGPExportLabelKey   = "egress-nodes"
	egressBGPExportLabelValue = "bgp"

	bgpEgressIPClientPodLabelKey   = "bgp-eip-client"
	bgpEgressIPClientPodLabelValue = "enabled"

	bgpEgressIPLocalPodName  = "bgp-eip-client-local"
	bgpEgressIPRemotePodName = "bgp-eip-client-remote"

	bgpEgressIPPollTimeout         = 3 * retryTimeout
	bgpEgressIPGRPCHealthCheckPort = "9107"

	// Matches FRRConfiguration created by contrib/kind-common.sh and the default-network RA from helm when advertiseDefaultNetwork is enabled.
	bgpE2EFRRConfigurationLabelKey   = "name"
	bgpE2EFRRConfigurationLabelValue = "receive-all"

	// bgpEgressIPMinWorkerNodes* is the minimum number of schedulable worker nodes required.
	bgpEgressIPMinWorkerNodesSingleStack = 2
	bgpEgressIPMinWorkerNodesDualStack   = 3

	egressAssignableLabel = "k8s.ovn.org/egress-assignable"

	// bgpEgressIPHostSubnet* bounds the host-route offset range used when allocating test EgressIP addresses.
	bgpEgressIPHostSubnetIPOffset = 200
	bgpEgressIPHostSubnetIPEnd    = 254
)

// FRR-K8s selector for receive-all applied by BGP Kind/Helm (contrib/kind-common.sh, helm default RouteAdvertisements).
var bgpE2EReceiveAllFRRConfigurationSelector = metav1.LabelSelector{
	MatchLabels: map[string]string{
		bgpE2EFRRConfigurationLabelKey: bgpE2EFRRConfigurationLabelValue,
	},
}

// bgpEgressIPNodeSet names schedulable workers used for egressIP assignment and failover (three for dual-stack) or two for single-stack):
//
//	egressNode1/egressNode2 host initial egressIP placement; nonInitialEgressNode is the intended node for egressIP failover to.
//	egressNodes is one or two names (single- or dual-stack); allScenarioNodeNames is nonInitialEgressNode plus egressNodes.
type bgpEgressIPNodeSet struct {
	nonInitialEgressNode corev1.Node
	egressNode1          corev1.Node
	egressNode2          corev1.Node // unset (zero value) on single-stack clusters
	dualStackCluster     bool
	egressNodes          []string
	allScenarioNodeNames []string
}

// bgpEgressIPScenario test initial BGP egressIP behavior, then for BGP egressIP supported cases,triggers failover from one egress node to another.
// (could be from BGP egressIP nodeSelector selected egress node to non-nodeSelector-selected egress node, or vice versa).
// Five test scenarios in five ginkgo.It() cases below:
// 1. BGP egressIP on default network with RouteAdvertisements nodeSelector {} for egressIP
// 2. BGP egressIP on default network with RouteAdvertisements nodeSelector {egress-nodes=bgp} for egressIP
// 3. BGP egressIP on L3 primary CUDN with RouteAdvertisements nodeSelector {} for egressIP
// 4. BGP egressIP on L3 primary CUDN with RouteAdvertisements nodeSelector {egress-nodes=bgp} for egressIP
// 5. Reject EgressIP RouteAdvertisements on Layer2 primary CUDN
type bgpEgressIPScenario struct {
	// useRouteAdvertisementsEgressIPNodeSelector: when true, RA matches egress-nodes=bgp and only those
	// nodes export assigned addresses to FRR; when false, any assignable node holding a spec IP may export.
	useRouteAdvertisementsEgressIPNodeSelector bool

	// failoverFromEgressIPAdvertisedNode applies when useRouteAdvertisementsEgressIPNodeSelector is true:
	// true = fail over from a BGP-export-labeled node (case 2); false = from a node without the label (case 4).
	failoverFromEgressIPAdvertisedNode bool
}

var _ = ginkgo.Describe("BGP: EgressIP advertisement", feature.RouteAdvertisements, feature.EgressIP, ginkgo.Ordered, func() {
	f := wrappedTestFramework("bgp-egressip-advertisement")

	var (
		nodeSet            bgpEgressIPNodeSet
		serverContainerIPs []string
		raClient           raclientset.Interface
	)

	ginkgo.BeforeEach(func() {
		if os.Getenv("ENABLE_ROUTE_ADVERTISEMENTS") != "true" {
			e2eskipper.Skipf("route advertisements are not enabled")
		}
		var err error
		raClient, err = raclientset.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err)

		serverContainerIPs = getBGPServerContainerIPs(f)
		gomega.Expect(len(serverContainerIPs)).To(gomega.BeNumerically(">", 0))

		nodeSet, err = pickBGPEgressIPNodeSet(f.ClientSet)
		framework.ExpectNoError(err)
		if nodeSet.dualStackCluster {
			ginkgo.By("For dualstack, selecting 3 schedulable workers for BGP egress IP test (nonInitialEgressNode + two egress nodes)")
		} else {
			ginkgo.By("For singlestack, selecting 2 schedulable workers for BGP egress IP test (nonInitialEgressNode + one gress node)")
		}
		ensureBGPEgressIPHealthCheckReachable(nodeSet.allScenarioNodeNames...)
		waitNodesHostCIDRsForEgressIP(f.ClientSet, nodeSet.allScenarioNodeNames...)
	})

	// Case 1: BGP egressIP on default network when RA egressIP nodeSelector is {}
	ginkgo.It("on default network without RouteAdvertisements nodeSelector for egressIP", func() {
		runBGPEgressIPScenario(f, raClient, nodeSet, serverContainerIPs, nil, bgpEgressIPScenario{
			useRouteAdvertisementsEgressIPNodeSelector: false,
		})
	})

	// Case 2: BGP egressIP on default network when RA egressIP nodeSelector is egress-nodes=bgp
	ginkgo.It("on default network with RouteAdvertisements nodeSelector for egressIP", func() {
		runBGPEgressIPScenario(f, raClient, nodeSet, serverContainerIPs, nil, bgpEgressIPScenario{
			useRouteAdvertisementsEgressIPNodeSelector: true,
			failoverFromEgressIPAdvertisedNode:         true,
		})
	})

	// Case 3: BGP egressIP on L3 CUDN when RA egressIP nodeSelector is {}
	ginkgo.It("on L3 primary CUDN without RouteAdvertisements nodeSelector for egressIP", func() {
		runBGPEgressIPScenario(f, raClient, nodeSet, serverContainerIPs, createBGPEgressIPL3CUDN, bgpEgressIPScenario{
			useRouteAdvertisementsEgressIPNodeSelector: false,
		})
	})

	// Case 4: BGP egressIP on L3 CUDN when RA egressIP nodeSelector is egress-nodes=bgp
	ginkgo.It("on L3 primary CUDN with RouteAdvertisements nodeSelector for egressIP", func() {
		runBGPEgressIPScenario(f, raClient, nodeSet, serverContainerIPs, createBGPEgressIPL3CUDN, bgpEgressIPScenario{
			useRouteAdvertisementsEgressIPNodeSelector: true,
			failoverFromEgressIPAdvertisedNode:         false,
		})
	})

	// Case 5: BGP egressIP on L2 CUDN is not supported; advertising EgressIP on Layer2 primary CUDN should be rejected
	ginkgo.It("rejects EgressIP RouteAdvertisements on Layer2 primary CUDN", func() {
		suffix := framework.RandomSuffix()
		cudnLabel := "bgp-eip-l2-" + suffix
		raName := "bgp-eip-l2-ra-" + suffix

		udnNamespace, err := f.CreateNamespace(context.TODO(), f.BaseName+"-l2-"+suffix, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		framework.ExpectNoError(err)
		ginkgo.DeferCleanup(func() {
			_ = f.ClientSet.CoreV1().Namespaces().Delete(context.TODO(), udnNamespace.Name, metav1.DeleteOptions{})
		})

		udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err)

		cudn := &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "bgp-eip-l2-",
				Labels:       map[string]string{cudnLabel: ""},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "kubernetes.io/metadata.name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{udnNamespace.Name},
				}}},
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role:    "Primary",
						Subnets: filterDualStackCIDRs(f.ClientSet, udnv1.DualStackCIDRs{"103.0.0.0/16", "2014:100::0/60"}),
					},
				},
			},
		}
		createdCUDN, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudn, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		ginkgo.DeferCleanup(func() {
			_ = f.ClientSet.CoreV1().Pods(udnNamespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
			_ = udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), createdCUDN.Name, metav1.DeleteOptions{})
		})
		gomega.Eventually(clusterUserDefinedNetworkReadyFunc(context.Background(), f.DynamicClient, createdCUDN.Name), 30*time.Second, time.Second).Should(gomega.Succeed())

		ra := &rav1.RouteAdvertisements{
			ObjectMeta: metav1.ObjectMeta{Name: raName},
			Spec: rav1.RouteAdvertisementsSpec{
				NetworkSelectors: apitypes.NetworkSelectors{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{cudnLabel: ""},
							},
						},
					},
				},
				NodeSelector:             metav1.LabelSelector{},
				FRRConfigurationSelector: bgpE2EReceiveAllFRRConfigurationSelector,
				Advertisements:           []rav1.AdvertisementType{rav1.EgressIP},
			},
		}
		_, err = raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		ginkgo.DeferCleanup(func() {
			_ = raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), raName, metav1.DeleteOptions{})
		})

		waitRouteAdvertisementCondition(raClient, raName, "", "ConfigurationError", "Layer2")
	})
})

// pickBGPEgressIPNodeSet picks the first N workers for the BGP egress IP test (two for single-stack or three for dual-stack)
func pickBGPEgressIPNodeSet(cs clientset.Interface) (bgpEgressIPNodeSet, error) {
	nodeList, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
	if err != nil {
		return bgpEgressIPNodeSet{}, err
	}
	schedulable := len(nodeList.Items)
	e2enode.Filter(nodeList, func(node corev1.Node) bool {
		return !isControlPlaneNode(node)
	})
	workers := nodeList.Items
	dualStackCluster := isIPv4Supported(cs) && isIPv6Supported(cs)
	minWorkers := bgpEgressIPMinWorkerNodesSingleStack
	if dualStackCluster {
		minWorkers = bgpEgressIPMinWorkerNodesDualStack
	}
	if len(workers) < minWorkers {
		e2eskipper.Skipf("need at least %d worker nodes for BGP egress IP tests, got %d (schedulable nodes: %d)",
			minWorkers, len(workers), schedulable)
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].Name < workers[j].Name })
	set := bgpEgressIPNodeSet{
		nonInitialEgressNode: workers[0],
		egressNode1:          workers[1],
		dualStackCluster:     dualStackCluster,
		egressNodes:          []string{workers[1].Name},
	}
	if dualStackCluster {
		set.egressNode2 = workers[2]
		set.egressNodes = append(set.egressNodes, workers[2].Name)
	}
	set.allScenarioNodeNames = append([]string{set.nonInitialEgressNode.Name}, set.egressNodes...)
	return set, nil
}

// ensureBGPEgressIPHealthCheckReachable opens the EgressIP gRPC health-check port on each node
func ensureBGPEgressIPHealthCheckReachable(nodeNames ...string) {
	for _, nodeName := range nodeNames {
		allowOrDropNodeInputTrafficOnPort("Allow", nodeName, "tcp", bgpEgressIPGRPCHealthCheckPort)
	}
}

// waitNodesHostCIDRsForEgressIP waits until each node has a non-empty k8s.ovn.org/host-cidrs annotation.
func waitNodesHostCIDRsForEgressIP(cs clientset.Interface, nodeNames ...string) {
	for _, nodeName := range nodeNames {
		gomega.Eventually(func() error {
			node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			hostCIDRs, err := util.ParseNodeHostCIDRsDropNetMask(node)
			if err != nil {
				return fmt.Errorf("node %s host-cidrs not parseable: %w", nodeName, err)
			}
			if hostCIDRs.Len() == 0 {
				return fmt.Errorf("node %s has empty host-cidrs", nodeName)
			}
			return nil
		}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())
	}
}

// runBGPEgressIPScenario is the shared flow for test scenarios 1–2 (default) and 3–4 (L3 CUDN): create RA(s),
// create egressIP object, assert advertised egressIP route on external FRR router, verify EgressIP SNAT from client pods,
// then failover egressIP from the chosen source egress node to a new egress node and
// re-check FRR routes and egressIP SNAT from client pods.
func runBGPEgressIPScenario(
	f *framework.Framework,
	raClient raclientset.Interface,
	nodeSet bgpEgressIPNodeSet,
	serverContainerIPs []string,
	setupNetwork func(*framework.Framework) (cudnMatchLabel, hostSubnetNetworkName string, cleanup func()),
	scenario bgpEgressIPScenario,
) {
	suffix := framework.RandomSuffix()
	raName := "bgp-eip-ra-" + suffix
	eipName := "bgp-eip-" + suffix
	if nodeSet.dualStackCluster {
		framework.Logf("BGP egress IP node roles: nonInitialEgressNode=%s egressNode1=%s egressNode2=%s",
			nodeSet.nonInitialEgressNode.Name, nodeSet.egressNode1.Name, nodeSet.egressNode2.Name)
	} else {
		framework.Logf("BGP egress IP node roles: nonInitialEgressNode=%s egressNode1=%s (single-stack)",
			nodeSet.nonInitialEgressNode.Name, nodeSet.egressNode1.Name)
	}

	var cudnMatchLabel, hostSubnetNetworkName string
	if setupNetwork != nil {
		var cleanup func()
		cudnMatchLabel, hostSubnetNetworkName, cleanup = setupNetwork(f)
		ginkgo.DeferCleanup(cleanup)
		ginkgo.DeferCleanup(func() {
			_ = f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
		})
	} else {
		updateNamespaceLabels(f, f.Namespace, map[string]string{"name": f.Namespace.Name})
	}

	ginkgo.DeferCleanup(func() {
		e2enode.RemoveLabelOffNode(f.ClientSet, nodeSet.egressNode1.Name, egressAssignableLabel)
		if nodeSet.dualStackCluster {
			e2enode.RemoveLabelOffNode(f.ClientSet, nodeSet.egressNode2.Name, egressAssignableLabel)
		}
	})

	if scenario.useRouteAdvertisementsEgressIPNodeSelector && scenario.failoverFromEgressIPAdvertisedNode {
		// Case 2: egress-nodes=bgp on egressNode1 (single-stack) or on both egress nodes (dual-stack).
		e2enode.AddOrUpdateLabelOnNode(f.ClientSet, nodeSet.egressNode1.Name, egressBGPExportLabelKey, egressBGPExportLabelValue)
		if nodeSet.dualStackCluster {
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, nodeSet.egressNode2.Name, egressBGPExportLabelKey, egressBGPExportLabelValue)
		}
		ginkgo.DeferCleanup(func() {
			e2enode.RemoveLabelOffNode(f.ClientSet, nodeSet.egressNode1.Name, egressBGPExportLabelKey)
			if nodeSet.dualStackCluster {
				e2enode.RemoveLabelOffNode(f.ClientSet, nodeSet.egressNode2.Name, egressBGPExportLabelKey)
			}
		})
	}

	nodeSelector := metav1.LabelSelector{}
	if scenario.useRouteAdvertisementsEgressIPNodeSelector {
		nodeSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{egressBGPExportLabelKey: egressBGPExportLabelValue},
		}
	}

	onPrimaryCUDN := cudnMatchLabel != ""
	if onPrimaryCUDN {
		podNetRAName := "bgp-eip-udn-podnet-ra-" + suffix
		createBGPL3CUDNPodNetworkRouteAdvertisement(raClient, podNetRAName, cudnMatchLabel)
		ginkgo.DeferCleanup(func() {
			_ = raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), podNetRAName, metav1.DeleteOptions{})
		})
		waitRouteAdvertisementCondition(raClient, podNetRAName, metav1.ConditionTrue, "", "")
	}

	createBGPEgressIPRouteAdvertisement(raClient, raName, cudnMatchLabel, nodeSelector)
	ginkgo.DeferCleanup(func() {
		_ = raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), raName, metav1.DeleteOptions{})
	})
	// Case 4: egressIP failover from egressNode that has no egress-nodes=bgp label to egressNode that has egress-nodes=bgp label
	if scenario.useRouteAdvertisementsEgressIPNodeSelector && !scenario.failoverFromEgressIPAdvertisedNode {
		waitRouteAdvertisementCondition(raClient, raName, metav1.ConditionFalse, "ConfigurationPending", "no nodes selected")
	} else {
		waitRouteAdvertisementCondition(raClient, raName, metav1.ConditionTrue, "", "")
	}

	if onPrimaryCUDN {
		waitBGPEgressCUDNHostSubnetsOnNodes(f.ClientSet, hostSubnetNetworkName, nodeSet.egressNodes...)
	}
	ensureBGPEgressIPHealthCheckReachable(nodeSet.egressNodes...)
	waitNodesHostCIDRsForEgressIP(f.ClientSet, nodeSet.egressNodes...)

	// Pick EgressIP addresses first, then mark egress node(s) as egress-assignable, then create the EgressIP CR
	var egressIPs []string
	var dualPlan bgpDualStackEgressIPPlan
	egressIPs, dualPlan = allocateBGPEgressTestIPsFromMachineNetwork(f.ClientSet, nodeSet, scenario)
	framework.Logf("BGP egress IP test using %d egress address(es): %v", len(egressIPs), egressIPs)

	dualStackEIP := len(egressIPs) > 1
	gomega.Expect(dualStackEIP).To(gomega.Equal(nodeSet.dualStackCluster), "cluster dual-stack capability must match allocated EgressIP spec")
	assignableEgressNodes := nodeSet.egressNodes
	framework.Logf("BGP egress IP assignable egress nodes: %v", assignableEgressNodes)

	prepareBGPEgressNodesForEgressIP(f.ClientSet, assignableEgressNodes...)

	podLabels := map[string]string{bgpEgressIPClientPodLabelKey: bgpEgressIPClientPodLabelValue}
	nsLabels := map[string]string{"name": f.Namespace.Name}
	eipManifest := createEIPManifest(eipName, podLabels, nsLabels, egressIPs...)
	eipManifestPath := filepath.Join(os.TempDir(), "bgp-eip-"+framework.RandomSuffix()+".yaml")
	framework.ExpectNoError(os.WriteFile(eipManifestPath, []byte(eipManifest), 0o644))
	defer func() { _ = os.Remove(eipManifestPath) }()
	e2ekubectl.RunKubectlOrDie("default", "create", "-f", eipManifestPath)
	ginkgo.DeferCleanup(func() {
		_, _ = e2ekubectl.RunKubectl("default", "delete", "eip", eipName, "--ignore-not-found")
	})
	assignments := waitForEgressIPAssignments(eipName, egressIPs)

	for egressIP, nodeName := range assignments {
		node := getClusterNode(f.ClientSet, nodeName)
		expectExported := !scenario.useRouteAdvertisementsEgressIPNodeSelector || node.Labels[egressBGPExportLabelKey] == egressBGPExportLabelValue
		expectEgressIPRouteInFRR(egressIP, expectExported, node)
	}

	// Create client pods local or remote to egress node(s) and verify EgressIP SNAT.
	localPod, err := createGenericPodWithLabel(f, bgpEgressIPLocalPodName, nodeSet.egressNode1.Name, f.Namespace.Name,
		getAgnHostHTTPPortBindFullCMD(netexecPort), podLabels)
	framework.ExpectNoError(err)
	remotePod, err := createGenericPodWithLabel(f, bgpEgressIPRemotePodName, nodeSet.nonInitialEgressNode.Name, f.Namespace.Name,
		getAgnHostHTTPPortBindFullCMD(netexecPort), podLabels)
	framework.ExpectNoError(err)
	framework.ExpectNoError(e2epod.WaitTimeoutForPodReadyInNamespace(context.TODO(), f.ClientSet, localPod.Name, f.Namespace.Name, bgpEgressIPPollTimeout))
	framework.ExpectNoError(e2epod.WaitTimeoutForPodReadyInNamespace(context.TODO(), f.ClientSet, remotePod.Name, f.Namespace.Name, bgpEgressIPPollTimeout))

	snatCheckPort := strconv.Itoa(netexecPort)
	serverIPByEgressIP := make(map[string]string, len(assignments))
	for egressIP := range assignments {
		serverIP := getFirstIPStringOfFamily(utilnet.IPFamilyOfString(egressIP), serverContainerIPs)
		if serverIP == "" {
			framework.Failf("no bgpserver IP for family of egress IP %s in %+v", egressIP, serverContainerIPs)
		}
		serverIPByEgressIP[egressIP] = serverIP
		gomega.Eventually(func() error {
			return curlAgnHostClientIPFromPod(f.Namespace.Name, localPod.Name, egressIP, serverIP, snatCheckPort)
		}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed())
		gomega.Eventually(func() error {
			return curlAgnHostClientIPFromPod(f.Namespace.Name, remotePod.Name, egressIP, serverIP, snatCheckPort)
		}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed())
	}

	// Failover egressIP from the chosen source egress node to a new egress node and verify FRR routes and EgressIP SNAT from client pods.
	var failoverIP, failoverFromEgressNode, failoverToEgressNode string
	failoverToEgressNode = nodeSet.nonInitialEgressNode.Name
	labelFailoverToNodeWithEgressBGPNodeselector := false
	if dualStackEIP {
		labelFailoverToNodeWithEgressBGPNodeselector = dualPlan.labelFailoverToNodeWithEgressBGPNodeselector
	} else {
		labelFailoverToNodeWithEgressBGPNodeselector = scenario.useRouteAdvertisementsEgressIPNodeSelector && !scenario.failoverFromEgressIPAdvertisedNode
	}
	prepareBGPEgressIPFailoverToNode(f, failoverToEgressNode, labelFailoverToNodeWithEgressBGPNodeselector)
	if scenario.useRouteAdvertisementsEgressIPNodeSelector && !scenario.failoverFromEgressIPAdvertisedNode {
		waitRouteAdvertisementCondition(raClient, raName, metav1.ConditionTrue, "", "")
	}
	failoverIP, failoverFromEgressNode = pickFailoverFromFirstEgressNode(f.ClientSet, assignments, nodeSet, failoverToEgressNode)
	ginkgo.By(fmt.Sprintf("Failover: moving %s from %s to %s", failoverIP, failoverFromEgressNode, failoverToEgressNode))
	otherAssignments := make(map[string]string, len(assignments))
	for k, v := range assignments {
		otherAssignments[k] = v
	}
	ginkgo.By(fmt.Sprintf("Failing over egress IP %s from node %s to node %s by removing egress-assignable", failoverIP, failoverFromEgressNode, failoverToEgressNode))
	egressAvailability := &egressNodeAvailabilityHandlerViaLabel{F: f}
	egressAvailability.Disable(failoverFromEgressNode)
	ginkgo.DeferCleanup(func() { egressAvailability.Enable(failoverFromEgressNode) })

	newAssignments := waitForEgressIPFailover(eipName, failoverIP, failoverFromEgressNode, failoverToEgressNode, otherAssignments, len(egressIPs))
	newNode := newAssignments[failoverIP]
	newNodeObj := getClusterNode(f.ClientSet, newNode)

	failoverExported := !scenario.useRouteAdvertisementsEgressIPNodeSelector ||
		newNodeObj.Labels[egressBGPExportLabelKey] == egressBGPExportLabelValue
	if scenario.useRouteAdvertisementsEgressIPNodeSelector && !failoverExported {
		ginkgo.By(fmt.Sprintf("Expect BGP route for egressIP %s to be withdrawn after failover from source egressnode %s to node %s (leaving RA nodeSelector-matching node)", failoverIP, failoverFromEgressNode, failoverToEgressNode))
	}
	expectEgressIPRouteInFRR(failoverIP, failoverExported, newNodeObj)

	failoverServerIP := serverIPByEgressIP[failoverIP]
	gomega.Eventually(func() error {
		return curlAgnHostClientIPFromPod(f.Namespace.Name, localPod.Name, failoverIP, failoverServerIP, snatCheckPort)
	}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed())
	gomega.Eventually(func() error {
		return curlAgnHostClientIPFromPod(f.Namespace.Name, remotePod.Name, failoverIP, failoverServerIP, snatCheckPort)
	}, bgpEgressIPPollTimeout, retryInterval).Should(gomega.Succeed())
}

// createBGPEgressIPL3CUDN sets up L3 primary CUDN e2e fixtures: a UDN namespace, then a
// ClusterUserDefinedNetwork (Layer3 primary) selected by that namespace. Returns the CUDN label
// key for RouteAdvertisements networkSelectors, the OVN network name that is passed to
// waitBGPEgressCUDNHostSubnetsOnNodes (poll until egress nodes have host subnets on that UDN),
// and a cleanup func that deletes the CUDN (namespace cleanup is handled by the test framework).
func createBGPEgressIPL3CUDN(f *framework.Framework) (string, string, func()) {
	suffix := framework.RandomSuffix()
	cudnLabel := "bgp-eip-l3-" + suffix

	namespace, err := f.CreateNamespace(context.TODO(), f.BaseName+"-"+suffix, map[string]string{
		"e2e-framework":           f.BaseName,
		RequiredUDNNamespaceLabel: "",
	})
	framework.ExpectNoError(err)
	f.Namespace = namespace
	updateNamespaceLabels(f, namespace, map[string]string{"name": namespace.Name})

	udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
	framework.ExpectNoError(err)

	cudn := &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "bgp-eip-l3-",
			Labels:       map[string]string{cudnLabel: ""},
		},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{f.Namespace.Name},
			}}},
			Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role:    "Primary",
					Subnets: filterL3Subnets(f.ClientSet, cudnLayer3SubnetsB()),
				},
			},
		},
	}
	createdCUDN, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudn, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	gomega.Eventually(clusterUserDefinedNetworkReadyFunc(context.Background(), f.DynamicClient, createdCUDN.Name), 60*time.Second, time.Second).Should(gomega.Succeed())

	cleanup := func() {
		_ = udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), createdCUDN.Name, metav1.DeleteOptions{})
	}
	return cudnLabel, util.GenerateCUDNNetworkName(createdCUDN.Name), cleanup
}

// createBGPL3CUDNPodNetworkRouteAdvertisement advertises pod CIDRs for the L3 CUDN (north-south for client pods).
func createBGPL3CUDNPodNetworkRouteAdvertisement(raClient raclientset.Interface, name, cudnMatchLabel string) {
	ra := &rav1.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: rav1.RouteAdvertisementsSpec{
			NetworkSelectors: apitypes.NetworkSelectors{
				{
					NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{cudnMatchLabel: ""},
						},
					},
				},
			},
			NodeSelector:             metav1.LabelSelector{},
			FRRConfigurationSelector: bgpE2EReceiveAllFRRConfigurationSelector,
			Advertisements:           []rav1.AdvertisementType{rav1.PodNetwork},
		},
	}
	_, err := raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
	framework.ExpectNoError(err)
}

// createBGPEgressIPRouteAdvertisement creates an RA with Advertisements=[EgressIP] on default or CUDN network.
func createBGPEgressIPRouteAdvertisement(raClient raclientset.Interface, name, cudnMatchLabel string, nodeSelector metav1.LabelSelector) {
	networkSelectors := apitypes.NetworkSelectors{
		{NetworkSelectionType: apitypes.DefaultNetwork},
	}
	if cudnMatchLabel != "" {
		networkSelectors = apitypes.NetworkSelectors{
			{
				NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
				ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
					NetworkSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{cudnMatchLabel: ""},
					},
				},
			},
		}
	}
	ra := &rav1.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: rav1.RouteAdvertisementsSpec{
			NetworkSelectors:         networkSelectors,
			NodeSelector:             nodeSelector,
			FRRConfigurationSelector: bgpE2EReceiveAllFRRConfigurationSelector,
			Advertisements:           []rav1.AdvertisementType{rav1.EgressIP},
		},
	}
	_, err := raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
	framework.ExpectNoError(err)
}

// waitRouteAdvertisementCondition verify expected RouteAdvertisements condition or error out
func waitRouteAdvertisementCondition(raClient raclientset.Interface, name string, wantStatus metav1.ConditionStatus, wantReason, messageContains string) {
	gomega.Eventually(func() error {
		ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		c := meta.FindStatusCondition(ra.Status.Conditions, rav1.RouteAdvertisementsAccepted)
		if c == nil {
			return fmt.Errorf("no Accepted condition on RouteAdvertisements %s", name)
		}
		if wantStatus != "" && c.Status != wantStatus {
			return fmt.Errorf("RouteAdvertisements %s: want status %s, got %s (reason=%s message=%s)",
				name, wantStatus, c.Status, c.Reason, c.Message)
		}
		if wantReason != "" && c.Reason != wantReason {
			return fmt.Errorf("RouteAdvertisements %s: want reason %s, got %s (status=%s message=%s)",
				name, wantReason, c.Reason, c.Status, c.Message)
		}
		if messageContains != "" && !strings.Contains(c.Message, messageContains) {
			return fmt.Errorf("RouteAdvertisements %s: message %q does not contain %q", name, c.Message, messageContains)
		}
		return nil
	}, 2*time.Minute, time.Second).Should(gomega.Succeed())
}

// waitBGPEgressCUDNHostSubnetsOnNodes waits for L3 CUDN host subnets before EgressIP assignment on that network.
func waitBGPEgressCUDNHostSubnetsOnNodes(cs clientset.Interface, networkName string, nodeNames ...string) {
	for _, nodeName := range nodeNames {
		gomega.Eventually(func() error {
			node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			ipNets, err := util.ParseNodeHostSubnetAnnotation(node, networkName)
			if err != nil {
				return err
			}
			if len(ipNets) == 0 {
				return fmt.Errorf("node %s has no host subnets for network %s", nodeName, networkName)
			}
			return nil
		}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())
	}
}

// egressIPListWithSpec is a minimal JSON shape for kubectl get eip -A -o json (occupied addresses).
type egressIPListWithSpec struct {
	Items []struct {
		Spec struct {
			EgressIPs []string `json:"egressIPs"`
		} `json:"spec"`
	} `json:"items"`
}

// clusterEgressIPSpecAddresses returns all spec.egressIPs already claimed in the cluster.
func clusterEgressIPSpecAddresses() (sets.Set[string], error) {
	stdout, err := e2ekubectl.RunKubectl("", "get", "eip", "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	list := egressIPListWithSpec{}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		return nil, err
	}
	occupied := sets.New[string]()
	for _, item := range list.Items {
		for _, ip := range item.Spec.EgressIPs {
			occupied.Insert(ip)
		}
	}
	return occupied, nil
}

// bgpDualStackEgressIPPlan holds dual-stack-only knobs for IP allocation and failover prep
type bgpDualStackEgressIPPlan struct {
	// labelFailoverToNodeWithEgressBGPNodeselector: when true, nonInitialEgressNode gets egress-nodes=bgp during prepareBGPEgressIPFailoverToNode (case 4).
	labelFailoverToNodeWithEgressBGPNodeselector bool
	failoverTo                                   string
}

// bgpDualStackEgressIPPlanForScenario maps dual-stack cases to failover-target labeling and allocation.
func bgpDualStackEgressIPPlanForScenario(scenario bgpEgressIPScenario, nodeSet bgpEgressIPNodeSet) bgpDualStackEgressIPPlan {
	plan := bgpDualStackEgressIPPlan{failoverTo: nodeSet.nonInitialEgressNode.Name}
	// Case 4: after failover, RA EgressIP nodeSelector should match the failover target.
	if scenario.useRouteAdvertisementsEgressIPNodeSelector && !scenario.failoverFromEgressIPAdvertisedNode {
		plan.labelFailoverToNodeWithEgressBGPNodeselector = true
	}
	return plan
}

// allocateBGPEgressTestIPsFromMachineNetwork matches egressip.go machine-network allocators.
func allocateBGPEgressTestIPsFromMachineNetwork(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, scenario bgpEgressIPScenario) ([]string, bgpDualStackEgressIPPlan) {
	var egressIPs []string
	plan := bgpDualStackEgressIPPlan{}
	dualStack := nodeSet.dualStackCluster
	gomega.Eventually(func() error {
		occupied, err := clusterEgressIPSpecAddresses()
		if err != nil {
			return err
		}
		if dualStack {
			plan = bgpDualStackEgressIPPlanForScenario(scenario, nodeSet)
		}
		return allocateBGPEgressTestIPs(cs, nodeSet, plan, dualStack, occupied, &egressIPs)
	}, 2*time.Minute, time.Second).Should(gomega.Succeed())
	return egressIPs, plan
}

// allocateBGPEgressTestIPs picks one machine-network egress IP (single-stack) or IPv4+IPv6 (dual-stack).
func allocateBGPEgressTestIPs(cs clientset.Interface, nodeSet bgpEgressIPNodeSet, plan bgpDualStackEgressIPPlan, dualStack bool, occupied sets.Set[string], egressIPs *[]string) error {
	maxAttempts := int(bgpEgressIPHostSubnetIPEnd - bgpEgressIPHostSubnetIPOffset + 1)
	if dualStack {
		// Spec has two addresses; call both allocators
		v4Alloc, v6Alloc := nodeSet.egressNode1.Name, nodeSet.egressNode2.Name
		for attempt := 0; attempt < maxAttempts; attempt++ {
			v4IP, err := allocators.NewPrimaryIPv4(v4Alloc)
			if err != nil {
				break
			}
			v6IP, err := allocators.NewPrimaryIPv6(v6Alloc)
			if err != nil {
				break
			}
			v4Str, v6Str := v4IP.String(), v6IP.String()
			if occupied.Has(v4Str) || occupied.Has(v6Str) {
				continue
			}
			if !bgpEgressIPAssignableOnNodeByName(cs, nodeSet.egressNode1.Name, v4Str) || !bgpEgressIPAssignableOnNodeByName(cs, nodeSet.egressNode2.Name, v4Str) ||
				!bgpEgressIPAssignableOnNodeByName(cs, nodeSet.egressNode1.Name, v6Str) || !bgpEgressIPAssignableOnNodeByName(cs, nodeSet.egressNode2.Name, v6Str) ||
				!bgpEgressIPAssignableOnNodeByName(cs, plan.failoverTo, v4Str) || !bgpEgressIPAssignableOnNodeByName(cs, plan.failoverTo, v6Str) {
				continue
			}
			*egressIPs = []string{v4Str, v6Str}
			framework.Logf("allocated BGP dual-stack egress test IPs %s and %s (assignable on egress nodes %s/%s, failover to %s)",
				v4Str, v6Str, nodeSet.egressNode1.Name, nodeSet.egressNode2.Name, plan.failoverTo)
			return nil
		}
		return fmt.Errorf("no dual-stack machine-network egress IPs assignable on egress nodes %s/%s (occupied: %v)",
			nodeSet.egressNode1.Name, nodeSet.egressNode2.Name, sets.List(occupied))
	}

	node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeSet.egressNode1.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	// IPv6-only cluster: isIPv6Supported && !isIPv4Supported. Otherwise use IPv4 (IPv4-only or default).
	useIPv6Allocator := isIPv6Supported(cs) && !isIPv4Supported(cs)
	for range maxAttempts {
		var ip net.IP
		if useIPv6Allocator {
			ip, err = allocators.NewPrimaryIPv6(node.Name)
		} else {
			ip, err = allocators.NewPrimaryIPv4(node.Name)
		}
		if err != nil {
			return err
		}
		candidate := ip.String()
		if occupied.Has(candidate) {
			continue
		}
		if !egressIPAssignableOnNodeForNetwork(node, candidate, types.DefaultNetworkName) {
			continue
		}
		*egressIPs = []string{candidate}
		allocatorFamily := "IPv4"
		if useIPv6Allocator {
			allocatorFamily = "IPv6"
		}
		framework.Logf("allocated BGP egress test IP %s via machine-network %s allocator on node %s", candidate, allocatorFamily, node.Name)
		return nil
	}
	return fmt.Errorf("no assignable machine-network egress IP on node %s (occupied: %v)", nodeSet.egressNode1.Name, sets.List(occupied))
}

// bgpEgressIPAssignableOnNodeByName wraps egressIPAssignableOnNode with a live Node GET.
func bgpEgressIPAssignableOnNodeByName(cs clientset.Interface, nodeName, ipStr string) bool {
	node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return egressIPAssignableOnNodeForNetwork(node, ipStr, types.DefaultNetworkName)
}

// egressIPAssignableOnNodeForNetwork mirrors cluster-manager assignability for a given network name.
func egressIPAssignableOnNodeForNetwork(node *corev1.Node, ipStr, _ string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	eIPConfig, err := util.GetNodeEIPConfig(node)
	if err != nil {
		return false
	}
	network, err := util.GetEgressIPNetwork(node, eIPConfig, ip)
	return err == nil && network != ""
}

// prepareBGPEgressNodesForEgressIP labels egress nodes immediately before creating an EgressIP.
func prepareBGPEgressNodesForEgressIP(cs clientset.Interface, nodeNames ...string) {
	ensureBGPEgressIPHealthCheckReachable(nodeNames...)
	for _, nodeName := range nodeNames {
		e2enode.AddOrUpdateLabelOnNode(cs, nodeName, egressAssignableLabel, "dummy")
		e2enode.ExpectNodeHasLabel(context.TODO(), cs, nodeName, egressAssignableLabel, "dummy")
	}
}

// getClusterNode re-reads the Node from the API so label checks match cluster state (not cached List objects).
func getClusterNode(cs clientset.Interface, nodeName string) corev1.Node {
	node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	framework.ExpectNoError(err)
	return *node
}

// prepareBGPEgressIPFailoverToNode prepares nonInitialEgressNode as the failover target (egress-assignable; optional egress-nodes=bgp).
// labelFailoverToNodeWithEgressBGPNodeselector false (case 2): egress-assignable only on failover target.
// true (case 4): also egress-nodes=bgp so RouteAdvertisements EgressIP nodeSelector matches after failover.
// Opens the EgressIP health-check port and waits for host-cidrs before a label transition so cluster-manager runs addEgressNode.
func prepareBGPEgressIPFailoverToNode(f *framework.Framework, failoverToNodeName string, labelFailoverToNodeWithEgressBGPNodeselector bool) {
	cs := f.ClientSet
	if labelFailoverToNodeWithEgressBGPNodeselector {
		ginkgo.By(fmt.Sprintf("Labeling failover target %s egress-assignable and egress-nodes=bgp", failoverToNodeName))
	} else {
		ginkgo.By(fmt.Sprintf("Labeling failover target %s egress-assignable only", failoverToNodeName))
	}
	ginkgo.DeferCleanup(func() { e2enode.RemoveLabelOffNode(cs, failoverToNodeName, egressAssignableLabel) })
	if labelFailoverToNodeWithEgressBGPNodeselector {
		ginkgo.DeferCleanup(func() { e2enode.RemoveLabelOffNode(cs, failoverToNodeName, egressBGPExportLabelKey) })
	}
	ensureBGPEgressIPHealthCheckReachable(failoverToNodeName)
	waitNodesHostCIDRsForEgressIP(cs, failoverToNodeName)
	e2enode.RemoveLabelOffNode(cs, failoverToNodeName, egressAssignableLabel)
	labelNodeForEgress(f, failoverToNodeName)
	if labelFailoverToNodeWithEgressBGPNodeselector {
		e2enode.AddOrUpdateLabelOnNode(cs, failoverToNodeName, egressBGPExportLabelKey, egressBGPExportLabelValue)
	} else {
		e2enode.RemoveLabelOffNode(cs, failoverToNodeName, egressBGPExportLabelKey)
	}
	e2enode.ExpectNodeHasLabel(context.TODO(), cs, failoverToNodeName, egressAssignableLabel, "dummy")
}

// getEgressIPStatusItems returns status.items from the EgressIP CR (kubectl json).
func getEgressIPStatusItems(eipName string) []egressIPStatus {
	egressIP := egressIP{}
	stdout, err := e2ekubectl.RunKubectl("default", "get", "eip", eipName, "-o", "json")
	if err != nil {
		return nil
	}
	if err := json.Unmarshal([]byte(stdout), &egressIP); err != nil {
		framework.Failf("failed to unmarshal egress IP %s: %v", eipName, err)
	}
	return egressIP.Status.Items
}

// egressIPAssignmentsByAddress maps spec egress IP string to assigned node name.
func egressIPAssignmentsByAddress(items []egressIPStatus) map[string]string {
	m := make(map[string]string, len(items))
	for _, item := range items {
		m[item.EgressIP] = item.Node
	}
	return m
}

// waitForEgressIPAssignments polls until every spec egress IP has a node in EgressIP status.
func waitForEgressIPAssignments(eipName string, expectedIPs []string) map[string]string {
	var assignments map[string]string
	err := wait.PollImmediate(retryInterval, bgpEgressIPPollTimeout, func() (bool, error) {
		items := getEgressIPStatusItems(eipName)
		assignments = egressIPAssignmentsByAddress(items)
		if len(items) != len(expectedIPs) {
			return false, nil
		}
		for _, ip := range expectedIPs {
			if assignments[ip] == "" {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		framework.Failf("timed out waiting for egress IP assignments on %s (want %v): last map %v",
			eipName, expectedIPs, assignments)
	}
	return assignments
}

// pickFailoverFromFirstEgressNode walks egress nodes in order (egressNode1, then egressNode2 when dual-stack),
// picks the first that holds a spec egress IP assignable on failoverToEgressNode, and returns that IP and source node.
func pickFailoverFromFirstEgressNode(cs clientset.Interface, assignments map[string]string, nodeSet bgpEgressIPNodeSet, failoverToEgressNode string) (string, string) {
	for _, onNode := range nodeSet.egressNodes {
		var ip string
		for candidate, assignedNode := range assignments {
			if assignedNode == onNode {
				ip = candidate
				break
			}
		}
		if ip == "" {
			continue
		}
		if !bgpEgressIPAssignableOnNodeByName(cs, failoverToEgressNode, ip) {
			continue
		}
		return ip, onNode
	}
	framework.Failf("no egress IP on egress nodes %v can fail over to %s", nodeSet.egressNodes, failoverToEgressNode)
	return "", ""
}

// waitForEgressIPFailover polls until failoverIP moves to toNode 
func waitForEgressIPFailover(eipName, failoverIP, fromNode, toNode string, unchanged map[string]string, expectedStatusCount int) map[string]string {
	var assignments map[string]string
	err := wait.PollImmediate(retryInterval, bgpEgressIPPollTimeout, func() (bool, error) {
		items := getEgressIPStatusItems(eipName)
		assignments = egressIPAssignmentsByAddress(items)
		if expectedStatusCount > 0 && len(items) != expectedStatusCount {
			return false, nil
		}
		if assignments[failoverIP] != toNode {
			return false, nil
		}
		for ip, node := range unchanged {
			if ip == failoverIP {
				continue
			}
			if assignments[ip] != node {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		framework.Failf("timed out waiting for egress IP %s to move from node %s to node %s (expected %d status items): %v",
			failoverIP, fromNode, toNode, expectedStatusCount, assignments)
	}
	return assignments
}

// hostRoutePrefix returns the BGP-advertised host route CIDR for an EgressIP address.
func hostRoutePrefix(egressIP string) string {
	if strings.Contains(egressIP, "/") {
		return egressIP
	}
	if utilnet.IsIPv6String(egressIP) {
		return egressIP + "/128"
	}
	return egressIP + "/32"
}

// egressIPFRRRouteCmd returns the external FRR container and the ip-route-show command
// for the given egress IP (handles both IPv4 /32 and IPv6 /128 host routes).
func egressIPFRRRouteCmd(egressIP string) (infraapi.ExternalContainer, []string) {
	ipVer := ""
	if utilnet.IsIPv6String(egressIP) {
		ipVer = " -6"
	}
	cmd := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, hostRoutePrefix(egressIP)), " ")
	return infraapi.ExternalContainer{Name: routerContainerName}, cmd
}

// bgpEgressIPFRRNexthop is the nexthop FRR should show for an egress IP on node (IPv6 link-local or IPv4 InternalIP).
func bgpEgressIPFRRNexthop(node corev1.Node, isIPv6 bool) string {
	if isIPv6 {
		lla, err := GetNodeIPv6LinkLocalAddressForEth0(node.Name)
		framework.ExpectNoError(err)
		return lla
	}
	ips := e2enode.GetAddressesByTypeAndFamily(&node, corev1.NodeInternalIP, corev1.IPv4Protocol)
	gomega.Expect(len(ips)).To(gomega.BeNumerically(">", 0), "must find a valid IPv4 nexthop for node %s", node.Name)
	return ips[0]
}

// expectEgressIPRouteInFRR checks whether the egress IP host route is exported on external FRR.
// When expectExported is true, node is the assigned egress node (nexthop is derived from it).
func expectEgressIPRouteInFRR(egressIP string, expectExported bool, node corev1.Node) {
	container, cmd := egressIPFRRRouteCmd(egressIP)
	if expectExported {
		nexthop := bgpEgressIPFRRNexthop(node, utilnet.IsIPv6String(egressIP))
		gomega.Eventually(func() bool {
			routes, err := infraprovider.Get().ExecExternalContainerCommand(container, cmd)
			framework.ExpectNoError(err)
			return strings.Contains(routes, nexthop)
		}, 90*time.Second, 5*time.Second).Should(gomega.BeTrue(),
			"expected FRR route for %s via %s on node %s", hostRoutePrefix(egressIP), nexthop, node.Name)
		return
	}
	gomega.Eventually(func() bool {
		routes, err := infraprovider.Get().ExecExternalContainerCommand(container, cmd)
		if err != nil {
			return false
		}
		return strings.TrimSpace(routes) == ""
	}, bgpEgressIPPollTimeout, 5*time.Second).Should(gomega.BeTrue(),
		"expected no FRR BGP route for egress IP prefix %s", hostRoutePrefix(egressIP))
}

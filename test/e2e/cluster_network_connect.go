// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	deploymentconfigapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
)

// ============================================================================
// CNC Test Constants
// ============================================================================
const (
	// Annotation keys used by the CNC controller
	ovnNetworkConnectSubnetAnnotation   = "k8s.ovn.org/network-connect-subnet"
	ovnConnectRouterTunnelKeyAnnotation = "k8s.ovn.org/connect-router-tunnel-key"

	// CNC connect subnet configuration
	cncConnectSubnetIPv4CIDR   = "192.168.0.0/16"
	cncConnectSubnetIPv4Prefix = 24
	// IPv6 networkPrefix must satisfy: 32 - ipv4Prefix == 128 - ipv6Prefix
	// With ipv4Prefix=24: 32-24=8, so ipv6Prefix must be 128-8=120
	cncConnectSubnetIPv6CIDR   = "fd00:10::/112"
	cncConnectSubnetIPv6Prefix = 120

	// Layer3 UDN CIDRs with hostSubnet (IPv4: /24, IPv6: /64)
	layer3UserDefinedNetworkIPv4CIDR       = "172.31.0.0/16"
	layer3UserDefinedNetworkIPv4HostSubnet = 24
	layer3UserDefinedNetworkIPv6CIDR       = "2014:100:200::0/60"
	layer3UserDefinedNetworkIPv6HostSubnet = 64

	// Layer2 UDN CIDRs
	layer2UserDefinedNetworkIPv4CIDR = "10.200.0.0/16"
	layer2UserDefinedNetworkIPv6CIDR = "2015:100:200::0/60"
)

// cncAnnotationSubnet represents the subnet annotation structure
type cncAnnotationSubnet struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

// ============================================================================
// CNC Test Global Utilities
// ============================================================================

// generateCNCName generates a random CNC name
func generateCNCName() string {
	return fmt.Sprintf("test-cnc-%s", rand.String(5))
}

// generateConnectSubnets generates connectSubnets YAML based on cluster IP family support
func generateConnectSubnets(cs clientset.Interface) string {
	return generateConnectSubnetsWithCIDRs(cs, cncConnectSubnetIPv4CIDR, cncConnectSubnetIPv4Prefix,
		cncConnectSubnetIPv6CIDR, cncConnectSubnetIPv6Prefix)
}

// generateConnectSubnetsWithCIDRs generates connectSubnets YAML with custom CIDRs
func generateConnectSubnetsWithCIDRs(cs clientset.Interface, v4CIDR string, v4Prefix int, v6CIDR string, v6Prefix int) string {
	var subnets []string
	if isIPv4Supported(cs) {
		subnets = append(subnets, fmt.Sprintf(`    - cidr: "%s"
      networkPrefix: %d`, v4CIDR, v4Prefix))
	}
	if isIPv6Supported(cs) {
		subnets = append(subnets, fmt.Sprintf(`    - cidr: "%s"
      networkPrefix: %d`, v6CIDR, v6Prefix))
	}
	return strings.Join(subnets, "\n")
}

// generateNetworkSubnets generates subnets YAML with custom CIDRs.
// Pass empty slices to use defaults.
func generateNetworkSubnets(cs clientset.Interface, topology string, v4Subnets, v6Subnets []string) string {
	// Use custom subnets if provided, otherwise fall back to defaults
	l3v4Subnets := []string{layer3UserDefinedNetworkIPv4CIDR}
	l3v6Subnets := []string{layer3UserDefinedNetworkIPv6CIDR}
	l2v4Subnets := []string{layer2UserDefinedNetworkIPv4CIDR}
	l2v6Subnets := []string{layer2UserDefinedNetworkIPv6CIDR}

	if len(v4Subnets) > 0 {
		l3v4Subnets = v4Subnets
		l2v4Subnets = v4Subnets
	}
	if len(v6Subnets) > 0 {
		l3v6Subnets = v6Subnets
		l2v6Subnets = v6Subnets
	}

	if topology == "Layer3" {
		var subnets []string
		if isIPv4Supported(cs) {
			for _, cidr := range l3v4Subnets {
				if len(strings.Split(cidr, "/")) == 2 {
					subnets = append(subnets, fmt.Sprintf(`{cidr: "%s", hostSubnet: %d}`, cidr, layer3UserDefinedNetworkIPv4HostSubnet))
				} else {
					subnets = append(subnets, generateLayer3Subnets(cidr)[0])
				}
			}
		}
		if isIPv6Supported(cs) {
			for _, cidr := range l3v6Subnets {
				if len(strings.Split(cidr, "/")) == 2 {
					subnets = append(subnets, fmt.Sprintf(`{cidr: "%s", hostSubnet: %d}`, cidr, layer3UserDefinedNetworkIPv6HostSubnet))
				} else {
					subnets = append(subnets, generateLayer3Subnets(cidr)[0])
				}
			}
		}
		return fmt.Sprintf("[%s]", strings.Join(subnets, ","))
	}
	// Layer2 format
	var quotedCidrs []string
	if isIPv4Supported(cs) {
		for _, cidr := range l2v4Subnets {
			quotedCidrs = append(quotedCidrs, fmt.Sprintf(`"%s"`, cidr))
		}
	}
	if isIPv6Supported(cs) {
		for _, cidr := range l2v6Subnets {
			quotedCidrs = append(quotedCidrs, fmt.Sprintf(`"%s"`, cidr))
		}
	}
	return fmt.Sprintf("[%s]", strings.Join(quotedCidrs, ","))
}

func generateLayer3NetworkSubnets(cs clientset.Interface, cidrs ...string) string {
	subnets := generateLayer3Subnets(joinStrings(filterCIDRs(cs, cidrs...)...))
	return fmt.Sprintf("[%s]", strings.Join(subnets, ","))
}

func generatePrimaryUDNSubnets(cs clientset.Interface, topology string, subnets []string) string {
	if len(subnets) == 0 {
		if topology == "Layer3" {
			subnets = []string{layer3UserDefinedNetworkIPv4CIDR, layer3UserDefinedNetworkIPv6CIDR}
		} else {
			subnets = []string{layer2UserDefinedNetworkIPv4CIDR, layer2UserDefinedNetworkIPv6CIDR}
		}
	}

	if topology == "Layer3" {
		return generateLayer3NetworkSubnets(cs, subnets...)
	}

	var quotedCidrs []string
	for _, cidr := range filterCIDRs(cs, subnets...) {
		quotedCidrs = append(quotedCidrs, fmt.Sprintf(`"%s"`, cidr))
	}
	return fmt.Sprintf("[%s]", strings.Join(quotedCidrs, ","))
}

func subnetList(subnets ...string) []string {
	return subnets
}

// newTestNetworkSubnetsAllocator returns a function that yields unique (v4CIDR, v6CIDR) pairs
// for UDN/CUDN creation within a single test.
//
// This prevents multiple networks in the same test from accidentally sharing the same CIDRs.
func newTestNetworkSubnetsAllocator() func(bool) ([]string, []string) {
	i := 0
	return func(isL3Topology bool) ([]string, []string) {
		i++
		// Allocated networks for ipv4 are 172.31.0.0/16, 172.32.0.0/16, ... (non-overlapping /16s)
		v4 := []string{fmt.Sprintf("172.%d.0.0/16", 30+i)}
		// Allocated networks for ipv6 are 2014:100:201::0/60, 2014:100:202::0/60, ... (non-overlapping /60s)
		v6 := []string{fmt.Sprintf("2014:100:%d::0/60", 200+i)}
		if isL3Topology && deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.L3UDNMultiSubnetConfig) {
			v4 = []string{
				fmt.Sprintf("172.%d.0.0/23/24", 30+i),
				fmt.Sprintf("172.%d.0.0/16/24", 31+i),
			}
			v6 = []string{
				fmt.Sprintf("2014:100:%d::0/63/64", 200+i),
				fmt.Sprintf("2014:100:%d::0/48/64", 201+i),
			}
			i++
		}
		return v4, v6
	}
}

// createUDNNamespaceWithName creates a namespace with UDN label and optional additional labels
func createUDNNamespaceWithName(cs clientset.Interface, name string, labels map[string]string) *corev1.Namespace {
	if labels == nil {
		labels = map[string]string{}
	}
	labels[RequiredUDNNamespaceLabel] = ""
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
	createdNs, err := cs.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	return createdNs
}

// createUDNNamespace creates a namespace with UDN label and a random suffix
func createUDNNamespace(cs clientset.Interface, baseName string, labels map[string]string) *corev1.Namespace {
	return createUDNNamespaceWithName(cs, baseName+"-"+rand.String(5), labels)
}

// deleteNamespace deletes a namespace
func deleteNamespace(cs clientset.Interface, nsName string) {
	_ = cs.CoreV1().Namespaces().Delete(context.Background(), nsName, metav1.DeleteOptions{})
}

// createOrUpdateCNC creates or updates a CNC with CUDN and/or PUDN selectors using default connect subnets
// Uses kubectl apply, so can be called to update an existing CNC
func createOrUpdateCNC(cs clientset.Interface, cncName string, cudnLabelSelector, udnLabelSelector map[string]string) {
	createOrUpdateCNCWithSubnetsAndConnectivity(cncName, cudnLabelSelector, udnLabelSelector,
		generateConnectSubnets(cs), []string{"PodNetwork"})
}

// createOrUpdateCNCWithSubnets creates or updates a CNC with custom connect subnets (PodNetwork connectivity only)
func createOrUpdateCNCWithSubnets(cncName string, cudnLabelSelector, udnLabelSelector map[string]string, connectSubnets string) {
	createOrUpdateCNCWithSubnetsAndConnectivity(cncName, cudnLabelSelector, udnLabelSelector,
		connectSubnets, []string{"PodNetwork"})
}

// createOrUpdateCNCWithConnectivity creates or updates a CNC with custom connectivity types
// connectivity should be a slice like []string{"PodNetwork"} or []string{"PodNetwork", "ServiceNetwork"}
func createOrUpdateCNCWithConnectivity(cs clientset.Interface, cncName string, cudnLabelSelector, udnLabelSelector map[string]string, connectivity []string) {
	createOrUpdateCNCWithSubnetsAndConnectivity(cncName, cudnLabelSelector, udnLabelSelector,
		generateConnectSubnets(cs), connectivity)
}

// generateCNCManifest builds a CNC YAML manifest string.
// annotations is optional; pass nil for no annotations.
func generateCNCManifest(
	cncName string,
	cudnLabelSelector, udnLabelSelector map[string]string,
	connectSubnets string,
	connectivity []string,
	annotations map[string]string,
) string {
	Expect(cudnLabelSelector != nil || udnLabelSelector != nil).To(BeTrue(),
		"generateCNCManifest requires at least one selector (cudnLabelSelector or udnLabelSelector)")

	var networkSelectors []string

	if cudnLabelSelector != nil {
		cudnLabelSelectorStr := ""
		for k, v := range cudnLabelSelector {
			if cudnLabelSelectorStr != "" {
				cudnLabelSelectorStr += "\n            "
			}
			cudnLabelSelectorStr += fmt.Sprintf("%s: \"%s\"", k, v)
		}
		networkSelectors = append(networkSelectors, fmt.Sprintf(`    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            %s`, cudnLabelSelectorStr))
	}

	if udnLabelSelector != nil {
		udnLabelSelectorStr := ""
		for k, v := range udnLabelSelector {
			if udnLabelSelectorStr != "" {
				udnLabelSelectorStr += "\n            "
			}
			udnLabelSelectorStr += fmt.Sprintf("%s: \"%s\"", k, v)
		}
		networkSelectors = append(networkSelectors, fmt.Sprintf(`    - networkSelectionType: "PrimaryUserDefinedNetworks"
      primaryUserDefinedNetworkSelector:
        namespaceSelector:
          matchLabels:
            %s`, udnLabelSelectorStr))
	}

	connectivityYAML := "["
	for i, c := range connectivity {
		if i > 0 {
			connectivityYAML += ", "
		}
		connectivityYAML += fmt.Sprintf(`"%s"`, c)
	}
	connectivityYAML += "]"

	annotationsBlock := ""
	if len(annotations) > 0 {
		lines := "  annotations:\n"
		for k, v := range annotations {
			lines += fmt.Sprintf("    %s: '%s'\n", k, v)
		}
		annotationsBlock = lines
	}

	return fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: %s
%sspec:
  networkSelectors:
%s
  connectSubnets:
%s
  connectivity: %s
`, cncName, annotationsBlock, strings.Join(networkSelectors, "\n"), connectSubnets, connectivityYAML)
}

// createOrUpdateCNCWithSubnetsAndConnectivity creates or updates a CNC with custom connect subnets and connectivity
func createOrUpdateCNCWithSubnetsAndConnectivity(
	cncName string,
	cudnLabelSelector, udnLabelSelector map[string]string,
	connectSubnets string,
	connectivity []string,
) {
	manifest := generateCNCManifest(
		cncName, cudnLabelSelector, udnLabelSelector,
		connectSubnets, connectivity, nil,
	)
	_, err := e2ekubectl.RunKubectlInput("", manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
}

// deleteCNC deletes a CNC
func deleteCNC(cncName string) {
	_, _ = e2ekubectl.RunKubectl("", "delete", "clusternetworkconnect", cncName, "--ignore-not-found")
}

// createPrimaryCUDNWithSubnets creates a primary CUDN with specified topology and custom subnets.
// Pass empty slices for v4Subnets/v6Subnets to use defaults.
func createPrimaryCUDNWithSubnets(cs clientset.Interface, cudnName, topology string, labels map[string]string, v4Subnets, v6Subnets []string, targetNamespaces ...string) {
	targetNs := strings.Join(targetNamespaces, ",")
	labelAnnotations := ""
	for k, v := range labels {
		if labelAnnotations != "" {
			labelAnnotations += "\n    "
		}
		labelAnnotations += fmt.Sprintf("%s: \"%s\"", k, v)
	}
	topologyLower := strings.ToLower(topology)
	manifest := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: %s
  labels:
    %s
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [ %s ]
  network:
    topology: %s
    %s:
      role: Primary
      subnets: %s
`, cudnName, labelAnnotations, targetNs, topology, topologyLower, generateNetworkSubnets(cs, topology, v4Subnets, v6Subnets))
	_, err := e2ekubectl.RunKubectlInput("", manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
}

// deleteCUDN deletes a CUDN
func deleteCUDN(cudnName string) {
	_, _ = e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", "--timeout=60s", "--ignore-not-found")
}

// createPrimaryUDNWithSubnets creates a primary UDN with specified topology and custom subnets.
// Pass empty slices for v4Subnets/v6Subnets to use defaults.
func createPrimaryUDNWithSubnets(cs clientset.Interface, namespace, udnName, topology string, v4Subnets, v6Subnets []string) {
	topologyLower := strings.ToLower(topology)
	subnets := append(append([]string{}, v4Subnets...), v6Subnets...)
	manifest := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: %s
spec:
  topology: %s
  %s:
    role: Primary
    subnets: %s
`, udnName, topology, topologyLower, generatePrimaryUDNSubnets(cs, topology, subnets))
	_, err := e2ekubectl.RunKubectlInput(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
}

// deleteUDN deletes a UDN
func deleteUDN(namespace, udnName string) {
	_, _ = e2ekubectl.RunKubectl(namespace, "delete", "userdefinednetwork", udnName, "--wait", "--timeout=60s", "--ignore-not-found")
}

// createLayer3PrimaryCUDNWithSubnets creates a Layer3 primary CUDN with custom subnets
func createLayer3PrimaryCUDNWithSubnets(cs clientset.Interface, cudnName string, labels map[string]string, v4Subnets, v6Subnets []string, targetNamespaces ...string) {
	createPrimaryCUDNWithSubnets(cs, cudnName, "Layer3", labels, v4Subnets, v6Subnets, targetNamespaces...)
}

// createLayer2PrimaryCUDNWithSubnets creates a Layer2 primary CUDN with custom subnets
func createLayer2PrimaryCUDNWithSubnets(cs clientset.Interface, cudnName string, labels map[string]string, v4Subnets, v6Subnets []string, targetNamespaces ...string) {
	createPrimaryCUDNWithSubnets(cs, cudnName, "Layer2", labels, v4Subnets, v6Subnets, targetNamespaces...)
}

// createLayer3PrimaryUDNWithSubnets creates a Layer3 primary UDN with custom subnets
func createLayer3PrimaryUDNWithSubnets(cs clientset.Interface, namespace, udnName string, v4Subnets, v6Subnets []string) {
	createPrimaryUDNWithSubnets(cs, namespace, udnName, "Layer3", v4Subnets, v6Subnets)
}

// createLayer2PrimaryUDNWithSubnets creates a Layer2 primary UDN with custom subnets
func createLayer2PrimaryUDNWithSubnets(cs clientset.Interface, namespace, udnName string, v4Subnets, v6Subnets []string) {
	createPrimaryUDNWithSubnets(cs, namespace, udnName, "Layer2", v4Subnets, v6Subnets)
}

// getCNCAnnotations gets CNC annotations
func getCNCAnnotations(cncName string) (map[string]string, error) {
	annotationsJSON, err := e2ekubectl.RunKubectl("", "get", "clusternetworkconnect", cncName, "-o", "jsonpath={.metadata.annotations}")
	if err != nil {
		return nil, err
	}
	if annotationsJSON == "" {
		return map[string]string{}, nil
	}
	var annotations map[string]string
	if err := json.Unmarshal([]byte(annotationsJSON), &annotations); err != nil {
		return nil, err
	}
	return annotations, nil
}

func getCNCNetworkIDForTopology(cncName, topology string) string {
	prefix := strings.ToLower(topology) + "_"
	var networkID string
	Eventually(func(g Gomega) {
		annotations, err := getCNCAnnotations(cncName)
		g.Expect(err).NotTo(HaveOccurred())
		subnetAnnotation := annotations[ovnNetworkConnectSubnetAnnotation]
		var subnets map[string]cncAnnotationSubnet
		g.Expect(json.Unmarshal([]byte(subnetAnnotation), &subnets)).To(Succeed())

		foundID := ""
		for owner := range subnets {
			if strings.HasPrefix(owner, prefix) {
				foundID = strings.TrimPrefix(owner, prefix)
				break
			}
		}
		g.Expect(foundID).NotTo(BeEmpty(), "CNC %s should have a %s owner key", cncName, topology)
		networkID = foundID
	}, 60*time.Second, 2*time.Second).Should(Succeed())
	return networkID
}

func getNodeIDAnnotation(cs clientset.Interface, nodeName string) string {
	node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	nodeID := node.Annotations["k8s.ovn.org/node-id"]
	Expect(nodeID).NotTo(BeEmpty(), "node %s should have k8s.ovn.org/node-id annotation", nodeName)
	return nodeID
}

// findOVNNBDBPod returns any running NB database pod. It is suitable for
// queries against a shared NBDB; local-zone callers should select by node.
func findOVNNBDBPod(cs clientset.Interface, ovnNamespace string) (*corev1.Pod, error) {
	return findOVNNBDBPodOnNode(cs, ovnNamespace, "")
}

var errNoOVNNBDBPodOnNode = errors.New("no running local NB database pod found on node")

// findOVNNBDBPodOnNode returns the running local NB database pod hosted on
// nodeName. An empty nodeName selects any pod for callers using a shared NBDB.
func findOVNNBDBPodOnNode(cs clientset.Interface, ovnNamespace, nodeName string) (*corev1.Pod, error) {
	pods, err := cs.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: "ovn-db-pod=true"})
	if err != nil {
		return nil, fmt.Errorf("failed to list OVN DB pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		if nodeName != "" && pod.Spec.NodeName != nodeName {
			continue
		}
		for _, container := range pod.Spec.Containers {
			if container.Name == "nb-ovsdb" {
				return pod, nil
			}
		}
	}
	if nodeName != "" {
		return nil, fmt.Errorf("%w %s in namespace %s", errNoOVNNBDBPodOnNode, nodeName, ovnNamespace)
	}
	return nil, fmt.Errorf("no running OVN DB pod with nb-ovsdb container found in namespace %s", ovnNamespace)
}

// findOVNNBDBPodForNode selects the NB database serving nodeName in
// local-zone deployments, or any NB database pod when NBDB is shared.
func findOVNNBDBPodForNode(cs clientset.Interface, ovnNamespace, nodeName string) (*corev1.Pod, error) {
	dbPod, err := findOVNNBDBPodOnNode(cs, ovnNamespace, nodeName)
	if err == nil {
		return dbPod, nil
	}
	if !errors.Is(err, errNoOVNNBDBPodOnNode) {
		return nil, err
	}
	nodeLookupErr := err

	// Do not fall back to another node's database when this is a local-zone
	// installation. Its per-node NB pods are labeled app/name=ovnkube-node.
	pods, listErr := cs.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: "ovn-db-pod=true"})
	if listErr != nil {
		return nil, fmt.Errorf("failed to list OVN DB pods: %w", listErr)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !podHasContainer(pod, "nb-ovsdb") {
			continue
		}
		if pod.Labels["app"] == "ovnkube-node" || pod.Labels["name"] == "ovnkube-node" {
			return nil, fmt.Errorf("%w; found local-zone NB database pod %s on node %s, refusing to query another zone", nodeLookupErr, pod.Name, pod.Spec.NodeName)
		}
	}

	// Centralized deployments do not have an NB database pod on each worker;
	// their NBDB is shared, so any running NB database pod is equivalent.
	return findOVNNBDBPod(cs, ovnNamespace)
}

// podHasContainer reports whether the pod declares a container with the given name.
func podHasContainer(pod *corev1.Pod, containerName string) bool {
	for _, container := range pod.Spec.Containers {
		if container.Name == containerName {
			return true
		}
	}
	return false
}

// runOVNNBCTL runs ovn-nbctl against any available NB pod. Use it only when
// the database is shared; callers inspecting local-zone state must select a pod.
func runOVNNBCTL(f *framework.Framework, cs clientset.Interface, args ...string) (string, error) {
	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	dbPod, err := findOVNNBDBPod(cs, ovnNamespace)
	if err != nil {
		return "", err
	}
	return runOVNNBCTLInPod(f, ovnNamespace, dbPod.Name, args...)
}

// runOVNNBCTLInPod runs ovn-nbctl in the explicitly selected NB pod so a
// caller can query the database serving a particular OVN zone.
func runOVNNBCTLInPod(f *framework.Framework, ovnNamespace, podName string, args ...string) (string, error) {
	cmd := append([]string{"ovn-nbctl"}, args...)
	stdout, stderr, err := ExecCommandInContainerWithFullOutput(f, ovnNamespace, podName, "nb-ovsdb", cmd...)
	if err != nil {
		return stdout, fmt.Errorf("failed running ovn-nbctl %v on %s/%s: %w, stderr: %s", args, ovnNamespace, podName, err, stderr)
	}
	return strings.TrimSpace(stdout), nil
}

func findCNCNodeOVNObjects(f *framework.Framework, cs clientset.Interface, table, columns, cncName, networkID, nodeID string) (string, error) {
	return runOVNNBCTL(f, cs,
		"--data=bare",
		"--no-heading",
		"--columns="+columns,
		"find", table,
		fmt.Sprintf("external_ids:k8s.ovn.org/name=%s", cncName),
		fmt.Sprintf("external_ids:network-id=%s", networkID),
		fmt.Sprintf("external_ids:node-id=%s", nodeID),
	)
}

func verifyCNCNodeOVNObjects(f *framework.Framework, cs clientset.Interface, cncName, networkID, nodeID string, expectPresent bool) {
	Eventually(func(g Gomega) {
		ports, err := findCNCNodeOVNObjects(f, cs, "Logical_Router_Port", "name", cncName, networkID, nodeID)
		g.Expect(err).NotTo(HaveOccurred())
		routes, err := findCNCNodeOVNObjects(f, cs, "Logical_Router_Static_Route", "ip_prefix,nexthop", cncName, networkID, nodeID)
		g.Expect(err).NotTo(HaveOccurred())
		if expectPresent {
			g.Expect(ports).NotTo(BeEmpty(), "expected CNC %s L3 router ports for network ID %s node ID %s", cncName, networkID, nodeID)
			g.Expect(routes).NotTo(BeEmpty(), "expected CNC %s L3 static routes for network ID %s node ID %s", cncName, networkID, nodeID)
			return
		}
		g.Expect(ports).To(BeEmpty(), "expected CNC %s L3 router ports for network ID %s node ID %s to be removed", cncName, networkID, nodeID)
		g.Expect(routes).To(BeEmpty(), "expected CNC %s L3 static routes for network ID %s node ID %s to be removed", cncName, networkID, nodeID)
	}, 60*time.Second, 2*time.Second).Should(Succeed())
}

// getCNCTunnelID gets CNC tunnel ID from annotations
func getCNCTunnelID(cncName string) string {
	annotations, err := getCNCAnnotations(cncName)
	Expect(err).NotTo(HaveOccurred())
	return annotations[ovnConnectRouterTunnelKeyAnnotation]
}

type cncStatusPayload struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// getCNCCondition returns the requested CNC status condition (or nil if missing).
func getCNCCondition(cncName, conditionType string) (*metav1.Condition, error) {
	out, err := e2ekubectl.RunKubectl("", "get", "clusternetworkconnect", cncName, "-o", "jsonpath={.status}")
	if err != nil {
		return nil, err
	}
	var payload cncStatusPayload
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return nil, err
	}
	for i := range payload.Conditions {
		if payload.Conditions[i].Type == conditionType {
			return &payload.Conditions[i], nil
		}
	}
	return nil, nil
}

func verifyCNCAcceptedConditionFailed(cncName, errorSubstring string) {
	Expect(errorSubstring).NotTo(BeEmpty(), "verifyCNCAcceptedConditionFailed requires a non-empty error substring")
	Eventually(func(g Gomega) {
		cond, err := getCNCCondition(cncName, "Accepted")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cond).NotTo(BeNil(), "CNC should have an Accepted condition")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal("ResourceAllocationFailed"))
		g.Expect(cond.Message).To(ContainSubstring(errorSubstring))
	}, 5*time.Second, 500*time.Millisecond).Should(Succeed())
}

// verifyCNCHasOnlyTunnelIDAnnotation verifies CNC has only tunnel ID annotation
func verifyCNCHasOnlyTunnelIDAnnotation(cncName string) {
	Eventually(func(g Gomega) {
		annotations, err := getCNCAnnotations(cncName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(annotations).To(HaveKey(ovnConnectRouterTunnelKeyAnnotation), "CNC should have tunnel ID annotation")
		if subnetAnnotation, exists := annotations[ovnNetworkConnectSubnetAnnotation]; exists {
			g.Expect(subnetAnnotation).To(Equal("{}"), "subnet annotation should be empty when no networks match")
		}
	}, 30*time.Second, 1*time.Second).Should(Succeed())
}

// verifyCNCHasBothAnnotations verifies CNC has both tunnel ID and subnet annotations
func verifyCNCHasBothAnnotations(cncName string) {
	Eventually(func(g Gomega) {
		annotations, err := getCNCAnnotations(cncName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(annotations).To(HaveKey(ovnConnectRouterTunnelKeyAnnotation), "CNC should have tunnel ID annotation")
		g.Expect(annotations).To(HaveKey(ovnNetworkConnectSubnetAnnotation), "CNC should have subnet annotation")
		subnetAnnotation := annotations[ovnNetworkConnectSubnetAnnotation]
		g.Expect(subnetAnnotation).NotTo(Equal("{}"), "subnet annotation should not be empty when networks match")
		var subnets map[string]cncAnnotationSubnet
		err = json.Unmarshal([]byte(subnetAnnotation), &subnets)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(len(subnets)).To(BeNumerically(">", 0), "should have at least one network subnet")
	}, 60*time.Second, 2*time.Second).Should(Succeed())
}

// verifyCNCSubnetAnnotationNetworkCount verifies CNC subnet annotation has expected network count
func verifyCNCSubnetAnnotationNetworkCount(cncName string, expectedCount int) {
	Eventually(func(g Gomega) {
		annotations, err := getCNCAnnotations(cncName)
		g.Expect(err).NotTo(HaveOccurred())
		subnetAnnotation := annotations[ovnNetworkConnectSubnetAnnotation]
		var subnets map[string]cncAnnotationSubnet
		err = json.Unmarshal([]byte(subnetAnnotation), &subnets)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(len(subnets)).To(Equal(expectedCount), fmt.Sprintf("should have %d network subnets", expectedCount))
	}, 60*time.Second, 2*time.Second).Should(Succeed())
}

// verifyCNCSubnetAnnotationContent verifies subnet annotation content: key format, topology counts, and CIDR format
// expectedTopologies is a list of expected topologies (e.g., ["Layer3", "Layer2", "Layer3"])
func verifyCNCSubnetAnnotationContent(cncName string, expectedTopologies []string) {
	Eventually(func(g Gomega) {
		annotations, err := getCNCAnnotations(cncName)
		g.Expect(err).NotTo(HaveOccurred())
		subnetAnnotation := annotations[ovnNetworkConnectSubnetAnnotation]
		var subnets map[string]cncAnnotationSubnet
		err = json.Unmarshal([]byte(subnetAnnotation), &subnets)
		g.Expect(err).NotTo(HaveOccurred())

		// Count topologies found
		topologyCounts := map[string]int{"layer2": 0, "layer3": 0}
		for networkKey, subnet := range subnets {
			// Key format should be <topology>_<networkID> e.g., "layer3_1", "layer2_2"
			g.Expect(networkKey).To(MatchRegexp(`^(layer2|layer3)_\d+$`),
				fmt.Sprintf("network key %s should match format <topology>_<networkID>", networkKey))

			if strings.HasPrefix(networkKey, "layer2_") {
				topologyCounts["layer2"]++
			} else if strings.HasPrefix(networkKey, "layer3_") {
				topologyCounts["layer3"]++
			}

			// Verify at least one of IPv4 or IPv6 is present
			hasIPv4 := subnet.IPv4 != ""
			hasIPv6 := subnet.IPv6 != ""
			g.Expect(hasIPv4 || hasIPv6).To(BeTrue(),
				fmt.Sprintf("network %s should have at least one subnet", networkKey))

			isLayer2 := strings.HasPrefix(networkKey, "layer2_")

			// Verify IPv4 format if present (should be CIDR within connectSubnets range)
			if hasIPv4 {
				g.Expect(subnet.IPv4).To(MatchRegexp(`^192\.16[89]\.\d+\.\d+/\d+$`),
					fmt.Sprintf("network %s IPv4 subnet should be in connectSubnets range (192.168.x.x or 192.169.x.x)", networkKey))
				// Layer2 networks use point-to-point /31 subnets
				if isLayer2 {
					g.Expect(subnet.IPv4).To(HaveSuffix("/31"),
						fmt.Sprintf("Layer2 network %s IPv4 should have /31 mask", networkKey))
				}
			}

			// Verify IPv6 format if present (should be CIDR within connectSubnets range)
			if hasIPv6 {
				g.Expect(subnet.IPv6).To(MatchRegexp(`^fd00:1[01]::[0-9a-f:]*/\d+$`),
					fmt.Sprintf("network %s IPv6 subnet should be in connectSubnets range (fd00:10:: or fd00:11::)", networkKey))
				// Layer2 networks use point-to-point /127 subnets
				if isLayer2 {
					g.Expect(subnet.IPv6).To(HaveSuffix("/127"),
						fmt.Sprintf("Layer2 network %s IPv6 should have /127 mask", networkKey))
				}
			}
		}

		// Verify expected topology counts match
		expectedCounts := map[string]int{"layer2": 0, "layer3": 0}
		for _, topo := range expectedTopologies {
			expectedCounts[strings.ToLower(topo)]++
		}
		g.Expect(topologyCounts["layer2"]).To(Equal(expectedCounts["layer2"]),
			fmt.Sprintf("expected %d Layer2 networks, got %d", expectedCounts["layer2"], topologyCounts["layer2"]))
		g.Expect(topologyCounts["layer3"]).To(Equal(expectedCounts["layer3"]),
			fmt.Sprintf("expected %d Layer3 networks, got %d", expectedCounts["layer3"], topologyCounts["layer3"]))
	}, 60*time.Second, 2*time.Second).Should(Succeed())
}

var _ = Describe("ClusterNetworkConnect ClusterManagerController", feature.NetworkConnect, func() {
	f := wrappedTestFramework("cnc-controller")
	// disable automatic namespace creation, we need to add the required UDN label
	f.SkipNamespaceCreation = true

	var (
		cs clientset.Interface
	)

	BeforeEach(func() {
		cs = f.ClientSet
	})

	// ===========================================
	// Group 1: No Matching Networks (1 test)
	// ===========================================
	Context("when CNC has no matching networks", func() {
		It("has only tunnel ID annotation", func() {
			cncName := generateCNCName()
			DeferCleanup(func() {
				deleteCNC(cncName)
			})

			By("creating a CNC with selector that matches no networks")
			createOrUpdateCNC(cs, cncName, map[string]string{"nonexistent": "label"}, nil)

			By("verifying CNC has only tunnel ID annotation")
			verifyCNCHasOnlyTunnelIDAnnotation(cncName)

			By("verifying tunnel ID is valid")
			tunnelID := getCNCTunnelID(cncName)
			Expect(tunnelID).NotTo(BeEmpty(), "CNC should have tunnel ID even with no matching networks")
		})
	})

	// ===========================================
	// Group 2: Static Creation - Networks exist first, then CNC created (7 tests)
	// ===========================================
	Context("when networks exist before CNC creation", func() {
		// Single network tests using DescribeTable
		DescribeTable("single network: has both subnet and tunnel ID annotations",
			func(ctx SpecContext, topology, kind string) {
				cncName := generateCNCName()
				networkName := fmt.Sprintf("test-%s-%s", strings.ToLower(kind), rand.String(5))
				testLabel := map[string]string{fmt.Sprintf("test-%s-%s", strings.ToLower(kind), strings.ToLower(topology)): "true"}
				nextSubnets := newTestNetworkSubnetsAllocator()

				if kind == "UDN" {
					ns := createUDNNamespace(cs, fmt.Sprintf("test-%s-%s", strings.ToLower(kind), strings.ToLower(topology)), testLabel)
					DeferCleanup(func() {
						deleteCNC(cncName)
						deleteUDN(ns.Name, networkName)
						cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
					})

					By(fmt.Sprintf("creating a %s primary UDN", topology))
					v4, v6 := nextSubnets(topology == "Layer3")
					createPrimaryUDNWithSubnets(cs, ns.Name, networkName, topology, v4, v6)

					By("waiting for UDN to be ready")
					Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns.Name, networkName), 30*time.Second, time.Second).Should(Succeed())

					By("creating a CNC with PUDN selector")
					createOrUpdateCNC(cs, cncName, nil, testLabel)
				} else {
					ns := createUDNNamespace(cs, fmt.Sprintf("test-%s-%s", strings.ToLower(kind), strings.ToLower(topology)), nil)
					DeferCleanup(func() {
						deleteCNC(cncName)
						deleteCUDN(networkName)
						cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
					})

					By(fmt.Sprintf("creating a %s primary CUDN", topology))
					v4, v6 := nextSubnets(topology == "Layer3")
					createPrimaryCUDNWithSubnets(cs, networkName, topology, testLabel, v4, v6, ns.Name)

					By("waiting for CUDN to be ready")
					Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, networkName), 30*time.Second, time.Second).Should(Succeed())

					By("creating a CNC with CUDN selector")
					createOrUpdateCNC(cs, cncName, testLabel, nil)
				}

				By("verifying CNC has both subnet and tunnel ID annotations")
				verifyCNCHasBothAnnotations(cncName)
				verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
				verifyCNCSubnetAnnotationContent(cncName, []string{topology})
			},
			Entry("L3 P-UDN", "Layer3", "UDN"),
			Entry("L2 P-UDN", "Layer2", "UDN"),
			Entry("L3 P-CUDN", "Layer3", "CUDN"),
			Entry("L2 P-CUDN", "Layer2", "CUDN"),
		)

		// Multiple networks of same kind tests using DescribeTable
		DescribeTable("multiple networks (2xL3 + 2xL2): has all networks in subnet annotation",
			func(ctx SpecContext, kind string) {
				cncName := generateCNCName()
				testLabel := map[string]string{fmt.Sprintf("test-multi-%s", strings.ToLower(kind)): "true"}
				var namespaces []*corev1.Namespace
				var networkNames []string
				var expectedTopologies []string
				nextSubnets := newTestNetworkSubnetsAllocator()

				if kind == "UDN" {
					// Create 4 namespaces with the same label for PUDN selector
					for i := 1; i <= 4; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-udn-%d", i), testLabel))
						networkNames = append(networkNames, fmt.Sprintf("udn%d", i))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						for i, ns := range namespaces {
							deleteUDN(ns.Name, networkNames[i])
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By("creating 2 Layer3 and 2 Layer2 primary UDNs")
					v4, v6 := nextSubnets(true)
					createLayer3PrimaryUDNWithSubnets(cs, namespaces[0].Name, networkNames[0], v4, v6)
					expectedTopologies = append(expectedTopologies, "Layer3")
					v4, v6 = nextSubnets(true)
					createLayer3PrimaryUDNWithSubnets(cs, namespaces[1].Name, networkNames[1], v4, v6)
					expectedTopologies = append(expectedTopologies, "Layer3")
					v4, v6 = nextSubnets(false)
					createLayer2PrimaryUDNWithSubnets(cs, namespaces[2].Name, networkNames[2], v4, v6)
					expectedTopologies = append(expectedTopologies, "Layer2")
					v4, v6 = nextSubnets(false)
					createLayer2PrimaryUDNWithSubnets(cs, namespaces[3].Name, networkNames[3], v4, v6)
					expectedTopologies = append(expectedTopologies, "Layer2")

					By("waiting for all UDNs to be ready")
					for i, ns := range namespaces {
						Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns.Name, networkNames[i]), 30*time.Second, time.Second).Should(Succeed())
					}

					By("creating a CNC with PUDN selector")
					createOrUpdateCNC(cs, cncName, nil, testLabel)
				} else {
					// CUDN case - one CUDN targets multiple namespaces
					for i := 1; i <= 5; i++ { // 5 namespaces for multi-ns CUDN test
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-cudn-ns%d", i), nil))
						networkNames = append(networkNames, fmt.Sprintf("cudn-%d-%s", i, rand.String(5)))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						for i := 0; i < 4; i++ { // only 4 CUDNs
							deleteCUDN(networkNames[i])
						}
						for _, ns := range namespaces {
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By("creating 2 Layer3 and 2 Layer2 primary CUDNs (one L3 targets multiple namespaces)")
					v4, v6 := nextSubnets(true)
					createPrimaryCUDNWithSubnets(cs, networkNames[0], "Layer3", testLabel, v4, v6, namespaces[0].Name)
					expectedTopologies = append(expectedTopologies, "Layer3")
					v4, v6 = nextSubnets(true)
					createPrimaryCUDNWithSubnets(cs, networkNames[1], "Layer3", testLabel, v4, v6, namespaces[1].Name, namespaces[4].Name) // multi-ns
					expectedTopologies = append(expectedTopologies, "Layer3")
					v4, v6 = nextSubnets(false)
					createPrimaryCUDNWithSubnets(cs, networkNames[2], "Layer2", testLabel, v4, v6, namespaces[2].Name)
					expectedTopologies = append(expectedTopologies, "Layer2")
					v4, v6 = nextSubnets(false)
					createPrimaryCUDNWithSubnets(cs, networkNames[3], "Layer2", testLabel, v4, v6, namespaces[3].Name)
					expectedTopologies = append(expectedTopologies, "Layer2")

					By("waiting for all CUDNs to be ready")
					for i := 0; i < 4; i++ {
						Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, networkNames[i]), 30*time.Second, time.Second).Should(Succeed())
					}

					By("creating a CNC with CUDN selector")
					createOrUpdateCNC(cs, cncName, testLabel, nil)
				}

				By("verifying CNC has 4 networks in subnet annotation")
				verifyCNCHasBothAnnotations(cncName)
				verifyCNCSubnetAnnotationNetworkCount(cncName, 4)
				verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
			},
			Entry("P-UDNs", "UDN"),
			Entry("P-CUDNs (one multi-ns)", "CUDN"),
		)

		It("full matrix (2x each type) - has all 8 networks in subnet annotation", func(ctx SpecContext) {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-full-matrix": "true"}
			udnLabel := map[string]string{"test-full-matrix": "true"}
			nextSubnets := newTestNetworkSubnetsAllocator()

			var cudnNames []string
			var udnNames []string
			var cudnNamespaces []*corev1.Namespace
			var udnNamespaces []*corev1.Namespace
			var expectedTopologies []string

			// Create namespaces and network names
			for i := 1; i <= 4; i++ {
				cudnNames = append(cudnNames, fmt.Sprintf("fm-cudn-%d-%s", i, rand.String(5)))
				udnNames = append(udnNames, fmt.Sprintf("udn%d", i))
				cudnNamespaces = append(cudnNamespaces, createUDNNamespace(cs, fmt.Sprintf("fm-cudn-ns%d", i), nil))
				udnNamespaces = append(udnNamespaces, createUDNNamespace(cs, fmt.Sprintf("fm-udn-ns%d", i), udnLabel))
			}

			DeferCleanup(func() {
				deleteCNC(cncName)
				for _, name := range cudnNames {
					deleteCUDN(name)
				}
				for i, ns := range udnNamespaces {
					deleteUDN(ns.Name, udnNames[i])
				}
				for _, ns := range cudnNamespaces {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
				for _, ns := range udnNamespaces {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
			})

			By("creating 4 CUDNs (2xL3 + 2xL2)")
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnNames[0], cudnLabel, v4, v6, cudnNamespaces[0].Name)
			expectedTopologies = append(expectedTopologies, "Layer3")
			v4, v6 = nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnNames[1], cudnLabel, v4, v6, cudnNamespaces[1].Name)
			expectedTopologies = append(expectedTopologies, "Layer3")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, cudnNames[2], cudnLabel, v4, v6, cudnNamespaces[2].Name)
			expectedTopologies = append(expectedTopologies, "Layer2")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, cudnNames[3], cudnLabel, v4, v6, cudnNamespaces[3].Name)
			expectedTopologies = append(expectedTopologies, "Layer2")

			By("creating 4 UDNs (2xL3 + 2xL2)")
			v4, v6 = nextSubnets(true)
			createLayer3PrimaryUDNWithSubnets(cs, udnNamespaces[0].Name, udnNames[0], v4, v6)
			expectedTopologies = append(expectedTopologies, "Layer3")
			v4, v6 = nextSubnets(true)
			createLayer3PrimaryUDNWithSubnets(cs, udnNamespaces[1].Name, udnNames[1], v4, v6)
			expectedTopologies = append(expectedTopologies, "Layer3")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, udnNamespaces[2].Name, udnNames[2], v4, v6)
			expectedTopologies = append(expectedTopologies, "Layer2")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, udnNamespaces[3].Name, udnNames[3], v4, v6)
			expectedTopologies = append(expectedTopologies, "Layer2")

			By("waiting for all networks to be ready")
			for _, name := range cudnNames {
				Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, name), 30*time.Second, time.Second).Should(Succeed())
			}
			for i, ns := range udnNamespaces {
				Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns.Name, udnNames[i]), 30*time.Second, time.Second).Should(Succeed())
			}

			By("creating a CNC with both CUDN and PUDN selectors")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("verifying CNC has all 8 networks in subnet annotation")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 8)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
		})
	})

	// ===========================================
	// Group 3: Dynamic Creation - CNC created first, then networks (7 tests)
	// ===========================================
	Context("when CNC is created before networks", func() {
		// Single network tests using DescribeTable
		DescribeTable("single network created after CNC: annotations are updated",
			func(ctx SpecContext, topology, kind string) {
				cncName := generateCNCName()
				networkName := fmt.Sprintf("test-%s-%s", strings.ToLower(kind), rand.String(5))
				testLabel := map[string]string{fmt.Sprintf("test-dyn-%s-%s", strings.ToLower(kind), strings.ToLower(topology)): "true"}
				var expectedTopologies []string

				if kind == "UDN" {
					ns := createUDNNamespace(cs, fmt.Sprintf("test-dyn-%s-%s", strings.ToLower(kind), strings.ToLower(topology)), testLabel)
					nextSubnets := newTestNetworkSubnetsAllocator()
					DeferCleanup(func() {
						deleteCNC(cncName)
						deleteUDN(ns.Name, networkName)
						cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
					})

					By("creating a CNC with PUDN selector (no matching networks yet)")
					createOrUpdateCNC(cs, cncName, nil, testLabel)

					By("verifying CNC has only tunnel ID annotation initially")
					verifyCNCHasOnlyTunnelIDAnnotation(cncName)

					By(fmt.Sprintf("creating a %s primary UDN", topology))
					v4, v6 := nextSubnets(topology == "Layer3")
					createPrimaryUDNWithSubnets(cs, ns.Name, networkName, topology, v4, v6)
					expectedTopologies = append(expectedTopologies, topology)

					By("waiting for UDN to be ready")
					Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns.Name, networkName), 30*time.Second, time.Second).Should(Succeed())
				} else {
					ns := createUDNNamespace(cs, fmt.Sprintf("test-dyn-%s-%s", strings.ToLower(kind), strings.ToLower(topology)), nil)
					nextSubnets := newTestNetworkSubnetsAllocator()
					DeferCleanup(func() {
						deleteCNC(cncName)
						deleteCUDN(networkName)
						cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
					})

					By("creating a CNC with CUDN selector (no matching networks yet)")
					createOrUpdateCNC(cs, cncName, testLabel, nil)

					By("verifying CNC has only tunnel ID annotation initially")
					verifyCNCHasOnlyTunnelIDAnnotation(cncName)

					By(fmt.Sprintf("creating a %s primary CUDN", topology))
					v4, v6 := nextSubnets(topology == "Layer3")
					createPrimaryCUDNWithSubnets(cs, networkName, topology, testLabel, v4, v6, ns.Name)
					expectedTopologies = append(expectedTopologies, topology)

					By("waiting for CUDN to be ready")
					Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, networkName), 30*time.Second, time.Second).Should(Succeed())
				}

				By("verifying CNC annotations are updated to include the network")
				verifyCNCHasBothAnnotations(cncName)
				verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
				verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
			},
			Entry("L3 P-UDN", "Layer3", "UDN"),
			Entry("L2 P-UDN", "Layer2", "UDN"),
			Entry("L3 P-CUDN", "Layer3", "CUDN"),
			Entry("L2 P-CUDN", "Layer2", "CUDN"),
		)

		// Multiple networks created after CNC
		DescribeTable("multiple networks created after CNC: annotations are updated",
			func(ctx SpecContext, kind string) {
				cncName := generateCNCName()
				testLabel := map[string]string{fmt.Sprintf("test-dyn-multi-%s", strings.ToLower(kind)): "true"}
				var namespaces []*corev1.Namespace
				var networkNames []string
				var expectedTopologies []string
				nextSubnets := newTestNetworkSubnetsAllocator()

				if kind == "UDN" {
					// Create namespaces first (with label for PUDN selector)
					for i := 1; i <= 4; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-dyn-udn-%d", i), testLabel))
						networkNames = append(networkNames, fmt.Sprintf("udn%d", i))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						for i, ns := range namespaces {
							deleteUDN(ns.Name, networkNames[i])
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By("creating a CNC with PUDN selector (no matching networks yet)")
					createOrUpdateCNC(cs, cncName, nil, testLabel)

					By("verifying CNC has only tunnel ID annotation initially")
					verifyCNCHasOnlyTunnelIDAnnotation(cncName)

					By("creating 2 Layer3 and 2 Layer2 primary UDNs")
					v4, v6 := nextSubnets(true)
					createLayer3PrimaryUDNWithSubnets(cs, namespaces[0].Name, networkNames[0], v4, v6)
					expectedTopologies = append(expectedTopologies, "Layer3")
					v4, v6 = nextSubnets(true)
					createLayer3PrimaryUDNWithSubnets(cs, namespaces[1].Name, networkNames[1], v4, v6)
					expectedTopologies = append(expectedTopologies, "Layer3")
					v4, v6 = nextSubnets(false)
					createLayer2PrimaryUDNWithSubnets(cs, namespaces[2].Name, networkNames[2], v4, v6)
					expectedTopologies = append(expectedTopologies, "Layer2")
					v4, v6 = nextSubnets(false)
					createLayer2PrimaryUDNWithSubnets(cs, namespaces[3].Name, networkNames[3], v4, v6)
					expectedTopologies = append(expectedTopologies, "Layer2")

					By("waiting for all UDNs to be ready")
					for i, ns := range namespaces {
						Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns.Name, networkNames[i]), 30*time.Second, time.Second).Should(Succeed())
					}
				} else {
					// CUDN case
					for i := 1; i <= 5; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-dyn-cudn-ns%d", i), nil))
						networkNames = append(networkNames, fmt.Sprintf("dyn-cudn-%d-%s", i, rand.String(5)))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						for i := 0; i < 4; i++ {
							deleteCUDN(networkNames[i])
						}
						for _, ns := range namespaces {
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By("creating a CNC with CUDN selector (no matching networks yet)")
					createOrUpdateCNC(cs, cncName, testLabel, nil)

					By("verifying CNC has only tunnel ID annotation initially")
					verifyCNCHasOnlyTunnelIDAnnotation(cncName)

					By("creating 2 Layer3 and 2 Layer2 primary CUDNs (one L3 targets multiple namespaces)")
					v4, v6 := nextSubnets(true)
					createPrimaryCUDNWithSubnets(cs, networkNames[0], "Layer3", testLabel, v4, v6, namespaces[0].Name)
					expectedTopologies = append(expectedTopologies, "Layer3")
					v4, v6 = nextSubnets(true)
					createPrimaryCUDNWithSubnets(cs, networkNames[1], "Layer3", testLabel, v4, v6, namespaces[1].Name, namespaces[4].Name)
					expectedTopologies = append(expectedTopologies, "Layer3")
					v4, v6 = nextSubnets(false)
					createPrimaryCUDNWithSubnets(cs, networkNames[2], "Layer2", testLabel, v4, v6, namespaces[2].Name)
					expectedTopologies = append(expectedTopologies, "Layer2")
					v4, v6 = nextSubnets(false)
					createPrimaryCUDNWithSubnets(cs, networkNames[3], "Layer2", testLabel, v4, v6, namespaces[3].Name)
					expectedTopologies = append(expectedTopologies, "Layer2")

					By("waiting for all CUDNs to be ready")
					for i := 0; i < 4; i++ {
						Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, networkNames[i]), 30*time.Second, time.Second).Should(Succeed())
					}
				}

				By("verifying CNC has 4 networks in subnet annotation")
				verifyCNCHasBothAnnotations(cncName)
				verifyCNCSubnetAnnotationNetworkCount(cncName, 4)
				verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
			},
			Entry("P-UDNs", "UDN"),
			Entry("P-CUDNs (one multi-ns)", "CUDN"),
		)

		It("full matrix created after CNC - annotations are updated with all 8 networks", func(ctx SpecContext) {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-dyn-full-matrix": "true"}
			udnLabel := map[string]string{"test-dyn-full-matrix": "true"}

			var cudnNames []string
			var udnNames []string
			var cudnNamespaces []*corev1.Namespace
			var udnNamespaces []*corev1.Namespace
			var expectedTopologies []string

			// Create namespaces first
			for i := 1; i <= 4; i++ {
				cudnNames = append(cudnNames, fmt.Sprintf("dyn-fm-cudn-%d-%s", i, rand.String(5)))
				udnNames = append(udnNames, fmt.Sprintf("udn%d", i))
				cudnNamespaces = append(cudnNamespaces, createUDNNamespace(cs, fmt.Sprintf("dyn-fm-cudn-ns%d", i), nil))
				udnNamespaces = append(udnNamespaces, createUDNNamespace(cs, fmt.Sprintf("dyn-fm-udn-ns%d", i), udnLabel))
			}

			DeferCleanup(func() {
				deleteCNC(cncName)
				for _, name := range cudnNames {
					deleteCUDN(name)
				}
				for i, ns := range udnNamespaces {
					deleteUDN(ns.Name, udnNames[i])
				}
				for _, ns := range cudnNamespaces {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
				for _, ns := range udnNamespaces {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
			})

			By("creating a CNC with both CUDN and PUDN selectors (no matching networks yet)")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("verifying CNC has only tunnel ID annotation initially")
			verifyCNCHasOnlyTunnelIDAnnotation(cncName)

			By("creating 4 CUDNs (2xL3 + 2xL2)")
			nextSubnets := newTestNetworkSubnetsAllocator()
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnNames[0], cudnLabel, v4, v6, cudnNamespaces[0].Name)
			expectedTopologies = append(expectedTopologies, "Layer3")
			v4, v6 = nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnNames[1], cudnLabel, v4, v6, cudnNamespaces[1].Name)
			expectedTopologies = append(expectedTopologies, "Layer3")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, cudnNames[2], cudnLabel, v4, v6, cudnNamespaces[2].Name)
			expectedTopologies = append(expectedTopologies, "Layer2")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, cudnNames[3], cudnLabel, v4, v6, cudnNamespaces[3].Name)
			expectedTopologies = append(expectedTopologies, "Layer2")

			By("creating 4 UDNs (2xL3 + 2xL2)")
			v4, v6 = nextSubnets(true)
			createLayer3PrimaryUDNWithSubnets(cs, udnNamespaces[0].Name, udnNames[0], v4, v6)
			expectedTopologies = append(expectedTopologies, "Layer3")
			v4, v6 = nextSubnets(true)
			createLayer3PrimaryUDNWithSubnets(cs, udnNamespaces[1].Name, udnNames[1], v4, v6)
			expectedTopologies = append(expectedTopologies, "Layer3")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, udnNamespaces[2].Name, udnNames[2], v4, v6)
			expectedTopologies = append(expectedTopologies, "Layer2")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, udnNamespaces[3].Name, udnNames[3], v4, v6)
			expectedTopologies = append(expectedTopologies, "Layer2")

			By("waiting for all networks to be ready")
			for _, name := range cudnNames {
				Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, name), 30*time.Second, time.Second).Should(Succeed())
			}
			for i, ns := range udnNamespaces {
				Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns.Name, udnNames[i]), 30*time.Second, time.Second).Should(Succeed())
			}

			By("verifying CNC has all 8 networks in subnet annotation")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 8)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
		})
	})

	// ===========================================
	// Group 4: Adding Networks - networks added to existing CNC (4 tests)
	// ===========================================
	Context("when networks are added to existing CNC", func() {
		// Adding single network to CNC with existing networks
		DescribeTable("adding a network to CNC with existing networks: count increases",
			func(ctx SpecContext, initialTopology, addedTopology, kind string) {
				cncName := generateCNCName()
				testLabel := map[string]string{fmt.Sprintf("test-add-%s", strings.ToLower(kind)): "true"}
				var namespaces []*corev1.Namespace
				var networkNames []string
				var expectedTopologies []string

				if kind == "UDN" {
					// Create 2 namespaces - one for initial, one for added
					for i := 1; i <= 2; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-add-udn-%d", i), testLabel))
						networkNames = append(networkNames, fmt.Sprintf("udn%d", i))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						for i, ns := range namespaces {
							deleteUDN(ns.Name, networkNames[i])
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By(fmt.Sprintf("creating initial %s primary UDN", initialTopology))
					nextSubnets := newTestNetworkSubnetsAllocator()
					v4, v6 := nextSubnets(initialTopology == "Layer3")
					createPrimaryUDNWithSubnets(cs, namespaces[0].Name, networkNames[0], initialTopology, v4, v6)
					expectedTopologies = append(expectedTopologies, initialTopology)
					Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, namespaces[0].Name, networkNames[0]), 30*time.Second, time.Second).Should(Succeed())

					By("creating CNC with PUDN selector")
					createOrUpdateCNC(cs, cncName, nil, testLabel)

					By("verifying CNC has 1 network in subnet annotation")
					verifyCNCHasBothAnnotations(cncName)
					verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
					verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

					By(fmt.Sprintf("adding a %s primary UDN", addedTopology))
					v4, v6 = nextSubnets(addedTopology == "Layer3")
					createPrimaryUDNWithSubnets(cs, namespaces[1].Name, networkNames[1], addedTopology, v4, v6)
					expectedTopologies = append(expectedTopologies, addedTopology)
					Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, namespaces[1].Name, networkNames[1]), 30*time.Second, time.Second).Should(Succeed())
				} else {
					// CUDN case
					for i := 1; i <= 2; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-add-cudn-ns%d", i), nil))
						networkNames = append(networkNames, fmt.Sprintf("add-cudn-%d-%s", i, rand.String(5)))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						for _, name := range networkNames {
							deleteCUDN(name)
						}
						for _, ns := range namespaces {
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By(fmt.Sprintf("creating initial %s primary CUDN", initialTopology))
					nextSubnets := newTestNetworkSubnetsAllocator()
					v4, v6 := nextSubnets(initialTopology == "Layer3")
					createPrimaryCUDNWithSubnets(cs, networkNames[0], initialTopology, testLabel, v4, v6, namespaces[0].Name)
					expectedTopologies = append(expectedTopologies, initialTopology)
					Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, networkNames[0]), 30*time.Second, time.Second).Should(Succeed())

					By("creating CNC with CUDN selector")
					createOrUpdateCNC(cs, cncName, testLabel, nil)

					By("verifying CNC has 1 network in subnet annotation")
					verifyCNCHasBothAnnotations(cncName)
					verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
					verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

					By(fmt.Sprintf("adding a %s primary CUDN", addedTopology))
					v4, v6 = nextSubnets(addedTopology == "Layer3")
					createPrimaryCUDNWithSubnets(cs, networkNames[1], addedTopology, testLabel, v4, v6, namespaces[1].Name)
					expectedTopologies = append(expectedTopologies, addedTopology)
					Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, networkNames[1]), 30*time.Second, time.Second).Should(Succeed())
				}

				By("verifying CNC now has 2 networks in subnet annotation")
				verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
				verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
			},
			Entry("add L2 P-UDN to L3 P-UDN", "Layer3", "Layer2", "UDN"),
			Entry("add L3 P-UDN to L2 P-UDN", "Layer2", "Layer3", "UDN"),
			Entry("add L2 P-CUDN to L3 P-CUDN", "Layer3", "Layer2", "CUDN"),
			Entry("add L3 P-CUDN to L2 P-CUDN", "Layer2", "Layer3", "CUDN"),
		)

		It("adding mixed networks (P-UDN + P-CUDN) to existing CNC - all networks appear", func(ctx SpecContext) {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-add-mixed": "true"}
			udnLabel := map[string]string{"test-add-mixed": "true"}
			var expectedTopologies []string
			nextSubnets := newTestNetworkSubnetsAllocator()

			// Initial: 1 L3 CUDN + 1 L3 UDN
			initialCudnName := fmt.Sprintf("add-mixed-cudn-init-%s", rand.String(5))
			initialUdnName := "udn-init"
			cudnNs := createUDNNamespace(cs, "test-add-mixed-cudn", nil)
			udnNs := createUDNNamespace(cs, "test-add-mixed-udn", udnLabel)

			// Added: 1 L2 CUDN + 1 L2 UDN
			addedCudnName := fmt.Sprintf("add-mixed-cudn-add-%s", rand.String(5))
			addedUdnName := "udn-add"
			addedCudnNs := createUDNNamespace(cs, "test-add-mixed-cudn2", nil)
			addedUdnNs := createUDNNamespace(cs, "test-add-mixed-udn2", udnLabel)

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(initialCudnName)
				deleteCUDN(addedCudnName)
				deleteUDN(udnNs.Name, initialUdnName)
				deleteUDN(addedUdnNs.Name, addedUdnName)
				for _, ns := range []*corev1.Namespace{cudnNs, udnNs, addedCudnNs, addedUdnNs} {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
			})

			By("creating initial L3 CUDN and L3 UDN")
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, initialCudnName, cudnLabel, v4, v6, cudnNs.Name)
			expectedTopologies = append(expectedTopologies, "Layer3")
			v4, v6 = nextSubnets(true)
			createLayer3PrimaryUDNWithSubnets(cs, udnNs.Name, initialUdnName, v4, v6)
			expectedTopologies = append(expectedTopologies, "Layer3")

			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, initialCudnName), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, udnNs.Name, initialUdnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with both selectors")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("verifying CNC has 2 networks initially")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

			By("adding L2 CUDN and L2 UDN")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, addedCudnName, cudnLabel, v4, v6, addedCudnNs.Name)
			expectedTopologies = append(expectedTopologies, "Layer2")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, addedUdnNs.Name, addedUdnName, v4, v6)
			expectedTopologies = append(expectedTopologies, "Layer2")

			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, addedCudnName), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, addedUdnNs.Name, addedUdnName), 30*time.Second, time.Second).Should(Succeed())

			By("verifying CNC now has 4 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
		})
	})

	// ===========================================
	// Group 5: Network Deletion - networks removed from CNC (4 tests)
	// ===========================================
	Context("when networks are deleted from CNC", func() {
		// Deleting single network from CNC with multiple networks
		DescribeTable("deleting networks from CNC: count decreases to zero",
			func(ctx SpecContext, topology, kind string) {
				cncName := generateCNCName()
				testLabel := map[string]string{fmt.Sprintf("test-del-%s", strings.ToLower(kind)): "true"}
				var namespaces []*corev1.Namespace
				var networkNames []string

				if kind == "UDN" {
					// Create 2 namespaces with 2 networks
					for i := 1; i <= 2; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-del-udn-%d", i), testLabel))
						networkNames = append(networkNames, fmt.Sprintf("udn%d", i))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						// Networks already deleted in test
						for _, ns := range namespaces {
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By("creating 2 primary UDNs (L3 + topology)")
					nextSubnets := newTestNetworkSubnetsAllocator()
					v4, v6 := nextSubnets(true)
					createLayer3PrimaryUDNWithSubnets(cs, namespaces[0].Name, networkNames[0], v4, v6)
					v4, v6 = nextSubnets(topology == "Layer3")
					createPrimaryUDNWithSubnets(cs, namespaces[1].Name, networkNames[1], topology, v4, v6)
					for i, ns := range namespaces {
						Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns.Name, networkNames[i]), 30*time.Second, time.Second).Should(Succeed())
					}

					By("creating CNC with PUDN selector")
					createOrUpdateCNC(cs, cncName, nil, testLabel)

					By("verifying CNC has 2 networks initially")
					verifyCNCHasBothAnnotations(cncName)
					verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
					verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", topology})

					By(fmt.Sprintf("deleting the %s UDN", topology))
					deleteUDN(namespaces[1].Name, networkNames[1])

					By("verifying CNC now has 1 network")
					verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
					verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

					By("deleting the remaining L3 UDN")
					deleteUDN(namespaces[0].Name, networkNames[0])
				} else {
					// CUDN case
					for i := 1; i <= 2; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-del-cudn-ns%d", i), nil))
						networkNames = append(networkNames, fmt.Sprintf("del-cudn-%d-%s", i, rand.String(5)))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						// Networks already deleted in test
						for _, ns := range namespaces {
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By("creating 2 primary CUDNs (L3 + topology)")
					nextSubnets := newTestNetworkSubnetsAllocator()
					v4, v6 := nextSubnets(true)
					createLayer3PrimaryCUDNWithSubnets(cs, networkNames[0], testLabel, v4, v6, namespaces[0].Name)
					v4, v6 = nextSubnets(topology == "Layer3")
					createPrimaryCUDNWithSubnets(cs, networkNames[1], topology, testLabel, v4, v6, namespaces[1].Name)
					for i := 0; i < 2; i++ {
						Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, networkNames[i]), 30*time.Second, time.Second).Should(Succeed())
					}

					By("creating CNC with CUDN selector")
					createOrUpdateCNC(cs, cncName, testLabel, nil)

					By("verifying CNC has 2 networks initially")
					verifyCNCHasBothAnnotations(cncName)
					verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
					verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", topology})

					By(fmt.Sprintf("deleting the %s CUDN", topology))
					deleteCUDN(networkNames[1])

					By("verifying CNC now has 1 network")
					verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
					verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

					By("deleting the remaining L3 CUDN")
					deleteCUDN(networkNames[0])
				}

				By("verifying CNC reverts to only tunnel ID annotation")
				verifyCNCHasOnlyTunnelIDAnnotation(cncName)
			},
			Entry("delete L2 then L3 P-UDN", "Layer2", "UDN"),
			Entry("delete L3 then L3 P-UDN", "Layer3", "UDN"),
			Entry("delete L2 then L3 P-CUDN", "Layer2", "CUDN"),
			Entry("delete L3 then L3 P-CUDN", "Layer3", "CUDN"),
		)

		It("deleting mixed networks (P-UDN + P-CUDN) - annotations update correctly", func(ctx SpecContext) {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-del-mixed": "true"}
			udnLabel := map[string]string{"test-del-mixed": "true"}

			// Create 2 CUDNs + 2 UDNs
			cudnNs1 := createUDNNamespace(cs, "test-del-mixed-cudn1", nil)
			cudnNs2 := createUDNNamespace(cs, "test-del-mixed-cudn2", nil)
			udnNs1 := createUDNNamespace(cs, "test-del-mixed-udn1", udnLabel)
			udnNs2 := createUDNNamespace(cs, "test-del-mixed-udn2", udnLabel)

			cudnName1 := fmt.Sprintf("del-mixed-cudn1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("del-mixed-cudn2-%s", rand.String(5))
			udnName1 := "udn1"
			udnName2 := "udn2"

			DeferCleanup(func() {
				deleteCNC(cncName)
				// Only delete remaining networks (others deleted in test)
				deleteCUDN(cudnName1)
				deleteUDN(udnNs1.Name, udnName1)
				for _, ns := range []*corev1.Namespace{cudnNs1, cudnNs2, udnNs1, udnNs2} {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
			})

			By("creating 2 CUDNs (L3 + L2) and 2 UDNs (L3 + L2)")
			nextSubnets := newTestNetworkSubnetsAllocator()
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnName1, cudnLabel, v4, v6, cudnNs1.Name)
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, cudnName2, cudnLabel, v4, v6, cudnNs2.Name)
			v4, v6 = nextSubnets(true)
			createLayer3PrimaryUDNWithSubnets(cs, udnNs1.Name, udnName1, v4, v6)
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, udnNs2.Name, udnName2, v4, v6)

			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, udnNs1.Name, udnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, udnNs2.Name, udnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with both selectors")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("verifying CNC has 4 networks initially")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2", "Layer3", "Layer2"})

			By("deleting L2 CUDN and L2 UDN")
			deleteCUDN(cudnName2)
			deleteUDN(udnNs2.Name, udnName2)

			By("verifying CNC has 2 L3 networks remaining")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer3"})

			By("deleting L3 CUDN and L3 UDN")
			deleteCUDN(cudnName1)
			deleteUDN(udnNs1.Name, udnName1)

			By("verifying CNC has no networks remaining")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 0)
			verifyCNCSubnetAnnotationContent(cncName, []string{})
		})
	})

	// ===========================================
	// Group 6: CNC Selector Update - CNC spec.networkSelectors changed (4 tests)
	// ===========================================
	Context("when CNC selector is updated", func() {
		It("widening then narrowing CUDN selector - count increases then decreases", func(ctx SpecContext) {
			cncName := generateCNCName()
			commonLabel := map[string]string{"test-cudn-sel": "true"}
			specificLabel := map[string]string{"test-cudn-sel": "true", "specific": "true"}

			ns1 := createUDNNamespace(cs, "test-cudn-sel-ns1", nil)
			ns2 := createUDNNamespace(cs, "test-cudn-sel-ns2", nil)
			cudnName1 := fmt.Sprintf("cudn-sel1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("cudn-sel2-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 CUDNs - both with common label, second also has specific label")
			nextSubnets := newTestNetworkSubnetsAllocator()
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnName1, commonLabel, v4, v6, ns1.Name)
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, cudnName2, specificLabel, v4, v6, ns2.Name)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with specific selector (matches only second CUDN)")
			createOrUpdateCNC(cs, cncName, specificLabel, nil)

			By("verifying CNC has 1 network initially")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})

			By("widening CNC selector to common label - count increases")
			createOrUpdateCNC(cs, cncName, commonLabel, nil)

			By("verifying CNC now has 2 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"})

			By("narrowing CNC selector back to specific - count decreases")
			createOrUpdateCNC(cs, cncName, specificLabel, nil)

			By("verifying CNC now has 1 network")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})
		})

		It("widening then narrowing PUDN namespace selector - count increases then decreases", func(ctx SpecContext) {
			cncName := generateCNCName()
			commonLabel := map[string]string{"test-pudn-sel": "true"}
			specificLabel := map[string]string{"test-pudn-sel": "true", "specific": "true"}

			ns1 := createUDNNamespace(cs, "test-pudn-sel-ns1", commonLabel)
			ns2 := createUDNNamespace(cs, "test-pudn-sel-ns2", specificLabel)
			udnName1 := "udn1"
			udnName2 := "udn2"

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteUDN(ns1.Name, udnName1)
				deleteUDN(ns2.Name, udnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 UDNs in namespaces - both with common label, second also has specific")
			nextSubnets := newTestNetworkSubnetsAllocator()
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryUDNWithSubnets(cs, ns1.Name, udnName1, v4, v6)
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, ns2.Name, udnName2, v4, v6)
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns1.Name, udnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns2.Name, udnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with specific selector (matches only second namespace)")
			createOrUpdateCNC(cs, cncName, nil, specificLabel)

			By("verifying CNC has 1 network initially")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})

			By("widening CNC selector to common label - count increases")
			createOrUpdateCNC(cs, cncName, nil, commonLabel)

			By("verifying CNC now has 2 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"})

			By("narrowing CNC selector back to specific - count decreases")
			createOrUpdateCNC(cs, cncName, nil, specificLabel)

			By("verifying CNC now has 1 network")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})
		})

		It("adding and removing PUDN selector from CNC - count increases then decreases", func(ctx SpecContext) {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-toggle-pudn-sel": "true"}
			udnLabel := map[string]string{"test-toggle-pudn-sel": "true"}

			cudnNs := createUDNNamespace(cs, "test-toggle-pudn-sel-cudn", nil)
			udnNs := createUDNNamespace(cs, "test-toggle-pudn-sel-udn", udnLabel)
			cudnName := fmt.Sprintf("toggle-pudn-sel-cudn-%s", rand.String(5))
			udnName := "udn1"

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName)
				deleteUDN(udnNs.Name, udnName)
				cs.CoreV1().Namespaces().Delete(context.Background(), cudnNs.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), udnNs.Name, metav1.DeleteOptions{})
			})

			By("creating L3 CUDN and L2 UDN")
			nextSubnets := newTestNetworkSubnetsAllocator()
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnName, cudnLabel, v4, v6, cudnNs.Name)
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, udnNs.Name, udnName, v4, v6)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, udnNs.Name, udnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with only CUDN selector")
			createOrUpdateCNC(cs, cncName, cudnLabel, nil)

			By("verifying CNC has 1 network initially (CUDN only)")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

			By("adding PUDN selector to CNC - count increases")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("verifying CNC now has 2 networks (CUDN + PUDN)")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"})

			By("removing PUDN selector from CNC - count decreases")
			createOrUpdateCNC(cs, cncName, cudnLabel, nil)

			By("verifying CNC now has 1 network (CUDN only)")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})
		})

		It("adding and removing CUDN selector from CNC - count increases then decreases", func(ctx SpecContext) {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-toggle-cudn-sel": "true"}
			udnLabel := map[string]string{"test-toggle-cudn-sel": "true"}

			cudnNs := createUDNNamespace(cs, "test-toggle-cudn-sel-cudn", nil)
			udnNs := createUDNNamespace(cs, "test-toggle-cudn-sel-udn", udnLabel)
			cudnName := fmt.Sprintf("toggle-cudn-sel-cudn-%s", rand.String(5))
			udnName := "udn1"

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName)
				deleteUDN(udnNs.Name, udnName)
				cs.CoreV1().Namespaces().Delete(context.Background(), cudnNs.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), udnNs.Name, metav1.DeleteOptions{})
			})

			By("creating L3 CUDN and L2 UDN")
			nextSubnets := newTestNetworkSubnetsAllocator()
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnName, cudnLabel, v4, v6, cudnNs.Name)
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, udnNs.Name, udnName, v4, v6)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, udnNs.Name, udnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with only PUDN selector")
			createOrUpdateCNC(cs, cncName, nil, udnLabel)

			By("verifying CNC has 1 network initially (PUDN only)")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})

			By("adding CUDN selector to CNC - count increases")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("verifying CNC now has 2 networks (CUDN + PUDN)")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"})

			By("removing CUDN selector from CNC - count decreases")
			createOrUpdateCNC(cs, cncName, nil, udnLabel)

			By("verifying CNC now has 1 network (PUDN only)")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})

			By("changing PUDN selector to non-matching label - count decreases to 0")
			createOrUpdateCNC(cs, cncName, nil, map[string]string{"nonexistent": "label"})

			By("verifying CNC has no networks remaining")
			verifyCNCHasOnlyTunnelIDAnnotation(cncName) // No networks match, so subnet annotation is empty
			verifyCNCSubnetAnnotationNetworkCount(cncName, 0)
		})
	})

	// ===========================================
	// Group 7: Label Mutation - network/namespace labels changed (2 tests)
	// ===========================================
	Context("when network or namespace labels are mutated", func() {
		It("CUDN label mutation - adding then removing label changes CNC count", func(ctx SpecContext) {
			cncName := generateCNCName()
			cncLabel := map[string]string{"test-cudn-label": "true"}

			ns1 := createUDNNamespace(cs, "test-cudn-label-ns1", nil)
			ns2 := createUDNNamespace(cs, "test-cudn-label-ns2", nil)
			cudnName1 := fmt.Sprintf("cudn-label1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("cudn-label2-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 CUDNs - first with matching label, second without")
			nextSubnets := newTestNetworkSubnetsAllocator()
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnName1, cncLabel, v4, v6, ns1.Name)
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, cudnName2, map[string]string{"other": "label"}, v4, v6, ns2.Name)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with CUDN selector")
			createOrUpdateCNC(cs, cncName, cncLabel, nil)

			By("verifying CNC has 1 network initially")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

			By("adding matching label to second CUDN - count increases")
			_, err := e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", cudnName2, "test-cudn-label=true")
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC now has 2 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"})

			By("removing matching label from second CUDN - count decreases")
			_, err = e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", cudnName2, "test-cudn-label-")
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC now has 1 network")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

			By("removing matching label from first CUDN - count decreases to 0")
			_, err = e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", cudnName1, "test-cudn-label-")
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC has no networks remaining")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 0)
			verifyCNCSubnetAnnotationContent(cncName, []string{})
		})

		It("namespace label mutation - adding then removing label changes CNC count", func(ctx SpecContext) {
			cncName := generateCNCName()
			cncLabel := map[string]string{"test-ns-label": "true"}

			ns1 := createUDNNamespace(cs, "test-ns-label-ns1", cncLabel)
			ns2 := createUDNNamespace(cs, "test-ns-label-ns2", nil) // no matching label initially
			udnName1 := "udn1"
			udnName2 := "udn2"

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteUDN(ns1.Name, udnName1)
				deleteUDN(ns2.Name, udnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 UDNs - first in namespace with matching label, second without")
			nextSubnets := newTestNetworkSubnetsAllocator()
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryUDNWithSubnets(cs, ns1.Name, udnName1, v4, v6)
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, ns2.Name, udnName2, v4, v6)
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns1.Name, udnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns2.Name, udnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with PUDN namespace selector")
			createOrUpdateCNC(cs, cncName, nil, cncLabel)

			By("verifying CNC has 1 network initially")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

			By("adding matching label to second namespace - count increases")
			_, err := cs.CoreV1().Namespaces().Patch(context.Background(), ns2.Name,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"test-ns-label":"true"}}}`),
				metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC now has 2 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"})

			By("removing matching label from second namespace - count decreases")
			_, err = cs.CoreV1().Namespaces().Patch(context.Background(), ns2.Name,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"test-ns-label":null}}}`),
				metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC now has 1 network")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

			By("removing matching label from first namespace - count decreases to 0")
			_, err = cs.CoreV1().Namespaces().Patch(context.Background(), ns1.Name,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"test-ns-label":null}}}`),
				metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC has no networks remaining")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 0)
			verifyCNCSubnetAnnotationContent(cncName, []string{})
		})
	})

	// ===========================================
	// Group 8: Multiple CNCs - multiple CNCs in cluster (3 tests)
	// ===========================================
	Context("when multiple CNCs exist", func() {
		// Second CNC connect subnet configuration (must be different from first CNC)
		const (
			cnc2ConnectSubnetIPv4CIDR   = "192.169.0.0/16"
			cnc2ConnectSubnetIPv4Prefix = 24
			cnc2ConnectSubnetIPv6CIDR   = "fd00:11::/112"
			cnc2ConnectSubnetIPv6Prefix = 120
		)

		It("two CNCs with non-overlapping selectors - each tracks its own networks", func(ctx SpecContext) {
			cncName1 := generateCNCName()
			cncName2 := generateCNCName()
			label1 := map[string]string{"test-multi-cnc-1": "true"}
			label2 := map[string]string{"test-multi-cnc-2": "true"}
			nextSubnets := newTestNetworkSubnetsAllocator()

			ns1 := createUDNNamespace(cs, "test-multi-cnc-ns1", nil)
			ns2 := createUDNNamespace(cs, "test-multi-cnc-ns2", nil)
			cudnName1 := fmt.Sprintf("multi-cnc-cudn1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("multi-cnc-cudn2-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName1)
				deleteCNC(cncName2)
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 CUDNs with different labels")
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnName1, label1, v4, v6, ns1.Name)
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, cudnName2, label2, v4, v6, ns2.Name)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating first CNC matching first CUDN (with first connect subnet)")
			createOrUpdateCNCWithSubnets(cncName1, label1, nil, generateConnectSubnets(cs))

			By("creating second CNC matching second CUDN (with different connect subnet)")
			createOrUpdateCNCWithSubnets(cncName2, label2, nil, generateConnectSubnetsWithCIDRs(cs, cnc2ConnectSubnetIPv4CIDR, cnc2ConnectSubnetIPv4Prefix, cnc2ConnectSubnetIPv6CIDR, cnc2ConnectSubnetIPv6Prefix))

			By("verifying first CNC has only first network")
			verifyCNCHasBothAnnotations(cncName1)
			verifyCNCSubnetAnnotationNetworkCount(cncName1, 1)
			verifyCNCSubnetAnnotationContent(cncName1, []string{"Layer3"})

			By("verifying second CNC has only second network")
			verifyCNCHasBothAnnotations(cncName2)
			verifyCNCSubnetAnnotationNetworkCount(cncName2, 1)
			verifyCNCSubnetAnnotationContent(cncName2, []string{"Layer2"})

			By("verifying CNCs have different tunnel IDs")
			annotations1, err := getCNCAnnotations(cncName1)
			Expect(err).NotTo(HaveOccurred())
			annotations2, err := getCNCAnnotations(cncName2)
			Expect(err).NotTo(HaveOccurred())
			Expect(annotations1[ovnConnectRouterTunnelKeyAnnotation]).NotTo(Equal(annotations2[ovnConnectRouterTunnelKeyAnnotation]),
				"CNCs should have different tunnel IDs")
		})

		It("two CNCs matching same network - both track the network (this works but is usually treated as misconfiguration)", func(ctx SpecContext) {
			cncName1 := generateCNCName()
			cncName2 := generateCNCName()
			sharedLabel := map[string]string{"test-shared-cudn": "true"}
			nextSubnets := newTestNetworkSubnetsAllocator()

			ns := createUDNNamespace(cs, "test-shared-cudn-ns", nil)
			cudnName := fmt.Sprintf("shared-cudn-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName1)
				deleteCNC(cncName2)
				deleteCUDN(cudnName)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
			})

			By("creating a CUDN with shared label")
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnName, sharedLabel, v4, v6, ns.Name)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating first CNC matching the CUDN (with first connect subnet)")
			createOrUpdateCNCWithSubnets(cncName1, sharedLabel, nil, generateConnectSubnets(cs))

			By("creating second CNC also matching the CUDN (with different connect subnet)")
			createOrUpdateCNCWithSubnets(cncName2, sharedLabel, nil, generateConnectSubnetsWithCIDRs(cs, cnc2ConnectSubnetIPv4CIDR, cnc2ConnectSubnetIPv4Prefix, cnc2ConnectSubnetIPv6CIDR, cnc2ConnectSubnetIPv6Prefix))

			By("verifying both CNCs have the network in their annotations")
			verifyCNCHasBothAnnotations(cncName1)
			verifyCNCSubnetAnnotationNetworkCount(cncName1, 1)
			verifyCNCSubnetAnnotationContent(cncName1, []string{"Layer3"})

			verifyCNCHasBothAnnotations(cncName2)
			verifyCNCSubnetAnnotationNetworkCount(cncName2, 1)
			verifyCNCSubnetAnnotationContent(cncName2, []string{"Layer3"})

			By("verifying CNCs have different tunnel IDs")
			annotations1, err := getCNCAnnotations(cncName1)
			Expect(err).NotTo(HaveOccurred())
			annotations2, err := getCNCAnnotations(cncName2)
			Expect(err).NotTo(HaveOccurred())
			Expect(annotations1[ovnConnectRouterTunnelKeyAnnotation]).NotTo(Equal(annotations2[ovnConnectRouterTunnelKeyAnnotation]),
				"CNCs should have different tunnel IDs")
		})

		It("deleting one CNC does not affect the other", func(ctx SpecContext) {
			cncName1 := generateCNCName()
			cncName2 := generateCNCName()
			label1 := map[string]string{"test-cnc-delete-1": "true"}
			label2 := map[string]string{"test-cnc-delete-2": "true"}
			nextSubnets := newTestNetworkSubnetsAllocator()

			ns1 := createUDNNamespace(cs, "test-cnc-delete-ns1", nil)
			ns2 := createUDNNamespace(cs, "test-cnc-delete-ns2", nil)
			cudnName1 := fmt.Sprintf("cnc-delete-cudn1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("cnc-delete-cudn2-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName2) // cncName1 deleted in test
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 CUDNs with different labels")
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnName1, label1, v4, v6, ns1.Name)
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, cudnName2, label2, v4, v6, ns2.Name)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating two CNCs with different selectors and different connect subnets")
			createOrUpdateCNCWithSubnets(cncName1, label1, nil, generateConnectSubnets(cs))
			createOrUpdateCNCWithSubnets(cncName2, label2, nil, generateConnectSubnetsWithCIDRs(cs, cnc2ConnectSubnetIPv4CIDR, cnc2ConnectSubnetIPv4Prefix, cnc2ConnectSubnetIPv6CIDR, cnc2ConnectSubnetIPv6Prefix))

			By("verifying both CNCs have their networks")
			verifyCNCHasBothAnnotations(cncName1)
			verifyCNCSubnetAnnotationNetworkCount(cncName1, 1)
			verifyCNCHasBothAnnotations(cncName2)
			verifyCNCSubnetAnnotationNetworkCount(cncName2, 1)

			By("deleting first CNC")
			deleteCNC(cncName1)

			By("verifying second CNC is unaffected")
			verifyCNCHasBothAnnotations(cncName2)
			verifyCNCSubnetAnnotationNetworkCount(cncName2, 1)
			verifyCNCSubnetAnnotationContent(cncName2, []string{"Layer2"})
		})
	})

	// ===========================================
	// Group 9: CNC Lifecycle - CNC deletion and recreation
	// ===========================================
	Context("CNC lifecycle", func() {
		It("CNC deletion and recreation - tunnel ID is allocated after recreate", func(ctx SpecContext) {
			cncName := generateCNCName()
			cncLabel := map[string]string{"test-cnc-lifecycle": "true"}
			nextSubnets := newTestNetworkSubnetsAllocator()

			ns := createUDNNamespace(cs, "test-cnc-lifecycle-ns", nil)
			cudnName := fmt.Sprintf("cnc-lifecycle-cudn-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
			})

			By("creating a CUDN")
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnName, cncLabel, v4, v6, ns.Name)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC")
			createOrUpdateCNC(cs, cncName, cncLabel, nil)

			By("verifying CNC has network and tunnel ID")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			originalTunnelID := getCNCTunnelID(cncName)
			Expect(originalTunnelID).NotTo(BeEmpty())

			By("deleting CNC")
			deleteCNC(cncName)

			By("verifying CNC is gone")
			Eventually(func() bool {
				_, err := getCNCAnnotations(cncName)
				return err != nil
			}, 30*time.Second, time.Second).Should(BeTrue())

			By("recreating CNC with same name")
			createOrUpdateCNC(cs, cncName, cncLabel, nil)

			By("verifying CNC has network again")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)

			By("verifying tunnel ID is newly allocated after CNC recreation")
			newTunnelID := getCNCTunnelID(cncName)
			Expect(newTunnelID).NotTo(BeEmpty())
			Expect(newTunnelID).NotTo(Equal(originalTunnelID))
		})

		It("tunnel ID is stable across CNC spec updates", func(ctx SpecContext) {
			cncName := generateCNCName()
			label1 := map[string]string{"test-tunnel-stable-1": "true"}
			label2 := map[string]string{"test-tunnel-stable-2": "true"}
			nextSubnets := newTestNetworkSubnetsAllocator()

			ns1 := createUDNNamespace(cs, "test-tunnel-stable-ns1", nil)
			ns2 := createUDNNamespace(cs, "test-tunnel-stable-ns2", nil)
			cudnName1 := fmt.Sprintf("tunnel-stable-cudn1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("tunnel-stable-cudn2-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 CUDNs with different labels")
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnName1, label1, v4, v6, ns1.Name)
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, cudnName2, label2, v4, v6, ns2.Name)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC matching first CUDN")
			createOrUpdateCNC(cs, cncName, label1, nil)

			By("verifying CNC has network and recording tunnel ID")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			originalTunnelID := getCNCTunnelID(cncName)

			By("updating CNC to match second CUDN instead")
			createOrUpdateCNC(cs, cncName, label2, nil)

			By("verifying CNC now has second network")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})

			By("verifying tunnel ID is unchanged")
			newTunnelID := getCNCTunnelID(cncName)
			Expect(newTunnelID).To(Equal(originalTunnelID),
				"tunnel ID should be stable across spec updates")

			By("updating CNC to match both CUDNs")
			// Add label1 to second CUDN so we can match both
			_, err := e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", cudnName2, "test-tunnel-stable-1=true")
			Expect(err).NotTo(HaveOccurred())
			createOrUpdateCNC(cs, cncName, label1, nil)

			By("verifying CNC now has both networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)

			By("verifying tunnel ID is still unchanged")
			finalTunnelID := getCNCTunnelID(cncName)
			Expect(finalTunnelID).To(Equal(originalTunnelID),
				"tunnel ID should remain stable")
		})

	})

	// ===========================================
	// Group 10: Full Lifecycle Workflow (1 comprehensive test)
	// ===========================================
	Context("full lifecycle workflow", func() {
		It("comprehensive workflow - create, add, update, remove networks through CNC lifecycle", func(ctx SpecContext) {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-lifecycle": "true"}
			udnLabel := map[string]string{"test-lifecycle": "true"}
			var expectedTopologies []string
			nextSubnets := newTestNetworkSubnetsAllocator()

			// Create namespaces
			cudnNs1 := createUDNNamespace(cs, "lifecycle-cudn-ns1", nil)
			cudnNs2 := createUDNNamespace(cs, "lifecycle-cudn-ns2", nil)
			udnNs1 := createUDNNamespace(cs, "lifecycle-udn-ns1", udnLabel)
			udnNs2 := createUDNNamespace(cs, "lifecycle-udn-ns2", udnLabel)

			cudnName1 := fmt.Sprintf("lifecycle-cudn1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("lifecycle-cudn2-%s", rand.String(5))
			udnName1 := "udn1"
			udnName2 := "udn2"

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				deleteUDN(udnNs1.Name, udnName1)
				deleteUDN(udnNs2.Name, udnName2)
				for _, ns := range []*corev1.Namespace{cudnNs1, cudnNs2, udnNs1, udnNs2} {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
			})

			// Phase 1: Create CNC with no matching networks
			By("Phase 1: Creating CNC with no matching networks yet")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)
			verifyCNCHasOnlyTunnelIDAnnotation(cncName)
			originalTunnelID := getCNCTunnelID(cncName)

			// Phase 2: Create first L3 CUDN - count goes to 1
			By("Phase 2: Creating first L3 CUDN")
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, cudnName1, cudnLabel, v4, v6, cudnNs1.Name)
			expectedTopologies = append(expectedTopologies, "Layer3")
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

			// Phase 3: Create first L2 UDN - count goes to 2
			By("Phase 3: Creating first L2 UDN")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, udnNs1.Name, udnName1, v4, v6)
			expectedTopologies = append(expectedTopologies, "Layer2")
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, udnNs1.Name, udnName1), 30*time.Second, time.Second).Should(Succeed())
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

			// Phase 4: Create second L2 CUDN - count goes to 3
			By("Phase 4: Creating second L2 CUDN")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, cudnName2, cudnLabel, v4, v6, cudnNs2.Name)
			expectedTopologies = append(expectedTopologies, "Layer2")
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

			// Phase 5: Create second L3 UDN - count goes to 4
			By("Phase 5: Creating second L3 UDN")
			v4, v6 = nextSubnets(true)
			createLayer3PrimaryUDNWithSubnets(cs, udnNs2.Name, udnName2, v4, v6)
			expectedTopologies = append(expectedTopologies, "Layer3")
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, udnNs2.Name, udnName2), 30*time.Second, time.Second).Should(Succeed())
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

			// Verify tunnel ID is stable
			By("Verifying tunnel ID unchanged after adding networks")
			Expect(getCNCTunnelID(cncName)).To(Equal(originalTunnelID))

			// Phase 6: Remove PUDN selector - count goes to 2 (only CUDNs remain)
			By("Phase 6: Removing PUDN selector from CNC")
			createOrUpdateCNC(cs, cncName, cudnLabel, nil)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"}) // cudnName1 is L3, cudnName2 is L2

			// Verify tunnel ID is stable
			By("Verifying tunnel ID unchanged after selector update")
			Expect(getCNCTunnelID(cncName)).To(Equal(originalTunnelID))

			// Phase 7: Delete one CUDN - count goes to 1
			By("Phase 7: Deleting first CUDN")
			deleteCUDN(cudnName1)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"}) // only cudnName2 remains

			// Phase 8: Add PUDN selector back - count goes to 3 (1 CUDN + 2 UDNs)
			By("Phase 8: Adding PUDN selector back to CNC")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2", "Layer2", "Layer3"}) // cudnName2(L2), udn1(L2), udn2(L3)

			// Verify tunnel ID is stable
			By("Verifying tunnel ID unchanged after adding selector back")
			Expect(getCNCTunnelID(cncName)).To(Equal(originalTunnelID))

			// Phase 9: Remove label from namespace - UDN1 no longer matches - count goes to 2
			By("Phase 9: Removing label from first UDN namespace")
			_, err := cs.CoreV1().Namespaces().Patch(context.Background(), udnNs1.Name,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"test-lifecycle":null}}}`),
				metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2", "Layer3"}) // cudnName2(L2), udn2(L3)

			// Phase 10: Delete remaining networks - count goes to 0
			By("Phase 10: Deleting remaining networks")
			deleteCUDN(cudnName2)
			deleteUDN(udnNs2.Name, udnName2)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 0)
			verifyCNCSubnetAnnotationContent(cncName, []string{})

			// Verify tunnel ID is stable even with no networks
			By("Verifying tunnel ID unchanged even with no networks")
			Expect(getCNCTunnelID(cncName)).To(Equal(originalTunnelID))

			// Final verification: CNC still exists with only tunnel ID
			By("Final: Verifying CNC has only tunnel ID annotation")
			verifyCNCHasOnlyTunnelIDAnnotation(cncName)

			By("Deleting CNC")
			deleteCNC(cncName)
		})
	})

	It("reports error on selected networks subnet overlap", func(ctx SpecContext) {
		cncName := generateCNCName()
		testLabel := map[string]string{"test-udn": "true"}

		topology := "Layer3"
		udnName1 := fmt.Sprintf("test-udn-a-%s", rand.String(5))
		udnName2 := fmt.Sprintf("test-udn-b-%s", rand.String(5))
		// Use the same subnet(s) for both UDNs to force overlap across selected networks.
		overlapV4Subnet := "10.250.0.0/16"
		overlapV6Subnet := "fd98:1:1::/60"

		ns1 := createUDNNamespace(cs, "test-udn-a", testLabel)
		ns2 := createUDNNamespace(cs, "test-udn-b", testLabel)
		DeferCleanup(func() {
			deleteCNC(cncName)
			deleteUDN(ns1.Name, udnName1)
			deleteUDN(ns2.Name, udnName2)
			deleteNamespace(cs, ns1.Name)
			deleteNamespace(cs, ns2.Name)
		})

		By(fmt.Sprintf("creating 2 %s primary UDNs with overlapping subnets", topology))
		createPrimaryUDNWithSubnets(cs, ns1.Name, udnName1, topology, subnetList(overlapV4Subnet), subnetList(overlapV6Subnet))
		createPrimaryUDNWithSubnets(cs, ns2.Name, udnName2, topology, subnetList(overlapV4Subnet), subnetList(overlapV6Subnet))

		By("waiting for UDNs to be ready")
		Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns1.Name, udnName1), 30*time.Second, time.Second).Should(Succeed())
		Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, ns2.Name, udnName2), 30*time.Second, time.Second).Should(Succeed())

		By("creating a CNC with PUDN selector matching both namespaces")
		createOrUpdateCNC(cs, cncName, nil, testLabel)

		By("verifying CNC status reports selected networks subnet overlap")
		verifyCNCAcceptedConditionFailed(cncName, "selected networks have overlapping subnets")
	})

})

// ============================================================================
// CNC Annotation ValidatingAdmissionPolicy Testing
// ============================================================================
var _ = Describe("ClusterNetworkConnect ValidatingAdmissionPolicy", feature.NetworkConnect, func() {
	f := wrappedTestFramework("cnc-vap")
	f.SkipNamespaceCreation = true

	var (
		cs clientset.Interface
	)

	BeforeEach(func() {
		cs = f.ClientSet
	})

	It("should reject CREATE with managed annotations pre-set", func() {
		cncName := generateCNCName()
		noMatchSelector := map[string]string{"nonexistent": "label"}
		connectSubnets := generateConnectSubnets(cs)

		By("attempting to create CNC with network-connect-subnet annotation")
		manifest := generateCNCManifest(
			cncName, noMatchSelector, nil, connectSubnets, []string{"PodNetwork"},
			map[string]string{ovnNetworkConnectSubnetAnnotation: `{"test": {"ipv4": "1.2.3.4/24"}}`},
		)
		_, err := e2ekubectl.RunKubectlInput("", manifest, "create", "-f", "-")
		Expect(err).To(HaveOccurred(), "CREATE should be rejected when network-connect-subnet annotation is present")
		Expect(err).To(MatchError(ContainSubstring(
			`ClusterNetworkConnect resources cannot be created with the` +
				` "k8s.ovn.org/network-connect-subnet" or "k8s.ovn.org/connect-router-tunnel-key"` +
				` annotations. These annotations are managed by the system.`)))

		By("attempting to create CNC with connect-router-tunnel-key annotation")
		manifest = generateCNCManifest(
			cncName, noMatchSelector, nil, connectSubnets, []string{"PodNetwork"},
			map[string]string{ovnConnectRouterTunnelKeyAnnotation: "42"},
		)
		_, err = e2ekubectl.RunKubectlInput("", manifest, "create", "-f", "-")
		Expect(err).To(HaveOccurred(), "CREATE should be rejected when connect-router-tunnel-key annotation is present")
		Expect(err).To(MatchError(ContainSubstring(
			`ClusterNetworkConnect resources cannot be created with the` +
				` "k8s.ovn.org/network-connect-subnet" or "k8s.ovn.org/connect-router-tunnel-key"` +
				` annotations. These annotations are managed by the system.`)))

		By("attempting to create CNC with both annotations")
		manifest = generateCNCManifest(
			cncName, noMatchSelector, nil, connectSubnets, []string{"PodNetwork"},
			map[string]string{
				ovnNetworkConnectSubnetAnnotation:   `{"test": {"ipv4": "1.2.3.4/24"}}`,
				ovnConnectRouterTunnelKeyAnnotation: "42",
			},
		)
		_, err = e2ekubectl.RunKubectlInput("", manifest, "create", "-f", "-")
		Expect(err).To(HaveOccurred(), "CREATE should be rejected when both managed annotations are present")
		Expect(err).To(MatchError(ContainSubstring(
			`ClusterNetworkConnect resources cannot be created with the` +
				` "k8s.ovn.org/network-connect-subnet" or "k8s.ovn.org/connect-router-tunnel-key"` +
				` annotations. These annotations are managed by the system.`)))
	})

	It("should allow CREATE with unmanaged annotations", func() {
		cncName := generateCNCName()
		DeferCleanup(func() {
			deleteCNC(cncName)
		})

		By("creating CNC with an unmanaged annotation")
		manifest := generateCNCManifest(
			cncName, map[string]string{"nonexistent": "label"}, nil,
			generateConnectSubnets(cs), []string{"PodNetwork"},
			map[string]string{"custom-annotation": "some-value"},
		)
		_, err := e2ekubectl.RunKubectlInput("", manifest, "create", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "CREATE should succeed with unmanaged annotations")
	})

	It("should reject regular admin adding network-connect-subnet annotation", func() {
		cncName := generateCNCName()
		DeferCleanup(func() {
			deleteCNC(cncName)
		})

		By("creating a CNC with no matching networks")
		createOrUpdateCNC(cs, cncName, map[string]string{"nonexistent": "label"}, nil)

		By("waiting for cluster-manager to set tunnel-key annotation")
		verifyCNCHasOnlyTunnelIDAnnotation(cncName)

		By("attempting to add network-connect-subnet annotation as regular admin")
		_, err := e2ekubectl.RunKubectl("", "annotate", "--overwrite", "clusternetworkconnect", cncName,
			fmt.Sprintf("%s={\"test\":{\"ipv4\":\"1.2.3.4/24\"}}", ovnNetworkConnectSubnetAnnotation))
		Expect(err).To(HaveOccurred(), "regular admin should not be able to add network-connect-subnet annotation")
		Expect(err).To(MatchError(ContainSubstring(
			`A regular user must not add, modify, or remove the` +
				` "k8s.ovn.org/network-connect-subnet" annotation on a` +
				` ClusterNetworkConnect custom resource.`)))
	})

	// Note: Rule 2 also prohibits non-CM users from *adding* connect-router-tunnel-key,
	// but the cluster-manager sets it immediately after CNC creation so there is no
	// reliable e2e window to test that branch. Rule 1 (CREATE rejection) and the
	// modify/remove tests below provide sufficient coverage.
	It("should reject modifying or removing connect-router-tunnel-key once set", func(ctx SpecContext) {
		cncName := generateCNCName()
		cncLabel := map[string]string{"test-vap-tunnel-key": "true"}
		nextSubnets := newTestNetworkSubnetsAllocator()

		ns := createUDNNamespace(cs, "test-vap-tunnel-ns", nil)
		cudnName := fmt.Sprintf("vap-tunnel-cudn-%s", rand.String(5))

		DeferCleanup(func() {
			deleteCNC(cncName)
			deleteCUDN(cudnName)
			cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
		})

		By("creating a CUDN and CNC so cluster-manager sets both annotations")
		v4, v6 := nextSubnets(true)
		createLayer3PrimaryCUDNWithSubnets(cs, cudnName, cncLabel, v4, v6, ns.Name)
		Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())

		createOrUpdateCNC(cs, cncName, cncLabel, nil)
		verifyCNCHasBothAnnotations(cncName)

		By("reading current tunnel-key value")
		tunnelID := getCNCTunnelID(cncName)
		Expect(tunnelID).NotTo(BeEmpty())

		newValue := "99999"
		if tunnelID == newValue {
			newValue = "99998"
		}

		By("attempting to modify connect-router-tunnel-key annotation")
		_, err := e2ekubectl.RunKubectl("", "annotate", "--overwrite", "clusternetworkconnect", cncName,
			fmt.Sprintf("%s=%s", ovnConnectRouterTunnelKeyAnnotation, newValue))
		Expect(err).To(HaveOccurred(), "connect-router-tunnel-key should be immutable once set")
		Expect(err).To(MatchError(ContainSubstring(
			`The "k8s.ovn.org/connect-router-tunnel-key" annotation is immutable` +
				` once set and can only be added by the cluster-manager service account.`)))

		By("attempting to remove connect-router-tunnel-key annotation")
		_, err = e2ekubectl.RunKubectl("", "annotate", "--overwrite", "clusternetworkconnect", cncName,
			fmt.Sprintf("%s-", ovnConnectRouterTunnelKeyAnnotation))
		Expect(err).To(HaveOccurred(), "connect-router-tunnel-key should not be removable once set")
		Expect(err).To(MatchError(ContainSubstring(
			`The "k8s.ovn.org/connect-router-tunnel-key" annotation is immutable` +
				` once set and can only be added by the cluster-manager service account.`)))
	})

	It("should reject regular user modifying or removing network-connect-subnet annotation", func(ctx SpecContext) {
		cncName := generateCNCName()
		cncLabel := map[string]string{"test-vap-subnet": "true"}
		nextSubnets := newTestNetworkSubnetsAllocator()

		ns := createUDNNamespace(cs, "test-vap-subnet-ns", nil)
		cudnName := fmt.Sprintf("vap-subnet-cudn-%s", rand.String(5))

		DeferCleanup(func() {
			deleteCNC(cncName)
			deleteCUDN(cudnName)
			cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
		})

		By("creating a CUDN and CNC so cluster-manager sets both annotations")
		v4, v6 := nextSubnets(true)
		createLayer3PrimaryCUDNWithSubnets(cs, cudnName, cncLabel, v4, v6, ns.Name)
		Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())

		createOrUpdateCNC(cs, cncName, cncLabel, nil)
		verifyCNCHasBothAnnotations(cncName)

		By("attempting to modify network-connect-subnet annotation")
		_, err := e2ekubectl.RunKubectl("", "annotate", "--overwrite", "clusternetworkconnect", cncName,
			fmt.Sprintf(`%s={"bogus":{"ipv4":"9.9.9.0/24"}}`, ovnNetworkConnectSubnetAnnotation))
		Expect(err).To(HaveOccurred(), "regular user should not be able to modify network-connect-subnet")
		Expect(err).To(MatchError(ContainSubstring(
			`A regular user must not add, modify, or remove the` +
				` "k8s.ovn.org/network-connect-subnet" annotation on a` +
				` ClusterNetworkConnect custom resource.`)))

		By("attempting to remove network-connect-subnet annotation")
		_, err = e2ekubectl.RunKubectl("", "annotate", "--overwrite", "clusternetworkconnect", cncName,
			fmt.Sprintf("%s-", ovnNetworkConnectSubnetAnnotation))
		Expect(err).To(HaveOccurred(), "regular user should not be able to remove network-connect-subnet")
		Expect(err).To(MatchError(ContainSubstring(
			`A regular user must not add, modify, or remove the` +
				` "k8s.ovn.org/network-connect-subnet" annotation on a` +
				` ClusterNetworkConnect custom resource.`)))
	})
})

// ============================================================================
// OVN Database Side Testing - End-to-End Connectivity Validation
// ============================================================================
var _ = Describe("ClusterNetworkConnect OVN-Kubernetes Controller", feature.NetworkConnect, func() {
	f := wrappedTestFramework("cnc-ovndb")
	// disable automatic namespace creation, we need to add the required UDN label
	f.SkipNamespaceCreation = true

	var (
		cs clientset.Interface
	)

	BeforeEach(func() {
		cs = f.ClientSet
	})

	// httpServerPodConfig returns a podConfiguration for an HTTP+UDP server pod
	httpServerPodConfig := func(podName, namespace string) podConfiguration {
		cfg := *podConfig(podName, withCommand(func() []string {
			return []string{"netexec", "--http-port", "8080", "--udp-port", "9090"}
		}))
		cfg.namespace = namespace
		return cfg
	}

	// getPrimaryNetworkPodIPs gets the pod IPs for supported IP families on a given primary network
	// Returns a slice of IP strings based on cluster's supported IP families
	// Uses Eventually to wait for OVN annotations to be populated
	getPrimaryNetworkPodIPs := func(namespace, podName, networkName string) []string {
		var ips []string
		Eventually(func() error {
			ips = make([]string, 0, 2)
			for _, family := range getSupportedIPFamiliesSlice(cs) {
				ip, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(cs, namespace, podName, networkName, family)
				if err != nil {
					return err
				}
				if ip != "" {
					ips = append(ips, ip)
				}
			}
			if len(ips) == 0 {
				return fmt.Errorf("pod %s/%s has no IPs on network %s yet", namespace, podName, networkName)
			}
			return nil
		}, 2*time.Minute, 5*time.Second).Should(Succeed(),
			fmt.Sprintf("waiting for pod %s/%s to have IPs on network %s", namespace, podName, networkName))
		return ips
	}

	// checkConnectivity checks connectivity from one pod to another
	// Returns true if connection succeeds/fails as expected
	checkConnectivity := func(fromNamespace, fromPodName, toIP string, expectSuccess bool) bool {
		// net.JoinHostPort properly handles IPv6 addresses by adding brackets
		url := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(toIP, "8080"))
		// --connect-timeout only covers the TCP connection phase.
		// --max-time caps total request time including data transfer, ensuring
		// curl doesn't hang indefinitely if connection succeeds but data never arrives.
		stdout, err := e2ekubectl.RunKubectl(fromNamespace, "exec", fromPodName, "--",
			"curl", "--connect-timeout", "0.5", "--max-time", "1", "-s", "-o", "/dev/null", "-w", "%{http_code}", url)
		if expectSuccess {
			return err == nil && stdout == "200"
		}
		return err != nil || stdout != "200"
	}

	// checkConnectivityFailureType checks connectivity and returns the failure type
	// Returns: "success", "timeout" (no route), "reject" (connection refused), or "other"
	// - timeout: no route exists (network deleted/disconnected)
	// - reject: route exists but no backends (endpoints deleted, OVN LB returns reject)
	checkConnectivityFailureType := func(fromNamespace, fromPodName, toIP string) string {
		url := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(toIP, "8080"))
		stdout, err := e2ekubectl.RunKubectl(fromNamespace, "exec", fromPodName, "--",
			"curl", "--connect-timeout", "0.5", "--max-time", "1", "-s", "-o", "/dev/null", "-w", "%{http_code}:%{exitcode}", url)
		if err == nil && strings.HasPrefix(stdout, "200:") {
			return "success"
		}
		// Parse exit code from output (format: "httpcode:exitcode")
		if strings.Contains(stdout, ":28") || strings.Contains(stdout, ":7") {
			// Exit 28 = timeout, Exit 7 = connection refused
			if strings.Contains(stdout, ":28") {
				return "timeout"
			}
			return "reject"
		}
		// Check error message for common patterns
		if err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "timed out") || strings.Contains(errStr, "timeout") {
				return "timeout"
			}
			if strings.Contains(errStr, "refused") || strings.Contains(errStr, "reset") {
				return "reject"
			}
		}
		return "other"
	}

	// checkPingConnectivity checks ICMP reachability from one pod to an IP.
	// It uses ping6 for IPv6 destinations.
	checkPingConnectivity := func(fromNamespace, fromPodName, toIP string) error {
		pingCmd := "ping"
		if ip := net.ParseIP(toIP); ip != nil && ip.To4() == nil {
			pingCmd = "ping6"
		}
		_, err := e2ekubectl.RunKubectl(fromNamespace, "exec", fromPodName, "--",
			pingCmd, "-c", "3", "-W", "2", toIP)
		return err
	}

	// verifyCrossNetworkConnectivity verifies connectivity from a set of pods to another set
	// Supports dual-stack by testing all IPs for each pod
	// Uses Eventually for expected success
	// For expected failure: first waits for connectivity to fail (Eventually), then verifies it stays failed (Consistently)
	verifyCrossNetworkConnectivity := func(fromPods map[string]*corev1.Pod, toPodIPs map[string][]string, expectSuccess bool) {
		for fromName, fromPod := range fromPods {
			for toName, toIPs := range toPodIPs {
				for _, toIP := range toIPs {
					msg := fmt.Sprintf("cross-network connectivity from %s to %s (%s) expectSuccess=%v", fromName, toName, toIP, expectSuccess)
					if expectSuccess {
						Eventually(func() bool {
							return checkConnectivity(fromPod.Namespace, fromPod.Name, toIP, true)
						}, 5*time.Second, 1*time.Second).Should(BeTrue(), msg)
					} else {
						// First wait for connectivity to fail (OVN flows take time to update)
						Eventually(func() bool {
							return checkConnectivity(fromPod.Namespace, fromPod.Name, toIP, false)
						}, 5*time.Second, 1*time.Second).Should(BeTrue(), msg+" (waiting for failure)")
						// Then verify it stays failed consistently
						Consistently(func() bool {
							return checkConnectivity(fromPod.Namespace, fromPod.Name, toIP, false)
						}, 3*time.Second, 1*time.Second).Should(BeTrue(), msg+" (consistent failure)")
					}
				}
			}
		}
	}

	// verifyFullMeshConnectivity verifies connectivity from source pods to all target pod IPs
	// Uses Eventually for expected success, Eventually+Consistently for expected failure
	verifyFullMeshConnectivity := func(srcPods map[string]*corev1.Pod, dstPodIPs map[string][]string, expectSuccess bool) {
		for fromName, fromPod := range srcPods {
			for toName, toIPs := range dstPodIPs {
				if fromName == toName {
					continue // Skip self-connectivity
				}
				for _, toIP := range toIPs {
					msg := fmt.Sprintf("connectivity from %s to %s (%s) expectSuccess=%v", fromName, toName, toIP, expectSuccess)
					if expectSuccess {
						Eventually(func() bool {
							return checkConnectivity(fromPod.Namespace, fromPod.Name, toIP, true)
						}, 5*time.Second, 1*time.Second).Should(BeTrue(), msg)
					} else {
						// First wait for connectivity to fail (OVN flows take time to update)
						Eventually(func() bool {
							return checkConnectivity(fromPod.Namespace, fromPod.Name, toIP, false)
						}, 5*time.Second, 1*time.Second).Should(BeTrue(), msg+" (waiting for failure)")
						// Then verify it stays failed consistently
						Consistently(func() bool {
							return checkConnectivity(fromPod.Namespace, fromPod.Name, toIP, false)
						}, 3*time.Second, 1*time.Second).Should(BeTrue(), msg+" (consistent failure)")
					}
				}
			}
		}
	}

	/*
	   This test validates end-to-end connectivity through CNC (ClusterNetworkConnect).

	   Test Scenario:
	   - Create 2 CUDNs: "black" (L3) and "white" (L2), each serving 2 namespaces with pods on different nodes
	   - Create 2 UDNs: "blue" (L3) and "green" (L2), each in its own namespace with 2 pods on different nodes
	   - Initially verify pods cannot communicate across different networks
	   - Create CNC selecting all 4 networks, verify cross-network connectivity (same-node and cross-node)
	   - Test all network deselection/reselection methods:
	     * UDN via namespace label change (blue - L3)
	     * UDN via CNC selector update (green - L2)
	     * CUDN via network label change (black - L3)
	     * CUDN via CNC selector update (black+white)
	   - Verify proper isolation when networks are disconnected
	   - Test CNC deletion and re-creation

	   Steps:
	   1.  Create 2 CUDNs: black (L3) and white (L2), each with 2 namespaces and pods on different nodes
	   2.  Create 2 UDNs: blue (L3) and green (L2), each with 2 pods on different nodes
	   3.  Verify initial isolation - pods cannot talk across networks
	   4.  Create CNC selecting all 4 networks
	   5.  Verify CNC annotations are set correctly (subnet allocation)
	   6.  Verify pods can communicate across all networks (same-node and cross-node)
	   7.  Deselect blue UDN (L3) via namespace label removal
	   8.  Verify blue pods isolated, other networks still connected
	   9.  Deselect green UDN (L2) via CNC selector update (remove PUDN selector)
	   10. Verify green pods isolated, CUDNs still connected
	   11. Re-select green UDN via CNC selector update (blue still deselected - no label)
	   12. Re-select blue UDN via namespace label restoration
	   13. Deselect black CUDN (L3) via CUDN label removal
	   14. Verify black pods isolated, other networks still connected
	   15. Re-select black CUDN via CUDN label restoration
	   16. Verify black pods can communicate again
	   17. Deselect both CUDNs (black+white) via CNC selector update (remove CUDN selector)
	   18. Verify CUDN pods isolated, UDNs still connected
	   19. Re-select CUDNs via CNC selector update
	   20. Verify CUDN pods can communicate again
	   21. Delete CNC
	   22. Verify all cross-network connectivity disabled
	   23. Re-create CNC
	   24. Verify all cross-network connectivity restored
	*/
	Context("Pod to pod connectivity validation", func() {
		const nodeHostnameKey = "kubernetes.io/hostname"

		It("should manage cross-network connectivity through CNC lifecycle", func(ctx SpecContext) {
			// Test identifiers
			testID := rand.String(5)
			cncName := generateCNCName()

			// Get 2 schedulable nodes for cross-node testing
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodes.Items)).To(BeNumerically(">=", 2), "test requires at least 2 schedulable nodes")
			node1Name, node2Name := nodes.Items[0].Name, nodes.Items[1].Name

			// Network names
			blackCUDN := fmt.Sprintf("black-cudn-%s", testID)
			whiteCUDN := fmt.Sprintf("white-cudn-%s", testID)
			blueUDN := "blue-udn"
			greenUDN := "green-udn"

			// Namespace names (fixed for predictability in CUDN selectors)
			blackNs0 := fmt.Sprintf("black-ns-0-%s", testID)
			blackNs1 := fmt.Sprintf("black-ns-1-%s", testID)
			whiteNs0 := fmt.Sprintf("white-ns-0-%s", testID)
			whiteNs1 := fmt.Sprintf("white-ns-1-%s", testID)
			blueNs := fmt.Sprintf("blue-ns-%s", testID)
			greenNs := fmt.Sprintf("green-ns-%s", testID)

			// Labels for CNC selection
			cudnLabel := map[string]string{"cnc-test": testID, "type": "cudn"}
			udnLabel := map[string]string{"cnc-test": testID, "type": "pudn"}
			nextSubnets := newTestNetworkSubnetsAllocator()

			// Store pods and their IPs for connectivity testing (supports dual-stack)
			pods := make(map[string]*corev1.Pod)
			podIPs := make(map[string][]string)

			// Cleanup
			DeferCleanup(func() {
				By("Cleanup: Deleting all test resources")
				deleteCNC(cncName)

				// Delete pods first
				for _, pod := range pods {
					_ = cs.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
				}

				// Delete UDNs
				deleteUDN(blueNs, blueUDN)
				deleteUDN(greenNs, greenUDN)

				// Delete CUDNs
				deleteCUDN(blackCUDN)
				deleteCUDN(whiteCUDN)

				// Delete namespaces
				deleteNamespace(cs, blackNs0)
				deleteNamespace(cs, blackNs1)
				deleteNamespace(cs, whiteNs0)
				deleteNamespace(cs, whiteNs1)
				deleteNamespace(cs, blueNs)
				deleteNamespace(cs, greenNs)
			})

			// =====================================================================
			// Step 1: Create 2 CUDNs (black, white) each with 2 namespaces and pods
			// =====================================================================
			By("1. Creating namespaces for black and white CUDNs")
			createUDNNamespaceWithName(cs, blackNs0, nil)
			createUDNNamespaceWithName(cs, blackNs1, nil)
			createUDNNamespaceWithName(cs, whiteNs0, nil)
			createUDNNamespaceWithName(cs, whiteNs1, nil)

			By("1. Creating black CUDN targeting black-ns-0 and black-ns-1")
			v4, v6 := nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, blackCUDN, cudnLabel, v4, v6, blackNs0, blackNs1)

			By("1. Waiting for black CUDN to be ready")
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, blackCUDN), 60*time.Second, time.Second).Should(Succeed())

			By("1. Creating white CUDN targeting white-ns-0 and white-ns-1")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryCUDNWithSubnets(cs, whiteCUDN, cudnLabel, v4, v6, whiteNs0, whiteNs1)

			By("1. Waiting for white CUDN to be ready")
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, whiteCUDN), 60*time.Second, time.Second).Should(Succeed())

			By("1. Creating pods in black CUDN namespaces (on different nodes for cross-node testing)")
			blackPodConfig0 := httpServerPodConfig("black-pod-0", blackNs0)
			blackPodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			blackPodConfig1 := httpServerPodConfig("black-pod-1", blackNs1)
			blackPodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["black-pod-0"] = runUDNPod(cs, blackNs0, blackPodConfig0, nil)
			pods["black-pod-1"] = runUDNPod(cs, blackNs1, blackPodConfig1, nil)
			podIPs["black-pod-0"] = getPrimaryNetworkPodIPs(blackNs0, "black-pod-0", blackCUDN)
			podIPs["black-pod-1"] = getPrimaryNetworkPodIPs(blackNs1, "black-pod-1", blackCUDN)

			By("1. Creating pods in white CUDN namespaces (on different nodes for cross-node testing)")
			whitePodConfig0 := httpServerPodConfig("white-pod-0", whiteNs0)
			whitePodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			whitePodConfig1 := httpServerPodConfig("white-pod-1", whiteNs1)
			whitePodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["white-pod-0"] = runUDNPod(cs, whiteNs0, whitePodConfig0, nil)
			pods["white-pod-1"] = runUDNPod(cs, whiteNs1, whitePodConfig1, nil)
			podIPs["white-pod-0"] = getPrimaryNetworkPodIPs(whiteNs0, "white-pod-0", whiteCUDN)
			podIPs["white-pod-1"] = getPrimaryNetworkPodIPs(whiteNs1, "white-pod-1", whiteCUDN)

			// =====================================================================
			// Step 2: Create 2 UDNs (blue, green) each with 1 namespace and 2 pods on different nodes
			// =====================================================================
			By("2. Creating namespaces for blue and green UDNs with PUDN labels")
			createUDNNamespaceWithName(cs, blueNs, udnLabel)
			createUDNNamespaceWithName(cs, greenNs, udnLabel)

			By("2. Creating blue UDN (L3)")
			v4, v6 = nextSubnets(true)
			createLayer3PrimaryUDNWithSubnets(cs, blueNs, blueUDN, v4, v6)

			By("2. Waiting for blue UDN to be ready")
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, blueNs, blueUDN), 60*time.Second, time.Second).Should(Succeed())

			By("2. Creating green UDN (L2)")
			v4, v6 = nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, greenNs, greenUDN, v4, v6)

			By("2. Waiting for green UDN to be ready")
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, greenNs, greenUDN), 60*time.Second, time.Second).Should(Succeed())

			By("2. Creating pods in blue UDN namespace (on different nodes for same and cross-node testing)")
			bluePodConfig0 := httpServerPodConfig("blue-pod-0", blueNs)
			bluePodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			bluePodConfig1 := httpServerPodConfig("blue-pod-1", blueNs)
			bluePodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["blue-pod-0"] = runUDNPod(cs, blueNs, bluePodConfig0, nil)
			pods["blue-pod-1"] = runUDNPod(cs, blueNs, bluePodConfig1, nil)
			podIPs["blue-pod-0"] = getPrimaryNetworkPodIPs(blueNs, "blue-pod-0", blueUDN)
			podIPs["blue-pod-1"] = getPrimaryNetworkPodIPs(blueNs, "blue-pod-1", blueUDN)

			By("2. Creating pods in green UDN namespace (on different nodes for same and cross-node testing)")
			greenPodConfig0 := httpServerPodConfig("green-pod-0", greenNs)
			greenPodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			greenPodConfig1 := httpServerPodConfig("green-pod-1", greenNs)
			greenPodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["green-pod-0"] = runUDNPod(cs, greenNs, greenPodConfig0, nil)
			pods["green-pod-1"] = runUDNPod(cs, greenNs, greenPodConfig1, nil)
			podIPs["green-pod-0"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-0", greenUDN)
			podIPs["green-pod-1"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-1", greenUDN)

			// =====================================================================
			// Step 3: Verify initial isolation - pods cannot talk across networks
			// =====================================================================
			By("3. Verifying initial isolation - black pods cannot reach white pods")
			blackPods := map[string]*corev1.Pod{"black-pod-0": pods["black-pod-0"], "black-pod-1": pods["black-pod-1"]}
			whitePodIPs := map[string][]string{"white-pod-0": podIPs["white-pod-0"], "white-pod-1": podIPs["white-pod-1"]}
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, false)

			By("3. Verifying initial isolation - white pods cannot reach blue pods")
			whitePods := map[string]*corev1.Pod{"white-pod-0": pods["white-pod-0"], "white-pod-1": pods["white-pod-1"]}
			bluePodIPs := map[string][]string{"blue-pod-0": podIPs["blue-pod-0"], "blue-pod-1": podIPs["blue-pod-1"]}
			verifyCrossNetworkConnectivity(whitePods, bluePodIPs, false)

			By("3. Verifying initial isolation - blue pods cannot reach green pods")
			bluePods := map[string]*corev1.Pod{"blue-pod-0": pods["blue-pod-0"], "blue-pod-1": pods["blue-pod-1"]}
			greenPodIPs := map[string][]string{"green-pod-0": podIPs["green-pod-0"], "green-pod-1": podIPs["green-pod-1"]}
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, false)

			// =====================================================================
			// Step 4: Create CNC selecting all 4 networks
			// =====================================================================
			By("4. Creating CNC selecting all 4 networks (2 CUDNs + 2 UDNs)")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			// =====================================================================
			// Step 5: Verify CM annotations are set correctly
			// =====================================================================
			By("5. Verifying CNC has both tunnel ID and subnet annotations")
			verifyCNCHasBothAnnotations(cncName)

			By("5. Verifying CNC subnet annotation has 4 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// =====================================================================
			// Step 6: Verify pods can now communicate across all networks
			// =====================================================================
			By("6. Verifying pods can communicate across all connected networks")
			// Use one pod per network as source (targets include both nodes for same/cross-node coverage)
			srcPods := map[string]*corev1.Pod{
				"black-pod-0": pods["black-pod-0"],
				"white-pod-0": pods["white-pod-0"],
				"blue-pod-0":  pods["blue-pod-0"],
				"green-pod-0": pods["green-pod-0"],
			}
			verifyFullMeshConnectivity(srcPods, podIPs, true)

			// =====================================================================
			// Step 7: Deselect blue UDN by changing namespace label
			// =====================================================================
			By("7. Removing PUDN label from blue namespace to deselect blue UDN")
			_, err = cs.CoreV1().Namespaces().Patch(context.Background(), blueNs,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"type":null}}}`),
				metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("7. Verifying CNC subnet annotation now has 3 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)

			// =====================================================================
			// Step 8: Verify blue pods cannot talk to other network pods
			// =====================================================================
			By("8. Verifying blue pods cannot reach other network pods")
			verifyCrossNetworkConnectivity(bluePods, whitePodIPs, false)
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, false)

			By("8. Verifying other networks can still communicate with each other")
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, true)
			verifyCrossNetworkConnectivity(whitePods, greenPodIPs, true)

			// =====================================================================
			// Step 9: Deselect green UDN by updating CNC to remove PUDN selector
			// =====================================================================
			By("9. Updating CNC to remove PUDN selector (deselects green UDN)")
			createOrUpdateCNC(cs, cncName, cudnLabel, nil)

			By("9. Verifying CNC subnet annotation now has 2 networks (only CUDNs)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)

			// =====================================================================
			// Step 10: Verify green pods cannot talk to other network pods
			// =====================================================================
			greenPods := map[string]*corev1.Pod{"green-pod-0": pods["green-pod-0"], "green-pod-1": pods["green-pod-1"]}
			By("10. Verifying green pods cannot reach other network pods")
			verifyCrossNetworkConnectivity(greenPods, whitePodIPs, false)
			verifyCrossNetworkConnectivity(greenPods, bluePodIPs, false)

			By("10. Verifying black and white CUDNs can still communicate")
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, true)
			verifyCrossNetworkConnectivity(whitePods, map[string][]string{"black-pod-0": podIPs["black-pod-0"]}, true)

			// =====================================================================
			// Step 11: Re-select green UDN via CNC selector update
			// (blue still deselected - doesn't have namespace label from step 7)
			// =====================================================================
			By("11. Updating CNC to add PUDN selector back (re-selects green UDN)")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("11. Verifying CNC subnet annotation has 3 networks (green re-selected, blue still deselected)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)

			By("11. Verifying green pods can communicate with black and white CUDNs pods")
			blackPodIPs := map[string][]string{"black-pod-0": podIPs["black-pod-0"], "black-pod-1": podIPs["black-pod-1"]}
			verifyCrossNetworkConnectivity(greenPods, whitePodIPs, true)
			verifyCrossNetworkConnectivity(greenPods, blackPodIPs, true)

			// =====================================================================
			// Step 12: Re-select blue UDN via namespace label
			// =====================================================================
			By("12. Adding PUDN label back to blue namespace (re-selects blue UDN)")
			_, err = cs.CoreV1().Namespaces().Patch(context.Background(), blueNs,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"type":"pudn"}}}`),
				metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("12. Verifying CNC subnet annotation has 4 networks again")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			By("12. Verifying blue pods can communicate with other network pods")
			verifyCrossNetworkConnectivity(bluePods, whitePodIPs, true)
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, true)

			// =====================================================================
			// Step 13: Deselect black CUDN via CUDN label change
			// =====================================================================
			By("13. Removing CNC-matching label from black CUDN (deselects black CUDN)")
			_, err = e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", blackCUDN, "type-")
			Expect(err).NotTo(HaveOccurred())

			By("13. Verifying CNC subnet annotation now has 3 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)

			// =====================================================================
			// Step 14: Verify black pods cannot talk with other networks
			// =====================================================================
			By("14. Verifying black pods cannot reach other network pods")
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, false)
			verifyCrossNetworkConnectivity(blackPods, bluePodIPs, false)
			verifyCrossNetworkConnectivity(blackPods, greenPodIPs, false)

			By("14. Verifying white, blue, and green networks can still communicate")
			verifyCrossNetworkConnectivity(whitePods, bluePodIPs, true)
			verifyCrossNetworkConnectivity(whitePods, greenPodIPs, true)
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, true)

			// =====================================================================
			// Step 15: Re-select black CUDN via CUDN label
			// =====================================================================
			By("15. Adding CNC-matching label back to black CUDN (re-selects black CUDN)")
			_, err = e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", blackCUDN, "type=cudn")
			Expect(err).NotTo(HaveOccurred())

			By("15. Verifying CNC subnet annotation has 4 networks again")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// =====================================================================
			// Step 16: Verify black pods can communicate again
			// =====================================================================
			By("16. Verifying black pods can reach other network pods again")
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, true)
			verifyCrossNetworkConnectivity(blackPods, bluePodIPs, true)
			verifyCrossNetworkConnectivity(blackPods, greenPodIPs, true)

			// =====================================================================
			// Step 17: Deselect both CUDNs (black+white) via CNC selector update
			// =====================================================================
			By("17. Updating CNC to remove CUDN selector (deselects black and white CUDNs)")
			createOrUpdateCNC(cs, cncName, nil, udnLabel)

			By("17. Verifying CNC subnet annotation now has 2 networks (only UDNs)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)

			// =====================================================================
			// Step 18: Verify both CUDN pods cannot talk with other networks
			// =====================================================================
			By("18. Verifying black and white CUDN pods cannot reach UDN pods")
			verifyCrossNetworkConnectivity(blackPods, bluePodIPs, false)
			verifyCrossNetworkConnectivity(blackPods, greenPodIPs, false)
			verifyCrossNetworkConnectivity(whitePods, bluePodIPs, false)
			verifyCrossNetworkConnectivity(whitePods, greenPodIPs, false)

			By("18. Verifying blue and green UDNs can still communicate")
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, true)

			// =====================================================================
			// Step 19: Re-select CUDNs via CNC selector update
			// =====================================================================
			By("19. Updating CNC to add CUDN selector back (re-selects black and white CUDNs)")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("19. Verifying CNC subnet annotation has 4 networks again")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// =====================================================================
			// Step 20: Verify CUDN pods can communicate again
			// =====================================================================
			By("20. Verifying black and white CUDN pods can reach other network pods again")
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, true)
			verifyCrossNetworkConnectivity(blackPods, bluePodIPs, true)
			verifyCrossNetworkConnectivity(whitePods, greenPodIPs, true)

			// =====================================================================
			// Step 21: Delete CNC
			// =====================================================================
			By("21. Deleting CNC")
			deleteCNC(cncName)

			By("21. Waiting for CNC deletion to complete")
			Eventually(func() bool {
				_, err := getCNCAnnotations(cncName)
				return err != nil
			}, 60*time.Second, 2*time.Second).Should(BeTrue())

			// =====================================================================
			// Step 22: Verify all pods cannot talk to pods in other networks
			// =====================================================================
			By("22. Verifying all cross-network connectivity is disabled")
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, false)
			verifyCrossNetworkConnectivity(blackPods, bluePodIPs, false)
			verifyCrossNetworkConnectivity(blackPods, greenPodIPs, false)
			verifyCrossNetworkConnectivity(whitePods, bluePodIPs, false)
			verifyCrossNetworkConnectivity(whitePods, greenPodIPs, false)
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, false)

			// =====================================================================
			// Step 23: Re-create the CNC
			// =====================================================================
			By("23. Re-creating CNC selecting all 4 networks")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("23. Verifying CNC has both tunnel ID and subnet annotations")
			verifyCNCHasBothAnnotations(cncName)

			By("23. Verifying CNC subnet annotation has 4 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// =====================================================================
			// Step 24: Verify pods can communicate across all networks again
			// =====================================================================
			By("24. Verifying pods can communicate across all connected networks again")
			verifyFullMeshConnectivity(srcPods, podIPs, true)

			By("Test completed successfully - CNC lifecycle validated")
		})
	})

	Context("Dynamic UDN allocation", func() {
		const (
			nodeHostnameKey         = "kubernetes.io/hostname"
			terminatingPodFinalizer = "k8s.ovn.org/e2e-terminating-pod"
		)

		It("should connect cross-node UDN pods and retain terminating node state", func(ctx SpecContext) {
			if !isDynamicUDNEnabled() {
				Skip("test requires DYNAMIC_UDN_ALLOCATION=true")
			}

			testID := rand.String(5)
			cncName := generateCNCName()
			l3UDNName := "blue-udn"
			l2UDNName := "green-udn"
			l3Namespace := fmt.Sprintf("blue-ns-%s", testID)
			l2Namespace := fmt.Sprintf("green-ns-%s", testID)
			udnLabel := map[string]string{"cnc-test": testID}

			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodes.Items)).To(BeNumerically(">=", 2), "test requires at least 2 schedulable nodes")
			node1Name, node2Name := nodes.Items[0].Name, nodes.Items[1].Name

			var l3PodNode1, l3PodNode2, l2Pod *corev1.Pod

			DeferCleanup(func() {
				if l3PodNode1 != nil {
					_, _ = cs.CoreV1().Pods(l3PodNode1.Namespace).Patch(
						context.Background(), l3PodNode1.Name, types.MergePatchType,
						[]byte(`{"metadata":{"finalizers":[]}}`), metav1.PatchOptions{})
				}
				deleteCNC(cncName)
				if l3PodNode1 != nil {
					_ = cs.CoreV1().Pods(l3PodNode1.Namespace).Delete(context.Background(), l3PodNode1.Name, metav1.DeleteOptions{})
				}
				if l3PodNode2 != nil {
					_ = cs.CoreV1().Pods(l3PodNode2.Namespace).Delete(context.Background(), l3PodNode2.Name, metav1.DeleteOptions{})
				}
				if l2Pod != nil {
					_ = cs.CoreV1().Pods(l2Pod.Namespace).Delete(context.Background(), l2Pod.Name, metav1.DeleteOptions{})
				}
				deleteUDN(l3Namespace, l3UDNName)
				deleteUDN(l2Namespace, l2UDNName)
				deleteNamespace(cs, l3Namespace)
				deleteNamespace(cs, l2Namespace)
			})

			By("Creating two UDN namespaces selected by the same CNC")
			createUDNNamespaceWithName(cs, l3Namespace, udnLabel)
			createUDNNamespaceWithName(cs, l2Namespace, udnLabel)

			By("Creating the L3 and L2 UDNs")
			createLayer3PrimaryUDNWithSubnets(cs, l3Namespace, l3UDNName, []string{"10.140.0.0/16"}, []string{"2014:100:600::0/60"})
			createLayer2PrimaryUDNWithSubnets(cs, l2Namespace, l2UDNName, []string{"10.141.0.0/16"}, []string{"2014:100:700::0/60"})

			By("Waiting for both UDNs to be ready")
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, l3Namespace, l3UDNName), 60*time.Second, time.Second).Should(Succeed())
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, l2Namespace, l2UDNName), 60*time.Second, time.Second).Should(Succeed())

			By("Creating the CNC before either node becomes active for the selected UDNs")
			createOrUpdateCNC(cs, cncName, nil, udnLabel)

			By("Verifying the CNC selected both UDNs")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			l3NetworkID := getCNCNetworkIDForTopology(cncName, "layer3")
			node1ID := getNodeIDAnnotation(cs, node1Name)
			node2ID := getNodeIDAnnotation(cs, node2Name)

			By(fmt.Sprintf("Creating an L3 pod on %s", node1Name))
			l3PodNode1Config := httpServerPodConfig("blue-pod-node1", l3Namespace)
			l3PodNode1Config.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			l3PodNode1 = runUDNPod(cs, l3Namespace, l3PodNode1Config, func(pod *corev1.Pod) {
				terminationGracePeriodSeconds := int64(90)
				pod.Finalizers = append(pod.Finalizers, terminatingPodFinalizer)
				pod.Spec.TerminationGracePeriodSeconds = &terminationGracePeriodSeconds
				pod.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{
					PreStop: &corev1.LifecycleHandler{
						Exec: &corev1.ExecAction{Command: []string{"/agnhost", "pause"}},
					},
				}
			})

			By(fmt.Sprintf("Creating a second L3 pod on %s", node2Name))
			l3PodNode2Config := httpServerPodConfig("blue-pod-node2", l3Namespace)
			l3PodNode2Config.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			l3PodNode2 = runUDNPod(cs, l3Namespace, l3PodNode2Config, nil)

			By(fmt.Sprintf("Creating an L2 pod on %s", node2Name))
			l2PodConfig := httpServerPodConfig("green-pod", l2Namespace)
			l2PodConfig.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			l2Pod = runUDNPod(cs, l2Namespace, l2PodConfig, nil)

			l3PodNode1IPs := getPrimaryNetworkPodIPs(l3Namespace, l3PodNode1.Name, l3UDNName)
			l3PodNode2IPs := getPrimaryNetworkPodIPs(l3Namespace, l3PodNode2.Name, l3UDNName)
			l2PodIPs := getPrimaryNetworkPodIPs(l2Namespace, l2Pod.Name, l2UDNName)

			By("Verifying the cross-node L3 and L2 pods can ping each other through the CNC")
			for _, l2PodIP := range l2PodIPs {
				Eventually(func() error {
					return checkPingConnectivity(l3PodNode1.Namespace, l3PodNode1.Name, l2PodIP)
				}, 30*time.Second, 2*time.Second).Should(Succeed(),
					fmt.Sprintf("expected %s/%s to ping %s", l3PodNode1.Namespace, l3PodNode1.Name, l2PodIP))
			}
			for _, l3PodIP := range l3PodNode1IPs {
				Eventually(func() error {
					return checkPingConnectivity(l2Pod.Namespace, l2Pod.Name, l3PodIP)
				}, 30*time.Second, 2*time.Second).Should(Succeed(),
					fmt.Sprintf("expected %s/%s to ping %s", l2Pod.Namespace, l2Pod.Name, l3PodIP))
			}

			By("Verifying CNC created L3 node-specific OVN state for both active L3 nodes")
			verifyCNCNodeOVNObjects(f, cs, cncName, l3NetworkID, node1ID, true)
			verifyCNCNodeOVNObjects(f, cs, cncName, l3NetworkID, node2ID, true)

			By(fmt.Sprintf("Deleting the final L3 pod on %s", node1Name))
			Expect(cs.CoreV1().Pods(l3PodNode1.Namespace).Delete(
				context.Background(), l3PodNode1.Name, metav1.DeleteOptions{})).To(Succeed())

			By("Waiting for the pod to be terminating while its container still runs")
			Eventually(func(g Gomega) {
				pod, err := cs.CoreV1().Pods(l3PodNode1.Namespace).Get(
					context.Background(), l3PodNode1.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pod.DeletionTimestamp).NotTo(BeNil())
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
				g.Expect(pod.Status.ContainerStatuses).NotTo(BeEmpty())
				g.Expect(pod.Status.ContainerStatuses[0].State.Running).NotTo(BeNil())
			}, 30*time.Second, time.Second).Should(Succeed(), fmt.Sprintf(
				"expected pod %s/%s to remain running while terminating",
				l3PodNode1.Namespace, l3PodNode1.Name))

			// Force a pod tracker reconciliation while the pod is terminating. A
			// semantically empty network-selection update must not remove the
			// primary UDN reference while the container is still running.
			By("Forcing pod tracker reconciliation during termination")
			networkSelectionPatch := fmt.Sprintf(
				`{"metadata":{"annotations":{%q:"[]"}}}`, nadapi.NetworkAttachmentAnnot)
			_, err = cs.CoreV1().Pods(l3PodNode1.Namespace).Patch(
				context.Background(), l3PodNode1.Name, types.MergePatchType,
				[]byte(networkSelectionPatch), metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying connectivity and node state persist during termination")
			Consistently(func(g Gomega) {
				pod, err := cs.CoreV1().Pods(l3PodNode1.Namespace).Get(
					context.Background(), l3PodNode1.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
				g.Expect(pod.Status.ContainerStatuses).NotTo(BeEmpty())
				g.Expect(pod.Status.ContainerStatuses[0].State.Running).NotTo(BeNil())
				for _, l3PodIP := range l3PodNode1IPs {
					g.Expect(checkConnectivity(
						l3PodNode2.Namespace, l3PodNode2.Name, l3PodIP, true)).To(BeTrue(),
						"expected terminating pod IP %s to remain reachable", l3PodIP)
				}
				ports, err := findCNCNodeOVNObjects(
					f, cs, "Logical_Router_Port", "name", cncName, l3NetworkID, node1ID)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ports).NotTo(BeEmpty())
				routes, err := findCNCNodeOVNObjects(
					f, cs, "Logical_Router_Static_Route", "ip_prefix,nexthop",
					cncName, l3NetworkID, node1ID)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(routes).NotTo(BeEmpty())
			}, 30*time.Second, 2*time.Second).Should(Succeed(), fmt.Sprintf(
				"expected connectivity and node-specific OVN state for terminating pod %s/%s to persist",
				l3PodNode1.Namespace, l3PodNode1.Name))

			By("Waiting for the terminating pod to reach a terminal phase")
			Eventually(func(g Gomega) {
				pod, err := cs.CoreV1().Pods(l3PodNode1.Namespace).Get(
					context.Background(), l3PodNode1.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pod.Status.Phase).To(Or(
					Equal(corev1.PodSucceeded), Equal(corev1.PodFailed)))
			}, 2*time.Minute, 2*time.Second).Should(Succeed(), fmt.Sprintf(
				"expected terminating pod %s/%s to reach a terminal phase",
				l3PodNode1.Namespace, l3PodNode1.Name))

			By("Verifying terminal pod cleanup removes only the inactive node state")
			verifyCNCNodeOVNObjects(f, cs, cncName, l3NetworkID, node1ID, false)
			verifyCNCNodeOVNObjects(f, cs, cncName, l3NetworkID, node2ID, true)

			By("Verifying the remaining L3 pod is still reachable through the CNC")
			for _, l3PodIP := range l3PodNode2IPs {
				Eventually(func() error {
					return checkPingConnectivity(l2Pod.Namespace, l2Pod.Name, l3PodIP)
				}, 30*time.Second, 2*time.Second).Should(Succeed(),
					fmt.Sprintf("expected %s/%s to ping remaining L3 pod IP %s", l2Pod.Namespace, l2Pod.Name, l3PodIP))
			}

			By("Removing the test finalizer and waiting for pod deletion")
			_, err = cs.CoreV1().Pods(l3PodNode1.Namespace).Patch(
				context.Background(), l3PodNode1.Name, types.MergePatchType,
				[]byte(`{"metadata":{"finalizers":[]}}`), metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(waitForPodNotFoundInNamespace(
				context.Background(), cs, l3PodNode1.Name, l3PodNode1.Namespace,
				l3PodNode1.UID, 30*time.Second)).To(Succeed())
			l3PodNode1 = nil
		})
	})

	// Multiple CNCs with overlapping network selection.
	// Tests validate behavior when multiple CNCs exist in the cluster with
	// overlapping or identical network selections, covering pod connectivity,
	// service connectivity, and CNC deletion scenarios.
	Context("Multiple CNCs with overlapping network selection", func() {
		const nodeHostnameKey = "kubernetes.io/hostname"

		// Second CNC connect subnet configuration (must be different from first CNC)
		const (
			cnc2ConnectSubnetIPv4CIDR   = "192.169.0.0/16"
			cnc2ConnectSubnetIPv4Prefix = 24
			cnc2ConnectSubnetIPv6CIDR   = "fd00:11::/112"
			cnc2ConnectSubnetIPv6Prefix = 120
		)

		/*
		   Non-transitive pod connectivity with overlapping CNCs:

		   Network topology:
		   - CNC-1: selects blue (L2 UDN) and red (L3 CUDN)
		   - CNC-2: selects blue (L2 UDN) and green (L3 UDN)
		   - Blue is shared between both CNCs

		   Expected connectivity:
		   - blue <-> red (via CNC-1)
		   - blue <-> green (via CNC-2)
		   - red <-/-> green (non-transitive)

		   Steps:
		   1. Create CNC-1 selecting blue and red (no networks exist yet)
		   2. Create CNC-2 selecting blue and green (no networks exist yet)
		   3. Create blue UDN (L2) with pods - gets added to both CNCs
		   4. Create red CUDN (L3) with pods - gets added to CNC-1
		   5. Create green UDN (L3) with pods - gets added to CNC-2
		   6. Verify blue <-> red connectivity via CNC-1
		   7. Verify blue <-> green connectivity via CNC-2
		   8. Verify red <-/-> green (non-transitive - no connectivity)
		   9. Delete CNC-1, verify blue <-> green still works, blue <-/-> red
		   10. Delete CNC-2, verify all networks isolated
		*/
		It("should maintain non-transitive connectivity when a network is selected by multiple CNCs", func(ctx SpecContext) {
			// Test identifiers
			testID := rand.String(5)
			cnc1Name := fmt.Sprintf("color-1-%s", testID)
			cnc2Name := fmt.Sprintf("color-2-%s", testID)
			nextSubnets := newTestNetworkSubnetsAllocator()

			// Get 2 schedulable nodes for cross-node testing
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodes.Items)).To(BeNumerically(">=", 2), "test requires at least 2 schedulable nodes")
			node1Name, node2Name := nodes.Items[0].Name, nodes.Items[1].Name

			// Network names
			blueUDN := "blue-udn"                         // L2 UDN - shared between both CNCs
			redCUDN := fmt.Sprintf("red-cudn-%s", testID) // L3 CUDN - CNC-1 only
			greenUDN := "green-udn"                       // L3 UDN - CNC-2 only

			// Namespace names
			blueNs := fmt.Sprintf("blue-ns-%s", testID)
			redNs := fmt.Sprintf("red-ns-%s", testID)
			greenNs := fmt.Sprintf("green-ns-%s", testID)

			// Labels for CNC selection
			// Blue needs labels for both CNC-1 (via blueLabel) and CNC-2 (via cnc2Label)
			blueLabel := map[string]string{"network-color": "blue", "test-id": testID, "cnc2-member": "true"}
			redLabel := map[string]string{"network-color": "red", "test-id": testID}
			// Green needs label for CNC-2
			greenLabel := map[string]string{"network-color": "green", "test-id": testID, "cnc2-member": "true"}
			// CNC-2 selector - matches both blue and green namespaces
			cnc2Label := map[string]string{"cnc2-member": "true"}

			// Store pods and their IPs
			pods := make(map[string]*corev1.Pod)
			podIPs := make(map[string][]string)

			// Cleanup
			DeferCleanup(func() {
				By("Cleanup: Deleting all test resources")
				deleteCNC(cnc1Name)
				deleteCNC(cnc2Name)

				for _, pod := range pods {
					_ = cs.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
				}

				deleteUDN(blueNs, blueUDN)
				deleteUDN(greenNs, greenUDN)
				deleteCUDN(redCUDN)

				deleteNamespace(cs, blueNs)
				deleteNamespace(cs, redNs)
				deleteNamespace(cs, greenNs)
			})

			// =====================================================================
			// Step 1: Create CNC-1 selecting blue and red (no networks exist yet)
			// Each CNC must have different connect subnets
			// =====================================================================
			By("1. Creating CNC-1 (color-1) selecting blue UDN and red CUDN with first connect subnet")
			createOrUpdateCNCWithSubnets(cnc1Name, redLabel, blueLabel, generateConnectSubnets(cs))

			By("1. Verifying CNC-1 has only tunnel ID annotation (no networks yet)")
			verifyCNCHasOnlyTunnelIDAnnotation(cnc1Name)

			// =====================================================================
			// Step 2: Create CNC-2 selecting blue and green (no networks exist yet)
			// =====================================================================
			By("2. Creating CNC-2 (color-2) selecting blue UDN and green UDN with second connect subnet")
			createOrUpdateCNCWithSubnets(cnc2Name, nil, cnc2Label, generateConnectSubnetsWithCIDRs(cs, cnc2ConnectSubnetIPv4CIDR, cnc2ConnectSubnetIPv4Prefix, cnc2ConnectSubnetIPv6CIDR, cnc2ConnectSubnetIPv6Prefix))

			By("2. Verifying CNC-2 has only tunnel ID annotation (no networks yet)")
			verifyCNCHasOnlyTunnelIDAnnotation(cnc2Name)

			// =====================================================================
			// Step 3: Create blue UDN (L2) with pods - gets added to both CNCs
			// =====================================================================
			By("3. Creating blue namespace and L2 UDN")
			createUDNNamespaceWithName(cs, blueNs, blueLabel)
			v4, v6 := nextSubnets(false)
			createLayer2PrimaryUDNWithSubnets(cs, blueNs, blueUDN, v4, v6)

			By("3. Waiting for blue UDN to be ready")
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, blueNs, blueUDN), 60*time.Second, time.Second).Should(Succeed())

			By("3. Verifying CNC-1 now has 1 network (blue)")
			verifyCNCSubnetAnnotationNetworkCount(cnc1Name, 1)

			By("3. Verifying CNC-2 now has 1 network (blue)")
			verifyCNCSubnetAnnotationNetworkCount(cnc2Name, 1)

			By("3. Creating pods in blue UDN namespace")
			bluePodConfig0 := httpServerPodConfig("blue-pod-0", blueNs)
			bluePodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			bluePodConfig1 := httpServerPodConfig("blue-pod-1", blueNs)
			bluePodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["blue-pod-0"] = runUDNPod(cs, blueNs, bluePodConfig0, nil)
			pods["blue-pod-1"] = runUDNPod(cs, blueNs, bluePodConfig1, nil)
			podIPs["blue-pod-0"] = getPrimaryNetworkPodIPs(blueNs, "blue-pod-0", blueUDN)
			podIPs["blue-pod-1"] = getPrimaryNetworkPodIPs(blueNs, "blue-pod-1", blueUDN)

			// =====================================================================
			// Step 4: Create red CUDN (L3) with pods - gets added to CNC-1
			// =====================================================================
			By("4. Creating red namespace and L3 CUDN")
			createUDNNamespaceWithName(cs, redNs, nil)
			v4, v6 = nextSubnets(true)
			createLayer3PrimaryCUDNWithSubnets(cs, redCUDN, redLabel, v4, v6, redNs)

			By("4. Waiting for red CUDN to be ready")
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, redCUDN), 60*time.Second, time.Second).Should(Succeed())

			By("4. Verifying CNC-1 now has 2 networks (blue + red)")
			verifyCNCSubnetAnnotationNetworkCount(cnc1Name, 2)

			By("4. Verifying CNC-2 still has 1 network (blue only)")
			verifyCNCSubnetAnnotationNetworkCount(cnc2Name, 1)

			By("4. Creating pods in red CUDN namespace")
			redPodConfig0 := httpServerPodConfig("red-pod-0", redNs)
			redPodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			redPodConfig1 := httpServerPodConfig("red-pod-1", redNs)
			redPodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["red-pod-0"] = runUDNPod(cs, redNs, redPodConfig0, nil)
			pods["red-pod-1"] = runUDNPod(cs, redNs, redPodConfig1, nil)
			podIPs["red-pod-0"] = getPrimaryNetworkPodIPs(redNs, "red-pod-0", redCUDN)
			podIPs["red-pod-1"] = getPrimaryNetworkPodIPs(redNs, "red-pod-1", redCUDN)

			// =====================================================================
			// Step 5: Create green UDN (L3) with pods - gets added to CNC-2
			// =====================================================================
			By("5. Creating green namespace and L3 UDN")
			createUDNNamespaceWithName(cs, greenNs, greenLabel)
			v4, v6 = nextSubnets(true)
			createLayer3PrimaryUDNWithSubnets(cs, greenNs, greenUDN, v4, v6)

			By("5. Waiting for green UDN to be ready")
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, greenNs, greenUDN), 60*time.Second, time.Second).Should(Succeed())

			By("5. Verifying CNC-1 still has 2 networks (blue + red)")
			verifyCNCSubnetAnnotationNetworkCount(cnc1Name, 2)

			By("5. Verifying CNC-2 now has 2 networks (blue + green)")
			verifyCNCSubnetAnnotationNetworkCount(cnc2Name, 2)

			By("5. Creating pods in green UDN namespace")
			greenPodConfig0 := httpServerPodConfig("green-pod-0", greenNs)
			greenPodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			greenPodConfig1 := httpServerPodConfig("green-pod-1", greenNs)
			greenPodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["green-pod-0"] = runUDNPod(cs, greenNs, greenPodConfig0, nil)
			pods["green-pod-1"] = runUDNPod(cs, greenNs, greenPodConfig1, nil)
			podIPs["green-pod-0"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-0", greenUDN)
			podIPs["green-pod-1"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-1", greenUDN)

			// Define pod groups for connectivity testing
			bluePods := map[string]*corev1.Pod{"blue-pod-0": pods["blue-pod-0"], "blue-pod-1": pods["blue-pod-1"]}
			redPods := map[string]*corev1.Pod{"red-pod-0": pods["red-pod-0"], "red-pod-1": pods["red-pod-1"]}
			greenPods := map[string]*corev1.Pod{"green-pod-0": pods["green-pod-0"], "green-pod-1": pods["green-pod-1"]}

			bluePodIPs := map[string][]string{"blue-pod-0": podIPs["blue-pod-0"], "blue-pod-1": podIPs["blue-pod-1"]}
			redPodIPs := map[string][]string{"red-pod-0": podIPs["red-pod-0"], "red-pod-1": podIPs["red-pod-1"]}
			greenPodIPs := map[string][]string{"green-pod-0": podIPs["green-pod-0"], "green-pod-1": podIPs["green-pod-1"]}

			// =====================================================================
			// Step 6: Verify blue <-> red connectivity via CNC-1
			// =====================================================================
			By("6. Verifying blue <-> red connectivity via CNC-1")
			verifyCrossNetworkConnectivity(bluePods, redPodIPs, true)
			verifyCrossNetworkConnectivity(redPods, bluePodIPs, true)

			// =====================================================================
			// Step 7: Verify blue <-> green connectivity via CNC-2
			// =====================================================================
			By("7. Verifying blue <-> green connectivity via CNC-2")
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, true)
			verifyCrossNetworkConnectivity(greenPods, bluePodIPs, true)

			// =====================================================================
			// Step 8: Verify red <-/-> green (non-transitive)
			// =====================================================================
			By("8. Verifying red <-/-> green (non-transitive - no direct connectivity)")
			verifyCrossNetworkConnectivity(redPods, greenPodIPs, false)
			verifyCrossNetworkConnectivity(greenPods, redPodIPs, false)

			By("8. Verifying blue still connected to both red and green")
			verifyCrossNetworkConnectivity(bluePods, redPodIPs, true)
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, true)

			// =====================================================================
			// Step 9: Delete CNC-1, verify blue <-> green still works
			// =====================================================================
			By("9. Deleting CNC-1")
			deleteCNC(cnc1Name)

			By("9. Waiting for CNC-1 deletion to complete")
			Eventually(func() bool {
				_, err := getCNCAnnotations(cnc1Name)
				return err != nil
			}, 60*time.Second, 2*time.Second).Should(BeTrue())

			By("9. Verifying blue <-> green still connected via CNC-2")
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, true)
			verifyCrossNetworkConnectivity(greenPods, bluePodIPs, true)

			By("9. Verifying blue <-/-> red (CNC-1 deleted)")
			verifyCrossNetworkConnectivity(bluePods, redPodIPs, false)
			verifyCrossNetworkConnectivity(redPods, bluePodIPs, false)

			// =====================================================================
			// Step 10: Delete CNC-2, verify all networks isolated
			// =====================================================================
			By("10. Deleting CNC-2")
			deleteCNC(cnc2Name)

			By("10. Waiting for CNC-2 deletion to complete")
			Eventually(func() bool {
				_, err := getCNCAnnotations(cnc2Name)
				return err != nil
			}, 60*time.Second, 2*time.Second).Should(BeTrue())

			By("10. Verifying all networks are now isolated")
			verifyCrossNetworkConnectivity(bluePods, redPodIPs, false)
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, false)
			verifyCrossNetworkConnectivity(redPods, greenPodIPs, false)

			By("Test completed successfully - Multiple CNCs with non-transitive connectivity validated")
		})

		/*
		   Non-transitive service connectivity with overlapping CNCs:

		   Same topology as the pod connectivity test above, but with
		   ServiceNetwork enabled on both CNCs:
		   - CNC-1: selects blue (L2 UDN) + red (L3 CUDN) with service connectivity
		   - CNC-2: selects blue (L2 UDN) + green (L3 UDN) with service connectivity

		   Steps:
		   1. Create CNC-1 and CNC-2 with ServiceNetwork enabled
		   2. Create networks (blue, red, green) with pods
		   3. Verify CNC annotations
		   4. Create ClusterIP services in all networks
		   5. Verify cross-network service connectivity:
		      - blue-svc reachable from red/green, red-svc from blue, green-svc from blue
		      - red-svc NOT reachable from green (non-transitive)
		   6. Delete CNC-1, verify CNC-2 service connectivity survives
		      - green->blue-svc and blue->green-svc still work
		      - red-svc and blue-svc no longer connected via CNC-1
		   7. Delete CNC-2, verify all services isolated
		*/
		It("should maintain non-transitive service connectivity when a network is selected by multiple CNCs", func(ctx SpecContext) {
			// Same topology as above but with ServiceNetwork enabled:
			// CNC-1: blue + red, CNC-2: blue + green
			// Verify service VIPs work cross-network and survive CNC-1 deletion

			testID := rand.String(5)
			cnc1Name := fmt.Sprintf("svc-color-1-%s", testID)
			cnc2Name := fmt.Sprintf("svc-color-2-%s", testID)

			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodes.Items)).To(BeNumerically(">=", 2), "test requires at least 2 schedulable nodes")
			node1Name, node2Name := nodes.Items[0].Name, nodes.Items[1].Name

			blueUDN := "blue-udn"
			redCUDN := fmt.Sprintf("red-cudn-%s", testID)
			greenUDN := "green-udn"

			blueNs := fmt.Sprintf("blue-ns-%s", testID)
			redNs := fmt.Sprintf("red-ns-%s", testID)
			greenNs := fmt.Sprintf("green-ns-%s", testID)

			blueLabel := map[string]string{"network-color": "blue", "test-id": testID, "cnc2-member": "true"}
			redLabel := map[string]string{"network-color": "red", "test-id": testID}
			greenLabel := map[string]string{"network-color": "green", "test-id": testID, "cnc2-member": "true"}
			cnc2Label := map[string]string{"cnc2-member": "true"}

			bluePodLabel := map[string]string{"app": fmt.Sprintf("blue-%s", testID)}
			redPodLabel := map[string]string{"app": fmt.Sprintf("red-%s", testID)}
			greenPodLabel := map[string]string{"app": fmt.Sprintf("green-%s", testID)}

			pods := make(map[string]*corev1.Pod)

			DeferCleanup(func() {
				By("Cleanup: Deleting all test resources")
				deleteCNC(cnc1Name)
				deleteCNC(cnc2Name)
				for _, pod := range pods {
					_ = cs.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
				}
				deleteUDN(blueNs, blueUDN)
				deleteUDN(greenNs, greenUDN)
				deleteCUDN(redCUDN)
				deleteNamespace(cs, blueNs)
				deleteNamespace(cs, redNs)
				deleteNamespace(cs, greenNs)
			})

			// =====================================================================
			// Step 1: Create CNCs with ServiceNetwork enabled
			// =====================================================================
			By("1. Creating CNC-1 selecting blue and red with service connectivity")
			createOrUpdateCNCWithSubnetsAndConnectivity(cnc1Name, redLabel, blueLabel,
				generateConnectSubnets(cs), []string{"PodNetwork", "ServiceNetwork"})

			By("1. Creating CNC-2 selecting blue and green with service connectivity")
			createOrUpdateCNCWithSubnetsAndConnectivity(cnc2Name, nil, cnc2Label,
				generateConnectSubnetsWithCIDRs(cs, cnc2ConnectSubnetIPv4CIDR, cnc2ConnectSubnetIPv4Prefix, cnc2ConnectSubnetIPv6CIDR, cnc2ConnectSubnetIPv6Prefix),
				[]string{"PodNetwork", "ServiceNetwork"})

			// =====================================================================
			// Step 2: Create networks and pods
			// =====================================================================
			By("2. Creating blue namespace, L2 UDN, and pods")
			createUDNNamespaceWithName(cs, blueNs, blueLabel)
			createLayer2PrimaryUDNWithSubnets(cs, blueNs, blueUDN, subnetList("10.128.0.0/16"), subnetList("2014:100:200::0/60"))
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, blueNs, blueUDN), 60*time.Second, time.Second).Should(Succeed())

			bluePodConfig0 := httpServerPodConfig("blue-pod-0", blueNs)
			bluePodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			bluePodConfig0.labels = bluePodLabel
			bluePodConfig1 := httpServerPodConfig("blue-pod-1", blueNs)
			bluePodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			bluePodConfig1.labels = bluePodLabel
			pods["blue-pod-0"] = runUDNPod(cs, blueNs, bluePodConfig0, nil)
			pods["blue-pod-1"] = runUDNPod(cs, blueNs, bluePodConfig1, nil)

			By("2. Creating red namespace, L3 CUDN, and pods")
			createUDNNamespaceWithName(cs, redNs, nil)
			createLayer3PrimaryCUDNWithSubnets(cs, redCUDN, redLabel, subnetList("10.129.0.0/16"), subnetList("2014:100:300::0/60"), redNs)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, redCUDN), 60*time.Second, time.Second).Should(Succeed())

			redPodConfig0 := httpServerPodConfig("red-pod-0", redNs)
			redPodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			redPodConfig0.labels = redPodLabel
			pods["red-pod-0"] = runUDNPod(cs, redNs, redPodConfig0, nil)

			By("2. Creating green namespace, L3 UDN, and pods")
			createUDNNamespaceWithName(cs, greenNs, greenLabel)
			createLayer3PrimaryUDNWithSubnets(cs, greenNs, greenUDN, subnetList("10.130.0.0/16"), subnetList("2014:100:400::0/60"))
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, greenNs, greenUDN), 60*time.Second, time.Second).Should(Succeed())

			greenPodConfig0 := httpServerPodConfig("green-pod-0", greenNs)
			greenPodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			greenPodConfig0.labels = greenPodLabel
			pods["green-pod-0"] = runUDNPod(cs, greenNs, greenPodConfig0, nil)

			// =====================================================================
			// Step 3: Verify CNC annotations
			// =====================================================================
			By("3. Verifying CNC-1 has 2 networks (blue + red)")
			verifyCNCSubnetAnnotationNetworkCount(cnc1Name, 2)
			By("3. Verifying CNC-2 has 2 networks (blue + green)")
			verifyCNCSubnetAnnotationNetworkCount(cnc2Name, 2)

			// =====================================================================
			// Step 4: Create services
			// =====================================================================
			By("4. Creating ClusterIP services")
			svcBlue := e2eservice.CreateServiceSpec("blue-svc", "", false, bluePodLabel)
			svcBlue.Spec.Ports = []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP}}
			familyPolicy := corev1.IPFamilyPolicyPreferDualStack
			svcBlue.Spec.IPFamilyPolicy = &familyPolicy
			createdBlueSvc, err := cs.CoreV1().Services(blueNs).Create(context.Background(), svcBlue, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			svcRed := e2eservice.CreateServiceSpec("red-svc", "", false, redPodLabel)
			svcRed.Spec.Ports = []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP}}
			svcRed.Spec.IPFamilyPolicy = &familyPolicy
			createdRedSvc, err := cs.CoreV1().Services(redNs).Create(context.Background(), svcRed, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			svcGreen := e2eservice.CreateServiceSpec("green-svc", "", false, greenPodLabel)
			svcGreen.Spec.Ports = []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP}}
			svcGreen.Spec.IPFamilyPolicy = &familyPolicy
			createdGreenSvc, err := cs.CoreV1().Services(greenNs).Create(context.Background(), svcGreen, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Wait for ClusterIPs
			Eventually(func(g Gomega) {
				createdBlueSvc, err = cs.CoreV1().Services(blueNs).Get(context.Background(), "blue-svc", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(createdBlueSvc.Spec.ClusterIP).NotTo(BeEmpty())
			}, 30*time.Second, time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				createdRedSvc, err = cs.CoreV1().Services(redNs).Get(context.Background(), "red-svc", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(createdRedSvc.Spec.ClusterIP).NotTo(BeEmpty())
			}, 30*time.Second, time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				createdGreenSvc, err = cs.CoreV1().Services(greenNs).Get(context.Background(), "green-svc", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(createdGreenSvc.Spec.ClusterIP).NotTo(BeEmpty())
			}, 30*time.Second, time.Second).Should(Succeed())

			blueSvcIPs := createdBlueSvc.Spec.ClusterIPs
			redSvcIPs := createdRedSvc.Spec.ClusterIPs
			greenSvcIPs := createdGreenSvc.Spec.ClusterIPs

			// Pod maps for connectivity testing (using one pod per network)
			bluePodMap := map[string]*corev1.Pod{"blue-pod-0": pods["blue-pod-0"]}
			redPodMap := map[string]*corev1.Pod{"red-pod-0": pods["red-pod-0"]}
			greenPodMap := map[string]*corev1.Pod{"green-pod-0": pods["green-pod-0"]}

			// =====================================================================
			// Step 5: Verify cross-network service connectivity
			// =====================================================================
			By("5. Verifying blue-svc reachable from red (via CNC-1)")
			verifyCrossNetworkConnectivity(redPodMap, map[string][]string{"blue-svc": blueSvcIPs}, true)

			By("5. Verifying red-svc reachable from blue (via CNC-1)")
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"red-svc": redSvcIPs}, true)

			By("5. Verifying blue-svc reachable from green (via CNC-2)")
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"blue-svc": blueSvcIPs}, true)

			By("5. Verifying green-svc reachable from blue (via CNC-2)")
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"green-svc": greenSvcIPs}, true)

			By("5. Verifying red-svc NOT reachable from green (non-transitive)")
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"red-svc": redSvcIPs}, false)

			By("5. Verifying green-svc NOT reachable from red (non-transitive)")
			verifyCrossNetworkConnectivity(redPodMap, map[string][]string{"green-svc": greenSvcIPs}, false)

			// =====================================================================
			// Step 6: Delete CNC-1, verify CNC-2 service connectivity survives
			// =====================================================================
			By("6. Deleting CNC-1")
			deleteCNC(cnc1Name)
			Eventually(func() bool {
				_, err := getCNCAnnotations(cnc1Name)
				return err != nil
			}, 60*time.Second, 2*time.Second).Should(BeTrue())

			By("6. Verifying blue-svc still reachable from green (CNC-2 active)")
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"blue-svc": blueSvcIPs}, true)

			By("6. Verifying green-svc still reachable from blue (CNC-2 active)")
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"green-svc": greenSvcIPs}, true)

			By("6. Verifying red-svc NOT reachable from blue (CNC-1 deleted)")
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"red-svc": redSvcIPs}, false)

			By("6. Verifying blue-svc NOT reachable from red (CNC-1 deleted)")
			verifyCrossNetworkConnectivity(redPodMap, map[string][]string{"blue-svc": blueSvcIPs}, false)

			// =====================================================================
			// Step 7: Delete CNC-2, verify all services isolated
			// =====================================================================
			By("7. Deleting CNC-2")
			deleteCNC(cnc2Name)
			Eventually(func() bool {
				_, err := getCNCAnnotations(cnc2Name)
				return err != nil
			}, 60*time.Second, 2*time.Second).Should(BeTrue())

			By("7. Verifying all cross-network service connectivity is disabled")
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"red-svc": redSvcIPs}, false)
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"green-svc": greenSvcIPs}, false)
			verifyCrossNetworkConnectivity(redPodMap, map[string][]string{"blue-svc": blueSvcIPs}, false)
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"blue-svc": blueSvcIPs}, false)

			By("Test completed - Non-transitive service connectivity with overlapping CNCs validated")
		})

		/*
		   Exact same network selection by 2 CNCs with service connectivity:

		   Both CNCs select the exact same pair of networks with
		   ServiceNetwork enabled. This tests the LB cross-referencing
		   concern: when CNC-1 is deleted, its cleanup should not remove LB
		   attachments that CNC-2 still needs.

		   Network topology:
		   - net-A (L3 CUDN): selected by both CNC-1 and CNC-2
		   - net-B (L2 UDN): selected by both CNC-1 and CNC-2

		   Steps:
		   1. Create CNC-1 and CNC-2 both selecting {net-A, net-B} with service connectivity
		   2. Create networks and pods
		   3. Verify both CNCs have both networks
		   4. Create ClusterIP services in both networks
		   5. Verify cross-network service connectivity works
		   6. Delete CNC-1, verify CNC-2 maintains service connectivity
		   7. Delete CNC-2, verify all services isolated
		*/
		It("should maintain service connectivity when both CNCs select the exact same networks", func(ctx SpecContext) {
			// Both CNCs select the same 2 networks with ServiceNetwork
			// Deleting one CNC should not break the other's service connectivity

			testID := rand.String(5)
			cnc1Name := fmt.Sprintf("same-1-%s", testID)
			cnc2Name := fmt.Sprintf("same-2-%s", testID)

			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodes.Items)).To(BeNumerically(">=", 2), "test requires at least 2 schedulable nodes")
			node1Name, node2Name := nodes.Items[0].Name, nodes.Items[1].Name

			netACUDN := fmt.Sprintf("net-a-cudn-%s", testID)
			netBUDN := "net-b-udn"

			netANs := fmt.Sprintf("net-a-ns-%s", testID)
			netBNs := fmt.Sprintf("net-b-ns-%s", testID)

			// Shared label so both CNCs select both networks
			sharedCUDNLabel := map[string]string{"same-cnc-test": testID}
			sharedUDNLabel := map[string]string{"same-cnc-test": testID, "k8s.ovn.org/primary-user-defined-network": ""}

			netAPodLabel := map[string]string{"app": fmt.Sprintf("net-a-%s", testID)}
			netBPodLabel := map[string]string{"app": fmt.Sprintf("net-b-%s", testID)}

			pods := make(map[string]*corev1.Pod)

			DeferCleanup(func() {
				By("Cleanup: Deleting all test resources")
				deleteCNC(cnc1Name)
				deleteCNC(cnc2Name)
				for _, pod := range pods {
					_ = cs.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
				}
				deleteUDN(netBNs, netBUDN)
				deleteCUDN(netACUDN)
				deleteNamespace(cs, netANs)
				deleteNamespace(cs, netBNs)
			})

			// =====================================================================
			// Step 1: Create both CNCs selecting the same networks
			// =====================================================================
			By("1. Creating CNC-1 selecting both networks with service connectivity")
			createOrUpdateCNCWithSubnetsAndConnectivity(cnc1Name, sharedCUDNLabel, sharedUDNLabel,
				generateConnectSubnets(cs), []string{"PodNetwork", "ServiceNetwork"})

			By("1. Creating CNC-2 selecting the same networks with service connectivity (different subnets)")
			createOrUpdateCNCWithSubnetsAndConnectivity(cnc2Name, sharedCUDNLabel, sharedUDNLabel,
				generateConnectSubnetsWithCIDRs(cs, cnc2ConnectSubnetIPv4CIDR, cnc2ConnectSubnetIPv4Prefix, cnc2ConnectSubnetIPv6CIDR, cnc2ConnectSubnetIPv6Prefix),
				[]string{"PodNetwork", "ServiceNetwork"})

			// =====================================================================
			// Step 2: Create networks and pods
			// =====================================================================
			By("2. Creating net-A namespace, L3 CUDN, and pods")
			createUDNNamespaceWithName(cs, netANs, nil)
			createLayer3PrimaryCUDNWithSubnets(cs, netACUDN, sharedCUDNLabel, subnetList("10.128.0.0/16"), subnetList("2014:100:200::0/60"), netANs)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, netACUDN), 60*time.Second, time.Second).Should(Succeed())

			netAPodConfig := httpServerPodConfig("net-a-pod-0", netANs)
			netAPodConfig.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			netAPodConfig.labels = netAPodLabel
			pods["net-a-pod-0"] = runUDNPod(cs, netANs, netAPodConfig, nil)

			By("2. Creating net-B namespace, L2 UDN, and pods")
			createUDNNamespaceWithName(cs, netBNs, sharedUDNLabel)
			createLayer2PrimaryUDNWithSubnets(cs, netBNs, netBUDN, subnetList("10.129.0.0/16"), subnetList("2014:100:300::0/60"))
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, netBNs, netBUDN), 60*time.Second, time.Second).Should(Succeed())

			netBPodConfig := httpServerPodConfig("net-b-pod-0", netBNs)
			netBPodConfig.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			netBPodConfig.labels = netBPodLabel
			pods["net-b-pod-0"] = runUDNPod(cs, netBNs, netBPodConfig, nil)

			// =====================================================================
			// Step 3: Verify both CNCs have both networks
			// =====================================================================
			By("3. Verifying both CNCs have 2 networks")
			verifyCNCSubnetAnnotationNetworkCount(cnc1Name, 2)
			verifyCNCSubnetAnnotationNetworkCount(cnc2Name, 2)

			// =====================================================================
			// Step 4: Create services
			// =====================================================================
			By("4. Creating ClusterIP services")
			svcA := e2eservice.CreateServiceSpec("net-a-svc", "", false, netAPodLabel)
			svcA.Spec.Ports = []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP}}
			familyPolicy := corev1.IPFamilyPolicyPreferDualStack
			svcA.Spec.IPFamilyPolicy = &familyPolicy
			createdSvcA, err := cs.CoreV1().Services(netANs).Create(context.Background(), svcA, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			svcB := e2eservice.CreateServiceSpec("net-b-svc", "", false, netBPodLabel)
			svcB.Spec.Ports = []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP}}
			svcB.Spec.IPFamilyPolicy = &familyPolicy
			createdSvcB, err := cs.CoreV1().Services(netBNs).Create(context.Background(), svcB, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				createdSvcA, err = cs.CoreV1().Services(netANs).Get(context.Background(), "net-a-svc", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(createdSvcA.Spec.ClusterIP).NotTo(BeEmpty())
			}, 30*time.Second, time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				createdSvcB, err = cs.CoreV1().Services(netBNs).Get(context.Background(), "net-b-svc", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(createdSvcB.Spec.ClusterIP).NotTo(BeEmpty())
			}, 30*time.Second, time.Second).Should(Succeed())

			svcAIPs := createdSvcA.Spec.ClusterIPs
			svcBIPs := createdSvcB.Spec.ClusterIPs

			netAPodMap := map[string]*corev1.Pod{"net-a-pod-0": pods["net-a-pod-0"]}
			netBPodMap := map[string]*corev1.Pod{"net-b-pod-0": pods["net-b-pod-0"]}

			// =====================================================================
			// Step 5: Verify cross-network service connectivity works
			// =====================================================================
			By("5. Verifying net-a-svc reachable from net-b")
			verifyCrossNetworkConnectivity(netBPodMap, map[string][]string{"net-a-svc": svcAIPs}, true)

			By("5. Verifying net-b-svc reachable from net-a")
			verifyCrossNetworkConnectivity(netAPodMap, map[string][]string{"net-b-svc": svcBIPs}, true)

			// =====================================================================
			// Step 6: Delete CNC-1, verify CNC-2 maintains service connectivity
			// =====================================================================
			By("6. Deleting CNC-1")
			deleteCNC(cnc1Name)
			Eventually(func() bool {
				_, err := getCNCAnnotations(cnc1Name)
				return err != nil
			}, 60*time.Second, 2*time.Second).Should(BeTrue())

			By("6. Verifying net-a-svc still reachable from net-b (CNC-2 still active)")
			verifyCrossNetworkConnectivity(netBPodMap, map[string][]string{"net-a-svc": svcAIPs}, true)

			By("6. Verifying net-b-svc still reachable from net-a (CNC-2 still active)")
			verifyCrossNetworkConnectivity(netAPodMap, map[string][]string{"net-b-svc": svcBIPs}, true)

			// =====================================================================
			// Step 7: Delete CNC-2, verify all services isolated
			// =====================================================================
			By("7. Deleting CNC-2")
			deleteCNC(cnc2Name)
			Eventually(func() bool {
				_, err := getCNCAnnotations(cnc2Name)
				return err != nil
			}, 60*time.Second, 2*time.Second).Should(BeTrue())

			By("7. Verifying all cross-network service connectivity is disabled")
			verifyCrossNetworkConnectivity(netAPodMap, map[string][]string{"net-b-svc": svcBIPs}, false)
			verifyCrossNetworkConnectivity(netBPodMap, map[string][]string{"net-a-svc": svcAIPs}, false)

			By("Test completed - Same-selector CNC service connectivity validated")
		})
	})

	// Service connectivity validation through CNC (ClusterNetworkConnect).
	// This context validates end-to-end service connectivity when CNC enables ServiceNetwork.
	// Tests cover full lifecycle management of services across connected networks, including:
	// - Network deselection/reselection (via namespace labels and CNC selector updates)
	// - Network deletion/recreation
	// - Service deletion/recreation
	// - Endpoint deletion (OVNLB reject behavior)
	// - CNC lifecycle (deletion and recreation)
	// - Connectivity type toggling (PodNetwork vs ServiceNetwork)
	Context("Service connectivity validation", func() {
		const (
			nodeHostnameKey = "kubernetes.io/hostname"
			servicePort     = 8080
		)

		var (
			// Shared across tests - set in BeforeEach
			node1Name, node2Name string
			testID               string

			// Network and namespace names - set in BeforeEach
			cncName                                 string
			blackCUDN, whiteCUDN, blueUDN, greenUDN string
			blackNs0, blackNs1, whiteNs0, whiteNs1  string
			blueNs, greenNs                         string

			// Labels for CNC selection and pod selectors
			cudnLabel     map[string]string
			pudnLabel     map[string]string
			blackPodLabel map[string]string
			whitePodLabel map[string]string
			bluePodLabel  map[string]string
			greenPodLabel map[string]string

			// Resource maps
			pods     map[string]*corev1.Pod
			services map[string]*corev1.Service

			// Tracking IPs for connectivity tests
			podIPs            map[string][]string
			serviceIPs        map[string][]string
			networkPods       map[string]*corev1.Pod
			networkPodIPs     map[string][]string
			networkServiceIPs map[string][]string

			// Pod configurations for recreation (needed by Test 1)
			bluePodConfig0, bluePodConfig1   podConfiguration
			greenPodConfig0, greenPodConfig1 podConfiguration
		)

		// createClusterIPService creates a ClusterIP service using e2eservice.CreateServiceSpec
		createClusterIPService := func(cs clientset.Interface, namespace, serviceName string, podSelector map[string]string) *corev1.Service {
			svc := e2eservice.CreateServiceSpec(serviceName, "", false, podSelector)
			svc.Spec.Ports = []corev1.ServicePort{{Port: int32(servicePort), Protocol: corev1.ProtocolTCP}}
			familyPolicy := corev1.IPFamilyPolicyPreferDualStack
			svc.Spec.IPFamilyPolicy = &familyPolicy
			createdSvc, err := cs.CoreV1().Services(namespace).Create(context.Background(), svc, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Wait for ClusterIP assignment and return updated service
			Eventually(func(g Gomega) {
				createdSvc, err = cs.CoreV1().Services(namespace).Get(context.Background(), serviceName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(createdSvc.Spec.ClusterIP).NotTo(BeEmpty())
				g.Expect(createdSvc.Spec.ClusterIP).NotTo(Equal("None"))
			}, 30*time.Second, 1*time.Second).Should(Succeed(),
				fmt.Sprintf("waiting for ClusterIP to be assigned to service %s/%s", namespace, serviceName))
			return createdSvc
		}

		// getServiceClusterIPs returns all ClusterIPs for a service (dual-stack support)
		getServiceClusterIPs := func(svc *corev1.Service) []string {
			if len(svc.Spec.ClusterIPs) > 0 {
				return svc.Spec.ClusterIPs
			}
			if svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != "None" {
				return []string{svc.Spec.ClusterIP}
			}
			return nil
		}

		// verifyFullMeshServiceConnectivity verifies that pods from each network can reach IPs in all OTHER networks
		// Reuses verifyCrossNetworkConnectivity from Describe level (port 8080 matches servicePort)
		verifyFullMeshServiceConnectivity := func(
			netPods map[string]*corev1.Pod, // one pod per network
			netServiceIPs map[string][]string,
			expectSuccess bool,
		) {
			for srcNetwork, srcPod := range netPods {
				for dstNetwork, dstIPs := range netServiceIPs {
					if srcNetwork == dstNetwork {
						continue // Skip same network - that's intra-network
					}
					framework.Logf("Testing connectivity: %s -> %s (pod %s -> %d service IP(s))", srcNetwork, dstNetwork, srcPod.Name, len(dstIPs))
					srcPodMap := map[string]*corev1.Pod{srcPod.Name: srcPod}
					targetIPs := map[string][]string{dstNetwork + "-svc": dstIPs}
					verifyCrossNetworkConnectivity(srcPodMap, targetIPs, expectSuccess)
				}
			}
		}

		BeforeEach(func(ctx SpecContext) {
			// Get 2 schedulable nodes for cross-node testing
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodes.Items)).To(BeNumerically(">=", 2), "test requires at least 2 schedulable nodes")
			node1Name, node2Name = nodes.Items[0].Name, nodes.Items[1].Name

			// Generate test ID and resource names
			testID = rand.String(5)
			cncName = generateCNCName()

			// Network names
			blackCUDN = fmt.Sprintf("black-cudn-%s", testID)
			whiteCUDN = fmt.Sprintf("white-cudn-%s", testID)
			blueUDN = "blue-udn"
			greenUDN = "green-udn"

			// Namespace names
			blackNs0 = fmt.Sprintf("black-ns0-%s", testID)
			blackNs1 = fmt.Sprintf("black-ns1-%s", testID)
			whiteNs0 = fmt.Sprintf("white-ns0-%s", testID)
			whiteNs1 = fmt.Sprintf("white-ns1-%s", testID)
			blueNs = fmt.Sprintf("blue-ns-%s", testID)
			greenNs = fmt.Sprintf("green-ns-%s", testID)

			// Labels for CNC selection
			cudnLabel = map[string]string{"cnc-svc-test": testID, "type": "cudn"}
			pudnLabel = map[string]string{"cnc-svc-test": testID, "type": "pudn"}

			// Pod labels for service selectors
			blackPodLabel = map[string]string{"app": "black-" + testID}
			whitePodLabel = map[string]string{"app": "white-" + testID}
			bluePodLabel = map[string]string{"app": "blue-" + testID}
			greenPodLabel = map[string]string{"app": "green-" + testID}

			// Initialize maps
			pods = make(map[string]*corev1.Pod)
			services = make(map[string]*corev1.Service)
			podIPs = make(map[string][]string)
			serviceIPs = make(map[string][]string)
			networkPods = make(map[string]*corev1.Pod)
			networkPodIPs = make(map[string][]string)
			networkServiceIPs = make(map[string][]string)

			// =====================================================================
			// Setup Step 1: Create 2 CUDNs (black L3, white L2) with namespaces and pods
			// =====================================================================
			By("Setup: Creating namespaces for black and white CUDNs")
			createUDNNamespaceWithName(cs, blackNs0, nil)
			createUDNNamespaceWithName(cs, blackNs1, nil)
			createUDNNamespaceWithName(cs, whiteNs0, nil)
			createUDNNamespaceWithName(cs, whiteNs1, nil)

			By("Setup: Creating black CUDN (L3)")
			createLayer3PrimaryCUDNWithSubnets(cs, blackCUDN, cudnLabel, subnetList("10.128.0.0/16"), subnetList("2014:100:200::0/60"), blackNs0, blackNs1)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, blackCUDN), 60*time.Second, time.Second).Should(Succeed())

			By("Setup: Creating white CUDN (L2)")
			createLayer2PrimaryCUDNWithSubnets(cs, whiteCUDN, cudnLabel, subnetList("10.129.0.0/16"), subnetList("2014:100:300::0/60"), whiteNs0, whiteNs1)
			Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, whiteCUDN), 60*time.Second, time.Second).Should(Succeed())

			By("Setup: Creating pods in black CUDN namespaces")
			blackPodConfig0 := httpServerPodConfig("black-pod-0", blackNs0)
			blackPodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			blackPodConfig0.labels = blackPodLabel
			blackPodConfig1 := httpServerPodConfig("black-pod-1", blackNs1)
			blackPodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			blackPodConfig1.labels = blackPodLabel
			pods["black-pod-0"] = runUDNPod(cs, blackNs0, blackPodConfig0, nil)
			pods["black-pod-1"] = runUDNPod(cs, blackNs1, blackPodConfig1, nil)
			podIPs["black-pod-0"] = getPrimaryNetworkPodIPs(blackNs0, "black-pod-0", blackCUDN)
			podIPs["black-pod-1"] = getPrimaryNetworkPodIPs(blackNs1, "black-pod-1", blackCUDN)

			By("Setup: Creating pods in white CUDN namespaces")
			whitePodConfig0 := httpServerPodConfig("white-pod-0", whiteNs0)
			whitePodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			whitePodConfig0.labels = whitePodLabel
			whitePodConfig1 := httpServerPodConfig("white-pod-1", whiteNs1)
			whitePodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			whitePodConfig1.labels = whitePodLabel
			pods["white-pod-0"] = runUDNPod(cs, whiteNs0, whitePodConfig0, nil)
			pods["white-pod-1"] = runUDNPod(cs, whiteNs1, whitePodConfig1, nil)
			podIPs["white-pod-0"] = getPrimaryNetworkPodIPs(whiteNs0, "white-pod-0", whiteCUDN)
			podIPs["white-pod-1"] = getPrimaryNetworkPodIPs(whiteNs1, "white-pod-1", whiteCUDN)

			// =====================================================================
			// Setup Step 2: Create 2 PUDNs (blue L3, green L2) with namespaces and pods
			// =====================================================================
			By("Setup: Creating namespaces for blue and green PUDNs with labels")
			createUDNNamespaceWithName(cs, blueNs, pudnLabel)
			createUDNNamespaceWithName(cs, greenNs, pudnLabel)

			By("Setup: Creating blue UDN (L3)")
			createLayer3PrimaryUDNWithSubnets(cs, blueNs, blueUDN, subnetList("10.130.0.0/16"), subnetList("2014:100:400::0/60"))
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, blueNs, blueUDN), 60*time.Second, time.Second).Should(Succeed())

			By("Setup: Creating green UDN (L2)")
			createLayer2PrimaryUDNWithSubnets(cs, greenNs, greenUDN, subnetList("10.131.0.0/16"), subnetList("2014:100:500::0/60"))
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, greenNs, greenUDN), 60*time.Second, time.Second).Should(Succeed())

			By("Setup: Creating pods in blue UDN namespace")
			bluePodConfig0 = httpServerPodConfig("blue-pod-0", blueNs)
			bluePodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			bluePodConfig0.labels = bluePodLabel
			bluePodConfig1 = httpServerPodConfig("blue-pod-1", blueNs)
			bluePodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			bluePodConfig1.labels = bluePodLabel
			pods["blue-pod-0"] = runUDNPod(cs, blueNs, bluePodConfig0, nil)
			pods["blue-pod-1"] = runUDNPod(cs, blueNs, bluePodConfig1, nil)
			podIPs["blue-pod-0"] = getPrimaryNetworkPodIPs(blueNs, "blue-pod-0", blueUDN)
			podIPs["blue-pod-1"] = getPrimaryNetworkPodIPs(blueNs, "blue-pod-1", blueUDN)

			By("Setup: Creating pods in green UDN namespace")
			greenPodConfig0 = httpServerPodConfig("green-pod-0", greenNs)
			greenPodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			greenPodConfig0.labels = greenPodLabel
			greenPodConfig1 = httpServerPodConfig("green-pod-1", greenNs)
			greenPodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			greenPodConfig1.labels = greenPodLabel
			pods["green-pod-0"] = runUDNPod(cs, greenNs, greenPodConfig0, nil)
			pods["green-pod-1"] = runUDNPod(cs, greenNs, greenPodConfig1, nil)
			podIPs["green-pod-0"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-0", greenUDN)
			podIPs["green-pod-1"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-1", greenUDN)

			// =====================================================================
			// Setup Step 3: Create 1 ClusterIP service per network
			// =====================================================================
			By("Setup: Creating ClusterIP services for each network")
			services["black-svc"] = createClusterIPService(cs, blackNs0, "black-svc", blackPodLabel)
			serviceIPs["black-svc"] = getServiceClusterIPs(services["black-svc"])

			services["white-svc"] = createClusterIPService(cs, whiteNs0, "white-svc", whitePodLabel)
			serviceIPs["white-svc"] = getServiceClusterIPs(services["white-svc"])

			services["blue-svc"] = createClusterIPService(cs, blueNs, "blue-svc", bluePodLabel)
			serviceIPs["blue-svc"] = getServiceClusterIPs(services["blue-svc"])

			services["green-svc"] = createClusterIPService(cs, greenNs, "green-svc", greenPodLabel)
			serviceIPs["green-svc"] = getServiceClusterIPs(services["green-svc"])

			// Organize pods and services by network for full mesh testing
			networkPods["black"] = pods["black-pod-0"]
			networkPods["white"] = pods["white-pod-0"]
			networkPods["blue"] = pods["blue-pod-0"]
			networkPods["green"] = pods["green-pod-0"]

			networkPodIPs["black"] = podIPs["black-pod-1"] // Use pod-1 as target (cross-node)
			networkPodIPs["white"] = podIPs["white-pod-1"]
			networkPodIPs["blue"] = podIPs["blue-pod-1"]
			networkPodIPs["green"] = podIPs["green-pod-1"]

			networkServiceIPs["black"] = serviceIPs["black-svc"]
			networkServiceIPs["white"] = serviceIPs["white-svc"]
			networkServiceIPs["blue"] = serviceIPs["blue-svc"]
			networkServiceIPs["green"] = serviceIPs["green-svc"]

			// =====================================================================
			// Setup Step 4: Verify initial isolation
			// =====================================================================
			By("Setup: Verifying initial isolation - pods cannot reach services in other networks")
			verifyFullMeshServiceConnectivity(networkPods, networkServiceIPs, false)
		})

		AfterEach(func() {
			By("Cleanup: Deleting all test resources")
			deleteCNC(cncName)

			for _, svc := range services {
				_ = cs.CoreV1().Services(svc.Namespace).Delete(context.Background(), svc.Name, metav1.DeleteOptions{})
			}

			for _, pod := range pods {
				_ = cs.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
			}

			deleteUDN(blueNs, blueUDN)
			deleteUDN(greenNs, greenUDN)
			deleteCUDN(blackCUDN)
			deleteCUDN(whiteCUDN)

			deleteNamespace(cs, blackNs0)
			deleteNamespace(cs, blackNs1)
			deleteNamespace(cs, whiteNs0)
			deleteNamespace(cs, whiteNs1)
			deleteNamespace(cs, blueNs)
			deleteNamespace(cs, greenNs)
		})

		/*
		   Test: Full CNC service connectivity lifecycle management
		   Creates 4 networks (2 CUDNs, 2 PUDNs) with pods and services, then validates:

		   --- Setup (Steps 1-4) ---
		   1.  Create 2 CUDNs: black (L3) and white (L2), each with 2 namespaces and pods on different nodes
		   2.  Create 2 PUDNs: blue (L3) and green (L2), each with 2 pods on different nodes
		   3.  Create 1 ClusterIP service per network (selecting both pods in each network)
		   4.  Verify initial isolation - pods cannot reach services in other networks

		   --- CNC Creation and Service Connectivity Enablement (Steps 5-10) ---
		   5.  Create CNC selecting all 4 networks with connectivity: ["PodNetwork"] only
		   6.  Verify CNC annotations are set correctly (subnet allocation, 4 networks)
		   7.  Verify pods CANNOT reach ClusterIP services of other networks (service connectivity not enabled)
		   8.  Update CNC to enable service connectivity: ["PodNetwork", "ServiceNetwork"]
		   9.  Verify pods can reach ClusterIP services of all connected networks (full mesh pod-to-service)
		   10. Verify intra-network service connectivity still works

		   --- Network Deselection Tests (PUDN via namespace label) (Steps 11-20) ---
		   11. Deselect blue PUDN (L3) via namespace label removal
		   12. Verify CNC subnet annotation has 3 networks (blue removed)
		   13. Verify blue service unreachable from other networks (no route)
		   14. Verify other networks (black, white, green) services still reachable
		   15. Deselect green PUDN (L2) via CNC selector update (remove PUDN selector)
		   16. Verify CNC subnet annotation has 2 networks (only CUDNs)
		   17. Verify green service unreachable (no route)
		   18. Verify black and white CUDN services still reachable from each other
		   19. Re-select green PUDN via CNC selector update (add PUDN selector back)
		   20. Re-select blue PUDN via namespace label restoration, verify all 4 services reachable

		   --- Network Deselection Tests (CUDN via CUDN label) (Steps 21-26) ---
		   21. Deselect black CUDN (L3) via CUDN label removal
		   22. Verify CNC subnet annotation has 3 networks (black removed)
		   23. Verify black service unreachable from other networks (no route)
		   24. Verify other network services still reachable
		   25. Re-select black CUDN via CUDN label restoration
		   26. Verify black service reachable again

		   --- Network Deletion Tests (route removal) (Steps 27-33) ---
		   27. Delete blue PUDN (network object) along with its pods
		   28. Verify CNC subnet annotation has 3 networks, blue service unreachable (no route - timeout)
		   29. Delete green PUDN along with its pods, verify CNC has 2 networks, green unreachable
		   30. Verify black and white CUDN services still reachable
		   31. Re-create blue PUDN network and pods (namespace and service were not deleted)
		   32. Re-create green PUDN network and pods (namespace and service were not deleted)
		   33. Verify all 4 network services are reachable again

		   --- Service Lifecycle Tests (Steps 34-37) ---
		   34. Delete the ClusterIP service in blue network (keep pods)
		   35. Verify CIP of blue service is not reachable (service VIP deleted)
		   36. Re-create the ClusterIP service in blue network (same selector)
		   37. Verify CIP of blue service is reachable again from other networks

		   --- Endpoint Deletion Tests (OVNLB reject vs no route) (Steps 38-41) ---
		   38. Delete all pods backing the green service (keep service)
		   39. Verify CIP of green service returns REJECT (no endpoints - OVNLB rejects)
		   40. Re-create pods backing the green service
		   41. Verify CIP of green service works again

		   --- CNC Lifecycle Tests (Steps 42-45) ---
		   42. Delete CNC
		   43. Verify all cross-network service connectivity disabled (no route)
		   44. Re-create CNC with service connectivity enabled
		   45. Verify all cross-network service connectivity restored
		*/
		It("should manage cross-network service connectivity through CNC lifecycle", func(ctx SpecContext) {
			// Setup (Steps 1-4) completed in BeforeEach
			// This test starts from Step 5
			var err error

			// =====================================================================
			// Step 5: Create CNC selecting all 4 networks with connectivity: ["PodNetwork"] only
			// =====================================================================
			By("5. Creating CNC with PodNetwork connectivity only (no service connectivity)")
			createOrUpdateCNCWithConnectivity(cs, cncName, cudnLabel, pudnLabel, []string{"PodNetwork"})

			// =====================================================================
			// Step 6: Verify CNC annotations are set correctly
			// =====================================================================
			By("6. Verifying CNC has both tunnel ID and subnet annotations")
			verifyCNCHasBothAnnotations(cncName)

			By("6. Verifying CNC subnet annotation has 4 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// =====================================================================
			// Step 7: Verify pods CANNOT reach ClusterIP services of other networks
			// (service connectivity not enabled yet)
			// =====================================================================
			By("7. Verifying pods CANNOT reach services in other networks (service connectivity not enabled)")
			verifyFullMeshServiceConnectivity(networkPods, networkServiceIPs, false)

			// =====================================================================
			// Step 8: Update CNC to enable service connectivity
			// =====================================================================
			By("8. Updating CNC to enable ServiceNetwork connectivity")
			createOrUpdateCNCWithConnectivity(cs, cncName, cudnLabel, pudnLabel, []string{"PodNetwork", "ServiceNetwork"})

			// =====================================================================
			// Step 9: Verify pods can reach ClusterIP services of all connected networks
			// =====================================================================
			By("9. Verifying pods CAN reach services in all connected networks (full mesh)")
			verifyFullMeshServiceConnectivity(networkPods, networkServiceIPs, true)

			// =====================================================================
			// Step 10: Verify intra-network service connectivity still works
			// =====================================================================
			By("10. Verifying intra-network service connectivity works")
			// Black pod can reach black service
			blackPodMap := map[string]*corev1.Pod{"black-pod-0": pods["black-pod-0"]}
			blackSvcIPs := map[string][]string{"black-svc": serviceIPs["black-svc"]}
			verifyCrossNetworkConnectivity(blackPodMap, blackSvcIPs, true)

			// Blue pod can reach blue service
			bluePodMap := map[string]*corev1.Pod{"blue-pod-0": pods["blue-pod-0"]}
			blueSvcIPs := map[string][]string{"blue-svc": serviceIPs["blue-svc"]}
			verifyCrossNetworkConnectivity(bluePodMap, blueSvcIPs, true)

			// =====================================================================
			// Step 11: Deselect blue PUDN (L3) via namespace label removal
			// =====================================================================
			By("11. Deselecting blue PUDN via namespace label removal")
			_, err = cs.CoreV1().Namespaces().Patch(context.Background(), blueNs,
				types.MergePatchType, []byte(`{"metadata":{"labels":{"type":null}}}`), metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			// =====================================================================
			// Step 12: Verify CNC subnet annotation now has 3 networks
			// =====================================================================
			By("12. Verifying CNC subnet annotation has 3 networks (blue removed)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)

			// =====================================================================
			// Step 13: Verify blue service unreachable from other networks (no route)
			// =====================================================================
			By("13. Verifying blue service is unreachable from other networks")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"blue-svc": serviceIPs["blue-svc"]}, false)

			// =====================================================================
			// Step 14: Verify other networks (black, white, green) services still reachable
			// =====================================================================
			By("14. Verifying black, white, green services still reachable from each other")
			whitePodMap := map[string]*corev1.Pod{"white-pod-0": pods["white-pod-0"]}
			greenPodMap := map[string]*corev1.Pod{"green-pod-0": pods["green-pod-0"]}
			// black -> white, green
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"white-svc": serviceIPs["white-svc"]}, true)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, true)
			// white -> black, green
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, true)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, true)
			// green -> black, white
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, true)
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"white-svc": serviceIPs["white-svc"]}, true)

			// =====================================================================
			// Step 15: Deselect green PUDN (L2) via CNC selector update (remove PUDN selector)
			// =====================================================================
			By("15. Deselecting green PUDN via CNC selector update (remove PUDN selector)")
			createOrUpdateCNCWithConnectivity(cs, cncName, cudnLabel, nil, []string{"PodNetwork", "ServiceNetwork"})

			// =====================================================================
			// Step 16: Verify CNC subnet annotation now has 2 networks (only CUDNs)
			// =====================================================================
			By("16. Verifying CNC subnet annotation has 2 networks (only CUDNs)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)

			// =====================================================================
			// Step 17: Verify green service unreachable (no route)
			// =====================================================================
			By("17. Verifying green service is unreachable from CUDNs")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, false)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, false)

			// =====================================================================
			// Step 18: Verify black and white CUDN services still reachable from each other
			// =====================================================================
			By("18. Verifying black and white CUDN services still reachable")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"white-svc": serviceIPs["white-svc"]}, true)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, true)

			// =====================================================================
			// Step 19: Re-select green PUDN via CNC selector update (add PUDN selector back)
			// =====================================================================
			By("19. Re-selecting green PUDN via CNC selector update")
			createOrUpdateCNCWithConnectivity(cs, cncName, cudnLabel, pudnLabel, []string{"PodNetwork", "ServiceNetwork"})

			By("19. Verifying CNC subnet annotation has 3 networks (green re-added)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)

			// =====================================================================
			// Step 20: Re-select blue PUDN via namespace label restoration
			// =====================================================================
			By("20. Re-selecting blue PUDN via namespace label restoration")
			_, err = cs.CoreV1().Namespaces().Patch(context.Background(), blueNs,
				types.MergePatchType, []byte(`{"metadata":{"labels":{"type":"pudn"}}}`), metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("20. Verifying CNC subnet annotation has 4 networks (blue re-added)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			By("20. Verifying all 4 network services are reachable again")
			verifyFullMeshServiceConnectivity(networkPods, networkServiceIPs, true)

			// =====================================================================
			// Step 21: Deselect black CUDN (L3) via CUDN label removal
			// =====================================================================
			By("21. Deselecting black CUDN via CUDN label removal")
			_, err = e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", blackCUDN, "type-")
			Expect(err).NotTo(HaveOccurred())

			// =====================================================================
			// Step 22: Verify CNC subnet annotation now has 3 networks
			// =====================================================================
			By("22. Verifying CNC subnet annotation has 3 networks (black removed)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)

			// =====================================================================
			// Step 23: Verify black service unreachable from other networks (no route)
			// =====================================================================
			By("23. Verifying black service is unreachable from other networks")
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, false)
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, false)
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, false)

			// =====================================================================
			// Step 24: Verify other network services still reachable
			// =====================================================================
			By("24. Verifying white, blue, green services still reachable from each other")
			// white -> blue, green
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"blue-svc": serviceIPs["blue-svc"]}, true)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, true)
			// blue -> white, green
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"white-svc": serviceIPs["white-svc"]}, true)
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, true)
			// green -> white, blue
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"white-svc": serviceIPs["white-svc"]}, true)
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"blue-svc": serviceIPs["blue-svc"]}, true)

			// =====================================================================
			// Step 25: Re-select black CUDN via CUDN label restoration
			// =====================================================================
			By("25. Re-selecting black CUDN via CUDN label restoration")
			_, err = e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", blackCUDN, "type=cudn")
			Expect(err).NotTo(HaveOccurred())

			By("25. Verifying CNC subnet annotation has 4 networks (black re-added)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// =====================================================================
			// Step 26: Verify black service reachable again
			// =====================================================================
			By("26. Verifying black service is reachable again")
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, true)
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, true)
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, true)

			// =====================================================================
			// --- Network Deletion Tests (route removal, not just LB cleanup) ---
			// =====================================================================

			// =====================================================================
			// Step 27: Delete blue PUDN (the network object) along with its pods and service
			// =====================================================================
			By("27. Deleting blue PUDN pods")
			err = cs.CoreV1().Pods(blueNs).Delete(context.Background(), pods["blue-pod-0"].Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			err = cs.CoreV1().Pods(blueNs).Delete(context.Background(), pods["blue-pod-1"].Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("27. Deleting blue PUDN network object")
			deleteUDN(blueNs, blueUDN)

			By("27. Waiting for blue pods to be deleted")
			Eventually(func() bool {
				_, err := cs.CoreV1().Pods(blueNs).Get(context.Background(), pods["blue-pod-0"].Name, metav1.GetOptions{})
				return err != nil
			}, 60*time.Second, 1*time.Second).Should(BeTrue())
			Eventually(func() bool {
				_, err := cs.CoreV1().Pods(blueNs).Get(context.Background(), pods["blue-pod-1"].Name, metav1.GetOptions{})
				return err != nil
			}, 60*time.Second, 1*time.Second).Should(BeTrue())

			// =====================================================================
			// Step 28: Verify CNC annotation and service connectivity to blue network
			// =====================================================================
			By("28. Verifying CNC subnet annotation has 3 networks (blue removed)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)

			By("28. Verifying blue service is unreachable (no route exists)")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"blue-svc": serviceIPs["blue-svc"]}, false)

			// =====================================================================
			// Step 29: Delete green PUDN along with its pods and service
			// =====================================================================
			By("29. Deleting green PUDN pods")
			err = cs.CoreV1().Pods(greenNs).Delete(context.Background(), pods["green-pod-0"].Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			err = cs.CoreV1().Pods(greenNs).Delete(context.Background(), pods["green-pod-1"].Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("29. Deleting green PUDN network object")
			deleteUDN(greenNs, greenUDN)

			By("29. Waiting for green pods to be deleted")
			Eventually(func() bool {
				_, err := cs.CoreV1().Pods(greenNs).Get(context.Background(), pods["green-pod-0"].Name, metav1.GetOptions{})
				return err != nil
			}, 60*time.Second, 1*time.Second).Should(BeTrue())
			Eventually(func() bool {
				_, err := cs.CoreV1().Pods(greenNs).Get(context.Background(), pods["green-pod-1"].Name, metav1.GetOptions{})
				return err != nil
			}, 60*time.Second, 1*time.Second).Should(BeTrue())

			By("29. Verifying CNC subnet annotation has 2 networks (only CUDNs remain)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)

			By("29. Verifying green service is unreachable (no route exists)")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, false)

			// =====================================================================
			// Step 30: Verify black and white CUDN services still reachable
			// =====================================================================
			By("30. Verifying black and white CUDN services still reachable")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"white-svc": serviceIPs["white-svc"]}, true)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, true)

			// =====================================================================
			// Step 31: Re-create blue PUDN network and pods (service was not deleted)
			// =====================================================================
			By("31. Recreating blue PUDN network")
			createLayer3PrimaryUDNWithSubnets(cs, blueNs, blueUDN, subnetList("103.103.0.0/16"), subnetList("2014:100:400::0/60"))
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, blueNs, blueUDN), 60*time.Second, time.Second).Should(Succeed())

			By("31. Recreating blue pods")
			pods["blue-pod-0"] = runUDNPod(cs, blueNs, bluePodConfig0, nil)
			pods["blue-pod-1"] = runUDNPod(cs, blueNs, bluePodConfig1, nil)
			podIPs["blue-pod-0"] = getPrimaryNetworkPodIPs(blueNs, "blue-pod-0", blueUDN)
			podIPs["blue-pod-1"] = getPrimaryNetworkPodIPs(blueNs, "blue-pod-1", blueUDN)

			// Update pod maps
			networkPods["blue"] = pods["blue-pod-0"]
			bluePodMap = map[string]*corev1.Pod{"blue-pod-0": pods["blue-pod-0"]}

			// =====================================================================
			// Step 32: Re-create green PUDN network and pods (service was not deleted)
			// =====================================================================
			By("32. Recreating green PUDN network")
			createLayer2PrimaryUDNWithSubnets(cs, greenNs, greenUDN, subnetList("104.104.0.0/16"), subnetList("2014:100:500::0/60"))
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, greenNs, greenUDN), 60*time.Second, time.Second).Should(Succeed())

			By("32. Recreating green pods")
			pods["green-pod-0"] = runUDNPod(cs, greenNs, greenPodConfig0, nil)
			pods["green-pod-1"] = runUDNPod(cs, greenNs, greenPodConfig1, nil)
			podIPs["green-pod-0"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-0", greenUDN)
			podIPs["green-pod-1"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-1", greenUDN)

			// Update pod maps
			networkPods["green"] = pods["green-pod-0"]
			greenPodMap = map[string]*corev1.Pod{"green-pod-0": pods["green-pod-0"]}

			// =====================================================================
			// Step 33: Verify all 4 network services are reachable again
			// =====================================================================
			By("33. Verifying CNC subnet annotation has 4 networks (blue and green re-added)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			By("33. Verifying all 4 network services are reachable again")
			verifyFullMeshServiceConnectivity(networkPods, networkServiceIPs, true)

			// =====================================================================
			// --- Service Lifecycle Tests ---
			// =====================================================================

			// =====================================================================
			// Step 34: Delete the ClusterIP service in blue network (keep pods)
			// =====================================================================
			By("34. Deleting blue ClusterIP service (keeping pods)")
			err = cs.CoreV1().Services(blueNs).Delete(context.Background(), "blue-svc", metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			// =====================================================================
			// Step 35: Verify CIP of blue service is not reachable (no route to deleted service VIP)
			// =====================================================================
			By("35. Verifying blue service CIP is not reachable after service deletion")
			// The service VIP no longer exists, so connections should fail
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"blue-svc": serviceIPs["blue-svc"]}, false)

			// =====================================================================
			// Step 36: Re-create the ClusterIP service in blue network (same selector)
			// =====================================================================
			By("36. Re-creating blue ClusterIP service")
			services["blue-svc"] = createClusterIPService(cs, blueNs, "blue-svc", bluePodLabel)
			// Update service IPs (ClusterIP may change)
			serviceIPs["blue-svc"] = getServiceClusterIPs(services["blue-svc"])
			networkServiceIPs["blue"] = serviceIPs["blue-svc"]

			// =====================================================================
			// Step 37: Verify CIP of blue service is reachable again from other networks
			// =====================================================================
			By("37. Verifying blue service is reachable again after recreation")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"blue-svc": serviceIPs["blue-svc"]}, true)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"blue-svc": serviceIPs["blue-svc"]}, true)
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"blue-svc": serviceIPs["blue-svc"]}, true)

			// =====================================================================
			// --- Endpoint Deletion Tests (OVNLB reject vs no route) ---
			// =====================================================================

			// =====================================================================
			// Step 38: Delete all pods backing the green service (keep service)
			// =====================================================================
			By("38. Deleting pods backing green service (keeping service)")
			err = cs.CoreV1().Pods(greenNs).Delete(context.Background(), "green-pod-0", metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			err = cs.CoreV1().Pods(greenNs).Delete(context.Background(), "green-pod-1", metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Wait for pods to be fully deleted
			Eventually(func() bool {
				_, err := cs.CoreV1().Pods(greenNs).Get(context.Background(), "green-pod-0", metav1.GetOptions{})
				return err != nil
			}, 30*time.Second, 1*time.Second).Should(BeTrue(), "green-pod-0 should be deleted")
			Eventually(func() bool {
				_, err := cs.CoreV1().Pods(greenNs).Get(context.Background(), "green-pod-1", metav1.GetOptions{})
				return err != nil
			}, 30*time.Second, 1*time.Second).Should(BeTrue(), "green-pod-1 should be deleted")

			// =====================================================================
			// Step 39: Verify CIP of green service is routable but returns REJECT
			// =====================================================================
			By("39. Verifying green service CIP returns REJECT (no endpoints, OVNLB rejects)")
			// The service still exists and route is there, but OVN LB has no backends -> reject
			for _, greenIP := range serviceIPs["green-svc"] {
				Eventually(func() string {
					return checkConnectivityFailureType(blackNs0, "black-pod-0", greenIP)
				}, 30*time.Second, 2*time.Second).Should(Equal("reject"),
					fmt.Sprintf("green service %s should return reject (no endpoints)", greenIP))
			}

			// =====================================================================
			// Step 40: Re-create pods backing the green service
			// =====================================================================
			By("40. Re-creating pods backing green service")
			pods["green-pod-0"] = runUDNPod(cs, greenNs, greenPodConfig0, nil)
			pods["green-pod-1"] = runUDNPod(cs, greenNs, greenPodConfig1, nil)
			podIPs["green-pod-0"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-0", greenUDN)
			podIPs["green-pod-1"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-1", greenUDN)
			networkPods["green"] = pods["green-pod-0"]
			greenPodMap = map[string]*corev1.Pod{"green-pod-0": pods["green-pod-0"]}

			// =====================================================================
			// Step 41: Verify CIP of green service works again
			// =====================================================================
			By("41. Verifying green service is reachable again after pod recreation")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, true)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, true)
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, true)

			// =====================================================================
			// --- CNC Lifecycle Tests ---
			// =====================================================================

			// =====================================================================
			// Step 42: Delete CNC
			// =====================================================================
			By("42. Deleting CNC")
			deleteCNC(cncName)

			// Wait for CNC to be deleted
			Eventually(func() bool {
				_, err := e2ekubectl.RunKubectl("", "get", "clusternetworkconnect", cncName)
				return err != nil
			}, 30*time.Second, 1*time.Second).Should(BeTrue(), "CNC should be deleted")

			// =====================================================================
			// Step 43: Verify all cross-network service connectivity disabled (no route)
			// =====================================================================
			By("43. Verifying all cross-network service connectivity is disabled after CNC deletion")
			// black -> other networks should fail
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"white-svc": serviceIPs["white-svc"]}, false)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"blue-svc": serviceIPs["blue-svc"]}, false)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, false)
			// white -> other networks should fail
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, false)
			// blue -> other networks should fail
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, false)
			// green -> other networks should fail
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, false)

			// =====================================================================
			// Step 44: Re-create CNC with service connectivity enabled
			// =====================================================================
			By("44. Re-creating CNC with service connectivity enabled")
			createOrUpdateCNCWithConnectivity(cs, cncName, cudnLabel, pudnLabel, []string{"PodNetwork", "ServiceNetwork"})

			By("44. Verifying CNC has both annotations")
			verifyCNCHasBothAnnotations(cncName)

			By("44. Verifying CNC subnet annotation has 4 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// =====================================================================
			// Step 45: Verify all cross-network service connectivity restored
			// =====================================================================
			By("45. Verifying all cross-network service connectivity is restored")
			verifyFullMeshServiceConnectivity(networkPods, networkServiceIPs, true)

			By("Steps 1-45 completed successfully - full service connectivity lifecycle validated")
		})

		/*
		   Test: Connectivity type toggling (PodNetwork vs ServiceNetwork)
		   Creates 4 networks (2 CUDNs, 2 PUDNs) with pods and services, then validates
		   independent control of pod and service connectivity through CNC.

		   Steps:
		   1-3.  Setup: Create 4 networks with pods and services
		   4.    Verify initial isolation
		   5-6.  Create CNC with ["PodNetwork"] only, verify annotations
		   7.    Verify pod-to-pod works cross-network and within same network
		   8.    Verify pod-to-service FAILS cross-network (service connectivity not enabled)
		   9.    Update CNC: ["PodNetwork", "ServiceNetwork"]
		   10.   Verify pod-to-service works cross-network (full mesh)
		   11.   Verify pod-to-pod still works
		   12.   Update CNC: ["ServiceNetwork"] only (disable PodNetwork)
		   13.   Verify service connectivity still works cross-network
		   14.   Verify pod-to-pod FAILS cross-network
		   15.   Verify pod-to-pod still works WITHIN same network
		   16.   Update CNC: ["PodNetwork"] only (swap back)
		   17.   Verify pod-to-pod works cross-network again
		   18.   Verify service connectivity FAILS cross-network, but works within same network
		*/
		It("pod and service connectivity through CNC connectivity types toggling", func() {
			// Setup (Steps 1-4) completed in BeforeEach
			// This test starts from Step 5

			// Pod maps for targeted tests (derived from Context-level pods map)
			blackPodMap := map[string]*corev1.Pod{"black-pod-0": pods["black-pod-0"]}
			whitePodMap := map[string]*corev1.Pod{"white-pod-0": pods["white-pod-0"]}
			bluePodMap := map[string]*corev1.Pod{"blue-pod-0": pods["blue-pod-0"]}
			greenPodMap := map[string]*corev1.Pod{"green-pod-0": pods["green-pod-0"]}

			// =====================================================================
			// Step 5: Create CNC with connectivity: ["PodNetwork"] only
			// =====================================================================
			By("5. Creating CNC with PodNetwork connectivity only")
			createOrUpdateCNCWithConnectivity(cs, cncName, cudnLabel, pudnLabel, []string{"PodNetwork"})

			// =====================================================================
			// Step 6: Verify CNC annotations are set correctly
			// =====================================================================
			By("6. Verifying CNC has both annotations")
			verifyCNCHasBothAnnotations(cncName)

			By("6. Verifying CNC subnet annotation has 4 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// =====================================================================
			// Step 7: Verify all pods can talk cross network and within same network
			// =====================================================================
			By("7. Verifying pod-to-pod connectivity works cross-network (PodNetwork enabled)")
			// Cross-network pod-to-pod
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"white-pod": networkPodIPs["white"]}, true)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"blue-pod": networkPodIPs["blue"]}, true)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"green-pod": networkPodIPs["green"]}, true)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"black-pod": networkPodIPs["black"]}, true)
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"black-pod": networkPodIPs["black"]}, true)
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"black-pod": networkPodIPs["black"]}, true)

			By("7. Verifying pod-to-pod connectivity works within same network")
			// Same network pod-to-pod (black-pod-0 -> black-pod-1)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"black-pod-1": podIPs["black-pod-1"]}, true)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"white-pod-1": podIPs["white-pod-1"]}, true)
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"blue-pod-1": podIPs["blue-pod-1"]}, true)
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"green-pod-1": podIPs["green-pod-1"]}, true)

			// =====================================================================
			// Step 8: Verify pods CANNOT reach ClusterIP services of other networks
			// =====================================================================
			By("8. Verifying pods CANNOT reach services in other networks (service connectivity not enabled)")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"white-svc": serviceIPs["white-svc"]}, false)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"blue-svc": serviceIPs["blue-svc"]}, false)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, false)

			// =====================================================================
			// Step 9: Update CNC to enable service connectivity
			// =====================================================================
			By("9. Updating CNC to enable ServiceNetwork connectivity")
			createOrUpdateCNCWithConnectivity(cs, cncName, cudnLabel, pudnLabel, []string{"PodNetwork", "ServiceNetwork"})

			// =====================================================================
			// Step 10: Verify pods can reach ClusterIP services of all connected networks
			// =====================================================================
			By("10. Verifying pods CAN reach services in all connected networks (full mesh)")
			verifyFullMeshServiceConnectivity(networkPods, networkServiceIPs, true)

			// =====================================================================
			// Step 11: Verify all pods can talk cross network and within same network
			// =====================================================================
			By("11. Verifying pod-to-pod still works cross-network")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"white-pod": networkPodIPs["white"]}, true)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"blue-pod": networkPodIPs["blue"]}, true)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"green-pod": networkPodIPs["green"]}, true)

			By("11. Verifying pod-to-pod still works within same network")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"black-pod-1": podIPs["black-pod-1"]}, true)
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"blue-pod-1": podIPs["blue-pod-1"]}, true)

			// =====================================================================
			// Step 12: Update CNC to enable only service connectivity
			// =====================================================================
			By("12. Updating CNC to enable ONLY ServiceNetwork (disabling PodNetwork)")
			createOrUpdateCNCWithConnectivity(cs, cncName, cudnLabel, pudnLabel, []string{"ServiceNetwork"})

			// =====================================================================
			// Step 13: Ensure ClusterIP service cross network works
			// =====================================================================
			By("13. Verifying service connectivity still works cross-network")
			verifyFullMeshServiceConnectivity(networkPods, networkServiceIPs, true)

			// =====================================================================
			// Step 14: Ensure direct pod2pod cross network doesn't work
			// =====================================================================
			By("14. Verifying pod-to-pod FAILS cross-network (PodNetwork disabled)")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"white-pod": networkPodIPs["white"]}, false)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"blue-pod": networkPodIPs["blue"]}, false)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"green-pod": networkPodIPs["green"]}, false)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"black-pod": networkPodIPs["black"]}, false)

			// =====================================================================
			// Step 15: Ensure direct pod2pod in same network works
			// =====================================================================
			By("15. Verifying pod-to-pod still works WITHIN same network")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"black-pod-1": podIPs["black-pod-1"]}, true)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"white-pod-1": podIPs["white-pod-1"]}, true)
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"blue-pod-1": podIPs["blue-pod-1"]}, true)
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"green-pod-1": podIPs["green-pod-1"]}, true)

			// =====================================================================
			// Step 16: Swap service connectivity with podnetwork in CNC update
			// =====================================================================
			By("16. Updating CNC to enable ONLY PodNetwork (disabling ServiceNetwork)")
			createOrUpdateCNCWithConnectivity(cs, cncName, cudnLabel, pudnLabel, []string{"PodNetwork"})

			// =====================================================================
			// Step 17: Ensure direct pod2pod works in cross network
			// =====================================================================
			By("17. Verifying pod-to-pod works cross-network again")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"white-pod": networkPodIPs["white"]}, true)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"blue-pod": networkPodIPs["blue"]}, true)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"green-pod": networkPodIPs["green"]}, true)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"black-pod": networkPodIPs["black"]}, true)
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"green-pod": networkPodIPs["green"]}, true)

			// =====================================================================
			// Step 18: Ensure service connectivity cross network fails but works within same network
			// =====================================================================
			By("18. Verifying service connectivity FAILS cross-network (ServiceNetwork disabled)")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"white-svc": serviceIPs["white-svc"]}, false)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"blue-svc": serviceIPs["blue-svc"]}, false)
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, false)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, false)

			By("18. Verifying service connectivity still works WITHIN same network")
			verifyCrossNetworkConnectivity(blackPodMap, map[string][]string{"black-svc": serviceIPs["black-svc"]}, true)
			verifyCrossNetworkConnectivity(whitePodMap, map[string][]string{"white-svc": serviceIPs["white-svc"]}, true)
			verifyCrossNetworkConnectivity(bluePodMap, map[string][]string{"blue-svc": serviceIPs["blue-svc"]}, true)
			verifyCrossNetworkConnectivity(greenPodMap, map[string][]string{"green-svc": serviceIPs["green-svc"]}, true)

			By("Steps 1-18 completed successfully - connectivity type switching validated")
		})

		/*
		   Test: Service protocol update triggers CNC reconciliation
		   Validates that updating a service to add a new protocol (UDP alongside TCP)
		   triggers the CNC controller to reconcile and add the new _cluster LB to the LBG.

		   Steps:
		   1. Create CNC with ["PodNetwork", "ServiceNetwork"]
		   2. Verify blue service reachable from other networks (TCP)
		   3. Update blue service to add a UDP port (new protocol -> new _cluster LB)
		   4. Verify blue service TCP still works (proves CNC reconciled without breaking existing LBs)
		   5. Verify blue service UDP works from another network via nc -u
		*/
		It("should maintain cross-network service connectivity after service protocol update", func() {
			// Step 1: Create CNC with service connectivity
			By("1. Creating CNC with PodNetwork and ServiceNetwork")
			createOrUpdateCNCWithConnectivity(cs, cncName, cudnLabel, pudnLabel,
				[]string{"PodNetwork", "ServiceNetwork"})

			By("1. Verifying CNC annotations")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// Step 2: Verify blue service is reachable from other networks (TCP)
			By("2. Verifying blue service reachable from other networks (TCP)")
			blackPod := pods["black-pod-0"]
			blueSvcIPs := map[string][]string{"blue-svc": serviceIPs["blue-svc"]}
			verifyCrossNetworkConnectivity(map[string]*corev1.Pod{blackPod.Name: blackPod}, blueSvcIPs, true)

			// Step 3: Update blue service to add a UDP port
			// This causes the services controller to create a new _cluster LB for UDP.
			// The CNC controller must react to the service update (protocol set changed)
			// and add the new LB to the CNC's LoadBalancerGroup.
			By("3. Updating blue service to add UDP port")
			blueSvc, err := cs.CoreV1().Services(blueNs).Get(context.Background(), "blue-svc", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			// When multiple ports exist, all ports must be named
			blueSvc.Spec.Ports[0].Name = "tcp"
			blueSvc.Spec.Ports = append(blueSvc.Spec.Ports, corev1.ServicePort{
				Name: "udp", Port: 9090, TargetPort: intstr.FromInt32(9090), Protocol: corev1.ProtocolUDP,
			})
			_, err = cs.CoreV1().Services(blueNs).Update(context.Background(), blueSvc, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Step 4: Verify blue service TCP still works after protocol update
			// This proves the CNC was reconciled after the service update and the LBG
			// is intact (the TCP _cluster LB was not removed during reconciliation).
			By("4. Verifying blue service TCP still works after protocol update")
			verifyCrossNetworkConnectivity(map[string]*corev1.Pod{blackPod.Name: blackPod}, blueSvcIPs, true)

			// Step 5: Verify blue service UDP works from another network
			// Use nc -u from a black pod to the blue service ClusterIP on port 9090.
			// The netexec UDP handler responds to "hostname" with the pod's hostname.
			By("5. Verifying blue service UDP works from black network (nc -u)")
			for _, blueIP := range serviceIPs["blue-svc"] {
				Eventually(func() bool {
					cmd := fmt.Sprintf("echo hostname | nc -u -w1 %s 9090", blueIP)
					stdout, err := e2ekubectl.RunKubectl(blackPod.Namespace, "exec", blackPod.Name, "--",
						"/bin/sh", "-c", cmd)
					if err != nil {
						framework.Logf("UDP check from %s to %s:9090 failed: %v", blackPod.Name, blueIP, err)
						return false
					}
					framework.Logf("UDP check from %s to %s:9090 returned: %q", blackPod.Name, blueIP, stdout)
					return len(strings.TrimSpace(stdout)) > 0
				}, 5*time.Second, 1*time.Second).Should(BeTrue(),
					fmt.Sprintf("UDP connectivity from %s to blue service %s:9090", blackPod.Name, blueIP))
			}
		})
	})
})

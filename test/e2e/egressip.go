// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/dsl/table"
	"github.com/onsi/gomega"
	"golang.org/x/sync/errgroup"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	"k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	utilnet "k8s.io/utils/net"
)

const (
	OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME     = "OVN_EGRESSIP_HEALTHCHECK_PORT"
	DEFAULT_OVN_EGRESSIP_GRPC_HEALTHCHECK_PORT = "9107"
	OVN_EGRESSIP_LEGACY_HEALTHCHECK_PORT_ENV   = "0" // the env value to enable legacy health check
	OVN_EGRESSIP_LEGACY_HEALTHCHECK_PORT       = "9" // the actual port used by legacy health check
	secondaryIPV4Subnet                        = "10.10.10.0/24"
	secondaryIPV6Subnet                        = "2001:db8:abcd:1234::/64"
	secondaryNetworkName                       = "secondarynetwork"
	aghHostNetexecSrcIPPath                    = "/clientip"
)

func labelNodeForEgress(f *framework.Framework, nodeName string) {
	framework.Logf("Labeling node %s with k8s.ovn.org/egress-assignable", nodeName)
	e2enode.AddOrUpdateLabelOnNode(f.ClientSet, nodeName, "k8s.ovn.org/egress-assignable", "dummy")
}

func unlabelNodeForEgress(f *framework.Framework, nodeName string) {
	framework.Logf("Removing label k8s.ovn.org/egress-assignable from node %s", nodeName)
	e2enode.RemoveLabelOffNode(f.ClientSet, nodeName, "k8s.ovn.org/egress-assignable")
}

type egressNodeAvailabilityHandler interface {
	// Enable node availability for egress
	Enable(nodeName string)
	// Disable node availability for egress
	Disable(nodeName string)
	// Restore a node to its original availability for egress
	Restore(nodeName string)
}

type egressNodeAvailabilityHandlerViaLabel struct {
	F *framework.Framework
}

func (h *egressNodeAvailabilityHandlerViaLabel) Enable(nodeName string) {
	labelNodeForEgress(h.F, nodeName)
}

func (h *egressNodeAvailabilityHandlerViaLabel) Disable(nodeName string) {
	unlabelNodeForEgress(h.F, nodeName)
}

func (h *egressNodeAvailabilityHandlerViaLabel) Restore(nodeName string) {
	gomega.Expect(h.F.ClientSet).NotTo(gomega.BeNil())
	unlabelNodeForEgress(h.F, nodeName)
}

type egressNodeAvailabilityHandlerViaHealthCheck struct {
	F              *framework.Framework
	Legacy         bool
	modeWasLegacy  bool
	modeWasChecked bool
	oldGRPCPort    string
}

// checkMode checks what kind of update this handler needs to do to set the
// egress ip health check working in the mode we want or back to the mode it was
// originally working at. Returns the port the health check environment value
// needs to be set at, the actual port the health check needs to be running on
// and whether a value change is needed in the environment to change the mode.
func (h *egressNodeAvailabilityHandlerViaHealthCheck) checkMode(restore bool) (string, string, bool) {
	if restore && !h.modeWasChecked {
		// we havent checked what was the original mode yet so there is nothing
		// to restore to.
		return "", "", false
	}
	ovnKubeNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	framework.Logf("Checking the ovnkube-node and ovnkube-cluster-manager healthcheck ports in use")
	portNode := getTemplateContainerEnv(ovnKubeNamespace, "daemonset/ovnkube-node", getNodeContainerName(), OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME)
	portControlPlane := getTemplateContainerEnv(ovnKubeNamespace, "deployment/ovnkube-control-plane", "ovnkube-cluster-manager", OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME)

	wantLegacy := (h.Legacy && !restore) || (h.modeWasLegacy && restore)
	isLegacy := portNode == "" || portNode == OVN_EGRESSIP_LEGACY_HEALTHCHECK_PORT_ENV
	outOfSync := portNode != portControlPlane

	if !h.modeWasChecked {
		h.modeWasChecked = true
		h.modeWasLegacy = isLegacy
		h.oldGRPCPort = portNode
	}

	if wantLegacy {
		// we want to change to legacy health check if we are not already in
		// that mode or if node and control plane are out of sync
		return OVN_EGRESSIP_LEGACY_HEALTHCHECK_PORT_ENV, OVN_EGRESSIP_LEGACY_HEALTHCHECK_PORT, !isLegacy || outOfSync
	}
	if !wantLegacy && !isLegacy {
		// we are is GRPC health check mode as we want but reset if node and
		// control plane are out of sync
		return portNode, portNode, outOfSync
	}
	// we are in legacy health check mode and we want to change to GRPC mode.
	// use the original GRPC port if restoring
	var port string
	if restore {
		port = h.oldGRPCPort
	} else {
		port = DEFAULT_OVN_EGRESSIP_GRPC_HEALTHCHECK_PORT
	}
	return port, port, true
}

// setMode reconfigures ovnkube, if needed, to use the health check type, either
// GRPC or Legacy, as indicated by the h.Legacy setting. Additionaly it can
// configure an iptables rule to drop the health check traffic on the given
// node. If restore is true, it will restore the configuration to the one first
// observed.
func (h *egressNodeAvailabilityHandlerViaHealthCheck) setMode(nodeName string, reject, restore bool) {
	portEnv, port, changeEnv := h.checkMode(restore)
	if changeEnv {
		framework.Logf("Updating ovnkube to use health check on port %s (0 is legacy, non 0 is GRPC)", portEnv)
		ovnKubeNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
		setEnv := map[string]string{OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME: portEnv}
		setUnsetTemplateContainerEnv(h.F.ClientSet, ovnKubeNamespace, "daemonset/ovnkube-node", getNodeContainerName(), setEnv)
		setUnsetTemplateContainerEnv(h.F.ClientSet, ovnKubeNamespace, "deployment/ovnkube-control-plane", "ovnkube-cluster-manager", setEnv)
	}
	if port != "" {
		op := "Allow"
		if reject {
			op = "Drop"
		}
		framework.Logf("%s health check traffic on port %s on node %s", op, port, nodeName)
		allowOrDropNodeInputTrafficOnPort(op, nodeName, "tcp", port)
	}
}

func (h *egressNodeAvailabilityHandlerViaHealthCheck) Enable(nodeName string) {
	labelNodeForEgress(h.F, nodeName)
	h.setMode(nodeName, false, false)
}

func (h *egressNodeAvailabilityHandlerViaHealthCheck) Restore(nodeName string) {
	h.setMode(nodeName, false, true)
	unlabelNodeForEgress(h.F, nodeName)
	h.modeWasChecked = false
}

func (h *egressNodeAvailabilityHandlerViaHealthCheck) Disable(nodeName string) {
	// keep the node labeled but block helath check traffic
	h.setMode(nodeName, true, false)
}

type node struct {
	name   string
	nodeIP string
	port   uint16
}

func getLastLogLine(data string) string {
	data = strings.TrimSuffix(data, "\n")
	logLines := strings.Split(data, "\n")
	if len(logLines) == 0 {
		return ""
	}
	return logLines[len(logLines)-1]
}

// checks if the given IP is found. If there are multiple lines, only consider the last line.
// The last line is expected to be in host:port format (e.g. "172.18.0.200:38137" or "[fc00::c8]:38137").
func containsIPInLastEntry(data, ip string) bool {
	lastLine := getLastLogLine(data)
	host, _, err := net.SplitHostPort(lastLine)
	if err != nil {
		return false
	}
	return host == ip
}

// support for agnhost image is limited to netexec command
func isSupportedAgnhostForEIP(externalContainer infraapi.ExternalContainer) bool {
	if externalContainer.Image != images.AgnHost() {
		return false
	}
	if !util.SliceHasStringItem(externalContainer.CmdArgs, "netexec") {
		return false
	}
	return true
}

// createEIPManifestWithNodeSelector extends createEIPManifest with an
// egressNodeSelector stanza.  Pass nil to omit the field entirely (identical
// to calling createEIPManifest directly, which triggers CRD defaulting).
func createEIPManifestWithNodeSelector(name string, podLabel, namespaceLabel map[string]string, nodeSelector *metav1.LabelSelector, egressIPs ...string) string {
	base := createEIPManifest(name, podLabel, namespaceLabel, egressIPs...)
	if nodeSelector == nil {
		return base
	}
	nodeSelectorYAML := "    egressNodeSelector:\n"
	if len(nodeSelector.MatchLabels) > 0 {
		nodeSelectorYAML += "        matchLabels:\n"
		for k, v := range nodeSelector.MatchLabels {
			nodeSelectorYAML += fmt.Sprintf("            %s: %s\n", k, v)
		}
	}
	if len(nodeSelector.MatchExpressions) > 0 {
		nodeSelectorYAML += "        matchExpressions:\n"
		for _, expr := range nodeSelector.MatchExpressions {
			nodeSelectorYAML += fmt.Sprintf("        - key: %s\n          operator: %s\n", expr.Key, string(expr.Operator))
			if len(expr.Values) > 0 {
				nodeSelectorYAML += "          values:\n"
				for _, v := range expr.Values {
					nodeSelectorYAML += fmt.Sprintf("          - %s\n", v)
				}
			}
		}
	}
	return base + nodeSelectorYAML
}

// Create EgressIP Manifest
func createEIPManifest(name string, podLabel, namespaceLabel map[string]string, egressIPs ...string) string {
	var ipsYAML string
	for _, ip := range egressIPs {
		// Quote the IP so IPv6 addresses (which contain colons and may end in
		// "::") are parsed as string scalars and not misread as YAML mappings.
		ipsYAML += fmt.Sprintf("\n    - %q", ip)
	}

	var podLabelYaml string
	for k, v := range podLabel {
		podLabelYaml = fmt.Sprintf("%s: %s", k, v)
	}

	var namespaceLabelYaml string
	for k, v := range namespaceLabel {
		namespaceLabelYaml = fmt.Sprintf("%s: %s", k, v)
	}
	egressIPConfig := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: %s
spec:
    egressIPs:%s
    podSelector:
        matchLabels:
            %s
    namespaceSelector:
        matchLabels:
            %s
`, name, ipsYAML, podLabelYaml, namespaceLabelYaml)

	return egressIPConfig

}

// targetHostNetworkContainerAndTest targets the internal host network test container from
// our test pods, collects its logs and verifies that the logs have traces
// of the `verifyIPs` provided. We need to target the test
// container multiple times until we verify that all IPs provided by
// `verifyIPs` have been verified. This is done by passing it a slice of
// verifyIPs and removing each item when it has been found. This function is
// wrapped in a `wait.PollImmediate` which results in the fact that it only
// passes once verifyIPs is of length 0. targetExternalContainerAndTest
// initiates only a single connection at a time, sequentially, hence: we
// perform one connection attempt, check that the IP seen is expected,
// remove it from the list of verifyIPs, see that it's length is not 0 and
// retry again. We do this until all IPs have been seen. If that never
// happens (because of a bug) the test fails.
func targetHostNetworkContainerAndTest(targetNode node, podNamespace, podName string, expectSuccess bool, verifyIPs []string) wait.ConditionFunc {
	// we only know how to extract src IP from agnhost host configured with netexec and curling path /clientip to return
	// the src IP

	return func() (bool, error) {
		clientStdOut, err := e2ekubectl.RunKubectl(podNamespace, "exec", podName, "--", "curl", "--connect-timeout", "2",
			net.JoinHostPort(targetNode.nodeIP, fmt.Sprintf("%d", targetNode.port))+aghHostNetexecSrcIPPath)
		if err != nil {
			if !expectSuccess {
				// curl should timeout with a string containing this error, and this should be the case if we expect a failure
				if !strings.Contains(err.Error(), "Connection timed out") {
					framework.Logf("the test expected netserver container to not be able to connect, but it did with another error, err : %v", err)
					return false, nil
				}
				return true, nil
			}
			return false, nil
		}
		// we determine the src IP based on the target image
		// agnhost netexec will return the source IP as payload
		for _, expectedIP := range verifyIPs {
			if containsIPInLastEntry(clientStdOut, expectedIP) {
				verifyIPs = util.RemoveItemFromSliceUnstable(verifyIPs, expectedIP)
			}
		}

		if len(verifyIPs) != 0 && expectSuccess {
			framework.Logf("the test external container did not have any trace of the IPs: %v being logged, last logs: %s", verifyIPs, getLastLogLine(clientStdOut))
			return false, nil
		}
		if len(verifyIPs) != 0 && expectSuccess {
			framework.Logf("the test host network container did not have any trace of the IPs: %v being logged, last logs: %s", verifyIPs, getLastLogLine(clientStdOut))
			return false, nil
		}
		if !expectSuccess && len(verifyIPs) == 0 {
			framework.Logf("the test host network did have a trace of the IPs: %v being logged, it should not have, last logs: %s", verifyIPs, getLastLogLine(clientStdOut))
			return false, nil
		}
		return true, nil
	}
}

// targetExternalContainerAndTest targets the external test container from
// our test pods, collects its logs and verifies that the logs have traces
// of the `verifyIPs` provided. We need to target the external test
// container multiple times until we verify that all IPs provided by
// `verifyIPs` have been verified. This is done by passing it a slice of
// verifyIPs and removing each item when it has been found. This function is
// wrapped in a `wait.PollImmediate` which results in the fact that it only
// passes once verifyIPs is of length 0. targetExternalContainerAndTest
// initiates only a single connection at a time, sequentially, hence: we
// perform one connection attempt, check that the IP seen is expected,
// remove it from the list of verifyIPs, see that it's length is not 0 and
// retry again. We do this until all IPs have been seen. If that never
// happens (because of a bug) the test fails.
func targetExternalContainerAndTest(externalContainer infraapi.ExternalContainer, podNamespace, podName string, expectSuccess bool, verifyIPs []string) wait.ConditionFunc {
	// we only know how to extract src IP from agnhost host configured with netexec and curling path /clientip to return
	// the src IP
	if !isSupportedAgnhostForEIP(externalContainer) {
		panic("unsupported image")
	}
	// first try to select the same IP family as IP(s) we are trying to verify.
	// if no verify IPs exist, pick v4 or v6 depending on whats available.
	var targetIP string
	if len(verifyIPs) > 0 {
		ip := verifyIPs[0]
		if utilnet.IsIPv4String(ip) {
			targetIP = externalContainer.GetIPv4()
		} else {
			targetIP = externalContainer.GetIPv6()
		}
	} else {
		// pick the first available IP family
		if externalContainer.IsIPv4() {
			targetIP = externalContainer.GetIPv4()
		} else {
			targetIP = externalContainer.GetIPv6()
		}
	}
	if targetIP == "" {
		framework.Fail("target container IP is not set")
	}
	URL := net.JoinHostPort(targetIP, externalContainer.GetPortStr()) + aghHostNetexecSrcIPPath

	return func() (bool, error) {
		clientStdOut, err := e2ekubectl.RunKubectl(podNamespace, "exec", podName, "--", "curl", "--connect-timeout", "2", URL)
		if err != nil {
			if !expectSuccess {
				// curl should timeout with a string containing this error, and this should be the case if we expect a failure
				if !strings.Contains(strings.ToLower(err.Error()), " timed out ") {
					framework.Logf("the test expected netserver container to not be able to connect, but it did with another error, err : %v", err)
					return false, nil
				}
				return true, nil
			}
			return false, nil
		}
		// we determine the src IP based on the target image
		// agnhost netexec will return the source IP as payload
		switch externalContainer.Image {
		case images.AgnHost():
			for _, expectedIP := range verifyIPs {
				if containsIPInLastEntry(clientStdOut, expectedIP) {
					verifyIPs = util.RemoveItemFromSliceUnstable(verifyIPs, expectedIP)
				}
			}
		default:
			panic("unimplemented container image")
		}
		if len(verifyIPs) != 0 && expectSuccess {
			framework.Logf("the test external container did not have any trace of the IPs: %v being logged, last logs: %s", verifyIPs, getLastLogLine(clientStdOut))
			return false, nil
		}
		if !expectSuccess && len(verifyIPs) == 0 {
			framework.Logf("the test external container did have a trace of the IPs: %v being logged, it should not have, last logs: %s", verifyIPs, getLastLogLine(clientStdOut))
			return false, nil
		}
		return true, nil
	}
}

// targetExternalContainerOrBGPServerAndTest returns a poll ConditionFunc verifying that a pod with NO
// EgressIP applied egresses with its default (non-EIP) source IP.
//
// When the default network is NOT advertised - or no-overlay outbound SNAT is enabled - the
// pod's traffic is SNAT'd to its node IP by the gateway router, so we verify the node IP
// against the plain external container on the primary (kind) network.
//
// When the default network IS advertised over BGP and outbound SNAT is not enabled, the
// cluster-subnet SNAT is scoped to node-destined traffic only (see GetNetworkScopedClusterSubnetSNATMatch),
// so a pod without an EIP egresses with its POD IP. The plain external container is not a BGP
// peer and has no return route to pod IPs - for IPv6 the pod subnet is advertised with a
// link-local next-hop usable only by the directly connected FRR router - so instead we target
// the BGP server behind FRR (which learns the pod routes) and verify the pod IP there.
func targetExternalContainerOrBGPServerAndTest(f *framework.Framework, externalContainer infraapi.ExternalContainer,
	podNamespace, podName, nodeIP string) wait.ConditionFunc {
	if !isDefaultNetworkAdvertised(defaultNetworkRouteAdvertisementName()) || isNoOverlayOutboundSNATEnabled(f) {
		return targetExternalContainerAndTest(externalContainer, podNamespace, podName, true, []string{nodeIP})
	}
	isV6 := utilnet.IsIPv6String(nodeIP)
	podV4IP, podV6IP, err := podIPsForDefaultNetwork(f.ClientSet, podNamespace, podName)
	framework.ExpectNoError(err, "must get pod IPs for %s/%s", podNamespace, podName)
	podIP := podV4IP
	if isV6 {
		podIP = podV6IP
	}
	gomega.Expect(podIP).NotTo(gomega.BeEmpty(), "pod %s/%s must have an IP for the tested family", podNamespace, podName)
	return targetExternalContainerAndTest(bgpServerAgnhostContainer(f), podNamespace, podName, true, []string{podIP})
}

// bgpServerAgnhostContainer returns an ExternalContainer handle for the agnhost netexec BGP
// server (reachable from pod IPs over BGP), populated so it can be used with
// targetExternalContainerAndTest.
func bgpServerAgnhostContainer(f *framework.Framework) infraapi.ExternalContainer {
	c := infraapi.ExternalContainer{
		Name:    bgpExternalServerContainerName,
		Image:   images.AgnHost(),
		CmdArgs: []string{"netexec"},
		ExtPort: netexecPort,
	}
	for _, ip := range getBGPServerContainerIPs(f) {
		if utilnet.IsIPv6String(ip) {
			c.IPv6 = ip
		} else {
			c.IPv4 = ip
		}
	}
	return c
}

func removeSliceElement(s []string, i int) []string {
	s[i] = s[len(s)-1]
	return s[:len(s)-1]
}

// checkForDuplicateMAC performs arping (IPv4) or ndisc6 (IPv6) checks to detect duplicate MAC address responses
// after egress IP migration. Returns error immediately if old node MAC is detected responding.
func checkForDuplicateMAC(externalContainer infraapi.ExternalContainer, interfaceName, egressIP, oldMAC, expectedMAC string, isIPv6 bool, maxChecks int, checkInterval time.Duration) error {
	// For arping: MAC is in brackets [aa:bb:cc:dd:ee:ff]
	// For ndisc6: MAC is on the line "Target link-layer address: aa:bb:cc:dd:ee:ff"
	macRegexArping := regexp.MustCompile(`\[([0-9a-fA-F:]+)\]`)
	macRegexNdisc6 := regexp.MustCompile(`Target link-layer address:\s+([0-9a-fA-F]{1,2}:[0-9a-fA-F]{1,2}:[0-9a-fA-F]{1,2}:[0-9a-fA-F]{1,2}:[0-9a-fA-F]{1,2}:[0-9a-fA-F]{1,2})`)

	oldMAC = strings.ToLower(oldMAC)
	expectedMAC = strings.ToLower(expectedMAC)

	var toolName string
	if isIPv6 {
		toolName = "ndisc6"
	} else {
		toolName = "arping"
	}

	framework.Logf("Checking for duplicate MAC responses after migration using %s (old MAC: %s, expected MAC: %s)...", toolName, oldMAC, expectedMAC)

	foundExpected := false
	for i := 0; i < maxChecks; i++ {
		var cmd string
		var macRegex *regexp.Regexp

		if isIPv6 {
			cmd = fmt.Sprintf("ndisc6 -1 -w 1000 %s %s 2>&1", egressIP, interfaceName)
			macRegex = macRegexNdisc6
		} else {
			cmd = fmt.Sprintf("arping -c 1 -I %s %s 2>&1", interfaceName, egressIP)
			macRegex = macRegexArping
		}

		output, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{"sh", "-c", cmd})
		if err != nil {
			framework.Logf("Check %d/%d: %s command returned error: %v; output: %s", i+1, maxChecks, toolName, err, output)
		}
		matches := macRegex.FindAllStringSubmatch(output, -1)
		for _, match := range matches {
			respondingMAC := strings.ToLower(strings.TrimSpace(match[1]))
			if respondingMAC == oldMAC {
				return fmt.Errorf("DUPLICATE MAC DETECTED on check %d: Old node MAC %s responded to %s for egress IP %s after migration. "+
					"The nftables drop rules should have prevented this response", i+1, oldMAC, toolName, egressIP)
			} else if respondingMAC == expectedMAC {
				foundExpected = true
				framework.Logf("Check %d/%d: New node MAC %s is responding (expected)", i+1, maxChecks, expectedMAC)
			} else {
				return fmt.Errorf("Unexpected MAC %s (not old or expected)", respondingMAC)
			}
		}

		if i < maxChecks-1 {
			time.Sleep(checkInterval)
		}
	}

	if !foundExpected {
		return fmt.Errorf("did not observe expected MAC %s responding to %s for egress IP %s after %d checks", expectedMAC, toolName, egressIP, maxChecks)
	}
	framework.Logf("✓ No duplicate MAC detected - nftables rules successfully blocked responses from old node")
	return nil
}

type egressIPStatus struct {
	Node     string `json:"node"`
	EgressIP string `json:"egressIP"`
}

type egressIP struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Items []egressIPStatus `json:"items"`
	} `json:"status"`
}
type egressIPs struct {
	Items []egressIP `json:"items"`
}

var _ = ginkgo.Describe("e2e egress IP validation", feature.EgressIP, func() {
	ginkgo.DescribeTableSubtree("on network of type", func(netConfigParams networkAttachmentConfigParams) {
		//FIXME: tests for CDN are designed for single stack clusters (IPv4 or IPv6) and must choose a single IP family for dual stack clusters.
		// Remove this restriction and allow the tests to detect if an IP family support is available.
		const (
			clusterIPPort           uint16 = 9999
			clusterNetworkHTTPPort  uint16 = 8080
			egressIPName            string = "egressip"
			egressIPName2           string = "egressip-2"
			targetNodeName          string = "egressTargetNode-allowed"
			deniedTargetNodeName    string = "egressTargetNode-denied"
			targetSecondaryNodeName string = "egressSecondaryTargetNode-allowed"
			egressIPYaml            string = "/tmp/egressip.yaml"
			egressFirewallYaml      string = "/tmp/egressfirewall.yaml"
			retryTimeout                   = 3 * retryTimeout // Boost the retryTimeout for EgressIP tests.
		)

		podEgressLabel := map[string]string{
			"wants": "egress",
		}

		var (
			egress1Node, egress2Node, pod1Node, pod2Node node
			providerCtx                                  infraapi.Context
			primaryTargetExternalContainer               infraapi.ExternalContainer
			primaryDeniedExternalContainer               infraapi.ExternalContainer
			secondaryTargetExternalContainer             infraapi.ExternalContainer
			pod1Name                                     = "e2e-egressip-pod-1"
			pod2Name                                     = "e2e-egressip-pod-2"
			usedEgressNodeAvailabilityHandler            egressNodeAvailabilityHandler
			isIPv6TestRun                                bool
		)

		targetPodAndTest := func(namespace, fromName, toName, toIP string, toPort uint16) wait.ConditionFunc {
			return func() (bool, error) {
				stdout, err := e2ekubectl.RunKubectl(namespace, "exec", fromName, "--",
					"curl", "--connect-timeout", "2", fmt.Sprintf("%s/hostname",
						net.JoinHostPort(toIP, fmt.Sprintf("%d", toPort))))
				if err != nil || stdout != toName {
					framework.Logf("Error: attempted connection to pod %s found err:  %v", toName, err)
					return false, nil
				}
				return true, nil
			}
		}

		targetDestinationAndTest := func(namespace, destination string, podNames []string) wait.ConditionFunc {
			return func() (bool, error) {
				for _, podName := range podNames {
					_, err := e2ekubectl.RunKubectl(namespace, "exec", podName, "--", "curl", "--connect-timeout", "2", "-k", destination)
					if err != nil {
						framework.Logf("Error: attempted connection to destination %s failed, found err:  %v", destination, err)
						return false, nil
					}
				}
				return true, nil
			}
		}

		waitForStatus := func(node string, isReady bool) {
			err := wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(context.Context) (bool, error) {
				status := getNodeStatus(node)
				if isReady {
					return status == string(corev1.ConditionTrue), nil
				}
				return status != string(corev1.ConditionTrue), nil
			})
			if err != nil {
				framework.Failf("failed while waiting for node %s to be ready: %v", node, err)
			}
		}

		hasTaint := func(node, taint string) bool {
			taint, err := e2ekubectl.RunKubectl("default", "get", "node", "-o", "jsonpath={.spec.taints[?(@.key=='"+taint+"')].key}", node)
			if err != nil {
				framework.Failf("failed to get node %s taint %s: %v", node, taint, err)
			}
			return taint != ""
		}

		waitForNoTaint := func(node, taint string) {
			err := wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(context.Context) (bool, error) {
				return !hasTaint(node, taint), nil
			})
			if err != nil {
				framework.Failf("failed while waiting for node %s to not have taint %s: %v", node, taint, err)
			}
		}

		setNodeReady := func(providerCtx infraapi.Context, node string, setReady bool) {
			if !setReady {
				_, err := infraprovider.Get().ExecK8NodeCommand(node, []string{"systemctl", "stop", "kubelet.service"})
				if err != nil {
					framework.Failf("failed to stop kubelet on node: %s, err: %v", node, err)
				}
				providerCtx.AddCleanUpFn(func() error {
					_, err := infraprovider.Get().ExecK8NodeCommand(node, []string{"systemctl", "start", "kubelet.service"})
					if err != nil {
						return fmt.Errorf("failed to restore kubelet service and ensure it is started on node: %s, err: %v", node, err)
					}
					return nil
				})
			} else if getNodeStatus(node) != string(corev1.ConditionTrue) {
				// Skip starting kubelet if the node is already ready.
				_, err := infraprovider.Get().ExecK8NodeCommand(node, []string{"systemctl", "start", "kubelet.service"})
				if err != nil {
					framework.Failf("failed to start kubelet on node: %s, err: %v", node, err)
				}
			}
			waitForStatus(node, setReady)
		}

		setNodeReachable := func(node string, setReachable bool) {
			op := "Drop"
			if setReachable {
				op = "Allow"
			}
			allowOrDropNodeInputTrafficOnPort(op, node, "tcp", "9107")
		}

		getSpecificEgressIPStatusItems := func(eipName string) []egressIPStatus {
			egressIP := egressIP{}
			egressIPStdout, err := e2ekubectl.RunKubectl("default", "get", "eip", eipName, "-o", "json")
			if err != nil {
				framework.Logf("Error: failed to get the EgressIP object, err: %v", err)
				return nil
			}
			if err := json.Unmarshal([]byte(egressIPStdout), &egressIP); err != nil {
				framework.Failf("failed to unmarshall: %v", err)
			}
			if len(egressIP.Status.Items) == 0 {
				return nil
			}
			return egressIP.Status.Items
		}

		verifySpecificEgressIPStatusLengthEquals := func(eipName string, statusLength int, verifier func(statuses []egressIPStatus) bool) []egressIPStatus {
			var statuses []egressIPStatus
			err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				statuses = getSpecificEgressIPStatusItems(eipName)
				if verifier != nil {
					return len(statuses) == statusLength && verifier(statuses), nil
				}
				framework.Logf("comparing status %d to status len %d", len(statuses), statusLength)
				return len(statuses) == statusLength, nil
			})
			if err != nil {
				framework.Failf("Error: expected to have %v egress IP assignment, got: %v", statusLength, len(statuses))
			}
			return statuses
		}

		getEgressIPStatusItems := func() []egressIPStatus {
			egressIPs := egressIPs{}
			egressIPStdout, err := e2ekubectl.RunKubectl("default", "get", "eip", "-o", "json")
			if err != nil {
				framework.Logf("Error: failed to get the EgressIP object, err: %v", err)
				return nil
			}
			json.Unmarshal([]byte(egressIPStdout), &egressIPs)
			if len(egressIPs.Items) > 1 {
				framework.Failf("Didn't expect to retrieve more than one egress IP during the execution of this test, saw: %v", len(egressIPs.Items))
			}
			return egressIPs.Items[0].Status.Items
		}

		verifyEgressIPStatusLengthEquals := func(statusLength int, verifier func(statuses []egressIPStatus) bool) []egressIPStatus {
			var statuses []egressIPStatus
			err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				statuses = getEgressIPStatusItems()
				if verifier != nil {
					return len(statuses) == statusLength && verifier(statuses), nil
				}
				return len(statuses) == statusLength, nil
			})
			if err != nil {
				framework.Failf("Error: expected to have %v egress IP assignment, got: %v", statusLength, len(statuses))
			}
			return statuses
		}

		verifyEgressIPStatusContainsIPs := func(statuses []egressIPStatus, ips []string) bool {
			eIPsFound := make([]string, 0, len(statuses))
			for _, status := range statuses {
				eIPsFound = append(eIPsFound, status.EgressIP)
			}
			sort.Strings(eIPsFound)
			sort.Strings(ips)
			return reflect.DeepEqual(eIPsFound, ips)
		}

		// verifyEgressIPAddrProto checks that the EgressIP address assigned on the
		// node has IFA_PROTO set to 85 (OVN-K). Value 85 is used instead of
		// RTPROT_OVN (84) so we do not reuse a protocol identifier owned by OVN.
		// iproute2 renders unknown protocol numbers in hex, so protocol
		// 85 appears as "0x55" in JSON output unless a name mapping exists in
		// rt_addrprotos. IFA_PROTO requires Linux kernel 5.18+; on older kernels
		// the attribute is silently ignored by the kernel and the ip CLI will not
		// report a protocol field, so the check is skipped.
		//
		// Callers must only invoke this when a Linux address is expected:
		// - secondary-host EIPs always assign the address on a host NIC
		// - OVN-network EIPs assign the address on the gateway bridge only when
		//   network segmentation is enabled (see canHandleBridgeEgressIP)
		verifyEgressIPAddrProto := func(nodeName, eipAddr string) {
			type ipAddrInfo struct {
				Local    string `json:"local"`
				Protocol string `json:"protocol"`
			}
			type ipAddrEntry struct {
				AddrInfo []ipAddrInfo `json:"addr_info"`
			}

			// iproute2 renders unknown protocol numbers in hex (e.g. 85 -> "0x55").
			expectedProtoHex := fmt.Sprintf("0x%x", types.IFAProtOVNK)

			gomega.Eventually(func(g gomega.Gomega) {
				output, err := infraprovider.Get().ExecK8NodeCommand(nodeName,
					[]string{"ip", "-d", "-j", "addr", "show", "to", eipAddr})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to get address info on node %s", nodeName)

				var entries []ipAddrEntry
				g.Expect(json.Unmarshal([]byte(output), &entries)).To(gomega.Succeed(), "failed to parse ip addr JSON")
				g.Expect(entries).To(gomega.HaveLen(1), "expected a single interface entry for EgressIP %s on node %s", eipAddr, nodeName)

				var ai *ipAddrInfo
				for i, info := range entries[0].AddrInfo {
					if info.Local == eipAddr {
						ai = &entries[0].AddrInfo[i]
						break
					}
				}
				g.Expect(ai).NotTo(gomega.BeNil(), "EgressIP %s not found on node %s", eipAddr, nodeName)
				if ai.Protocol == "" {
					framework.Logf("IFA_PROTO not reported for EgressIP %s on node %s; kernel may not support IFA_PROTO (requires 5.18+), skipping check", eipAddr, nodeName)
					return
				}
				g.Expect(ai.Protocol).To(gomega.Equal(expectedProtoHex),
					"EgressIP %s on node %s should have IFA_PROTO 85/0x55 (OVN-K), got %q", eipAddr, nodeName, ai.Protocol)
			}, 30*time.Second, 2*time.Second).Should(gomega.Succeed())
		}

		getIPVersions := func(ips ...string) (bool, bool) {
			var v4, v6 bool
			for _, ip := range ips {
				if utilnet.IsIPv6String(ip) {
					v6 = true
				} else {
					v4 = true
				}
			}
			return v4, v6
		}

		getNodesInternalAddresses := func(nodes *corev1.NodeList, family corev1.IPFamily) []string {
			ips := make([]string, 0, 3)
			for _, node := range nodes.Items {
				ips = append(ips, e2enode.GetAddressesByTypeAndFamily(&node, corev1.NodeInternalIP, family)...)
			}
			return ips
		}

		isNodeInternalAddressesPresentForIPFamily := func(nodes *corev1.NodeList, ipFamily corev1.IPFamily) bool {
			if len(getNodesInternalAddresses(nodes, ipFamily)) > 0 {
				return true
			}
			return false
		}

		isNetworkSupported := func(nodes *corev1.NodeList, netConfigParams networkAttachmentConfigParams) (bool, string) {
			// cluster default network
			if netConfigParams.networkName == types.DefaultNetworkName {
				return true, "cluster default network is always supported"
			}
			// user defined networks
			if !isNetworkSegmentationEnabled() {
				return false, "network segmentation is disabled. Environment variable 'ENABLE_NETWORK_SEGMENTATION' must have value true"
			}
			if netConfigParams.topology == types.LocalnetTopology {
				return false, "unsupported network topology"
			}
			if netConfigParams.cidr == "" {
				return false, "UDN network must have subnet specified"
			}
			if cidrsContainIPFamily(netConfigParams.cidr, false) && !isNodeInternalAddressesPresentForIPFamily(nodes, corev1.IPv4Protocol) {
				return false, "cluster must have IPv4 Node internal address"
			}
			if cidrsContainIPFamily(netConfigParams.cidr, true) && !isNodeInternalAddressesPresentForIPFamily(nodes, corev1.IPv6Protocol) {
				return false, "cluster must have IPv6 Node internal address"
			}
			return true, "network is supported"
		}

		getNodeIPs := func(nodes *corev1.NodeList, netConfigParams networkAttachmentConfigParams) []string {
			isIPv4Cluster := isNodeInternalAddressesPresentForIPFamily(nodes, corev1.IPv4Protocol)
			isIPv6Cluster := isNodeInternalAddressesPresentForIPFamily(nodes, corev1.IPv6Protocol)
			var ipFamily corev1.IPFamily
			// cluster default network
			if netConfigParams.networkName == types.DefaultNetworkName {
				// we do not create a CDN, we utilize the network within the cluster.
				// The current e2e tests assume a single stack, therefore if dual stack, default to IPv4
				// until the tests are refactored to accommodate dual stack.
				if isIPv6Cluster {
					ipFamily = corev1.IPv6Protocol
				}
				if isIPv4Cluster {
					ipFamily = corev1.IPv4Protocol
				}
			} else {
				// user defined network
				if netConfigParams.cidr == "" {
					framework.Failf("network config must have subnet defined")
				}
				if cidrsContainIPFamily(netConfigParams.cidr, true) && isIPv6Cluster {
					ipFamily = corev1.IPv6Protocol
				}
				if cidrsContainIPFamily(netConfigParams.cidr, false) && isIPv4Cluster {
					ipFamily = corev1.IPv4Protocol
				}
			}
			if ipFamily == corev1.IPFamilyUnknown {
				framework.Failf("network config is not supported by the cluster")
			}
			return getNodesInternalAddresses(nodes, ipFamily)
		}

		getPodIPWithRetry := func(clientSet clientset.Interface, v6 bool, namespace, name string) (net.IP, error) {
			var srcPodIP net.IP
			err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				pod, err := clientSet.CoreV1().Pods(namespace).Get(context.Background(), name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				ips, err := util.DefaultNetworkPodIPs(pod)
				if err != nil {
					return false, err
				}
				srcPodIP, err = util.MatchFirstIPFamily(isIPv6TestRun, ips)
				if err != nil {
					return false, err
				}
				return true, nil
			})
			if err != nil || srcPodIP == nil {
				return srcPodIP, fmt.Errorf("unable to fetch pod %s/%s IP after retrying: %v", namespace, name, err)
			}
			return srcPodIP, nil
		}

		isUserDefinedNetwork := func(netParams networkAttachmentConfigParams) bool {
			if netParams.networkName == types.DefaultNetworkName {
				return false
			}
			return true
		}

		isClusterDefaultNetwork := func(netParams networkAttachmentConfigParams) bool {
			if netParams.networkName == types.DefaultNetworkName {
				return true
			}
			return false
		}

		f := wrappedTestFramework(egressIPName)
		f.SkipNamespaceCreation = true

		// Determine what mode the CI is running in and get relevant endpoint information for the tests
		ginkgo.BeforeEach(func() {
			providerCtx = infraprovider.Get().NewTestContext()
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 3 {
				framework.Failf("Test requires >= 3 Ready nodes, but there are only %v nodes", len(nodes.Items))
			}
			filterSupportedNetworkConfig(f.ClientSet, &netConfigParams)
			if isSupported, reason := isNetworkSupported(nodes, netConfigParams); !isSupported {
				ginkgo.Skip(reason)
			}
			// tests are configured to introspect the Nodes Internal IP address family and then create an EgressIP of
			// the same IP family. If dual stack, we default to IPv4 because the tests aren't configured to handle dual stack.
			ips := getNodeIPs(nodes, netConfigParams)
			if len(ips) == 0 {
				framework.Failf("expect at least one IP address")
			}

			labels := map[string]string{
				"e2e-framework": f.BaseName,
			}
			if !isClusterDefaultNetwork(netConfigParams) {
				labels[RequiredUDNNamespaceLabel] = ""
			}
			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, labels)
			f.Namespace = namespace
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			isIPv6TestRun = utilnet.IsIPv6String(ips[0])
			egress1Node = node{
				name:   nodes.Items[1].Name,
				nodeIP: ips[1],
			}
			egress2Node = node{
				name:   nodes.Items[2].Name,
				nodeIP: ips[2],
			}
			pod1Node = node{
				name:   nodes.Items[0].Name,
				nodeIP: ips[0],
			}
			pod2Node = node{
				name:   nodes.Items[1].Name,
				nodeIP: ips[1],
			}
			// ensure all nodes are ready and reachable
			for _, node := range nodes.Items {
				setNodeReady(providerCtx, node.Name, true)
				setNodeReachable(node.Name, true)
				waitForNoTaint(node.Name, "node.kubernetes.io/unreachable")
				waitForNoTaint(node.Name, "node.kubernetes.io/not-ready")
			}
			// Primary provider network
			primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
			framework.ExpectNoError(err, "failed to get primary provider network")

			// attach containers to the primary network
			primaryTargetExternalContainerPort := infraprovider.Get().GetExternalContainerPort()
			primaryTargetExternalContainerSpec := infraapi.ExternalContainer{Name: targetNodeName, Image: images.AgnHost(),
				Network: primaryProviderNetwork, CmdArgs: getAgnHostHTTPPortBindCMDArgs(primaryTargetExternalContainerPort), ExtPort: primaryTargetExternalContainerPort}
			primaryTargetExternalContainer, err = providerCtx.CreateExternalContainer(primaryTargetExternalContainerSpec)
			framework.ExpectNoError(err, "failed to create external target container on primary network", primaryTargetExternalContainerSpec.String())

			primaryDeniedExternalContainerPort := infraprovider.Get().GetExternalContainerPort()
			primaryDeniedExternalContainerSpec := infraapi.ExternalContainer{Name: deniedTargetNodeName, Image: images.AgnHost(),
				Network: primaryProviderNetwork, CmdArgs: getAgnHostHTTPPortBindCMDArgs(primaryDeniedExternalContainerPort), ExtPort: primaryDeniedExternalContainerPort}
			primaryDeniedExternalContainer, err = providerCtx.CreateExternalContainer(primaryDeniedExternalContainerSpec)
			framework.ExpectNoError(err, "failed to create external denied container on primary network", primaryDeniedExternalContainer.String())

			// Setup secondary provider network
			secondarySubnet := secondaryIPV4Subnet
			if isIPv6TestRun {
				secondarySubnet = secondaryIPV6Subnet
			}
			// configure and add additional network to worker containers for EIP multi NIC feature
			secondaryProviderNetwork, err := providerCtx.CreateNetwork(secondaryNetworkName, secondarySubnet)
			framework.ExpectNoError(err, "creation of network %q with subnet %s must succeed", secondaryNetworkName, secondarySubnet)
			nodes, err = f.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			framework.ExpectNoError(err, "must list all Nodes")
			for _, node := range nodes.Items {
				_, err = providerCtx.AttachNetwork(secondaryProviderNetwork, node.Name)
				framework.ExpectNoError(err, "network %s must attach to node %s", secondaryProviderNetwork.Name, node.Name)
			}
			secondaryTargetExternalContainerPort := infraprovider.Get().GetExternalContainerPort()
			secondaryTargetExternalContainerSpec := infraapi.ExternalContainer{
				Name:    targetSecondaryNodeName,
				Image:   images.AgnHost(),
				Network: secondaryProviderNetwork,
				CmdArgs: getAgnHostHTTPPortBindCMDArgs(secondaryTargetExternalContainerPort),
				ExtPort: secondaryTargetExternalContainerPort,
			}
			secondaryTargetExternalContainer, err = providerCtx.CreateExternalContainer(secondaryTargetExternalContainerSpec)
			framework.ExpectNoError(err, "unable to create external container %s", secondaryTargetExternalContainerSpec.Name)
			if secondaryTargetExternalContainer.GetIPv4() == "" && !isIPv6TestRun {
				panic("failed to get v4 address")
			}
			if secondaryTargetExternalContainer.GetIPv6() == "" && isIPv6TestRun {
				panic("failed to get v6 address")
			}

			if isIPv6TestRun {
				if !primaryTargetExternalContainer.IsIPv6() || !primaryDeniedExternalContainer.IsIPv6() || !secondaryTargetExternalContainer.IsIPv6() {
					framework.Failf("one or more external containers do not have an IPv6 address,"+
						" target primary network %q, denied primary network %q, target secondary network %q",
						primaryTargetExternalContainer.GetIPv6(), primaryDeniedExternalContainer.GetIPv6(), secondaryTargetExternalContainer.GetIPv6())
				}
			} else {
				if !primaryTargetExternalContainer.IsIPv4() || !primaryDeniedExternalContainer.IsIPv4() || !secondaryTargetExternalContainer.IsIPv4() {
					framework.Failf("one or more external containers do not have an IPv4 address,"+
						" target primary network %q, denied primary network %q, target secondary network %q",
						primaryTargetExternalContainer.GetIPv4(), primaryDeniedExternalContainer.GetIPv4(), secondaryTargetExternalContainer.GetIPv4())
				}
			}
			// no further network creation is required if CDN
			if isClusterDefaultNetwork(netConfigParams) {
				return
			}
			// configure UDN
			nadClient, err := nadclient.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			netConfig := newNetworkAttachmentConfig(netConfigParams)
			netConfig.namespace = f.Namespace.Name
			_, err = nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
				context.Background(),
				generateNAD(netConfig, f.ClientSet),
				metav1.CreateOptions{},
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.AfterEach(func() {
			// ensure all nodes are ready and reachable before any other cleanup;
			// tests may have left nodes NotReady or unreachable intentionally
			for _, node := range []string{egress1Node.name, egress2Node.name} {
				setNodeReady(providerCtx, node, true)
				setNodeReachable(node, true)
				waitForNoTaint(node, "node.kubernetes.io/unreachable")
				waitForNoTaint(node, "node.kubernetes.io/not-ready")
			}

			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 3 {
				framework.Failf("Test requires >= 3 Ready nodes, but there are only %v nodes", len(nodes.Items))
			}
			if isSupported, reason := isNetworkSupported(nodes, netConfigParams); !isSupported {
				ginkgo.Skip(reason)
			}
			e2ekubectl.RunKubectlOrDie("default", "delete", "eip", egressIPName, "--ignore-not-found=true")
			e2ekubectl.RunKubectlOrDie("default", "delete", "eip", egressIPName2, "--ignore-not-found=true")
			e2ekubectl.RunKubectlOrDie("default", "label", "node", egress1Node.name, "k8s.ovn.org/egress-assignable-")
			e2ekubectl.RunKubectlOrDie("default", "label", "node", egress2Node.name, "k8s.ovn.org/egress-assignable-")
		})
		// Validate the egress IP by creating a httpd container on the kind networking
		// (effectively seen as "outside" the cluster) and curl it from a pod in the cluster
		// which matches the egress IP stanza.
		// Do this using different methods to disable a node for egress:
		// - removing the egress-assignable label
		// - impeding traffic for the GRPC health check

		/* This test does the following:
		   0. Set two nodes as available for egress
		   1. Create an EgressIP object with two egress IPs defined
		   2. Check that the status is of length two and both are assigned to different nodes
		   3. Create two pods matching the EgressIP: pod1 on a non-egress node and pod2 on an egress node
		   4. Check connectivity from both to an external "node" and verify that the IPs are both of the above
		   5. Check connectivity from one pod to the other and verify that the connection is achieved
		   6. Check connectivity from both pods to the api-server (running hostNetwork:true) and verifying that the connection is achieved
		   7. Update one of the pods, unmatching the EgressIP
		   8. Check connectivity from that one to an external "node" and verify that the IP is the node IP.
		   9. Check connectivity from the other one to an external "node"  and verify that the IPs are both of the above
		   10. Set one node as unavailable for egress
		   11. Check that the status is of length one
		   12. Check connectivity from the remaining pod to an external "node" and verify that the IP is the remaining egress IP
		   13. Set the other node as unavailable for egress
		   14. Check that the status is of length zero
		   15. Check connectivity from the remaining pod to an external "node" and verify that the IP is the node IP.
		   16. Set one node back as available for egress
		   17. Check that the status is of length one
		   18. Check connectivity from the remaining pod to an external "node" and verify that the IP is the remaining egress IP
		*/
		ginkgo.Describe("[OVN network] Using different methods to disable a node's availability for egress", func() {
			ginkgo.AfterEach(func() {
				usedEgressNodeAvailabilityHandler.Restore(egress1Node.name)
				usedEgressNodeAvailabilityHandler.Restore(egress2Node.name)
			})

			ginkgo.DescribeTable("Should validate the egress IP functionality against remote hosts",
				func(egressNodeAvailabilityHandler egressNodeAvailabilityHandler) {
					// set the egressNodeAvailabilityHandler that we are using so that
					// we can restore in AfterEach
					usedEgressNodeAvailabilityHandler = egressNodeAvailabilityHandler

					ginkgo.By("0. Setting two nodes as available for egress")
					usedEgressNodeAvailabilityHandler.Enable(egress1Node.name)
					usedEgressNodeAvailabilityHandler.Enable(egress2Node.name)

					podNamespace := f.Namespace
					labels := map[string]string{
						"name": f.Namespace.Name,
					}
					updateNamespaceLabels(f, f.Namespace, labels)

					ginkgo.By("1. Create an EgressIP object with two egress IPs defined")
					var egressIP1, egressIP2 net.IP
					var err, err2 error
					if utilnet.IsIPv6String(egress1Node.nodeIP) {
						egressIP1, err = allocators.NewPrimaryIPv6(egress1Node.name)
						egressIP2, err2 = allocators.NewPrimaryIPv6(egress2Node.name)
					} else {
						egressIP1, err = allocators.NewPrimaryIPv4(egress1Node.name)
						egressIP2, err2 = allocators.NewPrimaryIPv4(egress2Node.name)
					}
					gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")
					gomega.Expect(err2).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")

					var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - ` + egressIP1.String() + `
    - ` + egressIP2.String() + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`

					if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
						framework.Failf("Unable to write CRD config to disk: %v", err)
					}
					defer func() {
						if err := os.Remove(egressIPYaml); err != nil {
							framework.Logf("Unable to remove the CRD config from disk: %v", err)
						}
					}()

					framework.Logf("Create the EgressIP configuration")
					e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

					ginkgo.By("2. Check that the status is of length two and both are assigned to different nodes")
					statuses := verifyEgressIPStatusLengthEquals(2, nil)
					if statuses[0].Node == statuses[1].Node {
						framework.Failf("Step 2. Check that the status is of length two and both are assigned to different nodess, failed, err: both egress IPs have been assigned to the same node")
					}

					ginkgo.By("3. Create two pods matching the EgressIP: pod1 on a non-egress node and pod2 on an egress node")
					_, err = createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
					framework.ExpectNoError(err, "failed to create pod %s/%s", f.Namespace.Name, pod1Name)
					_, err = createGenericPodWithLabel(f, pod2Name, pod2Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
					framework.ExpectNoError(err, "failed to create pod %s/%s", f.Namespace.Name, pod2Name)

					err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
						for _, podName := range []string{pod1Name, pod2Name} {
							kubectlOut := getPodAddress(podName, f.Namespace.Name)
							srcIP := net.ParseIP(kubectlOut)
							if srcIP == nil {
								return false, nil
							}
						}
						return true, nil
					})
					framework.ExpectNoError(err, "Step 3. Create two pods matching the EgressIP: one running on each of the egress nodes, failed, err: %v", err)
					var pod1IP, pod2IP string
					if isClusterDefaultNetwork(netConfigParams) {
						pod1IPNet, err := getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, pod1Name)
						framework.ExpectNoError(err, "Step 3. failed to get pod1 IP, err: %v", err)
						pod1IP = pod1IPNet.String()
						pod2IPNet, err := getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, pod2Name)
						framework.ExpectNoError(err, "Step 3. failed to get pod2 IP, err: %v", err)
						pod2IP = pod2IPNet.String()
					} else {
						pod1IP, err = getPodAnnotationIPsForAttachmentByIndex(
							f.ClientSet,
							f.Namespace.Name,
							pod1Name,
							namespacedName(f.Namespace.Name, netConfigParams.name),
							0,
						)
						framework.ExpectNoError(err, "Step 3. Create two UDN pods matching the EgressIP: one running on each of the egress nodes, failed, err: %v", err)
						pod2IP, err = getPodAnnotationIPsForAttachmentByIndex(
							f.ClientSet,
							f.Namespace.Name,
							pod2Name,
							namespacedName(f.Namespace.Name, netConfigParams.name),
							0,
						)
						framework.ExpectNoError(err, "Step 3. Create two UDN pods matching the EgressIP: one running on each of the egress nodes, failed, err: %v", err)
					}

					ginkgo.By("4. Check connectivity from both to an external \"node\" and verify that the IPs are both of the above")
					err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{egressIP1.String(), egressIP2.String()}))
					framework.ExpectNoError(err, "Step 4. Check connectivity from first to an external \"node\" and verify that the IPs are both of the above, failed: %v", err)
					err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod2Name, true, []string{egressIP1.String(), egressIP2.String()}))
					framework.ExpectNoError(err, "Step 4. Check connectivity from second to an external \"node\" and verify that the IPs are both of the above, failed: %v", err)

					ginkgo.By("4a. Check that the EgressIP addresses have IFA_PROTO set to OVN-K (85)")
					// OVN-network EIPs are assigned on the gateway bridge only when
					// network segmentation is enabled (canHandleBridgeEgressIP).
					if isNetworkSegmentationEnabled() {
						for _, status := range statuses {
							verifyEgressIPAddrProto(status.Node, status.EgressIP)
						}
					}

					ginkgo.By("5. Check connectivity from non-egress node pod to egress node pod and verify that the connection is achieved")
					err = wait.PollImmediate(retryInterval, retryTimeout, targetPodAndTest(f.Namespace.Name, pod1Name, pod2Name, pod2IP, clusterNetworkHTTPPort))
					framework.ExpectNoError(err, "Step 5. Check connectivity from non-egress node pod to egress node pod, failed, err: %v", err)

					ginkgo.By("5. Check connectivity from egress node pod to non-egress node pod and verify that the connection is achieved")
					err = wait.PollImmediate(retryInterval, retryTimeout, targetPodAndTest(f.Namespace.Name, pod2Name, pod1Name, pod1IP, clusterNetworkHTTPPort))
					framework.ExpectNoError(err, "Step 5. Check connectivity from egress node pod to non-egress node pod, failed, err: %v", err)

					ginkgo.By("6. Check connectivity from both pods to the api-server (running hostNetwork:true) and verifying that the connection is achieved")
					// CDN exposes either IPv4 and/or IPv6 API endpoint depending on cluster configuration. The network which we are testing may not support this IP family. Skip if unsupported.
					apiAddress := getApiAddress()
					if utilnet.IsIPv6String(apiAddress) == isIPv6TestRun {
						err = wait.PollImmediate(retryInterval, retryTimeout, targetDestinationAndTest(podNamespace.Name,
							fmt.Sprintf("https://%s/version", net.JoinHostPort(apiAddress, "443")), []string{pod1Name, pod2Name}))
						framework.ExpectNoError(err, "6. Check connectivity from pod to the api-server (running hostNetwork:true) and verifying that the connection is achieved, failed, err: %v", err)
					} else {
						framework.Logf("Skipping API server reachability check because IP family does not equal IP family of the EgressIP")
					}

					ginkgo.By("7. Update one of the pods, unmatching the EgressIP")
					pod2 := getPod(f, pod2Name)
					pod2.Labels = map[string]string{}
					updatePod(f, pod2)

					ginkgo.By("8. Check connectivity from that one to an external \"node\" and verify that the IP is the node IP.")
					err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod2Name, true, []string{pod2Node.nodeIP}))
					framework.ExpectNoError(err, "Step 8. Check connectivity from that one to an external \"node\" and verify that the IP is the node IP, failed, err: %v", err)

					ginkgo.By("9. Check connectivity from the other one to an external \"node\" and verify that the IPs are both of the above")
					err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{egressIP1.String(), egressIP2.String()}))
					framework.ExpectNoError(err, "Step 9. Check connectivity from the other one to an external \"node\" and verify that the IP is one of the egress IPs, failed, err: %v", err)

					ginkgo.By("10. Setting one node as unavailable for egress")
					usedEgressNodeAvailabilityHandler.Disable(egress1Node.name)

					ginkgo.By("11. Check that the status is of length one")
					statuses = verifyEgressIPStatusLengthEquals(1, nil)

					ginkgo.By("12. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the remaining egress IP")
					err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{statuses[0].EgressIP}))
					framework.ExpectNoError(err, "Step 12. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the remaining egress IP, failed, err: %v", err)

					ginkgo.By("13. Setting the other node as unavailable for egress")
					usedEgressNodeAvailabilityHandler.Disable(egress2Node.name)

					ginkgo.By("14. Check that the status is of length zero")
					statuses = verifyEgressIPStatusLengthEquals(0, nil)

					ginkgo.By("15. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the node IP.")
					err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{pod1Node.nodeIP}))
					framework.ExpectNoError(err, "Step  15. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the node IP, failed, err: %v", err)

					ginkgo.By("16. Setting one node as available for egress")
					usedEgressNodeAvailabilityHandler.Enable(egress2Node.name)

					ginkgo.By("17. Check that the status is of length one")
					statuses = verifyEgressIPStatusLengthEquals(1, nil)

					ginkgo.By("18. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the remaining egress IP")
					err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{statuses[0].EgressIP}))
					framework.ExpectNoError(err, "Step 18. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the remaining egress IP, failed, err: %v", err)
				},
				ginkgo.Entry("disabling egress nodes with egress-assignable label", &egressNodeAvailabilityHandlerViaLabel{f}),
				ginkgo.Entry("disabling egress nodes impeding GRCP health check", &egressNodeAvailabilityHandlerViaHealthCheck{F: f, Legacy: false}),
				ginkgo.Entry("disabling egress nodes impeding Legacy health check", &egressNodeAvailabilityHandlerViaHealthCheck{F: f, Legacy: true}),
			)
		})

		// Validate the egress IP by creating a httpd container on the kind
		// networking (effectively seen as "outside" the cluster) and curl it from a
		// pod in the cluster which matches the egress IP stanza. Aim is to check
		// that the SNATs to egressIPs are being correctly deleted and recreated
		// but not used for intra-cluster traffic.

		/* This test does the following:
		   0. Add the "k8s.ovn.org/egress-assignable" label to egress1Node
		   1. Setting a secondary IP on non-egress node acting as "another node"
		   2. Creating host-networked pod on non-egress node (egress2Node) acting as "another node"
		   3. Create an EgressIP object with one egress IP defined
		   4. Check that the status is of length one and that it is assigned to egress1Node
		   5. Create one pod matching the EgressIP: running on egress1Node
		   6. Check connectivity from pod to an external "node" and verify that the srcIP is the expected egressIP
		   7. Check connectivity from pod to another node (egress2Node) primary IP and verify that the srcIP is the expected nodeIP
		   8. Check connectivity from pod to another node (egress2Node) secondary IP and verify that the srcIP is the expected nodeIP
		   9. Add the "k8s.ovn.org/egress-assignable" label to egress2Node
		   10. Remove the "k8s.ovn.org/egress-assignable" label from egress1Node
		   11. Check that the status is of length one and that it is assigned to egress2Node
		   12. Check connectivity from pod to an external "node" and verify that the srcIP is the expected egressIP
		   13. Check connectivity from pod to another node (egress2Node) primary IP and verify that the srcIP is the expected nodeIP
		   14. Check connectivity from pod to another node (egress2Node) secondary IP and verify that the srcIP is the expected nodeIP
		   15. Create second pod not matching the EgressIP: running on egress1Node
		   16. Check connectivity from second pod to external node and verify that the srcIP is the expected nodeIP
		   17. Add pod selector label to make second pod egressIP managed
		   18. Check connectivity from second pod to external node and verify that the srcIP is the expected egressIP
		   19. Check connectivity from second pod to another node (egress2Node) primary IP and verify that the srcIP is the expected nodeIP (this verifies SNAT's towards nodeIP are not deleted for pods unless pod is on its own egressNode)
		   20. Check connectivity from second pod to another node (egress2Node) secondary IP and verify that the srcIP is the expected nodeIP (this verifies SNAT's towards nodeIP are not deleted for pods unless pod is on its own egressNode)
		*/
		ginkgo.It("[OVN network] Should validate the egress IP SNAT functionality against host-networked pods", func() {
			ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to egress1Node node")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
			framework.Logf("Added egress-assignable label to node %s", egress1Node.name)
			e2enode.ExpectNodeHasLabel(context.TODO(), f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

			ginkgo.By("1. By setting a secondary IP on non-egress node acting as \"another node\"")
			var otherDstIP net.IP
			var err error
			if utilnet.IsIPv6String(egress2Node.nodeIP) {
				otherDstIP, err = allocators.NewPrimaryIPv6(egress2Node.name)
			} else {
				otherDstIP, err = allocators.NewPrimaryIPv4(egress2Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate secondary node IP")
			otherDst := otherDstIP.String()
			framework.Logf("Adding secondary IP %s to external bridge %s on Node %s", otherDst, deploymentconfig.Get().ExternalBridgeName(), egress2Node.name)
			_, err = infraprovider.Get().ExecK8NodeCommand(egress2Node.name, []string{"ip", "addr", "add", otherDst, "dev", deploymentconfig.Get().ExternalBridgeName()})
			if err != nil {
				framework.Failf("failed to add address to node %s: %v", egress2Node.name, err)
			}
			providerCtx.AddCleanUpFn(func() error {
				_, err := infraprovider.Get().ExecK8NodeCommand(egress2Node.name, []string{"ip", "addr", "del", otherDst, "dev", deploymentconfig.Get().ExternalBridgeName()})
				return err
			})

			hostNetPort := infraprovider.Get().GetK8HostPort()
			// Use a fixed pod name instead of deriving it from the node name to avoid
			// exceeding the 63-character limit or including dots in the pod name.
			hostNetPodName := "egress-host-net-pod"
			otherHostNetPodIP := node{
				name:   hostNetPodName,
				nodeIP: otherDst,
				port:   hostNetPort,
			}

			ginkgo.By("2. Creating host-networked pod, on non-egress node acting as \"another node\"")
			p, err := createPod(f, hostNetPodName, egress2Node.name, f.Namespace.Name, []string{}, map[string]string{}, func(p *corev1.Pod) {
				p.Spec.HostNetwork = true
				p.Spec.Containers[0].Image = images.AgnHost()
				p.Spec.Containers[0].Args = getAgnHostHTTPPortBindCMDArgs(hostNetPort)
			})
			framework.ExpectNoError(err)
			// block until host network pod is fully deleted because subsequent tests that require binding to the same port may fail
			defer func() {
				ctxWithTimeout, cancelFn := context.WithTimeout(context.Background(), time.Second*60)
				defer cancelFn()
				err = pod.DeletePodWithWait(ctxWithTimeout, f.ClientSet, p)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "deletion of host network pod must succeed")
				err = pod.WaitForPodNotFoundInNamespace(ctxWithTimeout, f.ClientSet, hostNetPodName, f.Namespace.Name, time.Second*59)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "pod must be fully deleted within 60 seconds")
			}()
			hostNetPod := node{
				name:   hostNetPodName,
				nodeIP: egress2Node.nodeIP,
				port:   hostNetPort,
			}
			framework.Logf("Created pod %s on node %s", hostNetPod.name, egress2Node.name)

			podNamespace := f.Namespace
			labels := map[string]string{
				"name": f.Namespace.Name,
			}
			updateNamespaceLabels(f, podNamespace, labels)

			ginkgo.By("3. Create an EgressIP object with one egress IP defined")
			var egressIP1 net.IP
			if utilnet.IsIPv6String(egress2Node.nodeIP) {
				egressIP1, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP1, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")

			framework.Logf("Selected EgressIP %s", egressIP1.String())
			var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - ` + egressIP1.String() + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("4. Check that the status is of length one and that it is assigned to egress1Node")
			statuses := verifyEgressIPStatusLengthEquals(1, nil)
			if statuses[0].Node != egress1Node.name {
				framework.Failf("Step 4. Check that the status is of length one and that it is assigned to egress1Node, failed")
			}

			ginkgo.By("5. Create one pod matching the EgressIP: running on egress1Node")
			_, err = createGenericPodWithLabel(f, pod1Name, pod2Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			framework.ExpectNoError(err, "failed to create pod %s/%s", f.Namespace.Name, pod1Name)

			_, err = getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, pod1Name)
			framework.ExpectNoError(err, "Step 5. Create one pod matching the EgressIP: running on egress1Node, failed, err: %v", err)
			framework.Logf("Created pod %s on node %s", pod1Name, pod2Node.name)

			ginkgo.By("6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "Step 6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP, failed: %v", err)

			ginkgo.By("6a. Check that the EgressIP address has IFA_PROTO set to OVN-K (85)")
			// OVN-network EIPs are assigned on the gateway bridge only when
			// network segmentation is enabled (canHandleBridgeEgressIP).
			if isNetworkSegmentationEnabled() {
				verifyEgressIPAddrProto(statuses[0].Node, statuses[0].EgressIP)
			}

			ginkgo.By("7. Check connectivity from pod to another node primary IP and verify that the srcIP is the expected nodeIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetHostNetworkContainerAndTest(hostNetPod, podNamespace.Name, pod1Name, true, []string{egress1Node.nodeIP}))
			framework.ExpectNoError(err, "Step 7. Check connectivity from pod to another node primary IP and verify that the srcIP is the expected nodeIP, failed: %v", err)

			ginkgo.By("8. Check connectivity from pod to another node secondary IP and verify that the srcIP is the expected nodeIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetHostNetworkContainerAndTest(otherHostNetPodIP, podNamespace.Name, pod1Name, true, []string{egress1Node.nodeIP}))
			framework.ExpectNoError(err, "Step 8. Check connectivity from pod to another node secondary IP and verify that the srcIP is the expected nodeIP, failed: %v", err)

			ginkgo.By("9. Add the \"k8s.ovn.org/egress-assignable\" label to egress2Node")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")
			framework.Logf("Added egress-assignable label to node %s", egress2Node.name)
			e2enode.ExpectNodeHasLabel(context.TODO(), f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")

			ginkgo.By("10. Remove the \"k8s.ovn.org/egress-assignable\" label from egress1Node")
			e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable")

			ginkgo.By("11. Check that the status is of length one and that it is assigned to egress2Node")
			// There is sometimes a slight delay for the EIP fail over to happen,
			// so let's use the pollimmediate struct to check if eventually egress2Node becomes the egress node
			err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				statuses := getEgressIPStatusItems()
				return (len(statuses) == 1) && (statuses[0].Node == egress2Node.name), nil
			})
			framework.ExpectNoError(err, "Step 11. Check that the status is of length one and that it is assigned to egress2Node, failed: %v", err)

			ginkgo.By("12. Check connectivity from pod to an external \"node\" and verify that the srcIP is the expected egressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "Step 12. Check connectivity from pod to an external \"node\" and verify that the srcIP is the expected egressIP, failed, err: %v", err)

			ginkgo.By("13. Check connectivity from pod to another node primary IP and verify that the srcIP is the expected nodeIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetHostNetworkContainerAndTest(hostNetPod, podNamespace.Name, pod1Name, true, []string{egress1Node.nodeIP}))
			framework.ExpectNoError(err, "Step 13. Check connectivity from pod to another node and verify that the srcIP is the expected nodeIP, failed: %v", err)

			ginkgo.By("14. Check connectivity from pod to another node secondary IP and verify that the srcIP is the expected nodeIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetHostNetworkContainerAndTest(otherHostNetPodIP, podNamespace.Name, pod1Name, true, []string{egress1Node.nodeIP}))
			framework.ExpectNoError(err, "Step 14. Check connectivity from pod to another node secondary IP and verify that the srcIP is the expected nodeIP, failed: %v", err)

			ginkgo.By("15. Create second pod not matching the EgressIP: running on egress1Node")
			_, err = createGenericPodWithLabel(f, pod2Name, pod2Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), map[string]string{})
			framework.ExpectNoError(err, "failed to create pod %s/%s", pod2Name, f.Namespace.Name)
			_, err = getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, pod2Name)
			framework.ExpectNoError(err, "Step 15. Create second pod not matching the EgressIP: running on egress1Node, failed, err: %v", err)
			framework.Logf("Created pod %s on node %s", pod2Name, pod2Node.name)

			ginkgo.By("16. Check connectivity from second pod to external node and verify that the srcIP is the expected nodeIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod2Name, true, []string{egress1Node.nodeIP}))
			framework.ExpectNoError(err, "Step 16. Check connectivity from second pod to external node and verify that the srcIP is the expected nodeIP, failed: %v", err)

			ginkgo.By("17. Add pod selector label to make second pod egressIP managed")
			pod2 := getPod(f, pod2Name)
			pod2.Labels = podEgressLabel
			updatePod(f, pod2)

			ginkgo.By("18. Check connectivity from second pod to external node and verify that the srcIP is the expected egressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod2Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "Step 18. Check connectivity from second pod to external node and verify that the srcIP is the expected egressIP, failed: %v", err)

			ginkgo.By("19. Check connectivity from second pod to another node primary IP and verify that the srcIP is the expected nodeIP (this verifies SNAT's towards nodeIP are not deleted unless node is egressNode)")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetHostNetworkContainerAndTest(hostNetPod, podNamespace.Name, pod2Name, true, []string{egress1Node.nodeIP}))
			framework.ExpectNoError(err, "Step 19. Check connectivity from second pod to another node and verify that the srcIP is the expected nodeIP (this verifies SNAT's towards nodeIP are not deleted unless node is egressNode), failed: %v", err)

			ginkgo.By("20. Check connectivity from second pod to another node secondary IP and verify that the srcIP is the expected nodeIP (this verifies SNAT's towards nodeIP are not deleted unless node is egressNode)")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetHostNetworkContainerAndTest(otherHostNetPodIP, podNamespace.Name, pod2Name, true, []string{egress1Node.nodeIP}))
			framework.ExpectNoError(err, "Step 20. Check connectivity from second pod to another node secondary IP and verify that the srcIP is the expected nodeIP (this verifies SNAT's towards nodeIP are not deleted unless node is egressNode), failed: %v", err)
		})

		// Validate the egress IP with stateful sets or pods recreated with same name
		/* This test does the following:
		   0. Add the "k8s.ovn.org/egress-assignable" label to node2 (egress1Node)
		   1. Create an EgressIP object with one egress IP defined
		   2. Check that the status is of length one and that it is assigned to node2 (egress1Node)
		   3. Create one pod matching the EgressIP: running on node2 (egress1Node)
		   4. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP
		   5. Delete the egressPod and recreate it immediately with the same name
		   6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP
		   7. Repeat steps 5&6 four times and swap the pod creation between node1 (nonEgressNode) and node2 (egressNode)
		*/
		ginkgo.It("Should validate the egress IP SNAT functionality for stateful-sets", func() {
			ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to egress1Node node")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
			framework.Logf("Added egress-assignable label to node %s", egress1Node.name)
			e2enode.ExpectNodeHasLabel(context.TODO(), f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

			podNamespace := f.Namespace
			labels := map[string]string{
				"name": f.Namespace.Name,
			}
			updateNamespaceLabels(f, podNamespace, labels)

			ginkgo.By("1. Create an EgressIP object with one egress IP defined")
			var egressIP1 net.IP
			var err error
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP1, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP1, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")

			var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - ` + egressIP1.String() + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("2. Check that the status is of length one and that it is assigned to egress1Node")
			statuses := verifyEgressIPStatusLengthEquals(1, nil)
			if statuses[0].Node != egress1Node.name {
				framework.Failf("Step 2. Check that the status is of length one and that it is assigned to egress1Node, failed")
			}

			ginkgo.By("3. Create one pod matching the EgressIP: running on egress1Node")
			_, err = createGenericPodWithLabel(f, pod1Name, pod2Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			framework.ExpectNoError(err, "failed to create pod %s/%s", f.Namespace.Name, pod1Name)

			_, err = getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, pod1Name)
			framework.ExpectNoError(err, "Step 3. Create one pod matching the EgressIP: running on egress1Node, failed, err: %v", err)
			framework.Logf("Created pod %s on node %s", pod1Name, pod2Node.name)

			ginkgo.By("4. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "Step 4. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP, failed: %v", err)

			for i := 0; i < 4; i++ {
				nodeSwapName := pod2Node.name // egressNode on odd runs
				if i%2 == 0 {
					nodeSwapName = pod1Node.name // non-egressNode on even runs
				}
				ginkgo.By("5. Delete the egressPod and recreate it immediately with the same name")
				_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "delete", "pod", pod1Name, "--grace-period=0", "--force")
				framework.ExpectNoError(err, "5. Run %d: Delete the egressPod and recreate it immediately with the same name, failed: %v", i, err)
				_, err = createGenericPodWithLabel(f, pod1Name, nodeSwapName, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
				framework.ExpectNoError(err, "failed to create pod %s/%s", f.Namespace.Name, pod1Name)

				_, err := getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, pod1Name)
				framework.ExpectNoError(err, "5. Run %d: Delete the egressPod and recreate it immediately with the same name, failed, err: %v", i, err)
				framework.Logf("Created pod %s on node %s", pod1Name, nodeSwapName)
				ginkgo.By("6. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP")
				err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{egressIP1.String()}))
				framework.ExpectNoError(err, "Step 6. Run %d: Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP, failed: %v", i, err)
			}
		})

		// Validate the egress IP when a pod is managed by more than one egressIP object
		/* This test does the following:
		   0. Add the "k8s.ovn.org/egress-assignable" label to node2 (pod2Node/egress1Node)
		   1. Create one pod matching the EgressIP: running on node2 (pod2Node/egress1Node)
		   2. Create an EgressIP object1 with two egress IP's - egressIP1 and egressIP2 defined
		   3. Check that the status is of length one and that one of them is assigned to node2 (pod2Node/egress1Node) while other is pending
		   4. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object1
		   ----
		   5. Create an EgressIP object2 with one egressIP3 defined (standby egressIP)
		   6. Check that the second egressIP object is assigned to node2 (pod2Node/egress1Node)
		   7. Check the OVN DB to ensure no SNATs are added for the standby egressIP
		   8. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object1
		   ----
		   9. Delete assigned egressIP1 from egressIP object1
		   10. Check that the status is of length one and that standby egressIP3 of egressIP object2 is assigned to node2 (pod2Node/egress1Node)
		   11. Check connectivity from pod to an external container and verify that the srcIP is the expected standby egressIP3 from object2
		   12. Check the OVN DB to ensure SNATs are added for only the standby egressIP3
		   ----
		   13. Mark egress2Node (node1) as assignable and egress1Node (node2/pod2Node) as unassignable
		   14. Ensure egressIP1 from egressIP object1 and egressIP3 from object2 is correctly transferred to egress2Node
		   15. Check the OVN DB to ensure SNATs are added for either egressIP1 or egressIP3
		   16. Check connectivity from pod to an external container and verify that the srcIP is either egressIP1 or egressIP3 - no guarantee which is picked
		   ----
		   17. Delete EgressIP object that was serving the pod before in Step 16
		   18. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP which was the one not serving before
		   19. Delete the remaining egressIP object
		   20. Check connectivity from pod to an external container and verify that the srcIP is the expected nodeIP
		*/
		ginkgo.It("Should validate egress IP logic when one pod is managed by more than one egressIP object", func() {
			if isUserDefinedNetwork(netConfigParams) {
				ginkgo.Skip("Unsupported for UDNs")
			}
			ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to egress1Node node")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
			framework.Logf("Added egress-assignable label to node %s", egress1Node.name)
			e2enode.ExpectNodeHasLabel(context.TODO(), f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

			podNamespace := f.Namespace
			labels := map[string]string{
				"name": f.Namespace.Name,
			}
			updateNamespaceLabels(f, podNamespace, labels)

			ginkgo.By("1. Create one pod matching the EgressIP: running on node2 (pod2Node, egress1Node)")
			_, err := createGenericPodWithLabel(f, pod1Name, pod2Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			framework.ExpectNoError(err, "Step 1. Create one pod matching the EgressIP: running on node2 (pod2Node, egress1Node), failed, err: %v", err)
			srcPodIP, err := getPodIPWithRetry(f.ClientSet, isIPv6TestRun, podNamespace.Name, pod1Name)
			framework.ExpectNoError(err, "Step 1. Create one pod matching the EgressIP: running on node2 (pod2Node, egress1Node), failed, err: %v", err)
			framework.Logf("Created pod %s on node %s", pod1Name, pod2Node.name)

			ginkgo.By("2. Create an EgressIP object1 with two egress IP's - egressIP1 and egressIP2 defined")
			var egressIP1, egressIP2 net.IP
			var err2 error
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP1, err = allocators.NewPrimaryIPv6(egress1Node.name)
				egressIP2, err2 = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP1, err = allocators.NewPrimaryIPv4(egress1Node.name)
				egressIP2, err2 = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")
			gomega.Expect(err2).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")

			var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - ` + egressIP1.String() + `
    - ` + egressIP2.String() + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			// NOTE: Load balancing algorithm never assigns the secondIP to any node; it waits for another node to become assignable
			ginkgo.By("3. Check that the status is of length one and that one of them is assigned to node2 (pod2Node/egress1Node) while other is pending")
			statuses := verifyEgressIPStatusLengthEquals(1, nil)
			if statuses[0].Node != egress1Node.name {
				framework.Failf("Step 3. Check that the status is of length two and that one of them is assigned to node2 (pod2Node/egress1Node) while other is pending, failed")
			}
			assignedEIP := statuses[0].EgressIP
			var toKeepEIP string
			if assignedEIP == egressIP1.String() {
				toKeepEIP = egressIP2.String()
			} else {
				toKeepEIP = egressIP1.String()
			}

			ginkgo.By("4. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object1")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{assignedEIP}))
			framework.ExpectNoError(err, "Step 4. Check connectivity from pod to an zexternal container and verify that the srcIP is the expected egressIP from object1, failed: %v", err)

			ginkgo.By("5. Create an EgressIP object2 with one egress IP3 defined (standby egressIP)")
			var egressIP3 net.IP
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP3, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP3, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")

			var egressIPConfig2 = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName2 + `
spec:
    egressIPs:
    - ` + egressIP3.String() + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig2), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("6. Check that the second egressIP object is assigned to node2 (pod2Node/egress1Node)")
			egressIPs := egressIPs{}
			var egressIPStdout string
			var statusEIP1, statusEIP2 []egressIPStatus
			err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				egressIPStdout, err = e2ekubectl.RunKubectl("default", "get", "eip", "-o", "json")
				if err != nil {
					return false, err
				}
				json.Unmarshal([]byte(egressIPStdout), &egressIPs)
				if len(egressIPs.Items) != 2 {
					return false, nil
				}
				statusEIP1 = egressIPs.Items[0].Status.Items
				statusEIP2 = egressIPs.Items[1].Status.Items
				if len(statusEIP1) != 1 || len(statusEIP2) != 1 {
					return false, nil
				}
				return statusEIP1[0].Node == egress1Node.name && statusEIP2[0].Node == egress1Node.name, nil
			})
			framework.ExpectNoError(err, "Step 6. Check that the second egressIP object is assigned to node2 (pod2Node/egress1Node), failed: %v", err)

			ginkgo.By("7. Check the OVN DB to ensure no SNATs are added for the standby egressIP")
			ovnKubernetesNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
			dbPods, err := e2ekubectl.RunKubectl(ovnKubernetesNamespace, "get", "pods", "-l", "app=ovnkube-node", "--field-selector", fmt.Sprintf("spec.nodeName=%s", egress1Node.name), "-o=jsonpath='{.items..metadata.name}'")
			dbContainerName := deploymentconfig.Get().NBDBContainerName()
			if err != nil || len(dbPods) == 0 {
				framework.Failf("Error: Check the OVN DB to ensure no SNATs are added for the standby egressIP, err: %v", err)
			}
			dbPod := strings.Split(dbPods, " ")[0]
			dbPod = strings.TrimPrefix(dbPod, "'")
			dbPod = strings.TrimSuffix(dbPod, "'")
			if len(dbPod) == 0 {
				framework.Failf("Error: Check the OVN DB to ensure no SNATs are added for the standby egressIP, err: %v", err)
			}
			logicalIP := fmt.Sprintf("logical_ip=%s", srcPodIP.String())
			if isIPv6TestRun {
				logicalIP = fmt.Sprintf("logical_ip=\"%s\"", srcPodIP.String())
			}
			snats, err := e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", dbPod, "-c", dbContainerName, "--", "ovn-nbctl", "--columns=external_ip", "find", "nat", logicalIP)
			if err != nil {
				framework.Failf("Error: Check the OVN DB to ensure no SNATs are added for the standby egressIP, err: %v", err)
			}
			if !strings.Contains(snats, "\""+statuses[0].EgressIP+"\"") || strings.Contains(snats, "\""+egressIP3.String()+"\"") {
				framework.Failf("Step 7. Check the OVN DB to ensure no SNATs are added for the standby egressIP, failed")
			}

			ginkgo.By("8. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object1")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{assignedEIP}))
			framework.ExpectNoError(err, "Step 8. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object1, failed: %v", err)

			ginkgo.By("9. Delete assigned egressIP1 from egressIP object1")
			egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - ` + toKeepEIP + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Apply the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "apply", "-f", egressIPYaml)

			ginkgo.By("10. Check that the status is of length one and that standby egressIP3 of egressIP object2 is assigned to node2 (pod2Node/egress1Node)")

			err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				egressIPStdout, err = e2ekubectl.RunKubectl("default", "get", "eip", "-o", "json")
				if err != nil {
					return false, err
				}
				json.Unmarshal([]byte(egressIPStdout), &egressIPs)
				if len(egressIPs.Items) != 2 {
					return false, nil
				}
				statusEIP1 = egressIPs.Items[0].Status.Items
				statusEIP2 = egressIPs.Items[1].Status.Items
				if len(statusEIP1) != 1 || len(statusEIP2) != 1 {
					return false, nil
				}
				return statusEIP1[0].Node == egress1Node.name && statusEIP2[0].Node == egress1Node.name, nil
			})
			framework.ExpectNoError(err, "Step 10. Check that the status is of length one and that standby egressIP3 of egressIP object2 is assigned to node2 (pod2Node/egress1Node), failed: %v", err)

			ginkgo.By("11. Check connectivity from pod to an external container and verify that the srcIP is the expected standby egressIP3 from object2")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{egressIP3.String()}))
			framework.ExpectNoError(err, "Step 11. Check connectivity from pod to an external container and verify that the srcIP is the expected standby egressIP3 from object2, failed: %v", err)

			ginkgo.By("12. Check the OVN DB to ensure SNATs are added for only the standby egressIP")
			snats, err = e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", dbPod, "-c", dbContainerName, "--", "ovn-nbctl", "--columns=external_ip", "find", "nat", logicalIP)
			if err != nil {
				framework.Failf("Error: Check the OVN DB to ensure SNATs are added for only the standby egressIP, err: %v", err)
			}
			if !strings.Contains(snats, "\""+egressIP3.String()+"\"") || strings.Contains(snats, "\""+egressIP1.String()+"\"") || strings.Contains(snats, "\""+egressIP2.String()+"\"") || strings.Contains(snats, "\""+egress1Node.nodeIP+"\"") {
				framework.Failf("Step 12. Check the OVN DB to ensure SNATs are added for only the standby egressIP, failed")
			}

			ginkgo.By("13. Mark egress2Node as assignable and egress1Node as unassignable")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")
			framework.Logf("Added egress-assignable label to node %s", egress2Node.name)
			e2enode.ExpectNodeHasLabel(context.TODO(), f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")
			e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable")
			framework.Logf("Removed egress-assignable label from node %s", egress1Node.name)

			ginkgo.By("14. Ensure egressIP1 from egressIP object1 and egressIP3 from object2 is correctly transferred to egress2Node")
			err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				egressIPStdout, err = e2ekubectl.RunKubectl("default", "get", "eip", "-o", "json")
				if err != nil {
					return false, err
				}
				json.Unmarshal([]byte(egressIPStdout), &egressIPs)
				if len(egressIPs.Items) != 2 {
					return false, nil
				}
				statusEIP1 = egressIPs.Items[0].Status.Items
				statusEIP2 = egressIPs.Items[1].Status.Items
				if len(statusEIP1) != 1 || len(statusEIP2) != 1 {
					return false, nil
				}
				return statusEIP1[0].Node == egress2Node.name && statusEIP2[0].Node == egress2Node.name, nil
			})
			framework.ExpectNoError(err, "Step 14. Ensure egressIP1 from egressIP object1 and egressIP3 from object2 is correctly transferred to egress2Node, failed: %v", err)

			dbPods, err = e2ekubectl.RunKubectl(ovnKubernetesNamespace, "get", "pods", "-l", "app=ovnkube-node", "--field-selector", fmt.Sprintf("spec.nodeName=%s", egress2Node.name), "-o=jsonpath='{.items..metadata.name}'")
			if err != nil || len(dbPods) == 0 {
				framework.Failf("Error: Check the OVN DB to ensure no SNATs are added for the standby egressIP, err: %v", err)
			}
			dbPod = strings.Split(dbPods, " ")[0]
			dbPod = strings.TrimPrefix(dbPod, "'")
			dbPod = strings.TrimSuffix(dbPod, "'")
			if len(dbPod) == 0 {
				framework.Failf("Error: Check the OVN DB to ensure no SNATs are added for the standby egressIP, err: %v", err)
			}

			ginkgo.By("15. Check the OVN DB to ensure SNATs are added for either egressIP1 or egressIP3")
			// Status assignment precedes OVN programming on the destination node.
			// Wait for its database, keeping the successful result for the next steps.
			gomega.Eventually(func(g gomega.Gomega) {
				snats, err = e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", dbPod, "-c", dbContainerName, "--", "ovn-nbctl", "--columns=external_ip", "find", "nat", logicalIP)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(snats).To(gomega.Or(
					gomega.ContainSubstring("\""+egressIP3.String()+"\""),
					gomega.ContainSubstring("\""+toKeepEIP+"\""),
				))
			}).WithTimeout(retryTimeout).WithPolling(retryInterval).Should(gomega.Succeed(),
				"Step 15. Check the OVN DB to ensure SNATs are added for either egressIP1 or egressIP3")
			var toDelete, unassignedEIP string
			if strings.Contains(snats, "\""+egressIP3.String()+"\"") {
				assignedEIP = egressIP3.String()
				unassignedEIP = toKeepEIP
				toDelete = egressIPName2
				toKeepEIP = egressIPName
			} else {
				assignedEIP = toKeepEIP
				unassignedEIP = egressIP3.String()
				toDelete = egressIPName
				toKeepEIP = egressIPName2
			}

			ginkgo.By("16. Check connectivity from pod to an external container and verify that the srcIP is either egressIP1 or egressIP3")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{assignedEIP}))
			framework.ExpectNoError(err, "Step 16. Check connectivity from pod to an external container and verify that the srcIP is either egressIP1 or egressIP3, failed: %v", err)

			ginkgo.By("17. Delete EgressIP object that was serving the pod before in Step 16")
			e2ekubectl.RunKubectlOrDie("default", "delete", "eip", toDelete)

			ginkgo.By("18.  Check connectivity from pod to an external container and verify that the srcIP is the expected standby egressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{unassignedEIP}))
			framework.ExpectNoError(err, "Step 18.  Check connectivity from pod to an external container and verify that the srcIP is the expected standby egressIP, failed: %v", err)

			ginkgo.By("19. Delete the remaining egressIP object")
			e2ekubectl.RunKubectlOrDie("default", "delete", "eip", toKeepEIP)

			ginkgo.By("20. Check connectivity from pod to an external container and verify that the srcIP is the expected nodeIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{pod2Node.nodeIP}))
			framework.ExpectNoError(err, "Step 20. Check connectivity from pod to an external container and verify that the srcIP is the expected nodeIP, failed: %v", err)
		})

		/* This test does the following:
		   0. Add the "k8s.ovn.org/egress-assignable" label to two nodes
		   1. Create an EgressIP object with one egress IP defined
		   2. Check that the status is of length one and assigned to node 1
		   3. Create one pod matching the EgressIP
		   4. Make egress node 1 unreachable
		   5. Check that egress IP has been moved to other node 2 with the "k8s.ovn.org/egress-assignable" label
		   6. Check connectivity from pod to an external "node" and verify that the IP is the egress IP
		   7. Check connectivity from pod to the api-server (running hostNetwork:true) and verifying that the connection is achieved
		   8, Make node 2 unreachable
		   9. Check that egress IP is un-assigned (empty status)
		   10. Check connectivity from pod to an external "node" and verify that the IP is the node IP
		   11. Make node 1 reachable again
		   12. Check that egress IP is assigned to node 1 again
		   13. Check connectivity from pod to an external "node" and verify that the IP is the egress IP
		   14. Make node 2 reachable again
		   15. Check that egress IP remains assigned to node 1. We should not be moving the egress IP to node 2 if the node 1 works fine, as to reduce cluster entropy - read: changes.
		   16. Check connectivity from pod to an external "node" and verify that the IP is the egress IP
		   17. Make node 1 NotReady
		   18. Check that egress IP is assigned to node 2
		   19. Check connectivity from pod to an external "node" and verify that the IP is the egress IP
		   20. Make node 1 not reachable
		   21. Unlabel node 2
		   22. Check that egress IP is un-assigned (since node 1 is both unreachable and NotReady)
		   23. Make node 1 Ready
		   24. Check that egress IP is un-assigned (since node 1 is unreachable)
		   25. Make node 1 reachable again
		   26. Check that egress IP is assigned to node 1 again
		   27. Check connectivity from pod to an external "node" and verify that the IP is the egress IP
		*/
		ginkgo.It("Should re-assign egress IPs when node readiness / reachability goes down/up", func() {

			ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to two nodes")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")

			ginkgo.By("1. Create an EgressIP object with one egress IP defined")
			var egressIP1 net.IP
			var err error
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP1, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP1, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")

			podNamespace := f.Namespace
			labels := map[string]string{
				"name": f.Namespace.Name,
			}
			updateNamespaceLabels(f, podNamespace, labels)

			var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - ` + egressIP1.String() + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Applying the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("2. Check that the status is of length one")
			statuses := verifyEgressIPStatusLengthEquals(1, nil)
			node1 := statuses[0].Node

			ginkgo.By("3. Create one pod matching the EgressIP")
			_, err = createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			framework.ExpectNoError(err, "failed to create pod %s/%s", f.Namespace.Name, pod1Name)

			ginkgo.By(fmt.Sprintf("4. Make egress node: %s unreachable", node1))
			setNodeReachable(node1, false)
			otherNode := egress1Node.name
			if node1 == egress1Node.name {
				otherNode = egress2Node.name
			}
			ginkgo.By(fmt.Sprintf("5. Check that egress IP has been moved to other node: %s with the \"k8s.ovn.org/egress-assignable\" label", otherNode))
			var node2 string
			statuses = verifyEgressIPStatusLengthEquals(1, func(statuses []egressIPStatus) bool {
				node2 = statuses[0].Node
				return node2 == otherNode
			})

			ginkgo.By("6. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "6. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP, failed, err: %v", err)

			ginkgo.By("7. Check connectivity from pod to the api-server (running hostNetwork:true) and verifying that the connection is achieved")
			// CDN exposes either IPv4 and/or IPv6 API endpoint depending on cluster configuration. The network which we are testing may not support this IP family. Skip if unsupported.
			apiAddress := getApiAddress()
			if utilnet.IsIPv6String(apiAddress) == isIPv6TestRun {
				err = wait.PollImmediate(retryInterval, retryTimeout, targetDestinationAndTest(podNamespace.Name, fmt.Sprintf("https://%s/version", net.JoinHostPort(apiAddress, "443")), []string{pod1Name}))
				framework.ExpectNoError(err, "7. Check connectivity from pod to the api-server (running hostNetwork:true) and verifying that the connection is achieved, failed, err: %v", err)
			} else {
				framework.Logf("Skipping API server reachability check because IP family does not equal IP family of the EgressIP")
			}
			ginkgo.By("8, Make node 2 unreachable")
			setNodeReachable(node2, false)

			ginkgo.By("9. Check that egress IP is un-assigned (empty status)")
			verifyEgressIPStatusLengthEquals(0, nil)

			ginkgo.By("10. Check connectivity from pod to an external \"node\" and verify that the IP is the node IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{pod1Node.nodeIP}))
			framework.ExpectNoError(err, "10. Check connectivity from pod to an external \"node\" and verify that the IP is the node IP, failed, err: %v", err)

			ginkgo.By("11. Make node 1 reachable again")
			setNodeReachable(node1, true)

			ginkgo.By("12. Check that egress IP is assigned to node 1 again")
			statuses = verifyEgressIPStatusLengthEquals(1, func(statuses []egressIPStatus) bool {
				testNode := statuses[0].Node
				return testNode == node1
			})

			ginkgo.By("13. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "13. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP, failed, err: %v", err)

			ginkgo.By("14. Make node 2 reachable again")
			setNodeReachable(node2, true)

			ginkgo.By("15. Check that egress IP remains assigned to node 1. We should not be moving the egress IP to node 2 if the node 1 works fine, as to reduce cluster entropy - read: changes.")
			statuses = verifyEgressIPStatusLengthEquals(1, func(statuses []egressIPStatus) bool {
				testNode := statuses[0].Node
				return testNode == node1
			})

			ginkgo.By("17. Make node 1 NotReady")
			setNodeReady(providerCtx, node1, false)

			ginkgo.By("18. Check that egress IP is assigned to node 2")
			statuses = verifyEgressIPStatusLengthEquals(1, func(statuses []egressIPStatus) bool {
				testNode := statuses[0].Node
				return testNode == node2
			})

			ginkgo.By("19. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "19. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP, failed, err: %v", err)

			ginkgo.By("20. Make node 1 not reachable")
			setNodeReachable(node1, false)

			ginkgo.By("21. Unlabel node 2")
			e2enode.RemoveLabelOffNode(f.ClientSet, node2, "k8s.ovn.org/egress-assignable")

			ginkgo.By("22. Check that egress IP is un-assigned (since node 1 is both unreachable and NotReady)")
			verifyEgressIPStatusLengthEquals(0, nil)

			ginkgo.By("23. Make node 1 Ready")
			setNodeReady(providerCtx, node1, true)

			ginkgo.By("24. Check that egress IP is un-assigned (since node 1 is unreachable)")
			verifyEgressIPStatusLengthEquals(0, nil)

			ginkgo.By("25. Make node 1 reachable again")
			setNodeReachable(node1, true)

			ginkgo.By("26. Check that egress IP is assigned to node 1 again")
			statuses = verifyEgressIPStatusLengthEquals(1, func(statuses []egressIPStatus) bool {
				testNode := statuses[0].Node
				return testNode == node1
			})

			ginkgo.By("27. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "27. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP, failed, err: %v", err)
		})

		// Validate the egress IP works with egress firewall by creating two httpd
		// containers on the kind networking (effectively seen as "outside" the cluster)
		// and curl them from a pod in the cluster which matches the egress IP stanza.
		// The IP allowed by the egress firewall rule should work, the other not.

		/* This test does the following:
		   0. Add the "k8s.ovn.org/egress-assignable" label to one node
		   1. Create an EgressIP object with one egress IP defined
		   2. Create an EgressFirewall object with one allow rule and one "block-all" rule defined
		   3. Create two pods matching both egress firewall and egress IP
		   4. Check connectivity to the blocked IP and verify that it fails
		   5. Check connectivity to the allowed IP and verify it has the egress IP
		   6. Check connectivity to the kubernetes API IP and verify that it works [currently skipped]
		   7. Check connectivity to the other pod IP and verify that it works
		   8. Check connectivity to the service IP and verify that it works
		*/
		ginkgo.It("Should validate the egress IP functionality against remote hosts with egress firewall applied", func() {
			if isUserDefinedNetwork(netConfigParams) {
				ginkgo.Skip("Unsupported for UDNs")
			}

			ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to one nodes")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

			podNamespace := f.Namespace
			labels := map[string]string{
				"name": f.Namespace.Name,
			}
			updateNamespaceLabels(f, podNamespace, labels)

			ginkgo.By("1. Create an EgressIP object with one egress IP defined")
			var egressIP net.IP
			var err error
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")

			var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - ` + egressIP.String() + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`

			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}

			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("2. Create an EgressFirewall object with one allow rule and one \"block-all\" rule defined")

			var firewallAllowNode, firewallDenyAll string

			if isIPv6TestRun {
				firewallAllowNode = primaryTargetExternalContainer.GetIPv6() + "/128"
				firewallDenyAll = "::/0"
			} else {
				firewallAllowNode = primaryTargetExternalContainer.GetIPv4() + "/32"
				firewallDenyAll = "0.0.0.0/0"
			}

			var egressFirewallConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressFirewall
metadata:
  name: default
  namespace: ` + f.Namespace.Name + `
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: ` + firewallAllowNode + `
  - type: Deny
    to:
      cidrSelector: ` + firewallDenyAll + `
`

			if err := os.WriteFile(egressFirewallYaml, []byte(egressFirewallConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}

			defer func() {
				if err := os.Remove(egressFirewallYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressFirewallYaml)

			ginkgo.By("3. Create two pods, and matching service, matching both egress firewall and egress IP")
			_, err = createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			framework.ExpectNoError(err, "failed to create pod %s/%s", f.Namespace.Name, pod1Name)
			_, err = createGenericPodWithLabel(f, pod2Name, pod2Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			framework.ExpectNoError(err, "failed to create pod %s/%s", f.Namespace.Name, pod2Name)
			serviceIP, err := createServiceForPodsWithLabel(f, f.Namespace.Name, clusterNetworkHTTPPort, clusterNetworkHTTPPort, "ClusterIP", podEgressLabel)
			framework.ExpectNoError(err, "Step 3. Create two pods, and matching service, matching both egress firewall and egress IP, failed creating service, err: %v", err)
			for _, podName := range []string{pod1Name, pod2Name} {
				_, err = getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, podName)
				framework.ExpectNoError(err, "Step 3. Create two pods matching both egress firewall and egress IP, failed for pod %s, err: %v", podName, err)
			}

			ginkgo.By("Checking that the status is of length one")
			verifyEgressIPStatusLengthEquals(1, nil)

			ginkgo.By("4. Check connectivity to the blocked IP and verify that it fails")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryDeniedExternalContainer, podNamespace.Name, pod1Name, false, []string{egressIP.String()}))
			framework.ExpectNoError(err, "Step:  4. Check connectivity to the blocked IP and verify that it fails, failed, err: %v", err)

			ginkgo.By("5. Check connectivity to the allowed IP and verify it has the egress IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name, true, []string{egressIP.String()}))
			framework.ExpectNoError(err, "Step: 5. Check connectivity to the allowed IP and verify it has the egress IP, failed, err: %v", err)

			// TODO: in the future once we only have shared gateway mode: implement egress firewall so that
			// pods that have a "deny all 0.0.0.0/0" rule, still can connect to the Kubernetes API service
			// and re-enable this check

			// ginkgo.By("6. Check connectivity to the kubernetes API IP and verify that it works")
			// err = wait.PollImmediate(retryInterval, retryTimeout, targetAPIServiceAndTest(podNamespace.Name, []string{pod1Name, pod2Name}))
			// framework.ExpectNoError(err, "Step 6. Check connectivity to the kubernetes API IP and verify that it works, failed, err %v", err)

			ginkgo.By("7. Check connectivity to the other pod IP and verify that it works")
			pod2IP, err := getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, pod2Name)
			framework.ExpectNoError(err, "Step 7. Check connectivity to the other pod IP and verify that it works, err retrieving pod %s IP: %v", err, pod2Name)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetPodAndTest(f.Namespace.Name, pod1Name, pod2Name, pod2IP.String(), clusterNetworkHTTPPort))
			framework.ExpectNoError(err, "Step 7. Check connectivity to the other pod IP and verify that it works, err: %v", err)

			ginkgo.By("8. Check connectivity to the service IP and verify that it works")
			servicePortAsString := strconv.Itoa(int(clusterNetworkHTTPPort))
			err = wait.PollImmediate(retryInterval, retryTimeout, targetDestinationAndTest(podNamespace.Name, fmt.Sprintf("http://%s/hostname", net.JoinHostPort(serviceIP, servicePortAsString)), []string{pod1Name, pod2Name}))
			framework.ExpectNoError(err, "8. Check connectivity to the service IP and verify that it works, failed, err %v", err)
		})

		// In SGW mode we don't support doing IP fragmentation when routing for most
		// of the flows because they don't go through the host kernel and OVN/OVS
		// does not support fragmentation. This is by design.
		// In LGW mode we support doing IP fragmentation when routing for the
		// opposite reason. However, egress IP is an exception since it doesn't go
		// through the host network stack even in LGW mode. To support fragmentation
		// for this type of flow we need to explicitly send replies to egress IP
		// traffic that requires fragmentation to the host kernel and this test
		// verifies it.
		// This test is specific to IPv4 LGW mode.
		ginkgo.It("of replies to egress IP packets that require fragmentation [LGW][IPv4]", func() {
			if isIPv6TestRun {
				ginkgo.Skip("IPv4 only")
			}
			if isUserDefinedNetwork(netConfigParams) {
				//FIXME: Fragmentation is broken for user defined networks
				// Remove when https://issues.redhat.com/browse/OCPBUGS-46476 is resolved
				ginkgo.Skip("Fragmentation is not working for user defined networks")
			}
			usedEgressNodeAvailabilityHandler = &egressNodeAvailabilityHandlerViaLabel{f}

			ginkgo.By("Setting a node as available for egress")
			usedEgressNodeAvailabilityHandler.Enable(egress1Node.name)

			podNamespace := f.Namespace
			labels := map[string]string{
				"name": f.Namespace.Name,
			}
			updateNamespaceLabels(f, podNamespace, labels)

			ginkgo.By("Creating an EgressIP object with one egress IPs defined")
			var egressIP1 net.IP
			var err error
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP1, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP1, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")

			var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - ` + egressIP1.String() + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`

			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("Checking that the status is of length one and assigned to node 1")
			statuses := verifyEgressIPStatusLengthEquals(1, nil)
			if statuses[0].Node != egress1Node.name {
				framework.Failf("egress IP not assigend to node 1")
			}

			ginkgo.By("Creating a client pod labeled to use the EgressIP running on a non egress node")
			command := []string{"/agnhost", "pause"}
			_, err = createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, command, podEgressLabel)
			framework.ExpectNoError(err, "can't create a client pod: %v", err)

			ginkgo.By("Creating an external container (outside k8 cluster) as server to send the traffic to/from")
			externalContainerPrimaryPort := infraprovider.Get().GetExternalContainerPort()
			// Then create and run the server
			httpPort := fmt.Sprintf("--http-port=%d", externalContainerPrimaryPort)
			udpPort := fmt.Sprintf("--udp-port=%d", externalContainerPrimaryPort)
			providerPrimaryNetwork, err := infraprovider.Get().PrimaryNetwork()
			framework.ExpectNoError(err, "failed to get providers primary network")
			externalContainerPrimary := infraapi.ExternalContainer{Name: "external-container-for-egressip-mtu-test", Image: images.AgnHost(),
				Network: providerPrimaryNetwork, RuntimeArgs: []string{"--sysctl", "net.ipv4.ip_no_pmtu_disc=2"},
				CmdArgs: []string{"netexec", httpPort, udpPort}, ExtPort: externalContainerPrimaryPort}
			externalContainerPrimary, err = providerCtx.CreateExternalContainer(externalContainerPrimary)
			framework.ExpectNoError(err, "failed to create external container: %s", externalContainerPrimary.String())

			ginkgo.By("Checking connectivity to the external kind container and verify that the source IP is the egress IP")
			var curlErr error
			err = wait.PollUntilContextTimeout(
				context.Background(),
				retryInterval,
				retryTimeout,
				true,
				func(ctx context.Context) (bool, error) {
					curlErr = curlAgnHostClientIPFromPod(podNamespace.Name, pod1Name, egressIP1.String(), externalContainerPrimary.GetIPv4(), externalContainerPrimary.GetPortStr())
					return curlErr == nil, nil
				},
			)
			framework.ExpectNoError(err, "connectivity check to the external kind container failed: %v", curlErr)

			// We will ask the server to reply with a UDP packet bigger than the pod
			// network MTU. Since PMTUD has been disabled on the server, the reply
			// won't have the DF flag set. If the reply is not forwarded through the
			// cluster host kernel then OVN will just drop the reply and send back
			// an ICMP needs frag that the server will ignore. If the reply is
			// forwarded through cluster host kernel, it will be fragmented and sent
			// back to OVN reaching the client pod.
			ginkgo.By("Making the external kind container reply an oversized UDP packet and checking that it is recieved")
			payload := fmt.Sprintf("%01420d", 1)
			cmd := fmt.Sprintf("echo 'echo %s' | nc -w2 -u %s %s",
				payload,
				externalContainerPrimary.GetIPv4(),
				externalContainerPrimary.GetPortStr(),
			)
			stdout, err := e2epodoutput.RunHostCmd(
				podNamespace.Name,
				pod1Name,
				cmd)
			framework.ExpectNoError(err, "sending echo request to external kind container failed: %v", err)

			if stdout != payload {
				framework.Failf("external kind container did not reply with the requested payload.\nstdout: %q\n\npayload: %q\nmust be equal",
					stdout, payload)
			}

			ginkgo.By("Checking that there is no IP route exception and thus reply was fragmented")
			stdout, err = infraprovider.Get().ExecExternalContainerCommand(externalContainerPrimary, []string{"ip", "route", "get", egressIP1.String()})
			framework.ExpectNoError(err, "listing the server IP route cache failed: %v", err)

			if regexp.MustCompile(`cache expires.*mtu.*`).Match([]byte(stdout)) {
				framework.Failf("unexpected server IP route cache: %s", stdout)
			}
		})

		/* This test does the following:
		   Note: 'OVN network' here means that OVN directly controls an interface that is attached to a network. This is
		   accomplished with OVN and therefore ovs rules. 'secondary host network' means that we partly use OVN/ovs rules to route the packet
		   and also use the linux networking stack to perform the local host routing when the packet is expected to egress that
		   particular node.

		   0. Set two nodes as available for egress
		   1. Create an EgressIP object with two egress IPs - both hosted by a secondary host networks
		   2. Check that the status is of length two, not blank and both are assigned to different nodes
		   3. Check that correct Egress IPs are assigned
		   4. Create two pods matching the EgressIP: one running on each of the egress nodes
		   5. Check connectivity from both to an external "node" hosted on the secondary host network and verify expected src IPs
		   6. Check connectivity from one pod to the other and verify that the connection is achieved
		   7. Check connectivity from both pods to the api-server (running hostNetwork:true) and verifying that the connection is achieved
		   8. Update one of the pods, unmatching the EgressIP
		   9. Check connectivity from pod that isn't selected by EgressIP anymore to an external "node" on the OVN network and verify that the src IP is the node IP.
		   10. Update the unselected pod to be selected by the EgressIP
		   11. Check connectivity from both pods to an external "node" hosted on the secondary host network and verify the expected src IPs
		   12. Set one node as unavailable for egress
		   13. Check that the status is of length one
		   14. Check that correct Egress IP is assigned
		   15. Check connectivity from a pod to an external "node" on the secondary host network and verify that the src IP is the remaining egress IP
		   16. Set the other node as unavailable for egress
		   17. Check connectivity from a pod to an external "node" on the OVN network and verify that the src IP is the node IP
		   18. Check that the status is of length zero
		   19. Set a node back as available for egress
		   20. Check that the status is of length one
		   21. Check that correct Egress IP is assigned
		   22. Check connectivity from a pod to an external "node" on the secondary host network and verify that the src IP is the remaining egress IP
		   23. Set the other node back as available for egress
		   24. Check that the status is of length two
		   25. Check that correct Egress IP is assigned
		   26. Check connectivity from the other pod to an external "node" on the secondary host network and verify the expected src IPs
		*/
		table.DescribeTable("[secondary-host-eip] Using different methods to disable a node or pod availability for egress", func(egressIPIP1, egressIPIP2 string) {
			if isUserDefinedNetwork(netConfigParams) {
				ginkgo.Skip("Unsupported for UDNs")
			}
			// get v4, v6 from eips
			// check that node has both of them
			v4, v6 := getIPVersions(egressIPIP1, egressIPIP2)
			if v4 && isIPv6TestRun {
				ginkgo.Skip("IPv4 EIP but IPv6 test run")
			}
			if v6 && !isIPv6TestRun {
				ginkgo.Skip("IPv6 EIP but IPv4 test run")
			}
			egressNodeAvailabilityHandler := egressNodeAvailabilityHandlerViaLabel{f}
			ginkgo.By("0. Set two nodes as available for egress")
			egressNodeAvailabilityHandler.Enable(egress1Node.name)
			egressNodeAvailabilityHandler.Enable(egress2Node.name)
			defer egressNodeAvailabilityHandler.Restore(egress1Node.name)
			defer egressNodeAvailabilityHandler.Restore(egress2Node.name)
			podNamespace := f.Namespace
			labels := map[string]string{
				"name": f.Namespace.Name,
			}
			updateNamespaceLabels(f, podNamespace, labels)

			ginkgo.By("1. Create an EgressIP object with two egress IPs - both hosted by the same secondary host network")
			egressIPConfig := `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - "` + egressIPIP1 + `"
    - "` + egressIPIP2 + `"
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`
			// IPv6 EIP statuses are represented as 'compressed' IPv6 strings. Switch to that for comparison.
			if v6 {
				egressIPIP1 = net.ParseIP(egressIPIP1).String()
				egressIPIP2 = net.ParseIP(egressIPIP2).String()
			}
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("2. Check that the status is of length two, not blank and both are assigned to different nodes")
			statuses := verifyEgressIPStatusLengthEquals(2, nil)
			if statuses[0].Node == "" || statuses[0].Node == statuses[1].Node {
				framework.Failf("Step 2. Check that the status is of length two and that it is assigned to different nodes, "+
					"failed: status 1 has node %q and status 2 has node %q", statuses[0].Node, statuses[1].Node)
			}

			ginkgo.By("3. Check that correct Egress IPs are assigned")
			gomega.Expect(verifyEgressIPStatusContainsIPs(statuses, []string{egressIPIP1, egressIPIP2})).Should(gomega.BeTrue())

			ginkgo.By("4. Create two pods matching the EgressIP: one running on each of the egress nodes")
			createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			createGenericPodWithLabel(f, pod2Name, pod2Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			for _, podName := range []string{pod1Name, pod2Name} {
				_, err := getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, podName)
				framework.ExpectNoError(err, "Step 4. Create two pods matching an EgressIP - running pod(s) failed to get "+
					"pod %s IP(s), failed, err: %v", podName, err)
			}
			framework.Logf("Created two pods - pod %s on node %s and pod %s on node %s", pod1Name, pod1Node.name, pod2Name,
				pod2Node.name)

			ginkgo.By("5. Check connectivity from both pods to an external \"node\" hosted on the secondary host network " +
				"and verify the expected IPs")
			err := wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod1Name, true, []string{egressIPIP1, egressIPIP2}))
			framework.ExpectNoError(err, "Step 5. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is a secondary host network and verify that the src IP is the expected egressIP, failed: %v",
				podNamespace.Name, pod1Name, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{egressIPIP1, egressIPIP2}))
			framework.ExpectNoError(err, "Step 5. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is a secondary host network and verify that the src IP is the expected egressIP, failed: %v", podNamespace.Name, pod2Name, err)

			ginkgo.By("5a. Check that the EgressIP addresses have IFA_PROTO set to OVN-K (85)")
			for _, status := range statuses {
				verifyEgressIPAddrProto(status.Node, status.EgressIP)
			}

			ginkgo.By("6. Check connectivity from one pod to the other and verify that the connection is achieved")
			pod2IP, err := getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, pod2Name)
			framework.ExpectNoError(err, "Step 6. Check connectivity from one pod to the other and verify that the connection "+
				"is achieved, failed for pod %s, err: %v", pod2Name, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetPodAndTest(f.Namespace.Name, pod1Name, pod2Name, pod2IP.String(), clusterNetworkHTTPPort))
			framework.ExpectNoError(err, "Step 6. Check connectivity from one pod to the other and verify that the connection "+
				"is achieved, failed, err: %v", err)

			ginkgo.By("7. Check connectivity from both pods to the api-server (running hostNetwork:true) and verifying that " +
				"the connection is achieved")
			// CDN exposes either IPv4 and/or IPv6 API endpoint depending on cluster configuration. The network which we are testing may not support this IP family. Skip if unsupported.
			apiAddress := getApiAddress()
			if utilnet.IsIPv6String(apiAddress) == isIPv6TestRun {
				err = wait.PollImmediate(retryInterval, retryTimeout, targetDestinationAndTest(podNamespace.Name,
					fmt.Sprintf("https://%s/version", net.JoinHostPort(apiAddress, "443")), []string{pod1Name, pod2Name}))
				framework.ExpectNoError(err, "7. Check connectivity from pod to the api-server (running hostNetwork:true) and verifying that the connection is achieved, failed, err: %v", err)
			} else {
				framework.Logf("Skipping API server reachability check because IP family does not equal IP family of the EgressIP")
			}

			ginkgo.By("8. Update one of the pods, unmatching the EgressIP")
			pod2 := getPod(f, pod2Name)
			pod2.Labels = map[string]string{}
			updatePod(f, pod2)

			ginkgo.By("9. Check connectivity from pod that isn't selected by EgressIP anymore to an external \"node\" on " +
				"the OVN network and verify that the IP is the node IP.")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{pod2Node.nodeIP}))
			framework.ExpectNoError(err, "Step 9. Check connectivity from that one to an external \"node\" on the OVN "+
				"network and verify that the IP is the node IP failed: %v", err)

			ginkgo.By("10. Update the unselected pod to be selected by the EgressIP")
			pod2 = getPod(f, pod2Name)
			pod2.Labels = podEgressLabel
			updatePod(f, pod2)

			ginkgo.By("11. Check connectivity from both pods to an external \"node\" hosted on the secondary host network " +
				"and verify the expected IPs")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer, podNamespace.Name, pod1Name,
				true, []string{egressIPIP1, egressIPIP2}))
			framework.ExpectNoError(err, "Step 11. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is a secondary host network and verify that the src IP is the expected egressIP, failed, err: %v", podNamespace.Name, pod1Name, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer, podNamespace.Name, pod2Name,
				true, []string{egressIPIP1, egressIPIP2}))
			framework.ExpectNoError(err, "Step 11. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is a secondary host network and verify that the src IP is the expected egressIP, failed, err: %v", podNamespace.Name, pod2Name, err)

			ginkgo.By("12. Set one node as unavailable for egress")
			egressNodeAvailabilityHandler.Disable(egress1Node.name)

			ginkgo.By("13. Check that the status is of length one")
			statuses = verifyEgressIPStatusLengthEquals(1, nil)

			ginkgo.By("14. Check that correct Egress IP is assigned")
			gomega.Expect(verifyEgressIPStatusContainsIPs(statuses, []string{egressIPIP1}) || verifyEgressIPStatusContainsIPs(statuses, []string{egressIPIP2})).Should(gomega.BeTrue())

			ginkgo.By("15. Check connectivity from a pod to an external \"node\" on the secondary host network and " +
				"verify that the IP is the remaining egress IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod1Name, true, []string{statuses[0].EgressIP}))
			framework.ExpectNoError(err, "15. Check connectivity from a pod to an external \"node\" on the secondary host network"+
				" network and verify that the IP is the remaining egress IP, failed, err: %v", err)

			ginkgo.By("16. Set the other node as unavailable for egress")
			egressNodeAvailabilityHandler.Disable(egress2Node.name)

			ginkgo.By("17. Check connectivity from a pod to an external \"node\" on the OVN network and " +
				"verify that the IP is the node IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name,
				true, []string{pod1Node.nodeIP}))
			framework.ExpectNoError(err, "17. Check connectivity from a pod to an external \"node\" on the OVN network "+
				"and verify that the IP is the node IP for pod %s/%s and egress-ing from node %s with node IP %s: %v",
				podNamespace.Name, pod1Name, pod1Node.name, pod1Node.nodeIP, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{pod2Node.nodeIP}))
			framework.ExpectNoError(err, "17. Check connectivity from a pod to an external \"node\" on the OVN network "+
				"and verify that the IP is the node IP for pod %s/%s and egress-ing from node %s with node IP %s: %v",
				podNamespace.Name, pod2Name, pod2Node.name, pod2Node.nodeIP, err)

			ginkgo.By("18. Check that the status is of length zero")
			verifyEgressIPStatusLengthEquals(0, nil)

			ginkgo.By("19. Set a node back as available for egress")
			egressNodeAvailabilityHandler.Enable(egress1Node.name)

			ginkgo.By("20. Check that the status is of length one")
			statuses = verifyEgressIPStatusLengthEquals(1, nil)

			ginkgo.By("21. Check that correct Egress IP is assigned")
			gomega.Expect(verifyEgressIPStatusContainsIPs(statuses, []string{egressIPIP1}) || verifyEgressIPStatusContainsIPs(statuses, []string{egressIPIP2})).Should(gomega.BeTrue())

			ginkgo.By("22. Check connectivity from a pod to an external \"node\" on the secondary host network and verify " +
				"that the IP is the remaining egress IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod1Name, true, []string{statuses[0].EgressIP}))
			framework.ExpectNoError(err, "22. Check connectivity from a pod (%s/%s) to an external \"node\" on the secondary host network and verify "+
				"that the IP is the remaining egress IP, failed, err: %v", podNamespace.Name, pod1Name, err)

			ginkgo.By("23. Set the other node back as available for egress")
			egressNodeAvailabilityHandler.Enable(egress2Node.name)

			ginkgo.By("24. Check that the status is of length two")
			statuses = verifyEgressIPStatusLengthEquals(2, nil)

			ginkgo.By("25. Check that correct Egress IPs are assigned")
			gomega.Expect(verifyEgressIPStatusContainsIPs(statuses, []string{egressIPIP1, egressIPIP2})).Should(gomega.BeTrue())

			ginkgo.By("26. Check connectivity from the other pod to an external \"node\" on the secondary host network and verify the expected IPs")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{egressIPIP1, egressIPIP2}))
			framework.ExpectNoError(err, "26. Check connectivity from the other pod (%s/%s) to an external \"node\" on the "+
				"secondary host network and verify the expected IPs, failed, err: %v", podNamespace, pod2Name, err)
		}, table.Entry("IPv4", "10.10.10.100", "10.10.10.200"),
			table.Entry("IPv6 uncompressed", "2001:db8:abcd:1234:c001:0000:0000:0000", "2001:db8:abcd:1234:c002:0000:0000:0000"),
			table.Entry("IPv6 compressed", "2001:db8:abcd:1234:c001::", "2001:db8:abcd:1234:c002::"))

		/* This test does the following:
		   Note: 'OVN network' here means that OVN directly controls an interface that is attached to a network. This is
		   accomplished with OVN and therefore ovs rules. 'secondary host network' means that we partly use OVN/ovs rules to route the packet
		   and also use the linux networking stack to perform the local host routing when the packet is expected to egress that
		   particular node.

		   0. Set two nodes as available for egress
		   1. Create an EgressIP object with two egress IPs - one hosted by an OVN network and one by a secondary host network
		   2. Check that the status is of length two, not blank and both are assigned to different nodes
		   3. Check that correct Egress IPs are assigned
		   4. Create two pods matching the EgressIP: one running on each of the egress nodes
		   5. Check connectivity from a pod to an external "node" hosted on the OVN network and verify the expected src IP
		   6. Check connectivity from a pod to an external "node" hosted on the secondary host network and verify the expected src IP
		   7. Check connectivity from one pod to the other and verify that the connection is achieved
		   8. Check connectivity from both pods to the api-server (running hostNetwork:true) and verifying that the connection is achieved
		   9. Update one of the pods, unmatching the EgressIP
		   10. Check connectivity from pod that isn't selected by EgressIP anymore to an external "node" on the OVN network and verify that the IP is the node IP.
		   11. Update the unselected pod to be selected by the EgressIP
		   12. Check connectivity from both pods to an external "node" hosted on the OVN network and the src IP is the expected egressIP
		   13. Check connectivity from both pods to an external "node" hosted on the secondary host network and the src IP is the expected egressIP
		   14. Set the node hosting the OVN egress IP as unavailable
		   15. Check that the status is of length one
		   16. Check that correct Egress IP is assigned
		   17. Check connectivity from both pods to an external "node" on the secondary host network and verify the src IP is the expected egressIP
		   18. Set the other node, which is hosting the secondary host network egress IP as unavailable for egress
		   19. Check that the status is of length zero
		   20. Check connectivity from both pods to an external "node" on the OVN network and verify that the src IP is the node IPs
		   21. Set a node (hosting secondary host network EgressIP) back as available for egress
		   22. Check that the status is of length one
		   23. Check that correct Egress IP is assigned
		   24. Set the other node back as available for egress
		   25. Check that the status is of length two
		   26. Check that correct Egress IPs are assigned
		   27. Check connectivity both pods to an external "node" on the OVN network and verify the src IP is the expected egressIP
		   28. Check connectivity both pods to an external "node" on the secondary host network and verify the src IP is the expected egressIP
		*/
		ginkgo.It("[secondary-host-eip] Using different methods to disable a node or pod availability for egress", func() {
			if isUserDefinedNetwork(netConfigParams) {
				ginkgo.Skip("Unsupported for UDNs")
			}
			if utilnet.IsIPv6(net.ParseIP(egress1Node.nodeIP)) {
				ginkgo.Skip("Node does not have IPv4 address")
			}
			egressNodeAvailabilityHandler := egressNodeAvailabilityHandlerViaLabel{f}
			ginkgo.By("0. Set two nodes as available for egress")
			egressNodeAvailabilityHandler.Enable(egress1Node.name)
			egressNodeAvailabilityHandler.Enable(egress2Node.name)
			defer egressNodeAvailabilityHandler.Restore(egress1Node.name)
			defer egressNodeAvailabilityHandler.Restore(egress2Node.name)
			podNamespace := f.Namespace
			labels := map[string]string{
				"name": f.Namespace.Name,
			}
			updateNamespaceLabels(f, podNamespace, labels)

			ginkgo.By("1. Create an EgressIP object with two egress IPs - one hosted by an OVN network and one by a secondary host network")
			var egressIP net.IP
			var err error
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")
			egressIPOVN := egressIP.String()
			egressIPSecondaryHost := "10.10.10.200"
			egressIPConfig := `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - ` + egressIPOVN + `
    - ` + egressIPSecondaryHost + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`

			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("2. Check that the status is of length two, not blank and both are assigned to different nodes")
			statuses := verifyEgressIPStatusLengthEquals(2, nil)
			if statuses[0].Node == "" || statuses[0].Node == statuses[1].Node {
				framework.Failf("Step 2. Check that the status is of length two and that it is assigned to different nodes, "+
					"failed: status 1 has node %q and status 2 has node %q", statuses[0].Node, statuses[1].Node)
			}

			ginkgo.By("3. Check that correct Egress IPs are assigned")
			gomega.Expect(verifyEgressIPStatusContainsIPs(statuses, []string{egressIPOVN, egressIPSecondaryHost})).Should(gomega.BeTrue())

			ginkgo.By("4. Create two pods matching the EgressIP: one running on each of the egress nodes")
			createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			createGenericPodWithLabel(f, pod2Name, pod2Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			for _, podName := range []string{pod1Name, pod2Name} {
				_, err := getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, podName)
				framework.ExpectNoError(err, "Step 4. Create two pods matching an EgressIP - running pod(s) failed to get "+
					"pod %s IP(s), failed, err: %v", podName, err)
			}
			framework.Logf("Created two pods - pod %s on node %s and pod %s on node %s", pod1Name, pod1Node.name, pod2Name,
				pod2Node.name)

			ginkgo.By("5. Check connectivity a pod to an external \"node\" hosted on the OVN network " +
				"and verify the expected IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
				podNamespace.Name, pod1Name, true, []string{egressIPOVN}))
			framework.ExpectNoError(err, "Step 5. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is OVN network and verify that the src IP is the expected egressIP, failed: %v",
				podNamespace.Name, pod1Name, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{egressIPOVN}))
			framework.ExpectNoError(err, "Step 5. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is OVN network and verify that the src IP is the expected egressIP, failed: %v", podNamespace.Name, pod2Name, err)

			ginkgo.By("6. Check connectivity a pod to an external \"node\" hosted on a secondary host network " +
				"and verify the expected IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod1Name, true, []string{egressIPSecondaryHost}))
			framework.ExpectNoError(err, "Step 6. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is secondary host network and verify that the src IP is the expected egressIP, failed: %v",
				podNamespace.Name, pod1Name, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{egressIPSecondaryHost}))
			framework.ExpectNoError(err, "Step 6. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is secondary host network and verify that the src IP is the expected egressIP, failed: %v",
				podNamespace.Name, pod2Name, err)

			ginkgo.By("7. Check connectivity from one pod to the other and verify that the connection is achieved")
			pod2IP, err := getPodIPWithRetry(f.ClientSet, isIPv6TestRun, podNamespace.Name, pod2Name)
			framework.ExpectNoError(err, "Step 7. Check connectivity from one pod to the other and verify that the connection "+
				"is achieved, failed to get Pod %s IP(s), err: %v", pod2Name, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetPodAndTest(f.Namespace.Name, pod1Name, pod2Name, pod2IP.String(), clusterNetworkHTTPPort))
			framework.ExpectNoError(err, "Step 7. Check connectivity from one pod to the other and verify that the connection "+
				"is achieved, failed, err: %v", err)

			ginkgo.By("8. Check connectivity from both pods to the api-server (running hostNetwork:true) and verifying that " +
				"the connection is achieved")
			// CDN exposes either IPv4 and/or IPv6 API endpoint depending on cluster configuration. The network which we are testing may not support this IP family. Skip if unsupported.
			apiAddress := getApiAddress()
			if utilnet.IsIPv6String(apiAddress) == isIPv6TestRun {
				err = wait.PollImmediate(retryInterval, retryTimeout, targetDestinationAndTest(podNamespace.Name,
					fmt.Sprintf("https://%s/version", net.JoinHostPort(apiAddress, "443")), []string{pod1Name, pod2Name}))
				framework.ExpectNoError(err, "8. Check connectivity from pod to the api-server (running hostNetwork:true) and verifying that the connection is achieved, failed, err: %v", err)
			} else {
				framework.Logf("Skipping API server reachability check because IP family does not equal IP family of the EgressIP")
			}

			ginkgo.By("9. Update one of the pods, unmatching the EgressIP")
			pod2 := getPod(f, pod2Name)
			pod2.Labels = map[string]string{}
			updatePod(f, pod2)

			ginkgo.By("10. Check connectivity from pod that isn't selected by EgressIP anymore to an external \"node\" on " +
				"the OVN network and verify that the IP is the node IP.")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{pod2Node.nodeIP}))
			framework.ExpectNoError(err, "Step 10. Check connectivity from that one to an external \"node\" on the OVN "+
				"network and verify that the IP is the node IP failed: %v", err)

			ginkgo.By("11. Update the unselected pod to be selected by the Egress IP")
			pod2 = getPod(f, pod2Name)
			pod2.Labels = podEgressLabel
			updatePod(f, pod2)

			ginkgo.By("12. Check connectivity from both pods to an external \"node\" hosted on the OVN network " +
				"and verify that the expected IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod1Name,
				true, []string{egressIPOVN}))
			framework.ExpectNoError(err, "Step 12. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is OVN network and verify that the src IP is the expected egress IP, failed, err: %v", podNamespace.Name, pod1Name, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, podNamespace.Name, pod2Name,
				true, []string{egressIPOVN}))
			framework.ExpectNoError(err, "Step 12. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is OVN network and verify that the src IP is the expected egress IP, failed, err: %v", podNamespace.Name, pod2Name, err)

			ginkgo.By("13. Check connectivity from both pods to an external \"node\" hosted on secondary host network " +
				"and verify that the expected IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer, podNamespace.Name, pod1Name,
				true, []string{egressIPSecondaryHost}))
			framework.ExpectNoError(err, "Step 13. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that isn't OVN network and verify that the src IP is the expected egress IP, failed, err: %v", podNamespace.Name, pod1Name, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer, podNamespace.Name, pod2Name,
				true, []string{egressIPSecondaryHost}))
			framework.ExpectNoError(err, "Step 13. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that isn't OVN network and verify that the src IP is the expected egress IP, failed, err: %v", podNamespace.Name, pod2Name, err)

			ginkgo.By("14. Set the node hosting the OVN egress IP as unavailable")
			var nodeNameHostingOVNEIP, nodeNameHostingSecondaryHostEIP string
			for _, status := range statuses {
				if status.EgressIP == egressIPOVN {
					nodeNameHostingOVNEIP = status.Node
				} else if status.EgressIP == egressIPSecondaryHost {
					nodeNameHostingSecondaryHostEIP = status.Node
				}
			}
			gomega.Expect(nodeNameHostingOVNEIP).ShouldNot(gomega.BeEmpty())
			gomega.Expect(nodeNameHostingSecondaryHostEIP).ShouldNot(gomega.BeEmpty())
			egressNodeAvailabilityHandler.Disable(nodeNameHostingOVNEIP)

			ginkgo.By("15. Check that the status is of length one")
			statuses = verifyEgressIPStatusLengthEquals(1, nil)

			ginkgo.By("16. Check that correct Egress IP is assigned")
			gomega.Expect(verifyEgressIPStatusContainsIPs(statuses, []string{egressIPSecondaryHost})).Should(gomega.BeTrue())

			ginkgo.By("17. Check connectivity from both pods to an external \"node\" on the secondary host network and " +
				"verify that the src IP is the expected egress IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod1Name, true, []string{statuses[0].EgressIP}))
			framework.ExpectNoError(err, "17. Check connectivity from both pods (%s/%s) to an external \"node\" on the secondary host"+
				" network and verify that the src IP is the expected egress IP, failed, err: %v", podNamespace.Name, pod1Name, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{statuses[0].EgressIP}))
			framework.ExpectNoError(err, "17. Check connectivity from both pods (%s/%s) to an external \"node\" on the secondary host network"+
				" network and verify that the src IP is the expected egress IP, failed, err: %v", podNamespace.Name, pod2Name, err)

			ginkgo.By("18. Set the other node, which is hosting the secondary host network egress IP as unavailable for egress")
			egressNodeAvailabilityHandler.Disable(nodeNameHostingSecondaryHostEIP)

			ginkgo.By("19. Check that the status is of length zero")
			statuses = verifyEgressIPStatusLengthEquals(0, nil)

			ginkgo.By("20. Check connectivity from both pods to an external \"node\" on the OVN network and verify that the src IP is the node IPs")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
				podNamespace.Name, pod1Name, true, []string{pod1Node.nodeIP}))
			framework.ExpectNoError(err, "20. Check connectivity from both pods (%s/%s) to an external \"node\" on the "+
				"OVN network and verify that the src IP is the node IP %s, failed: %v", podNamespace, pod1Name, pod1Node.nodeIP, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{pod2Node.nodeIP}))
			framework.ExpectNoError(err, "20. Check connectivity from both pods (%s/%s) to an external \"node\" on the "+
				"OVN network and verify that the src IP is the node IP %s, failed: %v", podNamespace, pod2Name, pod2Node.nodeIP, err)

			ginkgo.By("21. Set a node (hosting secondary host network Egress IP) back as available for egress")
			egressNodeAvailabilityHandler.Enable(nodeNameHostingSecondaryHostEIP)

			ginkgo.By("22. Check that the status is of length one")
			statuses = verifyEgressIPStatusLengthEquals(1, nil)

			ginkgo.By("23. Check that correct Egress IP is assigned")
			gomega.Expect(verifyEgressIPStatusContainsIPs(statuses, []string{egressIPSecondaryHost}) || verifyEgressIPStatusContainsIPs(statuses, []string{egressIPOVN})).Should(gomega.BeTrue())

			ginkgo.By("24. Set the other node back as available for egress")
			egressNodeAvailabilityHandler.Enable(nodeNameHostingOVNEIP)

			ginkgo.By("25. Check that the status is of length two")
			statuses = verifyEgressIPStatusLengthEquals(2, nil)

			ginkgo.By("26. Check that correct Egress IPs are assigned")
			gomega.Expect(verifyEgressIPStatusContainsIPs(statuses, []string{egressIPOVN, egressIPSecondaryHost})).Should(gomega.BeTrue())

			ginkgo.By("27. Check connectivity from both pods to an external \"node\" on the OVN network and verify the src IP is the expected egress IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
				podNamespace.Name, pod1Name, true, []string{egressIPOVN}))
			framework.ExpectNoError(err, "Step 27. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is OVN network and verify that the src IP is the expected egress IP, failed: %v", podNamespace.Name, pod1Name, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{egressIPOVN}))
			framework.ExpectNoError(err, "Step 27. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is OVN network and verify that the src IP is the expected egress IP, failed: %v", podNamespace.Name, pod2Name, err)

			ginkgo.By("28. Check connectivity both pods to an external \"node\" on the secondary host network and verify the src IP is the expected egress IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod1Name, true, []string{egressIPSecondaryHost}))
			framework.ExpectNoError(err, "Step 28. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is secondary host network and verify that the src IP is the expected egress IP, failed: %v", podNamespace.Name, pod1Name, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{egressIPSecondaryHost}))
			framework.ExpectNoError(err, "Step 28. Check connectivity from pod (%s/%s) to an external container attached to "+
				"a network that is secondary host network and verify that the src IP is the expected egress IP, failed: %v",
				podNamespace.Name, pod2Name, err)
		})

		// Multiple EgressIP objects where the Egress IPs of both objects are hosted on the same interface on a secondary host network
		// 0. Set one nodes as available for egress
		// 1. Create two EgressIP objects with one egress IP each - hosted by a secondary host network
		// 2. Check that status of both EgressIP objects is of length one
		// 3. Create two pods - one matching each EgressIP
		// 4. Check connectivity from both pods to an external "node" hosted on a secondary host network and verify the expected IPs
		// 5. Delete one EgressIP object
		// 6. Check connectivity to the host on the secondary host network from the pod selected by the other EgressIP
		// 7. Check connectivity to the host on the OVN network from the pod not selected by EgressIP
		ginkgo.It("[secondary-host-eip] Multiple EgressIP objects and their Egress IP hosted on the same interface", func() {
			if isUserDefinedNetwork(netConfigParams) {
				ginkgo.Skip("Unsupported for UDNs")
			}
			var egressIP1, egressIP2 string
			if utilnet.IsIPv6(net.ParseIP(egress1Node.nodeIP)) {
				egressIP1 = "2001:db8:abcd:1234:c001::"
				egressIP2 = "2001:db8:abcd:1234:c002::"

			} else {
				egressIP1 = "10.10.10.100"
				egressIP2 = "10.10.10.200"
			}
			egressNodeAvailabilityHandler := egressNodeAvailabilityHandlerViaLabel{f}
			ginkgo.By("0. Set one nodes as available for egress")
			egressNodeAvailabilityHandler.Enable(egress1Node.name)
			defer egressNodeAvailabilityHandler.Restore(egress1Node.name)
			podNamespace := f.Namespace
			labels := map[string]string{
				"name": f.Namespace.Name,
			}
			updateNamespaceLabels(f, podNamespace, labels)

			ginkgo.By("1. Create two EgressIP objects with one egress IP each - hosted by a secondary host network")
			egressIPConfig := `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - "` + egressIP1 + `"
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`

			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			framework.Logf("Create the first EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)
			egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName2 + `
spec:
    egressIPs:
    - "` + egressIP2 + `"
    podSelector:
        matchLabels:
            wants: egress2
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("2. Check that status of both EgressIP objects is of length one")
			verifySpecificEgressIPStatusLengthEquals(egressIPName, 1, nil)
			verifySpecificEgressIPStatusLengthEquals(egressIPName2, 1, nil)

			ginkgo.By("3. Create two pods - one matching each EgressIP")
			_, err := createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			podEgressLabel2 := map[string]string{
				"wants": "egress2",
			}
			createGenericPodWithLabel(f, pod2Name, pod2Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel2)
			for _, podName := range []string{pod1Name, pod2Name} {
				_, err := getPodIPWithRetry(f.ClientSet, isIPv6TestRun, podNamespace.Name, podName)
				framework.ExpectNoError(err, "Step 3. Create two pods - one matching each EgressIP, failed for pod %s, err: %v", podName, err)
			}

			ginkgo.By("4. Check connectivity from both pods to an external \"node\" hosted on a secondary host network " +
				"and verify the expected IPs")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod1Name, true, []string{egressIP1}))
			framework.ExpectNoError(err, "4. Check connectivity from both pods to an external \"node\" hosted on a secondary host network "+
				"and verify the expected IPs, failed for EgressIP %s: %v", egressIPName, err)
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{egressIP2}))
			framework.ExpectNoError(err, "4. Check connectivity from both pods to an external \"node\" hosted on a secondary host network "+
				"and verify the expected IPs, failed for EgressIP %s: %v", egressIPName2, err)

			ginkgo.By("5. Delete one EgressIP object")
			e2ekubectl.RunKubectlOrDie("default", "delete", "eip", egressIPName, "--ignore-not-found=true")

			ginkgo.By("6. Check connectivity to the host on the secondary host network from the pod selected by the other EgressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod2Name, true, []string{egressIP2}))
			framework.ExpectNoError(err, "6. Check connectivity to the host on the secondary host network from the pod "+
				"selected by the other EgressIP, failed: %v", err)

			ginkgo.By("7. Check connectivity to the host on the OVN network from the pod not selected by EgressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer,
				podNamespace.Name, pod1Name, true, []string{pod1Node.nodeIP}))
			framework.ExpectNoError(err, "7. Check connectivity to the host on the OVN network from the pod not selected by EgressIP, failed: %v", err)
		})

		// Single EgressIP object where the Egress IP of object is hosted on a single interface thats enslaved to a VRF device on a secondary host network
		// 0. create VRF and enslave expected egress interface
		// 1. Set one node as available for egress
		// 2. Create one EgressIP object with one egress IP hosted by a secondary host network
		// 3. Check that status of EgressIP object is of length one
		// 4. Create a pod matching the EgressIP
		// 5. Check connectivity from a pod to an external "node" hosted on a secondary host network and verify the expected IP
		ginkgo.It("[secondary-host-eip] uses VRF routing table if EIP assigned interface is VRF slave", func() {
			if !isKernelModuleLoaded(egress1Node.name, "vrf") {
				ginkgo.Skip("Node doesn't have VRF kernel module loaded")
			}
			if isUserDefinedNetwork(netConfigParams) {
				ginkgo.Skip("Unsupported for UDNs")
			}
			var egressIP1 string
			isV6Node := utilnet.IsIPv6(net.ParseIP(egress1Node.nodeIP))
			if isV6Node {
				egressIP1 = "2001:db8:abcd:1234:c001::"
			} else {
				egressIP1 = "10.10.10.100"
			}
			ginkgo.By("0. create VRF and enslave expected egress interface")
			vrfName := "egress-vrf"
			vrfRoutingTable := "99999"
			// find the egress interface name
			out, err := infraprovider.Get().ExecK8NodeCommand(egress1Node.name, []string{"ip", "-o", "route", "get", egressIP1})
			if err != nil {
				framework.Failf("failed to add expected EIP assigned interface, err %v, out: %s", err, out)
			}
			var egressInterface string
			outSplit := strings.Split(out, " ")
			for i, entry := range outSplit {
				if entry == "dev" && i+1 < len(outSplit) {
					egressInterface = outSplit[i+1]
					break
				}
			}
			if egressInterface == "" {
				framework.Failf("failed to find egress interface name")
			}
			// Enslaving a link to a VRF device may cause the removal of the non link local IPv6 address from the interface
			// Look up the IP address, add it after enslaving the link and perform test.
			secondaryNetwork, err := infraprovider.Get().GetNetwork(secondaryNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "network %s must exist", secondaryNetworkName)
			restoreLinkIPv6AddrFn := func() error { return nil }
			if isV6Node {
				ginkgo.By("attempting to find IPv6 global address for secondary network")
				inf, err := infraprovider.Get().GetK8NodeNetworkInterface(egress1Node.name, secondaryNetwork)
				framework.ExpectNoError(err, "failed to get network interface for network %s on instance %s", secondaryNetwork.Name(), egress1Node.name)
				gomega.Expect(net.ParseIP(inf.IPv6)).ShouldNot(gomega.BeNil(), "IPv6 address for secondary network must be present")
				_, err = strconv.Atoi(inf.IPv6Prefix)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "requires valid IPv6 address prefix")
				restoreLinkIPv6AddrFn = func() error {
					_, err := infraprovider.Get().ExecK8NodeCommand(egress1Node.name, []string{
						"ip", "-6", "address", "add",
						fmt.Sprintf("%s/%s", inf.IPv6, inf.IPv6Prefix), "dev", egressInterface, "nodad", "scope", "global",
					})
					return err
				}
			}
			_, err = infraprovider.Get().ExecK8NodeCommand(egress1Node.name, []string{"ip", "link", "add", vrfName, "type", "vrf", "table", vrfRoutingTable})
			framework.ExpectNoError(err, "failed to add VRF to node %s: %v", egress1Node.name)
			providerCtx.AddCleanUpFn(func() error {
				_, err := infraprovider.Get().ExecK8NodeCommand(egress1Node.name, []string{
					"ip", "link", "del", vrfName,
				})
				return err
			})
			_, err = infraprovider.Get().ExecK8NodeCommand(egress1Node.name, []string{"ip", "link", "set", "dev", egressInterface, "master", vrfName})
			framework.ExpectNoError(err, "failed to enslave interface %s to VRF %s node %s", egressInterface, vrfName, egress1Node.name)

			if isV6Node {
				gomega.Expect(restoreLinkIPv6AddrFn()).Should(gomega.Succeed(), "restoring IPv6 address should succeed")
			}
			egressNodeAvailabilityHandler := egressNodeAvailabilityHandlerViaLabel{f}
			ginkgo.By("1. Set one node as available for egress")
			egressNodeAvailabilityHandler.Enable(egress1Node.name)
			defer egressNodeAvailabilityHandler.Restore(egress1Node.name)
			podNamespace := f.Namespace
			labels := map[string]string{
				"name": f.Namespace.Name,
			}
			updateNamespaceLabels(f, podNamespace, labels)

			ginkgo.By("2. Create one EgressIP object with one egress IP hosted by a secondary host network")
			egressIPConfig := `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - "` + egressIP1 + `"
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`

			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)
			ginkgo.By("3. Check that status of EgressIP object is of length one")
			verifySpecificEgressIPStatusLengthEquals(egressIPName, 1, nil)
			ginkgo.By("4. Create a pod matching the EgressIP")
			createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			_, err = getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, pod1Name)
			framework.ExpectNoError(err, "Step 4. Create a pod matching the EgressIP, failed, err: %v", err)
			ginkgo.By("5. Check connectivity from a pod to an external \"node\" hosted on a secondary host network " +
				"and verify the expected IP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(secondaryTargetExternalContainer,
				podNamespace.Name, pod1Name, true, []string{egressIP1}))
			framework.ExpectNoError(err, "5. Check connectivity a pod to an external \"node\" hosted on a secondary host network "+
				"and verify the expected IP, failed for EgressIP %s: %v", egressIPName, err)
		})

		ginkgo.It("[secondary-host-eip] should send address advertisements for EgressIP", func() {
			if isUserDefinedNetwork(netConfigParams) {
				ginkgo.Skip("Unsupported for UDNs")
			}

			egressIPSecondaryHost := "10.10.10.220"
			isV6Node := utilnet.IsIPv6(net.ParseIP(egress1Node.nodeIP))
			if isV6Node {
				egressIPSecondaryHost = "2001:db8:abcd:1234:c001::"
			}

			// flush any potentially stale MACs
			_, err := infraprovider.Get().ExecExternalContainerCommand(secondaryTargetExternalContainer,
				[]string{"ip", "neigh", "flush", egressIPSecondaryHost})
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should flush neighbor cache")

			secondaryNetwork, err := infraprovider.Get().GetNetwork(secondaryNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "network %s must exist", secondaryNetworkName)

			inf, err := infraprovider.Get().GetExternalContainerNetworkInterface(secondaryTargetExternalContainer, secondaryNetwork)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should have network interface for network %s on instance %s", secondaryNetwork.Name(), secondaryTargetExternalContainer.Name)

			// The following is required for the test purposes since we are sending and unsolicited advertisement
			// for an address that is not tracked already
			if !isV6Node {
				_, err = infraprovider.Get().ExecExternalContainerCommand(secondaryTargetExternalContainer,
					[]string{"sysctl", "-w", fmt.Sprintf("net.ipv4.conf.%s.arp_accept=1", inf.InfName)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should enable arp_accept")
			} else {
				_, err = infraprovider.Get().ExecExternalContainerCommand(secondaryTargetExternalContainer,
					[]string{"sysctl", "-w", fmt.Sprintf("net.ipv6.conf.%s.forwarding=1", inf.InfName)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should enable forwarding")

				_, err = infraprovider.Get().ExecExternalContainerCommand(secondaryTargetExternalContainer,
					[]string{"sysctl", "-w", fmt.Sprintf("net.ipv6.conf.%s.accept_untracked_na=1", inf.InfName)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should enable accept_untracked_na")
			}

			podNamespace := f.Namespace
			labels := map[string]string{"name": f.Namespace.Name}
			updateNamespaceLabels(f, podNamespace, labels)

			ginkgo.By("Labeling node as available for egress")
			egressNodeAvailabilityHandler := egressNodeAvailabilityHandlerViaLabel{f}
			egressNodeAvailabilityHandler.Enable(egress1Node.name)
			defer egressNodeAvailabilityHandler.Restore(egress1Node.name)

			_, err = createGenericPodWithLabel(f, pod1Name, egress1Node.name, f.Namespace.Name, []string{"/agnhost", "pause"}, podEgressLabel)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should create egress pod")

			egressIPConfig := `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - "` + egressIPSecondaryHost + `"
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			status := verifyEgressIPStatusLengthEquals(1, nil)
			inf, err = infraprovider.Get().GetK8NodeNetworkInterface(status[0].Node, secondaryNetwork)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should have network interface for network %s on instance %s", secondaryNetwork.Name(), egress1Node.name)

			ginkgo.By("Verifying EgressIP address has IFA_PROTO set to OVN-K (85)")
			verifyEgressIPAddrProto(status[0].Node, status[0].EgressIP)

			ginkgo.By("Verifying neighbor table")
			var neighborMAC string
			gomega.Eventually(func() bool {
				output, err := infraprovider.Get().ExecExternalContainerCommand(secondaryTargetExternalContainer,
					[]string{"ip", "-j", "neigh", "show", egressIPSecondaryHost})
				if err != nil {
					framework.Logf("Failed to get neighbor table: %v", err)
					return false
				}

				var neighbors []IpNeighbor
				if err := json.Unmarshal([]byte(output), &neighbors); err != nil {
					framework.Logf("Failed to parse neighbor JSON: %v", err)
					return false
				}

				for _, n := range neighbors {
					if n.Lladdr != "" {
						neighborMAC = n.Lladdr
						framework.Logf("Neighbor entry found for %s -> MAC %s", egressIPSecondaryHost, neighborMAC)
						return true
					}
				}
				return false
			}, 30*time.Second, 2*time.Second).Should(gomega.BeTrue(),
				"Neighbor entry should appear")
			gomega.Expect(neighborMAC).Should(gomega.Equal(inf.MAC), "neighbor entry should have the correct MAC address")
		})

		var _ = ginkgo.Describe("[secondary-host-eip] Egress IP Traffic Leak Prevention with Packet Mark Validation", func() {
			const (
				egressIPName   = "egressip-pktmark-validation-test"
				egressIPYaml   = "/tmp/egressip-pktmark-validation.yaml"
				monitorPodName = "traffic-monitor-existing"
				podNamePrefix  = "existing-pod"
				numTestPods    = 2 // Create multiple pods to stress reconciliation
				retryInterval  = 1 * time.Second
				retryTimeout   = 120 * time.Second
			)

			var (
				// Pod IPs to check for leaks
				podIPs     []string
				podIPsLock sync.Mutex
			)

			ginkgo.BeforeEach(func() {
				podIPsLock.Lock()
				podIPs = []string{}
				podIPsLock.Unlock()
			})

			ginkgo.It("Should prevent pod IP traffic leak when EgressIP on secondary interface is applied to existing running pods", func() {
				// Test Overview:
				// This test validates a different race condition scenario:
				// 1. Pods are created FIRST and are actively sending traffic
				// 2. Then an EgressIP object is created and applied to them
				// 3. During the reconciliation window, traffic should NOT leak with pod IPs
				//
				// This tests the transition from "normal routing" to "egress IP routing"
				// which is common in production when EgressIP policies are added to existing workloads.

				if isUserDefinedNetwork(netConfigParams) {
					ginkgo.Skip("Unsupported for UDNs")
				}

				ginkgo.By("Step 0: Setting up egress IP address")

				// Determine egress IP based on IP family
				// Use SubTree's existing secondary network infrastructure
				egressIPAddr := "10.10.10.200" // IPv4 egress IP (different from other test)
				if isIPv6TestRun {
					egressIPAddr = "2001:db8:abcd:1234::200" // IPv6 egress IP (different from other test)
				}

				targetIP := secondaryTargetExternalContainer.GetIPv4()
				if isIPv6TestRun {
					targetIP = secondaryTargetExternalContainer.GetIPv6()
				}
				framework.Logf("Using external target container with IP %s", targetIP)

				ginkgo.By("Step 1: Label egress node for future egress IP assignment")
				labelNodeForEgress(f, egress1Node.name)
				defer unlabelNodeForEgress(f, egress1Node.name)

				ginkgo.By("Step 2: Label namespace for egress IP selection")
				podNamespace := f.Namespace
				labels := map[string]string{
					"egress-test": "existing-pods",
				}
				updateNamespaceLabels(f, podNamespace, labels)

				ginkgo.By("Step 3: Start traffic monitoring BEFORE creating EgressIP and pods")
				// Starts monitor pod with tcpdump using filter to get traffic from
				// egress node secondary interface, and filter it based on external node IP address
				// and source address from pod IPs subnet.
				// Given this filter, any tcpdump packet output is a traffic leak.
				secondaryNetwork, err := infraprovider.Get().GetNetwork(secondaryNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "network %s must exist", secondaryNetworkName)
				secondaryIface, err := infraprovider.Get().GetK8NodeNetworkInterface(egress1Node.name, secondaryNetwork)
				framework.ExpectNoError(err, "failed to get network interface for network %s on node %s", secondaryNetworkName, egress1Node.name)

				podV4CIDR, podV6CIDR, err := getNodePodCIDRs(pod1Node.name, "default")
				framework.ExpectNoError(err, "failed to get pod CIDRs for node %s", pod1Node.name)
				podCIDR := podV4CIDR
				if isIPv6TestRun {
					podCIDR = podV6CIDR
				}
				gomega.Expect(podCIDR).NotTo(gomega.BeEmpty(), "pod CIDR must not be empty for node %s", pod1Node.name)
				monitorFilter := fmt.Sprintf("src net %s and dst host %s", podCIDR, targetIP)

				// Create the monitor pod and wait (up to retryTimeout) until it is Running
				// (capturing) before generating any traffic, so the capture cannot miss the
				// EgressIP transition and reading the capture in Step 8 cannot race the pod
				// startup. A failure or timeout to start aborts the spec here.
				startTcpdumpMonitorPodOnNode(f, retryTimeout, monitorPodName, egress1Node.name, secondaryIface.InfName,
					"-n -vv -l", monitorFilter)
				framework.Logf("Traffic monitor pod started on node %s", egress1Node.name)

				ginkgo.By(fmt.Sprintf("Step 4: Create %d pods FIRST (before EgressIP exists)", numTestPods))
				// Pods are created before EgressIP, they will initially use normal routing (their pod IPs as source)

				podEgressLabel := map[string]string{
					"egress-existing-pod": "true",
				}

				// Create pods with continuous traffic generation
				// These pods will keep sending traffic throughout the test
				eg := errgroup.Group{}
				for i := 0; i < numTestPods; i++ {
					podName := fmt.Sprintf("%s-%d", podNamePrefix, i)
					eg.Go(func() error {
						// Main container continuously pings the target
						// This ensures traffic is flowing when EgressIP is applied
						mainCommand := []string{
							"/bin/sh",
							"-c",
							fmt.Sprintf("while true; do ping -c 1 -W 1 %s || true; sleep 0.5; done", targetIP),
						}

						createPod := &corev1.Pod{
							ObjectMeta: metav1.ObjectMeta{
								Name:      podName,
								Namespace: podNamespace.Name,
								Labels:    podEgressLabel,
							},
							Spec: corev1.PodSpec{
								NodeSelector: map[string]string{
									"kubernetes.io/hostname": pod1Node.name,
								},
								Containers: []corev1.Container{
									{
										Name:    "continuous-ping",
										Image:   images.AgnHost(),
										Command: mainCommand,
									},
								},
								RestartPolicy: corev1.RestartPolicyNever,
							},
						}

						createdPod, err := f.ClientSet.CoreV1().Pods(podNamespace.Name).Create(context.TODO(), createPod, metav1.CreateOptions{})
						if err != nil {
							return fmt.Errorf("failed to create pod %s: %v", podName, err)
						}

						// Wait for pod to be running
						err = pod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, createdPod)
						if err != nil {
							return fmt.Errorf("failed waiting for pod %s to be running: %v", podName, err)
						}

						// Get pod IP
						podIP, err := getPodIPWithRetry(f.ClientSet, isIPv6TestRun, podNamespace.Name, podName)
						if err != nil {
							return fmt.Errorf("failed to get IP for pod %s: %v", podName, err)
						}

						podIPsLock.Lock()
						podIPs = append(podIPs, podIP.String())
						podIPsLock.Unlock()

						framework.Logf("Created pod %s with IP %s", podName, podIP.String())
						return nil
					})
				}

				// Check for any pod creation errors
				if err := eg.Wait(); err != nil {
					framework.Failf("Failed to create some pods: %v", err)
				}

				framework.Logf("Successfully created %d pods, collected %d pod IPs", numTestPods, len(podIPs))
				defer func() {
					ginkgo.By("Cleanup - Delete test pods")
					for i := 0; i < numTestPods; i++ {
						podName := fmt.Sprintf("%s-%d", podNamePrefix, i)
						err := f.ClientSet.CoreV1().Pods(podNamespace.Name).Delete(context.TODO(), podName, metav1.DeleteOptions{})
						if err != nil {
							framework.Logf("Warning: failed to delete pod %s: %v", podName, err)
						}
					}
				}()

				ginkgo.By("Step 5: Create EgressIP object and apply it to existing running pods")
				// THIS IS THE CRITICAL MOMENT: EgressIP is created while pods are actively sending traffic
				// We want to detect any leaks during the reconciliation window

				// Create the EgressIP CRD
				egressIPConfig := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: %s
spec:
  egressIPs:
  - "%s"
  podSelector:
    matchLabels:
      egress-existing-pod: "true"
  namespaceSelector:
    matchLabels:
      egress-test: existing-pods
`, egressIPName, egressIPAddr)

				// Normalize IPv6 address for comparison
				normalizedEgressIP := egressIPAddr
				if isIPv6TestRun {
					normalizedEgressIP = net.ParseIP(egressIPAddr).String()
				}

				err = os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644)
				framework.ExpectNoError(err, "Failed to write EgressIP YAML")
				defer func() {
					if err := os.Remove(egressIPYaml); err != nil {
						framework.Logf("Unable to remove the egressIPYaml CRD config from disk: %v", err)
					}
				}()
				framework.Logf("Creating EgressIP object with IP %s for existing running pods", normalizedEgressIP)
				e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)
				defer func() {
					framework.Logf("Deleting EgressIP object")
					e2ekubectl.RunKubectlOrDie("default", "delete", "-f", egressIPYaml, "--ignore-not-found=true")
				}()

				ginkgo.By("Step 6: Verify EgressIP status is assigned to egress node")
				var egressIPStatusItems []egressIPStatus
				err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(ctx context.Context) (bool, error) {
					egressIPStatusItems = verifyEgressIPStatusLengthEquals(1, nil)
					if len(egressIPStatusItems) != 1 {
						return false, nil
					}
					if egressIPStatusItems[0].Node != egress1Node.name {
						framework.Logf("EgressIP not yet assigned to correct node, current: %s, expected: %s",
							egressIPStatusItems[0].Node, egress1Node.name)
						return false, nil
					}
					if egressIPStatusItems[0].EgressIP != normalizedEgressIP {
						framework.Logf("EgressIP address mismatch, current: %s, expected: %s",
							egressIPStatusItems[0].EgressIP, normalizedEgressIP)
						return false, nil
					}
					return true, nil
				})
				framework.ExpectNoError(err, "Failed to verify EgressIP status")
				framework.Logf("EgressIP %s assigned to node %s", normalizedEgressIP, egress1Node.name)

				ginkgo.By("Step 7: Wait for reconciliation to complete and verify egress IP is used")

				// Verify pods are now using the egress IP
				for i := 0; i < numTestPods; i++ {
					podName := fmt.Sprintf("%s-%d", podNamePrefix, i)
					conditionFunc := targetExternalContainerAndTest(secondaryTargetExternalContainer, podNamespace.Name, podName, true, []string{normalizedEgressIP})
					err = wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(ctx context.Context) (bool, error) {
						return conditionFunc()
					})
					framework.ExpectNoError(err, "Pod %s should be using egress IP after reconciliation", podName)
				}
				framework.Logf("Verified pods are now using egress IP %s correctly", normalizedEgressIP)

				ginkgo.By("Step 8: Analyze traffic capture for pod IP leaks during EgressIP application")
				// This is the critical check: did any traffic leak with pod IPs during the transition?
				// tcpdump keeps running; kubectl logs reads the capture so far (line-buffered via -l).
				monitorOutput, monitorErr := e2ekubectl.NewKubectlCommand(f.Namespace.Name, "logs", monitorPodName).
					WithTimeout(time.After(retryTimeout)).Exec()
				framework.ExpectNoError(monitorErr, "Failed to read monitor pod logs")
				if strings.Contains(monitorOutput, targetIP) {
					framework.Failf("TRAFFIC LEAK DETECTED! Pod IPs were seen in traffic to %s"+
						"This indicates traffic leaked with pod source IPs instead of egress IP %s during EgressIP application to existing pods.\ntcpdump output:\n%s",
						targetIP, normalizedEgressIP, monitorOutput)
				}
			})
		})

		// two pods attached to different namespaces but the same role primary user defined network
		// One pod is deleted and ensure connectivity for the other pod is ok
		// The previous pod namespace is deleted and again, ensure connectivity for the other pod is ok
		ginkgo.It("[OVN network] multiple namespaces sharing a role primary network", func() {
			if !isNetworkSegmentationEnabled() || isClusterDefaultNetwork(netConfigParams) {
				ginkgo.Skip("network segmentation disabled or unsupported for cluster default network")
			}
			ginkgo.By(fmt.Sprintf("Building another namespace api object, basename %s", f.BaseName))
			otherNetworkNamespace, err := f.CreateNamespace(context.Background(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			ginkgo.By(fmt.Sprintf("namespace is connected to UDN, create a namespace attached to this primary as a %s UDN", netConfigParams.topology))
			nadClient, err := nadclient.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			netConfig := newNetworkAttachmentConfig(netConfigParams)
			netConfig.namespace = otherNetworkNamespace.Name
			_, err = nadClient.NetworkAttachmentDefinitions(otherNetworkNamespace.Name).Create(
				context.Background(),
				generateNAD(netConfig, f.ClientSet),
				metav1.CreateOptions{},
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			egressNodeAvailabilityHandler := egressNodeAvailabilityHandlerViaLabel{f}
			ginkgo.By("1. Set one node as available for egress")
			egressNodeAvailabilityHandler.Enable(egress1Node.name)
			defer egressNodeAvailabilityHandler.Restore(egress1Node.name)

			selectedByEIPLabels := map[string]string{
				"wants": "egress",
			}
			pod1Namespace := f.Namespace
			updateNamespaceLabels(f, pod1Namespace, selectedByEIPLabels)
			pod2OtherNetworkNamespace := otherNetworkNamespace.Name
			updateNamespaceLabels(f, otherNetworkNamespace, selectedByEIPLabels)

			ginkgo.By("3. Create an EgressIP object with one egress IP defined")
			var egressIP1 net.IP
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP1, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP1, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")

			var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - ` + egressIP1.String() + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            wants: egress
`
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("4. Check that the status is of length one and that it is assigned to egress1Node")
			statuses := verifyEgressIPStatusLengthEquals(1, nil)
			if statuses[0].Node != egress1Node.name {
				framework.Failf("Step 4. Check that the status is of length one and that it is assigned to egress1Node, failed")
			}

			ginkgo.By("5. Create two pods matching the EgressIP with each connected to the same network")
			pod1, err := createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			framework.ExpectNoError(err, "5. Create one pod matching the EgressIP: running on egress1Node, failed: %v", err)
			pod2, err := createGenericPodWithLabel(f, pod2Name, pod2Node.name, otherNetworkNamespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			framework.ExpectNoError(err, "5. Create one pod matching the EgressIP: running on egress2Node, failed: %v", err)

			gomega.Expect(pod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, pod1)).Should(gomega.Succeed())
			gomega.Expect(pod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, pod2)).Should(gomega.Succeed())

			framework.ExpectNoError(err, "Step 5. Create one pod matching the EgressIP: running on egress1Node, failed, err: %v", err)
			framework.Logf("Created pod %s on node %s", pod1Name, pod1Node.name)
			framework.ExpectNoError(err, "Step 5. Create one pod matching the EgressIP: running on egress2Node, failed, err: %v", err)
			framework.Logf("Created pod %s on node %s", pod2Name, pod2Node.name)

			ginkgo.By("6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, pod1Namespace.Name, pod1Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "Step 6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP, failed: %v", err)

			ginkgo.By("7. Check connectivity from pod connected to the same network and verify that the srcIP is the expected egressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, pod2OtherNetworkNamespace, pod2Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "Step 7. Check connectivity from pod connected to the same network and verify that the srcIP is the expected nodeIP, failed: %v", err)

			ginkgo.By("8. Delete pod in one namespace")
			framework.ExpectNoError(pod.DeletePodWithWait(context.TODO(), f.ClientSet, pod1), "pod %s/%s deletion failed", pod1.Namespace, pod1.Name)

			ginkgo.By("9. Check connectivity from other pod and verify that the srcIP is the expected egressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, pod2OtherNetworkNamespace, pod2Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "Step 9. Check connectivity from other pod and verify that the srcIP is the expected egressIP, failed: %v", err)

			ginkgo.By("10. Delete namespace with zero pods")
			gomega.Expect(f.ClientSet.CoreV1().Namespaces().Delete(context.TODO(), pod1.Namespace, metav1.DeleteOptions{})).To(gomega.Succeed())

			ginkgo.By("11. Check connectivity from other pod and verify that the srcIP is the expected egressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, pod2OtherNetworkNamespace, pod2Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "Step 11. Check connectivity from other pod and verify that the srcIP is the expected egressIP and verify that the srcIP is the expected nodeIP, failed: %v", err)
		})
		/*
			This test does the following:
			0. Add the "k8s.ovn.org/egress-assignable" label to one node
			1. Create an EgressIP object1 with one egress IP1 defined
			2. Create an EgressIP object2 with one egress IP2 defined
			3. Check that status of both EgressIP objects is of length one
			4. Create one pod matching the EgressIP object1
			5. Update namespace labels match EgressIP object1,
			6. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object1
			7. Verify source IP is NOT the node IP
			8. Update namespace labels match EgressIP object2
			9. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object2
			10. Verify source IP is NOT the node IP
			11. Update both EgressIP objects that namespace having same labels, and different pod selectors labels
			12. Check that status of both EgressIP objects is of length one
			13. Check connectivity from that one to an external \"node\" and verify that the IP is the node IP.
			14. Update pod labels match EgressIP object1
			15. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object1
			16. Verify source IP is NOT the node IP
			17. Update pod labels match EgressIP object2
			15. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object2
			16. Verify source IP is NOT the node IP
			17. Update EgressIP object1 to match the current pod label, EgressIP object2 not match pod label
			18. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object1
			19. Verify source IP is NOT the node IP
			20. Update EgressIP object2 to match the current pod label, EgressIP object1 not match pod label
			21. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object2
			22. Verify source IP is NOT the node IP
		*/
		ginkgo.It("Should handle EIP reassignment correctly on namespace and pod label updates, and EIP object updates", func() {
			if isUserDefinedNetwork(netConfigParams) {
				ginkgo.Skip("Unsupported for UDNs")
			}

			ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to one node")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

			ginkgo.By("1. Create an EgressIP object with one egress IP1 defined")
			var egressIP1 net.IP
			var err error
			var retryTimeout2 = 2 * retryInterval
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP1, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP1, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new EgressIP")
			podNamespace := f.Namespace
			egressLabels := map[string]string{
				"wants": "egress",
			}
			egressIPConfig := createEIPManifest(egressIPName, egressLabels, egressLabels, egressIP1.String())
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("2. Create second EgressIP object with one egress IP2 defined")
			var egressIP2 net.IP
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP2, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP2, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new EgressIP")
			egressLabels2 := map[string]string{
				"wants": "egress2",
			}
			egressIPConfig2 := createEIPManifest(egressIPName2, egressLabels, egressLabels2, egressIP2.String())
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig2), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("3. Check that status of both EgressIP objects is of length one")
			verifySpecificEgressIPStatusLengthEquals(egressIPName, 1, nil)
			verifySpecificEgressIPStatusLengthEquals(egressIPName2, 1, nil)

			ginkgo.By("4. Create one pod matching the EgressIP")
			_, err = createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), egressLabels)
			framework.ExpectNoError(err, "failed to create pod %s/%s", f.Namespace.Name, pod1Name)
			framework.Logf("Created pod %s on node %s", pod1Name, pod1Node.name)

			// Run namespace label updates multiple times to ensure EIP reassignment works well
			for i := 1; i <= 5; i++ {
				ginkgo.By(fmt.Sprintf("5.%d. Update namespace labels match egressIP %s selectors (iteration %d)", i, egressIPName, i))
				podNamespace = getNamespace(f, podNamespace.Name)
				updateNamespaceLabels(f, podNamespace, egressLabels)
				ginkgo.By(fmt.Sprintf("6.%d. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from %s (iteration %d)", i, egressIPName, i))
				err := wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout,
					true, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{egressIP1.String()}).WithContext())
				framework.ExpectNoError(err, "6.%d. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from %s (iteration %d): %v", i, egressIPName, i, err)
				ginkgo.By(fmt.Sprintf("7.%d. Verify source IP is NOT the node IP (iteration %d)", i, i))
				err = wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout2,
					true, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{pod1Node.nodeIP}).WithContext())
				gomega.Expect(err).To(gomega.HaveOccurred(), "Node IP should NOT be used as source IP - connection should succeed but node IP should not be found")

				ginkgo.By(fmt.Sprintf("8.%d. Update namespace labels to match egressIP %s selectors (iteration %d)", i, egressIPName2, i))
				podNamespace = getNamespace(f, podNamespace.Name)
				updateNamespaceLabels(f, podNamespace, egressLabels2)
				ginkgo.By(fmt.Sprintf("9.%d. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from %s (iteration %d)", i, egressIPName2, i))
				err = wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout,
					true, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{egressIP2.String()}).WithContext())
				framework.ExpectNoError(err, "9.%d. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from %s (iteration %d): %v", i, egressIPName2, i, err)
				ginkgo.By(fmt.Sprintf("10.%d. Verify source IP is NOT the node IP (iteration %d)", i, i))
				err = wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout2,
					true, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{pod1Node.nodeIP}).WithContext())
				gomega.Expect(err).To(gomega.HaveOccurred(), "Node IP should NOT be used as source IP - connection should succeed but node IP should not be found")
			}

			ginkgo.By("11. Update both egressIP objects such that they have same namespace selector but different pod selector")
			egressLabelsJSON, err := json.Marshal(egressLabels)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			egressLabels2JSON, err := json.Marshal(egressLabels2)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			specString := fmt.Sprintf("{\"spec\":{\"podSelector\":{\"matchLabels\":%s},\"namespaceSelector\": {\"matchLabels\":%s}}}",
				string(egressLabelsJSON), string(egressLabelsJSON))
			e2ekubectl.RunKubectlOrDie("default", "patch", "EgressIP/"+egressIPName, "-p", specString, "--type=merge")
			specString = fmt.Sprintf("{\"spec\":{\"podSelector\":{\"matchLabels\":%s},\"namespaceSelector\": {\"matchLabels\":%s}}}",
				string(egressLabels2JSON), string(egressLabelsJSON))
			e2ekubectl.RunKubectlOrDie("default", "patch", "EgressIP/"+egressIPName2, "-p", specString, "--type=merge")

			ginkgo.By("12. Check that status of both EgressIP objects is of length one")
			verifySpecificEgressIPStatusLengthEquals(egressIPName, 1, nil)
			verifySpecificEgressIPStatusLengthEquals(egressIPName2, 1, nil)
			ginkgo.By("13. Check connectivity from that one to an external \"node\" and verify that the IP is the node IP.")
			err = wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout,
				true, targetExternalContainerAndTest(primaryTargetExternalContainer,
					podNamespace.Name, pod1Name, true, []string{pod1Node.nodeIP}).WithContext())
			framework.ExpectNoError(err, "Step 13. Check connectivity from that one to an external \"node\" and verify that the IP is the node IP, failed, err: %v", err)
			ginkgo.By("Update namespace label to match the change in step 11")
			podNamespace = getNamespace(f, podNamespace.Name)
			updateNamespaceLabels(f, podNamespace, egressLabels)
			ginkgo.By(fmt.Sprintf("14. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from %s ", egressIPName))
			err = wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout,
				true, targetExternalContainerAndTest(primaryTargetExternalContainer,
					podNamespace.Name, pod1Name, true, []string{egressIP1.String()}).WithContext())
			framework.ExpectNoError(err, "14. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from %s ", egressIPName)

			// Run pod label updates multiple times to ensure EIP reassignment works well
			for i := 1; i <= 5; i++ {
				ginkgo.By(fmt.Sprintf("15.%d. Update pod labels match egressIP %s selectors (iteration %d)", i, egressIPName, i))
				pod1 := getPod(f, pod1Name)
				pod1.Labels = egressLabels
				updatePod(f, pod1)
				ginkgo.By(fmt.Sprintf("16.%d. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from %s (iteration %d)", i, egressIPName, i))
				err := wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout,
					true, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{egressIP1.String()}).WithContext())
				framework.ExpectNoError(err, "16.%d. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from %s (iteration %d): %v", i, egressIPName, i, err)
				ginkgo.By(fmt.Sprintf("17.%d. Verify source IP is NOT the node IP (iteration %d)", i, i))
				err = wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout2,
					true, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{pod1Node.nodeIP}).WithContext())
				gomega.Expect(err).To(gomega.HaveOccurred(), "Node IP should NOT be used as source IP - connection should succeed but node IP should not be found")

				ginkgo.By(fmt.Sprintf("18.%d. Update pod labels to match egressIP object2 %s selectors (iteration %d)", i, egressIPName2, i))
				pod1 = getPod(f, pod1Name)
				pod1.Labels = egressLabels2
				updatePod(f, pod1)
				ginkgo.By(fmt.Sprintf("19.%d. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object2 %s (iteration %d)", i, egressIPName2, i))
				err = wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout,
					true, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{egressIP2.String()}).WithContext())
				framework.ExpectNoError(err, "19.%d. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object2 %s (iteration %d): %v", i, egressIPName2, i, err)
				ginkgo.By(fmt.Sprintf("20.%d. Verify source IP is NOT the node IP (iteration %d)", i, i))
				err = wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout2,
					true, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{pod1Node.nodeIP}).WithContext())
				gomega.Expect(err).To(gomega.HaveOccurred(), "Node IP should NOT be used as source IP - connection should succeed but node IP should not be found")
			}

			// Run EIP object updates multiple times to ensure EIP reassignment works well
			for i := 1; i <= 5; i++ {
				ginkgo.By(fmt.Sprintf("21.%d. Update EgressIP %s selectors to match pod labels and EgressIP %s not matching pod labels,(iteration %d)", i, egressIPName, egressIPName2, i))
				specString = fmt.Sprintf("{\"spec\":{\"podSelector\":{\"matchLabels\":%s},\"namespaceSelector\": {\"matchLabels\":%s}}}",
					string(egressLabels2JSON), string(egressLabelsJSON))
				e2ekubectl.RunKubectlOrDie("default", "patch", "EgressIP/"+egressIPName, "-p", specString, "--type=merge")
				specString = fmt.Sprintf("{\"spec\":{\"podSelector\":{\"matchLabels\":%s},\"namespaceSelector\": {\"matchLabels\":%s}}}",
					string(egressLabelsJSON), string(egressLabelsJSON))
				e2ekubectl.RunKubectlOrDie("default", "patch", "EgressIP/"+egressIPName2, "-p", specString, "--type=merge")
				ginkgo.By(fmt.Sprintf("22.%d. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object1 %s (iteration %d)", i, egressIPName, i))
				err = wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout,
					true, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{egressIP1.String()}).WithContext())
				framework.ExpectNoError(err, "22.%d. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from %s (iteration %d): %v", i, egressIPName, i, err)
				ginkgo.By(fmt.Sprintf("23.%d. Verify source IP is NOT the node IP (iteration %d)", i, i))
				err = wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout2,
					true, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{pod1Node.nodeIP}).WithContext())
				gomega.Expect(err).To(gomega.HaveOccurred(), "Node IP should NOT be used as source IP - connection should succeed but node IP should not be found")

				ginkgo.By(fmt.Sprintf("24.%d. Update EgressIP %s selectors to match pod labels and EgressIP %s not matching pod labels,(iteration %d)", i, egressIPName2, egressIPName, i))
				specString = fmt.Sprintf("{\"spec\":{\"podSelector\":{\"matchLabels\":%s},\"namespaceSelector\": {\"matchLabels\":%s}}}",
					string(egressLabelsJSON), string(egressLabelsJSON))
				e2ekubectl.RunKubectlOrDie("default", "patch", "EgressIP/"+egressIPName, "-p", specString, "--type=merge")
				specString = fmt.Sprintf("{\"spec\":{\"podSelector\":{\"matchLabels\":%s},\"namespaceSelector\": {\"matchLabels\":%s}}}",
					string(egressLabels2JSON), string(egressLabelsJSON))
				e2ekubectl.RunKubectlOrDie("default", "patch", "EgressIP/"+egressIPName2, "-p", specString, "--type=merge")
				ginkgo.By(fmt.Sprintf("25.%d. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from %s (iteration %d)", i, egressIPName2, i))
				err = wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout,
					true, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{egressIP2.String()}).WithContext())
				framework.ExpectNoError(err, "25.%d. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from %s (iteration %d): %v", i, egressIPName2, i, err)
				ginkgo.By(fmt.Sprintf("26.%d. Verify source IP is NOT the node IP (iteration %d)", i, i))
				err = wait.PollUntilContextTimeout(context.TODO(), retryInterval, retryTimeout2,
					true, targetExternalContainerAndTest(primaryTargetExternalContainer,
						podNamespace.Name, pod1Name, true, []string{pod1Node.nodeIP}).WithContext())
				gomega.Expect(err).To(gomega.HaveOccurred(), "Node IP should NOT be used as source IP - connection should succeed but node IP should not be found")
			}
		})

		ginkgo.It("Should fail if egressip-mark annotation is present during EgressIP creation", func() {
			ginkgo.By("1. Create an EgressIP object with one egress IP defined")
			var egressIP1 net.IP
			var err error
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP1, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP1, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")

			var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
    annotations:
      ` + util.EgressIPMarkAnnotation + `: "50000"
spec:
    egressIPs:
    - ` + egressIP1.String() + `
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			ginkgo.By("2. Create an EgressIP with k8s.ovn.org/egressip-mark annotation defined")
			_, err = e2ekubectl.RunKubectl("default", "create", "-f", egressIPYaml)
			gomega.Expect(err).To(gomega.HaveOccurred(), "Should fail if k8s.ovn.org/egressip-mark annotation is present during creation")
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("EgressIP resources cannot be created with the \"k8s.ovn.org/egressip-mark\" annotation. This annotation is managed by the system.")))
		})

		ginkgo.It("Should fail if egressip-mark annotation is being added by a regular user", func() {
			ginkgo.By("1. Add the \"k8s.ovn.org/egress-assignable\" label to egress1Node node")
			egressNodeAvailabilityHandler := egressNodeAvailabilityHandlerViaLabel{f}
			egressNodeAvailabilityHandler.Enable(egress1Node.name)
			defer egressNodeAvailabilityHandler.Restore(egress1Node.name)

			podNamespace := f.Namespace
			labels := map[string]string{
				"name": f.Namespace.Name,
			}
			updateNamespaceLabels(f, podNamespace, labels)

			ginkgo.By("2. Create an EgressIP object with one egress IP defined")
			var egressIP1 net.IP
			var err error
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP1, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP1, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")

			var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - ` + egressIP1.String() + `
    namespaceSelector:
        matchLabels:
            name: ` + f.Namespace.Name + `
`
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("3. Check that the status is of length one and that it is assigned to egress1Node")
			statuses := verifyEgressIPStatusLengthEquals(1, nil)
			if statuses[0].Node != egress1Node.name {
				framework.Failf("Step 3. Check that the status is of length one and that it is assigned to egress1Node, failed")
			}

			ginkgo.By("4. Try updating k8s.ovn.org/egressip-mark annotation")
			// Get the current annotation value to ensure we try to overwrite with a different value
			annotationsJSON, err := e2ekubectl.RunKubectl("", "get", "egressip", egressIPName, "-o", "jsonpath={.metadata.annotations}")
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Failed to get annotations")
			var annotations map[string]string
			err = json.Unmarshal([]byte(annotationsJSON), &annotations)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Failed to unmarshal annotations JSON")
			currentValue := annotations[util.EgressIPMarkAnnotation]

			newValue := 50000
			if currentValue == "50000" {
				newValue = 50001
			}

			_, err = e2ekubectl.RunKubectl("", "annotate", "--overwrite", "egressip", egressIPName, fmt.Sprintf("%s=%d", util.EgressIPMarkAnnotation, newValue))
			gomega.Expect(err).To(gomega.HaveOccurred(), "Should fail if k8s.ovn.org/egressip-mark is being updated")
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("The \"k8s.ovn.org/egressip-mark\" annotation cannot be modified or removed once set. This annotation is managed by the system.")))

			ginkgo.By("5. Try removing k8s.ovn.org/egressip-mark annotation")
			_, err = e2ekubectl.RunKubectl("", "annotate", "--overwrite", "egressip", egressIPName, fmt.Sprintf("%s-", util.EgressIPMarkAnnotation))
			gomega.Expect(err).To(gomega.HaveOccurred(), "Should fail if k8s.ovn.org/egressip-mark is being removed")
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("The \"k8s.ovn.org/egressip-mark\" annotation cannot be modified or removed once set. This annotation is managed by the system.")))
		})

		ginkgo.It("Should skip orphaned nodes and assign EgressIPs to valid nodes", func() {
			if isUserDefinedNetwork(netConfigParams) {
				ginkgo.Skip("Unsupported for UDNs")
			}

			// Label two nodes for egress assignment
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")

			podNamespace := f.Namespace
			ginkgo.By("Allocating EgressIP addresses before orphaning node")
			var egressIP1, egressIP2 net.IP
			var err error
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP1, err = allocators.NewPrimaryIPv6(egress1Node.name)
				framework.ExpectNoError(err, "Failed to allocate IPv6 for EgressIP1")
				egressIP2, err = allocators.NewPrimaryIPv6(egress2Node.name)
				framework.ExpectNoError(err, "Failed to allocate IPv6 for EgressIP2")
			} else {
				egressIP1, err = allocators.NewPrimaryIPv4(egress1Node.name)
				framework.ExpectNoError(err, "Failed to allocate IPv4 for EgressIP1")
				egressIP2, err = allocators.NewPrimaryIPv4(egress2Node.name)
				framework.ExpectNoError(err, "Failed to allocate IPv4 for EgressIP2")
			}

			// A bare Node is stable: unlike a real kind node, no ovnkube-node
			// instance can recreate its host-cidrs annotation. The conflict
			// check must skip this stale/orphaned node while assigning to the
			// two real, labelled nodes above.
			egressNode, err := f.ClientSet.CoreV1().Nodes().Get(
				context.TODO(), egress1Node.name, metav1.GetOptions{})
			framework.ExpectNoError(err, "Failed to get egress node %s", egress1Node.name)
			// Copy the egress configuration, but let cluster-manager assign its
			// own node subnet: admission permits only that controller to write it.
			// Deliberately omit host-cidrs so the node is rejected.
			orphanNodeAnnotations := map[string]string{
				util.OvnNodeIfAddr: egressNode.Annotations[util.OvnNodeIfAddr],
			}
			const cloudEgressIPConfigAnnotation = "cloud.network.openshift.io/egress-ipconfig"
			if cloudEgressIPConfig, ok := egressNode.Annotations[cloudEgressIPConfigAnnotation]; ok {
				orphanNodeAnnotations[cloudEgressIPConfigAnnotation] = cloudEgressIPConfig
			}
			orphanNodeName := fmt.Sprintf("eip-orphan-%d", time.Now().UnixNano())
			ginkgo.By("Creating an orphan node without host-cidrs")
			_, err = f.ClientSet.CoreV1().Nodes().Create(context.TODO(), &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: orphanNodeName,
					Labels: map[string]string{
						"k8s.ovn.org/egress-assignable": "dummy",
					},
					Annotations: orphanNodeAnnotations,
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:   corev1.NodeReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err, "Failed to create orphan node %s", orphanNodeName)
			providerCtx.AddCleanUpFn(func() error {
				_, err := e2ekubectl.RunKubectl(
					"default", "delete", "node", orphanNodeName,
					"--ignore-not-found=true", "--wait=false",
				)
				return err
			})

			ginkgo.By("Waiting for cluster-manager to observe the orphan node")
			err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				node, err := f.ClientSet.CoreV1().Nodes().Get(
					context.TODO(), orphanNodeName, metav1.GetOptions{})
				if err != nil {
					framework.Logf(
						"Failed to read orphan node %s while waiting for cluster-manager to observe it: %v",
						orphanNodeName, err,
					)
					return false, nil
				}
				subnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
				return node.Annotations[util.OvnNodeID] != "" && err == nil && len(subnets) > 0, nil
			})
			framework.ExpectNoError(err,
				"Cluster-manager did not observe orphan node %s", orphanNodeName)

			ginkgo.By("Waiting for the EgressIP controller to process the orphan node")
			controlPlaneNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
			controllerLogStart := time.Now().Add(-retryTimeout - time.Minute)
			expectedControllerLog := fmt.Sprintf(
				"Node %s is not egress-assignable: failed to parse host-cidrs", orphanNodeName)
			err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				controlPlanePods, err := f.ClientSet.CoreV1().Pods(controlPlaneNamespace).List(
					context.TODO(), metav1.ListOptions{LabelSelector: "name=ovnkube-control-plane"})
				if err != nil {
					framework.Logf("Failed to list ovnkube-control-plane pods while waiting for the EgressIP controller: %v", err)
					return false, nil
				}
				for _, controlPlanePod := range controlPlanePods.Items {
					logs, err := pod.GetPodLogsSince(context.TODO(), f.ClientSet, controlPlaneNamespace,
						controlPlanePod.Name, "ovnkube-cluster-manager", controllerLogStart)
					if err != nil {
						framework.Logf("Failed to read EgressIP controller logs from pod %s: %v", controlPlanePod.Name, err)
						continue
					}
					if strings.Contains(logs, expectedControllerLog) {
						return true, nil
					}
				}
				return false, nil
			})
			framework.ExpectNoError(err,
				"EgressIP controller did not process orphan node %s", orphanNodeName)

			egressIPConfig1 := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: eip-test-1
spec:
    egressIPs:
    - %s
    podSelector:
        matchLabels:
            egress-pod: "true"
    namespaceSelector:
        matchLabels:
            kubernetes.io/metadata.name: %s
`, egressIP1.String(), podNamespace.Name)

			egressIPConfig2 := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: eip-test-2
spec:
    egressIPs:
    - %s
    podSelector:
        matchLabels:
            egress-pod: "true"
    namespaceSelector:
        matchLabels:
            kubernetes.io/metadata.name: %s
`, egressIP2.String(), podNamespace.Name)

			tmpDir, err := os.MkdirTemp("", "egressip-test-")
			if err != nil {
				framework.Failf("Unable to create temp directory: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			egressIPYaml1 := filepath.Join(tmpDir, "eip-test-1.yaml")
			if err := os.WriteFile(egressIPYaml1, []byte(egressIPConfig1), 0644); err != nil {
				framework.Failf("Unable to write EgressIP YAML to disk: %v", err)
			}

			egressIPYaml2 := filepath.Join(tmpDir, "eip-test-2.yaml")
			if err := os.WriteFile(egressIPYaml2, []byte(egressIPConfig2), 0644); err != nil {
				framework.Failf("Unable to write EgressIP YAML to disk: %v", err)
			}

			framework.Logf("Create the EgressIP configurations")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml1)
			providerCtx.AddCleanUpFn(func() error {
				_, err := e2ekubectl.RunKubectl("default", "delete", "egressip", "eip-test-1", "--ignore-not-found=true")
				return err
			})

			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml2)
			providerCtx.AddCleanUpFn(func() error {
				_, err := e2ekubectl.RunKubectl("default", "delete", "egressip", "eip-test-2", "--ignore-not-found=true")
				return err
			})

			// Returns the node each of the two test EgressIPs is assigned to.
			// An EgressIP that has no assignment yet is absent from the map.
			assignedNodes := func() map[string]string {
				egressIPStdout, err := e2ekubectl.RunKubectl("default", "get", "eip", "-o", "json")
				if err != nil {
					framework.Logf("Error getting EgressIPs: %v", err)
					return nil
				}
				var egressIPList egressIPs
				if err := json.Unmarshal([]byte(egressIPStdout), &egressIPList); err != nil {
					framework.Logf("Error unmarshaling EgressIP list: %v", err)
					return nil
				}
				assigned := map[string]string{}
				for _, eip := range egressIPList.Items {
					if eip.Metadata.Name != "eip-test-1" && eip.Metadata.Name != "eip-test-2" {
						continue
					}
					if len(eip.Status.Items) > 0 {
						assigned[eip.Metadata.Name] = eip.Status.Items[0].Node
					}
				}
				return assigned
			}

			ginkgo.By("Verifying both EgressIPs are assigned while one node is orphaned")
			var assigned map[string]string
			err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				assigned = assignedNodes()
				framework.Logf("Current EgressIP assignments: %v", assigned)

				for egressIPName, nodeName := range assigned {
					if nodeName == orphanNodeName {
						return false, fmt.Errorf(
							"EgressIP %s was assigned to orphan node %s",
							egressIPName, orphanNodeName,
						)
					}
				}
				return len(assigned) == 2, nil
			})
			framework.ExpectNoError(err,
				"Both EgressIPs should have been assigned despite orphan node %s", orphanNodeName)
		})

		ginkgo.It("should prevent duplicate MAC responses when egress node is rebooted", func() {
			if !isNetworkSegmentationEnabled() {
				ginkgo.Skip("network segmentation is disabled")
			}

			ginkgo.By("1. Label node 1 as egress-assignable")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "")
			defer e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable")

			ginkgo.By("2. Creating EgressIP object")
			podNamespace := f.Namespace
			labels := map[string]string{
				"name": f.Namespace.Name,
			}
			updateNamespaceLabels(f, podNamespace, labels)
			var egressIP net.IP
			var err error
			if isIPv6TestRun {
				egressIP, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate egress IP")
			egressIPConfig := createEIPManifest(egressIPName, podEgressLabel, labels, egressIP.String())
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write EgressIP config: %v", err)
			}
			defer os.Remove(egressIPYaml)
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("3. Verifying EgressIP assigned to node 1 and creating a pod on this node")
			statuses := verifyEgressIPStatusLengthEquals(1, func(statuses []egressIPStatus) bool {
				return statuses[0].Node == egress1Node.name
			})
			framework.Logf("Egress IP %s assigned to node: %s", egressIP.String(), statuses[0].Node)
			_, err = createGenericPodWithLabel(f, pod1Name, egress1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			framework.ExpectNoError(err, "failed to create pod matching EgressIP selector")

			ginkgo.By("4. Labeling node 2 as egress-assignable for failover")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "")
			defer e2enode.RemoveLabelOffNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable")

			ginkgo.By("5. Getting network interfaces of both EgressIP nodes and external container")
			primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
			framework.ExpectNoError(err, "failed to get primary network")
			egress1NodeInf, err := infraprovider.Get().GetK8NodeNetworkInterface(egress1Node.name, primaryProviderNetwork)
			framework.ExpectNoError(err, "failed to get node 1 interface")
			egress2NodeInf, err := infraprovider.Get().GetK8NodeNetworkInterface(egress2Node.name, primaryProviderNetwork)
			framework.ExpectNoError(err, "failed to get node 2 interface")
			extContainerInf, err := infraprovider.Get().GetExternalContainerNetworkInterface(primaryTargetExternalContainer, primaryProviderNetwork)
			framework.ExpectNoError(err, "failed to get external container interface")

			ginkgo.By("6. Installing arping/ndisc6 in external container")
			var installCmd string
			if isIPv6TestRun {
				installCmd = "which ndisc6 >/dev/null 2>&1 || (apk update && apk add ndisc6)"
			} else {
				installCmd = "which arping >/dev/null 2>&1 || (apk update && apk add iputils)"
			}
			_, err = infraprovider.Get().ExecExternalContainerCommand(primaryTargetExternalContainer, []string{
				"sh", "-c", installCmd,
			})
			framework.ExpectNoError(err, "failed to install network discovery tool")

			ginkgo.By("7. Verifying egress IP resolves to node 1 MAC before migration")
			var discoveryCmd string
			var macRegex *regexp.Regexp
			if isIPv6TestRun {
				discoveryCmd = fmt.Sprintf("ndisc6 -1 -w 1000 %s %s 2>&1", egressIP.String(), extContainerInf.InfName)
				// ndisc6 output format: "Target link-layer address: aa:bb:cc:dd:ee:ff"
				macRegex = regexp.MustCompile(`Target link-layer address:\s+([0-9a-fA-F]{1,2}:[0-9a-fA-F]{1,2}:[0-9a-fA-F]{1,2}:[0-9a-fA-F]{1,2}:[0-9a-fA-F]{1,2}:[0-9a-fA-F]{1,2})`)
			} else {
				discoveryCmd = fmt.Sprintf("arping -c 1 -I %s %s 2>&1", extContainerInf.InfName, egressIP.String())
				macRegex = regexp.MustCompile(`\[([0-9a-fA-F:]+)\]`)
			}
			output, err := infraprovider.Get().ExecExternalContainerCommand(primaryTargetExternalContainer, []string{"sh", "-c", discoveryCmd})
			framework.ExpectNoError(err, "network discovery should succeed before migration")
			matches := macRegex.FindStringSubmatch(output)
			gomega.Expect(matches).To(gomega.HaveLen(2), "should extract MAC from discovery output")
			macBeforeMigration := strings.ToLower(strings.TrimSpace(matches[1]))
			expectedMAC1 := strings.ToLower(egress1NodeInf.MAC)
			framework.Logf("MAC before migration: %s, expected: %s", macBeforeMigration, expectedMAC1)
			gomega.Expect(macBeforeMigration).To(gomega.Equal(expectedMAC1), "Egress IP should resolve to node 1 MAC before migration")

			ginkgo.By("8. Getting ovnkube-node pod name on egress node 1")
			ovnkubeNodePods, err := f.ClientSet.CoreV1().Pods("ovn-kubernetes").List(context.TODO(), metav1.ListOptions{
				FieldSelector: fmt.Sprintf("spec.nodeName=%s", egress1Node.name),
				LabelSelector: "app=ovnkube-node",
			})
			framework.ExpectNoError(err, "failed to list ovnkube-node pods")
			gomega.Expect(ovnkubeNodePods.Items).To(gomega.HaveLen(1), "should have exactly one ovnkube-node pod on egress node 1")
			ovnkubeNodePod := ovnkubeNodePods.Items[0].Name
			framework.Logf("Found ovnkube-node pod: %s on node %s", ovnkubeNodePod, egress1Node.name)

			ginkgo.By("9. Start a goroutine to check for nftables chain during pod deletion")
			nftChainFound := make(chan bool, 1)
			stopChecking := make(chan bool, 1)
			goroutineReady := make(chan bool, 1)
			nftChainCheckCmd := "nft -j list chains | jq '.nftables[] | select(.chain.table==\"ovn-kubernetes-egressip\" and .chain.family==\"netdev\" and .chain.name==\"egressip-drop\").chain'"
			go func() {
				defer close(nftChainFound)
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				goroutineReady <- true
				for {
					select {
					case <-stopChecking:
						return
					case <-ticker.C:
						output, err := infraprovider.Get().ExecK8NodeCommand(egress1Node.name, []string{"sh", "-c", nftChainCheckCmd})
						if err == nil && strings.Contains(output, "egressip-drop") {
							nftChainFound <- true
							return
						}
					}
				}
			}()
			<-goroutineReady
			framework.Logf("Nftables chain monitoring goroutine started")

			ginkgo.By("10. Deleting ovnkube-node pod and intentionally dropping EgressIP health check packets to trigger egress IP migration")
			framework.Logf("Deleting ovnkube-node pod %s to trigger egress IP migration", ovnkubeNodePod)
			err = deletePodWithWaitByName(context.TODO(), f.ClientSet, ovnkubeNodePod, "ovn-kubernetes")
			framework.ExpectNoError(err, "failed to delete ovnkube-node pod and wait for termination")
			framework.Logf("✓ ovnkube-node pod %s deleted and fully terminated", ovnkubeNodePod)
			framework.Logf("Dropping EgressIP health check packets on node %s to trigger EgressIP migration", egress1Node.name)
			setNodeReachable(egress1Node.name, false)
			defer setNodeReachable(egress1Node.name, true)

			ginkgo.By("11. Verifying nftables chain exists on node 1 during shutdown")
			close(stopChecking)
			chainFound, ok := <-nftChainFound
			if !ok || !chainFound {
				framework.Failf("Nftables chain egressip-drop was not found on node %s during pod shutdown", egress1Node.name)
			}
			framework.Logf("✓ Nftables chain egressip-drop verified on node %s", egress1Node.name)

			ginkgo.By("12. Waiting for egress IP to migrate to node 2")
			verifyEgressIPStatusLengthEquals(1, func(statuses []egressIPStatus) bool {
				return statuses[0].Node == egress2Node.name
			})
			framework.Logf("✓ Egress IP successfully migrated to node %s", egress2Node.name)

			ginkgo.By("13. Checking for duplicate MAC responses after migration")
			// The old node should NOT respond to ARP/NDP requests even though the SNAT rule is present on the gateway router and egress IP might
			// still be on its br-ex interface temporarily. The nftables rules added during pod shutdown should prevent any ARP/NDP responses.
			err = checkForDuplicateMAC(
				primaryTargetExternalContainer,
				extContainerInf.InfName,
				egressIP.String(),
				egress1NodeInf.MAC,
				egress2NodeInf.MAC,
				isIPv6TestRun,
				20,                   // maxChecks
				500*time.Millisecond, // checkInterval
			)
			framework.ExpectNoError(err, "duplicate MAC detection check failed")

			ginkgo.By("14. Waiting for ovnkube-node pod to restart and OVN cluster to be healthy")
			err = waitOVNKubernetesHealthy(f)
			framework.ExpectNoError(err, "OVN-Kubernetes cluster should be healthy after ovnkube-node pod restart")

			ginkgo.By("15. Verifying nftables cleanup on node 1 after pod restart")
			ovnkubeNodePods, err = f.ClientSet.CoreV1().Pods("ovn-kubernetes").List(context.TODO(), metav1.ListOptions{
				FieldSelector: fmt.Sprintf("spec.nodeName=%s", egress1Node.name),
				LabelSelector: "app=ovnkube-node",
			})
			framework.ExpectNoError(err, "failed to list ovnkube-node pods")
			if len(ovnkubeNodePods.Items) > 0 {
				podName := ovnkubeNodePods.Items[0].Name
				nftCmd := "nft list table netdev ovn-kubernetes-egressip 2>&1"
				_, err := e2ekubectl.RunKubectl("ovn-kubernetes", "exec", podName, "-c", "ovnkube-controller", "--", "sh", "-c", nftCmd)
				// Command should fail because table should be deleted
				gomega.Expect(err).ToNot(gomega.BeNil(), "nft command should fail because table should be deleted")
				gomega.Expect(err.Error()).To(gomega.Or(
					gomega.ContainSubstring("No such file or directory"),
					gomega.ContainSubstring("No such file"),
				), "nftables egress IP table should be deleted after cleanup")
				framework.Logf("✓ Nftables table cleaned up on node 1")
			}

			framework.Logf("✓ Test passed: Egress IP migrated cleanly without duplicate MAC responses")
		})

		ginkgo.DescribeTable("[OVN network] multiple namespaces with different primary networks", func(otherNetworkAttachParms networkAttachmentConfigParams) {
			if !isNetworkSegmentationEnabled() {
				ginkgo.Skip("network segmentation is disabled")
			}
			var otherNetworkNamespace *corev1.Namespace
			var err error
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			// The EgressIP IP must match both networks IP family
			if !cidrsContainIPFamily(otherNetworkAttachParms.cidr, isIPv6TestRun) {
				ginkgo.Skip(fmt.Sprintf("Test run IP family (is IPv6: %v) isn't supported by other network", isIPv6TestRun))
			}
			// is the test namespace a CDN? If so create the UDN namespace
			if isClusterDefaultNetwork(netConfigParams) {
				ginkgo.By(fmt.Sprintf("Building other namespace api object for Primary UDN, basename %s", f.BaseName))
				otherNetworkNamespace, err = f.CreateNamespace(context.Background(), f.BaseName, map[string]string{
					RequiredUDNNamespaceLabel: "",
					"e2e-framework":           f.BaseName,
				})
				ginkgo.By(fmt.Sprintf("namespace is connected to CDN, create a namespace with %s primary UDN", otherNetworkAttachParms.topology))
				// create primary UDN
				nadClient, err := nadclient.NewForConfig(f.ClientConfig())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				netConfig := newNetworkAttachmentConfig(otherNetworkAttachParms)
				netConfig.namespace = otherNetworkNamespace.Name
				_, err = nadClient.NetworkAttachmentDefinitions(otherNetworkNamespace.Name).Create(
					context.Background(),
					generateNAD(netConfig, f.ClientSet),
					metav1.CreateOptions{},
				)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			} else {
				ginkgo.By(fmt.Sprintf("Building other namespace api object for CDN, basename %s", f.BaseName))
				otherNetworkNamespace, err = f.CreateNamespace(context.Background(), f.BaseName, map[string]string{
					"e2e-framework": f.BaseName,
				})
				// if network is L3 or L2 UDN, then other network is CDN
			}
			egressNodeAvailabilityHandler := egressNodeAvailabilityHandlerViaLabel{f}
			ginkgo.By("1. Set one node as available for egress")
			egressNodeAvailabilityHandler.Enable(egress1Node.name)
			defer egressNodeAvailabilityHandler.Restore(egress1Node.name)

			selectedByEIPLabels := map[string]string{
				"wants": "egress",
			}
			pod1Namespace := f.Namespace
			_, isUDNRequired := pod1Namespace.Labels[RequiredUDNNamespaceLabel]
			ginkgo.By(fmt.Sprintf("Updating namespace label for base namespace: %s, with required UDN label: %t",
				pod1Namespace.Name, isUDNRequired))
			updateNamespaceLabels(f, pod1Namespace, selectedByEIPLabels)
			pod2OtherNetworkNamespace := otherNetworkNamespace.Name
			_, isUDNRequired = otherNetworkNamespace.Labels[RequiredUDNNamespaceLabel]
			ginkgo.By(fmt.Sprintf("Updating namespace label for other namespace: %s, with required UDN label: %t",
				otherNetworkNamespace.Name, isUDNRequired))
			updateNamespaceLabels(f, otherNetworkNamespace, selectedByEIPLabels)

			ginkgo.By("3. Create an EgressIP object with one egress IP defined")
			var egressIP1 net.IP
			if utilnet.IsIPv6String(egress1Node.nodeIP) {
				egressIP1, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				egressIP1, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate new Node IP")

			var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + egressIPName + `
spec:
    egressIPs:
    - ` + egressIP1.String() + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            wants: egress
`
			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

			ginkgo.By("4. Check that the status is of length one and that it is assigned to egress1Node")
			statuses := verifyEgressIPStatusLengthEquals(1, nil)
			if statuses[0].Node != egress1Node.name {
				framework.Failf("Step 4. Check that the status is of length one and that it is assigned to egress1Node, failed")
			}

			ginkgo.By("5. Create two pods matching the EgressIP with each connected to a different network")
			_, err = createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			framework.ExpectNoError(err, "5. Create one pod matching the EgressIP: running on egress1Node, failed: %v", err)
			_, err = createGenericPodWithLabel(f, pod2Name, pod2Node.name, otherNetworkNamespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podEgressLabel)
			framework.ExpectNoError(err, "5. Create one pod matching the EgressIP: running on egress2Node, failed: %v", err)
			_, err = getPodIPWithRetry(f.ClientSet, isIPv6TestRun, f.Namespace.Name, pod1Name)
			framework.ExpectNoError(err, "Step 5. Create one pod matching the EgressIP: running on egress1Node, failed, err: %v", err)
			framework.Logf("Created pod %s on node %s", pod1Name, pod1Node.name)
			_, err = getPodIPWithRetry(f.ClientSet, isIPv6TestRun, otherNetworkNamespace.Name, pod2Name)
			framework.ExpectNoError(err, "Step 5. Create one pod matching the EgressIP: running on egress2Node, failed, err: %v", err)
			framework.Logf("Created pod %s on node %s", pod2Name, pod2Node.name)

			ginkgo.By("6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, pod1Namespace.Name, pod1Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "Step 6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP, failed: %v", err)

			ginkgo.By("7. Check connectivity from pod connected to a different network and verify that the srcIP is the expected egressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(primaryTargetExternalContainer, pod2OtherNetworkNamespace, pod2Name, true, []string{egressIP1.String()}))
			framework.ExpectNoError(err, "Step 7. Check connectivity from pod connected to a different network and verify that the srcIP is the expected nodeIP, failed: %v", err)
		},
			ginkgo.Entry("L3 Primary UDN", networkAttachmentConfigParams{
				name:     "l3primary",
				topology: types.Layer3Topology,
				cidr:     primaryLayer3MultiCIDRs(),
				role:     "primary",
			}),
			ginkgo.Entry("L2 Primary UDN", networkAttachmentConfigParams{
				name:     "l2primary",
				topology: types.Layer2Topology,
				cidr:     joinStrings("10.10.0.0/16", "2014:100:200::0/60"),
				role:     "primary",
			}),
		)

		// egressNodeSelector tests — added below
		// -----------------------------------------------------------------------
		// Helper: allocate one egress IP of the right family.
		newEgressIP := func() net.IP {
			var ip net.IP
			var err error
			if isIPv6TestRun {
				ip, err = allocators.NewPrimaryIPv6(egress1Node.name)
			} else {
				ip, err = allocators.NewPrimaryIPv4(egress1Node.name)
			}
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must allocate egress IP")
			return ip
		}

		// Helper: write manifest to a temp file, create the EgressIP, and return
		// a cleanup func that deletes the EgressIP and removes the temp file.
		applyEIPManifest := func(name, manifest string) func() {
			tmpFile, err := os.CreateTemp("", name+"*.yaml")
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must create temp file for EIP manifest")
			_, err = tmpFile.WriteString(manifest)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "must write EIP manifest")
			gomega.Expect(tmpFile.Close()).To(gomega.Succeed())
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", tmpFile.Name())
			return func() {
				e2ekubectl.RunKubectlOrDie("default", "delete", "egressip", name, "--ignore-not-found")
				_ = os.Remove(tmpFile.Name())
			}
		}

		// -----------------------------------------------------------------------
		// egressNodeSelector lifecycle
		// -----------------------------------------------------------------------
		ginkgo.It("[Primary EgressIP] egressNodeSelector lifecycle", func() {
			const (
				eipAName      = "egressip-ens-lifecycle-a"
				eipBName      = "egressip-ens-lifecycle-b"
				eipCName      = "egressip-ens-lifecycle-c"
				eipXName      = "egressip-ens-lifecycle-x"
				eipYName      = "egressip-ens-lifecycle-y"
				podAName      = "egressip-lifecycle-pod-a"
				podBName      = "egressip-lifecycle-pod-b"
				podCName      = "egressip-lifecycle-pod-c"
				podSharedName = "egressip-lifecycle-pod-shared"
				poolLabelKey  = "egresspool"
			)
			podLabelA := map[string]string{poolLabelKey: "a"}
			podLabelB := map[string]string{poolLabelKey: "b"}
			podLabelC := map[string]string{poolLabelKey: "c"}
			podLabelShared := map[string]string{poolLabelKey: "shared"}
			nsLabel := map[string]string{"name": f.Namespace.Name}
			updateNamespaceLabels(f, f.Namespace, nsLabel)

			// Node pool labels
			nodeLabelKey := "pool"
			nodeLabelA := "a"
			nodeLabelB := "b"
			tierLabelKey := "tier"
			tierLabelB := "b"

			// Allocate IPs
			eipA1 := newEgressIP()
			eipA2 := newEgressIP()
			eipB := newEgressIP()
			eipC1 := newEgressIP()
			eipC2 := newEgressIP()
			eipX := newEgressIP()
			eipY := newEgressIP()

			// Selectors
			selectorPoolA := &metav1.LabelSelector{MatchLabels: map[string]string{nodeLabelKey: nodeLabelA}}
			selectorPoolB := &metav1.LabelSelector{MatchLabels: map[string]string{nodeLabelKey: nodeLabelB}}
			selectorPoolAB := &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      nodeLabelKey,
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{nodeLabelA, nodeLabelB},
				}},
			}

			// Cleanup
			ginkgo.DeferCleanup(func() {
				for _, name := range []string{eipAName, eipBName, eipCName, eipXName, eipYName} {
					e2ekubectl.RunKubectlOrDie("default", "delete", "eip", name, "--ignore-not-found=true")
					os.Remove(filepath.Join(os.TempDir(), name+".yaml"))
				}
				for _, nodeName := range []string{egress1Node.name, egress2Node.name} {
					e2enode.RemoveLabelOffNode(f.ClientSet, nodeName, nodeLabelKey)
					e2enode.RemoveLabelOffNode(f.ClientSet, nodeName, tierLabelKey)
				}
				for _, podName := range []string{podAName, podBName, podCName, podSharedName} {
					e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "pod", podName, "--ignore-not-found=true")
				}
			})

			// --- Step 1: node-first ordering ---
			ginkgo.By("1. Node-first: label nodes, create EIPs, verify assignment")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, nodeLabelKey, nodeLabelA)
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, nodeLabelKey, nodeLabelB)
			cleanup := applyEIPManifest(eipAName, createEIPManifestWithNodeSelector(eipAName, podLabelA, nsLabel, selectorPoolA, eipA1.String(), eipA2.String()))
			defer cleanup()
			cleanup = applyEIPManifest(eipBName, createEIPManifestWithNodeSelector(eipBName, podLabelB, nsLabel, selectorPoolB, eipB.String()))
			defer cleanup()
			cleanup = applyEIPManifest(eipCName, createEIPManifestWithNodeSelector(eipCName, podLabelC, nsLabel, selectorPoolAB, eipC1.String()))
			defer cleanup()
			// EIP-A has 2 IPs but pool-A has only 1 node → 1 IP assigns, 1 stays pending.
			// EIP-B: 1 IP on egress2Node; EIP-C: 1 IP on either pool node.
			// Save status so step 2 can use the actual assigned IP (not the pending one).
			eipAStatus1 := verifySpecificEgressIPStatusLengthEquals(eipAName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			verifySpecificEgressIPStatusLengthEquals(eipBName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})
			// EIP-C matches pool A∨B, so its single IP must land on one of the two pool nodes.
			verifySpecificEgressIPStatusLengthEquals(eipCName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name || s[0].Node == egress2Node.name
			})

			// --- Step 2: create pods + baseline traffic ---
			ginkgo.By("2. Create pods and verify baseline traffic: each pod uses its EIP src IP")
			_, err := createGenericPodWithLabel(f, podAName, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podLabelA)
			framework.ExpectNoError(err, "create pod-A")
			_, err = createGenericPodWithLabel(f, podBName, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podLabelB)
			framework.ExpectNoError(err, "create pod-B")
			_, err = createGenericPodWithLabel(f, podCName, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podLabelC)
			framework.ExpectNoError(err, "create pod-C")
			// EIP-A: only pass the single assigned IP — the other is pending and will never appear.
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podAName, true, []string{eipAStatus1[0].EgressIP}).WithContext()),
				"pod-A must use EIP-A src IP")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podBName, true, []string{eipB.String()}).WithContext()),
				"pod-B must use EIP-B src IP")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podCName, true, []string{eipC1.String()}).WithContext()),
				"pod-C must use EIP-C src IP")

			// --- Step 3: EIP-first ordering ---
			ginkgo.By("3. EIP-first: remove EIP-A+label, recreate EIP-A pending, then label node; verify traffic")
			e2ekubectl.RunKubectlOrDie("default", "delete", "eip", eipAName, "--ignore-not-found=true")
			e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, nodeLabelKey)
			// EIP-A deleted → pod-A must fall back to its own node IP (no EIP routing involved)
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerOrBGPServerAndTest(f, primaryTargetExternalContainer, f.Namespace.Name, podAName, pod1Node.nodeIP).WithContext()),
				"pod-A must fall back to node IP after EIP-A deletion")
			// Re-create EIP-A with no matching node → must stay pending
			cleanup = applyEIPManifest(eipAName, createEIPManifestWithNodeSelector(eipAName, podLabelA, nsLabel, selectorPoolA, eipA1.String(), eipA2.String()))
			defer cleanup()
			gomega.Consistently(func() int {
				return len(getSpecificEgressIPStatusItems(eipAName))
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(0), "EIP-A must stay pending with no matching node")
			// Label the node → 1 of 2 EIP-A IPs assigns (other stays pending, only 1 node in pool-A)
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, nodeLabelKey, nodeLabelA)
			eipAStatus := verifySpecificEgressIPStatusLengthEquals(eipAName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			// Use the actual assigned IP from status — only one of the two EIP-A IPs is active
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podAName, true, []string{eipAStatus[0].EgressIP}).WithContext()),
				"pod-A must use EIP-A src IP after EIP-first ordering")

			// --- Step 4: node label removal ---
			ginkgo.By("4. Remove pool=a from egress1Node: EIP-A goes pending; EIP-B stays; EIP-C now only on egress2Node")
			e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, nodeLabelKey)
			verifySpecificEgressIPStatusLengthEquals(eipAName, 0, nil)
			verifySpecificEgressIPStatusLengthEquals(eipBName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})
			// EIP-C selects pool in {a,b}; with pool=a removed it can only be hosted on egress2Node (pool=b).
			verifySpecificEgressIPStatusLengthEquals(eipCName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})
			// pod-A: no EIP assigned → falls back to its own node IP
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerOrBGPServerAndTest(f, primaryTargetExternalContainer, f.Namespace.Name, podAName, pod1Node.nodeIP).WithContext()),
				"pod-A must fall back to node IP while EIP-A is pending")
			// pod-B and pod-C: EIP-B/C unaffected → still use their EIP src IPs
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podBName, true, []string{eipB.String()}).WithContext()),
				"pod-B must still use EIP-B src IP while EIP-A is pending")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podCName, true, []string{eipC1.String()}).WithContext()),
				"pod-C must still use EIP-C src IP while EIP-A is pending")

			// --- Step 5: restore label ---
			ginkgo.By("5. Restore pool=a to egress1Node: EIP-A reassigns to egress1Node; pod-A traffic restored")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, nodeLabelKey, nodeLabelA)
			// pool-A still has only 1 node → 1 of 2 EIP-A IPs assigns, other stays pending
			eipAStatus = verifySpecificEgressIPStatusLengthEquals(eipAName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			// Use the actual assigned IP from status — only one of the two EIP-A IPs is active
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podAName, true, []string{eipAStatus[0].EgressIP}).WithContext()),
				"pod-A must use EIP-A src IP after restore")

			// --- Step 6: union label change (anti-churn) ---
			// Prep: add tier=b to egress2Node and switch EIP-B selector to {tier: b}.
			// egress2Node still matches (has tier=b) so EIP-B must not move.
			ginkgo.By("6. Union label change (anti-churn): egress1Node gains tier=b eligibility; existing EIP-B on egress2Node must not move")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, tierLabelKey, tierLabelB)
			patchEipBSelectorTier := `[{"op":"replace","path":"/spec/egressNodeSelector","value":{"matchLabels":{"tier":"b"}}}]`
			e2ekubectl.RunKubectlOrDie("default", "patch", "eip", eipBName, "--type=json", "-p", patchEipBSelectorTier)
			// Verify EIP-B stays on egress2Node after selector change before capturing snapshots.
			eipBStableAfterPatch := verifySpecificEgressIPStatusLengthEquals(eipBName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipBName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(eipBStableAfterPatch), "EIP-B must stay on egress2Node after selector change to tier=b")
			// Capture snapshots after stability is confirmed.
			statusABefore6 := getSpecificEgressIPStatusItems(eipAName)
			statusBBefore6 := getSpecificEgressIPStatusItems(eipBName)
			statusCBefore6 := getSpecificEgressIPStatusItems(eipCName)

			// Op A: egress1Node joins tier=b; EIP-B selector now matches egress1Node too, but should stay on egress2Node (anti-churn)
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, tierLabelKey, tierLabelB)
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipAName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusABefore6), "EIP-A must not move when egress1Node joins tier=b")
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipBName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusBBefore6), "EIP-B must not move when egress1Node joins tier=b (anti-churn)")
			// EIP-C matches In[a,b] — egress1Node was already eligible before Op A; assignment must not change
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipCName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusCBefore6), "EIP-C must not move when egress1Node joins tier=b")

			// Op B: revert egress1Node by removing tier=b; no assignment should change
			e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, tierLabelKey)
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipAName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusABefore6), "EIP-A must not move when egress1Node leaves tier=b")
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipBName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusBBefore6), "EIP-B must not move when egress1Node leaves tier=b")
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipCName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusCBefore6), "EIP-C must not move when egress1Node leaves tier=b")

			// Op C: egress2Node joins pool=a (now two pool=a nodes)
			// EIP-A: 2 IPs + 2 eligible nodes → should spread to statusLen=2, 1 IP per node
			// EIP-C: already satisfied; must not bounce despite pool expanding
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, nodeLabelKey, nodeLabelA)
			verifySpecificEgressIPStatusLengthEquals(eipAName, 2, func(s []egressIPStatus) bool {
				nodes := map[string]bool{s[0].Node: true, s[1].Node: true}
				return nodes[egress1Node.name] && nodes[egress2Node.name]
			})
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipBName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusBBefore6), "EIP-B must not bounce when egress2Node joins pool=a")
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipCName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusCBefore6), "EIP-C must not bounce when egress2Node joins pool=a")
			// Restore egress2Node to pool=b; EIP-A drops back to statusLen=1 (one pool=a node)
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, nodeLabelKey, nodeLabelB)
			// Wait for EIP-A to drop back to 1 item on egress1Node, then verify all three stay stable.
			verifySpecificEgressIPStatusLengthEquals(eipAName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipAName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusABefore6), "EIP-A must return to original assignment when egress2Node leaves pool=a")
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipBName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusBBefore6), "EIP-B must not move when egress2Node leaves pool=a")
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipCName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusCBefore6), "EIP-C must not move when egress2Node leaves pool=a")
			// Traffic check: all three EIPs functional after pool restore
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podAName, true, []string{statusABefore6[0].EgressIP}).WithContext()),
				"pod-A traffic works after egress2Node leaves pool=a")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podBName, true, []string{eipB.String()}).WithContext()),
				"pod-B traffic works after egress2Node leaves pool=a")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podCName, true, []string{eipC1.String()}).WithContext()),
				"pod-C traffic works after egress2Node leaves pool=a")

			// --- Clean up step 6 tier changes: restore EIP-B selector to pool=b and remove tier=b from egress2Node ---
			patchEipBSelectorPoolB := `[{"op":"replace","path":"/spec/egressNodeSelector","value":{"matchLabels":{"pool":"b"}}}]`
			e2ekubectl.RunKubectlOrDie("default", "patch", "eip", eipBName, "--type=json", "-p", patchEipBSelectorPoolB)
			e2enode.RemoveLabelOffNode(f.ClientSet, egress2Node.name, tierLabelKey)
			// EIP-B must stay on egress2Node (pool=b still matches)
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipBName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusBBefore6), "EIP-B must stay on egress2Node after restoring to pool=b selector")

			// --- Step 7: EIP selector update round-trip ---
			ginkgo.By("7. EIP-B selector update: pool=b → pool=a → pool=b (full round-trip)")
			// Temporarily change EIP-B selector to pool=a (egress2Node no longer matches)
			patchPoolA := `[{"op":"replace","path":"/spec/egressNodeSelector","value":{"matchLabels":{"pool":"a"}}}]`
			e2ekubectl.RunKubectlOrDie("default", "patch", "eip", eipBName, "--type=json", "-p", patchPoolA)
			verifySpecificEgressIPStatusLengthEquals(eipBName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			// Switch back to pool=b selector; EIP-B must reassign back to egress2Node
			patchPoolB := `[{"op":"replace","path":"/spec/egressNodeSelector","value":{"matchLabels":{"pool":"b"}}}]`
			e2ekubectl.RunKubectlOrDie("default", "patch", "eip", eipBName, "--type=json", "-p", patchPoolB)
			verifySpecificEgressIPStatusLengthEquals(eipBName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podBName, true, []string{eipB.String()}).WithContext()),
				"pod-B traffic works after selector reverted to pool=b")

			// --- Step 8: active-active → active-passive ---
			ginkgo.By("8. Active-active then active-passive: EIP-A with 2 IPs across 2 pool=a nodes")
			// Add pool=a to egress2Node (replaces pool=b) → both nodes in pool=a; EIP-A spreads: 1 IP on each node
			// EIP-B loses its only eligible node (egress2Node left pool=b) → goes pending
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, nodeLabelKey, nodeLabelA)
			verifySpecificEgressIPStatusLengthEquals(eipAName, 2, func(s []egressIPStatus) bool {
				nodes := map[string]bool{s[0].Node: true, s[1].Node: true}
				return nodes[egress1Node.name] && nodes[egress2Node.name]
			})
			verifySpecificEgressIPStatusLengthEquals(eipBName, 0, nil)
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podAName, true, []string{eipA1.String(), eipA2.String()}).WithContext()),
				"pod-A traffic works in active-active state")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerOrBGPServerAndTest(f, primaryTargetExternalContainer, f.Namespace.Name, podBName, pod1Node.nodeIP).WithContext()),
				"pod-B falls back to node IP while EIP-B is pending")
			// Remove pool=a from egress1Node → only egress2Node in pool=a; 1 IP assigns, 1 stays pending
			e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, nodeLabelKey)
			eipAActive := verifySpecificEgressIPStatusLengthEquals(eipAName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podAName, true, []string{eipAActive[0].EgressIP}).WithContext()),
				"pod-A traffic works in active-passive state via the one assigned IP")
			// Restore: egress1Node back to pool=a, egress2Node back to pool=b
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, nodeLabelKey, nodeLabelA)
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, nodeLabelKey, nodeLabelB)
			verifySpecificEgressIPStatusLengthEquals(eipAName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			verifySpecificEgressIPStatusLengthEquals(eipBName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})

			// --- Step 9: EIP-C expand to 2 IPs via hostname selector (same-node egress case) ---
			ginkgo.By("9. EIP-C: selector type change (pool→hostname) must not move eipC1; then add eipC2 pinned to pod1Node (same-node egress)")
			// Capture eipC1's current node before any patch.
			eipCBefore := getSpecificEgressIPStatusItems(eipCName)
			gomega.Expect(eipCBefore).To(gomega.HaveLen(1), "EIP-C must have exactly 1 assignment before patch")
			eipC1OrigNode := eipCBefore[0].Node

			// Patch 1: selector type change only — pool In [a,b] → hostname In [egress1Node, egress2Node, pod1Node].
			// eipC1 is still eligible on its current node; assignment must not move.
			// matchLabels+matchExpressions are ANDed so we use a single matchExpressions for the union.
			patchEipCTypeChange := fmt.Sprintf(
				`{"spec":{"egressNodeSelector":{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"In","values":[%q,%q,%q]}]}}}`,
				egress1Node.name, egress2Node.name, pod1Node.name,
			)
			e2ekubectl.RunKubectlOrDie("default", "patch", "eip", eipCName, "--type=merge", "-p", patchEipCTypeChange)
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipCName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.And(
				gomega.Equal(eipCBefore),
				gomega.Satisfy(func(s []egressIPStatus) bool {
					return len(s) == 1 && s[0].Node == eipC1OrigNode
				}),
			), "eipC1 must not move when selector type changes from pool label to hostname (node still eligible)")
			// Traffic still works through eipC1 after selector type change
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podCName, true, []string{eipC1.String()}).WithContext()),
				"pod-C traffic must work via eipC1 after selector type change (before patch2)")

			// Patch 2: add eipC2, narrow selector to exactly [eipC1OrigNode, pod1Node].
			// 2 nodes for 2 IPs — eipC1 stays, eipC2 deterministically lands on pod1Node (same-node egress path).
			patchEipCAddIP := fmt.Sprintf(
				`{"spec":{"egressIPs":[%q,%q],"egressNodeSelector":{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"In","values":[%q,%q]}]}}}`,
				eipC1.String(), eipC2.String(), eipC1OrigNode, pod1Node.name,
			)
			e2ekubectl.RunKubectlOrDie("default", "patch", "eip", eipCName, "--type=merge", "-p", patchEipCAddIP)
			eipCSpread := verifySpecificEgressIPStatusLengthEquals(eipCName, 2, func(s []egressIPStatus) bool {
				nodeForIP := map[string]string{s[0].EgressIP: s[0].Node, s[1].EgressIP: s[1].Node}
				return nodeForIP[eipC1.String()] == eipC1OrigNode && nodeForIP[eipC2.String()] == pod1Node.name
			})
			// Accept either assigned IP — OVN-K decides which egress node routes pod-C's flow.
			// Check each IP independently: the shared helper requires every listed IP to be
			// observed before it succeeds, so passing both would wrongly demand both are used.
			eipCFirstFn := targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podCName, true, []string{eipCSpread[0].EgressIP})
			eipCSecondFn := targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podCName, true, []string{eipCSpread[1].EgressIP})
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true, func(context.Context) (bool, error) {
					if ok, err := eipCFirstFn(); ok || err != nil {
						return ok, err
					}
					return eipCSecondFn()
				}),
				"pod-C must use one of EIP-C's two src IPs (including same-node path via pod1Node)")

			// --- Step 10: delete EIP-C ---
			ginkgo.By("10. Delete EIP-C: pod-C falls back to node IP; EIP-A/B must not churn")
			// Snapshot A and B before deletion — nothing should change for them.
			statusABeforeC10 := getSpecificEgressIPStatusItems(eipAName)
			statusBBeforeC10 := getSpecificEgressIPStatusItems(eipBName)
			e2ekubectl.RunKubectlOrDie("default", "delete", "eip", eipCName, "--ignore-not-found=true")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerOrBGPServerAndTest(f, primaryTargetExternalContainer, f.Namespace.Name, podCName, pod1Node.nodeIP).WithContext()),
				"pod-C must use node IP after EIP-C deletion")
			// EIP-A and EIP-B must stay exactly where they were — no reassignment triggered by C's removal
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipAName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusABeforeC10),
				"EIP-A must not churn after EIP-C deletion")
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipBName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(statusBBeforeC10),
				"EIP-B must not churn after EIP-C deletion")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podAName, true, []string{statusABeforeC10[0].EgressIP}).WithContext()),
				"pod-A traffic must be unaffected by EIP-C deletion")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podBName, true, []string{statusBBeforeC10[0].EgressIP}).WithContext()),
				"pod-B traffic must be unaffected by EIP-C deletion")

			// --- Step 11: unsupported scenario — two EIPs matching same pod, different node pools ---
			// OVN-K considers a pod matching multiple EgressIP objects a user error (undefined behavior,
			// see pkg/ovn/egressip.go: "Pods should not match multiple EgressIP objects").
			// We test it anyway to assert egressNodeSelector behavior in this multiple-objects combo: even
			// when two EgressIP objects select the same pod, each EIP is still scoped to its own selected
			// node pool (EIP-X → pool-a node, EIP-Y → pool-b node; asserted below). That control-plane
			// result is the only deterministic guarantee — the datapath is intentionally undefined, so we
			// assert no traffic / do not pin the src IP (see why below).
			//
			// Why the src IP is undefined (and why pinning it flakes on interconnect/IC):
			// When a pod matches two EgressIP objects, each object's reconcile races to become the one
			// "serving" the pod. The winner is simply the first object processed for that pod
			// (podState.egressIPName, pkg/ovn/egressip.go addPodEgressIPAssignmentsWithLock); the loser is
			// parked in standbyEgressIPNames. In IC this decision is made *independently per zone* with no
			// cross-zone coordination, so zones can disagree. The datapath is then assembled from two
			// decisions taken by different zones:
			//   - the SNAT lives only on the zone that owns the winning EIP's assigned node
			//     (addPodEgressIPAssignment is a no-op unless isLocalEgressNode), and
			//   - the reroute is installed by the *pod's* zone toward whichever EIP *it* picked.
			// If the pod's zone picks EIP-Y (reroute → egress2Node) but egress2Node's zone picked EIP-X,
			// then egress2Node never programs the EIP-Y SNAT and the rerouted packet reaches a GR with no
			// matching SNAT. Hence the pod may egress with EIP-X, EIP-Y, an egress node's IP, or nothing at
			// all — all valid outcomes of this undefined scenario (enumerated below).
			ginkgo.By("11. [Unsupported] Two EIP objects matching same pod on different node pools: both assigned; datapath src IP is undefined")
			cleanupX := applyEIPManifest(eipXName, createEIPManifestWithNodeSelector(eipXName, podLabelShared, nsLabel, selectorPoolA, eipX.String()))
			defer cleanupX()
			cleanupY := applyEIPManifest(eipYName, createEIPManifestWithNodeSelector(eipYName, podLabelShared, nsLabel, selectorPoolB, eipY.String()))
			defer cleanupY()
			_, err = createGenericPodWithLabel(f, podSharedName, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podLabelShared)
			framework.ExpectNoError(err, "create pod-shared")
			// Both EIPs get assigned to their respective pool nodes (controller assigns regardless of overlap)
			verifySpecificEgressIPStatusLengthEquals(eipXName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			verifySpecificEgressIPStatusLengthEquals(eipYName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})
			// We deliberately assert no datapath here — with the split-brain above, the src IP is undefined
			// and may be any of:
			//   - either EgressIP, when the pod's zone and the winning EIP's node agree;
			//   - an egress node's IP, when they disagree but that node still masquerades the rerouted packet
			//     (e.g. a primary UDN whose cluster-wide subnet SNAT also matches the foreign pod); or
			//   - no connectivity at all (curl connect timeout, rc 28), when they disagree and the reroute
			//     lands on a node with no matching SNAT: on the default network each node's SNAT is scoped to
			//     its own pod subnet, so a rerouted foreign pod matches nothing and the packet is dropped.
			// Any traffic assertion would flake (and a transient blackhole would even pass instantly), so the
			// egressNodeSelector guarantee we verify is the deterministic control-plane one asserted above.

			// --- Step 12: simultaneous churn ---
			ginkgo.By("12. Simultaneous churn: remove pool=a label and patch EIP-A selector concurrently; verify convergence without duplication")
			e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, nodeLabelKey)
			e2ekubectl.RunKubectlOrDie("default", "patch", "eip", eipAName, "--type=json", "-p", patchPoolB)
			// pool=b has 1 node (egress2Node) → 1 of 2 EIP-A IPs assigns, other stays pending
			eipAStatusStep12 := verifySpecificEgressIPStatusLengthEquals(eipAName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podAName, true, []string{eipAStatusStep12[0].EgressIP}).WithContext()),
				"pod-A traffic converges to the single assigned EIP-A IP on egress2Node after churn")
		})

		// -----------------------------------------------------------------------
		// Backward compat — EIP without egressNodeSelector
		// -----------------------------------------------------------------------
		ginkgo.It("[Primary EgressIP] egressNodeSelector backward compatibility: legacy egress-assignable label", func() {
			// egressNodeSelector is a control-plane feature; datapath differences between
			// network types don't affect it. Run only on the cluster default network.
			// Steps 1, 3, 4 use iptables on the gRPC healthcheck port.
			// Skip if not in gRPC mode (port not configured or using legacy TCP probe).
			ovnKubeNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
			portNode := getTemplateContainerEnv(ovnKubeNamespace, "daemonset/ovnkube-node", getNodeContainerName(), OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME)
			if portNode == "" || portNode == OVN_EGRESSIP_LEGACY_HEALTHCHECK_PORT_ENV {
				ginkgo.Skip("health-check port not configured for gRPC mode; skipping iptables-based steps")
			}

			const eipLegacyName = "egressip-ns-legacy-compat"
			const podLegacyName = "egressip-legacy-pod"
			nsLabel := map[string]string{"name": f.Namespace.Name}
			updateNamespaceLabels(f, f.Namespace, nsLabel)
			podLabel := map[string]string{"wants": "egress-legacy"}

			eipLegacyIP := newEgressIP()

			ginkgo.DeferCleanup(func() {
				e2ekubectl.RunKubectlOrDie("default", "delete", "eip", eipLegacyName, "--ignore-not-found=true")
				e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable")
				e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "pod", podLegacyName, "--ignore-not-found=true")
			})

			ginkgo.By("1. Create EIP-legacy with no egressNodeSelector field: CRD default fills egress-assignable: Exists")
			// Block healthcheck probes on all nodes so no node can be marked reachable during initial setup.
			// This guarantees the EIP stays pending regardless of any pre-existing node state.
			for _, n := range []string{egress1Node.name, egress2Node.name, pod1Node.name} {
				setNodeReachable(n, false)
				defer setNodeReachable(n, true)
			}
			// nodeSelector=nil → field absent from YAML → CRD default fires
			cleanup := applyEIPManifest(eipLegacyName, createEIPManifestWithNodeSelector(eipLegacyName, podLabel, nsLabel, nil, eipLegacyIP.String()))
			defer cleanup()
			// No reachable node + no egress-assignable label → must stay pending
			gomega.Consistently(func() int {
				return len(getSpecificEgressIPStatusItems(eipLegacyName))
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(0), "EIP-legacy must stay pending with no reachable/assignable node")

			ginkgo.By("2. Restore healthcheck + label egress1Node with egress-assignable: EIP-legacy assigns")
			setNodeReachable(egress1Node.name, true)
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
			verifySpecificEgressIPStatusLengthEquals(eipLegacyName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			_, err := createGenericPodWithLabel(f, podLegacyName, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podLabel)
			framework.ExpectNoError(err, "create pod-legacy")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podLegacyName, true, []string{eipLegacyIP.String()}).WithContext()),
				"pod-legacy must use EIP-legacy src IP")

			ginkgo.By("3. Block healthcheck probes to egress1Node: OVN-K marks node unreachable, EIP-legacy goes pending")
			// Drop port-9107 traffic on egress1Node — OVN-K's health prober can no longer reach it.
			// This exercises the healthcheck probe pool path: node is evicted and EIP unassigned.
			setNodeReachable(egress1Node.name, false)
			verifySpecificEgressIPStatusLengthEquals(eipLegacyName, 0, nil)
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerOrBGPServerAndTest(f, primaryTargetExternalContainer, f.Namespace.Name, podLegacyName, pod1Node.nodeIP).WithContext()),
				"pod-legacy must fall back to node IP when egress1Node is unreachable")

			ginkgo.By("4. Restore healthcheck probes: egress1Node re-enters probe pool; EIP-legacy reassigns; traffic restored")
			setNodeReachable(egress1Node.name, true)
			verifySpecificEgressIPStatusLengthEquals(eipLegacyName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podLegacyName, true, []string{eipLegacyIP.String()}).WithContext()),
				"pod-legacy must use EIP-legacy src IP after egress1Node becomes reachable")

			ginkgo.By("5. Remove egress-assignable label: EIP-legacy goes pending (label-based eviction path)")
			e2ekubectl.RunKubectlOrDie("default", "label", "node", egress1Node.name, "k8s.ovn.org/egress-assignable-")
			verifySpecificEgressIPStatusLengthEquals(eipLegacyName, 0, nil)
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerOrBGPServerAndTest(f, primaryTargetExternalContainer, f.Namespace.Name, podLegacyName, pod1Node.nodeIP).WithContext()),
				"pod-legacy must fall back to node IP after egress-assignable label removed")

			ginkgo.By("6. Restore egress-assignable label: EIP-legacy reassigns; traffic restored")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
			verifySpecificEgressIPStatusLengthEquals(eipLegacyName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podLegacyName, true, []string{eipLegacyIP.String()}).WithContext()),
				"pod-legacy must use EIP-legacy src IP after label restore")

			ginkgo.By("7. Delete EIP-legacy: pod-legacy falls back to node IP permanently")
			e2ekubectl.RunKubectlOrDie("default", "delete", "egressip", eipLegacyName, "--ignore-not-found=true")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerOrBGPServerAndTest(f, primaryTargetExternalContainer, f.Namespace.Name, podLegacyName, pod1Node.nodeIP).WithContext()),
				"pod-legacy must use node IP after EIP deletion")
		})

		// -----------------------------------------------------------------------
		// Mixed fleet — legacy + custom selector EIPs coexist
		// -----------------------------------------------------------------------
		ginkgo.It("[Primary EgressIP] egressNodeSelector mixed fleet: legacy and custom selectors coexist", func() {
			const (
				eipOldName  = "egressip-ns-mixed-old"
				eipNewName  = "egressip-ns-mixed-new"
				podOldName  = "egressip-mixed-pod-old"
				podNewName  = "egressip-mixed-pod-new"
				poolBetaKey = "pool"
				poolBetaVal = "beta"
			)
			nsLabel := map[string]string{"name": f.Namespace.Name}
			updateNamespaceLabels(f, f.Namespace, nsLabel)
			podLabelOld := map[string]string{"wants": "egress-old"}
			podLabelNew := map[string]string{"wants": "egress-new"}

			eipOldIP := newEgressIP()
			eipNewIP := newEgressIP()

			selectorBeta := &metav1.LabelSelector{MatchLabels: map[string]string{poolBetaKey: poolBetaVal}}

			ginkgo.DeferCleanup(func() {
				for _, name := range []string{eipOldName, eipNewName} {
					e2ekubectl.RunKubectlOrDie("default", "delete", "eip", name, "--ignore-not-found=true")
					os.Remove(filepath.Join(os.TempDir(), name+".yaml"))
				}
				e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable")
				e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, poolBetaKey)
				e2enode.RemoveLabelOffNode(f.ClientSet, egress2Node.name, poolBetaKey)
				for _, podName := range []string{podOldName, podNewName} {
					e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "pod", podName, "--ignore-not-found=true")
				}
			})

			ginkgo.By("Setup: egress1Node=egress-assignable+pool=alpha; egress2Node=pool=beta only")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, poolBetaKey, poolBetaVal)

			// EIP-old: no egressNodeSelector → CRD default → egress-assignable: Exists
			cleanupOld := applyEIPManifest(eipOldName, createEIPManifestWithNodeSelector(eipOldName, podLabelOld, nsLabel, nil, eipOldIP.String()))
			defer cleanupOld()
			// EIP-new: custom selector pool=beta → only egress2Node
			cleanupNew := applyEIPManifest(eipNewName, createEIPManifestWithNodeSelector(eipNewName, podLabelNew, nsLabel, selectorBeta, eipNewIP.String()))
			defer cleanupNew()

			ginkgo.By("1. Initial assignment: each EIP stays in its own pool; no cross-contamination")
			verifySpecificEgressIPStatusLengthEquals(eipOldName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			verifySpecificEgressIPStatusLengthEquals(eipNewName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})
			// egress2Node must not appear in EIP-old (no egress-assignable)
			for _, item := range getSpecificEgressIPStatusItems(eipOldName) {
				gomega.Expect(item.Node).NotTo(gomega.Equal(egress2Node.name), "EIP-old must not land on egress2Node (no egress-assignable label)")
			}
			// egress1Node must not appear in EIP-new (no pool=beta)
			for _, item := range getSpecificEgressIPStatusItems(eipNewName) {
				gomega.Expect(item.Node).NotTo(gomega.Equal(egress1Node.name), "EIP-new must not land on egress1Node (no pool=beta)")
			}

			ginkgo.By("2. Traffic isolation: each pod uses its own EIP src IP")
			_, err := createGenericPodWithLabel(f, podOldName, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podLabelOld)
			framework.ExpectNoError(err, "create pod-old")
			_, err = createGenericPodWithLabel(f, podNewName, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podLabelNew)
			framework.ExpectNoError(err, "create pod-new")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podOldName, true, []string{eipOldIP.String()}).WithContext()),
				"pod-old uses EIP-old src IP")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podNewName, true, []string{eipNewIP.String()}).WithContext()),
				"pod-new uses EIP-new src IP")

			ginkgo.By("3. Add pool=beta to egress1Node: EIP-new gains eligible node; EIP-old stays on egress1Node")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, poolBetaKey, poolBetaVal)
			// EIP-old must stay on egress1Node (egress-assignable label unchanged)
			gomega.Consistently(func() bool {
				s := getSpecificEgressIPStatusItems(eipOldName)
				return len(s) == 1 && s[0].Node == egress1Node.name
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.BeTrue(), "EIP-old must not move when egress1Node gains pool=beta")

			ginkgo.By("4. Remove egress-assignable from egress1Node: EIP-old pending; EIP-new unaffected")
			e2ekubectl.RunKubectlOrDie("default", "label", "node", egress1Node.name, "k8s.ovn.org/egress-assignable-")
			verifySpecificEgressIPStatusLengthEquals(eipOldName, 0, nil)
			verifySpecificEgressIPStatusLengthEquals(eipNewName, 1, nil)
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerOrBGPServerAndTest(f, primaryTargetExternalContainer, f.Namespace.Name, podOldName, pod1Node.nodeIP).WithContext()),
				"pod-old falls back to node IP while EIP-old pending")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podNewName, true, []string{eipNewIP.String()}).WithContext()),
				"pod-new unaffected by EIP-old going pending")

			ginkgo.By("5. Restore egress-assignable on egress1Node: EIP-old reassigns; both pools healthy")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
			verifySpecificEgressIPStatusLengthEquals(eipOldName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podOldName, true, []string{eipOldIP.String()}).WithContext()),
				"pod-old traffic restored after egress-assignable label restored")

			ginkgo.By("6. Migrate EIP-old from the legacy egress-assignable selector to pool=beta: it moves to the pool=beta node")
			// Make egress2Node the only pool=beta node so the selector switch forces a real move.
			e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, poolBetaKey)
			// EIP-old is still on egress1Node via its legacy egress-assignable selector; switch it to pool=beta.
			patchOldToBeta := `[{"op":"replace","path":"/spec/egressNodeSelector","value":{"matchLabels":{"pool":"beta"}}}]`
			e2ekubectl.RunKubectlOrDie("default", "patch", "eip", eipOldName, "--type=json", "-p", patchOldToBeta)
			// EIP-old must migrate to egress2Node (the only pool=beta node); egress1Node is no longer eligible.
			verifySpecificEgressIPStatusLengthEquals(eipOldName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})
			// EIP-new is unaffected and stays on egress2Node.
			verifySpecificEgressIPStatusLengthEquals(eipNewName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})
			// pod-old still egresses via EIP-old's IP, now sourced from egress2Node.
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podOldName, true, []string{eipOldIP.String()}).WithContext()),
				"pod-old uses EIP-old src IP after migrating its selector to pool=beta")
		})

		// -----------------------------------------------------------------------
		// Multiple label ops in a single patch (remove one matchLabel + add another
		// atomically) move the EgressIP to a node matched by the new key.
		// -----------------------------------------------------------------------
		ginkgo.It("[Primary EgressIP] egressNodeSelector multiple label ops in a single patch move the EgressIP", func() {
			const (
				eipName = "egressip-ns-swap"
				podName = "egressip-swap-pod"
				poolKey = "pool"
				poolVal = "a"
				tierKey = "tier"
				tierVal = "b"
			)
			nsLabel := map[string]string{"name": f.Namespace.Name}
			updateNamespaceLabels(f, f.Namespace, nsLabel)
			podLabel := map[string]string{"wants": "egress-swap"}

			eipIP := newEgressIP()
			selectorPool := &metav1.LabelSelector{MatchLabels: map[string]string{poolKey: poolVal}}

			ginkgo.DeferCleanup(func() {
				e2ekubectl.RunKubectlOrDie("default", "delete", "eip", eipName, "--ignore-not-found=true")
				os.Remove(filepath.Join(os.TempDir(), eipName+".yaml"))
				e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, poolKey)
				e2enode.RemoveLabelOffNode(f.ClientSet, egress2Node.name, tierKey)
				e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "pod", podName, "--ignore-not-found=true")
			})

			ginkgo.By("Setup: egress1Node=pool=a, egress2Node=tier=b (different label schemes)")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, poolKey, poolVal)
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, tierKey, tierVal)

			ginkgo.By("1. EIP selects pool=a: assigns to egress1Node; pod egresses via the EIP IP")
			cleanup := applyEIPManifest(eipName, createEIPManifestWithNodeSelector(eipName, podLabel, nsLabel, selectorPool, eipIP.String()))
			defer cleanup()
			verifySpecificEgressIPStatusLengthEquals(eipName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			_, err := createGenericPodWithLabel(f, podName, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podLabel)
			framework.ExpectNoError(err, "create swap pod")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podName, true, []string{eipIP.String()}).WithContext()),
				"pod must egress via the EIP IP while assigned to egress1Node (pool=a)")

			ginkgo.By("2. Single patch, two label ops (remove pool, add tier): EIP moves to egress2Node; traffic preserved")
			// One PATCH carrying two ops applied atomically: drop the pool matchLabel and add the tier matchLabel.
			patchToTier := `[{"op":"remove","path":"/spec/egressNodeSelector/matchLabels/pool"},{"op":"add","path":"/spec/egressNodeSelector/matchLabels/tier","value":"b"}]`
			e2ekubectl.RunKubectlOrDie("default", "patch", "eip", eipName, "--type=json", "-p", patchToTier)
			verifySpecificEgressIPStatusLengthEquals(eipName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress2Node.name
			})
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podName, true, []string{eipIP.String()}).WithContext()),
				"pod must egress via the EIP IP after it moves to egress2Node (tier=b)")

			ginkgo.By("3. Single patch, two label ops (remove tier, add pool): EIP returns to egress1Node; traffic preserved")
			// One PATCH carrying two ops applied atomically: drop the tier matchLabel and add the pool matchLabel.
			patchToPool := `[{"op":"remove","path":"/spec/egressNodeSelector/matchLabels/tier"},{"op":"add","path":"/spec/egressNodeSelector/matchLabels/pool","value":"a"}]`
			e2ekubectl.RunKubectlOrDie("default", "patch", "eip", eipName, "--type=json", "-p", patchToPool)
			verifySpecificEgressIPStatusLengthEquals(eipName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podName, true, []string{eipIP.String()}).WithContext()),
				"pod must egress via the EIP IP after it returns to egress1Node (pool=a)")
		})

		// -----------------------------------------------------------------------
		// Health-check probe scoping
		// -----------------------------------------------------------------------
		ginkgo.It("[Primary EgressIP] egressNodeSelector scopes health-check probes to matching nodes", func() {
			if isUserDefinedNetwork(netConfigParams) {
				ginkgo.Skip("health-check probe scoping is network-type agnostic; CDN coverage sufficient")
			}
			// Detect whether gRPC health-check mode is active.
			ovnKubeNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
			portNode := getTemplateContainerEnv(ovnKubeNamespace, "daemonset/ovnkube-node", getNodeContainerName(), OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME)
			if portNode == "" || portNode == OVN_EGRESSIP_LEGACY_HEALTHCHECK_PORT_ENV {
				ginkgo.Skip("health-check port not configured for gRPC mode; skipping probe-scope E2E")
			}
			const (
				eipHCName = "egressip-ns-hc-scope"
				podHCName = "egressip-hc-pod"
				hcPoolKey = "pool"
				hcPoolVal = "hc-a"
			)
			nsLabel := map[string]string{"name": f.Namespace.Name}
			updateNamespaceLabels(f, f.Namespace, nsLabel)
			podLabel := map[string]string{"wants": "egress-hc"}
			eipHCIP := newEgressIP()
			selectorHC := &metav1.LabelSelector{MatchLabels: map[string]string{hcPoolKey: hcPoolVal}}

			ginkgo.DeferCleanup(func() {
				e2ekubectl.RunKubectlOrDie("default", "delete", "eip", eipHCName, "--ignore-not-found=true")
				os.Remove(filepath.Join(os.TempDir(), eipHCName+".yaml"))
				e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, hcPoolKey)
				e2enode.RemoveLabelOffNode(f.ClientSet, egress2Node.name, hcPoolKey)
				setNodeReachable(egress1Node.name, true)
				setNodeReachable(egress2Node.name, true)
				e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "pod", podHCName, "--ignore-not-found=true")
			})

			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, hcPoolKey, hcPoolVal)
			// egress2Node has no matching label initially
			cleanupHC := applyEIPManifest(eipHCName, createEIPManifestWithNodeSelector(eipHCName, podLabel, nsLabel, selectorHC, eipHCIP.String()))
			defer cleanupHC()
			verifySpecificEgressIPStatusLengthEquals(eipHCName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			_, err := createGenericPodWithLabel(f, podHCName, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podLabel)
			framework.ExpectNoError(err, "create pod-hc")

			ginkgo.By("1. Block health-check port on egress2Node (not in any EIP pool): EIP-HC status unchanged")
			setNodeReachable(egress2Node.name, false)
			// Wait longer than one health-check tick (~5 s) then verify stability
			gomega.Consistently(func() bool {
				s := getSpecificEgressIPStatusItems(eipHCName)
				return len(s) == 1 && s[0].Node == egress1Node.name
			}, 20*retryInterval, retryInterval).Should(gomega.BeTrue(),
				"EIP-HC must not be affected by health-check block on egress2Node (not in pool)")

			ginkgo.By("2. Restore egress2Node; block health-check on egress1Node (in pool=hc-a): EIP-HC de-assigns")
			setNodeReachable(egress2Node.name, true)
			setNodeReachable(egress1Node.name, false)
			verifySpecificEgressIPStatusLengthEquals(eipHCName, 0, nil)

			ginkgo.By("3. Restore egress1Node health-check: EIP-HC reassigns; traffic restored")
			setNodeReachable(egress1Node.name, true)
			verifySpecificEgressIPStatusLengthEquals(eipHCName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podHCName, true, []string{eipHCIP.String()}).WithContext()),
				"pod-hc traffic restored after egress1Node health-check restored")

			ginkgo.By("4. Add egress2Node to pool=hc-a: verify it now enters the probe set")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, hcPoolKey, hcPoolVal)
			// EIP-HC may spread to egress2Node; block 9107 on egress2Node → EIP-HC should react
			gomega.Eventually(func() bool {
				s := getSpecificEgressIPStatusItems(eipHCName)
				// Either node could hold the EIP now; just ensure there is one
				return len(s) == 1
			}, retryTimeout, retryInterval).Should(gomega.BeTrue(), "EIP-HC assigned after egress2Node joins pool")
			setNodeReachable(egress2Node.name, false)
			// Blocking egress2Node's probe leaves egress1Node as the only reachable pool
			// node, so EIP-HC must end up (len 1) on egress1Node and stay there.
			verifySpecificEgressIPStatusLengthEquals(eipHCName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name
			})
			gomega.Consistently(func() bool {
				s := getSpecificEgressIPStatusItems(eipHCName)
				return len(s) == 1 && s[0].Node == egress1Node.name
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.BeTrue(),
				"EIP-HC must move off egress2Node when its probe is blocked and stay on egress1Node")
			setNodeReachable(egress2Node.name, true)

			ginkgo.By("5. Delete EIP-HC: blocking health-check on egress1Node has no effect (no EIP to de-assign)")
			e2ekubectl.RunKubectlOrDie("default", "delete", "eip", eipHCName, "--ignore-not-found=true")
			setNodeReachable(egress1Node.name, false)
			// Nothing should happen — no EIP exists. Just confirm no panics / no orphaned status.
			gomega.Consistently(func() bool {
				return len(getSpecificEgressIPStatusItems(eipHCName)) == 0
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.BeTrue(),
				"no EIP status after EIP-HC deleted even with health-check blocked")
			setNodeReachable(egress1Node.name, true)
		})

		// -----------------------------------------------------------------------
		// secondary-host-eip + egressNodeSelector
		// -----------------------------------------------------------------------
		ginkgo.It("[Multi-NIC EgressIP] egressNodeSelector restricts assignment to labeled node", func() {
			if isUserDefinedNetwork(netConfigParams) {
				ginkgo.Skip("secondary-host-eip is unsupported for UDNs")
			}
			const (
				eipSecName = "egressip-ns-secondary-host"
				podSecName = "egressip-secondary-pod"
				secPoolKey = "secondary-pool"
				secPoolVal = "alpha"
			)
			nsLabel := map[string]string{"name": f.Namespace.Name}
			updateNamespaceLabels(f, f.Namespace, nsLabel)
			podLabel := map[string]string{"wants": "egress-secondary"}

			// One OVN-network IP + one secondary-host IP. A node hosts at most one
			// egress IP per EgressIP object, so both IPs can only be active at once
			// when two nodes match the egressNodeSelector - each then hosts exactly
			// one of them. The test grows the labeled pool one node at a time to
			// exercise this.
			// Secondary-host IP is picked from the appropriate subnet based on IP family.
			eipOVN := newEgressIP()
			eipSecondaryHost := "10.10.10.200" // secondaryIPV4Subnet
			if isIPv6TestRun {
				eipSecondaryHost = "2001:db8:abcd:1234:c001::" // secondaryIPV6Subnet
			}

			selectorSecPool := &metav1.LabelSelector{MatchLabels: map[string]string{secPoolKey: secPoolVal}}

			ginkgo.DeferCleanup(func() {
				e2ekubectl.RunKubectlOrDie("default", "delete", "eip", eipSecName, "--ignore-not-found=true")
				os.Remove(filepath.Join(os.TempDir(), eipSecName+".yaml"))
				e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, secPoolKey)
				e2enode.RemoveLabelOffNode(f.ClientSet, egress2Node.name, secPoolKey)
				e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "pod", podSecName, "--ignore-not-found=true")
			})

			ginkgo.By("1. Label egress1Node with secondary-pool=alpha; create EIP with a single secondary-host IP and egressNodeSelector")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, secPoolKey, secPoolVal)
			// egress2Node has no matching label yet.
			cleanupSec := applyEIPManifest(eipSecName, createEIPManifestWithNodeSelector(eipSecName, podLabel, nsLabel, selectorSecPool, eipSecondaryHost))
			defer cleanupSec()

			verifySpecificEgressIPStatusLengthEquals(eipSecName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name && s[0].EgressIP == eipSecondaryHost
			})

			ginkgo.By("2. Traffic uses EIP src IP from secondary interface")
			_, err := createGenericPodWithLabel(f, podSecName, pod1Node.name, f.Namespace.Name, getAgnHostHTTPPortBindFullCMD(clusterNetworkHTTPPort), podLabel)
			framework.ExpectNoError(err, "create pod-secondary")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(secondaryTargetExternalContainer, f.Namespace.Name, podSecName, true, []string{eipSecondaryHost}).WithContext()),
				"pod-secondary must use secondary-host EIP src IP")

			// Only egress1Node is labeled and it already hosts the secondary-host IP;
			// a node holds one IP per EIP, so the added OVN IP has nowhere to land.
			ginkgo.By("3. Add the OVN-network IP: secondary-host assignment is untouched, OVN IP stays pending")
			patchAddOVN := fmt.Sprintf(`[{"op":"add","path":"/spec/egressIPs/-","value":"%s"}]`, eipOVN.String())
			e2ekubectl.RunKubectlOrDie("default", "patch", "eip", eipSecName, "--type=json", "-p", patchAddOVN)
			// The pre-existing secondary-host assignment must remain and the OVN IP
			// has no free labeled node, so status length stays at 1.
			secOnly := verifySpecificEgressIPStatusLengthEquals(eipSecName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name && s[0].EgressIP == eipSecondaryHost
			})
			gomega.Consistently(func() []egressIPStatus {
				return getSpecificEgressIPStatusItems(eipSecName)
			}, 2*time.Second, 500*time.Millisecond).Should(gomega.Equal(secOnly), "secondary-host assignment must not move and OVN IP must stay pending after patch")
			// The working secondary-host path must be unaffected by the patch.
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(secondaryTargetExternalContainer, f.Namespace.Name, podSecName, true, []string{eipSecondaryHost}).WithContext()),
				"pod-secondary must keep using the secondary-host EIP after adding the (pending) OVN IP")

			ginkgo.By("4. Remove secondary-pool=alpha from egress1Node: no labeled node remains, both IPs pending")
			e2enode.RemoveLabelOffNode(f.ClientSet, egress1Node.name, secPoolKey)
			verifySpecificEgressIPStatusLengthEquals(eipSecName, 0, nil)
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerOrBGPServerAndTest(f, primaryTargetExternalContainer, f.Namespace.Name, podSecName, pod1Node.nodeIP).WithContext()),
				"pod-secondary falls back to node IP while both EIPs pending")

			ginkgo.By("5. Label egress1Node again: exactly one IP becomes assignable (a node hosts one IP per EIP)")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, secPoolKey, secPoolVal)
			oneBack := verifySpecificEgressIPStatusLengthEquals(eipSecName, 1, func(s []egressIPStatus) bool {
				return s[0].Node == egress1Node.name && (s[0].EgressIP == eipSecondaryHost || s[0].EgressIP == eipOVN.String())
			})
			// Which of the two IPs returns first is not deterministic (the set of
			// IPs to assign is unordered). The secondary-host IP is reached via the
			// secondary target, the OVN IP via the primary target; verify traffic
			// for whichever one landed.
			target := primaryTargetExternalContainer
			if oneBack[0].EgressIP == eipSecondaryHost {
				target = secondaryTargetExternalContainer
			}
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(target, f.Namespace.Name, podSecName, true, []string{oneBack[0].EgressIP}).WithContext()),
				"pod-secondary must use the assigned EIP src IP after first label restore")

			ginkgo.By("6. Label egress2Node too: the pending IP is assigned; both IPs are active, one per labeled node")
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, secPoolKey, secPoolVal)
			// egress1Node keeps the IP it already holds; the still-pending IP is the
			// other one and has only egress2Node free to land on.
			otherIP := eipOVN.String()
			if oneBack[0].EgressIP == eipOVN.String() {
				otherIP = eipSecondaryHost
			}
			verifySpecificEgressIPStatusLengthEquals(eipSecName, 2, func(s []egressIPStatus) bool {
				byNode := map[string]string{s[0].Node: s[0].EgressIP, s[1].Node: s[1].EgressIP}
				return byNode[egress1Node.name] == oneBack[0].EgressIP && byNode[egress2Node.name] == otherIP
			})
			// Both IPs are now active: secondary-target traffic uses the
			// secondary-host IP and primary-target traffic uses the OVN IP -
			// either source is valid depending on the destination.
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(secondaryTargetExternalContainer, f.Namespace.Name, podSecName, true, []string{eipSecondaryHost}).WithContext()),
				"pod-secondary must use secondary-host EIP src IP once both IPs are active")
			framework.ExpectNoError(
				wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true,
					targetExternalContainerAndTest(primaryTargetExternalContainer, f.Namespace.Name, podSecName, true, []string{eipOVN.String()}).WithContext()),
				"pod-secondary must use OVN EIP src IP once both IPs are active")
		})

	},
		ginkgo.Entry(
			"Cluster Default",
			networkAttachmentConfigParams{
				networkName: types.DefaultNetworkName,
				topology:    types.Layer3Topology,
			},
		),
		// FIXME: fix tests for CDN to specify IPv4 and IPv6 entries in-order to enable testing all IP families on dual stack clusters
		ginkgo.Entry(
			"IPv4 L3 role primary",
			feature.NetworkSegmentation,
			networkAttachmentConfigParams{
				name:     "l3primaryv4",
				topology: types.Layer3Topology,
				cidr:     primaryLayer3MultiIPv4CIDRs(),
				role:     "primary",
			},
		),
		ginkgo.Entry(
			"IPv6 L3 role primary",
			feature.NetworkSegmentation,
			networkAttachmentConfigParams{
				name:     "l3primaryv6",
				topology: types.Layer3Topology,
				cidr:     primaryLayer3MultiIPv6CIDRs(),
				role:     "primary",
			},
		),
		ginkgo.Entry(
			"IPv4 L2 role primary",
			feature.NetworkSegmentation,
			networkAttachmentConfigParams{
				name:     "l2primary",
				topology: types.Layer2Topology,
				cidr:     "20.10.0.0/16",
				role:     "primary",
			},
		),
		ginkgo.Entry(
			"IPv6 L2 role primary",
			feature.NetworkSegmentation,
			networkAttachmentConfigParams{
				name:     "l2primary",
				topology: types.Layer2Topology,
				cidr:     "2015:100:200::0/60",
				role:     "primary",
			},
		),
	)
})

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = ginkgo.Describe("Node Shutdown and Startup", ginkgo.Serial, func() {
	const (
		nodeShutdownTimeout = 5 * time.Minute
		nodeStartupTimeout  = 10 * time.Minute
	)

	var (
		f            *framework.Framework
		testNodeName string
	)

	f = wrappedTestFramework("node-shutdown-startup")

	ginkgo.BeforeEach(func() {
		testNodeName = ""
		// Skip test if not using kind provider
		if infraprovider.Get().Name() != "kind" {
			e2eskipper.Skipf("Node shutdown/startup test only supported for kind provider, got: %s", infraprovider.Get().Name())
		}

		// Get a worker node for testing (skip master/control-plane nodes)
		nodes, err := e2enode.GetReadySchedulableNodes(context.TODO(), f.ClientSet)
		framework.ExpectNoError(err, "Failed to get ready schedulable nodes")

		if len(nodes.Items) < 2 {
			e2eskipper.Skipf("Test requires at least 2 nodes, found %d", len(nodes.Items))
		}

		// Find a worker node (not master/control-plane)
		for _, node := range nodes.Items {
			if !isControlPlaneNode(node) {
				testNodeName = node.Name
				break
			}
		}

		if testNodeName == "" {
			e2eskipper.Skipf("No worker nodes found for testing")
		}

		framework.Logf("Using node %s for shutdown/startup test", testNodeName)
	})

	ginkgo.It("should maintain cluster health after node shutdown and startup", func() {
		err := waitOVNKubernetesHealthy(f)
		framework.ExpectNoError(err, "OVN-Kubernetes cluster should be healthy initially")

		ginkgo.By("Check breth0 IP address families before shutdown")
		initialIPFamilies, err := getBridgeIPAddressFamilies(testNodeName)
		framework.ExpectNoError(err, "Should be able to get breth0 IP address families before shutdown")
		framework.Logf("Node %s breth0 initial IP families: IPv4=%v, IPv6=%v", testNodeName, initialIPFamilies.hasIPv4, initialIPFamilies.hasIPv6)
		if !initialIPFamilies.hasIPv4 && !initialIPFamilies.hasIPv6 {
			framework.Failf("breth0 should have at least one IP address family (IPv4 or IPv6) before shutdown, but found IPv4=%v, IPv6=%v", initialIPFamilies.hasIPv4, initialIPFamilies.hasIPv6)
		}

		ginkgo.By("Shut down the node")
		framework.Logf("Shutting down node %s", testNodeName)
		err = infraprovider.Get().ShutdownNode(testNodeName)
		framework.ExpectNoError(err, "Failed to shutdown node %s", testNodeName)

		// Ensure node is started back up regardless of test failure
		defer func() {
			// If the test failed, dump container logs from the node before cleanup
			if ginkgo.CurrentSpecReport().Failed() {
				framework.Logf("Test failed, dumping container logs from node %s", testNodeName)
				dumpContainerLogsFromNode(testNodeName)
			}

			framework.Logf("Ensuring node %s is started (cleanup)", testNodeName)
			if startErr := infraprovider.Get().StartNode(testNodeName); startErr != nil {
				framework.Logf("Failed to start node %s during cleanup: %v", testNodeName, startErr)
			} else {
				// Wait for the node to become Ready after startup in cleanup
				framework.Logf("Waiting for node %s to become Ready after cleanup startup", testNodeName)
				waitForNodeReadyState(f, testNodeName, nodeStartupTimeout, true)
			}
		}()

		// Wait for the node to be marked as NotReady
		ginkgo.By("Waiting for node to be marked as NotReady")
		waitForNodeReadyState(f, testNodeName, nodeShutdownTimeout, false)

		ginkgo.By("Start the node")
		framework.Logf("Starting node %s", testNodeName)
		err = infraprovider.Get().StartNode(testNodeName)
		framework.ExpectNoError(err, "Failed to start node %s", testNodeName)

		// Wait for the node to become Ready again
		ginkgo.By("Waiting for node to become Ready")
		waitForNodeReadyState(f, testNodeName, nodeStartupTimeout, true)

		ginkgo.By("Confirm that ovn-k cluster is back to healthy after all services are settled")
		err = waitOVNKubernetesHealthy(f)
		framework.ExpectNoError(err, "OVN-Kubernetes cluster should be healthy after node restart")

		ginkgo.By("Confirm that breth0 on the node has IP addresses of expected families (that were moved from eth0)")
		err = checkBridgeIPAddressFamilies(testNodeName, initialIPFamilies)
		framework.ExpectNoError(err, "breth0 should have IP addresses of the same families as before restart")

		framework.Logf("Node shutdown/startup test completed successfully for node %s", testNodeName)
	})
})

// isControlPlaneNode checks if a node is a control plane node
func isControlPlaneNode(node corev1.Node) bool {
	if _, exists := node.Labels["node-role.kubernetes.io/control-plane"]; exists {
		return true
	}

	for _, taint := range node.Spec.Taints {
		if taint.Key == "node-role.kubernetes.io/control-plane" {
			return true
		}
	}

	return false
}

// ipAddressFamilies represents which IP address families are present on an interface
type ipAddressFamilies struct {
	hasIPv4 bool
	hasIPv6 bool
}

// getBridgeIPAddressFamilies checks which IP address families are present on breth0 interface
func getBridgeIPAddressFamilies(nodeName string) (ipAddressFamilies, error) {
	// TODO: change name of the bridge if running on non-kind clusters
	stdout, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "addr", "show", "breth0"})
	if err != nil {
		return ipAddressFamilies{}, fmt.Errorf("failed to get breth0 interface info on node %s: %v", nodeName, err)
	}

	families := ipAddressFamilies{
		hasIPv4: strings.Contains(stdout, "inet "),
		hasIPv6: strings.Contains(stdout, "inet6 "),
	}

	if !families.hasIPv4 && !families.hasIPv6 {
		return families, fmt.Errorf("breth0 interface on node %s has no IP addresses (neither IPv4 nor IPv6)", nodeName)
	}

	return families, nil
}

// checkBridgeIPAddressFamilies verifies that breth0 interface has IP addresses of the expected families
func checkBridgeIPAddressFamilies(nodeName string, expectedFamilies ipAddressFamilies) error {
	return wait.PollImmediate(2*time.Second, 60*time.Second, func() (bool, error) {
		currentFamilies, err := getBridgeIPAddressFamilies(nodeName)
		if err != nil {
			framework.Logf("Error checking breth0 IP families on node %s: %v", nodeName, err)
			return false, nil
		}

		// Check if current families match expected families
		if currentFamilies.hasIPv4 == expectedFamilies.hasIPv4 && currentFamilies.hasIPv6 == expectedFamilies.hasIPv6 {
			framework.Logf("Node %s breth0 has expected IP address families: IPv4=%v, IPv6=%v",
				nodeName, currentFamilies.hasIPv4, currentFamilies.hasIPv6)
			return true, nil
		}

		framework.Logf("Node %s breth0 IP families do not match yet - Current: IPv4=%v, IPv6=%v; Expected: IPv4=%v, IPv6=%v",
			nodeName, currentFamilies.hasIPv4, currentFamilies.hasIPv6, expectedFamilies.hasIPv4, expectedFamilies.hasIPv6)
		return false, nil
	})
}

// dumpContainerLogsFromNode dumps logs of all containers on the specified node using crictl
func dumpContainerLogsFromNode(nodeName string) {
	framework.Logf("Dumping container logs from node %s", nodeName)

	// First, get list of all containers
	containersOutput, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"crictl", "ps", "-a", "-o", "json"})
	if err != nil {
		framework.Logf("Failed to list containers on node %s: %v", nodeName, err)
		return
	}

	framework.Logf("Container list output from node %s:\n%s", nodeName, containersOutput)

	// Parse the JSON to get individual container IDs and names
	type Container struct {
		ID       string `json:"id"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	type ContainersList struct {
		Containers []Container `json:"containers"`
	}

	var containersList ContainersList
	if err := json.Unmarshal([]byte(containersOutput), &containersList); err != nil {
		framework.Logf("Failed to parse containers JSON from node %s: %v", nodeName, err)
		// Fallback: try to extract container IDs using crictl ps without JSON
		simpleOutput, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"crictl", "ps", "-a"})
		if err != nil {
			framework.Logf("Failed to list containers (simple format) on node %s: %v", nodeName, err)
			return
		}
		framework.Logf("Container list (simple format) from node %s:\n%s", nodeName, simpleOutput)
		return
	}

	// Dump logs for each container
	for _, container := range containersList.Containers {
		framework.Logf("Dumping logs for container %s (%s) on node %s", container.Metadata.Name, container.ID, nodeName)

		logs, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"crictl", "logs", "--tail=100", container.ID})
		if err != nil {
			framework.Logf("Failed to get logs for container %s (%s) on node %s: %v", container.Metadata.Name, container.ID, nodeName, err)
			continue
		}

		framework.Logf("=== Logs for container %s (%s) on node %s ===\n%s\n=== End logs ===",
			container.Metadata.Name, container.ID, nodeName, logs)
	}
}

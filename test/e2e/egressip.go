package e2e

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
)

const (
	OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME     = "OVN_EGRESSIP_HEALTHCHECK_PORT"
	DEFAULT_OVN_EGRESSIP_GRPC_HEALTHCHECK_PORT = "9107"
	OVN_EGRESSIP_LEGACY_HEALTHCHECK_PORT_ENV   = "0" // the env value to enable legacy health check
	OVN_EGRESSIP_LEGACY_HEALTHCHECK_PORT       = "9" // the actual port used by legacy health check
)

func labelNodeForEgress(f *framework.Framework, nodeName string) {
	framework.Logf("Labeling node %s with k8s.ovn.org/egress-assignable", nodeName)
	framework.AddOrUpdateLabelOnNode(f.ClientSet, nodeName, "k8s.ovn.org/egress-assignable", "dummy")
}

func unlabelNodeForEgress(f *framework.Framework, nodeName string) {
	framework.Logf("Removing label k8s.ovn.org/egress-assignable from node %s", nodeName)
	framework.RemoveLabelOffNode(f.ClientSet, nodeName, "k8s.ovn.org/egress-assignable")
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
	framework.Logf("Checking the ovnkube-node and ovnkube-master healthcheck ports in use")
	portNode := getTemplateContainerEnv(ovnNamespace, "daemonset/ovnkube-node", "ovnkube-node", OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME)
	portMaster := getTemplateContainerEnv(ovnNamespace, "deployment/ovnkube-master", "ovnkube-master", OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME)

	wantLegacy := (h.Legacy && !restore) || (h.modeWasLegacy && restore)
	isLegacy := portNode == "" || portNode == OVN_EGRESSIP_LEGACY_HEALTHCHECK_PORT_ENV
	outOfSync := portNode != portMaster

	if !h.modeWasChecked {
		h.modeWasChecked = true
		h.modeWasLegacy = isLegacy
		h.oldGRPCPort = portNode
	}

	if wantLegacy {
		// we want to change to legacy health check if we are not already in
		// that mode or if node and master are out of sync
		return OVN_EGRESSIP_LEGACY_HEALTHCHECK_PORT_ENV, OVN_EGRESSIP_LEGACY_HEALTHCHECK_PORT, !isLegacy || outOfSync
	}
	if !wantLegacy && !isLegacy {
		// we are is GRPC health check mode as we want but reset if node and
		// master are out of sync
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
		setEnv := map[string]string{OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME: portEnv}
		setUnsetTemplateContainerEnv(h.F.ClientSet, ovnNamespace, "daemonset/ovnkube-node", "ovnkube-node", setEnv)
		setUnsetTemplateContainerEnv(h.F.ClientSet, ovnNamespace, "deployment/ovnkube-master", "ovnkube-master", setEnv)
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

var _ = ginkgo.Describe("e2e egress IP validation", func() {
	const (
		servicePort          int32  = 9999
		podHTTPPort          string = "8080"
		egressIPName         string = "egressip"
		egressIPName2        string = "egressip-2"
		targetNodeName       string = "egressTargetNode-allowed"
		deniedTargetNodeName string = "egressTargetNode-denied"
		egressIPYaml         string = "egressip.yaml"
		egressFirewallYaml   string = "egressfirewall.yaml"
		waitInterval                = 3 * time.Second
		ciNetworkName               = "kind"
	)

	type node struct {
		name   string
		nodeIP string
	}

	podEgressLabel := map[string]string{
		"wants": "egress",
	}

	var (
		egress1Node, egress2Node, pod1Node, pod2Node, targetNode, deniedTargetNode node
		pod1Name                                                                   = "e2e-egressip-pod-1"
		pod2Name                                                                   = "e2e-egressip-pod-2"
		usedEgressNodeAvailabilityHandler                                          egressNodeAvailabilityHandler
		testsSkipped                                                               bool
	)

	targetPodAndTest := func(namespace, fromName, toName, toIP string) wait.ConditionFunc {
		return func() (bool, error) {
			stdout, err := framework.RunKubectl(namespace, "exec", fromName, "--", "curl", "--connect-timeout", "2", fmt.Sprintf("%s/hostname", net.JoinHostPort(toIP, podHTTPPort)))
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
				_, err := framework.RunKubectl(namespace, "exec", podName, "--", "curl", "--connect-timeout", "2", "-k", destination)
				if err != nil {
					framework.Logf("Error: attempted connection to API server found err:  %v", err)
					return false, nil
				}
			}
			return true, nil
		}
	}

	removeSliceElement := func(s []string, i int) []string {
		s[i] = s[len(s)-1]
		return s[:len(s)-1]
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
	targetExternalContainerAndTest := func(targetNode node, podName, podNamespace string, expectSuccess bool, verifyIPs []string) wait.ConditionFunc {
		return func() (bool, error) {
			_, err := framework.RunKubectl(podNamespace, "exec", podName, "--", "curl", "--connect-timeout", "2", net.JoinHostPort(targetNode.nodeIP, "80"))
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
			var targetNodeLogs string
			if strings.Contains(targetNode.name, "-host-net-pod") {
				// host-networked-pod
				targetNodeLogs, err = framework.RunKubectl(podNamespace, "logs", targetNode.name)
			} else {
				// external container
				targetNodeLogs, err = runCommand(containerRuntime, "logs", targetNode.name)
			}
			if err != nil {
				framework.Logf("failed to inspect logs in test container: %v", err)
				return false, nil
			}
			targetNodeLogs = strings.TrimSuffix(targetNodeLogs, "\n")
			logLines := strings.Split(targetNodeLogs, "\n")
			lastLine := logLines[len(logLines)-1]
			for i := 0; i < len(verifyIPs); i++ {
				if strings.Contains(lastLine, verifyIPs[i]) {
					verifyIPs = removeSliceElement(verifyIPs, i)
					break
				}
			}
			if len(verifyIPs) != 0 && expectSuccess {
				framework.Logf("the test external container did not have any trace of the IPs: %v being logged, last logs: %s", verifyIPs, logLines[len(logLines)-1])
				return false, nil
			}
			if !expectSuccess && len(verifyIPs) == 0 {
				framework.Logf("the test external container did have a trace of the IPs: %v being logged, it should not have, last logs: %s", verifyIPs, logLines[len(logLines)-1])
				return false, nil
			}
			return true, nil
		}
	}

	type egressIPStatus struct {
		Node     string `json:"node"`
		EgressIP string `json:"egressIP"`
	}

	type egressIP struct {
		Status struct {
			Items []egressIPStatus `json:"items"`
		} `json:"status"`
	}
	type egressIPs struct {
		Items []egressIP `json:"items"`
	}

	command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}

	dupIP := func(ip net.IP) net.IP {
		dup := make(net.IP, len(ip))
		copy(dup, ip)
		return dup
	}

	waitForStatus := func(node string, isReady bool) {
		wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			status := getNodeStatus(node)
			if isReady {
				return status == string(v1.ConditionTrue), nil
			}
			return status != string(v1.ConditionTrue), nil
		})
	}

	setNodeReady := func(node string, setReady bool) {
		if !setReady {
			_, err := runCommand("docker", "exec", node, "systemctl", "stop", "kubelet.service")
			if err != nil {
				framework.Failf("failed to stop kubelet on node: %s, err: %v", node, err)
			}
		} else {
			_, err := runCommand("docker", "exec", node, "systemctl", "start", "kubelet.service")
			if err != nil {
				framework.Failf("failed to start kubelet on node: %s, err: %v", node, err)
			}
		}
		waitForStatus(node, setReady)
	}

	setNodeReachable := func(node string, setReachable bool) {
		if !setReachable {
			_, err := runCommand("docker", "exec", node, "iptables", "-I", "INPUT", "-p", "tcp", "--dport", "9107", "-j", "DROP")
			if err != nil {
				framework.Failf("failed to block port 9107 on node: %s, err: %v", node, err)
			}
		} else {
			_, err := runCommand("docker", "exec", node, "iptables", "-I", "INPUT", "-p", "tcp", "--dport", "9107", "-j", "ACCEPT")
			if err != nil {
				framework.Failf("failed to allow port 9107 on node: %s, err: %v", node, err)
			}
		}
	}

	getEgressIPStatusItems := func() []egressIPStatus {
		egressIPs := egressIPs{}
		egressIPStdout, err := framework.RunKubectl("default", "get", "eip", "-o", "json")
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

	f := newPrivelegedTestFramework(egressIPName)

	// Determine what mode the CI is running in and get relevant endpoint information for the tests
	ginkgo.BeforeEach(func() {
		testsSkipped = true
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 3)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 3 {
			framework.Failf("Test requires >= 3 Ready nodes, but there are only %v nodes", len(nodes.Items))
		}

		multiZones, err := isMultipleZoneDeployment(f.ClientSet)
		if err != nil {
			framework.Failf("Failed to get the node zones : %v", err)
		}
		if multiZones {
			e2eskipper.Skipf(
				"Egress IPs are not yet supported with multiple zones deployment",
			)
		}
		testsSkipped = false
		ips := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)
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
		targetNode = node{
			name: targetNodeName,
		}
		deniedTargetNode = node{
			name: deniedTargetNodeName,
		}
		if utilnet.IsIPv6String(egress1Node.nodeIP) {
			_, targetNode.nodeIP = createClusterExternalContainer(targetNode.name, "docker.io/httpd", []string{"--network", ciNetworkName, "-P"}, []string{})
			_, deniedTargetNode.nodeIP = createClusterExternalContainer(deniedTargetNode.name, "docker.io/httpd", []string{"--network", ciNetworkName, "-P"}, []string{})
		} else {
			targetNode.nodeIP, _ = createClusterExternalContainer(targetNode.name, "docker.io/httpd", []string{"--network", ciNetworkName, "-P"}, []string{})
			deniedTargetNode.nodeIP, _ = createClusterExternalContainer(deniedTargetNode.name, "docker.io/httpd", []string{"--network", ciNetworkName, "-P"}, []string{})
		}
	})

	ginkgo.AfterEach(func() {
		if testsSkipped {
			return
		}
		framework.RunKubectlOrDie("default", "delete", "eip", egressIPName, "--ignore-not-found=true")
		framework.RunKubectlOrDie("default", "delete", "eip", egressIPName2, "--ignore-not-found=true")
		framework.RunKubectlOrDie("default", "label", "node", egress1Node.name, "k8s.ovn.org/egress-assignable-")
		framework.RunKubectlOrDie("default", "label", "node", egress2Node.name, "k8s.ovn.org/egress-assignable-")
		deleteClusterExternalContainer(targetNode.name)
		deleteClusterExternalContainer(deniedTargetNode.name)
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
	   3. Create two pods matching the EgressIP: one running on each of the egress nodes
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
	ginkgo.Describe("Using different methods to disable a node's availability for egress", func() {
		ginkgo.AfterEach(func() {
			usedEgressNodeAvailabilityHandler.Restore(egress1Node.name)
			usedEgressNodeAvailabilityHandler.Restore(egress2Node.name)
		})

		table.DescribeTable("Should validate the egress IP functionality against remote hosts",
			func(egressNodeAvailabilityHandler egressNodeAvailabilityHandler) {
				// set the egressNodeAvailabilityHandler that we are using so that
				// we can restore in AfterEach
				usedEgressNodeAvailabilityHandler = egressNodeAvailabilityHandler

				ginkgo.By("0. Setting two nodes as available for egress")
				usedEgressNodeAvailabilityHandler.Enable(egress1Node.name)
				usedEgressNodeAvailabilityHandler.Enable(egress2Node.name)

				podNamespace := f.Namespace
				podNamespace.Labels = map[string]string{
					"name": f.Namespace.Name,
				}
				updateNamespace(f, podNamespace)

				ginkgo.By("1. Create an EgressIP object with two egress IPs defined")
				// Assign the egress IP without conflicting with any node IP,
				// the kind subnet is /16 or /64 so the following should be fine.
				egressNodeIP := net.ParseIP(egress1Node.nodeIP)
				egressIP1 := dupIP(egressNodeIP)
				egressIP1[len(egressIP1)-2]++
				egressIP2 := dupIP(egressNodeIP)
				egressIP2[len(egressIP2)-2]++
				egressIP2[len(egressIP2)-1]++

				var egressIPConfig = fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)

				if err := ioutil.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
					framework.Failf("Unable to write CRD config to disk: %v", err)
				}
				defer func() {
					if err := os.Remove(egressIPYaml); err != nil {
						framework.Logf("Unable to remove the CRD config from disk: %v", err)
					}
				}()

				framework.Logf("Create the EgressIP configuration")
				framework.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

				ginkgo.By("2. Check that the status is of length two and both are assigned to different nodes")
				statuses := verifyEgressIPStatusLengthEquals(2, nil)
				if statuses[0].Node == statuses[1].Node {
					framework.Failf("Step 2. Check that the status is of length two and both are assigned to different nodess, failed, err: both egress IPs have been assigned to the same node")
				}

				ginkgo.By("3. Create two pods matching the EgressIP: one running on each of the egress nodes")
				command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}
				createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, command, podEgressLabel)
				createGenericPodWithLabel(f, pod2Name, pod2Node.name, f.Namespace.Name, command, podEgressLabel)

				err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
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

				pod2IP := getPodAddress(pod2Name, f.Namespace.Name)
				ginkgo.By("4. Check connectivity from both to an external \"node\" and verify that the IPs are both of the above")
				err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String(), egressIP2.String()}))
				framework.ExpectNoError(err, "Step 4. Check connectivity from first to an external \"node\" and verify that the IPs are both of the above, failed: %v", err)
				err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name, podNamespace.Name, true, []string{egressIP1.String(), egressIP2.String()}))
				framework.ExpectNoError(err, "Step 4. Check connectivity from second to an external \"node\" and verify that the IPs are both of the above, failed: %v", err)

				ginkgo.By("5. Check connectivity from one pod to the other and verify that the connection is achieved")
				err = wait.PollImmediate(retryInterval, retryTimeout, targetPodAndTest(f.Namespace.Name, pod1Name, pod2Name, pod2IP))
				framework.ExpectNoError(err, "Step 5. Check connectivity from one pod to the other and verify that the connection is achieved, failed, err: %v", err)

				ginkgo.By("6. Check connectivity from both pods to the api-server (running hostNetwork:true) and verifying that the connection is achieved")
				err = wait.PollImmediate(retryInterval, retryTimeout, targetDestinationAndTest(podNamespace.Name, fmt.Sprintf("https://%s/version", net.JoinHostPort(getApiAddress(), "443")), []string{pod1Name, pod2Name}))
				framework.ExpectNoError(err, "Step 6. Check connectivity from both pods to the api-server (running hostNetwork:true) and verifying that the connection is achieved, failed, err: %v", err)

				ginkgo.By("7. Update one of the pods, unmatching the EgressIP")
				pod2 := getPod(f, pod2Name)
				pod2.Labels = map[string]string{}
				updatePod(f, pod2)

				ginkgo.By("8. Check connectivity from that one to an external \"node\" and verify that the IP is the node IP.")
				err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name, podNamespace.Name, true, []string{pod2Node.nodeIP}))
				framework.ExpectNoError(err, "Step 8. Check connectivity from that one to an external \"node\" and verify that the IP is the node IP, failed, err: %v", err)

				ginkgo.By("9. Check connectivity from the other one to an external \"node\" and verify that the IPs are both of the above")
				err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String(), egressIP2.String()}))
				framework.ExpectNoError(err, "Step 9. Check connectivity from the other one to an external \"node\" and verify that the IP is one of the egress IPs, failed, err: %v", err)

				ginkgo.By("10. Setting one node as unavailable for egress")
				usedEgressNodeAvailabilityHandler.Disable(egress1Node.name)

				ginkgo.By("11. Check that the status is of length one")
				statuses = verifyEgressIPStatusLengthEquals(1, nil)

				ginkgo.By("12. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the remaining egress IP")
				err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{statuses[0].EgressIP}))
				framework.ExpectNoError(err, "Step 12. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the remaining egress IP, failed, err: %v", err)

				ginkgo.By("13. Setting the other node as unavailable for egress")
				usedEgressNodeAvailabilityHandler.Disable(egress2Node.name)

				ginkgo.By("14. Check that the status is of length zero")
				statuses = verifyEgressIPStatusLengthEquals(0, nil)

				ginkgo.By("15. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the node IP.")
				err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{pod1Node.nodeIP}))
				framework.ExpectNoError(err, "Step  15. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the node IP, failed, err: %v", err)

				ginkgo.By("16. Setting one node as available for egress")
				usedEgressNodeAvailabilityHandler.Enable(egress2Node.name)

				ginkgo.By("17. Check that the status is of length one")
				statuses = verifyEgressIPStatusLengthEquals(1, nil)

				ginkgo.By("18. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the remaining egress IP")
				err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{statuses[0].EgressIP}))
				framework.ExpectNoError(err, "Step 18. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the remaining egress IP, failed, err: %v", err)
			},
			table.Entry("disabling egress nodes with egress-assignable label", &egressNodeAvailabilityHandlerViaLabel{f}),
			table.Entry("disabling egress nodes impeding GRCP health check", &egressNodeAvailabilityHandlerViaHealthCheck{F: f, Legacy: false}),
			table.Entry("disabling egress nodes impeding Legacy health check", &egressNodeAvailabilityHandlerViaHealthCheck{F: f, Legacy: true}),
		)
	})

	// Validate the egress IP by creating a httpd container on the kind networking
	// (effectively seen as "outside" the cluster) and curl it from a pod in the cluster
	// which matches the egress IP stanza. Aim is to check if the SNATs towards nodeIP versus
	// SNATs towards egressIPs are being correctly deleted and recreated.

	/* This test does the following:
	   0. Add the "k8s.ovn.org/egress-assignable" label to egress1Node
	   1. Creating host-networked pod, on non-egress node (egress2Node) to act as "another node"
	   2. Create an EgressIP object with one egress IP defined
	   3. Check that the status is of length one and that it is assigned to egress1Node
	   4. Create one pod matching the EgressIP: running on egress1Node
	   5. Check connectivity from pod to an external "node" and verify that the srcIP is the expected egressIP
	   6. Check connectivity from pod to another node (egress2Node) and verify that the srcIP is the expected nodeIP
	   7. Add the "k8s.ovn.org/egress-assignable" label to egress2Node
	   8. Remove the "k8s.ovn.org/egress-assignable" label from egress1Node
	   9. Check that the status is of length one and that it is assigned to egress2Node
	   10. Check connectivity from pod to an external "node" and verify that the srcIP is the expected egressIP
	   11. Check connectivity from pod to another node (egress2Node) and verify that the srcIP is the expected nodeIP
	   12. Create second pod not matching the EgressIP: running on egress1Node
	   13. Check connectivity from second pod to external node and verify that the srcIP is the expected nodeIP
	   14. Add pod selector label to make second pod egressIP managed
	   15. Check connectivity from second pod to external node and verify that the srcIP is the expected egressIP
	   16. Check connectivity from second pod to another node (egress2Node) and verify that the srcIP is the expected nodeIP (this verifies SNAT's towards nodeIP are not deleted for pods unless pod is on its own egressNode)
	*/
	ginkgo.It("Should validate the egress IP SNAT functionality against host-networked pods", func() {

		command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}

		ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to egress1Node node")
		framework.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
		framework.Logf("Added egress-assignable label to node %s", egress1Node.name)
		framework.ExpectNodeHasLabel(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

		ginkgo.By("1. Creating host-networked pod, on non-egress node to act as \"another node\"")
		_, err := createPod(f, egress2Node.name+"-host-net-pod", egress2Node.name, f.Namespace.Name, []string{}, map[string]string{}, func(p *v1.Pod) {
			p.Spec.HostNetwork = true
			p.Spec.Containers[0].Image = "docker.io/httpd"
		})
		framework.ExpectNoError(err)
		hostNetPod := node{
			name:   egress2Node.name + "-host-net-pod",
			nodeIP: egress2Node.nodeIP,
		}
		framework.Logf("Created pod %s on node %s", hostNetPod.name, egress2Node.name)

		podNamespace := f.Namespace
		podNamespace.Labels = map[string]string{
			"name": f.Namespace.Name,
		}
		updateNamespace(f, podNamespace)

		ginkgo.By("2. Create an EgressIP object with one egress IP defined")
		// Assign the egress IP without conflicting with any node IP,
		// the kind subnet is /16 or /64 so the following should be fine.
		egressNodeIP := net.ParseIP(egress1Node.nodeIP)
		egressIP1 := dupIP(egressNodeIP)
		egressIP1[len(egressIP1)-2]++

		var egressIPConfig = fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)
		if err := ioutil.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(egressIPYaml); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()

		framework.Logf("Create the EgressIP configuration")
		framework.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

		ginkgo.By("3. Check that the status is of length one and that it is assigned to egress1Node")
		statuses := verifyEgressIPStatusLengthEquals(1, nil)
		if statuses[0].Node != egress1Node.name {
			framework.Failf("Step 2. Check that the status is of length one and that it is assigned to egress1Node, failed")
		}

		ginkgo.By("4. Create one pod matching the EgressIP: running on egress1Node")
		createGenericPodWithLabel(f, pod1Name, pod2Node.name, f.Namespace.Name, command, podEgressLabel)

		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut := getPodAddress(pod1Name, f.Namespace.Name)
			srcIP := net.ParseIP(kubectlOut)
			if srcIP == nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Step 4. Create one pod matching the EgressIP: running on egress1Node, failed, err: %v", err)
		framework.Logf("Created pod %s on node %s", pod1Name, pod2Node.name)

		ginkgo.By("5. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 5. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP, failed: %v", err)

		ginkgo.By("6. Check connectivity from pod to another node and verify that the srcIP is the expected nodeIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(hostNetPod, pod1Name, podNamespace.Name, true, []string{egressNodeIP.String()}))
		framework.ExpectNoError(err, "Step 6. Check connectivity from pod to another node and verify that the srcIP is the expected nodeIP, failed: %v", err)

		ginkgo.By("7. Add the \"k8s.ovn.org/egress-assignable\" label to egress2Node")
		framework.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")
		framework.Logf("Added egress-assignable label to node %s", egress2Node.name)
		framework.ExpectNodeHasLabel(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")

		ginkgo.By("8. Remove the \"k8s.ovn.org/egress-assignable\" label from egress1Node")
		framework.RemoveLabelOffNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable")

		ginkgo.By("9. Check that the status is of length one and that it is assigned to egress2Node")
		// There is sometimes a slight delay for the EIP fail over to happen,
		// so let's use the pollimmediate struct to check if eventually egress2Node becomes the egress node
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			statuses := getEgressIPStatusItems()
			return (len(statuses) == 1) && (statuses[0].Node == egress2Node.name), nil
		})
		framework.ExpectNoError(err, "Step 9. Check that the status is of length one and that it is assigned to egress2Node, failed: %v", err)

		ginkgo.By("10. Check connectivity from pod to an external \"node\" and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 10. Check connectivity from pod to an external \"node\" and verify that the srcIP is the expected egressIP, failed, err: %v", err)

		ginkgo.By("11. Check connectivity from pod to another node and verify that the srcIP is the expected nodeIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(hostNetPod, pod1Name, podNamespace.Name, true, []string{egressNodeIP.String()}))
		framework.ExpectNoError(err, "Step 11. Check connectivity from pod to another node and verify that the srcIP is the expected nodeIP, failed: %v", err)

		ginkgo.By("12. Create second pod not matching the EgressIP: running on egress1Node")
		createGenericPodWithLabel(f, pod2Name, pod2Node.name, f.Namespace.Name, command, map[string]string{})
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut := getPodAddress(pod2Name, f.Namespace.Name)
			srcIP := net.ParseIP(kubectlOut)
			if srcIP == nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Step 12. Create second pod not matching the EgressIP: running on egress1Node, failed, err: %v", err)
		framework.Logf("Created pod %s on node %s", pod2Name, pod2Node.name)

		ginkgo.By("13. Check connectivity from second pod to external node and verify that the srcIP is the expected nodeIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name, podNamespace.Name, true, []string{egressNodeIP.String()}))
		framework.ExpectNoError(err, "Step 13. Check connectivity from second pod to external node and verify that the srcIP is the expected nodeIP, failed: %v", err)

		ginkgo.By("14. Add pod selector label to make second pod egressIP managed")
		pod2 := getPod(f, pod2Name)
		pod2.Labels = podEgressLabel
		updatePod(f, pod2)

		ginkgo.By("15. Check connectivity from second pod to external node and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 15. Check connectivity from second pod to external node and verify that the srcIP is the expected egressIP, failed: %v", err)

		ginkgo.By("16. Check connectivity from second pod to another node and verify that the srcIP is the expected nodeIP (this verifies SNAT's towards nodeIP are not deleted unless node is egressNode)")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(hostNetPod, pod2Name, podNamespace.Name, true, []string{egressNodeIP.String()}))
		framework.ExpectNoError(err, "Step 16. Check connectivity from second pod to another node and verify that the srcIP is the expected nodeIP (this verifies SNAT's towards nodeIP are not deleted unless node is egressNode), failed: %v", err)
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

		command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}

		ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to egress1Node node")
		framework.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
		framework.Logf("Added egress-assignable label to node %s", egress1Node.name)
		framework.ExpectNodeHasLabel(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

		podNamespace := f.Namespace
		podNamespace.Labels = map[string]string{
			"name": f.Namespace.Name,
		}
		updateNamespace(f, podNamespace)

		ginkgo.By("1. Create an EgressIP object with one egress IP defined")
		// Assign the egress IP without conflicting with any node IP,
		// the kind subnet is /16 or /64 so the following should be fine.
		egressNodeIP := net.ParseIP(egress1Node.nodeIP)
		egressIP1 := dupIP(egressNodeIP)
		egressIP1[len(egressIP1)-2]++

		var egressIPConfig = fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)
		if err := ioutil.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(egressIPYaml); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()

		framework.Logf("Create the EgressIP configuration")
		framework.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

		ginkgo.By("2. Check that the status is of length one and that it is assigned to egress1Node")
		statuses := verifyEgressIPStatusLengthEquals(1, nil)
		if statuses[0].Node != egress1Node.name {
			framework.Failf("Step 2. Check that the status is of length one and that it is assigned to egress1Node, failed")
		}

		ginkgo.By("3. Create one pod matching the EgressIP: running on egress1Node")
		createGenericPodWithLabel(f, pod1Name, pod2Node.name, f.Namespace.Name, command, podEgressLabel)

		err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut := getPodAddress(pod1Name, f.Namespace.Name)
			srcIP := net.ParseIP(kubectlOut)
			if srcIP == nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Step 3. Create one pod matching the EgressIP: running on egress1Node, failed, err: %v", err)
		framework.Logf("Created pod %s on node %s", pod1Name, pod2Node.name)

		ginkgo.By("4. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 4. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP, failed: %v", err)

		for i := 0; i < 4; i++ {
			nodeSwapName := pod2Node.name // egressNode on odd runs
			if i%2 == 0 {
				nodeSwapName = pod1Node.name // non-egressNode on even runs
			}
			ginkgo.By("5. Delete the egressPod and recreate it immediately with the same name")
			_, err = framework.RunKubectl(f.Namespace.Name, "delete", "pod", pod1Name, "--grace-period=0", "--force")
			framework.ExpectNoError(err, "5. Run %d: Delete the egressPod and recreate it immediately with the same name, failed: %v", i, err)
			createGenericPodWithLabel(f, pod1Name, nodeSwapName, f.Namespace.Name, command, podEgressLabel)
			err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				kubectlOut := getPodAddress(pod1Name, f.Namespace.Name)
				srcIP := net.ParseIP(kubectlOut)
				if srcIP == nil {
					return false, nil
				}
				return true, nil
			})
			framework.ExpectNoError(err, "5. Run %d: Delete the egressPod and recreate it immediately with the same name, failed, err: %v", i, err)
			framework.Logf("Created pod %s on node %s", pod1Name, nodeSwapName)
			ginkgo.By("6. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP")
			err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
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

		command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}

		ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to egress1Node node")
		framework.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
		framework.Logf("Added egress-assignable label to node %s", egress1Node.name)
		framework.ExpectNodeHasLabel(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

		podNamespace := f.Namespace
		podNamespace.Labels = map[string]string{
			"name": f.Namespace.Name,
		}
		updateNamespace(f, podNamespace)

		ginkgo.By("1. Create one pod matching the EgressIP: running on node2 (pod2Node, egress1Node)")
		createGenericPodWithLabel(f, pod1Name, pod2Node.name, f.Namespace.Name, command, podEgressLabel)

		var srcPodIP net.IP
		err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut := getPodAddress(pod1Name, f.Namespace.Name)
			srcPodIP = net.ParseIP(kubectlOut)
			if srcPodIP == nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Step 1. Create one pod matching the EgressIP: running on node2 (pod2Node, egress1Node), failed, err: %v", err)
		framework.Logf("Created pod %s on node %s", pod1Name, pod2Node.name)

		ginkgo.By("2. Create an EgressIP object1 with two egress IP's - egressIP1 and egressIP2 defined")
		// Assign the egress IP without conflicting with any node IP,
		// the kind subnet is /16 or /64 so the following should be fine.
		egressNodeIP := net.ParseIP(egress1Node.nodeIP)
		egressIP1 := dupIP(egressNodeIP)
		egressIP1[len(egressIP1)-2]++
		egressIP2 := dupIP(egressNodeIP)
		egressIP2[len(egressIP2)-2]++
		egressIP2[len(egressIP2)-1]++

		var egressIPConfig = fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)
		if err := ioutil.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(egressIPYaml); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()

		framework.Logf("Create the EgressIP configuration")
		framework.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

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
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{assignedEIP}))
		framework.ExpectNoError(err, "Step 4. Check connectivity from pod to an zexternal container and verify that the srcIP is the expected egressIP from object1, failed: %v", err)

		ginkgo.By("5. Create an EgressIP object2 with one egress IP3 defined (standby egressIP)")
		// Assign the egress IP without conflicting with any node IP,
		// the kind subnet is /16 or /64 so the following should be fine.
		egressIP3 := dupIP(egressNodeIP)
		egressIP3[len(egressIP3)-2]++
		egressIP3[len(egressIP3)-1]++
		egressIP3[len(egressIP3)-1]++

		var egressIPConfig2 = fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)
		if err := ioutil.WriteFile(egressIPYaml, []byte(egressIPConfig2), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(egressIPYaml); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()

		framework.Logf("Create the EgressIP configuration")
		framework.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

		ginkgo.By("6. Check that the second egressIP object is assigned to node2 (pod2Node/egress1Node)")
		egressIPs := egressIPs{}
		var egressIPStdout string
		var statusEIP1, statusEIP2 []egressIPStatus
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			egressIPStdout, err = framework.RunKubectl("default", "get", "eip", "-o", "json")
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
		dbPods, err := framework.RunKubectl("ovn-kubernetes", "get", "pods", "-l", "name=ovnkube-db", "-o=jsonpath='{.items..metadata.name}'")
		if err != nil || len(dbPods) == 0 {
			framework.Failf("Error: Check the OVN DB to ensure no SNATs are added for the standby egressIP, err: %v", err)
		}
		dbPod := strings.Split(dbPods, " ")[0]
		dbPod = strings.TrimPrefix(dbPod, "'")
		dbPod = strings.TrimSuffix(dbPod, "'")
		logicalIP := fmt.Sprintf("logical_ip=%s", srcPodIP.String())
		snats, err := framework.RunKubectl("ovn-kubernetes", "exec", dbPod, "-c", "nb-ovsdb", "--", "ovn-nbctl", "--no-leader-only", "--columns=external_ip", "find", "nat", logicalIP)
		if err != nil {
			framework.Failf("Error: Check the OVN DB to ensure no SNATs are added for the standby egressIP, err: %v", err)
		}
		if !strings.Contains(snats, statuses[0].EgressIP) || strings.Contains(snats, egressIP3.String()) {
			framework.Failf("Step 7. Check that the second egressIP object is assigned to node2 (pod2Node/egress1Node), failed")
		}

		ginkgo.By("8. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object1")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{assignedEIP}))
		framework.ExpectNoError(err, "Step 8. Check connectivity from pod to an external container and verify that the srcIP is the expected egressIP from object1, failed: %v", err)

		ginkgo.By("9. Delete assigned egressIP1 from egressIP object1")
		egressIPConfig = fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)
		if err := ioutil.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(egressIPYaml); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()

		framework.Logf("Apply the EgressIP configuration")
		framework.RunKubectlOrDie("default", "apply", "-f", egressIPYaml)

		ginkgo.By("10. Check that the status is of length one and that standby egressIP3 of egressIP object2 is assigned to node2 (pod2Node/egress1Node)")

		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			egressIPStdout, err = framework.RunKubectl("default", "get", "eip", "-o", "json")
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
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP3.String()}))
		framework.ExpectNoError(err, "Step 11. Check connectivity from pod to an external container and verify that the srcIP is the expected standby egressIP3 from object2, failed: %v", err)

		ginkgo.By("12. Check the OVN DB to ensure SNATs are added for only the standby egressIP")
		snats, err = framework.RunKubectl("ovn-kubernetes", "exec", dbPod, "-c", "nb-ovsdb", "--", "ovn-nbctl", "--no-leader-only", "--columns=external_ip", "find", "nat", logicalIP)
		if err != nil {
			framework.Failf("Error: Check the OVN DB to ensure SNATs are added for only the standby egressIP, err: %v", err)
		}
		if !strings.Contains(snats, egressIP3.String()) || strings.Contains(snats, egressIP1.String()) || strings.Contains(snats, egressIP2.String()) || strings.Contains(snats, egress1Node.nodeIP) {
			framework.Failf("Step 12. Check the OVN DB to ensure SNATs are added for only the standby egressIP, failed")
		}

		ginkgo.By("13. Mark egress2Node as assignable and egress1Node as unassignable")
		framework.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")
		framework.Logf("Added egress-assignable label to node %s", egress2Node.name)
		framework.ExpectNodeHasLabel(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")
		framework.RemoveLabelOffNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable")
		framework.Logf("Removed egress-assignable label from node %s", egress1Node.name)

		ginkgo.By("14. Ensure egressIP1 from egressIP object1 and egressIP3 from object2 is correctly transferred to egress2Node")
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			egressIPStdout, err = framework.RunKubectl("default", "get", "eip", "-o", "json")
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

		ginkgo.By("15. Check the OVN DB to ensure SNATs are added for either egressIP1 or egressIP3")
		snats, err = framework.RunKubectl("ovn-kubernetes", "exec", dbPod, "-c", "nb-ovsdb", "--", "ovn-nbctl", "--no-leader-only", "--columns=external_ip", "find", "nat", logicalIP)
		if err != nil {
			framework.Failf("Error: Check the OVN DB to ensure SNATs are added for either egressIP1 or egressIP3, err: %v", err)
		}
		if !(strings.Contains(snats, egressIP3.String()) || strings.Contains(snats, toKeepEIP)) {
			framework.Failf("Step 15. Check the OVN DB to ensure SNATs are added for either egressIP1 or egressIP3, failed")
		}
		var toDelete, unassignedEIP string
		if strings.Contains(snats, egressIP3.String()) {
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
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{assignedEIP}))
		framework.ExpectNoError(err, "Step 16. Check connectivity from pod to an external container and verify that the srcIP is either egressIP1 or egressIP3, failed: %v", err)

		ginkgo.By("17. Delete EgressIP object that was serving the pod before in Step 16")
		framework.RunKubectlOrDie("default", "delete", "eip", toDelete)

		ginkgo.By("18.  Check connectivity from pod to an external container and verify that the srcIP is the expected standby egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{unassignedEIP}))
		framework.ExpectNoError(err, "Step 18.  Check connectivity from pod to an external container and verify that the srcIP is the expected standby egressIP, failed: %v", err)

		ginkgo.By("19. Delete the remaining egressIP object")
		framework.RunKubectlOrDie("default", "delete", "eip", toKeepEIP)

		ginkgo.By("20. Check connectivity from pod to an external container and verify that the srcIP is the expected nodeIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{pod2Node.nodeIP}))
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
		framework.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
		framework.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")

		ginkgo.By("1. Create an EgressIP object with one egress IP defined")
		// Assign the egress IP without conflicting with any node IP,
		// the kind subnet is /16 or /64 so the following should be fine.
		egressNodeIP := net.ParseIP(egress1Node.nodeIP)
		egressIP1 := dupIP(egressNodeIP)
		egressIP1[len(egressIP1)-2]++

		podNamespace := f.Namespace
		podNamespace.Labels = map[string]string{
			"name": f.Namespace.Name,
		}
		updateNamespace(f, podNamespace)

		var egressIPConfig = fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)
		if err := ioutil.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(egressIPYaml); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()

		framework.Logf("Applying the EgressIP configuration")
		framework.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

		ginkgo.By("2. Check that the status is of length one")
		statuses := verifyEgressIPStatusLengthEquals(1, nil)
		node1 := statuses[0].Node

		ginkgo.By("3. Create one pod matching the EgressIP")
		createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, command, podEgressLabel)

		ginkgo.By("4. Make egress node 1 unreachable")
		setNodeReachable(node1, false)

		ginkgo.By("5. Check that egress IP has been moved to other node 2 with the \"k8s.ovn.org/egress-assignable\" label")
		var node2 string
		statuses = verifyEgressIPStatusLengthEquals(1, func(statuses []egressIPStatus) bool {
			node2 = statuses[0].Node
			return node2 != node1
		})

		ginkgo.By("6. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP")
		err := wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "6. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP, failed, err: %v", err)

		ginkgo.By("7. Check connectivity from pod to the api-server (running hostNetwork:true) and verifying that the connection is achieved")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetDestinationAndTest(podNamespace.Name, fmt.Sprintf("https://%s/version", net.JoinHostPort(getApiAddress(), "443")), []string{pod1Name}))
		framework.ExpectNoError(err, "7. Check connectivity from pod to the api-server (running hostNetwork:true) and verifying that the connection is achieved, failed, err: %v", err)

		ginkgo.By("8, Make node 2 unreachable")
		setNodeReachable(node2, false)

		ginkgo.By("9. Check that egress IP is un-assigned (empty status)")
		verifyEgressIPStatusLengthEquals(0, nil)

		ginkgo.By("10. Check connectivity from pod to an external \"node\" and verify that the IP is the node IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{pod1Node.nodeIP}))
		framework.ExpectNoError(err, "10. Check connectivity from pod to an external \"node\" and verify that the IP is the node IP, failed, err: %v", err)

		ginkgo.By("11. Make node 1 reachable again")
		setNodeReachable(node1, true)

		ginkgo.By("12. Check that egress IP is assigned to node 1 again")
		statuses = verifyEgressIPStatusLengthEquals(1, func(statuses []egressIPStatus) bool {
			testNode := statuses[0].Node
			return testNode == node1
		})

		ginkgo.By("13. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "13. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP, failed, err: %v", err)

		ginkgo.By("14. Make node 2 reachable again")
		setNodeReachable(node2, true)

		ginkgo.By("15. Check that egress IP remains assigned to node 1. We should not be moving the egress IP to node 2 if the node 1 works fine, as to reduce cluster entropy - read: changes.")
		statuses = verifyEgressIPStatusLengthEquals(1, func(statuses []egressIPStatus) bool {
			testNode := statuses[0].Node
			return testNode == node1
		})

		ginkgo.By("17. Make node 1 NotReady")
		setNodeReady(node1, false)

		ginkgo.By("18. Check that egress IP is assigned to node 2")
		statuses = verifyEgressIPStatusLengthEquals(1, func(statuses []egressIPStatus) bool {
			testNode := statuses[0].Node
			return testNode == node2
		})

		ginkgo.By("19. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "19. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP, failed, err: %v", err)

		ginkgo.By("20. Make node 1 not reachable")
		setNodeReachable(node1, false)

		ginkgo.By("21. Unlabel node 2")
		framework.RemoveLabelOffNode(f.ClientSet, node2, "k8s.ovn.org/egress-assignable")

		ginkgo.By("22. Check that egress IP is un-assigned (since node 1 is both unreachable and NotReady)")
		verifyEgressIPStatusLengthEquals(0, nil)

		ginkgo.By("23. Make node 1 Ready")
		setNodeReady(node1, true)

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
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "27. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP, failed, err: %v", err)
	})

	// Validate the egress IP works with egress firewall by creating two httpd
	// containers on the kind networking (effectively seen as "outside" the cluster)
	// and curl them from a pod in the cluster which matches the egress IP stanza.
	// The IP allowed by the egress firewall rule should work, the other not.

	/* This test does the following:
	   0. Add the "k8s.ovn.org/egress-assignable" label to one nodes
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

		command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}

		ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to one nodes")
		framework.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

		podNamespace := f.Namespace
		podNamespace.Labels = map[string]string{
			"name": f.Namespace.Name,
		}
		updateNamespace(f, podNamespace)

		ginkgo.By("1. Create an EgressIP object with one egress IP defined")
		// Assign the egress IP without conflicting with any node IP,
		// the kind subnet is /16 or /64 so the following should be fine.
		egressNodeIP := net.ParseIP(egress1Node.nodeIP)
		egressIP := dupIP(egressNodeIP)
		egressIP[len(egressIP)-2]++

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

		if err := ioutil.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}

		defer func() {
			if err := os.Remove(egressIPYaml); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()

		framework.Logf("Create the EgressIP configuration")
		framework.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

		ginkgo.By("2. Create an EgressFirewall object with one allow rule and one \"block-all\" rule defined")

		var firewallAllowNode, firewallDenyAll string
		if utilnet.IsIPv6String(targetNode.nodeIP) {
			firewallAllowNode = targetNode.nodeIP + "/128"
			firewallDenyAll = "::/0"
		} else {
			firewallAllowNode = targetNode.nodeIP + "/32"
			firewallDenyAll = "0.0.0.0/0"
		}

		var egressFirewallConfig = fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)

		if err := ioutil.WriteFile(egressFirewallYaml, []byte(egressFirewallConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}

		defer func() {
			if err := os.Remove(egressFirewallYaml); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()

		framework.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressFirewallYaml)

		ginkgo.By("3. Create two pods, and matching service, matching both egress firewall and egress IP")
		createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, command, podEgressLabel)
		createGenericPodWithLabel(f, pod2Name, pod2Node.name, f.Namespace.Name, command, podEgressLabel)
		serviceIP, err := createServiceForPodsWithLabel(f, f.Namespace.Name, servicePort, podHTTPPort, "ClusterIP", podEgressLabel)
		framework.ExpectNoError(err, "Step 3. Create two pods, and matching service, matching both egress firewall and egress IP, failed creating service, err: %v", err)

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
		framework.ExpectNoError(err, "Step 3. Create two pods matching both egress firewall and egress IP, failed, err: %v", err)

		pod2IP := getPodAddress(pod2Name, f.Namespace.Name)

		ginkgo.By("Checking that the status is of length one")
		verifyEgressIPStatusLengthEquals(1, nil)

		ginkgo.By("4. Check connectivity to the blocked IP and verify that it fails")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(deniedTargetNode, pod1Name, podNamespace.Name, false, []string{egressIP.String()}))
		framework.ExpectNoError(err, "Step:  4. Check connectivity to the blocked IP and verify that it fails, failed, err: %v", err)

		ginkgo.By("5. Check connectivity to the allowed IP and verify it has the egress IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP.String()}))
		framework.ExpectNoError(err, "Step: 5. Check connectivity to the allowed IP and verify it has the egress IP, failed, err: %v", err)

		// TODO: in the future once we only have shared gateway mode: implement egress firewall so that
		// pods that have a "deny all 0.0.0.0/0" rule, still can connect to the Kubernetes API service
		// and re-enable this check

		// ginkgo.By("6. Check connectivity to the kubernetes API IP and verify that it works")
		// err = wait.PollImmediate(retryInterval, retryTimeout, targetAPIServiceAndTest(podNamespace.Name, []string{pod1Name, pod2Name}))
		// framework.ExpectNoError(err, "Step 6. Check connectivity to the kubernetes API IP and verify that it works, failed, err %v", err)

		ginkgo.By("7. Check connectivity to the other pod IP and verify that it works")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetPodAndTest(f.Namespace.Name, pod1Name, pod2Name, pod2IP))
		framework.ExpectNoError(err, "Step 7. Check connectivity to the other pod IP and verify that it works, err: %v", err)

		ginkgo.By("8. Check connectivity to the service IP and verify that it works")
		servicePortAsString := strconv.Itoa(int(servicePort))
		err = wait.PollImmediate(retryInterval, retryTimeout, targetDestinationAndTest(podNamespace.Name, fmt.Sprintf("http://%s/hostname", net.JoinHostPort(serviceIP, servicePortAsString)), []string{pod1Name, pod2Name}))
		framework.ExpectNoError(err, "8. Check connectivity to the service IP and verify that it works, failed, err %v", err)
	})
})

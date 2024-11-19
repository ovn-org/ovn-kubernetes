package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/dsl/table"
	"github.com/onsi/gomega"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
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
	primaryNetworkName                         = "kind"
	secondaryIPV4Subnet                        = "10.10.10.0/24"
	secondaryIPV6Subnet                        = "2001:db8:abcd:1234::/64"
	secondaryNetworkName                       = "secondary-network"
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
	framework.Logf("Checking the ovnkube-node and ovnkube-master (ovnkube-cluster-manager if interconnect=true) healthcheck ports in use")
	portNode := getTemplateContainerEnv(ovnNamespace, "daemonset/ovnkube-node", getNodeContainerName(), OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME)
	var portMaster string
	if isInterconnectEnabled() {
		portMaster = getTemplateContainerEnv(ovnNamespace, "deployment/ovnkube-control-plane", "ovnkube-cluster-manager", OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME)
	} else {
		portMaster = getTemplateContainerEnv(ovnNamespace, "deployment/ovnkube-master", "ovnkube-master", OVN_EGRESSIP_HEALTHCHECK_PORT_ENV_NAME)
	}

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
		setUnsetTemplateContainerEnv(h.F.ClientSet, ovnNamespace, "daemonset/ovnkube-node", getNodeContainerName(), setEnv)
		if isInterconnectEnabled() {
			setUnsetTemplateContainerEnv(h.F.ClientSet, ovnNamespace, "deployment/ovnkube-control-plane", "ovnkube-cluster-manager", setEnv)
		} else {
			setUnsetTemplateContainerEnv(h.F.ClientSet, ovnNamespace, "deployment/ovnkube-master", "ovnkube-master", setEnv)
		}
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

type node struct {
	name   string
	nodeIP string
}

func configNetworkAndGetTarget(subnet string, nodesToAttachNet []string, v6 bool, targetSecondaryNode node) (string, string) {
	// configure and add additional network to worker containers for EIP multi NIC feature
	createNetwork(secondaryNetworkName, subnet, v6)
	if v6 {
		// HACK: ensure bridges don't talk to each other. For IPv6, docker support for isolated networks is experimental.
		// Remove when it is no longer experimental. See func description for full details.
		if err := isolateIPv6Networks(primaryNetworkName, secondaryNetworkName); err != nil {
			framework.Failf("failed to isolate IPv6 networks: %v", err)
		}
	}
	for _, nodeName := range nodesToAttachNet {
		attachNetwork(secondaryNetworkName, nodeName)
	}
	v4Addr, v6Addr := createClusterExternalContainer(targetSecondaryNode.name, "docker.io/httpd", []string{"--network", secondaryNetworkName, "-P"}, []string{})
	if v4Addr == "" && !v6 {
		panic("failed to get v4 address")
	}
	if v6Addr == "" && v6 {
		panic("failed to get v6 address")
	}
	return v4Addr, v6Addr
}

func tearDownNetworkAndTargetForMultiNIC(nodeToDetachNet []string, targetSecondaryNode node) {
	deleteClusterExternalContainer(targetSecondaryNode.name)
	for _, nodeName := range nodeToDetachNet {
		detachNetwork(secondaryNetworkName, nodeName)
	}
	deleteNetwork(secondaryNetworkName)
}

func removeSliceElement(s []string, i int) []string {
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
func targetExternalContainerAndTest(targetNode node, podName, podNamespace string, expectSuccess bool, verifyIPs []string) wait.ConditionFunc {
	return func() (bool, error) {
		_, err := e2ekubectl.RunKubectl(podNamespace, "exec", podName, "--", "curl", "--connect-timeout", "2", net.JoinHostPort(targetNode.nodeIP, "80"))
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
			targetNodeLogs, err = e2ekubectl.RunKubectl(podNamespace, "logs", targetNode.name)
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

var _ = ginkgo.DescribeTableSubtree("e2e egress IP validation", func(netConfigParams networkAttachmentConfigParams, isHostNetwork bool) {
	const (
		servicePort             int32  = 9999
		echoServerPodPortMin           = 9900
		echoServerPodPortMax           = 9999
		podHTTPPort             string = "8080"
		egressIPName            string = "egressip"
		egressIPName2           string = "egressip-2"
		targetNodeName          string = "egressTargetNode-allowed"
		deniedTargetNodeName    string = "egressTargetNode-denied"
		targetSecondaryNodeName string = "egressSecondaryTargetNode-allowed"
		egressIPYaml            string = "egressip.yaml"
		egressFirewallYaml      string = "egressfirewall.yaml"
		ciNetworkName                  = "kind"
		retryTimeout                   = 3 * retryTimeout // Boost the retryTimeout for EgressIP tests.
	)

	podEgressLabel := map[string]string{
		"wants": "egress",
	}

	var (
		egress1Node, egress2Node, pod1Node, pod2Node, targetNode, deniedTargetNode, targetSecondaryNode node
		pod1Name                                                                                        = "e2e-egressip-pod-1"
		pod2Name                                                                                        = "e2e-egressip-pod-2"
		usedEgressNodeAvailabilityHandler                                                               egressNodeAvailabilityHandler
	)

	targetPodAndTest := func(namespace, fromName, toName, toIP string) wait.ConditionFunc {
		return func() (bool, error) {
			stdout, err := e2ekubectl.RunKubectl(namespace, "exec", fromName, "--", "curl", "--connect-timeout", "2", fmt.Sprintf("%s/hostname", net.JoinHostPort(toIP, podHTTPPort)))
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
					framework.Logf("Error: attempted connection to API server found err:  %v", err)
					return false, nil
				}
			}
			return true, nil
		}
	}

	command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}

	dupIP := func(ip net.IP) net.IP {
		dup := make(net.IP, len(ip))
		copy(dup, ip)
		return dup
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

	setNodeReachable := func(iptablesCmd, node string, setReachable bool) {
		if !setReachable {
			_, err := runCommand("docker", "exec", node, iptablesCmd, "-I", "INPUT", "-p", "tcp", "--dport", "9107", "-j", "DROP")
			if err != nil {
				framework.Failf("failed to block port 9107 on node: %s, err: %v", node, err)
			}
		} else {
			_, err := runCommand("docker", "exec", node, iptablesCmd, "-I", "INPUT", "-p", "tcp", "--dport", "9107", "-j", "ACCEPT")
			if err != nil {
				framework.Failf("failed to allow port 9107 on node: %s, err: %v", node, err)
			}
		}
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

	f := wrappedTestFramework(egressIPName)

	// Determine what mode the CI is running in and get relevant endpoint information for the tests
	ginkgo.BeforeEach(func() {
		if !isNetworkSegmentationEnabled() && netConfigParams.networkName != types.DefaultNetworkName {
			ginkgo.Skip("Network segmentation is disabled")
		}
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 3 {
			framework.Failf("Test requires >= 3 Ready nodes, but there are only %v nodes", len(nodes.Items))
		}
		ips := e2enode.CollectAddresses(nodes, corev1.NodeInternalIP)
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
		targetSecondaryNode = node{
			name: targetSecondaryNodeName,
		}
		isV6 := utilnet.IsIPv6String(egress1Node.nodeIP)
		if isV6 {
			_, targetNode.nodeIP = createClusterExternalContainer(targetNode.name, "docker.io/httpd", []string{"--network", ciNetworkName, "-P"}, []string{})
			_, deniedTargetNode.nodeIP = createClusterExternalContainer(deniedTargetNode.name, "docker.io/httpd", []string{"--network", ciNetworkName, "-P"}, []string{})
			// configure and add additional network to worker containers for EIP multi NIC feature
			_, targetSecondaryNode.nodeIP = configNetworkAndGetTarget(secondaryIPV6Subnet, []string{egress1Node.name, egress2Node.name}, isV6, targetSecondaryNode)
		} else {
			targetNode.nodeIP, _ = createClusterExternalContainer(targetNode.name, "docker.io/httpd", []string{"--network", ciNetworkName, "-P"}, []string{})
			deniedTargetNode.nodeIP, _ = createClusterExternalContainer(deniedTargetNode.name, "docker.io/httpd", []string{"--network", ciNetworkName, "-P"}, []string{})
			// configure and add additional network to worker containers for EIP multi NIC feature
			targetSecondaryNode.nodeIP, _ = configNetworkAndGetTarget(secondaryIPV4Subnet, []string{egress1Node.name, egress2Node.name}, isV6, targetSecondaryNode)
		}

		// ensure all nodes are ready and reachable
		for _, node := range nodes.Items {
			setNodeReady(node.Name, true)
			setNodeReachable("iptables", node.Name, true)
			if IsIPv6Cluster(f.ClientSet) {
				setNodeReachable("ip6tables", node.Name, true)
			}
			waitForNoTaint(node.Name, "node.kubernetes.io/unreachable")
			waitForNoTaint(node.Name, "node.kubernetes.io/not-ready")
		}
		// no further network creation is required if CDN
		if netConfigParams.networkName == types.DefaultNetworkName {
			return
		}
		// configure UDN
		nadClient, err := nadclient.NewForConfig(f.ClientConfig())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		netConfig := newNetworkAttachmentConfig(netConfigParams)
		netConfig.namespace = f.Namespace.Name
		_, err = nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
			context.Background(),
			generateNAD(netConfig),
			metav1.CreateOptions{},
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.AfterEach(func() {
		if !isNetworkSegmentationEnabled() && netConfigParams.networkName != types.DefaultNetworkName {
			ginkgo.Skip("Network segmentation is disabled")
		}
		e2ekubectl.RunKubectlOrDie("default", "delete", "eip", egressIPName, "--ignore-not-found=true")
		e2ekubectl.RunKubectlOrDie("default", "delete", "eip", egressIPName2, "--ignore-not-found=true")
		e2ekubectl.RunKubectlOrDie("default", "label", "node", egress1Node.name, "k8s.ovn.org/egress-assignable-")
		e2ekubectl.RunKubectlOrDie("default", "label", "node", egress2Node.name, "k8s.ovn.org/egress-assignable-")
		deleteClusterExternalContainer(targetNode.name)
		deleteClusterExternalContainer(deniedTargetNode.name)
		tearDownNetworkAndTargetForMultiNIC([]string{egress1Node.name, egress2Node.name}, targetSecondaryNode)
		// ensure all nodes are ready and reachable
		for _, node := range []string{egress1Node.name, egress2Node.name} {
			setNodeReady(node, true)
			setNodeReachable("iptables", node, true)
			if IsIPv6Cluster(f.ClientSet) {
				setNodeReachable("ip6tables", node, true)
			}
			waitForNoTaint(node, "node.kubernetes.io/unreachable")
			waitForNoTaint(node, "node.kubernetes.io/not-ready")
		}
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
				var pod2IP string
				if netConfigParams.networkName == types.DefaultNetworkName {
					pod2IP = getPodAddress(pod2Name, f.Namespace.Name)
				} else {
					pod2IP, err = podIPsForUserDefinedPrimaryNetwork(
						f.ClientSet,
						f.Namespace.Name,
						pod2Name,
						namespacedName(f.Namespace.Name, netConfigParams.name),
						0,
					)
					framework.ExpectNoError(err, "Step 3. Create two UDN pods matching the EgressIP: one running on each of the egress nodes, failed, err: %v", err)
				}

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

		command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}

		ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to egress1Node node")
		e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
		framework.Logf("Added egress-assignable label to node %s", egress1Node.name)
		e2enode.ExpectNodeHasLabel(context.TODO(), f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

		ginkgo.By("1. By setting a secondary IP on non-egress node acting as \"another node\"")
		var otherDstIP string
		if utilnet.IsIPv6String(egress2Node.nodeIP) {
			otherDstIP = "fc00:f853:ccd:e793:ffff::1"
		} else {
			// TODO(mk): replace with non-repeating IP allocator
			otherDstIP = "172.18.1.99"
		}
		_, err := runCommand(containerRuntime, "exec", egress2Node.name, "ip", "addr", "add", otherDstIP, "dev", "breth0")
		if err != nil {
			framework.Failf("failed to add address to node %s: %v", egress2Node.name, err)
		}
		defer func() {
			_, err = runCommand(containerRuntime, "exec", egress2Node.name, "ip", "addr", "delete", otherDstIP, "dev", "breth0")
			if err != nil {
				framework.Failf("failed to remove address from node %s: %v", egress2Node.name, err)
			}
		}()
		otherHostNetPodIP := node{
			name:   egress2Node.name + "-host-net-pod",
			nodeIP: otherDstIP,
		}

		ginkgo.By("2. Creating host-networked pod, on non-egress node acting as \"another node\"")
		_, err = createPod(f, egress2Node.name+"-host-net-pod", egress2Node.name, f.Namespace.Name, []string{}, map[string]string{}, func(p *corev1.Pod) {
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

		ginkgo.By("3. Create an EgressIP object with one egress IP defined")
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
		createGenericPodWithLabel(f, pod1Name, pod2Node.name, f.Namespace.Name, command, podEgressLabel)

		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut := getPodAddress(pod1Name, f.Namespace.Name)
			srcIP := net.ParseIP(kubectlOut)
			if srcIP == nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Step 5. Create one pod matching the EgressIP: running on egress1Node, failed, err: %v", err)
		framework.Logf("Created pod %s on node %s", pod1Name, pod2Node.name)

		ginkgo.By("6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP, failed: %v", err)

		ginkgo.By("7. Check connectivity from pod to another node primary IP and verify that the srcIP is the expected nodeIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(hostNetPod, pod1Name, podNamespace.Name, true, []string{egressNodeIP.String()}))
		framework.ExpectNoError(err, "Step 7. Check connectivity from pod to another node primary IP and verify that the srcIP is the expected nodeIP, failed: %v", err)

		ginkgo.By("8. Check connectivity from pod to another node secondary IP and verify that the srcIP is the expected nodeIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(otherHostNetPodIP, pod1Name, podNamespace.Name, true, []string{egressNodeIP.String()}))
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
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 12. Check connectivity from pod to an external \"node\" and verify that the srcIP is the expected egressIP, failed, err: %v", err)

		ginkgo.By("13. Check connectivity from pod to another node primary IP and verify that the srcIP is the expected nodeIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(hostNetPod, pod1Name, podNamespace.Name, true, []string{egressNodeIP.String()}))
		framework.ExpectNoError(err, "Step 13. Check connectivity from pod to another node and verify that the srcIP is the expected nodeIP, failed: %v", err)

		ginkgo.By("14. Check connectivity from pod to another node secondary IP and verify that the srcIP is the expected nodeIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(otherHostNetPodIP, pod1Name, podNamespace.Name, true, []string{egressNodeIP.String()}))
		framework.ExpectNoError(err, "Step 14. Check connectivity from pod to another node secondary IP and verify that the srcIP is the expected nodeIP, failed: %v", err)

		ginkgo.By("15. Create second pod not matching the EgressIP: running on egress1Node")
		createGenericPodWithLabel(f, pod2Name, pod2Node.name, f.Namespace.Name, command, map[string]string{})
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut := getPodAddress(pod2Name, f.Namespace.Name)
			srcIP := net.ParseIP(kubectlOut)
			if srcIP == nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Step 15. Create second pod not matching the EgressIP: running on egress1Node, failed, err: %v", err)
		framework.Logf("Created pod %s on node %s", pod2Name, pod2Node.name)

		ginkgo.By("16. Check connectivity from second pod to external node and verify that the srcIP is the expected nodeIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name, podNamespace.Name, true, []string{egressNodeIP.String()}))
		framework.ExpectNoError(err, "Step 16. Check connectivity from second pod to external node and verify that the srcIP is the expected nodeIP, failed: %v", err)

		ginkgo.By("17. Add pod selector label to make second pod egressIP managed")
		pod2 := getPod(f, pod2Name)
		pod2.Labels = podEgressLabel
		updatePod(f, pod2)

		ginkgo.By("18. Check connectivity from second pod to external node and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 18. Check connectivity from second pod to external node and verify that the srcIP is the expected egressIP, failed: %v", err)

		ginkgo.By("19. Check connectivity from second pod to another node primary IP and verify that the srcIP is the expected nodeIP (this verifies SNAT's towards nodeIP are not deleted unless node is egressNode)")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(hostNetPod, pod2Name, podNamespace.Name, true, []string{egressNodeIP.String()}))
		framework.ExpectNoError(err, "Step 19. Check connectivity from second pod to another node and verify that the srcIP is the expected nodeIP (this verifies SNAT's towards nodeIP are not deleted unless node is egressNode), failed: %v", err)

		ginkgo.By("20. Check connectivity from second pod to another node secondary IP and verify that the srcIP is the expected nodeIP (this verifies SNAT's towards nodeIP are not deleted unless node is egressNode)")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(otherHostNetPodIP, pod2Name, podNamespace.Name, true, []string{egressNodeIP.String()}))
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

		command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}

		ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to egress1Node node")
		e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
		framework.Logf("Added egress-assignable label to node %s", egress1Node.name)
		e2enode.ExpectNodeHasLabel(context.TODO(), f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

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
			_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "delete", "pod", pod1Name, "--grace-period=0", "--force")
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
		if netConfigParams.networkName != types.DefaultNetworkName {
			ginkgo.Skip("Unsupported for UDNs")
		}

		command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}

		ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to egress1Node node")
		e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
		framework.Logf("Added egress-assignable label to node %s", egress1Node.name)
		e2enode.ExpectNodeHasLabel(context.TODO(), f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

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
		dbPods, err := e2ekubectl.RunKubectl(ovnNamespace, "get", "pods", "-l", "name=ovnkube-db", "-o=jsonpath='{.items..metadata.name}'")
		dbContainerName := "nb-ovsdb"
		if isInterconnectEnabled() {
			dbPods, err = e2ekubectl.RunKubectl(ovnNamespace, "get", "pods", "-l", "name=ovnkube-node", "--field-selector", fmt.Sprintf("spec.nodeName=%s", egress1Node.name), "-o=jsonpath='{.items..metadata.name}'")
		}
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
		if IsIPv6Cluster(f.ClientSet) {
			logicalIP = fmt.Sprintf("logical_ip=\"%s\"", srcPodIP.String())
		}
		snats, err := e2ekubectl.RunKubectl(ovnNamespace, "exec", dbPod, "-c", dbContainerName, "--", "ovn-nbctl", "--no-leader-only", "--columns=external_ip", "find", "nat", logicalIP)
		if err != nil {
			framework.Failf("Error: Check the OVN DB to ensure no SNATs are added for the standby egressIP, err: %v", err)
		}
		if !strings.Contains(snats, statuses[0].EgressIP) || strings.Contains(snats, egressIP3.String()) {
			framework.Failf("Step 7. Check the OVN DB to ensure no SNATs are added for the standby egressIP, failed")
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
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP3.String()}))
		framework.ExpectNoError(err, "Step 11. Check connectivity from pod to an external container and verify that the srcIP is the expected standby egressIP3 from object2, failed: %v", err)

		ginkgo.By("12. Check the OVN DB to ensure SNATs are added for only the standby egressIP")
		snats, err = e2ekubectl.RunKubectl(ovnNamespace, "exec", dbPod, "-c", dbContainerName, "--", "ovn-nbctl", "--no-leader-only", "--columns=external_ip", "find", "nat", logicalIP)
		if err != nil {
			framework.Failf("Error: Check the OVN DB to ensure SNATs are added for only the standby egressIP, err: %v", err)
		}
		if !strings.Contains(snats, egressIP3.String()) || strings.Contains(snats, egressIP1.String()) || strings.Contains(snats, egressIP2.String()) || strings.Contains(snats, egress1Node.nodeIP) {
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

		if isInterconnectEnabled() {
			dbPods, err = e2ekubectl.RunKubectl(ovnNamespace, "get", "pods", "-l", "name=ovnkube-node", "--field-selector", fmt.Sprintf("spec.nodeName=%s", egress2Node.name), "-o=jsonpath='{.items..metadata.name}'")
		}
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
		snats, err = e2ekubectl.RunKubectl(ovnNamespace, "exec", dbPod, "-c", dbContainerName, "--", "ovn-nbctl", "--no-leader-only", "--columns=external_ip", "find", "nat", logicalIP)
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
		e2ekubectl.RunKubectlOrDie("default", "delete", "eip", toDelete)

		ginkgo.By("18.  Check connectivity from pod to an external container and verify that the srcIP is the expected standby egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{unassignedEIP}))
		framework.ExpectNoError(err, "Step 18.  Check connectivity from pod to an external container and verify that the srcIP is the expected standby egressIP, failed: %v", err)

		ginkgo.By("19. Delete the remaining egressIP object")
		e2ekubectl.RunKubectlOrDie("default", "delete", "eip", toKeepEIP)

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
		e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
		e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")

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
		createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, command, podEgressLabel)

		ginkgo.By(fmt.Sprintf("4. Make egress node: %s unreachable", node1))
		setNodeReachable("iptables", node1, false)
		if IsIPv6Cluster(f.ClientSet) {
			setNodeReachable("ip6tables", node1, false)
		}

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
		err := wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "6. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP, failed, err: %v", err)

		ginkgo.By("7. Check connectivity from pod to the api-server (running hostNetwork:true) and verifying that the connection is achieved")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetDestinationAndTest(podNamespace.Name, fmt.Sprintf("https://%s/version", net.JoinHostPort(getApiAddress(), "443")), []string{pod1Name}))
		framework.ExpectNoError(err, "7. Check connectivity from pod to the api-server (running hostNetwork:true) and verifying that the connection is achieved, failed, err: %v", err)

		ginkgo.By("8, Make node 2 unreachable")
		setNodeReachable("iptables", node2, false)
		if IsIPv6Cluster(f.ClientSet) {
			setNodeReachable("ip6tables", node2, false)
		}

		ginkgo.By("9. Check that egress IP is un-assigned (empty status)")
		verifyEgressIPStatusLengthEquals(0, nil)

		ginkgo.By("10. Check connectivity from pod to an external \"node\" and verify that the IP is the node IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{pod1Node.nodeIP}))
		framework.ExpectNoError(err, "10. Check connectivity from pod to an external \"node\" and verify that the IP is the node IP, failed, err: %v", err)

		ginkgo.By("11. Make node 1 reachable again")
		setNodeReachable("iptables", node1, true)
		if IsIPv6Cluster(f.ClientSet) {
			setNodeReachable("ip6tables", node1, true)
		}

		ginkgo.By("12. Check that egress IP is assigned to node 1 again")
		statuses = verifyEgressIPStatusLengthEquals(1, func(statuses []egressIPStatus) bool {
			testNode := statuses[0].Node
			return testNode == node1
		})

		ginkgo.By("13. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "13. Check connectivity from pod to an external \"node\" and verify that the IP is the egress IP, failed, err: %v", err)

		ginkgo.By("14. Make node 2 reachable again")
		setNodeReachable("iptables", node2, true)
		if IsIPv6Cluster(f.ClientSet) {
			setNodeReachable("ip6tables", node2, true)
		}

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
		setNodeReachable("iptables", node1, false)
		if IsIPv6Cluster(f.ClientSet) {
			setNodeReachable("ip6tables", node1, false)
		}

		ginkgo.By("21. Unlabel node 2")
		e2enode.RemoveLabelOffNode(f.ClientSet, node2, "k8s.ovn.org/egress-assignable")

		ginkgo.By("22. Check that egress IP is un-assigned (since node 1 is both unreachable and NotReady)")
		verifyEgressIPStatusLengthEquals(0, nil)

		ginkgo.By("23. Make node 1 Ready")
		setNodeReady(node1, true)

		ginkgo.By("24. Check that egress IP is un-assigned (since node 1 is unreachable)")
		verifyEgressIPStatusLengthEquals(0, nil)

		ginkgo.By("25. Make node 1 reachable again")
		setNodeReachable("iptables", node1, true)
		if IsIPv6Cluster(f.ClientSet) {
			setNodeReachable("ip6tables", node1, true)
		}

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
		if netConfigParams.networkName != types.DefaultNetworkName {
			ginkgo.Skip("Unsupported for UDNs")
		}
		command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}

		ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to one nodes")
		e2enode.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")

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
		usedEgressNodeAvailabilityHandler = &egressNodeAvailabilityHandlerViaLabel{f}

		ginkgo.By("Setting a node as available for egress")
		usedEgressNodeAvailabilityHandler.Enable(egress1Node.name)

		podNamespace := f.Namespace
		podNamespace.Labels = map[string]string{
			"name": f.Namespace.Name,
		}
		updateNamespace(f, podNamespace)

		ginkgo.By("Creating an EgressIP object with one egress IPs defined")
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
		_, err := createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, command, podEgressLabel)
		framework.ExpectNoError(err, "can't create a client pod: %v", err)

		ginkgo.By("Creating an external kind container as server to send the traffic to/from")
		externalKindContainerName := "kind-external-container-for-egressip-mtu-test"
		serverPodPort := rand.Intn(echoServerPodPortMax-echoServerPodPortMin) + echoServerPodPortMin

		deleteClusterExternalContainer(targetNode.name)
		targetNode.name = externalKindContainerName
		externalKindIPv4, _ := createClusterExternalContainer(
			externalKindContainerName,
			agnhostImage,
			[]string{"--privileged", "--network", "kind"},
			[]string{"pause"},
		)

		// First disable PMTUD
		_, err = runCommand(containerRuntime, "exec", externalKindContainerName, "sysctl", "-w", "net.ipv4.ip_no_pmtu_disc=2")
		framework.ExpectNoError(err, "disabling PMTUD in the external kind container failed: %v", err)

		// Then run the server
		httpPort := fmt.Sprintf("--http-port=%d", serverPodPort)
		udpPort := fmt.Sprintf("--udp-port=%d", serverPodPort)
		_, err = runCommand(containerRuntime, "exec", "-d", externalKindContainerName, "/agnhost", "netexec", httpPort, udpPort)
		framework.ExpectNoError(err, "running netexec server in the external kind container failed: %v", err)

		ginkgo.By("Checking connectivity to the external kind container and verify that the source IP is the egress IP")
		var curlErr error
		_ = wait.PollUntilContextTimeout(
			context.Background(),
			retryInterval,
			retryTimeout,
			true,
			func(ctx context.Context) (bool, error) {
				curlErr := curlAgnHostClientIPFromPod(podNamespace.Name, pod1Name, egressIP1.String(), externalKindIPv4, serverPodPort)
				return curlErr == nil, nil
			},
		)
		framework.ExpectNoError(curlErr, "connectivity check to the external kind container failed: %v", curlErr)

		// We will ask the server to reply with a UDP packet bigger than the pod
		// network MTU. Since PMTUD has been disabled on the server, the reply
		// won't have the DF flag set. If the reply is not forwarded through the
		// cluster host kernel then OVN will just drop the reply and send back
		// an ICMP needs frag that the server will ignore. If the reply is
		// forwarded through cluster host kernel, it will be fragmented and sent
		// back to OVN reaching the client pod.
		ginkgo.By("Making the external kind container reply an oversized UDP packet and checking that it is recieved")
		payload := fmt.Sprintf("%01420d", 1)
		cmd := fmt.Sprintf("echo 'echo %s' | nc -w2 -u %s %d",
			payload,
			externalKindIPv4,
			serverPodPort,
		)
		stdout, err := e2epodoutput.RunHostCmd(
			podNamespace.Name,
			pod1Name,
			cmd)
		framework.ExpectNoError(err, "sending echo request to external kind container failed: %v", err)

		if stdout != payload {
			framework.Failf("external kind container did not reply with the requested payload")
		}

		ginkgo.By("Checking that there is no IP route exception and thus reply was fragmented")
		stdout, err = runCommand(containerRuntime, "exec", externalKindContainerName, "ip", "route", "get", egressIP1.String())
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
		if netConfigParams.networkName != types.DefaultNetworkName {
			ginkgo.Skip("Unsupported for UDNs")
		}
		// get v4, v6 from eips
		// check that node has both of them
		v4, v6 := getIPVersions(egressIPIP1, egressIPIP2)
		if v4 && utilnet.IsIPv6(net.ParseIP(egress1Node.nodeIP)) {
			ginkgo.Skip("Node does not have IPv4 address")
		}
		if v6 && !utilnet.IsIPv6(net.ParseIP(egress1Node.nodeIP)) {
			ginkgo.Skip("Node does not have IPv6 address")
		}
		egressNodeAvailabilityHandler := egressNodeAvailabilityHandlerViaLabel{f}
		ginkgo.By("0. Set two nodes as available for egress")
		egressNodeAvailabilityHandler.Enable(egress1Node.name)
		egressNodeAvailabilityHandler.Enable(egress2Node.name)
		defer egressNodeAvailabilityHandler.Restore(egress1Node.name)
		defer egressNodeAvailabilityHandler.Restore(egress2Node.name)
		podNamespace := f.Namespace
		podNamespace.Labels = map[string]string{
			"name": f.Namespace.Name,
		}
		updateNamespace(f, podNamespace)

		ginkgo.By("1. Create an EgressIP object with two egress IPs - both hosted by the same secondary host network")
		egressIPConfig := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)
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
		framework.ExpectNoError(err, "Step 4. Create two pods matching an EgressIP - running pod(s) failed to get "+
			"their IP(s), failed, err: %v", err)
		framework.Logf("Created two pods - pod %s on node %s and pod %s on node %s", pod1Name, pod1Node.name, pod2Name,
			pod2Node.name)

		ginkgo.By("5. Check connectivity from both pods to an external \"node\" hosted on the secondary host network " +
			"and verify the expected IPs")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod1Name,
			podNamespace.Name, true, []string{egressIPIP1, egressIPIP2}))
		framework.ExpectNoError(err, "Step 5. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that is a secondary host network and verify that the src IP is the expected egressIP, failed: %v",
			podNamespace.Name, pod1Name, err)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod2Name,
			podNamespace.Name, true, []string{egressIPIP1, egressIPIP2}))
		framework.ExpectNoError(err, "Step 5. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that is a secondary host network and verify that the src IP is the expected egressIP, failed: %v", podNamespace.Name, pod2Name, err)

		ginkgo.By("6. Check connectivity from one pod to the other and verify that the connection is achieved")
		pod2IP := getPodAddress(pod2Name, f.Namespace.Name)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetPodAndTest(f.Namespace.Name, pod1Name, pod2Name, pod2IP))
		framework.ExpectNoError(err, "Step 6. Check connectivity from one pod to the other and verify that the connection "+
			"is achieved, failed, err: %v", err)

		ginkgo.By("7. Check connectivity from both pods to the api-server (running hostNetwork:true) and verifying that " +
			"the connection is achieved")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetDestinationAndTest(podNamespace.Name,
			fmt.Sprintf("https://%s/version", net.JoinHostPort(getApiAddress(), "443")), []string{pod1Name, pod2Name}))
		framework.ExpectNoError(err, "Step 7. Check connectivity from both pods to the api-server (running hostNetwork:true) "+
			"and verifying that the connection is achieved, failed, err: %v", err)

		ginkgo.By("8. Update one of the pods, unmatching the EgressIP")
		pod2 := getPod(f, pod2Name)
		pod2.Labels = map[string]string{}
		updatePod(f, pod2)

		ginkgo.By("9. Check connectivity from pod that isn't selected by EgressIP anymore to an external \"node\" on " +
			"the OVN network and verify that the IP is the node IP.")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name,
			podNamespace.Name, true, []string{pod2Node.nodeIP}))
		framework.ExpectNoError(err, "Step 9. Check connectivity from that one to an external \"node\" on the OVN "+
			"network and verify that the IP is the node IP failed: %v", err)

		ginkgo.By("10. Update the unselected pod to be selected by the EgressIP")
		pod2 = getPod(f, pod2Name)
		pod2.Labels = podEgressLabel
		updatePod(f, pod2)

		ginkgo.By("11. Check connectivity from both pods to an external \"node\" hosted on the secondary host network " +
			"and verify the expected IPs")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod1Name, podNamespace.Name,
			true, []string{egressIPIP1, egressIPIP2}))
		framework.ExpectNoError(err, "Step 11. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that is a secondary host network and verify that the src IP is the expected egressIP, failed, err: %v", podNamespace.Name, pod1Name, err)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod2Name, podNamespace.Name,
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
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod1Name,
			podNamespace.Name, true, []string{statuses[0].EgressIP}))
		framework.ExpectNoError(err, "15. Check connectivity from a pod to an external \"node\" on the secondary host network"+
			" network and verify that the IP is the remaining egress IP, failed, err: %v", err)

		ginkgo.By("16. Set the other node as unavailable for egress")
		egressNodeAvailabilityHandler.Disable(egress2Node.name)

		ginkgo.By("17. Check connectivity from a pod to an external \"node\" on the OVN network and " +
			"verify that the IP is the node IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name,
			true, []string{pod1Node.nodeIP}))
		framework.ExpectNoError(err, "17. Check connectivity from a pod to an external \"node\" on the OVN network "+
			"and verify that the IP is the node IP for pod %s/%s and egress-ing from node %s with node IP %s: %v",
			podNamespace.Name, pod1Name, pod1Node.name, pod1Node.nodeIP, err)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name,
			podNamespace.Name, true, []string{pod2Node.nodeIP}))
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
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod1Name,
			podNamespace.Name, true, []string{statuses[0].EgressIP}))
		framework.ExpectNoError(err, "22. Check connectivity from a pod (%s/%s) to an external \"node\" on the secondary host network and verify "+
			"that the IP is the remaining egress IP, failed, err: %v", podNamespace.Name, pod1Name, err)

		ginkgo.By("23. Set the other node back as available for egress")
		egressNodeAvailabilityHandler.Enable(egress2Node.name)

		ginkgo.By("24. Check that the status is of length two")
		statuses = verifyEgressIPStatusLengthEquals(2, nil)

		ginkgo.By("25. Check that correct Egress IPs are assigned")
		gomega.Expect(verifyEgressIPStatusContainsIPs(statuses, []string{egressIPIP1, egressIPIP2})).Should(gomega.BeTrue())

		ginkgo.By("26. Check connectivity from the other pod to an external \"node\" on the secondary host network and verify the expected IPs")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod2Name,
			podNamespace.Name, true, []string{egressIPIP1, egressIPIP2}))
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
		if netConfigParams.networkName != types.DefaultNetworkName {
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
		podNamespace.Labels = map[string]string{
			"name": f.Namespace.Name,
		}
		updateNamespace(f, podNamespace)

		ginkgo.By("1. Create an EgressIP object with two egress IPs - one hosted by an OVN network and one by a secondary host network")
		// Assign the egress IP without conflicting with any node IP,
		// the kind subnet is /16 or /64 so the following should be fine.
		egressNodeIP := net.ParseIP(egress1Node.nodeIP)
		egressIP := dupIP(egressNodeIP)
		egressIP[len(egressIP)-2]++
		egressIPOVN := egressIP.String()
		egressIPSecondaryHost := "10.10.10.200"
		egressIPConfig := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)

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
		framework.ExpectNoError(err, "Step 4. Create two pods matching an EgressIP - running pod(s) failed to get "+
			"their IP(s), failed, err: %v", err)
		framework.Logf("Created two pods - pod %s on node %s and pod %s on node %s", pod1Name, pod1Node.name, pod2Name,
			pod2Node.name)

		ginkgo.By("5. Check connectivity a pod to an external \"node\" hosted on the OVN network " +
			"and verify the expected IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name,
			podNamespace.Name, true, []string{egressIPOVN}))
		framework.ExpectNoError(err, "Step 5. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that is OVN network and verify that the src IP is the expected egressIP, failed: %v",
			podNamespace.Name, pod1Name, err)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name,
			podNamespace.Name, true, []string{egressIPOVN}))
		framework.ExpectNoError(err, "Step 5. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that is OVN network and verify that the src IP is the expected egressIP, failed: %v", podNamespace.Name, pod2Name, err)

		ginkgo.By("6. Check connectivity a pod to an external \"node\" hosted on a secondary host network " +
			"and verify the expected IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod1Name,
			podNamespace.Name, true, []string{egressIPSecondaryHost}))
		framework.ExpectNoError(err, "Step 6. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that is secondary host network and verify that the src IP is the expected egressIP, failed: %v",
			podNamespace.Name, pod1Name, err)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod2Name,
			podNamespace.Name, true, []string{egressIPSecondaryHost}))
		framework.ExpectNoError(err, "Step 6. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that is secondary host network and verify that the src IP is the expected egressIP, failed: %v",
			podNamespace.Name, pod2Name, err)

		ginkgo.By("7. Check connectivity from one pod to the other and verify that the connection is achieved")
		pod2IP := getPodAddress(pod2Name, f.Namespace.Name)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetPodAndTest(f.Namespace.Name, pod1Name, pod2Name, pod2IP))
		framework.ExpectNoError(err, "Step 7. Check connectivity from one pod to the other and verify that the connection "+
			"is achieved, failed, err: %v", err)

		ginkgo.By("8. Check connectivity from both pods to the api-server (running hostNetwork:true) and verifying that " +
			"the connection is achieved")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetDestinationAndTest(podNamespace.Name,
			fmt.Sprintf("https://%s/version", net.JoinHostPort(getApiAddress(), "443")), []string{pod1Name, pod2Name}))
		framework.ExpectNoError(err, "Step 8. Check connectivity from both pods to the api-server (running hostNetwork:true) "+
			"and verifying that the connection is achieved, failed, err: %v", err)

		ginkgo.By("9. Update one of the pods, unmatching the EgressIP")
		pod2 := getPod(f, pod2Name)
		pod2.Labels = map[string]string{}
		updatePod(f, pod2)

		ginkgo.By("10. Check connectivity from pod that isn't selected by EgressIP anymore to an external \"node\" on " +
			"the OVN network and verify that the IP is the node IP.")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name,
			podNamespace.Name, true, []string{pod2Node.nodeIP}))
		framework.ExpectNoError(err, "Step 10. Check connectivity from that one to an external \"node\" on the OVN "+
			"network and verify that the IP is the node IP failed: %v", err)

		ginkgo.By("11. Update the unselected pod to be selected by the Egress IP")
		pod2 = getPod(f, pod2Name)
		pod2.Labels = podEgressLabel
		updatePod(f, pod2)

		ginkgo.By("12. Check connectivity from both pods to an external \"node\" hosted on the OVN network " +
			"and verify that the expected IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name,
			true, []string{egressIPOVN}))
		framework.ExpectNoError(err, "Step 12. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that is OVN network and verify that the src IP is the expected egress IP, failed, err: %v", podNamespace.Name, pod1Name, err)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name, podNamespace.Name,
			true, []string{egressIPOVN}))
		framework.ExpectNoError(err, "Step 12. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that is OVN network and verify that the src IP is the expected egress IP, failed, err: %v", podNamespace.Name, pod2Name, err)

		ginkgo.By("13. Check connectivity from both pods to an external \"node\" hosted on secondary host network " +
			"and verify that the expected IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod1Name, podNamespace.Name,
			true, []string{egressIPSecondaryHost}))
		framework.ExpectNoError(err, "Step 13. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that isn't OVN network and verify that the src IP is the expected egress IP, failed, err: %v", podNamespace.Name, pod1Name, err)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod2Name, podNamespace.Name,
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
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod1Name,
			podNamespace.Name, true, []string{statuses[0].EgressIP}))
		framework.ExpectNoError(err, "17. Check connectivity from both pods (%s/%s) to an external \"node\" on the secondary host"+
			" network and verify that the src IP is the expected egress IP, failed, err: %v", podNamespace.Name, pod1Name, err)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod2Name,
			podNamespace.Name, true, []string{statuses[0].EgressIP}))
		framework.ExpectNoError(err, "17. Check connectivity from both pods (%s/%s) to an external \"node\" on the secondary host network"+
			" network and verify that the src IP is the expected egress IP, failed, err: %v", podNamespace.Name, pod2Name, err)

		ginkgo.By("18. Set the other node, which is hosting the secondary host network egress IP as unavailable for egress")
		egressNodeAvailabilityHandler.Disable(nodeNameHostingSecondaryHostEIP)

		ginkgo.By("19. Check that the status is of length zero")
		statuses = verifyEgressIPStatusLengthEquals(0, nil)

		ginkgo.By("20. Check connectivity from both pods to an external \"node\" on the OVN network and verify that the src IP is the node IPs")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name,
			podNamespace.Name, true, []string{pod1Node.nodeIP}))
		framework.ExpectNoError(err, "20. Check connectivity from both pods (%s/%s) to an external \"node\" on the "+
			"OVN network and verify that the src IP is the node IP %s, failed: %v", podNamespace, pod1Name, pod1Node.nodeIP, err)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name,
			podNamespace.Name, true, []string{pod2Node.nodeIP}))
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
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name,
			podNamespace.Name, true, []string{egressIPOVN}))
		framework.ExpectNoError(err, "Step 27. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that is OVN network and verify that the src IP is the expected egress IP, failed: %v", podNamespace.Name, pod1Name, err)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name,
			podNamespace.Name, true, []string{egressIPOVN}))
		framework.ExpectNoError(err, "Step 27. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that is OVN network and verify that the src IP is the expected egress IP, failed: %v", podNamespace.Name, pod2Name, err)

		ginkgo.By("28. Check connectivity both pods to an external \"node\" on the secondary host network and verify the src IP is the expected egress IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod1Name,
			podNamespace.Name, true, []string{egressIPSecondaryHost}))
		framework.ExpectNoError(err, "Step 28. Check connectivity from pod (%s/%s) to an external container attached to "+
			"a network that is secondary host network and verify that the src IP is the expected egress IP, failed: %v", podNamespace.Name, pod1Name, err)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod2Name,
			podNamespace.Name, true, []string{egressIPSecondaryHost}))
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
		if netConfigParams.networkName != types.DefaultNetworkName {
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
		podNamespace.Labels = map[string]string{
			"name": f.Namespace.Name,
		}
		updateNamespace(f, podNamespace)

		ginkgo.By("1. Create two EgressIP objects with one egress IP each - hosted by a secondary host network")
		egressIPConfig := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)

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
		egressIPConfig = fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)
		if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)

		ginkgo.By("2. Check that status of both EgressIP objects is of length one")
		verifySpecificEgressIPStatusLengthEquals(egressIPName, 1, nil)
		verifySpecificEgressIPStatusLengthEquals(egressIPName2, 1, nil)

		ginkgo.By("3. Create two pods - one matching each EgressIP")
		createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, command, podEgressLabel)
		podEgressLabel2 := map[string]string{
			"wants": "egress2",
		}
		createGenericPodWithLabel(f, pod2Name, pod2Node.name, f.Namespace.Name, command, podEgressLabel2)
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
		framework.ExpectNoError(err, "Step 3. Create two pods - one matching each EgressIP, failed, err: %v", err)

		ginkgo.By("4. Check connectivity from both pods to an external \"node\" hosted on a secondary host network " +
			"and verify the expected IPs")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod1Name,
			podNamespace.Name, true, []string{egressIP1}))
		framework.ExpectNoError(err, "4. Check connectivity from both pods to an external \"node\" hosted on a secondary host network "+
			"and verify the expected IPs, failed for EgressIP %s: %v", egressIPName, err)
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod2Name,
			podNamespace.Name, true, []string{egressIP2}))
		framework.ExpectNoError(err, "4. Check connectivity from both pods to an external \"node\" hosted on a secondary host network "+
			"and verify the expected IPs, failed for EgressIP %s: %v", egressIPName2, err)

		ginkgo.By("5. Delete one EgressIP object")
		e2ekubectl.RunKubectlOrDie("default", "delete", "eip", egressIPName, "--ignore-not-found=true")

		ginkgo.By("6. Check connectivity to the host on the secondary host network from the pod selected by the other EgressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod2Name,
			podNamespace.Name, true, []string{egressIP2}))
		framework.ExpectNoError(err, "6. Check connectivity to the host on the secondary host network from the pod "+
			"selected by the other EgressIP, failed: %v", err)

		ginkgo.By("7. Check connectivity to the host on the OVN network from the pod not selected by EgressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name,
			podNamespace.Name, true, []string{pod1Node.nodeIP}))
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
		if netConfigParams.networkName != types.DefaultNetworkName {
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
		out, err := runCommand(containerRuntime, "exec", egress1Node.name, "ip", "-o", "route", "get", egressIP1)
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
		restoreLinkIPv6AddrFn := func() error { return nil }
		if isV6Node {
			ginkgo.By("attempting to find IPv6 global address for secondary network")
			address, err := runCommand(containerRuntime, "inspect", "-f",
				fmt.Sprintf("'{{ (index .NetworkSettings.Networks \"%s\").GlobalIPv6Address }}'", secondaryNetworkName), egress1Node.name)
			if err != nil {
				framework.Failf("failed to get node %s IP address for network %s: %v", egress1Node.name, secondaryNetworkName, err)
			}
			address = strings.TrimSuffix(address, "\n")
			address = strings.Trim(address, "'")
			ginkgo.By(fmt.Sprintf("found address %q", address))
			gomega.Expect(net.ParseIP(address)).ShouldNot(gomega.BeNil(), "IPv6 address for secondary network must be present")
			prefix, err := runCommand(containerRuntime, "inspect", "-f",
				fmt.Sprintf("'{{ (index .NetworkSettings.Networks \"%s\").GlobalIPv6PrefixLen }}'", secondaryNetworkName), egress1Node.name)
			if err != nil {
				framework.Failf("failed to get node %s IP prefix length for network %s: %v", egress1Node.name, secondaryNetworkName, err)
			}
			prefix = strings.TrimSuffix(prefix, "\n")
			prefix = strings.Trim(prefix, "'")
			_, err = strconv.Atoi(prefix)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "requires valid IPv6 address prefix")
			restoreLinkIPv6AddrFn = func() error {
				_, err := runCommand(containerRuntime, "exec", egress1Node.name, "ip", "-6", "address", "add",
					fmt.Sprintf("%s/%s", address, prefix), "dev", egressInterface, "nodad", "scope", "global")
				return err
			}
		}
		_, err = runCommand(containerRuntime, "exec", egress1Node.name, "ip", "link", "add", vrfName, "type", "vrf", "table", vrfRoutingTable)
		if err != nil {
			framework.Failf("failed to add VRF to node %s: %v", egress1Node.name, err)
		}
		defer runCommand(containerRuntime, "exec", egress1Node.name, "ip", "link", "del", vrfName)
		_, err = runCommand(containerRuntime, "exec", egress1Node.name, "ip", "link", "set", "dev", egressInterface, "master", vrfName)
		if err != nil {
			framework.Failf("failed to enslave interface %s to VRF %s node %s: %v", egressInterface, vrfName, egress1Node.name, err)
		}
		if isV6Node {
			gomega.Expect(restoreLinkIPv6AddrFn()).Should(gomega.Succeed(), "restoring IPv6 address should succeed")
		}
		egressNodeAvailabilityHandler := egressNodeAvailabilityHandlerViaLabel{f}
		ginkgo.By("1. Set one node as available for egress")
		egressNodeAvailabilityHandler.Enable(egress1Node.name)
		defer egressNodeAvailabilityHandler.Restore(egress1Node.name)
		podNamespace := f.Namespace
		podNamespace.Labels = map[string]string{
			"name": f.Namespace.Name,
		}
		updateNamespace(f, podNamespace)

		ginkgo.By("2. Create one EgressIP object with one egress IP hosted by a secondary host network")
		egressIPConfig := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)

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
		createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, command, podEgressLabel)
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut := getPodAddress(pod1Name, f.Namespace.Name)
			srcIP := net.ParseIP(kubectlOut)
			if srcIP == nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Step 4. Create a pod matching the EgressIP, failed, err: %v", err)
		ginkgo.By("5. Check connectivity from a pod to an external \"node\" hosted on a secondary host network " +
			"and verify the expected IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetSecondaryNode, pod1Name,
			podNamespace.Name, true, []string{egressIP1}))
		framework.ExpectNoError(err, "5. Check connectivity a pod to an external \"node\" hosted on a secondary host network "+
			"and verify the expected IP, failed for EgressIP %s: %v", egressIPName, err)
	})

	// two pods attached to different namespaces but the same role primary user defined network
	// One pod is deleted and ensure connectivity for the other pod is ok
	// The previous pod namespace is deleted and again, ensure connectivity for the other pod is ok
	ginkgo.It("[OVN network] multiple namespaces sharing a primary networks", func() {
		if !isNetworkSegmentationEnabled() || netConfigParams.role != "primary" {
			ginkgo.Skip("Network segmentation is disabled or network isn't a role primary UDN")
		}
		ginkgo.By(fmt.Sprintf("Building another namespace api object, basename %s", f.BaseName))
		otherNetworkNamespace, err := f.CreateNamespace(context.Background(), f.BaseName, map[string]string{
			"e2e-framework": f.BaseName,
		})
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

		ginkgo.By("namespace is connected to UDN, create a namespace attached to this primary as a layer3 UDN")
		nadClient, err := nadclient.NewForConfig(f.ClientConfig())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		netConfig := newNetworkAttachmentConfig(netConfigParams)
		netConfig.namespace = otherNetworkNamespace.Name
		_, err = nadClient.NetworkAttachmentDefinitions(otherNetworkNamespace.Name).Create(
			context.Background(),
			generateNAD(netConfig),
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
		pod1Namespace.Labels = selectedByEIPLabels
		updateNamespace(f, pod1Namespace)
		pod2OtherNetworkNamespace := otherNetworkNamespace.Name
		otherNetworkNamespace.Labels = selectedByEIPLabels
		updateNamespace(f, otherNetworkNamespace)

		ginkgo.By("3. Create an EgressIP object with one egress IP defined")
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
            wants: egress
`)
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
		pod1, err := createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, command, podEgressLabel)
		framework.ExpectNoError(err, "5. Create one pod matching the EgressIP: running on egress1Node, failed: %v", err)
		pod2, err := createGenericPodWithLabel(f, pod2Name, pod2Node.name, otherNetworkNamespace.Name, command, podEgressLabel)
		framework.ExpectNoError(err, "5. Create one pod matching the EgressIP: running on egress2Node, failed: %v", err)

		gomega.Expect(pod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, pod1)).Should(gomega.Succeed())
		gomega.Expect(pod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, pod2)).Should(gomega.Succeed())

		framework.ExpectNoError(err, "Step 5. Create one pod matching the EgressIP: running on egress1Node, failed, err: %v", err)
		framework.Logf("Created pod %s on node %s", pod1Name, pod1Node.name)
		framework.ExpectNoError(err, "Step 5. Create one pod matching the EgressIP: running on egress2Node, failed, err: %v", err)
		framework.Logf("Created pod %s on node %s", pod2Name, pod2Node.name)

		ginkgo.By("6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, pod1Namespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP, failed: %v", err)

		ginkgo.By("7. Check connectivity from pod connected to the same network and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name, pod2OtherNetworkNamespace, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 7. Check connectivity from pod connected to the same network and verify that the srcIP is the expected nodeIP, failed: %v", err)

		ginkgo.By("8. Delete pod in one namespace")
		framework.ExpectNoError(pod.DeletePodWithWait(context.TODO(), f.ClientSet, pod1), "pod %s/%s deletion failed", pod1.Namespace, pod1.Name)

		ginkgo.By("9. Check connectivity from other pod and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name, pod2OtherNetworkNamespace, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 9. Check connectivity from other pod and verify that the srcIP is the expected egressIP, failed: %v", err)

		ginkgo.By("10. Delete namespace with zero pods")
		gomega.Expect(f.ClientSet.CoreV1().Namespaces().Delete(context.TODO(), pod1.Namespace, metav1.DeleteOptions{})).To(gomega.Succeed())

		ginkgo.By("11. Check connectivity from other pod and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name, pod2OtherNetworkNamespace, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 11. Check connectivity from other pod and verify that the srcIP is the expected egressIP and verify that the srcIP is the expected nodeIP, failed: %v", err)
	})

	ginkgo.DescribeTable("[OVN network] multiple namespaces with different primary networks", func(otherNetworkAttachParms networkAttachmentConfigParams) {
		if !isNetworkSegmentationEnabled() {
			ginkgo.Skip("Network segmentation is disabled")
		}
		ginkgo.By(fmt.Sprintf("Building a namespace api object, basename %s", f.BaseName))
		otherNetworkNamespace, err := f.CreateNamespace(context.Background(), f.BaseName, map[string]string{
			"e2e-framework": f.BaseName,
		})
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		if netConfigParams.networkName == types.DefaultNetworkName {
			ginkgo.By("namespace is connected to CDN, create a namespace with primary as a layer3 UDN")
			// create L3 Primary UDN
			nadClient, err := nadclient.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			netConfig := newNetworkAttachmentConfig(otherNetworkAttachParms)
			netConfig.namespace = otherNetworkNamespace.Name
			_, err = nadClient.NetworkAttachmentDefinitions(otherNetworkNamespace.Name).Create(
				context.Background(),
				generateNAD(netConfig),
				metav1.CreateOptions{},
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		} else {
			// if network is L2,L3 or other, then other network is CDN
		}
		egressNodeAvailabilityHandler := egressNodeAvailabilityHandlerViaLabel{f}
		ginkgo.By("1. Set one node as available for egress")
		egressNodeAvailabilityHandler.Enable(egress1Node.name)
		defer egressNodeAvailabilityHandler.Restore(egress1Node.name)

		selectedByEIPLabels := map[string]string{
			"wants": "egress",
		}
		pod1Namespace := f.Namespace
		pod1Namespace.Labels = selectedByEIPLabels
		updateNamespace(f, pod1Namespace)
		pod2OtherNetworkNamespace := otherNetworkNamespace.Name
		otherNetworkNamespace.Labels = selectedByEIPLabels
		updateNamespace(f, otherNetworkNamespace)

		ginkgo.By("3. Create an EgressIP object with one egress IP defined")
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
            wants: egress
`)
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
		_, err = createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, command, podEgressLabel)
		framework.ExpectNoError(err, "5. Create one pod matching the EgressIP: running on egress1Node, failed: %v", err)
		_, err = createGenericPodWithLabel(f, pod2Name, pod2Node.name, otherNetworkNamespace.Name, command, podEgressLabel)
		framework.ExpectNoError(err, "5. Create one pod matching the EgressIP: running on egress2Node, failed: %v", err)

		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut := getPodAddress(pod1Name, f.Namespace.Name)
			srcIP := net.ParseIP(kubectlOut)
			if srcIP == nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Step 5. Create one pod matching the EgressIP: running on egress1Node, failed, err: %v", err)
		framework.Logf("Created pod %s on node %s", pod1Name, pod1Node.name)

		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut := getPodAddress(pod2Name, otherNetworkNamespace.Name)
			srcIP := net.ParseIP(kubectlOut)
			if srcIP == nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Step 5. Create one pod matching the EgressIP: running on egress2Node, failed, err: %v", err)
		framework.Logf("Created pod %s on node %s", pod2Name, pod2Node.name)

		ginkgo.By("6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, pod1Namespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 6. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP, failed: %v", err)

		ginkgo.By("7. Check connectivity from pod connected to a different network and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod2Name, pod2OtherNetworkNamespace, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 7. Check connectivity from pod connected to a different network and verify that the srcIP is the expected nodeIP, failed: %v", err)
	}, ginkgo.Entry("L3 Primary UDN", networkAttachmentConfigParams{
		name:     "l3primary",
		topology: types.Layer3Topology,
		cidr:     "10.10.0.0/16",
		role:     "primary",
	}))
},
	ginkgo.Entry(
		"L3 CDN", // No UDN attachments
		networkAttachmentConfigParams{
			networkName: types.DefaultNetworkName,
		},
		false,
	),
	ginkgo.Entry(
		"L3 UDN role primary",
		networkAttachmentConfigParams{
			name:     "l3primary",
			topology: types.Layer3Topology,
			cidr:     "10.10.0.0/16",
			role:     "primary",
		},
		false,
	),
)

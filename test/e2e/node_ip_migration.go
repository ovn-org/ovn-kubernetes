package e2e

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	utilnet "k8s.io/utils/net"
)

var _ = Describe("Node IP address migration", func() {
	const (
		namespacePrefix            = "node-ip-migration"
		podWorkerNodeName          = "primary"
		podSecondaryWorkerNodeName = "secondary"
		pollingTimeout             = 120
		pollingInterval            = 10
		settleTimeout              = 180
		egressIPYaml               = "egressip.yaml"
		externalContainerImage     = "k8s.gcr.io/e2e-test-images/agnhost:2.26"
		ciNetworkName              = "kind"
		externalContainerName      = "ip-migration-external"
		externalContainerPort      = "80"
		externalContainerEndpoint  = "/clientip"
	)

	var (
		tmpDirIpMigration   string
		externalContainerIp string
		egressIp            string
	)

	podLabels := map[string]string{
		"app": "ip-migration-test",
	}
	podCommand := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
	externalContainerCommand := []string{"netexec", "--http-port=" + externalContainerPort}

	egressIPYamlTemplate := `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: %s
spec:
    egressIPs:
    - %s
    podSelector:
        matchLabels:
            %s: %s
    namespaceSelector:
        matchLabels:
            name: %s
`
	fr := framework.NewDefaultFramework(namespacePrefix)

	BeforeEach(func() {
		By("Creating the temp directory")
		var err error
		tmpDirIpMigration, err = ioutil.TempDir("", "e2e")
		Expect(err).NotTo(HaveOccurred())
		// get the primary worker node for IP address migration
		// get the secondary worker node to spawn another pod on for pod to pod reachability tests

		By("Selecting 2 random worker nodes from the list for IP address migration tests")
		// true here means only select worker nodes
		workerNodes, err := ocGetNodes(true)
		Expect(err).NotTo(HaveOccurred())
		// just get 2 worker nodes
		Expect(len(workerNodes)).Should(BeNumerically(">=", 2))
		workerNode := ""
		secondaryWorkerNode := ""
		for k := range workerNodes {
			if workerNode == "" {
				workerNode = k
			} else if secondaryWorkerNode == "" {
				secondaryWorkerNode = k
			} else {
				break
			}
		}
		workerNodeIP := workerNodes[workerNode]
		secondaryWorkerNodeIP := workerNodes[secondaryWorkerNode]

		By("Creating a cluster external container")
		externalContainerIpv4, externalContainerIpv6 := createClusterExternalContainer(externalContainerName, externalContainerImage, []string{"--network", ciNetworkName, "-P"}, externalContainerCommand)
		if utilnet.IsIPv6String(workerNodeIP) {
			externalContainerIp = externalContainerIpv6
		} else {
			externalContainerIp = externalContainerIpv4
		}
		Expect(externalContainerIp).NotTo(BeEmpty())
		framework.Logf("ExternalContainerIp is %s", externalContainerIp)

		By("Creating a test pod on both selected worker nodes")
		podWorkerNode := newAgnhostPodOnNode(
			podWorkerNodeName,
			workerNode,
			podLabels,
			podCommand...)
		podSecondaryWorkerNode := newAgnhostPodOnNode(
			podSecondaryWorkerNodeName,
			secondaryWorkerNode,
			podLabels,
			podCommand...)
		_ = fr.PodClient().CreateSync(podWorkerNode)
		_ = fr.PodClient().CreateSync(podSecondaryWorkerNode)

		By("Adding the \"k8s.ovn.org/egress-assignable\" label to the first node")
		framework.AddOrUpdateLabelOnNode(fr.ClientSet, workerNode, "k8s.ovn.org/egress-assignable", "dummy")
		podNamespace := fr.Namespace
		podNamespace.Labels = map[string]string{
			"name": fr.Namespace.Name,
		}
		updateNamespace(fr, podNamespace)

		By("Creating an EgressIP object with one egress IP defined")
		// Assign the egress IP without conflicting with any node IP,
		// the kind subnet is /16 or /64 so the following should be fine.
		// pick something at the end of the range to avoid conflicts with the kind / docker network setup.
		// Exclude current ndoe IPs just for safety.
		egressIp, err = findLastFreeSubnetIp(externalContainerName, externalContainerIp, []string{workerNodeIP, secondaryWorkerNodeIP})
		framework.Logf("egressIP will be %s", egressIp)
		Expect(err).NotTo(HaveOccurred())

		var egressIPConfig = fmt.Sprintf(egressIPYamlTemplate, podLabels["app"], egressIp, "app", podLabels["app"], fr.Namespace.Name)
		if err := ioutil.WriteFile(tmpDirIpMigration+"/"+egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		_, err = framework.RunKubectl("default", "apply", "-f", tmpDirIpMigration+"/"+egressIPYaml)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Finding worker node %s's migration IP address", workerNode))
		// pick something at the end of the range to avoid conflicts with the kind / docker network setup.
		// the egress IP reachability check will not work here, so exclude it from the list; also exclude current node IPs just for safety.
		nextWorkerNodeIP, err := findLastFreeSubnetIp(externalContainerName, externalContainerIp, []string{egressIp, workerNodeIP, secondaryWorkerNodeIP})
		framework.Logf("New worker node IP will be %s", nextWorkerNodeIP)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Migrating worker node %s to IP address %s", workerNode, nextWorkerNodeIP))
		err = migrateWorkerNodeIP(workerNode, workerNodeIP, nextWorkerNodeIP)
		Expect(err).NotTo(HaveOccurred())

		// It takes OVN up to 3 to 4 minutes to update the encap IP
		// for the OVS tunnel endpoints. To be investigated why, but for the time
		// being, give this enough time.
		By(fmt.Sprintf("Sleeping for %d seconds to give things time to settle", settleTimeout))
		time.Sleep(time.Duration(settleTimeout) * time.Second)
	})

	var _ = AfterEach(func() {
		By("Removing the temp directory")
		Expect(os.RemoveAll(tmpDirIpMigration)).To(Succeed())

		By("Deleting the external container for EgressIP tests")
		deleteClusterExternalContainer(externalContainerName)

		By("Deleting the gressip service")
		_, _ = framework.RunKubectl("default", "delete", "eip", podLabels["app"])
		//Expect(err).NotTo(HaveOccurred())

		By("Removing the egress assignable label from all nodes")
		// true here means only select worker nodes
		workerNodes, err := ocGetNodes(true)
		Expect(err).NotTo(HaveOccurred())
		for nodeName := range workerNodes {
			_, _ = framework.RunKubectl("default", "label", "node", nodeName, "k8s.ovn.org/egress-assignable-")
		}
	})

	Context("when the node IP address is updated", func() {
		It("makes sure that the cluster is still operational", func() {
			By("Retrieving the test pods")
			podWorkerNode, err := fr.PodClient().Get(context.TODO(), podWorkerNodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			podSecondaryWorkerNode, err := fr.PodClient().Get(context.TODO(), podSecondaryWorkerNodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("Poking from %s on node %s to %s on node %s for a maximum of %d seconds",
				podWorkerNode.Name, podWorkerNode.Spec.NodeName,
				podSecondaryWorkerNode.Name, podSecondaryWorkerNode.Spec.NodeName,
				pollingTimeout))
			Eventually(func() bool {
				By("Poking the pod")
				err := pokePod(fr, podWorkerNodeName, podSecondaryWorkerNode.Status.PodIP)
				return err == nil
			}, pollingTimeout, pollingInterval).Should(BeTrue())

			By("Verifying egress IP still works")
			expectedAnswer := "^" + egressIp + ":.*"
			if utilnet.IsIPv6String(egressIp) {
				expectedAnswer = "^\\[" + egressIp + "\\]:.*"
			}
			Eventually(func() bool {
				By("Checking the egress IP")
				res, err := targetExternalContainerConnectToEndpoint(
					externalContainerName,
					externalContainerIp,
					externalContainerPort,
					externalContainerEndpoint,
					podWorkerNode.Name,
					fr.Namespace.Name,
					true,
					expectedAnswer,
				)
				if err != nil {
					framework.Logf("Current verification failed with %s", err)
					return false
				}
				return res
			}, pollingTimeout, pollingInterval).Should(BeTrue())
		})

	})
})

// ocGetNodes returns a map[string]string that maps nodeNames to IP addresses.
// When workersOnly is true, return only worker nodes (nodes with label node-role.kubernetes.io/master!="").
func ocGetNodes(workersOnly bool) (map[string]string, error) {
	nodeMap := make(map[string]string)
	cmd := []string{
		"get", "nodes", "-o=jsonpath='{range.items[*]}{.metadata.name} {\"\\t\"} {.status.addresses[?(@.type==\"InternalIP\")].address}{\"\\n\"}{end}'"}
	if workersOnly {
		cmd = append(cmd, "-l", "node-role.kubernetes.io/master!=")
	}
	output, err := framework.RunKubectl("", cmd...)
	if err != nil {
		return nil, err
	}
	output = strings.Trim(output, "'")
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		s := strings.Split(scanner.Text(), "\t")
		if len(s) != 2 {
			continue
		}
		nodeMap[strings.TrimSpace(s[0])] = strings.TrimSpace(s[1])
	}
	return nodeMap, nil
}

// findNextWorkerNodeIP will set the worker's IP to the next higher available IP address.
// It returns the chosen IP address or an error.
func findNextWorkerNodeIP(nodeName, nodeIP string) (string, error) {
	parsedNetIPMask, _, err := findIpAddressMaskInterfaceOnHost(nodeName, nodeIP)
	if err != nil {
		return "", err
	}

	// We are using 172.18.0.0/16 for the docker subnet.
	// We should be able to find something that we need here and that's
	// not assigned.
	incrementedIPNet := net.IPNet{IP: parsedNetIPMask.IP, Mask: parsedNetIPMask.Mask}
	var isReachable bool
	for {
		incrementedIPNet, err = nextIP(incrementedIPNet)
		if err != nil {
			return "", err
		}
		isReachable, err = isAddressReachableFromContainer(nodeName, incrementedIPNet.IP.String())
		if err != nil {
			return "", err
		}
		if !isReachable {
			return incrementedIPNet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("Unexpected error while trying to find a new IP for node %s with current IP %s", nodeName, nodeIP)
}

// findIpAddressMaskOnHost finds the string "<IP address>/<mask>" and
// interface name on host nodeName for nodeIP
func findIpAddressMaskInterfaceOnHost(containerName, containerIP string) (net.IPNet, string, error) {
	ipAddressCmdOutput, err := runCommand("docker", "exec", containerName, "ip", "-o", "address")
	if err != nil {
		return net.IPNet{}, "", err
	}
	re, err := regexp.Compile(fmt.Sprintf("%s/[0-9]{1,2}", containerIP))
	if err != nil {
		return net.IPNet{}, "", err
	}
	ipAddressMask := ""
	iface := ""
	scanner := bufio.NewScanner(strings.NewReader(ipAddressCmdOutput))
	for scanner.Scan() {
		line := scanner.Text()
		ipAddressMask = re.FindString(line)
		if ipAddressMask != "" {
			if exploded := strings.Fields(line); len(exploded) > 1 {
				iface = exploded[1]
			}
			break
		}
	}
	if ipAddressMask == "" {
		return net.IPNet{}, "", fmt.Errorf("IP address and mask were not found via `ip address` for node %s with IP %s",
			containerName,
			containerIP)
	}
	if iface == "" {
		return net.IPNet{}, "", fmt.Errorf("Interface not found for node %s with IP %s",
			containerName, containerIP)
	}
	parsedNetIP, parsedNetCIDR, err := net.ParseCIDR(ipAddressMask)
	if err != nil {
		return net.IPNet{}, "", err
	}
	return net.IPNet{IP: parsedNetIP, Mask: parsedNetCIDR.Mask}, iface, nil
}

// nextIP returns IP incremented by 1. If the incremented IP does not fit in the IPNet, it returns
// an error.
func nextIP(ipNet net.IPNet) (net.IPNet, error) {
	i := ipToInt(ipNet.IP)
	newIP := intToIP(i.Add(i, big.NewInt(1)))
	if !ipNet.Contains(newIP) {
		return ipNet, fmt.Errorf("Could not find a suitable IP address for the container")
	}
	nextIPNet := net.IPNet{IP: newIP, Mask: ipNet.Mask}
	return nextIPNet, nil
}

// priorIP returns IP minus 1. If the decreased IP does not fit in the IPNet, it returns
// an error.
func priorIP(ipNet net.IPNet) (net.IPNet, error) {
	i := ipToInt(ipNet.IP)
	newIP := intToIP(i.Sub(i, big.NewInt(1)))
	if !ipNet.Contains(newIP) {
		return ipNet, fmt.Errorf("Could not find a suitable IP address for the container")
	}
	nextIPNet := net.IPNet{IP: newIP, Mask: ipNet.Mask}
	return nextIPNet, nil
}

func ipToInt(ip net.IP) *big.Int {
	if v := ip.To4(); v != nil {
		return big.NewInt(0).SetBytes(v)
	}
	return big.NewInt(0).SetBytes(ip.To16())
}

func intToIP(i *big.Int) net.IP {
	return net.IP(i.Bytes())
}

// findLastFreeSubnetIp will find the last available IP on this subnet
func findLastFreeSubnetIp(containerName, containerIP string, excludedIps []string) (string, error) {
	parsedNetIPMask, _, err := findIpAddressMaskInterfaceOnHost(containerName, containerIP)
	if err != nil {
		return "", err
	}

	// We are using 172.18.0.0/16 for the docker subnet.
	// We should be able to find something that we need here and that's
	// not assigned.
	broadcastIP, err := subnetBroadcastIP(net.IPNet{IP: parsedNetIPMask.IP, Mask: parsedNetIPMask.Mask})
	if err != nil {
		return "", err
	}
	decrementedIPNet := net.IPNet{IP: broadcastIP, Mask: parsedNetIPMask.Mask}
	var isReachable bool

outer:
	for {
		decrementedIPNet, err = priorIP(decrementedIPNet)
		if err != nil {
			return "", err
		}
		for _, excludedIp := range excludedIps {
			if excludedIp == decrementedIPNet.IP.String() {
				continue outer
			}
		}
		isReachable, err = isAddressReachableFromContainer(containerName, decrementedIPNet.IP.String())
		if err != nil {
			return "", err
		}
		if !isReachable {
			return decrementedIPNet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("Unexpected error while trying to find the last free subnet IP %s", containerIP)
}

// subnetBroadcastIP returns the IP network's broadcast IP.
func subnetBroadcastIP(ipnet net.IPNet) (net.IP, error) {
	// ipv4
	if ipnet.IP.To4() != nil {
		// ip address in uint32
		ipBits := binary.BigEndian.Uint32(ipnet.IP.To4())

		// mask will give us all fixed bits of the subnet
		maskBits := binary.BigEndian.Uint32(ipnet.Mask)

		// inverted mask will give us all moving bits of the subnet
		invertedMaskBits := maskBits ^ 0xffffffff // xor the mask

		// network ip
		networkIpBits := ipBits & maskBits

		// broadcastIP = networkIP added to the inverted mask
		broadcastIpBits := networkIpBits | invertedMaskBits

		broadcastIp := make(net.IP, 4)
		binary.BigEndian.PutUint32(broadcastIp, broadcastIpBits)
		return broadcastIp, nil
	}
	// ipv6
	// this conversion is actually easier, follows the same principle as above
	byteIp := []byte(ipnet.IP)                // []byte representation of IP
	byteMask := []byte(ipnet.Mask)            // []byte representation of mask
	byteTargetIp := make([]byte, len(byteIp)) // []byte holding target IP
	for k, _ := range byteIp {
		invertedMask := byteMask[k] ^ 0xff // inverted mask byte
		byteTargetIp[k] = byteIp[k]&byteMask[k] | invertedMask
	}

	return net.IP(byteTargetIp), nil
}

// isAddressReachableFromContainer will curl towards targetIP.
// It will then check the node's neighbor table. If a neighbor entry for targetIP exists, return true, false otherwise.
// We use curl because that's the tool that's installed by default in the ubuntu kind containers;
// ping/arping are unfortunately not available.
func isAddressReachableFromContainer(nodeName, targetIP string) (bool, error) {
	// there's no ping/arping inside the default containers, so just use curl instead.
	// it's good enough to trigger ARP resolution.
	cmd := []string{"docker", "exec", nodeName}
	if utilnet.IsIPv6String(targetIP) {
		targetIP = fmt.Sprintf("[%s]", targetIP)
	}
	curlCommand := strings.Split(fmt.Sprintf("curl -g -q -s http://%s:%d",
		targetIP,
		80), " ")
	cmd = append(cmd, curlCommand...)
	_, err := runCommand(cmd...)
	// if this curl works, then the node is logically reachable, shortcut.
	if err == nil {
		return true, nil
	}

	// now, check the neighbor table and if the entry does not have REACHABLE or STALE
	// or PERMANENT, then this must be an unreachable entry (could be FAILED or
	// INCOMPLETE)
	ipNeighborOutput, err := runCommand("docker", "exec", nodeName, "ip", "neigh")
	if err != nil {
		return false, err
	}
	re, err := regexp.Compile(fmt.Sprintf("^%s ", targetIP))
	if err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(strings.NewReader(ipNeighborOutput))
	for scanner.Scan() {
		line := scanner.Text()
		if re.MatchString(line) {
			if strings.Contains(line, "REACHABLE") ||
				strings.Contains(line, "STALE") ||
				strings.Contains(line, "PERMANENT") {
				return true, nil
			} else {
				return false, nil
			}
		}
	}

	// we get here if the ip neighbor output does not contain the IP
	// address, in that case we consider it as not reachable
	return false, nil
}

// migrateWorkerNodeIP migrates the node nodeName from fromIP to targetIP
// Todo: This isn't atomic. We might end up with a broken state in between if any of the
// commands fail.
func migrateWorkerNodeIP(nodeName, fromIP, targetIP string) error {
	parsedNetIPMask, iface, err := findIpAddressMaskInterfaceOnHost(nodeName, fromIP)
	if err != nil {
		return err
	}

	exploded := strings.Split(parsedNetIPMask.String(), "/")
	if len(exploded) < 2 {
		return fmt.Errorf("Not a valid CIDR: %v", parsedNetIPMask)
	}
	mask := exploded[1]

	// add new IP first - this will preserve the default route
	newIPMask := targetIP + "/" + mask
	By(fmt.Sprintf("Adding new IP address %s to node %s", newIPMask, nodeName))
	cmd := []string{
		"docker",
		"exec",
		nodeName,
		"ip",
		"address",
		"add",
		newIPMask,
		"dev",
		iface}
	_, err = runCommand(cmd...)
	if err != nil {
		return err
	}

	// delete current IP
	By(fmt.Sprintf("Deleting current IP address %s from node %s", parsedNetIPMask.String(), nodeName))
	cmd = []string{
		"docker",
		"exec",
		nodeName,
		"ip",
		"address",
		"del",
		parsedNetIPMask.String(),
		"dev",
		iface}
	_, err = runCommand(cmd...)
	if err != nil {
		return err
	}

	// change kubeadm-flags.env IP
	By(fmt.Sprintf("Modifying kubelet configuration for node %s", nodeName))
	cmd = []string{
		"docker",
		"exec",
		nodeName,
		"sed",
		"-i",
		fmt.Sprintf("s/node-ip=%s/node-ip=%s/", fromIP, targetIP),
		"/var/lib/kubelet/kubeadm-flags.env"}
	_, err = runCommand(cmd...)
	if err != nil {
		return err
	}

	// restart kubelet
	By(fmt.Sprintf("Restarting kubelet on node %s", nodeName))
	cmd = []string{
		"docker",
		"exec",
		nodeName,
		"systemctl",
		"restart",
		"kubelet"}
	_, err = runCommand(cmd...)
	if err != nil {
		return err
	}

	return nil
}

// targetExternalContainerConnectToEndpoint targets the external test container from the specified pod and compares expectedAnswer to the actual answer.
func targetExternalContainerConnectToEndpoint(externalContainerName, externalContainerIp, externalContainerPort, externalContainerEndpoint, podName, podNamespace string, expectSuccess bool, expectedAnswer string) (bool, error) {
	output, err := framework.RunKubectl(
		podNamespace,
		"exec",
		podName,
		"--", "curl", "--connect-timeout", "2", net.JoinHostPort(externalContainerIp, externalContainerPort)+externalContainerEndpoint,
	)
	if err != nil {
		return false, err
	}

	isMatch, err := regexp.MatchString(expectedAnswer, output)
	if err != nil {
		return false, err
	}
	if !isMatch {
		framework.Logf("Expected answer: '%s'. Got output: '%s'", expectedAnswer, output)
	}
	return isMatch, nil
}

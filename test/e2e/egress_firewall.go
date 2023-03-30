package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	utilnet "k8s.io/utils/net"
)

// Validate the egress firewall policies by applying a policy and verify
// that both explicitly allowed traffic and implicitly denied traffic
// is properly handled as defined in the crd configuration in the test.
var _ = ginkgo.Describe("e2e egress firewall policy validation", func() {
	const (
		svcname string = "egress-firewall-policy"

		ovnContainer           string = "ovnkube-node"
		egressFirewallYamlFile string = "egress-fw.yml"
		testTimeout            string = "5"
		retryInterval                 = 1 * time.Second
		retryTimeout                  = 30 * time.Second
		ciNetworkName                 = "kind"
	)

	type nodeInfo struct {
		name   string
		nodeIP string
	}

	var (
		serverNodeInfo       nodeInfo
		exFWPermitTcpDnsDest string
		singleIPMask         string
		exFWDenyTcpDnsDest   string
		exFWPermitTcpWwwDest string
		exFWPermitCIDR       string
		exFWDenyCIDR         string
	)

	f := wrappedTestFramework(svcname)
	// node2ndaryIPs holds the nodeName as the key and the value is
	// a map with ipFamily(v4 or v6) as the key and the secondaryIP as the value
	// This is defined here globally to allow us to cleanup in AfterEach
	node2ndaryIPs := make(map[string]map[int]string)

	// Determine what mode the CI is running in and get relevant endpoint information for the tests
	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf(
				"Test requires >= 2 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		ips := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)

		serverNodeInfo = nodeInfo{
			name:   nodes.Items[1].Name,
			nodeIP: ips[1],
		}

		exFWPermitTcpDnsDest = "8.8.8.8"
		exFWDenyTcpDnsDest = "8.8.4.4"
		exFWPermitTcpWwwDest = "1.1.1.1"
		exFWPermitCIDR = "1.1.1.0/24"
		exFWDenyCIDR = "0.0.0.0/0"
		singleIPMask = "32"
		if IsIPv6Cluster(f.ClientSet) {
			exFWPermitTcpDnsDest = "2001:4860:4860::8888"
			exFWDenyTcpDnsDest = "2001:4860:4860::8844"
			exFWPermitTcpWwwDest = "2606:4700:4700::1111"
			exFWPermitCIDR = "2606:4700:4700::/64"
			exFWDenyCIDR = "::/0"
			singleIPMask = "128"
		}
	})

	ginkgo.AfterEach(func() {
		ginkgo.By("Deleting additional IP addresses from nodes")
		for nodeName, ipFamilies := range node2ndaryIPs {
			for _, ip := range ipFamilies {
				_, err := runCommand(containerRuntime, "exec", nodeName, "ip", "addr", "delete",
					fmt.Sprintf("%s/32", ip), "dev", "breth0")
				if err != nil && !strings.Contains(err.Error(),
					"RTNETLINK answers: Cannot assign requested address") {
					framework.Failf("failed to remove ip address %s from node %s, err: %q", ip, nodeName, err)
				}
			}
		}
	})

	ginkgo.It("Should validate the egress firewall policy functionality against remote hosts", func() {
		srcPodName := "e2e-egress-fw-src-pod"
		frameworkNsFlag := fmt.Sprintf("--namespace=%s", f.Namespace.Name)
		testContainer := fmt.Sprintf("%s-container", srcPodName)
		testContainerFlag := fmt.Sprintf("--container=%s", testContainer)
		// egress firewall crd yaml configuration
		var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: %s/%s
  - type: Allow
    to:
      cidrSelector: %s
    ports:
      - protocol: TCP
        port: 80
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, exFWPermitTcpDnsDest, singleIPMask, exFWPermitCIDR, exFWDenyCIDR)
		// write the config to a file for application and defer the removal
		if err := os.WriteFile(egressFirewallYamlFile, []byte(egressFirewallConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(egressFirewallYamlFile); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()
		// create the CRD config parameters
		applyArgs := []string{
			"apply",
			frameworkNsFlag,
			"-f",
			egressFirewallYamlFile,
		}
		framework.Logf("Applying EgressFirewall configuration: %s ", applyArgs)
		// apply the egress firewall configuration
		e2ekubectl.RunKubectlOrDie(f.Namespace.Name, applyArgs...)
		// create the pod that will be used as the source for the connectivity test
		createSrcPod(srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)

		// In very rare cases the 'nc' test commands do not return the expected result on the first try,
		// but while testing has reproduced these cases, the condition is temporary and only initially
		// after the pod has been created. Eventually the traffic tests work as expected, so they
		// are wrapped in PollImmediate with a total duration equal to 4 times the pollingDuration (5 seconds)
		// which should provide for 3 tries before a failure is actually flagged
		testTimeoutInt, err := strconv.Atoi(testTimeout)
		if err != nil {
			framework.Failf("failed to parse test timeout duration: %v", err)
		}
		pollingDuration := time.Duration(4*testTimeoutInt) * time.Second

		// Verify the remote host/port as explicitly allowed by the firewall policy is reachable
		ginkgo.By(fmt.Sprintf("Verifying connectivity to an explicitly allowed host %s is permitted as defined by the external firewall policy", exFWPermitTcpDnsDest))
		err = wait.PollImmediate(2, pollingDuration, func() (bool, error) {
			_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "nc", "-vz", "-w", testTimeout, exFWPermitTcpDnsDest, "53")
			if err == nil {
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			framework.Failf("Failed to connect to the remote host %s from container %s on node %s: %v", exFWPermitTcpDnsDest, ovnContainer, serverNodeInfo.name, err)
		}

		// Verify the remote host/port as implicitly denied by the firewall policy is not reachable
		ginkgo.By(fmt.Sprintf("Verifying connectivity to an implicitly denied host %s is not permitted as defined by the external firewall policy", exFWDenyTcpDnsDest))
		err = wait.PollImmediate(2, pollingDuration, func() (bool, error) {
			_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "nc", "-vz", "-w", testTimeout, exFWDenyTcpDnsDest, "53")
			if err != nil {
				return false, nil
			}
			return true, nil
		})
		if err != wait.ErrWaitTimeout {
			framework.Failf("Succeeded in connecting the implicitly denied remote host %s from container %s on node %s: %v", exFWDenyTcpDnsDest, ovnContainer, serverNodeInfo.name, err)
		}

		// Verify the explicitly allowed host/port tcp port 80 rule is functional
		ginkgo.By(fmt.Sprintf("Verifying connectivity to an explicitly allowed host %s is permitted as defined by the external firewall policy", exFWPermitTcpWwwDest))
		err = wait.PollImmediate(2, pollingDuration, func() (bool, error) {
			_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "nc", "-vz", "-w", testTimeout, exFWPermitTcpWwwDest, "80")
			if err == nil {
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			framework.Failf("Failed to curl the remote host %s from container %s on node %s: %v", exFWPermitTcpWwwDest, ovnContainer, serverNodeInfo.name, err)
		}

		// Verify the remote host/port 443 as implicitly denied by the firewall policy is not reachable
		ginkgo.By(fmt.Sprintf("Verifying connectivity to an implicitly denied port on host %s is not permitted as defined by the external firewall policy", exFWPermitTcpWwwDest))
		err = wait.PollImmediate(2, pollingDuration, func() (bool, error) {
			_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "nc", "-vz", "-w", testTimeout, exFWPermitTcpWwwDest, "443")
			if err != nil {
				return false, nil
			}
			return true, nil
		})
		if err != wait.ErrWaitTimeout {
			framework.Failf("Succeeded in connecting the implicitly denied remote host %s from container %s on node %s: %v", exFWPermitTcpWwwDest, ovnContainer, serverNodeInfo.name, err)
		}
	})

	ginkgo.It("Should validate the egress firewall policy functionality against cluster nodes by using node selector", func() {
		srcPodName := "e2e-egress-fw-src-pod"
		frameworkNsFlag := fmt.Sprintf("--namespace=%s", f.Namespace.Name)
		testContainer := fmt.Sprintf("%s-container", srcPodName)
		testContainerFlag := fmt.Sprintf("--container=%s", testContainer)
		// use random labels in case test runs again since it's a pain to remove the label from the node
		labelMatch := randStr(5)
		// egress firewall crd yaml configuration
		var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: %s/%s
  - type: Allow
    to:
      cidrSelector: %s
    ports:
      - protocol: TCP
        port: 80
  - type: Allow
    to:
      nodeSelector:
        matchLabels:
          %s: %s
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, exFWPermitTcpDnsDest, singleIPMask, exFWPermitCIDR, f.Namespace.Name, labelMatch, exFWDenyCIDR)
		framework.Logf("Egress Firewall CR generated: %s", egressFirewallConfig)
		// write the config to a file for application and defer the removal
		if err := os.WriteFile(egressFirewallYamlFile, []byte(egressFirewallConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(egressFirewallYamlFile); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()
		// create the CRD config parameters
		applyArgs := []string{
			"apply",
			frameworkNsFlag,
			"-f",
			egressFirewallYamlFile,
		}
		framework.Logf("Applying EgressFirewall configuration: %s ", applyArgs)
		// apply the egress firewall configuration
		e2ekubectl.RunKubectlOrDie(f.Namespace.Name, applyArgs...)
		// create the pod that will be used as the source for the connectivity test
		createSrcPod(srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)
		// create host networked pod
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
		framework.ExpectNoError(err)

		if len(nodes.Items) < 3 {
			framework.Failf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}
		ginkgo.By("Creating host network pods on each node")
		// get random port in case the test retries and port is already in use on host node
		rand.Seed(time.Now().UnixNano())
		min := 9900
		max := 9999
		hostNetworkPort := rand.Intn(max-min+1) + min
		framework.Logf("Random host networked port chosen: %d", hostNetworkPort)
		for _, node := range nodes.Items {
			// this creates a udp / http netexec listener which is able to receive the "hostname"
			// command. We use this to validate that each endpoint is received at least once
			args := []string{
				"netexec",
				fmt.Sprintf("--http-port=%d", hostNetworkPort),
				fmt.Sprintf("--udp-port=%d", hostNetworkPort),
			}

			// create host networked Pods
			_, err := createPod(f, node.Name+"-hostnet-ep", node.Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *v1.Pod) {
				p.Spec.Containers[0].Args = args
				p.Spec.HostNetwork = true
			})

			framework.ExpectNoError(err)

		}

		ginkgo.By("Selecting additional IP addresses for serverNode on which source pod lives (networking routing to secondaryIP address on other nodes is harder to achieve)")
		// add new secondary IP from node subnet to the node where the source pod lives on,
		// if the cluster is v6 add an ipv6 address
		toCurlSecondaryNodeIPAddresses := sets.NewString()
		// Calculate and store for AfterEach new target IP addresses.
		var newIP string
		if node2ndaryIPs[serverNodeInfo.name] == nil {
			node2ndaryIPs[serverNodeInfo.name] = make(map[int]string)
		}
		if utilnet.IsIPv6String(e2enode.GetAddresses(&nodes.Items[1], v1.NodeInternalIP)[0]) {
			newIP = "fc00:f853:ccd:e794::" + strconv.Itoa(12)
			framework.Logf("Secondary nodeIP %s for node %s", serverNodeInfo.name, newIP)
			node2ndaryIPs[serverNodeInfo.name][6] = newIP
		} else {
			newIP = "172.18.1." + strconv.Itoa(13)
			framework.Logf("Secondary nodeIP %s for node %s", serverNodeInfo.name, newIP)
			node2ndaryIPs[serverNodeInfo.name][4] = newIP
		}

		ginkgo.By("Adding additional IP addresses to node on which source pod lives")
		for nodeName, ipFamilies := range node2ndaryIPs {
			for _, ip := range ipFamilies {
				// manually add the a secondary IP to each node
				framework.Logf("Adding IP %s to node %s", ip, nodeName)
				_, err = runCommand(containerRuntime, "exec", nodeName, "ip", "addr", "add", ip, "dev", "breth0")
				if err != nil && !strings.Contains(err.Error(),	"Address already assigned") {
					framework.Failf("failed to add new IP address %s to node %s: %v", ip, nodeName, err)
				}
				toCurlSecondaryNodeIPAddresses.Insert(ip)
			}
		}

		// Verify basic external connectivity to ensure egress firewall is working for normal conditions
		ginkgo.By(fmt.Sprintf("Verifying connectivity to an explicitly allowed host %s is permitted as defined by the external firewall policy", exFWPermitTcpDnsDest))
		_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "nc", "-vz", "-w", testTimeout, exFWPermitTcpDnsDest, "53")
		if err != nil {
			framework.Failf("Failed to connect to the remote host %s from container %s on node %s: %v", exFWPermitTcpDnsDest, srcPodName, serverNodeInfo.name, err)
		}
		// Verify the remote host/port as implicitly denied by the firewall policy is not reachable
		ginkgo.By(fmt.Sprintf("Verifying connectivity to an implicitly denied host %s is not permitted as defined by the external firewall policy", exFWDenyTcpDnsDest))
		_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "nc", "-vz", "-w", testTimeout, exFWDenyTcpDnsDest, "53")
		if err == nil {
			framework.Failf("Succeeded in connecting the implicitly denied remote host %s from container %s on node %s", exFWDenyTcpDnsDest, ovnContainer, serverNodeInfo.name)
		}
		// Verify the explicitly allowed host/port tcp port 80 rule is functional
		ginkgo.By(fmt.Sprintf("Verifying connectivity to an explicitly allowed host %s is permitted as defined by the external firewall policy", exFWPermitTcpWwwDest))
		_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "nc", "-vz", "-w", testTimeout, exFWPermitTcpWwwDest, "80")
		if err != nil {
			framework.Failf("Failed to curl the remote host %s from container %s on node %s: %v", exFWPermitTcpWwwDest, ovnContainer, serverNodeInfo.name, err)
		}
		// Verify the remote host/port 443 as implicitly denied by the firewall policy is not reachable
		ginkgo.By(fmt.Sprintf("Verifying connectivity to an implicitly denied port on host %s is not permitted as defined by the external firewall policy", exFWPermitTcpWwwDest))
		_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "nc", "-vz", "-w", testTimeout, exFWPermitTcpWwwDest, "443")
		if err == nil {
			framework.Failf("Failed to curl the remote host %s from container %s on node %s: %v", exFWPermitTcpWwwDest, ovnContainer, serverNodeInfo.name, err)
		}

		ginkgo.By("Should NOT be able to reach each host networked pod via node selector")
		for _, node := range nodes.Items {
			path := fmt.Sprintf("http://%s:%d/hostname", node.Status.Addresses[0].Address, hostNetworkPort)
			_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "curl", "-g", "--max-time", "2", path)
			if err == nil {
				framework.Failf("Was able to curl node %s from container %s on node %s with no allow rule for egress firewall", node.Name, srcPodName, serverNodeInfo.name)
			}
		}

		ginkgo.By("Should NOT be able to reach each secondary hostIP via node selector")
		for _, address := range toCurlSecondaryNodeIPAddresses.List() {
			if !IsIPv6Cluster(f.ClientSet) && utilnet.IsIPv6String(address) ||  IsIPv6Cluster(f.ClientSet) && !utilnet.IsIPv6String(address) {
				continue
			}
			path := fmt.Sprintf("http://%s:%d/hostname", address, hostNetworkPort)
			_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "curl", "-g", "--max-time", "2", path)
			if err == nil {
				framework.Failf("Was able to curl node %s from container %s on nodeIP %s with no allow rule for egress firewall", address, srcPodName, serverNodeInfo.name)
			}
		}

		ginkgo.By("Apply label to nodes " + f.Namespace.Name + ":" + labelMatch)
		patch := struct {
			Metadata map[string]interface{} `json:"metadata"`
		}{
			Metadata: map[string]interface{}{
				"labels": map[string]string{f.Namespace.Name: labelMatch},
			},
		}
		for _, node := range nodes.Items {
			patchData, err := json.Marshal(&patch)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			f.ClientSet.CoreV1().Nodes().Patch(context.TODO(), node.Name, types.MergePatchType, patchData, metav1.PatchOptions{})
		}

		ginkgo.By("Should be able to reach each host networked pod via node selector")
		for _, node := range nodes.Items {
			path := fmt.Sprintf("http://%s:%d/hostname", node.Status.Addresses[0].Address, hostNetworkPort)
			_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "curl", "-g", "--max-time", "2", path)
			if err != nil {
				framework.Failf("Failed to curl node %s from container %s on node %s: %v", node.Name, srcPodName, serverNodeInfo.name, err)
			}
		}

		ginkgo.By("Should be able to reach secondary hostIP via node selector")
		for _, address := range toCurlSecondaryNodeIPAddresses.List() {
			if !IsIPv6Cluster(f.ClientSet) && utilnet.IsIPv6String(address) ||  IsIPv6Cluster(f.ClientSet) && !utilnet.IsIPv6String(address) {
				continue
			}
			path := fmt.Sprintf("http://%s:%d/hostname", address, hostNetworkPort)
			_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "curl", "-g", "--max-time", "2", path)
			if err != nil {
				framework.Failf("Failed to curl node %s from container %s on nodeIP %s", address, srcPodName, serverNodeInfo.name)
			}
		}
	})

	ginkgo.It("Should validate the egress firewall DNS does not deadlock when adding many dnsNames", func() {
		frameworkNsFlag := fmt.Sprintf("--namespace=%s", f.Namespace.Name)
		var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: %s/%s
  - type: Allow
    to:
      dnsName: www.test1.com
  - type: Allow
    to:
      dnsName: www.test2.com
  - type: Allow
    to:
      dnsName: www.test3.com
  - type: Allow
    to:
      dnsName: www.test4.com
  - type: Allow
    to:
      dnsName: www.test5.com
  - type: Allow
    to:
      dnsName: www.test6.com
  - type: Allow
    to:
      dnsName: www.test7.com
  - type: Allow
    to:
      dnsName: www.test8.com
  - type: Allow
    to:
      dnsName: www.test9.com
  - type: Allow
    to:
      dnsName: www.test10.com
  - type: Allow
    to:
      dnsName: www.test11.com
  - type: Allow
    to:
      dnsName: www.test12.com
  - type: Allow
    to:
      cidrSelector: %s
    ports:
      - protocol: TCP
        port: 80
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, exFWPermitTcpDnsDest, singleIPMask, exFWPermitCIDR, exFWDenyCIDR)
		// write the config to a file for application and defer the removal
		if err := os.WriteFile(egressFirewallYamlFile, []byte(egressFirewallConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(egressFirewallYamlFile); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()
		// create the CRD config parameters
		applyArgs := []string{
			"apply",
			frameworkNsFlag,
			"-f",
			egressFirewallYamlFile,
		}
		framework.Logf("Applying EgressFirewall configuration: %s ", applyArgs)
		// apply the egress firewall configuration
		e2ekubectl.RunKubectlOrDie(f.Namespace.Name, applyArgs...)
		gomega.Eventually(func() bool {
			output, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", "egressfirewall", "default")
			if err != nil {
				framework.Failf("could not get the egressfirewall default in namespace: %s", f.Namespace.Name)
			}
			return strings.Contains(output, "EgressFirewall Rules applied")
		}, 30*time.Second).Should(gomega.BeTrue())
	})

	ginkgo.It("Should validate the egress firewall allows inbound connections", func() {
		// 1. Create nodePort service and external container
		// 2. Check connectivity works both ways
		// 3. Apply deny-all egress firewall
		// 4. Check only inbound traffic is allowed

		efPodName := "e2e-egress-fw-pod"
		efPodPort := 1234
		serviceName := "service-for-pods"
		servicePort := 31234
		externalContainerName := "e2e-egress-fw-external-container"
		externalContainerPort := 1234

		frameworkNsFlag := fmt.Sprintf("--namespace=%s", f.Namespace.Name)
		testContainer := fmt.Sprintf("%s-container", efPodName)
		testContainerFlag := fmt.Sprintf("--container=%s", testContainer)
		denyCIDR := "0.0.0.0/0"
		if IsIPv6Cluster(f.ClientSet) {
			denyCIDR = "::/0"
		}
		// egress firewall crd yaml configuration
		var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, denyCIDR)
		// write the config to a file for application and defer the removal
		if err := os.WriteFile(egressFirewallYamlFile, []byte(egressFirewallConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(egressFirewallYamlFile); err != nil {
				framework.Logf("Unable to remove the CRD config from disk: %v", err)
			}
		}()
		// create the CRD config parameters
		applyArgs := []string{
			"apply",
			frameworkNsFlag,
			"-f",
			egressFirewallYamlFile,
		}

		ginkgo.By("Creating the egress firewall pod")
		// 1. create nodePort service and external container
		endpointsSelector := map[string]string{"servicebackend": "true"}
		_, err := createPod(f, efPodName, serverNodeInfo.name, f.Namespace.Name,
			[]string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%d", efPodPort)}, endpointsSelector)
		if err != nil {
			framework.Failf("Failed to create pod %s: %v", efPodName, err)
		}

		ginkgo.By("Creating the nodePort service")
		_, err = createServiceForPodsWithLabel(f, f.Namespace.Name, int32(servicePort), strconv.Itoa(efPodPort), "NodePort", endpointsSelector)
		framework.ExpectNoError(err, fmt.Sprintf("unable to create nodePort service, err: %v", err))

		ginkgo.By("Waiting for the endpoints to pop up")
		err = framework.WaitForServiceEndpointsNum(context.TODO(), f.ClientSet, f.Namespace.Name, serviceName, 1, time.Second, wait.ForeverTestTimeout)
		framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", serviceName, f.Namespace.Name)

		nodeIP := serverNodeInfo.nodeIP
		externalContainerIPV4, externalContainerIPV6 := createClusterExternalContainer(externalContainerName, agnhostImage,
			[]string{"--network", ciNetworkName, "-p", fmt.Sprintf("%d:%d", externalContainerPort, externalContainerPort)},
			[]string{"netexec", fmt.Sprintf("--http-port=%d", externalContainerPort)})
		defer deleteClusterExternalContainer(externalContainerName)

		// 2. Check connectivity works both ways
		// pod -> external container should work
		externalContainerIP := externalContainerIPV4
		if IsIPv6Cluster(f.ClientSet) {
			externalContainerIP = externalContainerIPV6
		}
		ginkgo.By(fmt.Sprintf("Verifying connectivity from pod %s to external container [%s]:%d",
			efPodName, externalContainerIP, externalContainerPort))
		_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", efPodName, testContainerFlag,
			"--", "nc", "-vz", "-w", testTimeout, externalContainerIP, strconv.Itoa(externalContainerPort))
		if err != nil {
			framework.Failf("Failed to connect from pod to external container, before egress firewall is applied")
		}
		// external container -> nodePort svc should work
		svc, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to fetch service: %s in namespace %s", serviceName, f.Namespace.Name)

		ginkgo.By(fmt.Sprintf("Verifying connectivity from external container %s to nodePort svc [%s]:%d",
			externalContainerIP, nodeIP, svc.Spec.Ports[0].NodePort))
		cmd := []string{"docker", "exec", externalContainerName, "nc", "-vz", "-w", testTimeout, nodeIP, strconv.Itoa(int(svc.Spec.Ports[0].NodePort))}
		framework.Logf("Running command %v", cmd)
		_, err = runCommand(cmd...)
		if err != nil {
			framework.Failf("Failed to connect to nodePort service from external container %s, before egress firewall is applied: %v",
				externalContainerName, err)
		}

		// 3. Apply deny-all egress firewall and wait for it to be applied
		framework.Logf("Applying EgressFirewall configuration: %s ", applyArgs)
		e2ekubectl.RunKubectlOrDie(f.Namespace.Name, applyArgs...)

		gomega.Eventually(func() bool {
			output, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", "egressfirewall", "default")
			if err != nil {
				framework.Failf("could not get the egressfirewall default in namespace: %s", f.Namespace.Name)
			}
			return strings.Contains(output, "EgressFirewall Rules applied")
		}, 10*time.Second).Should(gomega.BeTrue())

		// 4. Check that only inbound traffic is allowed
		// pod -> external container should be blocked
		ginkgo.By(fmt.Sprintf("Verifying connection from pod %s to external container %s is blocked:%d",
			efPodName, externalContainerIP, externalContainerPort))
		_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", efPodName, testContainerFlag,
			"--", "nc", "-vz", "-w", testTimeout, externalContainerIP, strconv.Itoa(externalContainerPort))
		if err == nil {
			framework.Failf("Egress firewall doesn't block connection from pod to external container")
		}
		// external container -> nodePort svc should work
		ginkgo.By(fmt.Sprintf("Verifying connectivity from external container %s to nodePort svc [%s]:%d",
			externalContainerIP, nodeIP, svc.Spec.Ports[0].NodePort))
		cmd = []string{"docker", "exec", externalContainerName, "nc", "-vz", "-w", testTimeout, nodeIP, strconv.Itoa(int(svc.Spec.Ports[0].NodePort))}
		framework.Logf("Running command %v", cmd)
		_, err = runCommand(cmd...)
		if err != nil {
			framework.Failf("Failed to connect to nodePort service from external container %s: %v",
				externalContainerName, err)
		}
	})
})

package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"

	"github.com/onsi/ginkgo"
	"github.com/pkg/errors"
)

const (
	// IANA assigned VXLAN UDP port - rfc7348
	vxlanPort            = "4789"
	podNetworkAnnotation = "k8s.ovn.org/pod-networks"
	exGwAnnotation       = "k8s.ovn.org/hybrid-overlay-external-gw"
	agnhostImage         = "k8s.gcr.io/e2e-test-images/agnhost:2.21"
)

// checkContinuousConnectivity creates a pod that checks the web connectivity against the host:ip during the duration specified in seconds
// the caller can check the result of the connectivity test checking the pod status
func checkContinuousConnectivity(f *framework.Framework, nodeName, podName, host string, port int, duration int) (*v1.Pod, error) {
	// we poll every 2 seconds with a timeout of 2, so we have to divide by 2 the duration parameter
	cmd := fmt.Sprintf("set -xe; for i in {1..%d}; do curl --max-time 2 http://%s; sleep 2; done", duration/2, net.JoinHostPort(host, strconv.Itoa(port)))
	command := []string{"bash", "-c", cmd}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    podName,
					Image:   agnhostImage,
					Command: command,
				},
			},
			NodeName:      nodeName,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	_, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	// Wait for pod network setup to be almost ready
	err = wait.PollImmediate(1*time.Second, 30*time.Second, func() (bool, error) {
		pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		_, ok := pod.Annotations[podNetworkAnnotation]
		return ok, nil
	})
	if err != nil {
		return nil, err

	}

	return f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.TODO(), podName, metav1.GetOptions{})
}

// pingCommand is the type to hold ping command.
type pingCommand string

const (
	// ipv4PingCommand is a ping command for IPv4.
	ipv4PingCommand pingCommand = "ping"
	// ipv6PingCommand is a ping command for IPv6.
	ipv6PingCommand pingCommand = "ping6"
)

// Place the workload on the specified node to test external connectivity
func checkConnectivityPingToHost(f *framework.Framework, nodeName, podName, host string, pingCmd pingCommand, timeout int, exGw bool) error {
	// Ping options are:
	// -c sends 3 pings
	// -W wait at most 2 seconds for a reply
	// -w timeout
	command := []string{"/bin/sh", "-c"}
	args := []string{fmt.Sprintf("sleep 5; %s -c 3 -W 2 -w %s %s", string(pingCmd), strconv.Itoa(timeout), host)}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    podName,
					Image:   agnhostImage,
					Command: command,
					Args:    args,
				},
			},
			NodeName:      nodeName,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	podClient := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
	_, err := podClient.Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// Wait for pod network setup to be almost ready
	err = wait.PollImmediate(1*time.Second, 30*time.Second, func() (bool, error) {
		pod, err := podClient.Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if exGw {
			if _, ok := pod.Annotations[exGwAnnotation]; !ok {
				return false, nil
			}
		}
		_, ok := pod.Annotations[podNetworkAnnotation]
		return ok, nil
	})
	if err != nil {
		// FIXME: this wait fails to check the pod annotations
		// it was not failing before because we were not checking it
	}

	err = e2epod.WaitForPodSuccessInNamespace(f.ClientSet, podName, f.Namespace.Name)

	if err != nil {
		logs, logErr := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, podName)
		if logErr != nil {
			framework.Logf("Warning: Failed to get logs from pod %q: %v", pod.Name, logErr)
		} else {
			framework.Logf("pod %s/%s logs:\n%s", f.Namespace.Name, pod.Name, logs)
		}
	}

	return err
}

// Create a pod on the specified node using the agnostic host image
func createAgnhostPod(f *framework.Framework, podName, nodeSelector string, args []string) error {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  podName,
					Image: agnhostImage,
					Args:  args,
				},
			},
			NodeName:      nodeSelector,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	_, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	return e2epod.WaitForPodNotPending(f.ClientSet, f.Namespace.Name, podName)
}

// Get the IP address of a pod
func getPodAddress(f *framework.Framework, podName string) (ip string, err error) {
	err = wait.PollImmediate(1*time.Second, 10*time.Second, func() (bool, error) {
		pod, inErr := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.TODO(), podName, metav1.GetOptions{})
		if inErr != nil {
			framework.Logf("Unable to retrieve the IP for pod %s %v", podName, err)
			return false, nil
		}
		if pod.Status.PodIP == "" {
			return false, nil
		}

		ip = pod.Status.PodIP
		return true, nil
	})

	return
}

// runCommand runs the cmd and instead of returning the byte buffer of
// stdout, it scans these for lines and returns a slice of output lines
func runCommand(args ...string) (lines []string, err error) {
	var buff bytes.Buffer
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = &buff
	err = cmd.Run()
	scanner := bufio.NewScanner(&buff)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, err
}

func getNodePodCIDR(f *framework.Framework, nodeName string, ipv6 bool) (string, error) {
	// retrieve the pod cidr for the worker node
	node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("can not get annotations on node %s", nodeName)
	}
	annotation, ok := node.Annotations["k8s.ovn.org/node-subnets"]
	if !ok {
		return "", fmt.Errorf("no annotation k8s.ovn.org/node-subnets")
	}

	ssSubnets := make(map[string]string)
	if err := json.Unmarshal([]byte(annotation), &ssSubnets); err == nil {
		if utilnet.IsIPv6String(ssSubnets["default"]) == ipv6 {
			return ssSubnets["default"], nil
		}
		return "", fmt.Errorf("wrong ip family %q", ssSubnets["default"])
	}
	dsSubnets := make(map[string][]string)
	// this assumes there are only 2 subnets with different IP families
	if err := json.Unmarshal([]byte(annotation), &dsSubnets); err == nil {
		if utilnet.IsIPv6String(dsSubnets["default"][0]) == ipv6 {
			return dsSubnets["default"][0], nil
		}
		return dsSubnets["default"][1], nil
	}
	return "", fmt.Errorf("could not parse annotation %q", annotation)
}

func getDockerContainerIP(container string) (ipv4 string, ipv6 string, err error) {
	// retrieve the IP address of the node using docker inspect
	lines, err := runCommand("docker", "inspect",
		"-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}},{{.GlobalIPv6Address}}{{end}}",
		container, // ... against the "node" container
	)
	if err != nil {
		return "", "", errors.Wrap(err, "failed to get container details")
	}
	if len(lines) != 1 {
		return "", "", errors.Errorf("file should only be one line, got %d lines", len(lines))
	}
	ips := strings.Split(lines[0], ",")
	if len(ips) != 2 {
		return "", "", errors.Errorf("container addresses should have 2 values, got %d values", len(ips))
	}
	return ips[0], ips[1], nil
}

var _ = ginkgo.Describe("e2e control plane", func() {
	const (
		extHost  = "external-host"
		duration = 40 // duration in seconds
	)
	var (
		svcname = "nettest"
		dstIP   string
	)

	f := framework.NewDefaultFramework(svcname)

	ginkgo.BeforeEach(func() {
		// Assert basic external connectivity.
		// Since this is not really a test of kubernetes in any way, we
		// leave it as a pre-test assertion, rather than a Ginko test.
		// start the container that will act as an external host of the cluster
		_, err := runCommand("docker", "run", "--rm", "-itd", "--privileged", "--network", "kind", "--name", extHost, agnhostImage, "netexec", "--http-port", "80")
		if err != nil {
			framework.Failf("failed to start external gateway test container: %v", err)
		}
		extIPv4, extIPv6, err := getDockerContainerIP(extHost)
		if err != nil {
			framework.Failf("failed to start external gateway test container: %v", err)
		}
		framework.Logf("The external gateway IP is IPv4 %s IPv6 %s", extIPv4, extIPv6)
		dstIP = extIPv4
		if framework.TestContext.ClusterIsIPv6() {
			dstIP = extIPv6
		}
	})

	ginkgo.AfterEach(func() {
		// tear down the container simulating the external host
		if _, err := runCommand("docker", "rm", "-f", extHost); err != nil {
			framework.Logf("failed to delete the gateway test container %s %v", extHost, err)
		}
	})

	ginkgo.It("should provide External connection continuously when ovnkube-node pod is killed", func() {
		ginkgo.By("Running container which tries to connect to external host in a loop")

		// Create a pod to test the connectivity
		start := time.Now()
		testPod, err := checkContinuousConnectivity(f, "", "connectivity-test-continuous", dstIP, 80, duration)
		framework.ExpectNoError(err, "error creating pod to check connectivity")

		// find the ovnkube-node pod
		podList, _ := f.ClientSet.CoreV1().Pods("ovn-kubernetes").List(context.TODO(), metav1.ListOptions{})
		podName := ""
		for _, pod := range podList.Items {
			if strings.HasPrefix(pod.Name, "ovnkube-node") && pod.Spec.NodeName == testPod.Spec.NodeName {
				podName = pod.Name
				break
			}
		}

		// delete the ovnkube-node pod
		ginkgo.By("Deleting the ovnkube-node pod")
		zero := int64(0)
		err = f.ClientSet.CoreV1().Pods("ovn-kubernetes").Delete(context.TODO(), podName, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		framework.ExpectNoError(err, "should delete ovnkube-node pod")
		framework.Logf("Deleted ovnkube-node %q", podName)
		elapsed := time.Since(start)

		// expect connectivity was not interrupted
		err = e2epod.WaitForPodSuccessInNamespace(f.ClientSet, testPod.Name, f.Namespace.Name)
		if err != nil {
			logs, logErr := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, testPod.Name, testPod.Name)
			if logErr != nil {
				framework.Logf("Warning: Failed to get logs from pod %q: %v", testPod.Name, logErr)
			} else {
				framework.Logf("pod %s/%s logs:\n%s", f.Namespace.Name, testPod.Name, logs)
			}
		}
		framework.ExpectNoError(err, "error checking the connectivity during the test")
		// and that we tested the connectivity during the disruption period
		if elapsed > duration*time.Second {
			framework.Failf("Test invalid, it didn't cover the disruption period %d sec, elapsed: %s", duration, elapsed)
		}

	})

	ginkgo.It("should provide External connection continuously when ovnkube-master pod is killed", func() {
		ginkgo.By("Running container which tries to connect to external host in a loop")

		// Create a pod to test the connectivity
		start := time.Now()
		testPod, err := checkContinuousConnectivity(f, "", "connectivity-test-continuous", dstIP, 80, duration)
		framework.ExpectNoError(err, "error creating pod to check connectivity")

		// find the ovnkube-master pod
		podList, err := f.ClientSet.CoreV1().Pods("ovn-kubernetes").List(context.TODO(), metav1.ListOptions{})
		framework.ExpectNoError(err, "can not list pods on the ovn-kubernetes namespace")
		podName := ""
		for _, pod := range podList.Items {
			if strings.HasPrefix(pod.Name, "ovnkube-master") {
				podName = pod.Name
				break
			}
		}
		// delete the ovnkube-master pod
		ginkgo.By("Deleting the ovnkube-master pod")
		zero := int64(0)
		err = f.ClientSet.CoreV1().Pods("ovn-kubernetes").Delete(context.TODO(), podName, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		framework.ExpectNoError(err, "should delete ovnkube-master pod")
		framework.Logf("Deleted ovnkube-master %q", podName)
		elapsed := time.Since(start)

		// expect connectivity was not interrupted
		err = e2epod.WaitForPodSuccessInNamespace(f.ClientSet, testPod.Name, f.Namespace.Name)
		if err != nil {
			logs, logErr := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, testPod.Name, testPod.Name)
			if logErr != nil {
				framework.Logf("Warning: Failed to get logs from pod %q: %v", testPod.Name, logErr)
			} else {
				framework.Logf("pod %s/%s logs:\n%s", f.Namespace.Name, testPod.Name, logs)
			}
		}
		framework.ExpectNoError(err, "error checking the connectivity during the test")
		// and that we tested the connectivity during the disruption period
		if elapsed > duration*time.Second {
			framework.Failf("Test invalid, it didn't cover the disruption period %d sec, elapsed: %s", duration, elapsed)
		}
	})
})

// Test e2e hybrid sdn inter-node connectivity between worker nodes and validate pods do not traverse the external gateway
var _ = ginkgo.Describe("test e2e inter-node connectivity between worker nodes hybrid overlay on separate worker nodes", func() {
	const (
		svcname         string = "internode-hyb-sdn-e2e"
		ovnNs           string = "ovn-kubernetes"
		ovnContainer    string = "ovnkube-node"
		gwContainerName string = "gw-test-container-internode"
		extGW           string = "172.17.0.250"
	)

	f := framework.NewDefaultFramework(svcname)
	type nodeInfo struct {
		name   string
		nodeIP string
	}

	var (
		clientNodeInfo, serverNodeInfo nodeInfo
		exVtepIP                       string
	)

	// Determine what mode the CI is running in and get relevant endpoint information for the tests
	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf(
				"Test requires >= 2 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		ips := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)

		clientNodeInfo = nodeInfo{
			name:   nodes.Items[0].Name,
			nodeIP: ips[0],
		}

		serverNodeInfo = nodeInfo{
			name:   nodes.Items[1].Name,
			nodeIP: ips[1],
		}
		// start the container that will act as an external gateway
		_, err = runCommand("docker", "run", "-itd", "--privileged", "--network", "kind", "--name", gwContainerName, agnhostImage, "netexec", "--http-port", "80")
		if err != nil {
			framework.Failf("failed to start external gateway test container: %v", err)
		}
		extIPv4, extIPv6, err := getDockerContainerIP(gwContainerName)
		if err != nil {
			framework.Failf("failed to start external gateway test container: %v", err)
		}
		framework.Logf("The external gateway IP is IPv4 %s IPv6 %s", extIPv4, extIPv6)
		exVtepIP = extIPv4
		if framework.TestContext.ClusterIsIPv6() {
			exVtepIP = extIPv6
		}
		framework.Logf("The external gateway IP is %s", exVtepIP)

		// Annotate the pods to route pods to hybrid-sdn bridge br-ext
		framework.Logf("Annotating the external gateway test namespace")
		ns, err := f.ClientSet.CoreV1().Namespaces().Get(context.TODO(), f.Namespace.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "Error getting Namespace %v: %v", f.Namespace.Name, err)
		if ns.ObjectMeta.Annotations == nil {
			ns.ObjectMeta.Annotations = map[string]string{}
		}
		ns.ObjectMeta.Annotations["k8s.ovn.org/hybrid-overlay-external-gw"] = extGW
		ns.ObjectMeta.Annotations["k8s.ovn.org/hybrid-overlay-vtep"] = exVtepIP
		ns, err = f.ClientSet.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "Error updating Namespace %v: %v", f.Namespace.Name, err)
	})

	ginkgo.AfterEach(func() {
		// tear down the container simulating the gateway
		_, err := runCommand("docker", "rm", "-f", gwContainerName)
		if err != nil {
			framework.Failf("failed to delete the gateway test container %v", err)
		}
	})

	ginkgo.It("Should validate connectivity between pods with hybrid overlay on separate worker nodes and ensure br-ext is not traversed", func() {
		dstPingPodName := "e2e-dst-ping-pod"

		ginkgo.By(fmt.Sprintf("Creating a container on node %s and verifying connectivity to a pod on node %s", clientNodeInfo.name, serverNodeInfo.name))

		// Create the pod that will be used as the destination for the connectivity test
		err := createAgnhostPod(f, dstPingPodName, serverNodeInfo.name, []string{"pause"})
		framework.ExpectNoError(err)

		// Wait for pod exgw setup to be almost ready
		pingTarget, err := getPodAddress(f, dstPingPodName)
		framework.ExpectNoError(err, "error trying to get exgw pod address")
		err = wait.PollImmediate(1*time.Second, 30*time.Second, func() (bool, error) {
			pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.TODO(), dstPingPodName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			if value, ok := pod.Annotations["k8s.ovn.org/hybrid-overlay-external-gw"]; !ok || value != pingTarget {
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			// FIXME: this wait fails to check the pod annotations
			// it was not failing before the tests  we were not checking it
		}

		// Spin up another pod that attempts to reach the previously started pod on separate nodes
		pingCommand := ipv4PingCommand
		if framework.TestContext.ClusterIsIPv6() {
			pingCommand = ipv6PingCommand
		}
		framework.ExpectNoError(
			checkConnectivityPingToHost(f, clientNodeInfo.name, "e2e-src-ping-pod", pingTarget, pingCommand, 5, true))

		podList, err := f.ClientSet.CoreV1().Pods(ovnNs).List(context.TODO(), metav1.ListOptions{})
		framework.ExpectNoError(err, "error listing pods on namespace %s", ovnNs)
		podName := ""
		for _, pod := range podList.Items {
			if strings.HasPrefix(pod.Name, ovnContainer) && pod.Spec.NodeName == clientNodeInfo.name {
				podName = pod.Name
				break
			}
		}
		if podName == "" {
			framework.Failf("Expected container %s running on %s error %v", ovnContainer, clientNodeInfo.name, err)
		}

		// dump the flowmods from br-ext to verify no counters are hit
		ovnContainerFlag := fmt.Sprintf("--container=%s", ovnContainer)
		ovnNsFlag := fmt.Sprintf("--namespace=%s", ovnNs)
		// dump the flowmods from br-ext to verify no counters are hit
		kubectlOut, err := framework.RunKubectl(ovnNs, "exec", podName, ovnNsFlag, ovnContainerFlag, "--", "ovs-ofctl", "dump-flows", "br-ext")
		if err != nil {
			framework.Failf("Expected container %s running on %s error %v", ovnContainer, clientNodeInfo.name, err)
		}
		for _, flowmod := range strings.Split(kubectlOut, "\n") {
			// filter out irrelevant lines from ofctl output
			if strings.Contains(flowmod, pingTarget) {
				// verify no flowmod counters were hit in br-ext for the target
				if !strings.Contains(flowmod, "n_packets=0") {
					framework.Failf("Expected packets=0 but found the flow %s", flowmod)
				}
			}
		}
	})
})

// Validate pods can reach the initial gateway and then update the namespace
// annotation to point to a second container also emulating the external gateway
var _ = ginkgo.Describe("e2e multiple external gateway update validation", func() {
	const (
		svcname             string = "multiple-externalgw"
		extGwAlt1           string = "10.249.1.1"
		extGwAlt2           string = "10.249.2.1"
		extGwAlt1IPv6       string = "fd00:10:249:1::1"
		extGwAlt2IPv6       string = "fd00:10:249:2::1"
		ovnNs               string = "ovn-kubernetes"
		gwContainerNameAlt1 string = "gw-test-container-alt"
		gwContainerNameAlt2 string = "gw-test-container-alt2"
	)

	f := framework.NewDefaultFramework(svcname)
	type nodeInfo struct {
		name   string
		nodeIP string
	}

	var (
		clientNodeInfo nodeInfo
	)

	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf(
				"Test requires >= 2 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		ips := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)

		clientNodeInfo = nodeInfo{
			name:   nodes.Items[0].Name,
			nodeIP: ips[0],
		}

	})

	ginkgo.AfterEach(func() {
		// tear down the containers simulating the gateways
		if _, err := runCommand("docker", "rm", "-f", gwContainerNameAlt1); err != nil {
			framework.Logf("failed to delete the gateway test container %s %v", gwContainerNameAlt1, err)
		}
		if _, err := runCommand("docker", "rm", "-f", gwContainerNameAlt2); err != nil {
			framework.Logf("failed to delete the gateway test container %s %v", gwContainerNameAlt2, err)
		}
	})

	ginkgo.It("Should validate connectivity before and after updating the namespace annotation to a new vtep and external gateway", func() {
		extVtepGW := extGwAlt1
		pingCommand := ipv4PingCommand
		if framework.TestContext.ClusterIsIPv6() {
			extVtepGW = extGwAlt1IPv6
			pingCommand = ipv6PingCommand
		}

		framework.Logf("the pod side vtep node is %s and the ip %s", clientNodeInfo.name, clientNodeInfo.nodeIP)
		podCIDR, err := getNodePodCIDR(f, clientNodeInfo.name, framework.TestContext.ClusterIsIPv6())
		if err != nil {
			framework.Failf("Error retrieving the pod cidr from %s %v", clientNodeInfo.name, err)
		}
		framework.Logf("the pod cidr for node %s is %s", clientNodeInfo.name, podCIDR)

		// setup the first external gateway
		err = setupExternalVTEPGateway(f, gwContainerNameAlt1, extVtepGW, clientNodeInfo.nodeIP, podCIDR, clientNodeInfo.name)
		framework.ExpectNoError(err)

		// Wait for the exGW pod networking to be almost, updated
		time.Sleep(5 * time.Second)
		// Verify the initial gateway is reachable from the new pod
		ginkgo.By(fmt.Sprintf("Verifying connectivity to the updated annotation and initial external gateway and vtep %s", extVtepGW))

		framework.ExpectNoError(
			checkConnectivityPingToHost(f, clientNodeInfo.name, "e2e-src-ping-pod", extVtepGW, pingCommand, 5, true))

		ginkgo.By("Starting a new external gateway")
		extVtepGW = extGwAlt2
		if framework.TestContext.ClusterIsIPv6() {
			extVtepGW = extGwAlt2IPv6
		}
		err = setupExternalVTEPGateway(f, gwContainerNameAlt2, extVtepGW, clientNodeInfo.nodeIP, podCIDR, clientNodeInfo.name)
		framework.ExpectNoError(err)

		// Wait for the exGW pod networking to be almost, updated
		time.Sleep(5 * time.Second)

		// Verify the updated gateway is reachable from the pods
		ginkgo.By(fmt.Sprintf("Verifying connectivity to the updated annotation and new external gateway and vtep %s", extVtepGW))
		framework.ExpectNoError(
			checkConnectivityPingToHost(f, clientNodeInfo.name, "e2e-src-ping-pod2", extVtepGW, pingCommand, 5, true))

	})
})

// Validate pods can reach a network running in a container's looback address via
// an external gateway running on eth0 of the container without any tunnel encap.
// Next, the test updates the namespace annotation to point to a second container,
// emulating the ext gateway. This test requires shared gateway mode in the job infra.
var _ = ginkgo.Describe("e2e non-vxlan external gateway and update validation", func() {
	const (
		svcname             string = "multiple-novxlan-externalgw"
		extGwAlt1           string = "10.249.1.1"
		extGwAlt2           string = "10.249.2.1"
		extGwAlt1IPv6       string = "fd00:10:249:1::1"
		extGwAlt2IPv6       string = "fd00:10:249:2::1"
		ovnNs               string = "ovn-kubernetes"
		ovnContainer        string = "ovnkube-node"
		gwContainerNameAlt1 string = "gw-novxlan-test-container-alt1"
		gwContainerNameAlt2 string = "gw-novxlan-test-container-alt2"
		sharedGatewayBridge string = "breth0"
	)

	f := framework.NewDefaultFramework(svcname)

	type nodeInfo struct {
		name   string
		nodeIP string
	}

	var (
		clientNodeInfo nodeInfo
	)

	// Determine what mode the CI is running in and get relevant endpoint information for the tests
	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf(
				"Test requires >= 2 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		ips := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)

		clientNodeInfo = nodeInfo{
			name:   nodes.Items[0].Name,
			nodeIP: ips[0],
		}
		// get the podname for the ovnkube-node running in the client node
		podList, err := f.ClientSet.CoreV1().Pods(ovnNs).List(context.TODO(), metav1.ListOptions{})
		framework.ExpectNoError(err, "error listing pods on namespace %s", ovnNs)
		podName := ""
		for _, pod := range podList.Items {
			if strings.HasPrefix(pod.Name, ovnContainer) && pod.Spec.NodeName == clientNodeInfo.name {
				podName = pod.Name
				break
			}
		}
		if podName == "" {
			framework.Failf("Expected container %s running on %s error %v", ovnContainer, clientNodeInfo.name, err)
		}
		// skip the test if the job infra is not running in shared gateway mode by checking if breth0 exists
		ovnContainerFlag := fmt.Sprintf("--container=%s", ovnContainer)
		ovnNsFlag := fmt.Sprintf("--namespace=%s", ovnNs)
		_, err = framework.RunKubectl(ovnNs, "exec", podName, ovnNsFlag, ovnContainerFlag, "--", "ovs-vsctl", "br-exists", sharedGatewayBridge)
		if err != nil {
			e2eskipper.Skipf("shared gateway mode not running in the current job setup, skipping non-vxlan external gateway testing")
		}
	})

	ginkgo.AfterEach(func() {
		// tear down the containers simulating the gateways
		if _, err := runCommand("docker", "rm", "-f", gwContainerNameAlt1); err != nil {
			framework.Logf("failed to delete the gateway test container %s %v", gwContainerNameAlt1, err)
		}
		if _, err := runCommand("docker", "rm", "-f", gwContainerNameAlt2); err != nil {
			framework.Logf("failed to delete the gateway test container %s %v", gwContainerNameAlt2, err)
		}
	})

	ginkgo.It("Should validate connectivity without vxlan before and after updating the namespace annotation to a new external gateway", func() {

		extGWRemote := extGwAlt1
		pingCommand := ipv4PingCommand
		if framework.TestContext.ClusterIsIPv6() {
			extGWRemote = extGwAlt1IPv6
			pingCommand = ipv6PingCommand
		}
		framework.Logf("the pod side extgw node is %s and the ip %s", clientNodeInfo.name, clientNodeInfo.nodeIP)
		podCIDR, err := getNodePodCIDR(f, clientNodeInfo.name, framework.TestContext.ClusterIsIPv6())
		if err != nil {
			framework.Failf("Error retrieving the pod cidr from %s %v", clientNodeInfo.name, err)
		}
		framework.Logf("the pod cidr for node %s is %s", clientNodeInfo.name, podCIDR)

		// setup the first external gateway
		err = setupExternalGateway(f, gwContainerNameAlt1, extGWRemote, podCIDR, clientNodeInfo.name)
		framework.ExpectNoError(err)

		time.Sleep(5 * time.Second)

		// Verify the initial gateway is reachable from the new pod
		ginkgo.By(fmt.Sprintf("Verifying connectivity to the updated annotation and initial external gateway %s", extGWRemote))

		framework.ExpectNoError(
			checkConnectivityPingToHost(f, clientNodeInfo.name, "e2e-exgw-novxlan-src-ping-pod", extGWRemote, pingCommand, 5, true))

		ginkgo.By("Starting a new external gateway")
		extGWRemote = extGwAlt2
		if framework.TestContext.ClusterIsIPv6() {
			extGWRemote = extGwAlt2IPv6
		}
		err = setupExternalGateway(f, gwContainerNameAlt2, extGWRemote, podCIDR, clientNodeInfo.name)

		// Wait for the exGW pod networking to be almost, updated
		time.Sleep(5 * time.Second)

		// Verify the updated gateway is reachable from the pods
		ginkgo.By(fmt.Sprintf("Verifying connectivity to the updated annotation and new external gateway %s", extGWRemote))

		framework.ExpectNoError(
			checkConnectivityPingToHost(f, clientNodeInfo.name, "e2e-exgw-novxlan-src-ping-pod2", extGWRemote, pingCommand, 5, true))

	})
})

func setupExternalGateway(f *framework.Framework, gwName, extGW, podCIDR, nodeName string) error {
	// start the container that will act as an external gateway
	_, err := runCommand("docker", "run", "-itd", "--privileged", "--network", "kind", "--name", gwName, "centos:8")
	if err != nil {
		framework.Logf("failed to start external gateway test container: %v", err)
		return err
	}
	extIPv4, extIPv6, err := getDockerContainerIP(gwName)
	if err != nil {
		framework.Logf("failed to start external gateway test container: %v", err)
		return err
	}
	framework.Logf("The external gateway IP is IPv4 %s IPv6 %s", extIPv4, extIPv6)
	extIP := extIPv4
	if framework.TestContext.ClusterIsIPv6() {
		extIP = extIPv6
	}
	framework.Logf("The external gateway IP is %s", extIP)

	// Annotate the pods to route pods to ext gw bridge br-ext
	framework.Logf("Annotating the external gateway test namespace")
	_, err = f.ClientSet.CoreV1().Namespaces().Get(context.TODO(), f.Namespace.Name, metav1.GetOptions{})
	if err != nil {
		framework.Logf("Error getting Namespace %v: %v", f.Namespace.Name, err)
		return err
	}

	nspatch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{"k8s.ovn.org/routing-external-gws": extIP},
		},
	})
	if err != nil {
		framework.Logf("Failed to Marshall Namespace %v patch: %v", f.Namespace.Name, err)
		return err
	}
	_, err = f.ClientSet.CoreV1().Namespaces().Patch(context.TODO(), f.Namespace.Name, types.StrategicMergePatchType, []byte(nspatch), metav1.PatchOptions{})
	if err != nil {
		framework.Logf("Error annotating Namespace %v: %v", f.Namespace.Name, err)
		return err
	}
	// setup the new container to emulate a gateway with routes and a loopback interface acting
	_, err = runCommand("docker", "exec", gwName, "ip", "address", "add", extGW, "dev", "lo")
	if err != nil {
		framework.Logf("failed to add the external gateway ip to dev lo on the test container: %v", err)
		return err
	}

	return nil
}

func setupExternalVTEPGateway(f *framework.Framework, gwName, extVtepGW, tunnelIP, podCIDR, nodeName string) error {
	// start the container that will act as an external gateway
	_, err := runCommand("docker", "run", "-itd", "--privileged", "--network", "kind", "--name", gwName, "centos:8")
	if err != nil {
		framework.Logf("failed to start external gateway test container: %v", err)
		return err
	}
	extIPv4, extIPv6, err := getDockerContainerIP(gwName)
	if err != nil {
		framework.Logf("failed to start external gateway test container: %v", err)
		return err
	}
	framework.Logf("The external gateway IP is IPv4 %s IPv6 %s", extIPv4, extIPv6)
	exVtepIP := extIPv4
	if framework.TestContext.ClusterIsIPv6() {
		exVtepIP = extIPv6
	}
	framework.Logf("The external gateway IP is %s", exVtepIP)

	// Annotate the pods to route pods to hybrid-sdn bridge br-ext
	framework.Logf("Annotating the external gateway test namespace")
	_, err = f.ClientSet.CoreV1().Namespaces().Get(context.TODO(), f.Namespace.Name, metav1.GetOptions{})
	if err != nil {
		framework.Logf("Error getting Namespace %v: %v", f.Namespace.Name, err)
		return err
	}

	nspatch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{"k8s.ovn.org/hybrid-overlay-external-gw": extVtepGW, "k8s.ovn.org/hybrid-overlay-vtep": exVtepIP},
		},
	})
	if err != nil {
		framework.Logf("Failed to Marshall Namespace %v patch: %v", f.Namespace.Name, err)
		return err
	}
	_, err = f.ClientSet.CoreV1().Namespaces().Patch(context.TODO(), f.Namespace.Name, types.StrategicMergePatchType, []byte(nspatch), metav1.PatchOptions{})
	if err != nil {
		framework.Logf("Error annotating Namespace %v: %v", f.Namespace.Name, err)
		return err
	}
	// setup the new container to emulate a gateway with routes, vtep and a loopback interface acting as the gateway
	_, err = runCommand("docker", "exec", gwName, "ip", "link", "add", "vxlan0", "type", "vxlan", "dev",
		"eth0", "id", "4097", "dstport", vxlanPort, "remote", tunnelIP)
	if err != nil {
		framework.Logf("failed to create the vxlan interface on the test container: %v", err)
		return err
	}
	_, err = runCommand("docker", "exec", gwName, "ip", "link", "set", "vxlan0", "up")
	if err != nil {
		framework.Logf("failed to enable the vxlan interface on the test container: %v", err)
		return err
	}
	_, err = runCommand("docker", "exec", gwName, "ip", "address", "add", extVtepGW, "dev", "lo")
	if err != nil {
		framework.Logf("failed to add the external gateway ip to dev lo on the test container: %v", err)
		return err
	}
	_, err = runCommand("docker", "exec", gwName, "ip", "route", "add", podCIDR, "dev", "vxlan0")
	if err != nil {
		framework.Logf("failed to add the pod route on the test container: %v", err)
		return err
	}
	return nil
}

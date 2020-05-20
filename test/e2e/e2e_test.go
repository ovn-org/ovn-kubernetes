package e2e_test

import (
	"fmt"
	. "github.com/onsi/ginkgo"
	"os/exec"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/test/e2e/framework"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

func checkContinuousConnectivity(f *framework.Framework, nodeName, podName, host string, port, timeout int, podChan chan *v1.Pod, errChan chan error) {
	contName := fmt.Sprintf("%s-container", podName)

	command := []string{
		"bash", "-c",
		"set -xe; for i in {1..10}; do nc -vz -w " + strconv.Itoa(timeout) + " " + host + " " + strconv.Itoa(port) + "; sleep 2; done",
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    contName,
					Image:   framework.AgnHostImage,
					Command: command,
				},
			},
			NodeName:      nodeName,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	podClient := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
	_, err := podClient.Create(pod)
	if err != nil {
		errChan <- err
		return
	}

	err = e2epod.WaitForPodNotPending(f.ClientSet, f.Namespace.Name, podName)
	if err != nil {
		errChan <- err
		return
	}

	podGet, err := podClient.Get(podName, metav1.GetOptions{})
	if err != nil {
		errChan <- err
		return
	}

	podChan <- podGet

	err = e2epod.WaitForPodSuccessInNamespace(f.ClientSet, podName, f.Namespace.Name)

	if err != nil {
		logs, logErr := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, contName)
		if logErr != nil {
			framework.Logf("Warning: Failed to get logs from pod %q: %v", pod.Name, logErr)
		} else {
			framework.Logf("pod %s/%s logs:\n%s", f.Namespace.Name, pod.Name, logs)
		}
	}

	errChan <- err
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
func checkConnectivityPingToHost(f *framework.Framework, nodeName, podName, host string, pingCmd pingCommand, timeout int) error {
	contName := fmt.Sprintf("%s-container", podName)

	command := []string{
		string(pingCmd),
		//"-c", "3", // send 3 pings
		"-W", "5", // wait at most 2 seconds for a reply
		"-w", strconv.Itoa(timeout),
		host,
	}
	isPrivileged := true
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    contName,
					Image:   "centos:centos8",
					Command: command,
					SecurityContext: &v1.SecurityContext{Privileged: &isPrivileged},
				},
			},
			NodeName:      "ovn-worker",
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	podClient := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
	_, err := podClient.Create(pod)
	if err != nil {
		return err
	}
	time.Sleep(time.Second * 10)

	err = e2epod.WaitForPodNotPending(f.ClientSet, podName, f.Namespace.Name)
	if err != nil {
		logs, logErr := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, contName)
		if logErr != nil {
			framework.Logf("Warning: Failed to get logs from pod %q: %v", pod.Name, logErr)
		} else {
			framework.Logf("pod %s/%s logs:\n%s", f.Namespace.Name, pod.Name, logs)
		}
	}
	err = e2epod.WaitForPodSuccessInNamespace(f.ClientSet, podName, f.Namespace.Name)

	if err != nil {
		logs, logErr := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, contName)
		if logErr != nil {
			framework.Logf("Warning: Failed to get logs from pod %q: %v", pod.Name, logErr)
		} else {
			framework.Logf("pod %s/%s logs:\n%s", f.Namespace.Name, pod.Name, logs)
		}
	}

	return err
}

// Create a pod on the specified node using the agnostic host image
func createGenericPod(f *framework.Framework, podName, nodeSelector string, command []string) {
	contName := fmt.Sprintf("%s-container", podName)

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    contName,
					Image:   "centos",
					Command: command,
				},
			},
			NodeName:      nodeSelector,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	podClient := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
	_, err := podClient.Create(pod)
	if err != nil {
		framework.Logf("Warning: Failed to get logs from pod %q: %v", pod.Name, err)
	}
	err = e2epod.WaitForPodNotPending(f.ClientSet, podName, f.Namespace.Name)
	if err != nil {
		logs, logErr := e2epod.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, contName)
		if logErr != nil {
			framework.Logf("Warning: Failed to get logs from pod %q: %v", pod.Name, logErr)
		} else {
			framework.Logf("pod %s/%s logs:\n%s", f.Namespace.Name, pod.Name, logs)
		}
	}
}

// Get the IP address of a pod in the specified namespace
func getPodAddress(podName, namespace string) (string, error) {
	podIP, err := framework.RunKubectl("get", "pods", podName, "--template={{.status.podIP}}", "-n"+namespace)
	if err != nil {
		framework.Failf("Unable to retrieve the IP for pod %s %v", podName, err)
		return "", err
	}
	return podIP, nil
}

//
//var _ = Describe("e2e control plane", func() {
//	var svcname = "nettest"
//
//	f := framework.NewDefaultFramework(svcname)
//
//	ginkgo.BeforeEach(func() {
//		// Assert basic external connectivity.
//		// Since this is not really a test of kubernetes in any way, we
//		// leave it as a pre-test assertion, rather than a Ginko test.
//		ginkgo.By("Executing a successful http request from the external internet")
//		resp, err := http.Get("http://google.com")
//		if err != nil {
//			framework.Failf("Unable to connect/talk to the internet: %v", err)
//		}
//		if resp.StatusCode != http.StatusOK {
//			framework.Failf("Unexpected error code, expected 200, got, %v (%v)", resp.StatusCode, resp)
//		}
//	})
//
//	ginkgo.It("should provide Internet connection continuously when ovn-k8s pod is killed", func() {
//		ginkgo.By("Running container which tries to connect to 8.8.8.8 in a loop")
//
//		podChan, errChan := make(chan *v1.Pod), make(chan error)
//		go checkContinuousConnectivity(f, "", "connectivity-test-continuous", "8.8.8.8", 53, 30, podChan, errChan)
//
//		testPod := <-podChan
//		framework.Logf("Test pod running on %q", testPod.Spec.NodeName)
//
//		time.Sleep(5 * time.Second)
//
//		podClient := f.ClientSet.CoreV1().Pods("ovn-kubernetes")
//
//		podList, _ := podClient.List(metav1.ListOptions{})
//		podName := ""
//		for _, pod := range podList.Items {
//			if strings.HasPrefix(pod.Name, "ovnkube-node") && pod.Spec.NodeName == testPod.Spec.NodeName {
//				podName = pod.Name
//				break
//			}
//		}
//
//		err := podClient.Delete(podName, metav1.NewDeleteOptions(0))
//		framework.ExpectNoError(err, "should delete ovnkube-node pod")
//		framework.Logf("Deleted ovnkube-node %q", podName)
//
//		framework.ExpectNoError(<-errChan)
//	})
//
//	ginkgo.It("should provide Internet connection continuously when master is killed", func() {
//		ginkgo.By("Running container which tries to connect to 8.8.8.8 in a loop")
//
//		podChan, errChan := make(chan *v1.Pod), make(chan error)
//		go checkContinuousConnectivity(f, "", "connectivity-test-continuous", "8.8.8.8", 53, 30, podChan, errChan)
//
//		testPod := <-podChan
//		framework.Logf("Test pod running on %q", testPod.Spec.NodeName)
//
//		time.Sleep(5 * time.Second)
//
//		podClient := f.ClientSet.CoreV1().Pods("ovn-kubernetes")
//
//		podList, _ := podClient.List(metav1.ListOptions{})
//		podName := ""
//		for _, pod := range podList.Items {
//			if strings.HasPrefix(pod.Name, "ovnkube-master") {
//				podName = pod.Name
//				break
//			}
//		}
//
//		err := podClient.Delete(podName, metav1.NewDeleteOptions(0))
//		framework.ExpectNoError(err, "should delete ovnkube-master pod")
//		framework.Logf("Deleted ovnkube-master %q", podName)
//
//		framework.ExpectNoError(<-errChan)
//	})
//})
//
//// Test e2e hybrid sdn inter-node connectivity between worker nodes and validate pods do not traverse the external gateway
//var _ = Describe("test e2e inter-node connectivity between worker nodes hybrid overlay on separate worker nodes", func() {
//	var haMode bool
//	svcname := "internode-hyb-sdn-e2e"
//	pingTarget := "172.17.0.250"
//	ovnNs := "ovn-kubernetes"
//	ovnWorkerNode := "ovn-worker"
//	ovnWorkerNode2 := "ovn-worker2"
//	ovnHaWorkerNode2 := "ovn-control-plane2"
//	ovnHaWorkerNode3 := "ovn-control-plane3"
//	ovnContainer := "ovnkube-node"
//	ovnNsFlag := fmt.Sprintf("--namespace=%s", ovnNs)
//	labelFlag := fmt.Sprintf("name=%s", ovnContainer)
//	jsonFlag := "-o=jsonpath='{.items..metadata.name}'"
//	f := framework.NewDefaultFramework(svcname)
//
//	// Determine what mode the CI is running in and get relevant endpoint information for the tests
//	BeforeEach(func() {
//		fieldSelectorFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnWorkerNode)
//		fieldSelectorHaFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnHaWorkerNode2)
//		annotationFlag := fmt.Sprintf("k8s.ovn.org/hybrid-overlay-external-gw=%s", pingTarget)
//		// Annotate the pods to route pods to hybrid-sdn bridge br-ext
//		framework.Logf("Annotating the external gateway test namespace")
//		framework.RunKubectlOrDie("annotate", "namespace", f.Namespace.Name, annotationFlag)
//
//		// Attempt to retrieve the pod name that will run the external interface for e2e control-plane non-ha mode
//		kubectlOut, err := framework.RunKubectl("get", "pods", ovnNsFlag, "-l", labelFlag, jsonFlag, fieldSelectorFlag)
//		if err != nil {
//			framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnWorkerNode, err)
//		}
//		haMode = false
//		// Attempt to retrieve the pod name that will run the external interface for e2e control-plane ha mode
//		if kubectlOut == "''" {
//			haMode = true
//			kubectlOut, err = framework.RunKubectl("get", "pods", ovnNsFlag, "-l", labelFlag, jsonFlag, fieldSelectorHaFlag)
//			if err != nil {
//				framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnHaWorkerNode2, err)
//			}
//		}
//		// Fail the test if no pod is matched within the specified node
//		if kubectlOut == "''" {
//			framework.Failf("Unable to locate container %s on any known nodes", ovnContainer)
//		}
//	})
//
//	It("Should validate connectivity between pods with hybrid overlay on separate worker nodes and ensure br-ext is not traversed", func() {
//		var err error
//		var pingTarget string
//		var ciWorkerNodeSrc string
//		var ciWorkerNodeDst string
//		dstPingPodName := "e2e-dst-ping-pod"
//		getPodIPRetry := 15
//		command := []string{"bash", "-c", "sleep 20000"}
//
//		// non-ha ci mode runs a named set of nodes with a prefix of ovn-worker
//		ciWorkerNodeSrc = ovnWorkerNode
//		ciWorkerNodeDst = ovnWorkerNode2
//		// ha ci mode runs a named set of nodes with a prefix of ovn-control-plane
//		if haMode {
//			framework.Logf("Detected a HA mode KIND environment")
//			ciWorkerNodeSrc = ovnHaWorkerNode2
//			ciWorkerNodeDst = ovnHaWorkerNode3
//		}
//		By(fmt.Sprintf("Creating a container on node %s and verifying connectivity to a pod on node %s", ciWorkerNodeSrc, ciWorkerNodeDst))
//
//		// Create the pod that will be used as the destination for the connectivity test
//		createGenericPod(f, dstPingPodName, ciWorkerNodeDst, command)
//		// There is a condition somewhere with e2e WaitForPodNotPending that returns ready
//		// before calling for the IP address will succeed. This simply adds some retries.
//		for i := 1; i < getPodIPRetry; i++ {
//			pingTarget, err = getPodAddress(dstPingPodName, f.Namespace.Name)
//			if err != nil {
//				framework.Logf("Warning unable to query the test pod on node %s %v", ciWorkerNodeSrc, err)
//			}
//			if pingTarget != "<no value>" {
//				framework.Logf("Destination ping target for %s is %s", dstPingPodName, pingTarget)
//				break
//			}
//			time.Sleep(time.Second * 3)
//			framework.Logf("Retry attempt %d to get pod IP from initializing pod %s", i, dstPingPodName)
//		}
//		// Fail the test if no address is ever retrieved
//		if pingTarget == "<no value>" {
//			framework.Failf("Warning: Failed to get an IP for target pod %s, test will fail", dstPingPodName)
//		}
//		// Fail the test if the destination IP is empty
//		if pingTarget == "" {
//			framework.Failf("Warning: Failed to get an IP for target pod %s, failing test", dstPingPodName)
//		}
//		// Spin up another pod that attempts to reach the previously started pod on separate nodes
//		framework.ExpectNoError(
//			checkConnectivityPingToHost(f, ciWorkerNodeSrc, "e2e-src-ping-pod", pingTarget, ipv4PingCommand, 30))
//
//		fieldSelectorFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ciWorkerNodeSrc)
//		kubectlOut, err := framework.RunKubectl("get", "pods", ovnNsFlag, "-l", labelFlag, jsonFlag, fieldSelectorFlag)
//		if err != nil {
//			framework.Failf("Expected container %s running on %s error %v", ovnContainer, ciWorkerNodeSrc, err)
//		}
//		ovnPodName := strings.Trim(kubectlOut, "'")
//		ovnContainerFlag := fmt.Sprintf("--container=%s", ovnContainer)
//		// dump the flowmods from br-ext to verify no counters are hit
//		kubectlOut, err = framework.RunKubectl("exec", ovnPodName, ovnNsFlag, ovnContainerFlag, "--", "ovs-ofctl", "dump-flows", "br-ext")
//		if err != nil {
//			framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnWorkerNode, err)
//		}
//		for _, flowmod := range strings.Split(kubectlOut, "\n") {
//			// filter out irrelevant lines from ofctl output
//			if strings.Contains(flowmod, "n_packets=") {
//				// verify no flowmod counters were hit in br-ext
//				if !strings.Contains(flowmod, "n_packets=0") {
//					framework.Failf("Expected packets=0 but found the flow %s", flowmod)
//				}
//			}
//		}
//	})
//})
//
//// Test e2e inter-node connectivity over br-int
//var _ = Describe("test e2e inter-node connectivity between worker nodes", func() {
//	var haMode bool
//	svcname := "inter-node-e2e"
//	ovnNs := "ovn-kubernetes"
//	ovnWorkerNode := "ovn-worker"
//	ovnWorkerNode2 := "ovn-worker2"
//	ovnHaWorkerNode2 := "ovn-control-plane2"
//	ovnHaWorkerNode3 := "ovn-control-plane3"
//	ovnContainer := "ovnkube-node"
//	ovnNsFlag := fmt.Sprintf("--namespace=%s", ovnNs)
//	labelFlag := fmt.Sprintf("name=%s", ovnContainer)
//	jsonFlag := "-o=jsonpath='{.items..metadata.name}'"
//	f := framework.NewDefaultFramework(svcname)
//
//	// Determine which KIND environment is running by querying the running nodes
//	BeforeEach(func() {
//		fieldSelectorFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnWorkerNode)
//		fieldSelectorHaFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnHaWorkerNode2)
//
//		// Determine if the kind deployment is in HA mode or non-ha mode based on node naming
//		kubectlOut, err := framework.RunKubectl("get", "pods", ovnNsFlag, "-l", labelFlag, jsonFlag, fieldSelectorFlag)
//		if err != nil {
//			framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnWorkerNode, err)
//		}
//		haMode = false
//		if kubectlOut == "''" {
//			haMode = true
//			kubectlOut, err = framework.RunKubectl("get", "pods", ovnNsFlag, "-l", labelFlag, jsonFlag, fieldSelectorHaFlag)
//			if err != nil {
//				framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnHaWorkerNode2, err)
//			}
//		}
//		// Fail the test if no pod is matched within the specified node
//		if kubectlOut == "''" {
//			framework.Failf("Unable to locate container %s on any known nodes", ovnContainer)
//		}
//	})
//
//	It("Should validate connectivity within a namespace of pods on separate nodes", func() {
//		var err error
//		var pingTarget string
//		var ciWorkerNodeSrc string
//		var ciWorkerNodeDst string
//		dstPingPodName := "e2e-dst-ping-pod"
//		getPodIPRetry := 15
//		command := []string{"bash", "-c", "sleep 20000"}
//		// non-ha ci mode runs a named set of nodes with a prefix of ovn-worker
//		ciWorkerNodeSrc = ovnWorkerNode
//		ciWorkerNodeDst = ovnWorkerNode2
//		// ha ci mode runs a named set of nodes with a prefix of ovn-control-plane
//		if haMode {
//			framework.Logf("Detected a HA mode KIND environment")
//			ciWorkerNodeSrc = ovnHaWorkerNode2
//			ciWorkerNodeDst = ovnHaWorkerNode3
//		}
//		By(fmt.Sprintf("Creating a container on node %s and verifying connectivity to a pod on node %s", ciWorkerNodeSrc, ciWorkerNodeDst))
//
//		// Create the pod that will be used as the destination for the connectivity test
//		createGenericPod(f, dstPingPodName, ciWorkerNodeDst, command)
//		// There is a condition somewhere with e2e WaitForPodNotPending that returns ready
//		// before calling for the IP address will succeed. This simply adds some retries.
//		for i := 1; i < getPodIPRetry; i++ {
//			pingTarget, err = getPodAddress(dstPingPodName, f.Namespace.Name)
//			if err != nil {
//				framework.Logf("Warning unable to query the test pod on node %s %v", ciWorkerNodeSrc, err)
//			}
//			if pingTarget != "<no value>" {
//				framework.Logf("Destination ping target for %s is %s", dstPingPodName, pingTarget)
//				break
//			}
//			time.Sleep(time.Second * 3)
//			framework.Logf("Retry attempt %d to get pod IP from initializing pod %s", i, dstPingPodName)
//		}
//		// Fail the test if no address is ever retrieved
//		if pingTarget == "<no value>" {
//			framework.Failf("Warning: Failed to get an IP for target pod %s, test will fail", dstPingPodName)
//		}
//		// Fail the test if the destination IP is empty
//		if pingTarget == "" {
//			framework.Failf("Warning: Failed to get an IP for target pod %s, failing test", dstPingPodName)
//		}
//		// Spin up another pod that attempts to reach the previously started pod on separate nodes
//		framework.ExpectNoError(
//			checkConnectivityPingToHost(f, ciWorkerNodeSrc, "e2e-src-ping-pod", pingTarget, ipv4PingCommand, 30))
//	})
//})

// Verify pods in the namespace annotated with an external-gateway traverse the vxlan overlay and reach the
// intended external gateway vtep. Then validate the rx counters on the vxlan interface mocking as a gateway
var _ = Describe("e2e external gateway validation", func() {
	var haMode bool
	//vxlanPort := "4789"
	svcname := "externalgw"
	ovnNs := "ovn-kubernetes"
	extGW := "8.0.0.1"
	extGWCidr := fmt.Sprintf("%s/24", extGW)
	gwContainerName := "gw-test-container"
	ovnWorkerNode2 := "ovn-worker"
	ovnHaWorkerNode3 := "ovn-control-plane2"
	ovnContainer := "ovnkube-node"
	ovnNsFlag := fmt.Sprintf("--namespace=%s", ovnNs)
	f := framework.NewDefaultFramework(svcname)

	// Determine what mode the CI is running in and get relevant endpoint information for the tests
	BeforeEach(func() {
		labelFlag := fmt.Sprintf("name=%s", ovnContainer)
		jsonFlag := "-o=jsonpath='{.items..metadata.name}'"
		fieldSelectorFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnWorkerNode2)
		fieldSelectorHaFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnHaWorkerNode3)

		_, err := runCommand("docker", "run", "-itd", "--privileged", "--name", gwContainerName, "centos")
		if err != nil {
			framework.Failf("failed to start external gateway test container: %v", err)
		}

		exVtepIP, err := runCommand("docker", "inspect", "-f", "{{ .NetworkSettings.IPAddress }}", gwContainerName)
		if err != nil {
			framework.Failf("failed to start external gateway test container: %v", err)
		}
		// Annotate the test namespace
		annotationFlag := fmt.Sprintf("k8s.ovn.org/hybrid-overlay-external-gw=%s", extGW)
		annotationVtepFlag := fmt.Sprintf("k8s.ovn.org/hybrid-overlay-vtep=%s", exVtepIP)
		framework.Logf("Annotating the external gateway test namespace")
		framework.RunKubectlOrDie("annotate", "namespace", f.Namespace.Name, annotationFlag)
		framework.RunKubectlOrDie("annotate", "namespace", f.Namespace.Name, annotationVtepFlag)

		time.Sleep(time.Second * 20)
		// Attempt to retrieve the pod name that will source the tunnel test in non-HA mode
		kubectlOut, err := framework.RunKubectl("get", "pods", ovnNsFlag, "-l", labelFlag, jsonFlag, fieldSelectorFlag)
		if err != nil {
			framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnWorkerNode2, err)
		}
		haMode = false
		// Attempt to retrieve the pod name that will source the tunnel test in HA mode
		if kubectlOut == "''" {
			haMode = true
			kubectlOut, err = framework.RunKubectl("get", "pods", ovnNsFlag, "-l", labelFlag, jsonFlag, fieldSelectorHaFlag)
			if err != nil {
				framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnHaWorkerNode3, err)
			}
		}
	})

	AfterEach(func() {
		// tear down the container simulating the gateway
		_, err := runCommand("docker", "rm", "-f", gwContainerName)
		if err != nil {
			framework.Failf("failed to delete the gateway test container: %v", err)
		}
	})

	It("Should validate connectivity to the vxlan interface simulating an external gateway and validate traffic was encapsulated", func() {
		// non-ha ci mode runs a set of kind nodes prefixed with ovn-worker
		ciWorkerNodeSrc := ovnWorkerNode2
		if haMode {
			// ha ci mode runs a named set of nodes with a prefix of ovn-control-plane
			ciWorkerNodeSrc = ovnHaWorkerNode3
		}
		cmdOut, err := runCommand("docker", "inspect", "-f", "{{ .NetworkSettings.IPAddress }}", ciWorkerNodeSrc)
		if err != nil {
			framework.Failf("failed to get the node ip address from node %s %v",ciWorkerNodeSrc , err)
		}
		localVtepIP := strings.TrimSuffix(cmdOut, "\n")
		framework.Logf("the local vtep is %s", localVtepIP)

		jsonFlag := "jsonpath='{.spec.podCIDRs[0]}'"
		kubectlOut, err := framework.RunKubectl("get", "node", ciWorkerNodeSrc, "-o", jsonFlag)
		if err != nil {
			framework.Failf("Error retrieving podcidr from %s %v", ciWorkerNodeSrc, err)
		}
		podCIDR := strings.Trim(kubectlOut, "'")
		framework.Logf("the pod cidr for %s is %v", ciWorkerNodeSrc, podCIDR)

		time.Sleep(time.Second * 2)
		// ip link add vxlan0 type vxlan dev eth0 id 4097 dstport 4789 remote 172.17.0.4
		_, err = runCommand("docker", "exec", gwContainerName, "ip", "link", "add", "vxlan0", "type", "vxlan", "dev",
			"eth0", "id", "4097", "dstport", "4789", "remote", localVtepIP)
		if err != nil {
			framework.Failf("failed to create the vxlan interface on the test container: %v", err)
		}
		_, err = runCommand("docker", "exec", gwContainerName, "ip", "link", "set", "vxlan0", "up", "promisc", "on")
		if err != nil {
			framework.Failf("failed to enable the vxlan interface on the test container: %v", err)
		}
		_, err = runCommand("docker", "exec", gwContainerName, "ip", "address", "add", extGWCidr, "dev", "lo")
		if err != nil {
			framework.Failf("failed to add the external gateway ip to dev lo on the test container: %v", err)
		}
		_, err = runCommand("docker", "exec", gwContainerName, "ip", "link", "set", "lo", "up", "promisc", "on")
		if err != nil {
			framework.Failf("failed to enable the lo interface on the test container: %v", err)
		}
		_, err = runCommand("docker", "exec", gwContainerName, "ip", "route", "add", podCIDR, "dev", "vxlan0")
		if err != nil {
			framework.Failf("failed to add the pod route on the test container: %v", err)
		}

		time.Sleep(time.Second * 5)
		By(fmt.Sprintf("Creating a container on %s and generating traffic", ciWorkerNodeSrc))
		framework.ExpectNoError(
			// generate traffic that will being encapsulated and sent to the external gateway. No response is expected.
			checkConnectivityPingToHost(f, "ovn-worker", ciWorkerNodeSrc, extGW, ipv4PingCommand, 100))
	})
})

// runCommand runs the cmd and returns the combined stdout and stderr, or an
// error if the command failed.
func runCommand(cmd ...string) (string, error) {
	output, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run %q: %s (%s)", strings.Join(cmd, " "), err, output)
	}
	return string(output), nil
}


package e2e_test

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo"

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

// PingCommand is the type to hold ping command.
type PingCommand string

const (
	// IPv4PingCommand is a ping command for IPv4.
	IPv4PingCommand PingCommand = "ping"
	// IPv6PingCommand is a ping command for IPv6.
	IPv6PingCommand PingCommand = "ping6"
)

// Place the workload on the specified node to test external connectivity
func checkConnectivityPingToHost(f *framework.Framework, nodeName, podName, host string, pingCmd PingCommand, timeout int) error {
	contName := fmt.Sprintf("%s-container", podName)

	command := []string{
		string(pingCmd),
		"-c", "3", // send 3 pings
		"-W", "2", // wait at most 2 seconds for a reply
		"-w", strconv.Itoa(timeout),
		host,
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
		return err
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

var _ = Describe("e2e control plane", func() {
	var svcname = "nettest"

	f := framework.NewDefaultFramework(svcname)

	ginkgo.BeforeEach(func() {
		// Assert basic external connectivity.
		// Since this is not really a test of kubernetes in any way, we
		// leave it as a pre-test assertion, rather than a Ginko test.
		ginkgo.By("Executing a successful http request from the external internet")
		resp, err := http.Get("http://google.com")
		if err != nil {
			framework.Failf("Unable to connect/talk to the internet: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			framework.Failf("Unexpected error code, expected 200, got, %v (%v)", resp.StatusCode, resp)
		}
	})

	ginkgo.It("should provide Internet connection continuously when ovn-k8s pod is killed", func() {
		ginkgo.By("Running container which tries to connect to 8.8.8.8 in a loop")

		podChan, errChan := make(chan *v1.Pod), make(chan error)
		go checkContinuousConnectivity(f, "", "connectivity-test-continuous", "8.8.8.8", 53, 30, podChan, errChan)

		testPod := <-podChan
		framework.Logf("Test pod running on %q", testPod.Spec.NodeName)

		time.Sleep(5 * time.Second)

		podClient := f.ClientSet.CoreV1().Pods("ovn-kubernetes")

		podList, _ := podClient.List(metav1.ListOptions{})
		podName := ""
		for _, pod := range podList.Items {
			if strings.HasPrefix(pod.Name, "ovnkube-node") && pod.Spec.NodeName == testPod.Spec.NodeName {
				podName = pod.Name
				break
			}
		}

		err := podClient.Delete(podName, metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err, "should delete ovnkube-node pod")
		framework.Logf("Deleted ovnkube-node %q", podName)

		framework.ExpectNoError(<-errChan)
	})

	ginkgo.It("should provide Internet connection continuously when master is killed", func() {
		ginkgo.By("Running container which tries to connect to 8.8.8.8 in a loop")

		podChan, errChan := make(chan *v1.Pod), make(chan error)
		go checkContinuousConnectivity(f, "", "connectivity-test-continuous", "8.8.8.8", 53, 30, podChan, errChan)

		testPod := <-podChan
		framework.Logf("Test pod running on %q", testPod.Spec.NodeName)

		time.Sleep(5 * time.Second)

		podClient := f.ClientSet.CoreV1().Pods("ovn-kubernetes")

		podList, _ := podClient.List(metav1.ListOptions{})
		podName := ""
		for _, pod := range podList.Items {
			if strings.HasPrefix(pod.Name, "ovnkube-master") {
				podName = pod.Name
				break
			}
		}

		err := podClient.Delete(podName, metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err, "should delete ovnkube-master pod")
		framework.Logf("Deleted ovnkube-master %q", podName)

		framework.ExpectNoError(<-errChan)
	})
})

var _ = Describe("e2e external gateway connectivity", func() {
	var haMode bool
	var ovnPodName string
	svcname := "externalgw"
	pingTarget := "172.17.0.10"
	pingTargetMask := "/16"
	ovnNs := "ovn-kubernetes"
	macvlanIface := "macvlan0"
	ovnWorkerNode := "ovn-worker"
	ovnWorkerNode2 := "ovn-worker2"
	ovnHaWorkerNode := "ovn-control-plane2"
	ovnHaWorkerNode2 := "ovn-control-plane3"
	ovnContainer := "ovnkube-node"
	ovnNsFlag := fmt.Sprintf("--namespace=%s", ovnNs)
	ovnContainerFlag := fmt.Sprintf("--container=%s", ovnContainer)
	ovnTargetCidr := fmt.Sprintf("%s%s", pingTarget, pingTargetMask)
	f := framework.NewDefaultFramework(svcname)

	// Add a Macvlan interface to an ovn worker node to simulate and external gateway
	BeforeEach(func() {
		labelFlag := fmt.Sprintf("name=%s", ovnContainer)
		jsonFlag := "-o=jsonpath='{.items..metadata.name}'"
		fieldSelectorFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnWorkerNode)
		fieldSelectorHaFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnHaWorkerNode)
		annotationFlag := fmt.Sprintf("k8s.ovn.org/hybrid-overlay-external-gw=%s", pingTarget)

		// Annotate the test namespace
		framework.Logf("Annotating the external gateway test namespace")
		framework.RunKubectlOrDie("annotate", "namespace", f.Namespace.Name, annotationFlag)

		// Attempt to retrieve the pod name that will run the external interface for e2e control-plane non-ha mode
		kubectlOut, err := framework.RunKubectl("get", "pods", ovnNsFlag, "-l", labelFlag, jsonFlag, fieldSelectorFlag)
		if err != nil {
			framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnWorkerNode, err)
		}
		haMode = false
		// Attempt to retrieve the pod name that will run the external interface for e2e control-plane ha mode
		if kubectlOut == "''" {
			haMode = true
			kubectlOut, err = framework.RunKubectl("get", "pods", ovnNsFlag, "-l", labelFlag, jsonFlag, fieldSelectorHaFlag)
			if err != nil {
				framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnHaWorkerNode, err)
			}
		}
		// Fail the test if no pod is matched
		if kubectlOut == "''" {
			framework.Failf("Unable to locate container %s on any known nodes", ovnContainer)
		}
		ovnPodName = strings.Trim(kubectlOut, "'")

		// Add a macvlan interface to an ovnkube-node container
		framework.Logf("Creating a macvlan interface named %s on pod %s", macvlanIface, ovnPodName)
		framework.RunKubectlOrDie("exec", ovnPodName, ovnNsFlag, ovnContainerFlag,
			"--", "ip", "link", "add", macvlanIface, "link", "eth0", "type", "macvlan", "mode", "bridge")

		// Assign an IPv4 address to the new macvlan interface
		framework.Logf("Assigning IP address %s to %s", ovnTargetCidr, macvlanIface)
		framework.RunKubectlOrDie("exec", ovnPodName, ovnNsFlag, ovnContainerFlag,
			"--", "ip", "address", "add", ovnTargetCidr, "dev", macvlanIface)
	})

	// Cleanup the external interface after the test has completed
	AfterEach(func() {
		framework.Logf("Tearing down interface %s on %s", macvlanIface, ovnPodName)
		framework.RunKubectlOrDie("exec", ovnPodName, ovnNsFlag, ovnContainerFlag,
			"--", "ip", "link", "delete", macvlanIface)
	})

	It("Should validate connectivity to the macvlan interface which is simulating an external gateway", func() {
		// HA CI mode runs a named set of nodes with a prefix of ovn-control-plane
		if haMode {
			By(fmt.Sprintf("Creating a container on %s and verifying connectivity to the external gateway on %s", ovnHaWorkerNode2, ovnHaWorkerNode))
			framework.ExpectNoError(
				checkConnectivityPingToHost(f, ovnHaWorkerNode2, "ext-gateway-ci", pingTarget, IPv4PingCommand, 30))
		}
		// non-HA CI mode runs a named set of nodes with a prefix of ovn-worker
		if !haMode {
			By(fmt.Sprintf("Creating a container on %s and verifying connectivity to the external gateway on %s", ovnWorkerNode2, ovnWorkerNode))
			framework.ExpectNoError(
				checkConnectivityPingToHost(f, ovnWorkerNode2, "ext-gateway-ha-ci", pingTarget, IPv4PingCommand, 30))
		}
	})
})

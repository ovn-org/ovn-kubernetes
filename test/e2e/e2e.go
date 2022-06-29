package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	utilnet "k8s.io/utils/net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2edeployment "k8s.io/kubernetes/test/e2e/framework/deployment"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	testutils "k8s.io/kubernetes/test/utils"
)

const (
	vxlanPort            = "4789" // IANA assigned VXLAN UDP port - rfc7348
	podNetworkAnnotation = "k8s.ovn.org/pod-networks"
	retryInterval        = 1 * time.Second  // polling interval timer
	retryTimeout         = 40 * time.Second // polling timeout
	dsRestartTimeout     = 10 * time.Minute // ds restart timeout
	agnhostImage         = "k8s.gcr.io/e2e-test-images/agnhost:2.26"
)

type podCondition = func(pod *v1.Pod) (bool, error)

func checkContinuousConnectivity(f *framework.Framework, nodeName, podName, host string, port, tries, timeout int, podChan chan *v1.Pod, errChan chan error) {
	contName := fmt.Sprintf("%s-container", podName)

	command := []string{
		"bash", "-c",
		fmt.Sprintf("set -xe; for i in {1..%d}; do nc -vz -w %d %s %d ; sleep 2; done",
			tries, timeout, host, port),
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    contName,
					Image:   agnhostImage,
					Command: command,
				},
			},
			NodeName:      nodeName,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	podClient := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
	_, err := podClient.Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		errChan <- err
		return
	}

	// Wait for pod network setup to be almost ready
	wait.PollImmediate(1*time.Second, 30*time.Second, func() (bool, error) {
		pod, err := podClient.Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		_, ok := pod.Annotations[podNetworkAnnotation]
		return ok, nil
	})

	err = e2epod.WaitForPodNotPending(f.ClientSet, f.Namespace.Name, podName)
	if err != nil {
		errChan <- err
		return
	}

	podGet, err := podClient.Get(context.Background(), podName, metav1.GetOptions{})
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
func checkConnectivityPingToHost(f *framework.Framework, nodeName, podName, host string, pingCmd pingCommand, timeout int, exGw bool) error {
	contName := fmt.Sprintf("%s-container", podName)
	// Ping options are:
	// -c sends 3 pings
	// -W wait at most 2 seconds for a reply
	// -w timeout
	command := []string{"/bin/sh", "-c"}
	args := []string{fmt.Sprintf("sleep 20; %s -c 3 -W 2 -w %s %s", string(pingCmd), strconv.Itoa(timeout), host)}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    contName,
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
	_, err := podClient.Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// Wait for pod network setup to be almost ready
	err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
		pod, err := podClient.Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		_, ok := pod.Annotations[podNetworkAnnotation]
		return ok, nil
	})
	// Fail the test if no pod annotation is retrieved
	if err != nil {
		framework.Failf("Error trying to get the pod annotation")
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

// Place the workload on the specified node and return pod gw route
func getPodGWRoute(f *framework.Framework, nodeName string, podName string) net.IP {
	command := []string{"bash", "-c", "sleep 20000"}
	contName := fmt.Sprintf("%s-container", podName)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    contName,
					Image:   agnhostImage,
					Command: command,
				},
			},
			NodeName:      nodeName,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	podClient := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
	_, err := podClient.Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		framework.Failf("Error trying to create pod")
	}

	// Wait for pod network setup to be almost ready
	wait.PollImmediate(1*time.Second, 30*time.Second, func() (bool, error) {
		podGet, err := podClient.Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if podGet.Annotations != nil && podGet.Annotations[podNetworkAnnotation] != "" {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		framework.Failf("Error trying to get the pod annotations")
	}

	podGet, err := podClient.Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		framework.Failf("Error trying to get the pod object")
	}
	annotation, err := unmarshalPodAnnotation(podGet.Annotations)
	if err != nil {
		framework.Failf("Error trying to unmarshal pod annotations")
	}

	return annotation.Gateways[0]
}

// Create a pod on the specified node using the agnostic host image
func createGenericPod(f *framework.Framework, podName, nodeSelector, namespace string, command []string) (*v1.Pod, error) {
	return createPod(f, podName, nodeSelector, namespace, command, nil)
}

// Create a pod on the specified node using the agnostic host image
func createGenericPodWithLabel(f *framework.Framework, podName, nodeSelector, namespace string, command []string, labels map[string]string) (*v1.Pod, error) {
	return createPod(f, podName, nodeSelector, namespace, command, labels)
}

func createServiceForPodsWithLabel(f *framework.Framework, namespace string, servicePort int32, targetPort string, serviceType string, labels map[string]string) (string, error) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "service-for-pods",
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Protocol:   v1.ProtocolTCP,
					TargetPort: intstr.Parse(targetPort),
					Port:       servicePort,
				},
			},
			Type:     v1.ServiceType(serviceType),
			Selector: labels,
		},
	}
	serviceClient := f.ClientSet.CoreV1().Services(namespace)
	res, err := serviceClient.Create(context.Background(), service, metav1.CreateOptions{})
	if err != nil {
		return "", errors.Wrapf(err, "Failed to create service %s %s", service.Name, namespace)
	}
	err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
		res, err = serviceClient.Get(context.Background(), service.Name, metav1.GetOptions{})
		return res.Spec.ClusterIP != "", err
	})
	if err != nil {
		return "", errors.Wrapf(err, "Failed to get service %s %s", service.Name, namespace)
	}
	return res.Spec.ClusterIP, nil
}

func createClusterExternalContainer(containerName string, containerImage string, dockerArgs []string, entrypointArgs []string) (string, string) {
	args := []string{"docker", "run", "-itd"}
	args = append(args, dockerArgs...)
	args = append(args, []string{"--name", containerName, containerImage}...)
	args = append(args, entrypointArgs...)
	_, err := runCommand(args...)
	if err != nil {
		framework.Failf("failed to start external test container: %v", err)
	}
	ipv4, err := runCommand("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", containerName)
	if err != nil {
		framework.Failf("failed to inspect external test container for its IP: %v", err)
	}
	ipv6, err := runCommand("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.GlobalIPv6Address}}{{end}}", containerName)
	if err != nil {
		framework.Failf("failed to inspect external test container for its IP (v6): %v", err)
	}
	return strings.Trim(ipv4, "\n"), strings.Trim(ipv6, "\n")
}

func deleteClusterExternalContainer(containerName string) {
	_, err := runCommand("docker", "rm", "-f", containerName)
	if err != nil {
		framework.Failf("failed to delete external test container, err: %v", err)
	}
}

func updateNamespace(f *framework.Framework, namespace *v1.Namespace) {
	_, err := f.ClientSet.CoreV1().Namespaces().Update(context.Background(), namespace, metav1.UpdateOptions{})
	framework.ExpectNoError(err, fmt.Sprintf("unable to update namespace: %s, err: %v", namespace.Name, err))
}

func updatePod(f *framework.Framework, pod *v1.Pod) {
	_, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Update(context.Background(), pod, metav1.UpdateOptions{})
	framework.ExpectNoError(err, fmt.Sprintf("unable to update pod: %s, err: %v", pod.Name, err))
}
func getPod(f *framework.Framework, podName string) *v1.Pod {
	pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), podName, metav1.GetOptions{})
	framework.ExpectNoError(err, fmt.Sprintf("unable to get pod: %s, err: %v", podName, err))
	return pod
}

// Create a pod on the specified node using the agnostic host image
func createPod(f *framework.Framework, podName, nodeSelector, namespace string, command []string, labels map[string]string, options ...func(*v1.Pod)) (*v1.Pod, error) {

	contName := fmt.Sprintf("%s-container", podName)

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   podName,
			Labels: labels,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    contName,
					Image:   agnhostImage,
					Command: command,
				},
			},
			NodeName:      nodeSelector,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	for _, o := range options {
		o(pod)
	}

	podClient := f.ClientSet.CoreV1().Pods(namespace)
	res, err := podClient.Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		framework.Logf("Warning: Failed to create pod %s %v", pod.Name, err)
		return nil, errors.Wrapf(err, "Failed to create pod %s %s", pod.Name, namespace)
	}

	err = e2epod.WaitForPodRunningInNamespace(f.ClientSet, res)

	if err != nil {
		logs, logErr := e2epod.GetPodLogs(f.ClientSet, namespace, pod.Name, contName)
		if logErr != nil {
			framework.Logf("Warning: Failed to get logs from pod %q: %v", pod.Name, logErr)
		} else {
			framework.Logf("pod %s/%s logs:\n%s", namespace, pod.Name, logs)
		}
	}
	// Need to get it again to ensure the ip addresses are filled
	res, err = podClient.Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to get pod %s %s", pod.Name, namespace)
	}
	return res, nil
}

// Get the IP address of a pod in the specified namespace
func getPodAddress(podName, namespace string) string {
	podIP, err := framework.RunKubectl(namespace, "get", "pods", podName, "--template={{.status.podIP}}")
	if err != nil {
		framework.Failf("Unable to retrieve the IP for pod %s %v", podName, err)
	}
	return podIP
}

// Get the IP address of the API server
func getApiAddress() string {
	apiServerIP, err := framework.RunKubectl("default", "get", "svc", "kubernetes", "-o", "jsonpath='{.spec.clusterIP}'")
	apiServerIP = strings.Trim(apiServerIP, "'")
	if err != nil {
		framework.Failf("Error: unable to get API-server IP address, err:  %v", err)
	}
	apiServer := net.ParseIP(apiServerIP)
	if apiServer == nil {
		framework.Failf("Error: unable to parse API-server IP address:  %s", apiServerIP)
	}
	return apiServer.String()
}

// IsGatewayModeLocal returns true if the gateway mode is local
func IsGatewayModeLocal() bool {
	anno, err := framework.RunKubectl("default", "get", "node", "ovn-control-plane", "-o", "template", "--template={{.metadata.annotations}}")
	if err != nil {
		return false
	}
	return strings.Contains(anno, "local")
}

// runCommand runs the cmd and returns the combined stdout and stderr
func runCommand(cmd ...string) (string, error) {
	output, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run %q: %s (%s)", strings.Join(cmd, " "), err, output)
	}
	return string(output), nil
}

// restartOVNKubeNodePod restarts the ovnkube-node pod from namespace, running on nodeName
func restartOVNKubeNodePod(clientset kubernetes.Interface, namespace string, nodeName string) error {
	ovnKubeNodePods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "name=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return fmt.Errorf("could not get ovnkube-node pods: %w", err)
	}

	if len(ovnKubeNodePods.Items) <= 0 {
		return fmt.Errorf("could not find ovnkube-node pod running on node %s", nodeName)
	}
	for _, pod := range ovnKubeNodePods.Items {
		if err := e2epod.DeletePodWithWait(clientset, &pod); err != nil {
			return fmt.Errorf("could not delete ovnkube-node pod on node %s: %w", nodeName, err)
		}
	}

	framework.Logf("waiting for node %s to have running ovnkube-node pod", nodeName)
	wait.Poll(2*time.Second, 5*time.Minute, func() (bool, error) {
		ovnKubeNodePods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: "name=ovnkube-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil {
			return false, fmt.Errorf("could not get ovnkube-node pods: %w", err)
		}

		if len(ovnKubeNodePods.Items) <= 0 {
			framework.Logf("Node %s has no ovnkube-node pod yet", nodeName)
			return false, nil
		}
		for _, pod := range ovnKubeNodePods.Items {
			if ready, err := testutils.PodRunningReady(&pod); !ready {
				framework.Logf("%v", err)
				return false, nil
			}
		}
		return true, nil
	})

	return nil
}

var _ = ginkgo.Describe("e2e control plane", func() {
	var svcname = "nettest"

	f := newPrivelegedTestFramework(svcname)
	var (
		extDNSIP   string
		numMasters int
	)

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

		masterPods, err := f.ClientSet.CoreV1().Pods("ovn-kubernetes").List(context.Background(), metav1.ListOptions{
			LabelSelector: "name=ovnkube-master",
		})
		framework.ExpectNoError(err)
		numMasters = len(masterPods.Items)
		extDNSIP = "8.8.8.8"
		if IsIPv6Cluster(f.ClientSet) {
			extDNSIP = "2001:4860:4860::8888"
		}
	})

	ginkgo.It("should provide Internet connection continuously when ovn-k8s pod is killed", func() {
		ginkgo.By(fmt.Sprintf("Running container which tries to connect to %s in a loop", extDNSIP))

		podChan, errChan := make(chan *v1.Pod), make(chan error)
		go func() {
			defer ginkgo.GinkgoRecover()
			checkContinuousConnectivity(f, "", "connectivity-test-continuous", extDNSIP, 53, 30, 30, podChan, errChan)
		}()

		testPod := <-podChan
		nodeName := testPod.Spec.NodeName
		framework.Logf("Test pod running on %q", nodeName)

		ginkgo.By("Deleting ovn-kube pod on node " + nodeName)
		err := restartOVNKubeNodePod(f.ClientSet, "ovn-kubernetes", nodeName)
		framework.ExpectNoError(err)

		ginkgo.By("Ensuring there were no connectivity errors")
		framework.ExpectNoError(<-errChan)

		err = waitClusterHealthy(f, numMasters)
		framework.ExpectNoError(err, "one or more nodes failed to go back ready, schedulable, and untainted")
	})

	ginkgo.It("should provide Internet connection continuously when master is killed", func() {
		ginkgo.By(fmt.Sprintf("Running container which tries to connect to %s in a loop", extDNSIP))

		podChan, errChan := make(chan *v1.Pod), make(chan error)
		go func() {
			defer ginkgo.GinkgoRecover()
			checkContinuousConnectivity(f, "", "connectivity-test-continuous", extDNSIP, 53, 30, 30, podChan, errChan)
		}()
		testPod := <-podChan
		framework.Logf("Test pod running on %q", testPod.Spec.NodeName)

		time.Sleep(5 * time.Second)

		podClient := f.ClientSet.CoreV1().Pods("ovn-kubernetes")

		podList, err := podClient.List(context.Background(), metav1.ListOptions{
			LabelSelector: "name=ovnkube-master",
		})
		framework.ExpectNoError(err)

		podName := ""
		for _, pod := range podList.Items {
			if strings.HasPrefix(pod.Name, "ovnkube-master") {
				podName = pod.Name
				break
			}
		}

		ginkgo.By("Deleting ovn-kube master pod " + podName)
		e2epod.DeletePodWithWaitByName(f.ClientSet, podName, "ovn-kubernetes")
		framework.Logf("Deleted ovnkube-master %q", podName)

		ginkgo.By("Ensring there were no connectivity errors")
		framework.ExpectNoError(<-errChan)

		err = waitClusterHealthy(f, numMasters)
		framework.ExpectNoError(err, "one or more nodes failed to go back ready, schedulable, and untainted")
	})

	ginkgo.It("should provide connection to external host by DNS name from a pod", func() {
		ginkgo.By("Running container which tries to connect to www.google.com. in a loop")

		podChan, errChan := make(chan *v1.Pod), make(chan error)
		go func() {
			defer ginkgo.GinkgoRecover()
			checkContinuousConnectivity(f, "", "connectivity-test-continuous", "www.google.com.", 443, 10, 30, podChan, errChan)
		}()

		testPod := <-podChan
		framework.Logf("Test pod running on %q", testPod.Spec.NodeName)

		time.Sleep(10 * time.Second)

		framework.ExpectNoError(<-errChan)
	})

	ginkgo.Describe("test node readiness according to its defaults interface MTU size", func() {
		const testNodeName = "ovn-worker"
		var originalMTU int

		ginkgo.BeforeEach(func() {
			// get the interface current mtu and store it as original value to be able to reset it after the test
			res, err := runCommand("docker", "exec", testNodeName, "cat", "/sys/class/net/breth0/mtu")
			if err != nil {
				framework.Failf("could not get MTU of interface: %s", err)
			}

			res = strings.ReplaceAll(res, "\n", "")
			originalMTU, err = strconv.Atoi(res)
			if err != nil {
				framework.Failf("could not convert MTU to integer: %s", err)
			}
		})

		ginkgo.AfterEach(func() {
			// reset MTU to original value
			_, err := runCommand("docker", "exec", testNodeName, "ip", "link", "set", "breth0", "mtu", fmt.Sprintf("%d", originalMTU))
			if err != nil {
				framework.Failf("could not reset MTU of interface: %s", err)
			}

			// restart ovnkube-node pod
			if err := restartOVNKubeNodePod(f.ClientSet, ovnNamespace, testNodeName); err != nil {
				framework.Failf("could not restart ovnkube-node pod: %s", err)
			}

			err = waitClusterHealthy(f, numMasters)
			framework.ExpectNoError(err, "one or more nodes failed to go back ready, schedulable, and untainted")
		})

		ginkgo.It("should get node not ready with a too small MTU", func() {
			// set the defaults interface MTU very low
			_, err := runCommand("docker", "exec", testNodeName, "ip", "link", "set", "breth0", "mtu", "1000")
			if err != nil {
				framework.Failf("could not set MTU of interface: %s", err)
			}

			// restart ovnkube-node pod to trigger mtu validation
			if err := restartOVNKubeNodePod(f.ClientSet, ovnNamespace, testNodeName); err != nil {
				framework.Failf("could not restart ovnkube-node pod: %s", err)
			}
			node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), testNodeName, metav1.GetOptions{ResourceVersion: "0"})
			if err != nil {
				framework.Failf("could not find node resource: %s", err)
			}
			gomega.Eventually(func() bool {
				return e2enode.IsNodeReady(node)
			}, 30*time.Second).Should(gomega.BeFalse())
		})

		ginkgo.It("should get node ready with a big enough MTU", func() {
			// set the defaults interface MTU big enough
			_, err := runCommand("docker", "exec", testNodeName, "ip", "link", "set", "breth0", "mtu", "2000")
			if err != nil {
				framework.Failf("could not set MTU of interface: %s", err)
			}

			// restart ovnkube-node pod to trigger mtu validation
			if err := restartOVNKubeNodePod(f.ClientSet, ovnNamespace, testNodeName); err != nil {
				framework.Failf("could not restart ovnkube-node pod: %s", err)
			}

			// validate that node is in Ready state
			node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), testNodeName, metav1.GetOptions{ResourceVersion: "0"})
			if err != nil {
				framework.Failf("could not find node resource: %s", err)
			}
			gomega.Eventually(func() bool {
				return e2enode.IsNodeReady(node)
			}, 30*time.Second).Should(gomega.BeTrue())
		})
	})
})

// Test pod connectivity to other host IP addresses
var _ = ginkgo.Describe("test e2e pod connectivity to host addresses", func() {
	const (
		ovnWorkerNode string = "ovn-worker"
		svcname       string = "node-e2e-to-host"
	)
	var (
		targetIP     string
		singleIPMask string
	)

	f := newPrivelegedTestFramework(svcname)

	ginkgo.BeforeEach(func() {
		targetIP = "123.123.123.123"
		singleIPMask = "32"
		if IsIPv6Cluster(f.ClientSet) {
			targetIP = "2001:db8:3333:4444:CCCC:DDDD:EEEE:FFFF"
			singleIPMask = "128"
		}
		// Add another IP address to the worker
		_, err := runCommand("docker", "exec", ovnWorkerNode, "ip", "a", "add",
			fmt.Sprintf("%s/%s", targetIP, singleIPMask), "dev", "breth0")
		framework.ExpectNoError(err, "failed to add IP to %s", ovnWorkerNode)
	})

	ginkgo.AfterEach(func() {
		_, err := runCommand("docker", "exec", ovnWorkerNode, "ip", "a", "del",
			fmt.Sprintf("%s/%s", targetIP, singleIPMask), "dev", "breth0")
		framework.ExpectNoError(err, "failed to remove IP from %s", ovnWorkerNode)
	})

	ginkgo.It("Should validate connectivity from a pod to a non-node host address on same node", func() {
		// Spin up another pod that attempts to reach the previously started pod on separate nodes
		framework.ExpectNoError(
			checkConnectivityPingToHost(f, ovnWorkerNode, "e2e-src-ping-pod", targetIP, ipv4PingCommand, 30, false))
	})
})

// Test e2e inter-node connectivity over br-int
var _ = ginkgo.Describe("test e2e inter-node connectivity between worker nodes", func() {
	const (
		svcname          string = "inter-node-e2e"
		ovnNs            string = "ovn-kubernetes"
		ovnWorkerNode    string = "ovn-worker"
		ovnWorkerNode2   string = "ovn-worker2"
		ovnHaWorkerNode2 string = "ovn-control-plane2"
		ovnHaWorkerNode3 string = "ovn-control-plane3"
		ovnContainer     string = "ovnkube-node"
		jsonFlag         string = "-o=jsonpath='{.items..metadata.name}'"
		getPodIPRetry    int    = 20
	)

	var (
		haMode    bool
		labelFlag = fmt.Sprintf("name=%s", ovnContainer)
	)

	f := newPrivelegedTestFramework(svcname)

	// Determine which KIND environment is running by querying the running nodes
	ginkgo.BeforeEach(func() {
		fieldSelectorFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnWorkerNode)
		fieldSelectorHaFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnHaWorkerNode2)

		// Determine if the kind deployment is in HA mode or non-ha mode based on node naming
		kubectlOut, err := framework.RunKubectl(ovnNs, "get", "pods", "-l", labelFlag, jsonFlag, fieldSelectorFlag)
		if err != nil {
			framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnWorkerNode, err)
		}
		if kubectlOut == "''" {
			haMode = true
			kubectlOut, err = framework.RunKubectl(ovnNs, "get", "pods", "-l", labelFlag, jsonFlag, fieldSelectorHaFlag)
			if err != nil {
				framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnHaWorkerNode2, err)
			}
		}
		// Fail the test if no pod is matched within the specified node
		if kubectlOut == "''" {
			framework.Failf("Unable to locate container %s on any known nodes", ovnContainer)
		}
	})

	ginkgo.It("Should validate connectivity within a namespace of pods on separate nodes", func() {
		var validIP net.IP
		var pingTarget string
		var ciWorkerNodeSrc string
		var ciWorkerNodeDst string
		dstPingPodName := "e2e-dst-ping-pod"
		command := []string{"bash", "-c", "sleep 20000"}
		// non-ha ci mode runs a named set of nodes with a prefix of ovn-worker
		ciWorkerNodeSrc = ovnWorkerNode
		ciWorkerNodeDst = ovnWorkerNode2
		// ha ci mode runs a named set of nodes with a prefix of ovn-control-plane
		if haMode {
			framework.Logf("Detected a HA mode KIND environment")
			ciWorkerNodeSrc = ovnHaWorkerNode2
			ciWorkerNodeDst = ovnHaWorkerNode3
		}
		ginkgo.By(fmt.Sprintf("Creating a container on node %s and verifying connectivity to a pod on node %s", ciWorkerNodeSrc, ciWorkerNodeDst))

		// Create the pod that will be used as the destination for the connectivity test
		createGenericPod(f, dstPingPodName, ciWorkerNodeDst, f.Namespace.Name, command)

		// There is a condition somewhere with e2e WaitForPodNotPending that returns ready
		// before calling for the IP address will succeed. This simply adds some retries.
		for i := 1; i < getPodIPRetry; i++ {
			pingTarget = getPodAddress(dstPingPodName, f.Namespace.Name)
			validIP = net.ParseIP(pingTarget)
			if validIP != nil {
				framework.Logf("Destination ping target for %s is %s", dstPingPodName, pingTarget)
				break
			}
			time.Sleep(time.Second * 4)
			framework.Logf("Retry attempt %d to get pod IP from initializing pod %s", i, dstPingPodName)
		}
		// Fail the test if no address is ever retrieved
		if validIP == nil {
			framework.Failf("Warning: Failed to get an IP for target pod %s, test will fail", dstPingPodName)
		}
		// Spin up another pod that attempts to reach the previously started pod on separate nodes
		framework.ExpectNoError(
			checkConnectivityPingToHost(f, ciWorkerNodeSrc, "e2e-src-ping-pod", pingTarget, ipv4PingCommand, 30, false))
	})
})

var _ = ginkgo.Describe("e2e egress IP validation", func() {
	const (
		servicePort          int32  = 9999
		podHTTPPort          string = "8080"
		svcname              string = "egressip"
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
				targetNodeLogs, err = runCommand("docker", "logs", targetNode.name)
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

	verifyEgressIPStatusLengthEquals := func(statusLength int) []egressIPStatus {
		var statuses []egressIPStatus
		err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			statuses = getEgressIPStatusItems()
			return len(statuses) == statusLength, nil
		})
		if err != nil {
			framework.Failf("Error: expected to have %v egress IP assignment, got: %v", statusLength, len(statuses))
		}
		return statuses
	}

	f := newPrivelegedTestFramework(svcname)

	// Determine what mode the CI is running in and get relevant endpoint information for the tests
	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf("Test requires >= 2 Ready nodes, but there are only %v nodes", len(nodes.Items))
		}
		ips := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)
		egress1Node = node{
			name:   nodes.Items[0].Name,
			nodeIP: ips[0],
		}
		egress2Node = node{
			name:   nodes.Items[1].Name,
			nodeIP: ips[1],
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
		framework.RunKubectlOrDie("default", "delete", "eip", svcname)
		framework.RunKubectlOrDie("default", "label", "node", egress1Node.name, "k8s.ovn.org/egress-assignable-")
		framework.RunKubectlOrDie("default", "label", "node", egress2Node.name, "k8s.ovn.org/egress-assignable-")
		deleteClusterExternalContainer(targetNode.name)
		deleteClusterExternalContainer(deniedTargetNode.name)
	})

	// Validate the egress IP by creating a httpd container on the kind networking
	// (effectively seen as "outside" the cluster) and curl it from a pod in the cluster
	// which matches the egress IP stanza.

	/* This test does the following:
	   0. Add the "k8s.ovn.org/egress-assignable" label to two nodes
	   1. Create an EgressIP object with two egress IPs defined
	   2. Check that the status is of length two and both are assigned to different nodes
	   3. Create two pods matching the EgressIP: one running on each of the egress nodes
	   4. Check connectivity from both to an external "node" and verify that the IPs are both of the above
	   5. Check connectivity from one pod to the other and verify that the connection is achieved
	   6. Check connectivity from both pods to the api-server (running hostNetwork:true) and verifying that the connection is achieved
	   7. Update one of the pods, unmatching the EgressIP
	   8. Check connectivity from that one to an external "node" and verify that the IP is the node IP.
	   9. Check connectivity from the other one to an external "node"  and verify that the IPs are both of the above
	   10. Remove the node label off one of the egress node
	   11. Check that the status is of length one
	   12. Check connectivity from the remaining pod to an external "node" and verify that the IP is the remaining egress IP
	   13. Remove the node label off the last egress node
	   14. Check that the status is of length zero
	   15. Check connectivity from the remaining pod to an external "node" and verify that the IP is the node IP.
	   16. Re-add the label to one of the egress nodes
	   17. Check that the status is of length one
	   18. Check connectivity from the remaining pod to an external "node" and verify that the IP is the remaining egress IP
	*/
	ginkgo.It("Should validate the egress IP functionality against remote hosts", func() {

		command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}

		ginkgo.By("0. Add the \"k8s.ovn.org/egress-assignable\" label to two nodes")
		framework.AddOrUpdateLabelOnNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable", "dummy")
		framework.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")

		podNamespace := f.Namespace
		podNamespace.Labels = map[string]string{
			"name": f.Namespace.Name,
		}
		updateNamespace(f, podNamespace)

		ginkgo.By("1. Create an EgressIP object with two egress IPs defined")
		dupIP := func(ip net.IP) net.IP {
			dup := make(net.IP, len(ip))
			copy(dup, ip)
			return dup
		}
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
    name: ` + svcname + `
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
		statuses := verifyEgressIPStatusLengthEquals(2)
		if statuses[0].Node == statuses[1].Node {
			framework.Failf("Step 2. Check that the status is of length two and both are assigned to different nodess, failed, err: both egress IPs have been assigned to the same node")
		}

		ginkgo.By("3. Create two pods matching the EgressIP: one running on each of the egress nodes")
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

		ginkgo.By("10. Remove the node label off one of the egress node")
		framework.RemoveLabelOffNode(f.ClientSet, egress1Node.name, "k8s.ovn.org/egress-assignable")

		ginkgo.By("11. Check that the status is of length one")
		statuses = verifyEgressIPStatusLengthEquals(1)

		ginkgo.By("12. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the remaining egress IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{statuses[0].EgressIP}))
		framework.ExpectNoError(err, "Step 12. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the remaining egress IP, failed, err: %v", err)

		ginkgo.By("13. Remove the node label off the last egress node")
		framework.RemoveLabelOffNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable")

		ginkgo.By("14. Check that the status is of length zero")
		statuses = verifyEgressIPStatusLengthEquals(0)

		ginkgo.By("15. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the node IP.")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{pod1Node.nodeIP}))
		framework.ExpectNoError(err, "Step  15. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the node IP, failed, err: %v", err)

		ginkgo.By("16. Re-add the label to one of the egress nodes")
		framework.AddOrUpdateLabelOnNode(f.ClientSet, egress2Node.name, "k8s.ovn.org/egress-assignable", "dummy")

		ginkgo.By("17. Check that the status is of length one")
		statuses = verifyEgressIPStatusLengthEquals(1)

		ginkgo.By("18. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the remaining egress IP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{statuses[0].EgressIP}))
		framework.ExpectNoError(err, "Step 18. Check connectivity from the remaining pod to an external \"node\" and verify that the IP is the remaining egress IP, failed, err: %v", err)
	})

	// Validate the egress IP by creating a httpd container on the kind networking
	// (effectively seen as "outside" the cluster) and curl it from a pod in the cluster
	// which matches the egress IP stanza. Aim is to check if the SNATs towards nodeIP versus
	// SNATs towards egressIPs are being correctly deleted and recreated.
	// NOTE: See https://bugzilla.redhat.com/show_bug.cgi?id=2078222 for details on inconsistency

	/* This test does the following:
	   0. Add the "k8s.ovn.org/egress-assignable" label to egress1Node
	   1. Creating host-networked pod, on non-egress node (egress2Node) to act as "another node"
	   2. Create an EgressIP object with one egress IP defined
	   3. Check that the status is of length one and that it is assigned to egress1Node
	   4. Create one pod matching the EgressIP: running on egress1Node
	   5. Check connectivity from pod to an external "node" and verify that the srcIP is the expected egressIP
	   6. Check connectivity from pod to another node (egress2Node) and verify that the srcIP is the expected [egressIP if SGW] & [nodeIP if LGW]
	   7. Add the "k8s.ovn.org/egress-assignable" label to egress2Node
	   8. Remove the "k8s.ovn.org/egress-assignable" label from egress1Node
	   9. Check that the status is of length one and that it is assigned to egress2Node
	   10. Check connectivity from pod to an external "node" and verify that the srcIP is the expected egressIP
	   11. Check connectivity from pod to another node (egress2Node) and verify that the srcIP is the expected nodeIP (known inconsistency compared to step 6 for SGW)
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
		dupIP := func(ip net.IP) net.IP {
			dup := make(net.IP, len(ip))
			copy(dup, ip)
			return dup
		}
		// Assign the egress IP without conflicting with any node IP,
		// the kind subnet is /16 or /64 so the following should be fine.
		egressNodeIP := net.ParseIP(egress1Node.nodeIP)
		egressIP1 := dupIP(egressNodeIP)
		egressIP1[len(egressIP1)-2]++

		var egressIPConfig = fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + svcname + `
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
		statuses := verifyEgressIPStatusLengthEquals(1)
		if statuses[0].Node != egress1Node.name {
			framework.Failf("Step 2. Check that the status is of length one and that it is assigned to egress1Node, failed")
		}

		ginkgo.By("4. Create one pod matching the EgressIP: running on egress1Node")
		createGenericPodWithLabel(f, pod1Name, pod1Node.name, f.Namespace.Name, command, podEgressLabel)

		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut := getPodAddress(pod1Name, f.Namespace.Name)
			srcIP := net.ParseIP(kubectlOut)
			if srcIP == nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Step 4. Create one pod matching the EgressIP: running on egress1Node, failed, err: %v", err)
		framework.Logf("Created pod %s on node %s", pod1Name, pod1Node.name)

		ginkgo.By("5. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(targetNode, pod1Name, podNamespace.Name, true, []string{egressIP1.String()}))
		framework.ExpectNoError(err, "Step 5. Check connectivity from pod to an external node and verify that the srcIP is the expected egressIP, failed: %v", err)

		ginkgo.By("6. Check connectivity from pod to another node and verify that the srcIP is the expected [egressIP if SGW] & [nodeIP if LGW]")
		var verifyIP string
		if IsGatewayModeLocal() {
			verifyIP = egressNodeIP.String()
		} else {
			verifyIP = egressIP1.String()
		}
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(hostNetPod, pod1Name, podNamespace.Name, true, []string{verifyIP}))
		framework.ExpectNoError(err, "Step 6. Check connectivity from pod to another node and verify that the srcIP is the expected [egressIP if SGW] & [nodeIP if LGW], failed: %v", err)

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

		ginkgo.By("11. Check connectivity from pod to another node and verify that the srcIP is the expected nodeIP (known inconsistency compared to step 6 for SGW)")
		err = wait.PollImmediate(retryInterval, retryTimeout, targetExternalContainerAndTest(hostNetPod, pod1Name, podNamespace.Name, true, []string{egressNodeIP.String()}))
		framework.ExpectNoError(err, "Step 11. Check connectivity from pod to another node and verify that the srcIP is the expected nodeIP (known inconsistency compared to step 6), failed: %v", err)

		ginkgo.By("12. Create second pod not matching the EgressIP: running on egress1Node")
		createGenericPodWithLabel(f, pod2Name, pod1Node.name, f.Namespace.Name, command, map[string]string{})
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut := getPodAddress(pod2Name, f.Namespace.Name)
			srcIP := net.ParseIP(kubectlOut)
			if srcIP == nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Step 12. Create second pod not matching the EgressIP: running on egress1Node, failed, err: %v", err)
		framework.Logf("Created pod %s on node %s", pod2Name, pod1Node.name)

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
		dupIP := func(ip net.IP) net.IP {
			dup := make(net.IP, len(ip))
			copy(dup, ip)
			return dup
		}
		// Assign the egress IP without conflicting with any node IP,
		// the kind subnet is /16 or /64 so the following should be fine.
		egressNodeIP := net.ParseIP(egress1Node.nodeIP)
		egressIP := dupIP(egressNodeIP)
		egressIP[len(egressIP)-2]++

		var egressIPConfig = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: ` + svcname + `
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
		verifyEgressIPStatusLengthEquals(1)

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

// Validate pods can reach a network running in a container's looback address via
// an external gateway running on eth0 of the container without any tunnel encap.
// Next, the test updates the namespace annotation to point to a second container,
// emulating the ext gateway. This test requires shared gateway mode in the job infra.
var _ = ginkgo.Describe("e2e non-vxlan external gateway and update validation", func() {
	const (
		svcname             string = "multiple-novxlan-externalgw"
		ovnNs               string = "ovn-kubernetes"
		ovnWorkerNode       string = "ovn-worker"
		ovnHaWorkerNode     string = "ovn-control-plane2"
		ovnContainer        string = "ovnkube-node"
		gwContainerNameAlt1 string = "gw-novxlan-test-container-alt1"
		gwContainerNameAlt2 string = "gw-novxlan-test-container-alt2"
		ovnControlNode      string = "ovn-control-plane"
	)
	var (
		haMode           bool
		exGWRemoteIpAlt1 string
		exGWRemoteIpAlt2 string
	)
	f := newPrivelegedTestFramework(svcname)

	// Determine what mode the CI is running in and get relevant endpoint information for the tests
	ginkgo.BeforeEach(func() {
		labelFlag := fmt.Sprintf("name=%s", ovnContainer)
		jsonFlag := "-o=jsonpath='{.items..metadata.name}'"
		fieldSelectorFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnWorkerNode)
		fieldSelectorHaFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnHaWorkerNode)
		fieldSelectorControlFlag := fmt.Sprintf("--field-selector=spec.nodeName=%s", ovnControlNode)
		// retrieve pod names from the running cluster
		kubectlOut, err := framework.RunKubectl(ovnNs, "get", "pods", "-l", labelFlag, jsonFlag, fieldSelectorControlFlag)
		if err != nil {
			framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnControlNode, err)
		}
		// attempt to retrieve the pod name that will source the test in non-HA mode
		kubectlOut, err = framework.RunKubectl(ovnNs, "get", "pods", "-l", labelFlag, jsonFlag, fieldSelectorFlag)
		if err != nil {
			framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnWorkerNode, err)
		}
		// attempt to retrieve the pod name that will source the test in HA mode
		if kubectlOut == "''" {
			haMode = true
			kubectlOut, err = framework.RunKubectl(ovnNs, "get", "pods", "-l", labelFlag, jsonFlag, fieldSelectorHaFlag)
			if err != nil {
				framework.Failf("Expected container %s running on %s error %v", ovnContainer, ovnHaWorkerNode, err)
			}
		}

		exGWRemoteIpAlt1 = "10.249.3.1"
		exGWRemoteIpAlt2 = "10.249.4.1"
		if IsIPv6Cluster(f.ClientSet) {
			exGWRemoteIpAlt1 = "fc00:f853:ccd:e793::1"
			exGWRemoteIpAlt2 = "fc00:f853:ccd:e794::1"
		}
	})

	ginkgo.AfterEach(func() {
		// tear down the containers simulating the gateways
		if cid, _ := runCommand("docker", "ps", "-qaf", fmt.Sprintf("name=%s", gwContainerNameAlt1)); cid != "" {
			if _, err := runCommand("docker", "rm", "-f", gwContainerNameAlt1); err != nil {
				framework.Logf("failed to delete the gateway test container %s %v", gwContainerNameAlt1, err)
			}
		}
		if cid, _ := runCommand("docker", "ps", "-qaf", fmt.Sprintf("name=%s", gwContainerNameAlt2)); cid != "" {
			if _, err := runCommand("docker", "rm", "-f", gwContainerNameAlt2); err != nil {
				framework.Logf("failed to delete the gateway test container %s %v", gwContainerNameAlt2, err)
			}
		}
	})

	ginkgo.It("Should validate connectivity without vxlan before and after updating the namespace annotation to a new external gateway", func() {

		var pingSrc string
		var validIP net.IP

		isIPv6Cluster := IsIPv6Cluster(f.ClientSet)
		srcPingPodName := "e2e-exgw-novxlan-src-ping-pod"
		command := []string{"bash", "-c", "sleep 20000"}
		testContainer := fmt.Sprintf("%s-container", srcPingPodName)
		testContainerFlag := fmt.Sprintf("--container=%s", testContainer)
		// non-ha ci mode runs a set of kind nodes prefixed with ovn-worker
		ciWorkerNodeSrc := ovnWorkerNode
		if haMode {
			// ha ci mode runs a named set of nodes with a prefix of ovn-control-plane
			ciWorkerNodeSrc = ovnHaWorkerNode
		}

		// start the container that will act as an external gateway
		_, err := runCommand("docker", "run", "-itd", "--privileged", "--network", externalContainerNetwork, "--name", gwContainerNameAlt1, "centos")
		if err != nil {
			framework.Failf("failed to start external gateway test container %s: %v", gwContainerNameAlt1, err)
		}
		// retrieve the container ip of the external gateway container
		alt1IPv4, alt1IPv6 := getContainerAddressesForNetwork(gwContainerNameAlt1, externalContainerNetwork)
		if err != nil {
			framework.Failf("failed to start external gateway test container: %v", err)
		}
		nodeIPv4, nodeIPv6 := getContainerAddressesForNetwork(ciWorkerNodeSrc, externalContainerNetwork)

		exGWRemoteCidrAlt1 := fmt.Sprintf("%s/24", exGWRemoteIpAlt1)
		exGWRemoteCidrAlt2 := fmt.Sprintf("%s/24", exGWRemoteIpAlt2)
		exGWIpAlt1 := alt1IPv4
		nodeIP := nodeIPv4
		if isIPv6Cluster {
			exGWIpAlt1 = alt1IPv6
			exGWRemoteCidrAlt1 = fmt.Sprintf("%s/64", exGWRemoteIpAlt1)
			exGWRemoteCidrAlt2 = fmt.Sprintf("%s/64", exGWRemoteIpAlt2)
			nodeIP = nodeIPv6
		}

		// annotate the test namespace
		annotateArgs := []string{
			"annotate",
			"namespace",
			f.Namespace.Name,
			fmt.Sprintf("k8s.ovn.org/routing-external-gws=%s", exGWIpAlt1),
		}
		framework.Logf("Annotating the external gateway test namespace to a container gw: %s ", exGWIpAlt1)
		framework.RunKubectlOrDie(f.Namespace.Name, annotateArgs...)

		podCIDR, err := getNodePodCIDR(ciWorkerNodeSrc)
		if err != nil {
			framework.Failf("Error retrieving the pod cidr from %s %v", ciWorkerNodeSrc, err)
		}
		framework.Logf("the pod cidr for node %s is %s", ciWorkerNodeSrc, podCIDR)
		// add loopback interface used to validate all traffic is getting drained through the gateway
		_, err = runCommand("docker", "exec", gwContainerNameAlt1, "ip", "address", "add", exGWRemoteCidrAlt1, "dev", "lo")
		if err != nil {
			framework.Failf("failed to add the loopback ip to dev lo on the test container: %v", err)
		}
		// Create the pod that will be used as the source for the connectivity test
		createGenericPod(f, srcPingPodName, ciWorkerNodeSrc, f.Namespace.Name, command)
		// wait for pod setup to return a valid address
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			pingSrc = getPodAddress(srcPingPodName, f.Namespace.Name)
			validIP = net.ParseIP(pingSrc)
			if validIP == nil {
				return false, nil
			}
			return true, nil
		})
		// Fail the test if no address is ever retrieved
		if err != nil {
			framework.Failf("Error trying to get the pod IP address")
		}
		// add a host route on the first mock gateway for return traffic to the pod
		_, err = runCommand("docker", "exec", gwContainerNameAlt1, "ip", "route", "add", pingSrc, "via", nodeIP)
		if err != nil {
			framework.Failf("failed to add the pod host route on the test container: %v", err)
		}
		_, err = runCommand("docker", "exec", gwContainerNameAlt1, "ping", "-c", "5", pingSrc)
		framework.ExpectNoError(err, "Failed to ping %s from container %s", pingSrc, gwContainerNameAlt1)

		time.Sleep(time.Second * 15)
		// Verify the gateway and remote address is reachable from the initial pod
		ginkgo.By(fmt.Sprintf("Verifying connectivity without vxlan to the updated annotation and initial external gateway %s and remote address %s", exGWIpAlt1, exGWRemoteIpAlt1))
		_, err = framework.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, testContainerFlag, "--", "ping", "-w", "40", exGWRemoteIpAlt1)
		if err != nil {
			framework.Failf("Failed to ping the first gateway network %s from container %s on node %s: %v", exGWRemoteIpAlt1, ovnContainer, ovnWorkerNode, err)
		}
		// start the container that will act as a new external gateway that the tests will be updated to use
		_, err = runCommand("docker", "run", "-itd", "--privileged", "--network", externalContainerNetwork, "--name", gwContainerNameAlt2, "centos")
		if err != nil {
			framework.Failf("failed to start external gateway test container %s: %v", gwContainerNameAlt2, err)
		}
		// retrieve the container ip of the external gateway container
		alt2IPv4, alt2IPv6 := getContainerAddressesForNetwork(gwContainerNameAlt2, externalContainerNetwork)
		exGWIpAlt2 := alt2IPv4
		if isIPv6Cluster {
			exGWIpAlt2 = alt2IPv6
		}

		// override the annotation in the test namespace with the new gateway
		annotateArgs = []string{
			"annotate",
			"namespace",
			f.Namespace.Name,
			fmt.Sprintf("k8s.ovn.org/routing-external-gws=%s", exGWIpAlt2),
			"--overwrite",
		}
		framework.Logf("Annotating the external gateway test namespace to a new container remote IP:%s gw:%s ", exGWIpAlt2, exGWRemoteIpAlt2)
		framework.RunKubectlOrDie(f.Namespace.Name, annotateArgs...)
		// add loopback interface used to validate all traffic is getting drained through the gateway
		_, err = runCommand("docker", "exec", gwContainerNameAlt2, "ip", "address", "add", exGWRemoteCidrAlt2, "dev", "lo")
		if err != nil {
			framework.Failf("failed to add the loopback ip to dev lo on the test container: %v", err)
		}
		// add a host route on the second mock gateway for return traffic to the pod
		_, err = runCommand("docker", "exec", gwContainerNameAlt2, "ip", "route", "add", pingSrc, "via", nodeIP)
		if err != nil {
			framework.Failf("failed to add the pod route on the test container: %v", err)
		}

		_, err = runCommand("docker", "exec", gwContainerNameAlt2, "ping", "-c", "5", pingSrc)
		framework.ExpectNoError(err, "Failed to ping %s from container %s", pingSrc, gwContainerNameAlt1)

		// Verify the updated gateway and remote address is reachable from the initial pod
		ginkgo.By(fmt.Sprintf("Verifying connectivity without vxlan to the updated annotation and new external gateway %s and remote IP %s", exGWRemoteIpAlt2, exGWIpAlt2))
		_, err = framework.RunKubectl(f.Namespace.Name, "exec", srcPingPodName, testContainerFlag, "--", "ping", "-w", "40", exGWRemoteIpAlt2)
		if err != nil {
			framework.Failf("Failed to ping the second gateway network %s from container %s on node %s: %v", exGWRemoteIpAlt2, ovnContainer, ovnWorkerNode, err)
		}
	})
})

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

	f := newPrivelegedTestFramework(svcname)

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

	ginkgo.AfterEach(func() {})

	ginkgo.It("Should validate the egress firewall policy functionality against remote hosts", func() {
		srcPodName := "e2e-egress-fw-src-pod"
		command := []string{"bash", "-c", "sleep 20000"}
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
		if err := ioutil.WriteFile(egressFirewallYamlFile, []byte(egressFirewallConfig), 0644); err != nil {
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
		framework.RunKubectlOrDie(f.Namespace.Name, applyArgs...)
		// create the pod that will be used as the source for the connectivity test
		createGenericPod(f, srcPodName, serverNodeInfo.name, f.Namespace.Name, command)

		// Wait for pod exgw setup to be almost ready
		err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut := getPodAddress(srcPodName, f.Namespace.Name)
			validIP := net.ParseIP(kubectlOut)
			if validIP == nil {
				return false, nil
			}
			return true, nil
		})
		// Fail the test if no address is ever retrieved
		if err != nil {
			framework.Failf("Error trying to get the pod IP address %v", err)
		}
		// Verify the remote host/port as explicitly allowed by the firewall policy is reachable
		ginkgo.By(fmt.Sprintf("Verifying connectivity to an explicitly allowed host %s is permitted as defined by the external firewall policy", exFWPermitTcpDnsDest))
		_, err = framework.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "nc", "-vz", "-w", testTimeout, exFWPermitTcpDnsDest, "53")
		if err != nil {
			framework.Failf("Failed to connect to the remote host %s from container %s on node %s: %v", exFWPermitTcpDnsDest, ovnContainer, serverNodeInfo.name, err)
		}
		// Verify the remote host/port as implicitly denied by the firewall policy is not reachable
		ginkgo.By(fmt.Sprintf("Verifying connectivity to an implicitly denied host %s is not permitted as defined by the external firewall policy", exFWDenyTcpDnsDest))
		_, err = framework.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "nc", "-vz", "-w", testTimeout, exFWDenyTcpDnsDest, "53")
		if err == nil {
			framework.Failf("Succeeded in connecting the implicitly denied remote host %s from container %s on node %s", exFWDenyTcpDnsDest, ovnContainer, serverNodeInfo.name)
		}
		// Verify the the explicitly allowed host/port tcp port 80 rule is functional
		ginkgo.By(fmt.Sprintf("Verifying connectivity to an explicitly allowed host %s is permitted as defined by the external firewall policy", exFWPermitTcpWwwDest))
		_, err = framework.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "nc", "-vz", "-w", testTimeout, exFWPermitTcpWwwDest, "80")
		if err != nil {
			framework.Failf("Failed to curl the remote host %s from container %s on node %s: %v", exFWPermitTcpWwwDest, ovnContainer, serverNodeInfo.name, err)
		}
		// Verify the remote host/port 443 as implicitly denied by the firewall policy is not reachable
		ginkgo.By(fmt.Sprintf("Verifying connectivity to an implicitly denied port on host %s is not permitted as defined by the external firewall policy", exFWPermitTcpWwwDest))
		_, err = framework.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "nc", "-vz", "-w", testTimeout, exFWPermitTcpWwwDest, "443")
		if err == nil {
			framework.Failf("Failed to curl the remote host %s from container %s on node %s: %v", exFWPermitTcpWwwDest, ovnContainer, serverNodeInfo.name, err)
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
		if err := ioutil.WriteFile(egressFirewallYamlFile, []byte(egressFirewallConfig), 0644); err != nil {
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
		framework.RunKubectlOrDie(f.Namespace.Name, applyArgs...)
		gomega.Eventually(func() bool {
			output, err := framework.RunKubectl(f.Namespace.Name, "get", "egressfirewall", "default")
			if err != nil {
				framework.Failf("could not get the egressfirewall default in namespace: %s", f.Namespace.Name)
			}
			return strings.Contains(output, "EgressFirewall Rules applied")
		}, 30*time.Second).Should(gomega.BeTrue())
	})
})

var _ = ginkgo.Describe("e2e ingress traffic validation", func() {
	const (
		endpointHTTPPort    = 80
		endpointUDPPort     = 90
		clusterHTTPPort     = 81
		clusterHTTPPort2    = 82
		clusterUDPPort      = 91
		clusterUDPPort2     = 92
		clientContainerName = "npclient"
	)

	f := newPrivelegedTestFramework("nodeport-ingress-test")
	endpointsSelector := map[string]string{"servicebackend": "true"}

	var endPoints []*v1.Pod
	var nodesHostnames sets.String
	maxTries := 0
	var nodes *v1.NodeList
	var newNodeAddresses []string
	var externalIpv4 string
	var externalIpv6 string
	var isDualStack bool

	ginkgo.Context("Validating ingress traffic", func() {
		ginkgo.BeforeEach(func() {
			endPoints = make([]*v1.Pod, 0)
			nodesHostnames = sets.NewString()

			var err error
			nodes, err = e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 3)
			framework.ExpectNoError(err)

			if len(nodes.Items) < 3 {
				framework.Failf(
					"Test requires >= 3 Ready nodes, but there are only %v nodes",
					len(nodes.Items))
			}

			isDualStack = isDualStackCluster(nodes)

			ginkgo.By("Creating the endpoints pod, one for each worker")
			for _, node := range nodes.Items {
				// this create a udp / http netexec listener which is able to receive the "hostname"
				// command. We use this to validate that each endpoint is received at least once
				args := []string{
					"netexec",
					fmt.Sprintf("--http-port=%d", endpointHTTPPort),
					fmt.Sprintf("--udp-port=%d", endpointUDPPort),
				}
				pod, err := createPod(f, node.Name+"-ep", node.Name, f.Namespace.Name, []string{}, endpointsSelector, func(p *v1.Pod) {
					p.Spec.Containers[0].Args = args
				})
				framework.ExpectNoError(err)
				endPoints = append(endPoints, pod)
				nodesHostnames.Insert(pod.Name)

				// this is arbitrary and mutuated from k8s network e2e tests. We aim to hit all the endpoints at least once
				maxTries = len(endPoints)*len(endPoints) + 30
			}

			ginkgo.By("Creating an external container to send the traffic from")
			// the client uses the netexec command from the agnhost image, which is able to receive commands for poking other
			// addresses.
			// CAP NET_ADMIN is needed to remove neighbor entries for ARP/NS flap tests
			externalIpv4, externalIpv6 = createClusterExternalContainer(clientContainerName, agnhostImage, []string{"--network", "kind", "-P", "--cap-add", "NET_ADMIN"}, []string{"netexec", "--http-port=80"})
		})

		ginkgo.AfterEach(func() {
			deleteClusterExternalContainer(clientContainerName)
		})

		// This test validates ingress traffic to nodeports.
		// It creates a nodeport service on both udp and tcp, and creates a backend pod on each node.
		// The backend pods are using the agnhost - netexec command which replies to commands
		// with different protocols. We use the "hostname" command to have each backend pod to reply
		// with its hostname.
		// We use an external container to poke the service exposed on the node and we iterate until
		// all the hostnames are returned.
		// In case of dual stack enabled cluster, we iterate over all the nodes ips and try to hit the
		// endpoints from both each node's ips.
		ginkgo.It("Should be allowed by nodeport services", func() {
			serviceName := "nodeportsvc"
			ginkgo.By("Creating the nodeport service")
			npSpec := nodePortServiceSpecFrom(serviceName, v1.IPFamilyPolicyPreferDualStack, endpointHTTPPort, endpointUDPPort, clusterHTTPPort, clusterUDPPort, endpointsSelector, v1.ServiceExternalTrafficPolicyTypeCluster)
			np, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), npSpec, metav1.CreateOptions{})
			nodeTCPPort, nodeUDPPort := nodePortsFromService(np)
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for the endpoints to pop up")
			err = framework.WaitForServiceEndpointsNum(f.ClientSet, f.Namespace.Name, serviceName, len(endPoints), time.Second, wait.ForeverTestTimeout)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", serviceName, f.Namespace.Name)

			for _, protocol := range []string{"http", "udp"} {
				for _, node := range nodes.Items {
					for _, nodeAddress := range node.Status.Addresses {
						// skipping hostnames
						if !addressIsIP(nodeAddress) {
							continue
						}

						responses := sets.NewString()
						valid := false
						nodePort := nodeTCPPort
						if protocol == "udp" {
							nodePort = nodeUDPPort
						}

						ginkgo.By("Hitting the nodeport on " + node.Name + " and reaching all the endpoints " + protocol)
						for i := 0; i < maxTries; i++ {
							epHostname := pokeEndpointHostname(clientContainerName, protocol, nodeAddress.Address, nodePort)
							responses.Insert(epHostname)

							// each endpoint returns its hostname. By doing this, we validate that each ep was reached at least once.
							if responses.Equal(nodesHostnames) {
								framework.Logf("Validated node %s on address %s after %d tries", node.Name, nodeAddress.Address, i)
								valid = true
								break
							}
						}
						framework.ExpectEqual(valid, true, "Validation failed for node", node, responses, nodePort)
					}
				}
			}
		})

		// This test validates ingress traffic to NodePorts in a dual stack cluster after a Service upgrade from single stack to dual stack.
		// After an upgrade to DualStack cluster, 2 tests must be run:
		// a) Test from outside the cluster towards the NodePort - this test would fail in earlier versions of ovn-kubernetes
		// b) Test from the node itself towards its own NodePort - this test would fail in more recent versions of ovn-kubernetes even though a) would pass.
		//
		// This test tests a)
		// For test b), see test: "Should be allowed to node local host-networked endpoints by nodeport services with externalTrafficPolicy=local after upgrade to DualStack"
		//
		// In order to test this, this test does the following:
		// It creates a SingleStack nodeport service on both udp and tcp, and creates a backend pod on each node.
		// It then updates the nodeport service to PreferDualStack
		// It then waits for the service to get 2 ClusterIPs.
		// The backend pods are using the agnhost - netexec command which replies to commands
		// with different protocols. We use the "hostname" command to have each backend pod to reply
		// with its hostname.
		//
		// To test a) We use an external container to poke the service exposed on the node and we iterate until
		// all the hostnames are returned.
		// In case of dual stack enabled cluster, we iterate over all the nodes ips and try to hit the
		// endpoints from both each node's ips.
		//
		// This test will be skipped if the cluster is not in DualStack mode.
		ginkgo.It("Should be allowed by nodeport services after upgrade to DualStack", func() {
			if !isDualStack {
				ginkgo.Skip("Skipping as this is not a DualStack cluster")
			}
			serviceName := "nodeportsvc"

			ginkgo.By("Creating the nodeport service")
			npSpec := nodePortServiceSpecFrom(serviceName, v1.IPFamilyPolicySingleStack, endpointHTTPPort, endpointUDPPort, clusterHTTPPort, clusterUDPPort, endpointsSelector, v1.ServiceExternalTrafficPolicyTypeCluster)
			np, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), npSpec, metav1.CreateOptions{})
			nodeTCPPort, nodeUDPPort := nodePortsFromService(np)
			protocolPorts := map[string]int32{
				"http": nodeTCPPort,
				"udp":  nodeUDPPort,
			}
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for the endpoints to pop up")
			err = framework.WaitForServiceEndpointsNum(f.ClientSet, f.Namespace.Name, serviceName, len(endPoints), time.Second, wait.ForeverTestTimeout)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", serviceName, f.Namespace.Name)

			ginkgo.By("Collecting IPv4 and IPv6 node addresses")
			// Mapping of nodeName to all node IPv4 addresses and
			// mapping of nodeName to all node IPv6 addresses.
			ipv4Addresses := make(map[string][]string)
			ipv6Addresses := make(map[string][]string)
			var n string
			for _, node := range nodes.Items {
				n = node.Name
				ipv4Addresses[n] = []string{}
				ipv6Addresses[n] = []string{}
				for _, nodeAddress := range node.Status.Addresses {
					if addressIsIPv6(nodeAddress) {
						ipv6Addresses[n] = append(ipv6Addresses[n], nodeAddress.Address)
					} else if addressIsIPv4(nodeAddress) {
						ipv4Addresses[n] = append(ipv4Addresses[n], nodeAddress.Address)
					}
				}
			}
			// Mapping IPv4 -> nodeNames -> IP addreses.
			// Mapping IPv6 -> nodeNames -> IP addreses.
			ipAddressFamilyTargets := map[string]map[string][]string{
				"IPv4": ipv4Addresses,
				"IPv6": ipv6Addresses,
			}

			// First, upgrade to PreferDualStack and test endpoints.
			// Then, downgrade back to SingleStack and test endpoints.
			for _, ipFamilyPolicy := range []string{"PreferDualStack", "SingleStack"} {
				ginkgo.By(fmt.Sprintf("Changing the nodeport service to %s", ipFamilyPolicy))
				err = patchService(f.ClientSet, np.Name, np.Namespace, "/spec/ipFamilyPolicy", ipFamilyPolicy)
				framework.ExpectNoError(err)

				// It is expected that endpoints take a bit of time to come up after conversion. We remove all iptables rules and all breth0 flows.
				// Therefore, test IPv4 endpoints until they are stable, only then proceed to the actual test.
				// To be removed once https://github.com/ovn-org/ovn-kubernetes/issues/2933 is fixed.
				framework.Logf("Monitoring endpoints for up to 60 seconds for IPv4 to give them time to come up (issue 2933)")
				gomega.Eventually(func() (r bool) {
					// Sleep for 5 seconds before proceeding.
					framework.Logf("Sleeping for 5 seconds")
					time.Sleep(5 * time.Second)

					// Test all node IPv4 addresses http and return true if all of them come back with a valid answer.
					ipPort := net.JoinHostPort("localhost", "80")
					for _, ipAddresses := range ipv4Addresses {
						for _, targetHost := range ipAddresses {
							cmd := []string{"docker", "exec", clientContainerName}
							curlCommand := strings.Split(fmt.Sprintf("curl --max-time 2 -g -q -s http://%s/dial?request=hostname&protocol=http&host=%s&port=%d&tries=1",
								ipPort,
								targetHost,
								protocolPorts["http"]), " ")
							cmd = append(cmd, curlCommand...)
							framework.Logf("Running command %v", cmd)
							res, err := runCommand(cmd...)
							if err != nil {
								framework.Logf("Failed, res: %v, err: %v", res, err)
								return false
							}
							res, err = parseNetexecResponse(res)
							if err != nil {
								framework.Logf("Failed, res: %v, err: %v", res, err)
								return false
							}
						}
					}
					return true
				}, 60*time.Second, 10*time.Second).Should(gomega.BeTrue())

				// Test in the following order:
				// for IPv4, then IPv6:
				//   for each node:
				//      for all node IP addresses that belong to that node:
				//        probe http service port
				//          make sure that all endpoints can be reached
				//        probe udp service port
				//          make sure that all endpoints can be reached
				// Hit the exact same IP address family, IP address, protocol and port for maxTries times until we get back all endpoint hostnames for that
				// tuple.
				ipFamiliesToTest := []string{"IPv4"}
				if ipFamilyPolicy == "PreferDualStack" {
					ipFamiliesToTest = append(ipFamiliesToTest, "IPv6")
				}
				for _, ipAddressFamily := range ipFamiliesToTest {
					ginkgo.By(fmt.Sprintf("Testing %s services", ipAddressFamily))
					nodeToAddressesMapping := ipAddressFamilyTargets[ipAddressFamily]
					for nodeName, ipAddresses := range nodeToAddressesMapping {
						for _, address := range ipAddresses {
							// Use a slice for stable order, always tests http first and udp second due to
							// https://github.com/ovn-org/ovn-kubernetes/issues/2913.
							for _, protocol := range []string{"http", "udp"} {
								port := protocolPorts[protocol]
								ginkgo.By(fmt.Sprintf("Hitting nodeport %s/%d on %s with IP %s and reaching all the endpoints ", protocol, port, nodeName, address))
								responses := sets.NewString()
								valid := false
								for i := 0; i < maxTries; i++ {
									epHostname := pokeEndpointHostname(clientContainerName, protocol, address, port)
									responses.Insert(epHostname)

									// each endpoint returns its hostname. By doing this, we validate that each ep was reached at least once.
									if responses.Equal(nodesHostnames) {
										framework.Logf("Validated node %s on address %s after %d tries", nodeName, address, i)
										valid = true
										break
									}
								}
								framework.ExpectEqual(valid, true, "Validation failed for node", nodeName, responses, port)
							}
						}
					}
				}
			}
		})

		// This test validates ingress traffic to nodeports with externalTrafficPolicy Set to local.
		// It creates a nodeport service on both udp and tcp, and creates a backend pod on each node.
		// The backend pod is using the agnhost - netexec command which replies to commands
		// with different protocols. We use the "hostname" and "clientip" commands to have each backend
		// pod to reply with its hostname and the request packet's srcIP.
		// We use an external container to poke the service exposed on the node and ensure that only the
		// nodeport on the node with the backend actually receives traffic and that the packet is not
		// SNATed.
		// In case of dual stack enabled cluster, we iterate over all the nodes ips and try to hit the
		// endpoints from both each node's ips.
		ginkgo.It("Should be allowed to node local cluster-networked endpoints by nodeport services with externalTrafficPolicy=local", func() {
			serviceName := "nodeportsvclocal"
			ginkgo.By("Creating the nodeport service with externalTrafficPolicy=local")
			npSpec := nodePortServiceSpecFrom(serviceName, v1.IPFamilyPolicyPreferDualStack, endpointHTTPPort, endpointUDPPort, clusterHTTPPort, clusterUDPPort, endpointsSelector, v1.ServiceExternalTrafficPolicyTypeLocal)
			np, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), npSpec, metav1.CreateOptions{})
			nodeTCPPort, nodeUDPPort := nodePortsFromService(np)
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for the endpoints to pop up")
			err = framework.WaitForServiceEndpointsNum(f.ClientSet, f.Namespace.Name, serviceName, len(endPoints), time.Second, wait.ForeverTestTimeout)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", serviceName, f.Namespace.Name)

			for _, protocol := range []string{"http", "udp"} {
				for _, node := range nodes.Items {
					for _, nodeAddress := range node.Status.Addresses {
						// skipping hostnames
						if !addressIsIP(nodeAddress) {
							continue
						}

						responses := sets.NewString()
						// Fill expected responses, it should hit the nodeLocal endpoints and not SNAT packet IP
						expectedResponses := sets.NewString()

						if utilnet.IsIPv6String(nodeAddress.Address) {
							expectedResponses.Insert(node.Name+"-ep", externalIpv6)
						} else {
							expectedResponses.Insert(node.Name+"-ep", externalIpv4)
						}

						valid := false
						nodePort := nodeTCPPort
						if protocol == "udp" {
							nodePort = nodeUDPPort
						}

						ginkgo.By("Hitting the nodeport on " + node.Name + " and trying to reach only the local endpoint with protocol " + protocol)

						for i := 0; i < maxTries; i++ {
							epHostname := pokeEndpointHostname(clientContainerName, protocol, nodeAddress.Address, nodePort)
							epClientIP := pokeEndpointClientIP(clientContainerName, protocol, nodeAddress.Address, nodePort)
							responses.Insert(epHostname, epClientIP)

							if responses.Equal(expectedResponses) {
								framework.Logf("Validated local endpoint on node %s with address %s, and packet src IP ", node.Name, nodeAddress.Address, epClientIP)
								valid = true
								break
							}

						}
						framework.ExpectEqual(valid, true, "Validation failed for node", node.Name, responses, nodePort)
					}
				}
			}
		})
		// This test validates ingress traffic to externalservices.
		// It creates a service on both udp and tcp and assignes all the first node's addresses as
		// external addresses. Then, creates a backend pod on each node.
		// The backend pods are using the agnhost - netexec command which replies to commands
		// with different protocols. We use the "hostname" command to have each backend pod to reply
		// with its hostname.
		// We use an external container to poke the service exposed on the node and we iterate until
		// all the hostnames are returned.
		// In case of dual stack enabled cluster, we iterate over all the node's addresses and try to hit the
		// endpoints from both each node's ips.
		ginkgo.It("Should be allowed by externalip services", func() {
			serviceName := "externalipsvc"
			serviceName2 := "externalipsvc2"

			// collecting all the first node's addresses
			addresses := []string{}
			for _, a := range nodes.Items[0].Status.Addresses {
				if addressIsIP(a) {
					addresses = append(addresses, a.Address)
				}
			}

			// We will create 2 services, test the first, then delete the first service
			// Deleting the first externalip service should not affect the ARP/NS redirect rule pushed into OVS breth0 and the second service should still behave
			// correctly after deletion of the first service
			ginkgo.By("Creating the first externalip service")
			externalIPsvcSpec := externalIPServiceSpecFrom(serviceName, endpointHTTPPort, endpointUDPPort, clusterHTTPPort, clusterUDPPort, endpointsSelector, addresses)
			_, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), externalIPsvcSpec, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("Creating the second externalip service on the same VIP")
			externalIPsvcSpec2 := externalIPServiceSpecFrom(serviceName2, endpointHTTPPort, endpointUDPPort, clusterHTTPPort2, clusterUDPPort2, endpointsSelector, addresses)
			_, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), externalIPsvcSpec2, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for the endpoints to pop up")
			err = framework.WaitForServiceEndpointsNum(f.ClientSet, f.Namespace.Name, serviceName, len(endPoints), time.Second, wait.ForeverTestTimeout)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", serviceName, f.Namespace.Name)

			for _, externalAddress := range addresses {
				ginkgo.By(fmt.Sprintf("Making sure that the neighbor entry is stable for endpoint IP %s", externalAddress))
				valid := isNeighborEntryStable(clientContainerName, externalAddress, 10)
				framework.ExpectEqual(valid, true, "Validation failed for neighbor entry of external address: %s", externalAddress)

				for _, protocol := range []string{"http", "udp"} {
					externalPort := int32(clusterHTTPPort)
					if protocol == "udp" {
						externalPort = int32(clusterUDPPort)
					}
					ginkgo.By(
						fmt.Sprintf("Hitting the external service on IP %s, protocol %s, port %d and reaching all the endpoints",
							externalAddress,
							protocol,
							externalPort))
					valid = pokeExternalIpService(clientContainerName, protocol, externalAddress, externalPort, maxTries, nodesHostnames)
					framework.ExpectEqual(valid, true, "Validation failed for external address", externalAddress)
				}
			}

			// Deleting the first externalip service should not affect the ARP/NS redirect rules
			ginkgo.By("Deleting the first externalip service")
			err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Delete(context.Background(), serviceName, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete the first external IP service for service %s in namespace: %s", serviceName, f.Namespace.Name)

			for _, externalAddress := range addresses {
				ginkgo.By(fmt.Sprintf("Making sure that the neighbor entry is stable for endpoint IP %s", externalAddress))
				valid := isNeighborEntryStable(clientContainerName, externalAddress, 10)
				framework.ExpectEqual(valid, true, "Validation failed for neighbor entry of external address: %s", externalAddress)

				for _, protocol := range []string{"http", "udp"} {
					externalPort := int32(clusterHTTPPort2)
					if protocol == "udp" {
						externalPort = int32(clusterUDPPort2)
					}
					ginkgo.By(
						fmt.Sprintf("Hitting the external service on IP %s, protocol %s, port %d and reaching all the endpoints",
							externalAddress,
							protocol,
							externalPort))
					valid = pokeExternalIpService(clientContainerName, protocol, externalAddress, externalPort, maxTries, nodesHostnames)
					framework.ExpectEqual(valid, true, "Validation failed for external address", externalAddress)
				}
			}
		})
	})
	ginkgo.Context("Validating ExternalIP ingress traffic to manually added node IPs", func() {
		ginkgo.BeforeEach(func() {
			endPoints = make([]*v1.Pod, 0)
			nodesHostnames = sets.NewString()

			var err error
			nodes, err = e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 3)
			framework.ExpectNoError(err)

			if len(nodes.Items) < 3 {
				framework.Failf(
					"Test requires >= 3 Ready nodes, but there are only %v nodes",
					len(nodes.Items))
			}

			ginkgo.By("Creating the endpoints pod, one for each worker")
			for _, node := range nodes.Items {
				// this create a udp / http netexec listener which is able to receive the "hostname"
				// command. We use this to validate that each endpoint is received at least once
				args := []string{
					"netexec",
					fmt.Sprintf("--http-port=%d", endpointHTTPPort),
					fmt.Sprintf("--udp-port=%d", endpointUDPPort),
				}
				pod, err := createPod(f, node.Name+"-ep", node.Name, f.Namespace.Name, []string{}, endpointsSelector, func(p *v1.Pod) {
					p.Spec.Containers[0].Args = args
				})
				framework.ExpectNoError(err)
				endPoints = append(endPoints, pod)
				nodesHostnames.Insert(pod.Name)

				// this is arbitrary and mutuated from k8s network e2e tests. We aim to hit all the endpoints at least once
				maxTries = len(endPoints)*len(endPoints) + 30
			}

			ginkgo.By("Creating an external container to send the traffic from")
			// the client uses the netexec command from the agnhost image, which is able to receive commands for poking other
			// addresses.
			createClusterExternalContainer(clientContainerName, agnhostImage, []string{"--network", "kind", "-P"}, []string{"netexec", "--http-port=80"})

			ginkgo.By("Adding ip addresses to each node")
			// add new secondary IP from node subnet to all nodes, if the cluster is v6 add an ipv6 address
			var newIP string
			for i, node := range nodes.Items {
				if utilnet.IsIPv6String(e2enode.GetAddresses(&node, v1.NodeInternalIP)[0]) {
					newIP = "fc00:f853:ccd:e794::" + strconv.Itoa(i)
				} else {
					newIP = "172.18.1." + strconv.Itoa(i)
				}
				// manually add the a secondary IP to each node
				_, err := runCommand("docker", "exec", node.Name, "ip", "addr", "add", newIP, "dev", "breth0")
				if err != nil {
					framework.Failf("failed to add new Addresses to node %s: %v", node.Name, err)
				}

				newNodeAddresses = append(newNodeAddresses, newIP)
			}
		})

		ginkgo.AfterEach(func() {
			deleteClusterExternalContainer(clientContainerName)

			for i, node := range nodes.Items {
				// delete the secondary IP previoulsy added to the nodes
				_, err := runCommand("docker", "exec", node.Name, "ip", "addr", "delete", newNodeAddresses[i], "dev", "breth0")
				if err != nil {
					framework.Failf("failed to delete new Addresses to node %s: %v", node.Name, err)
				}
			}
		})
		// This test validates ingress traffic to externalservices after a new node Ip is added.
		// It creates a service on both udp and tcp and assigns the new node IPs as
		// external Addresses. Then, creates a backend pod on each node.
		// The backend pods are using the agnhost - netexec command which replies to commands
		// with different protocols. We use the "hostname" command to have each backend pod to reply
		// with its hostname.
		// We use an external container to poke the service exposed on the node and we iterate until
		// all the hostnames are returned.
		ginkgo.It("Should be allowed by externalip services to a new node ip", func() {
			serviceName := "externalipsvc"

			ginkgo.By("Creating the externalip service")
			externalIPsvcSpec := externalIPServiceSpecFrom(serviceName, endpointHTTPPort, endpointUDPPort, clusterHTTPPort, clusterUDPPort, endpointsSelector, newNodeAddresses)
			_, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), externalIPsvcSpec, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for the endpoints to pop up")
			err = framework.WaitForServiceEndpointsNum(f.ClientSet, f.Namespace.Name, serviceName, len(endPoints), time.Second, wait.ForeverTestTimeout)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", serviceName, f.Namespace.Name)

			for _, protocol := range []string{"http", "udp"} {
				for _, externalAddress := range newNodeAddresses {
					responses := sets.NewString()
					valid := false
					externalPort := int32(clusterHTTPPort)
					if protocol == "udp" {
						externalPort = int32(clusterUDPPort)
					}

					ginkgo.By("Hitting the external service on " + externalAddress + " and reaching all the endpoints " + protocol)
					for i := 0; i < maxTries; i++ {
						epHostname := pokeEndpointHostname(clientContainerName, protocol, externalAddress, externalPort)
						responses.Insert(epHostname)

						// each endpoint returns its hostname. By doing this, we validate that each ep was reached at least once.
						if responses.Equal(nodesHostnames) {
							framework.Logf("Validated external address %s after %d tries", externalAddress, i)
							valid = true
							break
						}
					}
					framework.ExpectEqual(valid, true, "Validation failed for external address", externalAddress)
				}
			}
		})
	})
})

var _ = ginkgo.Describe("e2e ingress to host-networked pods traffic validation", func() {
	const (
		endpointHTTPPort = 8085
		endpointUDPPort  = 9095
		clusterHTTPPort  = 81
		clusterUDPPort   = 91

		clientContainerName = "npclient"
	)

	f := newPrivelegedTestFramework("nodeport-ingress-test")
	hostNetEndpointsSelector := map[string]string{"hostNetservicebackend": "true"}
	var endPoints []*v1.Pod
	var nodesHostnames sets.String
	maxTries := 0
	var nodes *v1.NodeList
	var externalIpv4 string
	var externalIpv6 string

	// This test validates ingress traffic to nodeports with externalTrafficPolicy Set to local.
	// It creates a nodeport service on both udp and tcp, and creates a host networked
	// backend pod on each node. The backend pod is using the agnhost - netexec command which
	// replies to commands with different protocols. We use the "hostname" and "clientip" commands
	// to have each backend pod to reply with its hostname and the request packet's srcIP.
	// We use an external container to poke the service exposed on the node and ensure that only the
	// nodeport on the node with the backend actually receives traffic and that the packet is not
	// SNATed.
	ginkgo.Context("Validating ingress traffic to Host Networked pods with externalTrafficPolicy=local", func() {
		ginkgo.BeforeEach(func() {
			endPoints = make([]*v1.Pod, 0)
			nodesHostnames = sets.NewString()

			var err error
			nodes, err = e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 3)
			framework.ExpectNoError(err)

			if len(nodes.Items) < 3 {
				framework.Failf(
					"Test requires >= 3 Ready nodes, but there are only %v nodes",
					len(nodes.Items))
			}

			ginkgo.By("Creating the endpoints pod, one for each worker")
			for _, node := range nodes.Items {
				// this create a udp / http netexec listener which is able to receive the "hostname"
				// command. We use this to validate that each endpoint is received at least once
				args := []string{
					"netexec",
					fmt.Sprintf("--http-port=%d", endpointHTTPPort),
					fmt.Sprintf("--udp-port=%d", endpointUDPPort),
				}

				// create hostNeworkedPods
				hostNetPod, err := createPod(f, node.Name+"-hostnet-ep", node.Name, f.Namespace.Name, []string{}, hostNetEndpointsSelector, func(p *v1.Pod) {
					p.Spec.Containers[0].Args = args
					p.Spec.HostNetwork = true
				})

				framework.ExpectNoError(err)
				endPoints = append(endPoints, hostNetPod)
				nodesHostnames.Insert(hostNetPod.Name)

				// this is arbitrary and mutuated from k8s network e2e tests. We aim to hit all the endpoints at least once
				maxTries = len(endPoints)*len(endPoints) + 30
			}

			ginkgo.By("Creating an external container to send the traffic from")
			// the client uses the netexec command from the agnhost image, which is able to receive commands for poking other
			// addresses.
			externalIpv4, externalIpv6 = createClusterExternalContainer(clientContainerName, agnhostImage, []string{"--network", "kind", "-P"}, []string{"netexec", "--http-port=80"})
		})

		ginkgo.AfterEach(func() {
			deleteClusterExternalContainer(clientContainerName)
			// f.Delete will delete the namespace and run WaitForNamespacesDeleted
			// This is inside the Context and will happen before the framework's teardown inside the Describe
			f.DeleteNamespace(f.Namespace.Name)
		})

		// Make sure ingress traffic can reach host pod backends for a service without SNAT when externalTrafficPolicy is set to local
		ginkgo.It("Should be allowed to node local host-networked endpoints by nodeport services", func() {
			serviceName := "nodeportsvclocalhostnet"
			ginkgo.By("Creating the nodeport service")
			npSpec := nodePortServiceSpecFrom(serviceName, v1.IPFamilyPolicyPreferDualStack, endpointHTTPPort, endpointUDPPort, clusterHTTPPort, clusterUDPPort, hostNetEndpointsSelector, v1.ServiceExternalTrafficPolicyTypeLocal)
			np, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.Background(), npSpec, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			nodeTCPPort, nodeUDPPort := nodePortsFromService(np)

			ginkgo.By("Waiting for the endpoints to pop up")
			err = framework.WaitForServiceEndpointsNum(f.ClientSet, f.Namespace.Name, serviceName, len(endPoints), time.Second, wait.ForeverTestTimeout)
			framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", serviceName, f.Namespace.Name)

			for _, protocol := range []string{"http", "udp"} {
				for _, node := range nodes.Items {
					for _, nodeAddress := range node.Status.Addresses {
						// skipping hostnames
						if !addressIsIP(nodeAddress) {
							continue
						}

						responses := sets.NewString()
						// Fill expected responses, it should hit the nodeLocal endpoints and not SNAT packet IP
						expectedResponses := sets.NewString()

						if utilnet.IsIPv6String(nodeAddress.Address) {
							expectedResponses.Insert(node.Name, externalIpv6)
						} else {
							expectedResponses.Insert(node.Name, externalIpv4)
						}

						valid := false
						nodePort := nodeTCPPort
						if protocol == "udp" {
							nodePort = nodeUDPPort
						}

						ginkgo.By("Hitting the nodeport on " + node.Name + " and trying to reach only the local endpoint with protocol " + protocol)
						for i := 0; i < maxTries; i++ {
							epHostname := pokeEndpointHostname(clientContainerName, protocol, nodeAddress.Address, nodePort)
							epClientIP := pokeEndpointClientIP(clientContainerName, protocol, nodeAddress.Address, nodePort)
							responses.Insert(epHostname, epClientIP)

							if responses.Equal(expectedResponses) {
								framework.Logf("Validated local endpoint on node %s with address %s, and packet src IP %s ", node.Name, nodeAddress.Address, epClientIP)
								valid = true
								break
							}

						}
						framework.ExpectEqual(valid, true, "Validation failed for node", node.Name, responses, nodePort)
					}
				}
			}
		})
	})
})

// This test validates ingress traffic sourced from a mock external gateway
// running as a container. Add a namespace annotated with the IP of the
// mock external container's eth0 address. Add a loopback address and a
// route pointing to the pod in the test namespace. Validate connectivity
// sourcing from the mock gateway container loopback to the test ns pod.
var _ = ginkgo.Describe("e2e ingress gateway traffic validation", func() {
	const (
		svcname     string = "novxlan-externalgw-ingress"
		gwContainer string = "gw-ingress-test-container"
	)

	f := newPrivelegedTestFramework(svcname)

	type nodeInfo struct {
		name   string
		nodeIP string
	}

	var (
		workerNodeInfo nodeInfo
		IsIPv6         bool
	)

	ginkgo.BeforeEach(func() {

		// retrieve worker node names
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(f.ClientSet, 3)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 3 {
			framework.Failf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}
		ips := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)
		workerNodeInfo = nodeInfo{
			name:   nodes.Items[1].Name,
			nodeIP: ips[1],
		}
		IsIPv6 = IsIPv6Cluster(f.ClientSet)
	})

	ginkgo.AfterEach(func() {
		// tear down the container simulating the gateway
		if cid, _ := runCommand("docker", "ps", "-qaf", fmt.Sprintf("name=%s", gwContainer)); cid != "" {
			if _, err := runCommand("docker", "rm", "-f", gwContainer); err != nil {
				framework.Logf("failed to delete the gateway test container %s %v", gwContainer, err)
			}
		}
	})

	ginkgo.It("Should validate ingress connectivity from an external gateway", func() {

		var (
			pingDstPod     string
			dstPingPodName = "e2e-exgw-ingress-ping-pod"
			command        = []string{"bash", "-c", "sleep 20000"}
			exGWLo         = "10.30.1.1"
			exGWLoCidr     = fmt.Sprintf("%s/32", exGWLo)
			pingCmd        = ipv4PingCommand
			pingCount      = "3"
		)
		if IsIPv6 {
			exGWLo = "fc00::1" // unique local ipv6 unicast addr as per rfc4193
			exGWLoCidr = fmt.Sprintf("%s/64", exGWLo)
			pingCmd = ipv6PingCommand
		}

		// start the first container that will act as an external gateway
		_, err := runCommand("docker", "run", "-itd", "--privileged", "--network", externalContainerNetwork, "--name", gwContainer, "centos/tools")
		if err != nil {
			framework.Failf("failed to start external gateway test container %s: %v", gwContainer, err)
		}
		exGWIp, exGWIpv6 := getContainerAddressesForNetwork(gwContainer, externalContainerNetwork)
		if IsIPv6 {
			exGWIp = exGWIpv6
		}
		// annotate the test namespace with the external gateway address
		annotateArgs := []string{
			"annotate",
			"namespace",
			f.Namespace.Name,
			fmt.Sprintf("k8s.ovn.org/routing-external-gws=%s", exGWIp),
		}
		framework.Logf("Annotating the external gateway test namespace to container gateway: %s", exGWIp)
		framework.RunKubectlOrDie(f.Namespace.Name, annotateArgs...)

		nodeIP, nodeIPv6 := getContainerAddressesForNetwork(workerNodeInfo.name, externalContainerNetwork)
		if IsIPv6 {
			nodeIP = nodeIPv6
		}
		framework.Logf("the pod side node is %s and the source node ip is %s", workerNodeInfo.name, nodeIP)
		podCIDR, err := getNodePodCIDR(workerNodeInfo.name)
		if err != nil {
			framework.Failf("Error retrieving the pod cidr from %s %v", workerNodeInfo.name, err)
		}
		framework.Logf("the pod cidr for node %s is %s", workerNodeInfo.name, podCIDR)

		// Create the pod that will be used as the source for the connectivity test
		createGenericPod(f, dstPingPodName, workerNodeInfo.name, f.Namespace.Name, command)
		// wait for the pod setup to return a valid address
		err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			pingDstPod = getPodAddress(dstPingPodName, f.Namespace.Name)
			validIP := net.ParseIP(pingDstPod)
			if validIP == nil {
				return false, nil
			}
			return true, nil
		})
		// fail the test if a pod address is never retrieved
		if err != nil {
			framework.Failf("Error trying to get the pod IP address")
		}
		// add a host route on the gateway for return traffic to the pod
		_, err = runCommand("docker", "exec", gwContainer, "ip", "route", "add", pingDstPod, "via", nodeIP)
		if err != nil {
			framework.Failf("failed to add the pod host route on the test container %s: %v", gwContainer, err)
		}
		// add a loopback address to the mock container that will source the ingress test
		_, err = runCommand("docker", "exec", gwContainer, "ip", "address", "add", exGWLoCidr, "dev", "lo")
		if err != nil {
			framework.Failf("failed to add the loopback ip to dev lo on the test container: %v", err)
		}

		// Validate connectivity from the external gateway loopback to the pod in the test namespace
		ginkgo.By(fmt.Sprintf("Validate ingress traffic from the external gateway %s can reach the pod in the exgw annotated namespace", gwContainer))
		// generate traffic that will verify connectivity from the mock external gateway loopback
		_, err = runCommand("docker", "exec", gwContainer, string(pingCmd), "-c", pingCount, "-I", "eth0", pingDstPod)
		if err != nil {
			framework.Failf("failed to ping the pod address %s from mock container %s: %v", pingDstPod, gwContainer, err)
		}
	})
})

// This test validates that OVS exports flow monitoring data from br-int to an external collector
var _ = ginkgo.Describe("e2e br-int flow monitoring export validation", func() {
	type flowMonitoringProtocol string

	const (
		netflow_v5 flowMonitoringProtocol = "netflow"
		ipfix      flowMonitoringProtocol = "ipfix"
		sflow      flowMonitoringProtocol = "sflow"

		svcname            string = "netflow-test"
		ovnNs              string = "ovn-kubernetes"
		collectorContainer string = "netflow-collector"
		ciNetworkName      string = "kind"
	)

	keywordInLogs := map[flowMonitoringProtocol]string{
		netflow_v5: "NETFLOW_V5", ipfix: "IPFIX", sflow: "SFLOW_5"}

	f := newPrivelegedTestFramework(svcname)
	ginkgo.AfterEach(func() {
		// tear down the collector container
		if cid, _ := runCommand("docker", "ps", "-qaf", fmt.Sprintf("name=%s", collectorContainer)); cid != "" {
			if _, err := runCommand("docker", "rm", "-f", collectorContainer); err != nil {
				framework.Logf("failed to delete the collector test container %s %v",
					collectorContainer, err)
			}
		}
	})

	table.DescribeTable("Should validate flow data of br-int is sent to an external gateway",
		func(protocol flowMonitoringProtocol, collectorPort uint16) {
			protocolStr := string(protocol)
			ipField := "IPAddress"
			isIpv6 := IsIPv6Cluster(f.ClientSet)
			if isIpv6 {
				ipField = "GlobalIPv6Address"
			}
			ciNetworkFlag := fmt.Sprintf("{{ .NetworkSettings.Networks.kind.%s }}", ipField)

			ginkgo.By("Starting a flow collector container")
			// start the collector container that will receive data
			_, err := runCommand("docker", "run", "-itd", "--privileged", "--network", ciNetworkName,
				"--name", collectorContainer, "cloudflare/goflow", "-kafka=false")
			if err != nil {
				framework.Failf("failed to start flow collector container %s: %v", collectorContainer, err)
			}
			ovnEnvVar := fmt.Sprintf("OVN_%s_TARGETS", strings.ToUpper(protocolStr))
			// retrieve the ip of the collector container
			collectorIP, err := runCommand("docker", "inspect", "-f", ciNetworkFlag, collectorContainer)
			if err != nil {
				framework.Failf("could not retrieve IP address of collector container: %v", err)
			}
			// trim newline from the inspect output
			collectorIP = strings.TrimSpace(collectorIP)
			if net.ParseIP(collectorIP) == nil {
				framework.Failf("Unable to retrieve a valid address from container %s with inspect output of %s",
					collectorContainer, collectorIP)
			}
			addressAndPort := net.JoinHostPort(collectorIP, strconv.Itoa(int(collectorPort)))
			ginkgo.By(fmt.Sprintf("Configuring ovnkube-node to use the new %s collector target", protocolStr))
			framework.Logf("Setting %s environment variable to %s",
				ovnEnvVar, addressAndPort)

			framework.RunKubectlOrDie(ovnNs, "set", "env", "daemonset/ovnkube-node", "-c", "ovnkube-node",
				fmt.Sprintf("%s=%s", ovnEnvVar, addressAndPort))

			// Make sure the updated daemonset has rolled out
			// TODO (Change this to use the exported upstream function)
			err = waitForDaemonSetUpdate(f.ClientSet, ovnNs, "ovnkube-node", 0, dsRestartTimeout)
			framework.ExpectNoError(err)
			ginkgo.By(fmt.Sprintf("Checking that the collector container received %s data", protocolStr))
			keyword := keywordInLogs[protocol]
			collectorContainerLogsTest := func() wait.ConditionFunc {
				return func() (bool, error) {
					collectorContainerLogs, err := runCommand("docker", "logs", collectorContainer)
					if err != nil {
						framework.Logf("failed to inspect logs in test container: %v", err)
						return false, nil
					}
					collectorContainerLogs = strings.TrimSuffix(collectorContainerLogs, "\n")
					logLines := strings.Split(collectorContainerLogs, "\n")
					lastLine := logLines[len(logLines)-1]
					// check that flow monitoring traffic has been logged
					if strings.Contains(lastLine, keyword) {
						framework.Logf("Successfully found string %s in last log line of"+
							" the collector: %s", keyword, lastLine)
						return true, nil
					}
					framework.Logf("%s not found in last log line: %s", keyword, lastLine)
					return false, nil
				}
			}
			err = wait.PollImmediate(retryInterval, retryTimeout, collectorContainerLogsTest())
			framework.ExpectNoError(err, fmt.Sprintf("failed to verify that collector container "+
				"received %s data from br-int: string %s not found in logs",
				protocolStr, keyword))

			ginkgo.By(fmt.Sprintf("Unsetting %s variable in ovnkube-node daemonset", ovnEnvVar))
			framework.RunKubectlOrDie(ovnNs, "set", "env", "daemonset/ovnkube-node", "-c", "ovnkube-node",
				fmt.Sprintf("%s-", ovnEnvVar))

			// Make sure the updated daemonset has rolled out
			// TODO (Change this to use the exported upstream function)
			err = waitForDaemonSetUpdate(f.ClientSet, ovnNs, "ovnkube-node", 0, dsRestartTimeout)
			framework.ExpectNoError(err)

			ovnKubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNs).List(context.TODO(), metav1.ListOptions{
				LabelSelector: "name=ovnkube-node",
			})
			if err != nil {
				framework.Failf("could not get ovnkube-node pods: %v", err)
			}

			for _, ovnKubeNodePod := range ovnKubeNodePods.Items {

				execOptions := framework.ExecOptions{
					Command:       []string{"ovs-vsctl", "find", strings.ToLower(protocolStr)},
					Namespace:     ovnNs,
					PodName:       ovnKubeNodePod.Name,
					ContainerName: "ovnkube-node",
					CaptureStdout: true,
					CaptureStderr: true,
				}

				targets, stderr, _ := f.ExecWithOptions(execOptions)
				framework.Logf("execOptions are %v", execOptions)
				if err != nil {
					framework.Failf("could not lookup ovs %s targets: %v", protocolStr, stderr)
				}
				framework.ExpectEmpty(targets)
			}
		},
		// This is a long test (~5 minutes per run), so let's just validate netflow v5
		// in an IPv4 cluster and sflow in IPv6 cluster
		table.Entry("with netflow v5", netflow_v5, uint16(2056)),
		// goflow doesn't currently support OVS ipfix:
		// https://github.com/cloudflare/goflow/issues/99
		// table.Entry("ipfix", ipfix, uint16(2055)),
		table.Entry("with sflow", sflow, uint16(6343)),
	)

})

func getNodePodCIDR(nodeName string) (string, error) {
	// retrieve the pod cidr for the worker node
	jsonFlag := "jsonpath='{.metadata.annotations.k8s\\.ovn\\.org/node-subnets}'"
	kubectlOut, err := framework.RunKubectl("default", "get", "node", nodeName, "-o", jsonFlag)
	if err != nil {
		return "", err
	}
	// strip the apostrophe from stdout and parse the pod cidr
	annotation := strings.Replace(kubectlOut, "'", "", -1)

	ssSubnets := make(map[string]string)
	if err := json.Unmarshal([]byte(annotation), &ssSubnets); err == nil {
		return ssSubnets["default"], nil
	}
	dsSubnets := make(map[string][]string)
	if err := json.Unmarshal([]byte(annotation), &dsSubnets); err == nil {
		return dsSubnets["default"][0], nil
	}
	return "", fmt.Errorf("could not parse annotation %q", annotation)
}

var _ = ginkgo.Describe("e2e delete databases", func() {
	const (
		svcname                   string = "delete-db"
		ovnNs                     string = "ovn-kubernetes"
		databasePodPrefix         string = "ovnkube-db"
		databasePodNorthContainer string = "nb-ovsdb"
		northDBFileName           string = "ovnnb_db.db"
		southDBFileName           string = "ovnsb_db.db"
		dirDB                     string = "/etc/ovn"
		ovnWorkerNode             string = "ovn-worker"
		ovnWorkerNode2            string = "ovn-worker2"
		haModeMinDb               int    = 0
		haModeMaxDb               int    = 2
	)
	var (
		allDBFiles = []string{path.Join(dirDB, northDBFileName), path.Join(dirDB, southDBFileName)}
	)
	f := newPrivelegedTestFramework(svcname)

	// WaitForPodConditionAllowNotFoundError is a wrapper for WaitForPodCondition that allows at most 6 times for the pod not to be found.
	WaitForPodConditionAllowNotFoundErrors := func(f *framework.Framework, ns, podName, desc string, timeout time.Duration, condition podCondition) error {
		max_tries := 6               // 6 tries to waiting for the pod to restart
		cooldown := 10 * time.Second // 10 sec to cooldown between each try
		for i := 0; i < max_tries; i++ {
			err := e2epod.WaitForPodCondition(f.ClientSet, ns, podName, desc, 5*time.Minute, condition)
			if apierrors.IsNotFound(err) {
				// pod not found,try again after cooldown
				time.Sleep(cooldown)
				continue
			}
			if err != nil {
				return err
			}
			return nil
		}
		return fmt.Errorf("gave up after waiting %v for pod %q to be %q: pod is not found", timeout, podName, desc)
	}

	// waitForPodToFinishFullRestart waits for a the pod to finish its reset cycle and returns.
	waitForPodToFinishFullRestart := func(f *framework.Framework, pod *v1.Pod) {
		podClient := f.ClientSet.CoreV1().Pods(pod.Namespace)
		// loop until pod with new UID exists
		err := wait.PollImmediate(retryInterval, 5*time.Minute, func() (bool, error) {
			newPod, err := podClient.Get(context.Background(), pod.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			} else if err != nil {
				return false, err
			}

			return pod.UID != newPod.UID, nil
		})
		framework.ExpectNoError(err)

		// during this stage on the restarting process we can encounter "pod not found" errors.
		// these types of errors are valid because the pod is restarting so there will be a period of time it is unavailable
		// so we will use "WaitForPodConditionAllowNotFoundErrors" in order to handle properly those errors.
		err = WaitForPodConditionAllowNotFoundErrors(f, pod.Namespace, pod.Name, "running and ready", 5*time.Minute, testutils.PodRunningReady)
		if err != nil {
			framework.Failf("pod %v did not reach running and ready state: %v", pod.Name, err)
		}
	}

	deletePod := func(f *framework.Framework, namespace string, podName string) {
		podClient := f.ClientSet.CoreV1().Pods(namespace)
		_, err := podClient.Get(context.Background(), podName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}

		err = podClient.Delete(context.Background(), podName, metav1.DeleteOptions{})
		framework.ExpectNoError(err, "failed to delete pod "+podName)
	}

	fileExistsOnPod := func(f *framework.Framework, namespace string, pod *v1.Pod, file string) bool {
		containerFlag := fmt.Sprintf("-c=%s", pod.Spec.Containers[0].Name)
		_, err := framework.RunKubectl(ovnNs, "exec", pod.Name, containerFlag, "--", "ls", file)
		if err == nil {
			return true
		}
		if strings.Contains(err.Error(), fmt.Sprintf("ls: cannot access '%s': No such file or directory", file)) {
			return false
		}
		framework.Failf("failed to check if file %s exists on pod: %s, err: %v", file, pod.Name, err)
		return false
	}

	getDeployment := func(f *framework.Framework, namespace string, deploymentName string) *appsv1.Deployment {
		deploymentClient := f.ClientSet.AppsV1().Deployments(namespace)
		deployment, err := deploymentClient.Get(context.TODO(), deploymentName, metav1.GetOptions{})
		framework.ExpectNoError(err, "should get %s deployment", deploymentName)

		return deployment
	}

	allFilesExistOnPod := func(f *framework.Framework, namespace string, pod *v1.Pod, files []string) bool {
		for _, file := range files {
			if !fileExistsOnPod(f, namespace, pod, file) {
				framework.Logf("file %s not exists", file)
				return false
			}
			framework.Logf("file %s exists", file)
		}
		return true
	}

	deleteFileFromPod := func(f *framework.Framework, namespace string, pod *v1.Pod, file string) {
		containerFlag := fmt.Sprintf("-c=%s", pod.Spec.Containers[0].Name)
		framework.RunKubectl(ovnNs, "exec", pod.Name, containerFlag, "--", "rm", file)
		if fileExistsOnPod(f, namespace, pod, file) {
			framework.Failf("Error: failed to delete file %s", file)
		}
		framework.Logf("file %s deleted ", file)
	}

	singlePodConnectivityTest := func(f *framework.Framework, podName string) {
		framework.Logf("Running container which tries to connect to API server in a loop")
		podChan, errChan := make(chan *v1.Pod), make(chan error)

		go func() {
			defer ginkgo.GinkgoRecover()
			checkContinuousConnectivity(f, "", podName, getApiAddress(), 443, 10, 30, podChan, errChan)
		}()
		testPod := <-podChan

		framework.Logf("Test pod running on %q", testPod.Spec.NodeName)
		framework.ExpectNoError(<-errChan)
	}

	twoPodsContinuousConnectivityTest := func(f *framework.Framework,
		node1Name string, node2Name string,
		syncChan chan string, errChan chan error) {
		const (
			pod1Name                  string        = "connectivity-test-pod1"
			pod2Name                  string        = "connectivity-test-pod2"
			port                      string        = "8080"
			timeIntervalBetweenChecks time.Duration = 2 * time.Second
		)

		var (
			command = []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=" + port)}
		)
		createGenericPod(f, pod1Name, node1Name, f.Namespace.Name, command)
		createGenericPod(f, pod2Name, node2Name, f.Namespace.Name, command)

		pod2IP := getPodAddress(pod2Name, f.Namespace.Name)

		ginkgo.By("Checking initial connectivity from one pod to the other and verifying that the connection is achieved")

		stdout, err := framework.RunKubectl(f.Namespace.Name, "exec", pod1Name, "--", "curl", fmt.Sprintf("%s/hostname", net.JoinHostPort(pod2IP, port)))

		if err != nil || stdout != pod2Name {
			framework.Failf("Error: attempted connection to pod %s found err:  %v", pod2Name, err)
		}

		syncChan <- "connectivity test pods are ready"

	L:
		for {
			select {
			case msg := <-syncChan:
				framework.Logf(msg + ": finish connectivity test.")
				break L
			default:
				stdout, err := framework.RunKubectl(f.Namespace.Name, "exec", pod1Name, "--", "curl", fmt.Sprintf("%s/hostname", net.JoinHostPort(pod2IP, port)))
				if err != nil || stdout != pod2Name {
					errChan <- err
					framework.Failf("Error: attempted connection to pod %s found err:  %v", pod2Name, err)
				}
				time.Sleep(timeIntervalBetweenChecks)
			}
		}

		errChan <- nil
	}

	table.DescribeTable("recovering from deleting db files while maintaining connectivity",
		func(db_pod_num int, DBFileNamesToDelete []string) {
			var (
				db_pod_name = fmt.Sprintf("%s-%d", databasePodPrefix, db_pod_num)
			)
			if db_pod_num < haModeMinDb || db_pod_num > haModeMaxDb {
				framework.Failf("invalid db_pod_num.")
				return
			}

			// Adding db file path
			for i, file := range DBFileNamesToDelete {
				DBFileNamesToDelete[i] = path.Join(dirDB, file)
			}

			framework.Logf("connectivity test before deleting db files")
			framework.Logf("test simple connectivity from new pod to API server, before deleting db files")
			singlePodConnectivityTest(f, "before-delete-db-files")
			framework.Logf("setup two pods for continuous connectivity test")
			syncChan, errChan := make(chan string), make(chan error)
			go func() {
				defer ginkgo.GinkgoRecover()
				twoPodsContinuousConnectivityTest(f, ovnWorkerNode, ovnWorkerNode2, syncChan, errChan)
			}()
			// wait for the connectivity test pods to be ready
			framework.Logf(<-syncChan + ": delete and restart db pods.")

			// Start the db disruption - delete the db files and delete the db-pod in order to emulate the cluster/pod restart

			// Retrieve the DB pod
			dbPod, err := f.ClientSet.CoreV1().Pods(ovnNs).Get(context.Background(), db_pod_name, metav1.GetOptions{})
			framework.ExpectNoError(err, fmt.Sprintf("unable to get pod: %s, err: %v", db_pod_name, err))

			// Check that all files are on the db pod
			framework.Logf("make sure that all the db files are on db pod %s", dbPod.Name)
			if !allFilesExistOnPod(f, ovnNs, dbPod, allDBFiles) {
				framework.Failf("Error: db files not found")
			}
			// Delete the db files from the db-pod
			framework.Logf("deleting db files from db pod")
			for _, db_file := range DBFileNamesToDelete {
				deleteFileFromPod(f, ovnNs, dbPod, db_file)
			}
			// Delete the db-pod in order to emulate the cluster/pod restart
			framework.Logf("deleting db pod %s", dbPod.Name)
			deletePod(f, ovnNs, dbPod.Name)

			framework.Logf("wait for db pod to finish full restart")
			waitForPodToFinishFullRestart(f, dbPod)

			// Check db files existence
			// Check that all files are on pod
			framework.Logf("make sure that all the db files are on db pod %s", dbPod.Name)
			if !allFilesExistOnPod(f, ovnNs, dbPod, allDBFiles) {
				framework.Failf("Error: db files not found")
			}

			// disruption over.
			syncChan <- "disruption over"
			framework.ExpectNoError(<-errChan)

			framework.Logf("test simple connectivity from new pod to API server, after recovery")
			singlePodConnectivityTest(f, "after-delete-db-files")
		},

		// One can choose to delete only specific db file (uncomment the requested lines)

		// db pod 0
		table.Entry("when deleting both db files on ovnkube-db-0", 0, []string{northDBFileName, southDBFileName}),
		// table.Entry("when delete north db on ovnkube-db-0", 0, []string{northDBFileName}),
		// table.Entry("when delete south db on ovnkube-db-0", 0, []string{southDBFileName}),

		// db pod 1
		table.Entry("when deleting both db files on ovnkube-db-1", 1, []string{northDBFileName, southDBFileName}),
		// table.Entry("when delete north db on ovnkube-db-1", 1, []string{northDBFileName}),
		// table.Entry("when delete south db on ovnkube-db-1", 1, []string{southDBFileName}),

		// db pod 2
		table.Entry("when deleting both db files on ovnkube-db-2", 2, []string{northDBFileName, southDBFileName}),
		// table.Entry("when delete north db on ovnkube-db-2", 2, []string{northDBFileName}),
		// table.Entry("when delete south db on ovnkube-db-2", 2, []string{southDBFileName}),
	)

	ginkgo.It("Should validate connectivity before and after deleting all the db-pods at once in Non-HA mode", func() {
		dbDeployment := getDeployment(f, ovnNs, "ovnkube-db")
		dbPods, err := e2edeployment.GetPodsForDeployment(f.ClientSet, dbDeployment)
		if err != nil {
			framework.Failf("Error: Failed to get pods, err: %v", err)
		}
		if dbPods.Size() == 0 {
			framework.Failf("Error: db pods not found")
		}

		framework.Logf("test simple connectivity from new pod to API server,before deleting db pods")
		singlePodConnectivityTest(f, "before-delete-db-pods")

		framework.Logf("deleting all the db pods")

		for _, dbPod := range dbPods.Items {
			dbPodName := dbPod.Name
			framework.Logf("deleting db pod: %v", dbPodName)
			// Delete the db-pod in order to emulate the pod restart
			dbPod.Status.Message = "check"
			deletePod(f, ovnNs, dbPodName)
		}

		framework.Logf("wait for all the Deployment to become ready again after pod deletion")
		e2edeployment.WaitForDeploymentComplete(f.ClientSet, dbDeployment)

		framework.Logf("all the pods finish full restart")

		framework.Logf("test simple connectivity from new pod to API server,after recovery")
		singlePodConnectivityTest(f, "after-delete-db-pods")
	})

	ginkgo.It("Should validate connectivity before and after deleting all the db-pods at once in HA mode", func() {
		dbPods, err := e2epod.GetPods(f.ClientSet, ovnNs, map[string]string{"name": databasePodPrefix})
		if err != nil {
			framework.Failf("Error: Failed to get pods, err: %v", err)
		}
		if len(dbPods) == 0 {
			framework.Failf("Error: db pods not found")
		}

		framework.Logf("test simple connectivity from new pod to API server,before deleting db pods")
		singlePodConnectivityTest(f, "before-delete-db-pods")

		framework.Logf("deleting all the db pods")
		for _, dbPod := range dbPods {
			dbPodName := dbPod.Name
			framework.Logf("deleting db pod: %v", dbPodName)
			// Delete the db-pod in order to emulate the pod restart
			dbPod.Status.Message = "check"
			deletePod(f, ovnNs, dbPodName)
		}

		framework.Logf("wait for all the pods to finish full restart")
		var wg sync.WaitGroup
		for _, pod := range dbPods {
			wg.Add(1)
			go func(pod v1.Pod) {
				defer ginkgo.GinkgoRecover()
				defer wg.Done()
				waitForPodToFinishFullRestart(f, &pod)
			}(pod)
		}
		wg.Wait()
		framework.Logf("all the pods finish full restart")

		framework.Logf("test simple connectivity from new pod to API server,after recovery")
		singlePodConnectivityTest(f, "after-delete-db-pods")
	})
})

var _ = ginkgo.Describe("e2e IGMP validation", func() {
	const (
		svcname              string = "igmp-test"
		ovnNs                string = "ovn-kubernetes"
		port                 string = "8080"
		ovnWorkerNode        string = "ovn-worker"
		ovnWorkerNode2       string = "ovn-worker2"
		mcastGroup           string = "224.1.1.1"
		multicastListenerPod string = "multicast-listener-test-pod"
		multicastSourcePod   string = "multicast-source-test-pod"
		tcpdumpFileName      string = "tcpdump.txt"
		retryTimeout                = 5 * time.Minute // polling timeout
	)
	var (
		tcpDumpCommand = []string{"bash", "-c",
			fmt.Sprintf("apk update; apk add tcpdump ; tcpdump multicast > %s", tcpdumpFileName)}
		// Multicast group (-c 224.1.1.1), UDP (-u), TTL (-T 2), during (-t 3000) seconds, report every (-i 5) seconds
		multicastSourceCommand = []string{"bash", "-c",
			fmt.Sprintf("iperf -c %s -u -T 2 -t 3000 -i 5", mcastGroup)}
	)
	f := newPrivelegedTestFramework(svcname)
	ginkgo.It("can retrieve multicast IGMP query", func() {
		// Enable multicast of the test namespace annotation
		ginkgo.By(fmt.Sprintf("annotating namespace: %s to enable multicast", f.Namespace.Name))
		annotateArgs := []string{
			"annotate",
			"namespace",
			f.Namespace.Name,
			fmt.Sprintf("k8s.ovn.org/multicast-enabled=%s", "true"),
		}
		framework.RunKubectlOrDie(f.Namespace.Name, annotateArgs...)

		// Create a multicast source pod
		ginkgo.By("creating a multicast source pod in node " + ovnWorkerNode)
		createGenericPod(f, multicastSourcePod, ovnWorkerNode, f.Namespace.Name, multicastSourceCommand)

		// Create a multicast listener pod
		ginkgo.By("creating a multicast listener pod in node " + ovnWorkerNode2)
		createGenericPod(f, multicastListenerPod, ovnWorkerNode2, f.Namespace.Name, tcpDumpCommand)

		// Wait for tcpdump on listener pod to be ready
		err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
			kubectlOut, err := framework.RunKubectl(f.Namespace.Name, "exec", multicastListenerPod, "--", "/bin/bash", "-c", "ls")
			if err != nil {
				framework.Failf("failed to retrieve multicast IGMP query: " + err.Error())
			}
			if !strings.Contains(kubectlOut, tcpdumpFileName) {
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			framework.Failf("failed to retrieve multicast IGMP query: " + err.Error())
		}

		// The multicast listener pod join multicast group (-B 224.1.1.1), UDP (-u), during (-t 30) seconds, report every (-i 5) seconds
		ginkgo.By("multicast listener pod join multicast group")
		framework.RunKubectl(f.Namespace.Name, "exec", multicastListenerPod, "--", "/bin/bash", "-c", fmt.Sprintf("iperf -s -B %s -u -t 30 -i 5", mcastGroup))

		ginkgo.By(fmt.Sprintf("verifying that the IGMP query has been received"))
		kubectlOut, err := framework.RunKubectl(f.Namespace.Name, "exec", multicastListenerPod, "--", "/bin/bash", "-c", fmt.Sprintf("cat %s | grep igmp", tcpdumpFileName))
		if err != nil {
			framework.Failf("failed to retrieve multicast IGMP query: " + err.Error())
		}
		framework.Logf("output:")
		framework.Logf(kubectlOut)
		if kubectlOut == "" {
			framework.Failf("failed to retrieve multicast IGMP query: igmp messages on the tcpdump logfile not found")
		}
	})

})

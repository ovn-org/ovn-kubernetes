package e2e

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

// pulled from https://github.com/kubernetes/kubernetes/blob/v1.26.2/test/e2e/framework/pod/wait.go#L468
// had to modify function due to restart policy on static pods being set to always, which caused function to fail
func waitForPodRunningInNamespaceTimeout(c clientset.Interface, podName, namespace string, timeout time.Duration) error {
	return e2epod.WaitForPodCondition(context.TODO(), c, namespace, podName, fmt.Sprintf("%s", v1.PodRunning), timeout, func(pod *v1.Pod) (bool, error) {
		switch pod.Status.Phase {
		case v1.PodRunning:
			ginkgo.By("Saw pod running")
			return true, nil
		default:
			return false, nil
		}
	})
}

func createStaticPod(f *framework.Framework, nodeName string, podYaml string) {
	//create file
	var podFile = "static-pod.yaml"
	if err := os.WriteFile(podFile, []byte(podYaml), 0644); err != nil {
		framework.Failf("Unable to write static-pod.yaml  to disk: %v", err)
	}
	defer func() {
		if err := os.Remove(podFile); err != nil {
			framework.Logf("Unable to remove the static-pod.yaml from disk: %v", err)
		}
	}()
	var dst = fmt.Sprintf("%s:/etc/kubernetes/manifests/%s", nodeName, podFile)
	cmd := []string{"docker", "cp", podFile, dst}
	framework.Logf("Running command %v", cmd)
	_, err := runCommand(cmd...)
	if err != nil {
		framework.Failf("failed to copy pod file to node %s", nodeName)
	}

}

func removeStaticPodFile(nodeName string, podFile string) {
	cmd := []string{"docker", "exec", nodeName, "bash", "-c", "rm /etc/kubernetes/manifests/static-pod.yaml"}
	framework.Logf("Running command %v", cmd)
	_, err := runCommand(cmd...)
	if err != nil {
		framework.Failf("failed to remove pod file from node %s", nodeName)
	}

}

// This test does the following
// Applies a static-pod.yaml file to a nodes /etc/kubernetes/manifest dir
// Expects the static pod to succeed
var _ = ginkgo.Describe("Creating a static pod on a node", func() {

	const (
		podFile      string = "static-pod.yaml"
		agnhostImage string = "registry.k8s.io/e2e-test-images/agnhost:2.26"
	)

	f := wrappedTestFramework("staticpods")

	var cs clientset.Interface

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
	})

	ginkgo.It("Should successfully create then remove a static pod", func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 3)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 1 {
			framework.Failf("Test requires 1 Ready node, but there are none")
		}
		nodeName := nodes.Items[0].Name
		podName := fmt.Sprintf("static-pod-%s", nodeName)

		ginkgo.By("copying a pod.yaml file into the /etc/kubernetes/manifests dir of a node")
		framework.Logf("creating %s on node %s", podName, nodeName)
		var staticPodYaml = fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: static-pod
  namespace: %s
spec: 
  containers: 
    - name: web 
      image: %s
      command: ["/bin/bash", "-c", "trap : TERM INT; sleep infinity & wait"]
`, f.Namespace.Name, agnhostImage)
		createStaticPod(f, nodeName, staticPodYaml)
		err = waitForPodRunningInNamespaceTimeout(f.ClientSet, podName, f.Namespace.Name, time.Second*30)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.By("Removing the pod file from the nodes /etc/kubernetes/manifests")
		framework.Logf("Removing %s from %s", podName, nodeName)
		removeStaticPodFile(nodeName, podFile)
		err = e2epod.WaitForPodNotFoundInNamespace(context.TODO(), f.ClientSet, podName, f.Namespace.Name, time.Second*30)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
})

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"

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

func createStaticPod(nodeName string, podYaml string) {
	// FIXME; remove need to use a container runtime because its not portable
	runCommand := func(cmd ...string) (string, error) {
		output, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to run %q: %s (%s)", strings.Join(cmd, " "), err, output)
		}
		return string(output), nil
	}
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
	// FIXME; remove need to use a container runtime because its not portable
	runCommand := func(cmd ...string) (string, error) {
		output, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to run %q: %s (%s)", strings.Join(cmd, " "), err, output)
		}
		return string(output), nil
	}

	cmd := []string{"docker", "exec", nodeName, "bash", "-c", fmt.Sprintf("rm /etc/kubernetes/manifests/%s", podFile)}
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
	const podFile string = "static-pod.yaml"

	f := wrappedTestFramework("staticpods")

	ginkgo.It("Should successfully create then remove a static pod", func() {
		if infraprovider.Get().Name() == "kube" {
			ginkgo.Skip("static Pod setup uses local Docker to modify Kind Nodes")
		}

		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 1 {
			framework.Failf("Test requires 1 Ready node, but there are none")
		}
		nodeName := nodes.Items[0].Name
		podName := fmt.Sprintf("static-pod-%s", nodeName)

		ginkgo.By("creating static pod file")

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
`, f.Namespace.Name, images.AgnHost())
		createStaticPod(nodeName, staticPodYaml)
		err = waitForPodRunningInNamespaceTimeout(f.ClientSet, podName, f.Namespace.Name, time.Second*60)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.By("Removing the pod file from the nodes /etc/kubernetes/manifests")
		framework.Logf("Removing %s from %s", podName, nodeName)
		removeStaticPodFile(nodeName, podFile)
		err = e2epod.WaitForPodNotFoundInNamespace(context.TODO(), f.ClientSet, podName, f.Namespace.Name, time.Second*60)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
})

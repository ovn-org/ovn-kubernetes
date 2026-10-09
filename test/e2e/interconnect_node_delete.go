// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	ovnkubeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

const kubernetesAPIRequestTimeout = 30 * time.Second

func getOVNKubeNodePod(cs clientset.Interface, nodeName string) (*corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubernetesAPIRequestTimeout)
	defer cancel()

	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	pods, err := cs.CoreV1().Pods(ovnNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no ovnkube-node pod on node %s", nodeName)
	}
	return &pods.Items[0], nil
}

func runOVNNBCTLOnNode(f *framework.Framework, cs clientset.Interface, nodeName string, args ...string) (string, error) {
	pod, err := getOVNKubeNodePod(cs, nodeName)
	if err != nil {
		return "", err
	}
	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	cmd := append([]string{"ovn-nbctl"}, args...)
	stdout, stderr, err := ExecCommandInContainerWithFullOutput(f, ovnNamespace, pod.Name, deploymentconfig.Get().NBDBContainerName(), cmd...)
	if err != nil {
		return stdout, fmt.Errorf("ovn-nbctl on node %s: %w, stderr: %s", nodeName, err, stderr)
	}
	return strings.TrimSpace(stdout), nil
}

// remoteTransitPortExists reports whether observerNode's local NB has a remote tstor-remoteNodeName port.
func remoteTransitPortExists(f *framework.Framework, cs clientset.Interface, observerNode, remoteNodeName string) (bool, error) {
	portName := types.TransitSwitchToRouterPrefix + remoteNodeName
	out, err := runOVNNBCTLOnNode(f, cs, observerNode,
		"--bare", "--columns=type", "find", "logical_switch_port", fmt.Sprintf("name=%s", portName))
	if err != nil {
		return false, err
	}
	return out == "remote", nil
}

func readyWorkerNodes(cs clientset.Interface, minWorkers int) ([]corev1.Node, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubernetesAPIRequestTimeout)
	defer cancel()

	nodes, err := e2enode.GetReadySchedulableNodes(ctx, cs)
	if err != nil {
		return nil, err
	}
	workers := make([]corev1.Node, 0, len(nodes.Items))
	for i := range nodes.Items {
		if !isControlPlaneNode(nodes.Items[i]) {
			workers = append(workers, nodes.Items[i])
		}
	}
	if len(workers) < minWorkers {
		return nil, fmt.Errorf("only %d ready schedulable workers, need %d", len(workers), minWorkers)
	}
	for i := range workers {
		if workers[i].Annotations[ovnkubeutil.OvnNodeID] == "" {
			return nil, fmt.Errorf("worker %s missing %s", workers[i].Name, ovnkubeutil.OvnNodeID)
		}
	}
	return workers, nil
}

func nodeReportsReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

const workerNodeRecoveryTimeout = 10 * time.Minute

// restoreDeletedWorkerNode restarts a worker after its Node object was deleted from the API.
// Kubelet does not reliably re-register while the node keeps running without a provider restart.
func restoreDeletedWorkerNode(f *framework.Framework, observerNode, victimName string) {
	cs := f.ClientSet
	framework.Logf("Restarting worker %s after API node delete", victimName)
	framework.ExpectNoError(infraprovider.Get().ShutdownNode(victimName),
		"failed to shut down worker %s during recovery", victimName)
	framework.ExpectNoError(infraprovider.Get().StartNode(victimName),
		"failed to start worker %s during recovery", victimName)
	waitForNodeReadyState(f, victimName, workerNodeRecoveryTimeout, true)
	err := wait.PollImmediate(5*time.Second, workerNodeRecoveryTimeout, func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), kubernetesAPIRequestTimeout)
		defer cancel()
		node, err := cs.CoreV1().Nodes().Get(ctx, victimName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if !nodeReportsReady(node) || node.Annotations[ovnkubeutil.OvnNodeID] == "" {
			return false, nil
		}
		observerHasVictim, err := remoteTransitPortExists(f, cs, observerNode, victimName)
		if err != nil || !observerHasVictim {
			return false, nil
		}
		victimHasObserver, err := remoteTransitPortExists(f, cs, victimName, observerNode)
		if err != nil || !victimHasObserver {
			return false, nil
		}
		return true, nil
	})
	framework.ExpectNoError(err, "worker %s not Ready with %s and remote tstor ports after restart within %v",
		victimName, ovnkubeutil.OvnNodeID, workerNodeRecoveryTimeout)
}

var _ = ginkgo.Describe("Interconnect remote transit port cleanup on Node delete", feature.Interconnect, ginkgo.Serial, func() {
	const waitICResources = 3 * time.Minute

	f := wrappedTestFramework("interconnect-node-delete")

	ginkgo.It("removes remote tstor port from another zone when a Node is deleted", func() {
		if !infraprovider.SupportsNodeRecovery() {
			e2eskipper.Skipf("infraprovider %q does not support worker recovery after Node delete (ShutdownNode/StartNode)",
				infraprovider.Get().Name())
		}

		cs := f.ClientSet
		workers, err := readyWorkerNodes(cs, 2)
		if err != nil {
			e2eskipper.Skipf("%v", err)
		}
		observerNode := workers[0].Name
		victimName := workers[1].Name

		gomega.Eventually(func() bool {
			exists, err := remoteTransitPortExists(f, cs, observerNode, victimName)
			if err != nil {
				framework.Logf("waiting for tstor-%s on %s: %v", victimName, observerNode, err)
				return false
			}
			return exists
		}, waitICResources, 2*time.Second).Should(gomega.BeTrue(),
			"expected tstor-%s on observer %s before delete", victimName, observerNode)

		victimDeleted := false
		defer func() {
			if victimDeleted {
				restoreDeletedWorkerNode(f, observerNode, victimName)
			}
		}()

		victimDeleted = true
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), kubernetesAPIRequestTimeout)
		defer deleteCancel()
		err = cs.CoreV1().Nodes().Delete(deleteCtx, victimName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			getCtx, getCancel := context.WithTimeout(context.Background(), kubernetesAPIRequestTimeout)
			defer getCancel()
			_, getErr := cs.CoreV1().Nodes().Get(getCtx, victimName, metav1.GetOptions{})
			if getErr == nil {
				framework.ExpectNoError(err, "failed to delete node %s", victimName)
			} else if !apierrors.IsNotFound(getErr) {
				framework.ExpectNoError(getErr, "failed to confirm node %s deletion after delete error", victimName)
			}
		}

		gomega.Eventually(func() bool {
			exists, err := remoteTransitPortExists(f, cs, observerNode, victimName)
			return err == nil && !exists
		}, waitICResources, 2*time.Second).Should(gomega.BeTrue(),
			"remote transit port tstor-%s should be removed from observer %s after node delete", victimName, observerNode)

		gomega.Consistently(func() bool {
			exists, err := remoteTransitPortExists(f, cs, observerNode, victimName)
			return err == nil && !exists
		}, 30*time.Second, 2*time.Second).Should(gomega.BeTrue())
	})
})

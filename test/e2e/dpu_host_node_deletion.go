// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"

	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
)

const (
	dpuHostNodeDeletionPoll    = 2 * time.Second
	dpuHostNodeDeletionTimeout = 10 * time.Minute
	// dpuOvnkubeControllerContainer is the container of the DPU-side ovnkube-node
	// pod that runs ovnkube on behalf of the host node.
	dpuOvnkubeControllerContainer = "ovnkube-controller"
	// dpuHostOvnkubeNodeContainer is the container of the DPU-host ovnkube-node
	// pod that runs on the host node itself.
	dpuHostOvnkubeNodeContainer = "ovnkube-node"
	// dpuOwnNodeDeletedLogMarker is logged by ovnkube-node when it exits because
	// the Node object it runs on behalf of was deleted.
	dpuOwnNodeDeletedLogMarker = "runs on behalf of was deleted"
)

// The DPU-side ovnkube-node does not run on the host node it manages, so when
// that Node object is deleted nothing terminates it. It has to notice the
// deletion itself and exit, so that once the node is registered again the
// restarted process initializes the new Node object (chassis id, gateway
// config, ...) instead of silently serving a node that no longer exists.
//
// The spec needs the dpu-simulator Kind layout: the DPU that serves host node
// <cluster>-host-<n> is the container <cluster>-dpu-<n>, reachable with the
// container runtime, since the test has no API access to the DPU cluster.
var _ = ginkgo.Describe("DPU host node deletion", feature.DPU, ginkgo.Serial, func() {
	f := wrappedTestFramework("dpu-host-node-deletion")

	var (
		hostNode    *corev1.Node
		dpuNodeName string
		savedLabels map[string]string
	)

	ginkgo.BeforeEach(func(ctx context.Context) {
		if infraprovider.Get().Name() != "kind" {
			e2eskipper.Skipf("DPU host node deletion is only supported on the kind provider, got %q", infraprovider.Get().Name())
		}
		hostNode, dpuNodeName, savedLabels = nil, "", nil
		nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: uplinkDPUHostNodeLabel})
		framework.ExpectNoError(err, "failed to list DPU host nodes")
		var candidate *corev1.Node
		for i := range nodes.Items {
			if isSchedulableDPUHostNode(&nodes.Items[i]) {
				candidate = &nodes.Items[i]
				break
			}
		}
		if candidate == nil {
			e2eskipper.Skipf("no schedulable DPU host node found")
		}
		dpuNode, err := dpuNodeNameForHostNode(candidate.Name)
		if err != nil {
			e2eskipper.Skipf("cannot derive the DPU node of host node %q: %v", candidate.Name, err)
		}
		// Only commit to the node once nothing can skip anymore: AfterEach runs
		// after a Skipf too and must not touch a node it has not saved.
		hostNode, dpuNodeName = candidate, dpuNode
		savedLabels = map[string]string{}
		for k, v := range hostNode.Labels {
			savedLabels[k] = v
		}
	})

	ginkgo.AfterEach(func(ctx context.Context) {
		if hostNode == nil || savedLabels == nil {
			return
		}
		// Leave the cluster usable even if the spec failed midway.
		restoreDPUHostNode(ctx, f.ClientSet, hostNode, savedLabels)
	})

	ginkgo.It("restarts the DPU-side ovnkube-node and re-initializes the node once it is registered again", func(ctx context.Context) {
		ginkgo.By(fmt.Sprintf("finding the %s container serving host node %s on DPU %s", dpuOvnkubeControllerContainer, hostNode.Name, dpuNodeName))
		oldContainerID, err := dpuRunningContainerID(dpuNodeName, dpuOvnkubeControllerContainer)
		framework.ExpectNoError(err)
		gomega.Expect(oldContainerID).NotTo(gomega.BeEmpty(), "no running %s container on DPU %s", dpuOvnkubeControllerContainer, dpuNodeName)

		ginkgo.By(fmt.Sprintf("creating an Uplink that selects host node %s", hostNode.Name))
		// UplinkStates are owned by their Uplink, not their Node, and node
		// controllers delete their own states only while their Node exists,
		// so cluster-manager must remove the deleted node's states itself.
		uplinkName := "updelnode" + framework.RandomSuffix()
		createUplink(ctx, f, infraprovider.Get().NewTestContext(), uplinkName, []corev1.Node{*hostNode},
			collectDPUHostUplinkInterfaces([]corev1.Node{*hostNode}), "")
		gomega.Eventually(ctx, func() error {
			if _, err := getUplinkState(ctx, f, uplinkName, hostNode.Name); err != nil {
				return fmt.Errorf("failed to get the UplinkState of Uplink %s on node %s: %w", uplinkName, hostNode.Name, err)
			}
			return nil
		}).WithTimeout(2*time.Minute).WithPolling(dpuHostNodeDeletionPoll).Should(gomega.Succeed(),
			"no UplinkState for Uplink %s on host node %s", uplinkName, hostNode.Name)

		ginkgo.By(fmt.Sprintf("deleting host node %s", hostNode.Name))
		framework.ExpectNoError(f.ClientSet.CoreV1().Nodes().Delete(ctx, hostNode.Name, metav1.DeleteOptions{}))

		ginkgo.By("waiting for the DPU-side ovnkube-node to exit because its node was deleted")
		gomega.Eventually(func() (string, error) {
			return dpuContainerState(dpuNodeName, oldContainerID)
		}).WithTimeout(2*time.Minute).WithPolling(dpuHostNodeDeletionPoll).Should(gomega.Equal("CONTAINER_EXITED"),
			"the %s container on DPU %s kept running after its host node was deleted", dpuOvnkubeControllerContainer, dpuNodeName)
		logs, err := ForContainer(dpuNodeName).Exec("crictl", "logs", oldContainerID)
		framework.ExpectNoError(err, "failed to read the logs of container %s on DPU %s", oldContainerID, dpuNodeName)
		gomega.Expect(logs).To(gomega.ContainSubstring(dpuOwnNodeDeletedLogMarker),
			"the %s container on DPU %s did not exit because of the node deletion", dpuOvnkubeControllerContainer, dpuNodeName)

		ginkgo.By(fmt.Sprintf("waiting for the UplinkStates of deleted host node %s to be removed", hostNode.Name))
		gomega.Eventually(func() (int, error) {
			return countUplinkStatesForNode(ctx, f, hostNode.Name)
		}).WithTimeout(2*time.Minute).WithPolling(dpuHostNodeDeletionPoll).Should(gomega.BeZero(),
			"UplinkStates of deleted host node %s were not removed", hostNode.Name)

		ginkgo.By(fmt.Sprintf("waiting for the pods of host node %s to be garbage collected", hostNode.Name))
		// A reprovisioned host comes back without its pods. Let the pod garbage
		// collector remove the pods bound to the deleted node before the node
		// registers again: the DPU-host pod releases its management VF, and the
		// device plugin the DPU-host DaemonSet depends on starts afresh.
		gomega.Eventually(func() (int, error) {
			pods, err := f.ClientSet.CoreV1().Pods("").List(ctx, metav1.ListOptions{
				FieldSelector: fields.OneTermEqualSelector("spec.nodeName", hostNode.Name).String(),
			})
			if err != nil {
				return 0, err
			}
			return len(pods.Items), nil
		}).WithTimeout(2*time.Minute).WithPolling(dpuHostNodeDeletionPoll).Should(gomega.BeZero(),
			"pods bound to the deleted node %s were not garbage collected", hostNode.Name)
		// The DPU-host ovnkube-node ran on the deleted node until now and could
		// have recreated its UplinkState after cluster-manager removed it.
		gomega.Expect(countUplinkStatesForNode(ctx, f, hostNode.Name)).To(gomega.BeZero(),
			"UplinkStates of deleted host node %s came back", hostNode.Name)

		ginkgo.By(fmt.Sprintf("registering host node %s again", hostNode.Name))
		restoreDPUHostNode(ctx, f.ClientSet, hostNode, savedLabels)

		ginkgo.By("waiting for the DPU-side ovnkube-node to initialize the new Node object")
		gomega.Eventually(func() error {
			node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, hostNode.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			for _, annotation := range []string{"k8s.ovn.org/node-chassis-id", "k8s.ovn.org/l3-gateway-config", "k8s.ovn.org/node-subnets"} {
				if _, ok := node.Annotations[annotation]; !ok {
					return fmt.Errorf("node %s has no %s annotation yet", node.Name, annotation)
				}
			}
			return nil
		}).WithTimeout(dpuHostNodeDeletionTimeout).WithPolling(dpuHostNodeDeletionPoll).Should(gomega.Succeed())
		gomega.Eventually(func() (string, error) {
			// A running container other than the one that exited: crash-loop gaps
			// return no container at all.
			return dpuRunningContainerID(dpuNodeName, dpuOvnkubeControllerContainer)
		}).WithTimeout(dpuHostNodeDeletionTimeout).WithPolling(dpuHostNodeDeletionPoll).Should(
			gomega.And(gomega.Not(gomega.BeEmpty()), gomega.Not(gomega.Equal(oldContainerID))),
			"no new %s container running on DPU %s", dpuOvnkubeControllerContainer, dpuNodeName)

		ginkgo.By("checking that pods scheduled on the host node get networking again")
		// On a DPU host every pod needs a DPU VF device: the DPU-host CNI rejects a
		// pod without a device ID, since the pod has no other way to the network.
		serverPod, err := createGenericPodWithLabel(f, "dpu-host-node-server", hostNode.Name, f.Namespace.Name,
			[]string{"/agnhost", "netexec", "--http-port=8000"}, nil, addDPUVFRequest)
		framework.ExpectNoError(err, "failed to run a pod on host node %s", hostNode.Name)
		framework.ExpectNoError(e2epod.WaitTimeoutForPodRunningInNamespace(ctx, f.ClientSet, serverPod.Name, f.Namespace.Name, dpuHostNodeDeletionTimeout),
			"pod on the recovered host node %s did not get networking", hostNode.Name)
		clientNode := ""
		otherNodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: uplinkDPUHostNodeLabel})
		framework.ExpectNoError(err)
		for i := range otherNodes.Items {
			if otherNodes.Items[i].Name != hostNode.Name && isSchedulableDPUHostNode(&otherNodes.Items[i]) {
				clientNode = otherNodes.Items[i].Name
				break
			}
		}
		if clientNode == "" {
			framework.Logf("no other DPU host node, checking connectivity from the recovered node itself")
			clientNode = hostNode.Name
		}
		clientPod, err := createGenericPodWithLabel(f, "dpu-host-node-client", clientNode, f.Namespace.Name,
			[]string{"/agnhost", "pause"}, nil, addDPUVFRequest)
		framework.ExpectNoError(err, "failed to run a pod on node %s", clientNode)
		framework.ExpectNoError(e2epod.WaitTimeoutForPodRunningInNamespace(ctx, f.ClientSet, clientPod.Name, f.Namespace.Name, dpuHostNodeDeletionTimeout),
			"pod on node %s did not get networking", clientNode)
		gomega.Eventually(func() error {
			return pokeAllPodIPs(f, clientPod.Name, serverPod)
		}).WithTimeout(2*time.Minute).WithPolling(dpuHostNodeDeletionPoll).Should(gomega.Succeed(),
			"pod on %s cannot reach the pod on the recovered host node %s", clientNode, hostNode.Name)
	})
})

// countUplinkStatesForNode returns how many UplinkStates, across all Uplinks,
// name the node.
func countUplinkStatesForNode(ctx context.Context, f *framework.Framework, nodeName string) (int, error) {
	states, err := f.DynamicClient.Resource(uplinkStateGVR).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
	})
	if err != nil {
		return 0, fmt.Errorf("failed to list the UplinkStates of node %s: %w", nodeName, err)
	}
	return len(states.Items), nil
}

// addDPUVFRequest gives the pod's container one DPU VF of the resource the DPU
// device plugin advertises (dpusim.io/vf on dpu-simulator, overridable with
// OVN_TEST_DPU_UPLINK_RESOURCE_NAME), unconditionally: this spec only runs on
// DPU hosts.
func addDPUVFRequest(pod *corev1.Pod) {
	resourceName := corev1.ResourceName(dpuUplinkResourceName())
	container := &pod.Spec.Containers[0]
	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}
	container.Resources.Requests[resourceName] = resource.MustParse("1")
	container.Resources.Limits[resourceName] = resource.MustParse("1")
}

// isSchedulableDPUHostNode reports whether node is Ready and not cordoned.
func isSchedulableDPUHostNode(node *corev1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// dpuRunningContainerID returns the ID of the running container named name on
// the Kind node, or an empty string if there is none.
func dpuRunningContainerID(dpuNodeName, name string) (string, error) {
	out, err := ForContainer(dpuNodeName).Exec("crictl", "ps", "-q", "--name", "^"+name+"$")
	if err != nil {
		return "", fmt.Errorf("failed to list the %s containers on node %s: %w, output: %s", name, dpuNodeName, err, out)
	}
	ids := strings.Fields(out)
	switch len(ids) {
	case 0:
		return "", nil
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("%d %s containers running on node %s, expected one", len(ids), name, dpuNodeName)
	}
}

// dpuContainerState returns the CRI state (CONTAINER_RUNNING, CONTAINER_EXITED,
// ...) of the container with the given ID on the DPU Kind node.
func dpuContainerState(dpuNodeName, containerID string) (string, error) {
	out, err := ForContainer(dpuNodeName).Exec("crictl", "inspect", "--output", "json", containerID)
	if err != nil {
		return "", fmt.Errorf("failed to inspect container %s on DPU %s: %w, output: %s", containerID, dpuNodeName, err, out)
	}
	var inspect struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &inspect); err != nil {
		return "", fmt.Errorf("failed to parse the inspect output of container %s on DPU %s: %w", containerID, dpuNodeName, err)
	}
	return inspect.Status.State, nil
}

// releaseDPUSimManagementVF gives the management port of the DPU host node its
// VF name back. The DPU-host ovnkube-node renames the VF it gets from the
// dpu-simulator device plugin to the management port name and does not rename
// it back when it is killed, while the device plugin only advertises VFs by
// their original names when it registers with the (restarted) kubelet. A
// reprovisioned host starts from clean devices; on the Kind node the rename
// has to be undone by hand or the DPU-host DaemonSet pod never schedules.
func releaseDPUSimManagementVF(hostNode *corev1.Node) {
	annotation, ok := hostNode.Annotations["k8s.ovn.org/node-mgmt-port"]
	if !ok {
		framework.Logf("Node %s has no management port annotation, nothing to release", hostNode.Name)
		return
	}
	var mgmtPorts map[string]struct {
		DeviceID string `json:"DeviceId"`
	}
	framework.ExpectNoError(json.Unmarshal([]byte(annotation), &mgmtPorts),
		"failed to parse the management port annotation of node %s: %s", hostNode.Name, annotation)
	deviceID := mgmtPorts["default"].DeviceID
	if deviceID == "" {
		framework.Logf("Node %s has no management port device, nothing to release", hostNode.Name)
		return
	}
	out, err := infraprovider.Get().ExecK8NodeCommand(hostNode.Name, []string{"sh", "-c", fmt.Sprintf(
		"if ip link show %[1]s >/dev/null 2>&1; then ip link set %[1]s down && ip link set %[1]s name %[2]s && ip link set %[2]s up; fi",
		ovntypes.K8sMgmtIntfName, deviceID)})
	framework.ExpectNoError(err, "failed to rename %s back to %s on node %s: %s", ovntypes.K8sMgmtIntfName, deviceID, hostNode.Name, out)
}

// setKubeletNodeLabels makes the kubelet of the Kind node register its node
// with the OVN-Kubernetes labels from labels (the platform-owned ones, outside
// the kubernetes.io namespaces the node restriction admission reserves), by
// editing --node-labels in the kubeadm flags file kubelet is started with.
func setKubeletNodeLabels(nodeName string, labels map[string]string) {
	var wanted []string
	for k, v := range labels {
		if strings.HasPrefix(k, "k8s.ovn.org/") {
			wanted = append(wanted, k+"="+v)
		}
	}
	if len(wanted) == 0 {
		return
	}
	sort.Strings(wanted)
	// Labels carry no shell metacharacters (validated label syntax), and the
	// flag is never the last one in the file, so a plain [^ ]* match is enough.
	labelList := strings.Join(wanted, ",")
	script := fmt.Sprintf(`set -e
f=/var/lib/kubelet/kubeadm-flags.env
if grep -q -- '--node-labels=' "$f"; then
  sed -i -E 's#--node-labels=[^ ]*#--node-labels=%s#' "$f"
else
  sed -i -E 's#^KUBELET_KUBEADM_ARGS=.#&--node-labels=%s #' "$f"
fi
grep -- '--node-labels=' "$f"`, labelList, labelList)
	out, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"sh", "-c", script})
	framework.ExpectNoError(err, "failed to set kubelet node labels on %s: %s", nodeName, out)
	framework.Logf("kubelet on %s will register with: %s", nodeName, strings.TrimSpace(out))
}

// restoreDPUHostNode makes sure the host node is registered, carries the labels
// it had before the test and is Ready. A kubelet only registers its node at
// startup, so a deleted node comes back with a kubelet restart, once the pods
// bound to the deleted node are gone and its management VF is released; the
// labels the platform put on the node (e.g. the DPU host marker the DPU-host
// DaemonSet selects on) are not owned by the kubelet, so they are applied again
// here, and the full-mode ovnkube-node pod that lands on the node in the
// unlabeled window is waited out.
func restoreDPUHostNode(ctx context.Context, cs kubernetes.Interface, hostNode *corev1.Node, labels map[string]string) {
	nodeName := hostNode.Name
	_, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		gomega.Eventually(func() (int, error) {
			pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{
				FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
			})
			if err != nil {
				return 0, err
			}
			return len(pods.Items), nil
		}).WithTimeout(2*time.Minute).WithPolling(dpuHostNodeDeletionPoll).Should(gomega.BeZero(),
			"pods bound to the deleted node %s were not garbage collected", nodeName)
		// The pod garbage collector removes the pod object before the kubelet
		// has killed the container, and the kubelet restart below cancels an
		// in-flight container stop. A still running DPU-host ovnkube-node
		// recreates the management port from the VF as soon as it is renamed
		// back, so wait for the process to be gone before releasing the VF.
		gomega.Eventually(func() (string, error) {
			return dpuRunningContainerID(nodeName, dpuHostOvnkubeNodeContainer)
		}).WithTimeout(2*time.Minute).WithPolling(dpuHostNodeDeletionPoll).Should(gomega.BeEmpty(),
			"the %s container kept running on node %s after its pod was deleted", dpuHostOvnkubeNodeContainer, nodeName)
		releaseDPUSimManagementVF(hostNode)
		// Register with the DPU host label already set: a re-registered node
		// without it matches the full-mode ovnkube-node DaemonSet until the label
		// is restored, and that pod rewires the host (bridges its NIC) in seconds.
		setKubeletNodeLabels(nodeName, labels)
		framework.Logf("Restarting kubelet on %s so that it registers its node again", nodeName)
		_, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"systemctl", "restart", "kubelet"})
		framework.ExpectNoError(err, "failed to restart kubelet on %s", nodeName)
		gomega.Eventually(func() error {
			_, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			return err
		}).WithTimeout(2*time.Minute).WithPolling(dpuHostNodeDeletionPoll).Should(gomega.Succeed(), "node %s did not register again", nodeName)
	} else {
		framework.ExpectNoError(err, "failed to get node %s", nodeName)
	}
	patch := map[string]interface{}{"metadata": map[string]interface{}{"labels": labels}}
	patchBytes, err := json.Marshal(patch)
	framework.ExpectNoError(err)
	_, err = cs.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	framework.ExpectNoError(err, "failed to restore the labels of node %s", nodeName)
	// The DaemonSet controller removes the full-mode ovnkube-node pod that got
	// bound to the node while it carried no DPU host label.
	gomega.Eventually(func() (int, error) {
		pods, err := cs.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(ctx, metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
			FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
		})
		if err != nil {
			return 0, err
		}
		return len(pods.Items), nil
	}).WithTimeout(2*time.Minute).WithPolling(dpuHostNodeDeletionPoll).Should(gomega.BeZero(),
		"a full-mode ovnkube-node pod stayed bound to DPU host node %s", nodeName)
	gomega.Expect(e2enode.WaitForNodeToBeReady(ctx, cs, nodeName, dpuHostNodeDeletionTimeout)).To(gomega.BeTrue(), "node %s did not become Ready", nodeName)
}

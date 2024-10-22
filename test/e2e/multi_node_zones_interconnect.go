package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

func changeNodeZone(node *v1.Node, zone string, cs clientset.Interface) error {
	node.Labels[ovnNodeZoneNameAnnotation] = zone

	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"labels": node.Labels,
		},
	}

	patchData, err = json.Marshal(&patch)
	framework.ExpectNoError(err)

	_, err = cs.CoreV1().Nodes().Patch(context.TODO(), node.Name, types.MergePatchType, patchData, metav1.PatchOptions{})
	framework.ExpectNoError(err)

	// Restart the ovnkube-node on this node
	err = restartOVNKubeNodePod(cs, ovnNamespace, node.Name)
	framework.ExpectNoError(err)

	// Verify that the node is moved to the expected zone
	err = wait.PollImmediate(2*time.Second, 5*time.Minute, func() (bool, error) {
		// Nodes().Get(context.TODO(), node1.Name, metav1.GetOptions{})
		n, err := cs.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("could not get the node %s: %w", node.Name, err)
		}

		z, err := getNodeZone(n)
		if err != nil {
			return false, fmt.Errorf("could not get the node %s zone: %w", node.Name, err)
		}
		if z == zone {
			return true, nil
		}
		return false, fmt.Errorf("expected node %s to be %s, but found %s", node.Name, zone, z)
	})
	framework.ExpectNoError(err)

	return nil
}

func checkPodsInterconnectivity(clientPod, serverPod *v1.Pod, namespace string, cs clientset.Interface) error {
	gomega.Eventually(func() error {
		updatedPod, err := cs.CoreV1().Pods(namespace).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}

		clientPodConfig := podConfiguration{
			name:      clientPod.Name,
			namespace: namespace,
		}
		if updatedPod.Status.Phase == v1.PodRunning {
			return connectToServer(clientPodConfig, updatedPod.Status.PodIP, 8000)
		}

		return fmt.Errorf("pod not running. /me is sad")
	}, 2*time.Minute, 6*time.Second).Should(gomega.Succeed())

	return nil
}

var _ = ginkgo.Describe("Multi node zones interconnect", func() {

	const (
		serverPodNodeName = "ovn-control-plane"
		serverPodName     = "server-pod"
		clientPodNodeName = "ovn-worker3"
		clientPodName     = "client-pod"
	)
	fr := wrappedTestFramework("multi-node-zones")

	var (
		cs clientset.Interface

		serverPodNode *v1.Node
		clientPodNode *v1.Node

		serverPodNodeZone string
		clientPodNodeZone string
	)

	ginkgo.BeforeEach(func() {
		cs = fr.ClientSet
		//ns = fr.Namespace.Name

		nodes, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 3 {
			e2eskipper.Skipf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		serverPodNode, err = cs.CoreV1().Nodes().Get(context.TODO(), serverPodNodeName, metav1.GetOptions{})
		if err != nil {
			e2eskipper.Skipf(
				"Test requires node with the name %s", serverPodName,
			)
		}
		clientPodNode, err = cs.CoreV1().Nodes().Get(context.TODO(), clientPodNodeName, metav1.GetOptions{})
		if err != nil {
			e2eskipper.Skipf(
				"Test requires node with the name %s", clientPodName,
			)
		}

		serverPodNodeZone, err = getNodeZone(serverPodNode)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		clientPodNodeZone, err = getNodeZone(clientPodNode)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		if serverPodNodeZone == clientPodNodeZone {
			e2eskipper.Skipf(
				"Test requires nodes %s and %s are in different zones", serverPodNodeName, clientPodNodeName,
			)
		}
	})

	ginkgo.It("Pod interconnectivity", func() {
		// Create a server pod on zone - zone-1
		cmd := httpServerContainerCmd(8000)
		serverPod := e2epod.NewAgnhostPod(fr.Namespace.Name, serverPodName, nil, nil, nil, cmd...)
		serverPod.Spec.NodeName = serverPodNodeName
		e2epod.NewPodClient(fr).CreateSync(context.TODO(), serverPod)

		// Create a client pod on zone - zone-2
		cmd = []string{}
		clientPod := e2epod.NewAgnhostPod(fr.Namespace.Name, clientPodName, nil, nil, nil, cmd...)
		clientPod.Spec.NodeName = clientPodNodeName
		e2epod.NewPodClient(fr).CreateSync(context.TODO(), clientPod)

		ginkgo.By("asserting the *client* pod can contact the server pod exposed endpoint")
		checkPodsInterconnectivity(clientPod, serverPod, fr.Namespace.Name, cs)

		// Change the zone of client-pod node to that of server-pod node
		s := fmt.Sprintf("Changing the client-pod node %s zone from %s to %s", clientPodNodeName, clientPodNodeZone, serverPodNodeZone)
		ginkgo.By(s)
		changeNodeZone(clientPodNode, serverPodNodeZone, cs)

		ginkgo.By("Checking that the client-pod can connect to the server pod when they are in same zone")
		checkPodsInterconnectivity(clientPod, serverPod, fr.Namespace.Name, cs)

		// Change back the zone of client-pod node
		s = fmt.Sprintf("Changing back the client-pod node %s zone from %s to %s", clientPodNodeName, serverPodNodeZone, clientPodNodeZone)
		ginkgo.By(s)
		changeNodeZone(clientPodNode, clientPodNodeZone, cs)

		ginkgo.By("Checking again that the client-pod can connect to the server-pod when they are in different zone")
		checkPodsInterconnectivity(clientPod, serverPod, fr.Namespace.Name, cs)
	})
})

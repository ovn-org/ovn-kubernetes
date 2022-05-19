package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

const (
	mcastSource  = "pod-client"
	mcastServer1 = "pod-server1"
	mcastServer2 = "pod-server2"
)

var _ = ginkgo.Describe("Multicast", func() {

	fr := wrappedTestFramework("multicast")

	type nodeInfo struct {
		name   string
		nodeIP string
	}

	var (
		cs                             clientset.Interface
		ns                             string
		clientNodeInfo, serverNodeInfo nodeInfo
	)

	ginkgo.BeforeEach(func() {
		cs = fr.ClientSet
		ns = fr.Namespace.Name

		nodes, err := e2enode.GetBoundedReadySchedulableNodes(cs, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			e2eskipper.Skipf(
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

		// annotate namespace to enable multicast
		namespace, err := fr.ClientSet.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
		framework.ExpectNoError(err, "Error getting Namespace %v: %v", fr.Namespace.Name, err)
		if namespace.ObjectMeta.Annotations == nil {
			namespace.ObjectMeta.Annotations = map[string]string{}
		}
		namespace.ObjectMeta.Annotations["k8s.ovn.org/multicast-enabled"] = "true"
		_, err = fr.ClientSet.CoreV1().Namespaces().Update(context.Background(), namespace, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "Error updating Namespace %v: %v", ns, err)
	})

	ginkgo.It("should be able to send multicast UDP traffic between nodes", func() {
		mcastGroup := "224.3.3.3"
		mcastGroupBad := "224.5.5.5"
		if IsIPv6Cluster(cs) {
			mcastGroup = "ff3e::4321:1234"
			mcastGroupBad = "ff3e::4321:1235"
		}

		// Start the multicast source (iperf client is the sender in multicast)
		ginkgo.By("creating a pod as a multicast source in node " + clientNodeInfo.name)
		// multicast group (-c 224.3.3.3), UDP (-u), TTL (-T 2), during (-t 3000) seconds, report every (-i 5) seconds
		iperf := fmt.Sprintf("iperf -c %s -u -T 2 -t 3000 -i 5", mcastGroup)
		if IsIPv6Cluster(cs) {
			iperf = iperf + " -V"
		}
		cmd := []string{"/bin/sh", "-c", iperf}
		clientPod := newAgnhostPod(fr.Namespace.Name, mcastSource, cmd...)
		clientPod.Spec.NodeName = clientNodeInfo.name
		fr.PodClient().CreateSync(clientPod)

		// Start a multicast listener on the same groups and verify it received the traffic (iperf server is the multicast listener)
		// join multicast group (-B 224.3.3.3), UDP (-u), during (-t 30) seconds, report every (-i 1) seconds
		ginkgo.By("creating first multicast listener pod in node " + serverNodeInfo.name)
		iperf = fmt.Sprintf("iperf -s -B %s -u -t 180 -i 5", mcastGroup)
		if IsIPv6Cluster(cs) {
			iperf = iperf + " -V"
		}
		cmd = []string{"/bin/sh", "-c", iperf}
		mcastServerPod1 := newAgnhostPod(fr.Namespace.Name, mcastServer1, cmd...)
		mcastServerPod1.Spec.NodeName = serverNodeInfo.name
		fr.PodClient().CreateSync(mcastServerPod1)

		// Start a multicast listener on on other group and verify it does not receive the traffic (iperf server is the multicast listener)
		// join multicast group (-B 224.4.4.4), UDP (-u), during (-t 30) seconds, report every (-i 1) seconds
		ginkgo.By("creating second multicast listener pod in node " + serverNodeInfo.name)
		iperf = fmt.Sprintf("iperf -s -B %s -u -t 180 -i 5", mcastGroupBad)
		if IsIPv6Cluster(cs) {
			iperf = iperf + " -V"
		}
		cmd = []string{"/bin/sh", "-c", iperf}
		mcastServerPod2 := newAgnhostPod(fr.Namespace.Name, mcastServer2, cmd...)
		mcastServerPod2.Spec.NodeName = serverNodeInfo.name
		fr.PodClient().CreateSync(mcastServerPod2)

		ginkgo.By("checking if pod server1 received multicast traffic")
		gomega.Eventually(func() (string, error) {
			return e2epod.GetPodLogs(cs, ns, mcastServer1, mcastServer1)
		},
			30*time.Second, 1*time.Second).Should(gomega.ContainSubstring("connected"))

		ginkgo.By("checking if pod server2 does not received multicast traffic")
		gomega.Eventually(func() (string, error) {
			return e2epod.GetPodLogs(cs, ns, mcastServer2, mcastServer2)
		},
			30*time.Second, 1*time.Second).ShouldNot(gomega.ContainSubstring("connected"))
	})

})

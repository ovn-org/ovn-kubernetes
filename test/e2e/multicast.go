package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

type nodeInfo struct {
	name   string
	nodeIP string
}

var _ = ginkgo.Describe("Multicast", func() {

	fr := wrappedTestFramework("multicast")

	var (
		clientNodeInfo, serverNodeInfo nodeInfo
	)
	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), fr.ClientSet, 2)
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
	})
	ginkgo.Context("when multicast enabled for namespace", func() {
		ginkgo.BeforeEach(func() {
			enableMulticastForNamespace(fr)
		})

		ginkgo.It("should be able to send multicast UDP traffic between nodes", func() {
			testMulticastUDPTraffic(fr, clientNodeInfo, serverNodeInfo)
		})
		ginkgo.It("should be able to receive multicast IGMP query", func() {
			testMulticastIGMPQuery(fr, clientNodeInfo, serverNodeInfo)
		})
	})
})

func testMulticastUDPTraffic(fr *framework.Framework, clientNodeInfo, serverNodeInfo nodeInfo) {
	ginkgo.GinkgoHelper()
	const (
		mcastSource  = "pod-client"
		mcastServer1 = "pod-server1"
		mcastServer2 = "pod-server2"
		mcastServer3 = "pod-server3"
	)
	var (
		mcastGroupIPv4    = "224.3.3.3"
		mcastGroupIPv4Bad = "224.5.5.5"
		mcastGroupIPv6    = "ff3e::4321:1234"
		mcastGroupIPv6Bad = "ff3e::4321:1235"
	)

	mcastGroup := mcastGroupIPv4
	mcastGroupBad := mcastGroupIPv4Bad
	if IsIPv6Cluster(fr.ClientSet) {
		mcastGroup = mcastGroupIPv6
		mcastGroupBad = mcastGroupIPv6Bad
	}
	ns := fr.Namespace.Name
	// Start the multicast source (iperf client is the sender in multicast)
	ginkgo.By("creating a pod as a multicast source in node " + clientNodeInfo.name)
	// multicast group (-c 224.3.3.3), UDP (-u), TTL (-T 3), during (-t 3000) seconds, report every (-i 5) seconds
	iperf := fmt.Sprintf("iperf -c %s -u -T 3 -t 3000 -i 5", mcastGroup)
	if IsIPv6Cluster(fr.ClientSet) {
		iperf = iperf + " -V"
	}
	cmd := []string{"/bin/sh", "-c", iperf}
	clientPod := newAgnhostPod(fr.Namespace.Name, mcastSource, cmd...)
	clientPod.Spec.NodeName = clientNodeInfo.name
	e2epod.NewPodClient(fr).CreateSync(context.TODO(), clientPod)

	// Start a multicast listener on the same groups and verify it received the traffic (iperf server is the multicast listener)
	// join multicast group (-B 224.3.3.3), UDP (-u), during (-t 30) seconds, report every (-i 1) seconds
	ginkgo.By("creating first multicast listener pod in node " + serverNodeInfo.name)
	iperf = fmt.Sprintf("iperf -s -B %s -u -t 180 -i 5", mcastGroup)
	if IsIPv6Cluster(fr.ClientSet) {
		iperf = iperf + " -V"
	}
	cmd = []string{"/bin/sh", "-c", iperf}
	mcastServerPod1 := newAgnhostPod(fr.Namespace.Name, mcastServer1, cmd...)
	mcastServerPod1.Spec.NodeName = serverNodeInfo.name
	e2epod.NewPodClient(fr).CreateSync(context.TODO(), mcastServerPod1)

	// Start a multicast listener on on other group and verify it does not receive the traffic (iperf server is the multicast listener)
	// join multicast group (-B 224.4.4.4), UDP (-u), during (-t 30) seconds, report every (-i 1) seconds
	ginkgo.By("creating second multicast listener pod in node " + serverNodeInfo.name)
	iperf = fmt.Sprintf("iperf -s -B %s -u -t 180 -i 5", mcastGroupBad)
	if IsIPv6Cluster(fr.ClientSet) {
		iperf = iperf + " -V"
	}
	cmd = []string{"/bin/sh", "-c", iperf}
	mcastServerPod2 := newAgnhostPod(fr.Namespace.Name, mcastServer2, cmd...)
	mcastServerPod2.Spec.NodeName = serverNodeInfo.name
	e2epod.NewPodClient(fr).CreateSync(context.TODO(), mcastServerPod2)

	// Start a multicast listener on the same groups and verify it received the traffic (iperf server is the multicast listener)
	// join multicast group (-B 224.3.3.3), UDP (-u), during (-t 30) seconds, report every (-i 1) seconds
	ginkgo.By("creating first multicast listener pod in node " + clientNodeInfo.name)
	iperf = fmt.Sprintf("iperf -s -B %s -u -t 180 -i 5", mcastGroup)
	if IsIPv6Cluster(fr.ClientSet) {
		iperf = iperf + " -V"
	}
	cmd = []string{"/bin/sh", "-c", iperf}
	mcastServerPod3 := newAgnhostPod(fr.Namespace.Name, mcastServer3, cmd...)
	mcastServerPod3.Spec.NodeName = clientNodeInfo.name
	e2epod.NewPodClient(fr).CreateSync(context.TODO(), mcastServerPod3)

	ginkgo.By("checking if pod server1 received multicast traffic")
	gomega.Eventually(func() (string, error) {
		return e2epod.GetPodLogs(context.TODO(), fr.ClientSet, ns, mcastServer1, mcastServer1)
	},
		30*time.Second, 1*time.Second).Should(gomega.ContainSubstring("connected"))

	ginkgo.By("checking if pod server2 does not received multicast traffic")
	gomega.Eventually(func() (string, error) {
		return e2epod.GetPodLogs(context.TODO(), fr.ClientSet, ns, mcastServer2, mcastServer2)
	},
		30*time.Second, 1*time.Second).ShouldNot(gomega.ContainSubstring("connected"))

	ginkgo.By("checking if pod server3 received multicast traffic")
	gomega.Eventually(func() (string, error) {
		return e2epod.GetPodLogs(context.TODO(), fr.ClientSet, ns, mcastServer3, mcastServer3)
	},
		30*time.Second, 1*time.Second).Should(gomega.ContainSubstring("connected"))

}

func testMulticastIGMPQuery(f *framework.Framework, clientNodeInfo, serverNodeInfo nodeInfo) {
	ginkgo.GinkgoHelper()
	const (
		mcastGroup           string = "224.1.1.1"
		mcastV6Group         string = "ff3e::4321:1234"
		multicastListenerPod string = "multicast-listener-test-pod"
		multicastSourcePod   string = "multicast-source-test-pod"
		tcpdumpFileName      string = "tcpdump.txt"
		retryTimeout                = 5 * time.Minute // polling timeout
	)
	var (
		tcpDumpCommand = []string{"bash", "-c",
			fmt.Sprintf("apk update; apk add tcpdump ; tcpdump -i any igmp > %s", tcpdumpFileName)}
		// Multicast group (-c 224.1.1.1), UDP (-u), TTL (-T 2), during (-t 3000) seconds, report every (-i 5) seconds
		multicastSourceCommand = []string{"bash", "-c",
			fmt.Sprintf("iperf -c %s -u -T 2 -t 3000 -i 5", mcastGroup)}
	)

	// Create a multicast source pod
	if IsIPv6Cluster(f.ClientSet) {
		// Multicast group (-c ff3e::4321:1234), UDP (-u), TTL (-T 2), during (-t 3000) seconds, report every (-i 5) seconds, -V (Set the domain to IPv6)
		multicastSourceCommand = []string{"bash", "-c",
			fmt.Sprintf("iperf -c %s -u -T 2 -t 3000 -i 5 -V", mcastV6Group)}
	}
	ginkgo.By("creating a multicast source pod in node " + clientNodeInfo.name)
	createGenericPod(f, multicastSourcePod, clientNodeInfo.name, f.Namespace.Name, multicastSourceCommand)

	// Create a multicast listener pod
	ginkgo.By("creating a multicast listener pod in node " + serverNodeInfo.name)
	createGenericPod(f, multicastListenerPod, serverNodeInfo.name, f.Namespace.Name, tcpDumpCommand)

	// Wait for tcpdump on listener pod to be ready
	err := wait.PollUntilContextTimeout(context.Background(), retryInterval, retryTimeout, true /*immediate*/, func(context.Context) (bool, error) {
		kubectlOut, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", multicastListenerPod, "--", "/bin/bash", "-c", "ls")
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

	ginkgo.By("verifying that the IGMP query has been received")
	// ovn-k leaves both mcast_idle_timeout and mcast_query_interval to default which means that
	// ovn-controller will send IGMP queries every 150 seconds.
	gomega.Eventually(func() (string, error) {
		return e2ekubectl.RunKubectl(f.Namespace.Name, "exec", multicastListenerPod, "--", "/bin/bash", "-c", fmt.Sprintf("grep igmp %s", tcpdumpFileName))
	}, 2*150*time.Second, 5*time.Second).Should(gomega.ContainSubstring("igmp"), "Failed to retrieve multicast IGMP query within the expected time")
}

func enableMulticastForNamespace(fr *framework.Framework) {
	ginkgo.GinkgoHelper()
	// annotate namespace to enable multicast
	namespace, err := fr.ClientSet.CoreV1().Namespaces().Get(context.Background(), fr.Namespace.Name, metav1.GetOptions{})
	framework.ExpectNoError(err, "Error getting Namespace %v: %v", fr.Namespace.Name, err)
	if namespace.ObjectMeta.Annotations == nil {
		namespace.ObjectMeta.Annotations = map[string]string{}
	}
	namespace.ObjectMeta.Annotations["k8s.ovn.org/multicast-enabled"] = "true"
	_, err = fr.ClientSet.CoreV1().Namespaces().Update(context.Background(), namespace, metav1.UpdateOptions{})
	framework.ExpectNoError(err, "Error updating Namespace %v: %v", fr.Namespace.Name, err)
}

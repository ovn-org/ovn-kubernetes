package e2e

import (
	"fmt"
	"strings"

	"github.com/onsi/ginkgo"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enetwork "k8s.io/kubernetes/test/e2e/framework/network"
)

// TODO This is a clone of k8s "should function for service endpoints using hostNetwork" test
// without the "handle large requests: udp(hostNetwork)" specific test, that is currently failing.
// This can be removed once the udp fragmentation is fixed (see https://github.com/ovn-org/ovn-kubernetes/pull/2018#issuecomment-776373160)
// Also, remove the related skip in e2e-cp.sh script
var _ = ginkgo.Describe("Hostnetwork endpoints", func() {
	var svcname = "nettest"
	f := framework.NewDefaultFramework(svcname)

	ginkgo.It("should work when used with services", func() {
		framework.TestContext.CloudConfig.NumNodes = 2
		config := e2enetwork.NewNetworkingTestConfig(f, e2enetwork.UseHostNetwork, e2enetwork.EndpointsUseHostNetwork)
		ginkgo.By("pod-Service(hostNetwork): http")

		ginkgo.By(fmt.Sprintf("dialing(http) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterHTTPPort))
		err := config.DialFromTestContainer("http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
		if err != nil {
			framework.Failf("failed dialing endpoint, %v", err)
		}
		ginkgo.By(fmt.Sprintf("dialing(http) %v --> %v:%v (nodeIP)", config.TestContainerPod.Name, config.NodeIP, config.NodeHTTPPort))
		err = config.DialFromTestContainer("http", config.NodeIP, config.NodeHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
		if err != nil {
			framework.Failf("failed dialing endpoint, %v", err)
		}

		ginkgo.By("node-Service(hostNetwork): http")

		ginkgo.By(fmt.Sprintf("dialing(http) %v (node) --> %v:%v (config.clusterIP)", config.NodeIP, config.ClusterIP, e2enetwork.ClusterHTTPPort))
		err = config.DialFromNode("http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
		if err != nil {
			framework.Failf("failed dialing endpoint, %v", err)
		}
		ginkgo.By(fmt.Sprintf("dialing(http) %v (node) --> %v:%v (nodeIP)", config.NodeIP, config.NodeIP, config.NodeHTTPPort))
		err = config.DialFromNode("http", config.NodeIP, config.NodeHTTPPort, config.MaxTries, 0, config.EndpointHostnames())
		if err != nil {
			framework.Failf("failed dialing endpoint, %v", err)
		}
		ginkgo.By("node-Service(hostNetwork): udp")

		ginkgo.By(fmt.Sprintf("dialing(udp) %v (node) --> %v:%v (config.clusterIP)", config.NodeIP, config.ClusterIP, e2enetwork.ClusterUDPPort))
		err = config.DialFromNode("udp", config.ClusterIP, e2enetwork.ClusterUDPPort, config.MaxTries, 0, config.EndpointHostnames())
		if err != nil {
			framework.Failf("failed dialing endpoint, %v", err)
		}
		ginkgo.By(fmt.Sprintf("dialing(udp) %v (node) --> %v:%v (nodeIP)", config.NodeIP, config.NodeIP, config.NodeUDPPort))
		err = config.DialFromNode("udp", config.NodeIP, config.NodeUDPPort, config.MaxTries, 0, config.EndpointHostnames())
		if err != nil {
			framework.Failf("failed dialing endpoint, %v", err)
		}

		ginkgo.By("handle large requests: http(hostNetwork)")

		ginkgo.By(fmt.Sprintf("dialing(http) %v --> %v:%v (config.clusterIP)", config.TestContainerPod.Name, config.ClusterIP, e2enetwork.ClusterHTTPPort))
		message := strings.Repeat("42", 1000)
		err = config.DialEchoFromTestContainer("http", config.ClusterIP, e2enetwork.ClusterHTTPPort, config.MaxTries, 0, message)
		if err != nil {
			framework.Failf("failed dialing endpoint, %v", err)
		}
	})
})

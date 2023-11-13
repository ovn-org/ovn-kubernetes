package e2e

import (
	"context"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

var _ = ginkgo.Describe("Check whether gateway-mtu-support annotation on node is set based on disable-pkt-mtu-check value", func() {
	var nodes *v1.NodeList
	f := wrappedTestFramework("gateway-mtu-support")

	ginkgo.BeforeEach(func() {
		var err error
		ginkgo.By("Get all nodes")
		nodes, err = e2enode.GetReadySchedulableNodes(context.TODO(), f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
	ginkgo.When("DisablePacketMTUCheck is either not set or set to false", func() {
		ginkgo.It("Verify whether gateway-mtu-support annotation is not set on nodes when DisablePacketMTUCheck is either not set or set to false", func() {
			if !isDisablePacketMTUCheckEnabled() {
				for _, node := range nodes.Items {
					supported := getGatewayMTUSupport(&node)
					gomega.Expect(supported).To(gomega.Equal(true))

				}
			} else {
				ginkgo.Skip("DisablePacketMTUCheck is set to true")
			}
		})
	})
})

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
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

var _ = ginkgo.Describe("Check whether gateway-mtu-support annotation on node is set based on disable-pkt-mtu-check value", feature.DisablePacketMTUCheck, func() {
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

	ginkgo.When("the gateway-mtu-support annotation changes on a node", func() {
		// Serial: this mutates a shared node annotation and the cluster router port
		// derived from it, which other specs observe.
		ginkgo.It("Verify options:gateway_mtu is removed from and restored on the cluster router port", ginkgo.Serial, func() {
			node := nodes.Items[0]
			ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()

			ginkgo.By("Find the ovnkube-node pod holding the northbound database for " + node.Name)
			ovnkubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: "app=ovnkube-node",
				FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(ovnkubeNodePods.Items).NotTo(gomega.BeEmpty(), "should find ovnkube-node pod on node %s", node.Name)
			dbPod := ovnkubeNodePods.Items[0]

			lrpName := ovntypes.RouterToSwitchPrefix + node.Name
			findLRPColumn := func(column string) (string, error) {
				stdout, stderr, err := ExecCommandInContainerWithFullOutput(f, ovnNamespace, dbPod.Name,
					deploymentconfig.Get().NBDBContainerName(),
					"ovn-nbctl", "--bare", fmt.Sprintf("--columns=%s", column), "find", "logical_router_port",
					fmt.Sprintf("name=%s", lrpName))
				if err != nil {
					return "", fmt.Errorf("failed to read %s of %s: %w, stderr: %s", column, lrpName, err, stderr)
				}
				return strings.TrimSpace(stdout), nil
			}
			// Guard against a renamed port silently turning the assertions below into no-ops.
			ginkgo.By("Verify the cluster router port " + lrpName + " exists")
			name, err := findLRPColumn("name")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(name).To(gomega.Equal(lrpName))

			gatewayMTUSet := func() (bool, error) {
				options, err := findLRPColumn("options")
				if err != nil {
					return false, err
				}
				return strings.Contains(options, "gateway_mtu="), nil
			}

			patchNode := func(patch string) {
				_, err := f.ClientSet.CoreV1().Nodes().Patch(context.TODO(), node.Name,
					types.MergePatchType, []byte(patch), metav1.PatchOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
			setGatewayMTUSupport := func(value string) {
				patchNode(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, ovnGatewayMTUSupport, value))
			}
			// A null value deletes the annotation, and succeeds whether or not it is present.
			removeGatewayMTUSupport := func() {
				patchNode(fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, ovnGatewayMTUSupport))
			}

			// Put back whatever ovnkube-node decided for this node when it started.
			originalValue, hadAnnotation := node.Annotations[ovnGatewayMTUSupport]
			ginkgo.DeferCleanup(func() {
				if hadAnnotation {
					setGatewayMTUSupport(originalValue)
					return
				}
				removeGatewayMTUSupport()
			})

			ginkgo.By("Annotate the node as not supporting gateway MTU")
			setGatewayMTUSupport("false")

			ginkgo.By("Verify options:gateway_mtu is removed from " + lrpName)
			gomega.Eventually(gatewayMTUSet, 60*time.Second, 2*time.Second).Should(gomega.BeFalse())

			ginkgo.By("Remove the annotation so the node reports gateway MTU support again")
			removeGatewayMTUSupport()

			ginkgo.By("Verify options:gateway_mtu is restored on " + lrpName)
			gomega.Eventually(gatewayMTUSet, 60*time.Second, 2*time.Second).Should(gomega.BeTrue())
		})
	})
})

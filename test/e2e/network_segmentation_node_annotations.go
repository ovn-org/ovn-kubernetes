// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	ovnkubeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

var _ = Describe("Network Segmentation: Node Annotations", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("network-segmentation-node-annotations")
	f.SkipNamespaceCreation = true

	var (
		cs clientset.Interface
	)

	BeforeEach(func() {
		cs = f.ClientSet

		var err error
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		Expect(err).NotTo(HaveOccurred())
		f.Namespace = namespace
	})

	Context("Tunnel ID Annotations", func() {
		// NOTE: These tests validate the behavior based on the Layer2UsesTransitRouter configuration.
		// The configuration is determined by the k8s.ovn.org/layer2-topology-version annotation on nodes.
		// Version "2.0" indicates transit router topology (Layer2UsesTransitRouter=true).
		// Missing or other values indicate legacy topology (Layer2UsesTransitRouter=false).

		// Serial() is required because this test modifies cluster-wide state (node annotations)
		// that controls Layer2UsesTransitRouter and affects ovnkube-control-plane behavior.
		It("should clean up tunnel ID annotations when UDN is deleted with transit router topology", Serial, func(ctx SpecContext) {
			// This test verifies that tunnel ID annotations are cleaned up when a UDN is deleted
			// in transit router topology. The test simulates a migration scenario by:
			// 1. Creating an L2 primary network
			// 2. Ensuring all nodes have transit router topology annotation
			// 3. Manually injecting tunnel ID annotations on nodes (simulating legacy state)
			// 4. Deleting the UDN
			// 5. Verifying that tunnel ID annotations are cleaned up during UDN deletion
			testUdnName := "node-annotation-l2-tunnel-id-cleanup"

			By("ensuring all nodes have transit router topology annotation")
			nodeList, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodeList.Items)).To(BeNumerically(">", 0), "should have at least one node")

			for i := range nodeList.Items {
				node := &nodeList.Items[i]
				if node.Annotations[ovnkubeutil.Layer2TopologyVersion] != ovnkubeutil.TransitRouterTopoVersion {
					node.Annotations[ovnkubeutil.Layer2TopologyVersion] = ovnkubeutil.TransitRouterTopoVersion
					_, err = cs.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred(), "failed to set transit router topology on node %s", node.Name)
					framework.Logf("Set transit router topology on node %s", node.Name)
				} else {
					framework.Logf("Node %s already has transit router topology", node.Name)
				}
			}

			By("creating an L2 primary network")
			netConfig := &networkAttachmentConfigParams{
				name:      testUdnName,
				namespace: f.Namespace.Name,
				topology:  "layer2",
				cidr:      joinStrings("10.129.0.0/16", "2014:100:200::0/60"),
				role:      "primary",
			}
			udnManifest := generateUserDefinedNetworkManifest(netConfig, f.ClientSet)
			cleanup, err := createManifest(f.Namespace.Name, udnManifest)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for network to be ready")
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, f.Namespace.Name, testUdnName), 30*time.Second, time.Second).Should(Succeed())

			By("verifying no tunnel ID annotations are allocated in transit router topology")

			nodeList, err = e2enode.GetReadySchedulableNodes(context.TODO(), cs)
			Expect(err).NotTo(HaveOccurred())
			for i := range nodeList.Items {
				node := &nodeList.Items[i]
				node, err = cs.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				_, err := ovnkubeutil.ParseUDNLayer2NodeGRLRPTunnelIDs(node, f.Namespace.Name+"_"+testUdnName)
				Expect(ovnkubeutil.IsAnnotationNotSetError(err)).To(BeTrue(),
					"node %s should NOT have tunnel ID annotation with transit router topology", node.Name)
				framework.Logf("Node %s correctly has no tunnel ID annotation (transit router)", node.Name)
			}

			By("manually injecting tunnel ID annotations on nodes (simulating legacy migration scenario)")
			// Simulated controller-owned allocations must use the authorized writer.
			clusterManagerConfig := rest.CopyConfig(f.ClientConfig())
			clusterManagerConfig.Impersonate = rest.ImpersonationConfig{
				UserName: fmt.Sprintf("system:serviceaccount:%s:ovnkube-cluster-manager",
					deploymentconfig.Get().OVNKubernetesNamespace()),
			}
			clusterManagerClient, err := clientset.NewForConfig(clusterManagerConfig)
			Expect(err).NotTo(HaveOccurred())
			patchCtx, cancelPatch := context.WithTimeout(ctx, retryTimeout)
			defer cancelPatch()
			networkKey := f.Namespace.Name + "_" + testUdnName
			injectedTunnelIDs := make(map[string]int)
			nodeList, err = e2enode.GetReadySchedulableNodes(context.TODO(), cs)
			Expect(err).NotTo(HaveOccurred())

			for i := range nodeList.Items {
				node := &nodeList.Items[i]
				// Inject a unique tunnel ID for each node (starting from 100 to avoid conflicts)
				tunnelID := 10 + i
				injectedTunnelIDs[node.Name] = tunnelID

				err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
					// Rebuild from current annotations to preserve concurrent allocations.
					currentNode, err := cs.CoreV1().Nodes().Get(patchCtx, node.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					annotations, err := ovnkubeutil.UpdateUDNLayer2NodeGRLRPTunnelIDs(currentNode.Annotations, networkKey, tunnelID)
					if err != nil {
						return err
					}
					annotationPatch, err := json.Marshal(map[string]any{
						"metadata": map[string]any{
							"resourceVersion": currentNode.ResourceVersion,
							"annotations": map[string]string{
								ovntypes.UDNLayer2NodeGRLRPTunnelIDAnnotation: annotations[ovntypes.UDNLayer2NodeGRLRPTunnelIDAnnotation],
							},
						},
					})
					if err != nil {
						return err
					}
					_, err = clusterManagerClient.CoreV1().Nodes().Patch(patchCtx, node.Name,
						k8stypes.MergePatchType, annotationPatch, metav1.PatchOptions{}, "status")
					return err
				})
				Expect(err).NotTo(HaveOccurred(), "failed to inject tunnel ID on node %s", node.Name)
				framework.Logf("Injected tunnel ID %s: %d on node %s (simulating legacy state)", networkKey, tunnelID, node.Name)
			}

			By("verifying injected tunnel ID annotations are present")
			nodeList, err = e2enode.GetReadySchedulableNodes(context.TODO(), cs)
			Expect(err).NotTo(HaveOccurred())
			for i := range nodeList.Items {
				node := &nodeList.Items[i]
				node, err = cs.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				tunnelID, err := ovnkubeutil.ParseUDNLayer2NodeGRLRPTunnelIDs(node, networkKey)
				Expect(err).NotTo(HaveOccurred(), "should be able to parse injected tunnel ID on node %s", node.Name)
				Expect(tunnelID).To(Equal(injectedTunnelIDs[node.Name]), "injected tunnel ID should match on node %s", node.Name)
				framework.Logf("Verified injected tunnel ID %d on node %s", tunnelID, node.Name)
			}

			By("deleting the UDN")
			err = f.DynamicClient.Resource(udnGVR).Namespace(f.Namespace.Name).Delete(context.TODO(), testUdnName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred(), "failed to delete UDN %s", testUdnName)
			framework.Logf("Initiated deletion of UDN %s", testUdnName)

			// Clean up the temporary manifest files
			cleanup()

			By("verifying UDN is completely deleted")
			Eventually(func() error {
				_, err := f.DynamicClient.Resource(udnGVR).Namespace(f.Namespace.Name).Get(context.TODO(), testUdnName, metav1.GetOptions{})
				if err != nil {
					// Expected: UDN should not be found
					framework.Logf("UDN %s successfully deleted", testUdnName)
					return nil
				}
				return fmt.Errorf("UDN %s still exists, waiting for deletion", testUdnName)
			}, 120*time.Second, time.Second).Should(Succeed(), "UDN should be completely deleted")

			By("verifying tunnel ID annotations are cleaned up after UDN deletion")
			Eventually(func() error {
				nodeList, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
				if err != nil {
					return err
				}

				for i := range nodeList.Items {
					node := &nodeList.Items[i]
					// Refresh node to get latest annotations
					node, err = cs.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
					if err != nil {
						return fmt.Errorf("failed to get node %s: %v", node.Name, err)
					}

					_, err := ovnkubeutil.ParseUDNLayer2NodeGRLRPTunnelIDs(node, networkKey)
					if err != nil {
						if ovnkubeutil.IsAnnotationNotSetError(err) {
							// Expected: annotation should be removed
							framework.Logf("Node %s tunnel ID annotation correctly cleaned up", node.Name)
							continue
						}
						return fmt.Errorf("failed to parse tunnel ID for node %s: %v", node.Name, err)
					}

					// If we still have a tunnel ID, cleanup hasn't happened yet
					return fmt.Errorf("node %s still has tunnel ID annotation, waiting for cleanup", node.Name)
				}

				return nil
			}, 60*time.Second, 2*time.Second).Should(Succeed(), "all tunnel ID annotations should be cleaned up after UDN deletion")
		})

		It("should have all nodes with transit router settings Layer2TopologyVersion set to 2.0", func() {
			By("checking current Layer2 topology mode on nodes")
			nodeList, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodeList.Items)).To(BeNumerically(">", 0), "should have at least one node")

			// Determine if cluster is using transit router topology
			nodesUsingTransitRouter := make(map[string]bool)
			for i := range nodeList.Items {
				node := &nodeList.Items[i]
				usesTransitRouter := ovnkubeutil.UDNLayer2NodeUsesTransitRouter(node)
				nodesUsingTransitRouter[node.Name] = usesTransitRouter
				framework.Logf("Node %s uses transit router: %v (annotation: %s)",
					node.Name, usesTransitRouter, node.Annotations[ovnkubeutil.Layer2TopologyVersion])
			}
			// Check if ALL nodes are using transit router
			allNodesUseTransitRouter := true
			for _, usesTransitRouter := range nodesUsingTransitRouter {
				if !usesTransitRouter {
					allNodesUseTransitRouter = false
					break
				}
			}
			Expect(allNodesUseTransitRouter).To(BeTrue(), "all nodes should have TransitRouter enabled")
		})

		It("should NOT allocate tunnel ID annotations for L3 networks", func(ctx SpecContext) {
			testUdnName := "node-annotation-not-l3-tunnel-id"
			By("creating an L3 network")
			netConfig := &networkAttachmentConfigParams{
				name:      testUdnName,
				namespace: f.Namespace.Name,
				topology:  "layer3",
				cidr:      joinStrings("103.103.0.0/16", "2014:100:200::0/60"),
				role:      "primary",
			}
			udnManifest := generateUserDefinedNetworkManifest(netConfig, f.ClientSet)
			cleanup, err := createManifest(f.Namespace.Name, udnManifest)
			defer cleanup()
			Expect(err).NotTo(HaveOccurred())

			By("waiting for network to be ready")
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, f.Namespace.Name, testUdnName), 30*time.Second, time.Second).Should(Succeed())

			By("verifying tunnel ID annotations are NOT allocated on nodes")
			nodeList, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
			Expect(err).NotTo(HaveOccurred())

			for i := range nodeList.Items {
				node := &nodeList.Items[i]
				// Refresh node to get latest annotations
				node, err = cs.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				_, err := ovnkubeutil.ParseUDNLayer2NodeGRLRPTunnelIDs(node, f.Namespace.Name+"_"+testUdnName)
				Expect(ovnkubeutil.IsAnnotationNotSetError(err)).To(BeTrue(),
					"node %s should NOT have tunnel ID annotation for L3 network", node.Name)
			}
		})

		It("should NOT allocate tunnel ID annotations for secondary L2 networks", func(ctx SpecContext) {
			testUdnName := "node-annotation-not-l2-tunnel-id"
			By("creating a secondary L2 network")
			netConfig := &networkAttachmentConfigParams{
				name:      testUdnName,
				namespace: f.Namespace.Name,
				topology:  "layer2",
				cidr:      joinStrings("10.130.0.0/16", "2014:100:200::0/60"),
				role:      "secondary",
			}
			udnManifest := generateUserDefinedNetworkManifest(netConfig, f.ClientSet)
			cleanup, err := createManifest(f.Namespace.Name, udnManifest)
			defer cleanup()
			Expect(err).NotTo(HaveOccurred())

			By("waiting for network to be ready")
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, f.Namespace.Name, testUdnName), 30*time.Second, time.Second).Should(Succeed())

			By("verifying tunnel ID annotations are NOT allocated on nodes")
			nodeList, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
			Expect(err).NotTo(HaveOccurred())

			for i := range nodeList.Items {
				node := &nodeList.Items[i]
				// Refresh node to get latest annotations
				node, err = cs.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				_, err := ovnkubeutil.ParseUDNLayer2NodeGRLRPTunnelIDs(node, f.Namespace.Name+"_"+testUdnName)
				Expect(ovnkubeutil.IsAnnotationNotSetError(err)).To(BeTrue(),
					"node %s should NOT have tunnel ID annotation for secondary L2 network", node.Name)
			}
		})
	})
})

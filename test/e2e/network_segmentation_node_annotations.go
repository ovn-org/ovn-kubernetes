// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	ovnkubeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
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

	It("omits the legacy gateway annotation and CNI filter while blocking pod-originated RAs", func(ctx SpecContext) {
		if !isIPv6Supported(cs) {
			Skip("requires IPv6")
		}
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, cs, 2)
		Expect(err).NotTo(HaveOccurred())
		if len(nodes.Items) < 2 {
			Skip("requires two nodes")
		}
		const networkName = "ra-guard"
		config := &networkAttachmentConfigParams{
			name: networkName, namespace: f.Namespace.Name, topology: "layer2", role: "primary",
			cidr: filterCIDRsAndJoin(cs, joinStrings("10.129.0.0/16", "2014:100:200::0/60")),
		}
		cleanup, err := createManifest(f.Namespace.Name, generateUserDefinedNetworkManifest(config, cs))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(cleanup)
		Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, f.Namespace.Name, networkName), 30*time.Second, time.Second).Should(Succeed())
		pods := make([]*corev1.Pod, 2)
		for i := range pods {
			pods[i] = runUDNPod(cs, f.Namespace.Name, podConfiguration{
				name: fmt.Sprintf("ra-pod-%d", i), namespace: f.Namespace.Name, isPrivileged: true,
				nodeSelector: map[string]string{nodeHostnameKey: nodes.Items[i].Name},
			}, func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Image = images.Netshoot()
				pod.Spec.Containers[0].Command = []string{"sleep", "infinity"}
				pod.Spec.Containers[0].Args = nil
			})
			annotation, err := ovnkubeutil.UnmarshalPodAnnotation(pods[i].Annotations, namespacedName(f.Namespace.Name, networkName))
			Expect(err).NotTo(HaveOccurred())
			Expect(annotation.GatewayIPv6LLA).To(BeNil(), "new workloads must omit the legacy gateway LLA")
			output, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", pods[i].Name, "--", "nft", "list", "tables")
			Expect(err).NotTo(HaveOccurred(), output)
			Expect(output).NotTo(ContainSubstring("table netdev ingress_filter"), "new interfaces must not install the CNI RA filter")
		}
		execScript := func(pod *corev1.Pod, script string) (string, error) {
			return e2ekubectl.RunKubectl(pod.Namespace, "exec", pod.Name, "--", "sh", "-c", script)
		}
		receiverAnnotation, err := ovnkubeutil.UnmarshalPodAnnotation(pods[1].Annotations, namespacedName(f.Namespace.Name, networkName))
		Expect(err).NotTo(HaveOccurred())
		receiverIP, err := ovnkubeutil.MatchFirstIPNetFamily(true, receiverAnnotation.IPs)
		Expect(err).NotTo(HaveOccurred())
		Eventually(ctx, func() error {
			_, err := execScript(pods[0], fmt.Sprintf("ping -6 -c 1 -W 2 %s", receiverIP.IP))
			return err
		}, 30*time.Second, time.Second).Should(Succeed(), "ordinary cross-node IPv6 traffic must work")
		By("soliciting a native gateway RA without a CNI filter")
		output, err := execScript(pods[1], `python3 - <<'PY'
from scapy.all import Ether, IPv6, ICMPv6ND_RS, ICMPv6ND_RA, get_if_hwaddr, srp1
from scapy.utils6 import in6_mactoifaceid
iface = 'ovn-udn1'
mac = get_if_hwaddr(iface)
lla = 'fe80::' + in6_mactoifaceid(mac)
reply = srp1(Ether(src=mac, dst='33:33:00:00:00:02') / IPv6(src=lla, dst='ff02::2', hlim=255) / ICMPv6ND_RS(), iface=iface, timeout=10, retry=2, verbose=False)
assert reply is not None and ICMPv6ND_RA in reply, 'native gateway RA not received'
PY`)
		Expect(err).NotTo(HaveOccurred(), output)
		By("checking that a remote workload cannot advertise itself as a default router")
		output, err = execScript(pods[1], `python3 -u - <<'PY' >/tmp/ra-capture.log 2>&1 &
from pathlib import Path
import time
from scapy.all import AsyncSniffer, ICMPv6ND_RA
capture = AsyncSniffer(iface='ovn-udn1', filter='icmp6',
    lfilter=lambda p: ICMPv6ND_RA in p and p[ICMPv6ND_RA].routerlifetime == 1234,
    started_callback=lambda: open('/tmp/ra-capture-ready', 'w').close())
capture.start()
deadline = time.monotonic() + 60
while not Path('/tmp/ra-capture-stop').exists():
    if time.monotonic() > deadline:
        raise RuntimeError('sender did not finish before capture deadline')
    time.sleep(0.1)
packets = capture.stop()
open('/tmp/ra-capture-result', 'w').write(str(len(packets)))
PY`)
		Expect(err).NotTo(HaveOccurred(), output)
		Eventually(ctx, func() error {
			_, err := execScript(pods[1], "test -f /tmp/ra-capture-ready")
			return err
		}, 10*time.Second, time.Second).Should(Succeed())
		output, err = execScript(pods[0], `python3 - <<'PY'
from scapy.all import Ether, IPv6, ICMPv6ND_RA, get_if_hwaddr, sendp
from scapy.utils6 import in6_mactoifaceid
iface = 'ovn-udn1'
mac = get_if_hwaddr(iface)
lla = 'fe80::' + in6_mactoifaceid(mac)
sendp(Ether(src=mac, dst='33:33:00:00:00:01') / IPv6(src=lla, dst='ff02::1', hlim=255) / ICMPv6ND_RA(routerlifetime=1234), iface=iface, count=3, inter=0.2, verbose=False)
PY`)
		Expect(err).NotTo(HaveOccurred(), output)
		output, err = execScript(pods[1], "touch /tmp/ra-capture-stop")
		Expect(err).NotTo(HaveOccurred(), output)
		Eventually(ctx, func() (string, error) {
			return execScript(pods[1], "cat /tmp/ra-capture-result")
		}, 20*time.Second, time.Second).Should(Equal("0"), "pod-originated RAs must not reach another node's workload")
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
			networkKey := f.Namespace.Name + "_" + testUdnName
			injectedTunnelIDs := make(map[string]int)
			nodeList, err = e2enode.GetReadySchedulableNodes(context.TODO(), cs)
			Expect(err).NotTo(HaveOccurred())

			for i := range nodeList.Items {
				node := &nodeList.Items[i]
				// Inject a unique tunnel ID for each node (starting from 100 to avoid conflicts)
				tunnelID := 10 + i
				injectedTunnelIDs[node.Name] = tunnelID

				node.Annotations, err = ovnkubeutil.UpdateUDNLayer2NodeGRLRPTunnelIDs(node.Annotations, networkKey, tunnelID)
				Expect(err).NotTo(HaveOccurred())
				_, err = cs.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
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

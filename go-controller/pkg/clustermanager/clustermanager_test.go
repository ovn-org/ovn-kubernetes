// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	utilnet "k8s.io/utils/net"

	hotypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	udncontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork"
	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	networkconnect "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// ovnNodeIDAnnotaton is the node annotation name used to store the node id.
	ovnNodeIDAnnotaton = "k8s.ovn.org/node-id"

	// ovnTransitSwitchPortAddrAnnotation is the node annotation name to store the transit switch port ips.
	ovnTransitSwitchPortAddrAnnotation = "k8s.ovn.org/node-transit-switch-port-ifaddr"
)

var _ = ginkgo.Describe("Cluster Manager", func() {
	var (
		app *cli.App
		f   *factory.WatchFactory
		wg  *sync.WaitGroup
	)

	const (
		clusterIPNet             string = "10.1.0.0"
		clusterCIDR              string = clusterIPNet + "/16"
		clusterv6CIDR            string = "aef0::/48"
		hybridOverlayClusterCIDR string = "11.1.0.0/16/24"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		wg = &sync.WaitGroup{}
	})

	ginkgo.AfterEach(func() {
		if f != nil {
			f.Shutdown()
			f = nil
		}
		wg.Wait()
	})

	ginkgo.Context("Node subnet allocations", func() {
		ginkgo.It("Linux nodes", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node3",
						},
					},
				}
				kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				c, cancel := context.WithCancel(ctx.Context)
				defer cancel()
				clusterManager, err := NewClusterManager(fakeClient, f, "identity", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(c)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer clusterManager.Stop()

				// Check that cluster manager has set the subnet annotation for each node.
				for _, n := range nodes {
					gomega.Eventually(func() ([]*net.IPNet, error) {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return nil, err
						}

						return util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
					}, 2).Should(gomega.HaveLen(1))
				}

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Linux nodes - clear subnet annotations", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node3",
						},
					},
				}
				kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				c, cancel := context.WithCancel(ctx.Context)
				defer cancel()
				clusterManager, err := NewClusterManager(fakeClient, f, "identity", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(c)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer clusterManager.Stop()

				// Check that cluster manager has set the subnet annotation for each node.
				for _, n := range nodes {
					gomega.Eventually(func() ([]*net.IPNet, error) {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return nil, err
						}

						return util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
					}, 2).Should(gomega.HaveLen(1))
				}

				// Clear the subnet annotation of nodes and make sure it is re-allocated by cluster manager.
				for _, n := range nodes {
					nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, n.Name)
					util.DeleteNodeHostSubnetAnnotation(nodeAnnotator)
					err = nodeAnnotator.Run()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}

				// Check that cluster manager has reset the subnet annotation for each node.
				for _, n := range nodes {
					gomega.Eventually(func() ([]*net.IPNet, error) {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return nil, err
						}

						return util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
					}, 2).Should(gomega.HaveLen(1))
				}

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Hybrid and linux nodes", func() {

			app.Action = func(ctx *cli.Context) error {
				nodes := []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node3",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "winnode",
							Labels: map[string]string{corev1.LabelOSStable: "windows"},
						},
					}}
				kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				c, cancel := context.WithCancel(ctx.Context)
				defer cancel()
				clusterManager, err := NewClusterManager(fakeClient, f, "identity", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(c)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer clusterManager.Stop()

				// Check that cluster manager has set the subnet annotation for each node.
				for _, n := range nodes {
					if n.Name == "winnode" {
						continue
					}

					gomega.Eventually(func() ([]*net.IPNet, error) {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return nil, err
						}

						return util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
					}, 2).Should(gomega.HaveLen(1))
				}

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"--no-hostsubnet-nodes=kubernetes.io/os=windows",
				"-cluster-subnets=" + clusterCIDR,
				"-gateway-mode=shared",
				"-enable-hybrid-overlay",
				"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Hybrid nodes - clear subnet annotations", func() {

			app.Action = func(ctx *cli.Context) error {
				nodes := []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "winnode1",
							Labels: map[string]string{corev1.LabelOSStable: "windows"},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "winnode2",
							Labels: map[string]string{corev1.LabelOSStable: "windows"},
						},
					},
				}
				kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				c, cancel := context.WithCancel(ctx.Context)
				defer cancel()
				clusterManager, err := NewClusterManager(fakeClient, f, "identity", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(c)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer clusterManager.Stop()

				// Check that cluster manager has set the subnet annotation for each node.
				for _, n := range nodes {
					gomega.Eventually(func() (map[string]string, error) {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return nil, err
						}
						return updatedNode.Annotations, nil
					}, 2).Should(gomega.HaveKey(hotypes.HybridOverlayNodeSubnet))

					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}
						_, err = util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
						return err
					}, 2).Should(gomega.MatchError("could not find \"k8s.ovn.org/node-subnets\" annotation"))
				}

				// Clear the subnet annotation of nodes and make sure it is re-allocated by cluster manager.
				for _, n := range nodes {
					nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, n.Name)

					nodeAnnotations := n.Annotations
					for k, v := range nodeAnnotations {
						gomega.Expect(nodeAnnotator.Set(k, v)).To(gomega.Succeed())
					}
					nodeAnnotator.Delete(hotypes.HybridOverlayNodeSubnet)
					err = nodeAnnotator.Run()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}

				for _, n := range nodes {
					gomega.Eventually(func() (map[string]string, error) {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return nil, err
						}
						return updatedNode.Annotations, nil
					}, 2).Should(gomega.HaveKey(hotypes.HybridOverlayNodeSubnet))

					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}
						_, err = util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
						return err
					}, 2).Should(gomega.MatchError("could not find \"k8s.ovn.org/node-subnets\" annotation"))
				}
				return nil
			}

			err := app.Run([]string{
				app.Name,
				"--no-hostsubnet-nodes=kubernetes.io/os=windows",
				"-cluster-subnets=" + clusterCIDR,
				"-gateway-mode=shared",
				"-enable-hybrid-overlay",
				"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Add subnet to primary layer3 UDN", func() {
			app.Action = func(ctx *cli.Context) error {

				testNADName := "test/primary"
				oneSubnetNetwork := &ovncnitypes.NetConf{
					Topology: ovntypes.Layer3Topology,
					NADName:  testNADName,
					NetConf: cnitypes.NetConf{
						Name: "primary",
						Type: "ovn-k8s-cni-overlay",
					},
					Subnets: "10.10.0.0/16/17",
					Role:    ovntypes.NetworkRolePrimary,
					MTU:     1400,
				}
				twoSubnetNetwork := &ovncnitypes.NetConf{
					Topology: ovntypes.Layer3Topology,
					NADName:  testNADName,
					NetConf: cnitypes.NetConf{
						Name: "primary",
						Type: "ovn-k8s-cni-overlay",
					},
					Subnets: "10.10.0.0/16/17, 10.20.0.0/16/17",
					Role:    ovntypes.NetworkRolePrimary,
					MTU:     1400,
				}

				nodes := []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node3",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node4",
						},
					},
				}

				objs := []runtime.Object{
					&corev1.NodeList{
						Items: nodes,
					},
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""
				config.OVNKubernetesFeature.EnableMultiNetwork = true

				fakeClient := util.GetOVNClientset(objs...).GetClusterManagerClientset()

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				c, cancel := context.WithCancel(ctx.Context)
				defer cancel()
				clusterManager, err := NewClusterManager(fakeClient, f, "identity", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(c)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer clusterManager.Stop()

				meetsExpectations := func(g gomega.Gomega, netName string,
					expectedSubnetCount int, expectedSubnets []string, expectedNodeSubnetMap map[string]string) {
					// Check subnet allocations
					nodes, err := fakeClient.KubeClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
					g.Expect(err).ToNot(gomega.HaveOccurred())

					nodesWithSubnets := 0
					actualSubnets := []string{}
					actualNodeSubnetMap := map[string]string{}
					for _, node := range nodes.Items {
						ipnets, err := util.ParseNodeHostSubnetAnnotation(&node, netName)
						if err == nil {
							nodesWithSubnets++
							g.Expect(ipnets).To(gomega.HaveLen(1))
							actualSubnets = append(actualSubnets, ipnets[0].String())
							actualNodeSubnetMap[node.Name] = ipnets[0].String()
						}
					}
					g.Expect(nodesWithSubnets).To(gomega.Equal(expectedSubnetCount))
					if expectedSubnets != nil {
						g.Expect(actualSubnets).To(gomega.ConsistOf(expectedSubnets))
					}

					for k, v := range expectedNodeSubnetMap {

						g.Expect(actualNodeSubnetMap[k]).To(gomega.Equal(v))
					}
				}

				// Wait for nodes to be allocated a default network subnet.
				// Only the number of allocated subnets is checked here; the actual subnet values are not validated.
				gomega.Eventually(func(g gomega.Gomega) {
					meetsExpectations(g, ovntypes.DefaultNetworkName, 4, nil, nil)
				}, "10s", "250ms").Should(gomega.Succeed())

				// create NAD with one subnet
				namespace, nadName, err := cache.SplitMetaNamespaceKey(testNADName)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				nad, err := testing.BuildNAD(nadName, namespace, oneSubnetNetwork)
				nad.ResourceVersion = "2" // apparently changing the actual config isn't enough
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(context.Background(), nad, metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				// Check that only 2 nodes get subnet due to subnet shortage
				gomega.Eventually(func(g gomega.Gomega) {
					meetsExpectations(g, nadName, 2, []string{"10.10.0.0/17", "10.10.128.0/17"}, nil)
				}, "10s", "250ms").Should(gomega.Succeed())

				getNodeSubnetMap := func(netName string) map[string]string {
					nodes, err := fakeClient.KubeClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
					gomega.Expect(err).ToNot(gomega.HaveOccurred())

					ret := map[string]string{}
					for _, node := range nodes.Items {
						ipnets, _ := util.ParseNodeHostSubnetAnnotation(&node, netName)
						if len(ipnets) > 0 {
							ret[node.Name] = ipnets[0].String()
						}
					}
					return ret
				}

				expectedNodeSubnetMap := getNodeSubnetMap(nadName)

				// Update NAD to add one more subnet
				nad, err = testing.BuildNAD(nadName, namespace, twoSubnetNetwork)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Update(context.Background(), nad, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				// Verify that all nodes have been allocated subnets after adding the new subnet.
				gomega.Eventually(func(g gomega.Gomega) {
					meetsExpectations(g, nadName, 4, []string{"10.10.0.0/17", "10.10.128.0/17", "10.20.0.0/17", "10.20.128.0/17"}, expectedNodeSubnetMap)
				}, "10s", "250ms").Should(gomega.Succeed())

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("Node Id allocations", func() {
		ginkgo.It("check for node id allocations", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node3",
						},
					},
				}
				kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				clusterManager, err := NewClusterManager(fakeClient, f, "identity", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer clusterManager.Stop()

				// Check that cluster manager has allocated id for each node
				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						nodeId, ok := updatedNode.Annotations[ovnNodeIDAnnotaton]
						if !ok {
							return fmt.Errorf("expected node annotation for node %s to have node id allocated", n.Name)
						}

						_, err = strconv.Atoi(nodeId)
						if err != nil {
							return fmt.Errorf("expected node annotation for node %s to be an integer value, got %s", n.Name, nodeId)
						}
						return nil
					}).ShouldNot(gomega.HaveOccurred())
				}

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("clear the node ids and check", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node3",
						},
					},
				}
				kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				clusterManager, err := NewClusterManager(fakeClient, f, "identity", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer clusterManager.Stop()

				nodeIds := make(map[string]string)
				// Check that cluster manager has allocated id for each node before clearing
				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						nodeId, ok := updatedNode.Annotations[ovnNodeIDAnnotaton]
						if !ok {
							return fmt.Errorf("expected node annotation for node %s to have node id allocated", n.Name)
						}

						_, err = strconv.Atoi(nodeId)
						if err != nil {
							return fmt.Errorf("expected node annotation for node %s to be an integer value, got %s", n.Name, nodeId)
						}

						nodeIds[n.Name] = nodeId
						return nil
					}).ShouldNot(gomega.HaveOccurred())
				}

				// Clear the node id annotation of nodes and make sure it is reset by cluster manager
				// with the same ids.
				for _, n := range nodes {
					nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, n.Name)

					nodeAnnotations := n.Annotations
					for k, v := range nodeAnnotations {
						gomega.Expect(nodeAnnotator.Set(k, v)).To(gomega.Succeed())
					}
					nodeAnnotator.Delete(ovnNodeIDAnnotaton)
					err = nodeAnnotator.Run()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}

				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						nodeId, ok := updatedNode.Annotations[ovnNodeIDAnnotaton]
						if !ok {
							return fmt.Errorf("expected node annotation for node %s to have node id allocated", n.Name)
						}

						_, err = strconv.Atoi(nodeId)
						if err != nil {
							return fmt.Errorf("expected node annotation for node %s to be an integer value, got %s", n.Name, nodeId)
						}

						gomega.Expect(nodeId).To(gomega.Equal(nodeIds[n.Name]))
						return nil
					}).ShouldNot(gomega.HaveOccurred())
				}

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Stop and start a new cluster manager and verify the node ids", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node3",
						},
					},
				}
				kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				wg1 := &sync.WaitGroup{}
				clusterManager, err := NewClusterManager(fakeClient, f, "cm1", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Check that cluster manager has allocated id for each node before clearing
				nodeIds := make(map[string]string)
				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						nodeId, ok := updatedNode.Annotations[ovnNodeIDAnnotaton]
						if !ok {
							return fmt.Errorf("expected node annotation for node %s to have node id allocated", n.Name)
						}

						_, err = strconv.Atoi(nodeId)
						if err != nil {
							return fmt.Errorf("expected node annotation for node %s to be an integer value, got %s", n.Name, nodeId)
						}

						nodeIds[n.Name] = nodeId
						return nil
					}).ShouldNot(gomega.HaveOccurred())
				}

				updatedNodes := []corev1.Node{}
				for _, n := range nodes {
					updatedNode, _ := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
					updatedNodes = append(updatedNodes, *updatedNode)
				}
				// stop the cluster manager and start a new instance and make sure the node ids are same.
				clusterManager.Stop()
				wg1.Wait()

				// Close the watch factory and create a new one
				f.Shutdown()
				kubeFakeClient = fake.NewSimpleClientset(&corev1.NodeList{
					Items: updatedNodes,
				})
				fakeClient = &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}
				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				cm2, err := NewClusterManager(fakeClient, f, "cm2", nil)
				gomega.Expect(cm2).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = cm2.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer cm2.Stop()

				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						nodeId, ok := updatedNode.Annotations[ovnNodeIDAnnotaton]
						if !ok {
							return fmt.Errorf("expected node annotation for node %s to have node id allocated", n.Name)
						}

						_, err = strconv.Atoi(nodeId)
						if err != nil {
							return fmt.Errorf("expected node annotation for node %s to be an integer value, got %s", n.Name, nodeId)
						}

						gomega.Expect(nodeId).To(gomega.Equal(nodeIds[n.Name]))
						return nil
					}).ShouldNot(gomega.HaveOccurred())
				}

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Stop cluster manager, set duplicate id, restart and verify the node ids", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node3",
						},
					},
				}
				kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				wg1 := &sync.WaitGroup{}
				clusterManager, err := NewClusterManager(fakeClient, f, "cm1", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				nodeIds := make(map[string]string)
				// Check that cluster manager has allocated id for each node before clearing
				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						nodeId, ok := updatedNode.Annotations[ovnNodeIDAnnotaton]
						if !ok {
							return fmt.Errorf("expected node annotation for node %s to have node id allocated", n.Name)
						}

						_, err = strconv.Atoi(nodeId)
						if err != nil {
							return fmt.Errorf("expected node annotation for node %s to be an integer value, got %s", n.Name, nodeId)
						}

						nodeIds[n.Name] = nodeId
						return nil
					}).ShouldNot(gomega.HaveOccurred())
				}

				// stop the cluster manager.
				clusterManager.Stop()
				wg1.Wait()

				updatedNodes := []corev1.Node{}
				node2, _ := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), "node2", metav1.GetOptions{})
				for _, n := range nodes {
					updatedNode, _ := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
					if updatedNode.Name == "node3" {
						// Make the id of node3 duplicate.
						updatedNode.Annotations[ovnNodeIDAnnotaton] = node2.Annotations[ovnNodeIDAnnotaton]
					}
					updatedNodes = append(updatedNodes, *updatedNode)
				}

				// Close the watch factory and create a new one
				f.Shutdown()
				kubeFakeClient = fake.NewSimpleClientset(&corev1.NodeList{
					Items: updatedNodes,
				})
				fakeClient = &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}
				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Start a new cluster manager
				cm2, err := NewClusterManager(fakeClient, f, "cm2", nil)
				gomega.Expect(cm2).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = cm2.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer cm2.Stop()

				// Get the node ids of node2 and node3 and make sure that they are not equal
				gomega.Eventually(func() error {
					n2, _ := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), "node2", metav1.GetOptions{})
					n3, _ := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), "node3", metav1.GetOptions{})
					n2Id := n2.Annotations[ovnNodeIDAnnotaton]
					n3Id := n3.Annotations[ovnNodeIDAnnotaton]
					if n2Id == n3Id {
						return fmt.Errorf("expected node annotation for node2 and node3 to be not equal, but they are : node id %s", n2Id)
					}
					return nil
				}).ShouldNot(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("tunnel keys allocations", func() {
		ginkgo.It("check for tunnel keys allocations", func() {
			app.Action = func(_ *cli.Context) error {
				nad1 := testing.GenerateNAD("test1", "test1", "test", ovntypes.Layer2Topology,
					"10.0.0.0/24", ovntypes.NetworkRolePrimary)
				// start with test1 network that already has keys allocated
				nad1.Annotations = map[string]string{
					ovntypes.OvnNetworkTunnelKeysAnnotation: "[16711685,16715780]",
				}
				// and test2 network without keys allocated
				nad2 := testing.GenerateNAD("test2", "test2", "test", ovntypes.Layer2Topology,
					"10.0.0.0/24", ovntypes.NetworkRolePrimary)
				clientSet := util.GetOVNClientset(nad1, nad2)

				// init the allocator that should reserve already allocated keys for test1
				allocator, err := initTunnelKeysAllocator(clientSet.NetworkAttchDefClient, clientSet.NetworkConnectClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// check that reserving different keys for test2 will fail
				err = allocator.ReserveKeys(id.NetworkTunnelKeyOwner("test1"), []int{16711685, 16715779})
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("can't reserve ids [16715779] for the resource network/test1. It is already allocated with different ids [16715780]"))
				// now try to allocate correct number of keys for test1 and check that returned IDs are correct
				ids, err := allocator.AllocateKeys(id.NetworkTunnelKeyOwner("test1"), 2, 2)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(ids).To(gomega.Equal([]int{16711685, 16715780}))
				// now allocate ids for networkID 1
				ids, err = allocator.AllocateKeys(id.NetworkTunnelKeyOwner("test2"), 1, 2)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(ids).To(gomega.Equal([]int{16711684, 16715779}))
				// now try networkID 3 to make sure IDs of nad test1 are not allocated again
				ids, err = allocator.AllocateKeys(id.NetworkTunnelKeyOwner("test3"), 3, 2)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(ids).To(gomega.Equal([]int{16711686, 16715781}))
				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("check for CNC tunnel keys allocations", func() {
			app.Action = func(_ *cli.Context) error {
				config.OVNKubernetesFeature.EnableNetworkConnect = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				// CNC uses networkID 4097 (4096+1) which allocates from the idsAllocator range
				// The idsAllocator starts at 16715779 (16711683 + 4096)
				// create CNC with already allocated tunnel key
				cnc1 := &networkconnect.ClusterNetworkConnect{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cnc1",
						Annotations: map[string]string{
							util.OvnConnectRouterTunnelKeyAnnotation: "16715779",
						},
					},
				}
				// create CNC without tunnel key annotation
				cnc2 := &networkconnect.ClusterNetworkConnect{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cnc2",
					},
				}
				clientSet := util.GetOVNClientset(cnc1, cnc2)

				// init the allocator that should reserve already allocated key for cnc1
				allocator, err := initTunnelKeysAllocator(clientSet.NetworkAttchDefClient, clientSet.NetworkConnectClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// check that reserving different keys for cnc1 will fail
				err = allocator.ReserveKeys(id.NetworkConnectTunnelKeyOwner("cnc1"), []int{16715780})
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("can't reserve ids [16715780] for the resource cnc/cnc1. It is already allocated with different ids [16715779]"))
				// now try to allocate key for cnc1 (using networkID 4097 as CNCs do)
				// and check that returned ID is the already reserved one
				ids, err := allocator.AllocateKeys(id.NetworkConnectTunnelKeyOwner("cnc1"), 4097, 1)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(ids).To(gomega.Equal([]int{16715779}))
				// now allocate id for cnc2 (which had no annotation, also using networkID 4097)
				ids, err = allocator.AllocateKeys(id.NetworkConnectTunnelKeyOwner("cnc2"), 4097, 1)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(ids).To(gomega.Equal([]int{16715780}))
				// now try cnc3 to make sure IDs of cnc1 and cnc2 are not allocated again
				ids, err = allocator.AllocateKeys(id.NetworkConnectTunnelKeyOwner("cnc3"), 4097, 1)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(ids).To(gomega.Equal([]int{16715781}))
				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("check for combined NAD and same-named CNC tunnel keys allocations", func() {
			app.Action = func(_ *cli.Context) error {
				config.OVNKubernetesFeature.EnableNetworkConnect = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				// create NAD with already allocated tunnel keys
				// NAD with networkID 2 gets keys: [16711685 (preserved), 16715779 (idsAllocator)]
				nad1 := testing.GenerateNAD("test1", "test1", "test", ovntypes.Layer2Topology,
					"10.0.0.0/24", ovntypes.NetworkRolePrimary)
				nad1.Annotations = map[string]string{
					ovntypes.OvnNetworkTunnelKeysAnnotation: "[16711685,16715779]",
				}
				// create CNC with already allocated tunnel key
				// CNC uses networkID 4097, so it gets keys from idsAllocator range (16715779+)
				cnc1 := &networkconnect.ClusterNetworkConnect{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test1",
						Annotations: map[string]string{
							util.OvnConnectRouterTunnelKeyAnnotation: "16715780",
						},
					},
				}
				clientSet := util.GetOVNClientset(nad1, cnc1)

				// init the allocator that should reserve keys for both NAD and CNC
				allocator, err := initTunnelKeysAllocator(clientSet.NetworkAttchDefClient, clientSet.NetworkConnectClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// verify NAD keys are reserved (networkID 2 => first key from preserved range)
				ids, err := allocator.AllocateKeys(id.NetworkTunnelKeyOwner("test1"), 2, 2)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(ids).To(gomega.Equal([]int{16711685, 16715779}))
				// verify CNC key is reserved (networkID 4097 => all keys from idsAllocator)
				ids, err = allocator.AllocateKeys(id.NetworkConnectTunnelKeyOwner("test1"), 4097, 1)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(ids).To(gomega.Equal([]int{16715780}))
				// test conflict: CNC tries to reserve NAD's random pool key (16715779)
				err = allocator.ReserveKeys("conflicting-cnc", []int{16715779})
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("already reserved"))
				// test conflict: NAD tries to reserve CNC's key (16715780)
				err = allocator.ReserveKeys("conflicting-nad", []int{16715780})
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("already reserved"))
				// allocate new keys for a new NAD (networkID 3) and ensure reserved keys are not reused
				ids, err = allocator.AllocateKeys(id.NetworkTunnelKeyOwner("newnetwork"), 3, 2)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				// first key: 16711686 (preserved range for networkID 3)
				// second key: 16715781 (skipping 16715779 for test1 and 16715780 for cnc1)
				gomega.Expect(ids).To(gomega.Equal([]int{16711686, 16715781}))
				// allocate new keys for a resource with networkID > 4096 (like CNCs do)
				// this should get ALL keys from the random pool, no deterministic key
				ids, err = allocator.AllocateKeys("newresource", 4097, 2)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				// both keys from random pool: 16715782, 16715783 (skipping all previously allocated)
				gomega.Expect(ids).To(gomega.Equal([]int{16715782, 16715783}))
				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("CNC tunnel key and subnet allocations at cluster manager (re)start", func() {
			app.Action = func(ctx *cli.Context) error {
				// Create two namespaces that the CNC's Primary UDN selector will match
				// Note: k8s.ovn.org/primary-user-defined-network label is required for primary UDNs
				ns1 := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "frontend-ns",
						Labels: map[string]string{
							"tier": "frontend",
							"k8s.ovn.org/primary-user-defined-network": "",
						},
					},
				}
				ns2 := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "backend-ns",
						Labels: map[string]string{
							"tier": "backend",
							"k8s.ovn.org/primary-user-defined-network": "",
						},
					},
				}

				// Create two primary UDNs - the UDN controller will create the corresponding NADs
				// UDN1 gets network ID 2 (layer3_2), UDN2 gets network ID 3 (layer3_3)
				udn1 := &udnv1.UserDefinedNetwork{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "frontend-udn",
						Namespace: "frontend-ns",
					},
					Spec: udnv1.UserDefinedNetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Role: udnv1.NetworkRolePrimary,
							Subnets: []udnv1.Layer3Subnet{
								{CIDR: "10.128.0.0/16", HostSubnet: 24},
							},
						},
					},
				}
				udn2 := &udnv1.UserDefinedNetwork{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backend-udn",
						Namespace: "backend-ns",
					},
					Spec: udnv1.UserDefinedNetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Role: udnv1.NetworkRolePrimary,
							Subnets: []udnv1.Layer3Subnet{
								{CIDR: "10.129.0.0/16", HostSubnet: 24},
							},
						},
					},
				}

				// Create a CNC with pre-populated tunnel key annotation
				// This simulates a cluster manager restart scenario where tunnel key was already allocated
				// Note: We don't pre-populate subnet annotation because network IDs are assigned dynamically
				cnc := &networkconnect.ClusterNetworkConnect{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-cnc",
						Annotations: map[string]string{
							// Pre-populate tunnel key (CNC uses networkID 4097+, so from idsAllocator range)
							util.OvnConnectRouterTunnelKeyAnnotation: "16715781",
						},
					},
					Spec: networkconnect.ClusterNetworkConnectSpec{
						NetworkSelectors: apitypes.NetworkSelectors{
							{
								NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
								PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
									NamespaceSelector: metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "tier",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"frontend", "backend"},
											},
										},
									},
								},
							},
						},
						ConnectSubnets: []networkconnect.ConnectSubnet{
							{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
						},
						Connectivity: []networkconnect.ConnectivityType{networkconnect.PodNetwork},
					},
				}

				kubeFakeClient := fake.NewSimpleClientset(ns1, ns2)
				fakeClient := util.GetOVNClientset(udn1, udn2, cnc)
				fakeClient.KubeClient = kubeFakeClient

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
				config.OVNKubernetesFeature.EnableNetworkConnect = true

				f, err = factory.NewClusterManagerWatchFactory(fakeClient.GetClusterManagerClientset())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				c, cancel := context.WithCancel(ctx.Context)
				defer cancel()
				clusterManager, err := NewClusterManager(fakeClient.GetClusterManagerClientset(), f, "identity", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(c)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer clusterManager.Stop()

				// Verify that cluster manager preserved the tunnel key annotation on the CNC
				gomega.Eventually(func() (int, error) {
					updatedCNC, err := fakeClient.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.TODO(), "test-cnc", metav1.GetOptions{})
					if err != nil {
						return 0, err
					}
					tunnelKeyStr := updatedCNC.Annotations[util.OvnConnectRouterTunnelKeyAnnotation]
					if tunnelKeyStr == "" {
						return 0, fmt.Errorf("tunnel key annotation not set")
					}
					return strconv.Atoi(tunnelKeyStr)
				}, 5).Should(gomega.Equal(16715781)) // Should preserve the pre-populated value

				// Wait for NADs to be created by UDN controller and get their network IDs
				var frontendNetworkID, backendNetworkID string
				gomega.Eventually(func() error {
					nad1, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("frontend-ns").Get(
						context.TODO(), "frontend-udn", metav1.GetOptions{})
					if err != nil {
						return err
					}
					nad2, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("backend-ns").Get(
						context.TODO(), "backend-udn", metav1.GetOptions{})
					if err != nil {
						return err
					}
					frontendNetworkID = nad1.Annotations[ovntypes.OvnNetworkIDAnnotation]
					backendNetworkID = nad2.Annotations[ovntypes.OvnNetworkIDAnnotation]
					if frontendNetworkID == "" || backendNetworkID == "" {
						return fmt.Errorf("network IDs not yet assigned")
					}
					return nil
				}, 10).Should(gomega.Succeed())

				// Verify that cluster manager allocated subnets for both networks
				// Use the actual network IDs from the NADs
				gomega.Eventually(func() (string, error) {
					updatedCNC, err := fakeClient.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.TODO(), "test-cnc", metav1.GetOptions{})
					if err != nil {
						return "", err
					}
					return updatedCNC.Annotations["k8s.ovn.org/network-connect-subnet"], nil
				}, 10).Should(gomega.SatisfyAll(
					// Should have subnet allocations for both networks using their actual network IDs
					gomega.ContainSubstring(fmt.Sprintf("layer3_%s", frontendNetworkID)),
					gomega.ContainSubstring(fmt.Sprintf("layer3_%s", backendNetworkID)),
					// Both subnets should be from the connect subnet range (192.168.0.0/16) with /24 prefix
					gomega.MatchRegexp(`"layer3_\d+":\{"ipv4":"192\.168\.\d+\.0/24"\}.*"layer3_\d+":\{"ipv4":"192\.168\.\d+\.0/24"\}`),
				))

				// Verify the tunnel key is preserved after CNC update (triggers re-reconciliation)
				// Update CNC with a label to trigger reconciliation
				updatedCNC, err := fakeClient.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
					context.TODO(), "test-cnc", metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				if updatedCNC.Labels == nil {
					updatedCNC.Labels = make(map[string]string)
				}
				updatedCNC.Labels["test-update"] = "trigger-reconcile"
				_, err = fakeClient.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
					context.TODO(), updatedCNC, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Verify tunnel key is still preserved after reconciliation
				gomega.Eventually(func() (int, error) {
					cnc, err := fakeClient.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.TODO(), "test-cnc", metav1.GetOptions{})
					if err != nil {
						return 0, err
					}
					tunnelKeyStr := cnc.Annotations[util.OvnConnectRouterTunnelKeyAnnotation]
					if tunnelKeyStr == "" {
						return 0, fmt.Errorf("tunnel key annotation not set")
					}
					return strconv.Atoi(tunnelKeyStr)
				}, 5).Should(gomega.Equal(16715781)) // Should still be the same pre-populated value

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("Node gateway router port IP allocations", func() {
		ginkgo.It("verify the node annotations", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node3",
						},
					},
				}
				kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				clusterManager, err := NewClusterManager(fakeClient, f, "identity", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer clusterManager.Stop()

				// Check that cluster manager has set the node-gateway-router-lrp-ifaddr annotation for each node.
				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						gwLRPAddrs, err := udn.GetGWRouterIPs(updatedNode, &util.DefaultNetInfo{})
						if err != nil {
							return err
						}

						gomega.Expect(gwLRPAddrs).NotTo(gomega.BeNil())
						gomega.Expect(gwLRPAddrs).To(gomega.HaveLen(2))
						return nil
					}).ShouldNot(gomega.HaveOccurred())
				}

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR + "," + clusterv6CIDR,
				"-k8s-service-cidr=10.96.0.0/16,fd00:10:96::/112",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Transit switch port IP allocations", func() {
		ginkgo.It("allocates transit switch port IPs", func() {
			config.ClusterManager.V4TransitSubnet = "100.89.0.0/16"
			config.ClusterManager.V6TransitSubnet = "fd99::/64"
			app.Action = func(ctx *cli.Context) error {
				nodes := []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node3",
						},
					},
				}
				kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				clusterManager, err := NewClusterManager(fakeClient, f, "identity", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer clusterManager.Stop()

				// Check that cluster manager has allocated id transit switch port ips for each node
				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						_, ok := updatedNode.Annotations[ovnTransitSwitchPortAddrAnnotation]
						if !ok {
							return fmt.Errorf("expected node annotation for node %s to have transit switch port ips allocated", n.Name)
						}

						transitSwitchIps, err := util.ParseNodeTransitSwitchPortAddrs(updatedNode)
						if err != nil {
							return fmt.Errorf("error parsing transit switch ip annotations for the node %s", n.Name)
						}

						if len(transitSwitchIps) < 1 {
							return fmt.Errorf("transit switch ips for node %s not allocated", n.Name)
						}

						_, transitSwitchV4Subnet, err := net.ParseCIDR(config.ClusterManager.V4TransitSubnet)
						if err != nil {
							return fmt.Errorf("could not parse IPv4 transit switch subnet %v", err)
						}

						_, transitSwitchV6Subnet, err := net.ParseCIDR(config.ClusterManager.V6TransitSubnet)
						if err != nil {
							return fmt.Errorf("could not parse IPv6 transit switch subnet %v", err)
						}

						for _, ipNet := range transitSwitchIps {
							if !transitSwitchV4Subnet.Contains(ipNet.IP) && utilnet.IsIPv4CIDR(ipNet) {
								return fmt.Errorf("IPv4 transit switch ips for node %s does not belong to expected subnet", n.Name)
							} else if !transitSwitchV6Subnet.Contains(ipNet.IP) && utilnet.IsIPv6CIDR(ipNet) {
								return fmt.Errorf("IPv6 transit switch ips for node %s does not belong to expected subnet", n.Name)
							}
						}
						return nil
					}).ShouldNot(gomega.HaveOccurred())
				}

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR + "," + clusterv6CIDR,
				"-k8s-service-cidr=10.96.0.0/16,fd00:10:96::/112",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reallocates missing transit switch port IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node3",
						},
					},
				}
				kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				clusterManager, err := NewClusterManager(fakeClient, f, "identity", nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer clusterManager.Stop()

				// Check that cluster manager has allocated id transit switch port ips for each node
				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						_, ok := updatedNode.Annotations[ovnTransitSwitchPortAddrAnnotation]
						if !ok {
							return fmt.Errorf("expected node annotation for node %s to have transit switch port ips allocated", n.Name)
						}

						transitSwitchIps, err := util.ParseNodeTransitSwitchPortAddrs(updatedNode)
						if err != nil {
							return fmt.Errorf("error parsing transit switch ip annotations for the node %s", n.Name)
						}

						if len(transitSwitchIps) < 1 {
							return fmt.Errorf("transit switch ips for node %s not allocated", n.Name)
						}

						// Wait for the other initial node annotation writer before testing reallocation.
						hostSubnets, err := util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
						if err != nil {
							return fmt.Errorf("error parsing host subnet annotation for node %s: %v", n.Name, err)
						}
						if len(hostSubnets) < 1 {
							return fmt.Errorf("host subnet for node %s not allocated", n.Name)
						}

						return nil
					}).ShouldNot(gomega.HaveOccurred())
				}

				// Clear the transit switch port ip annotation from node 1.
				node1, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), "node1", metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				nodeAnnotations := node1.Annotations
				nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, "node1")
				for k, v := range nodeAnnotations {
					gomega.Expect(nodeAnnotator.Set(k, v)).To(gomega.Succeed())
				}
				node1TransitSwitchIps := node1.Annotations[ovnTransitSwitchPortAddrAnnotation]
				nodeAnnotator.Delete(ovnTransitSwitchPortAddrAnnotation)
				err = nodeAnnotator.Run()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), "node1", metav1.GetOptions{})
					if err != nil {
						return err
					}

					updatedNode1TransitSwitchIps, ok := updatedNode.Annotations[ovnTransitSwitchPortAddrAnnotation]
					if !ok {
						return fmt.Errorf("expected node annotation for node node1 to have transit switch port ips allocated")
					}

					transitSwitchIps, err := util.ParseNodeTransitSwitchPortAddrs(updatedNode)
					if err != nil {
						return fmt.Errorf("error parsing transit switch ip annotations for the node node1")
					}

					if len(transitSwitchIps) < 1 {
						return fmt.Errorf("transit switch ips for node node1 not allocated")
					}
					if node1TransitSwitchIps != updatedNode1TransitSwitchIps {
						return fmt.Errorf("expected transit switch ips %q for node node1, got %q",
							node1TransitSwitchIps, updatedNode1TransitSwitchIps)
					}
					return nil
				}).ShouldNot(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("starting the cluster manager", func() {
		const networkName = "default"

		var fakeClient *util.OVNClusterManagerClientset

		ginkgo.BeforeEach(func() {
			fakeClient = util.GetOVNClientset().GetClusterManagerClientset()
		})

		ginkgo.When("the required features are not enabled", func() {
			ginkgo.It("does *not* automatically provision a NAD for the default network", func() {
				app.Action = func(ctx *cli.Context) error {
					_, err := config.InitConfig(ctx, nil, nil)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					f, err = factory.NewClusterManagerWatchFactory(fakeClient)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					clusterMngr, err := clusterManager(fakeClient, f)
					gomega.Expect(clusterMngr).NotTo(gomega.BeNil())
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(clusterMngr.Start(ctx.Context)).To(gomega.Succeed())

					_, err = fakeClient.NetworkAttchDefClient.
						K8sCniCncfIoV1().
						NetworkAttachmentDefinitions(config.Kubernetes.OVNConfigNamespace).
						Get(
							context.Background(),
							networkName,
							metav1.GetOptions{},
						)
					gomega.Expect(err).To(
						gomega.MatchError("network-attachment-definitions.k8s.cni.cncf.io \"default\" not found"),
					)

					return nil
				}
				gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
			})
		})

		ginkgo.When("the multi-network, network-segmentation, and preconfigured-udn-addresses features are enabled", func() {
			ginkgo.BeforeEach(func() {
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
				config.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses = true
			})

			ginkgo.It("automatically provisions a NAD for the default network", func() {
				app.Action = func(ctx *cli.Context) error {
					_, err := config.InitConfig(ctx, nil, nil)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					f, err = factory.NewClusterManagerWatchFactory(fakeClient)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					clusterMngr, err := clusterManager(fakeClient, f)
					gomega.Expect(clusterMngr).NotTo(gomega.BeNil())
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					c, cancel := context.WithCancel(ctx.Context)
					defer cancel()
					gomega.Expect(clusterMngr.Start(c)).To(gomega.Succeed())
					defer clusterMngr.Stop()

					nad, err := fakeClient.NetworkAttchDefClient.
						K8sCniCncfIoV1().
						NetworkAttachmentDefinitions(config.Kubernetes.OVNConfigNamespace).
						Get(
							context.Background(),
							networkName,
							metav1.GetOptions{},
						)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					const expectedNADContents = `{"cniVersion": "1.1.0", "name": "ovn-kubernetes", "type": "ovn-k8s-cni-overlay"}`
					gomega.Expect(nad.Spec.Config).To(gomega.Equal(expectedNADContents))

					return nil
				}
				gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
			})
		})
	})

	ginkgo.Context("EVPN VTEP and UDN integration", func() {
		ginkgo.It("should freeze NADs when VTEP goes into CIDROverlap after a second VTEP is created", func() {
			app.Action = func(ctx *cli.Context) error {
				// Node with VTEP annotation for vtep-a
				node := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
						Annotations: map[string]string{
							util.OVNNodeVTEPs: `{"vtep-a": {"ips": ["100.64.0.1"]}}`,
						},
					},
				}

				ns := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-ns",
					},
				}

				vtepA := &vtepv1.VTEP{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vtep-a",
					},
					Spec: vtepv1.VTEPSpec{
						CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
						Mode:  vtepv1.VTEPModeUnmanaged,
					},
				}

				cudn := &udnv1.ClusterUserDefinedNetwork{
					ObjectMeta: metav1.ObjectMeta{
						Name: "evpn-cudn",
					},
					Spec: udnv1.ClusterUserDefinedNetworkSpec{
						NamespaceSelector: metav1.LabelSelector{},
						Network: udnv1.NetworkSpec{
							Topology: udnv1.NetworkTopologyLayer3,
							Layer3: &udnv1.Layer3Config{
								Role: udnv1.NetworkRoleSecondary,
								Subnets: []udnv1.Layer3Subnet{
									{CIDR: "10.100.0.0/16"},
								},
							},
							Transport: udnv1.TransportOptionEVPN,
							EVPN: &udnv1.EVPNConfig{
								VTEP: "vtep-a",
								IPVRF: &udnv1.VRFConfig{
									VNI: 200,
								},
							},
						},
					},
				}

				kubeFakeClient := fake.NewSimpleClientset(node, ns)
				fakeClient := util.GetOVNClientset(vtepA, cudn)
				fakeClient.KubeClient = kubeFakeClient

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
				config.OVNKubernetesFeature.EnableRouteAdvertisements = true
				config.OVNKubernetesFeature.EnableEgressIP = true
				config.OVNKubernetesFeature.EnableEVPN = true
				config.Gateway.Mode = config.GatewayModeLocal

				f, err = factory.NewClusterManagerWatchFactory(fakeClient.GetClusterManagerClientset())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				c, cancel := context.WithCancel(ctx.Context)
				defer cancel()
				cm, err := NewClusterManager(fakeClient.GetClusterManagerClientset(), f, "identity", record.NewFakeRecorder(10))
				gomega.Expect(cm).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = cm.Start(c)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer cm.Stop()

				cmClient := fakeClient.GetClusterManagerClientset()

				// Step 1: VTEP-A should become Accepted=True
				expectVTEPCondition(cmClient, "vtep-a", metav1.Condition{
					Type:   "Accepted",
					Status: metav1.ConditionTrue,
				})

				// Step 2: CUDN should have NAD created (NetworkCreated=True)
				var networkCreatedTransitionTime metav1.Time
				gomega.Eventually(func() bool {
					cudnObj, err := cmClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), "evpn-cudn", metav1.GetOptions{})
					if err != nil {
						return false
					}
					cond := meta.FindStatusCondition(cudnObj.Status.Conditions, "NetworkCreated")
					if cond != nil && cond.Status == metav1.ConditionTrue {
						networkCreatedTransitionTime = cond.LastTransitionTime
						return true
					}
					return false
				}, 10).Should(gomega.BeTrue(), "CUDN should have NetworkCreated=True")

				// TransportAccepted should report RA missing (VTEP passes but no RA)
				expectCUDNTransportCondition(cmClient, "evpn-cudn", metav1.Condition{
					Type:    udncontroller.ConditionTypeTransportAccepted,
					Status:  metav1.ConditionFalse,
					Reason:  udncontroller.ReasonEVPNRouteAdvertisementsIsMissing,
					Message: "No RouteAdvertisements CR is advertising the pod networks.",
				})

				// NAD should exist
				gomega.Eventually(func() error {
					_, err := cmClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("test-ns").Get(context.Background(), "evpn-cudn", metav1.GetOptions{})
					return err
				}, 10).Should(gomega.Succeed(), "NAD should be created")

				// Step 3: Create VTEP-B with overlapping CIDRs — this will cause
				// the VTEP controller to set both VTEPs to Accepted=False
				vtepB := &vtepv1.VTEP{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vtep-b",
					},
					Spec: vtepv1.VTEPSpec{
						CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
						Mode:  vtepv1.VTEPModeUnmanaged,
					},
				}
				_, err = cmClient.VTEPClient.K8sV1().VTEPs().Create(context.Background(), vtepB, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Step 4: VTEP-A should transition to Accepted=False (CIDROverlap)
				expectVTEPCondition(cmClient, "vtep-a", metav1.Condition{
					Type:   "Accepted",
					Status: metav1.ConditionFalse,
					Reason: "CIDROverlap",
				})

				// Step 5: NAD should still exist and NetworkCreated should remain True
				// (frozen, not torn down). Also verify LastTransitionTime hasn't changed.
				gomega.Consistently(func() bool {
					cudnObj, err := cmClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), "evpn-cudn", metav1.GetOptions{})
					if err != nil {
						return false
					}
					cond := meta.FindStatusCondition(cudnObj.Status.Conditions, "NetworkCreated")
					return cond != nil && cond.Status == metav1.ConditionTrue &&
						cond.LastTransitionTime.Equal(&networkCreatedTransitionTime)
				}, 3).Should(gomega.BeTrue(), "NetworkCreated should remain True with unchanged LastTransitionTime during CIDROverlap")

				// TransportAccepted should report VTEPNotAccepted
				expectCUDNTransportCondition(cmClient, "evpn-cudn", metav1.Condition{
					Type:    udncontroller.ConditionTypeTransportAccepted,
					Status:  metav1.ConditionFalse,
					Reason:  udncontroller.ReasonVTEPNotAccepted,
					Message: `VTEP "vtep-a" exists but is not accepted.`,
				})

				_, err = cmClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("test-ns").Get(context.Background(), "evpn-cudn", metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "NAD should still exist during CIDROverlap")

				// Step 6: Delete VTEP-B to resolve the overlap
				err = cmClient.VTEPClient.K8sV1().VTEPs().Delete(context.Background(), "vtep-b", metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Step 7: VTEP-A should go back to Accepted=True
				expectVTEPCondition(cmClient, "vtep-a", metav1.Condition{
					Type:   "Accepted",
					Status: metav1.ConditionTrue,
				})

				// TransportAccepted should transition back to RA missing (VTEP passes again)
				expectCUDNTransportCondition(cmClient, "evpn-cudn", metav1.Condition{
					Type:    udncontroller.ConditionTypeTransportAccepted,
					Status:  metav1.ConditionFalse,
					Reason:  udncontroller.ReasonEVPNRouteAdvertisementsIsMissing,
					Message: "No RouteAdvertisements CR is advertising the pod networks.",
				})

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not create NAD when VTEP is in failed state", func() {
			app.Action = func(ctx *cli.Context) error {
				// Node WITHOUT VTEP annotation — the VTEP controller will
				// set AllocationFailed because there's no IP for this VTEP.
				node := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
					},
				}

				ns := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-ns",
					},
				}

				vtep := &vtepv1.VTEP{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vtep-failed",
					},
					Spec: vtepv1.VTEPSpec{
						CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
						Mode:  vtepv1.VTEPModeUnmanaged,
					},
				}

				cudn := &udnv1.ClusterUserDefinedNetwork{
					ObjectMeta: metav1.ObjectMeta{
						Name: "evpn-cudn-blocked",
					},
					Spec: udnv1.ClusterUserDefinedNetworkSpec{
						NamespaceSelector: metav1.LabelSelector{},
						Network: udnv1.NetworkSpec{
							Topology: udnv1.NetworkTopologyLayer3,
							Layer3: &udnv1.Layer3Config{
								Role: udnv1.NetworkRoleSecondary,
								Subnets: []udnv1.Layer3Subnet{
									{CIDR: "10.200.0.0/16"},
								},
							},
							Transport: udnv1.TransportOptionEVPN,
							EVPN: &udnv1.EVPNConfig{
								VTEP: "vtep-failed",
								IPVRF: &udnv1.VRFConfig{
									VNI: 300,
								},
							},
						},
					},
				}

				kubeFakeClient := fake.NewSimpleClientset(node, ns)
				fakeClient := util.GetOVNClientset(vtep, cudn)
				fakeClient.KubeClient = kubeFakeClient

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
				config.OVNKubernetesFeature.EnableRouteAdvertisements = true
				config.OVNKubernetesFeature.EnableEgressIP = true
				config.OVNKubernetesFeature.EnableEVPN = true
				config.Gateway.Mode = config.GatewayModeLocal

				f, err = factory.NewClusterManagerWatchFactory(fakeClient.GetClusterManagerClientset())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				c, cancel := context.WithCancel(ctx.Context)
				defer cancel()
				cm, err := NewClusterManager(fakeClient.GetClusterManagerClientset(), f, "identity", record.NewFakeRecorder(10))
				gomega.Expect(cm).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = cm.Start(c)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer cm.Stop()

				cmClient := fakeClient.GetClusterManagerClientset()

				// Step 1: VTEP controller should set Accepted=False (AllocationFailed)
				// because the node has no VTEP annotation.
				expectVTEPCondition(cmClient, "vtep-failed", metav1.Condition{
					Type:   "Accepted",
					Status: metav1.ConditionFalse,
					Reason: "AllocationFailed",
				})

				// Step 2: CUDN should report VTEPNotAccepted — NAD must NOT be created.
				gomega.Eventually(func() string {
					cudnObj, err := cmClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), "evpn-cudn-blocked", metav1.GetOptions{})
					if err != nil {
						return ""
					}
					cond := meta.FindStatusCondition(cudnObj.Status.Conditions, "NetworkCreated")
					if cond == nil {
						return ""
					}
					return cond.Reason
				}, 10).Should(gomega.Equal("VTEPNotAccepted"), "CUDN should report VTEPNotAccepted")

				// TransportAccepted should also report VTEPNotAccepted
				expectCUDNTransportCondition(cmClient, "evpn-cudn-blocked", metav1.Condition{
					Type:    udncontroller.ConditionTypeTransportAccepted,
					Status:  metav1.ConditionFalse,
					Reason:  udncontroller.ReasonVTEPNotAccepted,
					Message: `VTEP "vtep-failed" exists but is not accepted.`,
				})

				// NAD should NOT exist
				gomega.Consistently(func() bool {
					_, err := cmClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("test-ns").Get(context.Background(), "evpn-cudn-blocked", metav1.GetOptions{})
					return err != nil
				}, 3).Should(gomega.BeTrue(), "NAD should not be created when VTEP is not accepted")

				// Step 3: Fix the node — add VTEP annotation so VTEP becomes Accepted
				node, err = kubeFakeClient.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				node.Annotations = map[string]string{
					util.OVNNodeVTEPs: `{"vtep-failed": {"ips": ["100.64.0.1"]}}`,
				}
				_, err = kubeFakeClient.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Step 4: VTEP should become Accepted=True
				expectVTEPCondition(cmClient, "vtep-failed", metav1.Condition{
					Type:   "Accepted",
					Status: metav1.ConditionTrue,
				})

				// Step 5: CUDN should now have NetworkCreated=True and NAD should exist
				gomega.Eventually(func() bool {
					cudnObj, err := cmClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), "evpn-cudn-blocked", metav1.GetOptions{})
					if err != nil {
						return false
					}
					cond := meta.FindStatusCondition(cudnObj.Status.Conditions, "NetworkCreated")
					return cond != nil && cond.Status == metav1.ConditionTrue
				}, 10).Should(gomega.BeTrue(), "CUDN should have NetworkCreated=True after VTEP is accepted")

				// TransportAccepted should transition to RA missing (VTEP now passes but no RA exists)
				expectCUDNTransportCondition(cmClient, "evpn-cudn-blocked", metav1.Condition{
					Type:    udncontroller.ConditionTypeTransportAccepted,
					Status:  metav1.ConditionFalse,
					Reason:  udncontroller.ReasonEVPNRouteAdvertisementsIsMissing,
					Message: "No RouteAdvertisements CR is advertising the pod networks.",
				})

				gomega.Eventually(func() error {
					_, err := cmClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("test-ns").Get(context.Background(), "evpn-cudn-blocked", metav1.GetOptions{})
					return err
				}, 10).Should(gomega.Succeed(), "NAD should be created after VTEP becomes accepted")

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

})

func expectCUDNTransportCondition(cmClient *util.OVNClusterManagerClientset, cudnName string, expected metav1.Condition) {
	gomega.Eventually(func(g gomega.Gomega) {
		cudnObj, err := cmClient.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		cond := meta.FindStatusCondition(cudnObj.Status.Conditions, expected.Type)
		g.Expect(cond).NotTo(gomega.BeNil())
		g.Expect(cond.Status).To(gomega.Equal(expected.Status))
		if expected.Reason != "" {
			g.Expect(cond.Reason).To(gomega.Equal(expected.Reason))
		}
		if expected.Message != "" {
			g.Expect(cond.Message).To(gomega.Equal(expected.Message))
		}
	}, 10).Should(gomega.Succeed())
}

func expectVTEPCondition(cmClient *util.OVNClusterManagerClientset, vtepName string, expected metav1.Condition) {
	gomega.Eventually(func(g gomega.Gomega) {
		vtep, err := cmClient.VTEPClient.K8sV1().VTEPs().Get(context.Background(), vtepName, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		cond := meta.FindStatusCondition(vtep.Status.Conditions, expected.Type)
		g.Expect(cond).NotTo(gomega.BeNil())
		g.Expect(cond.Status).To(gomega.Equal(expected.Status))
		if expected.Reason != "" {
			g.Expect(cond.Reason).To(gomega.Equal(expected.Reason))
		}
		if expected.Message != "" {
			g.Expect(cond.Message).To(gomega.Equal(expected.Message))
		}
	}, 10).Should(gomega.Succeed())
}

func clusterManager(client *util.OVNClusterManagerClientset, f *factory.WatchFactory) (*ClusterManager, error) {
	if err := f.Start(); err != nil {
		return nil, fmt.Errorf("failed to start the CM watch factory: %w", err)
	}

	clusterMngr, err := NewClusterManager(client, f, "identity", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to start the CM watch factory: %w", err)
	}

	return clusterMngr, nil
}

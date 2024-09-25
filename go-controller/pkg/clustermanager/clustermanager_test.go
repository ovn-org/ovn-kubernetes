package clustermanager

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	utilnet "k8s.io/utils/net"
)

const (
	// ovnNodeIDAnnotaton is the node annotation name used to store the node id.
	ovnNodeIDAnnotaton = "k8s.ovn.org/node-id"

	// ovnTransitSwitchPortAddrAnnotation is the node annotation name to store the transit switch port ips.
	ovnTransitSwitchPortAddrAnnotation = "k8s.ovn.org/node-transit-switch-port-ifaddr"
)

var _ = ginkgo.Describe("Cluster Manager", func() {
	var (
		app      *cli.App
		f        *factory.WatchFactory
		stopChan chan struct{}
		wg       *sync.WaitGroup
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
		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}
	})

	ginkgo.AfterEach(func() {
		close(stopChan)
		if f != nil {
			f.Shutdown()
		}
		wg.Wait()
	})

	ginkgo.Context("Node subnet allocations", func() {
		ginkgo.It("Linux nodes", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
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
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				c, cancel := context.WithCancel(ctx.Context)
				defer cancel()
				clusterManager, err := NewClusterManager(fakeClient, f, "identity", wg, nil)
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

				// Check that the network id 0 is allocated for the default network
				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						networkId, err := util.ParseNetworkIDAnnotation(updatedNode, "default")
						if err != nil {
							return fmt.Errorf("expected node network id annotation for node %s to have been allocated", n.Name)
						}

						gomega.Expect(networkId).To(gomega.Equal(0))
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

		ginkgo.It("Linux nodes - clear subnet annotations", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
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
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				c, cancel := context.WithCancel(ctx.Context)
				defer cancel()
				clusterManager, err := NewClusterManager(fakeClient, f, "identity", wg, nil)
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
					nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient}, n.Name)
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
				nodes := []v1.Node{
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
							Labels: map[string]string{v1.LabelOSStable: "windows"},
						},
					}}
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				c, cancel := context.WithCancel(ctx.Context)
				defer cancel()
				clusterManager, err := NewClusterManager(fakeClient, f, "identity", wg, nil)
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
				nodes := []v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "winnode1",
							Labels: map[string]string{v1.LabelOSStable: "windows"},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "winnode2",
							Labels: map[string]string{v1.LabelOSStable: "windows"},
						},
					},
				}
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				c, cancel := context.WithCancel(ctx.Context)
				defer cancel()
				clusterManager, err := NewClusterManager(fakeClient, f, "identity", wg, nil)
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
					nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient}, n.Name)

					nodeAnnotations := n.Annotations
					for k, v := range nodeAnnotations {
						nodeAnnotator.Set(k, v)
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
	})

	ginkgo.Context("Node Id allocations", func() {
		ginkgo.It("check for node id allocations", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
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
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				clusterManager, err := NewClusterManager(fakeClient, f, "identity", wg, nil)
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
				nodes := []v1.Node{
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
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				clusterManager, err := NewClusterManager(fakeClient, f, "identity", wg, nil)
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
					nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient}, n.Name)

					nodeAnnotations := n.Annotations
					for k, v := range nodeAnnotations {
						nodeAnnotator.Set(k, v)
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
				nodes := []v1.Node{
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
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				wg1 := &sync.WaitGroup{}
				clusterManager, err := NewClusterManager(fakeClient, f, "cm1", wg1, nil)
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

				updatedNodes := []v1.Node{}
				for _, n := range nodes {
					updatedNode, _ := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
					updatedNodes = append(updatedNodes, *updatedNode)
				}
				// stop the cluster manager and start a new instance and make sure the node ids are same.
				clusterManager.Stop()
				wg1.Wait()

				// Close the watch factory and create a new one
				f.Shutdown()
				kubeFakeClient = fake.NewSimpleClientset(&v1.NodeList{
					Items: updatedNodes,
				})
				fakeClient = &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}
				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				cm2, err := NewClusterManager(fakeClient, f, "cm2", wg, nil)
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
				nodes := []v1.Node{
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
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				wg1 := &sync.WaitGroup{}
				clusterManager, err := NewClusterManager(fakeClient, f, "cm1", wg1, nil)
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

				updatedNodes := []v1.Node{}
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
				kubeFakeClient = fake.NewSimpleClientset(&v1.NodeList{
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
				cm2, err := NewClusterManager(fakeClient, f, "cm2", wg, nil)
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

	ginkgo.Context("Node gateway router port IP allocations", func() {
		ginkgo.It("verify the node annotations", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
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
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				clusterManager, err := NewClusterManager(fakeClient, f, "identity", wg, nil)
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

						gwLRPAddrs, err := util.ParseNodeGatewayRouterJoinAddrs(updatedNode, types.DefaultNetworkName)
						if err != nil {
							return err
						}

						gomega.Expect(gwLRPAddrs).NotTo(gomega.BeNil())
						gomega.Expect(len(gwLRPAddrs)).To(gomega.Equal(2))
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

		ginkgo.It("clear the node annotations for gateway router port ips and check", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
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
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				clusterManager, err := NewClusterManager(fakeClient, f, "identity", wg, nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer clusterManager.Stop()

				nodeAddrs := make(map[string]string)
				// Check that cluster manager has set the node-gateway-router-lrp-ifaddr annotation for each node.
				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						gwLRPAddrs, err := util.ParseNodeGatewayRouterJoinAddrs(updatedNode, types.DefaultNetworkName)
						if err != nil {
							return err
						}
						gomega.Expect(gwLRPAddrs).NotTo(gomega.BeNil())
						gomega.Expect(len(gwLRPAddrs)).To(gomega.Equal(2))
						nodeAddrs[n.Name] = updatedNode.Annotations[util.OVNNodeGRLRPAddrs]
						return nil
					}).ShouldNot(gomega.HaveOccurred())
				}

				// Clear the node-gateway-router-lrp-ifaddr annotation of nodes and make sure it is reset by cluster manager
				// with the same addrs.
				for _, n := range nodes {
					nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient}, n.Name)

					nodeAnnotations := n.Annotations
					for k, v := range nodeAnnotations {
						nodeAnnotator.Set(k, v)
					}
					nodeAnnotator.Delete(util.OVNNodeGRLRPAddrs)
					err = nodeAnnotator.Run()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}

				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						nodeGWRPIPs, ok := updatedNode.Annotations[util.OVNNodeGRLRPAddrs]
						if !ok {
							return fmt.Errorf("expected node annotation for node %s to have node gateway-router-lrp-ifaddr allocated", n.Name)
						}

						gomega.Expect(nodeGWRPIPs).To(gomega.Equal(nodeAddrs[n.Name]))
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

		ginkgo.It("Stop cluster manager, change id of a node and verify the gateway router port addr node annotation", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
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
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				wg1 := &sync.WaitGroup{}
				clusterManager, err := NewClusterManager(fakeClient, f, "identity", wg1, nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clusterManager.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				node3GWRPAnnotation := ""
				// Check that cluster manager has set the node-gateway-router-lrp-ifaddr annotation for each node.
				for _, n := range nodes {
					gomega.Eventually(func() error {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}

						gwLRPAddrs, err := util.ParseNodeGatewayRouterJoinAddrs(updatedNode, types.DefaultNetworkName)
						if err != nil {
							return err
						}
						gomega.Expect(gwLRPAddrs).NotTo(gomega.BeNil())
						gomega.Expect(len(gwLRPAddrs)).To(gomega.Equal(2))

						// Store the node 3's gw router port addresses
						if updatedNode.Name == "node3" {
							node3GWRPAnnotation = updatedNode.Annotations[util.OVNNodeGRLRPAddrs]
						}
						return nil
					}).ShouldNot(gomega.HaveOccurred())
				}

				// stop the cluster manager.
				clusterManager.Stop()
				wg1.Wait()

				updatedNodes := []v1.Node{}

				for _, n := range nodes {
					updatedNode, _ := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
					if updatedNode.Name == "node3" {
						// Change the id of node3 duplicate.
						updatedNode.Annotations[ovnNodeIDAnnotaton] = "50"
					}
					updatedNodes = append(updatedNodes, *updatedNode)
				}

				// Close the watch factory and create a new one
				f.Shutdown()
				kubeFakeClient = fake.NewSimpleClientset(&v1.NodeList{
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
				cm2, err := NewClusterManager(fakeClient, f, "cm2", wg, nil)
				gomega.Expect(cm2).NotTo(gomega.BeNil())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = cm2.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer cm2.Stop()

				gomega.Eventually(func() error {
					updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), "node3", metav1.GetOptions{})
					if err != nil {
						return err
					}

					node3UpdatedGWRPAnnotation := updatedNode.Annotations[util.OVNNodeGRLRPAddrs]
					gomega.Expect(node3UpdatedGWRPAnnotation).NotTo(gomega.Equal(node3GWRPAnnotation))

					gwLRPAddrs, err := util.ParseNodeGatewayRouterJoinAddrs(updatedNode, types.DefaultNetworkName)
					if err != nil {
						return err
					}
					gomega.Expect(gwLRPAddrs).NotTo(gomega.BeNil())
					gomega.Expect(len(gwLRPAddrs)).To(gomega.Equal(2))
					return nil
				}).ShouldNot(gomega.HaveOccurred())
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
		ginkgo.It("Interconnect enabled", func() {
			config.ClusterManager.V4TransitSwitchSubnet = "100.89.0.0/16"
			config.ClusterManager.V6TransitSwitchSubnet = "fd99::/64"
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
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
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				clusterManager, err := NewClusterManager(fakeClient, f, "identity", wg, nil)
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

						_, transitSwitchV4Subnet, err := net.ParseCIDR(config.ClusterManager.V4TransitSwitchSubnet)
						if err != nil {
							return fmt.Errorf("could not parse IPv4 transit switch subnet %v", err)
						}

						_, transitSwitchV6Subnet, err := net.ParseCIDR(config.ClusterManager.V6TransitSwitchSubnet)
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
				"--enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Interconnect enabled - clear the transit switch port ips and check", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
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
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				clusterManager, err := NewClusterManager(fakeClient, f, "identity", wg, nil)
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

						return nil
					}).ShouldNot(gomega.HaveOccurred())
				}

				// Clear the transit switch port ip annotation from node 1.
				node1, _ := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), "node1", metav1.GetOptions{})
				nodeAnnotations := node1.Annotations
				nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient}, "node1")
				for k, v := range nodeAnnotations {
					nodeAnnotator.Set(k, v)
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
					gomega.Expect(node1TransitSwitchIps).To(gomega.Equal(updatedNode1TransitSwitchIps))
					return nil
				}).ShouldNot(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"--enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Interconnect disabled", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
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
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				clusterManager, err := NewClusterManager(fakeClient, f, "identity", wg, nil)
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
						if ok {
							return fmt.Errorf("not expected node annotation for node %s to have transit switch port ips allocated", n.Name)
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
	})

})

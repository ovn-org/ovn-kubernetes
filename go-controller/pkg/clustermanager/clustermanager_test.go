package clustermanager

import (
	"context"
	"net"
	"sync"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
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
				clusterManager := NewClusterManager(fakeClient, f, "identity", wg, nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
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
				clusterManager := NewClusterManager(fakeClient, f, "identity", wg, nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
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
				clusterManager := NewClusterManager(fakeClient, f, "identity", wg, nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
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
				clusterManager := NewClusterManager(fakeClient, f, "identity", wg, nil)
				gomega.Expect(clusterManager).NotTo(gomega.BeNil())
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
})

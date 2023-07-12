package clustermanager

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("Secondary Layer3 Cluster Controller Manager", func() {
	var (
		app      *cli.App
		f        *factory.WatchFactory
		stopChan chan struct{}
		wg       *sync.WaitGroup
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

	ginkgo.Context("Secondary networks", func() {
		ginkgo.It("Attach secondary layer3 network", func() {
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

				sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, record.NewFakeRecorder(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: types.NetConf{Name: "blue"}, Topology: ovntypes.Layer3Topology, Subnets: "192.168.0.0/16/24"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				nc, err := sncm.NewNetworkController(netInfo)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(nc).NotTo(gomega.BeNil())
				nc.Start(ctx.Context)
				defer nc.Stop()

				// Check that network controller for "blue" network has set the subnet annotation for each node.
				for _, n := range nodes {
					gomega.Eventually(func() ([]*net.IPNet, error) {
						updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
						if err != nil {
							return nil, err
						}

						return util.ParseNodeHostSubnetAnnotation(updatedNode, "blue")
					}, 2).Should(gomega.HaveLen(1))
				}

				return nil
			}

			err := app.Run([]string{
				app.Name,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Attach secondary layer2 network", func() {
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

				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableInterconnect = true
				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, record.NewFakeRecorder(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: types.NetConf{Name: "blue"}, Topology: ovntypes.Layer2Topology})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				config.OVNKubernetesFeature.EnableInterconnect = false
				nc, err := sncm.NewNetworkController(netInfo)
				gomega.Expect(err).To(gomega.Equal(nad.ErrNetworkControllerTopologyNotManaged))
				gomega.Expect(nc).To(gomega.BeNil())

				config.OVNKubernetesFeature.EnableInterconnect = true
				nc, err = sncm.NewNetworkController(netInfo)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(nc).NotTo(gomega.BeNil())

				err = nc.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{
				app.Name,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Cleanup", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
							Annotations: map[string]string{
								"k8s.ovn.org/node-subnets": "{\"default\":[\"10.244.0.0/24\"],\"blue\":[\"192.168.0.0/24\"],\"red\":[\"192.169.0.0/24\"]}",
								"k8s.ovn.org/network-ids":  "{\"default\":\"0\",\"blue\":\"1\",\"red\":\"2\"}",
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node2",
							Annotations: map[string]string{
								"k8s.ovn.org/node-subnets": "{\"default\":[\"10.244.1.0/24\"],\"blue\":[\"192.168.1.0/24\"],\"red\":[\"192.169.1.0/24\"]}",
								"k8s.ovn.org/network-ids":  "{\"default\":\"0\",\"blue\":\"1\",\"red\":\"2\"}",
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node3",
							Annotations: map[string]string{
								"k8s.ovn.org/node-subnets": "{\"default\":[\"10.244.2.0/24\"],\"blue\":[\"192.168.2.0/24\"],\"red\":[\"192.169.2.0/24\"]}",
								"k8s.ovn.org/network-ids":  "{\"default\":\"0\",\"blue\":\"1\",\"red\":\"2\"}",
							},
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

				var expectBlueCleanup, expectRedCleanup bool
				checkNodeAnnotations := func() error {
					for _, n := range nodes {
						updatedNode, err := f.GetNode(n.Name)
						if err != nil {
							return err
						}

						_, err = util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
						if err != nil {
							return err
						}
						_, err = util.ParseNetworkIDAnnotation(updatedNode, ovntypes.DefaultNetworkName)
						if err != nil {
							return err
						}

						_, err = util.ParseNodeHostSubnetAnnotation(updatedNode, "blue")
						if err == nil && expectBlueCleanup {
							return fmt.Errorf("unexpected subnet annotation presence for network blue on node %s: expected %v got %v", updatedNode.Name, !expectBlueCleanup, err == nil)
						}

						_, err = util.ParseNetworkIDAnnotation(updatedNode, "blue")
						if err == nil && expectBlueCleanup {
							return fmt.Errorf("unexpected network ID annotation presence for network blue on node %s: expected %v got %v", updatedNode.Name, !expectBlueCleanup, err == nil)
						}

						_, err = util.ParseNodeHostSubnetAnnotation(updatedNode, "red")
						if err == nil && expectRedCleanup {
							return fmt.Errorf("unexpected subnet annotation presence for network red on node %s: expected %v got %v", updatedNode.Name, !expectRedCleanup, err == nil)
						}

						_, err = util.ParseNetworkIDAnnotation(updatedNode, "red")
						if err == nil && expectRedCleanup {
							return fmt.Errorf("unexpected network ID annotation presence for network red on node %s: expected %v got %v", updatedNode.Name, !expectRedCleanup, err == nil)
						}
					}
					return nil
				}

				gomega.Eventually(checkNodeAnnotations).ShouldNot(gomega.HaveOccurred())

				sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, record.NewFakeRecorder(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = sncm.init()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create a fake nad controller for blue network so that the red network gets cleared
				// when CleanupDeletedNetworks is called.  If we don't pass any controller to
				// CleanupDeletedNetworks it will try to cleanup both blue and red networks and
				// there could be a race in updating the node annotations with the fakeclient.
				// fakeclient will not return an error in such cases to trigger retry by RetryOnConflict.
				// So testing the cleanup one at a time.
				netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: types.NetConf{Name: "blue"}, Topology: ovntypes.Layer3Topology})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				namedIDAllocator := sncm.networkIDAllocator.ForName(netInfo.GetNetworkName())
				oc := newNetworkClusterController(namedIDAllocator, netInfo, sncm.ovnClient, sncm.watchFactory)
				err = oc.init()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				nadControllers := []nad.NetworkController{oc}
				err = sncm.CleanupDeletedNetworks(nadControllers)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Clean up the red network
				expectBlueCleanup = false
				expectRedCleanup = true
				gomega.Eventually(checkNodeAnnotations).ShouldNot(gomega.HaveOccurred())
				err = sncm.networkIDAllocator.ReserveID("was_red_network_id_released", 2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Now call CleanupDeletedNetworks() with empty nad controllers.
				// Blue network should also be cleared.
				err = sncm.CleanupDeletedNetworks([]nad.NetworkController{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectBlueCleanup = true
				expectRedCleanup = true
				gomega.Eventually(checkNodeAnnotations).ShouldNot(gomega.HaveOccurred())
				err = sncm.networkIDAllocator.ReserveID("was_blue_network_id_released", 1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{
				app.Name,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})

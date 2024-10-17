package clustermanager

import (
	"context"
	"net"
	"sync"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("Network Cluster Controller", func() {
	var (
		app      *cli.App
		f        *factory.WatchFactory
		recorder record.EventRecorder
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
		recorder = record.NewFakeRecorder(10)
	})

	ginkgo.AfterEach(func() {
		close(stopChan)
		if f != nil {
			f.Shutdown()
		}
		wg.Wait()
	})

	ginkgo.Context("Host Subnets", func() {
		ginkgo.It("removes an unused dual-stack allocation from single-stack cluster", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
							Annotations: map[string]string{
								ovnNodeIDAnnotaton:         "3",
								"k8s.ovn.org/node-subnets": "{\"default\":[\"10.128.0.0/24\", \"fd02:0:0:2::2895/64\"]}",
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

				ncc := newDefaultNetworkClusterController(&util.DefaultNetInfo{}, fakeClient, f, recorder)
				ncc.Start(ctx.Context)
				defer ncc.Stop()

				// Check that the default network controller removes the unused v6 node subnet annotation
				gomega.Eventually(func() ([]*net.IPNet, error) {
					updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodes[0].Name, metav1.GetOptions{})
					if err != nil {
						return nil, err
					}

					return util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
				}, 2).Should(gomega.Equal(ovntest.MustParseIPNets("10.128.0.0/24")))

				return nil
			}

			err := app.Run([]string{
				app.Name,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("allocates a subnet for a new node", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
							Annotations: map[string]string{
								ovnNodeIDAnnotaton: "3",
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

				ncc := newDefaultNetworkClusterController(&util.DefaultNetInfo{}, fakeClient, f, recorder)
				ncc.Start(ctx.Context)
				defer ncc.Stop()

				// Check that the default network controller adds a subnet for the new node
				gomega.Eventually(func() ([]*net.IPNet, error) {
					updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodes[0].Name, metav1.GetOptions{})
					if err != nil {
						return nil, err
					}

					return util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
				}, 2).Should(gomega.Equal(ovntest.MustParseIPNets("10.128.0.0/23")))

				return nil
			}

			err := app.Run([]string{
				app.Name,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("removes an invalid single-stack annotation", func() {
			app.Action = func(ctx *cli.Context) error {
				nodes := []v1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "node1",
							Annotations: map[string]string{
								"k8s.ovn.org/node-subnets": "{\"default\":[\"10.128.0.0/24\", \"1.2.3.0/24\"]}",
								ovnNodeIDAnnotaton:         "3",
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

				ncc := newDefaultNetworkClusterController(&util.DefaultNetInfo{}, fakeClient, f, recorder)
				ncc.Start(ctx.Context)
				defer ncc.Stop()

				// Check that the default network controller removes the unused v6 node subnet annotation
				gomega.Eventually(func() ([]*net.IPNet, error) {
					updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodes[0].Name, metav1.GetOptions{})
					if err != nil {
						return nil, err
					}

					return util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
				}, 2).Should(gomega.Equal(ovntest.MustParseIPNets("10.128.0.0/24")))

				return nil
			}

			err := app.Run([]string{
				app.Name,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})

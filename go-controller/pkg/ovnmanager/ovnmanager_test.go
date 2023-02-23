package ovnmanager

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

var _ = ginkgo.Describe("Ovn Manager", func() {
	var (
		app      *cli.App
		f        *factory.WatchFactory
		stopChan chan struct{}
		wg       *sync.WaitGroup
		fexec    *ovntest.FakeExec
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
		fexec = ovntest.NewLooseCompareFakeExec()
		err := util.SetExec(fexec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.AfterEach(func() {
		close(stopChan)
		if f != nil {
			f.Shutdown()
		}
		wg.Wait()
	})

	ginkgo.Context("Ovn manager -  master", func() {
		ginkgo.It("Single master", func() {
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
				configmaps := []v1.ConfigMap{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "control-plane-status",
							Namespace: config.Kubernetes.OVNConfigNamespace,
						},
					},
				}
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
					Items: nodes,
				},
					&v1.ConfigMapList{
						Items: configmaps,
					})
				fakeClient := &util.OVNClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				nbGlobal := &nbdb.NBGlobal{
					UUID: "nb-global-uuid",
				}
				initialData := []libovsdbtest.TestData{
					nbGlobal,
				}
				dbSetup := libovsdbtest.TestSetup{
					NBData: initialData,
				}

				nbClient, sbClient, _, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				config.OvnNorth.Address = nbClient.CurrentEndpoint()
				config.OvnSouth.Address = sbClient.CurrentEndpoint()

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    fmt.Sprintf("ovsdb-client list-columns %s --data=bare --no-heading --format=json OVN_Northbound Load_Balancer", nbClient.CurrentEndpoint()),
					Output: `{"data":[["_version","uuid"],["health_check",{"key":{"refTable":"Load_Balancer_Health_Check","type":"uuid"},"max":"unlimited","min":0}],["name","string"],["protocol",{"key":{"enum":["set",["tcp","udp"]],"type":"string"},"min":0}]],"headings":["Column","Type"]}`,
				})

				// ovn-nbctl --timeout=15 --columns=_uuid list Load_Balancer_Group
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --columns=_uuid list Load_Balancer_Group",
					Output: ``,
				})

				ovnManager, err := NewOvnManager(ManagerTypeMaster, "master", fakeClient, wg, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ovnManager).NotTo(gomega.BeNil())

				err = ovnManager.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer ovnManager.Stop()

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

				// Check that network controller manager has created logical switches for the nodes
				for _, n := range nodes {
					gomega.Eventually(func() error {
						nodeLogicalSwitch := &nbdb.LogicalSwitch{
							Name: n.Name,
						}
						_, err := libovsdbops.GetLogicalSwitch(nbClient, nodeLogicalSwitch)
						return err
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

	ginkgo.Context("Ovn manager - Cluster manager", func() {
		ginkgo.It("Single cluster manager", func() {
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
				fakeClient := &util.OVNClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				ovnManager, err := NewOvnManager(ManagerTypeClusterManager, "cm", fakeClient, wg, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ovnManager).NotTo(gomega.BeNil())

				err = ovnManager.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer ovnManager.Stop()

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

		ginkgo.It("Multiple cluster managers", func() {
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
				fakeClient := &util.OVNClientset{
					KubeClient: kubeFakeClient,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				config.ClusterMgrHA.ElectionLeaseDuration = 3
				config.ClusterMgrHA.ElectionRenewDeadline = 2
				config.ClusterMgrHA.ElectionRetryPeriod = 1

				ovnManager1, err := NewOvnManager(ManagerTypeClusterManager, "cm1", fakeClient, wg, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ovnManager1).NotTo(gomega.BeNil())

				err = ovnManager1.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ovnManager2, err := NewOvnManager(ManagerTypeClusterManager, "cm2", fakeClient, wg, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ovnManager2).NotTo(gomega.BeNil())

				err = ovnManager2.Start(ctx.Context)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer ovnManager2.Stop()

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

				// Stop ovnManager1. Lock should be acquired by ovnManager2.
				ovnManager1.Stop()

				// Clear the subnet annotations.  ovnManager2 should set it back.
				for _, n := range nodes {
					nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient}, n.Name)
					util.DeleteNodeHostSubnetAnnotation(nodeAnnotator)
					err = nodeAnnotator.Run()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}

				// Sleep for a few seconds so that cm2 gets the chance to become leader
				time.Sleep(3 * time.Second)

				// Check that cluster manager 2 has reset the subnet annotation for each node.
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
	})
})

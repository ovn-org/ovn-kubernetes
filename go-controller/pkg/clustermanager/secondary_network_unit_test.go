package clustermanager

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	fakeipamclaimclient "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/clientset/versioned/fake"
	fakenadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("Cluster Controller Manager", func() {
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

	ginkgo.Context("Secondary networks", func() {
		ginkgo.It("Attach secondary layer3 network", func() {
			app.Action = func(ctx *cli.Context) error {
				kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{Items: nodes()})
				fakeClient := &util.OVNClusterManagerClientset{
					KubeClient:            kubeFakeClient,
					IPAMClaimsClient:      fakeipamclaimclient.NewSimpleClientset(),
					NetworkAttchDefClient: fakenadclient.NewSimpleClientset(),
				}

				gomega.Expect(initConfig(ctx, config.OVNKubernetesFeatureConfig{EnableMultiNetwork: true})).To(gomega.Succeed())
				var err error
				f, err = factory.NewClusterManagerWatchFactory(fakeClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = f.Start()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, networkmanager.Default().Interface(), recorder)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: types.NetConf{Name: "blue"}, Topology: ovntypes.Layer3Topology, Subnets: "192.168.0.0/16/24"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				nc, err := sncm.NewNetworkController(netInfo)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(nc).NotTo(gomega.BeNil())
				gomega.Expect(nc.Start(ctx.Context)).To(gomega.Succeed())
				defer nc.Stop()

				// Check that network controller for "blue" network has set the subnet annotation for each node.
				for _, n := range nodes() {
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

		ginkgo.When("Attaching to a layer2 network", func() {
			const subnets = "192.168.200.0/24,fd12:1234::0/64"
			var (
				fakeClient *util.OVNClusterManagerClientset
				netInfo    util.NetInfo
			)

			ginkgo.BeforeEach(func() {

				fakeClient = &util.OVNClusterManagerClientset{
					KubeClient:            fake.NewSimpleClientset(&v1.NodeList{Items: nodes()}),
					IPAMClaimsClient:      fakeipamclaimclient.NewSimpleClientset(),
					NetworkAttchDefClient: fakenadclient.NewSimpleClientset(),
				}

				var err error
				netInfo, err = util.NewNetInfo(
					&ovncnitypes.NetConf{
						NetConf:  types.NetConf{Name: "blue"},
						Topology: ovntypes.Layer2Topology,
						Subnets:  subnets,
					})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})

			ginkgo.It("The secondary network controller starts successfully", func() {
				app.Action = func(ctx *cli.Context) error {
					gomega.Expect(
						initConfig(ctx, config.OVNKubernetesFeatureConfig{
							EnableMultiNetwork: true,
							EnableInterconnect: true},
						)).To(gomega.Succeed())
					var err error
					f, err = factory.NewClusterManagerWatchFactory(fakeClient)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = f.Start()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, networkmanager.Default().Interface(), recorder)
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

			ginkgo.It("There aren't any automatically reserved IP addresses", func() {
				app.Action = func(ctx *cli.Context) error {
					gomega.Expect(
						initConfig(ctx, config.OVNKubernetesFeatureConfig{
							EnableMultiNetwork: true,
							EnableInterconnect: true},
						)).To(gomega.Succeed())

					var err error
					f, err = factory.NewClusterManagerWatchFactory(fakeClient)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(f.Start()).NotTo(gomega.HaveOccurred())

					sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, networkmanager.Default().Interface(), recorder)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					nc := newNetworkClusterController(
						netInfo,
						sncm.ovnClient,
						sncm.watchFactory,
						sncm.recorder,
						sncm.networkManager,
						nil,
					)
					gomega.Expect(nc.init()).To(gomega.Succeed())
					gomega.Expect(nc.Start(ctx.Context)).To(gomega.Succeed())

					namedSubnetAllocator := nc.subnetAllocator.ForSubnet(netInfo.GetNetworkName())

					firstAllocatableIPs := []string{"192.168.200.1/24", "fd12:1234::1/64"}
					allocatableIPs, err := util.ParseIPNets(firstAllocatableIPs)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(namedSubnetAllocator.AllocateIPs(allocatableIPs)).To(gomega.Succeed())
					return nil
				}

				gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
			})
		})

		ginkgo.When("Attaching to a localnet network", func() {
			const subnets = "192.168.200.0/24,fd12:1234::0/64"

			var (
				fakeClient *util.OVNClusterManagerClientset
				netInfo    util.NetInfo
			)

			ginkgo.BeforeEach(func() {
				fakeClient = &util.OVNClusterManagerClientset{
					KubeClient:            fake.NewSimpleClientset(&v1.NodeList{Items: nodes()}),
					NetworkAttchDefClient: fakenadclient.NewSimpleClientset(),
				}

				gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			})

			ginkgo.DescribeTable(
				"the secondary network controller",
				func(netConf *ovncnitypes.NetConf, featureConfig config.OVNKubernetesFeatureConfig, expectedError error) {
					var err error
					netInfo, err = util.NewNetInfo(netConf)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					app.Action = func(ctx *cli.Context) error {
						gomega.Expect(initConfig(ctx, featureConfig)).To(gomega.Succeed())

						f, err = factory.NewClusterManagerWatchFactory(fakeClient)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						err = f.Start()
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, networkmanager.Default().Interface(), recorder)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						_, err = sncm.NewNetworkController(netInfo)
						if expectedError == nil {
							gomega.Expect(err).NotTo(gomega.HaveOccurred())
						} else {
							gomega.Expect(err).To(gomega.MatchError(expectedError))
						}

						return nil
					}

					gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
				},
				ginkgo.Entry(
					"does not manage localnet topologies on IC deployments for networks without subnets",
					&ovncnitypes.NetConf{NetConf: types.NetConf{Name: "blue"}, Topology: ovntypes.LocalnetTopology},
					config.OVNKubernetesFeatureConfig{EnableInterconnect: true, EnableMultiNetwork: true},
					nad.ErrNetworkControllerTopologyNotManaged,
				),
				ginkgo.Entry(
					"manages localnet topologies on IC deployments for networks with subnets",
					&ovncnitypes.NetConf{
						NetConf:  types.NetConf{Name: "blue"},
						Topology: ovntypes.LocalnetTopology,
						Subnets:  subnets,
					},
					config.OVNKubernetesFeatureConfig{EnableInterconnect: true, EnableMultiNetwork: true},
					nil,
				),
				ginkgo.Entry(
					"does not manage localnet topologies on non-IC deployments without subnets",
					&ovncnitypes.NetConf{NetConf: types.NetConf{Name: "blue"}, Topology: ovntypes.LocalnetTopology},
					config.OVNKubernetesFeatureConfig{EnableMultiNetwork: true},
					nad.ErrNetworkControllerTopologyNotManaged,
				),
				ginkgo.Entry(
					"does not manage localnet topologies on non-IC deployments with subnets",
					&ovncnitypes.NetConf{
						NetConf:  types.NetConf{Name: "blue"},
						Topology: ovntypes.LocalnetTopology,
						Subnets:  subnets,
					},
					config.OVNKubernetesFeatureConfig{EnableMultiNetwork: true},
					nad.ErrNetworkControllerTopologyNotManaged,
				),
			)
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
					KubeClient:            kubeFakeClient,
					NetworkAttchDefClient: fakenadclient.NewSimpleClientset(),
				}

				gomega.Expect(initConfig(ctx, config.OVNKubernetesFeatureConfig{EnableMultiNetwork: true})).To(gomega.Succeed())
				var err error
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

				sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, networkmanager.Default().Interface(), recorder)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Create a fake nad controller for blue network so that the red network gets cleared
				// when CleanupDeletedNetworks is called.  If we don't pass any controller to
				// CleanupDeletedNetworks it will try to cleanup both blue and red networks and
				// there could be a race in updating the node annotations with the fakeclient.
				// fakeclient will not return an error in such cases to trigger retry by RetryOnConflict.
				// So testing the cleanup one at a time.
				netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: types.NetConf{Name: "blue"}, Topology: ovntypes.Layer3Topology})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				oc := newNetworkClusterController(
					netInfo,
					sncm.ovnClient,
					sncm.watchFactory,
					sncm.recorder,
					sncm.networkManager,
					nil,
				)
				err = oc.init()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = sncm.CleanupStaleNetworks(oc)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Clean up the red network
				expectBlueCleanup = false
				expectRedCleanup = true
				gomega.Eventually(checkNodeAnnotations).ShouldNot(gomega.HaveOccurred())

				// Now call CleanupDeletedNetworks() with empty nad controllers.
				// Blue network should also be cleared.
				err = sncm.CleanupStaleNetworks()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectBlueCleanup = true
				expectRedCleanup = true
				gomega.Eventually(checkNodeAnnotations).ShouldNot(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{
				app.Name,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.Context("persistent IP allocations", func() {
			const (
				claimName   = "claim1"
				claimName2  = "claim2"
				namespace   = "ns"
				networkName = "blue"
				subnetCIDR  = "192.168.200.0/24"
				subnetIP    = "192.168.200.2/24"
				subnetIP2   = "192.168.200.3/24"
			)

			var netInfo util.NetInfo

			ginkgo.BeforeEach(func() {
				var err error

				netInfo, err = util.NewNetInfo(
					&ovncnitypes.NetConf{
						NetConf:            types.NetConf{Name: networkName},
						Topology:           ovntypes.Layer2Topology,
						Subnets:            subnetCIDR,
						AllowPersistentIPs: true,
					})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})

			ginkgo.When("the network controller starts", func() {
				ginkgo.It("reserves IPs for the IPAMClaims for the network it manages", func() {
					app.Action = func(ctx *cli.Context) error {
						fakeClient := &util.OVNClusterManagerClientset{
							KubeClient: fake.NewSimpleClientset(),
							IPAMClaimsClient: fakeipamclaimclient.NewSimpleClientset(
								ipamClaimWithIPAddr(claimName, namespace, networkName, subnetIP),
								ipamClaimWithIPAddr(claimName2, namespace, networkName, subnetIP2),
							),
							NetworkAttchDefClient: fakenadclient.NewSimpleClientset(),
						}

						gomega.Expect(
							initConfig(ctx, config.OVNKubernetesFeatureConfig{
								EnableMultiNetwork:  true,
								EnableInterconnect:  true,
								EnablePersistentIPs: true},
							)).To(gomega.Succeed())
						var err error
						f, err = factory.NewClusterManagerWatchFactory(fakeClient)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(f.Start()).To(gomega.Succeed())

						sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, networkmanager.Default().Interface(), recorder)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						nc := newNetworkClusterController(
							netInfo,
							sncm.ovnClient,
							sncm.watchFactory,
							sncm.recorder,
							sncm.networkManager,
							nil,
						)
						gomega.Expect(nc.init()).To(gomega.Succeed())
						gomega.Expect(nc.Start(ctx.Context)).To(gomega.Succeed())

						ips, err := util.ParseIPNets([]string{subnetIP})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(nc.subnetAllocator.AllocateIPPerSubnet(netInfo.GetNetworkName(), ips)).To(
							gomega.Equal(ip.ErrAllocated))

						ips2, err := util.ParseIPNets([]string{subnetIP2})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(nc.subnetAllocator.AllocateIPPerSubnet(netInfo.GetNetworkName(), ips2)).To(
							gomega.Equal(ip.ErrAllocated))

						return nil
					}

					gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
				})

				ginkgo.It("ignores IPs on IPAMClaims for networks it does not manage", func() {
					app.Action = func(ctx *cli.Context) error {
						const someOtherNetwork = "othernet"

						fakeClient := &util.OVNClusterManagerClientset{
							KubeClient: fake.NewSimpleClientset(),
							IPAMClaimsClient: fakeipamclaimclient.NewSimpleClientset(
								ipamClaimWithIPAddr(claimName, namespace, someOtherNetwork, subnetIP),
							),
							NetworkAttchDefClient: fakenadclient.NewSimpleClientset(),
						}

						gomega.Expect(
							initConfig(ctx, config.OVNKubernetesFeatureConfig{
								EnableMultiNetwork:  true,
								EnableInterconnect:  true,
								EnablePersistentIPs: true},
							)).To(gomega.Succeed())
						var err error
						f, err = factory.NewClusterManagerWatchFactory(fakeClient)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(f.Start()).To(gomega.Succeed())

						sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, networkmanager.Default().Interface(), recorder)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						nc := newNetworkClusterController(
							netInfo,
							sncm.ovnClient,
							sncm.watchFactory,
							sncm.recorder,
							sncm.networkManager,
							nil,
						)
						gomega.Expect(nc.init()).To(gomega.Succeed())
						gomega.Expect(nc.Start(ctx.Context)).To(gomega.Succeed())

						ips, err := util.ParseIPNets([]string{subnetIP})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						gomega.Expect(nc.subnetAllocator.AllocateIPPerSubnet(netInfo.GetNetworkName(), ips)).To(gomega.Succeed())

						return nil
					}

					gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
				})
			})

			ginkgo.When("IPAMClaims are deleted from the datastore", func() {
				ginkgo.It("returns the IP addresses to the pool", func() {
					app.Action = func(ctx *cli.Context) error {
						ipamClaimsClient := fakeipamclaimclient.NewSimpleClientset(
							ipamClaimWithIPAddr(claimName, namespace, networkName, subnetIP),
						)
						fakeClient := &util.OVNClusterManagerClientset{
							KubeClient:            fake.NewSimpleClientset(),
							IPAMClaimsClient:      ipamClaimsClient,
							NetworkAttchDefClient: fakenadclient.NewSimpleClientset(),
						}

						gomega.Expect(
							initConfig(ctx, config.OVNKubernetesFeatureConfig{
								EnableMultiNetwork:  true,
								EnableInterconnect:  true,
								EnablePersistentIPs: true},
							)).To(gomega.Succeed())
						var err error
						f, err = factory.NewClusterManagerWatchFactory(fakeClient)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(f.Start()).To(gomega.Succeed())

						sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, networkmanager.Default().Interface(), recorder)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						nc := newNetworkClusterController(
							netInfo,
							sncm.ovnClient,
							sncm.watchFactory,
							sncm.recorder,
							sncm.networkManager,
							nil,
						)
						gomega.Expect(nc.init()).To(gomega.Succeed())
						gomega.Expect(nc.Start(ctx.Context)).To(gomega.Succeed())

						ips, err := util.ParseIPNets([]string{subnetIP})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						namedSubnetAllocator := nc.subnetAllocator.ForSubnet(netInfo.GetNetworkName())
						// the IP address is already allocated in t = x
						gomega.Expect(namedSubnetAllocator.AllocateIPs(ips)).To(gomega.Equal(ip.ErrAllocated))

						gomega.Expect(
							ipamClaimsClient.K8sV1alpha1().IPAMClaims(namespace).Delete(
								context.Background(),
								claimName,
								metav1.DeleteOptions{},
							)).To(gomega.Succeed())

						// the IP address is available in t = x + T
						gomega.Eventually(func() error {
							return namedSubnetAllocator.AllocateIPs(ips)
						}).Should(gomega.Succeed())

						return nil
					}

					gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
				})

				ginkgo.It("ignores IPs on deleted IPAMClaims for networks it does not manage", func() {
					const (
						claimName                 = "claim1"
						namespace                 = "ns"
						someOtherNetwork          = "othernet"
						someOtherNetworkClaimName = "claim321"
					)

					app.Action = func(ctx *cli.Context) error {
						ipamClaimsClient := fakeipamclaimclient.NewSimpleClientset(
							ipamClaimWithIPAddr(claimName, namespace, networkName, subnetIP),
							ipamClaimWithIPAddr(someOtherNetworkClaimName, namespace, someOtherNetwork, subnetIP),
						)
						fakeClient := &util.OVNClusterManagerClientset{
							KubeClient:            fake.NewSimpleClientset(),
							IPAMClaimsClient:      ipamClaimsClient,
							NetworkAttchDefClient: fakenadclient.NewSimpleClientset(),
						}

						gomega.Expect(
							initConfig(ctx, config.OVNKubernetesFeatureConfig{
								EnableMultiNetwork:  true,
								EnableInterconnect:  true,
								EnablePersistentIPs: true},
							)).To(gomega.Succeed())
						var err error
						f, err = factory.NewClusterManagerWatchFactory(fakeClient)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(f.Start()).To(gomega.Succeed())

						sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, networkmanager.Default().Interface(), recorder)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						nc := newNetworkClusterController(
							netInfo,
							sncm.ovnClient,
							sncm.watchFactory,
							sncm.recorder,
							sncm.networkManager,
							nil,
						)
						gomega.Expect(nc.init()).To(gomega.Succeed())
						gomega.Expect(nc.Start(ctx.Context)).To(gomega.Succeed())

						ips, err := util.ParseIPNets([]string{subnetIP})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						namedSubnetAllocator := nc.subnetAllocator.ForSubnet(netInfo.GetNetworkName())
						// the IP address is already allocated in t = x
						gomega.Expect(namedSubnetAllocator.AllocateIPs(ips)).To(gomega.Equal(ip.ErrAllocated))

						// deleting an allocation for a different network
						gomega.Expect(
							ipamClaimsClient.K8sV1alpha1().IPAMClaims(namespace).Delete(
								context.Background(),
								someOtherNetworkClaimName,
								metav1.DeleteOptions{},
							)).To(gomega.Succeed())

						// does not impact the network for which we have the controller
						gomega.Consistently(func() error {
							return namedSubnetAllocator.AllocateIPs(ips)
						}).Should(gomega.Equal(ip.ErrAllocated))

						return nil
					}

					gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
				})
			})

			ginkgo.When("the persistent IPs feature is disabled on the network", func() {
				ginkgo.It("does not sync the IP pool with existing persistent IP allocations", func() {
					app.Action = func(ctx *cli.Context) error {
						var err error
						netInfo, err = util.NewNetInfo(
							&ovncnitypes.NetConf{
								NetConf:  types.NetConf{Name: networkName},
								Topology: ovntypes.Layer2Topology,
								Subnets:  subnetCIDR,
							})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						fakeClient := &util.OVNClusterManagerClientset{
							KubeClient: fake.NewSimpleClientset(),
							IPAMClaimsClient: fakeipamclaimclient.NewSimpleClientset(
								ipamClaimWithIPAddr(claimName, namespace, networkName, subnetIP),
							),
							NetworkAttchDefClient: fakenadclient.NewSimpleClientset(),
						}

						gomega.Expect(
							initConfig(ctx, config.OVNKubernetesFeatureConfig{
								EnableMultiNetwork:  true,
								EnableInterconnect:  true,
								EnablePersistentIPs: true},
							)).To(gomega.Succeed())

						f, err = factory.NewClusterManagerWatchFactory(fakeClient)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(f.Start()).To(gomega.Succeed())

						sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, networkmanager.Default().Interface(), recorder)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						nc := newNetworkClusterController(
							netInfo,
							sncm.ovnClient,
							sncm.watchFactory,
							sncm.recorder,
							sncm.networkManager,
							nil,
						)
						gomega.Expect(nc.init()).To(gomega.Succeed())
						gomega.Expect(nc.Start(ctx.Context)).To(gomega.Succeed())

						ips, err := util.ParseIPNets([]string{subnetIP})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())

						namedSubnetAllocator := nc.subnetAllocator.ForSubnet(netInfo.GetNetworkName())
						gomega.Expect(namedSubnetAllocator.AllocateIPs(ips)).To(gomega.Succeed())

						return nil
					}

					gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
				})
			})
		})
	})

	ginkgo.Context("User defined primary networks", func() {

		ginkgo.When("Attaching to a layer2 network", func() {
			const subnets = "192.168.200.0/24,fd12:1234::0/64"

			var (
				fakeClient *util.OVNClusterManagerClientset
				netInfo    util.NetInfo
			)

			ginkgo.It("Automatically reserves IPs for the GW (.1) and mgmt port (.2)", func() {
				app.Action = func(ctx *cli.Context) error {
					gomega.Expect(
						initConfig(ctx, config.OVNKubernetesFeatureConfig{
							EnableMultiNetwork: true,
							EnableInterconnect: true},
						)).To(gomega.Succeed())

					var err error
					netInfo, err = util.NewNetInfo(
						&ovncnitypes.NetConf{
							NetConf:  types.NetConf{Name: "blue"},
							Role:     ovntypes.NetworkRolePrimary,
							Subnets:  subnets,
							Topology: ovntypes.Layer2Topology,
						})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeClient = &util.OVNClusterManagerClientset{
						KubeClient:            fake.NewSimpleClientset(&v1.NodeList{Items: nodes()}),
						IPAMClaimsClient:      fakeipamclaimclient.NewSimpleClientset(),
						NetworkAttchDefClient: fakenadclient.NewSimpleClientset(),
					}
					f, err = factory.NewClusterManagerWatchFactory(fakeClient)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(f.Start()).NotTo(gomega.HaveOccurred())

					sncm, err := newSecondaryNetworkClusterManager(fakeClient, f, networkmanager.Default().Interface(), recorder)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					nc := newNetworkClusterController(
						netInfo,
						sncm.ovnClient,
						sncm.watchFactory,
						sncm.recorder,
						sncm.networkManager,
						nil,
					)
					gomega.Expect(nc.init()).To(gomega.Succeed())
					gomega.Expect(nc.Start(ctx.Context)).To(gomega.Succeed())

					namedSubnetAllocator := nc.subnetAllocator.ForSubnet(netInfo.GetNetworkName())
					for _, alreadyAllocatedIP := range []string{
						"192.168.200.1/24",
						"192.168.200.2/24",
						"fd12:1234::1/64",
						"fd12:1234::2/64",
					} {
						autoExcludedIP, err := util.ParseIPNets([]string{alreadyAllocatedIP})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(namedSubnetAllocator.AllocateIPs(autoExcludedIP)).To(
							gomega.MatchError(ip.ErrAllocated),
							fmt.Sprintf("expected to fail allocating IP: %q", alreadyAllocatedIP),
						)
					}

					firstAllocatableIPs := []string{"192.168.200.3/24", "fd12:1234::3/64"}
					allocatableIPs, err := util.ParseIPNets(firstAllocatableIPs)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(namedSubnetAllocator.AllocateIPs(allocatableIPs)).To(gomega.Succeed())
					return nil
				}

				gomega.Expect(app.Run([]string{
					app.Name,
					// define the cluster as dualstack so the user defined primary network matches the ip family
					"--cluster-subnets=10.128.0.0/14,fd00:10:244::/48",
					"--k8s-service-cidrs=172.16.1.0/24,fd02::/112",
				})).To(gomega.Succeed())
			})

		})
	})
})

func nodes() []v1.Node {
	return []v1.Node{
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
}

func initConfig(ctx *cli.Context, ovkConfig config.OVNKubernetesFeatureConfig) error {
	_, err := config.InitConfig(ctx, nil, nil)
	if err != nil {
		return err
	}
	config.Kubernetes.HostNetworkNamespace = ""
	config.OVNKubernetesFeature = ovkConfig
	return nil
}

func ipamClaimWithIPAddr(claimName string, namespace string, networkName, ip string) *ipamclaimsapi.IPAMClaim {
	return &ipamclaimsapi.IPAMClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: namespace,
		},
		Spec: ipamclaimsapi.IPAMClaimSpec{
			Network: networkName,
		},
		Status: ipamclaimsapi.IPAMClaimStatus{
			IPs: []string{ip},
		},
	}
}

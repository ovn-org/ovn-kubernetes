package ovn

import (
	"context"
	"net"

	"github.com/urfave/cli/v2"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"

	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

func newNamespaceMeta(namespace string, additionalLabels map[string]string) metav1.ObjectMeta {
	labels := map[string]string{
		"name": namespace,
	}
	for k, v := range additionalLabels {
		labels[k] = v
	}
	return metav1.ObjectMeta{
		UID:         types.UID(namespace),
		Name:        namespace,
		Labels:      labels,
		Annotations: map[string]string{},
	}
}

func newNamespaceWithLabels(namespace string, additionalLabels map[string]string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: newNamespaceMeta(namespace, additionalLabels),
		Spec:       v1.NamespaceSpec{},
		Status:     v1.NamespaceStatus{},
	}
}

func newNamespace(namespace string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: newNamespaceMeta(namespace, nil),
		Spec:       v1.NamespaceSpec{},
		Status:     v1.NamespaceStatus{},
	}
}

var _ = ginkgo.Describe("OVN Namespace Operations", func() {
	const (
		namespaceName        = "namespace1"
		clusterIPNet  string = "10.1.0.0"
		clusterCIDR   string = clusterIPNet + "/16"
	)
	var (
		app     *cli.App
		fakeOvn *FakeOVN
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(ovntest.NewLooseCompareFakeExec())
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("on startup", func() {

		ginkgo.It("reconciles an existing namespace with pods", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace(namespaceName)
				tP := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"11:22:33:44:55:66",
					namespaceT.Name,
				)

				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(namespaceT.Name, tP.podName, tP.nodeName, tP.podIP),
						},
					},
				)
				podMAC := ovntest.MustParseMAC(tP.podMAC)
				podIPNets := []*net.IPNet{ovntest.MustParseIPNet(tP.podIP + "/24")}
				fakeOvn.controller.logicalPortCache.add(tP.nodeName, tP.portName, fakeUUID, podMAC, podIPNets)
				fakeOvn.controller.WatchNamespaces()

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceT.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName, []string{tP.podIP})

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("creates an empty address set for the namespace without pods", func() {
			app.Action = func(ctx *cli.Context) error {
				fakeOvn.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						*newNamespace(namespaceName),
					},
				})
				fakeOvn.controller.WatchNamespaces()

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName)

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("creates an address set for existing nodes when the host network traffic namespace is created", func() {
			app.Action = func(ctx *cli.Context) error {
				node1 := tNode{
					Name:                 "node1",
					NodeIP:               "1.2.3.4",
					NodeLRPMAC:           "0a:58:0a:01:01:01",
					LrpMAC:               "0a:58:64:40:00:02",
					LrpIP:                "100.64.0.2",
					LrpIPv6:              "fd98::2",
					DrLrpIP:              "100.64.0.1",
					PhysicalBridgeMAC:    "11:22:33:44:55:66",
					SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
					NodeSubnet:           "10.1.1.0/24",
					GWRouter:             ovntypes.GWRouterPrefix + "node1",
					GatewayRouterIPMask:  "172.16.16.2/24",
					GatewayRouterIP:      "172.16.16.2",
					GatewayRouterNextHop: "172.16.16.1",
					PhysicalBridgeName:   "br-eth0",
					NodeGWIP:             "10.1.1.1/24",
					NodeMgmtPortIP:       "10.1.1.2",
					NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
					DnatSnatIP:           "169.254.0.1",
				}
				// create a test node and annotate it with host subnet
				testNode := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1.Name,
					},
					Status: kapi.NodeStatus{
						Addresses: []kapi.NodeAddress{
							{
								Type:    kapi.NodeExternalIP,
								Address: node1.NodeIP,
							},
						},
					},
				}

				hostNetworkNamespace := "test-host-network-ns"
				config.Kubernetes.HostNetworkNamespace = hostNetworkNamespace
				hostNetworkNs := *newNamespace(hostNetworkNamespace)

				kubeFakeClient := fake.NewSimpleClientset(
					&v1.NamespaceList{
						Items: []v1.Namespace{
							hostNetworkNs,
						},
					},
				)
				egressFirewallFakeClient := &egressfirewallfake.Clientset{}
				egressIPFakeClient := &egressipfake.Clientset{}
				fakeClient := &util.OVNClientset{
					KubeClient:           kubeFakeClient,
					EgressIPClient:       egressIPFakeClient,
					EgressFirewallClient: egressFirewallFakeClient,
				}

				_, err := fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &testNode, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{fakeClient.KubeClient, fakeClient.EgressIPClient, fakeClient.EgressFirewallClient}, testNode.Name)

				ifaceID := node1.PhysicalBridgeName + "_" + node1.Name
				vlanID := uint(1024)
				l3Config := &util.L3GatewayConfig{
					Mode:           config.GatewayModeShared,
					ChassisID:      node1.SystemID,
					InterfaceID:    ifaceID,
					MACAddress:     ovntest.MustParseMAC(node1.PhysicalBridgeMAC),
					IPAddresses:    ovntest.MustParseIPNets(node1.GatewayRouterIPMask),
					NextHops:       ovntest.MustParseIPs(node1.GatewayRouterNextHop),
					NodePortEnable: true,
					VLANID:         &vlanID,
				}
				err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
				err = util.SetNodeManagementPortMACAddress(nodeAnnotator, ovntest.MustParseMAC(node1.NodeMgmtPortMAC))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = util.SetNodeLocalNatAnnotation(nodeAnnotator, []net.IP{ovntest.MustParseIP(node1.DnatSnatIP)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = util.SetNodeHostAddresses(nodeAnnotator, sets.NewString("9.9.9.9"))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = nodeAnnotator.Run()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				nodeHostSubnetAnnotations, err := util.ParseNodeHostSubnetAnnotation(updatedNode)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(nodeHostSubnetAnnotations[0].String()).Should(gomega.Equal(node1.NodeSubnet))
				_, err = config.InitConfig(ctx, fakeOvn.fakeExec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.fakeClient = fakeClient
				expectedOVNClusterRouter := &nbdb.LogicalRouter{
					UUID: ovntypes.OVNClusterRouter + "-UUID",
					Name: ovntypes.OVNClusterRouter,
				}
				expectedNodeSwitch := &nbdb.LogicalSwitch{
					UUID: node1.Name + "-UUID",
					Name: node1.Name,
				}
				expectedClusterRouterPortGroup := &nbdb.PortGroup{
					UUID: ovntypes.ClusterRtrPortGroupName + "-UUID",
					Name: ovntypes.ClusterRtrPortGroupName,
					ExternalIDs: map[string]string{
						"name": ovntypes.ClusterRtrPortGroupName,
					},
				}
				expectedClusterPortGroup := &nbdb.PortGroup{
					UUID: ovntypes.ClusterPortGroupName + "-UUID",
					Name: ovntypes.ClusterPortGroupName,
					ExternalIDs: map[string]string{
						"name": ovntypes.ClusterPortGroupName,
					},
				}
				fakeOvn.dbSetup = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitch{
							UUID: ovntypes.OVNJoinSwitch + "-UUID",
							Name: ovntypes.OVNJoinSwitch,
						},
						expectedOVNClusterRouter,
						expectedNodeSwitch,
						expectedClusterRouterPortGroup,
						expectedClusterPortGroup,
					},
				}
				fakeOvn.init()
				fakeOvn.controller.multicastSupport = false
				_, clusterNetwork, err := net.ParseCIDR(clusterCIDR)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.controller.masterSubnetAllocator.AddNetworkRange(clusterNetwork, 24)

				fakeOvn.controller.SCTPSupport = true

				fexec := fakeOvn.fakeExec

				expectedDatabaseState := []libovsdb.TestData{}
				expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, fexec, &node1, clusterCIDR, config.IPv6Mode)

				fakeOvn.controller.joinSwIPManager, _ = lsm.NewJoinLogicalSwitchIPManager(fakeOvn.nbClient, []string{node1.Name})
				_, err = fakeOvn.controller.joinSwIPManager.EnsureJoinLRPIPs(ovntypes.OVNClusterRouter)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gwLRPIPs, err := fakeOvn.controller.joinSwIPManager.EnsureJoinLRPIPs(node1.Name)
				gomega.Expect(len(gwLRPIPs) != 0).To(gomega.BeTrue())

				fakeOvn.controller.WatchNamespaces()
				fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(hostNetworkNamespace)

				fakeOvn.controller.WatchNodes()

				fakeOvn.controller.StartServiceController(fakeOvn.wg, false)

				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)

				nodeSubnet := ovntest.MustParseIPNet(node1.NodeSubnet)
				var clusterSubnets []*net.IPNet
				for _, clusterSubnet := range config.Default.ClusterSubnets {
					clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
				}
				joinLRPIP, joinLRNetwork, _ := net.ParseCIDR(node1.LrpIP + "/16")
				dLRPIP, dLRPNetwork, _ := net.ParseCIDR(node1.DrLrpIP + "/16")

				joinLRPIPs := &net.IPNet{
					IP:   joinLRPIP,
					Mask: joinLRNetwork.Mask,
				}
				dLRPIPs := &net.IPNet{
					IP:   dLRPIP,
					Mask: dLRPNetwork.Mask,
				}

				skipSnat := false
				expectedDatabaseState = generateGatewayInitExpectedNB(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{nodeSubnet}, l3Config, []*net.IPNet{joinLRPIPs}, []*net.IPNet{dLRPIPs}, skipSnat)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				gomega.Eventually(fexec.CalledMatchesExpected()).Should(gomega.BeTrue(), fexec.ErrorDesc)

				// check the namespace again and ensure the address set
				// being created with the right set of IPs in it.
				allowIPs := []string{node1.NodeMgmtPortIP}
				for _, lrpIP := range gwLRPIPs {
					allowIPs = append(allowIPs, lrpIP.IP.String())
				}
				fakeOvn.asf.ExpectAddressSetWithIPs(hostNetworkNamespace, allowIPs)

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"--init-gateways",
				"--nodeport",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})
	})

	ginkgo.Context("during execution", func() {
		ginkgo.It("deletes an empty namespace's resources", func() {
			app.Action = func(ctx *cli.Context) error {
				fakeOvn.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						*newNamespace(namespaceName),
					},
				})
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName)

				err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), namespaceName, *metav1.NewDeleteOptions(1))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.EventuallyExpectNoAddressSet(namespaceName)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})

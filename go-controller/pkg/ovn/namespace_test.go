package ovn

import (
	"context"
	"net"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
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

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

func getNamespaceAnnotations(fakeClient kubernetes.Interface, name string) map[string]string {
	ns, err := fakeClient.CoreV1().Namespaces().Get(context.TODO(), name, metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return ns.Annotations
}

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
		fakeOvn *FakeOVN
		wg      *sync.WaitGroup
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		err := config.PrepareTestConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		fakeOvn = NewFakeOVN()
		wg = &sync.WaitGroup{}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		wg.Wait()
	})

	ginkgo.Context("on startup", func() {

		ginkgo.It("reconciles an existing namespace with pods", func() {
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

			fakeOvn.start(
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
			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceT.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeOvn.asf.EventuallyExpectAddressSetWithIPs(namespaceName, []string{tP.podIP})
		})

		ginkgo.It("creates an empty address set for the namespace without pods", func() {
			fakeOvn.start(&v1.NamespaceList{
				Items: []v1.Namespace{
					*newNamespace(namespaceName),
				},
			})
			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeOvn.asf.ExpectEmptyAddressSet(namespaceName)
		})

		ginkgo.It("creates an address set for existing nodes when the host network traffic namespace is created", func() {
			config.Gateway.Mode = config.GatewayModeShared
			config.Gateway.NodeportEnable = true
			var err error
			config.Default.RawClusterSubnets = clusterCIDR
			config.Default.ClusterSubnets, err = config.ParseClusterSubnetEntries(clusterCIDR)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

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
				NodeHostAddress:      []string{"9.9.9.9"},
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
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				Name: ovntypes.ClusterLBGroupName,
				UUID: ovntypes.ClusterLBGroupName + "-UUID",
			}

			fakeOvn.startWithDBSetup(
				libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitch{
							UUID: ovntypes.OVNJoinSwitch + "-UUID",
							Name: ovntypes.OVNJoinSwitch,
						},
						expectedOVNClusterRouter,
						expectedNodeSwitch,
						expectedClusterRouterPortGroup,
						expectedClusterPortGroup,
						expectedClusterLBGroup,
					},
				},
				&v1.NamespaceList{
					Items: []v1.Namespace{
						*newNamespace(hostNetworkNamespace),
					},
				},
			)
			fakeOvn.controller.multicastSupport = false
			fakeOvn.controller.SCTPSupport = true

			fakeOvn.controller.defaultGatewayCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, clusterNetwork, err := net.ParseCIDR(clusterCIDR)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			fakeOvn.controller.masterSubnetAllocator.AddNetworkRange(clusterNetwork, 24)

			expectedDatabaseState := []libovsdb.TestData{}

			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), &testNode, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{fakeOvn.fakeClient.KubeClient, fakeOvn.fakeClient.EgressIPClient, fakeOvn.fakeClient.EgressFirewallClient, nil}, testNode.Name)

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
			err = util.SetNodeHostAddresses(nodeAnnotator, sets.NewString(node1.NodeHostAddress...))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			updatedNode, err := fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			nodeHostSubnetAnnotations, err := util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(nodeHostSubnetAnnotations[0].String()).Should(gomega.Equal(node1.NodeSubnet))

			// Add subnet to otherconfig for node
			expectedNodeSwitch.OtherConfig = map[string]string{"subnet": node1.NodeSubnet}

			// Add cluster LB Group to node switch.
			expectedNodeSwitch.LoadBalancerGroup = []string{expectedClusterLBGroup.UUID}

			expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1)

			fakeOvn.controller.joinSwIPManager, _ = lsm.NewJoinLogicalSwitchIPManager(fakeOvn.nbClient, expectedNodeSwitch.UUID, []string{node1.Name})
			_, err = fakeOvn.controller.joinSwIPManager.EnsureJoinLRPIPs(ovntypes.OVNClusterRouter)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gwLRPIPs, err := fakeOvn.controller.joinSwIPManager.EnsureJoinLRPIPs(node1.Name)
			gomega.Expect(len(gwLRPIPs) != 0).To(gomega.BeTrue())

			err = fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(hostNetworkNamespace)

			fakeOvn.controller.WatchNodes()

			fakeOvn.controller.StartServiceController(wg, false)

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
			expectedDatabaseState = generateGatewayInitExpectedNB(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{nodeSubnet}, l3Config, []*net.IPNet{joinLRPIPs}, []*net.IPNet{dLRPIPs}, skipSnat, node1.NodeMgmtPortIP)
			gomega.Eventually(fakeOvn.nbClient, 2).Should(libovsdbtest.HaveData(expectedDatabaseState))

			// check the namespace again and ensure the address set
			// being created with the right set of IPs in it.
			allowIPs := []string{node1.NodeMgmtPortIP}
			for _, lrpIP := range gwLRPIPs {
				allowIPs = append(allowIPs, lrpIP.IP.String())
			}
			fakeOvn.asf.EventuallyExpectAddressSetWithIPs(hostNetworkNamespace, allowIPs)
		})
	})

	ginkgo.Context("during execution", func() {
		ginkgo.It("deletes an empty namespace's resources", func() {
			fakeOvn.start(&v1.NamespaceList{
				Items: []v1.Namespace{
					*newNamespace(namespaceName),
				},
			})
			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			fakeOvn.asf.ExpectEmptyAddressSet(namespaceName)

			err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), namespaceName, *metav1.NewDeleteOptions(1))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			fakeOvn.asf.EventuallyExpectNoAddressSet(namespaceName)
		})
	})
})

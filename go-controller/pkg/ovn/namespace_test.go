package ovn

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	dnsnameresolver "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/dns_name_resolver"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"

	"github.com/onsi/ginkgo/v2"
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

func newUDNNamespaceWithLabels(namespace string, additionalLabels map[string]string) *v1.Namespace {
	n := &v1.Namespace{
		ObjectMeta: newNamespaceMeta(namespace, additionalLabels),
		Spec:       v1.NamespaceSpec{},
		Status:     v1.NamespaceStatus{},
	}
	n.Labels[ovntypes.RequiredUDNNamespaceLabel] = ""
	return n
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

func newUDNNamespace(namespace string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: newNamespaceMeta(namespace, map[string]string{ovntypes.RequiredUDNNamespaceLabel: ""}),
		Spec:       v1.NamespaceSpec{},
		Status:     v1.NamespaceStatus{},
	}
}

func getDefaultNetNsAddrSetHashNames(ns string) (string, string) {
	return getNsAddrSetHashNames(DefaultNetworkControllerName, ns)
}

func getNsAddrSetHashNames(netControllerName, ns string) (string, string) {
	return addressset.GetHashNamesForAS(getNamespaceAddrSetDbIDs(ns, netControllerName))
}

func buildNamespaceAddressSets(namespace string, ips []string) (*nbdb.AddressSet, *nbdb.AddressSet) {
	return addressset.GetTestDbAddrSets(getNamespaceAddrSetDbIDs(namespace, "default-network-controller"), ips)
}

var _ = ginkgo.Describe("OVN Namespace Operations", func() {
	const (
		namespaceName         = "namespace1"
		clusterIPNet   string = "10.1.0.0"
		clusterCIDR    string = clusterIPNet + "/16"
		controllerName        = DefaultNetworkControllerName
	)
	var (
		fakeOvn *FakeOVN
		wg      *sync.WaitGroup
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		err := config.PrepareTestConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		fakeOvn = NewFakeOVN(true)
		wg = &sync.WaitGroup{}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		wg.Wait()
	})

	ginkgo.Context("on startup", func() {
		ginkgo.It("only cleans up address sets owned by namespace", func() {
			namespace1 := newNamespace(namespaceName)
			// namespace-owned address set for existing namespace, should stay
			ns1 := getNamespaceAddrSetDbIDs(namespaceName, DefaultNetworkControllerName)
			fakeOvn.asf.NewAddressSet(ns1, []string{"1.1.1.1"})
			// namespace-owned address set for stale namespace, should be deleted
			ns2 := getNamespaceAddrSetDbIDs("namespace2", DefaultNetworkControllerName)
			fakeOvn.asf.NewAddressSet(ns2, []string{"1.1.1.2"})
			// netpol peer address set for existing netpol, should stay
			netpol := getPodSelectorAddrSetDbIDs("pasName", DefaultNetworkControllerName)
			fakeOvn.asf.NewAddressSet(netpol, []string{"1.1.1.3"})
			// egressQoS-owned address set, should stay
			qos := getEgressQosAddrSetDbIDs("namespace", "0", controllerName)
			fakeOvn.asf.NewAddressSet(qos, []string{"1.1.1.4"})
			// hybridNode-owned address set, should stay
			hybridNode := apbroute.GetHybridRouteAddrSetDbIDs("node", DefaultNetworkControllerName)
			fakeOvn.asf.NewAddressSet(hybridNode, []string{"1.1.1.5"})
			// egress firewall-owned address set, should stay
			ef := dnsnameresolver.GetEgressFirewallDNSAddrSetDbIDs("dnsname", controllerName)
			fakeOvn.asf.NewAddressSet(ef, []string{"1.1.1.6"})

			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{}})
			err := fakeOvn.controller.syncNamespaces([]interface{}{namespace1})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeOvn.asf.ExpectAddressSetWithAddresses(ns1, []string{"1.1.1.1"})
			fakeOvn.asf.EventuallyExpectNoAddressSet(ns2)
			fakeOvn.asf.ExpectAddressSetWithAddresses(netpol, []string{"1.1.1.3"})
			fakeOvn.asf.ExpectAddressSetWithAddresses(qos, []string{"1.1.1.4"})
			fakeOvn.asf.ExpectAddressSetWithAddresses(hybridNode, []string{"1.1.1.5"})
			fakeOvn.asf.ExpectAddressSetWithAddresses(ef, []string{"1.1.1.6"})
		})

		ginkgo.It("reconciles an existing namespace with pods", func() {
			// this flag will create namespaced port group
			config.OVNKubernetesFeature.EnableEgressFirewall = true
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

			tPod := newPod(namespaceT.Name, tP.podName, tP.nodeName, tP.podIP)
			fakeOvn.start(
				&v1.NamespaceList{
					Items: []v1.Namespace{
						namespaceT,
					},
				},
				&v1.NodeList{
					Items: []v1.Node{
						*newNode("node1", "192.168.126.202/24"),
					},
				},
				&v1.PodList{
					Items: []v1.Pod{
						*tPod,
					},
				},
			)
			podMAC := ovntest.MustParseMAC(tP.podMAC)
			podIPNets := []*net.IPNet{ovntest.MustParseIPNet(tP.podIP + "/24")}
			fakeOvn.controller.logicalPortCache.add(tPod, tP.nodeName, ovntypes.DefaultNetworkName, fakeUUID, podMAC, podIPNets)
			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceT.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeOvn.asf.EventuallyExpectAddressSetWithAddresses(namespaceName, []string{tP.podIP})

			// port group is empty, because it will be filled by pod add logic
			pgIDs := getNamespacePortGroupDbIDs(namespaceName, DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{pg}))
		})

		ginkgo.It("creates an empty address set and port group for the namespace without pods", func() {
			// this flag will create namespaced port group
			config.OVNKubernetesFeature.EnableEgressFirewall = true
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

			pgIDs := getNamespacePortGroupDbIDs(namespaceName, DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{pg}))
		})

		ginkgo.It("creates an address set for existing nodes when the host network traffic namespace is created", func() {
			config.Gateway.Mode = config.GatewayModeShared
			config.Gateway.NodeportEnable = true
			var err error
			config.Default.ClusterSubnets, err = config.ParseClusterSubnetEntries(clusterCIDR)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			node1 := tNode{
				Name:                 "node1",
				NodeIP:               "1.2.3.4",
				NodeLRPMAC:           "0a:58:0a:01:01:01",
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
			testNode := node1.k8sNode("2")

			hostNetworkNamespace := "test-host-network-ns"
			config.Kubernetes.HostNetworkNamespace = hostNetworkNamespace

			expectedClusterLBGroup := newLoadBalancerGroup(ovntypes.ClusterLBGroupName)
			expectedSwitchLBGroup := newLoadBalancerGroup(ovntypes.ClusterSwitchLBGroupName)
			expectedRouterLBGroup := newLoadBalancerGroup(ovntypes.ClusterRouterLBGroupName)
			expectedOVNClusterRouter := newOVNClusterRouter()
			expectedNodeSwitch := node1.logicalSwitch([]string{expectedClusterLBGroup.UUID, expectedSwitchLBGroup.UUID})
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			fakeOvn.startWithDBSetup(
				libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						newClusterJoinSwitch(),
						expectedOVNClusterRouter,
						expectedNodeSwitch,
						expectedClusterRouterPortGroup,
						expectedClusterPortGroup,
						expectedClusterLBGroup,
						expectedSwitchLBGroup,
						expectedRouterLBGroup,
					},
				},
				&v1.NamespaceList{
					Items: []v1.Namespace{
						*newNamespace(hostNetworkNamespace),
					},
				},
				&v1.NodeList{
					Items: []v1.Node{
						testNode,
					},
				},
			)
			fakeOvn.controller.multicastSupport = false
			fakeOvn.controller.SCTPSupport = true

			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: fakeOvn.fakeClient.KubeClient}, testNode.Name)

			vlanID := uint(1024)
			l3Config := node1.gatewayConfig(config.GatewayModeShared, vlanID)
			err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.UpdateNodeManagementPortMACAddresses(&testNode, nodeAnnotator, ovntest.MustParseMAC(node1.NodeMgmtPortMAC), ovntypes.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostCIDRs(nodeAnnotator, sets.New[string](fmt.Sprintf("%s/24", node1.NodeIP)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			updatedNode, err := fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			nodeHostSubnetAnnotations, err := util.ParseNodeHostSubnetAnnotation(updatedNode, ovntypes.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeHostSubnetAnnotations[0].String()).To(gomega.Equal(node1.NodeSubnet))

			expectedDatabaseState := []libovsdb.TestData{}
			expectedDatabaseState = addNodeLogicalFlowsWithServiceController(expectedDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1, fakeOvn.controller.svcTemplateSupport)

			// Addressset of the host-network namespace was initialized but the node logical switch management port address may or may not
			// be in the addressset yet, depending on if the host subnets annotation of the node exists in the informer cache. The addressset
			// can only be deterministic when WatchNamespaces() handles this host network namespace.

			gwLRPIPs, err := util.ParseNodeGatewayRouterJoinAddrs(&testNode, ovntypes.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(gwLRPIPs) != 0).To(gomega.BeTrue())

			err = fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = fakeOvn.controller.WatchNodes()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = fakeOvn.controller.StartServiceController(wg, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			nodeSubnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			var clusterSubnets []*net.IPNet
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
			}
			skipSnat := false
			expectedDatabaseState = generateGatewayInitExpectedNB(expectedDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{nodeSubnet}, l3Config,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)}, skipSnat,
				node1.NodeMgmtPortIP, "1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

			// check the namespace again and ensure the address set
			// being created with the right set of IPs in it.
			allowIPs := []string{node1.NodeMgmtPortIP}
			for _, lrpIP := range gwLRPIPs {
				allowIPs = append(allowIPs, lrpIP.IP.String())
			}
			fakeOvn.asf.EventuallyExpectAddressSetWithAddresses(hostNetworkNamespace, allowIPs)
		})

		ginkgo.It("reconciles an existing namespace port group, without updating it", func() {
			// this flag will create namespaced port group
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			namespaceT := *newNamespace(namespaceName)
			pgIDs := getNamespacePortGroupDbIDs(namespaceName, DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			initialData := []libovsdb.TestData{pg}

			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{NBData: initialData},
				&v1.NamespaceList{
					Items: []v1.Namespace{
						namespaceT,
					},
				},
				&v1.NodeList{
					Items: []v1.Node{
						*newNode("node1", "192.168.126.202/24"),
					},
				},
			)

			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeOvn.asf.EventuallyExpectAddressSetWithAddresses(namespaceName, []string{})
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(initialData))
		})
		ginkgo.It("deletes an existing namespace port group when egress firewall and multicast are disabled", func() {
			namespaceT := *newNamespace(namespaceName)
			pgIDs := getNamespacePortGroupDbIDs(namespaceName, DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			initialData := []libovsdb.TestData{pg}

			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{NBData: initialData},
				&v1.NamespaceList{
					Items: []v1.Namespace{
						namespaceT,
					},
				},
				&v1.NodeList{
					Items: []v1.Node{
						*newNode("node1", "192.168.126.202/24"),
					},
				},
			)

			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{}))
		})
		ginkgo.It("deletes an existing namespace port group when there are no namespaces", func() {
			// this flag will create namespaced port group
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			pgIDs := getNamespacePortGroupDbIDs(namespaceName, DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			initialData := []libovsdb.TestData{pg}

			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{NBData: initialData},
				&v1.NodeList{
					Items: []v1.Node{
						*newNode("node1", "192.168.126.202/24"),
					},
				},
			)

			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{}))
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

			// namespace's address set deletion is delayed by 20 second to let other handlers cleanup
			gomega.Eventually(func() bool {
				return fakeOvn.asf.AddressSetExists(namespaceName)
			}, 21*time.Second).Should(gomega.BeFalse())
		})
	})
})

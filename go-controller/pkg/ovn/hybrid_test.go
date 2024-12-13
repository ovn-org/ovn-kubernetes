package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kapitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	cm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressqosfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"
	egressservicefake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

func newTestNode(name, os, ovnHostSubnet, hybridHostSubnet, drMAC string) v1.Node {
	var err error
	annotations := make(map[string]string)
	if ovnHostSubnet != "" {
		annotations, err = util.UpdateNodeHostSubnetAnnotation(annotations, ovntest.MustParseIPNets(ovnHostSubnet), types.DefaultNetworkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	if hybridHostSubnet != "" {
		annotations[hotypes.HybridOverlayNodeSubnet] = hybridHostSubnet
	}
	if drMAC != "" {
		annotations[hotypes.HybridOverlayDRMAC] = drMAC
	}
	return v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{v1.LabelOSStable: os},
			Annotations: annotations,
		},
	}
}

func newTestHONode(name, hybridHostSubnet, drMAC string) v1.Node {
	annotations := make(map[string]string)
	if hybridHostSubnet != "" {
		annotations[hotypes.HybridOverlayNodeSubnet] = hybridHostSubnet
	}
	if drMAC != "" {
		annotations[hotypes.HybridOverlayDRMAC] = drMAC
	}
	annotations[util.OvnNodeChassisID] = "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec"
	return v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{v1.LabelOSStable: "windows"},
			Annotations: annotations,
		},
	}
}

func setupHybridOverlayOVNObjects(node tNode, hoNodeName, hoSubnet, nodeHOIP, nodeHOMAC string) (*nbdb.LogicalRouterStaticRoute, *nbdb.LogicalRouterStaticRoute, *nbdb.LogicalRouterPolicy, *nbdb.LogicalRouterPolicy, *nbdb.LogicalSwitchPort) {
	name := types.HybridSubnetPrefix + node.Name
	if hoNodeName != "" {
		name = name + ":" + hoNodeName
	}
	hybridOverlayLRSR1 := &nbdb.LogicalRouterStaticRoute{
		UUID: types.HybridSubnetPrefix + node.Name + "LRSR1-UUID",
		ExternalIDs: map[string]string{
			"name": name,
		},
		IPPrefix: hoSubnet,
		Nexthop:  nodeHOIP,
	}
	hybridOverlayLRSR2 := &nbdb.LogicalRouterStaticRoute{
		UUID: types.HybridSubnetPrefix + node.Name + "-gr-UUID",
		ExternalIDs: map[string]string{
			"name": types.HybridSubnetPrefix + node.Name + types.HybridOverlayGRSubfix,
		},
		IPPrefix: hoSubnet,
		Nexthop:  node.DrLrpIP,
	}
	hybridOverlayLRP1 := &nbdb.LogicalRouterPolicy{
		UUID:   types.HybridSubnetPrefix + node.Name + "-LRP1-UUID",
		Action: "reroute",
		ExternalIDs: map[string]string{
			"name": name,
		},
		Match:    "inport == \"" + types.RouterToSwitchPrefix + node.Name + "\" && ip4.dst == " + hoSubnet,
		Nexthops: []string{nodeHOIP},
		Priority: types.HybridOverlaySubnetPriority,
	}
	hybridOverlayLRP2 := &nbdb.LogicalRouterPolicy{
		UUID:   types.HybridOverlayPrefix + node.Name + "-LRP2-UUID",
		Action: "reroute",
		ExternalIDs: map[string]string{
			"name": types.HybridSubnetPrefix + node.Name + types.HybridOverlayGRSubfix,
		},
		Match:    "ip4.src == " + node.LrpIP + " && ip4.dst == " + hoSubnet,
		Nexthops: []string{nodeHOIP},
		Priority: types.HybridOverlaySubnetPriority,
	}
	hybridOverlayLSP := &nbdb.LogicalSwitchPort{
		UUID:      types.HybridOverlayPrefix + node.Name + "-UUID",
		Name:      types.HybridOverlayPrefix + node.Name,
		Addresses: []string{nodeHOMAC},
	}
	return hybridOverlayLRSR1, hybridOverlayLRSR2, hybridOverlayLRP1, hybridOverlayLRP2, hybridOverlayLSP

}

var _ = ginkgo.Describe("Hybrid SDN Master Operations", func() {
	var (
		app             *cli.App
		stopChan        chan struct{}
		wg              *sync.WaitGroup
		fexec           *ovntest.FakeExec
		libovsdbCleanup *libovsdbtest.Context

		f *factory.WatchFactory
	)

	const (
		clusterIPNet string = "10.1.0.0"
		clusterCIDR  string = clusterIPNet + "/16"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}
		fexec = ovntest.NewFakeExec()
		err := util.SetExec(fexec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		libovsdbCleanup = nil
	})

	ginkgo.AfterEach(func() {

		close(stopChan)
		wg.Wait()
		if libovsdbCleanup != nil {
			libovsdbCleanup.Cleanup()
		}
		f.Shutdown()
		wg.Wait()
	})

	const hybridOverlayClusterCIDR string = "11.1.0.0/16/24"

	ginkgo.It("allocates and assigns a hybrid-overlay subnet to a Windows node that doesn't have one", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "11.1.0.0/24"
			)

			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			dbSetup := libovsdbtest.TestSetup{}
			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "windows", "", "", ""),
				},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			egressQoSFakeClient := &egressqosfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				EgressQoSClient:      egressQoSFakeClient,
			}

			f, err = factory.NewMasterWatchFactory(fakeClient.GetMasterClientset())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			clusterController, err := NewOvnController(
				fakeClient.GetMasterClientset(),
				f,
				stopChan,
				nil,
				networkmanager.Default().Interface(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				record.NewFakeRecorder(10),
				wg,
				nil,
				NewPortCache(stopChan),
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			c, cancel := context.WithCancel(ctx.Context)
			defer cancel()
			clusterManager, err := cm.NewClusterManager(fakeClient.GetClusterManagerClientset(), f, "identity", wg, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = clusterManager.Start(c)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer clusterManager.Stop()
			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())

			// Windows node should be allocated a subnet
			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(gomega.HaveKeyWithValue(hotypes.HybridOverlayNodeSubnet, nodeSubnet))

			gomega.Eventually(func() error {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				_, err = util.ParseNodeHostSubnetAnnotation(updatedNode, types.DefaultNetworkName)
				return err
			}, 2).Should(gomega.MatchError("could not find \"k8s.ovn.org/node-subnets\" annotation"))

			gomega.Eventually(fexec.CalledMatchesExpected, 2).Should(gomega.BeTrue(), fexec.ErrorDesc)

			// nothing should be done in OVN dbs from HO running on windows node
			gomega.Eventually(clusterController.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(dbSetup.NBData))
			gomega.Eventually(clusterController.sbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(dbSetup.SBData))

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-loglevel=5",
			"-no-hostsubnet-nodes=" + v1.LabelOSStable + "=windows",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
			"-init-gateways",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("sets up and cleans up a Linux node with a OVN hostsubnet annotation", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeHOMAC string = "0a:58:0a:01:01:03"
				hoSubnet  string = "11.1.0.0/16"
				nodeHOIP  string = "10.1.1.3"
			)
			node1 := tNode{
				Name:                 "node1",
				NodeIP:               "1.2.3.4",
				NodeLRPMAC:           "0a:58:0a:01:01:01",
				LrpIP:                "100.64.0.2",
				DrLrpIP:              "100.64.0.1",
				PhysicalBridgeMAC:    "11:22:33:44:55:66",
				SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				NodeSubnet:           "10.1.1.0/24",
				GWRouter:             types.GWRouterPrefix + "node1",
				GatewayRouterIPMask:  "172.16.16.2/24",
				GatewayRouterIP:      "172.16.16.2",
				GatewayRouterNextHop: "172.16.16.1",
				PhysicalBridgeName:   "br-eth0",
				NodeGWIP:             "10.1.1.1/24",
				NodeMgmtPortIP:       "10.1.1.2",
				//NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
				NodeMgmtPortMAC: "0a:58:64:40:00:03",
				DnatSnatIP:      "169.254.0.1",
			}
			testNode := node1.k8sNode("2")

			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			egressQoSFakeClient := &egressqosfake.Clientset{}
			egressServiceFakeClient := &egressservicefake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				EgressQoSClient:      egressQoSFakeClient,
				EgressServiceClient:  egressServiceFakeClient,
			}

			vlanID := 1024
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.Kubernetes.HostNetworkNamespace = ""
			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, testNode.Name)
			l3Config := node1.gatewayConfig(config.GatewayModeShared, uint(vlanID))
			err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.UpdateNodeManagementPortMACAddresses(&testNode, nodeAnnotator,
				ovntest.MustParseMAC(node1.NodeMgmtPortMAC), types.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostCIDRs(nodeAnnotator, sets.New(fmt.Sprintf("%s/24", node1.NodeIP)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostAddrs, err := util.ParseNodeHostCIDRsDropNetMask(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f, err = factory.NewMasterWatchFactory(fakeClient.GetMasterClientset())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedClusterLBGroup := newLoadBalancerGroup(types.ClusterLBGroupName)
			expectedSwitchLBGroup := newLoadBalancerGroup(types.ClusterSwitchLBGroupName)
			expectedRouterLBGroup := newLoadBalancerGroup(types.ClusterRouterLBGroupName)
			expectedOVNClusterRouter := newOVNClusterRouter()
			ovnClusterRouterLRP := &nbdb.LogicalRouterPort{
				Name:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter,
				Networks: []string{"100.64.0.1/16"},
				UUID:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter + "-UUID",
			}
			expectedOVNClusterRouter.Ports = []string{ovnClusterRouterLRP.UUID}
			expectedNodeSwitch := node1.logicalSwitch([]string{expectedClusterLBGroup.UUID, expectedSwitchLBGroup.UUID})
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			dbSetup := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					newClusterJoinSwitch(),
					expectedNodeSwitch,
					ovnClusterRouterLRP,
					expectedOVNClusterRouter,
					expectedClusterRouterPortGroup,
					expectedClusterPortGroup,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			}
			var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedDatabaseState := []libovsdbtest.TestData{ovnClusterRouterLRP}
			expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1)

			clusterController, err := NewOvnController(
				fakeClient.GetMasterClientset(),
				f,
				stopChan,
				nil,
				networkmanager.Default().Interface(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				record.NewFakeRecorder(10),
				wg,
				nil,
				NewPortCache(stopChan),
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			setupCOPP := true
			setupClusterController(clusterController, setupCOPP)

			//assuming all the pods have finished processing
			atomic.StoreUint32(&clusterController.allInitialPodsProcessed, 1)

			c, cancel := context.WithCancel(ctx.Context)
			defer cancel()
			clusterManager, err := cm.NewClusterManager(fakeClient.GetClusterManagerClientset(), f, "identity", wg, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(clusterManager).NotTo(gomega.BeNil())
			err = clusterManager.Start(c)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer clusterManager.Stop()

			// Let the real code run and ensure OVN database sync
			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())

			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(gomega.HaveKeyWithValue(hotypes.HybridOverlayDRMAC, nodeHOMAC))

			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(gomega.HaveKeyWithValue(hotypes.HybridOverlayDRIP, nodeHOIP))

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			err = clusterController.syncDefaultGatewayLogicalNetwork(updatedNode, l3GatewayConfig, []*net.IPNet{subnet}, hostAddrs.UnsortedList())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var clusterSubnets []*net.IPNet
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
			}

			skipSnat := false
			expectedDatabaseState = generateGatewayInitExpectedNB(expectedDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3Config,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)}, skipSnat,
				node1.NodeMgmtPortIP, "1400")

			hybridSubnetStaticRoute1, hybridLogicalRouterStaticRoute, hybridSubnetLRP1, hybridSubnetLRP2, hybridLogicalSwitchPort := setupHybridOverlayOVNObjects(node1, "", hoSubnet, nodeHOIP, nodeHOMAC)

			for _, obj := range expectedDatabaseState {
				if logicalRouter, ok := obj.(*nbdb.LogicalRouter); ok {
					if logicalRouter.Name == "GR_node1" {
						logicalRouter.StaticRoutes = append(logicalRouter.StaticRoutes, hybridLogicalRouterStaticRoute.UUID)
					}
				}
			}

			expectedNodeSwitch.Ports = append(expectedNodeSwitch.Ports, hybridLogicalSwitchPort.UUID)
			expectedOVNClusterRouter.Policies = append(expectedOVNClusterRouter.Policies, hybridSubnetLRP1.UUID, hybridSubnetLRP2.UUID)
			expectedOVNClusterRouter.StaticRoutes = append(expectedOVNClusterRouter.StaticRoutes, hybridSubnetStaticRoute1.UUID)

			expectedDatabaseStateWithHybridNode := append([]libovsdbtest.TestData{hybridSubnetStaticRoute1, hybridSubnetLRP2, hybridSubnetLRP1, hybridLogicalSwitchPort, hybridLogicalRouterStaticRoute}, expectedDatabaseState...)
			expectedStaticMACBinding := &nbdb.StaticMACBinding{
				UUID:               "MAC-binding-HO-UUID",
				IP:                 nodeHOIP,
				LogicalPort:        "rtos-node1",
				MAC:                nodeHOMAC,
				OverrideDynamicMAC: true,
			}
			expectedDatabaseStateWithHybridNode = append(expectedDatabaseStateWithHybridNode, expectedStaticMACBinding)
			gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDatabaseStateWithHybridNode))

			err = fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// the best way to check if a node is deleted is to check some of the explicitly deleted elements
			gomega.Eventually(func() ([]string, error) {
				clusterRouter, err := libovsdbops.GetLogicalRouter(clusterController.nbClient, &nbdb.LogicalRouter{Name: types.OVNClusterRouter})
				if err != nil {
					return nil, err
				}
				return clusterRouter.Policies, nil
			}, 2).Should(gomega.HaveLen(0))

			gomega.Eventually(func() error {
				_, err := libovsdbops.GetLogicalSwitchPort(clusterController.nbClient, &nbdb.LogicalSwitchPort{Name: "jtor-GR_node1"})
				if err != nil {
					return err
				}
				return nil

			}, 2).Should(gomega.Equal(libovsdbclient.ErrNotFound))

			gomega.Eventually(func() ([]string, error) {
				ovnJoinSwitch, err := libovsdbops.GetLogicalSwitch(clusterController.nbClient, &nbdb.LogicalSwitch{Name: "join"})
				if err != nil {
					return nil, err
				}
				return ovnJoinSwitch.Ports, err

			}, 2).Should(gomega.HaveLen(0))

			//check if the hybrid overlay elements have been cleaned up
			gomega.Eventually(func() ([]*nbdb.LogicalRouterStaticRoute, error) {
				p := func(item *nbdb.LogicalRouterStaticRoute) bool {
					if item.ExternalIDs["name"] == "hybrid-subnet-node1-gr" ||
						strings.Contains(item.ExternalIDs["name"], "hybrid-subnet-node1") {
						return true
					}
					return false
				}
				logicalRouterStaticRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(clusterController.nbClient, p)
				if err != nil {
					return nil, err
				}
				return logicalRouterStaticRoutes, nil
			}, 2).Should(gomega.HaveLen(0))

			gomega.Eventually(func() ([]*nbdb.LogicalRouterPolicy, error) {
				p := func(item *nbdb.LogicalRouterPolicy) bool {
					if strings.Contains(item.ExternalIDs["name"], "hybrid-subnet-node1") ||
						item.ExternalIDs["name"] == "hybrid-subnet-node1-gr" {
						return true
					}
					return false
				}
				logicalRouterPolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(clusterController.nbClient, p)
				if err != nil {
					return nil, err
				}
				return logicalRouterPolicies, nil

			}, 2).Should(gomega.HaveLen(0))

			gomega.Eventually(func() error {
				_, err := libovsdbops.GetLogicalSwitchPort(clusterController.nbClient, &nbdb.LogicalSwitchPort{Name: "int-node1"})
				if err != nil {
					return err
				}
				return nil
			}, 2).Should(gomega.Equal(libovsdbclient.ErrNotFound))

			gomega.Eventually(clusterController.nbClient.Get(context.Background(), expectedStaticMACBinding), 2).Should(gomega.Equal(libovsdbclient.ErrNotFound))

			return nil
		}
		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-gateway-mode=shared",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("handles a Linux node with no annotation but an existing port and lrp", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeHOMAC string = "0a:58:0a:01:01:03"
				hoSubnet  string = "11.1.0.0/16"
				nodeHOIP  string = "10.1.1.3"
			)
			node1 := tNode{
				Name:                 "node1",
				NodeIP:               "1.2.3.4",
				NodeLRPMAC:           "0a:58:0a:01:01:01",
				LrpIP:                "100.64.0.2",
				DrLrpIP:              "100.64.0.1",
				PhysicalBridgeMAC:    "11:22:33:44:55:66",
				SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				NodeSubnet:           "10.1.1.0/24",
				GWRouter:             types.GWRouterPrefix + "node1",
				GatewayRouterIPMask:  "172.16.16.2/24",
				GatewayRouterIP:      "172.16.16.2",
				GatewayRouterNextHop: "172.16.16.1",
				PhysicalBridgeName:   "br-eth0",
				NodeGWIP:             "10.1.1.1/24",
				NodeMgmtPortIP:       "10.1.1.2",
				NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
				DnatSnatIP:           "169.254.0.1",
			}

			testNode := node1.k8sNode("2")

			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			egressQoSFakeClient := &egressqosfake.Clientset{}
			egressServiceFakeClient := &egressservicefake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				EgressQoSClient:      egressQoSFakeClient,
				EgressServiceClient:  egressServiceFakeClient,
			}

			vlanID := 1024
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.Kubernetes.HostNetworkNamespace = ""
			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, testNode.Name)
			l3Config := node1.gatewayConfig(config.GatewayModeShared, uint(vlanID))
			err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.UpdateNodeManagementPortMACAddresses(&testNode, nodeAnnotator,
				ovntest.MustParseMAC(node1.NodeMgmtPortMAC), types.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostCIDRs(nodeAnnotator, sets.New(fmt.Sprintf("%s/24", node1.NodeIP)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostAddrs, err := util.ParseNodeHostCIDRsDropNetMask(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f, err = factory.NewMasterWatchFactory(fakeClient.GetMasterClientset())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedClusterLBGroup := newLoadBalancerGroup(types.ClusterLBGroupName)
			expectedSwitchLBGroup := newLoadBalancerGroup(types.ClusterSwitchLBGroupName)
			expectedOVNClusterRouter := newOVNClusterRouter()
			ovnClusterRouterLRP := &nbdb.LogicalRouterPort{
				Name:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter,
				Networks: []string{"100.64.0.1/16"},
				UUID:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter + "-UUID",
			}
			expectedOVNClusterRouter.Ports = []string{ovnClusterRouterLRP.UUID}
			expectedNodeSwitch := node1.logicalSwitch([]string{expectedClusterLBGroup.UUID, expectedSwitchLBGroup.UUID})
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			expectedDatabaseState := []libovsdbtest.TestData{ovnClusterRouterLRP}
			expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1)

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)

			var clusterSubnets []*net.IPNet
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
			}

			skipSnat := false
			expectedDatabaseState = generateGatewayInitExpectedNB(expectedDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3Config,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)}, skipSnat,
				node1.NodeMgmtPortIP, "1400")

			hybridSubnetStaticRoute1, hybridLogicalRouterStaticRoute, hybridSubnetLRP1, hybridSubnetLRP2, hybridLogicalSwitchPort := setupHybridOverlayOVNObjects(node1, "", hoSubnet, nodeHOIP, nodeHOMAC)

			for _, obj := range expectedDatabaseState {
				if logicalRouter, ok := obj.(*nbdb.LogicalRouter); ok {
					if logicalRouter.Name == "GR_node1" {
						logicalRouter.StaticRoutes = append(logicalRouter.StaticRoutes, hybridLogicalRouterStaticRoute.UUID)
					}
				}
			}

			expectedNodeSwitch.Ports = append(expectedNodeSwitch.Ports, hybridLogicalSwitchPort.UUID)
			expectedOVNClusterRouter.Policies = append(expectedOVNClusterRouter.Policies, hybridSubnetLRP1.UUID, hybridSubnetLRP2.UUID)
			expectedOVNClusterRouter.StaticRoutes = append(expectedOVNClusterRouter.StaticRoutes, hybridSubnetStaticRoute1.UUID)

			expectedDatabaseStateWithHybridNode := append([]libovsdbtest.TestData{hybridSubnetStaticRoute1, hybridSubnetLRP2, hybridSubnetLRP1, hybridLogicalSwitchPort, hybridLogicalRouterStaticRoute}, expectedDatabaseState...)
			expectedStaticMACBinding := &nbdb.StaticMACBinding{
				UUID:               "MAC-binding-HO-UUID",
				IP:                 nodeHOIP,
				LogicalPort:        "rtos-node1",
				MAC:                nodeHOMAC,
				OverrideDynamicMAC: true,
			}
			expectedDatabaseStateWithHybridNode = append(expectedDatabaseStateWithHybridNode, expectedStaticMACBinding)

			dbSetup := libovsdbtest.TestSetup{
				NBData: expectedDatabaseStateWithHybridNode,
			}
			var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			clusterController, err := NewOvnController(
				fakeClient.GetMasterClientset(),
				f,
				stopChan,
				nil,
				networkmanager.Default().Interface(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				record.NewFakeRecorder(10),
				wg,
				nil,
				NewPortCache(stopChan),
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			setupCOPP := true
			setupClusterController(clusterController, setupCOPP)

			err = clusterController.syncDefaultGatewayLogicalNetwork(updatedNode, l3GatewayConfig, []*net.IPNet{subnet}, hostAddrs.UnsortedList())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			//assuming all the pods have finished processing
			atomic.StoreUint32(&clusterController.allInitialPodsProcessed, 1)
			// Let the real code run and ensure OVN database sync

			c, cancel := context.WithCancel(ctx.Context)
			defer cancel()
			clusterManager, err := cm.NewClusterManager(fakeClient.GetClusterManagerClientset(), f, "identity", wg, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(clusterManager).NotTo(gomega.BeNil())
			err = clusterManager.Start(c)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer clusterManager.Stop()

			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())

			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(gomega.HaveKeyWithValue(hotypes.HybridOverlayDRMAC, nodeHOMAC))

			gomega.Consistently(libovsdbOvnNBClient, 2).Should(libovsdbtest.HaveData(expectedDatabaseStateWithHybridNode))

			return nil
		}
		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-gateway-mode=shared",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("cluster handles a linux node when hybridOverlayClusterCIDR in unset but the HO annotations are available on windows nodes", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				//linNodeName   string = "node-linux"
				winNodeName string = "node-windows"
				//linNodeSubnet string = "10.1.2.0/24"
				winNodeSubnet string = "10.1.3.0/24"
				//linNodeHOIP   string = "10.1.2.3"
				//linNodeHOMAC  string = "0a:58:0a:01:02:03"
				nodeHOMAC string = "0a:58:0a:01:01:03"
				hoSubnet  string = "11.1.0.0/16"
				nodeHOIP  string = "10.1.1.3"
			)
			node1 := tNode{
				Name:                 "node1",
				NodeIP:               "1.2.3.4",
				NodeLRPMAC:           "0a:58:0a:01:01:01",
				LrpIP:                "100.64.0.2",
				DrLrpIP:              "100.64.0.1",
				PhysicalBridgeMAC:    "11:22:33:44:55:66",
				SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				NodeSubnet:           "10.1.1.0/24",
				GWRouter:             types.GWRouterPrefix + "node1",
				GatewayRouterIPMask:  "172.16.16.2/24",
				GatewayRouterIP:      "172.16.16.2",
				GatewayRouterNextHop: "172.16.16.1",
				PhysicalBridgeName:   "br-eth0",
				NodeGWIP:             "10.1.1.1/24",
				NodeMgmtPortIP:       "10.1.1.2",
				NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
				DnatSnatIP:           "169.254.0.1",
			}
			testNode := node1.k8sNode("2")

			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{

					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   winNodeName,
							Labels: map[string]string{v1.LabelOSStable: "windows"},
							Annotations: map[string]string{
								hotypes.HybridOverlayNodeSubnet: winNodeSubnet,
							},
						},
					},
					testNode,
				},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			egressQoSFakeClient := &egressqosfake.Clientset{}
			egressServiceFakeClient := &egressservicefake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				EgressQoSClient:      egressQoSFakeClient,
				EgressServiceClient:  egressServiceFakeClient,
			}

			vlanID := 1024
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.Kubernetes.HostNetworkNamespace = ""
			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, testNode.Name)
			l3Config := node1.gatewayConfig(config.GatewayModeShared, uint(vlanID))
			err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.UpdateNodeManagementPortMACAddresses(&testNode, nodeAnnotator,
				ovntest.MustParseMAC(node1.NodeMgmtPortMAC), types.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostCIDRs(nodeAnnotator, sets.New(fmt.Sprintf("%s/24", node1.NodeIP)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostAddrs, err := util.ParseNodeHostCIDRsDropNetMask(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f, err = factory.NewMasterWatchFactory(fakeClient.GetMasterClientset())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedClusterLBGroup := newLoadBalancerGroup(types.ClusterLBGroupName)
			expectedSwitchLBGroup := newLoadBalancerGroup(types.ClusterSwitchLBGroupName)
			expectedRouterLBGroup := newLoadBalancerGroup(types.ClusterRouterLBGroupName)
			expectedOVNClusterRouter := newOVNClusterRouter()
			ovnClusterRouterLRP := &nbdb.LogicalRouterPort{
				Name:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter,
				Networks: []string{"100.64.0.1/16"},
				UUID:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter + "-UUID",
			}
			expectedOVNClusterRouter.Ports = []string{ovnClusterRouterLRP.UUID}
			expectedNodeSwitch := node1.logicalSwitch([]string{expectedClusterLBGroup.UUID, expectedSwitchLBGroup.UUID})
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			dbSetup := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					newClusterJoinSwitch(),
					expectedNodeSwitch,
					ovnClusterRouterLRP,
					expectedOVNClusterRouter,
					expectedClusterRouterPortGroup,
					expectedClusterPortGroup,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			}
			var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedDatabaseState := []libovsdbtest.TestData{ovnClusterRouterLRP}
			expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1)

			clusterController, err := NewOvnController(
				fakeClient.GetMasterClientset(),
				f,
				stopChan,
				nil,
				networkmanager.Default().Interface(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				record.NewFakeRecorder(10),
				wg,
				nil,
				NewPortCache(stopChan),
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			setupCOPP := true
			setupClusterController(clusterController, setupCOPP)

			//assuming all the pods have finished processing
			atomic.StoreUint32(&clusterController.allInitialPodsProcessed, 1)
			// Let the real code run and ensure OVN database sync

			c, cancel := context.WithCancel(ctx.Context)
			defer cancel()
			clusterManager, err := cm.NewClusterManager(fakeClient.GetClusterManagerClientset(), f, "identity", wg, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(clusterManager).NotTo(gomega.BeNil())
			err = clusterManager.Start(c)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer clusterManager.Stop()

			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())

			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(gomega.HaveKeyWithValue(hotypes.HybridOverlayDRMAC, nodeHOMAC))

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			err = clusterController.syncDefaultGatewayLogicalNetwork(updatedNode, l3GatewayConfig, []*net.IPNet{subnet}, hostAddrs.UnsortedList())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var clusterSubnets []*net.IPNet
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
			}

			skipSnat := false
			expectedDatabaseState = generateGatewayInitExpectedNB(expectedDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3Config,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)}, skipSnat,
				node1.NodeMgmtPortIP, "1400")

			hybridSubnetStaticRoute1, hybridLogicalRouterStaticRoute, hybridSubnetLRP1, hybridSubnetLRP2, hybridLogicalSwitchPort := setupHybridOverlayOVNObjects(node1, winNodeName, winNodeSubnet, nodeHOIP, nodeHOMAC)

			for _, obj := range expectedDatabaseState {
				if logicalRouter, ok := obj.(*nbdb.LogicalRouter); ok {
					if logicalRouter.Name == "GR_node1" {
						logicalRouter.StaticRoutes = append(logicalRouter.StaticRoutes, hybridLogicalRouterStaticRoute.UUID)
					}
				}
			}

			expectedNodeSwitch.Ports = append(expectedNodeSwitch.Ports, hybridLogicalSwitchPort.UUID)
			expectedOVNClusterRouter.Policies = append(expectedOVNClusterRouter.Policies, hybridSubnetLRP1.UUID, hybridSubnetLRP2.UUID)
			expectedOVNClusterRouter.StaticRoutes = append(expectedOVNClusterRouter.StaticRoutes, hybridSubnetStaticRoute1.UUID)

			expectedDatabaseStateWithHybridNode := append([]libovsdbtest.TestData{hybridSubnetStaticRoute1, hybridSubnetLRP2, hybridSubnetLRP1, hybridLogicalSwitchPort, hybridLogicalRouterStaticRoute}, expectedDatabaseState...)
			expectedStaticMACBinding := &nbdb.StaticMACBinding{
				UUID:               "MAC-binding-HO-UUID",
				IP:                 nodeHOIP,
				LogicalPort:        "rtos-node1",
				MAC:                nodeHOMAC,
				OverrideDynamicMAC: true,
			}
			expectedDatabaseStateWithHybridNode = append(expectedDatabaseStateWithHybridNode, expectedStaticMACBinding)
			gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDatabaseStateWithHybridNode))

			err = fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// the best way to check if a node is deleted is to check some of the explicitly deleted elements
			gomega.Eventually(func() ([]string, error) {
				clusterRouter, err := libovsdbops.GetLogicalRouter(clusterController.nbClient, &nbdb.LogicalRouter{Name: types.OVNClusterRouter})
				if err != nil {
					return nil, err
				}
				return clusterRouter.Policies, nil
			}, 20).Should(gomega.HaveLen(0))

			gomega.Eventually(func() error {
				_, err := libovsdbops.GetLogicalSwitchPort(clusterController.nbClient, &nbdb.LogicalSwitchPort{Name: "jtor-GR_node1"})
				if err != nil {
					return err
				}
				return nil

			}, 2).Should(gomega.Equal(libovsdbclient.ErrNotFound))

			gomega.Eventually(func() ([]string, error) {
				ovnJoinSwitch, err := libovsdbops.GetLogicalSwitch(clusterController.nbClient, &nbdb.LogicalSwitch{Name: "join"})
				if err != nil {
					return nil, err
				}
				return ovnJoinSwitch.Ports, err

			}, 2).Should(gomega.HaveLen(0))

			//check if the hybrid overlay elements have been cleaned up
			gomega.Eventually(func() ([]*nbdb.LogicalRouterStaticRoute, error) {
				p := func(item *nbdb.LogicalRouterStaticRoute) bool {
					if item.ExternalIDs["name"] == "hybrid-subnet-node1-gr" ||
						strings.Contains(item.ExternalIDs["name"], "hybrid-subnet-node1") {
						return true
					}
					return false
				}
				logicalRouterStaticRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(clusterController.nbClient, p)
				if err != nil {
					return nil, err
				}
				return logicalRouterStaticRoutes, nil
			}, 2).Should(gomega.HaveLen(0))

			gomega.Eventually(func() ([]*nbdb.LogicalRouterPolicy, error) {
				p := func(item *nbdb.LogicalRouterPolicy) bool {
					if strings.Contains(item.ExternalIDs["name"], "hybrid-subnet-node1") ||
						item.ExternalIDs["name"] == "hybrid-subnet-node1-gr" {
						return true
					}
					return false
				}
				logicalRouterPolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(clusterController.nbClient, p)
				if err != nil {
					return nil, err
				}
				return logicalRouterPolicies, nil

			}, 2).Should(gomega.HaveLen(0))

			gomega.Eventually(func() error {
				_, err := libovsdbops.GetLogicalSwitchPort(clusterController.nbClient, &nbdb.LogicalSwitchPort{Name: "int-node1"})
				if err != nil {
					return err
				}
				return nil
			}, 2).Should(gomega.Equal(libovsdbclient.ErrNotFound))

			return nil
		}
		err := app.Run([]string{
			app.Name,
			"--no-hostsubnet-nodes=kubernetes.io/os=windows",
			"-cluster-subnets=" + clusterCIDR,
			"-gateway-mode=shared",
			"-enable-hybrid-overlay",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

	})

	ginkgo.It("handles a OVN node is switched to a HO node", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				hoNodeName   string = "node2"
				hoNodeSubnet string = "10.1.3.0/24"
				hoNodeDRMAC  string = "00:7f:0e:7f:ed:b5"
				nodeHOMAC    string = "0a:58:0a:01:01:03"
				nodeHOIP     string = "10.1.1.3"
			)
			node1 := tNode{
				Name:                 "node1",
				NodeIP:               "1.2.3.4",
				NodeLRPMAC:           "0a:58:0a:01:01:01",
				LrpIP:                "100.64.0.2",
				DrLrpIP:              "100.64.0.1",
				PhysicalBridgeMAC:    "11:22:33:44:55:66",
				SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				NodeSubnet:           "10.1.1.0/24",
				GWRouter:             types.GWRouterPrefix + "node1",
				GatewayRouterIPMask:  "172.16.16.2/24",
				GatewayRouterIP:      "172.16.16.2",
				GatewayRouterNextHop: "172.16.16.1",
				PhysicalBridgeName:   "br-eth0",
				NodeGWIP:             "10.1.1.1/24",
				NodeMgmtPortIP:       "10.1.1.2",
				NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
				DnatSnatIP:           "169.254.0.1",
			}

			node2 := tNode{
				Name:                 hoNodeName,
				NodeIP:               "1.2.3.5",
				NodeLRPMAC:           "0a:58:0a:01:02:01",
				LrpIP:                "100.64.0.3",
				DrLrpIP:              "100.64.0.1",
				PhysicalBridgeMAC:    "00:7f:0e:7f:ed:b5",
				SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47abc6",
				NodeSubnet:           hoNodeSubnet,
				GWRouter:             types.GWRouterPrefix + "node2",
				GatewayRouterIPMask:  "172.16.16.2/24",
				GatewayRouterIP:      "172.16.16.2",
				GatewayRouterNextHop: "172.16.16.1",
				PhysicalBridgeName:   "br-eth0",
				NodeGWIP:             "10.1.3.1/24",
				NodeMgmtPortIP:       "10.1.3.2",
				NodeMgmtPortMAC:      "0a:58:0a:01:02:02",
				DnatSnatIP:           "169.254.0.1",
			}
			testNode1 := node1.k8sNode("2")
			testNode2 := node2.k8sNode("3")
			testNode2.Annotations["k8s.ovn.org/node-subnets"] = "{\"default\":[\"10.1.3.0/24\"]}"

			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					testNode1,
					testNode2,
				},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			egressQoSFakeClient := &egressqosfake.Clientset{}
			egressServiceFakeClient := &egressservicefake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				EgressQoSClient:      egressQoSFakeClient,
				EgressServiceClient:  egressServiceFakeClient,
			}

			vlanID := 1024
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.Kubernetes.HostNetworkNamespace = ""
			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, testNode1.Name)
			l3Config := node1.gatewayConfig(config.GatewayModeShared, uint(vlanID))
			err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.UpdateNodeManagementPortMACAddresses(&testNode1, nodeAnnotator,
				ovntest.MustParseMAC(node1.NodeMgmtPortMAC), types.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostCIDRs(nodeAnnotator, sets.New(fmt.Sprintf("%s/24", node1.NodeIP)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode1.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostAddrs, err := util.ParseNodeHostCIDRsDropNetMask(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f, err = factory.NewMasterWatchFactory(fakeClient.GetMasterClientset())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedClusterLBGroup := newLoadBalancerGroup(types.ClusterLBGroupName)
			expectedSwitchLBGroup := newLoadBalancerGroup(types.ClusterSwitchLBGroupName)
			expectedRouterLBGroup := newLoadBalancerGroup(types.ClusterRouterLBGroupName)
			expectedOVNClusterRouter := newOVNClusterRouter()
			ovnClusterRouterLRP := &nbdb.LogicalRouterPort{
				Name:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter,
				Networks: []string{"100.64.0.1/16"},
				UUID:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter + "-UUID",
			}
			expectedOVNClusterRouter.Ports = []string{ovnClusterRouterLRP.UUID}
			expectedNodeSwitch := node1.logicalSwitch([]string{expectedClusterLBGroup.UUID, expectedSwitchLBGroup.UUID})
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			dbSetup := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					newClusterJoinSwitch(),
					expectedNodeSwitch,
					ovnClusterRouterLRP,
					expectedOVNClusterRouter,
					expectedClusterRouterPortGroup,
					expectedClusterPortGroup,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			}
			var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			clusterController, err := NewOvnController(
				fakeClient.GetMasterClientset(),
				f,
				stopChan,
				nil,
				networkmanager.Default().Interface(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				record.NewFakeRecorder(10),
				wg,
				nil,
				NewPortCache(stopChan),
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			setupCOPP := true
			setupClusterController(clusterController, setupCOPP)

			//assuming all the pods have finished processing
			atomic.StoreUint32(&clusterController.allInitialPodsProcessed, 1)
			// Let the real code run and ensure OVN database sync

			c, cancel := context.WithCancel(ctx.Context)
			defer cancel()
			clusterManager, err := cm.NewClusterManager(fakeClient.GetClusterManagerClientset(), f, "identity", wg, nil)
			gomega.Expect(clusterManager).NotTo(gomega.BeNil())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = clusterManager.Start(c)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer clusterManager.Stop()

			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())

			// switch the node to a HO node
			testNode2.Labels = map[string]string{v1.LabelOSStable: "windows"}
			testNode2.Annotations[hotypes.HybridOverlayNodeSubnet] = hoNodeSubnet
			testNode2.Annotations[hotypes.HybridOverlayDRMAC] = hoNodeDRMAC
			_, err = fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &testNode2, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode1.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(gomega.HaveKeyWithValue(hotypes.HybridOverlayDRMAC, nodeHOMAC))

			//ensure hybrid overlay elements have been added
			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			err = clusterController.syncDefaultGatewayLogicalNetwork(updatedNode, l3GatewayConfig, []*net.IPNet{subnet}, hostAddrs.UnsortedList())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() ([]*nbdb.LogicalRouterStaticRoute, error) {
				p := func(item *nbdb.LogicalRouterStaticRoute) bool {
					return item.ExternalIDs["name"] == "hybrid-subnet-node1-gr" &&
						item.Nexthop == "100.64.0.1" &&
						item.IPPrefix == hoNodeSubnet
				}
				logicalRouterStaticRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(clusterController.nbClient, p)
				if err != nil {
					return nil, err
				}
				return logicalRouterStaticRoutes, nil
			}, 2).Should(gomega.HaveLen(1))

			gomega.Eventually(func() ([]*nbdb.LogicalRouterStaticRoute, error) {
				p := func(item *nbdb.LogicalRouterStaticRoute) bool {
					return strings.Contains(item.ExternalIDs["name"], "hybrid-subnet-node1") &&
						item.Nexthop == nodeHOIP &&
						item.IPPrefix == hoNodeSubnet
				}
				logicalRouterStaticRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(clusterController.nbClient, p)
				if err != nil {
					return nil, err
				}
				return logicalRouterStaticRoutes, nil
			}, 2).Should(gomega.HaveLen(1))

			gomega.Eventually(func() ([]*nbdb.LogicalRouterPolicy, error) {
				p := func(item *nbdb.LogicalRouterPolicy) bool {
					return item.ExternalIDs["name"] == "hybrid-subnet-node1-gr" &&
						item.Match == fmt.Sprintf("ip4.src == 100.64.0.2 && ip4.dst == %s", hoNodeSubnet) &&
						item.Action == "reroute" &&
						item.Nexthops[0] == nodeHOIP
				}
				logicalRouterPolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(clusterController.nbClient, p)
				if err != nil {
					return nil, err
				}
				return logicalRouterPolicies, nil
			}, 2).Should(gomega.HaveLen(1))
			return nil
		}
		err := app.Run([]string{
			app.Name,
			"--no-hostsubnet-nodes=kubernetes.io/os=windows",
			"-cluster-subnets=" + clusterCIDR,
			"-gateway-mode=shared",
			"-enable-hybrid-overlay",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("handles a HO node is switched to a OVN node", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				hoNodeName   string = "node-windows"
				hoNodeSubnet string = "10.1.3.0/24"
				hoNodeDRMAC  string = "00:7f:0e:7f:ed:b5"
				nodeHOMAC    string = "0a:58:0a:01:01:03"
				hoSubnet     string = "11.1.0.0/16"
				nodeHOIP     string = "10.1.1.3"
			)
			node1 := tNode{
				Name:                 "node1",
				NodeIP:               "1.2.3.4",
				NodeLRPMAC:           "0a:58:0a:01:01:01",
				LrpIP:                "100.64.0.2",
				DrLrpIP:              "100.64.0.1",
				PhysicalBridgeMAC:    "11:22:33:44:55:66",
				SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				NodeSubnet:           "10.1.1.0/24",
				GWRouter:             types.GWRouterPrefix + "node1",
				GatewayRouterIPMask:  "172.16.16.2/24",
				GatewayRouterIP:      "172.16.16.2",
				GatewayRouterNextHop: "172.16.16.1",
				PhysicalBridgeName:   "br-eth0",
				NodeGWIP:             "10.1.1.1/24",
				NodeMgmtPortIP:       "10.1.1.2",
				NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
				DnatSnatIP:           "169.254.0.1",
			}
			testNode := node1.k8sNode("2")
			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestHONode(hoNodeName, hoNodeSubnet, hoNodeDRMAC),
					testNode,
				},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			egressQoSFakeClient := &egressqosfake.Clientset{}
			egressServiceFakeClient := &egressservicefake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				EgressQoSClient:      egressQoSFakeClient,
				EgressServiceClient:  egressServiceFakeClient,
			}

			vlanID := 1024
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.Kubernetes.HostNetworkNamespace = ""
			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, testNode.Name)
			l3Config := node1.gatewayConfig(config.GatewayModeShared, uint(vlanID))
			err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.UpdateNodeManagementPortMACAddresses(&testNode, nodeAnnotator,
				ovntest.MustParseMAC(node1.NodeMgmtPortMAC), types.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostCIDRs(nodeAnnotator, sets.New(fmt.Sprintf("%s/24", node1.NodeIP)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostAddrs, err := util.ParseNodeHostCIDRsDropNetMask(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f, err = factory.NewMasterWatchFactory(fakeClient.GetMasterClientset())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedClusterLBGroup := newLoadBalancerGroup(types.ClusterLBGroupName)
			expectedSwitchLBGroup := newLoadBalancerGroup(types.ClusterSwitchLBGroupName)
			expectedRouterLBGroup := newLoadBalancerGroup(types.ClusterRouterLBGroupName)
			expectedOVNClusterRouter := newOVNClusterRouter()
			ovnClusterRouterLRP := &nbdb.LogicalRouterPort{
				Name:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter,
				Networks: []string{"100.64.0.1/16"},
				UUID:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter + "-UUID",
			}
			expectedOVNClusterRouter.Ports = []string{ovnClusterRouterLRP.UUID}
			expectedNodeSwitch := node1.logicalSwitch([]string{expectedClusterLBGroup.UUID, expectedSwitchLBGroup.UUID})
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			dbSetup := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					newClusterJoinSwitch(),
					expectedNodeSwitch,
					ovnClusterRouterLRP,
					expectedOVNClusterRouter,
					expectedClusterRouterPortGroup,
					expectedClusterPortGroup,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			}
			var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			clusterController, err := NewOvnController(
				fakeClient.GetMasterClientset(),
				f,
				stopChan,
				nil,
				networkmanager.Default().Interface(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				record.NewFakeRecorder(10),
				wg,
				nil,
				NewPortCache(stopChan),
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			setupCOPP := true
			setupClusterController(clusterController, setupCOPP)

			//assuming all the pods have finished processing
			atomic.StoreUint32(&clusterController.allInitialPodsProcessed, 1)
			// Let the real code run and ensure OVN database sync

			c, cancel := context.WithCancel(ctx.Context)
			defer cancel()
			clusterManager, err := cm.NewClusterManager(fakeClient.GetClusterManagerClientset(), f, "identity", wg, nil)
			gomega.Expect(clusterManager).NotTo(gomega.BeNil())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = clusterManager.Start(c)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer clusterManager.Stop()

			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())

			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(gomega.HaveKeyWithValue(hotypes.HybridOverlayDRMAC, nodeHOMAC))

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			err = clusterController.syncDefaultGatewayLogicalNetwork(updatedNode, l3GatewayConfig, []*net.IPNet{subnet}, hostAddrs.UnsortedList())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// switch the node to a ovn node
			ginkgo.By("Removing the windows node label and switching to OVN node")
			patch := []map[string]string{
				{"op": "remove", "path": "/metadata/labels"},
			}
			patchData, err := json.Marshal(&patch)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// trigger update event
			_, err = fakeClient.KubeClient.CoreV1().Nodes().Patch(context.TODO(), hoNodeName,
				kapitypes.JSONPatchType, patchData, metav1.PatchOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			//check if the hybrid overlay elements have been cleaned up
			gomega.Eventually(func() ([]*nbdb.LogicalRouterStaticRoute, error) {
				p := func(item *nbdb.LogicalRouterStaticRoute) bool {
					if item.ExternalIDs["name"] == "hybrid-subnet-node1-gr" ||
						item.ExternalIDs["name"] == "hybrid-subnet-node1:node-windows" {
						return true
					}
					return false
				}
				logicalRouterStaticRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(clusterController.nbClient, p)
				if err != nil {
					return nil, err
				}
				return logicalRouterStaticRoutes, nil
			}, 2).Should(gomega.HaveLen(0))

			gomega.Eventually(func() ([]*nbdb.LogicalRouterPolicy, error) {
				p := func(item *nbdb.LogicalRouterPolicy) bool {
					return strings.Contains(item.ExternalIDs["name"], "hybrid-subnet-node1")
				}
				logicalRouterPolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(clusterController.nbClient, p)
				if err != nil {
					return nil, err
				}
				return logicalRouterPolicies, nil

			}, 2).Should(gomega.HaveLen(0))

			return nil
		}
		err := app.Run([]string{
			app.Name,
			"--no-hostsubnet-nodes=kubernetes.io/os=windows",
			"-cluster-subnets=" + clusterCIDR,
			"-gateway-mode=shared",
			"-enable-hybrid-overlay",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("cleans up a Linux node when the OVN hostsubnet annotation is removed", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeHOMAC string = "0a:58:0a:01:01:03"
				hoSubnet  string = "11.1.0.0/16"
				nodeHOIP  string = "10.1.1.3"
			)
			node1 := tNode{
				Name:                 "node1",
				NodeIP:               "1.2.3.4",
				NodeLRPMAC:           "0a:58:0a:01:01:01",
				LrpIP:                "100.64.0.2",
				DrLrpIP:              "100.64.0.1",
				PhysicalBridgeMAC:    "11:22:33:44:55:66",
				SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				NodeSubnet:           "10.1.1.0/24",
				GWRouter:             types.GWRouterPrefix + "node1",
				GatewayRouterIPMask:  "172.16.16.2/24",
				GatewayRouterIP:      "172.16.16.2",
				GatewayRouterNextHop: "172.16.16.1",
				PhysicalBridgeName:   "br-eth0",
				NodeGWIP:             "10.1.1.1/24",
				NodeMgmtPortIP:       "10.1.1.2",
				//NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
				NodeMgmtPortMAC: "0a:58:64:40:00:03",
				DnatSnatIP:      "169.254.0.1",
			}
			testNode := node1.k8sNode("2")

			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			egressQoSFakeClient := &egressqosfake.Clientset{}
			egressServiceFakeClient := &egressservicefake.Clientset{}
			fakeClient := &util.OVNMasterClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				EgressQoSClient:      egressQoSFakeClient,
				EgressServiceClient:  egressServiceFakeClient,
			}

			vlanID := 1024
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.Kubernetes.HostNetworkNamespace = ""
			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, testNode.Name)
			l3Config := node1.gatewayConfig(config.GatewayModeShared, uint(vlanID))
			err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.UpdateNodeManagementPortMACAddresses(&testNode, nodeAnnotator,
				ovntest.MustParseMAC(node1.NodeMgmtPortMAC), types.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostCIDRs(nodeAnnotator, sets.New(fmt.Sprintf("%s/24", node1.NodeIP)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostAddrs, err := util.ParseNodeHostCIDRsDropNetMask(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f, err = factory.NewMasterWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedClusterLBGroup := newLoadBalancerGroup(types.ClusterLBGroupName)
			expectedSwitchLBGroup := newLoadBalancerGroup(types.ClusterSwitchLBGroupName)
			expectedRouterLBGroup := newLoadBalancerGroup(types.ClusterRouterLBGroupName)
			expectedOVNClusterRouter := newOVNClusterRouter()
			ovnClusterRouterLRP := &nbdb.LogicalRouterPort{
				Name:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter,
				Networks: []string{"100.64.0.1/16"},
				UUID:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter + "-UUID",
			}
			expectedOVNClusterRouter.Ports = []string{ovnClusterRouterLRP.UUID}
			expectedNodeSwitch := node1.logicalSwitch([]string{expectedClusterLBGroup.UUID, expectedSwitchLBGroup.UUID})
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			dbSetup := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					newClusterJoinSwitch(),
					expectedNodeSwitch,
					ovnClusterRouterLRP,
					expectedOVNClusterRouter,
					expectedClusterRouterPortGroup,
					expectedClusterPortGroup,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			}
			var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedDatabaseState := []libovsdbtest.TestData{ovnClusterRouterLRP}
			expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1)

			clusterController, err := NewOvnController(
				fakeClient,
				f,
				stopChan,
				nil,
				networkmanager.Default().Interface(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				record.NewFakeRecorder(10),
				wg,
				nil,
				NewPortCache(stopChan),
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			setupCOPP := true
			setupClusterController(clusterController, setupCOPP)

			//assuming all the pods have finished processing
			atomic.StoreUint32(&clusterController.allInitialPodsProcessed, 1)
			// Let the real code run and ensure OVN database sync
			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())

			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(gomega.HaveKeyWithValue(hotypes.HybridOverlayDRMAC, nodeHOMAC))

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			err = clusterController.syncDefaultGatewayLogicalNetwork(updatedNode, l3GatewayConfig, []*net.IPNet{subnet}, hostAddrs.UnsortedList())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var clusterSubnets []*net.IPNet
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
			}

			skipSnat := false
			expectedDatabaseState = generateGatewayInitExpectedNB(expectedDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3Config,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)}, skipSnat,
				node1.NodeMgmtPortIP, "1400")

			hybridSubnetStaticRoute1, hybridLogicalRouterStaticRoute, hybridSubnetLRP1, hybridSubnetLRP2, hybridLogicalSwitchPort := setupHybridOverlayOVNObjects(node1, "", hoSubnet, nodeHOIP, nodeHOMAC)

			var node1LogicalRouter *nbdb.LogicalRouter
			var basicNode1StaticRoutes []string

			for _, obj := range expectedDatabaseState {
				if logicalRouter, ok := obj.(*nbdb.LogicalRouter); ok {
					if logicalRouter.Name == "GR_node1" {
						// keep a referance so that we can edit this object
						node1LogicalRouter = logicalRouter
						basicNode1StaticRoutes = logicalRouter.StaticRoutes
						logicalRouter.StaticRoutes = append(logicalRouter.StaticRoutes, hybridLogicalRouterStaticRoute.UUID)
					}
				}
			}

			// keep copies of these before appending hybrid overlay elements
			basicExpectedNodeSwitchPorts := expectedNodeSwitch.Ports
			basicExpectedOVNClusterRouterPolicies := expectedOVNClusterRouter.Policies
			basicExpectedOVNClusterStaticRoutes := expectedOVNClusterRouter.StaticRoutes

			expectedNodeSwitch.Ports = append(expectedNodeSwitch.Ports, hybridLogicalSwitchPort.UUID)
			expectedOVNClusterRouter.Policies = append(expectedOVNClusterRouter.Policies, hybridSubnetLRP1.UUID, hybridSubnetLRP2.UUID)
			expectedOVNClusterRouter.StaticRoutes = append(expectedOVNClusterRouter.StaticRoutes, hybridSubnetStaticRoute1.UUID)

			expectedDatabaseStateWithHybridNode := append([]libovsdbtest.TestData{hybridSubnetStaticRoute1, hybridSubnetLRP2, hybridSubnetLRP1, hybridLogicalSwitchPort, hybridLogicalRouterStaticRoute}, expectedDatabaseState...)
			expectedStaticMACBinding := &nbdb.StaticMACBinding{
				UUID:               "MAC-binding-HO-UUID",
				IP:                 nodeHOIP,
				LogicalPort:        "rtos-node1",
				MAC:                nodeHOMAC,
				OverrideDynamicMAC: true,
			}
			expectedDatabaseStateWithHybridNode = append(expectedDatabaseStateWithHybridNode, expectedStaticMACBinding)
			gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDatabaseStateWithHybridNode))

			nodeAnnotator = kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, testNode.Name)
			util.DeleteNodeHostSubnetAnnotation(nodeAnnotator)
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 5).ShouldNot(gomega.HaveKey(hotypes.HybridOverlayDRMAC))

			// restore values from the non-hybrid versions
			expectedNodeSwitch.Ports = basicExpectedNodeSwitchPorts
			expectedOVNClusterRouter.Policies = basicExpectedOVNClusterRouterPolicies
			expectedOVNClusterRouter.StaticRoutes = basicExpectedOVNClusterStaticRoutes
			node1LogicalRouter.StaticRoutes = basicNode1StaticRoutes

			gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

			return nil
		}
		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-gateway-mode=shared",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("cleans up a Linux node that has hybridOverlay annotations and database objects when hybrid overlay is disabled", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeHOMAC string = "0a:58:0a:01:01:03"
				hoSubnet  string = "11.1.0.0/16"
				nodeHOIP  string = "10.1.1.3"
			)
			node1 := tNode{
				Name:                 "node1",
				NodeIP:               "1.2.3.4",
				NodeLRPMAC:           "0a:58:0a:01:01:01",
				LrpIP:                "100.64.0.2",
				DrLrpIP:              "100.64.0.1",
				PhysicalBridgeMAC:    "11:22:33:44:55:66",
				SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
				NodeSubnet:           "10.1.1.0/24",
				GWRouter:             types.GWRouterPrefix + "node1",
				GatewayRouterIPMask:  "172.16.16.2/24",
				GatewayRouterIP:      "172.16.16.2",
				GatewayRouterNextHop: "172.16.16.1",
				PhysicalBridgeName:   "br-eth0",
				NodeGWIP:             "10.1.1.1/24",
				NodeMgmtPortIP:       "10.1.1.2",
				//NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
				NodeMgmtPortMAC: "0a:58:64:40:00:03",
				DnatSnatIP:      "169.254.0.1",
			}
			testNode := node1.k8sNode("2")
			testNode.Annotations = map[string]string{
				hotypes.HybridOverlayDRIP:  nodeHOIP,
				hotypes.HybridOverlayDRMAC: nodeHOMAC,
				"k8s.ovn.org/ovn-node-id":  "2",
				util.OVNNodeGRLRPAddrs:     "{\"default\":{\"ipv4\":\"100.64.0.2/16\"}}"}

			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			egressQoSFakeClient := &egressqosfake.Clientset{}
			fakeClient := &util.OVNMasterClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				EgressQoSClient:      egressQoSFakeClient,
			}

			vlanID := 1024
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.Kubernetes.HostNetworkNamespace = ""
			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, testNode.Name)
			l3Config := node1.gatewayConfig(config.GatewayModeShared, uint(vlanID))
			err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.UpdateNodeManagementPortMACAddresses(&testNode, nodeAnnotator,
				ovntest.MustParseMAC(node1.NodeMgmtPortMAC), types.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostCIDRs(nodeAnnotator, sets.New(fmt.Sprintf("%s/24", node1.NodeIP)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f, err = factory.NewMasterWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedClusterLBGroup := newLoadBalancerGroup(types.ClusterLBGroupName)
			expectedSwitchLBGroup := newLoadBalancerGroup(types.ClusterSwitchLBGroupName)
			expectedRouterLBGroup := newLoadBalancerGroup(types.ClusterRouterLBGroupName)
			expectedOVNClusterRouter := newOVNClusterRouter()
			ovnClusterRouterLRP := &nbdb.LogicalRouterPort{
				Name:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter,
				Networks: []string{"100.64.0.1/16"},
				UUID:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter + "-UUID",
			}
			expectedOVNClusterRouter.Ports = []string{ovnClusterRouterLRP.UUID}
			expectedNodeSwitch := node1.logicalSwitch([]string{expectedClusterLBGroup.UUID, expectedSwitchLBGroup.UUID})
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			hybridSubnetStaticRoute1, hybridLogicalRouterStaticRoute, hybridSubnetLRP1, hybridSubnetLRP2, hybridLogicalSwitchPort := setupHybridOverlayOVNObjects(node1, "", hoSubnet, nodeHOIP, nodeHOMAC)
			expectedStaticMACBinding := &nbdb.StaticMACBinding{
				UUID:               "MAC-binding-HO-UUID",
				IP:                 nodeHOIP,
				LogicalPort:        "rtos-node1",
				MAC:                nodeHOMAC,
				OverrideDynamicMAC: true,
			}

			dbSetup := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					newClusterJoinSwitch(),
					expectedNodeSwitch,
					ovnClusterRouterLRP,
					expectedOVNClusterRouter,
					expectedClusterRouterPortGroup,
					expectedClusterPortGroup,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
					hybridSubnetStaticRoute1,
					hybridLogicalRouterStaticRoute,
					hybridSubnetLRP1,
					hybridSubnetLRP2,
					hybridLogicalSwitchPort,
					expectedStaticMACBinding,
				},
			}
			var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			clusterController, err := NewOvnController(
				fakeClient,
				f,
				stopChan,
				nil,
				networkmanager.Default().Interface(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				record.NewFakeRecorder(10),
				wg,
				nil,
				NewPortCache(stopChan),
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			setupCOPP := true
			setupClusterController(clusterController, setupCOPP)

			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(gomega.HaveKeyWithValue(hotypes.HybridOverlayDRMAC, nodeHOMAC))

			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(gomega.HaveKeyWithValue(hotypes.HybridOverlayDRIP, nodeHOIP))

			//assuming all the pods have finished processing
			atomic.StoreUint32(&clusterController.allInitialPodsProcessed, 1)
			// Let the real code run and ensure OVN database sync
			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())

			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).ShouldNot(gomega.HaveKey(hotypes.HybridOverlayDRMAC))

			gomega.Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).ShouldNot(gomega.HaveKey(hotypes.HybridOverlayDRIP))

			gomega.Eventually(func() ([]*nbdb.LogicalRouterStaticRoute, error) {
				p := func(item *nbdb.LogicalRouterStaticRoute) bool {
					if item.ExternalIDs["name"] == "hybrid-subnet-node1-gr" ||
						strings.Contains(item.ExternalIDs["name"], "hybrid-subnet-node1") {
						return true
					}
					return false
				}
				logicalRouterStaticRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(clusterController.nbClient, p)
				if err != nil {
					return nil, err
				}
				return logicalRouterStaticRoutes, nil
			}, 2).Should(gomega.HaveLen(0))

			gomega.Eventually(func() ([]*nbdb.LogicalRouterPolicy, error) {
				p := func(item *nbdb.LogicalRouterPolicy) bool {
					if strings.Contains(item.ExternalIDs["name"], "hybrid-subnet-node1") ||
						item.ExternalIDs["name"] == "hybrid-subnet-node1-gr" {
						return true
					}
					return false
				}
				logicalRouterPolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(clusterController.nbClient, p)
				if err != nil {
					return nil, err
				}
				return logicalRouterPolicies, nil

			}, 2).Should(gomega.HaveLen(0))

			gomega.Eventually(func() error {
				_, err := libovsdbops.GetLogicalSwitchPort(clusterController.nbClient, &nbdb.LogicalSwitchPort{Name: "int-node1"})
				if err != nil {
					return err
				}
				return nil
			}, 2).Should(gomega.Equal(libovsdbclient.ErrNotFound))

			gomega.Eventually(clusterController.nbClient.Get(context.Background(), expectedStaticMACBinding), 2).Should(gomega.Equal(libovsdbclient.ErrNotFound))

			return nil
		}
		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-gateway-mode=shared",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

})

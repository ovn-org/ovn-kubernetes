package controller

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func newTestNode(name, os, ovnHostSubnet, hybridHostSubnet, drMAC string) v1.Node {
	annotations := make(map[string]string)
	if ovnHostSubnet != "" {
		subnetAnnotations, err := util.CreateNodeHostSubnetAnnotation(ovntest.MustParseIPNets(ovnHostSubnet))
		Expect(err).NotTo(HaveOccurred())
		for k, v := range subnetAnnotations {
			annotations[k] = fmt.Sprintf("%s", v)
		}
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

func newTestWinNode(name, os, ovnHostSubnet, hybridHostSubnet, drMAC string) v1.Node {
	annotations := make(map[string]string)
	if ovnHostSubnet != "" {
		subnetAnnotations, err := util.CreateNodeHostSubnetAnnotation(ovntest.MustParseIPNets(ovnHostSubnet))
		Expect(err).NotTo(HaveOccurred())
		for k, v := range subnetAnnotations {
			annotations[k] = fmt.Sprintf("%s", v)
		}
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

var _ = Describe("Hybrid SDN Master Operations", func() {
	var (
		app             *cli.App
		stopChan        chan struct{}
		wg              *sync.WaitGroup
		fexec           *ovntest.FakeExec
		libovsdbCleanup *libovsdbtest.Cleanup
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}
		fexec = ovntest.NewFakeExec()
		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		libovsdbCleanup = nil
	})

	AfterEach(func() {
		close(stopChan)
		wg.Wait()
		if libovsdbCleanup != nil {
			libovsdbCleanup.Cleanup()
		}
	})

	const hybridOverlayClusterCIDR string = "11.1.0.0/16/24"
	It("allocates and assigns a hybrid-overlay subnet to a Windows node that doesn't have one", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "11.1.0.0/24"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "windows", "", "", ""),
				},
			})

			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			dbSetup := libovsdbtest.TestSetup{}
			var libovsdbOvnNBClient libovsdbclient.Client
			var libovsdbOvnSBClient libovsdbclient.Client

			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			Expect(err).NotTo(HaveOccurred())

			m, err := NewMaster(
				&kube.Kube{KClient: fakeClient},
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Namespaces().Informer(),
				f.Core().V1().Pods().Informer(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				informer.NewTestEventHandler,
			)
			Expect(err).NotTo(HaveOccurred())

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.Run(stopChan)
			}()
			f.WaitForCacheSync(stopChan)

			// Windows node should be allocated a subnet
			Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(HaveKeyWithValue(hotypes.HybridOverlayNodeSubnet, nodeSubnet))

			Eventually(func() error {
				updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				_, err = util.ParseNodeHostSubnetAnnotation(updatedNode)
				return err
			}, 2).Should(MatchError(fmt.Sprintf("node %q has no \"k8s.ovn.org/node-subnets\" annotation", nodeName)))

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)

			// nothing should be done in OVN dbs from HO running on windows node
			Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(dbSetup.NBData))
			Eventually(libovsdbOvnSBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(dbSetup.SBData))

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-loglevel=5",
			"-no-hostsubnet-nodes=" + v1.LabelOSStable + "=windows",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sets up and cleans up a Linux node with a OVN hostsubnet annotation", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "10.1.2.0/24"
				nodeHOIP   string = "10.1.2.3"
				nodeHOMAC  string = "0a:58:0a:01:02:03"
				hoSubnet   string = "11.1.0.0/16"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "linux", nodeSubnet, "", ""),
				},
			})

			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			// pre-existing nbdb objects
			nodeSwitch := &nbdb.LogicalSwitch{
				Name: nodeName,
				UUID: nodeName + "-UUID",
			}

			ovnClusterRouter := &nbdb.LogicalRouter{
				Name: types.OVNClusterRouter,
				UUID: types.OVNClusterRouter + "-UUID",
			}

			ovnClusterRouterLRP := &nbdb.LogicalRouterPort{
				Name:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter,
				Networks: []string{"100.64.0.1/16"},
				UUID:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter + "-UUID",
			}
			ovnClusterRouter.Ports = []string{ovnClusterRouterLRP.UUID}

			nodeGWRouter := &nbdb.LogicalRouter{
				Name: types.GWRouterPrefix + nodeName,
				UUID: types.GWRouterPrefix + nodeName + "-UUID",
			}

			nodeGWLRP := &nbdb.LogicalRouterPort{
				Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName,
				Networks: []string{"100.64.0.2/16"},
				UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID",
			}

			nodeGWRouter.Ports = []string{nodeGWLRP.UUID}

			initialNBDB := []libovsdbtest.TestData{
				nodeSwitch,
				ovnClusterRouter,
				ovnClusterRouterLRP,
				nodeGWRouter,
				nodeGWLRP,
			}

			// pre-existing sbdb objects
			clusterRouterDatapath := &sbdb.DatapathBinding{
				UUID:        types.OVNClusterRouter + "-UUID",
				ExternalIDs: map[string]string{"logical-router": ovnClusterRouter.UUID, "name": types.OVNClusterRouter},
			}

			initialSBDB := []libovsdbtest.TestData{
				clusterRouterDatapath,
			}

			dbSetup := libovsdbtest.TestSetup{
				NBData: initialNBDB,
				SBData: initialSBDB,
			}

			var libovsdbOvnNBClient libovsdbclient.Client
			var libovsdbOvnSBClient libovsdbclient.Client

			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)
			m, err := NewMaster(
				&kube.Kube{KClient: fakeClient},
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Namespaces().Informer(),
				f.Core().V1().Pods().Informer(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				informer.NewTestEventHandler,
			)
			Expect(err).NotTo(HaveOccurred())

			// make sure the expected LSP is created and added to the node
			expectedLSP := &nbdb.LogicalSwitchPort{
				UUID:      types.HybridOverlayPrefix + nodeName + "-UUID",
				Name:      types.HybridOverlayPrefix + nodeName,
				Addresses: []string{nodeHOMAC},
			}

			// make sure the expected LRP is created and added to cluster router
			expectedLRP1 := &nbdb.LogicalRouterPolicy{
				Priority: 1002,
				ExternalIDs: map[string]string{
					"name": "hybrid-subnet-node1-gr",
				},
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{nodeHOIP},
				Match:    "ip4.src == 100.64.0.2 && ip4.dst == 11.1.0.0/16",
				UUID:     "expectedLRP-1-UUID",
			}

			expectedLRP2 := &nbdb.LogicalRouterPolicy{
				Priority: 1002,
				ExternalIDs: map[string]string{
					"name": "hybrid-subnet-node1",
				},
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{nodeHOIP},
				Match:    "inport == \"rtos-node1\" && ip4.dst == 11.1.0.0/16",
				UUID:     "expectedLRP-2-UUID",
			}

			expectedLRSR := &nbdb.LogicalRouterStaticRoute{
				IPPrefix: hoSubnet,
				Nexthop:  nodeHOIP,
				ExternalIDs: map[string]string{
					"name": "hybrid-subnet-node1",
				},
				UUID: "expectedLRSR-UUID",
			}

			nodeSwitch.Ports = []string{expectedLSP.UUID}
			ovnClusterRouter.Policies = []string{expectedLRP1.UUID, expectedLRP2.UUID}
			ovnClusterRouter.StaticRoutes = []string{expectedLRSR.UUID}

			expectedGRLRSR := &nbdb.LogicalRouterStaticRoute{
				IPPrefix: hoSubnet,
				Nexthop:  "100.64.0.1",
				ExternalIDs: map[string]string{
					"name": "hybrid-subnet-node1-gr",
				},
				UUID: "expectedGRLRSR-UUID",
			}
			nodeGWRouter.StaticRoutes = []string{expectedGRLRSR.UUID}

			expectedMACBinding := &sbdb.MACBinding{
				Datapath:    clusterRouterDatapath.UUID,
				IP:          nodeHOIP,
				LogicalPort: types.RouterToSwitchPrefix + nodeName,
				MAC:         nodeHOMAC,
			}

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.Run(stopChan)
			}()
			f.WaitForCacheSync(stopChan)

			Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(HaveKeyWithValue(hotypes.HybridOverlayDRMAC, nodeHOMAC))

			nodeSwitch.OtherConfig = map[string]string{"exclude_ips": "10.1.2.2"}

			expectedNBDatabaseState := []libovsdbtest.TestData{
				nodeSwitch,
				ovnClusterRouter,
				ovnClusterRouterLRP,
				nodeGWRouter,
				nodeGWLRP,
				expectedLSP,
				expectedLRP1,
				expectedLRP2,
				expectedLRSR,
				expectedGRLRSR,
			}

			expectedSBDatabaseState := []libovsdbtest.TestData{
				clusterRouterDatapath,
				expectedMACBinding,
			}

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))
			Eventually(libovsdbOvnSBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedSBDatabaseState))

			err = fakeClient.CoreV1().Nodes().Delete(context.TODO(), nodeName, *metav1.NewDeleteOptions(0))
			Expect(err).NotTo(HaveOccurred())

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)

			// LRP should have been deleted and removed
			ovnClusterRouter.Policies = []string{}

			// LSP should have been deleted and removed
			nodeSwitch.Ports = []string{}

			// Static Route should have been deleted and removed
			ovnClusterRouter.StaticRoutes = []string{}
			nodeGWRouter.StaticRoutes = []string{}

			expectedNBDatabaseState = []libovsdbtest.TestData{
				ovnClusterRouter,
				nodeSwitch,
				nodeGWRouter,
				nodeGWLRP,
				ovnClusterRouterLRP,
			}

			// in a real db, deleting the HO LSP would result in the Mac Binding being removed as well
			expectedSBDatabaseState = []libovsdbtest.TestData{
				clusterRouterDatapath,
				expectedMACBinding,
			}

			Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedNBDatabaseState))
			Eventually(libovsdbOvnSBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedSBDatabaseState))
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-loglevel=5",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles a Linux node with no annotation but an existing port and lrp", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "10.1.2.0/24"
				nodeHOIP   string = "10.1.2.3"
				nodeHOMAC  string = "00:00:00:52:19:d2"
				hoSubnet   string = "11.1.0.0/16"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "linux", nodeSubnet, "", ""),
				},
			})

			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			dynAdd := nodeHOMAC + " " + nodeHOIP

			// pre-existing nbdb objects

			existingLSP := &nbdb.LogicalSwitchPort{
				UUID:             types.HybridOverlayPrefix + nodeName + "-UUID",
				Name:             types.HybridOverlayPrefix + nodeName,
				Addresses:        []string{nodeHOMAC},
				DynamicAddresses: &dynAdd,
			}

			existingLRP1 := &nbdb.LogicalRouterPolicy{
				UUID:     "hybrid-subnet-node1-1-UUID",
				Priority: 1002,
				ExternalIDs: map[string]string{
					"name": "hybrid-subnet-node1-gr",
				},
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{nodeHOIP},
				Match:    "ip4.src == 100.64.0.2 && ip4.dst == 11.1.0.0/16",
			}

			existingLRP2 := &nbdb.LogicalRouterPolicy{
				Priority: 1002,
				ExternalIDs: map[string]string{
					"name": "hybrid-subnet-node1",
				},
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{nodeHOIP},
				Match:    "inport == \"rtos-node1\" && ip4.dst == 11.1.0.0/16",
				UUID:     "hybrid-subnet-node1-2-UUID",
			}

			existingLRSR := &nbdb.LogicalRouterStaticRoute{
				UUID:     "hybrid-subnet-node1-LRSR-UUID",
				IPPrefix: hoSubnet,
				Nexthop:  nodeHOIP,
				ExternalIDs: map[string]string{
					"name": "hybrid-subnet-node1",
				},
			}

			existingGRLRSR := &nbdb.LogicalRouterStaticRoute{
				UUID:     "hybrid-subnet-node1-GRLRSR-UUID",
				IPPrefix: hoSubnet,
				Nexthop:  "100.64.0.1",
				ExternalIDs: map[string]string{
					"name": "hybrid-subnet-node1-gr",
				},
			}

			nodeSwitch := &nbdb.LogicalSwitch{
				Name:  nodeName,
				UUID:  nodeName + "-LS-UUID",
				Ports: []string{existingLSP.UUID},
			}

			ovnClusterRouterLRP := &nbdb.LogicalRouterPort{
				UUID:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter + "-UUID",
				Name:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter,
				Networks: []string{"100.64.0.1/16"},
			}

			ovnClusterRouter := &nbdb.LogicalRouter{
				Name: types.OVNClusterRouter,
				UUID: types.OVNClusterRouter + "-UUID",
				// Something in the test harness causes this names uuid to be added again
				// comment out for now

				Ports: []string{ovnClusterRouterLRP.UUID},
			}
			nodeGWLRP := &nbdb.LogicalRouterPort{
				UUID:     nodeName + "-LRP-UUID",
				Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName,
				Networks: []string{"100.64.0.2/16"},
			}

			nodeGWRouter := &nbdb.LogicalRouter{
				UUID:         nodeName + "-LR-UUID",
				Name:         types.GWRouterPrefix + nodeName,
				Ports:        []string{nodeGWLRP.UUID},
				StaticRoutes: []string{existingGRLRSR.UUID},
			}

			initialNBDB := []libovsdbtest.TestData{
				nodeSwitch,
				ovnClusterRouter,
				existingLRP1,
				existingLRP2,
				existingLRSR,
				existingGRLRSR,
				existingLSP,
				ovnClusterRouterLRP,
				nodeGWRouter,
				nodeGWLRP,
			}

			// pre-existing sbdb objects
			clusterRouterDatapath := &sbdb.DatapathBinding{
				UUID:        types.OVNClusterRouter + "-UUID",
				ExternalIDs: map[string]string{"logical-router": ovnClusterRouter.UUID, "name": types.OVNClusterRouter},
			}

			// the mac binding should already exist
			existingMACBinding := &sbdb.MACBinding{
				UUID:        types.RouterToSwitchPrefix + nodeName + "-UUID",
				Datapath:    clusterRouterDatapath.UUID,
				IP:          nodeHOIP,
				LogicalPort: types.RouterToSwitchPrefix + nodeName,
				MAC:         nodeHOMAC,
			}

			initialSBDB := []libovsdbtest.TestData{
				clusterRouterDatapath,
				existingMACBinding,
			}

			dbSetup := libovsdbtest.TestSetup{
				NBData: initialNBDB,
				SBData: initialSBDB,
			}

			var libovsdbOvnNBClient libovsdbclient.Client
			var libovsdbOvnSBClient libovsdbclient.Client

			// nothing will occur in the SBDB or NBDB in this instance because the HO lrp already exists
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)
			m, err := NewMaster(
				&kube.Kube{KClient: fakeClient},
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Namespaces().Informer(),
				f.Core().V1().Pods().Informer(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				informer.NewTestEventHandler,
			)
			Expect(err).NotTo(HaveOccurred())

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.Run(stopChan)
			}()
			f.WaitForCacheSync(stopChan)

			Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(HaveKeyWithValue(hotypes.HybridOverlayDRMAC, nodeHOMAC))

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			// OVN DB state shouldn't change here
			ovnClusterRouter.Policies = []string{existingLRP1.UUID, existingLRP2.UUID}
			ovnClusterRouter.StaticRoutes = []string{existingLRSR.UUID}
			Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(dbSetup.NBData))
			Eventually(libovsdbOvnSBClient).Should(libovsdbtest.HaveData(dbSetup.SBData))

			return nil
		}
		err := app.Run([]string{
			app.Name,
			"-loglevel=5",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("cluster handles a linux node when hybridOverlayClusterCIDR in unset but the HO annotations are available on windows nodes", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				linNodeName   string = "node-linux"
				winNodeName   string = "node-windows"
				linNodeSubnet string = "10.1.2.0/24"
				winNodeSubnet string = "10.1.3.0/24"
				linNodeHOIP   string = "10.1.2.3"
				linNodeHOMAC  string = "0a:58:0a:01:02:03"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
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
					newTestNode(linNodeName, "linux", linNodeSubnet, "", ""),
				},
			})

			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			// pre-existing nbdb objects
			nodeSwitch := &nbdb.LogicalSwitch{
				Name: linNodeName,
				UUID: linNodeName + "-UUID",
			}

			ovnClusterRouter := &nbdb.LogicalRouter{
				Name: types.OVNClusterRouter,
				UUID: types.OVNClusterRouter + "-UUID",
			}

			ovnClusterRouterLRP := &nbdb.LogicalRouterPort{
				Name:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter,
				Networks: []string{"100.64.0.1/16"},
				UUID:     types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter + "-UUID",
			}
			ovnClusterRouter.Ports = []string{ovnClusterRouterLRP.UUID}

			nodeGWRouter := &nbdb.LogicalRouter{
				Name: types.GWRouterPrefix + linNodeName,
				UUID: types.GWRouterPrefix + linNodeName + "-UUID",
			}

			nodeGWLRP := &nbdb.LogicalRouterPort{
				Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + linNodeName,
				Networks: []string{"100.64.0.2/16"},
				UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + linNodeName + "-UUID",
			}

			nodeGWRouter.Ports = []string{nodeGWLRP.UUID}

			initialNBDB := []libovsdbtest.TestData{
				nodeSwitch,
				ovnClusterRouter,
				ovnClusterRouterLRP,
				nodeGWRouter,
				nodeGWLRP,
			}

			// pre-existing sbdb objects
			clusterRouterDatapath := &sbdb.DatapathBinding{
				UUID:        types.OVNClusterRouter + "-UUID",
				ExternalIDs: map[string]string{"logical-router": ovnClusterRouter.UUID, "name": types.OVNClusterRouter},
			}

			initialSBDB := []libovsdbtest.TestData{
				clusterRouterDatapath,
			}

			dbSetup := libovsdbtest.TestSetup{
				NBData: initialNBDB,
				SBData: initialSBDB,
			}

			var libovsdbOvnNBClient libovsdbclient.Client
			var libovsdbOvnSBClient libovsdbclient.Client

			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)
			m, err := NewMaster(
				&kube.Kube{KClient: fakeClient},
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Namespaces().Informer(),
				f.Core().V1().Pods().Informer(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				informer.NewTestEventHandler,
			)
			Expect(err).NotTo(HaveOccurred())

			// make sure the expected LSP is created and added to the node
			expectedLSP := &nbdb.LogicalSwitchPort{
				UUID:      types.HybridOverlayPrefix + linNodeName + "-UUID",
				Name:      types.HybridOverlayPrefix + linNodeName,
				Addresses: []string{linNodeHOMAC},
			}

			// make sure the expected LRP is created and added to cluster router
			expectedLRP1 := &nbdb.LogicalRouterPolicy{
				Priority: 1002,
				ExternalIDs: map[string]string{
					"name": "hybrid-subnet-node-linux-gr",
				},
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{linNodeHOIP},
				Match:    fmt.Sprintf("ip4.src == 100.64.0.2 && ip4.dst == %s", winNodeSubnet),
				UUID:     "expectedLRP-1-UUID",
			}

			expectedLRP2 := &nbdb.LogicalRouterPolicy{
				Priority: 1002,
				ExternalIDs: map[string]string{
					"name": "hybrid-subnet-node-linux",
				},
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{linNodeHOIP},
				Match:    fmt.Sprintf("inport == \"rtos-node-linux\" && ip4.dst == %s", winNodeSubnet),
				UUID:     "expectedLRP-2-UUID",
			}

			expectedLRSR := &nbdb.LogicalRouterStaticRoute{
				IPPrefix: winNodeSubnet,
				Nexthop:  linNodeHOIP,
				ExternalIDs: map[string]string{
					"name": "hybrid-subnet-node-linux",
				},
				UUID: "expectedLRSR-UUID",
			}

			nodeSwitch.Ports = []string{expectedLSP.UUID}
			ovnClusterRouter.Policies = []string{expectedLRP1.UUID, expectedLRP2.UUID}
			ovnClusterRouter.StaticRoutes = []string{expectedLRSR.UUID}

			expectedGRLRSR := &nbdb.LogicalRouterStaticRoute{
				IPPrefix: winNodeSubnet,
				Nexthop:  "100.64.0.1",
				ExternalIDs: map[string]string{
					"name": "hybrid-subnet-node-linux-gr",
				},
				UUID: "expectedGRLRSR-UUID",
			}
			nodeGWRouter.StaticRoutes = []string{expectedGRLRSR.UUID}

			expectedMACBinding := &sbdb.MACBinding{
				Datapath:    clusterRouterDatapath.UUID,
				IP:          linNodeHOIP,
				LogicalPort: types.RouterToSwitchPrefix + linNodeName,
				MAC:         linNodeHOMAC,
			}

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.Run(stopChan)
			}()
			f.WaitForCacheSync(stopChan)

			Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.TODO(), linNodeName, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(HaveKeyWithValue(hotypes.HybridOverlayDRMAC, linNodeHOMAC))

			nodeSwitch.OtherConfig = map[string]string{"exclude_ips": "10.1.2.2"}

			expectedNBDatabaseState := []libovsdbtest.TestData{
				nodeSwitch,
				ovnClusterRouter,
				ovnClusterRouterLRP,
				nodeGWRouter,
				nodeGWLRP,
				expectedLSP,
				expectedLRP1,
				expectedLRP2,
				expectedLRSR,
				expectedGRLRSR,
			}

			expectedSBDatabaseState := []libovsdbtest.TestData{
				clusterRouterDatapath,
				expectedMACBinding,
			}

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))
			Eventually(libovsdbOvnSBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedSBDatabaseState))

			err = fakeClient.CoreV1().Nodes().Delete(context.TODO(), linNodeName, *metav1.NewDeleteOptions(0))
			Expect(err).NotTo(HaveOccurred())

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)

			// LRP should have been deleted and removed
			ovnClusterRouter.Policies = []string{}

			// LSP should have been deleted and removed
			nodeSwitch.Ports = []string{}

			// Static Route should have been deleted and removed
			ovnClusterRouter.StaticRoutes = []string{}
			nodeGWRouter.StaticRoutes = []string{}

			expectedNBDatabaseState = []libovsdbtest.TestData{
				ovnClusterRouter,
				nodeSwitch,
				nodeGWRouter,
				nodeGWLRP,
				ovnClusterRouterLRP,
			}

			// in a real db, deleting the HO LSP would result in the Mac Binding being removed as well
			expectedSBDatabaseState = []libovsdbtest.TestData{
				clusterRouterDatapath,
				expectedMACBinding,
			}

			Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedNBDatabaseState))
			Eventually(libovsdbOvnSBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedSBDatabaseState))
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-loglevel=5",
			"-enable-hybrid-overlay",
			"--no-hostsubnet-nodes=kubernetes.io/os=windows",
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("cleans up a Linux node when the OVN hostsubnet annotation is removed", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "10.1.2.0/24"
				nodeHOIP   string = "10.1.2.3"
				nodeHOMAC  string = "00:00:00:52:19:d2"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "linux", nodeSubnet, "", nodeHOMAC),
				},
			})

			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			dynAdd := nodeHOMAC + " " + nodeHOIP
			initialDatabaseState := []libovsdbtest.TestData{
				&nbdb.LogicalRouterPolicy{
					Priority: 1002,
					ExternalIDs: map[string]string{
						"name": "hybrid-subnet-node1",
					},
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{nodeHOIP},
					Match:    "inport == \"rtos-node1\" && ip4.dst == 11.1.0.0/16",
					UUID:     "reroute-policy-UUID",
				},
				&nbdb.LogicalRouter{
					Name:     types.OVNClusterRouter,
					Policies: []string{"reroute-policy-UUID"},
				},
				&nbdb.LogicalSwitchPort{
					UUID:             "int-" + nodeName + "-UUID",
					Name:             "int-" + nodeName,
					Addresses:        []string{nodeHOMAC, nodeHOIP},
					DynamicAddresses: &dynAdd,
				},
				&nbdb.LogicalSwitch{
					Name:  nodeName,
					UUID:  nodeName + "-UUID",
					Ports: []string{"int-" + nodeName + "-UUID"},
				},
			}
			dbSetup := libovsdbtest.TestSetup{
				NBData: initialDatabaseState,
				SBData: nil,
			}
			var libovsdbOvnNBClient libovsdbclient.Client
			var libovsdbOvnSBClient libovsdbclient.Client

			// nothing will occur in the SBDB in this instance because we don't explicitly clean up any created
			// mac bindings
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			Expect(err).NotTo(HaveOccurred())

			m, err := NewMaster(
				&kube.Kube{KClient: fakeClient},
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Namespaces().Informer(),
				f.Core().V1().Pods().Informer(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				informer.NewTestEventHandler,
			)
			Expect(err).NotTo(HaveOccurred())

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.Run(stopChan)
			}()
			f.WaitForCacheSync(stopChan)

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(initialDatabaseState))

			k := &kube.Kube{KClient: fakeClient}
			updatedNode, err := k.GetNode(nodeName)
			Expect(err).NotTo(HaveOccurred())

			nodeAnnotator := kube.NewNodeAnnotator(k, updatedNode.Name)
			util.DeleteNodeHostSubnetAnnotation(nodeAnnotator)
			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() (map[string]string, error) {
				updatedNode, err = k.GetNode(nodeName)
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 5).ShouldNot(HaveKey(hotypes.HybridOverlayDRMAC))

			expectedDatabaseState := []libovsdbtest.TestData{
				&nbdb.LogicalRouterPolicy{
					Priority: 1002,
					ExternalIDs: map[string]string{
						"name": "hybrid-subnet-node1",
					},
					Action:   nbdb.LogicalRouterPolicyActionReroute,
					Nexthops: []string{nodeHOIP},
					Match:    "inport == \"rtos-node1\" && ip4.dst == 11.1.0.0/16",
					UUID:     "reroute-policy-UUID",
				},
				&nbdb.LogicalRouter{
					Name:     types.OVNClusterRouter,
					Policies: []string{"reroute-policy-UUID"},
				},
				&nbdb.LogicalSwitch{
					Name: nodeName,
					UUID: nodeName + "-UUID",
				},
			}

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

			// nothing will written to sbdb here since the logical router policy already exists
			Eventually(libovsdbOvnSBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(dbSetup.SBData))

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-loglevel=5",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("copies namespace annotations when a pod is added", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nsName     string = "nstest"
				nsVTEP            = "1.1.1.1"
				nsExGw            = "2.2.2.2"
				nodeName   string = "node1"
				nodeSubnet string = "10.1.2.0/24"
				nodeHOIP   string = "10.1.2.3"
				nodeHOMAC  string = "00:00:00:52:19:d2"
				pod1Name   string = "pod1"
				pod1IP     string = "1.2.3.5"
				pod1CIDR   string = pod1IP + "/24"
				pod1MAC    string = "aa:bb:cc:dd:ee:ff"
			)

			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					UID:  k8stypes.UID(nsName),
					Name: nsName,
					Annotations: map[string]string{
						hotypes.HybridOverlayVTEP:       nsVTEP,
						hotypes.HybridOverlayExternalGw: nsExGw,
					},
				},
				Spec:   v1.NamespaceSpec{},
				Status: v1.NamespaceStatus{},
			}
			fakeClient := fake.NewSimpleClientset([]runtime.Object{
				ns,
				createPod(nsName, pod1Name, nodeName, pod1CIDR, pod1MAC),
				&v1.NodeList{Items: []v1.Node{newTestNode(nodeName, "linux", nodeSubnet, "", nodeHOMAC)}},
			}...)

			_, err := config.InitConfig(ctx, nil, nil)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)
			// TODO(trozet) actually check some expected data in the DB?
			dbSetup := libovsdbtest.TestSetup{}
			var libovsdbOvnNBClient libovsdbclient.Client
			var libovsdbOvnSBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			Expect(err).NotTo(HaveOccurred())
			m, err := NewMaster(
				&kube.Kube{KClient: fakeClient},
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Namespaces().Informer(),
				f.Core().V1().Pods().Informer(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				informer.NewTestEventHandler,
			)
			Expect(err).NotTo(HaveOccurred())

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.Run(stopChan)
			}()
			f.WaitForCacheSync(stopChan)

			Eventually(func() error {
				pod, err := fakeClient.CoreV1().Pods(nsName).Get(context.TODO(), pod1Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if pod.Annotations[hotypes.HybridOverlayVTEP] != nsVTEP {
					return fmt.Errorf("error with annotation %s. expected: %s, got: %s", hotypes.HybridOverlayVTEP, nsVTEP, pod.Annotations[hotypes.HybridOverlayVTEP])
				}
				if pod.Annotations[hotypes.HybridOverlayExternalGw] != nsExGw {
					return fmt.Errorf("error with annotation %s. expected: %s, got: %s", hotypes.HybridOverlayVTEP, nsExGw, pod.Annotations[hotypes.HybridOverlayExternalGw])
				}
				return nil
			}, 2).Should(Succeed())

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-loglevel=5",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update pod annotations when a namespace is updated", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nsName        string = "nstest"
				nsVTEP        string = "1.1.1.1"
				nsVTEPUpdated string = "3.3.3.3"
				nsExGw        string = "2.2.2.2"
				nsExGwUpdated string = "4.4.4.4"
				nodeName      string = "node1"
				nodeSubnet    string = "10.1.2.0/24"
				nodeHOMAC     string = "00:00:00:52:19:d2"
				nodeHOIP      string = "10.1.2.3"
				pod1Name      string = "pod1"
				pod1IP        string = "1.2.3.5"
				pod1CIDR      string = pod1IP + "/24"
				pod1MAC       string = "aa:bb:cc:dd:ee:ff"
			)

			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					UID:  k8stypes.UID(nsName),
					Name: nsName,
					Annotations: map[string]string{
						hotypes.HybridOverlayVTEP:       nsVTEP,
						hotypes.HybridOverlayExternalGw: nsExGw,
					},
				},
				Spec:   v1.NamespaceSpec{},
				Status: v1.NamespaceStatus{},
			}
			fakeClient := fake.NewSimpleClientset([]runtime.Object{
				ns,
				&v1.NodeList{Items: []v1.Node{newTestNode(nodeName, "linux", nodeSubnet, "", nodeHOMAC)}},
				createPod(nsName, pod1Name, nodeName, pod1CIDR, pod1MAC),
			}...)

			addLinuxNodeCommands(fexec, nodeHOMAC, nodeName, nodeHOIP)
			_, err := config.InitConfig(ctx, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			// TODO(trozet) actually check some expected data in the DB?
			dbSetup := libovsdbtest.TestSetup{}
			var libovsdbOvnNBClient libovsdbclient.Client
			var libovsdbOvnSBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			Expect(err).NotTo(HaveOccurred())

			k := &kube.Kube{KClient: fakeClient}
			m, err := NewMaster(
				k,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Namespaces().Informer(),
				f.Core().V1().Pods().Informer(),
				libovsdbOvnNBClient,
				libovsdbOvnSBClient,
				informer.NewTestEventHandler,
			)
			Expect(err).NotTo(HaveOccurred())

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.Run(stopChan)
			}()
			f.WaitForCacheSync(stopChan)

			updatedNs, err := fakeClient.CoreV1().Namespaces().Get(context.TODO(), nsName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			nsAnnotator := kube.NewNamespaceAnnotator(k, updatedNs.Name)
			nsAnnotator.Set(hotypes.HybridOverlayVTEP, nsVTEPUpdated)
			nsAnnotator.Set(hotypes.HybridOverlayExternalGw, nsExGwUpdated)
			err = nsAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() error {
				pod, err := fakeClient.CoreV1().Pods(nsName).Get(context.TODO(), pod1Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if reflect.DeepEqual(pod.Annotations[hotypes.HybridOverlayVTEP], nsVTEP) {
					return fmt.Errorf("error with annotation %s. expected: %s, got: %s", hotypes.HybridOverlayVTEP, nsVTEPUpdated, pod.Annotations[hotypes.HybridOverlayVTEP])
				}
				if reflect.DeepEqual(pod.Annotations[hotypes.HybridOverlayExternalGw], nsExGw) {
					return fmt.Errorf("error with annotation %s. expected: %s, got: %s", hotypes.HybridOverlayExternalGw, nsExGwUpdated, pod.Annotations[hotypes.HybridOverlayExternalGw])
				}
				return nil
			}, 2).Should(Succeed())

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-loglevel=5",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

func addLinuxNodeCommands(fexec *ovntest.FakeExec, nodeHOMAC, nodeName, nodeHOIP string) {
	updateLogicalRouterPolicy(fexec)

	fexec.AddFakeCmdsNoOutputNoError([]string{
		// Setting the mac on the lsp
		"ovn-nbctl --timeout=15 -- " +
			"--may-exist lsp-add node1 int-node1 -- " +
			"lsp-set-addresses int-node1 " + nodeHOMAC,
	})

	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 lsp-list " + nodeName,
		Output: "29df5ce5-2802-4ee5-891f-4fb27ca776e9 (" + types.K8sPrefix + nodeName + ")",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 -- --if-exists set logical_switch " + nodeName + " other-config:exclude_ips=" + nodeHOIP,
	})
}

func updateLogicalRouterPolicy(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		// Find if policy exists already
		Cmd: "ovn-nbctl --timeout=15 --columns _uuid --no-headings find logical_router_policy priority=1002 " +
			"external_ids=name=hybrid-subnet-node1 action=reroute nexthops=\"10.1.2.3\" " +
			`match="inport == \"rtos-node1\" && ip4.dst == 11.1.0.0/16"`,
		Output: "19df5ce5-2802-4ee5-891f-4fb27ca776e9",
	})
}

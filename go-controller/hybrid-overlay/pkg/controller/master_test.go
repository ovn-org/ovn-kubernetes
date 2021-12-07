package controller

import (
	"context"
	"fmt"
	"sync"

	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	. "github.com/onsi/gomega"
)

const hoNodeCliArg string = "-no-hostsubnet-nodes=" + v1.LabelOSStable + "=windows"

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
			libovsdbOvnNBClient, libovsdbCleanup, err = libovsdbtest.NewNBTestHarness(dbSetup, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			m, err := NewMaster(
				&kube.Kube{KClient: fakeClient},
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Namespaces().Informer(),
				f.Core().V1().Pods().Informer(),
				libovsdbOvnNBClient,
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
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "linux", nodeSubnet, "", ""),
				},
			})

			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

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
			}

			nodeSwitch := &nbdb.LogicalSwitch{
				Name: nodeName,
				UUID: nodeName + "-UUID",
			}

			initialExpectedDB := append(expectedDatabaseState, nodeSwitch)

			dbSetup := libovsdbtest.TestSetup{
				NBData: initialExpectedDB,
			}
			var libovsdbOvnNBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbCleanup, err = libovsdbtest.NewNBTestHarness(dbSetup, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)
			m, err := NewMaster(
				&kube.Kube{KClient: fakeClient},
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Namespaces().Informer(),
				f.Core().V1().Pods().Informer(),
				libovsdbOvnNBClient,
				informer.NewTestEventHandler,
			)
			Expect(err).NotTo(HaveOccurred())

			// Make the expected LSP is created and added to the node
			expectedLSP := &nbdb.LogicalSwitchPort{
				UUID:      libovsdbops.BuildNamedUUID(),
				Name:      types.HybridOverlayPrefix + nodeName,
				Addresses: []string{nodeHOMAC},
			}

			nodeSwitch.Ports = []string{expectedLSP.UUID}

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

			expectedDatabaseState = append(expectedDatabaseState, nodeSwitch, expectedLSP)

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

			err = fakeClient.CoreV1().Nodes().Delete(context.TODO(), nodeName, *metav1.NewDeleteOptions(0))
			Expect(err).NotTo(HaveOccurred())

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)

			// In a real db, deleting the LSP would remove the reference here, but in our testing ovsdb server it does not
			// nodeSwitch.Ports = []string{}
			expectedDatabaseState = []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					Name: types.OVNClusterRouter,
				},
				nodeSwitch,
			}
			Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
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

	It("handles a Linux node with no annotation but an existing port", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "10.1.2.0/24"
				nodeHOIP   string = "10.1.2.3"
				nodeHOMAC  string = "00:00:00:52:19:d2"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "linux", nodeSubnet, "", ""),
				},
			})

			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			dynAdd := nodeHOMAC + " " + nodeHOIP
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
				&nbdb.LogicalSwitchPort{
					Name:             "int-" + nodeName,
					Addresses:        []string{nodeHOMAC, nodeHOIP},
					DynamicAddresses: &dynAdd,
				},
				&nbdb.LogicalRouter{
					Name:     types.OVNClusterRouter,
					Policies: []string{"reroute-policy-UUID"},
				},
			}
			dbSetup := libovsdbtest.TestSetup{
				NBData: expectedDatabaseState,
			}
			var libovsdbOvnNBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbCleanup, err = libovsdbtest.NewNBTestHarness(dbSetup, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)
			m, err := NewMaster(
				&kube.Kube{KClient: fakeClient},
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Namespaces().Informer(),
				f.Core().V1().Pods().Informer(),
				libovsdbOvnNBClient,
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
			Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
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
			lspUUID := libovsdbops.BuildNamedUUID()
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
					UUID:             lspUUID,
					Name:             "int-" + nodeName,
					Addresses:        []string{nodeHOMAC, nodeHOIP},
					DynamicAddresses: &dynAdd,
				},
				&nbdb.LogicalSwitch{
					Name:  nodeName,
					UUID:  nodeName + "-UUID",
					Ports: []string{lspUUID},
				},
			}
			dbSetup := libovsdbtest.TestSetup{
				NBData: initialDatabaseState,
			}
			var libovsdbOvnNBClient libovsdbclient.Client
			libovsdbOvnNBClient, libovsdbCleanup, err = libovsdbtest.NewNBTestHarness(dbSetup, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			m, err := NewMaster(
				&kube.Kube{KClient: fakeClient},
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Namespaces().Informer(),
				f.Core().V1().Pods().Informer(),
				libovsdbOvnNBClient,
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
					// CI server doesn't clean this up even though the LSP has been deleted, will
					// be garbage collected in real scenario
					Ports: []string{lspUUID},
				},
			}

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
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

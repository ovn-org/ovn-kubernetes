package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	t "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

func newObjectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       types.UID(namespace),
		Name:      name,
		Namespace: namespace,
	}
}

func newEgressFirewallObject(name, namespace string, egressRules []egressfirewallapi.EgressFirewallRule) *egressfirewallapi.EgressFirewall {
	return &egressfirewallapi.EgressFirewall{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: egressfirewallapi.EgressFirewallSpec{
			Egress: egressRules,
		},
	}
}

var _ = ginkgo.Describe("OVN EgressFirewall Operations", func() {
	var (
		app                    *cli.App
		fakeOVN                *FakeOVN
		clusterPortGroup       *nbdb.PortGroup
		nodeSwitch, joinSwitch *nbdb.LogicalSwitch
		initialData            []libovsdbtest.TestData
		dbSetup                libovsdbtest.TestSetup
	)
	const (
		node1Name string = "node1"
		node2Name string = "node2"
	)

	clusterRouter := &nbdb.LogicalRouter{
		UUID: t.OVNClusterRouter + "-UUID",
		Name: t.OVNClusterRouter,
	}

	startOvn := func(dbSetup libovsdb.TestSetup, namespaces []v1.Namespace, egressFirewalls []egressfirewallapi.EgressFirewall) {
		fakeOVN.startWithDBSetup(dbSetup,
			&egressfirewallapi.EgressFirewallList{
				Items: egressFirewalls,
			},
			&v1.NamespaceList{
				Items: namespaces,
			},
		)
		err := fakeOVN.controller.WatchNamespaces()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = fakeOVN.controller.WatchEgressFirewall()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, namespace := range namespaces {
			namespaceASip4, namespaceASip6 := buildNamespaceAddressSets(namespace.Name, []net.IP{})
			if config.IPv4Mode {
				initialData = append(initialData, namespaceASip4)
			}
			if config.IPv6Mode {
				initialData = append(initialData, namespaceASip6)
			}
		}
	}

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.OVNKubernetesFeature.EnableEgressFirewall = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOVN = NewFakeOVN(false)
		clusterPortGroup = newClusterPortGroup()
		nodeSwitch = &nbdb.LogicalSwitch{
			UUID: node1Name + "-UUID",
			Name: node1Name,
		}
		joinSwitch = &nbdb.LogicalSwitch{
			UUID: "join-UUID",
			Name: "join",
		}
		initialData = []libovsdbtest.TestData{
			nodeSwitch,
			joinSwitch,
			clusterPortGroup,
			clusterRouter,
		}
		dbSetup = libovsdbtest.TestSetup{
			NBData: initialData,
		}
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
	})

	for _, gwMode := range []config.GatewayMode{config.GatewayModeLocal, config.GatewayModeShared} {
		gwMode := gwMode
		ginkgo.Context("on startup", func() {
			ginkgo.It(fmt.Sprintf("reconciles stale ACLs, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					// owned by non-existing namespace
					fakeController := getFakeController(DefaultNetworkControllerName)
					purgeIDs := fakeController.getEgressFirewallACLDbIDs("none", 0)
					purgeACL := libovsdbops.BuildACL(
						"purgeACL1",
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						"",
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						purgeIDs.GetExternalIDs(),
						nil,
						t.PlaceHolderACLTier,
					)
					purgeACL.UUID = "purgeACL-UUID"
					// no externalIDs present => dbIDs can't be built
					purgeACL2 := libovsdbops.BuildACL(
						"purgeACL2",
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						"",
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						nil,
						nil,
						// we should not be in a situation where we have ACLs without externalIDs
						// but if we do have such lame ACLs then they will interfere with AdminNetPol logic
						t.PlaceHolderACLTier,
					)
					purgeACL2.UUID = "purgeACL2-UUID"

					namespace1 := *newNamespace("namespace1")

					namespace1ASip4, _ := buildNamespaceAddressSets(namespace1.Name, []net.IP{})

					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					updateIDs := fakeController.getEgressFirewallACLDbIDs(namespace1.Name, 0)
					updateACL := libovsdbops.BuildACL(
						"",
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ip4.dst != 10.128.0.0/14",
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						nbdb.ACLSeverityInfo,
						false,
						updateIDs.GetExternalIDs(),
						nil,
						t.PlaceHolderACLTier,
					)
					updateACL.UUID = "updateACL-UUID"

					// this ACL is not in the egress firewall priority range and should be untouched
					ignoreACL := libovsdbops.BuildACL(
						"ignoreACL",
						nbdb.ACLDirectionFromLport,
						t.MinimumReservedEgressFirewallPriority-1,
						"",
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						nil,
						nil,
						// we should not be in a situation where we have unknown ACL that doesn't belong to any feature
						// but if we do have such lame ACLs then they will interfere with AdminNetPol logic
						t.PlaceHolderACLTier,
					)
					ignoreACL.UUID = "ignoreACL-UUID"

					nodeSwitch.ACLs = []string{purgeACL.UUID, purgeACL2.UUID, updateACL.UUID, ignoreACL.UUID}
					joinSwitch.ACLs = []string{purgeACL.UUID, purgeACL2.UUID, updateACL.UUID, ignoreACL.UUID}
					clusterPortGroup.ACLs = []string{purgeACL.UUID, purgeACL2.UUID, updateACL.UUID, ignoreACL.UUID}

					dbSetup := libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							purgeACL,
							purgeACL2,
							ignoreACL,
							updateACL,
							nodeSwitch,
							joinSwitch,
							clusterRouter,
							clusterPortGroup,
							namespace1ASip4,
						},
					}

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall})

					// All ACLs in the egress firewall priority range will be removed from the switches
					joinSwitch.ACLs = []string{ignoreACL.UUID}
					nodeSwitch.ACLs = []string{ignoreACL.UUID}
					// purgeACL will be deleted as its namespace doesn't exist
					clusterPortGroup.ACLs = []string{purgeACL2.UUID, updateACL.UUID, ignoreACL.UUID}

					// updateACL will be updated
					// Direction of both ACLs will be converted to
					updateACL.Direction = nbdb.ACLDirectionToLport
					newName := libovsdbutil.GetACLName(updateIDs)
					updateACL.Name = &newName
					// check severity was reset from default to nil
					updateACL.Severity = nil
					// match shouldn't have cluster exclusion
					asHash, _ := getNsAddrSetHashNames(namespace1.Name)
					updateACL.Match = "(ip4.dst == 1.2.3.4/23) && ip4.src == $" + asHash
					updateACL.Tier = t.DefaultACLTier // ensure the tier of the ACL is updated from 0 to 2

					expectedDatabaseState := []libovsdb.TestData{
						purgeACL,
						purgeACL2,
						ignoreACL,
						updateACL,
						nodeSwitch,
						joinSwitch,
						clusterRouter,
						clusterPortGroup,
						namespace1ASip4,
					}

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("reconciles an existing egressFirewall with IPv4 CIDR, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall})

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					asHash, _ := getNsAddrSetHashNames(namespace1.Name)
					dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 0)
					ipv4ACL := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(dbIDs),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $"+asHash,
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						"",
						false,
						dbIDs.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{ipv4ACL.UUID}
					expectedDatabaseState := append(initialData, ipv4ACL)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("reconciles an existing egressFirewall with IPv6 CIDR, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "2002::1234:abcd:ffff:c0a8:101/64",
							},
						},
					})

					config.IPv6Mode = true
					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall})

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, asHash6 := getNsAddrSetHashNames(namespace1.Name)
					dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 0)
					ipv6ACL := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(dbIDs),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority,
						"(ip6.dst == 2002::1234:abcd:ffff:c0a8:101/64) && ip6.src == $"+asHash6,
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						"",
						false,
						dbIDs.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					ipv6ACL.UUID = "ipv6ACL-UUID"

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{ipv6ACL.UUID}
					expectedDatabaseState := append(initialData, ipv6ACL)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
		})
		ginkgo.Context("during execution", func() {
			ginkgo.It(fmt.Sprintf("correctly creates an egressfirewall denying traffic udp traffic on port 100, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							Ports: []egressfirewallapi.EgressFirewallPort{
								{
									Protocol: "UDP",
									Port:     100,
								},
							},
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall})

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					asHash, _ := getNsAddrSetHashNames(namespace1.Name)
					dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 0)
					udpACL := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(dbIDs),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $"+asHash+" && ((udp && ( udp.dst == 100 )))",
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						dbIDs.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					udpACL.UUID = "udpACL-UUID"

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{udpACL.UUID}
					expectedDatabaseState := append(initialData, udpACL)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly deletes an egressfirewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							Ports: []egressfirewallapi.EgressFirewallPort{
								{
									Protocol: "TCP",
									Port:     100,
								},
							},
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.5/23",
							},
						},
					})

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall})

					asHash, _ := getNsAddrSetHashNames(namespace1.Name)
					dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 0)
					ipv4ACL := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(dbIDs),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.5/23) && ip4.src == $"+asHash+" && ((tcp && ( tcp.dst == 100 )))",
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						"",
						false,
						dbIDs.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{ipv4ACL.UUID}
					expectedDatabaseState := append(initialData, ipv4ACL)

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// ACL should be removed from the port group egfw is deleted
					clusterPortGroup.ACLs = []string{}
					// this ACL will be deleted when test server starts deleting dereferenced ACLs
					expectedDatabaseState = append(initialData, ipv4ACL)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly updates an egressfirewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					egressFirewall1 := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall})

					asHash, _ := getNsAddrSetHashNames(namespace1.Name)
					dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 0)
					ipv4ACL := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(dbIDs),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $"+asHash,
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						"",
						false,
						dbIDs.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{ipv4ACL.UUID}
					expectedDatabaseState := append(initialData, ipv4ACL)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall1.Namespace).Update(context.TODO(), egressFirewall1, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ipv4ACL.Action = nbdb.ACLActionDrop
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("egress firewall with node selector updates during node update, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				var err error
				nodeName := "node1"
				nodeIP := "9.9.9.9"

				app.Action = func(ctx *cli.Context) error {
					expectedOVNClusterRouter := newOVNClusterRouter()
					expectedClusterPortGroup := newClusterPortGroup()
					namespace1 := *newNamespace("namespace1")

					namespace1ASip4, _ := buildNamespaceAddressSets(namespace1.Name, []net.IP{})

					initialTestData := []libovsdbtest.TestData{
						expectedOVNClusterRouter,
						expectedClusterPortGroup,
						namespace1ASip4,
					}
					dbSetup := libovsdbtest.TestSetup{
						NBData: initialTestData,
					}

					labelKey := "name"
					labelValue := "test"
					selector := metav1.LabelSelector{MatchLabels: map[string]string{labelKey: labelValue}}
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								NodeSelector: &selector,
							},
						},
					})

					fakeOVN.startWithDBSetup(dbSetup,
						&egressfirewallapi.EgressFirewallList{
							Items: []egressfirewallapi.EgressFirewall{
								*egressFirewall,
							},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
						&v1.NodeList{
							Items: []v1.Node{
								{
									ObjectMeta: metav1.ObjectMeta{
										Name: nodeName,
									},
									Status: v1.NodeStatus{
										Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: nodeIP}},
									},
								},
							},
						})

					ginkgo.By("Startup with node add that wont match Egress Firewall nodeSelector")
					err = fakeOVN.controller.WatchEgressFirewall()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOVN.controller.WatchEgressFwNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(initialTestData))

					// update the node to match the selector
					patch := struct {
						Metadata map[string]interface{} `json:"metadata"`
					}{
						Metadata: map[string]interface{}{
							"labels": map[string]string{labelKey: labelValue},
						},
					}
					ginkgo.By("Updating a node to match nodeSelector on Egress Firewall")
					patchData, err := json.Marshal(&patch)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// trigger update event
					_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Patch(context.TODO(), nodeName,
						types.MergePatchType, patchData, metav1.PatchOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					asHash, _ := getNsAddrSetHashNames(namespace1.Name)
					dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 0)
					ipv4ACL := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(dbIDs),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority,
						fmt.Sprintf("(ip4.dst == %s) && ip4.src == $%s", nodeIP, asHash),
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						"",
						false,
						dbIDs.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"

					// new ACL will be added to the port group
					expectedClusterPortGroup.ACLs = []string{ipv4ACL.UUID}
					expectedDatabaseState := []libovsdb.TestData{expectedClusterPortGroup, ipv4ACL, expectedOVNClusterRouter, namespace1ASip4}

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					ginkgo.By("Updating a node to not match nodeSelector on Egress Firewall")
					patch.Metadata = map[string]interface{}{"labels": map[string]string{labelKey: noneMatch}}
					patchData, err = json.Marshal(&patch)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// trigger update event
					_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Patch(context.TODO(), nodeName,
						types.MergePatchType, patchData, metav1.PatchOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					expectedClusterPortGroup.ACLs = []string{}
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err = app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly retries deleting an egressfirewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					namespace1ASip4, _ := buildNamespaceAddressSets(namespace1.Name, []net.IP{})

					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							Ports: []egressfirewallapi.EgressFirewallPort{
								{
									Protocol: "TCP",
									Port:     100,
								},
							},
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.5/23",
							},
						},
					})
					fakeOVN.startWithDBSetup(dbSetup,
						&egressfirewallapi.EgressFirewallList{
							Items: []egressfirewallapi.EgressFirewall{
								*egressFirewall,
							},
						},
						&v1.NodeList{
							Items: []v1.Node{
								{
									Status: v1.NodeStatus{
										Phase: v1.NodeRunning,
									},
									ObjectMeta: newObjectMeta(node1Name, ""),
								},
								{
									Status: v1.NodeStatus{
										Phase: v1.NodeRunning,
									},
									ObjectMeta: newObjectMeta(node2Name, ""),
								},
							},
						})

					fakeOVN.controller.WatchEgressFirewall()

					asHash, _ := getNsAddrSetHashNames(namespace1.Name)
					dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 0)
					ipv4ACL := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(dbIDs),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.5/23) && ip4.src == $"+asHash+" && ((tcp && ( tcp.dst == 100 )))",
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						"",
						false,
						dbIDs.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{ipv4ACL.UUID}
					expectedDatabaseState := append(initialData, ipv4ACL, namespace1ASip4)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					ginkgo.By("Bringing down NBDB")
					// inject transient problem, nbdb is down
					fakeOVN.controller.nbClient.Close()
					gomega.Eventually(func() bool {
						return fakeOVN.controller.nbClient.Connected()
					}).Should(gomega.BeFalse())

					err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// sleep long enough for TransactWithRetry to fail, causing egress firewall Add to fail
					time.Sleep(t.OVSDBTimeout + time.Second)
					// check to see if the retry cache has an entry for this egress firewall
					key := getEgressFirewallNamespacedName(egressFirewall)
					ginkgo.By("retry entry: old obj should not be nil, new obj should be nil")
					retry.CheckRetryObjectMultipleFieldsEventually(
						key,
						fakeOVN.controller.retryEgressFirewalls,
						gomega.Not(gomega.BeNil()), // oldObj should not be nil
						gomega.BeNil(),             // newObj should be nil
					)

					connCtx, cancel := context.WithTimeout(context.Background(), t.OVSDBTimeout)
					defer cancel()
					resetNBClient(connCtx, fakeOVN.controller.nbClient)
					retry.SetRetryObjWithNoBackoff(key, fakeOVN.controller.retryEgressFirewalls)
					fakeOVN.controller.retryEgressFirewalls.RequestRetryObjs()

					// ACL should be removed from the port group after egfw is deleted
					clusterPortGroup.ACLs = []string{}
					// this ACL will be deleted when test server starts deleting dereferenced ACLs
					expectedDatabaseState = append(initialData, ipv4ACL, namespace1ASip4)

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					// check the cache no longer has the entry
					retry.CheckRetryObjectEventually(key, false, fakeOVN.controller.retryEgressFirewalls)
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly retries adding and updating an egressfirewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					egressFirewall1 := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall})

					asHash, _ := getNsAddrSetHashNames(namespace1.Name)
					dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 0)
					ipv4ACL := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(dbIDs),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $"+asHash,
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						"",
						false,
						dbIDs.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{ipv4ACL.UUID}
					expectedDatabaseState := append(initialData, ipv4ACL)

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					ginkgo.By("Bringing down NBDB")
					// inject transient problem, nbdb is down
					fakeOVN.controller.nbClient.Close()
					gomega.Eventually(func() bool {
						return fakeOVN.controller.nbClient.Connected()
					}).Should(gomega.BeFalse())

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall1.Namespace).Update(context.TODO(), egressFirewall1, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// sleep long enough for TransactWithRetry to fail, causing egress firewall Add to fail
					time.Sleep(t.OVSDBTimeout + time.Second)
					// check to see if the retry cache has an entry for this egress firewall
					key, err := retry.GetResourceKey(egressFirewall)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ginkgo.By("retry entry: old obj should not be nil, new obj should not be nil")
					retry.CheckRetryObjectMultipleFieldsEventually(
						key,
						fakeOVN.controller.retryEgressFirewalls,
						gomega.Not(gomega.BeNil()), // oldObj should not be nil
						gomega.Not(gomega.BeNil()), // newObj should not be nil
					)

					connCtx, cancel := context.WithTimeout(context.Background(), t.OVSDBTimeout)
					defer cancel()
					ginkgo.By("bringing up NBDB and requesting retry of entry")
					resetNBClient(connCtx, fakeOVN.controller.nbClient)

					retry.SetRetryObjWithNoBackoff(key, fakeOVN.controller.retryEgressFirewalls)
					ginkgo.By("request immediate retry object")
					fakeOVN.controller.retryEgressFirewalls.RequestRetryObjs()
					// check the cache no longer has the entry
					retry.CheckRetryObjectEventually(key, false, fakeOVN.controller.retryEgressFirewalls)
					ipv4ACL.Action = nbdb.ACLActionDrop
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("correctly updates an egressfirewall's ACL logging, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall})

					asHash, _ := getNsAddrSetHashNames(namespace1.Name)
					dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 0)
					ipv4ACL := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(dbIDs),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $"+asHash,
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						"",
						false,
						dbIDs.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{ipv4ACL.UUID}
					expectedDatabaseState := append(initialData, ipv4ACL)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					// get the current namespace
					namespace, err := fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// enable ACL logging with severity alert, alert
					logSeverity := "alert"
					updatedLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, logSeverity, logSeverity)
					namespace.Annotations[util.AclLoggingAnnotation] = updatedLogSeverity
					_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespace, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// eventually, we should see the changes in the namespace reflected in the database
					ipv4ACL.Log = true
					ipv4ACL.Severity = &logSeverity
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			for _, ipMode := range []string{"IPv4", "IPv6"} {
				ginkgo.It(fmt.Sprintf("configures egress firewall correctly with node selector, gateway mode: %s, IP mode: %s", gwMode, ipMode), func() {
					nodeIP := "10.10.10.1"
					nodeIP6 := "fc00:f853:ccd:e793::2"
					config.Gateway.Mode = gwMode
					var nodeAddr v1.NodeAddress
					if ipMode == "IPv4" {
						config.IPv4Mode = true
						config.IPv6Mode = false
						nodeAddr = v1.NodeAddress{v1.NodeInternalIP, nodeIP}

					} else {
						config.IPv4Mode = false
						config.IPv6Mode = true
						nodeAddr = v1.NodeAddress{v1.NodeInternalIP, nodeIP6}
					}
					app.Action = func(ctx *cli.Context) error {
						labelKey := "name"
						labelValue := "test"
						selector := metav1.LabelSelector{MatchLabels: map[string]string{labelKey: labelValue}}
						namespace1 := *newNamespace("namespace1")
						namespaceASip4, namespaceASip6 := buildNamespaceAddressSets(namespace1.Name, []net.IP{})
						if config.IPv4Mode {
							initialData = append(initialData, namespaceASip4)
						}
						if config.IPv6Mode {
							initialData = append(initialData, namespaceASip6)
						}

						egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
							{
								Type: "Allow",
								To: egressfirewallapi.EgressFirewallDestination{
									NodeSelector: &selector,
								},
							},
						})
						mdata := newObjectMeta(node1Name, "")
						mdata.Labels = map[string]string{labelKey: labelValue}
						fakeOVN.startWithDBSetup(dbSetup,
							&egressfirewallapi.EgressFirewallList{
								Items: []egressfirewallapi.EgressFirewall{
									*egressFirewall,
								},
							},
							&v1.NamespaceList{
								Items: []v1.Namespace{
									namespace1,
								},
							},
							&v1.NodeList{
								Items: []v1.Node{
									{
										Status: v1.NodeStatus{
											Phase:     v1.NodeRunning,
											Addresses: []v1.NodeAddress{nodeAddr},
										},
										ObjectMeta: mdata,
									},
								},
							})
						err := fakeOVN.controller.WatchNamespaces()
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						err = fakeOVN.controller.WatchEgressFirewall()
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						err = fakeOVN.controller.WatchEgressFwNodes()
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						asHashv4, asHashv6 := getNsAddrSetHashNames(namespace1.Name)
						var match string
						if config.IPv4Mode {
							match = fmt.Sprintf("(ip4.dst == %s) && ip4.src == $%s",
								nodeIP, asHashv4)
						} else {
							match = fmt.Sprintf("(ip6.dst == %s) && ip6.src == $%s",
								nodeIP6, asHashv6)
						}
						dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 0)
						acl := libovsdbops.BuildACL(
							libovsdbutil.GetACLName(dbIDs),
							nbdb.ACLDirectionToLport,
							t.EgressFirewallStartPriority,
							match,
							nbdb.ACLActionAllow,
							t.OvnACLLoggingMeter,
							"",
							false,
							dbIDs.GetExternalIDs(),
							nil,
							t.DefaultACLTier,
						)
						acl.UUID = "ACL-UUID"

						clusterPortGroup.ACLs = []string{acl.UUID}
						expectedDatabaseState := append(initialData, acl)

						gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

						return nil
					}

					err := app.Run([]string{app.Name})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				})
			}
			ginkgo.It(fmt.Sprintf("correctly creates an egressfirewall with subnet exclusion, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					clusterSubnetStr := "10.128.0.0/14"
					_, clusterSubnet, _ := net.ParseCIDR(clusterSubnetStr)
					config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: clusterSubnet}}

					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "0.0.0.0/0",
							},
						},
					})
					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall})

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					asHash, _ := getNsAddrSetHashNames(namespace1.Name)
					dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 0)
					acl := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(dbIDs),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 0.0.0.0/0 && ip4.dst != "+clusterSubnetStr+") && ip4.src == $"+asHash,
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						dbIDs.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					acl.UUID = "acl-UUID"

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{acl.UUID}
					expectedDatabaseState := append(initialData, acl)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly creates an egressfirewall for namespace name > 43 symbols, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					// 52 characters namespace
					namespace1 := *newNamespace("abcdefghigklmnopqrstuvwxyzabcdefghigklmnopqrstuvwxyz")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.5/23",
							},
						},
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "2.2.3.5/23",
							},
						},
					})

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall})

					asHash, _ := getNsAddrSetHashNames(namespace1.Name)
					aclIDs1 := fakeOVN.controller.getEgressFirewallACLDbIDs(namespace1.Name, 0)
					ipv4ACL1 := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(aclIDs1),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.5/23) && ip4.src == $"+asHash,
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						"",
						false,
						aclIDs1.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					ipv4ACL1.UUID = "ipv4ACL1-UUID"

					aclIDs2 := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 1)
					ipv4ACL2 := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(aclIDs2),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority-1,
						"(ip4.dst == 2.2.3.5/23) && ip4.src == $"+asHash,
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						"",
						false,
						aclIDs2.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					ipv4ACL2.UUID = "ipv4ACL2-UUID"

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{ipv4ACL1.UUID, ipv4ACL2.UUID}
					expectedDatabaseState := append(initialData, ipv4ACL1, ipv4ACL2)

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					err := fakeOVN.controller.syncEgressFirewall([]interface{}{egressFirewall})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// ACL should be removed from the port group egfw is deleted
					clusterPortGroup.ACLs = []string{}
					// this ACL will be deleted when test server starts deleting dereferenced ACLs
					expectedDatabaseState = append(initialData, ipv4ACL1, ipv4ACL2)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly deletes object that failed to be created, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								// wrong CIDR format, creation will fail
								CIDRSelector: "1.2.3.4",
							},
						},
					})
					startOvn(dbSetup, []v1.Namespace{namespace1}, nil)

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// creation will fail, check retry object exists
					efKey, err := retry.GetResourceKey(egressFirewall)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					retry.CheckRetryObjectEventually(efKey, true, fakeOVN.controller.retryEgressFirewalls)

					// delete wrong object
					err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Delete(context.TODO(), egressFirewall.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// retry object should not be present
					gomega.Eventually(func() bool {
						return retry.CheckRetryObj(efKey, fakeOVN.controller.retryEgressFirewalls)
					}, time.Second).Should(gomega.BeFalse())

					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly cleans up object that failed to be created, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "a.b.c",
							},
						},
					})
					startOvn(dbSetup, []v1.Namespace{namespace1}, nil)

					// dns-based rule creation will fail, because addressset factory is nil
					fakeOVN.controller.egressFirewallDNS = &EgressDNS{
						dnsEntries:        make(map[string]*dnsEntry),
						addressSetFactory: nil,

						added:    make(chan struct{}, 1),
						deleted:  make(chan string, 1),
						stopChan: make(chan struct{}),
					}

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// creation will fail, check retry object exists
					efKey, err := retry.GetResourceKey(egressFirewall)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					retry.CheckRetryObjectEventually(efKey, true, fakeOVN.controller.retryEgressFirewalls)

					// check first acl was successfully created
					asHash, _ := getNsAddrSetHashNames(namespace1.Name)
					dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 0)
					acl := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(dbIDs),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $"+asHash,
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						dbIDs.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					acl.UUID = "acl-UUID"
					clusterPortGroup.ACLs = []string{acl.UUID}
					expectedDatabaseState := append(initialData, acl)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					// delete wrong object
					err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Delete(context.TODO(), egressFirewall.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// retry object should not be present
					gomega.Eventually(func() bool {
						return retry.CheckRetryObj(efKey, fakeOVN.controller.retryEgressFirewalls)
					}, time.Second).Should(gomega.BeFalse())

					// check created acl will be cleaned up on delete
					// acl will be dereferenced, but not deleted by the test server
					clusterPortGroup.ACLs = []string{}
					expectedDatabaseState = append(initialData, acl)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
		})
	}
})

var _ = ginkgo.Describe("OVN test basic functions", func() {
	var (
		app       *cli.App
		fakeOVN   *FakeOVN
		nodeLabel = map[string]string{"use": "this"}
	)

	const (
		node1Name string = "node1"
		node1Addr string = "9.9.9.9"
		node2Name string = "node2"
		node2Addr string = "10.10.10.10"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each test
		config.PrepareTestConfig()
		config.Gateway.Mode = config.GatewayModeShared
		config.OVNKubernetesFeature.EnableEgressFirewall = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		dbSetup := libovsdbtest.TestSetup{}
		fakeOVN = NewFakeOVN(false)
		a := newObjectMeta(node1Name, "")
		a.Labels = nodeLabel
		node1 := v1.Node{
			Status: v1.NodeStatus{
				Phase:     v1.NodeRunning,
				Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: node1Addr}},
			},
			ObjectMeta: a,
		}
		node2 := v1.Node{
			Status: v1.NodeStatus{
				Phase:     v1.NodeRunning,
				Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: node2Addr}},
			},
			ObjectMeta: newObjectMeta(node2Name, ""),
		}
		fakeOVN.startWithDBSetup(dbSetup, &v1.NodeList{Items: []v1.Node{node1, node2}})
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
	})

	ginkgo.It("computes correct L4Match", func() {
		type testcase struct {
			ports         []egressfirewallapi.EgressFirewallPort
			expectedMatch string
		}
		testcases := []testcase{
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
				},
				expectedMatch: "((tcp && ( tcp.dst == 100 )))",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
					{
						Protocol: "UDP",
					},
				},
				expectedMatch: "((udp) || (tcp && ( tcp.dst == 100 )))",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
					{
						Protocol: "SCTP",
						Port:     13,
					},
					{
						Protocol: "TCP",
						Port:     102,
					},
					{
						Protocol: "UDP",
						Port:     400,
					},
				},
				expectedMatch: "((udp && ( udp.dst == 400 )) || (tcp && ( tcp.dst == 100 || tcp.dst == 102 )) || (sctp && ( sctp.dst == 13 )))",
			},
		}
		for _, test := range testcases {
			l4Match := egressGetL4Match(test.ports)
			gomega.Expect(test.expectedMatch).To(gomega.Equal(l4Match))
		}
	})
	ginkgo.It("computes correct match function", func() {
		type testcase struct {
			clusterSubnets []string
			ipv4source     string
			ipv6source     string
			ipv4Mode       bool
			ipv6Mode       bool
			destinations   []matchTarget
			ports          []egressfirewallapi.EgressFirewallPort
			output         string
		}
		testcases := []testcase{
			{
				clusterSubnets: []string{"10.128.0.0/14"},
				ipv4source:     "testv4",
				ipv6source:     "",
				ipv4Mode:       true,
				ipv6Mode:       false,
				destinations:   []matchTarget{{matchKindV4CIDR, "1.2.3.4/32", false}},
				ports:          nil,
				output:         "(ip4.dst == 1.2.3.4/32) && ip4.src == $testv4",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				ipv4source:     "testv4",
				ipv6source:     "testv6",
				ipv4Mode:       true,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV4CIDR, "1.2.3.4/32", false}},
				ports:          nil,
				output:         "(ip4.dst == 1.2.3.4/32) && ip4.src == $testv4",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				ipv4source:     "testv4",
				ipv6source:     "testv6",
				ipv4Mode:       true,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV4AddressSet, "destv4", false}, {matchKindV6AddressSet, "destv6", false}},
				ports:          nil,
				output:         "(ip4.dst == $destv4 || ip6.dst == $destv6) && (ip4.src == $testv4 || ip6.src == $testv6)",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14"},
				ipv4source:     "testv4",
				ipv6source:     "",
				ipv4Mode:       true,
				ipv6Mode:       false,
				destinations:   []matchTarget{{matchKindV4AddressSet, "destv4", false}, {matchKindV6AddressSet, "", false}},
				ports:          nil,
				output:         "(ip4.dst == $destv4) && ip4.src == $testv4",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				ipv4source:     "testv4",
				ipv6source:     "testv6",
				ipv4Mode:       true,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV6CIDR, "2001::/64", false}},
				ports:          nil,
				output:         "(ip6.dst == 2001::/64) && ip6.src == $testv6",
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				ipv4source:     "",
				ipv6source:     "testv6",
				ipv4Mode:       false,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV6AddressSet, "destv6", false}},
				ports:          nil,
				output:         "(ip6.dst == $destv6) && ip6.src == $testv6",
			},
			// with cluster subnet exclusion
			{
				clusterSubnets: []string{"10.128.0.0/14"},
				ipv4source:     "testv4",
				ipv6source:     "",
				ipv4Mode:       true,
				ipv6Mode:       false,
				destinations:   []matchTarget{{matchKindV4CIDR, "1.2.3.4/32", true}},
				ports:          nil,
				output:         "(ip4.dst == 1.2.3.4/32 && ip4.dst != 10.128.0.0/14) && ip4.src == $testv4",
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				ipv4source:     "",
				ipv6source:     "testv6",
				ipv4Mode:       false,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV6AddressSet, "destv6", true}},
				ports:          nil,
				output:         "(ip6.dst == $destv6) && ip6.src == $testv6",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				ipv4source:     "testv4",
				ipv6source:     "testv6",
				ipv4Mode:       true,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV4CIDR, "1.2.3.4/32", true}},
				ports:          nil,
				output:         "(ip4.dst == 1.2.3.4/32 && ip4.dst != 10.128.0.0/14) && ip4.src == $testv4",
			},
		}

		for _, tc := range testcases {
			config.IPv4Mode = tc.ipv4Mode
			config.IPv6Mode = tc.ipv6Mode
			subnets := []config.CIDRNetworkEntry{}
			for _, clusterCIDR := range tc.clusterSubnets {
				_, cidr, _ := net.ParseCIDR(clusterCIDR)
				subnets = append(subnets, config.CIDRNetworkEntry{CIDR: cidr})
			}
			config.Default.ClusterSubnets = subnets

			config.Gateway.Mode = config.GatewayModeShared
			matchExpression := generateMatch(tc.ipv4source, tc.ipv6source, tc.destinations, tc.ports)
			gomega.Expect(matchExpression).To(gomega.Equal(tc.output))
		}
	})
	ginkgo.It("correctly parses egressFirewallRules", func() {
		type testcase struct {
			egressFirewallRule egressfirewallapi.EgressFirewallRule
			id                 int
			err                bool
			errOutput          string
			output             egressFirewallRule
			clusterSubnets     []string
		}
		testcases := []testcase{
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "1.2.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "1.2.3.4/32"},
				},
			},
			{
				clusterSubnets: []string{"10.128.0.0/16"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "1.2.3./32"},
				},
				id:        1,
				err:       true,
				errOutput: "invalid CIDR address: 1.2.3./32",
				output:    egressFirewallRule{},
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002::1235:abcd:ffff:c0a8:101/64"},
				},
				id:  2,
				err: false,
				output: egressFirewallRule{
					id:     2,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002::1235:abcd:ffff:c0a8:101/64"},
				},
			},
			// check clusterSubnet intersection
			{
				clusterSubnets: []string{"10.128.0.0/16"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "1.2.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "1.2.3.4/32", clusterSubnetIntersection: false},
				},
			},
			{
				clusterSubnets: []string{"10.128.0.0/16"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "10.128.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "10.128.3.4/32", clusterSubnetIntersection: true},
				},
			},
			{
				clusterSubnets: []string{"10.128.0.0/16"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "10.128.3.0/24"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "10.128.3.0/24", clusterSubnetIntersection: true},
				},
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002:0:0:1234:0001::/80"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002:0:0:1234:0001::/80", clusterSubnetIntersection: true},
				},
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002:0:0:1235::/80"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002:0:0:1235::/80", clusterSubnetIntersection: false},
				},
			},
			// dual stack
			{
				clusterSubnets: []string{"10.128.0.0/16", "2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "10.128.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "10.128.3.4/32", clusterSubnetIntersection: true},
				},
			},
			{
				clusterSubnets: []string{"10.128.0.0/16", "2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002:0:0:1234:0001::/80"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002:0:0:1234:0001::/80", clusterSubnetIntersection: true},
				},
			},
			// nodeSelector tests
			// selector matches nothing
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To: egressfirewallapi.EgressFirewallDestination{NodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"no": "match"}}},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to: destination{nodeAddrs: sets.New[string](), nodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"no": "match"}}},
				},
			},
			// empty selector, match all
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{NodeSelector: &metav1.LabelSelector{}},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{nodeAddrs: sets.New("9.9.9.9", "10.10.10.10"), nodeSelector: &metav1.LabelSelector{}},
				},
			},
			// match one node
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{NodeSelector: &metav1.LabelSelector{MatchLabels: nodeLabel}},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{nodeAddrs: sets.New(node1Addr), nodeSelector: &metav1.LabelSelector{MatchLabels: nodeLabel}},
				},
			},
		}
		for _, tc := range testcases {
			subnets := []config.CIDRNetworkEntry{}
			for _, clusterCIDR := range tc.clusterSubnets {
				_, cidr, _ := net.ParseCIDR(clusterCIDR)
				subnets = append(subnets, config.CIDRNetworkEntry{CIDR: cidr})
			}
			config.Default.ClusterSubnets = subnets
			output, err := fakeOVN.controller.newEgressFirewallRule(tc.egressFirewallRule, tc.id)
			if tc.err == true {
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(tc.errOutput).To(gomega.Equal(err.Error()))
			} else {
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(*output).To(gomega.Equal(tc.output))
			}
		}
	})
})

//helper functions to help test egressfirewallDNS

// Create an EgressDNS object without the Sync function
// To make it easier to mock EgressFirewall functionality create an egressFirewall
// without the go routine of the sync function

// GetDNSEntryForTest Gets a dnsEntry from a EgressDNS object for testing
func (e *EgressDNS) GetDNSEntryForTest(dnsName string) (map[string]struct{}, []net.IP, addressset.AddressSet, error) {
	if e.dnsEntries[dnsName] == nil {
		return nil, nil, nil, fmt.Errorf("there is no dnsEntry for dnsName: %s", dnsName)
	}
	return e.dnsEntries[dnsName].namespaces, e.dnsEntries[dnsName].dnsResolves, e.dnsEntries[dnsName].dnsAddressSet, nil
}

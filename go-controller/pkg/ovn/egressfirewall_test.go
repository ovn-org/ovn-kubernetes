package ovn

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	t "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

var _ = ginkgo.Describe("OVN EgressFirewall Operations for local gateway mode", func() {
	var (
		app     *cli.App
		fakeOVN *FakeOVN
	)
	const (
		node1Name string = "node1"
		node2Name string = "node2"
	)

	clusterRouter := &nbdb.LogicalRouter{
		UUID: t.OVNClusterRouter + "-UUID",
		Name: t.OVNClusterRouter,
	}

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.Gateway.Mode = config.GatewayModeLocal
		config.OVNKubernetesFeature.EnableEgressFirewall = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOVN = NewFakeOVN()
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
	})

	ginkgo.Context("on startup", func() {
		ginkgo.It("reconciles existing and non-existing egressfirewalls", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
				)

				purgeACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionFromLPort,
					t.EgressFirewallStartPriority,
					"",
					nbdb.ACLActionDrop,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "none"},
					nil,
				)
				purgeACL.UUID = "purgeACL-UUID"

				keepACL := libovsdbops.BuildACL(
					"",
					t.DirectionFromLPort,
					t.EgressFirewallStartPriority-1,
					"",
					nbdb.ACLActionDrop,
					"",
					"",
					false,
					map[string]string{"egressFirewall": "default"},
					nil,
				)
				keepACL.UUID = "keepACL-UUID"

				// this ACL is not in the egress firewall priority range and should be untouched
				otherACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority-1),
					t.DirectionFromLPort,
					t.MinimumReservedEgressFirewallPriority-1,
					"",
					nbdb.ACLActionDrop,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "default"},
					nil,
				)
				otherACL.UUID = "otherACL-UUID"

				InitialNodeSwitch := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
					ACLs: []string{purgeACL.UUID, keepACL.UUID},
				}
				InitialJoinSwitch := &nbdb.LogicalSwitch{
					UUID: "join-UUID",
					Name: "join",
					ACLs: []string{purgeACL.UUID, keepACL.UUID},
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						otherACL,
						purgeACL,
						keepACL,
						InitialNodeSwitch,
						InitialJoinSwitch,
						clusterRouter,
					},
				}

				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NodeList{
						Items: []v1.Node{
							{
								Status: v1.NodeStatus{
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})

				// only create one egressFirewall
				_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls("default").Create(context.TODO(), &egressfirewallapi.EgressFirewall{}, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.controller.WatchEgressFirewall()

				// Both ACLS will be removed from the join switch
				finalJoinSwitch := &nbdb.LogicalSwitch{
					UUID: InitialJoinSwitch.UUID,
					Name: InitialJoinSwitch.Name,
				}

				// stale ACL will be removed from the node switch
				finalNodeSwitch := &nbdb.LogicalSwitch{
					UUID: InitialNodeSwitch.UUID,
					Name: InitialNodeSwitch.Name,
					ACLs: []string{keepACL.UUID},
				}

				// Direction of both ACLs will be converted to
				keepACL.Direction = t.DirectionToLPort
				newName := buildEgressFwAclName("default", t.EgressFirewallStartPriority-1)
				meter := t.OvnACLLoggingMeter
				severity := defaultACLLoggingSeverity
				keepACL.Name = &newName
				keepACL.Direction = t.DirectionToLPort
				keepACL.Meter = &meter
				keepACL.Severity = &severity
				keepACL.Log = false

				expectedDatabaseState := []libovsdb.TestData{
					otherACL,
					keepACL,
					finalNodeSwitch,
					finalJoinSwitch,
					clusterRouter,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})
		ginkgo.It("reconciles an existing egressFirewall with IPv4 CIDR", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
				)

				InitialNodeSwitch := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						InitialNodeSwitch,
						clusterRouter,
					},
				}

				namespace1 := *newNamespace("namespace1")
				egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Allow",
						To: egressfirewallapi.EgressFirewallDestination{
							CIDRSelector: "1.2.3.4/23",
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
					}, &v1.NodeList{
						Items: []v1.Node{
							{
								Status: v1.NodeStatus{
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})

				fakeOVN.controller.WatchNamespaces()
				fakeOVN.controller.WatchEgressFirewall()

				_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ipv4ACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ip4.dst != 10.128.0.0/14",
					nbdb.ACLActionAllow,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				ipv4ACL.UUID = "ipv4ACL-UUID"

				// new ACL will be added to the switch
				finalNodeSwitch := &nbdb.LogicalSwitch{
					UUID: InitialNodeSwitch.UUID,
					Name: InitialNodeSwitch.Name,
					ACLs: []string{ipv4ACL.UUID},
				}

				expectedDatabaseState := []libovsdb.TestData{
					ipv4ACL,
					finalNodeSwitch,
					clusterRouter,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})
		ginkgo.It("reconciles an existing egressFirewall with IPv6 CIDR", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
				)

				InitialNodeSwitch := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						InitialNodeSwitch,
						clusterRouter,
					},
				}

				namespace1 := *newNamespace("namespace1")
				egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Allow",
						To: egressfirewallapi.EgressFirewallDestination{
							CIDRSelector: "2002::1234:abcd:ffff:c0a8:101/64",
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
					}, &v1.NodeList{
						Items: []v1.Node{
							{
								Status: v1.NodeStatus{
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})
				config.IPv6Mode = true
				err := fakeOVN.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOVN.controller.WatchEgressFirewall()

				_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ipv6ACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip6.dst == 2002::1234:abcd:ffff:c0a8:101/64) && (ip4.src == $a10481622940199974102 || ip6.src == $a10481620741176717680) && ip4.dst != 10.128.0.0/14",
					nbdb.ACLActionAllow,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				ipv6ACL.UUID = "ipv6ACL-UUID"

				// new ACL will be added to the switch
				finalNodeSwitch := &nbdb.LogicalSwitch{
					UUID: InitialNodeSwitch.UUID,
					Name: InitialNodeSwitch.Name,
					ACLs: []string{ipv6ACL.UUID},
				}

				expectedDatabaseState := []libovsdb.TestData{
					ipv6ACL,
					finalNodeSwitch,
					clusterRouter,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})
	})
	ginkgo.Context("during execution", func() {
		ginkgo.It("correctly creates an egressfirewall denying traffic udp traffic on port 100", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
				)

				InitialNodeSwitch := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						InitialNodeSwitch,
						clusterRouter,
					},
				}

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
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})

				err := fakeOVN.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.controller.WatchEgressFirewall()

				udpACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ((udp && ( udp.dst == 100 ))) && ip4.dst != 10.128.0.0/14",
					nbdb.ACLActionDrop,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				udpACL.UUID = "udpACL-UUID"

				// new ACL will be added to the switch
				finalNodeSwitch := &nbdb.LogicalSwitch{
					UUID: InitialNodeSwitch.UUID,
					Name: InitialNodeSwitch.Name,
					ACLs: []string{udpACL.UUID},
				}

				expectedDatabaseState := []libovsdb.TestData{
					udpACL,
					finalNodeSwitch,
					clusterRouter,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("correctly deletes an egressfirewall", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
					node2Name string = "node2"
				)

				nodeSwitch1 := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
				}
				nodeSwitch2 := &nbdb.LogicalSwitch{
					UUID: node2Name + "-UUID",
					Name: node2Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						nodeSwitch1,
						nodeSwitch2,
						clusterRouter,
					},
				}

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
					}, &v1.NodeList{
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

				fakeOVN.controller.WatchNamespaces()
				fakeOVN.controller.WatchEgressFirewall()

				ipv4ACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip4.dst == 1.2.3.5/23) && ip4.src == $a10481622940199974102 && ((tcp && ( tcp.dst == 100 ))) && ip4.dst != 10.128.0.0/14",
					nbdb.ACLActionAllow,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				ipv4ACL.UUID = "ipv4ACL-UUID"

				// new ACL will be added to the switches
				nodeSwitch1.ACLs = []string{ipv4ACL.UUID}
				nodeSwitch2.ACLs = []string{ipv4ACL.UUID}

				expectedDatabaseState := []libovsdb.TestData{
					ipv4ACL,
					nodeSwitch1,
					nodeSwitch2,
					clusterRouter,
				}

				gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

				err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// ACL should be removed from switches after egfw is deleted
				nodeSwitch1.ACLs = []string{}
				nodeSwitch2.ACLs = []string{}
				expectedDatabaseState = []libovsdb.TestData{
					nodeSwitch1,
					nodeSwitch2,
					clusterRouter,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("correctly updates an egressfirewall", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
				)

				InitialNodeSwitch := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						InitialNodeSwitch,
						clusterRouter,
					},
				}

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
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})

				err := fakeOVN.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOVN.controller.WatchEgressFirewall()

				ipv4ACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ip4.dst != 10.128.0.0/14",
					nbdb.ACLActionAllow,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				ipv4ACL.UUID = "ipv4ACL-UUID"

				// new ACL will be added to the switch
				finalNodeSwitch := &nbdb.LogicalSwitch{
					UUID: InitialNodeSwitch.UUID,
					Name: InitialNodeSwitch.Name,
					ACLs: []string{ipv4ACL.UUID},
				}

				// new ACL will be added to the switch
				expectedDatabaseState := []libovsdb.TestData{
					ipv4ACL,
					finalNodeSwitch,
					clusterRouter,
				}

				gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

				_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
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
		ginkgo.It("correctly retries deleting an egressfirewall", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
					node2Name string = "node2"
				)

				nodeSwitch1 := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
				}
				nodeSwitch2 := &nbdb.LogicalSwitch{
					UUID: node2Name + "-UUID",
					Name: node2Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						nodeSwitch1,
						nodeSwitch2,
						clusterRouter,
					},
				}

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

				ipv4ACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip4.dst == 1.2.3.5/23) && ip4.src == $a10481622940199974102 && ((tcp && ( tcp.dst == 100 ))) && ip4.dst != 10.128.0.0/14",
					nbdb.ACLActionAllow,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				ipv4ACL.UUID = "ipv4ACL-UUID"

				// new ACL will be added to the switches
				nodeSwitch1.ACLs = []string{ipv4ACL.UUID}
				nodeSwitch2.ACLs = []string{ipv4ACL.UUID}

				expectedDatabaseState := []libovsdb.TestData{
					ipv4ACL,
					nodeSwitch1,
					nodeSwitch2,
					clusterRouter,
				}

				gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

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
				gomega.Eventually(func() *retryObjEntry {
					return fakeOVN.controller.retryEgressFirewalls.getObjRetryEntry(key)
				}).ShouldNot(gomega.BeNil())
				retryEntry := fakeOVN.controller.retryEgressFirewalls.getObjRetryEntry(key)
				ginkgo.By("retry entry new obj should be nil")
				gomega.Expect(retryEntry.newObj).To(gomega.BeNil())
				ginkgo.By("retry entry old obj should not be nil")
				gomega.Expect(retryEntry.oldObj).NotTo(gomega.BeNil())

				connCtx, cancel := context.WithTimeout(context.Background(), t.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOVN.controller.nbClient)
				fakeOVN.controller.retryEgressFirewalls.requestRetryObjs()

				// ACL should be removed from switches after egfw is deleted
				nodeSwitch1.ACLs = []string{}
				nodeSwitch2.ACLs = []string{}
				expectedDatabaseState = []libovsdb.TestData{
					nodeSwitch1,
					nodeSwitch2,
					clusterRouter,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				// check the cache no longer has the entry
				gomega.Eventually(func() *retryObjEntry {
					return fakeOVN.controller.retryEgressFirewalls.getObjRetryEntry(key)
				}).Should(gomega.BeNil())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly retries adding and updating an egressfirewall", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
				)

				InitialNodeSwitch := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						InitialNodeSwitch,
						clusterRouter,
					},
				}

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
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})

				fakeOVN.controller.WatchNamespaces()
				fakeOVN.controller.WatchEgressFirewall()

				ipv4ACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ip4.dst != 10.128.0.0/14",
					nbdb.ACLActionAllow,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				ipv4ACL.UUID = "ipv4ACL-UUID"

				// new ACL will be added to the switch
				finalNodeSwitch := &nbdb.LogicalSwitch{
					UUID: InitialNodeSwitch.UUID,
					Name: InitialNodeSwitch.Name,
					ACLs: []string{ipv4ACL.UUID},
				}

				// new ACL will be added to the switch
				expectedDatabaseState := []libovsdb.TestData{
					ipv4ACL,
					finalNodeSwitch,
					clusterRouter,
				}

				gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))
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
				key := getEgressFirewallNamespacedName(egressFirewall)
				gomega.Eventually(func() *retryObjEntry {
					return fakeOVN.controller.retryEgressFirewalls.getObjRetryEntry(key)
				}).ShouldNot(gomega.BeNil())
				retryEntry := fakeOVN.controller.retryEgressFirewalls.getObjRetryEntry(key)
				ginkgo.By("retry entry new obj should not be nil")
				gomega.Expect(retryEntry.newObj).NotTo(gomega.BeNil())
				ginkgo.By("retry entry old obj should not be nil")
				gomega.Expect(retryEntry.oldObj).NotTo(gomega.BeNil())
				connCtx, cancel := context.WithTimeout(context.Background(), t.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOVN.controller.nbClient)
				fakeOVN.controller.retryEgressFirewalls.requestRetryObjs()
				// check the cache no longer has the entry
				gomega.Eventually(func() *retryObjEntry {
					return fakeOVN.controller.retryEgressFirewalls.getObjRetryEntry(key)
				}).Should(gomega.BeNil())
				ipv4ACL.Action = nbdb.ACLActionDrop
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})

		ginkgo.It("correctly updates an egressfirewall's ACL logging", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
				)

				InitialNodeSwitch := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						InitialNodeSwitch,
						clusterRouter,
					},
				}

				namespace1 := *newNamespace("namespace1")
				egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Allow",
						To: egressfirewallapi.EgressFirewallDestination{
							CIDRSelector: "1.2.3.4/23",
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
								Status: v1.NodeStatus{
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})

				fakeOVN.controller.WatchNamespaces()
				fakeOVN.controller.WatchEgressFirewall()

				ipv4ACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ip4.dst != 10.128.0.0/14",
					nbdb.ACLActionAllow,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				ipv4ACL.UUID = "ipv4ACL-UUID"

				// new ACL will be added to the switch
				finalNodeSwitch := &nbdb.LogicalSwitch{
					UUID: InitialNodeSwitch.UUID,
					Name: InitialNodeSwitch.Name,
					ACLs: []string{ipv4ACL.UUID},
				}

				// new ACL will be added to the switch
				expectedDatabaseState := []libovsdb.TestData{
					ipv4ACL,
					finalNodeSwitch,
					clusterRouter,
				}

				gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

				// get the current namespace
				namespace, err := fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// enable ACL logging with severity alert, alert
				logSeverity := "alert"
				updatedLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, logSeverity, logSeverity)
				namespace.Annotations[aclLoggingAnnotation] = updatedLogSeverity
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
	})
})

var _ = ginkgo.Describe("OVN EgressFirewall Operations for shared gateway mode", func() {
	var (
		app     *cli.App
		fakeOVN *FakeOVN
	)
	const (
		node1Name string = "node1"
		node2Name string = "node2"
	)

	clusterRouter := &nbdb.LogicalRouter{
		UUID: t.OVNClusterRouter + "-UUID",
		Name: t.OVNClusterRouter,
	}

	ginkgo.BeforeEach(func() {
		// Restore global default values before each test
		config.PrepareTestConfig()
		config.Gateway.Mode = config.GatewayModeShared
		config.OVNKubernetesFeature.EnableEgressFirewall = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOVN = NewFakeOVN()
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
	})

	ginkgo.Context("on startup", func() {
		ginkgo.It("reconciles existing and non-existing egressfirewalls", func() {
			app.Action = func(ctx *cli.Context) error {
				purgeACL := libovsdbops.BuildACL(
					"purgeACL",
					t.DirectionFromLPort,
					t.EgressFirewallStartPriority,
					"",
					nbdb.ACLActionDrop,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "none"},
					nil,
				)
				purgeACL.UUID = "purgeACL-UUID"

				keepACL := libovsdbops.BuildACL(
					"",
					t.DirectionFromLPort,
					t.EgressFirewallStartPriority-1,
					"",
					nbdb.ACLActionDrop,
					"",
					"",
					false,
					map[string]string{"egressFirewall": "default"},
					nil,
				)
				keepACL.UUID = "keepACL-UUID"

				// this ACL is not in the egress firewall priority range and should be untouched
				otherACL := libovsdbops.BuildACL(
					"otherACL",
					t.DirectionFromLPort,
					t.MinimumReservedEgressFirewallPriority-1,
					"",
					nbdb.ACLActionDrop,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "default"},
					nil,
				)
				otherACL.UUID = "otherACL-UUID"

				InitialNodeSwitch := &nbdb.LogicalSwitch{
					UUID: node1Name + "-UUID",
					Name: node1Name,
					ACLs: []string{purgeACL.UUID, keepACL.UUID},
				}

				InitialJoinSwitch := &nbdb.LogicalSwitch{
					UUID: "join-UUID",
					Name: "join",
					ACLs: []string{purgeACL.UUID, keepACL.UUID},
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						purgeACL,
						keepACL,
						otherACL,
						InitialNodeSwitch,
						InitialJoinSwitch,
						clusterRouter,
					},
				}
				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NodeList{
						Items: []v1.Node{
							{
								Status: v1.NodeStatus{
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})

				// only create one egressFirewall
				_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls("default").Create(context.TODO(), &egressfirewallapi.EgressFirewall{}, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.controller.WatchEgressFirewall()

				// Both ACLS will be removed from the node switch
				finalNodeSwitch := &nbdb.LogicalSwitch{
					UUID: InitialNodeSwitch.UUID,
					Name: node1Name,
				}

				// purgeACL will be removed form the join switch
				finalJoinSwitch := &nbdb.LogicalSwitch{
					UUID: InitialJoinSwitch.UUID,
					Name: InitialJoinSwitch.Name,
					ACLs: []string{keepACL.UUID},
				}

				// Direction of both ACLs will be converted to
				keepACL.Direction = t.DirectionToLPort
				newName := buildEgressFwAclName("default", t.EgressFirewallStartPriority-1)
				meter := t.OvnACLLoggingMeter
				severity := defaultACLLoggingSeverity
				keepACL.Name = &newName
				keepACL.Direction = t.DirectionToLPort
				keepACL.Meter = &meter
				keepACL.Severity = &severity
				keepACL.Log = false

				expectedDatabaseState := []libovsdb.TestData{
					otherACL,
					keepACL,
					finalNodeSwitch,
					finalJoinSwitch,
					clusterRouter,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})
		ginkgo.It("reconciles an existing egressFirewall with IPv4 CIDR", func() {
			app.Action = func(ctx *cli.Context) error {
				InitialJoinSwitch := &nbdb.LogicalSwitch{
					UUID: "join-UUID",
					Name: "join",
				}

				namespace1 := *newNamespace("namespace1")
				egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Allow",
						To: egressfirewallapi.EgressFirewallDestination{
							CIDRSelector: "1.2.3.4/23",
						},
					},
				})

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						InitialJoinSwitch,
						clusterRouter,
					},
				}
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
					}, &v1.NodeList{
						Items: []v1.Node{
							{
								Status: v1.NodeStatus{
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})

				fakeOVN.controller.WatchNamespaces()
				fakeOVN.controller.WatchEgressFirewall()

				_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ipv4ACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && inport == \""+t.JoinSwitchToGWRouterPrefix+t.OVNClusterRouter+"\"",
					nbdb.ACLActionAllow,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				ipv4ACL.UUID = "ipv4ACL-UUID"

				// new ACL will be added to the switch
				finalJoinSwitch := &nbdb.LogicalSwitch{
					UUID: InitialJoinSwitch.UUID,
					Name: InitialJoinSwitch.Name,
					ACLs: []string{ipv4ACL.UUID},
				}

				expectedDatabaseState := []libovsdb.TestData{
					ipv4ACL,
					finalJoinSwitch,
					clusterRouter,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})
		ginkgo.It("reconciles an existing egressFirewall with IPv6 CIDR", func() {
			app.Action = func(ctx *cli.Context) error {
				InitialJoinSwitch := &nbdb.LogicalSwitch{
					UUID: "join-UUID",
					Name: "join",
				}

				namespace1 := *newNamespace("namespace1")
				egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Allow",
						To: egressfirewallapi.EgressFirewallDestination{
							CIDRSelector: "2002::1234:abcd:ffff:c0a8:101/64",
						},
					},
				})

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						InitialJoinSwitch,
						clusterRouter,
					},
				}
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
					}, &v1.NodeList{
						Items: []v1.Node{
							{
								Status: v1.NodeStatus{
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})
				config.IPv6Mode = true
				err := fakeOVN.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOVN.controller.WatchEgressFirewall()

				_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ipv6ACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip6.dst == 2002::1234:abcd:ffff:c0a8:101/64) && (ip4.src == $a10481622940199974102 || ip6.src == $a10481620741176717680) && inport == \""+t.JoinSwitchToGWRouterPrefix+t.OVNClusterRouter+"\"",
					nbdb.ACLActionAllow,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				ipv6ACL.UUID = "ipv6ACL-UUID"

				// new ACL will be added to the switch
				finalJoinSwitch := &nbdb.LogicalSwitch{
					UUID: InitialJoinSwitch.UUID,
					Name: InitialJoinSwitch.Name,
					ACLs: []string{ipv6ACL.UUID},
				}

				expectedDatabaseState := []libovsdb.TestData{
					ipv6ACL,
					finalJoinSwitch,
					clusterRouter,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})
	})
	ginkgo.Context("during execution", func() {
		ginkgo.It("correctly creates an egressfirewall denying traffic udp traffic on port 100", func() {
			app.Action = func(ctx *cli.Context) error {
				initialJoinSwitch := &nbdb.LogicalSwitch{
					UUID: "join-UUID",
					Name: "join",
				}

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

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						initialJoinSwitch,
						clusterRouter,
					},
				}
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
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})

				err := fakeOVN.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.controller.WatchEgressFirewall()

				udpACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ((udp && ( udp.dst == 100 ))) && inport == \""+
						t.JoinSwitchToGWRouterPrefix+t.OVNClusterRouter+"\"",
					nbdb.ACLActionDrop,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)

				udpACL.UUID = "udpACL-UUID"

				// new ACL will be added to the switch
				finalJoinSwitch := &nbdb.LogicalSwitch{
					UUID: initialJoinSwitch.UUID,
					Name: initialJoinSwitch.Name,
					ACLs: []string{udpACL.UUID},
				}

				expectedDatabaseState := []libovsdb.TestData{
					udpACL,
					finalJoinSwitch,
					clusterRouter,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("correctly deletes an egressfirewall", func() {
			app.Action = func(ctx *cli.Context) error {
				initialJoinSwitch := &nbdb.LogicalSwitch{
					UUID: "join-UUID",
					Name: "join",
				}

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

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						initialJoinSwitch,
						clusterRouter,
					},
				}
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
					}, &v1.NodeList{
						Items: []v1.Node{
							{
								Status: v1.NodeStatus{
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})

				fakeOVN.controller.WatchNamespaces()
				fakeOVN.controller.WatchEgressFirewall()

				ipv4ACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip4.dst == 1.2.3.5/23) && "+
						"ip4.src == $a10481622940199974102 && ((tcp && ( tcp.dst == 100 ))) && inport == \""+t.JoinSwitchToGWRouterPrefix+t.OVNClusterRouter+"\"",
					nbdb.ACLActionAllow,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				ipv4ACL.UUID = "ipv4ACL-UUID"

				// new ACL will be added to the switch
				finalJoinSwitch := &nbdb.LogicalSwitch{
					UUID: initialJoinSwitch.UUID,
					Name: initialJoinSwitch.Name,
					ACLs: []string{ipv4ACL.UUID},
				}

				expectedDatabaseState := []libovsdb.TestData{
					ipv4ACL,
					finalJoinSwitch,
					clusterRouter,
				}

				gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

				err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// join switch should return to orignal state, egfw was deleted
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(fakeOVN.dbSetup.NBData))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("correctly updates an egressfirewall", func() {
			app.Action = func(ctx *cli.Context) error {
				initialJoinSwitch := &nbdb.LogicalSwitch{
					UUID: "join-UUID",
					Name: "join",
				}

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

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						initialJoinSwitch,
						clusterRouter,
					},
				}
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
					}, &v1.NodeList{
						Items: []v1.Node{
							{
								Status: v1.NodeStatus{
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})

				fakeOVN.controller.WatchNamespaces()
				fakeOVN.controller.WatchEgressFirewall()

				ipv4ACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && inport == \""+t.JoinSwitchToGWRouterPrefix+t.OVNClusterRouter+"\"",
					nbdb.ACLActionAllow,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				ipv4ACL.UUID = "ipv4ACL-UUID"

				// new ACL will be added to the switch
				finalJoinSwitch := &nbdb.LogicalSwitch{
					UUID: initialJoinSwitch.UUID,
					Name: initialJoinSwitch.Name,
					ACLs: []string{ipv4ACL.UUID},
				}

				expectedDatabaseState := []libovsdb.TestData{
					ipv4ACL,
					finalJoinSwitch,
					clusterRouter,
				}

				gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

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

		ginkgo.It("correctly updates an egressfirewall's ACL logging", func() {
			app.Action = func(ctx *cli.Context) error {
				initialJoinSwitch := &nbdb.LogicalSwitch{
					UUID: "join-UUID",
					Name: "join",
				}

				namespace1 := *newNamespace("namespace1")
				egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Allow",
						To: egressfirewallapi.EgressFirewallDestination{
							CIDRSelector: "1.2.3.4/23",
						},
					},
				})

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						initialJoinSwitch,
						clusterRouter,
					},
				}
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
					}, &v1.NodeList{
						Items: []v1.Node{
							{
								Status: v1.NodeStatus{
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
						},
					})

				fakeOVN.controller.WatchNamespaces()
				fakeOVN.controller.WatchEgressFirewall()

				ipv4ACL := libovsdbops.BuildACL(
					buildEgressFwAclName("namespace1", t.EgressFirewallStartPriority),
					t.DirectionToLPort,
					t.EgressFirewallStartPriority,
					"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && inport == \""+t.JoinSwitchToGWRouterPrefix+t.OVNClusterRouter+"\"",
					nbdb.ACLActionAllow,
					t.OvnACLLoggingMeter,
					defaultACLLoggingSeverity,
					false,
					map[string]string{"egressFirewall": "namespace1"},
					nil,
				)
				ipv4ACL.UUID = "ipv4ACL-UUID"

				// new ACL will be added to the switch
				finalJoinSwitch := &nbdb.LogicalSwitch{
					UUID: initialJoinSwitch.UUID,
					Name: initialJoinSwitch.Name,
					ACLs: []string{ipv4ACL.UUID},
				}

				expectedDatabaseState := []libovsdb.TestData{
					ipv4ACL,
					finalJoinSwitch,
					clusterRouter,
				}

				gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

				// get the current namespace
				namespace, err := fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// enable ACL logging with severity alert, alert
				logSeverity := "alert"
				updatedLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, logSeverity, logSeverity)
				namespace.Annotations[aclLoggingAnnotation] = updatedLogSeverity
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
	})

})

var _ = ginkgo.Describe("OVN test basic functions", func() {

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
			internalCIDR string
			ipv4source   string
			ipv6source   string
			ipv4Mode     bool
			ipv6Mode     bool
			destinations []matchTarget
			ports        []egressfirewallapi.EgressFirewallPort
			output       string
		}
		testcases := []testcase{
			{
				internalCIDR: "10.128.0.0/14",
				ipv4source:   "testv4",
				ipv6source:   "",
				ipv4Mode:     true,
				ipv6Mode:     false,
				destinations: []matchTarget{{matchKindV4CIDR, "1.2.3.4/32"}},
				ports:        nil,
				output:       "(ip4.dst == 1.2.3.4/32) && ip4.src == $testv4 && inport == \"" + t.JoinSwitchToGWRouterPrefix + t.OVNClusterRouter + "\"",
			},
			{
				internalCIDR: "10.128.0.0/14",
				ipv4source:   "testv4",
				ipv6source:   "testv6",
				ipv4Mode:     true,
				ipv6Mode:     true,
				destinations: []matchTarget{{matchKindV4CIDR, "1.2.3.4/32"}},
				ports:        nil,
				output:       "(ip4.dst == 1.2.3.4/32) && (ip4.src == $testv4 || ip6.src == $testv6) && inport == \"" + t.JoinSwitchToGWRouterPrefix + t.OVNClusterRouter + "\"",
			},
			{
				internalCIDR: "10.128.0.0/14",
				ipv4source:   "testv4",
				ipv6source:   "testv6",
				ipv4Mode:     true,
				ipv6Mode:     true,
				destinations: []matchTarget{{matchKindV4AddressSet, "destv4"}, {matchKindV6AddressSet, "destv6"}},
				ports:        nil,
				output:       "(ip4.dst == $destv4 || ip6.dst == $destv6) && (ip4.src == $testv4 || ip6.src == $testv6) && inport == \"" + t.JoinSwitchToGWRouterPrefix + t.OVNClusterRouter + "\"",
			},
			{
				internalCIDR: "10.128.0.0/14",
				ipv4source:   "testv4",
				ipv6source:   "",
				ipv4Mode:     true,
				ipv6Mode:     false,
				destinations: []matchTarget{{matchKindV4AddressSet, "destv4"}, {matchKindV6AddressSet, ""}},
				ports:        nil,
				output:       "(ip4.dst == $destv4) && ip4.src == $testv4 && inport == \"" + t.JoinSwitchToGWRouterPrefix + t.OVNClusterRouter + "\"",
			},
			{
				internalCIDR: "10.128.0.0/14",
				ipv4source:   "testv4",
				ipv6source:   "testv6",
				ipv4Mode:     true,
				ipv6Mode:     true,
				destinations: []matchTarget{{matchKindV6CIDR, "2001::/64"}},
				ports:        nil,
				output:       "(ip6.dst == 2001::/64) && (ip4.src == $testv4 || ip6.src == $testv6) && inport == \"" + t.JoinSwitchToGWRouterPrefix + t.OVNClusterRouter + "\"",
			},
			{
				internalCIDR: "2002:0:0:1234::/64",
				ipv4source:   "",
				ipv6source:   "testv6",
				ipv4Mode:     false,
				ipv6Mode:     true,
				destinations: []matchTarget{{matchKindV6AddressSet, "destv6"}},
				ports:        nil,
				output:       "(ip6.dst == $destv6) && ip6.src == $testv6 && inport == \"" + t.JoinSwitchToGWRouterPrefix + t.OVNClusterRouter + "\"",
			},
		}

		for _, tc := range testcases {
			config.IPv4Mode = tc.ipv4Mode
			config.IPv6Mode = tc.ipv6Mode
			_, cidr, _ := net.ParseCIDR(tc.internalCIDR)
			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: cidr}}
			config.Gateway.Mode = config.GatewayModeShared
			matchExpression := generateMatch(tc.ipv4source, tc.ipv6source, tc.destinations, tc.ports)
			gomega.Expect(tc.output).To(gomega.Equal(matchExpression))
		}
	})
	ginkgo.It("correctly parses egressFirewallRules", func() {
		type testcase struct {
			egressFirewallRule egressfirewallapi.EgressFirewallRule
			id                 int
			err                bool
			errOutput          string
			output             egressFirewallRule
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
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002::1234:abcd:ffff:c0a8:101/64"},
				},
				id:  2,
				err: false,
				output: egressFirewallRule{
					id:     2,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002::1234:abcd:ffff:c0a8:101/64"},
				},
			},
		}
		for _, tc := range testcases {
			output, err := newEgressFirewallRule(tc.egressFirewallRule, tc.id)
			if tc.err == true {
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(tc.errOutput).To(gomega.Equal(err.Error()))
			} else {
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(tc.output).To(gomega.Equal(*output))
			}
		}
	})
})

//helper functions to help test egressfirewallDNS

// Create an EgressDNS object without the Sync function
// To make it easier to mock EgressFirewall functionality create an egressFirewall
// without the go routine of the sync function

//GetDNSEntryForTest Gets a dnsEntry from a EgressDNS object for testing
func (e *EgressDNS) GetDNSEntryForTest(dnsName string) (map[string]struct{}, []net.IP, addressset.AddressSet, error) {
	if e.dnsEntries[dnsName] == nil {
		return nil, nil, nil, fmt.Errorf("there is no dnsEntry for dnsName: %s", dnsName)
	}
	return e.dnsEntries[dnsName].namespaces, e.dnsEntries[dnsName].dnsResolves, e.dnsEntries[dnsName].dnsAddressSet, nil
}

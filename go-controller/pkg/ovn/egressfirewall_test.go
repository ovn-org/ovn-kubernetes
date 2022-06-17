package ovn

import (
	"context"
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"net"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

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

func getDefaultACL(ns string) (*nbdb.ACL, *nbdb.ACL) {
	name := buildEgressFwAclName(ns, t.EgressFirewallDenyMgmtPortsPriority)
	denyMgmtPortAcl := libovsdbops.BuildACL(
		name,
		nbdb.ACLDirectionFromLport,
		t.EgressFirewallDenyMgmtPortsPriority,
		fmt.Sprintf("outport == @%s && inport == @%s", t.ClusterPortGroupName, getEgressFirewallPortGroupName(ns)),
		nbdb.ACLActionDrop,
		t.OvnACLLoggingMeter,
		"",
		false,
		map[string]string{egressFirewallACLExtIdKey: ns},
		map[string]string{"apply-after-lb": "true"},
	)
	denyMgmtPortAcl.UUID = fmt.Sprintf("%s-UUID", name)
	name = buildEgressFwAclName(ns, t.EgressFirewallAllowClusterSubnetsPriority)
	portGroup := getEgressFirewallPortGroupName(ns)
	allowClusterSubnetACL := libovsdbops.BuildACL(
		name,
		nbdb.ACLDirectionFromLport,
		t.EgressFirewallAllowClusterSubnetsPriority,
		fmt.Sprintf("%s && inport == @%s", getDstClusterSubnetsMatch(), portGroup),
		nbdb.ACLActionAllowRelated,
		t.OvnACLLoggingMeter,
		"",
		false,
		map[string]string{egressFirewallACLExtIdKey: ns},
		map[string]string{"apply-after-lb": "true"},
	)
	allowClusterSubnetACL.UUID = fmt.Sprintf("%s-UUID", name)

	return denyMgmtPortAcl, allowClusterSubnetACL
}

var _ = ginkgo.Describe("OVN EgressFirewall Operations", func() {
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
	initialNodeSwitch := &nbdb.LogicalSwitch{
		UUID: node1Name + "-UUID",
		Name: node1Name,
	}
	initialJoinSwitch := &nbdb.LogicalSwitch{
		UUID: "join-UUID",
		Name: "join",
	}
	initialClusterPG := &nbdb.PortGroup{
		UUID: t.ClusterPortGroupName + "-UUID",
		Name: t.ClusterPortGroupName,
		ExternalIDs: map[string]string{
			"name": t.ClusterPortGroupName,
		},
	}
	// node and join switch are here to make sure no ACLs are added for them
	initialDb := libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			initialNodeSwitch,
			initialJoinSwitch,
			clusterRouter,
			initialClusterPG,
		},
	}
	fmt.Printf("==================== %+v", config.Default.ClusterSubnets)
	//clusterSubnet := "10.128.0.0/14"

	testNamespace := "test-namespace"
	dnsAS := &nbdb.AddressSet{
		UUID:      "as-UUID",
		Addresses: []string{"10.244.0.2", "10.244.1.2", "10.244.2.2", "100.64.0.2", "100.64.0.3", "100.64.0.4"},
		ExternalIDs: map[string]string{
			"name": "www.openvswitch.org_v4",
		},
		Name: util.HashForOVN("www.openvswitch.org_v4"),
	}
	dnsDefaultACL1, dnsDefaultACL2 := getDefaultACL(testNamespace)
	dnsACL := libovsdbops.BuildACL(
		buildEgressFwAclName(testNamespace, t.EgressFirewallStartPriority),
		nbdb.ACLDirectionFromLport,
		t.EgressFirewallStartPriority,
		fmt.Sprintf("(ip4.dst == $%s) && inport == @%s", dnsAS.Name, getEgressFirewallPortGroupName(testNamespace)),
		nbdb.ACLActionAllowRelated,
		t.OvnACLLoggingMeter,
		"",
		false,
		map[string]string{egressFirewallACLExtIdKey: testNamespace},
		map[string]string{"apply-after-lb": "true"},
	)
	dnsACL.UUID = "dnsACL-UUID"
	dnsPG := &nbdb.PortGroup{
		UUID: "PortGroup-UUID",
		Name: getEgressFirewallPortGroupName(testNamespace),
		ACLs: []string{dnsACL.UUID, dnsDefaultACL1.UUID, dnsDefaultACL2.UUID},
		ExternalIDs: map[string]string{
			"name": getEgressFirewallPortGroupExternalIdName(testNamespace),
		},
	}

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		//config.Gateway.Mode = config.GatewayModeLocal
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
		for _, gwMode := range []config.GatewayMode{config.GatewayModeLocal, config.GatewayModeShared} {
			config.Gateway.Mode = gwMode
			ginkgo.It(fmt.Sprintf("reconciles existing and non-existing egressfirewalls, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					// ns1 and ns2 exist
					ns1 := "namespace1"
					ns2 := "namespace2"
					egressFirewall1 := newEgressFirewallObject("default", ns1, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					egressFirewall2 := newEgressFirewallObject("default", ns2, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					// stale ACL (legacy priority) for existing egress firewall, should be replaced with the new ACL
					staleACL := libovsdbops.BuildACL(
						buildEgressFwAclName(ns1, t.LegacyEgressFirewallStartPriority),
						nbdb.ACLDirectionToLport,
						t.LegacyEgressFirewallStartPriority,
						"match str",
						nbdb.ACLActionDrop,
						"",
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: ns1},
						nil,
					)
					staleACL.UUID = "staleACL-UUID"

					// updated ACL and Port group for existing ef, should be untouched
					updatedDefaultAcl1, updatedDefaultAcl2 := getDefaultACL(ns2)
					updatedACL := libovsdbops.BuildACL(
						buildEgressFwAclName(ns2, t.EgressFirewallStartPriority),
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						fmt.Sprintf("(ip4.dst == 1.2.3.4/23) && inport == @%s", getEgressFirewallPortGroupName(ns2)),
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: ns2},
						map[string]string{"apply-after-lb": "true"},
					)
					updatedACL.UUID = "updatedACL-UUID"
					updatedPG := &nbdb.PortGroup{
						UUID: "UpdatedPortGroup-UUID",
						Name: getEgressFirewallPortGroupName(ns2),
						ACLs: []string{updatedACL.UUID, updatedDefaultAcl1.UUID, updatedDefaultAcl2.UUID},
						ExternalIDs: map[string]string{
							"name": getEgressFirewallPortGroupExternalIdName(ns2),
						},
					}
					// legacy non-existing acl, should be deleted
					legacyPurgeACL := libovsdbops.BuildACL(
						"legacy_ACL",
						nbdb.ACLDirectionToLport,
						t.LegacyEgressFirewallStartPriority,
						"",
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: "legacyPurgeNS"},
						nil,
					)
					legacyPurgeACL.UUID = "legacyPurgeACL-UUID"

					// non-existing egress firewall ACL, should be deleted
					purgeNS := "doesnt_exist"
					purgeACL := libovsdbops.BuildACL(
						buildEgressFwAclName(purgeNS, t.EgressFirewallStartPriority),
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						"",
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: purgeNS},
						nil,
					)
					purgeACL.UUID = "purgeACL-UUID"
					purgePG := &nbdb.PortGroup{
						UUID: "PurgePortGroup-UUID",
						Name: getEgressFirewallPortGroupName(purgeNS),
						ACLs: []string{purgeACL.UUID},
						ExternalIDs: map[string]string{
							"name": getEgressFirewallPortGroupExternalIdName(purgeNS),
						},
					}

					// non-egress firewall ACL (doesn't have ef externalID), should be untouched
					otherACL1 := libovsdbops.BuildACL(
						"otherACL1",
						nbdb.ACLDirectionToLport,
						t.LegacyEgressFirewallStartPriority,
						"otherACL1-match",
						nbdb.ACLActionDrop,
						"",
						"",
						false,
						nil,
						nil,
					)
					otherACL1.UUID = "otherACL1-UUID"

					// non-egress firewall ACL (not in the egress firewall priority range), should be untouched
					otherACL2 := libovsdbops.BuildACL(
						"otherACL2",
						nbdb.ACLDirectionToLport,
						t.LegacyMinimumReservedEgressFirewallPriority-1,
						"otherACL2-match",
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						nil,
						nil,
					)
					otherACL2.UUID = "otherACL2-UUID"

					InitialNodeSwitch := &nbdb.LogicalSwitch{
						UUID: node1Name + "-UUID",
						Name: node1Name,
						ACLs: []string{staleACL.UUID, legacyPurgeACL.UUID, otherACL1.UUID, otherACL2.UUID},
					}
					InitialJoinSwitch := &nbdb.LogicalSwitch{
						UUID: "join-UUID",
						Name: "join",
						ACLs: []string{staleACL.UUID, legacyPurgeACL.UUID, otherACL1.UUID, otherACL2.UUID},
					}

					dbSetup := libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							staleACL,
							updatedACL,
							updatedDefaultAcl1,
							updatedDefaultAcl2,
							updatedPG,
							legacyPurgeACL,
							purgeACL,
							purgePG,
							otherACL1,
							otherACL2,
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
						},
						&egressfirewallapi.EgressFirewallList{
							Items: []egressfirewallapi.EgressFirewall{
								*egressFirewall1,
								*egressFirewall2,
							},
						})

					err := fakeOVN.controller.WatchEgressFirewall()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// other ACLs will not be changed, everything else will be removed from switches
					finalJoinSwitch := &nbdb.LogicalSwitch{
						UUID: InitialJoinSwitch.UUID,
						Name: InitialJoinSwitch.Name,
						ACLs: []string{otherACL1.UUID, otherACL2.UUID},
					}
					finalNodeSwitch := &nbdb.LogicalSwitch{
						UUID: InitialNodeSwitch.UUID,
						Name: InitialNodeSwitch.Name,
						ACLs: []string{otherACL1.UUID, otherACL2.UUID},
					}
					// New ACLs and port group will be created instead of staleACL for existing ef
					finalDefaultAcl1, finalDefaultAcl2 := getDefaultACL(ns1)
					finalACLStale := libovsdbops.BuildACL(
						buildEgressFwAclName(ns1, t.EgressFirewallStartPriority),
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						fmt.Sprintf("(ip4.dst == 1.2.3.4/23) && inport == @%s", getEgressFirewallPortGroupName(ns1)),
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: ns1},
						map[string]string{"apply-after-lb": "true"},
					)
					finalACLStale.UUID = "finalACLStale-UUID"
					finalEFPortGroup := &nbdb.PortGroup{
						UUID: "EFPortGroup-UUID",
						Name: getEgressFirewallPortGroupName(ns1),
						ACLs: []string{finalACLStale.UUID, finalDefaultAcl1.UUID, finalDefaultAcl2.UUID},
						ExternalIDs: map[string]string{
							"name": getEgressFirewallPortGroupExternalIdName(ns1),
						},
					}

					expectedDatabaseState := []libovsdb.TestData{
						finalACLStale,
						finalDefaultAcl1,
						finalDefaultAcl2,
						finalEFPortGroup,
						updatedACL,
						updatedDefaultAcl1,
						updatedDefaultAcl2,
						updatedPG,
						otherACL1,
						otherACL2,
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
			ginkgo.It(fmt.Sprintf("reconciles an existing egressFirewall with IPv4 CIDR, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {

					ns := "namespace1"
					namespace1 := *newNamespace(ns)
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					fakeOVN.startWithDBSetup(initialDb,
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

					err := fakeOVN.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOVN.controller.WatchEgressFirewall()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// new port group and ACl will be added
					ipv4DefaultAcl1, ipv4DefaultAcl2 := getDefaultACL(ns)
					ipv4ACL := libovsdbops.BuildACL(
						buildEgressFwAclName(ns, t.EgressFirewallStartPriority),
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						fmt.Sprintf("(ip4.dst == 1.2.3.4/23) && inport == @%s", getEgressFirewallPortGroupName(ns)),
						nbdb.ACLActionAllowRelated,
						t.OvnACLLoggingMeter,
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: ns},
						map[string]string{"apply-after-lb": "true"},
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"
					pg := &nbdb.PortGroup{
						UUID: "PortGroup-UUID",
						Name: getEgressFirewallPortGroupName(ns),
						ACLs: []string{ipv4ACL.UUID, ipv4DefaultAcl1.UUID, ipv4DefaultAcl2.UUID},
						ExternalIDs: map[string]string{
							"name": getEgressFirewallPortGroupExternalIdName(ns),
						},
					}

					expectedDatabaseState := []libovsdb.TestData{
						ipv4ACL,
						ipv4DefaultAcl1,
						ipv4DefaultAcl2,
						pg,
						initialNodeSwitch,
						initialJoinSwitch,
						clusterRouter,
						initialClusterPG,
					}

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("reconciles an existing egressFirewall with IPv6 CIDR, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					ns := "namespace1"
					namespace1 := *newNamespace(ns)
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "2002::1234:abcd:ffff:c0a8:101/64",
							},
						},
					})

					fakeOVN.startWithDBSetup(initialDb,
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
					err := fakeOVN.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOVN.controller.WatchEgressFirewall()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ipv6DefaultAcl1, ipv6DefaultAcl2 := getDefaultACL(ns)
					ipv6ACL := libovsdbops.BuildACL(
						buildEgressFwAclName(ns, t.EgressFirewallStartPriority),
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						fmt.Sprintf("(ip6.dst == 2002::1234:abcd:ffff:c0a8:101/64) && inport == @%s", getEgressFirewallPortGroupName(ns)),
						nbdb.ACLActionAllowRelated,
						t.OvnACLLoggingMeter,
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: ns},
						map[string]string{"apply-after-lb": "true"},
					)
					ipv6ACL.UUID = "ipv6ACL-UUID"

					pg := &nbdb.PortGroup{
						UUID: "PortGroup-UUID",
						Name: getEgressFirewallPortGroupName(ns),
						ACLs: []string{ipv6ACL.UUID, ipv6DefaultAcl1.UUID, ipv6DefaultAcl2.UUID},
						ExternalIDs: map[string]string{
							"name": getEgressFirewallPortGroupExternalIdName(ns),
						},
					}

					expectedDatabaseState := []libovsdb.TestData{
						ipv6ACL,
						ipv6DefaultAcl1,
						ipv6DefaultAcl2,
						pg,
						initialClusterPG,
						initialNodeSwitch,
						initialJoinSwitch,
						clusterRouter,
					}

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
		}
		ginkgo.It("cleans up db for deleted egress firewall", func() {
			app.Action = func(ctx *cli.Context) error {
				// create 2 egress firewalls referencing the same dns name,
				// one is stale, and one still exists
				// make sure stale EF gets deleted, but common address set and existing EF are not
				commonAS := &nbdb.AddressSet{
					UUID:      "commonAS-UUID",
					Addresses: []string{"1.1.1.1"},
					ExternalIDs: map[string]string{
						"name": "dnsName_v4",
					},
					Name: util.HashForOVN("dnsName_v4"),
				}
				ns1 := "ns1"
				staleDefaultAcl1, staleDefaultAcl2 := getDefaultACL(ns1)
				staleACL := libovsdbops.BuildACL(
					buildEgressFwAclName(ns1, t.EgressFirewallStartPriority),
					nbdb.ACLDirectionFromLport,
					t.EgressFirewallStartPriority,
					fmt.Sprintf("(ip4.dst == $%s) && inport == @%s", commonAS.Name, getEgressFirewallPortGroupName(ns1)),
					nbdb.ACLActionAllowRelated,
					t.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{egressFirewallACLExtIdKey: ns1},
					map[string]string{"apply-after-lb": "true"},
				)
				staleACL.UUID = "staleACL-UUID"
				stalePG := &nbdb.PortGroup{
					UUID: "stalePG-UUID",
					Name: getEgressFirewallPortGroupName(ns1),
					ACLs: []string{staleACL.UUID, staleDefaultAcl1.UUID, staleDefaultAcl2.UUID},
					ExternalIDs: map[string]string{
						"name": getEgressFirewallPortGroupExternalIdName(ns1),
					},
				}

				ns2 := "ns2"
				existingDefaultAcl1, existingDefaultAcl2 := getDefaultACL(ns2)
				existingACL := libovsdbops.BuildACL(
					buildEgressFwAclName(ns2, t.EgressFirewallStartPriority),
					nbdb.ACLDirectionFromLport,
					t.EgressFirewallStartPriority,
					fmt.Sprintf("(ip4.dst == $%s) && inport == @%s", commonAS.Name, getEgressFirewallPortGroupName(ns2)),
					nbdb.ACLActionAllowRelated,
					t.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{egressFirewallACLExtIdKey: ns2},
					map[string]string{"apply-after-lb": "true"},
				)
				existingACL.UUID = "existingACL-UUID"
				existingPG := &nbdb.PortGroup{
					UUID: "existingPG-UUID",
					Name: getEgressFirewallPortGroupName(ns2),
					ACLs: []string{existingACL.UUID, existingDefaultAcl1.UUID, existingDefaultAcl2.UUID},
					ExternalIDs: map[string]string{
						"name": getEgressFirewallPortGroupExternalIdName(ns2),
					},
				}
				existingEF := newEgressFirewallObject("default", ns2, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Allow",
						To: egressfirewallapi.EgressFirewallDestination{
							DNSName: "dnsName",
						},
					},
				})

				// use dns EF db entities for egress firewall that exists in db, but no longer in k8s
				// should be fully deleted
				initialDB := []libovsdb.TestData{
					dnsAS,
					dnsDefaultACL1,
					dnsDefaultACL2,
					dnsACL,
					dnsPG,
					commonAS,
					staleDefaultAcl1,
					staleDefaultAcl2,
					staleACL,
					stalePG,
					existingDefaultAcl1,
					existingDefaultAcl2,
					existingACL,
					existingPG,
					initialClusterPG,
					initialNodeSwitch,
					initialJoinSwitch,
					clusterRouter,
				}

				fakeOVN.startWithDBSetup(libovsdbtest.TestSetup{NBData: initialDB})
				efs := make([]interface{}, 1)
				efs[0] = existingEF
				err := fakeOVN.controller.syncEgressFirewall(efs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// make sure cleanup is successful
				expectedDatabaseState := []libovsdb.TestData{
					commonAS,
					existingDefaultAcl1,
					existingDefaultAcl2,
					existingACL,
					existingPG,
					initialClusterPG,
					initialNodeSwitch,
					initialJoinSwitch,
					clusterRouter,
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("cleans up db for disabled egress firewall feature", func() {
			app.Action = func(ctx *cli.Context) error {
				// egress firewall exists in db, but egress firewall feature was disabled
				initialDB := []libovsdb.TestData{
					dnsAS,
					dnsDefaultACL1,
					dnsDefaultACL2,
					dnsACL,
					dnsPG,
					initialClusterPG,
					initialNodeSwitch,
					initialJoinSwitch,
					clusterRouter,
				}

				fakeOVN.startWithDBSetup(libovsdbtest.TestSetup{NBData: initialDB},
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

				err := fakeOVN.controller.disableEgressFirewall()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// make sure everything EF-related is deleted
				expectedDatabaseState := []libovsdb.TestData{
					initialClusterPG,
					initialNodeSwitch,
					initialJoinSwitch,
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
		for _, gwMode := range []config.GatewayMode{config.GatewayModeLocal, config.GatewayModeShared} {
			config.Gateway.Mode = gwMode
			ginkgo.It(fmt.Sprintf("correctly creates an egressfirewall denying traffic udp traffic on port 100, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					ns := "namespace1"

					namespace1 := *newNamespace(ns)
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
					fakeOVN.startWithDBSetup(initialDb,
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

					err = fakeOVN.controller.WatchEgressFirewall()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					udpDefaultAcl1, udpDefaultAcl2 := getDefaultACL(ns)
					udpACL := libovsdbops.BuildACL(
						buildEgressFwAclName(ns, t.EgressFirewallStartPriority),
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						fmt.Sprintf("(ip4.dst == 1.2.3.4/23) && inport == @%s && ((udp && ( udp.dst == 100 )))", getEgressFirewallPortGroupName(ns)),
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: ns},
						map[string]string{"apply-after-lb": "true"},
					)
					udpACL.UUID = "udpACL-UUID"
					pg := &nbdb.PortGroup{
						UUID: "PortGroup-UUID",
						Name: getEgressFirewallPortGroupName(ns),
						ACLs: []string{udpACL.UUID, udpDefaultAcl1.UUID, udpDefaultAcl2.UUID},
						ExternalIDs: map[string]string{
							"name": getEgressFirewallPortGroupExternalIdName(ns),
						},
					}

					expectedDatabaseState := []libovsdb.TestData{
						udpDefaultAcl1,
						udpDefaultAcl2,
						udpACL,
						pg,
						initialClusterPG,
						initialNodeSwitch,
						initialJoinSwitch,
						clusterRouter,
					}

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly deletes an egressfirewall, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {

					ns := "namespace1"
					namespace1 := *newNamespace(ns)
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

					fakeOVN.startWithDBSetup(initialDb,
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
					err := fakeOVN.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOVN.controller.WatchEgressFirewall()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ipv4DefaultAcl1, ipv4DefaultAcl2 := getDefaultACL(ns)
					ipv4ACL := libovsdbops.BuildACL(
						buildEgressFwAclName(ns, t.EgressFirewallStartPriority),
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						fmt.Sprintf("(ip4.dst == 1.2.3.5/23) && inport == @%s && ((tcp && ( tcp.dst == 100 )))", getEgressFirewallPortGroupName(ns)),
						nbdb.ACLActionAllowRelated,
						t.OvnACLLoggingMeter,
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: ns},
						map[string]string{"apply-after-lb": "true"},
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"
					pg := &nbdb.PortGroup{
						UUID: "PortGroup-UUID",
						Name: getEgressFirewallPortGroupName(ns),
						ACLs: []string{ipv4ACL.UUID, ipv4DefaultAcl1.UUID, ipv4DefaultAcl2.UUID},
						ExternalIDs: map[string]string{
							"name": getEgressFirewallPortGroupExternalIdName(ns),
						},
					}

					expectedDatabaseState := []libovsdb.TestData{
						ipv4ACL,
						ipv4DefaultAcl1,
						ipv4DefaultAcl2,
						pg,
						initialClusterPG,
						initialNodeSwitch,
						initialJoinSwitch,
						clusterRouter,
					}

					gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

					err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(initialDb.NBData))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly updates an egressfirewall, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					ns := "namespace1"
					namespace1 := *newNamespace(ns)
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					egressFirewallUpdate := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					fakeOVN.startWithDBSetup(initialDb,
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
					err = fakeOVN.controller.WatchEgressFirewall()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ipv4DefaultAcl1, ipv4DefaultAcl2 := getDefaultACL(ns)
					ipv4ACL := libovsdbops.BuildACL(
						buildEgressFwAclName(ns, t.EgressFirewallStartPriority),
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						fmt.Sprintf("(ip4.dst == 1.2.3.4/23) && inport == @%s", getEgressFirewallPortGroupName(ns)),
						nbdb.ACLActionAllowRelated,
						t.OvnACLLoggingMeter,
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: ns},
						map[string]string{"apply-after-lb": "true"},
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"
					pg := &nbdb.PortGroup{
						UUID: "PortGroup-UUID",
						Name: getEgressFirewallPortGroupName(ns),
						ACLs: []string{ipv4ACL.UUID, ipv4DefaultAcl1.UUID, ipv4DefaultAcl2.UUID},
						ExternalIDs: map[string]string{
							"name": getEgressFirewallPortGroupExternalIdName(ns),
						},
					}
					expectedDatabaseState := []libovsdb.TestData{
						ipv4ACL,
						ipv4DefaultAcl1,
						ipv4DefaultAcl2,
						pg,
						initialClusterPG,
						initialNodeSwitch,
						initialJoinSwitch,
						clusterRouter,
					}

					gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewallUpdate.Namespace).
						Update(context.TODO(), egressFirewallUpdate, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ipv4ACL.Action = nbdb.ACLActionDrop

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("correctly retries deleting an egressfirewall, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					ns := "namespace1"
					namespace1 := *newNamespace(ns)
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

					fakeOVN.startWithDBSetup(initialDb,
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
					err := fakeOVN.controller.WatchEgressFirewall()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ipv4DefaultAcl1, ipv4DefaultAcl2 := getDefaultACL(ns)
					ipv4ACL := libovsdbops.BuildACL(
						buildEgressFwAclName(ns, t.EgressFirewallStartPriority),
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						fmt.Sprintf("(ip4.dst == 1.2.3.5/23) && inport == @%s && ((tcp && ( tcp.dst == 100 )))", getEgressFirewallPortGroupName(ns)),
						nbdb.ACLActionAllowRelated,
						t.OvnACLLoggingMeter,
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: ns},
						map[string]string{"apply-after-lb": "true"},
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"
					pg := &nbdb.PortGroup{
						UUID: "PortGroup-UUID",
						Name: getEgressFirewallPortGroupName(ns),
						ACLs: []string{ipv4ACL.UUID, ipv4DefaultAcl1.UUID, ipv4DefaultAcl2.UUID},
						ExternalIDs: map[string]string{
							"name": getEgressFirewallPortGroupExternalIdName(ns),
						},
					}

					expectedDatabaseState := []libovsdb.TestData{
						ipv4ACL,
						ipv4DefaultAcl1,
						ipv4DefaultAcl2,
						pg,
						initialClusterPG,
						initialNodeSwitch,
						initialJoinSwitch,
						clusterRouter,
					}

					gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

					ginkgo.By("Bringing down NBDB")
					// inject transient problem, nbdb is down
					fakeOVN.controller.nbClient.Close()
					gomega.Eventually(func() bool {
						return fakeOVN.controller.nbClient.Connected()
					}).Should(gomega.BeFalse())

					err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
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
					fakeOVN.controller.retryPods.setRetryObjWithNoBackoff(key)
					fakeOVN.controller.retryEgressFirewalls.requestRetryObjs()

					// ACLs and port group should be removed
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(initialDb.NBData))
					// check the cache no longer has the entry
					gomega.Eventually(func() *retryObjEntry {
						return fakeOVN.controller.retryEgressFirewalls.getObjRetryEntry(key)
					}).Should(gomega.BeNil())
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly retries adding and updating an egressfirewall, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					ns := "namespace1"
					namespace1 := *newNamespace(ns)
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					egressFirewallUpdate := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					fakeOVN.startWithDBSetup(initialDb,
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
					err = fakeOVN.controller.WatchEgressFirewall()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ipv4DefaultAcl1, ipv4DefaultAcl2 := getDefaultACL(ns)
					ipv4ACL := libovsdbops.BuildACL(
						buildEgressFwAclName(ns, t.EgressFirewallStartPriority),
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						fmt.Sprintf("(ip4.dst == 1.2.3.4/23) && inport == @%s", getEgressFirewallPortGroupName(ns)),
						nbdb.ACLActionAllowRelated,
						t.OvnACLLoggingMeter,
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: ns},
						map[string]string{"apply-after-lb": "true"},
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"
					pg := &nbdb.PortGroup{
						UUID: "PortGroup-UUID",
						Name: getEgressFirewallPortGroupName(ns),
						ACLs: []string{ipv4ACL.UUID, ipv4DefaultAcl1.UUID, ipv4DefaultAcl2.UUID},
						ExternalIDs: map[string]string{
							"name": getEgressFirewallPortGroupExternalIdName(ns),
						},
					}
					// new ACL and port group will be added
					expectedDatabaseState := []libovsdb.TestData{
						ipv4ACL,
						ipv4DefaultAcl1,
						ipv4DefaultAcl2,
						pg,
						initialClusterPG,
						initialNodeSwitch,
						initialJoinSwitch,
						clusterRouter,
					}

					gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))
					ginkgo.By("Bringing down NBDB")
					// inject transient problem, nbdb is down
					fakeOVN.controller.nbClient.Close()
					gomega.Eventually(func() bool {
						return fakeOVN.controller.nbClient.Connected()
					}).Should(gomega.BeFalse())

					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewallUpdate.Namespace).
						Update(context.TODO(), egressFirewallUpdate, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// sleep long enough for TransactWithRetry to fail, causing egress firewall Add to fail
					time.Sleep(t.OVSDBTimeout + time.Second)
					// check to see if the retry cache has an entry for this egress firewall
					key, err := getResourceKey(factory.EgressFirewallType, egressFirewall)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
					ginkgo.By("bringing up NBDB and requesting retry of entry")
					resetNBClient(connCtx, fakeOVN.controller.nbClient)
					fakeOVN.controller.retryEgressFirewalls.setRetryObjWithNoBackoff(key)
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
			ginkgo.It(fmt.Sprintf("correctly updates an egressfirewall's ACL logging, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					ns := "namespace1"
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					fakeOVN.startWithDBSetup(initialDb,
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
					err = fakeOVN.controller.WatchEgressFirewall()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ipv4DefaultAcl1, ipv4DefaultAcl2 := getDefaultACL(ns)
					ipv4ACL := libovsdbops.BuildACL(
						buildEgressFwAclName(ns, t.EgressFirewallStartPriority),
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						fmt.Sprintf("(ip4.dst == 1.2.3.4/23) && inport == @%s", getEgressFirewallPortGroupName(ns)),
						nbdb.ACLActionAllowRelated,
						t.OvnACLLoggingMeter,
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: ns},
						map[string]string{"apply-after-lb": "true"},
					)
					ipv4ACL.UUID = "ipv4ACL-UUID"
					pg := &nbdb.PortGroup{
						UUID: "PortGroup-UUID",
						Name: getEgressFirewallPortGroupName(ns),
						ACLs: []string{ipv4ACL.UUID, ipv4DefaultAcl1.UUID, ipv4DefaultAcl2.UUID},
						ExternalIDs: map[string]string{
							"name": getEgressFirewallPortGroupExternalIdName(ns),
						},
					}
					// new ACL and port group will be added
					expectedDatabaseState := []libovsdb.TestData{
						ipv4ACL,
						ipv4DefaultAcl1,
						ipv4DefaultAcl2,
						pg,
						initialClusterPG,
						initialNodeSwitch,
						initialJoinSwitch,
						clusterRouter,
					}

					gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

					// get the current namespace
					namespace, err := fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().
						Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// enable ACL logging with severity alert, alert
					logSeverity := nbdb.ACLSeverityAlert
					updatedLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, logSeverity, logSeverity)
					namespace.Annotations[util.AclLoggingAnnotation] = updatedLogSeverity
					_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespace, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// eventually, we should see the changes in the namespace reflected in the database
					ipv4DefaultAcl1.Log = true
					ipv4DefaultAcl1.Severity = &logSeverity
					ipv4DefaultAcl2.Log = true
					ipv4DefaultAcl2.Severity = &logSeverity
					ipv4ACL.Log = true
					ipv4ACL.Severity = &logSeverity
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
		}
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
			portGroup    string
			destination  matchTarget
			ports        []egressfirewallapi.EgressFirewallPort
			output       string
		}
		testcases := []testcase{
			{
				internalCIDR: "10.128.0.0/14",
				portGroup:    "testPG",
				destination:  matchTarget{matchKindV4CIDR, "1.2.3.4/32"},
				ports:        nil,
				output:       "(ip4.dst == 1.2.3.4/32) && inport == @testPG",
			},
			{
				internalCIDR: "10.128.0.0/14",
				portGroup:    "testPG",
				destination:  matchTarget{matchKindV6CIDR, "2001::/64"},
				ports:        nil,
				output:       "(ip6.dst == 2001::/64) && inport == @testPG",
			},
			{
				internalCIDR: "2002:0:0:1234::/64",
				portGroup:    "testPG",
				destination:  matchTarget{matchKindV6AddressSet, "destv6"},
				ports:        nil,
				output:       "(ip6.dst == $destv6) && inport == @testPG",
			},
		}

		for _, tc := range testcases {
			_, cidr, _ := net.ParseCIDR(tc.internalCIDR)
			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: cidr}}
			config.Gateway.Mode = config.GatewayModeShared
			matchExpression := generateMatch(tc.portGroup, tc.destination, tc.ports)
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

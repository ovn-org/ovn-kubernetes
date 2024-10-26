package address_set

import (
	"fmt"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type asSync struct {
	before                *nbdb.AddressSet
	after                 *libovsdbops.DbObjectIDs
	afterTweak            func(*nbdb.AddressSet)
	addressSetFactoryIPID string
	remove                bool
	leave                 bool
}

// data is used to pass address set for initial and expected db state, initialDbState may be used to add objects
// of other types to the initial db state, and finalDbState may be used to set the expected state of objects
// passed in initialDbState. If finalDbState is nil, final state will be updated automatically by changing address set
// references for initial objects from initialDbState.
func testSyncerWithData(data []asSync, initialDbState, finalDbState []libovsdbtest.TestData, controllerName string) {
	// create initial db setup
	dbSetup := libovsdbtest.TestSetup{NBData: initialDbState}
	for _, asSync := range data {
		dbSetup.NBData = append(dbSetup.NBData, asSync.before)
	}
	libovsdbOvnNBClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
	defer libovsdbCleanup.Cleanup()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	// create expected data using addressSetFactory
	expectedDbState := initialDbState
	if finalDbState != nil {
		expectedDbState = finalDbState
	}
	for _, asSync := range data {
		if asSync.remove {
			continue
		}
		if asSync.leave {
			expectedDbState = append(expectedDbState, asSync.before)
		} else if asSync.after != nil {
			updatedAS := getUpdatedAS(asSync.before, asSync.after, asSync.addressSetFactoryIPID)
			if asSync.afterTweak != nil {
				asSync.afterTweak(updatedAS)
			}
			expectedDbState = append(expectedDbState, updatedAS)
			if finalDbState == nil {
				for _, dbObj := range expectedDbState {
					if lrp, ok := dbObj.(*nbdb.LogicalRouterPolicy); ok {
						lrp.Match = strings.ReplaceAll(lrp.Match, "$"+asSync.before.Name, "$"+updatedAS.Name)
					}
					if acl, ok := dbObj.(*nbdb.ACL); ok {
						acl.Match = strings.ReplaceAll(acl.Match, "$"+asSync.before.Name, "$"+updatedAS.Name)
					}
					if qos, ok := dbObj.(*nbdb.QoS); ok {
						qos.Match = strings.ReplaceAll(qos.Match, "$"+asSync.before.Name, "$"+updatedAS.Name)
					}
				}
			}
		}
	}
	// run sync
	syncer := NewAddressSetSyncer(libovsdbOvnNBClient, controllerName)
	// to make sure batching works, set it to 2 to cover number of batches = 0,1,>1
	syncer.txnBatchSize = 2
	err = syncer.SyncAddressSets()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	// check results
	gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDbState))
}

func createInitialAS(name string, ips []string) *nbdb.AddressSet {
	return &nbdb.AddressSet{
		UUID: name,
		Name: hashedAddressSet(name),
		ExternalIDs: map[string]string{
			"name": name,
		},
		Addresses: ips,
	}
}

func getUpdatedAS(as *nbdb.AddressSet, dbIDs *libovsdbops.DbObjectIDs,
	addressSetFactoryIpID string) *nbdb.AddressSet {
	var nbdbAS *nbdb.AddressSet
	if addressSetFactoryIpID == ipv4AddressSetFactoryID {
		nbdbAS = buildNewAddressSet(dbIDs, ipv4AddressSetFactoryID)
	} else {
		nbdbAS = buildNewAddressSet(dbIDs, ipv6AddressSetFactoryID)
	}
	updatedAS := as.DeepCopy()
	updatedAS.Name = nbdbAS.Name

	delete(updatedAS.ExternalIDs, "name")
	for key, value := range nbdbAS.ExternalIDs {
		updatedAS.ExternalIDs[key] = value
	}
	return updatedAS
}

func hashedAddressSet(s string) string {
	return util.HashForOVN(s)
}

var _ = ginkgo.Describe("OVN Address Set Syncer", func() {
	const (
		testIPv4              = "1.1.1.1"
		testGatewayIPv4       = "1.2.3.4"
		testIPv6              = "2001:db8::68"
		testGatewayIPv6       = "2001:db8::69"
		controllerName        = "fake-controller"
		anotherControllerName = "another-controller"
		qosPriority           = 1000
	)
	var syncerToBuildData = AddressSetsSyncer{
		controllerName: controllerName,
	}

	ginkgo.It("destroys address sets in old non dual stack format", func() {
		testData := []asSync{
			// to be removed as the format is stale, and no references exist
			{
				before: createInitialAS("as1", nil),
				remove: true,
			},
			// not to be removed, address in new dual stack format, ipv4
			{
				before:                createInitialAS("as1"+legacyIPv4AddressSetSuffix, nil),
				after:                 syncerToBuildData.getNamespaceAddrSetDbIDs("as1"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
			// not to be removed, address in new dual stack format, ipv6
			{
				before:                createInitialAS("as2"+legacyIPv6AddressSetSuffix, nil),
				after:                 syncerToBuildData.getNamespaceAddrSetDbIDs("as2"),
				addressSetFactoryIPID: ipv6AddressSetFactoryID,
			},
		}
		testSyncerWithData(testData, nil, nil, controllerName)
	})
	ginkgo.It("skips address sets owned by another controller", func() {
		testData := []asSync{
			{
				before: &nbdb.AddressSet{
					UUID: "as1",
					Name: hashedAddressSet("as1"),
					ExternalIDs: map[string]string{
						libovsdbops.OwnerControllerKey.String(): anotherControllerName,
						"name":                                  "as_name"},
				},
				leave: true,
			},
			{
				before: &nbdb.AddressSet{
					UUID: "as2",
					Name: hashedAddressSet("as2"),
					ExternalIDs: map[string]string{
						libovsdbops.OwnerControllerKey.String(): anotherControllerName,
						"name":                                  "as_name_v4"},
				},
				leave: true,
			},
		}
		testSyncerWithData(testData, nil, nil, controllerName)
	})
	ginkgo.It("ignores stale address set with reference, no ipFamily", func() {
		// no ip family
		asName := hybridRoutePolicyPrefix + "node1"
		hashedASName := hashedAddressSet(asName)
		testData := []asSync{
			{
				before: createInitialAS(asName, []string{testIPv4}),
				leave:  true,
			},
		}
		initialDb := []libovsdbtest.TestData{
			&nbdb.LogicalRouterPolicy{
				UUID:     "lrp1",
				Priority: types.HybridOverlayReroutePriority,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{testGatewayIPv4},
				Match:    fmt.Sprintf(`inport == "%s%s" && ip4.src == $%s`, types.RouterToSwitchPrefix, "node1", hashedASName),
			},
			&nbdb.LogicalRouter{
				UUID:     types.OVNClusterRouter + "-UUID",
				Name:     types.OVNClusterRouter,
				Policies: []string{"lrp1"},
			},
		}
		testSyncerWithData(testData, initialDb, nil, controllerName)
	})
	ginkgo.It("ignores stale address set with reference, no ExternalIDs[name]", func() {
		hashedASName := hashedAddressSet("as1")
		testData := []asSync{
			{
				before: &nbdb.AddressSet{
					UUID: "as1",
					Name: hashedASName,
					ExternalIDs: map[string]string{
						"wrong-id": "as_name"},
				},
				leave: true,
			},
		}
		initialDb := []libovsdbtest.TestData{
			&nbdb.LogicalRouterPolicy{
				UUID:     "lrp1",
				Priority: types.HybridOverlayReroutePriority,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{testGatewayIPv4},
				Match:    fmt.Sprintf(`inport == "%s%s" && ip4.src == $%s`, types.RouterToSwitchPrefix, "node1", hashedASName),
			},
			&nbdb.LogicalRouter{
				UUID:     types.OVNClusterRouter + "-UUID",
				Name:     types.OVNClusterRouter,
				Policies: []string{"lrp1"},
			},
		}
		testSyncerWithData(testData, initialDb, nil, controllerName)
	})
	ginkgo.It("preserves unknown ExternalIDs", func() {
		initialAS := createInitialAS(hybridRoutePolicyPrefix+"node1_v4", []string{testIPv4})
		initialAS.ExternalIDs["unknown_id"] = "id_value"
		testData := []asSync{
			{
				before:                initialAS,
				after:                 syncerToBuildData.getHybridRouteAddrSetDbIDs("node1"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
				afterTweak: func(as *nbdb.AddressSet) {
					as.ExternalIDs["unknown_id"] = "id_value"
				},
			},
		}
		testSyncerWithData(testData, nil, nil, controllerName)
	})
	// verify different address set owners
	ginkgo.It("updates address set owned by HybridNodeRouteOwnerType and its references", func() {
		asName := hybridRoutePolicyPrefix + "node1_v4"
		hashedASName := hashedAddressSet(asName)
		testData := []asSync{
			{
				before:                createInitialAS(asName, []string{testIPv4}),
				after:                 syncerToBuildData.getHybridRouteAddrSetDbIDs("node1"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
		}
		initialDb := []libovsdbtest.TestData{
			&nbdb.LogicalRouterPolicy{
				UUID:     "lrp1",
				Priority: types.HybridOverlayReroutePriority,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{testGatewayIPv4},
				Match:    fmt.Sprintf(`inport == "%s%s" && ip4.src == $%s`, types.RouterToSwitchPrefix, "node1", hashedASName),
			},
			&nbdb.LogicalRouter{
				UUID:     types.OVNClusterRouter + "-UUID",
				Name:     types.OVNClusterRouter,
				Policies: []string{"lrp1"},
			},
		}
		testSyncerWithData(testData, initialDb, nil, controllerName)
	})
	ginkgo.It("updates address set owned by EgressQoSOwnerType and its references", func() {
		asName := egressQoSRulePrefix + "namespace-123_v4"
		hashedASName := hashedAddressSet(asName)
		testData := []asSync{
			{
				before:                createInitialAS(asName, []string{testIPv4}),
				after:                 syncerToBuildData.getEgressQosAddrSetDbIDs("namespace", "123"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
		}
		initialDb := []libovsdbtest.TestData{
			&nbdb.LogicalSwitch{
				UUID:     "node1-UUID",
				Name:     "node1",
				QOSRules: []string{"qos1-UUID"},
			},
			&nbdb.QoS{
				Direction:   nbdb.QoSDirectionToLport,
				Match:       "(ip4.dst == 1.2.3.4/32) && ip4.src == $" + hashedASName,
				Priority:    qosPriority,
				Action:      map[string]int{nbdb.QoSActionDSCP: 40},
				ExternalIDs: map[string]string{"EgressQoS": "namespace"},
				UUID:        "qos1-UUID",
			},
		}
		testSyncerWithData(testData, initialDb, nil, controllerName)
	})
	ginkgo.It("updates address set owned by EgressQoSOwnerType and its references", func() {
		// this test checks the batching works for number of address sets = 5, which is 2.5 test batch sizes.
		asName1 := egressQoSRulePrefix + "namespace-1_v4"
		hashedASName1 := hashedAddressSet(asName1)
		asName2 := egressQoSRulePrefix + "namespace-2_v4"
		hashedASName2 := hashedAddressSet(asName2)
		asName3 := egressQoSRulePrefix + "namespace-3_v4"
		hashedASName3 := hashedAddressSet(asName3)
		asName4 := egressQoSRulePrefix + "namespace-4_v4"
		hashedASName4 := hashedAddressSet(asName4)
		asName5 := egressQoSRulePrefix + "namespace-5_v4"
		hashedASName5 := hashedAddressSet(asName5)
		testData := []asSync{
			{
				before:                createInitialAS(asName1, []string{testIPv4}),
				after:                 syncerToBuildData.getEgressQosAddrSetDbIDs("namespace", "1"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
			{
				before:                createInitialAS(asName2, []string{testIPv4}),
				after:                 syncerToBuildData.getEgressQosAddrSetDbIDs("namespace", "2"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
			{
				before:                createInitialAS(asName3, []string{testIPv4}),
				after:                 syncerToBuildData.getEgressQosAddrSetDbIDs("namespace", "3"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
			{
				before:                createInitialAS(asName4, []string{testIPv4}),
				after:                 syncerToBuildData.getEgressQosAddrSetDbIDs("namespace", "4"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
			{
				before:                createInitialAS(asName5, []string{testIPv4}),
				after:                 syncerToBuildData.getEgressQosAddrSetDbIDs("namespace", "5"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
		}
		initialDb := []libovsdbtest.TestData{
			&nbdb.QoS{
				Direction:   nbdb.QoSDirectionToLport,
				Match:       "(ip4.dst == 1.2.3.4/32) && ip4.src == $" + hashedASName1,
				Priority:    qosPriority,
				Action:      map[string]int{nbdb.QoSActionDSCP: 40},
				ExternalIDs: map[string]string{"EgressQoS": "namespace"},
				UUID:        "qos1-UUID",
			},
			&nbdb.QoS{
				Direction:   nbdb.QoSDirectionToLport,
				Match:       "(ip4.dst == 1.2.3.4/32) && ip4.src == $" + hashedASName2,
				Priority:    qosPriority,
				Action:      map[string]int{nbdb.QoSActionDSCP: 40},
				ExternalIDs: map[string]string{"EgressQoS": "namespace"},
				UUID:        "qos2-UUID",
			},
			&nbdb.QoS{
				Direction:   nbdb.QoSDirectionToLport,
				Match:       "(ip4.dst == 1.2.3.4/32) && ip4.src == $" + hashedASName3,
				Priority:    qosPriority,
				Action:      map[string]int{nbdb.QoSActionDSCP: 40},
				ExternalIDs: map[string]string{"EgressQoS": "namespace"},
				UUID:        "qos3-UUID",
			},
			&nbdb.QoS{
				Direction:   nbdb.QoSDirectionToLport,
				Match:       "(ip4.dst == 1.2.3.4/32) && ip4.src == $" + hashedASName4,
				Priority:    qosPriority,
				Action:      map[string]int{nbdb.QoSActionDSCP: 40},
				ExternalIDs: map[string]string{"EgressQoS": "namespace"},
				UUID:        "qos4-UUID",
			},
			&nbdb.QoS{
				Direction:   nbdb.QoSDirectionToLport,
				Match:       "(ip4.dst == 1.2.3.4/32) && ip4.src == $" + hashedASName5,
				Priority:    qosPriority,
				Action:      map[string]int{nbdb.QoSActionDSCP: 40},
				ExternalIDs: map[string]string{"EgressQoS": "namespace"},
				UUID:        "qos5-UUID",
			},
			&nbdb.LogicalSwitch{
				UUID:     "node1-UUID",
				Name:     "node1",
				QOSRules: []string{"qos1-UUID", "qos2-UUID", "qos3-UUID", "qos4-UUID", "qos5-UUID"},
			}}
		testSyncerWithData(testData, initialDb, nil, controllerName)
	})
	ginkgo.It("updates address set owned by EgressFirewallDNSOwnerType and its references", func() {
		asName := "dns.name_v4"
		hashedASName := hashedAddressSet(asName)
		testData := []asSync{
			{
				before:                createInitialAS(asName, []string{testIPv4}),
				after:                 syncerToBuildData.getEgressFirewallDNSAddrSetDbIDs("dns.name"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
		}
		acl := libovsdbops.BuildACL(
			"aclName",
			nbdb.ACLDirectionToLport,
			types.EgressFirewallStartPriority,
			"(ip4.dst == 1.2.3.4/32) && ip4.src == $namespaceAS && ip4.dst == $"+hashedASName,
			nbdb.ACLActionAllow,
			types.OvnACLLoggingMeter,
			"",
			false,
			map[string]string{egressFirewallACLExtIdKey: "egressfirewall1"},
			nil,
			types.PrimaryACLTier,
		)
		acl.UUID = "acl-UUID"
		initialDb := []libovsdbtest.TestData{
			acl,
			&nbdb.LogicalSwitch{
				UUID: "node1-UUID",
				Name: "node1",
				ACLs: []string{acl.UUID},
			},
		}
		testSyncerWithData(testData, initialDb, nil, controllerName)
	})
	ginkgo.It("updates address set owned by NetworkPolicyOwnerType and its references", func() {
		asNameEgress := "namespace.netpol.egress.0_v4"
		hashedASNameasNameEgress := hashedAddressSet(asNameEgress)
		asNameIngress := "namespace.netpol.ingress.0_v4"
		hashedASNameasNameIngress := hashedAddressSet(asNameIngress)
		testData := []asSync{
			{
				before:                createInitialAS(asNameEgress, []string{testIPv4}),
				after:                 syncerToBuildData.getNetpolAddrSetDbIDs("namespace", "netpol", "egress", "0"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
			{
				before:                createInitialAS(asNameIngress, []string{"1.1.1.2"}),
				after:                 syncerToBuildData.getNetpolAddrSetDbIDs("namespace", "netpol", "ingress", "0"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
		}
		acl1 := libovsdbops.BuildACL(
			"acl1",
			nbdb.ACLDirectionFromLport,
			types.EgressFirewallStartPriority,
			fmt.Sprintf("ip4.src == {$%s} && outport == @a13757631697825269621", hashedASNameasNameEgress),
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			"",
			false,
			nil,
			map[string]string{
				"apply-after-lb": "true",
			},
			types.PrimaryACLTier,
		)
		acl1.UUID = "acl1-UUID"
		acl2 := libovsdbops.BuildACL(
			"acl2",
			nbdb.ACLDirectionToLport,
			types.EgressFirewallStartPriority,
			fmt.Sprintf("inport == @a13757631697825269621 && ip.dst == {$%s}", hashedASNameasNameIngress),
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			"",
			false,
			nil,
			nil,
			types.PrimaryACLTier,
		)
		acl2.UUID = "acl2-UUID"
		initialDb := []libovsdbtest.TestData{
			acl1,
			acl2,
			&nbdb.LogicalSwitch{
				UUID: "node1-UUID",
				Name: "node1",
				ACLs: []string{acl1.UUID, acl2.UUID},
			},
		}
		testSyncerWithData(testData, initialDb, nil, controllerName)
	})
	ginkgo.It("updates address set owned by NamespaceOwnerType and its references", func() {
		asName := "namespace_v4"
		hashedASName := hashedAddressSet(asName)
		testData := []asSync{
			{
				before:                createInitialAS(asName, []string{testIPv4}),
				after:                 syncerToBuildData.getNamespaceAddrSetDbIDs("namespace"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
		}
		// namespace-owned address set may be referenced from different objects,
		// test lrp, acl and qos
		acl := libovsdbops.BuildACL(
			"aclName",
			nbdb.ACLDirectionToLport,
			types.EgressFirewallStartPriority,
			"ip4.src == $"+hashedASName,
			nbdb.ACLActionAllow,
			types.OvnACLLoggingMeter,
			"",
			false,
			map[string]string{egressFirewallACLExtIdKey: "egressfirewall1"},
			nil,
			types.PrimaryACLTier,
		)
		acl.UUID = "acl-UUID"
		initialDb := []libovsdbtest.TestData{
			acl,
			&nbdb.LogicalRouterPolicy{
				UUID:     "lrp1",
				Priority: types.HybridOverlayReroutePriority,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{testGatewayIPv4},
				Match:    "ip4.src == $" + hashedASName,
			},
			&nbdb.QoS{
				Direction:   nbdb.QoSDirectionToLport,
				Match:       "(ip4.dst == 1.2.3.4/32) && ip4.src == $" + hashedASName,
				Priority:    qosPriority,
				Action:      map[string]int{nbdb.QoSActionDSCP: 40},
				ExternalIDs: map[string]string{"EgressQoS": "namespace"},
				UUID:        "qos1-UUID",
			},
			&nbdb.LogicalRouter{
				UUID:     types.OVNClusterRouter + "-UUID",
				Name:     types.OVNClusterRouter,
				Policies: []string{"lrp1"},
			},
			&nbdb.LogicalSwitch{
				UUID:     "node1-UUID",
				Name:     "node1",
				ACLs:     []string{acl.UUID},
				QOSRules: []string{"qos1-UUID"},
			},
		}
		testSyncerWithData(testData, initialDb, nil, controllerName)
	})
	ginkgo.It("updates address set owned by EgressIP and EgressSVC and its references", func() {
		asName1 := egressIPServedPods + legacyIPv4AddressSetSuffix
		hashedASName1 := hashedAddressSet(asName1)
		asName2 := clusterNodeIP + legacyIPv4AddressSetSuffix
		hashedASName2 := hashedAddressSet(asName2)
		asName3 := egressServiceServedPods + legacyIPv4AddressSetSuffix
		hashedASName3 := hashedAddressSet(asName3)
		testData := []asSync{
			{
				before:                createInitialAS(asName1, []string{testIPv4}),
				after:                 syncerToBuildData.getEgressIPAddrSetDbIDs(egressIPServedPodsAddrSetName, "default"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
			{
				before:                createInitialAS(asName2, []string{testIPv4}),
				after:                 syncerToBuildData.getEgressIPAddrSetDbIDs(nodeIPAddrSetName, "default"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
			{
				before:                createInitialAS(asName3, []string{testIPv4}),
				after:                 syncerToBuildData.getEgressServiceAddrSetDbIDs(),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
		}
		initialDb := []libovsdbtest.TestData{
			&nbdb.LogicalRouterPolicy{
				Priority: types.DefaultNoRereoutePriority,
				Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
					hashedASName1, hashedASName3, hashedASName2),
				Action:  nbdb.LogicalRouterPolicyActionAllow,
				UUID:    "default-no-reroute-node-UUID",
				Options: map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
			},
			&nbdb.LogicalRouter{
				UUID:     types.OVNClusterRouter + "-UUID",
				Name:     types.OVNClusterRouter,
				Policies: []string{"default-no-reroute-node-UUID"},
			},
		}
		testSyncerWithData(testData, initialDb, nil, controllerName)
	})
	ginkgo.It("updates address set and its references for both ip families", func() {
		// the difference from "updates address sets owned by HybridNodeRouteOwnerType and its references" test
		// is that there are 2 address sets for ipv4 and ipv6
		asNamev4 := hybridRoutePolicyPrefix + "node1_v4"
		hashedASNamev4 := hashedAddressSet(asNamev4)
		asNamev6 := hybridRoutePolicyPrefix + "node1_v6"
		hashedASNamev6 := hashedAddressSet(asNamev6)
		testData := []asSync{
			{
				before:                createInitialAS(asNamev4, []string{testIPv4}),
				after:                 syncerToBuildData.getHybridRouteAddrSetDbIDs("node1"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
			{
				before:                createInitialAS(asNamev6, []string{testIPv6}),
				after:                 syncerToBuildData.getHybridRouteAddrSetDbIDs("node1"),
				addressSetFactoryIPID: ipv6AddressSetFactoryID,
			},
		}
		initialDb := []libovsdbtest.TestData{
			&nbdb.LogicalRouterPolicy{
				UUID:     "lrp1",
				Priority: types.HybridOverlayReroutePriority,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{testGatewayIPv4},
				Match:    fmt.Sprintf(`inport == "%s%s" && ip4.src == $%s`, types.RouterToSwitchPrefix, "node1", hashedASNamev4),
			},
			&nbdb.LogicalRouterPolicy{
				UUID:     "lrp2",
				Priority: types.HybridOverlayReroutePriority,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{testGatewayIPv6},
				Match:    fmt.Sprintf(`inport == "%s%s" && ip6.src == $%s`, types.RouterToSwitchPrefix, "node1", hashedASNamev6),
			},
			&nbdb.LogicalRouter{
				UUID:     types.OVNClusterRouter + "-UUID",
				Name:     types.OVNClusterRouter,
				Policies: []string{"lrp1", "lrp2"},
			},
		}
		testSyncerWithData(testData, initialDb, nil, controllerName)
	})
	ginkgo.It("updates address set and its references for new dualstack format, and ignores a stale one", func() {
		// the difference from "updates address sets owned by HybridNodeRouteOwnerType and its references" test
		// is that there 2 address sets for ipv4 and old non-dualstack format.
		// a new one should be updated, and a stale one should be ignored
		asNamev4 := hybridRoutePolicyPrefix + "node1_v4"
		hashedASNamev4 := hashedAddressSet(asNamev4)
		asNameOld := hybridRoutePolicyPrefix + "node1"
		hashedASNameOld := hashedAddressSet(asNameOld)
		testData := []asSync{
			{
				before:                createInitialAS(asNamev4, []string{testIPv4}),
				after:                 syncerToBuildData.getHybridRouteAddrSetDbIDs("node1"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
			{
				before: createInitialAS(asNameOld, []string{testIPv4}),
				leave:  true,
			},
		}
		initialDb := []libovsdbtest.TestData{
			&nbdb.LogicalRouterPolicy{
				UUID:     "lrp1",
				Priority: types.HybridOverlayReroutePriority,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{testGatewayIPv4},
				Match:    fmt.Sprintf(`inport == "%s%s" && ip4.src == $%s`, types.RouterToSwitchPrefix, "node1", hashedASNamev4),
			},
			&nbdb.LogicalRouterPolicy{
				UUID:     "lrp2",
				Priority: types.HybridOverlayReroutePriority,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{testGatewayIPv4},
				Match:    fmt.Sprintf(`inport == "%s%s" && ip4.src == $%s`, types.RouterToSwitchPrefix, "node1", hashedASNameOld),
			},
			&nbdb.LogicalRouter{
				UUID:     types.OVNClusterRouter + "-UUID",
				Name:     types.OVNClusterRouter,
				Policies: []string{"lrp1", "lrp2"},
			},
		}
		newHashedASNamev4 := buildNewAddressSet(testData[0].after, ipv4AddressSetFactoryID).Name
		testSyncerWithData(testData, initialDb, []libovsdbtest.TestData{
			&nbdb.LogicalRouterPolicy{
				UUID:     "lrp1",
				Priority: types.HybridOverlayReroutePriority,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{testGatewayIPv4},
				Match:    fmt.Sprintf(`inport == "%s%s" && ip4.src == $%s`, types.RouterToSwitchPrefix, "node1", newHashedASNamev4),
			},
			&nbdb.LogicalRouterPolicy{
				UUID:     "lrp2",
				Priority: types.HybridOverlayReroutePriority,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{testGatewayIPv4},
				Match:    fmt.Sprintf(`inport == "%s%s" && ip4.src == $%s`, types.RouterToSwitchPrefix, "node1", hashedASNameOld),
			},
			&nbdb.LogicalRouter{
				UUID:     types.OVNClusterRouter + "-UUID",
				Name:     types.OVNClusterRouter,
				Policies: []string{"lrp1", "lrp2"},
			},
		}, controllerName)
	})

	ginkgo.It("updates referencing object if at least one address set was updated", func() {
		// address set without ip family and ips, will be ignored
		asName1 := egressIPServedPods
		hashedASName1 := hashedAddressSet(asName1)
		asName2 := clusterNodeIP + legacyIPv4AddressSetSuffix
		hashedASName2 := hashedAddressSet(asName2)
		asName3 := egressServiceServedPods + legacyIPv4AddressSetSuffix
		hashedASName3 := hashedAddressSet(asName3)
		testData := []asSync{
			{
				before: createInitialAS(asName1, []string{}),
				leave:  true,
			},
			{
				before:                createInitialAS(asName2, []string{testIPv4}),
				after:                 syncerToBuildData.getEgressIPAddrSetDbIDs(nodeIPAddrSetName, "default"),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
			{
				before:                createInitialAS(asName3, []string{testIPv4}),
				after:                 syncerToBuildData.getEgressServiceAddrSetDbIDs(),
				addressSetFactoryIPID: ipv4AddressSetFactoryID,
			},
		}
		initialDb := []libovsdbtest.TestData{
			&nbdb.LogicalRouterPolicy{
				Priority: types.DefaultNoRereoutePriority,
				Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
					hashedASName1, hashedASName3, hashedASName2),
				Action:  nbdb.LogicalRouterPolicyActionAllow,
				UUID:    "default-no-reroute-node-UUID",
				Options: map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
			},
			&nbdb.LogicalRouter{
				UUID:     types.OVNClusterRouter + "-UUID",
				Name:     types.OVNClusterRouter,
				Policies: []string{"default-no-reroute-node-UUID"},
			},
		}
		testSyncerWithData(testData, initialDb, nil, controllerName)
	})
})

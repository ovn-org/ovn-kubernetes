package port_group

import (
	"fmt"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"strings"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

type pgSync struct {
	before     *nbdb.PortGroup
	after      *libovsdbops.DbObjectIDs
	afterTweak func(group *nbdb.PortGroup)
	remove     bool
	leave      bool
}

// data is used to pass port group for initial and expected db state, initialDbState may be used to add objects
// of other types to the initial db state, and finalDbState may be used to set the expected state of objects
// passed in initialDbState. If finalDbState is nil, final state will be updated automatically by changing port group
// references for initial objects from initialDbState.
func testSyncerWithData(data []pgSync, initialDbState, finalDbState []libovsdbtest.TestData) {
	// create initial db setup
	var fakePortUUID string
	var dbPortAndSwitch []libovsdbtest.TestData

	dbSetup := libovsdbtest.TestSetup{NBData: append(initialDbState)}
	for _, pgSync := range data {
		dbSetup.NBData = append(dbSetup.NBData, pgSync.before)
		if len(pgSync.before.Ports) > 0 {
			fakePortUUID = pgSync.before.Ports[0]
		}
	}
	if fakePortUUID != "" {
		dbPort := &nbdb.LogicalSwitchPort{
			UUID: fakePortUUID,
			Name: "fake-port",
		}

		dbSwitch := &nbdb.LogicalSwitch{
			UUID:  "fake-switch",
			Name:  "fake-switch",
			Ports: []string{fakePortUUID},
		}
		dbPortAndSwitch = []libovsdbtest.TestData{dbPort, dbSwitch}
		dbSetup.NBData = append(dbSetup.NBData, dbPortAndSwitch...)
	}

	libovsdbOvnNBClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
	defer libovsdbCleanup.Cleanup()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	// create expected data using addressSetFactory
	expectedDbState := initialDbState
	if finalDbState != nil {
		expectedDbState = finalDbState
	}
	if fakePortUUID != "" {
		expectedDbState = append(expectedDbState, dbPortAndSwitch...)
	}

	for _, pgSync := range data {
		if pgSync.remove {
			continue
		}
		if pgSync.leave {
			expectedDbState = append(expectedDbState, pgSync.before)
		} else if pgSync.after != nil {
			updatedPG := getUpdatedPG(pgSync.before, pgSync.after)
			if pgSync.afterTweak != nil {
				pgSync.afterTweak(updatedPG)
			}
			expectedDbState = append(expectedDbState, updatedPG)
			if finalDbState == nil {
				for _, dbObj := range expectedDbState {
					if acl, ok := dbObj.(*nbdb.ACL); ok {
						acl.Match = strings.ReplaceAll(acl.Match, "@"+pgSync.before.Name, "@"+updatedPG.Name)
					}
				}
			}
		}
	}
	// run sync
	syncer := NewPortGroupSyncer(libovsdbOvnNBClient)
	// to make sure batching works, set it to 0.5 to cover number of batches = 0,1,>1
	syncer.getPGWeight = func(acls, ports int) float64 {
		return 0.5
	}
	err = syncer.SyncPortGroups()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	// check results
	gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDbState))
}

func createInitialPG(hashedName, name, networkName string, portUUIDs, aclUUIDs []string) *nbdb.PortGroup {
	externalIDs := map[string]string{"name": name}
	if networkName != "" {
		externalIDs[types.NetworkExternalID] = networkName
	}
	return &nbdb.PortGroup{
		UUID:        hashedName,
		Name:        hashedName,
		ExternalIDs: externalIDs,
		Ports:       portUUIDs,
		ACLs:        aclUUIDs,
	}
}

func createReferencingACL(hashedName string, externalIDs map[string]string) *nbdb.ACL {
	acl := libovsdbops.BuildACL(
		"",
		nbdb.ACLDirectionToLport,
		types.EgressFirewallStartPriority,
		"outport == @"+hashedName+" && ip4.src == $namespaceAS",
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		externalIDs,
		nil,
		types.DefaultACLTier,
	)
	acl.UUID = hashedName + "-UUID"
	return acl
}

func getUpdatedPG(pg *nbdb.PortGroup, dbIDs *libovsdbops.DbObjectIDs) *nbdb.PortGroup {
	newPG := pg.DeepCopy()
	newPG.UUID += "-new"

	newPG.Name = libovsdbutil.GetPortGroupName(dbIDs)
	newPG.ExternalIDs = dbIDs.GetExternalIDs()
	return newPG
}

func hashedPG(s string) string {
	return util.HashForOVN(s)
}

func getNetworkScopedName(netName, name string) string {
	if netName == "" {
		return name
	}
	return fmt.Sprintf("%s%s", util.GetSecondaryNetworkPrefix(netName), name)
}

var _ = ginkgo.Describe("OVN Port Group Syncer", func() {
	const (
		defaultNetworkExternalID   = ""
		anotherControllerName      = "another-controller"
		fakePortUUID               = "portUUID"
		secondaryNetworkExternalID = "secondary"
	)

	ginkgo.It("skips port groups with owner", func() {
		testData := []pgSync{
			{
				before: &nbdb.PortGroup{
					UUID: "pg1",
					Name: hashedPG("as1"),
					ExternalIDs: map[string]string{
						libovsdbops.OwnerControllerKey.String(): anotherControllerName,
						"name":                                  "pg_name"},
				},
				leave: true,
			},
		}
		testSyncerWithData(testData, nil, nil)
	})
	// Cluster port groups are only created by the Default Controller at this point
	ginkgo.It("updates port group owned by ClusterOwnerType and its references", func() {
		acl1 := createReferencingACL(types.ClusterPortGroupNameBase, nil)
		acl2 := createReferencingACL(types.ClusterRtrPortGroupNameBase, nil)
		testData := []pgSync{
			{
				before: createInitialPG(types.ClusterPortGroupNameBase, types.ClusterPortGroupNameBase, defaultNetworkExternalID,
					[]string{fakePortUUID}, []string{acl1.UUID}),
				after: getPortGroupClusterDbIDs(types.ClusterPortGroupNameBase, defaultNetworkExternalID),
			},
			{
				before: createInitialPG(types.ClusterRtrPortGroupNameBase, types.ClusterRtrPortGroupNameBase, defaultNetworkExternalID,
					[]string{fakePortUUID}, []string{acl2.UUID}),
				after: getPortGroupClusterDbIDs(types.ClusterRtrPortGroupNameBase, defaultNetworkExternalID),
			},
		}
		initialDb := []libovsdbtest.TestData{acl1, acl2}
		testSyncerWithData(testData, initialDb, nil)
	})
	// ANP/BANP port groups are only created by the Default Controller at this point
	ginkgo.It("updates port group owned by AdminNetworkPolicy and its references", func() {
		policyName := "test-anp"
		readableName := "ANP:" + policyName
		pgName := hashedPG("ANP:" + policyName)
		acl := createReferencingACL(pgName, nil)
		testData := []pgSync{
			{
				before: createInitialPG(pgName, readableName, defaultNetworkExternalID,
					[]string{fakePortUUID}, []string{acl.UUID}),
				after: getPortGroupAdminNetworkPolicyDbIDs(policyName, false, defaultNetworkExternalID),
			},
		}
		initialDb := []libovsdbtest.TestData{acl}
		testSyncerWithData(testData, initialDb, nil)
	})
	ginkgo.It("updates port group owned by BaselineAdminNetworkPolicy and its references", func() {
		policyName := "test-banp"
		readableName := "BANP:" + policyName
		pgName := hashedPG("BANP:" + policyName)
		acl := createReferencingACL(pgName, nil)
		testData := []pgSync{
			{
				before: createInitialPG(pgName, readableName, defaultNetworkExternalID,
					[]string{fakePortUUID}, []string{acl.UUID}),
				after: getPortGroupAdminNetworkPolicyDbIDs(policyName, true, defaultNetworkExternalID),
			},
		}
		initialDb := []libovsdbtest.TestData{acl}
		testSyncerWithData(testData, initialDb, nil)
	})
	// port groups that exist both for the Default and Secondary controller
	for _, networkExternalID := range []string{defaultNetworkExternalID, secondaryNetworkExternalID} {
		networkExternalID := networkExternalID
		// verify different port group owners
		ginkgo.It(fmt.Sprintf("updates port group owned by NamespaceOwnerType and its references, network %s", networkExternalID), func() {
			namespaceName := "namespace"
			pgName := hashedPG(getNetworkScopedName(networkExternalID, namespaceName))
			acl := createReferencingACL(pgName, nil)
			testData := []pgSync{
				{
					before: createInitialPG(pgName, namespaceName, networkExternalID,
						[]string{fakePortUUID}, []string{acl.UUID}),
					after: getPortGroupNamespaceDbIDs(namespaceName, networkExternalID),
				},
			}
			initialDb := []libovsdbtest.TestData{acl}
			testSyncerWithData(testData, initialDb, nil)
		})
		ginkgo.It(fmt.Sprintf("updates port group owned by NetpolNamespaceOwnerType and its references, network %s", networkExternalID), func() {
			namespaceName := "namespace"
			pgName := hashedPG(getNetworkScopedName(networkExternalID, namespaceName)) + "_" + egressDefaultDenySuffix
			// default deny port group's namespace is extracted from the referencing acl
			acl := createReferencingACL(pgName, map[string]string{
				libovsdbops.ObjectNameKey.String(): namespaceName,
			})
			testData := []pgSync{
				{
					before: createInitialPG(pgName, pgName, networkExternalID,
						[]string{fakePortUUID}, []string{acl.UUID}),
					after: getPortGroupNetpolNamespaceDbIDs(namespaceName, "Egress", networkExternalID),
				},
			}
			initialDb := []libovsdbtest.TestData{acl}
			testSyncerWithData(testData, initialDb, nil)
		})
		ginkgo.It(fmt.Sprintf("updates port group owned by NetworkPolicyOwnerType and its references, network %s", networkExternalID), func() {
			namespaceName := "namespace"
			policyName := "netpol"
			readableName := fmt.Sprintf("%s_%s", namespaceName, policyName)
			pgName := hashedPG(getNetworkScopedName(networkExternalID, readableName))
			acl := createReferencingACL(pgName, nil)
			testData := []pgSync{
				{
					before: createInitialPG(pgName, readableName, networkExternalID,
						[]string{fakePortUUID}, []string{acl.UUID}),
					after: getPortGroupNetworkPolicyDbIDs(namespaceName, policyName, networkExternalID),
				},
			}
			initialDb := []libovsdbtest.TestData{acl}
			testSyncerWithData(testData, initialDb, nil)
		})
	}
	for _, pgCount := range []int{0, 1, 2, 3, 4} {
		pgCount := pgCount
		// verify different batch sizes
		ginkgo.It(fmt.Sprintf("test batching, pg number %d", pgCount), func() {
			initialDb := []libovsdbtest.TestData{}
			testData := []pgSync{}
			for i := 0; i < pgCount; i++ {
				namespaceName := fmt.Sprintf("namespace-%d", i)
				pgName := hashedPG(getNetworkScopedName(defaultNetworkExternalID, namespaceName)) + "_" + egressDefaultDenySuffix
				// default deny port group's namespace is extracted from the referencing acl
				acl := createReferencingACL(pgName, map[string]string{
					libovsdbops.ObjectNameKey.String(): namespaceName,
				})
				testData = append(testData, pgSync{
					before: createInitialPG(pgName, pgName, defaultNetworkExternalID,
						[]string{fakePortUUID}, []string{acl.UUID}),
					after: getPortGroupNetpolNamespaceDbIDs(namespaceName, "Egress", defaultNetworkExternalID),
				})
				initialDb = append(initialDb, acl)
			}
			testSyncerWithData(testData, initialDb, nil)
		})
	}
})

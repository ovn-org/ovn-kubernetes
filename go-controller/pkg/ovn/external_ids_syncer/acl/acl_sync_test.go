package acl

import (
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"strings"
)

type aclSync struct {
	before *nbdb.ACL
	after  *libovsdbops.DbObjectIDs
}

func testSyncerWithData(data []aclSync, controllerName string) {
	// create initial db setup
	dbSetup := libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{}}
	for _, asSync := range data {
		asSync.before.UUID = *asSync.before.Name + "-UUID"
		dbSetup.NBData = append(dbSetup.NBData, asSync.before)
	}
	libovsdbOvnNBClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
	defer libovsdbCleanup.Cleanup()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	// create expected data using addressSetFactory
	expectedDbState := []libovsdbtest.TestData{}
	for _, aclSync := range data {
		acl := aclSync.before
		acl.ExternalIDs = aclSync.after.GetExternalIDs()
		expectedDbState = append(expectedDbState, acl)
	}
	// run sync
	syncer := NewACLSyncer(libovsdbOvnNBClient, controllerName)
	err = syncer.SyncACLs()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	// check results
	gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDbState))
}

func joinACLName(substrings ...string) string {
	return strings.Join(substrings, "_")
}

var _ = ginkgo.Describe("OVN ACL Syncer", func() {
	const (
		controllerName = "fake-controller"
		namespace1     = "namespace1"
	)
	var syncerToBuildData = aclSyncer{
		controllerName: controllerName,
	}

	ginkgo.It("updates multicast acls", func() {
		testData := []aclSync{
			// defaultDenyEgressACL
			{
				before: libovsdbops.BuildACL(
					joinACLName(types.ClusterPortGroupName, "DefaultDenyMulticastEgress"),
					nbdb.ACLDirectionFromLport,
					types.DefaultMcastDenyPriority,
					"(ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))",
					nbdb.ACLActionDrop,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: "Egress",
					},
					nil,
				),
				after: syncerToBuildData.getDefaultMcastACLDbIDs(mcastDefaultDenyID, "Egress"),
			},
			// defaultDenyIngressACL
			{
				before: libovsdbops.BuildACL(
					joinACLName(types.ClusterPortGroupName, "DefaultDenyMulticastIngress"),
					nbdb.ACLDirectionToLport,
					types.DefaultMcastDenyPriority,
					"(ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))",
					nbdb.ACLActionDrop,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: "Ingress",
					},
					nil,
				),
				after: syncerToBuildData.getDefaultMcastACLDbIDs(mcastDefaultDenyID, "Ingress"),
			},
			// defaultAllowEgressACL
			{
				before: libovsdbops.BuildACL(
					joinACLName(types.ClusterRtrPortGroupName, "DefaultAllowMulticastEgress"),
					nbdb.ACLDirectionFromLport,
					types.DefaultMcastAllowPriority,
					"inport == @clusterRtrPortGroup && (ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))",
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: "Egress",
					},
					nil,
				),
				after: syncerToBuildData.getDefaultMcastACLDbIDs(mcastAllowInterNodeID, "Egress"),
			},
			// defaultAllowIngressACL
			{
				before: libovsdbops.BuildACL(
					joinACLName(types.ClusterRtrPortGroupName, "DefaultAllowMulticastIngress"),
					nbdb.ACLDirectionToLport,
					types.DefaultMcastAllowPriority,
					"outport == @clusterRtrPortGroup && (ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))",
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: "Ingress",
					},
					nil,
				),
				after: syncerToBuildData.getDefaultMcastACLDbIDs(mcastAllowInterNodeID, "Ingress"),
			},
			// nsAllowEgressACL
			{
				before: libovsdbops.BuildACL(
					joinACLName(namespace1, "MulticastAllowEgress"),
					nbdb.ACLDirectionFromLport,
					types.DefaultMcastAllowPriority,
					"inport == @a16982411286042166782 && ip4.mcast",
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: "Egress",
					},
					nil,
				),
				after: syncerToBuildData.getNamespaceMcastACLDbIDs(namespace1, "Egress"),
			},
			// nsAllowIngressACL
			{
				before: libovsdbops.BuildACL(
					joinACLName(namespace1, "MulticastAllowIngress"),
					nbdb.ACLDirectionToLport,
					types.DefaultMcastAllowPriority,
					"outport == @a16982411286042166782 && (igmp || (ip4.src == $a4322231855293774466 && ip4.mcast))",
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: "Ingress",
					},
					nil,
				),
				after: syncerToBuildData.getNamespaceMcastACLDbIDs(namespace1, "Ingress"),
			},
		}
		testSyncerWithData(testData, controllerName)
	})
})

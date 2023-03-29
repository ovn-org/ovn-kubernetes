package acl

import (
	"encoding/json"
	"fmt"
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
)

type aclSync struct {
	before *nbdb.ACL
	after  *libovsdbops.DbObjectIDs
}

func testSyncerWithData(data []aclSync, controllerName string, initialDbState []libovsdbtest.TestData,
	existingNodes []v1.Node) {
	// create initial db setup
	dbSetup := libovsdbtest.TestSetup{NBData: initialDbState}
	for _, asSync := range data {
		asSync.before.UUID = asSync.after.String() + "-UUID"
		dbSetup.NBData = append(dbSetup.NBData, asSync.before)
	}
	libovsdbOvnNBClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
	defer libovsdbCleanup.Cleanup()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	// create expected data using addressSetFactory
	expectedDbState := initialDbState
	for _, aclSync := range data {
		acl := aclSync.before
		acl.ExternalIDs = aclSync.after.GetExternalIDs()
		expectedDbState = append(expectedDbState, acl)
	}
	// run sync
	syncer := NewACLSyncer(libovsdbOvnNBClient, controllerName)
	err = syncer.SyncACLs(&v1.NodeList{Items: existingNodes})
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
		testSyncerWithData(testData, controllerName, []libovsdbtest.TestData{}, nil)
	})
	ginkgo.It("updates allow from node acls", func() {
		nodeName := "node1"
		ipv4MgmtIP := "10.244.0.2"
		ipv6MgmtIP := "fd02:0:0:2::2"

		testData := []aclSync{
			// ipv4 acl
			{
				before: libovsdbops.BuildACL(
					"",
					nbdb.ACLDirectionToLport,
					types.DefaultAllowPriority,
					"ip4.src=="+ipv4MgmtIP,
					nbdb.ACLActionAllowRelated,
					types.OvnACLLoggingMeter,
					"",
					false,
					nil,
					nil),
				after: syncerToBuildData.getAllowFromNodeACLDbIDs(nodeName, ipv4MgmtIP),
			},
			// ipv6 acl
			{
				before: libovsdbops.BuildACL(
					"",
					nbdb.ACLDirectionToLport,
					types.DefaultAllowPriority,
					"ip6.src=="+ipv6MgmtIP,
					nbdb.ACLActionAllowRelated,
					types.OvnACLLoggingMeter,
					"",
					false,
					nil,
					nil),
				after: syncerToBuildData.getAllowFromNodeACLDbIDs(nodeName, ipv6MgmtIP),
			},
		}
		hostSubnets := map[string][]string{types.DefaultNetworkName: {"10.244.0.0/24", "fd02:0:0:2::2895/64"}}
		bytes, err := json.Marshal(hostSubnets)
		gomega.Expect(err).ToNot(gomega.HaveOccurred())
		existingNodes := []v1.Node{{
			ObjectMeta: metav1.ObjectMeta{
				Name:        nodeName,
				Annotations: map[string]string{"k8s.ovn.org/node-subnets": string(bytes)},
			}}}
		testSyncerWithData(testData, controllerName, []libovsdbtest.TestData{}, existingNodes)
	})
	ginkgo.It("updates gress policy acls", func() {
		policyNamespace := "policyNamespace"
		policyName := "policyName"
		testData := []aclSync{
			{
				before: libovsdbops.BuildACL(
					policyNamespace+"_"+policyName+"_0",
					nbdb.ACLDirectionToLport,
					types.DefaultAllowPriority,
					"ip4.dst == 10.244.1.5/32 && inport == @a2653181086423119552",
					nbdb.ACLActionAllowRelated,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						l4MatchACLExtIdKey:     "tcp && 6380<=tcp.dst<=7000",
						ipBlockCIDRACLExtIdKey: "true",
						namespaceACLExtIdKey:   policyNamespace,
						policyACLExtIdKey:      policyName,
						policyTypeACLExtIdKey:  string(knet.PolicyTypeEgress),
						fmt.Sprintf(policyTypeNumACLExtIdKey, knet.PolicyTypeEgress): "0",
					},
					nil,
				),
				after: syncerToBuildData.getNetpolGressACLDbIDs(policyNamespace, policyName, string(knet.PolicyTypeEgress),
					0, 0, 0),
			},
			{
				before: libovsdbops.BuildACL(
					policyNamespace+"_"+policyName+"_0",
					nbdb.ACLDirectionToLport,
					types.DefaultAllowPriority,
					"ip4.dst == 10.244.1.5/32 && inport == @a2653181086423119552",
					nbdb.ACLActionAllowRelated,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						l4MatchACLExtIdKey:     "tcp && 6380<=tcp.dst<=7000",
						ipBlockCIDRACLExtIdKey: "2",
						namespaceACLExtIdKey:   policyNamespace,
						policyACLExtIdKey:      policyName,
						policyTypeACLExtIdKey:  string(knet.PolicyTypeEgress),
						fmt.Sprintf(policyTypeNumACLExtIdKey, knet.PolicyTypeEgress): "0",
					},
					nil,
				),
				after: syncerToBuildData.getNetpolGressACLDbIDs(policyNamespace, policyName, string(knet.PolicyTypeEgress),
					0, 0, 1),
			},
			{
				before: libovsdbops.BuildACL(
					policyNamespace+"_"+policyName+"_0",
					nbdb.ACLDirectionToLport,
					types.DefaultAllowPriority,
					"ip4.dst == 10.244.1.5/32 && inport == @a2653181086423119552",
					nbdb.ACLActionAllowRelated,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						l4MatchACLExtIdKey:     "tcp && 1<=tcp.dst<=3",
						ipBlockCIDRACLExtIdKey: "true",
						namespaceACLExtIdKey:   policyNamespace,
						policyACLExtIdKey:      policyName,
						policyTypeACLExtIdKey:  string(knet.PolicyTypeEgress),
						fmt.Sprintf(policyTypeNumACLExtIdKey, knet.PolicyTypeEgress): "0",
					},
					nil,
				),
				after: syncerToBuildData.getNetpolGressACLDbIDs(policyNamespace, policyName, string(knet.PolicyTypeEgress),
					0, 1, 0),
			},
			{
				before: libovsdbops.BuildACL(
					policyNamespace+"_"+policyName+"_0",
					nbdb.ACLDirectionToLport,
					types.DefaultAllowPriority,
					"ip4.dst == 10.244.1.5/32 && inport == @a2653181086423119552",
					nbdb.ACLActionAllowRelated,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						l4MatchACLExtIdKey:     "tcp && 1<=tcp.dst<=3",
						ipBlockCIDRACLExtIdKey: "2",
						namespaceACLExtIdKey:   policyNamespace,
						policyACLExtIdKey:      policyName,
						policyTypeACLExtIdKey:  string(knet.PolicyTypeEgress),
						fmt.Sprintf(policyTypeNumACLExtIdKey, knet.PolicyTypeEgress): "0",
					},
					nil,
				),
				after: syncerToBuildData.getNetpolGressACLDbIDs(policyNamespace, policyName, string(knet.PolicyTypeEgress),
					0, 1, 1),
			},
			{
				before: libovsdbops.BuildACL(
					policyNamespace+"_"+policyName+"_0",
					nbdb.ACLDirectionFromLport,
					types.DefaultAllowPriority,
					"(ip4.src == {$a3733136965153973077} || (ip4.src == 169.254.169.5 && ip4.dst == {$a3733136965153973077})) && outport == @a2653181086423119552",
					nbdb.ACLActionAllowRelated,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						l4MatchACLExtIdKey:     noneMatch,
						ipBlockCIDRACLExtIdKey: "false",
						namespaceACLExtIdKey:   policyNamespace,
						policyACLExtIdKey:      policyName,
						policyTypeACLExtIdKey:  string(knet.PolicyTypeIngress),
						fmt.Sprintf(policyTypeNumACLExtIdKey, knet.PolicyTypeIngress): "0",
					},
					nil,
				),
				after: syncerToBuildData.getNetpolGressACLDbIDs(policyNamespace, policyName, string(knet.PolicyTypeIngress),
					0, emptyIdx, emptyIdx),
			},
		}
		testSyncerWithData(testData, controllerName, []libovsdbtest.TestData{}, nil)
	})
})

package adminnetworkpolicy

import (
	"fmt"
	"testing"

	"github.com/onsi/gomega"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilpointer "k8s.io/utils/pointer"
	"sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

func TestAdminNetworkPolicyRepair(t *testing.T) {
	tests := []struct {
		name       string
		anps       v1alpha1.AdminNetworkPolicyList
		initialDb  []libovsdbtest.TestData
		expectedDb []libovsdbtest.TestData
	}{
		{
			name: "repair stale portgroups",
			anps: v1alpha1.AdminNetworkPolicyList{
				Items: []v1alpha1.AdminNetworkPolicy{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "AegonTargaryen",
							Labels: map[string]string{"house": "targaryen"},
						},
						Spec: v1alpha1.AdminNetworkPolicySpec{
							Priority: 5,
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "DaenerysTargaryen",
							Labels: map[string]string{"house": "targaryen"},
						},
						Spec: v1alpha1.AdminNetworkPolicySpec{
							Priority: 8,
						},
					},
				},
			},
			initialDb: []libovsdbtest.TestData{
				portGroup("AegonTargaryen", nil, nil, false),
				portGroup("DaenerysTargaryen", nil, nil, false),
				portGroup("RoadRunner", nil, nil, false), // stalePG
			},
			expectedDb: []libovsdbtest.TestData{
				portGroup("AegonTargaryen", nil, nil, false),
				portGroup("DaenerysTargaryen", nil, nil, false),
			},
		},
		{
			name: "repair stale portgroups along with acls and ports only if it belongs to ANP controller",
			anps: v1alpha1.AdminNetworkPolicyList{
				Items: []v1alpha1.AdminNetworkPolicy{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "AegonTargaryen",
							Labels: map[string]string{"house": "targaryen"},
						},
						Spec: v1alpha1.AdminNetworkPolicySpec{
							Priority: 5,
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "DaenerysTargaryen",
							Labels: map[string]string{"house": "targaryen"},
						},
						Spec: v1alpha1.AdminNetworkPolicySpec{
							Priority: 8,
						},
					},
				},
			},
			initialDb: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Ports: []string{"sansa", "stark", "silvester", "tweety"},
				},
				&nbdb.LogicalSwitchPort{UUID: "sansa", Name: "sansa"},
				&nbdb.LogicalSwitchPort{UUID: "stark", Name: "stark"},
				&nbdb.LogicalSwitchPort{UUID: "silvester", Name: "silvester"},
				&nbdb.LogicalSwitchPort{UUID: "tweety", Name: "tweety"},
				portGroup("AegonTargaryen", []*nbdb.LogicalSwitchPort{{UUID: "sansa"}, {UUID: "stark"}}, nil, false),
				accessControlList("arya", libovsdbutil.ACLEgress, 3, false),
				accessControlList("stark", libovsdbutil.ACLEgress, 29200, false),
				portGroup("DaenerysTargaryen",
					nil,
					[]*nbdb.ACL{
						accessControlList("arya", libovsdbutil.ACLEgress, 3, false),
						accessControlList("stark", libovsdbutil.ACLEgress, 29200, false),
					},
					false),
				accessControlList("tom", libovsdbutil.ACLEgress, 3, false),
				accessControlList("jerry", libovsdbutil.ACLEgress, 3, false),
				portGroup("RoadRunner",
					[]*nbdb.LogicalSwitchPort{{UUID: "silvester"}, {UUID: "tweety"}},
					[]*nbdb.ACL{
						accessControlList("tom", libovsdbutil.ACLEgress, 3, false),
						accessControlList("jerry", libovsdbutil.ACLEgress, 3, false),
					},
					false), // stalePG
				// "RoadRunner1" PG doesn't have any externalIDs that match ANP controller's, so ignored
				accessControlList("tom1", libovsdbutil.ACLEgress, 3, false),
				accessControlList("jerry1", libovsdbutil.ACLEgress, 3, false),
				stalePGWithoutExtIDs("RoadRunner1",
					[]*nbdb.LogicalSwitchPort{{UUID: "silvester"}, {UUID: "tweety"}},
					[]*nbdb.ACL{
						accessControlList("tom1", libovsdbutil.ACLEgress, 3, false),
						accessControlList("jerry1", libovsdbutil.ACLEgress, 3, false),
					},
					false), // stalePG
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Ports: []string{"sansa", "stark", "silvester", "tweety"},
				},
				&nbdb.LogicalSwitchPort{UUID: "sansa", Name: "sansa"},
				&nbdb.LogicalSwitchPort{UUID: "stark", Name: "stark"},
				&nbdb.LogicalSwitchPort{UUID: "silvester", Name: "silvester"},
				&nbdb.LogicalSwitchPort{UUID: "tweety", Name: "tweety"},
				portGroup("AegonTargaryen", []*nbdb.LogicalSwitchPort{{UUID: "sansa"}, {UUID: "stark"}}, nil, false),
				accessControlList("arya", libovsdbutil.ACLEgress, 3, false),
				accessControlList("stark", libovsdbutil.ACLEgress, 29200, false),
				portGroup("DaenerysTargaryen",
					nil,
					[]*nbdb.ACL{
						accessControlList("arya", libovsdbutil.ACLEgress, 3, false),
						accessControlList("stark", libovsdbutil.ACLEgress, 29200, false),
					},
					false),
				// "RoadRunner1" PG doesn't have any externalIDs that match ANP controller's, so ignored
				accessControlList("tom1", libovsdbutil.ACLEgress, 3, false),
				accessControlList("jerry1", libovsdbutil.ACLEgress, 3, false),
				stalePGWithoutExtIDs("RoadRunner1",
					[]*nbdb.LogicalSwitchPort{{UUID: "silvester"}, {UUID: "tweety"}},
					[]*nbdb.ACL{
						accessControlList("tom1", libovsdbutil.ACLEgress, 3, false),
						accessControlList("jerry1", libovsdbutil.ACLEgress, 3, false),
					},
					false), // stalePG
			},
		},
		{
			name: "repair stale address-sets",
			anps: v1alpha1.AdminNetworkPolicyList{
				Items: []v1alpha1.AdminNetworkPolicy{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "AegonTargaryen",
							Labels: map[string]string{"house": "targaryen"},
						},
						Spec: v1alpha1.AdminNetworkPolicySpec{
							Priority: 5,
						},
					},
				},
			},
			initialDb: []libovsdbtest.TestData{
				addressSet("RoadRunner", string(libovsdbutil.ACLEgress), 29500, false), // staleAS because no matching ANP is present in the cluster
			},
			expectedDb: []libovsdbtest.TestData{},
		},
	}

	for i, tt := range tests {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)
			controller, err := newANPControllerWithDBSetup(libovsdbtest.TestSetup{NBData: tt.initialDb}, tt.anps, v1alpha1.BaselineAdminNetworkPolicyList{})
			if err != nil {
				t.Fatalf("Error creating ANP controller: %v", err)
			}
			err = controller.repairAdminNetworkPolicies()
			if err != nil {
				t.Fatalf("repairAdminNetworkPolicies error: %v", err)
			}
			g.Expect(controller.nbClient).To(libovsdbtest.HaveDataIgnoringUUIDs(tt.expectedDb))
		})
	}

}

func TestBaselineAdminNetworkPolicyRepair(t *testing.T) {
	tests := []struct {
		name       string
		banps      v1alpha1.BaselineAdminNetworkPolicyList
		initialDb  []libovsdbtest.TestData
		expectedDb []libovsdbtest.TestData
	}{
		{
			name: "repair stale portgroups",
			banps: v1alpha1.BaselineAdminNetworkPolicyList{
				Items: []v1alpha1.BaselineAdminNetworkPolicy{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "AegonTargaryen",
							Labels: map[string]string{"house": "targaryen"},
						},
						Spec: v1alpha1.BaselineAdminNetworkPolicySpec{},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "DaenerysTargaryen",
							Labels: map[string]string{"house": "targaryen"},
						},
						Spec: v1alpha1.BaselineAdminNetworkPolicySpec{},
					},
				},
			},
			initialDb: []libovsdbtest.TestData{
				portGroup("AegonTargaryen", nil, nil, true),
				portGroup("DaenerysTargaryen", nil, nil, true),
				portGroup("RoadRunner", nil, nil, true), // stalePG
			},
			expectedDb: []libovsdbtest.TestData{
				portGroup("AegonTargaryen", nil, nil, true),
				portGroup("DaenerysTargaryen", nil, nil, true),
			},
		},
		{
			name: "repair stale portgroups along with acls and ports only if it belongs to ANP controller",
			banps: v1alpha1.BaselineAdminNetworkPolicyList{
				Items: []v1alpha1.BaselineAdminNetworkPolicy{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "AegonTargaryen",
							Labels: map[string]string{"house": "targaryen"},
						},
						Spec: v1alpha1.BaselineAdminNetworkPolicySpec{},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "DaenerysTargaryen",
							Labels: map[string]string{"house": "targaryen"},
						},
						Spec: v1alpha1.BaselineAdminNetworkPolicySpec{},
					},
				},
			},
			initialDb: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Ports: []string{"sansa", "stark", "silvester", "tweety"},
				},
				&nbdb.LogicalSwitchPort{UUID: "sansa", Name: "sansa"},
				&nbdb.LogicalSwitchPort{UUID: "stark", Name: "stark"},
				&nbdb.LogicalSwitchPort{UUID: "silvester", Name: "silvester"},
				&nbdb.LogicalSwitchPort{UUID: "tweety", Name: "tweety"},
				portGroup("AegonTargaryen", []*nbdb.LogicalSwitchPort{{UUID: "sansa"}, {UUID: "stark"}}, nil, true),
				accessControlList("arya", libovsdbutil.ACLEgress, 3, true),
				accessControlList("stark", libovsdbutil.ACLEgress, 29200, true),
				portGroup("DaenerysTargaryen",
					nil,
					[]*nbdb.ACL{
						accessControlList("arya", libovsdbutil.ACLEgress, 3, true),
						accessControlList("stark", libovsdbutil.ACLEgress, 29200, true),
					},
					true),
				accessControlList("tom", libovsdbutil.ACLEgress, 3, true),
				accessControlList("jerry", libovsdbutil.ACLEgress, 3, true),
				portGroup("RoadRunner",
					[]*nbdb.LogicalSwitchPort{{UUID: "silvester"}, {UUID: "tweety"}},
					[]*nbdb.ACL{
						accessControlList("tom", libovsdbutil.ACLEgress, 3, true),
						accessControlList("jerry", libovsdbutil.ACLEgress, 3, true),
					},
					true), // stalePG
				// "RoadRunner1" PG doesn't have any externalIDs that match ANP controller's, so ignored
				accessControlList("tom1", libovsdbutil.ACLEgress, 3, true),
				accessControlList("jerry1", libovsdbutil.ACLEgress, 3, true),
				stalePGWithoutExtIDs("RoadRunner1",
					[]*nbdb.LogicalSwitchPort{{UUID: "silvester"}, {UUID: "tweety"}},
					[]*nbdb.ACL{
						accessControlList("tom1", libovsdbutil.ACLEgress, 3, true),
						accessControlList("jerry1", libovsdbutil.ACLEgress, 3, true),
					},
					true), // stalePG
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Ports: []string{"sansa", "stark", "silvester", "tweety"},
				},
				&nbdb.LogicalSwitchPort{UUID: "sansa", Name: "sansa"},
				&nbdb.LogicalSwitchPort{UUID: "stark", Name: "stark"},
				&nbdb.LogicalSwitchPort{UUID: "silvester", Name: "silvester"},
				&nbdb.LogicalSwitchPort{UUID: "tweety", Name: "tweety"},
				portGroup("AegonTargaryen", []*nbdb.LogicalSwitchPort{{UUID: "sansa"}, {UUID: "stark"}}, nil, true),
				accessControlList("arya", libovsdbutil.ACLEgress, 3, true),
				accessControlList("stark", libovsdbutil.ACLEgress, 29200, true),
				portGroup("DaenerysTargaryen",
					nil,
					[]*nbdb.ACL{
						accessControlList("arya", libovsdbutil.ACLEgress, 3, true),
						accessControlList("stark", libovsdbutil.ACLEgress, 29200, true),
					},
					true),
				// "RoadRunner1" PG doesn't have any externalIDs that match ANP controller's, so ignored
				accessControlList("tom1", libovsdbutil.ACLEgress, 3, true),
				accessControlList("jerry1", libovsdbutil.ACLEgress, 3, true),
				stalePGWithoutExtIDs("RoadRunner1",
					[]*nbdb.LogicalSwitchPort{{UUID: "silvester"}, {UUID: "tweety"}},
					[]*nbdb.ACL{
						accessControlList("tom1", libovsdbutil.ACLEgress, 3, true),
						accessControlList("jerry1", libovsdbutil.ACLEgress, 3, true),
					},
					true), // stalePG
			},
		},
		{
			name: "repair stale address-sets",
			banps: v1alpha1.BaselineAdminNetworkPolicyList{
				Items: []v1alpha1.BaselineAdminNetworkPolicy{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "AegonTargaryen",
							Labels: map[string]string{"house": "targaryen"},
						},
						Spec: v1alpha1.BaselineAdminNetworkPolicySpec{},
					},
				},
			},
			initialDb: []libovsdbtest.TestData{
				addressSet("RoadRunner", string(libovsdbutil.ACLEgress), BANPFlowPriority, true), // staleAS because no matching ANP is present in the cluster
			},
			expectedDb: []libovsdbtest.TestData{},
		},
	}

	for i, tt := range tests {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)
			controller, err := newANPControllerWithDBSetup(libovsdbtest.TestSetup{NBData: tt.initialDb}, v1alpha1.AdminNetworkPolicyList{}, tt.banps)
			if err != nil {
				t.Fatalf("Error creating ANP controller: %v", err)
			}
			err = controller.repairBaselineAdminNetworkPolicy()
			if err != nil {
				t.Fatalf("repairBaselineAdminNetworkPolicy error: %v", err)
			}
			g.Expect(controller.nbClient).To(libovsdbtest.HaveDataIgnoringUUIDs(tt.expectedDb))
		})
	}

}

func portGroup(name string, ports []*nbdb.LogicalSwitchPort, acls []*nbdb.ACL, banp bool) *nbdb.PortGroup {
	pgDbIDs := GetANPPortGroupDbIDs(name, banp, defaultNetworkControllerName)
	pg := libovsdbutil.BuildPortGroup(pgDbIDs, ports, acls)
	pg.UUID = pgDbIDs.String() + "-UUID"
	return pg
}

func stalePGWithoutExtIDs(name string, ports []*nbdb.LogicalSwitchPort, acls []*nbdb.ACL, banp bool) *nbdb.PortGroup {
	pg := portGroup(name, ports, acls, banp)
	pg.ExternalIDs = nil
	return pg
}

func accessControlList(name string, gressPrefix libovsdbutil.ACLDirection, priority int32, banp bool) *nbdb.ACL {
	objIDs := getANPRuleACLDbIDs(name, string(gressPrefix), fmt.Sprintf("%d", priority), "None",
		defaultNetworkControllerName, banp)
	acl := &nbdb.ACL{
		UUID:        objIDs.String() + "-UUID",
		Action:      nbdb.ACLActionAllow,
		Direction:   nbdb.ACLDirectionToLport,
		ExternalIDs: objIDs.GetExternalIDs(),
		Log:         true,
		Match:       "match",
		Name:        utilpointer.String(name),
		Options:     map[string]string{"key": "value"},
		Priority:    int(priority),
		Tier:        1,
	}
	return acl
}

func addressSet(name, gressPrefix string, priority int32, banp bool) *nbdb.AddressSet {
	objIDs := GetANPPeerAddrSetDbIDs(name, gressPrefix, fmt.Sprintf("%d", priority),
		defaultNetworkControllerName, banp)
	dbIDsWithIPFam := objIDs.AddIDs(map[libovsdbops.ExternalIDKey]string{libovsdbops.IPFamilyKey: "ipv4"})
	as := &nbdb.AddressSet{
		UUID:        dbIDsWithIPFam.String() + "-UUID",
		ExternalIDs: dbIDsWithIPFam.GetExternalIDs(),
		Name:        "blah",
		Addresses:   []string{},
	}
	return as
}

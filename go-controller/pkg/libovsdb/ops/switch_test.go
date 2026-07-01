// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/onsi/gomega"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestFindLogicalSwitchPortsByNamespaceAndNetwork(t *testing.T) {
	g := gomega.NewWithT(t)
	ports := []*nbdb.LogicalSwitchPort{
		{UUID: "a", Name: "a", ExternalIDs: map[string]string{"namespace": "ns-a", ovntypes.NetworkExternalID: "net-a"}},
		{UUID: "b", Name: "b", ExternalIDs: map[string]string{"namespace": "ns-a", ovntypes.NetworkExternalID: "net-a"}},
		{UUID: "c", Name: "c", ExternalIDs: map[string]string{"namespace": "ns-a", ovntypes.NetworkExternalID: "net-b"}},
		{UUID: "d", Name: "d", ExternalIDs: map[string]string{"namespace": "ns-b", ovntypes.NetworkExternalID: "net-a"}},
		{UUID: "unowned", Name: "unowned"},
	}
	sw := &nbdb.LogicalSwitch{UUID: "sw", Name: "sw"}
	data := []libovsdbtest.TestData{sw}
	for _, port := range ports {
		sw.Ports = append(sw.Ports, port.UUID)
		data = append(data, port)
	}
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup.Cleanup)
	check := func(namespace, network string, want []string) {
		t.Helper()
		g.Eventually(func() ([]string, error) {
			found, err := FindLogicalSwitchPortsByNamespaceAndNetwork(nbClient, namespace, network)
			names := make([]string, 0, len(found))
			for _, port := range found {
				names = append(names, port.Name)
			}
			return names, err
		}).Should(gomega.ConsistOf(want), "namespace %s network %s", namespace, network)
	}
	check("ns-a", "net-a", []string{"a", "b"})
	check("ns-a", "net-b", []string{"c"})
	check("ns-b", "net-a", []string{"d"})
	check("missing", "net-a", nil)
	check("ns-a", "missing", nil)

	// Index maintenance must track metadata changes and port deletion.
	updated := ports[1].DeepCopy()
	updated.UUID = ""
	updated.ExternalIDs[ovntypes.NetworkExternalID] = "net-b"
	m := newModelClient(nbClient)
	if _, err := m.CreateOrUpdate(operationModel{
		Model: updated, OnModelUpdates: []interface{}{&updated.ExternalIDs}, ErrNotFound: true,
	}); err != nil {
		t.Fatal(err)
	}
	check("ns-a", "net-a", []string{"a"})
	check("ns-a", "net-b", []string{"b", "c"})
	if err := DeleteLogicalSwitchPorts(nbClient, &nbdb.LogicalSwitch{Name: sw.Name}, &nbdb.LogicalSwitchPort{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	check("ns-a", "net-a", nil)

	// An absent name must not fall back to the empty external-ID bucket.
	if _, err := GetLogicalSwitchPort(nbClient, &nbdb.LogicalSwitchPort{Name: "new-port"}); !errors.Is(err, libovsdbclient.ErrNotFound) {
		t.Fatalf("missing port lookup returned %v", err)
	}
	newPort := &nbdb.LogicalSwitchPort{Name: "new-port", ExternalIDs: ports[2].ExternalIDs}
	if err := CreateOrUpdateLogicalSwitchPortsOnSwitch(nbClient, &nbdb.LogicalSwitch{Name: sw.Name}, newPort); err != nil {
		t.Fatal(err)
	}
	check("ns-a", "net-b", []string{"b", "c", "new-port"})
	if _, err := GetLogicalSwitchPort(nbClient, &nbdb.LogicalSwitchPort{Name: "unowned"}); err != nil {
		t.Fatalf("creating a port changed an unrelated port: %v", err)
	}
}

func TestLogicalSwitchPortOperationsDoNotFallBackToClientIndex(t *testing.T) {
	port := &nbdb.LogicalSwitchPort{
		UUID: "port", Name: "port", ExternalIDs: map[string]string{"namespace": "ns", ovntypes.NetworkExternalID: "net"},
	}
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
		&nbdb.LogicalSwitch{UUID: "sw", Name: "sw", Ports: []string{port.UUID}}, port,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup.Cleanup)
	m := newModelClient(nbClient)
	stale := port.DeepCopy()
	stale.UUID = "00000000-0000-0000-0000-000000000001"
	stale.Name = "deleted-port"
	stale.Addresses = []string{"dynamic"}
	for _, operation := range []string{"update", "mutate", "delete"} {
		t.Run(operation, func(t *testing.T) {
			var ops []ovsdb.Operation
			var err error
			switch operation {
			case "update":
				ops, err = m.update(stale, &operationModel{Model: stale, OnModelUpdates: []interface{}{&stale.Addresses}})
			case "mutate":
				ops, err = m.mutate(stale, &operationModel{Model: stale, OnModelMutations: []interface{}{&stale.Addresses}}, ovsdb.MutateOperationInsert)
			case "delete":
				ops, err = m.delete(stale)
			}
			if err != nil {
				t.Fatal(err)
			}
			results, err := TransactAndCheck(nbClient, ops)
			if err != nil {
				t.Fatal(err)
			}
			if len(ops) != 1 || len(results) != 1 || results[0].Count != 0 {
				t.Fatalf("operation on missing UUID must affect zero rows: ops=%+v results=%+v", ops, results)
			}
			found, err := GetLogicalSwitchPort(nbClient, &nbdb.LogicalSwitchPort{Name: port.Name})
			if err != nil || len(found.Addresses) != 0 {
				t.Fatalf("operation on missing UUID affected another port: %+v, %v", found, err)
			}
		})
	}
}

func BenchmarkFindLogicalSwitchPortsByNamespaceAndNetwork(b *testing.B) {
	for _, count := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("ports=%d", count), func(b *testing.B) {
			sw := &nbdb.LogicalSwitch{UUID: "sw", Name: "sw"}
			data := []libovsdbtest.TestData{sw}
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("port-%d", i)
				sw.Ports = append(sw.Ports, name)
				data = append(data, &nbdb.LogicalSwitchPort{UUID: name, Name: name, ExternalIDs: map[string]string{
					"namespace": fmt.Sprintf("ns-%d", i/2), ovntypes.NetworkExternalID: fmt.Sprintf("net-%d", i/2),
				}})
			}
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(cleanup.Cleanup)
			for _, indexed := range []bool{false, true} {
				b.Run(fmt.Sprintf("indexed=%t", indexed), func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						var ports []*nbdb.LogicalSwitchPort
						var err error
						if indexed {
							ports, err = FindLogicalSwitchPortsByNamespaceAndNetwork(nbClient, "ns-0", "other-network")
						} else {
							ports, err = FindLogicalSwitchPortWithPredicate(nbClient, func(p *nbdb.LogicalSwitchPort) bool {
								return p.ExternalIDs["namespace"] == "ns-0" && p.ExternalIDs[ovntypes.NetworkExternalID] == "other-network"
							})
						}
						if err != nil || len(ports) != 0 {
							b.Fatalf("unexpected ports: %v, %v", ports, err)
						}
					}
				})
			}
		})
	}
}

func TestRemoveACLsFromSwitches(t *testing.T) {
	fakeACL1 := nbdb.ACL{
		UUID: buildNamedUUID(),
	}

	fakeACL2 := nbdb.ACL{
		UUID: buildNamedUUID(),
	}

	fakeSwitch1 := nbdb.LogicalSwitch{
		Name: "sw1",
		UUID: buildNamedUUID(),
		ACLs: []string{fakeACL1.UUID},
	}

	fakeSwitch2 := nbdb.LogicalSwitch{
		Name: "sw2",
		UUID: buildNamedUUID(),
		ACLs: []string{fakeACL1.UUID, fakeACL2.UUID},
	}

	// Add switch without ACL to ensure the delete function
	// can handle this case
	fakeSwitch3 := nbdb.LogicalSwitch{
		Name: "sw3",
		UUID: buildNamedUUID(),
	}

	tests := []struct {
		desc         string
		expectErr    bool
		initialNbdb  libovsdbtest.TestSetup
		expectedNbdb libovsdbtest.TestSetup
	}{
		{
			desc:      "remove acl on two switches",
			expectErr: false,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeSwitch1.DeepCopy(),
					fakeSwitch2.DeepCopy(),
					fakeSwitch3.DeepCopy(),
					fakeACL1.DeepCopy(),
					fakeACL2.DeepCopy(),
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						Name: "sw1",
						UUID: fakeSwitch1.UUID,
					},
					&nbdb.LogicalSwitch{
						Name: "sw2",
						UUID: fakeSwitch2.UUID,
					},
					fakeSwitch3.DeepCopy(),
				},
			},
		},
		{
			desc:      "remove acl on no switches",
			expectErr: true,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeSwitch3.DeepCopy(),
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeSwitch3.DeepCopy(),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(tt.initialNbdb, nil)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			var ACLs []*nbdb.ACL
			err = nbClient.List(context.Background(), &ACLs)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to list ACLs: %v", tt.desc, err)
			}
			if len(ACLs) == 0 {
				ACLs = []*nbdb.ACL{
					&fakeACL1,
					&fakeACL2,
				}
			}

			p := func(*nbdb.LogicalSwitch) bool { return true }
			err = RemoveACLsFromLogicalSwitchesWithPredicate(nbClient, p, ACLs...)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("RemoveACLFromNodeSwitches() error = %v", err))
			}

			matcher := libovsdbtest.HaveData(tt.expectedNbdb.NBData)
			success, err := matcher.Match(nbClient)

			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(nbClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}

func TestDeleteSwitchesWithPredicateOps(t *testing.T) {
	fakeSwitch1 := nbdb.LogicalSwitch{
		Name:        "sw1",
		UUID:        buildNamedUUID(),
		ExternalIDs: map[string]string{"key": "a"},
	}

	fakeSwitch2 := nbdb.LogicalSwitch{
		Name:        "sw2",
		UUID:        buildNamedUUID(),
		ExternalIDs: map[string]string{"key": "a"},
	}

	fakeSwitch3 := nbdb.LogicalSwitch{
		Name:        "sw3",
		UUID:        buildNamedUUID(),
		ExternalIDs: map[string]string{"key": "b"},
	}

	tests := []struct {
		desc         string
		expectErr    bool
		initialNbdb  libovsdbtest.TestSetup
		expectedNbdb libovsdbtest.TestSetup
		p            logicalSwitchPredicate
	}{
		{
			desc:      "remove switches of specified external_id key",
			expectErr: false,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeSwitch1.DeepCopy(),
					fakeSwitch2.DeepCopy(),
					fakeSwitch3.DeepCopy(),
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeSwitch3.DeepCopy(),
				},
			},
			p: func(item *nbdb.LogicalSwitch) bool { return item.ExternalIDs["key"] == "a" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(tt.initialNbdb, nil)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			ops, err := DeleteLogicalSwitchesWithPredicateOps(nbClient, nil, tt.p)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("DeleteLogicalSwitchesWithPredicateOps() error = %v", err))
			}

			_, err = TransactAndCheck(nbClient, ops)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("TransactAndCheck() error = %v", err))
			}

			matcher := libovsdbtest.HaveData(tt.expectedNbdb.NBData)
			success, err := matcher.Match(nbClient)

			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(nbClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}

package ops

import (
	"fmt"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

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

			ACLs := []*nbdb.ACL{
				&fakeACL1,
				&fakeACL2,
			}

			p := func(item *nbdb.LogicalSwitch) bool { return true }
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

package libovsdbops

import (
	"fmt"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestRemoveACLsFromSwitches(t *testing.T) {
	fakeACL1 := &nbdb.ACL{
		UUID: BuildNamedUUID(),
	}

	fakeACL2 := &nbdb.ACL{
		UUID: BuildNamedUUID(),
	}

	fakeSwitch1 := &nbdb.LogicalSwitch{
		Name: "sw1",
		UUID: BuildNamedUUID(),
		ACLs: []string{fakeACL1.UUID},
	}

	fakeSwitch2 := &nbdb.LogicalSwitch{
		Name: "sw2",
		UUID: BuildNamedUUID(),
		ACLs: []string{fakeACL1.UUID, fakeACL2.UUID},
	}

	// Add switch without ACL to ensure the delete function
	// can handle this case
	fakeSwitch3 := &nbdb.LogicalSwitch{
		Name: "sw3",
		UUID: BuildNamedUUID(),
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
					fakeSwitch1,
					fakeSwitch2,
					fakeSwitch3,
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
					fakeSwitch3,
				},
			},
		},
		{
			desc:      "remove acl on no switches",
			expectErr: true,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeSwitch3,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeSwitch3,
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

			fakeSwitches := []nbdb.LogicalSwitch{
				*fakeSwitch1,
				*fakeSwitch2,
				*fakeSwitch3,
			}

			ACLs := []*nbdb.ACL{
				fakeACL1,
				fakeACL2,
			}

			err = removeACLsFromSwitches(nbClient, fakeSwitches, ACLs...)
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

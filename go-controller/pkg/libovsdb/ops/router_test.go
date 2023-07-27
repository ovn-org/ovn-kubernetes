package ops

import (
	"fmt"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestFindNATsUsingPredicate(t *testing.T) {
	fakeNAT1 := &nbdb.NAT{
		UUID: buildNamedUUID(),
		Type: nbdb.NATTypeSNAT,
	}

	fakeNAT2 := &nbdb.NAT{
		UUID:        buildNamedUUID(),
		ExternalIDs: map[string]string{"name": "fakeNAT2"},
	}

	initialNbdb := libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			fakeNAT1,
			fakeNAT2,
		},
	}

	tests := []struct {
		desc       string
		predFunc   func(item *nbdb.NAT) bool
		expectedRc []*nbdb.NAT
	}{
		{
			desc: "find no nats",
			predFunc: func(item *nbdb.NAT) bool {
				return false
			},
			expectedRc: []*nbdb.NAT{},
		},
		{
			desc: "find all nats",
			predFunc: func(item *nbdb.NAT) bool {
				return true
			},
			expectedRc: []*nbdb.NAT{fakeNAT1, fakeNAT2},
		},
		{
			desc: "find nat2",
			predFunc: func(item *nbdb.NAT) bool {
				name, _ := item.ExternalIDs["name"]
				return name == "fakeNAT2"
			},
			expectedRc: []*nbdb.NAT{fakeNAT2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			rc, err := FindNATsWithPredicate(nbClient, tt.predFunc)
			if err != nil {
				t.Fatal(fmt.Errorf("FindNATsUsingPredicate() error = %v", err))
			}

			if len(rc) != len(tt.expectedRc) {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match len expected %v with actual: %v", tt.desc, tt.expectedRc, rc))
			}

			var foundMatch bool
			for _, nat := range tt.expectedRc {
				foundMatch = false
				for _, rcNat := range rc {
					if isEquivalentNAT(rcNat, nat) {
						foundMatch = true
						break
					}
				}
				if !foundMatch {
					t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected nat %v", tt.desc, nat))

				}
			}
		})
	}
}

func TestDeleteNATsFromRouter(t *testing.T) {
	fakeNAT1 := &nbdb.NAT{
		UUID:       buildNamedUUID(),
		ExternalIP: "192.168.1.110",
		Type:       nbdb.NATTypeSNAT,
	}

	fakeNAT2 := &nbdb.NAT{
		UUID:       buildNamedUUID(),
		ExternalIP: "192.168.1.110",
		Type:       nbdb.NATTypeDNATAndSNAT,
	}

	fakeNAT3 := &nbdb.NAT{
		UUID:        buildNamedUUID(),
		ExternalIP:  "192.168.1.111",
		Type:        nbdb.NATTypeSNAT,
		ExternalIDs: map[string]string{"name": "fakeNAT3"},
	}

	fakeNAT4 := &nbdb.NAT{
		UUID:        buildNamedUUID(),
		ExternalIP:  "192.168.1.112",
		Type:        nbdb.NATTypeSNAT,
		ExternalIDs: map[string]string{"name": "fakeNAT4"},
	}

	fakeRouter1 := &nbdb.LogicalRouter{
		Name: "rtr1",
		UUID: buildNamedUUID(),
		Nat:  []string{fakeNAT1.UUID},
	}

	fakeRouter2 := &nbdb.LogicalRouter{
		Name: "rtr2",
		UUID: buildNamedUUID(),
		Nat:  []string{fakeNAT2.UUID, fakeNAT3.UUID},
	}

	initialNbdb := libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			fakeNAT1,
			fakeNAT2,
			fakeNAT3,
			fakeRouter1,
			fakeRouter2,
		},
	}

	tests := []struct {
		desc         string
		expectErr    bool
		routerName   string
		nats         []*nbdb.NAT
		expectedNbdb libovsdbtest.TestSetup
	}{
		{
			desc:         "no router",
			expectErr:    true,
			nats:         []*nbdb.NAT{fakeNAT1.DeepCopy(), fakeNAT2.DeepCopy(), fakeNAT3.DeepCopy(), fakeNAT4.DeepCopy()},
			expectedNbdb: initialNbdb,
		},
		{
			desc:         "no deletes: no matching nats",
			routerName:   "rtr1",
			nats:         []*nbdb.NAT{fakeNAT2.DeepCopy(), fakeNAT3.DeepCopy(), fakeNAT4.DeepCopy()},
			expectedNbdb: initialNbdb,
		},
		{
			desc:       "remove nat 2 from router 2",
			routerName: "rtr2",
			nats:       []*nbdb.NAT{fakeNAT2.DeepCopy(), fakeNAT4.DeepCopy()},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeNAT1,
					fakeNAT3,
					fakeRouter1,
					&nbdb.LogicalRouter{
						Name: fakeRouter2.Name,
						UUID: fakeRouter2.UUID,
						Nat:  []string{fakeNAT3.UUID},
					},
				},
			},
		},
		{
			desc:       "remove nats from router2",
			routerName: "rtr2",
			nats:       []*nbdb.NAT{fakeNAT1.DeepCopy(), fakeNAT2.DeepCopy(), fakeNAT3.DeepCopy(), fakeNAT4.DeepCopy()},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeNAT1,
					fakeRouter1,
					&nbdb.LogicalRouter{
						Name: fakeRouter2.Name,
						UUID: fakeRouter2.UUID,
						Nat:  []string{},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			logicalRouter := nbdb.LogicalRouter{
				Name: tt.routerName,
			}
			err = DeleteNATs(nbClient, &logicalRouter, tt.nats...)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("DeleteNATsFromRouter() error = %v", err))
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

func TestDeleteRoutersWithPredicateOps(t *testing.T) {
	fakeRouter1 := nbdb.LogicalRouter{
		Name:        "rtr1",
		UUID:        buildNamedUUID(),
		ExternalIDs: map[string]string{"key": "a"},
	}

	fakeRouter2 := nbdb.LogicalRouter{
		Name:        "rtr2",
		UUID:        buildNamedUUID(),
		ExternalIDs: map[string]string{"key": "a"},
	}

	fakeRouter3 := nbdb.LogicalRouter{
		Name:        "rtr3",
		UUID:        buildNamedUUID(),
		ExternalIDs: map[string]string{"key": "b"},
	}

	tests := []struct {
		desc         string
		expectErr    bool
		initialNbdb  libovsdbtest.TestSetup
		expectedNbdb libovsdbtest.TestSetup
		p            logicalRouterPredicate
	}{
		{
			desc:      "remove routers of specified external_id key",
			expectErr: false,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeRouter1.DeepCopy(),
					fakeRouter2.DeepCopy(),
					fakeRouter3.DeepCopy(),
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeRouter3.DeepCopy(),
				},
			},
			p: func(item *nbdb.LogicalRouter) bool { return item.ExternalIDs["key"] == "a" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(tt.initialNbdb, nil)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			ops, err := DeleteLogicalRoutersWithPredicateOps(nbClient, nil, tt.p)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("DeleteLogicalRoutersWithPredicateOps() error = %v", err))
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

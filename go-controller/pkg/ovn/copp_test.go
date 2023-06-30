package ovn

import (
	"fmt"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TestEnsureDefaultCOPP(t *testing.T) {
	meterMap := make(map[string]string, len(defaultProtocolNames))
	for _, protocol := range defaultProtocolNames {
		// format: <OVNSupportedProtocolName>-rate-limiter
		meterMap[protocol] = getMeterNameForProtocol(protocol)
	}

	meterBand := &nbdb.MeterBand{
		UUID:   "meter-band-UUID",
		Action: types.MeterAction,
		Rate:   int(25), // hard-coding for now. TODO(tssurya): make this configurable if needed
	}

	var meters []*nbdb.Meter
	meterFairness := true
	for i := 0; i < len(defaultProtocolNames); i++ {
		meters = append(meters, &nbdb.Meter{
			UUID:  fmt.Sprintf("meter-%d-UUID", i),
			Name:  getMeterNameForProtocol(defaultProtocolNames[i]),
			Fair:  &meterFairness,
			Unit:  types.PacketsPerSecond,
			Bands: []string{meterBand.UUID},
		})
	}

	expectedNBData := []libovsdbtest.TestData{
		&nbdb.Copp{
			Name:   "ovnkube-default",
			UUID:   "copp-UUID",
			Meters: meterMap,
		},
		meterBand,
	}
	for _, m := range meters {
		expectedNBData = append(expectedNBData, m)
	}

	existingNamedCOPPNBData := []libovsdbtest.TestData{
		&nbdb.Copp{
			UUID:   "copp-UUID",
			Name:   "ovnkube-default",
			Meters: meterMap,
		},
		meterBand,
	}
	for _, m := range meters {
		existingNamedCOPPNBData = append(existingNamedCOPPNBData, m)
	}

	multipleEmptyCOPPNameNBData := []libovsdbtest.TestData{
		&nbdb.Copp{
			UUID:   "copp-UUID-1",
			Meters: meterMap,
		},
		&nbdb.Copp{
			UUID:   "copp-UUID-2",
			Meters: meterMap,
		},
		&nbdb.Copp{
			UUID:   "copp-UUID-3",
			Meters: meterMap,
		},
		meterBand,
	}
	for _, m := range meters {
		multipleEmptyCOPPNameNBData = append(multipleEmptyCOPPNameNBData, m)
	}

	multipleEmptyAndNamedCOPPNBData := []libovsdbtest.TestData{
		&nbdb.Copp{
			Name:   "ovnkube-default",
			UUID:   "copp-UUID-1",
			Meters: meterMap,
		},
		&nbdb.Copp{
			UUID:   "copp-UUID-2",
			Meters: meterMap,
		},
		&nbdb.Copp{
			UUID:   "copp-UUID-3",
			Meters: meterMap,
		},
		&nbdb.Copp{
			UUID:   "copp-UUID-4",
			Meters: meterMap,
		},
		meterBand,
	}
	for _, m := range meters {
		multipleEmptyAndNamedCOPPNBData = append(multipleEmptyAndNamedCOPPNBData, m)
	}

	tests := []struct {
		desc         string
		expectErr    bool
		initialNbdb  libovsdbtest.TestSetup
		expectedNbdb libovsdbtest.TestSetup
	}{
		{
			desc:      "no existing COPP",
			expectErr: false,
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: expectedNBData,
			},
		},
		{
			desc:      "updates existing named COPP",
			expectErr: false,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: existingNamedCOPPNBData,
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: expectedNBData,
			},
		},
		{
			desc:      "cleans multiple empty-name default COPPs and adds named COPP",
			expectErr: false,
			initialNbdb: libovsdbtest.TestSetup{
				IgnoreConstraints: true,
				NBData:            multipleEmptyCOPPNameNBData,
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: expectedNBData,
			},
		},
		{
			desc:      "cleans multiple empty-name default COPPs when named COPP exists",
			expectErr: false,
			initialNbdb: libovsdbtest.TestSetup{
				IgnoreConstraints: true,
				NBData:            multipleEmptyAndNamedCOPPNBData,
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: expectedNBData,
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

			_, err = EnsureDefaultCOPP(nbClient)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("EnsureDefaultCOPP() error = %v", err))
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

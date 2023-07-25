package ops

import (
	"fmt"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestCreateMeterBandOps(t *testing.T) {
	firstUUID := "b9998337-2498-4d1e-86e6-fc0417abb2f0"
	secondUUID := "b9998337-2498-4d1e-86e6-fc0417abb2f1"
	thirdUUID := "b9998337-2498-4d1e-86e6-fc0417abb2f2"
	tests := []struct {
		desc              string
		inputMeterBand    *nbdb.MeterBand
		expectedMeterBand *nbdb.MeterBand
		initialNbdb       libovsdbtest.TestSetup
	}{
		{
			desc:           "create meter band when multiple equal bands exist",
			inputMeterBand: &nbdb.MeterBand{},
			expectedMeterBand: &nbdb.MeterBand{
				UUID: firstUUID,
			},
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.MeterBand{
						UUID: secondUUID,
					},
					&nbdb.MeterBand{
						UUID: firstUUID,
					},
					&nbdb.MeterBand{
						UUID: thirdUUID,
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(tt.initialNbdb, nil)
			if err != nil {
				t.Fatalf("%s: failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			meterBand := tt.inputMeterBand.DeepCopy()
			_, err = CreateMeterBandOps(nbClient, nil, meterBand)
			if err != nil {
				t.Fatal(fmt.Errorf("%s: got unexpected error: %v", tt.desc, err))
			}

			if !meterBand.Equals(tt.expectedMeterBand) {
				t.Fatal(fmt.Errorf("%s: unexpected meter band, got %+v expected %+v", tt.desc, meterBand, tt.expectedMeterBand))
			}
		})
	}
}

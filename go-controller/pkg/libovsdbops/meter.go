package libovsdbops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// FindMeterByName finds an ovn meter by name, will return `errNotFound` if it does not exist
func FindMeterByName(nbClient libovsdbclient.Client, name string) (*nbdb.Meter, error) {
	searched := &nbdb.Meter{
		Name: name,
	}

	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.Get(ctx, searched)
	if err != nil {
		return nil, err
	}

	return searched, nil
}

// UpdateMeterFairness updates the `fair` column of an ovn Meter, should only be called if a
// copy of the meter has already been acquired with FindMeter
func UpdateMeterFairness(nbClient libovsdbclient.Client, meter *nbdb.Meter, fairness bool) error {
	meter.Fair = &fairness

	opModel := OperationModel{
		Model: meter,
		OnModelUpdates: []interface{}{
			&meter.Fair,
		},
		ErrNotFound: true,
	}

	m := NewModelClient(nbClient)
	if _, err := m.CreateOrUpdate(opModel); err != nil {
		return fmt.Errorf("error while updating Meter Fairness to: %v error %v", fairness, err)
	}

	return nil
}

// CreateMeterWithBand simulates the ovn-nbctl operation performed by the `meter-add` command. i.e create the Meter
// and accompanying meter_band, and add the meterband to the meter only call this if the meter does not exist
func CreateMeterWithBand(nbClient libovsdbclient.Client, meter *nbdb.Meter, meterBand *nbdb.MeterBand) error {
	opModels := []OperationModel{
		{
			Model: meterBand,
			DoAfter: func() {
				meter.Bands = []string{meterBand.UUID}
			},
		},
		{
			Model: meter,
		},
	}

	m := NewModelClient(nbClient)
	if _, err := m.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("error while creating Meter_Band %+v and Meter %+v error %v", meterBand, meter, err)
	}

	return nil
}

package libovsdbops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"

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

// UpdateMeterFairness updates the `fair` column of an ovn Meter
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
func CreateMeterWithBand(nbClient libovsdbclient.Client, meter *nbdb.Meter, meterBand *nbdb.MeterBand) ([]ovsdb.OperationResult, error) {
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
	var results []ovsdb.OperationResult
	var err error
	if results, err = m.CreateOrUpdate(opModels...); err != nil {
		return nil, fmt.Errorf("error while creating Meter_Band %+v and Meter %+v error %v", meterBand, meter, err)
	}

	return results, nil
}

// FindMeterBands returns all MeterBands that belong to a given meter.
// If one of the meter bands cannot be found in the database, return an error.
func GetMeterBands(nbClient libovsdbclient.Client, meter *nbdb.Meter) ([]*nbdb.MeterBand, error) {
	var meterBands []*nbdb.MeterBand

	if meter == nil {
		return nil, fmt.Errorf("provided meter is invalid: <nil>")
	}

	for _, bandUUID := range meter.Bands {
		meterBand := &nbdb.MeterBand{
			UUID: bandUUID,
		}

		ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
		defer cancel()
		err := nbClient.Get(ctx, meterBand)
		if err != nil {
			return nil, err
		}
		meterBands = append(meterBands, meterBand)
	}
	return meterBands, nil
}

// UpdateMeterBandRate updates the `rate` column of an OVN MeterBand.
func UpdateMeterBandRate(nbClient libovsdbclient.Client, meterBand *nbdb.MeterBand, rate int) error {
	meterBand.Rate = rate

	opModel := OperationModel{
		Model: meterBand,
		OnModelUpdates: []interface{}{
			&meterBand.Rate,
		},
		ErrNotFound: true,
	}

	m := NewModelClient(nbClient)
	if _, err := m.CreateOrUpdate(opModel); err != nil {
		return fmt.Errorf("error while updating MeterBand Rate to: %d error %v", rate, err)
	}

	return nil
}

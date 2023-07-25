package ops

import (
	"reflect"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

func equalsMeterBand(a, b *nbdb.MeterBand) bool {
	return a.Action == b.Action &&
		a.BurstSize == b.BurstSize &&
		a.Rate == b.Rate &&
		reflect.DeepEqual(a.ExternalIDs, b.ExternalIDs)
}

// CreateMeterBandOps creates the provided meter band if it does not exist
func CreateMeterBandOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, meterBand *nbdb.MeterBand) ([]ovsdb.Operation, error) {
	bands := []*nbdb.MeterBand{}
	opModel := operationModel{
		Model:          meterBand,
		ModelPredicate: func(item *nbdb.MeterBand) bool { return equalsMeterBand(item, meterBand) },
		OnModelUpdates: onModelUpdatesNone(),
		ExistingResult: &bands,
		DoAfter: func() {
			// in case we have multiple equal bands, pick the first one for
			// convergence, OVSDB will remove unreferenced ones
			if len(bands) > 0 {
				uuids := sets.NewString()
				for _, band := range bands {
					uuids.Insert(band.UUID)
				}
				meterBand.UUID = uuids.List()[0]
			}
		},
		ErrNotFound: false,
		BulkOp:      true,
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModel)
}

// CreateOrUpdateMeterOps creates or updates the provided meter associated to
// the provided meter bands and returns the corresponding ops
func CreateOrUpdateMeterOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, meter *nbdb.Meter, meterBands []*nbdb.MeterBand, fields ...interface{}) ([]ovsdb.Operation, error) {
	if len(fields) == 0 {
		fields = onModelUpdatesAllNonDefault()
	}
	meter.Bands = make([]string, 0, len(meterBands))
	for _, band := range meterBands {
		meter.Bands = append(meter.Bands, band.UUID)
	}
	opModel := operationModel{
		Model:          meter,
		OnModelUpdates: fields,
		ErrNotFound:    false,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModel)
}

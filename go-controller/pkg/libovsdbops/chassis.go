package libovsdbops

import (
	"context"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// ListChassis looks up all chassis from the cache
func ListChassis(sbClient libovsdbclient.Client) ([]*sbdb.Chassis, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedChassis := []*sbdb.Chassis{}
	err := sbClient.List(ctx, &searchedChassis)
	return searchedChassis, err
}

// DeleteChassis deletes the provided chassis
func DeleteChassis(sbClient libovsdbclient.Client, chassis ...*sbdb.Chassis) error {
	opModels := make([]OperationModel, 0, len(chassis))
	for i := range chassis {
		opModel := OperationModel{
			Model:       chassis[i],
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels = append(opModels, opModel)
	}

	m := NewModelClient(sbClient)
	err := m.Delete(opModels...)
	return err
}

type chassisPredicate func(*sbdb.Chassis) bool

// DeleteChassisWithPredicate looks up chassis from the cache based on a given
// predicate and deletes them
func DeleteChassisWithPredicate(sbClient libovsdbclient.Client, p chassisPredicate) error {
	opModel := OperationModel{
		Model:          &sbdb.Chassis{},
		ModelPredicate: p,
		ErrNotFound:    false,
		BulkOp:         true,
	}
	m := NewModelClient(sbClient)
	err := m.Delete(opModel)
	return err
}

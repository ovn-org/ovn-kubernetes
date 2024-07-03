package ops

import (
	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// CreateOrUpdateStaticMacBinding creates or updates the provided static mac binding
func CreateOrUpdateStaticMacBinding(nbClient libovsdbclient.Client, smbs ...*nbdb.StaticMACBinding) error {
	opModels := make([]operationModel, len(smbs))
	for i := range smbs {
		opModel := operationModel{
			Model:          smbs[i],
			OnModelUpdates: onModelUpdatesAllNonDefault(),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels[i] = opModel
	}

	m := newModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModels...)
	return err
}

// DeleteStaticMacBindings deletes the provided static mac bindings
func DeleteStaticMacBindings(nbClient libovsdbclient.Client, smbs ...*nbdb.StaticMACBinding) error {
	opModels := make([]operationModel, len(smbs))
	for i := range smbs {
		opModel := operationModel{
			Model:       smbs[i],
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels[i] = opModel
	}

	m := newModelClient(nbClient)
	return m.Delete(opModels...)
}

type staticMACBindingPredicate func(*nbdb.StaticMACBinding) bool

// DeleteStaticMACBindingWithPredicate deletes a Static MAC entry for a logical port from the cache
func DeleteStaticMACBindingWithPredicate(nbClient libovsdbclient.Client, p staticMACBindingPredicate) error {
	found := []*nbdb.StaticMACBinding{}
	opModel := operationModel{
		ModelPredicate: p,
		ExistingResult: &found,
		ErrNotFound:    false,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	err := m.Delete(opModel)
	if err != nil {
		return err
	}
	return nil
}

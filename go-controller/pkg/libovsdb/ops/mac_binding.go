package ops

import (
	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
)

// CreateOrUpdateMacBinding creates or updates the provided mac binding
func CreateOrUpdateMacBinding(sbClient libovsdbclient.Client, mb *sbdb.MACBinding, fields ...interface{}) error {
	if len(fields) == 0 {
		fields = onModelUpdatesAllNonDefault()
	}

	opModel := operationModel{
		Model:          mb,
		OnModelUpdates: fields,
		ErrNotFound:    false,
		BulkOp:         false,
	}

	m := newModelClient(sbClient)
	_, err := m.CreateOrUpdate(opModel)
	return err
}

type macBindingPredicate func(*sbdb.MACBinding) bool

// DeleteMacBindingWithPredicate looks up mac bindings from the cache based on a
// given predicate and deletes them
func DeleteMacBindingWithPredicate(sbClient libovsdbclient.Client, p macBindingPredicate) error {
	deleted := []*sbdb.MACBinding{}
	opModel := operationModel{
		ModelPredicate: p,
		ExistingResult: &deleted,
		ErrNotFound:    false,
		BulkOp:         true,
	}

	m := newModelClient(sbClient)
	return m.Delete(opModel)
}

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

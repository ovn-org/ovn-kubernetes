package libovsdbops

import (
	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
)

// Create or UpdateMacBinding either create a new mac binding or ensure the existing one has the
// correct datapath, logical_port, ip, and mac column entries
func CreateOrUpdateMacBinding(sbClient libovsdbclient.Client, mb *sbdb.MACBinding) error {
	opModel := OperationModel{
		// no predicate needed, ip and logical_port columns are indexes
		Model: mb,
		OnModelUpdates: []interface{}{
			&mb.Datapath,
			&mb.LogicalPort,
			&mb.IP,
			&mb.MAC,
		},
	}

	m := NewModelClient(sbClient)
	if _, err := m.CreateOrUpdate(opModel); err != nil {
		return err
	}

	return nil
}

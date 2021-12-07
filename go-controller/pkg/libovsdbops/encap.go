package libovsdbops

import (
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
)

// MutateEncapOptions adds the provided options, and fails if the encap table for a given host does not exist in sbdb
func MutateEncapOptions(sbClient libovsdbclient.Client, chassisName string, options map[string]string) error {
	encap := &sbdb.Encap{
		ChassisName: chassisName,
		Options:     options,
	}

	opModel := OperationModel{
		Model: encap,
		// ChassisName column is not an index, use modelPredicate
		ModelPredicate: func(en *sbdb.Encap) bool { return en.ChassisName == encap.ChassisName },
		OnModelMutations: []interface{}{
			&encap.Options,
		},
		ErrNotFound: true,
	}

	m := NewModelClient(sbClient)
	if _, err := m.CreateOrUpdate(opModel); err != nil {
		return fmt.Errorf("failed to mutate encap options for chassis %s, error: %v", chassisName, err)
	}

	return nil
}

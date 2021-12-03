package libovsdbops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// findNBGlobal returns the NBGlobal table entry
func FindNBGlobal(nbClient libovsdbclient.Client) (*nbdb.NBGlobal, error) {
	nbGlobal := []nbdb.NBGlobal{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.List(ctx, &nbGlobal)
	if err != nil {
		return nil, fmt.Errorf("failed listing nbGlobal table entires err: %v", err)
	}
	// We should error if the nbGlobal table entry does not exist
	if len(nbGlobal) == 0 {
		return nil, libovsdbclient.ErrNotFound
	}
	// The nbGlobal table should only have one row
	if len(nbGlobal) != 1 {
		return nil, fmt.Errorf("multible NBGlobal rows found")
	}

	return &nbGlobal[0], nil
}

// UpdateNBGlobalOptions updates the options on the NBGlobal table, adding any newly specified options in the process
func UpdateNBGlobalOptions(nbClient libovsdbclient.Client, options map[string]string) error {
	// find the nbGlobal table's UUID, we don't have any other way to reliably look this table entry since it can
	// only be indexed by UUID
	nbGlobal, err := FindNBGlobal(nbClient)
	if err != nil {
		return err
	}

	for k, v := range options {
		nbGlobal.Options[k] = v
	}

	// Update the options column in the nbGlobal entry since we already performed a lookup
	opModel := OperationModel{
		Model: nbGlobal,
		OnModelUpdates: []interface{}{
			&nbGlobal.Options,
		},
		ErrNotFound: true,
	}

	m := NewModelClient(nbClient)
	if _, err := m.CreateOrUpdate(opModel); err != nil {
		return fmt.Errorf("error while updating NBGlobal Options: %v error %v", options, err)
	}

	return nil
}

// UpdateNBGlobalNbCfg updates the nb_cfg field for the single row in the NBGlobal table
func UpdateNBGlobalNbCfg(nbClient libovsdbclient.Client, nbCfg int) error {
	nbGlobal, err := FindNBGlobal(nbClient)
	if err != nil {
		return err
	}
	nbGlobal.NbCfg = nbCfg

	opModel := OperationModel{
		Model: nbGlobal,
		OnModelUpdates: []interface{}{
			&nbGlobal.NbCfg,
		},
		ErrNotFound: true,
	}

	m := NewModelClient(nbClient)
	if _, err := m.CreateOrUpdate(opModel); err != nil {
		return fmt.Errorf("error while updating NBGlobal nb_cfg: %v", err)
	}
	return nil
}

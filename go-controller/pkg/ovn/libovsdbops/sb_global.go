package libovsdbops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// findSBGlobal returns the SBGlobal table entry
func FindSBGlobal(sbClient libovsdbclient.Client) (*sbdb.SBGlobal, error) {
	sbGlobal := []sbdb.SBGlobal{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := sbClient.List(ctx, &sbGlobal)
	if err != nil {
		return nil, fmt.Errorf("failed listing nbGlobal table entires err: %v", err)
	}
	// We should error if the nbGlobal table entry does not exist
	if len(sbGlobal) == 0 {
		return nil, libovsdbclient.ErrNotFound
	}
	// The nbGlobal table should only have one row
	if len(sbGlobal) != 1 {
		return nil, fmt.Errorf("multible NBGlobal rows found")
	}

	return &sbGlobal[0], nil
}

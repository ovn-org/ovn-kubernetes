package ops

import (
	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
)

// GetNBGlobal looks up the SB Global entry from the cache
func GetSBGlobal(sbClient libovsdbclient.Client, sbGlobal *sbdb.SBGlobal) (*sbdb.SBGlobal, error) {
	found := []*sbdb.SBGlobal{}
	opModel := operationModel{
		Model:          sbGlobal,
		ModelPredicate: func(item *sbdb.SBGlobal) bool { return true },
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(sbClient)
	err := m.Lookup(opModel)
	if err != nil {
		return nil, err
	}

	return found[0], nil
}

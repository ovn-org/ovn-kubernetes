package ops

import (
	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// GetNBGlobal looks up the NB Global entry from the cache
func GetNBGlobal(nbClient libovsdbclient.Client, nbGlobal *nbdb.NBGlobal) (*nbdb.NBGlobal, error) {
	found := []*nbdb.NBGlobal{}
	opModel := operationModel{
		Model:          nbGlobal,
		ModelPredicate: func(item *nbdb.NBGlobal) bool { return true },
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	err := m.Lookup(opModel)
	if err != nil {
		return nil, err
	}

	return found[0], nil
}

// UpdateNBGlobalSetOptions sets options on the NB Global entry adding any
// missing, removing the ones set to an empty value and updating existing
func UpdateNBGlobalSetOptions(nbClient libovsdbclient.Client, nbGlobal *nbdb.NBGlobal) error {
	// find the nbGlobal table's UUID, we don't have any other way to reliably look this table entry since it can
	// only be indexed by UUID
	updatedNbGlobal, err := GetNBGlobal(nbClient, nbGlobal)
	if err != nil {
		return err
	}

	if updatedNbGlobal.Options == nil {
		updatedNbGlobal.Options = map[string]string{}
	}

	for k, v := range nbGlobal.Options {
		if v == "" {
			delete(updatedNbGlobal.Options, k)
		} else {
			updatedNbGlobal.Options[k] = v
		}
	}

	// Update the options column in the nbGlobal entry since we already performed a lookup
	opModel := operationModel{
		Model: updatedNbGlobal,
		OnModelUpdates: []interface{}{
			&updatedNbGlobal.Options,
		},
		ErrNotFound: true,
		BulkOp:      false,
	}

	m := newModelClient(nbClient)
	_, err = m.CreateOrUpdate(opModel)
	return err
}

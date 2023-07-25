package ops

import (
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

type coppPredicate func(*nbdb.Copp) bool

// CreateOrUpdateCOPPsOps creates or updates the provided COPP returning the
// corresponding ops
func CreateOrUpdateCOPPsOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, copps ...*nbdb.Copp) ([]ovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(copps))
	for i := range copps {
		// can't use i in the predicate, for loop replaces it in-memory
		copp := copps[i]
		opModel := operationModel{
			Model:          copp,
			OnModelUpdates: onModelUpdatesAllNonDefault(),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}

// DeleteCOPPsOps deletes the provided COPPs found using the predicate, returning the
// corresponding ops
func DeleteCOPPsWithPredicateOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, p coppPredicate) ([]ovsdb.Operation, error) {
	copp := nbdb.Copp{}
	opModels := []operationModel{
		{
			Model:          &copp,
			ModelPredicate: p,
			ErrNotFound:    false,
			BulkOp:         true,
		},
	}

	modelClient := newModelClient(nbClient)
	return modelClient.DeleteOps(ops, opModels...)
}

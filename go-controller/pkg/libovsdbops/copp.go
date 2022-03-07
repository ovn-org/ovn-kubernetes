package libovsdbops

import (
	"reflect"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// CreateOrUpdateCOPPsOps creates or updates the provided COPP returning the
// corresponding ops
func CreateOrUpdateCOPPsOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, copps ...*nbdb.Copp) ([]ovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(copps))
	for i := range copps {
		// can't use i in the predicate, for loop replaces it in-memory
		copp := copps[i]
		opModel := operationModel{
			Model:          copp,
			ModelPredicate: func(item *nbdb.Copp) bool { return reflect.DeepEqual(item.Meters, copp.Meters) },
			OnModelUpdates: onModelUpdatesNone(),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}

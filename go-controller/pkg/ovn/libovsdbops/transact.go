package libovsdbops

import (
	"context"
	"fmt"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TransactAndCheck(client client.Client, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	if len(ops) <= 0 {
		return []ovsdb.OperationResult{{}}, nil
	}

	ctx, cancel := context.WithTimeout(context.TODO(), types.OVSDBTimeout)
	defer cancel()

	results, err := client.Transact(ctx, ops...)
	if err != nil {
		return nil, fmt.Errorf("error in transact with ops %+v: %v", ops, err)
	}

	opErrors, err := ovsdb.CheckOperationResults(results, ops)
	if err != nil {
		return nil, fmt.Errorf("error in transact with ops %+v results %+v and errors %+v: %v", ops, results, opErrors, err)
	}

	return results, nil
}

// TransactAndCheckAndSetUUIDs transacts the given ops againts client and returns
// results if no error ocurred or an error otherwise. It sets the real uuids for
// the passed models if they were inserted and have a named-uuid (as built by
// BuildNamedUUID)
func TransactAndCheckAndSetUUIDs(client client.Client, models interface{}, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	results, err := TransactAndCheck(client, ops)
	if err != nil {
		return nil, err
	}

	namedModelMap := map[string]model.Model{}
	_ = onModels(models, func(model interface{}) error {
		uuid := getUUID(model)
		if IsNamedUUID(uuid) {
			namedModelMap[uuid] = model
		}
		return nil
	})

	if len(namedModelMap) == 0 {
		return results, nil
	}

	for i, op := range ops {
		if op.Op != ovsdb.OperationInsert {
			continue
		}

		if !IsNamedUUID(op.UUIDName) {
			continue
		}

		if model, ok := namedModelMap[op.UUIDName]; ok {
			setUUID(model, results[i].UUID.GoUUID)
		}
	}

	return results, nil
}

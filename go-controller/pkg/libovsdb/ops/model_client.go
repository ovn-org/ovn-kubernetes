package ops

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"k8s.io/klog/v2"
)

var errMultipleResults = errors.New("unexpectedly found multiple results for provided predicate")
var errNoIndexes = errors.New("no indexes found for given model")

type modelClient struct {
	client client.Client
}

func newModelClient(client client.Client) modelClient {
	return modelClient{
		client: client,
	}
}

/*
extractUUIDsFromModels is a helper function which constructs a mutation
for the specified field and mutator extracting the UUIDs of the provided
models as the value for the mutation.
*/
func extractUUIDsFromModels(models interface{}) []string {
	ids := []string{}
	_ = onModels(models, func(model interface{}) error {
		uuid := getUUID(model)
		if uuid != "" {
			ids = append(ids, uuid)
		}
		return nil
	})
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// buildMutationsFromFields builds mutations that use the fields as values.
func buildMutationsFromFields(fields []interface{}, mutator ovsdb.Mutator) ([]model.Mutation, error) {
	mutations := []model.Mutation{}
	for _, field := range fields {
		switch v := field.(type) {
		case *map[string]string:
			if v == nil || len(*v) == 0 {
				continue
			}
			if mutator == ovsdb.MutateOperationDelete {
				// turn empty map values into a mutation to remove the key for
				// delete mutations
				removeKeys := make([]string, 0, len(*v))
				updateKeys := make(map[string]string, len(*v))
				for key, value := range *v {
					if value == "" {
						removeKeys = append(removeKeys, key)
					} else {
						updateKeys[key] = value
					}
				}
				if len(removeKeys) > 0 {
					mutation := model.Mutation{
						Field:   field,
						Mutator: mutator,
						Value:   removeKeys,
					}
					mutations = append(mutations, mutation)
				}
				if len(updateKeys) > 0 {
					mutation := model.Mutation{
						Field:   field,
						Mutator: mutator,
						Value:   updateKeys,
					}
					mutations = append(mutations, mutation)
				}
				continue
			}
			// RFC 7047, section 5.1: a MutateOperationDelete is generated
			// automatically for every updated key.
			removeKeys := make([]string, 0, len(*v))
			for key := range *v {
				removeKeys = append(removeKeys, key)
			}
			if len(removeKeys) > 0 {
				mutation := model.Mutation{
					Field:   field,
					Mutator: ovsdb.MutateOperationDelete,
					Value:   removeKeys,
				}
				mutations = append(mutations, mutation)
			}
			mutation := model.Mutation{
				Field:   field,
				Mutator: mutator,
				Value:   *v,
			}
			mutations = append(mutations, mutation)
		case *[]string:
			if v == nil || len(*v) == 0 {
				continue
			}
			if mutator == ovsdb.MutateOperationInsert {
				// Most of string sets are UUIDs. The real server does not allow
				// this to be empty but the test server does for now. On other
				// types of sets most probably there is no need to have empty
				// items. So catch this early.
				for _, value := range *v {
					if value == "" {
						return nil, fmt.Errorf("unsupported mutation of set with empty values: %v", *v)
					}
				}
			}
			mutation := model.Mutation{
				Field:   field,
				Mutator: mutator,
				Value:   *v,
			}
			mutations = append(mutations, mutation)
		default:
			return nil, fmt.Errorf("mutation for type %T not implemented", v)
		}
	}

	return mutations, nil
}

/*
operationModel is a struct which uses reflection to determine and perform
idempotent operations against OVS DB (NB DB by default).
*/
type operationModel struct {
	// Model specifies the model to be created, or to look up in the cache
	// if ModelPredicate is not specified. The values in the fields of the
	// Model are used for mutations and updates as well. If this Model is
	// looked up or created, it will have its UUID set after the operation.
	Model interface{}
	// ModelPredicate specifies a predicate to look up models in the cache.
	ModelPredicate interface{}
	// ExistingResult is where the results of the look up are added to.
	// Required when Model is not specified.
	ExistingResult interface{}
	// OnModelMutations specifies the fields from Model that will be used as
	// the mutation value.
	OnModelMutations []interface{}
	// OnModelUpdates specifies the fields from Model that will be used as
	// the update value.
	OnModelUpdates []interface{}
	// ErrNotFound flags this operation to fail with ErrNotFound if a model is
	// not found.
	ErrNotFound bool
	// BulkOp flags this operation as a bulk operation capable of updating or
	// mutating more than 1 model.
	BulkOp bool
	// DoAfter is invoked at the end of the operation and allows to setup a
	// subsequent operation with values obtained from this one.
	// If model lookup was successful, or a new db entry was created,
	// Model will have UUID set, and it can be used in DoAfter. This only works
	// if BulkOp is false and Model != nil.
	DoAfter func()
}

func onModelUpdatesNone() []interface{} {
	return nil
}

func onModelUpdatesAllNonDefault() []interface{} {
	return []interface{}{}
}

/*
CreateOrUpdate performs idempotent operations against libovsdb according to the
following logic:

a) performs a lookup of the models in the cache by ModelPredicate if provided,
or by Model otherwise. If the models do not exist and ErrNotFound is set,
it returns ErrNotFound

b) if OnModelUpdates is specified; it performs a direct update of the model if
it exists.

c) if b) is not true, but OnModelMutations is specified; it performs a direct
mutation (insert) of the Model if it exists.

d) if b) and c) are not true, but Model is provided, it creates the Model
if it does not exist.

e) if none of the above are true, ErrNotFound is returned.

If BulkOp is set, update or mutate can happen accross multiple models found.
*/
func (m *modelClient) CreateOrUpdate(opModels ...operationModel) ([]ovsdb.OperationResult, error) {
	created, ops, err := m.createOrUpdateOps(nil, opModels...)
	if err != nil {
		return nil, err
	}
	return TransactAndCheckAndSetUUIDs(m.client, created, ops)
}

func (m *modelClient) CreateOrUpdateOps(ops []ovsdb.Operation, opModels ...operationModel) ([]ovsdb.Operation, error) {
	_, ops, err := m.createOrUpdateOps(ops, opModels...)
	return ops, err
}

func (m *modelClient) createOrUpdateOps(ops []ovsdb.Operation, opModels ...operationModel) (interface{}, []ovsdb.Operation, error) {
	hasGuardOp := len(ops) > 0 && isGuardOp(&ops[0])
	guardOp := []ovsdb.Operation{}
	doWhenFound := func(model interface{}, opModel *operationModel) ([]ovsdb.Operation, error) {
		// nil represents onModelUpdatesNone
		if opModel.OnModelUpdates != nil {
			return m.update(model, opModel)
		} else if opModel.OnModelMutations != nil {
			return m.mutate(model, opModel, ovsdb.MutateOperationInsert)
		}
		return nil, nil
	}
	doWhenNotFound := func(model interface{}, opModel *operationModel) ([]ovsdb.Operation, error) {
		if !hasGuardOp {
			// for the first insert of certain models, build a wait operation
			// that checks for duplicates as a guard op to prevent against
			// duplicate transactions
			var err error
			guardOp, err = buildFailOnDuplicateOps(m.client, opModel.Model)
			if err != nil {
				return nil, err
			}
			hasGuardOp = len(guardOp) > 0
		}
		return m.create(opModel)
	}
	created, ops, err := m.buildOps(ops, doWhenFound, doWhenNotFound, opModels...)
	if len(guardOp) > 0 {
		// set the guard op as the first of the list
		ops = append(guardOp, ops...)
	}
	return created, ops, err
}

/*
Delete performs idempotent delete operations against libovsdb according to the
following logic:

a) performs a lookup of the models in the cache by ModelPredicate if provided,
or by Model otherwise. If the models do not exist and ErrNotFound is set
it returns ErrNotFound.

b) if OnModelMutations is specified; it performs a direct mutation (delete) of the
Model if it exists.

c) if b) is not true; it performs a direct delete of the Model if it exists.

If BulkOp is set, delete or mutate can happen accross multiple models found.
*/
func (m *modelClient) Delete(opModels ...operationModel) error {
	ops, err := m.DeleteOps(nil, opModels...)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(m.client, ops)
	return err
}

func (m *modelClient) DeleteOps(ops []ovsdb.Operation, opModels ...operationModel) ([]ovsdb.Operation, error) {
	doWhenFound := func(model interface{}, opModel *operationModel) (o []ovsdb.Operation, err error) {
		if opModel.OnModelMutations != nil {
			return m.mutate(model, opModel, ovsdb.MutateOperationDelete)
		} else {
			return m.delete(model, opModel)
		}
	}
	_, ops, err := m.buildOps(ops, doWhenFound, nil, opModels...)
	return ops, err
}

type opModelToOpMapper func(model interface{}, opModel *operationModel) (o []ovsdb.Operation, err error)

func (m *modelClient) buildOps(ops []ovsdb.Operation, doWhenFound opModelToOpMapper, doWhenNotFound opModelToOpMapper, opModels ...operationModel) (interface{}, []ovsdb.Operation, error) {
	if ops == nil {
		ops = []ovsdb.Operation{}
	}
	notfound := []interface{}{}
	for _, opModel := range opModels {
		// do lookup
		err := m.lookup(&opModel)
		if err != nil && err != client.ErrNotFound {
			return nil, nil, fmt.Errorf("unable to lookup model %+v: %v", opModel, err)
		}

		// do updates
		var hadExistingResults bool
		err = onModels(opModel.ExistingResult, func(model interface{}) error {
			if hadExistingResults && !opModel.BulkOp {
				return errMultipleResults
			}
			hadExistingResults = true

			if doWhenFound != nil {
				o, err := doWhenFound(model, &opModel)
				if err != nil {
					return err
				}
				ops = append(ops, o...)
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}

		// otherwise act when not found
		if !hadExistingResults {
			// return ErrNotFound,
			// - if caller explicitly requested for it or
			// - failed to provide a Model for us to apply the operation on
			if opModel.ErrNotFound || (doWhenNotFound != nil && opModel.Model == nil) {
				return nil, nil, client.ErrNotFound
			}
			if doWhenNotFound != nil && opModel.Model != nil {
				o, err := doWhenNotFound(nil, &opModel)
				if err != nil {
					return nil, nil, err
				}
				ops = append(ops, o...)
				notfound = append(notfound, opModel.Model)
			}
		}

		if opModel.DoAfter != nil {
			opModel.DoAfter()
		}
	}

	return notfound, ops, nil
}

/*
create does a bit more than just "create". create needs to set the generated
UUID (because if this function is called we know the item does not exists yet)
then create the item. Generates an until clause and uses a wait operation to avoid
https://bugzilla.redhat.com/show_bug.cgi?id=2042001
*/
func (m *modelClient) create(opModel *operationModel) ([]ovsdb.Operation, error) {
	uuid := getUUID(opModel.Model)
	if uuid == "" {
		setUUID(opModel.Model, buildNamedUUID())
	}

	ops, err := m.client.Create(opModel.Model)
	if err != nil {
		return nil, fmt.Errorf("unable to create model, err: %v", err)
	}

	klog.V(5).Infof("Create operations generated as: %+v", ops)
	return ops, nil
}

func (m *modelClient) update(lookUpModel interface{}, opModel *operationModel) (o []ovsdb.Operation, err error) {
	o, err = m.client.Where(lookUpModel).Update(opModel.Model, opModel.OnModelUpdates...)
	if err != nil {
		return nil, fmt.Errorf("unable to update model, err: %v", err)
	}
	klog.V(5).Infof("Update operations generated as: %+v", o)
	return o, nil
}

func (m *modelClient) mutate(lookUpModel interface{}, opModel *operationModel, mutator ovsdb.Mutator) (o []ovsdb.Operation, err error) {
	if opModel.OnModelMutations == nil {
		return nil, nil
	}
	modelMutations, err := buildMutationsFromFields(opModel.OnModelMutations, mutator)
	if len(modelMutations) == 0 || err != nil {
		return nil, err
	}
	o, err = m.client.Where(lookUpModel).Mutate(opModel.Model, modelMutations...)
	if err != nil {
		return nil, fmt.Errorf("unable to mutate model, err: %v", err)
	}
	klog.V(5).Infof("Mutate operations generated as: %+v", o)
	return o, nil
}

func (m *modelClient) delete(lookUpModel interface{}, opModel *operationModel) (o []ovsdb.Operation, err error) {
	o, err = m.client.Where(lookUpModel).Delete()
	if err != nil {
		return nil, fmt.Errorf("unable to delete model, err: %v", err)
	}
	klog.V(5).Infof("Delete operations generated as: %+v", o)
	return o, nil
}

func (m *modelClient) Lookup(opModels ...operationModel) error {
	_, _, err := m.buildOps(nil, nil, nil, opModels...)
	return err
}

// CreateOrUpdate, Delete and Lookup can be called to
// 1. create or update a single model
// Model should be set, bulkOp = false, errNotfound = false
// 2. update/delete/lookup 0..n models (create can't be done for multiple models at the same time)
// Model index or predicate should be set
//
// The allowed combination of operationModel fields is different for these cases.
// Both Model db index, and ModelPredicate can only be empty for the first case
func lookupRequired(opModel *operationModel) bool {
	// we know create is not supposed to be performed, if these fields are set
	if opModel.BulkOp || opModel.ErrNotFound {
		return true
	}
	return false
}

// lookup the model in the cache prioritizing provided indexes over a
// predicate
// If lookup was successful, opModel.Model will have UUID set,
// so that further user operations with the same model are indexed by UUID
func (m *modelClient) lookup(opModel *operationModel) error {
	if opModel.ExistingResult == nil && opModel.Model != nil {
		opModel.ExistingResult = getListFromModel(opModel.Model)
	}

	var err error
	if opModel.Model != nil {
		err = m.where(opModel)
		if err != errNoIndexes {
			// if index wasn't provided by the Model, try predicate search
			// otherwise return where result
			return err
		}
	}
	// if index wasn't provided by the Model (errNoIndexes) or Model == nil, try predicate search
	if opModel.ModelPredicate != nil {
		return m.whereCache(opModel)
	}
	// the only operation that can be performed without a lookup (it can have no db indexes and no ModelPredicate set)
	// is Create.
	if lookupRequired(opModel) {
		return fmt.Errorf("missing model indixes or predicate when a lookup was required")
	}
	return nil
}

func (m *modelClient) where(opModel *operationModel) error {
	copyModel := copyIndexes(opModel.Model)
	if reflect.ValueOf(copyModel).Elem().IsZero() {
		// no indexes available
		return errNoIndexes
	}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	var err error
	if err = m.client.Where(copyModel).List(ctx, opModel.ExistingResult); err != nil {
		return err
	}
	if opModel.Model == nil || opModel.BulkOp {
		return nil
	}
	// for non-bulk op cases, copy (the one) uuid found to model provided.
	// so that further user operations with the same model are indexed by UUID
	err = onModels(opModel.ExistingResult, func(model interface{}) error {
		uuid := getUUID(model)
		setUUID(opModel.Model, uuid)
		return nil
	})
	return err
}

func (m *modelClient) whereCache(opModel *operationModel) error {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	var err error
	if err = m.client.WhereCache(opModel.ModelPredicate).List(ctx, opModel.ExistingResult); err != nil {
		return err
	}

	if opModel.Model == nil || opModel.BulkOp {
		return nil
	}

	// for non-bulk op cases, copy (the one) uuid found to model provided.
	// so that further user operations with the same model are indexed by UUID
	err = onModels(opModel.ExistingResult, func(model interface{}) error {
		uuid := getUUID(model)
		setUUID(opModel.Model, uuid)
		return nil
	})
	return err
}

func isGuardOp(op *ovsdb.Operation) bool {
	return op != nil && op.Op == ovsdb.OperationWait && op.Timeout != nil && *op.Timeout == types.OVSDBWaitTimeout
}

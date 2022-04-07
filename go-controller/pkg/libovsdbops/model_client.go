package libovsdbops

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
	DoAfter func()
}

func onModelUpdatesNone() []interface{} {
	return nil
}

func onModelUpdatesAll() []interface{} {
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
	doWhenFound := func(model interface{}, opModel *operationModel) ([]ovsdb.Operation, error) {
		if opModel.OnModelUpdates != nil {
			return m.update(model, opModel)
		} else if opModel.OnModelMutations != nil {
			return m.mutate(model, opModel, ovsdb.MutateOperationInsert)
		}
		return nil, nil
	}
	doWhenNotFound := func(model interface{}, opModel *operationModel) ([]ovsdb.Operation, error) {
		return m.create(opModel)
	}
	return m.buildOps(ops, doWhenFound, doWhenNotFound, opModels...)
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

	ops, err := buildFailOnDuplicateOps(m.client, opModel.Model)
	if err != nil {
		return nil, fmt.Errorf("unable to create model, err: %v", err)
	}

	op, err := m.client.Create(opModel.Model)
	if err != nil {
		return nil, fmt.Errorf("unable to create model, err: %v", err)
	}
	ops = append(ops, op...)

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

// lookup the model in the cache prioritizing provided indexes over a
// predicate unless bulkop is set
func (m *modelClient) lookup(opModel *operationModel) error {
	if opModel.ExistingResult == nil && opModel.Model != nil {
		opModel.ExistingResult = getListFromModel(opModel.Model)
	}

	var err error
	if opModel.Model != nil && !opModel.BulkOp {
		err = m.get(opModel)
		if err == nil || err != client.ErrNotFound {
			return err
		}
	}

	if opModel.ModelPredicate != nil {
		err = m.whereCache(opModel)
	} else if opModel.BulkOp {
		panic("Expected a ModelPredicate with BulkOp==true")
	}

	return err
}

/*
 get copies the model, since this function ends up being called from update /
 mutate, and Get'ing the model will modify the object to the one currently
 existing in the DB, thus overridding all new fields we are trying to set. Do
 return the retrived object though, in case the caller needs to act on the
 object's UUID
*/
func (m *modelClient) get(opModel *operationModel) error {
	copy := copyIndexes(opModel.Model)
	if reflect.ValueOf(copy).Elem().IsZero() {
		// no indexes available
		return client.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	if err := m.client.Get(ctx, copy); err != nil {
		return err
	}
	uuid := getUUID(opModel.Model)
	if uuid == "" || isNamedUUID(uuid) {
		setUUID(opModel.Model, getUUID(copy))
	}
	return addToExistingResult(copy, opModel.ExistingResult)
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

	// for non-bulk op cases, copy (the one) uuid found to model provided, for convenience
	err = onModels(opModel.ExistingResult, func(model interface{}) error {
		uuid := getUUID(model)
		setUUID(opModel.Model, uuid)
		return nil
	})
	return err
}

func addToExistingResult(model interface{}, existingResult interface{}) error {
	resultPtr := reflect.ValueOf(existingResult)
	if resultPtr.Type().Kind() != reflect.Ptr {
		return fmt.Errorf("expected existingResult as a pointer but got %s", resultPtr.Type().Kind())
	}

	resultVal := reflect.Indirect(resultPtr)
	if resultVal.Type().Kind() != reflect.Slice {
		return fmt.Errorf("expected existingResult as a pointer to a slice but got %s", resultVal.Type().Kind())
	}

	var v reflect.Value
	if resultVal.Type().Elem().Kind() == reflect.Ptr {
		v = reflect.ValueOf(model)
	} else {
		v = reflect.Indirect(reflect.ValueOf(model))
	}

	if v.Type() != resultVal.Type().Elem() {
		return fmt.Errorf("expected existingResult as a pointer to a slice of %s but got %s", v.Type(), resultVal.Type().Elem())
	}

	resultVal.Set(reflect.Append(resultVal, v))

	return nil
}

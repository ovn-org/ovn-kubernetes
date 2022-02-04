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

type ModelClient struct {
	client client.Client
}

func NewModelClient(client client.Client) ModelClient {
	return ModelClient{
		client: client,
	}
}

/*
 ExtractUUIDsFromModels is a helper function which constructs a mutation
 for the specified field and mutator extracting the UUIDs of the provided
 models as the value for the mutation.
*/
func ExtractUUIDsFromModels(models interface{}) []string {
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

// BuildMutationsFromFields builds mutations that use the fields as values.
func BuildMutationsFromFields(fields []interface{}, mutator ovsdb.Mutator) []model.Mutation {
	mutations := make([]model.Mutation, 0, len(fields))
	for _, field := range fields {
		v := reflect.ValueOf(field)
		if v.Kind() != reflect.Ptr {
			panic(fmt.Sprintf("Expected Ptr but got %s", v.Kind()))
		}
		if v.IsNil() || v.Elem().IsNil() {
			continue
		}

		if m, ok := field.(*map[string]string); ok {
			// check if all values on m are zero, if so create slice of keys of m and set that to field
			allEmpty := true
			keySlice := []string{}
			for key, value := range *m {
				keySlice = append(keySlice, key)
				if len(value) > 0 {
					allEmpty = false
					break
				}
			}
			if allEmpty {
				v = reflect.ValueOf(&keySlice)
			}
		} else if v.Elem().Kind() == reflect.Map {
			panic(fmt.Sprintf("map type %v is not supported", v.Elem().Kind()))
		}

		mutation := model.Mutation{
			Field:   field,
			Mutator: mutator,
			Value:   v.Elem().Interface(),
		}
		mutations = append(mutations, mutation)
	}
	return mutations
}

/*
 OperationModel is a struct which uses reflection to determine and perform
 idempotent operations against OVS DB (NB DB by default).
*/
type OperationModel struct {
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
	// Name is used to signify if this model has a name being used. Typically
	// corresponds to Name field used on ovsdb objects. Using a non-empty
	// Name indicates that during a Create the model will have a predicate
	// operation to ensure a duplicate txn will not occur. See:
	// https://bugzilla.redhat.com/show_bug.cgi?id=2042001
	Name interface{}
}

// WithClient is useful for ad-hoc override of the targetted OVS DB. Can be used,
// for example, to specify talking to the SB DB ad-hoc, for a given call.
func (m *ModelClient) WithClient(client client.Client) *ModelClient {
	cl := NewModelClient(client)
	return &cl
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
func (m *ModelClient) CreateOrUpdate(opModels ...OperationModel) ([]ovsdb.OperationResult, error) {
	created, ops, err := m.createOrUpdateOps(nil, opModels...)
	if err != nil {
		return nil, err
	}
	return TransactAndCheckAndSetUUIDs(m.client, created, ops)
}

func (m *ModelClient) CreateOrUpdateOps(ops []ovsdb.Operation, opModels ...OperationModel) ([]ovsdb.Operation, error) {
	_, ops, err := m.createOrUpdateOps(ops, opModels...)
	return ops, err
}

func (m *ModelClient) createOrUpdateOps(ops []ovsdb.Operation, opModels ...OperationModel) (interface{}, []ovsdb.Operation, error) {
	doWhenFound := func(model interface{}, opModel *OperationModel) ([]ovsdb.Operation, error) {
		if opModel.OnModelUpdates != nil {
			return m.update(model, opModel)
		} else if opModel.OnModelMutations != nil {
			return m.mutate(model, opModel, ovsdb.MutateOperationInsert)
		}
		return nil, nil
	}
	doWhenNotFound := func(model interface{}, opModel *OperationModel) ([]ovsdb.Operation, error) {
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
func (m *ModelClient) Delete(opModels ...OperationModel) error {
	ops, err := m.DeleteOps(nil, opModels...)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(m.client, ops)
	return err
}

func (m *ModelClient) DeleteOps(ops []ovsdb.Operation, opModels ...OperationModel) ([]ovsdb.Operation, error) {
	doWhenFound := func(model interface{}, opModel *OperationModel) (o []ovsdb.Operation, err error) {
		if opModel.OnModelMutations != nil {
			return m.mutate(model, opModel, ovsdb.MutateOperationDelete)
		} else {
			return m.delete(model, opModel)
		}
	}
	_, ops, err := m.buildOps(ops, doWhenFound, nil, opModels...)
	return ops, err
}

type opModelToOpMapper func(model interface{}, opModel *OperationModel) (o []ovsdb.Operation, err error)

func (m *ModelClient) buildOps(ops []ovsdb.Operation, doWhenFound opModelToOpMapper, doWhenNotFound opModelToOpMapper, opModels ...OperationModel) (interface{}, []ovsdb.Operation, error) {
	if ops == nil {
		ops = []ovsdb.Operation{}
	}
	notfound := []interface{}{}
	for _, opModel := range opModels {
		if opModel.ExistingResult == nil && opModel.Model != nil {
			opModel.ExistingResult = getListFromModel(opModel.Model)
		}

		// lookup
		if opModel.ModelPredicate != nil {
			if err := m.whereCache(&opModel); err != nil {
				return nil, nil, fmt.Errorf("unable to list items for model, err: %v", err)
			}
		} else if opModel.Model != nil {
			_, err := m.get(&opModel)
			if err != nil && !errors.Is(err, client.ErrNotFound) {
				return nil, nil, fmt.Errorf("unable to get model, err: %v", err)
			}
		}

		// do updates
		var hadExistingResults bool
		err := onModels(opModel.ExistingResult, func(model interface{}) error {
			if hadExistingResults && !opModel.BulkOp {
				return fmt.Errorf("unexpectedly found multiple results for provided predicate")
			}
			hadExistingResults = true
			o, err := doWhenFound(model, &opModel)
			if err != nil {
				return err
			}
			ops = append(ops, o...)
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
func (m *ModelClient) create(opModel *OperationModel) ([]ovsdb.Operation, error) {
	uuid := getUUID(opModel.Model)
	if uuid == "" {
		setUUID(opModel.Model, BuildNamedUUID())
	}
	ops := make([]ovsdb.Operation, 0, 1)
	o, err := m.client.Create(opModel.Model)
	if err != nil {
		return nil, fmt.Errorf("unable to create model, err: %v", err)
	}

	// Add wait methods accordingly
	// ACL we would have to use external_ids + name for unique match
	// However external_ids would be a performance hit, and in one case we use
	// an empty name and external_ids for addAllowACLFromNode
	if opModel.Name != nil && o[0].Table != "ACL" {
		timeout := types.OVSDBWaitTimeout
		condition := model.Condition{
			Field:    opModel.Name,
			Function: ovsdb.ConditionEqual,
			Value:    getString(opModel.Name),
		}
		waitOps, err := m.client.Where(opModel.Model, condition).Wait(ovsdb.WaitConditionNotEqual, &timeout, opModel.Model, opModel.Name)
		if err != nil {
			return nil, err
		}
		ops = append(ops, waitOps...)
	} else if info, err := m.client.Cache().DatabaseModel().NewModelInfo(opModel.Model); err == nil {
		if name, err := info.FieldByColumn("name"); err == nil {
			objName := getString(name)
			if len(objName) > 0 {
				klog.Warningf("OVSDB Create operation detected without setting opModel Name. Name: %s, %#v",
					objName, info)
			}
		}
	}

	ops = append(ops, o...)
	klog.V(5).Infof("Create operations generated as: %+v", ops)
	return ops, nil
}

func (m *ModelClient) update(lookUpModel interface{}, opModel *OperationModel) (o []ovsdb.Operation, err error) {
	o, err = m.client.Where(lookUpModel).Update(opModel.Model, opModel.OnModelUpdates...)
	if err != nil {
		return nil, fmt.Errorf("unable to update model, err: %v", err)
	}
	klog.V(5).Infof("Update operations generated as: %+v", o)
	return o, nil
}

func (m *ModelClient) mutate(lookUpModel interface{}, opModel *OperationModel, mutator ovsdb.Mutator) (o []ovsdb.Operation, err error) {
	if opModel.OnModelMutations == nil {
		return nil, nil
	}
	modelMutations := BuildMutationsFromFields(opModel.OnModelMutations, mutator)
	if len(modelMutations) == 0 {
		return nil, nil
	}
	o, err = m.client.Where(lookUpModel).Mutate(opModel.Model, modelMutations...)
	if err != nil {
		return nil, fmt.Errorf("unable to mutate model, err: %v", err)
	}
	klog.V(5).Infof("Mutate operations generated as: %+v", o)
	return o, nil
}

func (m *ModelClient) delete(lookUpModel interface{}, opModel *OperationModel) (o []ovsdb.Operation, err error) {
	o, err = m.client.Where(lookUpModel).Delete()
	if err != nil {
		return nil, fmt.Errorf("unable to delete model, err: %v", err)
	}
	klog.V(5).Infof("Delete operations generated as: %+v", o)
	return o, nil
}

/*
 get copies the model, since this function ends up being called from update /
 mutate, and Get'ing the model will modify the object to the one currently
 existing in the DB, thus overridding all new fields we are trying to set. Do
 return the retrived object though, in case the caller needs to act on the
 object's UUID
*/
func (m *ModelClient) get(opModel *OperationModel) (interface{}, error) {
	copy := copyIndexes(opModel.Model)
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	if err := m.client.Get(ctx, copy); err != nil {
		return nil, err
	}
	uuid := getUUID(opModel.Model)
	if uuid == "" || IsNamedUUID(uuid) {
		setUUID(opModel.Model, getUUID(copy))
	}
	addToExistingResult(copy, opModel.ExistingResult)
	return copy, nil
}

func (m *ModelClient) whereCache(opModel *OperationModel) error {
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

func addToExistingResult(model interface{}, existingResult interface{}) {
	resultPtr := reflect.ValueOf(existingResult)
	resultVal := reflect.Indirect(resultPtr)
	resultVal.Set(reflect.Append(resultVal, reflect.Indirect(reflect.ValueOf(model))))
}

func getString(field interface{}) string {
	objName, ok := field.(string)
	if !ok {
		if strPtr, ok := field.(*string); ok {
			if strPtr != nil {
				objName = *strPtr
			}
		}
	}
	return objName
}

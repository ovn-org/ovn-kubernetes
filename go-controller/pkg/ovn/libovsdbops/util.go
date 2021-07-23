package libovsdbops

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sync/atomic"

	"github.com/mitchellh/copystructure"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog/v2"
)

const (
	namedUUIDPrefix = 'u'
)

var (
	UUIDField        = "UUID"
	namedUUIDCounter = rand.Uint32()
)

type ModelClient struct {
	client client.Client
}

func NewModelClient(client client.Client) ModelClient {
	return ModelClient{client: client}
}

/*
 OnReferentialModelMutation is a helper function which assigns or unassigns
 all UUIDs provided by `items` against the referential (parent) model.

 Note the following:

- items can be a slice or a unique object. The reasons for this are: a slice
  makes more sense for delete operations where the client needs to bulk delete a
  bunch of objects at once (this is done in egressip.go / egressfirewall.go when
  syncing for example). For create operations usually only one object is to be
  specified, though multiple can.

- it compares existing referential UUIDs to the ones provided through items, as
  to avoid round-trips to the OVS DB and unnecessary operations
*/
func OnReferentialModelMutation(field interface{}, mutator ovsdb.Mutator, items interface{}) []model.Mutation {
	ids := []string{}
	if reflect.TypeOf(items).Kind() == reflect.Slice {
		itemsLength := reflect.ValueOf(items).Len()
		if itemsLength == 0 {
			return nil
		}
		for i := 0; i < itemsLength; i++ {
			uuidField := reflect.ValueOf(items).Index(i).FieldByName(UUIDField)
			if uuidField.IsValid() {
				uuid := uuidField.Interface().(string)
				if uuid != "" {
					ids = append(ids, uuidField.String())
				}
			}
		}
	} else {
		uuidField := reflect.ValueOf(items).FieldByName(UUIDField)
		if uuidField.IsValid() {
			uuid := uuidField.Interface().(string)
			if uuid != "" {
				ids = append(ids, uuidField.String())
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return []model.Mutation{
		{
			Field:   field,
			Mutator: mutator,
			Value:   ids,
		},
	}
}

/*
 OperationModel is a struct which uses reflection to determine and perform
 idempotent operations against OVS DB (NB DB by default). It has a couple of
 subtle quirks which are important to note when using this package:

- If an object is specified and is to be created, ExistingResult will not
  contain the object. In fact, ExistingResult will only contain the object when
  the operations are delete/mutate/update. This is done as to give the caller
  the possibility to discern the operation performed.
*/
type OperationModel struct {
	// Model specifies the model to be created/updated/deleted. It can also be
	// used to lookup the model if solely specified, note however that only
	// indexed fields can be used. The internals of OperationModel will assign
	// the object a named UUID, if the object is to be created.
	Model interface{}
	// ModelPredicate specifies the predicate at which the lookup for the model
	// is made. This is what defines if the object exists or not, for objects
	// whos lookup needs to be made on non indexed fields.
	ModelPredicate interface{}
	// OnModelMutations specifies the mutations to be performed on the model if an
	// item exists.
	OnModelMutations func() []model.Mutation
	// OnModelUpdates specifies the model fields to be updated if an item
	// exists. If specified: Model needs to be specified too, as that is used to
	// demark which values each field to be update will have.
	OnModelUpdates []interface{}
	// ExistingResult specifies the existing results used to determine the
	// existance/non-existance of the model. This is a required field when using
	// ModelPredicate / OnModelUpdates / OnModelMutations
	ExistingResult interface{}
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

 a) if a ModelPredicate is provided; it uses a lookup of the Model in the cache,
 following the predicate function specified.

 b) if a) is not true, but OnModelUpdates is specified; it performs a direct
 update of the Model if it exists, and creates it otherwise.

 c) if a) and b) are not true, but OnModelMutations is specified; it performs a
 direct mutation of the Model if it exists, and creates it otherwise.

 d) if none of the above are true, it creates the Model.

 This function assumes that only one object is retrieved for a given predicate
 (since create/update/mutate is logically performed against one entity).
*/
func (m *ModelClient) CreateOrUpdate(opModels ...OperationModel) ([]ovsdb.OperationResult, error) {
	ops, err := m.CreateOrUpdateOps(opModels...)
	if err != nil {
		return nil, err
	}
	return TransactAndCheck(m.client, ops)
}

func (m *ModelClient) CreateOrUpdateOps(opModels ...OperationModel) ([]ovsdb.Operation, error) {
	ops := []ovsdb.Operation{}
	for _, opModel := range opModels {
		if opModel.ModelPredicate != nil {
			o, err := m.createOrUpdateWithCache(opModel)
			if err != nil {
				return nil, err
			}
			ops = append(ops, o...)
		} else if opModel.OnModelUpdates != nil {
			o, err := m.update(true, opModel.Model, opModel)
			if err != nil {
				if errors.Is(err, client.ErrNotFound) {
					o, err = m.create(opModel)
					if err != nil {
						return nil, err
					}
					ops = append(ops, o...)
				} else {
					return nil, err
				}
			} else {
				ops = append(ops, o...)
			}
		} else if opModel.OnModelMutations != nil {
			o, err := m.mutate(true, opModel.Model, opModel)
			if err != nil {
				if errors.Is(err, client.ErrNotFound) {
					o, err = m.create(opModel)
					if err != nil {
						return nil, err
					}
					ops = append(ops, o...)
				} else {
					return nil, err
				}
			} else {
				ops = append(ops, o...)
			}
		} else {
			if _, err := m.get(opModel.Model); errors.Is(err, client.ErrNotFound) {
				o, err := m.create(opModel)
				if err != nil {
					return nil, err
				}
				ops = append(ops, o...)
			}
		}
	}
	return ops, nil
}

/*
 Delete performs idempotent delete operations against libovsdb according to the
 following logic:

 a) if a ModelPredicate is provided; it uses a lookup of the Model in the cache,
 following the predicate function specified.

 b) if a) is not true, but OnModelMutations is specified; it performs a direct
 mutation of the Model if it exists

 c) if a) and b) are not true, but Model is specified; it performs a direct
 delete of the Model.

 This function does not assume that only one object is retrieved for a given
 predicate, a delete/mutate operation will be performed on all retrived models
 should the predicate return multiple.
*/
func (m *ModelClient) Delete(opModels ...OperationModel) error {
	ops, err := m.DeleteOps(opModels...)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(m.client, ops)
	return err
}

func (m *ModelClient) DeleteOps(opModels ...OperationModel) (ops []ovsdb.Operation, err error) {
	for _, opModel := range opModels {
		if opModel.ModelPredicate != nil {
			if err := m.client.WhereCache(opModel.ModelPredicate).List(opModel.ExistingResult); err != nil {
				return nil, fmt.Errorf("unable to list items for model, err: %v", err)
			}
			for i := 0; i < reflect.ValueOf(opModel.ExistingResult).Elem().Len(); i++ {
				if opModel.OnModelMutations != nil {
					o, err := m.mutate(false, reflect.ValueOf(opModel.ExistingResult).Elem().Index(i).Addr().Interface(), opModel)
					if err != nil {
						return nil, err
					}
					ops = append(ops, o...)
				} else {
					o, err := m.delete(false, reflect.ValueOf(opModel.ExistingResult).Elem().Index(i).Addr().Interface(), opModel)
					if err != nil {
						return nil, err
					}
					ops = append(ops, o...)
				}
			}
		} else if opModel.OnModelMutations != nil {
			o, err := m.mutate(true, opModel.Model, opModel)
			if err != nil {
				return nil, err
			}
			ops = append(ops, o...)
		} else if opModel.Model != nil {
			o, err := m.delete(true, opModel.Model, opModel)
			if err != nil {
				return nil, err
			}
			ops = append(ops, o...)
		}
	}
	return ops, nil
}

func (m *ModelClient) createOrUpdateWithCache(opModel OperationModel) ([]ovsdb.Operation, error) {
	if err := m.client.WhereCache(opModel.ModelPredicate).List(opModel.ExistingResult); err != nil {
		return nil, fmt.Errorf("unable to list items for model, err: %v", err)
	}
	if reflect.ValueOf(opModel.ExistingResult).Elem().Len() == 0 {
		if opModel.Model != nil {
			return m.create(opModel)
		}
	} else if reflect.ValueOf(opModel.ExistingResult).Elem().Len() == 1 {
		if opModel.OnModelMutations != nil {
			return m.mutate(false, reflect.ValueOf(opModel.ExistingResult).Elem().Index(0).Addr().Interface(), opModel)
		} else if len(opModel.OnModelUpdates) > 0 {
			return m.update(false, reflect.ValueOf(opModel.ExistingResult).Elem().Index(0).Addr().Interface(), opModel)
		}
		return nil, nil
	}
	return nil, fmt.Errorf("error updating model: multiple results found for provided predicate")
}

/*
 create does a bit more than just "create". create needs to set the generated
 UUID (because if this function is called we know the item does not exists yet)
 then create the item.
*/
func (m *ModelClient) create(opModel OperationModel) ([]ovsdb.Operation, error) {
	v := reflect.ValueOf(opModel.Model).Elem().FieldByName(UUIDField)
	if v.IsValid() {
		v.SetString(BuildNamedUUID())
	}
	o, err := m.client.Create(opModel.Model)
	if err != nil {
		return nil, fmt.Errorf("unable to create model, err: %v", err)
	}
	klog.V(5).Infof("Create operations generated as: %+v", o)
	return o, nil
}

/*
 update either checks / validates that the item exists in the DB (needed in case
 WhereCache using a ModelPredicate was not called) and retrieved the UUID before
 performing the Update operation, this is done for performance reasons on OVS DB
 server's side. It also add the found item to ExistingResults as a
 "notification" on the callers end that indeed an update operation was
 performed, and not a create.
*/
func (m *ModelClient) update(checkExists bool, lookUpModel interface{}, opModel OperationModel) (o []ovsdb.Operation, err error) {
	if checkExists {
		if lookUpModel, err = m.get(lookUpModel); err != nil {
			return nil, err
		}
		m.addToExistingResult(lookUpModel, opModel.ExistingResult)
	}
	o, err = m.client.Where(lookUpModel).Update(opModel.Model, opModel.OnModelUpdates...)
	if err != nil {
		return nil, fmt.Errorf("unable to update model, err: %v", err)
	}
	klog.V(5).Infof("Update operations generated as: %+v", o)
	return o, nil
}

// see: update
func (m *ModelClient) mutate(checkExists bool, lookUpModel interface{}, opModel OperationModel) (o []ovsdb.Operation, err error) {
	modelMutations := opModel.OnModelMutations()
	if modelMutations == nil {
		return nil, nil
	}
	if checkExists {
		if lookUpModel, err = m.get(lookUpModel); err != nil {
			return nil, err
		}
		m.addToExistingResult(lookUpModel, opModel.ExistingResult)
	}
	o, err = m.client.Where(lookUpModel).Mutate(opModel.Model, modelMutations...)
	if err != nil {
		return nil, fmt.Errorf("unable to mutate model, err: %v", err)
	}
	klog.V(5).Infof("Mutate operations generated as: %+v", o)
	return o, nil
}

// see: update
func (m *ModelClient) delete(checkExists bool, lookUpModel interface{}, opModel OperationModel) (o []ovsdb.Operation, err error) {
	if checkExists {
		if lookUpModel, err = m.get(lookUpModel); errors.Is(err, client.ErrNotFound) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		m.addToExistingResult(lookUpModel, opModel.ExistingResult)
	}
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
func (m *ModelClient) get(model interface{}) (interface{}, error) {
	tmp, err := copystructure.Copy(model)
	if err != nil {
		return nil, fmt.Errorf("unable to copy model: %v for get", err)
	}
	if err := m.client.Get(tmp); err != nil {
		return nil, err
	}
	return tmp, nil
}

func (m *ModelClient) addToExistingResult(model interface{}, existingResult interface{}) {
	if existingResult != nil {
		resultPtr := reflect.ValueOf(existingResult)
		resultVal := reflect.Indirect(resultPtr)
		resultVal.Set(reflect.Append(resultVal, reflect.Indirect(reflect.ValueOf(model))))
	}
}

func TransactAndCheck(client client.Client, ops []ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	if len(ops) <= 0 {
		return []ovsdb.OperationResult{{}}, nil
	}

	ctx, cancel := context.WithTimeout(context.TODO(), util.OVSDBTimeout)
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

// IsNamedUUID checks if the passed id is a named-uuid built with
// BuildNamedUUID
func IsNamedUUID(id string) bool {
	return id != "" && id[0] == namedUUIDPrefix
}

// BuildNamedUUID builds an id that can be used as a named-uuid
// as per OVSDB rfc 7047 section 5.1
func BuildNamedUUID() string {
	return fmt.Sprintf("%c%010d", namedUUIDPrefix, atomic.AddUint32(&namedUUIDCounter, 1))
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

	s := reflect.ValueOf(models)
	if s.Kind() != reflect.Slice {
		panic("models given a non-slice type")
	}

	if s.IsNil() {
		return results, nil
	}

	namedModelMap := map[string]model.Model{}
	for i := 0; i < s.Len(); i++ {
		model := s.Index(i).Interface()
		uuid := getUUID(model)
		if IsNamedUUID(uuid) {
			namedModelMap[uuid] = model
		}
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

func getUUID(model model.Model) string {
	switch t := model.(type) {
	case *nbdb.LoadBalancer:
		return t.UUID
	default:
		panic("getUUID: unknown model")
	}
}

func setUUID(model model.Model, uuid string) {
	switch t := model.(type) {
	case *nbdb.LoadBalancer:
		t.UUID = uuid
	default:
		panic("setUUID: unknown model")
	}
}

func SliceHasStringItem(slice []string, item string) bool {
	for _, i := range slice {
		if i == item {
			return true
		}
	}
	return false
}

package client

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/go-logr/logr"
	"github.com/ovn-org/libovsdb/cache"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// API defines basic operations to interact with the database
type API interface {
	// List populates a slice of Models objects based on their type
	// The function parameter must be a pointer to a slice of Models
	// Models can be structs or pointers to structs
	// If the slice is null, the entire cache will be copied into the slice
	// If it has a capacity != 0, only 'capacity' elements will be filled in
	List(ctx context.Context, result interface{}) error

	// Create a Conditional API from a Function that is used to filter cached data
	// The function must accept a Model implementation and return a boolean. E.g:
	// ConditionFromFunc(func(l *LogicalSwitch) bool { return l.Enabled })
	WhereCache(predicate interface{}) ConditionalAPI

	// Create a ConditionalAPI from a Model's index data, where operations
	// apply to elements that match the values provided in one or more
	// model.Models according to the indexes. All provided Models must be
	// the same type or an error will be generated when operations are
	// are performed on the ConditionalAPI.
	Where(...model.Model) ConditionalAPI

	// WhereAny creates a ConditionalAPI from a list of Conditions where
	// operations apply to elements that match any (eg, logical OR) of the
	// conditions.
	WhereAny(model.Model, ...model.Condition) ConditionalAPI

	// WhereAll creates a ConditionalAPI from a list of Conditions where
	// operations apply to elements that match all (eg, logical AND) of the
	// conditions.
	WhereAll(model.Model, ...model.Condition) ConditionalAPI

	// Get retrieves a model from the cache
	// The way the object will be fetch depends on the data contained in the
	// provided model and the indexes defined in the associated schema
	// For more complex ways of searching for elements in the cache, the
	// preferred way is Where({condition}).List()
	Get(context.Context, model.Model) error

	// Create returns the operation needed to add the model(s) to the Database
	// Only fields with non-default values will be added to the transaction. If
	// the field associated with column "_uuid" has some content other than a
	// UUID, it will be treated as named-uuid
	Create(...model.Model) ([]ovsdb.Operation, error)
}

// ConditionalAPI is an interface used to perform operations that require / use Conditions
type ConditionalAPI interface {
	// List uses the condition to search on the cache and populates
	// the slice of Models objects based on their type
	List(ctx context.Context, result interface{}) error

	// Mutate returns the operations needed to perform the mutation specified
	// By the model and the list of Mutation objects
	// Depending on the Condition, it might return one or many operations
	Mutate(model.Model, ...model.Mutation) ([]ovsdb.Operation, error)

	// Update returns the operations needed to update any number of rows according
	// to the data in the given model.
	// By default, all the non-default values contained in model will be updated.
	// Optional fields can be passed (pointer to fields in the model) to select the
	// the fields to be updated
	Update(model.Model, ...interface{}) ([]ovsdb.Operation, error)

	// Delete returns the Operations needed to delete the models selected via the condition
	Delete() ([]ovsdb.Operation, error)

	// Wait returns the operations needed to perform the wait specified
	// by the until condition, timeout, row and columns based on provided parameters.
	Wait(ovsdb.WaitCondition, *int, model.Model, ...interface{}) ([]ovsdb.Operation, error)
}

// ErrWrongType is used to report the user provided parameter has the wrong type
type ErrWrongType struct {
	inputType reflect.Type
	reason    string
}

func (e *ErrWrongType) Error() string {
	return fmt.Sprintf("Wrong parameter type (%s): %s", e.inputType, e.reason)
}

// ErrNotFound is used to inform the object or table was not found in the cache
var ErrNotFound = errors.New("object not found")

// api struct implements both API and ConditionalAPI
// Where() can be used to create a ConditionalAPI api
type api struct {
	cache  *cache.TableCache
	cond   Conditional
	logger *logr.Logger
}

// List populates a slice of Models given as parameter based on the configured Condition
func (a api) List(ctx context.Context, result interface{}) error {
	resultPtr := reflect.ValueOf(result)
	if resultPtr.Type().Kind() != reflect.Ptr {
		return &ErrWrongType{resultPtr.Type(), "Expected pointer to slice of valid Models"}
	}

	resultVal := reflect.Indirect(resultPtr)
	if resultVal.Type().Kind() != reflect.Slice {
		return &ErrWrongType{resultPtr.Type(), "Expected pointer to slice of valid Models"}
	}

	// List accepts a slice of Models that can be either structs or pointer to
	// structs
	var appendValue func(reflect.Value)
	var m model.Model
	if resultVal.Type().Elem().Kind() == reflect.Ptr {
		m = reflect.New(resultVal.Type().Elem().Elem()).Interface()
		appendValue = func(v reflect.Value) {
			resultVal.Set(reflect.Append(resultVal, v))
		}
	} else {
		m = reflect.New(resultVal.Type().Elem()).Interface()
		appendValue = func(v reflect.Value) {
			resultVal.Set(reflect.Append(resultVal, reflect.Indirect(v)))
		}
	}

	table, err := a.getTableFromModel(m)
	if err != nil {
		return err
	}

	if a.cond != nil && a.cond.Table() != table {
		return &ErrWrongType{resultPtr.Type(),
			fmt.Sprintf("Table derived from input type (%s) does not match Table from Condition (%s)", table, a.cond.Table())}
	}

	tableCache := a.cache.Table(table)
	if tableCache == nil {
		return ErrNotFound
	}

	var rows map[string]model.Model
	if a.cond != nil {
		rows, err = a.cond.Matches()
		if err != nil {
			return err
		}
	} else {
		rows = tableCache.Rows()
	}
	// If given a null slice, fill it in the cache table completely, if not, just up to
	// its capability.
	if resultVal.IsNil() || resultVal.Cap() == 0 {
		resultVal.Set(reflect.MakeSlice(resultVal.Type(), 0, len(rows)))
	}
	i := resultVal.Len()
	maxCap := resultVal.Cap()

	for _, row := range rows {
		if i >= maxCap {
			break
		}
		appendValue(reflect.ValueOf(row))
		i++
	}

	return nil
}

// Where returns a conditionalAPI based on model indexes. All provided models
// must be the same type.
func (a api) Where(models ...model.Model) ConditionalAPI {
	return newConditionalAPI(a.cache, a.conditionFromModels(models), a.logger)
}

// WhereAny returns a conditionalAPI based on a Condition list that matches any
// of the conditions individually
func (a api) WhereAny(m model.Model, cond ...model.Condition) ConditionalAPI {
	return newConditionalAPI(a.cache, a.conditionFromExplicitConditions(false, m, cond...), a.logger)
}

// WhereAll returns a conditionalAPI based on a Condition list that matches all
// of the conditions together
func (a api) WhereAll(m model.Model, cond ...model.Condition) ConditionalAPI {
	return newConditionalAPI(a.cache, a.conditionFromExplicitConditions(true, m, cond...), a.logger)
}

// WhereCache returns a conditionalAPI based a Predicate
func (a api) WhereCache(predicate interface{}) ConditionalAPI {
	return newConditionalAPI(a.cache, a.conditionFromFunc(predicate), a.logger)
}

// Conditional interface implementation
// FromFunc returns a Condition from a function
func (a api) conditionFromFunc(predicate interface{}) Conditional {
	table, err := a.getTableFromFunc(predicate)
	if err != nil {
		return newErrorConditional(err)
	}

	condition, err := newPredicateConditional(table, a.cache, predicate)
	if err != nil {
		return newErrorConditional(err)
	}
	return condition
}

// conditionFromModels returns a Conditional from one or more models.
func (a api) conditionFromModels(models []model.Model) Conditional {
	if len(models) == 0 {
		return newErrorConditional(fmt.Errorf("at least one model required"))
	}
	tableName, err := a.getTableFromModel(models[0])
	if tableName == "" {
		return newErrorConditional(err)
	}
	conditional, err := newEqualityConditional(tableName, a.cache, models)
	if err != nil {
		return newErrorConditional(err)
	}
	return conditional
}

// conditionFromExplicitConditions returns a Conditional from a model and a set
// of explicit conditions. If matchAll is true, then models that match all the given
// conditions are selected by the Conditional. If matchAll is false, then any model
// that matches one of the conditions is selected.
func (a api) conditionFromExplicitConditions(matchAll bool, m model.Model, cond ...model.Condition) Conditional {
	if len(cond) == 0 {
		return newErrorConditional(fmt.Errorf("at least one condition is required"))
	}
	tableName, err := a.getTableFromModel(m)
	if tableName == "" {
		return newErrorConditional(err)
	}
	conditional, err := newExplicitConditional(tableName, a.cache, matchAll, m, cond...)
	if err != nil {
		return newErrorConditional(err)
	}
	return conditional
}

// Get is a generic Get function capable of returning (through a provided pointer)
// a instance of any row in the cache.
// 'result' must be a pointer to an Model that exists in the ClientDBModel
//
// The way the cache is searched depends on the fields already populated in 'result'
// Any table index (including _uuid) will be used for comparison
func (a api) Get(ctx context.Context, m model.Model) error {
	table, err := a.getTableFromModel(m)
	if err != nil {
		return err
	}

	tableCache := a.cache.Table(table)
	if tableCache == nil {
		return ErrNotFound
	}

	_, found, err := tableCache.RowByModel(m)
	if err != nil {
		return err
	} else if found == nil {
		return ErrNotFound
	}

	model.CloneInto(found, m)

	return nil
}

// Create is a generic function capable of creating any row in the DB
// A valid Model (pointer to object) must be provided.
func (a api) Create(models ...model.Model) ([]ovsdb.Operation, error) {
	var operations []ovsdb.Operation

	for _, model := range models {
		var realUUID, namedUUID string
		var err error

		tableName, err := a.getTableFromModel(model)
		if err != nil {
			return nil, err
		}

		// Read _uuid field, and use it as named-uuid
		info, err := a.cache.DatabaseModel().NewModelInfo(model)
		if err != nil {
			return nil, err
		}
		if uuid, err := info.FieldByColumn("_uuid"); err == nil {
			tmpUUID := uuid.(string)
			if ovsdb.IsNamedUUID(tmpUUID) {
				namedUUID = tmpUUID
			} else if ovsdb.IsValidUUID(tmpUUID) {
				realUUID = tmpUUID
			}
		} else {
			return nil, err
		}

		row, err := a.cache.Mapper().NewRow(info)
		if err != nil {
			return nil, err
		}
		// UUID is given in the operation, not the object
		delete(row, "_uuid")

		operations = append(operations, ovsdb.Operation{
			Op:       ovsdb.OperationInsert,
			Table:    tableName,
			Row:      row,
			UUID:     realUUID,
			UUIDName: namedUUID,
		})
	}
	return operations, nil
}

// Mutate returns the operations needed to transform the one Model into another one
func (a api) Mutate(model model.Model, mutationObjs ...model.Mutation) ([]ovsdb.Operation, error) {
	var mutations []ovsdb.Mutation
	var operations []ovsdb.Operation

	if len(mutationObjs) < 1 {
		return nil, fmt.Errorf("at least one Mutation must be provided")
	}

	tableName := a.cache.DatabaseModel().FindTable(reflect.ValueOf(model).Type())
	if tableName == "" {
		return nil, fmt.Errorf("table not found for object")
	}
	table := a.cache.Mapper().Schema.Table(tableName)
	if table == nil {
		return nil, fmt.Errorf("schema error: table not found in Database Model for type %s", reflect.TypeOf(model))
	}

	conditions, err := a.cond.Generate()
	if err != nil {
		return nil, err
	}

	info, err := a.cache.DatabaseModel().NewModelInfo(model)
	if err != nil {
		return nil, err
	}

	for _, mobj := range mutationObjs {
		col, err := info.ColumnByPtr(mobj.Field)
		if err != nil {
			return nil, err
		}

		mutation, err := a.cache.Mapper().NewMutation(info, col, mobj.Mutator, mobj.Value)
		if err != nil {
			return nil, err
		}
		mutations = append(mutations, *mutation)
	}
	for _, condition := range conditions {
		operations = append(operations,
			ovsdb.Operation{
				Op:        ovsdb.OperationMutate,
				Table:     tableName,
				Mutations: mutations,
				Where:     condition,
			},
		)
	}

	return operations, nil
}

// Update is a generic function capable of updating any mutable field in any row in the database
// Additional fields can be passed (variadic opts) to indicate fields to be updated
// All immutable fields will be ignored
func (a api) Update(model model.Model, fields ...interface{}) ([]ovsdb.Operation, error) {
	var operations []ovsdb.Operation
	table, err := a.getTableFromModel(model)
	if err != nil {
		return nil, err
	}
	tableSchema := a.cache.Mapper().Schema.Table(table)
	info, err := a.cache.DatabaseModel().NewModelInfo(model)
	if err != nil {
		return nil, err
	}

	if len(fields) > 0 {
		for _, f := range fields {
			colName, err := info.ColumnByPtr(f)
			if err != nil {
				return nil, err
			}
			if !tableSchema.Columns[colName].Mutable() {
				return nil, fmt.Errorf("unable to update field %s of table %s as it is not mutable", colName, table)
			}
		}
	}

	conditions, err := a.cond.Generate()
	if err != nil {
		return nil, err
	}

	row, err := a.cache.Mapper().NewRow(info, fields...)
	if err != nil {
		return nil, err
	}

	for colName, column := range tableSchema.Columns {
		if !column.Mutable() {
			a.logger.V(2).Info("removing immutable field", "name", colName)
			delete(row, colName)
		}
	}
	delete(row, "_uuid")

	if len(row) == 0 {
		return nil, fmt.Errorf("attempted to update using an empty row. please check that all fields you wish to update are mutable")
	}

	for _, condition := range conditions {
		operations = append(operations,
			ovsdb.Operation{
				Op:    ovsdb.OperationUpdate,
				Table: table,
				Row:   row,
				Where: condition,
			},
		)
	}
	return operations, nil
}

// Delete returns the Operation needed to delete the selected models from the database
func (a api) Delete() ([]ovsdb.Operation, error) {
	var operations []ovsdb.Operation
	conditions, err := a.cond.Generate()
	if err != nil {
		return nil, err
	}

	for _, condition := range conditions {
		operations = append(operations,
			ovsdb.Operation{
				Op:    ovsdb.OperationDelete,
				Table: a.cond.Table(),
				Where: condition,
			},
		)
	}

	return operations, nil
}

func (a api) Wait(untilConFun ovsdb.WaitCondition, timeout *int, model model.Model, fields ...interface{}) ([]ovsdb.Operation, error) {
	var operations []ovsdb.Operation

	/*
		    Ref: https://datatracker.ietf.org/doc/html/rfc7047.txt#section-5.2.6

			lb := &nbdb.LoadBalancer{}
			condition := model.Condition{
				Field:    &lb.Name,
				Function: ovsdb.ConditionEqual,
				Value:    "lbName",
			}
			timeout0 := 0
			client.Where(lb, condition).Wait(
				ovsdb.WaitConditionNotEqual, // Until
				&timeout0, // Timeout
				&lb, // Row (and Table)
				&lb.Name, // Cols (aka fields)
			)
	*/

	conditions, err := a.cond.Generate()
	if err != nil {
		return nil, err
	}

	table, err := a.getTableFromModel(model)
	if err != nil {
		return nil, err
	}

	info, err := a.cache.DatabaseModel().NewModelInfo(model)
	if err != nil {
		return nil, err
	}

	var columnNames []string
	if len(fields) > 0 {
		columnNames = make([]string, 0, len(fields))
		for _, f := range fields {
			colName, err := info.ColumnByPtr(f)
			if err != nil {
				return nil, err
			}
			columnNames = append(columnNames, colName)
		}
	}

	row, err := a.cache.Mapper().NewRow(info, fields...)
	if err != nil {
		return nil, err
	}
	rows := []ovsdb.Row{row}

	for _, condition := range conditions {
		operation := ovsdb.Operation{
			Op:      ovsdb.OperationWait,
			Table:   table,
			Where:   condition,
			Until:   string(untilConFun),
			Columns: columnNames,
			Rows:    rows,
		}

		if timeout != nil {
			operation.Timeout = timeout
		}

		operations = append(operations, operation)
	}

	return operations, nil
}

// getTableFromModel returns the table name from a Model object after performing
// type verifications on the model
func (a api) getTableFromModel(m interface{}) (string, error) {
	if _, ok := m.(model.Model); !ok {
		return "", &ErrWrongType{reflect.TypeOf(m), "Type does not implement Model interface"}
	}
	table := a.cache.DatabaseModel().FindTable(reflect.TypeOf(m))
	if table == "" {
		return "", &ErrWrongType{reflect.TypeOf(m), "Model not found in Database Model"}
	}
	return table, nil
}

// getTableFromModel returns the table name from a the predicate after performing
// type verifications
func (a api) getTableFromFunc(predicate interface{}) (string, error) {
	predType := reflect.TypeOf(predicate)
	if predType == nil || predType.Kind() != reflect.Func {
		return "", &ErrWrongType{predType, "Expected function"}
	}
	if predType.NumIn() != 1 || predType.NumOut() != 1 || predType.Out(0).Kind() != reflect.Bool {
		return "", &ErrWrongType{predType, "Expected func(Model) bool"}
	}

	modelInterface := reflect.TypeOf((*model.Model)(nil)).Elem()
	modelType := predType.In(0)
	if !modelType.Implements(modelInterface) {
		return "", &ErrWrongType{predType,
			fmt.Sprintf("Type %s does not implement Model interface", modelType.String())}
	}

	table := a.cache.DatabaseModel().FindTable(modelType)
	if table == "" {
		return "", &ErrWrongType{predType,
			fmt.Sprintf("Model %s not found in Database Model", modelType.String())}
	}
	return table, nil
}

// newAPI returns a new API to interact with the database
func newAPI(cache *cache.TableCache, logger *logr.Logger) API {
	return api{
		cache:  cache,
		logger: logger,
	}
}

// newConditionalAPI returns a new ConditionalAPI to interact with the database
func newConditionalAPI(cache *cache.TableCache, cond Conditional, logger *logr.Logger) ConditionalAPI {
	return api{
		cache:  cache,
		cond:   cond,
		logger: logger,
	}
}

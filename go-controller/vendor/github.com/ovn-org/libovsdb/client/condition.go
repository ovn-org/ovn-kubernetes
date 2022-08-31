package client

import (
	"fmt"
	"reflect"

	"github.com/ovn-org/libovsdb/cache"
	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// Conditional is the interface used by the ConditionalAPI to match on cache objects
// and generate ovsdb conditions
type Conditional interface {
	// Generate returns a list of lists of conditions to be used in Operations
	// Each element in the (outer) list corresponds to an operation
	Generate() ([][]ovsdb.Condition, error)
	// Returns the models that match the conditions
	Matches() (map[string]model.Model, error)
	// returns the table that this condition is associated with
	Table() string
}

func generateConditionsFromModels(dbModel model.DatabaseModel, models map[string]model.Model) ([][]ovsdb.Condition, error) {
	anyConditions := make([][]ovsdb.Condition, 0, len(models))
	for _, model := range models {
		info, err := dbModel.NewModelInfo(model)
		if err != nil {
			return nil, err
		}
		allConditions, err := dbModel.Mapper.NewEqualityCondition(info)
		if err != nil {
			return nil, err
		}
		anyConditions = append(anyConditions, allConditions)
	}
	return anyConditions, nil
}

func generateOvsdbConditionsFromModelConditions(dbModel model.DatabaseModel, info *mapper.Info, conditions []model.Condition, singleOp bool) ([][]ovsdb.Condition, error) {
	anyConditions := [][]ovsdb.Condition{}
	if singleOp {
		anyConditions = append(anyConditions, []ovsdb.Condition{})
	}
	for _, condition := range conditions {
		ovsdbCond, err := dbModel.Mapper.NewCondition(info, condition.Field, condition.Function, condition.Value)
		if err != nil {
			return nil, err
		}
		allConditions := []ovsdb.Condition{*ovsdbCond}
		if singleOp {
			anyConditions[0] = append(anyConditions[0], allConditions...)
		} else {
			anyConditions = append(anyConditions, allConditions)
		}
	}
	return anyConditions, nil
}

// equalityConditional uses the indexes available in a provided model to find a
// matching model in the database.
type equalityConditional struct {
	tableName string
	models    []model.Model
	cache     *cache.TableCache
}

func (c *equalityConditional) Table() string {
	return c.tableName
}

// Returns the models that match the indexes available through the provided
// model.
func (c *equalityConditional) Matches() (map[string]model.Model, error) {
	tableCache := c.cache.Table(c.tableName)
	if tableCache == nil {
		return nil, ErrNotFound
	}
	return tableCache.RowsByModels(c.models)
}

// Generate conditions based on the equality of the first available index. If
// the index can be matched against a model in the cache, the condition will be
// based on the UUID of the found model. Otherwise, the conditions will be based
// on the index.
func (c *equalityConditional) Generate() ([][]ovsdb.Condition, error) {
	models, err := c.Matches()
	if err != nil && err != ErrNotFound {
		return nil, err
	}
	if len(models) == 0 {
		// no cache hits, generate condition from models we were given
		modelMap := make(map[string]model.Model, len(c.models))
		for i, m := range c.models {
			// generateConditionsFromModels() ignores the map keys
			// so just use the range index
			modelMap[fmt.Sprintf("%d", i)] = m
		}
		return generateConditionsFromModels(c.cache.DatabaseModel(), modelMap)
	}
	return generateConditionsFromModels(c.cache.DatabaseModel(), models)
}

// NewEqualityCondition creates a new equalityConditional
func newEqualityConditional(table string, cache *cache.TableCache, models []model.Model) (Conditional, error) {
	return &equalityConditional{
		tableName: table,
		models:    models,
		cache:     cache,
	}, nil
}

// explicitConditional generates conditions based on the provided Condition list
type explicitConditional struct {
	tableName     string
	anyConditions [][]ovsdb.Condition
	cache         *cache.TableCache
}

func (c *explicitConditional) Table() string {
	return c.tableName
}

// Returns the models that match the conditions
func (c *explicitConditional) Matches() (map[string]model.Model, error) {
	tableCache := c.cache.Table(c.tableName)
	if tableCache == nil {
		return nil, ErrNotFound
	}
	found := map[string]model.Model{}
	for _, allConditions := range c.anyConditions {
		models, err := tableCache.RowsByCondition(allConditions)
		if err != nil {
			return nil, err
		}
		for uuid, model := range models {
			found[uuid] = model
		}
	}
	return found, nil
}

// Generate returns conditions based on the provided Condition list
func (c *explicitConditional) Generate() ([][]ovsdb.Condition, error) {
	models, err := c.Matches()
	if err != nil && err != ErrNotFound {
		return nil, err
	}
	if len(models) == 0 {
		// no cache hits, return conditions we were given
		return c.anyConditions, nil
	}
	return generateConditionsFromModels(c.cache.DatabaseModel(), models)
}

// newExplicitConditional creates a new explicitConditional
func newExplicitConditional(table string, cache *cache.TableCache, matchAll bool, model model.Model, cond ...model.Condition) (Conditional, error) {
	dbModel := cache.DatabaseModel()
	info, err := dbModel.NewModelInfo(model)
	if err != nil {
		return nil, err
	}
	anyConditions, err := generateOvsdbConditionsFromModelConditions(dbModel, info, cond, matchAll)
	if err != nil {
		return nil, err
	}
	return &explicitConditional{
		tableName:     table,
		anyConditions: anyConditions,
		cache:         cache,
	}, nil
}

// predicateConditional is a Conditional that calls a provided function pointer
// to match on models.
type predicateConditional struct {
	tableName string
	predicate interface{}
	cache     *cache.TableCache
}

// matches returns the result of the execution of the predicate
// Type verifications are not performed
// Returns the models that match the conditions
func (c *predicateConditional) Matches() (map[string]model.Model, error) {
	tableCache := c.cache.Table(c.tableName)
	if tableCache == nil {
		return nil, ErrNotFound
	}
	found := map[string]model.Model{}
	// run the predicate on a shallow copy of the models for speed and only
	// clone the matches
	for u, m := range tableCache.RowsShallow() {
		ret := reflect.ValueOf(c.predicate).Call([]reflect.Value{reflect.ValueOf(m)})
		if ret[0].Bool() {
			found[u] = model.Clone(m)
		}
	}
	return found, nil
}

func (c *predicateConditional) Table() string {
	return c.tableName
}

// generate returns a list of conditions that match, by _uuid equality, all the objects that
// match the predicate
func (c *predicateConditional) Generate() ([][]ovsdb.Condition, error) {
	models, err := c.Matches()
	if err != nil {
		return nil, err
	}
	return generateConditionsFromModels(c.cache.DatabaseModel(), models)
}

// newPredicateConditional creates a new predicateConditional
func newPredicateConditional(table string, cache *cache.TableCache, predicate interface{}) (Conditional, error) {
	return &predicateConditional{
		tableName: table,
		predicate: predicate,
		cache:     cache,
	}, nil
}

// errorConditional is a conditional that encapsulates an error
// It is used to delay the reporting of errors from conditional creation to API method call
type errorConditional struct {
	err error
}

func (e *errorConditional) Matches() (map[string]model.Model, error) {
	return nil, e.err
}

func (e *errorConditional) Table() string {
	return ""
}

func (e *errorConditional) Generate() ([][]ovsdb.Condition, error) {
	return nil, e.err
}

func newErrorConditional(err error) Conditional {
	return &errorConditional{
		err: fmt.Errorf("conditionerror: %s", err.Error()),
	}
}

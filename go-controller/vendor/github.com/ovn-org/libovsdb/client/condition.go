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
	// matches returns true if a model matches the condition
	Matches(m model.Model) (bool, error)
	// returns the table that this condition is associated with
	Table() string
}

// equalityConditional uses the information available in a model to generate conditions
// The conditions are based on the equality of the first available index.
// The priority of indexes is: uuid, {schema index}
type equalityConditional struct {
	mapper    *mapper.Mapper
	tableName string
	model     model.Model
	singleOp  bool
}

func (c *equalityConditional) Matches(m model.Model) (bool, error) {
	return c.mapper.EqualFields(c.tableName, c.model, m)
}

func (c *equalityConditional) Table() string {
	return c.tableName
}

// Generate returns a condition based on the model and the field pointers
func (c *equalityConditional) Generate() ([][]ovsdb.Condition, error) {
	var result [][]ovsdb.Condition

	conds, err := c.mapper.NewEqualityCondition(c.tableName, c.model)
	if err != nil {
		return nil, err
	}
	if c.singleOp {
		result = append(result, conds)
	} else {
		for _, c := range conds {
			result = append(result, []ovsdb.Condition{c})
		}
	}
	return result, nil
}

// NewEqualityCondition creates a new equalityConditional
func newEqualityConditional(mapper *mapper.Mapper, table string, all bool, model model.Model, fields ...interface{}) (Conditional, error) {
	return &equalityConditional{
		mapper:    mapper,
		tableName: table,
		model:     model,
		singleOp:  all,
	}, nil
}

// explicitConditional generates conditions based on the provided Condition list
type explicitConditional struct {
	mapper     *mapper.Mapper
	tableName  string
	model      model.Model
	conditions []model.Condition
	singleOp   bool
}

func (c *explicitConditional) Matches(m model.Model) (bool, error) {
	return false, fmt.Errorf("cannot perform cache comparisons using explicit conditions")
}

func (c *explicitConditional) Table() string {
	return c.tableName
}

// Generate returns a condition based on the model and the field pointers
func (c *explicitConditional) Generate() ([][]ovsdb.Condition, error) {
	var result [][]ovsdb.Condition
	var conds []ovsdb.Condition

	for _, cond := range c.conditions {
		ovsdbCond, err := c.mapper.NewCondition(c.tableName, c.model, cond.Field, cond.Function, cond.Value)
		if err != nil {
			return nil, err
		}
		if c.singleOp {
			conds = append(conds, *ovsdbCond)
		} else {
			result = append(result, []ovsdb.Condition{*ovsdbCond})
		}

	}
	if c.singleOp {
		result = append(result, conds)
	}
	return result, nil
}

// newIndexCondition creates a new equalityConditional
func newExplicitConditional(mapper *mapper.Mapper, table string, all bool, model model.Model, cond ...model.Condition) (Conditional, error) {
	return &explicitConditional{
		mapper:     mapper,
		tableName:  table,
		model:      model,
		conditions: cond,
		singleOp:   all,
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
func (c *predicateConditional) Matches(model model.Model) (bool, error) {
	ret := reflect.ValueOf(c.predicate).Call([]reflect.Value{reflect.ValueOf(model)})
	return ret[0].Bool(), nil
}

func (c *predicateConditional) Table() string {
	return c.tableName
}

// generate returns a list of conditions that match, by _uuid equality, all the objects that
// match the predicate
func (c *predicateConditional) Generate() ([][]ovsdb.Condition, error) {
	allConditions := make([][]ovsdb.Condition, 0)
	tableCache := c.cache.Table(c.tableName)
	if tableCache == nil {
		return nil, ErrNotFound
	}
	for _, row := range tableCache.Rows() {
		elem := tableCache.Row(row)
		match, err := c.Matches(elem)
		if err != nil {
			return nil, err
		}
		if match {
			elemCond, err := c.cache.Mapper().NewEqualityCondition(c.tableName, elem)
			if err != nil {
				return nil, err
			}
			allConditions = append(allConditions, elemCond)
		}
	}
	return allConditions, nil
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

func (e *errorConditional) Matches(model.Model) (bool, error) {
	return false, e.err
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

package server

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/cache"
	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// Database abstracts database operations from ovsdb
type Database interface {
	CreateDatabase(name string, model *ovsdb.DatabaseSchema) error
	Exists(name string) bool
	Transact(database string, operations []ovsdb.Operation) ([]ovsdb.OperationResult, ovsdb.TableUpdates)
	Select(database string, table string, where []ovsdb.Condition, columns []string) ovsdb.OperationResult
	Insert(database string, table string, uuidName string, row ovsdb.Row) (ovsdb.OperationResult, ovsdb.TableUpdates)
	Update(database, table string, where []ovsdb.Condition, row ovsdb.Row) (ovsdb.OperationResult, ovsdb.TableUpdates)
	Mutate(database, table string, where []ovsdb.Condition, mutations []ovsdb.Mutation) (ovsdb.OperationResult, ovsdb.TableUpdates)
	Delete(database, table string, where []ovsdb.Condition) (ovsdb.OperationResult, ovsdb.TableUpdates)
	Wait(database, table string, timeout int, conditions []ovsdb.Condition, columns []string, until string, rows []ovsdb.Row) ovsdb.OperationResult
	Commit(database, table string, durable bool) ovsdb.OperationResult
	Abort(database, table string) ovsdb.OperationResult
	Comment(database, table string, comment string) ovsdb.OperationResult
	Assert(database, table, lock string) ovsdb.OperationResult
}

type inMemoryDatabase struct {
	databases map[string]*cache.TableCache
	models    map[string]*model.DBModel
	mutex     sync.RWMutex
}

func NewInMemoryDatabase(models map[string]*model.DBModel) Database {
	return &inMemoryDatabase{
		databases: make(map[string]*cache.TableCache),
		models:    models,
		mutex:     sync.RWMutex{},
	}
}

func (db *inMemoryDatabase) CreateDatabase(name string, schema *ovsdb.DatabaseSchema) error {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	var mo *model.DBModel
	var ok bool
	if mo, ok = db.models[schema.Name]; !ok {
		return fmt.Errorf("no db model provided for schema with name %s", name)
	}
	database, err := cache.NewTableCache(schema, mo, nil)
	if err != nil {
		return nil
	}
	db.databases[name] = database
	return nil
}

func (db *inMemoryDatabase) Exists(name string) bool {
	db.mutex.RLock()
	defer db.mutex.RUnlock()
	_, ok := db.databases[name]
	return ok
}

func (db *inMemoryDatabase) Transact(name string, operations []ovsdb.Operation) ([]ovsdb.OperationResult, ovsdb.TableUpdates) {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	results := []ovsdb.OperationResult{}
	updates := make(ovsdb.TableUpdates)
	for _, op := range operations {
		switch op.Op {
		case ovsdb.OperationInsert:
			r, tu := db.Insert(name, op.Table, op.UUIDName, op.Row)
			results = append(results, r)
			if tu != nil {
				updates.Merge(tu)
			}
		case ovsdb.OperationSelect:
			r := db.Select(name, op.Table, op.Where, op.Columns)
			results = append(results, r)
		case ovsdb.OperationUpdate:
			r, tu := db.Update(name, op.Table, op.Where, op.Row)
			results = append(results, r)
			if tu != nil {
				updates.Merge(tu)
			}
		case ovsdb.OperationMutate:
			r, tu := db.Mutate(name, op.Table, op.Where, op.Mutations)
			results = append(results, r)
			if tu != nil {
				updates.Merge(tu)
			}
		case ovsdb.OperationDelete:
			r, tu := db.Delete(name, op.Table, op.Where)
			results = append(results, r)
			if tu != nil {
				updates.Merge(tu)
			}
		case ovsdb.OperationWait:
			r := db.Wait(name, op.Table, op.Timeout, op.Where, op.Columns, op.Until, op.Rows)
			results = append(results, r)
		case ovsdb.OperationCommit:
			durable := op.Durable
			r := db.Commit(name, op.Table, *durable)
			results = append(results, r)
		case ovsdb.OperationAbort:
			r := db.Abort(name, op.Table)
			results = append(results, r)
		case ovsdb.OperationComment:
			r := db.Comment(name, op.Table, *op.Comment)
			results = append(results, r)
		case ovsdb.OperationAssert:
			r := db.Assert(name, op.Table, *op.Lock)
			results = append(results, r)
		default:
			return nil, updates
		}
	}
	return results, updates
}

func (db *inMemoryDatabase) Insert(database string, table string, rowUUID string, row ovsdb.Row) (ovsdb.OperationResult, ovsdb.TableUpdates) {
	var targetDb *cache.TableCache
	var ok bool
	if targetDb, ok = db.databases[database]; !ok {
		return ovsdb.OperationResult{
			Error: "database does not exist",
		}, nil
	}
	if rowUUID == "" {
		rowUUID = uuid.NewString()
	}
	model, err := targetDb.CreateModel(table, &row, rowUUID)
	if err != nil {
		return ovsdb.OperationResult{
			Error: err.Error(),
		}, nil
	}

	// insert in to db
	if err := targetDb.Table(table).Create(rowUUID, model, true); err != nil {
		if indexErr, ok := err.(*cache.IndexExistsError); ok {
			e := ovsdb.ConstraintViolation{}
			return ovsdb.OperationResult{
				Error:   e.Error(),
				Details: indexErr.Error(),
			}, nil
		}
		panic(err)
	}

	resultRow, err := targetDb.Mapper().NewRow(table, model)
	if err != nil {
		return ovsdb.OperationResult{
			Error: err.Error(),
		}, nil
	}

	result := ovsdb.OperationResult{
		UUID: ovsdb.UUID{GoUUID: rowUUID},
	}
	return result, ovsdb.TableUpdates{
		table: {
			rowUUID: {
				New: &resultRow,
				Old: nil,
			},
		},
	}
}

func (db *inMemoryDatabase) Select(database string, table string, where []ovsdb.Condition, columns []string) ovsdb.OperationResult {
	var targetDb *cache.TableCache
	var ok bool
	if targetDb, ok = db.databases[database]; !ok {
		return ovsdb.OperationResult{
			Error: "database does not exist",
		}
	}

	var results []ovsdb.Row
	rows, err := matchCondition(targetDb, table, where)
	if err != nil {
		panic(err)
	}
	for _, row := range rows {
		resultRow, err := targetDb.Mapper().NewRow(table, row)
		if err != nil {
			panic(err)
		}
		results = append(results, resultRow)
	}
	return ovsdb.OperationResult{
		Rows: results,
	}
}

func (db *inMemoryDatabase) Update(database, table string, where []ovsdb.Condition, row ovsdb.Row) (ovsdb.OperationResult, ovsdb.TableUpdates) {
	var targetDb *cache.TableCache
	var ok bool
	if targetDb, ok = db.databases[database]; !ok {
		return ovsdb.OperationResult{
			Error: "database does not exist",
		}, nil
	}

	schema := targetDb.Mapper().Schema.Table(table)
	tableUpdate := make(ovsdb.TableUpdate)
	rows, err := matchCondition(targetDb, table, where)
	if err != nil {
		return ovsdb.OperationResult{
			Error: err.Error(),
		}, nil
	}
	for _, old := range rows {
		info, _ := mapper.NewInfo(schema, old)
		uuid, _ := info.FieldByColumn("_uuid")
		oldRow, err := targetDb.Mapper().NewRow(table, old)
		if err != nil {
			panic(err)
		}
		newRow, err := targetDb.Mapper().NewRow(table, row)
		if err != nil {
			panic(err)
		}
		if err = targetDb.Table(table).Update(uuid.(string), row, true); err != nil {
			if indexErr, ok := err.(*cache.IndexExistsError); ok {
				e := ovsdb.ConstraintViolation{}
				return ovsdb.OperationResult{
					Error: e.Error(),

					Details: indexErr.Error(),
				}, nil
			}
			panic(err)
		}
		tableUpdate.AddRowUpdate(uuid.(string), &ovsdb.RowUpdate{
			Old: &oldRow,
			New: &newRow,
		})
	}
	// FIXME: We need to filter the returned columns
	return ovsdb.OperationResult{
			Count: len(rows),
		}, ovsdb.TableUpdates{
			table: tableUpdate,
		}
}

func (db *inMemoryDatabase) Mutate(database, table string, where []ovsdb.Condition, mutations []ovsdb.Mutation) (ovsdb.OperationResult, ovsdb.TableUpdates) {
	var targetDb *cache.TableCache
	var ok bool
	if targetDb, ok = db.databases[database]; !ok {
		return ovsdb.OperationResult{
			Error: "database does not exist",
		}, nil
	}

	schema := targetDb.Mapper().Schema.Table(table)
	tableUpdate := make(ovsdb.TableUpdate)

	rows, err := matchCondition(targetDb, table, where)
	if err != nil {
		panic(err)
	}

	for _, old := range rows {
		info, err := mapper.NewInfo(schema, old)
		if err != nil {
			panic(err)
		}
		uuid, _ := info.FieldByColumn("_uuid")
		oldRow, err := targetDb.Mapper().NewRow(table, old)
		if err != nil {
			panic(err)
		}
		for _, m := range mutations {
			column := schema.Column(m.Column)
			nativeValue, err := ovsdb.OvsToNative(column, m.Value)
			if err != nil {
				panic(err)
			}
			if err := ovsdb.ValidateMutation(column, m.Mutator, nativeValue); err != nil {
				panic(err)
			}
			info, err := mapper.NewInfo(schema, old)
			if err != nil {
				panic(err)
			}
			current, err := info.FieldByColumn(m.Column)
			if err != nil {
				panic(err)
			}
			new := mutate(current, m.Mutator, nativeValue)
			if err := info.SetField(m.Column, new); err != nil {
				panic(err)
			}
			// the field in old has been set, write back to db
			err = targetDb.Table(table).Update(uuid.(string), old, true)
			if err != nil {
				if indexErr, ok := err.(*cache.IndexExistsError); ok {
					e := ovsdb.ConstraintViolation{}
					return ovsdb.OperationResult{
						Error:   e.Error(),
						Details: indexErr.Error(),
					}, nil
				}
				panic(err)
			}
			newRow, err := targetDb.Mapper().NewRow(table, old)
			if err != nil {
				panic(err)
			}
			tableUpdate.AddRowUpdate(uuid.(string), &ovsdb.RowUpdate{
				Old: &oldRow,
				New: &newRow,
			})
		}
	}
	return ovsdb.OperationResult{
			Count: len(rows),
		}, ovsdb.TableUpdates{
			table: tableUpdate,
		}
}

func (db *inMemoryDatabase) Delete(database, table string, where []ovsdb.Condition) (ovsdb.OperationResult, ovsdb.TableUpdates) {
	var targetDb *cache.TableCache
	var ok bool
	if targetDb, ok = db.databases[database]; !ok {
		return ovsdb.OperationResult{
			Error: "database does not exist",
		}, nil
	}

	schema := targetDb.Mapper().Schema.Table(table)
	tableUpdate := make(ovsdb.TableUpdate)
	rows, err := matchCondition(targetDb, table, where)
	if err != nil {
		panic(err)
	}
	for _, row := range rows {
		info, _ := mapper.NewInfo(schema, row)
		uuid, _ := info.FieldByColumn("_uuid")
		oldRow, err := targetDb.Mapper().NewRow(table, row)
		if err != nil {
			panic(err)
		}
		if err := targetDb.Table(table).Delete(uuid.(string)); err != nil {
			panic(err)
		}
		tableUpdate.AddRowUpdate(uuid.(string), &ovsdb.RowUpdate{
			Old: &oldRow,
			New: nil,
		})
	}
	return ovsdb.OperationResult{
			Count: len(rows),
		}, ovsdb.TableUpdates{
			table: tableUpdate,
		}
}

func (db *inMemoryDatabase) Wait(database, table string, timeout int, conditions []ovsdb.Condition, columns []string, until string, rows []ovsdb.Row) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func (db *inMemoryDatabase) Commit(database, table string, durable bool) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func (db *inMemoryDatabase) Abort(database, table string) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func (db *inMemoryDatabase) Comment(database, table string, comment string) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func (db *inMemoryDatabase) Assert(database, table, lock string) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}
func removeFromSlice(a, b reflect.Value) reflect.Value {
	for i := 0; i < a.Len(); i++ {
		if a.Index(i).Interface() == b.Interface() {
			v := reflect.AppendSlice(a.Slice(0, i), a.Slice(i+1, a.Len()))
			return v
		}
	}
	return a
}

func matchCondition(targetDb *cache.TableCache, table string, conditions []ovsdb.Condition) ([]model.Model, error) {
	var results []model.Model
	if len(conditions) == 0 {
		uuids := targetDb.Table(table).Rows()
		for _, uuid := range uuids {
			row := targetDb.Table(table).Row(uuid)
			results = append(results, row)
		}
		return results, nil
	}

	for _, condition := range conditions {
		if condition.Column == "_uuid" {
			ovsdbUUID, ok := condition.Value.(ovsdb.UUID)
			if !ok {
				panic(fmt.Sprintf("%+v is not an ovsdb uuid", ovsdbUUID))
			}
			uuid := ovsdbUUID.GoUUID
			for _, k := range targetDb.Table(table).Rows() {
				ok, err := condition.Function.Evaluate(k, uuid)
				if err != nil {
					return nil, err
				}
				if ok {
					row := targetDb.Table(table).Row(k)
					results = append(results, row)
				}
			}
		} else {
			index, err := targetDb.Table(table).Index(condition.Column)
			if err != nil {
				return nil, fmt.Errorf("conditions on non-index fields not supported")
			}
			for k, v := range index {
				tSchema := targetDb.Mapper().Schema.Tables[table].Columns[condition.Column]
				nativeValue, err := ovsdb.OvsToNative(tSchema, condition.Value)
				if err != nil {
					return nil, err
				}
				ok, err := condition.Function.Evaluate(k, nativeValue)
				if err != nil {
					return nil, err
				}
				if ok {
					row := targetDb.Table(table).Row(v)
					results = append(results, row)
				}
			}
		}
	}
	return results, nil
}

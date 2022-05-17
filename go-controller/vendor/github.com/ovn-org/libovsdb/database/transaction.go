package database

import (
	"fmt"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/cache"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

type Transaction struct {
	ID          uuid.UUID
	Cache       *cache.TableCache
	DeletedRows map[string]struct{}
	Model       model.DatabaseModel
	DbName      string
	Database    Database
}

func NewTransaction(model model.DatabaseModel, dbName string, database Database, logger *logr.Logger) Transaction {
	if logger != nil {
		l := logger.WithName("transaction")
		logger = &l
	}
	cache, err := cache.NewTableCache(model, nil, logger)
	if err != nil {
		panic(err)
	}
	return Transaction{
		ID:          uuid.New(),
		Cache:       cache,
		DeletedRows: make(map[string]struct{}),
		Model:       model,
		DbName:      dbName,
		Database:    database,
	}
}

func (t *Transaction) Transact(operations []ovsdb.Operation) ([]ovsdb.OperationResult, ovsdb.TableUpdates2) {
	results := make([]ovsdb.OperationResult, 0, len(operations)+1)
	updates := make(ovsdb.TableUpdates2)

	for _, op := range operations {
		switch op.Op {
		case ovsdb.OperationInsert:
			r, tu := t.Insert(op.Table, op.UUIDName, op.Row)
			results = append(results, r)
			if tu != nil {
				updates.Merge(tu)
				if err := t.Cache.Populate2(tu); err != nil {
					panic(err)
				}
			}
		case ovsdb.OperationSelect:
			r := t.Select(op.Table, op.Where, op.Columns)
			results = append(results, r)
		case ovsdb.OperationUpdate:
			r, tu := t.Update(op.Table, op.Where, op.Row)
			results = append(results, r)
			if tu != nil {
				updates.Merge(tu)
				if err := t.Cache.Populate2(tu); err != nil {
					panic(err)
				}
			}
		case ovsdb.OperationMutate:
			r, tu := t.Mutate(op.Table, op.Where, op.Mutations)
			results = append(results, r)
			if tu != nil {
				updates.Merge(tu)
				if err := t.Cache.Populate2(tu); err != nil {
					panic(err)
				}
			}
		case ovsdb.OperationDelete:
			r, tu := t.Delete(op.Table, op.Where)
			results = append(results, r)
			if tu != nil {
				updates.Merge(tu)
				if err := t.Cache.Populate2(tu); err != nil {
					panic(err)
				}
			}
		case ovsdb.OperationWait:
			r := t.Wait(op.Table, op.Timeout, op.Where, op.Columns, op.Until, op.Rows)
			results = append(results, r)
		case ovsdb.OperationCommit:
			durable := op.Durable
			r := t.Commit(op.Table, *durable)
			results = append(results, r)
		case ovsdb.OperationAbort:
			r := t.Abort(op.Table)
			results = append(results, r)
		case ovsdb.OperationComment:
			r := t.Comment(op.Table, *op.Comment)
			results = append(results, r)
		case ovsdb.OperationAssert:
			r := t.Assert(op.Table, *op.Lock)
			results = append(results, r)
		default:
			return nil, updates
		}
	}

	if err := t.checkIndexes(); err != nil {
		if indexExists, ok := err.(*cache.ErrIndexExists); ok {
			e := ovsdb.ConstraintViolation{}
			results = append(results, ovsdb.OperationResult{
				Error:   e.Error(),
				Details: newIndexExistsDetails(*indexExists),
			})
		} else {
			results = append(results, ovsdb.OperationResult{
				Error: err.Error(),
			})
		}
	}

	return results, updates
}

func (t *Transaction) rowsFromTransactionCacheAndDatabase(table string, where []ovsdb.Condition) (map[string]model.Model, error) {
	txnRows, err := t.Cache.Table(table).RowsByConditionShallow(where)
	if err != nil {
		return nil, fmt.Errorf("failed getting rows for table %s from transaction cache: %v", table, err)
	}
	rows, err := t.Database.List(t.DbName, table, where...)
	if err != nil {
		return nil, fmt.Errorf("failed getting rows for table %s from database: %v", table, err)
	}

	// prefer rows from transaction cache while copying into cache
	// rows that are in the db.
	for rowUUID, row := range rows {
		if txnRow, found := txnRows[rowUUID]; found {
			rows[rowUUID] = txnRow
		} else {
			// warm the transaction cache with the current contents of the row
			if err := t.Cache.Table(table).Create(rowUUID, row, false); err != nil {
				return nil, fmt.Errorf("failed warming transaction cache row %s %v for table %s: %v", rowUUID, row, table, err)
			}
			txnRows[rowUUID] = row
		}
	}
	// exclude deleted rows
	for rowUUID := range t.DeletedRows {
		delete(rows, rowUUID)
	}
	return rows, nil
}

// checkIndexes checks that there are no index conflicts:
// - no duplicate indexes among any two rows operated with in the transaction
// - no duplicate indexes of any transaction row with any database row
func (t *Transaction) checkIndexes() error {
	// check for index conflicts.
	tables := t.Cache.Tables()
	for _, table := range tables {
		tc := t.Cache.Table(table)
		for _, row := range tc.RowsShallow() {
			err := tc.IndexExists(row)
			if err != nil {
				return err
			}
			err = t.Database.CheckIndexes(t.DbName, table, row)
			errIndexExists, isErrIndexExists := err.(*cache.ErrIndexExists)
			if !isErrIndexExists {
				return err
			}
			for _, existing := range errIndexExists.Existing {
				if _, isDeleted := t.DeletedRows[existing]; isDeleted {
					// this model is deleted in the transaction, ignore it
					continue
				}
				if tc.HasRow(existing) {
					// this model is updated in the transaction and was not
					// detected as a duplicate, so an index must have been
					// updated, ignore it
					continue
				}
				return err
			}
		}
	}
	return nil
}

func (t *Transaction) Insert(table string, rowUUID string, row ovsdb.Row) (ovsdb.OperationResult, ovsdb.TableUpdates2) {
	dbModel := t.Model
	m := dbModel.Mapper

	if rowUUID == "" {
		rowUUID = uuid.NewString()
	}

	model, err := dbModel.NewModel(table)
	if err != nil {
		return ovsdb.OperationResult{
			Error: err.Error(),
		}, nil
	}

	mapperInfo, err := dbModel.NewModelInfo(model)
	if err != nil {
		return ovsdb.OperationResult{
			Error: err.Error(),
		}, nil
	}
	err = m.GetRowData(&row, mapperInfo)
	if err != nil {
		return ovsdb.OperationResult{
			Error: err.Error(),
		}, nil
	}

	if rowUUID != "" {
		if err := mapperInfo.SetField("_uuid", rowUUID); err != nil {
			return ovsdb.OperationResult{
				Error: err.Error(),
			}, nil
		}
	}

	resultRow, err := m.NewRow(mapperInfo)
	if err != nil {
		return ovsdb.OperationResult{
			Error: err.Error(),
		}, nil
	}

	result := ovsdb.OperationResult{
		UUID: ovsdb.UUID{GoUUID: rowUUID},
	}
	return result, ovsdb.TableUpdates2{
		table: {
			rowUUID: {
				Insert: &resultRow,
				New:    &resultRow,
				Old:    nil,
			},
		},
	}
}

func (t *Transaction) Select(table string, where []ovsdb.Condition, columns []string) ovsdb.OperationResult {
	var results []ovsdb.Row
	dbModel := t.Model

	rows, err := t.rowsFromTransactionCacheAndDatabase(table, where)
	if err != nil {
		panic(err)
	}

	m := dbModel.Mapper
	for _, row := range rows {
		info, err := dbModel.NewModelInfo(row)
		if err != nil {
			panic(err)
		}
		resultRow, err := m.NewRow(info)
		if err != nil {
			panic(err)
		}
		results = append(results, resultRow)
	}
	return ovsdb.OperationResult{
		Rows: results,
	}
}

func (t *Transaction) Update(table string, where []ovsdb.Condition, row ovsdb.Row) (ovsdb.OperationResult, ovsdb.TableUpdates2) {
	dbModel := t.Model
	m := dbModel.Mapper
	schema := dbModel.Schema.Table(table)
	tableUpdate := make(ovsdb.TableUpdate2)

	rows, err := t.rowsFromTransactionCacheAndDatabase(table, where)
	if err != nil {
		return ovsdb.OperationResult{
			Error: err.Error(),
		}, nil
	}

	for uuid, old := range rows {
		oldInfo, _ := dbModel.NewModelInfo(old)

		oldRow, err := m.NewRow(oldInfo)
		if err != nil {
			panic(err)
		}
		new, err := dbModel.NewModel(table)
		if err != nil {
			panic(err)
		}
		newInfo, err := dbModel.NewModelInfo(new)
		if err != nil {
			panic(err)
		}
		err = m.GetRowData(&oldRow, newInfo)
		if err != nil {
			panic(err)
		}
		err = newInfo.SetField("_uuid", uuid)
		if err != nil {
			panic(err)
		}

		rowDelta := ovsdb.NewRow()
		for column, value := range row {
			colSchema := schema.Column(column)
			if colSchema == nil {
				e := ovsdb.ConstraintViolation{}
				return ovsdb.OperationResult{
					Error:   e.Error(),
					Details: fmt.Sprintf("%s is not a valid column in the %s table", column, table),
				}, nil
			}
			if !colSchema.Mutable() {
				e := ovsdb.ConstraintViolation{}
				return ovsdb.OperationResult{
					Error:   e.Error(),
					Details: fmt.Sprintf("column %s is of table %s not mutable", column, table),
				}, nil
			}
			old, err := newInfo.FieldByColumn(column)
			if err != nil {
				panic(err)
			}

			oldValue, err := ovsdb.NativeToOvs(colSchema, old)
			if err != nil {
				oldValue = nil
			}

			native, err := ovsdb.OvsToNative(colSchema, value)
			if err != nil {
				panic(err)
			}

			if oldValue == native {
				continue
			}

			err = newInfo.SetField(column, native)
			if err != nil {
				panic(err)
			}
			// convert the native to an ovs value
			// since the value in the RowUpdate hasn't been normalized
			newValue, err := ovsdb.NativeToOvs(colSchema, native)
			if err != nil {
				panic(err)
			}
			diff := diff(oldValue, newValue)
			if diff != nil {
				rowDelta[column] = diff
			}
		}

		newRow, err := m.NewRow(newInfo)
		if err != nil {
			panic(err)
		}

		tableUpdate.AddRowUpdate(uuid, &ovsdb.RowUpdate2{
			Modify: &rowDelta,
			Old:    &oldRow,
			New:    &newRow,
		})
	}
	// FIXME: We need to filter the returned columns
	return ovsdb.OperationResult{
			Count: len(rows),
		}, ovsdb.TableUpdates2{
			table: tableUpdate,
		}
}

func (t *Transaction) Mutate(table string, where []ovsdb.Condition, mutations []ovsdb.Mutation) (ovsdb.OperationResult, ovsdb.TableUpdates2) {
	dbModel := t.Model
	m := dbModel.Mapper
	schema := dbModel.Schema.Table(table)
	tableUpdate := make(ovsdb.TableUpdate2)

	rows, err := t.rowsFromTransactionCacheAndDatabase(table, where)
	if err != nil {
		panic(err)
	}

	for uuid, old := range rows {
		oldInfo, err := dbModel.NewModelInfo(old)
		if err != nil {
			panic(err)
		}
		oldRow, err := m.NewRow(oldInfo)
		if err != nil {
			panic(err)
		}
		new, err := dbModel.NewModel(table)
		if err != nil {
			panic(err)
		}
		newInfo, err := dbModel.NewModelInfo(new)
		if err != nil {
			panic(err)
		}
		err = m.GetRowData(&oldRow, newInfo)
		if err != nil {
			panic(err)
		}
		err = newInfo.SetField("_uuid", uuid)
		if err != nil {
			panic(err)
		}

		rowDelta := ovsdb.NewRow()
		mutateCols := make(map[string]struct{})
		for _, mutation := range mutations {
			mutateCols[mutation.Column] = struct{}{}
			column := schema.Column(mutation.Column)
			var nativeValue interface{}
			// Usually a mutation value is of the same type of the value being mutated
			// except for delete mutation of maps where it can also be a list of same type of
			// keys (rfc7047 5.1). Handle this special case here.
			if mutation.Mutator == "delete" && column.Type == ovsdb.TypeMap && reflect.TypeOf(mutation.Value) != reflect.TypeOf(ovsdb.OvsMap{}) {
				nativeValue, err = ovsdb.OvsToNativeSlice(column.TypeObj.Key.Type, mutation.Value)
				if err != nil {
					panic(err)
				}
			} else {
				nativeValue, err = ovsdb.OvsToNative(column, mutation.Value)
				if err != nil {
					panic(err)
				}
			}
			if err := ovsdb.ValidateMutation(column, mutation.Mutator, nativeValue); err != nil {
				panic(err)
			}
			current, err := newInfo.FieldByColumn(mutation.Column)
			if err != nil {
				panic(err)
			}
			newValue, _ := ovsdb.Mutate(current, mutation.Mutator, nativeValue)
			if err := newInfo.SetField(mutation.Column, newValue); err != nil {
				panic(err)
			}
		}
		for changed := range mutateCols {
			colSchema := schema.Column(changed)
			oldValueNative, err := oldInfo.FieldByColumn(changed)
			if err != nil {
				panic(err)
			}

			newValueNative, err := newInfo.FieldByColumn(changed)
			if err != nil {
				panic(err)
			}

			oldValue, err := ovsdb.NativeToOvs(colSchema, oldValueNative)
			if err != nil {
				panic(err)
			}

			newValue, err := ovsdb.NativeToOvs(colSchema, newValueNative)
			if err != nil {
				panic(err)
			}

			delta := diff(oldValue, newValue)
			if delta != nil {
				rowDelta[changed] = delta
			}
		}

		newRow, err := m.NewRow(newInfo)
		if err != nil {
			panic(err)
		}

		tableUpdate.AddRowUpdate(uuid, &ovsdb.RowUpdate2{
			Modify: &rowDelta,
			Old:    &oldRow,
			New:    &newRow,
		})
	}

	return ovsdb.OperationResult{
			Count: len(rows),
		}, ovsdb.TableUpdates2{
			table: tableUpdate,
		}
}

func (t *Transaction) Delete(table string, where []ovsdb.Condition) (ovsdb.OperationResult, ovsdb.TableUpdates2) {
	dbModel := t.Model
	m := dbModel.Mapper
	tableUpdate := make(ovsdb.TableUpdate2)

	rows, err := t.rowsFromTransactionCacheAndDatabase(table, where)
	if err != nil {
		panic(err)
	}

	for uuid, row := range rows {
		info, _ := dbModel.NewModelInfo(row)
		oldRow, err := m.NewRow(info)
		if err != nil {
			panic(err)
		}
		tableUpdate.AddRowUpdate(uuid, &ovsdb.RowUpdate2{
			Delete: &ovsdb.Row{},
			Old:    &oldRow,
		})
		// track delete operation in transaction to complement cache
		t.DeletedRows[uuid] = struct{}{}
	}
	return ovsdb.OperationResult{
			Count: len(rows),
		}, ovsdb.TableUpdates2{
			table: tableUpdate,
		}
}

func (t *Transaction) Wait(table string, timeout *int, where []ovsdb.Condition, columns []string, until string, rows []ovsdb.Row) ovsdb.OperationResult {
	start := time.Now()

	if until != "!=" && until != "==" {
		e := ovsdb.NotSupported{}
		return ovsdb.OperationResult{Error: e.Error()}
	}

	dbModel := t.Model
	realTable := dbModel.Schema.Table(table)
	if realTable == nil {
		e := ovsdb.NotSupported{}
		return ovsdb.OperationResult{Error: e.Error()}
	}
	model, err := dbModel.NewModel(table)
	if err != nil {
		panic(err)
	}

Loop:
	for {
		var filteredRows []ovsdb.Row
		foundRowModels, err := t.rowsFromTransactionCacheAndDatabase(table, where)
		if err != nil {
			panic(err)
		}

		m := dbModel.Mapper
		for _, rowModel := range foundRowModels {
			info, err := dbModel.NewModelInfo(rowModel)
			if err != nil {
				panic(err)
			}

			foundMatch := true
			for _, column := range columns {
				columnSchema := info.Metadata.TableSchema.Column(column)
				for _, r := range rows {
					i, err := dbModel.NewModelInfo(model)
					if err != nil {
						panic(err)
					}
					err = dbModel.Mapper.GetRowData(&r, i)
					if err != nil {
						panic(err)
					}
					x, err := i.FieldByColumn(column)
					if err != nil {
						panic(err)
					}

					// check to see if field value is default for given rows
					// if it is default (not provided) we shouldn't try to compare
					// for equality
					if ovsdb.IsDefaultValue(columnSchema, x) {
						continue
					}
					y, err := info.FieldByColumn(column)
					if err != nil {
						panic(err)
					}
					if !reflect.DeepEqual(x, y) {
						foundMatch = false
					}
				}
			}

			if foundMatch {
				resultRow, err := m.NewRow(info)
				if err != nil {
					panic(err)
				}
				filteredRows = append(filteredRows, resultRow)
			}

		}

		if until == "==" && len(filteredRows) == len(rows) {
			return ovsdb.OperationResult{}
		} else if until == "!=" && len(filteredRows) != len(rows) {
			return ovsdb.OperationResult{}
		}

		if timeout != nil {
			// TODO(trozet): this really shouldn't just break and loop on a time interval
			// Really this client handler should pause, wait for another handler to update the DB
			// and then try again. However the server is single threaded for now and not capable of
			// doing something like that.
			if time.Since(start) > time.Duration(*timeout)*time.Millisecond {
				break Loop
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	e := ovsdb.TimedOut{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func (t *Transaction) Commit(table string, durable bool) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func (t *Transaction) Abort(table string) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func (t *Transaction) Comment(table string, comment string) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func (t *Transaction) Assert(table, lock string) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func diff(a interface{}, b interface{}) interface{} {
	switch a.(type) {
	case ovsdb.OvsSet:
		// original value
		original := a.(ovsdb.OvsSet)
		// replacement value
		replacement := b.(ovsdb.OvsSet)
		var c []interface{}
		for _, originalElem := range original.GoSet {
			found := false
			for _, replacementElem := range replacement.GoSet {
				if originalElem == replacementElem {
					found = true
					break
				}
			}
			if !found {
				// remove from client
				c = append(c, originalElem)
			}
		}
		for _, replacementElem := range replacement.GoSet {
			found := false
			for _, originalElem := range original.GoSet {
				if replacementElem == originalElem {
					found = true
					break
				}
			}
			if !found {
				// add to client
				c = append(c, replacementElem)
			}
		}
		if len(c) > 0 {
			cSet, _ := ovsdb.NewOvsSet(c)
			return cSet
		}
		return nil
	case ovsdb.OvsMap:
		originalMap := a.(ovsdb.OvsMap)
		replacementMap := b.(ovsdb.OvsMap)
		c := make(map[interface{}]interface{})
		for k, v := range originalMap.GoMap {
			// if key exists in replacement map
			if _, ok := replacementMap.GoMap[k]; ok {
				// and values are not equal
				if originalMap.GoMap[k] != replacementMap.GoMap[k] {
					// add to diff
					c[k] = replacementMap.GoMap[k]
				}
			} else {
				// if key does not exist in replacement map
				// add old value so it's deleted by client
				c[k] = v
			}
		}
		for k, v := range replacementMap.GoMap {
			// if key does not exist in original map
			if _, ok := originalMap.GoMap[k]; !ok {
				// add old value so it's added by client
				c[k] = v
			}
		}
		if len(c) > 0 {
			cMap, _ := ovsdb.NewOvsMap(c)
			return cMap
		}
		return nil
	default:
		return b
	}
}

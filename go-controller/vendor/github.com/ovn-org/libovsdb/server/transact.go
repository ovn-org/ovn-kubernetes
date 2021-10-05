package server

import (
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/cache"
	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/ovsdb"
)

func (o *OvsdbServer) transact(name string, operations []ovsdb.Operation) ([]ovsdb.OperationResult, ovsdb.TableUpdates2) {
	results := []ovsdb.OperationResult{}
	updates := make(ovsdb.TableUpdates2)
	for _, op := range operations {
		switch op.Op {
		case ovsdb.OperationInsert:
			r, tu := o.Insert(name, op.Table, op.UUIDName, op.Row)
			results = append(results, r)
			if tu != nil {
				updates.Merge(tu)
			}
		case ovsdb.OperationSelect:
			r := o.Select(name, op.Table, op.Where, op.Columns)
			results = append(results, r)
		case ovsdb.OperationUpdate:
			r, tu := o.Update(name, op.Table, op.Where, op.Row)
			results = append(results, r)
			if tu != nil {
				updates.Merge(tu)
			}
		case ovsdb.OperationMutate:
			r, tu := o.Mutate(name, op.Table, op.Where, op.Mutations)
			results = append(results, r)
			if tu != nil {
				updates.Merge(tu)
			}
		case ovsdb.OperationDelete:
			r, tu := o.Delete(name, op.Table, op.Where)
			results = append(results, r)
			if tu != nil {
				updates.Merge(tu)
			}
		case ovsdb.OperationWait:
			r := o.Wait(name, op.Table, op.Timeout, op.Where, op.Columns, op.Until, op.Rows)
			results = append(results, r)
		case ovsdb.OperationCommit:
			durable := op.Durable
			r := o.Commit(name, op.Table, *durable)
			results = append(results, r)
		case ovsdb.OperationAbort:
			r := o.Abort(name, op.Table)
			results = append(results, r)
		case ovsdb.OperationComment:
			r := o.Comment(name, op.Table, *op.Comment)
			results = append(results, r)
		case ovsdb.OperationAssert:
			r := o.Assert(name, op.Table, *op.Lock)
			results = append(results, r)
		default:
			return nil, updates
		}
	}
	return results, updates
}

func (o *OvsdbServer) Insert(database string, table string, rowUUID string, row ovsdb.Row) (ovsdb.OperationResult, ovsdb.TableUpdates2) {
	if !o.db.Exists(database) {
		return ovsdb.OperationResult{
			Error: "database does not exist",
		}, nil
	}
	o.modelsMutex.Lock()
	dbModel := o.models[database]
	o.modelsMutex.Unlock()

	m := mapper.NewMapper(dbModel.Schema)
	tSchema := dbModel.Schema.Table(table)

	if rowUUID == "" {
		rowUUID = uuid.NewString()
	}

	model, err := dbModel.Model.NewModel(table)
	if err != nil {
		return ovsdb.OperationResult{
			Error: err.Error(),
		}, nil
	}

	err = m.GetRowData(table, &row, model)
	if err != nil {
		return ovsdb.OperationResult{
			Error: err.Error(),
		}, nil
	}

	if rowUUID != "" {
		mapperInfo, err := mapper.NewInfo(tSchema, model)
		if err != nil {
			return ovsdb.OperationResult{
				Error: err.Error(),
			}, nil
		}
		if err := mapperInfo.SetField("_uuid", rowUUID); err != nil {
			return ovsdb.OperationResult{
				Error: err.Error(),
			}, nil
		}
	}

	resultRow, err := m.NewRow(table, model)
	if err != nil {
		return ovsdb.OperationResult{
			Error: err.Error(),
		}, nil
	}

	// check for index conflicts
	if err := o.db.CheckIndexes(database, table, model); err != nil {
		if indexExists, ok := err.(*cache.ErrIndexExists); ok {
			e := ovsdb.ConstraintViolation{}
			return ovsdb.OperationResult{
				Error:   e.Error(),
				Details: newIndexExistsDetails(*indexExists),
			}, nil
		}
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

func (o *OvsdbServer) Select(database string, table string, where []ovsdb.Condition, columns []string) ovsdb.OperationResult {
	if !o.db.Exists(database) {
		return ovsdb.OperationResult{
			Error: "database does not exist",
		}
	}
	o.modelsMutex.Lock()
	dbModel := o.models[database]
	o.modelsMutex.Unlock()

	m := mapper.NewMapper(dbModel.Schema)

	var results []ovsdb.Row
	rows, err := o.db.List(database, table, where...)
	if err != nil {
		panic(err)
	}
	for _, row := range rows {
		resultRow, err := m.NewRow(table, row)
		if err != nil {
			panic(err)
		}
		results = append(results, resultRow)
	}
	return ovsdb.OperationResult{
		Rows: results,
	}
}

func (o *OvsdbServer) Update(database, table string, where []ovsdb.Condition, row ovsdb.Row) (ovsdb.OperationResult, ovsdb.TableUpdates2) {
	if !o.db.Exists(database) {
		return ovsdb.OperationResult{
			Error: "database does not exist",
		}, nil
	}
	o.modelsMutex.Lock()
	dbModel := o.models[database]
	o.modelsMutex.Unlock()

	m := mapper.NewMapper(dbModel.Schema)
	schema := dbModel.Schema.Table(table)
	tableUpdate := make(ovsdb.TableUpdate2)
	rows, err := o.db.List(database, table, where...)
	if err != nil {
		return ovsdb.OperationResult{
			Error: err.Error(),
		}, nil
	}
	for _, old := range rows {
		info, _ := mapper.NewInfo(schema, old)
		uuid, _ := info.FieldByColumn("_uuid")

		oldRow, err := m.NewRow(table, old)
		if err != nil {
			panic(err)
		}
		new, err := dbModel.Model.NewModel(table)
		if err != nil {
			panic(err)
		}
		err = m.GetRowData(table, &oldRow, new)
		if err != nil {
			panic(err)
		}
		info, err = mapper.NewInfo(schema, new)
		if err != nil {
			panic(err)
		}
		err = info.SetField("_uuid", uuid)
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
			old, err := info.FieldByColumn(column)
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

			err = info.SetField(column, native)
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

		newRow, err := m.NewRow(table, new)
		if err != nil {
			panic(err)
		}

		// check for index conflicts
		if err := o.db.CheckIndexes(database, table, new); err != nil {
			if indexExists, ok := err.(*cache.ErrIndexExists); ok {
				e := ovsdb.ConstraintViolation{}
				return ovsdb.OperationResult{
					Error:   e.Error(),
					Details: newIndexExistsDetails(*indexExists),
				}, nil
			}
			return ovsdb.OperationResult{
				Error: err.Error(),
			}, nil
		}

		tableUpdate.AddRowUpdate(uuid.(string), &ovsdb.RowUpdate2{
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

func (o *OvsdbServer) Mutate(database, table string, where []ovsdb.Condition, mutations []ovsdb.Mutation) (ovsdb.OperationResult, ovsdb.TableUpdates2) {
	if !o.db.Exists(database) {
		return ovsdb.OperationResult{
			Error: "database does not exist",
		}, nil
	}
	o.modelsMutex.Lock()
	dbModel := o.models[database]
	o.modelsMutex.Unlock()

	m := mapper.NewMapper(dbModel.Schema)
	schema := dbModel.Schema.Table(table)

	tableUpdate := make(ovsdb.TableUpdate2)

	rows, err := o.db.List(database, table, where...)
	if err != nil {
		panic(err)
	}

	for _, old := range rows {
		oldInfo, err := mapper.NewInfo(schema, old)
		if err != nil {
			panic(err)
		}
		uuid, _ := oldInfo.FieldByColumn("_uuid")
		oldRow, err := m.NewRow(table, old)
		if err != nil {
			panic(err)
		}
		new, err := dbModel.Model.NewModel(table)
		if err != nil {
			panic(err)
		}
		err = m.GetRowData(table, &oldRow, new)
		if err != nil {
			panic(err)
		}
		newInfo, err := mapper.NewInfo(schema, new)
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
			newValue, _ := mutate(current, mutation.Mutator, nativeValue)
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

		// check indexes
		if err := o.db.CheckIndexes(database, table, new); err != nil {
			if indexExists, ok := err.(*cache.ErrIndexExists); ok {
				e := ovsdb.ConstraintViolation{}
				return ovsdb.OperationResult{
					Error:   e.Error(),
					Details: newIndexExistsDetails(*indexExists),
				}, nil
			}
			return ovsdb.OperationResult{
				Error: err.Error(),
			}, nil
		}

		newRow, err := m.NewRow(table, new)
		if err != nil {
			panic(err)
		}

		tableUpdate.AddRowUpdate(uuid.(string), &ovsdb.RowUpdate2{
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

func (o *OvsdbServer) Delete(database, table string, where []ovsdb.Condition) (ovsdb.OperationResult, ovsdb.TableUpdates2) {
	if !o.db.Exists(database) {
		return ovsdb.OperationResult{
			Error: "database does not exist",
		}, nil
	}
	o.modelsMutex.Lock()
	dbModel := o.models[database]
	o.modelsMutex.Unlock()
	m := mapper.NewMapper(dbModel.Schema)
	schema := dbModel.Schema.Table(table)
	tableUpdate := make(ovsdb.TableUpdate2)
	rows, err := o.db.List(database, table, where...)
	if err != nil {
		panic(err)
	}
	for _, row := range rows {
		info, _ := mapper.NewInfo(schema, row)
		uuid, _ := info.FieldByColumn("_uuid")
		oldRow, err := m.NewRow(table, row)
		if err != nil {
			panic(err)
		}
		tableUpdate.AddRowUpdate(uuid.(string), &ovsdb.RowUpdate2{
			Delete: &ovsdb.Row{},
			Old:    &oldRow,
		})
	}
	return ovsdb.OperationResult{
			Count: len(rows),
		}, ovsdb.TableUpdates2{
			table: tableUpdate,
		}
}

func (o *OvsdbServer) Wait(database, table string, timeout int, conditions []ovsdb.Condition, columns []string, until string, rows []ovsdb.Row) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func (o *OvsdbServer) Commit(database, table string, durable bool) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func (o *OvsdbServer) Abort(database, table string) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func (o *OvsdbServer) Comment(database, table string, comment string) ovsdb.OperationResult {
	e := ovsdb.NotSupported{}
	return ovsdb.OperationResult{Error: e.Error()}
}

func (o *OvsdbServer) Assert(database, table, lock string) ovsdb.OperationResult {
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

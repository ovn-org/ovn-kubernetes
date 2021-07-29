package server

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/cache"
	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/ovsdb"
)

func (o *OvsdbServer) transact(name string, operations []ovsdb.Operation) ([]ovsdb.OperationResult, ovsdb.TableUpdates) {
	results := []ovsdb.OperationResult{}
	updates := make(ovsdb.TableUpdates)
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

func (o *OvsdbServer) Insert(database string, table string, rowUUID string, row ovsdb.Row) (ovsdb.OperationResult, ovsdb.TableUpdates) {
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
	return result, ovsdb.TableUpdates{
		table: {
			rowUUID: {
				New: &resultRow,
				Old: nil,
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

func (o *OvsdbServer) Update(database, table string, where []ovsdb.Condition, row ovsdb.Row) (ovsdb.OperationResult, ovsdb.TableUpdates) {
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
	tableUpdate := make(ovsdb.TableUpdate)
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
			native, err := ovsdb.OvsToNative(colSchema, value)
			if err != nil {
				panic(err)
			}
			err = info.SetField(column, native)
			if err != nil {
				panic(err)
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

func (o *OvsdbServer) Mutate(database, table string, where []ovsdb.Condition, mutations []ovsdb.Mutation) (ovsdb.OperationResult, ovsdb.TableUpdates) {
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

	tableUpdate := make(ovsdb.TableUpdate)

	rows, err := o.db.List(database, table, where...)
	if err != nil {
		panic(err)
	}

	for _, old := range rows {
		info, err := mapper.NewInfo(schema, old)
		if err != nil {
			panic(err)
		}
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
		for _, mutation := range mutations {
			column := schema.Column(mutation.Column)
			nativeValue, err := ovsdb.OvsToNative(column, mutation.Value)
			if err != nil {
				panic(err)
			}
			if err := ovsdb.ValidateMutation(column, mutation.Mutator, nativeValue); err != nil {
				panic(err)
			}
			current, err := info.FieldByColumn(mutation.Column)
			if err != nil {
				panic(err)
			}
			newValue := mutate(current, mutation.Mutator, nativeValue)
			if err := info.SetField(mutation.Column, newValue); err != nil {
				panic(err)
			}
			newRow, err := m.NewRow(table, new)
			if err != nil {
				panic(err)
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

func (o *OvsdbServer) Delete(database, table string, where []ovsdb.Condition) (ovsdb.OperationResult, ovsdb.TableUpdates) {
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
	tableUpdate := make(ovsdb.TableUpdate)
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

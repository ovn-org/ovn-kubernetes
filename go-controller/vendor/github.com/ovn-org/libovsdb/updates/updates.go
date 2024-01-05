package updates

import (
	"fmt"
	"reflect"

	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

type rowUpdate2 = ovsdb.RowUpdate2

// modelUpdate contains an update in model and OVSDB RowUpdate2 notation
type modelUpdate struct {
	rowUpdate2 *rowUpdate2
	old        model.Model
	new        model.Model
}

// isEmpty returns whether this update is empty
func (mu modelUpdate) isEmpty() bool {
	return mu == modelUpdate{}
}

// ModelUpdates contains updates indexed by table and uuid
type ModelUpdates struct {
	updates map[string]map[string]modelUpdate
}

// GetUpdatedTables returns the tables that have updates
func (u ModelUpdates) GetUpdatedTables() []string {
	tables := make([]string, 0, len(u.updates))
	for table, updates := range u.updates {
		if len(updates) > 0 {
			tables = append(tables, table)
		}
	}
	return tables
}

// ForEachModelUpdate processes each row update of a given table in model
// notation
func (u ModelUpdates) ForEachModelUpdate(table string, do func(uuid string, old, new model.Model) error) error {
	models := u.updates[table]
	for uuid, model := range models {
		err := do(uuid, model.old, model.new)
		if err != nil {
			return err
		}
	}
	return nil
}

// ForEachRowUpdate processes each row update of a given table in OVSDB
// RowUpdate2 notation
func (u ModelUpdates) ForEachRowUpdate(table string, do func(uuid string, row ovsdb.RowUpdate2) error) error {
	rows := u.updates[table]
	for uuid, row := range rows {
		err := do(uuid, *row.rowUpdate2)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetModel returns the last known state of the requested model. If the model is
// unknown or has been deleted, returns nil.
func (u ModelUpdates) GetModel(table, uuid string) model.Model {
	if u.updates == nil {
		return nil
	}
	if t, found := u.updates[table]; found {
		if update, found := t[uuid]; found {
			return update.new
		}
	}
	return nil
}

// GetRow returns the last known state of the requested row. If the row is
// unknown or has been deleted, returns nil.
func (u ModelUpdates) GetRow(table, uuid string) *ovsdb.Row {
	if u.updates == nil {
		return nil
	}
	if t, found := u.updates[table]; found {
		if update, found := t[uuid]; found {
			return update.rowUpdate2.New
		}
	}
	return nil
}

// Merge a set of updates with an earlier set of updates
func (u *ModelUpdates) Merge(dbModel model.DatabaseModel, new ModelUpdates) error {
	for table, models := range new.updates {
		for uuid, update := range models {
			err := u.addUpdate(dbModel, table, uuid, update)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// AddOperation adds an update for a model from a OVSDB Operation. If several
// updates for the same model are aggregated, the user is responsible that the
// provided model to be updated matches the updated model of the previous
// update.
func (u *ModelUpdates) AddOperation(dbModel model.DatabaseModel, table, uuid string, current model.Model, op *ovsdb.Operation) error {
	switch op.Op {
	case ovsdb.OperationInsert:
		return u.addInsertOperation(dbModel, table, uuid, op)
	case ovsdb.OperationUpdate:
		return u.addUpdateOperation(dbModel, table, uuid, current, op)
	case ovsdb.OperationMutate:
		return u.addMutateOperation(dbModel, table, uuid, current, op)
	case ovsdb.OperationDelete:
		return u.addDeleteOperation(dbModel, table, uuid, current, op)
	default:
		return fmt.Errorf("database update from operation %#v not supported", op.Op)
	}
}

// AddRowUpdate adds an update for a model from a OVSDB RowUpdate. If several
// updates for the same model are aggregated, the user is responsible that the
// provided model to be updated matches the updated model of the previous
// update.
func (u *ModelUpdates) AddRowUpdate(dbModel model.DatabaseModel, table, uuid string, current model.Model, ru ovsdb.RowUpdate) error {
	switch {
	case ru.Old == nil && ru.New != nil:
		new, err := model.CreateModel(dbModel, table, ru.New, uuid)
		if err != nil {
			return err
		}
		err = u.addUpdate(dbModel, table, uuid, modelUpdate{new: new, rowUpdate2: &rowUpdate2{New: ru.New}})
		if err != nil {
			return err
		}
	case ru.Old != nil && ru.New != nil:
		old := current
		new := model.Clone(current)
		info, err := dbModel.NewModelInfo(new)
		if err != nil {
			return err
		}
		changed, err := updateModel(dbModel, table, info, ru.New, nil)
		if !changed || err != nil {
			return err
		}
		err = u.addUpdate(dbModel, table, uuid, modelUpdate{old: old, new: new, rowUpdate2: &rowUpdate2{Old: ru.Old, New: ru.New}})
		if err != nil {
			return err
		}
	case ru.New == nil:
		old := current
		err := u.addUpdate(dbModel, table, uuid, modelUpdate{old: old, rowUpdate2: &rowUpdate2{Old: ru.Old}})
		if err != nil {
			return err
		}
	}
	return nil
}

// AddRowUpdate2 adds an update for a model from a OVSDB RowUpdate2. If several
// updates for the same model are aggregated, the user is responsible that the
// provided model to be updated matches the updated model of the previous
// update.
func (u *ModelUpdates) AddRowUpdate2(dbModel model.DatabaseModel, table, uuid string, current model.Model, ru2 ovsdb.RowUpdate2) error {
	switch {
	case ru2.Initial != nil:
		ru2.Insert = ru2.Initial
		fallthrough
	case ru2.Insert != nil:
		new, err := model.CreateModel(dbModel, table, ru2.Insert, uuid)
		if err != nil {
			return err
		}
		err = u.addUpdate(dbModel, table, uuid, modelUpdate{new: new, rowUpdate2: &ru2})
		if err != nil {
			return err
		}
	case ru2.Modify != nil:
		old := current
		new := model.Clone(current)
		info, err := dbModel.NewModelInfo(new)
		if err != nil {
			return err
		}
		changed, err := modifyModel(dbModel, table, info, ru2.Modify)
		if !changed || err != nil {
			return err
		}
		err = u.addUpdate(dbModel, table, uuid, modelUpdate{old: old, new: new, rowUpdate2: &ru2})
		if err != nil {
			return err
		}
	default:
		old := current
		err := u.addUpdate(dbModel, table, uuid, modelUpdate{old: old, rowUpdate2: &ru2})
		if err != nil {
			return err
		}
	}
	return nil
}

func (u *ModelUpdates) addUpdate(dbModel model.DatabaseModel, table, uuid string, update modelUpdate) error {
	if u.updates == nil {
		u.updates = map[string]map[string]modelUpdate{}
	}
	if _, ok := u.updates[table]; !ok {
		u.updates[table] = make(map[string]modelUpdate)
	}

	ts := dbModel.Schema.Table(table)
	update, err := merge(ts, u.updates[table][uuid], update)
	if err != nil {
		return err
	}

	if !update.isEmpty() {
		u.updates[table][uuid] = update
		return nil
	}

	// If after the merge this amounts to no update, remove it from the list and
	// clean up
	delete(u.updates[table], uuid)
	if len(u.updates[table]) == 0 {
		delete(u.updates, table)
	}
	if len(u.updates) == 0 {
		u.updates = nil
	}

	return nil
}

func (u *ModelUpdates) addInsertOperation(dbModel model.DatabaseModel, table, uuid string, op *ovsdb.Operation) error {
	m := dbModel.Mapper

	model, err := dbModel.NewModel(table)
	if err != nil {
		return err
	}

	mapperInfo, err := dbModel.NewModelInfo(model)
	if err != nil {
		return err
	}

	err = m.GetRowData(&op.Row, mapperInfo)
	if err != nil {
		return err
	}

	err = mapperInfo.SetField("_uuid", uuid)
	if err != nil {
		return err
	}

	resultRow, err := m.NewRow(mapperInfo)
	if err != nil {
		return err
	}

	err = u.addUpdate(dbModel, table, uuid,
		modelUpdate{
			old: nil,
			new: model,
			rowUpdate2: &rowUpdate2{
				Insert: &resultRow,
				New:    &resultRow,
				Old:    nil,
			},
		},
	)

	return err
}

func (u *ModelUpdates) addUpdateOperation(dbModel model.DatabaseModel, table, uuid string, old model.Model, op *ovsdb.Operation) error {
	m := dbModel.Mapper

	oldInfo, err := dbModel.NewModelInfo(old)
	if err != nil {
		return err
	}

	oldRow, err := m.NewRow(oldInfo)
	if err != nil {
		return err
	}

	new := model.Clone(old)
	newInfo, err := dbModel.NewModelInfo(new)
	if err != nil {
		return err
	}

	delta := ovsdb.NewRow()
	changed, err := updateModel(dbModel, table, newInfo, &op.Row, &delta)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	newRow, err := m.NewRow(newInfo)
	if err != nil {
		return err
	}

	err = u.addUpdate(dbModel, table, uuid,
		modelUpdate{
			old: old,
			new: new,
			rowUpdate2: &rowUpdate2{
				Modify: &delta,
				Old:    &oldRow,
				New:    &newRow,
			},
		},
	)

	return err
}

func (u *ModelUpdates) addMutateOperation(dbModel model.DatabaseModel, table, uuid string, old model.Model, op *ovsdb.Operation) error {
	m := dbModel.Mapper
	schema := dbModel.Schema.Table(table)

	oldInfo, err := dbModel.NewModelInfo(old)
	if err != nil {
		return err
	}

	oldRow, err := m.NewRow(oldInfo)
	if err != nil {
		return err
	}

	new := model.Clone(old)
	newInfo, err := dbModel.NewModelInfo(new)
	if err != nil {
		return err
	}

	differences := make(map[string]interface{})
	for _, mutation := range op.Mutations {
		column := schema.Column(mutation.Column)
		if column == nil {
			continue
		}

		var nativeValue interface{}
		// Usually a mutation value is of the same type of the value being mutated
		// except for delete mutation of maps where it can also be a list of same type of
		// keys (rfc7047 5.1). Handle this special case here.
		if mutation.Mutator == "delete" && column.Type == ovsdb.TypeMap && reflect.TypeOf(mutation.Value) != reflect.TypeOf(ovsdb.OvsMap{}) {
			nativeValue, err = ovsdb.OvsToNativeSlice(column.TypeObj.Key.Type, mutation.Value)
			if err != nil {
				return err
			}
		} else {
			nativeValue, err = ovsdb.OvsToNative(column, mutation.Value)
			if err != nil {
				return err
			}
		}

		if err := ovsdb.ValidateMutation(column, mutation.Mutator, nativeValue); err != nil {
			return err
		}

		current, err := newInfo.FieldByColumn(mutation.Column)
		if err != nil {
			return err
		}

		newValue, diff := mutate(current, mutation.Mutator, nativeValue)
		if err := newInfo.SetField(mutation.Column, newValue); err != nil {
			return err
		}

		old, err := oldInfo.FieldByColumn(mutation.Column)
		if err != nil {
			return err
		}
		diff, changed := mergeDifference(old, differences[mutation.Column], diff)
		if changed {
			differences[mutation.Column] = diff
		} else {
			delete(differences, mutation.Column)
		}
	}

	if len(differences) == 0 {
		return nil
	}

	delta := ovsdb.NewRow()
	for column, diff := range differences {
		colSchema := schema.Column(column)
		diffOvs, err := ovsdb.NativeToOvs(colSchema, diff)
		if err != nil {
			return err
		}
		delta[column] = diffOvs
	}

	newRow, err := m.NewRow(newInfo)
	if err != nil {
		return err
	}

	err = u.addUpdate(dbModel, table, uuid,
		modelUpdate{
			old: old,
			new: new,
			rowUpdate2: &rowUpdate2{
				Modify: &delta,
				Old:    &oldRow,
				New:    &newRow,
			},
		},
	)

	return err
}

func (u *ModelUpdates) addDeleteOperation(dbModel model.DatabaseModel, table, uuid string, old model.Model, op *ovsdb.Operation) error {
	m := dbModel.Mapper

	info, err := dbModel.NewModelInfo(old)
	if err != nil {
		return err
	}

	oldRow, err := m.NewRow(info)
	if err != nil {
		return err
	}

	err = u.addUpdate(dbModel, table, uuid,
		modelUpdate{
			old: old,
			new: nil,
			rowUpdate2: &rowUpdate2{
				Delete: &ovsdb.Row{},
				Old:    &oldRow,
			},
		},
	)

	return err
}

func updateModel(dbModel model.DatabaseModel, table string, info *mapper.Info, update, modify *ovsdb.Row) (bool, error) {
	return updateOrModifyModel(dbModel, table, info, update, modify, false)
}

func modifyModel(dbModel model.DatabaseModel, table string, info *mapper.Info, modify *ovsdb.Row) (bool, error) {
	return updateOrModifyModel(dbModel, table, info, modify, nil, true)
}

// updateOrModifyModel updates info about a model with a given row containing
// the change. The change row itself can be interpreted as an update or a
// modify. If the change is an update and a modify row is provided, it will be
// filled with the modify data.
func updateOrModifyModel(dbModel model.DatabaseModel, table string, info *mapper.Info, changeRow, modifyRow *ovsdb.Row, isModify bool) (bool, error) {
	schema := dbModel.Schema.Table(table)
	var changed bool

	for column, updateOvs := range *changeRow {
		colSchema := schema.Column(column)
		if colSchema == nil {
			// ignore columns we don't know about in our schema
			continue
		}

		currentNative, err := info.FieldByColumn(column)
		if err != nil {
			return false, err
		}

		updateNative, err := ovsdb.OvsToNative(colSchema, updateOvs)
		if err != nil {
			return false, err
		}

		if isModify {
			differenceNative, isDifferent := applyDifference(currentNative, updateNative)
			if isDifferent && !colSchema.Mutable() {
				return false, ovsdb.NewConstraintViolation(fmt.Sprintf("column %q of table %q is not mutable", column, table))
			}
			changed = changed || isDifferent
			err = info.SetField(column, differenceNative)
			if err != nil {
				return false, err
			}
		} else {
			differenceNative, isDifferent := difference(currentNative, updateNative)
			if isDifferent && !colSchema.Mutable() {
				return false, ovsdb.NewConstraintViolation(fmt.Sprintf("column %q of table %q is not mutable", column, table))
			}
			changed = changed || isDifferent
			if isDifferent && modifyRow != nil {
				deltaOvs, err := ovsdb.NativeToOvs(colSchema, differenceNative)
				if err != nil {
					return false, err
				}
				(*modifyRow)[column] = deltaOvs
			}
			err = info.SetField(column, updateNative)
			if err != nil {
				return false, err
			}
		}
	}

	return changed, nil
}

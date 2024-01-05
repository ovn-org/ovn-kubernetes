package updates

import (
	"fmt"

	"github.com/ovn-org/libovsdb/database"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// ReferenceProvider should be implemented by a database that tracks references
type ReferenceProvider interface {
	// GetReferences provides the references to the provided row
	GetReferences(database, table, uuid string) (database.References, error)
	// Get provides the corresponding model
	Get(database, table string, uuid string) (model.Model, error)
}

// DatabaseUpdate bundles updates together with the updated
// reference information
type DatabaseUpdate struct {
	ModelUpdates
	referenceUpdates database.References
}

func (u DatabaseUpdate) ForReferenceUpdates(do func(references database.References) error) error {
	refsCopy := database.References{}
	// since refsCopy is empty, this will just copy everything
	applyReferenceModifications(refsCopy, u.referenceUpdates)
	return do(refsCopy)
}

func NewDatabaseUpdate(updates ModelUpdates, references database.References) DatabaseUpdate {
	return DatabaseUpdate{
		ModelUpdates:     updates,
		referenceUpdates: references,
	}
}

// ProcessReferences tracks referential integrity for the provided set of
// updates. It returns an updated set of updates which includes additional
// updates and updated references as a result of the reference garbage
// collection described in RFC7047. These additional updates resulting from the
// reference garbage collection are also returned separately. Any constraint or
// referential integrity violation is returned as an error.
func ProcessReferences(dbModel model.DatabaseModel, provider ReferenceProvider, updates ModelUpdates) (ModelUpdates, ModelUpdates, database.References, error) {
	referenceTracker := newReferenceTracker(dbModel, provider)
	return referenceTracker.processReferences(updates)
}

type referenceTracker struct {
	dbModel  model.DatabaseModel
	provider ReferenceProvider

	// updates that are being processed
	updates ModelUpdates

	// references are the updated references by the set of updates processed
	references database.References

	// helper maps to track the rows that we are processing and their tables
	tracked map[string]string
	added   map[string]string
	deleted map[string]string
}

func newReferenceTracker(dbModel model.DatabaseModel, provider ReferenceProvider) *referenceTracker {
	return &referenceTracker{
		dbModel:  dbModel,
		provider: provider,
	}
}

func (rt *referenceTracker) processReferences(updates ModelUpdates) (ModelUpdates, ModelUpdates, database.References, error) {
	rt.updates = updates
	rt.tracked = make(map[string]string)
	rt.added = make(map[string]string)
	rt.deleted = make(map[string]string)
	rt.references = make(database.References)

	referenceUpdates, err := rt.processReferencesLoop(updates)
	if err != nil {
		return ModelUpdates{}, ModelUpdates{}, nil, err
	}

	// merge the updates generated from reference tracking into the main updates
	err = updates.Merge(rt.dbModel, referenceUpdates)
	if err != nil {
		return ModelUpdates{}, ModelUpdates{}, nil, err
	}

	return updates, referenceUpdates, rt.references, nil
}

func (rt *referenceTracker) processReferencesLoop(updates ModelUpdates) (ModelUpdates, error) {
	referenceUpdates := ModelUpdates{}

	// references can be transitive and deleting them can lead to further
	// references having to be removed so loop until there are no updates to be
	// made
	for len(updates.updates) > 0 {
		// update the references from the updates
		err := rt.processModelUpdates(updates)
		if err != nil {
			return ModelUpdates{}, err
		}

		// process strong reference integrity
		updates, err = rt.processStrongReferences()
		if err != nil {
			return ModelUpdates{}, err
		}

		// process weak reference integrity
		weakUpdates, err := rt.processWeakReferences()
		if err != nil {
			return ModelUpdates{}, err
		}

		// merge strong and weak reference updates
		err = updates.Merge(rt.dbModel, weakUpdates)
		if err != nil {
			return ModelUpdates{}, err
		}

		// merge updates from this iteration to the overall reference updates
		err = referenceUpdates.Merge(rt.dbModel, updates)
		if err != nil {
			return ModelUpdates{}, err
		}
	}

	return referenceUpdates, nil
}

// processModelUpdates keeps track of the updated references by a set of updates
func (rt *referenceTracker) processModelUpdates(updates ModelUpdates) error {
	tables := updates.GetUpdatedTables()
	for _, table := range tables {
		err := updates.ForEachRowUpdate(table, func(uuid string, row ovsdb.RowUpdate2) error {
			return rt.processRowUpdate(table, uuid, &row)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// processRowUpdate keeps track of the updated references by a given row update
func (rt *referenceTracker) processRowUpdate(table, uuid string, row *ovsdb.RowUpdate2) error {

	// getReferencesFromRowModify extracts updated references from the
	// modifications. Following the same strategy as the modify field of Update2
	// notification, it will extract a difference, that is, both old removed
	// references and new added references are extracted. This difference will
	// then be applied to currently tracked references to come up with the
	// updated references.

	// For more info on the modify field of Update2 notification and the
	// strategy used to apply differences, check
	// https://docs.openvswitch.org/en/latest/ref/ovsdb-server.7/#update2-notification

	var updateRefs database.References
	switch {
	case row.Delete != nil:
		rt.deleted[uuid] = table
		updateRefs = getReferenceModificationsFromRow(&rt.dbModel, table, uuid, row.Old, row.Old)
	case row.Modify != nil:
		updateRefs = getReferenceModificationsFromRow(&rt.dbModel, table, uuid, row.Modify, row.Old)
	case row.Insert != nil:
		if !isRoot(&rt.dbModel, table) {
			// track rows added that are not part of the root set, we might need
			// to delete those later
			rt.added[uuid] = table
			rt.tracked[uuid] = table
		}
		updateRefs = getReferenceModificationsFromRow(&rt.dbModel, table, uuid, row.Insert, nil)
	}

	// (lazy) initialize existing references to the same rows from the database
	for spec, refs := range updateRefs {
		for to := range refs {
			err := rt.initReferences(spec.ToTable, to)
			if err != nil {
				return err
			}
		}
	}

	// apply the reference modifications to the initialized references
	applyReferenceModifications(rt.references, updateRefs)

	return nil
}

// processStrongReferences adds delete operations for rows that are not part of
// the root set and are no longer strongly referenced. Returns a referential
// integrity violation if a nonexistent row is strongly referenced or a strongly
// referenced row has been deleted.
func (rt *referenceTracker) processStrongReferences() (ModelUpdates, error) {
	// make sure that we are tracking the references to the deleted rows
	err := rt.initReferencesOfDeletedRows()
	if err != nil {
		return ModelUpdates{}, err
	}

	// track if rows are referenced or not
	isReferenced := map[string]bool{}

	// go over the updated references
	for spec, refs := range rt.references {

		// we only care about strong references
		if !isStrong(&rt.dbModel, spec) {
			continue
		}

		for to, from := range refs {
			// check if the referenced row exists
			exists, err := rt.rowExists(spec.ToTable, to)
			if err != nil {
				return ModelUpdates{}, err
			}
			if !exists {
				for _, uuid := range from {
					// strong reference to a row that does not exist
					return ModelUpdates{}, ovsdb.NewReferentialIntegrityViolation(fmt.Sprintf(
						"Table %s column %s row %s references nonexistent or deleted row %s in table %s",
						spec.FromTable, spec.FromColumn, uuid, to, spec.ToTable))
				}
				// we deleted the row ourselves on a previous loop
				continue
			}

			// track if this row is referenced from this location spec
			isReferenced[to] = isReferenced[to] || len(from) > 0
		}
	}

	// inserted rows that are unreferenced and not part of the root set will
	// silently be dropped from the updates
	for uuid := range rt.added {
		if isReferenced[uuid] {
			continue
		}
		isReferenced[uuid] = false
	}

	// delete rows that are not referenced
	updates := ModelUpdates{}
	for uuid, isReferenced := range isReferenced {
		if isReferenced {
			// row is still referenced, ignore
			continue
		}

		if rt.deleted[uuid] != "" {
			// already deleted, ignore
			continue
		}

		table := rt.tracked[uuid]
		if isRoot(&rt.dbModel, table) {
			// table is part of the root set, ignore
			continue
		}

		// delete row that is not part of the root set and is no longer
		// referenced
		update, err := rt.deleteRow(table, uuid)
		if err != nil {
			return ModelUpdates{}, err
		}
		err = updates.Merge(rt.dbModel, update)
		if err != nil {
			return ModelUpdates{}, err
		}
	}

	return updates, nil
}

// processWeakReferences deletes weak references to rows that were deleted.
// Returns a constraint violation if this results in invalid values
func (rt *referenceTracker) processWeakReferences() (ModelUpdates, error) {
	// make sure that we are tracking the references to rows that might have
	// been deleted as a result of strong reference garbage collection
	err := rt.initReferencesOfDeletedRows()
	if err != nil {
		return ModelUpdates{}, err
	}

	tables := map[string]string{}
	originalRows := map[string]ovsdb.Row{}
	updatedRows := map[string]ovsdb.Row{}

	for spec, refs := range rt.references {
		// fetch some reference information from the schema
		extendedType, minLenAllowed, refType, _ := refInfo(&rt.dbModel, spec.FromTable, spec.FromColumn, spec.FromValue)
		isEmptyAllowed := minLenAllowed == 0

		if refType != ovsdb.Weak {
			// we only care about weak references
			continue
		}

		for to, from := range refs {
			if len(from) == 0 {
				// not referenced from anywhere, ignore
				continue
			}

			// check if the referenced row exists
			exists, err := rt.rowExists(spec.ToTable, to)
			if err != nil {
				return ModelUpdates{}, err
			}
			if exists {
				// we only care about rows that have been deleted or otherwise
				// don't exist
				continue
			}

			// generate the updates to remove the references to deleted rows
			for _, uuid := range from {
				if _, ok := updatedRows[uuid]; !ok {
					updatedRows[uuid] = ovsdb.NewRow()
				}

				if rt.deleted[uuid] != "" {
					// already deleted, ignore
					continue
				}

				// fetch the original rows
				if originalRows[uuid] == nil {
					originalRow, err := rt.getRow(spec.FromTable, uuid)
					if err != nil {
						return ModelUpdates{}, err
					}
					if originalRow == nil {
						return ModelUpdates{}, fmt.Errorf("reference from non-existent model with uuid %s", uuid)
					}
					originalRows[uuid] = *originalRow
				}

				var becomesLen int
				switch extendedType {
				case ovsdb.TypeMap:
					// a map referencing the row
					// generate the mutation to remove the entry form the map
					originalMap := originalRows[uuid][spec.FromColumn].(ovsdb.OvsMap).GoMap
					var mutationMap map[interface{}]interface{}
					value, ok := updatedRows[uuid][spec.FromColumn]
					if !ok {
						mutationMap = map[interface{}]interface{}{}
					} else {
						mutationMap = value.(ovsdb.OvsMap).GoMap
					}
					// copy the map entries referencing the row from the original map
					mutationMap = copyMapKeyValues(originalMap, mutationMap, !spec.FromValue, ovsdb.UUID{GoUUID: to})

					// track the new length of the map
					if !isEmptyAllowed {
						becomesLen = len(originalMap) - len(mutationMap)
					}

					updatedRows[uuid][spec.FromColumn] = ovsdb.OvsMap{GoMap: mutationMap}

				case ovsdb.TypeSet:
					// a set referencing the row
					// generate the mutation to remove the entry form the set
					var mutationSet []interface{}
					value, ok := updatedRows[uuid][spec.FromColumn]
					if !ok {
						mutationSet = []interface{}{}
					} else {
						mutationSet = value.(ovsdb.OvsSet).GoSet
					}
					mutationSet = append(mutationSet, ovsdb.UUID{GoUUID: to})

					// track the new length of the set
					if !isEmptyAllowed {
						originalSet := originalRows[uuid][spec.FromColumn].(ovsdb.OvsSet).GoSet
						becomesLen = len(originalSet) - len(mutationSet)
					}

					updatedRows[uuid][spec.FromColumn] = ovsdb.OvsSet{GoSet: mutationSet}

				case ovsdb.TypeUUID:
					// this is an atomic UUID value that needs to be cleared
					updatedRows[uuid][spec.FromColumn] = nil
					becomesLen = 0
				}

				if becomesLen < minLenAllowed {
					return ModelUpdates{}, ovsdb.NewConstraintViolation(fmt.Sprintf(
						"Deletion of a weak reference to a deleted (or never-existing) row from column %s in table %s "+
							"row %s caused this column to have an invalid length.",
						spec.FromColumn, spec.FromTable, uuid))
				}

				// track the table of the row we are going to update
				tables[uuid] = spec.FromTable
			}
		}
	}

	// process the updates
	updates := ModelUpdates{}
	for uuid, rowUpdate := range updatedRows {
		update, err := rt.updateRow(tables[uuid], uuid, rowUpdate)
		if err != nil {
			return ModelUpdates{}, err
		}
		err = updates.Merge(rt.dbModel, update)
		if err != nil {
			return ModelUpdates{}, err
		}
	}

	return updates, nil
}

func copyMapKeyValues(from, to map[interface{}]interface{}, isKey bool, keyValue ovsdb.UUID) map[interface{}]interface{} {
	if isKey {
		to[keyValue] = from[keyValue]
		return to
	}
	for key, value := range from {
		if value.(ovsdb.UUID) == keyValue {
			to[key] = from[key]
		}
	}
	return to
}

// initReferences initializes the references to the provided row from the
// database
func (rt *referenceTracker) initReferences(table, uuid string) error {
	if _, ok := rt.tracked[uuid]; ok {
		// already initialized
		return nil
	}
	existingRefs, err := rt.provider.GetReferences(rt.dbModel.Client().Name(), table, uuid)
	if err != nil {
		return err
	}
	rt.references.UpdateReferences(existingRefs)
	rt.tracked[uuid] = table
	return nil
}

func (rt *referenceTracker) initReferencesOfDeletedRows() error {
	for uuid, table := range rt.deleted {
		err := rt.initReferences(table, uuid)
		if err != nil {
			return err
		}
	}
	return nil
}

// deleteRow adds an update to delete the provided row.
func (rt *referenceTracker) deleteRow(table, uuid string) (ModelUpdates, error) {
	model, err := rt.getModel(table, uuid)
	if err != nil {
		return ModelUpdates{}, err
	}
	row, err := rt.getRow(table, uuid)
	if err != nil {
		return ModelUpdates{}, err
	}

	updates := ModelUpdates{}
	update := ovsdb.RowUpdate2{Delete: &ovsdb.Row{}, Old: row}
	err = updates.AddRowUpdate2(rt.dbModel, table, uuid, model, update)

	rt.deleted[uuid] = table

	return updates, err
}

// updateRow generates updates for the provided row
func (rt *referenceTracker) updateRow(table, uuid string, row ovsdb.Row) (ModelUpdates, error) {
	model, err := rt.getModel(table, uuid)
	if err != nil {
		return ModelUpdates{}, err
	}

	// In agreement with processWeakReferences, columns with values are assumed
	// to be values of sets or maps that need to be mutated for deletion.
	// Columns with no values are assumed to be atomic optional values that need
	// to be cleared with an update.

	mutations := make([]ovsdb.Mutation, 0, len(row))
	update := ovsdb.Row{}
	for column, value := range row {
		if value != nil {
			mutations = append(mutations, *ovsdb.NewMutation(column, ovsdb.MutateOperationDelete, value))
			continue
		}
		update[column] = ovsdb.OvsSet{GoSet: []interface{}{}}
	}

	updates := ModelUpdates{}

	if len(mutations) > 0 {
		err = updates.AddOperation(rt.dbModel, table, uuid, model, &ovsdb.Operation{
			Op:        ovsdb.OperationMutate,
			Table:     table,
			Mutations: mutations,
			Where:     []ovsdb.Condition{ovsdb.NewCondition("_uuid", ovsdb.ConditionEqual, ovsdb.UUID{GoUUID: uuid})},
		})
		if err != nil {
			return ModelUpdates{}, err
		}
	}

	if len(update) > 0 {
		err = updates.AddOperation(rt.dbModel, table, uuid, model, &ovsdb.Operation{
			Op:    ovsdb.OperationUpdate,
			Table: table,
			Row:   update,
			Where: []ovsdb.Condition{ovsdb.NewCondition("_uuid", ovsdb.ConditionEqual, ovsdb.UUID{GoUUID: uuid})},
		})
		if err != nil {
			return ModelUpdates{}, err
		}
	}

	return updates, nil
}

// getModel gets the model from the updates or the database
func (rt *referenceTracker) getModel(table, uuid string) (model.Model, error) {
	if _, deleted := rt.deleted[uuid]; deleted {
		// model has been deleted
		return nil, nil
	}
	// look for the model in the updates
	model := rt.updates.GetModel(table, uuid)
	if model != nil {
		return model, nil
	}
	// look for the model in the database
	model, err := rt.provider.Get(rt.dbModel.Client().Name(), table, uuid)
	if err != nil {
		return nil, err
	}
	return model, nil
}

// getRow gets the row from the updates or the database
func (rt *referenceTracker) getRow(table, uuid string) (*ovsdb.Row, error) {
	if _, deleted := rt.deleted[uuid]; deleted {
		// row has been deleted
		return nil, nil
	}
	// look for the row in the updates
	row := rt.updates.GetRow(table, uuid)
	if row != nil {
		return row, nil
	}
	// look for the model in the database and build the row
	model, err := rt.provider.Get(rt.dbModel.Client().Name(), table, uuid)
	if err != nil {
		return nil, err
	}
	info, err := rt.dbModel.NewModelInfo(model)
	if err != nil {
		return nil, err
	}
	newRow, err := rt.dbModel.Mapper.NewRow(info)
	if err != nil {
		return nil, err
	}
	return &newRow, nil
}

// rowExists returns whether the row exists either in the updates or the database
func (rt *referenceTracker) rowExists(table, uuid string) (bool, error) {
	model, err := rt.getModel(table, uuid)
	return model != nil, err
}

func getReferenceModificationsFromRow(dbModel *model.DatabaseModel, table, uuid string, modify, old *ovsdb.Row) database.References {
	refs := database.References{}
	for column, value := range *modify {
		var oldValue interface{}
		if old != nil {
			oldValue = (*old)[column]
		}
		crefs := getReferenceModificationsFromColumn(dbModel, table, uuid, column, value, oldValue)
		refs.UpdateReferences(crefs)
	}
	return refs
}

func getReferenceModificationsFromColumn(dbModel *model.DatabaseModel, table, uuid, column string, modify, old interface{}) database.References {
	switch v := modify.(type) {
	case ovsdb.UUID:
		var oldUUID ovsdb.UUID
		if old != nil {
			oldUUID = old.(ovsdb.UUID)
		}
		return getReferenceModificationsFromAtom(dbModel, table, uuid, column, v, oldUUID)
	case ovsdb.OvsSet:
		var oldSet ovsdb.OvsSet
		if old != nil {
			oldSet = old.(ovsdb.OvsSet)
		}
		return getReferenceModificationsFromSet(dbModel, table, uuid, column, v, oldSet)
	case ovsdb.OvsMap:
		return getReferenceModificationsFromMap(dbModel, table, uuid, column, v)
	}
	return nil
}

func getReferenceModificationsFromMap(dbModel *model.DatabaseModel, table, uuid, column string, value ovsdb.OvsMap) database.References {
	if len(value.GoMap) == 0 {
		return nil
	}

	// get the referenced table
	keyRefTable := refTable(dbModel, table, column, false)
	valueRefTable := refTable(dbModel, table, column, true)
	if keyRefTable == "" && valueRefTable == "" {
		return nil
	}

	from := uuid
	keySpec := database.ReferenceSpec{ToTable: keyRefTable, FromTable: table, FromColumn: column, FromValue: false}
	valueSpec := database.ReferenceSpec{ToTable: valueRefTable, FromTable: table, FromColumn: column, FromValue: true}

	refs := database.References{}
	for k, v := range value.GoMap {
		if keyRefTable != "" {
			switch to := k.(type) {
			case ovsdb.UUID:
				if _, ok := refs[keySpec]; !ok {
					refs[keySpec] = database.Reference{to.GoUUID: []string{from}}
				} else if _, ok := refs[keySpec][to.GoUUID]; !ok {
					refs[keySpec][to.GoUUID] = append(refs[keySpec][to.GoUUID], from)
				}
			}
		}
		if valueRefTable != "" {
			switch to := v.(type) {
			case ovsdb.UUID:
				if _, ok := refs[valueSpec]; !ok {
					refs[valueSpec] = database.Reference{to.GoUUID: []string{from}}
				} else if _, ok := refs[valueSpec][to.GoUUID]; !ok {
					refs[valueSpec][to.GoUUID] = append(refs[valueSpec][to.GoUUID], from)
				}
			}
		}
	}

	return refs
}

func getReferenceModificationsFromSet(dbModel *model.DatabaseModel, table, uuid, column string, modify, old ovsdb.OvsSet) database.References {
	// if the modify set is empty, it means the op is clearing an atomic value
	// so pick the old value instead
	value := modify
	if len(modify.GoSet) == 0 {
		value = old
	}

	if len(value.GoSet) == 0 {
		return nil
	}

	// get the referenced table
	refTable := refTable(dbModel, table, column, false)
	if refTable == "" {
		return nil
	}

	spec := database.ReferenceSpec{ToTable: refTable, FromTable: table, FromColumn: column}
	from := uuid
	refs := database.References{spec: database.Reference{}}
	for _, v := range value.GoSet {
		switch to := v.(type) {
		case ovsdb.UUID:
			refs[spec][to.GoUUID] = append(refs[spec][to.GoUUID], from)
		}
	}
	return refs
}

func getReferenceModificationsFromAtom(dbModel *model.DatabaseModel, table, uuid, column string, modify, old ovsdb.UUID) database.References {
	// get the referenced table
	refTable := refTable(dbModel, table, column, false)
	if refTable == "" {
		return nil
	}
	spec := database.ReferenceSpec{ToTable: refTable, FromTable: table, FromColumn: column}
	from := uuid
	to := modify.GoUUID
	refs := database.References{spec: {to: {from}}}
	if old.GoUUID != "" {
		// extract the old value as well
		refs[spec][old.GoUUID] = []string{from}
	}
	return refs
}

// applyReferenceModifications updates references in 'a' from those in 'b'
func applyReferenceModifications(a, b database.References) {
	for spec, bv := range b {
		for to, bfrom := range bv {
			if av, ok := a[spec]; ok {
				if afrom, ok := av[to]; ok {
					r, _ := applyDifference(afrom, bfrom)
					av[to] = r.([]string)
				} else {
					// this reference is not in 'a', so add it
					av[to] = bfrom
				}
			} else {
				// this reference is not in 'a', so add it
				a[spec] = database.Reference{to: bfrom}
			}
		}
	}
}

func refInfo(dbModel *model.DatabaseModel, table, column string, mapValue bool) (ovsdb.ExtendedType, int, ovsdb.RefType, string) {
	tSchema := dbModel.Schema.Table(table)
	if tSchema == nil {
		panic(fmt.Sprintf("unexpected schema error: no schema for table %s", table))
	}

	cSchema := tSchema.Column(column)
	if cSchema == nil {
		panic(fmt.Sprintf("unexpected schema error: no schema for column %s", column))
	}

	cType := cSchema.TypeObj
	if cType == nil {
		// this is not a reference
		return "", 0, "", ""
	}

	var bType *ovsdb.BaseType
	switch {
	case !mapValue && cType.Key != nil:
		bType = cType.Key
	case mapValue && cType.Value != nil:
		bType = cType.Value
	default:
		panic(fmt.Sprintf("unexpected schema error: no schema for map value on column %s", column))
	}
	if bType.Type != ovsdb.TypeUUID {
		// this is not a reference
		return "", 0, "", ""
	}

	// treat optional values represented with sets as atomic UUIDs
	extendedType := cSchema.Type
	if extendedType == ovsdb.TypeSet && cType.Min() == 0 && cType.Max() == 1 {
		extendedType = ovsdb.TypeUUID
	}

	rType, err := bType.RefType()
	if err != nil {
		panic(fmt.Sprintf("unexpected schema error: %v", err))
	}

	rTable, err := bType.RefTable()
	if err != nil {
		panic(fmt.Sprintf("unexpected schema error: %v", err))
	}

	return extendedType, cType.Min(), rType, rTable
}

func refTable(dbModel *model.DatabaseModel, table, column string, mapValue bool) ovsdb.RefType {
	_, _, _, refTable := refInfo(dbModel, table, column, mapValue)
	return refTable
}

func isRoot(dbModel *model.DatabaseModel, table string) bool {
	isRoot, err := dbModel.Schema.IsRoot(table)
	if err != nil {
		panic(fmt.Sprintf("unexpected schema error: %v", err))
	}
	return isRoot
}

func isStrong(dbModel *model.DatabaseModel, spec database.ReferenceSpec) bool {
	_, _, refType, _ := refInfo(dbModel, spec.FromTable, spec.FromColumn, spec.FromValue)
	return refType == ovsdb.Strong
}

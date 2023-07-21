package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/libovsdb/updates"
)

const (
	updateEvent     = "update"
	addEvent        = "add"
	deleteEvent     = "delete"
	bufferSize      = 65536
	columnDelimiter = ","
	keyDelimiter    = "|"
)

// ErrCacheInconsistent is an error that can occur when an operation
// would cause the cache to be inconsistent
type ErrCacheInconsistent struct {
	details string
}

// Error implements the error interface
func (e *ErrCacheInconsistent) Error() string {
	msg := "cache inconsistent"
	if e.details != "" {
		msg += ": " + e.details
	}
	return msg
}

func NewErrCacheInconsistent(details string) *ErrCacheInconsistent {
	return &ErrCacheInconsistent{
		details: details,
	}
}

// ErrIndexExists is returned when an item in the database cannot be inserted due to existing indexes
type ErrIndexExists struct {
	Table    string
	Value    interface{}
	Index    string
	New      string
	Existing []string
}

func (e *ErrIndexExists) Error() string {
	return fmt.Sprintf("cannot insert %s in the %s table. item %s has identical indexes. index: %s, value: %v", e.New, e.Table, e.Existing, e.Index, e.Value)
}

func NewIndexExistsError(table string, value interface{}, index string, new string, existing []string) *ErrIndexExists {
	return &ErrIndexExists{
		table, value, index, new, existing,
	}
}

// map of unique values to uuids
type valueToUUIDs map[interface{}]uuidset

// map of column name(s) to unique values, to UUIDs
type columnToValue map[index]valueToUUIDs

// index is the type used to implement multiple cache indexes
type index string

// indexType is the type of index
type indexType uint

const (
	schemaIndexType indexType = iota
	clientIndexType
)

// indexSpec contains details about an index
type indexSpec struct {
	index     index
	columns   []model.ColumnKey
	indexType indexType
}

func (s indexSpec) isClientIndex() bool {
	return s.indexType == clientIndexType
}

func (s indexSpec) isSchemaIndex() bool {
	return s.indexType == schemaIndexType
}

// newIndex builds a index from a list of columns
func newIndexFromColumns(columns ...string) index {
	sort.Strings(columns)
	return index(strings.Join(columns, columnDelimiter))
}

// newIndexFromColumnKeys builds a index from a list of column keys
func newIndexFromColumnKeys(columnsKeys ...model.ColumnKey) index {
	// RFC 7047 says that Indexes is a [<column-set>] and "Each <column-set> is a set of
	// columns whose values, taken together within any given row, must be
	// unique within the table". We'll store the column names, separated by comma
	// as we'll assume (RFC is not clear), that comma isn't valid in a <id>
	columns := make([]string, 0, len(columnsKeys))
	columnsMap := map[string]struct{}{}
	for _, columnKey := range columnsKeys {
		var column string
		if columnKey.Key != nil {
			column = fmt.Sprintf("%s%s%v", columnKey.Column, keyDelimiter, columnKey.Key)
		} else {
			column = columnKey.Column
		}
		if _, found := columnsMap[column]; !found {
			columns = append(columns, column)
			columnsMap[column] = struct{}{}
		}
	}
	return newIndexFromColumns(columns...)
}

// newColumnKeysFromColumns builds a list of column keys from a list of columns
func newColumnKeysFromColumns(columns ...string) []model.ColumnKey {
	columnKeys := make([]model.ColumnKey, len(columns))
	for i, column := range columns {
		columnKeys[i] = model.ColumnKey{Column: column}
	}
	return columnKeys
}

// RowCache is a collections of Models hashed by UUID
type RowCache struct {
	name       string
	dbModel    model.DatabaseModel
	dataType   reflect.Type
	cache      map[string]model.Model
	indexSpecs []indexSpec
	indexes    columnToValue
	mutex      sync.RWMutex
}

// rowByUUID returns one model from the cache by UUID. Caller must hold the row
// cache lock.
func (r *RowCache) rowByUUID(uuid string) model.Model {
	if row, ok := r.cache[uuid]; ok {
		return model.Clone(row)
	}
	return nil
}

// Row returns one model from the cache by UUID
func (r *RowCache) Row(uuid string) model.Model {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	return r.rowByUUID(uuid)
}

func (r *RowCache) HasRow(uuid string) bool {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	_, found := r.cache[uuid]
	return found
}

// rowsByModels searches the cache to find all rows matching any of the provided
// models, either by UUID or indexes. An error is returned if the model schema
// has no UUID field, or if the provided models are not all the same type.
func (r *RowCache) rowsByModels(models []model.Model, useClientIndexes bool) (map[string]model.Model, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	results := make(map[string]model.Model, len(models))
	for _, m := range models {
		if reflect.TypeOf(m) != r.dataType {
			return nil, fmt.Errorf("model type %s didn't match expected row type %s", reflect.TypeOf(m), r.dataType)
		}
		info, _ := r.dbModel.NewModelInfo(m)
		field, err := info.FieldByColumn("_uuid")
		if err != nil {
			return nil, err
		}
		if uuid := field.(string); uuid != "" {
			if _, ok := results[uuid]; !ok {
				if row := r.rowByUUID(uuid); row != nil {
					results[uuid] = row
					continue
				}
			}
		}

		// indexSpecs are ordered, schema indexes go first, then client indexes
		for _, indexSpec := range r.indexSpecs {
			if indexSpec.isClientIndex() && !useClientIndexes {
				// Given the ordered indexSpecs, we can break here if we reach the
				// first client index
				break
			}
			val, err := valueFromIndex(info, indexSpec.columns)
			if err != nil {
				continue
			}
			vals := r.indexes[indexSpec.index]
			if uuids, ok := vals[val]; ok {
				for uuid := range uuids {
					if _, ok := results[uuid]; !ok {
						results[uuid] = r.rowByUUID(uuid)
					}
				}
				// Break after handling the first found index
				// to ensure we preserve index order preference
				break
			}
		}
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results, nil
}

// RowByModel searches the cache by UUID and schema indexes. UUID search is
// performed first. Then schema indexes are evaluated in turn by the same order
// with which they are defined in the schema. The model for the first matching
// index is returned along with its UUID. An empty string and nil is returned if
// no Model is found.
func (r *RowCache) RowByModel(m model.Model) (string, model.Model, error) {
	models, err := r.rowsByModels([]model.Model{m}, false)
	if err != nil {
		return "", nil, err
	}
	for uuid, model := range models {
		return uuid, model, nil
	}
	return "", nil, nil
}

// RowsByModels searches the cache by UUID, schema indexes and client indexes.
// UUID search is performed first. Schema indexes are evaluated next in turn by
// the same order with which they are defined in the schema. Finally, client
// indexes are evaluated in turn by the same order with which they are defined
// in the client DB model. The models for the first matching index are returned,
// which might be more than 1 if they were found through a client index since in
// that case uniqueness is not enforced. Nil is returned if no Model is found.
func (r *RowCache) RowsByModels(models []model.Model) (map[string]model.Model, error) {
	return r.rowsByModels(models, true)
}

// Create writes the provided content to the cache
func (r *RowCache) Create(uuid string, m model.Model, checkIndexes bool) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if _, ok := r.cache[uuid]; ok {
		return NewErrCacheInconsistent(fmt.Sprintf("cannot create row %s as it already exists", uuid))
	}
	if reflect.TypeOf(m) != r.dataType {
		return fmt.Errorf("expected data of type %s, but got %s", r.dataType.String(), reflect.TypeOf(m).String())
	}
	info, err := r.dbModel.NewModelInfo(m)
	if err != nil {
		return err
	}
	addIndexes := r.newIndexes()
	for _, indexSpec := range r.indexSpecs {
		index := indexSpec.index
		val, err := valueFromIndex(info, indexSpec.columns)
		if err != nil {
			return err
		}

		uuidset := newUUIDSet(uuid)

		vals := r.indexes[index]
		existing := vals[val]
		if checkIndexes && indexSpec.isSchemaIndex() && !existing.empty() && !existing.equals(uuidset) {
			return NewIndexExistsError(r.name, val, string(index), uuid, existing.list())
		}

		addIndexes[index][val] = uuidset
	}

	// write indexes
	for _, indexSpec := range r.indexSpecs {
		index := indexSpec.index
		for k, v := range addIndexes[index] {
			if indexSpec.isSchemaIndex() {
				r.indexes[index][k] = v
			} else {
				r.indexes[index][k] = addUUIDSet(r.indexes[index][k], v)
			}
		}
	}

	r.cache[uuid] = model.Clone(m)
	return nil
}

// Update updates the content in the cache and returns the original (pre-update) model
func (r *RowCache) Update(uuid string, m model.Model, checkIndexes bool) (model.Model, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if _, ok := r.cache[uuid]; !ok {
		return nil, NewErrCacheInconsistent(fmt.Sprintf("cannot update row %s as it does not exist in the cache", uuid))
	}
	oldRow := model.Clone(r.cache[uuid])
	oldInfo, err := r.dbModel.NewModelInfo(oldRow)
	if err != nil {
		return nil, err
	}
	newInfo, err := r.dbModel.NewModelInfo(m)
	if err != nil {
		return nil, err
	}

	addIndexes := r.newIndexes()
	removeIndexes := r.newIndexes()
	var errs []error
	for _, indexSpec := range r.indexSpecs {
		index := indexSpec.index
		var err error
		oldVal, err := valueFromIndex(oldInfo, indexSpec.columns)
		if err != nil {
			return nil, err
		}
		newVal, err := valueFromIndex(newInfo, indexSpec.columns)
		if err != nil {
			return nil, err
		}

		// if old and new values are the same, don't worry
		if oldVal == newVal {
			continue
		}
		// old and new values are NOT the same

		uuidset := newUUIDSet(uuid)

		// check that there are no conflicts
		vals := r.indexes[index]
		existing := vals[newVal]
		if checkIndexes && indexSpec.isSchemaIndex() && !existing.empty() && !existing.equals(uuidset) {
			errs = append(errs, NewIndexExistsError(
				r.name,
				newVal,
				string(index),
				uuid,
				existing.list(),
			))
		}

		addIndexes[index][newVal] = uuidset
		removeIndexes[index][oldVal] = uuidset
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%+v", errs)
	}

	// write indexes
	for _, indexSpec := range r.indexSpecs {
		index := indexSpec.index
		for k, v := range addIndexes[index] {
			if indexSpec.isSchemaIndex() {
				r.indexes[index][k] = v
			} else {
				r.indexes[index][k] = addUUIDSet(r.indexes[index][k], v)
			}
		}
		for k, v := range removeIndexes[index] {
			if indexSpec.isSchemaIndex() || substractUUIDSet(r.indexes[index][k], v).empty() {
				delete(r.indexes[index], k)
			}
		}
	}

	r.cache[uuid] = model.Clone(m)
	return oldRow, nil
}

// IndexExists checks if any of the schema indexes of the provided model is
// already in the cache under a different UUID.
func (r *RowCache) IndexExists(row model.Model) error {
	info, err := r.dbModel.NewModelInfo(row)
	if err != nil {
		return err
	}
	field, err := info.FieldByColumn("_uuid")
	if err != nil {
		return nil
	}
	uuid := field.(string)
	for _, indexSpec := range r.indexSpecs {
		if !indexSpec.isSchemaIndex() {
			// Given the ordered indexSpecs, we can break here if we reach the
			// first non schema index
			break
		}
		index := indexSpec.index
		val, err := valueFromIndex(info, indexSpec.columns)
		if err != nil {
			continue
		}
		vals := r.indexes[index]
		existing := vals[val]
		if !existing.empty() && !existing.equals(newUUIDSet(uuid)) {
			return NewIndexExistsError(
				r.name,
				val,
				string(index),
				uuid,
				existing.list(),
			)
		}
	}
	return nil
}

// Delete deletes a row from the cache
func (r *RowCache) Delete(uuid string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if _, ok := r.cache[uuid]; !ok {
		return NewErrCacheInconsistent(fmt.Sprintf("cannot delete row %s as it does not exist in the cache", uuid))
	}
	oldRow := r.cache[uuid]
	oldInfo, err := r.dbModel.NewModelInfo(oldRow)
	if err != nil {
		return err
	}

	removeIndexes := r.newIndexes()
	for _, indexSpec := range r.indexSpecs {
		index := indexSpec.index
		oldVal, err := valueFromIndex(oldInfo, indexSpec.columns)
		if err != nil {
			return err
		}

		removeIndexes[index][oldVal] = newUUIDSet(uuid)
	}

	// write indexes
	for _, indexSpec := range r.indexSpecs {
		index := indexSpec.index
		for k, v := range removeIndexes[index] {
			// only remove the index if it is pointing to this uuid
			// otherwise we can cause a consistency issue if we've processed
			// updates out of order
			if substractUUIDSet(r.indexes[index][k], v).empty() {
				delete(r.indexes[index], k)
			}
		}
	}

	delete(r.cache, uuid)
	return nil
}

// Rows returns a copy of all Rows in the Cache
func (r *RowCache) Rows() map[string]model.Model {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	result := make(map[string]model.Model)
	for k, v := range r.cache {
		result[k] = model.Clone(v)
	}
	return result
}

// RowsShallow returns a clone'd list of f all Rows in the cache, but does not
// clone the underlying objects. Therefore, the objects returned are READ ONLY.
// This is, however, thread safe, as the cached objects are cloned before being updated
// when modifications come in.
func (r *RowCache) RowsShallow() map[string]model.Model {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	result := make(map[string]model.Model, len(r.cache))
	for k, v := range r.cache {
		result[k] = v
	}
	return result
}

// uuidsByConditionsAsIndexes checks possible indexes that can be built with a
// subset of the provided conditions and returns the uuids for the models that
// match that subset of conditions. If no conditions could be used as indexes,
// returns nil. Note that this method does not necessarily match all the
// provided conditions. Thus the caller is required to evaluate all the
// conditions against the returned candidates. This is only useful to obtain, as
// quick as possible, via indexes, a reduced list of candidate models that might
// match all conditions, which should be better than just evaluating all
// conditions against all rows of a table.
//
//nolint:gocyclo // warns overall function is complex but ignores inner functions
func (r *RowCache) uuidsByConditionsAsIndexes(conditions []ovsdb.Condition, nativeValues []interface{}) (uuidset, error) {
	type indexableCondition struct {
		column      string
		keys        []interface{}
		nativeValue interface{}
	}

	// build an indexable condition, more appropriate for our processing, from
	// an ovsdb condition. Only equality based conditions can be used as indexes
	// (or `includes` conditions on map values).
	toIndexableCondition := func(condition ovsdb.Condition, nativeValue interface{}) *indexableCondition {
		if condition.Column == "_uuid" {
			return nil
		}
		if condition.Function != ovsdb.ConditionEqual && condition.Function != ovsdb.ConditionIncludes {
			return nil
		}
		v := reflect.ValueOf(nativeValue)
		if !v.IsValid() {
			return nil
		}
		isSet := v.Kind() == reflect.Slice || v.Kind() == reflect.Array
		if condition.Function == ovsdb.ConditionIncludes && isSet {
			return nil
		}
		keys := []interface{}{}
		if v.Kind() == reflect.Map && condition.Function == ovsdb.ConditionIncludes {
			for _, key := range v.MapKeys() {
				keys = append(keys, key.Interface())
			}
		}
		return &indexableCondition{
			column:      condition.Column,
			keys:        keys,
			nativeValue: nativeValue,
		}
	}

	// for any given set of conditions, we need to check if an index uses the
	// same fields as the conditions
	indexMatchesConditions := func(spec indexSpec, conditions []*indexableCondition) bool {
		columnKeys := []model.ColumnKey{}
		for _, condition := range conditions {
			if len(condition.keys) == 0 {
				columnKeys = append(columnKeys, model.ColumnKey{Column: condition.column})
				continue
			}
			for _, key := range condition.keys {
				columnKeys = append(columnKeys, model.ColumnKey{Column: condition.column, Key: key})
			}
		}
		index := newIndexFromColumnKeys(columnKeys...)
		return index == spec.index
	}

	// for a specific set of conditions, check if an index can be built from
	// them and return the associated UUIDs
	evaluateConditionSetAsIndex := func(conditions []*indexableCondition) (uuidset, error) {
		// build a model with the values from the conditions
		m, err := r.dbModel.NewModel(r.name)
		if err != nil {
			return nil, err
		}
		info, err := r.dbModel.NewModelInfo(m)
		if err != nil {
			return nil, err
		}
		for _, conditions := range conditions {
			err := info.SetField(conditions.column, conditions.nativeValue)
			if err != nil {
				return nil, err
			}
		}
		for _, spec := range r.indexSpecs {
			if !indexMatchesConditions(spec, conditions) {
				continue
			}
			// if we have an index for those conditions, calculate the index
			// value. The models mapped to that value match the conditions.
			v, err := valueFromIndex(info, spec.columns)
			if err != nil {
				return nil, err
			}
			if v != nil {
				uuids := r.indexes[spec.index][v]
				if uuids == nil {
					// this set of conditions was represented by an index but
					// had no matches, return an empty set
					uuids = uuidset{}
				}
				return uuids, nil
			}
		}
		return nil, nil
	}

	// set of uuids that match the conditions as we evaluate them
	var matching uuidset

	// attempt to evaluate a set of conditions via indexes and intersect the
	// results against matches of previous sets
	intersectUUIDsFromConditionSet := func(indexableConditions []*indexableCondition) (bool, error) {
		uuids, err := evaluateConditionSetAsIndex(indexableConditions)
		if err != nil {
			return true, err
		}
		if matching == nil {
			matching = uuids
		} else if uuids != nil {
			matching = intersectUUIDSets(matching, uuids)
		}
		if matching != nil && len(matching) <= 1 {
			// if we had no matches or a single match, no point in continuing
			// searching for additional indexes. If we had a single match, it's
			// cheaper to just evaluate all conditions on it.
			return true, nil
		}
		return false, nil
	}

	// First, filter out conditions that cannot be matched against indexes. With
	// the remaining conditions build all possible subsets (the power set of all
	// conditions) and for any subset that is an index, intersect the obtained
	// uuids with the ones obtained from previous subsets
	matchUUIDsFromConditionsPowerSet := func() error {
		ps := [][]*indexableCondition{}
		// prime the power set with a first empty subset
		ps = append(ps, []*indexableCondition{})
		for i, condition := range conditions {
			nativeValue := nativeValues[i]
			iCondition := toIndexableCondition(condition, nativeValue)
			// this is not a condition we can use as an index, skip it
			if iCondition == nil {
				continue
			}
			// the power set is built appending the subsets that result from
			// adding each item to each of the previous subsets
			ss := make([][]*indexableCondition, len(ps))
			for j := range ss {
				ss[j] = make([]*indexableCondition, len(ps[j]), len(ps[j])+1)
				copy(ss[j], ps[j])
				ss[j] = append(ss[j], iCondition)
				// as we add them to the power set, attempt to evaluate this
				// subset of conditions as indexes
				stop, err := intersectUUIDsFromConditionSet(ss[j])
				if stop || err != nil {
					return err
				}
			}
			ps = append(ps, ss...)
		}
		return nil
	}

	// finally
	err := matchUUIDsFromConditionsPowerSet()
	return matching, err
}

// RowsByCondition searches models in the cache that match all conditions
func (r *RowCache) RowsByCondition(conditions []ovsdb.Condition) (map[string]model.Model, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	results := make(map[string]model.Model)
	schema := r.dbModel.Schema.Table(r.name)

	// no conditions matches all rows
	if len(conditions) == 0 {
		for uuid := range r.cache {
			results[uuid] = r.rowByUUID(uuid)
		}
		return results, nil
	}

	// one pass to obtain the native values
	nativeValues := make([]interface{}, 0, len(conditions))
	for _, condition := range conditions {
		tSchema := schema.Column(condition.Column)
		nativeValue, err := ovsdb.OvsToNative(tSchema, condition.Value)
		if err != nil {
			return nil, err
		}
		nativeValues = append(nativeValues, nativeValue)
	}

	// obtain all possible matches using conditions as indexes
	matching, err := r.uuidsByConditionsAsIndexes(conditions, nativeValues)
	if err != nil {
		return nil, err
	}

	// From the matches obtained with indexes, which might have not used all
	// conditions, continue trimming down the list explicitly evaluating the
	// conditions.
	for i, condition := range conditions {
		matchingCondition := uuidset{}

		if condition.Column == "_uuid" && (condition.Function == ovsdb.ConditionEqual || condition.Function == ovsdb.ConditionIncludes) {
			uuid, ok := nativeValues[i].(string)
			if !ok {
				panic(fmt.Sprintf("%+v is not a uuid", nativeValues[i]))
			}
			if _, found := r.cache[uuid]; found {
				matchingCondition.add(uuid)
			}
		} else {
			matchCondition := func(uuid string) error {
				row := r.cache[uuid]
				info, err := r.dbModel.NewModelInfo(row)
				if err != nil {
					return err
				}
				value, err := info.FieldByColumn(condition.Column)
				if err != nil {
					return err
				}
				ok, err := condition.Function.Evaluate(value, nativeValues[i])
				if err != nil {
					return err
				}
				if ok {
					matchingCondition.add(uuid)
				}
				return nil
			}
			if matching != nil {
				// we just need to consider rows that matched previous
				// conditions
				for uuid := range matching {
					err = matchCondition(uuid)
					if err != nil {
						return nil, err
					}
				}
			} else {
				// If this is the first condition we are able to check, just run
				// it by whole table
				for uuid := range r.cache {
					err = matchCondition(uuid)
					if err != nil {
						return nil, err
					}
				}
			}
		}
		if matching == nil {
			matching = matchingCondition
		} else {
			matching = intersectUUIDSets(matching, matchingCondition)
		}
		if matching.empty() {
			// no models match the conditions checked up to now, no need to
			// check remaining conditions
			break
		}
	}

	for uuid := range matching {
		results[uuid] = r.rowByUUID(uuid)
	}

	return results, nil
}

// Len returns the length of the cache
func (r *RowCache) Len() int {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	return len(r.cache)
}

func (r *RowCache) Index(columns ...string) (map[interface{}][]string, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	spec := newIndexFromColumns(columns...)
	index, ok := r.indexes[spec]
	if !ok {
		return nil, fmt.Errorf("%v is not an index", columns)
	}
	dbIndex := make(map[interface{}][]string, len(index))
	for k, v := range index {
		dbIndex[k] = v.list()
	}
	return dbIndex, nil
}

// EventHandler can handle events when the contents of the cache changes
type EventHandler interface {
	OnAdd(table string, model model.Model)
	OnUpdate(table string, old model.Model, new model.Model)
	OnDelete(table string, model model.Model)
}

// EventHandlerFuncs is a wrapper for the EventHandler interface
// It allows a caller to only implement the functions they need
type EventHandlerFuncs struct {
	AddFunc    func(table string, model model.Model)
	UpdateFunc func(table string, old model.Model, new model.Model)
	DeleteFunc func(table string, model model.Model)
}

// OnAdd calls AddFunc if it is not nil
func (e *EventHandlerFuncs) OnAdd(table string, model model.Model) {
	if e.AddFunc != nil {
		e.AddFunc(table, model)
	}
}

// OnUpdate calls UpdateFunc if it is not nil
func (e *EventHandlerFuncs) OnUpdate(table string, old, new model.Model) {
	if e.UpdateFunc != nil {
		e.UpdateFunc(table, old, new)
	}
}

// OnDelete calls DeleteFunc if it is not nil
func (e *EventHandlerFuncs) OnDelete(table string, row model.Model) {
	if e.DeleteFunc != nil {
		e.DeleteFunc(table, row)
	}
}

// TableCache contains a collection of RowCaches, hashed by name,
// and an array of EventHandlers that respond to cache updates
// It implements the ovsdb.NotificationHandler interface so it may
// handle update notifications
type TableCache struct {
	cache          map[string]*RowCache
	eventProcessor *eventProcessor
	dbModel        model.DatabaseModel
	ovsdb.NotificationHandler
	mutex  sync.RWMutex
	logger *logr.Logger
}

// Data is the type for data that can be prepopulated in the cache
type Data map[string]map[string]model.Model

// NewTableCache creates a new TableCache
func NewTableCache(dbModel model.DatabaseModel, data Data, logger *logr.Logger) (*TableCache, error) {
	if !dbModel.Valid() {
		return nil, fmt.Errorf("tablecache without valid databasemodel cannot be populated")
	}
	if logger == nil {
		l := stdr.NewWithOptions(log.New(os.Stderr, "", log.LstdFlags), stdr.Options{LogCaller: stdr.All}).WithName("cache")
		logger = &l
	} else {
		l := logger.WithName("cache")
		logger = &l
	}
	eventProcessor := newEventProcessor(bufferSize, logger)
	cache := make(map[string]*RowCache)
	tableTypes := dbModel.Types()
	for name := range dbModel.Schema.Tables {
		cache[name] = newRowCache(name, dbModel, tableTypes[name])
	}
	for table, rowData := range data {
		if _, ok := dbModel.Schema.Tables[table]; !ok {
			return nil, fmt.Errorf("table %s is not in schema", table)
		}
		rowCache := cache[table]
		for uuid, row := range rowData {
			if err := rowCache.Create(uuid, row, true); err != nil {
				return nil, err
			}
		}
	}
	return &TableCache{
		cache:          cache,
		eventProcessor: eventProcessor,
		dbModel:        dbModel,
		mutex:          sync.RWMutex{},
		logger:         logger,
	}, nil
}

// Mapper returns the mapper
func (t *TableCache) Mapper() mapper.Mapper {
	return t.dbModel.Mapper
}

// DatabaseModel returns the DatabaseModelRequest
func (t *TableCache) DatabaseModel() model.DatabaseModel {
	return t.dbModel
}

// Table returns the a Table from the cache with a given name
func (t *TableCache) Table(name string) *RowCache {
	t.mutex.RLock()
	defer t.mutex.RUnlock()
	if table, ok := t.cache[name]; ok {
		return table
	}
	return nil
}

// Tables returns a list of table names that are in the cache
func (t *TableCache) Tables() []string {
	t.mutex.RLock()
	defer t.mutex.RUnlock()
	var result []string
	for k := range t.cache {
		result = append(result, k)
	}
	return result
}

// Update implements the update method of the NotificationHandler interface
// this populates a channel with updates so they can be processed after the initial
// state has been Populated
func (t *TableCache) Update(context interface{}, tableUpdates ovsdb.TableUpdates) error {
	if len(tableUpdates) == 0 {
		return nil
	}
	if err := t.Populate(tableUpdates); err != nil {
		t.logger.Error(err, "during libovsdb cache populate")
		return err
	}
	return nil
}

// Update2 implements the update method of the NotificationHandler interface
// this populates a channel with updates so they can be processed after the initial
// state has been Populated
func (t *TableCache) Update2(context interface{}, tableUpdates ovsdb.TableUpdates2) error {
	if len(tableUpdates) == 0 {
		return nil
	}
	if err := t.Populate2(tableUpdates); err != nil {
		t.logger.Error(err, "during libovsdb cache populate2")
		return err
	}
	return nil
}

// Locked implements the locked method of the NotificationHandler interface
func (t *TableCache) Locked([]interface{}) {
}

// Stolen implements the stolen method of the NotificationHandler interface
func (t *TableCache) Stolen([]interface{}) {
}

// Echo implements the echo method of the NotificationHandler interface
func (t *TableCache) Echo([]interface{}) {
}

// Disconnected implements the disconnected method of the NotificationHandler interface
func (t *TableCache) Disconnected() {
}

// Populate adds data to the cache and places an event on the channel
func (t *TableCache) Populate(tableUpdates ovsdb.TableUpdates) error {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	for table := range t.dbModel.Types() {
		tu, ok := tableUpdates[table]
		if !ok {
			continue
		}
		tCache := t.cache[table]
		for uuid, row := range tu {
			t.logger.V(5).Info("processing update", "table", table, "uuid", uuid)
			update := updates.ModelUpdates{}
			current := tCache.cache[uuid]
			err := update.AddRowUpdate(t.dbModel, table, uuid, current, *row)
			if err != nil {
				return err
			}
			err = t.ApplyCacheUpdate(update)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Populate2 adds data to the cache and places an event on the channel
func (t *TableCache) Populate2(tableUpdates ovsdb.TableUpdates2) error {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	for table := range t.dbModel.Types() {
		tu, ok := tableUpdates[table]
		if !ok {
			continue
		}
		tCache := t.cache[table]
		for uuid, row := range tu {
			t.logger.V(5).Info("processing update", "table", table, "uuid", uuid)
			update := updates.ModelUpdates{}
			current := tCache.cache[uuid]
			if row.Initial == nil && row.Insert == nil && current == nil {
				return NewErrCacheInconsistent(fmt.Sprintf("row with uuid %s does not exist", uuid))
			}
			err := update.AddRowUpdate2(t.dbModel, table, uuid, current, *row)
			if err != nil {
				return err
			}
			err = t.ApplyCacheUpdate(update)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Purge drops all data in the cache and reinitializes it using the
// provided database model
func (t *TableCache) Purge(dbModel model.DatabaseModel) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.dbModel = dbModel
	tableTypes := t.dbModel.Types()
	for name := range t.dbModel.Schema.Tables {
		t.cache[name] = newRowCache(name, t.dbModel, tableTypes[name])
	}
}

// AddEventHandler registers the supplied EventHandler to receive cache events
func (t *TableCache) AddEventHandler(handler EventHandler) {
	t.eventProcessor.AddEventHandler(handler)
}

// Run starts the event processing and update processing loops.
// It blocks until the stop channel is closed.
// Once closed, it clears the updates/updates2 channels to ensure we don't process stale updates on a new connection
func (t *TableCache) Run(stopCh <-chan struct{}) {
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t.eventProcessor.Run(stopCh)
	}()
	wg.Wait()
}

// newRowCache creates a new row cache with the provided data
// if the data is nil, and empty RowCache will be created
func newRowCache(name string, dbModel model.DatabaseModel, dataType reflect.Type) *RowCache {
	schemaIndexes := dbModel.Schema.Table(name).Indexes
	clientIndexes := dbModel.Client().Indexes(name)

	r := &RowCache{
		name:       name,
		dbModel:    dbModel,
		indexSpecs: make([]indexSpec, 0, len(schemaIndexes)+len(clientIndexes)),
		dataType:   dataType,
		cache:      make(map[string]model.Model),
		mutex:      sync.RWMutex{},
	}

	// respect the order of indexes, add first schema indexes, then client
	// indexes
	indexes := map[index]indexSpec{}
	for _, columns := range schemaIndexes {
		columnKeys := newColumnKeysFromColumns(columns...)
		index := newIndexFromColumnKeys(columnKeys...)
		spec := indexSpec{index: index, columns: columnKeys, indexType: schemaIndexType}
		r.indexSpecs = append(r.indexSpecs, spec)
		indexes[index] = spec
	}
	for _, clientIndex := range clientIndexes {
		columnKeys := clientIndex.Columns
		index := newIndexFromColumnKeys(columnKeys...)
		// if this is already a DB index, ignore
		if _, ok := indexes[index]; ok {
			continue
		}
		spec := indexSpec{index: index, columns: columnKeys, indexType: clientIndexType}
		r.indexSpecs = append(r.indexSpecs, spec)
		indexes[index] = spec
	}

	r.indexes = r.newIndexes()
	return r
}

func (r *RowCache) newIndexes() columnToValue {
	c := make(columnToValue)
	for _, indexSpec := range r.indexSpecs {
		index := indexSpec.index
		c[index] = make(valueToUUIDs)
	}
	return c
}

// event encapsulates a cache event
type event struct {
	eventType string
	table     string
	old       model.Model
	new       model.Model
}

// eventProcessor handles the queueing and processing of cache events
type eventProcessor struct {
	events chan *event
	// handlersMutex locks the handlers array when we add a handler or dispatch events
	// we don't need a RWMutex in this case as we only have one thread reading and the write
	// volume is very low (i.e only when AddEventHandler is called)
	handlersMutex sync.Mutex
	handlers      []EventHandler
	logger        *logr.Logger
}

func newEventProcessor(capacity int, logger *logr.Logger) *eventProcessor {
	return &eventProcessor{
		events:   make(chan *event, capacity),
		handlers: []EventHandler{},
		logger:   logger,
	}
}

// AddEventHandler registers the supplied EventHandler with the eventProcessor
// EventHandlers MUST process events quickly, for example, pushing them to a queue
// to be processed by the client. Long Running handler functions adversely affect
// other handlers and MAY cause loss of data if the channel buffer is full
func (e *eventProcessor) AddEventHandler(handler EventHandler) {
	e.handlersMutex.Lock()
	defer e.handlersMutex.Unlock()
	e.handlers = append(e.handlers, handler)
}

// AddEvent writes an event to the channel
func (e *eventProcessor) AddEvent(eventType string, table string, old model.Model, new model.Model) {
	// We don't need to check for error here since there
	// is only a single writer. RPC is run in blocking mode
	event := event{
		eventType: eventType,
		table:     table,
		old:       old,
		new:       new,
	}
	select {
	case e.events <- &event:
		// noop
		return
	default:
		e.logger.V(0).Info("dropping event because event buffer is full")
	}
}

// Run runs the eventProcessor loop.
// It will block until the stopCh has been closed
// Otherwise it will wait for events to arrive on the event channel
// Once received, it will dispatch the event to each registered handler
func (e *eventProcessor) Run(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		case event := <-e.events:
			e.handlersMutex.Lock()
			for _, handler := range e.handlers {
				switch event.eventType {
				case addEvent:
					handler.OnAdd(event.table, event.new)
				case updateEvent:
					handler.OnUpdate(event.table, event.old, event.new)
				case deleteEvent:
					handler.OnDelete(event.table, event.old)
				}
			}
			e.handlersMutex.Unlock()
		}
	}
}

type cacheUpdate interface {
	GetUpdatedTables() []string
	ForEachModelUpdate(table string, do func(uuid string, old, new model.Model) error) error
}

func (t *TableCache) ApplyCacheUpdate(update cacheUpdate) error {
	tables := update.GetUpdatedTables()
	for _, table := range tables {
		tCache := t.cache[table]
		err := update.ForEachModelUpdate(table, func(uuid string, old, new model.Model) error {
			switch {
			case old == nil && new != nil:
				t.logger.V(5).Info("inserting model", "table", table, "uuid", uuid, "model", new)
				err := tCache.Create(uuid, new, false)
				if err != nil {
					return err
				}
				t.eventProcessor.AddEvent(addEvent, table, nil, new)
			case old != nil && new != nil:
				t.logger.V(5).Info("updating model", "table", table, "uuid", uuid, "old", old, "new", new)
				_, err := tCache.Update(uuid, new, false)
				if err != nil {
					return err
				}
				t.eventProcessor.AddEvent(updateEvent, table, old, new)
			case new == nil:
				t.logger.V(5).Info("deleting model", "table", table, "uuid", uuid, "model", old)
				err := tCache.Delete(uuid)
				if err != nil {
					return err
				}
				t.eventProcessor.AddEvent(deleteEvent, table, old, nil)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func valueFromIndex(info *mapper.Info, columnKeys []model.ColumnKey) (interface{}, error) {
	if len(columnKeys) > 1 {
		var buf bytes.Buffer
		enc := gob.NewEncoder(&buf)
		for _, columnKey := range columnKeys {
			val, err := valueFromColumnKey(info, columnKey)
			if err != nil {
				return "", err
			}
			// if object is nil dont try to encode it
			value := reflect.ValueOf(val)
			if value.Kind() == reflect.Invalid {
				continue
			}
			// if object is a nil pointer dont try to encode it
			if value.Kind() == reflect.Pointer && value.IsNil() {
				continue
			}
			err = enc.Encode(val)
			if err != nil {
				return "", err
			}
		}
		h := sha256.New()
		val := hex.EncodeToString(h.Sum(buf.Bytes()))
		return val, nil
	}
	val, err := valueFromColumnKey(info, columnKeys[0])
	if err != nil {
		return "", err
	}
	return val, err
}

func valueFromColumnKey(info *mapper.Info, columnKey model.ColumnKey) (interface{}, error) {
	val, err := info.FieldByColumn(columnKey.Column)
	if err != nil {
		return nil, err
	}
	if columnKey.Key != nil {
		val, err = valueFromMap(val, columnKey.Key)
		if err != nil {
			return "", fmt.Errorf("can't get key value from map: %v", err)
		}
	}
	return val, err
}

func valueFromMap(aMap interface{}, key interface{}) (interface{}, error) {
	m := reflect.ValueOf(aMap)
	if m.Kind() != reflect.Map {
		return nil, fmt.Errorf("expected map but got %s", m.Kind())
	}
	v := m.MapIndex(reflect.ValueOf(key))
	if !v.IsValid() {
		// return the zero value for the map value type
		return reflect.Indirect(reflect.New(m.Type().Elem())).Interface(), nil
	}

	return v.Interface(), nil
}

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
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

const (
	updateEvent     = "update"
	addEvent        = "add"
	deleteEvent     = "delete"
	bufferSize      = 65536
	columnDelimiter = ","
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
	Existing string
}

func (e *ErrIndexExists) Error() string {
	return fmt.Sprintf("cannot insert %s in the %s table. item %s has identical indexes. index: %s, value: %v", e.New, e.Table, e.Existing, e.Index, e.Value)
}

func NewIndexExistsError(table string, value interface{}, index string, new, existing string) *ErrIndexExists {
	return &ErrIndexExists{
		table, value, index, new, existing,
	}
}

// map of unique values to uuids
type valueToUUID map[interface{}]string

// map of column name(s) to a unique values, to UUIDs
type columnToValue map[index]valueToUUID

// index is the type used to implement multiple cache indexes
type index string

// columns returns the columns that conform the index
func (i index) columns() []string {
	return strings.Split(string(i), columnDelimiter)
}

// newIndex builds a index from a list of columns
func newIndex(columns ...string) index {
	return index(strings.Join(columns, columnDelimiter))
}

// RowCache is a collections of Models hashed by UUID
type RowCache struct {
	name     string
	dbModel  model.DatabaseModel
	dataType reflect.Type
	cache    map[string]model.Model
	indexes  columnToValue
	mutex    sync.RWMutex
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

// RowByModel searches the cache using a the indexes for a provided model
func (r *RowCache) RowByModel(m model.Model) model.Model {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	if reflect.TypeOf(m) != r.dataType {
		return nil
	}
	info, _ := r.dbModel.NewModelInfo(m)
	uuid, err := info.FieldByColumn("_uuid")
	if err != nil {
		return nil
	}
	if uuid.(string) != "" {
		return r.rowByUUID(uuid.(string))
	}
	for index, vals := range r.indexes {
		val, err := valueFromIndex(info, index)
		if err != nil {
			continue
		}
		if uuid, ok := vals[val]; ok {
			return r.rowByUUID(uuid)
		}
	}
	return nil
}

// Create writes the provided content to the cache
func (r *RowCache) CreateTime(uuid string, m model.Model, checkIndexes bool, opt *OpTime) error {
	start := time.Now()
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
	opt.setupTime = time.Since(start)
	start = time.Now()
	newIndexes := newColumnToValue(r.dbModel.Schema.Table(r.name).Indexes)
	for index, vals := range r.indexes {
		val, err := valueFromIndex(info, index)
		if err != nil {
			return err
		}

		if existing, ok := vals[val]; ok && checkIndexes {
			return NewIndexExistsError(r.name, val, string(index), uuid, existing)
		}

		newIndexes[index][val] = uuid
	}
	opt.genIdxTime = time.Since(start)
	opt.numIdx = len(r.indexes)

	// write indexes
	start = time.Now()
	for k1, v1 := range newIndexes {
		vals := r.indexes[k1]
		for k2, v2 := range v1 {
			vals[k2] = v2
		}
	}
	opt.writeIdxTime = time.Since(start)
	start = time.Now()
	r.cache[uuid] = model.Clone(m)
	opt.cloneTime = time.Since(start)
	return nil
}

func (r *RowCache) Create(uuid string, m model.Model, checkIndexes bool) error {
	a := &OpTime{}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.CreateTime(uuid, m, checkIndexes, a)
}

// Update updates the content in the cache; caller must check for inconsistency
func (r *RowCache) UpdateTime(uuid string, m model.Model, checkIndexes bool, opt *OpTime) error {
	start := time.Now()
	oldRow := model.Clone(r.cache[uuid])
	oldInfo, err := r.dbModel.NewModelInfo(oldRow)
	if err != nil {
		return err
	}
	newInfo, err := r.dbModel.NewModelInfo(m)
	if err != nil {
		return err
	}
	opt.setupTime = time.Since(start)
	start = time.Now()
	indexes := r.dbModel.Schema.Table(r.name).Indexes
	newIndexes := newColumnToValue(indexes)
	oldIndexes := newColumnToValue(indexes)
	var errs []error
	opt.numIdx = len(r.indexes)
	for index, vals := range r.indexes {
		var err error
		oldVal, err := valueFromIndex(oldInfo, index)
		if err != nil {
			return err
		}
		newVal, err := valueFromIndex(newInfo, index)
		if err != nil {
			return err
		}

		// if old and new values are the same, don't worry
		if oldVal == newVal {
			continue
		}
		// old and new values are NOT the same

		// check that there are no conflicts
		if conflict, ok := vals[newVal]; ok && checkIndexes && conflict != uuid {
			errs = append(errs, NewIndexExistsError(
				r.name,
				newVal,
				string(index),
				uuid,
				conflict,
			))
		}

		newIndexes[index][newVal] = uuid
		oldIndexes[index][oldVal] = ""
	}
	opt.genIdxTime = time.Since(start)
	if len(errs) > 0 {
		return fmt.Errorf("%+v", errs)
	}
	start = time.Now()
	// write indexes
	for k1, v1 := range newIndexes {
		vals := r.indexes[k1]
		for k2, v2 := range v1 {
			vals[k2] = v2
		}
	}
	// delete old indexes
	for k1, v1 := range oldIndexes {
		vals := r.indexes[k1]
		for k2 := range v1 {
			delete(vals, k2)
		}
	}
	opt.writeIdxTime = time.Since(start)
	start = time.Now()
	r.cache[uuid] = model.Clone(m)
	opt.cloneTime = time.Since(start)
	return nil
}

func (r *RowCache) Update(uuid string, m model.Model, checkIndexes bool) error {
	a := &OpTime{}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if _, ok := r.cache[uuid]; !ok {
		return NewErrCacheInconsistent(fmt.Sprintf("cannot update row %s as it does not exist in the cache", uuid))
	}
	return r.UpdateTime(uuid, m, checkIndexes, a)
}

func (r *RowCache) IndexExists(row model.Model) error {
	info, err := r.dbModel.NewModelInfo(row)
	if err != nil {
		return err
	}
	uuid, err := info.FieldByColumn("_uuid")
	if err != nil {
		return nil
	}
	for index, vals := range r.indexes {
		val, err := valueFromIndex(info, index)
		if err != nil {
			continue
		}
		if existing, ok := vals[val]; ok && existing != uuid.(string) {
			return NewIndexExistsError(
				r.name,
				val,
				string(index),
				uuid.(string),
				existing,
			)
		}
	}
	return nil
}

func (r *RowCache) DeleteUnlocked(uuid string, opt *OpTime) error {
	start := time.Now()
	if _, ok := r.cache[uuid]; !ok {
		return NewErrCacheInconsistent(fmt.Sprintf("cannot delete row %s as it does not exist in the cache", uuid))
	}
	oldRow := r.cache[uuid]
	oldInfo, err := r.dbModel.NewModelInfo(oldRow)
	if err != nil {
		return err
	}
	opt.setupTime = time.Since(start)
	start = time.Now()
	opt.numIdx = len(r.indexes)
	for index, vals := range r.indexes {
		oldVal, err := valueFromIndex(oldInfo, index)
		if err != nil {
			return err
		}
		// only remove the index if it is pointing to this uuid
		// otherwise we can cause a consistency issue if we've processed
		// updates out of order
		if vals[oldVal] == uuid {
			delete(vals, oldVal)
		}
	}
	opt.genIdxTime = time.Since(start)
	start = time.Now()
	delete(r.cache, uuid)
	opt.cloneTime = time.Since(start)
	return nil
}

// Delete deletes a row from the cache
func (r *RowCache) Delete(uuid string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	a := &OpTime{}
	return r.DeleteUnlocked(uuid, a)
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

func (r *RowCache) RowsByCondition(conditions []ovsdb.Condition) (map[string]model.Model, error) {
	results := make(map[string]model.Model)
	schema := r.dbModel.Schema.Table(r.name)
	if len(conditions) == 0 {
		for uuid, row := range r.Rows() {
			results[uuid] = row
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
			for rowUUID, row := range r.Rows() {
				ok, err := condition.Function.Evaluate(rowUUID, uuid)
				if err != nil {
					return nil, err
				}
				if ok {
					results[rowUUID] = row
				}
			}
		} else if index, err := r.Index(condition.Column); err != nil {
			for k, rowUUID := range index {
				tSchema := schema.Columns[condition.Column]
				nativeValue, err := ovsdb.OvsToNative(tSchema, condition.Value)
				if err != nil {
					return nil, err
				}
				ok, err := condition.Function.Evaluate(k, nativeValue)
				if err != nil {
					return nil, err
				}
				if ok {
					row := r.Row(rowUUID)
					results[rowUUID] = row
				}
			}
		} else {
			for uuid, row := range r.Rows() {
				info, err := r.dbModel.NewModelInfo(row)
				if err != nil {
					return nil, err
				}
				value, err := info.FieldByColumn(condition.Column)
				if err != nil {
					return nil, err
				}
				ok, err := condition.Function.Evaluate(value, condition.Value)
				if err != nil {
					return nil, err
				}
				if ok {
					results[uuid] = row
				}
			}
		}
	}
	return results, nil
}

// Len returns the length of the cache
func (r *RowCache) Len() int {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	return len(r.cache)
}

func (r *RowCache) Index(columns ...string) (map[interface{}]string, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	index, ok := r.indexes[newIndex(columns...)]
	if !ok {
		return nil, fmt.Errorf("%s is not an index", index)
	}
	return index, nil
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
	name           string
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
func NewTableCache(dbName string, dbModel model.DatabaseModel, data Data, logger *logr.Logger) (*TableCache, error) {
	if !dbModel.Valid() {
		return nil, fmt.Errorf("tablecache without valid databasemodel cannot be populated")
	}
	if logger == nil {
		l := stdr.NewWithOptions(log.New(os.Stderr, "", log.LstdFlags), stdr.Options{LogCaller: stdr.All}).WithName("cache").WithValues(
			"database", dbName,
		)
		logger = &l
	} else {
		l := logger.WithName("cache").WithValues(
			"database", dbName,
		)
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
func (t *TableCache) Update2Time(context interface{}, tableUpdates ovsdb.TableUpdates2) (error, time.Duration, time.Duration, []*OpTime) {
	if len(tableUpdates) == 0 {
		return nil, 0, 0, nil
	}
	err, lockTime, popTime, opTimes := t.Populate2Time(tableUpdates)
	if err != nil {
		t.logger.Error(err, "during libovsdb cache populate2")
		return err, 0, 0, nil
	}
	return nil, lockTime, popTime, opTimes
}

func (t *TableCache) Update2(context interface{}, tableUpdates ovsdb.TableUpdates2) error {
	err, _, _, _ := t.Update2Time(context, tableUpdates)
	return err
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
		updates, ok := tableUpdates[table]
		if !ok {
			continue
		}
		tCache := t.cache[table]
		for uuid, row := range updates {
			logger := t.logger.WithValues("uuid", uuid, "table", table)
			logger.V(5).Info("processing update")
			if row.New != nil {
				newModel, err := t.CreateModel(table, row.New, uuid)
				if err != nil {
					return err
				}
				if existing := tCache.Row(uuid); existing != nil {
					if !model.Equal(newModel, existing) {
						logger.V(5).Info("updating row", "old:", fmt.Sprintf("%+v", existing), "new", fmt.Sprintf("%+v", newModel))
						if err := tCache.Update(uuid, newModel, false); err != nil {
							return err
						}
						t.eventProcessor.AddEvent(updateEvent, table, existing, newModel)
					}
					// no diff
					continue
				}
				logger.V(5).Info("creating row", "model", fmt.Sprintf("%+v", newModel))
				if err := tCache.Create(uuid, newModel, false); err != nil {
					return err
				}
				t.eventProcessor.AddEvent(addEvent, table, nil, newModel)
				continue
			} else {
				oldModel, err := t.CreateModel(table, row.Old, uuid)
				if err != nil {
					return err
				}
				logger.V(5).Info("deleting row", "model", fmt.Sprintf("%+v", oldModel))
				if err := tCache.Delete(uuid); err != nil {
					return err
				}
				t.eventProcessor.AddEvent(deleteEvent, table, oldModel, nil)
				continue
			}
		}
	}
	return nil
}

type OpTime struct {
	optype       string
	overall      time.Duration
	lockTime     time.Duration
	modelTime    time.Duration
	applyTime    time.Duration
	eqTime       time.Duration
	setupTime    time.Duration
	genIdxTime   time.Duration
	numIdx       int
	writeIdxTime time.Duration
	cloneTime    time.Duration
	evtTime      time.Duration
}

func (o *OpTime) String() string {
	return fmt.Sprintf("[{%s %v} lk: %v, m: %v, ay: %v, eq: %v, st: %v, gi(%d): %v, wi: %v, cln: %v, evt: %v]",
		o.optype, o.overall, o.lockTime, o.modelTime, o.applyTime, o.eqTime, o.setupTime, o.numIdx, o.genIdxTime, o.writeIdxTime, o.cloneTime, o.evtTime)
}

func (t *TableCache) updateRow(table, uuid string, row *ovsdb.RowUpdate2, rCache *RowCache) (error, *OpTime) {
	logger := t.logger.WithValues("uuid", uuid, "table", table)
	logger.V(5).Info("processing update")

	start := time.Now()
	rCache.mutex.Lock()
	defer rCache.mutex.Unlock()
	opt := &OpTime{}
	opt.lockTime = time.Since(start)

	switch {
	case row.Initial != nil:
		opt.optype = "INI"
		start = time.Now()
		m, err := t.CreateModel(table, row.Initial, uuid)
		opt.modelTime = time.Since(start)
		if err != nil {
			return err, nil
		}
		logger.V(5).Info("creating row", "model", fmt.Sprintf("%+v", m))
		if err := rCache.CreateTime(uuid, m, false, opt); err != nil {
			return err, nil
		}
		start = time.Now()
		t.eventProcessor.AddEvent(addEvent, table, nil, m)
		opt.evtTime = time.Since(start)
	case row.Insert != nil:
		opt.optype = "INS"
		start = time.Now()
		m, err := t.CreateModel(table, row.Insert, uuid)
		opt.modelTime = time.Since(start)
		if err != nil {
			return err, nil
		}
		logger.V(5).Info("inserting row", "model", fmt.Sprintf("%+v", m))
		if err := rCache.CreateTime(uuid, m, false, opt); err != nil {
			return err, nil
		}
		start = time.Now()
		t.eventProcessor.AddEvent(addEvent, table, nil, m)
		opt.evtTime = time.Since(start)
	case row.Modify != nil:
		opt.optype = "MOD"
		start = time.Now()
		existing := rCache.rowByUUID(uuid)
		if existing == nil {
			return NewErrCacheInconsistent(fmt.Sprintf("row with uuid %s does not exist", uuid)), nil
		}
		modified := model.Clone(existing)
		opt.modelTime = time.Since(start)
		start = time.Now()
		err := t.ApplyModifications(table, modified, *row.Modify)
		opt.applyTime = time.Since(start)
		if err != nil {
			return fmt.Errorf("unable to apply row modifications: %w", err), nil
		}
		start = time.Now()
		equal := model.Equal(modified, existing)
		opt.eqTime = time.Since(start)
		if !equal {
			logger.V(5).Info("updating row", "old", fmt.Sprintf("%+v", existing), "new", fmt.Sprintf("%+v", modified))
			if err := rCache.UpdateTime(uuid, modified, false, opt); err != nil {
				return err, nil
			}
			start = time.Now()
			t.eventProcessor.AddEvent(updateEvent, table, existing, modified)
			opt.evtTime = time.Since(start)
		}
	case row.Delete != nil:
		fallthrough
	default:
		opt.optype = "DEL"
		// If everything else is nil (including Delete because it's a key with
		// no value on the wire), then process a delete
		start = time.Now()
		m := rCache.rowByUUID(uuid)
		opt.modelTime = time.Since(start)
		if m == nil {
			return NewErrCacheInconsistent(fmt.Sprintf("row with uuid %s does not exist", uuid)), nil
		}
		logger.V(5).Info("deleting row", "model", fmt.Sprintf("%+v", m))
		if err := rCache.DeleteUnlocked(uuid, opt); err != nil {
			return err, nil
		}
		start = time.Now()
		t.eventProcessor.AddEvent(deleteEvent, table, m, nil)
		opt.evtTime = time.Since(start)
	}
	return nil, opt
}

// Populate2 adds data to the cache and places an event on the channel
func (t *TableCache) Populate2Time(tableUpdates ovsdb.TableUpdates2) (error, time.Duration, time.Duration, []*OpTime) {
	start := time.Now()
	t.mutex.Lock()
	lockTime := time.Since(start)
	defer t.mutex.Unlock()
	start = time.Now()
	opTimes := make([]*OpTime, 0, 10)
	for table := range t.dbModel.Types() {
		updates, ok := tableUpdates[table]
		if !ok {
			continue
		}
		rCache := t.cache[table]
		for uuid, row := range updates {
			foo := time.Now()
			err, opt := t.updateRow(table, uuid, row, rCache)
			if err != nil {
				return err, 0, 0, nil
			}
			opt.overall = time.Since(foo)
			opTimes = append(opTimes, opt)
		}
	}
	return nil, lockTime, time.Since(start), opTimes
}

func (t *TableCache) Populate2(tableUpdates ovsdb.TableUpdates2) error {
	err, _, _, _ := t.Populate2Time(tableUpdates)
	return err
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
	r := &RowCache{
		name:     name,
		dbModel:  dbModel,
		indexes:  newColumnToValue(dbModel.Schema.Table(name).Indexes),
		dataType: dataType,
		cache:    make(map[string]model.Model),
		mutex:    sync.RWMutex{},
	}
	return r
}

func newColumnToValue(schemaIndexes [][]string) columnToValue {
	// RFC 7047 says that Indexes is a [<column-set>] and "Each <column-set> is a set of
	// columns whose values, taken together within any given row, must be
	// unique within the table". We'll store the column names, separated by comma
	// as we'll assume (RFC is not clear), that comma isn't valid in a <id>
	var indexes []index
	for i := range schemaIndexes {
		indexes = append(indexes, newIndex(schemaIndexes[i]...))
	}
	c := make(columnToValue)
	for _, index := range indexes {
		c[index] = make(valueToUUID)
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
	events chan event
	// handlersMutex locks the handlers array when we add a handler or dispatch events
	// we don't need a RWMutex in this case as we only have one thread reading and the write
	// volume is very low (i.e only when AddEventHandler is called)
	handlersMutex sync.Mutex
	handlers      []EventHandler
	logger        *logr.Logger
}

func newEventProcessor(capacity int, logger *logr.Logger) *eventProcessor {
	return &eventProcessor{
		events:   make(chan event, capacity),
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
	case e.events <- event:
		// noop
		return
	default:
		e.logger.V(0).Info("dropping event because event buffer is full", "event", fmt.Sprintf("%+v", event))
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

// CreateModel creates a new Model instance based on the Row information
func (t *TableCache) CreateModel(tableName string, row *ovsdb.Row, uuid string) (model.Model, error) {
	if !t.dbModel.Valid() {
		return nil, fmt.Errorf("database model not valid")
	}

	table := t.dbModel.Schema.Table(tableName)
	if table == nil {
		return nil, fmt.Errorf("table %s not found", tableName)
	}
	model, err := t.dbModel.NewModel(tableName)
	if err != nil {
		return nil, err
	}
	info, err := t.dbModel.NewModelInfo(model)
	if err != nil {
		return nil, err
	}
	err = t.dbModel.Mapper.GetRowData(row, info)
	if err != nil {
		return nil, err
	}

	if uuid != "" {
		if err := info.SetField("_uuid", uuid); err != nil {
			return nil, err
		}
	}

	return model, nil
}

// ApplyModifications applies the contents of a RowUpdate2.Modify to a model
// nolint: gocyclo
func (t *TableCache) ApplyModifications(tableName string, base model.Model, update ovsdb.Row) error {
	if !t.dbModel.Valid() {
		return fmt.Errorf("database model not valid")
	}
	table := t.dbModel.Schema.Table(tableName)
	if table == nil {
		return fmt.Errorf("table %s not found", tableName)
	}
	schema := t.dbModel.Schema.Table(tableName)
	if schema == nil {
		return fmt.Errorf("no schema for table %s", tableName)
	}
	info, err := t.dbModel.NewModelInfo(base)
	if err != nil {
		return err
	}
	for k, v := range update {
		if k == "_uuid" {
			continue
		}

		current, err := info.FieldByColumn(k)
		if err != nil {
			return err
		}

		var value interface{}
		value, err = ovsdb.OvsToNative(schema.Column(k), v)
		// we can overflow the max of a set with min: 0, max: 1 here because of the update2/update3 notation
		// which to replace "foo" with "bar" would send a set with ["foo", "bar"]
		if err != nil && schema.Column(k).Type == ovsdb.TypeSet && schema.Column(k).TypeObj.Max() == 1 {
			value, err = ovsdb.OvsToNativeSlice(schema.Column(k).TypeObj.Key.Type, v)
		}
		if err != nil {
			return err
		}
		nv := reflect.ValueOf(value)

		switch reflect.ValueOf(current).Kind() {
		case reflect.Slice, reflect.Array:
			// The difference between two sets are all elements that only belong to one of the sets.
			// Iterate new values
			for i := 0; i < nv.Len(); i++ {
				// search for match in base values
				baseValue, err := info.FieldByColumn(k)
				if err != nil {
					return err
				}
				bv := reflect.ValueOf(baseValue)
				var found bool
				for j := 0; j < bv.Len(); j++ {
					if bv.Index(j).Interface() == nv.Index(i).Interface() {
						// found a match, delete from slice
						found = true
						newValue := reflect.AppendSlice(bv.Slice(0, j), bv.Slice(j+1, bv.Len()))
						err = info.SetField(k, newValue.Interface())
						if err != nil {
							return err
						}
						break
					}
				}
				if !found {
					newValue := reflect.Append(bv, nv.Index(i))
					err = info.SetField(k, newValue.Interface())
					if err != nil {
						return err
					}
				}
			}
		case reflect.Ptr:
			// if NativeToOVS was successful, then simply assign
			if nv.Type() == reflect.ValueOf(current).Type() {
				err = info.SetField(k, nv.Interface())
				return err
			}
			// With a pointer type, an update value could be a set with 2 elements [old, new]
			if nv.Len() != 2 {
				return fmt.Errorf("expected a slice with 2 elements for update: %+v", update)
			}
			// the new value is the value in the slice which isn't equal to the existing string
			for i := 0; i < nv.Len(); i++ {
				baseValue, err := info.FieldByColumn(k)
				if err != nil {
					return err
				}
				bv := reflect.ValueOf(baseValue)
				if nv.Index(i) != bv {
					err = info.SetField(k, nv.Index(i).Addr().Interface())
					if err != nil {
						return err
					}
				}
			}
		case reflect.Map:
			// The difference between two maps are all key-value pairs whose keys appears in only one of the maps,
			// plus the key-value pairs whose keys appear in both maps but with different values.
			// For the latter elements, <row> includes the value from the new column.
			iter := nv.MapRange()

			baseValue, err := info.FieldByColumn(k)
			if err != nil {
				return err
			}

			bv := reflect.ValueOf(baseValue)
			if bv.IsNil() {
				bv = reflect.MakeMap(nv.Type())
			}

			for iter.Next() {
				mk := iter.Key()
				mv := iter.Value()

				existingValue := bv.MapIndex(mk)

				// key does not exist, add it
				if !existingValue.IsValid() {
					bv.SetMapIndex(mk, mv)
				} else if reflect.DeepEqual(mv.Interface(), existingValue.Interface()) {
					// delete it
					bv.SetMapIndex(mk, reflect.Value{})
				} else {
					// set new value
					bv.SetMapIndex(mk, mv)
				}
			}
			if len(bv.MapKeys()) == 0 {
				bv = reflect.Zero(nv.Type())
			}
			err = info.SetField(k, bv.Interface())
			if err != nil {
				return err
			}

		default:
			// For columns with single value, the difference is the value of the new column.
			err = info.SetField(k, value)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func valueFromIndex(info *mapper.Info, index index) (interface{}, error) {
	columns := index.columns()
	if len(columns) > 1 {
		var buf bytes.Buffer
		enc := gob.NewEncoder(&buf)
		for _, column := range columns {
			val, err := info.FieldByColumn(column)
			if err != nil {
				return "", err
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
	val, err := info.FieldByColumn(columns[0])
	if err != nil {
		return nil, err
	}
	return val, err
}

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
	schema   ovsdb.TableSchema
	dataType reflect.Type
	cache    map[string]model.Model
	indexes  columnToValue
	mutex    sync.RWMutex
}

// Row returns one model from the cache by UUID
func (r *RowCache) Row(uuid string) model.Model {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	if row, ok := r.cache[uuid]; ok {
		return model.Clone(row)
	}
	return nil
}

// RowByModel searches the cache using a the indexes for a provided model
func (r *RowCache) RowByModel(m model.Model) model.Model {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	if reflect.TypeOf(m) != r.dataType {
		return nil
	}
	info, _ := mapper.NewInfo(&r.schema, m)
	uuid, err := info.FieldByColumn("_uuid")
	if err != nil {
		return nil
	}
	if uuid.(string) != "" {
		return r.Row(uuid.(string))
	}
	for index := range r.indexes {
		val, err := valueFromIndex(info, index)
		if err != nil {
			continue
		}
		if uuid, ok := r.indexes[index][val]; ok {
			return r.Row(uuid)
		}
	}
	return nil
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
	info, err := mapper.NewInfo(&r.schema, m)
	if err != nil {
		return err
	}
	newIndexes := newColumnToValue(r.schema.Indexes)
	for index := range r.indexes {
		val, err := valueFromIndex(info, index)
		if err != nil {
			return err
		}

		if existing, ok := r.indexes[index][val]; ok && checkIndexes {
			return NewIndexExistsError(r.name, val, string(index), uuid, existing)
		}

		newIndexes[index][val] = uuid
	}

	// write indexes
	for k1, v1 := range newIndexes {
		for k2, v2 := range v1 {
			r.indexes[k1][k2] = v2
		}
	}
	r.cache[uuid] = model.Clone(m)
	return nil
}

// Update updates the content in the cache
func (r *RowCache) Update(uuid string, m model.Model, checkIndexes bool) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if _, ok := r.cache[uuid]; !ok {
		return NewErrCacheInconsistent(fmt.Sprintf("cannot update row %s as it does not exist in the cache", uuid))
	}
	oldRow := model.Clone(r.cache[uuid])
	oldInfo, err := mapper.NewInfo(&r.schema, oldRow)
	if err != nil {
		return err
	}
	newInfo, err := mapper.NewInfo(&r.schema, m)
	if err != nil {
		return err
	}
	newIndexes := newColumnToValue(r.schema.Indexes)
	oldIndexes := newColumnToValue(r.schema.Indexes)
	var errs []error
	for index := range r.indexes {
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
		if conflict, ok := r.indexes[index][newVal]; ok && checkIndexes && conflict != uuid {
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
	if len(errs) > 0 {
		return fmt.Errorf("%+v", errs)
	}
	// write indexes
	for k1, v1 := range newIndexes {
		for k2, v2 := range v1 {
			r.indexes[k1][k2] = v2
		}
	}
	// delete old indexes
	for k1, v1 := range oldIndexes {
		for k2 := range v1 {
			delete(r.indexes[k1], k2)
		}
	}
	r.cache[uuid] = model.Clone(m)
	return nil
}

func (r *RowCache) IndexExists(row model.Model) error {
	info, err := mapper.NewInfo(&r.schema, row)
	if err != nil {
		return err
	}
	uuid, err := info.FieldByColumn("_uuid")
	if err != nil {
		return nil
	}
	for index := range r.indexes {
		val, err := valueFromIndex(info, index)
		if err != nil {
			continue
		}
		if existing, ok := r.indexes[index][val]; ok && existing != uuid.(string) {
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

// Delete deletes a row from the cache
func (r *RowCache) Delete(uuid string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if _, ok := r.cache[uuid]; !ok {
		return NewErrCacheInconsistent(fmt.Sprintf("cannot delete row %s as it does not exist in the cache", uuid))
	}
	oldRow := r.cache[uuid]
	oldInfo, err := mapper.NewInfo(&r.schema, oldRow)
	if err != nil {
		return err
	}
	for index := range r.indexes {
		oldVal, err := valueFromIndex(oldInfo, index)
		if err != nil {
			return err
		}
		delete(r.indexes[index], oldVal)
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

func (r *RowCache) RowsByCondition(conditions []ovsdb.Condition) ([]model.Model, error) {
	var results []model.Model
	if len(conditions) == 0 {
		for _, row := range r.Rows() {
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
			for rowUUID, row := range r.Rows() {
				ok, err := condition.Function.Evaluate(rowUUID, uuid)
				if err != nil {
					return nil, err
				}
				if ok {
					results = append(results, row)
				}
			}
		} else if index, err := r.Index(condition.Column); err != nil {
			for k, v := range index {
				tSchema := r.schema.Columns[condition.Column]
				nativeValue, err := ovsdb.OvsToNative(tSchema, condition.Value)
				if err != nil {
					return nil, err
				}
				ok, err := condition.Function.Evaluate(k, nativeValue)
				if err != nil {
					return nil, err
				}
				if ok {
					row := r.Row(v)
					results = append(results, row)
				}
			}
		} else {
			for _, row := range r.Rows() {
				info, err := mapper.NewInfo(&r.schema, row)
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
					results = append(results, row)
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
	cache          map[string]*RowCache
	eventProcessor *eventProcessor
	mapper         *mapper.Mapper
	dbModel        *model.DBModel
	schema         *ovsdb.DatabaseSchema
	updates        chan ovsdb.TableUpdates
	updates2       chan ovsdb.TableUpdates2
	errorChan      chan error
	ovsdb.NotificationHandler
	mutex  sync.RWMutex
	logger *logr.Logger
}

// Data is the type for data that can be prepopulated in the cache
type Data map[string]map[string]model.Model

// NewTableCache creates a new TableCache
func NewTableCache(schema *ovsdb.DatabaseSchema, dbModel *model.DBModel, data Data, logger *logr.Logger) (*TableCache, error) {
	if schema == nil || dbModel == nil {
		return nil, fmt.Errorf("tablecache without databasemodel cannot be populated")
	}
	if logger == nil {
		l := stdr.NewWithOptions(log.New(os.Stderr, "", log.LstdFlags), stdr.Options{LogCaller: stdr.All}).WithName("libovsdb/cache")
		logger = &l
	} else {
		l := logger.WithName("cache")
		logger = &l
	}
	eventProcessor := newEventProcessor(bufferSize, logger)
	cache := make(map[string]*RowCache)
	tableTypes := dbModel.Types()
	for name, tableSchema := range schema.Tables {
		cache[name] = newRowCache(name, tableSchema, tableTypes[name])
	}
	for table, rowData := range data {
		if _, ok := schema.Tables[table]; !ok {
			return nil, fmt.Errorf("table %s is not in schema", table)
		}
		for uuid, row := range rowData {
			if err := cache[table].Create(uuid, row, true); err != nil {
				return nil, err
			}
		}
	}
	return &TableCache{
		cache:          cache,
		schema:         schema,
		eventProcessor: eventProcessor,
		mapper:         mapper.NewMapper(schema),
		dbModel:        dbModel,
		mutex:          sync.RWMutex{},
		updates:        make(chan ovsdb.TableUpdates, bufferSize),
		updates2:       make(chan ovsdb.TableUpdates2, bufferSize),
		errorChan:      make(chan error),
		logger:         logger,
	}, nil
}

// Mapper returns the mapper
func (t *TableCache) Mapper() *mapper.Mapper {
	return t.mapper
}

// DBModel returns the DBModel
func (t *TableCache) DBModel() *model.DBModel {
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
func (t *TableCache) Update(context interface{}, tableUpdates ovsdb.TableUpdates) {
	if len(tableUpdates) == 0 {
		return
	}
	t.updates <- tableUpdates
}

// Update2 implements the update method of the NotificationHandler interface
// this populates a channel with updates so they can be processed after the initial
// state has been Populated
func (t *TableCache) Update2(context interface{}, tableUpdates ovsdb.TableUpdates2) {
	if len(tableUpdates) == 0 {
		return
	}
	t.updates2 <- tableUpdates
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
			t.logger.V(5).Info("processing update for row", "uuid", uuid, "table", table)
			if row.New != nil {
				newModel, err := t.CreateModel(table, row.New, uuid)
				if err != nil {
					return err
				}
				if existing := tCache.Row(uuid); existing != nil {
					if !reflect.DeepEqual(newModel, existing) {
						t.logger.V(5).Info("updating row", "uuid", uuid, "old:", fmt.Sprintf("%+v", existing), "new", fmt.Sprintf("%+v", newModel))
						if err := tCache.Update(uuid, newModel, false); err != nil {
							return err
						}
						t.eventProcessor.AddEvent(updateEvent, table, existing, newModel)
					}
					// no diff
					continue
				}
				t.logger.V(5).Info("creating row", "uuid", uuid, "model", fmt.Sprintf("%+v", newModel))
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
				t.logger.V(5).Info("deleting row", "uuid", uuid, "model", fmt.Sprintf("%+v", oldModel))
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

// Populate2 adds data to the cache and places an event on the channel
func (t *TableCache) Populate2(tableUpdates ovsdb.TableUpdates2) error {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	for table := range t.dbModel.Types() {
		updates, ok := tableUpdates[table]
		if !ok {
			continue
		}
		tCache := t.cache[table]
		for uuid, row := range updates {
			t.logger.V(5).Info("processing update for row", "uuid", uuid, "table", table)
			switch {
			case row.Initial != nil:
				m, err := t.CreateModel(table, row.Initial, uuid)
				if err != nil {
					return err
				}
				t.logger.V(5).Info("creating row", "uuid", uuid, "model", fmt.Sprintf("%+v", m))
				if err := tCache.Create(uuid, m, false); err != nil {
					return err
				}
				t.eventProcessor.AddEvent(addEvent, table, nil, m)
			case row.Insert != nil:
				m, err := t.CreateModel(table, row.Insert, uuid)
				if err != nil {
					return err
				}
				t.logger.V(5).Info("creating row", "uuid", uuid, "model", fmt.Sprintf("%+v", m))
				if err := tCache.Create(uuid, m, false); err != nil {
					return err
				}
				t.eventProcessor.AddEvent(addEvent, table, nil, m)
			case row.Modify != nil:
				existing := tCache.Row(uuid)
				if existing == nil {
					panic(fmt.Errorf("row with uuid %s does not exist", uuid))
				}
				modified := tCache.Row(uuid)
				err := t.ApplyModifications(table, modified, *row.Modify)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(modified, existing) {
					t.logger.V(5).Info("updating row", "uuid", uuid, "old", fmt.Sprintf("%+v", existing), "new", fmt.Sprintf("%+v", modified))
					if err := tCache.Update(uuid, modified, false); err != nil {
						return err
					}
					t.eventProcessor.AddEvent(updateEvent, table, existing, modified)
				}
			case row.Delete != nil:
				fallthrough
			default:
				// If everything else is nil (including Delete because it's a key with
				// no value on the wire), then process a delete
				m := tCache.Row(uuid)
				if m == nil {
					panic(fmt.Errorf("row with uuid %s does not exist", uuid))
				}
				t.logger.V(5).Info("deleting row", "uuid", uuid, "model", fmt.Sprintf("%+v", m))
				if err := tCache.Delete(uuid); err != nil {
					return err
				}
				t.eventProcessor.AddEvent(deleteEvent, table, m, nil)
			}
		}
	}
	return nil
}

// Purge drops all data in the cache and reinitializes it using the
// provided schema
func (t *TableCache) Purge(schema *ovsdb.DatabaseSchema) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	tableTypes := t.dbModel.Types()
	for name, tableSchema := range t.schema.Tables {
		t.cache[name] = newRowCache(name, tableSchema, tableTypes[name])
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
	go t.processUpdates(stopCh)
	wg.Add(1)
	go t.eventProcessor.Run(stopCh)
	wg.Wait()
	t.updates = make(chan ovsdb.TableUpdates, bufferSize)
	t.updates2 = make(chan ovsdb.TableUpdates2, bufferSize)
}

// Errors returns a channel where errors that occur during cache propagation can be received
func (t *TableCache) Errors() <-chan error {
	return t.errorChan
}

func (t *TableCache) processUpdates(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		case update := <-t.updates:
			if err := t.Populate(update); err != nil {
				select {
				case t.errorChan <- err:
					// error sent to client
				default:
					// client not listening for errors
				}
			}
		case update2 := <-t.updates2:
			if err := t.Populate2(update2); err != nil {
				select {
				case t.errorChan <- err:
					// error sent to client
				default:
					// client not listening for errors
				}
			}
		}
	}
}

// newRowCache creates a new row cache with the provided data
// if the data is nil, and empty RowCache will be created
func newRowCache(name string, schema ovsdb.TableSchema, dataType reflect.Type) *RowCache {
	r := &RowCache{
		name:     name,
		schema:   schema,
		indexes:  newColumnToValue(schema.Indexes),
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

// CreateModel creates a new Model instance based on the Row information
func (t *TableCache) CreateModel(tableName string, row *ovsdb.Row, uuid string) (model.Model, error) {
	table := t.mapper.Schema.Table(tableName)
	if table == nil {
		return nil, fmt.Errorf("table %s not found", tableName)
	}
	model, err := t.dbModel.NewModel(tableName)
	if err != nil {
		return nil, err
	}

	err = t.mapper.GetRowData(tableName, row, model)
	if err != nil {
		return nil, err
	}

	if uuid != "" {
		mapperInfo, err := mapper.NewInfo(table, model)
		if err != nil {
			return nil, err
		}
		if err := mapperInfo.SetField("_uuid", uuid); err != nil {
			return nil, err
		}
	}

	return model, nil
}

// ApplyModifications applies the contents of a RowUpdate2.Modify to a model
// nolint: gocyclo
func (t *TableCache) ApplyModifications(tableName string, base model.Model, update ovsdb.Row) error {
	table := t.mapper.Schema.Table(tableName)
	if table == nil {
		return fmt.Errorf("table %s not found", tableName)
	}
	schema := t.schema.Table(tableName)
	if schema == nil {
		return fmt.Errorf("no schema for table %s", tableName)
	}
	info, err := mapper.NewInfo(schema, base)
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
				panic("expected a slice with 2 elements")
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
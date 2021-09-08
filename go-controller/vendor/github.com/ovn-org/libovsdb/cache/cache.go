package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"log"

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

type IndexExistsError struct {
	table    string
	value    interface{}
	index    index
	new      string
	existing string
}

func (i *IndexExistsError) Error() string {
	return fmt.Sprintf("operation would cause rows in the \"%s\" table to have identical values (%v) for index on column \"%s\". First row, with UUID %s, was inserted by this transaction. Second row, with UUID %s, existed in the database before this operation and was not modified",
		i.table,
		i.value,
		i.index,
		i.new,
		i.existing,
	)
}

func NewIndexExistsError(table string, value interface{}, index index, new, existing string) *IndexExistsError {
	return &IndexExistsError{
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
	return r.row(uuid)
}

func (r *RowCache) row(uuid string) model.Model {
	if row, ok := r.cache[uuid]; ok {
		return row.(model.Model)
	}
	return nil
}

// Create writes the provided content to the cache
func (r *RowCache) Create(uuid string, m model.Model) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.create(uuid, m)
}

func (r *RowCache) create(uuid string, m model.Model) error {
	if _, ok := r.cache[uuid]; ok {
		return fmt.Errorf("row %s already exists", uuid)
	}
	if reflect.TypeOf(m) != r.dataType {
		return fmt.Errorf("expected data of type %s, but got %s", r.dataType.String(), reflect.TypeOf(m).String())
	}
	info, err := mapper.NewInfo(&r.schema, m)
	if err != nil {
		return err
	}
	newIndexes := newColumnToValue(r.schema.Indexes)
	var errs []error
	for index := range r.indexes {

		val, err := valueFromIndex(info, index)

		if err != nil {
			return err
		}
		if existing, ok := r.indexes[index][val]; ok {
			errs = append(errs,
				NewIndexExistsError(r.name, val, index, uuid, existing))
		}
		newIndexes[index][val] = uuid
	}
	if len(errs) != 0 {
		return fmt.Errorf("%v", errs)
	}
	// write indexes
	for k1, v1 := range newIndexes {
		for k2, v2 := range v1 {
			r.indexes[k1][k2] = v2
		}
	}
	r.cache[uuid] = m
	return nil
}

// Update updates the content in the cache
func (r *RowCache) Update(uuid string, m model.Model) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.update(uuid, m)
}

func (r *RowCache) update(uuid string, m model.Model) error {
	if _, ok := r.cache[uuid]; !ok {
		return fmt.Errorf("row %s does not exist", uuid)
	}
	oldRow := r.cache[uuid]
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
		if conflict, ok := r.indexes[index][newVal]; ok && conflict != uuid {
			errs = append(errs, NewIndexExistsError(
				r.name,
				newVal,
				index,
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
	r.cache[uuid] = m
	return nil
}

// Delete deletes a row from the cache
func (r *RowCache) Delete(uuid string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.delete(uuid)
}

func (r *RowCache) delete(uuid string) error {
	if _, ok := r.cache[uuid]; !ok {
		return fmt.Errorf("row %s does not exist", uuid)
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

// Rows returns a list of row UUIDs as strings
func (r *RowCache) Rows() []string {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	var result []string
	for k := range r.cache {
		result = append(result, k)
	}
	return result
}

// Len returns the length of the cache
func (r *RowCache) Len() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
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
// It implements the ovsdb.NotifcationHandler interface so it may
// handle update notifications
type TableCache struct {
	cache          map[string]*RowCache
	eventProcessor *eventProcessor
	mapper         *mapper.Mapper
	dbModel        *model.DBModel
	ovsdb.NotificationHandler
}

// Data is the type for data that can be prepoulated in the cache
type Data map[string]map[string]model.Model

// NewTableCache creates a new TableCache
func NewTableCache(schema *ovsdb.DatabaseSchema, dbModel *model.DBModel, data Data) (*TableCache, error) {
	if schema == nil || dbModel == nil {
		return nil, fmt.Errorf("tablecache without databasemodel cannot be populated")
	}
	eventProcessor := newEventProcessor(bufferSize)
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
			if err := cache[table].Create(uuid, row); err != nil {
				return nil, err
			}
		}
	}
	return &TableCache{
		cache:          cache,
		eventProcessor: eventProcessor,
		mapper:         mapper.NewMapper(schema),
		dbModel:        dbModel,
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
	if table, ok := t.cache[name]; ok {
		return table
	}
	return nil
}

// Tables returns a list of table names that are in the cache
func (t *TableCache) Tables() []string {
	var result []string
	for k := range t.cache {
		result = append(result, k)
	}
	return result
}

// Update implements the update method of the NotificationHandler interface
// this populates the cache with new updates
func (t *TableCache) Update(context interface{}, tableUpdates ovsdb.TableUpdates) {
	if len(tableUpdates) == 0 {
		return
	}
	t.Populate(tableUpdates)
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

// lock acquires a lock on all tables in the cache
func (t *TableCache) lock() {
	for _, r := range t.cache {
		r.mutex.Lock()
	}
}

// unlock releases a lock on all tables in the cache
func (t *TableCache) unlock() {
	for _, r := range t.cache {
		r.mutex.Unlock()
	}
}

// Populate adds data to the cache and places an event on the channel
func (t *TableCache) Populate(tableUpdates ovsdb.TableUpdates) {
	t.lock()
	defer t.unlock()
	for table := range t.dbModel.Types() {
		updates, ok := tableUpdates[table]
		if !ok {
			continue
		}
		tCache := t.cache[table]
		for uuid, row := range updates {
			if row.New != nil {
				newModel, err := t.CreateModel(table, row.New, uuid)
				if err != nil {
					panic(err)
				}
				if existing := tCache.row(uuid); existing != nil {
					if !reflect.DeepEqual(newModel, existing) {
						if err := tCache.update(uuid, newModel); err != nil {
							panic(err)
						}
						t.eventProcessor.AddEvent(updateEvent, table, existing, newModel)
					}
					// no diff
					continue
				}
				if err := tCache.create(uuid, newModel); err != nil {
					panic(err)
				}
				t.eventProcessor.AddEvent(addEvent, table, nil, newModel)
				continue
			} else {
				oldModel, err := t.CreateModel(table, row.Old, uuid)
				if err != nil {
					panic(err)
				}
				if err := tCache.delete(uuid); err != nil {
					panic(err)
				}
				t.eventProcessor.AddEvent(deleteEvent, table, oldModel, nil)
				continue
			}
		}
	}
}

// AddEventHandler registers the supplied EventHandler to receive cache events
func (t *TableCache) AddEventHandler(handler EventHandler) {
	t.eventProcessor.AddEventHandler(handler)
}

// Run starts the event processing loop. It blocks until the channel is closed.
func (t *TableCache) Run(stopCh <-chan struct{}) {
	t.eventProcessor.Run(stopCh)
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
	// as we'll assuume (RFC is not clear), that comma isn't valid in a <id>
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
}

func newEventProcessor(capacity int) *eventProcessor {
	return &eventProcessor{
		events:   make(chan event, capacity),
		handlers: []EventHandler{},
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
		log.Print("dropping event because event buffer is full")
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

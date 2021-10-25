package model

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// A DatabaseModel represents libovsdb's metadata about the database.
// It's the result of combining the client's ClientDBModel and the server's Schema
type DatabaseModel struct {
	client   *ClientDBModel
	schema   *ovsdb.DatabaseSchema
	mapper   *mapper.Mapper
	mutex    sync.RWMutex
	metadata map[reflect.Type]*mapper.Metadata
}

// NewDatabaseModel returns a new DatabaseModel
func NewDatabaseModel(schema *ovsdb.DatabaseSchema, client *ClientDBModel) (*DatabaseModel, []error) {
	dbModel := NewPartialDatabaseModel(client)
	errs := dbModel.SetSchema(schema)
	if len(errs) > 0 {
		return nil, errs
	}
	return dbModel, nil
}

// NewPartialDatabaseModel returns a DatabaseModel what does not have a schema yet
func NewPartialDatabaseModel(client *ClientDBModel) *DatabaseModel {
	return &DatabaseModel{
		client: client,
	}
}

// Valid returns whether the DatabaseModel is fully functional
func (db *DatabaseModel) Valid() bool {
	db.mutex.RLock()
	defer db.mutex.RUnlock()
	return db.schema != nil
}

// SetSchema adds the Schema to the DatabaseModel making it valid if it was not before
func (db *DatabaseModel) SetSchema(schema *ovsdb.DatabaseSchema) []error {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	errors := db.client.validate(schema)
	if len(errors) > 0 {
		return errors
	}
	db.schema = schema
	db.mapper = mapper.NewMapper(schema)
	errs := db.generateModelInfo()
	if len(errs) > 0 {
		db.schema = nil
		db.mapper = nil
		return errs
	}
	return []error{}
}

// ClearSchema removes the Schema from the DatabaseModel making it not valid
func (db *DatabaseModel) ClearSchema() {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	db.schema = nil
	db.mapper = nil
}

// Client returns the DatabaseModel's client dbModel
func (db *DatabaseModel) Client() *ClientDBModel {
	return db.client
}

// Schema returns the DatabaseModel's schema
func (db *DatabaseModel) Schema() *ovsdb.DatabaseSchema {
	db.mutex.RLock()
	defer db.mutex.RUnlock()
	return db.schema
}

// Mapper returns the DatabaseModel's mapper
func (db *DatabaseModel) Mapper() *mapper.Mapper {
	db.mutex.RLock()
	defer db.mutex.RUnlock()
	return db.mapper
}

// NewModel returns a new instance of a model from a specific string
func (db *DatabaseModel) NewModel(table string) (Model, error) {
	mtype, ok := db.client.types[table]
	if !ok {
		return nil, fmt.Errorf("table %s not found in database model", string(table))
	}
	model := reflect.New(mtype.Elem())
	return model.Interface().(Model), nil
}

// Types returns the DatabaseModel Types
// the DatabaseModel types is a map of reflect.Types indexed by string
// The reflect.Type is a pointer to a struct that contains 'ovs' tags
// as described above. Such pointer to struct also implements the Model interface
func (db *DatabaseModel) Types() map[string]reflect.Type {
	return db.client.types
}

// FindTable returns the string associated with a reflect.Type or ""
func (db *DatabaseModel) FindTable(mType reflect.Type) string {
	for table, tType := range db.client.types {
		if tType == mType {
			return table
		}
	}
	return ""
}

// generateModelMetadata creates metadata objects from all models included in the
// database and caches them for future re-use
func (db *DatabaseModel) generateModelInfo() []error {
	errors := []error{}
	metadata := make(map[reflect.Type]*mapper.Metadata, len(db.client.types))
	for tableName, tType := range db.client.types {
		tableSchema := db.schema.Table(tableName)
		if tableSchema == nil {
			errors = append(errors, fmt.Errorf("Database Model contains model for table %s which is not present in schema", tableName))
			continue
		}
		obj, err := db.NewModel(tableName)
		if err != nil {
			errors = append(errors, err)
			continue
		}
		info, err := mapper.NewInfo(tableName, tableSchema, obj)
		if err != nil {
			errors = append(errors, err)
			continue
		}
		metadata[tType] = info.Metadata
	}
	db.metadata = metadata
	return errors
}

// NewModelInfo returns a mapper.Info object based on a provided model
func (db *DatabaseModel) NewModelInfo(obj interface{}) (*mapper.Info, error) {
	meta, ok := db.metadata[reflect.TypeOf(obj)]
	if !ok {
		return nil, ovsdb.NewErrWrongType("NewModelInfo", "type that is part of the DatabaseModel", obj)
	}
	return &mapper.Info{
		Obj:      obj,
		Metadata: meta,
	}, nil
}

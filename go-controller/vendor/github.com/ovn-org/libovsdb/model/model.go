package model

import (
	"fmt"
	"reflect"

	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// A Model is the base interface used to build Database Models. It is used
// to express how data from a specific Database Table shall be translated into structs
// A Model is a struct with at least one (most likely more) field tagged with the 'ovs' tag
// The value of 'ovs' field must be a valid column name in the OVS Database
// A field associated with the "_uuid" column mandatory. The rest of the columns are optional
// The struct may also have non-tagged fields (which will be ignored by the API calls)
// The Model interface must be implemented by the pointer to such type
// Example:
//type MyLogicalRouter struct {
//	UUID          string            `ovsdb:"_uuid"`
//	Name          string            `ovsdb:"name"`
//	ExternalIDs   map[string]string `ovsdb:"external_ids"`
//	LoadBalancers []string          `ovsdb:"load_balancer"`
//}
type Model interface{}

// DBModel is a Database model
type DBModel struct {
	name  string
	types map[string]reflect.Type
}

// NewModel returns a new instance of a model from a specific string
func (db DBModel) NewModel(table string) (Model, error) {
	mtype, ok := db.types[table]
	if !ok {
		return nil, fmt.Errorf("table %s not found in database model", string(table))
	}
	model := reflect.New(mtype.Elem())
	return model.Interface().(Model), nil
}

// Types returns the DBModel Types
// the DBModel types is a map of reflect.Types indexed by string
// The reflect.Type is a pointer to a struct that contains 'ovs' tags
// as described above. Such pointer to struct also implements the Model interface
func (db DBModel) Types() map[string]reflect.Type {
	return db.types
}

// Name returns the database name
func (db DBModel) Name() string {
	return db.name
}

// FindTable returns the string associated with a reflect.Type or ""
func (db DBModel) FindTable(mType reflect.Type) string {
	for table, tType := range db.types {
		if tType == mType {
			return table
		}
	}
	return ""
}

// Validate validates the DatabaseModel against the input schema
// Returns all the errors detected
func (db DBModel) Validate(schema *ovsdb.DatabaseSchema) []error {
	var errors []error
	if db.name != schema.Name {
		errors = append(errors, fmt.Errorf("database model name (%s) does not match schema (%s)",
			db.name, schema.Name))
	}

	for tableName := range db.types {
		tableSchema := schema.Table(tableName)
		if tableSchema == nil {
			errors = append(errors, fmt.Errorf("database model contains a model for table %s that does not exist in schema", tableName))
			continue
		}
		model, err := db.NewModel(tableName)
		if err != nil {
			errors = append(errors, err)
			continue
		}
		if _, err := mapper.NewInfo(tableSchema, model); err != nil {
			errors = append(errors, err)
		}
	}
	return errors
}

// NewDBModel constructs a DBModel based on a database name and dictionary of models indexed by table name
func NewDBModel(name string, models map[string]Model) (*DBModel, error) {
	types := make(map[string]reflect.Type, len(models))
	for table, model := range models {
		modelType := reflect.TypeOf(model)
		if modelType.Kind() != reflect.Ptr || modelType.Elem().Kind() != reflect.Struct {
			return nil, fmt.Errorf("model is expected to be a pointer to struct")
		}
		hasUUID := false
		for i := 0; i < modelType.Elem().NumField(); i++ {
			if field := modelType.Elem().Field(i); field.Tag.Get("ovsdb") == "_uuid" &&
				field.Type.Kind() == reflect.String {
				hasUUID = true
			}
		}
		if !hasUUID {
			return nil, fmt.Errorf("model is expected to have a string field called uuid")
		}

		types[table] = reflect.TypeOf(model)
	}
	return &DBModel{
		types: types,
		name:  name,
	}, nil
}

func modelSetUUID(model Model, uuid string) error {
	modelVal := reflect.ValueOf(model).Elem()
	for i := 0; i < modelVal.NumField(); i++ {
		if field := modelVal.Type().Field(i); field.Tag.Get("ovsdb") == "_uuid" &&
			field.Type.Kind() == reflect.String {
			modelVal.Field(i).Set(reflect.ValueOf(uuid))
			return nil
		}
	}
	return fmt.Errorf("model is expected to have a string field mapped to column _uuid")
}

// Condition is a model-based representation of an OVSDB Condition
type Condition struct {
	// Pointer to the field of the model where the operation applies
	Field interface{}
	// Condition function
	Function ovsdb.ConditionFunction
	// Value to use in the condition
	Value interface{}
}

// Mutation is a model-based representation of an OVSDB Mutation
type Mutation struct {
	// Pointer to the field of the model that shall be mutated
	Field interface{}
	// String representing the mutator (as per RFC7047)
	Mutator ovsdb.Mutator
	// Value to use in the mutation
	Value interface{}
}

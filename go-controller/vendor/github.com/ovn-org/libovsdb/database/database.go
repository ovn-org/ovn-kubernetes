package database

import (
	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// Database abstracts a database that a server can use to store and transact data
type Database interface {
	CreateDatabase(database string, model ovsdb.DatabaseSchema) error
	Exists(database string) bool
	NewTransaction(database string) Transaction
	Commit(database string, id uuid.UUID, update Update) error
	CheckIndexes(database string, table string, m model.Model) error
	List(database, table string, conditions ...ovsdb.Condition) (map[string]model.Model, error)
	Get(database, table string, uuid string) (model.Model, error)
	GetReferences(database, table, row string) (References, error)
}

// Transaction abstracts a database transaction that can generate database
// updates
type Transaction interface {
	Transact(operations ...ovsdb.Operation) ([]*ovsdb.OperationResult, Update)
}

// Update abstracts an update that can be committed to a database
type Update interface {
	GetUpdatedTables() []string
	ForEachModelUpdate(table string, do func(uuid string, old, new model.Model) error) error
	ForEachRowUpdate(table string, do func(uuid string, row ovsdb.RowUpdate2) error) error
	ForReferenceUpdates(do func(references References) error) error
}

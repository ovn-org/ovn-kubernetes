package inmemory

import (
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/cache"
	dbase "github.com/ovn-org/libovsdb/database"
	"github.com/ovn-org/libovsdb/database/transaction"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

type inMemoryDatabase struct {
	databases  map[string]*cache.TableCache
	models     map[string]model.ClientDBModel
	references map[string]dbase.References
	logger     *logr.Logger
	mutex      sync.RWMutex
}

func NewDatabase(models map[string]model.ClientDBModel) dbase.Database {
	logger := stdr.NewWithOptions(log.New(os.Stderr, "", log.LstdFlags), stdr.Options{LogCaller: stdr.All}).WithName("database")
	return &inMemoryDatabase{
		databases:  make(map[string]*cache.TableCache),
		models:     models,
		references: make(map[string]dbase.References),
		mutex:      sync.RWMutex{},
		logger:     &logger,
	}
}

func (db *inMemoryDatabase) NewTransaction(dbName string) dbase.Transaction {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	var model model.DatabaseModel
	if database, ok := db.databases[dbName]; ok {
		model = database.DatabaseModel()
	}
	transaction := transaction.NewTransaction(model, dbName, db, db.logger)
	return &transaction
}

func (db *inMemoryDatabase) CreateDatabase(name string, schema ovsdb.DatabaseSchema) error {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	var mo model.ClientDBModel
	var ok bool
	if mo, ok = db.models[schema.Name]; !ok {
		return fmt.Errorf("no db model provided for schema with name %s", name)
	}
	dbModel, errs := model.NewDatabaseModel(schema, mo)
	if len(errs) > 0 {
		return fmt.Errorf("failed to create DatabaseModel: %#+v", errs)
	}
	database, err := cache.NewTableCache(dbModel, nil, nil)
	if err != nil {
		return err
	}
	db.databases[name] = database
	db.references[name] = make(dbase.References)
	return nil
}

func (db *inMemoryDatabase) Exists(name string) bool {
	db.mutex.RLock()
	defer db.mutex.RUnlock()
	_, ok := db.databases[name]
	return ok
}

func (db *inMemoryDatabase) Commit(database string, id uuid.UUID, update dbase.Update) error {
	if !db.Exists(database) {
		return fmt.Errorf("db does not exist")
	}
	db.mutex.RLock()
	targetDb := db.databases[database]
	db.mutex.RUnlock()

	err := targetDb.ApplyCacheUpdate(update)
	if err != nil {
		return err
	}

	return update.ForReferenceUpdates(func(references dbase.References) error {
		db.references[database].UpdateReferences(references)
		return nil
	})
}

func (db *inMemoryDatabase) CheckIndexes(database string, table string, m model.Model) error {
	if !db.Exists(database) {
		return nil
	}
	db.mutex.RLock()
	targetDb := db.databases[database]
	db.mutex.RUnlock()
	targetTable := targetDb.Table(table)
	return targetTable.IndexExists(m)
}

func (db *inMemoryDatabase) List(database, table string, conditions ...ovsdb.Condition) (map[string]model.Model, error) {
	if !db.Exists(database) {
		return nil, fmt.Errorf("db does not exist")
	}
	db.mutex.RLock()
	targetDb := db.databases[database]
	db.mutex.RUnlock()

	targetTable := targetDb.Table(table)
	if targetTable == nil {
		return nil, fmt.Errorf("table does not exist")
	}

	return targetTable.RowsByCondition(conditions)
}

func (db *inMemoryDatabase) Get(database, table string, uuid string) (model.Model, error) {
	if !db.Exists(database) {
		return nil, fmt.Errorf("db does not exist")
	}
	db.mutex.RLock()
	targetDb := db.databases[database]
	db.mutex.RUnlock()

	targetTable := targetDb.Table(table)
	if targetTable == nil {
		return nil, fmt.Errorf("table does not exist")
	}
	return targetTable.Row(uuid), nil
}

func (db *inMemoryDatabase) GetReferences(database, table, row string) (dbase.References, error) {
	if !db.Exists(database) {
		return nil, fmt.Errorf("db does not exist")
	}
	db.mutex.RLock()
	defer db.mutex.RUnlock()
	return db.references[database].GetReferences(table, row), nil
}

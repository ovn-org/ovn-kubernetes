package client

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/cenkalti/rpc2"
	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/cache"
	db "github.com/ovn-org/libovsdb/database"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// Transact performs the provided Operations on the database
// RFC 7047 : transact
func (o *ovsdbClient) Transact(ctx context.Context, operation ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	// if transactions are to be validated, they need to be serialized as well
	if o.options.validateTransactions {
		return o.transactSerial(ctx, operation...)
	}
	return o.transactReconnect(ctx, operation...)
}

func (o *ovsdbClient) transactSerial(ctx context.Context, operation ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	select {
	case o.transactionLockCh <- struct{}{}:
		res, err := o.transactReconnect(ctx, operation...)
		<-o.transactionLockCh
		return res, err
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: while awaiting previous transaction to complete", ctx.Err())
	}
}

func (o *ovsdbClient) transactReconnect(ctx context.Context, operation ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	o.rpcMutex.RLock()
	if o.rpcClient == nil || !o.connected {
		o.rpcMutex.RUnlock()
		if o.options.reconnect {
			o.logger.V(5).Info("blocking transaction until reconnected", "operations", operation)
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
		ReconnectWaitLoop:
			for {
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("%w: while awaiting reconnection", ctx.Err())
				case <-ticker.C:
					o.rpcMutex.RLock()
					if o.rpcClient != nil && o.connected {
						break ReconnectWaitLoop
					}
					o.rpcMutex.RUnlock()
				}
			}
		} else {
			return nil, ErrNotConnected
		}
	}
	defer o.rpcMutex.RUnlock()

	return o.transact(ctx, o.primaryDBName, operation...)
}

func (o *ovsdbClient) transact(ctx context.Context, dbName string, operation ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	if err := o.ValidateOperations(dbName, operation...); err != nil {
		return nil, err
	}

	args := ovsdb.NewTransactArgs(dbName, operation...)
	if o.rpcClient == nil {
		return nil, ErrNotConnected
	}
	o.logger.V(4).Info("transacting operations", "database", dbName, "operations", operation)
	var reply []ovsdb.OperationResult
	err := o.rpcClient.CallWithContext(ctx, "transact", args, &reply)
	if err != nil {
		if err == rpc2.ErrShutdown {
			return nil, ErrNotConnected
		}
		return nil, err
	}
	return reply, nil
}

func (o *ovsdbClient) ValidateOperations(dbName string, operations ...ovsdb.Operation) error {
	database := o.databases[dbName]
	database.modelMutex.RLock()
	schema := o.databases[dbName].model.Schema
	database.modelMutex.RUnlock()
	if reflect.DeepEqual(schema, ovsdb.DatabaseSchema{}) {
		return fmt.Errorf("cannot transact to database %s: schema unknown", dbName)
	}

	// validate operations against the schema
	if ok := schema.ValidateOperations(operations...); !ok {
		return fmt.Errorf("validation failed for the operation")
	}

	// don't do any further validations unless the option is enabled
	if !o.options.validateTransactions {
		return nil
	}

	// validate operations against a temporary database built from the cache
	database.cacheMutex.RLock()
	defer database.cacheMutex.RUnlock()
	cacheDatabase := cacheDatabase{database.cache}
	transaction := db.NewTransaction(database.model, dbName, &cacheDatabase, o.logger)
	results, _ := transaction.Transact(operations)
	_, err := ovsdb.CheckOperationResults(results, operations)

	return err
}

type cacheDatabase struct {
	cache *cache.TableCache
}

func (c *cacheDatabase) CreateDatabase(database string, model ovsdb.DatabaseSchema) error {
	panic("not implemented") // TODO: Implement
}

func (c *cacheDatabase) Exists(database string) bool {
	panic("not implemented") // TODO: Implement
}

func (c *cacheDatabase) Commit(database string, id uuid.UUID, updates ovsdb.TableUpdates2) error {
	panic("not implemented") // TODO: Implement
}

func (c *cacheDatabase) CheckIndexes(database string, table string, m model.Model) error {
	targetTable := c.cache.Table(table)
	if targetTable == nil {
		return fmt.Errorf("table does not exist")
	}
	return targetTable.IndexExists(m)
}

func (c *cacheDatabase) List(database string, table string, conditions ...ovsdb.Condition) (map[string]model.Model, error) {
	targetTable := c.cache.Table(table)
	if targetTable == nil {
		return nil, fmt.Errorf("table does not exist")
	}
	return targetTable.RowsByConditionShallow(conditions)
}

func (c *cacheDatabase) Get(database string, table string, uuid string) (model.Model, error) {
	panic("not implemented") // TODO: Implement
}

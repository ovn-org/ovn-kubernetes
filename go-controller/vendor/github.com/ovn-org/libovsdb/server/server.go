package server

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"github.com/cenkalti/rpc2"
	"github.com/cenkalti/rpc2/jsonrpc"
	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// OvsdbServer is an ovsdb server
type OvsdbServer struct {
	srv          *rpc2.Server
	listener     net.Listener
	done         chan struct{}
	db           Database
	dbUpdates    chan ovsdb.TableUpdates
	ready        bool
	readyMutex   sync.RWMutex
	models       map[string]DatabaseModel
	modelsMutex  sync.RWMutex
	monitors     map[*rpc2.Client]*connectionMonitors
	monitorMutex sync.RWMutex
}

type DatabaseModel struct {
	Model  *model.DBModel
	Schema *ovsdb.DatabaseSchema
}

// NewOvsdbServer returns a new OvsdbServer
func NewOvsdbServer(db Database, models ...DatabaseModel) (*OvsdbServer, error) {
	o := &OvsdbServer{
		done:         make(chan struct{}, 1),
		db:           db,
		models:       make(map[string]DatabaseModel),
		modelsMutex:  sync.RWMutex{},
		monitors:     make(map[*rpc2.Client]*connectionMonitors),
		monitorMutex: sync.RWMutex{},
		dbUpdates:    make(chan ovsdb.TableUpdates),
	}
	o.modelsMutex.Lock()
	for _, model := range models {
		o.models[model.Schema.Name] = model
	}
	o.modelsMutex.Unlock()
	for database, model := range o.models {
		if err := o.db.CreateDatabase(database, model.Schema); err != nil {
			return nil, err
		}
	}
	o.srv = rpc2.NewServer()
	o.srv.Handle("list_dbs", o.ListDatabases)
	o.srv.Handle("get_schema", o.GetSchema)
	o.srv.Handle("transact", o.Transact)
	o.srv.Handle("cancel", o.Cancel)
	o.srv.Handle("monitor", o.Monitor)
	o.srv.Handle("monitor_cancel", o.MonitorCancel)
	o.srv.Handle("steal", o.Steal)
	o.srv.Handle("unlock", o.Unlock)
	o.srv.Handle("echo", o.Echo)
	return o, nil
}

// Serve starts the OVSDB server on the given path and protocol
func (o *OvsdbServer) Serve(protocol string, path string) error {
	var err error
	o.listener, err = net.Listen(protocol, path)
	if err != nil {
		return err
	}
	go o.dispatch()
	o.readyMutex.Lock()
	o.ready = true
	o.readyMutex.Unlock()
	for {
		conn, err := o.listener.Accept()
		if err != nil {
			if !o.Ready() {
				return nil
			}
			return err
		}

		// TODO: Need to cleanup when connection is closed
		go o.srv.ServeCodec(jsonrpc.NewJSONCodec(conn))
	}
}

// Close closes the OvsdbServer
func (o *OvsdbServer) Close() {
	o.readyMutex.Lock()
	o.ready = false
	o.readyMutex.Unlock()
	o.listener.Close()
	close(o.done)
}

// Ready returns true if a server is ready to handle connections
func (o *OvsdbServer) Ready() bool {
	o.readyMutex.RLock()
	defer o.readyMutex.RUnlock()
	return o.ready
}

// ListDatabases lists the databases in the current system
func (o *OvsdbServer) ListDatabases(client *rpc2.Client, args []interface{}, reply *[]string) error {
	dbs := []string{}
	o.modelsMutex.RLock()
	for _, db := range o.models {
		dbs = append(dbs, db.Schema.Name)
	}
	o.modelsMutex.RUnlock()
	*reply = dbs
	return nil
}

func (o *OvsdbServer) GetSchema(client *rpc2.Client, args []interface{}, reply *ovsdb.DatabaseSchema,
) error {
	db, ok := args[0].(string)
	if !ok {
		return fmt.Errorf("database %v is not a string", args[0])
	}
	o.modelsMutex.RLock()
	model, ok := o.models[db]
	if !ok {
		return fmt.Errorf("database %s does not exist", db)
	}
	o.modelsMutex.RUnlock()
	*reply = *model.Schema
	return nil
}

// Transact issues a new database transaction and returns the results
func (o *OvsdbServer) Transact(client *rpc2.Client, args []json.RawMessage, reply *[]ovsdb.OperationResult) error {
	if len(args) < 2 {
		return fmt.Errorf("not enough args")
	}
	var db string
	err := json.Unmarshal(args[0], &db)
	if err != nil {
		return fmt.Errorf("database %v is not a string", args[0])
	}
	if !o.db.Exists(db) {
		return fmt.Errorf("db does not exist")
	}
	var ops []ovsdb.Operation
	namedUUID := make(map[string]ovsdb.UUID)
	for i := 1; i < len(args); i++ {
		var op ovsdb.Operation
		err = json.Unmarshal(args[i], &op)
		if err != nil {
			return err
		}
		if op.UUIDName != "" {
			newUUID := uuid.NewString()
			namedUUID[op.UUIDName] = ovsdb.UUID{GoUUID: newUUID}
			op.UUIDName = newUUID
		}
		for i, condition := range op.Where {
			op.Where[i].Value = expandNamedUUID(condition.Value, namedUUID)
		}
		for i, mutation := range op.Mutations {
			op.Mutations[i].Value = expandNamedUUID(mutation.Value, namedUUID)
		}
		ops = append(ops, op)
	}
	response, update := o.db.Transact(db, ops)
	*reply = response
	o.dbUpdates <- update
	return nil
}

// Cancel cancels the last transaction
func (o *OvsdbServer) Cancel(client *rpc2.Client, args []interface{}, reply *[]interface{}) error {
	return fmt.Errorf("not implemented")
}

// Monitor monitors a given database table and provides updates to the client via an RPC callback
func (o *OvsdbServer) Monitor(client *rpc2.Client, args []json.RawMessage, reply *ovsdb.TableUpdates) error {
	var db string
	if err := json.Unmarshal(args[0], &db); err != nil {
		return fmt.Errorf("database %v is not a string", args[0])
	}
	if !o.db.Exists(db) {
		return fmt.Errorf("db does not exist")
	}
	var value string
	if err := json.Unmarshal(args[1], &value); err != nil {
		return fmt.Errorf("values %v is not a string", args[1])
	}
	var request map[string]*ovsdb.MonitorRequest
	if err := json.Unmarshal(args[2], &request); err != nil {
		return err
	}
	o.monitorMutex.Lock()
	defer o.monitorMutex.Unlock()
	clientMonitors, ok := o.monitors[client]
	if !ok {
		o.monitors[client] = newConnectionMonitors()
	} else {
		if _, ok := clientMonitors.monitors[value]; ok {
			return fmt.Errorf("monitor with that value already exists")
		}
	}
	tableUpdates := make(ovsdb.TableUpdates)
	for t, request := range request {
		rows := o.db.Select(db, t, nil, request.Columns)
		for i := range rows.Rows {
			tu := make(ovsdb.TableUpdate)
			uuid := rows.Rows[i]["_uuid"].(ovsdb.UUID).GoUUID
			tu[uuid] = &ovsdb.RowUpdate{
				New: &rows.Rows[i],
			}
			tableUpdates.AddTableUpdate(t, tu)
		}
	}
	*reply = tableUpdates
	o.monitors[client].monitors[value] = newMonitor(value, request, client)
	return nil
}

// MonitorCancel cancels a monitor on a given table
func (o *OvsdbServer) MonitorCancel(client *rpc2.Client, args []interface{}, reply *[]interface{}) error {
	return fmt.Errorf("not implemented")
}

// Lock acquires a lock on a table for a the client
func (o *OvsdbServer) Lock(client *rpc2.Client, args []interface{}, reply *[]interface{}) error {
	return fmt.Errorf("not implemented")
}

// Steal steals a lock for a client
func (o *OvsdbServer) Steal(client *rpc2.Client, args []interface{}, reply *[]interface{}) error {
	return fmt.Errorf("not implemented")
}

// Unlock releases a lock for a client
func (o *OvsdbServer) Unlock(client *rpc2.Client, args []interface{}, reply *[]interface{}) error {
	return fmt.Errorf("not implemented")
}

// Echo tests the liveness of the connection
func (o *OvsdbServer) Echo(client *rpc2.Client, args []interface{}, reply *[]interface{}) error {
	echoReply := make([]interface{}, len(args))
	copy(echoReply, args)
	*reply = echoReply
	return nil
}

func (o *OvsdbServer) dispatch() {
	for {
		select {
		case update := <-o.dbUpdates:
			o.monitorMutex.RLock()
			for _, c := range o.monitors {
				for _, m := range c.monitors {
					m.Enqueue(update)
				}
			}
			o.monitorMutex.RUnlock()
		case <-o.done:
			o.monitorMutex.RLock()
			for _, c := range o.monitors {
				for _, m := range c.monitors {
					close(m.stopCh)
				}
			}
			o.monitorMutex.RUnlock()
			return
		}
	}
}

func expandNamedUUID(value interface{}, namedUUID map[string]ovsdb.UUID) interface{} {
	if uuid, ok := value.(ovsdb.UUID); ok {
		if newUUID, ok := namedUUID[uuid.GoUUID]; ok {
			return newUUID
		}
	}
	if set, ok := value.(ovsdb.OvsSet); ok {
		for i, s := range set.GoSet {
			if _, ok := s.(ovsdb.UUID); !ok {
				return value
			}
			uuid := s.(ovsdb.UUID)
			if newUUID, ok := namedUUID[uuid.GoUUID]; ok {
				set.GoSet[i] = newUUID
			}
		}
	}
	if m, ok := value.(ovsdb.OvsMap); ok {
		for k, v := range m.GoMap {
			if uuid, ok := v.(ovsdb.UUID); ok {
				if newUUID, ok := namedUUID[uuid.GoUUID]; ok {
					m.GoMap[k] = newUUID
				}
			}
			if uuid, ok := k.(ovsdb.UUID); ok {
				if newUUID, ok := namedUUID[uuid.GoUUID]; ok {
					m.GoMap[newUUID] = m.GoMap[k]
					delete(m.GoMap, uuid)
				}
			}
		}
	}
	return value
}

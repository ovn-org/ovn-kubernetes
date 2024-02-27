package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"

	"github.com/cenkalti/rpc2"
	"github.com/cenkalti/rpc2/jsonrpc"
	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/database"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// OvsdbServer is an ovsdb server
type OvsdbServer struct {
	srv          *rpc2.Server
	listener     net.Listener
	done         chan struct{}
	db           database.Database
	ready        bool
	doEcho       bool
	readyMutex   sync.RWMutex
	models       map[string]model.DatabaseModel
	modelsMutex  sync.RWMutex
	monitors     map[*rpc2.Client]*connectionMonitors
	monitorMutex sync.RWMutex
	logger       logr.Logger
	txnMutex     sync.Mutex
}

func init() {
	stdr.SetVerbosity(5)
}

// NewOvsdbServer returns a new OvsdbServer
func NewOvsdbServer(db database.Database, models ...model.DatabaseModel) (*OvsdbServer, error) {
	l := stdr.NewWithOptions(log.New(os.Stderr, "", log.LstdFlags), stdr.Options{LogCaller: stdr.All}).WithName("server")
	o := &OvsdbServer{
		done:         make(chan struct{}, 1),
		doEcho:       true,
		db:           db,
		models:       make(map[string]model.DatabaseModel),
		modelsMutex:  sync.RWMutex{},
		monitors:     make(map[*rpc2.Client]*connectionMonitors),
		monitorMutex: sync.RWMutex{},
		logger:       l,
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
	o.srv.Handle("monitor_cond", o.MonitorCond)
	o.srv.Handle("monitor_cond_since", o.MonitorCondSince)
	o.srv.Handle("monitor_cancel", o.MonitorCancel)
	o.srv.Handle("steal", o.Steal)
	o.srv.Handle("unlock", o.Unlock)
	o.srv.Handle("echo", o.Echo)
	return o, nil
}

// OnConnect registers a function to run when a client connects.
func (o *OvsdbServer) OnConnect(f func(*rpc2.Client)) {
	o.srv.OnConnect(f)
}

// OnDisConnect registers a function to run when a client disconnects.
func (o *OvsdbServer) OnDisConnect(f func(*rpc2.Client)) {
	o.srv.OnDisconnect(f)
}

func (o *OvsdbServer) DoEcho(ok bool) {
	o.readyMutex.Lock()
	o.doEcho = ok
	o.readyMutex.Unlock()
}

// Serve starts the OVSDB server on the given path and protocol
func (o *OvsdbServer) Serve(protocol string, path string) error {
	var err error
	o.listener, err = net.Listen(protocol, path)
	if err != nil {
		return err
	}
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

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
	}

	return false
}

// Close closes the OvsdbServer
func (o *OvsdbServer) Close() {
	o.readyMutex.Lock()
	o.ready = false
	o.readyMutex.Unlock()
	// Only close the listener if Serve() has been called
	if o.listener != nil {
		if err := o.listener.Close(); err != nil {
			o.logger.Error(err, "failed to close listener")
		}
	}
	if !isClosed(o.done) {
		close(o.done)
	}
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
	*reply = model.Schema
	return nil
}

// Transact issues a new database transaction and returns the results
func (o *OvsdbServer) Transact(client *rpc2.Client, args []json.RawMessage, reply *[]*ovsdb.OperationResult) error {
	// While allowing other rpc handlers to run in parallel, this ovsdb server expects transactions
	// to be serialized. The following mutex ensures that.
	// Ref: https://github.com/cenkalti/rpc2/blob/c1acbc6ec984b7ae6830b6a36b62f008d5aefc4c/client.go#L187
	o.txnMutex.Lock()
	defer o.txnMutex.Unlock()

	if len(args) < 2 {
		return fmt.Errorf("not enough args")
	}
	var db string
	err := json.Unmarshal(args[0], &db)
	if err != nil {
		return fmt.Errorf("database %v is not a string", args[0])
	}
	var ops []ovsdb.Operation
	for i := 1; i < len(args); i++ {
		var op ovsdb.Operation
		err = json.Unmarshal(args[i], &op)
		if err != nil {
			return err
		}
		ops = append(ops, op)
	}
	response, updates := o.transact(db, ops)
	*reply = response
	for _, operResult := range response {
		if operResult.Error != "" {
			o.logger.Error(errors.New("failed to process operation"), "Skipping transaction DB commit due to error", "operations", ops, "results", response, "operation error", operResult.Error)
			return nil
		}
	}
	transactionID := uuid.New()
	o.processMonitors(transactionID, updates)
	return o.db.Commit(db, transactionID, updates)
}

func (o *OvsdbServer) transact(name string, operations []ovsdb.Operation) ([]*ovsdb.OperationResult, database.Update) {
	transaction := o.db.NewTransaction(name)
	return transaction.Transact(operations...)
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
	value := string(args[1])
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

	transaction := o.db.NewTransaction(db)

	tableUpdates := make(ovsdb.TableUpdates)
	for t, request := range request {
		op := ovsdb.Operation{Op: ovsdb.OperationSelect, Table: t, Columns: request.Columns}
		result, _ := transaction.Transact(op)
		if len(result) == 0 || len(result[0].Rows) == 0 {
			continue
		}
		rows := result[0].Rows
		tableUpdates[t] = make(ovsdb.TableUpdate, len(rows))
		for i := range rows {
			uuid := rows[i]["_uuid"].(ovsdb.UUID).GoUUID
			tableUpdates[t][uuid] = &ovsdb.RowUpdate{New: &rows[i]}
		}
	}
	*reply = tableUpdates
	o.monitors[client].monitors[value] = newMonitor(value, request, client)
	return nil
}

// MonitorCond monitors a given database table and provides updates to the client via an RPC callback
func (o *OvsdbServer) MonitorCond(client *rpc2.Client, args []json.RawMessage, reply *ovsdb.TableUpdates2) error {
	var db string
	if err := json.Unmarshal(args[0], &db); err != nil {
		return fmt.Errorf("database %v is not a string", args[0])
	}
	if !o.db.Exists(db) {
		return fmt.Errorf("db does not exist")
	}
	value := string(args[1])
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

	transaction := o.db.NewTransaction(db)

	tableUpdates := make(ovsdb.TableUpdates2)
	for t, request := range request {
		op := ovsdb.Operation{Op: ovsdb.OperationSelect, Table: t, Columns: request.Columns}
		result, _ := transaction.Transact(op)
		if len(result) == 0 || len(result[0].Rows) == 0 {
			continue
		}
		rows := result[0].Rows
		tableUpdates[t] = make(ovsdb.TableUpdate2, len(rows))
		for i := range rows {
			uuid := rows[i]["_uuid"].(ovsdb.UUID).GoUUID
			tableUpdates[t][uuid] = &ovsdb.RowUpdate2{Initial: &rows[i]}
		}
	}
	*reply = tableUpdates
	o.monitors[client].monitors[value] = newConditionalMonitor(value, request, client)
	return nil
}

// MonitorCondSince monitors a given database table and provides updates to the client via an RPC callback
func (o *OvsdbServer) MonitorCondSince(client *rpc2.Client, args []json.RawMessage, reply *ovsdb.MonitorCondSinceReply) error {
	var db string
	if err := json.Unmarshal(args[0], &db); err != nil {
		return fmt.Errorf("database %v is not a string", args[0])
	}
	if !o.db.Exists(db) {
		return fmt.Errorf("db does not exist")
	}
	value := string(args[1])
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

	transaction := o.db.NewTransaction(db)

	tableUpdates := make(ovsdb.TableUpdates2)
	for t, request := range request {
		op := ovsdb.Operation{Op: ovsdb.OperationSelect, Table: t, Columns: request.Columns}
		result, _ := transaction.Transact(op)
		if len(result) == 0 || len(result[0].Rows) == 0 {
			continue
		}
		rows := result[0].Rows
		tableUpdates[t] = make(ovsdb.TableUpdate2, len(rows))
		for i := range rows {
			uuid := rows[i]["_uuid"].(ovsdb.UUID).GoUUID
			tableUpdates[t][uuid] = &ovsdb.RowUpdate2{Initial: &rows[i]}
		}
	}
	*reply = ovsdb.MonitorCondSinceReply{Found: false, LastTransactionID: "00000000-0000-0000-000000000000", Updates: tableUpdates}
	o.monitors[client].monitors[value] = newConditionalSinceMonitor(value, request, client)
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
	o.readyMutex.Lock()
	defer o.readyMutex.Unlock()
	if !o.doEcho {
		return fmt.Errorf("no echo reply")
	}
	echoReply := make([]interface{}, len(args))
	copy(echoReply, args)
	*reply = echoReply
	return nil
}

func (o *OvsdbServer) processMonitors(id uuid.UUID, update database.Update) {
	o.monitorMutex.RLock()
	for _, c := range o.monitors {
		for _, m := range c.monitors {
			switch m.kind {
			case monitorKindOriginal:
				m.Send(update)
			case monitorKindConditional:
				m.Send2(update)
			case monitorKindConditionalSince:
				m.Send3(id, update)
			}
		}
	}
	o.monitorMutex.RUnlock()
}

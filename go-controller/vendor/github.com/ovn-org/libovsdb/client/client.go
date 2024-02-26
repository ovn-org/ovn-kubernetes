package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cenkalti/rpc2"
	"github.com/cenkalti/rpc2/jsonrpc"
	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"github.com/ovn-org/libovsdb/cache"
	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/libovsdb/ovsdb/serverdb"
)

// Constants defined for libovsdb
const (
	SSL  = "ssl"
	TCP  = "tcp"
	UNIX = "unix"
)

const serverDB = "_Server"

// ErrNotConnected is an error returned when the client is not connected
var ErrNotConnected = errors.New("not connected")

// ErrAlreadyConnected is an error returned when the client is already connected
var ErrAlreadyConnected = errors.New("already connected")

// ErrUnsupportedRPC is an error returned when an unsupported RPC method is called
var ErrUnsupportedRPC = errors.New("unsupported rpc")

// Client represents an OVSDB Client Connection
// It provides all the necessary functionality to Connect to a server,
// perform transactions, and build your own replica of the database with
// Monitor or MonitorAll. It also provides a Cache that is populated from OVSDB
// update notifications.
type Client interface {
	Connect(context.Context) error
	Disconnect()
	Close()
	Schema() ovsdb.DatabaseSchema
	Cache() *cache.TableCache
	UpdateEndpoints([]string)
	SetOption(Option) error
	Connected() bool
	DisconnectNotify() chan struct{}
	Echo(context.Context) error
	Transact(context.Context, ...ovsdb.Operation) ([]ovsdb.OperationResult, error)
	Monitor(context.Context, *Monitor) (MonitorCookie, error)
	MonitorAll(context.Context) (MonitorCookie, error)
	MonitorCancel(ctx context.Context, cookie MonitorCookie) error
	NewMonitor(...MonitorOption) *Monitor
	CurrentEndpoint() string
	API
}

type bufferedUpdate struct {
	updates   *ovsdb.TableUpdates
	updates2  *ovsdb.TableUpdates2
	lastTxnID string
}

type epInfo struct {
	address  string
	serverID string
}

// ovsdbClient is an OVSDB client
type ovsdbClient struct {
	options   *options
	metrics   metrics
	connected bool
	rpcClient *rpc2.Client
	rpcMutex  sync.RWMutex
	// endpoints contains all possible endpoints; the first element is
	// the active endpoint if connected=true
	endpoints []*epInfo

	// The name of the "primary" database - that is to say, the DB
	// that the user expects to interact with.
	primaryDBName string
	databases     map[string]*database

	errorCh       chan error
	stopCh        chan struct{}
	disconnect    chan struct{}
	shutdown      bool
	shutdownMutex sync.Mutex

	handlerShutdown *sync.WaitGroup

	trafficSeen chan struct{}

	logger *logr.Logger
}

// database is everything needed to map between go types and an ovsdb Database
type database struct {
	// model encapsulates the database schema and model of the database we're connecting to
	model model.DatabaseModel
	// modelMutex protects model from being replaced (via reconnect) while in use
	modelMutex sync.RWMutex

	// cache is used to store the updates for monitored tables
	cache *cache.TableCache
	// cacheMutex protects cache from being replaced (via reconnect) while in use
	cacheMutex sync.RWMutex

	api API

	// any ongoing monitors, so we can re-create them if we disconnect
	monitors      map[string]*Monitor
	monitorsMutex sync.Mutex

	// tracks any outstanding updates while waiting for a monitor response
	deferUpdates    bool
	deferredUpdates []*bufferedUpdate
}

// NewOVSDBClient creates a new OVSDB Client with the provided
// database model. The client can be configured using one or more Option(s),
// like WithTLSConfig. If no WithEndpoint option is supplied, the default of
// unix:/var/run/openvswitch/ovsdb.sock is used
func NewOVSDBClient(clientDBModel model.ClientDBModel, opts ...Option) (Client, error) {
	return newOVSDBClient(clientDBModel, opts...)
}

// newOVSDBClient creates a new ovsdbClient
func newOVSDBClient(clientDBModel model.ClientDBModel, opts ...Option) (*ovsdbClient, error) {
	ovs := &ovsdbClient{
		primaryDBName: clientDBModel.Name(),
		databases: map[string]*database{
			clientDBModel.Name(): {
				model:           model.NewPartialDatabaseModel(clientDBModel),
				monitors:        make(map[string]*Monitor),
				deferUpdates:    true,
				deferredUpdates: make([]*bufferedUpdate, 0),
			},
		},
		errorCh:         make(chan error),
		handlerShutdown: &sync.WaitGroup{},
		disconnect:      make(chan struct{}),
	}
	var err error
	ovs.options, err = newOptions(opts...)
	if err != nil {
		return nil, err
	}
	for _, address := range ovs.options.endpoints {
		ovs.endpoints = append(ovs.endpoints, &epInfo{address: address})
	}

	if ovs.options.logger == nil {
		// create a new logger to log to stdout
		l := stdr.NewWithOptions(log.New(os.Stderr, "", log.LstdFlags), stdr.Options{LogCaller: stdr.All}).WithName("libovsdb").WithValues(
			"database", ovs.primaryDBName,
		)
		stdr.SetVerbosity(5)
		ovs.logger = &l
	} else {
		// add the "database" value to the structured logger
		// to make it easier to tell between different DBs (e.g. ovn nbdb vs. sbdb)
		l := ovs.options.logger.WithValues(
			"database", ovs.primaryDBName,
		)
		ovs.logger = &l
	}
	ovs.metrics.init(clientDBModel.Name(), ovs.options.metricNamespace, ovs.options.metricSubsystem)
	ovs.registerMetrics()

	// if we should only connect to the leader, then add the special "_Server" database as well
	if ovs.options.leaderOnly {
		sm, err := serverdb.FullDatabaseModel()
		if err != nil {
			return nil, fmt.Errorf("could not initialize model _Server: %w", err)
		}
		ovs.databases[serverDB] = &database{
			model:    model.NewPartialDatabaseModel(sm),
			monitors: make(map[string]*Monitor),
		}
	}

	return ovs, nil
}

// Connect opens a connection to an OVSDB Server using the
// endpoint provided when the Client was created.
// The connection can be configured using one or more Option(s), like WithTLSConfig
// If no WithEndpoint option is supplied, the default of unix:/var/run/openvswitch/ovsdb.sock is used
func (o *ovsdbClient) Connect(ctx context.Context) error {
	if err := o.connect(ctx, false); err != nil {
		if err == ErrAlreadyConnected {
			return nil
		}
		return err
	}
	if o.options.leaderOnly {
		if err := o.watchForLeaderChange(); err != nil {
			return err
		}
	}
	return nil
}

// moveEndpointFirst makes the endpoint requested by active the first element
// in the endpoints slice, indicating it is the active endpoint
func (o *ovsdbClient) moveEndpointFirst(i int) {
	firstEp := o.endpoints[i]
	othereps := append(o.endpoints[:i], o.endpoints[i+1:]...)
	o.endpoints = append([]*epInfo{firstEp}, othereps...)
}

// moveEndpointLast moves the requested endpoint to the end of the list
func (o *ovsdbClient) moveEndpointLast(i int) {
	lastEp := o.endpoints[i]
	othereps := append(o.endpoints[:i], o.endpoints[i+1:]...)
	o.endpoints = append(othereps, lastEp)
}

func (o *ovsdbClient) resetRPCClient() {
	if o.rpcClient != nil {
		o.rpcClient.Close()
		o.rpcClient = nil
	}
}

func (o *ovsdbClient) connect(ctx context.Context, reconnect bool) error {
	o.rpcMutex.Lock()
	defer o.rpcMutex.Unlock()
	if o.rpcClient != nil {
		return ErrAlreadyConnected
	}

	connected := false
	connectErrors := []error{}
	for i, endpoint := range o.endpoints {
		u, err := url.Parse(endpoint.address)
		if err != nil {
			return err
		}
		if sid, err := o.tryEndpoint(ctx, u); err != nil {
			o.resetRPCClient()
			connectErrors = append(connectErrors,
				fmt.Errorf("failed to connect to %s: %w", endpoint.address, err))
			continue
		} else {
			o.logger.V(3).Info("successfully connected", "endpoint", endpoint.address, "sid", sid)
			endpoint.serverID = sid
			o.moveEndpointFirst(i)
			connected = true
			break
		}
	}

	if !connected {
		if len(connectErrors) == 1 {
			return connectErrors[0]
		}
		var combined []string
		for _, e := range connectErrors {
			combined = append(combined, e.Error())
		}

		return fmt.Errorf("unable to connect to any endpoints: %s", strings.Join(combined, ". "))
	}

	// if we're reconnecting, re-start all the monitors
	if reconnect {
		o.logger.V(3).Info("reconnected - restarting monitors")
		for dbName, db := range o.databases {
			db.monitorsMutex.Lock()
			defer db.monitorsMutex.Unlock()

			// Purge entire cache if no monitors exist to update dynamically
			if len(db.monitors) == 0 {
				db.cache.Purge(db.model)
				continue
			}

			// Restart all monitors; each monitor will handle purging
			// the cache if necessary
			for id, request := range db.monitors {
				err := o.monitor(ctx, MonitorCookie{DatabaseName: dbName, ID: id}, true, request)
				if err != nil {
					o.resetRPCClient()
					return err
				}
			}
		}
	}

	go o.handleDisconnectNotification()
	if o.options.inactivityTimeout > 0 {
		o.handlerShutdown.Add(1)
		go o.handleInactivityProbes()
	}
	for _, db := range o.databases {
		o.handlerShutdown.Add(1)
		eventStopChan := make(chan struct{})
		go o.handleClientErrors(eventStopChan)
		o.handlerShutdown.Add(1)
		go func(db *database) {
			defer o.handlerShutdown.Done()
			db.cache.Run(o.stopCh)
			close(eventStopChan)
		}(db)
	}

	o.connected = true
	return nil
}

// tryEndpoint connects to a single database endpoint. Returns the
// server ID (if clustered) on success, or an error.
func (o *ovsdbClient) tryEndpoint(ctx context.Context, u *url.URL) (string, error) {
	o.logger.V(3).Info("trying to connect", "endpoint", fmt.Sprintf("%v", u))
	var dialer net.Dialer
	var err error
	var c net.Conn

	switch u.Scheme {
	case UNIX:
		c, err = dialer.DialContext(ctx, u.Scheme, u.Path)
	case TCP:
		c, err = dialer.DialContext(ctx, u.Scheme, u.Opaque)
	case SSL:
		dialer := tls.Dialer{
			Config: o.options.tlsConfig,
		}
		c, err = dialer.DialContext(ctx, "tcp", u.Opaque)
	default:
		err = fmt.Errorf("unknown network protocol %s", u.Scheme)
	}
	if err != nil {
		return "", fmt.Errorf("failed to open connection: %w", err)
	}

	o.createRPC2Client(c)

	serverDBNames, err := o.listDbs(ctx)
	if err != nil {
		return "", err
	}

	// for every requested database, ensure the DB exists in the server and
	// that the schema matches what we expect.
	for dbName, db := range o.databases {
		// check the server has what we want
		found := false
		for _, name := range serverDBNames {
			if name == dbName {
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("target database %s not found", dbName)
		}

		// load and validate the schema
		schema, err := o.getSchema(ctx, dbName)
		if err != nil {
			return "", err
		}

		db.modelMutex.Lock()
		var errors []error
		db.model, errors = model.NewDatabaseModel(schema, db.model.Client())
		db.modelMutex.Unlock()
		if len(errors) > 0 {
			var combined []string
			for _, err := range errors {
				combined = append(combined, err.Error())
			}
			return "", fmt.Errorf("database %s validation error (%d): %s",
				dbName, len(errors), strings.Join(combined, ". "))
		}

		db.cacheMutex.Lock()
		if db.cache == nil {
			db.cache, err = cache.NewTableCache(db.model, nil, o.logger)
			if err != nil {
				db.cacheMutex.Unlock()
				return "", err
			}
			db.api = newAPI(db.cache, o.logger)
		}
		db.cacheMutex.Unlock()
	}

	// check that this is the leader
	var sid string
	if o.options.leaderOnly {
		var leader bool
		leader, sid, err = o.isEndpointLeader(ctx)
		if err != nil {
			return "", err
		}
		if !leader {
			return "", fmt.Errorf("endpoint is not leader")
		}
	}
	return sid, nil
}

// createRPC2Client creates an rpcClient using the provided connection
// It is also responsible for setting up go routines for client-side event handling
// Should only be called when the mutex is held
func (o *ovsdbClient) createRPC2Client(conn net.Conn) {
	o.stopCh = make(chan struct{})
	if o.options.inactivityTimeout > 0 {
		o.trafficSeen = make(chan struct{})
	}
	o.rpcClient = rpc2.NewClientWithCodec(jsonrpc.NewJSONCodec(conn))
	o.rpcClient.SetBlocking(true)
	o.rpcClient.Handle("echo", func(_ *rpc2.Client, args []interface{}, reply *[]interface{}) error {
		return o.echo(args, reply)
	})
	o.rpcClient.Handle("update", func(_ *rpc2.Client, args []json.RawMessage, reply *[]interface{}) error {
		return o.update(args, reply)
	})
	o.rpcClient.Handle("update2", func(_ *rpc2.Client, args []json.RawMessage, reply *[]interface{}) error {
		return o.update2(args, reply)
	})
	o.rpcClient.Handle("update3", func(_ *rpc2.Client, args []json.RawMessage, reply *[]interface{}) error {
		return o.update3(args, reply)
	})
	go o.rpcClient.Run()
}

// isEndpointLeader returns true if the currently connected endpoint is leader,
// otherwise false or an error. If the currently connected endpoint is the leader
// and the database is clustered, also returns the database's Server ID.
// Assumes rpcMutex is held.
func (o *ovsdbClient) isEndpointLeader(ctx context.Context) (bool, string, error) {
	op := ovsdb.Operation{
		Op:      ovsdb.OperationSelect,
		Table:   "Database",
		Columns: []string{"name", "model", "leader", "sid"},
	}
	results, err := o.transact(ctx, serverDB, true, op)
	if err != nil {
		return false, "", fmt.Errorf("could not check if server was leader: %w", err)
	}
	// for now, if no rows are returned, just accept this server
	if len(results) != 1 {
		return true, "", nil
	}
	result := results[0]
	if len(result.Rows) == 0 {
		return true, "", nil
	}

	for _, row := range result.Rows {
		dbName, ok := row["name"].(string)
		if !ok {
			return false, "", fmt.Errorf("could not parse name")
		}
		if dbName != o.primaryDBName {
			continue
		}

		model, ok := row["model"].(string)
		if !ok {
			return false, "", fmt.Errorf("could not parse model")
		}

		// the database reports whether or not it is part of a cluster via the
		// "model" column. If it's not clustered, it is by definition leader.
		if model != serverdb.DatabaseModelClustered {
			return true, "", nil
		}

		// Clustered database must have a Server ID
		sid, ok := row["sid"].(ovsdb.UUID)
		if !ok {
			return false, "", fmt.Errorf("could not parse server id")
		}

		leader, ok := row["leader"].(bool)
		if !ok {
			return false, "", fmt.Errorf("could not parse leader")
		}

		return leader, sid.GoUUID, nil
	}

	// Extremely unlikely: there is no _Server row for the desired DB (which we made sure existed)
	// for now, just continue
	o.logger.V(3).Info("Couldn't find a row in _Server for our database. Continuing without leader detection", "database", o.primaryDBName)
	return true, "", nil
}

func (o *ovsdbClient) primaryDB() *database {
	return o.databases[o.primaryDBName]
}

// Schema returns the DatabaseSchema that is being used by the client
// it will be nil until a connection has been established
func (o *ovsdbClient) Schema() ovsdb.DatabaseSchema {
	db := o.primaryDB()
	db.modelMutex.RLock()
	defer db.modelMutex.RUnlock()
	return db.model.Schema
}

// Cache returns the TableCache that is populated from
// ovsdb update notifications. It will be nil until a connection
// has been established, and empty unless you call Monitor
func (o *ovsdbClient) Cache() *cache.TableCache {
	db := o.primaryDB()
	db.cacheMutex.RLock()
	defer db.cacheMutex.RUnlock()
	return db.cache
}

// UpdateEndpoints sets client endpoints
// It is intended to be called at runtime
func (o *ovsdbClient) UpdateEndpoints(endpoints []string) {
	o.logger.V(3).Info("update endpoints", "endpoints", endpoints)
	o.rpcMutex.Lock()
	defer o.rpcMutex.Unlock()
	if len(endpoints) == 0 {
		endpoints = []string{defaultUnixEndpoint}
	}
	o.options.endpoints = endpoints
	originEps := o.endpoints[:]
	var newEps []*epInfo
	activeIdx := -1
	for i, address := range o.options.endpoints {
		var serverID string
		for j, origin := range originEps {
			if address == origin.address {
				if j == 0 {
					activeIdx = i
				}
				serverID = origin.serverID
				break
			}
		}
		newEps = append(newEps, &epInfo{address: address, serverID: serverID})
	}
	o.endpoints = newEps
	if activeIdx > 0 {
		o.moveEndpointFirst(activeIdx)
	} else if activeIdx == -1 {
		o._disconnect()
	}
}

// SetOption sets a new value for an option.
// It may only be called when the client is not connected
func (o *ovsdbClient) SetOption(opt Option) error {
	o.rpcMutex.RLock()
	defer o.rpcMutex.RUnlock()
	if o.rpcClient != nil {
		return fmt.Errorf("cannot set option when client is connected")
	}
	return opt(o.options)
}

// Connected returns whether or not the client is currently connected to the server
func (o *ovsdbClient) Connected() bool {
	o.rpcMutex.RLock()
	defer o.rpcMutex.RUnlock()
	return o.connected
}

func (o *ovsdbClient) CurrentEndpoint() string {
	o.rpcMutex.RLock()
	defer o.rpcMutex.RUnlock()
	if o.rpcClient == nil {
		return ""
	}
	return o.endpoints[0].address
}

// DisconnectNotify returns a channel which will notify the caller when the
// server has disconnected
func (o *ovsdbClient) DisconnectNotify() chan struct{} {
	return o.disconnect
}

// RFC 7047 : Section 4.1.6 : Echo
func (o *ovsdbClient) echo(args []interface{}, reply *[]interface{}) error {
	*reply = args
	return nil
}

// RFC 7047 : Update Notification Section 4.1.6
// params is an array of length 2: [json-value, table-updates]
// - json-value: the arbitrary json-value passed when creating the Monitor, i.e. the "cookie"
// - table-updates: map of table name to table-update. Table-update is a map of uuid to (old, new) row paris
func (o *ovsdbClient) update(params []json.RawMessage, reply *[]interface{}) error {
	cookie := MonitorCookie{}
	*reply = []interface{}{}
	if len(params) > 2 {
		return fmt.Errorf("update requires exactly 2 args")
	}
	err := json.Unmarshal(params[0], &cookie)
	if err != nil {
		return err
	}
	var updates ovsdb.TableUpdates
	err = json.Unmarshal(params[1], &updates)
	if err != nil {
		return err
	}
	db := o.databases[cookie.DatabaseName]
	if db == nil {
		return fmt.Errorf("update: invalid database name: %s unknown", cookie.DatabaseName)
	}
	o.metrics.numUpdates.WithLabelValues(cookie.DatabaseName).Inc()
	for tableName := range updates {
		o.metrics.numTableUpdates.WithLabelValues(cookie.DatabaseName, tableName).Inc()
	}

	db.cacheMutex.Lock()
	if db.deferUpdates {
		db.deferredUpdates = append(db.deferredUpdates, &bufferedUpdate{&updates, nil, ""})
		db.cacheMutex.Unlock()
		return nil
	}
	db.cacheMutex.Unlock()

	// Update the local DB cache with the tableUpdates
	db.cacheMutex.RLock()
	err = db.cache.Update(cookie.ID, updates)
	db.cacheMutex.RUnlock()

	if err != nil {
		o.errorCh <- err
	}

	return err
}

// update2 handling from ovsdb-server.7
func (o *ovsdbClient) update2(params []json.RawMessage, reply *[]interface{}) error {
	cookie := MonitorCookie{}
	*reply = []interface{}{}
	if len(params) > 2 {
		return fmt.Errorf("update2 requires exactly 2 args")
	}
	err := json.Unmarshal(params[0], &cookie)
	if err != nil {
		return err
	}
	var updates ovsdb.TableUpdates2
	err = json.Unmarshal(params[1], &updates)
	if err != nil {
		return err
	}
	db := o.databases[cookie.DatabaseName]
	if db == nil {
		return fmt.Errorf("update: invalid database name: %s unknown", cookie.DatabaseName)
	}

	db.cacheMutex.Lock()
	if db.deferUpdates {
		db.deferredUpdates = append(db.deferredUpdates, &bufferedUpdate{nil, &updates, ""})
		db.cacheMutex.Unlock()
		return nil
	}
	db.cacheMutex.Unlock()

	// Update the local DB cache with the tableUpdates
	db.cacheMutex.RLock()
	err = db.cache.Update2(cookie, updates)
	db.cacheMutex.RUnlock()

	if err != nil {
		o.errorCh <- err
	}

	return err
}

// update3 handling from ovsdb-server.7
func (o *ovsdbClient) update3(params []json.RawMessage, reply *[]interface{}) error {
	cookie := MonitorCookie{}
	*reply = []interface{}{}
	if len(params) > 3 {
		return fmt.Errorf("update requires exactly 3 args")
	}
	err := json.Unmarshal(params[0], &cookie)
	if err != nil {
		return err
	}
	var lastTransactionID string
	err = json.Unmarshal(params[1], &lastTransactionID)
	if err != nil {
		return err
	}
	var updates ovsdb.TableUpdates2
	err = json.Unmarshal(params[2], &updates)
	if err != nil {
		return err
	}

	db := o.databases[cookie.DatabaseName]
	if db == nil {
		return fmt.Errorf("update: invalid database name: %s unknown", cookie.DatabaseName)
	}

	db.cacheMutex.Lock()
	if db.deferUpdates {
		db.deferredUpdates = append(db.deferredUpdates, &bufferedUpdate{nil, &updates, lastTransactionID})
		db.cacheMutex.Unlock()
		return nil
	}
	db.cacheMutex.Unlock()

	// Update the local DB cache with the tableUpdates
	db.cacheMutex.RLock()
	err = db.cache.Update2(cookie, updates)
	db.cacheMutex.RUnlock()

	if err == nil {
		db.monitorsMutex.Lock()
		mon := db.monitors[cookie.ID]
		mon.LastTransactionID = lastTransactionID
		db.monitorsMutex.Unlock()
	}

	return err
}

// getSchema returns the schema in use for the provided database name
// RFC 7047 : get_schema
// Should only be called when mutex is held
func (o *ovsdbClient) getSchema(ctx context.Context, dbName string) (ovsdb.DatabaseSchema, error) {
	args := ovsdb.NewGetSchemaArgs(dbName)
	var reply ovsdb.DatabaseSchema
	err := o.rpcClient.CallWithContext(ctx, "get_schema", args, &reply)
	if err != nil {
		if err == rpc2.ErrShutdown {
			return ovsdb.DatabaseSchema{}, ErrNotConnected
		}
		return ovsdb.DatabaseSchema{}, err
	}
	return reply, err
}

// listDbs returns the list of databases on the server
// RFC 7047 : list_dbs
// Should only be called when mutex is held
func (o *ovsdbClient) listDbs(ctx context.Context) ([]string, error) {
	var dbs []string
	err := o.rpcClient.CallWithContext(ctx, "list_dbs", nil, &dbs)
	if err != nil {
		if err == rpc2.ErrShutdown {
			return nil, ErrNotConnected
		}
		return nil, fmt.Errorf("listdbs failure - %v", err)
	}
	return dbs, err
}

// logFromContext returns a Logger from ctx or return the default logger
func (o *ovsdbClient) logFromContext(ctx context.Context) *logr.Logger {
	if logger, err := logr.FromContext(ctx); err == nil {
		return &logger
	}
	return o.logger
}

// Transact performs the provided Operations on the database
// RFC 7047 : transact
func (o *ovsdbClient) Transact(ctx context.Context, operation ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	logger := o.logFromContext(ctx)
	o.rpcMutex.RLock()
	if o.rpcClient == nil || !o.connected {
		o.rpcMutex.RUnlock()
		if o.options.reconnect {
			logger.V(5).Info("blocking transaction until reconnected", "operations",
				fmt.Sprintf("%+v", operation))
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
	return o.transact(ctx, o.primaryDBName, false, operation...)
}

func (o *ovsdbClient) transact(ctx context.Context, dbName string, skipChWrite bool, operation ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	logger := o.logFromContext(ctx)
	var reply []ovsdb.OperationResult
	db := o.databases[dbName]
	db.modelMutex.RLock()
	schema := o.databases[dbName].model.Schema
	db.modelMutex.RUnlock()
	if reflect.DeepEqual(schema, ovsdb.DatabaseSchema{}) {
		return nil, fmt.Errorf("cannot transact to database %s: schema unknown", dbName)
	}
	if ok := schema.ValidateOperations(operation...); !ok {
		return nil, fmt.Errorf("validation failed for the operation")
	}

	args := ovsdb.NewTransactArgs(dbName, operation...)
	if o.rpcClient == nil {
		return nil, ErrNotConnected
	}
	dbgLogger := logger.WithValues("database", dbName).V(4)
	if dbgLogger.Enabled() {
		dbgLogger.Info("transacting operations", "operations", fmt.Sprintf("%+v", operation))
	}
	err := o.rpcClient.CallWithContext(ctx, "transact", args, &reply)
	if err != nil {
		if err == rpc2.ErrShutdown {
			return nil, ErrNotConnected
		}
		return nil, err
	}

	if !skipChWrite && o.trafficSeen != nil {
		o.trafficSeen <- struct{}{}
	}
	return reply, nil
}

// MonitorAll is a convenience method to monitor every table/column
func (o *ovsdbClient) MonitorAll(ctx context.Context) (MonitorCookie, error) {
	m := newMonitor()
	for name := range o.primaryDB().model.Types() {
		m.Tables = append(m.Tables, TableMonitor{Table: name})
	}
	return o.Monitor(ctx, m)
}

// MonitorCancel will request cancel a previously issued monitor request
// RFC 7047 : monitor_cancel
func (o *ovsdbClient) MonitorCancel(ctx context.Context, cookie MonitorCookie) error {
	var reply ovsdb.OperationResult
	args := ovsdb.NewMonitorCancelArgs(cookie)
	o.rpcMutex.Lock()
	defer o.rpcMutex.Unlock()
	if o.rpcClient == nil {
		return ErrNotConnected
	}
	err := o.rpcClient.CallWithContext(ctx, "monitor_cancel", args, &reply)
	if err != nil {
		if err == rpc2.ErrShutdown {
			return ErrNotConnected
		}
		return err
	}
	if reply.Error != "" {
		return fmt.Errorf("error while executing transaction: %s", reply.Error)
	}
	o.primaryDB().monitorsMutex.Lock()
	defer o.primaryDB().monitorsMutex.Unlock()
	delete(o.primaryDB().monitors, cookie.ID)
	o.metrics.numMonitors.Dec()
	return nil
}

// Monitor will provide updates for a given table/column
// and populate the cache with them. Subsequent updates will be processed
// by the Update Notifications
// RFC 7047 : monitor
func (o *ovsdbClient) Monitor(ctx context.Context, monitor *Monitor) (MonitorCookie, error) {
	cookie := newMonitorCookie(o.primaryDBName)
	db := o.databases[o.primaryDBName]
	db.monitorsMutex.Lock()
	defer db.monitorsMutex.Unlock()
	return cookie, o.monitor(ctx, cookie, false, monitor)
}

// If fields is provided, the request will be constrained to the provided columns
// If no fields are provided, all columns will be used
func newMonitorRequest(data *mapper.Info, fields []string, conditions []ovsdb.Condition) (*ovsdb.MonitorRequest, error) {
	var columns []string
	if len(fields) > 0 {
		columns = append(columns, fields...)
	} else {
		for c := range data.Metadata.TableSchema.Columns {
			columns = append(columns, c)
		}
	}
	return &ovsdb.MonitorRequest{Columns: columns, Where: conditions, Select: ovsdb.NewDefaultMonitorSelect()}, nil
}

// monitor must only be called with a lock on monitorsMutex
//
//gocyclo:ignore
func (o *ovsdbClient) monitor(ctx context.Context, cookie MonitorCookie, reconnecting bool, monitor *Monitor) error {
	// if we're reconnecting, we already hold the rpcMutex
	if !reconnecting {
		o.rpcMutex.RLock()
		defer o.rpcMutex.RUnlock()
	}
	if o.rpcClient == nil {
		return ErrNotConnected
	}
	if len(monitor.Errors) != 0 {
		var errString []string
		for _, err := range monitor.Errors {
			errString = append(errString, err.Error())
		}
		return fmt.Errorf(strings.Join(errString, ". "))
	}
	if len(monitor.Tables) == 0 {
		return fmt.Errorf("at least one table should be monitored")
	}
	dbName := cookie.DatabaseName
	db := o.databases[dbName]
	db.modelMutex.RLock()
	typeMap := db.model.Types()
	requests := make(map[string]ovsdb.MonitorRequest)
	for _, o := range monitor.Tables {
		_, ok := typeMap[o.Table]
		if !ok {
			return fmt.Errorf("type for table %s does not exist in model", o.Table)
		}
		model, err := db.model.NewModel(o.Table)
		if err != nil {
			return err
		}
		info, err := db.model.NewModelInfo(model)
		if err != nil {
			return err
		}
		request, err := newMonitorRequest(info, o.Fields, o.Conditions)
		if err != nil {
			return err
		}
		requests[o.Table] = *request
	}
	db.modelMutex.RUnlock()

	var args []interface{}
	if monitor.Method == ovsdb.ConditionalMonitorSinceRPC {
		// If we are reconnecting a CondSince monitor that is the only
		// monitor, then we can use its LastTransactionID since it is
		// valid (because we're reconnecting) and we can safely keep
		// the cache intact (because it's the only monitor).
		transactionID := emptyUUID
		if reconnecting && len(db.monitors) == 1 {
			transactionID = monitor.LastTransactionID
		}
		args = ovsdb.NewMonitorCondSinceArgs(dbName, cookie, requests, transactionID)
	} else {
		args = ovsdb.NewMonitorArgs(dbName, cookie, requests)
	}
	var err error
	var tableUpdates interface{}

	var lastTransactionFound bool
	switch monitor.Method {
	case ovsdb.MonitorRPC:
		var reply ovsdb.TableUpdates
		err = o.rpcClient.CallWithContext(ctx, monitor.Method, args, &reply)
		tableUpdates = reply
	case ovsdb.ConditionalMonitorRPC:
		var reply ovsdb.TableUpdates2
		err = o.rpcClient.CallWithContext(ctx, monitor.Method, args, &reply)
		tableUpdates = reply
	case ovsdb.ConditionalMonitorSinceRPC:
		var reply ovsdb.MonitorCondSinceReply
		err = o.rpcClient.CallWithContext(ctx, monitor.Method, args, &reply)
		if err == nil && reply.Found {
			monitor.LastTransactionID = reply.LastTransactionID
			lastTransactionFound = true
		}
		tableUpdates = reply.Updates
	default:
		return fmt.Errorf("unsupported monitor method: %v", monitor.Method)
	}

	if err != nil {
		if err == rpc2.ErrShutdown {
			return ErrNotConnected
		}
		if err.Error() == "unknown method" {
			if monitor.Method == ovsdb.ConditionalMonitorSinceRPC {
				o.logger.V(3).Error(err, "method monitor_cond_since not supported, falling back to monitor_cond")
				monitor.Method = ovsdb.ConditionalMonitorRPC
				return o.monitor(ctx, cookie, reconnecting, monitor)
			}
			if monitor.Method == ovsdb.ConditionalMonitorRPC {
				o.logger.V(3).Error(err, "method monitor_cond not supported, falling back to monitor")
				monitor.Method = ovsdb.MonitorRPC
				return o.monitor(ctx, cookie, reconnecting, monitor)
			}
		}
		return err
	}

	if !reconnecting {
		db.monitors[cookie.ID] = monitor
		o.metrics.numMonitors.Inc()
	}

	db.cacheMutex.Lock()
	defer db.cacheMutex.Unlock()

	// On reconnect, purge the cache _unless_ the only monitor is a
	// MonitorCondSince one, whose LastTransactionID was known to the
	// server. In this case the reply contains only updates to the existing
	// cache data, while otherwise it includes complete DB data so we must
	// purge to get rid of old rows.
	if reconnecting && (len(db.monitors) > 1 || !lastTransactionFound) {
		db.cache.Purge(db.model)
	}

	if monitor.Method == ovsdb.MonitorRPC {
		u := tableUpdates.(ovsdb.TableUpdates)
		err = db.cache.Populate(u)
	} else {
		u := tableUpdates.(ovsdb.TableUpdates2)
		err = db.cache.Populate2(u)
	}

	if err != nil {
		return err
	}

	// populate any deferred updates
	db.deferUpdates = false
	for _, update := range db.deferredUpdates {
		if update.updates != nil {
			if err = db.cache.Populate(*update.updates); err != nil {
				return err
			}
		}

		if update.updates2 != nil {
			if err = db.cache.Populate2(*update.updates2); err != nil {
				return err
			}
		}
		if len(update.lastTxnID) > 0 {
			db.monitors[cookie.ID].LastTransactionID = update.lastTxnID
		}
	}
	// clear deferred updates for next time
	db.deferredUpdates = make([]*bufferedUpdate, 0)

	return err
}

// Echo tests the liveness of the OVSDB connetion
func (o *ovsdbClient) Echo(ctx context.Context) error {
	args := ovsdb.NewEchoArgs()
	var reply []interface{}
	o.rpcMutex.RLock()
	defer o.rpcMutex.RUnlock()
	if o.rpcClient == nil {
		return ErrNotConnected
	}
	err := o.rpcClient.CallWithContext(ctx, "echo", args, &reply)
	if err != nil {
		if err == rpc2.ErrShutdown {
			return ErrNotConnected
		}
	}
	if !reflect.DeepEqual(args, reply) {
		return fmt.Errorf("incorrect server response: %v, %v", args, reply)
	}
	return nil
}

// watchForLeaderChange will trigger a reconnect if the connected endpoint
// ever loses leadership
func (o *ovsdbClient) watchForLeaderChange() error {
	updates := make(chan model.Model)
	o.databases[serverDB].cache.AddEventHandler(&cache.EventHandlerFuncs{
		UpdateFunc: func(table string, _, new model.Model) {
			if table == "Database" {
				updates <- new
			}
		},
	})

	m := newMonitor()
	// NOTE: _Server does not support monitor_cond_since
	m.Method = ovsdb.ConditionalMonitorRPC
	m.Tables = []TableMonitor{{Table: "Database"}}
	db := o.databases[serverDB]
	db.monitorsMutex.Lock()
	defer db.monitorsMutex.Unlock()
	err := o.monitor(context.Background(), newMonitorCookie(serverDB), false, m)
	if err != nil {
		return err
	}

	go func() {
		for m := range updates {
			dbInfo, ok := m.(*serverdb.Database)
			if !ok {
				continue
			}

			// Ignore the dbInfo for _Server
			if dbInfo.Name != o.primaryDBName {
				continue
			}

			// Only handle leadership changes for clustered databases
			if dbInfo.Model != serverdb.DatabaseModelClustered {
				continue
			}

			// Clustered database servers must have a valid Server ID
			var sid string
			if dbInfo.Sid != nil {
				sid = *dbInfo.Sid
			}
			if sid == "" {
				o.logger.V(3).Info("clustered database update contained invalid server ID")
				continue
			}

			o.rpcMutex.Lock()
			if !dbInfo.Leader && o.connected {
				activeEndpoint := o.endpoints[0]
				if sid == activeEndpoint.serverID {
					o.logger.V(3).Info("endpoint lost leader, reconnecting",
						"endpoint", activeEndpoint.address, "sid", sid)
					// don't immediately reconnect to the active endpoint since it's no longer leader
					o.moveEndpointLast(0)
					o._disconnect()
				} else {
					o.logger.V(3).Info("endpoint lost leader but had unexpected server ID",
						"endpoint", activeEndpoint.address,
						"expected", activeEndpoint.serverID, "found", sid)
				}
			}
			o.rpcMutex.Unlock()
		}
	}()
	return nil
}

func (o *ovsdbClient) handleClientErrors(stopCh <-chan struct{}) {
	defer o.handlerShutdown.Done()
	var errColumnNotFound *mapper.ErrColumnNotFound
	var errCacheInconsistent *cache.ErrCacheInconsistent
	var errIndexExists *cache.ErrIndexExists
	for {
		select {
		case <-stopCh:
			return
		case err := <-o.errorCh:
			if errors.As(err, &errColumnNotFound) {
				o.logger.V(3).Error(err, "error updating cache, DB schema may be newer than client!")
			} else if errors.As(err, &errCacheInconsistent) || errors.As(err, &errIndexExists) {
				// trigger a reconnect, which will purge the cache
				// hopefully a rebuild will fix any inconsistency
				o.logger.V(3).Error(err, "triggering reconnect to rebuild cache")
				// for rebuilding cache with mon_cond_since (not yet fully supported in libovsdb) we
				// need to reset the last txn ID
				for _, db := range o.databases {
					db.monitorsMutex.Lock()
					for _, mon := range db.monitors {
						mon.LastTransactionID = emptyUUID
					}
					db.monitorsMutex.Unlock()
				}
				o.Disconnect()
			} else {
				o.logger.V(3).Error(err, "error updating cache")
			}
		}
	}
}

func (o *ovsdbClient) sendEcho(args []interface{}, reply *[]interface{}) *rpc2.Call {
	o.rpcMutex.RLock()
	defer o.rpcMutex.RUnlock()
	if o.rpcClient == nil {
		return nil
	}
	return o.rpcClient.Go("echo", args, reply, make(chan *rpc2.Call, 1))
}

func (o *ovsdbClient) handleInactivityProbes() {
	defer o.handlerShutdown.Done()
	echoReplied := make(chan string)
	var lastEcho string
	stopCh := o.stopCh
	trafficSeen := o.trafficSeen
	for {
		select {
		case <-stopCh:
			return
		case <-trafficSeen:
			// We got some traffic from the server, restart our timer
		case ts := <-echoReplied:
			// Got a response from the server, check it against lastEcho; if same clear lastEcho; if not same Disconnect()
			if ts != lastEcho {
				o.Disconnect()
				return
			}
			lastEcho = ""
		case <-time.After(o.options.inactivityTimeout):
			// If there's a lastEcho already, then we didn't get a server reply, disconnect
			if lastEcho != "" {
				o.Disconnect()
				return
			}
			// Otherwise send an echo
			thisEcho := fmt.Sprintf("%d", time.Now().UnixMicro())
			args := []interface{}{"libovsdb echo", thisEcho}
			var reply []interface{}
			// Can't use o.Echo() because it blocks; we need the Call object direct from o.rpcClient.Go()
			call := o.sendEcho(args, &reply)
			if call == nil {
				o.Disconnect()
				return
			}
			lastEcho = thisEcho
			go func() {
				// Wait for the echo reply
				select {
				case <-stopCh:
					return
				case <-call.Done:
					if call.Error != nil {
						// RPC timeout; disconnect
						o.logger.V(3).Error(call.Error, "server echo reply error")
						o.Disconnect()
					} else if !reflect.DeepEqual(args, reply) {
						o.logger.V(3).Info("warning: incorrect server echo reply",
							"expected", args, "reply", reply)
						o.Disconnect()
					} else {
						// Otherwise stuff thisEcho into the echoReplied channel
						echoReplied <- thisEcho
					}
				}
			}()
		}
	}
}

func (o *ovsdbClient) handleDisconnectNotification() {
	<-o.rpcClient.DisconnectNotify()
	// close the stopCh, which will stop the cache event processor
	close(o.stopCh)
	if o.trafficSeen != nil {
		close(o.trafficSeen)
	}
	o.metrics.numDisconnects.Inc()
	// wait for client related handlers to shutdown
	o.handlerShutdown.Wait()
	o.rpcMutex.Lock()
	if o.options.reconnect && !o.shutdown {
		o.rpcClient = nil
		o.rpcMutex.Unlock()
		suppressionCounter := 1
		connect := func() error {
			// need to ensure deferredUpdates is cleared on every reconnect attempt
			for _, db := range o.databases {
				db.cacheMutex.Lock()
				db.deferredUpdates = make([]*bufferedUpdate, 0)
				db.deferUpdates = true
				db.cacheMutex.Unlock()
			}
			ctx, cancel := context.WithTimeout(context.Background(), o.options.timeout)
			defer cancel()
			err := o.connect(ctx, true)
			if err != nil {
				if suppressionCounter < 5 {
					o.logger.V(2).Error(err, "failed to reconnect")
				} else if suppressionCounter == 5 {
					o.logger.V(2).Error(err, "reconnect has failed 5 times, suppressing logging "+
						"for future attempts")
				}
			}
			suppressionCounter++
			return err
		}
		o.logger.V(3).Info("connection lost, reconnecting", "endpoint", o.endpoints[0].address)
		err := backoff.Retry(connect, o.options.backoff)
		if err != nil {
			// TODO: We should look at passing this back to the
			// caller to handle
			panic(err)
		}
		// this goroutine finishes, and is replaced with a new one (from Connect)
		return
	}

	// clear connection state
	o.rpcClient = nil
	o.rpcMutex.Unlock()

	for _, db := range o.databases {
		db.cacheMutex.Lock()
		defer db.cacheMutex.Unlock()
		db.cache = nil
		// need to defer updates if/when we reconnect and clear any stale updates
		db.deferUpdates = true
		db.deferredUpdates = make([]*bufferedUpdate, 0)

		db.modelMutex.Lock()
		defer db.modelMutex.Unlock()
		db.model = model.NewPartialDatabaseModel(db.model.Client())

		db.monitorsMutex.Lock()
		defer db.monitorsMutex.Unlock()
		db.monitors = make(map[string]*Monitor)
	}
	o.metrics.numMonitors.Set(0)

	o.shutdownMutex.Lock()
	defer o.shutdownMutex.Unlock()
	o.shutdown = false

	select {
	case o.disconnect <- struct{}{}:
		// sent disconnect notification to client
	default:
		// client is not listening to the channel
	}
}

// _disconnect will close the connection to the OVSDB server
// If the client was created with WithReconnect then the client
// will reconnect afterwards. Assumes rpcMutex is held.
func (o *ovsdbClient) _disconnect() {
	o.connected = false
	if o.rpcClient == nil {
		return
	}
	o.rpcClient.Close()
}

// Disconnect will close the connection to the OVSDB server
// If the client was created with WithReconnect then the client
// will reconnect afterwards
func (o *ovsdbClient) Disconnect() {
	o.rpcMutex.Lock()
	defer o.rpcMutex.Unlock()
	o._disconnect()
}

// Close will close the connection to the OVSDB server
// It will remove all stored state ready for the next connection
// Even If the client was created with WithReconnect it will not reconnect afterwards
func (o *ovsdbClient) Close() {
	o.rpcMutex.Lock()
	defer o.rpcMutex.Unlock()
	o.connected = false
	if o.rpcClient == nil {
		return
	}
	o.shutdownMutex.Lock()
	defer o.shutdownMutex.Unlock()
	o.shutdown = true
	o.rpcClient.Close()
}

// Ensures the cache is consistent by evaluating that the client is connected
// and the monitor is fully setup, with the cache populated. Caller must hold
// the database's cache mutex for reading.
func isCacheConsistent(db *database) bool {
	// This works because when a client is disconnected the deferUpdates variable
	// will be set to true. deferUpdates is also protected by the db.cacheMutex.
	// When the client reconnects and then re-establishes the monitor; the final step
	// is to process all deferred updates, set deferUpdates back to false, and unlock cacheMutex
	return !db.deferUpdates
}

// best effort to ensure cache is in a good state for reading. RLocks the
// database's cache before returning; caller must always unlock.
func waitForCacheConsistent(ctx context.Context, db *database, logger *logr.Logger, dbName string) {
	if !hasMonitors(db) {
		db.cacheMutex.RLock()
		return
	}
	// Check immediately as a fastpath
	db.cacheMutex.RLock()
	if isCacheConsistent(db) {
		return
	}
	db.cacheMutex.RUnlock()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.V(3).Info("warning: unable to ensure cache consistency for reading",
				"database", dbName)
			db.cacheMutex.RLock()
			return
		case <-ticker.C:
			db.cacheMutex.RLock()
			if isCacheConsistent(db) {
				return
			}
			db.cacheMutex.RUnlock()
		}
	}
}

func hasMonitors(db *database) bool {
	db.monitorsMutex.Lock()
	defer db.monitorsMutex.Unlock()
	return len(db.monitors) > 0
}

// Client API interface wrapper functions
// We add this wrapper to allow users to access the API directly on the
// client object

// Get implements the API interface's Get function
func (o *ovsdbClient) Get(ctx context.Context, model model.Model) error {
	primaryDB := o.primaryDB()
	waitForCacheConsistent(ctx, primaryDB, o.logger, o.primaryDBName)
	defer primaryDB.cacheMutex.RUnlock()
	return primaryDB.api.Get(ctx, model)
}

// Create implements the API interface's Create function
func (o *ovsdbClient) Create(models ...model.Model) ([]ovsdb.Operation, error) {
	return o.primaryDB().api.Create(models...)
}

// List implements the API interface's List function
func (o *ovsdbClient) List(ctx context.Context, result interface{}) error {
	primaryDB := o.primaryDB()
	waitForCacheConsistent(ctx, primaryDB, o.logger, o.primaryDBName)
	defer primaryDB.cacheMutex.RUnlock()
	return primaryDB.api.List(ctx, result)
}

// Where implements the API interface's Where function
func (o *ovsdbClient) Where(models ...model.Model) ConditionalAPI {
	return o.primaryDB().api.Where(models...)
}

// WhereAny implements the API interface's WhereAny function
func (o *ovsdbClient) WhereAny(m model.Model, conditions ...model.Condition) ConditionalAPI {
	return o.primaryDB().api.WhereAny(m, conditions...)
}

// WhereAll implements the API interface's WhereAll function
func (o *ovsdbClient) WhereAll(m model.Model, conditions ...model.Condition) ConditionalAPI {
	return o.primaryDB().api.WhereAll(m, conditions...)
}

// WhereCache implements the API interface's WhereCache function
func (o *ovsdbClient) WhereCache(predicate interface{}) ConditionalAPI {
	return o.primaryDB().api.WhereCache(predicate)
}

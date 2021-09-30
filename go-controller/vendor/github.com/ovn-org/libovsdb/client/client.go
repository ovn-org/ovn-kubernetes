package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strings"
	"sync"

	"github.com/cenkalti/rpc2"
	"github.com/cenkalti/rpc2/jsonrpc"
	"github.com/ovn-org/libovsdb/cache"
	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// Constants defined for libovsdb
const (
	SSL  = "ssl"
	TCP  = "tcp"
	UNIX = "unix"
)

// ErrNotConnected is an error returned when the client is not connected
var ErrNotConnected = errors.New("not connected")

// Client represents an OVSDB Client Connection
// It provides all the necessary functionality to Connect to a server,
// perform transactions, and build your own replica of the database with
// Monitor or MonitorAll. It also provides a Cache that is populated from OVSDB
// update notifications.
type Client interface {
	Connect(context.Context) error
	Disconnect()
	Schema() *ovsdb.DatabaseSchema
	Cache() *cache.TableCache
	SetOption(Option) error
	Connected() bool
	DisconnectNotify() chan struct{}
	Echo() error
	Transact(...ovsdb.Operation) ([]ovsdb.OperationResult, error)
	Monitor(jsonContext interface{}, t ...TableMonitor) error
	MonitorAll(jsonContext interface{}) error
	MonitorCancel(jsonContext interface{}) error
	NewTableMonitor(m model.Model, fields ...interface{}) TableMonitor
	API
}

// ovsdbClient is an OVSDB client
type ovsdbClient struct {
	options    *options
	rpcClient  *rpc2.Client
	dbModel    *model.DBModel
	schema     *ovsdb.DatabaseSchema
	cache      *cache.TableCache
	stopCh     chan struct{}
	connected  bool
	disconnect chan struct{}
	api        API
	mutex      sync.Mutex
}

// NewOVSDBClient creates a new OVSDB Client with the provided
// database model. The client can be configured using one or more Option(s),
// like WithTLSConfig. If no WithEndpoint option is supplied, the default of
// unix:/var/run/openvswitch/ovsdb.sock is used
func NewOVSDBClient(databaseModel *model.DBModel, opts ...Option) (Client, error) {
	return newOVSDBClient(databaseModel, opts...)
}

// newOVSDBClient creates a new ovsdbClient
func newOVSDBClient(databaseModel *model.DBModel, opts ...Option) (*ovsdbClient, error) {
	ovs := &ovsdbClient{
		dbModel:    databaseModel,
		disconnect: make(chan struct{}),
	}
	var err error
	ovs.options, err = newOptions(opts...)
	if err != nil {
		return nil, err
	}
	return ovs, nil
}

// Connect opens a connection to an OVSDB Server using the
// endpoint provided when the Client was created.
// The connection can be configured using one or more Option(s), like WithTLSConfig
// If no WithEndpoint option is supplied, the default of unix:/var/run/openvswitch/ovsdb.sock is used
func (o *ovsdbClient) Connect(ctx context.Context) error {
	if o.connected {
		return nil
	}
	o.mutex.Lock()
	defer o.mutex.Unlock()
	var c net.Conn
	var dialer net.Dialer
	var err error
	var u *url.URL
	connected := false
	for _, endpoint := range o.options.endpoints {
		if u, err = url.Parse(endpoint); err != nil {
			return err
		}
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
		if err == nil {
			connected = true
			break
		}
	}
	if !connected {
		// FIXME: This only emits the error from the last attempted connection
		return fmt.Errorf("failed to connect to endpoints %q: %v", o.options.endpoints, err)
	}
	if err := o.createRPC2Client(c); err != nil {
		return err
	}
	o.connected = true
	return nil
}

// createRPC2Client creates an rpcClient using the provided connection
// It is also responsible for setting up go routines for handling disconnect notification
// and cache population
func (o *ovsdbClient) createRPC2Client(conn net.Conn) error {
	o.stopCh = make(chan struct{})
	o.rpcClient = rpc2.NewClientWithCodec(jsonrpc.NewJSONCodec(conn))
	o.rpcClient.SetBlocking(true)
	o.rpcClient.Handle("echo", func(_ *rpc2.Client, args []interface{}, reply *[]interface{}) error {
		return o.echo(args, reply)
	})
	o.rpcClient.Handle("update", func(_ *rpc2.Client, args []json.RawMessage, reply *[]interface{}) error {
		return o.update(args, reply)
	})
	go o.rpcClient.Run()

	dbs, err := o.listDbs()
	if err != nil {
		o.rpcClient.Close()
		return err
	}

	found := false
	for _, db := range dbs {
		if db == o.dbModel.Name() {
			found = true
			break
		}
	}
	if !found {
		o.rpcClient.Close()
		return fmt.Errorf("target database not found")
	}

	schema, err := o.getSchema(o.dbModel.Name())
	errors := o.dbModel.Validate(schema)
	if len(errors) > 0 {
		var combined []string
		for _, err := range errors {
			combined = append(combined, err.Error())
		}
		return fmt.Errorf("database validation error (%d): %s", len(errors),
			strings.Join(combined, ". "))
	}

	if err == nil {
		o.schema = schema
		if cache, err := cache.NewTableCache(schema, o.dbModel, nil); err == nil {
			o.cache = cache
			o.api = newAPI(o.cache)
		} else {
			o.rpcClient.Close()
			return err
		}
	} else {
		o.rpcClient.Close()
		return err
	}

	go o.cache.Run(o.stopCh)
	go o.handleDisconnectNotification()

	return nil
}

// Schema returns the DatabaseSchema that is being used by the client
// it will be nil until a connection has been established
func (o *ovsdbClient) Schema() *ovsdb.DatabaseSchema {
	return o.schema
}

// Cache returns the TableCache that is populated from
// ovsdb update notifications. It will be nil until a connection
// has been established, and empty unless you call Monitor
func (o *ovsdbClient) Cache() *cache.TableCache {
	return o.cache
}

// SetOption sets a new value for an option.
// It may only be called when the client is not connected
func (o *ovsdbClient) SetOption(opt Option) error {
	if o.connected {
		return fmt.Errorf("cannot set option when client is connected")
	}
	return opt(o.options)
}

// Connected returns whether or not the client is currently connected to the server
func (o *ovsdbClient) Connected() bool {
	return o.connected
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
func (o *ovsdbClient) update(args []json.RawMessage, reply *[]interface{}) error {
	var value string
	if len(args) > 2 {
		return fmt.Errorf("update requires exactly 2 args")
	}
	err := json.Unmarshal(args[0], &value)
	if err != nil {
		return err
	}
	var updates ovsdb.TableUpdates
	err = json.Unmarshal(args[1], &updates)
	if err != nil {
		return err
	}
	// Update the local DB cache with the tableUpdates
	o.cache.Update(value, updates)
	*reply = []interface{}{}
	return nil
}

// getSchema returns the schema in use for the provided database name
// RFC 7047 : get_schema
func (o *ovsdbClient) getSchema(dbName string) (*ovsdb.DatabaseSchema, error) {
	args := ovsdb.NewGetSchemaArgs(dbName)
	var reply ovsdb.DatabaseSchema
	err := o.rpcClient.Call("get_schema", args, &reply)
	if err != nil {
		return nil, err
	}
	return &reply, err
}

// listDbs returns the list of databases on the server
// RFC 7047 : list_dbs
func (o *ovsdbClient) listDbs() ([]string, error) {
	var dbs []string
	err := o.rpcClient.Call("list_dbs", nil, &dbs)
	if err != nil {
		return nil, fmt.Errorf("listdbs failure - %v", err)
	}
	return dbs, err
}

// Transact performs the provided Operations on the database
// RFC 7047 : transact
func (o *ovsdbClient) Transact(operation ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	if !o.connected {
		return nil, ErrNotConnected
	}
	var reply []ovsdb.OperationResult

	if ok := o.schema.ValidateOperations(operation...); !ok {
		return nil, fmt.Errorf("validation failed for the operation")
	}

	args := ovsdb.NewTransactArgs(o.schema.Name, operation...)
	err := o.rpcClient.Call("transact", args, &reply)
	if err != nil {
		return nil, err
	}
	return reply, nil
}

// MonitorAll is a convenience method to monitor every table/column
func (o *ovsdbClient) MonitorAll(jsonContext interface{}) error {
	if !o.connected {
		return ErrNotConnected
	}
	var options []TableMonitor
	for name := range o.dbModel.Types() {
		options = append(options, TableMonitor{Table: name})
	}
	return o.Monitor(jsonContext, options...)
}

// MonitorCancel will request cancel a previously issued monitor request
// RFC 7047 : monitor_cancel
func (o *ovsdbClient) MonitorCancel(jsonContext interface{}) error {
	if !o.connected {
		return ErrNotConnected
	}
	var reply ovsdb.OperationResult

	args := ovsdb.NewMonitorCancelArgs(jsonContext)

	err := o.rpcClient.Call("monitor_cancel", args, &reply)
	if err != nil {
		return err
	}
	if reply.Error != "" {
		return fmt.Errorf("error while executing transaction: %s", reply.Error)
	}
	return nil
}

// TableMonitor is a table to be monitored
type TableMonitor struct {
	// Table is the table to be monitored
	Table string
	// Fields are the fields in the model to monitor
	// If none are supplied, all fields will be used
	Fields []interface{}
	// Error will contain any errors caught in the creation of a TableMonitor
	Error error
}

func (o *ovsdbClient) NewTableMonitor(m model.Model, fields ...interface{}) TableMonitor {
	tableName := o.dbModel.FindTable(reflect.TypeOf(m))
	if tableName == "" {
		return TableMonitor{
			Error: fmt.Errorf("object of type %s is not part of the DBModel", reflect.TypeOf(m)),
		}
	}
	return TableMonitor{
		Table:  tableName,
		Fields: fields,
	}
}

// Monitor will provide updates for a given table/column
// and populate the cache with them. Subsequent updates will be processed
// by the Update Notifications
// RFC 7047 : monitor
func (o *ovsdbClient) Monitor(jsonContext interface{}, options ...TableMonitor) error {
	if !o.connected {
		return ErrNotConnected
	}
	if len(options) == 0 {
		return fmt.Errorf("no monitor options provided")
	}
	var reply ovsdb.TableUpdates
	mapper := mapper.NewMapper(o.schema)
	typeMap := o.dbModel.Types()
	requests := make(map[string]ovsdb.MonitorRequest)
	for _, o := range options {
		if o.Error != nil {
			return o.Error
		}
		m, ok := typeMap[o.Table]
		if !ok {
			return fmt.Errorf("type for table %s does not exist in dbModel", o.Table)
		}
		request, err := mapper.NewMonitorRequest(o.Table, m, o.Fields)
		if err != nil {
			return err
		}
		requests[o.Table] = *request
	}
	args := ovsdb.NewMonitorArgs(o.schema.Name, jsonContext, requests)
	err := o.rpcClient.Call("monitor", args, &reply)
	if err != nil {
		return err
	}
	o.cache.Populate(reply)
	return nil
}

// Echo tests the liveness of the OVSDB connetion
func (o *ovsdbClient) Echo() error {
	if !o.connected {
		return ErrNotConnected
	}
	args := ovsdb.NewEchoArgs()
	var reply []interface{}
	err := o.rpcClient.Call("echo", args, &reply)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(args, reply) {
		return fmt.Errorf("incorrect server response: %v, %v", args, reply)
	}
	return nil
}

func (o *ovsdbClient) handleDisconnectNotification() {
	// this will block until Connect() has released the lock via defer
	o.mutex.Lock()
	// we continue to hold the lock until the client has disconnected
	// this prevents another call to Connect() changing the rpcClient
	// while we're still listening for disconnects
	defer o.mutex.Unlock()
	<-o.rpcClient.DisconnectNotify()
	close(o.stopCh)
	o.rpcClient = nil
	o.cache = nil
	select {
	case o.disconnect <- struct{}{}:
		// sent disconnect notification to client
	default:
		// client is not listening to the channel
	}
}

// Disconnect will close the connection to the OVSDB server
func (o *ovsdbClient) Disconnect() {
	if !o.connected {
		return
	}
	o.connected = false
	o.rpcClient.Close()
}

// Client API interface wrapper functions
// We add this wrapper to allow users to access the API directly on the
// client object

//Get implements the API interface's Get function
func (o *ovsdbClient) Get(model model.Model) error {
	return o.api.Get(model)
}

//Create implements the API interface's Create function
func (o *ovsdbClient) Create(models ...model.Model) ([]ovsdb.Operation, error) {
	return o.api.Create(models...)
}

//List implements the API interface's List function
func (o *ovsdbClient) List(result interface{}) error {
	return o.api.List(result)
}

//Where implements the API interface's Where function
func (o *ovsdbClient) Where(m model.Model, conditions ...model.Condition) ConditionalAPI {
	return o.api.Where(m, conditions...)
}

//WhereAll implements the API interface's WhereAll function
func (o *ovsdbClient) WhereAll(m model.Model, conditions ...model.Condition) ConditionalAPI {
	return o.api.WhereAll(m, conditions...)
}

//WhereCache implements the API interface's WhereCache function
func (o *ovsdbClient) WhereCache(predicate interface{}) ConditionalAPI {
	return o.api.WhereCache(predicate)
}

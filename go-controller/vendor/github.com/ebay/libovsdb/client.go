package libovsdb

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/rpc2"
	"github.com/cenkalti/rpc2/jsonrpc"
)

// OvsdbClient is an OVSDB client
type OvsdbClient struct {
	rpcClient     *rpc2.Client
	Schema        map[string]DatabaseSchema
	handlers      []NotificationHandler
	handlersMutex *sync.Mutex
	timeout       time.Duration
}

func newOvsdbClient(timeout time.Duration, c *rpc2.Client) *OvsdbClient {
	ovs := &OvsdbClient{
		rpcClient:     c,
		Schema:        make(map[string]DatabaseSchema),
		handlersMutex: &sync.Mutex{},
		timeout:       timeout,
	}
	connectionsMutex.Lock()
	defer connectionsMutex.Unlock()
	if connections == nil {
		connections = make(map[*rpc2.Client]*OvsdbClient)
	}
	connections[c] = ovs
	return ovs
}

// Would rather replace this connection map with an OvsdbClient Receiver scoped method
// Unfortunately rpc2 package acts wierd with a receiver scoped method and needs some investigation.
var (
	connections      map[*rpc2.Client]*OvsdbClient
	connectionsMutex = &sync.RWMutex{}
)

// Constants defined for libovsdb
const (
	defaultTCPAddress  = "127.0.0.1:6640"
	defaultUnixAddress = "/var/run/openvswitch/ovnnb_db.sock"
	SSL                = "ssl"
	TCP                = "tcp"
	UNIX               = "unix"
)

// Connect to ovn, using endpoint in format ovsdb Connection Methods
// If address is empty, use default address for specified protocol
func Connect(timeout time.Duration, endpoints string, tlsConfig *tls.Config) (*OvsdbClient, error) {
	var c net.Conn
	var err error
	var u *url.URL

	if timeout == 0 {
		timeout = 1 * time.Minute
	}

	var dialer net.Dialer
	for _, endpoint := range strings.Split(endpoints, ",") {
		if u, err = url.Parse(endpoint); err != nil {
			return nil, err
		}
		// u.Opaque contains the original endPoint with the leading protocol stripped
		// off. For example: endPoint is "tcp:127.0.0.1:6640" and u.Opaque is "127.0.0.1:6640"
		host := u.Opaque
		if len(host) == 0 {
			host = defaultTCPAddress
		}
		ctx, cancel := context.WithTimeout(context.TODO(), timeout)
		switch u.Scheme {
		case UNIX:
			path := u.Path
			if len(path) == 0 {
				path = defaultUnixAddress
			}
			c, err = dialer.DialContext(ctx, u.Scheme, path)
		case TCP:
			c, err = dialer.DialContext(ctx, u.Scheme, host)
		case SSL:
			dialer := tls.Dialer{
				Config: tlsConfig,
			}
			c, err = dialer.DialContext(ctx, "tcp", host)
		default:
			err = fmt.Errorf("unknown network protocol %s", u.Scheme)
		}
		cancel()

		if err == nil {
			return newRPC2Client(timeout, c)
		}
	}

	return nil, fmt.Errorf("failed to connect: %s", err.Error())
}

func handleMonitorCancel(client *rpc2.Client, params []interface{}, reply *interface{}) error {
	log.Println("monitor cancel received")
	return client.Close()
}

func newRPC2Client(timeout time.Duration, conn net.Conn) (*OvsdbClient, error) {
	c := rpc2.NewClientWithCodec(jsonrpc.NewJSONCodec(conn))
	c.SetBlocking(true)
	c.Handle("echo", echo)
	c.Handle("update", update)
	c.Handle("monitor_cancel", handleMonitorCancel)
	c.Handle("update2", update2)
	c.Handle("update3", update3)
	go c.Run()
	go handleDisconnectNotification(c)

	ovs := newOvsdbClient(timeout, c)

	// Process Async Notifications
	dbs, err := ovs.ListDbs()
	if err == nil {
		for _, db := range dbs {
			schema, err := ovs.GetSchema(db)
			if err == nil {
				ovs.Schema[db] = *schema
			} else {
				return nil, err
			}
		}
	}
	return ovs, nil
}

// Register registers the supplied NotificationHandler to recieve OVSDB Notifications
func (ovs *OvsdbClient) Register(handler NotificationHandler) {
	ovs.handlersMutex.Lock()
	defer ovs.handlersMutex.Unlock()
	ovs.handlers = append(ovs.handlers, handler)
}

//Get Handler by index
func getHandlerIndex(handler NotificationHandler, handlers []NotificationHandler) (int, error) {
	for i, h := range handlers {
		if reflect.DeepEqual(h, handler) {
			return i, nil
		}
	}
	return -1, errors.New("Handler not found")
}

// Unregister the supplied NotificationHandler to not recieve OVSDB Notifications anymore
func (ovs *OvsdbClient) Unregister(handler NotificationHandler) error {
	ovs.handlersMutex.Lock()
	defer ovs.handlersMutex.Unlock()
	i, err := getHandlerIndex(handler, ovs.handlers)
	if err != nil {
		return err
	}
	ovs.handlers = append(ovs.handlers[:i], ovs.handlers[i+1:]...)
	return nil
}

// NotificationHandler is the interface that must be implemented to receive notifcations
type NotificationHandler interface {
	// RFC 7047 section 4.1.6 Update Notification
	Update(context interface{}, tableUpdates TableUpdates)

	Update2(context interface{}, tableUpdates TableUpdates2)

	Update3(context interface{}, tableUpdates TableUpdates2, lastTxnId string)

	// RFC 7047 section 4.1.9 Locked Notification
	Locked([]interface{})

	// RFC 7047 section 4.1.10 Stolen Notification
	Stolen([]interface{})

	// RFC 7047 section 4.1.11 Echo Notification
	Echo([]interface{})

	Disconnected(*OvsdbClient)
}

// RFC 7047 : Section 4.1.6 : Echo
func echo(client *rpc2.Client, args []interface{}, reply *[]interface{}) error {
	*reply = args
	connectionsMutex.RLock()
	defer connectionsMutex.RUnlock()
	if _, ok := connections[client]; ok {
		connections[client].handlersMutex.Lock()
		defer connections[client].handlersMutex.Unlock()
		for _, handler := range connections[client].handlers {
			handler.Echo(nil)
		}
	}
	return nil
}

// RFC 7047 : Update Notification Section 4.1.6
// Processing "params": [<json-value>, <table-updates>]
func update(client *rpc2.Client, params []interface{}, reply *interface{}) error {
	if len(params) < 2 {
		return errors.New("Invalid Update message")
	}
	// Ignore params[0] as we dont use the <json-value> currently for comparison

	raw, ok := params[1].(map[string]interface{})
	if !ok {
		return errors.New("Invalid Update message")
	}
	var rowUpdates map[string]map[string]RowUpdate

	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	err = json.Unmarshal(b, &rowUpdates)
	if err != nil {
		return err
	}

	// Update the local DB cache with the tableUpdates
	tableUpdates := getTableUpdatesFromRawUnmarshal(rowUpdates)
	connectionsMutex.RLock()
	defer connectionsMutex.RUnlock()
	if _, ok := connections[client]; ok {
		connections[client].handlersMutex.Lock()
		defer connections[client].handlersMutex.Unlock()
		for _, handler := range connections[client].handlers {
			handler.Update(params[0], tableUpdates)
		}
	}

	return nil
}

func update2(client *rpc2.Client, params []interface{}, reply *interface{}) error {
	if len(params) < 2 {
		return errors.New("Invalid Update message")
	}
	// Ignore params[0] as we dont use the <json-value> currently for comparison

	raw, ok := params[1].(map[string]interface{})
	if !ok {
		return errors.New("Invalid Update message")
	}
	var rowUpdates2 map[string]map[string]RowUpdate2

	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	err = json.Unmarshal(b, &rowUpdates2)
	if err != nil {
		return err
	}

	// Update the local DB cache with the tableUpdates
	tableUpdates2 := getTableUpdates2FromRawUnmarshal(rowUpdates2)
	connectionsMutex.RLock()
	defer connectionsMutex.RUnlock()
	if _, ok := connections[client]; ok {
		connections[client].handlersMutex.Lock()
		defer connections[client].handlersMutex.Unlock()
		for _, handler := range connections[client].handlers {
			handler.Update2(params[0], tableUpdates2)
		}
	}

	return nil
}

func update3(client *rpc2.Client, params []interface{}, reply *interface{}) error {
	if len(params) != 3 {
		return fmt.Errorf("update3 requires exactly 3 args")
	}

	raw, ok := params[2].(map[string]interface{})
	if !ok {
		return errors.New("Invalid Update message")
	}
	var rowUpdates2 map[string]map[string]RowUpdate2

	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	err = json.Unmarshal(b, &rowUpdates2)
	if err != nil {
		return err
	}

	// Update the local DB cache with the tableUpdates
	tableUpdates2 := getTableUpdates2FromRawUnmarshal(rowUpdates2)
	connectionsMutex.RLock()
	defer connectionsMutex.RUnlock()
	if _, ok := connections[client]; ok {
		connections[client].handlersMutex.Lock()
		defer connections[client].handlersMutex.Unlock()
		for _, handler := range connections[client].handlers {
			handler.Update3(params[0], tableUpdates2, params[1].(string))
		}
	}
	return nil
}

// GetSchema returns the schema in use for the provided database name
// RFC 7047 : get_schema
func (ovs OvsdbClient) GetSchema(dbName string) (*DatabaseSchema, error) {
	ctx, cancel := context.WithTimeout(context.TODO(), ovs.timeout)
	defer cancel()

	args := NewGetSchemaArgs(dbName)
	var reply DatabaseSchema
	err := ovs.rpcClient.CallWithContext(ctx, "get_schema", args, &reply)
	if err != nil {
		return nil, err
	}
	ovs.Schema[dbName] = reply
	return &reply, err
}

// ListDbs returns the list of databases on the server
// RFC 7047 : list_dbs
func (ovs OvsdbClient) ListDbs() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.TODO(), ovs.timeout)
	defer cancel()

	var dbs []string
	err := ovs.rpcClient.CallWithContext(ctx, "list_dbs", nil, &dbs)
	if err != nil {
		log.Fatal("ListDbs failure", err)
	}
	return dbs, err
}

// Transact performs the provided Operation's on the database
// RFC 7047 : transact
func (ovs OvsdbClient) Transact(database string, operation ...Operation) ([]OperationResult, error) {
	var reply []OperationResult
	db, ok := ovs.Schema[database]
	if !ok {
		return nil, fmt.Errorf("invalid Database %q Schema", database)
	}

	if ok := db.ValidateOperations(operation...); !ok {
		return nil, errors.New("Validation failed for the operation")
	}

	ctx, cancel := context.WithTimeout(context.TODO(), ovs.timeout)
	defer cancel()

	args := NewTransactArgs(database, operation...)
	err := ovs.rpcClient.CallWithContext(ctx, "transact", args, &reply)
	if err != nil {
		return nil, err
	}
	return reply, nil
}

// MonitorAll is a convenience method to monitor every table/column
func (ovs OvsdbClient) MonitorAll(database string, jsonContext interface{}) (*TableUpdates, error) {
	schema, ok := ovs.Schema[database]
	if !ok {
		return nil, fmt.Errorf("invalid Database %q Schema", database)
	}

	requests := make(map[string]MonitorRequest)
	for table, tableSchema := range schema.Tables {
		var columns []string
		for column := range tableSchema.Columns {
			columns = append(columns, column)
		}
		requests[table] = MonitorRequest{
			Columns: columns,
			Select: MonitorSelect{
				Initial: true,
				Insert:  true,
				Delete:  true,
				Modify:  true,
			}}
	}
	return ovs.Monitor(database, jsonContext, requests)
}

// MonitorAll is a convenience method to monitor every table/column
func (ovs OvsdbClient) Monitor2All(database string, jsonContext interface{}) (*TableUpdates2, error) {
	schema, ok := ovs.Schema[database]
	if !ok {
		return nil, fmt.Errorf("invalid Database %q Schema", database)
	}

	requests := make(map[string]MonitorRequest)
	for table, tableSchema := range schema.Tables {
		var columns []string
		for column := range tableSchema.Columns {
			columns = append(columns, column)
		}
		requests[table] = MonitorRequest{
			Columns: columns,
			Select: MonitorSelect{
				Initial: true,
				Insert:  true,
				Delete:  true,
				Modify:  true,
			}}
	}
	return ovs.Monitor2(database, jsonContext, requests)
}

// MonitorCancel will request cancel a previously issued monitor request
// RFC 7047 : monitor_cancel
func (ovs OvsdbClient) MonitorCancel(database string, jsonContext interface{}) error {
	var reply OperationResult

	ctx, cancel := context.WithTimeout(context.TODO(), ovs.timeout)
	defer cancel()

	args := NewMonitorCancelArgs(jsonContext)
	err := ovs.rpcClient.CallWithContext(ctx, "monitor_cancel", args, &reply)
	if err != nil {
		return err
	}
	if reply.Error != "" {
		return fmt.Errorf("Error while executing transaction: %s", reply.Error)
	}
	return nil
}

// Monitor will provide updates for a given table/column
// RFC 7047 : monitor
func (ovs OvsdbClient) Monitor(database string, jsonContext interface{}, requests map[string]MonitorRequest) (*TableUpdates, error) {
	var reply TableUpdates

	ctx, cancel := context.WithTimeout(context.TODO(), ovs.timeout)
	defer cancel()

	args := NewMonitorArgs(database, jsonContext, requests)

	// This totally sucks. Refer to golang JSON issue #6213
	var response map[string]map[string]RowUpdate
	err := ovs.rpcClient.CallWithContext(ctx, "monitor", args, &response)
	reply = getTableUpdatesFromRawUnmarshal(response)
	if err != nil {
		return nil, err
	}
	return &reply, err
}

func (ovs OvsdbClient) Monitor2(database string, jsonContext interface{}, requests map[string]MonitorRequest) (*TableUpdates2, error) {
	var reply TableUpdates2

	ctx, cancel := context.WithTimeout(context.TODO(), ovs.timeout)
	defer cancel()

	args := NewMonitorArgs(database, jsonContext, requests)

	// This totally sucks. Refer to golang JSON issue #6213
	var response2 map[string]map[string]RowUpdate2
	err := ovs.rpcClient.CallWithContext(ctx, "monitor_cond", args, &response2)
	reply = getTableUpdates2FromRawUnmarshal(response2)
	if err != nil {
		return nil, err
	}
	return &reply, err
}

func (ovs OvsdbClient) Monitor3(database string, jsonContext interface{}, requests map[string]MonitorRequest, currentTxn string) (*TableUpdates2, string, error) {
	var reply TableUpdates2

	ctx, cancel := context.WithTimeout(context.TODO(), ovs.timeout)
	defer cancel()

	args := NewMonitorArgs3(database, jsonContext, requests, currentTxn)

	// This totally sucks. Refer to golang JSON issue #6213
	var response []interface{}
	err := ovs.rpcClient.CallWithContext(ctx, "monitor_cond_since", args, &response)
	if len(response) < 3 {
		return nil, "", fmt.Errorf("monitor_cond_since reply has less than 3 elements: %v", response)
	}
	b, err := json.Marshal(response[2])
	if err != nil {
		return nil, "", err
	}
	parsedResponse := make(map[string]map[string]RowUpdate2)
	err = json.Unmarshal(b, &parsedResponse)
	if err != nil {
		return nil, "", err
	}
	reply = getTableUpdates2FromRawUnmarshal(parsedResponse)
	if err != nil {
		return nil, "", err
	}
	return &reply, response[1].(string), err
}

func getTableUpdatesFromRawUnmarshal(raw map[string]map[string]RowUpdate) TableUpdates {
	var tableUpdates TableUpdates
	tableUpdates.Updates = make(map[string]TableUpdate)
	for table, update := range raw {
		tableUpdate := TableUpdate{update}
		tableUpdates.Updates[table] = tableUpdate
	}
	return tableUpdates
}

func getTableUpdates2FromRawUnmarshal(raw map[string]map[string]RowUpdate2) TableUpdates2 {
	var tableUpdates TableUpdates2
	tableUpdates.Updates = make(map[string]TableUpdate2)
	for table, update := range raw {
		tableUpdate := TableUpdate2{update}
		tableUpdates.Updates[table] = tableUpdate
	}
	return tableUpdates
}

func clearConnection(c *rpc2.Client) {
	connectionsMutex.Lock()
	defer connectionsMutex.Unlock()
	if _, ok := connections[c]; ok {
		for _, handler := range connections[c].handlers {
			if handler != nil {
				handler.Disconnected(connections[c])
			}
		}
	}
	delete(connections, c)
}

func handleDisconnectNotification(c *rpc2.Client) {
	disconnected := c.DisconnectNotify()
	select {
	case <-disconnected:
		clearConnection(c)
	}
}

// Disconnect will close the OVSDB connection
func (ovs OvsdbClient) Disconnect() {
	ovs.rpcClient.Close()
}

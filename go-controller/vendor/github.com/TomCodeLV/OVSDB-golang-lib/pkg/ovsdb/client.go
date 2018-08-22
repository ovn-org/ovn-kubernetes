package ovsdb

import (
	"net"
	"strconv"
	"math/rand"
	"time"
	"encoding/json"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/dbmonitor"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/dbtransaction"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/dbcache"
	"errors"
	)

type Lock struct {
	Locked bool
}

type Pending struct {
	channel  chan int
	response *json.RawMessage
	error *json.RawMessage
}

type callback func(interface{})

// ovsdb session handle structure
type OVSDB struct {
	Conn net.Conn
	ID string
	dec *json.Decoder
	enc *json.Encoder
	pending map[uint64]*Pending
	callbacks map[string]dbmonitor.Callback
	lockedCallback func(string)
	stolenCallback func(string)
	counter uint64
}

// Dial initiates ovsdb session
// It returns session handle and error if encountered
func Dial(network, address string) (*OVSDB, error) {
	ovsdb := new(OVSDB)
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}

	ovsdb.Conn = conn

	ovsdb.dec = json.NewDecoder(conn)
	ovsdb.enc = json.NewEncoder(conn)

	rand.Seed(time.Now().UnixNano())
	ovsdb.ID = "id" + strconv.FormatUint(rand.Uint64(), 10)

	ovsdb.pending = make(map[uint64]*Pending)
	ovsdb.callbacks = make(map[string]dbmonitor.Callback)
	ovsdb.counter = 0

	go ovsdb.loop()

	return ovsdb, nil
}

// closes ovsdb network connection
func (ovsdb *OVSDB) Close() error {
	return ovsdb.Conn.Close()
}

// incoming message header structure
// note that Result is stored in raw format
type message struct {
	Method string        		`json:"method"`
	Params []*json.RawMessage 	`json:"params"`
	Result *json.RawMessage		`json:"result"`
	Error  *json.RawMessage		`json:"error"`
	ID     interface{} 			`json:"id"`
}

type Error struct {
	Syntax string	`json:"syntax"`
	Details string	`json:"details"`
	Error string	`json:"error"`
}

// loop is responsible for receiving all incoming messages
func (ovsdb *OVSDB) loop() error {
	for true {
		var msg message
		// receive incoming message and store in header structure
		if err := ovsdb.dec.Decode(&msg); err != nil {
			return err
		}

		switch msg.Method {
		case "echo": // handle incoming echo messages
			resp := map[string]interface{}{
				"result": msg.Params,
				"error":  nil,
				"id":     "echo",
			}
			ovsdb.enc.Encode(resp)
		case "update": // handle incoming update notification
			var id string
			json.Unmarshal(*msg.Params[0], &id)
			ovsdb.callbacks[id](*msg.Params[1])
		case "locked":
			if ovsdb.lockedCallback != nil {
				var resp string
				json.Unmarshal(*msg.Params[0], &resp)
				ovsdb.lockedCallback(resp)
			}
		case "stolen":
			if ovsdb.stolenCallback != nil {
				var resp string
				json.Unmarshal(*msg.Params[0], &resp)
				ovsdb.stolenCallback(resp)
			}
		default: // handle incoming response
			id := uint64(msg.ID.(float64))
			if msg.Error == nil {
				ovsdb.pending[id].response = msg.Result
			} else {
				ovsdb.pending[id].error = msg.Error
			}
			// unblock related call invocation
			ovsdb.pending[id].channel <- 1
		}
	}

	return nil
}

// call sends request to server and blocks
// after it is unblocked in incoming message receiver loop it returns response
// from server as raw data to be unmarshaled later
func (ovsdb *OVSDB) Call(method string, args interface{}, idref *uint64) (json.RawMessage, error) {
	id := ovsdb.GetCounter()
	if idref != nil {
		*idref = id
	}

	// create RPC request
	req := map[string]interface{}{
		"method": method,
		"params": args,
		"id":     id,
	}

	ch := make(chan int, 1)

	// store channel in list to pass it to receiver loop
	ovsdb.pending[id] = &Pending{
		channel:  ch,
	}

	// send message
	err := ovsdb.enc.Encode(req)

	// block function
	<-ch

	if ovsdb.pending[id].error != nil {
		var err2 Error

		json.Unmarshal(*ovsdb.pending[id].error, &err2)

		delete(ovsdb.pending, id)

		return nil, errors.New(err2.Error + ": " + err2.Details + " (" + err2.Syntax + ")" )
	}

	response := ovsdb.pending[id].response
	delete(ovsdb.pending, id)

	return *response, err
}

func (ovsdb *OVSDB) Notify(method string, args interface{}) error {
	req := map[string]interface{}{
		"method": method,
		"params": args,
		"id":     nil,
	}
	err := ovsdb.enc.Encode(req)

	return err
}

func (ovsdb *OVSDB) AddCallBack(id string, callback dbmonitor.Callback) {
	ovsdb.callbacks[id] = callback
}

func (ovsdb *OVSDB) GetCounter() uint64 {
	counter := ovsdb.counter
	ovsdb.counter++
	return counter
}

// ListDbs returns list of databases
func (ovsdb *OVSDB) ListDbs() []string {
	response, _ := ovsdb.Call("list_dbs", []interface{}{}, nil)
	dbs := []string{}
	json.Unmarshal(response, &dbs)
	return dbs
}

// GetSchema returns schema object containing all db schema data
func (ovsdb *OVSDB) GetSchema(schema string) (json.RawMessage, error) {
	return ovsdb.Call("get_schema", []string{schema}, nil)
}

// ===================================
// ADVANCED FUNCTIONALITY CONSTRUCTORS
// ===================================

// Transaction returns transaction handle
func (ovsdb *OVSDB) Transaction(schema string) *dbtransaction.Transaction {
	txn := new(dbtransaction.Transaction)

	txn.OVSDB = ovsdb
	txn.Schema = schema
	txn.Tables = map[string]string{}
	txn.References = make(map[string][]interface{})
	txn.Counter = 1

	return txn
}

func (ovsdb *OVSDB) Monitor(schema string) *dbmonitor.Monitor {
	monitor := new(dbmonitor.Monitor)

	monitor.OVSDB = ovsdb
	monitor.Schema = schema
	monitor.MonitorRequests = make(map[string]interface{})

	return monitor
}

type Cache struct {
	Schema string
	Tables map[string][]string
	Indexes map[string][]string
}

func (ovsdb *OVSDB) Cache(c Cache) (*dbcache.Cache, error) {
	cache := new(dbcache.Cache)

	cache.OVSDB = ovsdb
	cache.Schema = c.Schema
	cache.Indexes = c.Indexes

	err := cache.StartMonitor(c.Schema, c.Tables)
	if err != nil {
		return nil, err
	}

	return cache, nil
}

// =======
// LOCKING
// =======

func (ovsdb *OVSDB) RegisterLockedCallback(Callback func(string)) {
	ovsdb.lockedCallback = Callback
}

func (ovsdb *OVSDB) RegisterStolenCallback(Callback func(string)) {
	ovsdb.stolenCallback = Callback
}

func (ovsdb *OVSDB) Lock(id string) (interface{}, error) {
	response, err := ovsdb.Call("lock", []string{id}, nil)
	lock := Lock{}
	json.Unmarshal(response, &lock)
	return lock, err
}

func (ovsdb *OVSDB) Steal(id string) (interface{}, error) {
	response, err := ovsdb.Call("steal", []string{id}, nil)
	lock := Lock{}
	json.Unmarshal(response, &lock)
	return lock, err
}

func (ovsdb *OVSDB) Unlock(id string) (interface{}, error) {
	response, err := ovsdb.Call("unlock", []string{id}, nil)
	lock := Lock{}
	json.Unmarshal(response, &lock)
	return lock, err
}



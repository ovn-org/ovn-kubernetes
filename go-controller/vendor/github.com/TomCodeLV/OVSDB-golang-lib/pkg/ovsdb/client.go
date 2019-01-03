package ovsdb

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/dbcache"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/dbmonitor"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/dbtransaction"
	"io/ioutil"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"
)

type Lock struct {
	Locked bool
}

type Pending struct {
	channel  chan int
	response *json.RawMessage
	error *json.RawMessage
	connectionClosed bool
}

type callback func(interface{})

// ovsdb session handle structure
type OVSDB struct {
	Conn net.Conn
	ID string
	dec *json.Decoder
	enc *json.Encoder
	decoderMutex *sync.Mutex
	encoderMutex *sync.Mutex
	pending map[uint64]*Pending
	pendingMutex *sync.Mutex
	callbacks map[string]dbmonitor.Callback
	callbacksMutex *sync.Mutex
	lockedCallback func(string)
	stolenCallback func(string)
	counter uint64
	counterMutex *sync.Mutex
	synchronize *Synchronize
	closed bool
	closedMutex *sync.Mutex
}

// helper structure for synchronizing db connection and socket reads and writes
type Synchronize struct {
	connected customCond
	initialized customCond
	socketError customCond
}

type customCond struct {
	sync.Mutex
	cond *sync.Cond
	val bool
}

func (s *Synchronize) init() {
	s.connected = customCond{}
	s.connected.cond = sync.NewCond(&s.connected)

	s.initialized = customCond{}
	s.initialized.cond = sync.NewCond(&s.initialized)

	s.socketError = customCond{}
	s.socketError.cond = sync.NewCond(&s.socketError)
}

// if not connected waits until connection is established
func (s *Synchronize) WaitConnected() bool {
	s.connected.Lock()
	if !s.connected.val {
		s.connected.cond.Wait()
		s.connected.Unlock()
		return true
	}
	s.connected.Unlock()
	return false
}

func (s *Synchronize) SetConnected() {
	s.connected.Lock()
	s.socketError.Lock()
	s.connected.val = true
	s.socketError.val = false
	s.connected.cond.Broadcast()
	s.socketError.Unlock()
	s.connected.Unlock()
}

// if not initialized waits until initialization callback is completed
func (s *Synchronize) WaitInitialized() {
	s.initialized.Lock()
	if !s.initialized.val {
		s.initialized.cond.Wait()
	}
	s.initialized.Unlock()
}

func (s *Synchronize) SetInitialized() {
	s.initialized.Lock()
	s.initialized.val = true
	s.initialized.cond.Broadcast()
	s.initialized.Unlock()
}

// if there is no socket error, locks and waits until socket errors
func (s *Synchronize) WaitError() {
	s.socketError.Lock()
	if !s.socketError.val {
		s.socketError.cond.Wait()
	}
	s.socketError.Unlock()
}

func (s *Synchronize) SetError() {
	s.socketError.Lock()
	s.connected.Lock()
	s.initialized.Lock()
	s.socketError.val = true
	s.connected.val = false
	s.initialized.val = false
	s.socketError.cond.Broadcast()
	s.initialized.Unlock()
	s.connected.Unlock()
	s.socketError.Unlock()
}

// PersistentDial provides automatic reconnection in case of connection failure.
// Reconnection will be performed with each provided address.
// After unsuccessfully trying all addresses it will sleep for 1,2,4,8,8,8,...
// seconds before trying again.
// Initialize will be called after every successful connection to db.
// Function will lock until first successful connect.
// Returns a pointer to db which will point to new db structure on each connect.
func Dial(addressList [][]string, initialize func(*OVSDB) error, options map[string]interface{}) *OVSDB {
	ovsdb := new(OVSDB)

	ovsdb.decoderMutex = new(sync.Mutex)
	ovsdb.encoderMutex = new(sync.Mutex)

	ovsdb.pendingMutex = new(sync.Mutex)
	ovsdb.pending = make(map[uint64]*Pending)

	ovsdb.callbacks = make(map[string]dbmonitor.Callback)
	ovsdb.callbacksMutex = new(sync.Mutex)

	ovsdb.counterMutex = new(sync.Mutex)
	ovsdb.counter = 0

	ovsdb.synchronize = new(Synchronize)
	ovsdb.synchronize.init()

	ovsdb.closedMutex = new(sync.Mutex)

	idx := 0
	timeOut := 1

	go func() {
		for true {
			network := addressList[idx][0]
			address := addressList[idx][1]

			var err error
			var conn net.Conn
			if network == "ssl" {
				certFile := addressList[idx][2]
				privateKeyFile := addressList[idx][3]
				CACertFile := addressList[idx][4]

				cert, _ := tls.LoadX509KeyPair(certFile, privateKeyFile)
				caCert, _ := ioutil.ReadFile(CACertFile)
				caCertPool := x509.NewCertPool()
				caCertPool.AppendCertsFromPEM(caCert)

				cfg := &tls.Config{
					Certificates: []tls.Certificate{cert},
					ServerName: options["ServerName"].(string),
					RootCAs : caCertPool,
					InsecureSkipVerify: options["InsecureSkipVerify"].(bool),
				}

				conn, err = tls.Dial("tcp", address, cfg)
			} else {
				conn, err = net.Dial(network, address)
			}

			if err != nil {
				idx = idx + 1
				if idx == len(addressList) {
					time.Sleep(time.Duration(timeOut) * time.Second)

					idx = 0
					if timeOut < 8 {
						timeOut = timeOut * 2
					}
				}
			} else {
				idx = 0
				timeOut = 1

				ovsdb.closedMutex.Lock()
				ovsdb.closed = false
				ovsdb.closedMutex.Unlock()

				ovsdb.Conn = conn

				ovsdb.decoderMutex.Lock()
				ovsdb.dec = json.NewDecoder(conn)
				ovsdb.decoderMutex.Unlock()
				ovsdb.encoderMutex.Lock()
				ovsdb.enc = json.NewEncoder(conn)
				ovsdb.encoderMutex.Unlock()

				rand.Seed(time.Now().UnixNano())
				ovsdb.ID = "id" + strconv.FormatUint(rand.Uint64(), 10)

				go ovsdb.loop()

				ovsdb.synchronize.SetConnected()

				// need to run initialize concurrently so connect loop wouldn't
				// lock if initialize hits socket error and locks
				go func() {
					if initialize == nil {
						ovsdb.synchronize.SetInitialized()
					} else {
						err = initialize(ovsdb)
						if err == nil {
							ovsdb.synchronize.SetInitialized()
						}
					}
				}()

				ovsdb.synchronize.WaitError()
			}
		}
	}()

	// lock until initialize called
	ovsdb.synchronize.WaitInitialized()

	return ovsdb
}

// closes ovsdb network connection
func (ovsdb *OVSDB) Close() error {
	ovsdb.closedMutex.Lock()
	if ovsdb.closed == true {
		ovsdb.closedMutex.Unlock()
		return nil
	}
	ovsdb.closed = true
	ovsdb.closedMutex.Unlock()

	resp := ovsdb.Conn.Close()

	ovsdb.callbacksMutex.Lock()
	for id, _ := range ovsdb.callbacks {
		delete(ovsdb.callbacks, id)
	}
	ovsdb.callbacksMutex.Unlock()

	// unlock all pending calls
	ovsdb.pendingMutex.Lock()
	for _, val := range ovsdb.pending {
		val.connectionClosed = true
		val.channel <- 1
	}
	ovsdb.pendingMutex.Unlock()

	return resp
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


func (ovsdb *OVSDB) encodeWrapper(v interface{}) error {
	ovsdb.encoderMutex.Lock()
	err := ovsdb.enc.Encode(v)
	ovsdb.encoderMutex.Unlock()
	if err != nil {
		if ovsdb.synchronize != nil {
			ovsdb.Close()
			ovsdb.synchronize.SetError()
		}
		return err
	}
	return nil
}

func (ovsdb *OVSDB) decodeWrapper(v *message) error {
	ovsdb.decoderMutex.Lock()
	err := ovsdb.dec.Decode(v)
	ovsdb.decoderMutex.Unlock()
	if err != nil {
		ovsdb.Close()
		if ovsdb.synchronize != nil {
			ovsdb.synchronize.SetError()
		}
		return err
	}
	return nil
}

// loop is responsible for receiving all incoming messages
func (ovsdb *OVSDB) loop() {
	for true {
		var msg message
		// receive incoming message and store in header structure
		if err := ovsdb.decodeWrapper(&msg); err != nil {
			return
		}

		switch msg.Method {
		case "echo": // handle incoming echo messages
			resp := map[string]interface{}{
				"result": msg.Params,
				"error":  nil,
				"id":     "echo",
			}
			ovsdb.encodeWrapper(resp)
		case "update": // handle incoming update notification
			var id string
			json.Unmarshal(*msg.Params[0], &id)
			ovsdb.callbacksMutex.Lock()
			if _, ok := ovsdb.callbacks[id]; ok {
				ovsdb.callbacks[id](*msg.Params[1])
			}
			ovsdb.callbacksMutex.Unlock()
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
			ovsdb.pendingMutex.Lock()
			if msg.Error == nil {
				ovsdb.pending[id].response = msg.Result
			} else {
				ovsdb.pending[id].error = msg.Error
			}
			// unblock related call invocation
			ovsdb.pending[id].channel <- 1
			ovsdb.pendingMutex.Unlock()
		}
	}
}

// call sends request to server and blocks
// after it is unblocked in incoming message receiver loop it returns response
// from server as raw data to be unmarshaled later
func (ovsdb *OVSDB) Call(method string, args interface{}, idref *uint64) (json.RawMessage, error) {
	if ovsdb.synchronize != nil && ovsdb.synchronize.WaitConnected() {
		return nil, errors.New("no connection")
	}

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
	ovsdb.pendingMutex.Lock()
	ovsdb.pending[id] = &Pending{
		channel:  ch,
	}
	ovsdb.pendingMutex.Unlock()

	// send message
	err := ovsdb.encodeWrapper(req)
	if err != nil {
		return nil, err
	}

	// block function
	<-ch

	ovsdb.pendingMutex.Lock()
	if ovsdb.pending[id].connectionClosed {
		ovsdb.pendingMutex.Unlock()
		return nil, errors.New("connection closed")
	}

	// transaction error always is null, OVSDB errors for transactions are handled later
	if ovsdb.pending[id].error != nil {
		var err2 Error

		json.Unmarshal(*ovsdb.pending[id].error, &err2)

		delete(ovsdb.pending, id)

		ovsdb.pendingMutex.Unlock()
		return nil, errors.New(err2.Error + ": " + err2.Details + " (" + err2.Syntax + ")" )
	}

	response := ovsdb.pending[id].response
	delete(ovsdb.pending, id)

	ovsdb.pendingMutex.Unlock()
	return *response, nil
}

func (ovsdb *OVSDB) Notify(method string, args interface{}) error {
	req := map[string]interface{}{
		"method": method,
		"params": args,
		"id":     nil,
	}
	err := ovsdb.encodeWrapper(req)

	return err
}

func (ovsdb *OVSDB) AddCallBack(id string, callback dbmonitor.Callback) {
	ovsdb.callbacksMutex.Lock()
	ovsdb.callbacks[id] = callback
	ovsdb.callbacksMutex.Unlock()
}

func (ovsdb *OVSDB) GetCounter() uint64 {
	ovsdb.counterMutex.Lock()
	counter := ovsdb.counter
	ovsdb.counter++
	ovsdb.counterMutex.Unlock()
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

	cache.ID = "id" + strconv.FormatUint(rand.Uint64(), 10)
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



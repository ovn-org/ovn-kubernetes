package libovsdb

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/alexflint/go-filemutex"
	guuid "github.com/google/uuid"
	"github.com/mitchellh/copystructure"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/database"
	"github.com/ovn-org/libovsdb/database/inmemory"
	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/libovsdb/ovsdb/serverdb"
	"github.com/ovn-org/libovsdb/server"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cryptorand"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type TestSetup struct {
	// IgnoreConstraints, when true, ignores constraints validation errors
	// when adding data to the database, allowing a testcase to force
	// addition of invalid data (like duplicate indexes).
	IgnoreConstraints bool

	NBData  []TestData
	SBData  []TestData
	OVSData []TestData
}

type TestData interface{}

type clientBuilderFn func(config.OvnAuthConfig, *Context) (libovsdbclient.Client, error)
type serverBuilderFn func(config.OvnAuthConfig, []TestData, bool) (*TestOvsdbServer, error)

var validUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Context struct {
	clientStopCh chan struct{}
	clientWg     *sync.WaitGroup
	serverStopCh chan struct{}
	serverWg     *sync.WaitGroup

	SBServer  *TestOvsdbServer
	NBServer  *TestOvsdbServer
	OVSServer *TestOvsdbServer
}

func newContext() *Context {
	return &Context{
		clientStopCh: make(chan struct{}),
		clientWg:     &sync.WaitGroup{},
		serverStopCh: make(chan struct{}),
		serverWg:     &sync.WaitGroup{},
	}
}

func (c *Context) Cleanup() {
	// Stop the client first to ensure we don't trigger reconnect behavior
	// due to a stopped server
	close(c.clientStopCh)
	c.clientWg.Wait()
	close(c.serverStopCh)
	c.serverWg.Wait()
}

// NewNBSBTestHarness runs NB & SB OVSDB servers and returns corresponding clients
func NewNBSBTestHarness(setup TestSetup) (libovsdbclient.Client, libovsdbclient.Client, *Context, error) {
	testCtx := newContext()

	nbClient, _, err := NewNBTestHarness(setup, testCtx)
	if err != nil {
		return nil, nil, nil, err
	}
	sbClient, _, err := NewSBTestHarness(setup, testCtx)
	if err != nil {
		return nil, nil, nil, err
	}
	return nbClient, sbClient, testCtx, nil
}

// NewNBTestHarness runs NB server and returns corresponding client
func NewNBTestHarness(setup TestSetup, testCtx *Context) (libovsdbclient.Client, *Context, error) {
	if testCtx == nil {
		testCtx = newContext()
	}

	client, server, err := newOVSDBTestHarness(setup.NBData, setup.IgnoreConstraints, newNBServer, newNBClient, testCtx)
	if err != nil {
		return nil, nil, err
	}
	testCtx.NBServer = server

	return client, testCtx, err
}

// NewSBTestHarness runs SB server and returns corresponding client
func NewSBTestHarness(setup TestSetup, testCtx *Context) (libovsdbclient.Client, *Context, error) {
	if testCtx == nil {
		testCtx = newContext()
	}

	client, server, err := newOVSDBTestHarness(setup.SBData, setup.IgnoreConstraints, newSBServer, newSBClient, testCtx)
	if err != nil {
		return nil, nil, err
	}
	testCtx.SBServer = server

	return client, testCtx, err
}

// NewOVSTestHarness runs OVSDB server and returns corresponding client
func NewOVSTestHarness(setup TestSetup) (libovsdbclient.Client, *Context, error) {
	testCtx := newContext()
	randBytes := make([]byte, 16)
	cryptorand.Read(randBytes)
	tmpOVSSocketPath := filepath.Join(os.TempDir(), "ovs-"+hex.EncodeToString(randBytes))

	cfg := config.OvnAuthConfig{
		Scheme:  config.OvnDBSchemeUnix,
		Address: "unix:" + tmpOVSSocketPath,
	}

	server, err := newOVSServer(cfg, setup.OVSData, false)
	if err != nil {
		return nil, nil, err
	}

	client, err := newOVSClient(cfg, testCtx)
	if err != nil {
		server.Close()
		return nil, nil, err
	}

	testCtx.serverWg.Add(1)
	go func() {
		defer testCtx.serverWg.Done()
		<-testCtx.serverStopCh
		server.Close()
	}()

	if err != nil {
		return nil, nil, err
	}
	testCtx.OVSServer = server

	return client, testCtx, err
}

func newOVSDBTestHarness(serverData []TestData, ignoreConstraints bool, newServer serverBuilderFn, newClient clientBuilderFn, testCtx *Context) (libovsdbclient.Client, *TestOvsdbServer, error) {
	cfg := config.OvnAuthConfig{
		Scheme:  config.OvnDBSchemeUnix,
		Address: "unix:" + tempOVSDBSocketFileName(),
	}

	s, err := newServer(cfg, serverData, ignoreConstraints)
	if err != nil {
		return nil, nil, err
	}

	c, err := newClient(cfg, testCtx)
	if err != nil {
		s.Close()
		return nil, nil, err
	}

	testCtx.serverWg.Add(1)
	go func() {
		defer testCtx.serverWg.Done()
		<-testCtx.serverStopCh
		s.Close()
	}()

	return c, s, nil
}

func clientWaitOnCleanup(testCtx *Context, client libovsdbclient.Client, stopChan chan struct{}) {
	testCtx.clientWg.Add(1)
	go func() {
		defer testCtx.clientWg.Done()
		<-testCtx.clientStopCh
		close(stopChan)
		client.Close()
	}()
}

func newNBClient(cfg config.OvnAuthConfig, testCtx *Context) (libovsdbclient.Client, error) {
	stopChan := make(chan struct{})
	nbClient, err := libovsdb.NewNBClientWithConfig(cfg, prometheus.NewRegistry(), stopChan)
	if err != nil {
		return nil, err
	}
	clientWaitOnCleanup(testCtx, nbClient, stopChan)
	return nbClient, err
}

func newSBClient(cfg config.OvnAuthConfig, testCtx *Context) (libovsdbclient.Client, error) {
	stopChan := make(chan struct{})
	sbClient, err := libovsdb.NewSBClientWithConfig(cfg, prometheus.NewRegistry(), stopChan)
	if err != nil {
		return nil, err
	}

	// we require additional monitoring for testing
	// see CreateTransitSwitchPortBindings
	_, err = sbClient.Monitor(context.Background(), sbClient.NewMonitor(libovsdbclient.WithTable(&sbdb.DatapathBinding{})))
	if err != nil {
		return nil, err
	}

	clientWaitOnCleanup(testCtx, sbClient, stopChan)
	return sbClient, err
}

func newSBServer(cfg config.OvnAuthConfig, data []TestData, ignoreConstraints bool) (*TestOvsdbServer, error) {
	dbModel, err := sbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	schema := sbdb.Schema()
	return newOVSDBServer(cfg, dbModel, schema, data, ignoreConstraints)
}

func newNBServer(cfg config.OvnAuthConfig, data []TestData, ignoreConstraints bool) (*TestOvsdbServer, error) {
	dbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	schema := nbdb.Schema()
	return newOVSDBServer(cfg, dbModel, schema, data, ignoreConstraints)
}

func newOVSServer(cfg config.OvnAuthConfig, data []TestData, ignoreConstraints bool) (*TestOvsdbServer, error) {
	dbModel, err := vswitchd.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	schema := vswitchd.Schema()
	return newOVSDBServer(cfg, dbModel, schema, data, ignoreConstraints)
}

func newOVSClient(cfg config.OvnAuthConfig, testCtx *Context) (libovsdbclient.Client, error) {
	stopChan := make(chan struct{})
	ovsClient, err := libovsdb.NewOVSClientWithConfig(cfg, stopChan)
	if err != nil {
		return nil, err
	}

	clientWaitOnCleanup(testCtx, ovsClient, stopChan)
	return ovsClient, err
}

func testDataToOperations(dbMod model.DatabaseModel, data []TestData) ([]ovsdb.Operation, error) {
	m := mapper.NewMapper(dbMod.Schema)
	newData := copystructure.Must(copystructure.Copy(data)).([]TestData)

	ops := make([]ovsdb.Operation, 0, len(newData))
	for _, d := range newData {
		tableName := dbMod.FindTable(reflect.TypeOf(d))
		if tableName == "" {
			return nil, fmt.Errorf("object of type %s is not part of the DBModel", reflect.TypeOf(d))
		}

		info, err := mapper.NewInfo(tableName, dbMod.Schema.Table(tableName), d)
		if err != nil {
			return nil, err
		}

		var realUUID, namedUUID string
		if uuid, err := info.FieldByColumn("_uuid"); err == nil {
			tmpUUID := uuid.(string)
			if ovsdb.IsNamedUUID(tmpUUID) {
				namedUUID = tmpUUID
			} else if ovsdb.IsValidUUID(tmpUUID) {
				realUUID = tmpUUID
			}
		} else {
			return nil, err
		}

		row, err := m.NewRow(info)
		if err != nil {
			return nil, err
		}
		// UUID is given in the operation, not the object
		delete(row, "_uuid")

		// Since we may be writing directly to the database we need to
		// generate real UUIDs if the row didn't have one
		if realUUID == "" {
			realUUID = guuid.NewString()
		}

		ops = append(ops, ovsdb.Operation{
			Op:       ovsdb.OperationInsert,
			Table:    tableName,
			Row:      row,
			UUID:     realUUID,
			UUIDName: namedUUID,
		})
	}

	if ok := dbMod.Schema.ValidateOperations(ops...); !ok {
		return nil, fmt.Errorf("operations invalid for database schema %q", dbMod.Schema.Name)
	}

	return ops, nil
}

func updateData(db database.Database, dbMod model.DatabaseModel, data []TestData, ignoreConstraints bool) error {
	ops, err := testDataToOperations(dbMod, data)
	if err != nil {
		return err
	}

	t := db.NewTransaction(dbMod.Schema.Name)
	res, updates := t.Transact(ops...)

	cr := make([]ovsdb.OperationResult, 0, len(res))
	for _, r := range res {
		cr = append(cr, *r)
	}
	if _, err := ovsdb.CheckOperationResults(cr, ops); err != nil {
		// Return any error, but optionally ignore constraint violations if requested
		_, isConstraintErr := err.(*ovsdb.ConstraintViolation)
		if !isConstraintErr || !ignoreConstraints {
			return fmt.Errorf("failed to insert test data %v: %v", ops, err)
		}
	}
	if err := db.Commit(dbMod.Schema.Name, guuid.New(), updates); err != nil {
		return fmt.Errorf("error populating server with initial data: %v", err)
	}

	return nil
}

type TestOvsdbServer struct {
	*server.OvsdbServer
	db    database.Database
	dbMod model.DatabaseModel
}

func newOVSDBServer(cfg config.OvnAuthConfig, dbModel model.ClientDBModel, schema ovsdb.DatabaseSchema, data []TestData, ignoreConstraints bool) (*TestOvsdbServer, error) {
	serverDBModel, err := serverdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	serverSchema := serverdb.Schema()

	db := inmemory.NewDatabase(map[string]model.ClientDBModel{
		schema.Name:       dbModel,
		serverSchema.Name: serverDBModel,
	})

	dbMod, errs := model.NewDatabaseModel(schema, dbModel)
	if len(errs) > 0 {
		log.Fatal(errs)
	}

	servMod, errs := model.NewDatabaseModel(serverSchema, serverDBModel)
	if len(errs) > 0 {
		log.Fatal(errs)
	}

	s, err := server.NewOvsdbServer(db, dbMod, servMod)
	if err != nil {
		return nil, err
	}

	// Populate the _Server database table
	sid := fmt.Sprintf("%04x", cryptorand.Uint32())
	serverData := []TestData{
		&serverdb.Database{
			Name:      dbModel.Name(),
			Connected: true,
			Leader:    true,
			Model:     serverdb.DatabaseModelClustered,
			Sid:       &sid,
		},
	}
	if err := updateData(db, servMod, serverData, false); err != nil {
		return nil, err
	}

	// Populate with testcase data
	if len(data) > 0 {
		if err := updateData(db, dbMod, data, ignoreConstraints); err != nil {
			return nil, err
		}
	}

	sockPath := strings.TrimPrefix(cfg.Address, "unix:")
	lockPath := fmt.Sprintf("%s.lock", sockPath)
	fileMutex, err := filemutex.New(lockPath)
	if err != nil {
		return nil, err
	}

	err = fileMutex.Lock()
	if err != nil {
		return nil, err
	}
	go func() {
		if err := s.Serve(string(cfg.Scheme), sockPath); err != nil {
			log.Fatalf("libovsdb test harness error: %v", err)
		}
		fileMutex.Close()
		os.RemoveAll(lockPath)
		os.RemoveAll(sockPath)
	}()

	err = wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) { return s.Ready(), nil })
	if err != nil {
		s.Close()
		return nil, err
	}

	return &TestOvsdbServer{
		OvsdbServer: s,
		db:          db,
		dbMod:       dbMod,
	}, nil
}

// CreateTestData inserts test data into the database after the server is started.
// We must use the server's Transact() method to ensure updates are sent to clients
// that may have already been created.
func (t *TestOvsdbServer) CreateTestData(data []TestData) error {
	ops, err := testDataToOperations(t.dbMod, data)
	if err != nil {
		return err
	}

	// Marshal Transact args to JSON
	args := ovsdb.NewTransactArgs(t.dbMod.Schema.Name, ops...)
	jsonBytes, err := json.Marshal(struct {
		Params interface{} `json:"params"`
	}{args})
	if err != nil {
		return fmt.Errorf("failed to marshal parameters: %v", err)
	}

	// Unmarshal each arg into a raw JSON message for Transact
	var rawArgs struct {
		Params []json.RawMessage `json:"params"`
	}
	if err = json.Unmarshal(jsonBytes, &rawArgs); err != nil {
		return fmt.Errorf("failed to unmarshal parameters: %v", err)
	}

	var rawReply []*ovsdb.OperationResult
	if err := t.OvsdbServer.Transact(nil, rawArgs.Params, &rawReply); err != nil {
		return fmt.Errorf("failed to transact test data: %v", err)
	}

	results := make([]ovsdb.OperationResult, 0, len(rawReply))
	for _, result := range rawReply {
		results = append(results, *result)
	}
	opErrors, err := ovsdb.CheckOperationResults(results, ops)
	if err != nil {
		return fmt.Errorf("error in transact with ops %+v results %+v and errors %+v: %v", ops, results, opErrors, err)
	}

	return nil
}

func tempOVSDBSocketFileName() string {
	randBytes := make([]byte, 16)
	cryptorand.Read(randBytes)
	return filepath.Join(os.TempDir(), "ovsdb-"+hex.EncodeToString(randBytes))
}

func getTestDataFromClientCache(client libovsdbclient.Client) []TestData {
	cache := client.Cache()
	data := []TestData{}
	for _, tname := range cache.Tables() {
		table := cache.Table(tname)
		for _, row := range table.Rows() {
			data = append(data, row)
		}
	}
	return data
}

// replaceUUIDs replaces atomic, slice or map strings from the mapping
// function provided
func replaceUUIDs(data TestData, mapFrom func(string, int) string) {
	v := reflect.ValueOf(data)
	if v.Kind() != reflect.Ptr {
		return
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return
	}
	for i, n := 0, v.NumField(); i < n; i++ {
		f := v.Field(i).Interface()
		switch f := f.(type) {
		case string:
			v.Field(i).Set(reflect.ValueOf(mapFrom(f, i)))
		case *string:
			if f != nil {
				tmp := mapFrom(*f, i)
				v.Field(i).Set(reflect.ValueOf(&tmp))
			}
		case []string:
			for si, sv := range f {
				f[si] = mapFrom(sv, i)
			}
		case map[string]string:
			for mk, mv := range f {
				nv := mapFrom(mv, i)
				nk := mapFrom(mk, i)
				f[nk] = nv
				if nk != mk {
					delete(f, mk)
				}
			}
		}
	}
}

// getUUID gets the value of the field with ovsdb tag `uuid`
func getUUID(x TestData) (string, int) {
	v := reflect.ValueOf(x)
	if v.Kind() != reflect.Ptr {
		return "", -1
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return "", -1
	}
	for i, n := 0, v.NumField(); i < n; i++ {
		if tag := v.Type().Field(i).Tag.Get("ovsdb"); tag == "_uuid" {
			return v.Field(i).String(), i
		}
	}
	return "", -1
}

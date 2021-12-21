package libovsdb

import (
	"encoding/hex"
	"fmt"
	"log"
	"math/rand"
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
	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/libovsdb/ovsdb/serverdb"
	"github.com/ovn-org/libovsdb/server"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
)

type TestSetup struct {
	NBData []TestData
	SBData []TestData
}

type TestData interface{}

type clientBuilderFn func(config.OvnAuthConfig, <-chan struct{}) (*libovsdb.Client, error)

var validUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type dbServer struct {
	s        *server.OvsdbServer
	db       server.Database
	schema   ovsdb.DatabaseSchema
	model    model.ClientDBModel
	cfg      config.OvnAuthConfig
	lockPath string
	sockPath string
	lock     *filemutex.FileMutex
}

func (dbs *dbServer) start(data []TestData, wg *sync.WaitGroup) error {
	// Populate with testcase data
	if len(data) > 0 {
		if err := updateData(dbs.db, dbs.model, dbs.schema, data); err != nil {
			return err
		}
	}

	dbs.sockPath = strings.TrimPrefix(dbs.cfg.Address, "unix:")
	dbs.lockPath = fmt.Sprintf("%s.lock", dbs.sockPath)
	var err error
	dbs.lock, err = filemutex.New(dbs.lockPath)
	if err != nil {
		return err
	}
	if err := dbs.lock.Lock(); err != nil {
		return err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := dbs.s.Serve(string(dbs.cfg.Scheme), dbs.sockPath); err != nil {
			log.Fatalf("libovsdb test harness error: %v", err)
		}
	}()

	return wait.Poll(100*time.Millisecond, 500*time.Millisecond, func() (bool, error) {
		return dbs.s.Ready(), nil
	})
}

func (dbs *dbServer) Close() {
	dbs.s.Close()
	if dbs.lock != nil {
		dbs.lock.Close()
	}
	if dbs.lockPath != "" {
		os.RemoveAll(dbs.lockPath)
	}
	if dbs.sockPath != "" {
		os.RemoveAll(dbs.sockPath)
	}
}

type Harness struct {
	NBClient *libovsdb.Client
	nbServer *dbServer
	SBClient *libovsdb.Client
	sbServer *dbServer

	// Shared between NB and SB
	clientStopCh chan struct{}
	serverWg     *sync.WaitGroup
}

func newHarness() *Harness {
	return &Harness{
		clientStopCh: make(chan struct{}),
		serverWg:     &sync.WaitGroup{},
	}
}

func (h *Harness) Cleanup() {
	// Stop the clients first to ensure we don't trigger reconnect behavior
	// due to a stopped server
	close(h.clientStopCh)
	if h.NBClient != nil {
		h.NBClient.Close()
	}
	if h.SBClient != nil {
		h.SBClient.Close()
	}

	if h.nbServer != nil {
		h.nbServer.Close()
	}
	if h.sbServer != nil {
		h.sbServer.Close()
	}
	h.serverWg.Wait()
}

// NewNBSBTestHarness creates NB & SB OVSDB servers and and clients for a test
// harness and and returns the harness
func NewNBSBTestHarness() (*Harness, error) {
	h := newHarness()

	if err := h.addNBTestHarness(); err != nil {
		return nil, err
	}
	if err := h.addSBTestHarness(); err != nil {
		return nil, err
	}
	return h, nil
}

// addNBTestHarness runs NB server and returns corresponding client,
// using the given cleanup to clean up the server and client
func (h *Harness) addNBTestHarness() error {
	dbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return err
	}
	h.nbServer, h.NBClient, err = h.newOVSDBTestHarness(dbModel, nbdb.Schema(), libovsdb.NewNBClientWithConfig)
	return err
}

// NewNBTestHarness runs NB server and returns corresponding harness
func NewNBTestHarness() (*Harness, error) {
	h := newHarness()
	if err := h.addNBTestHarness(); err != nil {
		return nil, err
	}
	return h, nil
}

// newSBTestHarnessWithCleanup runs SB server and returns corresponding client,
// using the given cleanup to clean up the server and client
func (h *Harness) addSBTestHarness() error {
	dbModel, err := sbdb.FullDatabaseModel()
	if err != nil {
		return err
	}
	h.sbServer, h.SBClient, err = h.newOVSDBTestHarness(dbModel, sbdb.Schema(), libovsdb.NewSBClientWithConfig)
	return err
}

// NewSBTestHarness runs SB server and returns corresponding harness
func NewSBTestHarness() (*Harness, error) {
	h := newHarness()
	if err := h.addSBTestHarness(); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Harness) Run(setup TestSetup) error {
	if h.NBClient != nil {
		if err := h.nbServer.start(setup.NBData, h.serverWg); err != nil {
			return err
		}
		if err := h.NBClient.Run(); err != nil {
			return err
		}
	} else if len(setup.NBData) > 0 {
		return fmt.Errorf("unused NB test data provided")
	}
	if h.SBClient != nil {
		if err := h.sbServer.start(setup.SBData, h.serverWg); err != nil {
			return err
		}
		if err := h.SBClient.Run(); err != nil {
			return err
		}
	} else if len(setup.SBData) > 0 {
		return fmt.Errorf("unused SB test data provided")
	}
	return nil
}

func (h *Harness) newOVSDBTestHarness(serverDBModel model.ClientDBModel, serverDBSchema ovsdb.DatabaseSchema, newClient clientBuilderFn) (*dbServer, *libovsdb.Client, error) {
	cfg := config.OvnAuthConfig{
		Scheme:  config.OvnDBSchemeUnix,
		Address: "unix:" + tempOVSDBSocketFileName(),
	}

	s, err := newOVSDBServer(cfg, serverDBModel, serverDBSchema)
	if err != nil {
		return nil, nil, err
	}

	c, err := newClient(cfg, h.clientStopCh)
	if err != nil {
		s.Close()
		return nil, nil, err
	}

	return s, c, nil
}

func updateData(db server.Database, dbModel model.ClientDBModel, schema ovsdb.DatabaseSchema, data []TestData) error {
	dbName := dbModel.Name()
	m := mapper.NewMapper(schema)
	updates := ovsdb.TableUpdates2{}
	namedUUIDs := map[string]string{}
	newData := copystructure.Must(copystructure.Copy(data)).([]TestData)

	dbMod, errs := model.NewDatabaseModel(schema, dbModel)
	if len(errs) > 0 {
		return errs[0]
	}

	for _, d := range newData {
		tableName := dbMod.FindTable(reflect.TypeOf(d))
		if tableName == "" {
			return fmt.Errorf("object of type %s is not part of the DBModel", reflect.TypeOf(d))
		}

		var dupNamedUUID string
		uuid, uuidf := getUUID(d)
		replaceUUIDs(d, func(name string, field int) string {
			uuid, ok := namedUUIDs[name]
			if !ok {
				return name
			}
			if field == uuidf {
				// if we are replacing a model uuid, it's a dupe
				dupNamedUUID = name
				return name
			}
			return uuid
		})
		if dupNamedUUID != "" {
			return fmt.Errorf("initial data contains duplicated named UUIDs %s", dupNamedUUID)
		}
		if uuid == "" {
			uuid = guuid.NewString()
		} else if !validUUID.MatchString(uuid) {
			namedUUID := uuid
			uuid = guuid.NewString()
			namedUUIDs[namedUUID] = uuid
		}

		info, err := mapper.NewInfo(tableName, schema.Table(tableName), d)
		if err != nil {
			return err
		}

		row, err := m.NewRow(info)
		if err != nil {
			return err
		}

		if _, ok := updates[tableName]; !ok {
			updates[tableName] = ovsdb.TableUpdate2{}
		}

		updates[tableName][uuid] = &ovsdb.RowUpdate2{Insert: &row}
	}

	uuid := guuid.New()
	err := db.Commit(dbName, uuid, updates)
	if err != nil {
		return fmt.Errorf("error populating server with initial data: %v", err)
	}

	return nil
}

func newOVSDBServer(cfg config.OvnAuthConfig, dbModel model.ClientDBModel, schema ovsdb.DatabaseSchema) (*dbServer, error) {
	dbs := &dbServer{
		cfg:    cfg,
		model:  dbModel,
		schema: schema,
	}

	serverDBModel, err := serverdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	serverSchema := serverdb.Schema()

	dbs.db = server.NewInMemoryDatabase(map[string]model.ClientDBModel{
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

	dbs.s, err = server.NewOvsdbServer(dbs.db, dbMod, servMod)
	if err != nil {
		return nil, err
	}

	// Populate the _Server database table
	serverData := []TestData{
		&serverdb.Database{
			Name:      dbModel.Name(),
			Connected: true,
			Leader:    true,
			Model:     serverdb.DatabaseModelClustered,
		},
	}
	if err := updateData(dbs.db, serverDBModel, serverSchema, serverData); err != nil {
		return nil, err
	}

	return dbs, nil
}

var random = rand.New(rand.NewSource(time.Now().UnixNano()))

func tempOVSDBSocketFileName() string {
	randBytes := make([]byte, 16)
	random.Read(randBytes)
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

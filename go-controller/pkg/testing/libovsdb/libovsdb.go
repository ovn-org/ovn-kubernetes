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
	"github.com/ovn-org/libovsdb/database"
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

type clientBuilderFn func(config.OvnAuthConfig, *Cleanup) (libovsdbclient.Client, error)
type serverBuilderFn func(config.OvnAuthConfig, []TestData) (*server.OvsdbServer, error)

var validUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Cleanup struct {
	clientStopCh chan struct{}
	clientWg     *sync.WaitGroup
	serverStopCh chan struct{}
	serverWg     *sync.WaitGroup
}

func newCleanup() *Cleanup {
	return &Cleanup{
		clientStopCh: make(chan struct{}),
		clientWg:     &sync.WaitGroup{},
		serverStopCh: make(chan struct{}),
		serverWg:     &sync.WaitGroup{},
	}
}

func (c *Cleanup) Cleanup() {
	// Stop the client first to ensure we don't trigger reconnect behavior
	// due to a stopped server
	close(c.clientStopCh)
	c.clientWg.Wait()
	close(c.serverStopCh)
	c.serverWg.Wait()
}

// NewNBSBTestHarness runs NB & SB OVSDB servers and returns corresponding clients
func NewNBSBTestHarness(setup TestSetup) (libovsdbclient.Client, libovsdbclient.Client, *Cleanup, error) {
	cleanup := newCleanup()

	nbClient, _, err := NewNBTestHarness(setup, cleanup)
	if err != nil {
		return nil, nil, nil, err
	}
	sbClient, _, err := NewSBTestHarness(setup, cleanup)
	if err != nil {
		return nil, nil, nil, err
	}
	return nbClient, sbClient, cleanup, nil
}

// NewNBTestHarness runs NB server and returns corresponding client
func NewNBTestHarness(setup TestSetup, cleanup *Cleanup) (libovsdbclient.Client, *Cleanup, error) {
	if cleanup == nil {
		cleanup = newCleanup()
	}

	client, err := newOVSDBTestHarness(setup.NBData, newNBServer, newNBClient, cleanup)
	if err != nil {
		return nil, nil, err
	}

	return client, cleanup, err
}

// NewSBTestHarness runs SB server and returns corresponding client
func NewSBTestHarness(setup TestSetup, cleanup *Cleanup) (libovsdbclient.Client, *Cleanup, error) {
	if cleanup == nil {
		cleanup = newCleanup()
	}

	client, err := newOVSDBTestHarness(setup.SBData, newSBServer, newSBClient, cleanup)
	if err != nil {
		return nil, nil, err
	}

	return client, cleanup, err
}

func newOVSDBTestHarness(serverData []TestData, newServer serverBuilderFn, newClient clientBuilderFn, cleanup *Cleanup) (libovsdbclient.Client, error) {
	cfg := config.OvnAuthConfig{
		Scheme:  config.OvnDBSchemeUnix,
		Address: "unix:" + tempOVSDBSocketFileName(),
	}

	s, err := newServer(cfg, serverData)
	if err != nil {
		return nil, err
	}

	c, err := newClient(cfg, cleanup)
	if err != nil {
		s.Close()
		return nil, err
	}

	cleanup.serverWg.Add(1)
	go func() {
		defer cleanup.serverWg.Done()
		<-cleanup.serverStopCh
		s.Close()
	}()

	return c, nil
}

func newNBClient(cfg config.OvnAuthConfig, cleanup *Cleanup) (libovsdbclient.Client, error) {
	stopChan := make(chan struct{})
	libovsdbOvnNBClient, err := libovsdb.NewNBClientWithConfig(cfg, stopChan)
	if err != nil {
		return nil, err
	}
	cleanup.clientWg.Add(1)
	go func() {
		defer cleanup.clientWg.Done()
		<-cleanup.clientStopCh
		close(stopChan)
		libovsdbOvnNBClient.Close()
	}()
	return libovsdbOvnNBClient, err
}

func newSBClient(cfg config.OvnAuthConfig, cleanup *Cleanup) (libovsdbclient.Client, error) {
	stopChan := make(chan struct{})
	libovsdbOvnSBClient, err := libovsdb.NewSBClientWithConfig(cfg, stopChan)
	if err != nil {
		return nil, err
	}
	cleanup.clientWg.Add(1)
	go func() {
		defer cleanup.clientWg.Done()
		<-cleanup.clientStopCh
		close(stopChan)
		libovsdbOvnSBClient.Close()
	}()
	return libovsdbOvnSBClient, err
}

func newSBServer(cfg config.OvnAuthConfig, data []TestData) (*server.OvsdbServer, error) {
	dbModel, err := sbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	schema := sbdb.Schema()
	return newOVSDBServer(cfg, dbModel, schema, data)
}

func newNBServer(cfg config.OvnAuthConfig, data []TestData) (*server.OvsdbServer, error) {
	dbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	schema := nbdb.Schema()
	return newOVSDBServer(cfg, dbModel, schema, data)
}

func updateData(db database.Database, dbModel model.ClientDBModel, schema ovsdb.DatabaseSchema, data []TestData) error {
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

func newOVSDBServer(cfg config.OvnAuthConfig, dbModel model.ClientDBModel, schema ovsdb.DatabaseSchema, data []TestData) (*server.OvsdbServer, error) {
	serverDBModel, err := serverdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	serverSchema := serverdb.Schema()

	db := database.NewInMemoryDatabase(map[string]model.ClientDBModel{
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
	sid := fmt.Sprintf("%04x", rand.Uint32())
	serverData := []TestData{
		&serverdb.Database{
			Name:      dbModel.Name(),
			Connected: true,
			Leader:    true,
			Model:     serverdb.DatabaseModelClustered,
			Sid:       &sid,
		},
	}
	if err := updateData(db, serverDBModel, serverSchema, serverData); err != nil {
		return nil, err
	}

	// Populate with testcase data
	if len(data) > 0 {
		if err := updateData(db, dbModel, schema, data); err != nil {
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

	err = wait.Poll(100*time.Millisecond, 500*time.Millisecond, func() (bool, error) { return s.Ready(), nil })
	if err != nil {
		s.Close()
		return nil, err
	}

	return s, nil
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

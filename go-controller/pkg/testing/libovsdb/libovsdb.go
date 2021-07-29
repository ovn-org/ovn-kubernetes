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
	"time"

	"github.com/alexflint/go-filemutex"
	guuid "github.com/google/uuid"
	"github.com/mitchellh/copystructure"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/mapper"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/libovsdb/server"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type TestSetup struct {
	NBData []TestData
	SBData []TestData
}

type TestData interface{}

type clientBuilderFn func(config.OvnAuthConfig, chan struct{}) (libovsdbclient.Client, error)
type serverBuilderFn func(config.OvnAuthConfig, []TestData) (*server.OvsdbServer, error)

var validUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NewNBSBTestHarness runs NB & SB OVSDB servers and returns corresponding clients
func NewNBSBTestHarness(setup TestSetup, stopChan chan struct{}) (libovsdbclient.Client, libovsdbclient.Client, error) {
	nbClient, err := NewNBTestHarness(setup, stopChan)
	if err != nil {
		return nil, nil, err
	}
	sbClient, err := NewSBTestHarness(setup, stopChan)
	if err != nil {
		return nil, nil, err
	}
	return nbClient, sbClient, nil
}

// NewNBTestHarness runs NB server and returns corresponding client
func NewNBTestHarness(setup TestSetup, stopChan chan struct{}) (libovsdbclient.Client, error) {
	return newOVSDBTestHarness(setup.NBData, stopChan, newNBServer, newNBClient)
}

// NewSBTestHarness runs SB server and returns corresponding client
func NewSBTestHarness(setup TestSetup, stopChan chan struct{}) (libovsdbclient.Client, error) {
	return newOVSDBTestHarness(setup.SBData, stopChan, newSBServer, newSBClient)
}

func newOVSDBTestHarness(serverData []TestData, stopChan chan struct{}, newServer serverBuilderFn, newClient clientBuilderFn) (libovsdbclient.Client, error) {
	cfg := config.OvnAuthConfig{
		Scheme:  config.OvnDBSchemeUnix,
		Address: "unix:" + tempOVSDBSocketFileName(),
	}

	s, err := newServer(cfg, serverData)
	if err != nil {
		return nil, err
	}

	internalStopChan := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopChan:
				s.Close()
				return
			case <-internalStopChan:
				s.Close()
				return
			}
		}
	}()

	c, err := newClient(cfg, stopChan)
	if err != nil {
		close(internalStopChan)
	}

	return c, err
}

func newNBClient(cfg config.OvnAuthConfig, stopChan chan struct{}) (libovsdbclient.Client, error) {
	libovsdbOvnNBClient, err := util.NewNBClientWithConfig(cfg, stopChan)
	if err != nil {
		return nil, err
	}
	return libovsdbOvnNBClient, err
}

func newSBClient(cfg config.OvnAuthConfig, stopChan chan struct{}) (libovsdbclient.Client, error) {
	libovsdbOvnSBClient, err := util.NewSBClientWithConfig(cfg, stopChan)
	if err != nil {
		return nil, err
	}
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

func newOVSDBServer(cfg config.OvnAuthConfig, dbModel *model.DBModel, schema ovsdb.DatabaseSchema, data []TestData) (*server.OvsdbServer, error) {
	db := server.NewInMemoryDatabase(map[string]*model.DBModel{
		schema.Name: dbModel,
	})
	s, err := server.NewOvsdbServer(db, server.DatabaseModel{
		Model:  dbModel,
		Schema: &schema,
	})
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		dbName := dbModel.Name()
		m := mapper.NewMapper(&schema)
		updates := ovsdb.TableUpdates{}
		namedUUIDs := map[string]string{}
		data := copystructure.Must(copystructure.Copy(data)).([]TestData)
		for _, d := range data {
			tableName := dbModel.FindTable(reflect.TypeOf(d))
			if tableName == "" {
				return nil, fmt.Errorf("object of type %s is not part of the DBModel", reflect.TypeOf(d))
			}

			replaceUUIDs(d, func(s string, i int) string {
				uuid, ok := namedUUIDs[s]
				if !ok {
					return s
				}
				return uuid
			})
			uuid := getUUID(d)
			if !validUUID.MatchString(uuid) {
				namedUUID := uuid
				uuid = guuid.NewString()
				namedUUIDs[namedUUID] = uuid
			}

			row, err := m.NewRow(tableName, d)
			if err != nil {
				return nil, err
			}

			if _, ok := updates[tableName]; !ok {
				updates[tableName] = ovsdb.TableUpdate{}
			}

			updates[tableName][uuid] = &ovsdb.RowUpdate{New: &row}
		}

		err := db.Commit(dbName, updates)
		if err != nil {
			return nil, fmt.Errorf("error populating server with initial data: %v", err)
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
		for _, uuid := range table.Rows() {
			row := table.Row(uuid)
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
func getUUID(x TestData) string {
	v := reflect.ValueOf(x)
	if v.Kind() != reflect.Ptr {
		return ""
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return ""
	}
	for i, n := 0, v.NumField(); i < n; i++ {
		if tag := v.Type().Field(i).Tag.Get("ovsdb"); tag == "_uuid" {
			return v.Field(i).String()
		}
	}
	return ""
}

package libovsdb

import (
	"encoding/hex"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/alexflint/go-filemutex"
	"github.com/google/uuid"
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
		for _, d := range data {
			tableName := dbModel.FindTable(reflect.TypeOf(d))
			if tableName == "" {
				return nil, fmt.Errorf("object of type %s is not part of the DBModel", reflect.TypeOf(m))
			}
			row, err := m.NewRow(tableName, d)
			if err != nil {
				return nil, err
			}
			res, _ := db.Insert(dbName, tableName, uuid.NewString(), row)
			if res.Error != "" {
				return nil, fmt.Errorf("%s: %s", res.Error, res.Details)
			}
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

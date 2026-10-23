// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package libovsdb

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/natefinch/lumberjack.v2"

	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func newClientLogger(dbModelName string) (logger logr.Logger, err error) {
	logggerFilename := config.Logging.LibovsdbFile
	if len(logggerFilename) == 0 {
		// Not using a separate log file for libovsdb client
		config := textlogger.NewConfig()
		logger = textlogger.NewLogger(config)
		return logger, nil
	}

	// Make sure logger file can be opened and created with the right perms
	// Ref: https://github.com/natefinch/lumberjack/issues/82#issuecomment-482143273
	err = os.MkdirAll(filepath.Dir(logggerFilename), 0755)
	if err != nil {
		return logger, fmt.Errorf("making directories for logger file %s for libovsdb failed: %w", logggerFilename, err)
	}
	checkFile, err := os.OpenFile(logggerFilename, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0640)
	if err != nil {
		return logger, fmt.Errorf("opening logger file %s for libovsdb failed: %w", logggerFilename, err)
	}
	_ = checkFile.Close()

	// Create the lumberjack logger, which will write to a rolling log file.
	ll := &lumberjack.Logger{
		Filename:   logggerFilename,
		MaxSize:    config.Logging.LogFileMaxSize, // MB
		MaxBackups: config.Logging.LogFileMaxBackups,
		MaxAge:     config.Logging.LogFileMaxAge, // Days
		Compress:   true,
	}
	klog.Infof("Client for %s using log verbosity %d with lumberjack %#v", dbModelName, config.Logging.Level, ll)
	clientLog := log.New(ll, "", log.Ldate|log.Ltime|log.Lshortfile)
	_ = stdr.SetVerbosity(config.Logging.Level)
	logger = stdr.New(clientLog)
	return logger, nil
}

// newClient creates a new client object connecting to the given unix-socket
// endpoint (e.g. "unix:/var/run/ovn/ovnnb_db.sock").
func newClient(endpoint string, dbModel model.ClientDBModel, opts ...client.Option) (client.Client, error) {
	const connectTimeout time.Duration = types.OVSDBTimeout * 2
	const inactivityTimeout time.Duration = types.OVSDBTimeout * 18
	logger, err := newClientLogger(dbModel.Name())
	if err != nil {
		return nil, err
	}
	options := []client.Option{
		// Reading and parsing the DB after reconnect at scale can (unsurprisingly)
		// take longer than a normal ovsdb operation. Give it a bit more time so
		// we don't time out and enter a reconnect loop. In addition it also enables
		// inactivity check on the ovsdb connection.
		client.WithInactivityCheck(inactivityTimeout, connectTimeout, &backoff.ZeroBackOff{}),
		client.WithLogger(&logger),
		client.WithEndpoint(endpoint),
	}
	options = append(options, opts...)

	c, err := client.NewOVSDBClient(dbModel, options...)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		return nil, err
	}

	return c, nil
}

// NewSBClient creates a new OVN Southbound Database client connected to the
// local OVN SB DB.
func NewSBClient(stopCh <-chan struct{}) (client.Client, error) {
	return NewSBClientWithEndpoint(config.OvnSouth.GetURL(), prometheus.DefaultRegisterer, stopCh)
}

// NewSBClientWithEndpoint creates a new OVN Southbound Database client connected
// to the given unix-socket endpoint (e.g. "unix:/var/run/ovn/ovnsb_db.sock").
func NewSBClientWithEndpoint(endpoint string, promRegistry prometheus.Registerer, stopCh <-chan struct{}) (client.Client, error) {
	dbModel, err := sbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}

	enableMetricsOption := client.WithMetricsRegistryNamespaceSubsystem(promRegistry,
		"ovnkube", "master_libovsdb")

	if config.Gateway.DisableUDNARPNDPFlood {
		// used by the MAC binding controller
		dbModel.SetIndexes(map[string][]model.ClientIndex{
			sbdb.MACBindingTable: {{Columns: []model.ColumnKey{{Column: "logical_port"}}}},
		})
	}

	c, err := newClient(endpoint, dbModel, enableMetricsOption)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout*2)
	go func() {
		<-stopCh
		cancel()
		c.Close()
	}()

	// Only Monitor Required SBDB tables to reduce memory overhead
	chassisPrivate := sbdb.ChassisPrivate{}
	igmpGroup := sbdb.IGMPGroup{}
	monitors := []client.MonitorOption{
		// used by unidling controller
		client.WithTable(&sbdb.ControllerEvent{}),
		// used by node sync
		client.WithTable(&sbdb.Chassis{}),
		// used by zone interconnect
		client.WithTable(&sbdb.Encap{}),
		// used by node sync, only interested in names
		client.WithTable(&chassisPrivate, &chassisPrivate.Name),
		// used by node sync, only interested in Chassis reference
		client.WithTable(&igmpGroup, &igmpGroup.Chassis),
		// used for metrics
		client.WithTable(&sbdb.SBGlobal{}),
		// used for metrics
		client.WithTable(&sbdb.PortBinding{}),
	}
	if config.Gateway.DisableUDNARPNDPFlood {
		// used by the MAC binding controller
		monitors = append(monitors, client.WithTable(&sbdb.MACBinding{}))
	}
	_, err = c.Monitor(ctx, c.NewMonitor(monitors...))
	if err != nil {
		cancel()
		c.Close()
		return nil, err
	}

	return c, nil
}

// NewNBClient creates a new OVN Northbound Database client connected to the
// local OVN NB DB.
func NewNBClient(stopCh <-chan struct{}) (client.Client, error) {
	return NewNBClientWithEndpoint(config.OvnNorth.GetURL(), prometheus.DefaultRegisterer, stopCh)
}

// NewNBClientWithEndpoint creates a new OVN Northbound Database client connected
// to the given unix-socket endpoint (e.g. "unix:/var/run/ovn/ovnnb_db.sock").
func NewNBClientWithEndpoint(endpoint string, promRegistry prometheus.Registerer, stopCh <-chan struct{}) (client.Client, error) {
	dbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}

	enableMetricsOption := client.WithMetricsRegistryNamespaceSubsystem(promRegistry, "ovnkube",
		"master_libovsdb")

	// define client indexes for objects that are using dbIDs
	indexes := map[string][]model.ClientIndex{
		nbdb.ACLTable:           {{Columns: []model.ColumnKey{{Column: "external_ids", Key: types.PrimaryIDKey}}}},
		nbdb.DHCPOptionsTable:   {{Columns: []model.ColumnKey{{Column: "external_ids", Key: types.PrimaryIDKey}}}},
		nbdb.LoadBalancerTable:  {{Columns: []model.ColumnKey{{Column: "name"}}}},
		nbdb.LogicalSwitchTable: {{Columns: []model.ColumnKey{{Column: "name"}}}},
		nbdb.LogicalRouterTable: {{Columns: []model.ColumnKey{{Column: "name"}}}},
		nbdb.QoSTable:           {{Columns: []model.ColumnKey{{Column: "external_ids", Key: types.PrimaryIDKey}}}},
	}
	if config.Gateway.DisableUDNARPNDPFlood {
		// used by the MAC binding controller
		indexes[nbdb.StaticMACBindingTable] = []model.ClientIndex{{Columns: []model.ColumnKey{{Column: "logical_port"}}}}
	}
	dbModel.SetIndexes(indexes)

	c, err := newClient(endpoint, dbModel, enableMetricsOption)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout*2)
	go func() {
		<-stopCh
		cancel()
		c.Close()
	}()

	_, err = c.MonitorAll(ctx)
	if err != nil {
		cancel()
		c.Close()
		return nil, err
	}

	return c, nil
}

// NewOVSClient creates a new openvswitch Database client
func NewOVSClient(stopCh <-chan struct{}) (client.Client, error) {
	endpoint := fmt.Sprintf("unix:%s", filepath.Join(config.OvsPaths.RunDir, "db.sock"))
	return NewOVSClientWithEndpoint(endpoint, stopCh)
}

// NewOVSClientWithEndpoint connects to the OVS DB at the given unix-socket
// endpoint (e.g. "unix:/var/run/openvswitch/db.sock").
func NewOVSClientWithEndpoint(endpoint string, stopCh <-chan struct{}) (client.Client, error) {
	dbModel, err := vswitchd.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	c, err := newClient(endpoint, dbModel)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	go func() {
		<-stopCh
		cancel()
		c.Close()
	}()

	_, err = c.Monitor(ctx,
		c.NewMonitor(
			client.WithTable(&vswitchd.OpenvSwitch{}),
			client.WithTable(&vswitchd.Bridge{}),
			client.WithTable(&vswitchd.Port{}),
			client.WithTable(&vswitchd.Interface{}),
		),
	)
	if err != nil {
		cancel()
		c.Close()
		return nil, err
	}

	return c, nil
}

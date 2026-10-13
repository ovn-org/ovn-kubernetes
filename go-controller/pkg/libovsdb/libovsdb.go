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
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

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

// stoppableClient aborts transactions as soon as the client is stopped.
//
// libovsdb's Transact blocks in a reconnect-wait loop whenever the client is
// not connected, and that loop only exits on ctx.Done(). After Close() the
// client can never reconnect, so without this every in-flight and subsequent
// transaction burns its full OVSDBTxnTimeout before failing, which is what
// turns any init failure into a multi-minute zombie process.
//
// The client's own shutdown flag is not usable for this: it is reset to false
// on the disconnect notification that Close() triggers, and Close() skips
// setting it entirely when the client is already disconnected.
type stoppableClient struct {
	client.Client
	stopCtx context.Context
}

func (c *stoppableClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	txnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Fires immediately if stopCtx is already done, so this covers both
	// transactions already blocked in the wait loop and ones started after the
	// client was stopped. The resulting error is not client.ErrNotConnected, so
	// TransactWithRetry gives up instead of polling until the deadline.
	stopWatch := context.AfterFunc(c.stopCtx, cancel)
	defer stopWatch()
	return c.Client.Transact(txnCtx, ops...)
}

// closeOnStop closes c when stopCh closes, cancelling the monitor-setup context
// first. The returned context is cancelled at the same moment; stoppableClient
// uses it to abort transactions.
func closeOnStop(c client.Client, stopCh <-chan struct{}, monitorCancel context.CancelFunc) context.Context {
	stopCtx, stopCancel := context.WithCancel(context.Background())
	go func() {
		<-stopCh
		stopCancel()
		monitorCancel()
		c.Close()
	}()
	return stopCtx
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

	c, err := newClient(endpoint, dbModel, enableMetricsOption)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout*2)
	stopCtx := closeOnStop(c, stopCh, cancel)

	// Only Monitor Required SBDB tables to reduce memory overhead
	chassisPrivate := sbdb.ChassisPrivate{}
	igmpGroup := sbdb.IGMPGroup{}
	_, err = c.Monitor(ctx,
		c.NewMonitor(
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
		),
	)
	if err != nil {
		cancel()
		c.Close()
		return nil, err
	}

	return &stoppableClient{Client: c, stopCtx: stopCtx}, nil
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
	dbModel.SetIndexes(map[string][]model.ClientIndex{
		nbdb.ACLTable:           {{Columns: []model.ColumnKey{{Column: "external_ids", Key: types.PrimaryIDKey}}}},
		nbdb.DHCPOptionsTable:   {{Columns: []model.ColumnKey{{Column: "external_ids", Key: types.PrimaryIDKey}}}},
		nbdb.LoadBalancerTable:  {{Columns: []model.ColumnKey{{Column: "name"}}}},
		nbdb.LogicalSwitchTable: {{Columns: []model.ColumnKey{{Column: "name"}}}},
		nbdb.LogicalRouterTable: {{Columns: []model.ColumnKey{{Column: "name"}}}},
		nbdb.QoSTable:           {{Columns: []model.ColumnKey{{Column: "external_ids", Key: types.PrimaryIDKey}}}},
	})

	c, err := newClient(endpoint, dbModel, enableMetricsOption)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout*2)
	stopCtx := closeOnStop(c, stopCh, cancel)

	_, err = c.MonitorAll(ctx)
	if err != nil {
		cancel()
		c.Close()
		return nil, err
	}

	return &stoppableClient{Client: c, stopCtx: stopCtx}, nil
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
	stopCtx := closeOnStop(c, stopCh, cancel)

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

	return &stoppableClient{Client: c, stopCtx: stopCtx}, nil
}

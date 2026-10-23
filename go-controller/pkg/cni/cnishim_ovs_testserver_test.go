// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ovn-kubernetes/libovsdb/cache"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// Set OVSDB_TEST_BIN_DIR to a directory containing ovsdb-tool and ovsdb-server
// to verify actual monitor projection. The in-memory server ignores columns
// and conditions in the initial monitor response. Real servers use a private
// temporary database/socket, never the host's OVS database.
func newShimOVSTestServer(tb testing.TB, data []libovsdbtest.TestData) (client.Client, bool) {
	tb.Helper()
	binDir := os.Getenv("OVSDB_TEST_BIN_DIR")
	if binDir == "" {
		c, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: data})
		require.NoError(tb, err)
		tb.Cleanup(testCtx.Cleanup)
		return c, false
	}
	// Avoid exceeding the Unix socket path limit with long subtest names.
	dir, err := os.MkdirTemp("", "shim-ovs-")
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, os.RemoveAll(dir)) })
	schema, err := json.Marshal(vswitchd.Schema())
	require.NoError(tb, err)
	schemaPath, dbPath := filepath.Join(dir, "schema"), filepath.Join(dir, "db")
	require.NoError(tb, os.WriteFile(schemaPath, schema, 0600))
	output, err := exec.Command(filepath.Join(binDir, "ovsdb-tool"), "create", dbPath, schemaPath).CombinedOutput()
	require.NoError(tb, err, "%s", output)
	logFile, err := os.Create(filepath.Join(dir, "server.log"))
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = logFile.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	socket := filepath.Join(dir, "db.sock")
	server := exec.CommandContext(ctx, filepath.Join(binDir, "ovsdb-server"),
		"--remote=punix:"+socket, "--unixctl="+filepath.Join(dir, "control.sock"), "--no-chdir", dbPath)
	server.Stdout, server.Stderr = logFile, logFile
	if err := server.Start(); err != nil {
		cancel()
		tb.Fatalf("start isolated ovsdb-server: %v", err)
	}
	tb.Cleanup(func() {
		cancel()
		_ = server.Wait()
	})
	require.Eventually(tb, func() bool {
		_, err := os.Stat(socket)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	stopCh := make(chan struct{})
	tb.Cleanup(func() { close(stopCh) })
	c, err := libovsdb.NewOVSClientWithEndpoint("unix:"+socket, stopCh)
	require.NoError(tb, err)
	tb.Cleanup(c.Close)
	var ops []ovsdb.Operation
	for _, row := range data {
		insert, err := c.Create(row)
		require.NoError(tb, err)
		ops = append(ops, insert...)
	}
	_, err = ovsops.TransactAndCheck(c, ops)
	require.NoError(tb, err)
	return c, true
}

// emulateShimOVSReadiness substitutes ovn-controller's readiness marker;
// it does not emulate a working datapath.
func emulateShimOVSReadiness(tb testing.TB, c client.Client) {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	changes := make(chan struct{}, 1)
	notify := func(table string, row model.Model) {
		if table != vswitchd.InterfaceTable {
			return
		}
		iface := row.(*vswitchd.Interface)
		if iface.ExternalIDs["iface-id"] == "" || iface.ExternalIDs["ovn-installed"] == "true" {
			return
		}
		select {
		case changes <- struct{}{}:
		default:
		}
	}
	c.Cache().AddEventHandler(&cache.EventHandlerFuncs{
		AddFunc: notify,
		UpdateFunc: func(table string, _, row model.Model) {
			notify(table, row)
		},
	})
	select {
	case changes <- struct{}{}:
	default:
	}
	tb.Cleanup(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-changes:
			}
			var ops []ovsdb.Operation
			for _, row := range c.Cache().Table(vswitchd.InterfaceTable).Rows() {
				iface := row.(*vswitchd.Interface)
				if iface.ExternalIDs["iface-id"] == "" || iface.ExternalIDs["ovn-installed"] == "true" {
					continue
				}
				ready, err := c.Where(&vswitchd.Interface{UUID: iface.UUID}).Mutate(iface, model.Mutation{
					Field: &iface.ExternalIDs, Mutator: ovsdb.MutateOperationInsert,
					Value: map[string]string{"ovn-installed": "true"},
				})
				if err != nil {
					tb.Errorf("build readiness update: %v", err)
					return
				}
				ops = append(ops, ready...)
			}
			_, err := ovsops.TransactAndCheck(c, ops)
			if err != nil {
				tb.Errorf("set interface readiness: %v", err)
				return
			}
		}
	}()
}

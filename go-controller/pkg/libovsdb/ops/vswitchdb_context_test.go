// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	libovsdbmodel "github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// Block a real client's lookup, transaction, or acknowledgement read so the
// test controls exactly which phase is in flight when the caller cancels.
type blockingVSwitchdClient struct {
	libovsdbclient.Client
	phase    string
	entered  chan struct{}
	release  chan struct{}
	deadline time.Time
}

func (c *blockingVSwitchdClient) block(ctx context.Context) error {
	c.deadline, _ = ctx.Deadline()
	close(c.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.release:
		return errors.New("test released blocked OVS operation")
	}
}

func (c *blockingVSwitchdClient) List(ctx context.Context, result interface{}) error {
	if c.phase == "lookup" {
		return c.block(ctx)
	}
	return c.Client.List(ctx, result)
}

func (c *blockingVSwitchdClient) Transact(ctx context.Context, operations ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	if c.phase == "transaction" {
		return nil, c.block(ctx)
	}
	return c.Client.Transact(ctx, operations...)
}

func (c *blockingVSwitchdClient) Get(ctx context.Context, model libovsdbmodel.Model) error {
	if c.phase == "wait" {
		return c.block(ctx)
	}
	return c.Client.Get(ctx, model)
}

func TestTransactAndCheckAndWaitForVSwitchdCancellation(t *testing.T) {
	for _, phase := range []string{"lookup", "transaction", "wait"} {
		t.Run(phase, func(t *testing.T) {
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", NextCfg: 7, CurCfg: 7},
			}})
			if err != nil {
				t.Fatalf("harness setup: %v", err)
			}
			t.Cleanup(cleanup.Cleanup)
			ovs, err := GetOpenvSwitch(ovsClient)
			if err != nil {
				t.Fatalf("get Open_vSwitch: %v", err)
			}
			update := &vswitchd.OpenvSwitch{UUID: ovs.UUID, ExternalIDs: map[string]string{"updated": "true"}}
			operations, err := ovsClient.Where(update).Update(update, &update.ExternalIDs)
			if err != nil {
				t.Fatalf("build update: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			blocked := &blockingVSwitchdClient{
				Client: ovsClient, phase: phase, entered: make(chan struct{}), release: make(chan struct{}),
			}
			done := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				done <- TransactAndCheckAndWaitForVSwitchd(ctx, blocked, operations)
			}()
			t.Cleanup(func() {
				cancel()
				close(blocked.release)
				<-finished
			})
			select {
			case <-blocked.entered:
			case err := <-done:
				t.Fatalf("operation returned before reaching %s: %v", phase, err)
			case <-time.After(types.OVSDBTimeout):
				t.Fatalf("operation did not reach %s", phase)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation during %s returned %v, want context.Canceled", phase, err)
				}
			case <-time.After(types.OVSDBTimeout / 2):
				t.Fatalf("cancellation did not interrupt %s before the helper's timeout", phase)
			}

			ovs, err = GetOpenvSwitch(ovsClient)
			if err != nil {
				t.Fatalf("read state after cancellation: %v", err)
			}
			if phase == "wait" {
				if ovs.NextCfg != 8 || ovs.CurCfg != 7 || ovs.ExternalIDs["updated"] != "true" {
					t.Fatalf("cancellation while waiting must retain the committed update: %+v", ovs)
				}
			} else if ovs.NextCfg != 7 || len(ovs.ExternalIDs) != 0 {
				t.Fatalf("cancellation before submission changed the database: %+v", ovs)
			}
		})
	}
}

func TestTransactAndCheckAndWaitForVSwitchdCanceledBeforeSubmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A nil client makes any attempt to use OVS after cancellation fail the test.
	err := TransactAndCheckAndWaitForVSwitchd(ctx, nil, []ovsdb.Operation{{Op: ovsdb.OperationComment}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled caller returned %v, want context.Canceled", err)
	}
}

func TestTransactAndCheckAndWaitForVSwitchdCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	blocked := &blockingVSwitchdClient{
		phase: "lookup", entered: make(chan struct{}), release: make(chan struct{}),
	}
	t.Cleanup(func() { close(blocked.release) })
	err := TransactAndCheckAndWaitForVSwitchd(ctx, blocked, []ovsdb.Operation{{Op: ovsdb.OperationComment}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline returned %v, want context.DeadlineExceeded", err)
	}
	deadline, _ := ctx.Deadline()
	if !blocked.deadline.Equal(deadline) {
		t.Fatalf("lookup deadline %v did not preserve caller deadline %v", blocked.deadline, deadline)
	}
}

type acknowledgementClient struct {
	libovsdbclient.Client
	transactions int
	get          func(context.Context, *vswitchd.OpenvSwitch) error
}

func (c *acknowledgementClient) List(_ context.Context, result any) error {
	*result.(*[]*vswitchd.OpenvSwitch) = []*vswitchd.OpenvSwitch{{UUID: "root-ovs", NextCfg: 7, CurCfg: 7}}
	return nil
}

func (c *acknowledgementClient) Transact(_ context.Context, operations ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	c.transactions++
	results := make([]ovsdb.OperationResult, len(operations))
	// Another writer can advance next_cfg between the initial lookup and our
	// transaction. The wait must use the transaction's value, not cached 7 + 1.
	results[len(results)-1].Rows = []ovsdb.Row{{"next_cfg": float64(9)}}
	return results, nil
}

func (c *acknowledgementClient) Get(ctx context.Context, row libovsdbmodel.Model) error {
	return c.get(ctx, row.(*vswitchd.OpenvSwitch))
}

func TestVSwitchdAcknowledgementPolling(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		ackAfter, cancelAfter, deadline, wantElapsed time.Duration
		ackValue                                     int
		readErr, wantErr                             error
	}{
		{name: "already acknowledged", ackValue: 9},
		{name: "later generation acknowledged", ackValue: 10},
		{name: "fast acknowledgement", ackAfter: 3 * time.Millisecond, ackValue: 9, wantElapsed: 6 * time.Millisecond},
		{name: "slow acknowledgement", ackAfter: 350 * time.Millisecond, ackValue: 9, wantElapsed: 426 * time.Millisecond},
		{name: "caller deadline", ackAfter: -1, deadline: 325 * time.Millisecond,
			wantElapsed: 325 * time.Millisecond, wantErr: context.DeadlineExceeded},
		{name: "cancel during backoff", ackAfter: -1, cancelAfter: 100 * time.Millisecond,
			wantElapsed: 100 * time.Millisecond, wantErr: context.Canceled},
		{name: "internal timeout", ackAfter: -1, wantElapsed: types.OVSDBTimeout, wantErr: context.DeadlineExceeded},
		{name: "cache error", readErr: libovsdbclient.ErrNotFound, wantErr: libovsdbclient.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.deadline != 0 {
					var cancelDeadline context.CancelFunc
					ctx, cancelDeadline = context.WithTimeout(ctx, tc.deadline)
					defer cancelDeadline()
				}
				if tc.cancelAfter != 0 {
					go func() {
						time.Sleep(tc.cancelAfter)
						cancel()
					}()
				}
				var reads []time.Duration
				c := &acknowledgementClient{get: func(readCtx context.Context, row *vswitchd.OpenvSwitch) error {
					reads = append(reads, time.Since(start))
					require.Equal(t, "root-ovs", row.UUID)
					deadline, ok := readCtx.Deadline()
					require.True(t, ok)
					wantDeadline := types.OVSDBTimeout
					if tc.deadline != 0 {
						wantDeadline = tc.deadline
					}
					require.Equal(t, start.Add(wantDeadline), deadline)
					row.CurCfg = 8
					if tc.ackAfter >= 0 && time.Since(start) >= tc.ackAfter {
						row.CurCfg = tc.ackValue
					}
					return tc.readErr
				}}
				err := TransactAndCheckAndWaitForVSwitchd(ctx, c, []ovsdb.Operation{{Op: ovsdb.OperationComment}})
				if tc.wantErr != nil {
					require.ErrorIs(t, err, tc.wantErr)
				} else {
					require.NoError(t, err)
				}
				require.Equal(t, tc.wantElapsed, time.Since(start))
				require.Equal(t, 1, c.transactions, "polling must not resubmit the transaction")
				if tc.name == "internal timeout" {
					require.LessOrEqual(t, len(reads), 110, "long waits must back off their cache polling")
					require.Equal(t, 100*time.Millisecond, reads[len(reads)-1]-reads[len(reads)-2])
				}
			})
		})
	}
}

// BenchmarkVSwitchdAcknowledgement measures a transaction plus its cache wait.
// The database is real protocol traffic to the in-memory test server, but
// ovs-vswitchd acknowledgement is emulated on a 10ms tick, not a real datapath.
func BenchmarkVSwitchdAcknowledgement(b *testing.B) {
	c, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root-ovs"},
	}})
	require.NoError(b, err)
	b.Cleanup(cleanup.Cleanup)
	b.Cleanup(libovsdbtest.EmulateVSwitchdConfig(c))
	ctx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)
	root, err := GetOpenvSwitch(c)
	require.NoError(b, err)
	update := &vswitchd.OpenvSwitch{UUID: root.UUID, ExternalIDs: map[string]string{"benchmark": "acknowledgement"}}
	operations, err := c.Where(update).Update(update, &update.ExternalIDs)
	require.NoError(b, err)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		require.NoError(b, TransactAndCheckAndWaitForVSwitchd(ctx, c, operations))
	}
}

type blockedVSwitchdClient struct {
	libovsdbclient.Client
	started  chan struct{}
	finished chan struct{}
}

func (c *blockedVSwitchdClient) Transact(ctx context.Context, _ ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	close(c.started)
	<-ctx.Done()
	close(c.finished)
	return nil, ctx.Err()
}

func TestEmulateVSwitchdConfigShutdown(t *testing.T) {
	ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{&vswitchd.OpenvSwitch{UUID: "root", NextCfg: 1}},
	})
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	client := &blockedVSwitchdClient{
		Client:   ovsClient,
		started:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	stop := libovsdbtest.EmulateVSwitchdConfig(client)
	t.Cleanup(stop)
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("emulator did not start a transaction")
	}

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("emulator did not stop an in-flight transaction")
	}
	select {
	case <-client.finished:
	default:
		t.Fatal("stop returned before the transaction finished")
	}
}

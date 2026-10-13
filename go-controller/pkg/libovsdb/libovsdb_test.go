// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package libovsdb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// blockingClient mimics the part of libovsdb's Transact that causes the
// problem: when the client is not connected it parks in a reconnect-wait loop
// that only ever exits on ctx.Done(). After Close() it can never reconnect, so
// the only way out is for the context to be cancelled.
//
// Only Transact and Close are reachable from this test; the embedded nil
// interface is enough to satisfy client.Client.
type blockingClient struct {
	client.Client
	closed  chan struct{}
	entered chan struct{} // non-nil iff the test wants entry synchronization
}

func (b *blockingClient) Transact(ctx context.Context, _ ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	if b.entered != nil {
		select {
		case b.entered <- struct{}{}:
		default:
		}
	}
	<-ctx.Done()
	return nil, fmt.Errorf("%w: while awaiting reconnection", ctx.Err())
}

func (b *blockingClient) Close() { close(b.closed) }

// TestStoppableClientAbortsTransactOnStop pins the fix for the shutdown death
// spiral: transactions must fail as soon as the client is stopped rather than
// waiting out OVSDBTxnTimeout. Without stoppableClient these transactions never
// return and the deadlines below trip.
func TestStoppableClientAbortsTransactOnStop(t *testing.T) {
	// Single-close wrapper: explicit call and t.Cleanup can both fire safely.
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }
	t.Cleanup(stop)

	stub := &blockingClient{closed: make(chan struct{}), entered: make(chan struct{}, 1)}
	c := &stoppableClient{Client: stub, stopCtx: closeOnStop(stub, stopCh, func() {})}

	// A transaction already blocked in the wait loop when the client stops.
	// Cancellable context so the goroutine can be cleaned up if the test fails early.
	ctx1, cancel1 := context.WithCancel(context.Background())
	t.Cleanup(cancel1)
	inflight := make(chan error, 1)
	go func() {
		_, err := c.Transact(ctx1)
		inflight <- err
	}()
	// Wait until the goroutine has actually entered Transact so this exercises
	// the in-flight case and not the already-stopped one.
	select {
	case <-stub.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("transaction did not enter Transact within 5s")
	}

	// Assertions run before t.Cleanup so cancellation cannot mask a
	// transaction-cancellation regression.
	stop()

	select {
	case err := <-inflight:
		if err == nil {
			t.Fatal("in-flight transaction returned no error after the client stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight transaction was not aborted when the client stopped")
	}

	// A transaction started after the client has already stopped.
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	after := make(chan error, 1)
	go func() {
		_, err := c.Transact(ctx2)
		after <- err
	}()

	select {
	case err := <-after:
		if err == nil {
			t.Fatal("transaction started after stop returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transaction started after stop was not aborted")
	}

	select {
	case <-stub.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closeOnStop did not close the underlying client")
	}
}

// TestStoppableClientPassesThroughWhenRunning guards against the wrapper
// cancelling transactions that should be allowed to run.
func TestStoppableClientPassesThroughWhenRunning(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	stub := &blockingClient{closed: make(chan struct{})}
	c := &stoppableClient{Client: stub, stopCtx: closeOnStop(stub, stopCh, func() {})}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()

	// Must be the caller's deadline that surfaces, not a cancellation from the
	// wrapper.
	if _, err := c.Transact(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the caller's own deadline to surface, got %v", err)
	}
	// The wrapper must not shorten the caller's deadline.
	if now := time.Now(); now.Before(deadline) {
		t.Fatalf("transaction aborted %v before the caller's deadline", deadline.Sub(now))
	}
}

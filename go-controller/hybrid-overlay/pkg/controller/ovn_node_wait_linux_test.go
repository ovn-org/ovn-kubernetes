// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type bridgeMACClient struct {
	libovsdbclient.Client
	list func(context.Context, interface{}) error
}

func (c *bridgeMACClient) Where(_ ...model.Model) libovsdbclient.ConditionalAPI {
	return &bridgeMACQuery{list: c.list}
}

type bridgeMACQuery struct {
	libovsdbclient.ConditionalAPI
	list func(context.Context, interface{}) error
}

func (q *bridgeMACQuery) List(ctx context.Context, result interface{}) error {
	return q.list(ctx, result)
}

func TestHybridOverlayBridgeMACPolling(t *testing.T) {
	cacheErr := errors.New("cache unavailable")
	for _, tc := range []struct {
		name                                           string
		readyAfter, cancelAfter, deadline, wantElapsed time.Duration
		initiallyMissing, initiallyEmpty               bool
		readErr, wantErr                               error
	}{
		{name: "already ready"},
		{name: "fast MAC", readyAfter: 3 * time.Millisecond, wantElapsed: 6 * time.Millisecond},
		{name: "initially absent", initiallyMissing: true, readyAfter: 3 * time.Millisecond, wantElapsed: 6 * time.Millisecond},
		{name: "initially empty", initiallyEmpty: true, readyAfter: 3 * time.Millisecond, wantElapsed: 6 * time.Millisecond},
		{name: "slow MAC", readyAfter: 350 * time.Millisecond, wantElapsed: 426 * time.Millisecond},
		{name: "caller deadline", readyAfter: -1, deadline: 325 * time.Millisecond,
			wantElapsed: 325 * time.Millisecond, wantErr: context.DeadlineExceeded},
		{name: "cancel during backoff", readyAfter: -1, cancelAfter: 100 * time.Millisecond,
			wantElapsed: 100 * time.Millisecond, wantErr: context.Canceled},
		{name: "internal timeout", readyAfter: -1, wantElapsed: ovsBridgeMACWaitTimeout, wantErr: context.DeadlineExceeded},
		{name: "cache error", readyAfter: -1, readErr: cacheErr, wantErr: cacheErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.deadline > 0 {
					var cancelDeadline context.CancelFunc
					ctx, cancelDeadline = context.WithTimeout(ctx, tc.deadline)
					defer cancelDeadline()
				}
				if tc.cancelAfter > 0 {
					go func() {
						time.Sleep(tc.cancelAfter)
						cancel()
					}()
				}
				var reads []time.Duration
				c := &bridgeMACClient{list: func(_ context.Context, result interface{}) error {
					reads = append(reads, time.Since(start))
					iface := &vswitchd.Interface{UUID: "bridge-iface", Name: extBridgeName}
					if tc.readyAfter >= 0 && time.Since(start) >= tc.readyAfter {
						mac := "0a:58:01:02:03:04"
						iface.MACInUse = &mac
					} else if tc.initiallyMissing {
						return nil
					} else if tc.initiallyEmpty {
						mac := ""
						iface.MACInUse = &mac
					}
					*result.(*[]*vswitchd.Interface) = []*vswitchd.Interface{iface}
					return tc.readErr
				}}
				n := &NodeController{ovsClient: c}
				mac, err := n.waitForHybridOverlayBridgeMAC(ctx)
				if tc.wantErr != nil {
					require.ErrorIs(t, err, tc.wantErr)
					require.Empty(t, mac)
				} else {
					require.NoError(t, err)
					require.Equal(t, "0a:58:01:02:03:04", mac)
				}
				require.Equal(t, tc.wantElapsed, time.Since(start))
				if tc.name == "internal timeout" {
					require.LessOrEqual(t, len(reads), 310, "long waits must back off their cache polling")
					require.Equal(t, 100*time.Millisecond, reads[len(reads)-1]-reads[len(reads)-2])
				}
			})
		})
	}
}

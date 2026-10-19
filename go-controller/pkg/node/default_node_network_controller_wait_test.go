// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

type subnetWatchFactory struct {
	factory.NodeWatchFactory
	getNode func(string) (*corev1.Node, error)
}

func (f *subnetWatchFactory) GetNode(name string) (*corev1.Node, error) {
	return f.getNode(name)
}

func TestNodeSubnetPolling(t *testing.T) {
	for _, tc := range []struct {
		name                                           string
		readyAfter, cancelAfter, deadline, wantElapsed time.Duration
		pendingAnnotation                              string
		readErr, wantErr                               error
	}{
		{name: "already ready"},
		{name: "fast annotation", readyAfter: 3 * time.Millisecond, wantElapsed: 6 * time.Millisecond},
		{name: "initial cache miss", readyAfter: 3 * time.Millisecond, wantElapsed: 6 * time.Millisecond,
			readErr: apierrors.NewNotFound(corev1.Resource("nodes"), "node")},
		{name: "transient cache error", readyAfter: 3 * time.Millisecond, wantElapsed: 6 * time.Millisecond,
			readErr: errors.New("cache unavailable")},
		{name: "malformed annotation replaced", readyAfter: 3 * time.Millisecond, wantElapsed: 6 * time.Millisecond,
			pendingAnnotation: "invalid"},
		{name: "slow annotation", readyAfter: 1250 * time.Millisecond, wantElapsed: 1510 * time.Millisecond},
		{name: "caller deadline", readyAfter: -1, deadline: 325 * time.Millisecond,
			wantElapsed: 325 * time.Millisecond, wantErr: context.DeadlineExceeded},
		{name: "cancel during backoff", readyAfter: -1, cancelAfter: 100 * time.Millisecond,
			wantElapsed: 100 * time.Millisecond, wantErr: context.Canceled},
		{name: "startup timeout", readyAfter: -1, wantElapsed: 300 * time.Second, wantErr: context.DeadlineExceeded},
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
				readyNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Annotations: map[string]string{
					types.NodeSubnetsAnnotation: `{"default":["10.128.0.0/24","fd00::/64"]}`,
				}}}
				var reads []time.Duration
				wf := &subnetWatchFactory{getNode: func(name string) (*corev1.Node, error) {
					require.Equal(t, "node", name)
					reads = append(reads, time.Since(start))
					if tc.readyAfter >= 0 && time.Since(start) >= tc.readyAfter {
						return readyNode, nil
					}
					if tc.readErr != nil {
						return nil, tc.readErr
					}
					pending := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
					if tc.pendingAnnotation != "" {
						pending.Annotations = map[string]string{types.NodeSubnetsAnnotation: tc.pendingAnnotation}
					}
					return pending, nil
				}}
				nc := &DefaultNodeNetworkController{BaseNodeNetworkController: BaseNodeNetworkController{
					CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{name: "node", watchFactory: wf},
				}}
				node, subnets, err := nc.waitForNodeSubnets(ctx)
				if tc.wantErr != nil {
					require.ErrorIs(t, err, tc.wantErr)
				} else {
					require.NoError(t, err)
					require.Same(t, readyNode, node)
					require.Len(t, subnets, 2)
					require.Equal(t, "10.128.0.0/24", subnets[0].String())
					require.Equal(t, "fd00::/64", subnets[1].String())
				}
				require.Equal(t, tc.wantElapsed, time.Since(start))
				if tc.name == "startup timeout" {
					require.LessOrEqual(t, len(reads), 610, "long waits must back off their cache polling")
					require.Equal(t, 500*time.Millisecond, reads[len(reads)-1]-reads[len(reads)-2])
				}
			})
		})
	}
}

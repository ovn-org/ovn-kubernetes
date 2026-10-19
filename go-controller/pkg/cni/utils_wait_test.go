// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestPodAnnotationPolling(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		readyAfter, cancelAfter, wantElapsed time.Duration
		initiallyMissing                     bool
		wantErr                              error
	}{
		{name: "already ready"},
		{name: "fast annotation", readyAfter: 3 * time.Millisecond, wantElapsed: 6 * time.Millisecond},
		{name: "initial cache miss", initiallyMissing: true, readyAfter: 3 * time.Millisecond, wantElapsed: 6 * time.Millisecond},
		{name: "slow annotation", readyAfter: 650 * time.Millisecond, wantElapsed: 654 * time.Millisecond},
		{name: "deadline", readyAfter: -1, wantElapsed: 1050 * time.Millisecond, wantErr: context.DeadlineExceeded},
		{name: "cancel during backoff", readyAfter: -1, cancelAfter: 105 * time.Millisecond,
			wantElapsed: 105 * time.Millisecond, wantErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), 1050*time.Millisecond)
				defer cancel()
				if tc.cancelAfter != 0 {
					go func() {
						time.Sleep(tc.cancelAfter)
						cancel()
					}()
				}
				annotations, err := util.MarshalPodAnnotation(nil, &util.PodAnnotation{
					MAC: net.HardwareAddr{0x0a, 0x58, 0x0a, 0x80, 0, 1},
				}, types.DefaultNetworkName)
				require.NoError(t, err)
				pod := newPod("ns", "pod", annotations)
				indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
				if !tc.initiallyMissing {
					pending := pod.DeepCopy()
					pending.Annotations = nil
					require.NoError(t, indexer.Add(pending))
				}
				if tc.readyAfter == 0 {
					require.NoError(t, indexer.Update(pod))
				} else if tc.readyAfter > 0 {
					go func() {
						time.Sleep(tc.readyAfter)
						if err := indexer.Update(pod); err != nil {
							t.Error(err)
						}
					}()
				}
				kclient := fake.NewClientset()
				clientset := &ClientSet{kclient: kclient, podLister: corelisters.NewPodLister(indexer)}
				gotPod, annotations, annotation, err := getPodWithAnnotations(ctx, clientset,
					"ns", "pod", types.DefaultNetworkName, string(pod.UID), false, isOvnReady)
				if tc.wantErr != nil {
					require.ErrorIs(t, err, tc.wantErr)
				} else {
					require.NoError(t, err)
					require.Equal(t, pod, gotPod)
					require.Equal(t, pod.Annotations, annotations)
					require.NotNil(t, annotation)
				}
				require.Equal(t, tc.wantElapsed, time.Since(start))
				require.Empty(t, kclient.Actions(), "fast polling must only read the informer cache")
			})
		})
	}
}

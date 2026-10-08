// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package libovsdb

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestOVSInterfaceWatchers(t *testing.T) {
	c := &ovsClient{waiters: make(map[string]map[chan struct{}]struct{})}
	first, stopFirst := c.WatchOVSInterface("pod-port")
	second, stopSecond := c.WatchOVSInterface("pod-port")
	other, stopOther := c.WatchOVSInterface("other-port")
	// Notifications coalesce without blocking the cache event dispatcher.
	for range 10 {
		c.notifyInterface(vswitchd.InterfaceTable, &vswitchd.Interface{Name: "pod-port"})
	}
	require.Len(t, first, 1)
	require.Len(t, second, 1)
	require.Empty(t, other)
	<-first
	<-second
	stopFirst()
	stopFirst()
	c.notifyInterface(vswitchd.InterfaceTable, &vswitchd.Interface{Name: "pod-port"})
	require.Empty(t, first)
	require.Len(t, second, 1)
	stopSecond()
	stopOther()
	require.Empty(t, c.waiters)
	select {
	case <-first:
		t.Fatal("stopping a watch must not close its channel")
	default:
	}
}

func TestOVSInterfaceWatcherConcurrentCleanup(t *testing.T) {
	c := &ovsClient{waiters: make(map[string]map[chan struct{}]struct{})}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 100 {
				_, stop := c.WatchOVSInterface("pod-port")
				c.notifyInterface(vswitchd.InterfaceTable, &vswitchd.Interface{Name: "pod-port"})
				stop()
				stop()
			}
		})
	}
	wg.Wait()
	require.Empty(t, c.waiters)
}

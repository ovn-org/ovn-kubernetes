// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package libovsdb

import (
	"sync"

	"github.com/ovn-kubernetes/libovsdb/cache"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// OVSInterfaceWatcher provides binding-change notifications from the OVS cache.
// NewOVSClient and NewOVSClientWithEndpoint return clients implementing it.
type OVSInterfaceWatcher interface {
	// WatchOVSInterface registers before the caller reads the cache, avoiding a
	// lost wakeup between reading and subscribing. Notifications are coalesced
	// hints: callers must reread the cache and retain a polling fallback because
	// libovsdb can drop events. The caller must stop the watch when done; stopping
	// is idempotent and does not close the channel.
	WatchOVSInterface(name string) (<-chan struct{}, func())
}

type ovsClient struct {
	client.Client
	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
}

func newOVSClient(c client.Client) *ovsClient {
	ovs := &ovsClient{Client: c, waiters: make(map[string]map[chan struct{}]struct{})}
	// libovsdb has no handler removal API. One handler belongs to the client;
	// individual CNI requests only add and remove entries in waiters.
	c.Cache().AddEventHandler(&cache.EventHandlerFuncs{
		AddFunc:    ovs.notifyInterface,
		DeleteFunc: ovs.notifyInterface,
		UpdateFunc: func(table string, old, new model.Model) {
			if table != vswitchd.InterfaceTable {
				return
			}
			before, after := old.(*vswitchd.Interface), new.(*vswitchd.Interface)
			if before.ExternalIDs["iface-id"] == after.ExternalIDs["iface-id"] &&
				before.ExternalIDs["ovn-installed"] == after.ExternalIDs["ovn-installed"] {
				return
			}
			ovs.notifyInterface(table, new)
		},
	})
	return ovs
}

func (c *ovsClient) WatchOVSInterface(name string) (<-chan struct{}, func()) {
	changes := make(chan struct{}, 1)
	c.mu.Lock()
	if c.waiters[name] == nil {
		c.waiters[name] = make(map[chan struct{}]struct{})
	}
	c.waiters[name][changes] = struct{}{}
	c.mu.Unlock()
	return changes, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.waiters[name], changes)
		if len(c.waiters[name]) == 0 {
			delete(c.waiters, name)
		}
	}
}

func (c *ovsClient) notifyInterface(table string, row model.Model) {
	if table != vswitchd.InterfaceTable {
		return
	}
	name := row.(*vswitchd.Interface).Name
	c.mu.Lock()
	defer c.mu.Unlock()
	for changes := range c.waiters[name] {
		select {
		case changes <- struct{}{}:
		default:
		}
	}
}

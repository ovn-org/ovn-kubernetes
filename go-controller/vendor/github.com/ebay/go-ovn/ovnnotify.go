/**
 * Copyright (c) 2017 eBay Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 **/

package goovn

import (
	"sync"

	"k8s.io/klog/v2"

	"github.com/ebay/libovsdb"
)

type update struct {
	db        string
	updates   *libovsdb.TableUpdates
	updates2  *libovsdb.TableUpdates2
	lastTxnId string
}

type ovnNotifier struct {
	sync.Mutex
	odbi            *ovndb
	deferUpdates    bool
	deferredUpdates []*update
}

func newOVNNotifier(odbi *ovndb) *ovnNotifier {
	return &ovnNotifier{
		odbi:            odbi,
		deferUpdates:    true,
		deferredUpdates: make([]*update, 0),
	}
}

func newUpdate(context interface{}, updates *libovsdb.TableUpdates, updates2 *libovsdb.TableUpdates2, lastTxnId string) *update {
	dbName, ok := context.(string)
	if !ok {
		klog.Warningf("Expected string-type OVN update context but got %v", context)
		return nil
	}
	return &update{
		db:        dbName,
		updates:   updates,
		updates2:  updates2,
		lastTxnId: lastTxnId,
	}
}

func (n *ovnNotifier) processUpdate(u *update, signal bool) {
	if u.updates != nil {
		n.odbi.populateCache(u.db, u.updates, signal)
	} else {
		n.odbi.populateCache2(u.db, u.updates2, signal)
		if u.lastTxnId != "" {
			n.odbi.currentTxn = u.lastTxnId
		}
	}
}

func (n *ovnNotifier) processDeferredUpdates() {
	// we don't need to hold the lock while iterating deferred
	// updates since nothing can add to the array after we set
	// deferUpdates to false
	n.Lock()
	n.deferUpdates = false
	n.Unlock()

	for _, u := range n.deferredUpdates {
		n.processUpdate(u, false)
	}
}

// maybeDeferUpdate adds the update to the deferred updates list if it should
// be deferred because we are in the middle of the initial DB dump. Returns
// true if the update was deferred.
func (n *ovnNotifier) maybeDeferUpdate(u *update) bool {
	n.Lock()
	defer n.Unlock()
	if n.deferUpdates {
		n.deferredUpdates = append(n.deferredUpdates, u)
	}
	return n.deferUpdates
}

// handleUpdate either adds the update to the deferred updates list or
// processes the update immediately. Assumes the caller holds the cache
// mutexes while updates are deferred and calls processDeferredUpdates()
// when the initial DB dump is done. This ensures that deferred updates
// get processed first since non-deferred updates will block on the
// cache mutexes that the caller holds until processDeferredUpdates() is done.
func (n *ovnNotifier) handleUpdate(u *update) {
	if n.maybeDeferUpdate(u) {
		return
	}

	if u.db == DBServer {
		n.odbi.serverCacheMutex.Lock()
		defer n.odbi.serverCacheMutex.Unlock()
	} else {
		n.odbi.cachemutex.Lock()
		defer n.odbi.cachemutex.Unlock()
	}

	n.processUpdate(u, true)
}

func (n *ovnNotifier) Update(context interface{}, tableUpdates libovsdb.TableUpdates) {
	if u := newUpdate(context, &tableUpdates, nil, ""); u != nil {
		n.handleUpdate(u)
	}
}

func (n *ovnNotifier) Update2(context interface{}, tableUpdates libovsdb.TableUpdates2) {
	if u := newUpdate(context, nil, &tableUpdates, ""); u != nil {
		n.handleUpdate(u)
	}
}

func (n *ovnNotifier) Update3(context interface{}, tableUpdates libovsdb.TableUpdates2, lastTxnId string) {
	if u := newUpdate(context, nil, &tableUpdates, lastTxnId); u != nil {
		n.handleUpdate(u)
	}
}

func (notify *ovnNotifier) Locked([]interface{}) {
}
func (notify *ovnNotifier) Stolen([]interface{}) {
}
func (notify *ovnNotifier) Echo([]interface{}) {
}

func (notify *ovnNotifier) Disconnected(client *libovsdb.OvsdbClient) {
	if notify.odbi.reconn {
		notify.odbi.reconnect()
	} else if notify.odbi.disconnectCB != nil {
		notify.odbi.disconnectCB()
	}
}

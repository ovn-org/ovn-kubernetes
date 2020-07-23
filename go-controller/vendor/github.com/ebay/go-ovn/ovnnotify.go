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
	"github.com/ebay/libovsdb"
)

type ovnNotifier struct {
	odbi *ovndb
}

func (notify ovnNotifier) Update(context interface{}, tableUpdates libovsdb.TableUpdates) {
	notify.odbi.populateCache(tableUpdates)
}
func (notify ovnNotifier) Locked([]interface{}) {
}
func (notify ovnNotifier) Stolen([]interface{}) {
}
func (notify ovnNotifier) Echo([]interface{}) {
}

func (notify ovnNotifier) Disconnected(client *libovsdb.OvsdbClient) {
	if notify.odbi.reconn {
		notify.odbi.reconnect()
	} else if notify.odbi.disconnectCB != nil {
		notify.odbi.disconnectCB()
	}
}

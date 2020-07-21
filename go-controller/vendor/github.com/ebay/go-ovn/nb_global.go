/**
 * Copyright (c) 2020 eBay Inc.
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

type NBGlobalTableRow struct {
	UUID        string
	Options     map[interface{}]interface{}
	ExternalID  map[interface{}]interface{}
	Connections []string
	SSL         string
	IPSec       bool
}

func (odbi *ovndb) nbGlobalAddImp(options map[string]string) (*OvnCommand, error) {
	return odbi.addGlobalTableRowImp(options, tableNBGlobal)
}

func (odbi *ovndb) nbGlobalDelImp() (*OvnCommand, error) {
	return odbi.delGlobalTableRowImp(tableNBGlobal)
}

// ovsdb-client -v transact '["Open_vSwitch", {"op" : "update", "table" : "NB_Global", "where": [["_uuid", "==", ["uuid", "587c6ee2-93f9-4bd8-9794-f4a983d139a4"]]],
// "row":{ "options" : [ "map", [[ "bar", "baz"],["engine_test", "engine-foo"]]],}}]'

func (odbi *ovndb) nbGlobalSetOptionsImp(options map[string]string) (*OvnCommand, error) {
	return odbi.globalSetOptionsImp(options, tableNBGlobal)
}

func (odbi *ovndb) nbGlobalGetOptionsImp() (map[string]string, error) {
	return odbi.globalGetOptionsImp(tableNBGlobal)
}

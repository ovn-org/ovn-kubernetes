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

// AddressSet ovnnb item
type AddressSet struct {
	UUID       string
	Name       string
	Addresses  []string
	ExternalID map[interface{}]interface{}
}

func (odbi *ovndb) asUpdateImp(name string, addrs []string, external_ids map[string]string) (*OvnCommand, error) {
	row := make(OVNRow)
	row["name"] = name
	addresses, err := libovsdb.NewOvsSet(addrs)
	if err != nil {
		return nil, err
	}

	row["addresses"] = addresses
	if external_ids != nil {
		oMap, err := libovsdb.NewOvsMap(external_ids)
		if err != nil {
			return nil, err
		}
		row["external_ids"] = oMap
	}
	condition := libovsdb.NewCondition("name", "==", name)
	updateOp := libovsdb.Operation{
		Op:    opUpdate,
		Table: tableAddressSet,
		Row:   row,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{updateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) asAddImp(name string, addrs []string, external_ids map[string]string) (*OvnCommand, error) {
	row := make(OVNRow)
	row["name"] = name
	//should support the -is-exist flag here.

	if uuid := odbi.getRowUUID(tableAddressSet, row); len(uuid) > 0 {
		return nil, ErrorExist
	}

	if external_ids != nil {
		oMap, err := libovsdb.NewOvsMap(external_ids)
		if err != nil {
			return nil, err
		}
		row["external_ids"] = oMap
	}
	addresses, err := libovsdb.NewOvsSet(addrs)
	if err != nil {
		return nil, err
	}
	row["addresses"] = addresses
	insertOp := libovsdb.Operation{
		Op:    opInsert,
		Table: tableAddressSet,
		Row:   row,
	}
	operations := []libovsdb.Operation{insertOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

// TODO fix to get as from cache directly
func (odbi *ovndb) asGetImp(name string) (*AddressSet, error) {
	listAS, err := odbi.ASList()
	if err != nil {
		return nil, err
	}

	for _, s := range listAS {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, ErrorNotFound
}

func (odbi *ovndb) asDelImp(name string) (*OvnCommand, error) {
	condition := libovsdb.NewCondition("name", "==", name)
	deleteOp := libovsdb.Operation{
		Op:    opDelete,
		Table: tableAddressSet,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{deleteOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

// Get all addressset
func (odbi *ovndb) asListImp() ([]*AddressSet, error) {
	var listAS []*AddressSet

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheAddressSet, ok := odbi.cache[tableAddressSet]
	if !ok {
		return nil, ErrorSchema
	}

	for uuid, drows := range cacheAddressSet {
		ta := &AddressSet{
			UUID:       uuid,
			Name:       drows.Fields["name"].(string),
			ExternalID: drows.Fields["external_ids"].(libovsdb.OvsMap).GoMap,
		}
		addresses := []string{}
		as := drows.Fields["addresses"]
		switch as.(type) {
		case libovsdb.OvsSet:
			//TODO: is it possible return interface type directly instead of GoSet
			if goset, ok := drows.Fields["addresses"].(libovsdb.OvsSet); ok {
				for _, i := range goset.GoSet {
					addresses = append(addresses, i.(string))
				}
			}
		case string:
			if v, ok := drows.Fields["addresses"].(string); ok {
				addresses = append(addresses, v)
			}
		}
		ta.Addresses = addresses
		listAS = append(listAS, ta)
	}
	return listAS, nil
}

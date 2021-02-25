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

import (
	"fmt"
	"github.com/ebay/libovsdb"
)

// Chassis_Private table OVN SB
type ChassisPrivate struct {
	UUID       string
	ExternalID map[interface{}]interface{}
	Name       string
	NbCfg      int
}

func (odbi *ovndb) chassisPrivateAddImp(chName string,
	external_ids map[string]string) (*OvnCommand, error) {

	if len(chName) == 0 {
		return nil, fmt.Errorf("chassis name cannot be empty")
	}

	namedUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}
	// row to insert
	chPrivate := make(OVNRow)
	chPrivate["name"] = chName

	if uuid := odbi.getRowUUID(TableChassisPrivate, chPrivate); len(uuid) > 0 {
		return nil, ErrorExist
	}

	if external_ids != nil {
		oMap, err := libovsdb.NewOvsMap(external_ids)
		if err != nil {
			return nil, err
		}
		chPrivate["external_ids"] = oMap
	}
	insertOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    TableChassisPrivate,
		Row:      chPrivate,
		UUIDName: namedUUID,
	}
	operations := []libovsdb.Operation{insertOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) chassisPrivateDelImp(name string) (*OvnCommand, error) {
	var operations []libovsdb.Operation

	condition := libovsdb.NewCondition("name", "==", name)
	deleteOp := libovsdb.Operation{
		Op:    opDelete,
		Table: TableChassisPrivate,
		Where: []interface{}{condition},
	}
	operations = append(operations, deleteOp)
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) chassisPrivateListImp() ([]*ChassisPrivate, error) {
	odbi.cachemutex.RLock()
	cacheChassisPrivate, ok := odbi.cache[TableChassisPrivate]
	odbi.cachemutex.RUnlock()

	if !ok {
		return nil, ErrorSchema
	}

	listChassisPrivate := make([]*ChassisPrivate, 0, len(cacheChassisPrivate))
	for uuid := range cacheChassisPrivate {
		chPrivate, err := odbi.rowToChassisPrivate(uuid)
		if err != nil {
			return nil, err
		}
		listChassisPrivate = append(listChassisPrivate, chPrivate)
	}
	return listChassisPrivate, nil
}

func (odbi *ovndb) chassisPrivateGetImp(chassis string) ([]*ChassisPrivate, error) {
	var listChassisPrivate []*ChassisPrivate

	odbi.cachemutex.RLock()
	cacheChassisPrivate, ok := odbi.cache[TableChassisPrivate]
	odbi.cachemutex.RUnlock()

	if !ok {
		return nil, ErrorSchema
	}

	for uuid, drows := range cacheChassisPrivate {
		if chName, ok := drows.Fields["name"].(string); ok && chName == chassis {
			chPrivate, err := odbi.rowToChassisPrivate(uuid)
			if err != nil {
				return nil, err
			}
			listChassisPrivate = append(listChassisPrivate, chPrivate)
		}
	}
	return listChassisPrivate, nil
}

func (odbi *ovndb) rowToChassisPrivate(uuid string) (*ChassisPrivate, error) {

	odbi.cachemutex.RLock()
	cacheChassisPrivate, ok := odbi.cache[TableChassisPrivate][uuid]
	odbi.cachemutex.RUnlock()

	if !ok {
		return nil, fmt.Errorf("row in chassis_private with uuid %s not found", uuid)
	}

	chPrivate := &ChassisPrivate{
		UUID:       uuid,
		ExternalID: cacheChassisPrivate.Fields["external_ids"].(libovsdb.OvsMap).GoMap,
		Name:       cacheChassisPrivate.Fields["name"].(string),
		NbCfg:      cacheChassisPrivate.Fields["nb_cfg"].(int),
	}
	return chPrivate, nil
}

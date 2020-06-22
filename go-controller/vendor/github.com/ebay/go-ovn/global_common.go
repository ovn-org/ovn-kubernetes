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

func (odbi *ovndb) addGlobalTableRowImp(options map[string]string, table string) (*OvnCommand, error) {
	namedUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}
	row := make(OVNRow)

	optionsMap, err := libovsdb.NewOvsMap(options)

	if err != nil {
		return nil, err
	}

	row["options"] = optionsMap

	if uuid := odbi.getRowUUID(table, row); len(uuid) > 0 {
		return nil, ErrorExist
	}

	insertOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    table,
		Row:      row,
		UUIDName: namedUUID,
	}

	operations := []libovsdb.Operation{insertOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) delGlobalTableRowImp(table string) (*OvnCommand, error) {
	if table == "" {
		return nil, fmt.Errorf("Invalid table name passed to delete")
	}

	uuid, err := func() (string, error) {
		odbi.cachemutex.RLock()
		defer odbi.cachemutex.RUnlock()
		cacheGlobal, ok := odbi.cache[table]
		if !ok {
			return "", fmt.Errorf("Table %s not found in cache %v", table, odbi.cache)
		}
		for uuid, _ := range cacheGlobal {
			return uuid, nil
		}
		return "", fmt.Errorf("No row found in %s table", table)
	}()
	if err != nil {
		return nil, err
	}

	condition := libovsdb.NewCondition("_uuid", "==", stringToGoUUID(uuid))
	deleteOp := libovsdb.Operation{
		Op:    opDelete,
		Table: table,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{deleteOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) globalSetOptionsImp(options map[string]string, table string) (*OvnCommand, error) {
	if options == nil || table == "" {
		return nil, fmt.Errorf("Invalid arguments passed to set options: table: %s, options:  %v", table, options)
	}
	optionsMap, err := libovsdb.NewOvsMap(options)
	if err != nil {
		return nil, err
	}

	uuid, err := func() (string, error) {
		odbi.cachemutex.RLock()
		defer odbi.cachemutex.RUnlock()
		cacheGlobal, ok := odbi.cache[table]
		if !ok {
			return "", fmt.Errorf("Table %s not found in cache %v", table, odbi.cache)
		}
		for uuid, _ := range cacheGlobal {
			return uuid, nil
		}
		return "", fmt.Errorf("No row found in %s table", table)
	}()
	if err != nil {
		return nil, err
	}
	row := make(OVNRow)
	row["options"] = optionsMap
	condition := libovsdb.NewCondition("_uuid", "==", stringToGoUUID(uuid))

	// simple mutate operation
	updateOp := libovsdb.Operation{
		Op:    opUpdate,
		Table: table,
		Row:   row,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{updateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) globalGetOptionsImp(table string) (map[string]string, error) {
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()
	cacheGlobal, ok := odbi.cache[table]
	if !ok {
		return nil, ErrorSchema
	}
	for _, drows := range cacheGlobal {
		if options, ok := drows.Fields["options"]; ok {
			switch options.(type) {
			case libovsdb.OvsMap:
				optionsGoMap := options.(libovsdb.OvsMap).GoMap
				optionsMap := make(map[string]string)
				for k, v := range optionsGoMap {
					key, keyOk := k.(string)
					value, valueOk := v.(string)
					if !keyOk || !valueOk {
						continue
					}
					optionsMap[key] = value
				}
				return optionsMap, nil
			default:
				return nil, fmt.Errorf("Error getting options field of the %s table - unsupported type", table)
			}
		}
	}
	return nil, fmt.Errorf("No row found in %s table", table)
}

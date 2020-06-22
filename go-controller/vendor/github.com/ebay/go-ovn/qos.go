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
	"fmt"
	"reflect"

	"github.com/ebay/libovsdb"
)

// QoS ovnnb item
type QoS struct {
	UUID       string
	Priority   int
	Direction  string
	Match      string
	Action     map[interface{}]interface{}
	Bandwidth  map[interface{}]interface{}
	ExternalID map[interface{}]interface{}
}

func (odbi *ovndb) rowToQoS(uuid string) *QoS {
	cacheQoS, ok := odbi.cache[tableQoS][uuid]
	if !ok {
		return nil
	}

	qos := &QoS{
		UUID:       uuid,
		Priority:   cacheQoS.Fields["priority"].(int),
		Direction:  cacheQoS.Fields["direction"].(string),
		Match:      cacheQoS.Fields["match"].(string),
		Action:     cacheQoS.Fields["action"].(libovsdb.OvsMap).GoMap,
		Bandwidth:  cacheQoS.Fields["bandwidth"].(libovsdb.OvsMap).GoMap,
		ExternalID: cacheQoS.Fields["external_ids"].(libovsdb.OvsMap).GoMap,
	}

	return qos
}

func (odbi *ovndb) qosAddImp(ls string, direction string, priority int, match string, action map[string]int, bandwidth map[string]int, external_ids map[string]string) (*OvnCommand, error) {
	namedUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}

	row := make(OVNRow)
	row["direction"] = direction
	row["priority"] = priority
	row["match"] = match

	if action != nil {
		oMap, err := libovsdb.NewOvsMap(action)
		if err != nil {
			return nil, err
		}
		row["action"] = oMap
	}
	if bandwidth != nil {
		oMap, err := libovsdb.NewOvsMap(bandwidth)
		if err != nil {
			return nil, err
		}
		row["bandwidth"] = oMap
	}
	if external_ids != nil {
		oMap, err := libovsdb.NewOvsMap(external_ids)
		if err != nil {
			return nil, err
		}
		row["external_ids"] = oMap
	}

	insertOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    tableQoS,
		Row:      row,
		UUIDName: namedUUID,
	}

	mutateUUID := []libovsdb.UUID{stringToGoUUID(namedUUID)}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}

	mutation := libovsdb.NewMutation("qos_rules", opInsert, mutateSet)
	condition := libovsdb.NewCondition("name", "==", ls)

	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalSwitch,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{condition},
	}
	operations := []libovsdb.Operation{insertOp, mutateOp}

	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) qosDelImp(ls string, direction string, priority int, match string) (*OvnCommand, error) {
	row := make(OVNRow)

	if len(direction) > 0 {
		row["direction"] = direction
	}
	//in ovn pirority is greater than/equal 0,
	//if input the priority < 0, lots of acls will be deleted if matches direct and match condition judgement.
	if priority >= 0 {
		row["priority"] = priority
	}
	if len(match) > 0 {
		row["match"] = match
	}

	selUUIDs := odbi.getRowUUIDs(tableQoS, row)
	if len(selUUIDs) == 0 && !reflect.DeepEqual(row, make(OVNRow)) {
		return nil, ErrorNotFound
	}

	if len(selUUIDs) == 0 {
		lsw, err := odbi.lsGetImp(ls)
		if err != nil {
			return nil, err
		}
		// TODO return error if multiple switches have the same name or modify all of them?
		selUUIDs = lsw[0].QoSRules
	}

	if len(selUUIDs) == 0 {
		return nil, ErrorNotFound
	}

	delUUIDs := make([]libovsdb.UUID, len(selUUIDs))
	for i, uuid := range selUUIDs {
		delUUIDs[i] = stringToGoUUID(uuid)
	}

	deleteSet, err := libovsdb.NewOvsSet(delUUIDs)
	if err != nil {
		return nil, err
	}

	// not need to delete rules form QoS table, because logical_switch
	// table reference uuid on it and delete automatic, also this is similar to
	// ovn-nbctl behaviour
	mutation := libovsdb.NewMutation("qos_rules", opDelete, deleteSet)
	condition := libovsdb.NewCondition("name", "==", ls)
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalSwitch,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{condition},
	}

	operations := []libovsdb.Operation{mutateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) qosListImp(ls string) ([]*QoS, error) {
	var listQoS []*QoS

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalSwitch, ok := odbi.cache[tableLogicalSwitch]
	if !ok {
		return nil, ErrorNotFound
	}
	var lsFound bool
	for _, drows := range cacheLogicalSwitch {
		if rlsw, ok := drows.Fields["name"].(string); ok && rlsw == ls {
			qosrules := drows.Fields["qos_rules"]
			if qosrules != nil {
				switch qosrules.(type) {
				case libovsdb.OvsSet:
					if ps, ok := qosrules.(libovsdb.OvsSet); ok {
						for _, p := range ps.GoSet {
							if vp, ok := p.(libovsdb.UUID); ok {
								tp := odbi.rowToQoS(vp.GoUUID)
								listQoS = append(listQoS, tp)
							}
						}
					} else {
						return nil, fmt.Errorf("type libovsdb.OvsSet casting failed")
					}
				case libovsdb.UUID:
					if vp, ok := qosrules.(libovsdb.UUID); ok {
						tp := odbi.rowToQoS(vp.GoUUID)
						listQoS = append(listQoS, tp)
					} else {
						return nil, fmt.Errorf("type libovsdb.UUID casting failed")
					}
				default:
					return nil, fmt.Errorf("Unsupport type found in ovsdb rows")
				}
			}
			lsFound = true
			break
		}
	}
	if !lsFound {
		return nil, ErrorNotFound
	}
	return listQoS, nil
}

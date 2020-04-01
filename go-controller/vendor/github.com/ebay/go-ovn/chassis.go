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

// Chassis table OVN SB
type Chassis struct {
	UUID                string
	Encaps              []string
	ExternalID          map[interface{}]interface{}
	Hostname            string
	Name                string
	NbCfg               int
	TransportZones      []string
	VtepLogicalSwitches []string
}

func (odbi *ovndb) chassisAddImp(name string, hostname string, etype []string, ip string,
	external_ids map[string]string, transport_zones []string, vtep_lswitches []string) (*OvnCommand, error) {
	if len(name) == 0 {
		return nil, fmt.Errorf("chassis name cannot be empty")
	}
	if len(etype) == 0 {
		return nil, fmt.Errorf("chassis encap type cannot be empty")
	}
	if len(ip) == 0 {
		return nil, fmt.Errorf("chassis ip cannot be empty")
	}
	//Prepare for encap record
	var encap_ids [][]string
	var namedUUID = "named-uuid"
	var operations []libovsdb.Operation
	for _, et := range etype {
		enCapUUID, err := newRowUUID()
		if err != nil {
			return nil, err
		}
		row := make(OVNRow)
		row["chassis_name"] = name
		var encap_id []string
		encap_id = append(encap_id, namedUUID)
		encap_id = append(encap_id, enCapUUID)
		encap_ids = append(encap_ids, encap_id)
		row["ip"] = ip
		row["type"] = et
		if uuid := odbi.getRowUUID(tableEncap, row); len(uuid) > 0 {
			return nil, ErrorExist
		}
		insertEncapOp := libovsdb.Operation{
			Op:       opInsert,
			Table:    tableEncap,
			Row:      row,
			UUIDName: enCapUUID,
		}
		operations = append(operations, insertEncapOp)
	}

	// Prepare for chassis record
	ChassisUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}
	rowChassis := make(OVNRow)
	// consolidate encaps
	var encaps []interface{}
	encaps = append(encaps, "set")
	encaps = append(encaps, encap_ids)
	rowChassis["encaps"] = encaps
	rowChassis["name"] = name
	rowChassis["hostname"] = hostname
	if external_ids != nil {
		oMap, err := libovsdb.NewOvsMap(external_ids)
		if err != nil {
			return nil, err
		}
		rowChassis["external_ids"] = oMap
	}
	if len(transport_zones) != 0 {
		rowChassis["transport_zones"] = transport_zones
	}
	if len(vtep_lswitches) != 0 {
		rowChassis["vtep_logical_switches"] = vtep_lswitches
	}
	insertChassisOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    tableChassis,
		Row:      rowChassis,
		UUIDName: ChassisUUID,
	}
	operations = append(operations, insertChassisOp)
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil

}

func (odbi *ovndb) chassisDelImp(name string) (*OvnCommand, error) {
	var operations []libovsdb.Operation

	condition := libovsdb.NewCondition("name", "==", name)
	deleteOp := libovsdb.Operation{
		Op:    opDelete,
		Table: tableChassis,
		Where: []interface{}{condition},
	}
	operations = append(operations, deleteOp)
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) chassisGetImp(chassis string) ([]*Chassis, error) {
	var listChassis []*Chassis

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheChassis, ok := odbi.cache[tableChassis]

	if !ok {
		return nil, ErrorSchema
	}

	for uuid, drows := range cacheChassis {
		if chName, ok := drows.Fields["hostname"].(string); ok && chName == chassis {
			ch, err := odbi.rowToChassis(uuid)
			if err != nil {
				return nil, err
			}
			listChassis = append(listChassis, ch)
		}
		if chName, ok := drows.Fields["name"].(string); ok && chName == chassis {
			ch, err := odbi.rowToChassis(uuid)
			if err != nil {
				return nil, err
			}
			listChassis = append(listChassis, ch)
		}
	}
	return listChassis, nil
}

func (odbi *ovndb) rowToChassis(uuid string) (*Chassis, error) {

	cacheChassis, ok := odbi.cache[tableChassis][uuid]
	if !ok {
		return nil, fmt.Errorf("Chassis with uuid%s not found", uuid)
	}
	ch := &Chassis{
		UUID:       uuid,
		Name:       cacheChassis.Fields["name"].(string),
		Hostname:   cacheChassis.Fields["hostname"].(string),
		ExternalID: cacheChassis.Fields["external_ids"].(libovsdb.OvsMap).GoMap,
		NbCfg:      cacheChassis.Fields["nb_cfg"].(int),
	}

	if tz, ok := cacheChassis.Fields["transport_zones"]; ok {
		switch tz.(type) {
		case libovsdb.UUID:
			ch.TransportZones = []string{tz.(libovsdb.UUID).GoUUID}
		case libovsdb.OvsSet:
			ch.TransportZones = odbi.ConvertGoSetToStringArray(tz.(libovsdb.OvsSet))
		}
	}
	if vtep, ok := cacheChassis.Fields["vtep_logical_switches"]; ok {
		switch vtep.(type) {
		case libovsdb.UUID:
			ch.VtepLogicalSwitches = []string{vtep.(libovsdb.UUID).GoUUID}
		case libovsdb.OvsSet:
			ch.VtepLogicalSwitches = odbi.ConvertGoSetToStringArray(vtep.(libovsdb.OvsSet))
		}
	}
	var encaps []string
	if enc, ok := cacheChassis.Fields["encaps"]; ok {
		switch enc.(type) {
		case libovsdb.UUID:
			if enuid, ok := enc.(libovsdb.UUID); ok {
				encaps = append(encaps, enuid.GoUUID)
			} else {
				return nil, fmt.Errorf("type libovsdb.UUID casting failed")
			}
		case libovsdb.OvsSet:
			if en, ok := enc.(libovsdb.OvsSet); ok {
				for _, e := range en.GoSet {
					if euid, ok := e.(libovsdb.UUID); ok {
						encaps = append(encaps, euid.GoUUID)
					}
				}
			} else {
				return nil, fmt.Errorf("type libovsdb.OvsSet casting failed")
			}
		}
	}
	ch.Encaps = encaps
	return ch, nil
}

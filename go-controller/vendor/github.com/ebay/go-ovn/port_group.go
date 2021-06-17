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

	"github.com/ebay/libovsdb"
)

// PortGroup ovnnb item
type PortGroup struct {
	UUID       string
	Name       string
	Ports      []string
	ACLs       []string
	ExternalID map[interface{}]interface{}
}

func (odbi *ovndb) pgAddImp(group string, ports []string, external_ids map[string]string) (*OvnCommand, error) {
	namedUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}

	row := make(OVNRow)
	row["name"] = group

	if uuid := odbi.getRowUUID(TablePortGroup, row); len(uuid) > 0 {
		return nil, ErrorExist
	}

	if ports != nil {
		portUUIDs := make([]libovsdb.UUID, 0, len(ports))
		for _, u := range ports {
			portUUIDs = append(portUUIDs, stringToGoUUID(u))
		}
		pgports, err := libovsdb.NewOvsSet(portUUIDs)
		if err != nil {
			return nil, err
		}
		row["ports"] = pgports
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
		Table:    TablePortGroup,
		Row:      row,
		UUIDName: namedUUID,
	}
	operations := []libovsdb.Operation{insertOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) pgUpdateImp(group string, ports []string, external_ids map[string]string) (*OvnCommand, error) {
	row := make(OVNRow)
	row["name"] = group

	if uuid := odbi.getRowUUID(TablePortGroup, row); len(uuid) == 0 {
		return nil, ErrorNotFound
	}

	if ports == nil && external_ids == nil {
		return nil, ErrorNoChanges
	}

	if ports != nil {
		portUUIDs := make([]libovsdb.UUID, 0, len(ports))
		for _, u := range ports {
			portUUIDs = append(portUUIDs, stringToGoUUID(u))
		}
		pgports, err := libovsdb.NewOvsSet(portUUIDs)
		if err != nil {
			return nil, err
		}
		row["ports"] = pgports
	}

	if external_ids != nil {
		oMap, err := libovsdb.NewOvsMap(external_ids)
		if err != nil {
			return nil, err
		}
		row["external_ids"] = oMap
	}

	condition := libovsdb.NewCondition("name", "==", group)
	updateOp := libovsdb.Operation{
		Op:    opUpdate,
		Table: TablePortGroup,
		Row:   row,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{updateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) pgAddPortImp(group, port string) (*OvnCommand, error) {
	if _, err := odbi.pgGetImp(group); err != nil {
		return nil, err
	}

	mutateUUID := []libovsdb.UUID{stringToGoUUID(port)}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}

	mutation := libovsdb.NewMutation("ports", opInsert, mutateSet)
	condition := libovsdb.NewCondition("name", "==", group)

	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     TablePortGroup,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{condition},
	}
	operations := []libovsdb.Operation{mutateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) pgRemovePortImp(group string, port string) (*OvnCommand, error) {
	if _, err := odbi.pgGetImp(group); err != nil {
		return nil, err
	}

	mutateUUID := []libovsdb.UUID{stringToGoUUID(port)}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}

	mutation := libovsdb.NewMutation("ports", opDelete, mutateSet)
	condition := libovsdb.NewCondition("name", "==", group)

	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     TablePortGroup,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{condition},
	}
	operations := []libovsdb.Operation{mutateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) pgDelImp(group string) (*OvnCommand, error) {
	if _, err := odbi.pgGetImp(group); err != nil {
		return nil, err
	}

	condition := libovsdb.NewCondition("name", "==", group)
	deleteOp := libovsdb.Operation{
		Op:    opDelete,
		Table: TablePortGroup,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{deleteOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) pgGetImp(pg string) (*PortGroup, error) {
	var pgList []*PortGroup
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cachePortGroup, ok := odbi.cache[TablePortGroup]
	if !ok {
		return nil, ErrorNotFound
	}

	for uuid, drows := range cachePortGroup {
		if rlsw, ok := drows.Fields["name"].(string); ok && rlsw == pg {
			pgList = append(pgList, odbi.RowToPortGroup(uuid))
		}
	}

	if len(pgList) == 0 {
		return nil, ErrorNotFound
	} else if len(pgList) != 1 {
		return nil, ErrorDuplicateName
	} else {
		return pgList[0], nil
	}
}

func (odbi *ovndb) RowToPortGroup(uuid string) *PortGroup {
	cachePortGroup, ok := odbi.cache[TablePortGroup][uuid]
	if !ok {
		return nil
	}
	pg := &PortGroup{
		UUID:       uuid,
		Name:       cachePortGroup.Fields["name"].(string),
		ExternalID: cachePortGroup.Fields["external_ids"].(libovsdb.OvsMap).GoMap,
	}
	ports := cachePortGroup.Fields["ports"]
	switch ports.(type) {
	case libovsdb.UUID:
		pg.Ports = []string{ports.(libovsdb.UUID).GoUUID}
	case libovsdb.OvsSet:
		pg.Ports = odbi.ConvertGoSetToStringArray(ports.(libovsdb.OvsSet))
	}
	acls := cachePortGroup.Fields["acls"]
	switch acls.(type) {
	case libovsdb.UUID:
		pg.ACLs = []string{acls.(libovsdb.UUID).GoUUID}
	case libovsdb.OvsSet:
		pg.ACLs = odbi.ConvertGoSetToStringArray(acls.(libovsdb.OvsSet))
	}
	return pg
}

func (odbi *ovndb) GetLogicalPortsByPortGroup(group string) ([]*LogicalSwitchPort, error) {
	var listLSP []*LogicalSwitchPort

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cachePortGroup, ok := odbi.cache[TablePortGroup]
	if !ok {
		return nil, ErrorSchema
	}

	for _, drows := range cachePortGroup {
		if pgname, ok := drows.Fields["name"].(string); ok && pgname == group {
			ports := drows.Fields["ports"]
			if ports != nil {
				switch ports.(type) {
				case libovsdb.OvsSet:
					if ps, ok := ports.(libovsdb.OvsSet); ok {
						for _, p := range ps.GoSet {
							if vp, ok := p.(libovsdb.UUID); ok {
								tp, err := odbi.rowToLogicalPort(vp.GoUUID)
								if err != nil {
									return nil, fmt.Errorf("Couldn't get logical port: %s", err)
								}
								listLSP = append(listLSP, tp)
							}
						}
					} else {
						return nil, fmt.Errorf("type libovsdb.OvsSet casting failed")
					}
				case libovsdb.UUID:
					if vp, ok := ports.(libovsdb.UUID); ok {
						tp, err := odbi.rowToLogicalPort(vp.GoUUID)
						if err != nil {
							return nil, fmt.Errorf("Couldn't get logical port: %s", err)
						}
						listLSP = append(listLSP, tp)
					} else {
						return nil, fmt.Errorf("type libovsdb.UUID casting failed")
					}
				default:
					return nil, fmt.Errorf("Unsupport type found in ovsdb rows")
				}
			}
			break
		}
	}
	return listLSP, nil
}

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

// LogicalRouterPort ovnnb item
type LogicalRouterPort struct {
	UUID           string
	Name           string
	GatewayChassis []string
	Networks       []string
	MAC            string
	Enabled        bool
	IPv6RAConfigs  map[interface{}]interface{}
	Options        map[interface{}]interface{}
	Peer           string
	ExternalID     map[interface{}]interface{}
}

func (odbi *ovndb) lrpAddImp(lr string, lrp string, mac string, network []string, peer string, external_ids map[string]string) (*OvnCommand, error) {
	namedUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}
	row := make(OVNRow)
	row["name"] = lrp
	row["mac"] = mac

	networks, err := libovsdb.NewOvsSet(network)
	if err != nil {
		return nil, err
	}
	row["networks"] = networks
	if len(peer) > 0 {
		row["peer"] = peer
	}

	if external_ids != nil {
		oMap, err := libovsdb.NewOvsMap(external_ids)
		if err != nil {
			return nil, err
		}
		row["external_ids"] = oMap
	}

	if uuid := odbi.getRowUUID(TableLogicalRouterPort, row); len(uuid) > 0 {
		return nil, ErrorExist
	}

	insertOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    TableLogicalRouterPort,
		Row:      row,
		UUIDName: namedUUID,
	}

	mutateUUID := []libovsdb.UUID{stringToGoUUID(namedUUID)}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}
	mutation := libovsdb.NewMutation("ports", opInsert, mutateSet)
	condition := libovsdb.NewCondition("name", "==", lr)

	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     TableLogicalRouter,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{condition},
	}
	operations := []libovsdb.Operation{insertOp, mutateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil

}

func (odbi *ovndb) lrpDelImp(lr, lrp string) (*OvnCommand, error) {
	row := make(OVNRow)
	row["name"] = lrp

	lrpUUID := odbi.getRowUUID(TableLogicalRouterPort, row)
	if len(lrpUUID) == 0 {
		return nil, ErrorNotFound
	}

	mutateUUID := []libovsdb.UUID{stringToGoUUID(lrpUUID)}
	condition := libovsdb.NewCondition("name", "==", lr)
	deleteOp := libovsdb.Operation{
		Op:    opDelete,
		Table: TableLogicalRouterPort,
		Where: []interface{}{condition},
	}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}
	mutation := libovsdb.NewMutation("ports", opDelete, mutateSet)
	ucondition, err := odbi.getRowUUIDContainsUUID(TableLogicalRouter, "ports", lrpUUID)
	if err != nil {
		return nil, err
	}

	mucondition := libovsdb.NewCondition("_uuid", "==", stringToGoUUID(ucondition))
	// simple mutate operation
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     TableLogicalRouter,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{mucondition},
	}
	operations := []libovsdb.Operation{deleteOp, mutateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) rowToLogicalRouterPort(uuid string) *LogicalRouterPort {
	lrp := &LogicalRouterPort{
		UUID:       uuid,
		Name:       odbi.cache[TableLogicalRouterPort][uuid].Fields["name"].(string),
		MAC:        odbi.cache[TableLogicalRouterPort][uuid].Fields["mac"].(string),
		ExternalID: odbi.cache[TableLogicalRouterPort][uuid].Fields["external_ids"].(libovsdb.OvsMap).GoMap,
	}

	if peer, ok := odbi.cache[TableLogicalRouterPort][uuid].Fields["peer"]; ok {
		switch peer.(type) {
		case string:
			lrp.Peer = peer.(string)
		}
	}

	if options, ok := odbi.cache[TableLogicalRouterPort][uuid].Fields["options"]; ok {
		lrp.Options = options.(libovsdb.OvsMap).GoMap
	}

	if ipv6_ra_configs, ok := odbi.cache[TableLogicalRouterPort][uuid].Fields["ipv6_ra_configs"]; ok {
		lrp.IPv6RAConfigs = ipv6_ra_configs.(libovsdb.OvsMap).GoMap
	}

	if enabled, ok := odbi.cache[TableLogicalRouterPort][uuid].Fields["enabled"]; ok {
		switch enabled.(type) {
		case bool:
			lrp.Enabled = enabled.(bool)
		case libovsdb.OvsSet:
			if enabled.(libovsdb.OvsSet).GoSet == nil {
				lrp.Enabled = true
			}
		}
	}

	gateway_chassis := odbi.cache[TableLogicalRouterPort][uuid].Fields["gateway_chassis"]
	switch gateway_chassis.(type) {
	case string:
		lrp.GatewayChassis = []string{gateway_chassis.(string)}
	case libovsdb.OvsSet:
		lrp.GatewayChassis = odbi.ConvertGoSetToStringArray(gateway_chassis.(libovsdb.OvsSet))
	}
	networks := odbi.cache[TableLogicalRouterPort][uuid].Fields["networks"]
	switch networks.(type) {
	case string:
		lrp.Networks = []string{networks.(string)}
	case libovsdb.OvsSet:
		lrp.Networks = odbi.ConvertGoSetToStringArray(networks.(libovsdb.OvsSet))
	}

	return lrp
}

func (odbi *ovndb) lrpListImp(lr string) ([]*LogicalRouterPort, error) {
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalRouter, ok := odbi.cache[TableLogicalRouter]
	if !ok {
		return nil, ErrorNotFound
	}

	for _, drows := range cacheLogicalRouter {
		if rlr, ok := drows.Fields["name"].(string); ok && rlr == lr {
			ports := drows.Fields["ports"]
			if ports != nil {
				switch ports.(type) {
				case libovsdb.OvsSet:
					if ps, ok := ports.(libovsdb.OvsSet); ok {
						listLRP := make([]*LogicalRouterPort, 0, len(ps.GoSet))
						for _, p := range ps.GoSet {
							if vp, ok := p.(libovsdb.UUID); ok {
								tp := odbi.rowToLogicalRouterPort(vp.GoUUID)
								listLRP = append(listLRP, tp)
							}
						}
						return listLRP, nil
					} else {
						return nil, fmt.Errorf("type libovsdb.OvsSet casting failed")
					}
				case libovsdb.UUID:
					if vp, ok := ports.(libovsdb.UUID); ok {
						tp := odbi.rowToLogicalRouterPort(vp.GoUUID)
						return []*LogicalRouterPort{tp}, nil
					} else {
						return nil, fmt.Errorf("type libovsdb.UUID casting failed")
					}
				default:
					return nil, fmt.Errorf("Unsupport type found in ovsdb rows")
				}
			}
			return []*LogicalRouterPort{}, nil
		}
	}
	return nil, ErrorNotFound
}

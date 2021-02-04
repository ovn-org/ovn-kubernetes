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

// LogicalRouterStaticRoute ovnnb item
type LogicalRouterStaticRoute struct {
	UUID       string
	IPPrefix   string
	Nexthop    string
	OutputPort *string
	Policy     *string
	ExternalID map[interface{}]interface{}
}

func (odbi *ovndb) lrsrAddImp(lr string, ip_prefix string, nexthop string, output_port *string, policy *string, external_ids map[string]string) (*OvnCommand, error) {
	namedUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}

	row := make(OVNRow)
	row["ip_prefix"] = ip_prefix
	row["nexthop"] = nexthop
	if output_port != nil {
		row["output_port"] = *output_port
	}
	if policy != nil {
		row["policy"] = *policy
	}
	if external_ids != nil {
		oMap, err := libovsdb.NewOvsMap(external_ids)
		if err != nil {
			return nil, err
		}
		row["external_ids"] = oMap
	}

	if uuid := odbi.getRowUUID(TableLogicalRouterStaticRoute, row); len(uuid) > 0 {
		return nil, ErrorExist
	}

	insertOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    TableLogicalRouterStaticRoute,
		Row:      row,
		UUIDName: namedUUID,
	}

	mutateUUID := []libovsdb.UUID{stringToGoUUID(namedUUID)}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}
	mutation := libovsdb.NewMutation("static_routes", opInsert, mutateSet)
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

func (odbi *ovndb) lrsrDelImp(lr string, prefix string, nexthop, outputPort, policy *string) (*OvnCommand, error) {
	if lr == "" {
		return nil, fmt.Errorf("lr (logical router name) is required")
	}
	if prefix == "" {
		return nil, fmt.Errorf("prefix is required")
	}
	var operations []libovsdb.Operation
	row := make(OVNRow)
	row["ip_prefix"] = prefix
	if nexthop != nil {
		row["nexthop"] = *nexthop
	}
	if policy != nil {
		row["policy"] = *policy
	}
	if outputPort != nil {
		row["output_port"] = *outputPort
	}
	lrsruuid := odbi.getRowUUID(TableLogicalRouterStaticRoute, row)
	if len(lrsruuid) == 0 {
		return nil, ErrorNotFound
	}
	mutateUUID := []libovsdb.UUID{stringToGoUUID(lrsruuid)}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}
	row = make(OVNRow)
	row["name"] = lr
	lruuid := odbi.getRowUUID(TableLogicalRouter, row)
	if len(lruuid) == 0 {
		return nil, ErrorNotFound
	}
	mutation := libovsdb.NewMutation("static_routes", opDelete, mutateSet)
	// mutate  lrouter for the corresponding static_routes
	mucondition := libovsdb.NewCondition("name", "==", lr)
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     TableLogicalRouter,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{mucondition},
	}
	operations = append(operations, mutateOp)
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lrsrDelByUUIDImp(lr, uuid string) (*OvnCommand, error) {
	if lr == "" {
		return nil, fmt.Errorf("lr (logical router name) is required")
	}
	if uuid == "" {
		return nil, fmt.Errorf("uuid is required")
	}
	row := make(OVNRow)
	row["name"] = lr
	lruuid := odbi.getRowUUID(TableLogicalRouter, row)
	if len(lruuid) == 0 {
		return nil, ErrorNotFound
	}

	mutateSet, err := libovsdb.NewOvsSet([]libovsdb.UUID{stringToGoUUID(uuid)})
	if err != nil {
		return nil, err
	}
	mutation := libovsdb.NewMutation("static_routes", opDelete, mutateSet)
	// mutate  lrouter for the corresponding static_routes
	mucondition := libovsdb.NewCondition("name", "==", lr)
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     TableLogicalRouter,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{mucondition},
	}
	operations := []libovsdb.Operation{mutateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) rowToLogicalRouterStaticRoute(uuid string) *LogicalRouterStaticRoute {
	cacheLogicalRouterStaticRoute, ok := odbi.cache[TableLogicalRouterStaticRoute][uuid]
	if !ok {
		return nil
	}
	lrsr := &LogicalRouterStaticRoute{
		UUID:       uuid,
		IPPrefix:   cacheLogicalRouterStaticRoute.Fields["ip_prefix"].(string),
		Nexthop:    cacheLogicalRouterStaticRoute.Fields["nexthop"].(string),
		ExternalID: cacheLogicalRouterStaticRoute.Fields["external_ids"].(libovsdb.OvsMap).GoMap,
	}

	if policy, ok := cacheLogicalRouterStaticRoute.Fields["policy"]; ok {
		lrsr.Policy = odbi.optionalStringFieldToPointer(policy)
	}
	if outputPort, ok := cacheLogicalRouterStaticRoute.Fields["output_port"]; ok {
		lrsr.OutputPort = odbi.optionalStringFieldToPointer(outputPort)
	}
	return lrsr
}

func (odbi *ovndb) lrsrListImp(lr string) ([]*LogicalRouterStaticRoute, error) {
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalRouter, ok := odbi.cache[TableLogicalRouter]
	if !ok {
		return nil, ErrorNotFound
	}
	for _, drows := range cacheLogicalRouter {
		if rlr, ok := drows.Fields["name"].(string); ok && rlr == lr {
			staticRoutes := drows.Fields["static_routes"]
			if staticRoutes != nil {
				switch staticRoutes.(type) {
				case libovsdb.OvsSet:
					if sr, ok := staticRoutes.(libovsdb.OvsSet); ok {
						listLRSR := make([]*LogicalRouterStaticRoute, 0, len(sr.GoSet))
						for _, s := range sr.GoSet {
							if sruid, ok := s.(libovsdb.UUID); ok {
								rsr := odbi.rowToLogicalRouterStaticRoute(sruid.GoUUID)
								listLRSR = append(listLRSR, rsr)
							}
						}
						return listLRSR, nil
					} else {
						return nil, fmt.Errorf("type libovsdb.OvsSet casting failed")
					}
				case libovsdb.UUID:
					if sruid, ok := staticRoutes.(libovsdb.UUID); ok {
						rsr := odbi.rowToLogicalRouterStaticRoute(sruid.GoUUID)
						return []*LogicalRouterStaticRoute{rsr}, nil
					} else {
						return nil, fmt.Errorf("type libovsdb.UUID casting failed")
					}
				default:
					return nil, fmt.Errorf("Unsupport type found in ovsdb rows")
				}
			}
			return []*LogicalRouterStaticRoute{}, nil
		}
	}

	return nil, ErrorNotFound
}

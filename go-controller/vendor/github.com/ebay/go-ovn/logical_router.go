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

// LogicalRouter ovnnb item
type LogicalRouter struct {
	UUID    string
	Name    string
	Enabled bool

	Ports        []string
	StaticRoutes []string
	NAT          []string
	LoadBalancer []string
	Policies     []string

	Options    map[interface{}]interface{}
	ExternalID map[interface{}]interface{}
}

func (odbi *ovndb) lrAddImp(name string, external_ids map[string]string) (*OvnCommand, error) {
	namedUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}

	row := make(OVNRow)
	row["name"] = name

	if external_ids != nil {
		oMap, err := libovsdb.NewOvsMap(external_ids)
		if err != nil {
			return nil, err
		}
		row["external_ids"] = oMap
	}

	if uuid := odbi.getRowUUID(TableLogicalRouter, row); len(uuid) > 0 {
		return nil, ErrorExist
	}

	insertOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    TableLogicalRouter,
		Row:      row,
		UUIDName: namedUUID,
	}

	operations := []libovsdb.Operation{insertOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lrDelImp(name string) (*OvnCommand, error) {
	condition := libovsdb.NewCondition("name", "==", name)
	deleteOp := libovsdb.Operation{
		Op:    opDelete,
		Table: TableLogicalRouter,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{deleteOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lrGetImp(name string) ([]*LogicalRouter, error) {
	var lrList []*LogicalRouter

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalRouter, ok := odbi.cache[TableLogicalRouter]
	if !ok {
		return nil, ErrorNotFound
	}

	for uuid, drows := range cacheLogicalRouter {
		if lrName, ok := drows.Fields["name"].(string); ok && lrName == name {
			lr := odbi.rowToLogicalRouter(uuid)
			lrList = append(lrList, lr)
		}
	}
	return lrList, nil
}

func (odbi *ovndb) rowToLogicalRouter(uuid string) *LogicalRouter {
	cacheLogicalRouter, ok := odbi.cache[TableLogicalRouter][uuid]
	if !ok {
		return nil
	}
	lr := &LogicalRouter{
		UUID:       uuid,
		Name:       cacheLogicalRouter.Fields["name"].(string),
		Options:    cacheLogicalRouter.Fields["options"].(libovsdb.OvsMap).GoMap,
		ExternalID: cacheLogicalRouter.Fields["external_ids"].(libovsdb.OvsMap).GoMap,
	}

	if enabled, ok := cacheLogicalRouter.Fields["enabled"]; ok {
		switch enabled.(type) {
		case bool:
			lr.Enabled = enabled.(bool)
		case libovsdb.OvsSet:
			if enabled.(libovsdb.OvsSet).GoSet == nil {
				lr.Enabled = true
			}
		}
	}

	if lbs, ok := cacheLogicalRouter.Fields["load_balancer"]; ok {
		switch lbs.(type) {
		case libovsdb.UUID:
			lr.LoadBalancer = []string{lbs.(libovsdb.UUID).GoUUID}
		case libovsdb.OvsSet:
			lr.LoadBalancer = odbi.ConvertGoSetToStringArray(lbs.(libovsdb.OvsSet))
		}
	}

	if ports, ok := cacheLogicalRouter.Fields["ports"]; ok {
		switch ports.(type) {
		case libovsdb.UUID:
			lr.Ports = []string{ports.(libovsdb.UUID).GoUUID}
		case libovsdb.OvsSet:
			lr.Ports = odbi.ConvertGoSetToStringArray(ports.(libovsdb.OvsSet))
		}
	}

	if lrsrs, ok := cacheLogicalRouter.Fields["static_routes"]; ok {
		switch lrsrs.(type) {
		case libovsdb.UUID:
			lr.StaticRoutes = []string{lrsrs.(libovsdb.UUID).GoUUID}
		case libovsdb.OvsSet:
			lr.StaticRoutes = odbi.ConvertGoSetToStringArray(lrsrs.(libovsdb.OvsSet))
		}
	}

	if nats, ok := cacheLogicalRouter.Fields["nat"]; ok {
		switch nats.(type) {
		case libovsdb.UUID:
			lr.NAT = []string{nats.(libovsdb.UUID).GoUUID}
		case libovsdb.OvsSet:
			lr.NAT = odbi.ConvertGoSetToStringArray(nats.(libovsdb.OvsSet))
		}
	}

	if policies, ok := cacheLogicalRouter.Fields["policies"]; ok {
		switch policies.(type) {
		case libovsdb.UUID:
			lr.Policies = []string{policies.(libovsdb.UUID).GoUUID}
		case libovsdb.OvsSet:
			lr.Policies = odbi.ConvertGoSetToStringArray(policies.(libovsdb.OvsSet))
		}
	}

	return lr
}

// Get all logical routers
func (odbi *ovndb) lrListImp() ([]*LogicalRouter, error) {
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalRouter, ok := odbi.cache[TableLogicalRouter]
	if !ok {
		return nil, ErrorNotFound
	}

	listLR := make([]*LogicalRouter, 0, len(cacheLogicalRouter))
	for uuid := range cacheLogicalRouter {
		listLR = append(listLR, odbi.rowToLogicalRouter(uuid))
	}

	return listLR, nil
}

func (odbi *ovndb) lrlbAddImp(lr string, lb string) (*OvnCommand, error) {
	var operations []libovsdb.Operation
	row := make(OVNRow)
	row["name"] = lb
	lbuuid := odbi.getRowUUID(TableLoadBalancer, row)
	if len(lbuuid) == 0 {
		return nil, ErrorNotFound
	}
	mutateUUID := []libovsdb.UUID{stringToGoUUID(lbuuid)}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	mutation := libovsdb.NewMutation("load_balancer", opInsert, mutateSet)
	if err != nil {
		return nil, err
	}
	row = make(OVNRow)
	row["name"] = lr
	lruuid := odbi.getRowUUID(TableLogicalRouter, row)
	if len(lruuid) == 0 {
		return nil, ErrorNotFound
	}
	condition := libovsdb.NewCondition("name", "==", lr)
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     TableLogicalRouter,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{condition},
	}
	operations = append(operations, mutateOp)
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lrlbDelImp(lr string, lb string) (*OvnCommand, error) {
	var operations []libovsdb.Operation
	row := make(OVNRow)
	row["name"] = lb
	lbuuid := odbi.getRowUUID(TableLoadBalancer, row)
	if len(lbuuid) == 0 {
		return nil, ErrorNotFound
	}
	mutateUUID := []libovsdb.UUID{stringToGoUUID(lbuuid)}
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
	mutation := libovsdb.NewMutation("load_balancer", opDelete, mutateSet)
	// mutate  lswitch for the corresponding load_balancer
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

func (odbi *ovndb) lrlbListImp(lr string) ([]*LoadBalancer, error) {
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalRouter, ok := odbi.cache[TableLogicalRouter]
	if !ok {
		return nil, ErrorSchema
	}
	for _, drows := range cacheLogicalRouter {
		if router, ok := drows.Fields["name"].(string); ok && router == lr {
			lbs := drows.Fields["load_balancer"]
			if lbs != nil {
				switch lbs.(type) {
				case libovsdb.OvsSet:
					if lb, ok := lbs.(libovsdb.OvsSet); ok {
						listLB := make([]*LoadBalancer, 0, len(lb.GoSet))
						for _, l := range lb.GoSet {
							if lb, ok := l.(libovsdb.UUID); ok {
								lb, err := odbi.rowToLB(lb.GoUUID)
								if err != nil {
									return nil, err
								}
								listLB = append(listLB, lb)
							}
						}
						return listLB, nil
					} else {
						return nil, fmt.Errorf("type libovsdb.OvsSet casting failed")
					}
				case libovsdb.UUID:
					if lb, ok := lbs.(libovsdb.UUID); ok {
						lb, err := odbi.rowToLB(lb.GoUUID)
						if err != nil {
							return nil, err
						}
						return []*LoadBalancer{lb}, nil
					} else {
						return nil, fmt.Errorf("type libovsdb.UUID casting failed")
					}
				default:
					return nil, fmt.Errorf("Unsupport type found in ovsdb rows")
				}
			}
			return []*LoadBalancer{}, nil
		}
	}
	return nil, ErrorNotFound
}

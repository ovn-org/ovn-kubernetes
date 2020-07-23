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

	if uuid := odbi.getRowUUID(tableLogicalRouter, row); len(uuid) > 0 {
		return nil, ErrorExist
	}

	insertOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    tableLogicalRouter,
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
		Table: tableLogicalRouter,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{deleteOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lrGetImp(name string) ([]*LogicalRouter, error) {
	var lrList []*LogicalRouter

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalRouter, ok := odbi.cache[tableLogicalRouter]
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
	lr := &LogicalRouter{
		UUID:       uuid,
		Name:       odbi.cache[tableLogicalRouter][uuid].Fields["name"].(string),
		Options:    odbi.cache[tableLogicalRouter][uuid].Fields["options"].(libovsdb.OvsMap).GoMap,
		ExternalID: odbi.cache[tableLogicalRouter][uuid].Fields["external_ids"].(libovsdb.OvsMap).GoMap,
	}

	if enabled, ok := odbi.cache[tableLogicalRouter][uuid].Fields["enabled"]; ok {
		switch enabled.(type) {
		case bool:
			lr.Enabled = enabled.(bool)
		case libovsdb.OvsSet:
			if enabled.(libovsdb.OvsSet).GoSet == nil {
				lr.Enabled = true
			}
		}
	}

	var lbs []string
	load_balancer := odbi.cache[tableLogicalRouter][uuid].Fields["load_balancer"]
	if load_balancer != nil {
		switch load_balancer.(type) {
		case libovsdb.OvsSet:
			if lb, ok := load_balancer.(libovsdb.OvsSet); ok {
				for _, l := range lb.GoSet {
					if lb, ok := l.(libovsdb.UUID); ok {
						lb, _ := odbi.rowToLB(lb.GoUUID)
						lbs = append(lbs, lb.Name)
					}
				}
			}
		case libovsdb.UUID:
			if lb, ok := load_balancer.(libovsdb.UUID); ok {
				lb, _ := odbi.rowToLB(lb.GoUUID)

				lbs = append(lbs, lb.Name)
			}
		}
	}
	lr.LoadBalancer = lbs

	var lps []string
	ports := odbi.cache[tableLogicalRouter][uuid].Fields["ports"]
	if ports != nil {
		switch ports.(type) {
		case string:
			lr.Ports = []string{ports.(string)}
		case libovsdb.OvsSet:
			if ps, ok := ports.(libovsdb.OvsSet); ok {
				for _, p := range ps.GoSet {
					if vp, ok := p.(libovsdb.UUID); ok {
						tp := odbi.rowToLogicalRouterPort(vp.GoUUID)
						lps = append(lps, tp.Name)
					}
				}
			}
		case libovsdb.UUID:
			if vp, ok := ports.(libovsdb.UUID); ok {
				tp := odbi.rowToLogicalRouterPort(vp.GoUUID)
				lps = append(lps, tp.Name)
			}
		}
	}
	lr.Ports = lps

	var listLRSR []string
	staticRoutes := odbi.cache[tableLogicalRouter][uuid].Fields["static_routes"]
	if staticRoutes != nil {
		switch staticRoutes.(type) {
		case libovsdb.OvsSet:
			if sr, ok := staticRoutes.(libovsdb.OvsSet); ok {
				for _, s := range sr.GoSet {
					if sruid, ok := s.(libovsdb.UUID); ok {
						rsr := odbi.rowToLogicalRouterStaticRoute(sruid.GoUUID)
						listLRSR = append(listLRSR, rsr.IPPrefix)
					}
				}
			}
		case libovsdb.UUID:
			if sruid, ok := staticRoutes.(libovsdb.UUID); ok {
				rsr := odbi.rowToLogicalRouterStaticRoute(sruid.GoUUID)
				listLRSR = append(listLRSR, rsr.IPPrefix)
			}
		}
	}
	lr.StaticRoutes = listLRSR

	var NATList []string
	nat := odbi.cache[tableLogicalRouter][uuid].Fields["nat"]
	if nat != nil {
		switch nat.(type) {
		case libovsdb.OvsSet:
			if sr, ok := nat.(libovsdb.OvsSet); ok {
				for _, s := range sr.GoSet {
					if sruid, ok := s.(libovsdb.UUID); ok {
						rsr := odbi.rowToNat(sruid.GoUUID)
						NATList = append(NATList, rsr.UUID)
					}
				}
			}
		case libovsdb.UUID:
			if sruid, ok := nat.(libovsdb.UUID); ok {
				rsr := odbi.rowToNat(sruid.GoUUID)
				NATList = append(NATList, rsr.UUID)
			}
		}
	}
	lr.NAT = NATList

	return lr
}

// Get all logical switches
func (odbi *ovndb) lrListImp() ([]*LogicalRouter, error) {
	var listLR []*LogicalRouter

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalRouter, ok := odbi.cache[tableLogicalRouter]
	if !ok {
		return nil, ErrorNotFound
	}

	for uuid := range cacheLogicalRouter {
		listLR = append(listLR, odbi.rowToLogicalRouter(uuid))
	}

	return listLR, nil
}

func (odbi *ovndb) lrlbAddImp(lr string, lb string) (*OvnCommand, error) {
	var operations []libovsdb.Operation
	row := make(OVNRow)
	row["name"] = lb
	lbuuid := odbi.getRowUUID(tableLoadBalancer, row)
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
	lruuid := odbi.getRowUUID(tableLogicalRouter, row)
	if len(lruuid) == 0 {
		return nil, ErrorNotFound
	}
	condition := libovsdb.NewCondition("name", "==", lr)
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalRouter,
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
	lbuuid := odbi.getRowUUID(tableLoadBalancer, row)
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
	lruuid := odbi.getRowUUID(tableLogicalRouter, row)
	if len(lruuid) == 0 {
		return nil, ErrorNotFound
	}
	mutation := libovsdb.NewMutation("load_balancer", opDelete, mutateSet)
	// mutate  lswitch for the corresponding load_balancer
	mucondition := libovsdb.NewCondition("name", "==", lr)
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalRouter,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{mucondition},
	}
	operations = append(operations, mutateOp)
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lrlbListImp(lr string) ([]*LoadBalancer, error) {
	var listLB []*LoadBalancer
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalRouter, ok := odbi.cache[tableLogicalRouter]
	if !ok {
		return nil, ErrorSchema
	}
	var lrFound bool
	for _, drows := range cacheLogicalRouter {
		if router, ok := drows.Fields["name"].(string); ok && router == lr {
			lbs := drows.Fields["load_balancer"]
			if lbs != nil {
				switch lbs.(type) {
				case libovsdb.OvsSet:
					if lb, ok := lbs.(libovsdb.OvsSet); ok {
						for _, l := range lb.GoSet {
							if lb, ok := l.(libovsdb.UUID); ok {
								lb, err := odbi.rowToLB(lb.GoUUID)
								if err != nil {
									return nil, err
								}
								listLB = append(listLB, lb)
							}
						}
					} else {
						return nil, fmt.Errorf("type libovsdb.OvsSet casting failed")
					}
				case libovsdb.UUID:
					if lb, ok := lbs.(libovsdb.UUID); ok {
						lb, err := odbi.rowToLB(lb.GoUUID)
						if err != nil {
							return nil, err
						}
						listLB = append(listLB, lb)
					} else {
						return nil, fmt.Errorf("type libovsdb.UUID casting failed")
					}
				default:
					return nil, fmt.Errorf("Unsupport type found in ovsdb rows")
				}
			}
			lrFound = true
			break
		}
	}
	if !lrFound {
		return nil, ErrorNotFound
	}
	return listLB, nil
}

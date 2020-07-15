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

// LogicalSwitch ovnnb item
type LogicalSwitch struct {
	UUID         string
	Name         string
	Ports        []string
	LoadBalancer []string
	ACLs         []string
	QoSRules     []string
	DNSRecords   []string
	OtherConfig  map[interface{}]interface{}
	ExternalID   map[interface{}]interface{}
}

func (odbi *ovndb) lsAddImp(lsw string) (*OvnCommand, error) {
	namedUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}

	//row to insert
	lswitch := make(OVNRow)
	lswitch["name"] = lsw

	if uuid := odbi.getRowUUID(tableLogicalSwitch, lswitch); len(uuid) > 0 {
		return nil, ErrorExist
	}

	insertOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    tableLogicalSwitch,
		Row:      lswitch,
		UUIDName: namedUUID,
	}
	operations := []libovsdb.Operation{insertOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lsDelImp(lsw string) (*OvnCommand, error) {
	condition := libovsdb.NewCondition("name", "==", lsw)
	deleteOp := libovsdb.Operation{
		Op:    opDelete,
		Table: tableLogicalSwitch,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{deleteOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) rowToLogicalSwitch(uuid string) *LogicalSwitch {
	cacheLogicalSwitch, ok := odbi.cache[tableLogicalSwitch][uuid]
	if !ok {
		return nil
	}

	ls := &LogicalSwitch{
		UUID:        uuid,
		Name:        cacheLogicalSwitch.Fields["name"].(string),
		OtherConfig: cacheLogicalSwitch.Fields["other_config"].(libovsdb.OvsMap).GoMap,
		ExternalID:  cacheLogicalSwitch.Fields["external_ids"].(libovsdb.OvsMap).GoMap,
	}
	if ports, ok := cacheLogicalSwitch.Fields["ports"]; ok {
		switch ports.(type) {
		case libovsdb.UUID:
			ls.Ports = []string{ports.(libovsdb.UUID).GoUUID}
		case libovsdb.OvsSet:
			ls.Ports = odbi.ConvertGoSetToStringArray(ports.(libovsdb.OvsSet))
		}
	}
	if lbs, ok := cacheLogicalSwitch.Fields["load_balancer"]; ok {
		switch lbs.(type) {
		case libovsdb.UUID:
			ls.LoadBalancer = []string{lbs.(libovsdb.UUID).GoUUID}
		case libovsdb.OvsSet:
			ls.LoadBalancer = odbi.ConvertGoSetToStringArray(lbs.(libovsdb.OvsSet))
		}
	}
	if acls, ok := cacheLogicalSwitch.Fields["acls"]; ok {
		switch acls.(type) {
		case libovsdb.UUID:
			ls.ACLs = []string{acls.(libovsdb.UUID).GoUUID}
		case libovsdb.OvsSet:
			ls.ACLs = odbi.ConvertGoSetToStringArray(acls.(libovsdb.OvsSet))
		}
	}
	if qosrules, ok := cacheLogicalSwitch.Fields["qos_rules"]; ok {
		switch qosrules.(type) {
		case libovsdb.UUID:
			ls.QoSRules = []string{qosrules.(libovsdb.UUID).GoUUID}
		case libovsdb.OvsSet:
			ls.QoSRules = odbi.ConvertGoSetToStringArray(qosrules.(libovsdb.OvsSet))
		}
	}
	if dnsrecords, ok := cacheLogicalSwitch.Fields["dns_records"]; ok {
		switch dnsrecords.(type) {
		case libovsdb.UUID:
			ls.DNSRecords = []string{dnsrecords.(libovsdb.UUID).GoUUID}
		case libovsdb.OvsSet:
			ls.DNSRecords = odbi.ConvertGoSetToStringArray(dnsrecords.(libovsdb.OvsSet))
		}
	}

	return ls
}

func (odbi *ovndb) lsGetImp(ls string) ([]*LogicalSwitch, error) {
	var lsList []*LogicalSwitch
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalSwitch, ok := odbi.cache[tableLogicalSwitch]
	if !ok {
		return nil, ErrorNotFound
	}

	for uuid, drows := range cacheLogicalSwitch {
		if rlsw, ok := drows.Fields["name"].(string); ok && rlsw == ls {
			lsList = append(lsList, odbi.rowToLogicalSwitch(uuid))
		}
	}

	if len(lsList) == 0 {
		return nil, ErrorNotFound
	}
	return lsList, nil
}

func (odbi *ovndb) lsListImp() ([]*LogicalSwitch, error) {
	var listLS []*LogicalSwitch

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalSwitch, ok := odbi.cache[tableLogicalSwitch]
	if !ok {
		return nil, ErrorSchema
	}

	for uuid := range cacheLogicalSwitch {
		listLS = append(listLS, odbi.rowToLogicalSwitch(uuid))
	}

	return listLS, nil
}

func (odbi *ovndb) lslbAddImp(lswitch string, lb string) (*OvnCommand, error) {
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
	row["name"] = lswitch
	lsuuid := odbi.getRowUUID(tableLogicalSwitch, row)
	if len(lsuuid) == 0 {
		return nil, ErrorNotFound
	}
	condition := libovsdb.NewCondition("name", "==", lswitch)
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalSwitch,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{condition},
	}
	operations = append(operations, mutateOp)
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lslbDelImp(lswitch string, lb string) (*OvnCommand, error) {
	var operations []libovsdb.Operation
	row := make(OVNRow)
	row["name"] = lb
	lbuuid := odbi.getRowUUID(tableLoadBalancer, row)
	if len(lbuuid) == 0 {
		return nil, ErrorNotFound
	}
	row = make(OVNRow)
	row["name"] = lswitch
	lsuuid := odbi.getRowUUID(tableLogicalSwitch, row)
	if len(lsuuid) == 0 {
		return nil, ErrorNotFound
	}
	mutateUUID := []libovsdb.UUID{stringToGoUUID(lbuuid)}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}
	mutation := libovsdb.NewMutation("load_balancer", opDelete, mutateSet)
	// mutate  lswitch for the corresponding load_balancer
	mucondition := libovsdb.NewCondition("name", "==", lswitch)
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalSwitch,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{mucondition},
	}
	operations = append(operations, mutateOp)
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lslbListImp(lswitch string) ([]*LoadBalancer, error) {
	var listLB []*LoadBalancer
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalSwitch, ok := odbi.cache[tableLogicalSwitch]
	if !ok {
		return nil, ErrorSchema
	}
	var lsFound bool
	for _, drows := range cacheLogicalSwitch {
		if rlsw, ok := drows.Fields["name"].(string); ok && rlsw == lswitch {
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
			lsFound = true
			break
		}
	}
	if !lsFound {
		return nil, ErrorNotFound
	}
	return listLB, nil
}

func (odbi *ovndb) lsExtIdsAddImp(ls string, external_ids map[string]string) (*OvnCommand, error) {
	var operations []libovsdb.Operation
	row := make(OVNRow)
	row["name"] = ls
	lsuuid := odbi.getRowUUID(tableLogicalSwitch, row)
	if len(lsuuid) == 0 {
		return nil, ErrorNotFound
	}
	if len(external_ids) == 0 {
		return nil, fmt.Errorf("external_ids is nil or empty")
	}
	mutateSet, err := libovsdb.NewOvsMap(external_ids)
	if err != nil {
		return nil, err
	}
	mutation := libovsdb.NewMutation("external_ids", opInsert, mutateSet)
	condition := libovsdb.NewCondition("name", "==", ls)
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalSwitch,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{condition},
	}
	operations = append(operations, mutateOp)
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lsExtIdsDelImp(ls string, external_ids map[string]string) (*OvnCommand, error) {
	var operations []libovsdb.Operation
	row := make(OVNRow)
	row["name"] = ls
	lsuuid := odbi.getRowUUID(tableLogicalSwitch, row)
	if len(lsuuid) == 0 {
		return nil, ErrorNotFound
	}
	if len(external_ids) == 0 {
		return nil, fmt.Errorf("external_ids is nil or empty")
	}
	mutateSet, err := libovsdb.NewOvsMap(external_ids)
	if err != nil {
		return nil, err
	}
	mutation := libovsdb.NewMutation("external_ids", opDelete, mutateSet)
	condition := libovsdb.NewCondition("name", "==", ls)
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalSwitch,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{condition},
	}
	operations = append(operations, mutateOp)
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) linkSwitchToRouterImp(lsw, lsp, lr, lrp, lrpMac string, networks []string, externalIds map[string]string) (*OvnCommand, error) {
	// validate logical switch
	row := make(OVNRow)
	row["name"] = lsw
	lswUUID := odbi.getRowUUID(tableLogicalSwitch, row)
	if len(lswUUID) == 0 {
		return nil, fmt.Errorf("logical switch %s not found", lsw)
	}
	// add logical router port
	strLrpUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}
	row = make(OVNRow)
	row["name"] = lrp
	row["mac"] = lrpMac
	// validate
	if uuid := odbi.getRowUUID(tableLogicalRouterPort, row); len(uuid) > 0 {
		return nil, fmt.Errorf("logical router port %s already existed", lrp)
	}
	networkSet, err := libovsdb.NewOvsSet(networks)
	if err != nil {
		return nil, err
	}
	row["networks"] = networkSet
	if externalIds != nil {
		oMap, err := libovsdb.NewOvsMap(externalIds)
		if err != nil {
			return nil, err
		}
		row["external_ids"] = oMap
	}
	addLrpOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    tableLogicalRouterPort,
		Row:      row,
		UUIDName: strLrpUUID,
	}

	// add lrp to lr
	objLrpUUID := []libovsdb.UUID{stringToGoUUID(strLrpUUID)}
	lrpSet, err := libovsdb.NewOvsSet(objLrpUUID)
	if err != nil {
		return nil, err
	}
	lrPortChange := libovsdb.NewMutation("ports", opInsert, lrpSet)
	lrCondition := libovsdb.NewCondition("name", "==", lr)
	addLrpToLrOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalRouter,
		Mutations: []interface{}{lrPortChange},
		Where:     []interface{}{lrCondition},
	}

	// add logical switch port
	strLspUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}
	port := make(OVNRow)
	port["name"] = lsp
	port["type"] = "router"
	port["addresses"] = "router"
	options := make(map[string]string)
	options["router-port"] = lrp
	optMap, _ := libovsdb.NewOvsMap(options)
	port["options"] = optMap
	if uuid := odbi.getRowUUID(tableLogicalSwitchPort, port); len(uuid) > 0 {
		return nil, ErrorExist
	}
	addLspOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    tableLogicalSwitchPort,
		Row:      port,
		UUIDName: strLspUUID,
	}

	// add logical switch port to switch
	objLspUUID := []libovsdb.UUID{stringToGoUUID(strLspUUID)}
	lspSet, err := libovsdb.NewOvsSet(objLspUUID)
	if err != nil {
		return nil, err
	}
	lsPortChange := libovsdb.NewMutation("ports", opInsert, lspSet)
	lsCondition := libovsdb.NewCondition("name", "==", lsw)
	addLspToLsOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalSwitch,
		Mutations: []interface{}{lsPortChange},
		Where:     []interface{}{lsCondition},
	}

	operations := []libovsdb.Operation{addLrpOp, addLrpToLrOp, addLspOp, addLspToLsOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

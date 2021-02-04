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
	"strings"

	"github.com/ebay/libovsdb"
)

// LoadBalancer ovnnb item
type LoadBalancer struct {
	UUID            string
	Name            string
	vips            map[interface{}]interface{}
	protocol        string
	selectionFields string
	ExternalID      map[interface{}]interface{}
}

func (odbi *ovndb) lbUpdateImp(name string, vipPort string, protocol string, addrs []string) (*OvnCommand, error) {
	row := make(OVNRow)

	// prepare vips map
	vipMap := make(map[string]string)
	vipMap[vipPort] = strings.Join(addrs, ",")

	oMap, err := libovsdb.NewOvsMap(vipMap)
	if err != nil {
		return nil, err
	}

	row["vips"] = oMap
	row["protocol"] = protocol

	condition := libovsdb.NewCondition("name", "==", name)

	insertOp := libovsdb.Operation{
		Op:    opUpdate,
		Table: TableLoadBalancer,
		Row:   row,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{insertOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lbAddImp(name string, vipPort string, protocol string, addrs []string) (*OvnCommand, error) {
	var operations []libovsdb.Operation
	namedUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}

	row := make(OVNRow)
	row["name"] = name

	if uuid := odbi.getRowUUID(TableLoadBalancer, row); len(uuid) > 0 {
		return nil, ErrorExist
	}

	// prepare vips map
	vipMap := make(map[string]string)
	vipMap[vipPort] = strings.Join(addrs, ",")

	oMap, err := libovsdb.NewOvsMap(vipMap)
	if err != nil {
		return nil, err
	}
	row["vips"] = oMap
	row["protocol"] = protocol

	insertOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    TableLoadBalancer,
		Row:      row,
		UUIDName: namedUUID,
	}
	operations = append(operations, insertOp)
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lbDelImp(name string) (*OvnCommand, error) {
	var operations []libovsdb.Operation

	condition := libovsdb.NewCondition("name", "==", name)
	deleteOp := libovsdb.Operation{
		Op:    opDelete,
		Table: TableLoadBalancer,
		Where: []interface{}{condition},
	}
	// Also delete references from Logical switches
	row := make(OVNRow)
	row["name"] = name
	lbuuid := odbi.getRowUUID(TableLoadBalancer, row)
	if len(lbuuid) == 0 {
		return nil, ErrorNotFound
	}
	mutateUUID := []libovsdb.UUID{stringToGoUUID(lbuuid)}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}
	mutation := libovsdb.NewMutation("load_balancer", opDelete, mutateSet)
	lswitches, err := odbi.getRowsMatchingUUID(TableLogicalSwitch, "load_balancer", lbuuid)
	if err != nil && err != ErrorNotFound {
		return nil, err
	} else if err == nil {
		// mutate all matching lswitches for the corresponding load_balancer
		for _, lswitch := range lswitches {
			mucondition := libovsdb.NewCondition("_uuid", "==", stringToGoUUID(lswitch))
			mutateOp := libovsdb.Operation{
				Op:        opMutate,
				Table:     TableLogicalSwitch,
				Mutations: []interface{}{mutation},
				Where:     []interface{}{mucondition},
			}
			operations = append(operations, mutateOp)
		}
	}
	operations = append(operations, deleteOp)
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lbGetImp(name string) ([]*LoadBalancer, error) {
	var listLB []*LoadBalancer

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLoadBalancer, ok := odbi.cache[TableLoadBalancer]
	if !ok {
		return nil, ErrorSchema
	}

	for uuid, drows := range cacheLoadBalancer {
		if lbName, ok := drows.Fields["name"].(string); ok && lbName == name {
			lb, err := odbi.rowToLB(uuid)
			if err != nil {
				return nil, err
			}
			listLB = append(listLB, lb)
		}
	}
	return listLB, nil
}

func (odbi *ovndb) lbSetSelectionFieldsImp(name string, selectionFields string) (*OvnCommand, error) {
	row := make(OVNRow)
	row["selection_fields"] = selectionFields

	condition := libovsdb.NewCondition("name", "==", name)

	updateOp := libovsdb.Operation{
		Op:    opUpdate,
		Table: TableLoadBalancer,
		Row:   row,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{updateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) rowToLB(uuid string) (*LoadBalancer, error) {
	cacheLoadBalancer, ok := odbi.cache[TableLoadBalancer][uuid]
	if !ok {
		return nil, ErrorSchema
	}

	lb := &LoadBalancer{
		UUID:       uuid,
		protocol:   cacheLoadBalancer.Fields["protocol"].(string),
		Name:       cacheLoadBalancer.Fields["name"].(string),
		vips:       cacheLoadBalancer.Fields["vips"].(libovsdb.OvsMap).GoMap,
		ExternalID: cacheLoadBalancer.Fields["external_ids"].(libovsdb.OvsMap).GoMap,
	}

	if fields, ok := cacheLoadBalancer.Fields["selection_fields"].(string); ok {
		lb.selectionFields = fields
	}
	return lb, nil
}

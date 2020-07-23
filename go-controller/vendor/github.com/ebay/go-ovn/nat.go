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
	"github.com/ebay/libovsdb"
)

// NAT ovnnb item
type NAT struct {
	UUID        string
	Type        string
	ExternalIP  string
	ExternalMAC string
	LogicalIP   string
	LogicalPort string
	ExternalID  map[interface{}]interface{}
}

func (odbi *ovndb) rowToNat(uuid string) *NAT {
	cacheNAT, ok := odbi.cache[tableNAT][uuid]
	if !ok {
		return nil
	}

	nat := &NAT{
		UUID:       uuid,
		Type:       cacheNAT.Fields["type"].(string),
		ExternalIP: cacheNAT.Fields["external_ip"].(string),
		LogicalIP:  cacheNAT.Fields["logical_ip"].(string),
		ExternalID: cacheNAT.Fields["external_ids"].(libovsdb.OvsMap).GoMap,
	}

	if mac, ok := cacheNAT.Fields["external_mac"]; ok {
		switch mac.(type) {
		case libovsdb.UUID:
			nat.ExternalMAC = mac.(libovsdb.UUID).GoUUID
		case string:
			nat.ExternalMAC = mac.(string)
		}
	}

	if lip, ok := cacheNAT.Fields["logical_port"]; ok {
		switch lip.(type) {
		case libovsdb.UUID:
			nat.LogicalIP = lip.(libovsdb.UUID).GoUUID
		case string:
			nat.LogicalIP = lip.(string)
		}

	}

	return nat
}

func (odbi *ovndb) lrNatAddImp(lr string, ntype string, externalIp string, logicalIp string, external_ids map[string]string, logicalPortAndExternalMac ...string) (*OvnCommand, error) {
	nameUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}
	row := make(OVNRow)

	row["external_ip"] = externalIp

	row["logical_ip"] = logicalIp

	switch ntype {
	case "snat":
		row["type"] = ntype
	case "dnat":
		row["type"] = ntype
	case "dnat_and_snat":
		row["type"] = ntype
	default:
		return nil, ErrorOption
	}

	if uuid := odbi.getRowUUID(tableNAT, row); len(uuid) > 0 {
		return nil, ErrorExist
	}

	// The logical_port and  external_mac  are  only  accepted
	// when  router  is  a  distributed  router  (rather than a gateway
	// router) and type is dnat_and_snat.
	if row["type"] == "dnat_and_snat" {
		switch len(logicalPortAndExternalMac) {
		case 0:
		case 2:
			row["logical_port"] = logicalPortAndExternalMac[0]
			row["external_mac"] = logicalPortAndExternalMac[1]
		default:
			return nil, ErrorOption
		}
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
		Table:    tableNAT,
		Row:      row,
		UUIDName: nameUUID,
	}

	mutateUUID := []libovsdb.UUID{stringToGoUUID(nameUUID)}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}

	mutation := libovsdb.NewMutation("nat", opInsert, mutateSet)
	condition := libovsdb.NewCondition("name", "==", lr)
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalRouter,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{condition},
	}

	operations := []libovsdb.Operation{insertOp, mutateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

// Deletes  NATs  from  router. If only router is supplied, all the
// NATs from the logical router are deleted. If type is also speci‐
// fied, then all the NATs that match the type will be deleted from
// the logical router. If all the fields are given, then  a  single
// NAT  rule that matches all the fields will be deleted. When type
// is snat, the ip should be  logical_ip.  When  type  is  dnat  or
// dnat_and_snat, the ip shoud be external_ip.
func (odbi *ovndb) lrNatDelImp(lr string, ntype string, ip ...string) (*OvnCommand, error) {
	var operations []libovsdb.Operation

	row := make(OVNRow)

	switch ntype {
	case "snat":
		row["type"] = ntype
		if len(ip) != 0 {
			row["logical_ip"] = ip[0]
		}
	case "dnat":
		row["type"] = ntype
		if len(ip) != 0 {
			row["external_ip"] = ip[0]
		}
	case "dnat_and_snat":
		row["type"] = ntype
		if len(ip) != 0 {
			row["external_ip"] = ip[0]
		}
	case "":
	default:
		return nil, ErrorOption
	}

	lrNatUUID := odbi.getRowUUIDs(tableNAT, row)
	if len(lrNatUUID) == 0 {
		return nil, ErrorNotFound
	}

	LRs, err := odbi.LRGet(lr)
	if err != nil {
		return nil, err
	}
	if len(LRs) == 0 {
		return nil, ErrorNotFound
	}
	natlist := make([]string, len(LRs[0].NAT))
	for i, v := range LRs[0].NAT {
		natlist[i] = v
	}

	var mutateUUID []libovsdb.UUID
	for _, v := range natlist {
		for s, lv := range lrNatUUID {
			switch lv {
			case v:
				mutateUUID = append(mutateUUID, libovsdb.UUID{GoUUID: lrNatUUID[s]})
			case "":
				mutateUUID = append(mutateUUID, libovsdb.UUID{GoUUID: v})
			}
		}
	}

	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}

	lrNatUUID = odbi.getRowUUIDs(tableNAT, row)
	if len(lrNatUUID) == 0 {
		return nil, ErrorNotFound
	}

	row = make(OVNRow)
	row["name"] = lr
	mutation := libovsdb.NewMutation("nat", opDelete, mutateSet)
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

func (odbi *ovndb) lrNatListImp(lr string) ([]*NAT, error) {
	LRs, err := odbi.LRGet(lr)
	if err != nil {
		return nil, err
	}

	natlist := make([]*NAT, len(LRs[0].NAT))

	for i, v := range LRs[0].NAT {
		natlist[i] = odbi.rowToNat(v)
	}

	return natlist, nil
}

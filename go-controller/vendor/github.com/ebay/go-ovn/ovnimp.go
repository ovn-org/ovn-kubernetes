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
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/ebay/libovsdb"
)

var (
	// ErrorOption used when invalid args specified
	ErrorOption = errors.New("invalid option specified")
	// ErrorSchema used when something wrong in ovnnb
	ErrorSchema = errors.New("table schema error")
	// ErrorNotFound used when object not found in ovnnb
	ErrorNotFound = errors.New("object not found")
	// ErrorExist used when object already exists in ovnnb
	ErrorExist = errors.New("object exist")
)

// OVNRow ovn nb/sb row
type OVNRow map[string]interface{}

func (odbi *ovndb) getRowUUIDs(table string, row OVNRow) []string {
	var uuids []string
	var wildcard bool

	if reflect.DeepEqual(row, make(OVNRow)) {
		wildcard = true
	}

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheTable, ok := odbi.cache[table]
	if !ok {
		return nil
	}

	for uuid, drows := range cacheTable {
		if wildcard {
			uuids = append(uuids, uuid)
			continue
		}

		found := false
		for field, value := range row {
			if v, ok := drows.Fields[field]; ok {
				if v == value {
					found = true
				} else {
					found = false
					break
				}
			}
		}
		if found {
			uuids = append(uuids, uuid)
		}
	}

	return uuids
}

func (odbi *ovndb) getRowUUID(table string, row OVNRow) string {
	uuids := odbi.getRowUUIDs(table, row)
	if len(uuids) > 0 {
		return uuids[0]
	}
	return ""
}

//test if map s contains t
//This function is not both s and t are nil at same time
func (odbi *ovndb) oMapContians(s, t map[interface{}]interface{}) bool {
	if s == nil || t == nil {
		return false
	}

	for tk, tv := range t {
		if sv, ok := s[tk]; !ok {
			return false
		} else if tv != sv {
			return false
		}
	}
	return true
}

func (odbi *ovndb) getRowUUIDContainsUUID(table, field, uuid string) (string, error) {
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheTable, ok := odbi.cache[table]
	if !ok {
		return "", ErrorSchema
	}

	for id, drows := range cacheTable {
		v := fmt.Sprintf("%s", drows.Fields[field])
		if strings.Contains(v, uuid) {
			return id, nil
		}
	}
	return "", ErrorNotFound
}

func (odbi *ovndb) getRowsMatchingUUID(table, field, uuid string) ([]string, error) {
	odbi.cachemutex.Lock()
	defer odbi.cachemutex.Unlock()
	var uuids []string
	for id, drows := range odbi.cache[table] {
		v := fmt.Sprintf("%s", drows.Fields[field])
		if strings.Contains(v, uuid) {
			uuids = append(uuids, id)
		}
	}
	if len(uuids) == 0 {
		return uuids, ErrorNotFound
	}
	return uuids, nil
}

func (odbi *ovndb) transact(db string, ops ...libovsdb.Operation) ([]libovsdb.OperationResult, error) {
	// Only support one trans at same time now.
	odbi.tranmutex.Lock()
	defer odbi.tranmutex.Unlock()
	reply, err := odbi.client.Transact(db, ops...)

	if err != nil {
		return reply, err
	}

	if len(reply) < len(ops) {
		for i, o := range reply {
			if o.Error != "" && i < len(ops) {
				return nil, fmt.Errorf("Transaction Failed due to an error : %v details: %v in %v", o.Error, o.Details, ops[i])
			}
		}
		return reply, fmt.Errorf("Number of Replies should be atleast equal to number of operations")
	}
	return reply, nil
}

func (odbi *ovndb) execute(cmds ...*OvnCommand) error {
	if cmds == nil {
		return nil
	}
	var ops []libovsdb.Operation
	for _, cmd := range cmds {
		if cmd != nil {
			ops = append(ops, cmd.Operations...)
		}
	}

	_, err := odbi.transact(odbi.db, ops...)
	if err != nil {
		return err
	}
	return nil
}

func (odbi *ovndb) float64_to_int(row libovsdb.Row) {
	for field, value := range row.Fields {
		if v, ok := value.(float64); ok {
			n := int(v)
			if float64(n) == v {
				row.Fields[field] = n
			}
		}
	}
}

func (odbi *ovndb) populateCache(updates libovsdb.TableUpdates) {
	empty := libovsdb.Row{}

	odbi.cachemutex.Lock()
	defer odbi.cachemutex.Unlock()

	for _, table := range tablesOrder {
		tableUpdate, ok := updates.Updates[table]
		if !ok {
			continue
		}

		if _, ok := odbi.cache[table]; !ok {
			odbi.cache[table] = make(map[string]libovsdb.Row)
		}
		for uuid, row := range tableUpdate.Rows {
			// TODO: this is a workaround for the problem of
			// missing json number conversion in libovsdb
			odbi.float64_to_int(row.New)

			if !reflect.DeepEqual(row.New, empty) {
				if reflect.DeepEqual(row.New, odbi.cache[table][uuid]) {
					// Already existed and unchanged, ignore (this can happen when auto-reconnect)
					continue
				}
				odbi.cache[table][uuid] = row.New

				if odbi.signalCB != nil {
					switch table {
					case tableLogicalRouter:
						lr := odbi.rowToLogicalRouter(uuid)
						odbi.signalCB.OnLogicalRouterCreate(lr)
					case tableLogicalRouterPort:
						lrp := odbi.rowToLogicalRouterPort(uuid)
						odbi.signalCB.OnLogicalRouterPortCreate(lrp)
					case tableLogicalRouterStaticRoute:
						lrsr := odbi.rowToLogicalRouterStaticRoute(uuid)
						odbi.signalCB.OnLogicalRouterStaticRouteCreate(lrsr)
					case tableLogicalSwitch:
						ls := odbi.rowToLogicalSwitch(uuid)
						odbi.signalCB.OnLogicalSwitchCreate(ls)
					case tableLogicalSwitchPort:
						lp, err := odbi.rowToLogicalPort(uuid)
						if err == nil {
							odbi.signalCB.OnLogicalPortCreate(lp)
						}
					case tableACL:
						acl := odbi.rowToACL(uuid)
						odbi.signalCB.OnACLCreate(acl)
					case tableDHCPOptions:
						dhcp := odbi.rowToDHCPOptions(uuid)
						odbi.signalCB.OnDHCPOptionsCreate(dhcp)
					case tableQoS:
						qos := odbi.rowToQoS(uuid)
						odbi.signalCB.OnQoSCreate(qos)
					case tableLoadBalancer:
						lb, _ := odbi.rowToLB(uuid)
						odbi.signalCB.OnLoadBalancerCreate(lb)
					case tableMeter:
						meter := odbi.rowToMeter(uuid)
						odbi.signalCB.onMeterCreate(meter)
					case tableMeterBand:
						band, _ := odbi.rowToMeterBand(uuid)
						odbi.signalCB.onMeterBandCreate(band)
					case tableChassis:
						chassis, _ := odbi.rowToChassis(uuid)
						odbi.signalCB.onChassisCreate(chassis)
					case tableEncap:
						encap, _ := odbi.rowToEncap(uuid)
						odbi.signalCB.onEncapCreate(encap)
					}
				}
			} else {
				defer delete(odbi.cache[table], uuid)

				if odbi.signalCB != nil {
					defer func(table, uuid string) {
						switch table {
						case tableLogicalRouter:
							lr := odbi.rowToLogicalRouter(uuid)
							odbi.signalCB.OnLogicalRouterDelete(lr)
						case tableLogicalRouterPort:
							lrp := odbi.rowToLogicalRouterPort(uuid)
							odbi.signalCB.OnLogicalRouterPortDelete(lrp)
						case tableLogicalRouterStaticRoute:
							lrsr := odbi.rowToLogicalRouterStaticRoute(uuid)
							odbi.signalCB.OnLogicalRouterStaticRouteDelete(lrsr)
						case tableLogicalSwitch:
							ls := odbi.rowToLogicalSwitch(uuid)
							odbi.signalCB.OnLogicalSwitchDelete(ls)
						case tableLogicalSwitchPort:
							lp, err := odbi.rowToLogicalPort(uuid)
							if err == nil {
								odbi.signalCB.OnLogicalPortDelete(lp)
							}
						case tableACL:
							acl := odbi.rowToACL(uuid)
							odbi.signalCB.OnACLDelete(acl)
						case tableDHCPOptions:
							dhcp := odbi.rowToDHCPOptions(uuid)
							odbi.signalCB.OnDHCPOptionsDelete(dhcp)
						case tableQoS:
							qos := odbi.rowToQoS(uuid)
							odbi.signalCB.OnQoSDelete(qos)
						case tableLoadBalancer:
							lb, _ := odbi.rowToLB(uuid)
							odbi.signalCB.OnLoadBalancerDelete(lb)
						case tableMeter:
							meter := odbi.rowToMeter(uuid)
							odbi.signalCB.onMeterDelete(meter)
						case tableMeterBand:
							band, _ := odbi.rowToMeterBand(uuid)
							odbi.signalCB.onMeterBandDelete(band)
						case tableChassis:
							chassis, _ := odbi.rowToChassis(uuid)
							odbi.signalCB.onChassisDelete(chassis)
						case tableEncap:
							encap, _ := odbi.rowToEncap(uuid)
							odbi.signalCB.onEncapDelete(encap)
						}
					}(table, uuid)
				}
			}
		}
	}
}

func (odbi *ovndb) ConvertGoSetToStringArray(oset libovsdb.OvsSet) []string {
	var ret = []string{}
	for _, s := range oset.GoSet {
		value, ok := s.(string)
		if ok {
			ret = append(ret, value)
		}
	}
	return ret
}

func stringToGoUUID(uuid string) libovsdb.UUID {
	return libovsdb.UUID{GoUUID: uuid}
}

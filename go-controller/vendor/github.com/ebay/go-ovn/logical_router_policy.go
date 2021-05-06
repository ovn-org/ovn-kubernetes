/**
 * Copyright (c) 2021 Red Hat
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

// LogicalRouterPolicy ovnnb item
type LogicalRouterPolicy struct {
	UUID       string
	Priority   int
	Match      string
	Action     string
	Nexthop    *string
	NextHops   []string
	Options    map[interface{}]interface{}
	ExternalID map[interface{}]interface{}
}

func (odbi *ovndb) lrpolicyAddImp(lr string, priority int, match string, action string, nexthop *string, nexthops []string, options map[string]string, external_ids map[string]string) (*OvnCommand, error) {
	namedUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}

	row := make(OVNRow)
	row["priority"] = priority
	row["match"] = match
	row["action"] = action

	if nexthop != nil {
		row["nexthop"] = *nexthop
	}
	if nexthops != nil {
		nexthopsSet, err := libovsdb.NewOvsSet(nexthops)
		if err != nil {
			return nil, err
		}
		row["nexthops"] = nexthopsSet
	}
	if options != nil {
		optionsMap, err := libovsdb.NewOvsMap(options)
		if err != nil {
			return nil, err
		}
		row["options"] = optionsMap
	}
	if external_ids != nil {
		oMap, err := libovsdb.NewOvsMap(external_ids)
		if err != nil {
			return nil, err
		}
		row["external_ids"] = oMap
	}

	if uuid := odbi.getRowUUID(TableLogicalRouterPolicy, row); len(uuid) > 0 {
		return nil, ErrorExist
	}

	insertOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    TableLogicalRouterPolicy,
		Row:      row,
		UUIDName: namedUUID,
	}

	mutateUUID := []libovsdb.UUID{stringToGoUUID(namedUUID)}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}
	mutation := libovsdb.NewMutation("policies", opInsert, mutateSet)
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

func (odbi *ovndb) lrpolicyDelImp(lr string, priority int, match *string) (*OvnCommand, error) {
	if lr == "" {
		return nil, fmt.Errorf("lr (logical router name) is required")
	}

	row := make(OVNRow)
	row["name"] = lr
	lruuid := odbi.getRowUUID(TableLogicalRouter, row)
	if len(lruuid) == 0 {
		return nil, ErrorNotFound
	}

	var mutateSet *libovsdb.OvsSet
	var err error

	if match != nil {
		// delete all with same priority and match
		row := make(OVNRow)
		row["prority"] = priority
		row["match"] = *match
		lrpolicyuuids := odbi.getRowUUIDs(TableLogicalRouterPolicy, row)
		if len(lrpolicyuuids) == 0 {
			return nil, ErrorNotFound
		}
		delUUIDs := make([]libovsdb.UUID, len(lrpolicyuuids))
		for i, uuid := range lrpolicyuuids {
			delUUIDs[i] = stringToGoUUID(uuid)
		}
		mutateSet, err = libovsdb.NewOvsSet(delUUIDs)
		if err != nil {
			return nil, err
		}
	} else {
		row := make(OVNRow)
		row["prority"] = priority
		lrpolicyuuids := odbi.getRowUUIDs(TableLogicalRouterPolicy, row)
		if len(lrpolicyuuids) == 0 {
			return nil, ErrorNotFound
		}
		delUUIDs := make([]libovsdb.UUID, len(lrpolicyuuids))
		for i, uuid := range lrpolicyuuids {
			delUUIDs[i] = stringToGoUUID(uuid)
		}
		mutateSet, err = libovsdb.NewOvsSet(delUUIDs)
		if err != nil {
			return nil, err
		}
	}

	mutation := libovsdb.NewMutation("policies", opDelete, mutateSet)
	// mutate  lrouter for the corresponding policies
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

func (odbi *ovndb) lrpolicyDelByUUIDImp(lr string, uuid string) (*OvnCommand, error) {
	if lr == "" {
		return nil, fmt.Errorf("lr (logical router name) is required")
	}
	row := make(OVNRow)
	row["name"] = lr
	lruuid := odbi.getRowUUID(TableLogicalRouter, row)
	if len(lruuid) == 0 {
		return nil, ErrorNotFound
	}
	// delete with specified uuid
	mutateSet, err := libovsdb.NewOvsSet([]libovsdb.UUID{stringToGoUUID(uuid)})
	if err != nil {
		return nil, err
	}
	mutation := libovsdb.NewMutation("policies", opDelete, mutateSet)
	// mutate  lrouter for the corresponding policies
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

func (odbi *ovndb) lrpolicyDelAllImp(lr string) (*OvnCommand, error) {
	if lr == "" {
		return nil, fmt.Errorf("lr (logical router name) is required")
	}

	row := make(OVNRow)
	row["name"] = lr
	lruuid := odbi.getRowUUID(TableLogicalRouter, row)
	if len(lruuid) == 0 {
		return nil, ErrorNotFound
	}

	// delete everything
	row = make(OVNRow)
	lrpolicyuuids := odbi.getRowUUIDs(TableLogicalRouterPolicy, row)
	if len(lrpolicyuuids) == 0 {
		return nil, ErrorNotFound
	}
	delUUIDs := make([]libovsdb.UUID, len(lrpolicyuuids))
	for i, uuid := range lrpolicyuuids {
		delUUIDs[i] = stringToGoUUID(uuid)
	}
	mutateSet, err := libovsdb.NewOvsSet(delUUIDs)
	if err != nil {
		return nil, err
	}

	mutation := libovsdb.NewMutation("policies", opDelete, mutateSet)
	// mutate  lrouter for the corresponding policies
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

func (odbi *ovndb) rowToLogicalRouterPolicy(uuid string) *LogicalRouterPolicy {
	cacheLogicalRouterPolicy, ok := odbi.cache[TableLogicalRouterPolicy][uuid]
	if !ok {
		return nil
	}
	lrpolicy := &LogicalRouterPolicy{
		UUID:       uuid,
		Priority:   cacheLogicalRouterPolicy.Fields["priority"].(int),
		Match:      cacheLogicalRouterPolicy.Fields["match"].(string),
		Action:     cacheLogicalRouterPolicy.Fields["action"].(string),
		Options:    cacheLogicalRouterPolicy.Fields["options"].(libovsdb.OvsMap).GoMap,
		ExternalID: cacheLogicalRouterPolicy.Fields["external_ids"].(libovsdb.OvsMap).GoMap,
	}

	if nexthop, ok := cacheLogicalRouterPolicy.Fields["nexthop"]; ok {
		lrpolicy.Nexthop = odbi.optionalStringFieldToPointer(nexthop)
	}

	for _, n := range cacheLogicalRouterPolicy.Fields["nexthops"].(libovsdb.OvsSet).GoSet {
		lrpolicy.NextHops = append(lrpolicy.NextHops, n.(string))
	}
	return lrpolicy
}

func (odbi *ovndb) lrPolicyListImp(lr string) ([]*LogicalRouterPolicy, error) {
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()
	cacheLogicalRouter, ok := odbi.cache[TableLogicalRouter]
	if !ok {
		return nil, ErrorNotFound
	}
	for _, drows := range cacheLogicalRouter {
		if rlr, ok := drows.Fields["name"].(string); ok && rlr == lr {
			policies := drows.Fields["policies"]
			if policies != nil {
				switch policies.(type) {
				case libovsdb.OvsSet:
					if sr, ok := policies.(libovsdb.OvsSet); ok {
						listLRPolicy := make([]*LogicalRouterPolicy, 0, len(sr.GoSet))
						for _, s := range sr.GoSet {
							if sruid, ok := s.(libovsdb.UUID); ok {
								policy := odbi.rowToLogicalRouterPolicy(sruid.GoUUID)
								listLRPolicy = append(listLRPolicy, policy)
							}
						}
						return listLRPolicy, nil
					} else {
						return nil, fmt.Errorf("type libovsdb.OvsSet casting failed")
					}
				case libovsdb.UUID:
					if policyuuid, ok := policies.(libovsdb.UUID); ok {
						policy := odbi.rowToLogicalRouterPolicy(policyuuid.GoUUID)
						return []*LogicalRouterPolicy{policy}, nil
					} else {
						return nil, fmt.Errorf("type libovsdb.UUID casting failed")
					}
				default:
					return nil, fmt.Errorf("unsupported type found in ovsdb rows")
				}
			}
			return []*LogicalRouterPolicy{}, nil
		}
	}

	return nil, ErrorNotFound
}

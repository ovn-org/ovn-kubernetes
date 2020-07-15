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
	"strings"

	"github.com/ebay/libovsdb"
)

// LogicalSwitchPort ovnnb item
type LogicalSwitchPort struct {
	UUID             string
	Name             string
	Type             string
	Options          map[interface{}]interface{}
	Addresses        []string
	DynamicAddresses string
	PortSecurity     []string
	DHCPv4Options    string
	DHCPv6Options    string
	ExternalID       map[interface{}]interface{}
}

func (odbi *ovndb) lspAddImp(lsw, lsp string) (*OvnCommand, error) {
	namedUUID, err := newRowUUID()
	if err != nil {
		return nil, err
	}
	row := make(OVNRow)
	row["name"] = lsp

	if uuid := odbi.getRowUUID(tableLogicalSwitchPort, row); len(uuid) > 0 {
		return nil, ErrorExist
	}

	insertOp := libovsdb.Operation{
		Op:       opInsert,
		Table:    tableLogicalSwitchPort,
		Row:      row,
		UUIDName: namedUUID,
	}

	mutateUUID := []libovsdb.UUID{stringToGoUUID(namedUUID)}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}

	mutation := libovsdb.NewMutation("ports", opInsert, mutateSet)
	condition := libovsdb.NewCondition("name", "==", lsw)

	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalSwitch,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{condition},
	}
	operations := []libovsdb.Operation{insertOp, mutateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lspDelImp(lsp string) (*OvnCommand, error) {
	row := make(OVNRow)
	row["name"] = lsp

	lspUUID := odbi.getRowUUID(tableLogicalSwitchPort, row)
	if len(lspUUID) == 0 {
		return nil, ErrorNotFound
	}

	mutateUUID := []libovsdb.UUID{stringToGoUUID(lspUUID)}
	condition := libovsdb.NewCondition("name", "==", lsp)
	deleteOp := libovsdb.Operation{
		Op:    opDelete,
		Table: tableLogicalSwitchPort,
		Where: []interface{}{condition},
	}
	mutateSet, err := libovsdb.NewOvsSet(mutateUUID)
	if err != nil {
		return nil, err
	}
	mutation := libovsdb.NewMutation("ports", opDelete, mutateSet)
	ucondition, err := odbi.getRowUUIDContainsUUID(tableLogicalSwitch, "ports", lspUUID)
	if err != nil {
		return nil, err
	}

	mucondition := libovsdb.NewCondition("_uuid", "==", stringToGoUUID(ucondition))
	// simple mutate operation
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalSwitch,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{mucondition},
	}
	operations := []libovsdb.Operation{deleteOp, mutateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lspSetAddressImp(lsp string, addr ...string) (*OvnCommand, error) {
	row := make(OVNRow)
	addresses, err := libovsdb.NewOvsSet(addr)
	if err != nil {
		return nil, err
	}
	row["addresses"] = addresses
	condition := libovsdb.NewCondition("name", "==", lsp)
	updateOp := libovsdb.Operation{
		Op:    opUpdate,
		Table: tableLogicalSwitchPort,
		Row:   row,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{updateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lspSetPortSecurityImp(lsp string, security ...string) (*OvnCommand, error) {
	row := make(OVNRow)
	port_security, err := libovsdb.NewOvsSet(security)
	if err != nil {
		return nil, err
	}
	row["port_security"] = port_security
	condition := libovsdb.NewCondition("name", "==", lsp)
	updateOp := libovsdb.Operation{
		Op:    opUpdate,
		Table: tableLogicalSwitchPort,
		Row:   row,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{updateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lspSetDHCPv4OptionsImp(lsp string, uuid string) (*OvnCommand, error) {
	row := make(OVNRow)
	row["dhcpv4_options"] = stringToGoUUID(uuid)
	condition := libovsdb.NewCondition("name", "==", lsp)
	updateOp := libovsdb.Operation{
		Op:    opUpdate,
		Table: tableLogicalSwitchPort,
		Row:   row,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{updateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lspGetDHCPv4OptionsImp(lsp string) (*DHCPOptions, error) {
	lp, err := odbi.lspGetImp(lsp)
	if err != nil {
		return nil, err
	}
	return odbi.rowToDHCPOptions(lp.DHCPv4Options), nil
}

func (odbi *ovndb) lspSetDHCPv6OptionsImp(lsp string, options string) (*OvnCommand, error) {
	mutation := libovsdb.NewMutation("dhcpv6_options", opInsert, options)
	condition := libovsdb.NewCondition("name", "==", lsp)
	mutateOp := libovsdb.Operation{
		Op:        opMutate,
		Table:     tableLogicalSwitchPort,
		Mutations: []interface{}{mutation},
		Where:     []interface{}{condition},
	}
	operations := []libovsdb.Operation{mutateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lspGetDHCPv6OptionsImp(lsp string) (*DHCPOptions, error) {
	lp, err := odbi.lspGetImp(lsp)
	if err != nil {
		return nil, err
	}
	return odbi.rowToDHCPOptions(lp.DHCPv6Options), nil
}

func (odbi *ovndb) lspSetOptionsImp(lsp string, options map[string]string) (*OvnCommand, error) {
	if options == nil {
		return nil, ErrorOption
	}

	if len(lsp) == 0 {
		return nil, fmt.Errorf("LSP name cannot be empty while setting options")
	}

	optionsMap, err := libovsdb.NewOvsMap(options)
	if err != nil {
		return nil, err
	}

	row := make(OVNRow)
	row["options"] = optionsMap

	condition := libovsdb.NewCondition("name", "==", lsp)

	// simple mutate operation
	updateOp := libovsdb.Operation{
		Op:    opUpdate,
		Table: tableLogicalSwitchPort,
		Row:   row,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{updateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lspGetOptionsImp(lsp string) (map[string]string, error) {
	lp, err := odbi.lspGetImp(lsp)
	if err != nil {
		return nil, err
	}
	options := make(map[string]string)
	for k, v := range lp.Options {
		key, keyOk := k.(string)
		value, valueOk := v.(string)
		if !keyOk || !valueOk {
			continue
		}
		options[key] = value
	}
	return options, nil
}

func (odbi *ovndb) lspSetDynamicAddressesImp(lsp string, address string) (*OvnCommand, error) {
	if len(lsp) == 0 {
		return nil, fmt.Errorf("LSP name cannot be empty while setting dynamic addresses")
	}

	row := make(OVNRow)
	row["dynamic_addresses"] = address
	condition := libovsdb.NewCondition("name", "==", lsp)
	updateOp := libovsdb.Operation{
		Op:    opUpdate,
		Table: tableLogicalSwitchPort,
		Row:   row,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{updateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lspGetDynamicAddressesImp(lsp string) (string, error) {
	lp, err := odbi.lspGetImp(lsp)
	if err != nil {
		return "", err
	}
	return lp.DynamicAddresses, nil
}

func (odbi *ovndb) lspSetExternalIdsImp(lsp string, external_ids map[string]string) (*OvnCommand, error) {
	if external_ids == nil {
		return nil, ErrorOption
	}

	if len(lsp) == 0 {
		return nil, fmt.Errorf("LSP name cannot be empty while setting external_ids")
	}

	externalIdsMap, err := libovsdb.NewOvsMap(external_ids)
	if err != nil {
		return nil, err
	}

	row := make(OVNRow)
	row["external_ids"] = externalIdsMap

	condition := libovsdb.NewCondition("name", "==", lsp)

	// simple mutate operation
	updateOp := libovsdb.Operation{
		Op:    opUpdate,
		Table: tableLogicalSwitchPort,
		Row:   row,
		Where: []interface{}{condition},
	}
	operations := []libovsdb.Operation{updateOp}
	return &OvnCommand{operations, odbi, make([][]map[string]interface{}, len(operations))}, nil
}

func (odbi *ovndb) lspGetExternalIdsImp(lsp string) (map[string]string, error) {
	lp, err := odbi.lspGetImp(lsp)
	if err != nil {
		return nil, err
	}
	extIds := make(map[string]string)
	for k, v := range lp.ExternalID {
		key, keyOk := k.(string)
		value, valueOk := v.(string)
		if !keyOk || !valueOk {
			continue
		}
		extIds[key] = value
	}
	return extIds, nil
}

func (odbi *ovndb) rowToLogicalPort(uuid string) (*LogicalSwitchPort, error) {
	lp := &LogicalSwitchPort{
		UUID:       uuid,
		Name:       odbi.cache[tableLogicalSwitchPort][uuid].Fields["name"].(string),
		Type:       odbi.cache[tableLogicalSwitchPort][uuid].Fields["type"].(string),
		ExternalID: odbi.cache[tableLogicalSwitchPort][uuid].Fields["external_ids"].(libovsdb.OvsMap).GoMap,
	}

	if dhcpv4, ok := odbi.cache[tableLogicalSwitchPort][uuid].Fields["dhcpv4_options"]; ok {
		switch dhcpv4.(type) {
		case libovsdb.UUID:
			lp.DHCPv4Options = dhcpv4.(libovsdb.UUID).GoUUID
		case libovsdb.OvsSet:
		default:
		}
	}
	if dhcpv6, ok := odbi.cache[tableLogicalSwitchPort][uuid].Fields["dhcpv6_options"]; ok {
		switch dhcpv6.(type) {
		case libovsdb.UUID:
			lp.DHCPv6Options = dhcpv6.(libovsdb.UUID).GoUUID
		case libovsdb.OvsSet:
		default:
		}
	}

	if addr, ok := odbi.cache[tableLogicalSwitchPort][uuid].Fields["addresses"]; ok {
		switch addr.(type) {
		case string:
			lp.Addresses = []string{addr.(string)}
		case libovsdb.OvsSet:
			lp.Addresses = odbi.ConvertGoSetToStringArray(addr.(libovsdb.OvsSet))
		default:
			return nil, fmt.Errorf("Unsupported type found in lport address.")
		}
	}

	if portsecurity, ok := odbi.cache[tableLogicalSwitchPort][uuid].Fields["port_security"]; ok {
		switch portsecurity.(type) {
		case string:
			lp.PortSecurity = []string{portsecurity.(string)}
		case libovsdb.OvsSet:
			lp.PortSecurity = odbi.ConvertGoSetToStringArray(portsecurity.(libovsdb.OvsSet))
		default:
			return nil, fmt.Errorf("Unsupported type found in port security.")
		}
	}

	if options, ok := odbi.cache[tableLogicalSwitchPort][uuid].Fields["options"]; ok {
		lp.Options = options.(libovsdb.OvsMap).GoMap
	}

	if dynamicAddresses, ok := odbi.cache[tableLogicalSwitchPort][uuid].Fields["dynamic_addresses"]; ok {
		switch dynamicAddresses.(type) {
		case string:
			lp.DynamicAddresses = dynamicAddresses.(string)
		case libovsdb.OvsSet:
			lp.DynamicAddresses = strings.Join(odbi.ConvertGoSetToStringArray(dynamicAddresses.(libovsdb.OvsSet)), " ")
		default:
			return nil, fmt.Errorf("Unsupport type found in lport dynamic address.")
		}
	}

	return lp, nil
}

// Get lsp by name
func (odbi *ovndb) lspGetImp(lsp string) (*LogicalSwitchPort, error) {
	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalSwitchPort, ok := odbi.cache[tableLogicalSwitchPort]
	if !ok {
		return nil, ErrorSchema
	}

	for uuid, drows := range cacheLogicalSwitchPort {
		if rlsp, ok := drows.Fields["name"].(string); ok && rlsp == lsp {
			return odbi.rowToLogicalPort(uuid)
		}
	}
	return nil, ErrorNotFound
}

// Get all lport by lswitch
func (odbi *ovndb) lspListImp(lsw string) ([]*LogicalSwitchPort, error) {
	var listLSP []*LogicalSwitchPort

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheLogicalSwitch, ok := odbi.cache[tableLogicalSwitch]
	if !ok {
		return nil, ErrorSchema
	}
	var lsFound bool
	for _, drows := range cacheLogicalSwitch {
		if rlsw, ok := drows.Fields["name"].(string); ok && rlsw == lsw {
			ports := drows.Fields["ports"]
			if ports != nil {
				switch ports.(type) {
				case libovsdb.OvsSet:
					if ps, ok := ports.(libovsdb.OvsSet); ok {
						for _, p := range ps.GoSet {
							if vp, ok := p.(libovsdb.UUID); ok {
								tp, err := odbi.rowToLogicalPort(vp.GoUUID)
								if err != nil {
									return nil, fmt.Errorf("Failed to get logical port: %s", err)
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
							return nil, fmt.Errorf("Failed to get logical port: %s", err)
						}
						listLSP = append(listLSP, tp)
					} else {
						return nil, fmt.Errorf("type libovsdb.UUID casting failed")
					}
				default:
					return nil, fmt.Errorf("Unsupported type found in ovsdb rows")
				}
			}
			lsFound = true
			break
		}
	}
	if !lsFound {
		return nil, ErrorNotFound
	}
	return listLSP, nil
}

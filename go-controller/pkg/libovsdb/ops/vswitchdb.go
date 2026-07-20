// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// OVS bridge predicates for filtering.
type ovsBridgePredicate func(*vswitchd.Bridge) bool

// OVS port predicates for filtering
type ovsPortPredicate func(*vswitchd.Port) bool

// OVS interface predicates for filtering.
type ovsInterfacePredicate func(*vswitchd.Interface) bool

// GetOpenvSwitch returns the singleton Open_vSwitch row from the cache.
// When no row exists, the returned error wraps libovsdbclient.ErrNotFound so
// callers can detect that case via errors.Is. This is the libovsdb equivalent
// of `ovs-vsctl list Open_vSwitch`.
func GetOpenvSwitch(ovsClient libovsdbclient.Client) (*vswitchd.OpenvSwitch, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	openvSwitchList := []*vswitchd.OpenvSwitch{}
	err := ovsClient.List(ctx, &openvSwitchList)
	if err != nil {
		return nil, err
	}
	if len(openvSwitchList) == 0 {
		return nil, fmt.Errorf("no openvSwitch entry found: %w", libovsdbclient.ErrNotFound)
	}

	return openvSwitchList[0], nil
}

// TransactAndCheckAndWaitForVSwitchd transacts ops and waits for ovs-vswitchd
// to apply them, matching ovs-vsctl's default next_cfg/cur_cfg synchronization.
// After the transaction completes, this can block for up to types.OVSDBTimeout
// waiting for ovs-vswitchd, in addition to the transaction's own timeout.
// It does not accept caller cancellation. Use it only when subsequent work
// requires the configuration to be applied; otherwise use TransactAndCheck,
// especially in latency-sensitive paths or while holding locks.
func TransactAndCheckAndWaitForVSwitchd(ovsClient libovsdbclient.Client, ops []ovsdb.Operation) error {
	if len(ops) == 0 {
		return nil
	}
	ovs, err := GetOpenvSwitch(ovsClient)
	if err != nil {
		return err
	}
	where := []ovsdb.Condition{ovsdb.NewCondition("_uuid", ovsdb.ConditionEqual, ovsdb.UUID{GoUUID: ovs.UUID})}
	ops = append(ops,
		ovsdb.Operation{Op: ovsdb.OperationMutate, Table: vswitchd.OpenvSwitchTable, Where: where,
			Mutations: []ovsdb.Mutation{*ovsdb.NewMutation("next_cfg", ovsdb.MutateOperationAdd, 1)}},
		ovsdb.Operation{Op: ovsdb.OperationSelect, Table: vswitchd.OpenvSwitchTable, Where: where, Columns: []string{"next_cfg"}},
	)
	results, err := TransactAndCheck(ovsClient, ops)
	if err != nil {
		return err
	}
	rows := results[len(results)-1].Rows
	if len(rows) != 1 {
		return fmt.Errorf("expected one Open_vSwitch row, got %d", len(rows))
	}
	value := rows[0]["next_cfg"]
	var nextCfg int
	switch value := value.(type) {
	case int:
		nextCfg = value
	case float64:
		nextCfg = int(value)
	default:
		return fmt.Errorf("unexpected next_cfg value %T(%v)", value, value)
	}
	err = wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, types.OVSDBTimeout, true,
		func(ctx context.Context) (bool, error) {
			ovs := &vswitchd.OpenvSwitch{UUID: ovs.UUID}
			if err := ovsClient.Get(ctx, ovs); err != nil {
				return false, err
			}
			return ovs.CurCfg >= nextCfg, nil
		})
	if err != nil {
		return fmt.Errorf("waiting for ovs-vswitchd to apply configuration %d: %w", nextCfg, err)
	}
	return nil
}

// UpdateOpenvSwitchExternalIDs merges the given map into the Open_vSwitch
// row's external_ids. Every entry in the map is inserted or overwritten;
// existing keys that are not in the map are left alone. Returns ErrNotFound
// if the row does not exist. This is the libovsdb equivalent of
// `ovs-vsctl set Open_vSwitch . external_ids:key=value ...`.
func UpdateOpenvSwitchExternalIDs(ovsClient libovsdbclient.Client, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	ovs := &vswitchd.OpenvSwitch{ExternalIDs: kv}
	opModel := operationModel{
		Model:            ovs,
		ModelPredicate:   func(*vswitchd.OpenvSwitch) bool { return true },
		OnModelMutations: []interface{}{&ovs.ExternalIDs},
		ErrNotFound:      true,
		BulkOp:           false,
	}
	m := newModelClient(ovsClient)
	_, err := m.CreateOrUpdate(opModel)
	return err
}

// RemoveOpenvSwitchExternalIDs removes the given keys from the Open_vSwitch
// row's external_ids. Keys that are not present, and a missing row, are both
// no-ops. This is the libovsdb equivalent of
// `ovs-vsctl --if-exists remove Open_vSwitch . external_ids <key> ...`.
func RemoveOpenvSwitchExternalIDs(ovsClient libovsdbclient.Client, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	// modelClient interprets a map field with empty string values as a
	// delete-by-key mutation (see buildMutationsFromFields).
	ids := make(map[string]string, len(keys))
	for _, k := range keys {
		ids[k] = ""
	}
	ovs := &vswitchd.OpenvSwitch{ExternalIDs: ids}
	opModel := operationModel{
		Model:            ovs,
		ModelPredicate:   func(*vswitchd.OpenvSwitch) bool { return true },
		OnModelMutations: []interface{}{&ovs.ExternalIDs},
		ErrNotFound:      false,
		BulkOp:           false,
	}
	m := newModelClient(ovsClient)
	return m.Delete(opModel)
}

// FindOVSPortsWithPredicate returns all OVS ports matching the predicate. This
// is the libovsdb equivalent of `ovs-vsctl find Port <conditions>`.
func FindOVSPortsWithPredicate(ovsClient libovsdbclient.Client, p ovsPortPredicate) ([]*vswitchd.Port, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	var ports []*vswitchd.Port
	err := ovsClient.WhereCache(p).List(ctx, &ports)
	return ports, err
}

// ListBridges looks up all OVS bridges from the cache. This is the libovsdb
// equivalent of `ovs-vsctl list-br`.
func ListBridges(ovsClient libovsdbclient.Client) ([]*vswitchd.Bridge, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedBridges := []*vswitchd.Bridge{}
	err := ovsClient.List(ctx, &searchedBridges)
	return searchedBridges, err
}

// FindBridgesWithPredicate returns all OVS bridges in the cache that match the
// predicate. This is the libovsdb equivalent of `ovs-vsctl find Bridge
// <conditions>`.
func FindBridgesWithPredicate(ovsClient libovsdbclient.Client, p ovsBridgePredicate) ([]*vswitchd.Bridge, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedBridges := []*vswitchd.Bridge{}

	err := ovsClient.WhereCache(p).List(ctx, &searchedBridges)
	return searchedBridges, err
}

// GetPortBridge returns the OVS bridge that owns the named port.
// Returns ErrNotFound if the port does not exist, no bridge references it
// in its ports column, or the port is the bridge's local port. This is the
// libovsdb equivalent of `ovs-vsctl port-to-br <port>`.
//
// Like ovs-vsctl port-to-br / list-ports, a bridge's local port (the Port
// created by add-br with the same name as the bridge) is not reported.
func GetPortBridge(ovsClient libovsdbclient.Client, portName string) (*vswitchd.Bridge, error) {
	port, err := GetOVSPort(ovsClient, portName)
	if err != nil {
		return nil, err
	}
	bridges, err := FindBridgesWithPredicate(ovsClient, func(bridge *vswitchd.Bridge) bool {
		for _, uuid := range bridge.Ports {
			if uuid == port.UUID {
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	if len(bridges) > 1 {
		return nil, fmt.Errorf("OVSDB corruption: port %q is referenced by multiple bridges: %w", portName, errMultipleResults)
	}
	if len(bridges) == 1 {
		// ovs-vsctl port-to-br does not resolve a bridge's own local port.
		if bridges[0].Name == portName {
			return nil, fmt.Errorf("port %q is the local port of bridge %q: %w",
				portName, bridges[0].Name, libovsdbclient.ErrNotFound)
		}
		return bridges[0], nil
	}
	return nil, fmt.Errorf("no bridge contains port %q: %w", portName, libovsdbclient.ErrNotFound)
}

// FindPortsForPortOrInterface returns the Ports that carry name: the Port
// named name, and any Port holding an Interface of that name under another
// Port name (e.g. a bond member). Unlike GetPortBridge, it makes no
// port-to-br judgment: callers decide what bridge membership of the returned
// ports means.
func FindPortsForPortOrInterface(ovsClient libovsdbclient.Client, name string) ([]*vswitchd.Port, error) {
	interfaces, err := FindInterfacesWithPredicate(ovsClient, func(iface *vswitchd.Interface) bool {
		return iface.Name == name
	})
	if err != nil {
		return nil, fmt.Errorf("failed to look up OVS interface %s: %w", name, err)
	}
	interfaceIDs := make(map[string]struct{}, len(interfaces))
	for _, iface := range interfaces {
		interfaceIDs[iface.UUID] = struct{}{}
	}
	ports, err := FindOVSPortsWithPredicate(ovsClient, func(port *vswitchd.Port) bool {
		if port.Name == name {
			return true
		}
		for _, interfaceID := range port.Interfaces {
			if _, ok := interfaceIDs[interfaceID]; ok {
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("failed to look up OVS port for %s: %w", name, err)
	}
	return ports, nil
}

// getInterfacePort returns the OVS port that owns the named interface.
// Returns ErrNotFound if the interface does not exist or no port references it.
func getInterfacePort(ovsClient libovsdbclient.Client, interfaceName string) (*vswitchd.Port, error) {
	iface, err := GetOVSInterface(ovsClient, interfaceName)
	if err != nil {
		return nil, err
	}
	ports, err := FindOVSPortsWithPredicate(ovsClient, func(port *vswitchd.Port) bool {
		for _, uuid := range port.Interfaces {
			if uuid == iface.UUID {
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	if len(ports) > 1 {
		return nil, fmt.Errorf("OVSDB corruption: interface %q is referenced by multiple ports: %w", interfaceName, errMultipleResults)
	}
	if len(ports) == 1 {
		return ports[0], nil
	}
	return nil, fmt.Errorf("no port contains interface %q: %w", interfaceName, libovsdbclient.ErrNotFound)
}

// CreateOrUpdateNicBridge creates (or reconfigures) an OVS bridge for an
// uplink NIC. It sets fail-mode=standalone, the supplied hardware address,
// and external_ids bridge-id/bridge-uplink on the bridge; attaches the
// uplink port with other_config:transient=true; and references the bridge
// from the Open_vSwitch root row.
//
// Like `ovs-vsctl add-br`, this also creates the bridge's same-named
// internal Port/Interface (type=internal); without it, ovs-vswitchd never
// materialises the kernel netdev and the bridge's `mac_in_use` stays empty.
//
// external_ids/other_config are merged key-by-key, so any pre-existing
// metadata on the bridge or uplink port is preserved. If the uplink port
// (or the bridge's same-named internal port) already exists on a different
// bridge the call fails, matching `ovs-vsctl add-port`'s cross-bridge
// behaviour.
//
// This is the libovsdb equivalent of:
//
//	ovs-vsctl -- --may-exist add-br <bridge>
//	          -- br-set-external-id <bridge> bridge-id <bridge>
//	          -- br-set-external-id <bridge> bridge-uplink <uplink>
//	          -- set bridge <bridge> fail-mode=standalone other_config:hwaddr=<hwaddr>
//	          -- --may-exist add-port <bridge> <uplink>
//	          -- set port <uplink> other-config:transient=true
func CreateOrUpdateNicBridge(ovsClient libovsdbclient.Client, bridgeName, uplinkName, hwaddr string) error {
	// Refuse to silently re-parent any port that's already attached to a
	// different bridge — ovs-vsctl add-port errors out in that case. This
	// applies to both the uplink port and the bridge's same-named internal
	// port.
	for _, portName := range []string{bridgeName, uplinkName} {
		if portName == "" {
			continue
		}
		existing, err := GetPortBridge(ovsClient, portName)
		if err != nil {
			if !errors.Is(err, libovsdbclient.ErrNotFound) {
				return fmt.Errorf("failed to check existing bridge for port %q: %w", portName, err)
			}
		} else if existing.Name != bridgeName {
			return fmt.Errorf("port %q is already attached to bridge %q", portName, existing.Name)
		}

		owner, err := getInterfacePort(ovsClient, portName)
		if err != nil {
			if errors.Is(err, libovsdbclient.ErrNotFound) {
				continue
			}
			return fmt.Errorf("failed to check existing port for interface %q: %w", portName, err)
		}
		if owner.Name != portName {
			return fmt.Errorf("interface %q is already attached to port %q", portName, owner.Name)
		}
	}

	bridge := &vswitchd.Bridge{
		Name:     bridgeName,
		FailMode: ptr.To(vswitchd.BridgeFailModeStandalone),
		ExternalIDs: map[string]string{
			"bridge-id":     bridgeName,
			"bridge-uplink": uplinkName,
		},
		OtherConfig: map[string]string{
			"hwaddr": hwaddr,
		},
	}
	// Internal bridge port/interface (named after the bridge).
	internalIface := &vswitchd.Interface{Name: bridgeName, Type: "internal"}
	internalPort := &vswitchd.Port{Name: bridgeName}
	// Uplink port/interface.
	uplinkIface := &vswitchd.Interface{Name: uplinkName}
	uplinkPort := &vswitchd.Port{
		Name:        uplinkName,
		OtherConfig: map[string]string{"transient": "true"},
	}

	internalIfaceModel := operationModel{
		Model:          internalIface,
		OnModelUpdates: []interface{}{&internalIface.Type},
		DoAfter: func() {
			internalPort.Interfaces = []string{internalIface.UUID}
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	internalPortModel := operationModel{
		Model:          internalPort,
		OnModelUpdates: []interface{}{&internalPort.Interfaces},
		DoAfter: func() {
			bridge.Ports = append(bridge.Ports, internalPort.UUID)
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	uplinkIfaceModel := operationModel{
		Model: uplinkIface,
		DoAfter: func() {
			uplinkPort.Interfaces = []string{uplinkIface.UUID}
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	uplinkPortModel := operationModel{
		Model:            uplinkPort,
		OnModelUpdates:   []interface{}{&uplinkPort.Interfaces},
		OnModelMutations: []interface{}{&uplinkPort.OtherConfig},
		DoAfter: func() {
			bridge.Ports = append(bridge.Ports, uplinkPort.UUID)
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	ovs := &vswitchd.OpenvSwitch{}
	bridgeModel := operationModel{
		Model:            bridge,
		OnModelUpdates:   []interface{}{&bridge.FailMode},
		OnModelMutations: []interface{}{&bridge.ExternalIDs, &bridge.OtherConfig, &bridge.Ports},
		DoAfter: func() {
			ovs.Bridges = []string{bridge.UUID}
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	ovsModel := operationModel{
		Model:            ovs,
		ModelPredicate:   func(*vswitchd.OpenvSwitch) bool { return true },
		OnModelMutations: []interface{}{&ovs.Bridges},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(ovsClient)
	ops, err := m.CreateOrUpdateOps(nil,
		internalIfaceModel, internalPortModel,
		uplinkIfaceModel, uplinkPortModel,
		bridgeModel, ovsModel)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(ovsClient, ops)
	return err
}

// GetBridge looks up an OVS bridge by name. This is the libovsdb equivalent of
// `ovs-vsctl br-exists <name>` plus `ovs-vsctl list Bridge <name>`.
func GetBridge(ovsClient libovsdbclient.Client, name string) (*vswitchd.Bridge, error) {
	found := []*vswitchd.Bridge{}
	opModel := operationModel{
		Model:          &vswitchd.Bridge{Name: name},
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(ovsClient)
	if err := m.Lookup(opModel); err != nil {
		return nil, err
	}

	return found[0], nil
}

// DeleteBridge deletes an OVS bridge and detaches it from the Open_vSwitch root
// row. OVSDB garbage collection removes Port and Interface rows that become
// unreachable through the bridge's strong references. It is idempotent: a
// missing bridge is not an error. This is the libovsdb equivalent of
// `ovs-vsctl --if-exists del-br <name>`.
func DeleteBridge(ovsClient libovsdbclient.Client, bridgeName string) error {
	ops, err := DeleteBridgeOps(ovsClient, nil, bridgeName)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return nil
	}
	_, err = TransactAndCheck(ovsClient, ops)
	return err
}

// DeleteBridgeOps returns operations to delete an OVS bridge and detach it from
// the Open_vSwitch root row's bridges set. A missing bridge yields zero
// operations.
func DeleteBridgeOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, bridgeName string) ([]ovsdb.Operation, error) {
	bridge, err := GetBridge(ovsClient, bridgeName)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return ops, nil
		}
		return nil, err
	}

	// Flow_Sample_Collector_Set is a root table with a strong reference to
	// Bridge. ovs-vsctl del-br removes these rows before deleting the bridge;
	// otherwise OVSDB rejects the transaction for referential-integrity.
	collector := &vswitchd.FlowSampleCollectorSet{}
	collectorOps, err := ovsClient.WhereAll(collector, model.Condition{
		Field:    &collector.Bridge,
		Function: ovsdb.ConditionEqual,
		Value:    bridge.UUID,
	}).Delete()
	if err != nil {
		return nil, fmt.Errorf("failed to build flow sample collector delete operations for bridge %q: %w", bridgeName, err)
	}
	ops = append(ops, collectorOps...)

	m := newModelClient(ovsClient)
	ovs := &vswitchd.OpenvSwitch{Bridges: []string{bridge.UUID}}
	return m.DeleteOps(ops,
		operationModel{
			Model:            ovs,
			ModelPredicate:   func(*vswitchd.OpenvSwitch) bool { return true },
			OnModelMutations: []interface{}{&ovs.Bridges},
			ErrNotFound:      false,
			BulkOp:           false,
		},
		operationModel{
			Model:       bridge,
			ErrNotFound: false,
			BulkOp:      false,
		},
	)
}

// GetOVSInterface looks up an OVS interface by name. Returns ErrNotFound if no
// interface with that name exists. This is the libovsdb equivalent of
// `ovs-vsctl find Interface name=<name>`.
func GetOVSInterface(ovsClient libovsdbclient.Client, name string) (*vswitchd.Interface, error) {
	found := []*vswitchd.Interface{}
	opModel := operationModel{
		Model:          &vswitchd.Interface{Name: name},
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(ovsClient)
	if err := m.Lookup(opModel); err != nil {
		return nil, err
	}

	return found[0], nil
}

// RemoveOVSInterfaceExternalIDs removes the given keys from an Interface's
// external_ids. Missing keys are no-ops.
func RemoveOVSInterfaceExternalIDs(ovsClient libovsdbclient.Client, name string, keys ...string) error {
	ops, err := RemoveOVSInterfaceExternalIDsOps(ovsClient, nil, name, keys...)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(ovsClient, ops)
	return err
}

// RemoveOVSInterfaceExternalIDsOps returns operations that remove the given
// keys from an Interface's external_ids. Missing keys are no-ops.
func RemoveOVSInterfaceExternalIDsOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, name string, keys ...string) ([]ovsdb.Operation, error) {
	if len(keys) == 0 {
		return ops, nil
	}
	ids := make(map[string]string, len(keys))
	for _, key := range keys {
		ids[key] = ""
	}
	iface := &vswitchd.Interface{Name: name, ExternalIDs: ids}
	opModel := operationModel{
		Model:            iface,
		OnModelMutations: []interface{}{&iface.ExternalIDs},
		ErrNotFound:      false,
		BulkOp:           false,
	}
	m := newModelClient(ovsClient)
	return m.DeleteOps(ops, opModel)
}

// ListInterfaces looks up all OVS interfaces from the cache. This is the
// libovsdb equivalent of `ovs-vsctl list Interface`.
func ListInterfaces(ovsClient libovsdbclient.Client) ([]*vswitchd.Interface, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedInterfaces := []*vswitchd.Interface{}
	err := ovsClient.List(ctx, &searchedInterfaces)
	return searchedInterfaces, err
}

// FindInterfacesWithPredicate returns all OVS interfaces in the cache that
// match the predicate. This is the libovsdb equivalent of
// `ovs-vsctl find Interface <conditions>`.
func FindInterfacesWithPredicate(ovsClient libovsdbclient.Client, p ovsInterfacePredicate) ([]*vswitchd.Interface, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedInterfaces := []*vswitchd.Interface{}

	err := ovsClient.WhereCache(p).List(ctx, &searchedInterfaces)
	return searchedInterfaces, err
}

// GetOVSPort looks up an OVS port by name. Returns ErrNotFound if no port with
// that name exists. This is the libovsdb equivalent of
// `ovs-vsctl find Port name=<name>`.
func GetOVSPort(ovsClient libovsdbclient.Client, name string) (*vswitchd.Port, error) {
	found := []*vswitchd.Port{}
	opModel := operationModel{
		Model:          &vswitchd.Port{Name: name},
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(ovsClient)
	if err := m.Lookup(opModel); err != nil {
		return nil, err
	}

	return found[0], nil
}

// CreateOrUpdatePortWithInterface creates or updates an OVS port and its interface on a bridge.
// This creates both the Port and Interface objects atomically in a single transaction,
// and attaches the port to the specified bridge.
// The interface type is set to "internal". This is the libovsdb equivalent of:
//
//	ovs-vsctl --may-exist add-port <bridge> <port>
//	          -- set Interface <port> type=internal external_ids:...
func CreateOrUpdatePortWithInterface(ovsClient libovsdbclient.Client, bridgeName, portName string, portExternalIDs, ifaceExternalIDs map[string]string) error {
	ops, err := CreateOrUpdatePortWithInterfaceOps(ovsClient, nil, bridgeName, portName, portExternalIDs, ifaceExternalIDs)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(ovsClient, ops)
	return err
}

// CreateOrUpdatePortWithInterfaceOps returns operations to create or update an OVS port and its interface.
// OVS uses the following hierarchy: Bridge/Port/Interface. A port is referenced by a bridge,
// and an interface is referenced by a port.
func CreateOrUpdatePortWithInterfaceOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, bridgeName, portName string, portExternalIDs, ifaceExternalIDs map[string]string) ([]ovsdb.Operation, error) {
	iface := &vswitchd.Interface{Name: portName, Type: "internal", ExternalIDs: ifaceExternalIDs}
	port := &vswitchd.Port{Name: portName, ExternalIDs: portExternalIDs}
	bridge := &vswitchd.Bridge{Name: bridgeName}

	// Interface model - DoAfter captures UUID for port reference
	ifaceModel := operationModel{
		Model:          iface,
		OnModelUpdates: []interface{}{&iface.Type, &iface.ExternalIDs},
		DoAfter: func() {
			port.Interfaces = []string{iface.UUID}
		},
		ErrNotFound: false,
		BulkOp:      false,
	}

	// Port model - DoAfter captures UUID for bridge reference
	portModel := operationModel{
		Model:          port,
		OnModelUpdates: []interface{}{&port.ExternalIDs},
		DoAfter: func() {
			bridge.Ports = append(bridge.Ports, port.UUID)
		},
		ErrNotFound: false,
		BulkOp:      false,
	}

	// Bridge model - mutates Ports to add our port
	bridgeModel := operationModel{
		Model:            bridge,
		OnModelMutations: []interface{}{&bridge.Ports},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(ovsClient)
	return m.CreateOrUpdateOps(ops, ifaceModel, portModel, bridgeModel)
}

// CreateOrUpdatePodPort creates or updates an OVS port and its single backing
// interface on bridgeName in one transaction. The helper updates these
// columns from the caller-supplied models:
//   - Port: OtherConfig, ExternalIDs
//   - Interface: Type, Options, MTURequest, ExternalIDs
//
// Both port.Name and iface.Name are forced to portName for consistency.
// On create, the model is written as-is; on update, only the listed columns
// are touched. This is the libovsdb equivalent of an `ovs-vsctl --may-exist
// add-port BRIDGE PORT -- set Interface PORT key=value ...` chain.
func CreateOrUpdatePodPort(ovsClient libovsdbclient.Client, bridgeName, portName string, port *vswitchd.Port, iface *vswitchd.Interface) error {
	ops, err := CreateOrUpdatePodPortOps(ovsClient, nil, bridgeName, portName, port, iface)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(ovsClient, ops)
	return err
}

// CreateOrUpdatePodPortOps returns the operations to create or update a pod's
// OVS port and interface for chaining into a larger transaction. See
// CreateOrUpdatePodPort for semantics.
func CreateOrUpdatePodPortOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, bridgeName, portName string, port *vswitchd.Port, iface *vswitchd.Interface) ([]ovsdb.Operation, error) {
	if port == nil {
		return nil, fmt.Errorf("CreateOrUpdatePodPortOps: nil port")
	}
	if iface == nil {
		return nil, fmt.Errorf("CreateOrUpdatePodPortOps: nil iface")
	}
	// Reject ports that already live on a different bridge. OVS's schema
	// does not enforce one-bridge-per-port (Bridge.ports is a strong-set
	// with no Port back-reference), so without this check the mutation
	// below would leave the same Port UUID in two Bridges' ports lists.
	// This matches the user-space safety check that `ovs-vsctl
	// --may-exist add-port BRIDGE PORT` performs.
	existing, err := GetPortBridge(ovsClient, portName)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil, err
	}
	if existing != nil && existing.Name != bridgeName {
		return nil, fmt.Errorf("port %q is already attached to bridge %q", portName, existing.Name)
	}
	existingIface, err := GetOVSInterface(ovsClient, portName)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil, err
	}
	if existingIface != nil {
		existingPort, err := GetOVSPort(ovsClient, portName)
		if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil, err
		}
		// Names are indexed. Avoid scanning every Port on a repeated CNI ADD
		// when the named port already owns the interface. Resolve the owner
		// only on a possible conflict, to include its name in the error.
		if existingPort == nil || !slices.Contains(existingPort.Interfaces, existingIface.UUID) {
			owner, err := getInterfacePort(ovsClient, portName)
			if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
				return nil, err
			}
			if owner != nil && owner.Name != portName {
				return nil, fmt.Errorf("interface %q is already attached to port %q", portName, owner.Name)
			}
		}
	}

	iface.Name = portName
	port.Name = portName
	bridge := &vswitchd.Bridge{Name: bridgeName}

	ifaceModel := operationModel{
		Model:            iface,
		OnModelUpdates:   []interface{}{&iface.Type, &iface.Options, &iface.MTURequest},
		OnModelMutations: []interface{}{&iface.ExternalIDs},
		DoAfter: func() {
			port.Interfaces = []string{iface.UUID}
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	portModel := operationModel{
		Model:            port,
		OnModelUpdates:   []interface{}{&port.Interfaces},
		OnModelMutations: []interface{}{&port.OtherConfig, &port.ExternalIDs},
		DoAfter: func() {
			bridge.Ports = append(bridge.Ports, port.UUID)
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	bridgeModel := operationModel{
		Model:            bridge,
		OnModelMutations: []interface{}{&bridge.Ports},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(ovsClient)
	return m.CreateOrUpdateOps(ops, ifaceModel, portModel, bridgeModel)
}

// DeletePortWithInterfaces deletes a named OVS port, or the port containing a
// named interface, and all of that port's interfaces from a bridge. Like
// `ovs-vsctl --if-exists --with-iface del-port`, a missing target is a no-op.
// The deletion is safely scoped to bridgeName: a target attached to another
// bridge is logged and left untouched.
func DeletePortWithInterfaces(ovsClient libovsdbclient.Client, bridgeName, portOrInterfaceName string) error {
	port, err := GetOVSPort(ovsClient, portOrInterfaceName)
	if err != nil {
		if !errors.Is(err, libovsdbclient.ErrNotFound) {
			return err
		}
		// Match ovs-vsctl del-port --with-iface: if the target is not a
		// Port name, resolve it as an Interface name and delete its Port.
		port, err = getInterfacePort(ovsClient, portOrInterfaceName)
		if err != nil {
			if errors.Is(err, libovsdbclient.ErrNotFound) {
				return nil // Neither port nor interface exists
			}
			return err
		}
	}
	if port.Name == bridgeName {
		return nil // The bridge's local port can only be removed with the bridge
	}
	bridge, err := GetBridge(ovsClient, bridgeName)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil // Bridge gone; nothing to detach from
		}
		return err
	}
	onBridge := false
	for _, uuid := range bridge.Ports {
		if uuid == port.UUID {
			onBridge = true
			break
		}
	}
	if !onBridge {
		klog.Warningf("OVS port %q exists but is not attached to bridge %q; leaving it untouched", port.Name, bridgeName)
		return nil // Port lives on a different bridge; out of scope
	}
	ops, err := DeletePortWithInterfacesOps(ovsClient, nil, port, bridgeName)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return nil // Port doesn't exist
	}
	_, err = TransactAndCheck(ovsClient, ops)
	return err
}

// DeleteMultiplePortsWithInterfaces deletes multiple OVS ports and their
// interfaces from bridgeName in a single transaction. Only ports that exist and
// are attached to that bridge are removed; anything else is skipped, so the
// call is idempotent per port name.
func DeleteMultiplePortsWithInterfaces(ovsClient libovsdbclient.Client, bridgeName string, portNames ...string) error {
	if len(portNames) == 0 {
		return nil
	}

	bridge, err := GetBridge(ovsClient, bridgeName)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("failed to get bridge %s: %w", bridgeName, err)
	}

	onBridge := make(map[string]struct{}, len(bridge.Ports))
	for _, portUUID := range bridge.Ports {
		onBridge[portUUID] = struct{}{}
	}

	ports := make(map[string]struct{}, len(portNames))
	var ops []ovsdb.Operation
	for _, name := range portNames {
		if _, dup := ports[name]; dup {
			continue
		}
		ports[name] = struct{}{}

		port, err := GetOVSPort(ovsClient, name)
		if err != nil {
			if errors.Is(err, libovsdbclient.ErrNotFound) {
				continue
			}
			return fmt.Errorf("failed to get port %s: %w", name, err)
		}
		if _, ok := onBridge[port.UUID]; !ok {
			klog.Warningf("OVS port %q exists but is not attached to bridge %q; leaving it untouched", name, bridgeName)
			continue
		}
		ops, err = DeletePortWithInterfacesOps(ovsClient, ops, port, bridgeName)
		if err != nil {
			return fmt.Errorf("failed to build delete ops for port %s: %w", name, err)
		}
	}

	if len(ops) == 0 {
		return nil
	}

	if _, err := TransactAndCheck(ovsClient, ops); err != nil {
		return fmt.Errorf("failed to delete ports from bridge %s: %w", bridgeName, err)
	}
	return nil
}

// DeletePortWithInterfacesOps returns operations to delete an OVS port and all its interfaces.
// This handles both the Port and Interface objects, and detaches the port from the bridge.
func DeletePortWithInterfacesOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, port *vswitchd.Port, bridgeName string) ([]ovsdb.Operation, error) {
	bridge := &vswitchd.Bridge{Name: bridgeName}

	m := newModelClient(ovsClient)
	if len(port.Interfaces) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
		defer cancel()
		interfaces := []*vswitchd.Interface{}
		err := ovsClient.WhereCacheByUUIDs(func(*vswitchd.Interface) bool { return true }, port.Interfaces...).List(ctx, &interfaces)
		if err != nil {
			return nil, fmt.Errorf("failed to find interfaces for port %q: %w", port.Name, err)
		}
		for _, iface := range interfaces {
			ifaceOps, err := m.delete(iface)
			if err != nil {
				return nil, fmt.Errorf("failed to build delete interface ops: %w", err)
			}
			ops = append(ops, ifaceOps...)
		}
	}

	// Delete port and remove from bridge
	bridge.Ports = []string{port.UUID}
	portModel := operationModel{
		Model:       port,
		ErrNotFound: false,
		BulkOp:      false,
	}
	bridgeModel := operationModel{
		Model:            bridge,
		OnModelMutations: []interface{}{&bridge.Ports},
		ErrNotFound:      false,
		BulkOp:           false,
	}

	return m.DeleteOps(ops, portModel, bridgeModel)
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"errors"
	"fmt"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// LOGICAL_SWITCH OPs

type switchPredicate func(*nbdb.LogicalSwitch) bool
type switchPortPredicate func(port *nbdb.LogicalSwitchPort) bool

// FindLogicalSwitchPortWithPredicate looks up logical switches ports from the cache
// based on a given predicate
func FindLogicalSwitchPortWithPredicate(nbClient libovsdbclient.Client, p switchPortPredicate) ([]*nbdb.LogicalSwitchPort, error) {
	found := []*nbdb.LogicalSwitchPort{}
	ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereCache(p).List(ctx, &found)
	return found, err
}

// FindLogicalSwitchPortsByNamespaceAndNetwork looks up ports using their namespace
// and network external IDs. The NB client indexes this pair to avoid a table scan.
func FindLogicalSwitchPortsByNamespaceAndNetwork(nbClient libovsdbclient.Client, namespace, network string) ([]*nbdb.LogicalSwitchPort, error) {
	port := &nbdb.LogicalSwitchPort{ExternalIDs: map[string]string{
		"namespace":                namespace,
		ovntypes.NetworkExternalID: network,
	}}
	found := []*nbdb.LogicalSwitchPort{}
	ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereAll(port, model.Condition{
		Field: &port.ExternalIDs, Function: ovsdb.ConditionIncludes, Value: port.ExternalIDs,
	}).List(ctx, &found)
	return found, err
}

// FindLogicalSwitchesWithPredicate looks up logical switches from the cache
// based on a given predicate
func FindLogicalSwitchesWithPredicate(nbClient libovsdbclient.Client, p switchPredicate) ([]*nbdb.LogicalSwitch, error) {
	found := []*nbdb.LogicalSwitch{}
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	err := nbClient.WhereCache(p).List(ctx, &found)
	return found, err
}

// GetLogicalSwitch looks up a logical switch from the cache
func GetLogicalSwitch(nbClient libovsdbclient.Client, sw *nbdb.LogicalSwitch) (*nbdb.LogicalSwitch, error) {
	found := []*nbdb.LogicalSwitch{}
	opModel := operationModel{
		Model:          sw,
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	err := m.Lookup(opModel)
	if err != nil {
		return nil, err
	}

	return found[0], nil
}

// CreateOrUpdateLogicalSwitch creates or updates the provided logical switch
func CreateOrUpdateLogicalSwitch(nbClient libovsdbclient.Client, sw *nbdb.LogicalSwitch, fields ...interface{}) error {
	if len(fields) == 0 {
		fields = onModelUpdatesAllNonDefault()
	}
	opModel := operationModel{
		Model:          sw,
		OnModelUpdates: fields,
		ErrNotFound:    false,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModel)
	return err
}

// UpdateLogicalSwitchSetExternalIDs updates the external IDs on the provided logical
// switch. Empty values means the corresponding keys are to be deleted.
func UpdateLogicalSwitchSetExternalIDs(nbClient libovsdbclient.Client, logicalSwitch *nbdb.LogicalSwitch) error {
	externalIds := logicalSwitch.ExternalIDs
	logicalSwitch, err := GetLogicalSwitch(nbClient, logicalSwitch)
	if err != nil {
		return err
	}

	if logicalSwitch.ExternalIDs == nil {
		logicalSwitch.ExternalIDs = map[string]string{}
	}

	for k, v := range externalIds {
		if v == "" {
			delete(logicalSwitch.ExternalIDs, k)
		} else {
			logicalSwitch.ExternalIDs[k] = v
		}
	}

	opModel := operationModel{
		Model:          logicalSwitch,
		OnModelUpdates: []interface{}{&logicalSwitch.ExternalIDs},
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	_, err = m.CreateOrUpdate(opModel)
	return err
}

type logicalSwitchPredicate func(*nbdb.LogicalSwitch) bool

// DeleteLogicalSwitchesWithPredicateOps returns the operations to delete the logical switches matching the provided predicate
func DeleteLogicalSwitchesWithPredicateOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation,
	p logicalSwitchPredicate) ([]ovsdb.Operation, error) {
	opModel := operationModel{
		Model:          &nbdb.LogicalSwitch{},
		ModelPredicate: p,
		ErrNotFound:    false,
		BulkOp:         true,
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModel)
}

// DeleteLogicalSwitchOps returns the operations to delete the provided logical switch
func DeleteLogicalSwitchOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation,
	swName string) ([]ovsdb.Operation, error) {
	opModel := operationModel{
		Model:       &nbdb.LogicalSwitch{Name: swName},
		ErrNotFound: false,
		BulkOp:      false,
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModel)
}

// DeleteLogicalSwitch deletes the provided logical switch
func DeleteLogicalSwitch(nbClient libovsdbclient.Client, swName string) error {
	ops, err := DeleteLogicalSwitchOps(nbClient, nil, swName)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// LB ops

// AddLoadBalancersToLogicalSwitchOps adds the provided load balancers to the
// provided logical switch and returns the corresponding ops
func AddLoadBalancersToLogicalSwitchOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, sw *nbdb.LogicalSwitch, lbs ...*nbdb.LoadBalancer) ([]ovsdb.Operation, error) {
	sw.LoadBalancer = make([]string, 0, len(lbs))
	for _, lb := range lbs {
		sw.LoadBalancer = append(sw.LoadBalancer, lb.UUID)
	}
	opModel := operationModel{
		Model:            sw,
		OnModelMutations: []interface{}{&sw.LoadBalancer},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModel)
}

// RemoveLoadBalancersFromLogicalSwitchOps removes the provided load balancers from the
// provided logical switch and returns the corresponding ops
func RemoveLoadBalancersFromLogicalSwitchOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, sw *nbdb.LogicalSwitch, lbs ...*nbdb.LoadBalancer) ([]ovsdb.Operation, error) {
	sw.LoadBalancer = make([]string, 0, len(lbs))
	for _, lb := range lbs {
		sw.LoadBalancer = append(sw.LoadBalancer, lb.UUID)
	}
	opModel := operationModel{
		Model:            sw,
		OnModelMutations: []interface{}{&sw.LoadBalancer},
		// if we want to delete loadbalancer from the switch that doesn't exist, that is noop
		ErrNotFound: false,
		BulkOp:      false,
	}

	modelClient := newModelClient(nbClient)
	return modelClient.DeleteOps(ops, opModel)
}

// AddLoadBalancerGroupsToLogicalSwitchOps adds the provided load balancer groups to the
// provided logical switch and returns the corresponding ops
func AddLoadBalancerGroupsToLogicalSwitchOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, sw *nbdb.LogicalSwitch, lbGroups ...*nbdb.LoadBalancerGroup) ([]ovsdb.Operation, error) {
	sw.LoadBalancerGroup = make([]string, 0, len(lbGroups))
	for _, lbg := range lbGroups {
		sw.LoadBalancerGroup = append(sw.LoadBalancerGroup, lbg.UUID)
	}
	opModel := operationModel{
		Model:            sw,
		OnModelMutations: []interface{}{&sw.LoadBalancerGroup},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModel)
}

// RemoveLoadBalancerGroupsFromLogicalSwitchOps removes the provided load balancer groups from the
// provided logical switch and returns the corresponding ops
func RemoveLoadBalancerGroupsFromLogicalSwitchOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, sw *nbdb.LogicalSwitch, lbGroups ...*nbdb.LoadBalancerGroup) ([]ovsdb.Operation, error) {
	sw.LoadBalancerGroup = make([]string, 0, len(lbGroups))
	for _, lbg := range lbGroups {
		sw.LoadBalancerGroup = append(sw.LoadBalancerGroup, lbg.UUID)
	}
	opModel := operationModel{
		Model:            sw,
		OnModelMutations: []interface{}{&sw.LoadBalancerGroup},
		// if we want to delete load balancer group from the switch that doesn't exist, that is noop
		ErrNotFound: false,
		BulkOp:      false,
	}

	modelClient := newModelClient(nbClient)
	return modelClient.DeleteOps(ops, opModel)
}

// ACL ops

// AddACLsToLogicalSwitchOps adds the provided ACLs to the provided logical
// switch and returns the corresponding ops
func AddACLsToLogicalSwitchOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, name string, acls ...*nbdb.ACL) ([]ovsdb.Operation, error) {
	sw := &nbdb.LogicalSwitch{
		Name: name,
		ACLs: make([]string, 0, len(acls)),
	}
	for _, acl := range acls {
		sw.ACLs = append(sw.ACLs, acl.UUID)
	}

	opModels := operationModel{
		Model:            sw,
		OnModelMutations: []interface{}{&sw.ACLs},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModels)
}

// RemoveACLsFromLogicalSwitchOps removes the provided ACLs from the named logical switch
// and returns the corresponding ops
func RemoveACLsFromLogicalSwitchOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, name string, acls ...*nbdb.ACL) ([]ovsdb.Operation, error) {
	sw := &nbdb.LogicalSwitch{
		Name: name,
		ACLs: make([]string, 0, len(acls)),
	}
	for _, acl := range acls {
		sw.ACLs = append(sw.ACLs, acl.UUID)
	}

	opModel := operationModel{
		Model:            sw,
		OnModelMutations: []interface{}{&sw.ACLs},
		ErrNotFound:      false,
		BulkOp:           false,
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModel)
}

// RemoveACLsFromLogicalSwitchesWithPredicateOps looks up logical switches from the cache
// based on a given predicate, removes from them the provided ACLs, and returns the
// corresponding ops
func RemoveACLsFromLogicalSwitchesWithPredicateOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation,
	p switchPredicate, acls ...*nbdb.ACL) ([]ovsdb.Operation, error) {
	sw := nbdb.LogicalSwitch{
		ACLs: make([]string, 0, len(acls)),
	}
	for _, acl := range acls {
		sw.ACLs = append(sw.ACLs, acl.UUID)
	}
	opModel := operationModel{
		Model:            &sw,
		ModelPredicate:   p,
		OnModelMutations: []interface{}{&sw.ACLs},
		ErrNotFound:      false,
		BulkOp:           true,
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModel)
}

// RemoveACLsFromLogicalSwitchesWithPredicate looks up logical switches from the cache
// based on a given predicate and removes from them the provided ACLs
func RemoveACLsFromLogicalSwitchesWithPredicate(nbClient libovsdbclient.Client, p switchPredicate, acls ...*nbdb.ACL) error {
	ops, err := RemoveACLsFromLogicalSwitchesWithPredicateOps(nbClient, nil, p, acls...)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// UpdateLogicalSwitchSetOtherConfig sets other config on the provided logical
// switch adding any missing, removing the ones set to an empty value and
// updating existing
func UpdateLogicalSwitchSetOtherConfig(nbClient libovsdbclient.Client, sw *nbdb.LogicalSwitch) error {
	otherConfig := sw.OtherConfig
	sw, err := GetLogicalSwitch(nbClient, sw)
	if err != nil {
		return err
	}

	if sw.OtherConfig == nil {
		sw.OtherConfig = map[string]string{}
	}

	for k, v := range otherConfig {
		if v == "" {
			delete(sw.OtherConfig, k)
		} else {
			sw.OtherConfig[k] = v
		}
	}

	opModel := operationModel{
		Model:          sw,
		OnModelUpdates: []interface{}{&sw.OtherConfig},
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	_, err = m.CreateOrUpdate(opModel)
	return err
}

// LOGICAL SWITCH PORT OPs

// GetLogicalSwitchPort looks up a logical switch port from the cache
func GetLogicalSwitchPort(nbClient libovsdbclient.Client, lsp *nbdb.LogicalSwitchPort) (*nbdb.LogicalSwitchPort, error) {
	found := []*nbdb.LogicalSwitchPort{}
	opModel := operationModel{
		Model:          lsp,
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	err := m.Lookup(opModel)
	if err != nil {
		return nil, err
	}

	return found[0], nil
}

func createOrUpdateLogicalSwitchPortOpModelWithCustomFields(sw *nbdb.LogicalSwitch, lsp *nbdb.LogicalSwitchPort, createLSP bool, customFields []ModelUpdateField) operationModel {
	var fieldInterfaces []interface{}
	if len(customFields) != 0 {
		fieldInterfaces = getFieldsToUpdate(lsp, customFields)
	} else {
		fieldInterfaces = getAllUpdatableFields(lsp)
	}
	return operationModel{
		Model:          lsp,
		OnModelUpdates: fieldInterfaces,
		DoAfter: func() {
			// lsp.UUID should be set here
			sw.Ports = append(sw.Ports, lsp.UUID)
		},
		ErrNotFound: !createLSP,
		BulkOp:      false,
	}
}

func createOrUpdateLogicalSwitchPortsOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, sw *nbdb.LogicalSwitch, createSwitch, createLSP bool, customFields []ModelUpdateField, lsps ...*nbdb.LogicalSwitchPort) ([]ovsdb.Operation, error) {
	originalPorts := sw.Ports
	sw.Ports = make([]string, 0, len(lsps))
	opModels := make([]operationModel, 0, len(lsps)+1)

	for _, lsp := range lsps {
		opModel := createOrUpdateLogicalSwitchPortOpModelWithCustomFields(sw, lsp, createLSP, customFields)
		opModels = append(opModels, opModel)
	}

	opModelSwitch := operationModel{
		Model:            sw,
		OnModelMutations: []interface{}{&sw.Ports},
		ErrNotFound:      !createSwitch,
		BulkOp:           false,
	}
	opModels = append(opModels, opModelSwitch)

	m := newModelClient(nbClient)
	ops, err := m.CreateOrUpdateOps(ops, opModels...)
	sw.Ports = originalPorts
	if err != nil && errors.Is(err, libovsdbclient.ErrNotFound) && !createSwitch {
		err = fmt.Errorf("could not find switch: %q, %w", sw.Name, err)
	}
	return ops, err
}

func createOrUpdateLogicalSwitchPorts(nbClient libovsdbclient.Client, sw *nbdb.LogicalSwitch, createSwitch bool, lsps ...*nbdb.LogicalSwitchPort) error {
	ops, err := createOrUpdateLogicalSwitchPortsOps(nbClient, nil, sw, createSwitch, true, nil, lsps...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheckAndSetUUIDs(nbClient, lsps, ops)
	return err
}

// CreateOrUpdateLogicalSwitchPortsOnSwitchWithCustomFieldsOps creates or updates the provided
// logical switch ports, adds them to the provided logical switch and returns
// the corresponding ops
func CreateOrUpdateLogicalSwitchPortsOnSwitchWithCustomFieldsOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, sw *nbdb.LogicalSwitch, customFields []ModelUpdateField, lsps ...*nbdb.LogicalSwitchPort) ([]ovsdb.Operation, error) {
	return createOrUpdateLogicalSwitchPortsOps(nbClient, ops, sw, false, true, customFields, lsps...)
}

// UpdateLogicalSwitchPortsOnSwitchWithCustomFieldsOps updates the provided
// logical switch ports, adds them to the provided logical switch and returns
// the corresponding ops
func UpdateLogicalSwitchPortsOnSwitchWithCustomFieldsOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, sw *nbdb.LogicalSwitch, customFields []ModelUpdateField, lsps ...*nbdb.LogicalSwitchPort) ([]ovsdb.Operation, error) {
	return createOrUpdateLogicalSwitchPortsOps(nbClient, ops, sw, false, false, customFields, lsps...)
}

// CreateOrUpdateLogicalSwitchPortsOnSwitch creates or updates the provided
// logical switch ports and adds them to the provided logical switch
func CreateOrUpdateLogicalSwitchPortsOnSwitch(nbClient libovsdbclient.Client, sw *nbdb.LogicalSwitch, lsps ...*nbdb.LogicalSwitchPort) error {
	return createOrUpdateLogicalSwitchPorts(nbClient, sw, false, lsps...)
}

// CreateOrUpdateLogicalSwitchPortsAndSwitchOps creates or updates the provided
// logical switch ports and adds them to the provided logical switch creating it
// if it does not exist and returns the corresponding ops
func CreateOrUpdateLogicalSwitchPortsAndSwitchOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, sw *nbdb.LogicalSwitch, lsps ...*nbdb.LogicalSwitchPort) ([]ovsdb.Operation, error) {
	return createOrUpdateLogicalSwitchPortsOps(nbClient, ops, sw, true, true, nil, lsps...)
}

// DeleteLogicalSwitchPortsOps deletes the provided logical switch ports, removes
// them from the provided logical switch and returns the corresponding ops
func DeleteLogicalSwitchPortsOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, sw *nbdb.LogicalSwitch, lsps ...*nbdb.LogicalSwitchPort) ([]ovsdb.Operation, error) {
	originalPorts := sw.Ports
	sw.Ports = make([]string, 0, len(lsps))
	opModels := make([]operationModel, 0, len(lsps)+1)
	for i := range lsps {
		lsp := lsps[i]
		opModel := operationModel{
			Model: lsp,
			DoAfter: func() {
				if lsp.UUID != "" {
					sw.Ports = append(sw.Ports, lsp.UUID)
				}
			},
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels = append(opModels, opModel)
	}
	opModel := operationModel{
		Model:            sw,
		OnModelMutations: []interface{}{&sw.Ports},
		ErrNotFound:      false,
		BulkOp:           false,
	}
	opModels = append(opModels, opModel)

	m := newModelClient(nbClient)
	ops, err := m.DeleteOps(ops, opModels...)
	sw.Ports = originalPorts
	return ops, err
}

// DeleteLogicalSwitchPorts deletes the provided logical switch ports and
// removes them from the provided logical switch
func DeleteLogicalSwitchPorts(nbClient libovsdbclient.Client, sw *nbdb.LogicalSwitch, lsps ...*nbdb.LogicalSwitchPort) error {
	ops, err := DeleteLogicalSwitchPortsOps(nbClient, nil, sw, lsps...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

type logicalSwitchPortPredicate func(*nbdb.LogicalSwitchPort) bool

// DeleteLogicalSwitchPortsWithPredicateOps looks up logical switch ports from
// the cache based on a given predicate and removes from them the provided
// logical switch
func DeleteLogicalSwitchPortsWithPredicateOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, sw *nbdb.LogicalSwitch, p logicalSwitchPortPredicate) ([]ovsdb.Operation, error) {
	swName := sw.Name
	sw, err := GetLogicalSwitch(nbClient, sw)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return ops, nil
		}
		return nil, fmt.Errorf("error retrieving logical switch %s from libovsdb cache: %w", swName, err)
	}

	var matches []*nbdb.LogicalSwitchPort
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	err = nbClient.WhereCacheByUUIDs(p, sw.Ports...).List(ctx, &matches)
	if err != nil {
		return nil, fmt.Errorf("error retrieving logical switch ports associated with logical switch %s from libovsdb cache: %w", swName, err)
	}
	portsByUUID := make(map[string]*nbdb.LogicalSwitchPort, len(matches))
	for _, lsp := range matches {
		portsByUUID[lsp.UUID] = lsp
	}
	lsps := make([]*nbdb.LogicalSwitchPort, 0, len(matches))
	for _, portUUID := range sw.Ports {
		if lsp, ok := portsByUUID[portUUID]; ok {
			lsps = append(lsps, lsp)
		}
	}

	opModels := make([]operationModel, 0, len(lsps)+1)
	sw.Ports = make([]string, 0, len(lsps))
	for _, lsp := range lsps {
		sw.Ports = append(sw.Ports, lsp.UUID)
		opModel := operationModel{
			Model:       lsp,
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels = append(opModels, opModel)
	}
	opModel := operationModel{
		Model:            sw,
		OnModelMutations: []interface{}{&sw.Ports},
		ErrNotFound:      false,
		BulkOp:           false,
	}
	opModels = append(opModels, opModel)

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModels...)
}

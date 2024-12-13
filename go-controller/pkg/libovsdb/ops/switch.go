package ops

import (
	"context"
	"errors"
	"fmt"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
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
func DeleteLogicalSwitchesWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation,
	p logicalSwitchPredicate) ([]libovsdb.Operation, error) {
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
func DeleteLogicalSwitchOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation,
	swName string) ([]libovsdb.Operation, error) {
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
func AddLoadBalancersToLogicalSwitchOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, sw *nbdb.LogicalSwitch, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
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
func RemoveLoadBalancersFromLogicalSwitchOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, sw *nbdb.LogicalSwitch, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
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

// ACL ops

// AddACLsToLogicalSwitchOps adds the provided ACLs to the provided logical
// switch and returns the corresponding ops
func AddACLsToLogicalSwitchOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, name string, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
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

// RemoveACLsFromLogicalSwitchesWithPredicateOps looks up logical switches from the cache
// based on a given predicate, removes from them the provided ACLs, and returns the
// corresponding ops
func RemoveACLsFromLogicalSwitchesWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation,
	p switchPredicate, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
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

func createOrUpdateLogicalSwitchPortsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, sw *nbdb.LogicalSwitch, createSwitch, createLSP bool, customFields []ModelUpdateField, lsps ...*nbdb.LogicalSwitchPort) ([]libovsdb.Operation, error) {
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
func CreateOrUpdateLogicalSwitchPortsOnSwitchWithCustomFieldsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, sw *nbdb.LogicalSwitch, customFields []ModelUpdateField, lsps ...*nbdb.LogicalSwitchPort) ([]libovsdb.Operation, error) {
	return createOrUpdateLogicalSwitchPortsOps(nbClient, ops, sw, false, true, customFields, lsps...)
}

// UpdateLogicalSwitchPortsOnSwitchWithCustomFieldsOps updates the provided
// logical switch ports, adds them to the provided logical switch and returns
// the corresponding ops
func UpdateLogicalSwitchPortsOnSwitchWithCustomFieldsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, sw *nbdb.LogicalSwitch, customFields []ModelUpdateField, lsps ...*nbdb.LogicalSwitchPort) ([]libovsdb.Operation, error) {
	return createOrUpdateLogicalSwitchPortsOps(nbClient, ops, sw, false, false, customFields, lsps...)
}

// CreateOrUpdateLogicalSwitchPortsOnSwitch creates or updates the provided
// logical switch ports and adds them to the provided logical switch
func CreateOrUpdateLogicalSwitchPortsOnSwitch(nbClient libovsdbclient.Client, sw *nbdb.LogicalSwitch, lsps ...*nbdb.LogicalSwitchPort) error {
	return createOrUpdateLogicalSwitchPorts(nbClient, sw, false, lsps...)
}

// CreateOrUpdateLogicalSwitchPortsAndSwitch creates or updates the provided
// logical switch ports and adds them to the provided logical switch creating it
// if it does not exist
func CreateOrUpdateLogicalSwitchPortsAndSwitch(nbClient libovsdbclient.Client, sw *nbdb.LogicalSwitch, lsps ...*nbdb.LogicalSwitchPort) error {
	return createOrUpdateLogicalSwitchPorts(nbClient, sw, true, lsps...)
}

// DeleteLogicalSwitchPortsOps deletes the provided logical switch ports, removes
// them from the provided logical switch and returns the corresponding ops
func DeleteLogicalSwitchPortsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, sw *nbdb.LogicalSwitch, lsps ...*nbdb.LogicalSwitchPort) ([]libovsdb.Operation, error) {
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
func DeleteLogicalSwitchPortsWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, sw *nbdb.LogicalSwitch, p logicalSwitchPortPredicate) ([]libovsdb.Operation, error) {
	swName := sw.Name
	sw, err := GetLogicalSwitch(nbClient, sw)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return ops, nil
		}
		return nil, fmt.Errorf("error retrieving logical switch %s from libovsdb cache: %w", swName, err)
	}

	var lsps []*nbdb.LogicalSwitchPort
	for _, port := range sw.Ports {
		lsp := &nbdb.LogicalSwitchPort{UUID: port}
		lsp, err = GetLogicalSwitchPort(nbClient, lsp)
		if err != nil {
			if errors.Is(err, libovsdbclient.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("error retrieving logical switch port with UUID %s associated with logical"+
				" switch %s from libovsdb cache: %w", port, swName, err)
		}
		if p(lsp) {
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

// UpdateLogicalSwitchPortSetOptions sets options on the provided logical switch
// port adding any missing, removing the ones set to an empty value and updating
// existing
func UpdateLogicalSwitchPortSetOptions(nbClient libovsdbclient.Client, lsp *nbdb.LogicalSwitchPort) error {
	options := lsp.Options
	lsp, err := GetLogicalSwitchPort(nbClient, lsp)
	if err != nil {
		return err
	}

	if lsp.Options == nil {
		lsp.Options = map[string]string{}
	}

	for k, v := range options {
		if v == "" {
			delete(lsp.Options, k)
		} else {
			lsp.Options[k] = v
		}
	}

	opModel := operationModel{
		// For LSP's Name is a valid index, so no predicate is needed
		Model:          lsp,
		OnModelUpdates: []interface{}{&lsp.Options},
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	_, err = m.CreateOrUpdate(opModel)
	return err
}

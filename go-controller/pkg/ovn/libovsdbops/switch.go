package libovsdbops

import (
	"context"
	"fmt"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// findSwitch looks up the switch in the cache and sets the UUID
func findSwitch(nbClient libovsdbclient.Client, lswitch *nbdb.LogicalSwitch) error {
	if lswitch.UUID != "" && !IsNamedUUID(lswitch.UUID) {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	switches := []nbdb.LogicalSwitch{}
	err := nbClient.WhereCache(func(item *nbdb.LogicalSwitch) bool {
		return item.Name == lswitch.Name
	}).List(ctx, &switches)
	if err != nil {
		return fmt.Errorf("can't find switch %+v: %v", *lswitch, err)
	}

	if len(switches) > 1 {
		return fmt.Errorf("unexpectedly found multiple switches: %+v", switches)
	}

	if len(switches) == 0 {
		return libovsdbclient.ErrNotFound
	}

	lswitch.UUID = switches[0].UUID
	return nil
}

// findSwitches returns all the current logicalSwitches
func findSwitches(nbClient libovsdbclient.Client) ([]nbdb.LogicalSwitch, error) {
	switches := []nbdb.LogicalSwitch{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.List(ctx, &switches)
	if err != nil {
		return nil, fmt.Errorf("can't find Locial Switches err: %v", err)
	}

	if len(switches) == 0 {
		return nil, libovsdbclient.ErrNotFound
	}

	return switches, nil
}

// findSwitchesByPredicate Looks up switches in the cache based on the lookup function
func findSwitchesByPredicate(nbClient libovsdbclient.Client, lookupFunction func(item *nbdb.LogicalSwitch) bool) ([]nbdb.LogicalSwitch, error) {
	switches := []nbdb.LogicalSwitch{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereCache(lookupFunction).List(ctx, &switches)
	if err != nil {
		return nil, fmt.Errorf("can't find switches: %v", err)
	}

	if len(switches) == 0 {
		return nil, libovsdbclient.ErrNotFound
	}

	return switches, nil
}

// FindSwitchesWithOtherConfig finds switches with otherconfig value/s
func FindSwitchesWithOtherConfig(nbClient libovsdbclient.Client) ([]nbdb.LogicalSwitch, error) {
	// Get all logical siwtches with other-config set
	otherConfigSearch := func(item *nbdb.LogicalSwitch) bool {
		return item.OtherConfig != nil
	}

	switches, err := findSwitchesByPredicate(nbClient, otherConfigSearch)
	if err != nil {
		return nil, err
	}

	return switches, nil
}

func FindPerNodeJoinSwitches(nbClient libovsdbclient.Client) ([]nbdb.LogicalSwitch, error) {
	// Get the legacy node join switches -> join_<NodeName>
	joinSwitchSearch := func(item *nbdb.LogicalSwitch) bool {
		return strings.HasPrefix(item.Name, types.JoinSwitchPrefix)
	}

	switches, err := findSwitchesByPredicate(nbClient, joinSwitchSearch)
	if err != nil {
		return nil, err
	}

	return switches, nil
}

func AddLoadBalancersToSwitchOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, lswitch *nbdb.LogicalSwitch, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	if len(lbs) == 0 {
		return ops, nil
	}

	err := findSwitch(nbClient, lswitch)
	if err != nil {
		return nil, err
	}

	lbUUIDs := make([]string, 0, len(lbs))
	for _, lb := range lbs {
		lbUUIDs = append(lbUUIDs, lb.UUID)
	}

	op, err := nbClient.Where(lswitch).Mutate(lswitch, model.Mutation{
		Field:   &lswitch.LoadBalancer,
		Mutator: libovsdb.MutateOperationInsert,
		Value:   lbUUIDs,
	})
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)
	return ops, nil
}

func RemoveLoadBalancersFromSwitchOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, lswitch *nbdb.LogicalSwitch, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	if len(lbs) == 0 {
		return ops, nil
	}

	err := findSwitch(nbClient, lswitch)
	if err != nil {
		return nil, err
	}

	lbUUIDs := make([]string, 0, len(lbs))
	for _, lb := range lbs {
		lbUUIDs = append(lbUUIDs, lb.UUID)
	}

	op, err := nbClient.Where(lswitch).Mutate(lswitch, model.Mutation{
		Field:   &lswitch.LoadBalancer,
		Mutator: libovsdb.MutateOperationDelete,
		Value:   lbUUIDs,
	})
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)

	return ops, nil
}

func ListSwitchesWithLoadBalancers(nbClient libovsdbclient.Client) ([]nbdb.LogicalSwitch, error) {
	switches := &[]nbdb.LogicalSwitch{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereCache(func(item *nbdb.LogicalSwitch) bool {
		return item.LoadBalancer != nil
	}).List(ctx, switches)
	return *switches, err
}

// RemoveACLFromSwitches removes the ACL uuid entry from Logical Switch acl's list.
func removeACLsFromSwitches(nbClient libovsdbclient.Client, switches []nbdb.LogicalSwitch, acls []nbdb.ACL) error {
	var opModels []OperationModel
	var aclUUIDs []string

	for _, acl := range acls {
		aclUUIDs = append(aclUUIDs, acl.UUID)
	}

	for i, sw := range switches {
		sw.ACLs = aclUUIDs
		swName := switches[i].Name
		opModels = append(opModels, OperationModel{
			Model:          &sw,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == swName },
			OnModelMutations: []interface{}{
				&sw.ACLs,
			},
			ErrNotFound: true,
			BulkOp:      true,
		})
	}

	m := NewModelClient(nbClient)
	if err := m.Delete(opModels...); err != nil {
		return fmt.Errorf("error while removing ACLS: %v, from switches err: %v", aclUUIDs, err)
	}

	return nil
}

// RemoveACLsFromNodeSwitches removes the specified ACLs from the per node Logical Switches
func RemoveACLsFromNodeSwitches(nbClient libovsdbclient.Client, acls []nbdb.ACL) error {
	// Find all node switches
	nodeSwichLookupFcn := func(item *nbdb.LogicalSwitch) bool {
		// Ignore external and Join switches(both legacy and current)
		return !(strings.HasPrefix(item.Name, types.JoinSwitchPrefix) || item.Name == "join" || strings.HasPrefix(item.Name, types.ExternalSwitchPrefix))
	}

	switches, err := findSwitchesByPredicate(nbClient, nodeSwichLookupFcn)
	if err != nil {
		return err
	}

	err = removeACLsFromSwitches(nbClient, switches, acls)
	if err != nil {
		return err
	}

	return nil
}

// RemoveACLsFromJoinSwitch removes the specified ACLs from the distributed join switch
func RemoveACLsFromJoinSwitch(nbClient libovsdbclient.Client, acls []nbdb.ACL) error {
	// Find join switch
	joinSwichLookupFcn := func(item *nbdb.LogicalSwitch) bool {
		// Return only join switch (the per node ones if its old topology & distributed one if its new topology)
		return (strings.HasPrefix(item.Name, types.JoinSwitchPrefix) || item.Name == "join")
	}

	switches, err := findSwitchesByPredicate(nbClient, joinSwichLookupFcn)
	if err != nil {
		return err
	}

	err = removeACLsFromSwitches(nbClient, switches, acls)
	if err != nil {
		return err
	}

	return nil
}

// RemoveACLFromSwitches removes the ACL uuid entry from Logical Switch acl's list.
func RemoveACLsFromAllSwitches(nbClient libovsdbclient.Client, acls []nbdb.ACL) error {
	// Find all switches
	switches, err := findSwitches(nbClient)
	if err != nil {
		return err
	}

	err = removeACLsFromSwitches(nbClient, switches, acls)
	if err != nil {
		return err
	}

	return nil
}

// AddACLToNodeSwitch will add the provided ACL to a singe nodeSwitch, create the ACL if needed
func AddACLToNodeSwitch(nbClient libovsdbclient.Client, nodeName string, nodeACL *nbdb.ACL) error {
	nodeSwitch := nbdb.LogicalSwitch{
		Name: nodeName,
	}

	existingACLres := []nbdb.ACL{}

	// Here we either need to create the ACL and add to the LS or simply add to the LS
	opModels := []OperationModel{
		{
			Model:          nodeACL,
			ModelPredicate: func(acl *nbdb.ACL) bool { return IsEquivalentACL(acl, nodeACL) },
			ExistingResult: &existingACLres,
			DoAfter: func() {
				// Bulkop is false, we should fail early if we get more than one result
				// If ACL exists It's UUID will be in opModel.ExistingResult
				// If it does not, it will be added to the model
				if len(existingACLres) == 1 {
					nodeSwitch.ACLs = []string{existingACLres[0].UUID}
				} else {
					nodeSwitch.ACLs = []string{nodeACL.UUID}
				}
			},
		},
		{
			Model:          &nodeSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == nodeName },
			OnModelMutations: []interface{}{
				&nodeSwitch.ACLs,
			},
			ErrNotFound: true,
		},
	}

	m := NewModelClient(nbClient)
	if _, err := m.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add ACL %v, error: %v", nodeACL, err)
	}

	return nil
}

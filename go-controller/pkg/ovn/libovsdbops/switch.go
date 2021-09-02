package libovsdbops

import (
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// findSwitch looks up the switch in the cache and sets the UUID
func findSwitch(nbClient libovsdbclient.Client, lswitch *nbdb.LogicalSwitch) error {
	if lswitch.UUID != "" && !IsNamedUUID(lswitch.UUID) {
		return nil
	}

	switches := []nbdb.LogicalSwitch{}
	err := nbClient.WhereCache(func(item *nbdb.LogicalSwitch) bool {
		return item.Name == lswitch.Name
	}).List(&switches)
	if err != nil {
		return fmt.Errorf("can't find router %+v: %v", *lswitch, err)
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
	err := nbClient.WhereCache(func(item *nbdb.LogicalSwitch) bool {
		return item.LoadBalancer != nil
	}).List(switches)
	return *switches, err
}

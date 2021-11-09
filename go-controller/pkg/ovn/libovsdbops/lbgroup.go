package libovsdbops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// findLBGroup looks up the Group in the cache and sets the UUID
func findLBGroup(nbClient libovsdbclient.Client, group *nbdb.LoadBalancerGroup) error {
	if group.UUID != "" && !IsNamedUUID(group.UUID) {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	groups := []nbdb.LoadBalancerGroup{}
	err := nbClient.WhereCache(func(item *nbdb.LoadBalancerGroup) bool {
		return item.Name == group.Name
	}).List(ctx, &groups)
	if err != nil {
		return fmt.Errorf("can't find LB group %+v: %v", *group, err)
	}

	if len(groups) > 1 {
		return fmt.Errorf("unexpectedly found multiple LB Groups: %+v", groups)
	}

	if len(groups) == 0 {
		return libovsdbclient.ErrNotFound
	}

	group.UUID = groups[0].UUID
	return nil
}

func AddLoadBalancersToGroupOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, group *nbdb.LoadBalancerGroup, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	if len(lbs) == 0 {
		return ops, nil
	}

	err := findLBGroup(nbClient, group)
	if err != nil {
		return nil, err
	}

	lbUUIDs := make([]string, 0, len(lbs))
	for _, lb := range lbs {
		lbUUIDs = append(lbUUIDs, lb.UUID)
	}

	op, err := nbClient.Where(group).Mutate(group, model.Mutation{
		Field:   &group.LoadBalancer,
		Mutator: libovsdb.MutateOperationInsert,
		Value:   lbUUIDs,
	})
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)
	return ops, nil
}

func RemoveLoadBalancersFromGroupOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, group *nbdb.LoadBalancerGroup, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	if len(lbs) == 0 {
		return ops, nil
	}

	err := findLBGroup(nbClient, group)
	if err != nil {
		return nil, err
	}

	lbUUIDs := make([]string, 0, len(lbs))
	for _, lb := range lbs {
		lbUUIDs = append(lbUUIDs, lb.UUID)
	}

	op, err := nbClient.Where(group).Mutate(group, model.Mutation{
		Field:   &group.LoadBalancer,
		Mutator: libovsdb.MutateOperationDelete,
		Value:   lbUUIDs,
	})
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)

	return ops, nil
}

func ListGroupsWithLoadBalancers(nbClient libovsdbclient.Client) ([]nbdb.LoadBalancerGroup, error) {
	groups := &[]nbdb.LoadBalancerGroup{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereCache(func(item *nbdb.LoadBalancerGroup) bool {
		return item.LoadBalancer != nil
	}).List(ctx, groups)
	if err != nil {
		return nil, err
	}
	return *groups, nil
}

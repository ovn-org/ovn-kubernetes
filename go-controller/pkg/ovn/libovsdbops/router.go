package libovsdbops

import (
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// findRouter looks up the router in the cache and sets the UUID
func findRouter(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter) error {
	if router.UUID != "" && !IsNamedUUID(router.UUID) {
		return nil
	}

	routers := []nbdb.LogicalRouter{}
	err := nbClient.WhereCache(func(item *nbdb.LogicalRouter) bool {
		return item.Name == router.Name
	}).List(&routers)
	if err != nil {
		return fmt.Errorf("can't find router %+v: %v", *router, err)
	}

	if len(routers) > 1 {
		return fmt.Errorf("unexpectedly found multiple routers: %+v", routers)
	}

	if len(routers) == 0 {
		return libovsdbclient.ErrNotFound
	}

	router.UUID = routers[0].UUID
	return nil
}

func AddLoadBalancersToRouterOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	if len(lbs) == 0 {
		return ops, nil
	}

	err := findRouter(nbClient, router)
	if err != nil {
		return nil, err
	}

	lbUUIDs := make([]string, 0, len(lbs))
	for _, lb := range lbs {
		lbUUIDs = append(lbUUIDs, lb.UUID)
	}

	op, err := nbClient.Where(router).Mutate(router, model.Mutation{
		Field:   &router.LoadBalancer,
		Mutator: libovsdb.MutateOperationInsert,
		Value:   lbUUIDs,
	})
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)

	return ops, nil
}

func RemoveLoadBalancersFromRouterOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	if len(lbs) == 0 {
		return ops, nil
	}

	err := findRouter(nbClient, router)
	if err != nil {
		return nil, err
	}

	lbUUIDs := make([]string, 0, len(lbs))
	for _, lb := range lbs {
		lbUUIDs = append(lbUUIDs, lb.UUID)
	}

	op, err := nbClient.Where(router).Mutate(router, model.Mutation{
		Field:   &router.LoadBalancer,
		Mutator: libovsdb.MutateOperationDelete,
		Value:   lbUUIDs,
	})
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)

	return ops, nil
}

func ListRoutersWithLoadBalancers(nbClient libovsdbclient.Client) ([]nbdb.LogicalRouter, error) {
	routers := &[]nbdb.LogicalRouter{}
	err := nbClient.WhereCache(func(item *nbdb.LogicalRouter) bool {
		return item.LoadBalancer != nil
	}).List(routers)
	return *routers, err
}

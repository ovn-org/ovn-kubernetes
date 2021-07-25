package libovsdbops

import (
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

func FindRouter(nbClient libovsdbclient.Client, routerName string) (*nbdb.LogicalRouter, error) {
	routers := []nbdb.LogicalRouter{}
	err := nbClient.WhereCache(func(item *nbdb.LogicalRouter) bool {
		return item.Name == routerName
	}).List(&routers)

	if err != nil {
		return nil, fmt.Errorf("error finding logical router %s: %v", routerName, err)
	}

	if len(routers) == 0 {
		return nil, fmt.Errorf("found no logical routers with name %s", routerName)
	}

	if len(routers) > 1 {
		return nil, fmt.Errorf("unexpectedly found multiple logical routers: %+v", routers)
	}

	return &routers[0], nil
}

func mutateRouterNatsOps(mutator libovsdb.Mutator, nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, natUUIDs ...string) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	mutations := []model.Mutation{
		{
			Field:   &router.Nat,
			Mutator: mutator,
			Value:   natUUIDs,
		},
	}
	mutateOp, err := nbClient.Where(router).Mutate(router, mutations...)
	if err != nil {
		return ops, err
	}
	ops = append(ops, mutateOp...)
	return ops, nil
}

func AddNatsToRouterOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, natUUIDs ...string) ([]libovsdb.Operation, error) {
	ops, err := mutateRouterNatsOps(libovsdb.MutateOperationInsert, nbClient, ops, router, natUUIDs...)
	if err != nil {
		return ops, fmt.Errorf("error adding NAT to logical router %s: %v", router.Name, err)
	}
	return ops, nil
}

func DelNatsFromRouterOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, natUUIDs ...string) ([]libovsdb.Operation, error) {
	ops, err := mutateRouterNatsOps(libovsdb.MutateOperationDelete, nbClient, ops, router, natUUIDs...)
	if err != nil {
		return ops, fmt.Errorf("error deleting NAT from logical router %s: %v", router.Name, err)
	}
	return ops, nil
}

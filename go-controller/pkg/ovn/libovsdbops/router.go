package libovsdbops

import (
	"context"
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"k8s.io/apimachinery/pkg/util/sets"
)

// findRouter looks up the router in the cache
func findRouter(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter) (*nbdb.LogicalRouter, error) {
	var err error
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	if router.UUID != "" && !IsNamedUUID(router.UUID) {
		err = nbClient.Get(ctx, router)
		return router, err
	}

	routers := []nbdb.LogicalRouter{}
	err = nbClient.WhereCache(func(item *nbdb.LogicalRouter) bool {
		return item.Name == router.Name
	}).List(ctx, &routers)
	if err != nil {
		return nil, fmt.Errorf("can't find router %+v: %v", *router, err)
	}

	if len(routers) > 1 {
		return nil, fmt.Errorf("unexpectedly found multiple routers: %+v", routers)
	}

	if len(routers) == 0 {
		return nil, libovsdbclient.ErrNotFound
	}

	router.UUID = routers[0].UUID
	return &routers[0], nil
}

func AddLoadBalancersToRouterOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	if len(lbs) == 0 {
		return ops, nil
	}

	_, err := findRouter(nbClient, router)
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

	_, err := findRouter(nbClient, router)
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
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereCache(func(item *nbdb.LogicalRouter) bool {
		return item.LoadBalancer != nil
	}).List(ctx, routers)
	return *routers, err
}

func buildRouterNAT(
	natType nbdb.NATType,
	externalIP string,
	logicalIP string,
	logicalPort string,
	externalMac string,
	externalIDs map[string]string,
) *nbdb.NAT {
	nat := &nbdb.NAT{
		Type:        natType,
		ExternalIP:  externalIP,
		LogicalIP:   logicalIP,
		Options:     map[string]string{"stateless": "false"},
		ExternalIDs: externalIDs,
	}

	if logicalPort != "" {
		nat.LogicalPort = &logicalPort
	}

	if externalMac != "" {
		nat.ExternalMAC = &externalMac
	}

	return nat
}

func BuildRouterSNAT(
	externalIP *net.IP,
	logicalIP *net.IPNet,
	logicalPort string,
	externalIDs map[string]string,
) *nbdb.NAT {
	externalIPStr := ""
	if externalIP != nil {
		externalIPStr = externalIP.String()
	}
	// Strip out mask of logicalIP only if it is a host mask
	logicalIPMask, _ := logicalIP.Mask.Size()
	logicalIPStr := logicalIP.IP.String()
	if logicalIPMask != 32 && logicalIPMask != 128 {
		logicalIPStr = logicalIP.String()
	}
	return buildRouterNAT(nbdb.NATTypeSNAT, externalIPStr, logicalIPStr, logicalPort, "", externalIDs)
}

func BuildRouterDNATAndSNAT(
	externalIP *net.IP,
	logicalIP *net.IPNet,
	logicalPort string,
	externalMac string,
	externalIDs map[string]string,
) *nbdb.NAT {
	externalIPStr := ""
	if externalIP != nil {
		externalIPStr = externalIP.String()
	}
	logicalIPStr := ""
	if logicalIP != nil {
		logicalIPStr = logicalIP.IP.String()
	}
	return buildRouterNAT(
		nbdb.NATTypeDNATAndSNAT,
		externalIPStr,
		logicalIPStr,
		logicalPort,
		externalMac,
		externalIDs)
}

// isEquivalentNAT if it has same uuid. Otherwise, check if types match.
// ExternalIP must be unique amonst non-SNATs;
// LogicalIP must be unique amonst SNATs;
// If provided, LogicalPort is expected to match;
func isEquivalentNAT(existing *nbdb.NAT, searched *nbdb.NAT) bool {
	// Simple case: uuid was provided.
	if searched.UUID != "" && existing.UUID == searched.UUID {
		return true
	}

	if searched.Type != existing.Type {
		return false
	}

	// Compre externalIP if its not empty.
	if searched.ExternalIP != "" && searched.ExternalIP != existing.ExternalIP {
		return false
	}

	// Compare logicalIP only for SNAT, since DNAT types must have unique ExternalIP.
	if searched.Type == nbdb.NATTypeSNAT && searched.LogicalIP != existing.LogicalIP {
		return false
	}

	// When searching based on logicalPort, no need to go any further.
	if searched.LogicalPort != nil &&
		(existing.LogicalPort == nil || *searched.LogicalPort != *existing.LogicalPort) {
		return false
	}

	// When searched external ids is populated, check if provided key,value exist in existing row.
	// A usage case is when doing NAT operations where external id "name" is provided.
	for externalIdKey, externalIdValue := range searched.ExternalIDs {
		if foundValue, found := existing.ExternalIDs[externalIdKey]; !found || foundValue != externalIdValue {
			return false
		}
	}

	return true
}

func FindNatsUsingPredicate(nbClient libovsdbclient.Client, predicate func(item *nbdb.NAT) bool) ([]*nbdb.NAT, error) {
	nats := []nbdb.NAT{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereCache(predicate).List(ctx, &nats)
	if err != nil {
		return nil, fmt.Errorf("unable to find NAT IDs, err: %v", err)
	}
	// Turn nats into nat pointers, bc that is what callers actually need
	natsPtrs := make([]*nbdb.NAT, 0, len(nats))
	for _, nat := range nats {
		natsPtrs = append(natsPtrs, &nat)
	}
	return natsPtrs, nil
}

// FindRoutersUsingNat looks up routers that have any of the provided nats in its column
func FindRoutersUsingNat(nbClient libovsdbclient.Client, nats []*nbdb.NAT) ([]nbdb.LogicalRouter, error) {
	natUUIDs := sets.String{}
	for _, nat := range nats {
		if nat != nil {
			natUUIDs.Insert(nat.UUID)
		}
	}

	// At this point, we have a set of NAT UUIDs that we care about.
	// Iterate through the routers and identify which ones have these nat(s).
	routers := []nbdb.LogicalRouter{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereCache(func(item *nbdb.LogicalRouter) bool {
		for _, rtrNatUUID := range item.Nat {
			if natUUIDs.Has(rtrNatUUID) {
				return true
			}
		}
		return false
	}).List(ctx, &routers)

	if err != nil {
		return nil, fmt.Errorf("unable find routers, err: %v", err)
	}
	return routers, nil
}

func getRouterNats(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter) ([]*nbdb.NAT, error) {
	nats := []*nbdb.NAT{}

	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	var err error
	var nat *nbdb.NAT
	for _, rtrNatUUID := range router.Nat {
		nat = &nbdb.NAT{UUID: rtrNatUUID}
		err = nbClient.Get(ctx, nat)
		if err != nil {
			return nil, err
		}
		nats = append(nats, nat)
	}

	return nats, nil
}

// This non-public function can be leveraged by future cases when logical router is created with nats via libovsdb
func addOrUpdateNatToRouterOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, routerNats []*nbdb.NAT, nat *nbdb.NAT) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	if nat == nil {
		return ops, fmt.Errorf("error creating NAT for logical router %s: nat is nil", router.Name)
	}

	// Find out if NAT is already listed in the logical router.
	natIndex := -1
	for i := 0; natIndex == -1 && i < len(routerNats); i++ {
		if isEquivalentNAT(routerNats[i], nat) {
			// Done iterating on the very first match.
			natIndex = i
			break
		}
	}

	if natIndex == -1 {
		nat.UUID = BuildNamedUUID()

		op, err := nbClient.Create(nat)
		if err != nil {
			return ops, fmt.Errorf("error creating NAT %s for logical router %s %#v : %v", nat.UUID, router.Name, *nat, err)
		}
		ops = append(ops, op...)

		mutations := []model.Mutation{
			{
				Field:   &router.Nat,
				Mutator: libovsdb.MutateOperationInsert,
				Value:   []string{nat.UUID},
			},
		}
		mutateOp, err := nbClient.Where(router).Mutate(router, mutations...)
		if err != nil {
			return ops, err
		}
		ops = append(ops, mutateOp...)
	} else {
		op, err := nbClient.Where(
			&nbdb.NAT{
				UUID: routerNats[natIndex].UUID,
			}).Update(nat)
		if err != nil {
			return ops, fmt.Errorf("error updating NAT %s for logical router %s: %v", routerNats[natIndex].UUID, router.Name, err)
		}
		ops = append(ops, op...)
	}

	return ops, nil
}

func AddOrUpdateNatsToRouterOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, nats ...*nbdb.NAT) ([]libovsdb.Operation, error) {
	router, err := findRouter(nbClient, router)
	if err != nil {
		return ops, err
	}

	routerNats, err := getRouterNats(nbClient, router)
	if err != nil {
		return ops, err
	}

	for _, nat := range nats {
		if nat != nil {
			ops, err = addOrUpdateNatToRouterOps(nbClient, ops, router, routerNats, nat)
			if err != nil {
				return ops, err
			}
		}
	}

	return ops, nil
}

func DeleteNatsFromRouterOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, nats ...*nbdb.NAT) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	router, err := findRouter(nbClient, router)
	if err != nil {
		return ops, err
	}

	routerNats, err := getRouterNats(nbClient, router)
	if err != nil {
		return ops, err
	}

	natUUIDs := make([]string, 0, len(nats))
	natIndexesAdded := make(map[int]bool)
	for _, nat := range nats {
		if nat == nil {
			continue
		}
		for i := 0; i < len(routerNats); i++ {
			if !natIndexesAdded[i] && isEquivalentNAT(routerNats[i], nat) {
				natIndexesAdded[i] = true
				natUUIDs = append(natUUIDs, routerNats[i].UUID)
			}
		}
	}

	if len(natUUIDs) > 0 {
		for _, natUUID := range natUUIDs {
			op, err := nbClient.Where(
				&nbdb.NAT{
					UUID: natUUID,
				}).Delete()
			if err != nil {
				return ops, fmt.Errorf("error deleting NAT %s for logical router %s: %v", natUUID, router.Name, err)
			}
			ops = append(ops, op...)
		}

		mutations := []model.Mutation{
			{
				Field:   &router.Nat,
				Mutator: libovsdb.MutateOperationDelete,
				Value:   natUUIDs,
			},
		}
		mutateOp, err := nbClient.Where(router).Mutate(router, mutations...)
		if err != nil {
			return ops, err
		}
		ops = append(ops, mutateOp...)
	}

	return ops, nil
}

func AddOrUpdateNatsToRouter(nbClient libovsdbclient.Client, routerName string, nats ...*nbdb.NAT) error {
	router := &nbdb.LogicalRouter{Name: routerName}
	ops, err := AddOrUpdateNatsToRouterOps(nbClient, nil, router, nats...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func DeleteNatsFromRouter(nbClient libovsdbclient.Client, routerName string, nats ...*nbdb.NAT) error {
	router := &nbdb.LogicalRouter{Name: routerName}
	ops, err := DeleteNatsFromRouterOps(nbClient, nil, router, nats...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

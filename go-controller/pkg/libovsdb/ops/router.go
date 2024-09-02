package ops

import (
	"context"
	"errors"
	"fmt"
	"net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// ROUTER OPs

type logicalRouterPredicate func(*nbdb.LogicalRouter) bool

// GetLogicalRouter looks up a logical router from the cache
func GetLogicalRouter(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter) (*nbdb.LogicalRouter, error) {
	found := []*nbdb.LogicalRouter{}
	opModel := operationModel{
		Model:          router,
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

// FindLogicalRoutersWithPredicate looks up logical routers from the cache based on a
// given predicate
func FindLogicalRoutersWithPredicate(nbClient libovsdbclient.Client, p logicalRouterPredicate) ([]*nbdb.LogicalRouter, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	found := []*nbdb.LogicalRouter{}
	err := nbClient.WhereCache(p).List(ctx, &found)
	return found, err
}

// CreateOrUpdateLogicalRouter creates or updates the provided logical router
func CreateOrUpdateLogicalRouter(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter, fields ...interface{}) error {
	if len(fields) == 0 {
		fields = onModelUpdatesAllNonDefault()
	}
	opModel := operationModel{
		Model:          router,
		OnModelUpdates: fields,
		ErrNotFound:    false,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModel)
	return err
}

// UpdateLogicalRouterSetExternalIDs sets external IDs on the provided logical
// router adding any missing, removing the ones set to an empty value and
// updating existing
func UpdateLogicalRouterSetExternalIDs(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter) error {
	externalIds := router.ExternalIDs
	router, err := GetLogicalRouter(nbClient, router)
	if err != nil {
		return err
	}

	if router.ExternalIDs == nil {
		router.ExternalIDs = map[string]string{}
	}

	for k, v := range externalIds {
		if v == "" {
			delete(router.ExternalIDs, k)
		} else {
			router.ExternalIDs[k] = v
		}
	}

	opModel := operationModel{
		Model:          router,
		OnModelUpdates: []interface{}{&router.ExternalIDs},
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	_, err = m.CreateOrUpdate(opModel)
	return err
}

// DeleteLogicalRoutersWithPredicateOps returns the operations to delete the logical routers matching the provided predicate
func DeleteLogicalRoutersWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation,
	p logicalRouterPredicate) ([]libovsdb.Operation, error) {
	opModel := operationModel{
		Model:          &nbdb.LogicalRouter{},
		ModelPredicate: p,
		ErrNotFound:    false,
		BulkOp:         true,
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModel)
}

// DeleteLogicalRouterOps returns the operations to delete the provided logical router
func DeleteLogicalRouterOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation,
	router *nbdb.LogicalRouter) ([]libovsdb.Operation, error) {
	opModel := operationModel{
		Model:       router,
		ErrNotFound: false,
		BulkOp:      false,
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModel)
}

// DeleteLogicalRouter deletes the provided logical router
func DeleteLogicalRouter(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter) error {
	ops, err := DeleteLogicalRouterOps(nbClient, nil, router)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// LOGICAL ROUTER PORT OPs

type logicalRouterPortPredicate func(*nbdb.LogicalRouterPort) bool

// FindLogicalRouterPortWithPredicate looks up logical router port from
// the cache based on a given predicate
func FindLogicalRouterPortWithPredicate(nbClient libovsdbclient.Client, p logicalRouterPortPredicate) ([]*nbdb.LogicalRouterPort, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	found := []*nbdb.LogicalRouterPort{}
	err := nbClient.WhereCache(p).List(ctx, &found)
	return found, err
}

// GetLogicalRouterPort looks up a logical router port from the cache
func GetLogicalRouterPort(nbClient libovsdbclient.Client, lrp *nbdb.LogicalRouterPort) (*nbdb.LogicalRouterPort, error) {
	found := []*nbdb.LogicalRouterPort{}
	opModel := operationModel{
		Model:          lrp,
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

// CreateOrUpdateLogicalRouterPort creates or updates the provided logical
// router port together with the gateway chassis (if not nil), and adds it to the provided logical router
func CreateOrUpdateLogicalRouterPort(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter,
	lrp *nbdb.LogicalRouterPort, chassis *nbdb.GatewayChassis, fields ...interface{}) error {
	opModels := []operationModel{}
	if chassis != nil {
		opModels = append(opModels, operationModel{
			Model:          chassis,
			OnModelUpdates: onModelUpdatesAllNonDefault(),
			DoAfter:        func() { lrp.GatewayChassis = []string{chassis.UUID} },
			ErrNotFound:    false,
			BulkOp:         false,
		})
	}
	if len(fields) == 0 {
		fields = onModelUpdatesAllNonDefault()
	} else if chassis != nil {
		fields = append(fields, &lrp.GatewayChassis)
	}
	originalPorts := router.Ports
	router.Ports = []string{}
	opModels = append(opModels, operationModel{
		Model:          lrp,
		OnModelUpdates: fields,
		DoAfter:        func() { router.Ports = append(router.Ports, lrp.UUID) },
		ErrNotFound:    false,
		BulkOp:         false,
	})
	opModels = append(opModels, operationModel{
		Model:            router,
		OnModelMutations: []interface{}{&router.Ports},
		ErrNotFound:      true,
		BulkOp:           false,
	})
	m := newModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModels...)
	router.Ports = originalPorts
	return err
}

// DeleteLogicalRouterPorts deletes the provided logical router ports and
// removes them from the provided logical router
func DeleteLogicalRouterPorts(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter, lrps ...*nbdb.LogicalRouterPort) error {
	originalPorts := router.Ports
	router.Ports = make([]string, 0, len(lrps))
	opModels := make([]operationModel, 0, len(lrps)+1)
	for i := range lrps {
		lrp := lrps[i]
		opModel := operationModel{
			Model: lrp,
			DoAfter: func() {
				if lrp.UUID != "" {
					router.Ports = append(router.Ports, lrp.UUID)
				}
			},
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels = append(opModels, opModel)
	}
	opModel := operationModel{
		Model:            router,
		OnModelMutations: []interface{}{&router.Ports},
		ErrNotFound:      false,
		BulkOp:           false,
	}
	opModels = append(opModels, opModel)

	m := newModelClient(nbClient)
	err := m.Delete(opModels...)
	router.Ports = originalPorts
	return err
}

// LOGICAL ROUTER POLICY OPs

type logicalRouterPolicyPredicate func(*nbdb.LogicalRouterPolicy) bool

// FindLogicalRouterPoliciesWithPredicate looks up logical router policies from
// the cache based on a given predicate
func FindLogicalRouterPoliciesWithPredicate(nbClient libovsdbclient.Client, p logicalRouterPolicyPredicate) ([]*nbdb.LogicalRouterPolicy, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	found := []*nbdb.LogicalRouterPolicy{}
	err := nbClient.WhereCache(p).List(ctx, &found)
	return found, err
}

// GetLogicalRouterPolicy looks up a logical router policy from the cache
func GetLogicalRouterPolicy(nbClient libovsdbclient.Client, policy *nbdb.LogicalRouterPolicy) (*nbdb.LogicalRouterPolicy, error) {
	found := []*nbdb.LogicalRouterPolicy{}
	opModel := operationModel{
		Model:          policy,
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

// CreateOrUpdateLogicalRouterPolicyWithPredicate looks up a logical router
// policy from the cache based on a given predicate. If it does not exist, it
// creates the provided logical router policy. If it does, it updates it. The
// logical router policy is added to the provided logical router.
// fields determines which columns to updated. Passing no fields is assumes
// all fields need to be updated. Passing a single nil field indicates no fields should be updated.
// Otherwise a caller may pass as many individual fields as desired to specify which columsn need updating.
func CreateOrUpdateLogicalRouterPolicyWithPredicate(nbClient libovsdbclient.Client, routerName string, lrp *nbdb.LogicalRouterPolicy, p logicalRouterPolicyPredicate, fields ...interface{}) error {
	ops, err := CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient, nil, routerName, lrp, p, fields...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// CreateOrUpdateLogicalRouterPolicyWithPredicateOps looks up a logical
// router policy from the cache based on a given predicate. If it does not
// exist, it creates the provided logical router policy. If it does, it
// updates it. The logical router policy is added to the provided logical
// router. Returns the corresponding ops
func CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation,
	routerName string, lrp *nbdb.LogicalRouterPolicy, p logicalRouterPolicyPredicate, fields ...interface{}) ([]libovsdb.Operation, error) {
	if len(fields) == 0 {
		fields = onModelUpdatesAllNonDefault()
	}
	router := &nbdb.LogicalRouter{
		Name: routerName,
	}

	opModels := []operationModel{
		{
			Model:          lrp,
			ModelPredicate: p,
			OnModelUpdates: fields,
			DoAfter:        func() { router.Policies = []string{lrp.UUID} },
			ErrNotFound:    false,
			BulkOp:         false,
		},
		{
			Model:            router,
			OnModelMutations: []interface{}{&router.Policies},
			ErrNotFound:      true,
			BulkOp:           false,
		},
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModels...)
}

func UpdateLogicalRouterPoliciesOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation,
	lrps ...*nbdb.LogicalRouterPolicy) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(lrps))
	for i := range lrps {
		lrp := lrps[i]
		opModel := []operationModel{
			{
				Model:          lrp,
				OnModelUpdates: onModelUpdatesAllNonDefault(),
				ErrNotFound:    true,
				BulkOp:         false,
			},
		}
		opModels = append(opModels, opModel...)
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModels...)
}

// DeleteLogicalRouterPolicyWithPredicateOps looks up a logical
// router policy from the cache based on a given predicate and returns the
// corresponding ops to delete it and remove it from the provided router.
func DeleteLogicalRouterPolicyWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, routerName string, p logicalRouterPolicyPredicate) ([]libovsdb.Operation, error) {
	router := &nbdb.LogicalRouter{
		Name: routerName,
	}

	deleted := []*nbdb.LogicalRouterPolicy{}
	opModels := []operationModel{
		{
			ModelPredicate: p,
			ExistingResult: &deleted,
			DoAfter:        func() { router.Policies = extractUUIDsFromModels(&deleted) },
			ErrNotFound:    false,
			BulkOp:         true,
		},
		{
			Model:            router,
			OnModelMutations: []interface{}{&router.Policies},
			ErrNotFound:      false,
			BulkOp:           false,
		},
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModels...)
}

// DeleteLogicalRouterPoliciesWithPredicate looks up logical router policies
// from the cache based on a given predicate, deletes them and removes them from
// the provided logical router
func DeleteLogicalRouterPoliciesWithPredicate(nbClient libovsdbclient.Client, routerName string, p logicalRouterPolicyPredicate) error {
	ops, err := DeleteLogicalRouterPolicyWithPredicateOps(nbClient, nil, routerName, p)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// CreateOrAddNextHopsToLogicalRouterPolicyWithPredicateOps looks up a logical
// router policy from the cache based on a given predicate. If it doesn't find
// any, it creates the provided logical router policy. If it does, adds any
// missing Nexthops to the existing logical router policy. The logical router
// policy is added to the provided logical router. Returns the corresponding ops
func CreateOrAddNextHopsToLogicalRouterPolicyWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, routerName string, lrp *nbdb.LogicalRouterPolicy, p logicalRouterPolicyPredicate) ([]libovsdb.Operation, error) {
	router := &nbdb.LogicalRouter{
		Name: routerName,
	}

	opModels := []operationModel{
		{
			Model:            lrp,
			ModelPredicate:   p,
			OnModelMutations: []interface{}{&lrp.Nexthops},
			DoAfter:          func() { router.Policies = []string{lrp.UUID} },
			ErrNotFound:      false,
			BulkOp:           false,
		},
		{
			Model:            router,
			OnModelMutations: []interface{}{&router.Policies},
			ErrNotFound:      true,
			BulkOp:           false,
		},
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModels...)
}

// DeleteNextHopsFromLogicalRouterPolicyOps removes the Nexthops from the
// provided logical router policies.
func DeleteNextHopsFromLogicalRouterPolicyOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, routerName string, lrps []*nbdb.LogicalRouterPolicy, nextHops ...string) ([]libovsdb.Operation, error) {
	nextHopSet := sets.NewString(nextHops...)
	opModels := []operationModel{}
	router := &nbdb.LogicalRouter{
		Name:     routerName,
		Policies: []string{},
	}

	for i := range lrps {
		lrp := lrps[i]
		if nextHopSet.HasAll(lrp.Nexthops...) {
			// if no next-hops remain in the policy, remove it alltogether
			router.Policies = append(router.Policies, lrp.UUID)
			opModel := operationModel{
				Model:       lrp,
				BulkOp:      false,
				ErrNotFound: false,
			}
			opModels = append(opModels, opModel)
		} else {
			// otherwise just remove the next-hops
			lrp.Nexthops = nextHops
			opModel := operationModel{
				Model:            lrp,
				OnModelMutations: []interface{}{&lrp.Nexthops},
				BulkOp:           false,
				ErrNotFound:      false,
			}
			opModels = append(opModels, opModel)
		}
	}

	if len(router.Policies) > 0 {
		opModel := operationModel{
			Model:            router,
			OnModelMutations: []interface{}{&router.Policies},
			BulkOp:           false,
			ErrNotFound:      false,
		}
		opModels = append(opModels, opModel)
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModels...)
}

// DeleteNextHopsFromLogicalRouterPolicies removes the Nexthops from the
// provided logical router policies. If a logical router policy ends up with no
// Nexthops, it is deleted and removed from the provided logical router.
func DeleteNextHopsFromLogicalRouterPolicies(nbClient libovsdbclient.Client, routerName string, lrps ...*nbdb.LogicalRouterPolicy) error {
	ops := []libovsdb.Operation{}
	for _, lrp := range lrps {
		nextHops := lrp.Nexthops
		lrp, err := GetLogicalRouterPolicy(nbClient, lrp)
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}

		ops, err = DeleteNextHopsFromLogicalRouterPolicyOps(nbClient, ops, routerName, []*nbdb.LogicalRouterPolicy{lrp}, nextHops...)
		if err != nil {
			return err
		}
	}

	_, err := TransactAndCheck(nbClient, ops)
	return err
}

// DeleteNextHopFromLogicalRouterPoliciesWithPredicateOps looks up a logical
// router policy from the cache based on a given predicate and removes the
// provided Nexthop from it. If the logical router policy ends up with no
// Nexthops, it is deleted and removed from the provided logical router. Returns
// the corresponding ops
func DeleteNextHopFromLogicalRouterPoliciesWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, routerName string, p logicalRouterPolicyPredicate, nextHop string) ([]libovsdb.Operation, error) {
	lrps, err := FindLogicalRouterPoliciesWithPredicate(nbClient, p)
	if err != nil {
		return nil, err
	}

	return DeleteNextHopsFromLogicalRouterPolicyOps(nbClient, ops, routerName, lrps, nextHop)
}

// DeleteNextHopFromLogicalRouterPoliciesWithPredicate looks up a logical router
// policy from the cache based on a given predicate and removes the provided
// Nexthop from it. If the logical router policy ends up with no Nexthops, it is
// deleted and removed from the provided logical router.
func DeleteNextHopFromLogicalRouterPoliciesWithPredicate(nbClient libovsdbclient.Client, routerName string, p logicalRouterPolicyPredicate, nextHop string) error {
	ops, err := DeleteNextHopFromLogicalRouterPoliciesWithPredicateOps(nbClient, nil, routerName, p, nextHop)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// DeleteLogicalRouterPolicies deletes the logical router policies and removes
// them from the provided logical router
func DeleteLogicalRouterPolicies(nbClient libovsdbclient.Client, routerName string, lrps ...*nbdb.LogicalRouterPolicy) error {
	opModels := getDeleteOpModelsForLogicalRouterPolicies(routerName, lrps...)

	m := newModelClient(nbClient)
	return m.Delete(opModels...)
}

// DeleteLogicalRouterPoliciesOps builds and returns corresponding delete operations for Logical Router
// Policies from the provided logical router.
func DeleteLogicalRouterPoliciesOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, routerName string, lrps ...*nbdb.LogicalRouterPolicy) ([]libovsdb.Operation, error) {
	opModels := getDeleteOpModelsForLogicalRouterPolicies(routerName, lrps...)

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModels...)
}

func getDeleteOpModelsForLogicalRouterPolicies(routerName string, lrps ...*nbdb.LogicalRouterPolicy) []operationModel {
	router := &nbdb.LogicalRouter{
		Name:     routerName,
		Policies: make([]string, 0, len(lrps)),
	}

	opModels := make([]operationModel, 0, len(lrps)+1)
	for _, lrp := range lrps {
		router.Policies = append(router.Policies, lrp.UUID)
		opModel := operationModel{
			Model:       lrp,
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels = append(opModels, opModel)
	}

	opModel := operationModel{
		Model:            router,
		OnModelMutations: []interface{}{&router.Policies},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	return append(opModels, opModel)
}

// LOGICAL ROUTER STATIC ROUTES

type logicalRouterStaticRoutePredicate func(*nbdb.LogicalRouterStaticRoute) bool

// FindLogicalRouterStaticRoutesWithPredicate looks up logical router static
// routes from the cache based on a given predicate
func FindLogicalRouterStaticRoutesWithPredicate(nbClient libovsdbclient.Client, p logicalRouterStaticRoutePredicate) ([]*nbdb.LogicalRouterStaticRoute, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	found := []*nbdb.LogicalRouterStaticRoute{}
	err := nbClient.WhereCache(p).List(ctx, &found)
	return found, err
}

// CreateOrUpdateLogicalRouterStaticRoutesWithPredicateOps looks up a logical
// router static route from the cache based on a given predicate. If it does not
// exist, it creates the provided logical router static route. If it does, it
// updates it. The logical router static route is added to the provided logical
// router. Returns the corresponding ops
func CreateOrUpdateLogicalRouterStaticRoutesWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation,
	routerName string, lrsr *nbdb.LogicalRouterStaticRoute, p logicalRouterStaticRoutePredicate, fields ...interface{}) ([]libovsdb.Operation, error) {
	if len(fields) == 0 {
		fields = onModelUpdatesAllNonDefault()
	}
	router := &nbdb.LogicalRouter{
		Name: routerName,
	}

	opModels := []operationModel{
		{
			Model:          lrsr,
			OnModelUpdates: fields,
			DoAfter:        func() { router.StaticRoutes = []string{lrsr.UUID} },
			ErrNotFound:    false,
			BulkOp:         false,
		},
		{
			Model:            router,
			OnModelMutations: []interface{}{&router.StaticRoutes},
			ErrNotFound:      true,
			BulkOp:           false,
		},
	}

	if p != nil {
		opModels[0].ModelPredicate = p
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModels...)
}

// PolicyEqualPredicate determines if two static routes have the same routing policy (dst-ip or src-ip)
// If policy is nil, OVN considers that as dst-ip
func PolicyEqualPredicate(p1, p2 *nbdb.LogicalRouterStaticRoutePolicy) bool {
	if p1 == nil {
		return p2 == nil || (p2 != nil && *p2 == nbdb.LogicalRouterStaticRoutePolicyDstIP)
	}

	if p2 == nil {
		return *p1 == nbdb.LogicalRouterStaticRoutePolicyDstIP
	}

	return *p1 == *p2
}

// CreateOrReplaceLogicalRouterStaticRouteWithPredicate looks up a logical
// router static route from the cache based on a given predicate. If it does not
// exist, it creates the provided logical router static route. If it does, it
// updates it. The logical router static route is added to the provided logical
// router.
// If more than one route matches the predicate on the router, the additional routes are removed.
func CreateOrReplaceLogicalRouterStaticRouteWithPredicate(nbClient libovsdbclient.Client, routerName string,
	lrsr *nbdb.LogicalRouterStaticRoute, p logicalRouterStaticRoutePredicate, fields ...interface{}) error {

	lr := &nbdb.LogicalRouter{Name: routerName}
	router, err := GetLogicalRouter(nbClient, lr)
	if err != nil {
		return fmt.Errorf("unable to get logical router %s: %w", routerName, err)
	}
	newPredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
		for _, routeUUID := range router.StaticRoutes {
			if routeUUID == item.UUID && p(item) {
				return true
			}
		}
		return false
	}
	routes, err := FindLogicalRouterStaticRoutesWithPredicate(nbClient, newPredicate)
	if err != nil {
		return fmt.Errorf("unable to get logical router static routes with predicate on router %s: %w", routerName, err)
	}

	var ops []libovsdb.Operation
	m := newModelClient(nbClient)

	if len(routes) > 0 {
		lrsr.UUID = routes[0].UUID
	}

	if len(routes) > 1 {
		// should only be a single route remove all except the first
		routes = routes[1:]
		opModels := make([]operationModel, 0, len(routes)+1)
		router.StaticRoutes = []string{}
		for _, route := range routes {
			route := route
			router.StaticRoutes = append(router.StaticRoutes, route.UUID)
			opModel := operationModel{
				Model:       route,
				ErrNotFound: false,
				BulkOp:      false,
			}
			opModels = append(opModels, opModel)
		}
		opModel := operationModel{
			Model:            router,
			OnModelMutations: []interface{}{&router.StaticRoutes},
			ErrNotFound:      true,
			BulkOp:           false,
		}
		opModels = append(opModels, opModel)

		ops, err = m.DeleteOps(nil, opModels...)
		if err != nil {
			return err
		}
	}

	ops, err = CreateOrUpdateLogicalRouterStaticRoutesWithPredicateOps(nbClient, ops, routerName, lrsr, nil, fields...)
	if err != nil {
		return fmt.Errorf("unable to get create or update logical router static routes on router %s: %w", routerName, err)
	}
	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// DeleteLogicalRouterStaticRoutesWithPredicate looks up logical router static
// routes from the cache based on a given predicate, deletes them and removes
// them from the provided logical router
func DeleteLogicalRouterStaticRoutesWithPredicate(nbClient libovsdbclient.Client, routerName string, p logicalRouterStaticRoutePredicate) error {
	var ops []libovsdb.Operation
	var err error
	ops, err = DeleteLogicalRouterStaticRoutesWithPredicateOps(nbClient, ops, routerName, p)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// DeleteLogicalRouterStaticRoutesWithPredicateOps looks up logical router static
// routes from the cache based on a given predicate, and returns the ops to delete
// them and remove them from the provided logical router
func DeleteLogicalRouterStaticRoutesWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, routerName string, p logicalRouterStaticRoutePredicate) ([]libovsdb.Operation, error) {
	router := &nbdb.LogicalRouter{
		Name: routerName,
	}

	deleted := []*nbdb.LogicalRouterStaticRoute{}
	opModels := []operationModel{
		{
			ModelPredicate: p,
			ExistingResult: &deleted,
			DoAfter:        func() { router.StaticRoutes = extractUUIDsFromModels(deleted) },
			ErrNotFound:    false,
			BulkOp:         true,
		},
		{
			Model:            router,
			OnModelMutations: []interface{}{&router.StaticRoutes},
			ErrNotFound:      false,
			BulkOp:           false,
		},
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModels...)
}

// DeleteLogicalRouterStaticRoutes deletes the logical router static routes and
// removes them from the provided logical router
func DeleteLogicalRouterStaticRoutes(nbClient libovsdbclient.Client, routerName string, lrsrs ...*nbdb.LogicalRouterStaticRoute) error {
	router := &nbdb.LogicalRouter{
		Name:         routerName,
		StaticRoutes: make([]string, 0, len(lrsrs)),
	}

	opModels := make([]operationModel, 0, len(lrsrs)+1)
	for _, lrsr := range lrsrs {
		lrsr := lrsr
		router.StaticRoutes = append(router.StaticRoutes, lrsr.UUID)
		opModel := operationModel{
			Model:       lrsr,
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels = append(opModels, opModel)
	}

	opModel := operationModel{
		Model:            router,
		OnModelMutations: []interface{}{&router.StaticRoutes},
		ErrNotFound:      true,
		BulkOp:           false,
	}
	opModels = append(opModels, opModel)

	m := newModelClient(nbClient)
	return m.Delete(opModels...)
}

// BFD ops

// CreateOrUpdateBFDOps creates or updates the provided BFDs and returns
// the corresponding ops
func CreateOrUpdateBFDOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, bfds ...*nbdb.BFD) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(bfds))
	for i := range bfds {
		bfd := bfds[i]
		opModel := operationModel{
			Model:          bfd,
			OnModelUpdates: onModelUpdatesAllNonDefault(),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModels...)
}

// DeleteBFDs deletes the provided BFDs
func DeleteBFDs(nbClient libovsdbclient.Client, bfds ...*nbdb.BFD) error {
	opModels := make([]operationModel, 0, len(bfds))
	for i := range bfds {
		bfd := bfds[i]
		opModel := operationModel{
			Model:       bfd,
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels = append(opModels, opModel)
	}

	m := newModelClient(nbClient)
	return m.Delete(opModels...)
}

func LookupBFD(nbClient libovsdbclient.Client, bfd *nbdb.BFD) (*nbdb.BFD, error) {
	found := []*nbdb.BFD{}
	opModel := operationModel{
		Model:          bfd,
		ModelPredicate: func(item *nbdb.BFD) bool { return item.DstIP == bfd.DstIP && item.LogicalPort == bfd.LogicalPort },
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

// LB OPs

// AddLoadBalancersToLogicalRouterOps adds the provided load balancers to the
// provided logical router and returns the corresponding ops
func AddLoadBalancersToLogicalRouterOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	originalLBs := router.LoadBalancer
	router.LoadBalancer = make([]string, 0, len(lbs))
	for _, lb := range lbs {
		router.LoadBalancer = append(router.LoadBalancer, lb.UUID)
	}
	opModel := operationModel{
		Model:            router,
		OnModelMutations: []interface{}{&router.LoadBalancer},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	modelClient := newModelClient(nbClient)
	ops, err := modelClient.CreateOrUpdateOps(ops, opModel)
	router.LoadBalancer = originalLBs
	return ops, err
}

// RemoveLoadBalancersFromLogicalRouterOps removes the provided load balancers from the
// provided logical router and returns the corresponding ops
func RemoveLoadBalancersFromLogicalRouterOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	originalLBs := router.LoadBalancer
	router.LoadBalancer = make([]string, 0, len(lbs))
	for _, lb := range lbs {
		router.LoadBalancer = append(router.LoadBalancer, lb.UUID)
	}
	opModel := operationModel{
		Model:            router,
		OnModelMutations: []interface{}{&router.LoadBalancer},
		// if we want to delete loadbalancer from the router that doesn't exist, that is noop
		ErrNotFound: false,
		BulkOp:      false,
	}

	modelClient := newModelClient(nbClient)
	ops, err := modelClient.DeleteOps(ops, opModel)
	router.LoadBalancer = originalLBs
	return ops, err
}

func buildNAT(
	natType nbdb.NATType,
	externalIP string,
	logicalIP string,
	logicalPort string,
	externalMac string,
	externalIDs map[string]string,
	match string,
) *nbdb.NAT {
	nat := &nbdb.NAT{
		Type:        natType,
		ExternalIP:  externalIP,
		LogicalIP:   logicalIP,
		Options:     map[string]string{"stateless": "false"},
		ExternalIDs: externalIDs,
		Match:       match,
	}

	if logicalPort != "" {
		nat.LogicalPort = &logicalPort
	}

	if externalMac != "" {
		nat.ExternalMAC = &externalMac
	}

	return nat
}

// BuildSNAT builds a logical router SNAT
func BuildSNAT(
	externalIP *net.IP,
	logicalIP *net.IPNet,
	logicalPort string,
	externalIDs map[string]string,
) *nbdb.NAT {
	return BuildSNATWithMatch(externalIP, logicalIP, logicalPort, externalIDs, "")
}

func BuildSNATWithMatch(
	externalIP *net.IP,
	logicalIP *net.IPNet,
	logicalPort string,
	externalIDs map[string]string,
	match string,
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
	return buildNAT(nbdb.NATTypeSNAT, externalIPStr, logicalIPStr, logicalPort, "", externalIDs, match)
}

// BuildDNATAndSNAT builds a logical router DNAT/SNAT
func BuildDNATAndSNAT(
	externalIP *net.IP,
	logicalIP *net.IPNet,
	logicalPort string,
	externalMac string,
	externalIDs map[string]string,
) *nbdb.NAT {
	return BuildDNATAndSNATWithMatch(externalIP, logicalIP, logicalPort, externalMac, externalIDs, "")
}

func BuildDNATAndSNATWithMatch(
	externalIP *net.IP,
	logicalIP *net.IPNet,
	logicalPort string,
	externalMac string,
	externalIDs map[string]string,
	match string,
) *nbdb.NAT {
	externalIPStr := ""
	if externalIP != nil {
		externalIPStr = externalIP.String()
	}
	logicalIPStr := ""
	if logicalIP != nil {
		logicalIPStr = logicalIP.IP.String()
	}
	return buildNAT(
		nbdb.NATTypeDNATAndSNAT,
		externalIPStr,
		logicalIPStr,
		logicalPort,
		externalMac,
		externalIDs,
		match)
}

// isEquivalentNAT checks if the `searched` NAT is equivalent to `existing`.
// Returns true if the UUID is set in `searched` and matches the UUID of `existing`.
// Otherwise, perform the following checks:
//   - Compare the Type and Match fields.
//   - Compare ExternalIP if it is set in `searched`.
//   - Compare LogicalIP if the Type in `searched` is SNAT.
//   - Compare LogicalPort if it is set in `searched`.
//   - Ensure that all ExternalIDs of `searched` exist and have the same value in `existing`.
func isEquivalentNAT(existing *nbdb.NAT, searched *nbdb.NAT) bool {
	// Simple case: uuid was provided.
	if searched.UUID != "" && existing.UUID == searched.UUID {
		return true
	}

	if searched.Type != existing.Type {
		return false
	}

	if searched.Match != existing.Match {
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

type natPredicate func(*nbdb.NAT) bool

// GetNAT looks up an NAT from the cache
func GetNAT(nbClient libovsdbclient.Client, nat *nbdb.NAT) (*nbdb.NAT, error) {
	found := []*nbdb.NAT{}
	opModel := operationModel{
		Model:          nat,
		ModelPredicate: func(item *nbdb.NAT) bool { return isEquivalentNAT(item, nat) },
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

// FindNATsWithPredicate looks up NATs from the cache based on a given predicate
func FindNATsWithPredicate(nbClient libovsdbclient.Client, predicate natPredicate) ([]*nbdb.NAT, error) {
	nats := []*nbdb.NAT{}
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	err := nbClient.WhereCache(predicate).List(ctx, &nats)
	return nats, err
}

// GetRouterNATs looks up NATs associated to the provided logical router from
// the cache
func GetRouterNATs(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter) ([]*nbdb.NAT, error) {
	r, err := GetLogicalRouter(nbClient, router)
	if err != nil {
		return nil, fmt.Errorf("failed to get router: %s, error: %w", router.Name, err)
	}

	nats := []*nbdb.NAT{}
	for _, uuid := range r.Nat {
		nat, err := GetNAT(nbClient, &nbdb.NAT{UUID: uuid})
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to lookup NAT entry with uuid: %s, error: %w", uuid, err)
		}
		nats = append(nats, nat)
	}

	return nats, nil
}

// CreateOrUpdateNATsOps creates or updates the provided NATs, adds them to
// the provided logical router and returns the corresponding ops
func CreateOrUpdateNATsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, nats ...*nbdb.NAT) ([]libovsdb.Operation, error) {
	routerNats, err := GetRouterNATs(nbClient, router)
	if err != nil {
		return ops, fmt.Errorf("unable to get NAT entries for router %+v: %w", router, err)
	}

	originalNats := router.Nat
	router.Nat = make([]string, 0, len(nats))
	opModels := make([]operationModel, 0, len(nats)+1)
	for i := range nats {
		inputNat := nats[i]
		for _, routerNat := range routerNats {
			if isEquivalentNAT(routerNat, inputNat) {
				inputNat.UUID = routerNat.UUID
				break
			}
		}
		opModel := operationModel{
			Model:          inputNat,
			OnModelUpdates: onModelUpdatesAllNonDefault(),
			ErrNotFound:    false,
			BulkOp:         false,
			DoAfter:        func() { router.Nat = append(router.Nat, inputNat.UUID) },
		}
		opModels = append(opModels, opModel)
	}
	opModel := operationModel{
		Model:            router,
		OnModelMutations: []interface{}{&router.Nat},
		ErrNotFound:      true,
		BulkOp:           false,
	}
	opModels = append(opModels, opModel)

	m := newModelClient(nbClient)
	ops, err = m.CreateOrUpdateOps(ops, opModels...)
	router.Nat = originalNats
	return ops, err
}

// CreateOrUpdateNATs creates or updates the provided NATs and adds them to
// the provided logical router
func CreateOrUpdateNATs(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter, nats ...*nbdb.NAT) error {
	ops, err := CreateOrUpdateNATsOps(nbClient, nil, router, nats...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// DeleteNATsOps deletes the provided NATs, removes them from the provided
// logical router and returns the corresponding ops
func DeleteNATsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, nats ...*nbdb.NAT) ([]libovsdb.Operation, error) {
	routerNats, err := GetRouterNATs(nbClient, router)
	if errors.Is(err, libovsdbclient.ErrNotFound) {
		return ops, nil
	}
	if err != nil {
		return ops, fmt.Errorf("unable to get NAT entries for router %+v: %w", router, err)
	}

	originalNats := router.Nat
	router.Nat = make([]string, 0, len(nats))
	opModels := make([]operationModel, 0, len(routerNats)+1)
	for _, routerNat := range routerNats {
		for _, inputNat := range nats {
			if isEquivalentNAT(routerNat, inputNat) {
				router.Nat = append(router.Nat, routerNat.UUID)
				opModel := operationModel{
					Model:       routerNat,
					ErrNotFound: false,
					BulkOp:      false,
				}
				opModels = append(opModels, opModel)
				break
			}
		}
	}
	if len(router.Nat) == 0 {
		return ops, nil
	}
	opModel := operationModel{
		Model:            router,
		OnModelMutations: []interface{}{&router.Nat},
		ErrNotFound:      false,
		BulkOp:           false,
	}
	opModels = append(opModels, opModel)

	m := newModelClient(nbClient)
	ops, err = m.DeleteOps(ops, opModels...)
	router.Nat = originalNats
	return ops, err
}

// DeleteNATs deletes the provided NATs and removes them from the provided
// logical router
func DeleteNATs(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter, nats ...*nbdb.NAT) error {
	ops, err := DeleteNATsOps(nbClient, nil, router, nats...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// DeleteNATsWithPredicateOps looks up NATs from the cache based on a given
// predicate, deletes them, removes them from associated logical routers and
// returns the corresponding ops
func DeleteNATsWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, p natPredicate) ([]libovsdb.Operation, error) {
	deleted := []*nbdb.NAT{}
	router := &nbdb.LogicalRouter{}
	natUUIDs := sets.Set[string]{}
	opModels := []operationModel{
		{
			ModelPredicate: p,
			ExistingResult: &deleted,
			DoAfter: func() {
				router.Nat = extractUUIDsFromModels(&deleted)
				natUUIDs.Insert(router.Nat...)
			},
			BulkOp:      true,
			ErrNotFound: false,
		},
		{
			Model:            router,
			ModelPredicate:   func(lr *nbdb.LogicalRouter) bool { return natUUIDs.HasAny(lr.Nat...) },
			OnModelMutations: []interface{}{&router.Nat},
			ErrNotFound:      false,
			BulkOp:           true,
		},
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModels...)
}

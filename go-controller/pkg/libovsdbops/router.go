package libovsdbops

import (
	"context"
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// ROUTER OPs

type logicalRouterPredicate func(*nbdb.LogicalRouter) bool

func GetLogicalRouter(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter) (*nbdb.LogicalRouter, error) {
	found := []*nbdb.LogicalRouter{}
	opModel := OperationModel{
		Model:          router,
		ModelPredicate: func(item *nbdb.LogicalRouter) bool { return item.Name == router.Name },
		ExistingResult: &found,
		OnModelUpdates: nil, // no update
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := NewModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModel)
	if err != nil {
		return nil, err
	}

	return found[0], nil
}

func FindLogicalRoutersWithPredicate(nbClient libovsdbclient.Client, p logicalRouterPredicate) ([]*nbdb.LogicalRouter, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	found := []*nbdb.LogicalRouter{}
	err := nbClient.WhereCache(p).List(ctx, &found)
	return found, err
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
	opModel := OperationModel{
		Name:             router.Name,
		Model:            router,
		ModelPredicate:   func(item *nbdb.LogicalRouter) bool { return item.Name == router.Name },
		OnModelMutations: []interface{}{&router.LoadBalancer},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	modelClient := NewModelClient(nbClient)
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
	opModel := OperationModel{
		Name:             router.Name,
		Model:            router,
		ModelPredicate:   func(item *nbdb.LogicalRouter) bool { return item.Name == router.Name },
		OnModelMutations: []interface{}{&router.LoadBalancer},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	modelClient := NewModelClient(nbClient)
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

// BuildSNAT builds a logical router SNAT
func BuildSNAT(
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
	return buildNAT(nbdb.NATTypeSNAT, externalIPStr, logicalIPStr, logicalPort, "", externalIDs)
}

// BuildLogicalRouterSNAT builds a logical router DNAT/SNAT
func BuildDNATAndSNAT(
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
	return buildNAT(
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

type natPredicate func(*nbdb.NAT) bool

// GetNAT looks up an NAT from the cache
func GetNAT(nbClient libovsdbclient.Client, nat *nbdb.NAT) (*nbdb.NAT, error) {
	found := []*nbdb.NAT{}
	opModel := OperationModel{
		Model:          nat,
		ModelPredicate: func(item *nbdb.NAT) bool { return isEquivalentNAT(item, nat) },
		ExistingResult: &found,
		OnModelUpdates: nil, // no update
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := NewModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModel)
	if err != nil {
		return nil, err
	}

	return found[0], nil
}

// FindNATsWithPredicate looks up NATs from the cache based on a given predicate
func FindNATsWithPredicate(nbClient libovsdbclient.Client, predicate natPredicate) ([]*nbdb.NAT, error) {
	nats := []*nbdb.NAT{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereCache(predicate).List(ctx, &nats)
	return nats, err
}

// getRouterNATs looks up NATs associated to the provided logical router from
// the cache
func getRouterNATs(nbClient libovsdbclient.Client, router *nbdb.LogicalRouter) ([]*nbdb.NAT, error) {
	router, err := GetLogicalRouter(nbClient, router)
	if err != nil {
		return nil, err
	}

	nats := []*nbdb.NAT{}
	for _, uuid := range router.Nat {
		nat, err := GetNAT(nbClient, &nbdb.NAT{UUID: uuid})
		if err != nil {
			return nil, err
		}
		nats = append(nats, nat)
	}

	return nats, nil
}

// CreateOrUpdateNATsOps creates or updates the provided NATs, adds them to
// the provided logical router and returns the corresponding ops
func CreateOrUpdateNATsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, router *nbdb.LogicalRouter, nats ...*nbdb.NAT) ([]libovsdb.Operation, error) {
	routerNats, err := getRouterNATs(nbClient, router)
	if err != nil {
		return ops, fmt.Errorf("unable to get NAT entries for router %+v: %w", router, err)
	}

	originalNats := router.Nat
	router.Nat = make([]string, 0, len(nats))
	opModels := make([]OperationModel, 0, len(nats)+1)
	for i := range nats {
		inputNat := nats[i]
		for _, routerNat := range routerNats {
			if isEquivalentNAT(routerNat, inputNat) {
				inputNat.UUID = routerNat.UUID
				break
			}
		}
		opModel := OperationModel{
			Model:          inputNat,
			OnModelUpdates: onModelUpdatesAll(),
			ErrNotFound:    false,
			BulkOp:         false,
			DoAfter:        func() { router.Nat = append(router.Nat, inputNat.UUID) },
		}
		opModels = append(opModels, opModel)
	}
	opModel := OperationModel{
		Model:            router,
		OnModelMutations: []interface{}{&router.Nat},
		ErrNotFound:      true,
		BulkOp:           false,
	}
	opModels = append(opModels, opModel)

	m := NewModelClient(nbClient)
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
	routerNats, err := getRouterNATs(nbClient, router)
	if err != nil {
		return ops, fmt.Errorf("unable to get NAT entries for router %+v: %w", router, err)
	}

	originalNats := router.Nat
	router.Nat = make([]string, 0, len(nats))
	opModels := make([]OperationModel, 0, len(routerNats)+1)
	for _, routerNat := range routerNats {
		for _, inputNat := range nats {
			if isEquivalentNAT(routerNat, inputNat) {
				router.Nat = append(router.Nat, routerNat.UUID)
				opModel := OperationModel{
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
	opModel := OperationModel{
		Model:            router,
		OnModelMutations: []interface{}{&router.Nat},
		ErrNotFound:      true,
		BulkOp:           false,
	}
	opModels = append(opModels, opModel)

	m := NewModelClient(nbClient)
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
// predicate, deletes them, removes them from the provided logical router and
// returns the corrsponding ops
func DeleteNATsWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, routerName string, p natPredicate) ([]libovsdb.Operation, error) {
	router := &nbdb.LogicalRouter{
		Name: routerName,
	}

	deleted := []*nbdb.NAT{}
	opModels := []OperationModel{
		{
			ModelPredicate: p,
			ExistingResult: &deleted,
			DoAfter:        func() { router.Nat = ExtractUUIDsFromModels(&deleted) },
			BulkOp:         true,
		},
		{
			Model:            router,
			ModelPredicate:   func(lr *nbdb.LogicalRouter) bool { return lr.Name == routerName },
			OnModelMutations: []interface{}{&router.Nat},
			ErrNotFound:      true,
			BulkOp:           false,
		},
	}

	m := NewModelClient(nbClient)
	return m.DeleteOps(ops, opModels...)
}

// GATEWAY CHASSIS OPs

// CreateOrUpdateGatewayChassis creates or updates the provided gateway chassis
// and sets it to the provided logical router port
func CreateOrUpdateGatewayChassis(nbClient libovsdbclient.Client, port *nbdb.LogicalRouterPort, chassis *nbdb.GatewayChassis) error {
	opModels := []OperationModel{
		{
			Model:          chassis,
			OnModelUpdates: onModelUpdatesAll(),
			DoAfter:        func() { port.GatewayChassis = []string{chassis.UUID} },
			ErrNotFound:    false,
			BulkOp:         false,
		},
		{
			Model: port,
			// use update here, as of now we only ever want a single chassis
			// associated with a port
			OnModelUpdates: []interface{}{&port.GatewayChassis},
			ErrNotFound:    true,
			BulkOp:         false,
		},
	}

	modelClient := NewModelClient(nbClient)
	_, err := modelClient.CreateOrUpdate(opModels...)
	return err
}

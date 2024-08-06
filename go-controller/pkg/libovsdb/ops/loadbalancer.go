package ops

import (
	"context"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// getNonZeroLoadBalancerMutableFields builds a list of load balancer
// mutable fields with non zero values to be used as the list of fields to
// Update.
// The purpose is to prevent libovsdb interpreting non-nil empty maps/slices
// as default and thus being filtered out of the update. The intention is to
// use non-nil empty maps/slices to clear them out in the update.
// See: https://github.com/ovn-org/libovsdb/issues/226
func getNonZeroLoadBalancerMutableFields(lb *nbdb.LoadBalancer) []interface{} {
	fields := []interface{}{}
	if lb.Name != "" {
		fields = append(fields, &lb.Name)
	}
	if lb.ExternalIDs != nil {
		fields = append(fields, &lb.ExternalIDs)
	}
	if lb.HealthCheck != nil {
		fields = append(fields, &lb.HealthCheck)
	}
	if lb.IPPortMappings != nil {
		fields = append(fields, &lb.IPPortMappings)
	}
	if lb.Options != nil {
		fields = append(fields, &lb.Options)
	}
	if lb.Protocol != nil {
		fields = append(fields, &lb.Protocol)
	}
	if lb.SelectionFields != nil {
		fields = append(fields, &lb.SelectionFields)
	}
	if lb.Vips != nil {
		fields = append(fields, &lb.Vips)
	}
	return fields
}

// BuildLoadBalancer builds a load balancer
func BuildLoadBalancer(name string, protocol nbdb.LoadBalancerProtocol, selectionFields []nbdb.LoadBalancerSelectionFields, vips, options, externalIds map[string]string) *nbdb.LoadBalancer {
	return &nbdb.LoadBalancer{
		Name:            name,
		Protocol:        &protocol,
		Vips:            vips,
		SelectionFields: selectionFields,
		Options:         options,
		ExternalIDs:     externalIds,
	}
}

// CreateOrUpdateLoadBalancersOps creates or updates the provided load balancers
// returning the corresponding ops
func CreateOrUpdateLoadBalancersOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(lbs))
	for i := range lbs {
		// can't use i in the predicate, for loop replaces it in-memory
		lb := lbs[i]
		opModel := operationModel{
			Model:          lb,
			OnModelUpdates: getNonZeroLoadBalancerMutableFields(lb),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}

// RemoveLoadBalancerVipsOps removes the provided VIPs from the provided load
// balancer set and returns the corresponding ops
func RemoveLoadBalancerVipsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, lb *nbdb.LoadBalancer, vips ...string) ([]libovsdb.Operation, error) {
	originalVips := lb.Vips
	lb.Vips = make(map[string]string, len(vips))
	for _, vip := range vips {
		lb.Vips[vip] = ""
	}
	opModel := operationModel{
		Model:            lb,
		OnModelMutations: []interface{}{&lb.Vips},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	modelClient := newModelClient(nbClient)
	ops, err := modelClient.DeleteOps(ops, opModel)
	lb.Vips = originalVips
	return ops, err
}

// DeleteLoadBalancersOps deletes the provided load balancers and returns the
// corresponding ops
func DeleteLoadBalancersOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(lbs))
	for i := range lbs {
		// can't use i in the predicate, for loop replaces it in-memory
		lb := lbs[i]
		opModel := operationModel{
			Model:       lb,
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.DeleteOps(ops, opModels...)
}

// DeleteLoadBalancers deletes the provided load balancers
func DeleteLoadBalancers(nbClient libovsdbclient.Client, lbs []*nbdb.LoadBalancer) error {
	ops, err := DeleteLoadBalancersOps(nbClient, nil, lbs...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// ListLoadBalancers looks up all load balancers from the cache
func ListLoadBalancers(nbClient libovsdbclient.Client) ([]*nbdb.LoadBalancer, error) {
	lbs := []*nbdb.LoadBalancer{}
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	err := nbClient.List(ctx, &lbs)
	return lbs, err
}

type loadBalancerPredicate func(*nbdb.LoadBalancer) bool

// FindLoadBalancersWithPredicate looks up loadbalancers from the cache
// based on a given predicate
func FindLoadBalancersWithPredicate(nbClient libovsdbclient.Client, p loadBalancerPredicate) ([]*nbdb.LoadBalancer, error) {
	found := []*nbdb.LoadBalancer{}
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	err := nbClient.WhereCache(p).List(ctx, &found)
	return found, err
}

package libovsdbops

import (
	"context"
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// findLoadBalancer looks up the load balancer in the cache and sets the UUID
func findLoadBalancer(nbClient libovsdbclient.Client, lb *nbdb.LoadBalancer) error {
	if lb.UUID != "" && !IsNamedUUID(lb.UUID) {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	lbs := []nbdb.LoadBalancer{}
	err := nbClient.WhereCache(func(item *nbdb.LoadBalancer) bool {
		return item.Name == lb.Name
	}).List(ctx, &lbs)
	if err != nil {
		return fmt.Errorf("can't find load balancer %+v: %v", *lb, err)
	}

	if len(lbs) > 1 {
		return fmt.Errorf("unexpectedly found multiple load balancers: %+v", lbs)
	}

	if len(lbs) == 0 {
		return libovsdbclient.ErrNotFound
	}

	lb.UUID = lbs[0].UUID
	return nil
}

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

func BuildLoadBalancer(name string, protocol nbdb.LoadBalancerProtocol, selectionFields []nbdb.LoadBalancerSelectionFields, vips, options, externalIds map[string]string) *nbdb.LoadBalancer {
	return &nbdb.LoadBalancer{
		Name:            name,
		Protocol:        &protocol,
		SelectionFields: selectionFields,
		Vips:            vips,
		Options:         options,
		ExternalIDs:     externalIds,
	}
}

func ensureLoadBalancerUUID(lb *nbdb.LoadBalancer) {
	if lb.UUID == "" {
		lb.UUID = BuildNamedUUID()
	}
}

func createOrUpdateLoadBalancerOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, lb *nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	err := findLoadBalancer(nbClient, lb)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return nil, err
	}

	// If LoadBalancer does not exist, create it
	if err == libovsdbclient.ErrNotFound {
		ensureLoadBalancerUUID(lb)
		op, err := nbClient.Create(lb)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op...)
		return ops, nil
	}

	fields := getNonZeroLoadBalancerMutableFields(lb)
	op, err := nbClient.Where(lb).Update(lb, fields...)
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)

	return ops, nil
}

func CreateOrUpdateLoadBalancersOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	for _, lb := range lbs {
		var err error
		ops, err = createOrUpdateLoadBalancerOps(nbClient, ops, lb)
		if err != nil {
			return nil, err
		}
	}

	return ops, nil
}

func RemoveLoadBalancerVipsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, lb *nbdb.LoadBalancer, vips ...string) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	err := findLoadBalancer(nbClient, lb)
	if err != nil {
		return nil, err
	}

	op, err := nbClient.Where(lb).Mutate(lb, model.Mutation{
		Field:   &lb.Vips,
		Mutator: libovsdb.MutateOperationDelete,
		Value:   vips,
	})
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)

	return ops, nil
}

func deleteLoadBalancerOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, lb *nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	err := findLoadBalancer(nbClient, lb)
	if err == libovsdbclient.ErrNotFound {
		// noop
		return ops, nil
	}
	if err != nil {
		return nil, err
	}

	op, err := nbClient.Where(lb).Delete()
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)

	return ops, nil
}

func DeleteLoadBalancersOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, lbs ...*nbdb.LoadBalancer) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	var err error
	for _, lb := range lbs {
		ops, err = deleteLoadBalancerOps(nbClient, ops, lb)
		if err != nil {
			return nil, err
		}
	}

	return ops, nil
}

func DeleteLoadBalancers(nbClient libovsdbclient.Client, lbs []*nbdb.LoadBalancer) error {
	ops, err := DeleteLoadBalancersOps(nbClient, nil, lbs...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func ListLoadBalancers(nbClient libovsdbclient.Client) ([]nbdb.LoadBalancer, error) {
	lbs := &[]nbdb.LoadBalancer{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := nbClient.List(ctx, lbs)
	if err != nil {
		return nil, err
	}
	return *lbs, nil
}

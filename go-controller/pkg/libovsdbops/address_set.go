package libovsdbops

import (
	"context"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

type addressSetPredicate func(*nbdb.AddressSet) bool

// getNonZeroAddressSetMutableFields builds a list of address set
// mutable fields with non zero values to be used as the list of fields to
// Update.
// The purpose is to prevent libovsdb interpreting non-nil empty maps/slices
// as default and thus being filtered out of the update. The intention is to
// use non-nil empty maps/slices to clear them out in the update.
// See: https://github.com/ovn-org/libovsdb/issues/226
func getNonZeroAddressSetMutableFields(as *nbdb.AddressSet) []interface{} {
	fields := []interface{}{}
	if as.Addresses != nil {
		fields = append(fields, &as.Addresses)
	}
	if as.ExternalIDs != nil {
		fields = append(fields, &as.ExternalIDs)
	}
	return fields
}

// FindAddressSetsWithPredicate looks up address sets from the cache based on a
// given predicate
func FindAddressSetsWithPredicate(nbClient libovsdbclient.Client, p addressSetPredicate) ([]*nbdb.AddressSet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	found := []*nbdb.AddressSet{}
	err := nbClient.WhereCache(p).List(ctx, &found)
	return found, err
}

// GetAddressSet looks up an address sets from the cache
func GetAddressSet(nbClient libovsdbclient.Client, as *nbdb.AddressSet) (*nbdb.AddressSet, error) {
	found := []*nbdb.AddressSet{}
	opModel := operationModel{
		Model:          as,
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

// CreateAddressSets creates the provided address sets
func CreateAddressSets(nbClient libovsdbclient.Client, ass ...*nbdb.AddressSet) error {
	opModels := make([]operationModel, 0, len(ass))
	for i := range ass {
		as := ass[i]
		opModel := operationModel{
			Model:          as,
			OnModelUpdates: onModelUpdatesNone(),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	m := newModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModels...)
	return err
}

// CreateOrUpdateAddressSets creates or updates the provided address sets
func CreateOrUpdateAddressSets(nbClient libovsdbclient.Client, ass ...*nbdb.AddressSet) error {
	opModels := make([]operationModel, 0, len(ass))
	for i := range ass {
		as := ass[i]
		opModel := operationModel{
			Model:          as,
			OnModelUpdates: getNonZeroAddressSetMutableFields(as),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	m := newModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModels...)
	return err
}

// UpdateAddressSetsIPs updates the IPs on the provided address sets
func UpdateAddressSetsIPs(nbClient libovsdbclient.Client, ass ...*nbdb.AddressSet) error {
	opModels := make([]operationModel, 0, len(ass))
	for i := range ass {
		as := ass[i]
		opModel := operationModel{
			Model:          as,
			OnModelUpdates: []interface{}{&as.Addresses},
			ErrNotFound:    true,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	m := newModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModels...)
	return err
}

// AddIPsToAddressSetOps adds the provided IPs to the provided address set and
// returns the corresponding ops
func AddIPsToAddressSetOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, as *nbdb.AddressSet, ips ...string) ([]libovsdb.Operation, error) {
	originalIPs := as.Addresses
	as.Addresses = ips
	opModel := operationModel{
		Model:            as,
		OnModelMutations: []interface{}{&as.Addresses},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(nbClient)
	ops, err := m.CreateOrUpdateOps(ops, opModel)
	as.Addresses = originalIPs
	return ops, err
}

// DeleteIPsFromAddressSetOps removes the provided IPs from the provided address
// set and returns the corresponding ops
func DeleteIPsFromAddressSetOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, as *nbdb.AddressSet, ips ...string) ([]libovsdb.Operation, error) {
	originalIPs := as.Addresses
	as.Addresses = ips
	opModel := operationModel{
		Model:            as,
		OnModelMutations: []interface{}{&as.Addresses},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(nbClient)
	ops, err := m.DeleteOps(ops, opModel)
	as.Addresses = originalIPs
	return ops, err
}

// DeleteAddressSets deletes the provided address sets
func DeleteAddressSets(nbClient libovsdbclient.Client, ass ...*nbdb.AddressSet) error {
	opModels := make([]operationModel, 0, len(ass))
	for i := range ass {
		as := ass[i]
		opModel := operationModel{
			Model:       as,
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels = append(opModels, opModel)
	}

	m := newModelClient(nbClient)
	return m.Delete(opModels...)
}

// DeleteAddressSetsWithPredicate looks up address sets from the cache based on
// a given predicate and deletes them
func DeleteAddressSetsWithPredicate(nbClient libovsdbclient.Client, p addressSetPredicate) error {
	deleted := []*nbdb.AddressSet{}
	opModel := operationModel{
		ModelPredicate: p,
		ExistingResult: &deleted,
		ErrNotFound:    false,
		BulkOp:         true,
	}

	m := newModelClient(nbClient)
	return m.Delete(opModel)
}

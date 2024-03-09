package ops

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

// CreateAddressSetsOps creates the create-ops for the provided address sets
func CreateAddressSetsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, addrSets ...*nbdb.AddressSet) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(addrSets))
	for i := range addrSets {
		as := addrSets[i]
		opModel := operationModel{
			Model:          as,
			OnModelUpdates: onModelUpdatesNone(),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModels...)
}

// CreateAddressSets creates the provided address sets
func CreateAddressSets(nbClient libovsdbclient.Client, addrSets ...*nbdb.AddressSet) error {
	ops, err := CreateAddressSetsOps(nbClient, nil, addrSets...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func CreateOrUpdateAddressSetsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation,
	addrSets ...*nbdb.AddressSet) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(addrSets))
	for i := range addrSets {
		as := addrSets[i]
		opModel := operationModel{
			Model:          as,
			OnModelUpdates: getNonZeroAddressSetMutableFields(as),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	m := newModelClient(nbClient)
	return m.CreateOrUpdateOps(ops, opModels...)
}

// CreateOrUpdateAddressSets creates or updates the provided address sets
func CreateOrUpdateAddressSets(nbClient libovsdbclient.Client, addrSets ...*nbdb.AddressSet) error {
	ops, err := CreateOrUpdateAddressSetsOps(nbClient, nil, addrSets...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// UpdateAddressSetsAddresses updates the Addresses on the provided address sets
func UpdateAddressSetsAddresses(nbClient libovsdbclient.Client, addrSets ...*nbdb.AddressSet) error {
	opModels := make([]operationModel, 0, len(addrSets))
	for i := range addrSets {
		as := addrSets[i]
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

// AddAddressesToAddressSetOps adds the provided addresses to the provided address set and
// returns the corresponding ops
func AddAddressesToAddressSetOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, as *nbdb.AddressSet, addresses ...string) ([]libovsdb.Operation, error) {
	originalAddresses := as.Addresses
	as.Addresses = addresses
	opModel := operationModel{
		Model:            as,
		OnModelMutations: []interface{}{&as.Addresses},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(nbClient)
	ops, err := m.CreateOrUpdateOps(ops, opModel)
	as.Addresses = originalAddresses
	return ops, err
}

// DeleteAddressesFromAddressSetOps removes the provided addresses from the provided address
// set and returns the corresponding ops
func DeleteAddressesFromAddressSetOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, as *nbdb.AddressSet, addresses ...string) ([]libovsdb.Operation, error) {
	originalAddresses := as.Addresses
	as.Addresses = addresses
	opModel := operationModel{
		Model:            as,
		OnModelMutations: []interface{}{&as.Addresses},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(nbClient)
	ops, err := m.DeleteOps(ops, opModel)
	as.Addresses = originalAddresses
	return ops, err
}

func DeleteAddressSetsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, addrSets ...*nbdb.AddressSet) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(addrSets))
	for i := range addrSets {
		as := addrSets[i]
		opModel := operationModel{
			Model:       as,
			ErrNotFound: false,
			BulkOp:      false,
		}
		opModels = append(opModels, opModel)
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModels...)
}

// DeleteAddressSets deletes the provided address sets
func DeleteAddressSets(nbClient libovsdbclient.Client, addrSets ...*nbdb.AddressSet) error {
	opModels := make([]operationModel, 0, len(addrSets))
	for i := range addrSets {
		as := addrSets[i]
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

// DeleteAddressSetsWithPredicateOps returns the ops to delete address sets based on a given predicate
func DeleteAddressSetsWithPredicateOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, p addressSetPredicate) ([]libovsdb.Operation, error) {
	deleted := []*nbdb.AddressSet{}
	opModel := operationModel{
		ModelPredicate: p,
		ExistingResult: &deleted,
		ErrNotFound:    false,
		BulkOp:         true,
	}

	m := newModelClient(nbClient)
	return m.DeleteOps(ops, opModel)
}

// DeleteAddressSetsWithPredicate looks up address sets from the cache based on
// a given predicate and deletes them
func DeleteAddressSetsWithPredicate(nbClient libovsdbclient.Client, p addressSetPredicate) error {
	ops, err := DeleteAddressSetsWithPredicateOps(nbClient, nil, p)
	if err != nil {
		return nil
	}
	_, err = TransactAndCheck(nbClient, ops)
	return err
}

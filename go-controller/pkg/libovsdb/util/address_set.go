// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

// DeleteAddrSetsWithoutACLRef deletes the address sets related to the predicateIDs without any acl reference.
func DeleteAddrSetsWithoutACLRef(predicateIDs *libovsdbops.DbObjectIDs, nbClient libovsdbclient.Client) error {
	return deleteAddrSetsWithoutACLRef(predicateIDs, nil, nbClient)
}

func DeleteAddrSetsWithoutACLRefAnyController(dbOwnerType *libovsdbops.ObjectIDsType, nbClient libovsdbclient.Client) error {
	return deleteAddrSetsWithoutACLRef(nil, dbOwnerType, nbClient)
}

// DeleteAddrSetsWithoutMatchRef deletes address sets related to predicateIDs
// when they are not referenced from ACL, NAT, or logical router policy matches.
func DeleteAddrSetsWithoutMatchRef(predicateIDs *libovsdbops.DbObjectIDs, nbClient libovsdbclient.Client) error {
	addrSets, err := libovsdbops.FindAddressSetsWithPredicate(
		nbClient,
		libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil),
	)
	if err != nil {
		return fmt.Errorf("failed to find address sets with predicate: %w", err)
	}
	if len(addrSets) == 0 {
		return nil
	}

	addrSetNames := sets.New[string]()
	for _, addrSet := range addrSets {
		addrSetNames.Insert(addrSet.Name)
	}
	referencedNames, err := addressSetNamesReferencedInMatches(nbClient, addrSetNames)
	if err != nil {
		return err
	}

	staleAddressSets := make([]*nbdb.AddressSet, 0, len(addrSets))
	for _, addrSet := range addrSets {
		if !referencedNames.Has(addrSet.Name) {
			staleAddressSets = append(staleAddressSets, addrSet)
		}
	}
	if len(staleAddressSets) == 0 {
		return nil
	}
	if err := libovsdbops.DeleteAddressSets(nbClient, staleAddressSets...); err != nil {
		return fmt.Errorf("failed to delete address sets without match references: %w", err)
	}
	return nil
}

// DeleteAddrSetsWithoutMatchRefAnyController deletes the address sets of the given owner type,
// across all controllers, that no ACL, NAT, logical router policy or QoS match refers to.
func DeleteAddrSetsWithoutMatchRefAnyController(dbOwnerType *libovsdbops.ObjectIDsType, nbClient libovsdbclient.Client) error {
	addrSets, err := libovsdbops.FindAddressSetsWithPredicate(
		nbClient,
		libovsdbops.GetAnyControllerPredicate[*nbdb.AddressSet](dbOwnerType, nil),
	)
	if err != nil {
		return fmt.Errorf("failed to find address sets with predicate: %w", err)
	}
	if len(addrSets) == 0 {
		return nil
	}

	addrSetNames := sets.New[string]()
	for _, addrSet := range addrSets {
		addrSetNames.Insert(addrSet.Name)
	}
	referencedNames, err := addressSetNamesReferencedInMatches(nbClient, addrSetNames)
	if err != nil {
		return err
	}
	_, err = libovsdbops.FindQoSesWithPredicate(nbClient, func(qos *nbdb.QoS) bool {
		for addrSetName := range addrSetNames {
			if strings.Contains(qos.Match, addrSetName) {
				referencedNames.Insert(addrSetName)
			}
		}
		return false
	})
	if err != nil {
		return fmt.Errorf("failed to find QoSes referencing address sets: %w", err)
	}

	staleAddressSets := make([]*nbdb.AddressSet, 0, len(addrSets))
	for _, addrSet := range addrSets {
		if !referencedNames.Has(addrSet.Name) {
			staleAddressSets = append(staleAddressSets, addrSet)
		}
	}
	if len(staleAddressSets) == 0 {
		return nil
	}
	if err := libovsdbops.DeleteAddressSets(nbClient, staleAddressSets...); err != nil {
		return fmt.Errorf("failed to delete address sets without match references: %w", err)
	}
	return nil
}

func addressSetNamesReferencedInMatches(nbClient libovsdbclient.Client, addressSetNames sets.Set[string]) (sets.Set[string], error) {
	referencedNames := sets.New[string]()
	recordReferences := func(match string) {
		for addressSetName := range addressSetNames {
			if strings.Contains(match, addressSetName) {
				referencedNames.Insert(addressSetName)
			}
		}
	}

	_, err := libovsdbops.FindACLsWithPredicate(nbClient, func(acl *nbdb.ACL) bool {
		recordReferences(acl.Match)
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find ACLs referencing address sets: %w", err)
	}

	_, err = libovsdbops.FindNATsWithPredicate(nbClient, func(nat *nbdb.NAT) bool {
		recordReferences(nat.Match)
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find NATs referencing address sets: %w", err)
	}

	_, err = libovsdbops.FindLogicalRouterPoliciesWithPredicate(nbClient, func(policy *nbdb.LogicalRouterPolicy) bool {
		recordReferences(policy.Match)
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find logical router policies referencing address sets: %w", err)
	}

	return referencedNames, nil
}

func deleteAddrSetsWithoutACLRef(predicateIDs *libovsdbops.DbObjectIDs, dbOwnerType *libovsdbops.ObjectIDsType, nbClient libovsdbclient.Client) error {
	// Get the list of existing address sets for the predicateIDs. Fill the address set
	// names and mark them as unreferenced.
	addrSetReferenced := map[string]bool{}
	// this is used for new style controllers that work for all controller/network names
	var predicate func(item *nbdb.AddressSet) bool
	if predicateIDs == nil {
		predicate = libovsdbops.GetAnyControllerPredicate[*nbdb.AddressSet](dbOwnerType, func(item *nbdb.AddressSet) bool {
			addrSetReferenced[item.Name] = false
			return false
		})
	} else {
		predicate = libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, func(item *nbdb.AddressSet) bool {
			addrSetReferenced[item.Name] = false
			return false
		})
	}

	_, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, predicate)
	if err != nil {
		return fmt.Errorf("failed to find address sets with predicate: %w", err)
	}

	// Set addrSetReferenced[addrSetName] = true if referencing acl exists.
	_, err = libovsdbops.FindACLsWithPredicate(nbClient, func(item *nbdb.ACL) bool {
		for addrSetName := range addrSetReferenced {
			if strings.Contains(item.Match, addrSetName) {
				addrSetReferenced[addrSetName] = true
			}
		}
		return false
	})
	if err != nil {
		return fmt.Errorf("cannot find ACLs referencing address set: %v", err)
	}

	// Iterate through each address set and if an address set is not referenced by any
	// acl then delete it.
	ops := []ovsdb.Operation{}
	for addrSetName, isReferenced := range addrSetReferenced {
		if !isReferenced {
			// No references for stale address set, delete.
			ops, err = libovsdbops.DeleteAddressSetsOps(nbClient, ops, &nbdb.AddressSet{
				Name: addrSetName,
			})
			if err != nil {
				return fmt.Errorf("failed to get delete address set ops: %w", err)
			}
		}
	}

	// Delete the stale address sets.
	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to transact db ops to delete address sets: %v", err)
	}
	return nil
}

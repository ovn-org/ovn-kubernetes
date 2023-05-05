package hash_primary_id

import (
	"fmt"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/batching"
)

func HashPrimaryIDACL(nbClient libovsdbclient.Client, batchSize int) error {
	legacyACLSetPred := func(item *nbdb.ACL) bool {
		// we know that old format used ":" as delimiter for IDs
		// and new format has only alphanumerical symbols
		return strings.Contains(item.ExternalIDs[libovsdbops.PrimaryIDKey.String()], ":")
	}

	staleACLs, err := libovsdbops.FindACLsWithPredicate(nbClient, legacyACLSetPred)
	if err != nil {
		return fmt.Errorf("failed to find address sets with stale primaryID: %v", err)
	}

	for _, acl := range staleACLs {
		newID, err := libovsdbops.SyncPrimaryID(acl.ExternalIDs[libovsdbops.PrimaryIDKey.String()])
		if err != nil {
			return fmt.Errorf("failed to sync primaryID: %w", err)
		}
		acl.ExternalIDs[libovsdbops.PrimaryIDKey.String()] = newID
	}

	return batching.Batch[*nbdb.ACL](batchSize, staleACLs, func(batchACLs []*nbdb.ACL) error {
		ops, err := libovsdbops.UpdateACLsOps(nbClient, nil, batchACLs...)
		fmt.Printf("OPS: %+v", ops)
		if err != nil {
			return fmt.Errorf("failed to get update acl ops: %v", err)
		}
		_, err = libovsdbops.TransactAndCheck(nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to transact update acl ops: %v", err)
		}
		return nil
	})
}

func HashPrimaryIDAddrSet(nbClient libovsdbclient.Client, batchSize int) error {
	legacyAddrSetPred := func(item *nbdb.AddressSet) bool {
		// we know that old format used ":" as delimiter for IDs
		// and new format has only alphanumerical symbols
		return strings.Contains(item.ExternalIDs[libovsdbops.PrimaryIDKey.String()], ":")
	}

	staleAddrSets, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, legacyAddrSetPred)
	if err != nil {
		return fmt.Errorf("failed to find address sets with stale primaryID: %v", err)
	}

	for _, addrSet := range staleAddrSets {
		newID, err := libovsdbops.SyncPrimaryID(addrSet.ExternalIDs[libovsdbops.PrimaryIDKey.String()])
		if err != nil {
			return fmt.Errorf("failed to sync primaryID: %w", err)
		}
		addrSet.ExternalIDs[libovsdbops.PrimaryIDKey.String()] = newID
	}

	return batching.Batch[*nbdb.AddressSet](batchSize, staleAddrSets, func(batchAddrSets []*nbdb.AddressSet) error {
		err := libovsdbops.CreateOrUpdateAddressSets(nbClient, batchAddrSets...)
		if err != nil {
			return fmt.Errorf("failed to update address sets: %v", err)
		}
		return nil
	})
}

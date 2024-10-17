package util

import (
	"fmt"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"strings"
)

func GetPortGroupName(dbIDs *libovsdbops.DbObjectIDs) string {
	return util.HashForOVN(dbIDs.GetExternalIDs()[libovsdbops.PrimaryIDKey.String()])
}

func BuildPortGroup(pgIDs *libovsdbops.DbObjectIDs, ports []*nbdb.LogicalSwitchPort, acls []*nbdb.ACL) *nbdb.PortGroup {
	externalIDs := pgIDs.GetExternalIDs()
	pg := nbdb.PortGroup{
		Name:        util.HashForOVN(externalIDs[libovsdbops.PrimaryIDKey.String()]),
		ExternalIDs: externalIDs,
	}

	if len(acls) > 0 {
		pg.ACLs = make([]string, 0, len(acls))
		for _, acl := range acls {
			pg.ACLs = append(pg.ACLs, acl.UUID)
		}
	}

	if len(ports) > 0 {
		pg.Ports = make([]string, 0, len(ports))
		for _, port := range ports {
			pg.Ports = append(pg.Ports, port.UUID)
		}
	}

	return &pg
}

// DeletePortGroupsWithoutACLRef deletes the port groups related to the predicateIDs without any acl reference.
func DeletePortGroupsWithoutACLRef(predicateIDs *libovsdbops.DbObjectIDs, nbClient libovsdbclient.Client) error {
	// Get the list of existing address sets for the predicateIDs. Fill the address set
	// names and mark them as unreferenced.
	portGroupReferenced := map[string]bool{}
	predicate := libovsdbops.GetPredicate[*nbdb.PortGroup](predicateIDs, func(item *nbdb.PortGroup) bool {
		portGroupReferenced[item.Name] = false
		return false
	})
	_, err := libovsdbops.FindPortGroupsWithPredicate(nbClient, predicate)
	if err != nil {
		return fmt.Errorf("failed to find address sets with predicate: %w", err)
	}

	// Set portGroupReferenced[pgName] = true if referencing acl exists.
	_, err = libovsdbops.FindACLsWithPredicate(nbClient, func(item *nbdb.ACL) bool {
		for pgName := range portGroupReferenced {
			if strings.Contains(item.Match, pgName) {
				portGroupReferenced[pgName] = true
			}
		}
		return false
	})
	if err != nil {
		return fmt.Errorf("cannot find ACLs referencing port group: %v", err)
	}

	// Iterate through each address set and if an address set is not referenced by any
	// acl then delete it.
	ops := []ovsdb.Operation{}
	for pgName, isReferenced := range portGroupReferenced {
		if !isReferenced {
			// No references for stale address set, delete.
			ops, err = libovsdbops.DeletePortGroupsOps(nbClient, ops, pgName)
			if err != nil {
				return fmt.Errorf("failed to get delete port groups ops: %w", err)
			}
		}
	}

	// Delete the stale address sets.
	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to transact db ops to delete port groups: %v", err)
	}
	return nil
}

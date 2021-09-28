package libovsdbops

import (
	"fmt"
	"reflect"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// getACLName returns the ACL name if it has one otherwise returns
// an empty string.
func getACLName(acl *nbdb.ACL) string {
	if acl.Name != nil {
		return *acl.Name
	}
	return ""
}

// isEquivalentACL if it has same uuid, or if it has same name
// and external ids, or if it has same priority, direction, match
// and action.
func isEquivalentACL(existing *nbdb.ACL, searched *nbdb.ACL) bool {
	if searched.UUID != "" && existing.UUID == searched.UUID {
		return true
	}

	eName := getACLName(existing)
	sName := getACLName(searched)
	// TODO if we want to support adding/removing external ids,
	// we need to compare them differently, perhaps just the common subset
	if eName != "" && eName == sName && reflect.DeepEqual(existing.ExternalIDs, searched.ExternalIDs) {
		return true
	}

	return existing.Priority == searched.Priority &&
		existing.Direction == searched.Direction &&
		existing.Match == searched.Match &&
		existing.Action == searched.Action
}

// findACL looks up the ACL in the cache and sets the UUID
func findACL(nbClient libovsdbclient.Client, acl *nbdb.ACL) error {
	if acl.UUID != "" && !IsNamedUUID(acl.UUID) {
		return nil
	}

	acls := []nbdb.ACL{}
	err := nbClient.WhereCache(func(item *nbdb.ACL) bool {
		return isEquivalentACL(item, acl)
	}).List(&acls)

	if err != nil {
		return fmt.Errorf("can't find ACL by equivalence %+v: %v", *acl, err)
	}

	if len(acls) > 1 {
		return fmt.Errorf("unexpectedly found multiple equivalent ACLs: %+v", acls)
	}

	if len(acls) == 0 {
		return libovsdbclient.ErrNotFound
	}

	acl.UUID = acls[0].UUID
	return nil
}

func ensureACLUUID(acl *nbdb.ACL) {
	if acl.UUID == "" {
		acl.UUID = BuildNamedUUID()
	}
}

func BuildACL(name string, direction nbdb.ACLDirection, priority int, match string, action nbdb.ACLAction, meter string, severity nbdb.ACLSeverity, log bool, externalIds map[string]string) *nbdb.ACL {
	name = fmt.Sprintf("%.63s", name)
	return &nbdb.ACL{
		Name:        &name,
		Direction:   direction,
		Match:       match,
		Action:      action,
		Priority:    priority,
		Severity:    &severity,
		Log:         log,
		Meter:       &meter,
		ExternalIDs: externalIds,
	}
}

func createOrUpdateACLOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, acl *nbdb.ACL) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	err := findACL(nbClient, acl)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return nil, err
	}

	// If ACL does not exist, create it
	if err == libovsdbclient.ErrNotFound {
		ensureACLUUID(acl)
		op, err := nbClient.Create(acl)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op...)
		return ops, nil
	}

	uuid := acl.UUID
	acl.UUID = ""
	op, err := nbClient.Where(&nbdb.ACL{UUID: uuid}).Update(acl)
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)
	acl.UUID = uuid

	return ops, nil
}

func CreateOrUpdateACLsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}
	for _, acl := range acls {
		var err error
		ops, err = createOrUpdateACLOps(nbClient, ops, acl)
		if err != nil {
			return nil, err
		}
	}

	return ops, nil
}

func CreateOrUpdateACLs(nbClient libovsdbclient.Client, acls ...*nbdb.ACL) error {
	ops, err := CreateOrUpdateACLsOps(nbClient, nil, acls...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func UpdateACLLoggingOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, acl *nbdb.ACL) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	err := findACL(nbClient, acl)
	if err != nil {
		return nil, err
	}

	uuid := acl.UUID
	acl.UUID = ""
	op, err := nbClient.Where(&nbdb.ACL{UUID: uuid}).Update(acl, &acl.Severity, &acl.Log)
	if err != nil {
		return nil, err
	}
	ops = append(ops, op...)
	acl.UUID = uuid

	return ops, nil
}

func UpdateACLLogging(nbClient libovsdbclient.Client, acl *nbdb.ACL) error {
	ops, err := UpdateACLLoggingOps(nbClient, nil, acl)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func FindRejectACLs(nbClient libovsdbclient.Client) ([]*nbdb.ACL, error) {
	acls := []nbdb.ACL{}
	err := nbClient.WhereCache(func(acl *nbdb.ACL) bool {
		return acl.Action == nbdb.ACLActionReject
	}).List(&acls)
	if err != nil {
		return nil, err
	}

	aclPtrs := []*nbdb.ACL{}
	for i := range acls {
		aclPtrs = append(aclPtrs, &acls[i])
	}

	return aclPtrs, nil
}

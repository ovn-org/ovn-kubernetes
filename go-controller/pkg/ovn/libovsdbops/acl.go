package libovsdbops

import (
	"context"
	"fmt"
	"reflect"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// getACLName returns the ACL name if it has one otherwise returns
// an empty string.
func getACLName(acl *nbdb.ACL) string {
	if acl.Name != nil {
		return *acl.Name
	}
	return ""
}

// IsEquivalentACL if it has same uuid, or if it has same name
// and external ids, or if it has same priority, direction, match
// and action.
func IsEquivalentACL(existing *nbdb.ACL, searched *nbdb.ACL) bool {
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

// findACLsByPredicate looks up ACLs from the cache based on a given predicate
func findACLsByPredicate(nbClient libovsdbclient.Client, lookupFunction func(item *nbdb.ACL) bool) ([]nbdb.ACL, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	acls := []nbdb.ACL{}
	err := nbClient.WhereCache(lookupFunction).List(ctx, &acls)
	if err != nil {
		return nil, err
	}

	return acls, nil
}

// findACL looks up the ACL in the cache and sets the UUID
func findACL(nbClient libovsdbclient.Client, acl *nbdb.ACL) error {
	if acl.UUID != "" && !IsNamedUUID(acl.UUID) {
		return nil
	}

	acls, err := findACLsByPredicate(nbClient, func(item *nbdb.ACL) bool {
		return IsEquivalentACL(item, acl)
	})

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

// FindRejectACLs looks up the acls with reject action
func FindRejectACLs(nbClient libovsdbclient.Client) ([]nbdb.ACL, error) {
	rejectACLLookupFcn := func(acl *nbdb.ACL) bool {
		return acl.Action == nbdb.ACLActionReject
	}

	return findACLsByPredicate(nbClient, rejectACLLookupFcn)
}

// FindACLsByPriorityRange looks up the acls with priorities within a given inclusive range
func FindACLsByPriorityRange(nbClient libovsdbclient.Client, minPriority, maxPriority int) ([]nbdb.ACL, error) {
	// Lookup all ACLs in the specified priority range
	priorityRangeLookupFcn := func(item *nbdb.ACL) bool {
		return (item.Priority >= minPriority) && (item.Priority <= maxPriority)
	}

	return findACLsByPredicate(nbClient, priorityRangeLookupFcn)
}

// FindACLsByExternalID looks up the acls with the given externalID/s
func FindACLsByExternalID(nbClient libovsdbclient.Client, externalIDs map[string]string) ([]nbdb.ACL, error) {
	// Find ACLs for with a given exernalID
	ACLLookupFcn := func(item *nbdb.ACL) bool {
		aclMatch := false
		for k, v := range externalIDs {
			if itemVal, ok := item.ExternalIDs[k]; ok {
				if itemVal == v {
					aclMatch = true
				}
			}
		}
		return aclMatch
	}

	return findACLsByPredicate(nbClient, ACLLookupFcn)
}

func ensureACLUUID(acl *nbdb.ACL) {
	if acl.UUID == "" {
		acl.UUID = BuildNamedUUID()
	}
}

func BuildACL(name string, direction nbdb.ACLDirection, priority int, match string, action nbdb.ACLAction, meter string, severity nbdb.ACLSeverity, log bool, externalIds map[string]string) *nbdb.ACL {
	name = fmt.Sprintf("%.63s", name)

	var realName *string
	var realMeter *string
	var realSeverity *string
	if len(name) != 0 {
		realName = &name
	}
	if len(meter) != 0 {
		realMeter = &meter
	}
	if len(severity) != 0 {
		realSeverity = &severity
	}

	return &nbdb.ACL{
		Name:        realName,
		Direction:   direction,
		Match:       match,
		Action:      action,
		Priority:    priority,
		Severity:    realSeverity,
		Log:         log,
		Meter:       realMeter,
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

func UpdateACLsLoggingOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
	if ops == nil {
		ops = []libovsdb.Operation{}
	}

	for _, acl := range acls {
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
	}

	return ops, nil
}

func DeleteACLs(nbClient libovsdbclient.Client, acls []nbdb.ACL) error {
	opModels := []OperationModel{}
	for _, acl := range acls {
		opModels = append(opModels, OperationModel{
			Model:          &acl,
			ModelPredicate: func(item *nbdb.ACL) bool { return IsEquivalentACL(item, &acl) },
		})
	}

	m := NewModelClient(nbClient)
	err := m.Delete(opModels...)
	if err != nil {
		return fmt.Errorf("failed to manually delete ACLs err: %v", err)
	}

	return nil
}

func UpdateACLLogging(nbClient libovsdbclient.Client, acl *nbdb.ACL) error {
	ops, err := UpdateACLsLoggingOps(nbClient, nil, acl)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

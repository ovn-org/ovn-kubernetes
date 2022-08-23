package ovn

import (
	"fmt"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// GetNamespaceACLLogging retrieves ACLLoggingLevels for the Namespace.
// nsInfo will be locked (and unlocked at the end) for given namespace if it exists.
func (oc *Controller) GetNamespaceACLLogging(ns string) *ACLLoggingLevels {
	nsInfo, nsUnlock := oc.getNamespaceLocked(ns, true)
	if nsInfo == nil {
		return &ACLLoggingLevels{
			Allow: "",
			Deny:  "",
		}
	}
	defer nsUnlock()
	return &nsInfo.aclLogging
}

func getLogSeverity(action string, aclLogging *ACLLoggingLevels) (log bool, severity string) {
	severity = ""
	if aclLogging != nil {
		if action == nbdb.ACLActionAllow || action == nbdb.ACLActionAllowRelated || action == nbdb.ACLActionAllowStateless {
			severity = aclLogging.Allow
		} else if action == nbdb.ACLActionDrop || action == nbdb.ACLActionReject {
			severity = aclLogging.Deny
		}
	}
	log = severity != ""
	return log, severity
}

// UpdateACLLoggingWithPredicate finds all ACLs based on a given predicate, updates log settings,
// then transacts these changes with a single transaction.
func UpdateACLLoggingWithPredicate(nbClient libovsdbclient.Client, p func(*nbdb.ACL) bool, aclLogging *ACLLoggingLevels) error {
	ACLs, err := libovsdbops.FindACLsWithPredicate(nbClient, p)
	if err != nil {
		return fmt.Errorf("unable to list ACLs with predicate, err: %v", err)
	}
	return UpdateACLLogging(nbClient, ACLs, aclLogging)
}

func UpdateACLLogging(nbClient libovsdbclient.Client, ACLs []*nbdb.ACL, aclLogging *ACLLoggingLevels) error {
	if len(ACLs) == 0 {
		return nil
	}
	for i := range ACLs {
		log, severity := getLogSeverity(ACLs[i].Action, aclLogging)
		libovsdbops.SetACLLogging(ACLs[i], severity, log)
	}
	ops, err := libovsdbops.UpdateACLsLoggingOps(nbClient, nil, ACLs...)
	if err != nil {
		return fmt.Errorf("unable to get ACL logging ops: %v", err)
	}
	if _, err := libovsdbops.TransactAndCheck(nbClient, ops); err != nil {
		return fmt.Errorf("unable to update ACL logging: %v", err)
	}
	return nil
}

// BuildACL should be used to build ACL instead of directly calling libovsdbops.BuildACL.
// It can properly set and reset log settings for ACL based on ACLLoggingLevels
func BuildACL(name string, direction nbdb.ACLDirection, priority int, match string, action nbdb.ACLAction,
	logLevels *ACLLoggingLevels, externalIds map[string]string, options map[string]string) *nbdb.ACL {
	log, logSeverity := getLogSeverity(action, logLevels)
	ACL := libovsdbops.BuildACL(
		name,
		direction,
		priority,
		match,
		action,
		types.OvnACLLoggingMeter,
		logSeverity,
		log,
		externalIds,
		options,
	)
	return ACL
}

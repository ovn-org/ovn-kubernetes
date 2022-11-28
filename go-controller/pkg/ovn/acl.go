package ovn

import (
	"fmt"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	knet "k8s.io/api/networking/v1"
)

func joinACLName(substrings ...string) string {
	return strings.Join(substrings, "_")
}

// aclType defines when ACLs will be applied (direction and pipeline stage).
// All acls of the same type will be sorted by priority, priorities for different types are independent.
type aclType string

const (
	// lportIngress will be converted to direction="to-lport" ACL
	lportIngress aclType = "to-lport"
	// lportEgressAfterLB will be converted to direction="from-lport", options={"apply-after-lb": "true"} ACL
	lportEgressAfterLB aclType = "from-lport-after-lb"
)

func aclTypeToPolicyType(aclT aclType) knet.PolicyType {
	switch aclT {
	case lportEgressAfterLB:
		return knet.PolicyTypeEgress
	case lportIngress:
		return knet.PolicyTypeIngress
	default:
		panic(fmt.Sprintf("Failed to convert aclType to PolicyType: unknown acl type %s", aclT))
	}
}

func policyTypeToAclType(policyType knet.PolicyType) aclType {
	switch policyType {
	case knet.PolicyTypeEgress:
		return lportEgressAfterLB
	case knet.PolicyTypeIngress:
		return lportIngress
	default:
		panic(fmt.Sprintf("Failed to convert PolicyType to aclType: unknown policyType type %s", policyType))
	}
}

// hash the provided input to make it a valid portGroup name.
func hashedPortGroup(s string) string {
	return util.HashForOVN(s)
}

// BuildACL should be used to build ACL instead of directly calling libovsdbops.BuildACL.
// It can properly set and reset log settings for ACL based on ACLLoggingLevels
func BuildACL(aclName string, priority int, match, action string,
	logLevels *ACLLoggingLevels, aclT aclType, externalIDs map[string]string) *nbdb.ACL {
	var options map[string]string
	var direction string
	switch aclT {
	case lportEgressAfterLB:
		direction = nbdb.ACLDirectionFromLport
		options = map[string]string{
			"apply-after-lb": "true",
		}
	case lportIngress:
		direction = nbdb.ACLDirectionToLport
	default:
		panic(fmt.Sprintf("Failed to build ACL: unknown acl type %s", aclT))
	}
	log, logSeverity := getLogSeverity(action, logLevels)
	ACL := libovsdbops.BuildACL(
		aclName,
		direction,
		priority,
		match,
		action,
		types.OvnACLLoggingMeter,
		logSeverity,
		log,
		externalIDs,
		options,
	)
	return ACL
}

func getACLMatch(portGroupName, match string, aclT aclType) string {
	var aclMatch string
	switch aclT {
	case lportIngress:
		aclMatch = "outport == @" + portGroupName
	case lportEgressAfterLB:
		aclMatch = "inport == @" + portGroupName
	default:
		panic(fmt.Sprintf("Unknown acl type %s", aclT))
	}
	if match != "" {
		aclMatch += " && " + match
	}
	return aclMatch
}

// GetNamespaceACLLogging retrieves ACLLoggingLevels for the Namespace.
// nsInfo will be locked (and unlocked at the end) for given namespace if it exists.
func (oc *DefaultNetworkController) GetNamespaceACLLogging(ns string) *ACLLoggingLevels {
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

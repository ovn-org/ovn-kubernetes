package util

import (
	"fmt"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
)

// aclPipelineType defines when ACLs will be applied (direction and pipeline stage).
// All acls of the same type will be sorted by priority, priorities for different types are independent.
type ACLPipelineType string

const (
	// LportIngress will be converted to direction="to-lport" ACL
	LportIngress ACLPipelineType = "to-lport"
	// LportEgressAfterLB will be converted to direction="from-lport", options={"apply-after-lb": "true"} ACL
	LportEgressAfterLB ACLPipelineType = "from-lport-after-lb"
)

func PolicyTypeToAclPipeline(policyType knet.PolicyType) ACLPipelineType {
	switch policyType {
	case knet.PolicyTypeEgress:
		return LportEgressAfterLB
	case knet.PolicyTypeIngress:
		return LportIngress
	default:
		panic(fmt.Sprintf("Failed to convert PolicyType to aclPipelineType: unknown policyType type %s", policyType))
	}
}

type ACLDirection string

const (
	ACLEgress  ACLDirection = "Egress"
	ACLIngress ACLDirection = "Ingress"
)

func ACLDirectionToACLPipeline(aclDir ACLDirection) ACLPipelineType {
	switch aclDir {
	case ACLEgress:
		return LportEgressAfterLB
	case ACLIngress:
		return LportIngress
	default:
		panic(fmt.Sprintf("Failed to convert ACLDirection to aclPipelineType: unknown ACLDirection type %s", aclDir))
	}
}

func JoinACLName(substrings ...string) string {
	return strings.Join(substrings, "_")
}

// acl.Name is cropped to 64 symbols and is used for logging.
// currently only egress firewall, gress network policy and default deny network policy ACLs are logged.
// Other ACLs don't need a name.
// Just a namespace name may be 63 symbols long, therefore some information may be cropped.
// Therefore, "feature" as "EF" for EgressFirewall and "NP" for network policy goes first, then namespace,
// then acl-related info.
func GetACLName(dbIDs *libovsdbops.DbObjectIDs) string {
	t := dbIDs.GetIDsType()
	aclName := ""
	switch {
	case t.IsSameType(libovsdbops.ACLNetworkPolicy):
		aclName = "NP:" + dbIDs.GetObjectID(libovsdbops.ObjectNameKey) + ":" + dbIDs.GetObjectID(libovsdbops.PolicyDirectionKey) +
			":" + dbIDs.GetObjectID(libovsdbops.GressIdxKey)
	case t.IsSameType(libovsdbops.ACLNetpolNamespace):
		aclName = "NP:" + dbIDs.GetObjectID(libovsdbops.ObjectNameKey) + ":" + dbIDs.GetObjectID(libovsdbops.PolicyDirectionKey)
	case t.IsSameType(libovsdbops.ACLEgressFirewall):
		aclName = "EF:" + dbIDs.GetObjectID(libovsdbops.ObjectNameKey) + ":" + dbIDs.GetObjectID(libovsdbops.RuleIndex)
	case t.IsSameType(libovsdbops.ACLAdminNetworkPolicy):
		aclName = "ANP:" + dbIDs.GetObjectID(libovsdbops.ObjectNameKey) + ":" + dbIDs.GetObjectID(libovsdbops.PolicyDirectionKey) +
			":" + dbIDs.GetObjectID(libovsdbops.GressIdxKey)
	case t.IsSameType(libovsdbops.ACLBaselineAdminNetworkPolicy):
		aclName = "BANP:" + dbIDs.GetObjectID(libovsdbops.ObjectNameKey) + ":" + dbIDs.GetObjectID(libovsdbops.PolicyDirectionKey) +
			":" + dbIDs.GetObjectID(libovsdbops.GressIdxKey)
	}
	return fmt.Sprintf("%.63s", aclName)
}

// BuildACL should be used to build ACL instead of directly calling libovsdbops.BuildACL.
// It can properly set and reset log settings for ACL based on ACLLoggingLevels, and
// set acl.Name and acl.ExternalIDs based on given DbIDs
func BuildACL(dbIDs *libovsdbops.DbObjectIDs, priority int, match, action string, logLevels *ACLLoggingLevels,
	aclT ACLPipelineType) *nbdb.ACL {
	var options map[string]string
	var direction string
	switch aclT {
	case LportEgressAfterLB:
		direction = nbdb.ACLDirectionFromLport
		options = map[string]string{
			"apply-after-lb": "true",
		}
	case LportIngress:
		direction = nbdb.ACLDirectionToLport
	default:
		panic(fmt.Sprintf("Failed to build ACL: unknown acl type %s", aclT))
	}
	externalIDs := dbIDs.GetExternalIDs()
	aclName := GetACLName(dbIDs)
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
		types.DefaultACLTier,
	)
	return ACL
}

func BuildANPACL(dbIDs *libovsdbops.DbObjectIDs, priority int, match, action string, aclT ACLPipelineType, logLevels *ACLLoggingLevels) *nbdb.ACL {
	anpACL := BuildACL(dbIDs, priority, match, action, logLevels, aclT)
	anpACL.Tier = GetACLTier(dbIDs)
	return anpACL
}

func GetACLTier(dbIDs *libovsdbops.DbObjectIDs) int {
	t := dbIDs.GetIDsType()
	switch {
	case t.IsSameType(libovsdbops.ACLAdminNetworkPolicy):
		return types.DefaultANPACLTier
	case t.IsSameType(libovsdbops.ACLBaselineAdminNetworkPolicy):
		return types.DefaultBANPACLTier
	default:
		return types.DefaultACLTier
	}
}

func GetACLMatch(portGroupName, match string, aclDir ACLDirection) string {
	var aclMatch string
	switch aclDir {
	case ACLIngress:
		aclMatch = "outport == @" + portGroupName
	case ACLEgress:
		aclMatch = "inport == @" + portGroupName
	default:
		panic(fmt.Sprintf("Unknown acl direction %s", aclDir))
	}
	if match != "" {
		aclMatch += " && " + match
	}
	return aclMatch
}

// ACL logging severity levels
type ACLLoggingLevels struct {
	Allow string `json:"allow,omitempty"`
	Deny  string `json:"deny,omitempty"`
	Pass  string `json:"pass,omitempty"`
}

func getLogSeverity(action string, aclLogging *ACLLoggingLevels) (log bool, severity string) {
	severity = ""
	if aclLogging != nil {
		if action == nbdb.ACLActionAllow || action == nbdb.ACLActionAllowRelated || action == nbdb.ACLActionAllowStateless {
			severity = aclLogging.Allow
		} else if action == nbdb.ACLActionDrop || action == nbdb.ACLActionReject {
			severity = aclLogging.Deny
		} else if action == nbdb.ACLActionPass {
			severity = aclLogging.Pass
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

// ACL L4 Match Construct Utils
const (
	// UnspecifiedL4Protocol is used to create ACL for gressPolicy that
	// doesn't have port policies hence no protocols. The value "None" here is
	// used as the value in libovsdbops.PortPolicyProtocolKey DB Index and hence
	// that shouldn't be changed since it will cause a full ACL update during upgrades.
	UnspecifiedL4Protocol = "None"
	// UnspecifiedL4Match is used to create ACL for gressPolicy that
	// doesn't have port policies hence no L4Match. The value "None" here is used
	// as the value of l4MatchACLExtIdKey in acl external_ids_syncer for older ACLs.
	// This value shouldn't be changed.
	UnspecifiedL4Match = "None"
)

// convertK8sProtocolToOVNProtocol returns the OVN syntax-specific protocol value for a v1.Protocol K8s type
func convertK8sProtocolToOVNProtocol(proto v1.Protocol) string {
	var protocol string
	switch proto {
	case v1.ProtocolTCP:
		protocol = "tcp"
	case v1.ProtocolSCTP:
		protocol = "sctp"
	case v1.ProtocolUDP:
		protocol = "udp"
	}
	return protocol
}

// NetworkPolicyPort is an internal representation of
// knet.NetworkPolicyPort and anpapi.AdminNetworkPolicyPort
// in a simpler representation format for the caches
type NetworkPolicyPort struct {
	Protocol string // will store the OVN protocol string syntax for the corresponding K8s protocol
	Port     int32  // will store startPort if its a range
	EndPort  int32
}

// GetNetworkPolicyPort returns an internal NetworkPolicyPort struct
// It also sets the provided protocol, port and endPort fields
func GetNetworkPolicyPort(proto v1.Protocol, port, endPort int32) *NetworkPolicyPort {
	return &NetworkPolicyPort{
		Protocol: convertK8sProtocolToOVNProtocol(proto),
		Port:     port,
		EndPort:  endPort,
	}
}

// gressRulePortsForL4ACLMatch for a given ingress/egress rule in a policy,
// captures all the provided port ranges and individual ports
// so that using this, ACL L4 matches can be generated
type gressRulePortsForL4ACLMatch struct {
	portList  []string // list of provided ports as string
	portRange []string // list of provided port ranges in OVN ACL format
}

func getProtocolPortsMap(rulePorts []*NetworkPolicyPort) map[string]*gressRulePortsForL4ACLMatch {
	gressProtoPortsMap := make(map[string]*gressRulePortsForL4ACLMatch)
	for _, pp := range rulePorts {
		gpp, ok := gressProtoPortsMap[pp.Protocol]
		if !ok {
			gpp = &gressRulePortsForL4ACLMatch{portList: []string{}, portRange: []string{}}
			gressProtoPortsMap[pp.Protocol] = gpp
		}
		if pp.EndPort != 0 && pp.EndPort != pp.Port {
			gpp.portRange = append(gpp.portRange, fmt.Sprintf("%d<=%s.dst<=%d", pp.Port, pp.Protocol, pp.EndPort))
		} else if pp.Port != 0 {
			gpp.portList = append(gpp.portList, fmt.Sprintf("%d", pp.Port))
		}
	}
	return gressProtoPortsMap
}

func getL4Match(protocol string, ports *gressRulePortsForL4ACLMatch) string {
	allL4Matches := []string{}
	if len(ports.portList) > 0 {
		// if there is just one port, then don't use `{}`
		template := "%s.dst==%s"
		if len(ports.portList) > 1 {
			template = "%s.dst=={%s}"
		}
		allL4Matches = append(allL4Matches, fmt.Sprintf(template, protocol, strings.Join(ports.portList, ",")))
	}
	allL4Matches = append(allL4Matches, ports.portRange...)
	l4Match := protocol
	if len(allL4Matches) > 0 {
		template := "%s && %s"
		if len(allL4Matches) > 1 {
			template = "%s && (%s)"
		}
		l4Match = fmt.Sprintf(template, protocol, strings.Join(allL4Matches, " || "))
	}
	return l4Match
}

// GetL4MatchesFromNetworkPolicyPorts accepts a list of NetworkPolicyPorts cache and
// constructs l4Matches for each protocol type
// It returns a map that has protocol as the key and the l4Match as the value
// If len(rulePorts)==0; it returns map["None"] = "None" which means there is no L4 match
func GetL4MatchesFromNetworkPolicyPorts(rulePorts []*NetworkPolicyPort) map[string]string {
	l4Matches := make(map[string]string)
	gressProtoPortsMap := getProtocolPortsMap(rulePorts)
	if len(gressProtoPortsMap) == 0 {
		gressProtoPortsMap[UnspecifiedL4Protocol] = nil
	}
	for protocol, ports := range gressProtoPortsMap {
		l4Match := UnspecifiedL4Match
		if ports != nil {
			l4Match = getL4Match(protocol, ports)
		}
		l4Matches[protocol] = l4Match
	}
	return l4Matches
}

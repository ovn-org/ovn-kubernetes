package adminnetworkpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

var ErrorANPPriorityUnsupported = errors.New("OVNK only supports priority ranges 0-99")
var ANPWithDuplicatePriorityEvent = "ANPWithDuplicatePriority"
var ANPWithUnsupportedPriorityEvent = "ANPWithUnsupportedPriority"

func GetANPPortGroupDbIDs(anpName string, isBanp bool, networkName string) *libovsdbops.DbObjectIDs {
	if networkName == types.DefaultNetworkName {
		// backwards compatibility to not change any of the dbIndexes for default network
		networkName = defaultNetworkControllerName
	}
	idsType := libovsdbops.PortGroupAdminNetworkPolicy
	if isBanp {
		idsType = libovsdbops.PortGroupBaselineAdminNetworkPolicy
	}
	return libovsdbops.NewDbObjectIDs(idsType, networkName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: anpName,
		})
}

func (c *Controller) getANPPortGroupName(anpName, networkName string, isBanp bool) string {
	if networkName == types.DefaultNetworkName {
		// backwards compatibility to not change any of the dbIndexes for default network
		networkName = c.controllerName
	}
	return libovsdbutil.GetPortGroupName(GetANPPortGroupDbIDs(anpName, isBanp, networkName))
}

// getANPRuleACLDbIDs will return the dbObjectIDs for a given rule's ACLs
func getANPRuleACLDbIDs(name, gressPrefix, gressIndex, protocol, networkName string, isBanp bool) *libovsdbops.DbObjectIDs {
	if networkName == types.DefaultNetworkName {
		// backwards compatibility to not change any of the dbIndexes for default network
		networkName = defaultNetworkControllerName
	}
	idType := libovsdbops.ACLAdminNetworkPolicy
	if isBanp {
		idType = libovsdbops.ACLBaselineAdminNetworkPolicy
	}
	return libovsdbops.NewDbObjectIDs(idType, networkName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey:      name,
		libovsdbops.PolicyDirectionKey: gressPrefix,
		// gressidx is the unique id for address set within given objectName and gressPrefix
		libovsdbops.GressIdxKey: gressIndex,
		// protocol key
		libovsdbops.PortPolicyProtocolKey: protocol,
	})
}

// GetACLActionForANPRule returns the corresponding OVN ACL action for a given ANP rule action
func GetACLActionForANPRule(action anpapi.AdminNetworkPolicyRuleAction) string {
	var ovnACLAction string
	switch action {
	case anpapi.AdminNetworkPolicyRuleActionAllow:
		ovnACLAction = nbdb.ACLActionAllowRelated
	case anpapi.AdminNetworkPolicyRuleActionDeny:
		ovnACLAction = nbdb.ACLActionDrop
	case anpapi.AdminNetworkPolicyRuleActionPass:
		ovnACLAction = nbdb.ACLActionPass
	default:
		panic(fmt.Sprintf("Failed to build ANP ACL: unknown acl action %s", action))
	}
	return ovnACLAction
}

// GetACLActionForBANPRule returns the corresponding OVN ACL action for a given BANP rule action
func GetACLActionForBANPRule(action anpapi.BaselineAdminNetworkPolicyRuleAction) string {
	var ovnACLAction string
	switch action {
	case anpapi.BaselineAdminNetworkPolicyRuleActionAllow:
		ovnACLAction = nbdb.ACLActionAllowRelated
	case anpapi.BaselineAdminNetworkPolicyRuleActionDeny:
		ovnACLAction = nbdb.ACLActionDrop
	default:
		panic(fmt.Sprintf("Failed to build BANP ACL: unknown acl action %s", action))
	}
	return ovnACLAction
}

// GetANPPeerAddrSetDbIDs will return the dbObjectIDs for a given rule's address-set
func GetANPPeerAddrSetDbIDs(name, gressPrefix, gressIndex, networkName string, isBanp bool) *libovsdbops.DbObjectIDs {
	if networkName == types.DefaultNetworkName {
		// backwards compatibility to not change any of the dbIndexes for default network
		networkName = defaultNetworkControllerName
	}
	idType := libovsdbops.AddressSetAdminNetworkPolicy
	if isBanp {
		idType = libovsdbops.AddressSetBaselineAdminNetworkPolicy
	}
	return libovsdbops.NewDbObjectIDs(idType, networkName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey:      name,
		libovsdbops.PolicyDirectionKey: gressPrefix,
		// gressidx is the unique id for address set within given objectName and gressPrefix
		libovsdbops.GressIdxKey: gressIndex,
	})
}

func getDirectionFromGressPrefix(gressPrefix string) string {
	if gressPrefix == string(libovsdbutil.ACLIngress) {
		return "src"
	}
	return "dst"
}

// constructMatchFromAddressSet returns the L3Match for an ACL constructed from a gressRule
func constructMatchFromAddressSet(gressPrefix string, addrSetIndex *libovsdbops.DbObjectIDs) string {
	hashedAddressSetNameIPv4, hashedAddressSetNameIPv6 := addressset.GetHashNamesForAS(addrSetIndex)
	var match string
	direction := getDirectionFromGressPrefix(gressPrefix)

	switch {
	case config.IPv4Mode && config.IPv6Mode:
		match = fmt.Sprintf("(ip4.%s == $%s || ip6.%s == $%s)", direction, hashedAddressSetNameIPv4, direction, hashedAddressSetNameIPv6)
	case config.IPv4Mode:
		match = fmt.Sprintf("(ip4.%s == $%s)", direction, hashedAddressSetNameIPv4)
	case config.IPv6Mode:
		match = fmt.Sprintf("(ip6.%s == $%s)", direction, hashedAddressSetNameIPv6)
	}

	return fmt.Sprintf("(%s)", match)
}

// getACLLoggingLevelsForANP takes the ANP's annotations:
// if the "k8s.ovn.org/acl-logging" is set, it parses it
// if parsed values are correct, then it returns those aclLogLevels
// if annotation is not set or parsed values are incorrect/invalid, then it returns empty aclLogLevels which implies logging is disabled
func getACLLoggingLevelsForANP(annotations map[string]string) (*libovsdbutil.ACLLoggingLevels, error) {
	aclLogLevels := &libovsdbutil.ACLLoggingLevels{
		Allow: "", Deny: "", Pass: "",
	}
	annotation, ok := annotations[util.AclLoggingAnnotation]
	if !ok {
		return aclLogLevels, nil
	}
	// If the annotation is "" or "{}", use empty strings. Otherwise, parse the annotation.
	if annotation != "" && annotation != "{}" {
		err := json.Unmarshal([]byte(annotation), aclLogLevels)
		if err != nil {
			// Disable Allow, Deny, Pass logging to ensure idempotency.
			aclLogLevels.Allow = ""
			aclLogLevels.Deny = ""
			aclLogLevels.Pass = ""
			return aclLogLevels, fmt.Errorf("could not unmarshal ANP ACL annotation '%s', disabling logging, err: %q",
				annotation, err)
		}
	}

	// Valid log levels are the various preestablished levels or the empty string.
	validLogLevels := sets.NewString(nbdb.ACLSeverityAlert, nbdb.ACLSeverityWarning, nbdb.ACLSeverityNotice,
		nbdb.ACLSeverityInfo, nbdb.ACLSeverityDebug, "")
	var errs []error
	// Ensure value parsed is valid
	// Set Deny logging.
	if !validLogLevels.Has(aclLogLevels.Deny) {
		errs = append(errs, fmt.Errorf("disabling deny logging due to an invalid deny annotation. "+
			"%q is not a valid log severity", aclLogLevels.Deny))
		aclLogLevels.Deny = ""
	}

	// Set Allow logging.
	if !validLogLevels.Has(aclLogLevels.Allow) {
		errs = append(errs, fmt.Errorf("disabling allow logging due to an invalid allow annotation. "+
			"%q is not a valid log severity", aclLogLevels.Allow))
		aclLogLevels.Allow = ""
	}

	// Set Pass logging.
	if !validLogLevels.Has(aclLogLevels.Pass) {
		errs = append(errs, fmt.Errorf("disabling pass logging due to an invalid pass annotation. "+
			"%q is not a valid log severity", aclLogLevels.Pass))
		aclLogLevels.Pass = ""
	}
	return aclLogLevels, utilerrors.Join(errs...)
}

// convertPodIPContainerPortToNNPP converts the given pod container port and podIPs into a list (max 2 for dualstack)
// of libovsdbutil.NamedNetworkPolicyPort (NNPP)
func convertPodIPContainerPortToNNPP(cPort v1.ContainerPort, podIPs []net.IP) []libovsdbutil.NamedNetworkPolicyPort {
	out := make([]libovsdbutil.NamedNetworkPolicyPort, 0)
	namedPortRep := libovsdbutil.NamedNetworkPolicyPort{
		L4Protocol: libovsdbutil.ConvertK8sProtocolToOVNProtocol(cPort.Protocol),
		L4PodPort:  fmt.Sprintf("%d", cPort.ContainerPort),
	}
	for _, podIP := range podIPs {
		family := ""
		if utilnet.IsIPv4(podIP) {
			family = "ip4"
		} else {
			family = "ip6"
		}
		namedPortRep.L3PodIP = podIP.String()
		namedPortRep.L3PodIPFamily = family
		out = append(out, namedPortRep)
	}
	return out
}

// sortNamedPorts does an in place sort where it arranges the NamedNetworkPolicyPort
// in the provided array in a sorted fashion based on L3PodIP.
// We can use the L3PodIP because it is guaranteed to be unique for a given named port
func sortNamedPorts(namedPortList []libovsdbutil.NamedNetworkPolicyPort) {
	sort.SliceStable(namedPortList, func(i, j int) bool { return namedPortList[i].L3PodIP < namedPortList[j].L3PodIP })
}

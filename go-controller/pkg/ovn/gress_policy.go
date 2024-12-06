package ovn

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	knet "k8s.io/api/networking/v1"
	utilnet "k8s.io/utils/net"
)

const (
	// emptyIdx is used to create ACL for gressPolicy that doesn't have ipBlocks
	emptyIdx = -1
)

type gressPolicy struct {
	controllerName  string
	policyNamespace string
	policyName      string
	policyType      knet.PolicyType
	aclPipeline     libovsdbutil.ACLPipelineType
	idx             int

	// peerVxAddressSets include PodSelectorAddressSet names, and address sets for selected namespaces
	// peerV4AddressSets has Address sets for all namespaces and pod selectors for IPv4
	peerV4AddressSets *sync.Map
	// peerV6AddressSets has Address sets for all namespaces and pod selectors for IPv6
	peerV6AddressSets *sync.Map
	// if gressPolicy has at least 1 rule with selector, set this field to true.
	// This is required to distinguish gress that doesn't have any peerAddressSets added yet
	// (e.g. because there are no namespaces matching label selector) and should allow nothing,
	// from empty gress, which should allow all.
	hasPeerSelector bool

	// portPolicies represents all the ports to which traffic is allowed for
	// the rule in question.
	portPolicies []*libovsdbutil.NetworkPolicyPort

	ipBlocks []*knet.IPBlock

	// set to true for stateless network policies (stateless acls), otherwise set to false
	isNetPolStateless bool

	// supported IP mode
	ipv4Mode bool
	ipv6Mode bool
}

func newGressPolicy(policyType knet.PolicyType, idx int, namespace, name, controllerName string, isNetPolStateless bool, netInfo util.NetInfo) *gressPolicy {
	ipv4Mode, ipv6Mode := netInfo.IPMode()
	return &gressPolicy{
		controllerName:    controllerName,
		policyNamespace:   namespace,
		policyName:        name,
		policyType:        policyType,
		aclPipeline:       libovsdbutil.PolicyTypeToAclPipeline(policyType),
		idx:               idx,
		peerV4AddressSets: &sync.Map{},
		peerV6AddressSets: &sync.Map{},
		portPolicies:      make([]*libovsdbutil.NetworkPolicyPort, 0),
		isNetPolStateless: isNetPolStateless,
		ipv4Mode:          ipv4Mode,
		ipv6Mode:          ipv6Mode,
	}
}

type sortableSlice []string

func (s sortableSlice) Len() int           { return len(s) }
func (s sortableSlice) Less(i, j int) bool { return s[i] < s[j] }
func (s sortableSlice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

func syncMapToSortedList(m *sync.Map) []string {
	result := sortableSlice{}
	m.Range(func(key, _ interface{}) bool {
		result = append(result, key.(string))
		return true
	})
	sort.Sort(result)
	return result
}

func (gp *gressPolicy) addPeerAddressSets(asHashNameV4, asHashNameV6 string) {
	if gp.ipv4Mode && asHashNameV4 != "" {
		gp.peerV4AddressSets.Store("$"+asHashNameV4, true)
	}
	if gp.ipv6Mode && asHashNameV6 != "" {
		gp.peerV6AddressSets.Store("$"+asHashNameV6, true)
	}
}

// If the port is not specified, it implies all ports for that protocol
func (gp *gressPolicy) addPortPolicy(portJSON *knet.NetworkPolicyPort) {
	var pp *libovsdbutil.NetworkPolicyPort
	if portJSON.Port != nil && portJSON.EndPort != nil {
		pp = libovsdbutil.GetNetworkPolicyPort(*portJSON.Protocol, portJSON.Port.IntVal, *portJSON.EndPort)
	} else if portJSON.Port != nil {
		pp = libovsdbutil.GetNetworkPolicyPort(*portJSON.Protocol, portJSON.Port.IntVal, 0)
	} else {
		pp = libovsdbutil.GetNetworkPolicyPort(*portJSON.Protocol, 0, 0)
	}
	gp.portPolicies = append(gp.portPolicies, pp)
}

func (gp *gressPolicy) addIPBlock(ipblockJSON *knet.IPBlock) {
	gp.ipBlocks = append(gp.ipBlocks, ipblockJSON)
}

// getL3MatchFromAddressSet may return empty string, which means that there are no address sets selected for giver
// gressPolicy at the time, and acl should not be created.
func (gp *gressPolicy) getL3MatchFromAddressSet() string {
	v4AddressSets := syncMapToSortedList(gp.peerV4AddressSets)
	v6AddressSets := syncMapToSortedList(gp.peerV6AddressSets)

	// We sort address slice,
	// Hence we'll be constructing the sorted address set string here
	var direction, v4Match, v6Match, match string
	if gp.policyType == knet.PolicyTypeIngress {
		direction = "src"
	} else {
		direction = "dst"
	}

	//  At this point there will be address sets in one or both of them.
	//  Contents in both address sets mean dual stack, else one will be empty because we will only populate
	//  entries for enabled stacks
	if gp.ipv4Mode && len(v4AddressSets) > 0 {
		v4AddressSetStr := strings.Join(v4AddressSets, ", ")
		v4Match = fmt.Sprintf("%s.%s == {%s}", "ip4", direction, v4AddressSetStr)
		match = v4Match
	}
	if gp.ipv6Mode && len(v6AddressSets) > 0 {
		v6AddressSetStr := strings.Join(v6AddressSets, ", ")
		v6Match = fmt.Sprintf("%s.%s == {%s}", "ip6", direction, v6AddressSetStr)
		match = v6Match
	}
	// if at least one match is empty in dualstack mode, the non-empty one will be assigned to match
	if gp.ipv4Mode && gp.ipv6Mode && v4Match != "" && v6Match != "" {
		match = fmt.Sprintf("(%s || %s)", v4Match, v6Match)
	}
	return match
}

func (gp *gressPolicy) allIPsMatch() string {
	switch {
	case gp.ipv4Mode && gp.ipv6Mode:
		return "(ip4 || ip6)"
	case gp.ipv6Mode:
		return "ip6"
	default:
		return "ip4"
	}
}

func (gp *gressPolicy) getMatchFromIPBlock(lportMatch, l4Match string) []string {
	var direction string
	if gp.policyType == knet.PolicyTypeIngress {
		direction = "src"
	} else {
		direction = "dst"
	}
	var matchStrings []string
	var matchStr, ipVersion string
	for _, ipBlock := range gp.ipBlocks {
		if utilnet.IsIPv6CIDRString(ipBlock.CIDR) {
			ipVersion = "ip6"
		} else {
			ipVersion = "ip4"
		}
		if len(ipBlock.Except) == 0 {
			matchStr = fmt.Sprintf("%s.%s == %s", ipVersion, direction, ipBlock.CIDR)
		} else {
			matchStr = fmt.Sprintf("%s.%s == %s && %s.%s != {%s}", ipVersion, direction, ipBlock.CIDR,
				ipVersion, direction, strings.Join(ipBlock.Except, ", "))
		}
		if l4Match == libovsdbutil.UnspecifiedL4Match {
			matchStr = fmt.Sprintf("%s && %s", matchStr, lportMatch)
		} else {
			matchStr = fmt.Sprintf("%s && %s && %s", matchStr, l4Match, lportMatch)
		}
		matchStrings = append(matchStrings, matchStr)
	}
	return matchStrings
}

// addNamespaceAddressSet adds a namespace address set to the gress policy.
// If the address set is not found in the db, return error.
// If the address set is already added for this policy, return false, otherwise returns true.
// This function is safe for concurrent use, doesn't require additional synchronization
func (gp *gressPolicy) addNamespaceAddressSet(name string, asf addressset.AddressSetFactory) (bool, error) {
	dbIDs := getNamespaceAddrSetDbIDs(name, gp.controllerName)
	as, err := asf.GetAddressSet(dbIDs)
	if err != nil {
		return false, fmt.Errorf("cannot add peer namespace %s: failed to get address set: %v", name, err)
	}
	v4HashName, v6HashName := as.GetASHashNames()
	v4HashName = "$" + v4HashName
	v6HashName = "$" + v6HashName

	v4NoUpdate := true
	v6NoUpdate := true
	// only update vXNoUpdate if value was stored and not loaded
	if gp.ipv4Mode {
		_, v4NoUpdate = gp.peerV4AddressSets.LoadOrStore(v4HashName, true)
	}
	if gp.ipv6Mode {
		_, v6NoUpdate = gp.peerV6AddressSets.LoadOrStore(v6HashName, true)
	}
	if v4NoUpdate && v6NoUpdate {
		// no changes were applied, return false
		return false, nil
	}
	return true, nil
}

// delNamespaceAddressSet removes a namespace address set from the gress policy.
// If the address set is already deleted for this policy, return false, otherwise returns true.
// This function is safe for concurrent use, doesn't require additional synchronization
func (gp *gressPolicy) delNamespaceAddressSet(name string) bool {
	dbIDs := getNamespaceAddrSetDbIDs(name, gp.controllerName)
	v4HashName, v6HashName := addressset.GetHashNamesForAS(dbIDs)
	v4HashName = "$" + v4HashName
	v6HashName = "$" + v6HashName

	v4Update := false
	v6Update := false
	// only update vXUpdate if value was loaded
	if gp.ipv4Mode {
		_, v4Update = gp.peerV4AddressSets.LoadAndDelete(v4HashName)
	}
	if gp.ipv6Mode {
		_, v6Update = gp.peerV6AddressSets.LoadAndDelete(v6HashName)
	}
	if v4Update || v6Update {
		// at least 1 address set was updated, return true
		return true
	}
	return false
}

func (gp *gressPolicy) isEmpty() bool {
	// empty gress allows all, it is empty if there are no ipBlocks and no peerSelectors
	return !gp.hasPeerSelector && len(gp.ipBlocks) == 0
}

// buildLocalPodACLs builds the ACLs that implement the gress policy's rules to the
// given Port Group (which should contain all pod logical switch ports selected
// by the parent NetworkPolicy)
// buildLocalPodACLs is safe for concurrent use, since it only uses gressPolicy fields that don't change
// since creation, or are safe for concurrent use like peerVXAddressSets
func (gp *gressPolicy) buildLocalPodACLs(portGroupName string, aclLogging *libovsdbutil.ACLLoggingLevels) (createdACLs []*nbdb.ACL,
	skippedACLs []*nbdb.ACL) {
	var lportMatch string
	if gp.policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", portGroupName)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", portGroupName)
	}
	action := nbdb.ACLActionAllowRelated
	if gp.isNetPolStateless {
		action = nbdb.ACLActionAllowStateless
	}
	for protocol, l4Match := range libovsdbutil.GetL4MatchesFromNetworkPolicyPorts(gp.portPolicies) {
		if len(gp.ipBlocks) > 0 {
			// Add ACL allow rule for IPBlock CIDR
			ipBlockMatches := gp.getMatchFromIPBlock(lportMatch, l4Match)
			for ipBlockIdx, ipBlockMatch := range ipBlockMatches {
				aclIDs := gp.getNetpolACLDbIDs(ipBlockIdx, protocol)
				acl := libovsdbutil.BuildACL(aclIDs, types.DefaultAllowPriority, ipBlockMatch, action,
					aclLogging, gp.aclPipeline)
				createdACLs = append(createdACLs, acl)
			}
		}
		// if there are pod/namespace selector, then allow packets from/to that address_set or
		// if the NetworkPolicyPeer is empty, then allow from all sources or to all destinations.
		if gp.hasPeerSelector || gp.isEmpty() {
			var l3Match, addrSetMatch string
			if gp.isEmpty() {
				l3Match = gp.allIPsMatch()
			} else {
				l3Match = gp.getL3MatchFromAddressSet()
			}

			if l4Match == libovsdbutil.UnspecifiedL4Match {
				addrSetMatch = fmt.Sprintf("%s && %s", l3Match, lportMatch)
			} else {
				addrSetMatch = fmt.Sprintf("%s && %s && %s", l3Match, l4Match, lportMatch)
			}
			aclIDs := gp.getNetpolACLDbIDs(emptyIdx, protocol)
			acl := libovsdbutil.BuildACL(aclIDs, types.DefaultAllowPriority, addrSetMatch, action,
				aclLogging, gp.aclPipeline)
			if l3Match == "" {
				// if l3Match is empty, then no address sets are selected for a given gressPolicy.
				// fortunately l3 match is not a part of externalIDs, that means that we can find
				// the deleted acl without knowing the previous match.
				skippedACLs = append(skippedACLs, acl)
			} else {
				createdACLs = append(createdACLs, acl)
			}
		}
	}
	return
}

func (gp *gressPolicy) getNetpolACLDbIDs(ipBlockIdx int, protocol string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetworkPolicy, gp.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			// policy namespace+name
			libovsdbops.ObjectNameKey: libovsdbops.BuildNamespaceNameKey(gp.policyNamespace, gp.policyName),
			// egress or ingress
			libovsdbops.PolicyDirectionKey: string(gp.policyType),
			// gress rule index
			libovsdbops.GressIdxKey: strconv.Itoa(gp.idx),
			// acls are created for every gp.portPolicies which are grouped by protocol:
			// - for empty policy (no selectors and no ip blocks) - empty ACL
			// OR
			// - all selector-based peers ACL
			// - for every IPBlock +1 ACL
			// Therefore unique id for a given gressPolicy is protocol name + IPBlock idx
			// (protocol will be "None" if no port policy is defined, and empty policy and all
			// selector-based peers ACLs will have idx=-1)
			libovsdbops.IpBlockIndexKey: strconv.Itoa(ipBlockIdx),
			// protocol key
			libovsdbops.PortPolicyProtocolKey: protocol,
		})
}

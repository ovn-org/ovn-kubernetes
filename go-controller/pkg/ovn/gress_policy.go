package ovn

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	knet "k8s.io/api/networking/v1"
	utilnet "k8s.io/utils/net"
)

type gressPolicy struct {
	controllerName  string
	policyNamespace string
	policyName      string
	policyType      knet.PolicyType
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
	portPolicies []*portPolicy

	ipBlock []*knet.IPBlock
}

type portPolicy struct {
	protocol string
	port     int32
	endPort  int32
}

func (pp *portPolicy) getL4Match() (string, error) {
	var supportedProtocols = []string{TCP, UDP, SCTP}
	var foundProtocol string
	for _, protocol := range supportedProtocols {
		if protocol == pp.protocol {
			foundProtocol = strings.ToLower(pp.protocol)
			break
		}
	}
	if len(foundProtocol) == 0 {
		return "", fmt.Errorf("unknown port protocol %v", pp.protocol)
	}
	if pp.endPort != 0 && pp.endPort != pp.port {
		return fmt.Sprintf("%s && %d<=%s.dst<=%d", foundProtocol, pp.port, foundProtocol, pp.endPort), nil

	} else if pp.port != 0 {
		return fmt.Sprintf("%s && %s.dst==%d", foundProtocol, foundProtocol, pp.port), nil
	}
	return foundProtocol, nil
}

func newGressPolicy(policyType knet.PolicyType, idx int, namespace, name, controllerName string) *gressPolicy {
	return &gressPolicy{
		controllerName:    controllerName,
		policyNamespace:   namespace,
		policyName:        name,
		policyType:        policyType,
		idx:               idx,
		peerV4AddressSets: &sync.Map{},
		peerV6AddressSets: &sync.Map{},
		portPolicies:      make([]*portPolicy, 0),
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
	if config.IPv4Mode && asHashNameV4 != "" {
		gp.peerV4AddressSets.Store("$"+asHashNameV4, true)
	}
	if config.IPv6Mode && asHashNameV6 != "" {
		gp.peerV6AddressSets.Store("$"+asHashNameV6, true)
	}
}

// If the port is not specified, it implies all ports for that protocol
func (gp *gressPolicy) addPortPolicy(portJSON *knet.NetworkPolicyPort) {
	pp := &portPolicy{protocol: string(*portJSON.Protocol),
		port:    0,
		endPort: 0,
	}
	if portJSON.Port != nil {
		pp.port = portJSON.Port.IntVal
	}
	if portJSON.EndPort != nil {
		pp.endPort = *portJSON.EndPort
	}
	gp.portPolicies = append(gp.portPolicies, pp)
}

func (gp *gressPolicy) addIPBlock(ipblockJSON *knet.IPBlock) {
	gp.ipBlock = append(gp.ipBlock, ipblockJSON)
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
	if config.IPv4Mode && len(v4AddressSets) > 0 {
		v4AddressSetStr := strings.Join(v4AddressSets, ", ")
		v4Match = fmt.Sprintf("%s.%s == {%s}", "ip4", direction, v4AddressSetStr)
		if gp.policyType == knet.PolicyTypeIngress {
			v4Match = fmt.Sprintf("(%s || (%s.src == %s && %s.dst == {%s}))", v4Match, "ip4",
				types.V4OVNServiceHairpinMasqueradeIP, "ip4", v4AddressSetStr)
		}
		match = v4Match
	}
	if config.IPv6Mode && len(v6AddressSets) > 0 {
		v6AddressSetStr := strings.Join(v6AddressSets, ", ")
		v6Match = fmt.Sprintf("%s.%s == {%s}", "ip6", direction, v6AddressSetStr)
		if gp.policyType == knet.PolicyTypeIngress {
			v6Match = fmt.Sprintf("(%s || (%s.src == %s && %s.dst == {%s}))", v6Match, "ip6",
				types.V6OVNServiceHairpinMasqueradeIP, "ip6", v6AddressSetStr)
		}
		match = v6Match
	}
	// if at least one match is empty in dualstack mode, the non-empty one will be assigned to match
	if config.IPv4Mode && config.IPv6Mode && v4Match != "" && v6Match != "" {
		match = fmt.Sprintf("(%s || %s)", v4Match, v6Match)
	}
	return match
}

func constructEmptyMatchString() string {
	switch {
	case config.IPv4Mode && config.IPv6Mode:
		return "(ip4 || ip6)"
	case config.IPv6Mode:
		return "ip6"
	default:
		return "ip4"
	}
}

func (gp *gressPolicy) getMatchFromIPBlock(lportMatch, l4Match string) []string {
	var ipBlockMatches []string
	if gp.policyType == knet.PolicyTypeIngress {
		ipBlockMatches = constructIPBlockStringsForACL("src", gp.ipBlock, lportMatch, l4Match)
	} else {
		ipBlockMatches = constructIPBlockStringsForACL("dst", gp.ipBlock, lportMatch, l4Match)
	}
	return ipBlockMatches
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
	if config.IPv4Mode {
		_, v4NoUpdate = gp.peerV4AddressSets.LoadOrStore(v4HashName, true)
	}
	if config.IPv6Mode {
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
	if config.IPv4Mode {
		_, v4Update = gp.peerV4AddressSets.LoadAndDelete(v4HashName)
	}
	if config.IPv6Mode {
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
	return !gp.hasPeerSelector && len(gp.ipBlock) == 0
}

// buildLocalPodACLs builds the ACLs that implement the gress policy's rules to the
// given Port Group (which should contain all pod logical switch ports selected
// by the parent NetworkPolicy)
// buildLocalPodACLs is safe for concurrent use, since it only uses gressPolicy fields that don't change
// since creation, or are safe for concurrent use like peerVXAddressSets
func (gp *gressPolicy) buildLocalPodACLs(portGroupName string, aclLogging *ACLLoggingLevels) (createdACLs []*nbdb.ACL,
	skippedACLs []*nbdb.ACL) {
	var lportMatch, match, l3Match string
	if gp.policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", portGroupName)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", portGroupName)
	}

	var l4Matches []string
	if len(gp.portPolicies) == 0 {
		l4Matches = append(l4Matches, noneMatch)
	} else {
		l4Matches = make([]string, 0, len(gp.portPolicies))
		for _, port := range gp.portPolicies {
			l4Match, err := port.getL4Match()
			if err != nil {
				continue
			}
			l4Matches = append(l4Matches, l4Match)
		}
	}
	for _, l4Match := range l4Matches {
		if len(gp.ipBlock) > 0 {
			// Add ACL allow rule for IPBlock CIDR
			cidrMatches := gp.getMatchFromIPBlock(lportMatch, l4Match)
			for i, cidrMatch := range cidrMatches {
				acl := gp.buildACLAllow(cidrMatch, l4Match, i+1, aclLogging)
				createdACLs = append(createdACLs, acl)
			}
		}
		// if there are pod/namespace selector, then allow packets from/to that address_set or
		// if the NetworkPolicyPeer is empty, then allow from all sources or to all destinations.
		if gp.hasPeerSelector || gp.isEmpty() {
			if gp.isEmpty() {
				l3Match = constructEmptyMatchString()
			} else {
				l3Match = gp.getL3MatchFromAddressSet()
			}

			if l4Match == noneMatch {
				match = fmt.Sprintf("%s && %s", l3Match, lportMatch)
			} else {
				match = fmt.Sprintf("%s && %s && %s", l3Match, l4Match, lportMatch)
			}
			acl := gp.buildACLAllow(match, l4Match, 0, aclLogging)
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

func getGressPolicyACLName(ns, policyName string, idx int) string {
	return joinACLName(ns, policyName, fmt.Sprintf("%v", idx))
}

// buildACLAllow builds an allow-related ACL for a given match
func (gp *gressPolicy) buildACLAllow(match, l4Match string, ipBlockCIDR int, aclLogging *ACLLoggingLevels) *nbdb.ACL {
	aclT := policyTypeToAclType(gp.policyType)
	priority := types.DefaultAllowPriority
	action := nbdb.ACLActionAllowRelated
	aclName := getGressPolicyACLName(gp.policyNamespace, gp.policyName, gp.idx)

	// For backward compatibility with existing ACLs, we use "ipblock_cidr=false" for
	// non-ipblock ACLs and "ipblock_cidr=true" for the first ipblock ACL in a policy,
	// but then number them after that.
	var ipBlockCIDRString string
	switch ipBlockCIDR {
	case 0:
		ipBlockCIDRString = "false"
	case 1:
		ipBlockCIDRString = "true"
	default:
		ipBlockCIDRString = strconv.FormatInt(int64(ipBlockCIDR), 10)
	}

	policyTypeNum := fmt.Sprintf(policyTypeNumACLExtIdKey, gp.policyType)
	policyTypeIndex := strconv.FormatInt(int64(gp.idx), 10)

	externalIds := map[string]string{
		l4MatchACLExtIdKey:     l4Match,
		ipBlockCIDRACLExtIdKey: ipBlockCIDRString,
		namespaceACLExtIdKey:   gp.policyNamespace,
		policyACLExtIdKey:      gp.policyName,
		policyTypeACLExtIdKey:  string(gp.policyType),
		policyTypeNum:          policyTypeIndex,
	}
	acl := BuildACL(aclName, priority, match, action, aclLogging, aclT, externalIds)
	return acl
}

func constructIPBlockStringsForACL(direction string, ipBlocks []*knet.IPBlock, lportMatch, l4Match string) []string {
	var matchStrings []string
	var matchStr, ipVersion string
	for _, ipBlock := range ipBlocks {
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
		if l4Match == noneMatch {
			matchStr = fmt.Sprintf("%s && %s", matchStr, lportMatch)
		} else {
			matchStr = fmt.Sprintf("%s && %s && %s", matchStr, l4Match, lportMatch)
		}
		matchStrings = append(matchStrings, matchStr)
	}
	return matchStrings
}

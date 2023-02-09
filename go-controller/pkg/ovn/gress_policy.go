package ovn

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type gressPolicy struct {
	policyNamespace string
	policyName      string
	policyType      knet.PolicyType
	idx             int

	// peerAddressSet points to the addressSet that holds all peer pod or peer service
	// IP addresess.
	// used for pod, pod with namespace, and svc peer selectors
	// This addressSet is synced by networkPolicy lock:
	// ensurePeerAddressSet and destroy functions need to hold networkPolicy write lock,
	// and function that modify addressSet need to hold networkPolicy read lock.
	// Adding/deleting ips to/from address set is safe for concurrent use.
	peerAddressSet addressset.AddressSet

	// peerVxAddressSets includes gressPolicy.peerAddressSet, and address sets for selected namespaces
	// peerV4AddressSets has Address sets for all namespaces and pod selectors for IPv4
	peerV4AddressSets *sync.Map
	// peerV6AddressSets has Address sets for all namespaces and pod selectors for IPv6
	peerV6AddressSets *sync.Map

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

func newGressPolicy(policyType knet.PolicyType, idx int, namespace, name string) *gressPolicy {
	return &gressPolicy{
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

func syncMapLen(m *sync.Map) int {
	result := 0
	m.Range(func(_, _ interface{}) bool {
		result++
		return true
	})
	return result
}

// must be called with network policy write lock
func (gp *gressPolicy) ensurePeerAddressSet(factory addressset.AddressSetFactory) error {
	if gp.peerAddressSet != nil {
		return nil
	}

	direction := strings.ToLower(string(gp.policyType))
	asName := fmt.Sprintf("%s.%s.%s.%d", gp.policyNamespace, gp.policyName, direction, gp.idx)
	as, err := factory.NewAddressSet(asName, nil)
	if err != nil {
		return err
	}

	gp.peerAddressSet = as
	ipv4HashedAS, ipv6HashedAS := as.GetASHashNames()
	if ipv4HashedAS != "" {
		gp.peerV4AddressSets.Store("$"+ipv4HashedAS, true)
	}
	if ipv6HashedAS != "" {
		gp.peerV6AddressSets.Store("$"+ipv6HashedAS, true)
	}
	return nil
}

// addPeerPods will get all currently assigned ips for given pods, and add them to the address set.
// If pod ips change, this function should be called again.
// must be called with network policy read lock
func (gp *gressPolicy) addPeerPods(pods ...*v1.Pod) error {
	if gp.peerAddressSet == nil {
		return fmt.Errorf("peer AddressSet is nil, cannot add peer pod(s): for gressPolicy: %s",
			gp.policyName)
	}

	podIPFactor := 1
	if config.IPv4Mode && config.IPv6Mode {
		podIPFactor = 2
	}
	ips := make([]net.IP, 0, len(pods)*podIPFactor)
	for _, pod := range pods {
		podIPs, err := util.GetPodIPsOfNetwork(pod, &util.DefaultNetInfo{})
		if err != nil {
			return err
		}
		ips = append(ips, podIPs...)
	}

	return gp.peerAddressSet.AddIPs(ips)
}

// must be called with network policy read lock
func (gp *gressPolicy) deletePeerPod(pod *v1.Pod) error {
	ips, err := util.GetPodIPsOfNetwork(pod, &util.DefaultNetInfo{})
	if err != nil {
		// if pod ips can't be fetched on delete, we don't expect that information about ips will ever be updated,
		// therefore just log the error and return.
		klog.Infof("Failed to get pod IPs %s/%s to delete from network policy peers: %w", pod.Namespace, pod.Name, err)
		return nil
	}
	return gp.peerAddressSet.DeleteIPs(ips)
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

func (gp *gressPolicy) getL3MatchFromAddressSet() string {
	var l3Match string
	v4List := syncMapToSortedList(gp.peerV4AddressSets)
	v6List := syncMapToSortedList(gp.peerV6AddressSets)

	if len(v4List) == 0 && len(v6List) == 0 {
		l3Match = constructEmptyMatchString()
	} else {
		// We sort address slice,
		// Hence we'll be constructing the sorted address set string here
		l3Match = gp.constructMatchString(v4List, v6List)
	}
	return l3Match
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

func (gp *gressPolicy) constructMatchString(v4AddressSets, v6AddressSets []string) string {
	var direction, v4MatchStr, v6MatchStr, matchStr string
	if gp.policyType == knet.PolicyTypeIngress {
		direction = "src"
	} else {
		direction = "dst"
	}

	//  At this point there will be address sets in one or both of them.
	//  Contents in both address sets mean dual stack, else one will be empty because we will only populate
	//  entries for enabled stacks
	if len(v4AddressSets) > 0 {
		v4AddressSetStr := strings.Join(v4AddressSets, ", ")
		v4MatchStr = fmt.Sprintf("%s.%s == {%s}", "ip4", direction, v4AddressSetStr)
		if gp.policyType == knet.PolicyTypeIngress {
			v4MatchStr = fmt.Sprintf("(%s || (%s.src == %s && %s.dst == {%s}))", v4MatchStr, "ip4", types.V4OVNServiceHairpinMasqueradeIP, "ip4", v4AddressSetStr)
		}
		matchStr = v4MatchStr
	}
	if len(v6AddressSets) > 0 {
		v6AddressSetStr := strings.Join(v6AddressSets, ", ")
		v6MatchStr = fmt.Sprintf("%s.%s == {%s}", "ip6", direction, v6AddressSetStr)
		if gp.policyType == knet.PolicyTypeIngress {
			v6MatchStr = fmt.Sprintf("(%s || (%s.src == %s && %s.dst == {%s}))", v6MatchStr, "ip6", types.V6OVNServiceHairpinMasqueradeIP, "ip6", v6AddressSetStr)
		}
		matchStr = v6MatchStr
	}
	if len(v4AddressSets) > 0 && len(v6AddressSets) > 0 {
		matchStr = fmt.Sprintf("(%s || %s)", v4MatchStr, v6MatchStr)
	}
	return matchStr
}

func (gp *gressPolicy) sizeOfAddressSet() int {
	return syncMapLen(gp.peerV4AddressSets) + syncMapLen(gp.peerV6AddressSets)
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

// addNamespaceAddressSet adds a namespace address set to the gress policy
// if the address set does not exist and returns `true`;  if the address set already exists,
// it returns `false`.
// This function is safe for concurrent use, doesn't require additional synchronization
func (gp *gressPolicy) addNamespaceAddressSet(name string) bool {
	v4HashName, v6HashName := addressset.MakeAddressSetHashNames(name)
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
		return false
	}
	return true
}

// delNamespaceAddressSet removes a namespace address set from the gress policy
// and returns whether the address set was in the policy or not.
// This function is safe for concurrent use, doesn't require additional synchronization
func (gp *gressPolicy) delNamespaceAddressSet(name string) bool {
	v4HashName, v6HashName := addressset.MakeAddressSetHashNames(name)
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

// buildLocalPodACLs builds the ACLs that implement the gress policy's rules to the
// given Port Group (which should contain all pod logical switch ports selected
// by the parent NetworkPolicy)
// buildLocalPodACLs is safe for concurrent use, since it only uses gressPolicy fields that don't change
// since creation, or are safe for concurrent use like peerVXAddressSets
func (gp *gressPolicy) buildLocalPodACLs(portGroupName string, aclLogging *ACLLoggingLevels) []*nbdb.ACL {
	l3Match := gp.getL3MatchFromAddressSet()
	var lportMatch string
	var cidrMatches []string
	if gp.policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", portGroupName)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", portGroupName)
	}

	acls := []*nbdb.ACL{}
	if len(gp.portPolicies) == 0 {
		match := fmt.Sprintf("%s && %s", l3Match, lportMatch)
		l4Match := noneMatch

		if len(gp.ipBlock) > 0 {
			// Add ACL allow rule for IPBlock CIDR
			cidrMatches = gp.getMatchFromIPBlock(lportMatch, l4Match)
			for i, cidrMatch := range cidrMatches {
				acl := gp.buildACLAllow(cidrMatch, l4Match, i+1, aclLogging)
				acls = append(acls, acl)
			}
		}
		// if there are pod/namespace selector, then allow packets from/to that address_set or
		// if the NetworkPolicyPeer is empty, then allow from all sources or to all destinations.
		if gp.sizeOfAddressSet() > 0 || len(gp.ipBlock) == 0 {
			acl := gp.buildACLAllow(match, l4Match, 0, aclLogging)
			acls = append(acls, acl)
		}
	}
	for _, port := range gp.portPolicies {
		l4Match, err := port.getL4Match()
		if err != nil {
			continue
		}
		match := fmt.Sprintf("%s && %s && %s", l3Match, l4Match, lportMatch)
		if len(gp.ipBlock) > 0 {
			// Add ACL allow rule for IPBlock CIDR
			cidrMatches = gp.getMatchFromIPBlock(lportMatch, l4Match)
			for i, cidrMatch := range cidrMatches {
				acl := gp.buildACLAllow(cidrMatch, l4Match, i+1, aclLogging)
				acls = append(acls, acl)
			}
		}
		if gp.sizeOfAddressSet() > 0 || len(gp.ipBlock) == 0 {
			acl := gp.buildACLAllow(match, l4Match, 0, aclLogging)
			acls = append(acls, acl)
		}
	}

	return acls
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

// must be called with network policy write lock
func (gp *gressPolicy) destroy() error {
	if gp.peerAddressSet != nil {
		if err := gp.peerAddressSet.Destroy(); err != nil {
			return err
		}
		gp.peerAddressSet = nil
	}
	return nil
}

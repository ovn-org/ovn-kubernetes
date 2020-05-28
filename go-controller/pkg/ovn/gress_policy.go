package ovn

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"
)

type gressPolicy struct {
	policyNamespace string
	policyName      string
	policyType      knet.PolicyType
	idx             int

	// peerAddressSet points to the addressSet that holds all peer pod
	// IP addresess.
	peerAddressSet AddressSet

	// nsAddressSets holds the names of all namespace address sets
	nsAddressSets sets.String

	// sortedPeerAddressSets has the sorted peerAddressSets
	sortedPeerAddressSets []string

	// portPolicies represents all the ports to which traffic is allowed for
	// the rule in question.
	portPolicies []*portPolicy

	// ipBlockCidr represents the CIDR from which traffic is allowed
	// except the IP block in the except, which should be dropped.
	ipBlockCidr   []string
	ipBlockExcept []string
}

type portPolicy struct {
	protocol string
	port     int32
}

func (pp *portPolicy) getL4Match() (string, error) {
	if pp.protocol == TCP {
		return fmt.Sprintf("tcp && tcp.dst==%d", pp.port), nil
	} else if pp.protocol == UDP {
		return fmt.Sprintf("udp && udp.dst==%d", pp.port), nil
	} else if pp.protocol == SCTP {
		return fmt.Sprintf("sctp && sctp.dst==%d", pp.port), nil
	}
	return "", fmt.Errorf("unknown port protocol %v", pp.protocol)
}

func newGressPolicy(policyType knet.PolicyType, idx int, namespace, name string) *gressPolicy {
	return &gressPolicy{
		policyNamespace:       namespace,
		policyName:            name,
		policyType:            policyType,
		idx:                   idx,
		nsAddressSets:         sets.String{},
		sortedPeerAddressSets: make([]string, 0),
		portPolicies:          make([]*portPolicy, 0),
		ipBlockCidr:           make([]string, 0),
		ipBlockExcept:         make([]string, 0),
	}
}

func (gp *gressPolicy) ensurePeerAddressSet(factory AddressSetFactory) error {
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
	gp.sortedPeerAddressSets = append(gp.sortedPeerAddressSets, as.GetHashName())
	sort.Strings(gp.sortedPeerAddressSets)
	return nil
}

func (gp *gressPolicy) addPeerPod(pod *v1.Pod) error {
	ips, err := util.GetAllPodIPs(pod)
	if err != nil {
		return err
	}
	// FIXME dual-stack
	return gp.peerAddressSet.AddIP(ips[0])
}

func (gp *gressPolicy) deletePeerPod(pod *v1.Pod) error {
	ips, err := util.GetAllPodIPs(pod)
	if err != nil {
		return err
	}
	// FIXME dual-stack
	return gp.peerAddressSet.DeleteIP(ips[0])
}

func (gp *gressPolicy) addPortPolicy(portJSON *knet.NetworkPolicyPort) {
	gp.portPolicies = append(gp.portPolicies, &portPolicy{
		protocol: string(*portJSON.Protocol),
		port:     portJSON.Port.IntVal,
	})
}

func (gp *gressPolicy) addIPBlock(ipblockJSON *knet.IPBlock) {
	gp.ipBlockCidr = append(gp.ipBlockCidr, ipblockJSON.CIDR)
	gp.ipBlockExcept = append(gp.ipBlockExcept, ipblockJSON.Except...)
}

func ipMatch() string {
	if config.IPv6Mode {
		return "ip6"
	}
	return "ip4"
}

func (gp *gressPolicy) getL3MatchFromAddressSet() string {
	addressSets := make([]string, 0, len(gp.sortedPeerAddressSets))
	for _, hashName := range gp.sortedPeerAddressSets {
		addressSets = append(addressSets, fmt.Sprintf("$%s", hashName))
	}

	var l3Match string
	if len(addressSets) == 0 {
		l3Match = ipMatch()
	} else {
		addresses := strings.Join(addressSets, ", ")
		if gp.policyType == knet.PolicyTypeIngress {
			l3Match = fmt.Sprintf("%s.src == {%s}", ipMatch(), addresses)
		} else {
			l3Match = fmt.Sprintf("%s.dst == {%s}", ipMatch(), addresses)
		}
	}
	return l3Match
}

func (gp *gressPolicy) getMatchFromIPBlock(lportMatch, l4Match string) string {
	var match string
	ipBlockCidr := fmt.Sprintf("{%s}", strings.Join(gp.ipBlockCidr, ", "))
	if gp.policyType == knet.PolicyTypeIngress {
		if l4Match == noneMatch {
			match = fmt.Sprintf("match=\"%s.src == %s && %s\"",
				ipMatch(), ipBlockCidr, lportMatch)
		} else {
			match = fmt.Sprintf("match=\"%s.src == %s && %s && %s\"",
				ipMatch(), ipBlockCidr, l4Match, lportMatch)
		}
	} else {
		if l4Match == noneMatch {
			match = fmt.Sprintf("match=\"%s.dst == %s && %s\"",
				ipMatch(), ipBlockCidr, lportMatch)
		} else {
			match = fmt.Sprintf("match=\"%s.dst == %s && %s && %s\"",
				ipMatch(), ipBlockCidr, l4Match, lportMatch)
		}
	}
	return match
}

// addNamespaceAddressSet adds a new namespace address set to the gress policy
func (gp *gressPolicy) addNamespaceAddressSet(name, portGroupName string) {
	hashName := hashedAddressSet(name)
	if gp.nsAddressSets.Has(hashName) {
		return
	}

	oldL3Match := gp.getL3MatchFromAddressSet()
	gp.nsAddressSets.Insert(hashName)
	gp.sortedPeerAddressSets = append(gp.sortedPeerAddressSets, hashName)
	sort.Strings(gp.sortedPeerAddressSets)
	gp.localPodUpdateACL(oldL3Match, gp.getL3MatchFromAddressSet(), portGroupName)
}

// delNamespaceAddressSet removes a namespace address set from the gress policy
func (gp *gressPolicy) delNamespaceAddressSet(name, portGroupName string) {
	hashName := hashedAddressSet(name)
	if !gp.nsAddressSets.Has(hashName) {
		return
	}

	oldL3Match := gp.getL3MatchFromAddressSet()
	for i, addressSet := range gp.sortedPeerAddressSets {
		if addressSet == hashName {
			gp.sortedPeerAddressSets = append(
				gp.sortedPeerAddressSets[:i],
				gp.sortedPeerAddressSets[i+1:]...)
			break
		}
	}
	gp.nsAddressSets.Delete(hashName)
	gp.localPodUpdateACL(oldL3Match, gp.getL3MatchFromAddressSet(), portGroupName)
}

// localPodAddACL adds an ACL that implements the gress policy's rules to the
// given Port Group (which should contain all pod logical switch ports selected
// by the parent NetworkPolicy)
func (gp *gressPolicy) localPodAddACL(portGroupName, portGroupUUID string) {
	l3Match := gp.getL3MatchFromAddressSet()

	var lportMatch, cidrMatch string
	if gp.policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", portGroupName)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", portGroupName)
	}

	// If IPBlock CIDR is not empty and except string [] is not empty,
	// add deny acl rule with priority ipBlockDenyPriority (1010).
	if len(gp.ipBlockCidr) > 0 && len(gp.ipBlockExcept) > 0 {
		except := fmt.Sprintf("{%s}", strings.Join(gp.ipBlockExcept, ", "))
		if err := gp.addIPBlockACLDeny(except, ipBlockDenyPriority, portGroupName, portGroupUUID); err != nil {
			klog.Warningf(err.Error())
		}
	}

	if len(gp.portPolicies) == 0 {
		match := fmt.Sprintf("match=\"%s && %s\"", l3Match, lportMatch)
		l4Match := noneMatch

		if len(gp.ipBlockCidr) > 0 {
			// Add ACL allow rule for IPBlock CIDR
			cidrMatch = gp.getMatchFromIPBlock(lportMatch, l4Match)
			if err := gp.addACLAllow(cidrMatch, l4Match, portGroupUUID, true); err != nil {
				klog.Warningf(err.Error())
			}
		}
		// if there are pod/namespace selector, then allow packets from/to that address_set or
		// if the NetworkPolicyPeer is empty, then allow from all sources or to all destinations.
		if len(gp.sortedPeerAddressSets) > 0 || len(gp.ipBlockCidr) == 0 {
			if err := gp.addACLAllow(match, l4Match, portGroupUUID, false); err != nil {
				klog.Warningf(err.Error())
			}
		}
	}
	for _, port := range gp.portPolicies {
		l4Match, err := port.getL4Match()
		if err != nil {
			continue
		}
		match := fmt.Sprintf("match=\"%s && %s && %s\"", l3Match, l4Match, lportMatch)
		if len(gp.ipBlockCidr) > 0 {
			// Add ACL allow rule for IPBlock CIDR
			cidrMatch = gp.getMatchFromIPBlock(lportMatch, l4Match)
			if err := gp.addACLAllow(cidrMatch, l4Match, portGroupUUID, true); err != nil {
				klog.Warningf(err.Error())
			}
		}
		if len(gp.sortedPeerAddressSets) > 0 || len(gp.ipBlockCidr) == 0 {
			if err := gp.addACLAllow(match, l4Match, portGroupUUID, false); err != nil {
				klog.Warningf(err.Error())
			}
		}
	}
}

// addACLAllow adds an "allow" ACL with a given match to the given Port Group
func (gp *gressPolicy) addACLAllow(match, l4Match, portGroupUUID string, ipBlockCidr bool) error {
	var direction, action string
	direction = toLport
	if gp.policyType == knet.PolicyTypeIngress {
		action = "allow-related"
	} else {
		action = "allow"
	}

	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:l4Match=\"%s\"", l4Match),
		fmt.Sprintf("external-ids:ipblock_cidr=%t", ipBlockCidr),
		fmt.Sprintf("external-ids:namespace=%s", gp.policyNamespace),
		fmt.Sprintf("external-ids:policy=%s", gp.policyName),
		fmt.Sprintf("external-ids:%s_num=%d", gp.policyType, gp.idx),
		fmt.Sprintf("external-ids:policy_type=%s", gp.policyType))
	if err != nil {
		return fmt.Errorf("find failed to get the allow rule for "+
			"namespace=%s, policy=%s, stderr: %q (%v)",
			gp.policyNamespace, gp.policyName, stderr, err)
	}

	if uuid != "" {
		return nil
	}

	_, stderr, err = util.RunOVNNbctl("--id=@acl", "create",
		"acl", fmt.Sprintf("priority=%s", defaultAllowPriority),
		fmt.Sprintf("direction=%s", direction), match,
		fmt.Sprintf("action=%s", action),
		fmt.Sprintf("external-ids:l4Match=\"%s\"", l4Match),
		fmt.Sprintf("external-ids:ipblock_cidr=%t", ipBlockCidr),
		fmt.Sprintf("external-ids:namespace=%s", gp.policyNamespace),
		fmt.Sprintf("external-ids:policy=%s", gp.policyName),
		fmt.Sprintf("external-ids:%s_num=%d", gp.policyType, gp.idx),
		fmt.Sprintf("external-ids:policy_type=%s", gp.policyType),
		"--", "add", "port_group", portGroupUUID, "acls", "@acl")
	if err != nil {
		return fmt.Errorf("failed to create the acl allow rule for "+
			"namespace=%s, policy=%s, stderr: %q (%v)", gp.policyNamespace,
			gp.policyName, stderr, err)
	}

	return nil
}

// modifyACLAllow updates an "allow" ACL with a new match
func (gp *gressPolicy) modifyACLAllow(oldMatch, newMatch string) error {
	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", oldMatch,
		fmt.Sprintf("external-ids:namespace=%s", gp.policyNamespace),
		fmt.Sprintf("external-ids:policy=%s", gp.policyName),
		fmt.Sprintf("external-ids:%s_num=%d", gp.policyType, gp.idx),
		fmt.Sprintf("external-ids:policy_type=%s", gp.policyType))
	if err != nil {
		return fmt.Errorf("find failed to get the allow rule for "+
			"namespace=%s, policy=%s, stderr: %q (%v)",
			gp.policyNamespace, gp.policyName, stderr, err)
	}
	if uuid == "" {
		return nil
	}

	// We already have an ACL. We will update it.
	if _, stderr, err = util.RunOVNNbctl("set", "acl", uuid, newMatch); err != nil {
		return fmt.Errorf("failed to modify the allow-from rule for "+
			"namespace=%s, policy=%s, stderr: %q (%v)",
			gp.policyNamespace, gp.policyName, stderr, err)
	}

	return nil
}

// addIPBlockACLDeny adds an IPBlock deny ACL to the given Port Group
func (gp *gressPolicy) addIPBlockACLDeny(except, priority, portGroupName, portGroupUUID string) error {
	var match, l3Match, direction, lportMatch string
	direction = toLport
	if gp.policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", portGroupName)
		l3Match = fmt.Sprintf("%s.src == %s", ipMatch(), except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", portGroupName)
		l3Match = fmt.Sprintf("%s.dst == %s", ipMatch(), except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
	}

	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
		fmt.Sprintf("external-ids:ipblock-deny-policy-type=%s", gp.policyType),
		fmt.Sprintf("external-ids:namespace=%s", gp.policyNamespace),
		fmt.Sprintf("external-ids:%s_num=%d", gp.policyType, gp.idx),
		fmt.Sprintf("external-ids:policy=%s", gp.policyName))
	if err != nil {
		return fmt.Errorf("find failed to get the ipblock default deny rule for "+
			"namespace=%s, policy=%s stderr: %q, (%v)",
			gp.policyNamespace, gp.policyName, stderr, err)
	}

	if uuid != "" {
		return nil
	}

	_, stderr, err = util.RunOVNNbctl("--id=@acl", "create", "acl",
		fmt.Sprintf("priority=%s", priority),
		fmt.Sprintf("direction=%s", direction), match, "action=drop",
		fmt.Sprintf("external-ids:ipblock-deny-policy-type=%s", gp.policyType),
		fmt.Sprintf("external-ids:%s_num=%d", gp.policyType, gp.idx),
		fmt.Sprintf("external-ids:namespace=%s", gp.policyNamespace),
		fmt.Sprintf("external-ids:policy=%s", gp.policyName),
		"--", "add", "port_group", portGroupUUID,
		"acls", "@acl")
	if err != nil {
		return fmt.Errorf("error executing create ACL command, stderr: %q, %+v",
			stderr, err)
	}

	return nil
}

// localPodUpdateACL updates an existing ACL that implements the gress policy's rules
func (gp *gressPolicy) localPodUpdateACL(oldl3Match, newl3Match, portGroupName string) {
	var lportMatch string
	if gp.policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", portGroupName)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", portGroupName)
	}
	if len(gp.portPolicies) == 0 {
		oldMatch := fmt.Sprintf("match=\"%s && %s\"", oldl3Match, lportMatch)
		newMatch := fmt.Sprintf("match=\"%s && %s\"", newl3Match, lportMatch)
		if err := gp.modifyACLAllow(oldMatch, newMatch); err != nil {
			klog.Warningf(err.Error())
		}
	}
	for _, port := range gp.portPolicies {
		l4Match, err := port.getL4Match()
		if err != nil {
			continue
		}
		oldMatch := fmt.Sprintf("match=\"%s && %s && %s\"", oldl3Match, l4Match, lportMatch)
		newMatch := fmt.Sprintf("match=\"%s && %s && %s\"", newl3Match, l4Match, lportMatch)
		if err := gp.modifyACLAllow(oldMatch, newMatch); err != nil {
			klog.Warningf(err.Error())
		}
	}
}

func (gp *gressPolicy) destroy() error {
	if gp.peerAddressSet != nil {
		if err := gp.peerAddressSet.Destroy(); err != nil {
			return err
		}
		gp.peerAddressSet = nil
	}
	return nil
}

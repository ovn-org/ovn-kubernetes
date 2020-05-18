package ovn

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	knet "k8s.io/api/networking/v1"
	"k8s.io/klog"
)

type gressPolicy struct {
	policyType knet.PolicyType
	idx        int

	// peerAddressSets points to all the addressSets that hold
	// the peer pod's IP addresses. We will have one addressSet for
	// local pods and multiple addressSets that each represent a
	// peer namespace
	peerAddressSets map[string]bool

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

func newGressPolicy(policyType knet.PolicyType, idx int) *gressPolicy {
	return &gressPolicy{
		policyType:            policyType,
		idx:                   idx,
		peerAddressSets:       make(map[string]bool),
		sortedPeerAddressSets: make([]string, 0),
		portPolicies:          make([]*portPolicy, 0),
		ipBlockCidr:           make([]string, 0),
		ipBlockExcept:         make([]string, 0),
	}
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
	var l3Match, addresses string
	for _, addressSet := range gp.sortedPeerAddressSets {
		if addresses == "" {
			addresses = fmt.Sprintf("$%s", addressSet)
			continue
		}
		addresses = fmt.Sprintf("%s, $%s", addresses, addressSet)
	}
	if addresses == "" {
		l3Match = ipMatch()
	} else {
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

func (gp *gressPolicy) addAddressSet(hashedAddressSet string) (string, string, bool) {
	if gp.peerAddressSets[hashedAddressSet] {
		return "", "", false
	}

	oldL3Match := gp.getL3MatchFromAddressSet()

	gp.sortedPeerAddressSets = append(gp.sortedPeerAddressSets, hashedAddressSet)
	sort.Strings(gp.sortedPeerAddressSets)
	gp.peerAddressSets[hashedAddressSet] = true

	return oldL3Match, gp.getL3MatchFromAddressSet(), true
}

func (gp *gressPolicy) delAddressSet(hashedAddressSet string) (string, string, bool) {
	if !gp.peerAddressSets[hashedAddressSet] {
		return "", "", false
	}

	oldL3Match := gp.getL3MatchFromAddressSet()

	for i, addressSet := range gp.sortedPeerAddressSets {
		if addressSet == hashedAddressSet {
			gp.sortedPeerAddressSets = append(
				gp.sortedPeerAddressSets[:i],
				gp.sortedPeerAddressSets[i+1:]...)
			break
		}
	}
	delete(gp.peerAddressSets, hashedAddressSet)

	return oldL3Match, gp.getL3MatchFromAddressSet(), true
}

func localPodAddACL(np *namespacePolicy, gress *gressPolicy) {
	l3Match := gress.getL3MatchFromAddressSet()

	var lportMatch, cidrMatch string
	if gress.policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", np.portGroupName)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", np.portGroupName)
	}

	// If IPBlock CIDR is not empty and except string [] is not empty,
	// add deny acl rule with priority ipBlockDenyPriority (1010).
	if len(gress.ipBlockCidr) > 0 && len(gress.ipBlockExcept) > 0 {
		except := fmt.Sprintf("{%s}", strings.Join(gress.ipBlockExcept, ", "))
		addIPBlockACLDeny(np, except, ipBlockDenyPriority, gress.idx, gress.policyType)
	}

	if len(gress.portPolicies) == 0 {
		match := fmt.Sprintf("match=\"%s && %s\"", l3Match,
			lportMatch)
		l4Match := noneMatch

		if len(gress.ipBlockCidr) > 0 {
			// Add ACL allow rule for IPBlock CIDR
			cidrMatch = gress.getMatchFromIPBlock(lportMatch, l4Match)
			addACLAllow(np, cidrMatch, l4Match, true, gress.idx, gress.policyType)
		}
		// if there are pod/namespace selector, then allow packets from/to that address_set or
		// if the NetworkPolicyPeer is empty, then allow from all sources or to all destinations.
		if len(gress.sortedPeerAddressSets) > 0 || len(gress.ipBlockCidr) == 0 {
			addACLAllow(np, match, l4Match, false, gress.idx, gress.policyType)
		}
	}
	for _, port := range gress.portPolicies {
		l4Match, err := port.getL4Match()
		if err != nil {
			continue
		}
		match := fmt.Sprintf("match=\"%s && %s && %s\"",
			l3Match, l4Match, lportMatch)
		if len(gress.ipBlockCidr) > 0 {
			// Add ACL allow rule for IPBlock CIDR
			cidrMatch = gress.getMatchFromIPBlock(lportMatch, l4Match)
			addACLAllow(np, cidrMatch, l4Match, true, gress.idx, gress.policyType)
		}
		if len(gress.sortedPeerAddressSets) > 0 || len(gress.ipBlockCidr) == 0 {
			addACLAllow(np, match, l4Match, false, gress.idx, gress.policyType)
		}
	}
}

func addACLAllow(np *namespacePolicy, match, l4Match string, ipBlockCidr bool, gressNum int, policyType knet.PolicyType) {
	var direction, action string
	direction = toLport
	if policyType == knet.PolicyTypeIngress {
		action = "allow-related"
	} else {
		action = "allow"
	}

	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:l4Match=\"%s\"", l4Match),
		fmt.Sprintf("external-ids:ipblock_cidr=%t", ipBlockCidr),
		fmt.Sprintf("external-ids:namespace=%s", np.namespace),
		fmt.Sprintf("external-ids:policy=%s", np.name),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:policy_type=%s", policyType))
	if err != nil {
		klog.Errorf("find failed to get the allow rule for "+
			"namespace=%s, policy=%s, stderr: %q (%v)",
			np.namespace, np.name, stderr, err)
		return
	}

	if uuid != "" {
		return
	}

	_, stderr, err = util.RunOVNNbctl("--id=@acl", "create",
		"acl", fmt.Sprintf("priority=%s", defaultAllowPriority),
		fmt.Sprintf("direction=%s", direction), match,
		fmt.Sprintf("action=%s", action),
		fmt.Sprintf("external-ids:l4Match=\"%s\"", l4Match),
		fmt.Sprintf("external-ids:ipblock_cidr=%t", ipBlockCidr),
		fmt.Sprintf("external-ids:namespace=%s", np.namespace),
		fmt.Sprintf("external-ids:policy=%s", np.name),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:policy_type=%s", policyType),
		"--", "add", "port_group", np.portGroupUUID, "acls", "@acl")
	if err != nil {
		klog.Errorf("failed to create the acl allow rule for "+
			"namespace=%s, policy=%s, stderr: %q (%v)", np.namespace,
			np.name, stderr, err)
		return
	}
}

func modifyACLAllow(namespace, policy, oldMatch string, newMatch string, gressNum int, policyType knet.PolicyType) {
	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", oldMatch,
		fmt.Sprintf("external-ids:namespace=%s", namespace),
		fmt.Sprintf("external-ids:policy=%s", policy),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:policy_type=%s", policyType))
	if err != nil {
		klog.Errorf("find failed to get the allow rule for "+
			"namespace=%s, policy=%s, stderr: %q (%v)",
			namespace, policy, stderr, err)
		return
	}

	if uuid != "" {
		// We already have an ACL. We will update it.
		_, stderr, err = util.RunOVNNbctl("set", "acl", uuid,
			newMatch)
		if err != nil {
			klog.Errorf("failed to modify the allow-from rule for "+
				"namespace=%s, policy=%s, stderr: %q (%v)",
				namespace, policy, stderr, err)
		}
		return
	}
}

func addIPBlockACLDeny(np *namespacePolicy, except, priority string, gressNum int, policyType knet.PolicyType) {
	var match, l3Match, direction, lportMatch string
	direction = toLport
	if policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", np.portGroupName)
		l3Match = fmt.Sprintf("%s.src == %s", ipMatch(), except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", np.portGroupName)
		l3Match = fmt.Sprintf("%s.dst == %s", ipMatch(), except)
		match = fmt.Sprintf("match=\"%s && %s\"", lportMatch, l3Match)
	}

	uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action=drop",
		fmt.Sprintf("external-ids:ipblock-deny-policy-type=%s", policyType),
		fmt.Sprintf("external-ids:namespace=%s", np.namespace),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:policy=%s", np.name))
	if err != nil {
		klog.Errorf("find failed to get the ipblock default deny rule for "+
			"namespace=%s, policy=%s stderr: %q, (%v)",
			np.namespace, np.name, stderr, err)
		return
	}

	if uuid != "" {
		return
	}

	_, stderr, err = util.RunOVNNbctl("--id=@acl", "create", "acl",
		fmt.Sprintf("priority=%s", priority),
		fmt.Sprintf("direction=%s", direction), match, "action=drop",
		fmt.Sprintf("external-ids:ipblock-deny-policy-type=%s", policyType),
		fmt.Sprintf("external-ids:%s_num=%d", policyType, gressNum),
		fmt.Sprintf("external-ids:namespace=%s", np.namespace),
		fmt.Sprintf("external-ids:policy=%s", np.name),
		"--", "add", "port_group", np.portGroupUUID,
		"acls", "@acl")
	if err != nil {
		klog.Errorf("error executing create ACL command, stderr: %q, %+v",
			stderr, err)
	}
}

func (oc *Controller) handlePeerNamespaceSelectorModify(
	gress *gressPolicy, np *namespacePolicy, oldl3Match, newl3Match string) {

	var lportMatch string
	if gress.policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", np.portGroupName)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", np.portGroupName)
	}
	if len(gress.portPolicies) == 0 {
		oldMatch := fmt.Sprintf("match=\"%s && %s\"", oldl3Match,
			lportMatch)
		newMatch := fmt.Sprintf("match=\"%s && %s\"", newl3Match,
			lportMatch)
		modifyACLAllow(np.namespace, np.name, oldMatch, newMatch, gress.idx, gress.policyType)
	}
	for _, port := range gress.portPolicies {
		l4Match, err := port.getL4Match()
		if err != nil {
			continue
		}
		oldMatch := fmt.Sprintf("match=\"%s && %s && %s\"",
			oldl3Match, l4Match, lportMatch)
		newMatch := fmt.Sprintf("match=\"%s && %s && %s\"",
			newl3Match, l4Match, lportMatch)
		modifyACLAllow(np.namespace, np.name, oldMatch, newMatch, gress.idx, gress.policyType)
	}
}

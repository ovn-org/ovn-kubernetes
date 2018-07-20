package ovn

import (
	"fmt"
	util "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	knet "k8s.io/api/networking/v1"
	"net"
	"sort"
	"sync"
)

type namespacePolicy struct {
	sync.Mutex
	name            string
	namespace       string
	ingressPolicies []*gressPolicy
	egressPolicies  []*gressPolicy
	stop            chan bool
	stopWg          *sync.WaitGroup
	localPods       map[string]bool //pods effected by this policy
	portGroupUUID   string          //uuid for OVN port_group
	portGroupName   string
	deleted         bool //deleted policy
}

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

	// ipBlock represents the CIDR IP block from which traffic is allowed
	// except the IP block in the except, which should be dropped.
	ipBlockCidr   string
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
	}
}

func (gp *gressPolicy) addPortPolicy(portJSON *knet.NetworkPolicyPort) {
	gp.portPolicies = append(gp.portPolicies, &portPolicy{
		protocol: string(*portJSON.Protocol),
		port:     portJSON.Port.IntVal,
	})
}

func (gp *gressPolicy) addIPBlock(ipblockJSON *knet.IPBlock) {
	gp.ipBlockCidr = ipblockJSON.CIDR
	gp.ipBlockExcept = append([]string{}, ipblockJSON.Except...)
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
		l3Match = "ip4"
	} else {
		if gp.policyType == knet.PolicyTypeIngress {
			l3Match = fmt.Sprintf("ip4.src == {%s}", addresses)
		} else {
			l3Match = fmt.Sprintf("ip4.dst == {%s}", addresses)
		}
	}
	return l3Match
}

func (gp *gressPolicy) getMatchFromIPBlock(lportMatch, l4Match string) string {
	var match string
	if gp.policyType == knet.PolicyTypeIngress {
		if l4Match == noneMatch {
			match = fmt.Sprintf("match=\"ip4.src == {%s} && %s\"",
				gp.ipBlockCidr, lportMatch)
		} else {
			match = fmt.Sprintf("match=\"ip4.src == {%s} && %s && %s\"",
				gp.ipBlockCidr, l4Match, lportMatch)
		}
	} else {
		if l4Match == noneMatch {
			match = fmt.Sprintf("match=\"ip4.dst == {%s} && %s\"",
				gp.ipBlockCidr, lportMatch)
		} else {
			match = fmt.Sprintf("match=\"ip4.dst == {%s} && %s && %s\"",
				gp.ipBlockCidr, l4Match, lportMatch)
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

const (
	toLport   = "to-lport"
	addACL    = "add"
	deleteACL = "delete"
	noneMatch = "None"
	// Default deny acl rule priority
	defaultDenyPriority = "1000"
	// Default allow acl rule priority
	defaultAllowPriority = "1001"
	// IP Block except deny acl rule priority
	ipBlockDenyPriority = "1010"
)

func (oc *Controller) addAllowACLFromNode(logicalSwitch string) {
	uuid, stderr, err := util.RunOVNNbctlHA("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		"external-ids:node-acl=yes")
	if err != nil {
		logrus.Errorf("find failed to get the node acl for "+
			"logical_switch=%s, stderr: %q, (%v)", logicalSwitch, stderr, err)
		return
	}

	if uuid != "" {
		return
	}

	subnet, stderr, err := util.RunOVNNbctlHA("get", "logical_switch",
		logicalSwitch, "other-config:subnet")
	if err != nil {
		logrus.Errorf("failed to get the logical_switch %s subnet, "+
			"stderr: %q (%v)", logicalSwitch, stderr, err)
		return
	}

	if subnet == "" {
		return
	}

	ip, _, err := net.ParseCIDR(subnet)
	if err != nil {
		logrus.Errorf("failed to parse subnet %s", subnet)
		return
	}

	// K8s only supports IPv4 right now. The second IP address of the
	// network is the node IP address.
	ip = ip.To4()
	ip[3] = ip[3] + 2
	address := ip.String()

	match := fmt.Sprintf("match=\"ip4.src == %s\"", address)

	_, stderr, err = util.RunOVNNbctlHA("--id=@acl", "create", "acl",
		fmt.Sprintf("priority=%s", defaultAllowPriority),
		"direction=to-lport", match, "action=allow-related",
		fmt.Sprintf("external-ids:logical_switch=%s", logicalSwitch),
		"external-ids:node-acl=yes",
		"--", "add", "logical_switch", logicalSwitch, "acls", "@acl")
	if err != nil {
		logrus.Errorf("failed to create the node acl for "+
			"logical_switch=%s, stderr: %q (%v)", logicalSwitch, stderr, err)
		return
	}
}

func (oc *Controller) syncNetworkPolicies(networkPolicies []interface{}) {
	if oc.portGroupSupport {
		oc.syncNetworkPoliciesPortGroup(networkPolicies)
	} else {
		oc.syncNetworkPoliciesOld(networkPolicies)
	}
}

// AddNetworkPolicy adds network policy and create corresponding acl rules
func (oc *Controller) AddNetworkPolicy(policy *knet.NetworkPolicy) {
	if oc.portGroupSupport {
		oc.addNetworkPolicyPortGroup(policy)
	} else {
		oc.addNetworkPolicyOld(policy)
	}
}

func (oc *Controller) deleteNetworkPolicy(
	policy *knet.NetworkPolicy) {
	if oc.portGroupSupport {
		oc.deleteNetworkPolicyPortGroup(policy)
	} else {
		oc.deleteNetworkPolicyOld(policy)
	}

}

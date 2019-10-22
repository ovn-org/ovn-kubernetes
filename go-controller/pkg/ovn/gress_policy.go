package ovn

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

type gressPolicy struct {
	namespace  string
	name       string
	policyType knet.PolicyType
	idx        int

	// peerAddressSet points to the addressSet that holds all peer pod
	// IP addresess.
	peerAddressSet *addressSet

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
		namespace:             namespace,
		name:                  name,
		policyType:            policyType,
		idx:                   idx,
		nsAddressSets:         sets.String{},
		sortedPeerAddressSets: make([]string, 0),
		portPolicies:          make([]*portPolicy, 0),
		ipBlockCidr:           make([]string, 0),
		ipBlockExcept:         make([]string, 0),
	}
}

func (gp *gressPolicy) ensurePeerAddressSet() error {
	if gp.peerAddressSet != nil {
		return nil
	}

	direction := strings.ToLower(string(gp.policyType))
	asName := fmt.Sprintf("%s.%s.%s.%d", gp.namespace, gp.name, direction, gp.idx)
	as, err := NewAddressSet(asName, nil)
	if err != nil {
		return err
	}

	gp.peerAddressSet = as
	gp.sortedPeerAddressSets = append(gp.sortedPeerAddressSets, as.hashName)
	sort.Strings(gp.sortedPeerAddressSets)
	return nil
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

func (gp *gressPolicy) addNamespaceAddressSet(name string) (string, string, bool) {
	hashName := hashedAddressSet(name)
	if gp.nsAddressSets.Has(hashName) {
		return "", "", false
	}

	oldL3Match := gp.getL3MatchFromAddressSet()

	gp.nsAddressSets.Insert(hashName)
	gp.sortedPeerAddressSets = append(gp.sortedPeerAddressSets, hashName)
	sort.Strings(gp.sortedPeerAddressSets)

	return oldL3Match, gp.getL3MatchFromAddressSet(), true
}

func (gp *gressPolicy) delNamespaceAddressSet(name string) (string, string, bool) {
	hashName := hashedAddressSet(name)
	if !gp.nsAddressSets.Has(hashName) {
		return "", "", false
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

	return oldL3Match, gp.getL3MatchFromAddressSet(), true
}

func (gp *gressPolicy) destroy() {
	gp.peerAddressSet.Destroy()
	gp.peerAddressSet = nil
}

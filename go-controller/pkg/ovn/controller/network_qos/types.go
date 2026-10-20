// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"

	networkqosv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
)

// nqosCacheEntry holds the state of one NetworkQoS on each of its networks, by network name.
type nqosCacheEntry struct {
	networks map[string]*networkQoSState
}

// key is used to release the set. The hashes are used in OVN matches.
type addrSetRef struct {
	key    string
	hashV4 string
	hashV6 string
}

type networkQoSState struct {
	name      string
	namespace string

	networkName    string
	controllerName string
	// Requeue this policy if a switch with this name is created.
	switchName string

	// source or destination selectors may share the same address set.
	addrSetKeys sets.Set[string]

	applied bool

	// Keep this true until reconciliation succeeds.
	// Rules of a previous process may be bound to the switches of other topologies.
	inherited bool
}

func (state *networkQoSState) getObjectNameKey() string {
	return ovnObjectName(state.namespace, state.name)
}

func (state *networkQoSState) getDbObjectIDs(ruleID string) *libovsdbops.DbObjectIDs {
	return qosDbObjectIDs(state.controllerName, state.getObjectNameKey(), ruleID)
}

func qosDbObjectIDs(controllerName, objectName, ruleID string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.NetworkQoS, controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: objectName,
		libovsdbops.RuleIndex:     ruleID,
	})
}

func ruleStringID(ruleIndex int) string {
	return fmt.Sprintf("%d", ruleIndex)
}

// The row of a rule for the packets routers send into the switch.
func returnPassRuleID(ruleIndex int) string {
	return ruleStringID(ruleIndex) + "-return"
}

const addrSetBackRefPrefix = "NetworkQoS:"

// Address set keys already include the network, so the back reference only needs the policy.
func (state *networkQoSState) addrSetBackRef() string {
	return addrSetBackRefPrefix + nqosKey(state.namespace, state.name)
}

func (state *networkQoSState) holdAddressSet(ref addrSetRef) {
	if state.addrSetKeys == nil {
		state.addrSetKeys = sets.New[string]()
	}
	state.addrSetKeys.Insert(ref.key)
}

// GressRule represents one egress rule and its OVN QoS settings.
type GressRule struct {
	Priority   int
	Dscp       int
	Classifier *Classifier

	Rate  *int
	Burst *int
}

// The spec must already be validated and its CIDRs normalized.
func buildGressRules(nqos *networkqosv1alpha1.NetworkQoS) []*GressRule {
	rules := make([]*GressRule, 0, len(nqos.Spec.Egress))
	for index, ruleSpec := range nqos.Spec.Egress {
		rule := &GressRule{
			Priority: getQoSRulePriority(nqos.Spec.Priority, index),
			Dscp:     ruleSpec.DSCP,
		}
		if rate := int(ruleSpec.Bandwidth.Rate); rate > 0 {
			rule.Rate = &rate
		}
		if burst := int(ruleSpec.Bandwidth.Burst); burst > 0 {
			rule.Burst = &burst
		}
		destinations := make([]*Destination, 0, len(ruleSpec.Classifier.To))
		for _, destSpec := range ruleSpec.Classifier.To {
			destinations = append(destinations, &Destination{
				IpBlock:           destSpec.IPBlock.DeepCopy(),
				NamespaceSelector: destSpec.NamespaceSelector.DeepCopy(),
				PodSelector:       destSpec.PodSelector.DeepCopy(),
			})
		}
		rule.Classifier = &Classifier{
			Destinations: destinations,
			Ports:        ruleSpec.Classifier.Ports,
		}
		rules = append(rules, rule)
	}
	return rules
}

type trafficDirection string

const (
	trafficDirSource trafficDirection = "src"
	trafficDirDest   trafficDirection = "dst"
)

// Classifier selects egress destinations, protocols, and ports.
type Classifier struct {
	Destinations []*Destination
	Ports        []*networkqosv1alpha1.Port
}

// ToQosMatchString builds the OVN match for destinations, protocols, and ports.
func (c *Classifier) ToQosMatchString(ipv4Enabled, ipv6Enabled bool) string {
	if c == nil {
		return ""
	}
	destMatchStrings := []string{}
	for _, dest := range c.Destinations {
		match := "ip4.dst == 0.0.0.0/0 || ip6.dst == ::/0"
		if dest.hasSelectors() {
			match = dest.addrSet.matchString(trafficDirDest, ipv4Enabled, ipv6Enabled)
		} else if dest.IpBlock != nil && dest.IpBlock.CIDR != "" {
			ipVersion := ipBlockFamily(dest.IpBlock.CIDR)
			if len(dest.IpBlock.Except) == 0 {
				match = fmt.Sprintf("%s.%s == %s", ipVersion, trafficDirDest, dest.IpBlock.CIDR)
			} else {
				match = fmt.Sprintf("%s.%s == %s && %s.%s != {%s}", ipVersion, trafficDirDest, dest.IpBlock.CIDR, ipVersion, trafficDirDest, strings.Join(dest.IpBlock.Except, ","))
			}
		}
		destMatchStrings = append(destMatchStrings, match)
	}

	output := ""
	if len(destMatchStrings) == 1 {
		output = destMatchStrings[0]
	} else {
		for index, str := range destMatchStrings {
			if index > 0 {
				output += " || "
			}
			if strings.Contains(str, "||") || strings.Contains(str, "&&") {
				output = output + fmt.Sprintf("(%s)", str)
			} else {
				output = output + str
			}
		}
	}
	if strings.Contains(output, "||") {
		output = fmt.Sprintf("(%s)", output)
	}
	protoPortMap := map[string][]string{}
	addPort := func(protocol string, port *int32) {
		ports, exists := protoPortMap[protocol]
		// A protocol with no port matches all its ports, even if other entries specify
		// ports.
		if exists && len(ports) == 0 {
			return
		}
		if port == nil {
			protoPortMap[protocol] = nil
			return
		}
		protoPortMap[protocol] = append(ports, fmt.Sprintf("%d", *port))
	}
	for _, port := range c.Ports {
		switch {
		case port.Protocol != "":
			addPort(strings.ToLower(port.Protocol), port.Port)
		case port.Port != nil:
			// Earlier CRDs admit a port without a protocol. Match the port on every protocol.
			for _, protocol := range []string{"tcp", "udp", "sctp"} {
				addPort(protocol, port.Port)
			}
		}
	}

	sortedProtocols := make([]string, 0, len(protoPortMap))
	for protocol := range protoPortMap {
		sortedProtocols = append(sortedProtocols, protocol)
	}
	sort.Strings(sortedProtocols)

	portMatches := []string{}
	for _, protocol := range sortedProtocols {
		ports := protoPortMap[protocol]
		match := protocol
		if len(ports) == 1 {
			match = fmt.Sprintf("%s && %s.dst == %s", protocol, protocol, ports[0])
		} else if len(ports) > 1 {
			match = fmt.Sprintf("%s && %s.dst == {%s}", protocol, protocol, strings.Join(ports, ","))
		}
		portMatches = append(portMatches, match)
	}
	if len(portMatches) == 0 {
		return output
	}
	portMatch := portMatches[0]
	if len(portMatches) > 1 {
		portMatch = fmt.Sprintf("((%s))", strings.Join(portMatches, ") || ("))
	}
	if output == "" {
		return portMatch
	}
	return output + " && " + portMatch
}

// IPv4-mapped IPv6 prefixes still need an ip6 match.
func ipBlockFamily(cidr string) string {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		if utilnet.IsIPv6CIDRString(cidr) {
			return "ip6"
		}
		return "ip4"
	}
	if prefix.Addr().Is4() {
		return "ip4"
	}
	return "ip6"
}

// Destination selects pods, an IP block, or all destinations.
type Destination struct {
	IpBlock *knet.IPBlock

	// Keep selectors for the address set manager to evaluate. Nil is unspecified, while an
	// empty selector matches all.
	PodSelector       *metav1.LabelSelector
	NamespaceSelector *metav1.LabelSelector

	addrSet addrSetRef
}

func (dest *Destination) hasSelectors() bool {
	return dest.PodSelector != nil || dest.NamespaceSelector != nil
}

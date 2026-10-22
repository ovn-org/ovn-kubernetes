// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"fmt"
	"net"
	"strings"

	utilnet "k8s.io/utils/net"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func nqosKey(namespace, name string) string {
	return namespace + "/" + name
}

// Existing OVN external IDs use this separator. Changing it would break object lookups.
const ovnObjectNameSeparator = ":"

func ovnObjectName(namespace, name string) string {
	return namespace + ovnObjectNameSeparator + name
}

// An empty ownerController matches every network.
func isNetworkQoSObject(externalIDs map[string]string, ownerController, objectName string) bool {
	return externalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.NetworkQoSOwnerType) &&
		(ownerController == "" || externalIDs[libovsdbops.OwnerControllerKey.String()] == ownerController) &&
		externalIDs[libovsdbops.ObjectNameKey.String()] == objectName
}

func generateNetworkQoSMatch(source addrSetRef, rule *GressRule, ipv4Enabled, ipv6Enabled bool) string {
	match := source.matchString(trafficDirSource, ipv4Enabled, ipv6Enabled)
	if classifierMatch := rule.Classifier.ToQosMatchString(ipv4Enabled, ipv6Enabled); classifierMatch != "" {
		match += " && " + classifierMatch
	}
	return match
}

// A shared set may have been created before the network changed IP families. Only use
// families with a hash, or OVN will reject the match. Return empty if none are usable.
func (ref addrSetRef) matchString(dir trafficDirection, ipv4Enabled, ipv6Enabled bool) string {
	ipv4 := ipv4Enabled && ref.hashV4 != ""
	ipv6 := ipv6Enabled && ref.hashV6 != ""
	switch {
	case ipv4 && ipv6:
		return fmt.Sprintf("(ip4.%s == {$%s} || ip6.%s == {$%s})", dir, ref.hashV4, dir, ref.hashV6)
	case ipv4:
		return fmt.Sprintf("ip4.%s == {$%s}", dir, ref.hashV4)
	case ipv6:
		return fmt.Sprintf("ip6.%s == {$%s}", dir, ref.hashV6)
	default:
		return ""
	}
}

// Each spec.priority gets a band of priorities. Within the band, later rules win.
func getQoSRulePriority(qosPriority, ruleIndex int) int {
	return types.NetworkQoSBasePriority + qosPriority*types.NetworkQoSMaxRules + ruleIndex
}

// Packets to a Service whose selected pod is behind the same switch, or to another subnet of
// the switch, pass it twice on their source node: towards a router of the network, and back.
// These exclusions skip one pass. Both return empty without router MACs.
//
// Where rules apply as packets enter the switch, skip the packets addressed to a router for a
// destination on the network. They are classified when they come back, load balanced.
func routerReentryExclusion(routerMACs []string, subnets []*net.IPNet, ipv4Enabled, ipv6Enabled bool) string {
	if len(routerMACs) == 0 {
		return ""
	}
	var v4Subnets, v6Subnets []string
	for _, subnet := range subnets {
		if utilnet.IsIPv6CIDR(subnet) {
			if ipv6Enabled {
				v6Subnets = append(v6Subnets, subnet.String())
			}
		} else if ipv4Enabled {
			v4Subnets = append(v4Subnets, subnet.String())
		}
	}
	var destinations []string
	if len(v4Subnets) > 0 {
		destinations = append(destinations, "ip4.dst == "+matchSet(v4Subnets))
	}
	if len(v6Subnets) > 0 {
		destinations = append(destinations, "ip6.dst == "+matchSet(v6Subnets))
	}
	if len(destinations) == 0 {
		return ""
	}
	destination := destinations[0]
	if len(destinations) > 1 {
		destination = "(" + strings.Join(destinations, " || ") + ")"
	}
	return fmt.Sprintf("!(eth.dst == %s && %s)", matchSet(routerMACs), destination)
}

// Where rules apply as packets leave the switch, skip the packets a router sends into it.
// They were classified as they left the switch towards the router, with the same addresses.
func routerReturnExclusion(routerMACs []string) string {
	if len(routerMACs) == 0 {
		return ""
	}
	return fmt.Sprintf("!(%s)", routerReturnMatch(routerMACs))
}

// The packets a router sends into the switch, which routerReturnExclusion skips.
func routerReturnMatch(routerMACs []string) string {
	if len(routerMACs) == 0 {
		return ""
	}
	return "eth.src == " + matchSet(routerMACs)
}

func matchSet(values []string) string {
	if len(values) == 1 {
		return values[0]
	}
	return "{" + strings.Join(values, ", ") + "}"
}

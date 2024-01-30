package util

import (
	"fmt"
	"net"
	"regexp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
)

const (
	// dnsRegex gives the regular expression for DNS names when DNSNameResolver is enabled.
	dnsRegex = `^(\*\.)?([a-zA-Z0-9]([-a-zA-Z0-9]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z0-9]([-a-zA-Z0-9]{0,61}[a-zA-Z0-9])?\.?$`
)

// ValidateAndGetEgressFirewallDestination validates an egress firewall rule destination and returns
// the parsed contents of the destination.
func ValidateAndGetEgressFirewallDestination(egressFirewallDestination egressfirewallapi.EgressFirewallDestination) (
	cidrSelector string,
	dnsName string,
	clusterSubnetIntersection bool,
	nodeSelector *metav1.LabelSelector,
	err error) {
	// Validate the egress firewall rule.
	if egressFirewallDestination.DNSName != "" {
		// Validate that DNS name is not wildcard when DNSNameResolver is not enabled.
		if !config.OVNKubernetesFeature.EnableDNSNameResolver && IsWildcard(egressFirewallDestination.DNSName) {
			return "", "", false, nil, fmt.Errorf("wildcard dns name is not supported as rule destination, %s", egressFirewallDestination.DNSName)
		}
		// Validate that DNS name if DNSNameResolver is enabled.
		if config.OVNKubernetesFeature.EnableDNSNameResolver {
			exp := regexp.MustCompile(dnsRegex)
			if !exp.MatchString(egressFirewallDestination.DNSName) {
				return "", "", false, nil, fmt.Errorf("invalid dns name used as rule destination, %s", egressFirewallDestination.DNSName)
			}
		}
		dnsName = egressFirewallDestination.DNSName
	} else if len(egressFirewallDestination.CIDRSelector) > 0 {
		// Validate CIDR selector.
		_, ipNet, err := net.ParseCIDR(egressFirewallDestination.CIDRSelector)
		if err != nil {
			return "", "", false, nil, err
		}
		cidrSelector = egressFirewallDestination.CIDRSelector
		for _, clusterSubnet := range config.Default.ClusterSubnets {
			if clusterSubnet.CIDR.Contains(ipNet.IP) || ipNet.Contains(clusterSubnet.CIDR.IP) {
				clusterSubnetIntersection = true
				break
			}
		}
	} else {
		// Validate node selector.
		_, err := metav1.LabelSelectorAsSelector(egressFirewallDestination.NodeSelector)
		if err != nil {
			return "", "", false, nil, fmt.Errorf("rule destination has invalid node selector, err: %v", err)
		}
		nodeSelector = egressFirewallDestination.NodeSelector
	}

	return
}

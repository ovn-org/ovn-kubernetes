package util

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/miekg/dns"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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

// IsWildcard checks if the domain name is wildcard.
func IsWildcard(dnsName string) bool {
	return strings.HasPrefix(dnsName, "*.")
}

// IsDNSNameResolverEnabled retuns true if both EgressFirewall
// and DNSNameResolver are enabled.
func IsDNSNameResolverEnabled() bool {
	return config.OVNKubernetesFeature.EnableEgressFirewall && config.OVNKubernetesFeature.EnableDNSNameResolver
}

// LowerCaseFQDN convert the DNS name to lower case fully qualified
// domain name.
func LowerCaseFQDN(dnsName string) string {
	return strings.ToLower(dns.Fqdn(dnsName))
}

// GetDNSNames iterates through the egress firewall rules and returns the DNS
// names present in them after validating the rules.
func GetDNSNames(ef *egressfirewall.EgressFirewall) []string {
	var dnsNameSlice []string
	for i, egressFirewallRule := range ef.Spec.Egress {
		if i > types.EgressFirewallStartPriority-types.MinimumReservedEgressFirewallPriority {
			klog.Warningf("egressFirewall for namespace %s has too many rules, the rest will be ignored", ef.Namespace)
			break
		}

		// Validate egress firewall rule destination and get the DNS name
		// if used in the rule.
		_, dnsName, _, _, err := ValidateAndGetEgressFirewallDestination(egressFirewallRule.To)
		if err != nil {
			return []string{}
		}

		if dnsName != "" {
			dnsNameSlice = append(dnsNameSlice, LowerCaseFQDN(dnsName))
		}
	}

	return dnsNameSlice
}

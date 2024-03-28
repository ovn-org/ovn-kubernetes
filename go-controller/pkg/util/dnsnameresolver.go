package util

import "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"

// IsDNSNameResolverEnabled retuns true if both EgressFirewall
// and DNSNameResolver are enabled.
func IsDNSNameResolverEnabled() bool {
	return config.OVNKubernetesFeature.EnableEgressFirewall && config.OVNKubernetesFeature.EnableDNSNameResolver
}

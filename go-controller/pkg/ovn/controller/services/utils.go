package services

import (
	"net"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

// hasHostEndpoints determines if a slice of endpoints contains a host networked pod
func hasHostEndpoints(endpointIPs []string) bool {
	for _, endpointIP := range endpointIPs {
		if IsHostEndpoint(endpointIP) {
			return true
		}
	}
	return false
}

// IsHostEndpoint determines if the given endpoint ip belongs to a host networked pod
func IsHostEndpoint(endpointIP string) bool {
	for _, clusterNet := range globalconfig.Default.ClusterSubnets {
		if clusterNet.CIDR.Contains(net.ParseIP(endpointIP)) {
			return false
		}
	}
	return true
}

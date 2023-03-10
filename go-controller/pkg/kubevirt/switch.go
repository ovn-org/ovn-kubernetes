package kubevirt

import (
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

const (
	// ARPProxyIPv4 is the IPv4 link-local address that will be use at the
	// "arp_proxy" field on the router type LSP
	ARPProxyIPv4 = "169.254.1.1"

	// ARPProxyIPv4 is the IPv6 link-local address that will be use at the
	// "arp_proxy" field on the router type LSP
	ARPProxyIPv6 = "fe80::1"

	// ARPProxyMAC is the generated mac that will be returned when ARP arrives
	// for the ARPProxyIPv4 or ARPProxyIPv6 values at the router type LSP
	ARPProxyMAC = "0a:58:a9:fe:01:01"
)

// ComposeARPProxyLSPOption returns the "arp_proxy" field needed at router type
// LSP to implement stable default gw for pod ip migration, it consiste of
// generated MAC address, a link local ipv4 and ipv6( it's the same
// for all the logical switches) and the cluster subnet to allow the migrated
// vm to ping pods from the same subnet).
func ComposeARPProxyLSPOption() string {
	arpProxy := []string{ARPProxyMAC, ARPProxyIPv4, ARPProxyIPv6}
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		arpProxy = append(arpProxy, clusterSubnet.CIDR.String())
	}
	return strings.Join(arpProxy, " ")
}

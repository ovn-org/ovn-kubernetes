package kubevirt

import (
	"strings"
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
// LSP to implement stable default gw for pod ip migration, it consist of
// generated MAC address and a link local ipv4 and ipv6, it's the same
// for all the logical switches.
func ComposeARPProxyLSPOption() string {
	return strings.Join([]string{ARPProxyMAC, ARPProxyIPv4, ARPProxyIPv6}, " ")
}

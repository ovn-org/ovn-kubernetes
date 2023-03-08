package kubevirt

import (
	"strings"
)

const (
	// ARPProxyIPv4 is a randomly chosen IPv4 link-local address that kubevirt
	// pods will have as default gateway
	ARPProxyIPv4 = "169.254.1.1"

	// ARPProxyIPv6 is a randomly chosen IPv6 link-local address that kubevirt
	// pods will have as default gateway
	ARPProxyIPv6 = "fe80::1"

	// ARPProxyMAC is a generated mac from ARPProxyIPv4, it's generated with
	// the mechanism at `util.IPAddrToHWAddr`
	ARPProxyMAC = "0a:58:a9:fe:01:01"
)

// ComposeARPProxyLSPOption returns the "arp_proxy" field needed at router type
// LSP to implement stable default gw for pod ip migration, it consists of
// generated MAC address and a link local ipv4 and ipv6, it's the same
// for all the logical switches.
// The used address are link local addresses so they are not part of any subnet
// and the mac is calculated from the IPv4 arp proxy address.
func ComposeARPProxyLSPOption() string {
	return strings.Join([]string{ARPProxyMAC, ARPProxyIPv4, ARPProxyIPv6}, " ")
}

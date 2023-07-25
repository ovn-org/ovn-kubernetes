package kubevirt

import (
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
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
// generated MAC address, a link local ipv4 and ipv6( it's the same
// for all the logical switches) and the cluster subnet to allow the migrated
// vm to ping pods for the same subnet.
// This is how it works step by step:
// For default gw:
//   - VM is configured with arp proxy IPv4/IPv6 as default gw
//   - when a VM access an address that do not belong to its subnet it will
//     send an ARP asking for the default gw IP
//   - This will reach the OVN flows from arp_proxy and answer back with the
//     mac address here
//   - The vm will send the packet with that mac address so it will en being
//     route by ovn.
//
// For vm accessing pods at the same subnet after live migration
//   - Since the dst address is at the same subnet it will
//     not use default gw and will send an ARP for dst IP
//   - The logical switch do not have any LSP with that address since
//     vm has being live migrated
//   - ovn will fallback to arp_proxy flows to resolve ARP (these flows have
//     less priority that LSPs ones so they don't collide with them)
//   - The ovn flow for the cluster wide CIDR will be hit and ovn will answer
//     back with arp_proxy mac
//   - VM will send the message to that mac and it will end being route by
//     ovn
func ComposeARPProxyLSPOption() string {
	arpProxy := []string{ARPProxyMAC, ARPProxyIPv4, ARPProxyIPv6}
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		arpProxy = append(arpProxy, clusterSubnet.CIDR.String())
	}
	return strings.Join(arpProxy, " ")
}

package kubevirt

import (
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	ARPProxyIPv4 = "169.254.1.1"
	ARPProxyIPv6 = "d7b:6b4d:7b25:d22f::1"
)

// ComposeARPProxyLSPOption returns the "arp_proxy" field needed at router type
// LSP to implement stable default gw for pod ip migration, it consiste of
// generated MAC address and a link local ipv4 and ipv6, it's the same
// for all the logical switches.
func ComposeARPProxyLSPOption() string {
	mac := util.IPAddrToHWAddr(net.ParseIP(ARPProxyIPv4)).String()
	arpProxy := []string{mac, ARPProxyIPv4, ARPProxyIPv6}
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		arpProxy = append(arpProxy, clusterSubnet.CIDR.String())
	}
	return strings.Join(arpProxy, " ")
}

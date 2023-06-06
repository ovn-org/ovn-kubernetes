package clustermanager

import (
	"fmt"
	"math/big"
	"net"

	utilnet "k8s.io/utils/net"
)

// ipGenerator is used to generate an IP from the provided CIDR and the index.
// It is not an allocator and doesn't maintain any cache.
type ipGenerator struct {
	netCidr   *net.IPNet
	netBaseIP *big.Int
}

// newIPGenerator returns an ipGenerator instance
func newIPGenerator(subnet string) (*ipGenerator, error) {
	_, netCidr, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("error parsing subnet string %s: %v", subnet, err)
	}

	return &ipGenerator{
		netCidr:   netCidr,
		netBaseIP: utilnet.BigForIP(netCidr.IP),
	}, nil
}

// GenerateIP generates an IP from the base ip and the provided 'idx'
// and returns the IPNet with the generated IP and the netmask of
// cidr. If suppose the subnet was - 168.254.0.0/16 and the specified
// index is 10, it will return IPNet { IP : 168.254.0.10, Mask : 16}
// Returns error if the generated IP is out of network range.
func (ipGenerator *ipGenerator) GenerateIP(idx int) (*net.IPNet, error) {
	ip := utilnet.AddIPOffset(ipGenerator.netBaseIP, idx)
	if ipGenerator.netCidr.Contains(ip) {
		return &net.IPNet{IP: ip, Mask: ipGenerator.netCidr.Mask}, nil
	}
	return nil, fmt.Errorf("generated ip %s from the idx %d is out of range in the network %s", ip.String(), idx, ipGenerator.netCidr.String())
}

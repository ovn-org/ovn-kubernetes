package ip

import (
	"fmt"
	"math/big"
	"net"

	utilnet "k8s.io/utils/net"
)

// IPGenerator is used to generate an IP from the provided CIDR and the index.
// It is not an allocator and doesn't maintain any cache.
type IPGenerator struct {
	netCidr   *net.IPNet
	netBaseIP *big.Int
}

// NewIPGenerator returns an ipGenerator instance
func NewIPGenerator(subnet string) (*IPGenerator, error) {
	_, netCidr, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("error parsing subnet string %s: %v", subnet, err)
	}

	return &IPGenerator{
		netCidr:   netCidr,
		netBaseIP: utilnet.BigForIP(netCidr.IP),
	}, nil
}

// GenerateIP generates an IP from the base ip and the provided 'idx'
// and returns the IPNet with the generated IP and the netmask of
// cidr. If suppose the subnet was - 100.88.0.0/16 and the specified
// index is 10, it will return IPNet { IP : 100.88.0.10, Mask : 16}
// Returns error if the generated IP is out of network range.
func (ipGenerator *IPGenerator) GenerateIP(idx int) (*net.IPNet, error) {
	ip := utilnet.AddIPOffset(ipGenerator.netBaseIP, idx)
	if ipGenerator.netCidr.Contains(ip) {
		return &net.IPNet{IP: ip, Mask: ipGenerator.netCidr.Mask}, nil
	}
	return nil, fmt.Errorf("generated ip %s from the idx %d is out of range in the network %s", ip.String(), idx, ipGenerator.netCidr.String())
}

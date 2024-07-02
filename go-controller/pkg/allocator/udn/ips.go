package udn

import (
	"fmt"
	"net"

	idallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	utilnet "k8s.io/utils/net"
)

const (
	masqueradeIPv4IDName = "v4-masquerade-ips"
	masqueradeIPv6IDName = "v6-masquerade-ips"
)

// AllocateV4MasqueradeIPs will return a pair of masquerade IPv4 addresses calculated from
// the networkID argument
func AllocateV4MasqueradeIPs(networkID int) ([]net.IP, error) {
	return allocateMasqueradeIPs(masqueradeIPv4IDName, config.Gateway.V4MasqueradeSubnet, uint(networkID), 2)
}

// AllocateV4MasqueradeIPs will return a pair of masquerade IPv6 addresses calculated from
// the networkID argument
func AllocateV6MasqueradeIPs(networkID int) ([]net.IP, error) {
	return allocateMasqueradeIPs(masqueradeIPv6IDName, config.Gateway.V6MasqueradeSubnet, uint(networkID), 2)
}

func allocateMasqueradeIPs(idName string, masqueradeSubnet string, index, cardinality uint) ([]net.IP, error) {
	ipOffset, err := idallocator.CalculateIDBase(idName, index, types.UserDefinedNetworkMasqueradeIPBase, config.UserDefinedNetworks.MaxNetworks*cardinality, cardinality)
	if err != nil {
		return nil, err
	}
	ip, netCidr, err := net.ParseCIDR(masqueradeSubnet)
	if err != nil {
		return nil, err
	}

	masqueradeIPs := []net.IP{}
	baseIPBigInt := utilnet.BigForIP(ip)
	for i := 1; i <= int(cardinality); i++ {
		ip := utilnet.AddIPOffset(baseIPBigInt, int(ipOffset)+i)
		if ip == nil {
			return nil, fmt.Errorf("failed incrementing ip %s by '%d'", ip.String(), int(ipOffset)+i)
		}
		if !netCidr.Contains(ip) {
			return nil, fmt.Errorf("failed calculating user defined network %s masquerade IPs: ip %s out of bound for subnet %s", idName, ip, netCidr)
		}
		masqueradeIPs = append(masqueradeIPs, ip)
	}
	return masqueradeIPs, nil
}

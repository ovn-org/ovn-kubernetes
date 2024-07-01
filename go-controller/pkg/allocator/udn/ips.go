package udn

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	utilnet "k8s.io/utils/net"
)

const (
	masqueradeIPv4IDName = "v4-masquerade-ips"
	masqueradeIPv6IDName = "v6-masquerade-ips"
)

type MasqueradeIPs struct {
	Shared net.IP
	Local  net.IP
}

// AllocateV4MasqueradeIPs will return local and shared gateway masquerade IPv4 addresses calculated from
// the networkID argument
func AllocateV4MasqueradeIPs(networkID int) (*MasqueradeIPs, error) {
	return allocateMasqueradeIPs(masqueradeIPv4IDName, config.Gateway.V4MasqueradeSubnet, networkID)
}

// AllocateV4MasqueradeIPs will return local and shared gateway masquerade IPv6 addresses calculated from
// the networkID argument
func AllocateV6MasqueradeIPs(networkID int) (*MasqueradeIPs, error) {
	return allocateMasqueradeIPs(masqueradeIPv6IDName, config.Gateway.V6MasqueradeSubnet, networkID)
}

func allocateMasqueradeIPs(idName string, masqueradeSubnet string, networkID int) (*MasqueradeIPs, error) {
	ip, netCidr, err := net.ParseCIDR(masqueradeSubnet)
	if err != nil {
		return nil, err
	}
	ipOffset := types.UserDefinedNetworkMasqueradeIPBase + (networkID-1)*2
	masqueradeIPs := []net.IP{}
	baseIPBigInt := utilnet.BigForIP(ip)
	for i := 1; i <= 2; i++ {
		ip := utilnet.AddIPOffset(baseIPBigInt, int(ipOffset)+i)
		if ip == nil {
			return nil, fmt.Errorf("failed incrementing ip %s by '%d'", ip.String(), int(ipOffset)+i)
		}
		if !netCidr.Contains(ip) {
			return nil, fmt.Errorf("failed calculating user defined network %s masquerade IPs: ip %s out of bound for subnet %s", idName, ip, netCidr)
		}
		masqueradeIPs = append(masqueradeIPs, ip)
	}
	return &MasqueradeIPs{
		Shared: masqueradeIPs[0],
		Local:  masqueradeIPs[1],
	}, nil
}

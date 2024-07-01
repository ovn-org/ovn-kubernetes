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

// AllocateV6MasqueradeIPs will return local and shared gateway masquerade IPv6 addresses calculated from
// the networkID argument
func AllocateV6MasqueradeIPs(networkID int) (*MasqueradeIPs, error) {
	return allocateMasqueradeIPs(masqueradeIPv6IDName, config.Gateway.V6MasqueradeSubnet, networkID)
}

func allocateMasqueradeIPs(idName string, masqueradeSubnet string, networkID int) (*MasqueradeIPs, error) {
	if networkID < 1 {
		return nil, fmt.Errorf("invalid argument: network ID should be bigger that 0")
	}
	subnetIP, subnetCIDR, err := net.ParseCIDR(masqueradeSubnet)
	if err != nil {
		return nil, err
	}
	// Let's illustrate the expected IPs for networkID 1 and 2
	// with UserDefinedNetworkMasqueradeIPBase=10 and subnet=169.254.169.0/16
	// networkID=1
	// 	Shared: 169.254.169.11
	// 	Local: 169.254.169.12
	// networkID=2
	//  Shared: 169.254.169.13
	//  Local: 169.254.169.14
	//
	// So the calculations for networkID=2 is done by first position the
	// previousNetworkLastIPOffset to 12 and then increment from there
	// until numberOfIPs which is 2 that is 13 and 14, those values are
	// added to the subnet (169.254.169.0) with AddIPOffset to convert the offset to the
	// masquerade IPs 169.254.169.13 and 169.254.169.14
	numberOfIPs := 2
	previousNetworkID := networkID - 1
	previousNetworkLastIPOffset := previousNetworkID * numberOfIPs
	previousNetworkLastIPOffset += types.UserDefinedNetworkMasqueradeIPBase
	masqueradeIPs := []net.IP{}
	for i := 1; i <= numberOfIPs; i++ {
		ipOffset := previousNetworkLastIPOffset + i
		masqueradeIP := utilnet.AddIPOffset(utilnet.BigForIP(subnetIP), ipOffset)
		if masqueradeIP == nil {
			return nil, fmt.Errorf("failed incrementing subnet IP '%s' by '%d'", subnetIP.String(), ipOffset)
		}
		if !subnetCIDR.Contains(masqueradeIP) {
			return nil, fmt.Errorf("failed calculating user defined network %s masquerade IPs: IP %s is not within subnet %s", idName, masqueradeIP, subnetCIDR)
		}
		masqueradeIPs = append(masqueradeIPs, masqueradeIP)
	}
	return &MasqueradeIPs{
		Shared: masqueradeIPs[0],
		Local:  masqueradeIPs[1],
	}, nil
}

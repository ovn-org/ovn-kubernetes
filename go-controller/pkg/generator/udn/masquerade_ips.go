package udn

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ipgenerator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/ip"
)

const (
	masqueradeIPv4IDName = "v4-masquerade-ips"
	masqueradeIPv6IDName = "v6-masquerade-ips"
	// userDefinedNetworkMasqueradeIPBase define the base to calculate udn
	// masquerade IPs
	userDefinedNetworkMasqueradeIPBase = 10
)

// MasqueradeIPs contains the Masquerade IPs needed for user defined network
// topology
type MasqueradeIPs struct {
	// GatewayRouter is the masquerade IP for gateway router
	GatewayRouter *net.IPNet
	// ManagementPort is the masquerade IP for management port
	ManagementPort *net.IPNet
}

// AllocateV4MasqueradeIPs will return the gateway router and management port masquerade IPv4 addresses calculated from
// the networkID argument
func AllocateV4MasqueradeIPs(networkID int) (*MasqueradeIPs, error) {
	return allocateMasqueradeIPs(masqueradeIPv4IDName, config.Gateway.V4MasqueradeSubnet, networkID)
}

// AllocateV4MasqueradeIPs will return the gateway router and management port masquerade IPv6 addresses calculated from
// the networkID argument
func AllocateV6MasqueradeIPs(networkID int) (*MasqueradeIPs, error) {
	return allocateMasqueradeIPs(masqueradeIPv6IDName, config.Gateway.V6MasqueradeSubnet, networkID)
}

func allocateMasqueradeIPs(idName string, masqueradeSubnet string, networkID int) (*MasqueradeIPs, error) {
	if networkID < 1 {
		return nil, fmt.Errorf("invalid argument: network ID should be bigger that 0")
	}
	ipGenerator, err := ipgenerator.NewIPGenerator(masqueradeSubnet)
	if err != nil {
		return nil, fmt.Errorf("failed initializing generation of network id '%d' %s IPs: %w", networkID, idName, err)
	}
	// Let's illustrate the expected IPs for networkID 1 and 2
	// with userDefinedNetworkMasqueradeIPBase=10 and subnet=169.254.0.0/17
	// networkID=1
	//  GatewayRouter: 169.254.0.11
	//  ManagementPort: 169.254.0.12
	// networkID=2
	//  GatewayRouter: 169.254.0.13
	//  ManagementPort: 169.254.0.14
	//
	numberOfIPs := 2
	masqueradeIPs := &MasqueradeIPs{}
	masqueradeIPs.GatewayRouter, err = ipGenerator.GenerateIP(userDefinedNetworkMasqueradeIPBase + networkID*numberOfIPs - 1)
	if err != nil {
		return nil, fmt.Errorf("failed generating network id '%d' %s gateway router ip: %w", networkID, idName, err)
	}
	masqueradeIPs.ManagementPort, err = ipGenerator.GenerateIP(userDefinedNetworkMasqueradeIPBase + networkID*numberOfIPs)
	if err != nil {
		return nil, fmt.Errorf("failed generating network id '%d' %s management port ip: %w", networkID, idName, err)
	}
	return masqueradeIPs, nil
}

// GetUDNGatewayMasqueradeIPs returns the list of gateway router masqueradeIPs for the given UDN's networkID
func GetUDNGatewayMasqueradeIPs(networkID int) ([]*net.IPNet, error) {
	var masqIPs []*net.IPNet
	if config.IPv4Mode {
		v4MasqIPs, err := AllocateV4MasqueradeIPs(networkID)
		if err != nil {
			return nil, fmt.Errorf("failed to get v4 masquerade IP, networkID %d: %v", networkID, err)
		}
		masqIPs = append(masqIPs, v4MasqIPs.GatewayRouter)
	}
	if config.IPv6Mode {
		v6MasqIPs, err := AllocateV6MasqueradeIPs(networkID)
		if err != nil {
			return nil, fmt.Errorf("failed to get v6 masquerade IP, networkID %d: %v", networkID, err)
		}
		masqIPs = append(masqIPs, v6MasqIPs.GatewayRouter)
	}
	return masqIPs, nil
}

package util

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/libovsdb/client"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// ExtractPortAddresses returns the MAC and IPs of the given logical switch port
func ExtractPortAddresses(lsp *nbdb.LogicalSwitchPort) (net.HardwareAddr, []net.IP, error) {
	var addresses []string

	if lsp.DynamicAddresses == nil {
		if len(lsp.Addresses) > 0 {
			addresses = strings.Split(lsp.Addresses[0], " ")
		}
	} else {
		// dynamic addresses have format "0a:00:00:00:00:01 192.168.1.3"
		// static addresses have format ["0a:00:00:00:00:01", "192.168.1.3"]
		addresses = strings.Split(*lsp.DynamicAddresses, " ")
	}

	if len(addresses) == 0 || addresses[0] == "dynamic" {
		return nil, nil, nil
	}

	mac, err := net.ParseMAC(addresses[0])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse logical switch port %q MAC %q: %v", lsp.Name, addresses[0], err)
	}
	var ips []net.IP
	for _, addr := range addresses[1:] {
		ip := net.ParseIP(addr)
		if ip == nil {
			return nil, nil, fmt.Errorf("failed to parse logical switch port %q IP %q is not a valid ip address", lsp.Name, addr)
		}
		ips = append(ips, ip)
	}
	return mac, ips, nil
}

// GetLRPAddrs returns the addresses for the given logical router port
func GetLRPAddrs(nbClient client.Client, portName string) ([]*net.IPNet, error) {
	lrp := &nbdb.LogicalRouterPort{Name: portName}
	lrp, err := libovsdbops.GetLogicalRouterPort(nbClient, lrp)
	if err != nil {
		return nil, fmt.Errorf("failed to get router port %s: %w", portName, err)
	}
	gwLRPIPs := []*net.IPNet{}
	for _, network := range lrp.Networks {
		ip, network, err := net.ParseCIDR(network)
		if err != nil {
			return nil, fmt.Errorf("unable to parse network CIDR: %s for router port: %s, err: %v ", network, portName, err)
		}
		gwLRPIPs = append(gwLRPIPs, &net.IPNet{
			IP:   ip,
			Mask: network.Mask,
		})
	}
	return gwLRPIPs, nil
}

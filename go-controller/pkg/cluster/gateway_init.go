// +build linux

package cluster

import (
	"fmt"
	"net"
	"syscall"

	"github.com/coreos/go-iptables/iptables"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"
)

// getIPv4Address returns the ipv4 address for the network interface 'iface'.
func getIPv4Address(iface string) (string, error) {
	var ipAddress string
	intf, err := net.InterfaceByName(iface)
	if err != nil {
		return ipAddress, err
	}

	addrs, err := intf.Addrs()
	if err != nil {
		return ipAddress, err
	}

	for _, addr := range addrs {
		switch ip := addr.(type) {
		case *net.IPNet:
			if ip.IP.To4() != nil {
				ipAddress = ip.String()
			}
		}
	}
	return ipAddress, nil
}

// getDefaultGatewayInterfaceDetails returns the interface name on
// which the default gateway (for route to 0.0.0.0) is configured.
// It also returns the default gateway itself.
func getDefaultGatewayInterfaceDetails() (string, string, error) {
	routes, err := netlink.RouteList(nil, syscall.AF_INET)
	if err != nil {
		return "", "", fmt.Errorf("Failed to get routing table in node")
	}

	for i := range routes {
		route := routes[i]
		if route.Dst == nil && route.Gw != nil && route.LinkIndex > 0 {
			intfLink, err := netlink.LinkByIndex(route.LinkIndex)
			if err != nil {
				continue
			}
			intfName := intfLink.Attrs().Name
			if intfName != "" {
				return intfName, route.Gw.String(), nil
			}
		}
	}
	return "", "", fmt.Errorf("Failed to get default gateway interface")
}

func (cluster *OvnClusterController) initGateway(
	nodeName string, clusterIPSubnet []string, subnet string) error {
	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return fmt.Errorf("failed to initialize iptables: %v", err)
	}
	return cluster.initGatewayInternal(nodeName, clusterIPSubnet, subnet, ipt)
}

func (cluster *OvnClusterController) initGatewayInternal(
	nodeName string, clusterIPSubnet []string, subnet string,
	ipt util.IPTablesHelper) error {
	if cluster.LocalnetGateway {
		return initLocalnetGateway(nodeName, clusterIPSubnet, subnet, ipt)
	}

	if cluster.GatewayNextHop == "" || cluster.GatewayIntf == "" {
		// We need to get the interface details from the default gateway.
		gatewayIntf, gatewayNextHop, err := getDefaultGatewayInterfaceDetails()
		if err != nil {
			return err
		}

		if cluster.GatewayNextHop == "" {
			cluster.GatewayNextHop = gatewayNextHop
		}

		if cluster.GatewayIntf == "" {
			cluster.GatewayIntf = gatewayIntf
		}
	}

	if cluster.GatewaySpareIntf {
		return initSpareGateway(nodeName, clusterIPSubnet, subnet,
			cluster.GatewayNextHop, cluster.GatewayIntf,
			cluster.NodePortEnable)
	}

	bridge, gwIntf, err := initSharedGateway(nodeName, clusterIPSubnet, subnet,
		cluster.GatewayNextHop, cluster.GatewayIntf, cluster.NodePortEnable,
		cluster.watchFactory)
	if err != nil {
		return err
	}
	cluster.GatewayBridge = bridge
	cluster.GatewayIntf = gwIntf
	return nil
}

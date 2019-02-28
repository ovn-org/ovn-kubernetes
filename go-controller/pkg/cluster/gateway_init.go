package cluster

import (
	"net"
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

func (cluster *OvnClusterController) initGateway(
	nodeName string, clusterIPSubnet []string, subnet string) error {
	if cluster.LocalnetGateway {
		return initLocalnetGateway(nodeName, clusterIPSubnet, subnet,
			cluster.NodePortEnable)
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
			cluster.GatewayNextHop, cluster.GatewayIntf, cluster.GatewayVLANID,
			cluster.NodePortEnable)
	}

	bridge, gwIntf, err := initSharedGateway(nodeName, clusterIPSubnet, subnet,
		cluster.GatewayNextHop, cluster.GatewayIntf, cluster.GatewayVLANID,
		cluster.NodePortEnable, cluster.watchFactory)
	if err != nil {
		return err
	}
	cluster.GatewayBridge = bridge
	cluster.GatewayIntf = gwIntf
	return nil
}

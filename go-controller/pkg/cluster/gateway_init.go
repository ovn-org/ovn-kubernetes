package cluster

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// bridgedGatewayNodeSetup makes the bridge's MAC address permanent, sets up
// the physical network name mappings for the bridge, and returns an ifaceID
// created from the bridge name and the node name
func bridgedGatewayNodeSetup(nodeName, bridgeInterface string) (string, string, error) {
	// A OVS bridge's mac address can change when ports are added to it.
	// We cannot let that happen, so make the bridge mac address permanent.
	macAddress, err := util.GetOVSPortMACAddress(bridgeInterface)
	if err != nil {
		return "", "", err
	}
	stdout, stderr, err := util.RunOVSVsctl("set", "bridge",
		bridgeInterface, "other-config:hwaddr="+macAddress.String())
	if err != nil {
		return "", "", fmt.Errorf("Failed to set bridge, stdout: %q, stderr: %q, "+
			"error: %v", stdout, stderr, err)
	}

	// ovn-bridge-mappings maps a physical network name to a local ovs bridge
	// that provides connectivity to that network.
	_, stderr, err = util.RunOVSVsctl("set", "Open_vSwitch", ".",
		fmt.Sprintf("external_ids:ovn-bridge-mappings=%s:%s", util.PhysicalNetworkName, bridgeInterface))
	if err != nil {
		return "", "", fmt.Errorf("Failed to set ovn-bridge-mappings for ovs bridge %s"+
			", stderr:%s (%v)", bridgeInterface, stderr, err)
	}

	ifaceID := bridgeInterface + "_" + nodeName
	return ifaceID, macAddress.String(), nil
}

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
loop:
	for _, addr := range addrs {
		switch ip := addr.(type) {
		case *net.IPNet:
			if ip.IP.To4() != nil {
				ipAddress = ip.String()
			}
			// get the first ip address
			if ipAddress != "" {
				break loop
			}
		}
	}
	return ipAddress, nil
}

func (cluster *OvnClusterController) initGateway(
	nodeName string, clusterIPSubnet []string, subnet string) error {
	if cluster.LocalnetGateway {
		return initLocalnetGateway(nodeName, clusterIPSubnet, subnet,
			cluster.NodePortEnable, cluster.watchFactory)
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

	gwIntf, err := initSharedGateway(nodeName, clusterIPSubnet, subnet,
		cluster.GatewayNextHop, cluster.GatewayIntf, cluster.GatewayVLANID,
		cluster.NodePortEnable, cluster.watchFactory)
	if err != nil {
		return err
	}
	cluster.GatewayIntf = gwIntf
	return nil
}

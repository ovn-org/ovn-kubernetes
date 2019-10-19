package cluster

import (
	"fmt"
	"runtime"

	"github.com/sirupsen/logrus"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
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
		bridgeInterface, "other-config:hwaddr="+macAddress)
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
	return ifaceID, macAddress, nil
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

	if config.Gateway.NodeportEnable {
		err := initLoadBalancerHealthChecker(nodeName, cluster.watchFactory)
		if err != nil {
			return err
		}
	}

	switch config.Gateway.Mode {
	case config.GatewayModeLocal:
		return initLocalnetGateway(nodeName, clusterIPSubnet, subnet,
			cluster.watchFactory)
	case config.GatewayModeShared:
		gatewayNextHop := config.Gateway.NextHop
		gatewayIntf := config.Gateway.Interface
		if gatewayNextHop == "" || gatewayIntf == "" {
			// We need to get the interface details from the default gateway.
			defaultGatewayIntf, defaultGatewayNextHop, err := getDefaultGatewayInterfaceDetails()
			if err != nil {
				return err
			}

			if gatewayNextHop == "" {
				gatewayNextHop = defaultGatewayNextHop
			}

			if gatewayIntf == "" {
				gatewayIntf = defaultGatewayIntf
			}
		}
		return initSharedGateway(nodeName, clusterIPSubnet, subnet,
			gatewayNextHop, gatewayIntf, cluster.watchFactory)
	}

	return nil
}

// CleanupClusterNode cleans up OVS resources on the k8s node on ovnkube-node daemonset deletion.
// This is going to be a best effort cleanup.
func CleanupClusterNode(name string) error {
	var err error

	logrus.Debugf("Cleaning up gateway resources on node: %q", name)
	switch config.Gateway.Mode {
	case config.GatewayModeLocal:
		err = cleanupLocalnetGateway()
	case config.GatewayModeShared:
		err = cleanupSharedGateway()
	}
	if err != nil {
		logrus.Errorf("Failed to cleanup Gateway, error: %v", err)
	}

	// Make sure br-int is deleted, the management internal port is also deleted at the same time.
	stdout, stderr, err := util.RunOVSVsctl("--", "--if-exists", "del-br", "br-int")
	if err != nil {
		logrus.Errorf("Failed to delete bridge br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
	}

	// Delete iptable rules for management port on Linux.
	if runtime.GOOS != "windows" {
		DelMgtPortIptRules(name)
	}

	return nil
}

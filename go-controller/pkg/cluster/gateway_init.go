package cluster

import (
	"fmt"
	"runtime"

	"github.com/sirupsen/logrus"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// bridgedGatewayNodeSetup makes the bridge's MAC address permanent (if needed), sets up
// the physical network name mappings for the bridge, and returns an ifaceID
// created from the bridge name and the node name
func bridgedGatewayNodeSetup(nodeName, bridgeName, bridgeInterface string, syncBridgeMac bool) (string, string, error) {
	// A OVS bridge's mac address can change when ports are added to it.
	// We cannot let that happen, so make the bridge mac address permanent.
	macAddress, err := util.GetOVSPortMACAddress(bridgeInterface)
	if err != nil {
		return "", "", err
	}
	if syncBridgeMac {
		var err error

		stdout, stderr, err := util.RunOVSVsctl("set", "bridge",
			bridgeName, "other-config:hwaddr="+macAddress)
		if err != nil {
			return "", "", fmt.Errorf("Failed to set bridge, stdout: %q, stderr: %q, "+
				"error: %v", stdout, stderr, err)
		}
	}

	// ovn-bridge-mappings maps a physical network name to a local ovs bridge
	// that provides connectivity to that network.
	_, stderr, err := util.RunOVSVsctl("set", "Open_vSwitch", ".",
		fmt.Sprintf("external_ids:ovn-bridge-mappings=%s:%s", util.PhysicalNetworkName, bridgeName))
	if err != nil {
		return "", "", fmt.Errorf("Failed to set ovn-bridge-mappings for ovs bridge %s"+
			", stderr:%s (%v)", bridgeName, stderr, err)
	}

	ifaceID := bridgeName + "_" + nodeName
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
	nodeName string, subnet string) (map[string]string, postReadyFn, error) {

	if config.Gateway.NodeportEnable {
		err := initLoadBalancerHealthChecker(nodeName, cluster.watchFactory)
		if err != nil {
			return nil, nil, err
		}
	}

	switch config.Gateway.Mode {
	case config.GatewayModeLocal:
		annotations, err := initLocalnetGateway(nodeName, subnet, cluster.watchFactory)
		return annotations, nil, err
	case config.GatewayModeShared:
		gatewayNextHop := config.Gateway.NextHop
		gatewayIntf := config.Gateway.Interface
		if gatewayNextHop == "" || gatewayIntf == "" {
			// We need to get the interface details from the default gateway.
			defaultGatewayIntf, defaultGatewayNextHop, err := getDefaultGatewayInterfaceDetails()
			if err != nil {
				return nil, nil, err
			}

			if gatewayNextHop == "" {
				gatewayNextHop = defaultGatewayNextHop
			}

			if gatewayIntf == "" {
				gatewayIntf = defaultGatewayIntf
			}
		}
		return initSharedGateway(nodeName, subnet, gatewayNextHop, gatewayIntf, cluster.watchFactory)
	}

	return nil, nil, nil
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

// GatewayReady will check to see if the gateway was created
func GatewayReady(nodeName string, portName string) (bool, error) {

	gatewayRouter := "GR_" + nodeName
	stdout, stderr, err := util.RunOVNNbctl("lsp-get-addresses", "etor-"+gatewayRouter)
	if err != nil {
		logrus.Errorf("Error while obtaining gateway router addresses for %s - %v", nodeName, err)
		return false, err
	}
	// Did master create etor-GR_nodeName port on ls?
	if stdout == "" || stderr != "" {
		return false, nil
	}
	return true, nil
}

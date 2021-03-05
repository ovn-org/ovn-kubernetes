package node

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// bridgedGatewayNodeSetup makes the bridge's MAC address permanent (if needed), sets up
// the physical network name mappings for the bridge, and returns an ifaceID
// created from the bridge name and the node name
func bridgedGatewayNodeSetup(nodeName, bridgeName, bridgeInterface, physicalNetworkName string,
	syncBridgeMAC bool) (string, net.HardwareAddr, error) {
	// A OVS bridge's mac address can change when ports are added to it.
	// We cannot let that happen, so make the bridge mac address permanent.
	macAddress, err := util.GetOVSPortMACAddress(bridgeInterface)
	if err != nil {
		return "", nil, err
	}
	if syncBridgeMAC {
		var err error

		stdout, stderr, err := util.RunOVSVsctl("set", "bridge",
			bridgeName, "other-config:hwaddr="+macAddress.String())
		if err != nil {
			return "", nil, fmt.Errorf("failed to set bridge, stdout: %q, stderr: %q, "+
				"error: %v", stdout, stderr, err)
		}
	}

	// ovn-bridge-mappings maps a physical network name to a local ovs bridge
	// that provides connectivity to that network. It is in the form of physnet1:br1,physnet2:br2.
	// Note that there may be multiple ovs bridge mappings, be sure not to override
	// the mappings for the other physical network
	stdout, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return "", nil, fmt.Errorf("failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	// skip the existing mapping setting for the specified physicalNetworkName
	mapString := ""
	bridgeMappings := strings.Split(stdout, ",")
	for _, bridgeMapping := range bridgeMappings {
		m := strings.Split(bridgeMapping, ":")
		if network := m[0]; network != physicalNetworkName {
			if len(mapString) != 0 {
				mapString += ","
			}
			mapString += bridgeMapping
		}
	}
	if len(mapString) != 0 {
		mapString += ","
	}
	mapString += physicalNetworkName + ":" + bridgeName

	_, stderr, err = util.RunOVSVsctl("set", "Open_vSwitch", ".",
		fmt.Sprintf("external_ids:ovn-bridge-mappings=%s", mapString))
	if err != nil {
		return "", nil, fmt.Errorf("failed to set ovn-bridge-mappings for ovs bridge %s"+
			", stderr:%s (%v)", bridgeName, stderr, err)
	}

	ifaceID := bridgeName + "_" + nodeName
	return ifaceID, macAddress, nil
}

// getNetworkInterfaceIPAddresses returns the IP addresses for the network interface 'iface'.
func getNetworkInterfaceIPAddresses(iface string) ([]*net.IPNet, error) {
	allIPs, err := util.GetNetworkInterfaceIPs(iface)
	if err != nil {
		return nil, fmt.Errorf("could not find IP addresses: %v", err)
	}

	var ips []*net.IPNet
	var foundIPv4 bool
	var foundIPv6 bool
	for _, ip := range allIPs {
		if utilnet.IsIPv6CIDR(ip) {
			if config.IPv6Mode && !foundIPv6 {
				ips = append(ips, ip)
				foundIPv6 = true
			}
		} else if config.IPv4Mode && !foundIPv4 {
			ips = append(ips, ip)
			foundIPv4 = true
		}
	}
	if config.IPv4Mode && !foundIPv4 {
		return nil, fmt.Errorf("failed to find IPv4 address on interface %s", iface)
	} else if config.IPv6Mode && !foundIPv6 {
		return nil, fmt.Errorf("failed to find IPv6 address on interface %s", iface)
	}
	return ips, nil
}

func getGatewayNextHops() ([]net.IP, string, error) {
	var gatewayNextHops []net.IP
	var needIPv4NextHop bool
	var needIPv6NextHop bool

	if config.IPv4Mode {
		needIPv4NextHop = true
	}
	if config.IPv6Mode {
		needIPv6NextHop = true
	}

	// FIXME DUAL-STACK: config.Gateway.NextHop should be a slice of nexthops
	if config.Gateway.NextHop != "" {
		// Parse NextHop to make sure it is valid before using. Return error if not valid.
		nextHop := net.ParseIP(config.Gateway.NextHop)
		if nextHop == nil {
			return nil, "", fmt.Errorf("failed to parse configured next-hop: %s", config.Gateway.NextHop)
		}
		if config.IPv4Mode && !utilnet.IsIPv6(nextHop) {
			gatewayNextHops = append(gatewayNextHops, nextHop)
			needIPv4NextHop = false
		}
		if config.IPv6Mode && utilnet.IsIPv6(nextHop) {
			gatewayNextHops = append(gatewayNextHops, nextHop)
			needIPv6NextHop = false
		}
	}
	gatewayIntf := config.Gateway.Interface
	if needIPv4NextHop || needIPv6NextHop || gatewayIntf == "" {
		defaultGatewayIntf, defaultGatewayNextHops, err := getDefaultGatewayInterfaceDetails()
		if err != nil {
			return nil, "", err
		}
		if needIPv4NextHop || needIPv6NextHop {
			for _, defaultGatewayNextHop := range defaultGatewayNextHops {
				if needIPv4NextHop && !utilnet.IsIPv6(defaultGatewayNextHop) {
					gatewayNextHops = append(gatewayNextHops, defaultGatewayNextHop)
					needIPv4NextHop = false
				} else if needIPv6NextHop && utilnet.IsIPv6(defaultGatewayNextHop) {
					gatewayNextHops = append(gatewayNextHops, defaultGatewayNextHop)
					needIPv6NextHop = false
				}
			}
			if needIPv4NextHop || needIPv6NextHop {
				return nil, "", fmt.Errorf("failed to get next-hop: IPv4=%v IPv6=%v", needIPv4NextHop, needIPv6NextHop)
			}
		}
		if gatewayIntf == "" {
			gatewayIntf = defaultGatewayIntf
		}
	}
	return gatewayNextHops, gatewayIntf, nil
}

func (n *OvnNode) initGateway(subnets []*net.IPNet, nodeAnnotator kube.Annotator,
	waiter *startupWaiter, managementPortConfig *managementPortConfig) error {
	klog.Info("Initializing Gateway Functionality")
	var err error

	var loadBalancerHealthChecker *loadBalancerHealthChecker
	var portClaimWatcher *portClaimWatcher

	if config.Gateway.NodeportEnable {
		loadBalancerHealthChecker = newLoadBalancerHealthChecker(n.name)
		portClaimWatcher, err = newPortClaimWatcher(n.recorder)
		if err != nil {
			return err
		}
	}

	gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
	if err != nil {
		return err
	}

	ifAddrs, err := getNetworkInterfaceIPAddresses(gatewayIntf)
	if err != nil {
		return err
	}

	v4IfAddr, _ := util.MatchIPNetFamily(false, ifAddrs)
	v6IfAddr, _ := util.MatchIPNetFamily(true, ifAddrs)
	if err := util.SetNodePrimaryIfAddr(nodeAnnotator, v4IfAddr, v6IfAddr); err != nil {
		klog.Errorf("Unable to set primary IP net label on node, err: %v", err)
	}

	var gw *gateway
	switch config.Gateway.Mode {
	case config.GatewayModeLocal:
		klog.Info("Preparing Local Gateway")
		gw, err = newLocalGateway(n.name, subnets, gatewayNextHops, gatewayIntf, nodeAnnotator, n.recorder, managementPortConfig)
	case config.GatewayModeShared:
		klog.Info("Preparing Shared Gateway")
		gw, err = newSharedGateway(n.name, subnets, gatewayNextHops, gatewayIntf, nodeAnnotator)
	case config.GatewayModeDisabled:
		klog.Info("Gateway Mode is disabled")
		gw = &gateway{
			initFunc:  func() error { return nil },
			readyFunc: func() (bool, error) { return true, nil },
		}
		err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{
			Mode: config.GatewayModeDisabled,
		})
	}
	if err != nil {
		return err
	}
	// a golang interface has two values <type, value>. an interface is nil if both type and
	// value is nil. so, you cannot directly set the value to an interface and later check if
	// value was nil by comparing the interface to nil. this is because if the value is `nil`,
	// then the interface will still hold the type of the value being set.
	if loadBalancerHealthChecker != nil {
		gw.loadBalancerHealthChecker = loadBalancerHealthChecker
	}
	if portClaimWatcher != nil {
		gw.portClaimWatcher = portClaimWatcher
	}
	initGw := func() error {
		return gw.Init(n.watchFactory)
	}

	waiter.AddWait(gw.readyFunc, initGw)
	n.gateway = gw
	return err
}

// CleanupClusterNode cleans up OVS resources on the k8s node on ovnkube-node daemonset deletion.
// This is going to be a best effort cleanup.
func CleanupClusterNode(name string) error {
	var err error

	klog.V(5).Infof("Cleaning up gateway resources on node: %q", name)
	if config.Gateway.Mode == config.GatewayModeLocal || config.Gateway.Mode == config.GatewayModeShared {
		err = cleanupLocalnetGateway(types.LocalNetworkName)
		if err != nil {
			klog.Errorf("Failed to cleanup Localnet Gateway, error: %v", err)
		}
		err = cleanupSharedGateway()
	}
	if err != nil {
		klog.Errorf("Failed to cleanup Gateway, error: %v", err)
	}

	stdout, stderr, err := util.RunOVSVsctl("--", "--if-exists", "remove", "Open_vSwitch", ".", "external_ids",
		"ovn-bridge-mappings")
	if err != nil {
		klog.Errorf("Failed to delete ovn-bridge-mappings, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
	}

	// Delete iptable rules for management port
	DelMgtPortIptRules()

	return nil
}

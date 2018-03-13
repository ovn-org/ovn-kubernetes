// +build linux

package cluster

import (
	"fmt"
	"net"
	"syscall"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
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

func (cluster *OvnClusterController) addDefaultConntrackRules() error {
	patchPort := "k8s-patch-" + cluster.GatewayBridge + "-br-int"
	// Get ofport of pathPort
	ofportPatch, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", patchPort, "ofport")
	if err != nil {
		return fmt.Errorf("Failed to get ofport of %s, stderr: %q, error: %v",
			patchPort, stderr, err)
	}

	// Get ofport of physical interface
	ofportPhys, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", cluster.GatewayIntf, "ofport")
	if err != nil {
		return fmt.Errorf("Failed to get ofport of %s, stderr: %q, error: %v",
			cluster.GatewayIntf, stderr, err)
	}

	// table 0, packets coming from pods headed externally. Commit connections
	// so that reverse direction goes back to the pods.
	_, stderr, err = util.RunOVSOfctl("add-flow", cluster.GatewayBridge,
		fmt.Sprintf("priority=100, in_port=%s, ip, "+
			"actions=ct(commit, zone=%d), output:%s",
			ofportPatch, config.Default.ConntrackZone, ofportPhys))
	if err != nil {
		return fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", cluster.GatewayBridge, stderr, err)
	}

	// table 0, packets coming from external. Send it through conntrack and
	// resubmit to table 1 to know the state of the connection.
	_, stderr, err = util.RunOVSOfctl("add-flow", cluster.GatewayBridge,
		fmt.Sprintf("priority=50, in_port=%s, ip, "+
			"actions=ct(zone=%d, table=1)", ofportPhys, config.Default.ConntrackZone))
	if err != nil {
		return fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", cluster.GatewayBridge, stderr, err)
	}

	// table 1, established and related connections go to pod
	_, stderr, err = util.RunOVSOfctl("add-flow", cluster.GatewayBridge,
		fmt.Sprintf("priority=100, table=1, ct_state=+trk+est, "+
			"actions=output:%s", ofportPatch))
	if err != nil {
		return fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", cluster.GatewayBridge, stderr, err)
	}
	_, stderr, err = util.RunOVSOfctl("add-flow", cluster.GatewayBridge,
		fmt.Sprintf("priority=100, table=1, ct_state=+trk+rel, "+
			"actions=output:%s", ofportPatch))
	if err != nil {
		return fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", cluster.GatewayBridge, stderr, err)
	}

	// table 1, all other connections go to the bridge interface.
	_, stderr, err = util.RunOVSOfctl("add-flow", cluster.GatewayBridge,
		"priority=0, table=1, actions=output:LOCAL")
	if err != nil {
		return fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", cluster.GatewayBridge, stderr, err)
	}
	return nil
}

func (cluster *OvnClusterController) initGateway(
	nodeName, clusterIPSubnet, subnet string) error {
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

	var ipAddress string
	var err error
	if !cluster.GatewaySpareIntf {
		// Check to see whether the interface is OVS bridge.
		_, _, err = util.RunOVSVsctl("--", "br-exists", cluster.GatewayIntf)
		if err != nil {
			// This is not a OVS bridge. We need to create a OVS bridge
			// and add cluster.GatewayIntf as a port of that bridge.
			err = util.NicToBridge(cluster.GatewayIntf)
			if err != nil {
				return fmt.Errorf("Failed to convert nic %s to OVS bridge "+
					"(%v)", cluster.GatewayIntf, err)
			}
			cluster.GatewayBridge = fmt.Sprintf("br%s", cluster.GatewayIntf)
		} else {
			return fmt.Errorf("gateway interface is not a physical device, " +
				"but rather a bridge device")
		}

		// Now, we get IP address from OVS bridge. If IP does not exist,
		// error out.
		ipAddress, err = getIPv4Address(cluster.GatewayBridge)
		if err != nil {
			return fmt.Errorf("Failed to get interface details for %s (%v)",
				cluster.GatewayBridge, err)
		}
		if ipAddress == "" {
			return fmt.Errorf("%s does not have a ipv4 address",
				cluster.GatewayBridge)
		}
		err = util.GatewayInit(clusterIPSubnet, nodeName, ipAddress, "",
			cluster.GatewayBridge, cluster.GatewayNextHop, subnet)
		if err != nil {
			return err
		}

	} else {
		// Now, we get IP address from physical interface. If IP does not
		// exists error out.
		ipAddress, err = getIPv4Address(cluster.GatewayIntf)
		if err != nil {
			return fmt.Errorf("Failed to get interface details for %s (%v)",
				cluster.GatewayIntf, err)
		}
		if ipAddress == "" {
			return fmt.Errorf("%s does not have a ipv4 address",
				cluster.GatewayIntf)
		}
		err = util.GatewayInit(clusterIPSubnet, nodeName, ipAddress,
			cluster.GatewayIntf, "", cluster.GatewayNextHop, subnet)
		if err != nil {
			return err
		}
	}

	if !cluster.NodePortEnable && !cluster.GatewaySpareIntf {
		// Program cluster.GatewayIntf to let non-pod traffic to go to host
		// stack
		err = cluster.addDefaultConntrackRules()
		if err != nil {
			return err
		}
	}
	return nil
}

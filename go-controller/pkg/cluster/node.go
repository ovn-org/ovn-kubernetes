package cluster

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/vishvananda/netlink"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
)

// StartClusterNode learns the subnet assigned to it by the master controller
// and calls the SetupNode script which establishes the logical switch
func (cluster *OvnClusterController) StartClusterNode(name string) error {
	count := 30
	var err error
	var node *kapi.Node
	var subnet *net.IPNet

	for count > 0 {
		if count != 30 {
			time.Sleep(time.Second)
		}
		count--

		// setup the node, create the logical switch
		node, err = cluster.Kube.GetNode(name)
		if err != nil {
			logrus.Errorf("Error starting node %s, no node found - %v", name, err)
			continue
		}

		sub, ok := node.Annotations[OvnHostSubnet]
		if !ok {
			logrus.Errorf("Error starting node %s, no annotation found on node for subnet - %v", name, err)
			continue
		}
		_, subnet, err = net.ParseCIDR(sub)
		if err != nil {
			logrus.Errorf("Invalid hostsubnet found for node %s - %v", node.Name, err)
			return err
		}
		break
	}

	if count == 0 {
		logrus.Errorf("Failed to get node/node-annotation for %s - %v", name, err)
		return err
	}

	nodeIP, err := netutils.GetNodeIP(node.Name)
	if err != nil {
		logrus.Errorf("Failed to obtain node's IP: %v", err)
		return err
	}

	logrus.Infof("Node %s ready for ovn initialization with subnet %s", node.Name, subnet.String())

	err = util.StartOVS()
	if err != nil {
		return err
	}

	args := []string{
		"set",
		"Open_vSwitch",
		".",
		fmt.Sprintf("external_ids:ovn-nb=\"%s\"", cluster.NorthDBClientAuth.GetURL()),
		fmt.Sprintf("external_ids:ovn-remote=\"%s\"", cluster.SouthDBClientAuth.GetURL()),
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", nodeIP),
		"external_ids:ovn-encap-type=\"geneve\"",
		fmt.Sprintf("external_ids:k8s-api-server=\"%s\"", cluster.KubeServer),
		fmt.Sprintf("external_ids:k8s-api-token=\"%s\"", cluster.Token),
	}
	out, err := exec.Command("ovs-vsctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error setting OVS external IDs: %v\n  %q", err, string(out))
	}

	err = util.RestartOvnController()
	if err != nil {
		return err
	}

	// Fetch config file to override default values.
	config.FetchConfig()

	// Update config globals that OVN exec utils use
	cluster.NorthDBClientAuth.SetConfig()

	if err := ovn.CreateManagementPort(node.Name, subnet.String(), cluster.ClusterIPNet.String()); err != nil {
		return err
	}

	if cluster.GatewayInit {
		err = cluster.initGateway(node.Name, cluster.ClusterIPNet.String(),
			subnet.String())
		if err != nil {
			return err
		}
	}

	// Install the CNI config file after all initialization is done
	if runtime.GOOS != "win32" {
		// MkdirAll() returns no error if the path already exists
		err = os.MkdirAll(config.CniConfPath, os.ModeDir)
		if err != nil {
			return err
		}

		// Always create the CNI config for consistency.
		cniConf := config.CniConfPath + "/10-ovn-kubernetes.conf"

		var f *os.File
		f, err = os.OpenFile(cniConf, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		defer f.Close()
		confJSON := fmt.Sprintf("{\"name\":\"ovn-kubernetes\", \"type\":\"%s\"}", config.CniPlugin)
		_, err = f.Write([]byte(confJSON))
		if err != nil {
			return err
		}
	}

	return nil
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

	// Check to see whether the interface is OVS bridge.
	_, _, err := util.RunOVSVsctl("--", "br-exists", cluster.GatewayIntf)
	if err != nil {
		// This is not a OVS bridge. We need to create a OVS bridge
		// and add cluster.GatewayIntf as a port of that bridge.
		err = util.NicToBridge(cluster.GatewayIntf)
		if err != nil {
			return fmt.Errorf("Failed to convert nic %s to OVS bridge (%v)",
				cluster.GatewayIntf, err)
		}
		cluster.GatewayIntf = fmt.Sprintf("br%s", cluster.GatewayIntf)
	}

	// Now, we get IP address from OVS bridge. If IP does not exist, error out.
	ipAddress, err := getIPv4Address(cluster.GatewayIntf)
	if err != nil {
		return fmt.Errorf("Failed to get interface details for %s (%v)",
			cluster.GatewayIntf, err)
	}
	if ipAddress == "" {
		return fmt.Errorf("%s does not have a ipv4 address",
			cluster.GatewayIntf)
	}

	err = util.GatewayInit(clusterIPSubnet, nodeName, ipAddress, "",
		cluster.GatewayIntf, cluster.GatewayNextHop, subnet)

	return err
}

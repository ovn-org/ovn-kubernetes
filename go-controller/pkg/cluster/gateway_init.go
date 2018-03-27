// +build linux

package cluster

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"syscall"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
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

func (cluster *OvnClusterController) addService(
	service *kapi.Service, inport, outport string) {
	if service.Spec.Type != kapi.ServiceTypeNodePort {
		return
	}

	for _, svcPort := range service.Spec.Ports {
		if svcPort.Protocol != kapi.ProtocolTCP &&
			svcPort.Protocol != kapi.ProtocolUDP {
			continue
		}
		protocol := strings.ToLower(string(svcPort.Protocol))

		_, stderr, err := util.RunOVSOfctl("add-flow", cluster.GatewayBridge,
			fmt.Sprintf("priority=100, in_port=%s, %s, tp_dst=%d, actions=%s",
				inport, protocol, svcPort.NodePort, outport))
		if err != nil {
			logrus.Errorf("Failed to add openflow flow on %s for nodePort "+
				"%d, stderr: %q, error: %v", cluster.GatewayBridge,
				svcPort.NodePort, stderr, err)
		}
	}
}

func (cluster *OvnClusterController) deleteService(service *kapi.Service,
	inport string) {
	if service.Spec.Type != kapi.ServiceTypeNodePort {
		return
	}

	for _, svcPort := range service.Spec.Ports {
		if svcPort.Protocol != kapi.ProtocolTCP &&
			svcPort.Protocol != kapi.ProtocolUDP {
			continue
		}

		protocol := strings.ToLower(string(svcPort.Protocol))

		_, stderr, err := util.RunOVSOfctl("del-flows", cluster.GatewayBridge,
			fmt.Sprintf("in_port=%s, %s, tp_dst=%d",
				inport, protocol, svcPort.NodePort))
		if err != nil {
			logrus.Errorf("Failed to delete openflow flow on %s for nodePort "+
				"%d, stderr: %q, error: %v", cluster.GatewayBridge,
				svcPort.NodePort, stderr, err)
		}
	}
}

func (cluster *OvnClusterController) syncServices(services []interface{}) {
	// Get ofport of physical interface
	inport, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", cluster.GatewayIntf, "ofport")
	if err != nil {
		logrus.Errorf("Failed to get ofport of %s, stderr: %q, error: %v",
			cluster.GatewayIntf, stderr, err)
		return
	}

	nodePorts := make(map[string]bool)
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			logrus.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}

		if service.Spec.Type != kapi.ServiceTypeNodePort ||
			len(service.Spec.Ports) == 0 {
			continue
		}

		for _, svcPort := range service.Spec.Ports {
			port := svcPort.NodePort
			if port == 0 {
				continue
			}

			prot := svcPort.Protocol
			if prot != kapi.ProtocolTCP && prot != kapi.ProtocolUDP {
				continue
			}
			protocol := strings.ToLower(string(prot))
			nodePortKey := fmt.Sprintf("%s_%d", protocol, port)
			nodePorts[nodePortKey] = true
		}
	}

	stdout, stderr, err := util.RunOVSOfctl("dump-flows",
		cluster.GatewayBridge)
	if err != nil {
		logrus.Errorf("dump-flows failed: %q (%v)", stderr, err)
		return
	}
	flows := strings.Split(stdout, "\n")

	re, err := regexp.Compile(`tp_dst=(.*?)[, ]`)
	if err != nil {
		logrus.Errorf("regexp compile failed: %v", err)
		return
	}

	for _, flow := range flows {
		group := re.FindStringSubmatch(flow)
		if group == nil {
			continue
		}

		var key string
		if strings.Contains(flow, "tcp") {
			key = fmt.Sprintf("tcp_%s", group[1])
		} else if strings.Contains(flow, "udp") {
			key = fmt.Sprintf("udp_%s", group[1])
		} else {
			continue
		}

		if _, ok := nodePorts[key]; !ok {
			pair := strings.Split(key, "_")
			protocol, port := pair[0], pair[1]

			stdout, _, err := util.RunOVSOfctl(
				"del-flows", cluster.GatewayBridge,
				fmt.Sprintf("in_port=%s, %s, tp_dst=%s",
					inport, protocol, port))
			if err != nil {
				logrus.Errorf("del-flows of %s failed: %q",
					cluster.GatewayBridge, stdout)
			}
		}
	}
}

func (cluster *OvnClusterController) nodePortWatcher() error {
	patchPort := "k8s-patch-" + cluster.GatewayBridge + "-br-int"
	// Get ofport of patchPort
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

	cluster.watchFactory.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			service := obj.(*kapi.Service)
			cluster.addService(service, ofportPhys, ofportPatch)
		},
		UpdateFunc: func(old, new interface{}) {
		},
		DeleteFunc: func(obj interface{}) {
			service := obj.(*kapi.Service)
			cluster.deleteService(service, ofportPhys)
		},
	}, cluster.syncServices)

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

	if cluster.NodePortEnable && !cluster.GatewaySpareIntf {
		// Program cluster.GatewayIntf to let non-pod traffic to go to host
		// stack
		err = cluster.addDefaultConntrackRules()
		if err != nil {
			return err
		}

		// Program cluster.GatewayIntf to let nodePort traffic to go to pods.
		err = cluster.nodePortWatcher()
		if err != nil {
			return err
		}
	}

	return nil
}

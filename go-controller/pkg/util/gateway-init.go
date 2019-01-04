package util

import (
	"fmt"
	"github.com/sirupsen/logrus"
	"net"
	"runtime"
	"strings"
)

// GetK8sClusterRouter returns back the OVN distibuted router
func GetK8sClusterRouter() (string, error) {
	k8sClusterRouter, stderr, err := RunOVNNbctl("--data=bare",
		"--no-heading", "--columns=_uuid", "find", "logical_router",
		"external_ids:k8s-cluster-router=yes")
	if err != nil {
		logrus.Errorf("Failed to get k8s cluster router, stderr: %q, "+
			"error: %v", stderr, err)
		return "", err
	}
	if k8sClusterRouter == "" {
		return "", fmt.Errorf("Failed to get k8s cluster router")
	}

	return k8sClusterRouter, nil
}

func getLocalSystemID() (string, error) {
	localSystemID, stderr, err := RunOVSVsctl("--if-exists", "get",
		"Open_vSwitch", ".", "external_ids:system-id")
	if err != nil {
		logrus.Errorf("No system-id configured in the local host, "+
			"stderr: %q, error: %v", stderr, err)
		return "", err
	}
	if localSystemID == "" {
		return "", fmt.Errorf("No system-id configured in the local host")
	}

	return localSystemID, nil
}

// GetDefaultGatewayRouterIP returns the first gateway logical router name
// and IP address as listed in the OVN database
func GetDefaultGatewayRouterIP() (string, net.IP, error) {
	stdout, stderr, err := RunOVNNbctl("--data=bare", "--format=table",
		"--no-heading", "--columns=name,options", "find", "logical_router",
		"options:lb_force_snat_ip!=-")
	if err != nil {
		return "", nil, fmt.Errorf("failed to get logical routers, stdout: %q, "+
			"stderr: %q, err: %v", stdout, stderr, err)
	}
	// Convert \r\n to \n to support Windows line endings
	stdout = strings.Replace(strings.TrimSpace(stdout), "\r\n", "\n", -1)
	gatewayRouters := strings.Split(stdout, "\n")
	if len(gatewayRouters) == 0 {
		return "", nil, fmt.Errorf("failed to get default gateway router (no routers found)")
	}
	parts := strings.Fields(gatewayRouters[0])
	var ip net.IP
	for _, p := range parts {
		const forceTag string = "lb_force_snat_ip="
		if strings.HasPrefix(p, forceTag) {
			ip = net.ParseIP(p[len(forceTag):])
			break
		}
	}
	if ip == nil {
		return "", nil, fmt.Errorf("failed to parse gateway router %q IP", parts[0])
	}
	return parts[0], ip, nil
}

func ensureGatewayPortAddress(portName string) (string, *net.IPNet, error) {
	routerMac, routerIP, _ := GetPortAddresses(portName, false)
	if routerMac == "" || routerIP == "" {
		// Create the gateway switch port in 'join' if it doesn't exist yet
		stdout, stderr, err := RunOVNNbctl("--wait=sb",
			"--may-exist", "lsp-add", "join", portName,
			"--", "--if-exists", "clear", "logical_switch_port", portName, "dynamic_addresses",
			"--", "lsp-set-addresses", portName, "dynamic")
		if err != nil {
			return "", nil, fmt.Errorf("failed to add logical switch "+
				"port %s, stdout: %q, stderr: %q, error: %v",
				portName, stdout, stderr, err)
		}
		// Should have an address already since we waited for the SB above
		routerMac, routerIP, err = GetPortAddresses(portName, false)
		if err != nil {
			return "", nil, fmt.Errorf("error while waiting for addresses "+
				"for gateway switch port %q: %v", portName, err)
		}
		if routerMac == "" || routerIP == "" {
			return "", nil, fmt.Errorf("empty addresses for gateway "+
				"switch port %q", portName)
		}
	}

	// Grab the 'join' switch prefix length to add to our gateway router's IP
	joinPrefixLen, stderr, err := RunOVNNbctl("--if-exists", "get",
		"logical_switch", "join", "external-ids:join-subnet-prefix-length")
	if err != nil {
		return "", nil, fmt.Errorf("Failed to get 'join' switch external-ids: "+
			"stderr: %q, %v", stderr, err)
	}
	stringCIDR := routerIP + "/" + joinPrefixLen
	ip, cidr, err := net.ParseCIDR(stringCIDR)
	if err != nil {
		return "", nil, fmt.Errorf("Invalid router CIDR %q: %v", stringCIDR, err)
	}
	cidr.IP = ip

	return routerMac, cidr, nil
}

// GatewayInit creates a gateway router for the local chassis.
func GatewayInit(clusterIPSubnet []string, nodeName, nicIP, physicalInterface,
	bridgeInterface, defaultGW, rampoutIPSubnet string,
	gatewayLBEnable bool) error {

	ip, physicalIPNet, err := net.ParseCIDR(nicIP)
	if err != nil {
		return fmt.Errorf("error parsing %s (%v)", nicIP, err)
	}
	n, _ := physicalIPNet.Mask.Size()
	physicalIPMask := fmt.Sprintf("%s/%d", ip.String(), n)
	physicalIP := ip.String()

	if defaultGW != "" {
		defaultgwByte := net.ParseIP(defaultGW)
		defaultGW = defaultgwByte.String()
	}

	k8sClusterRouter, err := GetK8sClusterRouter()
	if err != nil {
		return err
	}

	systemID, err := getLocalSystemID()
	if err != nil {
		return err
	}

	// Create a gateway router.
	gatewayRouter := "GR_" + nodeName
	stdout, stderr, err := RunOVNNbctl("--", "--may-exist", "lr-add",
		gatewayRouter, "--", "set", "logical_router", gatewayRouter,
		"options:chassis="+systemID, "external_ids:physical_ip="+physicalIP)
	if err != nil {
		return fmt.Errorf("Failed to create logical router %v, stdout: %q, "+
			"stderr: %q, error: %v", gatewayRouter, stdout, stderr, err)
	}

	gwSwitchPort := "jtor-" + gatewayRouter
	gwRouterPort := "rtoj-" + gatewayRouter
	routerMac, routerCIDR, err := ensureGatewayPortAddress(gwSwitchPort)
	if err != nil {
		return err
	}

	// Must move the IP from the LSP to the LRP and set the LSP addresses
	// to 'router' in one transaction, because IPAM doesn't consider LSPs that
	// are attached to routers when checking reserved addresses.
	stdout, stderr, err = RunOVNNbctl(
		"--", "--may-exist", "lrp-add", gatewayRouter, gwRouterPort, routerMac, routerCIDR.String(),
		"--", "set", "logical_switch_port", gwSwitchPort, "type=router",
		"options:router-port="+gwRouterPort, "addresses=router")
	if err != nil {
		return fmt.Errorf("failed to add logical port to router, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
	}

	stdout, stderr, err = RunOVNNbctl("set", "logical_router",
		gatewayRouter, "options:lb_force_snat_ip="+routerCIDR.IP.String())
	if err != nil {
		return fmt.Errorf("Failed to set logical router, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
	}

	for _, entry := range clusterIPSubnet {
		// Add a static route in GR with distributed router as the nexthop.
		stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-route-add",
			gatewayRouter, entry, "100.64.0.1")
		if err != nil {
			return fmt.Errorf("Failed to add a static route in GR with distributed "+
				"router as the nexthop, stdout: %q, stderr: %q, error: %v",
				stdout, stderr, err)
		}
	}

	// Add a default route in distributed router with first GR as the nexthop.
	_, defGatewayIP, err := GetDefaultGatewayRouterIP()
	if err != nil {
		return err
	}
	stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-route-add",
		k8sClusterRouter, "0.0.0.0/0", defGatewayIP.String())
	if err != nil {
		return fmt.Errorf("Failed to add a default route in distributed router "+
			"with first GR as the nexthop, stdout: %q, stderr: %q, error: %v",
			stdout, stderr, err)
	}

	if gatewayLBEnable {
		// Create 2 load-balancers for north-south traffic for each gateway
		// router.  One handles UDP and another handles TCP.
		var k8sNSLbTCP, k8sNSLbUDP string
		k8sNSLbTCP, stderr, err = RunOVNNbctl("--data=bare", "--no-heading",
			"--columns=_uuid", "find", "load_balancer",
			"external_ids:TCP_lb_gateway_router="+gatewayRouter)
		if err != nil {
			return fmt.Errorf("Failed to get k8sNSLbTCP, stderr: %q, error: %v",
				stderr, err)
		}
		if k8sNSLbTCP == "" {
			k8sNSLbTCP, stderr, err = RunOVNNbctl("--", "create",
				"load_balancer",
				"external_ids:TCP_lb_gateway_router="+gatewayRouter)
			if err != nil {
				return fmt.Errorf("Failed to create load balancer: "+
					"stderr: %q, error: %v", stderr, err)
			}
		}

		k8sNSLbUDP, stderr, err = RunOVNNbctl("--data=bare", "--no-heading",
			"--columns=_uuid", "find", "load_balancer",
			"external_ids:UDP_lb_gateway_router="+gatewayRouter)
		if err != nil {
			return fmt.Errorf("Failed to get k8sNSLbUDP, stderr: %q, error: %v",
				stderr, err)
		}
		if k8sNSLbUDP == "" {
			k8sNSLbUDP, stderr, err = RunOVNNbctl("--", "create",
				"load_balancer",
				"external_ids:UDP_lb_gateway_router="+gatewayRouter,
				"protocol=udp")
			if err != nil {
				return fmt.Errorf("Failed to create load balancer: "+
					"stderr: %q, error: %v", stderr, err)
			}
		}

		// Add north-south load-balancers to the gateway router.
		stdout, stderr, err = RunOVNNbctl("set", "logical_router",
			gatewayRouter, "load_balancer="+k8sNSLbTCP)
		if err != nil {
			return fmt.Errorf("Failed to set north-south load-balancers to the "+
				"gateway router, stdout: %q, stderr: %q, error: %v",
				stdout, stderr, err)
		}
		stdout, stderr, err = RunOVNNbctl("add", "logical_router",
			gatewayRouter, "load_balancer", k8sNSLbUDP)
		if err != nil {
			return fmt.Errorf("Failed to add north-south load-balancers to the "+
				"gateway router, stdout: %q, stderr: %q, error: %v",
				stdout, stderr, err)
		}
	}

	// Create the external switch for the physical interface to connect to.
	externalSwitch := "ext_" + nodeName
	stdout, stderr, err = RunOVNNbctl("--may-exist", "ls-add",
		externalSwitch)
	if err != nil {
		return fmt.Errorf("Failed to create logical switch, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
	}

	var ifaceID, macAddress string
	if physicalInterface != "" {
		// Connect physical interface to br-int. Get its mac address.
		ifaceID = physicalInterface + "_" + nodeName
		stdout, stderr, err = RunOVSVsctl("--", "--may-exist", "add-port",
			"br-int", physicalInterface, "--", "set", "interface",
			physicalInterface, "external-ids:iface-id="+ifaceID)
		if err != nil {
			return fmt.Errorf("Failed to add port to br-int, stdout: %q, "+
				"stderr: %q, error: %v", stdout, stderr, err)
		}
		macAddress, stderr, err = RunOVSVsctl("--if-exists", "get",
			"interface", physicalInterface, "mac_in_use")
		if err != nil {
			return fmt.Errorf("Failed to get macAddress, stderr: %q, error: %v",
				stderr, err)
		}

		// Flush the IP address of the physical interface.
		_, _, err = RunIP("addr", "flush", "dev", physicalInterface)
		if err != nil {
			return err
		}
	} else {
		// A OVS bridge's mac address can change when ports are added to it.
		// We cannot let that happen, so make the bridge mac address permanent.
		macAddress, stderr, err = RunOVSVsctl("--if-exists", "get",
			"interface", bridgeInterface, "mac_in_use")
		if err != nil {
			return fmt.Errorf("Failed to get macAddress, stderr: %q, error: %v",
				stderr, err)
		}
		if macAddress == "" {
			return fmt.Errorf("No mac_address found for the bridge-interface")
		}
		if runtime.GOOS == windowsOS && macAddress == "00:00:00:00:00:00" {
			macAddress, err = FetchIfMacWindows(bridgeInterface)
			if err != nil {
				return err
			}
		}
		stdout, stderr, err = RunOVSVsctl("set", "bridge",
			bridgeInterface, "other-config:hwaddr="+macAddress)
		if err != nil {
			return fmt.Errorf("Failed to set bridge, stdout: %q, stderr: %q, "+
				"error: %v", stdout, stderr, err)
		}
		ifaceID = bridgeInterface + "_" + nodeName

		// Connect bridge interface to br-int via patch ports.
		patch1 := "k8s-patch-br-int-" + bridgeInterface
		patch2 := "k8s-patch-" + bridgeInterface + "-br-int"

		stdout, stderr, err = RunOVSVsctl("--may-exist", "add-port",
			bridgeInterface, patch2, "--", "set", "interface", patch2,
			"type=patch", "options:peer="+patch1)
		if err != nil {
			return fmt.Errorf("Failed to add port, stdout: %q, stderr: %q, "+
				"error: %v", stdout, stderr, err)
		}

		stdout, stderr, err = RunOVSVsctl("--may-exist", "add-port",
			"br-int", patch1, "--", "set", "interface", patch1, "type=patch",
			"options:peer="+patch2, "external-ids:iface-id="+ifaceID)
		if err != nil {
			return fmt.Errorf("Failed to add port, stdout: %q, stderr: %q, "+
				"error: %v", stdout, stderr, err)
		}
	}

	// Add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	stdout, stderr, err = RunOVNNbctl("--", "--may-exist", "lsp-add",
		externalSwitch, ifaceID, "--", "lsp-set-addresses", ifaceID, "unknown")
	if err != nil {
		return fmt.Errorf("Failed to add logical port to switch, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
	}

	// Connect GR to external_switch with mac address of external interface
	// and that IP address.
	stdout, stderr, err = RunOVNNbctl("--", "--may-exist", "lrp-add",
		gatewayRouter, "rtoe-"+gatewayRouter, macAddress, physicalIPMask,
		"--", "set", "logical_router_port", "rtoe-"+gatewayRouter,
		"external-ids:gateway-physical-ip=yes")
	if err != nil {
		return fmt.Errorf("Failed to add logical port to router, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
	}

	// Connect the external_switch to the router.
	stdout, stderr, err = RunOVNNbctl("--", "--may-exist", "lsp-add",
		externalSwitch, "etor-"+gatewayRouter, "--", "set",
		"logical_switch_port", "etor-"+gatewayRouter, "type=router",
		"options:router-port=rtoe-"+gatewayRouter,
		"addresses="+"\""+macAddress+"\"")
	if err != nil {
		return fmt.Errorf("Failed to add logical port to router, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
	}

	// Add a static route in GR with physical gateway as the default next hop.
	if defaultGW != "" {
		stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-route-add",
			gatewayRouter, "0.0.0.0/0", defaultGW,
			fmt.Sprintf("rtoe-%s", gatewayRouter))
		if err != nil {
			return fmt.Errorf("Failed to add a static route in GR with physical "+
				"gateway as the default next hop, stdout: %q, "+
				"stderr: %q, error: %v", stdout, stderr, err)
		}
	}

	// Default SNAT rules.
	for _, entry := range clusterIPSubnet {
		stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-nat-add",
			gatewayRouter, "snat", physicalIP, entry)
		if err != nil {
			return fmt.Errorf("Failed to create default SNAT rules, stdout: %q, "+
				"stderr: %q, error: %v", stdout, stderr, err)
		}
	}

	// When there are multiple gateway routers (which would be the likely
	// default for any sane deployment), we need to SNAT traffic
	// heading to the logical space with the Gateway router's IP so that
	// return traffic comes back to the same gateway router.
	if rampoutIPSubnet != "" {
		rampoutIPSubnets := strings.Split(rampoutIPSubnet, ",")
		for _, rampoutIPSubnet = range rampoutIPSubnets {
			_, _, err = net.ParseCIDR(rampoutIPSubnet)
			if err != nil {
				continue
			}

			// Add source IP address based routes in distributed router
			// for this gateway router.
			stdout, stderr, err = RunOVNNbctl("--may-exist",
				"--policy=src-ip", "lr-route-add", k8sClusterRouter,
				rampoutIPSubnet, routerCIDR.IP.String())
			if err != nil {
				return fmt.Errorf("Failed to add source IP address based "+
					"routes in distributed router, stdout: %q, "+
					"stderr: %q, error: %v", stdout, stderr, err)
			}
		}
	}

	return nil
}

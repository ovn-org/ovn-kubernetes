package util

import (
	"fmt"
	"github.com/sirupsen/logrus"
	"net"
	"runtime"
	"strings"
)

const (
	// PhysicalNetworkName is the name that maps to an OVS bridge that provides
	// access to physical/external network
	PhysicalNetworkName = "physnet"
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

func lockNBForGateways() error {
	localSystemID, err := getLocalSystemID()
	if err != nil {
		return err
	}

	stdout, stderr, err := RunOVNNbctlWithTimeout(60, "--", "wait-until",
		"nb_global", ".", "external-ids:gateway-lock=\"\"", "--", "set",
		"nb_global", ".", "external_ids:gateway-lock="+localSystemID)
	if err != nil {
		return fmt.Errorf("Failed to set gateway-lock "+
			"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
	}
	return nil
}

func unlockNBForGateways() {
	stdout, stderr, err := RunOVNNbctl("--", "set", "nb_global", ".",
		"external-ids:gateway-lock=\"\"")
	if err != nil {
		logrus.Errorf("Failed to delete lock for gateways, "+
			"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
	}
}

func generateGatewayIP() (string, error) {
	// All the routers connected to "join" switch are in 100.64.0.0/16
	// network and they have their external_ids:connect_to_join set.
	stdout, stderr, err := RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=network", "find", "logical_router_port",
		"external_ids:connect_to_join=yes")
	if err != nil {
		logrus.Errorf("Failed to get logical router ports which connect to "+
			"\"join\" switch, stdout: %q, stderr: %q, error: %v",
			stdout, stderr, err)
		return "", err
	}
	// Convert \r\n to \n to support Windows line endings
	stdout = strings.Replace(strings.TrimSpace(stdout), "\r\n", "\n", -1)
	ips := strings.Split(stdout, "\n")

	ipStart, ipStartNet, _ := net.ParseCIDR("100.64.0.0/16")
	ipMax, _, _ := net.ParseCIDR("100.64.255.255/16")
	n, _ := ipStartNet.Mask.Size()
	for !ipStart.Equal(ipMax) {
		ipStart = NextIP(ipStart)
		used := 0
		for _, v := range ips {
			ipCompare, _, _ := net.ParseCIDR(v)
			if ipStart.String() == ipCompare.String() {
				used = 1
				break
			}
		}
		if used == 1 {
			continue
		} else {
			break
		}
	}
	ipMask := fmt.Sprintf("%s/%d", ipStart.String(), n)
	return ipMask, nil
}

// GatewayInit creates a gateway router for the local chassis.
func GatewayInit(clusterIPSubnet []string, nodeName, nicIP, physicalInterface,
	bridgeInterface, defaultGW, rampoutIPSubnet string, gatewayVLANId uint,
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

	// Connect gateway router to switch "join".
	routerMac, stderr, err := RunOVNNbctl("--if-exist", "get",
		"logical_router_port", "rtoj-"+gatewayRouter, "mac")
	if err != nil {
		return fmt.Errorf("Failed to get logical router port, stderr: %q, "+
			"error: %v", stderr, err)
	}

	var routerIP string
	if routerMac == "" {
		routerMac = GenerateMac()
		if err = func() error {
			err = lockNBForGateways()
			if err != nil {
				return err
			}
			defer unlockNBForGateways()
			routerIP, err = generateGatewayIP()
			if err != nil {
				return err
			}

			stdout, stderr, err = RunOVNNbctl("--", "--may-exist", "lrp-add",
				gatewayRouter, "rtoj-"+gatewayRouter, routerMac, routerIP,
				"--", "set", "logical_router_port", "rtoj-"+gatewayRouter,
				"external_ids:connect_to_join=yes")
			if err != nil {
				return fmt.Errorf("failed to add logical port to router, stdout: %q, "+
					"stderr: %q, error: %v", stdout, stderr, err)
			}
			return nil
		}(); err != nil {
			return err
		}
	}

	if routerIP == "" {
		stdout, stderr, err = RunOVNNbctl("--if-exists", "get",
			"logical_router_port", "rtoj-"+gatewayRouter, "networks")
		if err != nil {
			return fmt.Errorf("failed to get routerIP for %s "+
				"stdout: %q, stderr: %q, error: %v",
				"rtoj-"+gatewayRouter, stdout, stderr, err)
		}
		routerIP = strings.Trim(stdout, "[]\"")
	}

	// Connect the switch "join" to the router.
	stdout, stderr, err = RunOVNNbctl("--", "--may-exist", "lsp-add",
		"join", "jtor-"+gatewayRouter, "--", "set", "logical_switch_port",
		"jtor-"+gatewayRouter, "type=router",
		"options:router-port=rtoj-"+gatewayRouter,
		"addresses="+"\""+routerMac+"\"")
	if err != nil {
		return fmt.Errorf("Failed to add logical port to switch, stdout: %q, "+
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
	stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-route-add",
		k8sClusterRouter, "0.0.0.0/0", "100.64.0.2")
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
				"external_ids:TCP_lb_gateway_router="+gatewayRouter,
				"protocol=tcp")
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
	var localNetArgs = []string{}
	if physicalInterface != "" {
		// Connect physical interface to br-int. Get its mac address.
		ifaceID = physicalInterface + "_" + nodeName
		addPortCmdArgs := []string{"--", "--may-exist", "add-port",
			"br-int", physicalInterface, "--", "set", "interface",
			physicalInterface, "external-ids:iface-id=" + ifaceID}
		if gatewayVLANId != 0 {
			addPortCmdArgs = append(addPortCmdArgs, "--", "set", "port", physicalInterface,
				fmt.Sprintf("tag=%d", gatewayVLANId))
		}
		stdout, stderr, err = RunOVSVsctl(addPortCmdArgs...)
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
		localNetArgs = []string{"--", "lsp-set-type", ifaceID, "localnet",
			"--", "lsp-set-options", ifaceID,
			fmt.Sprintf("network_name=%s", PhysicalNetworkName)}
		if gatewayVLANId != 0 {
			localNetArgs = append(localNetArgs, "--", "set", "logical_switch_port",
				ifaceID, fmt.Sprintf("tag_request=%d", gatewayVLANId))
		}
	}

	// Add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	cmdArgs := []string{"--", "--may-exist", "lsp-add", externalSwitch, ifaceID,
		"--", "lsp-set-addresses", ifaceID, "unknown"}
	cmdArgs = append(cmdArgs, localNetArgs...)
	stdout, stderr, err = RunOVNNbctl(cmdArgs...)
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
	if routerIP != "" {
		routerIPByte, _, err := net.ParseCIDR(routerIP)
		if err != nil {
			return err
		}

		// We need to add a /32 route to the Gateway router's IP, on the
		// cluster router, to ensure that the return traffic goes back
		// to the same gateway router
		stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-route-add",
			k8sClusterRouter, routerIPByte.String(), routerIPByte.String())
		if err != nil {
			return fmt.Errorf("Failed to add /32 route to Gateway router's IP of %q "+
				"on the distributed router, stdout: %q, stderr: %q, error: %v",
				routerIPByte.String(), stdout, stderr, err)
		}

		stdout, stderr, err = RunOVNNbctl("set", "logical_router",
			gatewayRouter, "options:lb_force_snat_ip="+routerIPByte.String())
		if err != nil {
			return fmt.Errorf("Failed to set logical router, stdout: %q, "+
				"stderr: %q, error: %v", stdout, stderr, err)
		}
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
					rampoutIPSubnet, routerIPByte.String())
				if err != nil {
					return fmt.Errorf("Failed to add source IP address based "+
						"routes in distributed router, stdout: %q, "+
						"stderr: %q, error: %v", stdout, stderr, err)
				}
			}
		}
	}

	return nil
}

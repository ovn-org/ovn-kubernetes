package util

import (
	"fmt"
	"github.com/sirupsen/logrus"
	"net"
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
	// All the routers connected to "join" switch are in 100.64.1.0/24
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
	ips := strings.Split(strings.TrimSpace(stdout), "\n")

	ipStart, ipStartNet, _ := net.ParseCIDR("100.64.1.0/24")
	ipMax, _, _ := net.ParseCIDR("100.64.1.255/24")
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
func GatewayInit(clusterIPSubnet, nodeName, nicIP, physicalInterface,
	bridgeInterface, defaultGW, rampoutIPSubnet string,
	gatewayLBEnable bool) error {

	ip, physicalIPNet, err := net.ParseCIDR(nicIP)
	if err != nil {
		logrus.Errorf("error parsing %s (%v)", nicIP, err)
		return err
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
		logrus.Errorf("Failed to create logical router %v, stdout: %q, "+
			"stderr: %q, error: %v", gatewayRouter, stdout, stderr, err)
		return err
	}

	// Connect gateway router to switch "join".
	routerMac, stderr, err := RunOVNNbctl("--if-exist", "get",
		"logical_router_port", "rtoj-"+gatewayRouter, "mac")
	if err != nil {
		logrus.Errorf("Failed to get logical router port, stderr: %q, "+
			"error: %v", stderr, err)
		return err
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
			logrus.Errorf(err.Error())
			return err
		}
	}

	// Connect the switch "join" to the router.
	stdout, stderr, err = RunOVNNbctl("--", "--may-exist", "lsp-add",
		"join", "jtor-"+gatewayRouter, "--", "set", "logical_switch_port",
		"jtor-"+gatewayRouter, "type=router",
		"options:router-port=rtoj-"+gatewayRouter,
		"addresses="+"\""+routerMac+"\"")
	if err != nil {
		logrus.Errorf("Failed to add logical port to switch, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Add a static route in GR with distributed router as the nexthop.
	stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-route-add",
		gatewayRouter, clusterIPSubnet, "100.64.1.1")
	if err != nil {
		logrus.Errorf("Failed to add a static route in GR with distributed "+
			"router as the nexthop, stdout: %q, stderr: %q, error: %v",
			stdout, stderr, err)
		return err
	}

	// Add a default route in distributed router with first GR as the nexthop.
	stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-route-add",
		k8sClusterRouter, "0.0.0.0/0", "100.64.1.2")
	if err != nil {
		logrus.Errorf("Failed to add a default route in distributed router "+
			"with first GR as the nexthop, stdout: %q, stderr: %q, error: %v",
			stdout, stderr, err)
		return err
	}

	if gatewayLBEnable {
		// Create 2 load-balancers for north-south traffic for each gateway
		// router.  One handles UDP and another handles TCP.
		var k8sNSLbTCP, k8sNSLbUDP string
		k8sNSLbTCP, stderr, err = RunOVNNbctl("--data=bare", "--no-heading",
			"--columns=_uuid", "find", "load_balancer",
			"external_ids:TCP_lb_gateway_router="+gatewayRouter)
		if err != nil {
			logrus.Errorf("Failed to get k8sNSLbTCP, stderr: %q, error: %v",
				stderr, err)
			return err
		}
		if k8sNSLbTCP == "" {
			k8sNSLbTCP, stderr, err = RunOVNNbctl("--", "create",
				"load_balancer",
				"external_ids:TCP_lb_gateway_router="+gatewayRouter)
			if err != nil {
				logrus.Errorf("Failed to create load balancer, stdout: %q, "+
					"stderr: %q, error: %v", stdout, stderr, err)
				return err
			}
		}

		k8sNSLbUDP, stderr, err = RunOVNNbctl("--data=bare", "--no-heading",
			"--columns=_uuid", "find", "load_balancer",
			"external_ids:UDP_lb_gateway_router="+gatewayRouter)
		if err != nil {
			logrus.Errorf("Failed to get k8sNSLbUDP, stderr: %q, error: %v",
				stderr, err)
			return err
		}
		if k8sNSLbUDP == "" {
			k8sNSLbUDP, stderr, err = RunOVNNbctl("--", "create",
				"load_balancer",
				"external_ids:UDP_lb_gateway_router="+gatewayRouter,
				"protocol=udp")
			if err != nil {
				logrus.Errorf("Failed to create load balancer, stdout: %q, "+
					"stderr: %q, error: %v", stdout, stderr, err)
				return err
			}
		}

		// Add north-south load-balancers to the gateway router.
		stdout, stderr, err = RunOVNNbctl("set", "logical_router",
			gatewayRouter, "load_balancer="+k8sNSLbTCP)
		if err != nil {
			logrus.Errorf("Failed to set north-south load-balancers to the "+
				"gateway router, stdout: %q, stderr: %q, error: %v",
				stdout, stderr, err)
			return err
		}
		stdout, stderr, err = RunOVNNbctl("add", "logical_router",
			gatewayRouter, "load_balancer", k8sNSLbUDP)
		if err != nil {
			logrus.Errorf("Failed to add north-south load-balancers to the "+
				"gateway router, stdout: %q, stderr: %q, error: %v",
				stdout, stderr, err)
			return err
		}
	}

	// Create the external switch for the physical interface to connect to.
	externalSwitch := "ext_" + nodeName
	stdout, stderr, err = RunOVNNbctl("--may-exist", "ls-add",
		externalSwitch)
	if err != nil {
		logrus.Errorf("Failed to create logical switch, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	var ifaceID, macAddress string
	if physicalInterface != "" {
		// Connect physical interface to br-int. Get its mac address.
		ifaceID = physicalInterface + "_" + nodeName
		stdout, stderr, err = RunOVSVsctl("--", "--may-exist", "add-port",
			"br-int", physicalInterface, "--", "set", "interface",
			physicalInterface, "external-ids:iface-id="+ifaceID)
		if err != nil {
			logrus.Errorf("Failed to add port to br-int, stdout: %q, "+
				"stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
		macAddress, stderr, err = RunOVSVsctl("--if-exists", "get",
			"interface", physicalInterface, "mac_in_use")
		if err != nil {
			logrus.Errorf("Failed to get macAddress, stderr: %q, error: %v",
				stderr, err)
			return err
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
			logrus.Errorf("Failed to get macAddress, stderr: %q, error: %v",
				stderr, err)
			return err
		}
		if macAddress == "" {
			return fmt.Errorf("No mac_address found for the bridge-interface")
		}
		stdout, stderr, err = RunOVSVsctl("set", "bridge",
			bridgeInterface, "other-config:hwaddr="+macAddress)
		if err != nil {
			logrus.Errorf("Failed to set bridge, stdout: %q, stderr: %q, "+
				"error: %v", stdout, stderr, err)
			return err
		}
		ifaceID = bridgeInterface + "_" + nodeName

		// Connect bridge interface to br-int via patch ports.
		patch1 := "k8s-patch-br-int-" + bridgeInterface
		patch2 := "k8s-patch-" + bridgeInterface + "-br-int"

		stdout, stderr, err = RunOVSVsctl("--may-exist", "add-port",
			bridgeInterface, patch2, "--", "set", "interface", patch2,
			"type=patch", "options:peer="+patch1)
		if err != nil {
			logrus.Errorf("Failed to add port, stdout: %q, stderr: %q, "+
				"error: %v", stdout, stderr, err)
			return err
		}

		stdout, stderr, err = RunOVSVsctl("--may-exist", "add-port",
			"br-int", patch1, "--", "set", "interface", patch1, "type=patch",
			"options:peer="+patch2, "external-ids:iface-id="+ifaceID)
		if err != nil {
			logrus.Errorf("Failed to add port, stdout: %q, stderr: %q, "+
				"error: %v", stdout, stderr, err)
			return err
		}
	}

	// Add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	stdout, stderr, err = RunOVNNbctl("--", "--may-exist", "lsp-add",
		externalSwitch, ifaceID, "--", "lsp-set-addresses", ifaceID, "unknown")
	if err != nil {
		logrus.Errorf("Failed to add logical port to switch, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Connect GR to external_switch with mac address of external interface
	// and that IP address.
	stdout, stderr, err = RunOVNNbctl("--", "--may-exist", "lrp-add",
		gatewayRouter, "rtoe-"+gatewayRouter, macAddress, physicalIPMask,
		"--", "set", "logical_router_port", "rtoe-"+gatewayRouter,
		"external-ids:gateway-physical-ip=yes")
	if err != nil {
		logrus.Errorf("Failed to add logical port to router, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Connect the external_switch to the router.
	stdout, stderr, err = RunOVNNbctl("--", "--may-exist", "lsp-add",
		externalSwitch, "etor-"+gatewayRouter, "--", "set",
		"logical_switch_port", "etor-"+gatewayRouter, "type=router",
		"options:router-port=rtoe-"+gatewayRouter,
		"addresses="+"\""+macAddress+"\"")
	if err != nil {
		logrus.Errorf("Failed to add logical port to router, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Add a static route in GR with physical gateway as the default next hop.
	if defaultGW != "" {
		stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-route-add",
			gatewayRouter, "0.0.0.0/0", defaultGW,
			fmt.Sprintf("rtoe-%s", gatewayRouter))
		if err != nil {
			logrus.Errorf("Failed to add a static route in GR with physical "+
				"gateway as the default next hop, stdout: %q, "+
				"stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	// Default SNAT rules.
	stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-nat-add",
		gatewayRouter, "snat", physicalIP, clusterIPSubnet)
	if err != nil {
		logrus.Errorf("Failed to create default SNAT rules, stdout: %q, "+
			"stderr: %q, error: %v", stdout, stderr, err)
		return err
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
		stdout, stderr, err = RunOVNNbctl("set", "logical_router",
			gatewayRouter, "options:lb_force_snat_ip="+routerIPByte.String())
		if err != nil {
			logrus.Errorf("Failed to set logical router, stdout: %q, "+
				"stderr: %q, error: %v", stdout, stderr, err)
			return err
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
					logrus.Errorf("Failed to add source IP address based "+
						"routes in distributed router, stdout: %q, "+
						"stderr: %q, error: %v", stdout, stderr, err)
					return err
				}
			}
		}
	}
	return nil
}

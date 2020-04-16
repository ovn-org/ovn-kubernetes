package util

import (
	"bytes"
	"fmt"
	kapi "k8s.io/api/core/v1"
	"net"
	"sort"
	"strings"

	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

const (
	// PhysicalNetworkName is the name that maps to an OVS bridge that provides
	// access to physical/external network
	PhysicalNetworkName = "physnet"
	// OvnClusterRouter is the name of the distributed router
	OvnClusterRouter     = "ovn_cluster_router"
	JoinSwitchPrefix     = "join_"
	ExternalSwitchPrefix = "ext_"
	GWRouterPrefix       = "GR_"
)

// GetK8sClusterRouter returns back the OVN distributed router. This is meant to be used on the
// master alone. If the worker nodes need to know about distributed cluster router (which they
// don't need to), then they need to use ovn-nbctl call and shouldn't make any assumption on
// how the distributed router is named.
func GetK8sClusterRouter() string {
	return OvnClusterRouter
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

	type gwRouter struct {
		name string
		ip   net.IP
	}

	// Get the list of all gateway router names and IPs
	routers := make([]gwRouter, 0, len(gatewayRouters))
	for _, gwRouterLine := range gatewayRouters {
		parts := strings.Fields(gwRouterLine)
		for _, p := range parts {
			const forceTag string = "lb_force_snat_ip="
			if strings.HasPrefix(p, forceTag) {
				ipStr := p[len(forceTag):]
				if ip := net.ParseIP(ipStr); ip != nil {
					routers = append(routers, gwRouter{parts[0], ip})
				} else {
					klog.Warningf("failed to parse gateway router %q IP %q", parts[0], ipStr)
				}
			}
		}
	}
	if len(routers) == 0 {
		return "", nil, fmt.Errorf("failed to parse gateway routers")
	}

	// Stably sort the list
	sort.Slice(routers, func(i, j int) bool {
		return bytes.Compare(routers[i].ip, routers[j].ip) < 0
	})
	return routers[0].name, routers[0].ip, nil
}

// getGatewayLoadBalancers find TCP, SCTP, UDP load-balancers from gateway router.
func getGatewayLoadBalancers(gatewayRouter string) (string, string, string, error) {
	lbTCP, stderr, err := RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "load_balancer",
		"external_ids:TCP_lb_gateway_router="+gatewayRouter)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get gateway router %q TCP "+
			"loadbalancer, stderr: %q, error: %v", gatewayRouter, stderr, err)
	}

	lbUDP, stderr, err := RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "load_balancer",
		"external_ids:UDP_lb_gateway_router="+gatewayRouter)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get gateway router %q UDP "+
			"loadbalancer, stderr: %q, error: %v", gatewayRouter, stderr, err)
	}

	lbSCTP, stderr, err := RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "load_balancer",
		"external_ids:SCTP_lb_gateway_router="+gatewayRouter)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get gateway router %q SCTP "+
			"loadbalancer, stderr: %q, error: %v", gatewayRouter, stderr, err)
	}
	return lbTCP, lbUDP, lbSCTP, nil
}

// GatewayInit creates a gateway router for the local chassis.
func GatewayInit(clusterIPSubnet []*net.IPNet, hostSubnet *net.IPNet, joinSubnet *net.IPNet, nodeName string, l3GatewayConfig *L3GatewayConfig, sctpSupport bool) error {
	k8sClusterRouter := GetK8sClusterRouter()

	// Create a gateway router.
	gatewayRouter := GWRouterPrefix + nodeName
	physicalIPs := make([]string, len(l3GatewayConfig.IPAddresses))
	for i, ip := range l3GatewayConfig.IPAddresses {
		physicalIPs[i] = ip.IP.String()
	}
	stdout, stderr, err := RunOVNNbctl("--", "--may-exist", "lr-add",
		gatewayRouter, "--", "set", "logical_router", gatewayRouter,
		"options:chassis="+l3GatewayConfig.ChassisID,
		"external_ids:physical_ip="+physicalIPs[0],
		"external_ids:physical_ips="+strings.Join(physicalIPs, ","))
	if err != nil {
		return fmt.Errorf("failed to create logical router %v, stdout: %q, "+
			"stderr: %q, error: %v", gatewayRouter, stdout, stderr, err)
	}

	prefixLen, _ := joinSubnet.Mask.Size()
	gwLRPIp := NextIP(joinSubnet.IP)
	drLRPIp := NextIP(gwLRPIp)
	gwLRPMAC := IPAddrToHWAddr(gwLRPIp)
	drLRPMAC := IPAddrToHWAddr(drLRPIp)

	joinSwitch := JoinSwitchPrefix + nodeName
	// create the per-node join switch
	stdout, stderr, err = RunOVNNbctl("--", "--may-exist", "ls-add", joinSwitch)
	if err != nil {
		return fmt.Errorf("failed to create logical switch %q, stdout: %q, stderr: %q, error: %v",
			joinSwitch, stdout, stderr, err)
	}

	gwSwitchPort := "jtor-" + gatewayRouter
	gwRouterPort := "rtoj-" + gatewayRouter
	stdout, stderr, err = RunOVNNbctl(
		"--", "--may-exist", "lsp-add", joinSwitch, gwSwitchPort,
		"--", "set", "logical_switch_port", gwSwitchPort, "type=router", "options:router-port="+gwRouterPort,
		"addresses=router")
	if err != nil {
		return fmt.Errorf("failed to add port %q to logical switch %q, "+
			"stdout: %q, stderr: %q, error: %v", gwSwitchPort, joinSwitch, stdout, stderr, err)
	}

	_, stderr, err = RunOVNNbctl(
		"--", "--may-exist", "lrp-add", gatewayRouter, gwRouterPort, gwLRPMAC.String(),
		fmt.Sprintf("%s/%d", gwLRPIp.String(), prefixLen))
	if err != nil {
		return fmt.Errorf("failed to add logical router port %q for gateway router %s, "+
			"stderr: %q, error: %v", gwRouterPort, gatewayRouter, stderr, err)
	}

	// jtod/dtoj - patch ports that connect the per-node join switch to distributed router
	drSwitchPort := "jtod-" + nodeName
	drRouterPort := "dtoj-" + nodeName

	// Connect the per-node join switch to the distributed router.
	stdout, stderr, err = RunOVNNbctl(
		"--", "--may-exist", "lsp-add", joinSwitch, drSwitchPort,
		"--", "set", "logical_switch_port", drSwitchPort, "type=router", "options:router-port="+drRouterPort,
		"addresses=router")
	if err != nil {
		return fmt.Errorf("failed to add port %q to logical switch %q, "+
			"stdout: %q, stderr: %q, error: %v", drSwitchPort, joinSwitch, stdout, stderr, err)
	}

	_, stderr, err = RunOVNNbctl(
		"--", "--may-exist", "lrp-add", k8sClusterRouter, drRouterPort, drLRPMAC.String(),
		fmt.Sprintf("%s/%d", drLRPIp.String(), prefixLen))
	if err != nil {
		return fmt.Errorf("failed to add logical router port %q to %s, "+
			"stderr: %q, error: %v", drRouterPort, k8sClusterRouter, stderr, err)
	}

	// When there are multiple gateway routers (which would be the likely
	// default for any sane deployment), we need to SNAT traffic
	// heading to the logical space with the Gateway router's IP so that
	// return traffic comes back to the same gateway router.
	stdout, stderr, err = RunOVNNbctl("set", "logical_router",
		gatewayRouter, "options:lb_force_snat_ip="+gwLRPIp.String())
	if err != nil {
		return fmt.Errorf("failed to set logical router %s's lb_force_snat_ip option, "+
			"stdout: %q, stderr: %q, error: %v", gatewayRouter, stdout, stderr, err)
	}

	for _, entry := range clusterIPSubnet {
		// Add a static route in GR with distributed router as the nexthop.
		stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-route-add",
			gatewayRouter, entry.String(), drLRPIp.String())
		if err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with distributed "+
				"router as the nexthop, stdout: %q, stderr: %q, error: %v",
				gatewayRouter, stdout, stderr, err)
		}
	}

	if l3GatewayConfig.NodePortEnable {
		// Create 3 load-balancers for north-south traffic for each gateway
		// router: UDP, TCP, SCTP
		var k8sNSLbTCP, k8sNSLbUDP, k8sNSLbSCTP string
		k8sNSLbTCP, k8sNSLbUDP, k8sNSLbSCTP, err = getGatewayLoadBalancers(gatewayRouter)
		if err != nil {
			return err
		}
		protoLBMap := map[kapi.Protocol]string{
			kapi.ProtocolTCP:  k8sNSLbTCP,
			kapi.ProtocolUDP:  k8sNSLbUDP,
			kapi.ProtocolSCTP: k8sNSLbSCTP,
		}
		enabledProtos := []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolUDP}
		if sctpSupport {
			enabledProtos = append(enabledProtos, kapi.ProtocolSCTP)
		}
		for _, proto := range enabledProtos {
			if protoLBMap[proto] == "" {
				protoLBMap[proto], stderr, err = RunOVNNbctl("--", "create",
					"load_balancer",
					fmt.Sprintf("external_ids:%s_lb_gateway_router=%s", proto, gatewayRouter),
					fmt.Sprintf("protocol=%s", strings.ToLower(string(proto))))
				if err != nil {
					return fmt.Errorf("failed to create load balancer for gateway router %s for protocol %s: "+
						"stderr: %q, error: %v", gatewayRouter, proto, stderr, err)
				}
			}
		}
		// Add north-south load-balancers to the gateway router.
		lbString := fmt.Sprintf("%s,%s", protoLBMap[kapi.ProtocolTCP], protoLBMap[kapi.ProtocolUDP])
		if sctpSupport {
			lbString = lbString + "," + protoLBMap[kapi.ProtocolSCTP]
		}
		stdout, stderr, err = RunOVNNbctl("set", "logical_router", gatewayRouter, "load_balancer="+lbString)
		if err != nil {
			return fmt.Errorf("failed to set north-south load-balancers to the "+
				"gateway router %s, stdout: %q, stderr: %q, error: %v",
				gatewayRouter, stdout, stderr, err)
		}
	}

	// Create the external switch for the physical interface to connect to.
	externalSwitch := ExternalSwitchPrefix + nodeName
	stdout, stderr, err = RunOVNNbctl("--may-exist", "ls-add",
		externalSwitch)
	if err != nil {
		return fmt.Errorf("failed to create logical switch %s, stdout: %q, "+
			"stderr: %q, error: %v", externalSwitch, stdout, stderr, err)
	}

	// Add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	cmdArgs := []string{
		"--", "--may-exist", "lsp-add", externalSwitch, l3GatewayConfig.InterfaceID,
		"--", "lsp-set-addresses", l3GatewayConfig.InterfaceID, "unknown",
		"--", "lsp-set-type", l3GatewayConfig.InterfaceID, "localnet",
		"--", "lsp-set-options", l3GatewayConfig.InterfaceID, "network_name=" + PhysicalNetworkName}

	if l3GatewayConfig.VLANID != nil {
		lspArgs := []string{
			"--", "set", "logical_switch_port", l3GatewayConfig.InterfaceID,
			fmt.Sprintf("tag_request=%d", *l3GatewayConfig.VLANID),
		}
		cmdArgs = append(cmdArgs, lspArgs...)
	}

	stdout, stderr, err = RunOVNNbctl(cmdArgs...)
	if err != nil {
		return fmt.Errorf("failed to add logical port to switch %s, stdout: %q, "+
			"stderr: %q, error: %v", externalSwitch, stdout, stderr, err)
	}

	// Connect GR to external_switch with mac address of external interface
	// and that IP address. In the case of `local` gateway mode, whenever ovnkube-node container
	// restarts a new br-local bridge will be created with a new `nicMacAddress`. As a result,
	// direct addition of logical_router_port with --may-exists will not work since the MAC
	// has changed. So, we need to delete that port, if it exists, and it back.
	cmdArgs = []string{
		"--", "--if-exists", "lrp-del", "rtoe-" + gatewayRouter,
		"--", "lrp-add", gatewayRouter, "rtoe-" + gatewayRouter,
		l3GatewayConfig.MACAddress.String(),
	}
	for _, ip := range l3GatewayConfig.IPAddresses {
		cmdArgs = append(cmdArgs, ip.String())
	}
	cmdArgs = append(cmdArgs,
		"--", "set", "logical_router_port", "rtoe-"+gatewayRouter,
		"external-ids:gateway-physical-ip=yes")

	stdout, stderr, err = RunOVNNbctl(cmdArgs...)
	if err != nil {
		return fmt.Errorf("failed to add logical port to router %s, stdout: %q, "+
			"stderr: %q, error: %v", gatewayRouter, stdout, stderr, err)
	}

	// Connect the external_switch to the router.
	stdout, stderr, err = RunOVNNbctl("--", "--may-exist", "lsp-add",
		externalSwitch, "etor-"+gatewayRouter, "--", "set",
		"logical_switch_port", "etor-"+gatewayRouter, "type=router",
		"options:router-port=rtoe-"+gatewayRouter,
		"addresses="+"\""+l3GatewayConfig.MACAddress.String()+"\"")
	if err != nil {
		return fmt.Errorf("failed to add logical port to router %s, stdout: %q, "+
			"stderr: %q, error: %v", gatewayRouter, stdout, stderr, err)
	}

	// Add static routes in GR with physical gateway as the default next hop.
	for _, nextHop := range l3GatewayConfig.NextHops {
		var allIPs string
		if utilnet.IsIPv6(nextHop) {
			allIPs = "::/0"
		} else {
			allIPs = "0.0.0.0/0"
		}
		stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-route-add",
			gatewayRouter, allIPs, nextHop.String(),
			fmt.Sprintf("rtoe-%s", gatewayRouter))
		if err != nil {
			return fmt.Errorf("Failed to add a static route in GR %s with physical "+
				"gateway as the default next hop, stdout: %q, "+
				"stderr: %q, error: %v", gatewayRouter, stdout, stderr, err)
		}
	}

	// Add source IP address based routes in distributed router
	// for this gateway router.
	stdout, stderr, err = RunOVNNbctl("--may-exist",
		"--policy=src-ip", "lr-route-add", k8sClusterRouter,
		hostSubnet.String(), gwLRPIp.String())
	if err != nil {
		return fmt.Errorf("failed to add source IP address based "+
			"routes in distributed router %s, stdout: %q, "+
			"stderr: %q, error: %v", k8sClusterRouter, stdout, stderr, err)
	}

	// Default SNAT rules.
	var v4ExternalIP, v6ExternalIP string
	for _, ip := range l3GatewayConfig.IPAddresses {
		if utilnet.IsIPv6(ip.IP) {
			v6ExternalIP = ip.IP.String()
		} else {
			v4ExternalIP = ip.IP.String()
		}
	}
	for _, entry := range clusterIPSubnet {
		var externalIP string
		if utilnet.IsIPv6CIDR(entry) {
			externalIP = v6ExternalIP
		} else {
			externalIP = v4ExternalIP
		}
		stdout, stderr, err = RunOVNNbctl("--may-exist", "lr-nat-add",
			gatewayRouter, "snat", externalIP, entry.String())
		if err != nil {
			return fmt.Errorf("failed to create default SNAT rules for gateway router %s, "+
				"stdout: %q, stderr: %q, error: %v", gatewayRouter, stdout, stderr, err)
		}
	}

	return nil
}

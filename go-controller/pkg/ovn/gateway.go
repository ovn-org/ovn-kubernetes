package ovn

import (
	"bytes"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
)

const (
	// ovnClusterRouter is the name of the distributed router
	ovnClusterRouter = "ovn_cluster_router"

	joinSwitchPrefix     = "join_"
	externalSwitchPrefix = "ext_"
	gwRouterPrefix       = "GR_"
)

func (ovn *Controller) getOvnGateways() ([]string, string, error) {
	// Return all created gateways.
	out, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name", "find",
		"logical_router",
		"options:chassis!=null")
	return strings.Fields(out), stderr, err
}

func (ovn *Controller) getGatewayPhysicalIPs(physicalGateway string) ([]string, error) {
	physicalIPs, _, err := util.RunOVNNbctl("get", "logical_router",
		physicalGateway, "external_ids:physical_ips")
	if err == nil {
		return strings.Split(physicalIPs, ","), nil
	}

	physicalIP, _, err := util.RunOVNNbctl("get", "logical_router",
		physicalGateway, "external_ids:physical_ip")
	if err != nil {
		return nil, err
	}

	return []string{physicalIP}, nil
}

func (ovn *Controller) getGatewayLoadBalancer(physicalGateway string, protocol kapi.Protocol) (string, error) {
	externalIDKey := string(protocol) + "_lb_gateway_router"
	loadBalancer, _, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "load_balancer",
		"external_ids:"+externalIDKey+"="+
			physicalGateway)
	if err != nil {
		return "", err
	}
	return loadBalancer, nil
}

func (ovn *Controller) createGatewayVIPs(protocol kapi.Protocol, sourcePort int32, targetIPs []string, targetPort int32) error {
	klog.V(5).Infof("Creating Gateway VIPs - %s, %d, [%v], %d", protocol, sourcePort, targetIPs, targetPort)

	// Each gateway has a separate load-balancer for N/S traffic

	physicalGateways, _, err := ovn.getOvnGateways()
	if err != nil {
		return err
	}

	for _, physicalGateway := range physicalGateways {
		loadBalancer, err := ovn.getGatewayLoadBalancer(physicalGateway, protocol)
		if err != nil {
			klog.Errorf("physical gateway %s does not have load_balancer (%v)",
				physicalGateway, err)
			continue
		}
		if loadBalancer == "" {
			continue
		}
		physicalIPs, err := ovn.getGatewayPhysicalIPs(physicalGateway)
		if err != nil {
			klog.Errorf("physical gateway %s does not have physical ip (%v)",
				physicalGateway, err)
			continue
		}
		// With the physical_ip:sourcePort as the VIP, add an entry in
		// 'load_balancer'.
		err = ovn.createLoadBalancerVIPs(loadBalancer, physicalIPs, sourcePort, targetIPs, targetPort)
		if err != nil {
			klog.Errorf("Failed to create VIP in load balancer %s - %v", loadBalancer, err)
			continue
		}
	}
	return nil
}

func (ovn *Controller) deleteGatewayVIPs(protocol kapi.Protocol, sourcePort int32) {
	klog.V(5).Infof("Searching to remove Gateway VIPs - %s, %d", protocol, sourcePort)
	physicalGateways, _, err := ovn.getOvnGateways()
	if err != nil {
		klog.Errorf("Error while searching for gateways: %v", err)
		return
	}

	for _, physicalGateway := range physicalGateways {
		loadBalancer, err := ovn.getGatewayLoadBalancer(physicalGateway, protocol)
		if err != nil {
			klog.Errorf("physical gateway %s does not have load_balancer (%v)",
				physicalGateway, err)
			continue
		}
		if loadBalancer == "" {
			continue
		}
		physicalIPs, err := ovn.getGatewayPhysicalIPs(physicalGateway)
		if err != nil {
			klog.Errorf("physical gateway %s does not have physical ip (%v)",
				physicalGateway, err)
			continue
		}
		for _, physicalIP := range physicalIPs {
			// With the physical_ip:sourcePort as the VIP, delete an entry in 'load_balancer'.
			vip := util.JoinHostPortInt32(physicalIP, sourcePort)
			klog.V(5).Infof("Removing gateway VIP: %s from loadbalancer: %s", vip, loadBalancer)
			ovn.deleteLoadBalancerVIP(loadBalancer, vip)
		}
	}
}

// getDefaultGatewayRouterIP returns the first gateway logical router name
// and IP address as listed in the OVN database
func getDefaultGatewayRouterIP() (string, net.IP, error) {
	stdout, stderr, err := util.RunOVNNbctl("--data=bare", "--format=table",
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
	lbTCP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "load_balancer",
		"external_ids:TCP_lb_gateway_router="+gatewayRouter)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get gateway router %q TCP "+
			"loadbalancer, stderr: %q, error: %v", gatewayRouter, stderr, err)
	}

	lbUDP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "load_balancer",
		"external_ids:UDP_lb_gateway_router="+gatewayRouter)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get gateway router %q UDP "+
			"loadbalancer, stderr: %q, error: %v", gatewayRouter, stderr, err)
	}

	lbSCTP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "load_balancer",
		"external_ids:SCTP_lb_gateway_router="+gatewayRouter)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get gateway router %q SCTP "+
			"loadbalancer, stderr: %q, error: %v", gatewayRouter, stderr, err)
	}
	return lbTCP, lbUDP, lbSCTP, nil
}

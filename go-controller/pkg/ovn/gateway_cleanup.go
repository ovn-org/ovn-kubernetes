package ovn

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
)

// gatewayCleanup removes all the NB DB objects created for a node's gateway
func gatewayCleanup(nodeName string, nodeSubnets []*net.IPNet) error {
	gatewayRouter := gwRouterPrefix + nodeName

	// Get the gateway router port's IP address (connected to join switch)
	var routerIP net.IP
	var nextHops []net.IP
	routerIPNetworks, stderr, err := util.RunOVNNbctl("--if-exist", "get",
		"logical_router_port", "rtoj-"+gatewayRouter, "networks")
	if err != nil {
		return fmt.Errorf("failed to get logical router port for gateway router %s, "+
			"stderr: %q, error: %v", gatewayRouter, stderr, err)
	}

	// eg: `["100.64.1.1/29", "fd98:1::/125"]`
	routerIPNetworks = strings.Trim(routerIPNetworks, "[]")
	if routerIPNetworks != "" {
		for _, routerIPNetwork := range strings.Split(routerIPNetworks, ", ") {
			routerIPNetwork = strings.Trim(routerIPNetwork, "\"")
			routerIP, _, err = net.ParseCIDR(routerIPNetwork)
			if err != nil {
				return fmt.Errorf("could not parse logical router port %q: %v",
					routerIPNetwork, err)
			}
			if routerIP != nil {
				nextHops = append(nextHops, routerIP)
			}
		}
	}

	for _, nodeSubnet := range nodeSubnets {
		mgmtIfAddr := util.GetNodeManagementIfAddr(nodeSubnet)
		nextHops = append(nextHops, mgmtIfAddr.IP)
	}
	staticRouteCleanup(nextHops)

	// Remove the join switch that connects ovn_cluster_router to gateway router
	_, stderr, err = util.RunOVNNbctl("--if-exist", "ls-del", joinSwitchPrefix+nodeName)
	if err != nil {
		return fmt.Errorf("failed to delete the join logical switch %s, "+
			"stderr: %q, error: %v", joinSwitchPrefix+nodeName, stderr, err)
	}

	// Remove the gateway router associated with nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "lr-del",
		gatewayRouter)
	if err != nil {
		return fmt.Errorf("failed to delete gateway router %s, stderr: %q, "+
			"error: %v", gatewayRouter, stderr, err)
	}

	// Remove external switch
	externalSwitch := externalSwitchPrefix + nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "ls-del",
		externalSwitch)
	if err != nil {
		return fmt.Errorf("failed to delete external switch %s, stderr: %q, "+
			"error: %v", externalSwitch, stderr, err)
	}

	// Remove the patch port on the distributed router that connects to join switch
	_, stderr, err = util.RunOVNNbctl("--if-exist", "lrp-del", "dtoj-"+nodeName)
	if err != nil {
		return fmt.Errorf("failed to delete the patch port dtoj-%s on distributed router "+
			"stderr: %q, error: %v", nodeName, stderr, err)
	}

	// If exists, remove the TCP, UDP load-balancers created for north-south traffic for gateway router.
	k8sNSLbTCP, k8sNSLbUDP, k8sNSLbSCTP, err := getGatewayLoadBalancers(gatewayRouter)
	if err != nil {
		return err
	}
	protoLBMap := map[kapi.Protocol]string{
		kapi.ProtocolTCP:  k8sNSLbTCP,
		kapi.ProtocolUDP:  k8sNSLbUDP,
		kapi.ProtocolSCTP: k8sNSLbSCTP,
	}
	for proto, uuid := range protoLBMap {
		if uuid != "" {
			_, stderr, err = util.RunOVNNbctl("lb-del", uuid)
			if err != nil {
				return fmt.Errorf("failed to delete Gateway router %s's %s load balancer %s, stderr: %q, "+
					"error: %v", gatewayRouter, proto, uuid, stderr, err)
			}
		}
	}
	return nil
}

func staticRouteCleanup(nextHops []net.IP) {
	for _, nextHop := range nextHops {
		// Get a list of all the routes in cluster router with the next hop IP.
		var uuids string
		uuids, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
			"--columns=_uuid", "find", "logical_router_static_route",
			"nexthop=\""+nextHop.String()+"\"")
		if err != nil {
			klog.Errorf("Failed to fetch all routes with "+
				"IP %s as nexthop, stderr: %q, "+
				"error: %v", nextHop.String(), stderr, err)
			continue
		}

		// Remove all the routes in cluster router with this IP as the nexthop.
		routes := strings.Fields(uuids)
		for _, route := range routes {
			_, stderr, err = util.RunOVNNbctl("--if-exists", "remove",
				"logical_router", ovnClusterRouter, "static_routes", route)
			if err != nil {
				klog.Errorf("Failed to delete static route %s"+
					", stderr: %q, err = %v", route, stderr, err)
				continue
			}
		}
	}
}

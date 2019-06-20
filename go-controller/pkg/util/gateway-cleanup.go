package util

import (
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

// GatewayCleanup removes all the NB DB objects created for a node's gateway
func GatewayCleanup(nodeName string) error {
	// Get the cluster router
	clusterRouter, err := GetK8sClusterRouter()
	if err != nil {
		return fmt.Errorf("failed to get cluster router")
	}

	gatewayRouter := fmt.Sprintf("GR_%s", nodeName)

	// Get the gateway router port's IP address (connected to join switch)
	var routerIP string
	routerIPNetwork, stderr, err := RunOVNNbctl("--if-exist", "get",
		"logical_router_port", "rtoj-"+gatewayRouter, "networks")
	if err != nil {
		return fmt.Errorf("Failed to get logical router port, stderr: %q, "+
			"error: %v", stderr, err)
	}

	if routerIPNetwork != "" {
		routerIPNetwork = strings.Trim(routerIPNetwork, "[]\"")
		if routerIPNetwork != "" {
			routerIP = strings.Split(routerIPNetwork, "/")[0]
		}
	}

	if routerIP != "" {
		// Get a list of all the routes in cluster router with this gateway
		// Router as the next hop.
		var uuids string
		uuids, stderr, err = RunOVNNbctl("--data=bare", "--no-heading",
			"--columns=_uuid", "find", "logical_router_static_route",
			"nexthop="+routerIP)
		if err != nil {
			return fmt.Errorf("Failed to fetch all routes with gateway "+
				"router %s as nexthop, stderr: %q, "+
				"error: %v", gatewayRouter, stderr, err)
		}

		// Remove all the routes in cluster router with this gateway Router
		// as the nexthop.
		routes := strings.Fields(uuids)
		for _, route := range routes {
			_, stderr, err = RunOVNNbctl("--if-exists", "remove",
				"logical_router", clusterRouter, "static_routes", route)
			if err != nil {
				logrus.Errorf("Failed to delete static route %s"+
					", stderr: %q, err = %v", route, stderr, err)
				continue
			}
		}
	}

	// Remove the patch port that connects join switch to gateway router
	_, stderr, err = RunOVNNbctl("--if-exist", "lsp-del", "jtor-"+gatewayRouter)
	if err != nil {
		return fmt.Errorf("Failed to delete logical switch port jtor-%s, "+
			"stderr: %q, error: %v", gatewayRouter, stderr, err)
	}

	// Remove any gateway routers associated with nodeName
	_, stderr, err = RunOVNNbctl("--if-exist", "lr-del",
		gatewayRouter)
	if err != nil {
		return fmt.Errorf("Failed to delete gateway router %s, stderr: %q, "+
			"error: %v", gatewayRouter, stderr, err)
	}

	// Remove external switch
	externalSwitch := "ext_" + nodeName
	_, stderr, err = RunOVNNbctl("--if-exist", "ls-del",
		externalSwitch)
	if err != nil {
		return fmt.Errorf("Failed to delete external switch %s, stderr: %q, "+
			"error: %v", externalSwitch, stderr, err)
	}

	if config.Gateway.NodeportEnable {
		//Remove the TCP, UDP load-balancers created for north-south traffic for gateway router.
		k8sNSLbTCP, k8sNSLbUDP, err := getGatewayLoadBalancers(gatewayRouter)
		if err != nil {
			return err
		}
		_, stderr, err = RunOVNNbctl("lb-del", k8sNSLbTCP)
		if err != nil {
			return fmt.Errorf("Failed to delete Gateway router TCP load balancer %s, stderr: %q, "+
				"error: %v", k8sNSLbTCP, stderr, err)
		}
		_, stderr, err = RunOVNNbctl("lb-del", k8sNSLbUDP)
		if err != nil {
			return fmt.Errorf("Failed to delete Gateway router UDP load balancer %s, stderr: %q, "+
				"error: %v", k8sNSLbTCP, stderr, err)
		}
	}
	return nil
}

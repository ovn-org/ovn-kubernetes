package util

import (
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
)

// RemoveNode removes all the NB DB objects created for that node.
func RemoveNode(nodeName string) error {
	// Get the cluster router
	clusterRouter, err := GetK8sClusterRouter()
	if err != nil {
		return fmt.Errorf("failed to get cluster router")
	}

	// Remove the logical switch associated with nodeName
	_, stderr, err := RunOVNNbctl("--if-exist", "ls-del", nodeName)
	if err != nil {
		return fmt.Errorf("Failed to delete logical switch %s, "+
			"stderr: %q, error: %v", nodeName, stderr, err)
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

	return nil
}

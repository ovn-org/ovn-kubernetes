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
func gatewayCleanup(nodeName string) error {
	gatewayRouter := util.GwRouterPrefix + nodeName

	// Get the gateway router port's IP address (connected to join switch)
	nextHops, err := util.GetNodeGatewayRouterIPs(nodeName)
	if err != nil {
		return fmt.Errorf("failed to get logical router port for gateway router %s, error: %v", gatewayRouter, err)
	}

	staticRouteCleanup(nextHops)

	// Remove the join switch that connects ovn_cluster_router to gateway router
	_, stderr, err := util.RunOVNNbctl("--if-exist", "ls-del", util.JoinSwitchPrefix+nodeName)
	if err != nil {
		return fmt.Errorf("failed to delete the join logical switch %s, "+
			"stderr: %q, error: %v", util.JoinSwitchPrefix+nodeName, stderr, err)
	}

	// Remove the gateway router associated with nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "lr-del",
		gatewayRouter)
	if err != nil {
		return fmt.Errorf("failed to delete gateway router %s, stderr: %q, "+
			"error: %v", gatewayRouter, stderr, err)
	}

	// Remove external switch
	externalSwitch := util.ExternalSwitchPrefix + nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "ls-del",
		externalSwitch)
	if err != nil {
		return fmt.Errorf("failed to delete external switch %s, stderr: %q, "+
			"error: %v", externalSwitch, stderr, err)
	}

	// Remove the patch port on the distributed router that connects to join switch
	_, stderr, err = util.RunOVNNbctl("--if-exist", "lrp-del", util.DistRouterToJoinSwitchPrefix+nodeName)
	if err != nil {
		return fmt.Errorf("failed to delete the patch port %s%s on distributed router "+
			"stderr: %q, error: %v", util.DistRouterToJoinSwitchPrefix, nodeName, stderr, err)
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

	// We don't know the gateway mode as this is running in the master, try to delete the additional local
	// gateway for the shared gateway mode. it will be no op if this is done for other gateway modes.
	delPbrAndNatRules(nodeName)
	return nil
}

func delPbrAndNatRules(nodeName string) {
	// delete the dnat_and_snat entry that we added for the management port IP
	// Note: we don't need to delete any MAC bindings that are dynamically learned from OVN SB DB
	// because there will be none since this NAT is only for outbound traffic and not for inbound
	mgmtPortName := util.K8sPrefix + nodeName
	externalIP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=external_ip", "find", "nat", fmt.Sprintf("logical_port=%s", mgmtPortName))
	if err != nil {
		klog.Errorf("Failed to fetch the dnat_and_snat entry for the management port %s "+
			"stderr: %s, error: %v", mgmtPortName, stderr, err)
		externalIP = ""
	}
	if externalIP != "" {
		_, stderr, err = util.RunOVNNbctl("--if-exists", "lr-nat-del", util.OVNClusterRouter, "dnat_and_snat", externalIP)
		if err != nil {
			klog.Errorf("Failed to delete the dnat_and_snat ip %s associated with the management "+
				"port %s: stderr: %s, error: %v", externalIP, mgmtPortName, stderr, err)
		}
	}

	// find the pbr rules associated with the node and delete them
	matches, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=match",
		"find", "logical_router_policy")
	if err != nil {
		klog.Errorf("Failed to fetch the policy route for the node subnet "+
			"stdout: %s, stderr: %s, error: %v", matches, stderr, err)
		matches = ""
	}
	// matches inport for a policy, must use ending quote to substring match to avoid matching other nodes accidentally
	// i.e. rtos-ovn-worker would match rtos-ovn-worker2
	nodeSubnetMatchSubStr := fmt.Sprintf("%s%s\"", util.RouterToSwitchPrefix, nodeName)
	// for inter node, match the comment in the policy, and include extra space to avoid accidental submatch
	interNodeMatchSubStr := fmt.Sprintf("%s%s ", util.InterPrefix, nodeName)
	// mgmt port policy matches node name in comment, use extra spaces to avoid accidental match of other nodes
	mgmtPortPolicyMatchSubStr := fmt.Sprintf(" %s ", nodeName)
	for _, match := range strings.Split(matches, "\n\n") {
		var priority string
		if strings.Contains(match, nodeSubnetMatchSubStr) {
			priority = nodeSubnetPolicyPriority
		} else if strings.Contains(match, interNodeMatchSubStr) {
			priority = interNodePolicyPriority
		} else if strings.Contains(match, mgmtPortPolicyMatchSubStr) {
			priority = mgmtPortPolicyPriority
		} else {
			continue
		}
		_, stderr, err = util.RunOVNNbctl("lr-policy-del", util.OVNClusterRouter, priority, match)
		if err != nil {
			klog.Errorf("Failed to remove the policy routes (%s, %s) associated with the node %s: stderr: %s, error: %v",
				priority, match, nodeName, stderr, err)
		}
	}
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
				"logical_router", util.OVNClusterRouter, "static_routes", route)
			if err != nil {
				klog.Errorf("Failed to delete static route %s"+
					", stderr: %q, err = %v", route, stderr, err)
				continue
			}
		}
	}
}

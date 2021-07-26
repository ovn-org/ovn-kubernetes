package ovn

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// gatewayCleanup removes all the NB DB objects created for a node's gateway
func (oc *Controller) gatewayCleanup(nodeName string) error {
	gatewayRouter := types.GWRouterPrefix + nodeName

	// Get the gateway router port's IP address (connected to join switch)
	var nextHops []net.IP

	gwIPAddrs, err := util.GetLRPAddrs(oc.nbClient, types.GWRouterToJoinSwitchPrefix+gatewayRouter)
	if err != nil {
		return err
	}

	for _, gwIPAddr := range gwIPAddrs {
		nextHops = append(nextHops, gwIPAddr.IP)
	}
	staticRouteCleanup(nextHops)

	// Remove the patch port that connects join switch to gateway router
	_, stderr, err := util.RunOVNNbctl("--if-exist", "lsp-del", types.JoinSwitchToGWRouterPrefix+gatewayRouter)
	if err != nil {
		return fmt.Errorf("failed to delete logical switch port %s%s: "+
			"stderr: %q, error: %v", types.JoinSwitchToGWRouterPrefix, gatewayRouter, stderr, err)
	}

	// Remove the gateway router associated with nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "lr-del",
		gatewayRouter)
	if err != nil {
		return fmt.Errorf("failed to delete gateway router %s, stderr: %q, "+
			"error: %v", gatewayRouter, stderr, err)
	}

	// Remove external switch
	externalSwitch := types.ExternalSwitchPrefix + nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "ls-del",
		externalSwitch)
	if err != nil {
		return fmt.Errorf("failed to delete external switch %s, stderr: %q, "+
			"error: %v", externalSwitch, stderr, err)
	}

	exGWexternalSwitch := types.ExternalSwitchPrefix + types.ExternalSwitchPrefix + nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "ls-del",
		exGWexternalSwitch)
	if err != nil {
		return fmt.Errorf("failed to delete external switch %s, stderr: %q, "+
			"error: %v", exGWexternalSwitch, stderr, err)
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
	oc.delPbrAndNatRules(nodeName, nil)
	return nil
}

func (oc *Controller) delPbrAndNatRules(nodeName string, lrpTypes []string) {
	// delete the dnat_and_snat entry that we added for the management port IP
	// Note: we don't need to delete any MAC bindings that are dynamically learned from OVN SB DB
	// because there will be none since this NAT is only for outbound traffic and not for inbound
	mgmtPortName := types.K8sPrefix + nodeName
	externalIP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=external_ip", "find", "nat", fmt.Sprintf("logical_port=%s", mgmtPortName))
	if err != nil {
		klog.Errorf("Failed to fetch the dnat_and_snat entry for the management port %s "+
			"stderr: %s, error: %v", mgmtPortName, stderr, err)
		externalIP = ""
	}
	if externalIP != "" {
		_, stderr, err = util.RunOVNNbctl("--if-exists", "lr-nat-del", types.OVNClusterRouter, "dnat_and_snat", externalIP)
		if err != nil {
			klog.Errorf("Failed to delete the dnat_and_snat ip %s associated with the management "+
				"port %s: stderr: %s, error: %v", externalIP, mgmtPortName, stderr, err)
		}
	}

	// delete all logical router policies on ovn_cluster_router
	oc.removeLRPolicies(nodeName, lrpTypes)
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
				"logical_router", types.OVNClusterRouter, "static_routes", route)
			if err != nil {
				klog.Errorf("Failed to delete static route %s"+
					", stderr: %q, err = %v", route, stderr, err)
			}
		}
	}
}

// multiJoinSwitchGatewayCleanup removes the OVN NB gateway logical entities that for
// the obsoleted multiple join switch OVN topology.
//
// if "upgradeOnly" is true, this function only deletes the gateway logical entities that
// are unique for the multiple join switch OVN topology version; this is used for
// upgrading its OVN logical topology to the single join switch version.
//
// if "upgradeOnly" is false, this function deletes all the gateway logical entities of
// the specific node, even some of them are common in both the multiple join switch and
// the single join switch versions; this is to cleanup the logical entities for the
// specified node if the node was deleted when the ovnkube-master pod was brought down
// to do the version upgrade.
func (oc *Controller) multiJoinSwitchGatewayCleanup(nodeName string, upgradeOnly bool) error {
	gatewayRouter := types.GWRouterPrefix + nodeName

	// Get the gateway router port's IP address (connected to join switch)
	var nextHops []net.IP

	gwIPAddrs, err := util.GetLRPAddrs(oc.nbClient, types.GWRouterToJoinSwitchPrefix+gatewayRouter)
	if err != nil {
		return err
	}

	for _, gwIPAddr := range gwIPAddrs {
		// Delete logical router policy whose nexthop is the old rtoj- gateway port address
		logicalRouter := nbdb.LogicalRouter{}
		logicalRouterPolicyRes := []nbdb.LogicalRouterPolicy{}
		opModels := []util.OperationModel{
			{
				ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
					return util.SliceHasStringItem(lrp.Nexthop, gwIPAddr.IP.String())
				},
				ExistingResult: &logicalRouterPolicyRes,
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
				OnModelMutations: func() []model.Mutation {
					if len(logicalRouterPolicyRes) == 0 {
						return nil
					}
					uuids := []string{}
					for _, lrp := range logicalRouterPolicyRes {
						uuids = append(uuids, lrp.UUID)
					}
					return []model.Mutation{
						{
							Field:   &logicalRouter.Policies,
							Mutator: ovsdb.MutateOperationDelete,
							Value:   uuids,
						},
					}
				},
				ExistingResult: &[]nbdb.LogicalRouter{},
			},
		}
		if err := oc.modelClient.Delete(opModels...); err != nil {
			klog.Errorf("Unable to remove LR policy : %s, err: %v", err)
		}
		nextHops = append(nextHops, gwIPAddr.IP)
	}
	staticRouteCleanup(nextHops)

	// Remove the join switch that connects ovn_cluster_router to gateway router
	_, stderr, err := util.RunOVNNbctl("--if-exist", "ls-del", "join_"+nodeName)
	if err != nil {
		return fmt.Errorf("failed to delete the join logical switch %s, "+
			"stderr: %q, error: %v", "join_"+nodeName, stderr, err)
	}

	// Remove the logical router port on the distributed router that connects to the join switch
	_, stderr, err = util.RunOVNNbctl("--if-exist", "lrp-del", "dtoj-"+nodeName)
	if err != nil {
		return fmt.Errorf("failed to delete the patch port dtoj-%s on distributed router "+
			"stderr: %q, error: %v", nodeName, stderr, err)
	}

	// Remove the logical router port on the gateway router that connects to the join switch
	_, stderr, err = util.RunOVNNbctl("--if-exist", "lrp-del", types.GWRouterToJoinSwitchPrefix+gatewayRouter)
	if err != nil {
		return fmt.Errorf("failed to delete the port %s%s on gateway router "+
			"stderr: %q, error: %v", types.GWRouterToJoinSwitchPrefix, gatewayRouter, stderr, err)
	}

	if upgradeOnly {
		return nil
	}

	// Remove the gateway router associated with nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "lr-del",
		gatewayRouter)
	if err != nil {
		return fmt.Errorf("failed to delete gateway router %s, stderr: %q, "+
			"error: %v", gatewayRouter, stderr, err)
	}

	// Remove external switch
	externalSwitch := types.ExternalSwitchPrefix + nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "ls-del",
		externalSwitch)
	if err != nil {
		return fmt.Errorf("failed to delete external switch %s, stderr: %q, "+
			"error: %v", externalSwitch, stderr, err)
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
	oc.delPbrAndNatRules(nodeName, nil)
	return nil
}

// remove Logical Router Policy on ovn_cluster_router for a specific node.
// Specify priorities to only delete specific types
func (oc *Controller) removeLRPolicies(nodeName string, priorities []string) {
	if len(priorities) == 0 {
		priorities = []string{types.InterNodePolicyPriority, types.NodeSubnetPolicyPriority, types.MGMTPortPolicyPriority}
	}
	for _, priority := range priorities {
		intPriority, _ := strconv.Atoi(priority)

		logicalRouter := nbdb.LogicalRouter{}
		result := []nbdb.LogicalRouterPolicy{}
		opModels := []util.OperationModel{
			{
				Model: &nbdb.LogicalRouterPolicy{},
				ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
					return strings.Contains(lrp.Match, fmt.Sprintf("%s ", nodeName)) && lrp.Priority == intPriority
				},
				ExistingResult: &result,
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
				OnModelMutations: func() []model.Mutation {
					if len(result) == 0 {
						return nil
					}
					uuids := []string{}
					for _, item := range result {
						uuids = append(uuids, item.UUID)
					}
					return []model.Mutation{
						{
							Field:   &logicalRouter.Policies,
							Mutator: ovsdb.MutateOperationDelete,
							Value:   uuids,
						},
					}
				},
				ExistingResult: &[]nbdb.LogicalRouter{},
			},
		}
		if err := oc.modelClient.Delete(opModels...); err != nil {
			klog.Errorf("Failed to remove the policy routes %s associated with the node %s, error: %v", priority, nodeName, err)
		}
	}
}

// removes DGP, snat_and_dnat entries, and LRPs
func (oc *Controller) cleanupDGP(nodes *kapi.NodeList) error {
	// remove dnat_snat entries as well as LRPs
	for _, node := range nodes.Items {
		oc.delPbrAndNatRules(node.Name, []string{types.InterNodePolicyPriority, types.MGMTPortPolicyPriority})
	}
	// remove SBDB MAC bindings for DGP
	for _, ip := range []string{types.V4NodeLocalNATSubnetNextHop, types.V6NodeLocalNATSubnetNextHop} {
		uuid, stderr, err := util.RunOVNSbctl("--columns=_uuid", "--no-headings", "find", "mac_binding",
			fmt.Sprintf(`ip="%s"`, ip))
		if err != nil {
			return fmt.Errorf("unable to get DGP MAC binding, err: %v, stderr: %s", err, stderr)
		}
		if len(uuid) > 0 {
			_, stderr, err = util.RunOVNSbctl("destroy", "mac_binding", uuid)
			if err != nil {
				return fmt.Errorf("unable to remove mac_binding for DGP, err: %v, stderr: %s", err, stderr)
			}
		}
	}
	// remove node local switch
	_, stderr, err := util.RunOVNNbctl("--if-exists", "ls-del", types.NodeLocalSwitch)
	if err != nil {
		return fmt.Errorf("unable to remove node local switch, err: %v, stderr: %s", err, stderr)
	}
	dgpName := types.RouterToSwitchPrefix + types.NodeLocalSwitch

	// remove lrp on ovn_cluster_router. Will also remove gateway chassis.
	_, stderr, err = util.RunOVNNbctl("--if-exists", "lrp-del", dgpName)
	if err != nil {
		return fmt.Errorf("unable to delete DGP LRP, error: %v, stderr: %s", err, stderr)
	}
	return nil
}

package ovn

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// gatewayCleanup removes all the NB DB objects created for a node's gateway
func (oc *DefaultNetworkController) gatewayCleanup(nodeName string) error {
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
	oc.staticRouteCleanup(nextHops)
	oc.policyRouteCleanup(nextHops)

	// Remove the patch port that connects join switch to gateway router
	portName := types.JoinSwitchToGWRouterPrefix + gatewayRouter
	lsp := nbdb.LogicalSwitchPort{Name: portName}
	sw := nbdb.LogicalSwitch{Name: types.OVNJoinSwitch}
	err = libovsdbops.DeleteLogicalSwitchPorts(oc.nbClient, &sw, &lsp)
	if err != nil {
		return fmt.Errorf("failed to delete logical switch port %s from switch %s: %v", portName, types.OVNJoinSwitch, err)
	}

	// Remove the logical router port on the gateway router that connects to the join switch
	logicalRouter := nbdb.LogicalRouter{Name: gatewayRouter}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name: types.GWRouterToJoinSwitchPrefix + gatewayRouter,
	}
	err = libovsdbops.DeleteLogicalRouterPorts(oc.nbClient, &logicalRouter, &logicalRouterPort)
	if err != nil {
		return fmt.Errorf("failed to delete port %s on router %s: %v", logicalRouterPort.Name, gatewayRouter, err)
	}

	// Remove the gateway router associated with nodeName
	err = libovsdbops.DeleteLogicalRouter(oc.nbClient, &logicalRouter)
	if err != nil {
		return fmt.Errorf("failed to delete gateway router %s: %v", gatewayRouter, err)
	}

	// Remove external switch
	externalSwitch := types.ExternalSwitchPrefix + nodeName
	err = libovsdbops.DeleteLogicalSwitch(oc.nbClient, externalSwitch)
	if err != nil {
		return fmt.Errorf("failed to delete external switch %s: %v", externalSwitch, err)
	}

	exGWexternalSwitch := types.EgressGWSwitchPrefix + types.ExternalSwitchPrefix + nodeName
	err = libovsdbops.DeleteLogicalSwitch(oc.nbClient, exGWexternalSwitch)
	if err != nil {
		return fmt.Errorf("failed to delete external switch %s: %v", exGWexternalSwitch, err)
	}

	// This will cleanup the NodeSubnetPolicy in local and shared gateway modes. It will be a no-op for any other mode.
	oc.delPbrAndNatRules(nodeName, nil)
	return nil
}

func (oc *DefaultNetworkController) delPbrAndNatRules(nodeName string, lrpTypes []string) {
	// delete the dnat_and_snat entry that we added for the management port IP
	// Note: we don't need to delete any MAC bindings that are dynamically learned from OVN SB DB
	// because there will be none since this NAT is only for outbound traffic and not for inbound
	mgmtPortName := types.K8sPrefix + nodeName
	nat := libovsdbops.BuildDNATAndSNAT(nil, nil, mgmtPortName, "", nil)
	logicalRouter := nbdb.LogicalRouter{
		Name: types.OVNClusterRouter,
	}
	err := libovsdbops.DeleteNATs(oc.nbClient, &logicalRouter, nat)
	if err != nil {
		klog.Errorf("Failed to delete the dnat_and_snat associated with the management port %s: %v", mgmtPortName, err)
	}

	// delete all logical router policies on ovn_cluster_router
	oc.removeLRPolicies(nodeName, lrpTypes)
}

func (oc *DefaultNetworkController) staticRouteCleanup(nextHops []net.IP) {
	ips := sets.String{}
	for _, nextHop := range nextHops {
		ips.Insert(nextHop.String())
	}
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return ips.Has(item.Nexthop)
	}
	err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(oc.nbClient, types.OVNClusterRouter, p)
	if err != nil {
		klog.Errorf("Failed to delete static route for nexthops %+v: %v", ips.UnsortedList(), err)
	}
}

// policyRouteCleanup cleansup all policies on cluster router that have a nextHop
// in the provided list.
// - if the LRP exists and has the len(nexthops) > 1: it removes
// the specified gatewayRouterIP from nexthops
// - if the LRP exists and has the len(nexthops) == 1: it removes
// the LRP completely
func (oc *DefaultNetworkController) policyRouteCleanup(nextHops []net.IP) {
	for _, nextHop := range nextHops {
		gwIP := nextHop.String()
		policyPred := func(item *nbdb.LogicalRouterPolicy) bool {
			for _, nexthop := range item.Nexthops {
				if nexthop == gwIP {
					return true
				}
			}
			return false
		}
		err := libovsdbops.DeleteNextHopFromLogicalRouterPoliciesWithPredicate(oc.nbClient, types.OVNClusterRouter, policyPred, gwIP)
		if err != nil {
			klog.Errorf("Failed to delete policy route for nexthop %+v: %v", nextHop, err)
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
func (oc *DefaultNetworkController) multiJoinSwitchGatewayCleanup(nodeName string, upgradeOnly bool) error {
	gatewayRouter := types.GWRouterPrefix + nodeName

	// Get the gateway router port's IP address (connected to join switch)

	gwIPAddrs, err := util.GetLRPAddrs(oc.nbClient, types.GWRouterToJoinSwitchPrefix+gatewayRouter)
	if err != nil {
		return err
	}

	var nextHops []net.IP
	nextHopsStr := sets.String{}
	for _, gwIPAddr := range gwIPAddrs {
		nextHops = append(nextHops, gwIPAddr.IP)
		nextHopsStr.Insert(gwIPAddr.IP.String())
	}

	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Nexthop != nil && nextHopsStr.Has(*item.Nexthop)
	}
	err = libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, types.OVNClusterRouter, p)
	if err != nil {
		klog.Errorf("Error deleting policies with nexthops %+v: %v", nextHopsStr.UnsortedList(), err)
	}

	oc.staticRouteCleanup(nextHops)
	oc.policyRouteCleanup(nextHops)

	// Remove the join switch that connects ovn_cluster_router to gateway router
	joinSwitchName := types.JoinSwitchPrefix + nodeName
	err = libovsdbops.DeleteLogicalSwitch(oc.nbClient, joinSwitchName)
	if err != nil {
		return fmt.Errorf("failed to delete the join logical switch %s: %v", joinSwitchName, err)
	}

	// Remove the logical router port on the distributed router that connects to the join switch
	logicalRouter := nbdb.LogicalRouter{Name: types.OVNClusterRouter}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name: types.DistRouterToJoinSwitchPrefix + nodeName,
	}
	err = libovsdbops.DeleteLogicalRouterPorts(oc.nbClient, &logicalRouter, &logicalRouterPort)
	if err != nil {
		return fmt.Errorf("failed to delete the patch port %s on router %s: %v", logicalRouterPort.Name, types.OVNClusterRouter, err)
	}

	// Remove the logical router port on the gateway router that connects to the join switch
	logicalRouter = nbdb.LogicalRouter{Name: gatewayRouter}
	logicalRouterPort = nbdb.LogicalRouterPort{
		Name: types.GWRouterToJoinSwitchPrefix + gatewayRouter,
	}
	err = libovsdbops.DeleteLogicalRouterPorts(oc.nbClient, &logicalRouter, &logicalRouterPort)
	if err != nil {
		return fmt.Errorf("failed to delete port %s on router %s: %v", logicalRouterPort.Name, gatewayRouter, err)
	}

	if upgradeOnly {
		return nil
	}

	// Remove the gateway router associated with nodeName
	err = libovsdbops.DeleteLogicalRouter(oc.nbClient, &logicalRouter)
	if err != nil {
		return fmt.Errorf("failed to delete gateway router %s: %v", gatewayRouter, err)
	}

	// Remove external switch
	extSwitchName := types.ExternalSwitchPrefix + nodeName
	err = libovsdbops.DeleteLogicalSwitch(oc.nbClient, extSwitchName)
	if err != nil {
		return fmt.Errorf("failed to delete external switch %s: %v", extSwitchName, err)
	}

	// This will cleanup the NodeSubnetPolicy in local and shared gateway modes. It will be a no-op for any other mode.
	oc.delPbrAndNatRules(nodeName, nil)
	return nil
}

// remove Logical Router Policy on ovn_cluster_router for a specific node.
// Specify priorities to only delete specific types
func (oc *DefaultNetworkController) removeLRPolicies(nodeName string, priorities []string) {
	if len(priorities) == 0 {
		priorities = []string{types.NodeSubnetPolicyPriority}
	}

	intPriorities := sets.Int{}
	for _, priority := range priorities {
		intPriority, _ := strconv.Atoi(priority)
		intPriorities.Insert(intPriority)
	}

	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return strings.Contains(item.Match, fmt.Sprintf("%s ", nodeName)) && intPriorities.Has(item.Priority)
	}
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, types.OVNClusterRouter, p)
	if err != nil {
		klog.Errorf("Error deleting policies with priorities %v associated with the node %s: %v", priorities, nodeName, err)
	}
}

// removes DGP, snat_and_dnat entries, and LRPs
func (oc *DefaultNetworkController) cleanupDGP(nodes *kapi.NodeList) error {
	klog.Infof("Removing DGP %v", nodes)
	// remove dnat_snat entries as well as LRPs
	for _, node := range nodes.Items {
		oc.delPbrAndNatRules(node.Name, []string{types.InterNodePolicyPriority, types.MGMTPortPolicyPriority})
	}
	// remove SBDB MAC bindings for DGP
	p := func(item *sbdb.MACBinding) bool {
		return item.IP == types.V4NodeLocalNATSubnetNextHop || item.IP == types.V6NodeLocalNATSubnetNextHop
	}
	err := libovsdbops.DeleteMacBindingWithPredicate(oc.sbClient, p)
	if err != nil {
		return fmt.Errorf("unable to remove mac_binding for DGP: %v", err)
	}

	// remove node local switch
	err = libovsdbops.DeleteLogicalSwitch(oc.nbClient, types.NodeLocalSwitch)
	if err != nil {
		return fmt.Errorf("unable to remove node local switch %s, err: %v", types.NodeLocalSwitch, err)
	}

	// remove lrp on ovn_cluster_router. Will also remove gateway chassis.
	logicalRouter := nbdb.LogicalRouter{Name: types.OVNClusterRouter}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name: types.RouterToSwitchPrefix + types.NodeLocalSwitch,
	}
	err = libovsdbops.DeleteLogicalRouterPorts(oc.nbClient, &logicalRouter, &logicalRouterPort)
	if err != nil {
		return fmt.Errorf("unable to delete router port %s: %v", logicalRouterPort.Name, err)
	}

	return nil
}

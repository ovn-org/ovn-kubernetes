package ovn

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/gateway"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// TODO(dceara): do we need to move this one to base_network_controller.go?
// gatewayCleanup removes all the NB DB objects created for a node's gateway
func (bnc *BaseNetworkController) gatewayCleanup(nodeName string) error {
	gatewayRouter := bnc.GetNetworkScopedGWRouterName(nodeName)

	// Get the gateway router port's IP address (connected to join switch)
	var nextHops []net.IP

	gwIPAddrs, err := libovsdbutil.GetLRPAddrs(bnc.nbClient, types.GWRouterToJoinSwitchPrefix+gatewayRouter)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return err
	}

	for _, gwIPAddr := range gwIPAddrs {
		nextHops = append(nextHops, gwIPAddr.IP)
	}
	bnc.staticRouteCleanup(nextHops)
	bnc.policyRouteCleanup(nextHops)

	// Remove the patch port that connects join switch to gateway router
	portName := types.JoinSwitchToGWRouterPrefix + gatewayRouter
	lsp := nbdb.LogicalSwitchPort{Name: portName}
	sw := nbdb.LogicalSwitch{Name: bnc.GetNetworkScopedJoinSwitchName()}
	err = libovsdbops.DeleteLogicalSwitchPorts(bnc.nbClient, &sw, &lsp)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete logical switch port %s from switch %s: %w", portName, sw.Name, err)
	}

	// Remove the logical router port on the gateway router that connects to the join switch
	logicalRouter := nbdb.LogicalRouter{Name: gatewayRouter}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name: types.GWRouterToJoinSwitchPrefix + gatewayRouter,
	}
	err = libovsdbops.DeleteLogicalRouterPorts(bnc.nbClient, &logicalRouter, &logicalRouterPort)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete port %s on router %s: %w", logicalRouterPort.Name, gatewayRouter, err)
	}

	// Remove the static mac bindings of the gateway router
	err = gateway.DeleteDummyGWMacBindings(bnc.nbClient, gatewayRouter)
	if err != nil {
		return fmt.Errorf("failed to delete GR dummy mac bindings for node %s: %w", bnc.GetNetworkScopedName(nodeName), err)
	}

	// Remove the gateway router associated with nodeName
	err = libovsdbops.DeleteLogicalRouter(bnc.nbClient, &logicalRouter)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete gateway router %s: %w", gatewayRouter, err)
	}

	// Remove external switch
	externalSwitch := bnc.GetNetworkScopedExtSwitchName(nodeName)
	err = libovsdbops.DeleteLogicalSwitch(bnc.nbClient, externalSwitch)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete external switch %s: %w", externalSwitch, err)
	}

	exGWexternalSwitch := types.EgressGWSwitchPrefix + externalSwitch
	err = libovsdbops.DeleteLogicalSwitch(bnc.nbClient, exGWexternalSwitch)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete external switch %s: %w", exGWexternalSwitch, err)
	}

	// This will cleanup the NodeSubnetPolicy in local and shared gateway modes. It will be a no-op for any other mode.
	bnc.delPbrAndNatRules(nodeName, nil)
	return nil
}

// TODO(dceara): do we need to move this one to base_network_controller.go?
func (bnc *BaseNetworkController) delPbrAndNatRules(nodeName string, lrpTypes []string) {
	// delete the dnat_and_snat entry that we added for the management port IP
	// Note: we don't need to delete any MAC bindings that are dynamically learned from OVN SB DB
	// because there will be none since this NAT is only for outbound traffic and not for inbound
	mgmtPortName := bnc.GetNetworkScopedK8sMgmtIntfName(nodeName)
	nat := libovsdbops.BuildDNATAndSNAT(nil, nil, mgmtPortName, "", nil)
	logicalRouter := nbdb.LogicalRouter{
		Name: bnc.GetNetworkScopedClusterRouterName(),
	}
	err := libovsdbops.DeleteNATs(bnc.nbClient, &logicalRouter, nat)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Errorf("Failed to delete the dnat_and_snat associated with the management port %s: %v", mgmtPortName, err)
	}

	// delete all logical router policies on ovn_cluster_router
	bnc.removeLRPolicies(nodeName, lrpTypes)
}

// TODO(dceara): do we need to move this one to base_network_controller.go?
func (bnc *BaseNetworkController) staticRouteCleanup(nextHops []net.IP) {
	ips := sets.Set[string]{}
	for _, nextHop := range nextHops {
		ips.Insert(nextHop.String())
	}
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return ips.Has(item.Nexthop)
	}
	err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(bnc.nbClient,
		bnc.GetNetworkScopedClusterRouterName(), p)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Errorf("Failed to delete static route for nexthops %+v: %v", ips.UnsortedList(), err)
	}
}

// TODO(dceara): do we need to move this one to base_network_controller.go?
// policyRouteCleanup cleansup all policies on cluster router that have a nextHop
// in the provided list.
// - if the LRP exists and has the len(nexthops) > 1: it removes
// the specified gatewayRouterIP from nexthops
// - if the LRP exists and has the len(nexthops) == 1: it removes
// the LRP completely
func (bnc *BaseNetworkController) policyRouteCleanup(nextHops []net.IP) {
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
		err := libovsdbops.DeleteNextHopFromLogicalRouterPoliciesWithPredicate(bnc.nbClient,
			bnc.GetNetworkScopedClusterRouterName(), policyPred, gwIP)
		if err != nil && err != libovsdbclient.ErrNotFound {
			klog.Errorf("Failed to delete policy route for nexthop %+v: %v", nextHop, err)
		}
	}
}

// TODO(dceara): do we need to move this one to base_network_controller.go?
// remove Logical Router Policy on ovn_cluster_router for a specific node.
// Specify priorities to only delete specific types
func (bnc *BaseNetworkController) removeLRPolicies(nodeName string, priorities []string) {
	switchName := bnc.GetNetworkScopedName(nodeName)

	if len(priorities) == 0 {
		priorities = []string{types.NodeSubnetPolicyPriority}
	}

	intPriorities := sets.Set[int]{}
	for _, priority := range priorities {
		intPriority, _ := strconv.Atoi(priority)
		intPriorities.Insert(intPriority)
	}

	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return strings.Contains(item.Match, fmt.Sprintf("%s ", switchName)) && intPriorities.Has(item.Priority)
	}
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(bnc.nbClient, bnc.GetNetworkScopedName(types.OVNClusterRouter), p)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Errorf("Error deleting policies with priorities %v associated with the node %s: %v", priorities, switchName, err)
	}
}

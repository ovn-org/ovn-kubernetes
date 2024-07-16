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

// gatewayCleanup removes all the NB DB objects created for a node's gateway
func (oc *DefaultNetworkController) gatewayCleanup(nodeName string) error {
	gatewayRouter := oc.GetNetworkScopedGWRouterName(nodeName)

	// Get the gateway router port's IP address (connected to join switch)
	var nextHops []net.IP

	gwIPAddrs, err := libovsdbutil.GetLRPAddrs(oc.nbClient, types.GWRouterToJoinSwitchPrefix+gatewayRouter)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return err
	}

	for _, gwIPAddr := range gwIPAddrs {
		nextHops = append(nextHops, gwIPAddr.IP)
	}
	staticRouteCleanup(oc.nbClient, nextHops)
	oc.policyRouteCleanup(nextHops)

	// Remove the patch port that connects join switch to gateway router
	portName := types.JoinSwitchToGWRouterPrefix + gatewayRouter
	lsp := nbdb.LogicalSwitchPort{Name: portName}
	sw := nbdb.LogicalSwitch{Name: oc.GetNetworkScopedJoinSwitchName()}
	err = libovsdbops.DeleteLogicalSwitchPorts(oc.nbClient, &sw, &lsp)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete logical switch port %s from switch %s: %w", portName, sw.Name, err)
	}

	// Remove the logical router port on the gateway router that connects to the join switch
	logicalRouter := nbdb.LogicalRouter{Name: gatewayRouter}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name: types.GWRouterToJoinSwitchPrefix + gatewayRouter,
	}
	err = libovsdbops.DeleteLogicalRouterPorts(oc.nbClient, &logicalRouter, &logicalRouterPort)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete port %s on router %s: %w", logicalRouterPort.Name, gatewayRouter, err)
	}

	// Remove the static mac bindings of the gateway router
	err = gateway.DeleteDummyGWMacBindings(oc.nbClient, gatewayRouter)
	if err != nil {
		return fmt.Errorf("failed to delete GR dummy mac bindings for node %s: %w", nodeName, err)
	}

	// Remove the gateway router associated with nodeName
	err = libovsdbops.DeleteLogicalRouter(oc.nbClient, &logicalRouter)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete gateway router %s: %w", gatewayRouter, err)
	}

	// Remove external switch
	externalSwitch := oc.GetNetworkScopedExtSwitchName(nodeName)
	err = libovsdbops.DeleteLogicalSwitch(oc.nbClient, externalSwitch)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete external switch %s: %w", externalSwitch, err)
	}

	exGWexternalSwitch := types.EgressGWSwitchPrefix + externalSwitch
	err = libovsdbops.DeleteLogicalSwitch(oc.nbClient, exGWexternalSwitch)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete external switch %s: %w", exGWexternalSwitch, err)
	}

	// This will cleanup the NodeSubnetPolicy in local and shared gateway modes. It will be a no-op for any other mode.
	oc.delPbrAndNatRules(nodeName, nil)
	return nil
}

func (oc *DefaultNetworkController) delPbrAndNatRules(nodeName string, lrpTypes []string) {
	// delete the dnat_and_snat entry that we added for the management port IP
	// Note: we don't need to delete any MAC bindings that are dynamically learned from OVN SB DB
	// because there will be none since this NAT is only for outbound traffic and not for inbound
	mgmtPortName := oc.GetNetworkScopedK8sMgmtIntfName(nodeName)
	nat := libovsdbops.BuildDNATAndSNAT(nil, nil, mgmtPortName, "", nil)
	logicalRouter := nbdb.LogicalRouter{
		Name: oc.GetNetworkScopedClusterRouterName(),
	}
	err := libovsdbops.DeleteNATs(oc.nbClient, &logicalRouter, nat)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Errorf("Failed to delete the dnat_and_snat associated with the management port %s: %v", mgmtPortName, err)
	}

	// delete all logical router policies on ovn_cluster_router
	oc.removeLRPolicies(nodeName, lrpTypes)
}

func staticRouteCleanup(nbClient libovsdbclient.Client, nextHops []net.IP) {
	ips := sets.Set[string]{}
	for _, nextHop := range nextHops {
		ips.Insert(nextHop.String())
	}
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return ips.Has(item.Nexthop)
	}
	err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(nbClient, types.OVNClusterRouter, p)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
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
		if err != nil && err != libovsdbclient.ErrNotFound {
			klog.Errorf("Failed to delete policy route for nexthop %+v: %v", nextHop, err)
		}
	}
}

// remove Logical Router Policy on ovn_cluster_router for a specific node.
// Specify priorities to only delete specific types
func (oc *DefaultNetworkController) removeLRPolicies(nodeName string, priorities []string) {
	if len(priorities) == 0 {
		priorities = []string{types.NodeSubnetPolicyPriority}
	}

	intPriorities := sets.Set[int]{}
	for _, priority := range priorities {
		intPriority, _ := strconv.Atoi(priority)
		intPriorities.Insert(intPriority)
	}

	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return strings.Contains(item.Match, fmt.Sprintf("%s ", nodeName)) && intPriorities.Has(item.Priority)
	}
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, types.OVNClusterRouter, p)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Errorf("Error deleting policies with priorities %v associated with the node %s: %v", priorities, nodeName, err)
	}
}

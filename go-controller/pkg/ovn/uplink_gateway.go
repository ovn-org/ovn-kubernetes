// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// syncUplinkDefaultRoutes preserves all discovered next hops as ECMP routes
// and removes withdrawn next hops in the same transaction. Imported routes
// have separate ownership and must survive a gateway refresh.
func (gw *GatewayManager) syncUplinkDefaultRoutes(nextHops []net.IP, externalRouterPort string) error {
	desired := sets.New[string]()
	v4, v6 := gw.netInfo.IPMode()
	for _, nextHop := range nextHops {
		if utilnet.IsIPv6(nextHop) && v6 || !utilnet.IsIPv6(nextHop) && v4 {
			desired.Insert(nextHop.String())
		}
	}
	defaultPrefix := func(nextHop string) string {
		if utilnet.IsIPv6String(nextHop) {
			return "::/0"
		}
		return "0.0.0.0/0"
	}
	owned := func(route *nbdb.LogicalRouterStaticRoute) bool {
		return route.ExternalIDs[types.NetworkExternalID] == gw.netInfo.GetNetworkName() &&
			route.ExternalIDs[string(libovsdbops.OwnerControllerKey)] == "" &&
			route.OutputPort != nil && *route.OutputPort == externalRouterPort &&
			route.RouteTable == "" && libovsdbops.PolicyEqualPredicate(route.Policy, nil) &&
			(route.IPPrefix == "0.0.0.0/0" || route.IPPrefix == "::/0")
	}
	// One scan of the router's routes; the advertised gateway router also
	// holds the imported routes, so the scan is not cheap.
	existing, err := libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(gw.nbClient,
		&nbdb.LogicalRouter{Name: gw.gwRouterName}, owned)
	if err != nil {
		return fmt.Errorf("failed to list Uplink default routes of GR %s: %w", gw.gwRouterName, err)
	}
	present := sets.New[string]()
	var stale []*nbdb.LogicalRouterStaticRoute
	for _, route := range existing {
		if desired.Has(route.Nexthop) && route.IPPrefix == defaultPrefix(route.Nexthop) && !present.Has(route.Nexthop) {
			present.Insert(route.Nexthop)
			continue
		}
		stale = append(stale, route)
	}
	missing := desired.Difference(present)
	if len(stale) == 0 && missing.Len() == 0 {
		return nil
	}
	var ops []ovsdb.Operation
	if len(stale) > 0 {
		ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesOps(gw.nbClient, ops, gw.gwRouterName, stale...)
		if err != nil {
			return fmt.Errorf("failed to remove stale Uplink default routes from GR %s: %w", gw.gwRouterName, err)
		}
	}
	for _, nextHop := range sets.List(missing) {
		route := &nbdb.LogicalRouterStaticRoute{
			IPPrefix:   defaultPrefix(nextHop),
			Nexthop:    nextHop,
			OutputPort: &externalRouterPort,
			ExternalIDs: map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			},
		}
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicateOps(gw.nbClient, ops, gw.gwRouterName, route, nil)
		if err != nil {
			return fmt.Errorf("failed to build Uplink default route via %s on GR %s: %w", nextHop, gw.gwRouterName, err)
		}
	}
	if _, err := libovsdbops.TransactAndCheck(gw.nbClient, ops); err != nil {
		return fmt.Errorf("failed to synchronize Uplink default routes on GR %s: %w", gw.gwRouterName, err)
	}
	return nil
}

func (oc *BaseNetworkController) uplinkGatewayConfig(node *corev1.Node) (*util.L3GatewayConfig, bool, error) {
	uplinkName := oc.Uplink()
	if uplinkName == "" {
		return nil, false, nil
	}

	stateName := uplinkutil.StateName(uplinkName, node.Name)
	state, err := uplinkutil.GetState(oc.watchFactory.UplinkStateInformer().Lister(), uplinkName, node.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true, fmt.Errorf("waiting for UplinkState %s for uplink %q on node %q",
				stateName, uplinkName, node.Name)
		}
		return nil, true, fmt.Errorf("failed to get UplinkState %s: %w", stateName, err)
	}
	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		return nil, true, fmt.Errorf("failed to get chassis ID for node %q: %w", node.Name, err)
	}
	if err := uplinkutil.ValidateOVSBridgeState(state, uplinkName, node.Name, true); err != nil {
		return nil, true, err
	}
	l3GatewayConfig, err := resolvedUplinkL3GatewayConfig(state, node.Name, chassisID)
	return l3GatewayConfig, true, err
}

func resolvedUplinkL3GatewayConfig(
	state *uplinkv1alpha1.UplinkState,
	nodeName, chassisID string,
) (*util.L3GatewayConfig, error) {
	macAddress, err := net.ParseMAC(string(state.Status.MACAddress))
	if err != nil {
		return nil, fmt.Errorf("failed to parse UplinkState MAC address %q: %w", state.Status.MACAddress, err)
	}

	ipAddresses := make([]*net.IPNet, 0, len(state.Status.IPAddresses))
	for _, ipAddress := range state.Status.IPAddresses {
		ip, cidr, err := net.ParseCIDR(string(ipAddress))
		if err != nil {
			return nil, fmt.Errorf("failed to parse UplinkState IP address %q: %w", ipAddress, err)
		}
		cidr.IP = ip
		ipAddresses = append(ipAddresses, cidr)
	}
	if len(ipAddresses) == 0 {
		return nil, fmt.Errorf("UplinkState has no gateway IP addresses")
	}

	defaultGateways := make([]net.IP, 0, len(state.Status.DefaultGateways))
	for _, defaultGateway := range state.Status.DefaultGateways {
		ip := net.ParseIP(string(defaultGateway))
		if ip == nil {
			return nil, fmt.Errorf("failed to parse UplinkState default gateway %q", defaultGateway)
		}
		defaultGateways = append(defaultGateways, ip)
	}

	bridgeName := state.Status.OVSBridge.Name
	return &util.L3GatewayConfig{
		Mode:           config.Gateway.Mode,
		ChassisID:      chassisID,
		BridgeID:       bridgeName,
		InterfaceID:    bridgeName + "_" + nodeName,
		MACAddress:     macAddress,
		IPAddresses:    ipAddresses,
		NextHops:       defaultGateways,
		NodePortEnable: config.Gateway.NodeportEnable,
	}, nil
}

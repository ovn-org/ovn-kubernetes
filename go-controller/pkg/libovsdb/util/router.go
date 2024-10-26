package util

import (
	"fmt"
	"net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilnet "k8s.io/utils/net"
)

// CreateDefaultRouteToExternal is called only when IC=true. This function adds a "catch-all" kind of LRSR to ovn-cluster-router
// 100.64.0.2               100.88.0.2 dst-ip
// 100.64.0.3               100.88.0.3 dst-ip
// 100.64.0.4               100.64.0.4 dst-ip
// 10.244.0.0/24            100.88.0.2 dst-ip
// 10.244.1.0/24            100.88.0.3 dst-ip
// 10.244.2.0/24            100.64.0.4 src-ip
// 10.244.0.0/16            100.64.0.4 src-ip ----> This is the reroute added to send all traffic that did not match earlier LRSR's to outside the cluster
// This logic works under the assumption that we have all other paths covered via routes that exist with higher precedence prefix match
// On first look it may seem like we are sending out traffic that doesn't "fit/match" other routes which is true, but the intent is that
// if we don't know where to send the traffic within the cluster, then we make it leave the cluster (we have a flow on br-ex that protects us and
// drops it if its not supposed to be going outside). This is needed when IC=true to ensure traffic from the other node arriving at this remote node
// does not get dropped. This removes the need for per-pod LRSR for primaryEIP and secondaryEIP && ESVC add a per-pod LRP on each egressNode to
// override this LRSR and send it to it's management port. NOTE: Handle changes around this logic with care. This is being added intentionally.
// (TODO: FIXME): With this route, we are officially breaking support for IC with zones that have multiple-nodes
// NOTE: This route is exactly the same as what is added by pod-live-migration feature and we keep the route exactly
// same across the 3 features so that if the route already exists on the node, this is just a no-op
func CreateDefaultRouteToExternal(nbClient libovsdbclient.Client, clusterRouter, gwRouterName string, clusterSubnets []*net.IPNet) error {
	gatewayIPs, err := GetLRPAddrs(nbClient, types.GWRouterToJoinSwitchPrefix+gwRouterName)
	if err != nil {
		return fmt.Errorf("attempt at finding node gateway router %s network information failed, err: %w", gwRouterName, err)
	}
	for _, subnet := range clusterSubnets {
		gatewayIP, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6String(subnet.IP.String()), gatewayIPs)
		if err != nil {
			return fmt.Errorf("could not find gateway IP for gateway router %s with family %v: %v", gwRouterName, false, err)
		}
		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix: subnet.String(),
			Nexthop:  gatewayIP.IP.String(),
			Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		}
		p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
			_, itemCIDR, err := net.ParseCIDR(lrsr.IPPrefix)
			if err != nil {
				return false
			}
			return util.ContainsCIDR(subnet, itemCIDR) &&
				lrsr.Nexthop == gatewayIP.IP.String() &&
				lrsr.Policy != nil && *lrsr.Policy == nbdb.LogicalRouterStaticRoutePolicySrcIP
		}
		if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(nbClient, clusterRouter, &lrsr, p); err != nil {
			return fmt.Errorf("unable to create pod to external catch-all reroute for gateway router %s, err: %v", gwRouterName, err)
		}
	}
	return nil
}

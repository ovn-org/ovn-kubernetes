package apbroute

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedroutelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute/gateway_info"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type networkClient interface {
	deleteGatewayIPs(podNsName ktypes.NamespacedName, toBeDeletedGWIPs, toBeKept sets.Set[string]) error
	addGatewayIPs(pod *v1.Pod, egress *gateway_info.GatewayInfoList) (bool, error)
}

type northBoundClient struct {
	routeLister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister
	nodeLister  corev1listers.NodeLister
	podLister   corev1listers.PodLister
	// NorthBound client interface
	nbClient libovsdbclient.Client

	// An address set factory that creates address sets
	addressSetFactory        addressset.AddressSetFactory
	externalGatewayRouteInfo *ExternalGatewayRouteInfoCache

	controllerName string

	zone string
}

type conntrackClient struct {
	podLister corev1listers.PodLister
}

func (nb *northBoundClient) findLogicalRouterPortWithPredicate(p func(item *nbdb.LogicalRouterPort) bool) ([]*nbdb.LogicalRouterPort, error) {
	return libovsdbops.FindLogicalRouterPortWithPredicate(nb.nbClient, p)
}

func (nb *northBoundClient) findLogicalRouterPoliciesWithPredicate(p func(item *nbdb.LogicalRouterPolicy) bool) ([]*nbdb.LogicalRouterPolicy, error) {
	return libovsdbops.FindLogicalRouterPoliciesWithPredicate(nb.nbClient, p)
}

func (nb *northBoundClient) findLogicalRouterStaticRoutesWithPredicate(p func(item *nbdb.LogicalRouterStaticRoute) bool) ([]*nbdb.LogicalRouterStaticRoute, error) {
	return libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(nb.nbClient, p)
}
func (nb *northBoundClient) deleteLogicalRouterStaticRoutes(routerName string, lrsrs ...*nbdb.LogicalRouterStaticRoute) error {
	return libovsdbops.DeleteLogicalRouterStaticRoutes(nb.nbClient, routerName, lrsrs...)
}

func (nb *northBoundClient) findLogicalRoutersWithPredicate(p func(item *nbdb.LogicalRouter) bool) ([]*nbdb.LogicalRouter, error) {
	return libovsdbops.FindLogicalRoutersWithPredicate(nb.nbClient, p)
}

// When IC is enabled, isNodeInLocalZone returns whether the provided node is in a zone local to the zone controller
func (nb *northBoundClient) isPodInLocalZone(pod *v1.Pod) (bool, error) {
	node, err := nb.nodeLister.Get(pod.Spec.NodeName)
	if err != nil {
		return false, err
	}
	return util.GetNodeZone(node) == nb.zone, nil
}

// delAllHybridRoutePolicies deletes all the 501 hybrid-route-policies that
// force pod egress traffic to be rerouted to a gateway router for local gateway mode.
// Called when migrating to SGW from LGW.
func (nb *northBoundClient) delAllHybridRoutePolicies() error {
	// nuke all the policies
	policyPred := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == types.HybridOverlayReroutePriority
	}
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nb.nbClient, types.OVNClusterRouter, policyPred)
	if err != nil {
		return fmt.Errorf("error deleting hybrid route policies on %s: %v", types.OVNClusterRouter, err)
	}

	// nuke all the address-sets.
	// if we fail to remove LRP's above, we don't attempt to remove ASes due to dependency constraints.
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetHybridNodeRoute, nb.controllerName, nil)
	asPred := libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil)
	err = libovsdbops.DeleteAddressSetsWithPredicate(nb.nbClient, asPred)
	if err != nil {
		return fmt.Errorf("failed to remove hybrid route address sets: %v", err)
	}

	return nil
}

// delAllLegacyHybridRoutePolicies deletes all the 501 hybrid-route-policies that
// force pod egress traffic to be rerouted to a gateway router for local gateway mode.
// New hybrid route matches on address set, while legacy matches just on pod IP
func (nb *northBoundClient) delAllLegacyHybridRoutePolicies() error {
	// nuke all the policies
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		if item.Priority != types.HybridOverlayReroutePriority {
			return false
		}
		if isNewVer, err := regexp.MatchString(`src\s*==\s*\$`, item.Match); err == nil && isNewVer {
			return false
		}
		return true
	}
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nb.nbClient, types.OVNClusterRouter, p)
	if err != nil {
		return fmt.Errorf("error deleting legacy hybrid route policies on %s: %v", types.OVNClusterRouter, err)
	}
	return nil
}

// deleteGatewayIPs handles deleting static routes for pods on a specific GR.
// If a set of gateways is given, only routes for that gateway are deleted. If no gateways
// are given, all routes for the namespace are deleted.
func (nb *northBoundClient) deleteGatewayIPs(podNsName ktypes.NamespacedName, toBeDeletedGWIPs, _ sets.Set[string]) error {
	return nb.externalGatewayRouteInfo.Cleanup(podNsName, func(routeInfo *RouteInfo) error {
		pod, err := nb.podLister.Pods(routeInfo.PodName.Namespace).Get(routeInfo.PodName.Name)
		var deletedPod bool
		if err != nil && apierrors.IsNotFound(err) {
			// Mark this routeInfo as deleted
			deletedPod = true
		}
		if err == nil {
			local, err := nb.isPodInLocalZone(pod)
			if err != nil {
				return err
			}
			if !local {
				klog.V(4).Infof("APB will not delete exgw routes for pod %s not in the local zone %s", routeInfo.PodName, nb.zone)
				return nil
			}
		}
		for podIP, routes := range routeInfo.PodExternalRoutes {
			for gw, gr := range routes {
				if toBeDeletedGWIPs.Has(gw) || deletedPod {
					// we cannot delete an external gateway IP from the north bound if it's also being provided by an external gateway annotation or if it is also
					// defined by a coexisting policy in the same namespace
					if err := nb.deletePodGWRoute(routeInfo, podIP, gw, gr); err != nil {
						return fmt.Errorf("APB delete pod GW route failed: %w", err)
					}
					delete(routes, gw)
				}
			}
		}
		return nil
	})
}

// addGatewayIPs adds the Gateway IP to the pod's next hops. It returns two values: a boolean and an error
// In the case of the boolean, it returns false in any of the following conditions:
// * If the pod wants to use host network
// * If the pod's phase is either Completed or Failed
// * If the pod's `PodIPs` status field is empty
// This value is used to signal the caller that the gateway IPs were not applied to the pod for reasons that are not errors.
// The error value is populated when an error occurs as usual.
func (nb *northBoundClient) addGatewayIPs(pod *v1.Pod, egress *gateway_info.GatewayInfoList) (bool, error) {
	if util.PodCompleted(pod) || util.PodWantsHostNetwork(pod) {
		return false, nil
	}
	klog.V(5).Infof("Processing %s/%s with status %s and IPs %+v", pod.Namespace, pod.Name, pod.Status.Phase, pod.Status.PodIPs)
	podIPs := make([]*net.IPNet, 0)
	for _, podIP := range pod.Status.PodIPs {
		ip := utilnet.ParseIPSloppy(podIP.IP)
		ipNet := &net.IPNet{
			IP:   ip,
			Mask: util.GetIPFullMask(ip),
		}
		ipNet = util.IPsToNetworkIPs(ipNet)[0]
		podIPs = append(podIPs, ipNet)
	}
	if len(podIPs) == 0 {
		// At this stage the pod is either in Pending or Running phase, but Pending should not have an IP, therefore it should not
		// be processed yet. Return false without error to prevent the pod from being perceived as correctly configured with the gateway IP.
		klog.Warningf("Will not add gateway routes pod %s/%s. IPs not found!", pod.Namespace, pod.Name)
		return false, nil
	}
	if config.Gateway.DisableSNATMultipleGWs {
		// delete all perPodSNATs (if this pod was controlled by egressIP controller, it will stop working since
		// a pod cannot be used for multiple-external-gateways and egressIPs at the same time)
		if err := nb.deletePodSNAT(pod.Spec.NodeName, []*net.IPNet{}, podIPs); err != nil {
			klog.Error(err.Error())
		}
	}
	podNsName := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	return true, nb.addGWRoutesForPod(egress.Elems(), podIPs, podNsName, pod.Spec.NodeName)
}

// deletePodSNAT removes per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// if allSNATs flag is set, then all the SNATs (including against egressIPs if any) for that pod will be deleted
// used when disableSNATMultipleGWs=true
func (nb *northBoundClient) deletePodSNAT(nodeName string, extIPs, podIPNets []*net.IPNet) error {
	node, err := nb.nodeLister.Get(nodeName)
	if err != nil {
		return err
	}
	if util.GetNodeZone(node) != nb.zone {
		klog.V(4).Infof("Node %s is not in the local zone %s", nodeName, nb.zone)
		return nil
	}
	nats, err := buildPodSNAT(extIPs, podIPNets)
	if err != nil {
		return err
	}
	logicalRouter := nbdb.LogicalRouter{
		Name: types.GWRouterPrefix + nodeName,
	}
	err = libovsdbops.DeleteNATs(nb.nbClient, &logicalRouter, nats...)
	if err != nil {
		return fmt.Errorf("failed to delete SNAT rule for pod on gateway router %s: %v", logicalRouter.Name, err)
	}
	return nil
}

// addEgressGwRoutesForPod handles adding all routes to gateways for a pod on a specific GR
func (nb *northBoundClient) addGWRoutesForPod(gateways []*gateway_info.GatewayInfo, podIfAddrs []*net.IPNet, podNsName ktypes.NamespacedName, node string) error {
	pod, err := nb.podLister.Pods(podNsName.Namespace).Get(podNsName.Name)
	if err != nil {
		return err
	}
	local, err := nb.isPodInLocalZone(pod)
	if err != nil {
		return err
	}
	if !local {
		klog.V(4).Infof("APB will not add exgw routes for pod %s not in the local zone %s", podNsName, nb.zone)
		return nil
	}

	gr := util.GetGatewayRouterFromNode(node)

	routesAdded := 0
	portPrefix, err := nb.extSwitchPrefix(node)
	if err != nil {
		klog.Warningf("Failed to find ext switch prefix for %s %v", node, err)
		return err
	}

	port := portPrefix + types.GWRouterToExtSwitchPrefix + gr
	return nb.externalGatewayRouteInfo.CreateOrLoad(podNsName, func(routeInfo *RouteInfo) error {
		for _, podIPNet := range podIfAddrs {
			for _, gateway := range gateways {
				// TODO (trozet): use the go bindings here and batch commands
				// validate the ip and gateway belong to the same address family
				gws, err := util.MatchAllIPStringFamily(utilnet.IsIPv6(podIPNet.IP), gateway.Gateways.UnsortedList())
				if err != nil {
					klog.Warningf("Address families for the pod address %s and gateway %s did not match", podIPNet.IP.String(), gateway.Gateways)
					continue
				}
				podIP := podIPNet.IP.String()
				for _, gw := range gws {
					// if route was already programmed, skip it
					if foundGR, ok := routeInfo.PodExternalRoutes[podIP][gw]; ok && foundGR == gr {
						routesAdded++
						continue
					}
					mask := util.GetIPFullMaskString(podIP)
					if err := nb.createOrUpdateBFDStaticRoute(gateway.BFDEnabled, gw, podIP, gr, port, mask); err != nil {
						return err
					}
					if routeInfo.PodExternalRoutes[podIP] == nil {
						routeInfo.PodExternalRoutes[podIP] = make(map[string]string)
					}
					routeInfo.PodExternalRoutes[podIP][gw] = gr
					routesAdded++
					if len(routeInfo.PodExternalRoutes[podIP]) == 1 {
						if err := nb.addHybridRoutePolicyForPod(podIPNet.IP, node); err != nil {
							return err
						}
					}
				}
			}
		}
		// if no routes are added return an error
		if routesAdded < 1 {
			return fmt.Errorf("gateway specified for namespace %s with gateway addresses %v but no valid routes exist for pod: %s",
				podNsName.Namespace, podIfAddrs, podNsName.Name)
		}
		return nil
	})
}

// AddHybridRoutePolicyForPod handles adding a higher priority allow policy to allow traffic to be routed normally
// by ecmp routes
func (nb *northBoundClient) addHybridRoutePolicyForPod(podIP net.IP, node string) error {
	if config.Gateway.Mode == config.GatewayModeLocal {
		// Add podIP to the node's address_set.
		asIndex := GetHybridRouteAddrSetDbIDs(node, nb.controllerName)
		as, err := nb.addressSetFactory.EnsureAddressSet(asIndex)
		if err != nil {
			return fmt.Errorf("cannot ensure that addressSet for node %s exists %v", node, err)
		}
		err = as.AddAddresses([]string{podIP.String()})
		if err != nil {
			return fmt.Errorf("unable to add PodIP %s: to the address set %s, err: %v", podIP.String(), node, err)
		}

		// add allow policy to bypass lr-policy in GR
		ipv4HashedAS, ipv6HashedAS := as.GetASHashNames()
		var l3Prefix string
		var matchSrcAS string
		isIPv6 := utilnet.IsIPv6(podIP)
		if isIPv6 {
			l3Prefix = "ip6"
			matchSrcAS = ipv6HashedAS
		} else {
			l3Prefix = "ip4"
			matchSrcAS = ipv4HashedAS
		}

		// get the GR to join switch ip address
		grJoinIfAddrs, err := libovsdbutil.GetLRPAddrs(nb.nbClient, types.GWRouterToJoinSwitchPrefix+types.GWRouterPrefix+node)
		if err != nil {
			return fmt.Errorf("unable to find IP address for node: %s, %s port, err: %v", node, types.GWRouterToJoinSwitchPrefix, err)
		}
		grJoinIfAddr, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6(podIP), grJoinIfAddrs)
		if err != nil {
			return fmt.Errorf("failed to match gateway router join interface IPs: %v, err: %v", grJoinIfAddr, err)
		}

		var matchDst string
		var clusterL3Prefix string
		for _, clusterSubnet := range config.Default.ClusterSubnets {
			if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
				clusterL3Prefix = "ip6"
			} else {
				clusterL3Prefix = "ip4"
			}
			if l3Prefix != clusterL3Prefix {
				continue
			}
			matchDst += fmt.Sprintf(" && %s.dst != %s", clusterL3Prefix, clusterSubnet.CIDR)
		}

		// traffic destined outside of cluster subnet go to GR
		matchStr := fmt.Sprintf(`inport == "%s%s" && %s.src == $%s`, types.RouterToSwitchPrefix, node, l3Prefix, matchSrcAS)
		matchStr += matchDst

		logicalRouterPolicy := nbdb.LogicalRouterPolicy{
			Priority: types.HybridOverlayReroutePriority,
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			Nexthops: []string{grJoinIfAddr.IP.String()},
			Match:    matchStr,
		}
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Priority == logicalRouterPolicy.Priority && strings.Contains(item.Match, matchSrcAS)
		}
		err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(nb.nbClient, types.OVNClusterRouter,
			&logicalRouterPolicy, p, &logicalRouterPolicy.Nexthops, &logicalRouterPolicy.Match, &logicalRouterPolicy.Action)
		if err != nil {
			return fmt.Errorf("failed to add policy route %+v to %s: %v", logicalRouterPolicy, types.OVNClusterRouter, err)
		}
	}
	return nil
}

func (nb *northBoundClient) createOrUpdateBFDStaticRoute(bfdEnabled bool, gw string, podIP, gr, port, mask string) error {
	lrsr := nbdb.LogicalRouterStaticRoute{
		Policy: &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		Options: map[string]string{
			"ecmp_symmetric_reply": "true",
		},
		Nexthop:    gw,
		IPPrefix:   podIP + mask,
		OutputPort: &port,
	}

	ops := []ovsdb.Operation{}
	var err error
	if bfdEnabled {
		bfd := nbdb.BFD{
			DstIP:       gw,
			LogicalPort: port,
		}
		ops, err = libovsdbops.CreateOrUpdateBFDOps(nb.nbClient, ops, &bfd)
		if err != nil {
			return fmt.Errorf("error creating or updating BFD %+v: %v", bfd, err)
		}
		lrsr.BFD = &bfd.UUID
	}

	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.IPPrefix == lrsr.IPPrefix &&
			item.Nexthop == lrsr.Nexthop &&
			item.OutputPort != nil &&
			*item.OutputPort == *lrsr.OutputPort &&
			item.Policy == lrsr.Policy
	}
	ops, err = libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicateOps(nb.nbClient, ops, gr, &lrsr, p,
		&lrsr.Options)
	if err != nil {
		return fmt.Errorf("error creating or updating static route %+v on router %s: %v", lrsr, gr, err)
	}

	_, err = libovsdbops.TransactAndCheck(nb.nbClient, ops)
	if err != nil {
		return fmt.Errorf("error transacting static route: %v", err)
	}

	return nil
}

func (nb *northBoundClient) updateExternalGWInfoCacheForPodIPWithGatewayIP(podIP, gwIP, nodeName string, bfdEnabled bool, namespacedName ktypes.NamespacedName) error {
	gr := util.GetGatewayRouterFromNode(nodeName)

	return nb.externalGatewayRouteInfo.CreateOrLoad(namespacedName, func(routeInfo *RouteInfo) error {
		// if route was already programmed, skip it
		if foundGR, ok := routeInfo.PodExternalRoutes[podIP][gwIP]; ok && foundGR == gr {
			return nil
		}
		mask := util.GetIPFullMaskString(podIP)

		portPrefix, err := nb.extSwitchPrefix(nodeName)
		if err != nil {
			klog.Warningf("Failed to find ext switch prefix for %s %v", nodeName, err)
			return err
		}
		if bfdEnabled {
			port := portPrefix + types.GWRouterToExtSwitchPrefix + gr
			// update the BFD static route just in case it has changed
			if err := nb.createOrUpdateBFDStaticRoute(bfdEnabled, gwIP, podIP, gr, port, mask); err != nil {
				return err
			}
		} else {
			_, err := nb.lookupBFDEntry(gwIP, gr, portPrefix)
			if err != nil {
				err = nb.cleanUpBFDEntry(gwIP, gr, portPrefix)
				if err != nil {
					return err
				}
			}
		}

		if routeInfo.PodExternalRoutes[podIP] == nil {
			routeInfo.PodExternalRoutes[podIP] = make(map[string]string)
		}
		routeInfo.PodExternalRoutes[podIP][gwIP] = gr

		return nil
	})
}

func (nb *northBoundClient) deletePodGWRoute(routeInfo *RouteInfo, podIP, gw, gr string) error {
	if utilnet.IsIPv6String(gw) != utilnet.IsIPv6String(podIP) {
		return nil
	}

	mask := util.GetIPFullMaskString(podIP)
	if err := nb.deleteLogicalRouterStaticRoute(podIP, mask, gw, gr); err != nil {
		return fmt.Errorf("unable to delete pod %s ECMP route to GR %s, GW: %s: %w",
			routeInfo.PodName, gr, gw, err)
	}

	node := util.GetWorkerFromGatewayRouter(gr)

	// The gw is deleted from the routes cache after this func is called, length 1
	// means it is the last gw for the pod and the hybrid route policy should be deleted.
	if entry := routeInfo.PodExternalRoutes[podIP]; len(entry) <= 1 {
		if err := nb.delHybridRoutePolicyForPod(net.ParseIP(podIP), node); err != nil {
			return fmt.Errorf("unable to delete hybrid route policy for pod %s: err: %v", routeInfo.PodName, err)
		}
	}

	portPrefix, err := nb.extSwitchPrefix(node)
	if err != nil {
		return err
	}
	return nb.cleanUpBFDEntry(gw, gr, portPrefix)
}

// cleanUpBFDEntry checks if the BFD table entry related to the associated
// gw router / port / gateway ip is referenced by other routing rules, and if
// not removes the entry to avoid having dangling BFD entries.
func (nb *northBoundClient) cleanUpBFDEntry(gatewayIP, gatewayRouter, prefix string) error {
	portName := prefix + types.GWRouterToExtSwitchPrefix + gatewayRouter
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		if item.OutputPort != nil && *item.OutputPort == portName && item.Nexthop == gatewayIP && item.BFD != nil && *item.BFD != "" {
			return true
		}
		return false
	}
	logicalRouterStaticRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(nb.nbClient, p)
	if err != nil {
		return fmt.Errorf("cleanUpBFDEntry failed to list routes for %s: %w", portName, err)
	}
	if len(logicalRouterStaticRoutes) > 0 {
		return nil
	}

	bfd := nbdb.BFD{
		LogicalPort: portName,
		DstIP:       gatewayIP,
	}
	err = libovsdbops.DeleteBFDs(nb.nbClient, &bfd)
	if err != nil {
		return fmt.Errorf("error deleting BFD %+v: %v", bfd, err)
	}

	return nil
}

func (nb *northBoundClient) deleteLogicalRouterStaticRoute(podIP, mask, gw, gr string) error {
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.Policy != nil &&
			*item.Policy == nbdb.LogicalRouterStaticRoutePolicySrcIP &&
			item.IPPrefix == podIP+mask &&
			item.Nexthop == gw
	}
	err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(nb.nbClient, gr, p)
	if err != nil {
		return fmt.Errorf("error deleting static route from router %s: %v", gr, err)
	}

	return nil
}

// DelHybridRoutePolicyForPod handles deleting a logical route policy that
// forces pod egress traffic to be rerouted to a gateway router for local gateway mode.
func (nb *northBoundClient) delHybridRoutePolicyForPod(podIP net.IP, node string) error {
	if config.Gateway.Mode != config.GatewayModeLocal {
		return nil
	}

	// Delete podIP from the node's address_set.
	asIndex := GetHybridRouteAddrSetDbIDs(node, nb.controllerName)
	as, err := nb.addressSetFactory.EnsureAddressSet(asIndex)
	if err != nil {
		return fmt.Errorf("cannot Ensure that addressSet for node %s exists %v", node, err)
	}
	err = as.DeleteAddresses([]string{podIP.String()})
	if err != nil {
		return fmt.Errorf("unable to remove PodIP %s: to the address set %s, err: %v", podIP.String(), node, err)
	}

	// delete hybrid policy to bypass lr-policy in GR, only if there are zero pods on this node.
	ipv4HashedAS, ipv6HashedAS := as.GetASHashNames()
	ipv4PodIPs, ipv6PodIPs := as.GetAddresses()
	deletePolicy := false
	var l3Prefix string
	var matchSrcAS string
	if utilnet.IsIPv6(podIP) {
		l3Prefix = "ip6"
		if len(ipv6PodIPs) == 0 {
			deletePolicy = true
		}
		matchSrcAS = ipv6HashedAS
	} else {
		l3Prefix = "ip4"
		if len(ipv4PodIPs) == 0 {
			deletePolicy = true
		}
		matchSrcAS = ipv4HashedAS
	}
	if deletePolicy {
		var matchDst string
		var clusterL3Prefix string
		for _, clusterSubnet := range config.Default.ClusterSubnets {
			if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
				clusterL3Prefix = "ip6"
			} else {
				clusterL3Prefix = "ip4"
			}
			if l3Prefix != clusterL3Prefix {
				continue
			}
			matchDst += fmt.Sprintf(" && %s.dst != %s", l3Prefix, clusterSubnet.CIDR)
		}
		matchStr := fmt.Sprintf(`inport == "%s%s" && %s.src == $%s`, types.RouterToSwitchPrefix, node, l3Prefix, matchSrcAS)
		matchStr += matchDst

		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Priority == types.HybridOverlayReroutePriority && item.Match == matchStr
		}
		err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nb.nbClient, types.OVNClusterRouter, p)
		if err != nil {
			return fmt.Errorf("error deleting policy %s on router %s: %v", matchStr, types.OVNClusterRouter, err)
		}
	}
	if len(ipv4PodIPs) == 0 && len(ipv6PodIPs) == 0 {
		// delete address set.
		err := as.Destroy()
		if err != nil {
			return fmt.Errorf("failed to remove address set: %s, on: %s, err: %v",
				as.GetName(), node, err)
		}
	}
	return nil

}

// extSwitchPrefix returns the prefix of the external switch to use for
// external gateway routes. In case no second bridge is configured, we
// use the default one and the prefix is empty.
func (nb *northBoundClient) extSwitchPrefix(nodeName string) (string, error) {
	node, err := nb.nodeLister.Get(nodeName)
	if err != nil {
		return "", errors.Wrapf(err, "extSwitchPrefix: failed to find node %s", nodeName)
	}
	l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return "", errors.Wrapf(err, "extSwitchPrefix: failed to parse l3 gateway annotation for node %s", nodeName)
	}

	if l3GatewayConfig.EgressGWInterfaceID != "" {
		return types.EgressGWSwitchPrefix, nil
	}
	return "", nil
}

func (nb *northBoundClient) lookupBFDEntry(gatewayIP, gatewayRouter, prefix string) (*nbdb.BFD, error) {
	portName := prefix + types.GWRouterToExtSwitchPrefix + gatewayRouter
	bfd := nbdb.BFD{
		LogicalPort: portName,
		DstIP:       gatewayIP,
	}
	found, err := libovsdbops.LookupBFD(nb.nbClient, &bfd)
	if err != nil {
		klog.Warningf("Failed to lookup BFD for gateway IP %s, gateway router %s and prefix %s", gatewayIP, gatewayRouter, prefix)
		return nil, err
	}

	return found, nil
}

// buildPodSNAT builds per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// if allSNATs flag is set, then all the SNATs (including against egressIPs if any) for that pod will be returned
func buildPodSNAT(extIPs, podIPNets []*net.IPNet) ([]*nbdb.NAT, error) {
	nats := make([]*nbdb.NAT, 0, len(extIPs)*len(podIPNets))
	var nat *nbdb.NAT

	for _, podIPNet := range podIPNets {
		fullMaskPodNet := util.IPsToNetworkIPs(podIPNet)[0]
		if len(extIPs) == 0 {
			nat = libovsdbops.BuildSNAT(nil, fullMaskPodNet, "", nil)
		} else {
			for _, gwIPNet := range extIPs {
				if utilnet.IsIPv6CIDR(gwIPNet) != utilnet.IsIPv6CIDR(podIPNet) {
					continue
				}
				nat = libovsdbops.BuildSNAT(&gwIPNet.IP, fullMaskPodNet, "", nil)
			}
		}
		nats = append(nats, nat)
	}
	return nats, nil
}

func GetHybridRouteAddrSetDbIDs(nodeName, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetHybridNodeRoute, controller,
		map[libovsdbops.ExternalIDKey]string{
			// there is only 1 address set of this type per node
			libovsdbops.ObjectNameKey: nodeName,
		})
}

func (c *conntrackClient) deleteGatewayIPs(podNsName ktypes.NamespacedName, _, toBeKept sets.Set[string]) error {
	// loop through all the IPs on the annotations; ARP for their MACs and form an allowlist
	return util.SyncConntrackForExternalGateways(toBeKept, nil, func() ([]*v1.Pod, error) {
		pod, err := c.podLister.Pods(podNsName.Namespace).Get(podNsName.Name)
		return []*v1.Pod{pod}, err
	})
}

// addGatewayIPs is a NOP (no operation) in the conntrack client as it does not add any entry to the conntrack table.
func (c *conntrackClient) addGatewayIPs(pod *v1.Pod, egress *gateway_info.GatewayInfoList) (bool, error) {
	return true, nil
}

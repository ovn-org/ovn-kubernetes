package apbroute

import (
	"fmt"
	"net"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute/gateway_info"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type managedGWIPs struct {
	namespacedName ktypes.NamespacedName
	nodeName       string
	gwList         *gateway_info.GatewayInfoList
}

func (c *ExternalGatewayMasterController) Repair() error {
	start := time.Now()
	defer func() {
		klog.V(4).Infof("Syncing exgw routes took %v", time.Since(start))
	}()

	// migration from LGW to SGW mode
	// for shared gateway mode, these LRPs shouldn't exist, so delete them all
	if config.Gateway.Mode == config.GatewayModeShared {
		if err := c.nbClient.delAllHybridRoutePolicies(); err != nil {
			klog.Errorf("Error while removing hybrid policies on moving to SGW mode, error: %v", err)
		}
	} else if config.Gateway.Mode == config.GatewayModeLocal {
		// remove all legacy hybrid route policies
		if err := c.nbClient.delAllLegacyHybridRoutePolicies(); err != nil {
			klog.Errorf("Error while removing legacy hybrid policies, error: %v", err)
		}
	}

	// Build cache of expected routes in the cluster
	// will be used for cleanup
	policyGWIPsMap, err := c.syncPoliciesWithoutCleanup()
	if err != nil {
		return fmt.Errorf("error while aggregating the external policy routes: %v", err)
	}

	// Get all ECMP routes in OVN and build cache
	ovnRouteCache, err := c.buildOVNECMPCache()
	if err != nil {
		return fmt.Errorf("failed to build ECMP cache: %w", err)
	}

	if len(ovnRouteCache) == 0 {
		// Even if no ECMP routes exist, we should ensure no 501 LRPs exist either
		if err := c.nbClient.delAllHybridRoutePolicies(); err != nil {
			if err != nil {
				return fmt.Errorf("error while removing hybrid policies: %w", err)
			}
		}
		// nothing in OVN, so no reason to search for stale routes
		return nil
	}

	annotatedGWIPsMap, err := c.buildExternalIPGatewaysFromAnnotations()
	if err != nil {
		return fmt.Errorf("cannot retrieve the annotated gateway IPs:%w", err)
	}

	// compare caches and see if OVN routes are stale
	for podIP, ovnRoutes := range ovnRouteCache {
		// pod IP does not exist in the cluster
		// remove route and any hybrid policy
		expectedNextHopsPolicy, okPolicy := policyGWIPsMap[podIP]
		expectedNextHopsAnnotation, okAnnotation := annotatedGWIPsMap[podIP]
		if !okPolicy && !okAnnotation {
			// No external gateways found for this Pod IP
			continue
		}

		for _, ovnRoute := range ovnRoutes {
			// if length of the output port is 0, this is a legacy route (we now always specify output interface)
			if len(ovnRoute.outport) == 0 {
				continue
			}

			node := util.GetWorkerFromGatewayRouter(ovnRoute.router)
			// prefix will signify secondary exgw bridge, or empty if normal setup
			// have to determine if a node changed while master was down and if the route swapped from
			// the default bridge to a new secondary bridge (or vice versa)
			prefix, err := c.nbClient.extSwitchPrefix(node)
			if err != nil {
				// we shouldn't continue in this case, because we cant be sure this is a route we want to remove
				return fmt.Errorf("cannot sync exgw route: %+v, unable to determine exgw switch prefix: %v",
					ovnRoute, err)
			} else if (prefix != "" && !strings.Contains(ovnRoute.outport, prefix)) ||
				(prefix == "" && strings.Contains(ovnRoute.outport, types.EgressGWSwitchPrefix)) {
				continue
			}
			if expectedNextHopsPolicy != nil {
				ovnRoute.shouldExist = c.processOVNRoute(ovnRoute, expectedNextHopsPolicy.gwList, podIP, expectedNextHopsPolicy, true)
				if ovnRoute.shouldExist {
					continue
				}
			}
			if expectedNextHopsAnnotation != nil {
				ovnRoute.shouldExist = c.processOVNRoute(ovnRoute, expectedNextHopsAnnotation.gwList, podIP, expectedNextHopsAnnotation, false)
			}
		}
	}

	klog.V(4).Infof("OVN ECMP route cache is: %+v", ovnRouteCache)
	klog.V(4).Infof("Cluster ECMP route cache is: %+v", policyGWIPsMap)

	// iterate through ovn routes and remove any stale entries
	for podIP, ovnRoutes := range ovnRouteCache {
		podHasAnyECMPRoutes := false
		for _, ovnRoute := range ovnRoutes {
			if !ovnRoute.shouldExist {
				klog.V(4).Infof("Found stale exgw ecmp route, podIP: %s, nexthop: %s, router: %s",
					podIP, ovnRoute.nextHop, ovnRoute.router)
				lrsr := nbdb.LogicalRouterStaticRoute{UUID: ovnRoute.uuid}
				err := c.nbClient.deleteLogicalRouterStaticRoutes(ovnRoute.router, &lrsr)
				if err != nil {
					return fmt.Errorf("error deleting static route %s from router %s: %v", ovnRoute.uuid, ovnRoute.router, err)
				}

				// check to see if we should also clean up bfd
				node := util.GetWorkerFromGatewayRouter(ovnRoute.router)
				// prefix will signify secondary exgw bridge, or empty if normal setup
				// have to determine if a node changed while master was down and if the route swapped from
				// the default bridge to a new secondary bridge (or vice versa)
				prefix, err := c.nbClient.extSwitchPrefix(node)
				if err != nil {
					// We shouldn't continue in this case, because we cant be sure this is a route we want to remove
					return fmt.Errorf("cannot sync exgw bfd: %+v, unable to determine exgw switch prefix: %v",
						ovnRoute, err)
				} else {
					if err := c.nbClient.cleanUpBFDEntry(ovnRoute.nextHop, ovnRoute.router, prefix); err != nil {
						return fmt.Errorf("cannot clean up BFD entry: %w", err)
					}
				}

			} else {
				podHasAnyECMPRoutes = true
			}
		}

		// if pod had no ECMP routes we need to make sure we remove logical route policy for local gw mode
		if !podHasAnyECMPRoutes {
			for _, ovnRoute := range ovnRoutes {
				node := strings.TrimPrefix(ovnRoute.router, types.GWRouterPrefix)
				if err := c.nbClient.delHybridRoutePolicyForPod(net.ParseIP(podIP), node); err != nil {
					return fmt.Errorf("error while removing hybrid policy for pod IP: %s, on node: %s, error: %v",
						podIP, node, err)
				}
			}
		}
	}

	// could be stale hybrid policies with stale addresses in the set that had no corresponding OVN ecmp routes
	// get all pods, attempt to delete their hybridRoutePolicy that have no policy
	if config.Gateway.Mode == config.GatewayModeLocal {
		ovnHybridCache, err := c.buildOVNHybridCache()
		if err != nil {
			return fmt.Errorf("failed to build hybrid cache: %w", err)
		}
		for ip, node := range ovnHybridCache {
			// check if this pod IP has a corresponding policy, if not, remove it
			_, okPolicy := policyGWIPsMap[ip]
			_, okAnnotation := annotatedGWIPsMap[ip]
			if !okPolicy && !okAnnotation {
				klog.Infof("CleanHybridPRoutes: Removing IP: %s from hybrid route policy", ip)
				if err := c.nbClient.delHybridRoutePolicyForPod(net.ParseIP(ip), node); err != nil {
					return fmt.Errorf("CleanHybridPRoutes: error while removing hybrid policy for pod IP: %s, on node: %s, error: %v",
						ip, node, err)
				}
			}
		}
	}
	return nil
}

// syncPoliciesWithoutCleanup handles all existing policies and initializes caches. It doesn't do any cleanup,
// but it returns managedGWIPs that should exist, and relies on the repair code to cleanup everything else.
// After cleanup is completed, regular handling can be started.
func (c *ExternalGatewayMasterController) syncPoliciesWithoutCleanup() (map[string]*managedGWIPs, error) {
	clusterRouteCache := make(map[string]*managedGWIPs)
	externalRoutePolicies, err := c.mgr.routeLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list AdminPolicyBasedExternalRoute: %w", err)
	}

	for _, policy := range externalRoutePolicies {
		if policy.Status.Status != adminpolicybasedrouteapi.SuccessStatus {
			// skip handling policies without status that were not completely handled, or policies with error status
			// because handling them will cause startup failure.
			// db objects for these policies will be cleaned up, and policies will be handled from scratch by the
			// general controller logic.
			klog.Infof("Skip initial sync for APBRoute policy %s", policy.Name)
			continue
		}
		_, err = c.mgr.syncRoutePolicy(policy.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to sync policy %s: %w", policy.Name, err)
		}
	}
	policyKeys := c.mgr.routePolicySyncCache.GetKeys()

	for _, policyName := range policyKeys {
		err = c.mgr.routePolicySyncCache.DoWithLock(policyName, func(key string) error {
			existingPolicy, found := c.mgr.routePolicySyncCache.Load(policyName)
			if !found {
				return fmt.Errorf("policy %s was deleted during repair", key)
			}
			for _, targetPods := range existingPolicy.targetNamespaces {
				for targetPodNamespacedName, targetPodInfo := range targetPods {
					targetPod, err := c.mgr.podLister.Pods(targetPodNamespacedName.Namespace).Get(targetPodNamespacedName.Name)
					if err != nil {
						return fmt.Errorf("failed getting target pod %s for policy %s: %w", targetPodNamespacedName, key, err)
					}
					for _, targetPodIP := range targetPod.Status.PodIPs {
						podIPStr := utilnet.ParseIPSloppy(targetPodIP.IP).String()
						clusterRouteCache[podIPStr] = &managedGWIPs{
							namespacedName: ktypes.NamespacedName{Namespace: targetPod.Namespace, Name: targetPod.Name},
							nodeName:       targetPod.Spec.NodeName,
							gwList:         gateway_info.NewGatewayInfoList()}

						allGWIPs := gateway_info.NewGatewayInfoList()
						allGWIPs.InsertOverwrite(targetPodInfo.StaticGateways.Elems()...)
						allGWIPs.InsertOverwrite(targetPodInfo.DynamicGateways.Elems()...)

						for _, gwInfo := range allGWIPs.Elems() {
							for gw := range gwInfo.Gateways {
								if utilnet.IsIPv6String(gw) != utilnet.IsIPv6String(podIPStr) {
									continue
								}
								clusterRouteCache[podIPStr].gwList.InsertOverwrite(gwInfo)
							}
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return clusterRouteCache, nil
}

func (c *ExternalGatewayMasterController) processOVNRoute(ovnRoute *ovnRoute, gwList *gateway_info.GatewayInfoList, podIP string,
	managedIPGWInfo *managedGWIPs, noDbChanges bool) bool {
	// podIP exists, check if route matches
	for _, gwInfo := range gwList.Elems() {
		for clusterNextHop := range gwInfo.Gateways {
			if ovnRoute.nextHop == clusterNextHop {
				// populate the externalGWInfo cache with this pair podIP->next Hop IP.
				if noDbChanges {
					return true
				}
				err := c.nbClient.updateExternalGWInfoCacheForPodIPWithGatewayIP(podIP, ovnRoute.nextHop, managedIPGWInfo.nodeName, gwInfo.BFDEnabled, managedIPGWInfo.namespacedName)
				if err == nil {
					return true
				}
				klog.Errorf("Failed to add cache routeInfo for %s, error: %v", managedIPGWInfo.namespacedName.Name, err)
			}
		}
	}
	return false
}

func (c *ExternalGatewayMasterController) buildExternalIPGatewaysFromAnnotations() (map[string]*managedGWIPs, error) {
	clusterRouteCache := make(map[string]*managedGWIPs, 0)

	nsList, err := c.mgr.namespaceLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, ns := range nsList {
		if nsGWIPs, ok := ns.Annotations[util.RoutingExternalGWsAnnotation]; ok && nsGWIPs != "" {
			var bfdEnabled bool
			ips := sets.Set[string]{}
			for _, ip := range strings.Split(nsGWIPs, ",") {
				podIPStr := utilnet.ParseIPSloppy(ip).String()
				ips.Insert(podIPStr)
			}

			if _, ok := ns.Annotations[util.BfdAnnotation]; ok {
				bfdEnabled = true
			}
			gwInfo := gateway_info.NewGatewayInfo(ips, bfdEnabled)
			nsPodList, err := c.mgr.podLister.Pods(ns.Name).List(labels.Everything())
			if err != nil {
				return nil, err
			}
			// set static gateway ips for all pods in the namespace
			populateManagedGWIPsCacheForPods(gwInfo, clusterRouteCache, nsPodList)
		}
	}

	podList, err := c.mgr.podLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, pod := range podList {
		networkName, ok := pod.Annotations[util.RoutingNetworkAnnotation]
		if !ok {
			continue
		}
		targetNamespaces, ok := pod.Annotations[util.RoutingNamespaceAnnotation]
		if !ok {
			continue
		}
		foundGws, err := getExGwPodIPs(pod, networkName)
		if err != nil {
			klog.Errorf("Error getting exgw IPs for pod: %s, error: %v", pod.Name, err)
			return nil, err
		}
		if foundGws.Len() == 0 {
			klog.Errorf("No pod IPs found for pod %s/%s", pod.Namespace, pod.Name)
			continue
		}
		var bfdEnabled bool
		if _, ok := pod.Annotations[util.BfdAnnotation]; ok {
			bfdEnabled = true
		}
		gwInfo := gateway_info.NewGatewayInfo(foundGws, bfdEnabled)
		for _, targetNs := range strings.Split(targetNamespaces, ",") {
			nsPodList, err := c.mgr.podLister.Pods(targetNs).List(labels.Everything())
			if err != nil {
				return nil, err
			}
			// set dynamic gateway ips for all pods in the targetNamespaces
			populateManagedGWIPsCacheForPods(gwInfo, clusterRouteCache, nsPodList)
		}
	}
	return clusterRouteCache, nil
}

func populateManagedGWIPsCacheForPods(gwInfo *gateway_info.GatewayInfo, cache map[string]*managedGWIPs, podList []*v1.Pod) {
	for gwIP := range gwInfo.Gateways {
		for _, pod := range podList {
			// ignore completed pods, host networked pods, pods not scheduled
			if util.PodWantsHostNetwork(pod) || util.PodCompleted(pod) || !util.PodScheduled(pod) {
				continue
			}
			for _, podIP := range pod.Status.PodIPs {
				podIPStr := utilnet.ParseIPSloppy(podIP.IP).String()
				if utilnet.IsIPv6String(gwIP) != utilnet.IsIPv6String(podIPStr) {
					continue
				}
				if _, ok := cache[podIPStr]; !ok {
					cache[podIPStr] = &managedGWIPs{
						namespacedName: ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
						nodeName:       pod.Spec.NodeName,
						gwList:         gateway_info.NewGatewayInfoList(),
					}
				}
				cache[podIPStr].gwList.InsertOverwrite(gateway_info.NewGatewayInfo(sets.New(gwIP), gwInfo.BFDEnabled))
			}
		}
	}
}

// Build cache of routes in OVN
// map[podIP][]ovnRoute
type ovnRoute struct {
	nextHop     string
	uuid        string
	router      string
	outport     string
	shouldExist bool
}

func (c *ExternalGatewayMasterController) buildOVNECMPCache() (map[string][]*ovnRoute, error) {
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.Options["ecmp_symmetric_reply"] == "true"
	}
	logicalRouterStaticRoutes, err := c.nbClient.findLogicalRouterStaticRoutesWithPredicate(p)
	if err != nil {
		return nil, fmt.Errorf("CleanECMPRoutes: failed to list ecmp routes: %v", err)
	}

	ovnRouteCache := make(map[string][]*ovnRoute)
	for _, logicalRouterStaticRoute := range logicalRouterStaticRoutes {
		p := func(item *nbdb.LogicalRouter) bool {
			return util.SliceHasStringItem(item.StaticRoutes, logicalRouterStaticRoute.UUID)
		}
		logicalRouters, err := c.nbClient.findLogicalRoutersWithPredicate(p)
		if err != nil {
			return nil, fmt.Errorf("CleanECMPRoutes: failed to find logical router for %s, err: %v", logicalRouterStaticRoute.UUID, err)
		}

		route := &ovnRoute{
			nextHop: logicalRouterStaticRoute.Nexthop,
			uuid:    logicalRouterStaticRoute.UUID,
			router:  logicalRouters[0].Name,
			outport: *logicalRouterStaticRoute.OutputPort,
		}
		podIP, _, _ := net.ParseCIDR(logicalRouterStaticRoute.IPPrefix)
		if _, ok := ovnRouteCache[podIP.String()]; !ok {
			ovnRouteCache[podIP.String()] = []*ovnRoute{route}
		} else {
			ovnRouteCache[podIP.String()] = append(ovnRouteCache[podIP.String()], route)
		}
	}
	return ovnRouteCache, nil
}

// returns a hybrid cache of map[podIPs]nodeName
func (c *ExternalGatewayMasterController) buildOVNHybridCache() (map[string]string, error) {
	ovnHybridCache := make(map[string]string)
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == types.HybridOverlayReroutePriority
	}

	logicalRouterPolicies, err := c.nbClient.findLogicalRouterPoliciesWithPredicate(p)
	if err != nil {
		return nil, fmt.Errorf("CleanHybridPRoutes: failed to list hybrid routes: %v", err)
	}

	foundNextHops := sets.Set[string]{}
	for _, lrp := range logicalRouterPolicies {
		foundNextHops.Insert(lrp.Nexthops...)
	}

	r := func(item *nbdb.LogicalRouterPort) bool {
		for _, ip := range item.Networks {
			// grab only IP prefix and not mask
			p := strings.Split(ip, "/")
			if foundNextHops.Has(p[0]) {
				return true
			}
		}
		return false
	}

	grPorts, err := c.nbClient.findLogicalRouterPortWithPredicate(r)
	if err != nil {
		return nil, fmt.Errorf("CleanHybridPRoutes: failed to search for logical router port: %v", err)
	}

	for _, grPort := range grPorts {
		nodeName := strings.TrimPrefix(grPort.Name, types.GWRouterToJoinSwitchPrefix+types.GWRouterPrefix)
		if len(nodeName) == 0 && nodeName != grPort.Name {
			continue
		}

		// nodeName has been found
		// get address set and list all addresses
		asIndex := GetHybridRouteAddrSetDbIDs(nodeName, c.nbClient.controllerName)
		as, err := c.nbClient.addressSetFactory.GetAddressSet(asIndex)
		if err != nil {
			klog.Errorf("CleanHybridPRoutes: unable to find get address set %s: %v", asIndex, err)
			continue
		}
		ipv4Addrs, ipv6Addrs := as.GetAddresses()
		for _, ip := range ipv4Addrs {
			ovnHybridCache[ip] = nodeName
		}
		for _, ip := range ipv6Addrs {
			ovnHybridCache[ip] = nodeName
		}
	}

	klog.Infof("CleanHybridRoutes: OVN cache built: %#v", ovnHybridCache)
	return ovnHybridCache, nil
}

package apbroute

import (
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
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type managedGWIPs struct {
	namespacedName ktypes.NamespacedName
	nodeName       string
	gwList         gatewayInfoList
}

func (c *ExternalGatewayMasterController) repair() {
	start := time.Now()
	defer func() {
		klog.Infof("Syncing exgw routes took %v", time.Since(start))
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

	// Get all ECMP routes in OVN and build cache
	ovnRouteCache := c.buildOVNECMPCache()

	if len(ovnRouteCache) == 0 {
		// Even if no ECMP routes exist, we should ensure no 501 LRPs exist either
		if err := c.nbClient.delAllHybridRoutePolicies(); err != nil {
			klog.Errorf("Error while removing hybrid policies, error: %v", err)
		}
		// nothing in OVN, so no reason to search for stale routes
		return
	}

	// Build cache of expected routes in the cluster
	// map[podIP]set[podNamespacedName,nodeName,expectedGWIPs]
	policyGWIPsMap, err := c.buildExternalIPGatewaysFromPolicyRules()
	if err != nil {
		klog.Errorf("Error while aggregating the external policy routes: %v", err)
	}

	annotatedGWIPsMap, err := c.buildExternalIPGatewaysFromAnnotations()
	if err != nil {
		klog.Errorf("Cannot retrieve the annotated gateway IPs:%w", err)
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
				klog.Errorf("Cannot sync exgw route: %+v, unable to determine exgw switch prefix: %v",
					ovnRoute, err)
			} else if (prefix != "" && !strings.Contains(ovnRoute.outport, prefix)) ||
				(prefix == "" && strings.Contains(ovnRoute.outport, types.EgressGWSwitchPrefix)) {
				continue
			}
			if expectedNextHopsPolicy != nil {
				ovnRoute.shouldExist = c.processOVNRoute(ovnRoute, expectedNextHopsPolicy.gwList, podIP, expectedNextHopsPolicy)
				if ovnRoute.shouldExist {
					continue
				}
			}
			if expectedNextHopsAnnotation != nil {
				ovnRoute.shouldExist = c.processOVNRoute(ovnRoute, expectedNextHopsAnnotation.gwList, podIP, expectedNextHopsAnnotation)
			}
		}
	}

	klog.Infof("OVN ECMP route cache is: %+v", ovnRouteCache)
	klog.Infof("Cluster ECMP route cache is: %+v", policyGWIPsMap)

	// iterate through ovn routes and remove any stale entries
	for podIP, ovnRoutes := range ovnRouteCache {
		podHasAnyECMPRoutes := false
		for _, ovnRoute := range ovnRoutes {
			if !ovnRoute.shouldExist {
				klog.Infof("Found stale exgw ecmp route, podIP: %s, nexthop: %s, router: %s",
					podIP, ovnRoute.nextHop, ovnRoute.router)
				lrsr := nbdb.LogicalRouterStaticRoute{UUID: ovnRoute.uuid}
				err := c.nbClient.deleteLogicalRouterStaticRoutes(ovnRoute.router, &lrsr)
				// err :=
				if err != nil {
					klog.Errorf("Error deleting static route %s from router %s: %v", ovnRoute.uuid, ovnRoute.router, err)
				}

				// check to see if we should also clean up bfd
				node := util.GetWorkerFromGatewayRouter(ovnRoute.router)
				// prefix will signify secondary exgw bridge, or empty if normal setup
				// have to determine if a node changed while master was down and if the route swapped from
				// the default bridge to a new secondary bridge (or vice versa)
				prefix, err := c.nbClient.extSwitchPrefix(node)
				if err != nil {
					// we shouldn't continue in this case, because we cant be sure this is a route we want to remove
					klog.Errorf("Cannot sync exgw bfd: %+v, unable to determine exgw switch prefix: %v",
						ovnRoute, err)
				} else {
					if err := c.nbClient.cleanUpBFDEntry(ovnRoute.nextHop, ovnRoute.router, prefix); err != nil {
						klog.Errorf("Cannot clean up BFD entry: %w", err)
					}
				}

			} else {
				podHasAnyECMPRoutes = true
			}
		}

		// if pod had no ECMP routes we need to make sure we remove logical route policy for local gw mode
		if !podHasAnyECMPRoutes {
			for _, ovnRoute := range ovnRoutes {
				gr := strings.TrimPrefix(ovnRoute.router, types.GWRouterPrefix)
				if err := c.nbClient.delHybridRoutePolicyForPod(net.ParseIP(podIP), gr); err != nil {
					klog.Errorf("Error while removing hybrid policy for pod IP: %s, on node: %s, error: %v",
						podIP, gr, err)
				}
			}
		}
	}
}

func (c *ExternalGatewayMasterController) buildExternalIPGatewaysFromPolicyRules() (map[string]*managedGWIPs, error) {

	clusterRouteCache := make(map[string]*managedGWIPs)
	externalRoutePolicies, err := c.routeLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, policy := range externalRoutePolicies {
		p, err := c.mgr.processExternalRoutePolicy(policy)
		if err != nil {
			return nil, err
		}
		// store the policy manifest in the routePolicy cache to avoid hitting the informer every time the annotation logic recalls all the gw IPs from the CRs.
		err = c.mgr.storeRoutePolicyInCache(policy)
		if err != nil {
			return nil, err
		}
		nsList, err := c.mgr.listNamespacesBySelector(p.targetNamespacesSelector)
		if err != nil {
			return nil, err
		}
		allGWIPs := make(gatewayInfoList, 0)
		allGWIPs = append(allGWIPs, p.staticGateways...)
		for _, gw := range p.dynamicGateways {
			allGWIPs = append(allGWIPs, gw)
		}
		for _, ns := range nsList {
			nsPods, err := c.podLister.Pods(ns.Name).List(labels.Everything())
			if err != nil {
				return nil, err
			}
			for _, nsPod := range nsPods {
				// ignore completed pods, host networked pods, pods not scheduled
				if util.PodWantsHostNetwork(nsPod) || util.PodCompleted(nsPod) || !util.PodScheduled(nsPod) {
					continue
				}
				for _, podIP := range nsPod.Status.PodIPs {
					podIPStr := utilnet.ParseIPSloppy(podIP.IP).String()
					clusterRouteCache[podIPStr] = &managedGWIPs{namespacedName: ktypes.NamespacedName{Namespace: nsPod.Namespace, Name: nsPod.Name}, nodeName: nsPod.Spec.NodeName, gwList: make(gatewayInfoList, 0)}
					for _, gwInfo := range allGWIPs {
						for gw := range gwInfo.gws {
							if utilnet.IsIPv6String(gw) != utilnet.IsIPv6String(podIPStr) {
								continue
							}
							clusterRouteCache[podIPStr].gwList = append(clusterRouteCache[podIPStr].gwList, gwInfo)
						}
					}
				}
			}
		}

	}
	// flag the route policy cache as populated so that the logic to retrieve the dynamic and static gw IPs from the annotation side can use the cache instead of hitting the informer.
	c.mgr.setRoutePolicyCacheAsPopulated()
	return clusterRouteCache, nil
}

func (c *ExternalGatewayMasterController) processOVNRoute(ovnRoute *ovnRoute, gwList gatewayInfoList, podIP string, managedIPGWInfo *managedGWIPs) bool {
	// podIP exists, check if route matches
	for _, gwInfo := range gwList {
		for clusterNextHop := range gwInfo.gws {
			if ovnRoute.nextHop == clusterNextHop {
				// populate the externalGWInfo cache with this pair podIP->next Hop IP.
				err := c.nbClient.updateExternalGWInfoCacheForPodIPWithGatewayIP(podIP, ovnRoute.nextHop, managedIPGWInfo.nodeName, gwInfo.bfdEnabled, managedIPGWInfo.namespacedName)
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

	nsList, err := c.namespaceLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, ns := range nsList {
		if nsGWIPs, ok := ns.Annotations[util.RoutingExternalGWsAnnotation]; ok && nsGWIPs != "" {
			gwInfo := &gatewayInfo{gws: sets.New[string]()}
			for _, ip := range strings.Split(nsGWIPs, ",") {
				podIPStr := utilnet.ParseIPSloppy(ip).String()
				gwInfo.gws.Insert(podIPStr)
			}
			if _, ok := ns.Annotations[util.BfdAnnotation]; ok {
				gwInfo.bfdEnabled = true
			}
			nsPodList, err := c.podLister.Pods(ns.Name).List(labels.Everything())
			if err != nil {
				return nil, err
			}
			// iterate through all the pods in the namespace and associate the gw ips to those that correspond
			populateManagedGWIPsCacheInNamespace(ns.Name, gwInfo, clusterRouteCache, nsPodList)
		}
	}

	podList, err := c.podLister.List(labels.Everything())
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
		gwInfo := &gatewayInfo{gws: foundGws}
		if _, ok := pod.Annotations[util.BfdAnnotation]; ok {
			gwInfo.bfdEnabled = true
		}
		for _, targetNs := range strings.Split(targetNamespaces, ",") {
			// iterate through all pods and associate the gw ips to those that correspond
			populateManagedGWIPsCacheInNamespace(targetNs, gwInfo, clusterRouteCache, podList)
		}
	}
	return clusterRouteCache, nil
}

func populateManagedGWIPsCacheInNamespace(targetNamespace string, gwInfo *gatewayInfo, cache map[string]*managedGWIPs, podList []*v1.Pod) {
	for gwIP := range gwInfo.gws {
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
					}
				}
				cache[podIPStr].gwList = append(cache[podIPStr].gwList, &gatewayInfo{gws: sets.New(gwIP), bfdEnabled: gwInfo.bfdEnabled})
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

func (c *ExternalGatewayMasterController) buildOVNECMPCache() map[string][]*ovnRoute {
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.Options["ecmp_symmetric_reply"] == "true"
	}
	logicalRouterStaticRoutes, err := c.nbClient.findLogicalRouterStaticRoutesWithPredicate(p)
	if err != nil {
		klog.Errorf("CleanECMPRoutes: failed to list ecmp routes: %v", err)
		return nil
	}

	ovnRouteCache := make(map[string][]*ovnRoute)
	for _, logicalRouterStaticRoute := range logicalRouterStaticRoutes {
		p := func(item *nbdb.LogicalRouter) bool {
			return util.SliceHasStringItem(item.StaticRoutes, logicalRouterStaticRoute.UUID)
		}
		logicalRouters, err := c.nbClient.findLogicalRoutersWithPredicate(p)
		if err != nil {
			klog.Errorf("CleanECMPRoutes: failed to find logical router for %s, err: %v", logicalRouterStaticRoute.UUID, err)
			continue
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
	return ovnRouteCache
}

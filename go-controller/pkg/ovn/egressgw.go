package ovn

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	kapi "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type gatewayInfo struct {
	gws        sets.String
	bfdEnabled bool
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

type externalRouteInfo struct {
	sync.Mutex
	deleted bool
	podName ktypes.NamespacedName
	// podExternalRoutes is a cache keeping the LR routes added to the GRs when
	// external gateways are used. The first map key is the podIP (src-ip of the route),
	// the second the GW IP (next hop), and the third the GR name
	podExternalRoutes map[string]map[string]string
}

// ensureRouteInfoLocked either gets the current routeInfo in the cache with a lock, or creates+locks a new one if missing
func (oc *DefaultNetworkController) ensureRouteInfoLocked(podName ktypes.NamespacedName) (*externalRouteInfo, error) {
	// We don't want to hold the cache lock while we try to lock the routeInfo (unless we are creating it, then we know
	// no one else is using it). This could lead to dead lock. Therefore the steps here are:
	// 1. Get the cache lock, try to find the routeInfo
	// 2. If routeInfo existed, release the cache lock
	// 3. If routeInfo did not exist, safe to hold the cache lock while we create the new routeInfo
	oc.exGWCacheMutex.Lock()
	routeInfo, ok := oc.externalGWCache[podName]
	if !ok {
		routeInfo = &externalRouteInfo{
			podExternalRoutes: make(map[string]map[string]string),
			podName:           podName,
		}
		// we are creating routeInfo and going to set it in podExternalRoutes map
		// so safe to hold the lock while we create and add it
		defer oc.exGWCacheMutex.Unlock()
		oc.externalGWCache[podName] = routeInfo
	} else {
		// if we found an existing routeInfo, do not hold the cache lock
		// while waiting for routeInfo to Lock
		oc.exGWCacheMutex.Unlock()
	}

	// 4. Now lock the routeInfo
	routeInfo.Lock()

	// 5. If routeInfo was deleted between releasing the cache lock and grabbing
	// the routeInfo lock, return an error so the caller doesn't use it and
	// retries the operation later
	if routeInfo.deleted {
		routeInfo.Unlock()
		return nil, fmt.Errorf("routeInfo for pod %s, was altered during ensure route info", podName)
	}

	return routeInfo, nil
}

// getRouteInfosForNamespace returns all routeInfos for a specific namespace
func (oc *DefaultNetworkController) getRouteInfosForNamespace(namespace string) []*externalRouteInfo {
	oc.exGWCacheMutex.RLock()
	defer oc.exGWCacheMutex.RUnlock()

	routes := make([]*externalRouteInfo, 0)
	for namespacedName, routeInfo := range oc.externalGWCache {
		if namespacedName.Namespace == namespace {
			routes = append(routes, routeInfo)
		}
	}

	return routes
}

// deleteRouteInfoLocked removes a routeInfo from the cache, and returns it locked
func (oc *DefaultNetworkController) deleteRouteInfoLocked(name ktypes.NamespacedName) *externalRouteInfo {
	// Attempt to find the routeInfo in the cache, release the cache lock while
	// we try to lock the routeInfo to avoid any deadlock
	oc.exGWCacheMutex.RLock()
	routeInfo := oc.externalGWCache[name]
	oc.exGWCacheMutex.RUnlock()

	if routeInfo == nil {
		return nil
	}
	routeInfo.Lock()

	if routeInfo.deleted {
		routeInfo.Unlock()
		return nil
	}

	routeInfo.deleted = true

	go func() {
		oc.exGWCacheMutex.Lock()
		defer oc.exGWCacheMutex.Unlock()
		if newRouteInfo := oc.externalGWCache[name]; routeInfo == newRouteInfo {
			delete(oc.externalGWCache, name)
		}
	}()

	return routeInfo
}

// addPodExternalGW handles detecting if a pod is serving as an external gateway for namespace(s) and adding routes
// to all pods in that namespace
func (oc *DefaultNetworkController) addPodExternalGW(pod *kapi.Pod) error {
	podRoutingNamespaceAnno := pod.Annotations[util.RoutingNamespaceAnnotation]
	if podRoutingNamespaceAnno == "" {
		return nil
	}
	enableBFD := false
	if _, ok := pod.Annotations[util.BfdAnnotation]; ok {
		enableBFD = true
	}

	klog.Infof("External gateway pod: %s, detected for namespace(s) %s", pod.Name, podRoutingNamespaceAnno)

	foundGws, err := getExGwPodIPs(pod)
	if err != nil {
		klog.Errorf("Error getting exgw IPs for pod: %s, error: %v", pod.Name, err)
		oc.recordPodEvent("ErrorAddingLogicalPort", err, pod)
		return nil
	}

	// if we found any gateways then we need to update current pods routing in the relevant namespace
	if len(foundGws) == 0 {
		klog.Warningf("No valid gateway IPs found for requested external gateway pod: %s", pod.Name)
		return nil
	}

	for _, namespace := range strings.Split(podRoutingNamespaceAnno, ",") {
		err := oc.addPodExternalGWForNamespace(namespace, pod, gatewayInfo{gws: foundGws, bfdEnabled: enableBFD})
		if err != nil {
			return err
		}
	}
	return nil
}

// addPodExternalGWForNamespace handles adding routes to all pods in that namespace for a pod GW
func (oc *DefaultNetworkController) addPodExternalGWForNamespace(namespace string, pod *kapi.Pod, egress gatewayInfo) error {
	nsInfo, nsUnlock, err := oc.ensureNamespaceLocked(namespace, false, nil)
	if err != nil {
		return fmt.Errorf("failed to ensure namespace locked: %v", err)
	}
	tmpPodGWs := oc.getRoutingPodGWs(nsInfo)
	tmpPodGWs[makePodGWKey(pod)] = egress
	if err = validateRoutingPodGWs(tmpPodGWs); err != nil {
		nsUnlock()
		return fmt.Errorf("unable to add pod: %s/%s as external gateway for namespace: %s, error: %v",
			pod.Namespace, pod.Name, namespace, err)
	}
	nsInfo.routingExternalPodGWs[makePodGWKey(pod)] = egress
	existingGWs := sets.NewString()
	for _, gwInfo := range nsInfo.routingExternalPodGWs {
		existingGWs.Insert(gwInfo.gws.UnsortedList()...)
	}
	nsUnlock()

	klog.Infof("Adding routes for external gateway pod: %s, next hops: %q, namespace: %s, bfd-enabled: %t",
		pod.Name, strings.Join(egress.gws.UnsortedList(), ","), namespace, egress.bfdEnabled)
	err = oc.addGWRoutesForNamespace(namespace, egress)
	if err != nil {
		return err
	}
	// add the exgw podIP to the namespace's k8s.ovn.org/external-gw-pod-ips list
	if err := util.UpdateExternalGatewayPodIPsAnnotation(oc.kube, namespace, existingGWs.List()); err != nil {
		klog.Errorf("Unable to update %s/%v annotation for namespace %s: %v", util.ExternalGatewayPodIPsAnnotation, existingGWs, namespace, err)
	}
	return nil
}

// addExternalGWsForNamespace handles adding annotated gw routes to all pods in namespace
// This should only be called with a lock on nsInfo
func (oc *DefaultNetworkController) addExternalGWsForNamespace(egress gatewayInfo, nsInfo *namespaceInfo, namespace string) error {
	if egress.gws == nil {
		return fmt.Errorf("unable to add gateways routes for namespace: %s, gateways are nil", namespace)
	}
	nsInfo.routingExternalGWs = egress
	return oc.addGWRoutesForNamespace(namespace, egress)
}

// addGWRoutesForNamespace handles adding routes for all existing pods in namespace
func (oc *DefaultNetworkController) addGWRoutesForNamespace(namespace string, egress gatewayInfo) error {
	existingPods, err := oc.watchFactory.GetPods(namespace)
	if err != nil {
		return fmt.Errorf("failed to get all the pods (%v)", err)
	}
	for _, pod := range existingPods {
		if util.PodCompleted(pod) || util.PodWantsHostNetwork(pod) {
			continue
		}
		podIPs := make([]*net.IPNet, 0)
		for _, podIP := range pod.Status.PodIPs {
			podIPStr := utilnet.ParseIPSloppy(podIP.IP).String()
			cidr := podIPStr + util.GetIPFullMask(podIPStr)
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				return fmt.Errorf("failed to parse CIDR: %s, error: %v", cidr, err)
			}
			podIPs = append(podIPs, ipNet)
		}
		if len(podIPs) == 0 {
			klog.Warningf("Will not add gateway routes pod %s/%s. IPs not found!", pod.Namespace, pod.Name)
			continue
		}
		if config.Gateway.DisableSNATMultipleGWs {
			// delete all perPodSNATs (if this pod was controlled by egressIP controller, it will stop working since
			// a pod cannot be used for multiple-external-gateways and egressIPs at the same time)
			if err = deletePodSNAT(oc.nbClient, pod.Spec.NodeName, []*net.IPNet{}, podIPs); err != nil {
				klog.Error(err.Error())
			}
		}
		podNsName := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
		if err := oc.addGWRoutesForPod([]*gatewayInfo{&egress}, podIPs, podNsName, pod.Spec.NodeName); err != nil {
			return err
		}
	}
	return nil
}

func (oc *DefaultNetworkController) createBFDStaticRoute(bfdEnabled bool, gw string, podIP, gr, port, mask string) error {
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
		ops, err = libovsdbops.CreateOrUpdateBFDOps(oc.nbClient, ops, &bfd)
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
	ops, err = libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicateOps(oc.nbClient, ops, gr, &lrsr, p,
		&lrsr.Options)
	if err != nil {
		return fmt.Errorf("error creating or updating static route %+v on router %s: %v", lrsr, gr, err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("error transacting static route: %v", err)
	}

	return nil
}

func (oc *DefaultNetworkController) deleteLogicalRouterStaticRoute(podIP, mask, gw, gr string) error {
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.Policy != nil &&
			*item.Policy == nbdb.LogicalRouterStaticRoutePolicySrcIP &&
			item.IPPrefix == podIP+mask &&
			item.Nexthop == gw
	}
	err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(oc.nbClient, gr, p)
	if err != nil {
		return fmt.Errorf("error deleting static route from router %s: %v", gr, err)
	}

	return nil
}

// deletePodGWRoute deletes all associated gateway routing resources for one
// pod gateway route
func (oc *DefaultNetworkController) deletePodGWRoute(routeInfo *externalRouteInfo, podIP, gw, gr string) error {
	if utilnet.IsIPv6String(gw) != utilnet.IsIPv6String(podIP) {
		return nil
	}

	mask := util.GetIPFullMask(podIP)
	if err := oc.deleteLogicalRouterStaticRoute(podIP, mask, gw, gr); err != nil {
		return fmt.Errorf("unable to delete pod %s ECMP route to GR %s, GW: %s: %w",
			routeInfo.podName, gr, gw, err)
	}

	klog.V(5).Infof("ECMP route deleted for pod: %s, on gr: %s, to gw: %s",
		routeInfo.podName, gr, gw)

	node := util.GetWorkerFromGatewayRouter(gr)
	// The gw is deleted from the routes cache after this func is called, length 1
	// means it is the last gw for the pod and the hybrid route policy should be deleted.
	if entry := routeInfo.podExternalRoutes[podIP]; len(entry) == 1 {
		if err := oc.delHybridRoutePolicyForPod(net.ParseIP(podIP), node); err != nil {
			return fmt.Errorf("unable to delete hybrid route policy for pod %s: err: %v", routeInfo.podName, err)
		}
	}

	portPrefix, err := oc.extSwitchPrefix(node)
	if err != nil {
		return err
	}
	return oc.cleanUpBFDEntry(gw, gr, portPrefix)
}

// deletePodExternalGW detects if a given pod is acting as an external GW and removes all routes in all namespaces
// associated with that pod
func (oc *DefaultNetworkController) deletePodExternalGW(pod *kapi.Pod) (err error) {
	podRoutingNamespaceAnno := pod.Annotations[util.RoutingNamespaceAnnotation]
	if podRoutingNamespaceAnno == "" {
		return nil
	}
	klog.Infof("Deleting routes for external gateway pod: %s, for namespace(s) %s", pod.Name,
		podRoutingNamespaceAnno)
	for _, namespace := range strings.Split(podRoutingNamespaceAnno, ",") {
		if err := oc.deletePodGWRoutesForNamespace(pod, namespace); err != nil {
			// if we encounter error while deleting things in one namespace we return and don't try subsequent namespaces
			return fmt.Errorf("failed to delete ecmp routes for pod %s in namespace %s", pod.Name, namespace)
		}
	}
	return nil
}

// deletePodGwRoutesForNamespace handles deleting all routes in a namespace for a specific pod GW
func (oc *DefaultNetworkController) deletePodGWRoutesForNamespace(pod *kapi.Pod, namespace string) (err error) {
	nsInfo, nsUnlock := oc.getNamespaceLocked(namespace, false)
	if nsInfo == nil {
		return nil
	}
	podGWKey := makePodGWKey(pod)
	// check if any gateways were stored for this pod
	foundGws, ok := nsInfo.routingExternalPodGWs[podGWKey]
	delete(nsInfo.routingExternalPodGWs, podGWKey)
	existingGWs := sets.NewString()
	for _, gwInfo := range nsInfo.routingExternalPodGWs {
		existingGWs.Insert(gwInfo.gws.UnsortedList()...)
	}
	nsUnlock()

	if !ok || len(foundGws.gws) == 0 {
		klog.Infof("No gateways found to remove for annotated gateway pod: %s on namespace: %s",
			pod, namespace)
		return nil
	}

	if err := oc.deleteGWRoutesForNamespace(namespace, foundGws.gws); err != nil {
		// add the entry back to nsInfo for retrying later
		nsInfo, nsUnlock := oc.getNamespaceLocked(namespace, false)
		if nsInfo == nil {
			return fmt.Errorf("failed to get nsInfo %s to add back all the gw routes: %w", namespace, err)
		}
		// we add back all the gw routes as we don't know which specific route for which pod error-ed out
		nsInfo.routingExternalPodGWs[podGWKey] = foundGws
		nsUnlock()
		return fmt.Errorf("failed to delete GW routes for pod %s: %w", pod.Name, err)
	}
	// remove the exgw podIP from the namespace's k8s.ovn.org/external-gw-pod-ips list
	if err := util.UpdateExternalGatewayPodIPsAnnotation(oc.kube, namespace, existingGWs.List()); err != nil {
		klog.Errorf("Unable to update %s/%v annotation for namespace %s: %v", util.ExternalGatewayPodIPsAnnotation, existingGWs, namespace, err)
	}
	return nil
}

// deleteGwRoutesForNamespace handles deleting routes to gateways for a pod on a specific GR.
// If a set of gateways is given, only routes for that gateway are deleted. If no gateways
// are given, all routes for the namespace are deleted.
func (oc *DefaultNetworkController) deleteGWRoutesForNamespace(namespace string, matchGWs sets.String) error {
	deleteAll := (matchGWs == nil || matchGWs.Len() == 0)
	for _, routeInfo := range oc.getRouteInfosForNamespace(namespace) {
		routeInfo.Lock()
		if routeInfo.deleted {
			routeInfo.Unlock()
			continue
		}
		for podIP, routes := range routeInfo.podExternalRoutes {
			for gw, gr := range routes {
				if deleteAll || matchGWs.Has(gw) {
					if err := oc.deletePodGWRoute(routeInfo, podIP, gw, gr); err != nil {
						// if we encounter error while deleting routes for one pod; we return and don't try subsequent pods
						routeInfo.Unlock()
						return fmt.Errorf("delete pod GW route failed: %w", err)
					}
					delete(routes, gw)
				}
			}
		}
		routeInfo.Unlock()
	}
	return nil
}

// deleteGwRoutesForPod handles deleting all routes to gateways for a pod IP on a specific GR
func (oc *DefaultNetworkController) deleteGWRoutesForPod(name ktypes.NamespacedName, podIPNets []*net.IPNet) (err error) {
	routeInfo := oc.deleteRouteInfoLocked(name)
	if routeInfo == nil {
		return nil
	}
	defer routeInfo.Unlock()

	for _, podIPNet := range podIPNets {
		podIP := podIPNet.IP.String()
		routes, ok := routeInfo.podExternalRoutes[podIP]
		if !ok {
			continue
		}
		if len(routes) == 0 {
			delete(routeInfo.podExternalRoutes, podIP)
			continue
		}
		for gw, gr := range routes {
			if err := oc.deletePodGWRoute(routeInfo, podIP, gw, gr); err != nil {
				// if we encounter error while deleting routes for one pod; we return and don't try subsequent pods
				return fmt.Errorf("delete pod GW route failed: %w", err)
			}
			delete(routes, gw)
		}
	}
	return nil
}

// addEgressGwRoutesForPod handles adding all routes to gateways for a pod on a specific GR
func (oc *DefaultNetworkController) addGWRoutesForPod(gateways []*gatewayInfo, podIfAddrs []*net.IPNet, podNsName ktypes.NamespacedName, node string) error {
	gr := util.GetGatewayRouterFromNode(node)

	routesAdded := 0
	portPrefix, err := oc.extSwitchPrefix(node)
	if err != nil {
		klog.Infof("Failed to find ext switch prefix for %s %v", node, err)
		return err
	}

	port := portPrefix + types.GWRouterToExtSwitchPrefix + gr
	routeInfo, err := oc.ensureRouteInfoLocked(podNsName)
	if err != nil {
		return fmt.Errorf("failed to ensure routeInfo for %s, error: %v", podNsName, err)
	}
	defer routeInfo.Unlock()
	for _, podIPNet := range podIfAddrs {
		for _, gateway := range gateways {
			// TODO (trozet): use the go bindings here and batch commands
			// validate the ip and gateway belong to the same address family
			gws, err := util.MatchAllIPStringFamily(utilnet.IsIPv6(podIPNet.IP), gateway.gws.UnsortedList())
			if err == nil {
				podIP := podIPNet.IP.String()
				for _, gw := range gws {
					// if route was already programmed, skip it
					if foundGR, ok := routeInfo.podExternalRoutes[podIP][gw]; ok && foundGR == gr {
						routesAdded++
						continue
					}
					mask := util.GetIPFullMask(podIP)

					if err := oc.createBFDStaticRoute(gateway.bfdEnabled, gw, podIP, gr, port, mask); err != nil {
						return err
					}
					if routeInfo.podExternalRoutes[podIP] == nil {
						routeInfo.podExternalRoutes[podIP] = make(map[string]string)
					}
					routeInfo.podExternalRoutes[podIP][gw] = gr
					routesAdded++
					if len(routeInfo.podExternalRoutes[podIP]) == 1 {
						if err := oc.addHybridRoutePolicyForPod(podIPNet.IP, node); err != nil {
							return err
						}
					}
				}
			} else {
				klog.Warningf("Address families for the pod address %s and gateway %s did not match", podIPNet.IP.String(), gateway.gws)
			}
		}
	}
	// if no routes are added return an error
	if routesAdded < 1 {
		return fmt.Errorf("gateway specified for namespace %s with gateway addresses %v but no valid routes exist for pod: %s",
			podNsName.Namespace, podIfAddrs, podNsName.Name)
	}
	return nil
}

// buildPodSNAT builds per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
func buildPodSNAT(extIPs, podIPNets []*net.IPNet) ([]*nbdb.NAT, error) {
	nats := make([]*nbdb.NAT, 0, len(extIPs)*len(podIPNets))
	var nat *nbdb.NAT

	for _, podIPNet := range podIPNets {
		podIP := podIPNet.IP.String()
		mask := util.GetIPFullMask(podIP)
		_, fullMaskPodNet, err := net.ParseCIDR(podIP + mask)
		if err != nil {
			return nil, fmt.Errorf("invalid IP: %s and mask: %s combination, error: %v", podIP, mask, err)
		}
		if len(extIPs) == 0 {
			nat = libovsdbops.BuildSNAT(nil, fullMaskPodNet, "", nil)
		} else {
			for _, gwIPNet := range extIPs {
				gwIP := gwIPNet.IP.String()
				if utilnet.IsIPv6String(gwIP) != utilnet.IsIPv6String(podIP) {
					continue
				}
				nat = libovsdbops.BuildSNAT(&gwIPNet.IP, fullMaskPodNet, "", nil)
			}
		}
		nats = append(nats, nat)
	}
	return nats, nil
}

// getExternalIPsGR returns all the externalIPs for a node(GR) from its l3 gateway annotation
func getExternalIPsGR(watchFactory *factory.WatchFactory, nodeName string) ([]*net.IPNet, error) {
	var err error
	node, err := watchFactory.GetNode(nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %v", nodeName, err)
	}
	l3GWConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return nil, fmt.Errorf("unable to parse node L3 gw annotation: %v", err)
	}
	return l3GWConfig.IPAddresses, nil
}

// deletePodSNAT removes per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// used when disableSNATMultipleGWs=true
func deletePodSNAT(nbClient libovsdbclient.Client, nodeName string, extIPs, podIPNets []*net.IPNet) error {
	ops, err := deletePodSNATOps(nbClient, nil, nodeName, extIPs, podIPNets)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to delete SNAT rule for pod on gateway router %s: %w", types.GWRouterPrefix+nodeName, err)
	}
	return nil
}

// deletePodSNATOps creates ovsdb operation that removes per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// used when disableSNATMultipleGWs=true
func deletePodSNATOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, nodeName string, extIPs, podIPNets []*net.IPNet) ([]ovsdb.Operation, error) {
	nats, err := buildPodSNAT(extIPs, podIPNets)
	if err != nil {
		return nil, err
	}
	logicalRouter := nbdb.LogicalRouter{
		Name: types.GWRouterPrefix + nodeName,
	}
	ops, err = libovsdbops.DeleteNATsOps(nbClient, ops, &logicalRouter, nats...)
	if err != nil {
		return nil, fmt.Errorf("failed create operation for deleting SNAT rule for pod on gateway router %s: %v", logicalRouter.Name, err)
	}
	return ops, nil
}

// addOrUpdatePodSNAT adds or updates per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// used when disableSNATMultipleGWs=true
func addOrUpdatePodSNAT(nbClient libovsdbclient.Client, nodeName string, extIPs, podIfAddrs []*net.IPNet) error {
	nats, err := buildPodSNAT(extIPs, podIfAddrs)
	if err != nil {
		return err
	}
	logicalRouter := nbdb.LogicalRouter{
		Name: types.GWRouterPrefix + nodeName,
	}
	if err := libovsdbops.CreateOrUpdateNATs(nbClient, &logicalRouter, nats...); err != nil {
		return fmt.Errorf("failed to update SNAT for pods of router %s: %v", logicalRouter.Name, err)
	}
	return nil
}

// addOrUpdatePodSNATOps returns the operation that adds or updates per pod SNAT rules towards the nodeIP that are
// applied to the GR where the pod resides
// used when disableSNATMultipleGWs=true
func addOrUpdatePodSNATOps(nbClient libovsdbclient.Client, nodeName string, extIPs, podIfAddrs []*net.IPNet, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {
	gr := types.GWRouterPrefix + nodeName
	router := &nbdb.LogicalRouter{Name: gr}
	nats, err := buildPodSNAT(extIPs, podIfAddrs)
	if err != nil {
		return nil, err
	}
	if ops, err = libovsdbops.CreateOrUpdateNATsOps(nbClient, ops, router, nats...); err != nil {
		return nil, fmt.Errorf("failed to update SNAT for pods of router: %s, error: %v", gr, err)
	}
	return ops, nil
}

// addHybridRoutePolicyForPod handles adding a higher priority allow policy to allow traffic to be routed normally
// by ecmp routes
func (oc *DefaultNetworkController) addHybridRoutePolicyForPod(podIP net.IP, node string) error {
	if config.Gateway.Mode == config.GatewayModeLocal {
		// Add podIP to the node's address_set.
		as, err := oc.addressSetFactory.EnsureAddressSet(types.HybridRoutePolicyPrefix + node)
		if err != nil {
			return fmt.Errorf("cannot ensure that addressSet for node %s exists %v", node, err)
		}
		err = as.AddIPs([]net.IP{(podIP)})
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
		grJoinIfAddrs, err := util.GetLRPAddrs(oc.nbClient, types.GWRouterToJoinSwitchPrefix+types.GWRouterPrefix+node)
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
		err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(oc.nbClient, types.OVNClusterRouter,
			&logicalRouterPolicy, p, &logicalRouterPolicy.Nexthops, &logicalRouterPolicy.Match, &logicalRouterPolicy.Action)
		if err != nil {
			return fmt.Errorf("failed to add policy route %+v to %s: %v", logicalRouterPolicy, types.OVNClusterRouter, err)
		}
	}
	return nil
}

// delHybridRoutePolicyForPod handles deleting a logical route policy that
// forces pod egress traffic to be rerouted to a gateway router for local gateway mode.
func (oc *DefaultNetworkController) delHybridRoutePolicyForPod(podIP net.IP, node string) error {
	if config.Gateway.Mode == config.GatewayModeLocal {
		// Delete podIP from the node's address_set.
		as, err := oc.addressSetFactory.EnsureAddressSet(types.HybridRoutePolicyPrefix + node)
		if err != nil {
			return fmt.Errorf("cannot Ensure that addressSet for node %s exists %v", node, err)
		}
		err = as.DeleteIPs([]net.IP{(podIP)})
		if err != nil {
			return fmt.Errorf("unable to remove PodIP %s: to the address set %s, err: %v", podIP.String(), node, err)
		}

		// delete hybrid policy to bypass lr-policy in GR, only if there are zero pods on this node.
		ipv4HashedAS, ipv6HashedAS := as.GetASHashNames()
		ipv4PodIPs, ipv6PodIPs := as.GetIPs()
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
			err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, types.OVNClusterRouter, p)
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
	}
	return nil
}

// delAllHybridRoutePolicies deletes all the 501 hybrid-route-policies that
// force pod egress traffic to be rerouted to a gateway router for local gateway mode.
// Called when migrating to SGW from LGW.
func (oc *DefaultNetworkController) delAllHybridRoutePolicies() error {
	// nuke all the policies
	policyPred := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == types.HybridOverlayReroutePriority
	}
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, types.OVNClusterRouter, policyPred)
	if err != nil {
		return fmt.Errorf("error deleting hybrid route policies on %s: %v", types.OVNClusterRouter, err)
	}

	// nuke all the address-sets.
	// if we fail to remove LRP's above, we don't attempt to remove ASes due to dependency constraints.
	asPred := func(item *nbdb.AddressSet) bool {
		return strings.Contains(item.ExternalIDs["name"], types.HybridRoutePolicyPrefix)
	}
	err = libovsdbops.DeleteAddressSetsWithPredicate(oc.nbClient, asPred)
	if err != nil {
		return fmt.Errorf("failed to remove hybrid route address sets: %v", err)
	}

	return nil
}

// delAllLegacyHybridRoutePolicies deletes all the 501 hybrid-route-policies that
// force pod egress traffic to be rerouted to a gateway router for local gateway mode.
// New hybrid route matches on address set, while legacy matches just on pod IP
func (oc *DefaultNetworkController) delAllLegacyHybridRoutePolicies() error {
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
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, types.OVNClusterRouter, p)
	if err != nil {
		return fmt.Errorf("error deleting legacy hybrid route policies on %s: %v", types.OVNClusterRouter, err)
	}

	return nil
}

// cleanUpBFDEntry checks if the BFD table entry related to the associated
// gw router / port / gateway ip is referenced by other routing rules, and if
// not removes the entry to avoid having dangling BFD entries.
func (oc *DefaultNetworkController) cleanUpBFDEntry(gatewayIP, gatewayRouter, prefix string) error {
	portName := prefix + types.GWRouterToExtSwitchPrefix + gatewayRouter
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.OutputPort != nil && *item.OutputPort == portName && item.Nexthop == gatewayIP && item.BFD != nil && *item.BFD != ""
	}
	logicalRouterStaticRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(oc.nbClient, p)
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

	err = libovsdbops.DeleteBFDs(oc.nbClient, &bfd)
	if err != nil {
		return fmt.Errorf("error deleting BFD %+v: %v", bfd, err)
	}

	return nil
}

// extSwitchPrefix returns the prefix of the external switch to use for
// external gateway routes. In case no second bridge is configured, we
// use the default one and the prefix is empty.
func (oc *DefaultNetworkController) extSwitchPrefix(nodeName string) (string, error) {
	node, err := oc.watchFactory.GetNode(nodeName)
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

func (oc *DefaultNetworkController) cleanExGwECMPRoutes() {
	start := time.Now()
	defer func() {
		klog.Infof("Syncing exgw routes took %v", time.Since(start))
	}()

	// migration from LGW to SGW mode
	// for shared gateway mode, these LRPs shouldn't exist, so delete them all
	if config.Gateway.Mode == config.GatewayModeShared {
		if err := oc.delAllHybridRoutePolicies(); err != nil {
			klog.Errorf("Error while removing hybrid policies on moving to SGW mode, error: %v", err)
		}
	} else if config.Gateway.Mode == config.GatewayModeLocal {
		// remove all legacy hybrid route policies
		if err := oc.delAllLegacyHybridRoutePolicies(); err != nil {
			klog.Errorf("Error while removing legacy hybrid policies, error: %v", err)
		}
	}

	// Get all ECMP routes in OVN and build cache
	ovnRouteCache := oc.buildOVNECMPCache()

	if len(ovnRouteCache) == 0 {
		// Even if no ECMP routes exist, we should ensure no 501 LRPs exist either
		if err := oc.delAllHybridRoutePolicies(); err != nil {
			klog.Errorf("Error while removing hybrid policies, error: %v", err)
		}
		// nothing in OVN, so no reason to search for stale routes
		return
	}

	// Build cache of expected routes in the cluster
	// map[podIP][]nextHops
	clusterRouteCache := make(map[string][]string)

	// Find all pods serving as exgw
	oc.buildClusterECMPCacheFromPods(clusterRouteCache)

	// Get all namespaces with exgw routes specified
	oc.buildClusterECMPCacheFromNamespaces(clusterRouteCache)

	// compare caches and see if OVN routes are stale
	for podIP, ovnRoutes := range ovnRouteCache {
		// pod IP does not exist in the cluster
		// remove route and any hybrid policy
		if _, ok := clusterRouteCache[podIP]; !ok {
			continue
		}

		// podIP exists, check if route matches
		expectedNexthops := clusterRouteCache[podIP]
		for _, ovnRoute := range ovnRoutes {
			// if length of the output port is 0, this is a legacy route (we now always specify output interface)
			if len(ovnRoute.outport) == 0 {
				continue
			}

			node := util.GetWorkerFromGatewayRouter(ovnRoute.router)
			// prefix will signify secondary exgw bridge, or empty if normal setup
			// have to determine if a node changed while master was down and if the route swapped from
			// the default bridge to a new secondary bridge (or vice versa)
			prefix, err := oc.extSwitchPrefix(node)
			if err != nil {
				// we shouldn't continue in this case, because we cant be sure this is a route we want to remove
				klog.Errorf("Cannot sync exgw route: %+v, unable to determine exgw switch prefix: %v",
					ovnRoute, err)
			} else if (prefix != "" && !strings.Contains(ovnRoute.outport, prefix)) ||
				(prefix == "" && strings.Contains(ovnRoute.outport, types.EgressGWSwitchPrefix)) {
				continue
			}

			for _, clusterNexthop := range expectedNexthops {
				if ovnRoute.nextHop == clusterNexthop {
					ovnRoute.shouldExist = true
				}
			}
		}
	}

	klog.Infof("OVN ECMP route cache is: %+v", ovnRouteCache)
	klog.Infof("Cluster ECMP route cache is: %+v", clusterRouteCache)

	// iterate through ovn routes and remove any stale entries
	for podIP, ovnRoutes := range ovnRouteCache {
		podHasAnyECMPRoutes := false
		for _, ovnRoute := range ovnRoutes {
			if !ovnRoute.shouldExist {
				klog.Infof("Found stale exgw ecmp route, podIP: %s, nexthop: %s, router: %s",
					podIP, ovnRoute.nextHop, ovnRoute.router)
				lrsr := nbdb.LogicalRouterStaticRoute{UUID: ovnRoute.uuid}
				err := libovsdbops.DeleteLogicalRouterStaticRoutes(oc.nbClient, ovnRoute.router, &lrsr)
				if err != nil {
					klog.Errorf("Error deleting static route %s from router %s: %v", ovnRoute.uuid, ovnRoute.router, err)
				}

				// check to see if we should also clean up bfd
				node := util.GetWorkerFromGatewayRouter(ovnRoute.router)
				// prefix will signify secondary exgw bridge, or empty if normal setup
				// have to determine if a node changed while master was down and if the route swapped from
				// the default bridge to a new secondary bridge (or vice versa)
				prefix, err := oc.extSwitchPrefix(node)
				if err != nil {
					// we shouldn't continue in this case, because we cant be sure this is a route we want to remove
					klog.Errorf("Cannot sync exgw bfd: %+v, unable to determine exgw switch prefix: %v",
						ovnRoute, err)
				} else {
					if err := oc.cleanUpBFDEntry(ovnRoute.nextHop, ovnRoute.router, prefix); err != nil {
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
				if err := oc.delHybridRoutePolicyForPod(net.ParseIP(podIP), gr); err != nil {
					klog.Errorf("Error while removing hybrid policy for pod IP: %s, on node: %s, error: %v",
						podIP, gr, err)
				}
			}
		}
	}
}

func getExGwPodIPs(gatewayPod *kapi.Pod) (sets.String, error) {
	foundGws := sets.NewString()
	if gatewayPod.Annotations[util.RoutingNetworkAnnotation] != "" {
		var multusNetworks []nettypes.NetworkStatus
		err := json.Unmarshal([]byte(gatewayPod.ObjectMeta.Annotations[nettypes.NetworkStatusAnnot]), &multusNetworks)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshall annotation k8s.v1.cni.cncf.io/network-status on pod %s: %v",
				gatewayPod.Name, err)
		}
		for _, multusNetwork := range multusNetworks {
			if multusNetwork.Name == gatewayPod.Annotations[util.RoutingNetworkAnnotation] {
				for _, gwIP := range multusNetwork.IPs {
					ip := net.ParseIP(gwIP)
					if ip != nil {
						foundGws.Insert(ip.String())
					}
				}
			}
		}
	} else if gatewayPod.Spec.HostNetwork {
		for _, podIP := range gatewayPod.Status.PodIPs {
			ip := utilnet.ParseIPSloppy(podIP.IP)
			if ip != nil {
				foundGws.Insert(ip.String())
			}
		}
	} else {
		return nil, fmt.Errorf("ignoring pod %s as an external gateway candidate. Invalid combination "+
			"of host network: %t and routing-network annotation: %s", gatewayPod.Name, gatewayPod.Spec.HostNetwork,
			gatewayPod.Annotations[util.RoutingNetworkAnnotation])
	}
	return foundGws, nil
}

func (oc *DefaultNetworkController) buildClusterECMPCacheFromNamespaces(clusterRouteCache map[string][]string) {
	namespaces, err := oc.watchFactory.GetNamespaces()
	if err != nil {
		klog.Errorf("Error getting all namespaces for exgw ecmp route sync: %v", err)
		return
	}
	for _, namespace := range namespaces {
		if _, ok := namespace.Annotations[util.RoutingExternalGWsAnnotation]; !ok {
			continue
		}
		// namespace has exgw routes, build cache
		gwIPs, err := util.ParseRoutingExternalGWAnnotation(namespace.Annotations[util.RoutingExternalGWsAnnotation])
		if err != nil {
			klog.Errorf("Unable to clean ExGw ECMP routes for namespace: %s, %v", namespace.Name, err)
			continue
		}
		// get all pods in the namespace
		nsPods, err := oc.watchFactory.GetPods(namespace.Name)
		if err != nil {
			klog.Errorf("Unable to clean ExGw ECMP routes for namespace: %s, %v",
				namespace, err)
			continue
		}
		for _, gwIP := range gwIPs.UnsortedList() {
			for _, nsPod := range nsPods {
				// ignore completed pods, host networked pods, pods not scheduled
				if util.PodWantsHostNetwork(nsPod) || util.PodCompleted(nsPod) || !util.PodScheduled(nsPod) {
					continue
				}
				for _, podIP := range nsPod.Status.PodIPs {
					podIPStr := utilnet.ParseIPSloppy(podIP.IP).String()
					if utilnet.IsIPv6String(gwIP) != utilnet.IsIPv6String(podIPStr) {
						continue
					}
					if val, ok := clusterRouteCache[podIPStr]; ok {
						// add gwIP to cache only if buildClusterECMPCacheFromPods hasn't already added it
						gwIPexists := false
						for _, existingGwIP := range val {
							if existingGwIP == gwIP {
								gwIPexists = true
								break
							}
						}
						if !gwIPexists {
							clusterRouteCache[podIPStr] = append(clusterRouteCache[podIPStr], gwIP)
						}
					} else {
						clusterRouteCache[podIPStr] = []string{gwIP}
					}
				}
			}
		}
	}
}

func (oc *DefaultNetworkController) buildClusterECMPCacheFromPods(clusterRouteCache map[string][]string) {
	// Get all Pods serving as exgws
	pods, err := oc.watchFactory.GetAllPods()
	if err != nil {
		klog.Error("Error getting all pods for exgw ecmp route sync: %v", err)
		return
	}
	for _, pod := range pods {
		podRoutingNamespaceAnno := pod.Annotations[util.RoutingNamespaceAnnotation]
		if podRoutingNamespaceAnno == "" {
			continue
		}
		// get all pods in the namespace
		nsPods, err := oc.watchFactory.GetPods(podRoutingNamespaceAnno)
		if err != nil {
			klog.Errorf("Unable to clean ExGw ECMP routes for exgw: %s, serving namespace: %s, %v",
				pod.Name, podRoutingNamespaceAnno, err)
			continue
		}

		// pod is serving as exgw, build cache
		gwIPs, err := getExGwPodIPs(pod)
		if err != nil {
			klog.Errorf("Error getting exgw IPs for pod: %s, error: %v", pod.Name, err)
			continue
		}
		for _, gwIP := range gwIPs.UnsortedList() {
			for _, nsPod := range nsPods {
				// ignore completed pods, host networked pods, pods not scheduled
				if util.PodWantsHostNetwork(nsPod) || util.PodCompleted(nsPod) || !util.PodScheduled(nsPod) {
					continue
				}
				for _, podIP := range nsPod.Status.PodIPs {
					podIPStr := utilnet.ParseIPSloppy(podIP.IP).String()
					if utilnet.IsIPv6String(gwIP) != utilnet.IsIPv6String(podIPStr) {
						continue
					}
					clusterRouteCache[podIPStr] = append(clusterRouteCache[podIPStr], gwIP)
				}
			}
		}
	}
}

func (oc *DefaultNetworkController) buildOVNECMPCache() map[string][]*ovnRoute {
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.Options["ecmp_symmetric_reply"] == "true"
	}
	logicalRouterStaticRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(oc.nbClient, p)
	if err != nil {
		klog.Errorf("CleanECMPRoutes: failed to list ecmp routes: %v", err)
		return nil
	}

	ovnRouteCache := make(map[string][]*ovnRoute)
	for _, logicalRouterStaticRoute := range logicalRouterStaticRoutes {
		p := func(item *nbdb.LogicalRouter) bool {
			return util.SliceHasStringItem(item.StaticRoutes, logicalRouterStaticRoute.UUID)
		}
		logicalRouters, err := libovsdbops.FindLogicalRoutersWithPredicate(oc.nbClient, p)
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

func makePodGWKey(pod *kapi.Pod) string {
	return fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
}

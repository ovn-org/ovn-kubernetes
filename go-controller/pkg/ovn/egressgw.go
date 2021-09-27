package ovn

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	kapi "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

const (
	DuplicateECMPError = "duplicate nexthop for the same ECMP route"
)

type gatewayInfo struct {
	gws        []net.IP
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
	sync.RWMutex
	// podExternalRoutes is a cache keeping the LR routes added to the GRs when
	// external gateways are used. The first map key is the podIP (src-ip of the route),
	// the second the GW IP (next hop), and the third the GR name
	podExternalRoutes map[string]map[string]string
}

// ensureRouteInfoLocked either gets the current routeInfo in the cache with a lock, or creates+locks a new one if missing
func (oc *Controller) ensureRouteInfoLocked(podName ktypes.NamespacedName) (*externalRouteInfo, error) {
	// We don't want to hold the cache lock while we try to lock the routeInfo (unless we are creating it, then we know
	// no one else is using it). This could lead to dead lock. Therefore the steps here are:
	// 1. Get the cache lock, try to find the routeInfo
	// 2. If routeInfo existed, release the cache lock
	// 3. If routeInfo did not exist, safe to hold the cache lock while we create the new routeInfo
	oc.exGWCacheMutex.Lock()
	routeInfo, routeInfoExisted := oc.externalGWCache[podName]
	if routeInfo == nil {
		routeInfo = &externalRouteInfo{
			podExternalRoutes: make(map[string]map[string]string),
		}
		// we are creating routeInfo and going to set it in podExternalRoutes map
		// so safe to hold the lock while we create and add it
		defer oc.exGWCacheMutex.Unlock()
		oc.externalGWCache[podName] = routeInfo
	} else {
		routeInfoExisted = true
		// if we found an existing routeInfo, do not hold the cache lock
		// while waiting for routeInfo to Lock
		oc.exGWCacheMutex.Unlock()
	}

	// 4. Now lock the routeInfo
	routeInfo.Lock()

	// 5. If routeInfo originally existed in the cache, there is a chance it was altered after we
	// released the cache lock, and before we were able to lock routeInfo
	if routeInfoExisted {
		// Check that the routeInfo wasn't altered in the cache while we were waiting for the lock
		// and that routeInfo that we locked is not stale.
		oc.exGWCacheMutex.RLock()
		defer oc.exGWCacheMutex.RUnlock()
		if routeInfo != oc.externalGWCache[podName] {
			routeInfo.Unlock()
			return nil, fmt.Errorf("routeInfo for pod %s, was altered during ensure route info", podName)
		}
	}

	return routeInfo, nil
}

// getRouteInfosForGateway returns all routeInfos locked for a specific namespace and gateway IP
func (oc *Controller) getRouteInfosForGateway(gatewayIP, namespace string) []*externalRouteInfo {
	oc.exGWCacheMutex.RLock()
	defer oc.exGWCacheMutex.RUnlock()

	routes := make([]*externalRouteInfo, 0)
	for namespacedName, routeInfo := range oc.externalGWCache {
		if namespacedName.Namespace != namespace {
			continue
		}

		routeFound := false
		for _, route := range routeInfo.podExternalRoutes {
			if _, ok := route[gatewayIP]; ok {
				routes = append(routes, routeInfo)
				routeFound = true
			}
		}
		if routeFound {
			routeInfo.Lock()
		}
	}

	return routes
}

// getRouteInfosForNamespace returns all routeInfos locked for a specific namespace
func (oc *Controller) getRouteInfosForNamespace(namespace string) []*externalRouteInfo {
	oc.exGWCacheMutex.RLock()
	defer oc.exGWCacheMutex.RUnlock()

	routes := make([]*externalRouteInfo, 0)
	for namespacedName, routeInfo := range oc.externalGWCache {
		if namespacedName.Namespace != namespace {
			continue
		}
		routeInfo.Lock()
		routes = append(routes, routeInfo)
	}

	return routes
}

// deleteRouteInfoLocked removes a routeInfo from the cache, and returns it locked
func (oc *Controller) deleteRouteInfoLocked(name ktypes.NamespacedName) *externalRouteInfo {
	// Attempt to find the routeInfo in the cache, release the cache lock while
	// we try to lock the routeInfo to avoid any deadlock
	oc.exGWCacheMutex.RLock()
	routeInfo := oc.externalGWCache[name]
	oc.exGWCacheMutex.RUnlock()

	if routeInfo == nil {
		return nil
	}
	routeInfo.Lock()

	// ensure the routeInfo we acquired the lock for is consistent with the
	// one still in the cache
	oc.exGWCacheMutex.Lock()
	defer oc.exGWCacheMutex.Unlock()
	if routeInfo != oc.externalGWCache[name] {
		routeInfo.Unlock()
		return nil
	}

	delete(oc.externalGWCache, name)

	return routeInfo
}

// addPodExternalGW handles detecting if a pod is serving as an external gateway for namespace(s) and adding routes
// to all pods in that namespace
func (oc *Controller) addPodExternalGW(pod *kapi.Pod) error {
	podRoutingNamespaceAnno := pod.Annotations[routingNamespaceAnnotation]
	if podRoutingNamespaceAnno == "" {
		return nil
	}
	enableBFD := false
	if _, ok := pod.Annotations[bfdAnnotation]; ok {
		enableBFD = true
	}

	klog.Infof("External gateway pod: %s, detected for namespace(s) %s", pod.Name, podRoutingNamespaceAnno)

	foundGws, err := getExGwPodIPs(pod)
	if err != nil {
		return err
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
func (oc *Controller) addPodExternalGWForNamespace(namespace string, pod *kapi.Pod, egress gatewayInfo) error {
	var gws string
	for _, ip := range egress.gws {
		if len(gws) != 0 {
			gws += ","
		}
		gws += ip.String()
	}
	nsInfo, nsUnlock, err := oc.ensureNamespaceLocked(namespace, false)
	if err != nil {
		return fmt.Errorf("failed to ensure namespace locked: %v", err)
	}
	nsInfo.routingExternalPodGWs[pod.Name] = egress
	nsUnlock()

	klog.Infof("Adding routes for external gateway pod: %s, next hops: %q, namespace: %s, bfd-enabled: %t",
		pod.Name, gws, namespace, egress.bfdEnabled)
	return oc.addGWRoutesForNamespace(namespace, egress)
}

// addExternalGWsForNamespace handles adding annotated gw routes to all pods in namespace
// This should only be called with a lock on nsInfo
func (oc *Controller) addExternalGWsForNamespace(egress gatewayInfo, nsInfo *namespaceInfo, namespace string) error {
	if egress.gws == nil {
		return fmt.Errorf("unable to add gateways routes for namespace: %s, gateways are nil", namespace)
	}
	nsInfo.routingExternalGWs = egress
	return oc.addGWRoutesForNamespace(namespace, egress)
}

// addGWRoutesForNamespace handles adding routes for all existing pods in namespace
func (oc *Controller) addGWRoutesForNamespace(namespace string, egress gatewayInfo) error {
	existingPods, err := oc.watchFactory.GetPods(namespace)
	if err != nil {
		return fmt.Errorf("failed to get all the pods (%v)", err)
	}
	// TODO (trozet): use the go bindings here and batch commands
	for _, pod := range existingPods {
		podNsName := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
		if config.Gateway.DisableSNATMultipleGWs {
			logicalPort := util.GetLogicalPortName(pod.Namespace, pod.Name)
			portInfo, err := oc.logicalPortCache.get(logicalPort)
			if err != nil {
				klog.Warningf("Unable to get port %s in cache for SNAT rule removal", logicalPort)
			} else {
				oc.deletePerPodGRSNAT(pod.Spec.NodeName, portInfo.ips)
			}
		}

		podIPs := make([]*net.IPNet, 0)
		for _, podIP := range pod.Status.PodIPs {
			cidr := podIP.IP + GetIPFullMask(podIP.IP)
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				return fmt.Errorf("failed to parse CIDR: %s, error: %v", cidr, err)
			}
			podIPs = append(podIPs, ipNet)
		}
		if err := oc.addGWRoutesForPod([]*gatewayInfo{&egress}, podIPs, podNsName, pod.Spec.NodeName); err != nil {
			return err
		}
	}
	return nil
}

// deletePodExternalGW detects if a given pod is acting as an external GW and removes all routes in all namespaces
// associated with that pod
func (oc *Controller) deletePodExternalGW(pod *kapi.Pod) {
	podRoutingNamespaceAnno := pod.Annotations[routingNamespaceAnnotation]
	if podRoutingNamespaceAnno == "" {
		return
	}
	klog.Infof("Deleting routes for external gateway pod: %s, for namespace(s) %s", pod.Name,
		podRoutingNamespaceAnno)
	for _, namespace := range strings.Split(podRoutingNamespaceAnno, ",") {
		oc.deletePodGWRoutesForNamespace(pod.Name, namespace)
	}
}

// deletePodGwRoutesForNamespace handles deleting all routes in a namespace for a specific pod GW
func (oc *Controller) deletePodGWRoutesForNamespace(pod, namespace string) {
	nsInfo, nsUnlock := oc.getNamespaceLocked(namespace, false)
	if nsInfo == nil {
		return
	}
	// check if any gateways were stored for this pod
	foundGws, ok := nsInfo.routingExternalPodGWs[pod]
	delete(nsInfo.routingExternalPodGWs, pod)
	nsUnlock()

	if !ok || len(foundGws.gws) == 0 {
		klog.Infof("No gateways found to remove for annotated gateway pod: %s on namespace: %s",
			pod, namespace)
		return
	}

	for _, gwIP := range foundGws.gws {
		// check for previously configured pod routes
		routeInfos := oc.getRouteInfosForGateway(gwIP.String(), namespace)
		for _, routeInfo := range routeInfos {
			for podIP, route := range routeInfo.podExternalRoutes {
				for routeGwIP, gr := range route {
					if gwIP.String() != routeGwIP {
						continue
					}
					if gr == "" {
						continue
					}
					mask := GetIPFullMask(podIP)
					node := util.GetWorkerFromGatewayRouter(gr)
					portPrefix, err := oc.extSwitchPrefix(node)
					if err != nil {
						klog.Infof("Failed to find ext switch prefix for %s %v", node, err)
						continue
					}

					_, stderr, err := util.RunOVNNbctl("--if-exists", "--policy=src-ip",
						"lr-route-del", gr, podIP+mask, gwIP.String())
					if err != nil {
						klog.Errorf("Unable to delete pod %s route to GR %s, GW: %s, stderr:%q, err:%v",
							pod, gr, gwIP.String(), stderr, err)
					} else {
						klog.V(5).Infof("ECMP route deleted for pod: %s, on gr: %s, to gw: %s", pod,
							gr, gwIP.String())

						delete(routeInfo.podExternalRoutes[podIP], gwIP.String())
						// clean up if there are no more routes for this podIP
						if entry := routeInfo.podExternalRoutes[podIP]; len(entry) == 0 {
							// TODO (trozet): use the go bindings here and batch commands
							// delete the ovn_cluster_router policy if the pod has no more exgws to revert back to normal
							// default gw behavior
							if err := oc.delHybridRoutePolicyForPod(net.ParseIP(podIP), node, false); err != nil {
								klog.Error(err)
							}
						}
					}
					cleanUpBFDEntry(gwIP.String(), gr, portPrefix)
				}
			}
			routeInfo.Unlock()
		}
	}
}

// deleteGwRoutesForNamespace handles deleting all routes to gateways for a pod on a specific GR
func (oc *Controller) deleteGWRoutesForNamespace(namespace string) {
	// TODO(trozet): batch all of these with ebay bindings
	routeInfos := oc.getRouteInfosForNamespace(namespace)
	for _, routeInfo := range routeInfos {
		for podIP, gwToGr := range routeInfo.podExternalRoutes {
			for gw, gr := range gwToGr {
				if utilnet.IsIPv6String(gw) != utilnet.IsIPv6String(podIP) {
					continue
				}
				mask := GetIPFullMask(podIP)
				node := util.GetWorkerFromGatewayRouter(gr)
				if err := oc.delHybridRoutePolicyForPod(net.ParseIP(podIP), node, false); err != nil {
					klog.Error(err)
				}
				_, stderr, err := util.RunOVNNbctl("--if-exists", "--policy=src-ip",
					"lr-route-del", gr, podIP+mask, gw)
				if err != nil {
					klog.Errorf("Unable to delete src-ip route to GR router, stderr:%q, err:%v", stderr, err)
				} else {
					delete(routeInfo.podExternalRoutes[podIP], gw)
				}

				portPrefix, err := oc.extSwitchPrefix(node)
				if err != nil {
					klog.Infof("Failed to find ext switch prefix for %s %v", node, err)
					continue
				}
				cleanUpBFDEntry(gw, gr, portPrefix)
			}
		}
		routeInfo.Unlock()
	}
}

// deleteGwRoutesForPod handles deleting all routes to gateways for a pod IP on a specific GR
func (oc *Controller) deleteGWRoutesForPod(name ktypes.NamespacedName, podIPNets []*net.IPNet) {
	routeInfo := oc.deleteRouteInfoLocked(name)
	if routeInfo == nil {
		return
	}
	defer routeInfo.Unlock()

	for _, podIPNet := range podIPNets {
		pod := podIPNet.IP.String()
		if gwToGr, ok := routeInfo.podExternalRoutes[pod]; ok {
			if len(gwToGr) == 0 {
				delete(routeInfo.podExternalRoutes, pod)
				return
			}
			mask := GetIPFullMask(pod)
			for gw, gr := range gwToGr {
				node := util.GetWorkerFromGatewayRouter(gr)
				portPrefix, err := oc.extSwitchPrefix(node)
				if err != nil {
					klog.Infof("Failed to find ext switch prefix for %s %v", node, err)
					continue
				}

				if err := oc.delHybridRoutePolicyForPod(podIPNet.IP, node, false); err != nil {
					klog.Error(err)
				}
				_, stderr, err := util.RunOVNNbctl("--if-exists", "--policy=src-ip",
					"lr-route-del", gr, pod+mask, gw)
				if err != nil {
					klog.Errorf("Unable to delete ECMP route for pod: %s to GR %s, GW: %s, stderr:%q, err:%v",
						name, gr, gw, stderr, err)
				} else {
					delete(routeInfo.podExternalRoutes[pod], gw)
					klog.V(5).Infof("ECMP route deleted for pod: %s, on gr: %s, to gw: %s", name,
						gr, gw)
				}
				cleanUpBFDEntry(gw, gr, portPrefix)
			}
		}
	}
}

// addEgressGwRoutesForPod handles adding all routes to gateways for a pod on a specific GR
func (oc *Controller) addGWRoutesForPod(gateways []*gatewayInfo, podIfAddrs []*net.IPNet, podNsName ktypes.NamespacedName, node string) error {
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
			gws, err := util.MatchIPFamily(utilnet.IsIPv6(podIPNet.IP), gateway.gws)
			if err == nil {
				podIP := podIPNet.IP.String()
				for _, gw := range gws {
					gwStr := gw.String()
					// if route was already programmed, skip it
					if foundGR, ok := routeInfo.podExternalRoutes[podIP][gwStr]; ok && foundGR == gr {
						routesAdded++
						continue
					}
					mask := GetIPFullMask(podIP)
					nbctlArgs := []string{"--may-exist", "--policy=src-ip", "--ecmp-symmetric-reply",
						"lr-route-add", gr, podIP + mask, gw.String(), port}
					if gateway.bfdEnabled {
						nbctlArgs = []string{"--may-exist", "--bfd", "--policy=src-ip", "--ecmp-symmetric-reply",
							"lr-route-add", gr, podIP + mask, gw.String(), port}
					}
					_, stderr, err := util.RunOVNNbctl(nbctlArgs...)
					if err != nil && !strings.Contains(stderr, DuplicateECMPError) {
						return fmt.Errorf("unable to add external gwStr src-ip route to GR router, stderr:%q, err:%gw", stderr, err)
					}
					if err := oc.addHybridRoutePolicyForPod(podIPNet.IP, node); err != nil {
						return err
					}
					if routeInfo.podExternalRoutes[podIP] == nil {
						routeInfo.podExternalRoutes[podIP] = make(map[string]string)
					}
					routeInfo.podExternalRoutes[podIP][gwStr] = gr
					routesAdded++
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

// deletePerPodGRSNAT removes per pod SNAT rules that are applied to the GR where the pod resides if
// there are no gateways
func (oc *Controller) deletePerPodGRSNAT(node string, podIPNets []*net.IPNet) {
	gr := util.GetGatewayRouterFromNode(node)
	for _, podIPNet := range podIPNets {
		podIP := podIPNet.IP.String()
		stdout, stderr, err := util.RunOVNNbctl("--if-exists", "lr-nat-del",
			gr, "snat", podIP)
		if err != nil {
			klog.Errorf("Failed to delete SNAT rule for pod on gateway router %s, "+
				"stdout: %q, stderr: %q, error: %v", gr, stdout, stderr, err)
		}
	}
}

func (oc *Controller) addPerPodGRSNAT(pod *kapi.Pod, podIfAddrs []*net.IPNet) error {
	nodeName := pod.Spec.NodeName
	node, err := oc.watchFactory.GetNode(nodeName)
	if err != nil {
		return fmt.Errorf("failed to get node %s: %v", nodeName, err)
	}
	l3GWConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return fmt.Errorf("unable to parse node L3 gw annotation: %v", err)
	}
	gr := types.GWRouterPrefix + nodeName
	for _, gwIPNet := range l3GWConfig.IPAddresses {
		gwIP := gwIPNet.IP.String()
		for _, podIPNet := range podIfAddrs {
			podIP := podIPNet.IP.String()
			if utilnet.IsIPv6String(gwIP) != utilnet.IsIPv6String(podIP) {
				continue
			}
			mask := GetIPFullMask(podIP)
			_, fullMaskPodNet, err := net.ParseCIDR(podIP + mask)
			if err != nil {
				return fmt.Errorf("invalid IP: %s and mask: %s combination, error: %v", podIP, mask, err)
			}
			if err := util.UpdateRouterSNAT(gr, gwIPNet.IP, fullMaskPodNet); err != nil {
				return fmt.Errorf("failed to update NAT for pod: %s, error: %v", pod.Name, err)
			}
		}
	}
	return nil
}

// addHybridRoutePolicyForPod handles adding a higher priority allow policy to allow traffic to be routed normally
// by ecmp routes
func (oc *Controller) addHybridRoutePolicyForPod(podIP net.IP, node string) error {
	if config.Gateway.Mode == config.GatewayModeLocal {
		// add allow policy to bypass lr-policy in GR
		var l3Prefix string
		isIPv6 := utilnet.IsIPv6(podIP)
		if isIPv6 {
			l3Prefix = "ip6"
		} else {
			l3Prefix = "ip4"
		}
		// get the GR to join switch ip address
		grJoinIfAddrs, err := util.GetLRPAddrs(types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + node)
		if err != nil {
			return fmt.Errorf("unable to find IP address for node: %s, %s port, err: %v", node, types.GWRouterToJoinSwitchPrefix, err)
		}
		grJoinIfAddr, err := util.MatchIPNetFamily(utilnet.IsIPv6(podIP), grJoinIfAddrs)
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
		matchStr := fmt.Sprintf(`inport == "%s%s" && %s.src == %s`, types.RouterToSwitchPrefix, node, l3Prefix, podIP)
		matchStr += matchDst
		_, stderr, err := util.RunOVNNbctl("lr-policy-add", types.OVNClusterRouter, types.HybridOverlayReroutePriority, matchStr, "reroute",
			grJoinIfAddr.IP.String())
		if err != nil {
			// TODO: lr-policy-add doesn't support --may-exist, resort to this workaround for now.
			// Have raised an issue against ovn repository (https://github.com/ovn-org/ovn/issues/49)
			if !strings.Contains(stderr, "already existed") {
				return fmt.Errorf("failed to add policy route '%s' to %s "+
					"stderr: %s, error: %v", matchStr, types.OVNClusterRouter, stderr, err)
			}
		}
	}
	return nil
}

// delHybridRoutePolicyForPod handles deleting a higher priority allow policy to allow traffic to be routed normally
// by ecmp routes
// force is used to indicate to force remove LRPs, useful for when going from LGW->SGW
func (oc *Controller) delHybridRoutePolicyForPod(podIP net.IP, node string, force bool) error {
	if config.Gateway.Mode == config.GatewayModeLocal || force {
		// delete allow policy to bypass lr-policy in GR
		var l3Prefix string
		if utilnet.IsIPv6(podIP) {
			l3Prefix = "ip6"
		} else {
			l3Prefix = "ip4"
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
			matchDst += fmt.Sprintf(" && %s.dst != %s", l3Prefix, clusterSubnet.CIDR)
		}
		matchStr := fmt.Sprintf(`inport == "%s%s" && %s.src == %s`, types.RouterToSwitchPrefix, node, l3Prefix, podIP)
		matchStr += matchDst
		_, stderr, err := util.RunOVNNbctl("lr-policy-del", types.OVNClusterRouter, types.HybridOverlayReroutePriority, matchStr)
		if err != nil {
			klog.Errorf("Failed to remove policy: %s, on: %s, stderr: %s, err: %v",
				matchStr, types.OVNClusterRouter, stderr, err)
		}
	}
	return nil
}

// cleanUpBFDEntry checks if the BFD table entry related to the associated
// gw router / port / gateway ip is referenced by other routing rules, and if
// not removes the entry to avoid having dangling BFD entries.
// This is temporary and can be safely removed when we consume an ovn version
// that includes http://patchwork.ozlabs.org/project/ovn/patch/3c39dc96a36a3445cfa8485a67de79f9f3d5651b.1614602770.git.lorenzo.bianconi@redhat.com/
func cleanUpBFDEntry(gatewayIP, gatewayRouter, prefix string) {
	portName := prefix + types.GWRouterToExtSwitchPrefix + gatewayRouter

	output, stderr, err := util.RunOVNNbctl(
		"--format=csv", "--data=bare", "--no-heading", "--columns=bfd", "find", "Logical_Router_Static_Route", "output_port="+portName, "nexthop=\""+gatewayIP+"\"", "bfd!=[]")

	if err != nil {
		klog.Errorf("cleanUpBFDEntry: failed to list routes for %s, stderr: %q, (%v)", portName, gatewayIP, err, stderr)
		return
	}
	// the bfd entry is still referenced, meaning there's another route on the router
	// referencing it.
	if strings.TrimSpace(output) != "" {
		return
	}
	uuids, stderr, err := util.RunOVNNbctl(
		"--format=csv", "--data=bare", "--no-heading", "--columns=_uuid", "find", "BFD", "logical_port="+portName, "dst_ip=\""+gatewayIP+"\"")
	if err != nil {
		klog.Errorf("Failed to list routes for %s, stderr: %q, (%v)", gatewayRouter, err, stderr)
		return
	}

	if strings.TrimSpace(uuids) == "" {
		klog.Infof("Did not find bfd entry for %s %s", portName, gatewayIP)
		return
	}

	for _, uuid := range strings.Split(uuids, "\n") {
		_, stderr, err = util.RunOVNNbctl("--if-exists", "destroy", "BFD", uuid)
		if err != nil {
			klog.Errorf("Failed to destroy BFD %s, stderr: %q, (%v)",
				uuid, stderr, err)
		}
	}
}

// extSwitchPrefix returns the prefix of the external switch to use for
// external gateway routes. In case no second bridge is configured, we
// use the default one and the prefix is empty.
func (oc *Controller) extSwitchPrefix(nodeName string) (string, error) {
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

func (oc *Controller) cleanExGwECMPRoutes() {
	start := time.Now()
	defer func() {
		klog.Infof("Syncing exgw routes took %v", time.Since(start))
	}()

	// Get all ECMP routes in OVN and build cache
	ovnRouteCache := buildOVNECMPCache()

	if len(ovnRouteCache) == 0 {
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
				_, stderr, err := util.RunOVNNbctl("--if-exists", "remove", "Logical_Router",
					strings.TrimSuffix(ovnRoute.router, "\n"), "static_routes", ovnRoute.uuid)
				if err != nil {
					klog.Errorf("Failed to destroy Logical_Router_Static_Route %s, stderr: %q, (%v)",
						ovnRoute.uuid, stderr, err)
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
					cleanUpBFDEntry(ovnRoute.nextHop, ovnRoute.router, prefix)
				}

			} else {
				podHasAnyECMPRoutes = true
			}
		}

		// if pod had no ECMP routes we need to make sure we remove any logical route policy for local gw mode
		// for shared gateway mode, these LRPs shouldn't exist, so delete them all
		if !podHasAnyECMPRoutes || config.Gateway.Mode == config.GatewayModeShared {
			for _, ovnRoute := range ovnRoutes {
				gr := strings.TrimPrefix(ovnRoute.router, types.GWRouterPrefix)
				if err := oc.delHybridRoutePolicyForPod(net.ParseIP(podIP), gr, true); err != nil {
					klog.Errorf("Error while removing hybrid policy for pod IP: %s, on node: %s, error: %v",
						podIP, gr, err)
				}
			}
		}
	}
}

func getExGwPodIPs(gatewayPod *kapi.Pod) ([]net.IP, error) {
	var foundGws []net.IP
	if gatewayPod.Annotations[routingNetworkAnnotation] != "" {
		var multusNetworks []nettypes.NetworkStatus
		err := json.Unmarshal([]byte(gatewayPod.ObjectMeta.Annotations[nettypes.NetworkStatusAnnot]), &multusNetworks)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshall annotation k8s.v1.cni.cncf.io/network-status on pod %s: %v",
				gatewayPod.Name, err)
		}
		for _, multusNetwork := range multusNetworks {
			if multusNetwork.Name == gatewayPod.Annotations[routingNetworkAnnotation] {
				for _, gwIP := range multusNetwork.IPs {
					ip := net.ParseIP(gwIP)
					if ip != nil {
						foundGws = append(foundGws, ip)
					}
				}
			}
		}
	} else if gatewayPod.Spec.HostNetwork {
		for _, podIP := range gatewayPod.Status.PodIPs {
			ip := net.ParseIP(podIP.IP)
			if ip != nil {
				foundGws = append(foundGws, ip)
			}
		}
	} else {
		klog.Errorf("Ignoring pod %s as an external gateway candidate. Invalid combination "+
			"of host network: %t and routing-network annotation: %s", gatewayPod.Name, gatewayPod.Spec.HostNetwork,
			gatewayPod.Annotations[routingNetworkAnnotation])
		return nil, nil
	}
	return foundGws, nil
}

func (oc *Controller) buildClusterECMPCacheFromNamespaces(clusterRouteCache map[string][]string) {
	namespaces, err := oc.watchFactory.GetNamespaces()
	if err != nil {
		klog.Errorf("Error getting all namespaces for exgw ecmp route sync: %v", err)
		return
	}
	for _, namespace := range namespaces {
		if _, ok := namespace.Annotations[routingExternalGWsAnnotation]; !ok {
			continue
		}
		// namespace has exgw routes, build cache
		gwIPs, err := parseRoutingExternalGWAnnotation(namespace.Annotations[routingExternalGWsAnnotation])
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
		for _, gwIP := range gwIPs {
			for _, nsPod := range nsPods {
				for _, podIP := range nsPod.Status.PodIPs {
					if utilnet.IsIPv6(gwIP) != utilnet.IsIPv6String(podIP.IP) {
						continue
					}
					if val, ok := clusterRouteCache[podIP.IP]; ok {
						// add gwIP to cache only if buildClusterECMPCacheFromPods hasn't already added it
						gwIPexists := false
						for _, existingGwIP := range val {
							if existingGwIP == gwIP.String() {
								gwIPexists = true
								break
							}
						}
						if !gwIPexists {
							clusterRouteCache[podIP.IP] = append(clusterRouteCache[podIP.IP], gwIP.String())
						}
					} else {
						clusterRouteCache[podIP.IP] = []string{gwIP.String()}
					}
				}
			}
		}
	}
}

func (oc *Controller) buildClusterECMPCacheFromPods(clusterRouteCache map[string][]string) {
	// Get all Pods serving as exgws
	pods, err := oc.watchFactory.GetAllPods()
	if err != nil {
		klog.Error("Error getting all pods for exgw ecmp route sync: %v", err)
		return
	}
	for _, pod := range pods {
		podRoutingNamespaceAnno := pod.Annotations[routingNamespaceAnnotation]
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
		for _, gwIP := range gwIPs {
			for _, nsPod := range nsPods {
				for _, podIP := range nsPod.Status.PodIPs {
					if utilnet.IsIPv6(gwIP) != utilnet.IsIPv6String(podIP.IP) {
						continue
					}
					clusterRouteCache[podIP.IP] = append(clusterRouteCache[podIP.IP], gwIP.String())
				}
			}
		}
	}
}

func buildOVNECMPCache() map[string][]*ovnRoute {
	ovnRouteCache := make(map[string][]*ovnRoute)
	out, stderr, err := util.RunOVNNbctl(
		"--format=csv", "--data=bare", "--no-heading", "--columns=_uuid,ip_prefix,nexthop,output_port", "find", "Logical_Router_Static_Route", "options={ecmp_symmetric_reply=\"true\"}")
	if err != nil {
		klog.Errorf("CleanECMPRoutes: failed to list ecmp routes %v %s", err, stderr)
		return nil
	}
	if strings.TrimSpace(out) == "" {
		klog.Infof("Did not find ecmp routes to clean")
		return nil
	}

	for _, line := range strings.Split(out, "\n") {
		values := strings.Split(line, ",")
		uuid := values[0]
		podIP := values[1]
		nexthop := values[2]
		outport := values[3]
		gr, stderr, err := util.RunOVNNbctl(
			"--format=csv", "--data=bare", "--no-heading", "--columns=name", "find", "Logical_Router", fmt.Sprintf("static_routes{>=}[%s]", uuid))
		if err != nil || gr == "" {
			klog.Errorf("CleanECMPRoutes: failed to find logical router for %s", uuid, err, stderr)
			continue
		}
		route := &ovnRoute{
			nextHop: nexthop,
			uuid:    uuid,
			router:  gr,
			outport: outport,
		}
		if _, ok := ovnRouteCache[podIP]; !ok {
			ovnRouteCache[podIP] = []*ovnRoute{route}
		} else {
			ovnRouteCache[podIP] = append(ovnRouteCache[podIP], route)
		}
	}
	return ovnRouteCache
}

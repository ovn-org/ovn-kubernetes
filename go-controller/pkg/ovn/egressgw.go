package ovn

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	apbroutecontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type gatewayInfo struct {
	gws        sets.Set[string]
	bfdEnabled bool
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
	if config.OVNKubernetesFeature.EnableInterconnect && oc.zone != types.OvnDefaultZone {
		existingGWs.Insert(nsInfo.routingExternalGWs.gws.UnsortedList()...)
	}
	nsUnlock()

	klog.Infof("Adding routes for external gateway pod: %s, next hops: %q, namespace: %s, bfd-enabled: %t",
		pod.Name, strings.Join(egress.gws.UnsortedList(), ","), namespace, egress.bfdEnabled)
	err = oc.addGWRoutesForNamespace(namespace, egress)
	if err != nil {
		return err
	}
	// add the exgw podIP to the namespace's k8s.ovn.org/external-gw-pod-ips list
	if !config.OVNKubernetesFeature.EnableInterconnect || oc.zone == types.OvnDefaultZone {
		// If interconnect is disabled OR interconnect is running in single-zone-mode,
		// the ovnkube-master is responsible for patching ICNI managed namespaces with
		// "k8s.ovn.org/external-gw-pod-ips". In that case, we need ovnkube-node to flush
		// conntrack on every node. In multi-zone-interconnect case, we will handle the flushing
		// directly on the ovnkube-controller code to avoid an extra namespace annotation
		if err := util.UpdateExternalGatewayPodIPsAnnotation(oc.kube, namespace, existingGWs.List()); err != nil {
			klog.Errorf("Unable to update %s/%v annotation for namespace %s: %v", util.ExternalGatewayPodIPsAnnotation, existingGWs, namespace, err)
		}
	} else {
		// flush here since we know we have added an egressgw pod and we also know the full list of existing gatewayIPs
		gatewayIPs, err := oc.apbExternalRouteController.GetAdminPolicyBasedExternalRouteIPsForTargetNamespace(namespace)
		if err != nil {
			return fmt.Errorf("unable to retrieve gateway IPs for Admin Policy Based External Route objects: %w", err)
		}
		gatewayIPs = gatewayIPs.Insert(existingGWs.List()...)
		err = oc.syncConntrackForExternalGateways(namespace, gatewayIPs) // best effort
		if err != nil {
			klog.Errorf("Syncing conntrack entries for egressGW pod %v serving the namespace %s failed: %v",
				egress, namespace, err)
		}
	}
	return nil
}

func (oc *DefaultNetworkController) syncConntrackForExternalGateways(namespace string, gwIPsToKeep sets.Set[string]) error {
	return util.SyncConntrackForExternalGateways(gwIPsToKeep, oc.isPodInLocalZone, func() ([]*kapi.Pod, error) {
		return oc.watchFactory.GetPods(namespace)
	})
}

func (oc *DefaultNetworkController) checkAndDeleteStaleConntrackEntries() {
	namespaces, err := oc.watchFactory.GetNamespaces()
	if err != nil {
		klog.Errorf("Unable to get pods from informer: %v", err)
		return
	}
	for _, namespace := range namespaces {
		// flush here since we know we have added an egressgw pod and we also know the full list of existing gatewayIPs
		existingGWs, err := oc.apbExternalRouteController.GetAdminPolicyBasedExternalRouteIPsForTargetNamespace(namespace.Name)
		if err != nil {
			klog.Errorf("Unable to retrieve gateway IPs for Admin Policy Based External Route objects for ns %s: %v", namespace.Name, err)
			return
		}
		// by now the nsInfo cache must be repaired for this feature fully;
		// however this introduces cache lock scale concern by doing this every minute
		// versus previously this was done purely using annotations
		nsInfo, nsUnlock, err := oc.ensureNamespaceLocked(namespace.Name, false, nil)
		if err != nil {
			klog.Errorf("Failed to ensure namespace %s locked: %v", namespace, err)
			return
		}
		for _, gwInfo := range nsInfo.routingExternalPodGWs {
			existingGWs.Insert(gwInfo.gws.UnsortedList()...)
		}
		existingGWs.Insert(nsInfo.routingExternalGWs.gws.UnsortedList()...)
		nsUnlock()
		if len(existingGWs) > 0 {
			pods, err := oc.watchFactory.GetPods(namespace.Name)
			if err != nil {
				klog.Warningf("Unable to get pods from informer for namespace %s: %v", namespace.Name, err)
			}
			if len(pods) > 0 || err != nil {
				// we only need to proceed if there is at least one pod in this namespace on this node
				// OR if we couldn't fetch the pods for some reason at this juncture
				err = oc.syncConntrackForExternalGateways(namespace.Name, existingGWs)
				if err != nil {
					klog.Errorf("Syncing conntrack entries for egressGWs %+v serving the namespace %s failed: %v",
						existingGWs, namespace.Name, err)
				}
			}
		}
	}
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

func (oc *DefaultNetworkController) isPodInLocalZone(pod *kapi.Pod) (bool, error) {
	node, err := oc.watchFactory.GetNode(pod.Spec.NodeName)
	if err != nil {
		return false, err
	}
	return oc.isLocalZoneNode(node), nil
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
			podIP := &net.IPNet{IP: utilnet.ParseIPSloppy(podIP.IP)}
			podIP.Mask = util.GetIPFullMask(podIP.IP)
			podIPs = append(podIPs, podIP)
		}
		if len(podIPs) == 0 {
			klog.Warningf("Will not add gateway routes pod %s/%s. IPs not found!", pod.Namespace, pod.Name)
			continue
		}
		if config.Gateway.DisableSNATMultipleGWs {
			// delete all perPodSNATs (if this pod was controlled by egressIP controller, it will stop working since
			// a pod cannot be used for multiple-external-gateways and egressIPs at the same time)
			if err = oc.deletePodSNAT(pod.Spec.NodeName, []*net.IPNet{}, podIPs); err != nil {
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
// this MUST be called with a lock on routeInfo
func (oc *DefaultNetworkController) deletePodGWRoute(routeInfo *apbroutecontroller.RouteInfo, podIP, gw, gr string) error {
	if utilnet.IsIPv6String(gw) != utilnet.IsIPv6String(podIP) {
		return nil
	}
	pod, err := oc.watchFactory.PodCoreInformer().Lister().Pods(routeInfo.PodName.Namespace).Get(routeInfo.PodName.Name)
	if err == nil {
		local, err := oc.isPodInLocalZone(pod)
		if err != nil {
			return err
		}
		if !local {
			klog.V(4).Infof("Not deleting exgw routes for pod %s not in the local zone %s", routeInfo.PodName, oc.zone)
			return nil
		}
	}
	mask := util.GetIPFullMaskString(podIP)
	if err := oc.deleteLogicalRouterStaticRoute(podIP, mask, gw, gr); err != nil {
		return fmt.Errorf("unable to delete pod %s ECMP route to GR %s, GW: %s: %w",
			routeInfo.PodName, gr, gw, err)
	}

	klog.V(5).Infof("ECMP route deleted for pod: %s, on gr: %s, to gw: %s",
		routeInfo.PodName, gr, gw)

	node := util.GetWorkerFromGatewayRouter(gr)

	// The gw is deleted from the routes cache after this func is called, length 1
	// means it is the last gw for the pod and the hybrid route policy should be deleted.
	if entry := routeInfo.PodExternalRoutes[podIP]; len(entry) <= 1 {
		if err := oc.delHybridRoutePolicyForPod(net.ParseIP(podIP), node); err != nil {
			return fmt.Errorf("unable to delete hybrid route policy for pod %s: err: %v", routeInfo.PodName, err)
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
	existingGWs := sets.New[string]()
	for _, gwInfo := range nsInfo.routingExternalPodGWs {
		existingGWs.Insert(gwInfo.gws.UnsortedList()...)
	}
	if config.OVNKubernetesFeature.EnableInterconnect && oc.zone != types.OvnDefaultZone {
		existingGWs.Insert(nsInfo.routingExternalGWs.gws.UnsortedList()...)
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
	if !config.OVNKubernetesFeature.EnableInterconnect || oc.zone == types.OvnDefaultZone {
		// If interconnect is disabled OR interconnect is running in single-zone-mode,
		// the ovnkube-master is responsible for patching ICNI managed namespaces with
		// "k8s.ovn.org/external-gw-pod-ips". In that case, we need ovnkube-node to flush
		// conntrack on every node. In multi-zone-interconnect case, we will handle the flushing
		// directly on the ovnkube-controller code to avoid an extra namespace annotation
		if err := util.UpdateExternalGatewayPodIPsAnnotation(oc.kube, namespace, sets.List(existingGWs)); err != nil {
			klog.Errorf("Unable to update %s/%v annotation for namespace %s: %v", util.ExternalGatewayPodIPsAnnotation, existingGWs, namespace, err)
		}
	} else {
		// flush here since we know we have deleted an egressgw pod and we also know the full list of existing gatewayIPs
		gatewayIPs, err := oc.apbExternalRouteController.GetAdminPolicyBasedExternalRouteIPsForTargetNamespace(namespace)
		if err != nil {
			return fmt.Errorf("unable to retrieve gateway IPs for Admin Policy Based External Route objects: %w", err)
		}
		gatewayIPs = gatewayIPs.Insert(sets.List(existingGWs)...)
		err = oc.syncConntrackForExternalGateways(namespace, gatewayIPs) // best effort
		if err != nil {
			klog.Errorf("Syncing conntrack entries for egressGWs %+v serving the namespace %s failed: %v",
				gatewayIPs, namespace, err)
		}
	}
	return nil
}

// deleteGwRoutesForNamespace handles deleting routes to gateways for a pod on a specific GR.
// If a set of gateways is given, only routes for that gateway are deleted. If no gateways
// are given, all routes for the namespace are deleted.
func (oc *DefaultNetworkController) deleteGWRoutesForNamespace(namespace string, matchGWs sets.Set[string]) error {
	deleteAll := (matchGWs == nil || matchGWs.Len() == 0)

	policyGWIPs, err := oc.apbExternalRouteController.GetDynamicGatewayIPsForTargetNamespace(namespace)
	if err != nil {
		return err
	}
	policyStaticGWIPs, err := oc.apbExternalRouteController.GetStaticGatewayIPsForTargetNamespace(namespace)
	if err != nil {
		return err
	}
	policyGWIPs = policyGWIPs.Union(policyStaticGWIPs)
	return oc.externalGatewayRouteInfo.CleanupNamespace(namespace, func(routeInfo *apbroutecontroller.RouteInfo) error {
		for podIP, routes := range routeInfo.PodExternalRoutes {
			for gw, gr := range routes {
				if (deleteAll || matchGWs.Has(gw)) && !policyGWIPs.Has(gw) {
					if err := oc.deletePodGWRoute(routeInfo, podIP, gw, gr); err != nil {
						// if we encounter error while deleting routes for one pod; we return and don't try subsequent pods
						return fmt.Errorf("delete pod GW route failed: %w", err)
					}
					delete(routes, gw)
				}
			}
		}
		return nil
	})
}

// deleteGwRoutesForPod handles deleting all routes to gateways for a pod IP on a specific GR
func (oc *DefaultNetworkController) deleteGWRoutesForPod(name ktypes.NamespacedName, podIPNets []*net.IPNet) (err error) {
	return oc.externalGatewayRouteInfo.Cleanup(name, func(routeInfo *apbroutecontroller.RouteInfo) error {
		policyGWIPs, err := oc.apbExternalRouteController.GetDynamicGatewayIPsForTargetNamespace(name.Namespace)
		if err != nil {
			return err
		}
		policyStaticGWIPs, err := oc.apbExternalRouteController.GetStaticGatewayIPsForTargetNamespace(name.Namespace)
		if err != nil {
			return err
		}
		policyGWIPs = policyGWIPs.Union(policyStaticGWIPs)

		for _, podIPNet := range podIPNets {
			podIP := podIPNet.IP.String()
			routes, ok := routeInfo.PodExternalRoutes[podIP]
			if !ok {
				continue
			}
			if len(routes) == 0 {
				delete(routeInfo.PodExternalRoutes, podIP)
				continue
			}
			for gw, gr := range routes {
				if !policyGWIPs.Has(gw) {
					if err := oc.deletePodGWRoute(routeInfo, podIP, gw, gr); err != nil {
						// if we encounter error while deleting routes for one pod; we return and don't try subsequent pods
						return fmt.Errorf("delete pod GW route failed: %w", err)
					}
					delete(routes, gw)
				}
			}
		}
		return nil
	})
}

// addEgressGwRoutesForPod handles adding all routes to gateways for a pod on a specific GR
func (oc *DefaultNetworkController) addGWRoutesForPod(gateways []*gatewayInfo, podIfAddrs []*net.IPNet, podNsName ktypes.NamespacedName, node string) error {
	pod, err := oc.watchFactory.PodCoreInformer().Lister().Pods(podNsName.Namespace).Get(podNsName.Name)
	if err != nil {
		return err
	}

	local, err := oc.isPodInLocalZone(pod)
	if err != nil {
		return err
	}
	if !local {
		klog.V(4).Infof("Not adding exgw routes for pod %s not in the local zone %s", podNsName, oc.zone)
		return nil
	}

	gr := oc.GetNetworkScopedGWRouterName(node)

	routesAdded := 0
	portPrefix, err := oc.extSwitchPrefix(node)
	if err != nil {
		klog.Infof("Failed to find ext switch prefix for %s %v", node, err)
		return err
	}

	port := portPrefix + types.GWRouterToExtSwitchPrefix + gr

	return oc.externalGatewayRouteInfo.CreateOrLoad(podNsName, func(routeInfo *apbroutecontroller.RouteInfo) error {
		policyGWIPs, err := oc.apbExternalRouteController.GetDynamicGatewayIPsForTargetNamespace(podNsName.Namespace)
		if err != nil {
			return err
		}
		policyStaticGWIPs, err := oc.apbExternalRouteController.GetStaticGatewayIPsForTargetNamespace(podNsName.Namespace)
		if err != nil {
			return err
		}
		policyGWIPs = policyGWIPs.Union(policyStaticGWIPs)

		for _, podIPNet := range podIfAddrs {
			for _, gateway := range gateways {
				// TODO (trozet): use the go bindings here and batch commands
				// validate the ip and gateway belong to the same address family
				gws, err := util.MatchAllIPStringFamily(utilnet.IsIPv6(podIPNet.IP), gateway.gws.UnsortedList())
				if err == nil {
					podIP := podIPNet.IP.String()
					for _, gw := range gws {
						// if route was already programmed, skip it
						foundGR, ok := routeInfo.PodExternalRoutes[podIP][gw]
						if (ok && foundGR == gr) || policyGWIPs.Has(gw) {
							routesAdded++
							continue
						}
						mask := util.GetIPFullMaskString(podIP)

						if err := oc.createBFDStaticRoute(gateway.bfdEnabled, gw, podIP, gr, port, mask); err != nil {
							return err
						}
						if routeInfo.PodExternalRoutes[podIP] == nil {
							routeInfo.PodExternalRoutes[podIP] = make(map[string]string)
						}
						routeInfo.PodExternalRoutes[podIP][gw] = gr
						routesAdded++
						if len(routeInfo.PodExternalRoutes[podIP]) == 1 {
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
	})
}

// deletePodSNAT removes per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// used when disableSNATMultipleGWs=true
func (oc *DefaultNetworkController) deletePodSNAT(nodeName string, extIPs, podIPNets []*net.IPNet) error {

	node, err := oc.watchFactory.NodeCoreInformer().Lister().Get(nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If node does not exist, there is nothing to delete
			return nil
		}
		return err
	}
	if !oc.isLocalZoneNode(node) {
		klog.V(4).Infof("Node %s is not in the local zone %s", nodeName, oc.zone)
		return nil
	}
	// Default network does not set any matches in Pod SNAT
	ops, err := deletePodSNATOps(oc.nbClient, nil, oc.GetNetworkScopedGWRouterName(nodeName), extIPs, podIPNets, "")
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to delete SNAT rule for pod on gateway router %s: %w", oc.GetNetworkScopedGWRouterName(nodeName), err)
	}
	return nil
}

// buildPodSNAT builds per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
func buildPodSNAT(extIPs, podIPNets []*net.IPNet, match string) ([]*nbdb.NAT, error) {
	nats := make([]*nbdb.NAT, 0, len(extIPs)*len(podIPNets))
	for _, podIPNet := range podIPNets {
		fullMaskPodNet := &net.IPNet{
			IP:   podIPNet.IP,
			Mask: util.GetIPFullMask(podIPNet.IP),
		}
		if len(extIPs) == 0 {
			nats = append(nats, libovsdbops.BuildSNATWithMatch(nil, fullMaskPodNet, "", nil, match))
		} else {
			for _, gwIPNet := range extIPs {
				if utilnet.IsIPv6CIDR(gwIPNet) != utilnet.IsIPv6CIDR(podIPNet) {
					continue
				}
				nats = append(nats, libovsdbops.BuildSNATWithMatch(&gwIPNet.IP, fullMaskPodNet, "", nil, match))
			}
		}
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

// deletePodSNATOps creates ovsdb operation that removes per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// used when disableSNATMultipleGWs=true
func deletePodSNATOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, gwRouterName string, extIPs, podIPNets []*net.IPNet, match string) ([]ovsdb.Operation, error) {
	nats, err := buildPodSNAT(extIPs, podIPNets, match)
	if err != nil {
		return nil, err
	}
	logicalRouter := nbdb.LogicalRouter{
		Name: gwRouterName,
	}
	ops, err = libovsdbops.DeleteNATsOps(nbClient, ops, &logicalRouter, nats...)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil, fmt.Errorf("failed create operation for deleting SNAT rule for pod on gateway router %s: %v", logicalRouter.Name, err)
	}
	return ops, nil
}

// addOrUpdatePodSNAT adds or updates per pod SNAT rules towards the nodeIP that are applied to the GR where the pod resides
// used when disableSNATMultipleGWs=true
func addOrUpdatePodSNAT(nbClient libovsdbclient.Client, gwRouterName string, extIPs, podIfAddrs []*net.IPNet) error {
	nats, err := buildPodSNAT(extIPs, podIfAddrs, "")
	if err != nil {
		return err
	}
	logicalRouter := nbdb.LogicalRouter{
		Name: gwRouterName,
	}
	if err := libovsdbops.CreateOrUpdateNATs(nbClient, &logicalRouter, nats...); err != nil {
		return fmt.Errorf("failed to update SNAT for pods of router %s: %v", logicalRouter.Name, err)
	}
	return nil
}

// addOrUpdatePodSNATOps returns the operation that adds or updates per pod SNAT rules towards the nodeIP that are
// applied to the GR where the pod resides
// used when disableSNATMultipleGWs=true
func addOrUpdatePodSNATOps(nbClient libovsdbclient.Client, gwRouterName string, extIPs, podIfAddrs []*net.IPNet, match string, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {
	router := &nbdb.LogicalRouter{Name: gwRouterName}
	nats, err := buildPodSNAT(extIPs, podIfAddrs, match)
	if err != nil {
		return nil, err
	}
	if ops, err = libovsdbops.CreateOrUpdateNATsOps(nbClient, ops, router, nats...); err != nil {
		return nil, fmt.Errorf("failed to update SNAT for pods of router: %s, error: %v", gwRouterName, err)
	}
	return ops, nil
}

// addHybridRoutePolicyForPod handles adding a higher priority allow policy to allow traffic to be routed normally
// by ecmp routes.
// WARNING: updates same db entries as apbroutecontroller. Make sure to call only when route is not managed by
// apbroute controller.
func (oc *DefaultNetworkController) addHybridRoutePolicyForPod(podIP net.IP, node string) error {
	if config.Gateway.Mode == config.GatewayModeLocal {
		// Add podIP to the node's address_set.
		asIndex := apbroutecontroller.GetHybridRouteAddrSetDbIDs(node, oc.controllerName)
		as, err := oc.addressSetFactory.EnsureAddressSet(asIndex)
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
		grJoinIfAddrs, err := libovsdbutil.GetLRPAddrs(oc.nbClient, types.GWRouterToJoinSwitchPrefix+oc.GetNetworkScopedGWRouterName(node))
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
		err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(oc.nbClient, oc.GetNetworkScopedClusterRouterName(),
			&logicalRouterPolicy, p, &logicalRouterPolicy.Nexthops, &logicalRouterPolicy.Match, &logicalRouterPolicy.Action)
		if err != nil {
			return fmt.Errorf("failed to add policy route %+v to %s: %v", logicalRouterPolicy, oc.GetNetworkScopedClusterRouterName(), err)
		}
	}
	return nil
}

// delHybridRoutePolicyForPod handles deleting a logical route policy that
// forces pod egress traffic to be rerouted to a gateway router for local gateway mode.
// WARNING: updates same db entries as apbroutecontroller. Make sure to call only when route is not managed by
// apbroute controller.
func (oc *DefaultNetworkController) delHybridRoutePolicyForPod(podIP net.IP, node string) error {
	if config.Gateway.Mode == config.GatewayModeLocal {
		// Delete podIP from the node's address_set.
		asIndex := apbroutecontroller.GetHybridRouteAddrSetDbIDs(node, oc.controllerName)
		as, err := oc.addressSetFactory.EnsureAddressSet(asIndex)
		if err != nil {
			return fmt.Errorf("cannot Ensure that addressSet for node %s exists %v", node, err)
		}
		err = as.DeleteAddresses([]string{podIP.String()})
		if err != nil {
			return fmt.Errorf("unable to remove PodIP %s: to the address set %s, err: %v", podIP, node, err)
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
			err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, oc.GetNetworkScopedClusterRouterName(), p)
			if err != nil {
				return fmt.Errorf("error deleting policy %s on router %s: %v", matchStr, oc.GetNetworkScopedClusterRouterName(), err)
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
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, oc.GetNetworkScopedClusterRouterName(), policyPred)
	if err != nil {
		return fmt.Errorf("error deleting hybrid route policies on %s: %v", oc.GetNetworkScopedClusterRouterName(), err)
	}

	// nuke all the address-sets.
	// if we fail to remove LRP's above, we don't attempt to remove ASes due to dependency constraints.
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetHybridNodeRoute, oc.controllerName, nil)
	asPred := libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil)
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
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, oc.GetNetworkScopedClusterRouterName(), p)
	if err != nil {
		return fmt.Errorf("error deleting legacy hybrid route policies on %s: %v", oc.GetNetworkScopedClusterRouterName(), err)
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
		return "", fmt.Errorf("extSwitchPrefix failed to find node %s: %w", nodeName, err)
	}
	l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return "", fmt.Errorf("extSwitchPrefix failed to parse l3 gateway annotation for node %s: %w", nodeName, err)
	}

	if l3GatewayConfig.EgressGWInterfaceID != "" {
		return types.EgressGWSwitchPrefix, nil
	}
	return "", nil
}

func getExGwPodIPs(gatewayPod *kapi.Pod) (sets.Set[string], error) {
	foundGws := sets.New[string]()
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

func makePodGWKey(pod *kapi.Pod) string {
	return fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
}

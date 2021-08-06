package ovn

import (
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"

	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	DuplicateECMPError = "duplicate nexthop for the same ECMP route"
)

type gatewayInfo struct {
	gws        []net.IP
	bfdEnabled bool
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
	var foundGws []net.IP

	if pod.Annotations[routingNetworkAnnotation] != "" {
		var multusNetworks []nettypes.NetworkStatus
		err := json.Unmarshal([]byte(pod.ObjectMeta.Annotations[nettypes.NetworkStatusAnnot]), &multusNetworks)
		if err != nil {
			return fmt.Errorf("unable to unmarshall annotation k8s.v1.cni.cncf.io/network-status on pod %s: %v", pod.Name, err)
		}
		for _, multusNetwork := range multusNetworks {
			if multusNetwork.Name == pod.Annotations[routingNetworkAnnotation] {
				for _, gwIP := range multusNetwork.IPs {
					ip := net.ParseIP(gwIP)
					if ip != nil {
						foundGws = append(foundGws, ip)
					}
				}
			}
		}
	} else if pod.Spec.HostNetwork {
		for _, podIP := range pod.Status.PodIPs {
			ip := net.ParseIP(podIP.IP)
			if ip != nil {
				foundGws = append(foundGws, ip)
			}
		}
	} else {
		klog.Errorf("Ignoring pod %s as an external gateway candidate. Invalid combination "+
			"of host network: %t and routing-network annotation: %s", pod.Name, pod.Spec.HostNetwork,
			pod.Annotations[routingNetworkAnnotation])
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
func (oc *Controller) addPodExternalGWForNamespace(namespace string, pod *kapi.Pod, egress gatewayInfo) error {
	var gws string
	for _, ip := range egress.gws {
		if len(gws) != 0 {
			gws += ","
		}
		gws += ip.String()
	}
	klog.Infof("Adding routes for external gateway pod: %s, next hops: %q, namespace: %s, bfd-enabled: %t",
		pod.Name, gws, namespace, egress.bfdEnabled)
	nsInfo := oc.ensureNamespaceLocked(namespace)
	defer nsInfo.Unlock()
	nsInfo.routingExternalPodGWs[pod.Name] = egress
	return oc.addGWRoutesForNamespace(namespace, egress, nsInfo)
}

// addExternalGWsForNamespace handles adding annotated gw routes to all pods in namespace
// This should only be called with a lock on nsInfo
func (oc *Controller) addExternalGWsForNamespace(egress gatewayInfo, nsInfo *namespaceInfo, namespace string) error {
	if egress.gws == nil {
		return fmt.Errorf("unable to add gateways routes for namespace: %s, gateways are nil", namespace)
	}
	nsInfo.routingExternalGWs = egress
	return oc.addGWRoutesForNamespace(namespace, egress, nsInfo)
}

// addGWRoutesForNamespace handles adding routes for all existing pods in namespace
// This should only be called with a lock on nsInfo
func (oc *Controller) addGWRoutesForNamespace(namespace string, egress gatewayInfo, nsInfo *namespaceInfo) error {
	existingPods, err := oc.watchFactory.GetPods(namespace)
	if err != nil {
		return fmt.Errorf("failed to get all the pods (%v)", err)
	}
	// TODO (trozet): use the go bindings here and batch commands
	for _, pod := range existingPods {
		if config.Gateway.DisableSNATMultipleGWs {
			logicalPort := podLogicalPortName(pod)
			portInfo, err := oc.logicalPortCache.get(logicalPort)
			if err != nil {
				klog.Warningf("Unable to get port %s in cache for SNAT rule removal", logicalPort)
			} else {
				oc.deletePerPodGRSNAT(pod.Spec.NodeName, portInfo.ips)
			}
		}
		gr := util.GetGatewayRouterFromNode(pod.Spec.NodeName)
		prefix, err := oc.extSwitchPrefix(pod.Spec.NodeName)
		if err != nil {
			klog.Infof("Failed to find ext switch prefix for %s %v", pod.Spec.NodeName, err)
			continue
		}

		port := prefix + types.GWRouterToExtSwitchPrefix + gr
		for _, gw := range egress.gws {
			for _, podIP := range pod.Status.PodIPs {
				if utilnet.IsIPv6(gw) != utilnet.IsIPv6String(podIP.IP) {
					continue
				}

				// if route was already programmed, skip it
				if foundGR, ok := nsInfo.podExternalRoutes[podIP.IP][gw.String()]; ok && foundGR == gr {
					continue
				}

				mask := GetIPFullMask(podIP.IP)

				if err := oc.createBFDStaticRoute(egress.bfdEnabled, gw, podIP.IP, gr, port, mask); err != nil {
					return err
				}
				if err := oc.addHybridRoutePolicyForPod(net.ParseIP(podIP.IP), pod.Spec.NodeName); err != nil {
					return err
				}
				if nsInfo.podExternalRoutes[podIP.IP] == nil {
					nsInfo.podExternalRoutes[podIP.IP] = make(map[string]string)
				}
				nsInfo.podExternalRoutes[podIP.IP][gw.String()] = gr
			}
		}
	}
	return nil
}

func (oc *Controller) createBFDStaticRoute(bfdEnabled bool, gw net.IP, podIP, gr, port, mask string) error {
	opModels := []util.OperationModel{}

	bfdNamedUUID := util.GenerateNamedUUID()
	logicalRouterStaticRouteNamedUUID := util.GenerateNamedUUID()

	bfdRes := []nbdb.BFD{}
	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterStaticRouteRes := []nbdb.LogicalRouterStaticRoute{}

	logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
		UUID:   logicalRouterStaticRouteNamedUUID,
		Policy: []string{nbdb.LogicalRouterStaticRoutePolicySrcIP},
		Options: map[string]string{
			"ecmp_symmetric_reply": "true",
		},
		Nexthop:    gw.String(),
		IPPrefix:   podIP + mask,
		OutputPort: []string{port},
	}
	if bfdEnabled {
		bfd := nbdb.BFD{
			UUID:        bfdNamedUUID,
			DstIP:       gw.String(),
			LogicalPort: port,
		}
		logicalRouterStaticRoute.BFD = []string{bfdNamedUUID}
		opModels = append(opModels, util.OperationModel{
			Model: &bfd,
			ModelPredicate: func(bfd *nbdb.BFD) bool {
				return bfd.DstIP == gw.String() && bfd.LogicalPort == port
			},
			ExistingResult: &bfdRes,
		})
	}
	opModels = append(opModels, util.OperationModel{
		Model: &logicalRouterStaticRoute,
		ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
			return lrsr.IPPrefix == podIP+mask &&
				lrsr.Nexthop == gw.String() &&
				reflect.DeepEqual(lrsr.OutputPort, []string{port})
		},
		ExistingResult: &logicalRouterStaticRouteRes,
	})
	opModels = append(opModels, util.OperationModel{
		Model: &logicalRouter,
		ModelPredicate: func(lr *nbdb.LogicalRouter) bool {
			return lr.Name == gr
		},
		OnModelMutations: func() []model.Mutation {
			if len(logicalRouterStaticRouteRes) > 0 {
				return nil
			}
			return []model.Mutation{
				{
					Field:   &logicalRouter.StaticRoutes,
					Mutator: ovsdb.MutateOperationInsert,
					Value:   []string{logicalRouterStaticRouteNamedUUID},
				},
			}
		},
		ExistingResult: &[]nbdb.LogicalRouter{},
	})
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("unable to add src-ip route to GR router, err: %v", err)
	}
	return nil
}

func (oc *Controller) deleteLogicalRouterStaticRoute(podIP, mask, gw, gr string) error {
	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{}
	logicalRouterStaticRouteRes := []nbdb.LogicalRouterStaticRoute{}
	opModels := []util.OperationModel{
		{
			Model: &logicalRouterStaticRoute,
			ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
				return reflect.DeepEqual(lrsr.Policy, []string{nbdb.LogicalRouterStaticRoutePolicySrcIP}) &&
					lrsr.IPPrefix == podIP+mask &&
					lrsr.Nexthop == gw
			},
			ExistingResult: &logicalRouterStaticRouteRes,
		},
		{
			Model: &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool {
				return lr.Name == gr
			},
			OnModelMutations: func() []model.Mutation {
				if len(logicalRouterStaticRouteRes) == 0 {
					return nil
				}
				uuids := []string{}
				for _, item := range logicalRouterStaticRouteRes {
					uuids = append(uuids, item.UUID)
				}
				return []model.Mutation{
					{
						Field:   &logicalRouter.StaticRoutes,
						Mutator: ovsdb.MutateOperationDelete,
						Value:   uuids,
					},
				}
			},
			ExistingResult: &[]nbdb.LogicalRouter{},
		},
	}
	if err := oc.modelClient.Delete(opModels...); err != nil {
		return fmt.Errorf("unable to delete src-ip route to GR router, err: %v", err)
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
	nsInfo := oc.getNamespaceLocked(namespace)
	if nsInfo == nil {
		return
	}
	defer nsInfo.Unlock()
	// check if any gateways were stored for this pod
	foundGws, ok := nsInfo.routingExternalPodGWs[pod]
	if !ok || len(foundGws.gws) == 0 {
		klog.Infof("No gateways found to remove for annotated gateway pod: %s on namespace: %s",
			pod, namespace)
		return
	}

	for _, gwIP := range foundGws.gws {
		// check for previously configured pod routes
		for podIP, gwInfo := range nsInfo.podExternalRoutes {
			if len(gwInfo) == 0 {
				continue
			}
			gr := gwInfo[gwIP.String()]
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

			if err := oc.deleteLogicalRouterStaticRoute(podIP, mask, gwIP.String(), gr); err != nil {
				klog.Error(err)
			} else {
				klog.V(5).Infof("ECMP route deleted for pod: %s, on gr: %s, to gw: %s", pod,
					gr, gwIP.String())
				delete(nsInfo.podExternalRoutes[podIP], gwIP.String())
				// clean up if there are no more routes for this podIP
				if entry := nsInfo.podExternalRoutes[podIP]; len(entry) == 0 {
					delete(nsInfo.podExternalRoutes, podIP)
					// TODO (trozet): use the go bindings here and batch commands
					// delete the ovn_cluster_router policy if the pod has no more exgws to revert back to normal
					// default gw behavior
					if err := oc.delHybridRoutePolicyForPod(net.ParseIP(podIP), node); err != nil {
						klog.Error(err)
					}
				}
			}
			oc.cleanUpBFDEntry(gwIP.String(), gr, portPrefix)
		}
	}
	delete(nsInfo.routingExternalPodGWs, pod)
}

// deleteGwRoutesForNamespace handles deleting all routes to gateways for a pod on a specific GR
// This should only be called with a lock on nsInfo
func (oc *Controller) deleteGWRoutesForNamespace(nsInfo *namespaceInfo) {
	if nsInfo == nil {
		return
	}

	// TODO(trozet): batch all of these with ebay bindings
	for podIP, gwToGr := range nsInfo.podExternalRoutes {
		for gw, gr := range gwToGr {
			if utilnet.IsIPv6String(gw) != utilnet.IsIPv6String(podIP) {
				continue
			}
			mask := GetIPFullMask(podIP)
			node := util.GetWorkerFromGatewayRouter(gr)
			if err := oc.delHybridRoutePolicyForPod(net.ParseIP(podIP), node); err != nil {
				klog.Error(err)
			}

			if err := oc.deleteLogicalRouterStaticRoute(podIP, mask, gw, gr); err != nil {
				klog.Error(err)
			} else {
				delete(nsInfo.podExternalRoutes, podIP)
			}

			portPrefix, err := oc.extSwitchPrefix(node)
			if err != nil {
				klog.Infof("Failed to find ext switch prefix for %s %v", node, err)
				continue
			}
			oc.cleanUpBFDEntry(gw, gr, portPrefix)
		}
	}
	nsInfo.routingExternalGWs = gatewayInfo{}
}

// deleteGwRoutesForPod handles deleting all routes to gateways for a pod IP on a specific GR
func (oc *Controller) deleteGWRoutesForPod(namespace string, podIPNets []*net.IPNet) {
	// delete src-ip cached route to GR
	nsInfo := oc.getNamespaceLocked(namespace)
	if nsInfo == nil {
		return
	}
	defer nsInfo.Unlock()

	for _, podIPNet := range podIPNets {
		pod := podIPNet.IP.String()
		if gwToGr, ok := nsInfo.podExternalRoutes[pod]; ok {
			if len(gwToGr) == 0 {
				delete(nsInfo.podExternalRoutes, pod)
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

				if err := oc.delHybridRoutePolicyForPod(podIPNet.IP, node); err != nil {
					klog.Error(err)
				}
				if err := oc.deleteLogicalRouterStaticRoute(pod, mask, gw, gr); err != nil {
					klog.Errorf("Unable to delete external gw ecmp route to GR router, err: %v", err)
				} else {
					delete(nsInfo.podExternalRoutes, pod)
				}
				oc.cleanUpBFDEntry(gw, gr, portPrefix)
			}
		}
	}
}

// addEgressGwRoutesForPod handles adding all routes to gateways for a pod on a specific GR
func (oc *Controller) addGWRoutesForPod(gateways []gatewayInfo, podIfAddrs []*net.IPNet, namespace, node string) error {
	nsInfo := oc.getNamespaceLocked(namespace)
	defer nsInfo.Unlock()
	gr := util.GetGatewayRouterFromNode(node)

	routesAdded := 0
	portPrefix, err := oc.extSwitchPrefix(node)
	if err != nil {
		klog.Infof("Failed to find ext switch prefix for %s %v", node, err)
		return err
	}

	port := portPrefix + types.GWRouterToExtSwitchPrefix + gr

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
					if foundGR, ok := nsInfo.podExternalRoutes[podIP][gwStr]; ok && foundGR == gr {
						routesAdded++
						continue
					}
					mask := GetIPFullMask(podIP)

					if err := oc.createBFDStaticRoute(gateway.bfdEnabled, gw, podIP, gr, port, mask); err != nil {
						return err
					}
					if err := oc.addHybridRoutePolicyForPod(podIPNet.IP, node); err != nil {
						return err
					}
					if nsInfo.podExternalRoutes[podIP] == nil {
						nsInfo.podExternalRoutes[podIP] = make(map[string]string)
					}
					nsInfo.podExternalRoutes[podIP][gwStr] = gr
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
			namespace, podIfAddrs, node)
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
		grJoinIfAddrs, err := util.GetLRPAddrs(oc.nbClient, types.GWRouterToJoinSwitchPrefix+types.GWRouterPrefix+node)
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

		intPriority, _ := strconv.Atoi(types.HybridOverlayReroutePriority)
		namedUUID := util.GenerateNamedUUID()

		logicalRouter := nbdb.LogicalRouter{}
		logicalRouterPolicy := nbdb.LogicalRouterPolicy{
			UUID:     namedUUID,
			Priority: intPriority,
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			Nexthops: []string{grJoinIfAddr.IP.String()},
			Match:    matchStr,
		}
		logicalRouterPolicyRes := []nbdb.LogicalRouterPolicy{}
		opModels := []util.OperationModel{
			{
				Model: &logicalRouterPolicy,
				ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
					return lrp.Priority == intPriority && strings.Contains(lrp.Match, podIP.String())
				},
				OnModelUpdates: []interface{}{
					&logicalRouterPolicy.Nexthops,
					&logicalRouterPolicy.Match,
				},
				ExistingResult: &logicalRouterPolicyRes,
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
				OnModelMutations: func() []model.Mutation {
					if len(logicalRouterPolicyRes) > 0 {
						return nil
					}
					return []model.Mutation{
						{
							Field:   &logicalRouter.Policies,
							Mutator: ovsdb.MutateOperationInsert,
							Value:   []string{namedUUID},
						},
					}
				},
				ExistingResult: &[]nbdb.LogicalRouter{},
			},
		}
		if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("failed to add policy route '%s' to %s, error: %v", matchStr, types.OVNClusterRouter, err)
		}
	}
	return nil
}

// delHybridRoutePolicyForPod handles deleting a higher priority allow policy to allow traffic to be routed normally
// by ecmp routes
func (oc *Controller) delHybridRoutePolicyForPod(podIP net.IP, node string) error {
	if config.Gateway.Mode == config.GatewayModeLocal {
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

		intPriority, _ := strconv.Atoi(types.HybridOverlayReroutePriority)

		logicalRouter := nbdb.LogicalRouter{}
		logicalRouterPolicyRes := []nbdb.LogicalRouterPolicy{}
		opModels := []util.OperationModel{
			{
				ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
					return lrp.Priority == intPriority && lrp.Match == matchStr
				},
				ExistingResult: &logicalRouterPolicyRes,
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == ovntypes.OVNClusterRouter },
				OnModelMutations: func() []model.Mutation {
					if len(logicalRouterPolicyRes) == 0 {
						return nil
					}
					uuids := []string{}
					for _, lrp := range logicalRouterPolicyRes {
						uuids = append(uuids, lrp.UUID)
					}
					return []model.Mutation{
						{
							Field:   &logicalRouter.Policies,
							Mutator: ovsdb.MutateOperationDelete,
							Value:   uuids,
						},
					}
				},
				ExistingResult: &[]nbdb.LogicalRouter{},
			},
		}
		if err := oc.modelClient.Delete(opModels...); err != nil {
			klog.Errorf("Failed to remove policy: %s, on: %s, err: %v", matchStr, types.OVNClusterRouter, err)
		}
	}
	return nil
}

// cleanUpBFDEntry checks if the BFD table entry related to the associated
// gw router / port / gateway ip is referenced by other routing rules, and if
// not removes the entry to avoid having dangling BFD entries.
func (oc *Controller) cleanUpBFDEntry(gatewayIP, gatewayRouter, prefix string) {
	portName := prefix + types.GWRouterToExtSwitchPrefix + gatewayRouter
	opModels := []util.OperationModel{
		{
			Model: &nbdb.BFD{},
			ModelPredicate: func(bfd *nbdb.BFD) bool {
				return bfd.LogicalPort == portName && bfd.DstIP == gatewayIP
			},
			ExistingResult: &[]nbdb.BFD{},
		},
	}
	if err := oc.modelClient.Delete(opModels...); err != nil {
		klog.Errorf("Failed to delete BFD, err: %v", err)
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

// cleanECMPRoutes clean legacy ecmp routes when switching from
// single bridge to default bridge plus second bridge used for external
// gateway routes.
func (oc *Controller) cleanECMPRoutes() {
	out, stderr, err := util.RunOVNNbctl(
		"--format=csv", "--data=bare", "--no-heading", "--columns=_uuid,output_port", "find", "Logical_Router_Static_Route", "options={ecmp_symmetric_reply=\"true\"}")
	if err != nil {
		klog.Errorf("cleanECMPRoutes: failed to list ecmp routes %v %s", err, stderr)
		return
	}
	if strings.TrimSpace(out) == "" {
		klog.Infof("Did not find ecmp routes to clean")
		return
	}
	for _, line := range strings.Split(out, "\n") {
		values := strings.Split(line, ",")
		uuid := values[0]
		port := values[1]

		out, stderr, err := util.RunOVNNbctl(
			"--format=csv", "--data=bare", "--no-heading", "--columns=_uuid,name", "find", "Logical_Router", fmt.Sprintf("static_routes{>=}[%s]", uuid))
		if err != nil || out == "" {
			klog.Errorf("cleanECMPRoutes: failed to find logical router for %s", uuid, err, stderr)
			continue
		}
		values = strings.Split(out, ",")
		lruuid := values[0]
		gr := values[1]
		node := util.GetWorkerFromGatewayRouter(gr)
		prefix, err := oc.extSwitchPrefix(node)
		if err != nil {
			klog.Errorf("cleanECMPRoutes: failed to find logical router for %s", uuid, err, stderr)
			continue
		}
		if (prefix != "" && !strings.Contains(port, prefix)) ||
			(prefix == "" && strings.Contains(port, prefix)) {
			klog.Infof("Found legacy ecmp route, output_port=%s, extSwitchPrefix=%s", port, prefix)
			_, stderr, err = util.RunOVNNbctl("--if-exists", "remove", "Logical_Router", strings.TrimSuffix(lruuid, "\n"), "static_routes", uuid)
			if err != nil {
				klog.Errorf("Failed to destroy Logical_Router_Static_Route %s, stderr: %q, (%v)",
					uuid, stderr, err)
			}
		}
	}
}

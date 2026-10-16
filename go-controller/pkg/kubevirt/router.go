// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	logicalswitchmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// DeleteRoutingForMigratedPodWithZone deletes migrated-pod routes and policies for the specified OVN zone.
func DeleteRoutingForMigratedPodWithZone(nbClient libovsdbclient.Client, pod *corev1.Pod, zone string) error {
	vmDescription, err := NewVMDescriptionFromPod(pod)
	if err != nil {
		return err
	}
	if vmDescription == nil {
		return nil
	}
	predicate := func(itemExternalIDs map[string]string) bool {
		containsZone := true
		if zone != "" {
			containsZone = itemExternalIDs[OvnZoneExternalIDKey] == zone
		}
		return containsZone && externalIDsContainsVM(itemExternalIDs, ptr.To(vmDescription.Key()))
	}
	routePredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return predicate(item.ExternalIDs)
	}
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(nbClient, types.OVNClusterRouter, routePredicate); err != nil {
		return fmt.Errorf("failed deleting pod routing when deleting the LR static routes: %v", err)
	}
	policyPredicate := func(item *nbdb.LogicalRouterPolicy) bool {
		return predicate(item.ExternalIDs)
	}
	if err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nbClient, types.OVNClusterRouter, policyPredicate); err != nil {
		return fmt.Errorf("failed deleting pod routing when deleting the LR policies: %v", err)
	}
	return nil
}

// DeleteRoutingForMigratedPod deletes migrated-pod routes and policies across all OVN zones.
func DeleteRoutingForMigratedPod(nbClient libovsdbclient.Client, pod *corev1.Pod) error {
	return DeleteRoutingForMigratedPodWithZone(nbClient, pod, "")
}

// EnsureDefaultNetworkForMigratablePod reconciles local or remote default-network
// routing for the active VM pod without waiting for guest readiness. For a ready
// local migration target, it refreshes the IPv4 gateway neighbor entry after
// routing has converged.
func EnsureDefaultNetworkForMigratablePod(watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client,
	lsManager *logicalswitchmanager.LogicalSwitchManager, localNodeName string, pod *corev1.Pod, clusterSubnets []config.CIDRNetworkEntry) error {
	if !util.PodScheduled(pod) || util.PodWantsHostNetwork(pod) || !IsPodLiveMigratable(pod) || util.PodCompleted(pod) {
		return nil
	}
	status, err := DiscoverLiveMigrationStatus(watchFactory.PodCoreInformer().Lister(), pod)
	if err != nil {
		return fmt.Errorf("failed discovering live migration status for pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	if status != nil && status.ActivePod.UID != pod.UID {
		return nil
	}
	if pod.Spec.NodeName != localNodeName {
		return ensureRemoteZonePodAddressesToNodeRoute(watchFactory, nbClient, pod)
	}
	if err := ensureLocalZonePodAddressesToNodeRoute(watchFactory, nbClient, lsManager, pod, types.DefaultNetworkName, clusterSubnets); err != nil {
		return fmt.Errorf("failed ensuring local-zone migration routes for pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	if config.IPv4Mode && status != nil && status.IsTarget(pod) {
		r := NewClusterDefaultNetworkGatewayAnnouncer(types.K8sMgmtIntfName)
		if err := r.AnnounceIPv4AfterLiveMigration(status); err != nil {
			return fmt.Errorf("failed reconciling IPv4 gateway after live migration for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	return nil
}

// ensureLocalZonePodAddressesToNodeRoute adds static routes to the ovn_cluster_router logical router
// so VM traffic works as expected after live migration when the pod is running in the local zone.
//
// Following is the list of NB logical resources created:
//
//   - static route with cluster wide CIDR as src-ip prefix and nexthop GR; it has less
//     priority than route to use overlay in case of pod to pod communication
//   - static route with VM ip as dst-ip prefix and output port the LRP pointing to the VM's node switch
func ensureLocalZonePodAddressesToNodeRoute(watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client,
	lsManager *logicalswitchmanager.LogicalSwitchManager, pod *corev1.Pod, nadKey string, clusterSubnets []config.CIDRNetworkEntry) error {
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if err != nil {
		return fmt.Errorf("failed reading local pod annotation: %v", err)
	}

	nodeOwningSubnet, _ := ZoneContainsPodSubnet(lsManager, podAnnotation.IPs)
	vmRunningAtNodeOwningSubnet := nodeOwningSubnet == pod.Spec.NodeName
	if vmRunningAtNodeOwningSubnet {
		// Point to point routing is no longer needed if vm
		// is running at the node that owns the subnet
		if err := DeleteRoutingForMigratedPod(nbClient, pod); err != nil {
			return fmt.Errorf("failed configuring pod routing when deleting stale static routes or policies for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		return nil
	}

	// NOTE: EIP & ESVC use same route and if this is already present thanks to
	// those features, this will be a no-op.
	node, err := watchFactory.GetNode(pod.Spec.NodeName)
	if err != nil {
		return fmt.Errorf("failed getting to list node %q for pod %s/%s: %w", pod.Spec.NodeName, pod.Namespace, pod.Name, err)
	}
	gatewayIPs, err := udn.GetGWRouterIPs(node, &util.DefaultNetInfo{})
	if err != nil {
		return fmt.Errorf("failed to get default network gateway router join IPs for node %q: %w", node.Name, err)
	}
	if err := libovsdbutil.CreateDefaultRouteToExternal(nbClient, types.OVNClusterRouter,
		types.GWRouterPrefix+pod.Spec.NodeName, clusterSubnets, gatewayIPs); err != nil {
		return err
	}

	for _, podIP := range podAnnotation.IPs {
		podAddress := podIP.IP.String()
		vmDescription, err := NewVMDescriptionFromPod(pod)
		if err != nil {
			return err
		}
		if vmDescription == nil {
			return nil
		}

		// Add a route for reroute ingress traffic to the VM port since
		// the subnet is alien to ovn_cluster_router
		defaultNetInfo := &util.DefaultNetInfo{}
		outputPort := defaultNetInfo.GetNetworkScopedRouterToSwitchPortName(pod.Spec.NodeName)
		ingressRoute := nbdb.LogicalRouterStaticRoute{
			IPPrefix:   podAddress,
			Nexthop:    podAddress,
			Policy:     &nbdb.LogicalRouterStaticRoutePolicyDstIP,
			OutputPort: &outputPort,
			ExternalIDs: map[string]string{
				OvnZoneExternalIDKey:         OvnLocalZone,
				VirtualMachineExternalIDsKey: vmDescription.Key().Name,
				NamespaceExternalIDsKey:      pod.Namespace,
			},
		}
		if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(nbClient, types.OVNClusterRouter, &ingressRoute, func(item *nbdb.LogicalRouterStaticRoute) bool {
			matches := item.IPPrefix == ingressRoute.IPPrefix && item.Policy != nil && *item.Policy == *ingressRoute.Policy
			return matches
		}); err != nil {
			return fmt.Errorf("failed adding static route: %v", err)
		}
	}
	return nil
}

// ensureRemoteZonePodAddressesToNodeRoute adds static routes when a live
// migrated pod belongs to a remote zone, sending traffic over the transit switch
// port of the node where the pod is running:
//   - A dst-ip with live migrated pod ip as prefix and nexthop the pod's
//     current node transit switch port.
func ensureRemoteZonePodAddressesToNodeRoute(watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client, pod *corev1.Pod) error {
	// DHCPOptions are only needed at the node is running the VM
	// at that's the local zone node not the remote zone
	if err := DeleteDHCPOptions(nbClient, pod); err != nil {
		return err
	}

	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, types.DefaultNetworkName)
	if err != nil {
		return fmt.Errorf("failed reading remote pod annotation: %v", err)
	}

	vmRunningAtNodeOwningSubnet, err := nodeContainsPodSubnet(watchFactory, pod.Spec.NodeName, podAnnotation, types.DefaultNetworkName)
	if err != nil {
		return err
	}
	if vmRunningAtNodeOwningSubnet {
		// Point to point routing is no longer needed if vm
		// is running at the node with VM's subnet
		if err := DeleteRoutingForMigratedPod(nbClient, pod); err != nil {
			return err
		}
		return nil
	} else {
		// Since we are at remote zone we should not have local zone point to
		// to point routing
		if err := DeleteRoutingForMigratedPodWithZone(nbClient, pod, OvnLocalZone); err != nil {
			return err
		}
	}

	node, err := watchFactory.GetNode(pod.Spec.NodeName)
	if err != nil {
		return err
	}
	transitSwitchPortAddrs, err := util.ParseNodeTransitSwitchPortAddrs(node)
	if err != nil {
		return err
	}
	vmDescription, err := NewVMDescriptionFromPod(pod)
	if err != nil {
		return err
	}
	if vmDescription == nil {
		return nil
	}
	for _, podIP := range podAnnotation.IPs {
		ipFamily := utilnet.IPFamilyOfCIDR(podIP)
		transitSwitchPortAddr, err := util.MatchFirstIPNetFamily(ipFamily == utilnet.IPv6, transitSwitchPortAddrs)
		if err != nil {
			return err
		}
		route := nbdb.LogicalRouterStaticRoute{
			IPPrefix: podIP.IP.String(),
			Nexthop:  transitSwitchPortAddr.IP.String(),
			Policy:   &nbdb.LogicalRouterStaticRoutePolicyDstIP,
			ExternalIDs: map[string]string{
				OvnZoneExternalIDKey:         OvnRemoteZone,
				VirtualMachineExternalIDsKey: vmDescription.Key().Name,
				NamespaceExternalIDsKey:      pod.Namespace,
			},
		}
		if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(nbClient, types.OVNClusterRouter, &route, func(item *nbdb.LogicalRouterStaticRoute) bool {
			matches := item.IPPrefix == route.IPPrefix && item.Policy != nil && *item.Policy == *route.Policy
			return matches
		}); err != nil {
			return fmt.Errorf("failed adding static route to remote pod: %v", err)
		}
	}
	return nil
}

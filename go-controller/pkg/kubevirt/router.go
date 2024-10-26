package kubevirt

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"

	kubevirtv1 "kubevirt.io/api/core/v1"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	logicalswitchmanager "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func DeleteRoutingForMigratedPodWithZone(nbClient libovsdbclient.Client, pod *corev1.Pod, zone string) error {
	vm := ExtractVMNameFromPod(pod)
	predicate := func(itemExternalIDs map[string]string) bool {
		containsZone := true
		if zone != "" {
			containsZone = itemExternalIDs[OvnZoneExternalIDKey] == zone
		}
		return containsZone && externalIDsContainsVM(itemExternalIDs, vm)
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

func DeleteRoutingForMigratedPod(nbClient libovsdbclient.Client, pod *corev1.Pod) error {
	return DeleteRoutingForMigratedPodWithZone(nbClient, pod, "")
}

// EnsureLocalZonePodAddressesToNodeRoute will add static routes and policies to ovn_cluster_route logical router
// to ensure VM traffic work as expected after live migration if the pod is running at the local/global zone.
//
// NOTE: IC with multiple nodes per zone is not supported
//
// Following is the list of NB logical resources created depending if it's interconnected or not:
//
// IC (on node per zone):
//   - static route with cluster wide CIDR as src-ip prefix and nexthop GR, it has less
//     priority than route to use overlay in case of pod to pod communication
//
// NO IC:
//   - low priority policy with src VM ip and reroute GR, since it has low priority
//     it will not override the policy to enroute pod to pod traffic using overlay
//
// Both:
//   - static route with VM ip as dst-ip prefix and output port the LRP pointing to the VM's node switch
func EnsureLocalZonePodAddressesToNodeRoute(watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client,
	lsManager *logicalswitchmanager.LogicalSwitchManager, pod *corev1.Pod, nadName string, clusterSubnets []config.CIDRNetworkEntry) error {
	vmReady, err := virtualMachineReady(watchFactory, pod)
	if err != nil {
		return err
	}
	if !vmReady {
		return nil
	}
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadName)
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

	// For interconnect at static route with a cluster-wide src-ip address is
	// needed to route egress n/s traffic
	if config.OVNKubernetesFeature.EnableInterconnect {
		// NOTE: EIP & ESVC use same route and if this is already present thanks to those features,
		// this will be a no-op
		if err := libovsdbutil.CreateDefaultRouteToExternal(nbClient, types.OVNClusterRouter, types.GWRouterPrefix+pod.Spec.NodeName, clusterSubnets); err != nil {
			return err
		}
	}

	lrpName := types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + pod.Spec.NodeName
	lrpAddresses, err := libovsdbutil.GetLRPAddrs(nbClient, lrpName)
	if err != nil {
		return fmt.Errorf("failed configuring pod routing when reading LRP %s addresses: %v", lrpName, err)
	}
	for _, podIP := range podAnnotation.IPs {
		podAddress := podIP.IP.String()

		if !config.OVNKubernetesFeature.EnableInterconnect {
			// Policy to with low priority to route traffic to the gateway
			ipFamily := utilnet.IPFamilyOfCIDR(podIP)
			nodeGRAddress, err := util.MatchFirstIPNetFamily(ipFamily == utilnet.IPv6, lrpAddresses)
			if err != nil {
				return err
			}

			// adds a policy so that a migrated pods egress traffic
			// will be routed to the local GR where it now resides
			match := fmt.Sprintf("ip%s.src == %s", ipFamily, podAddress)
			egressPolicy := nbdb.LogicalRouterPolicy{
				Match:    match,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{nodeGRAddress.IP.String()},
				Priority: types.EgressLiveMigrationReroutePriority,
				ExternalIDs: map[string]string{
					OvnZoneExternalIDKey:         OvnLocalZone,
					VirtualMachineExternalIDsKey: pod.Labels[kubevirtv1.VirtualMachineNameLabel],
					NamespaceExternalIDsKey:      pod.Namespace,
				},
			}
			if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(nbClient, types.OVNClusterRouter, &egressPolicy, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Priority == egressPolicy.Priority && item.Match == egressPolicy.Match && item.Action == egressPolicy.Action
			}); err != nil {
				return fmt.Errorf("failed adding point to point policy for pod %s/%s : %v", pod.Namespace, pod.Name, err)
			}
		}
		// Add a route for reroute ingress traffic to the VM port since
		// the subnet is alien to ovn_cluster_router
		outputPort := types.RouterToSwitchPrefix + pod.Spec.NodeName
		ingressRoute := nbdb.LogicalRouterStaticRoute{
			IPPrefix:   podAddress,
			Nexthop:    podAddress,
			Policy:     &nbdb.LogicalRouterStaticRoutePolicyDstIP,
			OutputPort: &outputPort,
			ExternalIDs: map[string]string{
				OvnZoneExternalIDKey:         OvnLocalZone,
				VirtualMachineExternalIDsKey: pod.Labels[kubevirtv1.VirtualMachineNameLabel],
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

// EnsureRemoteZonePodAddressesToNodeRoute will add static routes when live
// migrated pod belongs to remote zone to send traffic over transwitch switch
// port of the node where the pod is running:
//   - A dst-ip with live migrated pod ip as prefix and nexthop the pod's
//     current node transit switch port.
func EnsureRemoteZonePodAddressesToNodeRoute(controllerName string, watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client, lsManager *logicalswitchmanager.LogicalSwitchManager, pod *corev1.Pod, nadName string) error {
	vmReady, err := virtualMachineReady(watchFactory, pod)
	if err != nil {
		return err
	}
	if !vmReady {
		return nil
	}
	// DHCPOptions are only needed at the node is running the VM
	// at that's the local zone node not the remote zone
	if err := DeleteDHCPOptions(nbClient, pod); err != nil {
		return err
	}

	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadName)
	if err != nil {
		return fmt.Errorf("failed reading remote pod annotation: %v", err)
	}

	vmRunningAtNodeOwningSubnet, err := nodeContainsPodSubnet(watchFactory, pod.Spec.NodeName, podAnnotation, nadName)
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
				VirtualMachineExternalIDsKey: pod.Labels[kubevirtv1.VirtualMachineNameLabel],
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

func virtualMachineReady(watchFactory *factory.WatchFactory, pod *corev1.Pod) (bool, error) {
	isMigratedSourcePodStale, err := IsMigratedSourcePodStale(watchFactory, pod)
	if err != nil {
		return false, err
	}
	if util.PodWantsHostNetwork(pod) || !IsPodLiveMigratable(pod) || isMigratedSourcePodStale {
		return false, nil
	}

	// When a virtual machine start up this
	// label is the signal from KubeVirt to notify that the VM is
	// ready to receive traffic.
	targetNode := pod.Labels[kubevirtv1.NodeNameLabel]

	// This annotation only appears on live migration scenarios and it signals
	// that target VM pod is ready to receive traffic so we can route
	// taffic to it.
	targetReadyTimestamp := pod.Annotations[kubevirtv1.MigrationTargetReadyTimestamp]

	// VM is ready to receive traffic
	return targetNode == pod.Spec.NodeName || targetReadyTimestamp != "", nil
}

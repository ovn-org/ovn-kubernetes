package ovn

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	anpcontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/admin_network_policy"
	egresssvc_zone "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/egressservice"
	nqoscontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/network_qos"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	listers "k8s.io/client-go/listers/core/v1"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/klog/v2"
)

const egressFirewallDNSDefaultDuration = 30 * time.Minute

const (
	// TCP is the constant string for the string "TCP"
	TCP = "TCP"

	// UDP is the constant string for the string "UDP"
	UDP = "UDP"

	// SCTP is the constant string for the string "SCTP"
	SCTP = "SCTP"
)

// getPodNamespacedName returns <namespace>_<podname> for the provided pod
func getPodNamespacedName(pod *kapi.Pod) string {
	return util.GetLogicalPortName(pod.Namespace, pod.Name)
}

// syncPeriodic adds a goroutine that periodically does some work
// right now there is only one ticker registered
// for syncNodesPeriodic which deletes chassis records from the sbdb
// every 5 minutes
func (oc *DefaultNetworkController) syncPeriodic() {
	go func() {
		nodeSyncTicker := time.NewTicker(5 * time.Minute)
		defer nodeSyncTicker.Stop()
		for {
			select {
			case <-nodeSyncTicker.C:
				oc.syncNodesPeriodic()
			case <-oc.stopChan:
				return
			}
		}
	}()
}

func (oc *DefaultNetworkController) getPortInfo(pod *kapi.Pod) *lpInfo {
	var portInfo *lpInfo
	key := util.GetLogicalPortName(pod.Namespace, pod.Name)
	if util.PodWantsHostNetwork(pod) {
		// create dummy logicalPortInfo for host-networked pods
		mac, _ := net.ParseMAC("00:00:00:00:00:00")
		portInfo = &lpInfo{
			logicalSwitch: "host-networked",
			name:          key,
			uuid:          "host-networked",
			ips:           []*net.IPNet{},
			mac:           mac,
		}
	} else {
		portInfo, _ = oc.logicalPortCache.get(pod, ovntypes.DefaultNetworkName)
	}
	return portInfo
}

func (oc *DefaultNetworkController) recordPodEvent(reason string, addErr error, pod *kapi.Pod) {
	podRef, err := ref.GetReference(scheme.Scheme, pod)
	if err != nil {
		klog.Errorf("Couldn't get a reference to pod %s/%s to post an event: '%v'",
			pod.Namespace, pod.Name, err)
	} else {
		klog.V(5).Infof("Posting a %s event for Pod %s/%s", kapi.EventTypeWarning, pod.Namespace, pod.Name)
		oc.recorder.Eventf(podRef, kapi.EventTypeWarning, reason, addErr.Error())
	}
}

func (oc *DefaultNetworkController) recordNodeEvent(reason string, addErr error, node *kapi.Node) {
	nodeRef, err := ref.GetReference(scheme.Scheme, node)
	if err != nil {
		klog.Errorf("Couldn't get a reference to node %s to post an event: '%v'", node.Name, err)
	} else {
		klog.V(5).Infof("Posting a %s event for node %s", kapi.EventTypeWarning, node.Name)
		oc.recorder.Eventf(nodeRef, kapi.EventTypeWarning, reason, addErr.Error())
	}
}

func exGatewayAnnotationsChanged(oldPod, newPod *kapi.Pod) bool {
	return oldPod.Annotations[util.RoutingNamespaceAnnotation] != newPod.Annotations[util.RoutingNamespaceAnnotation] ||
		oldPod.Annotations[util.RoutingNetworkAnnotation] != newPod.Annotations[util.RoutingNetworkAnnotation] ||
		oldPod.Annotations[util.BfdAnnotation] != newPod.Annotations[util.BfdAnnotation]
}

func networkStatusAnnotationsChanged(oldPod, newPod *kapi.Pod) bool {
	return oldPod.Annotations[nettypes.NetworkStatusAnnot] != newPod.Annotations[nettypes.NetworkStatusAnnot]
}

// ensurePod tries to set up a pod. It returns nil on success and error on failure; failure
// indicates the pod set up should be retried later.
func (oc *DefaultNetworkController) ensurePod(oldPod, pod *kapi.Pod, addPort bool) error {
	// Try unscheduled pods later
	if !util.PodScheduled(pod) {
		return nil
	}

	// skip the pods on no host subnet nodes
	switchName := pod.Spec.NodeName
	if oc.lsManager.IsNonHostSubnetSwitch(switchName) {
		return nil
	}

	if oc.isPodScheduledinLocalZone(pod) {
		klog.V(5).Infof("Ensuring zone local for Pod %s/%s in node %s", pod.Namespace, pod.Name, pod.Spec.NodeName)
		return oc.ensureLocalZonePod(oldPod, pod, addPort)
	}

	klog.V(5).Infof("Ensuring zone remote for Pod %s/%s in node %s", pod.Namespace, pod.Name, pod.Spec.NodeName)
	return oc.ensureRemoteZonePod(oldPod, pod, addPort)
}

// ensureLocalZonePod tries to set up a local zone pod. It returns nil on success and error on failure; failure
// indicates the pod set up should be retried later.
func (oc *DefaultNetworkController) ensureLocalZonePod(oldPod, pod *kapi.Pod, addPort bool) error {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			eventName := "add"
			if !addPort {
				eventName = "update"
			}
			metrics.RecordPodEvent(eventName, duration)
		}()
	}

	if oldPod != nil && (exGatewayAnnotationsChanged(oldPod, pod) || networkStatusAnnotationsChanged(oldPod, pod)) {
		// No matter if a pod is ovn networked, or host networked, we still need to check for exgw
		// annotations. If the pod is ovn networked and is in update reschedule, addLogicalPort will take
		// care of updating the exgw updates
		if err := oc.deletePodExternalGW(oldPod); err != nil {
			return fmt.Errorf("ensurePod failed %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	if !util.PodWantsHostNetwork(pod) && addPort {
		if err := oc.addLogicalPort(pod); err != nil {
			return fmt.Errorf("addLogicalPort failed for %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	} else {
		// either pod is host-networked or its an update for a normal pod (addPort=false case)
		if oldPod == nil || exGatewayAnnotationsChanged(oldPod, pod) || networkStatusAnnotationsChanged(oldPod, pod) {
			if err := oc.addPodExternalGW(pod); err != nil {
				return fmt.Errorf("addPodExternalGW failed for %s/%s: %w", pod.Namespace, pod.Name, err)
			}
		}
	}

	if kubevirt.IsPodLiveMigratable(pod) {
		return kubevirt.EnsureLocalZonePodAddressesToNodeRoute(oc.watchFactory, oc.nbClient, oc.lsManager, pod, ovntypes.DefaultNetworkName)
	}

	return nil
}

// ensureRemoteZonePod tries to set up remote zone pod bits required to interconnect it.
//   - Adds the remote pod ips to the pod namespace address set for network policy and egress gw
//
// It returns nil on success and error on failure; failure indicates the pod set up should be retried later.
func (oc *DefaultNetworkController) ensureRemoteZonePod(oldPod, pod *kapi.Pod, addPort bool) error {
	if (addPort || (oldPod != nil && len(pod.Status.PodIPs) != len(oldPod.Status.PodIPs))) && !util.PodWantsHostNetwork(pod) {
		podIfAddrs, err := util.GetPodCIDRsWithFullMask(pod, oc.NetInfo)
		if err != nil {
			// not finding pod IPs on a remote pod is common until the other node wires the pod, suppress it
			return fmt.Errorf("failed to obtain IPs to add remote pod %s/%s: %w",
				pod.Namespace, pod.Name, ovntypes.NewSuppressedError(err))
		}
		if err := oc.addRemotePodToNamespace(pod.Namespace, podIfAddrs); err != nil {
			return fmt.Errorf("failed to add remote pod %s/%s to namespace: %w", pod.Namespace, pod.Name, err)
		}
	}

	//FIXME: Update comments & reduce code duplication.
	// check if this remote pod is serving as an external GW.
	if oldPod != nil && (exGatewayAnnotationsChanged(oldPod, pod) || networkStatusAnnotationsChanged(oldPod, pod)) {
		// Delete the routes in the namespace associated with this remote oldPod if its acting as an external GW
		if err := oc.deletePodExternalGW(oldPod); err != nil {
			return fmt.Errorf("deletePodExternalGW failed for remote pod %s/%s: %w", oldPod.Namespace, oldPod.Name, err)
		}
	}

	// either pod is host-networked or its an update for a normal pod (addPort=false case)
	if oldPod == nil || exGatewayAnnotationsChanged(oldPod, pod) || networkStatusAnnotationsChanged(oldPod, pod) {
		// check if this remote pod is serving as an external GW. If so add the routes in the namespace
		// associated with this remote pod
		if err := oc.addPodExternalGW(pod); err != nil {
			return fmt.Errorf("addPodExternalGW failed for remote pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
	}
	if kubevirt.IsPodLiveMigratable(pod) {
		return kubevirt.EnsureRemoteZonePodAddressesToNodeRoute(oc.controllerName, oc.watchFactory, oc.nbClient, oc.lsManager, pod, ovntypes.DefaultNetworkName)
	}
	return nil
}

// removePod tried to tear down a pod. It returns nil on success and error on failure;
// failure indicates the pod tear down should be retried later.
func (oc *DefaultNetworkController) removePod(pod *kapi.Pod, portInfo *lpInfo) error {
	if oc.isPodScheduledinLocalZone(pod) {
		if err := oc.removeLocalZonePod(pod, portInfo); err != nil {
			return err
		}
	} else {
		if err := oc.removeRemoteZonePod(pod); err != nil {
			return err
		}
	}

	err := kubevirt.CleanUpLiveMigratablePod(oc.nbClient, oc.watchFactory, pod)
	if err != nil {
		return err
	}

	oc.forgetPodReleasedBeforeStartup(string(pod.UID), ovntypes.DefaultNetworkName)
	return nil
}

// removeLocalZonePod tries to tear down a local zone pod. It returns nil on success and error on failure;
// failure indicates the pod tear down should be retried later.
func (oc *DefaultNetworkController) removeLocalZonePod(pod *kapi.Pod, portInfo *lpInfo) error {
	oc.logicalPortCache.remove(pod, ovntypes.DefaultNetworkName)

	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordPodEvent("delete", duration)
		}()
	}
	if util.PodWantsHostNetwork(pod) {
		if err := oc.deletePodExternalGW(pod); err != nil {
			return fmt.Errorf("unable to delete external gateway routes for pod %s: %w",
				getPodNamespacedName(pod), err)
		}
		return nil
	}
	if err := oc.deleteLogicalPort(pod, portInfo); err != nil {
		return fmt.Errorf("deleteLogicalPort failed for pod %s: %w",
			getPodNamespacedName(pod), err)
	}

	return nil
}

// removeRemoteZonePod tries to tear down a remote zone pod bits. It returns nil on success and error on failure;
// failure indicates the pod tear down should be retried later.
// It removes the remote pod ips from the namespace address set and if its an external gw pod, removes
// its routes.
func (oc *DefaultNetworkController) removeRemoteZonePod(pod *kapi.Pod) error {
	if util.PodWantsHostNetwork(pod) {
		// Delete the routes in the namespace associated with this remote pod if it was acting as an external GW
		if err := oc.deletePodExternalGW(pod); err != nil {
			return fmt.Errorf("unable to delete external gateway routes for remote pod %s: %w",
				getPodNamespacedName(pod), err)
		}
	}

	// while this check is only intended for local pods, we also need it for
	// remote live migrated pods that might have been allocated from this zone
	if oc.wasPodReleasedBeforeStartup(string(pod.UID), ovntypes.DefaultNetworkName) {
		klog.Infof("Completed pod %s/%s was already released before startup",
			pod.Namespace,
			pod.Name,
		)
		return nil
	}

	if err := oc.removeRemoteZonePodFromNamespaceAddressSet(pod); err != nil {
		return fmt.Errorf("failed to remove the remote zone pod: %w", err)
	}

	if kubevirt.IsPodLiveMigratable(pod) {
		ips, err := util.GetPodCIDRsWithFullMask(pod, oc.NetInfo)
		if err != nil && !errors.Is(err, util.ErrNoPodIPFound) {
			return fmt.Errorf("failed to get pod ips for the pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		switchName, zoneContainsPodSubnet := kubevirt.ZoneContainsPodSubnet(oc.lsManager, ips)
		if zoneContainsPodSubnet {
			if err := oc.lsManager.ReleaseIPs(switchName, ips); err != nil {
				return err
			}
		}
	}

	return nil
}

// WatchEgressFirewall starts the watching of egressfirewall resource and calls
// back the appropriate handler logic
func (oc *DefaultNetworkController) WatchEgressFirewall() error {
	_, err := oc.retryEgressFirewalls.WatchResource()
	return err
}

// WatchEgressNodes starts the watching of egress assignable nodes and calls
// back the appropriate handler logic.
func (oc *DefaultNetworkController) WatchEgressNodes() error {
	_, err := oc.retryEgressNodes.WatchResource()
	return err
}

// WatchEgressIP starts the watching of egressip resource and calls back the
// appropriate handler logic. It also initiates the other dedicated resource
// handlers for egress IP setup: namespaces, pods.
func (oc *DefaultNetworkController) WatchEgressIP() error {
	_, err := oc.retryEgressIPs.WatchResource()
	return err
}

func (oc *DefaultNetworkController) WatchEgressIPNamespaces() error {
	_, err := oc.retryEgressIPNamespaces.WatchResource()
	return err
}

func (oc *DefaultNetworkController) WatchEgressIPPods() error {
	_, err := oc.retryEgressIPPods.WatchResource()
	return err
}

// syncNodeGateway ensures a node's gateway router is configured
func (oc *DefaultNetworkController) syncNodeGateway(node *kapi.Node, hostSubnets []*net.IPNet) error {
	l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return err
	}

	if hostSubnets == nil {
		hostSubnets, err = util.ParseNodeHostSubnetAnnotation(node, ovntypes.DefaultNetworkName)
		if err != nil {
			return err
		}
	}

	if l3GatewayConfig.Mode == config.GatewayModeDisabled {
		if err := oc.gatewayCleanup(node.Name); err != nil {
			return fmt.Errorf("error cleaning up gateway for node %s: %v", node.Name, err)
		}
	} else if hostSubnets != nil {
		var hostAddrs []string
		if config.Gateway.Mode == config.GatewayModeShared {
			hostAddrs, err = util.GetNodeHostAddrs(node)
			if err != nil && !util.IsAnnotationNotSetError(err) {
				return fmt.Errorf("failed to get host CIDRs for node: %s: %v", node.Name, err)
			}
		}
		if err := oc.syncGatewayLogicalNetwork(node, l3GatewayConfig, hostSubnets, hostAddrs); err != nil {
			return fmt.Errorf("error creating gateway for node %s: %v", node.Name, err)
		}
	}
	return nil
}

// gatewayChanged() compares old annotations to new and returns true if something has changed.
func gatewayChanged(oldNode, newNode *kapi.Node) bool {
	oldL3GatewayConfig, _ := util.ParseNodeL3GatewayAnnotation(oldNode)
	l3GatewayConfig, _ := util.ParseNodeL3GatewayAnnotation(newNode)
	return !reflect.DeepEqual(oldL3GatewayConfig, l3GatewayConfig)
}

// hostCIDRsChanged compares old annotations to new and returns true if the something has changed.
func hostCIDRsChanged(oldNode, newNode *kapi.Node) bool {
	oldAddrs, _ := util.ParseNodeHostCIDRs(oldNode)
	Addrs, _ := util.ParseNodeHostCIDRs(newNode)
	return !oldAddrs.Equal(Addrs)
}

// macAddressChanged() compares old annotations to new and returns true if something has changed.
func macAddressChanged(oldNode, node *kapi.Node) bool {
	oldMacAddress, _ := util.ParseNodeManagementPortMACAddress(oldNode)
	macAddress, _ := util.ParseNodeManagementPortMACAddress(node)
	return !bytes.Equal(oldMacAddress, macAddress)
}

func nodeSubnetChanged(oldNode, node *kapi.Node) bool {
	oldSubnets, _ := util.ParseNodeHostSubnetAnnotation(oldNode, ovntypes.DefaultNetworkName)
	newSubnets, _ := util.ParseNodeHostSubnetAnnotation(node, ovntypes.DefaultNetworkName)
	return !reflect.DeepEqual(oldSubnets, newSubnets)
}

func primaryAddrChanged(oldNode, newNode *kapi.Node) bool {
	oldIP, _ := util.GetNodePrimaryIP(oldNode)
	newIP, _ := util.GetNodePrimaryIP(newNode)
	return oldIP != newIP
}

func nodeChassisChanged(oldNode, node *kapi.Node) bool {
	oldChassis, _ := util.ParseNodeChassisIDAnnotation(oldNode)
	newChassis, _ := util.ParseNodeChassisIDAnnotation(node)
	return oldChassis != newChassis
}

// nodeGatewayMTUSupportChanged returns true if annotation "k8s.ovn.org/gateway-mtu-support" on the node was updated.
func nodeGatewayMTUSupportChanged(oldNode, node *kapi.Node) bool {
	return util.ParseNodeGatewayMTUSupport(oldNode) != util.ParseNodeGatewayMTUSupport(node)
}

// shouldUpdateNode() determines if the ovn-kubernetes plugin should update the state of the node.
// ovn-kube should not perform an update if it does not assign a hostsubnet, or if you want to change
// whether or not ovn-kubernetes assigns a hostsubnet
func shouldUpdateNode(node, oldNode *kapi.Node) (bool, error) {
	newNoHostSubnet := util.NoHostSubnet(node)
	oldNoHostSubnet := util.NoHostSubnet(oldNode)

	if oldNoHostSubnet && newNoHostSubnet {
		return false, nil
	}

	return true, nil
}

func (oc *DefaultNetworkController) StartServiceController(wg *sync.WaitGroup, runRepair bool) error {
	klog.Infof("Starting OVN Service Controller: Using Endpoint Slices")
	wg.Add(1)
	go func() {
		defer wg.Done()
		useLBGroups := oc.clusterLoadBalancerGroupUUID != ""
		// use 5 workers like most of the kubernetes controllers in the
		// kubernetes controller-manager
		err := oc.svcController.Run(5, oc.stopChan, runRepair, useLBGroups, oc.svcTemplateSupport)
		if err != nil {
			klog.Errorf("Error running OVN Kubernetes Services controller: %v", err)
		}
	}()
	return nil
}

func (oc *DefaultNetworkController) InitEgressServiceZoneController() (*egresssvc_zone.Controller, error) {
	// If the EgressIP controller is enabled it will take care of creating the
	// "no reroute" policies - we can pass "noop" functions to the egress service controller.
	initClusterEgressPolicies := func(libovsdbclient.Client, addressset.AddressSetFactory, string, string) error { return nil }
	ensureNodeNoReroutePolicies := func(libovsdbclient.Client, addressset.AddressSetFactory, string, string, listers.NodeLister) error {
		return nil
	}
	deleteLegacyDefaultNoRerouteNodePolicies := func(libovsdbclient.Client, string, string) error { return nil }
	// used only when IC=true
	createDefaultNodeRouteToExternal := func(libovsdbclient.Client, string, string) error { return nil }

	if !config.OVNKubernetesFeature.EnableEgressIP {
		initClusterEgressPolicies = InitClusterEgressPolicies
		ensureNodeNoReroutePolicies = ensureDefaultNoRerouteNodePolicies
		deleteLegacyDefaultNoRerouteNodePolicies = DeleteLegacyDefaultNoRerouteNodePolicies
		createDefaultNodeRouteToExternal = libovsdbutil.CreateDefaultRouteToExternal
	}

	return egresssvc_zone.NewController(oc.NetInfo, DefaultNetworkControllerName, oc.client, oc.nbClient, oc.addressSetFactory,
		initClusterEgressPolicies, ensureNodeNoReroutePolicies, deleteLegacyDefaultNoRerouteNodePolicies,
		createDefaultNodeRouteToExternal,
		oc.stopChan, oc.watchFactory.EgressServiceInformer(), oc.watchFactory.ServiceCoreInformer(),
		oc.watchFactory.EndpointSliceCoreInformer(),
		oc.watchFactory.NodeCoreInformer(), oc.zone)
}

func (oc *DefaultNetworkController) newANPController() error {
	var err error
	oc.anpController, err = anpcontroller.NewController(
		DefaultNetworkControllerName,
		oc.nbClient,
		oc.kube.ANPClient,
		oc.watchFactory.ANPInformer(),
		oc.watchFactory.BANPInformer(),
		oc.watchFactory.NamespaceCoreInformer(),
		oc.watchFactory.PodCoreInformer(),
		oc.watchFactory.NodeCoreInformer(),
		oc.addressSetFactory,
		oc.isPodScheduledinLocalZone,
		oc.zone,
		oc.recorder,
	)
	return err
}

func (oc *DefaultNetworkController) newNetworkQoSController() error {
	var err error
	oc.nqosController, err = nqoscontroller.NewController(
		DefaultNetworkControllerName,
		oc.nbClient,
		oc.kube.NetworkQoSClient,
		oc.watchFactory.NetworkQoSInformer(),
		oc.watchFactory.NamespaceCoreInformer(),
		oc.watchFactory.PodCoreInformer(),
		oc.watchFactory.NodeCoreInformer(),
		oc.addressSetFactory,
		oc.isPodScheduledinLocalZone,
		oc.zone,
		oc.recorder,
	)
	return err
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	anpcontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/admin_network_policy"
	egresssvc_zone "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/egressservice"
	networkconnectcontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/networkconnect"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
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
func getPodNamespacedName(pod *corev1.Pod) string {
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

func (oc *DefaultNetworkController) getPortInfo(pod *corev1.Pod) *lpInfo {
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

func (oc *DefaultNetworkController) recordPodEvent(reason string, addErr error, pod *corev1.Pod) {
	podRef, err := ref.GetReference(scheme.Scheme, pod)
	if err != nil {
		klog.Errorf("Couldn't get a reference to pod %s/%s to post an event: '%v'",
			pod.Namespace, pod.Name, err)
	} else {
		klog.V(5).Infof("Posting a %s event for Pod %s/%s", corev1.EventTypeWarning, pod.Namespace, pod.Name)
		oc.recorder.Eventf(podRef, corev1.EventTypeWarning, reason, "%s", addErr.Error())
	}
}

// ensurePod tries to set up a pod. It returns nil on success and error on failure; failure
// indicates the pod set up should be retried later.
func (oc *DefaultNetworkController) ensurePod(oldPod, pod *corev1.Pod, addPort bool) error {
	// Try unscheduled pods later
	if !util.PodScheduled(pod) {
		return nil
	}

	if oc.isPodScheduledOnLocalNode(pod) {
		klog.V(5).Infof("Ensuring zone local for Pod %s/%s in node %s", pod.Namespace, pod.Name, pod.Spec.NodeName)
		return oc.ensureLocalZonePod(oldPod, pod, addPort)
	}

	klog.V(5).Infof("Ensuring zone remote for Pod %s/%s in node %s", pod.Namespace, pod.Name, pod.Spec.NodeName)
	return oc.ensureRemoteZonePod(oldPod, pod)
}

// ensureLocalZonePod tries to set up a local zone pod. It returns nil on success and error on failure; failure
// indicates the pod set up should be retried later.
func (oc *DefaultNetworkController) ensureLocalZonePod(oldPod, pod *corev1.Pod, addPort bool) error {
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

	if !util.PodWantsHostNetwork(pod) && addPort {
		if err := oc.addLogicalPort(pod); err != nil {
			return fmt.Errorf("addLogicalPort failed for %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	// update open ports for UDN pods on pod update.
	if util.IsNetworkSegmentationSupportEnabled() && !util.PodWantsHostNetwork(pod) && !addPort &&
		pod != nil && oldPod != nil &&
		pod.Annotations[util.UDNOpenPortsAnnotationName] != oldPod.Annotations[util.UDNOpenPortsAnnotationName] {
		networkRole, err := oc.GetNetworkRole(pod)
		if err != nil {
			return err
		}
		if networkRole == ovntypes.NetworkRoleInfrastructure {
			// only update for non-default network pods
			portName := oc.GetLogicalPortName(pod, oc.GetNetworkName())
			err := oc.setUDNPodOpenPorts(pod.Namespace+"/"+pod.Name, pod.Annotations, portName)
			if err != nil {
				return fmt.Errorf("failed to update UDN pod  %s/%s open ports: %w", pod.Namespace, pod.Name, err)
			}
		}
	}

	if kubevirt.IsPodLiveMigratable(pod) {
		v4Subnets, v6Subnets := util.GetClusterSubnetsWithHostPrefix()
		return kubevirt.EnsureDefaultNetworkForLocalMigratablePod(oc.watchFactory, oc.nbClient, oc.lsManager, pod, append(v4Subnets, v6Subnets...))
	}

	return nil
}

// ensureRemoteZonePod tries to set up remote zone pod bits required to interconnect it.
//   - For live-migratable VMs, ensures remote-zone pod-to-node routes
//
// It returns nil on success and error on failure; failure indicates the pod set up should be retried later.
func (oc *DefaultNetworkController) ensureRemoteZonePod(_, pod *corev1.Pod) error {
	if kubevirt.IsPodLiveMigratable(pod) {
		return kubevirt.EnsureRemoteZonePodAddressesToNodeRoute(oc.watchFactory, oc.nbClient, pod)
	}
	return nil
}

// removePod tries to tear down a pod. It returns nil on success and error on failure;
// failure indicates the pod tear down should be retried later.
func (oc *DefaultNetworkController) removePod(pod *corev1.Pod, portInfo *lpInfo) error {
	if err := kubevirt.CleanUpLiveMigratablePod(oc.nbClient, oc.watchFactory, pod); err != nil {
		return err
	}

	if oc.isPodScheduledOnLocalNode(pod) {
		return oc.removeLocalZonePod(pod, portInfo)
	}
	return oc.removeRemoteZonePod(pod)
}

// removeLocalZonePod tries to tear down a local zone pod. It returns nil on success and error on failure;
// failure indicates the pod tear down should be retried later.
func (oc *DefaultNetworkController) removeLocalZonePod(pod *corev1.Pod, portInfo *lpInfo) error {
	oc.logicalPortCache.remove(pod, ovntypes.DefaultNetworkName)

	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordPodEvent("delete", duration)
		}()
	}
	if util.PodWantsHostNetwork(pod) {
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
// It removes the remote pod ips from the namespace address set.
func (oc *DefaultNetworkController) removeRemoteZonePod(pod *corev1.Pod) error {
	// FIXME: there are other things we are probably leaving behind and should
	// be removed for completed VMs, like per-pod SNAT. Also
	// removeRemoteZonePodFromNamespaceAddressSet above should probably not be
	// called for migrations.
	// https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5627
	if kubevirt.IsPodLiveMigratable(pod) {
		allVMPodsAreCompleted, err := kubevirt.AllVMPodsAreCompleted(oc.watchFactory.PodCoreInformer().Lister(), pod)
		if err != nil {
			return err
		}

		if allVMPodsAreCompleted {
			ips, err := util.GetPodCIDRsWithFullMask(pod, oc.GetNetInfo(), nil)
			if err != nil && !errors.Is(err, util.ErrNoPodIPFound) {
				return fmt.Errorf("failed to get pod ips for the pod %s/%s: %w", pod.Namespace, pod.Name, err)
			}
			switchName, zoneContainsPodSubnet := kubevirt.ZoneContainsPodSubnet(oc.lsManager, ips)
			if zoneContainsPodSubnet {
				if err := oc.releasePodIPs(pod, ovntypes.DefaultNetworkName, &lpInfo{logicalSwitch: switchName, ips: ips}); err != nil {
					return err
				}
			}
		}
	}

	return nil
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
func (oc *DefaultNetworkController) syncNodeGateway(node *corev1.Node) error {
	gwConfig, err := oc.nodeGatewayConfig(node)
	if err != nil {
		return fmt.Errorf("error getting gateway config for node %s: %v", node.Name, err)
	}

	if err := oc.newGatewayManager(node.Name).SyncGateway(
		node,
		gwConfig,
	); err != nil {
		return fmt.Errorf("error creating gateway for node %s: %v", node.Name, err)
	}

	if util.IsPodNetworkAdvertisedAtNode(oc, node.Name) &&
		config.OVNKubernetesFeature.AdvertisedUDNIsolationMode == config.AdvertisedUDNIsolationModeStrict {
		return oc.addAdvertisedNetworkIsolation(node.Name)
	}
	return oc.deleteAdvertisedNetworkIsolation(node.Name)
}

// gatewayChanged compares the per-network gateway annotation between node
// revisions. Chassis changes are handled separately by callers that need them.
func gatewayChanged(oldNode, newNode *corev1.Node, oldState, newState *nodecontroller.NodeAnnotationState, netName string) bool {
	if oldState != nil && newState != nil {
		return nodecontroller.GatewayAnnotationChangedForNetworkWithState(oldState, newState, netName)
	}
	return oldNode.Annotations[util.OvnNodeL3GatewayConfig] != newNode.Annotations[util.OvnNodeL3GatewayConfig]
}

// hostCIDRsChanged compares old annotations to new and returns true if the something has changed.
func hostCIDRsChanged(oldNode, newNode *corev1.Node) bool {
	return util.NodeHostCIDRsAnnotationChanged(oldNode, newNode)
}

func primaryAddrChanged(oldNode, newNode *corev1.Node) bool {
	oldIP, _ := util.GetNodePrimaryIP(oldNode)
	newIP, _ := util.GetNodePrimaryIP(newNode)
	return oldIP != newIP
}

func nodeChassisChanged(oldNode, node *corev1.Node) bool {
	return util.NodeChassisIDAnnotationChanged(oldNode, node)
}

// nodeGatewayMTUSupportChanged returns true if annotation "k8s.ovn.org/gateway-mtu-support" on the node was updated.
func nodeGatewayMTUSupportChanged(oldNode, node *corev1.Node) bool {
	return oldNode.Annotations[util.OvnNodeGatewayMtuSupport] != node.Annotations[util.OvnNodeGatewayMtuSupport]
}

// shouldUpdateNode() determines if the ovn-kubernetes plugin should update the state of the node.
// ovn-kube should not perform an update if it does not assign a hostsubnet, or if you want to change
// whether or not ovn-kubernetes assigns a hostsubnet
func shouldUpdateNode(node, oldNode *corev1.Node) bool {
	newNoHostSubnet := util.NoHostSubnet(node)
	oldNoHostSubnet := util.NoHostSubnet(oldNode)

	if oldNoHostSubnet && newNoHostSubnet {
		return false
	}

	return true
}

func (oc *DefaultNetworkController) StartServiceController(wg *sync.WaitGroup, runRepair bool) error {
	useLBGroups := oc.clusterLoadBalancerGroupUUID != ""
	// use 5 workers like most of the kubernetes controllers in the
	// kubernetes controller-manager
	err := oc.svcController.Run(5, oc.stopChan, wg, runRepair, useLBGroups, oc.svcTemplateSupport)
	if err != nil {

		return fmt.Errorf("error running OVN-Kubernetes Services controller: %v", err)
	}
	return nil
}

func (oc *DefaultNetworkController) InitEgressServiceZoneController() (*egresssvc_zone.Controller, error) {
	// If the EgressIP controller is enabled it will take care of creating the
	// "no reroute" policies - we can pass "noop" functions to the egress service controller.
	initClusterEgressPolicies := func(_ libovsdbclient.Client, _ addressset.AddressSetFactory, _ util.NetInfo, _ []*net.IPNet, _, _ string) error {
		return nil
	}
	ensureNodeNoReroutePolicies := func(_ libovsdbclient.Client, _ addressset.AddressSetFactory, _, _, _ string, _ addressset.AddressSet, _, _ bool) error {
		return nil
	}
	// used only when IC=true
	createDefaultNodeRouteToExternal := func(_ libovsdbclient.Client, _, _ string, _ []config.CIDRNetworkEntry, _ []*net.IPNet) error {
		return nil
	}

	if !config.OVNKubernetesFeature.EnableEgressIP {
		initClusterEgressPolicies = func(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory,
			ni util.NetInfo, clusterSubnets []*net.IPNet, controllerName, routerName string) error {
			clusterNodeIPsAddrSetDbIDs, err := oc.addressSetManager.EnsureClusterNodeIPsAddressSet(addresssetmanager.ClusterNodeIPsEgressServiceBackRef)
			if err != nil {
				return fmt.Errorf("failed to ensure cluster node IP address set for EgressService: %w", err)
			}
			return InitClusterEgressPolicies(nbClient, addressSetFactory, ni, clusterSubnets, controllerName, routerName, clusterNodeIPsAddrSetDbIDs)
		}
		ensureNodeNoReroutePolicies = func(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory,
			network, router, controller string, clusterNodesAddressSets addressset.AddressSet, v4, v6 bool) error {
			return ensureDefaultNoRerouteNodePolicies(nbClient, addressSetFactory, network, router, controller, clusterNodesAddressSets, v4, v6)
		}
		createDefaultNodeRouteToExternal = libovsdbutil.CreateDefaultRouteToExternal
	}

	return egresssvc_zone.NewController(oc.GetNetInfo(), ovntypes.DefaultNetworkControllerName, oc.client, oc.nbClient, oc.addressSetFactory,
		oc.addressSetManager, initClusterEgressPolicies, ensureNodeNoReroutePolicies,
		createDefaultNodeRouteToExternal,
		oc.stopChan, oc.watchFactory.EgressServiceInformer(), oc.watchFactory.ServiceCoreInformer(),
		oc.watchFactory.EndpointSliceCoreInformer(),
		oc.watchFactory.NodeCoreInformer(), oc.nodeName)
}

func (oc *DefaultNetworkController) newANPController() error {
	var err error
	oc.anpController, err = anpcontroller.NewController(
		ovntypes.DefaultNetworkControllerName,
		oc.nbClient,
		oc.kube.ANPClient,
		oc.watchFactory.ANPInformer(),
		oc.watchFactory.BANPInformer(),
		oc.watchFactory.NamespaceCoreInformer(),
		oc.watchFactory.PodCoreInformer(),
		oc.watchFactory.NodeCoreInformer(),
		oc.addressSetFactory,
		oc.nodeName,
		oc.recorder,
		oc.observManager,
	)
	return err
}

func (oc *DefaultNetworkController) newNetworkConnectController() error {
	oc.networkConnectController = networkconnectcontroller.NewController(
		oc.nodeName,
		oc.nbClient,
		oc.watchFactory,
		oc.networkManager,
	)
	return nil
}

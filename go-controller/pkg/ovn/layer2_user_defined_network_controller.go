// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	svccontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	lsm "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/routeimport"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/topology"
	zoneinterconnect "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// method/structure shared by all layer 2 network controller, including localnet and layer2 network controllres.

type layer2UserDefinedNetworkControllerEventHandler struct {
	baseHandler  baseNetworkControllerEventHandler
	watchFactory *factory.WatchFactory
	objType      reflect.Type
	oc           *Layer2UserDefinedNetworkController
	syncFunc     func([]interface{}) error
}

func (h *layer2UserDefinedNetworkControllerEventHandler) FilterOutResource(obj interface{}) bool {
	return h.oc.FilterOutResource(h.objType, obj)
}

// FilterOutDeleteResource applies delete-specific filtering for the configured resource type.
func (h *layer2UserDefinedNetworkControllerEventHandler) FilterOutDeleteResource(obj interface{}) bool {
	return h.oc.FilterOutDeleteResource(h.objType, obj)
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *layer2UserDefinedNetworkControllerEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	return h.baseHandler.areResourcesEqual(h.objType, obj1, obj2)
}

// GetInternalCacheEntry returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (h *layer2UserDefinedNetworkControllerEventHandler) GetInternalCacheEntry(obj interface{}) interface{} {
	return h.oc.GetInternalCacheEntryForUserDefinedNetwork(h.objType, obj)
}

// GetResourceFromInformerCache returns the latest state of the object, given an object key and its type.
// from the informers cache.
func (h *layer2UserDefinedNetworkControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	return h.baseHandler.getResourceFromInformerCache(h.objType, h.watchFactory, key)
}

// RecordAddEvent records the add event on this given object.
func (h *layer2UserDefinedNetworkControllerEventHandler) RecordAddEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordUpdateEvent records the udpate event on this given object.
func (h *layer2UserDefinedNetworkControllerEventHandler) RecordUpdateEvent(obj interface{}) {
	h.baseHandler.recordUpdateEvent(h.objType, obj)
}

// RecordDeleteEvent records the delete event on this given object.
func (h *layer2UserDefinedNetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
	h.baseHandler.recordDeleteEvent(h.objType, obj)
}

// RecordSuccessEvent records the success event on this given object.
func (h *layer2UserDefinedNetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
	h.baseHandler.recordSuccessEvent(h.objType, obj)
}

// RecordErrorEvent records the error event on this given object.
func (h *layer2UserDefinedNetworkControllerEventHandler) RecordErrorEvent(_ interface{}, _ string, _ error) {
}

// IsResourceScheduled returns true if the given object has been scheduled.
// Only applied to pods for now. Returns true for all other types.
func (h *layer2UserDefinedNetworkControllerEventHandler) IsResourceScheduled(obj interface{}) bool {
	return h.baseHandler.isResourceScheduled(h.objType, obj)
}

// AddResource adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
// Given an object to add and a boolean specifying if the function was executed from iterateRetryResources
func (h *layer2UserDefinedNetworkControllerEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	_ = fromRetryLoop
	return h.oc.AddUserDefinedNetworkResourceCommon(h.objType, obj)
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *layer2UserDefinedNetworkControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	return h.oc.DeleteUserDefinedNetworkResourceCommon(h.objType, obj, cachedObj)
}

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (h *layer2UserDefinedNetworkControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	return h.oc.UpdateUserDefinedNetworkResourceCommon(h.objType, oldObj, newObj, inRetryCache)
}

func (h *layer2UserDefinedNetworkControllerEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.PodType:
			syncFunc = h.oc.syncPodsForUserDefinedNetwork

		case factory.NamespaceType:
			syncFunc = h.oc.syncNamespaces

		case factory.PolicyType:
			syncFunc = h.oc.syncNetworkPolicies

		case factory.MultiNetworkPolicyType:
			syncFunc = h.oc.syncMultiNetworkPolicies

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

// IsObjectInTerminalState returns true if the given object is a in terminal state.
// This is used now for pods that are either in a PodSucceeded or in a PodFailed state.
func (h *layer2UserDefinedNetworkControllerEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	return h.baseHandler.isObjectInTerminalState(h.objType, obj)
}

// Layer2UserDefinedNetworkController is created for logical network infrastructure and policy
// for a layer2 UDN
type Layer2UserDefinedNetworkController struct {
	BaseLayer2UserDefinedNetworkController

	// Node-specific syncMaps used by node event handler
	mgmtPortFailed              sync.Map
	gatewaysFailed              sync.Map
	syncZoneICFailed            sync.Map
	syncEIPNodeRerouteFailed    sync.Map
	nodeClusterRouterPortFailed sync.Map

	// Cluster-wide router default Control Plane Protection (COPP) UUID
	defaultCOPPUUID string

	gatewayManagers        sync.Map
	gatewayTopologyFactory *topology.GatewayTopologyFactory

	// Cluster wide Load_Balancer_Group UUID.
	// Includes the cluster switch and all node gateway routers.
	clusterLoadBalancerGroupUUID string

	// Cluster wide switch Load_Balancer_Group UUID.
	// Includes the cluster switch.
	switchLoadBalancerGroupUUID string

	// Cluster wide router Load_Balancer_Group UUID.
	// Includes all node gateway routers.
	routerLoadBalancerGroupUUID string

	// Shared controller in charge of services. Nil for cleanup-only controllers.
	svcController *svccontroller.Controller

	// EgressIP controller utilized only to initialize a network with OVN polices to support EgressIP functionality.
	eIPController *EgressIPController
}

// NewLayer2UserDefinedNetworkController create a new OVN controller for the given layer2 NAD
func NewLayer2UserDefinedNetworkController(
	cnci *CommonNetworkControllerInfo,
	netInfo util.NetInfo,
	networkManager networkmanager.Interface,
	routeImportManager routeimport.Manager,
	portCache *PortCache,
	eIPController *EgressIPController,
	addressSetManager *addresssetmanager.AddressSetManager,
	nodeReconciler *nodecontroller.NodeController,
	serviceController *svccontroller.Controller,
) (*Layer2UserDefinedNetworkController, error) {

	stopChan := make(chan struct{})

	ipv4Mode, ipv6Mode := netInfo.IPMode()
	addressSetFactory := addressset.NewOvnAddressSetFactory(cnci.nbClient, ipv4Mode, ipv6Mode)

	lsManager := lsm.NewL2SwitchManager()
	nodeAnnotationCache := nodecontroller.NewNodeAnnotationCache()
	if nodeReconciler != nil {
		nodeAnnotationCache = nodeReconciler.AnnotationCache()
	}
	if netInfo.IsPrimaryNetwork() {
		var gatewayIPs, mgmtIPs []*net.IPNet
		for _, subnet := range netInfo.Subnets() {
			if gwIP := netInfo.GetNodeGatewayIP(subnet.CIDR); gwIP != nil {
				gatewayIPs = append(gatewayIPs, gwIP)
			}
			if mgmtIP := netInfo.GetNodeManagementIP(subnet.CIDR); mgmtIP != nil {
				mgmtIPs = append(mgmtIPs, mgmtIP)
			}
		}

		lsManager = lsm.NewL2SwitchManagerForUserDefinedPrimaryNetwork(gatewayIPs, mgmtIPs)
	}

	oc := &Layer2UserDefinedNetworkController{
		BaseLayer2UserDefinedNetworkController: BaseLayer2UserDefinedNetworkController{

			BaseUserDefinedNetworkController: BaseUserDefinedNetworkController{
				BaseNetworkController: BaseNetworkController{
					CommonNetworkControllerInfo: *cnci,
					controllerName:              getNetworkControllerName(netInfo.GetNetworkName()),
					ReconcilableNetInfo:         util.NewReconcilableNetInfo(netInfo),
					lsManager:                   lsManager,
					logicalPortCache:            portCache,
					namespaces:                  make(map[string]*namespaceInfo),
					namespacesMutex:             sync.Mutex{},
					addressSetFactory:           addressSetFactory,
					networkPolicies:             syncmap.NewSyncMap[*networkPolicy](),
					sharedNetpolPortGroups:      syncmap.NewSyncMap[*defaultDenyPortGroups](),
					stopChan:                    stopChan,
					wg:                          &sync.WaitGroup{},
					cancelableCtx:               util.NewCancelableContext(),
					networkManager:              networkManager,
					routeImportManager:          routeImportManager,
					addressSetManager:           addressSetManager,
					nodeReconciler:              nodeReconciler,
					nodeAnnotationCache:         nodeAnnotationCache,
				},
			},
		},
		mgmtPortFailed:         sync.Map{},
		syncZoneICFailed:       sync.Map{},
		gatewayTopologyFactory: topology.NewGatewayTopologyFactory(cnci.nbClient),
		gatewayManagers:        sync.Map{},
		eIPController:          eIPController,
	}
	if oc.IsPrimaryNetwork() {
		oc.onLogicalPortCacheAdd = func(pod *corev1.Pod, _ string) {
			oc.requestLocalPodPolicyRetriesForPod(pod, "logical port cache update")
			if oc.eIPController != nil {
				if err := oc.eIPController.addEgressIPPodRetry(pod, "logical port cache update"); err != nil {
					klog.Warningf("Failed to enqueue EgressIP retry for pod %s/%s after logical port cache update: %v",
						pod.Namespace, pod.Name, err)
				}
			}
		}
	}

	oc.zoneICHandler = zoneinterconnect.NewZoneInterconnectHandler(oc.GetNetInfo(), oc.nbClient, oc.sbClient, oc.watchFactory)

	if util.IsNetworkSegmentationSupportEnabled() && netInfo.IsPrimaryNetwork() {
		oc.svcController = serviceController
	}

	if oc.allocatesPodAnnotation() {
		oc.podAnnotationAllocator = pod.NewPodAnnotationAllocator(
			oc.GetNetInfo(),
			cnci.watchFactory.PodCoreInformer().Lister(),
			cnci.kube,
			nil)
	}

	// enable multicast support for UDN only for primaries + multicast enabled
	// TBD: changes needs to be made to support multicast beyond primary UDN
	oc.multicastSupport = oc.IsPrimaryNetwork() && util.IsNetworkSegmentationSupportEnabled() && config.EnableMulticast

	oc.initRetryFramework()
	return oc, nil
}

// Start starts the layer2 UDN controller, handles all events and creates all needed logical entities
func (oc *Layer2UserDefinedNetworkController) Start(_ context.Context) error {
	klog.Infof("Starting controller for UDN %s", oc.GetNetworkName())

	start := time.Now()
	defer func() {
		klog.Infof("Starting controller for UDN %s took %v", oc.GetNetworkName(), time.Since(start))
	}()

	if err := oc.init(); err != nil {
		return err
	}
	if err := oc.RegisterNodeHandler(); err != nil {
		return err
	}
	if err := oc.run(); err != nil {
		oc.DeregisterServiceNetwork()
		oc.DeregisterNodeHandler()
		return err
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) run() error {
	err := oc.BaseLayer2UserDefinedNetworkController.run()
	if err != nil {
		return err
	}
	oc.enqueueEgressIPNetwork()
	if oc.svcController != nil {
		startSvc := time.Now()

		err := oc.RegisterServiceNetwork(true)
		endSvc := time.Since(startSvc)

		metrics.MetricOVNKubeControllerSyncDuration.WithLabelValues("service_" + oc.GetNetworkName()).Set(endSvc.Seconds())
		if err != nil {
			return err
		}
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) enqueueEgressIPNetwork() {
	if !config.OVNKubernetesFeature.EnableEgressIP || !util.IsNetworkSegmentationSupportEnabled() ||
		!oc.IsPrimaryNetwork() || oc.eIPController == nil {
		return
	}
	oc.eIPController.EnqueueNetwork(oc.GetNetworkName())
}

func (oc *Layer2UserDefinedNetworkController) unregisterEgressIPNetwork() {
	if !config.OVNKubernetesFeature.EnableEgressIP || !util.IsNetworkSegmentationSupportEnabled() ||
		!oc.IsPrimaryNetwork() || oc.eIPController == nil {
		return
	}
	oc.eIPController.UnregisterNetwork(oc.GetNetworkName())
}

func (oc *Layer2UserDefinedNetworkController) cleanupEgressIPAddressSet() error {
	if !config.OVNKubernetesFeature.EnableEgressIP || !util.IsNetworkSegmentationSupportEnabled() ||
		!oc.IsPrimaryNetwork() || oc.eIPController == nil {
		return nil
	}
	return oc.eIPController.DestroyNetworkServedPodsAddressSet(oc.GetNetworkName())
}

// Cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *Layer2UserDefinedNetworkController) Cleanup() error {
	networkName := oc.GetNetworkName()
	oc.unregisterEgressIPNetwork()

	// For primary Layer2 UDN only: when this is a cleanup-only controller (dummy for stale UDN
	// cleanup; GetNetworkID() is InvalidID because netInfo was never reconciled from a NAD),
	// discover and cleanup all gateway routers from the NB DB. DB-driven cleanup works even
	// when nodes are already gone.
	if oc.IsPrimaryNetwork() && oc.GetNetworkID() == types.InvalidID {
		if err := cleanupGatewayRoutersForNetworkFromDB(oc.nbClient, oc.GetNetInfo(),
			oc.GetNetworkScopedClusterRouterName(), oc.GetNetworkScopedJoinSwitchName()); err != nil {
			return fmt.Errorf("failed to cleanup gateway routers for network %s: %w", networkName, err)
		}
	}

	// Switch that holds management ports is deleted below (BaseLayer2UserDefinedNetworkController.cleanup);
	// LSPs are cascade-deleted with the logical switch.
	if err := oc.BaseLayer2UserDefinedNetworkController.cleanup(); err != nil {
		return fmt.Errorf("failed to cleanup network %q: %w", networkName, err)
	}

	oc.gatewayManagers.Range(func(nodeName, value any) bool {
		gwManager, isGWManagerType := value.(*GatewayManager)
		if !isGWManagerType {
			klog.Errorf(
				"Failed to cleanup GW manager for network %q on node %s: could not retrieve GWManager",
				networkName,
				nodeName,
			)
			return true
		}
		if err := gwManager.Cleanup(); err != nil {
			klog.Errorf("Failed to cleanup GW manager for network %q on node %s: %v", networkName, nodeName, err)
		}
		return true
	})

	// now delete cluster router
	if config.Layer2UsesTransitRouter {
		ops, err := libovsdbops.DeleteLogicalRouterOps(oc.nbClient, nil,
			&nbdb.LogicalRouter{
				Name: oc.GetNetworkScopedClusterRouterName(),
			})
		if err != nil {
			return fmt.Errorf("failed to get ops for deleting routers of network %s: %v", oc.GetNetworkName(), err)
		}
		_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to deleting routers/switches of network %s: %v", oc.GetNetworkName(), err)
		}
	}

	// Delete all load balancers owned by this network to prevent orphaned LBs
	// When network is deleted, all switches and routers are also deleted, but load balancers are not.
	// Load_Balancer is a root table in OVSDB, so rows are never auto-deleted
	// when the switch or LB group referencing them is removed.
	lbs, err := libovsdbops.FindLoadBalancersWithPredicate(oc.nbClient, func(lb *nbdb.LoadBalancer) bool {
		return lb.ExternalIDs[types.NetworkExternalID] == networkName
	})
	if err != nil {
		klog.Errorf("Failed to find load balancers for network %q: %v", networkName, err)
	} else if len(lbs) > 0 {
		if err := libovsdbops.DeleteLoadBalancers(oc.nbClient, lbs); err != nil {
			klog.Errorf("Failed to delete load balancers on network %q: %v", networkName, err)
		}
	}

	// remove load balancer groups
	cleanupLoadBalancerGroups(oc.nbClient, oc.GetNetInfo(),
		oc.switchLoadBalancerGroupUUID, oc.clusterLoadBalancerGroupUUID, oc.routerLoadBalancerGroupUUID)

	// Remove the EgressIP served-pods address set after router/policy teardown;
	// it is not covered by this network's own address-set cleanup.
	if err := oc.cleanupEgressIPAddressSet(); err != nil {
		return err
	}

	return nil
}

func (oc *Layer2UserDefinedNetworkController) init() (err error) {
	start := time.Now()
	defer func() {
		if err == nil && config.Metrics.EnableScaleMetrics {
			duration := time.Since(start)
			metrics.RecordUDNNBDBProgrammedDuration(oc.TopologyType(), duration.Seconds())
			klog.Infof("Recorded NBDB programmed duration for network %s: %v", oc.GetNetworkName(), duration)
		}
	}()

	// Create default Control Plane Protection (COPP) entry for routers
	defaultCOPPUUID, err := EnsureDefaultCOPP(oc.nbClient)
	if err != nil {
		return fmt.Errorf("unable to create router control plane protection: %w", err)
	}
	oc.defaultCOPPUUID = defaultCOPPUUID

	if config.Layer2UsesTransitRouter && oc.IsPrimaryNetwork() {
		if _, err = oc.newClusterRouter(); err != nil {
			return fmt.Errorf("failed to create OVN cluster router for network %q: %v", oc.GetNetworkName(), err)
		}
	}

	clusterLBGroupUUID, switchLBGroupUUID, routerLBGroupUUID, err := initLoadBalancerGroups(oc.nbClient, oc.GetNetInfo())
	if err != nil {
		return err
	}
	oc.clusterLoadBalancerGroupUUID = clusterLBGroupUUID
	oc.switchLoadBalancerGroupUUID = switchLBGroupUUID
	oc.routerLoadBalancerGroupUUID = routerLBGroupUUID
	excludeSubnets := oc.ExcludeSubnets()
	excludeSubnets = append(excludeSubnets, oc.InfrastructureSubnets()...)

	// Configure cluster port groups and multicast default policies for user defined primary networks.
	if oc.IsPrimaryNetwork() && util.IsNetworkSegmentationSupportEnabled() {
		if err := oc.setupClusterPortGroups(); err != nil {
			return fmt.Errorf("failed to create cluster port groups for network %q: %w", oc.GetNetworkName(), err)
		}

		if err := oc.syncDefaultMulticastPolicies(); err != nil {
			return fmt.Errorf("failed to sync default multicast policies for network %q: %w", oc.GetNetworkName(), err)
		}
	}

	_, err = oc.initializeLogicalSwitch(
		oc.GetNetworkScopedSwitchName(""),
		oc.Subnets(),
		excludeSubnets,
		oc.ReservedSubnets(),
		oc.clusterLoadBalancerGroupUUID,
		oc.switchLoadBalancerGroupUUID,
	)
	if err != nil {
		return err
	}

	return nil
}

func (oc *Layer2UserDefinedNetworkController) Stop() {
	klog.Infof("Stopping controller for UDN %s", oc.GetNetworkName())
	oc.unregisterEgressIPNetwork()
	oc.DeregisterServiceNetwork()
	oc.DeregisterNodeHandler()
	oc.BaseLayer2UserDefinedNetworkController.stop()
}

func (oc *Layer2UserDefinedNetworkController) Reconcile(netInfo util.NetInfo) error {
	if err := oc.BaseNetworkController.reconcile(
		netInfo,
		func(node string) { oc.gatewaysFailed.Store(node, true) },
	); err != nil {
		return err
	}
	return oc.ReconcileServiceNetwork()
}

func (oc *Layer2UserDefinedNetworkController) RegisterNodeHandler() error {
	return oc.nodeReconciler.RegisterNetworkController(oc)
}

// MarkGatewaySyncNeeded marks gateway state dirty so the next node
// reconciliation syncs it even when node annotations did not change.
func (oc *Layer2UserDefinedNetworkController) MarkGatewaySyncNeeded(nodeName string) {
	oc.gatewaysFailed.Store(nodeName, true)
}

// ReconcileNode reconciles a node for a layer2 UDN controller.
func (oc *Layer2UserDefinedNetworkController) ReconcileNode(oldNode, newNode *corev1.Node, oldState, newState *nodecontroller.NodeAnnotationState) error {
	if newNode == nil {
		if oldNode == nil {
			return fmt.Errorf("nil node received for network %s", oc.GetNetworkName())
		}
		return oc.deleteNodeEvent(oldNode)
	}

	if oc.isLocalNode(newNode) {
		var nodeParams *nodeSyncs
		if oldNode == nil {
			_, syncMgmtPort := oc.mgmtPortFailed.Load(newNode.Name)
			_, syncGw := oc.gatewaysFailed.Load(newNode.Name)
			_, syncReroute := oc.syncEIPNodeRerouteFailed.Load(newNode.Name)
			_, syncClusterRouterPort := oc.nodeClusterRouterPortFailed.Load(newNode.Name)
			if syncMgmtPort || syncGw || syncReroute || syncClusterRouterPort {
				nodeParams = &nodeSyncs{
					syncMgmtPort:          syncMgmtPort,
					syncGw:                syncGw,
					syncReroute:           syncReroute,
					syncClusterRouterPort: syncClusterRouterPort,
				}
			} else {
				nodeParams = &nodeSyncs{
					syncMgmtPort:          true,
					syncGw:                true,
					syncReroute:           true,
					syncClusterRouterPort: true,
				}
			}
		} else {
			nodeSubnetChange := nodeSubnetChangedForUDN(oldNode, newNode, oc.GetNetworkName(), oldState, newState)
			_, mgmtUpdateFailed := oc.mgmtPortFailed.Load(newNode.Name)
			shouldSyncMgmtPort := mgmtUpdateFailed || nodeSubnetChange
			_, gwUpdateFailed := oc.gatewaysFailed.Load(newNode.Name)
			shouldSyncGW := gwUpdateFailed ||
				gatewayChanged(oldNode, newNode, oldState, newState, oc.GetNetworkName()) ||
				nodeChassisChanged(oldNode, newNode) ||
				hostCIDRsChanged(oldNode, newNode) ||
				nodeGatewayMTUSupportChanged(oldNode, newNode)
			_, syncRerouteFailed := oc.syncEIPNodeRerouteFailed.Load(newNode.Name)
			shouldSyncReroute := syncRerouteFailed || util.NodeHostCIDRsAnnotationChanged(oldNode, newNode)
			_, clusterRouterPortFailed := oc.nodeClusterRouterPortFailed.Load(newNode.Name)
			nodeParams = &nodeSyncs{
				syncMgmtPort:          shouldSyncMgmtPort,
				syncGw:                shouldSyncGW,
				syncReroute:           shouldSyncReroute,
				syncClusterRouterPort: clusterRouterPortFailed,
			}
		}
		return oc.addUpdateLocalNodeEvent(newNode, nodeParams)
	}

	if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		if !oc.networkManager.NodeHasNetwork(newNode.Name, oc.GetNetworkName()) {
			klog.V(5).Infof("Ignoring processing remote node: %s as it has no active NAD for network: %s",
				newNode.Name, oc.GetNetworkName())
			// store sync IC failed for the node, so if on node update if the NAD is no longer filtered, we actually
			// process it
			oc.syncZoneICFailed.Store(newNode.Name, true)
			return nil
		}
	}

	if oldNode == nil {
		return oc.addUpdateRemoteNodeEvent(newNode, true, newState)
	}

	_, syncZoneIC := oc.syncZoneICFailed.Load(newNode.Name)
	if util.NodeChassisIDAnnotationChanged(oldNode, newNode) {
		syncZoneIC = true
	}
	if nodecontroller.TunnelIDAnnotationChangedForNetworkWithState(oldState, newState, oc.GetNetworkName()) {
		syncZoneIC = true
	}
	return oc.addUpdateRemoteNodeEvent(newNode, syncZoneIC, newState)
}

// SyncNodes runs the node sync for a layer2 UDN controller.
func (oc *Layer2UserDefinedNetworkController) SyncNodes(nodes []*corev1.Node) error {
	return oc.syncNodes(nodesToInterfaces(nodes))
}

func (oc *Layer2UserDefinedNetworkController) initRetryFramework() {
	oc.retryPods = oc.newRetryFramework(factory.PodType)

	// When a user-defined network is enabled as a primary network for namespace,
	// then watch for namespace and network policy events.
	if oc.IsPrimaryNetwork() {
		oc.retryNamespaces = oc.newRetryFramework(factory.NamespaceType)
		oc.retryNetworkPolicies = oc.newRetryFramework(factory.PolicyType)
	}

	// For secondary networks, we don't have to watch namespace events if
	// multi-network policy support is not enabled. We don't support
	// multi-network policy for IPAM-less secondary networks either.
	if util.IsMultiNetworkPoliciesSupportEnabled() {
		oc.retryNamespaces = oc.newRetryFramework(factory.NamespaceType)
		oc.retryMultiNetworkPolicies = oc.newRetryFramework(factory.MultiNetworkPolicyType)
	}
}

// newRetryFramework builds and returns a retry framework for the input resource type;
func (oc *Layer2UserDefinedNetworkController) newRetryFramework(
	objectType reflect.Type) *retry.RetryFramework {
	eventHandler := &layer2UserDefinedNetworkControllerEventHandler{
		baseHandler:  baseNetworkControllerEventHandler{},
		objType:      objectType,
		watchFactory: oc.watchFactory,
		oc:           oc,
		syncFunc:     nil,
	}
	resourceHandler := &retry.ResourceHandler{
		HasUpdateFunc:          hasResourceAnUpdateFunc(objectType),
		NeedsUpdateDuringRetry: needsUpdateDuringRetry(objectType),
		ObjType:                objectType,
		EventHandler:           eventHandler,
	}
	return retry.NewRetryFramework(
		oc.GetNetworkName()+"/networkController",
		oc.stopChan,
		oc.wg,
		oc.watchFactory,
		resourceHandler,
	)
}

func (oc *Layer2UserDefinedNetworkController) addUpdateLocalNodeEvent(node *corev1.Node, nSyncs *nodeSyncs) error {
	var errs []error
	var err error

	hostSubnets := make([]*net.IPNet, 0, len(oc.Subnets()))
	for _, subnet := range oc.Subnets() {
		hostSubnets = append(hostSubnets, subnet.CIDR)
	}

	if util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() {
		if nSyncs.syncClusterRouterPort && config.Layer2UsesTransitRouter {
			if err = oc.syncClusterRouterPorts(node, hostSubnets); err != nil {
				errs = append(errs, err)
				oc.nodeClusterRouterPortFailed.Store(node.Name, true)
			} else {
				oc.nodeClusterRouterPortFailed.Delete(node.Name)
			}
		}
		if nSyncs.syncGw {
			gwManager := oc.gatewayManagerForNode(node.Name)
			oc.gatewayManagers.Store(node.Name, gwManager)

			err := func() error {
				gwConfig, err := oc.nodeGatewayConfig(node)
				if err != nil {
					return err
				}
				if err := gwManager.SyncGateway(
					node,
					gwConfig,
				); err != nil {
					return err
				}
				isUDNAdvertised := util.IsPodNetworkAdvertisedAtNode(oc, node.Name)
				err = oc.addOrUpdateUDNClusterSubnetEgressSNAT(gwConfig.hostSubnets, node.Name, isUDNAdvertised)
				if err != nil {
					return err
				}
				shouldIsolate := isUDNAdvertised && config.OVNKubernetesFeature.AdvertisedUDNIsolationMode == config.AdvertisedUDNIsolationModeStrict
				if shouldIsolate {
					if err = oc.addAdvertisedNetworkIsolation(node.Name); err != nil {
						return err
					}
				} else {
					if err = oc.deleteAdvertisedNetworkIsolation(node.Name); err != nil {
						return err
					}
				}
				oc.gatewaysFailed.Delete(node.Name)
				return nil
			}()

			if err != nil {
				errs = append(errs, err)
				oc.gatewaysFailed.Store(node.Name, true)
			}
		}

		if nSyncs.syncMgmtPort {
			routerName := oc.GetNetworkScopedClusterRouterName()
			if !config.Layer2UsesTransitRouter {
				routerName = oc.GetNetworkScopedGWRouterName(node.Name)
			}
			if _, err := oc.syncNodeManagementPort(node, oc.GetNetworkScopedSwitchName(""),
				routerName, hostSubnets); err != nil {
				errs = append(errs, err)
				oc.mgmtPortFailed.Store(node.Name, true)
			} else {
				oc.mgmtPortFailed.Delete(node.Name)
			}
		}

		if config.OVNKubernetesFeature.EnableEgressIP && nSyncs.syncReroute {
			rerouteFailed := false
			if err := oc.eIPController.ensureRouterPoliciesForNetwork(oc.GetNetInfo(), node); err != nil {
				errs = append(errs, fmt.Errorf("failed to ensure EgressIP router policies for network %s: %v", oc.GetNetworkName(), err))
				rerouteFailed = true
			}
			if err := oc.eIPController.ensureSwitchPoliciesForNode(oc.GetNetInfo(), node.Name); err != nil {
				errs = append(errs, fmt.Errorf("failed to ensure EgressIP switch policies for network %s: %v", oc.GetNetworkName(), err))
				rerouteFailed = true
			}
			if rerouteFailed {
				oc.syncEIPNodeRerouteFailed.Store(node.Name, true)
			} else {
				oc.syncEIPNodeRerouteFailed.Delete(node.Name)
			}
		}
	}

	err = utilerrors.Join(errs...)
	if err != nil {
		oc.recordNodeErrorEvent(node, err)
	}
	return err
}

func (oc *Layer2UserDefinedNetworkController) addUpdateRemoteNodeEvent(node *corev1.Node, syncZoneIC bool, state *nodecontroller.NodeAnnotationState) error {
	var errs []error

	if util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() {
		if syncZoneIC && oc.hasInterconnectTransport() {
			err := oc.addInterconnectSetupForRemoteNode(node, state)
			if err != nil {
				errs = append(errs, err)
				oc.syncZoneICFailed.Store(node.Name, true)
			} else {
				oc.syncZoneICFailed.Delete(node.Name)
			}
		}
	}

	err := utilerrors.Join(errs...)
	if err != nil {
		oc.recordNodeErrorEvent(node, err)
	}
	return err
}

func (oc *Layer2UserDefinedNetworkController) addInterconnectSetupForRemoteNode(node *corev1.Node, state *nodecontroller.NodeAnnotationState) error {
	var err error
	if config.Layer2UsesTransitRouter {
		err = oc.addRouterSetupForRemoteNodeGR(node)
	} else {
		err = oc.addSwitchPortForRemoteNodeGR(node, state)
	}
	if err != nil {
		return fmt.Errorf("failed to add the remote zone node %s's remote port: %w", node.Name, err)
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) tunnelIDForNode(state *nodecontroller.NodeAnnotationState) (int, error) {
	return state.TunnelID(oc.GetNetworkName())
}

func (oc *Layer2UserDefinedNetworkController) addSwitchPortForRemoteNodeGR(node *corev1.Node, state *nodecontroller.NodeAnnotationState) error {
	nodeJoinSubnetIPs, err := udn.GetGWRouterIPs(node, oc.GetNetInfo())
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// remote node may not have the annotation yet, suppress it
			return types.NewSuppressedError(err)
		}
		return fmt.Errorf("failed to get the node %s join subnet IPs: %w", node.Name, err)
	}
	if len(nodeJoinSubnetIPs) == 0 {
		return fmt.Errorf("annotation on the node %s had empty join subnet IPs", node.Name)
	}

	remoteGRPortMac := util.IPAddrToHWAddr(nodeJoinSubnetIPs[0].IP)
	var remoteGRPortNetworks []string
	for _, ip := range nodeJoinSubnetIPs {
		remoteGRPortNetworks = append(remoteGRPortNetworks, ip.String())
	}

	remotePortAddr := remoteGRPortMac.String() + " " + strings.Join(remoteGRPortNetworks, " ")
	klog.V(5).Infof("The remote port addresses for node %s in network %s are %s", node.Name, oc.GetNetworkName(), remotePortAddr)
	sw := nbdb.LogicalSwitch{Name: oc.GetNetworkScopedSwitchName("")}
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      types.SwitchToRouterPrefix + sw.Name + "_" + node.Name,
		Type:      "remote",
		Addresses: []string{remotePortAddr},
	}
	logicalSwitchPort.ExternalIDs = map[string]string{
		types.NetworkExternalID:  oc.GetNetworkName(),
		types.TopologyExternalID: oc.TopologyType(),
		types.NodeExternalID:     node.Name,
	}
	tunnelID, err := oc.tunnelIDForNode(state)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// remote node may not have the annotation yet, suppress it
			return types.NewSuppressedError(err)
		}
		// Don't consider this node as cluster-manager has not allocated node id yet.
		return fmt.Errorf("failed to fetch tunnelID annotation from the node %s for network %s, err: %w",
			node.Name, oc.GetNetworkName(), err)
	}

	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// remote node may not have the annotation yet, suppress it
			return types.NewSuppressedError(err)
		}
		return fmt.Errorf("failed to parse node chassis-id for node %s: %w", node.Name, err)
	}

	logicalSwitchPort.Options = map[string]string{
		libovsdbops.RequestedTnlKey:  strconv.Itoa(tunnelID),
		libovsdbops.RequestedChassis: chassisID,
	}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return fmt.Errorf("failed to create port %v on logical switch %q: %v", logicalSwitchPort, sw.Name, err)
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) addRouterSetupForRemoteNodeGR(node *corev1.Node) error {
	transitRouterInfo, err := getTransitRouterInfo(oc.GetNetInfo(), node)
	if err != nil {
		return nil
	}

	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// remote node may not have the annotation yet, suppress it
			return types.NewSuppressedError(err)
		}
		return fmt.Errorf("failed to parse node chassis-id for node %s: %w", node.Name, err)
	}

	transitPort := nbdb.LogicalRouterPort{
		Name:     types.TransitRouterToRouterPrefix + oc.GetNetworkScopedGWRouterName(node.Name),
		MAC:      util.IPAddrToHWAddr(transitRouterInfo.transitRouterNets[0].IP).String(),
		Networks: util.IPNetsToStringSlice(transitRouterInfo.transitRouterNets),
		Options: map[string]string{
			libovsdbops.RequestedTnlKey:  getTransitRouterPortTunnelKey(transitRouterInfo.nodeID),
			libovsdbops.RequestedChassis: chassisID,
		},
		ExternalIDs: map[string]string{
			types.NetworkExternalID:  oc.GetNetworkName(),
			types.TopologyExternalID: oc.TopologyType(),
			types.NodeExternalID:     node.Name,
		},
	}
	transitRouter := nbdb.LogicalRouter{Name: oc.GetNetworkScopedClusterRouterName()}
	if err := libovsdbops.CreateOrUpdateLogicalRouterPort(oc.nbClient, &transitRouter,
		&transitPort, nil, &transitPort.MAC, &transitPort.Networks,
		&transitPort.Options, &transitPort.ExternalIDs); err != nil {
		return fmt.Errorf("failed to create remote port %+v on router %+v: %v", transitPort, transitRouter, err)
	}
	return oc.addTransitRouterRoutes(node, transitRouterInfo.gatewayRouterNets)
}

func (oc *Layer2UserDefinedNetworkController) addTransitRouterRoutes(node *corev1.Node, nextHops []*net.IPNet) error {
	gwRouterJoinIPs, err := udn.GetGWRouterIPs(node, oc.GetNetInfo())
	if err != nil {
		return err
	}
	for _, nextHop := range nextHops {
		gwRouterJoinIP, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(nextHop), gwRouterJoinIPs)
		if err != nil {
			return fmt.Errorf("failed to add remote node join ip based "+
				"routes in distributed router %s: %v",
				oc.GetNetworkScopedClusterRouterName(), err)
		}
		lrsr := nbdb.LogicalRouterStaticRoute{
			ExternalIDs: map[string]string{
				types.NodeExternalID:     node.Name,
				types.NetworkExternalID:  oc.GetNetworkName(),
				types.TopologyExternalID: oc.TopologyType(),
			},
			IPPrefix: gwRouterJoinIP.IP.String(),
			Nexthop:  nextHop.IP.String(),
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.IPPrefix == lrsr.IPPrefix &&
				libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
		}

		if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(oc.nbClient,
			oc.GetNetworkScopedClusterRouterName(), &lrsr, p, &lrsr.Nexthop); err != nil {
			return fmt.Errorf("error creating static route %+v in %s: %v", lrsr, oc.GetNetworkScopedClusterRouterName(), err)
		}
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) delPortForRemoteNodeGR(node *corev1.Node) error {
	swName := oc.GetNetworkScopedSwitchName("")
	sw := &nbdb.LogicalSwitch{Name: swName}
	logicalSwitchPort := &nbdb.LogicalSwitchPort{
		Name: types.SwitchToRouterPrefix + swName + "_" + node.Name,
	}
	if err := libovsdbops.DeleteLogicalSwitchPorts(oc.nbClient, sw, logicalSwitchPort); err != nil {
		return fmt.Errorf("failed to delete remote GR switch port %q from layer 2 switch %q for the node %q, error: %w",
			logicalSwitchPort.Name, swName, node.Name, err)
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) cleanupRouterSetupForRemoteNodeGR(nodeName string) error {
	transitPort := &nbdb.LogicalRouterPort{
		Name: types.TransitRouterToRouterPrefix + oc.GetNetworkScopedGWRouterName(nodeName),
	}
	var err error
	transitPort, err = libovsdbops.GetLogicalRouterPort(oc.nbClient, transitPort)
	if err != nil {
		// logical router port doesn't exist. So nothing to cleanup.
		return nil
	}

	transitRouter := nbdb.LogicalRouter{
		Name: oc.GetNetworkScopedClusterRouterName(),
	}

	if err = libovsdbops.DeleteLogicalRouterPorts(oc.nbClient, &transitRouter, transitPort); err != nil {
		return fmt.Errorf("failed to delete logical router port %s from router %s for the node %s, error: %w",
			transitPort.Name, transitRouter.Name, nodeName, err)
	}

	// Delete any static routes in the transit router for this node.
	p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
		return lrsr.ExternalIDs[types.NetworkExternalID] == oc.GetNetworkName() && lrsr.ExternalIDs[types.NodeExternalID] == nodeName
	}
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(oc.nbClient, oc.GetNetworkScopedClusterRouterName(), p); err != nil {
		return fmt.Errorf("failed to cleanup static routes for the node %s: %w", nodeName, err)
	}

	return nil
}

func (oc *Layer2UserDefinedNetworkController) deleteNodeEvent(node *corev1.Node) error {
	if oc.isLocalNode(node) {
		if util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() {
			if err := oc.gatewayManagerForNode(node.Name).Cleanup(); err != nil {
				return fmt.Errorf("failed to cleanup gateway on node %q: %w", node.Name, err)
			}
			oc.gatewayManagers.Delete(node.Name)
		}
	} else if oc.hasInterconnectTransport() {
		if err := oc.cleanupInterconnectSetupForRemoteNode(node); err != nil {
			return err
		}
	}
	oc.mgmtPortFailed.Delete(node.Name)
	oc.syncEIPNodeRerouteFailed.Delete(node.Name)
	oc.syncZoneICFailed.Delete(node.Name)
	return nil
}

func (oc *Layer2UserDefinedNetworkController) cleanupInterconnectSetupForRemoteNode(node *corev1.Node) error {
	var err error
	if config.Layer2UsesTransitRouter {
		err = oc.cleanupRouterSetupForRemoteNodeGR(node.Name)
	} else {
		err = oc.delPortForRemoteNodeGR(node)
	}
	if err != nil {
		return fmt.Errorf("failed to cleanup the remote zone node %s's remote port: %w", node.Name, err)
	}
	return nil
}

// addOrUpdateUDNClusterSubnetEgressSNAT adds or updates the SNAT on each node's GR in L2 networks for each UDN
// Based on the isUDNAdvertised flag, the SNAT matches are slightly different
// snat eth.dst == d6:cf:fd:2c:a6:44 169.254.0.12 10.128.0.0/14
// snat eth.dst == d6:cf:fd:2c:a6:44 169.254.0.12 2010:100:200::/64
// these SNATs are required for pod2Egress traffic in LGW mode and pod2SameNode traffic in SGW mode to function properly on UDNs
// SNAT Breakdown:
// match = "eth.dst == d6:cf:fd:2c:a6:44"; the MAC here is the mpX interface MAC address for this UDN
// logicalIP = "10.128.0.0/14"; which is the clustersubnet for this L2 UDN
// externalIP = "169.254.0.12"; which is the masqueradeIP for this L2 UDN
// so all in all we want to condionally SNAT all packets that are coming from pods hosted on this node,
// which are leaving via UDN's mpX interface to the UDN's masqueradeIP.
// If isUDNAdvertised is true, then we want to SNAT packets from pods on this network
// leaving towards cluster node IPs and UDN-enabled service IPs to the masqueradeIP.
// In shared gateway mode, advertised SNAT keeps NAT.match empty and uses
// NAT.allowed_ext_ips to point directly at those address sets. Local gateway mode
// keeps the destination address-set checks in NAT.match, for example:
// "eth.dst == 0a:58:5d:5d:00:02 && (ip4.dst == $a712973235162149816)" "169.254.0.36" "93.93.0.0/16"
func (oc *Layer2UserDefinedNetworkController) addOrUpdateUDNClusterSubnetEgressSNAT(localPodSubnets []*net.IPNet,
	nodeName string, isUDNAdvertised bool) error {
	outputPort := oc.GetNetworkScopedRouterToSwitchPortName("")
	routerName := oc.GetNetworkScopedClusterRouterName()
	if !config.Layer2UsesTransitRouter {
		routerName = oc.GetNetworkScopedGWRouterName(nodeName)
		outputPort = types.GWRouterToJoinSwitchPrefix + routerName
	}
	nats, err := oc.buildUDNEgressSNAT(localPodSubnets, outputPort, isUDNAdvertised)
	if err != nil {
		return err
	}
	if len(nats) == 0 {
		return nil // nothing to do
	}
	router := &nbdb.LogicalRouter{
		Name: routerName,
	}
	if err := libovsdbops.CreateOrUpdateNATs(oc.nbClient, router, nats...); err != nil {
		return fmt.Errorf("failed to update SNAT for cluster on router: %q for network %q, error: %w",
			routerName, oc.GetNetworkName(), err)
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) nodeGatewayConfig(node *corev1.Node) (*GatewayConfig, error) {
	networkName := oc.GetNetworkName()
	l3GatewayConfig, hasUplink, err := oc.uplinkGatewayConfig(node)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s network %s Uplink gateway config: %w", node.Name, networkName, err)
	}
	if !hasUplink {
		l3GatewayConfig, err = util.ParseNodeL3GatewayAnnotation(node)
		if err != nil {
			return nil, fmt.Errorf("failed to get node %s network %s L3 gateway config: %w", node.Name, networkName, err)
		}
	}

	networkID := oc.GetNetworkID()

	masqIPs, err := udn.GetUDNGatewayMasqueradeIPs(networkID)
	if err != nil {
		return nil, fmt.Errorf("failed to get masquerade IPs, network %s (%d): %v", networkName, networkID, err)
	}

	for _, masqIP := range masqIPs {
		hostMask := util.GetIPFullMask(masqIP.IP)
		l3GatewayConfig.IPAddresses = append(l3GatewayConfig.IPAddresses,
			&net.IPNet{IP: masqIP.IP, Mask: hostMask})
	}

	// Always SNAT to the per network masquerade IP.
	var externalIPs []net.IP
	for _, masqIP := range masqIPs {
		externalIPs = append(externalIPs, masqIP.IP)
	}

	// Use the host subnets present in the network attachment definition.
	hostSubnets := make([]*net.IPNet, 0, len(oc.Subnets()))
	for _, subnet := range oc.Subnets() {
		hostSubnets = append(hostSubnets, subnet.CIDR)
	}

	// at layer2 the GR LRP should be different per node same we do for layer3
	// since they should not collide at the distributed switch later on
	gwRouterJoinCIDRs, err := udn.GetGWRouterIPs(node, oc.GetNetInfo())
	if err != nil {
		return nil, fmt.Errorf("failed composing LRP addresses for layer2 network %s: %w", oc.GetNetworkName(), err)
	}

	// Overwrite the primary interface ID with the correct, per-network one.
	l3GatewayConfig.InterfaceID = oc.GetNetworkScopedExtPortName(l3GatewayConfig.BridgeID, node.Name)
	return &GatewayConfig{
		annoConfig:                 l3GatewayConfig,
		hostSubnets:                hostSubnets,
		clusterSubnets:             hostSubnets,
		gwRouterJoinCIDRs:          gwRouterJoinCIDRs,
		hostAddrs:                  nil,
		externalIPs:                externalIPs,
		ovnClusterLRPToJoinIfAddrs: nil,
	}, nil
}

// newClusterRouter creates the cluster router for the network. This router is a
// transit router if the interconnect overlay is in use.
func (oc *Layer2UserDefinedNetworkController) newClusterRouter() (*nbdb.LogicalRouter, error) {
	if !oc.hasInterconnectTransport() {
		return oc.gatewayTopologyFactory.NewClusterRouter(
			oc.GetNetworkScopedClusterRouterName(),
			oc.GetNetInfo(),
			oc.defaultCOPPUUID,
		)
	}

	tunnelKeys := oc.GetTunnelKeys()
	if len(tunnelKeys) != 2 {
		return nil, fmt.Errorf("layer2 network %s with transit router enabled requires exactly 2 tunnel keys, got: %v", oc.GetNetworkName(), tunnelKeys)
	}
	return oc.gatewayTopologyFactory.NewTransitRouter(
		oc.GetNetworkScopedClusterRouterName(),
		oc.GetNetInfo(),
		oc.defaultCOPPUUID,
		strconv.Itoa(tunnelKeys[1]),
	)
}

func (oc *Layer2UserDefinedNetworkController) newGatewayManager(nodeName string) *GatewayManager {
	return NewGatewayManagerForLayer2Topology(
		nodeName,
		oc.defaultCOPPUUID,
		oc.kube,
		oc.nbClient,
		oc.GetNetInfo(),
		oc.watchFactory,
		oc.nodeAnnotationCache,
		oc.addressSetManager,
		config.Layer2UsesTransitRouter,
		oc.gatewayOptions()...,
	)
}

func (oc *Layer2UserDefinedNetworkController) gatewayManagerForNode(nodeName string) *GatewayManager {
	obj, isFound := oc.gatewayManagers.Load(nodeName)
	if !isFound {
		return oc.newGatewayManager(nodeName)
	} else {
		gwManager, isGWManagerType := obj.(*GatewayManager)
		if !isGWManagerType {
			klog.Errorf(
				"failed to extract a gateway manager from the network %q on node %s; creating new one",
				oc.GetNetworkName(),
				nodeName,
			)
			return oc.newGatewayManager(nodeName)
		}
		return gwManager
	}
}

func (oc *Layer2UserDefinedNetworkController) gatewayOptions() []GatewayOption {
	var opts []GatewayOption
	if oc.clusterLoadBalancerGroupUUID != "" {
		opts = append(opts, WithLoadBalancerGroups(
			oc.routerLoadBalancerGroupUUID,
			oc.clusterLoadBalancerGroupUUID,
			oc.switchLoadBalancerGroupUUID,
		))
	}
	if resolver := oc.getNetworkNameForNADKeyFunc(); resolver != nil {
		opts = append(opts, WithNetworkNameForNADKeyResolver(resolver))
	}
	return opts
}

func (oc *Layer2UserDefinedNetworkController) RegisterServiceNetwork(runRepair bool) error {
	err := oc.svcController.RegisterNetwork(oc.GetNetInfo(), oc.serviceNetworkOptions(runRepair))
	if err != nil {
		return fmt.Errorf("error registering OVN-Kubernetes Services controller for network %s: %v", oc.GetNetworkName(), err)
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) ReconcileServiceNetwork() error {
	if oc.svcController == nil {
		return nil
	}
	if err := oc.svcController.ReconcileNetwork(oc.GetNetInfo(), oc.serviceNetworkOptions(false)); err != nil {
		return fmt.Errorf("error reconciling OVN-Kubernetes Services controller for network %s: %v", oc.GetNetworkName(), err)
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) serviceNetworkOptions(runRepair bool) svccontroller.NetworkOptions {
	// Do not use LB templates for UDNs - OVN bug https://issues.redhat.com/browse/FDP-988.
	return svccontroller.NetworkOptions{
		RunRepair:    runRepair,
		UseLBGroups:  oc.clusterLoadBalancerGroupUUID != "",
		UseTemplates: false,
	}
}

func (oc *Layer2UserDefinedNetworkController) DeregisterServiceNetwork() {
	if oc.svcController != nil {
		if err := oc.svcController.DeregisterNetwork(oc.GetNetworkName()); err != nil {
			klog.Errorf("Error deregistering OVN-Kubernetes Services controller for network %s: %v", oc.GetNetworkName(), err)
		}
	}
}

// syncClusterRouterPorts connects the network switch to the transit router
func (oc *Layer2UserDefinedNetworkController) syncClusterRouterPorts(node *corev1.Node, hostSubnets []*net.IPNet) error {
	switchName := oc.GetNetworkScopedSwitchName("")

	// Connect the switch to the router.
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      oc.GetNetworkScopedSwitchToRouterPortName(""),
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			libovsdbops.RouterPort: oc.GetNetworkScopedRouterToSwitchPortName(""),
		},
		ExternalIDs: map[string]string{
			types.NetworkExternalID:  oc.GetNetworkName(),
			types.TopologyExternalID: oc.TopologyType(),
		},
	}
	sw := nbdb.LogicalSwitch{Name: switchName}
	err := libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return err
	}

	return oc.syncNodeClusterRouterPort(node, hostSubnets)
}

// syncNodes finds nodes that still have LRP on the transit router, but the node doesn't exist anymore
// and cleans it up.
func (oc *Layer2UserDefinedNetworkController) syncNodes(nodes []interface{}) error {
	foundNodeNames := sets.New[string]()
	dynamicUDN := config.OVNKubernetesFeature.EnableDynamicUDNAllocation
	for _, obj := range nodes {
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", obj)
		}
		if oc.isLocalNode(node) {
			foundNodeNames.Insert(node.Name)
			continue
		}
		// Clean up remote nodes that went inactive
		if dynamicUDN && !oc.nodeHasActiveNetwork(node.Name) {
			if err := oc.deleteNodeEvent(node); err != nil {
				return err
			}
			continue
		}
		foundNodeNames.Insert(node.Name)
	}
	// Transit Router cleanup
	// Get the transit router. If it's not present - no cleanup to do
	tr := &nbdb.LogicalRouter{
		Name: oc.GetNetworkScopedClusterRouterName(),
	}
	tr, err := libovsdbops.GetLogicalRouter(oc.nbClient, tr)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil
		}
		return err
	}

	staleNodeNames := []string{}
	for _, p := range tr.Ports {
		lp := &nbdb.LogicalRouterPort{
			UUID: p,
		}

		lp, err = libovsdbops.GetLogicalRouterPort(oc.nbClient, lp)
		if err != nil {
			continue
		}

		if lp.ExternalIDs == nil {
			continue
		}

		lportNode := lp.ExternalIDs[types.NodeExternalID]
		if lportNode != "" && !foundNodeNames.Has(lportNode) {
			staleNodeNames = append(staleNodeNames, lportNode)
		}
	}
	for _, staleNodeName := range staleNodeNames {
		if err = oc.cleanupRouterSetupForRemoteNodeGR(staleNodeName); err != nil {
			klog.Errorf("Failed to cleanup the transit router resources from OVN Northbound db for the stale node %s: %v", staleNodeName, err)
		}
	}
	return nil
}

func (oc *Layer2UserDefinedNetworkController) nodeHasActiveNetwork(nodeName string) bool {
	return oc.networkManager.NodeHasNetwork(nodeName, oc.GetNetworkName())
}

// HandleNetworkRefChange marks the node for interconnect sync so a queued update does not skip it.
func (oc *Layer2UserDefinedNetworkController) HandleNetworkRefChange(nodeName string, active bool) {
	if active {
		oc.syncZoneICFailed.Store(nodeName, true)
	}
	oc.nodeReconciler.ReconcileNetwork(nodeName, oc.GetNetworkName())
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

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
	zoneic "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

type Layer3UserDefinedNetworkControllerEventHandler struct {
	baseHandler  baseNetworkControllerEventHandler
	watchFactory *factory.WatchFactory
	objType      reflect.Type
	oc           *Layer3UserDefinedNetworkController
	syncFunc     func([]interface{}) error
}

func (h *Layer3UserDefinedNetworkControllerEventHandler) FilterOutResource(obj interface{}) bool {
	return h.oc.FilterOutResource(h.objType, obj)
}

// FilterOutDeleteResource applies delete-specific filtering for the configured resource type.
func (h *Layer3UserDefinedNetworkControllerEventHandler) FilterOutDeleteResource(obj interface{}) bool {
	return h.oc.FilterOutDeleteResource(h.objType, obj)
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *Layer3UserDefinedNetworkControllerEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	return h.baseHandler.areResourcesEqual(h.objType, obj1, obj2)
}

// GetInternalCacheEntry returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (h *Layer3UserDefinedNetworkControllerEventHandler) GetInternalCacheEntry(obj interface{}) interface{} {
	return h.oc.GetInternalCacheEntryForUserDefinedNetwork(h.objType, obj)
}

// GetResourceFromInformerCache returns the latest state of the object, given an object key and its type.
// from the informers cache.
func (h *Layer3UserDefinedNetworkControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	return h.baseHandler.getResourceFromInformerCache(h.objType, h.watchFactory, key)
}

// RecordAddEvent records the add event on this given object.
func (h *Layer3UserDefinedNetworkControllerEventHandler) RecordAddEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordUpdateEvent records the udpate event on this given object.
func (h *Layer3UserDefinedNetworkControllerEventHandler) RecordUpdateEvent(obj interface{}) {
	h.baseHandler.recordUpdateEvent(h.objType, obj)
}

// RecordDeleteEvent records the delete event on this given object.
func (h *Layer3UserDefinedNetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
	h.baseHandler.recordDeleteEvent(h.objType, obj)
}

// RecordSuccessEvent records the success event on this given object.
func (h *Layer3UserDefinedNetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
	h.baseHandler.recordSuccessEvent(h.objType, obj)
}

// RecordErrorEvent records the error event on this given object.
func (h *Layer3UserDefinedNetworkControllerEventHandler) RecordErrorEvent(_ interface{}, _ string, _ error) {
}

// IsResourceScheduled returns true if the given object has been scheduled.
// Only applied to pods for now. Returns true for all other types.
func (h *Layer3UserDefinedNetworkControllerEventHandler) IsResourceScheduled(obj interface{}) bool {
	return h.baseHandler.isResourceScheduled(h.objType, obj)
}

// AddResource adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
// Given an object to add and a boolean specifying if the function was executed from iterateRetryResources
func (h *Layer3UserDefinedNetworkControllerEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	_ = fromRetryLoop
	return h.oc.AddUserDefinedNetworkResourceCommon(h.objType, obj)
}

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (h *Layer3UserDefinedNetworkControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	return h.oc.UpdateUserDefinedNetworkResourceCommon(h.objType, oldObj, newObj, inRetryCache)
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *Layer3UserDefinedNetworkControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	return h.oc.DeleteUserDefinedNetworkResourceCommon(h.objType, obj, cachedObj)
}

func (h *Layer3UserDefinedNetworkControllerEventHandler) SyncFunc(objs []interface{}) error {
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
func (h *Layer3UserDefinedNetworkControllerEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	return h.baseHandler.isObjectInTerminalState(h.objType, obj)
}

// Layer3UserDefinedNetworkController is created for logical network infrastructure and policy
// for a l3 UDN
type Layer3UserDefinedNetworkController struct {
	BaseUserDefinedNetworkController

	// Node-specific syncMaps used by node event handler
	mgmtPortFailed              sync.Map
	addNodeFailed               sync.Map
	nodeClusterRouterPortFailed sync.Map
	syncZoneICFailed            sync.Map
	gatewaysFailed              sync.Map
	syncEIPNodeRerouteFailed    sync.Map

	gatewayManagers        sync.Map
	gatewayTopologyFactory *topology.GatewayTopologyFactory

	// Cluster wide Load_Balancer_Group UUID.
	// Includes all node switches and node gateway routers.
	clusterLoadBalancerGroupUUID string

	// Cluster wide switch Load_Balancer_Group UUID.
	// Includes all node switches.
	switchLoadBalancerGroupUUID string

	// Cluster wide router Load_Balancer_Group UUID.
	// Includes all node gateway routers.
	routerLoadBalancerGroupUUID string

	// Cluster-wide router default Control Plane Protection (COPP) UUID
	defaultCOPPUUID string

	// Shared controller in charge of services. Nil for cleanup-only controllers.
	svcController *svccontroller.Controller

	// EgressIP controller utilized only to initialize a network with OVN polices to support EgressIP functionality.
	eIPController *EgressIPController
}

// NewLayer3UserDefinedNetworkController create a new OVN controller for the given layer3 NAD
func NewLayer3UserDefinedNetworkController(
	cnci *CommonNetworkControllerInfo,
	netInfo util.NetInfo,
	networkManager networkmanager.Interface,
	routeImportManager routeimport.Manager,
	eIPController *EgressIPController,
	portCache *PortCache,
	addressSetManager *addresssetmanager.AddressSetManager,
	nodeReconciler *nodecontroller.NodeController,
	serviceController *svccontroller.Controller,
) (*Layer3UserDefinedNetworkController, error) {

	stopChan := make(chan struct{})
	ipv4Mode, ipv6Mode := netInfo.IPMode()
	nodeAnnotationCache := nodecontroller.NewNodeAnnotationCache()
	if nodeReconciler != nil {
		nodeAnnotationCache = nodeReconciler.AnnotationCache()
	}

	addressSetFactory := addressset.NewOvnAddressSetFactory(cnci.nbClient, ipv4Mode, ipv6Mode)

	oc := &Layer3UserDefinedNetworkController{
		BaseUserDefinedNetworkController: BaseUserDefinedNetworkController{
			BaseNetworkController: BaseNetworkController{
				CommonNetworkControllerInfo: *cnci,
				controllerName:              getNetworkControllerName(netInfo.GetNetworkName()),
				ReconcilableNetInfo:         util.NewReconcilableNetInfo(netInfo),
				lsManager:                   lsm.NewLogicalSwitchManager(),
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
		mgmtPortFailed:              sync.Map{},
		addNodeFailed:               sync.Map{},
		nodeClusterRouterPortFailed: sync.Map{},
		syncZoneICFailed:            sync.Map{},
		gatewaysFailed:              sync.Map{},
		gatewayTopologyFactory:      topology.NewGatewayTopologyFactory(cnci.nbClient),
		gatewayManagers:             sync.Map{},
		eIPController:               eIPController,
	}

	if oc.IsPrimaryNetwork() {
		oc.onLogicalPortCacheAdd = func(pod *corev1.Pod, _ string) {
			oc.requestLocalPodPolicyRetriesForPod(pod, "logical port cache update")
			if oc.eIPController != nil {
				oc.eIPController.addEgressIPPodRetry(pod, "logical port cache update")
			}
		}
	}

	oc.zoneICHandler = zoneic.NewZoneInterconnectHandler(oc.GetNetInfo(), cnci.nbClient, cnci.sbClient, cnci.watchFactory)

	if util.IsNetworkSegmentationSupportEnabled() && netInfo.IsPrimaryNetwork() {
		oc.svcController = serviceController
	}

	if oc.allocatesPodAnnotation() {
		podAnnotationAllocator := pod.NewPodAnnotationAllocator(
			oc.GetNetInfo(),
			cnci.watchFactory.PodCoreInformer().Lister(),
			cnci.kube,
			nil)
		oc.podAnnotationAllocator = podAnnotationAllocator
	}

	// enable multicast support for UDN only for primaries + multicast enabled
	// TBD: changes needs to be made to support multicast beyond primary UDN
	oc.multicastSupport = oc.IsPrimaryNetwork() && util.IsNetworkSegmentationSupportEnabled() && config.EnableMulticast

	oc.initRetryFramework()
	return oc, nil
}

func (oc *Layer3UserDefinedNetworkController) initRetryFramework() {
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
func (oc *Layer3UserDefinedNetworkController) newRetryFramework(
	objectType reflect.Type) *retry.RetryFramework {
	eventHandler := &Layer3UserDefinedNetworkControllerEventHandler{
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

// Start starts the UDN layer3 controller, handles all events and creates all needed logical entities
func (oc *Layer3UserDefinedNetworkController) Start(_ context.Context) error {
	klog.Infof("Start %s UDN controller for network %s", oc.TopologyType(), oc.GetNetworkName())
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

// Stop gracefully stops the controller, and delete all logical entities for this network if requested
func (oc *Layer3UserDefinedNetworkController) Stop() {
	if oc.stopChan == nil {
		klog.Infof("%s UDN controller of network %s is already stopped", oc.TopologyType(), oc.GetNetworkName())
		return
	}
	klog.Infof("Stop %s UDN controller of network %s", oc.TopologyType(), oc.GetNetworkName())
	oc.DeregisterServiceNetwork()
	oc.DeregisterNodeHandler()
	close(oc.stopChan)
	oc.stopChan = nil
	oc.cancelableCtx.Cancel()
	oc.wg.Wait()

	if oc.netPolicyHandler != nil {
		oc.watchFactory.RemovePolicyHandler(oc.netPolicyHandler)
	}
	if oc.multiNetPolicyHandler != nil {
		oc.watchFactory.RemoveMultiNetworkPolicyHandler(oc.multiNetPolicyHandler)
	}
	if oc.podHandler != nil {
		oc.watchFactory.RemovePodHandler(oc.podHandler)
	}
	if oc.namespaceHandler != nil {
		oc.watchFactory.RemoveNamespaceHandler(oc.namespaceHandler)
	}
	if oc.routeImportManager != nil {
		oc.routeImportManager.ForgetNetwork(oc.GetNetworkName())
	}
}

// Cleanup cleans up logical entities for the given network, called from the
// net-attach-def routine or stale network cleanup.
func (oc *Layer3UserDefinedNetworkController) Cleanup() error {
	// cleans up related OVN logical entities
	var ops []ovsdb.Operation
	var err error

	// Note : Cluster manager removes the subnet annotation for the node.
	netName := oc.GetNetworkName()
	klog.Infof("Delete OVN logical entities for %s network controller of network %s", types.Layer3Topology, netName)

	// For primary L3 UDN only: when this is a cleanup-only controller (dummy for stale UDN
	// cleanup; GetNetworkID() is InvalidID because netInfo was never reconciled from a NAD),
	// discover and cleanup all gateway routers from the NB DB. DB-driven cleanup works even
	// when nodes are already gone.
	if oc.IsPrimaryNetwork() && oc.GetNetworkID() == types.InvalidID {
		if err := cleanupGatewayRoutersForNetworkFromDB(oc.nbClient, oc.GetNetInfo(),
			oc.GetNetworkScopedClusterRouterName(), oc.GetNetworkScopedJoinSwitchName()); err != nil {
			return fmt.Errorf("failed to cleanup gateway routers for network %s: %w", netName, err)
		}
	}

	// Node switches (which hold management port LSPs) are deleted below; LSPs are cascade-deleted with the logical switch.
	// first delete node logical switches
	ops, err = libovsdbops.DeleteLogicalSwitchesWithPredicateOps(oc.nbClient, ops,
		func(item *nbdb.LogicalSwitch) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting switches of network %s: %v", netName, err)
	}

	oc.gatewayManagers.Range(func(nodeName, value any) bool {
		gwManager, isGWManagerType := value.(*GatewayManager)
		if !isGWManagerType {
			klog.Errorf(
				"Failed to cleanup GW manager for network %q on node %s: could not retrieve GWManager",
				netName,
				nodeName,
			)
			return true
		}
		if err := gwManager.Cleanup(); err != nil {
			klog.Errorf("Failed to cleanup GW manager for network %q on node %s: %v", netName, nodeName, err)
		}
		return true
	})

	// now delete cluster router
	ops, err = libovsdbops.DeleteLogicalRoutersWithPredicateOps(oc.nbClient, ops,
		func(item *nbdb.LogicalRouter) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting routers of network %s: %v", netName, err)
	}

	ops, err = cleanupPolicyLogicalEntities(oc.nbClient, ops, oc.controllerName)
	if err != nil {
		return err
	}

	// cleanup address sets after ACLs are deleted to avoid ovn-controller errors
	if oc.addressSetManager != nil {
		if err = oc.addressSetManager.CleanupForController(oc.controllerName); err != nil {
			return fmt.Errorf("failed to cleanup address sets for controller %s: %v", oc.controllerName, err)
		}
	}

	// Delete QoS rows for this network (e.g. from NetworkQoS controller). Applies to primary and
	// secondary Layer3 UDNs when EnableNetworkQoS is set.
	ops, err = libovsdbops.DeleteQoSesWithPredicateOps(oc.nbClient, ops,
		func(item *nbdb.QoS) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting QoSes of network %s: %v", netName, err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to deleting routers/switches of network %s: %v", netName, err)
	}

	if err = oc.zoneICHandler.Cleanup(); err != nil {
		return fmt.Errorf("failed to delete interconnect transit switch of network %s: %v", netName, err)
	}

	// Delete all load balancers owned by this network to prevent orphaned LBs.
	// When network is deleted, all switches and routers are also deleted, but load balancers are not.
	// Load_Balancer is a root table in OVSDB, so rows are never auto-deleted
	// when the switch or LB group referencing them is removed.
	lbs, err := libovsdbops.FindLoadBalancersWithPredicate(oc.nbClient, func(lb *nbdb.LoadBalancer) bool {
		return lb.ExternalIDs[types.NetworkExternalID] == netName
	})
	if err != nil {
		klog.Errorf("Failed to find load balancers for network %q: %v", netName, err)
	} else if len(lbs) > 0 {
		if err := libovsdbops.DeleteLoadBalancers(oc.nbClient, lbs); err != nil {
			klog.Errorf("Failed to delete load balancers on network %q: %v", netName, err)
		}
	}

	// remove load balancer groups
	cleanupLoadBalancerGroups(oc.nbClient, oc.GetNetInfo(),
		oc.switchLoadBalancerGroupUUID, oc.clusterLoadBalancerGroupUUID, oc.routerLoadBalancerGroupUUID)

	// Cleanup noOverlay SNAT exemption address set
	if err := cleanupNoOverlaySNATExemptionAddressSet(oc.addressSetFactory, oc.GetNetInfo(), oc.controllerName); err != nil {
		klog.Warningf("Failed to cleanup noOverlay SNAT exemption address set for network %s: %v", netName, err)
	}

	return nil
}

func (oc *Layer3UserDefinedNetworkController) run() error {
	klog.Infof("Starting all the Watchers for network %s ...", oc.GetNetworkName())
	start := time.Now()

	// WatchNamespaces() should be started first because it has no other
	// dependencies.
	if err := oc.WatchNamespaces(); err != nil {
		return err
	}

	if err := oc.waitForLocalZoneNodeLogicalSwitches(); err != nil {
		return err
	}

	if oc.svcController != nil {
		startSvc := time.Now()
		// Services should be registered after nodes to prevent LB churn.
		err := oc.RegisterServiceNetwork(true)
		endSvc := time.Since(startSvc)

		metrics.MetricOVNKubeControllerSyncDuration.WithLabelValues("service_" + oc.GetNetworkName()).Set(endSvc.Seconds())
		if err != nil {
			return err
		}
	}

	if err := oc.WatchPods(); err != nil {
		return err
	}

	if util.IsMultiNetworkPoliciesSupportEnabled() && !oc.IsPrimaryNetwork() {
		// WatchMultiNetworkPolicy depends on WatchPods and WatchNamespaces
		if err := oc.WatchMultiNetworkPolicy(); err != nil {
			return err
		}
	}

	if oc.IsPrimaryNetwork() {
		// WatchNetworkPolicy depends on WatchPods and WatchNamespaces
		if err := oc.WatchNetworkPolicy(); err != nil {
			return err
		}
	}

	// Add ourselves to the route import manager
	if oc.routeImportManager != nil {
		err := oc.routeImportManager.AddNetwork(oc.GetNetInfo())
		if err != nil {
			return fmt.Errorf("failed to add network %s to the route import manager: %v", oc.GetNetworkName(), err)
		}
	}
	// start NetworkQoS controller if feature is enabled
	if config.OVNKubernetesFeature.EnableNetworkQoS {
		err := oc.newNetworkQoSController()
		if err != nil {
			return fmt.Errorf("unable to create network qos controller, err: %w", err)
		}
		oc.wg.Add(1)
		go func(ch <-chan struct{}) {
			defer oc.wg.Done()
			// Until we have scale issues in future let's spawn only one thread
			oc.nqosController.Run(1, ch)
		}(oc.stopChan)
	}

	klog.Infof("Completing all the Watchers for network %s took %v", oc.GetNetworkName(), time.Since(start))

	return nil
}

func (oc *Layer3UserDefinedNetworkController) waitForLocalZoneNodeLogicalSwitches() error {
	node, err := oc.GetLocalNode()
	if err != nil {
		return fmt.Errorf("failed to get local node for network %s: %w", oc.GetNetworkName(), err)
	}
	if util.NoHostSubnet(node) {
		return nil
	}
	switchName := oc.GetNetworkScopedSwitchName(node.Name)
	if _, err := oc.waitForNodeLogicalSwitch(switchName); err != nil {
		return fmt.Errorf("failed waiting for local node %s logical switch %s for network %s: %w",
			node.Name, switchName, oc.GetNetworkName(), err)
	}
	return nil
}

func (oc *Layer3UserDefinedNetworkController) Reconcile(netInfo util.NetInfo) error {
	if err := oc.BaseNetworkController.reconcile(
		netInfo,
		func(node string) {
			oc.addNodeFailed.Store(node, true)
			oc.gatewaysFailed.Store(node, true)
		},
	); err != nil {
		return err
	}
	return oc.ReconcileServiceNetwork()
}

func (oc *Layer3UserDefinedNetworkController) RegisterNodeHandler() error {
	return oc.nodeReconciler.RegisterNetworkController(oc)
}

// MarkGatewaySyncNeeded marks gateway state dirty so the next node
// reconciliation syncs it even when node annotations did not change.
func (oc *Layer3UserDefinedNetworkController) MarkGatewaySyncNeeded(nodeName string) {
	oc.gatewaysFailed.Store(nodeName, true)
}

// ReconcileNode reconciles a node for a layer3 UDN controller.
func (oc *Layer3UserDefinedNetworkController) ReconcileNode(oldNode, newNode *corev1.Node, oldState, newState *nodecontroller.NodeAnnotationState) error {
	if newNode == nil {
		if oldNode == nil {
			return fmt.Errorf("nil node received for network %s", oc.GetNetworkName())
		}
		return oc.deleteNodeEvent(oldNode)
	}

	if oc.isLocalNode(newNode) {
		var nodeParams *nodeSyncs
		if oldNode == nil {
			_, nodeSync := oc.addNodeFailed.Load(newNode.Name)
			_, clusterRtrSync := oc.nodeClusterRouterPortFailed.Load(newNode.Name)
			_, syncMgmtPort := oc.mgmtPortFailed.Load(newNode.Name)
			_, syncGw := oc.gatewaysFailed.Load(newNode.Name)
			_, syncZoneIC := oc.syncZoneICFailed.Load(newNode.Name)
			_, syncReRoute := oc.syncEIPNodeRerouteFailed.Load(newNode.Name)
			if nodeSync || clusterRtrSync || syncMgmtPort || syncGw || syncZoneIC || syncReRoute {
				nodeParams = &nodeSyncs{
					syncNode:              nodeSync,
					syncClusterRouterPort: clusterRtrSync,
					syncMgmtPort:          syncMgmtPort,
					syncZoneIC:            syncZoneIC,
					syncGw:                syncGw,
					syncReroute:           syncReRoute,
				}
			} else {
				nodeParams = &nodeSyncs{
					syncNode:              true,
					syncClusterRouterPort: true,
					syncMgmtPort:          true,
					syncZoneIC:            true,
					syncGw:                true,
					syncReroute:           true,
				}
			}
		} else {
			zoneClusterChanged := oc.nodeZoneClusterChanged(oldNode, newNode)
			nodeSubnetChange := nodeSubnetChangedForUDN(oldNode, newNode, oc.GetNetworkName(), oldState, newState)

			_, nodeSync := oc.addNodeFailed.Load(newNode.Name)
			nodeSync = nodeSync || nodeSubnetChange
			_, failed := oc.nodeClusterRouterPortFailed.Load(newNode.Name)
			clusterRtrSync := failed || nodeChassisChanged(oldNode, newNode) || nodeSubnetChange || nodeGatewayMTUSupportChanged(oldNode, newNode)
			_, failed = oc.mgmtPortFailed.Load(newNode.Name)
			syncMgmtPort := failed || nodeSubnetChange
			_, syncZoneIC := oc.syncZoneICFailed.Load(newNode.Name)
			syncZoneIC = syncZoneIC || zoneClusterChanged
			_, failed = oc.gatewaysFailed.Load(newNode.Name)
			syncGw := failed ||
				gatewayChanged(oldNode, newNode, oldState, newState, oc.GetNetworkName()) ||
				nodeChassisChanged(oldNode, newNode) ||
				nodeSubnetChange ||
				hostCIDRsChanged(oldNode, newNode) ||
				nodeGatewayMTUSupportChanged(oldNode, newNode)
			_, failed = oc.syncEIPNodeRerouteFailed.Load(newNode.Name)
			syncReroute := failed || util.NodeHostCIDRsAnnotationChanged(oldNode, newNode)
			nodeParams = &nodeSyncs{
				syncNode:              nodeSync,
				syncClusterRouterPort: clusterRtrSync,
				syncMgmtPort:          syncMgmtPort,
				syncZoneIC:            syncZoneIC,
				syncGw:                syncGw,
				syncReroute:           syncReroute,
			}
		}
		return oc.addUpdateLocalNodeEvent(newNode, nodeParams)
	}

	if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		if !oc.networkManager.NodeHasNetwork(newNode.Name, oc.GetNetworkName()) {
			klog.V(5).Infof("Ignoring processing remote node: %s as it has no active NAD for network: %s",
				newNode.Name, oc.GetNetworkName())
			oc.syncZoneICFailed.Store(newNode.Name, true)
			return nil
		}
	}

	if oldNode == nil {
		return oc.addUpdateRemoteNodeEvent(newNode, true)
	}

	zoneClusterChanged := oc.nodeZoneClusterChanged(oldNode, newNode)
	nodeSubnetChange := nodeSubnetChangedForUDN(oldNode, newNode, oc.GetNetworkName(), oldState, newState)
	_, syncZoneIC := oc.syncZoneICFailed.Load(newNode.Name)
	syncZoneIC = syncZoneIC || nodeSubnetChange || zoneClusterChanged
	if syncZoneIC {
		klog.Infof("Node %s in remote zone %s needs interconnect zone sync up. Zone cluster changed: %v",
			newNode.Name, newNode.Name, zoneClusterChanged)
	}
	return oc.addUpdateRemoteNodeEvent(newNode, syncZoneIC)
}

// SyncNodes runs the node sync for a layer3 UDN controller.
func (oc *Layer3UserDefinedNetworkController) SyncNodes(nodes []*corev1.Node) error {
	return oc.syncNodes(nodesToInterfaces(nodes))
}

func (oc *Layer3UserDefinedNetworkController) init() (err error) {
	start := time.Now()
	defer func() {
		if err == nil && config.Metrics.EnableScaleMetrics {
			duration := time.Since(start)
			metrics.RecordUDNNBDBProgrammedDuration(oc.TopologyType(), duration.Seconds())
			klog.Infof("Recorded NBDB programmed duration for network %s: %v", oc.GetNetworkName(), duration)
		}
	}()

	if err := oc.gatherJoinSwitchIPs(); err != nil {
		return fmt.Errorf("failed to gather join switch IPs for network %s: %v", oc.GetNetworkName(), err)
	}

	// Create default Control Plane Protection (COPP) entry for routers
	defaultCOPPUUID, err := EnsureDefaultCOPP(oc.nbClient)
	if err != nil {
		return fmt.Errorf("unable to create router control plane protection: %w", err)
	}
	oc.defaultCOPPUUID = defaultCOPPUUID

	clusterRouter, err := oc.newClusterRouter()
	if err != nil {
		return fmt.Errorf("failed to create OVN cluster router for network %q: %v", oc.GetNetworkName(), err)
	}

	// Only configure join switch, GR, cluster port groups and multicast default policies for user defined primary networks.
	if util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() {
		if err := oc.gatewayTopologyFactory.NewJoinSwitch(clusterRouter, oc.GetNetInfo(), oc.ovnClusterLRPToJoinIfAddrs); err != nil {
			return fmt.Errorf("failed to create join switch for network %q: %v", oc.GetNetworkName(), err)
		}

		if err := oc.setupClusterPortGroups(); err != nil {
			return fmt.Errorf("failed to create cluster port groups for network %q: %w", oc.GetNetworkName(), err)
		}

		if err := oc.syncDefaultMulticastPolicies(); err != nil {
			return fmt.Errorf("failed to sync default multicast policies for network %q: %w", oc.GetNetworkName(), err)
		}
	}

	// FIXME: When https://github.com/ovn-kubernetes/libovsdb/issues/235 is fixed,
	// use IsTableSupported(nbdb.LoadBalancerGroup).
	if _, _, err := util.RunOVNNbctl("--columns=_uuid", "list", "Load_Balancer_Group"); err != nil {
		klog.Warningf("Load Balancer Group support enabled, however version of OVN in use does not support Load Balancer Groups.")
	} else {
		clusterLBGroupUUID, switchLBGroupUUID, routerLBGroupUUID, err := initLoadBalancerGroups(oc.nbClient, oc.GetNetInfo())
		if err != nil {
			return err
		}
		oc.clusterLoadBalancerGroupUUID = clusterLBGroupUUID
		oc.switchLoadBalancerGroupUUID = switchLBGroupUUID
		oc.routerLoadBalancerGroupUUID = routerLBGroupUUID
	}

	// Initialize noOverlay SNAT exemption address set when using noOverlay transport with outboundSNAT enabled
	// This is needed for both local and shared gateway modes
	if oc.GetNetInfo().Transport() == types.NetworkTransportNoOverlay &&
		oc.GetNetInfo().OutboundSNAT() == types.NoOverlaySNATEnabled {
		if _, err := ensureNoOverlaySNATExemptionAddressSet(oc.addressSetFactory, oc.GetNetInfo(), oc.controllerName); err != nil {
			return fmt.Errorf("failed to ensure noOverlay SNAT exemption address set for network %s: %w", oc.GetNetworkName(), err)
		}
	}

	return nil
}

func (oc *Layer3UserDefinedNetworkController) addUpdateLocalNodeEvent(node *corev1.Node, nSyncs *nodeSyncs) error {
	var hostSubnets []*net.IPNet
	var errs []error
	var err error
	if noHostSubnet := util.NoHostSubnet(node); noHostSubnet {
		err := oc.lsManager.AddNoHostSubnetSwitch(oc.GetNetworkScopedName(node.Name))
		if err != nil {
			return fmt.Errorf("nodeAdd: error adding noHost subnet for switch %s: %w", oc.GetNetworkScopedName(node.Name), err)
		}
		return nil
	}

	if !nodeNeedsSync(nSyncs) {
		return nil
	}

	klog.Infof("Adding or Updating local node %q for network %q", node.Name, oc.GetNetworkName())

	// Sync the no-overlay SNAT exemption address set before any SNAT that
	// references it is created: addNode() adds the local-gateway egress SNAT
	// and SyncGateway() the shared-gateway cluster-subnet SNAT. Stop and retry
	// on failure, so that we never SNAT east-west traffic that must be exempted.
	if oc.GetNetInfo().Transport() == types.NetworkTransportNoOverlay &&
		oc.GetNetInfo().OutboundSNAT() == types.NoOverlaySNATEnabled &&
		(nSyncs.syncNode || nSyncs.syncGw) {
		hostAddrs, err := util.GetNodeHostAddrs(node)
		if err == nil {
			err = syncNoOverlaySNATExemptionAddressSet(oc.addressSetFactory, oc.GetNetInfo(), oc.controllerName, hostAddrs)
		}
		if err != nil {
			if nSyncs.syncNode {
				oc.addNodeFailed.Store(node.Name, true)
				oc.nodeClusterRouterPortFailed.Store(node.Name, true)
				oc.mgmtPortFailed.Store(node.Name, true)
				oc.syncZoneICFailed.Store(node.Name, true)
				oc.syncEIPNodeRerouteFailed.Store(node.Name, true)
			}
			oc.gatewaysFailed.Store(node.Name, true)
			err = fmt.Errorf("nodeAdd: error syncing no-overlay SNAT exemption address set for node %q for network %s: %w",
				node.Name, oc.GetNetworkName(), err)
			oc.recordNodeErrorEvent(node, err)
			return err
		}
	}

	if nSyncs.syncNode {
		if hostSubnets, err = oc.addNode(node); err != nil {
			oc.addNodeFailed.Store(node.Name, true)
			oc.nodeClusterRouterPortFailed.Store(node.Name, true)
			oc.mgmtPortFailed.Store(node.Name, true)
			oc.syncZoneICFailed.Store(node.Name, true)
			oc.gatewaysFailed.Store(node.Name, true)
			oc.syncEIPNodeRerouteFailed.Store(node.Name, true)
			err = fmt.Errorf("nodeAdd: error adding node %q for network %s: %w", node.Name, oc.GetNetworkName(), err)
			oc.recordNodeErrorEvent(node, err)
			return err
		}
		oc.addNodeFailed.Delete(node.Name)
	}

	if nSyncs.syncClusterRouterPort {
		if err = oc.syncNodeClusterRouterPort(node, hostSubnets); err != nil {
			errs = append(errs, err)
			oc.nodeClusterRouterPortFailed.Store(node.Name, true)
		} else {
			oc.nodeClusterRouterPortFailed.Delete(node.Name)
		}
	}

	if util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() {
		if nSyncs.syncMgmtPort {
			hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, oc.GetNetworkName())
			if err != nil {
				errs = append(errs, err)
				oc.mgmtPortFailed.Store(node.Name, true)
			} else {
				_, err = oc.syncNodeManagementPort(node, oc.GetNetworkScopedSwitchName(node.Name), oc.GetNetworkScopedClusterRouterName(), hostSubnets)
				if err != nil {
					errs = append(errs, err)
					oc.mgmtPortFailed.Store(node.Name, true)
				} else {
					oc.mgmtPortFailed.Delete(node.Name)
				}
			}
		}
	}

	// ensure pods that already exist on this node have their logical ports created
	if nSyncs.syncNode { // do this only if it is a new node add
		errors := oc.addAllPodsOnNode(node.Name)
		errs = append(errs, errors...)
	}

	if util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() {
		if nSyncs.syncGw {
			gwManager := oc.gatewayManagerForNode(node.Name)
			oc.gatewayManagers.Store(node.Name, gwManager)

			gwConfig, err := oc.nodeGatewayConfig(node)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to generate node GW configuration: %v", err))
				oc.gatewaysFailed.Store(node.Name, true)
			} else {
				if err := gwManager.SyncGateway(
					node,
					gwConfig,
				); err != nil {
					errs = append(errs, fmt.Errorf(
						"failed to sync node GW for network %q: %v",
						gwManager.netInfo.GetNetworkName(),
						err,
					))
					oc.gatewaysFailed.Store(node.Name, true)
				} else {
					oc.gatewaysFailed.Delete(node.Name)
				}
			}
		}

		// if per pod SNAT is being used, then l3 gateway config is required to be able to add pods
		_, gwFailed := oc.gatewaysFailed.Load(node.Name)
		if !gwFailed || !config.Gateway.DisableSNATMultipleGWs {
			if nSyncs.syncNode || nSyncs.syncGw { // do this only if it is a new node add or a gateway sync happened
				errors := oc.addAllPodsOnNode(node.Name)
				errs = append(errs, errors...)
			}
		}
	}

	if oc.hasInterconnectTransport() && nSyncs.syncZoneIC {
		if err := oc.zoneICHandler.AddLocalZoneNode(node); err != nil {
			errs = append(errs, err)
			oc.syncZoneICFailed.Store(node.Name, true)
		} else {
			oc.syncZoneICFailed.Delete(node.Name)
		}
	}

	if config.OVNKubernetesFeature.EnableEgressIP && util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() && nSyncs.syncReroute {
		rerouteFailed := false
		if err = oc.eIPController.ensureRouterPoliciesForNetwork(oc.GetNetInfo(), node); err != nil {
			errs = append(errs, fmt.Errorf("failed to ensure EgressIP router polices for network %s: %v", oc.GetNetworkName(), err))
			rerouteFailed = true
		}
		if err = oc.eIPController.ensureSwitchPoliciesForNode(oc.GetNetInfo(), node.Name); err != nil {
			errs = append(errs, fmt.Errorf("failed to ensure EgressIP switch policies for network %s: %v", oc.GetNetworkName(), err))
			rerouteFailed = true
		}
		if rerouteFailed {
			oc.syncEIPNodeRerouteFailed.Store(node.Name, true)
		} else {
			oc.syncEIPNodeRerouteFailed.Delete(node.Name)
		}
	}

	err = utilerrors.Join(errs...)
	if err != nil {
		oc.recordNodeErrorEvent(node, err)
	}
	return err
}

func (oc *Layer3UserDefinedNetworkController) addUpdateRemoteNodeEvent(node *corev1.Node, syncZoneIc bool) error {
	var err error
	if oc.hasInterconnectTransport() && syncZoneIc {
		if err = oc.zoneICHandler.AddRemoteZoneNode(node); err != nil {
			err = fmt.Errorf("failed to add the remote zone node [%s] to the zone interconnect handler, err : %w", node.Name, err)
			oc.syncZoneICFailed.Store(node.Name, true)
		} else {
			oc.syncZoneICFailed.Delete(node.Name)
		}
	}
	return err
}

// addOrUpdateUDNNodeSubnetEgressSNAT adds or updates the SNAT on each node's ovn-cluster-router in L3 networks for each UDN
// Based on the isUDNAdvertised flag, the SNAT matches are slightly different
// snat eth.dst == d6:cf:fd:2c:a6:44 169.254.0.12 10.128.0.0/24
// snat eth.dst == d6:cf:fd:2c:a6:44 169.254.0.12 2010:100:200::/64
// these SNATs are required for pod2Egress traffic in LGW mode and pod2SameNode traffic in SGW mode to function properly on UDNs
// SNAT Breakdown:
// match = "eth.dst == d6:cf:fd:2c:a6:44"; the MAC here is the mpX interface MAC address for this UDN
// logicalIP = "10.128.0.0/24"; which is the podsubnet for this node in L3 UDN
// externalIP = "169.254.0.12"; which is the masqueradeIP for this L3 UDN
// so all in all we want to condionally SNAT all packets that are coming from pods hosted on this node,
// which are leaving via UDN's mpX interface to the UDN's masqueradeIP.
// If isUDNAdvertised is true, then we want to SNAT packets from pods on this network
// leaving towards cluster node IPs and UDN-enabled service IPs to the masqueradeIP.
// In shared gateway mode, advertised SNAT keeps NAT.match empty and uses
// NAT.allowed_ext_ips to point directly at those address sets. Local gateway mode
// keeps the destination address-set checks in NAT.match, for example:
// "eth.dst == 0a:58:5d:5d:00:02 && (ip4.dst == $a712973235162149816)" "169.254.0.36" "93.93.0.0/24"
func (oc *Layer3UserDefinedNetworkController) addOrUpdateUDNNodeSubnetEgressSNAT(localPodSubnets []*net.IPNet, node *corev1.Node, isUDNAdvertised bool) error {
	outputPort := oc.GetNetworkScopedRouterToSwitchPortName(node.Name)
	nats, err := oc.buildUDNEgressSNAT(localPodSubnets, outputPort, isUDNAdvertised)
	if err != nil {
		return fmt.Errorf("failed to build UDN masquerade SNATs for network %q on node %q, err: %w",
			oc.GetNetworkName(), node.Name, err)
	}
	if len(nats) == 0 {
		return nil // nothing to do
	}
	router := &nbdb.LogicalRouter{
		Name: oc.GetNetworkScopedClusterRouterName(),
	}
	if err := libovsdbops.CreateOrUpdateNATs(oc.nbClient, router, nats...); err != nil {
		return fmt.Errorf("failed to update SNAT for node subnet on router: %q for network %q, error: %w",
			oc.GetNetworkScopedClusterRouterName(), oc.GetNetworkName(), err)
	}
	return nil
}

func (oc *Layer3UserDefinedNetworkController) addNode(node *corev1.Node) ([]*net.IPNet, error) {
	// Node subnet for the layer3 UDN is allocated by cluster manager.
	// Make sure that the node is allocated with the subnet before proceeding
	// to create OVN Northbound resources.
	hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, oc.GetNetworkName())
	if err != nil || len(hostSubnets) < 1 {
		return nil, fmt.Errorf("subnet annotation in the node %q for the layer3 UDN %s is missing : %w", node.Name, oc.GetNetworkName(), err)
	}

	err = oc.createNodeLogicalSwitch(node.Name, hostSubnets, oc.clusterLoadBalancerGroupUUID, oc.switchLoadBalancerGroupUUID)
	if err != nil {
		return nil, err
	}
	if util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() {
		isUDNAdvertised := util.IsPodNetworkAdvertisedAtNode(oc, node.Name)
		if err := oc.addOrUpdateUDNNodeSubnetEgressSNAT(hostSubnets, node, isUDNAdvertised); err != nil {
			return nil, err
		}
		shouldIsolate := isUDNAdvertised && config.OVNKubernetesFeature.AdvertisedUDNIsolationMode == config.AdvertisedUDNIsolationModeStrict
		if shouldIsolate {
			if err = oc.addAdvertisedNetworkIsolation(node.Name); err != nil {
				return nil, err
			}
		} else {
			if err = oc.deleteAdvertisedNetworkIsolation(node.Name); err != nil {
				return nil, err
			}
		}
	}
	return hostSubnets, nil
}

func (oc *Layer3UserDefinedNetworkController) deleteNodeEvent(node *corev1.Node) error {
	klog.V(5).Infof("Deleting Node %q for network %s. Removing the node from "+
		"various caches", node.Name, oc.GetNetworkName())

	if oc.isLocalNode(node) {
		if err := oc.deleteNode(node.Name); err != nil {
			return err
		}

		if gwObj, ok := oc.gatewayManagers.Load(node.Name); ok {
			if gw, ok := gwObj.(*GatewayManager); ok {
				if err := gw.Cleanup(); err != nil {
					return fmt.Errorf("failed to cleanup gateway on node %q: %w", node.Name, err)
				}
			} else {
				klog.Errorf("Failed to cleanup GW manager for network %q on node %s: could not retrieve GatewayManager", oc.GetNetworkName(), node.Name)
			}
			oc.gatewayManagers.Delete(node.Name)
		}
		oc.lsManager.DeleteSwitch(oc.GetNetworkScopedName(node.Name))
		oc.addNodeFailed.Delete(node.Name)
		oc.mgmtPortFailed.Delete(node.Name)
		oc.nodeClusterRouterPortFailed.Delete(node.Name)
		oc.gatewaysFailed.Delete(node.Name)
	} else {
		if oc.hasInterconnectTransport() {
			if err := oc.zoneICHandler.DeleteNode(node); err != nil {
				return err
			}
		}
	}
	oc.syncZoneICFailed.Delete(node.Name)
	oc.syncEIPNodeRerouteFailed.Delete(node.Name)
	return nil
}

func (oc *Layer3UserDefinedNetworkController) deleteNode(nodeName string) error {
	if err := oc.deleteNodeLogicalNetwork(nodeName); err != nil {
		return fmt.Errorf("error deleting node %s logical network: %v", nodeName, err)
	}

	return nil
}

// We only deal with cleaning up nodes that shouldn't exist here, since
// watchNodes() will be called for all existing nodes at startup anyway.
// Note that this list will include the 'join' cluster switch, which we
// do not want to delete.
func (oc *Layer3UserDefinedNetworkController) syncNodes(nodes []interface{}) error {
	foundNodes := sets.New[string]()
	activeNodes := nodes
	dynamicUDN := config.OVNKubernetesFeature.EnableDynamicUDNAllocation
	if dynamicUDN {
		activeNodes = make([]interface{}, 0, len(nodes))
	}
	for _, tmp := range nodes {
		node, ok := tmp.(*corev1.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", tmp)
		}
		if util.NoHostSubnet(node) {
			continue
		}

		// Add the node to foundNodes only if it is managed by this controller.
		if oc.isLocalNode(node) {
			foundNodes.Insert(node.Name)
			if dynamicUDN {
				activeNodes = append(activeNodes, node)
			}
			continue
		}
		if dynamicUDN {
			if oc.nodeHasActiveNetwork(node.Name) {
				foundNodes.Insert(node.Name)
				activeNodes = append(activeNodes, node)
			} else {
				if err := oc.deleteNodeEvent(node); err != nil {
					return err
				}
			}
		}
	}

	p := func(item *nbdb.LogicalSwitch) bool {
		return len(item.OtherConfig) > 0 && item.ExternalIDs[types.NetworkExternalID] == oc.GetNetworkName()
	}
	nodeSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(oc.nbClient, p)
	if err != nil {
		return fmt.Errorf("failed to get node logical switches which have other-config set for network %s: %v", oc.GetNetworkName(), err)
	}
	for _, nodeSwitch := range nodeSwitches {
		nodeName := oc.RemoveNetworkScopeFromName(nodeSwitch.Name)
		if !foundNodes.Has(nodeName) {
			if err := oc.deleteNode(nodeName); err != nil {
				return fmt.Errorf("failed to delete node:%s, err:%v", nodeName, err)
			}
		}
	}

	if oc.hasInterconnectTransport() {
		if err := oc.zoneICHandler.CleanupStaleNodes(activeNodes); err != nil {
			return fmt.Errorf("zoneICHandler failed to cleanup stale nodes: error: %w", err)
		}
	}

	return nil
}

func (oc *Layer3UserDefinedNetworkController) nodeHasActiveNetwork(nodeName string) bool {
	return oc.networkManager.NodeHasNetwork(nodeName, oc.GetNetworkName())
}

func (oc *Layer3UserDefinedNetworkController) gatherJoinSwitchIPs() error {
	// Allocate IPs for logical router port prefixed with
	// `GwRouterToJoinSwitchPrefix` for the network managed by this controller.
	// This should always allocate the first IPs in the join switch subnets.
	gwLRPIfAddrs, err := oc.getOVNClusterRouterPortToJoinSwitchIfAddrs()
	if err != nil {
		return fmt.Errorf("failed to allocate join switch IP for network %s: %v", oc.GetNetworkName(), err)
	}
	oc.ovnClusterLRPToJoinIfAddrs = gwLRPIfAddrs
	return nil
}

func (oc *Layer3UserDefinedNetworkController) nodeGatewayConfig(node *corev1.Node) (*GatewayConfig, error) {
	l3GatewayConfig, hasUplink, err := oc.uplinkGatewayConfig(node)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to get node %s network %s Uplink gateway config: %v",
			node.Name,
			oc.GetNetworkName(),
			err,
		)
	}
	if !hasUplink {
		l3GatewayConfig, err = util.ParseNodeL3GatewayAnnotation(node)
		if err != nil {
			return nil, fmt.Errorf("failed to get node %s network %s L3 gateway config: %v", node.Name, oc.GetNetworkName(), err)
		}
	}

	networkName := oc.GetNetworkName()
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

	var hostAddrs []string
	for _, externalIP := range externalIPs {
		hostAddrs = append(hostAddrs, externalIP.String())
	}

	// Use the cluster subnets present in the network attachment definition.
	clusterSubnets := make([]*net.IPNet, 0, len(oc.Subnets()))
	for _, subnet := range oc.Subnets() {
		clusterSubnets = append(clusterSubnets, subnet.CIDR)
	}

	// Fetch the host subnets present in the node annotation for this network
	hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, oc.GetNetworkName())
	if err != nil {
		return nil, fmt.Errorf("failed to get node %q subnet annotation for network %q: %v", node.Name, oc.GetNetworkName(), err)
	}

	gwRouterJoinCIDRs, err := udn.GetGWRouterIPs(node, oc.GetNetInfo())
	if err != nil {
		return nil, fmt.Errorf("failed extracting node %q GW router join subnet IP for layer3 network %q: %w", node.Name, networkName, err)
	}

	// Overwrite the primary interface ID with the correct, per-network one.
	l3GatewayConfig.InterfaceID = oc.GetNetworkScopedExtPortName(l3GatewayConfig.BridgeID, node.Name)

	return &GatewayConfig{
		annoConfig:                 l3GatewayConfig,
		hostSubnets:                hostSubnets,
		clusterSubnets:             clusterSubnets,
		gwRouterJoinCIDRs:          gwRouterJoinCIDRs,
		hostAddrs:                  hostAddrs,
		externalIPs:                externalIPs,
		ovnClusterLRPToJoinIfAddrs: oc.ovnClusterLRPToJoinIfAddrs,
	}, nil
}

func (oc *Layer3UserDefinedNetworkController) newClusterRouter() (*nbdb.LogicalRouter, error) {
	if oc.multicastSupport {
		return oc.gatewayTopologyFactory.NewClusterRouterWithMulticastSupport(
			oc.GetNetworkScopedClusterRouterName(),
			oc.GetNetInfo(),
			oc.defaultCOPPUUID,
		)
	}
	return oc.gatewayTopologyFactory.NewClusterRouter(
		oc.GetNetworkScopedClusterRouterName(),
		oc.GetNetInfo(),
		oc.defaultCOPPUUID,
	)
}

func (oc *Layer3UserDefinedNetworkController) newGatewayManager(nodeName string) *GatewayManager {
	return NewGatewayManager(
		nodeName,
		oc.defaultCOPPUUID,
		oc.kube,
		oc.nbClient,
		oc.GetNetInfo(),
		oc.watchFactory,
		oc.nodeAnnotationCache,
		oc.addressSetManager,
		oc.gatewayOptions()...,
	)
}

func (oc *Layer3UserDefinedNetworkController) gatewayOptions() []GatewayOption {
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

func (oc *Layer3UserDefinedNetworkController) gatewayManagerForNode(nodeName string) *GatewayManager {
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

func (oc *Layer3UserDefinedNetworkController) RegisterServiceNetwork(runRepair bool) error {
	err := oc.svcController.RegisterNetwork(oc.GetNetInfo(), oc.serviceNetworkOptions(runRepair))
	if err != nil {
		return fmt.Errorf("error registering OVN-Kubernetes Services controller for network %s: %v", oc.GetNetworkName(), err)
	}
	return nil
}

func (oc *Layer3UserDefinedNetworkController) ReconcileServiceNetwork() error {
	if oc.svcController == nil {
		return nil
	}
	if err := oc.svcController.ReconcileNetwork(oc.GetNetInfo(), oc.serviceNetworkOptions(false)); err != nil {
		return fmt.Errorf("error reconciling OVN-Kubernetes Services controller for network %s: %v", oc.GetNetworkName(), err)
	}
	return nil
}

func (oc *Layer3UserDefinedNetworkController) serviceNetworkOptions(runRepair bool) svccontroller.NetworkOptions {
	// Do not use LB templates for UDNs - OVN bug https://issues.redhat.com/browse/FDP-988.
	return svccontroller.NetworkOptions{
		RunRepair:    runRepair,
		UseLBGroups:  oc.clusterLoadBalancerGroupUUID != "",
		UseTemplates: false,
	}
}

func (oc *Layer3UserDefinedNetworkController) DeregisterServiceNetwork() {
	if oc.svcController != nil {
		if err := oc.svcController.DeregisterNetwork(oc.GetNetworkName()); err != nil {
			klog.Errorf("Error deregistering OVN-Kubernetes Services controller for network %s: %v", oc.GetNetworkName(), err)
		}
	}
}

// HandleNetworkRefChange marks the node for interconnect sync so a queued update does not skip it.
func (oc *Layer3UserDefinedNetworkController) HandleNetworkRefChange(nodeName string, active bool) {
	if active {
		oc.syncZoneICFailed.Store(nodeName, true)
	}
	oc.nodeReconciler.ReconcileNetwork(nodeName, oc.GetNetworkName())
}

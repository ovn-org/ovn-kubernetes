package ovn

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/udn"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	svccontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/topology"
	zoneic "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type secondaryLayer3NetworkControllerEventHandler struct {
	baseHandler  baseNetworkControllerEventHandler
	watchFactory *factory.WatchFactory
	objType      reflect.Type
	oc           *SecondaryLayer3NetworkController
	syncFunc     func([]interface{}) error
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *secondaryLayer3NetworkControllerEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	return h.baseHandler.areResourcesEqual(h.objType, obj1, obj2)
}

// GetInternalCacheEntry returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (h *secondaryLayer3NetworkControllerEventHandler) GetInternalCacheEntry(obj interface{}) interface{} {
	return h.oc.GetInternalCacheEntryForSecondaryNetwork(h.objType, obj)
}

// GetResourceFromInformerCache returns the latest state of the object, given an object key and its type.
// from the informers cache.
func (h *secondaryLayer3NetworkControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	return h.baseHandler.getResourceFromInformerCache(h.objType, h.watchFactory, key)
}

// RecordAddEvent records the add event on this given object.
func (h *secondaryLayer3NetworkControllerEventHandler) RecordAddEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordUpdateEvent records the udpate event on this given object.
func (h *secondaryLayer3NetworkControllerEventHandler) RecordUpdateEvent(obj interface{}) {
	h.baseHandler.recordUpdateEvent(h.objType, obj)
}

// RecordDeleteEvent records the delete event on this given object.
func (h *secondaryLayer3NetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
	h.baseHandler.recordDeleteEvent(h.objType, obj)
}

// RecordSuccessEvent records the success event on this given object.
func (h *secondaryLayer3NetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
	h.baseHandler.recordSuccessEvent(h.objType, obj)
}

// RecordErrorEvent records the error event on this given object.
func (h *secondaryLayer3NetworkControllerEventHandler) RecordErrorEvent(obj interface{}, reason string, err error) {
}

// IsResourceScheduled returns true if the given object has been scheduled.
// Only applied to pods for now. Returns true for all other types.
func (h *secondaryLayer3NetworkControllerEventHandler) IsResourceScheduled(obj interface{}) bool {
	return h.baseHandler.isResourceScheduled(h.objType, obj)
}

// AddResource adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
// Given an object to add and a boolean specifying if the function was executed from iterateRetryResources
func (h *secondaryLayer3NetworkControllerEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	switch h.objType {
	case factory.NodeType:
		node, ok := obj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *kapi.Node", obj)
		}

		if h.oc.isLocalZoneNode(node) {
			var nodeParams *nodeSyncs
			if fromRetryLoop {
				_, nodeSync := h.oc.addNodeFailed.Load(node.Name)
				_, clusterRtrSync := h.oc.nodeClusterRouterPortFailed.Load(node.Name)
				_, syncMgmtPort := h.oc.mgmtPortFailed.Load(node.Name)
				_, syncGw := h.oc.gatewaysFailed.Load(node.Name)
				_, syncZoneIC := h.oc.syncZoneICFailed.Load(node.Name)
				nodeParams = &nodeSyncs{
					syncNode:              nodeSync,
					syncClusterRouterPort: clusterRtrSync,
					syncMgmtPort:          syncMgmtPort,
					syncZoneIC:            syncZoneIC,
					syncGw:                syncGw,
				}
			} else {
				nodeParams = &nodeSyncs{
					syncNode:              true,
					syncClusterRouterPort: true,
					syncMgmtPort:          true,
					syncZoneIC:            config.OVNKubernetesFeature.EnableInterconnect,
					syncGw:                true,
				}
			}
			if err := h.oc.addUpdateLocalNodeEvent(node, nodeParams); err != nil {
				klog.Errorf("Node add failed for %s, will try again later: %v",
					node.Name, err)
				return err
			}
		} else {
			if err := h.oc.addUpdateRemoteNodeEvent(node, config.OVNKubernetesFeature.EnableInterconnect); err != nil {
				return err
			}
		}
	default:
		return h.oc.AddSecondaryNetworkResourceCommon(h.objType, obj)
	}
	return nil
}

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (h *secondaryLayer3NetworkControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	switch h.objType {
	case factory.NodeType:
		newNode, ok := newObj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast newObj of type %T to *kapi.Node", newObj)
		}
		oldNode, ok := oldObj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast oldObj of type %T to *kapi.Node", oldObj)
		}
		newNodeIsLocalZoneNode := h.oc.isLocalZoneNode(newNode)
		zoneClusterChanged := h.oc.nodeZoneClusterChanged(oldNode, newNode, newNodeIsLocalZoneNode, h.oc.NetInfo.GetNetworkName())
		nodeSubnetChanged := nodeSubnetChanged(oldNode, newNode, h.oc.NetInfo.GetNetworkName())
		if newNodeIsLocalZoneNode {
			var nodeSyncsParam *nodeSyncs
			if h.oc.isLocalZoneNode(oldNode) {
				// determine what actually changed in this update
				_, nodeSync := h.oc.addNodeFailed.Load(newNode.Name)
				_, failed := h.oc.nodeClusterRouterPortFailed.Load(newNode.Name)
				clusterRtrSync := failed || nodeChassisChanged(oldNode, newNode) || nodeSubnetChanged
				_, failed = h.oc.mgmtPortFailed.Load(newNode.Name)
				syncMgmtPort := failed || macAddressChanged(oldNode, newNode, h.oc.GetNetworkName()) || nodeSubnetChanged
				_, syncZoneIC := h.oc.syncZoneICFailed.Load(newNode.Name)
				syncZoneIC = syncZoneIC || zoneClusterChanged
				_, failed = h.oc.gatewaysFailed.Load(newNode.Name)
				syncGw := failed ||
					gatewayChanged(oldNode, newNode) ||
					nodeSubnetChanged ||
					hostCIDRsChanged(oldNode, newNode) ||
					nodeGatewayMTUSupportChanged(oldNode, newNode)
				nodeSyncsParam = &nodeSyncs{
					syncNode:              nodeSync,
					syncClusterRouterPort: clusterRtrSync,
					syncMgmtPort:          syncMgmtPort,
					syncZoneIC:            syncZoneIC,
					syncGw:                syncGw,
				}
			} else {
				klog.Infof("Node %s moved from the remote zone %s to local zone %s.",
					newNode.Name, util.GetNodeZone(oldNode), util.GetNodeZone(newNode))
				// The node is now a local zone node. Trigger a full node sync.
				nodeSyncsParam = &nodeSyncs{
					syncNode:              true,
					syncClusterRouterPort: true,
					syncMgmtPort:          true,
					syncZoneIC:            config.OVNKubernetesFeature.EnableInterconnect,
					syncGw:                true,
				}
			}

			return h.oc.addUpdateLocalNodeEvent(newNode, nodeSyncsParam)
		} else {
			_, syncZoneIC := h.oc.syncZoneICFailed.Load(newNode.Name)

			// Check if the node moved from local zone to remote zone and if so syncZoneIC should be set to true.
			// Also check if node subnet changed, so static routes are properly set
			syncZoneIC = syncZoneIC || h.oc.isLocalZoneNode(oldNode) || nodeSubnetChanged || zoneClusterChanged
			if syncZoneIC {
				klog.Infof("Node %s in remote zone %s needs interconnect zone sync up. Zone cluster changed: %v",
					newNode.Name, util.GetNodeZone(newNode), zoneClusterChanged)
			}
			return h.oc.addUpdateRemoteNodeEvent(newNode, syncZoneIC)
		}
	default:
		return h.oc.UpdateSecondaryNetworkResourceCommon(h.objType, oldObj, newObj, inRetryCache)
	}
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *secondaryLayer3NetworkControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	switch h.objType {
	case factory.NodeType:
		node, ok := obj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.Node", obj)
		}
		return h.oc.deleteNodeEvent(node)

	default:
		return h.oc.DeleteSecondaryNetworkResourceCommon(h.objType, obj, cachedObj)
	}
}

func (h *secondaryLayer3NetworkControllerEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.PodType:
			syncFunc = h.oc.syncPodsForSecondaryNetwork

		case factory.NodeType:
			syncFunc = h.oc.syncNodes

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
func (h *secondaryLayer3NetworkControllerEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	return h.baseHandler.isObjectInTerminalState(h.objType, obj)
}

// SecondaryLayer3NetworkController is created for logical network infrastructure and policy
// for a secondary l3 network
type SecondaryLayer3NetworkController struct {
	BaseSecondaryNetworkController

	// Node-specific syncMaps used by node event handler
	mgmtPortFailed              sync.Map
	addNodeFailed               sync.Map
	nodeClusterRouterPortFailed sync.Map
	syncZoneICFailed            sync.Map
	gatewaysFailed              sync.Map

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

	// Controller in charge of services
	svcController *svccontroller.Controller
}

// NewSecondaryLayer3NetworkController create a new OVN controller for the given secondary layer3 NAD
func NewSecondaryLayer3NetworkController(cnci *CommonNetworkControllerInfo, netInfo util.NetInfo, nadController nad.NADController) (*SecondaryLayer3NetworkController, error) {

	stopChan := make(chan struct{})
	ipv4Mode, ipv6Mode := netInfo.IPMode()
	var zoneICHandler *zoneic.ZoneInterconnectHandler
	if config.OVNKubernetesFeature.EnableInterconnect {
		zoneICHandler = zoneic.NewZoneInterconnectHandler(netInfo, cnci.nbClient, cnci.sbClient, cnci.watchFactory)
	}

	addressSetFactory := addressset.NewOvnAddressSetFactory(cnci.nbClient, ipv4Mode, ipv6Mode)

	var svcController *svccontroller.Controller
	if util.IsNetworkSegmentationSupportEnabled() && netInfo.IsPrimaryNetwork() {
		var err error
		svcController, err = svccontroller.NewController(
			cnci.client, cnci.nbClient,
			cnci.watchFactory.ServiceCoreInformer(),
			cnci.watchFactory.EndpointSliceCoreInformer(),
			cnci.watchFactory.NodeCoreInformer(),
			nadController,
			cnci.recorder,
			netInfo,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create new service controller for network=%s: %w", netInfo.GetNetworkName(), err)
		}
	}

	oc := &SecondaryLayer3NetworkController{
		BaseSecondaryNetworkController: BaseSecondaryNetworkController{
			BaseNetworkController: BaseNetworkController{
				CommonNetworkControllerInfo: *cnci,
				controllerName:              getNetworkControllerName(netInfo.GetNetworkName()),
				NetInfo:                     netInfo,
				lsManager:                   lsm.NewLogicalSwitchManager(),
				logicalPortCache:            newPortCache(stopChan),
				namespaces:                  make(map[string]*namespaceInfo),
				namespacesMutex:             sync.Mutex{},
				addressSetFactory:           addressSetFactory,
				networkPolicies:             syncmap.NewSyncMap[*networkPolicy](),
				sharedNetpolPortGroups:      syncmap.NewSyncMap[*defaultDenyPortGroups](),
				podSelectorAddressSets:      syncmap.NewSyncMap[*PodSelectorAddressSet](),
				stopChan:                    stopChan,
				wg:                          &sync.WaitGroup{},
				localZoneNodes:              &sync.Map{},
				zoneICHandler:               zoneICHandler,
				cancelableCtx:               util.NewCancelableContext(),
				nadController:               nadController,
			},
		},
		mgmtPortFailed:              sync.Map{},
		addNodeFailed:               sync.Map{},
		nodeClusterRouterPortFailed: sync.Map{},
		syncZoneICFailed:            sync.Map{},
		gatewaysFailed:              sync.Map{},
		gatewayTopologyFactory:      topology.NewGatewayTopologyFactory(cnci.nbClient),
		gatewayManagers:             sync.Map{},
		svcController:               svcController,
	}

	if oc.allocatesPodAnnotation() {
		podAnnotationAllocator := pod.NewPodAnnotationAllocator(
			netInfo,
			cnci.watchFactory.PodCoreInformer().Lister(),
			cnci.kube,
			nil)
		oc.podAnnotationAllocator = podAnnotationAllocator
	}

	// disable multicast support for secondary networks
	// TBD: changes needs to be made to support multicast in secondary networks
	oc.multicastSupport = false

	oc.initRetryFramework()
	return oc, nil
}

func (oc *SecondaryLayer3NetworkController) initRetryFramework() {
	oc.retryPods = oc.newRetryFramework(factory.PodType)
	oc.retryNodes = oc.newRetryFramework(factory.NodeType)

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
func (oc *SecondaryLayer3NetworkController) newRetryFramework(
	objectType reflect.Type) *retry.RetryFramework {
	eventHandler := &secondaryLayer3NetworkControllerEventHandler{
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
		oc.stopChan,
		oc.wg,
		oc.watchFactory,
		resourceHandler,
	)
}

// Start starts the secondary layer3 controller, handles all events and creates all needed logical entities
func (oc *SecondaryLayer3NetworkController) Start(ctx context.Context) error {
	klog.Infof("Start secondary %s network controller of network %s", oc.TopologyType(), oc.GetNetworkName())
	_, err := oc.getNetworkID()
	if err != nil {
		return fmt.Errorf("unable to set networkID on secondary L3 controller for network %s, err: %w", oc.GetNetworkName(), err)
	}
	if err = oc.Init(ctx); err != nil {
		return err
	}

	return oc.Run()
}

// Stop gracefully stops the controller, and delete all logical entities for this network if requested
func (oc *SecondaryLayer3NetworkController) Stop() {
	klog.Infof("Stop secondary %s network controller of network %s", oc.TopologyType(), oc.GetNetworkName())
	close(oc.stopChan)
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
	if oc.nodeHandler != nil {
		oc.watchFactory.RemoveNodeHandler(oc.nodeHandler)
	}
	if oc.namespaceHandler != nil {
		oc.watchFactory.RemoveNamespaceHandler(oc.namespaceHandler)
	}
}

// Cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *SecondaryLayer3NetworkController) Cleanup() error {
	// cleans up related OVN logical entities
	var ops []ovsdb.Operation
	var err error

	// Note : Cluster manager removes the subnet annotation for the node.
	netName := oc.GetNetworkName()
	klog.Infof("Delete OVN logical entities for %s network controller of network %s", types.Layer3Topology, netName)
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

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to deleting routers/switches of network %s: %v", netName, err)
	}

	if config.OVNKubernetesFeature.EnableInterconnect {
		if err = oc.zoneICHandler.Cleanup(); err != nil {
			return fmt.Errorf("failed to delete interconnect transit switch of network %s: %v", netName, err)
		}
	}
	return nil
}

func (oc *SecondaryLayer3NetworkController) Run() error {
	klog.Infof("Starting all the Watchers for network %s ...", oc.GetNetworkName())
	start := time.Now()

	// WatchNamespaces() should be started first because it has no other
	// dependencies, and WatchNodes() depends on it
	if err := oc.WatchNamespaces(); err != nil {
		return err
	}

	if err := oc.WatchNodes(); err != nil {
		return err
	}

	if oc.svcController != nil {
		startSvc := time.Now()
		// Services should be started after nodes to prevent LB churn
		err := oc.StartServiceController(oc.wg, true)
		endSvc := time.Since(startSvc)

		metrics.MetricOVNKubeControllerSyncDuration.WithLabelValues("service_" + oc.GetNetworkName()).Set(endSvc.Seconds())
		if err != nil {
			return err
		}
	}

	if err := oc.WatchPods(); err != nil {
		return err
	}

	if util.IsMultiNetworkPoliciesSupportEnabled() {
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

	// start NetworkQoS controller if feature is enabled
	if config.OVNKubernetesFeature.EnableNetworkQoS {
		err := oc.newNetworkQoSController()
		if err != nil {
			return fmt.Errorf("unable to create network qos controller, err: %w", err)
		}
		oc.wg.Add(1)
		go func() {
			defer oc.wg.Done()
			// Until we have scale issues in future let's spawn only one thread
			oc.nqosController.Run(1, oc.stopChan)
		}()
	}

	klog.Infof("Completing all the Watchers for network %s took %v", oc.GetNetworkName(), time.Since(start))

	return nil
}

// WatchNodes starts the watching of node resource and calls
// back the appropriate handler logic
func (oc *SecondaryLayer3NetworkController) WatchNodes() error {
	if oc.nodeHandler != nil {
		return nil
	}
	handler, err := oc.retryNodes.WatchResource()
	if err == nil {
		oc.nodeHandler = handler
	}
	return err
}

func (oc *SecondaryLayer3NetworkController) Init(ctx context.Context) error {
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

	// Only configure join switch and GR for user defined primary networks.
	if util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() {
		if err := oc.gatewayTopologyFactory.NewJoinSwitch(clusterRouter, oc.NetInfo, oc.ovnClusterLRPToJoinIfAddrs); err != nil {
			return fmt.Errorf("failed to create join switch for network %q: %v", oc.GetNetworkName(), err)
		}
	}

	// FIXME: When https://github.com/ovn-org/libovsdb/issues/235 is fixed,
	// use IsTableSupported(nbdb.LoadBalancerGroup).
	if _, _, err := util.RunOVNNbctl("--columns=_uuid", "list", "Load_Balancer_Group"); err != nil {
		klog.Warningf("Load Balancer Group support enabled, however version of OVN in use does not support Load Balancer Groups.")
	} else {
		clusterLBGroupUUID, switchLBGroupUUID, routerLBGroupUUID, err := initLoadBalancerGroups(oc.nbClient, oc.NetInfo)
		if err != nil {
			return err
		}
		oc.clusterLoadBalancerGroupUUID = clusterLBGroupUUID
		oc.switchLoadBalancerGroupUUID = switchLBGroupUUID
		oc.routerLoadBalancerGroupUUID = routerLBGroupUUID
	}
	return nil
}

func (oc *SecondaryLayer3NetworkController) addUpdateLocalNodeEvent(node *kapi.Node, nSyncs *nodeSyncs) error {
	var hostSubnets []*net.IPNet
	var errs []error
	var err error

	_, _ = oc.localZoneNodes.LoadOrStore(node.Name, true)

	if noHostSubnet := util.NoHostSubnet(node); noHostSubnet {
		err := oc.lsManager.AddNoHostSubnetSwitch(oc.GetNetworkScopedName(node.Name))
		if err != nil {
			return fmt.Errorf("nodeAdd: error adding noHost subnet for switch %s: %w", oc.GetNetworkScopedName(node.Name), err)
		}
		return nil
	}

	klog.Infof("Adding or Updating Node %q for network %s", node.Name, oc.GetNetworkName())
	if nSyncs.syncNode {
		if hostSubnets, err = oc.addNode(node); err != nil {
			oc.addNodeFailed.Store(node.Name, true)
			oc.nodeClusterRouterPortFailed.Store(node.Name, true)
			oc.mgmtPortFailed.Store(node.Name, true)
			oc.syncZoneICFailed.Store(node.Name, true)
			oc.gatewaysFailed.Store(node.Name, true)
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
				if err := gwManager.syncNodeGateway(
					node,
					gwConfig.config,
					gwConfig.hostSubnets,
					gwConfig.hostAddrs,
					gwConfig.clusterSubnets,
					gwConfig.gwLRPIPs,
					oc.SCTPSupport,
					oc.ovnClusterLRPToJoinIfAddrs,
					gwConfig.externalIPs,
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

	if nSyncs.syncZoneIC && config.OVNKubernetesFeature.EnableInterconnect {
		if err := oc.zoneICHandler.AddLocalZoneNode(node); err != nil {
			errs = append(errs, err)
			oc.syncZoneICFailed.Store(node.Name, true)
		} else {
			oc.syncZoneICFailed.Delete(node.Name)
		}
	}

	err = utilerrors.Join(errs...)
	if err != nil {
		oc.recordNodeErrorEvent(node, err)
	}
	return err
}

func (oc *SecondaryLayer3NetworkController) addUpdateRemoteNodeEvent(node *kapi.Node, syncZoneIc bool) error {
	_, present := oc.localZoneNodes.Load(node.Name)

	if present {
		if err := oc.deleteNodeEvent(node); err != nil {
			return err
		}
	}

	var err error
	if syncZoneIc && config.OVNKubernetesFeature.EnableInterconnect {
		if err = oc.zoneICHandler.AddRemoteZoneNode(node); err != nil {
			err = fmt.Errorf("failed to add the remote zone node [%s] to the zone interconnect handler, err : %v", node.Name, err)
			oc.syncZoneICFailed.Store(node.Name, true)
		} else {
			oc.syncZoneICFailed.Delete(node.Name)
		}
	}
	return err
}

// addNodeSubnetEgressSNAT adds the SNAT on each node's ovn-cluster-router in L3 networks
// snat eth.dst == d6:cf:fd:2c:a6:44 169.254.0.12 10.128.0.0/24
// snat eth.dst == d6:cf:fd:2c:a6:44 169.254.0.12 2010:100:200::/64
// these SNATs are required for pod2Egress traffic in LGW mode and pod2SameNode traffic in SGW mode to function properly on UDNs
// SNAT Breakdown:
// match = "eth.dst == d6:cf:fd:2c:a6:44"; the MAC here is the mpX interface MAC address for this UDN
// logicalIP = "10.128.0.0/24"; which is the podsubnet for this node in L3 UDN
// externalIP = "169.254.0.12"; which is the masqueradeIP for this L3 UDN
// so all in all we want to condionally SNAT all packets that are coming from pods hosted on this node,
// which are leaving via UDN's mpX interface to the UDN's masqueradeIP.
func (oc *SecondaryLayer3NetworkController) addUDNNodeSubnetEgressSNAT(localPodSubnets []*net.IPNet, node *kapi.Node) error {
	outputPort := types.RouterToSwitchPrefix + oc.GetNetworkScopedName(node.Name)
	nats, err := oc.buildUDNEgressSNAT(localPodSubnets, outputPort, node)
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

func (oc *SecondaryLayer3NetworkController) addNode(node *kapi.Node) ([]*net.IPNet, error) {
	// Node subnet for the secondary layer3 network is allocated by cluster manager.
	// Make sure that the node is allocated with the subnet before proceeding
	// to create OVN Northbound resources.
	hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, oc.GetNetworkName())
	if err != nil || len(hostSubnets) < 1 {
		return nil, fmt.Errorf("subnet annotation in the node %q for the layer3 secondary network %s is missing : %w", node.Name, oc.GetNetworkName(), err)
	}

	err = oc.createNodeLogicalSwitch(node.Name, hostSubnets, oc.clusterLoadBalancerGroupUUID, oc.switchLoadBalancerGroupUUID)
	if err != nil {
		return nil, err
	}
	if util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() {
		if err := oc.addUDNNodeSubnetEgressSNAT(hostSubnets, node); err != nil {
			return nil, err
		}
	}
	return hostSubnets, nil
}

func (oc *SecondaryLayer3NetworkController) deleteNodeEvent(node *kapi.Node) error {
	klog.V(5).Infof("Deleting Node %q for network %s. Removing the node from "+
		"various caches", node.Name, oc.GetNetworkName())

	if err := oc.deleteNode(node.Name); err != nil {
		return err
	}

	if err := oc.gatewayManagerForNode(node.Name).Cleanup(); err != nil {
		return fmt.Errorf("failed to cleanup gateway on node %q: %w", node.Name, err)
	}
	oc.gatewayManagers.Delete(node.Name)
	oc.localZoneNodes.Delete(node.Name)

	oc.lsManager.DeleteSwitch(oc.GetNetworkScopedName(node.Name))
	oc.addNodeFailed.Delete(node.Name)
	oc.mgmtPortFailed.Delete(node.Name)
	oc.nodeClusterRouterPortFailed.Delete(node.Name)
	if config.OVNKubernetesFeature.EnableInterconnect {
		if err := oc.zoneICHandler.DeleteNode(node); err != nil {
			return err
		}
		oc.syncZoneICFailed.Delete(node.Name)
	}
	return nil
}

func (oc *SecondaryLayer3NetworkController) deleteNode(nodeName string) error {
	if err := oc.deleteNodeLogicalNetwork(nodeName); err != nil {
		return fmt.Errorf("error deleting node %s logical network: %v", nodeName, err)
	}

	return nil
}

// We only deal with cleaning up nodes that shouldn't exist here, since
// watchNodes() will be called for all existing nodes at startup anyway.
// Note that this list will include the 'join' cluster switch, which we
// do not want to delete.
func (oc *SecondaryLayer3NetworkController) syncNodes(nodes []interface{}) error {
	foundNodes := sets.New[string]()
	for _, tmp := range nodes {
		node, ok := tmp.(*kapi.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", tmp)
		}
		if util.NoHostSubnet(node) {
			continue
		}

		// Add the node to the foundNodes only if it belongs to the local zone.
		if oc.isLocalZoneNode(node) {
			foundNodes.Insert(node.Name)
			oc.localZoneNodes.Store(node.Name, true)
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

	if config.OVNKubernetesFeature.EnableInterconnect {
		if err := oc.zoneICHandler.SyncNodes(nodes); err != nil {
			return fmt.Errorf("zoneICHandler failed to sync nodes: error: %w", err)
		}
	}

	return nil
}

func (oc *SecondaryLayer3NetworkController) gatherJoinSwitchIPs() error {
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

type SecondaryL3GatewayConfig struct {
	config         *util.L3GatewayConfig
	hostSubnets    []*net.IPNet
	clusterSubnets []*net.IPNet
	gwLRPIPs       []*net.IPNet
	hostAddrs      []string
	externalIPs    []net.IP
}

func (oc *SecondaryLayer3NetworkController) nodeGatewayConfig(node *kapi.Node) (*SecondaryL3GatewayConfig, error) {
	l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s network %s L3 gateway config: %v", node.Name, oc.GetNetworkName(), err)
	}

	networkName := oc.GetNetworkName()
	networkID, err := oc.getNetworkID()
	if err != nil {
		return nil, fmt.Errorf("failed to get networkID for network %q: %v", networkName, err)
	}

	masqIPs, err := udn.GetUDNGatewayMasqueradeIPs(networkID)
	if err != nil {
		return nil, fmt.Errorf("failed to get masquerade IPs, network %s (%d): %v", networkName, networkID, err)
	}

	l3GatewayConfig.IPAddresses = append(l3GatewayConfig.IPAddresses, masqIPs...)

	// Always SNAT to the per network masquerade IP.
	var externalIPs []net.IP
	for _, masqIP := range masqIPs {
		if masqIP == nil {
			continue
		}
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

	gwLRPIPs, err := util.ParseNodeGatewayRouterJoinAddrs(node, oc.GetNetworkName())
	if err != nil {
		return nil, fmt.Errorf("failed extracting node %q GW router join subnet IP for layer3 network %q: %w", node.Name, networkName, err)
	}

	// Overwrite the primary interface ID with the correct, per-network one.
	l3GatewayConfig.InterfaceID = oc.GetNetworkScopedExtPortName(l3GatewayConfig.BridgeID, node.Name)

	return &SecondaryL3GatewayConfig{
		config:         l3GatewayConfig,
		hostSubnets:    hostSubnets,
		clusterSubnets: clusterSubnets,
		gwLRPIPs:       gwLRPIPs,
		hostAddrs:      hostAddrs,
		externalIPs:    externalIPs,
	}, nil
}

func (oc *SecondaryLayer3NetworkController) newClusterRouter() (*nbdb.LogicalRouter, error) {
	if oc.multicastSupport {
		return oc.gatewayTopologyFactory.NewClusterRouterWithMulticastSupport(
			oc.GetNetworkScopedClusterRouterName(),
			oc.NetInfo,
			oc.defaultCOPPUUID,
		)
	}
	return oc.gatewayTopologyFactory.NewClusterRouter(
		oc.GetNetworkScopedClusterRouterName(),
		oc.NetInfo,
		oc.defaultCOPPUUID,
	)
}

func (oc *SecondaryLayer3NetworkController) newGatewayManager(nodeName string) *GatewayManager {
	return NewGatewayManager(
		nodeName,
		oc.defaultCOPPUUID,
		oc.kube,
		oc.nbClient,
		oc.NetInfo,
		oc.watchFactory,
		oc.gatewayOptions()...,
	)
}

func (oc *SecondaryLayer3NetworkController) gatewayOptions() []GatewayOption {
	var opts []GatewayOption
	if oc.clusterLoadBalancerGroupUUID != "" {
		opts = append(opts, WithLoadBalancerGroups(
			oc.routerLoadBalancerGroupUUID,
			oc.clusterLoadBalancerGroupUUID,
			oc.switchLoadBalancerGroupUUID,
		))
	}
	return opts
}

func (oc *SecondaryLayer3NetworkController) gatewayManagerForNode(nodeName string) *GatewayManager {
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

func (oc *SecondaryLayer3NetworkController) StartServiceController(wg *sync.WaitGroup, runRepair bool) error {
	wg.Add(1)
	go func() {
		defer wg.Done()
		useLBGroups := oc.clusterLoadBalancerGroupUUID != ""
		// use 5 workers like most of the kubernetes controllers in the
		// kubernetes controller-manager
		err := oc.svcController.Run(5, oc.stopChan, runRepair, useLBGroups, oc.svcTemplateSupport)
		if err != nil {
			klog.Errorf("Error running OVN Kubernetes Services controller for network %s: %v", oc.GetNetworkName(), err)
		}
	}()
	return nil
}

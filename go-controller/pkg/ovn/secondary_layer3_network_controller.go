package ovn

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/subnetallocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
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
}

// RecordUpdateEvent records the udpate event on this given object.
func (h *secondaryLayer3NetworkControllerEventHandler) RecordUpdateEvent(obj interface{}) {
}

// RecordDeleteEvent records the delete event on this given object.
func (h *secondaryLayer3NetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
}

// RecordSuccessEvent records the success event on this given object.
func (h *secondaryLayer3NetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
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
		var nodeParams *nodeSyncs
		if fromRetryLoop {
			_, nodeSync := h.oc.addNodeFailed.Load(node.Name)
			_, clusterRtrSync := h.oc.nodeClusterRouterPortFailed.Load(node.Name)
			nodeParams = &nodeSyncs{syncNode: nodeSync, syncClusterRouterPort: clusterRtrSync}
		} else {
			nodeParams = &nodeSyncs{syncNode: true, syncClusterRouterPort: true}
		}

		if err := h.oc.addUpdateNodeEvent(node, nodeParams); err != nil {
			klog.Errorf("Node add failed for %s, will try again later: %v",
				node.Name, err)
			return err
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
		// determine what actually changed in this update
		_, nodeSync := h.oc.addNodeFailed.Load(newNode.Name)
		_, failed := h.oc.nodeClusterRouterPortFailed.Load(newNode.Name)
		clusterRtrSync := failed || nodeChassisChanged(oldNode, newNode) || nodeSubnetChanged(oldNode, newNode)

		return h.oc.addUpdateNodeEvent(newNode, &nodeSyncs{syncNode: nodeSync, syncClusterRouterPort: clusterRtrSync})
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

	// waitGroup per-Controller
	wg *sync.WaitGroup

	// FIXME DUAL-STACK -  Make IP Allocators more dual-stack friendly
	masterSubnetAllocator *subnetallocator.HostSubnetAllocator

	// Node-specific syncMaps used by node event handler
	addNodeFailed               sync.Map
	nodeClusterRouterPortFailed sync.Map
}

// NewSecondaryLayer3NetworkController create a new OVN controller for the given secondary layer3 NAD
func NewSecondaryLayer3NetworkController(cnci *CommonNetworkControllerInfo, netInfo util.NetInfo,
	netconfInfo util.NetConfInfo) *SecondaryLayer3NetworkController {
	stopChan := make(chan struct{})
	oc := &SecondaryLayer3NetworkController{
		BaseSecondaryNetworkController: BaseSecondaryNetworkController{
			BaseNetworkController: BaseNetworkController{
				CommonNetworkControllerInfo: *cnci,
				NetConfInfo:                 netconfInfo,
				NetInfo:                     netInfo,
				lsManager:                   lsm.NewLogicalSwitchManager(),
				logicalPortCache:            newPortCache(stopChan),
				namespaces:                  make(map[string]*namespaceInfo),
				namespacesMutex:             sync.Mutex{},
				addressSetFactory:           addressset.NewOvnAddressSetFactory(cnci.nbClient),
				stopChan:                    stopChan,
			},
		},
		wg:                          &sync.WaitGroup{},
		masterSubnetAllocator:       subnetallocator.NewHostSubnetAllocator(),
		addNodeFailed:               sync.Map{},
		nodeClusterRouterPortFailed: sync.Map{},
	}

	// disable multicast support for secondary networks
	oc.multicastSupport = false

	oc.initRetryFramework()
	return oc
}

func (oc *SecondaryLayer3NetworkController) initRetryFramework() {
	oc.retryPods = oc.newRetryFramework(factory.PodType)
	oc.retryNodes = oc.newRetryFramework(factory.NodeType)
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
	if err := oc.Init(); err != nil {
		return err
	}

	return oc.Run()
}

// Stop gracefully stops the controller, and delete all logical entities for this network if requested
func (oc *SecondaryLayer3NetworkController) Stop() {
	klog.Infof("Stop secondary %s network controller of network %s", oc.TopologyType(), oc.GetNetworkName())
	close(oc.stopChan)
	oc.wg.Wait()

	if oc.podHandler != nil {
		oc.watchFactory.RemovePodHandler(oc.podHandler)
	}

	if oc.nodeHandler != nil {
		oc.watchFactory.RemoveNodeHandler(oc.nodeHandler)
	}
}

// Cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *SecondaryLayer3NetworkController) Cleanup(netName string) error {
	// cleans up related OVN logical entities
	var ops []ovsdb.Operation
	var err error

	// remove hostsubnet annotation for this network
	klog.Infof("Remove node-subnets annotation for network %s on all nodes", netName)
	existingNodes, err := oc.watchFactory.GetNodes()
	if err != nil {
		klog.Errorf("Error in initializing/fetching subnets: %v", err)
		return nil
	}
	for _, node := range existingNodes {
		if noHostSubnet(node) {
			klog.V(5).Infof("Node %s is not managed by OVN", node.Name)
			continue
		}
		hostSubnetsMap := map[string][]*net.IPNet{netName: nil}
		err = oc.UpdateNodeHostSubnetAnnotationWithRetry(node.Name, hostSubnetsMap, nil)
		if err != nil {
			return fmt.Errorf("failed to clear node %q subnet annotation for network %s",
				node.Name, netName)
		}
	}

	klog.Infof("Delete OVN logical entities for %s network controller of network %s", types.Layer3Topology, netName)
	// first delete node logical switches
	ops, err = libovsdbops.DeleteLogicalSwitchesWithPredicateOps(oc.nbClient, ops,
		func(item *nbdb.LogicalSwitch) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting switches of network %s: %v", netName, err)
	}

	// now delete cluster router
	ops, err = libovsdbops.DeleteLogicalRoutersWithPredicateOps(oc.nbClient, ops,
		func(item *nbdb.LogicalRouter) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting routers of network %s: %v", netName, err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to deleting routers/switches of network %s: %v", netName, err)
	}
	return nil
}

func (oc *SecondaryLayer3NetworkController) Run() error {
	klog.Infof("Starting all the Watchers for network %s ...", oc.GetNetworkName())
	start := time.Now()

	if err := oc.WatchNodes(); err != nil {
		return err
	}

	if err := oc.WatchPods(); err != nil {
		return err
	}

	klog.Infof("Completing all the Watchers for network %s took %v", oc.GetNetworkName(), time.Since(start))

	// controller is fully running and resource handlers have synced, update Topology version in OVN
	if err := oc.updateL3TopologyVersion(); err != nil {
		return fmt.Errorf("failed to update topology version for network %s: %v", oc.GetNetworkName(), err)
	}

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

func (oc *SecondaryLayer3NetworkController) Init() error {
	klog.Infof("Allocating subnets")
	layer3NetConfInfo := oc.NetConfInfo.(*util.Layer3NetConfInfo)
	if err := oc.masterSubnetAllocator.InitRanges(layer3NetConfInfo.ClusterSubnets); err != nil {
		klog.Errorf("Failed to initialize host subnet allocator ranges: %v", err)
		return err
	}

	_, err := oc.createOvnClusterRouter()
	return err
}

func (oc *SecondaryLayer3NetworkController) addUpdateNodeEvent(node *kapi.Node, nSyncs *nodeSyncs) error {
	var hostSubnets []*net.IPNet
	var errs []error
	var err error

	if noHostSubnet := noHostSubnet(node); noHostSubnet {
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

	// ensure pods that already exist on this node have their logical ports created
	if nSyncs.syncNode { // do this only if it is a new node add
		errors := oc.addAllPodsOnNode(node.Name)
		errs = append(errs, errors...)
	}

	err = kerrors.NewAggregate(errs)
	if err != nil {
		oc.recordNodeErrorEvent(node, err)
	}
	return err
}

func (oc *SecondaryLayer3NetworkController) addNode(node *kapi.Node) ([]*net.IPNet, error) {
	hostSubnets, err := oc.allocateNodeSubnets(node, oc.masterSubnetAllocator)
	if err != nil {
		return nil, err
	}

	hostSubnetsMap := map[string][]*net.IPNet{oc.GetNetworkName(): hostSubnets}
	err = oc.UpdateNodeHostSubnetAnnotationWithRetry(node.Name, hostSubnetsMap, nil)
	if err != nil {
		return nil, err
	}

	err = oc.createNodeLogicalSwitch(node.Name, hostSubnets, "")
	if err != nil {
		return nil, err
	}
	return hostSubnets, nil
}

func (oc *SecondaryLayer3NetworkController) deleteNodeEvent(node *kapi.Node) error {
	klog.V(5).Infof("Deleting Node %q for network %s. Removing the node from "+
		"various caches", node.Name, oc.GetNetworkName())

	if err := oc.deleteNode(node.Name); err != nil {
		return err
	}

	oc.lsManager.DeleteSwitch(oc.GetNetworkScopedName(node.Name))
	oc.addNodeFailed.Delete(node.Name)
	oc.nodeClusterRouterPortFailed.Delete(node.Name)
	return nil
}

func (oc *SecondaryLayer3NetworkController) deleteNode(nodeName string) error {
	oc.masterSubnetAllocator.ReleaseAllNodeSubnets(nodeName)

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
	foundNodes := sets.NewString()
	for _, tmp := range nodes {
		node, ok := tmp.(*kapi.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", tmp)
		}
		_ = oc.updateNodesManageHostSubnets(node, oc.masterSubnetAllocator, foundNodes)
	}

	p := func(item *nbdb.LogicalSwitch) bool {
		return len(item.OtherConfig) > 0 && item.ExternalIDs[types.NetworkExternalID] == oc.GetNetworkName()
	}
	nodeSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(oc.nbClient, p)
	if err != nil {
		return fmt.Errorf("failed to get node logical switches which have other-config set for network %s: %v", oc.GetNetworkName(), err)
	}
	for _, nodeSwitch := range nodeSwitches {
		if !strings.HasPrefix(nodeSwitch.Name, oc.GetPrefix()) {
			klog.Errorf("Node switch name %s unexpected, expect prefixed with %s", nodeSwitch.Name, oc.GetPrefix())
			continue
		}
		nodeName := strings.Trim(nodeSwitch.Name, oc.GetPrefix())
		if !foundNodes.Has(nodeName) {
			if err := oc.deleteNode(nodeName); err != nil {
				return fmt.Errorf("failed to delete node:%s, err:%v", nodeName, err)
			}
		}
	}
	return nil
}

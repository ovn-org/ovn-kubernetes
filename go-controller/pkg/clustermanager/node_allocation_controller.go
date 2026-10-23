// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sync"

	corev1 "k8s.io/api/core/v1"
	cache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	ipgenerator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	objretry "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// Maximum node IDs that can be generated. Limited to maximum nodes supported by k8s.
	maxNodeIDs = 5000
)

// nodeAllocationController allocates node IDs and transit switch port addresses.
type nodeAllocationController struct {
	kube         kube.Interface
	watchFactory *factory.WatchFactory
	stopChan     chan struct{}
	wg           *sync.WaitGroup

	// node events factory handler
	nodeHandler *factory.Handler

	// retry framework for nodes
	retryNodes *objretry.RetryFramework

	// ID allocator for the nodes
	nodeIDAllocator id.Allocator

	// Transit switch IP generators allocate node transit switch port addresses.
	transitSwitchIPv4Generator *ipgenerator.IPGenerator
	transitSwitchIPv6Generator *ipgenerator.IPGenerator
}

func newNodeAllocationController(ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory) (*nodeAllocationController, error) {
	// Since we don't assign 0 to any node, create IDAllocator with one extra element in maxIds.
	nodeIDAllocator := id.NewIDAllocator("NodeIDs", maxNodeIDs+1)
	// Reserve the id 0. We don't want to assign this id to any of the nodes.
	if err := nodeIDAllocator.ReserveID("zero", 0); err != nil {
		return nil, fmt.Errorf("idAllocator failed to reserve id 0")
	}
	if err := nodeIDAllocator.ReserveID("one", 1); err != nil {
		return nil, fmt.Errorf("idAllocator failed to reserve id 1")
	}

	kube := &kube.Kube{
		KClient: ovnClient.KubeClient,
	}
	wg := &sync.WaitGroup{}

	var transitSwitchIPv4Generator, transitSwitchIPv6Generator *ipgenerator.IPGenerator
	var err error
	if config.IPv4Mode {
		transitSwitchIPv4Generator, err = ipgenerator.NewIPGenerator(config.ClusterManager.V4TransitSubnet)
		if err != nil {
			return nil, fmt.Errorf("error creating IP Generator for v4 transit subnet %s: %w", config.ClusterManager.V4TransitSubnet, err)
		}
	}

	if config.IPv6Mode {
		transitSwitchIPv6Generator, err = ipgenerator.NewIPGenerator(config.ClusterManager.V6TransitSubnet)
		if err != nil {
			return nil, fmt.Errorf("error creating IP Generator for v6 transit subnet %s: %w", config.ClusterManager.V6TransitSubnet, err)
		}
	}

	nac := &nodeAllocationController{
		kube:                       kube,
		watchFactory:               wf,
		stopChan:                   make(chan struct{}),
		wg:                         wg,
		nodeIDAllocator:            nodeIDAllocator,
		transitSwitchIPv4Generator: transitSwitchIPv4Generator,
		transitSwitchIPv6Generator: transitSwitchIPv6Generator,
	}

	nac.initRetryFramework()
	return nac, nil
}

func (nac *nodeAllocationController) initRetryFramework() {
	// We are interested in only nodes
	resourceHandler := &objretry.ResourceHandler{
		HasUpdateFunc:          true,
		NeedsUpdateDuringRetry: false,
		ObjType:                factory.NodeType,
		EventHandler: &nodeAllocationControllerEventHandler{
			objType:  factory.NodeType,
			nac:      nac,
			syncFunc: nil,
		},
	}

	nac.retryNodes = objretry.NewRetryFramework("nodeAllocationController", nac.stopChan, nac.wg, nac.watchFactory, resourceHandler)
}

// Start starts the node allocation controller to watch the kubernetes nodes
func (nac *nodeAllocationController) Start(_ context.Context) error {
	nodeHandler, err := nac.retryNodes.WatchResource()

	if err != nil {
		return fmt.Errorf("unable to watch nodes: %w", err)
	}

	nac.nodeHandler = nodeHandler
	return nil
}

func (nac *nodeAllocationController) Stop() {
	close(nac.stopChan)
	nac.wg.Wait()

	if nac.nodeHandler != nil {
		nac.watchFactory.RemoveNodeHandler(nac.nodeHandler)
	}
}

func needsNodeAllocation(node *corev1.Node) bool {
	if config.HybridOverlay.Enabled && util.NoHostSubnet(node) {
		// skip hybrid overlay nodes
		return false
	}

	if _, ok := node.Annotations[util.OvnNodeID]; !ok {
		return true
	}
	if _, ok := node.Annotations[util.OvnTransitSwitchPortAddr]; !ok {
		return true
	}
	return false
}

// handleAddUpdateNodeEvent handles the add or update node event
func (nac *nodeAllocationController) handleAddUpdateNodeEvent(ctx context.Context, node *corev1.Node) error {
	if config.HybridOverlay.Enabled && util.NoHostSubnet(node) {
		// skip hybrid overlay nodes
		return nil
	}
	allocatedNodeID, err := nac.nodeIDAllocator.AllocateID(node.Name)
	if err != nil {
		return fmt.Errorf("failed to allocate an id to the node %s : err - %w", node.Name, err)
	}
	klog.V(5).Infof("Allocated id %d to the node %s", allocatedNodeID, node.Name)
	nodeAnnotations := util.UpdateNodeIDAnnotation(nil, allocatedNodeID)

	// Allocate the IP address(es) for the node Gateway router port connecting
	// to the Join switch
	var v4Addr, v6Addr *net.IPNet

	if config.IPv4Mode {
		v4Addr, err = nac.transitSwitchIPv4Generator.GenerateIP(allocatedNodeID)
		if err != nil {
			return fmt.Errorf("failed to generate transit switch port IPv4 address for node %s : err - %w", node.Name, err)
		}
	}

	if config.IPv6Mode {
		v6Addr, err = nac.transitSwitchIPv6Generator.GenerateIP(allocatedNodeID)
		if err != nil {
			return fmt.Errorf("failed to generate transit switch port IPv6 address for node %s : err - %w", node.Name, err)
		}
	}

	nodeAnnotations, err = util.CreateNodeTransitSwitchPortAddrAnnotation(nodeAnnotations, v4Addr, v6Addr)
	if err != nil {
		return fmt.Errorf("failed to marshal node %q transit switch port IP annotation, err : %w",
			node.Name, err)
	}

	return nac.kube.SetAnnotationsOnNode(ctx, node.Name, nodeAnnotations)
}

// handleDeleteNode handles the delete node event.
func (nac *nodeAllocationController) handleDeleteNode(node *corev1.Node) error {
	nac.nodeIDAllocator.ReleaseID(node.Name)
	return nil
}

func (nac *nodeAllocationController) syncNodes(nodes []interface{}) error {
	return nac.syncNodeIDs(nodes)
}

func (nac *nodeAllocationController) syncNodeIDs(nodes []interface{}) error {
	duplicateIdNodes := []string{}

	for _, nodeObj := range nodes {
		node, ok := nodeObj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", nodeObj)
		}

		nodeID, _ := util.GetNodeID(node)
		if nodeID != util.InvalidNodeID {
			klog.Infof("Node %s has the id %d set", node.Name, nodeID)
			if err := nac.nodeIDAllocator.ReserveID(node.Name, nodeID); err != nil {
				// The id set on this node is duplicate.
				klog.Infof("Node %s has a duplicate id %d set", node.Name, nodeID)
				duplicateIdNodes = append(duplicateIdNodes, node.Name)
			}
		}
	}

	for i := range duplicateIdNodes {
		newNodeID, err := nac.nodeIDAllocator.AllocateID(duplicateIdNodes[i])
		if err != nil {
			return fmt.Errorf("failed to allocate id for node %s : err - %w", duplicateIdNodes[i], err)
		} else {
			klog.Infof("Allocated new id %d for node %q", newNodeID, duplicateIdNodes[i])
		}
	}

	return nil
}

// nodeAllocationControllerEventHandler object handles the events
// from retry framework.
type nodeAllocationControllerEventHandler struct {
	objretry.DefaultEventHandler

	objType  reflect.Type
	nac      *nodeAllocationController
	syncFunc func([]interface{}) error

	nodeSyncFailed sync.Map
}

func (h *nodeAllocationControllerEventHandler) FilterOutResource(_ interface{}) bool {
	return false
}

// nodeAllocationControllerEventHandler functions

// AddResource adds the specified object to the cluster according to its type and
// returns the error, if any, yielded during object creation.
func (h *nodeAllocationControllerEventHandler) AddResource(ctx context.Context, obj interface{}, _ bool) error {
	var err error

	switch h.objType {
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Node", obj)
		}
		if err = h.nac.handleAddUpdateNodeEvent(ctx, node); err != nil {
			h.nodeSyncFailed.Store(node.Name, true)
			return fmt.Errorf("node add failed for %s, will try again later: %w",
				node.Name, err)
		}
		h.nodeSyncFailed.Delete(node.Name)
	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}
	return nil
}

// UpdateResource updates the specified object in the cluster to its version in newObj according
// to its type and returns the error, if any, yielded during the object update.
// The inRetryCache boolean argument is to indicate if the given resource is in the retryCache or not.
func (h *nodeAllocationControllerEventHandler) UpdateResource(ctx context.Context, _, newObj interface{}, _ bool) error {
	var err error

	switch h.objType {
	case factory.NodeType:
		node, ok := newObj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *corev1.Node", newObj)
		}
		_, nodeFailed := h.nodeSyncFailed.Load(node.GetName())
		if !nodeFailed && !needsNodeAllocation(node) {
			// node ID and transit switch IP are assigned by us and cannot change
			return nil
		}
		if err = h.nac.handleAddUpdateNodeEvent(ctx, node); err != nil {
			return fmt.Errorf("node update failed for %s, will try again later: %w",
				node.Name, err)
		}
		h.nodeSyncFailed.Delete(node.GetName())
	default:
		return fmt.Errorf("no update function for object type %s", h.objType)
	}
	return nil
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// cachedObj is the internal cache entry for this object, used for now for pods and network policies.
func (h *nodeAllocationControllerEventHandler) DeleteResource(_ context.Context, obj, _ interface{}) error {
	switch h.objType {
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.Node", obj)
		}
		err := h.nac.handleDeleteNode(node)
		if err != nil {
			return err
		}
		h.nodeSyncFailed.Delete(node.GetName())
	}
	return nil
}

func (h *nodeAllocationControllerEventHandler) SyncFunc(_ context.Context, objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.NodeType:
			syncFunc = h.nac.syncNodes

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

func (h *nodeAllocationControllerEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	// switch based on type
	if h.objType == factory.NodeType {
		node1, ok := obj1.(*corev1.Node)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *corev1.Node", obj1)
		}
		node2, ok := obj2.(*corev1.Node)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *corev1.Node", obj2)
		}

		// Check if the annotations have changed.
		if util.NodeIDAnnotationChanged(node1, node2) {
			return false, nil
		}
		if util.NodeTransitSwitchPortAddrAnnotationChanged(node1, node2) {
			return false, nil
		}
		// Check if a node is switched between ho node to ovn node
		if util.NoHostSubnet(node1) != util.NoHostSubnet(node2) {
			return false, nil
		}
		return true, nil
	}

	return false, nil
}

// GetResourceFromInformerCache returns the latest state of the object from the informers cache
// given an object key and its type
func (h *nodeAllocationControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var name string
	var err error

	_, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %w", key, err)
	}

	switch h.objType {
	case factory.NodeType:
		obj, err = h.nac.watchFactory.GetNode(name)

	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			h.objType)
	}
	return obj, err
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	v1 "k8s.io/client-go/listers/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// NodeHandler handles node reconciliation for a single network.
type NodeHandler interface {
	// GetNetworkName returns the network this handler reconciles.
	GetNetworkName() string
	// ReconcileNode reconciles the network-specific state for a node. oldNode and
	// oldState may be nil when the node is first seen for the network or becomes
	// active again. For delete reconciliation, oldNode is the best available
	// prior object: a real cached node when available, otherwise a name-only
	// stub node; oldState is nil when no cached prior annotations exist. newNode
	// and newState describe the latest desired state; they are nil when
	// network-specific state for the node should be deleted.
	ReconcileNode(ctx context.Context, oldNode, newNode *corev1.Node, oldState, newState *NodeAnnotationState) error
	// SyncNodes performs the initial full-network sync before per-node
	// reconciliation is queued for the handler.
	SyncNodes(nodes []*corev1.Node) error
}

// NodeController reconciles node topology for all registered networks.
type NodeController struct {
	name     string
	nodeName string

	nodeController controller.Controller
	networkManager networkmanager.Interface
	nodeLister     v1.NodeLister

	// handlers maps network name to node handler.
	handlers *syncmap.SyncMap[NodeHandler]

	// stateMu protects nodeReconciliation, nodeActive, nodeNetworks, nodeCache,
	// and latestInformerNodeCache.
	stateMu sync.RWMutex
	// nodeReconciliation tracks nodes that should be treated as "new" per network.
	// keyed by network -> nodes
	// bool is true when there was a previous reconciliation attempt with a delete event
	nodeReconciliation map[string]map[string]bool
	// nodeActive tracks whether a node/network is active
	// keyed by network -> nodes
	// presence indicates active
	// absence indicates inactive/does not exist
	nodeActive map[string]map[string]struct{}
	// nodeNetworks is a reverse index of active networks per node.
	// keyed by node -> networks
	nodeNetworks map[string]map[string]struct{}
	// nodeCache contains configured node state
	// keyed by network -> nodeName
	nodeCache map[string]map[string]*corev1.Node
	// latestInformerNodeCache contains the latest informer object seen for a node
	// even if reconciliation failed. It is only used as a delete fallback when
	// no configured node state exists in nodeCache.
	// keyed by network -> nodeName
	latestInformerNodeCache map[string]map[string]*corev1.Node
	// annotationCache stores parsed annotation maps keyed by node.
	annotationCache *NodeAnnotationCache

	startMu sync.Mutex
	started bool
}

const scopedNodeQueueKeySeparator = "|"

// NewController builds a shared node controller.
func NewController(wf *factory.WatchFactory, name string, networkManager networkmanager.Interface) *NodeController {
	if networkManager == nil {
		panic("node controller network manager must not be nil")
	}
	nodeInformer := wf.NodeCoreInformer()
	c := &NodeController{
		name:                    name,
		networkManager:          networkManager,
		nodeLister:              nodeInformer.Lister(),
		handlers:                syncmap.NewSyncMap[NodeHandler](),
		nodeReconciliation:      map[string]map[string]bool{},
		nodeActive:              map[string]map[string]struct{}{},
		nodeNetworks:            map[string]map[string]struct{}{},
		nodeCache:               map[string]map[string]*corev1.Node{},
		latestInformerNodeCache: map[string]map[string]*corev1.Node{},
		annotationCache:         NewNodeAnnotationCache(),
	}

	nodeControllerConfig := &controller.ControllerConfig[corev1.Node]{
		Informer:       nodeInformer.Informer(),
		Lister:         nodeInformer.Lister().List,
		MaxAttempts:    controller.InfiniteAttempts,
		ObjNeedsUpdate: func(_, _ *corev1.Node) bool { return true },
		Reconcile:      c.reconcileNode,
		Threadiness:    15,
	}
	c.nodeController = controller.NewController(c.name+"-node", nodeControllerConfig)

	return c
}

// NewNodeController builds a controller that handles node events for all UDNs.
func NewNodeController(wf *factory.WatchFactory, networkManager networkmanager.Interface, nodeName string) *NodeController {
	if nodeName == "" {
		panic("node controller node name must not be empty")
	}
	c := NewController(wf, "node-topology", networkManager)
	c.nodeName = nodeName
	return c
}

// Start starts the node worker.
func (c *NodeController) Start() error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.started {
		return nil
	}

	err := controller.Start(c.nodeController)
	if err != nil {
		return err
	}

	c.started = true
	return nil
}

// Stop stops the node worker.
func (c *NodeController) Stop() {
	c.startMu.Lock()
	c.started = false
	c.startMu.Unlock()
	controller.Stop(c.nodeController)
}

// ReconcileNetwork queues reconciliation for a single node/network pair.
func (c *NodeController) ReconcileNetwork(nodeName, netName string) {
	c.nodeController.Reconcile(scopedNodeQueueKey(nodeName, netName))
}

// ReconcileNetworkWithPreflight runs preflight against the network handler and
// queues reconciliation for a single node/network pair. Callers use this for
// external state that must update handler-local state before reconciliation.
func (c *NodeController) ReconcileNetworkWithPreflight(
	nodeName string,
	netName string,
	preflight func(NodeHandler),
) {
	_ = c.handlers.DoWithLock(netName, func(handlerKey string) error {
		if handler, ok := c.handlers.Load(handlerKey); ok {
			preflight(handler)
		}
		return nil
	})
	c.ReconcileNetwork(nodeName, netName)
}

// AnnotationCache returns the cache used for parsed node annotations.
func (c *NodeController) AnnotationCache() *NodeAnnotationCache {
	return c.annotationCache
}

// RegisterNetworkController registers or replaces a per-network node handler.
// Registration is the activation signal for node handling.
func (c *NodeController) RegisterNetworkController(handler NodeHandler) error {
	if handler == nil {
		return fmt.Errorf("%s: nil node handler registration", c.name)
	}
	netName := handler.GetNetworkName()
	return c.handlers.DoWithLock(netName, func(key string) error {
		if existing, ok := c.handlers.Load(key); ok && existing != nil {
			panic(fmt.Sprintf("%s: duplicate node handler registration for network %q", c.name, key))
		}
		c.handlers.Store(key, handler)
		if err := c.bootstrapNetwork(key, handler); err != nil {
			c.handlers.Delete(key)
			return fmt.Errorf("%s: failed to bootstrap network %s: %w", c.name, netName, err)
		}
		return nil
	})
}

// DeregisterNetworkController removes a per-network node handler and clears
// associated network state. Note, OVN cleanup for nodes will be executed by the handler.
func (c *NodeController) DeregisterNetworkController(netName string) {
	_ = c.handlers.DoWithLock(netName, func(key string) error {
		c.handlers.Delete(key)
		c.stateMu.Lock()
		delete(c.nodeReconciliation, key)
		if nodes, ok := c.nodeActive[key]; ok {
			for nodeName := range nodes {
				if networks, ok := c.nodeNetworks[nodeName]; ok {
					delete(networks, key)
					if len(networks) == 0 {
						delete(c.nodeNetworks, nodeName)
					}
				}
			}
		}
		delete(c.nodeActive, key)
		delete(c.nodeCache, key)
		delete(c.latestInformerNodeCache, key)
		c.stateMu.Unlock()
		return nil
	})
}

// reconcileNode handles node add/update/delete by comparing cached state.
func (c *NodeController) reconcileNode(ctx context.Context, key string) error {
	nodeName, netName := parseScopedNodeQueueKey(key)
	// if netName is empty, then we always requeue keys based on network.
	// This trick allows us to always process each network individually, allowing us to retry only
	// specific networks, rather than retrying all later.
	if netName == "" {
		handlerKeys := c.handlers.GetKeys()
		for _, networkName := range handlerKeys {
			c.ReconcileNetwork(nodeName, networkName)
		}
		return nil
	}

	return c.handlers.DoWithLock(netName, func(handlerKey string) error {
		handler, ok := c.handlers.Load(handlerKey)
		if !ok || handler == nil {
			return nil
		}

		// Special case where we know we need to reconcile a node delete.
		// We handle delete of old object first, before we try to reconcile update.
		// This is the same behavior as the old retry framework for nodes.
		needsDelete := c.nodeNeedsDeleteReconciliation(netName, nodeName)
		needsAddUpdate := true

		// Check for node existence
		newNode, err := c.nodeLister.Get(nodeName)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}

		// We don't normally update the anno cache on delete.
		// Only exception is on a true node delete, where it makes sense to update
		// cache for all network deletes
		updateAnnoCacheOnDelete := false
		if newNode == nil {
			needsDelete = true
			needsAddUpdate = false
			updateAnnoCacheOnDelete = true
		}

		oldNode := c.getCachedNode(netName, nodeName)
		nodeHadNetwork := c.nodeHasNetwork(netName, nodeName)
		nodeHasNetwork := c.networkManager.NodeHasNetwork(nodeName, netName)

		if c.shouldFilterByRemoteNetworkActivity(newNode, netName) || c.shouldFilterByRemoteNetworkActivity(oldNode, netName) {
			// If the node is going inactive we need to delete it and not update
			if nodeHadNetwork && !nodeHasNetwork {
				needsAddUpdate = false
				needsDelete = true
				c.markNodeNeedsDeleteReconciliation(netName, nodeName)
				c.deleteNodeActive(netName, nodeName)
				// If we have no configured node in cache, retain the latest informer object
				// so delete reconciliation has the metadata needed for cleanup.
				if newNode != nil && oldNode == nil {
					oldNode = newNode
					c.setLatestInformerNode(netName, oldNode)
				}
			} else if !nodeHadNetwork && nodeHasNetwork {
				// node going active, but do not purge delete state (may need to retry previous failed delete)
				needsAddUpdate = true
				c.setNodeActive(netName, nodeName)
				// set node for reconciliation in case we fail this add/update so we force a retry later
				c.markNodeNeedsReconciliation(netName, nodeName)
			}
		} else if nodeHasNetwork {
			c.setNodeActive(netName, nodeName)
		} else {
			c.deleteNodeActive(netName, nodeName)
		}

		oldState := c.annotationCache.updateNodeAnnotationState(oldNode, updateAnnoCacheOnDelete)
		if needsDelete {
			if delErr := c.reconcileDelete(ctx, handler, nodeName, netName, oldNode, oldState); delErr != nil {
				return fmt.Errorf("%s: failed to delete node %s for network %s: %w", c.name, nodeName, netName, delErr)
			}
		}

		if !needsAddUpdate || newNode == nil {
			// remove any previous mark for needing an add reconciliation
			c.deleteNodeReconciliation(netName, nodeName)
			return nil
		}

		newState := c.annotationCache.updateNodeAnnotationState(newNode, true)

		// if we are marked for reconciliation (first time going active/bootstrapping) treat as an add
		if c.nodeNeedsReconciliation(netName, nodeName) {
			oldNode = nil
			oldState = nil
		}

		return c.reconcileUpdate(ctx, handler, oldNode, newNode, netName, oldState, newState)
	})
}

// reconcileUpdate reconciles per-network node changes.
// oldNode is derived from this controller's internal cache.
// newNode is the latest state of the node from informer cache.
// Reconciliation is level-driven.
func (c *NodeController) reconcileUpdate(ctx context.Context, handler NodeHandler, oldNode, newNode *corev1.Node, netName string, oldState, newState *NodeAnnotationState) error {
	err := handler.ReconcileNode(ctx, oldNode, newNode, oldState, newState)
	// Preserve the latest informer object for delete cleanup without
	// overwriting the last successfully applied node state.
	c.setLatestInformerNode(netName, newNode)
	if err != nil {
		return err
	}

	// successful update, now update caches
	c.setCachedNode(netName, newNode)
	c.deleteNodeReconciliation(netName, newNode.Name)

	return nil
}

// reconcileDelete handles deletion using cached state.
func (c *NodeController) reconcileDelete(ctx context.Context, handler NodeHandler, nodeName, netName string, oldNode *corev1.Node, oldState *NodeAnnotationState) error {
	if oldNode == nil {
		oldNode = c.getLatestInformerNode(netName, nodeName)
		oldState = c.annotationCache.updateNodeAnnotationState(oldNode, false)
	}
	// if there is no cached node (previously configured) or no informer cached node
	// then we create a dummy node to at least attempt some best-effort cleanup
	if oldNode == nil {
		oldNode = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
		}
	}
	if err := handler.ReconcileNode(ctx, oldNode, nil, oldState, nil); err != nil {
		return err
	}

	// Update per network maps
	c.deleteNodeActive(netName, nodeName)
	c.clearNodeDeleteReconciliation(netName, nodeName)
	c.deleteCachedNode(netName, nodeName)
	c.deleteLatestInformerNode(netName, nodeName)

	// We delete nodes per network, so we need to clear global caches when no networks reference it anymore.
	// A cheap trick to do this is to leverage a map that is referenced by network.
	if c.nodeHasAnyNetwork(nodeName) {
		return nil
	}

	// node is no longer being used by any network cleanup
	c.annotationCache.deleteNode(nodeName)
	return nil
}

// bootstrapNetwork handles syncing, initializing bootstrap node cache, and queuing up nodes for reconciliation
func (c *NodeController) bootstrapNetwork(netName string, handler NodeHandler) error {
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return err
	}
	if err := handler.SyncNodes(nodes); err != nil {
		return err
	}

	c.setBootstrapNodes(netName, nodes)
	for _, node := range nodes {
		c.nodeController.Reconcile(scopedNodeQueueKey(node.Name, netName))
	}
	return nil
}

// setBootstrapNodes stores bootstrap node tracking for a network.
func (c *NodeController) setBootstrapNodes(netName string, nodes []*corev1.Node) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if len(nodes) != 0 {
		nodeSet := make(map[string]bool, len(nodes))
		// setup reconciliation to emulate adds
		for _, node := range nodes {
			nodeSet[node.Name] = false
		}
		c.nodeReconciliation[netName] = nodeSet
	}
}

// nodeNeedsReconciliation returns true if a node should be treated as new for a network.
func (c *NodeController) nodeNeedsReconciliation(netName, nodeName string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	nodes := c.nodeReconciliation[netName]
	if len(nodes) == 0 {
		return false
	}
	_, ok := nodes[nodeName]
	return ok
}

// nodeNeedsDeleteReconciliation returns true if a node should be treated as new for a network.
func (c *NodeController) nodeNeedsDeleteReconciliation(netName, nodeName string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	nodes := c.nodeReconciliation[netName]
	if len(nodes) == 0 {
		return false
	}
	if needsDelete, ok := nodes[nodeName]; ok && needsDelete {
		return true
	}

	return false
}

// clearNodeDeleteReconciliation resets node reconciliation needs delete flag
func (c *NodeController) clearNodeDeleteReconciliation(netName, nodeName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	nodes := c.nodeReconciliation[netName]
	if len(nodes) == 0 {
		return
	}
	if _, ok := nodes[nodeName]; ok {
		nodes[nodeName] = false
	}

}

// deleteNodeReconciliation removes the node from needing reconciliation for a network
func (c *NodeController) deleteNodeReconciliation(netName, nodeName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	nodes := c.nodeReconciliation[netName]
	if len(nodes) == 0 {
		return
	}
	delete(nodes, nodeName)

	if len(nodes) == 0 {
		delete(c.nodeReconciliation, netName)
	}
}

// markNodeNeedsReconciliation marks the node as needing reconciliation
func (c *NodeController) markNodeNeedsReconciliation(netName, nodeName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	nodes := c.nodeReconciliation[netName]
	if nodes == nil {
		nodes = map[string]bool{}
		c.nodeReconciliation[netName] = nodes
	}
	// if it already exists, do nothing
	if _, ok := nodes[nodeName]; ok {
		return
	}
	// doesn't exist, set it with false indicating there is no pending delete
	nodes[nodeName] = false
}

// markNodeNeedsDeleteReconciliation marks the node as needing reconciliation with delete
func (c *NodeController) markNodeNeedsDeleteReconciliation(netName, nodeName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	nodes := c.nodeReconciliation[netName]
	if nodes == nil {
		nodes = map[string]bool{}
		c.nodeReconciliation[netName] = nodes
	}
	nodes[nodeName] = true
}

// nodeHasAnyNetwork returns whether there is any cached state for this node on any network.
func (c *NodeController) nodeHasAnyNetwork(nodeName string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return len(c.nodeNetworks[nodeName]) > 0
}

// nodeHasNetwork returns whether the last cached state of the nodeName/netName was active/configured.
func (c *NodeController) nodeHasNetwork(netName, nodeName string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	nodes := c.nodeActive[netName]
	if len(nodes) == 0 {
		return false
	}
	_, ok := nodes[nodeName]
	return ok
}

// setNodeActive records whether the last successfully applied state
// for a node/network was active/configured.
func (c *NodeController) setNodeActive(netName, nodeName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.nodeActive == nil {
		c.nodeActive = map[string]map[string]struct{}{}
	}
	if c.nodeNetworks == nil {
		c.nodeNetworks = map[string]map[string]struct{}{}
	}
	nodes := c.nodeActive[netName]
	if nodes == nil {
		nodes = map[string]struct{}{}
		c.nodeActive[netName] = nodes
	}
	nodes[nodeName] = struct{}{}
	networks := c.nodeNetworks[nodeName]
	if networks == nil {
		networks = map[string]struct{}{}
		c.nodeNetworks[nodeName] = networks
	}
	networks[netName] = struct{}{}
}

// deleteNodeNetworkState removes configured-state tracking across all networks.
func (c *NodeController) deleteNodeActive(netName, nodeName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if nodes, ok := c.nodeActive[netName]; ok {
		delete(nodes, nodeName)
		if len(nodes) == 0 {
			delete(c.nodeActive, netName)
		}
	}
	if networks, ok := c.nodeNetworks[nodeName]; ok {
		delete(networks, netName)
		if len(networks) == 0 {
			delete(c.nodeNetworks, nodeName)
		}
	}
}

// shouldFilterByRemoteNetworkActivity returns true when dynamic UDN activity
// filtering should be applied for the node. This is limited to remote nodes;
// the controller's own node always runs unfiltered reconciliation.
func (c *NodeController) shouldFilterByRemoteNetworkActivity(node *corev1.Node, netName string) bool {
	if node == nil || netName == types.DefaultNetworkName || !config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		return false
	}
	return node.Name != c.nodeName
}

// scopedNodeQueueKey allows us to queue keys with network references.
// This allows us to queue node updates for only specific networks.
// Useful for when networks first start up and register.
func scopedNodeQueueKey(nodeName, netName string) string {
	if netName == "" {
		return nodeName
	}
	return nodeName + scopedNodeQueueKeySeparator + netName
}

func parseScopedNodeQueueKey(key string) (nodeName, netName string) {
	parts := strings.SplitN(key, scopedNodeQueueKeySeparator, 2)
	if len(parts) != 2 {
		return key, ""
	}
	if parts[0] == "" || parts[1] == "" {
		return key, ""
	}
	return parts[0], parts[1]
}

// getCachedNode returns a cached node by name for read-only access.
// WARNING: The returned node MUST NOT be modified. It is a shared reference
// from the controller cache. All verified usages are read-only.
func (c *NodeController) getCachedNode(netName, nodeName string) *corev1.Node {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.nodeCache[netName][nodeName]
}

// setCachedNode stores a reference to the node in cache.
// The node is from the k8s informer cache (immutable), safe to cache by reference.
func (c *NodeController) setCachedNode(netName string, node *corev1.Node) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	if c.nodeCache == nil {
		c.nodeCache = make(map[string]map[string]*corev1.Node)
	}
	if c.nodeCache[netName] == nil {
		c.nodeCache[netName] = make(map[string]*corev1.Node)
	}

	c.nodeCache[netName][node.Name] = node
}

// getLatestInformerNode returns the latest informer node for read-only access.
// WARNING: The returned node MUST NOT be modified. It is a shared reference.
func (c *NodeController) getLatestInformerNode(netName, nodeName string) *corev1.Node {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.latestInformerNodeCache[netName][nodeName]
}

// setLatestInformerNode stores a reference to the latest informer node in cache.
// The node is from the k8s informer cache (immutable), safe to cache by reference.
func (c *NodeController) setLatestInformerNode(netName string, node *corev1.Node) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	if c.latestInformerNodeCache == nil {
		c.latestInformerNodeCache = make(map[string]map[string]*corev1.Node)
	}
	if c.latestInformerNodeCache[netName] == nil {
		c.latestInformerNodeCache[netName] = make(map[string]*corev1.Node)
	}

	c.latestInformerNodeCache[netName][node.Name] = node
}

// Delete removes a node from the cache.
func (c *NodeController) deleteCachedNode(netName, nodeName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	nodes := c.nodeCache[netName]
	if nodes == nil {
		return
	}

	delete(nodes, nodeName)

	if len(nodes) == 0 {
		delete(c.nodeCache, netName)
	}
}

func (c *NodeController) deleteLatestInformerNode(netName, nodeName string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	nodes := c.latestInformerNodeCache[netName]
	if nodes == nil {
		return
	}

	delete(nodes, nodeName)

	if len(nodes) == 0 {
		delete(c.latestInformerNodeCache, netName)
	}
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	ralisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/listers/routeadvertisements/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// defaultNetworkReconcilerThreadiness defines the number of worker goroutines
	// for processing network reconciliation events. Value of 5 provides good
	// parallelism for bulk UDN creation while controlling resource consumption.
	// Aligns with Kubernetes controller-manager conventions and UDN service controller.
	defaultNetworkReconcilerThreadiness = 5
)

func newNetworkController(name, node string, cm ControllerManager, wf watchFactory, asyncNetworkStart bool) *networkController {
	nc := &networkController{
		name:                   fmt.Sprintf("[%s network controller]", name),
		node:                   node,
		cm:                     cm,
		asyncNetworkStart:      asyncNetworkStart,
		networks:               map[string]util.MutableNetInfo{},
		networksByID:           map[int]string{},
		networkControllers:     map[string]*networkControllerState{},
		pendingNetworkRefNodes: map[string]sets.Set[string]{},
		networkRefStates:       map[string]map[string]bool{},
	}

	// this controller does not feed from an informer, networks are manually
	// added to the queue for processing
	networkConfig := &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   nc.syncNetwork,
		Threadiness: defaultNetworkReconcilerThreadiness,
		MaxAttempts: controller.InfiniteAttempts,
	}
	nc.networkReconciler = controller.NewReconciler(
		nc.name,
		networkConfig,
	)

	// we don't care about route advertisements in cluster manager
	if nc.hasRouteAdvertisements() {
		nc.nadLister = wf.NADInformer().Lister()
		nc.raLister = wf.RouteAdvertisementsInformer().Lister()
		nc.nodeLister = wf.NodeCoreInformer().Lister()

		// ra controller
		raConfig := &controller.ControllerConfig[ratypes.RouteAdvertisements]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Informer:       wf.RouteAdvertisementsInformer().Informer(),
			Lister:         nc.raLister.List,
			Reconcile:      func(string) error { return nc.syncRunningNetworks() },
			ObjNeedsUpdate: raNeedsUpdate,
			Threadiness:    1,
		}
		nc.raController = controller.NewController(
			nc.name,
			raConfig,
		)

		// node controller
		nodeConfig := &controller.ControllerConfig[corev1.Node]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Informer:       wf.NodeCoreInformer().Informer(),
			Lister:         nc.nodeLister.List,
			Reconcile:      func(string) error { return nc.syncRunningNetworks() },
			ObjNeedsUpdate: nodeNeedsUpdate,
			Threadiness:    1,
		}
		nc.nodeController = controller.NewController(
			nc.name,
			nodeConfig,
		)
	}

	return nc
}

type networkControllerState struct {
	controller         NetworkController
	stoppedAndDeleting bool
	// startFailed retains a stopped controller whose Cleanup method still owns
	// any resources left by Start. Cleanup must succeed before deletion or a
	// replacement Start so two controllers never own the same network state.
	startFailed bool
}

type networkController struct {
	sync.RWMutex

	name string
	node string
	// asyncNetworkStart, when true, skips inline network start during initial
	// sync so reconciler workers start networks with bounded concurrency.
	// Used by ovnkube-controller; ovnkube-node starts networks inline so they
	// are ready before node setup continues.
	asyncNetworkStart bool

	nadLister  nadlisters.NetworkAttachmentDefinitionLister
	raLister   ralisters.RouteAdvertisementsLister
	nodeLister corelisters.NodeLister

	networkReconciler controller.Reconciler
	raController      controller.Controller
	nodeController    controller.Controller

	cm                 ControllerManager
	networks           map[string]util.MutableNetInfo
	networksByID       map[int]string // reverse index: network ID -> network name for O(1) lookup
	networkControllers map[string]*networkControllerState
	// getNADKeysForNetwork returns NAD keys associated with a network name.
	getNADKeysForNetwork func(networkName string) []string

	// networkRefLock is used to protect pendingNetworkRefNodes and networkRefStates access
	networkRefLock sync.Mutex
	// pendingNetworkRefNodes is used to temporarily hold network name -> nodes mapping for future reconciliation
	// Dynamic UDN asks network controller to reconcile and stores nodes that have changed state in this map
	pendingNetworkRefNodes map[string]sets.Set[string]
	// networkRefStates is used to hold network name -> node (active/inactive) state as it is currently configured
	// This is used by Dynamic UDN to avoid calling the network controller's HandleNetworkRefChange for nodes that have
	// not changed state.
	networkRefStates map[string]map[string]bool
	// nodeHasNetwork is a function used to query if a node has a resource (pod or egress IP) that would inform the
	// network controller to either add or remove the remote resources for that network.
	nodeHasNetwork func(node, networkName string) bool
}

// Start will cleanup stale networks that have not been ensured via
// EnsuredNetwork before this call
func (c *networkController) Start() error {
	controllers := []controller.Reconciler{c.networkReconciler}
	if c.raController != nil {
		controllers = append(controllers, c.raController)
	}
	if c.nodeController != nil {
		controllers = append(controllers, c.nodeController)
	}
	return controller.StartWithInitialSync(
		c.syncAll,
		controllers...,
	)
}

func (c *networkController) Stop() {
	controllers := []controller.Reconciler{c.networkReconciler}
	if c.raController != nil {
		controllers = append(controllers, c.raController)
	}
	if c.nodeController != nil {
		controllers = append(controllers, c.nodeController)
	}
	controller.Stop(controllers...)
	for _, networkControllerState := range c.getAllNetworkStates() {
		if networkControllerState.controller.GetNetworkName() == types.DefaultNetworkName {
			// we don't own the lifecycle of the default network, so don't stop
			// it
			continue
		}
		if networkControllerState.stoppedAndDeleting || networkControllerState.startFailed {
			continue
		}
		networkControllerState.controller.Stop()
	}
}

func (c *networkController) EnsureNetwork(network util.MutableNetInfo) {
	c.setNetwork(network.GetNetworkName(), network)
	c.networkReconciler.Reconcile(network.GetNetworkName())
}

func (c *networkController) DeleteNetwork(network string) {
	switch network {
	case types.DefaultNetworkName:
		// for the default network however ensure it runs with the default
		// config
		c.setNetwork(network, &util.DefaultNetInfo{})
	default:
		c.setNetwork(network, nil)
	}
	c.networkReconciler.Reconcile(network)
}

func (c *networkController) setNetwork(network string, netInfo util.MutableNetInfo) {
	c.Lock()
	defer c.Unlock()
	// Remove old ID mapping if network existed
	if old, exists := c.networks[network]; exists {
		if oldID := old.GetNetworkID(); oldID != types.InvalidID {
			delete(c.networksByID, oldID)
		}
	}
	if netInfo == nil {
		delete(c.networks, network)
		return
	}
	c.networks[network] = netInfo
	// Add new ID mapping
	if id := netInfo.GetNetworkID(); id != types.InvalidID {
		c.networksByID[id] = network
	}
}

func (c *networkController) getNetwork(network string) util.MutableNetInfo {
	c.RLock()
	defer c.RUnlock()
	return c.networks[network]
}

func (c *networkController) getAllNetworks() []util.NetInfo {
	c.RLock()
	defer c.RUnlock()
	networks := make([]util.NetInfo, 0, len(c.networks))
	for _, network := range c.networks {
		networks = append(networks, network)
	}
	return networks
}

func (c *networkController) setNetworkState(network string, state *networkControllerState) {
	c.Lock()
	defer c.Unlock()
	if state == nil {
		delete(c.networkControllers, network)
		return
	}
	c.networkControllers[network] = state
}

func (c *networkController) getNetworkState(network string) *networkControllerState {
	c.RLock()
	defer c.RUnlock()
	state := c.networkControllers[network]
	if state == nil {
		return &networkControllerState{}
	}
	return state
}

func (c *networkController) getReconcilableNetworkState(network string) (ReconcilableNetworkController, bool, bool) {
	if network == types.DefaultNetworkName {
		return c.cm.GetDefaultNetworkController(), false, false
	}
	c.RLock()
	defer c.RUnlock()
	state := c.networkControllers[network]
	if state == nil {
		return nil, false, false
	}
	return state.controller, state.stoppedAndDeleting, state.startFailed
}

func (c *networkController) getControllerForNotify(networkName string) (NetworkController, bool) {
	c.RLock()
	defer c.RUnlock()
	state := c.networkControllers[networkName]
	if state == nil || state.controller == nil || state.stoppedAndDeleting || state.startFailed {
		return nil, false
	}
	return state.controller, true
}

// NotifyNetworkRefChange enqueues a network reconcile so node-level state can be recomputed.
func (c *networkController) NotifyNetworkRefChange(networkName, node string) {
	c.queueNetworkRefChange(networkName, node)
	c.networkReconciler.Reconcile(networkName)
}

func (c *networkController) queueNetworkRefChange(networkName, node string) {
	if networkName == "" || node == "" {
		return
	}
	c.networkRefLock.Lock()
	defer c.networkRefLock.Unlock()
	if c.pendingNetworkRefNodes == nil {
		c.pendingNetworkRefNodes = map[string]sets.Set[string]{}
	}
	nodes := c.pendingNetworkRefNodes[networkName]
	if nodes == nil {
		nodes = sets.New[string]()
		c.pendingNetworkRefNodes[networkName] = nodes
	}
	nodes.Insert(node)
}

func (c *networkController) popPendingNetworkRefNodes(networkName string) []string {
	c.networkRefLock.Lock()
	defer c.networkRefLock.Unlock()
	nodes := c.pendingNetworkRefNodes[networkName]
	if len(nodes) == 0 {
		return nil
	}
	delete(c.pendingNetworkRefNodes, networkName)
	return nodes.UnsortedList()
}

func (c *networkController) clearPendingNetworkRefNodes(networkName string) {
	c.networkRefLock.Lock()
	defer c.networkRefLock.Unlock()
	delete(c.pendingNetworkRefNodes, networkName)
}

func (c *networkController) clearNetworkRefState(networkName string) {
	c.networkRefLock.Lock()
	defer c.networkRefLock.Unlock()
	delete(c.networkRefStates, networkName)
}

func (c *networkController) shouldHandleNetworkRefChange(networkName, node string, active bool) bool {
	c.networkRefLock.Lock()
	defer c.networkRefLock.Unlock()
	if c.networkRefStates == nil {
		c.networkRefStates = map[string]map[string]bool{}
	}
	nodeStates := c.networkRefStates[networkName]
	if nodeStates == nil {
		nodeStates = map[string]bool{}
		c.networkRefStates[networkName] = nodeStates
	}
	prev, ok := nodeStates[node]
	if ok && prev == active {
		return false
	}
	nodeStates[node] = active
	return true
}

func (c *networkController) reconcilePendingNetworkRefChanges(networkName string) {
	ctrl, ok := c.getControllerForNotify(networkName)
	if !ok || c.nodeHasNetwork == nil {
		return
	}
	nodes := c.popPendingNetworkRefNodes(networkName)
	if len(nodes) == 0 {
		return
	}
	for _, node := range nodes {
		active := c.nodeHasNetwork(node, networkName)
		if !c.shouldHandleNetworkRefChange(networkName, node, active) {
			continue
		}
		ctrl.HandleNetworkRefChange(node, active)
	}
}

func (c *networkController) getAllNetworkStates() []*networkControllerState {
	c.RLock()
	defer c.RUnlock()
	networkStates := make([]*networkControllerState, 0, len(c.networks))
	for _, state := range c.networkControllers {
		networkStates = append(networkStates, state)
	}
	return networkStates
}

func (c *networkController) syncAll() error {
	// as we sync upon start, consider networks that have not been ensured as
	// stale and clean them up
	validNetworks := c.getAllNetworks()
	if err := c.cm.CleanupStaleNetworks(validNetworks...); err != nil {
		return err
	}

	// For async start, skip inline sync and let reconciler workers handle networks
	// asynchronously.
	if c.asyncNetworkStart {
		klog.Infof("%s: skipping inline sync for async start", c.name)
		return nil
	}

	// sync all known networks. There is no informer for networks. Keys are added by NAD controller.
	// Certain downstream controllers that handle configuration for multiple networks depend on being
	// aware of all the existing networks on initialization. To achieve that, we need to start existing
	// networks synchronously. Otherwise, these controllers might incorrectly assess valid configuration
	// as stale.
	start := time.Now()
	klog.Infof("%s: syncing all networks", c.name)
	for _, network := range validNetworks {
		err := c.syncNetwork(network.GetNetworkName())
		if err != nil {
			return fmt.Errorf("failed to sync network %s: %w", network.GetNetworkName(), err)
		}
	}
	klog.Infof("%s: finished syncing all networks. Time taken: %s", c.name, time.Since(start))
	return nil
}

func (c *networkController) syncRunningNetworks() error {
	c.networkReconciler.Reconcile(types.DefaultNetworkName)
	for _, network := range c.getAllNetworkStates() {
		c.networkReconciler.Reconcile(network.controller.GetNetworkName())
	}

	return nil
}

// syncNetwork must be called with nm mutex locked
func (c *networkController) syncNetwork(network string) error {
	startTime := time.Now()
	klog.V(5).Infof("%s: sync network %s", c.name, network)
	defer func() {
		klog.V(4).Infof("%s: finished syncing network %s, took %v", c.name, network, time.Since(startTime))
	}()

	have, stoppedAndDeleting, startFailed := c.getReconcilableNetworkState(network)
	want := c.getNetwork(network)
	compatible := util.AreNetworksCompatible(have, want)

	// we will dispose of the old network if deletion is in progress or if
	// non-reconcilable configuration changed. A stopped controller whose Start
	// failed remains the cleanup owner and must release that state before a new
	// controller may attempt to program the same network.
	dispose := startFailed || stoppedAndDeleting || !compatible
	if dispose {
		err := c.deleteNetwork(network)
		if err != nil {
			return err
		}
		have = nil
	}

	// fetch other relevant network information
	err := c.gatherNetwork(want, have)
	if err != nil {
		return fmt.Errorf("failed to fetch other network information for network %s: %w", network, err)
	}

	ensureNetwork := !compatible || util.DoesNetworkNeedReconciliation(have, want)
	if ensureNetwork {
		// inform controller manager of upcoming changes so other controllers are
		// aware
		err = c.cm.Reconcile(network, have, want)
		if err != nil {
			return fmt.Errorf("failed to reconcile controller manager for network %s: %w", network, err)
		}

		// ensure the network controller
		err = c.ensureNetwork(want)
		if err != nil {
			return fmt.Errorf("%s: failed to ensure network %s: %w", c.name, network, err)
		}
	}

	c.reconcilePendingNetworkRefChanges(network)

	return nil
}

func (c *networkController) ensureNetwork(network util.MutableNetInfo) error {
	if network == nil {
		return nil
	}

	networkName := network.GetNetworkName()
	reconcilable, _, startFailed := c.getReconcilableNetworkState(networkName)

	// this might just be an update of reconcilable network configuration
	if reconcilable != nil && !startFailed {
		err := reconcilable.Reconcile(network)
		if err != nil {
			return fmt.Errorf("failed to reconcile controller for network %s: %w", networkName, err)
		}
		return nil
	}

	// otherwise setup & start the new network controller
	nc, err := c.cm.NewNetworkController(network)
	if err != nil {
		if errors.Is(err, ErrNetworkControllerTopologyNotManaged) {
			klog.V(5).Infof("Network %q of topology %s is not managed by %s, skipping it",
				networkName, network.TopologyType(), c.name)
			return nil
		}
		return fmt.Errorf("failed to create network %s: %w", networkName, err)
	}
	err = nc.Start(context.Background())
	if err != nil {
		nc.Stop()
		c.setNetworkState(networkName, &networkControllerState{
			controller:  nc,
			startFailed: true,
		})
		return fmt.Errorf("failed to start network %s: %w", networkName, err)
	}
	c.setNetworkState(network.GetNetworkName(), &networkControllerState{controller: nc})

	return nil
}

func (c *networkController) deleteNetwork(network string) error {
	c.Lock()
	have := c.networkControllers[network]
	if have == nil || have.controller == nil {
		c.Unlock()
		c.clearPendingNetworkRefNodes(network)
		c.clearNetworkRefState(network)
		return nil
	}
	ctrl := have.controller
	alreadyStopping := have.stoppedAndDeleting
	startFailed := have.startFailed
	have.stoppedAndDeleting = true
	c.Unlock()

	if !alreadyStopping && !startFailed {
		ctrl.Stop()
	}

	err := ctrl.Cleanup()
	if err != nil {
		return fmt.Errorf("%s: failed to cleanup network %s: %w", c.name, network, err)
	}

	c.setNetworkState(network, nil)
	c.clearPendingNetworkRefNodes(network)
	c.clearNetworkRefState(network)
	return nil
}

func (c *networkController) gatherNetwork(network util.MutableNetInfo, current util.NetInfo) error {
	if network == nil {
		return nil
	}
	return c.setAdvertisements(network, current)
}

func (c *networkController) setAdvertisements(network util.MutableNetInfo, current util.NetInfo) error {
	if !network.IsDefault() && !network.IsPrimaryNetwork() {
		return nil
	}
	if !c.hasRouteAdvertisements() {
		return nil
	}
	if c.getNADKeysForNetwork == nil {
		return fmt.Errorf("missing NAD resolver for network %q", network.GetNetworkName())
	}

	raNames := sets.New[string]()
	for _, nadNamespacedName := range c.getNADKeysForNetwork(network.GetNetworkName()) {
		namespace, name, err := cache.SplitMetaNamespaceKey(nadNamespacedName)
		if err != nil {
			return err
		}

		nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
		if err != nil {
			return err
		}

		var nadRANames []string
		if nad.Annotations[types.OvnRouteAdvertisementsKey] != "" {
			err = json.Unmarshal([]byte(nad.Annotations[types.OvnRouteAdvertisementsKey]), &nadRANames)
			if err != nil {
				return err
			}
		}

		raNames.Insert(nadRANames...)
	}

	podAdvertisements := map[string][]string{}
	eipAdvertisements := map[string][]string{}
	for raName := range raNames {
		ra, err := c.raLister.Get(raName)
		if apierrors.IsNotFound(err) {
			// A stale NAD annotation should not prevent a network controller
			// from starting. Preserve the current advertised state until the
			// RouteAdvertisements controller removes the stale annotation.
			preserveAdvertisements(network, current)
			return nil
		}
		if err != nil {
			return err
		}

		advertisements := sets.New(ra.Spec.Advertisements...)
		if !advertisements.Has(ratypes.PodNetwork) {
			continue
		}

		accepted := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
		if accepted == nil {
			// if there is no status we can safely ignore
			continue
		}
		if accepted.Status != metav1.ConditionTrue || accepted.ObservedGeneration != ra.Generation {
			// The RA is not accepted yet (e.g. a brand-new network whose
			// subnets haven't been allocated). Don't abort here: starting the
			// controller is what lets the node allocator assign subnets, which
			// is in turn what lets the RA become accepted - aborting would
			// deadlock the two. Keep the previously-advertised VRFs (empty for
			// a new network) for now; the RA's later transition to Accepted
			// re-triggers this sync and fills in the real VRFs.
			preserveAdvertisements(network, current)
			return nil
		}

		nodeSelector, err := metav1.LabelSelectorAsSelector(&ra.Spec.NodeSelector)
		if err != nil {
			return err
		}

		nodes, err := c.nodeLister.List(nodeSelector)
		if err != nil {
			return err
		}

		vrf := ra.Spec.TargetVRF
		if vrf == "" {
			vrf = types.DefaultNetworkName
		}

		for _, node := range nodes {
			if !c.isNodeManaged(node) {
				continue
			}
			if advertisements.Has(ratypes.PodNetwork) {
				podAdvertisements[node.Name] = append(podAdvertisements[node.Name], vrf)
			}
			if advertisements.Has(ratypes.EgressIP) {
				eipAdvertisements[node.Name] = append(eipAdvertisements[node.Name], vrf)
			}
		}
	}
	network.SetPodNetworkAdvertisedVRFs(podAdvertisements)
	network.SetEgressIPAdvertisedVRFs(eipAdvertisements)
	return nil
}

// preserveAdvertisements carries the currently-advertised VRFs over to the
// network being synced, so a missing or not-yet-accepted RouteAdvertisements
// neither blocks the controller from starting nor clears existing advertisement
// state. For a brand-new network, current is nil and the advertised VRFs are
// left empty.
func preserveAdvertisements(network util.MutableNetInfo, current util.NetInfo) {
	if current == nil {
		return
	}
	network.SetPodNetworkAdvertisedVRFs(current.GetPodNetworkAdvertisedVRFs())
	network.SetEgressIPAdvertisedVRFs(current.GetEgressIPAdvertisedVRFs())
}

func (c *networkController) hasRouteAdvertisements() bool {
	return util.IsRouteAdvertisementsEnabled()
}

func (c *networkController) isNodeManaged(node *corev1.Node) bool {
	if c.node == "" {
		// cluster manager manages all nodes
		return true
	}
	// ovnkube-node only manages its own node.
	return node.Name == c.node
}

func raNeedsUpdate(oldRA, newRA *ratypes.RouteAdvertisements) bool {
	if oldRA == nil || newRA == nil {
		// handle RA add/delete through the NAD annotation update
		return false
	}

	// don't process resync or objects that are marked for deletion
	if oldRA.ResourceVersion == newRA.ResourceVersion ||
		!newRA.GetDeletionTimestamp().IsZero() {
		return false
	}

	// the RA needs to be accepted on its current generation, otherwise we
	// commit to no change
	newCondition := meta.FindStatusCondition(newRA.Status.Conditions, "Accepted")
	if newCondition == nil {
		return false
	}
	if newCondition.Status != metav1.ConditionTrue {
		return false
	}
	if newCondition.ObservedGeneration != newRA.Generation {
		return false
	}
	// there had to be a change on observed generation or status, otherwise we
	// already processed this RA in its current form
	oldCondition := meta.FindStatusCondition(oldRA.Status.Conditions, "Accepted")
	if oldCondition == nil {
		return true
	}
	return oldCondition.ObservedGeneration != newCondition.ObservedGeneration || oldCondition.Status != newCondition.Status
}

func nodeNeedsUpdate(oldNode, newNode *corev1.Node) bool {
	if oldNode == nil || newNode == nil {
		return true
	}

	// don't process resync or objects that are marked for deletion
	if oldNode.ResourceVersion == newNode.ResourceVersion ||
		!newNode.GetDeletionTimestamp().IsZero() {
		return false
	}

	return !reflect.DeepEqual(oldNode.Labels, newNode.Labels)
}

func (c *networkController) getRunningNetwork(id int) string {
	if id == types.DefaultNetworkID {
		return types.DefaultNetworkName
	}
	for _, state := range c.getAllNetworkStates() {
		if state.controller.GetNetworkID() == id {
			return state.controller.GetNetworkName()
		}
	}
	return ""
}

// GetNetworkByID returns the network with the given ID or nil if not found.
// This is an O(1) lookup using an internal index.
func (c *networkController) GetNetworkByID(id int) util.NetInfo {
	if id == types.DefaultNetworkID {
		return &util.DefaultNetInfo{}
	}
	c.RLock()
	defer c.RUnlock()
	if name, ok := c.networksByID[id]; ok {
		return c.networks[name]
	}
	return nil
}

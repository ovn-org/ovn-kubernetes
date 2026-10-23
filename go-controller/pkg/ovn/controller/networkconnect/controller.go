// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkconnect

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/listers/clusternetworkconnect/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	controllerName         = "ovnkube-network-connect-controller"
	networkRefKeySeparator = "/"
)

// Controller manages network connectivity between (C)UDNs based on ClusterNetworkConnect CRs.
// It runs in each ovnkube-controller and creates OVN topology:
// - 1 connect-router for each CNC
// - ports connecting each selected (C)UDN network routers to connect-router
// - logical router policies on each selected (C)UDN's network router steering traffic to the connect-router
// - logical router static routes on the connect-router routing traffic to the corresponding selected (C)UDNs
// - load balancer attachments for ServiceNetwork connectivity enabled CNCs
type Controller struct {
	// zone is the name of the zone that this controller manages
	zone string

	// nbClient is the libovsdb northbound client interface
	nbClient libovsdbclient.Client

	// wf is the watch factory for accessing informers
	wf *factory.WatchFactory

	// listers
	cncLister     networkconnectlisters.ClusterNetworkConnectLister
	nodeLister    corev1listers.NodeLister
	nadLister     nadlisters.NetworkAttachmentDefinitionLister
	serviceLister corev1listers.ServiceLister

	// networkManager provides access to network information
	networkManager networkmanager.Interface

	// addressSetFactory creates and manages OVN address sets
	addressSetFactory addressset.AddressSetFactory

	// cncController handles ClusterNetworkConnect events
	cncController controllerutil.Controller

	// nodeController handles Node events (for updating routes when nodes change)
	nodeController controllerutil.Controller

	// nadReconciler handles NAD-triggered CNC requeues
	nadReconciler   networkmanager.NADReconciler
	nadReconcilerID uint64
	// serviceController handles Service events (for ServiceNetwork connectivity)
	serviceController controllerutil.Controller
	// networkRefQueue handles node+network activity-triggered CNC requeues
	networkRefQueue controllerutil.Reconciler
	// networkRefReconciler receives node+network activity notifications from networkmanager
	networkRefReconciler   networkmanager.NetworkRefReconciler
	networkRefReconcilerID uint64

	// Single global lock protecting all controller state
	sync.RWMutex

	// cncCache holds the state for each CNC keyed by CNC name
	cncCache map[string]*networkConnectState

	// cncNetworkIDs tracks desired owner network IDs from each CNC's subnet annotation.
	cncNetworkIDs map[string]sets.Set[int]
	// cncsByNetworkID indexes CNCs by desired owner network ID for targeted requeues.
	cncsByNetworkID map[int]sets.Set[string]

	// localZoneNode is the node in this controller's zone.
	// We only support 1 node per zone for this feature.
	// Updated during each CNC reconciliation via computeNodeInfo().
	localZoneNode *corev1.Node
}

// networkConnectState tracks the state of a single ClusterNetworkConnect
type networkConnectState struct {
	// name of the ClusterNetworkConnect
	name string
	// tunnelID for the connect router
	tunnelID int
	// connectedNetworks is the set of owner keys (e.g., "layer3_1", "layer2_2") for networks
	// connected by this CNC. Used to track OVN resources created and detect NAD matching changes.
	connectedNetworks sets.Set[string]
	// serviceNetworkEnabled tracks whether ServiceNetwork connectivity is enabled
	// Used for cleanup when the connectivity type is removed from the CNC spec.
	serviceNetworkConnectEnabled bool
	// podNetworkConnectEnabled tracks whether PodNetwork connectivity is enabled
	// Used to determine if partial connectivity (service without pod) was active.
	podNetworkConnectEnabled bool
}

type networkRefReconcilerFunc func(node, networkName string)

// Reconcile adapts a plain function to the NetworkRefReconciler interface.
func (f networkRefReconcilerFunc) Reconcile(node, networkName string) {
	f(node, networkName)
}

// NewController creates a new network connect controller for ovnkube-controller.
func NewController(
	zone string,
	nbClient libovsdbclient.Client,
	wf *factory.WatchFactory,
	networkManager networkmanager.Interface,
) *Controller {
	cncLister := wf.ClusterNetworkConnectInformer().Lister()
	nodeLister := wf.NodeCoreInformer().Lister()
	nadLister := wf.NADInformer().Lister()
	serviceLister := wf.ServiceCoreInformer().Lister()

	c := &Controller{
		zone:              zone,
		nbClient:          nbClient,
		wf:                wf,
		cncLister:         cncLister,
		nodeLister:        nodeLister,
		nadLister:         nadLister,
		serviceLister:     serviceLister,
		networkManager:    networkManager,
		addressSetFactory: addressset.NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode),
		cncCache:          make(map[string]*networkConnectState),
		cncNetworkIDs:     make(map[string]sets.Set[int]),
		cncsByNetworkID:   make(map[int]sets.Set[string]),
	}

	cncCfg := &controllerutil.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:    wf.ClusterNetworkConnectInformer().Informer(),
		Lister:      cncLister.List,
		Reconcile: func(_ context.Context, key string) error {
			return c.reconcileCNC(key)
		},
		ObjNeedsUpdate: cncNeedsUpdate,
		Threadiness:    1,
	}
	c.cncController = controllerutil.NewController(
		"ovnkube-network-connect-controller",
		cncCfg,
	)

	nodeCfg := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:    wf.NodeCoreInformer().Informer(),
		Lister:      nodeLister.List,
		Reconcile: func(_ context.Context, key string) error {
			return c.reconcileNode(key)
		},
		ObjNeedsUpdate: nodeNeedsUpdate,
		Threadiness:    1,
	}
	c.nodeController = controllerutil.NewController(
		"ovnkube-network-connect-node-controller",
		nodeCfg,
	)

	// this controller does not feed from an informer, nads are added
	// to the queue by NAD Controller
	nadReconcilerConfig := &controllerutil.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile: func(_ context.Context, key string) error {
			return c.syncNAD(key)
		},
		Threadiness: 1,
		MaxAttempts: controllerutil.InfiniteAttempts,
	}
	c.nadReconciler = controllerutil.NewReconciler(
		"ovnkube-network-connect-nad",
		nadReconcilerConfig,
	)
	networkRefReconcilerConfig := &controllerutil.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile: func(_ context.Context, key string) error {
			return c.syncNetworkRef(key)
		},
		Threadiness: 1,
		MaxAttempts: controllerutil.InfiniteAttempts,
	}
	c.networkRefQueue = controllerutil.NewReconciler(
		"ovnkube-network-connect-network-ref",
		networkRefReconcilerConfig,
	)
	networkRefQueue := c.networkRefQueue
	c.networkRefReconciler = networkRefReconcilerFunc(func(node, networkName string) {
		if node == "" || networkName == "" || networkRefQueue == nil {
			return
		}
		// Network manager notifies with semantic node+network values. Adapt
		// them to the CNC controller's string-keyed workqueue locally so the
		// queue key format stays private to this controller.
		networkRefQueue.Reconcile(networkRefKey(node, networkName))
	})

	serviceCfg := &controllerutil.ControllerConfig[corev1.Service]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:    wf.ServiceCoreInformer().Informer(),
		Lister:      serviceLister.List,
		Reconcile: func(_ context.Context, key string) error {
			return c.reconcileService(key)
		},
		ObjNeedsUpdate: serviceNeedsUpdate,
		Threadiness:    1,
	}
	c.serviceController = controllerutil.NewController(
		"ovnkube-network-connect-service-controller",
		serviceCfg,
	)

	return c
}

// Start starts the controller. Uses StartWithInitialSync so that repairStaleCNCs
// runs once at startup (after informers are synced, before workers) to clean up
// OVN state for CNCs that no longer exist in the API.
func (c *Controller) Start() (err error) {
	klog.Infof("Starting ovnkube network connect controller for zone %s", c.zone)
	initialSync := func() error { return c.repairStaleCNCs() }
	controllers := []controllerutil.Reconciler{
		c.cncController,
		c.nodeController,
		c.serviceController,
	}
	if c.nadReconciler != nil {
		c.nadReconcilerID = c.networkManager.RegisterNADReconciler(c.nadReconciler)
		controllers = append(controllers, c.nadReconciler)
	}
	if c.networkRefReconciler != nil {
		c.networkRefReconcilerID = c.networkManager.RegisterNetworkRefReconciler(c.networkRefReconciler)
		if c.networkRefQueue != nil {
			controllers = append(controllers, c.networkRefQueue)
		}
	}
	defer func() {
		if err != nil {
			if c.nadReconcilerID != 0 {
				c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID)
				c.nadReconcilerID = 0
			}
			if c.networkRefReconcilerID != 0 {
				c.networkManager.DeRegisterNetworkRefReconciler(c.networkRefReconcilerID)
				c.networkRefReconcilerID = 0
			}
		}
	}()
	return controllerutil.StartWithInitialSync(initialSync, controllers...)
}

// Stop stops the controller.
func (c *Controller) Stop() {
	if c.nadReconcilerID != 0 {
		c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID)
	}
	if c.networkRefReconcilerID != 0 {
		c.networkManager.DeRegisterNetworkRefReconciler(c.networkRefReconcilerID)
	}
	controllers := []controllerutil.Reconciler{
		c.cncController,
		c.nodeController,
		c.serviceController,
	}
	if c.nadReconciler != nil {
		controllers = append(controllers, c.nadReconciler)
	}
	if c.networkRefQueue != nil {
		controllers = append(controllers, c.networkRefQueue)
	}
	controllerutil.Stop(controllers...)
	c.nadReconciler = nil
	c.nadReconcilerID = 0
	c.networkRefQueue = nil
	c.networkRefReconciler = nil
	c.networkRefReconcilerID = 0
	klog.Infof("Stopped ovnkube network connect controller for zone %s", c.zone)
}

// networkRefKey builds the queue key used for deferred node+network activity
// processing. The network name is used instead of ID because activity can be
// observed before NAD reconciliation has resolved the ID locally.
func networkRefKey(node, networkName string) string {
	return node + networkRefKeySeparator + networkName
}

// parseNetworkRefKey decodes a queue key back into node and network name.
func parseNetworkRefKey(key string) (string, string, error) {
	idx := strings.LastIndex(key, networkRefKeySeparator)
	if idx <= 0 || idx == len(key)-1 {
		return "", "", fmt.Errorf("invalid network-ref key %q", key)
	}
	node := key[:idx]
	return node, key[idx+1:], nil
}

func (c *Controller) syncNetworkRef(key string) error {
	if c.networkManager == nil {
		return nil
	}
	_, networkName, err := parseNetworkRefKey(key)
	if err != nil {
		klog.Warningf("Skipping invalid network-ref key %q: %v", key, err)
		return nil
	}

	c.reconcileCNCsForNetworkIDs(c.networkIDsForNetworkName(networkName)...)
	return nil
}

func (c *Controller) syncNAD(key string) error {
	if c.networkManager == nil {
		return nil
	}
	nadNetwork := c.networkManager.GetNetInfoForNADKey(key)
	if nadNetwork != nil {
		// Common path: the NAD's network is rendered, so we can requeue
		// CNCs directly by its network ID without scanning the CNC index.
		networkID := nadNetwork.GetNetworkID()
		if networkID == types.InvalidID {
			return nil
		}
		c.reconcileCNCsForNetworkIDs(networkID)
		return nil
	}

	// Dynamic UDN can know a NAD-to-network mapping while the network is
	// filtered and therefore absent from networkController. Fall back to the
	// network name and resolve matching indexed CNC network IDs.
	networkName := c.networkManager.GetNetworkNameForNADKey(key)
	c.reconcileCNCsForNetworkIDs(c.networkIDsForNetworkName(networkName)...)
	return nil
}

func networkIDsFromAllocatedSubnets(allocatedSubnets map[string][]*net.IPNet) sets.Set[int] {
	networkIDs := sets.New[int]()
	for owner := range allocatedSubnets {
		_, ownerNetworkID, err := util.ParseNetworkOwner(owner)
		if err != nil || ownerNetworkID == types.InvalidID {
			continue
		}
		networkIDs.Insert(ownerNetworkID)
	}
	return networkIDs
}

func (c *Controller) networkIDsForNetworkName(networkName string) []int {
	if c.networkManager == nil || networkName == "" {
		return nil
	}

	c.RLock()
	indexedNetworkIDs := make([]int, 0, len(c.cncsByNetworkID))
	for networkID := range c.cncsByNetworkID {
		indexedNetworkIDs = append(indexedNetworkIDs, networkID)
	}
	c.RUnlock()

	matchingNetworkIDs := sets.New[int]()
	for _, networkID := range indexedNetworkIDs {
		netInfo := c.networkManager.GetNetworkByID(networkID)
		if netInfo != nil && netInfo.GetNetworkName() == networkName {
			matchingNetworkIDs.Insert(networkID)
		}
	}
	return matchingNetworkIDs.UnsortedList()
}

// updateCNCNetworkIDsLocked refreshes the desired network ID reverse index for a CNC.
// Caller must hold c.Lock().
func (c *Controller) updateCNCNetworkIDsLocked(cncName string, networkIDs sets.Set[int]) {
	if c.cncNetworkIDs == nil {
		c.cncNetworkIDs = map[string]sets.Set[int]{}
	}
	if c.cncsByNetworkID == nil {
		c.cncsByNetworkID = map[int]sets.Set[string]{}
	}

	for networkID := range c.cncNetworkIDs[cncName] {
		indexedCNCs := c.cncsByNetworkID[networkID]
		indexedCNCs.Delete(cncName)
		if len(indexedCNCs) == 0 {
			delete(c.cncsByNetworkID, networkID)
		}
	}

	if networkIDs == nil {
		delete(c.cncNetworkIDs, cncName)
		return
	}

	c.cncNetworkIDs[cncName] = networkIDs
	for networkID := range networkIDs {
		indexedCNCs := c.cncsByNetworkID[networkID]
		if indexedCNCs == nil {
			indexedCNCs = sets.New[string]()
			c.cncsByNetworkID[networkID] = indexedCNCs
		}
		indexedCNCs.Insert(cncName)
	}
}

func (c *Controller) reconcileCNCsForNetworkIDs(networkIDs ...int) {
	if c.cncController == nil || len(networkIDs) == 0 {
		return
	}

	cncsToReconcile := sets.New[string]()
	c.RLock()
	for _, networkID := range networkIDs {
		for cncName := range c.cncsByNetworkID[networkID] {
			cncsToReconcile.Insert(cncName)
		}
	}
	c.RUnlock()

	for cncName := range cncsToReconcile {
		c.cncController.Reconcile(cncName)
	}
}

// cncNeedsUpdate determines if a CNC update requires reconciliation.
func cncNeedsUpdate(oldObj, newObj *networkconnectv1.ClusterNetworkConnect) bool {
	// Always process create/delete
	if oldObj == nil || newObj == nil {
		return true
	}

	// Process if annotations changed (subnet or tunnel key updates from cluster manager)
	// this event is triggered when the cluster manager updates the annotations on the CNC object
	// based on CNC or NAD or namespace changes.
	// Since we watch for the annotation updates on CNC objects, we don't need to directly
	// watch for NAD or namespace changes or even CNC network selectors changes as part of this controller.
	if util.NetworkConnectSubnetAnnotationChanged(oldObj, newObj) || util.NetworkConnectTunnelKeyAnnotationsChanged(oldObj, newObj) {
		return true
	}

	// Process if connectivity changed
	if !reflect.DeepEqual(oldObj.Spec.Connectivity, newObj.Spec.Connectivity) {
		return true
	}

	return false
}

// nodeNeedsUpdate determines if a node update requires reconciliation.
func nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	// Always process create/delete
	if oldObj == nil || newObj == nil {
		return true
	}

	// Process if node subnet annotation changed (affects static routes)
	if util.NodeSubnetAnnotationChanged(oldObj, newObj) {
		return true
	}

	// Process if node ID annotation changed (the only supported scenario
	// is when the node is added and get's an annotation update once the nodeID is allocated)
	return util.NodeIDAnnotationChanged(oldObj, newObj) && oldObj.Annotations[util.OvnNodeID] == ""
}

// serviceNeedsUpdate determines if a service change requires reconciliation.
// We care about service CREATE and protocol-set changes for ServiceNetwork.
// Service deletes don't need processing because:
//   - LoadBalancerGroup.load_balancer is a weak reference with min=0 and refType=weak
//   - When the LB is deleted by services controller, OVSDB automatically
//     garbage-collects the UUID from the LBG
//
// Service updates that only change endpoints, port numbers, or labels are handled
// by the services controller. We only need to react when the set of unique protocols
// changes (e.g., adding UDP alongside TCP), because the services controller creates
// one _cluster LB per protocol and we need to add the new LB to the CNC's LBG.
// Note: spec.clusterIP is immutable after creation, so it cannot change via updates.
func serviceNeedsUpdate(oldObj, newObj *corev1.Service) bool {
	// Only process create (oldObj == nil)
	// Delete events are handled automatically by OVN weak reference cleanup
	if oldObj == nil && newObj != nil {
		klog.V(5).Infof("serviceNeedsUpdate: CREATE event for %s/%s, returning true", newObj.Namespace, newObj.Name)
		return true
	}
	// Process updates where the set of unique protocols changed
	if oldObj != nil && newObj != nil {
		needsUpdate := !serviceProtocolsEqual(oldObj, newObj)
		klog.V(5).Infof("serviceNeedsUpdate: UPDATE event for %s/%s, protocolsChanged=%v", newObj.Namespace, newObj.Name, needsUpdate)
		return needsUpdate
	}
	return false
}

// serviceProtocolsEqual returns true if both services have the same set of unique protocols.
func serviceProtocolsEqual(a, b *corev1.Service) bool {
	aProto := sets.New[corev1.Protocol]()
	for _, p := range a.Spec.Ports {
		aProto.Insert(p.Protocol)
	}
	bProto := sets.New[corev1.Protocol]()
	for _, p := range b.Spec.Ports {
		bProto.Insert(p.Protocol)
	}
	return aProto.Equal(bProto)
}

// reconcileCNC reconciles a ClusterNetworkConnect object.
func (c *Controller) reconcileCNC(key string) error {
	c.Lock()
	defer c.Unlock()

	startTime := time.Now()
	_, cncName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	klog.V(5).Infof("Reconciling CNC %s", cncName)
	defer func() {
		klog.V(4).Infof("Reconciling CNC %s took %v", cncName, time.Since(startTime))
	}()

	cnc, err := c.cncLister.Get(cncName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// CNC was deleted, clean up OVN resources
			return c.cleanupCNC(cncName)
		}
		return err
	}

	return c.syncCNC(cnc)
}

// reconcileNode reconciles node changes that might affect network connectivity.
func (c *Controller) reconcileNode(key string) error {
	startTime := time.Now()
	klog.V(5).Infof("Reconciling node %s for network connect", key)
	defer func() {
		klog.V(4).Infof("Reconciling node %s for network connect took %v", key, time.Since(startTime))
	}()

	_, err := c.nodeLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Node was deleted, requeue all CNCs to update routes
			return c.requeueAllCNCs()
		}
		return err
	}

	// Requeue all CNCs to update routes for this node.
	// We process ALL nodes (not just our zone) because the connect-router
	// needs static routes to all node subnets across all zones.
	// TODO (trozet): we should check what changed in the node and only reconcile affected CNCs
	return c.requeueAllCNCs()
}

// reconcileService reconciles service changes for ServiceNetwork connectivity.
// When a service is created/deleted, we check if its network is connected by any CNC
// with ServiceNetwork enabled and requeue those CNCs.
// This follows the same pattern as reconcileNAD in cluster manager.
func (c *Controller) reconcileService(key string) error {
	c.RLock()
	defer c.RUnlock()

	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	klog.V(5).Infof("Reconciling service %s/%s for network connect", namespace, name)
	defer func() {
		klog.V(4).Infof("Reconciling service %s/%s for network connect took %v", namespace, name, time.Since(startTime))
	}()

	// Get the service
	svc, err := c.serviceLister.Services(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// Service may be nil if deleted (this shouldn't happen since we don't process delete events)
	if svc == nil {
		klog.V(5).Infof("Service %s/%s deleted, skipping network connect reconciliation", namespace, name)
		return nil
	}

	// Get the network owner key for this service's namespace.
	// This determines which network the service belongs to.
	networkOwnerKey, err := c.getNetworkOwnerKeyForNamespace(namespace)
	if err != nil {
		return fmt.Errorf("could not get network owner key for namespace %s: %w", namespace, err)
	}
	if networkOwnerKey == "" {
		// Namespace uses default network, which we don't handle for network-connect
		klog.V(5).Infof("Namespace %s uses default network, skipping service %s", namespace, name)
		return nil
	}

	// List all CNCs and check which ones need to be reconciled for this service
	existingCNCs, err := c.cncLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CNCs: %w", err)
	}

	// Process each CNC to check if this service's network matching state changed
	for _, cnc := range existingCNCs {
		if c.mustProcessCNCForService(svc, cnc, networkOwnerKey) {
			c.cncController.Reconcile(cnc.Name)
		}
	}

	return nil
}

// getNetworkOwnerKeyForNamespace returns the owner key (e.g., "layer3_5") for the primary network
// of the given namespace. Returns empty string if namespace uses default network.
func (c *Controller) getNetworkOwnerKeyForNamespace(namespace string) (string, error) {
	// Get the primary network for this namespace
	primaryNetwork, err := c.networkManager.GetActiveNetworkForNamespace(namespace)
	if err != nil && !util.IsInvalidPrimaryNetworkError(err) {
		return "", fmt.Errorf("failed to get primary network for namespace %s: %w", namespace, err)
	}

	// Check if it's the default network OR
	// No active network: namespace gone, NAD not yet processed, or namespace requires
	// a UDN that doesn't exist yet. Skip for now; CNC will be requeued when ready.
	if primaryNetwork == nil || primaryNetwork.IsDefault() {
		return "", nil
	}

	// Build owner key: "layer3_{networkID}" or "layer2_{networkID}"
	return util.ComputeNetworkOwner(primaryNetwork.TopologyType(), primaryNetwork.GetNetworkID()), nil
}

// mustProcessCNCForService checks if:
// 1. the service's network is currently in the CNC's connectedNetworks AND
// 2. the CNC has ServiceNetwork enabled
// Returns true if both conditions are met, meaning we need to attach this service's LB
// to the other connected networks' switches.
// This function is READ-ONLY and does not update the cache.
// NOTE: Caller must hold at least RLock.
func (c *Controller) mustProcessCNCForService(svc *corev1.Service, cnc *networkconnectv1.ClusterNetworkConnect, networkOwnerKey string) bool {
	// Check if CNC has ServiceNetwork enabled - if not, skip entirely
	if !serviceConnectivityEnabled(cnc) {
		return false
	}

	cncState, cncExists := c.cncCache[cnc.Name]

	// If CNC state doesn't exist yet, we don't know the connected networks
	// Cache will be populated during CNC reconciliation and that will trigger
	// service reconciliation as well from reconcileCNC.
	if !cncExists {
		klog.V(5).Infof("CNC %s state not found in cache, skipping service %s/%s", cnc.Name, svc.Namespace, svc.Name)
		return false
	}

	// Check if service's network is connected by this CNC
	isConnected := cncState.connectedNetworks.Has(networkOwnerKey)

	if isConnected {
		klog.V(5).Infof("Service %s/%s network %s is connected by CNC %s with ServiceNetwork",
			svc.Namespace, svc.Name, networkOwnerKey, cnc.Name)
	}

	return isConnected
}

// serviceConnectivityEnabled checks if the CNC has ServiceNetwork in its connectivity spec.
func serviceConnectivityEnabled(cnc *networkconnectv1.ClusterNetworkConnect) bool {
	for _, ct := range cnc.Spec.Connectivity {
		if ct == networkconnectv1.ServiceNetwork {
			return true
		}
	}
	return false
}

// podConnectivityEnabled checks if the CNC has PodNetwork in its connectivity spec.
func podConnectivityEnabled(cnc *networkconnectv1.ClusterNetworkConnect) bool {
	for _, ct := range cnc.Spec.Connectivity {
		if ct == networkconnectv1.PodNetwork {
			return true
		}
	}
	return false
}

// requeueAllCNCs requeues all CNCs for reconciliation.
func (c *Controller) requeueAllCNCs() error {
	cncs, err := c.cncLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CNCs: %v", err)
	}

	for _, cnc := range cncs {
		c.cncController.Reconcile(cnc.Name)
	}
	return nil
}

// syncCNC synchronizes the OVN topology for a CNC.
func (c *Controller) syncCNC(cnc *networkconnectv1.ClusterNetworkConnect) error {
	// Get or create CNC state
	cncState, exists := c.cncCache[cnc.Name]
	if !exists {
		// this means its CNC create event
		cncState = &networkConnectState{
			name:              cnc.Name,
			connectedNetworks: sets.New[string](),
		}
		c.cncCache[cnc.Name] = cncState
	}
	allocatedSubnets, err := util.ParseNetworkConnectSubnetAnnotation(cnc)
	if err != nil {
		c.updateCNCNetworkIDsLocked(cnc.Name, nil)
		return fmt.Errorf("failed to parse subnet annotation for CNC %s: %w", cnc.Name, err)
	}
	c.updateCNCNetworkIDsLocked(cnc.Name, networkIDsFromAllocatedSubnets(allocatedSubnets))

	// STEP1: Create the connect router for the CNC using tunnel ID from CNC annotation
	// This is always a one time operation - Every CNC has exactly one connect router.
	// tunnelID is set by cluster manager during CNC creation and is considered immutable
	if cncState.tunnelID == 0 {
		// Parse tunnel key from annotation (set by cluster manager)
		tunnelID, err := util.ParseNetworkConnectTunnelKeyAnnotation(cnc)
		if err != nil {
			return fmt.Errorf("failed to parse tunnel key annotation for CNC %s: %v", cnc.Name, err)
		}
		if tunnelID == 0 {
			klog.V(4).Infof("CNC %s does not have tunnel key annotation yet, waiting for cluster manager", cnc.Name)
			// we don't return error here because we want to wait for the cluster manager to set the annotation
			// and cncUpdate event will trigger the reconciliation again.
			return nil
		}
		// Create the connect router
		if err := c.ensureConnectRouter(cnc, tunnelID); err != nil {
			return fmt.Errorf("failed to ensure connect router for CNC %s: %v", cnc.Name, err)
		}
		cncState.tunnelID = tunnelID
	}

	if err := c.syncNetworkConnections(cnc, allocatedSubnets); err != nil {
		return fmt.Errorf("failed to sync network connections for CNC %s: %v", cnc.Name, err)
	}
	return nil
}

// cleanupCNC removes OVN resources for a deleted CNC.
func (c *Controller) cleanupCNC(cncName string) error {
	klog.V(4).Infof("Cleaning up CNC %s", cncName)
	c.updateCNCNetworkIDsLocked(cncName, nil)

	cncState, exists := c.cncCache[cncName]
	if !exists {
		klog.V(4).Infof("CNC %s not found in cache, nothing to clean up", cncName)
		return nil
	}

	// Cleanup network connections (includes service connectivity, partial connectivity ACLs, ports, and policies)
	if err := c.cleanupNetworkConnections(cncName, cncState.serviceNetworkConnectEnabled, cncState.podNetworkConnectEnabled); err != nil {
		return fmt.Errorf("failed to cleanup network connections for CNC %s: %v", cncName, err)
	}

	// Remove the connect router
	if err := c.deleteConnectRouter(cncName); err != nil {
		return fmt.Errorf("failed to delete connect router for CNC %s: %v", cncName, err)
	}

	// Remove from cache
	delete(c.cncCache, cncName)
	klog.V(4).Infof("Cleaned up CNC %s", cncName)

	return nil
}

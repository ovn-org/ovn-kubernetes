// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkmanager

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectlister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/listers/clusternetworkconnect/v1"
	userdefinednetworklister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// nadController handles namespaced scoped NAD events and
// manages cluster scoped networks defined in those NADs. NADs are mostly
// referenced from pods to give them access to the network. Different NADs can
// define the same network as long as those definitions are actually equal.
// Unexpected situations are handled on best effort basis but improper NAD
// administration can lead to undefined behavior if referenced by running pods.
type nadController struct {
	sync.RWMutex
	// reconcilers keyed by registration ID.
	reconcilers      map[uint64]reconcilerRegistration
	nextReconcilerID uint64
	// networkRefReconcilers keyed by registration ID.
	networkRefReconcilers      map[uint64]networkRefReconcilerRegistration
	nextNetworkRefReconcilerID uint64

	name            string
	stopChan        chan struct{}
	stopOnce        sync.Once
	nadLister       nadlisters.NetworkAttachmentDefinitionLister
	udnLister       userdefinednetworklister.UserDefinedNetworkLister
	cudnLister      userdefinednetworklister.ClusterUserDefinedNetworkLister
	namespaceLister corelisters.NamespaceLister
	nodeLister      corelisters.NodeLister
	cncLister       networkconnectlister.ClusterNetworkConnectLister

	controller controller.Controller
	// cncController tracks CNC connectivity and updates derived
	// network activity for Dynamic UDN filtering.
	cncController controller.Controller
	recorder      record.EventRecorder

	// networkController reconciles network specific controllers
	networkController *networkController

	// nads to network mapping
	nads map[string]string
	// nadsByNetwork tracks NAD keys grouped by network name.
	nadsByNetwork map[string]sets.Set[string]
	// dynamicFilterNADs tracks whether a NAD should be activity-gated by Dynamic UDN.
	// Bare NADs are not filtered and therefore should be treated as present on all nodes.
	dynamicFilterNADs map[string]bool
	// cncSelectedNetworks tracks network names selected by each CNC.
	cncSelectedNetworks map[string]sets.Set[string]
	// cncNetworkIDs tracks owner network IDs referenced by each CNC.
	cncNetworkIDs map[string]sets.Set[int]
	// cncsByNetworkID indexes CNCs by their referenced owner network IDs.
	cncsByNetworkID map[int]sets.Set[string]
	// cncConnectedNetworks is a symmetric adjacency map where key/value are
	// network names connected through a CNC.
	cncConnectedNetworks map[string]sets.Set[string]

	// primaryNADs holds a mapping of namespace to NAD of primary UDNs
	primaryNADs map[string]string

	// networkIDAllocator is used by cluster-manager to allocate new IDs; node mode only uses it as a cache.
	networkIDAllocator  id.Allocator
	tunnelKeysAllocator *id.TunnelKeysAllocator
	nadClient           nadclientset.Interface

	markedForRemoval map[string]time.Time
	// dynamicallyRemovedNADs tracks NADs that still exist in the informer but
	// have already been locally removed after their Dynamic UDN inactivity grace
	// period expired. This prevents repeated local delete scheduling while the
	// NAD remains inactive.
	dynamicallyRemovedNADs sets.Set[string]

	// filterNADsOnNode is the node identity used for local Dynamic UDN NAD
	// rendering decisions. When set, this controller only renders a dynamic UDN
	// NAD if the network is active for this node. Empty means this controller is
	// cluster-scoped and does not locally filter NAD rendering, even when Dynamic
	// UDN allocation is enabled.
	filterNADsOnNode string

	podTracker      *PodTrackerController
	egressIPTracker *EgressIPTrackerController
	podReconcilerID uint64
	eipReconcilerID uint64
}

type reconcilerRegistration struct {
	id uint64
	r  NADReconciler
}

type networkRefReconcilerRegistration struct {
	id uint64
	r  NetworkRefReconciler
}

func newController(
	name string,
	node string,
	cm ControllerManager,
	wf watchFactory,
	ovnClient *util.OVNClusterManagerClientset,
	recorder record.EventRecorder,
	tunnelKeysAllocator *id.TunnelKeysAllocator,
	filterNADsOnNode string,
	asyncNetworkStart bool,
) (*nadController, error) {
	networkController := newNetworkController(name, node, cm, wf, asyncNetworkStart)
	c := &nadController{
		name:                   fmt.Sprintf("[%s NAD controller]", name),
		stopChan:               make(chan struct{}),
		recorder:               recorder,
		nadLister:              wf.NADInformer().Lister(),
		nodeLister:             wf.NodeCoreInformer().Lister(),
		networkController:      networkController,
		reconcilers:            map[uint64]reconcilerRegistration{},
		networkRefReconcilers:  map[uint64]networkRefReconcilerRegistration{},
		nads:                   map[string]string{},
		nadsByNetwork:          map[string]sets.Set[string]{},
		dynamicFilterNADs:      map[string]bool{},
		cncSelectedNetworks:    map[string]sets.Set[string]{},
		cncNetworkIDs:          map[string]sets.Set[int]{},
		cncsByNetworkID:        map[int]sets.Set[string]{},
		cncConnectedNetworks:   map[string]sets.Set[string]{},
		primaryNADs:            map[string]string{},
		markedForRemoval:       map[string]time.Time{},
		dynamicallyRemovedNADs: sets.New[string](),
	}
	networkController.getNADKeysForNetwork = c.GetNADKeysForNetwork

	if cm != nil && config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		c.podTracker = NewPodTrackerController(fmt.Sprintf("%s pod-tracker", c.name), wf, c.OnNetworkRefChange, c.GetPrimaryNADForNamespace)
		podID := c.RegisterNADReconciler(c.podTracker.NADReconciler())
		c.podReconcilerID = podID
		if config.OVNKubernetesFeature.EnableEgressIP {
			c.egressIPTracker = NewEgressIPTrackerController(fmt.Sprintf("%s egress-ip-tracker", c.name), wf, c.OnNetworkRefChange, c.GetPrimaryNADForNamespace)
			eipID := c.RegisterNADReconciler(c.egressIPTracker.NADReconciler())
			c.eipReconcilerID = eipID
		}
		c.filterNADsOnNode = filterNADsOnNode
		if util.IsNetworkConnectEnabled() {
			cncInformer := wf.ClusterNetworkConnectInformer()
			c.cncLister = cncInformer.Lister()
			c.cncController = controller.NewController(
				fmt.Sprintf("%s-cnc-connectivity-controller", c.name),
				&controller.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
					RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
					Informer:       cncInformer.Informer(),
					Lister:         c.cncLister.List,
					Reconcile:      c.syncCNC,
					ObjNeedsUpdate: c.cncNeedsUpdate,
					Threadiness:    1,
				},
			)
		}
	}

	c.networkController.nodeHasNetwork = c.NodeHasNetwork

	if ovnClient != nil {
		c.nadClient = ovnClient.NetworkAttchDefClient
	}

	c.networkIDAllocator = id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	// Reserve the ID of the default network
	err := c.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate default network ID: %w", err)
	}
	if node == "" {
		// tunnelKeysAllocator must be passed for cluster manager
		c.tunnelKeysAllocator = tunnelKeysAllocator
	}

	config := &controller.ControllerConfig[nettypes.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.NADInformer().Informer(),
		Lister:         c.nadLister.List,
		Reconcile:      c.sync,
		ObjNeedsUpdate: c.nadNeedsUpdate,
		Threadiness:    1,
	}

	if util.IsNetworkSegmentationSupportEnabled() {
		if udnInformer := wf.UserDefinedNetworkInformer(); udnInformer != nil {
			c.udnLister = udnInformer.Lister()
		}
		if cudnInformer := wf.ClusterUserDefinedNetworkInformer(); cudnInformer != nil {
			c.cudnLister = cudnInformer.Lister()
		}
		if nsInformer := wf.NamespaceInformer(); nsInformer != nil {
			c.namespaceLister = nsInformer.Lister()
		}
	}
	c.controller = controller.NewController(
		c.name,
		config,
	)

	return c, nil
}

// usesLocalDynamicFiltering reports whether this controller instance should
// gate local NAD rendering on Dynamic UDN node activity. Cluster-scoped
// instances keep all NADs rendered and leave per-node allocation decisions to
// their network controllers.
func (c *nadController) usesLocalDynamicFiltering() bool {
	return c.filterNADsOnNode != ""
}

func (c *nadController) nodeHasDirectNAD(node, nad string) bool {
	if !config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		return true
	}
	if c.podTracker != nil && c.podTracker.NodeHasNAD(node, nad) {
		return true
	}
	if c.egressIPTracker != nil && c.egressIPTracker.NodeHasNAD(node, nad) {
		return true
	}
	return false
}

// nodeHasNAD reports whether a node should render the given NAD. A Dynamic UDN
// NAD is active when its network is active; the network can be active through
// any of its NADs' pod/EgressIP refs or through a CNC-connected network.
// syncNAD records this NAD in nadsByNetwork before filtering, so the network
// activity lookup can include the current NAD and all same-network peers.
// Caller must hold nadController lock.
func (c *nadController) nodeHasNAD(node string, nad *nettypes.NetworkAttachmentDefinition) bool {
	if !config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		return true
	}
	nadKey := util.GetNADName(nad.Namespace, nad.Name)
	networkName := c.nads[nadKey]
	if networkName == "" {
		nadNetwork, err := util.ParseNADInfo(nad)
		if err != nil || nadNetwork == nil {
			return false
		}
		networkName = nadNetwork.GetNetworkName()
	}

	return c.nodeHasNetworkNoLock(node, networkName)
}

// nodeHasDirectNetworkNoLock reports whether the node has any direct NAD
// reference for the provided network.
// Caller must hold nadController lock.
func (c *nadController) nodeHasDirectNetworkNoLock(node, networkName string) bool {
	if networkName == "" {
		return false
	}
	nadSet := c.nadsByNetwork[networkName]
	if len(nadSet) == 0 {
		return false
	}
	for nad := range nadSet {
		if !c.dynamicFilterNADs[nad] {
			return true
		}
		if c.nodeHasDirectNAD(node, nad) {
			return true
		}
	}
	return false
}

// nodeHasNetworkNoLock reports whether the node is active for the network
// either directly or via CNC-connected networks.
// Caller must hold nadController lock.
func (c *nadController) nodeHasNetworkNoLock(node, networkName string) bool {
	if c.nodeHasDirectNetworkNoLock(node, networkName) {
		return true
	}
	for connectedNetwork := range c.cncConnectedNetworks[networkName] {
		if c.nodeHasDirectNetworkNoLock(node, connectedNetwork) {
			return true
		}
	}
	return false
}

func (c *nadController) NodeHasNetwork(node, networkName string) bool {
	if !config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		return true
	}
	if networkName == "" {
		return false
	}
	if networkName == types.DefaultNetworkName {
		return true
	}
	c.RLock()
	defer c.RUnlock()
	return c.nodeHasNetworkNoLock(node, networkName)
}

func nadRequiresDynamicFiltering(nad *nettypes.NetworkAttachmentDefinition) bool {
	ownerRef := metav1.GetControllerOf(nad)
	if ownerRef == nil {
		return false
	}

	return ownerRef.Kind == "ClusterUserDefinedNetwork" || ownerRef.Kind == "UserDefinedNetwork"
}

// addNADToNetworkLocked must be called with nadController locked
func (c *nadController) addNADToNetworkLocked(networkName, nadKey string) {
	if networkName == "" {
		return
	}
	if c.nadsByNetwork == nil {
		c.nadsByNetwork = map[string]sets.Set[string]{}
	}
	nadSet := c.nadsByNetwork[networkName]
	if nadSet == nil {
		nadSet = sets.New[string]()
		c.nadsByNetwork[networkName] = nadSet
	}
	nadSet.Insert(nadKey)
}

// deleteNADFromNetworkLocked must be called with nadController locked
func (c *nadController) deleteNADFromNetworkLocked(networkName, nadKey string) {
	if networkName == "" {
		return
	}
	nadSet := c.nadsByNetwork[networkName]
	if nadSet == nil {
		return
	}
	nadSet.Delete(nadKey)
	if len(nadSet) == 0 {
		delete(c.nadsByNetwork, networkName)
	}
}

// OnNetworkRefChange is a callback function used to signal an action to this controller when
// a network needs to be added or removed or just updated.
// Used as a callback for pod/egress IP events when dynamic UDN allocation is enabled.
// This callback is invoked by pod/egress IP trackers and is blocking. Therefore its work should be as lightweight
// as possible.
// The function handles:
//  1. Queuing local node event NADs to the NAD Controller for reconciliation later in the NAD Controller worker.
//  2. Queuing remote node event networks to the Network Manager for reconciliation later in the Network Manager worker.
//
// Trackers invoke this callback after releasing their cache locks, so local
// activity checks are safe here but should stay brief.
func (c *nadController) OnNetworkRefChange(node, nadNamespacedName string, active bool) {
	klog.V(4).Infof("%s Network change for node controller triggered by pod/egress IP events "+
		"on node: %s, NAD: %s, active: %t", c.name, node, nadNamespacedName, active)

	namespace, name, err := cache.SplitMetaNamespaceKey(nadNamespacedName)
	if err != nil {
		klog.Errorf("Failed splitting key %q, falling back to normal network reconcile: %v", nadNamespacedName, err)
		// fallback to regular reconcile
		c.reconcile(nadNamespacedName)
		return
	}

	nadNetwork := c.GetNetInfoForNADKey(nadNamespacedName)
	if nadNetwork == nil {
		nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
		if err != nil {
			klog.Errorf("Failed to find NAD %q in informer, falling back to normal network reconcile: %v", nadNamespacedName, err)
			// fallback to regular reconcile
			c.reconcile(nadNamespacedName)
			return
		}

		ownerRef := metav1.GetControllerOf(nad)
		if ownerRef == nil {
			return
		}

		if ownerRef.Kind != "ClusterUserDefinedNetwork" && ownerRef.Kind != "UserDefinedNetwork" {
			return
		}

		nadNetwork, err = util.ParseNADInfo(nad)
		if err != nil || nadNetwork == nil {
			klog.Errorf("Failed to parse NAD %q info, falling back to normal network reconcile: %v", nadNamespacedName, err)
			// fallback to regular reconcile
			c.reconcile(nadNamespacedName)
			return
		}
	} else if !nadNetwork.IsUserDefinedNetwork() {
		return
	}

	isLocal := c.usesLocalDynamicFiltering() && node == c.filterNADsOnNode
	networkName := nadNetwork.GetNetworkName()
	affectedNetworks := c.getNetworkAndConnectedNetworks(networkName)
	for _, affectedNetwork := range affectedNetworks {
		c.notifyNetworkRefReconcilers(node, affectedNetwork)
	}
	// Enqueue a network reconcile for remote nodes (non-blocking).
	if !isLocal {
		for _, affectedNetwork := range affectedNetworks {
			c.networkController.NotifyNetworkRefChange(affectedNetwork, node)
		}
	}
	// Let the NAD controller handle lifecycle/teardown decisions asynchronously for local networks only.
	if isLocal {
		// Tracker events can arrive before syncNAD has populated nadsByNetwork
		// for this NAD. Requeue the changed key directly, then requeue any
		// already-known NADs for this network and its CNC-connected peers.
		c.reconcile(nadNamespacedName)
		c.reconcileNetworkActivity(affectedNetworks)
	}

}

// getNetworkAndConnectedNetworks returns the provided network and all networks
// directly connected to it through CNC connectivity.
func (c *nadController) getNetworkAndConnectedNetworks(networkName string) []string {
	if networkName == "" {
		return nil
	}
	c.RLock()
	defer c.RUnlock()

	networks := make([]string, 0, len(c.cncConnectedNetworks[networkName])+1)
	networks = append(networks, networkName)
	for connectedNetwork := range c.cncConnectedNetworks[networkName] {
		networks = append(networks, connectedNetwork)
	}
	return networks
}

// reconcileNetworkActivity requeues NAD sync for all NADs belonging to the
// provided networks. syncNAD recomputes current activity and updates local
// Dynamic UDN removal state.
func (c *nadController) reconcileNetworkActivity(networkNames []string) {
	if len(networkNames) == 0 {
		return
	}

	for _, networkName := range networkNames {
		if networkName == "" {
			continue
		}
		nadKeys := c.GetNADKeysForNetwork(networkName)
		if len(nadKeys) == 0 {
			continue
		}

		for _, nadKey := range nadKeys {
			c.reconcile(nadKey)
		}
	}
}

// filter should only be called when this controller uses local Dynamic UDN filtering.
func (c *nadController) filter(nad *nettypes.NetworkAttachmentDefinition) (bool, error) {
	if !nadRequiresDynamicFiltering(nad) {
		return false, nil
	}

	ourNode := c.filterNADsOnNode

	if c.nodeHasNAD(ourNode, nad) {
		return false, nil
	}

	return true, nil
}

func (c *nadController) Interface() Interface {
	return c
}

func (c *nadController) Start() error {
	// initial sync here will ensure networks in network manager
	// network manager will use this initial set of ensured networks to consider
	// any other network stale on its own sync
	err := controller.StartWithInitialSync(
		c.syncAll,
		c.controller,
	)
	if err != nil {
		return err
	}

	if c.cncController != nil {
		err = controller.StartWithInitialSync(
			c.syncAllCNCs,
			c.cncController,
		)
		if err != nil {
			return err
		}
	}

	// Pod and Egress IP Trackers start and process existing pods/egress IPs.
	// The trackers warm up their cache and trigger OnNetworkRefChange to queue keys
	// to NAD Controller.
	if c.podTracker != nil {
		if err := c.podTracker.Start(); err != nil {
			return fmt.Errorf("failed to start pod tracker: %w", err)
		}
	}

	if c.egressIPTracker != nil {
		if err := c.egressIPTracker.Start(); err != nil {
			return fmt.Errorf("failed to start egress ip tracker: %w", err)
		}
	}

	// NetworkController starts last and starts to process network keys to spin up network controllers.
	// At this point the tracker cache's are warm to get accurate information for filtering.
	err = c.networkController.Start()
	if err != nil {
		return err
	}

	klog.Infof("%s: started", c.name)
	return nil
}

func (c *nadController) Stop() {
	klog.Infof("%s: shutting down", c.name)
	c.stopOnce.Do(func() {
		close(c.stopChan)
	})
	controller.Stop(c.controller)
	if c.cncController != nil {
		controller.Stop(c.cncController)
	}
	c.networkController.Stop()
	if c.podReconcilerID != 0 {
		c.DeRegisterNADReconciler(c.podReconcilerID)
	}
	if c.podTracker != nil {
		c.podTracker.Stop()
	}
	if c.eipReconcilerID != 0 {
		c.DeRegisterNADReconciler(c.eipReconcilerID)
	}
	if c.egressIPTracker != nil {
		c.egressIPTracker.Stop()
	}
}

// cncNeedsUpdate decides if a CNC update may change derived activity mapping.
func (c *nadController) cncNeedsUpdate(oldObj, newObj *networkconnectv1.ClusterNetworkConnect) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec.Connectivity, newObj.Spec.Connectivity) ||
		util.NetworkConnectSubnetAnnotationChanged(oldObj, newObj)
}

// networkNameForIDLocked resolves a network name from the cached network ID.
// Caller must hold nadController lock.
func (c *nadController) networkNameForIDLocked(networkID int) string {
	if networkID == types.InvalidID {
		return ""
	}
	for networkName := range c.nadsByNetwork {
		if c.networkIDAllocator.GetID(networkName) == networkID {
			return networkName
		}
	}
	return ""
}

func (c *nadController) networkSelectionsForCNC(cnc *networkconnectv1.ClusterNetworkConnect) (sets.Set[string], sets.Set[int]) {
	selectedNetworks := sets.New[string]()
	networkIDs := sets.New[int]()
	subnets, err := util.ParseNetworkConnectSubnetAnnotation(cnc)
	if err != nil {
		return selectedNetworks, networkIDs
	}

	c.RLock()
	defer c.RUnlock()
	for owner := range subnets {
		_, networkID, err := util.ParseNetworkOwner(owner)
		if err != nil {
			continue
		}
		networkIDs.Insert(networkID)
		networkName := c.networkNameForIDLocked(networkID)
		if networkName == "" {
			continue
		}
		selectedNetworks.Insert(networkName)
	}
	return selectedNetworks, networkIDs
}

// buildCNCConnectedNetworks returns network name -> connected peer network names
// derived from each CNC's selected network set.
func buildCNCConnectedNetworks(selectedNetworksByCNC map[string]sets.Set[string]) map[string]sets.Set[string] {
	connectedNetworks := map[string]sets.Set[string]{}
	for _, selectedNetworks := range selectedNetworksByCNC {
		for selectedNetwork := range selectedNetworks {
			peers := connectedNetworks[selectedNetwork]
			if peers == nil {
				peers = sets.New[string]()
				connectedNetworks[selectedNetwork] = peers
			}
			for peer := range selectedNetworks {
				if selectedNetwork == peer {
					continue
				}
				peers.Insert(peer)
			}
		}
	}
	return connectedNetworks
}

func changedCNCNetworks(oldConnected, newConnected map[string]sets.Set[string]) sets.Set[string] {
	changedNetworks := sets.New[string]()
	for networkName := range oldConnected {
		changedNetworks.Insert(networkName)
	}
	for networkName := range newConnected {
		changedNetworks.Insert(networkName)
	}

	networksToReconcile := sets.New[string]()
	for networkName := range changedNetworks {
		oldPeers := oldConnected[networkName]
		if oldPeers == nil {
			oldPeers = sets.New[string]()
		}
		newPeers := newConnected[networkName]
		if newPeers == nil {
			newPeers = sets.New[string]()
		}
		if !oldPeers.Equal(newPeers) {
			networksToReconcile.Insert(networkName)
		}
	}
	return networksToReconcile
}

// updateCNCConnectivityLocked refreshes the derived CNC adjacency map from the
// selected network cache and returns networks whose peer set changed.
// Caller must hold nadController lock.
func (c *nadController) updateCNCConnectivityLocked() sets.Set[string] {
	connectedNetworks := buildCNCConnectedNetworks(c.cncSelectedNetworks)
	networksToReconcile := changedCNCNetworks(c.cncConnectedNetworks, connectedNetworks)
	c.cncConnectedNetworks = connectedNetworks
	return networksToReconcile
}

// updateCNCNetworkIDsLocked updates the per-CNC owner ID cache and reverse
// lookup used to target CNC reconciles when a NAD/network ID becomes available
// or is removed.
// Caller must hold nadController lock.
func (c *nadController) updateCNCNetworkIDsLocked(cncName string, networkIDs sets.Set[int]) {
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

// syncAllCNCs rebuilds CNC selection state from the lister. It is used for
// controller startup; per-key queue events should use syncCNC.
func (c *nadController) syncAllCNCs() error {
	cncs, err := c.cncLister.List(labels.Everything())
	if err != nil {
		return err
	}

	selectedNetworksByCNC := map[string]sets.Set[string]{}
	networkIDsByCNC := map[string]sets.Set[int]{}
	cncsByNetworkID := map[int]sets.Set[string]{}
	for _, cnc := range cncs {
		selectedNetworks, networkIDs := c.networkSelectionsForCNC(cnc)
		selectedNetworksByCNC[cnc.Name] = selectedNetworks
		networkIDsByCNC[cnc.Name] = networkIDs
		for networkID := range networkIDs {
			indexedCNCs := cncsByNetworkID[networkID]
			if indexedCNCs == nil {
				indexedCNCs = sets.New[string]()
				cncsByNetworkID[networkID] = indexedCNCs
			}
			indexedCNCs.Insert(cnc.Name)
		}
	}

	c.Lock()
	c.cncSelectedNetworks = selectedNetworksByCNC
	c.cncNetworkIDs = networkIDsByCNC
	c.cncsByNetworkID = cncsByNetworkID
	networksToReconcile := c.updateCNCConnectivityLocked()
	c.Unlock()

	c.reconcileNetworkActivity(networksToReconcile.UnsortedList())
	return nil
}

// syncCNC refreshes one queued CNC and requeues affected networks when
// connectivity relationships change.
func (c *nadController) syncCNC(key string) error {
	cnc, err := c.cncLister.Get(key)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	var selectedNetworks sets.Set[string]
	var networkIDs sets.Set[int]
	if err == nil {
		selectedNetworks, networkIDs = c.networkSelectionsForCNC(cnc)
	}

	c.Lock()
	if c.cncSelectedNetworks == nil {
		c.cncSelectedNetworks = map[string]sets.Set[string]{}
	}
	if selectedNetworks == nil {
		delete(c.cncSelectedNetworks, key)
	} else {
		c.cncSelectedNetworks[key] = selectedNetworks
	}
	c.updateCNCNetworkIDsLocked(key, networkIDs)
	networksToReconcile := c.updateCNCConnectivityLocked()
	c.Unlock()

	c.reconcileNetworkActivity(networksToReconcile.UnsortedList())
	return nil
}

// reconcileCNCsForNetworkIDs requeues only CNCs indexed by one of the affected
// network IDs in their subnet annotation owner keys.
func (c *nadController) reconcileCNCsForNetworkIDs(networkIDs ...int) {
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

// RegisterNADReconciler registers a reconciler to receive NAD keys for reconciliation.
func (c *nadController) RegisterNADReconciler(r NADReconciler) uint64 {
	c.Lock()
	defer c.Unlock()
	if c.reconcilers == nil {
		c.reconcilers = map[uint64]reconcilerRegistration{}
	}
	c.nextReconcilerID++
	id := c.nextReconcilerID
	c.reconcilers[id] = reconcilerRegistration{id: id, r: r}
	return id
}

// DeRegisterNADReconciler removes a previously registered reconciler by ID.
func (c *nadController) DeRegisterNADReconciler(id uint64) {
	c.Lock()
	defer c.Unlock()
	if _, ok := c.reconcilers[id]; !ok {
		return
	}
	delete(c.reconcilers, id)
}

// RegisterNetworkRefReconciler registers a reconciler to receive node+network activity changes.
func (c *nadController) RegisterNetworkRefReconciler(r NetworkRefReconciler) uint64 {
	c.Lock()
	defer c.Unlock()
	if c.networkRefReconcilers == nil {
		c.networkRefReconcilers = map[uint64]networkRefReconcilerRegistration{}
	}
	c.nextNetworkRefReconcilerID++
	id := c.nextNetworkRefReconcilerID
	c.networkRefReconcilers[id] = networkRefReconcilerRegistration{id: id, r: r}
	return id
}

// DeRegisterNetworkRefReconciler removes a previously registered network-ref reconciler by ID.
func (c *nadController) DeRegisterNetworkRefReconciler(id uint64) {
	c.Lock()
	defer c.Unlock()
	if _, ok := c.networkRefReconcilers[id]; !ok {
		return
	}
	delete(c.networkRefReconcilers, id)
}

// notifyReconcilers enqueues the NAD key to all registered reconcilers
// Must be called with nadController Mutex locked
func (c *nadController) notifyReconcilers(key string) {
	for _, entry := range c.reconcilers {
		entry.r.Reconcile(key)
	}
}

func (c *nadController) notifyNetworkRefReconcilers(node, networkName string) {
	if node == "" || networkName == "" {
		return
	}
	c.RLock()
	defer c.RUnlock()
	for _, entry := range c.networkRefReconcilers {
		entry.r.Reconcile(node, networkName)
	}
}

func (c *nadController) reconcile(key string) {
	c.controller.Reconcile(key)
}

// setMarkedForRemovalLocked starts the Dynamic UDN inactivity grace period for
// a NAD that is currently filtered from local rendering.
// Caller must hold nadController lock.
func (c *nadController) setMarkedForRemovalLocked(key string) {
	if c.dynamicallyRemovedNADs.Has(key) {
		return
	}
	if _, ok := c.markedForRemoval[key]; ok {
		return
	}
	if c.markedForRemoval == nil {
		c.markedForRemoval = map[string]time.Time{}
	}
	removalTime := time.Now().Add(config.OVNKubernetesFeature.UDNDeletionGracePeriod)
	c.markedForRemoval[key] = removalTime

	// ensure we reconcile later
	stopCh := c.stopChan
	go func() {
		klog.V(5).Infof("Scheduling to remove nad %q after %v", key, removalTime)
		timer := time.NewTimer(time.Until(removalTime))
		defer timer.Stop()

		select {
		case <-stopCh:
			return
		case <-timer.C:
			shouldReconcile := false
			c.Lock()
			if rt, ok := c.markedForRemoval[key]; ok && time.Now().After(rt) {
				shouldReconcile = true
			}
			c.Unlock()
			if shouldReconcile {
				c.reconcile(key)
			}
		}
	}()
}

// clearDynamicRemovalStateLocked clears pending or completed local dynamic
// removal state for a NAD that should render again or no longer exists.
// Caller must hold nadController lock.
func (c *nadController) clearDynamicRemovalStateLocked(key string) {
	delete(c.markedForRemoval, key)
	if c.dynamicallyRemovedNADs != nil {
		c.dynamicallyRemovedNADs.Delete(key)
	}
}

func (c *nadController) syncAll() (err error) {
	existingNADs, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("%s: failed to list NADs: %w", c.name, err)
	}

	syncNAD := func(nad *nettypes.NetworkAttachmentDefinition) error {
		key, err := cache.MetaNamespaceKeyFunc(nad)
		if err != nil {
			klog.Errorf("%s: failed to sync %v: %v", c.name, nad, err)
			return nil
		}
		err = c.syncNAD(key, nad)
		if err != nil {
			return fmt.Errorf("%s: failed to sync %s: %v", c.name, key, err)
		}
		return nil
	}

	// create all networks with their updated list of NADs and only then start
	// the corresponding controllers so as to avoid to the extent possible the
	// errors and retries that would result if the controller attempted to
	// process pods attached with NADs we wouldn't otherwise know about yet
	nadsWithoutID := []*nettypes.NetworkAttachmentDefinition{}
	for _, nad := range existingNADs {
		// skip NADs that are not annotated with an ID
		if nad.Annotations[types.OvnNetworkIDAnnotation] == "" {
			nadsWithoutID = append(nadsWithoutID, nad)
			continue
		}
		err := syncNAD(nad)
		if err != nil {
			return err
		}
	}

	if len(nadsWithoutID) == 0 {
		return nil
	}

	// preallocate all node IDs to avoid new NADs taking them post start up
	// If we are missing IDs, get them from the nodes which is where we
	// originally had them
	klog.Infof("%s: %d NADs are missing the network ID annotation, fetching from nodes", c.name, len(nadsWithoutID))
	for _, nad := range nadsWithoutID {
		nadNetwork, err := util.ParseNADInfo(nad)
		if err != nil {
			// in case the type for the NAD is not ovn-k we should not record the error event
			if err.Error() != util.ErrorAttachDefNotOvnManaged.Error() {
				klog.Errorf("%s: failed parsing NAD %s/%s: %v", c.name, nad.Namespace, nad.Name, err)
			}
			continue
		}
		netID, err := c.getNetworkIDFromNode(nadNetwork)
		if err != nil {
			return fmt.Errorf("%s: failed to fetch network ID from nodes for nad %s/%s: %v",
				c.name, nad.Namespace, nad.Name, err)
		}
		if netID != types.InvalidID {
			// Reserve the id for the network name. We can safely
			// ignore any errors if there are duplicate ids or if
			// two networks have the same id. We will assign network
			// IDs anyway on sync.
			_ = c.networkIDAllocator.ReserveID(nadNetwork.GetNetworkName(), netID)
		}
	}

	// finally process the pending NADs
	for _, nad := range nadsWithoutID {
		err := syncNAD(nad)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *nadController) sync(key string) error {
	startTime := time.Now()
	klog.V(5).Infof("%s: sync NAD %s", c.name, key)
	defer func() {
		klog.V(4).Infof("%s: finished syncing NAD %s, took %v", c.name, key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("%s: failed splitting key %s: %v", c.name, key, err)
		return nil
	}

	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return c.syncNAD(key, nad)
}

func (c *nadController) syncNAD(key string, nad *nettypes.NetworkAttachmentDefinition) (syncErr error) {
	var nadNetworkName string
	var nadNetwork util.NetInfo
	var oldNetwork, ensureNetwork util.MutableNetInfo
	var err error
	dynamicDelete := false
	shouldReconcileCNC := false
	affectedCNCNetworkIDs := sets.New[int]()

	// Reconcile CNC connectivity after unlock when this NAD's network mapping
	// changes. This closes the window where syncCNC can run before NAD/network
	// mapping is available and skip that network.
	defer func() {
		if c.cncController == nil || syncErr != nil || !shouldReconcileCNC {
			return
		}
		c.reconcileCNCsForNetworkIDs(affectedCNCNetworkIDs.UnsortedList()...)
	}()

	c.Lock()
	defer c.Unlock()

	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("%s: failed splitting key %s: %v", c.name, key, err)
	}
	previousNetworkName := c.nads[key]
	previousNetworkID := types.InvalidID
	if previousNetworkName != "" {
		previousNetworkID = c.networkIDAllocator.GetID(previousNetworkName)
	}

	deleteTime, setforDeletion := c.markedForRemoval[key]
	removalExpired := setforDeletion && time.Now().After(deleteTime)
	if removalExpired && c.usesLocalDynamicFiltering() && c.nodeHasNetworkNoLock(c.filterNADsOnNode, previousNetworkName) {
		// Activity may have returned while the grace-period reconcile was still
		// queued. Since syncNAD is now the source of truth for activity state,
		// clear the stale timer instead of honoring the expired edge.
		c.clearDynamicRemovalStateLocked(key)
		removalExpired = false
	}
	if removalExpired {
		// Grace period expired. Force a local teardown, but keep caches aligned to informer state.
		klog.Infof("%s: NAD %q: marked for deletion and time has expired, will remove locally", c.name, key)
		dynamicDelete = nad != nil
		// Act like a delete for rendering/ensure paths
		nad = nil
		defer func() {
			if syncErr == nil {
				delete(c.markedForRemoval, key)
				if dynamicDelete {
					if c.dynamicallyRemovedNADs == nil {
						c.dynamicallyRemovedNADs = sets.New[string]()
					}
					c.dynamicallyRemovedNADs.Insert(key)
				}
			}
		}()
	}

	if nad != nil {
		nadNetwork, err = util.ParseNADInfo(nad)
		if err != nil {
			// in case the type for the NAD is not ovn-k we should not record the error event
			if err.Error() == util.ErrorAttachDefNotOvnManaged.Error() {
				return nil
			}

			if c.recorder != nil {
				c.recorder.Eventf(&corev1.ObjectReference{Kind: nad.Kind, Namespace: nad.Namespace, Name: nad.Name}, corev1.EventTypeWarning,
					"InvalidConfig", "Failed to parse network config: %v", err.Error())
			}
			klog.Errorf("%s: failed parsing NAD %s: %v", c.name, key, err)
			return nil
		}
		nadNetworkName = nadNetwork.GetNetworkName()
		if c.dynamicFilterNADs == nil {
			c.dynamicFilterNADs = map[string]bool{}
		}
		c.dynamicFilterNADs[key] = nadRequiresDynamicFiltering(nad)
	}

	defer func() {
		currentNetworkName := c.nads[key]
		currentNetworkID := types.InvalidID
		if currentNetworkName != "" {
			currentNetworkID = c.networkIDAllocator.GetID(currentNetworkName)
		}
		if previousNetworkName != currentNetworkName || previousNetworkID != currentNetworkID {
			shouldReconcileCNC = true
			if previousNetworkID != types.InvalidID {
				affectedCNCNetworkIDs.Insert(previousNetworkID)
			}
			if currentNetworkID != types.InvalidID {
				affectedCNCNetworkIDs.Insert(currentNetworkID)
			}
		}
		c.notifyReconcilers(key) // notify reconcilers after the sync runs with the latest information
	}()

	// We can only have one primary NAD per namespace
	primaryNAD := c.primaryNADs[namespace]
	if nadNetwork != nil && nadNetwork.IsPrimaryNetwork() && primaryNAD != "" && primaryNAD != key {
		return fmt.Errorf("%s: NAD %s is primary for the namespace, NAD %s can't be primary", c.name, primaryNAD, key)
	}

	// As multiple NADs may define networks with the same name, these networks
	// should also have the same config to be considered compatible. If an
	// incompatible network change happens on NAD update, we can:
	// - Re-create network with the same name but different config, if it is not
	//   referred to by any other NAD
	// - Return an error AND clean up NAD from the old network

	// the NAD refers to a different network than before
	if nadNetworkName != c.nads[key] {
		oldNetwork = c.networkController.getNetwork(c.nads[key])
	}

	currentNetwork := c.networkController.getNetwork(nadNetworkName)

	switch {
	case currentNetwork == nil:
		// the NAD refers to a new network, ensure it
		ensureNetwork = util.NewMutableNetInfo(nadNetwork)
	case util.AreNetworksCompatible(currentNetwork, nadNetwork):
		// the NAD refers to an existing compatible network, ensure that
		// existing network holds references to all NADs while still allowing
		// dynamic fields from the latest NAD config to reconcile.
		ensureNetwork = util.NewMutableNetInfo(nadNetwork)
		if nadSet := c.nadsByNetwork[nadNetworkName]; len(nadSet) > 0 {
			ensureNetwork.AddNADs(nadSet.UnsortedList()...)
		}
	case func() bool {
		nadSet := c.nadsByNetwork[nadNetworkName]
		return len(nadSet) == 1 && nadSet.Has(key)
	}():
		// the NAD is the only NAD referring to an existing incompatible
		// network, remove the reference from the old network and ensure that
		// existing network holds a reference to this NAD
		oldNetwork = currentNetwork
		ensureNetwork = util.NewMutableNetInfo(nadNetwork)
	// the NAD refers to an existing incompatible network referred by other
	// NADs, return error
	case oldNetwork == nil:
		// the NAD refers to the same network as before but with different
		// incompatible configuration, remove the NAD reference from the network
		oldNetwork = currentNetwork
		fallthrough
	default:
		err = fmt.Errorf("%s: NAD %s CNI config does not match that of network %s", c.name, key, nadNetworkName)
	}

	// remove the NAD reference from the old network and delete the network if
	// it is no longer referenced
	if oldNetwork != nil {
		klog.V(5).Infof("%s: removing NAD %q reference for network %q", c.name, key, oldNetwork.GetNetworkName())
		oldNetworkName := oldNetwork.GetNetworkName()
		oldNetwork.DeleteNADs(key)
		// NADs removed locally after their Dynamic UDN grace period stay in
		// nadsByNetwork for the informer but no longer render the network.
		rendered := c.nadsByNetwork[oldNetworkName].Difference(c.dynamicallyRemovedNADs).Delete(key)
		if rendered.Len() == 0 || oldNetworkName == types.DefaultNetworkName {
			c.networkController.DeleteNetwork(oldNetworkName)
		} else {
			c.networkController.EnsureNetwork(oldNetwork)
		}
		if !dynamicDelete && c.primaryNADs[namespace] == key {
			delete(c.primaryNADs, namespace)
		}
	}

	// handleNetworkAnnotations prevents duplicated IDs from being allocated, so we call it even
	// if the NAD/network is filtered by Dynamic UDN while we are ensuring the network.
	// handleNetworkAnnotations also handles deletion and releasing based on NAD cache state
	// IDs are not released during dynamicDeletes (going inactive) and are only released on a true
	// NAD/network delete
	if !dynamicDelete {
		if err := c.handleNetworkAnnotations(ensureNetwork, nad, key, previousNetworkName); err != nil {
			return err
		}
	}

	// this was a nad delete
	if ensureNetwork == nil {
		// On a true delete (incoming nad nil or expired grace period) we must clean caches,
		// except for dynamicDelete where we keep informer-derived state.
		if !dynamicDelete {
			c.clearDynamicRemovalStateLocked(key)
			// clean up primary mapping even if we never had an oldNetwork rendered
			if c.primaryNADs[namespace] == key {
				delete(c.primaryNADs, namespace)
			}
			networkName := previousNetworkName
			delete(c.nads, key)
			delete(c.dynamicFilterNADs, key)
			if networkName != "" {
				c.deleteNADFromNetworkLocked(networkName, key)
			}
		}
		return err
	}

	// if network ID has not been set and this is not the well known default
	// network, need to wait until cluster nad controller allocates an ID for
	// the network
	if ensureNetwork.GetNetworkID() == types.InvalidID {
		klog.V(4).Infof("%s: will wait for cluster manager to allocate an ID before ensuring network %s, NAD: %s",
			c.name, nadNetworkName, key)
		return nil
	}

	klog.V(5).Infof("%s: ensuring NAD %q reference for network %q with id %d",
		c.name, key, ensureNetwork.GetNetworkName(), ensureNetwork.GetNetworkID())

	networkName := ensureNetwork.GetNetworkName()
	if previousNetworkName != "" && previousNetworkName != networkName {
		c.deleteNADFromNetworkLocked(previousNetworkName, key)
	}
	c.nads[key] = networkName
	c.addNADToNetworkLocked(networkName, key)
	// track primary NAD
	switch {
	case ensureNetwork.IsPrimaryNetwork():
		c.primaryNADs[namespace] = key
	default:
		if c.primaryNADs[namespace] == key {
			delete(c.primaryNADs, namespace)
		}
	}

	shouldNetworkExist := true
	if c.usesLocalDynamicFiltering() {
		// IMPORTANT: nad/network caches should be updated before
		// filtering the nad. Asynchronous routines like trackers
		// depend on cache state to enqueue keys. Trackers track
		// filtering, and filter relies on the trackers. Trackers
		// enqueue keys and rely on nad/network caches for CNC enqueuing.
		// Therefore with this dual dependency we must ensure ordering is correct
		// to avoid races between the two.
		shouldFilter, err := c.filter(nad)
		if err != nil {
			return fmt.Errorf("%s: failed filtering NAD %s: %w", c.name, key, err)
		}
		if shouldFilter {
			c.setMarkedForRemovalLocked(key)
			shouldNetworkExist = false
		} else {
			c.clearDynamicRemovalStateLocked(key)
		}
	}
	if shouldNetworkExist {
		// ensure the network is associated with the NAD
		ensureNetwork.AddNADs(key)
		// reconcile the network
		c.networkController.EnsureNetwork(ensureNetwork)
	} else {
		klog.V(4).Infof("%s: Network is filtered and will not be rendered: %s", c.name, ensureNetwork.GetNetworkName())
	}
	return nil
}

// isOwnUpdate checks if an object was updated by us last, as indicated by its
// managed fields. Used to avoid reconciling an update that we made ourselves.
func isOwnUpdate(manager string, managedFields []metav1.ManagedFieldsEntry) bool {
	return util.IsLastUpdatedByManager(manager, managedFields)
}

func (c *nadController) nadNeedsUpdate(oldNAD, newNAD *nettypes.NetworkAttachmentDefinition) (needsUpdate bool) {
	if oldNAD == nil || newNAD == nil {
		return true
	}

	// don't process resync or objects that are marked for deletion
	if oldNAD.ResourceVersion == newNAD.ResourceVersion ||
		!newNAD.GetDeletionTimestamp().IsZero() {
		return false
	}

	if isOwnUpdate(c.name, newNAD.ManagedFields) {
		return false
	}

	// notifyReconcilers during sync happens after netInfo is updated, so controllers receive the latest info,
	// and it is safe to ignore own updates
	defer func() {
		if !needsUpdate { // ensure we send the NAD event to registered handlers anyway
			var key string
			var err error
			if newNAD != nil {
				key, err = cache.MetaNamespaceKeyFunc(newNAD)
				if err != nil && oldNAD != nil {
					key, err = cache.MetaNamespaceKeyFunc(oldNAD)
				}
			}
			if err != nil || len(key) == 0 {
				klog.Errorf("Failed to parse nad key during update, error: %v", err)
			} else {
				c.Lock()
				defer c.Unlock()
				c.notifyReconcilers(key)
			}
		}
	}()

	// also reconcile the network in case its route advertisements changed
	return !reflect.DeepEqual(oldNAD.Spec, newNAD.Spec) ||
		oldNAD.Annotations[types.OvnRouteAdvertisementsKey] != newNAD.Annotations[types.OvnRouteAdvertisementsKey] ||
		oldNAD.Annotations[types.OvnNetworkIDAnnotation] != newNAD.Annotations[types.OvnNetworkIDAnnotation] ||
		oldNAD.Annotations[types.OvnNetworkNameAnnotation] != newNAD.Annotations[types.OvnNetworkNameAnnotation]
}

// GetActiveNetworkForNamespace attempts to get the netInfo of a primary active network where this OVNK instance is running.
// Returns DefaultNetwork if Network Segmentation disabled or namespace does not require primary UDN.
// Returns nil if there is no active network.
// Returns InvalidPrimaryNetworkError if a network should be present but is not.
// Returns a NotFound error if the namespace is absent from the informer
// cache (not necessarily definitively deleted).
func (c *nadController) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return &util.DefaultNetInfo{}, nil
	}

	// check if required UDN label is on namespace
	ns, err := c.namespaceLister.Get(namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to get namespace %q: %w", namespace, err)
	}
	if _, exists := ns.Labels[types.RequiredUDNNamespaceLabel]; !exists {
		// UDN required label not set on namespace, assume default network
		return &util.DefaultNetInfo{}, nil
	}

	// primary UDN territory, check if our NAD controller to see if it has processed the network and if the
	// network manager has rendered the network
	network, primaryNAD := c.getActiveNetworkForNamespace(namespace)
	if network != nil && network.IsPrimaryNetwork() {
		// primary UDN network found in network controller
		copy := util.NewMutableNetInfo(network)
		copy.SetNADs(primaryNAD)
		return copy, nil
	}

	// no network exists in the network manager
	if primaryNAD != "" {
		if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
			// primary NAD exists, no network, and DUDN is enabled, treat this like the network doesn't exist
			return nil, nil
		}
		// primary NAD exists, but missing in network manager. This should never happen.
		panic(fmt.Sprintf("NAD Controller broken consistency with Network Manager for primary NAD: %s", primaryNAD))
	}

	return nil, util.NewInvalidPrimaryNetworkError(namespace)
}

func (c *nadController) GetActiveNetworkForNamespaceFast(namespace string) util.NetInfo {
	network, _ := c.getActiveNetworkForNamespace(namespace)
	return network
}

// GetPrimaryNADForNamespace returns the full namespaced key of the
// primary NAD for the given namespace, if one exists.
// Returns default network if namespace has no primary UDN or Network Segmentation is disabled.
// Returns a NotFound error if the namespace is absent from the informer
// cache (not necessarily definitively deleted).
func (c *nadController) GetPrimaryNADForNamespace(namespace string) (string, error) {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return types.DefaultNetworkName, nil
	}
	c.RLock()
	primary := c.primaryNADs[namespace]
	c.RUnlock()
	if primary != "" {
		return primary, nil
	}

	// Double-check if the namespace *requires* a primary UDN.
	ns, err := c.namespaceLister.Get(namespace)
	if err != nil {
		return "", fmt.Errorf("failed to fetch namespace %q: %w", namespace, err)
	}
	if _, exists := ns.Labels[types.RequiredUDNNamespaceLabel]; exists {
		// Namespace promises a primary UDN, but we haven't cached one yet.
		return "", util.NewInvalidPrimaryNetworkError(namespace)
	}

	// No required label: means default network only.
	return types.DefaultNetworkName, nil
}

func (c *nadController) getActiveNetworkForNamespace(namespace string) (util.NetInfo, string) {
	c.RLock()
	defer c.RUnlock()

	var network util.NetInfo
	primaryNAD := c.primaryNADs[namespace]
	switch primaryNAD {
	case "":
		// default network
		network = c.networkController.getNetwork(types.DefaultNetworkName)
		if network == nil {
			network = &util.DefaultNetInfo{}
		}
	default:
		// we have a primary network
		netName := c.nads[primaryNAD]
		if netName == "" {
			// this should never happen where we have a nad keyed in the primaryNADs
			// map, but it doesn't exist in the nads map
			panic("NAD Controller broken consistency between primary NADs and cached NADs")
		}
		network = c.networkController.getNetwork(netName)
	}

	return network, primaryNAD
}

func (c *nadController) GetNetwork(name string) util.NetInfo {
	network := c.networkController.getNetwork(name)
	if network == nil && name == types.DefaultNetworkName {
		return &util.DefaultNetInfo{}
	}
	return network
}

func (c *nadController) GetNetInfoForNADKey(nadKey string) util.NetInfo {
	c.RLock()
	networkName := c.nads[nadKey]
	c.RUnlock()
	if networkName == "" {
		return nil
	}
	network := c.networkController.getNetwork(networkName)
	if network == nil && networkName == types.DefaultNetworkName {
		return &util.DefaultNetInfo{}
	}
	if network == nil {
		return nil
	}
	// Return a copy so callers can safely read fields without depending on controller locks.
	return util.NewMutableNetInfo(network)
}

func (c *nadController) GetNetworkNameForNADKey(nadKey string) string {
	c.RLock()
	defer c.RUnlock()
	return c.nads[nadKey]
}

func (c *nadController) GetNADKeysForNetwork(networkName string) []string {
	if networkName == "" {
		return nil
	}
	c.RLock()
	defer c.RUnlock()
	nadSet := c.nadsByNetwork[networkName]
	if len(nadSet) == 0 {
		return nil
	}
	return nadSet.UnsortedList()
}

func (c *nadController) GetActiveNetworkNamespaces(networkName string) ([]string, error) {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return []string{"default"}, nil
	}
	namespaces := make([]string, 0)
	c.RLock()
	defer c.RUnlock()
	for namespaceName, primaryNAD := range c.primaryNADs {
		nadNetworkName := c.nads[primaryNAD]
		if nadNetworkName != networkName {
			continue
		}
		namespaces = append(namespaces, namespaceName)
	}
	return namespaces, nil
}

// DoWithLock iterates over all role primary user defined networks and executes the given fn with each network as input.
// An error will not block execution and instead all errors will be aggregated and returned when all networks are processed.
func (c *nadController) DoWithLock(f func(network util.NetInfo) error) error {
	if !util.IsNetworkSegmentationSupportEnabled() {
		defaultNetwork := &util.DefaultNetInfo{}
		return f(defaultNetwork)
	}
	c.RLock()
	defer c.RUnlock()

	var errs []error
	for _, primaryNAD := range c.primaryNADs {
		if primaryNAD == "" {
			continue
		}
		netName := c.nads[primaryNAD]
		if netName == "" {
			// this should never happen where we have a nad keyed in the primaryNADs
			// map, but it doesn't exist in the nads map
			panic("NAD Controller broken consistency between primary NADs and cached NADs")
		}
		network := c.networkController.getNetwork(netName)
		if network == nil {
			// network may not always be rendered with Dynamic UDN
			// otherwise this should never happen
			if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
				continue
			}
			panic("NAD Controller broken consistency between primary NADs and network controller cache")
		}
		n := util.NewMutableNetInfo(network)
		// update the returned netInfo copy to only have the primary NAD for this namespace
		n.SetNADs(primaryNAD)
		if err := f(n); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// handleNetworkAnnotations assigns or reads info from the NAD annotations.
// We store network ID and tunnel keys in the AND annotation. This function
// finds out what these values should be for a new network and
// sets it on 'new'. If not annotated, it means it is still to be allocated.
// If this is not the NAD controller running in cluster manager, then we don't
// do anything as we are expected to wait until it happens.
// If this is the NAD controller running in cluster manager then a new ID
// is allocated and annotated on the NAD. The NAD controller running in
// cluster manager also releases here the network ID of a network that is being deleted.
func (c *nadController) handleNetworkAnnotations(new util.MutableNetInfo, nad *nettypes.NetworkAttachmentDefinition, nadKey, previousNetworkName string) (err error) {
	newNetworkName := ""
	if new != nil {
		newNetworkName = new.GetNetworkName()
	}
	if previousNetworkName != "" && previousNetworkName != types.DefaultNetworkName &&
		(new == nil || previousNetworkName != newNetworkName) {
		if c.networkReferencedLocked(previousNetworkName, nadKey) {
			klog.V(5).Infof("%s: NADs still reference network %s; skipping ID release", c.name, previousNetworkName)
		} else {
			c.networkIDAllocator.ReleaseID(previousNetworkName)
			if c.isClusterManagerMode() {
				c.tunnelKeysAllocator.ReleaseKeys(previousNetworkName)
			}
		}
	}

	if new != nil && new.IsDefault() {
		return nil
	}

	id := types.InvalidID

	// check if in cache first
	if new != nil {
		id = c.networkIDAllocator.GetID(new.GetNetworkName())
		if id != types.InvalidID {
			klog.V(5).Infof("Previously cached network ID %d found for network: %s", id, new.GetNetworkName())
		}
	}
	if nad != nil && id == types.InvalidID {
		// check what ID is currently annotated
		if nad.Annotations[types.OvnNetworkIDAnnotation] != "" {
			annotated := nad.Annotations[types.OvnNetworkIDAnnotation]
			id, err = strconv.Atoi(annotated)
			if err != nil {
				return fmt.Errorf("failed to parse annotated network ID: %w", err)
			}
			klog.V(5).Infof("Previously annotated network ID %d found for NAD: %s/%s", id, nad.Namespace, nad.Name)
		}
	}

	tunnelKeys := []int{}
	// check what tunnel keys are currently annotated
	if nad != nil && nad.Annotations[types.OvnNetworkTunnelKeysAnnotation] != "" {
		tunnelKeys, err = util.ParseTunnelKeysAnnotation(nad.Annotations[types.OvnNetworkTunnelKeysAnnotation])
		if err != nil {
			return fmt.Errorf("failed to parse annotated tunnel keys: %w", err)
		}
	}

	// nothing to allocate, delete case
	if new == nil {
		return nil
	}
	name := new.GetNetworkName()

	// a network ID was annotated, check if it is free to use or stale
	if id != types.InvalidID {
		err = c.networkIDAllocator.ReserveID(name, id)
		if err != nil {
			// already reserved for a different network, allocate a new id
			id = types.InvalidID
		}
	}

	// this is not the cluster manager nad controller, and we are not allocating
	// so just return what ids we already found
	if !c.isClusterManagerMode() {
		new.SetNetworkID(id)
		new.SetTunnelKeys(tunnelKeys)
		return nil
	}

	// tunnel key annotation doesn't need the same check ^ because it is initialized outside the
	// nad controller and has already assured that all annotated tunnel keys are reserved.

	// we are about to allocate resources, so prepare a cleanup function
	// in case of error to release them.
	var allocatedNetworkID, allocatedTunnelKeys bool
	defer func() {
		if err != nil {
			if allocatedNetworkID {
				c.networkIDAllocator.ReleaseID(name)
			}
			if allocatedTunnelKeys {
				c.tunnelKeysAllocator.ReleaseKeys(name)
			}
		}
	}()
	// we don't have an ID, allocate a new one
	if id == types.InvalidID {
		id, err = c.networkIDAllocator.AllocateID(name)
		if err != nil {
			return fmt.Errorf("failed to allocate network ID: %w", err)
		}
		allocatedNetworkID = true

		// check if there is still a network running with that ID in the process
		// of being stopped
		other := c.networkController.getRunningNetwork(id)
		if other != "" && c.networkController.getNetwork(other) == nil {
			return fmt.Errorf("found other network %s being stopped with allocated ID %d, will retry", other, id)
		}
	}

	// allocate tunnel keys
	if len(tunnelKeys) != getNumberOfTunnelKeys(new) {
		if len(tunnelKeys) > 0 {
			// this should never happen, but if it does
			// AllocateKeys will add missing keys, when len(tunnelKeys) < getNumberOfTunnelKeys(new)
			// and will return an error when len(tunnelKeys) > getNumberOfTunnelKeys(new)
			// log an error for visibility
			klog.Errorf("Unexpected number of tunnel keys annotated on NAD for network %s, expected %d got %d",
				name, getNumberOfTunnelKeys(new), len(tunnelKeys))
		}
		tunnelKeys, err = c.tunnelKeysAllocator.AllocateKeys(name, id, getNumberOfTunnelKeys(new))
		if err != nil {
			return fmt.Errorf("failed to allocate tunnel keys: %w", err)
		}
		allocatedTunnelKeys = true
	}

	// set and annotate the network ID
	tunnelKeyAnno, err := util.FormatTunnelKeysAnnotation(tunnelKeys)
	if err != nil {
		return fmt.Errorf("failed to format tunnel keys annotation: %w", err)
	}
	annotations := map[string]string{
		types.OvnNetworkNameAnnotation:       name,
		types.OvnNetworkIDAnnotation:         strconv.Itoa(id),
		types.OvnNetworkTunnelKeysAnnotation: tunnelKeyAnno,
	}
	if nad.Annotations[types.OvnNetworkNameAnnotation] == annotations[types.OvnNetworkNameAnnotation] {
		delete(annotations, types.OvnNetworkNameAnnotation)
	}
	if nad.Annotations[types.OvnNetworkIDAnnotation] == annotations[types.OvnNetworkIDAnnotation] {
		delete(annotations, types.OvnNetworkIDAnnotation)
	}
	if nad.Annotations[types.OvnNetworkTunnelKeysAnnotation] == annotations[types.OvnNetworkTunnelKeysAnnotation] {
		delete(annotations, types.OvnNetworkTunnelKeysAnnotation)
	}
	if len(annotations) == 0 {
		new.SetNetworkID(id)
		new.SetTunnelKeys(tunnelKeys)
		return nil
	}

	k := kube.KubeOVN{
		NADClient: c.nadClient,
	}

	err = k.SetAnnotationsOnNAD(
		nad.Namespace,
		nad.Name,
		annotations,
		c.name,
	)
	if err != nil {
		return fmt.Errorf("failed to annotate network ID and/or tunnel keys on NAD: %w", err)
	}
	new.SetNetworkID(id)
	new.SetTunnelKeys(tunnelKeys)

	return nil
}

// networkReferencedLocked reports whether any NADs still reference a network.
// When excludeNAD is set, it is ignored in the reference check.
// Must be called with nadController locked.
func (c *nadController) networkReferencedLocked(networkName, excludeNAD string) bool {
	if networkName == "" || networkName == types.DefaultNetworkName {
		return false
	}
	nadSet := c.nadsByNetwork[networkName]
	if len(nadSet) == 0 {
		return false
	}
	if excludeNAD == "" {
		return len(nadSet) > 0
	}
	if nadSet.Has(excludeNAD) {
		return len(nadSet) > 1
	}
	return len(nadSet) > 0
}

func (c *nadController) getNetworkIDFromNode(nadNetwork util.NetInfo) (int, error) {
	// check if the node has a legacy ID
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return types.InvalidID, fmt.Errorf("failed to list nodes: %w", err)
	}
	// sort to make retrieval semi-consistent across nodes
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].CreationTimestamp.Before(&nodes[j].CreationTimestamp)
	})
	netName := nadNetwork.GetNetworkName()
	// Find from node annotations
	for _, node := range nodes {
		idMap, err := util.GetNodeNetworkIDsAnnotationNetworkIDs(node)
		if err != nil {
			continue
		}
		if v, ok := idMap[netName]; ok && v != types.InvalidID {
			return v, nil
		}
	}
	return types.InvalidID, nil
}

func (c *nadController) GetActiveNetwork(network string) util.NetInfo {
	c.RLock()
	defer c.RUnlock()
	state := c.networkController.getNetworkState(network)
	if state == nil {
		return nil
	}
	return state.controller
}

func (c *nadController) GetNetworkByID(id int) util.NetInfo {
	if id == types.InvalidID {
		return nil
	}
	netInfo := c.networkController.GetNetworkByID(id)
	if netInfo != nil {
		return netInfo
	}
	c.RLock()
	networkName := c.networkNameForIDLocked(id)
	nadSet := c.nadsByNetwork[networkName]
	nadKeys := make([]string, 0, len(nadSet))
	for key := range nadSet {
		nadKeys = append(nadKeys, key)
	}
	c.RUnlock()
	// Handles the case where there is a cache miss. This is needed for filtered networks with
	// Dynamic UDN as there will be no network manager cache entry.
	// TODO (trozet): this is slow. We should optimize this by potentially storing the cache
	// of netInfos even for filtered entries.
	for _, key := range nadKeys {
		namespace, name, err := cache.SplitMetaNamespaceKey(key)
		if err != nil {
			continue
		}
		nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
		if err != nil {
			continue
		}
		nadNetwork, err := util.ParseNADInfo(nad)
		if err != nil || nadNetwork == nil {
			continue
		}
		if nadNetwork.GetNetworkID() == id {
			return nadNetwork
		}
	}
	return nil
}

func (c *nadController) isClusterManagerMode() bool {
	return c.tunnelKeysAllocator != nil
}

func getNumberOfTunnelKeys(netInfo util.NetInfo) int {
	if netInfo.IsDefault() {
		// default network does not need tunnel keys allocation because it always uses network ID 0.
		return 0
	}
	// Layer3, Secondary Layer2 and Localnet topologies need only 1 tunnel key for now that is derived from the network ID
	// and is limited by the MaxNetworks. Don't annotate any tunnel keys in that case until we decide to
	// increase the MaxNetworks.
	if netInfo.TopologyType() != types.Layer2Topology || !netInfo.IsPrimaryNetwork() {
		return 0
	}
	// Primary Layer2 UDNs need 2 tunnel keys: one for the layer2 switch and one for the transit router
	return 2
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkconnect

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned"
	networkconnectlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/listers/clusternetworkconnect/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var (
	cudnGVK = userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork")
	udnGVK  = userdefinednetworkv1.SchemeGroupVersion.WithKind("UserDefinedNetwork")
)

// clusterNetworkConnectState is the cache that keeps the state of a single
// cluster network connect in the cluster with name being unique
type clusterNetworkConnectState struct {
	// name of the cluster network connect (unique across cluster)
	name string
	// allocator for this CNC's subnet allocation
	allocator HybridConnectSubnetAllocator
	// connectSubnets are used for overlap check across multiple CNCs
	connectSubnets []*net.IPNet
	// map of NADs currently selected by this CNC's network selectors
	// {value: NAD namespace/name key}
	// this cache is mainly required to be able to detect when a
	// NAD is started or stopped matching the CNC. That way, we don't
	// need to call reconcile on all CNCs and only the specific ones
	// that select this NAD.
	// Specially, when a NAD is deleted, we need to release the subnets allocated for
	// matching CNCs and since nadKey is the only information we get
	// since NAD object itself will be nil since its deleted, we need
	// to keep track of NAD keys.
	selectedNADs sets.Set[string]
	// set of networks currently selected by this CNC's network selectors
	// {value: network owner key like "layer3_1" or "layer2_2"}
	// Owner keys are computed from topology type (layer3 or layer2) and network ID, enabling subnet release
	// without needing to re-discover network info.
	selectedNetworks sets.Set[string]
	// tunnelID for this CNC's connect router
	tunnelID int
}

type Controller struct {
	// wf is the watch factory for accessing informers
	wf *factory.WatchFactory
	// listers
	cncLister       networkconnectlisters.ClusterNetworkConnectLister
	namespaceLister corelisters.NamespaceLister
	nadLister       nadlisters.NetworkAttachmentDefinitionLister
	//clientset
	cncClient networkconnectclientset.Interface
	// Controller for managing cluster-network-connect events
	cncController controllerutil.Controller
	// Controller for managing Namespace events
	namespaceController controllerutil.Controller
	networkManager      networkmanager.Interface
	// Reconciler for managing NetworkAttachmentDefinition events
	nadReconciler   networkmanager.NADReconciler
	nadReconcilerID uint64

	// Single global lock protecting all controller state
	// We can improve this later by using a more fine-grained lock based on performance testing
	sync.RWMutex
	// holds the state for each CNC keyed by CNC name
	cncCache            map[string]*clusterNetworkConnectState
	tunnelKeysAllocator *id.TunnelKeysAllocator
}

func NewController(
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
	networkManager networkmanager.Interface,
	tunnelKeysAllocator *id.TunnelKeysAllocator,
) *Controller {
	cncLister := wf.ClusterNetworkConnectInformer().Lister()
	nadLister := wf.NADInformer().Lister()
	namespaceLister := wf.NamespaceInformer().Lister()
	c := &Controller{
		wf:                  wf,
		cncClient:           ovnClient.NetworkConnectClient,
		cncLister:           cncLister,
		nadLister:           nadLister,
		namespaceLister:     namespaceLister,
		networkManager:      networkManager,
		cncCache:            make(map[string]*clusterNetworkConnectState),
		tunnelKeysAllocator: tunnelKeysAllocator,
	}

	cncCfg := &controllerutil.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.ClusterNetworkConnectInformer().Informer(),
		Lister:         cncLister.List,
		Reconcile:      c.reconcileClusterNetworkConnect,
		ObjNeedsUpdate: cncNeedsUpdate,
		Threadiness:    1,
	}
	c.cncController = controllerutil.NewController(
		"clustermanager-network-connect-controller",
		cncCfg,
	)

	nadReconcilerCfg := &controllerutil.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.reconcileNAD,
		Threadiness: 1,
		MaxAttempts: controllerutil.InfiniteAttempts,
	}
	c.nadReconciler = controllerutil.NewReconciler(
		"clustermanager-network-connect-network-attachment-definition-controller",
		nadReconcilerCfg,
	)

	namespaceCfg := &controllerutil.ControllerConfig[corev1.Namespace]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.NamespaceInformer().Informer(),
		Lister:         namespaceLister.List,
		Reconcile:      c.reconcileNamespace,
		ObjNeedsUpdate: namespaceNeedsUpdate,
		Threadiness:    1,
	}
	c.namespaceController = controllerutil.NewController(
		"clustermanager-network-connect-namespace-controller",
		namespaceCfg,
	)

	return c
}

func (c *Controller) Start() (err error) {
	defer func() {
		if err != nil {
			c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID)
		} else {
			klog.Infof("Cluster manager network connect controllers started")
		}
	}()

	c.nadReconcilerID = c.networkManager.RegisterNADReconciler(c.nadReconciler)
	return controllerutil.StartWithInitialSync(
		c.initialSync,
		c.cncController,
		c.nadReconciler,
		c.namespaceController,
	)
}

// initialSync restores allocator state from existing CNC annotations at startup.
// This is called after informers are synced but before workers start processing.
// It ensures that subnets already allocated (stored in annotations) are not re-allocated.
func (c *Controller) initialSync() error {
	c.Lock()
	defer c.Unlock()

	cncs, err := c.cncLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CNCs during initial sync: %v", err)
	}

	for _, cnc := range cncs {
		// Parse existing subnet annotation
		allocatedSubnets, err := util.ParseNetworkConnectSubnetAnnotation(cnc)
		if err != nil {
			klog.Warningf("Failed to parse subnet annotation for CNC %s: %v, skipping", cnc.Name, err)
			continue
		}

		// Parse existing tunnel key annotation
		tunnelID, err := util.ParseNetworkConnectTunnelKeyAnnotation(cnc)
		if err != nil {
			klog.Warningf("Failed to parse tunnel key annotation for CNC %s: %v, skipping", cnc.Name, err)
			continue
		}

		// Initialize CNC state in cache
		cncState := &clusterNetworkConnectState{
			name: cnc.Name,
			// NOTE: We intentionally don't restore selectedNADs as its not strictly needed.
			// Why this is okay:
			// selectedNADs tracks NAD keys (e.g., "namespace/name") which aren't stored in
			// the annotation - the annotation only has owner keys.
			// During the first CNC reconcile (which happens right after initialSync since the
			// Add events are queued), reconcileClusterNetworkConnect runs discoverSelectedNetworks which:
			// Iterates through NADs matching the selectors
			// Returns allMatchingNADKeys
			// Then updates the cache: cncState.selectedNADs = allMatchingNADKeys
			// The ordering is safe because StartWithInitialSync ensures:
			// Informer caches are synced (all NADs visible)
			// initialSync runs (allocator state restored)
			// THEN workers start processing the queue (CNC reconciles happen)
			// Edge case: If a NAD update comes in during this window, mustProcessCNCForNAD might
			// see wasSelected=false (empty cache) and isSelected=true → trigger an extra reconcile.
			// But that's benign - just an extra no-op reconcile.
			selectedNADs:     sets.New[string](),
			selectedNetworks: sets.New[string](),
			tunnelID:         tunnelID,
		}
		connectSubnetAllocator, err := NewHybridConnectSubnetAllocator(cnc.Spec.ConnectSubnets, cnc.Name)
		if err != nil {
			return fmt.Errorf("failed to initialize subnet allocator for CNC %s: %w", cnc.Name, err)
		}
		connectSubnets := []*net.IPNet{}
		for _, cs := range cnc.Spec.ConnectSubnets {
			// ignore error, this was already parsed for NewHybridConnectSubnetAllocator
			_, cidr, _ := net.ParseCIDR(string(cs.CIDR))
			connectSubnets = append(connectSubnets, cidr)
		}
		cncState.connectSubnets = connectSubnets
		cncState.allocator = connectSubnetAllocator
		c.cncCache[cnc.Name] = cncState

		// Restore tunnel key in allocator if present
		if tunnelID > 0 {
			// Reserve tunnel key in the allocator so it won't be re-allocated
			// We already reserve the key from cluster manager sync in initTunnelKeysAllocator,
			// but no harm in doing it again here for completeness.
			if err := c.tunnelKeysAllocator.ReserveKeys(cnc.Name, []int{tunnelID}); err != nil {
				klog.Warningf("Failed to restore tunnel key %d for CNC %s: %v", tunnelID, cnc.Name, err)
			} else {
				klog.V(4).Infof("Restored tunnel key %d for CNC %s", tunnelID, cnc.Name)
			}
		}

		// Restore subnets if present
		if len(allocatedSubnets) > 0 {
			if err := cncState.allocator.MarkAllocatedSubnets(allocatedSubnets); err != nil {
				klog.Warningf("Failed to restore subnets for CNC %s: %v", cnc.Name, err)
				continue
			}

			// Populate selectedNetworks from the restored allocations
			for owner := range allocatedSubnets {
				cncState.selectedNetworks.Insert(owner)
			}
			klog.V(4).Infof("Restored %d subnet allocations for CNC %s", len(allocatedSubnets), cnc.Name)
		}
	}

	klog.Infof("Initial sync completed: restored state for %d CNCs", len(cncs))
	return nil
}

func (c *Controller) Stop() {
	if c.nadReconcilerID != 0 {
		c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID)
	}
	controllerutil.Stop(
		c.cncController,
		c.nadReconciler,
		c.namespaceController,
	)
	c.nadReconciler = nil
	c.nadReconcilerID = 0
	klog.Infof("Cluster manager network connect controllers stopped")
}

func cncNeedsUpdate(oldObj, newObj *networkconnectv1.ClusterNetworkConnect) bool {
	// Case 1: CNC is being deleted
	// Case 2: CNC is being created
	if oldObj == nil || newObj == nil {
		return true
	}
	// Case 3: CNC is being updated
	// Only trigger updates when the Spec.NetworkSelectors changes
	// We only need to check for selector changes
	// and don't need to react on connectivity enabled field changes
	// from cluster manager.
	// connectSubnet is immutable so that can't change after creation.
	return !reflect.DeepEqual(oldObj.Spec.NetworkSelectors, newObj.Spec.NetworkSelectors)
}

func (c *Controller) reconcileNAD(_ context.Context, key string) error {
	// Use single global lock following ANP controller pattern
	c.Lock()
	defer c.Unlock()

	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split NAD key %s: %w", key, err)
	}

	klog.V(5).Infof("reconcileNAD %s", key)
	defer func() {
		klog.Infof("reconcileNAD %s took %v", key, time.Since(startTime))
	}()

	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get NAD %s: %w", key, err)
	}

	nadSupported := func(nad *nadv1.NetworkAttachmentDefinition) bool {
		if nad == nil {
			return true
		}
		// we don't support direct NADs anymore. CNC is only supported for CUDNs and UDNs
		controller := metav1.GetControllerOfNoCopy(nad)
		isCUDN := controller != nil && controller.Kind == cudnGVK.Kind && controller.APIVersion == cudnGVK.GroupVersion().String()
		isUDN := controller != nil && controller.Kind == udnGVK.Kind && controller.APIVersion == udnGVK.GroupVersion().String()
		if !isCUDN && !isUDN {
			return false
		}
		network := c.networkManager.GetNetInfoForNADKey(key)
		if network == nil {
			// this should never happen, because we get NAD events from networkManager
			klog.Warningf("CNC controller failed to get network info for NAD %s", key)
			return true
		}
		if network.IsPrimaryNetwork() {
			// only layer3 and layer2 topology are supported
			// but since primary network is always layer3 or layer2,
			// we can ignored the need to check the topology
			return true
		}
		return false // we don't support secondary networks, so we can ignore it
	}
	// ignore if we don't support this NAD
	if !nadSupported(nad) {
		return nil
	}

	existingCNCs, err := c.cncLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CNCs: %w", err)
	}

	// Process each CNC to check if this NAD's matching state changed
	for _, cnc := range existingCNCs {
		if c.mustProcessCNCForNAD(nad, cnc, key) {
			c.cncController.Reconcile(cnc.Name)
		}
	}
	return nil
}

// mustProcessCNCForNAD checks if:
// 1. the provided NAD previously matched the given CNC and now it stopped matching OR
// 2. the provided NAD currently matches the given CNC and previously it didn't match
// 3. the provided NAD previously matched the given CNC and now it continues to match it
// Returns true if any of the above conditions are true.
// This function is READ-ONLY and does not update the cache.
// NOTE: Caller must hold the global lock.
func (c *Controller) mustProcessCNCForNAD(nad *nadv1.NetworkAttachmentDefinition, cnc *networkconnectv1.ClusterNetworkConnect, nadKey string) bool {
	cncState, cncExists := c.cncCache[cnc.Name]

	// If CNC state doesn't exist yet, we don't know the previous state
	// so we assume no change (cache will be populated during CNC reconciliation)
	if !cncExists {
		klog.V(5).Infof("CNC %s state not found in cache, assuming no matching state change for NAD %s", cnc.Name, nadKey)
		return false
	}

	// Check if NAD used to be selected (using cache)
	wasSelected := cncState.selectedNADs.Has(nadKey)

	// Determine if NAD started to be selected now
	isSelected := false
	if nad != nil {
		nadLabels := labels.Set(nad.Labels)
	selectorLoop: // break out of the loop if we find a match
		for _, networkSelector := range cnc.Spec.NetworkSelectors {
			switch networkSelector.NetworkSelectionType {
			case apitypes.ClusterUserDefinedNetworks:
				cudnSelector, err := metav1.LabelSelectorAsSelector(&networkSelector.ClusterUserDefinedNetworkSelector.NetworkSelector)
				if err != nil {
					klog.Errorf("Failed to create selector for CNC %s: %v", cnc.Name, err)
					continue
				}
				// labels on CUDN are copied to the corresponding NADs, so we can use the same selector
				if cudnSelector.Matches(nadLabels) {
					isSelected = true
					break selectorLoop
				}
			case apitypes.PrimaryUserDefinedNetworks:
				namespaceSelector, err := metav1.LabelSelectorAsSelector(&networkSelector.PrimaryUserDefinedNetworkSelector.NamespaceSelector)
				if err != nil {
					klog.Errorf("Failed to create selector for CNC %s: %v", cnc.Name, err)
					continue
				}
				namespaces, err := c.namespaceLister.List(namespaceSelector)
				if err != nil {
					klog.Errorf("Failed to list namespaces for CNC %s: %v", cnc.Name, err)
					continue
				}
				for _, namespace := range namespaces {
					nsPrimaryNetwork, err := c.networkManager.GetActiveNetworkForNamespace(namespace.Name)
					if err != nil {
						if util.IsInvalidPrimaryNetworkError(err) || apierrors.IsNotFound(err) {
							continue
						}
						klog.Errorf("Failed to get active network for namespace %s: %v", namespace.Name, err)
						continue
					}
					if nsPrimaryNetwork == nil {
						continue
					}
					networkName := c.networkManager.GetNetworkNameForNADKey(nadKey)
					if networkName != "" && networkName == nsPrimaryNetwork.GetNetworkName() {
						isSelected = true
						break selectorLoop
					}
				}
			default:
				klog.Errorf("Unsupported network selection type %s for CNC %s", networkSelector.NetworkSelectionType, cnc.Name)
				continue
			}
		}
	}

	// Log state changes
	stateChanged := wasSelected != isSelected
	if stateChanged {
		if isSelected && !wasSelected {
			klog.V(4).Infof("NAD %s started to match CNC %s, requeuing...", nadKey, cnc.Name)
		} else if !isSelected && wasSelected {
			klog.V(4).Infof("NAD %s used to match CNC %s, requeuing...", nadKey, cnc.Name)
		}
	}

	// reason we need to also process if the NAD simply continues to match is because
	// NAD could have had its network-id annotation update which we use in CNC reconciliation to
	// generate the subnet for the connect router corresponding to this CNC.
	return wasSelected || isSelected
}

func namespaceNeedsUpdate(oldObj, newObj *corev1.Namespace) bool {
	// When searching for primary UDNs matching CNC selector, we list namespaces.
	// Very rarely a NAD event can be handled, while namespace informer doesn't have
	// that namespace yet. That is why we may need to reconcile on namespace creation.
	if newObj == nil {
		// we don't care about deletes for namespace, because NAD delete events are handled independently
		return false
	}
	namespaceSupported := func(namespace *corev1.Namespace) bool {
		if namespace == nil {
			return false
		}
		// we only support primary UDNs in namespaces that have the required label
		_, ok := namespace.Labels[ovntypes.RequiredUDNNamespaceLabel]
		return ok
	}
	if oldObj == nil {
		// reconcile supported namespaces on create
		return namespaceSupported(newObj)
	}
	if !namespaceSupported(oldObj) && !namespaceSupported(newObj) {
		return false
	}
	// Namespace is being updated (we only care about labels changes)
	oldNamespaceLabels := labels.Set(oldObj.Labels)
	newNamespaceLabels := labels.Set(newObj.Labels)
	labelsChanged := !labels.Equals(oldNamespaceLabels, newNamespaceLabels)
	return labelsChanged
}

func (c *Controller) reconcileNamespace(_ context.Context, key string) error {
	c.Lock()
	defer c.Unlock()

	startTime := time.Now()
	klog.V(5).Infof("reconcileNamespace %s", key)
	defer func() {
		klog.Infof("reconcileNamespace %s took %v", key, time.Since(startTime))
	}()

	namespace, err := c.namespaceLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Namespace deleted - nothing to do since NAD controller
			// will handle any NAD deletions in this namespace
			// which will trigger a CNC reconcile for any CNCs selecting
			// this namespace.
			return nil
		}
		return fmt.Errorf("failed to get namespace %s: %w", key, err)
	}

	primaryNAD, _, err := getPrimaryNADForNamespace(c.networkManager, key)
	if err != nil {
		klog.Errorf("Failed to get primary NAD for namespace %s: %v", key, err)
		// best effort, usually if a NAD then gets created/deleted in this namespace,
		// we will get a NAD event anyways
		return nil
	}
	if primaryNAD == "" {
		// no primary UDN in this namespace, so we don't need to do anything
		return nil
	}

	existingCNCs, err := c.cncLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CNCs: %w", err)
	}
	for _, cnc := range existingCNCs {
		if c.mustProcessCNCForNamespace(cnc, namespace, primaryNAD) {
			c.cncController.Reconcile(cnc.Name)
		}
	}
	return nil
}

// mustProcessCNCForNamespace determines if:
// 1. the given namespace was previously selected by the given CNC and now it stopped matching OR
// 2. if its currently selected by the given CNC and previously it didn't match
// returns true if either of the above conditions are true
func (c *Controller) mustProcessCNCForNamespace(cnc *networkconnectv1.ClusterNetworkConnect, namespace *corev1.Namespace, primaryNAD string) bool {
	cncState, cncExists := c.cncCache[cnc.Name]

	// If CNC state doesn't exist yet, we don't know the previous state
	// so we assume no change (cache will be populated during CNC reconciliation)
	if !cncExists {
		klog.V(5).Infof("CNC %s state not found in cache, assuming no matching state change for namespace %s", cnc.Name, namespace.Name)
		return false
	}
	wasSelected := cncState.selectedNADs.Has(primaryNAD)
	isSelected := false

selectorLoop:
	for _, networkSelector := range cnc.Spec.NetworkSelectors {
		switch networkSelector.NetworkSelectionType {
		case apitypes.PrimaryUserDefinedNetworks:
			namespaceSelector, err := metav1.LabelSelectorAsSelector(
				&networkSelector.PrimaryUserDefinedNetworkSelector.NamespaceSelector)
			if err != nil {
				klog.Errorf("Failed to create selector for CNC %s: %v", cnc.Name, err)
				continue
			}
			if namespaceSelector.Matches(labels.Set(namespace.Labels)) {
				isSelected = true
				break selectorLoop
			}
		}
	}
	stateChanged := wasSelected != isSelected
	if stateChanged {
		if isSelected && !wasSelected {
			klog.V(4).Infof("Namespace %s started to match CNC %s, requeuing...", namespace.Name, cnc.Name)
		} else if !isSelected && wasSelected {
			klog.V(4).Infof("Namespace %s used to match CNC %s, requeuing...", namespace.Name, cnc.Name)
		}
	}
	// If state didn't change, that is if this namespace was previously selected
	// and continues to be selected, it means it was some other label updates to
	// namespace that we don't care about. State changes are the only ones we care about.
	return stateChanged
}

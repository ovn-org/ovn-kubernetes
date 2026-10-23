// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package addresssetmanager

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// podSelectorAddressSet stores address set for modifications and used selectors that define this address set.
type podSelectorAddressSet struct {
	// backRefs is a map of objects that use this address set.
	// keys must be unique for all possible users, e.g. for NetworkPolicy use (np *networkPolicy) getKeyWithKind().
	backRefs map[string]bool

	podSelector       labels.Selector
	namespaceSelector labels.Selector
	// namespace is used when namespaceSelector is nil to set static namespace
	namespace string
	// nodeSelector decides which nodes' pods should be added to the address set; nil means all nodes
	nodeSelector labels.Selector

	addressSet addressset.AddressSet

	// selectedNamespaces is a cache for namespaces that were selected by this address set during the last reconciliation
	// used to optimize events processing.
	selectedNamespaces *selectedNamespaces
	// selectedNodes is a cache for nodes that were selected by this address set during the last reconciliation
	// used to optimize node event processing. Only set when nodeSelector is non-nil.
	selectedNodes sets.Set[string]

	// network-specific fields
	controllerName string
	netInfo        util.NetInfo

	// legacyNetpolMode makes nil and empty PodSelectors behave differently (it shouldn't be the case,
	// but this is a legacy behaviour that customers rely on).
	// when set to true hostNetwork pods aren't selected,
	// and config.Kubernetes.HostNetworkNamespace address set IPs will be included when that namespace is matched and
	// podSelector is empty.
	legacyNetpolMode bool
}

const (
	ClusterNodeIPsAddrSetName                = "node-ips"
	ClusterNodeIPsEgressIPBackRef            = "egressip"
	ClusterNodeIPsEgressServiceBackRef       = "egressservice"
	ClusterNodeIPsRouteAdvertisementsBackRef = "route-advertisements"
)

// AddressSetManager manages shared address sets with pod IPs based on provided pod and namespace selectors.
// It shared across network controllers.
type AddressSetManager struct {
	name     string
	nbClient libovsdbclient.Client

	// address set factory ip modes only affect which IPs are getting selected for the operations
	// different networks may have different setups, so we need all combinations
	addressSetFactoryV4        addressset.AddressSetFactory
	addressSetFactoryV6        addressset.AddressSetFactory
	addressSetFactoryDualstack addressset.AddressSetFactory

	// addressSets stores all currently used address sets.
	addressSets *syncmap.SyncMap[*podSelectorAddressSet]

	podLister       listers.PodLister
	namespaceLister listers.NamespaceLister
	nodeLister      listers.NodeLister

	podController        controller.Controller
	nsController         controller.Controller
	nodeController       controller.Controller
	addressSetReconciler controller.Reconciler

	// All network controllers are getting this function from the same networkmanager, so we can share it
	getNetworkNameForNADKey func(nadKey string) string

	// hostNetworkNamespaceExists, hostNetworkNamespaceIPsPerNode and hostNetworkSelectingAddrSets are protected by the same lock.
	// can only be taken after the addressSets key lock and never vice versa to avoid deadlocks.
	hostNetworkNamespaceLock   sync.RWMutex
	hostNetworkNamespaceExists bool
	// local cache of HostNetworkNamespace address set IPs
	hostNetworkNamespaceIPsPerNode map[string][]string
	// local cache of address sets that select HostNetworkNamespace
	hostNetworkSelectingAddrSets sets.Set[string]

	clusterNodeIPsLock     sync.RWMutex
	clusterNodeIPsBackRefs sets.Set[string]
}

func NewAddressSetManager(podInformer coreinformers.PodInformer, namespaceInformer coreinformers.NamespaceInformer,
	nodeInformer coreinformers.NodeInformer, nbClient libovsdbclient.Client, getNetworkNameForNADKey func(nadKey string) string) *AddressSetManager {
	m := &AddressSetManager{
		name:                           "pod-selector-address-set-manager",
		nbClient:                       nbClient,
		addressSetFactoryV4:            addressset.NewOvnAddressSetFactory(nbClient, true, false),
		addressSetFactoryV6:            addressset.NewOvnAddressSetFactory(nbClient, false, true),
		addressSetFactoryDualstack:     addressset.NewOvnAddressSetFactory(nbClient, true, true),
		addressSets:                    syncmap.NewSyncMap[*podSelectorAddressSet](),
		podLister:                      podInformer.Lister(),
		namespaceLister:                namespaceInformer.Lister(),
		nodeLister:                     nodeInformer.Lister(),
		getNetworkNameForNADKey:        getNetworkNameForNADKey,
		hostNetworkSelectingAddrSets:   sets.New[string](),
		hostNetworkNamespaceIPsPerNode: make(map[string][]string),
		clusterNodeIPsBackRefs:         sets.New[string](),
	}
	podCfg := &controller.ControllerConfig[corev1.Pod]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      m.reconcilePod,
		ObjNeedsUpdate: m.podNeedUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       podInformer.Informer(),
		Lister:         podInformer.Lister().List,
	}
	m.podController = controller.NewController[corev1.Pod](m.name+"-pod", podCfg)

	nsCfg := &controller.ControllerConfig[corev1.Namespace]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      m.reconcileNamespace,
		ObjNeedsUpdate: m.nsNeedUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       namespaceInformer.Informer(),
		Lister:         namespaceInformer.Lister().List,
	}
	m.nsController = controller.NewController[corev1.Namespace](m.name+"-namespace", nsCfg)

	nodeCfg := &controller.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      m.reconcileNode,
		ObjNeedsUpdate: m.nodeNeedUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       nodeInformer.Informer(),
		Lister:         nodeInformer.Lister().List,
	}
	m.nodeController = controller.NewController[corev1.Node](m.name+"-node", nodeCfg)

	// addressSetReconciler is fed from the pod, namespace and node controllers
	m.addressSetReconciler = controller.NewReconciler(
		m.name+"-addrset",
		&controller.ReconcilerConfig{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:   m.reconcileAddressSet,
			Threadiness: 1,
			MaxAttempts: controller.InfiniteAttempts,
		},
	)
	return m
}

func (m *AddressSetManager) Start() error {
	klog.Infof("Starting %s controller", m.name)
	return controller.StartWithInitialSync(m.initialSync, m.podController, m.nsController, m.nodeController, m.addressSetReconciler)
}

func (m *AddressSetManager) Stop() {
	klog.Infof("Stopping %s controller", m.name)
	controller.Stop(m.podController, m.nsController, m.nodeController, m.addressSetReconciler)
}

// initialSync will clean up all address sets that don't have ACL reference
// Since addressset manager is started before its users, the cleanup for not-anymore-existing objects will be done
// after this function returns, so we technically clean up address sets that are not used anymore on the next restart only.
// There is no good way to know at this point which address sets will become unused after all the users finish their cleanup.
// Address sets don't have an informer, so we won't run reconcile for every address set, only when someone requests
// it through EnsureAddressSet, so if you look in the db directly some address sets may have stale IPs, but that only means
// that they are not used anymore.
func (m *AddressSetManager) initialSync() error {
	if config.Kubernetes.HostNetworkNamespace != "" {
		if err := m.updateHostNetworkNamespaceExists(); err != nil {
			return fmt.Errorf("failed to check if host network namespace %s exists: %v", config.Kubernetes.HostNetworkNamespace, err)
		}
	}
	return libovsdbutil.DeleteAddrSetsWithoutACLRefAnyController(libovsdbops.AddressSetPodSelector, m.nbClient)
}

// getClusterNodeIPsAddrSetDbIDs returns the DB IDs for the shared cluster node IP address set.
func getClusterNodeIPsAddrSetDbIDs() *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetClusterNodeIPs, ovntypes.DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ClusterNodeIPsAddrSetName,
		})
}

// EnsureClusterNodeIPsAddressSet registers a controller-lifetime user of the
// shared cluster node IP address set and returns its DB IDs. The address set is
// populated from node k8s.ovn.org/host-cidrs annotations through
// util.GetNodeAddresses. Users do not deregister because each backref
// represents a feature controller, not an individual object.
func (m *AddressSetManager) EnsureClusterNodeIPsAddressSet(backRef string) (*libovsdbops.DbObjectIDs, error) {
	dbIDs := getClusterNodeIPsAddrSetDbIDs()
	if backRef == "" {
		return nil, fmt.Errorf("cluster node IP address set backref is empty")
	}
	m.clusterNodeIPsLock.Lock()
	defer m.clusterNodeIPsLock.Unlock()

	if m.clusterNodeIPsBackRefs.Has(backRef) {
		return dbIDs, nil
	}

	m.clusterNodeIPsBackRefs.Insert(backRef)
	if err := m.syncClusterNodeIPsAddressSetLocked(); err != nil {
		m.clusterNodeIPsBackRefs.Delete(backRef)
		return nil, err
	}
	return dbIDs, nil
}

func (m *AddressSetManager) clusterNodeIPsAddressSetInUse() bool {
	m.clusterNodeIPsLock.RLock()
	defer m.clusterNodeIPsLock.RUnlock()
	return m.clusterNodeIPsBackRefs.Len() > 0
}

func (m *AddressSetManager) syncClusterNodeIPsAddressSet() error {
	m.clusterNodeIPsLock.Lock()
	defer m.clusterNodeIPsLock.Unlock()

	return m.syncClusterNodeIPsAddressSetLocked()
}

func (m *AddressSetManager) syncClusterNodeIPsAddressSetLocked() error {
	if m.clusterNodeIPsBackRefs.Len() == 0 {
		return nil
	}

	as, err := addressset.NewOvnAddressSetFactory(m.nbClient, config.IPv4Mode, config.IPv6Mode).EnsureAddressSet(getClusterNodeIPsAddrSetDbIDs())
	if err != nil {
		return fmt.Errorf("cannot ensure address set %s exists: %w", ClusterNodeIPsAddrSetName, err)
	}

	nodes, err := m.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}
	v4NodeAddrs, v6NodeAddrs, err := util.GetNodeAddresses(config.IPv4Mode, config.IPv6Mode, nodes...)
	if err != nil {
		return fmt.Errorf("failed to get node addresses: %w", err)
	}
	allAddresses := make([]net.IP, 0, len(v4NodeAddrs)+len(v6NodeAddrs))
	allAddresses = append(allAddresses, v4NodeAddrs...)
	allAddresses = append(allAddresses, v6NodeAddrs...)
	if err := as.SetAddresses(util.StringSlice(allAddresses)); err != nil {
		return fmt.Errorf("failed to set node IP address set addresses: %w", err)
	}
	return nil
}

// EnsureAddressSet returns address set for requested (podSelector, namespaceSelector, namespace, nodeSelector).
// If namespaceSelector is nil, namespace will be used with podSelector statically.
// podSelector should not be nil, use metav1.LabelSelector{} to match all pods.
// namespaceSelector can only be nil when namespace is set, use metav1.LabelSelector{} to match all namespaces.
// nodeSelector is optional; nil means pods on all nodes are included.
// podSelector = metav1.LabelSelector{} + static namespace may be replaced with namespace address set,
// podSelector = metav1.LabelSelector{} + namespaceSelector may be replaced with a set of namespace address sets,
// but both cases will work here too.
// legacyNetpolMode will not select hostnetwork pod IPs and will include config.Kubernetes.HostNetworkNamespace address set IPs
// when that namespace is matched with an empty pod selector.
//
// backRef is the key that should be used for cleanup.
// psAddrSetHashV4, psAddrSetHashV6 may be set to empty string if address set for that ipFamily wasn't created.
func (m *AddressSetManager) EnsureAddressSet(podSelector, namespaceSelector, nodeSelector *metav1.LabelSelector,
	namespace, backRef, controllerName string, netInfo util.NetInfo, legacyNetpolMode bool) (addrSetKey, psAddrSetHashV4, psAddrSetHashV6 string, err error) {
	nodeSelector = normalizeNodeSelector(nodeSelector)
	if podSelector == nil {
		err = fmt.Errorf("pod selector is nil")
		return
	}
	if namespaceSelector == nil && namespace == "" {
		err = fmt.Errorf("namespace selector is nil and namespace is empty")
		return
	}
	if namespaceSelector != nil {
		// namespace will be ignored in this case
		namespace = ""
	}
	var nsSel, podSel, nodeSel labels.Selector
	if namespaceSelector != nil {
		nsSel, err = metav1.LabelSelectorAsSelector(namespaceSelector)
		if err != nil {
			err = fmt.Errorf("can't parse namespace selector %v: %w", namespaceSelector, err)
			return
		}
	}

	podSel, err = metav1.LabelSelectorAsSelector(podSelector)
	if err != nil {
		err = fmt.Errorf("can't parse pod selector %v: %w", podSelector, err)
		return
	}
	if nodeSelector != nil {
		nodeSel, err = metav1.LabelSelectorAsSelector(nodeSelector)
		if err != nil {
			err = fmt.Errorf("can't parse node selector %v: %w", nodeSelector, err)
			return
		}
	}
	addrSetKey = getInternalKey(podSelector, namespaceSelector, nodeSelector, namespace, controllerName, legacyNetpolMode)

	var found bool
	err = m.addressSets.DoWithLock(addrSetKey, func(key string) error {
		var psAddrSet *podSelectorAddressSet
		psAddrSet, found = m.addressSets.Load(key)
		if !found {
			addrSetDbIDs := GetPodSelectorAddrSetDbIDs(podSelector, namespaceSelector, nodeSelector, namespace, controllerName, legacyNetpolMode)
			ipv4Mode, ipv6Mode := netInfo.IPMode()
			var addrSet addressset.AddressSet
			switch {
			case ipv4Mode && !ipv6Mode:
				addrSet, err = m.addressSetFactoryV4.EnsureAddressSet(addrSetDbIDs)
			case !ipv4Mode && ipv6Mode:
				addrSet, err = m.addressSetFactoryV6.EnsureAddressSet(addrSetDbIDs)
			case ipv4Mode && ipv6Mode:
				addrSet, err = m.addressSetFactoryDualstack.EnsureAddressSet(addrSetDbIDs)
			default:
				return fmt.Errorf("neither IPv4 nor IPv6 mode is enabled")
			}
			// if the first step of creating address set fails, return error since there is nothing to cleanup
			if err != nil {
				return err
			}
			psAddrSet = &podSelectorAddressSet{
				backRefs:          map[string]bool{},
				podSelector:       podSel,
				namespaceSelector: nsSel,
				namespace:         namespace,
				nodeSelector:      nodeSel,
				addressSet:        addrSet,
				controllerName:    controllerName,
				netInfo:           netInfo,
				legacyNetpolMode:  legacyNetpolMode,
				selectedNamespaces: &selectedNamespaces{
					set: sets.New[string](),
					// until the first reconcile, we assume all namespaces are selected to avoid missing any updates
					all: true,
				},
			}
			m.addressSets.LoadOrStore(key, psAddrSet)
		}
		// psAddrSet is successfully init-ed
		psAddrSet.backRefs[backRef] = true
		psAddrSetHashV4, psAddrSetHashV6 = psAddrSet.addressSet.GetASHashNames()
		return nil
	})
	if err != nil {
		return
	}
	if !found {
		// Populate a newly created address set before returning hashes to the caller.
		// Until reconcile runs, the NB object is empty while ACLs may already reference it
		// (e.g. on DB ID change or first ensure). This is a one-time sync on creation;
		// existing sets are kept up to date by the async reconciler on pod/namespace/node events.
		if reconcileErr := m.reconcileAddressSet(context.Background(), addrSetKey); reconcileErr != nil {
			klog.Errorf("Failed to reconcile address set %s on ensure: %v", addrSetKey, reconcileErr)
			// this only puts key to the queue, no lock
			m.addressSetReconciler.Reconcile(addrSetKey)
		}
	}
	return
}

// CleanupForController destroys all address sets owned by the given controller
func (m *AddressSetManager) CleanupForController(controllerName string) error {
	var errs []error
	for _, key := range m.addressSets.GetKeys() {
		if err := m.addressSets.DoWithLock(key, func(key string) error {
			psAddrSet, found := m.addressSets.Load(key)
			if !found || psAddrSet.controllerName != controllerName {
				return nil
			}
			if err := psAddrSet.addressSet.Destroy(); err != nil {
				return fmt.Errorf("failed to destroy address set %s: %w", key, err)
			}
			m.addressSets.Delete(key)
			return nil
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

func (m *AddressSetManager) DeleteAddressSet(addrSetKey, backRef string) error {
	return m.addressSets.DoWithLock(addrSetKey, func(key string) error {
		psAddrSet, found := m.addressSets.Load(key)
		if !found {
			return nil
		}
		delete(psAddrSet.backRefs, backRef)
		if len(psAddrSet.backRefs) == 0 {
			err := psAddrSet.addressSet.Destroy()
			if err != nil {
				return err
			}
			m.addressSets.Delete(key)
			m.hostNetworkNamespaceLock.Lock()
			m.hostNetworkSelectingAddrSets.Delete(key)
			m.hostNetworkNamespaceLock.Unlock()
		}
		return nil
	})
}

func (m *AddressSetManager) podNeedUpdate(old, new *corev1.Pod) bool {
	if new == nil {
		return true
	}
	if new.Spec.NodeName == "" {
		// pod is not scheduled yet, no IPs should be assigned, but update event will be received when pod gets scheduled, so we can wait for that
		return false
	}
	if old == nil {
		// new pod, check if it has IPs already, if not, wait for update event when IPs are assigned
		return new.Annotations[ovntypes.OvnPodAnnotationName] != "" || len(new.Status.PodIPs) > 0
	}
	if new.Annotations[ovntypes.OvnPodAnnotationName] != old.Annotations[ovntypes.OvnPodAnnotationName] {
		// this annotation is set when pod gets its IPs, so if it changes, we need to reconcile to update address set with new IPs
		return true
	}
	if !slices.Equal(new.Status.PodIPs, old.Status.PodIPs) {
		// if pod IPs change, we need to reconcile to update address set with new IPs
		return true
	}
	if util.PodCompleted(new) != util.PodCompleted(old) {
		// if pod has completed, handle as delete event following retry framework logic
		return true
	}
	if !labels.Equals(new.Labels, old.Labels) {
		// labels updates affect selectors
		return true
	}
	return false
}

func (m *AddressSetManager) reconcilePod(_ context.Context, podKey string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(podKey)
	if err != nil {
		return fmt.Errorf("failed to split meta namespace key %q: %v", podKey, err)
	}
	var pod *corev1.Pod
	// only reconcile if this pod is in a namespace that is selected by an address set
	// Get all existing keys, then lock address sets per key and check if they are affected.
	// If the new keys are added, it will always call reconcile for that new key, so there is no race.
	// If some keys are deleted, we just ignore it.
	existingAddrSets := m.addressSets.GetKeys()
	for _, addrSetKey := range existingAddrSets {
		// never returns error
		if err = m.addressSets.DoWithLock(addrSetKey, func(addrSetKey string) error {
			addrSet, found := m.addressSets.Load(addrSetKey)
			if !found {
				// nothing to do
				return nil
			}
			if addrSet.nodeSelector != nil {
				if pod == nil {
					pod, err = m.podLister.Pods(namespace).Get(name)
					if err != nil {
						if apierrors.IsNotFound(err) {
							// pod deleted
							pod = nil
						} else {
							return fmt.Errorf("failed to get pod %s in namespace %s: %v", name, namespace, err)
						}
					}
				}
				// check if pod's node matches address set's node selector
				if pod == nil || addrSet.selectedNodes == nil || addrSet.selectedNodes.Has(pod.Spec.NodeName) {
					m.addressSetReconciler.Reconcile(addrSetKey)
					return nil
				}
				return nil
			}
			// only check address sets that have previously matched pod's namespace to avoid extra reconciliations
			previouslyMatchedNamespaces := addrSet.selectedNamespaces
			if previouslyMatchedNamespaces == nil || previouslyMatchedNamespaces.Has(namespace) {
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			return nil
		}); err != nil {
			return fmt.Errorf("failed to reconcile address set %s for pod %s: %v", addrSetKey, podKey, err)
		}
	}
	return nil
}

func (m *AddressSetManager) nsNeedUpdate(old, new *corev1.Namespace) bool {
	if new == nil || old == nil {
		return true
	}
	if !labels.Equals(new.Labels, old.Labels) {
		// if namespace labels change, we need to reconcile to check if this namespace still matches address set selectors
		return true
	}
	return false
}

func (m *AddressSetManager) updateHostNetworkNamespaceExists() error {
	_, err := m.namespaceLister.Get(config.Kubernetes.HostNetworkNamespace)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get host network namespace %s: %v", config.Kubernetes.HostNetworkNamespace, err)
		}
	}
	m.hostNetworkNamespaceLock.Lock()
	defer m.hostNetworkNamespaceLock.Unlock()
	if err != nil {
		// namespace was deleted/never existed
		clear(m.hostNetworkNamespaceIPsPerNode)
		m.hostNetworkNamespaceExists = false
		return nil
	}
	if !m.hostNetworkNamespaceExists {
		// namespace was just created, get all existing host network IPs
		ips, err := m.getAllHostNamespaceAddresses()
		if err != nil {
			return fmt.Errorf("error getting host network namespace %s IPs: %v", config.Kubernetes.HostNetworkNamespace, err)
		}

		m.hostNetworkNamespaceIPsPerNode = ips
		for addrSetKey := range m.hostNetworkSelectingAddrSets {
			m.addressSetReconciler.Reconcile(addrSetKey)
		}
	}
	m.hostNetworkNamespaceExists = true
	return nil
}

func (m *AddressSetManager) reconcileNamespace(_ context.Context, nsKey string) error {
	if config.Kubernetes.HostNetworkNamespace != "" && nsKey == config.Kubernetes.HostNetworkNamespace {
		if err := m.updateHostNetworkNamespaceExists(); err != nil {
			return fmt.Errorf("failed to check if host network namespace %s exists: %v", config.Kubernetes.HostNetworkNamespace, err)
		}
	}
	// find address sets that could be affected by this namespace event
	// Get all existing keys, then lock address sets per key and check if they are affected.
	// If the new keys are added, it will always call reconcile for that new key, so there is no race.
	// If some keys are deleted, we just ignore it.
	existingAddrSets := m.addressSets.GetKeys()
	for _, addrSetKey := range existingAddrSets {
		err := m.addressSets.DoWithLock(addrSetKey, func(addrSetKey string) error {
			addrSet, found := m.addressSets.Load(addrSetKey)
			if !found {
				// nothing to do
				return nil
			}
			// first find namespaces that currently match this address set
			currentlyMatchedNamespaces, err := m.getSelectedNamespaces(addrSet)
			if err != nil {
				return err
			}
			if currentlyMatchedNamespaces.Has(nsKey) {
				// this namespace is relevant for this address set, reconcile
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			// now check if this address set was matching this namespace before, if yes, reconcile since it might not match anymore
			previouslyMatchedNamespaces := addrSet.selectedNamespaces
			if previouslyMatchedNamespaces.Has(nsKey) {
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to reconcile address set %s for namespace %s: %v", addrSetKey, nsKey, err)
		}

	}
	return nil
}

func (m *AddressSetManager) nodeNeedUpdate(old, new *corev1.Node) bool {
	if new == nil || old == nil {
		return true
	}
	if !labels.Equals(new.Labels, old.Labels) {
		// if node labels change, we need to reconcile address sets that use a node selector
		return true
	}
	if util.NodeHostCIDRsAnnotationChanged(old, new) && m.clusterNodeIPsAddressSetInUse() {
		return true
	}
	// only check annotations that are used in getHostNamespaceAddressesForNode
	if util.NodeSubnetAnnotationChangedForNetwork(old, new, ovntypes.DefaultNetworkName) || util.NodeIDAnnotationChanged(old, new) ||
		old.Annotations[util.OvnNodeIfAddr] != new.Annotations[util.OvnNodeIfAddr] {
		// hostnetwork namespace may need to be updated
		m.hostNetworkNamespaceLock.Lock()
		defer m.hostNetworkNamespaceLock.Unlock()
		return m.hostNetworkNamespaceExists
	}

	return false
}

func (m *AddressSetManager) reconcileNode(_ context.Context, nodeKey string) error {
	// update host network IPs first to have fresh info for addr set reconcile
	// don't return error immediately to let other changes like node selector be propagated
	hostNetworkErr := m.updateHostNetworkIPs(nodeKey)
	clusterNodeIPsErr := m.syncClusterNodeIPsAddressSet()

	// find address sets that could be affected by this node event
	// Get all existing keys, then lock address sets per key and check if they are affected.
	// If the new keys are added, it will always call reconcile for that new key, so there is no race.
	// If some keys are deleted, we just ignore it.
	existingAddrSets := m.addressSets.GetKeys()
	for _, addrSetKey := range existingAddrSets {
		err := m.addressSets.DoWithLock(addrSetKey, func(addrSetKey string) error {
			addrSet, _ := m.addressSets.Load(addrSetKey)
			if addrSet == nil || addrSet.nodeSelector == nil || addrSet.nodeSelector.Empty() {
				// nothing to do
				return nil
			}
			// first find nodes that currently match this address set
			currentlyMatchedNodes, err := m.getSelectedNodes(addrSet.nodeSelector)
			if err != nil {
				return err
			}
			if currentlyMatchedNodes.Has(nodeKey) {
				// this node is relevant for this address set, reconcile
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			// reconcile the address set if the node matches the previous selected nodes
			previouslyMatchedNodes := addrSet.selectedNodes
			// previouslyMatchedNodes == nil means the address set hasn't been reconciled yet, so need to reconcile
			if previouslyMatchedNodes == nil || previouslyMatchedNodes.Has(nodeKey) {
				m.addressSetReconciler.Reconcile(addrSetKey)
				return nil
			}
			return nil
		})
		if err != nil {
			return utilerrors.Join(hostNetworkErr, clusterNodeIPsErr,
				fmt.Errorf("failed to reconcile address set %s for node %s: %v", addrSetKey, nodeKey, err))
		}
	}
	return utilerrors.Join(hostNetworkErr, clusterNodeIPsErr)
}

func (m *AddressSetManager) updateHostNetworkIPs(nodeName string) error {
	m.hostNetworkNamespaceLock.Lock()
	defer m.hostNetworkNamespaceLock.Unlock()
	if !m.hostNetworkNamespaceExists {
		return nil
	}
	node, err := m.nodeLister.Get(nodeName)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get node %s: %v", nodeName, err)
	}
	if err != nil || config.HybridOverlay.Enabled && util.NoHostSubnet(node) {
		// delete event OR node started matching hybrid overlay and should be ignored for host network namespace
		// delete host network IPs for this node from host network namespace's address set
		updated := len(m.hostNetworkNamespaceIPsPerNode[nodeName]) != 0
		delete(m.hostNetworkNamespaceIPsPerNode, nodeName)

		if !updated {
			return nil
		}
		for addrSetKey := range m.hostNetworkSelectingAddrSets {
			m.addressSetReconciler.Reconcile(addrSetKey)
		}
		return nil
	}
	// add/update node event
	hostNetworkPolicyIPs, err := m.getHostNamespaceAddressesForNode(node)
	if err != nil {
		return fmt.Errorf("error parsing annotation for node %s: %w", node.Name, err)
	}
	// add the host network IPs for this node to host network namespace's address set
	// hostNetworkPolicyIPs is built from the annotations and always preserves ips order
	if slices.Equal(m.hostNetworkNamespaceIPsPerNode[node.Name], hostNetworkPolicyIPs) {
		return nil
	}
	m.hostNetworkNamespaceIPsPerNode[node.Name] = hostNetworkPolicyIPs
	for addrSetKey := range m.hostNetworkSelectingAddrSets {
		m.addressSetReconciler.Reconcile(addrSetKey)
	}
	return nil
}

// getAllHostNamespaceAddresses retrieves management port and gateway router LRP
// IP for all nodes in the cluster
func (m *AddressSetManager) getAllHostNamespaceAddresses() (map[string][]string, error) {
	ips := make(map[string][]string)
	// add the mp0 interface addresses to this namespace.
	existingNodes, err := m.nodeLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to get all nodes (%v)", err)
	} else {
		for _, node := range existingNodes {
			if config.HybridOverlay.Enabled && util.NoHostSubnet(node) {
				// skip hybrid overlay nodes
				continue
			}
			hostNetworkIPs, err := m.getHostNamespaceAddressesForNode(node)
			if err != nil {
				klog.Errorf("Error parsing annotation for node %s: %v", node.Name, err)
			}
			ips[node.Name] = hostNetworkIPs
		}
	}
	return ips, nil
}

// getHostNamespaceAddressesForNode retrieves management port and gateway router LRP
// IP of a specific node
func (m *AddressSetManager) getHostNamespaceAddressesForNode(node *corev1.Node) ([]string, error) {
	var ips []string
	defaultNetInfo := &util.DefaultNetInfo{}
	hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, ovntypes.DefaultNetworkName)
	if err != nil {
		if !util.IsAnnotationNotSetError(err) {
			return nil, fmt.Errorf("failed to get node host subnets: %w", err)
		}
	}
	for _, hostSubnet := range hostSubnets {
		mgmtIfAddr := defaultNetInfo.GetNodeManagementIP(hostSubnet)
		if mgmtIfAddr == nil {
			return nil, fmt.Errorf("node %s has no management IP in subnet %s", node.Name, hostSubnet.String())
		}
		ips = append(ips, mgmtIfAddr.IP.String())
	}
	// for shared gateway mode we will use LRP IPs to SNAT host network traffic
	// so add these to the address set.
	lrpIPs, gwIPsErr := udn.GetGWRouterIPs(node, defaultNetInfo)
	if gwIPsErr != nil {
		if !util.IsAnnotationNotSetError(gwIPsErr) {
			return nil, gwIPsErr
		}
	}

	for _, lrpIP := range lrpIPs {
		ips = append(ips, lrpIP.IP.String())
	}
	// When NoOverlay mode is enabled, also include the node's primary physical interface IP
	if defaultNetInfo.Transport() == ovntypes.NetworkTransportNoOverlay {
		nodeIfAddr, err := util.GetNodeIfAddrAnnotation(node)
		if err != nil {
			if !util.IsAnnotationNotSetError(err) {
				return nil, fmt.Errorf("failed to get node primary interface address: %w", err)
			}
		} else {
			if nodeIfAddr.IPv4 != "" {
				ipv4, _, err := net.ParseCIDR(nodeIfAddr.IPv4)
				if err != nil {
					return nil, fmt.Errorf("failed to parse node primary IPv4 address %s: %w", nodeIfAddr.IPv4, err)
				}
				ips = append(ips, ipv4.String())
			}
			if nodeIfAddr.IPv6 != "" {
				ipv6, _, err := net.ParseCIDR(nodeIfAddr.IPv6)
				if err != nil {
					return nil, fmt.Errorf("failed to parse node primary IPv6 address %s: %w", nodeIfAddr.IPv6, err)
				}
				ips = append(ips, ipv6.String())
			}
		}
	}
	return ips, nil
}

func (m *AddressSetManager) reconcileAddressSet(_ context.Context, key string) error {
	return m.addressSets.DoWithLock(key, func(key string) error {
		psAddrSet, found := m.addressSets.Load(key)
		if !found {
			return nil
		}
		matchedNamespaces, err := m.getSelectedNamespaces(psAddrSet)
		if err != nil {
			return fmt.Errorf("failed to get selected namespaces for address set %s: %v", key, err)
		}
		var pods []*corev1.Pod
		if matchedNamespaces.all {
			// no namespace selector, use pod selector only
			if psAddrSet.podSelector.Empty() {
				// all cluster pods
				pods, err = m.podLister.List(labels.Everything())
				if err != nil {
					return fmt.Errorf("failed to list pods: %v", err)
				}
			} else {
				// global pod selector
				pods, err = m.podLister.List(psAddrSet.podSelector)
				if err != nil {
					return fmt.Errorf("failed to list pods: %v", err)
				}
			}
		} else {
			// namespace selector is set, apply pod selector in every namespace
			for ns := range matchedNamespaces.set {
				if psAddrSet.podSelector.Empty() {
					// empty selector means no filtering, select all pods in a given namespace
					nsPods, err := m.podLister.Pods(ns).List(labels.Everything())
					if err != nil {
						return fmt.Errorf("failed to list pods in namespace %s: %v", ns, err)
					}
					pods = append(pods, nsPods...)
				} else {
					// namespaced pod selector, select matching pods in a given namespace
					nsPods, err := m.podLister.Pods(ns).List(psAddrSet.podSelector)
					if err != nil {
						return fmt.Errorf("failed to list pods in namespace %s: %v", ns, err)
					}
					pods = append(pods, nsPods...)
				}
			}
		}
		// apply node selector filter if it's not empty
		if psAddrSet.nodeSelector != nil && !psAddrSet.nodeSelector.Empty() {
			selectedNodes, err := m.getSelectedNodes(psAddrSet.nodeSelector)
			if err != nil {
				return fmt.Errorf("failed to get selected nodes for address set %s: %v", key, err)
			}
			filtered := make([]*corev1.Pod, 0, len(pods))
			for _, pod := range pods {
				if pod.Spec.NodeName != "" && selectedNodes.Has(pod.Spec.NodeName) {
					filtered = append(filtered, pod)
				}
			}
			pods = filtered
			psAddrSet.selectedNodes = selectedNodes
		}
		ips, err := m.getPodIPs(pods, psAddrSet.netInfo, psAddrSet.legacyNetpolMode)
		if err != nil {
			return fmt.Errorf("failed to get pod IPs: %v", err)
		}
		// now check if this address set should add hostNetworkNamespace IPs
		// it only makes sense for the default network
		if psAddrSet.legacyNetpolMode && psAddrSet.netInfo.IsDefault() && config.Kubernetes.HostNetworkNamespace != "" &&
			psAddrSet.podSelector.Empty() {
			// update m.hostNetworkSelectingAddrSets
			m.hostNetworkNamespaceLock.Lock()
			if m.hostNetworkNamespaceExists {
				if matchedNamespaces.Has(config.Kubernetes.HostNetworkNamespace) {
					for _, hostnetIPs := range m.hostNetworkNamespaceIPsPerNode {
						ips = append(ips, hostnetIPs...)
					}
					m.hostNetworkSelectingAddrSets.Insert(key)
				} else {
					m.hostNetworkSelectingAddrSets.Delete(key)
				}
			}
			m.hostNetworkNamespaceLock.Unlock()
		}

		// this operation doesn't check the contents on the address set and will run a db transaction
		// every time, may be improved.
		err = psAddrSet.addressSet.SetAddresses(ips)
		if err != nil {
			return fmt.Errorf("failed to set addresses for address set %s: %v", key, err)
		}
		psAddrSet.selectedNamespaces = matchedNamespaces
		return nil
	})
}

type selectedNamespaces struct {
	set sets.Set[string]
	// if true, it means all namespaces are selected, so set won't be populated
	all bool
}

func (s *selectedNamespaces) Has(namespace string) bool {
	return s.all || s.set.Has(namespace)
}

// getSelectedNamespaces returns a set of namespaces that should be selected for a given podSelectorAddressSet.
// nil set means no namespace selector is set and all namespaces match.
func (m *AddressSetManager) getSelectedNamespaces(s *podSelectorAddressSet) (*selectedNamespaces, error) {
	matchedNamespaces := &selectedNamespaces{
		set: sets.New[string](),
	}
	if s.namespace != "" {
		// static namespace case
		matchedNamespaces.set.Insert(s.namespace)
	} else if s.namespaceSelector.Empty() {
		// any namespace
		matchedNamespaces.all = true
	} else {
		// selected namespaces
		namespaces, err := m.namespaceLister.List(s.namespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %v", err)
		}
		for _, ns := range namespaces {
			matchedNamespaces.set.Insert(ns.Name)
		}
	}
	return matchedNamespaces, nil
}

// getSelectedNodes returns the set of node names that match the node selector.
func (m *AddressSetManager) getSelectedNodes(nodeSelector labels.Selector) (sets.Set[string], error) {
	nodes, err := m.nodeLister.List(nodeSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %v", err)
	}
	names := sets.New[string]()
	for _, n := range nodes {
		names.Insert(n.Name)
	}
	return names, nil
}

func (m *AddressSetManager) getPodIPs(pods []*corev1.Pod, netInfo util.NetInfo, noHostNetwork bool) ([]string, error) {
	ips := []string{}
	for _, pod := range pods {
		if noHostNetwork && pod.Spec.HostNetwork {
			// skip hostNetwork pods if requested, since they are not selected in legacyNetpolMode
			continue
		}
		if pod.Annotations[ovntypes.OvnPodAnnotationName] == "" && len(pod.Status.PodIPs) == 0 {
			// pod doesn't have IPs yet, skip it
			continue
		}
		// handle completed pods as deleted since their IPs may be already released and re-allocated to other pods
		// due to retry framework logic
		if util.PodCompleted(pod) {
			continue
		}
		podIPs, err := util.GetPodIPsOfNetwork(pod, netInfo, m.getNetworkNameForNADKey)
		if err != nil {
			// not finding pod IPs on a remote pod is common until the other node wires the pod, suppress it
			return nil, ovntypes.NewSuppressedError(err)
		}
		ips = append(ips, util.StringSlice(podIPs)...)
	}
	return ips, nil
}

func GetPodSelectorAddrSetDbIDs(podSelector, namespaceSelector, nodeSelector *metav1.LabelSelector, namespace, controller string, legacyNetpolMode bool) *libovsdbops.DbObjectIDs {
	nodeSelector = normalizeNodeSelector(nodeSelector)
	addrsetKey := getPodSelectorKey(podSelector, namespaceSelector, nodeSelector, namespace, legacyNetpolMode)
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetPodSelector, controller, map[libovsdbops.ExternalIDKey]string{
		// pod selector address sets are cluster-scoped, only need name
		libovsdbops.ObjectNameKey: addrsetKey,
	})
}

// sortedLSRString is based on *LabelSelectorRequirement.String(),
// but adds sorting for Values
func sortedLSRString(lsr *metav1.LabelSelectorRequirement) string {
	if lsr == nil {
		return "nil"
	}
	lsrValues := make([]string, 0, len(lsr.Values))
	lsrValues = append(lsrValues, lsr.Values...)
	sort.Strings(lsrValues)
	s := strings.Join([]string{`LSR{`,
		`Key:` + fmt.Sprintf("%v", lsr.Key) + `,`,
		`Operator:` + fmt.Sprintf("%v", lsr.Operator) + `,`,
		`Values:` + fmt.Sprintf("%v", lsrValues) + `,`,
		`}`,
	}, "")
	return s
}

// shortLabelSelectorString is based on *LabelSelector.String(),
// but makes sure to generate the same string for equivalent selectors (by additional sorting).
// It also tries to reduce return string length, since this string will be put to the db ad ExternalID.
func shortLabelSelectorString(sel *metav1.LabelSelector) string {
	if sel == nil {
		return "nil"
	}
	var repeatedStringForMatchExpressions, mapStringForMatchLabels string
	if len(sel.MatchExpressions) > 0 {
		repeatedStringForMatchExpressions = "ME:{"
		matchExpressions := make([]string, 0, len(sel.MatchExpressions))
		for _, f := range sel.MatchExpressions {
			matchExpressions = append(matchExpressions, sortedLSRString(&f))
		}
		// sort match expressions to not depend on MatchExpressions order
		sort.Strings(matchExpressions)
		repeatedStringForMatchExpressions += strings.Join(matchExpressions, ",")
		repeatedStringForMatchExpressions += "}"
	} else {
		repeatedStringForMatchExpressions = ""
	}
	keysForMatchLabels := make([]string, 0, len(sel.MatchLabels))
	for k := range sel.MatchLabels {
		keysForMatchLabels = append(keysForMatchLabels, k)
	}
	sort.Strings(keysForMatchLabels)
	if len(keysForMatchLabels) > 0 {
		mapStringForMatchLabels = "ML:{"
		for _, k := range keysForMatchLabels {
			mapStringForMatchLabels += fmt.Sprintf("%v: %v,", k, sel.MatchLabels[k])
		}
		mapStringForMatchLabels += "}"
	} else {
		mapStringForMatchLabels = ""
	}
	s := "LS{"
	if mapStringForMatchLabels != "" {
		s += mapStringForMatchLabels + ","
	}
	if repeatedStringForMatchExpressions != "" {
		s += repeatedStringForMatchExpressions + ","
	}
	s += "}"
	return s
}

// Since we have joined this manager for multiple controllers, we need to make keys unique across controllers.
// In the db it is already achieved by using controller name in ExternalIDs, but for internal map we also need to add controller name
func getInternalKey(podSelector, namespaceSelector, nodeSelector *metav1.LabelSelector, namespace, controllerName string, legacyNetpolMode bool) string {
	return controllerName + "_" + getPodSelectorKey(podSelector, namespaceSelector, nodeSelector, namespace, legacyNetpolMode)
}

func getPodSelectorKey(podSelector, namespaceSelector, nodeSelector *metav1.LabelSelector, namespace string, legacyNetpolMode bool) string {
	var namespaceKey string
	if namespaceSelector == nil {
		// namespace is static
		namespaceKey = namespace
	} else {
		namespaceKey = shortLabelSelectorString(namespaceSelector)
	}
	key := namespaceKey + "_" + shortLabelSelectorString(podSelector)
	if nodeSelector != nil {
		key += "_" + shortLabelSelectorString(nodeSelector)
	}
	if legacyNetpolMode {
		return key + "_LNM"
	} else {
		return key
	}
}

func normalizeNodeSelector(sel *metav1.LabelSelector) *metav1.LabelSelector {
	if sel != nil && len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
		return nil
	}
	return sel
}

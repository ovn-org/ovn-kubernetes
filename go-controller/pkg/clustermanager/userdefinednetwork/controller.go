// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package userdefinednetwork

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netv1clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	netv1infomer "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	netv1lister "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	corev1informer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/notifier"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	rainformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/informers/externalversions/routeadvertisements/v1"
	ralister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/listers/routeadvertisements/v1"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnapplyconfkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/applyconfiguration/userdefinednetwork/v1"
	userdefinednetworkclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	userdefinednetworkscheme "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/scheme"
	userdefinednetworkinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/informers/externalversions/userdefinednetwork/v1"
	userdefinednetworklister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	vtepinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/informers/externalversions/vtep/v1"
	vteplister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/listers/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utiludn "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/udn"
)

const (
	conditionTypeNetworkCreated = "NetworkCreated"

	// Condition reasons
	reasonNADCreated   = "NetworkAttachmentDefinitionCreated"
	reasonSyncError    = "SyncError"
	reasonNADDeleted   = "NetworkAttachmentDefinitionDeleted"
	reasonNADSyncError = "NetworkAttachmentDefinitionSyncError"

	// MaxEVPNVIDs is the maximum number of VIDs available for EVPN networks (0-4094, but 0 and 1 are reserved).
	MaxEVPNVIDs = 4095
	// reservedVIDZeroKey is the key used to reserve VID 0 (reserved per IEEE 802.1Q for priority tagging).
	reservedVIDZeroKey = "__vid_zero_reserved__"
	// reservedVIDOneKey is the key used to reserve VID 1 (default VLAN on many switches, avoided by convention).
	reservedVIDOneKey = "__vid_one_reserved__"
)

// macVRFKey returns the VID allocator key for a network's MAC-VRF.
func macVRFKey(networkName string) string {
	return networkName + "/macvrf"
}

// ipVRFKey returns the VID allocator key for a network's IP-VRF.
func ipVRFKey(networkName string) string {
	return networkName + "/ipvrf"
}

// vniKey identifies a VNI within a VTEP scope. VNIs must be unique per VTEP.
type vniKey struct {
	vtep string
	vni  int32
}

type RenderNetAttachDefManifest func(obj client.Object, targetNamespace string, opts ...template.RenderOption) (*netv1.NetworkAttachmentDefinition, error)

type networkInUseError struct {
	err error
}

func (n *networkInUseError) Error() string {
	return n.err.Error()
}

// vtepNotFoundError indicates that a required VTEP CR does not exist.
type vtepNotFoundError struct {
	vtepName string
}

func (e *vtepNotFoundError) Error() string {
	return fmt.Sprintf("VTEP %q does not exist", e.vtepName)
}

// vtepNotAcceptedError indicates that a required VTEP CR exists but is not yet accepted.
type vtepNotAcceptedError struct {
	vtepName string
}

func (e *vtepNotAcceptedError) Error() string {
	return fmt.Sprintf("VTEP %q is not accepted", e.vtepName)
}

// evpnConfigError indicates an EVPN configuration issue (e.g. feature not enabled, wrong gateway mode).
type evpnConfigError struct {
	msg string
}

func (e *evpnConfigError) Error() string {
	return e.msg
}

type Controller struct {
	// cudnController manage ClusterUserDefinedNetwork CRs.
	cudnController controller.Controller
	// udnController manage UserDefinedNetwork CRs.
	udnController controller.Controller
	// nadNotifier notifies subscribing controllers about NetworkAttachmentDefinition events.
	nadNotifier *notifier.NetAttachDefNotifier
	// namespaceInformer notifies subscribing controllers about Namespace events.
	namespaceNotifier *notifier.NamespaceNotifier
	// namespaceTracker tracks each CUDN CRs affected namespaces, enable finding stale NADs.
	// Keys are CR name, value is affected namespace names slice.
	namespaceTracker     map[string]sets.Set[string]
	namespaceTrackerLock sync.RWMutex
	// cudnMetricTracker tracks which CUDNs are counted in the CUDN gauge metric.
	// Maps (role, topology, transport) labels to the set of CUDN names sharing
	// that combination.
	// Only mutated from syncClusterUDN and initializeController (single-threaded).
	cudnMetricTracker map[cudnMetricKey]sets.Set[string]
	// renderNadFn render NAD manifest from given object, enable replacing in tests.
	renderNadFn RenderNetAttachDefManifest
	// createNetworkLock lock should be held when NAD is created to avoid having two components
	// trying to create an object with the same name.
	createNetworkLock sync.Mutex

	networkManager networkmanager.Interface

	// vidAllocator allocates cluster-wide VLAN IDs for EVPN networks.
	// VIDs are allocated per network name and stored in the NAD config JSON.
	vidAllocator id.Allocator

	// reservedVNIs tracks (VTEP, VNI) → network name to ensure no two networks
	// sharing the same VTEP use the same VNI.
	reservedVNIs     map[vniKey]string
	reservedVNIsLock sync.RWMutex

	udnClient         userdefinednetworkclientset.Interface
	udnLister         userdefinednetworklister.UserDefinedNetworkLister
	cudnLister        userdefinednetworklister.ClusterUserDefinedNetworkLister
	nadClient         netv1clientset.Interface
	nadLister         netv1lister.NetworkAttachmentDefinitionLister
	podInformer       corev1informer.PodInformer
	namespaceInformer corev1informer.NamespaceInformer
	// vtepLister provides read access to VTEP CRs for validating EVPN configuration.
	vtepLister vteplister.VTEPLister
	// vtepNotifier notifies subscribing controllers about VTEP events.
	vtepNotifier *notifier.VTEPNotifier
	// raLister provides read access to RouteAdvertisements CRs for validating no-overlay transport.
	raLister ralister.RouteAdvertisementsLister
	// raNotifier notifies subscribing controllers about RouteAdvertisements events.
	raNotifier *notifier.RouteAdvertisementsNotifier

	networkInUseRequeueInterval time.Duration
	eventRecorder               record.EventRecorder
}

func New(
	nadClient netv1clientset.Interface,
	nadInfomer netv1infomer.NetworkAttachmentDefinitionInformer,
	udnClient userdefinednetworkclientset.Interface,
	udnInformer userdefinednetworkinformer.UserDefinedNetworkInformer,
	cudnInformer userdefinednetworkinformer.ClusterUserDefinedNetworkInformer,
	renderNadFn RenderNetAttachDefManifest,
	networkManager networkmanager.Interface,
	podInformer corev1informer.PodInformer,
	namespaceInformer corev1informer.NamespaceInformer,
	vtepInformer vtepinformer.VTEPInformer,
	raInformer rainformer.RouteAdvertisementsInformer,
	eventRecorder record.EventRecorder,
) *Controller {
	udnLister := udnInformer.Lister()
	cudnLister := cudnInformer.Lister()

	// Allocates VIDs in range 1-4094 (0 is reserved per IEEE 802.1Q).
	vidAllocator := id.NewIDAllocator("EVPN-VIDs", MaxEVPNVIDs)

	c := &Controller{
		nadClient:         nadClient,
		nadLister:         nadInfomer.Lister(),
		udnClient:         udnClient,
		udnLister:         udnLister,
		cudnLister:        cudnLister,
		renderNadFn:       renderNadFn,
		podInformer:       podInformer,
		namespaceInformer: namespaceInformer,
		networkManager:    networkManager,
		namespaceTracker:  map[string]sets.Set[string]{},
		cudnMetricTracker: map[cudnMetricKey]sets.Set[string]{},
		vidAllocator:      vidAllocator,
		reservedVNIs:      map[vniKey]string{},
		eventRecorder:     eventRecorder,
	}
	udnCfg := &controller.ControllerConfig[userdefinednetworkv1.UserDefinedNetwork]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileUDN,
		ObjNeedsUpdate: c.udnNeedUpdate,
		Threadiness:    1,
		Informer:       udnInformer.Informer(),
		Lister:         udnLister.List,
	}
	c.udnController = controller.NewController[userdefinednetworkv1.UserDefinedNetwork]("user-defined-network-controller", udnCfg)

	cudnCfg := &controller.ControllerConfig[userdefinednetworkv1.ClusterUserDefinedNetwork]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileCUDN,
		ObjNeedsUpdate: c.cudnNeedUpdate,
		Threadiness:    1,
		Informer:       cudnInformer.Informer(),
		Lister:         cudnLister.List,
	}
	c.cudnController = controller.NewController[userdefinednetworkv1.ClusterUserDefinedNetwork]("cluster-user-defined-network-controller", cudnCfg)

	c.nadNotifier = notifier.NewNetAttachDefNotifier(nadInfomer, c)
	c.namespaceNotifier = notifier.NewNamespaceNotifier(namespaceInformer, c)

	// Setup EVPN components only when EVPN is enabled.
	if util.IsEVPNEnabled() && vtepInformer != nil {
		// Setup VTEP watching for EVPN support.
		c.vtepLister = vtepInformer.Lister()
		c.vtepNotifier = notifier.NewVTEPNotifier(vtepInformer, c)
	}

	// Setup RouteAdvertisements watching for no-overlay and EVPN transport validation.
	if raInformer != nil {
		c.raLister = raInformer.Lister()
		c.raNotifier = notifier.NewRouteAdvertisementsNotifier(raInformer, c)
	}

	return c
}

func (c *Controller) Run() error {
	klog.Infof("Starting user-defined network controllers")

	controllers := []controller.Reconciler{
		c.cudnController,
		c.udnController,
		c.nadNotifier.Controller,
		c.namespaceNotifier.Controller,
	}
	if c.vtepNotifier != nil {
		controllers = append(controllers, c.vtepNotifier.Controller)
	}
	if c.raNotifier != nil {
		controllers = append(controllers, c.raNotifier.Controller)
	}

	if err := controller.StartWithInitialSync(c.initializeController, controllers...); err != nil {
		return fmt.Errorf("unable to start user-defined network controller: %v", err)
	}

	if util.IsPreconfiguredUDNAddressesEnabled() {
		if _, err := util.EnsureDefaultNetworkNAD(c.nadLister, c.nadClient); err != nil {
			return fmt.Errorf("failed to ensure default network nad exists: %w", err)
		}
	}

	return nil
}

// initializeController performs all startup initialization before controllers begin processing.
func (c *Controller) initializeController() error {
	// Reserve VID 0 and VID 1 to ensure they're never allocated to any network.
	// VID 0 is reserved per IEEE 802.1Q standard.
	// VID 1 is the default VLAN on many switches and avoided by convention.
	if err := c.vidAllocator.ReserveID(reservedVIDZeroKey, 0); err != nil {
		return fmt.Errorf("failed to reserve VID 0: %w", err)
	}
	if err := c.vidAllocator.ReserveID(reservedVIDOneKey, 1); err != nil {
		return fmt.Errorf("failed to reserve VID 1: %w", err)
	}

	cudnNADs, err := c.buildCUDNToNADs()
	if err != nil {
		return err
	}
	if len(cudnNADs) == 0 {
		return nil
	}

	c.initializeNamespaceTracker(cudnNADs)
	c.seedCUDNCountMetrics(cudnNADs)

	if util.IsEVPNEnabled() {
		// Recover VID allocations from existing EVPN CUDNs.
		// Recovery failures are logged and the affected CUDNs are enqueued for reconciliation,
		// but don't block startup - this prevents a DoS where a malicious NAD could
		// crash the entire cluster-manager.
		c.recoverEVPNIDs(cudnNADs)
	}

	return nil
}

// cudnWithNADs pairs a CUDN with its owned NADs.
type cudnWithNADs struct {
	cudn *userdefinednetworkv1.ClusterUserDefinedNetwork
	nads []netv1.NetworkAttachmentDefinition
}

// cudnToNADs maps CUDN name to its object and owned NADs.
type cudnToNADs map[string]*cudnWithNADs

// buildCUDNToNADs builds an index of CUDNs to their owned NADs.
// It returns an entry for every existing CUDN, including CUDNs that currently own no NADs
func (c *Controller) buildCUDNToNADs() (cudnToNADs, error) {
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	if len(cudns) == 0 {
		return nil, nil
	}

	nads, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	cudnByUID := make(map[types.UID]*userdefinednetworkv1.ClusterUserDefinedNetwork, len(cudns))
	index := make(cudnToNADs, len(cudns))
	for _, cudn := range cudns {
		cudnByUID[cudn.UID] = cudn
		index[cudn.Name] = &cudnWithNADs{cudn: cudn}
	}

	for _, nad := range nads {
		if nad == nil {
			continue
		}
		controllerRef := metav1.GetControllerOfNoCopy(nad)
		if controllerRef == nil {
			continue
		}
		if cudn, ok := cudnByUID[controllerRef.UID]; ok {
			index[cudn.Name].nads = append(index[cudn.Name].nads, *nad.DeepCopy())
		}
	}

	return index, nil
}

// initializeNamespaceTracker populates the namespace tracker with NAD namespaces owned by each CUDN.
func (c *Controller) initializeNamespaceTracker(cudnNADs cudnToNADs) {
	for cudnName, entry := range cudnNADs {
		c.namespaceTracker[cudnName] = sets.New[string]()
		for _, nad := range entry.nads {
			c.namespaceTracker[cudnName].Insert(nad.Namespace)
		}
	}
}

// recoverEVPNIDs recovers VID allocations and VNI reservations from existing EVPN CUDNs using
// NetworkManager's cached NetInfo. NetworkManager has already processed all NADs
// by the time this function is called (it starts before UDN controller).
//
// CUDNs are processed in order of creation timestamp (oldest first) to ensure
// deterministic VID assignment when conflicts occur. If two CUDNs have NADs
// claiming the same VID, the oldest CUDN wins ("first come, first served").
// CUDN name is used as tie-breaker when timestamps are equal.
//
// If VID recovery fails for a CUDN (e.g., NetworkManager couldn't parse the NAD),
// this logs an error and enqueues the CUDN for reconciliation.
func (c *Controller) recoverEVPNIDs(cudnNADs cudnToNADs) {
	// Extract EVPN CUDNs with NADs into a slice for deterministic ordering.
	evpnCUDNs := make([]*cudnWithNADs, 0, len(cudnNADs))
	for _, entry := range cudnNADs {
		if entry.cudn.Spec.Network.Transport != userdefinednetworkv1.TransportOptionEVPN {
			continue
		}
		if len(entry.nads) == 0 {
			klog.V(4).Infof("EVPN CUDN %s has no NADs, skipping VID recovery", entry.cudn.Name)
			continue
		}
		evpnCUDNs = append(evpnCUDNs, entry)
	}

	// Sort by creation timestamp (oldest first) for deterministic conflict resolution.
	// When two CUDNs have conflicting VIDs, the oldest one wins.
	// Use name as tie-breaker when timestamps are equal for consistent ordering.
	slices.SortFunc(evpnCUDNs, func(a, b *cudnWithNADs) int {
		if a.cudn.CreationTimestamp.Before(&b.cudn.CreationTimestamp) {
			return -1
		}
		if b.cudn.CreationTimestamp.Before(&a.cudn.CreationTimestamp) {
			return 1
		}
		return strings.Compare(a.cudn.Name, b.cudn.Name)
	})

	for _, entry := range evpnCUDNs {
		if err := c.recoverEVPNIDsForCUDN(entry.cudn.Name); err != nil {
			klog.Errorf("VID recovery failed for EVPN CUDN %s: %v. "+
				"The CUDN will be reconciled and existing NAD VIDs will be preserved if possible.",
				entry.cudn.Name, err)
			c.cudnController.Reconcile(entry.cudn.Name)
		}
	}
}

// recoverEVPNIDsForCUDN attempts to recover VIDs and VNI reservations for a single CUDN using NetworkManager's cache.
// Returns nil if VIDs were successfully recovered or if no VIDs are allocated yet.
// Returns error if VID reservation fails (e.g., conflict with another network).
func (c *Controller) recoverEVPNIDsForCUDN(cudnName string) error {
	networkName := util.GenerateCUDNNetworkName(cudnName)

	// Use NetworkManager's cached NetInfo - it has already parsed the NAD
	netInfo := c.networkManager.GetNetwork(networkName)
	if netInfo == nil {
		// NetworkManager doesn't have this network cached. This can happen if:
		// - NetworkManager failed to parse the NAD (corrupted)
		// - NAD doesn't exist yet
		return fmt.Errorf("network %s not found in NetworkManager cache", networkName)
	}

	if err := c.reserveVNIs(cudnName, netInfo.EVPNVTEPName(), netInfo.EVPNMACVRFVNI(), netInfo.EVPNIPVRFVNI()); err != nil {
		return fmt.Errorf("failed to reserve VNIs for cudn %s: %w", cudnName, err)
	}

	macVRFVID := netInfo.EVPNMACVRFVID()
	ipVRFVID := netInfo.EVPNIPVRFVID()
	// Check if this network has EVPN VIDs allocated
	if macVRFVID == 0 && ipVRFVID == 0 {
		klog.V(4).Infof("EVPN CUDN %s has no VIDs allocated yet, skipping recovery", cudnName)
		return nil // No VIDs to recover
	}

	if err := c.reserveRecoveredVIDs(cudnName, macVRFVID, ipVRFVID); err != nil {
		return fmt.Errorf("failed to reserve VIDs for cudn %s: %w", cudnName, err)
	}

	klog.V(4).Infof("Recovered VIDs for CUDN %s (macVRF=%d, ipVRF=%d)", cudnName, macVRFVID, ipVRFVID)
	return nil
}

// reserveRecoveredVIDs reserves the given VIDs in the allocator for a network.
// VIDs of 0 are skipped (not allocated).
//
// Both VIDs are attempted even if one fails - this maximizes recovery and protects
// as many VIDs as possible. We don't release successfully reserved VIDs on partial
// failure because they represent state that already exists in NADs; releasing them
// could allow another network to "steal" the VID, causing route leakage.
func (c *Controller) reserveRecoveredVIDs(networkName string, macVRFVID, ipVRFVID int) error {
	var errs []error

	if macVRFVID > 0 {
		if err := c.vidAllocator.ReserveID(macVRFKey(networkName), macVRFVID); err != nil {
			errs = append(errs, fmt.Errorf("failed to reserve VID %d for MAC-VRF of network %s: %w", macVRFVID, networkName, err))
		} else {
			klog.V(4).Infof("Recovered VID %d for MAC-VRF of network %s", macVRFVID, networkName)
		}
	}
	if ipVRFVID > 0 {
		if err := c.vidAllocator.ReserveID(ipVRFKey(networkName), ipVRFVID); err != nil {
			errs = append(errs, fmt.Errorf("failed to reserve VID %d for IP-VRF of network %s: %w", ipVRFVID, networkName, err))
		} else {
			klog.V(4).Infof("Recovered VID %d for IP-VRF of network %s", ipVRFVID, networkName)
		}
	}

	return errors.Join(errs...)
}

// releaseEVPNIDsForNetwork releases the VIDs and VNI reservations for a network's VRFs.
//
// NOTE: VID release is not synchronized with node-side dataplane cleanup.
// In theory, a rapidly created new network could get the same VID while nodes
// are still tearing down the old network's bridge configuration. In practice,
// VID collisions are unlikely because the allocator is monotonic and won't
// reallocate the same VID unless the pool fills up or CUDNs are recycled rapidly.
// The actual mitigation is on the node-side: nodes should check for VID conflicts
// and refuse to configure a VID already in use by a different network, waiting
// until the old network is cleaned up.
func (c *Controller) releaseEVPNIDsForNetwork(networkName string) {
	macVID := c.vidAllocator.ReleaseID(macVRFKey(networkName))
	ipVID := c.vidAllocator.ReleaseID(ipVRFKey(networkName))
	if macVID >= 0 || ipVID >= 0 {
		klog.V(4).Infof("Released VIDs for network %s: MAC-VRF=%d, IP-VRF=%d", networkName, macVID, ipVID)
	}
	c.releaseVNIs(networkName)
}

// reserveVNIs reserves VNIs for a network within a VTEP scope, ensuring no two
// networks sharing the same VTEP use the same VNI.
func (c *Controller) reserveVNIs(networkName, vtepName string, macVRFVNI, ipVRFVNI int32) error {
	c.reservedVNIsLock.Lock()
	defer c.reservedVNIsLock.Unlock()

	var errs []error
	for _, vni := range []int32{macVRFVNI, ipVRFVNI} {
		if vni == 0 {
			continue
		}
		key := vniKey{vtep: vtepName, vni: vni}
		if owner, exists := c.reservedVNIs[key]; exists && owner != networkName {
			errs = append(errs, fmt.Errorf("VNI %d on VTEP %q is already reserved by network %q", vni, vtepName, owner))
			continue
		}
		c.reservedVNIs[key] = networkName
	}
	return errors.Join(errs...)
}

// releaseVNIs releases all VNIs owned by the given network.
func (c *Controller) releaseVNIs(networkName string) {
	c.reservedVNIsLock.Lock()
	defer c.reservedVNIsLock.Unlock()

	for vni, owner := range c.reservedVNIs {
		if owner == networkName {
			delete(c.reservedVNIs, vni)
		}
	}
}

func (c *Controller) Shutdown() {
	controllers := []controller.Reconciler{
		c.cudnController,
		c.udnController,
		c.nadNotifier.Controller,
		c.namespaceNotifier.Controller,
	}
	if c.vtepNotifier != nil {
		controllers = append(controllers, c.vtepNotifier.Controller)
	}
	if c.raNotifier != nil {
		controllers = append(controllers, c.raNotifier.Controller)
	}
	controller.Stop(controllers...)
}

// ReconcileNetAttachDef enqueue NAD requests following NAD events.
func (c *Controller) ReconcileNetAttachDef(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split meta namespace key %q: %v", key, err)
	}
	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get NetworkAttachmentDefinition %q from cache: %v", key, err)
	}
	ownerRef := metav1.GetControllerOf(nad)
	if ownerRef == nil {
		return nil
	}

	switch ownerRef.Kind {
	case "ClusterUserDefinedNetwork":
		owner, err := c.cudnLister.Get(ownerRef.Name)
		if err != nil {
			return fmt.Errorf("failed to get ClusterUserDefinedNetwork %q from cache: %v", ownerRef.Name, err)
		}
		ownerKey, err := cache.MetaNamespaceKeyFunc(owner)
		if err != nil {
			return fmt.Errorf("failed to generate meta namespace key for CUDN: %v", err)
		}
		c.cudnController.Reconcile(ownerKey)
	case "UserDefinedNetwork":
		owner, err := c.udnLister.UserDefinedNetworks(nad.Namespace).Get(ownerRef.Name)
		if err != nil {
			return fmt.Errorf("failed to get UserDefinedNetwork %q from cache: %v", ownerRef.Name, err)
		}
		ownerKey, err := cache.MetaNamespaceKeyFunc(owner)
		if err != nil {
			return fmt.Errorf("failed to generate meta namespace key for UDN: %v", err)
		}
		c.udnController.Reconcile(ownerKey)
	default:
		return nil
	}
	return nil
}

// ReconcileNamespace enqueue relevant Cluster UDN CR requests following namespace events.
func (c *Controller) ReconcileNamespace(key string) error {
	namespace, err := c.namespaceInformer.Lister().Get(key)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get namespace %q from cache: %w", key, err)
	}

	var namespaceLabels labels.Set
	if namespace != nil {
		namespaceLabels = namespace.Labels
	}

	c.namespaceTrackerLock.RLock()
	defer c.namespaceTrackerLock.RUnlock()

	for cudnName, affectedNamespaces := range c.namespaceTracker {
		affectedNamespace := affectedNamespaces.Has(key)

		// For deleted namespaces, only reconcile if tracked
		if namespace == nil {
			if affectedNamespace {
				klog.Errorf("BUG: namespace %q was deleted but still tracked by ClusterUDN %q, forcing reconcile to cleanup", key, cudnName)
				c.cudnController.Reconcile(cudnName)
			}
			continue
		}

		selectedNamespace := false
		if !affectedNamespace {
			cudn, err := c.cudnLister.Get(cudnName)
			if err != nil {
				return fmt.Errorf("failed to get CUDN %q from cache: %w", cudnName, err)
			}
			cudnSelector, err := metav1.LabelSelectorAsSelector(&cudn.Spec.NamespaceSelector)
			if err != nil {
				return fmt.Errorf("failed to convert CUDN namespace selector: %w", err)
			}
			selectedNamespace = cudnSelector.Matches(namespaceLabels)
		}

		if affectedNamespace || selectedNamespace {
			klog.Infof("Enqueue ClusterUDN %q following namespace %q event", cudnName, key)
			c.cudnController.Reconcile(cudnName)
		}
	}

	return nil
}

// UpdateSubsystemCondition may be used by other controllers handling UDN/CUDN/NAD/network setup to report conditions that
// may affect UDN/CUDN functionality.
// FieldManager should be unique for every subsystem.
// If given network is not managed by a UDN/CUDN, no condition will be reported and no error will be returned.
// Events may be used to report additional information about the condition to avoid overloading the condition message.
// When condition should not change, but new events should be reported, pass condition = nil.
func (c *Controller) UpdateSubsystemCondition(
	networkName string,
	fieldManager string,
	condition *metav1.Condition,
	events ...*util.EventDetails,
) error {
	// try to find udn/cudn using network name
	udnNamespace, udnName := util.ParseNetworkName(networkName)
	if udnName == "" {
		return nil
	}

	var obj runtime.Object
	var err error
	if udnNamespace == "" {
		obj, err = c.cudnLister.Get(udnName)
	} else {
		obj, err = c.udnLister.UserDefinedNetworks(udnNamespace).Get(udnName)
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get (Cluster)UserDefinedNetwork %s/%s from cache: %w", udnNamespace, udnName, err)
	}

	objRef, err := reference.GetReference(userdefinednetworkscheme.Scheme, obj)
	if err != nil {
		return fmt.Errorf("failed to get object reference for (Cluster)UserDefinedNetwork %s/%s: %w", udnNamespace, udnName, err)
	}
	for _, event := range events {
		c.eventRecorder.Event(objRef, event.EventType, event.Reason, event.Note)
	}

	if condition == nil {
		return nil
	}

	applyCondition := &metaapplyv1.ConditionApplyConfiguration{
		Type:               &condition.Type,
		Status:             &condition.Status,
		LastTransitionTime: &condition.LastTransitionTime,
		Reason:             &condition.Reason,
		Message:            &condition.Message,
	}

	opts := metav1.ApplyOptions{
		FieldManager: fieldManager,
		Force:        true,
	}

	if udnNamespace == "" {
		applyConf := udnapplyconfkv1.ClusterUserDefinedNetwork(udnName).
			WithStatus(udnapplyconfkv1.ClusterUserDefinedNetworkStatus().WithConditions(applyCondition))
		_, err = c.udnClient.K8sV1().ClusterUserDefinedNetworks().ApplyStatus(context.Background(), applyConf, opts)
	} else {
		udnStatus := udnapplyconfkv1.UserDefinedNetworkStatus().WithConditions(applyCondition)
		applyUDN := udnapplyconfkv1.UserDefinedNetwork(udnName, udnNamespace).WithStatus(udnStatus)
		_, err = c.udnClient.K8sV1().UserDefinedNetworks(udnNamespace).ApplyStatus(context.Background(), applyUDN, opts)
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to update (Cluster)UserDefinedNetwork %s/%s status: %w", udnNamespace, udnName, err)
	}
	return nil
}

func (c *Controller) udnNeedUpdate(_, _ *userdefinednetworkv1.UserDefinedNetwork) bool {
	return true
}

// reconcileUDN get UserDefinedNetwork CR key and reconcile it according to spec.
// It creates NAD according to spec at the namespace the CR resides.
// The NAD objects are created with the same key as the request CR, having both kinds have the same key enable
// the controller to act on NAD changes as well and reconciles NAD objects (e.g: in case NAD is deleted it will be re-created).
func (c *Controller) reconcileUDN(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	udn, err := c.udnLister.UserDefinedNetworks(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get UserDefinedNetwork %q from cache: %v", key, err)
	}

	udnCopy := udn.DeepCopy()

	nadCopy, syncErr := c.syncUserDefinedNetwork(udnCopy)

	updateStatusErr := c.updateUserDefinedNetworkStatus(udnCopy, nadCopy, syncErr)

	var networkInUse *networkInUseError
	if errors.As(syncErr, &networkInUse) {
		// Call ReconcileRateLimited directly to ensure retries without the default limits
		c.udnController.ReconcileRateLimited(key)
		return updateStatusErr
	}

	return errors.Join(syncErr, updateStatusErr)
}

func (c *Controller) syncUserDefinedNetwork(udn *userdefinednetworkv1.UserDefinedNetwork) (*netv1.NetworkAttachmentDefinition, error) {
	if udn == nil {
		return nil, nil
	}

	var role, topology string
	if udn.Spec.Layer2 != nil {
		role = string(udn.Spec.Layer2.Role)
	} else if udn.Spec.Layer3 != nil {
		role = string(udn.Spec.Layer3.Role)
	}
	topology = string(udn.Spec.Topology)

	if !udn.DeletionTimestamp.IsZero() { // udn is being  deleted
		if controllerutil.ContainsFinalizer(udn, template.FinalizerUserDefinedNetwork) {
			if err := c.deleteNAD(udn, udn.Namespace); err != nil {
				return nil, fmt.Errorf("failed to delete NetworkAttachmentDefinition [%s/%s]: %w", udn.Namespace, udn.Name, err)
			}

			// Ensure that the network controller is stopped(GetActiveNetwork returns nil) before allowing the UDN
			// to be removed.
			if c.networkManager.GetActiveNetwork(util.GenerateUDNNetworkName(udn.Namespace, udn.Name)) != nil {
				return nil, &networkInUseError{err: fmt.Errorf("cannot remove UDN, controller for network %s is still running", util.GenerateUDNNetworkName(udn.Namespace, udn.Name))}
			}

			controllerutil.RemoveFinalizer(udn, template.FinalizerUserDefinedNetwork)
			udn, err := c.udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Update(context.Background(), udn, metav1.UpdateOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to remove finalizer to UserDefinedNetwork: %w", err)
			}
			klog.Infof("Finalizer removed from UserDefinedNetworks [%s/%s]", udn.Namespace, udn.Name)
			metrics.DecrementUDNCount(role, topology)
			metrics.DeleteDynamicUDNNodeCount(util.GenerateUDNNetworkName(udn.Namespace, udn.Name))

			if config.Metrics.EnableScaleMetrics {
				// Clean up UDN metric time series to prevent cardinality explosion
				networkName := util.GenerateUDNNetworkName(udn.Namespace, udn.Name)
				metrics.CleanupUDNMetrics(networkName)
			}
		}

		return nil, nil
	}

	if finalizerAdded := controllerutil.AddFinalizer(udn, template.FinalizerUserDefinedNetwork); finalizerAdded {
		udn, err := c.udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Update(context.Background(), udn, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to add finalizer to UserDefinedNetwork: %w", err)
		}
		klog.Infof("Added Finalizer to UserDefinedNetwork [%s/%s]", udn.Namespace, udn.Name)
		metrics.IncrementUDNCount(role, topology)
	}

	return c.updateNAD(udn, udn.Namespace)
}

func (c *Controller) updateUserDefinedNetworkStatus(udn *userdefinednetworkv1.UserDefinedNetwork, nad *netv1.NetworkAttachmentDefinition, syncError error) error {
	if udn == nil {
		return nil
	}

	networkCreatedCondition := newNetworkCreatedCondition(nad, syncError)

	updated := meta.SetStatusCondition(&udn.Status.Conditions, *networkCreatedCondition)

	vrfName := udnVRFName(udn, nad, syncError, nad != nil || c.nadStillExists(udn.Namespace, udn.Name))
	vrfNameUpdated := vrfName != ptr.Deref(udn.Status.VRFName, "")

	if !updated && !vrfNameUpdated {
		return nil
	}

	var err error
	conditionsApply := make([]*metaapplyv1.ConditionApplyConfiguration, len(udn.Status.Conditions))
	for i, condition := range udn.Status.Conditions {
		conditionsApply[i] = &metaapplyv1.ConditionApplyConfiguration{
			Type:               &condition.Type,
			Status:             &condition.Status,
			LastTransitionTime: &condition.LastTransitionTime,
			Reason:             &condition.Reason,
			Message:            &condition.Message,
		}
	}
	statusApply := udnapplyconfkv1.UserDefinedNetworkStatus().
		WithConditions(conditionsApply...)
	if vrfName != "" {
		statusApply = statusApply.WithVRFName(vrfName)
	}
	udnApplyConf := udnapplyconfkv1.UserDefinedNetwork(udn.Name, udn.Namespace).
		WithStatus(statusApply)
	opts := metav1.ApplyOptions{FieldManager: "user-defined-network-controller"}
	udn, err = c.udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).ApplyStatus(context.Background(), udnApplyConf, opts)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to update UserDefinedNetwork status: %w", err)
	}
	klog.Infof("Updated status UserDefinedNetwork [%s/%s]", udn.Namespace, udn.Name)
	return nil
}

func newNetworkCreatedCondition(nad *netv1.NetworkAttachmentDefinition, syncError error) *metav1.Condition {
	now := metav1.Now()
	networkCreatedCondition := &metav1.Condition{
		Type:               conditionTypeNetworkCreated,
		Status:             metav1.ConditionTrue,
		Reason:             reasonNADCreated,
		Message:            "NetworkAttachmentDefinition has been created",
		LastTransitionTime: now,
	}

	if nad != nil && !nad.DeletionTimestamp.IsZero() {
		networkCreatedCondition.Status = metav1.ConditionFalse
		networkCreatedCondition.Reason = reasonNADDeleted
		networkCreatedCondition.Message = "NetworkAttachmentDefinition is being deleted"
	}
	if syncError != nil {
		networkCreatedCondition.Status = metav1.ConditionFalse
		networkCreatedCondition.Reason = reasonSyncError
		networkCreatedCondition.Message = syncError.Error()
	}

	return networkCreatedCondition
}

func (c *Controller) cudnNeedUpdate(_ *userdefinednetworkv1.ClusterUserDefinedNetwork, _ *userdefinednetworkv1.ClusterUserDefinedNetwork) bool {
	return true
}

// reconcileCUDN get ClusterUserDefinedNetwork CR key and reconcile it according to spec.
// It creates NADs according to spec at the specified selected namespaces.
// The NAD objects are created with the same key as the request CR, having both kinds have the same key enable
// the controller to act on NAD changes as well and reconciles NAD objects (e.g: in case NAD is deleted it will be re-created).
func (c *Controller) reconcileCUDN(key string) error {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	cudn, err := c.cudnLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get ClusterUserDefinedNetwork %q from cache: %v", key, err)
	}

	cudnCopy := cudn.DeepCopy()

	// Validate EVPN configuration once and pass the result to both sync and
	// transport status so they see the same VTEP snapshot.
	var evpnErr error
	if cudnCopy != nil {
		evpnErr = c.validateEVPN(cudnCopy)
	}

	nads, syncErr := c.syncClusterUDN(cudnCopy, evpnErr)

	// Set transport status condition (TransportAccepted) on cudnCopy
	// The actual status update will be performed by updateClusterUDNStatus() below
	transportUpdated, transportErr := c.setTransportStatusCondition(cudnCopy, evpnErr)
	if transportErr != nil {
		return fmt.Errorf("failed to validate transport for ClusterUserDefinedNetwork %q: %v", cudnCopy.Name, transportErr)
	}

	// Update status with ALL conditions (TransportAccepted + NetworkCreated) in a single API call
	updateStatusErr := c.updateClusterUDNStatus(cudnCopy, nads, syncErr, transportUpdated)

	var networkInUse *networkInUseError
	if errors.As(syncErr, &networkInUse) {
		// Call ReconcileRateLimited directly to ensure retries without the default limits
		c.cudnController.ReconcileRateLimited(key)
		return updateStatusErr
	}

	// vtepNotFoundError / vtepNotAcceptedError are non-fatal: the status has
	// been updated to reflect the VTEP issue, and the VTEPNotifier will
	// re-queue this CUDN when the VTEP is created or becomes accepted.
	var vtepNotFound *vtepNotFoundError
	var vtepNotAccepted *vtepNotAcceptedError
	if errors.As(syncErr, &vtepNotFound) || errors.As(syncErr, &vtepNotAccepted) {
		return updateStatusErr
	}

	return errors.Join(syncErr, updateStatusErr)
}

func (c *Controller) syncClusterUDN(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, evpnErr error) ([]netv1.NetworkAttachmentDefinition, error) {
	c.namespaceTrackerLock.Lock()
	defer c.namespaceTrackerLock.Unlock()

	if cudn == nil {
		return nil, nil
	}

	cudnName := cudn.Name
	affectedNamespaces := c.namespaceTracker[cudnName]

	if !cudn.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(cudn, template.FinalizerUserDefinedNetwork) {
			var errs []error
			for nsToDelete := range affectedNamespaces {
				if err := c.deleteNAD(cudn, nsToDelete); err != nil {
					errs = append(errs, fmt.Errorf("failed to delete NetworkAttachmentDefinition [%s/%s]: %w",
						nsToDelete, cudnName, err))
				} else {
					c.namespaceTracker[cudnName].Delete(nsToDelete)
				}
			}

			if len(errs) > 0 {
				return nil, errors.Join(errs...)
			}

			// Ensure that the network controller is stopped(GetActiveNetwork returns nil) before allowing the cUDN
			// to be removed.
			if c.networkManager.GetActiveNetwork(util.GenerateCUDNNetworkName(cudn.Name)) != nil {
				return nil, &networkInUseError{err: fmt.Errorf("cannot remove cluster UDN, controller for network %s is still running", util.GenerateCUDNNetworkName(cudn.Name))}
			}

			var err error
			controllerutil.RemoveFinalizer(cudn, template.FinalizerUserDefinedNetwork)
			cudn, err = c.udnClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudn, metav1.UpdateOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to remove finalizer from ClusterUserDefinedNetwork %q: %w",
					cudnName, err)
			}
			klog.Infof("Finalizer removed from ClusterUserDefinedNetwork %q", cudn.Name)
			delete(c.namespaceTracker, cudnName)
			c.cudnMetricUncounted(cudnName, &cudn.Spec.Network)
			metrics.DeleteCUDNCondition(cudnName)
			metrics.DeleteDynamicUDNNodeCount(util.GenerateCUDNNetworkName(cudn.Name))
			c.releaseEVPNIDsForNetwork(cudnName)
		}

		return nil, nil
	}

	// The Uplink is always a part of the CUDN CRD, but it is optional.
	// Reject a CUDN that references an Uplink while the feature is disabled instead of silently ignoring spec.uplinks.
	if len(cudn.Spec.Uplinks) > 0 && !util.IsUplinkEnabled() {
		return nil, fmt.Errorf("spec.uplinks is set but the Uplink feature is disabled, ignoring this CUDN")
	}

	if _, exist := c.namespaceTracker[cudnName]; !exist {
		c.namespaceTracker[cudnName] = sets.Set[string]{}
	}

	if finalizerAdded := controllerutil.AddFinalizer(cudn, template.FinalizerUserDefinedNetwork); finalizerAdded {
		var err error
		cudn, err = c.udnClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudn, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to add finalizer to ClusterUserDefinedNetwork %q: %w", cudnName, err)
		}
		klog.Infof("Added Finalizer to ClusterUserDefinedNetwork %q", cudnName)
		c.cudnMetricCounted(cudnName, &cudn.Spec.Network)
	}

	if evpnErr != nil {
		var vtepNA *vtepNotAcceptedError
		if errors.As(evpnErr, &vtepNA) && len(c.namespaceTracker[cudnName]) > 0 {
			// NADs already exist — don't tear down the working network,
			// but also don't propagate any further changes (spec updates,
			// new namespaces, etc.) while the VTEP is unhealthy.
			// Return the existing NADs so NetworkCreated stays True.
			klog.Warningf("VTEP %q is not accepted; freezing NADs for ClusterUserDefinedNetwork %q until VTEP is accepted again", vtepNA.vtepName, cudnName)
			return c.getExistingNADs(cudn, c.namespaceTracker[cudnName])
		}
		return nil, evpnErr
	}

	selectedNamespaces, err := c.getSelectedNamespaces(cudn.Spec.NamespaceSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to get selected namespaces: %w", err)
	}

	var errs []error
	for nsToDelete := range affectedNamespaces.Difference(selectedNamespaces) {
		if err := c.deleteNAD(cudn, nsToDelete); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete NetworkAttachmentDefinition [%s/%s]: %w",
				nsToDelete, cudnName, err))
		} else {
			c.namespaceTracker[cudnName].Delete(nsToDelete)
		}
	}

	var nads []netv1.NetworkAttachmentDefinition
	for nsToUpdate := range selectedNamespaces {
		nad, err := c.updateNAD(cudn, nsToUpdate)
		if err != nil {
			errs = append(errs, err)
		} else {
			c.namespaceTracker[cudn.Name].Insert(nsToUpdate)
			nads = append(nads, *nad)
		}
	}

	return nads, errors.Join(errs...)
}

// getExistingNADs retrieves the current NADs for a CUDN from the cache for the
// tracked namespaces. Used to return existing NADs without modifying them when
// further reconciliation should be blocked (e.g. VTEP not accepted).
func (c *Controller) getExistingNADs(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, trackedNamespaces sets.Set[string]) ([]netv1.NetworkAttachmentDefinition, error) {
	var nads []netv1.NetworkAttachmentDefinition
	var errs []error
	for ns := range trackedNamespaces {
		nad, err := c.nadLister.NetworkAttachmentDefinitions(ns).Get(cudn.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("namespace %s: %w", ns, err))
			continue
		}
		nads = append(nads, *nad)
	}
	return nads, errors.Join(errs...)
}

// getSelectedNamespaces list all selected namespaces according to given selector and create
// a set of the selected namespaces keys.
func (c *Controller) getSelectedNamespaces(sel metav1.LabelSelector) (sets.Set[string], error) {
	selectedNamespaces := sets.Set[string]{}
	labelSelector, err := metav1.LabelSelectorAsSelector(&sel)
	if err != nil {
		return nil, fmt.Errorf("failed to create label-selector: %w", err)
	}
	selectedNamespacesList, err := c.namespaceInformer.Lister().List(labelSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}
	for _, selectedNs := range selectedNamespacesList {
		if !selectedNs.DeletionTimestamp.IsZero() {
			klog.V(5).Infof("Namespace %s is being deleted, skipping", selectedNs.Name)
			continue
		}
		selectedNamespaces.Insert(selectedNs.Name)
	}
	return selectedNamespaces, nil
}

func (c *Controller) updateClusterUDNStatus(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, nads []netv1.NetworkAttachmentDefinition, syncError error, transportUpdated bool) error {
	if cudn == nil {
		return nil
	}

	// sort NADs by namespace names to avoid redundant updated due to inconsistent ordering
	slices.SortFunc(nads, func(a, b netv1.NetworkAttachmentDefinition) int {
		return strings.Compare(a.Namespace, b.Namespace)
	})

	networkCreatedCondition := newClusterNetworkCreatedCondition(nads, syncError)

	networkCreatedOrUpdated := meta.SetStatusCondition(&cudn.Status.Conditions, networkCreatedCondition)

	vrfName := clusterUDNVRFName(cudn, nads, syncError, c.hasTrackedNamespaces(cudn.Name))
	vrfNameUpdated := vrfName != ptr.Deref(cudn.Status.VRFName, "")

	// Apply status if the VRF name, the NetworkCreated condition or the TransportAccepted condition changed
	if !networkCreatedOrUpdated && !transportUpdated && !vrfNameUpdated {
		// Record the metric from the existing API-confirmed conditions so it is
		// populated after controller restarts, where the informer fires synthetic
		// creates for all CUDNs but the conditions haven't changed.
		recordCUDNConditionMetrics(cudn)
		return nil
	}
	conditionsApply := make([]*metaapplyv1.ConditionApplyConfiguration, len(cudn.Status.Conditions))
	for i, condition := range cudn.Status.Conditions {
		conditionsApply[i] = &metaapplyv1.ConditionApplyConfiguration{
			Type:               &condition.Type,
			Status:             &condition.Status,
			LastTransitionTime: &condition.LastTransitionTime,
			Reason:             &condition.Reason,
			Message:            &condition.Message,
		}
	}
	statusApply := udnapplyconfkv1.ClusterUserDefinedNetworkStatus().
		WithConditions(conditionsApply...)
	if vrfName != "" {
		statusApply = statusApply.WithVRFName(vrfName)
	}
	var err error
	applyConf := udnapplyconfkv1.ClusterUserDefinedNetwork(cudn.Name).
		WithStatus(statusApply)
	opts := metav1.ApplyOptions{FieldManager: "user-defined-network-controller"}
	cudnName := cudn.Name
	cudn, err = c.udnClient.K8sV1().ClusterUserDefinedNetworks().ApplyStatus(context.Background(), applyConf, opts)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to update ClusterUserDefinedNetwork status %q: %w", cudnName, err)
	}
	klog.Infof("Updated status ClusterUserDefinedNetwork %q", cudn.Name)
	recordCUDNConditionMetrics(cudn)

	return nil
}

// clusterUDNVRFName returns the VRF device name to publish on the CUDN
// status, or an empty string when no name should be published. Only primary
// CUDNs publish a name, since those are the networks that get a VRF device on
// the nodes and whose VRF external consumers (e.g. FRRConfiguration authors)
// must name. nads are the NADs the last sync returned for the CUDN: they
// carry the network-id annotation the ID-derived fallback name is built from.
// nadsRemain reports whether any NAD is known to still exist when the sync
// returned none, distinguishing a sync that failed before determining the
// NADs (the published name is preserved) from one that ran after the last NAD
// was deleted (the name is cleared, since the released network ID may be
// reallocated). The name states how the VRF is named, not whether the network
// is ready: readiness is what the conditions report.
func clusterUDNVRFName(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, nads []netv1.NetworkAttachmentDefinition, syncError error, nadsRemain bool) string {
	if !utiludn.IsPrimaryNetwork(&cudn.Spec.Network) {
		return ""
	}
	if len(nads) == 0 {
		if syncError != nil && nadsRemain {
			// the sync failed but NADs are known to remain: keep the published
			// name rather than clearing it on incomplete information
			return ptr.Deref(cudn.Status.VRFName, "")
		}
		// no NAD is left, whether the sync fully succeeded or failed after
		// deleting the last one: the network is gone and its released network
		// ID may be reallocated to another network, so a stale name could
		// point consumers at that network's VRF; clear it
		return ""
	}
	vrfName := util.GetCUDNVRFName(cudn.Name, networkIDFromNADs(nads))
	if vrfName == "" {
		// the name needs the network ID and the NADs are not annotated with
		// it yet: keep the published name; the NAD update that adds the
		// annotation triggers another reconcile
		return ptr.Deref(cudn.Status.VRFName, "")
	}
	return vrfName
}

// udnVRFName returns the VRF device name to publish on the UDN status, given
// the NAD and the error the last sync returned, and whether the NAD is known
// to still exist, or an empty string when no name should be published. Only
// primary UDNs publish a name, since those are the networks that get a VRF
// device on the nodes. The name of a namespaced UDN is always ID-derived, so
// it needs the network ID that the NAD controller annotates on the NAD
// asynchronously; the lifecycle mirrors clusterUDNVRFName: preserve the
// published name while the NAD exists without the annotation or while a sync
// fails with the NAD still in place, clear it once the NAD is gone.
func udnVRFName(udn *userdefinednetworkv1.UserDefinedNetwork, nad *netv1.NetworkAttachmentDefinition, syncError error, nadExists bool) string {
	if !utiludn.IsPrimaryNetwork(&udn.Spec) {
		return ""
	}
	if nad == nil {
		if syncError != nil && nadExists {
			// the sync failed but the NAD is known to remain: keep the
			// published name rather than clearing it on incomplete information
			return ptr.Deref(udn.Status.VRFName, "")
		}
		// the NAD is gone, whether the sync fully succeeded or failed after
		// deleting it (e.g. on the finalizer removal): the network ID is
		// released and may be reallocated to another network; clear the name
		return ""
	}
	vrfName := util.GetUDNVRFName(networkIDFromNADs([]netv1.NetworkAttachmentDefinition{*nad}))
	if vrfName == "" {
		// the NAD is not annotated with the network ID yet: keep the
		// published name; the NAD update that adds the annotation triggers
		// another reconcile
		return ptr.Deref(udn.Status.VRFName, "")
	}
	return vrfName
}

// nadStillExists reports whether the UDN's NAD is still present in the
// informer cache; only a NotFound answer establishes known absence, any
// other lookup failure counts as unknown, hence as still existing.
func (c *Controller) nadStillExists(namespace, name string) bool {
	_, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	return !apierrors.IsNotFound(err)
}

// hasTrackedNamespaces reports whether any NAD is known to remain for the
// given CUDN. The namespace tracker is maintained per NAD operation, so after
// a partial sync it reflects what actually survived it.
func (c *Controller) hasTrackedNamespaces(cudnName string) bool {
	c.namespaceTrackerLock.RLock()
	defer c.namespaceTrackerLock.RUnlock()
	return c.namespaceTracker[cudnName].Len() > 0
}

// networkIDFromNADs returns the network ID annotated on any of the given NADs,
// or InvalidID when none of them carries a network-id annotation that the
// allocator could have assigned to a CUDN: DefaultNetworkID is reserved for
// the default network and MaxNetworks bounds the allocator.
func networkIDFromNADs(nads []netv1.NetworkAttachmentDefinition) int {
	for i := range nads {
		annotation := nads[i].Annotations[ovntypes.OvnNetworkIDAnnotation]
		if annotation == "" {
			continue
		}
		if id, err := strconv.Atoi(annotation); err == nil && id > ovntypes.DefaultNetworkID && id < networkmanager.MaxNetworks {
			return id
		}
		klog.Warningf("Ignoring invalid network-id annotation %q on NetworkAttachmentDefinition %s/%s",
			annotation, nads[i].Namespace, nads[i].Name)
	}
	return ovntypes.InvalidID
}

func newClusterNetworkCreatedCondition(nads []netv1.NetworkAttachmentDefinition, syncError error) metav1.Condition {
	var namespaces []string
	for _, nad := range nads {
		namespaces = append(namespaces, nad.Namespace)
	}
	affectedNamespaces := strings.Join(namespaces, ", ")

	now := metav1.Now()
	condition := metav1.Condition{
		Type:               conditionTypeNetworkCreated,
		Status:             metav1.ConditionTrue,
		Reason:             reasonNADCreated,
		Message:            fmt.Sprintf("NetworkAttachmentDefinition has been created in following namespaces: [%s]", affectedNamespaces),
		LastTransitionTime: now,
	}

	var deletedNadKeys []string
	for _, nad := range nads {
		if nad.DeletionTimestamp != nil {
			deletedNadKeys = append(deletedNadKeys, nad.Namespace+"/"+nad.Name)
		}
	}
	if len(deletedNadKeys) > 0 {
		condition.Status = metav1.ConditionFalse
		condition.Reason = reasonNADDeleted
		condition.Message = fmt.Sprintf("NetworkAttachmentDefinition are being deleted: %v", deletedNadKeys)
	}

	if syncError != nil {
		condition.Status = metav1.ConditionFalse

		// Check for specific error types to provide better status reasons
		var vtepNotFound *vtepNotFoundError
		var vtepNotAccepted *vtepNotAcceptedError
		var evpnCfgErr *evpnConfigError
		if errors.As(syncError, &vtepNotFound) {
			condition.Reason = ReasonVTEPNotFound
			condition.Message = fmt.Sprintf("Cannot create network: VTEP '%s' does not exist. "+
				"Create the VTEP CR first or update the CUDN to reference an existing VTEP.",
				vtepNotFound.vtepName)
		} else if errors.As(syncError, &vtepNotAccepted) {
			condition.Reason = ReasonVTEPNotAccepted
			condition.Message = fmt.Sprintf("Cannot create network: VTEP '%s' exists but is not accepted. "+
				"The VTEP must be in Accepted status before the network can be created.",
				vtepNotAccepted.vtepName)
		} else if errors.As(syncError, &evpnCfgErr) {
			condition.Reason = ReasonEVPNConfigError
			condition.Message = evpnCfgErr.Error()
		} else {
			condition.Reason = reasonNADSyncError
			condition.Message = syncError.Error()
		}
	}

	return condition
}

// validateEVPN validates EVPN configuration for a CUDN.
// Returns an error if EVPN is requested but disabled, if the referenced VTEP
// doesn't exist, or if the VTEP is not yet accepted.
func (c *Controller) validateEVPN(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) error {
	if cudn.Spec.Network.Transport != userdefinednetworkv1.TransportOptionEVPN {
		return nil // Not an EVPN network
	}

	if !config.OVNKubernetesFeature.EnableEVPN {
		return &evpnConfigError{msg: "EVPN transport requested but EVPN feature is not enabled"}
	}
	if config.Gateway.Mode != config.GatewayModeLocal {
		return &evpnConfigError{msg: "EVPN transport requested but EVPN feature is only supported in local gateway mode"}
	}

	return c.validateVTEP(cudn)
}

// ReconcileVTEP handles VTEP events by re-queuing all CUDNs that reference the VTEP.
//
// This uses O(n) iteration over all CUDNs rather than maintaining an index because:
// VTEP create/delete events are expected to be rare; scanning all CUDNs from the
// informer cache keeps the logic simple. If this becomes a hot path at large
// CUDN counts, add an informer indexer keyed by VTEP.
func (c *Controller) ReconcileVTEP(vtepName string) error {
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CUDNs: %w", err)
	}

	for _, cudn := range cudns {
		if cudnReferencesVTEP(cudn, vtepName) {
			klog.V(4).InfoS("Re-queueing CUDN following VTEP event", "cudn", cudn.Name, "vtep", vtepName)
			c.cudnController.Reconcile(cudn.Name)
		}
	}

	return nil
}

// cudnReferencesVTEP returns true if the CUDN is an EVPN network referencing the given VTEP.
// CEL validation ensures EVPN is set when transport is EVPN.
func cudnReferencesVTEP(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, vtepName string) bool {
	if cudn.Spec.Network.Transport != userdefinednetworkv1.TransportOptionEVPN {
		return false
	}
	return cudn.Spec.Network.EVPN.VTEP == vtepName
}

// ReconcileRouteAdvertisements handles RouteAdvertisements events by re-queuing CUDNs
// whose transport validation depends on RouteAdvertisements.
// This is called when a RouteAdvertisements CR is created, updated, or deleted.
func (c *Controller) ReconcileRouteAdvertisements(raName string) error {
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CUDNs: %w", err)
	}

	// Requeue all no-overlay/EVPN CUDNs. Filtering on what the RA currently selects
	// would miss CUDNs that an update deselected (or that a deleted RA selected), whose
	// acceptance status may need to change as a result; with only the RA name available
	// there is no way to tell which CUDNs those were. RA events are rare and the
	// notifier already filters out changes irrelevant to transport validation.
	for _, cudn := range cudns {
		transport := cudn.Spec.Network.GetTransport()
		if transport == userdefinednetworkv1.TransportOptionNoOverlay || transport == userdefinednetworkv1.TransportOptionEVPN {
			klog.V(4).InfoS("Re-queueing CUDN following RouteAdvertisements event", "cudn", cudn.Name, "ra", raName, "transport", transport)
			c.cudnController.Reconcile(cudn.Name)
		}
	}

	return nil
}

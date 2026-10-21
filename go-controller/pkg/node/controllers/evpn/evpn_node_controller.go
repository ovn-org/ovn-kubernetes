// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package evpn

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/netlinkdevicemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// nodeAddressManager provides access to the node's IP addresses
// and allows registering callbacks for address changes.
type nodeAddressManager interface {
	ListAddresses() ([]net.IP, []*net.IPNet)
	AddOnAddressesChangedHandler(handler func())
}

const (
	ovsBridgeInt = "br-int"

	// reconcileNodeAddressChange is a synthetic key enqueued into the VTEP
	// controller workqueue when node addresses change. It triggers
	// reconciliation of unmanaged VTEPs whose annotated IPs are stale.
	reconcileNodeAddressChange = "//node-address-change"

	// reconcileVTEPAnnotationChange is a synthetic key enqueued when the
	// node's VTEP annotation is externally modified or removed. It
	// invalidates the annotation cache and triggers reconciliation to
	// restore the expected state.
	reconcileVTEPAnnotationChange = "//vtep-annotation-change"

	// vtepAnnotationFieldManager identifies this controller as the owner of
	// the VTEP annotation on the node, used to detect external modifications.
	vtepAnnotationFieldManager = "node-vtep-controller"
)

type Controller struct {
	nodeName       string
	watchFactory   factory.NodeWatchFactory
	kube           kube.Interface
	networkMgr     networkmanager.Interface
	ndm            netlinkdevicemanager.Interface
	ovsClient      libovsdbclient.Client
	addressManager nodeAddressManager

	// vtepController reconciles VTEP CRs: ensures bridge, VXLAN, SVI, and OVS port
	// lifecycle for each VTEP assigned to this node.
	vtepController controller.Controller
	// nodeEventHandler watches for node annotation changes that should
	// trigger VTEP reconciliation if needed.
	nodeEventHandler cache.ResourceEventHandlerRegistration
	// nadReconciler triggers VTEP reconciliation when NADs referencing a VTEP
	// are added or removed, so VID/VNI mappings and SVIs stay in sync.
	nadReconciler   controller.Reconciler
	nadReconcilerID uint64

	// Cache NAD key -> VTEP name for cleanup when NADs are deleted.
	nadVTEPInfoLock sync.Mutex
	nadVTEPInfo     map[string]string

	// svisByBridge tracks SVI names created per bridge, so we can detect
	// stale SVIs
	svisByBridgeLock sync.Mutex
	svisByBridge     map[string]sets.Set[string]

	// vtepsAnnotation is a controller-local copy of the node's VTEP annotation,
	// used instead of the informer cache to avoid stale reads when multiple
	// VTEPs are reconciled in sequence before the cache catches up.
	vtepsAnnotation map[string]util.VTEPNodeAnnotation

	// podController manages static FDB and neighbor entries for local pods
	// on EVPN networks, enabling ARP/ND suppression and known-unicast forwarding.
	podController controller.Controller
	podLister     corelisters.PodLister
	podNeighLock  sync.Mutex
	podNeighbors  map[string]*neighEntries

	stopChan chan struct{}
}

func NewController(nodeName string, wf factory.NodeWatchFactory, kube kube.Interface, ndm netlinkdevicemanager.Interface, networkMgr networkmanager.Interface, ovsClient libovsdbclient.Client, addressManager nodeAddressManager) (*Controller, error) {
	if addressManager == nil {
		return nil, fmt.Errorf("EVPN node VTEP controller requires a non-nil node address manager")
	}

	c := &Controller{
		nodeName:       nodeName,
		watchFactory:   wf,
		kube:           kube,
		networkMgr:     networkMgr,
		ndm:            ndm,
		ovsClient:      ovsClient,
		addressManager: addressManager,
		nadVTEPInfo:    make(map[string]string),
		svisByBridge:   make(map[string]sets.Set[string]),
		stopChan:       make(chan struct{}),
	}

	vtepInformer := wf.VTEPInformer()
	c.vtepController = controller.NewController("evpn-node-vtep-controller", &controller.ControllerConfig[vtepv1.VTEP]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		ObjNeedsUpdate: c.vtepNeedsUpdate,
		Threadiness:    1,
		Informer:       vtepInformer.Informer(),
		Lister:         vtepInformer.Lister().List,
	})
	c.nadReconciler = controller.NewReconciler("evpn-nad-reconciler", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.reconcileNAD,
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	})

	podInformer := wf.PodCoreInformer()
	c.podLister = podInformer.Lister()
	c.podNeighbors = make(map[string]*neighEntries)
	c.podController = controller.NewController("evpn-pod-neighbor-controller",
		&controller.ControllerConfig[corev1.Pod]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      c.reconcilePod,
			ObjNeedsUpdate: c.podNeedsUpdate,
			Threadiness:    1,
			Informer:       podInformer.Informer(),
			Lister:         c.podLister.List,
		})

	var err error
	c.nodeEventHandler, err = wf.NodeCoreInformer().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: c.onNodeUpdate,
		})
	if err != nil {
		return nil, fmt.Errorf("failed to add node event handler: %w", err)
	}

	addressManager.AddOnAddressesChangedHandler(func() {
		c.vtepController.Reconcile(reconcileNodeAddressChange)
	})

	return c, nil
}

func (c *Controller) Start() (err error) {
	klog.Info("Starting EVPN node controller")

	c.nadReconcilerID = c.networkMgr.RegisterNADReconciler(c.nadReconciler)
	defer func() {
		if err != nil {
			c.networkMgr.DeRegisterNADReconciler(c.nadReconcilerID)
			c.nadReconcilerID = 0
		}
	}()
	return controller.StartWithInitialSync(c.initialSync, c.vtepController, c.nadReconciler, c.podController)
}

func (c *Controller) initialSync() error {
	// VTEP informer cache sync is handled by StartWithInitialSync, but we also
	// need the node informer cache synced to read VTEP IPs annotation.
	if !util.WaitForInformerCacheSyncWithTimeout("evpn-node", c.stopChan, c.watchFactory.NodeCoreInformer().Informer().HasSynced) {
		return fmt.Errorf("timed out waiting for node informer cache to sync")
	}

	vteps, err := c.watchFactory.VTEPInformer().Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list VTEPs: %w", err)
	}

	// Pre-populate NDM desired state so it doesn't remove existing devices on startup.
	// Only NDM-managed devices (bridge, VXLANs, SVIs) are handled here.
	// OVS ports are deferred to normal reconciliation after controllers start.
	activeVTEPs := sets.New[string]()
	for _, vtep := range vteps {
		activeVTEPs.Insert(vtep.Name)
		if vtep.Spec.Mode == vtepv1.VTEPModeManaged {
			continue
		}
		vtepIPv4, vtepIPv6, err := c.discoverUnmanagedVTEPIPs(vtep)
		if err != nil {
			klog.Errorf("Failed to get VTEP IPs for %s: %v", vtep.Name, err)
			continue
		}
		if vtepIPv4 == nil && vtepIPv6 == nil {
			continue
		}
		if _, err := c.ensureDevices(vtep, vtepIPv4, vtepIPv6); err != nil {
			klog.Errorf("Failed to pre-populate NDM for VTEP %s: %v", vtep.Name, err)
		}
	}

	// Clean up stale OVS ports from VTEPs that were deleted while the controller was down.
	// NDM handles netlink device cleanup via alias-based ownership, but OVS ports are managed
	// directly through libovsdb and need explicit cleanup here.
	if err := c.cleanupStaleOVSPorts(activeVTEPs); err != nil {
		klog.Errorf("Failed to clean up stale OVS ports: %v", err)
	}

	if err := c.cleanStalePodEntries(); err != nil {
		klog.Errorf("Failed to sync pod neighbors: %v", err)
	}

	return nil
}

func (c *Controller) Stop() {
	klog.Info("Stopping EVPN node controller")

	if c.nodeEventHandler != nil {
		if err := c.watchFactory.NodeCoreInformer().Informer().RemoveEventHandler(c.nodeEventHandler); err != nil {
			klog.Errorf("Failed to remove node event handler: %v", err)
		}
	}

	if c.nadReconcilerID != 0 {
		c.networkMgr.DeRegisterNADReconciler(c.nadReconcilerID)
	}

	controller.Stop(c.vtepController, c.nadReconciler, c.podController)

	close(c.stopChan)
}

func (c *Controller) vtepNeedsUpdate(oldObj, newObj *vtepv1.VTEP) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec, newObj.Spec)
}

func (c *Controller) onNodeUpdate(oldObj, newObj interface{}) {
	oldNode, ok := oldObj.(*corev1.Node)
	if !ok {
		return
	}
	newNode, ok := newObj.(*corev1.Node)
	if !ok {
		return
	}
	if newNode.Name != c.nodeName {
		return
	}
	if !util.NodeVTEPsAnnotationChanged(oldNode, newNode) {
		return
	}
	// don't reconcile on our own updates
	if util.IsLastUpdatedByManager(vtepAnnotationFieldManager, oldNode.ManagedFields, newNode.ManagedFields) {
		return
	}

	c.vtepController.Reconcile(reconcileVTEPAnnotationChange)
}

// reconcileNodeAddressChange triggers reconciliation for unmanaged VTEPs whose
// annotated IPs are no longer valid or missing. Called when the node's
// addresses change via the address manager callback.
func (c *Controller) reconcileNodeAddressChange() error {
	vtepsAnnotation, err := c.getVTEPsAnnotation()
	if err != nil {
		return fmt.Errorf("failed to get VTEP annotation for address change reconciliation: %w", err)
	}

	nodeIPs, _ := c.addressManager.ListAddresses()
	nodeIPSet := sets.New(util.StringSlice(nodeIPs)...)

	vteps, err := c.watchFactory.VTEPInformer().Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list VTEPs for address change reconciliation: %w", err)
	}
	for _, vtep := range vteps {
		if vtep.Spec.Mode != vtepv1.VTEPModeUnmanaged {
			continue
		}
		needsIPv4 := util.MatchFirstCIDRStringFamily(false, vtep.Spec.CIDRs) != ""
		needsIPv6 := util.MatchFirstCIDRStringFamily(true, vtep.Spec.CIDRs) != ""
		v4AnnotatedIP := matchFirstIPStringFamily(false, vtepsAnnotation[vtep.Name].IPs)
		v6AnnotatedIP := matchFirstIPStringFamily(true, vtepsAnnotation[vtep.Name].IPs)
		missesIPv4 := needsIPv4 && !nodeIPSet.Has(v4AnnotatedIP)
		missesIPv6 := needsIPv6 && !nodeIPSet.Has(v6AnnotatedIP)
		if missesIPv4 || missesIPv6 {
			klog.V(5).Infof("Node addresses changed on %s, reconciling unmanaged VTEP %s", c.nodeName, vtep.Name)
			c.vtepController.Reconcile(vtep.Name)
		}
	}

	return nil
}

// reconcile ensures all node-local devices for a VTEP are in the desired state:
// 1. Discover/read VTEP IPs
// 2. Ensure bridge, VXLAN tunnels, and VID/VNI mappings via NDM
// 3. Reconcile SVIs and OVS ports for each EVPN-enabled network
// If the VTEP is deleted or unsupported, all its devices are cleaned up.
// The synthetic keys reconcileVTEPAnnotationChange and
// reconcileNodeAddressChange trigger reconciliation of unmanaged VTEPs
// whose annotated IPs are stale or missing.
func (c *Controller) reconcile(key string) error {
	switch key {
	case reconcileVTEPAnnotationChange:
		// node annotation changed, reset the cached annotation to re-read
		c.vtepsAnnotation = nil
		fallthrough
	case reconcileNodeAddressChange:
		return c.reconcileNodeAddressChange()
	}

	vtep, err := c.watchFactory.VTEPInformer().Lister().Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("VTEP %s not found, cleaning up", key)
			if err := c.deleteVTEPDevices(key); err != nil {
				return err
			}
			return c.setVTEPAnnotation(key, nil, nil)
		}
		return err
	}
	if vtep.Spec.Mode == vtepv1.VTEPModeManaged {
		klog.Warningf("VTEP %s uses unsupported %s mode, cleaning up", vtep.Name, vtepv1.VTEPModeManaged)
		if err := c.deleteVTEPDevices(key); err != nil {
			return err
		}
		return c.setVTEPAnnotation(key, nil, nil)
	}

	if config.HybridOverlay.Enabled && config.HybridOverlay.VXLANPort == config.DefaultVXLANPort {
		return fmt.Errorf("hybrid overlay is enabled with VXLAN port %d which conflicts with EVPN VXLAN; "+
			"configure a different hybrid-overlay-vxlan-port to avoid the conflict", config.DefaultVXLANPort)
	}

	vtepIPv4, vtepIPv6, err := c.discoverUnmanagedVTEPIPs(vtep)
	if err != nil {
		return fmt.Errorf("failed to discover VTEP %s IPs: %w", vtep.Name, err)
	}
	if vtepIPv4 == nil && vtepIPv6 == nil {
		klog.Infof("VTEP %s IPs not yet available for node %s", vtep.Name, c.nodeName)
		return c.deleteVTEPDevices(key)
	}

	networks, err := c.ensureDevices(vtep, vtepIPv4, vtepIPv6)
	if err != nil {
		return err
	}

	if err := c.reconcileOVSPorts(vtep.Name, GetEVPNBridgeName(vtep.Name), networks); err != nil {
		var linkNotFound netlink.LinkNotFoundError
		if errors.As(err, &linkNotFound) {
			klog.V(4).Infof("VTEP %s OVS port not yet available (%v), will retry", vtep.Name, err)
			c.vtepController.ReconcileRateLimited(vtep.Name)
			return nil
		}
		return fmt.Errorf("failed to reconcile VTEP %s OVS ports: %w", vtep.Name, err)
	}

	return nil
}

// ensureDevices programs NDM-managed devices for a VTEP: bridge, VXLANs, and SVIs.
// OVS ports are handled separately by reconcileOVSPorts.
func (c *Controller) ensureDevices(vtep *vtepv1.VTEP, vtepIPv4, vtepIPv6 net.IP) ([]evpnNetworkInfo, error) {
	bridgeName := GetEVPNBridgeName(vtep.Name)

	klog.V(4).Infof("Applying EVPN devices for VTEP %s: bridge=%s, IPv4=%v, IPv6=%v",
		vtep.Name, bridgeName, vtepIPv4, vtepIPv6)

	err := c.ndm.EnsureLink(netlinkdevicemanager.DeviceConfig{
		Link: &netlink.Bridge{
			LinkAttrs:       netlink.LinkAttrs{Name: bridgeName},
			VlanFiltering:   ptr.To(true),
			VlanDefaultPVID: ptr.To(uint16(0)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to apply VTEP %s bridge %s: %w", vtep.Name, bridgeName, err)
	}

	networks, err := c.collectEVPNNetworks(vtep.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to collect VTEP %s EVPN networks: %w", vtep.Name, err)
	}

	mappings := c.getVIDVNIMappings(networks)
	if vtepIPv4 != nil {
		vxlan4Name := GetEVPNVXLANName(vtep.Name, utilnet.IPv4)
		if err := c.ensureVXLAN(vxlan4Name, bridgeName, vtepIPv4, mappings); err != nil {
			return nil, fmt.Errorf("failed to configure VTEP %s IPv4 VXLAN device: %w", vtep.Name, err)
		}
	} else {
		// Cover losing ipv4 vtep IP
		if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtep.Name, utilnet.IPv4)); err != nil {
			return nil, fmt.Errorf("failed to delete VTEP %s IPv4 VXLAN device: %w", vtep.Name, err)
		}
	}

	if vtepIPv6 != nil {
		vxlan6Name := GetEVPNVXLANName(vtep.Name, utilnet.IPv6)
		if err := c.ensureVXLAN(vxlan6Name, bridgeName, vtepIPv6, mappings); err != nil {
			return nil, fmt.Errorf("failed to configure VTEP %s IPv6 VXLAN device: %w", vtep.Name, err)
		}
	} else {
		// Cover losing ipv6 vtep IP
		if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtep.Name, utilnet.IPv6)); err != nil {
			return nil, fmt.Errorf("failed to delete VTEP %s IPv6 VXLAN device: %w", vtep.Name, err)
		}
	}

	if err := c.reconcileSVIs(bridgeName, networks); err != nil {
		return nil, fmt.Errorf("failed to reconcile VTEP %s SVIs: %w", vtep.Name, err)
	}

	return networks, nil
}

func (c *Controller) reconcileNAD(key string) error {
	netInfo := c.networkMgr.GetNetInfoForNADKey(key)
	c.nadVTEPInfoLock.Lock()
	defer c.nadVTEPInfoLock.Unlock()

	if netInfo == nil {
		vtepName, ok := c.nadVTEPInfo[key]
		if ok {
			klog.Infof("Network %s removed, reconciling VTEP %s", key, vtepName)
			delete(c.nadVTEPInfo, key)
			c.vtepController.ReconcileRateLimited(vtepName)
		}
		return nil
	}

	vtepName := netInfo.EVPNVTEPName()
	if vtepName == "" {
		return nil
	}

	if c.nadVTEPInfo[key] == vtepName {
		// NAD was already reconciled
		return nil
	}

	c.nadVTEPInfo[key] = vtepName
	c.vtepController.ReconcileRateLimited(vtepName)

	return nil
}

// evpnNetworkInfo holds EVPN config for a network.
type evpnNetworkInfo struct {
	macVRFVID, macVRFVNI int
	ipVRFVID, ipVRFVNI   int
	l3SVIName            string
	l2SVIName            string
	ovsPortName          string
	macVRFLSPName        string
	vrfName              string
}

// collectEVPNNetworks gathers EVPN network info for all networks using this VTEP.
// This is rebuilt on every reconcile to pick up network additions/removals without
// maintaining a separate long-lived cache.
func (c *Controller) collectEVPNNetworks(vtepName string) ([]evpnNetworkInfo, error) {
	var networks []evpnNetworkInfo

	err := c.networkMgr.DoWithLock(func(netInfo util.NetInfo) error {
		if netInfo == nil || netInfo.EVPNVTEPName() != vtepName {
			return nil
		}
		switchName := netInfo.GetNetworkScopedSwitchName(types.OVNLayer2Switch)
		networks = append(networks, evpnNetworkInfo{
			macVRFVID:     netInfo.EVPNMACVRFVID(),
			macVRFVNI:     int(netInfo.EVPNMACVRFVNI()),
			ipVRFVID:      netInfo.EVPNIPVRFVID(),
			ipVRFVNI:      int(netInfo.EVPNIPVRFVNI()),
			l3SVIName:     GetEVPNL3SVIName(netInfo),
			l2SVIName:     GetEVPNL2SVIName(netInfo),
			ovsPortName:   GetEVPNOVSPortName(netInfo),
			macVRFLSPName: util.GetMACVRFPortName(switchName),
			vrfName:       util.GetNetworkVRFName(netInfo),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return networks, nil
}

// getVIDVNIMappings builds the VID-VNI mapping table from the collected networks.
// Returns a non-nil empty slice when no networks exist so NDM removes stale mappings.
func (c *Controller) getVIDVNIMappings(networks []evpnNetworkInfo) []netlinkdevicemanager.VIDVNIMapping {
	mappings := make([]netlinkdevicemanager.VIDVNIMapping, 0)
	for _, n := range networks {
		if n.macVRFVID != 0 && n.macVRFVNI != 0 {
			mappings = append(mappings, netlinkdevicemanager.VIDVNIMapping{VID: uint16(n.macVRFVID), VNI: uint32(n.macVRFVNI)})
		}
		if n.ipVRFVID != 0 && n.ipVRFVNI != 0 {
			mappings = append(mappings, netlinkdevicemanager.VIDVNIMapping{VID: uint16(n.ipVRFVID), VNI: uint32(n.ipVRFVNI)})
		}
	}
	return mappings
}

// reconcileSVIs ensures desired SVIs exist and removes stale ones.
// Creates L3 (IP-VRF) SVIs for routing and L2 (MAC-VRF) SVIs for ARP suppression
// and L2VNI-to-VRF association. Tracks created SVIs in svisByBridge to detect
// stale ones.
func (c *Controller) reconcileSVIs(bridgeName string, networks []evpnNetworkInfo) error {
	desiredSVIs := sets.New[string]()
	for _, net := range networks {
		if net.ipVRFVID != 0 {
			desiredSVIs.Insert(net.l3SVIName)
			if err := c.ndm.EnsureLink(netlinkdevicemanager.DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: net.l3SVIName},
					VlanId:    net.ipVRFVID,
				},
				VLANParent: bridgeName,
				Master:     net.vrfName,
			}); err != nil {
				return fmt.Errorf("failed to ensure L3 SVI %s: %w", net.l3SVIName, err)
			}
		}

		if net.macVRFVID != 0 {
			desiredSVIs.Insert(net.l2SVIName)
			if err := c.ndm.EnsureLink(netlinkdevicemanager.DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: net.l2SVIName},
					VlanId:    net.macVRFVID,
				},
				VLANParent: bridgeName,
				Master:     net.vrfName,
			}); err != nil {
				return fmt.Errorf("failed to ensure L2 SVI %s: %w", net.l2SVIName, err)
			}
		}
	}

	c.svisByBridgeLock.Lock()
	defer c.svisByBridgeLock.Unlock()
	for sviName := range c.svisByBridge[bridgeName] {
		if !desiredSVIs.Has(sviName) {
			if err := c.ndm.DeleteLink(sviName); err != nil {
				return fmt.Errorf("failed to delete stale SVI %s: %w", sviName, err)
			}
		}
	}
	c.svisByBridge[bridgeName] = desiredSVIs

	return nil
}

// reconcileOVSPorts ensures OVS internal ports exist for MAC-VRF networks and removes stale ones.
func (c *Controller) reconcileOVSPorts(vtepName, bridgeName string, networks []evpnNetworkInfo) error {
	desiredPorts := sets.New[string]()
	for _, net := range networks {
		if net.macVRFVID == 0 {
			continue
		}
		desiredPorts.Insert(net.ovsPortName)
		if err := c.ensureOVSPort(vtepName, bridgeName, net.ovsPortName, net.macVRFLSPName, net.macVRFVID); err != nil {
			return fmt.Errorf("failed to ensure OVS port %s: %w", net.ovsPortName, err)
		}
	}

	ports, err := libovsdbops.FindOVSPortsWithPredicate(c.ovsClient, func(port *vswitchd.Port) bool {
		return port.ExternalIDs[types.EVPNVTEPExternalID] == vtepName
	})
	if err != nil {
		return fmt.Errorf("failed to list OVS ports for VTEP %s: %w", vtepName, err)
	}
	for _, port := range ports {
		if !desiredPorts.Has(port.Name) {
			klog.Infof("OVS port %s no longer needed for VTEP %s, removing", port.Name, vtepName)
			if err := libovsdbops.DeletePortWithInterfaces(c.ovsClient, ovsBridgeInt, port.Name); err != nil {
				return fmt.Errorf("failed to delete stale OVS port %s: %w", port.Name, err)
			}
		}
	}

	return nil
}

// ensureOVSPort creates an OVS internal port on br-int, attaches it to the EVPN bridge, and sets VLAN access.
// The iface-id on the Interface external_ids is set to macVRFLSPName so that ovn-controller can bind the
// corresponding MAC-VRF logical switch port to this OVS port.
func (c *Controller) ensureOVSPort(vtepName, bridgeName, portName, macVRFLSPName string, vid int) error {
	if err := libovsdbops.CreateOrUpdatePortWithInterface(c.ovsClient, ovsBridgeInt, portName,
		map[string]string{types.EVPNVTEPExternalID: vtepName},
		map[string]string{"iface-id": macVRFLSPName}); err != nil {
		return fmt.Errorf("failed to create OVS port %s: %w", portName, err)
	}

	ovsLink, err := util.GetNetLinkOps().LinkByName(portName)
	if err != nil {
		return fmt.Errorf("failed to get OVS port link %s: %w", portName, err)
	}
	evpnBridge, err := util.GetNetLinkOps().LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get EVPN bridge %s: %w", bridgeName, err)
	}
	if err := util.GetNetLinkOps().LinkSetMaster(ovsLink, evpnBridge); err != nil {
		return fmt.Errorf("failed to attach OVS port %s to bridge %s: %w", portName, bridgeName, err)
	}
	if err := util.GetNetLinkOps().LinkSetUp(ovsLink); err != nil {
		return fmt.Errorf("failed to bring up OVS port %s: %w", portName, err)
	}

	if err := util.GetNetLinkOps().BridgeVlanAdd(ovsLink, uint16(vid), true, true, false, true); err != nil {
		return fmt.Errorf("failed to set VLAN %d on OVS port %s: %w", vid, portName, err)
	}

	return nil
}

func (c *Controller) deleteVTEPDevices(vtepName string) error {
	bridgeName := GetEVPNBridgeName(vtepName)
	var errs []error

	ports, err := libovsdbops.FindOVSPortsWithPredicate(c.ovsClient, func(port *vswitchd.Port) bool {
		return port.ExternalIDs[types.EVPNVTEPExternalID] == vtepName
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to list VTEP %s OVS ports: %w", vtepName, err))
	}
	for _, port := range ports {
		if err := libovsdbops.DeletePortWithInterfaces(c.ovsClient, ovsBridgeInt, port.Name); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete VTEP %s OVS port %s: %w", vtepName, port.Name, err))
		}
	}

	// Delete all SVIs parented to this bridge
	c.svisByBridgeLock.Lock()
	for sviName := range c.svisByBridge[bridgeName] {
		if err := c.ndm.DeleteLink(sviName); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete VTEP %s SVI %s: %w", vtepName, sviName, err))
		} else {
			c.svisByBridge[bridgeName].Delete(sviName)
		}
	}
	if c.svisByBridge[bridgeName].Len() == 0 {
		delete(c.svisByBridge, bridgeName)
	}
	c.svisByBridgeLock.Unlock()

	if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtepName, utilnet.IPv4)); err != nil {
		errs = append(errs, fmt.Errorf("failed to delete VTEP %s IPv4 VXLAN device: %w", vtepName, err))
	}
	if err := c.ndm.DeleteLink(GetEVPNVXLANName(vtepName, utilnet.IPv6)); err != nil {
		errs = append(errs, fmt.Errorf("failed to delete VTEP %s IPv6 VXLAN device: %w", vtepName, err))
	}
	if err := c.ndm.DeleteLink(bridgeName); err != nil {
		errs = append(errs, fmt.Errorf("failed to delete VTEP %s bridge %s: %w", vtepName, bridgeName, err))
	}

	c.nadVTEPInfoLock.Lock()
	for key, name := range c.nadVTEPInfo {
		if name == vtepName {
			delete(c.nadVTEPInfo, key)
		}
	}
	c.nadVTEPInfoLock.Unlock()

	return utilerrors.Join(errs...)
}

// cleanupStaleOVSPorts removes OVS ports tagged with the evpn-vtep external-id
// whose VTEP no longer exists. This handles VTEPs deleted while the controller was down,
// which the per-VTEP reconcile loop cannot catch.
func (c *Controller) cleanupStaleOVSPorts(activeVTEPs sets.Set[string]) error {
	ports, err := libovsdbops.FindOVSPortsWithPredicate(c.ovsClient, func(port *vswitchd.Port) bool {
		vtep, ok := port.ExternalIDs[types.EVPNVTEPExternalID]
		return ok && !activeVTEPs.Has(vtep)
	})
	if err != nil {
		return fmt.Errorf("failed to list stale OVS ports: %w", err)
	}
	var errs []error
	for _, port := range ports {
		klog.Infof("Cleaning up stale OVS port %s (VTEP %s no longer exists)", port.Name, port.ExternalIDs[types.EVPNVTEPExternalID])
		if err := libovsdbops.DeletePortWithInterfaces(c.ovsClient, ovsBridgeInt, port.Name); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete stale OVS port %s: %w", port.Name, err))
		}
	}
	return utilerrors.Join(errs...)
}

// ensureVXLAN programs a VXLAN device on the EVPN bridge.
func (c *Controller) ensureVXLAN(vxlanName string, bridgeName string, srcIP net.IP, mappings []netlinkdevicemanager.VIDVNIMapping) error {
	err := c.ndm.EnsureLink(netlinkdevicemanager.DeviceConfig{
		Link: &netlink.Vxlan{
			LinkAttrs: netlink.LinkAttrs{Name: vxlanName, MTU: config.Default.MTU},
			Port:      config.DefaultVXLANPort,
			SrcAddr:   srcIP,
			// FlowBased (ip link: external): destination VTEP is resolved per-flow from the FDB, populated by FRR via BGP EV
			FlowBased: true,
			// Only accept traffic for configured VNIs
			VniFilter: true,
		},
		Master: bridgeName,
		BridgePortSettings: &netlinkdevicemanager.BridgePortSettings{
			VLANTunnel: true,
			// Answer ARP/ND locally from the bridge neigh table instead of flooding
			NeighSuppress: true,
			// Disable data-plane MAC learning, rely on BGP EVPN Type-2 routes
			Learning: false,
			// Isolated VXLAN ports can only forward to non-isolated ports.
			// In dual-stack setups with separate IPv4 and IPv6 VXLAN devices on the
			// same bridge, BUM traffic arriving on one VXLAN would be flooded out
			// the other, creating an amplification loop between VTEP peers.
			Isolated: true,
		},
		VIDVNIMappings: mappings,
	})
	if err != nil {
		return fmt.Errorf("failed to apply VXLAN %s: %w", vxlanName, err)
	}
	return nil
}

// discoverUnmanagedVTEPIPs resolves the IPs to use for an unmanaged VTEP.
// For each IP family present in the VTEP's CIDRs, it checks the node's VTEP annotation
// for a previously selected IP. If that IP is still present in the address manager and
// within the VTEP's CIDRs, it is reused for stability. Otherwise, a new IP is discovered
// from the address manager and the annotation is updated.
func (c *Controller) discoverUnmanagedVTEPIPs(vtep *vtepv1.VTEP) (net.IP, net.IP, error) {
	// fetch the annotated VTEP IPs
	vtepsAnnotation, err := c.getVTEPsAnnotation()
	if err != nil {
		return nil, nil, err
	}
	v4AnnotatedIP := net.ParseIP(matchFirstIPStringFamily(false, vtepsAnnotation[vtep.Name].IPs))
	v6AnnotatedIP := net.ParseIP(matchFirstIPStringFamily(true, vtepsAnnotation[vtep.Name].IPs))

	// get valid node IP addresses
	nodeIPs, _ := c.addressManager.ListAddresses()
	v4NodeIPs, v6NodeIPs := util.SplitIPsByIPFamily(nodeIPs)

	// get the VTEP CIDRs
	vtepIPNets, err := util.ParseIPNets(vtep.Spec.CIDRs)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse VTEP %q CIDRs %v: %w", vtep.Name, vtep.Spec.CIDRs, err)
	}
	v4VTEPCIDRS, v6VTEPCIDRS := util.SplitIPNetsByIPFamily(vtepIPNets)

	// reuse the annotated IP if still valid otherwise select a new one,
	// resolving each IP family independently
	v4SelectedIP, err := c.resolveVTEPIP(netlink.FAMILY_V4, v4VTEPCIDRS, v4NodeIPs, v4AnnotatedIP)
	if err != nil {
		return nil, nil, err
	}

	v6SelectedIP, err := c.resolveVTEPIP(netlink.FAMILY_V6, v6VTEPCIDRS, v6NodeIPs, v6AnnotatedIP)
	if err != nil {
		return nil, nil, err
	}

	changed := !v4AnnotatedIP.Equal(v4SelectedIP) || !v6AnnotatedIP.Equal(v6SelectedIP)
	if changed {
		if err := c.setVTEPAnnotation(vtep.Name, v4SelectedIP, v6SelectedIP); err != nil {
			return nil, nil, fmt.Errorf("failed to annotate VTEP %s IPs: %w", vtep.Name, err)
		}
	}
	return v4SelectedIP, v6SelectedIP, nil
}

// resolveVTEPIP returns the IP to use for a given family. If the VTEP IP
// is still present among the node IPs and within the VTEP's CIDRs, it is
// returned. Otherwise, a new IP is selected from matching node IPs.
func (c *Controller) resolveVTEPIP(family int, vtepCIDRs []*net.IPNet, nodeIPs []net.IP, vtepIP net.IP) (net.IP, error) {
	if len(vtepCIDRs) == 0 {
		return nil, nil
	}

	equalsVTEPIP := func(ip net.IP) bool { return ip.Equal(vtepIP) }
	if vtepIP != nil && slices.ContainsFunc(nodeIPs, equalsVTEPIP) && util.IsIPContainedInAnyCIDR(vtepIP, vtepCIDRs...) {
		return vtepIP, nil
	}

	// Select a new IP from node addresses matching the CIDRs.
	var matches []net.IP
	for _, nodeIP := range nodeIPs {
		if util.IsIPContainedInAnyCIDR(nodeIP, vtepCIDRs...) {
			matches = append(matches, nodeIP)
		}
	}
	return c.pickVTEPIP(matches, family)
}

// getVTEPsAnnotation returns the controller-local copy of the VTEP annotation.
// On first call it bootstraps from the informer cache; subsequent calls return
// the local copy which is kept in sync by setVTEPAnnotation. This avoids stale
// reads when multiple VTEPs are reconciled before the informer cache updates.
func (c *Controller) getVTEPsAnnotation() (map[string]util.VTEPNodeAnnotation, error) {
	if c.vtepsAnnotation != nil {
		return c.vtepsAnnotation, nil
	}
	node, err := c.watchFactory.GetNode(c.nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", c.nodeName, err)
	}
	vtepsAnnotation, err := util.ParseNodeVTEPs(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			c.vtepsAnnotation = map[string]util.VTEPNodeAnnotation{}
			return c.vtepsAnnotation, nil
		}
		return nil, fmt.Errorf("failed to parse VTEP annotation: %w", err)
	}
	c.vtepsAnnotation = vtepsAnnotation
	return c.vtepsAnnotation, nil
}

// setVTEPAnnotation updates or removes a VTEP entry in the node's VTEP
// annotation. If both IPs are nil, the entry is removed; otherwise it is
// set to the provided IPs.
func (c *Controller) setVTEPAnnotation(vtepName string, ipv4, ipv6 net.IP) (err error) {
	vtepsAnnotation, err := c.getVTEPsAnnotation()
	if err != nil {
		return err
	}

	// restore previous value on error to ensure retry will reattempt
	previous := vtepsAnnotation[vtepName]
	defer func() {
		if err == nil {
			return
		}
		vtepsAnnotation[vtepName] = previous
		if len(previous.IPs) == 0 {
			delete(vtepsAnnotation, vtepName)
		}
	}()

	switch {
	case ipv4 == nil && ipv6 == nil:
		delete(vtepsAnnotation, vtepName)
	default:
		entry := util.VTEPNodeAnnotation{}
		if ipv4 != nil {
			entry.IPs = append(entry.IPs, ipv4.String())
		}
		if ipv6 != nil {
			entry.IPs = append(entry.IPs, ipv6.String())
		}
		vtepsAnnotation[vtepName] = entry
	}

	// inhibit API request if noop
	if slices.Equal(slices.Sorted(slices.Values(previous.IPs)), slices.Sorted(slices.Values(vtepsAnnotation[vtepName].IPs))) {
		return nil
	}

	annotations, err := util.MarshalNodeVTEPs(vtepsAnnotation)
	if err != nil {
		return err
	}

	return c.kube.SetAnnotationsOnNodeWithFieldManager(c.nodeName, annotations, vtepAnnotationFieldManager)
}

// pickVTEPIP selects a single VTEP IP from the candidates for the given address family.
// If there's exactly one match, it's used directly. Multiple matches trigger a netlink
// lookup to filter out keepalived VIPs and secondary addresses (which can float between
// nodes). If ambiguity remains, the lexicographically lowest IP is chosen for determinism.
func (c *Controller) pickVTEPIP(matches []net.IP, family int) (net.IP, error) {
	if len(matches) <= 1 {
		if len(matches) == 1 {
			return matches[0], nil
		}
		return nil, nil
	}

	klog.Infof("Multiple VTEP IP candidates %v (family %d), filtering VIPs via netlink", matches, family)
	addrs, err := util.GetNetLinkOps().AddrList(nil, family)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses (family %d): %w", family, err)
	}
	skipIPs := map[string]bool{}
	for _, addr := range addrs {
		if util.IsAddressAddedByKeepAlived(addr) || (addr.Flags&unix.IFA_F_SECONDARY) != 0 {
			skipIPs[addr.IP.String()] = true
		}
	}
	filtered := matches[:0]
	for _, ip := range matches {
		if !skipIPs[ip.String()] {
			filtered = append(filtered, ip)
		}
	}
	slices.SortFunc(filtered, func(a, b net.IP) int {
		addrA, _ := netip.AddrFromSlice(a)
		addrB, _ := netip.AddrFromSlice(b)
		return addrA.Compare(addrB)
	})
	if len(filtered) > 0 {
		return filtered[0], nil
	}
	return nil, nil

}

func matchFirstIPStringFamily(isIPv6 bool, ips []string) string {
	ip, err := util.MatchIPStringFamily(isIPv6, ips)
	if err != nil {
		return ""
	}
	return ip
}

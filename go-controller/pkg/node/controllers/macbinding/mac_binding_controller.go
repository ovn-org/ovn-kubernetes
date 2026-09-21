// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package macbinding implements the MAC Binding mirror controller for OKEP-6691
// (Scalable ARP and NDP Broadcast Handling for UDN).
//
// Networks (the default network and the UDNs) that share a physical uplink form
// a group. Within each group a single designated network (the "source")
// resolves ARP/ND on the wire; the result lands in the source Gateway Router's
// MAC_Binding rows. This controller mirrors those rows onto the MAC_Binding of
// every other ("follower") Gateway Router in the group, so the followers
// resolve without depending on receiving ARP/ND replies, which is what we can't
// afford to flood to the UDNs sharing the uplink.
//
//   - The default group (networks with no dedicated uplink, i.e. Uplink()=="",
//     sharing breth0) always designates the CDN as source; its followers
//     are the primary L2/L3 UDNs and CUDNs on the node that have no dedicated
//     uplink.
//   - An uplink group (a dedicated uplink, i.e. Uplink()!="") contains only
//     CUDNs; the source is designated externally and reported through
//     ReconcileUplinkSource.
//
// Alongside these mirrored (dynamic) bindings, the controller also writes
// static MAC bindings for node IPs on each uplink segment, rather than
// mirroring dynamic bindings for them which would not scale on large clusters.
// Only the default uplink is supported for now.
//
// The controller reconciles on events:
//
//   - From Network Manager: network additions or deletions
//   - From uplinkSourceProvider: changes on designated sources for uplinks
//   - From SBDB: external GR port binding additions or deletions
//   - From SBDB: mac binding updates
//   - From the node informer: node IP or gateway MAC changes, or node deletions
//
// The controller requires the SBDB MAC binding table to be monitored and
// secondary indexes on logical_port for the SBDB MAC_Binding and NBDB
// Static_MAC_Binding tables.
package macbinding

import (
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// unknownSource designates followers for which we don't know the source
	// yet. It is used as key in the followers map.
	unknownSource = "unknown"

	// keySep separates the components of a composite reconciliation key.
	keySep = "|"
)

// repair done once per run
var repair sync.Once

// MACBindingController mirrors dynamic MAC bindings and writes static node-IP MAC
// bindings across a node's networks. See the package doc for the design.
type MACBindingController struct {
	watchFactory         factory.NodeWatchFactory
	nbClient             libovsdbclient.Client
	sbClient             libovsdbclient.Client
	networkManager       networkmanager.Interface
	uplinkSourceProvider uplinkSourceProvider

	nodeName       string
	cdnGatewayPort string

	dynamicMacBindingReconciler        controller.Reconciler
	dynamicMacBindingRefreshReconciler controller.Reconciler
	networkReconciler                  controller.Reconciler
	nodeIPsReconciler                  controller.Reconciler
	staticMacBindingReconciler         controller.Reconciler

	nodeEventHandler cache.ResourceEventHandlerRegistration

	// macBindingSourceForUplinks memoizes the uplink->source designation
	// provided by the uplinkSourceProvider.
	macBindingSourceForUplinks atomic.Pointer[map[string]string]

	// lastBacklogWarningMs throttles the mirror-backlog warning (UnixMilli of
	// the last emission); see throttleBacklogWarning.
	lastBacklogWarningMs atomic.Int64

	// mutex for the following maps
	sync.RWMutex
	// followers maps sources to followers. They are external GW router port
	// names. The controller syncs mac bindings from the sources to the
	// corresponding followers. The source is the CDN for the default uplink or
	// a designated CUDN by openflow manager for uplink bridges. Designated
	// uplink sources can change dynamically and followers might not have known
	// sources in which case they are tracked with "unknown" key. While there is
	// an unknown source the controller does full network reconciliations since
	// it needs the complete picture to re-allocate.
	followers map[string]sets.Set[string]

	// nodeIPs holds the desired node-IP static bindings as uplink->ip->mac
	nodeIPs map[string]map[string]string
}

type uplinkSourceProvider interface {
	GetMacBindingSourceForUplinks() map[string]string
}

// NewMACBindingController creates a new MACBindingController.
func NewMACBindingController(
	nbClient libovsdbclient.Client,
	sbClient libovsdbclient.Client,
	watchFactory factory.NodeWatchFactory,
	networkManager networkmanager.Interface,
	uplinkSourceProvider uplinkSourceProvider,
	nodeName string,
) *MACBindingController {
	c := &MACBindingController{
		nbClient:             nbClient,
		sbClient:             sbClient,
		watchFactory:         watchFactory,
		networkManager:       networkManager,
		uplinkSourceProvider: uplinkSourceProvider,
		nodeName:             nodeName,
		cdnGatewayPort:       util.GetNetworkScopedGWRouterExtPortName(types.DefaultNetworkName, nodeName),
		followers:            map[string]sets.Set[string]{},
		nodeIPs:              map[string]map[string]string{},
	}

	c.dynamicMacBindingReconciler = controller.NewReconciler(
		"mac-binding-dynamic-reconciler",
		&controller.ReconcilerConfig{
			// exponential backoff capped at 10s so the final attempt lands near 1m
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](5*time.Millisecond, 10*time.Second),
			Reconcile:   c.reconcileDynamicMacBindings,
			Threadiness: 1,
			MaxAttempts: 16, // retry during ~1m (final attempt ~60.2s)
		},
	)

	c.dynamicMacBindingRefreshReconciler = controller.NewReconciler(
		"mac-binding-dynamic-refresh-reconciler",
		&controller.ReconcilerConfig{
			// fixed 10s delay between retries
			RateLimiter: workqueue.NewTypedItemFastSlowRateLimiter[string](10*time.Second, 10*time.Second, 1),
			Reconcile:   c.reconcileDynamicMacBindingsRefresh,
			Threadiness: 1,
			MaxAttempts: 7, // retry every 10s during ~1m
		},
	)

	c.networkReconciler = controller.NewReconciler(
		"mac-binding-network-reconciler",
		&controller.ReconcilerConfig{
			Reconcile:   c.reconcileNetwork,
			Threadiness: 1,
			MaxAttempts: controller.InfiniteAttempts,
		},
	)

	c.nodeIPsReconciler = controller.NewReconciler(
		"mac-binding-node-ips-reconciler",
		&controller.ReconcilerConfig{
			Reconcile:   c.reconcileNodeIPs,
			Threadiness: 1,
			MaxAttempts: controller.InfiniteAttempts,
		},
	)

	c.staticMacBindingReconciler = controller.NewReconciler(
		"mac-binding-static-reconciler",
		&controller.ReconcilerConfig{
			Reconcile:   c.reconcileStaticMacBindings,
			Threadiness: 1,
			MaxAttempts: controller.InfiniteAttempts,
		},
	)

	return c
}

// Run starts the controller and blocks until stopCh is closed.
func (c *MACBindingController) Run(stopCh <-chan struct{}) error {
	klog.Info("Running MAC Binding controller...")

	// enqueue full network reconcile first
	c.enqueueAllNetworks()

	// register for events
	c.networkManager.RegisterNetworkRefReconciler(networkRefReconcilerFunc(func(node, networkName string) {
		if node != c.nodeName {
			return
		}
		c.enqueueNetwork(networkName)
	}))
	c.networkManager.RegisterNADReconciler(c.networkReconciler)
	c.registerSouthBoundEventHandlers()

	if err := c.registerNodeIPsHandlers(); err != nil {
		return fmt.Errorf("failed to register node nodeIPs handlers: %w", err)
	}
	defer c.deRegisterNodeIPsHandlers()

	// start the reconcilers
	reconcilers := []controller.Reconciler{
		c.dynamicMacBindingReconciler,
		c.dynamicMacBindingRefreshReconciler,
		c.networkReconciler,
		c.nodeIPsReconciler,
		c.staticMacBindingReconciler,
	}
	if err := controller.Start(reconcilers...); err != nil {
		return fmt.Errorf("failed to start MAC Binding controller: %w", err)
	}
	defer controller.Stop(reconcilers...)

	<-stopCh
	klog.Info("Stopping MAC Binding controller...")
	return nil
}

func (c *MACBindingController) repair(knownPorts sets.Set[string]) {
	// delete stale static mac bindings
	c.repairStaticMacBindings(knownPorts)
}

// ReconcileUplinkSource is called by the uplinkSourceProvider to report that
// network has become the designated source for its uplink group.
func (c *MACBindingController) ReconcileUplinkSource(network string) {
	port := util.GetNetworkScopedGWRouterExtPortName(network, c.nodeName)
	if c.tracksSource(port) {
		return
	}
	// reset the cache with an empty map to refetch
	c.invalidateBindingSourceForUplinks()
	c.enqueueNetwork(network)
}

// --- reconcile Networks ----------------------------------------------------

// reconcileNetwork or all networks with a zero key.
func (c *MACBindingController) reconcileNetwork(key string) error {
	if key == "" {
		// special key "" reconciles all
		return c.syncAllNetworks()
	}
	return c.syncNetwork(key)
}

func (c *MACBindingController) enqueueNetwork(network string) {
	c.networkReconciler.Reconcile(network)
}

func (c *MACBindingController) enqueueAllNetworks() {
	c.networkReconciler.Reconcile("")
}

// --- dynamic mac bindings ------------------------------------------

// reconcileDynamicMacBindings handles the reconciliation of new dynamic mac
// bindings:
//   - all dynamic mac bindings from the uplink/source to a follower where key is "<follower>"
//   - a dynamic mac binding from the source to all followers where the key is "<source>|<ip>"
func (c *MACBindingController) reconcileDynamicMacBindings(key string) error {
	return c.doReconcileDynamicMacBindings(key, addWarnDelayThreshold)
}

func (c *MACBindingController) enqueueDynamicMacBinding(source, ip string) {
	c.dynamicMacBindingReconciler.Reconcile(source + keySep + ip)
}

// reconcileDynamicMacBindingsRefresh handles the timestamp refresh of dynamic
// mac bindings. Key is "<source>|<ip>". Handled through a different queue to
// not disturb mission critical additions of new mac bindings.
func (c *MACBindingController) reconcileDynamicMacBindingsRefresh(key string) error {
	return c.doReconcileDynamicMacBindings(key, refreshWarnDelayThreshold)
}

func (c *MACBindingController) enqueueDynamicMacBindingRefresh(source, ip string) {
	c.dynamicMacBindingRefreshReconciler.Reconcile(source + keySep + ip)
}

func (c *MACBindingController) enqueueFollowerDynamicMacBindings(follower string) {
	c.dynamicMacBindingReconciler.Reconcile(follower)
}

// doReconcileDynamicMacBindings dispatches the sync of dynamic mac bindings:
//   - the dynamic mac bindings mirrored from a follower's source onto it where key is "<follower>"
//   - a dynamic mac binding from the source to all followers where the key is "<source>|<ip>"
func (c *MACBindingController) doReconcileDynamicMacBindings(key string, warnDelayThresholdMs int) error {
	parts := strings.Split(key, keySep)
	switch len(parts) {
	case 2:
		source := parts[0]
		ip := parts[1]
		return c.syncDynamicMacBinding(source, ip, warnDelayThresholdMs)
	default:
		follower := parts[0]
		return c.syncDynamicMacBindingsToFollower(follower)
	}
}

// --- static mac bindings ----------------------------------------------

// reconcileStaticMacBindings handles the reconciliation of static mac bindings:
//   - apply or remove a node IP static mac binding where key is "<uplink>|<ip>"
//   - ensure or remove a follower's static mac bindings where key is "<follower>"
func (c *MACBindingController) reconcileStaticMacBindings(key string) error {
	parts := strings.Split(key, keySep)
	switch len(parts) {
	case 2:
		uplink := parts[0]
		ip := parts[1]
		return c.syncStaticMacBinding(uplink, ip)
	default:
		follower := parts[0]
		return c.syncFollowerStaticMacBindings(follower)
	}
}

func (c *MACBindingController) enqueueStaticMacBinding(uplink, ip string) {
	c.staticMacBindingReconciler.Reconcile(uplink + keySep + ip)
}

func (c *MACBindingController) enqueueFollowerStaticMacBindings(follower string) {
	c.staticMacBindingReconciler.Reconcile(follower)
}

// --- node IPs recompute ------------------------------------------------

// reconcileNodeIPs recomputes the node-IP ip->mac map for an uplink where the
// key is "<uplink>".
func (c *MACBindingController) reconcileNodeIPs(key string) error {
	return c.syncNodeIPsForUplink(key)
}

func (c *MACBindingController) enqueueNodeIPs(uplink string) {
	c.nodeIPsReconciler.Reconcile(uplink)
}

// --- locked state accessors ---------------------------------------------

// tracksSource reports whether source is a designated source, i.e. it is present
// in the follower map, even if it has no followers yet.
func (c *MACBindingController) tracksSource(source string) bool {
	c.RLock()
	defer c.RUnlock()
	_, ok := c.followers[source]
	return ok
}

// tracksSourceWithFollowers reports whether source is a designated source that
// currently has at least one follower to mirror MAC bindings onto.
func (c *MACBindingController) tracksSourceWithFollowers(source string) bool {
	c.RLock()
	defer c.RUnlock()
	return len(c.followers[source]) > 0
}

// getAllPorts returns every port the controller tracks, both sources and
// followers.
func (c *MACBindingController) getAllPorts() sets.Set[string] {
	c.RLock()
	defer c.RUnlock()
	ports := sets.New[string]()
	for source, followers := range c.followers {
		ports.Insert(followers.UnsortedList()...)
		if source == unknownSource {
			continue
		}
		ports.Insert(source)
	}
	return ports
}

// getFollowers returns the followers currently mirrored from source, or nil if
// it has none.
func (c *MACBindingController) getFollowers(source string) []string {
	c.RLock()
	defer c.RUnlock()
	followers := c.followers[source]
	if followers == nil {
		return nil
	}
	return followers.UnsortedList()
}

// getValidSourceForFollower returns the designated source for a follower, or ""
// if it has no known source.
func (c *MACBindingController) getValidSourceForFollower(follower string) string {
	source := c.getSourceForPort(follower)
	if source == follower || source == unknownSource {
		return ""
	}
	return source
}

// getSourceForPort returns the source of the port if port is a follower
// including "unknownSource" if the source is unknown, self if port is a source,
// or "" if neither.
func (c *MACBindingController) getSourceForPort(port string) string {
	c.RLock()
	defer c.RUnlock()
	for source, followers := range c.followers {
		if followers.Has(port) {
			return source
		}
		if source == port {
			return port
		}
	}
	return ""
}

// getUplinkForSource returns the uplink that source is designated for.
func (c *MACBindingController) getUplinkForSource(source string) (string, bool) {
	for uplink, s := range c.getMacBindingSourceForUplinks() {
		if s == source {
			return uplink, true
		}
	}
	return "", false
}

// hasUnknownSource reports whether a full network reconcile is due to resolve
// sources: either followers are parked in the unknownSource bucket, or the
// uplink->source designation cache is invalidated.
func (c *MACBindingController) hasUnknownSource() bool {
	c.RLock()
	defer c.RUnlock()
	if _, hasUnknown := c.followers[unknownSource]; hasUnknown {
		return true
	}
	return !isValidBindingSourceForUplinks(c.macBindingSourceForUplinks.Load())
}

// getNodeIPMAC returns the cached MAC for a node ip on uplink. A false ok means
// the ip is no longer a node IP there and must be deleted from followers.
func (c *MACBindingController) getNodeIPMAC(uplink, ip string) (string, bool) {
	c.RLock()
	defer c.RUnlock()
	mac, ok := c.nodeIPs[uplink][ip]
	return mac, ok
}

// getNodeIPMACs returns a copy of the cached ip->mac map for uplink, for
// catching a newly allocated follower up on the whole set.
func (c *MACBindingController) getNodeIPMACs(uplink string) map[string]string {
	c.RLock()
	defer c.RUnlock()
	return maps.Clone(c.nodeIPs[uplink])
}

// isNodeIP reports whether ip is a node IP on uplink, i.e. it has a static MAC
// binding there and so must not also be mirrored dynamically.
func (c *MACBindingController) isNodeIP(uplink, ip string) bool {
	c.RLock()
	defer c.RUnlock()
	_, ok := c.nodeIPs[uplink][ip]
	return ok
}

func (c *MACBindingController) invalidateBindingSourceForUplinks() {
	c.macBindingSourceForUplinks.Store(&map[string]string{})
}

func isValidBindingSourceForUplinks(macBindingSourceForUplinks *map[string]string) bool {
	return macBindingSourceForUplinks != nil && len(*macBindingSourceForUplinks) > 0
}

// getMacBindingSourceForUplinks returns the uplink->source designation, fetching
// and caching it from the uplinkSourceProvider when the cache is invalidated.
func (c *MACBindingController) getMacBindingSourceForUplinks() map[string]string {
	macBindingSourceForUplinksPtr := c.macBindingSourceForUplinks.Load()
	if isValidBindingSourceForUplinks(macBindingSourceForUplinksPtr) {
		return *macBindingSourceForUplinksPtr
	}
	// we need to refetch
	macBindingSourceForUplinks := c.uplinkSourceProvider.GetMacBindingSourceForUplinks()
	if macBindingSourceForUplinks == nil {
		macBindingSourceForUplinks = map[string]string{}
	}
	macBindingSourceForUplinks[""] = c.cdnGatewayPort
	c.macBindingSourceForUplinks.CompareAndSwap(macBindingSourceForUplinksPtr, &macBindingSourceForUplinks)
	return macBindingSourceForUplinks
}

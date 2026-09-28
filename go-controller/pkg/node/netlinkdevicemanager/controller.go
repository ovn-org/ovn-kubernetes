// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package netlinkdevicemanager

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// defaultReconcilePeriod is the default interval for periodic sync of VNI-VID mappings of VXLAN devices.
const defaultReconcilePeriod = 60 * time.Second

// defaultMaxSyncJitter is the maximum random delay added when enqueuing
// devices during periodic sync. Distributes reconciliation over time to
// avoid CPU spikes at scale.
const defaultMaxSyncJitter = 5 * time.Second

// Key prefixes for workqueue item type routing.
// Workqueue deduplicates by key, so rapid updates to the same device coalesce.
const (
	deviceKeyPrefix = "device/"    // e.g., "device/br-evpn"
	fullSyncKey     = "fullsync"   // Channel recreation: orphan cleanup + re-enqueue all devices
	vxlanSyncKey    = "vxlan-sync" // Periodic: re-enqueue VXLAN devices for VNI filter drift recovery
)

// eventChanBufferSize is the buffer size for netlink event channels (link and addr).
// The buffer decouples the kernel socket drain rate from event processing,
// absorbing bursts (e.g., during startup or bulk reconfiguration) without
// tearing down the subscription.
const eventChanBufferSize = 100

// notOwnedError is returned when an operation is blocked because the device
// exists but is not owned by us (no alias or foreign alias).
// This is a permanent error - retrying won't help unless the external device is removed.
type notOwnedError struct {
	deviceName string
	reason     string
}

func (e *notOwnedError) Error() string {
	return fmt.Sprintf("device %s not owned by us: %s", e.deviceName, e.reason)
}

// isNotOwnedError returns true if the error indicates a device ownership conflict.
func isNotOwnedError(err error) bool {
	var notOwned *notOwnedError
	return errors.As(err, &notOwned)
}

// dependencyError indicates a device couldn't be created because a dependency is missing.
type dependencyError struct {
	dependency string
	reason     string
}

func (e *dependencyError) Error() string {
	return fmt.Sprintf("dependency not ready: %s (%s)", e.dependency, e.reason)
}

func isDependencyError(err error) bool {
	var depErr *dependencyError
	return errors.As(err, &depErr)
}

// managedDevice tracks a device with its config and operational state.
type managedDevice struct {
	cfg   managedDeviceConfig // Normalized desired config
	state managedDeviceState  // Operational state
}

// Controller manages Linux network device lifecycle using a workqueue-based reconciler.
// Public API methods store desired state and enqueue work; a single worker goroutine
// performs all netlink I/O. Self-heals via periodic sync (VNI filter drift),
// orphan cleanup, and netlink event-driven reconciliation (link events via
// address events).
type Controller struct {
	mu    sync.RWMutex
	store map[string]*managedDevice // device name -> managed device info

	reconciler controller.Reconciler // workqueue reconciler (single worker, all I/O)
}

// NewController creates a new NetlinkDeviceManager with default settings.
func NewController() *Controller {
	c := &Controller{
		store: make(map[string]*managedDevice),
	}

	c.reconciler = controller.NewReconciler("netlink-device-manager", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.reconcileWorkqueue,
		Threadiness: 1,                           // Single worker — serializes all netlink I/O
		MaxAttempts: controller.InfiniteAttempts, // Self-healing: infinite retries
	})

	return c
}

// EnsureLink stores the desired device configuration and enqueues it for reconciliation.
//
// Callers MUST call EnsureLink for all previously-existing desired devices
// before calling Run. On startup, Run performs orphan cleanup: it scans the
// kernel for devices with our ownership alias that are not in the desired state
// store and deletes them. If the store is empty, all previously-managed devices
// will be removed.
// INVARIANT: This relies on MaxAttempts = InfiniteAttempts to eventually converge.
func (c *Controller) EnsureLink(cfg DeviceConfig) error {
	managed, err := newManagedDeviceConfig(cfg)
	if err != nil {
		return err
	}

	name := managed.Name()

	c.mu.Lock()
	defer c.mu.Unlock()

	if existing := c.store[name]; existing != nil {
		if existing.cfg.Equal(&managed) {
			return nil
		}
	}

	c.store[name] = &managedDevice{cfg: managed}

	c.reconciler.Reconcile(deviceKeyPrefix + name)
	return nil
}

// DeleteLink removes a device from the desired state and enqueues reconciliation.
// The worker will see the device absent from store and delete it from the kernel.
func (c *Controller) DeleteLink(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, wasManaged := c.store[name]
	if !wasManaged {
		return nil
	}

	delete(c.store, name)

	c.reconciler.Reconcile(deviceKeyPrefix + name)
	return nil
}

// Run starts the background reconciler and netlink event listener.
func (c *Controller) Run(stopCh <-chan struct{}, doneWg *sync.WaitGroup) error {
	// Subscribe to netlink events BEFORE starting workers.
	// This ensures no events are missed between worker startup and subscription.
	onSubscribeError := func(err error) {
		klog.Errorf("NetlinkDeviceManager: netlink subscribe error: %v", err)
	}
	linkChan := subscribeLinkEvents(stopCh, onSubscribeError)
	addrChan := subscribeAddrEvents(stopCh, onSubscribeError)

	// Start reconciler with orphan cleanup as initial sync.
	if err := controller.StartWithInitialSync(
		c.cleanupOrphanedDevices,
		c.reconciler,
	); err != nil {
		return fmt.Errorf("failed to start reconciler: %w", err)
	}

	// Queue initial fullsync
	c.reconciler.Reconcile(fullSyncKey)

	doneWg.Add(1)
	go func() {
		defer doneWg.Done()
		defer controller.Stop(c.reconciler)

		syncTimer := time.NewTicker(defaultReconcilePeriod)
		defer syncTimer.Stop()

		for {
			// Exit immediately if stopCh is closed
			// Handle race condition between stopCh and events.
			select {
			case <-stopCh:
				klog.Info("NetlinkDeviceManager: stopping")
				return
			default:
			}

			select {
			case update, ok := <-linkChan:
				// Note: we do NOT reset the periodic sync timer on events.
				// Resetting would starve periodic sync on busy nodes with many
				// netlink events. The periodic sync is a hard safety net.
				if !ok {
					klog.Warning("NetlinkDeviceManager: link channel closed, resubscribing")
					linkChan = subscribeLinkEvents(stopCh, onSubscribeError)
					if linkChan != nil {
						c.reconciler.Reconcile(fullSyncKey)
					}
					continue
				}
				c.handleLinkUpdate(update)

			case update, ok := <-addrChan:
				if !ok {
					klog.Warning("NetlinkDeviceManager: addr channel closed, resubscribing")
					addrChan = subscribeAddrEvents(stopCh, onSubscribeError)
					if addrChan != nil {
						c.reconciler.Reconcile(fullSyncKey)
					}
					continue
				}
				c.handleAddrUpdate(update)

			case <-syncTimer.C:
				var resubscribed bool
				if linkChan == nil {
					linkChan = subscribeLinkEvents(stopCh, onSubscribeError)
					if linkChan != nil {
						resubscribed = true
					}
				}
				if addrChan == nil {
					addrChan = subscribeAddrEvents(stopCh, onSubscribeError)
					if addrChan != nil {
						resubscribed = true
					}
				}
				if resubscribed {
					c.reconciler.Reconcile(fullSyncKey)
				} else {
					c.reconciler.Reconcile(vxlanSyncKey)
				}

			case <-stopCh:
				klog.Info("NetlinkDeviceManager: stopping")
				return
			}
		}
	}()

	klog.Info("NetlinkDeviceManager: running")
	return nil
}

// subscribeLinkEvents creates a buffered channel and subscribes to netlink link
// events. Returns the channel on success or nil on failure.
func subscribeLinkEvents(stopCh <-chan struct{}, onError func(error)) chan netlink.LinkUpdate {
	ch := make(chan netlink.LinkUpdate, eventChanBufferSize)
	options := netlink.LinkSubscribeOptions{
		ErrorCallback: onError,
	}
	if err := netlink.LinkSubscribeWithOptions(ch, stopCh, options); err != nil {
		onError(err)
		return nil
	}
	return ch
}

// subscribeAddrEvents creates a buffered channel and subscribes to netlink address
// events. Returns the channel on success or nil on failure.
func subscribeAddrEvents(stopCh <-chan struct{}, onError func(error)) chan netlink.AddrUpdate {
	ch := make(chan netlink.AddrUpdate, eventChanBufferSize)
	options := netlink.AddrSubscribeOptions{
		ErrorCallback: onError,
	}
	if err := netlink.AddrSubscribeWithOptions(ch, stopCh, options); err != nil {
		onError(err)
		return nil
	}
	return ch
}

// handleLinkUpdate enqueues reconciliation for devices affected by a netlink event.
// The update's Header.Type distinguishes create/update (unix.RTM_NEWLINK) from
// delete (unix.RTM_DELLINK).
//
// For non-delete events, device reconciliation is skipped when the event link either
// does not carry our ownership alias (nothing actionable, the device is not
// ours) or already matches the desired state (avoids redundant reconciliation
// after self-triggered changes). Delete events are always reconciled because they
// signal that an external blocker may have disappeared or that our device was
// removed.
func (c *Controller) handleLinkUpdate(update netlink.LinkUpdate) {
	linkName := update.Link.Attrs().Name
	klog.V(5).Infof("NetlinkDeviceManager: link event for %s (nlmsg_type=%d)", linkName, update.Header.Type)

	c.mu.RLock()
	defer c.mu.RUnlock()

	if d, exists := c.store[linkName]; exists {
		if update.Header.Type == unix.RTM_DELLINK || (isOurDevice(update.Link) && !d.linkStateEquals(update.Link)) {
			c.reconciler.Reconcile(deviceKeyPrefix + linkName)
		} else {
			klog.V(5).Infof("NetlinkDeviceManager: skipping reconciliation for device %s on event %d", linkName, update.Header.Type)
		}
	}

	// Queue devices that depend on this link (regardless if we own the link or not).
	for name, device := range c.store {
		if device.cfg.Master == linkName || device.cfg.VLANParent == linkName {
			c.reconciler.Reconcile(deviceKeyPrefix + name)
		}
	}
}

// linkStateEquals compares netlink event's Link against the desired DeviceConfig,
// covering link-level attributes only.
//
// Intentionally composed from the same building blocks the reconciler uses so
// that adding a field to any building block automatically covers both paths.
// The remaining checks (FlagUp, master presence) are one-off checks unlikely to
// grow new fields.
func (d *managedDevice) linkStateEquals(event netlink.Link) bool {
	cfg := &d.cfg
	// Guard against unsupported link types from netlink events.
	if event.Type() != cfg.Link.Type() {
		return false
	}
	if !linkEqual(normalizeLinkState(cfg.Link, event), cfg.Link) {
		return false
	}
	if event.Attrs().Flags&net.FlagUp == 0 {
		return false
	}
	// Compare against the master's ifindex tracked in the store (updated
	// atomically during reconciliation I/O).
	if event.Attrs().MasterIndex != d.state.getMasterIfindex() {
		return false
	}
	// Bridge port setting changes include IFLA_PROTINFO in the RTM_NEWLINK
	// event. When present, compare against desired; when absent, the event
	// was about link-level attributes and bridge port settings were not
	// modified.
	if cfg.BridgePortSettings != nil {
		if pi := event.Attrs().Protinfo; pi != nil {
			if !bridgePortSettingsMatch(pi, cfg.BridgePortSettings) {
				return false
			}
		}
	}
	return true
}

// handleAddrUpdate enqueues reconciliation for a device affected by an address change.
//
// AddrUpdate only carries LinkIndex, so we match it against ifindexes tracked in
// the store (populated during reconciliation).
//
// Reconciliation is skipped when the event moves the device closer to (or keeps
// it at) the desired state:
//   - A desired address was added   → already correct, skip
//   - An unwanted address was removed → converging, skip
//
// Reconciliation is triggered when the event diverges from the desired state:
//   - A desired address was removed   → must re-add
//   - An unwanted address was added   → must remove
func (c *Controller) handleAddrUpdate(update netlink.AddrUpdate) {
	if isLinkLocalAddress(update.LinkAddress.IP) {
		return
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	for name, device := range c.store {
		// If the ifindex is not set, the device is undergoing reconciliation.
		// Events dropped due to ifindex not yet set (i.e. following equality fails)
		// are harmless since after the ifindex is set a subsequent address sync is performed during
		// the reconciliation process.
		if device.state.getIfindex() == update.LinkIndex {
			if !addrUpdateRequiresReconcile(device.cfg.Addresses, &update) {
				klog.V(5).Infof("NetlinkDeviceManager: skipping addr update for %s (addr %s, added=%v)",
					name, update.LinkAddress.String(), update.NewAddr)
				continue
			}
			klog.V(5).Infof("NetlinkDeviceManager: addr update for %s (ifindex %d, addr %s, added=%v)",
				name, update.LinkIndex, update.LinkAddress.String(), update.NewAddr)
			c.reconciler.Reconcile(deviceKeyPrefix + name)
		}
	}
}

// reconcileWorkqueue routes workqueue items to appropriate handlers based on key prefix.
// This is the single entry point for all I/O — called by the workqueue worker goroutine.
func (c *Controller) reconcileWorkqueue(_ context.Context, key string) error {
	klog.V(5).Infof("NetlinkDeviceManager: reconciling %s", key)
	switch {
	case strings.HasPrefix(key, deviceKeyPrefix):
		return c.reconcileDeviceKey(strings.TrimPrefix(key, deviceKeyPrefix))
	case key == fullSyncKey:
		return c.reconcileFullSyncKey()
	case key == vxlanSyncKey:
		return c.reconcileVxlanSyncKey()
	default:
		klog.Warningf("NetlinkDeviceManager: unknown reconcile key: %s", key)
		return nil
	}
}

// reconcileDeviceKey is the core device reconciler.
// Handles both create/update (device in store) and delete (device not in store).
func (c *Controller) reconcileDeviceKey(name string) error {
	start := time.Now()
	defer func() {
		klog.V(5).Infof("NetlinkDeviceManager: reconcile %s took %v", name, time.Since(start))
	}()

	c.mu.RLock()
	device, exists := c.store[name]
	if !exists {
		c.mu.RUnlock()
		// Not desired — delete from kernel if present.
		err := deleteDevice(name)
		if err != nil && !isNotOwnedError(err) {
			return err // rate-limited retry
		}
		return nil
	}
	// Snapshot config for lock-free I/O. Clone the config so downstream
	// functions can mutate it freely.
	cfg := device.cfg.Clone()
	c.mu.RUnlock()

	// All netlink I/O OUTSIDE lock.
	// The state pointer is passed through so I/O functions can update
	// ifindex/masterIfindex atomically during I/O.
	err := applyDeviceConfig(&cfg, &device.state)
	if isDependencyError(err) || isNotOwnedError(err) {
		// Dependency and ownership errors return nil: no rate-limited retry,
		// re-triggered by netlink events.
		return nil
	}
	return err
}

// cleanupOrphanedDevices scans kernel for devices with our alias that are NOT in the
// desired state store, and deletes them. Used during startup/resubscribe.
//
// Note: a device could be re-desired (via EnsureLink) between the scan and the delete.
// We don't re-check for this — if we delete a re-desired device, the worker will
// recreate it immediately since EnsureLink already enqueued the key.
func (c *Controller) cleanupOrphanedDevices() error {
	// Scan kernel — I/O, no lock needed
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return fmt.Errorf("failed to list links: %w", err)
	}

	// Find orphans — check store under RLock (read-only check)
	c.mu.RLock()
	var orphans []netlink.Link
	for _, link := range links {
		if isOurDevice(link) {
			name := link.Attrs().Name
			if _, desired := c.store[name]; !desired {
				orphans = append(orphans, link)
			}
		}
	}
	c.mu.RUnlock()

	// Delete orphans
	var deleted int
	for _, link := range orphans {
		name := link.Attrs().Name
		klog.V(5).Infof("NetlinkDeviceManager: deleting orphaned device %s", name)
		if err := cleanupBridgeSelfVLANs(link); err != nil {
			klog.Warningf("NetlinkDeviceManager: bridge self VLAN cleanup failed for orphan %s: %v", name, err)
		}
		if err := util.GetNetLinkOps().LinkDelete(link); err != nil {
			klog.Errorf("NetlinkDeviceManager: failed to delete orphan %s: %v", name, err)
		} else {
			deleted++
		}
	}
	if deleted > 0 {
		klog.Infof("NetlinkDeviceManager: cleaned up %d orphaned devices", deleted)
	}
	return nil
}

// reconcileFullSyncKey runs orphan cleanup then re-enqueues ALL managed devices.
func (c *Controller) reconcileFullSyncKey() error {
	count := c.enqueueDevices(nil)
	klog.V(5).Infof("NetlinkDeviceManager: full sync enqueued %d device(s) (max jitter: %v)", count, defaultMaxSyncJitter)
	return nil
}

// reconcileVxlanSyncKey re-enqueues VXLAN devices for VNI filter drift recovery.
// Only VXLAN devices need periodic sync because VNI filter changes emit
// RTNLGRP_TUNNEL events, which the netlink library has no subscription API for.
// Link and address attributes are covered by their respective event subscriptions.
func (c *Controller) reconcileVxlanSyncKey() error {
	isVxlan := func(d *managedDevice) bool {
		_, ok := d.cfg.Link.(*netlink.Vxlan)
		return ok
	}
	count := c.enqueueDevices(isVxlan)
	if count > 0 {
		klog.V(5).Infof("NetlinkDeviceManager: periodic sync enqueued %d VXLAN device(s) (max jitter: %v)", count, defaultMaxSyncJitter)
	}
	return nil
}

// enqueueDevices iterates the store and enqueues matching devices for reconciliation
// with a random delay in [0, defaultMaxSyncJitter). If filter is nil, all devices are enqueued.
// Returns the number of devices enqueued.
func (c *Controller) enqueueDevices(filter func(*managedDevice) bool) int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var count int
	for name, device := range c.store {
		if filter != nil && !filter(device) {
			continue
		}
		var jitter time.Duration
		if defaultMaxSyncJitter > 0 {
			jitter = rand.N(defaultMaxSyncJitter)
		}
		c.reconciler.ReconcileAfter(deviceKeyPrefix+name, jitter)
		count++
	}
	return count
}

// managedDeviceState holds operational state updated atomically by the reconciler
// during I/O — specifically BEFORE operations that generate events dependent on
// those values. This allows event handlers to filter accurately even while
// reconciliation is in-flight.
type managedDeviceState struct {
	ifindex       atomic.Int64 // Kernel ifindex, updated during reconciliation I/O
	masterIfindex atomic.Int64 // Master's kernel ifindex, updated during reconciliation I/O (0 = no master)
}

func (s *managedDeviceState) setIfindex(v int)       { s.ifindex.Store(int64(v)) }
func (s *managedDeviceState) getIfindex() int        { return int(s.ifindex.Load()) }
func (s *managedDeviceState) setMasterIfindex(v int) { s.masterIfindex.Store(int64(v)) }
func (s *managedDeviceState) getMasterIfindex() int  { return int(s.masterIfindex.Load()) }

// managedDeviceConfig wraps DeviceConfig.
// The exported DeviceConfig stays a plain data struct for the public API.
// This wrapper owns the internal behavior.
type managedDeviceConfig struct {
	DeviceConfig
}

// newManagedDeviceConfig validates, clones, and normalizes DeviceConfig,
// returning managedDeviceConfig. Normalization strips unsupported
// link fields.
func newManagedDeviceConfig(cfg DeviceConfig) (managedDeviceConfig, error) {
	if err := validateConfig(&cfg); err != nil {
		return managedDeviceConfig{}, err
	}
	m := managedDeviceConfig{DeviceConfig: cloneDeviceConfig(&cfg)}
	m.Link.Attrs().Alias = m.Alias()
	return m, nil
}

// Clone returns a deep copy of the managedDeviceConfig.
func (m *managedDeviceConfig) Clone() managedDeviceConfig {
	return managedDeviceConfig{DeviceConfig: cloneDeviceConfig(&m.DeviceConfig)}
}

// cloneDeviceConfig returns a deep copy of a DeviceConfig.
// NOTE: Uses cloneLink wich copies only the fields NDM manages.
func cloneDeviceConfig(cfg *DeviceConfig) DeviceConfig {
	out := *cfg
	out.Link = cloneLink(cfg.Link)
	out.Addresses = slices.Clone(cfg.Addresses)
	out.VIDVNIMappings = slices.Clone(cfg.VIDVNIMappings)
	if cfg.BridgePortSettings != nil {
		out.BridgePortSettings = ptr.To(*cfg.BridgePortSettings)
	}
	return out
}

// Name returns the device name.
func (m *managedDeviceConfig) Name() string {
	return m.Link.Attrs().Name
}

// Alias returns the ownership alias string.
// Format: "ovn-k8s-ndm:<type>:<name>" for debugging and collision avoidance.
func (m *managedDeviceConfig) Alias() string {
	return managedAliasPrefix + m.Link.Type() + ":" + m.Name()
}

// Equal compares two managedDeviceConfigs for equality of all managed fields.
func (a *managedDeviceConfig) Equal(b *managedDeviceConfig) bool {
	if (a.Link == nil) != (b.Link == nil) {
		return false
	}

	if a.Name() != b.Name() ||
		a.Master != b.Master ||
		a.VLANParent != b.VLANParent ||
		!ptr.Equal(a.BridgePortSettings, b.BridgePortSettings) {
		return false
	}

	if !linkEqual(a.Link, b.Link) {
		return false
	}

	if !addressesEqual(a.Addresses, b.Addresses) {
		return false
	}

	if !vidVNIMappingsEqual(a.VIDVNIMappings, b.VIDVNIMappings) {
		return false
	}

	return true
}

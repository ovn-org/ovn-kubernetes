// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vrfmanager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// reconcile period for vrf manager, this would kick in for every 60 seconds if there is no
// explicit link update events. In the event of link update, reconcile period is automatically
// extended by another 60 seconds.
var reconcilePeriod = 60 * time.Second

type vrf struct {
	name  string
	table uint32
	// managedSlaves are the desired netlink interfaces whose master will be
	// this VRF.
	managedSlaves sets.Set[string]
	routes        []netlink.Route
}

// VRFSlaveConflictError reports that an interface is already assigned to a
// different VRF and cannot be moved without disrupting its current network.
type VRFSlaveConflictError struct {
	Interface    string
	RequestedVRF string
	ExistingVRF  string
}

func (e *VRFSlaveConflictError) Error() string {
	return fmt.Sprintf("interface %s is already assigned to VRF %s and cannot be assigned to VRF %s",
		e.Interface, e.ExistingVRF, e.RequestedVRF)
}

type Controller struct {
	mu           *sync.Mutex
	vrfs         map[int]vrf
	routeManager *routemanager.Controller
}

func NewController(routeManager *routemanager.Controller) *Controller {
	return &Controller{
		mu:           &sync.Mutex{},
		vrfs:         make(map[int]vrf),
		routeManager: routeManager,
	}
}

// Run starts the VRF Manager to manage its devices
func (vrfm *Controller) Run(stopCh <-chan struct{}, doneWg *sync.WaitGroup) error {
	linkSubscribeOptions := netlink.LinkSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.Errorf("Failed during LinkSubscribe callback: %v", err)
			// Note: Not calling sync() from here: it is redundant and unsafe when stopChan is closed.
		},
	}

	subscribe := func() (bool, chan netlink.LinkUpdate, error) {
		linkChan := make(chan netlink.LinkUpdate)
		if err := netlink.LinkSubscribeWithOptions(linkChan, stopCh, linkSubscribeOptions); err != nil {
			return false, nil, err
		}
		// Ensure VRFs are in sync while subscribing for Link events.
		err := vrfm.reconcile()
		if err != nil {
			klog.Errorf("VRF Manager: Error while reconciling VRFs, err: %v", err)
		}
		return true, linkChan, nil
	}
	return vrfm.runInternal(stopCh, doneWg, subscribe)
}

type subscribeFn func() (bool, chan netlink.LinkUpdate, error)

func (vrfm *Controller) runInternal(stopChan <-chan struct{}, doneWg *sync.WaitGroup,
	subscribe subscribeFn) error {
	// Get the current network namespace handle
	currentNs, err := ns.GetCurrentNS()
	if err != nil {
		return fmt.Errorf("error retrieving current net namespace, err: %v", err)
	}
	subscribed, linkUpdateCh, err := subscribe()
	if err != nil {
		if closeErr := currentNs.Close(); closeErr != nil {
			klog.Warningf("VRF Manager: failed to close namespace handle: %v", closeErr)
		}
		return fmt.Errorf("error during netlink subscribe, err: %v", err)
	}
	doneWg.Add(1)
	go func() {
		defer doneWg.Done()
		defer func() {
			if err := currentNs.Close(); err != nil {
				klog.Warningf("VRF Manager: failed to close namespace handle: %v", err)
			}
		}()
		err = currentNs.Do(func(_ ns.NetNS) error {
			linkSyncTimer := time.NewTicker(reconcilePeriod)
			defer linkSyncTimer.Stop()

			for {
				select {
				case linkUpdateEvent, ok := <-linkUpdateCh:
					linkSyncTimer.Reset(reconcilePeriod)
					if !ok {
						if subscribed, linkUpdateCh, err = subscribe(); err != nil {
							klog.Errorf("VRF Manager: Error during netlink re-subscribe due to channel closing: %v", err)
						}
						continue
					}
					ifName := linkUpdateEvent.Link.Attrs().Name
					klog.V(5).Infof("VRF Manager: link update received for interface %s", ifName)
					err = vrfm.syncVRF(linkUpdateEvent.Link)
					if err != nil {
						klog.Errorf("VRF Manager: Error syncing link %s update event, err: %v", ifName, err)
					}

				case <-linkSyncTimer.C:
					klog.V(5).Info("VRF Manager: calling reconcile() explicitly")
					if err = vrfm.reconcile(); err != nil {
						klog.Errorf("VRF Manager: Error while reconciling VRFs, err: %v", err)
					}
					if !subscribed {
						if subscribed, linkUpdateCh, err = subscribe(); err != nil {
							klog.Errorf("VRF Manager: Error during netlink re-subscribe: %v", err)
						}
					}
				case <-stopChan:
					return nil
				}
			}
		})
		if err != nil {
			klog.Errorf("VRF Manager: failed to run link reconcile goroutine, err: %v", err)
		}
	}()
	klog.Info("VRF manager is running")
	return nil
}

// reconcile synchronizes all desired VRFs and removal of stale objects
func (vrfm *Controller) reconcile() error {
	vrfm.mu.Lock()
	defer vrfm.mu.Unlock()
	start := time.Now()
	defer func() {
		klog.V(5).Infof("VRF Manager: reconciling VRFs took %v", time.Since(start))
	}()

	var errorAggregate []error
	validVRFDevices := make(sets.Set[string])
	for _, vrf := range vrfm.vrfs {
		validVRFDevices.Insert(vrf.name)
		err := vrfm.sync(vrf)
		if err != nil {
			errorAggregate = append(errorAggregate, fmt.Errorf("error syncing VRF %s: %v", vrf.name, err))
		}
	}

	// clean up anything stale
	if err := vrfm.repair(validVRFDevices); err != nil {
		errorAggregate = append(errorAggregate, fmt.Errorf("error repairing VRFs: %v", err))
	}

	if len(errorAggregate) > 0 {
		return utilerrors.Join(errorAggregate...)
	}

	return nil
}

func (vrfm *Controller) syncVRF(link netlink.Link) error {
	vrfm.mu.Lock()
	defer vrfm.mu.Unlock()
	vrf, ok := vrfm.vrfs[link.Attrs().Index]
	if !ok {
		return nil
	}
	return vrfm.sync(vrf)
}

// sync ensures that the netlink VRF device exists, and the managedSlaves are
// enslaved to it. It does not handle removal of the VRF or managedSlaves,
// other than if it detects a conflict while adding.
func (vrfm *Controller) sync(vrf vrf) error {
	vrfLink, err := util.GetNetLinkOps().LinkByName(vrf.name)
	var mustRecreate bool
	if err == nil {
		vrfDev, ok := vrfLink.(*netlink.Vrf)
		if !ok {
			return fmt.Errorf("node has another non VRF device with same name %s", vrf.name)
		}
		if vrfDev.Table < uint32(config.OvnKubeNode.RoutingTableIDStart) {
			return fmt.Errorf("node has another VRF device with same name %s that is not managed by ovn-kubernetes", vrf.name)
		}
		if vrfDev.Table != vrf.table {
			klog.Warningf("Found a conflict with existing VRF device table id for VRF device %s, recreating it", vrf.name)
			err = vrfm.deleteVRF(vrfLink)
			if err != nil {
				return fmt.Errorf("failed to delete existing VRF device %s to recreate, err: %w", vrf.name, err)
			}
			mustRecreate = true
		}
	}
	// Create VRF device if it doesn't exist or if it's needed to be recreated.
	if util.GetNetLinkOps().IsLinkNotFoundError(err) || mustRecreate {
		if vrfLink != nil {
			delete(vrfm.vrfs, vrfLink.Attrs().Index)
		}
		vrfLink = &netlink.Vrf{
			LinkAttrs: netlink.LinkAttrs{Name: vrf.name},
			Table:     vrf.table,
		}
		if err = util.GetNetLinkOps().LinkAdd(vrfLink); err != nil {
			return fmt.Errorf("failed to create VRF device %s, err: %v", vrf.name, err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to retrieve existing VRF device %s, err: %v", vrf.name, err)
	}
	vrfLink, err = util.GetNetLinkOps().LinkByName(vrf.name)
	if err != nil {
		return fmt.Errorf("failed to retrieve VRF device %s, err: %v", vrf.name, err)
	}
	if vrfLink.Attrs().OperState != netlink.OperUp {
		if err = util.GetNetLinkOps().LinkSetUp(vrfLink); err != nil {
			return fmt.Errorf("failed to get VRF device %s operationally up, err: %v", vrf.name, err)
		}
	}
	for managedSlave := range vrf.managedSlaves {
		alreadyEnslaved, err := isInterfaceSlaveOfVRF(managedSlave, vrfLink.Attrs().Index)
		if err != nil {
			return fmt.Errorf("failed to check if %s is slave of VRF device %s, err: %v", managedSlave, vrfLink.Attrs().Name, err)
		}
		if !alreadyEnslaved {
			if err = enslaveInterfaceToVRF(vrf.name, managedSlave, vrf.table); err != nil {
				return fmt.Errorf("failed to enslave interface %s into VRF device: %s, err: %v", managedSlave, vrf.name, err)
			}
		}
	}
	// Handover vrf routes into route manager to manage it.
	for _, route := range vrf.routes {
		if err = vrfm.routeManager.Add(route); err != nil {
			return fmt.Errorf("failed to add route %v for VRF device %s, err: %w", route, vrf.name, err)
		}
	}

	vrfm.vrfs[vrfLink.Attrs().Index] = vrf
	return nil
}

func newVRF(name string, table uint32, slaveInterface string, routes []netlink.Route) vrf {
	managedSlaves := sets.New[string]()
	if slaveInterface != "" {
		managedSlaves.Insert(slaveInterface)
	}
	return vrf{
		name:          name,
		table:         table,
		managedSlaves: managedSlaves,
		routes:        markOVNKRoutes(routes),
	}
}

// markOVNKRoutes tags the routes with the OVN-Kubernetes protocol, marking
// their ownership in the kernel: route migration across VRF enslavement
// relies on the protocol to tell OVN-Kubernetes routes apart from
// third-party ones, surviving restarts of this process.
func markOVNKRoutes(routes []netlink.Route) []netlink.Route {
	routes = slices.Clone(routes)
	for i := range routes {
		routes[i].Protocol = netlink.RouteProtocol(types.OVNKProtocol)
	}
	return routes
}

// AddVRF adds a VRF device into the node.
func (vrfm *Controller) AddVRF(name string, slaveInterface string, table uint32, routes []netlink.Route) error {
	vrfm.mu.Lock()
	defer vrfm.mu.Unlock()

	if len(name) > 15 {
		return fmt.Errorf("VRF Manager: VRF name %s must be within 15 characters", name)
	}
	if table < uint32(config.OvnKubeNode.RoutingTableIDStart) {
		return fmt.Errorf("VRF Manager: cannot manage a VRF %s with table %d lower than %d", name, table, config.OvnKubeNode.RoutingTableIDStart)
	}
	var (
		vrfDev vrf
		ok     bool
	)
	vrfLink, err := util.GetNetLinkOps().LinkByName(name)
	if vrfLink != nil {
		vrfDev, ok = vrfm.vrfs[vrfLink.Attrs().Index]
		if ok {
			klog.V(5).Infof("VRF Manager: VRF %s already found in the cache", name)
			if slaveInterface != "" && !vrfDev.managedSlaves.Has(slaveInterface) {
				return fmt.Errorf("VRF Manager: slave interface mismatch for VRF device %s", name)
			}
			if vrfDev.table != table {
				return fmt.Errorf("VRF Manager: table id mismatch for VRF device %s", name)
			}
		} else {
			vrfDev = newVRF(name, table, slaveInterface, routes)
		}
	}

	if err != nil && util.GetNetLinkOps().IsLinkNotFoundError(err) {
		vrfDev = newVRF(name, table, slaveInterface, routes)
	} else if err != nil {
		return fmt.Errorf("failed to retrieve VRF device %s, err: %v", name, err)
	}

	return vrfm.sync(vrfDev)
}

// AddVRFSlave adds another slave interface to an existing VRF.
func (vrfm *Controller) AddVRFSlave(name string, slaveInterface string) error {
	vrfm.mu.Lock()
	defer vrfm.mu.Unlock()

	if slaveInterface == "" {
		return nil
	}
	vrfLink, err := util.GetNetLinkOps().LinkByName(name)
	if err != nil {
		return fmt.Errorf("failed to retrieve VRF device %s, err: %v", name, err)
	}
	vrfDev, ok := vrfm.vrfs[vrfLink.Attrs().Index]
	if !ok {
		return fmt.Errorf("failed to find VRF %s", name)
	}
	for _, existingVRF := range vrfm.vrfs {
		if existingVRF.name != name && existingVRF.managedSlaves.Has(slaveInterface) {
			return &VRFSlaveConflictError{
				Interface:    slaveInterface,
				RequestedVRF: name,
				ExistingVRF:  existingVRF.name,
			}
		}
	}

	slaveLink, err := util.GetNetLinkOps().LinkByName(slaveInterface)
	if err != nil {
		return fmt.Errorf("failed to retrieve slave interface %s: %v", slaveInterface, err)
	}
	if slaveLink.Attrs().MasterIndex != 0 && slaveLink.Attrs().MasterIndex != vrfLink.Attrs().Index {
		masterLink, err := util.GetNetLinkOps().LinkByIndex(slaveLink.Attrs().MasterIndex)
		if err != nil {
			return fmt.Errorf("failed to retrieve master for slave interface %s: %v", slaveInterface, err)
		}
		return &VRFSlaveConflictError{
			Interface:    slaveInterface,
			RequestedVRF: name,
			ExistingVRF:  masterLink.Attrs().Name,
		}
	}
	vrfDev.managedSlaves.Insert(slaveInterface)
	return vrfm.sync(vrfDev)
}

// DeleteVRFSlave stops managing a slave interface for an existing VRF.
func (vrfm *Controller) DeleteVRFSlave(name string, slaveInterface string) error {
	vrfm.mu.Lock()
	defer vrfm.mu.Unlock()

	if slaveInterface == "" {
		return nil
	}
	vrfLink, err := util.GetNetLinkOps().LinkByName(name)
	if err != nil {
		return fmt.Errorf("failed to retrieve VRF device %s, err: %v", name, err)
	}
	vrfDev, ok := vrfm.vrfs[vrfLink.Attrs().Index]
	if !ok {
		return fmt.Errorf("failed to find VRF %s", name)
	}
	if err = releaseInterfaceFromVRF(slaveInterface, vrfLink.Attrs().Index, vrfDev.table); err != nil {
		return fmt.Errorf("failed to release interface %s from VRF %s, err: %v",
			slaveInterface, name, err)
	}
	// Stop managing the slave only once released, so that a failed release
	// leaves it managed and a later retry still releases it.
	vrfDev.managedSlaves.Delete(slaveInterface)
	return vrfm.sync(vrfDev)
}

// AddVRFRoutes adds routes to the specified VRF
func (vrfm *Controller) AddVRFRoutes(name string, routes []netlink.Route) error {
	vrfm.mu.Lock()
	defer vrfm.mu.Unlock()

	vrfLink, err := util.GetNetLinkOps().LinkByName(name)
	if err != nil {
		return fmt.Errorf("failed to retrieve VRF device %s, err: %v", name, err)
	}

	vrfDev, ok := vrfm.vrfs[vrfLink.Attrs().Index]
	if !ok {
		return fmt.Errorf("failed to find VRF %s", name)
	}

	// Route manager replaces the kernel route tracked under the same key as
	// a new route, so the cache follows, also within one batch. The clone
	// keeps the stored entry intact if the sync fails.
	tracked := slices.Clone(vrfDev.routes)
	for _, route := range markOVNKRoutes(routes) {
		tracked = slices.DeleteFunc(tracked, func(t netlink.Route) bool {
			return routemanager.SameKey(t, route)
		})
		tracked = append(tracked, route)
	}
	vrfDev.routes = tracked

	return vrfm.sync(vrfDev)
}

// DeleteVRFRoutes deletes a set of routes from a VRF
func (vrfm *Controller) DeleteVRFRoutes(name string, routes []netlink.Route) error {
	vrfm.mu.Lock()
	defer vrfm.mu.Unlock()

	vrfLink, err := util.GetNetLinkOps().LinkByName(name)
	if err != nil {
		return fmt.Errorf("failed to retrieve VRF device %s, err: %v", name, err)
	}

	vrf, ok := vrfm.vrfs[vrfLink.Attrs().Index]
	if !ok {
		return fmt.Errorf("failed to find VRF %s", name)
	}
	// Every CUDN VRF table holds an unreachable default route (see
	// computeRoutesForUDN) that must outlive the managed default routes.
	// It has no link index, and neither has an ECMP default, so the key
	// records whether a route is unreachable to keep the two apart.
	type route struct {
		LinkIndex   int
		Dst         string
		Table       int
		Unreachable bool
	}
	keyOf := func(r netlink.Route) route {
		return route{
			LinkIndex:   r.LinkIndex,
			Dst:         r.Dst.String(),
			Table:       r.Table,
			Unreachable: r.Type == unix.RTN_UNREACHABLE,
		}
	}
	deletedRoutes := sets.New[route]()
	for _, r := range routes {
		// Delete the explicitly requested kernel route even when it is not
		// present in the tracked VRF routes, e.g. a route discovered in the
		// kernel by its protocol after a restart. routeManager.Del also
		// forgets a tracked route with the same key when one exists.
		if err = vrfm.routeManager.Del(r); err != nil {
			break
		}
		deletedRoutes.Insert(keyOf(r))
	}

	vrf.routes = slices.DeleteFunc(vrf.routes, func(r netlink.Route) bool {
		return deletedRoutes.Has(keyOf(r))
	})
	vrfm.vrfs[vrfLink.Attrs().Index] = vrf
	return err
}

// Repair deletes stale VRF device(s) on the host. This helps remove
// device(s) for which DeleteVRF is never invoked.
func (vrfm *Controller) Repair(validVRFs sets.Set[string]) error {
	vrfm.mu.Lock()
	defer vrfm.mu.Unlock()

	return vrfm.repair(validVRFs)
}

func (vrfm *Controller) repair(validVRFs sets.Set[string]) error {
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return fmt.Errorf("failed to list links on the node, err: %v", err)
	}

	for _, link := range links {
		vrf, isVRF := link.(*netlink.Vrf)
		if !isVRF {
			// not a vrf device
			continue
		}
		if vrf.Table < uint32(config.OvnKubeNode.RoutingTableIDStart) {
			// vrf device not managed by us
			continue
		}
		name := vrf.Name
		if validVRFs.Has(name) {
			// vrf not stale
			continue
		}
		err = vrfm.deleteVRF(link)
		if err != nil {
			klog.Errorf("VRF Manager: error deleting stale VRF device %s, err: %v", name, err)
			continue
		}
		delete(vrfm.vrfs, vrf.Index)
	}
	return nil
}

// DeleteVRF deletes given VRF device from the node.
func (vrfm *Controller) DeleteVRF(name string) (err error) {
	vrfm.mu.Lock()
	var vrfLink netlink.Link
	defer func() {
		if err == nil && vrfLink != nil {
			delete(vrfm.vrfs, vrfLink.Attrs().Index)
		}
		vrfm.mu.Unlock()
	}()
	vrfLink, err = util.GetNetLinkOps().LinkByName(name)
	if util.GetNetLinkOps().IsLinkNotFoundError(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to retrieve VRF device %s, err: %v", name, err)
	}
	vrf, ok := vrfm.vrfs[vrfLink.Attrs().Index]
	if !ok {
		klog.V(5).Infof("VRF Manager: VRF %s not found in cache for deletion", name)
		return nil
	}

	// Request route manager to delete vrf associated routes.
	for _, route := range vrf.routes {
		if err = vrfm.routeManager.Del(route); err != nil {
			return fmt.Errorf("failed to delete route %v for VRF device %s, err: %w", route, vrf.name, err)
		}
	}

	err = vrfm.deleteVRF(vrfLink)
	if err != nil {
		return fmt.Errorf("failed to delete VRF device %s, err: %w", vrf.name, err)
	}
	return nil
}

func (vrfm *Controller) deleteVRF(link netlink.Link) error {
	vrfLink, isVRF := link.(*netlink.Vrf)
	if !isVRF {
		return fmt.Errorf("node has another non VRF device with same name %s, refusing to delete it",
			link.Attrs().Name)
	}
	// Release enslaved interfaces first so that their routes are preserved
	// into the main table; deleting the VRF device would otherwise purge them
	// along with the VRF routing table. Slaves and table are derived from
	// kernel state rather than the cache, so that this also covers VRF
	// devices the cache does not know about (e.g. a stale VRF repaired after
	// a restart) or caches with a different, desired table id (recreation on
	// table conflict).
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return fmt.Errorf("failed to list links to release slaves of VRF device %s, err: %w", vrfLink.Name, err)
	}
	var errs []error
	for _, l := range links {
		if l.Attrs().MasterIndex != vrfLink.Index {
			continue
		}
		if err := releaseInterfaceFromVRF(l.Attrs().Name, vrfLink.Index, vrfLink.Table); err != nil {
			errs = append(errs, fmt.Errorf("failed to release interface %s from VRF %s, err: %w",
				l.Attrs().Name, vrfLink.Name, err))
		}
	}
	if len(errs) > 0 {
		// Do not delete the device while interfaces are still enslaved:
		// deletion would purge their routes. Callers retry the deletion.
		return utilerrors.Join(errs...)
	}
	return util.GetNetLinkOps().LinkDelete(vrfLink)
}

// isInterfaceSlaveOfVRF checks if a specific interface is enslaved to a VRF
func isInterfaceSlaveOfVRF(ifName string, vrfIndex int) (bool, error) {
	link, err := util.GetNetLinkOps().LinkByName(ifName)
	if err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get link %s, err: %v", ifName, err)
	}
	return link.Attrs().MasterIndex == vrfIndex, nil
}

func enslaveInterfaceToVRF(vrfName, ifName string, table uint32) error {
	klog.V(5).Infof("Enslaving interface %s to VRF: %s", ifName, vrfName)
	iface, err := util.GetNetLinkOps().LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to retrieve interface %s, err: %v", ifName, err)
	}
	vrfLink, err := util.GetNetLinkOps().LinkByName(vrfName)
	if err != nil {
		return fmt.Errorf("failed to retrieve VRF device %s, err: %v", vrfName, err)
	}
	// Changing the interface master makes the kernel purge every FIB entry
	// referencing the interface, regenerating only local and connected routes
	// in the VRF table. Routes installed by other agents (e.g. a DHCP default
	// route on an Uplink interface) would be silently destroyed, so capture
	// them first and restore them into the VRF table after enslavement.
	routes, err := listRoutesForLink(iface, unix.RT_TABLE_MAIN)
	if err != nil {
		return fmt.Errorf("failed to list routes of interface %s before enslaving to VRF %s: %v", ifName, vrfName, err)
	}
	err = util.GetNetLinkOps().LinkSetMaster(iface, vrfLink)
	if err != nil {
		return fmt.Errorf("failed to enslave interface %s to VRF %s: %v", ifName, vrfName, err)
	}
	if restoreErr := restoreRoutesToTable(routes, table); restoreErr != nil {
		// Without its routes the interface is not functional in the VRF, and
		// nothing could recover them later: the captured routes are the only
		// remaining copy once the kernel purged the originals. Undo the
		// enslavement and put the routes back in the main table, so that the
		// returned error surfaces the failure and the next reconcile retries
		// the whole enslavement from a clean state.
		if err := util.GetNetLinkOps().LinkSetNoMaster(iface); err != nil {
			return utilerrors.Join(
				fmt.Errorf("failed to restore routes of interface %s into VRF %s: %w", ifName, vrfName, restoreErr),
				fmt.Errorf("failed to undo the enslavement of interface %s: %w", ifName, err))
		}
		if err := restoreRoutesToTable(routes, unix.RT_TABLE_MAIN); err != nil {
			klog.Errorf("VRF Manager: failed to restore routes of interface %s back to the main table "+
				"after undoing its enslavement: %v", ifName, err)
		}
		return fmt.Errorf("failed to restore routes of interface %s into VRF %s, undid the enslavement: %w",
			ifName, vrfName, restoreErr)
	}
	return nil
}

func releaseInterfaceFromVRF(ifName string, vrfIndex int, table uint32) error {
	iface, err := util.GetNetLinkOps().LinkByName(ifName)
	if err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("failed to retrieve interface %s, err: %v", ifName, err)
	}
	if iface.Attrs().MasterIndex != vrfIndex {
		return nil
	}
	klog.V(5).Infof("Releasing interface %s from VRF", ifName)
	// Releasing the interface from the VRF purges its routes from the VRF
	// table just like enslaving purged them from the main table. Capture them
	// and restore them into the main table; the routes that ovn-kubernetes
	// itself programmed are skipped by their protocol.
	routes, err := listRoutesForLink(iface, int(table))
	if err != nil {
		return fmt.Errorf("failed to list routes of interface %s before releasing from VRF: %v", ifName, err)
	}
	if err = util.GetNetLinkOps().LinkSetNoMaster(iface); err != nil {
		return err
	}
	// Log-only: the release already happened, and a returned error could not
	// be retried meaningfully since the captured routes would be gone.
	if err = restoreRoutesToTable(routes, unix.RT_TABLE_MAIN); err != nil {
		klog.Warningf("VRF Manager: failed to restore routes of interface %s released from VRF: %v", ifName, err)
	}
	return nil
}

// listRoutesForLink returns the routes of both IP families that reference the
// given link in the given routing table.
func listRoutesForLink(link netlink.Link, table int) ([]netlink.Route, error) {
	// Filter by interface in code rather than with RT_FILTER_OIF: the kernel
	// filter only compares the top-level link index, which is 0 for
	// multipath routes (e.g. static ECMP gateway routes through the
	// interface), whose interfaces are carried in the nexthops instead.
	// Multipath routes spanning other interfaces as well are left out: they
	// cannot follow the interface across routing tables.
	filter := &netlink.Route{Table: table}
	routes, err := util.GetNetLinkOps().RouteListFiltered(netlink.FAMILY_ALL, filter, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, err
	}
	return util.FilterRoutesByIfIndex(routes, link.Attrs().Index), nil
}

// shouldSkipRouteMigration returns true for routes that must not follow the
// interface across routing tables. Non-unicast routes stay: the kernel
// derives the local, broadcast, anycast and multicast entries from the
// interface's addresses in whichever routing domain the interface currently
// is, so a migrated copy would duplicate entries the kernel maintains itself.
// Also skipped are routes whose owning daemon programs them per routing
// domain on its own: OVN-Kubernetes (and OVN) program their routes in the
// tables they belong to, and routing daemons (FRR/zebra and friends) install
// and withdraw their routes in the routing domain they peer in, so a migrated
// copy would be a stale duplicate that no one manages. Kernel-protocol
// unicast routes are migrated like any other: the kernel only regenerates the
// prefix route of an address after a master change when it installed that
// route itself, which it does not for a noprefixroute address (NetworkManager
// sets noprefixroute on the addresses it manages and installs the prefix
// route on its own, tagged proto kernel, without being VRF-aware). When the
// kernel did regenerate a route, restoring the identical captured copy is a
// benign EEXIST skip.
func shouldSkipRouteMigration(route netlink.Route) bool {
	if route.Type != unix.RTN_UNICAST {
		return true
	}
	switch int(route.Protocol) {
	case types.OVNKProtocol, unix.RTPROT_OVN,
		unix.RTPROT_ZEBRA, unix.RTPROT_BGP, unix.RTPROT_OSPF,
		unix.RTPROT_ISIS, unix.RTPROT_RIP, unix.RTPROT_EIGRP, unix.RTPROT_BABEL:
		return true
	}
	return false
}

// equivalentRouteExists reports whether the route's table holds a route with
// the same kernel key ({dst, priority}) that is effectively the same route:
// same output interface, same gateway or nexthops and same preferred source.
// Such a route is the kernel's regenerated copy of an interface prefix route,
// or one reinstalled by the route's owner, so a failure to restore the
// captured copy over it lost nothing.
func equivalentRouteExists(route netlink.Route) bool {
	dstEqual := func(a, b *net.IPNet) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		return a.String() == b.String()
	}
	routes, err := util.GetNetLinkOps().RouteListFiltered(route.Family,
		&netlink.Route{Table: route.Table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		klog.Warningf("VRF Manager: unable to list routes of table %d: %v", route.Table, err)
		return false
	}
	for _, existing := range routes {
		if existing.Priority != route.Priority || !dstEqual(existing.Dst, route.Dst) {
			continue
		}
		if existing.LinkIndex != route.LinkIndex || !existing.Gw.Equal(route.Gw) ||
			!existing.Src.Equal(route.Src) ||
			len(existing.MultiPath) != len(route.MultiPath) {
			return false
		}
		for i := range existing.MultiPath {
			if existing.MultiPath[i].LinkIndex != route.MultiPath[i].LinkIndex ||
				!existing.MultiPath[i].Gw.Equal(route.MultiPath[i].Gw) ||
				existing.MultiPath[i].Hops != route.MultiPath[i].Hops {
				return false
			}
		}
		return true
	}
	return false
}

// restoreRoutesToTable re-adds routes into the given routing table, preserving
// all their attributes. Routes that their owner reprograms per routing table
// are skipped. Failures are joined and returned so that the caller can decide
// whether they are fatal for the master change they follow.
func restoreRoutesToTable(routes []netlink.Route, table uint32) error {
	// Restore routes without a gateway first: a gateway route is only
	// accepted once the route covering its gateway is in place. The covering
	// route is usually a connected route that the kernel regenerates on its
	// own, but e.g. a DHCP client serving an off-subnet gateway installs a
	// host route toward it, and that one is migrated like any other.
	routes = slices.Clone(routes)
	slices.SortStableFunc(routes, func(a, b netlink.Route) int {
		gatewayRoutes := func(r netlink.Route) int {
			if len(r.Gw) > 0 {
				return 1
			}
			for _, nh := range r.MultiPath {
				if len(nh.Gw) > 0 {
					return 1
				}
			}
			return 0
		}
		return gatewayRoutes(a) - gatewayRoutes(b)
	})
	var errs []error
	for _, route := range routes {
		if shouldSkipRouteMigration(route) {
			continue
		}
		route.Table = int(table)
		// Keep only the configuration flags relevant to route installation:
		// the kernel rejects requests that carry its own state flags, e.g.
		// RTNH_F_LINKDOWN captured from a carrier-down interface. Multipath
		// nexthops carry their own flags; copy them before masking, the
		// captured originals are shared with the caller.
		route.Flags &= unix.RTNH_F_ONLINK
		if len(route.MultiPath) > 0 {
			nexthops := make([]*netlink.NexthopInfo, 0, len(route.MultiPath))
			for _, nexthop := range route.MultiPath {
				nexthopCopy := *nexthop
				nexthopCopy.Flags &= unix.RTNH_F_ONLINK
				nexthops = append(nexthops, &nexthopCopy)
			}
			route.MultiPath = nexthops
		}
		if err := addRouteWithRetry(&route); err != nil {
			if util.GetNetLinkOps().IsAlreadyExistsError(err) {
				// A route with the same kernel key ({table, dst, tos, priority})
				// is already in the table and legitimately wins: nothing was
				// added. When the existing
				// route is an equivalent copy -- the kernel regenerating an
				// interface's prefix route, or the route's owner reinstalling
				// it -- nothing was lost. Otherwise a restore into the main
				// table leaves the captured route in no table at all, so make
				// that visible.
				if table == unix.RT_TABLE_MAIN && !equivalentRouteExists(route) {
					klog.Warningf("VRF Manager: route %v not restored into the main table, a different route with the same key already exists", route)
				} else {
					klog.V(5).Infof("VRF Manager: route %v already present in table %d, not restored", route, table)
				}
				continue
			}
			errs = append(errs, fmt.Errorf("failed to restore route %v into table %d: %w", route, table, err))
			continue
		}
		klog.V(5).Infof("VRF Manager: restored route %v into table %d", route, table)
	}
	return utilerrors.Join(errs...)
}

const (
	// routeRestoreTimeout bounds how long a route restore is retried while
	// the kernel deems the route's gateway or source address unusable. It
	// has to cover duplicate address detection (about a second with the
	// default dad_transmits) plus the addrconf work queue latency, with
	// headroom for loaded nodes.
	routeRestoreTimeout      = 4 * time.Second
	routeRestorePollInterval = 100 * time.Millisecond
)

// addRouteWithRetry adds the route, retrying briefly while the kernel deems
// its gateway unreachable (EHOSTUNREACH/ENETUNREACH) or its source address
// invalid (EINVAL): IPv6 connected routes are regenerated asynchronously
// after a master change and IPv6 addresses re-run duplicate address
// detection, so a route restored right after — a gateway route, or one
// carrying a still-tentative IPv6 source — can transiently fail the kernel's
// validation.
func addRouteWithRetry(route *netlink.Route) error {
	var err error
	_ = wait.PollUntilContextTimeout(context.Background(), routeRestorePollInterval, routeRestoreTimeout, true,
		func(context.Context) (bool, error) {
			err = util.GetNetLinkOps().RouteAdd(route)
			return err == nil || !(errors.Is(err, unix.EHOSTUNREACH) || errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EINVAL)), nil
		})
	return err
}

package vrfmanager

import (
	"fmt"

	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// reconcile period for vrf manager, this would kick in for every 60 seconds if there is no
// explicit link update events. In the event of link update, reconcile period is automatically
// extended by another 60 seconds.
var reconcilePeriod = 60 * time.Second

type vrf struct {
	name  string
	table uint32
	// managedSlave is the desired netlink interface who's master will be this VRF.
	// It cannot be changed after VRF creation.
	managedSlave string
	routes       []netlink.Route
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
		return fmt.Errorf("error during netlink subscribe, err: %v", err)
	}
	doneWg.Add(1)
	go func() {
		defer doneWg.Done()
		err = currentNs.Do(func(netNS ns.NetNS) error {
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

// sync ensures that the netlink VRF device exists, and the managedSlave is enslaved to it.
// It does not handle removal of the VRF or managedSlave, other than if it detects a conflict while adding.
func (vrfm *Controller) sync(vrf vrf) error {
	vrfLink, err := util.GetNetLinkOps().LinkByName(vrf.name)
	var mustRecreate bool
	if err == nil {
		vrfDev, ok := vrfLink.(*netlink.Vrf)
		if !ok {
			return fmt.Errorf("node has another non VRF device with same name %s", vrf.name)
		}
		if vrfDev.Table < util.RoutingTableIDStart {
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
	if len(vrf.managedSlave) > 0 {
		existingSlaves, err := getSlaveInterfaceNamesForVRF(vrfLink)
		if err != nil {
			return fmt.Errorf("failed to get existing slaves for VRF device %s, err: %v", vrfLink.Attrs().Name, err)
		}
		if !existingSlaves.Has(vrf.managedSlave) {
			if err = enslaveInterfaceToVRF(vrf.name, vrf.managedSlave); err != nil {
				return fmt.Errorf("failed to enslave interface %s into VRF device: %s, err: %v", vrf.managedSlave, vrf.name, err)
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

// AddVRF adds a VRF device into the node.
func (vrfm *Controller) AddVRF(name string, slaveInterface string, table uint32, routes []netlink.Route) error {
	vrfm.mu.Lock()
	defer vrfm.mu.Unlock()

	if len(name) > 15 {
		return fmt.Errorf("VRF Manager: VRF name %s must be within 15 characters", name)
	}
	if table < util.RoutingTableIDStart {
		return fmt.Errorf("VRF Manager: cannot manage a VRF %s with table %d lower than %d", name, table, util.RoutingTableIDStart)
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
			if vrfDev.managedSlave != slaveInterface {
				return fmt.Errorf("VRF Manager: slave interface mismatch for VRF device %s", name)
			}
			if vrfDev.table != table {
				return fmt.Errorf("VRF Manager: table id mismatch for VRF device %s", name)
			}
		} else {
			vrfDev = vrf{name, table, slaveInterface, routes}
		}
	}

	if err != nil && util.GetNetLinkOps().IsLinkNotFoundError(err) {
		vrfDev = vrf{name, table, slaveInterface, routes}
	} else if err != nil {
		return fmt.Errorf("failed to retrieve VRF device %s, err: %v", name, err)
	}

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

	vrfDev.routes = append(vrfDev.routes, routes...)

	return vrfm.sync(vrfDev)
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
		if vrf.Table < util.RoutingTableIDStart {
			// vrf device not managed by us
			continue
		}
		name := vrf.Name
		if validVRFs.Has(name) {
			// vrf not stale
			continue
		}
		err = util.GetNetLinkOps().LinkDelete(link)
		if err != nil {
			klog.Errorf("VRF Manager: error deleting stale VRF device %s, err: %v", name, err)
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
	return util.GetNetLinkOps().LinkDelete(link)
}

func getSlaveInterfaceNamesForVRF(vrfLink netlink.Link) (sets.Set[string], error) {
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list links on the node, err: %v", err)
	}
	enslavedInterfaces := make(sets.Set[string])
	for _, link := range links {
		if link.Attrs().MasterIndex == vrfLink.Attrs().Index {
			enslavedInterfaces.Insert(link.Attrs().Name)
		}
	}
	return enslavedInterfaces, nil
}

func enslaveInterfaceToVRF(vrfName, ifName string) error {
	klog.V(5).Infof("Enslaving interface %s to VRF: %s", ifName, vrfName)
	iface, err := util.GetNetLinkOps().LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to retrieve interface %s, err: %v", ifName, err)
	}
	vrfLink, err := util.GetNetLinkOps().LinkByName(vrfName)
	if err != nil {
		return fmt.Errorf("failed to retrieve VRF device %s, err: %v", vrfName, err)
	}
	err = util.GetNetLinkOps().LinkSetMaster(iface, vrfLink)
	if err != nil {
		return fmt.Errorf("failed to enslave interface %s to VRF %s: %v", ifName, vrfName, err)
	}
	return nil
}

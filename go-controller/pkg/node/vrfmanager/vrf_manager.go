package vrfmanager

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type vrf struct {
	name               string
	table              uint32
	enslavedInterfaces sets.Set[string]
}

type Controller struct {
	mu   *sync.Mutex
	vrfs map[int]vrf
}

func NewController() *Controller {
	return &Controller{
		mu:   &sync.Mutex{},
		vrfs: make(map[int]vrf),
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
			linkSyncTimer := time.NewTicker(60 * time.Second)
			defer linkSyncTimer.Stop()

			for {
				select {
				case linkUpdateEvent, ok := <-linkUpdateCh:
					linkSyncTimer.Reset(60 * time.Second)
					if !ok {
						if subscribed, linkUpdateCh, err = subscribe(); err != nil {
							klog.Errorf("VRF Manager: Error during netlink re-subscribe due to channel closing: %v", err)
						}
						continue
					}
					ifName := linkUpdateEvent.Link.Attrs().Name
					klog.V(3).Infof("VRF Manager: link update received for interface %s", ifName)
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

func (vrfm *Controller) reconcile() error {
	vrfm.mu.Lock()
	defer vrfm.mu.Unlock()
	start := time.Now()
	defer func() {
		klog.V(5).Infof("VRF Manager: reconciling VRFs took %v", time.Since(start))
	}()

	for _, vrf := range vrfm.vrfs {
		err := vrfm.sync(vrf)
		if err != nil {
			klog.Errorf("VRF Manager: error syncing VRF device %s during reconcile, err: %v", vrf.name, err)
		}
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

func (vrfm *Controller) sync(vrf vrf) error {
	vrfLink, err := util.GetNetLinkOps().LinkByName(vrf.name)
	var mustRecreate bool
	if err == nil {
		if vrfLink.Type() != "vrf" {
			return fmt.Errorf("node has another non VRF device with same name %s", vrf.name)
		}
		vrfDev, ok := vrfLink.(*netlink.Vrf)
		if ok && vrfDev.Table != vrf.table {
			klog.Warningf("Found a conflict with existing VRF device table id for VRF device %s, recreating it", vrf.name)
			err = vrfm.deleteVRF(vrf)
			if err != nil {
				return fmt.Errorf("failed to delete existing VRF device %s to recreate, err: %w", vrf.name, err)
			}
			mustRecreate = true
		}
	}
	// Create VRF device if it doesn't exist or if it's needed to be recreated.
	if util.GetNetLinkOps().IsLinkNotFoundError(err) || mustRecreate {
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
	existingEnslaves, err := getSlaveInterfaceNamesForVRF(vrfLink)
	if err != nil {
		return err
	}
	if existingEnslaves.Equal(vrf.enslavedInterfaces) {
		return nil
	}
	for _, iface := range vrf.enslavedInterfaces.UnsortedList() {
		if !existingEnslaves.Has(iface) {
			err = enslaveInterfaceToVRF(vrf.name, iface)
			if err != nil {
				return fmt.Errorf("failed to enslave inteface %s into VRF device: %s, err: %v", iface, vrf.name, err)
			}
		}
	}
	for _, iface := range existingEnslaves.UnsortedList() {
		if !vrf.enslavedInterfaces.Has(iface) {
			err = removeInterfaceFromVRF(vrf.name, iface)
			if err != nil {
				return fmt.Errorf("failed to remove inteface %s from VRF device: %s, err: %v", iface, vrf.name, err)
			}
		}
	}
	vrfm.vrfs[vrfLink.Attrs().Index] = vrf
	return nil
}

// AddVRF adds a VRF device into the node.
func (vrfm *Controller) AddVRF(name string, slaveInterfaces sets.Set[string], table uint32) error {
	vrfm.mu.Lock()
	defer vrfm.mu.Unlock()

	if len(name) > 15 {
		return fmt.Errorf("VRF Manager: VRF name %s must be within 15 characters", name)
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
			if !vrfDev.enslavedInterfaces.Equal(slaveInterfaces) {
				return fmt.Errorf("VRF Manager: enslave interfaces mismatch for VRF device %s", name)
			}
			if vrfDev.table != table {
				return fmt.Errorf("VRF Manager: table id mismatch for VRF device %s", name)
			}
		} else {
			vrfDev = vrf{name, table, slaveInterfaces}
		}
	}

	if err != nil && util.GetNetLinkOps().IsLinkNotFoundError(err) {
		vrfDev = vrf{name, table, slaveInterfaces}
	} else if err != nil {
		return fmt.Errorf("failed to retrieve VRF device %s, err: %v", name, err)
	}

	return vrfm.sync(vrfDev)
}

// Repair deletes stale VRF device(s) on the host. This helps remove
// device(s) for which DeleteVRF is never invoked.
// Assumptions: 1) The validVRFs list must contain device for which AddVRF
// is already invoked. 2) The device name(s) in validVRFs are suffixed
// with -vrf.
func (vrfm *Controller) Repair(validVRFs sets.Set[string]) error {
	vrfm.mu.Lock()
	defer vrfm.mu.Unlock()

	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return fmt.Errorf("failed to list links on the node, err: %v", err)
	}

	for _, link := range links {
		name := link.Attrs().Name
		// Skip if the link is not a vrf type or name is not suffixed with -vrf.
		if link.Type() != "vrf" || !strings.HasSuffix(name, types.UDNVRFDeviceSuffix) {
			continue
		}
		if !validVRFs.Has(name) {
			err = util.GetNetLinkOps().LinkDelete(link)
			if err != nil {
				klog.Errorf("VRF Manager: error deleting stale VRF device %s, err: %v", name, err)
			}
		}
		delete(vrfm.vrfs, link.Attrs().Index)
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
	err = vrfm.deleteVRF(vrf)
	if err != nil {
		return fmt.Errorf("failed to delete VRF device %s, err: %w", vrf.name, err)
	}
	return nil
}

func (vrfm *Controller) deleteVRF(vrf vrf) error {
	link, err := util.GetNetLinkOps().LinkByName(vrf.name)
	if err != nil && util.GetNetLinkOps().IsLinkNotFoundError(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to retrieve VRF device %s, err: %v", vrf.name, err)
	}
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

func removeInterfaceFromVRF(vrfName, ifName string) error {
	iface, err := util.GetNetLinkOps().LinkByName(ifName)
	if err != nil && util.GetNetLinkOps().IsLinkNotFoundError(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to retrieve interface %s, err: %v", ifName, err)
	}
	err = util.GetNetLinkOps().LinkSetNoMaster(iface)
	if err != nil {
		return fmt.Errorf("failed to remove interface %s from VRF %s: %v", ifName, vrfName, err)
	}
	return nil
}

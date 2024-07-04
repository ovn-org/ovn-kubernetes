package vrfmanager

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type vrf struct {
	name       string
	table      uint32
	interfaces sets.Set[string]
	routes     []netlink.Route
	delete     bool
}

type Controller struct {
	routeManager *routemanager.Controller
	mu           *sync.Mutex
	vrfs         map[string]*vrf
	addVrfCh     chan vrf
	delVrfCh     chan vrf
}

func NewController(rc *routemanager.Controller) *Controller {
	return &Controller{
		routeManager: rc,
		mu:           &sync.Mutex{},
		vrfs:         make(map[string]*vrf),
		addVrfCh:     make(chan vrf, 5),
		delVrfCh:     make(chan vrf, 5),
	}
}

// Run starts manages its own VRF devices
func (vm *Controller) Run(stopCh <-chan struct{}, doneWg *sync.WaitGroup) {
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
		// sync the vrfs now with current cache.
		err := vm.reconcile()
		return true, linkChan, err
	}
	vm.runInternal(stopCh, doneWg, subscribe)
}

type subscribeFn func() (bool, chan netlink.LinkUpdate, error)

func (c *Controller) runInternal(stopChan <-chan struct{}, doneWg *sync.WaitGroup, subscribe subscribeFn) {
	doneWg.Add(1)
	go func() {
		defer doneWg.Done()

		linkSyncTimer := time.NewTicker(2 * time.Minute)
		defer linkSyncTimer.Stop()

		subscribed, addrChan, err := subscribe()
		if err != nil {
			klog.Errorf("Vrf manager: Error during netlink subscribe for Link Manager: %v", err)
		}

		for {
			select {
			case a, ok := <-addrChan:
				linkSyncTimer.Reset(2 * time.Minute)
				if !ok {
					if subscribed, addrChan, err = subscribe(); err != nil {
						klog.Errorf("Vrf manager: Error during netlink re-subscribe due to channel closing for Link Manager: %v", err)
					}
					continue
				}
				if err = c.syncLink(a.Link); err != nil {
					klog.Errorf("Vrf manager: Error while syncing link %q: %v", a.Link.Attrs().Name, err)
				}

			case <-linkSyncTimer.C:
				if subscribed {
					klog.V(5).Info("Vrf manager calling sync() explicitly")
					if err = c.reconcile(); err != nil {
						klog.Errorf("Vrf manager: Error while reconciling vrfs: %v", err)
					}
				} else {
					if subscribed, addrChan, err = subscribe(); err != nil {
						klog.Errorf("Vrf manager: Error during netlink re-subscribe for Link Manager: %v", err)
					}
				}
			case <-stopChan:
				return
			}
		}
	}()

	klog.Info("Vrf manager is running")
}

// AddVrf adds a Vrf device into the node.
func (c *Controller) AddVrf(name string, table uint32, interfaces sets.Set[string], routes []netlink.Route) error {
	return c.addVrf(&vrf{name, table, interfaces, routes, false})
}

// DeleteVrf deletes given Vrf device from the node.
func (c *Controller) DeleteVrf(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	vrf, ok := c.vrfs[name]
	if !ok {
		klog.V(5).Infof("Vrf Manager: vrf %s not found in cache for deletion", name)
		return nil
	}
	vrf.delete = true
	return c.delVrf(vrf)
}

func (c *Controller) addVrf(vrf *vrf) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.vrfs[vrf.name]
	if ok {
		klog.V(5).Infof("Vrf Manager: vrf %s already found in the cache", vrf.name)
		return nil
	}
	return c.sync(vrf)
}

func (c *Controller) delVrf(vrf *vrf) (err error) {
	c.mu.Lock()
	defer func() {
		if err == nil {
			delete(c.vrfs, vrf.name)
		}
		c.mu.Unlock()
	}()
	vrf, ok := c.vrfs[vrf.name]
	if !ok {
		klog.V(5).Infof("Vrf Manager: vrf %s not found in cache", vrf.name)
		return nil
	}
	err = c.deleteVRF(vrf)
	return
}

func (c *Controller) reconcile() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	start := time.Now()
	defer func() {
		klog.V(5).Infof("Vrf Manager: reconciling VRFs took %v", time.Since(start))
	}()

	vrfsToKeep := make(map[string]*vrf)
	for _, vrf := range c.vrfs {
		if vrf.delete {
			err := c.deleteVRF(vrf)
			if err != nil {
				return err
			}
			continue
		}
		err := c.sync(vrf)
		if err != nil {
			return err
		}
		vrfsToKeep[vrf.name] = vrf
	}
	c.vrfs = vrfsToKeep
	return nil
}

func (c *Controller) syncLink(link netlink.Link) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	vrf, ok := c.vrfs[link.Attrs().Name]
	if !ok {
		return nil
	}
	return c.sync(vrf)
}

func (c *Controller) sync(vrf *vrf) error {
	if vrf == nil {
		return nil
	}
	vrfLink, err := util.GetNetLinkOps().LinkByName(vrf.name)
	if err == nil {
		if vrfLink.Type() != "vrf" {
			return errors.New("node has another non vrf device with same name")
		}
		vrfDev, ok := vrfLink.(*netlink.Vrf)
		if ok && vrfDev.Table != vrf.table {
			return errors.New("found conflict with existing vrf device table id")
		}
	}
	// Create Vrf device if it doesn't exist.
	if util.GetNetLinkOps().IsLinkNotFoundError(err) {
		vrfLink = &netlink.Vrf{
			LinkAttrs: netlink.LinkAttrs{Name: vrf.name},
			Table:     vrf.table,
		}
		err = util.GetNetLinkOps().LinkAdd(vrfLink)
	}
	if err != nil {
		return err
	}
	vrfLink, err = util.GetNetLinkOps().LinkByName(vrf.name)
	if err != nil {
		return err
	}
	if vrfLink.Attrs().OperState != netlink.OperUp {
		if err = util.GetNetLinkOps().LinkSetUp(vrfLink); err != nil {
			return err
		}
	}
	// Ensure enslave interface(s) are synced up with the cache.
	existingEnslaves := make(sets.Set[string])
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		return err
	}
	for _, link := range links {
		klog.Infof("Vrf Manager: link name %s, master index: %d, vrf index: %d", link.Attrs().Name, link.Attrs().MasterIndex, vrfLink.Attrs().Index)
		if link.Attrs().MasterIndex == vrfLink.Attrs().Index {
			existingEnslaves.Insert(link.Attrs().Name)
		}
	}
	if existingEnslaves.Equal(vrf.interfaces) {
		return nil
	}
	for _, iface := range vrf.interfaces.UnsortedList() {
		if !existingEnslaves.Has(iface) {
			err = enslaveInterfaceToVRF(vrf.name, iface)
			if err != nil {
				return fmt.Errorf("failed to enslave inteface %s into vrf device: %s, err: %v", iface, vrf.name, err)
			}
		}
	}
	for _, iface := range existingEnslaves.UnsortedList() {
		if !vrf.interfaces.Has(iface) {
			err = removeInterfaceFromVRF(vrf.name, iface)
			if err != nil {
				return fmt.Errorf("failed to remove inteface %s from vrf device: %s, err: %v", iface, vrf.name, err)
			}
		}
	}
	// Handover vrf routes into route manager to manage it.
	for _, route := range vrf.routes {
		c.routeManager.Add(route)
	}
	return nil
}

func (c *Controller) deleteVRF(vrf *vrf) error {
	if vrf == nil {
		return nil
	}
	link, err := util.GetNetLinkOps().LinkByName(vrf.name)
	if err != nil && util.GetNetLinkOps().IsLinkNotFoundError(err) {
		return nil
	} else if err != nil {
		return err
	}
	// Request route manager to delete vrf associated routes.
	for _, route := range vrf.routes {
		c.routeManager.Del(route)
	}
	return util.GetNetLinkOps().LinkDelete(link)
}

func enslaveInterfaceToVRF(vrfName, ifName string) error {
	iface, err := util.GetNetLinkOps().LinkByName(ifName)
	if err != nil {
		return err
	}
	vrfLink, err := util.GetNetLinkOps().LinkByName(vrfName)
	if err != nil {
		return err
	}
	err = util.GetNetLinkOps().LinkSetMaster(iface, vrfLink)
	if err != nil {
		return fmt.Errorf("failed to enslave interface %s to VRF %s: %v", ifName, vrfName, err)
	}
	return nil
}

func removeInterfaceFromVRF(vrfName, ifName string) error {
	iface, err := util.GetNetLinkOps().LinkByName(ifName)
	if err != nil {
		return err
	}
	err = util.GetNetLinkOps().LinkSetMaster(iface, nil)
	if err != nil {
		return fmt.Errorf("failed to remove interface %s from VRF %s: %v", ifName, vrfName, err)
	}
	return nil
}

func GetVRFDevice(vrfName string) (*netlink.Vrf, error) {
	vrfLink, err := netlink.LinkByName(vrfName)
	if err != nil {
		return nil, err
	}
	vrf, ok := vrfLink.(*netlink.Vrf)
	if !ok {
		return nil, errors.New("node has another non vrf device with same name")
	}
	return vrf, nil
}

package vrfmanager

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type vrf struct {
	name       string
	table      uint32
	interfaces sets.Set[string]
	delete     bool
}

type Controller struct {
	mu       *sync.Mutex
	vrfs     map[string]*vrf
	addVrfCh chan vrf
	delVrfCh chan vrf
}

func NewController() *Controller {
	return &Controller{
		mu:       &sync.Mutex{},
		vrfs:     make(map[string]*vrf),
		addVrfCh: make(chan vrf, 5),
		delVrfCh: make(chan vrf, 5),
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

func (vm *Controller) runInternal(stopChan <-chan struct{}, doneWg *sync.WaitGroup, subscribe subscribeFn) {
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
				if err = vm.syncLink(a.Link); err != nil {
					klog.Errorf("Vrf manager: Error while syncing link %q: %v", a.Link.Attrs().Name, err)
				}

			case <-linkSyncTimer.C:
				if subscribed {
					klog.V(5).Info("Vrf manager calling sync() explicitly")
					if err = vm.reconcile(); err != nil {
						klog.Errorf("Vrf manager: Error while reconciling vrfs: %v", err)
					}
				} else {
					if subscribed, addrChan, err = subscribe(); err != nil {
						klog.Errorf("Vrf manager: Error during netlink re-subscribe for Link Manager: %v", err)
					}
				}
			case newVrf := <-vm.addVrfCh:
				if err = vm.addVrf(&newVrf); err != nil {
					klog.Errorf("Vrf manager: failed to add vrf (%v): %v", newVrf, err)
				}
			case delVrf := <-vm.delVrfCh:
				if err = vm.delVrf(&delVrf); err != nil {
					klog.Errorf("Vrf Manager: failed to delete vrf (%v): %v", delVrf, err)
				}
			case <-stopChan:
				return
			}
		}
	}()

	klog.Info("Vrf manager is running")
}

// Add submits a request to add a Vrf
func (vm *Controller) Add(name string, table uint32, interfaces sets.Set[string]) {
	vm.addVrfCh <- vrf{name, table, interfaces, false}
}

// Del submits a request to del a Vrf
func (vm *Controller) Del(name string, table uint32, interfaces sets.Set[string]) {
	vm.delVrfCh <- vrf{name, table, interfaces, true}
}

func (vm *Controller) addVrf(vrf *vrf) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.sync(vrf)
}

func (vm *Controller) delVrf(vrf *vrf) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.sync(vrf)
}

func (vm *Controller) reconcile() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	start := time.Now()
	defer func() {
		klog.V(5).Infof("Vrf Manager: reconciling VRFs took %v", time.Since(start))
	}()

	vrfsToKeep := make(map[string]*vrf)
	for _, vrf := range vm.vrfs {
		if vrf.delete {
			err := vm.deleteVRF(vrf)
			if err != nil {
				return err
			}
			continue
		}
		err := vm.sync(vrf)
		if err != nil {
			return err
		}
		vrfsToKeep[vrf.name] = vrf
	}
	vm.vrfs = vrfsToKeep
	return nil
}

func (vm *Controller) syncLink(link netlink.Link) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vrf, ok := vm.vrfs[link.Attrs().Name]
	if !ok {
		return nil
	}
	return vm.sync(vrf)
}

func (vm *Controller) sync(vrf *vrf) error {
	if vrf == nil {
		return nil
	}
	vrfLink, err := netlink.LinkByName(vrf.name)
	if err == nil {
		vrfDev, ok := vrfLink.(*netlink.Vrf)
		if !ok {
			return errors.New("node has another non vrf device with same name")
		}
		if vrfDev.Table != vrf.table {
			return errors.New("found conflict with existing vrf device table id")
		}
	}
	// Create Vrf device if it doesn't exist.
	if util.GetNetLinkOps().IsLinkNotFoundError(err) {
		vrfLink = &netlink.Vrf{
			LinkAttrs: netlink.LinkAttrs{Name: vrf.name},
			Table:     vrf.table,
		}
		if err = netlink.LinkAdd(vrfLink); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}

	if vrfLink.Attrs().OperState != netlink.OperUp {
		if err = netlink.LinkSetUp(vrfLink); err != nil {
			return err
		}
	}

	// Ensure enslave interface(s) are synced up with the cache.
	var existingEnslaves sets.Set[string]
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	for _, link := range links {
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
				return fmt.Errorf("failed to enslave inteface %s into vrf %s", iface, vrf.name)
			}
		}
	}
	for _, iface := range existingEnslaves.UnsortedList() {
		if !vrf.interfaces.Has(iface) {
			err = removeInterfaceFromVRF(vrf.name, iface)
			if err != nil {
				return fmt.Errorf("failed to remove inteface %s from vrf %s", iface, vrf.name)
			}
		}
	}
	return nil
}

func (vm *Controller) deleteVRF(vrf *vrf) error {
	if vrf == nil {
		return nil
	}
	link, err := netlink.LinkByName(vrf.name)
	if err != nil && util.GetNetLinkOps().IsLinkNotFoundError(err) {
		return nil
	} else if err != nil {
		return err
	}
	return netlink.LinkDel(link)
}

func enslaveInterfaceToVRF(vrfName, ifName string) error {
	iface, err := netlink.LinkByName(ifName)
	if err != nil {
		return err
	}
	vrfLink, err := netlink.LinkByName(vrfName)
	if err != nil {
		return err
	}
	err = netlink.LinkSetMaster(iface, vrfLink)
	if err != nil {
		return fmt.Errorf("failed to enslave interface %s to VRF %s: %v", ifName, vrfName, err)
	}
	return nil
}

func removeInterfaceFromVRF(vrfName, ifName string) error {
	iface, err := netlink.LinkByName(ifName)
	if err != nil {
		return err
	}
	err = netlink.LinkSetMaster(iface, nil)
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

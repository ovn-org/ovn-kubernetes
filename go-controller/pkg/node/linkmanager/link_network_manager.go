package linkmanager

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/mdlayher/arp"
	"github.com/vishvananda/netlink"
)

// Gather all suitable interface address + network mask and offer this as a service.
// Also offer address assignment to interfaces and ensure the state we want is maintained through a sync func

type LinkAddress struct {
	Link      netlink.Link
	Addresses []netlink.Addr
}

type Controller struct {
	mu              *sync.Mutex
	name            string
	ipv4Enabled     bool
	ipv6Enabled     bool
	store           map[string][]netlink.Addr
	linkHandlerFunc func(link netlink.Link) error
}

// NewController creates a controller to manage linux network interfaces
func NewController(name string, v4, v6 bool, linkHandlerFunc func(link netlink.Link) error) *Controller {
	return &Controller{
		mu:              &sync.Mutex{},
		name:            name,
		ipv4Enabled:     v4,
		ipv6Enabled:     v6,
		store:           make(map[string][]netlink.Addr),
		linkHandlerFunc: linkHandlerFunc,
	}
}

// Run starts the controller and syncs at least every syncPeriod
// linkHandlerFunc fires as an additional handler when sync runs
func (c *Controller) Run(stopCh <-chan struct{}, doneWg *sync.WaitGroup) {
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
		// sync the manager with current addresses on the node
		c.sync()
		return true, linkChan, nil
	}

	c.runInternal(stopCh, doneWg, subscribe)
}

type subscribeFn func() (bool, chan netlink.LinkUpdate, error)

// runInternal can be used by testcases to provide a fake subscription function
// rather than using netlink
func (c *Controller) runInternal(stopChan <-chan struct{}, doneWg *sync.WaitGroup, subscribe subscribeFn) {

	doneWg.Add(1)
	go func() {
		defer doneWg.Done()

		linkSyncTimer := time.NewTicker(2 * time.Minute)
		defer linkSyncTimer.Stop()

		subscribed, addrChan, err := subscribe()
		if err != nil {
			klog.Errorf("Error during netlink subscribe for Link Manager: %v", err)
		}

		for {
			select {
			case a, ok := <-addrChan:
				linkSyncTimer.Reset(2 * time.Minute)
				if !ok {
					if subscribed, addrChan, err = subscribe(); err != nil {
						klog.Errorf("Error during netlink re-subscribe due to channel closing for Link Manager: %v", err)
					}
					continue
				}
				if err := c.syncLinkLocked(a.Link); err != nil {
					klog.Errorf("Error while syncing link %q: %v", a.Link.Attrs().Name, err)
				}

			case <-linkSyncTimer.C:
				if subscribed {
					klog.V(5).Info("Link manager calling sync() explicitly")
					c.sync()
				} else {
					if subscribed, addrChan, err = subscribe(); err != nil {
						klog.Errorf("Error during netlink re-subscribe for Link Manager: %v", err)
					}
				}
			case <-stopChan:
				return
			}
		}
	}()

	klog.Info("Link manager is running")
}

// AddAddress stores the address in a store and ensures its applied
func (c *Controller) AddAddress(address netlink.Addr) error {
	if !c.isAddressValid(address) {
		return fmt.Errorf("address (%s) is not valid", address.String())
	}
	link, err := util.GetNetLinkOps().LinkByIndex(address.LinkIndex)
	if err != nil {
		return fmt.Errorf("no valid link associated with addresses %s: %v", address.String(), err)
	}
	klog.Infof("Link manager: adding address %s to link %s", address.String(), link.Attrs().Name)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addAddressToStore(link.Attrs().Name, address)
	return c.syncLink(link)
}

// DelAddress removes the address from the store and ensure its removed from a link
func (c *Controller) DelAddress(address netlink.Addr) error {
	if !c.isAddressValid(address) {
		return fmt.Errorf("address (%s) is not valid", address.String())
	}
	link, err := util.GetNetLinkOps().LinkByIndex(address.LinkIndex)
	if err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			c.mu.Lock()
			c.delLinkFromStoreByIndex(address.LinkIndex)
			c.mu.Unlock()
			return nil
		}
		return fmt.Errorf("no valid link associated with addresses %s: %v", address.String(), err)
	}
	klog.Infof("Link manager: deleting address %s from link %s", address.String(), link.Attrs().Name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := util.GetNetLinkOps().AddrDel(link, &address); err != nil {
		if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return fmt.Errorf("failed to delete address %s: %v", address.String(), err)
		}
	}
	c.delAddressFromStore(link.Attrs().Name, address)
	return nil
}

// syncLinkLocked is just a wrapper around syncLink that ensures the mutex is locked in advance
func (c *Controller) syncLinkLocked(link netlink.Link) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.syncLink(link)
}

// syncLink handles link updates
// It MUST be called with the controller locked
func (c *Controller) syncLink(link netlink.Link) error {
	if c.linkHandlerFunc != nil {
		if err := c.linkHandlerFunc(link); err != nil {
			klog.Errorf("Failed to execute link handler function on link: %s, error: %v", link.Attrs().Name, err)
		}
	}
	linkName := link.Attrs().Name
	// get all addresses associated with the link depending on which IP families we support
	foundAddresses, err := util.GetFilteredInterfaceAddrs(link, c.ipv4Enabled, c.ipv6Enabled)
	if err != nil {
		return fmt.Errorf("failed to get address from link %q: %w", linkName, err)
	}
	wantedAddresses, found := c.store[linkName]
	// we don't manage this link therefore we don't need to add any addresses
	if !found {
		return nil
	}
	// add addresses we want that are not found on the link
	for _, addressWanted := range wantedAddresses {
		if containsAddress(foundAddresses, addressWanted) {
			continue
		}
		if err = util.GetNetLinkOps().AddrAdd(link, &addressWanted); err != nil {
			klog.Errorf("Link manager: failed to add address %q to link %q: %v", addressWanted.String(), linkName, err)
		}
		// For IPv4, use arping to try to update other hosts ARP caches, in case this IP was
		// previously active on another node
		if addressWanted.IP.To4() != nil {
			if err = gratuitousArpOverIfaceByName(addressWanted.IP, linkName); err != nil {
				klog.Errorf("Failed to send a GARP for IP %s over interface %s: %v", addressWanted.IP.String(),
					linkName, err)
			}
		}
		klog.Infof("Link manager: completed adding address %s to link %s", addressWanted, linkName)
	}
	return nil
}

func (c *Controller) sync() {
	// 1. get all the links on the node
	// 2. iterate over the links and get the addresses associated with it
	// 3. add addresses that are missing from a link that we manage
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		klog.Errorf("Link manager: failed to list links: %v", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, link := range links {
		if err := c.syncLink(link); err != nil {
			klog.Errorf("Sync Link failed for link %q: %v", link.Attrs().Name, err)
		}
	}
}

func (c *Controller) addAddressToStore(linkName string, newAddress netlink.Addr) {
	addressesSaved, found := c.store[linkName]
	if !found {
		c.store[linkName] = []netlink.Addr{newAddress}
		return
	}
	// check if the address already exists
	for _, addressSaved := range addressesSaved {
		if addressSaved.Equal(newAddress) {
			return
		}
	}
	// add it to store if not found
	c.store[linkName] = append(addressesSaved, newAddress)
}

func (c *Controller) delAddressFromStore(linkName string, address netlink.Addr) {
	addressesSaved, found := c.store[linkName]
	if !found {
		return
	}
	temp := addressesSaved[:0]
	for _, addressSaved := range addressesSaved {
		if !addressSaved.Equal(address) {
			temp = append(temp, addressSaved)
		}
	}
	if len(temp) == 0 {
		delete(c.store, linkName)
	} else {
		c.store[linkName] = temp
	}
}

func (c *Controller) delLinkFromStoreByIndex(linkToDelIndex int) {
	var linkNameToDelete string
	for linkName, addresses := range c.store {
		if len(addresses) > 0 {
			if linkToDelIndex == addresses[0].LinkIndex {
				linkNameToDelete = linkName
				break
			}
		}
	}
	if linkNameToDelete != "" {
		delete(c.store, linkNameToDelete)
	}
}

func (c *Controller) isAddressValid(address netlink.Addr) bool {
	if address.LinkIndex == 0 {
		return false
	}
	if address.IPNet == nil {
		return false
	}
	if address.IPNet.IP.IsUnspecified() {
		return false
	}
	if utilnet.IsIPv4(address.IP) && !c.ipv4Enabled {
		return false
	}
	if utilnet.IsIPv6(address.IP) && !c.ipv6Enabled {
		return false
	}
	return true
}

// DeprecatedGetAssignedAddressLabel returns the label that must be assigned to each egress IP address bound to an interface
func DeprecatedGetAssignedAddressLabel(linkName string) string {
	return fmt.Sprintf("%sovn", linkName)
}

func containsAddress(addresses []netlink.Addr, candidate netlink.Addr) bool {
	for _, address := range addresses {
		if address.Equal(candidate) {
			return true
		}
	}
	return false
}

func gratuitousArpOverIfaceByName(srcIP net.IP, ifaceName string) error {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("failed finding interface '%s': %w", ifaceName, err)
	}

	c, err := arp.Dial(iface)
	if err != nil {
		return fmt.Errorf("failed dialing logical switch interface '%s': %w", ifaceName, err)
	}
	defer c.Close()
	addr, err := netip.ParseAddr(srcIP.String())
	if err != nil {
		return fmt.Errorf("failed converting net.IP to netip.Addr: %w", err)
	}
	p, err := arp.NewPacket(arp.OperationRequest, iface.HardwareAddr, addr, net.HardwareAddr{0, 0, 0, 0, 0, 0}, addr)
	if err != nil {
		return fmt.Errorf("failed create GARP: %w", err)
	}
	err = c.WriteTo(p, net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	if err != nil {
		return fmt.Errorf("failed sending GARP: %w", err)
	}
	return nil
}

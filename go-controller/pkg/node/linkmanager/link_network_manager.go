package linkmanager

import (
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"

	"github.com/j-keck/arping"
	"github.com/vishvananda/netlink"
)

// Gather all suitable interface address + network mask and offer this as a service.
// Also offer address assignment to interfaces and ensure the state we want is maintained through a sync func

type LinkAddress struct {
	Link      netlink.Link
	Addresses []netlink.Addr
}

type Controller struct {
	mu          *sync.Mutex
	name        string
	ipv4Enabled bool
	ipv6Enabled bool
	store       map[string][]netlink.Addr
}

// NewController creates a controller to manage linux network interfaces
func NewController(name string, v4, v6 bool) *Controller {
	return &Controller{
		mu:          &sync.Mutex{},
		name:        name,
		ipv4Enabled: v4,
		ipv6Enabled: v6,
		store:       make(map[string][]netlink.Addr, 0),
	}
}

// Run starts the controller and syncs at least every syncPeriod
func (c *Controller) Run(stopCh <-chan struct{}, syncPeriod time.Duration) {
	ticker := time.NewTicker(syncPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			c.mu.Lock()
			c.reconcile()
			c.mu.Unlock()
		}
	}

}

// AddAddress stores the address in a store and ensures its applied
func (c *Controller) AddAddress(address netlink.Addr) error {
	if address.LinkIndex == 0 {
		return fmt.Errorf("link index must be non-zero")
	}
	if address.IPNet == nil {
		return fmt.Errorf("IP must be non-nil")
	}
	if address.IPNet.IP.IsUnspecified() {
		return fmt.Errorf("IP must be specified")
	}
	link, err := util.GetNetLinkOps().LinkByIndex(address.LinkIndex)
	if err != nil {
		return fmt.Errorf("no valid link associated with addresses %s: %v", address.String(), err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// overwrite label to the name of this component in-order to aid address ownership. Label must start with link name.
	address.Label = GetAssignedAddressLabel(link.Attrs().Name)
	c.addAddressToStore(link.Attrs().Name, address)
	c.reconcile()
	return nil
}

// DelAddress removes the address from the store and ensure its removed from a link
func (c *Controller) DelAddress(address netlink.Addr) error {
	if address.LinkIndex == 0 {
		return fmt.Errorf("link index must be non-zero")
	}
	if address.IPNet == nil {
		return fmt.Errorf("IP must be non-nil")
	}
	if address.IPNet.IP.IsUnspecified() {

		return fmt.Errorf("IP must be specified")
	}
	link, err := util.GetNetLinkOps().LinkByIndex(address.LinkIndex)
	if err != nil && !util.GetNetLinkOps().IsLinkNotFoundError(err) {
		return fmt.Errorf("no valid link associated with addresses %s: %v", address.String(), err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delAddressFromStore(link.Attrs().Name, address)
	c.reconcile()
	return nil
}

func (c *Controller) reconcile() {
	// 1. get all the links on the node
	// 2. iterate over the links and get the addresses associated with it
	// 3. cleanup any stale addresses from link that we no longer managed
	// 4. remove any stale addresses from links that we do manage
	// 5. add addresses that are missing from a link that we managed
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		klog.Errorf("Link Network Manager: failed to list links: %v", err)
		return
	}
	for _, link := range links {
		linkName := link.Attrs().Name
		// get all addresses associated with the link depending on which IP families we support
		foundAddresses, err := getAllLinkAddressesByIPFamily(link, c.ipv4Enabled, c.ipv6Enabled)
		if err != nil {
			klog.Errorf("Link Network Manager: failed to get address from link %q", linkName)
			continue
		}
		wantedAddresses, found := c.store[linkName]
		// cleanup any stale addresses on the link
		for _, foundAddress := range foundAddresses {
			// we label any address we create, so if we aren't managing a link, we must remove any stale addresses
			if foundAddress.Label == GetAssignedAddressLabel(linkName) && !containsAddress(wantedAddresses, foundAddress) {
				if err := util.GetNetLinkOps().AddrDel(link, &foundAddress); err != nil && !util.GetNetLinkOps().IsLinkNotFoundError(err) {
					klog.Errorf("Link Network Manager: failed to delete address %q from link %q",
						foundAddress.String(), linkName)
				} else {
					klog.Infof("Link Network Manager: successfully removed stale address %q from link %q",
						foundAddress.String(), linkName)
				}
			}
		}
		// we don't manage this link therefore we don't need to add any addresses
		if !found {
			continue
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
				if err = arping.GratuitousArpOverIfaceByName(addressWanted.IP, linkName); err != nil {
					klog.Errorf("Failed to send a GARP for IP %s over interface %s: %v", addressWanted.IP.String(),
						linkName, err)
				}
			}
			klog.Infof("Link manager completed adding address %s to link %s", addressWanted, linkName)
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
			temp = append(temp, address)
		}
	}
	c.store[linkName] = temp
}

// GetAssignedAddressLabel returns the label that must be assigned to each egress IP address bound to an interface
func GetAssignedAddressLabel(linkName string) string {
	return fmt.Sprintf("%sovn", linkName)
}

// GetExternallyAvailableAddressesExcludeAssigned gets all addresses assigned on an interface with the following characteristics:
// Must be up
// Address must have scope universe
// Assigned addresses are excluded
func GetExternallyAvailableAddressesExcludeAssigned(link netlink.Link, v4, v6 bool) ([]netlink.Addr, error) {
	addresses, err := GetExternallyAvailableAddresses(link, v4, v6)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return addresses, nil
	}
	tmp := addresses[:0]
	for _, address := range addresses {
		if address.Label == GetAssignedAddressLabel(link.Attrs().Name) {
			continue
		}
		tmp = append(tmp, address)
	}
	return tmp, nil
}

// GetExternallyAvailableAddresses gets all addresses assigned on an interface with the following characteristics:
// Must be up
// Address must have scope universe
// Assigned addresses are included
func GetExternallyAvailableAddresses(link netlink.Link, v4, v6 bool) ([]netlink.Addr, error) {
	validAddresses := make([]netlink.Addr, 0)
	flags := link.Attrs().Flags.String()
	if !isValidLinkFlags(flags) {
		return validAddresses, nil
	}
	linkAddresses, err := getAllLinkAddressesByIPFamily(link, v4, v6)
	if err != nil {
		return validAddresses, fmt.Errorf("failed to get all valid link addresses: %v", err)
	}
	for _, address := range linkAddresses {
		// consider only GLOBAL scope addresses
		if address.Scope != int(netlink.SCOPE_UNIVERSE) {
			continue
		}
		validAddresses = append(validAddresses, address)
	}
	return validAddresses, nil
}

// GetExternallyAvailablePrefixesExcludeAssigned returns address Prefixes from interfaces with the following characteristics:
// Must be up
// Must not be loopback
// Must not have a parent
// Address must have scope universe
// Address must not be an address assigned to a link by link manager
func GetExternallyAvailablePrefixesExcludeAssigned(link netlink.Link, v4, v6 bool) ([]netip.Prefix, error) {
	validAddresses := make([]netip.Prefix, 0)
	flags := link.Attrs().Flags.String()
	if !isValidLinkFlags(flags) {
		return validAddresses, nil
	}
	linkAddresses, err := getAllLinkAddressesByIPFamily(link, v4, v6)
	if err != nil {
		return validAddresses, fmt.Errorf("failed to get all valid link addresses: %v", err)
	}
	// netip pkg does not expose addresses labels. We must get addresses with netlink lib first to check if addr
	// is managed or not
	for _, address := range linkAddresses {
		// check for assigned address - assigned addresses contain a well-known label
		if address.Label != "" {
			link, err := netlink.LinkByIndex(address.LinkIndex)
			if err != nil {
				return nil, fmt.Errorf("failed to understand if address (%s) is managed or not by link with index %d: %v",
					address.String(), address.LinkIndex, err)
			}
			if address.Label == GetAssignedAddressLabel(link.Attrs().Name) {
				continue
			}
		}
		addr, err := netip.ParsePrefix(address.IPNet.String())
		if err != nil {
			return nil, fmt.Errorf("unable to parse address %s on link %s: %v", address.String(), link.Attrs().Name, err)
		}
		if !addr.Addr().IsGlobalUnicast() {
			continue
		}
		validAddresses = append(validAddresses, addr)
	}
	return validAddresses, nil
}

func isValidLinkFlags(flags string) bool {
	// exclude interfaces that aren't up
	return strings.Contains(flags, "up")
}

func getAllLinkAddressesByIPFamily(link netlink.Link, v4, v6 bool) ([]netlink.Addr, error) {
	links := make([]netlink.Addr, 0)
	if v4 {
		linksFound, err := util.GetNetLinkOps().AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return links, fmt.Errorf("failed to list link addresses: %v", err)
		}
		links = linksFound
	}
	if v6 {
		linksFound, err := util.GetNetLinkOps().AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			return links, fmt.Errorf("failed to list link addresses: %v", err)
		}
		links = append(links, linksFound...)
	}
	return links, nil
}

func containsAddress(addresses []netlink.Addr, candidate netlink.Addr) bool {
	for _, address := range addresses {
		if address.Equal(candidate) {
			return true
		}
	}
	return false
}

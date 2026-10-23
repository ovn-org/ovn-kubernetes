// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package util

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mdlayher/arp"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
)

func init() {
	// Disable VF collection for package-level link operations; it is costly on PFs
	// with many VFs and unnecessary.
	if err := netlink.ConfigureHandle(netlink.HandleOptions{DisableVFInfoCollection: true}); err != nil {
		panic(fmt.Sprintf("failed to configure netlink package handle: %v", err))
	}
}

type NetLinkOps interface {
	LinkList() ([]netlink.Link, error)
	LinkByName(ifaceName string) (netlink.Link, error)
	LinkByIndex(index int) (netlink.Link, error)
	LinkSetDown(link netlink.Link) error
	LinkAdd(link netlink.Link) error
	LinkModify(link netlink.Link) error
	LinkDelete(link netlink.Link) error
	LinkSetAlias(link netlink.Link, name string) error
	LinkSetName(link netlink.Link, newName string) error
	LinkSetUp(link netlink.Link) error
	LinkSetNsFd(link netlink.Link, fd int) error
	LinkSetHardwareAddr(link netlink.Link, hwaddr net.HardwareAddr) error
	LinkSetMaster(link netlink.Link, master netlink.Link) error
	LinkSetNoMaster(link netlink.Link) error
	LinkSetMTU(link netlink.Link, mtu int) error
	LinkSetTxQLen(link netlink.Link, qlen int) error
	IsLinkNotFoundError(err error) bool
	AddrList(link netlink.Link, family int) ([]netlink.Addr, error)
	AddrDel(link netlink.Link, addr *netlink.Addr) error
	AddrAdd(link netlink.Link, addr *netlink.Addr) error
	RouteList(link netlink.Link, family int) ([]netlink.Route, error)
	RouteDel(route *netlink.Route) error
	RouteAdd(route *netlink.Route) error
	RouteReplace(route *netlink.Route) error
	RouteListFiltered(family int, filter *netlink.Route, filterMask uint64) ([]netlink.Route, error)
	RouteListFilteredIter(family int, filter *netlink.Route, filterMask uint64, f func(netlink.Route) bool) error
	RuleListFiltered(family int, filter *netlink.Rule, filterMask uint64) ([]netlink.Rule, error)
	RuleAdd(rule *netlink.Rule) error
	RuleDel(rule *netlink.Rule) error
	NeighAdd(neigh *netlink.Neigh) error
	NeighSet(neigh *netlink.Neigh) error
	NeighDel(neigh *netlink.Neigh) error
	NeighList(linkIndex, family int) ([]netlink.Neigh, error)
	ConntrackDeleteFilters(table netlink.ConntrackTableType, family netlink.InetFamily, filters ...netlink.CustomConntrackFilter) (uint, error)
	LinkSetVfHardwareAddr(pfLink netlink.Link, vfIndex int, hwaddr net.HardwareAddr) error
	RouteSubscribeWithOptions(ch chan<- netlink.RouteUpdate, done <-chan struct{}, options netlink.RouteSubscribeOptions) error
	LinkSubscribeWithOptions(ch chan<- netlink.LinkUpdate, done <-chan struct{}, options netlink.LinkSubscribeOptions) error
	// Error checking helpers
	IsEntryNotFoundError(err error) bool
	IsAlreadyExistsError(err error) bool
	// Bridge VLAN operations
	BridgeVlanAdd(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error
	BridgeVlanDel(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error
	BridgeVniAdd(link netlink.Link, vni uint32) error
	BridgeVniDel(link netlink.Link, vni uint32) error
	BridgeVlanAddTunnelInfo(link netlink.Link, vid uint16, vni uint32, self, master bool) error
	BridgeVlanDelTunnelInfo(link netlink.Link, vid uint16, vni uint32, self, master bool) error
	BridgeVlanTunnelShowDev(link netlink.Link) ([]nl.TunnelInfo, error)
	BridgeVlanList() (map[int32][]*nl.BridgeVlanInfo, error)
	BridgeVniList() (map[int32][]*nl.BridgeVniInfo, error)
	// Bridge port settings
	LinkSetVlanTunnel(link netlink.Link, mode bool) error
	LinkSetBrNeighSuppress(link netlink.Link, mode bool) error
	LinkSetLearning(link netlink.Link, mode bool) error
	LinkSetIsolated(link netlink.Link, mode bool) error
	LinkGetProtinfo(link netlink.Link) (netlink.Protinfo, error)
}

type defaultNetLinkOps struct {
}

var netLinkOps NetLinkOps = &defaultNetLinkOps{}

// SetNetLinkOpMockInst method would be used by unit tests in other packages
// LinkByNameOrAlias returns the link named name, falling back to the link
// whose alias matches name. Callers that rename a link record its original
// name as the alias so it can be found and renamed back later; name-based
// resolution (e.g. simulated DPU netdevices, which have no PCI identity)
// needs the fallback to survive such renames.
func LinkByNameOrAlias(name string) (netlink.Link, error) {
	link, err := GetNetLinkOps().LinkByName(name)
	if err != nil && GetNetLinkOps().IsLinkNotFoundError(err) {
		links, listErr := GetNetLinkOps().LinkList()
		if listErr != nil {
			return nil, fmt.Errorf("link %s not found and listing links failed: %v", name, listErr)
		}
		for _, l := range links {
			if attrs := l.Attrs(); attrs != nil && attrs.Alias == name {
				klog.Infof("Resolved netdevice %s by alias on link %s", name, attrs.Name)
				return l, nil
			}
		}
	}
	return link, err
}

func SetNetLinkOpMockInst(mockInst NetLinkOps) {
	netLinkOps = mockInst
}

// ResetNetLinkOpMockInst resets the mock instance for netlink to the defaultNetLinkOps
func ResetNetLinkOpMockInst() {
	netLinkOps = &defaultNetLinkOps{}
}

// GetNetLinkOps will be invoked by functions in other packages that would need access to the netlink library methods.
func GetNetLinkOps() NetLinkOps {
	return netLinkOps
}

func (defaultNetLinkOps) LinkList() ([]netlink.Link, error) {
	return netlink.LinkList()
}

func (defaultNetLinkOps) LinkByName(ifaceName string) (netlink.Link, error) {
	return netlink.LinkByName(ifaceName)
}

func (defaultNetLinkOps) LinkByIndex(index int) (netlink.Link, error) {
	return netlink.LinkByIndex(index)
}

func (defaultNetLinkOps) LinkSetDown(link netlink.Link) error {
	return netlink.LinkSetDown(link)
}

func (defaultNetLinkOps) LinkAdd(link netlink.Link) error {
	return netlink.LinkAdd(link)
}

func (defaultNetLinkOps) LinkModify(link netlink.Link) error {
	return netlink.LinkModify(link)
}

func (defaultNetLinkOps) LinkSetAlias(link netlink.Link, name string) error {
	return netlink.LinkSetAlias(link, name)
}

func (defaultNetLinkOps) LinkDelete(link netlink.Link) error {
	return netlink.LinkDel(link)
}

func (defaultNetLinkOps) LinkSetUp(link netlink.Link) error {
	return netlink.LinkSetUp(link)
}

func (defaultNetLinkOps) LinkSetName(link netlink.Link, newName string) error {
	return netlink.LinkSetName(link, newName)
}

func (defaultNetLinkOps) LinkSetNsFd(link netlink.Link, fd int) error {
	return netlink.LinkSetNsFd(link, fd)
}

func (defaultNetLinkOps) LinkSetHardwareAddr(link netlink.Link, hwaddr net.HardwareAddr) error {
	return netlink.LinkSetHardwareAddr(link, hwaddr)
}

func (defaultNetLinkOps) LinkSetMaster(link netlink.Link, master netlink.Link) error {
	return netlink.LinkSetMaster(link, master)
}

func (defaultNetLinkOps) LinkSetNoMaster(link netlink.Link) error {
	return netlink.LinkSetNoMaster(link)
}

func (defaultNetLinkOps) LinkSetMTU(link netlink.Link, mtu int) error {
	return netlink.LinkSetMTU(link, mtu)
}

func (defaultNetLinkOps) LinkSetTxQLen(link netlink.Link, qlen int) error {
	return netlink.LinkSetTxQLen(link, qlen)
}

func (defaultNetLinkOps) IsLinkNotFoundError(err error) bool {
	return reflect.TypeOf(err) == reflect.TypeOf(netlink.LinkNotFoundError{})
}

// IsEntryNotFoundError checks if an error indicates an entry was not found.
// This covers ENOENT (entry not found), ENODEV (device not found, which can
// occur if the device is deleted after we obtained the link handle), and
// EADDRNOTAVAIL (address not found, returned by RTM_DELADDR for non-existent addresses).
func (defaultNetLinkOps) IsEntryNotFoundError(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.EADDRNOTAVAIL)
}

// IsAlreadyExistsError checks if an error indicates an entry already exists.
// Used for idempotent add operations where EEXIST is expected and acceptable.
func (defaultNetLinkOps) IsAlreadyExistsError(err error) bool {
	return errors.Is(err, syscall.EEXIST)
}

// AddrList uses a per-call handle with NETLINK_GET_STRICT_CHK enabled to allow
// the kernel to filter addresses by ifindex server-side instead of dumping all.
func (defaultNetLinkOps) AddrList(link netlink.Link, family int) ([]netlink.Addr, error) {
	h, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("failed to create netlink handle: %v", err)
	}
	defer h.Close()
	if err := h.SetStrictCheck(true); err != nil {
		klog.V(5).Infof("Failed to enable strict check on netlink handle, falling back to unfiltered dump: %v", err)
		return netlink.AddrList(link, family)
	}
	return h.AddrList(link, family)
}

func (defaultNetLinkOps) AddrDel(link netlink.Link, addr *netlink.Addr) error {
	return netlink.AddrDel(link, addr)
}

func (defaultNetLinkOps) AddrAdd(link netlink.Link, addr *netlink.Addr) error {
	return netlink.AddrAdd(link, addr)
}

func (defaultNetLinkOps) RouteList(link netlink.Link, family int) ([]netlink.Route, error) {
	return netlink.RouteList(link, family)
}

func (defaultNetLinkOps) RouteDel(route *netlink.Route) error {
	return netlink.RouteDel(route)
}

func (defaultNetLinkOps) RouteAdd(route *netlink.Route) error {
	return netlink.RouteAdd(route)
}

func (defaultNetLinkOps) RouteReplace(route *netlink.Route) error {
	return netlink.RouteReplace(route)
}

func (defaultNetLinkOps) RouteListFiltered(family int, filter *netlink.Route, filterMask uint64) ([]netlink.Route, error) {
	return netlink.RouteListFiltered(family, filter, filterMask)
}

func (defaultNetLinkOps) RouteListFilteredIter(family int, filter *netlink.Route, filterMask uint64, f func(netlink.Route) bool) error {
	return netlink.RouteListFilteredIter(family, filter, filterMask, f)
}

func (defaultNetLinkOps) RuleListFiltered(family int, filter *netlink.Rule, filterMask uint64) ([]netlink.Rule, error) {
	return netlink.RuleListFiltered(family, filter, filterMask)
}

func (defaultNetLinkOps) RuleAdd(rule *netlink.Rule) error {
	return netlink.RuleAdd(rule)
}

func (defaultNetLinkOps) RuleDel(rule *netlink.Rule) error {
	return netlink.RuleDel(rule)
}

func (defaultNetLinkOps) NeighAdd(neigh *netlink.Neigh) error {
	return netlink.NeighAdd(neigh)
}

func (defaultNetLinkOps) NeighSet(neigh *netlink.Neigh) error {
	return netlink.NeighSet(neigh)
}

func (defaultNetLinkOps) NeighDel(neigh *netlink.Neigh) error {
	return netlink.NeighDel(neigh)
}

func (defaultNetLinkOps) NeighList(linkIndex, family int) ([]netlink.Neigh, error) {
	return netlink.NeighList(linkIndex, family)
}

func (defaultNetLinkOps) ConntrackDeleteFilters(table netlink.ConntrackTableType, family netlink.InetFamily, filters ...netlink.CustomConntrackFilter) (uint, error) {
	return netlink.ConntrackDeleteFilters(table, family, filters...)
}

func (defaultNetLinkOps) RouteSubscribeWithOptions(ch chan<- netlink.RouteUpdate, done <-chan struct{}, options netlink.RouteSubscribeOptions) error {
	return netlink.RouteSubscribeWithOptions(ch, done, options)
}

func (defaultNetLinkOps) LinkSubscribeWithOptions(ch chan<- netlink.LinkUpdate, done <-chan struct{}, options netlink.LinkSubscribeOptions) error {
	return netlink.LinkSubscribeWithOptions(ch, done, options)
}

func (defaultNetLinkOps) BridgeVlanAdd(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error {
	return netlink.BridgeVlanAdd(link, vid, pvid, untagged, self, master)
}

func (defaultNetLinkOps) BridgeVlanDel(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error {
	return netlink.BridgeVlanDel(link, vid, pvid, untagged, self, master)
}

func (defaultNetLinkOps) BridgeVniAdd(link netlink.Link, vni uint32) error {
	return netlink.BridgeVniAdd(link, vni)
}

func (defaultNetLinkOps) BridgeVniDel(link netlink.Link, vni uint32) error {
	return netlink.BridgeVniDel(link, vni)
}

func (defaultNetLinkOps) BridgeVlanAddTunnelInfo(link netlink.Link, vid uint16, vni uint32, self, master bool) error {
	return netlink.BridgeVlanAddTunnelInfo(link, vid, vni, self, master)
}

func (defaultNetLinkOps) BridgeVlanDelTunnelInfo(link netlink.Link, vid uint16, vni uint32, self, master bool) error {
	return netlink.BridgeVlanDelTunnelInfo(link, vid, vni, self, master)
}

func (defaultNetLinkOps) BridgeVlanTunnelShowDev(link netlink.Link) ([]nl.TunnelInfo, error) {
	return netlink.BridgeVlanTunnelShowDev(link)
}

func (defaultNetLinkOps) BridgeVlanList() (map[int32][]*nl.BridgeVlanInfo, error) {
	return netlink.BridgeVlanList()
}

func (defaultNetLinkOps) BridgeVniList() (map[int32][]*nl.BridgeVniInfo, error) {
	return netlink.BridgeVniList()
}

func (defaultNetLinkOps) LinkSetVlanTunnel(link netlink.Link, mode bool) error {
	return netlink.LinkSetVlanTunnel(link, mode)
}

func (defaultNetLinkOps) LinkSetBrNeighSuppress(link netlink.Link, mode bool) error {
	return netlink.LinkSetBrNeighSuppress(link, mode)
}

func (defaultNetLinkOps) LinkSetLearning(link netlink.Link, mode bool) error {
	return netlink.LinkSetLearning(link, mode)
}

func (defaultNetLinkOps) LinkSetIsolated(link netlink.Link, mode bool) error {
	return netlink.LinkSetIsolated(link, mode)
}

func (defaultNetLinkOps) LinkGetProtinfo(link netlink.Link) (netlink.Protinfo, error) {
	return netlink.LinkGetProtinfo(link)
}

func getFamily(ip net.IP) int {
	if utilnet.IsIPv6(ip) {
		return netlink.FAMILY_V6
	} else {
		return netlink.FAMILY_V4
	}
}

// LinkByName returns the netlink device
func LinkByName(interfaceName string) (netlink.Link, error) {
	link, err := netLinkOps.LinkByName(interfaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup link %s: %w", interfaceName, err)
	}
	return link, nil
}

// LinkSetUp returns the netlink device with its state marked up
func LinkSetUp(interfaceName string) (netlink.Link, error) {
	link, err := netLinkOps.LinkByName(interfaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup link %s: %v", interfaceName, err)
	}
	err = netLinkOps.LinkSetUp(link)
	if err != nil {
		return nil, fmt.Errorf("failed to set the link %s up: %v", interfaceName, err)
	}
	return link, nil
}

// LinkDelete removes an interface
func LinkDelete(interfaceName string) error {
	link, err := netLinkOps.LinkByName(interfaceName)
	if err != nil {
		return fmt.Errorf("failed to lookup link %s: %v", interfaceName, err)
	}
	err = netLinkOps.LinkDelete(link)
	if err != nil {
		return fmt.Errorf("failed to remove link %s, error: %v", interfaceName, err)
	}
	return nil
}

// LinkAddrFlush flushes all the addresses on the given link, except IPv6 link-local addresses
func LinkAddrFlush(link netlink.Link) error {
	addrs, err := netLinkOps.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to list addresses for the link %s: %v", link.Attrs().Name, err)
	}
	for _, addr := range addrs {
		if utilnet.IsIPv6(addr.IP) && addr.IP.IsLinkLocalUnicast() {
			continue
		}
		err = netLinkOps.AddrDel(link, &addr)
		if err != nil {
			return fmt.Errorf("failed to delete address %s on link %s: %v",
				addr.IP.String(), link.Attrs().Name, err)
		}
	}
	return nil
}

// SyncAddresses ensures the link has the provided addresses only
// Ignores IPv6 LLA
// addresses should all be of the same family
func SyncAddresses(link netlink.Link, addresses []*net.IPNet) error {
	if len(addresses) == 0 {
		return nil
	}
	firstFamily := getFamily(addresses[0].IP)
	for _, addr := range addresses[1:] {
		if getFamily(addr.IP) != firstFamily {
			return fmt.Errorf("all addresses are not the same family: %#v", addresses)
		}
	}

	addrs, err := netLinkOps.AddrList(link, firstFamily)
	if err != nil {
		return fmt.Errorf("failed to list addresses for the link %s: %v",
			link.Attrs().Name, err)
	}

	// desired addresses - true if already exist
	matched := map[*net.IPNet]bool{}
	for _, desiredAddr := range addresses {
		matched[desiredAddr] = false
	}

	// cycle through found addresses
	for _, addr := range addrs {
		if utilnet.IsIPv6(addr.IP) && addr.IP.IsLinkLocalUnicast() {
			continue
		}

		exists := false
		for _, desiredAddr := range addresses {
			if addr.IPNet.String() == desiredAddr.String() {
				matched[desiredAddr] = true
				exists = true
				break
			}
		}

		// found address is not in desired list, remove it
		if !exists {
			if err := LinkAddrDel(link, addr.IPNet); err != nil {
				return err
			}
		}
	}

	// cycle through leftover addresses to add
	for addr, alreadyExists := range matched {
		if !alreadyExists {
			if err := LinkAddrAdd(link, addr, 0, 0, 0); err != nil {
				return err
			}
		}
	}

	return nil
}

// LinkAddrExist returns true if the given address is present on the link
func LinkAddrExist(link netlink.Link, address *net.IPNet) (bool, error) {
	addrs, err := netLinkOps.AddrList(link, getFamily(address.IP))
	if err != nil {
		return false, fmt.Errorf("failed to list addresses for the link %s: %v",
			link.Attrs().Name, err)
	}
	for _, addr := range addrs {
		if addr.IPNet.String() == address.String() {
			return true, nil
		}
	}
	return false, nil
}

// LinkAddrGetIPNet returns IPNet given the IP of an address present on given link
func LinkAddrGetIPNet(link netlink.Link, ip net.IP) (*net.IPNet, error) {
	addrs, err := netLinkOps.AddrList(link, getFamily(ip))
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses for the link %s: %v",
			link.Attrs().Name, err)
	}
	for _, addr := range addrs {
		if addr.IPNet.IP.Equal(ip) {
			return addr.IPNet, nil
		}
	}
	return nil, nil
}

// LinkAddrAdd adds a new address. If both preferredLifetime & validLifetime,
// are zero, then they are not applied, but if either parameters are not zero, both are applied.
func LinkAddrAdd(link netlink.Link, address *net.IPNet, flags, preferredLifetime, validLifetime int) error {
	err := netLinkOps.AddrAdd(link, &netlink.Addr{IPNet: address, Flags: flags, PreferedLft: preferredLifetime, ValidLft: validLifetime})
	if err != nil {
		return fmt.Errorf("failed to add address %s on link %s: %v", address.String(), link.Attrs().Name, err)
	}

	return nil
}

// LinkAddrDel removes an existing address from a link. Expects address is present otherwise, an error is returned.
func LinkAddrDel(link netlink.Link, address *net.IPNet) error {
	err := netLinkOps.AddrDel(link, &netlink.Addr{IPNet: address})
	if err != nil {
		return fmt.Errorf("failed to delete address %s on link %s: %v", address.String(), link.Attrs().Name, err)
	}
	return nil
}

// IsDeprecatedAddr returns true if the address is deprecated. An address is deprecated when preferred lifetime is zero.
func IsDeprecatedAddr(link netlink.Link, address *net.IPNet) (bool, error) {
	if link == nil {
		return false, fmt.Errorf("nil link is not allowed")
	}
	if address == nil {
		return false, fmt.Errorf("nil address is not allowed")
	}
	existingAddrs, err := netLinkOps.AddrList(link, getFamily(address.IP))
	if err != nil {
		return false, fmt.Errorf("failed to detect if address %s is deprecated because unable to list addresses on link %s: %v",
			address.IP.String(), link.Attrs().Name, err)
	}
	for _, existingAddr := range existingAddrs {
		if existingAddr.IPNet.String() == address.String() {
			// deprecated addresses have 0 preferred lifetime
			if existingAddr.PreferedLft == 0 {
				return true, nil
			}
			return false, nil
		}
	}
	return false, fmt.Errorf("failed to detect if address %s is deprecated because it doesn't exist", address.IP.String())
}

// LinkRoutesDel deletes all the routes for the given subnets via the link
// if subnets is empty, then all routes will be removed for a link
// if any item in subnets is nil the default route will be removed
func LinkRoutesDel(link netlink.Link, subnets []*net.IPNet) error {
	routes, err := netLinkOps.RouteList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to get all the routes for link %s: %v",
			link.Attrs().Name, err)
	}
	for _, route := range routes {
		if len(subnets) == 0 {
			err = netLinkOps.RouteDel(&route)
			if err != nil {
				return fmt.Errorf("failed to delete route '%s via %s' for link %s : %v",
					route.Dst.String(), route.Gw.String(), link.Attrs().Name, err)
			}
			continue
		}
		for _, subnet := range subnets {
			deleteRoute := false

			if subnet == nil {
				deleteRoute = IsNilOrAnyNetwork(route.Dst)
			} else if route.Dst != nil {
				deleteRoute = route.Dst.String() == subnet.String()
			}

			if deleteRoute {
				err = netLinkOps.RouteDel(&route)
				if err != nil {
					net := "default"
					if route.Dst != nil {
						net = route.Dst.String()
					}
					return fmt.Errorf("failed to delete route '%s via %s' for link %s : %v",
						net, route.Gw.String(), link.Attrs().Name, err)
				}
				break
			}
		}
	}
	return nil
}

// LinkRoutesAdd adds a new route for given subnets through the gwIPstr
func LinkRoutesAdd(link netlink.Link, gwIP net.IP, subnets []*net.IPNet, mtu int, src net.IP) error {
	for _, subnet := range subnets {
		route := &netlink.Route{
			Dst:       subnet,
			LinkIndex: link.Attrs().Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Gw:        gwIP,
		}
		if len(src) > 0 {
			route.Src = src
		}
		if mtu != 0 {
			route.MTU = mtu
		}
		err := netLinkOps.RouteAdd(route)
		if err != nil {
			return fmt.Errorf("failed to add route for subnet %s via gateway %s with mtu %d and src: %s: %v",
				subnet.String(), gwIP.String(), mtu, src, err)
		}
	}
	return nil
}

// IsNilOrAnyNetwork checks if the argument network is nil or an any network for ipv4 or ipv6.
func IsNilOrAnyNetwork(ipNet *net.IPNet) bool {
	if ipNet == nil {
		return true
	}

	return ipNet.IP.IsUnspecified()
}

// LinkRouteGetFilteredRoute gets a route for the given route filter.
// returns nil if route is not found
func LinkRouteGetFilteredRoute(routeFilter *netlink.Route, filterMask uint64) (*netlink.Route, error) {
	routes, err := netLinkOps.RouteListFiltered(getFamily(routeFilter.Dst.IP), routeFilter, filterMask)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to get routes for filter %v with mask %d: %v", *routeFilter, filterMask, err)
	}
	if len(routes) == 0 {
		return nil, nil
	}
	return &routes[0], nil
}

// LinkRouteGetByDstAndGw checks for existence of routes for the given subnet through gwIPStr
func LinkRouteGetByDstAndGw(link netlink.Link, gwIP net.IP, subnet *net.IPNet) (*netlink.Route, error) {
	route, err := LinkRouteGetFilteredRoute(filterRouteByDstAndGw(link, subnet, gwIP))
	return route, err
}

// LinkFDBSet adds or replaces a static FDB entry on the bridge that owns the given port.
// It uses NeighSet (NLM_F_CREATE|NLM_F_REPLACE) so it succeeds whether
// the entry is new or already exists.
func LinkFDBSet(port netlink.Link, mac net.HardwareAddr, vlan int) error {
	neigh := &netlink.Neigh{
		LinkIndex:    port.Attrs().Index,
		Family:       syscall.AF_BRIDGE,
		State:        netlink.NUD_NOARP,
		Flags:        netlink.NTF_MASTER,
		Vlan:         vlan,
		HardwareAddr: mac,
	}
	if err := netLinkOps.NeighSet(neigh); err != nil {
		return fmt.Errorf("failed to set FDB entry %s vlan %d on %s: %w", mac, vlan, port.Attrs().Name, err)
	}
	return nil
}

// LinkFDBDel deletes a static FDB entry from the bridge that owns the given port.
func LinkFDBDel(port netlink.Link, mac net.HardwareAddr, vlan int) error {
	neigh := &netlink.Neigh{
		LinkIndex:    port.Attrs().Index,
		Family:       syscall.AF_BRIDGE,
		Flags:        netlink.NTF_MASTER,
		Vlan:         vlan,
		HardwareAddr: mac,
	}
	if err := netLinkOps.NeighDel(neigh); err != nil {
		return fmt.Errorf("failed to delete FDB entry %s vlan %d from %s: %w", mac, vlan, port.Attrs().Name, err)
	}
	return nil
}

// LinkNeighDel deletes an ip binding for a given link
func LinkNeighDel(link netlink.Link, neighIP net.IP) error {
	neigh := &netlink.Neigh{
		LinkIndex: link.Attrs().Index,
		Family:    getFamily(neighIP),
		IP:        neighIP,
	}
	err := netLinkOps.NeighDel(neigh)
	if err != nil {
		return fmt.Errorf("failed to delete neighbour entry %+v: %w", neigh, err)
	}
	return nil
}

// LinkNeighSet adds or replaces MAC/IP bindings for the given link.
// It uses NeighSet (NLM_F_CREATE|NLM_F_REPLACE) so it succeeds whether
// the entry is new or already exists (e.g. zebra's extern_learn).
func LinkNeighSet(link netlink.Link, neighIP net.IP, neighMAC net.HardwareAddr) error {
	neigh := &netlink.Neigh{
		LinkIndex:    link.Attrs().Index,
		Family:       getFamily(neighIP),
		State:        netlink.NUD_PERMANENT,
		IP:           neighIP,
		HardwareAddr: neighMAC,
	}
	err := netLinkOps.NeighSet(neigh)
	if err != nil {
		return fmt.Errorf("failed to set neighbour entry %+v: %w", neigh, err)
	}
	return nil
}

func GetMACAddressFromARP(neighIP net.IP) (net.HardwareAddr, error) {
	selectedIface, err := findUsableInterfaceForNetwork(neighIP)
	if err != nil {
		return nil, err
	}
	cli, err := arp.Dial(selectedIface)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	if err := cli.SetDeadline(time.Now().Add(50 * time.Millisecond)); err != nil { // hard-coded for now
		return nil, err
	}
	neighAddr, err := netip.ParseAddr(neighIP.String())
	if err != nil {
		return nil, err
	}
	hwAddr, err := cli.Resolve(neighAddr)
	if err != nil {
		return nil, err
	}
	return hwAddr, nil
}

// LinkNeighExists checks to see if the given MAC/IP bindings exists
func LinkNeighExists(link netlink.Link, neighIP net.IP, neighMAC net.HardwareAddr) (bool, error) {
	neighs, err := netLinkOps.NeighList(link.Attrs().Index, getFamily(neighIP))
	if err != nil {
		return false, fmt.Errorf("failed to get the list of neighbour entries for link %s",
			link.Attrs().Name)
	}

	for _, neigh := range neighs {
		if neigh.IP.Equal(neighIP) {
			if bytes.Equal(neigh.HardwareAddr, neighMAC) &&
				(neigh.State&netlink.NUD_PERMANENT) == netlink.NUD_PERMANENT {
				return true, nil
			}
		}
	}
	return false, nil
}

// LinkNeighIPExists checks to see if the IP exists in IP neighbour cache
func LinkNeighIPExists(link netlink.Link, neighIP net.IP) (bool, error) {
	neighs, err := netLinkOps.NeighList(link.Attrs().Index, getFamily(neighIP))
	if err != nil {
		return false, fmt.Errorf("failed to get the list of neighbour entries for link %s",
			link.Attrs().Name)
	}

	for _, neigh := range neighs {
		if neigh.IP.Equal(neighIP) {
			return true, nil
		}
	}
	return false, nil
}

func DeleteConntrack(ip string, port int32, protocol corev1.Protocol, ipFilterType netlink.ConntrackFilterType, labels [][]byte) (uint, error) {
	ipAddress := net.ParseIP(ip)
	if ipAddress == nil {
		return 0, fmt.Errorf("value %q passed to DeleteConntrack is not an IP address", ip)
	}

	filter := &netlink.ConntrackFilter{}
	if protocol == corev1.ProtocolUDP {
		// 17 = UDP protocol
		if err := filter.AddProtocol(17); err != nil {
			return 0, fmt.Errorf("could not add Protocol UDP to conntrack filter %v", err)
		}
	} else if protocol == corev1.ProtocolSCTP {
		// 132 = SCTP protocol
		if err := filter.AddProtocol(132); err != nil {
			return 0, fmt.Errorf("could not add Protocol SCTP to conntrack filter %v", err)
		}
	} else if protocol == corev1.ProtocolTCP {
		// 6 = TCP protocol
		if err := filter.AddProtocol(6); err != nil {
			return 0, fmt.Errorf("could not add Protocol TCP to conntrack filter %v", err)
		}
	}
	if port > 0 {
		if err := filter.AddPort(netlink.ConntrackOrigDstPort, uint16(port)); err != nil {
			return 0, fmt.Errorf("could not add port %d to conntrack filter: %v", port, err)
		}
	}
	if err := filter.AddIP(ipFilterType, ipAddress); err != nil {
		return 0, fmt.Errorf("could not add IP: %s to conntrack filter: %v", ipAddress, err)
	}

	if len(labels) > 0 {
		// for now we only need unmatch label, we can add match label later if needed
		if err := filter.AddLabels(netlink.ConntrackUnmatchLabels, labels); err != nil {
			return 0, fmt.Errorf("could not add label %s to conntrack filter: %v", labels, err)
		}
	}
	klog.V(5).Infof("Deleting conntrack entry for IP %s, port %d, protocol %s, conntrack filter type %v, labels %x", ip, port, protocol, ipFilterType, labels)
	var matched uint
	var err error
	if ipAddress.To4() != nil {
		matched, err = netLinkOps.ConntrackDeleteFilters(netlink.ConntrackTable, netlink.FAMILY_V4, filter)
	} else {
		matched, err = netLinkOps.ConntrackDeleteFilters(netlink.ConntrackTable, netlink.FAMILY_V6, filter)
	}
	return matched, err
}

// DeleteConntrackServicePort is a wrapper around DeleteConntrack for the purpose of deleting conntrack entries that
// belong to ServicePorts. Before deleting any conntrack entry, it makes sure that the port is valid. If the port is
// invalid, it will log a level 5 info message and simply return.
func DeleteConntrackServicePort(ip string, port int32, protocol corev1.Protocol, ipFilterType netlink.ConntrackFilterType,
	labels [][]byte) (uint, error) {
	if err := ValidatePort(protocol, port); err != nil {
		klog.V(5).Infof("Skipping conntrack deletion for IP %q, protocol %q, port \"%d\", err: %q",
			ip, protocol, port, err)
		return 0, nil
	}
	return DeleteConntrack(ip, port, protocol, ipFilterType, labels)
}

// GetFilteredInterfaceV4V6IPs returns the IP addresses for the network interface 'iface' for ipv4 and ipv6.
// Filter out addresses that are link local, reserved for internal use or added by keepalived.
func GetFilteredInterfaceV4V6IPs(iface string) ([]*net.IPNet, error) {
	link, err := netLinkOps.LinkByName(iface)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup link %s: %v", iface, err)
	}
	netlinkAddrs, err := GetFilteredInterfaceAddrs(link, true, true)
	if err != nil {
		return nil, fmt.Errorf("failed get link %s addresses: %v", link.Attrs().Name, err)
	}
	ips := make([]*net.IPNet, 0, len(netlinkAddrs))
	for _, netlinkAddr := range netlinkAddrs {
		ips = append(ips, netlinkAddr.IPNet)
	}
	return ips, nil
}

// GetFilteredInterfaceAddrs returns addresses attached to a link and filters out link local addresses, OVN reserved IPs,
// keepalived IPs and addresses marked as secondary or deprecated.
func GetFilteredInterfaceAddrs(link netlink.Link, v4, v6 bool) ([]netlink.Addr, error) {
	var ipFamily int // value of 0 means include both IP v4 and v6 addresses
	if v4 && !v6 {
		ipFamily = netlink.FAMILY_V4
	} else if !v4 && v6 {
		ipFamily = netlink.FAMILY_V6
	}
	addrs, err := netLinkOps.AddrList(link, ipFamily)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses for %q: %v", link.Attrs().Name, err)
	}
	validAddrs := make([]netlink.Addr, 0)
	for _, addr := range addrs {
		if addr.IP.IsLinkLocalUnicast() || IsAddressReservedForInternalUse(addr.IP) || IsAddressAddedByKeepAlived(addr) {
			continue
		}
		// Ignore addresses marked as secondary or deprecated since they may
		// disappear. (In bare metal clusters using MetalLB or similar, these
		// flags are used to mark load balancer IPs that aren't permanently owned
		// by the node).
		if (addr.Flags & (unix.IFA_F_SECONDARY | unix.IFA_F_DEPRECATED)) != 0 {
			continue
		}
		validAddrs = append(validAddrs, addr)
	}
	return validAddrs, nil
}

func IsAddressReservedForInternalUse(addr net.IP) bool {
	var subnetStr string
	if addr.To4() != nil {
		subnetStr = config.Gateway.V4MasqueradeSubnet
	} else {
		subnetStr = config.Gateway.V6MasqueradeSubnet
	}
	_, subnet, err := net.ParseCIDR(subnetStr)
	if err != nil {
		klog.Errorf("Could not determine if %s is in reserved subnet %v: %v",
			addr, subnetStr, err)
		return false
	}
	return subnet.Contains(addr)
}

// IsAddressAddedByKeepAlived returns true if the input interface address obtained
// through netlink has a "vip" label which is how keepalived
// marks the IP addresses it adds (https://github.com/openshift/machine-config-operator/pull/4040)
// A previous implementation made the label end with ":vip", so for backwards compatibility
// "HasSuffix" is used.
func IsAddressAddedByKeepAlived(addr netlink.Addr) bool {
	return strings.HasSuffix(addr.Label, "vip")
}

// GetIPv6OnSubnet when given an IPv6 address with a 128 prefix for an interface,
// looks for possible broadest subnet on-link routes and returns the same address
// with the found subnet prefix. Otherwise it returns the provided address unchanged.
func GetIPv6OnSubnet(iface string, ip *net.IPNet) (*net.IPNet, error) {
	if s, _ := ip.Mask.Size(); s != 128 {
		return ip, nil
	}

	link, err := netLinkOps.LinkByName(iface)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup link %s: %v", iface, err)
	}

	routeFilter := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Gw:        nil,
	}
	filterMask := netlink.RT_FILTER_GW | netlink.RT_FILTER_OIF
	routes, err := netLinkOps.RouteListFiltered(netlink.FAMILY_V6, routeFilter, filterMask)
	if err != nil {
		return nil, fmt.Errorf("failed to get on-link routes for ip %s and iface %s", ip.String(), iface)
	}

	dst := *ip
	for _, route := range routes {
		if route.Dst.Contains(dst.IP) && !dst.Contains(route.Dst.IP) {
			dst.Mask = route.Dst.Mask
		}
	}

	return &dst, nil
}

// GetIFNameAndMTUForAddress returns the interfaceName and MTU for the given network address
func GetIFNameAndMTUForAddress(ifAddress net.IP) (string, int, error) {
	// from the IP address arrive at the link
	addressFamily := getFamily(ifAddress)
	allAddresses, err := netLinkOps.AddrList(nil, addressFamily)
	if err != nil {
		return "", 0, fmt.Errorf("failed to list all the addresses for address family (%d): %v", addressFamily, err)

	}
	for _, address := range allAddresses {
		if address.IP.Equal(ifAddress) {
			link, err := netLinkOps.LinkByIndex(address.LinkIndex)
			if err != nil {
				return "", 0, fmt.Errorf("failed to lookup link with address(%s) and index(%d): %v",
					ifAddress, address.LinkIndex, err)
			}

			return link.Attrs().Name, link.Attrs().MTU, nil
		}
	}

	return "", 0, fmt.Errorf("couldn't not find a link associated with the given OVN Encap IP (%s)", ifAddress)
}

// IsIPNetEqual returns true if both IPNet are equal
func IsIPNetEqual(ipn1 *net.IPNet, ipn2 *net.IPNet) bool {
	if ipn1 == ipn2 {
		return true
	}
	if ipn1 == nil || ipn2 == nil {
		return false
	}
	m1, _ := ipn1.Mask.Size()
	m2, _ := ipn2.Mask.Size()
	return m1 == m2 && ipn1.IP.Equal(ipn2.IP)
}

func filterRouteByDstAndGw(link netlink.Link, subnet *net.IPNet, gw net.IP) (*netlink.Route, uint64) {
	return &netlink.Route{
			Dst:       subnet,
			LinkIndex: link.Attrs().Index,
			Gw:        gw,
		},
		netlink.RT_FILTER_DST | netlink.RT_FILTER_OIF | netlink.RT_FILTER_GW
}

func GetIPFamily(v6 bool) int {
	if v6 {
		return netlink.FAMILY_V6
	}
	return netlink.FAMILY_V4
}

func (defaultNetLinkOps) LinkSetVfHardwareAddr(pfLink netlink.Link, vfIndex int, hwaddr net.HardwareAddr) error {
	return netlink.LinkSetVfHardwareAddr(pfLink, vfIndex, hwaddr)
}

func findUsableInterfaceForNetwork(ipAddr net.IP) (*net.Interface, error) {
	ifaces, err := net.Interfaces()

	if err != nil {
		return nil, err
	}

	isDown := func(iface net.Interface) bool {
		return iface.Flags&1 == 0
	}

	for _, iface := range ifaces {
		if isDown(iface) {
			continue
		}
		found, err := ipAddrExistsAtInterface(ipAddr, iface)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}

		return &iface, nil
	}
	return nil, errors.New("no usable interface found")
}

func ipAddrExistsAtInterface(ipAddr net.IP, iface net.Interface) (bool, error) {
	addrs, err := iface.Addrs()

	if err != nil {
		return false, err
	}

	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ipnet.Contains(ipAddr) {
				return true, nil
			}
		}
	}
	return false, nil
}

var supportsIPv6InterfaceForwarding atomic.Bool
var checkSupportsIPv6InterfaceForwarding sync.Once

// SupportsIPv6InterfaceForwarding checks whether the kernel supports per-interface IPv6
// forwarding, as opposed to only global IPv6 forwarding.
func SupportsIPv6InterfaceForwarding() bool {
	checkSupportsIPv6InterfaceForwarding.Do(func() {
		// We avoid using RunSysctl here because we don't want unit tests to have
		// to deal with figuring out exactly when this function is going to be
		// called so they can fake it out with fExec.
		_, err := exec.Command("sysctl", "net.ipv6.conf.lo.force_forwarding").CombinedOutput()
		supportsIPv6InterfaceForwarding.Store(err == nil)
	})
	return supportsIPv6InterfaceForwarding.Load()
}

// SetSupportsIPv6InterfaceForwarding overrides the value returned by
// SupportsIPv6InterfaceForwarding(), for testing purposes.
func SetSupportsIPv6InterfaceForwarding(val bool) {
	// Make sure the Once has run, so it doesn't override us later.
	_ = SupportsIPv6InterfaceForwarding()

	supportsIPv6InterfaceForwarding.Store(val)
}

// hostProcSysNet is where the chart mounts the host's /proc/sys/net for an
// unprivileged ovnkube-node. The runtime mounts the container's own /proc/sys
// read-only unless the container is privileged. A hostPath of /proc/sys/net
// is the same procfs without that flag: the pod runs in the host network namespace,
// and CAP_NET_ADMIN is all the kernel asks for to write them.
const hostProcSysNet = "/host/proc/sys/net"

// procSysNet is the directory sysctl writes go to.
var procSysNet = detectProcSysNet()

// procSysNetIsTestDir is set when tests redirect procSysNet to a scratch
// directory, where the sysctl files have to be created rather than found.
var procSysNetIsTestDir bool

func detectProcSysNet() string {
	if _, err := os.Stat(hostProcSysNet); err == nil {
		return hostProcSysNet
	}
	return "/proc/sys/net"
}

// SysctlNetPath returns the file for the net.* sysctl at path, given relative
// to net/, e.g. "ipv4/conf/eth0/forwarding".
func SysctlNetPath(path string) string {
	return filepath.Join(procSysNet, path)
}

// SetSysctlNet writes value to the net.* sysctl at path, given relative to
// net/. Interface names are used as-is: unlike the sysctl CLI, the file path
// has no separator conflict with dots in interface names.
func SetSysctlNet(path, value string) error {
	file := SysctlNetPath(path)
	if procSysNetIsTestDir {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(file, []byte(value), 0o644); err != nil {
		return fmt.Errorf("failed to set sysctl %s to %s: %w", file, value, err)
	}
	return nil
}

// SetProcSysNetForTesting redirects sysctl writes to dir and returns a function
// restoring the previous location.
func SetProcSysNetForTesting(dir string) func() {
	prev, prevIsTestDir := procSysNet, procSysNetIsTestDir
	procSysNet, procSysNetIsTestDir = dir, true
	return func() { procSysNet, procSysNetIsTestDir = prev, prevIsTestDir }
}

// SetForwardingModeForInterface updates the forwarding options for the specified interface
func SetForwardingModeForInterface(ifName string) error {
	if config.IPv4Mode {
		if err := SetSysctlNet(fmt.Sprintf("ipv4/conf/%s/forwarding", ifName), "1"); err != nil {
			return fmt.Errorf("could not set the correct IPv4 forwarding value for interface %s: %w", ifName, err)
		}
	}
	if config.IPv6Mode && SupportsIPv6InterfaceForwarding() {
		if err := SetSysctlNet(fmt.Sprintf("ipv6/conf/%s/force_forwarding", ifName), "1"); err != nil {
			return fmt.Errorf("could not set the correct IPv6 forwarding value for interface %s: %w", ifName, err)
		}
	}
	return nil
}

const rpFilterLooseMode = "2"

// SetRPFilterLooseModeForInterface updates the reverse path filtering options for the
// specified interface to avoid dropping packets with masqueradeIP coming out of
// managementport interface.
// NOTE: v6 doesn't have rp_filter strict mode block
func SetRPFilterLooseModeForInterface(ifName string) error {
	if err := SetSysctlNet(fmt.Sprintf("ipv4/conf/%s/rp_filter", ifName), rpFilterLooseMode); err != nil {
		return fmt.Errorf("could not set the correct rp_filter value for interface %s: %w", ifName, err)
	}
	return nil
}

// SetIPv6KeepAddrOnDownForInterface preserves global IPv6 addresses when an
// interface changes state while it is attached to or detached from a VRF.
// IsIPv6SysctlSupported reports whether the kernel exposes IPv6 sysctls: a
// kernel with IPv6 disabled (ipv6.disable=1) has no /proc/sys/net/ipv6 tree.
func IsIPv6SysctlSupported() bool {
	_, err := os.Stat("/proc/sys/net/ipv6")
	return err == nil
}

func SetIPv6KeepAddrOnDownForInterface(ifName string) error {
	if err := SetSysctlNet(fmt.Sprintf("ipv6/conf/%s/keep_addr_on_down", ifName), "1"); err != nil {
		return fmt.Errorf("could not enable IPv6 address retention for interface %s: %w", ifName, err)
	}
	return nil
}

// FilterRoutesByIfIndex sieves the provided routes to those that reference
// the provided interface index. Filtering with netlink.RT_FILTER_OIF instead
// would miss multipath routes, whose interfaces are carried in the nexthops
// of Route.MultiPath while the top-level LinkIndex is 0.
// FilterRoutesByIfIndex will return:
//
//	a) if ifIdx <= 0: routesUnfiltered
//	b) else: a filtered list of routes
//
// The filter works as follows:
//
//	i)  return routes with LinkIndex == ifIdx
//	ii) return routes with LinkIndex == 0 and where *all* MultiPaths have LinkIndex == ifIdx
//
// That also means: if a MultiPath route points out different interfaces,
// then skip that route and do not return it.
func FilterRoutesByIfIndex(routesUnfiltered []netlink.Route, ifIdx int) []netlink.Route {
	if ifIdx <= 0 {
		return routesUnfiltered
	}
	var routes []netlink.Route
	for _, r := range routesUnfiltered {
		if r.LinkIndex == ifIdx ||
			multipathRouteMatchesIfIndex(r, ifIdx) {
			routes = append(routes, r)
		}
	}
	return routes
}

// multipathRouteMatchesIfIndex is a helper for FilterRoutesByIfIndex.
func multipathRouteMatchesIfIndex(r netlink.Route, ifIdx int) bool {
	if r.LinkIndex != 0 {
		return false
	}
	if len(r.MultiPath) == 0 {
		return false
	}
	for _, mr := range r.MultiPath {
		if mr.LinkIndex != ifIdx {
			return false
		}
	}
	return true
}

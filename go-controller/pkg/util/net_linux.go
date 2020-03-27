// +build linux

package util

import (
	"fmt"
	"net"
	"os"

	"github.com/vishvananda/netlink"
)

// LinkSetUp returns the netlink device with its state marked up
func LinkSetUp(interfaceName string) (netlink.Link, error) {
	link, err := netlink.LinkByName(interfaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup link %s: %v", interfaceName, err)
	}
	err = netlink.LinkSetUp(link)
	if err != nil {
		return nil, fmt.Errorf("failed to set the link %s up: %v", interfaceName, err)
	}
	return link, nil
}

// LinkAddrAdd removes existing addresses on the link and adds the new address
func LinkAddrAdd(link netlink.Link, address string) error {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to list addresses for the link %s: %v", link.Attrs().Name, err)
	}
	for _, addr := range addrs {
		err = netlink.AddrDel(link, &addr)
		if err != nil {
			return fmt.Errorf("failed to delete address on link %s: %v", link.Attrs().Name, err)
		}
	}

	ipnet, err := netlink.ParseIPNet(address)
	if err != nil {
		return fmt.Errorf("failed to parse ip %s :%v\n", address, err)
	}
	err = netlink.AddrAdd(link, &netlink.Addr{IPNet: ipnet})
	if err != nil {
		return fmt.Errorf("failed to add address %s on link %s: %v", address, link.Attrs().Name, err)
	}
	return nil
}

// LinkRouteAdd flushes any existing routes for given subnet and adds a new route
// for that subnet through the gwIPstr
func LinkRouteAdd(link netlink.Link, gwIPstr string, subnets []string) error {
	gwIP := net.ParseIP(gwIPstr)
	if gwIP == nil {
		return fmt.Errorf("gateway IP %s is not a valid IPv4 or IPv6 address", gwIPstr)
	}
	family := netlink.FAMILY_V4
	if gwIP.To4() == nil {
		family = netlink.FAMILY_V6
	}
	for _, subnet := range subnets {
		dstIPnet, err := netlink.ParseIPNet(subnet)
		if err != nil {
			return fmt.Errorf("failed to parse subnet %s :%v\n", subnet, err)
		}
		routeFilter := &netlink.Route{Dst: dstIPnet}
		filterMask := netlink.RT_FILTER_DST
		routes, err := netlink.RouteListFiltered(family, routeFilter, filterMask)
		if err != nil {
			return fmt.Errorf("failed to get the list of routes for subnet %s", subnet)
		}
		for _, route := range routes {
			err = netlink.RouteDel(&route)
			if err != nil {
				return fmt.Errorf("failed to delete route for subnet %s : %v\n", subnet, err)
			}
		}
		route := &netlink.Route{
			Dst:       dstIPnet,
			LinkIndex: link.Attrs().Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Gw:        gwIP,
		}
		err = netlink.RouteAdd(route)
		if err != nil {
			if os.IsExist(err) {
				return err
			}
			return fmt.Errorf("failed to add route for subnet %s via gateway %s: %v",
				subnet, gwIPstr, err)
		}
	}
	return nil
}

// LinkNeighAdd flushes any existing MAC/IP bindings and adds the new neighbour entries
func LinkNeighAdd(link netlink.Link, neighIPstr, neighMacstr string) error {
	neighIP := net.ParseIP(neighIPstr)
	if neighIP == nil {
		return fmt.Errorf("neighbour IP %s is not a valid IPv4 or IPv6 address", neighIPstr)
	}
	hwAddr, err := net.ParseMAC(neighMacstr)
	if err != nil {
		return fmt.Errorf("neighbour MAC address %s is not valid: %v", neighMacstr, err)
	}

	family := netlink.FAMILY_V4
	if neighIP.To4() == nil {
		family = netlink.FAMILY_V6
	}
	neigh := &netlink.Neigh{
		LinkIndex:    link.Attrs().Index,
		Family:       family,
		State:        netlink.NUD_PERMANENT,
		IP:           neighIP,
		HardwareAddr: hwAddr,
	}
	err = netlink.NeighSet(neigh)
	if err != nil {
		return fmt.Errorf("failed to add neighbour entry %+v: %v", neigh, err)
	}
	return nil
}

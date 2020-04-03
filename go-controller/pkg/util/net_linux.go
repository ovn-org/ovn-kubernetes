// +build linux

package util

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/vishvananda/netlink"

	utilnet "k8s.io/utils/net"
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

// LinkAddrFlush flushes all the addresses on the given link
func LinkAddrFlush(link netlink.Link) error {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to list addresses for the link %s: %v", link.Attrs().Name, err)
	}
	for _, addr := range addrs {
		err = netlink.AddrDel(link, &addr)
		if err != nil {
			return fmt.Errorf("failed to delete address %s on link %s: %v",
				addr.IP.String(), link.Attrs().Name, err)
		}
	}
	return nil
}

// LinkAddrExist returns true if the given address is present on the link
func LinkAddrExist(link netlink.Link, address string) (bool, error) {
	ipnet, err := netlink.ParseIPNet(address)
	if err != nil {
		return false, fmt.Errorf("failed to parse ip %s :%v\n", address, err)
	}
	family := netlink.FAMILY_V4
	if ipnet.IP.To4() == nil {
		family = netlink.FAMILY_V6
	}
	addrs, err := netlink.AddrList(link, family)
	if err != nil {
		return false, fmt.Errorf("failed to list addresses for the link %s: %v",
			link.Attrs().Name, err)
	}
	for _, addr := range addrs {
		if addr.IPNet.String() == address {
			return true, nil
		}
	}
	return false, nil
}

// LinkAddrAdd removes existing addresses on the link and adds the new address
func LinkAddrAdd(link netlink.Link, address string) error {
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

// LinkRoutesDel deletes all the routes for the given subnets via the link
func LinkRoutesDel(link netlink.Link, subnets []string) error {
	routes, err := netlink.RouteList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to get all the routes for link %s: %v",
			link.Attrs().Name, err)
	}
	for _, subnet := range subnets {
		for _, route := range routes {
			if route.Dst.String() == subnet {
				err = netlink.RouteDel(&route)
				if err != nil {
					return fmt.Errorf("failed to delete route '%s via %s' for link %s : %v\n",
						route.Dst.String(), route.Gw.String(), link.Attrs().Name, err)
				}
				break
			}
		}
	}
	return nil
}

// LinkRoutesAdd adds a new route for given subnets through the gwIPstr
func LinkRoutesAdd(link netlink.Link, gwIPstr string, subnets []string) error {
	gwIP := net.ParseIP(gwIPstr)
	if gwIP == nil {
		return fmt.Errorf("gateway IP %s is not a valid IPv4 or IPv6 address", gwIPstr)
	}
	for _, subnet := range subnets {
		dstIPnet, err := netlink.ParseIPNet(subnet)
		if err != nil {
			return fmt.Errorf("failed to parse subnet %s :%v\n", subnet, err)
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

// LinkRouteExists checks for existence of routes for the given subnet through gwIPStr
func LinkRouteExists(link netlink.Link, gwIPstr, subnet string) (bool, error) {
	gwIP := net.ParseIP(gwIPstr)
	if gwIP == nil {
		return false, fmt.Errorf("gateway IP %s is not a valid IPv4 or IPv6 address", gwIPstr)
	}
	family := netlink.FAMILY_V4
	if utilnet.IsIPv6(gwIP) {
		family = netlink.FAMILY_V6
	}

	dstIPnet, err := netlink.ParseIPNet(subnet)
	if err != nil {
		return false, fmt.Errorf("failed to parse subnet %s :%v\n", subnet, err)
	}
	routeFilter := &netlink.Route{Dst: dstIPnet}
	filterMask := netlink.RT_FILTER_DST
	routes, err := netlink.RouteListFiltered(family, routeFilter, filterMask)
	if err != nil {
		return false, fmt.Errorf("failed to get routes for subnet %s", subnet)
	}
	for _, route := range routes {
		if route.Gw.String() == gwIPstr {
			return true, nil
		}
	}
	return false, nil
}

// LinkNeighAdd adds MAC/IP bindings for the given link
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
	if utilnet.IsIPv6(neighIP) {
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

// LinkNeighExists checks to see if the given MAC/IP bindings exists
func LinkNeighExists(link netlink.Link, neighIPstr, neighMacstr string) (bool, error) {
	neighIP := net.ParseIP(neighIPstr)
	if neighIP == nil {
		return false, fmt.Errorf("neighbour IP %s is not a valid IPv4 or IPv6 address",
			neighIPstr)
	}

	family := netlink.FAMILY_V4
	if utilnet.IsIPv6(neighIP) {
		family = netlink.FAMILY_V6
	}

	neighs, err := netlink.NeighList(link.Attrs().Index, family)
	if err != nil {
		return false, fmt.Errorf("failed to get the list of neighbour entries for link %s",
			link.Attrs().Name)
	}

	for _, neigh := range neighs {
		if neigh.IP.String() == neighIPstr {
			if neigh.HardwareAddr.String() == strings.ToLower(neighMacstr) &&
				(neigh.State&netlink.NUD_PERMANENT) == netlink.NUD_PERMANENT {
				return true, nil
			}
		}
	}
	return false, nil
}

// +build linux

package util

import (
	"bytes"
	"fmt"
	"net"
	"os"

	"github.com/vishvananda/netlink"

	utilnet "k8s.io/utils/net"
)

func getFamily(ip net.IP) int {
	if utilnet.IsIPv6(ip) {
		return netlink.FAMILY_V6
	} else {
		return netlink.FAMILY_V4
	}
}

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
func LinkAddrExist(link netlink.Link, address *net.IPNet) (bool, error) {
	addrs, err := netlink.AddrList(link, getFamily(address.IP))
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

// LinkAddrAdd removes existing addresses on the link and adds the new address
func LinkAddrAdd(link netlink.Link, address *net.IPNet) error {
	err := netlink.AddrAdd(link, &netlink.Addr{IPNet: address})
	if err != nil {
		return fmt.Errorf("failed to add address %s on link %s: %v", address, link.Attrs().Name, err)
	}
	return nil
}

// LinkRoutesDel deletes all the routes for the given subnets via the link
func LinkRoutesDel(link netlink.Link, subnets []*net.IPNet) error {
	routes, err := netlink.RouteList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to get all the routes for link %s: %v",
			link.Attrs().Name, err)
	}
	for _, subnet := range subnets {
		for _, route := range routes {
			if route.Dst.String() == subnet.String() {
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
func LinkRoutesAdd(link netlink.Link, gwIP net.IP, subnets []*net.IPNet) error {
	for _, subnet := range subnets {
		route := &netlink.Route{
			Dst:       subnet,
			LinkIndex: link.Attrs().Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Gw:        gwIP,
		}
		err := netlink.RouteAdd(route)
		if err != nil {
			if os.IsExist(err) {
				return err
			}
			return fmt.Errorf("failed to add route for subnet %s via gateway %s: %v",
				subnet.String(), gwIP.String(), err)
		}
	}
	return nil
}

// LinkRouteExists checks for existence of routes for the given subnet through gwIPStr
func LinkRouteExists(link netlink.Link, gwIP net.IP, subnet *net.IPNet) (bool, error) {
	routeFilter := &netlink.Route{Dst: subnet}
	filterMask := netlink.RT_FILTER_DST
	routes, err := netlink.RouteListFiltered(getFamily(gwIP), routeFilter, filterMask)
	if err != nil {
		return false, fmt.Errorf("failed to get routes for subnet %s", subnet.String())
	}
	for _, route := range routes {
		if route.Gw.Equal(gwIP) {
			return true, nil
		}
	}
	return false, nil
}

// LinkNeighAdd adds MAC/IP bindings for the given link
func LinkNeighAdd(link netlink.Link, neighIP net.IP, neighMAC net.HardwareAddr) error {
	neigh := &netlink.Neigh{
		LinkIndex:    link.Attrs().Index,
		Family:       getFamily(neighIP),
		State:        netlink.NUD_PERMANENT,
		IP:           neighIP,
		HardwareAddr: neighMAC,
	}
	err := netlink.NeighSet(neigh)
	if err != nil {
		return fmt.Errorf("failed to add neighbour entry %+v: %v", neigh, err)
	}
	return nil
}

// LinkNeighExists checks to see if the given MAC/IP bindings exists
func LinkNeighExists(link netlink.Link, neighIP net.IP, neighMAC net.HardwareAddr) (bool, error) {
	neighs, err := netlink.NeighList(link.Attrs().Index, getFamily(neighIP))
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

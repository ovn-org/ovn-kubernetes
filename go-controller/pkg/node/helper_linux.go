//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"
)

type GatewayInterfaceMismatchError struct {
	msg string
}

func (error *GatewayInterfaceMismatchError) Error() string {
	return error.msg
}

func newGatewayInterfaceMismatchError(format string, args ...interface{}) *GatewayInterfaceMismatchError {
	return &GatewayInterfaceMismatchError{
		msg: fmt.Sprintf(format, args...),
	}
}

// getDefaultGatewayInterfaceDetails returns the interface name on
// which the default gateway (for route to 0.0.0.0) is configured.
// optionally pass the pre-determined gateway interface
// It also returns the default gateways themselves.
func getDefaultGatewayInterfaceDetails(gwIface string, ipV4Mode, ipV6Mode bool) (string, []net.IP, error) {
	var intfName string
	var gatewayIPs []net.IP

	if ipV4Mode {
		intfIPv4Name, gw, err := getDefaultGatewayInterfaceByFamily(netlink.FAMILY_V4, gwIface)
		if err != nil {
			return "", gatewayIPs, err
		}
		intfName = intfIPv4Name

		// only add the GW IP if it is specified
		if len(gw) != 0 {
			gatewayIPs = append(gatewayIPs, gw)
		}
	}

	if ipV6Mode {
		intfIPv6Name, gw, err := getDefaultGatewayInterfaceByFamily(netlink.FAMILY_V6, gwIface)
		if err != nil {
			return "", gatewayIPs, err
		}

		// if there is an interface specified for both IP families
		// validate they use the same one
		if intfName == "" {
			intfName = intfIPv6Name
		} else if (len(intfName) > 0 && len(intfIPv6Name) > 0) && intfName != intfIPv6Name {
			return "", nil, fmt.Errorf("multiple gateway interfaces detected: '%s' '%s'", intfName, intfIPv6Name)
		}

		// only add the GW IP if it is specified
		if len(gw) != 0 {
			gatewayIPs = append(gatewayIPs, gw)
		}
	}

	return intfName, gatewayIPs, nil
}

// uses netlink to do a route lookup for default gateway
// takes IP address family and optional name of a pre-determined gateway interface
// returns name of default gateway interface, the default gateway ip, and any error
func getDefaultGatewayInterfaceByFamily(family int, gwIface string) (string, net.IP, error) {
	// filter the default route to obtain the gateway
	filter := &netlink.Route{Dst: nil}
	mask := netlink.RT_FILTER_DST
	gwIfIdx := 0
	// gw interface provided
	if len(gwIface) > 0 {
		link, err := util.GetNetLinkOps().LinkByName(gwIface)
		if err != nil {
			return "", nil, fmt.Errorf("error looking up gw interface: %q, error: %w", gwIface, err)
		}
		if link.Attrs() == nil {
			return "", nil, fmt.Errorf("no attributes found for link: %#v", link)
		}
		gwIfIdx = link.Attrs().Index
		klog.Infof("Provided gateway interface %q, found as index: %d", gwIface, gwIfIdx)
	}

	routeList, err := util.GetNetLinkOps().RouteListFiltered(family, filter, mask)
	if err != nil {
		return "", nil, errors.Wrapf(err, "failed to get routing table in node")
	}
	routes := filterRoutesByIfIndex(routeList, gwIfIdx)
	// use the first valid default gateway
	for _, r := range routes {
		// no multipath
		if len(r.MultiPath) == 0 {
			intfLink, err := util.GetNetLinkOps().LinkByIndex(r.LinkIndex)
			if err != nil {
				klog.Warningf("Failed to get interface link for route %v : %v", r, err)
				continue
			}
			if r.Gw == nil {
				klog.Warningf("Failed to get gateway for route %v : %v", r, err)
				continue
			}
			if intfLink.Attrs() == nil {
				return "", nil, fmt.Errorf("no attributes found for link: %#v", intfLink.Attrs())
			}
			foundIfName := intfLink.Attrs().Name
			klog.Infof("Found default gateway interface %s %s", foundIfName, r.Gw.String())
			if len(gwIface) > 0 && gwIface != foundIfName {
				// this should not happen, but if it did, indicates something broken with our use of the netlink lib
				return "", nil, newGatewayInterfaceMismatchError("mismatching provided gw interface: %s and gateway found: %s",
					gwIface, foundIfName)
			}
			return foundIfName, r.Gw, nil
		}

		// multipath, use the first valid entry
		// TODO: revisit for full multipath support
		// xref: https://github.com/vishvananda/netlink/blob/6ffafa9fc19b848776f4fd608c4ad09509aaacb4/route.go#L137-L145
		for _, nh := range r.MultiPath {
			intfLink, err := util.GetNetLinkOps().LinkByIndex(nh.LinkIndex)
			if err != nil {
				klog.Warningf("Failed to get interface link for route %v : %v", nh, err)
				continue
			}
			if nh.Gw == nil {
				klog.Warningf("Failed to get gateway for multipath route %v : %v", nh, err)
				continue
			}
			if intfLink.Attrs() == nil {
				return "", nil, fmt.Errorf("no attributes found for link: %#v", intfLink.Attrs())
			}
			foundIfName := intfLink.Attrs().Name
			klog.Infof("Found default gateway interface %s %s", foundIfName, nh.Gw.String())
			if len(gwIface) > 0 && gwIface != foundIfName {
				// this should not happen, but if it did, indicates something broken with our use of the netlink lib
				return "", nil, newGatewayInterfaceMismatchError("mismatching provided gw interface: %q and gateway found: %q",
					gwIface, foundIfName)
			}
			return foundIfName, nh.Gw, nil
		}
	}
	return "", net.IP{}, nil
}

func getIntfName(gatewayIntf string) (string, error) {
	// The given (or autodetected) interface is an OVS bridge and this could be
	// created by us using util.NicToBridge() or it was pre-created by the user.

	// Is intfName a port of gatewayIntf?
	intfName, err := util.GetNicName(gatewayIntf)
	if err != nil {
		return "", err
	}
	_, stderr, err := util.RunOVSVsctl("get", "interface", intfName, "ofport")
	if err != nil {
		return "", fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			intfName, stderr, err)
	}
	return intfName, nil
}

// filterRoutesByIfIndex is a helper function that will sieve the provided routes and check
// if they match the provided index. This used to be implemented with netlink.RT_FILTER_OIF,
// however the problem is that this filtered out MultiPath IPv6 routes which have a LinkIndex of 0.
// filterRoutesByIfIndex will return:
//
//	a) if ifidx <= 0: routesUnfiltered
//	b) else: a filtered list of routes
//
// The filter works as follows:
//
//	i)  return routes with LinkIndex == ifIdx
//	ii) return routes with LinkIndex == 0 and where *all* MultiPaths have LinkIndex == ifIdx
//
// That also means: If a MultiPath route points out different interfaces, then skip that route and
// do not return it.
func filterRoutesByIfIndex(routesUnfiltered []netlink.Route, ifIdx int) []netlink.Route {
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

// multipathRouteMatchesIfIndex is a helper for filterRoutesByIfIndex.
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

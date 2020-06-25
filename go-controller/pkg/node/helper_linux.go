// +build linux

package node

import (
	"fmt"
	"net"
	"syscall"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"

	"github.com/vishvananda/netlink"
)

// getDefaultGatewayInterfaceDetails returns the interface name on
// which the default gateway (for route to 0.0.0.0) is configured.
// It also returns the default gateway itself.
func getDefaultGatewayInterfaceDetails() (string, net.IP, error) {
	routes, err := netlink.RouteList(nil, syscall.AF_INET)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get routing table in node")
	}

	for i := range routes {
		route := routes[i]
		if route.Dst == nil && route.Gw != nil && route.LinkIndex > 0 {
			intfLink, err := netlink.LinkByIndex(route.LinkIndex)
			if err != nil {
				continue
			}
			intfName := intfLink.Attrs().Name
			if intfName != "" {
				return intfName, route.Gw, nil
			}
		}
	}
	return "", nil, fmt.Errorf("failed to get default gateway interface")
}

func getIntfName(gatewayIntf string) (string, error) {
	// The given (or autodetected) interface is an OVS bridge and this could be
	// created by us using util.NicToBridge() or it was pre-created by the user.

	// Is intfName a port of gatewayIntf?
	intfName, err := util.GetNicName(gatewayIntf)
	if err != nil {
		return "", err
	}
	_, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", intfName, "ofport")
	if err != nil {
		return "", fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			intfName, stderr, err)
	}
	return intfName, nil
}

func deleteConntrack(ip string, port int32, protocol kapi.Protocol) error {
	return util.DeleteConntrack(ip, port, protocol)
}

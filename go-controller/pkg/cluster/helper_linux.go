// +build linux

package cluster

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	"github.com/vishvananda/netlink"
)

// getDefaultGatewayInterfaceDetails returns the interface name on
// which the default gateway (for route to 0.0.0.0) is configured.
// It also returns the default gateway itself.
func getDefaultGatewayInterfaceDetails() (string, string, error) {
	routes, err := netlink.RouteList(nil, syscall.AF_INET)
	if err != nil {
		return "", "", fmt.Errorf("Failed to get routing table in node")
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
				return intfName, route.Gw.String(), nil
			}
		}
	}
	return "", "", fmt.Errorf("Failed to get default gateway interface")
}

func getIntfName(gatewayIntf string) (string, error) {
	// The given (or autodetected) interface is an OVS bridge;
	// detect if we previously ran NIC/bridge setup
	if !strings.HasPrefix(gatewayIntf, "br") {
		return "", fmt.Errorf("gateway interface %s is an OVS bridge not "+
			"a physical device", gatewayIntf)
	}

	// Is intfName a port of gatewayIntf?
	intfName := util.GetNicName(gatewayIntf)
	_, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", intfName, "ofport")
	if err != nil {
		return "", fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			intfName, stderr, err)
	}
	return intfName, nil
}

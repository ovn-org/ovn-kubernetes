package app

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/errors"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
)

// NicsToBridgeCommand creates ovs bridge for provided nic interfaces.
var NicsToBridgeCommand = cli.Command{
	Name:  "nics-to-bridge",
	Usage: "Create ovs bridge for nic interfaces",
	Flags: []cli.Flag{},
	Action: func(context *cli.Context) error {
		args := context.Args()
		if len(args) == 0 {
			return fmt.Errorf("Please specify list of nic interfaces")
		}

		var errorList []error
		for _, nic := range args {
			if err := nicToBridge(nic); err != nil {
				errorList = append(errorList, err)
			}
		}

		return errors.NewAggregate(errorList)
	},
}

func getBridgeName(iface string) string {
	return fmt.Sprintf("br%s", iface)
}

func saveIPAddress(iface, bridge netlink.Link, addrs []netlink.Addr) error {
	for i := range addrs {
		addr := addrs[i]

		// Remove from old interface
		if err := netlink.AddrDel(iface, &addr); err != nil {
			logrus.Errorf("Remove addr from %q failed: %v", iface.Attrs().Name, err)
			return err
		}

		// Add to ovs bridge
		addr.Label = bridge.Attrs().Name
		if err := netlink.AddrAdd(bridge, &addr); err != nil {
			logrus.Errorf("Add addr to bridge %q failed: %v", bridge.Attrs().Name, err)
			return err
		}
		logrus.Infof("Successfully saved addr %q to bridge %q", addr.String(), bridge.Attrs().Name)
	}

	return netlink.LinkSetUp(bridge)
}

func saveRoute(iface, bridge netlink.Link, routes []netlink.Route) error {
	for i := range routes {
		route := routes[i]

		// Remove from old interface
		if err := netlink.RouteDel(&route); err != nil && !strings.Contains(err.Error(), "no such process") {
			logrus.Errorf("Remove route from %q failed: %v", iface.Attrs().Name, err)
			return err
		}

		// Add to ovs bridge
		route.LinkIndex = bridge.Attrs().Index
		if err := netlink.RouteAdd(&route); err != nil && !os.IsExist(err) {
			logrus.Errorf("Add route to bridge %q failed: %v", bridge.Attrs().Name, err)
			return err
		}

		logrus.Infof("Successfully saved route %q", route.String())
	}

	return nil
}

func nicToBridge(iface string) error {
	ifaceLink, err := netlink.LinkByName(iface)
	if err != nil {
		return err
	}

	bridge := getBridgeName(iface)
	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--may-exist", "add-br", bridge,
		"--", "br-set-external-id", bridge, "bridge-id", bridge,
		"--", "set", "bridge", bridge, "fail-mode=standalone",
		fmt.Sprintf("other_config:hwaddr=%s", ifaceLink.Attrs().HardwareAddr),
		"--", "add-port", bridge, iface)
	if err != nil {
		logrus.Errorf("Failed to create OVS bridge, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}
	logrus.Infof("Successfully created OVS bridge %q", bridge)

	// Get ip addresses and routes before any real operations.
	// Note: addrList is a workround for also getting broadcast addr.
	// TODO: switch to netlink.AddrList after https://github.com/vishvananda/netlink/pull/226 is merged.
	addrs, err := addrList(ifaceLink, syscall.AF_INET)
	if err != nil {
		return err
	}
	routes, err := netlink.RouteList(ifaceLink, syscall.AF_INET)
	if err != nil {
		return err
	}

	bridgeLink, err := netlink.LinkByName(bridge)
	if err != nil {
		return err
	}

	// save ip addresses to bridge.
	if err = saveIPAddress(ifaceLink, bridgeLink, addrs); err != nil {
		return err
	}

	// save routes to bridge.
	if err = saveRoute(ifaceLink, bridgeLink, routes); err != nil {
		return err
	}

	return nil
}

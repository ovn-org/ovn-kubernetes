// +build linux

package util

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"
	"k8s.io/klog"
)

const (
	ubuntuDefaultFile = "/etc/default/openvswitch-switch"
	rhelDefaultFile   = "/etc/default/openvswitch"
)

func getBridgeName(iface string) string {
	return fmt.Sprintf("br%s", iface)
}

// GetNicName returns the physical NIC name, given an OVS bridge name
// configured by NicToBridge()
func GetNicName(brName string) (string, error) {
	stdout, stderr, err := RunOVSVsctl(
		"br-get-external-id", brName, "bridge-uplink")
	if err != nil {
		return "", fmt.Errorf("Failed to get the bridge-uplink for the bridge %q:, stderr: %q, error: %v",
			brName, stderr, err)
	}
	if stdout == "" && strings.HasPrefix(brName, "br") {
		// This would happen if the bridge was created before the bridge-uplink
		// changes got integrated.
		return brName[len("br"):], nil
	}
	return stdout, nil
}

func saveIPAddress(oldLink, newLink netlink.Link, addrs []netlink.Addr) error {
	for i := range addrs {
		addr := addrs[i]

		// Remove from oldLink
		if err := netlink.AddrDel(oldLink, &addr); err != nil {
			klog.Errorf("Remove addr from %q failed: %v", oldLink.Attrs().Name, err)
			return err
		}

		// Add to newLink
		addr.Label = newLink.Attrs().Name
		if err := netlink.AddrAdd(newLink, &addr); err != nil {
			klog.Errorf("Add addr to newLink %q failed: %v", newLink.Attrs().Name, err)
			return err
		}
		klog.Infof("Successfully saved addr %q to newLink %q", addr.String(), newLink.Attrs().Name)
	}

	return netlink.LinkSetUp(newLink)
}

// delAddRoute removes 'route' from 'oldLink' and moves to 'newLink'
func delAddRoute(oldLink, newLink netlink.Link, route netlink.Route) error {
	// Remove route from old interface
	if err := netlink.RouteDel(&route); err != nil && !strings.Contains(err.Error(), "no such process") {
		klog.Errorf("Remove route from %q failed: %v", oldLink.Attrs().Name, err)
		return err
	}

	// Add route to newLink
	route.LinkIndex = newLink.Attrs().Index
	if err := netlink.RouteAdd(&route); err != nil && !os.IsExist(err) {
		klog.Errorf("Add route to newLink %q failed: %v", newLink.Attrs().Name, err)
		return err
	}

	klog.Infof("Successfully saved route %q", route.String())
	return nil
}

func saveRoute(oldLink, newLink netlink.Link, routes []netlink.Route) error {
	for i := range routes {
		route := routes[i]

		// Handle routes for default gateway later.  This is a special case for
		// GCE where we have /32 IP addresses and we can't add the default
		// gateway before the route to the gateway.
		if route.Dst == nil && route.Gw != nil && route.LinkIndex > 0 {
			continue
		}

		err := delAddRoute(oldLink, newLink, route)
		if err != nil {
			return err
		}
	}

	// Now add the default gateway (if any) via this interface.
	for i := range routes {
		route := routes[i]
		if route.Dst == nil && route.Gw != nil && route.LinkIndex > 0 {
			// Remove route from 'oldLink' and move it to 'newLink'
			err := delAddRoute(oldLink, newLink, route)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func setupDefaultFile() {
	platform, err := runningPlatform()
	if err != nil {
		klog.Errorf("Failed to set OVS package default file (%v)", err)
		return
	}

	var defaultFile, text string
	if platform == ubuntu {
		defaultFile = ubuntuDefaultFile
		text = "OVS_CTL_OPTS=\"$OVS_CTL_OPTS --delete-transient-ports\""
	} else if platform == rhel {
		defaultFile = rhelDefaultFile
		text = "OPTIONS=--delete-transient-ports"
	} else {
		return
	}

	fileContents, err := ioutil.ReadFile(defaultFile)
	if err != nil {
		klog.Warningf("failed to parse file %s (%v)",
			defaultFile, err)
		return
	}

	ss := strings.Split(string(fileContents), "\n")
	for _, line := range ss {
		if strings.Contains(line, "--delete-transient-ports") {
			// Nothing to do
			return
		}
	}

	// The defaultFile does not contain '--delete-transient-ports' set.
	// We should set it.
	f, err := os.OpenFile(defaultFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		klog.Errorf("failed to open %s to write (%v)", defaultFile, err)
		return
	}
	defer f.Close()

	if _, err = f.WriteString(text); err != nil {
		klog.Errorf("failed to write to %s (%v)",
			defaultFile, err)
		return
	}
}

// NicToBridge creates a OVS bridge for the 'iface' and also moves the IP
// address and routes of 'iface' to OVS bridge.
func NicToBridge(iface string) (string, error) {
	ifaceLink, err := netlink.LinkByName(iface)
	if err != nil {
		return "", err
	}

	bridge := getBridgeName(iface)
	stdout, stderr, err := RunOVSVsctl(
		"--", "--may-exist", "add-br", bridge,
		"--", "br-set-external-id", bridge, "bridge-id", bridge,
		"--", "br-set-external-id", bridge, "bridge-uplink", iface,
		"--", "set", "bridge", bridge, "fail-mode=standalone",
		fmt.Sprintf("other_config:hwaddr=%s", ifaceLink.Attrs().HardwareAddr),
		"--", "--may-exist", "add-port", bridge, iface,
		"--", "set", "port", iface, "other-config:transient=true")
	if err != nil {
		klog.Errorf("Failed to create OVS bridge, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return "", err
	}
	klog.Infof("Successfully created OVS bridge %q", bridge)

	setupDefaultFile()

	// Get ip addresses and routes before any real operations.
	addrs, err := netlink.AddrList(ifaceLink, syscall.AF_INET)
	if err != nil {
		return "", err
	}
	routes, err := netlink.RouteList(ifaceLink, syscall.AF_INET)
	if err != nil {
		return "", err
	}

	bridgeLink, err := netlink.LinkByName(bridge)
	if err != nil {
		return "", err
	}

	// save ip addresses to bridge.
	if err = saveIPAddress(ifaceLink, bridgeLink, addrs); err != nil {
		return "", err
	}

	// save routes to bridge.
	if err = saveRoute(ifaceLink, bridgeLink, routes); err != nil {
		return "", err
	}

	return bridge, nil
}

// BridgeToNic moves the IP address and routes of internal port of the bridge to
// underlying NIC interface and deletes the OVS bridge.
func BridgeToNic(bridge string) error {
	// Internal port is named same as the bridge
	bridgeLink, err := netlink.LinkByName(bridge)
	if err != nil {
		return err
	}

	// Get ip addresses and routes before any real operations.
	addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)
	if err != nil {
		return err
	}
	routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)
	if err != nil {
		return err
	}

	nicName, err := GetNicName(bridge)
	if err != nil {
		return err
	}
	ifaceLink, err := netlink.LinkByName(nicName)
	if err != nil {
		return err
	}

	// save ip addresses to iface.
	if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil {
		return err
	}

	// save routes to iface.
	if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil {
		return err
	}

	// for every bridge interface that is of type "patch", find the peer
	// interface and delete that interface from the integration bridge
	stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)
	if err != nil {
		klog.Errorf("Failed to get interfaces for OVS bridge: %q, "+
			"stderr: %q, error: %v", bridge, stderr, err)
		return err
	}
	ifacesList := strings.Split(strings.TrimSpace(stdout), "\n")
	for _, iface := range ifacesList {
		stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "type")
		if err != nil {
			klog.Warningf("Failed to determine the type of interface: %q, "+
				"stderr: %q, error: %v", iface, stderr, err)
			continue
		} else if stdout != "patch" {
			continue
		}
		stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "options:peer")
		if err != nil {
			klog.Warningf("Failed to get the peer port for patch interface: %q, "+
				"stderr: %q, error: %v", iface, stderr, err)
			continue
		}
		// stdout has the peer interface, just delete it
		peer := strings.TrimSpace(stdout)
		_, stderr, err = RunOVSVsctl("--if-exists", "del-port", "br-int", peer)
		if err != nil {
			klog.Warningf("Failed to delete patch port %q on br-int, "+
				"stderr: %q, error: %v", peer, stderr, err)
		}
	}

	// Now delete the bridge
	stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)
	if err != nil {
		klog.Errorf("Failed to delete OVS bridge, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}
	klog.Infof("Successfully deleted OVS bridge %q", bridge)
	return nil
}

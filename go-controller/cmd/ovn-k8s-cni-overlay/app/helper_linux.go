// +build linux

package app

import (
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

func renameLink(curName, newName string) error {
	link, err := netlink.LinkByName(curName)
	if err != nil {
		return err
	}

	if err := netlink.LinkSetDown(link); err != nil {
		return err
	}
	if err := netlink.LinkSetName(link, newName); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}

	return nil
}

func setupInterface(netns ns.NetNS, containerID, ifName, macAddress, ipAddress, gatewayIP string, mtu int) (*current.Interface, *current.Interface, error) {
	hostIface := &current.Interface{}
	contIface := &current.Interface{}

	var oldHostVethName string
	err := netns.Do(func(hostNS ns.NetNS) error {
		// create the veth pair in the container and move host end into host netns
		hostVeth, containerVeth, err := ip.SetupVeth(ifName, mtu, hostNS)
		if err != nil {
			return err
		}
		hostIface.Mac = hostVeth.HardwareAddr.String()
		contIface.Name = containerVeth.Name

		link, err := netlink.LinkByName(contIface.Name)
		if err != nil {
			return fmt.Errorf("failed to lookup %s: %v", contIface.Name, err)
		}

		hwAddr, err := net.ParseMAC(macAddress)
		if err != nil {
			return fmt.Errorf("failed to parse mac address for %s: %v", contIface.Name, err)
		}
		err = netlink.LinkSetHardwareAddr(link, hwAddr)
		if err != nil {
			return fmt.Errorf("failed to add mac address %s to %s: %v", macAddress, contIface.Name, err)
		}
		contIface.Mac = macAddress
		contIface.Sandbox = netns.Path()

		addr, err := netlink.ParseAddr(ipAddress)
		if err != nil {
			return err
		}
		err = netlink.AddrAdd(link, addr)
		if err != nil {
			return fmt.Errorf("failed to add IP addr %s to %s: %v", ipAddress, contIface.Name, err)
		}

		gw := net.ParseIP(gatewayIP)
		if gw == nil {
			return fmt.Errorf("parse ip of gateway failed")
		}
		err = ip.AddRoute(nil, gw, link)
		if err != nil {
			return err
		}

		oldHostVethName = hostVeth.Name

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// rename the host end of veth pair
	hostIface.Name = containerID[:15]
	if err := renameLink(oldHostVethName, hostIface.Name); err != nil {
		return nil, nil, fmt.Errorf("failed to rename %s to %s: %v", oldHostVethName, hostIface.Name, err)
	}

	return hostIface, contIface, nil
}

// ConfigureInterface sets up the container interface
func ConfigureInterface(args *skel.CmdArgs, namespace string, podName string, macAddress string, ipAddress string, gatewayIP string, mtu int) ([]*current.Interface, error) {
	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return nil, fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	hostIface, contIface, err := setupInterface(netns, args.ContainerID, args.IfName, macAddress, ipAddress, gatewayIP, mtu)
	if err != nil {
		return nil, err
	}

	ifaceID := fmt.Sprintf("%s_%s", namespace, podName)

	ovsArgs := []string{
		"add-port", "br-int", hostIface.Name, "--", "set",
		"interface", hostIface.Name,
		fmt.Sprintf("external_ids:attached_mac=%s", macAddress),
		fmt.Sprintf("external_ids:iface-id=%s", ifaceID),
		fmt.Sprintf("external_ids:ip_address=%s", ipAddress),
	}
	var out []byte
	out, err = exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failure in plugging pod interface: %v\n  %q", err, string(out))
	}

	return []*current.Interface{hostIface, contIface}, nil
}

// PlatformSpecificCleanup deletes the OVS port
func PlatformSpecificCleanup(args *skel.CmdArgs) error {
	ifaceName := args.ContainerID[:15]
	ovsArgs := []string{
		"del-port", "br-int", ifaceName,
	}
	out, err := exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "no port named") {
		// DEL should be idempotent; don't return an error just log it
		logrus.Warningf("failed to delete OVS port %s: %v\n  %q", ifaceName, err, string(out))
	}
	return nil
}

// InitialPlatformCheck does nothing on Linux.
func InitialPlatformCheck(args *skel.CmdArgs) (bool, *current.Result) {
	return false, &current.Result{}
}

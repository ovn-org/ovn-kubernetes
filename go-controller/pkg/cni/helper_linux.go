// +build linux

package cni

import (
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/Mellanox/sriovnet"
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

func moveIfToNetns(ifname string, netns ns.NetNS) error {
	vfDev, err := netlink.LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("failed to lookup vf device %v: %q", ifname, err)
	}

	// move VF device to ns
	if err = netlink.LinkSetNsFd(vfDev, int(netns.Fd())); err != nil {
		return fmt.Errorf("failed to move device %+v to netns: %q", ifname, err)
	}

	return nil
}

func setupNetwork(link netlink.Link, macAddress, ipAddress, gatewayIP string) error {
	hwAddr, err := net.ParseMAC(macAddress)
	if err != nil {
		return fmt.Errorf("failed to parse mac address for %s: %v", link.Attrs().Name, err)
	}
	err = netlink.LinkSetHardwareAddr(link, hwAddr)
	if err != nil {
		return fmt.Errorf("failed to add mac address %s to %s: %v", macAddress, link.Attrs().Name, err)
	}
	addr, err := netlink.ParseAddr(ipAddress)
	if err != nil {
		return err
	}
	err = netlink.AddrAdd(link, addr)
	if err != nil {
		return fmt.Errorf("failed to add IP addr %s to %s: %v", ipAddress, link.Attrs().Name, err)
	}

	gw := net.ParseIP(gatewayIP)
	if gw == nil {
		return fmt.Errorf("parse ip of gateway failed")
	}
	return ip.AddRoute(nil, gw, link)
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

		err = setupNetwork(link, macAddress, ipAddress, gatewayIP)
		if err != nil {
			return err
		}
		contIface.Mac = macAddress
		contIface.Sandbox = netns.Path()

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

// Setup sriov interface in the pod
func setupSriovInterface(netns ns.NetNS, containerID, ifName, macAddress, ipAddress, gatewayIP string, mtu int, pciAddrs string) (*current.Interface, *current.Interface, error) {
	hostIface := &current.Interface{}
	contIface := &current.Interface{}

	// 1. get VF netdevice from PCI
	vfNetdevices, err := sriovnet.GetNetDevicesFromPci(pciAddrs)
	if err != nil {
		return nil, nil, err

	}

	// Make sure we have 1 netdevice per pci address
	if len(vfNetdevices) != 1 {
		return nil, nil, fmt.Errorf("failed to get one netdevice interface per %s", pciAddrs)
	}
	vfNetdevice := vfNetdevices[0]

	// 2. get Uplink netdevice
	uplink, err := sriovnet.GetUplinkRepresentor(pciAddrs)
	if err != nil {
		return nil, nil, err
	}

	// 3. get VF index from PCI
	vfIndex, err := sriovnet.GetVfIndexByPciAddress(pciAddrs)
	if err != nil {
		return nil, nil, err
	}

	// 4. lookup representor
	rep, err := sriovnet.GetVfRepresentor(uplink, vfIndex)
	if err != nil {
		return nil, nil, err
	}
	oldHostRepName := rep

	// 5. rename the host VF representor
	hostIface.Name = containerID[:15]
	if err = renameLink(oldHostRepName, hostIface.Name); err != nil {
		return nil, nil, fmt.Errorf("failed to rename %s to %s: %v", oldHostRepName, hostIface.Name, err)
	}
	link, err := netlink.LinkByName(hostIface.Name)
	if err != nil {
		return nil, nil, err
	}
	hostIface.Mac = link.Attrs().HardwareAddr.String()

	// 6. set MTU on VF representor
	if err = netlink.LinkSetMTU(link, mtu); err != nil {
		return nil, nil, fmt.Errorf("failed to set MTU on %s: %v", hostIface.Name, err)
	}

	// 7. Move VF to Container namespace
	err = moveIfToNetns(vfNetdevice, netns)
	if err != nil {
		return nil, nil, err
	}

	err = netns.Do(func(hostNS ns.NetNS) error {
		contIface.Name = ifName
		err = renameLink(vfNetdevice, contIface.Name)
		if err != nil {
			return err
		}
		link, err = netlink.LinkByName(contIface.Name)
		if err != nil {
			return err
		}
		err = netlink.LinkSetMTU(link, mtu)
		if err != nil {
			return err
		}
		err = netlink.LinkSetUp(link)
		if err != nil {
			return err
		}

		err = setupNetwork(link, macAddress, ipAddress, gatewayIP)
		if err != nil {
			return err
		}

		contIface.Mac = macAddress
		contIface.Sandbox = netns.Path()

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return hostIface, contIface, nil
}

var iptablesCommands = [][]string{
	// Block MCS
	{"-A", "OUTPUT", "-p", "tcp", "-m", "tcp", "--dport", "22623", "-j", "REJECT"},
	{"-A", "OUTPUT", "-p", "tcp", "-m", "tcp", "--dport", "22624", "-j", "REJECT"},
	{"-A", "FORWARD", "-p", "tcp", "-m", "tcp", "--dport", "22623", "-j", "REJECT"},
	{"-A", "FORWARD", "-p", "tcp", "-m", "tcp", "--dport", "22624", "-j", "REJECT"},

	// Block cloud provider metadata IP except DNS
	{"-A", "OUTPUT", "-p", "tcp", "-m", "tcp", "-d", "169.254.169.254", "!", "--dport", "53", "-j", "REJECT"},
	{"-A", "OUTPUT", "-p", "udp", "-m", "udp", "-d", "169.254.169.254", "!", "--dport", "53", "-j", "REJECT"},
	{"-A", "FORWARD", "-p", "tcp", "-m", "tcp", "-d", "169.254.169.254", "!", "--dport", "53", "-j", "REJECT"},
	{"-A", "FORWARD", "-p", "udp", "-m", "udp", "-d", "169.254.169.254", "!", "--dport", "53", "-j", "REJECT"},
}

// ConfigureInterface sets up the container interface
func (pr *PodRequest) ConfigureInterface(namespace string, podName string, macAddress string, ipAddress string, gatewayIP string, mtu int, ingress, egress int64) ([]*current.Interface, error) {
	netns, err := ns.GetNS(pr.Netns)
	if err != nil {
		return nil, fmt.Errorf("failed to open netns %q: %v", pr.Netns, err)
	}
	defer netns.Close()

	var hostIface, contIface *current.Interface

	logrus.Debugf("CNI Conf %v", pr.CNIConf)
	if pr.CNIConf.DeviceID != "" {
		// SR-IOV Case
		hostIface, contIface, err = setupSriovInterface(netns, pr.SandboxID, pr.IfName, macAddress, ipAddress, gatewayIP, mtu, pr.CNIConf.DeviceID)

	} else {
		// General case
		hostIface, contIface, err = setupInterface(netns, pr.SandboxID, pr.IfName, macAddress, ipAddress, gatewayIP, mtu)
	}
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
		fmt.Sprintf("external_ids:sandbox=%s", pr.SandboxID),
	}

	if out, err := ovsExec(ovsArgs...); err != nil {
		return nil, fmt.Errorf("failure in plugging pod interface: %v\n  %q", err, out)
	}

	if err := clearPodBandwidth(pr.SandboxID); err != nil {
		return nil, err
	}

	if ingress > 0 || egress > 0 {
		var l netlink.Link
		l, err = netlink.LinkByName(hostIface.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to find host veth interface %s: %v", hostIface.Name, err)
		}
		err = netlink.LinkSetTxQLen(l, 1000)
		if err != nil {
			return nil, fmt.Errorf("failed to set host veth txqlen: %v", err)
		}

		if err := setPodBandwidth(pr.SandboxID, hostIface.Name, ingress, egress); err != nil {
			return nil, err
		}
	}

	err = netns.Do(func(hostNS ns.NetNS) error {
		// Block access to certain things
		for _, args := range iptablesCommands {
			out, err := exec.Command("iptables", args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("could not set up pod iptables rules: %s", string(out))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return []*current.Interface{hostIface, contIface}, nil
}

// PlatformSpecificCleanup deletes the OVS port
func (pr *PodRequest) PlatformSpecificCleanup() error {
	ifaceName := pr.SandboxID[:15]
	ovsArgs := []string{
		"del-port", "br-int", ifaceName,
	}
	out, err := exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "no port named") {
		// DEL should be idempotent; don't return an error just log it
		logrus.Warningf("failed to delete OVS port %s: %v\n  %q", ifaceName, err, string(out))
	}

	_ = clearPodBandwidth(pr.SandboxID)

	return nil
}

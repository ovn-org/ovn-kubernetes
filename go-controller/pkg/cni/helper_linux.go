//go:build linux
// +build linux

package cni

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strconv"
	"strings"

	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

type CNIPluginLibOps interface {
	AddRoute(ipn *net.IPNet, gw net.IP, dev netlink.Link, mtu int) error
	SetupVeth(contVethName string, mtu int, hostNS ns.NetNS) (net.Interface, net.Interface, error)
}

type defaultCNIPluginLibOps struct{}

var cniPluginLibOps CNIPluginLibOps = &defaultCNIPluginLibOps{}

func (defaultCNIPluginLibOps) AddRoute(ipn *net.IPNet, gw net.IP, dev netlink.Link, mtu int) error {
	route := &netlink.Route{
		LinkIndex: dev.Attrs().Index,
		Scope:     netlink.SCOPE_UNIVERSE,
		Dst:       ipn,
		Gw:        gw,
		MTU:       mtu,
	}

	return util.GetNetLinkOps().RouteAdd(route)
}

func (defaultCNIPluginLibOps) SetupVeth(contVethName string, mtu int, hostNS ns.NetNS) (net.Interface, net.Interface, error) {
	return ip.SetupVeth(contVethName, mtu, hostNS)
}

func renameLink(curName, newName string) error {
	link, err := util.GetNetLinkOps().LinkByName(curName)
	if err != nil {
		return err
	}

	if err := util.GetNetLinkOps().LinkSetDown(link); err != nil {
		return err
	}
	if err := util.GetNetLinkOps().LinkSetName(link, newName); err != nil {
		return err
	}
	if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
		return err
	}

	return nil
}

func setSysctl(sysctl string, newVal int) error {
	return ioutil.WriteFile(sysctl, []byte(strconv.Itoa(newVal)), 0640)
}

func moveIfToNetns(ifname string, netns ns.NetNS) error {
	vfDev, err := util.GetNetLinkOps().LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("failed to lookup vf device %v: %q", ifname, err)
	}

	// move VF device to ns
	if err = util.GetNetLinkOps().LinkSetNsFd(vfDev, int(netns.Fd())); err != nil {
		return fmt.Errorf("failed to move device %+v to netns: %q", ifname, err)
	}

	return nil
}

func setupNetwork(link netlink.Link, ifInfo *PodInterfaceInfo) error {
	// set the mac addresss, set down the interface before changing the mac
	// so the EUI64 link local address generated uses the new MAC when we set it up again
	if err := util.GetNetLinkOps().LinkSetDown(link); err != nil {
		return fmt.Errorf("failed to set down interface %s: %v", link.Attrs().Name, err)
	}
	if err := util.GetNetLinkOps().LinkSetHardwareAddr(link, ifInfo.MAC); err != nil {
		return fmt.Errorf("failed to add mac address %s to %s: %v", ifInfo.MAC, link.Attrs().Name, err)
	}
	if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to set up interface %s: %v", link.Attrs().Name, err)
	}

	// set the IP address
	for _, ip := range ifInfo.IPs {
		addr := &netlink.Addr{IPNet: ip}
		if err := util.GetNetLinkOps().AddrAdd(link, addr); err != nil {
			return fmt.Errorf("failed to add IP addr %s to %s: %v", ip, link.Attrs().Name, err)
		}
	}
	for _, gw := range ifInfo.Gateways {
		if err := cniPluginLibOps.AddRoute(nil, gw, link, ifInfo.RoutableMTU); err != nil {
			return fmt.Errorf("failed to add gateway route: %v", err)
		}
	}
	for _, route := range ifInfo.Routes {
		if err := cniPluginLibOps.AddRoute(route.Dest, route.NextHop, link, ifInfo.RoutableMTU); err != nil {
			return fmt.Errorf("failed to add pod route %v via %v: %v", route.Dest, route.NextHop, err)
		}
	}

	return nil
}

func setupInterface(netns ns.NetNS, containerID, ifName string, ifInfo *PodInterfaceInfo) (*current.Interface, *current.Interface, error) {
	hostIface := &current.Interface{}
	contIface := &current.Interface{}

	var oldHostVethName string
	err := netns.Do(func(hostNS ns.NetNS) error {
		// create the veth pair in the container and move host end into host netns
		hostVeth, containerVeth, err := cniPluginLibOps.SetupVeth(ifName, ifInfo.MTU, hostNS)
		if err != nil {
			return err
		}
		hostIface.Mac = hostVeth.HardwareAddr.String()
		contIface.Name = containerVeth.Name

		link, err := util.GetNetLinkOps().LinkByName(contIface.Name)
		if err != nil {
			return fmt.Errorf("failed to lookup %s: %v", contIface.Name, err)
		}

		err = setupNetwork(link, ifInfo)
		if err != nil {
			return err
		}
		contIface.Mac = ifInfo.MAC.String()
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
func setupSriovInterface(netns ns.NetNS, containerID, ifName string, ifInfo *PodInterfaceInfo, pciAddrs string) (*current.Interface, *current.Interface, error) {
	hostIface := &current.Interface{}
	contIface := &current.Interface{}

	// 1. get VF netdevice from PCI
	vfNetdevices, err := util.GetSriovnetOps().GetNetDevicesFromPci(pciAddrs)
	if err != nil {
		return nil, nil, err

	}

	// Make sure we have 1 netdevice per pci address
	if len(vfNetdevices) != 1 {
		return nil, nil, fmt.Errorf("failed to get one netdevice interface per %s", pciAddrs)
	}
	vfNetdevice := vfNetdevices[0]

	if !ifInfo.IsDPUHostMode {
		// 2. get Uplink netdevice
		uplink, err := util.GetSriovnetOps().GetUplinkRepresentor(pciAddrs)
		if err != nil {
			return nil, nil, err
		}

		// 3. get VF index from PCI
		vfIndex, err := util.GetSriovnetOps().GetVfIndexByPciAddress(pciAddrs)
		if err != nil {
			return nil, nil, err
		}

		// 4. lookup representor
		rep, err := util.GetSriovnetOps().GetVfRepresentor(uplink, vfIndex)
		if err != nil {
			return nil, nil, err
		}
		oldHostRepName := rep

		// 5. rename the host VF representor
		hostIface.Name = containerID[:15]
		if err = renameLink(oldHostRepName, hostIface.Name); err != nil {
			return nil, nil, fmt.Errorf("failed to rename %s to %s: %v", oldHostRepName, hostIface.Name, err)
		}
		link, err := util.GetNetLinkOps().LinkByName(hostIface.Name)
		if err != nil {
			return nil, nil, err
		}
		hostIface.Mac = link.Attrs().HardwareAddr.String()

		// 6. set MTU on VF representor
		if err = util.GetNetLinkOps().LinkSetMTU(link, ifInfo.MTU); err != nil {
			return nil, nil, fmt.Errorf("failed to set MTU on %s: %v", hostIface.Name, err)
		}
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
		link, err := util.GetNetLinkOps().LinkByName(contIface.Name)
		if err != nil {
			return err
		}
		err = util.GetNetLinkOps().LinkSetMTU(link, ifInfo.MTU)
		if err != nil {
			return err
		}
		err = util.GetNetLinkOps().LinkSetUp(link)
		if err != nil {
			return err
		}

		err = setupNetwork(link, ifInfo)
		if err != nil {
			return err
		}

		contIface.Mac = ifInfo.MAC.String()
		contIface.Sandbox = netns.Path()

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return hostIface, contIface, nil
}

// ConfigureOVS performs OVS configurations in order to set up Pod networking
func ConfigureOVS(ctx context.Context, namespace, podName, hostIfaceName string,
	ifInfo *PodInterfaceInfo, sandboxID string, podLister corev1listers.PodLister,
	kclient kubernetes.Interface) error {
	klog.Infof("ConfigureOVS: namespace: %s, podName: %s", namespace, podName)
	ifaceID := util.GetIfaceId(namespace, podName)
	initialPodUID := ifInfo.PodUID

	// Find and remove any existing OVS port with this iface-id. Pods can
	// have multiple sandboxes if some are waiting for garbage collection,
	// but only the latest one should have the iface-id set.
	uuids, _ := ovsFind("Interface", "_uuid", "external-ids:iface-id="+ifaceID)
	for _, uuid := range uuids {
		if out, err := ovsExec("remove", "Interface", uuid, "external-ids", "iface-id"); err != nil {
			klog.Warningf("Failed to clear stale OVS port %q iface-id %q: %v\n  %q", uuid, ifaceID, err, out)
		}
	}
	ipStrs := make([]string, len(ifInfo.IPs))
	for i, ip := range ifInfo.IPs {
		ipStrs[i] = ip.String()
	}
	// Add the new sandbox's OVS port, tag the interface as transient so stale
	// pod interfaces are scrubbed on hard reboot
	ovsArgs := []string{
		"add-port", "br-int", hostIfaceName, "--", "set",
		"interface", hostIfaceName,
		fmt.Sprintf("external_ids:attached_mac=%s", ifInfo.MAC),
		fmt.Sprintf("external_ids:iface-id=%s", ifaceID),
		fmt.Sprintf("external_ids:iface-id-ver=%s", initialPodUID),
		fmt.Sprintf("external_ids:ip_addresses=%s", strings.Join(ipStrs, ",")),
		fmt.Sprintf("external_ids:sandbox=%s", sandboxID),
		"other_config:transient=true",
	}

	if out, err := ovsExec(ovsArgs...); err != nil {
		return fmt.Errorf("failure in plugging pod interface: %v\n  %q", err, out)
	}

	if err := clearPodBandwidth(sandboxID); err != nil {
		return err
	}

	if ifInfo.Ingress > 0 || ifInfo.Egress > 0 {
		l, err := netlink.LinkByName(hostIfaceName)
		if err != nil {
			return fmt.Errorf("failed to find host veth interface %s: %v", hostIfaceName, err)
		}
		err = netlink.LinkSetTxQLen(l, 1000)
		if err != nil {
			return fmt.Errorf("failed to set host veth txqlen: %v", err)
		}

		if err := setPodBandwidth(sandboxID, hostIfaceName, ifInfo.Ingress, ifInfo.Egress); err != nil {
			return err
		}
	}

	ofPort, err := getIfaceOFPort(hostIfaceName)
	if err != nil {
		return err
	}

	if err = waitForPodInterface(ctx, ifInfo.MAC.String(), ifInfo.IPs, hostIfaceName,
		ifaceID, ofPort, ifInfo.CheckExtIDs, podLister, kclient, namespace, podName,
		initialPodUID); err != nil {
		// Ensure the error shows up in node logs, rather than just
		// being reported back to the runtime.
		klog.Warningf("[%s/%s %s] pod uid %s: %v", namespace, podName, sandboxID, initialPodUID, err)
		return err
	}
	return nil
}

// ConfigureInterface sets up the container interface
func (pr *PodRequest) ConfigureInterface(podLister corev1listers.PodLister, kclient kubernetes.Interface, ifInfo *PodInterfaceInfo) ([]*current.Interface, error) {
	netns, err := ns.GetNS(pr.Netns)
	if err != nil {
		return nil, fmt.Errorf("failed to open netns %q: %v", pr.Netns, err)
	}
	defer netns.Close()

	var hostIface, contIface *current.Interface

	klog.V(5).Infof("CNI Conf %v", pr.CNIConf)
	if pr.CNIConf.DeviceID != "" {
		// SR-IOV Case
		hostIface, contIface, err = setupSriovInterface(netns, pr.SandboxID, pr.IfName, ifInfo, pr.CNIConf.DeviceID)
	} else {
		if ifInfo.IsDPUHostMode {
			return nil, fmt.Errorf("unexpected configuration, pod request on dpu host. " +
				"device ID must be provided")
		}
		// General case
		hostIface, contIface, err = setupInterface(netns, pr.SandboxID, pr.IfName, ifInfo)
	}
	if err != nil {
		return nil, err
	}

	if !ifInfo.IsDPUHostMode {
		err = ConfigureOVS(pr.ctx, pr.PodNamespace, pr.PodName, hostIface.Name, ifInfo, pr.SandboxID,
			podLister, kclient)
		if err != nil {
			pr.deletePorts()
			return nil, err
		}
	}

	// OCP HACK: block access to MCS/metadata; https://github.com/openshift/ovn-kubernetes/pull/19
	err = setupIPTablesBlocks(netns, ifInfo)
	if err != nil {
		return nil, err
	}
	// END OCP HACK

	err = netns.Do(func(hostNS ns.NetNS) error {
		// deny IPv6 neighbor solicitations
		dadSysctlIface := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/dad_transmits", contIface.Name)
		if _, err := os.Stat(dadSysctlIface); !os.IsNotExist(err) {
			err = setSysctl(dadSysctlIface, 0)
			if err != nil {
				klog.Warningf("Failed to disable IPv6 DAD: %q", err)
			}
		}
		// generate address based on EUI64
		genSysctlIface := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/addr_gen_mode", contIface.Name)
		if _, err := os.Stat(genSysctlIface); !os.IsNotExist(err) {
			err = setSysctl(genSysctlIface, 0)
			if err != nil {
				klog.Warningf("Failed to set IPv6 address generation mode to EUI64: %q", err)
			}
		}

		return ip.SettleAddresses(contIface.Name, 10)
	})
	if err != nil {
		klog.Warningf("Failed to settle addresses: %q", err)
	}

	return []*current.Interface{hostIface, contIface}, nil
}

func (pr *PodRequest) deletePodConntrack() {
	if pr.CNIConf.PrevResult == nil {
		return
	}
	result, err := current.NewResultFromResult(pr.CNIConf.PrevResult)
	if err != nil {
		klog.Warningf("Could not convert result to current version: %v", err)
		return
	}

	for _, ip := range result.IPs {
		// Skip known non-sandbox interfaces
		if ip.Interface != nil {
			intIdx := *ip.Interface
			if intIdx >= 0 &&
				intIdx < len(result.Interfaces) && result.Interfaces[intIdx].Sandbox == "" {
				continue
			}
		}
		err = util.DeleteConntrack(ip.Address.IP.String(), 0, "")
		if err != nil {
			klog.Errorf("Failed to delete Conntrack Entry for %s: %v", ip.Address.IP.String(), err)
			continue
		}
	}
}

func (pr *PodRequest) deletePorts() {
	ifaceName := pr.SandboxID[:15]
	out, err := ovsExec("del-port", "br-int", ifaceName)
	_ = util.LinkDelete(ifaceName)
	if err != nil && !strings.Contains(out, "no port named") {
		// DEL should be idempotent; don't return an error just log it
		klog.Warningf("Failed to delete OVS port %s: %v\n  %q", ifaceName, err, string(out))
	}
}

// PlatformSpecificCleanup deletes the OVS port
func (pr *PodRequest) PlatformSpecificCleanup() error {
	pr.deletePorts()
	_ = clearPodBandwidth(pr.SandboxID)
	pr.deletePodConntrack()

	return nil
}

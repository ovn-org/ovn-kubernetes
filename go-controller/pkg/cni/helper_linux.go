//go:build linux
// +build linux

package cni

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchdb"

	"github.com/ovn-org/libovsdb/client"

	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/safchain/ethtool"
	"github.com/vishvananda/netlink"
)

type CNIPluginLibOps interface {
	AddRoute(ipn *net.IPNet, gw net.IP, dev netlink.Link, mtu int) error
	SetupVeth(contVethName string, hostVethName string, mtu int, contVethMac string, hostNS ns.NetNS) (net.Interface, net.Interface, error)
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

func (defaultCNIPluginLibOps) SetupVeth(contVethName string, hostVethName string, mtu int, contVethMac string, hostNS ns.NetNS) (net.Interface, net.Interface, error) {
	return ip.SetupVethWithName(contVethName, hostVethName, mtu, contVethMac, hostNS)
}

// This is a good value that allows fast streams of small packets to be aggregated,
// without introducing noticeable latency in slower traffic.
const udpPacketAggregationTimeout = 50 * time.Microsecond

var udpPacketAggregationTimeoutBytes = []byte(fmt.Sprintf("%d\n", udpPacketAggregationTimeout.Nanoseconds()))

// sets up the host side of a veth for UDP packet aggregation
func setupVethUDPAggregationHost(ifname string) error {
	e, err := ethtool.NewEthtool()
	if err != nil {
		return fmt.Errorf("failed to initialize ethtool: %v", err)
	}
	defer e.Close()

	err = e.Change(ifname, map[string]bool{
		"rx-gro":                true,
		"rx-udp-gro-forwarding": true,
	})
	if err != nil {
		return fmt.Errorf("could not enable interface features: %v", err)
	}
	channels, err := e.GetChannels(ifname)
	if err == nil {
		channels.RxCount = uint32(runtime.NumCPU())
		_, err = e.SetChannels(ifname, channels)
	}
	if err != nil {
		return fmt.Errorf("could not update channels: %v", err)
	}

	timeoutFile := fmt.Sprintf("/sys/class/net/%s/gro_flush_timeout", ifname)
	err = os.WriteFile(timeoutFile, udpPacketAggregationTimeoutBytes, 0644)
	if err != nil {
		return fmt.Errorf("could not set flush timeout: %v", err)
	}

	return nil
}

// sets up the container side of a veth for UDP packet aggregation
func setupVethUDPAggregationContainer(ifname string) error {
	e, err := ethtool.NewEthtool()
	if err != nil {
		return fmt.Errorf("failed to initialize ethtool: %v", err)
	}
	defer e.Close()

	channels, err := e.GetChannels(ifname)
	if err == nil {
		channels.TxCount = uint32(runtime.NumCPU())
		_, err = e.SetChannels(ifname, channels)
	}
	if err != nil {
		return fmt.Errorf("could not update channels: %v", err)
	}

	return nil
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
	return os.WriteFile(sysctl, []byte(strconv.Itoa(newVal)), 0o640)
}

func moveIfToNetns(ifname string, netns ns.NetNS) error {
	dev, err := util.GetNetLinkOps().LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("failed to lookup device %v: %q", ifname, err)
	}

	// move netdevice to ns
	if err = util.GetNetLinkOps().LinkSetNsFd(dev, int(netns.Fd())); err != nil {
		return fmt.Errorf("failed to move device %+v to netns: %q", ifname, err)
	}

	return nil
}

func setupNetwork(link netlink.Link, ifInfo *PodInterfaceInfo) error {
	// make sure link is up
	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to set up interface %s: %v", link.Attrs().Name, err)
		}
	}

	if ifInfo.SkipIPConfig {
		klog.Infof("Skipping network configuration for pod: %s", ifInfo.PodUID)
		return nil
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
	ifnameSuffix := ""

	var oldHostVethName string
	err := netns.Do(func(hostNS ns.NetNS) error {
		// create the veth pair in the container and move host end into host netns
		// set host interface name now for default network as it is already known; otherwise for secondary network,
		// host interface will be renamed later.
		if ifInfo.NetName == types.DefaultNetworkName {
			hostIface.Name = containerID[:15]
		} else {
			hostIface.Name = ""
		}
		contIface.Mac = ifInfo.MAC.String()
		hostVeth, containerVeth, err := cniPluginLibOps.SetupVeth(ifName, hostIface.Name, ifInfo.MTU, contIface.Mac, hostNS)
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
		contIface.Sandbox = netns.Path()

		if ifInfo.EnableUDPAggregation {
			err = setupVethUDPAggregationContainer(contIface.Name)
			if err != nil {
				return fmt.Errorf("could not enable UDP packet aggregation in container: %v", err)
			}
		}

		oldHostVethName = hostVeth.Name

		// to generate the unique host interface name, postfix it with the podInterface index for non-default network
		if ifInfo.NetName != types.DefaultNetworkName {
			ifnameSuffix = fmt.Sprintf("_%d", containerVeth.Index)
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	if ifInfo.Ingress > 0 || ifInfo.Egress > 0 {
		l, err := netlink.LinkByName(hostIface.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to find host veth interface %s: %v", hostIface.Name, err)
		}
		if err := netlink.LinkSetTxQLen(l, 1000); err != nil {
			return nil, nil, fmt.Errorf("failed to set host veth txqlen: %v", err)
		}
	}

	// rename the host end of veth pair for the secondary network
	if ifInfo.NetName != types.DefaultNetworkName {
		hostIface.Name = containerID[:(15-len(ifnameSuffix))] + ifnameSuffix
		if err := renameLink(oldHostVethName, hostIface.Name); err != nil {
			return nil, nil, fmt.Errorf("failed to rename %s to %s: %v", oldHostVethName, hostIface.Name, err)
		}
	}

	if ifInfo.EnableUDPAggregation {
		err = setupVethUDPAggregationHost(hostIface.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("could not enable UDP packet aggregation on host veth interface %q: %v", hostIface.Name, err)
		}
	}

	return hostIface, contIface, nil
}

// Setup sriov interface in the pod
func setupSriovInterface(vsClient client.Client, netns ns.NetNS, containerID,
	ifName string, ifInfo *PodInterfaceInfo, deviceID string) (*current.Interface, *current.Interface, error) {
	hostIface := &current.Interface{}
	contIface := &current.Interface{}
	ifnameSuffix := ""

	netdevice := ifInfo.NetdevName

	// 1. Move netdevice to Container namespace
	err := moveIfToNetns(netdevice, netns)
	if err != nil {
		return nil, nil, err
	}

	err = netns.Do(func(hostNS ns.NetNS) error {
		contIface.Name = ifName
		err = renameLink(netdevice, contIface.Name)
		if err != nil {
			return err
		}
		link, err := util.GetNetLinkOps().LinkByName(contIface.Name)
		if err != nil {
			return err
		}
		err = util.GetNetLinkOps().LinkSetHardwareAddr(link, ifInfo.MAC)
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

		// to generate the unique host interface name, postfix it with the podInterface index for non-default network
		if ifInfo.NetName != types.DefaultNetworkName {
			ifnameSuffix = fmt.Sprintf("_%d", link.Attrs().Index)
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	if !ifInfo.IsDPUHostMode {
		// 2. get device representor name
		oldHostRepName, err := util.GetFunctionRepresentorName(deviceID)
		if err != nil {
			return nil, nil, err
		}

		// 3. make sure it's not a port managed by OVS to avoid conflicts when renaming the representor
		if err := libovsdbops.DeletePort(vsClient, "br-int", oldHostRepName); err != nil {
			return nil, nil, err
		}

		// 4. rename the host representor
		hostIface.Name = containerID[:(15-len(ifnameSuffix))] + ifnameSuffix
		if err = renameLink(oldHostRepName, hostIface.Name); err != nil {
			return nil, nil, fmt.Errorf("failed to rename %s to %s: %v", oldHostRepName, hostIface.Name, err)
		}

		link, err := util.GetNetLinkOps().LinkByName(hostIface.Name)
		if err != nil {
			return nil, nil, err
		}
		hostIface.Mac = link.Attrs().HardwareAddr.String()

		// 5. set MTU on the representor
		if err = util.GetNetLinkOps().LinkSetMTU(link, ifInfo.MTU); err != nil {
			return nil, nil, fmt.Errorf("failed to set MTU on %s: %v", hostIface.Name, err)
		}
	}

	return hostIface, contIface, nil
}

// ConfigureOVS performs OVS configurations in order to set up Pod networking
func ConfigureOVS(vsClient client.Client, ctx context.Context, namespace, podName, hostIfaceName string,
	ifInfo *PodInterfaceInfo, sandboxID string, getter PodInfoGetter) error {

	ifaceID := util.GetIfaceId(namespace, podName)
	if ifInfo.NetName != types.DefaultNetworkName {
		ifaceID = util.GetSecondaryNetworkIfaceId(namespace, podName, ifInfo.NADName)
	}
	initialPodUID := ifInfo.PodUID
	ipStrs := make([]string, len(ifInfo.IPs))
	for i, ip := range ifInfo.IPs {
		ipStrs[i] = ip.String()
	}

	klog.Infof("ConfigureOVS: namespace: %s, podName: %s, network: %s, NAD %s, SandboxID: %q, UID: %q, MAC: %s, IPs: %v",
		namespace, podName, ifInfo.NetName, ifInfo.NADName, sandboxID, initialPodUID, ifInfo.MAC, ipStrs)

	// Find and remove any existing OVS port with this iface-id. Pods can
	// have multiple sandboxes if some are waiting for garbage collection,
	// but only the latest one should have the iface-id set.
	p := func(iface *vswitchdb.Interface) bool {
		if iface.Name == hostIfaceName {
			// this may be result of restarting ovnkube-node, and it is trying to add the same VF representor to
			// br-int for the same pod; do not delete port in this case.
			return false
		}
		foundID, ok := iface.ExternalIDs["iface-id"]
		return ok && foundID == ifaceID
	}
	if err := libovsdbops.DeleteInterfacesWithPredicate(vsClient, p); err != nil {
		klog.Warningf("Failed to delete stale OVS ports with iface-id %q from br-int: %v", ifaceID, err)
	}

	// if the specified interface was created for other Pod/NAD, return error
	found, _ := libovsdbops.FindInterfacesWithPredicate(vsClient, func(iface *vswitchdb.Interface) bool {
		name, ok := iface.ExternalIDs["name"]
		return ok && name == hostIfaceName
	})
	for _, f := range found {
		if f.ExternalIDs["iface-id"] != ifaceID {
			return fmt.Errorf("OVS port %s was added for iface-id (%s), now readding it for (%s)", hostIfaceName, f.ExternalIDs["iface-id"], ifaceID)
		}
		// if NADExternalID does not exists, it is default network
		nadNameString, ok := f.ExternalIDs[types.NADExternalID]
		if !ok {
			nadNameString = types.DefaultNetworkName
		}
		if nadNameString != ifInfo.NADName {
			return fmt.Errorf("OVS port %s was added for NAD (%s), expect (%s)", hostIfaceName, nadNameString, ifInfo.NADName)
		}
	}

	if err := libovsdbops.ClearPortQoSBySandboxID(vsClient, sandboxID); err != nil {
		return err
	}

	// Add the new sandbox's OVS port, tag the port as transient so stale
	// pod ports are scrubbed on hard reboot
	iface := &vswitchdb.Interface{
		ExternalIDs: map[string]string{
			"attached_mac": ifInfo.MAC.String(),
			"iface-id":     ifaceID,
			"iface-id-ver": initialPodUID,
			"sandbox":      sandboxID,
		},
	}
	// IPAM is optional for secondary flatL2 networks; thus, the ifaces may not
	// have IP addresses.
	if len(ifInfo.IPs) > 0 {
		iface.ExternalIDs["ip_addresses"] = strings.Join(ipStrs, ",")
	}

	if len(ifInfo.NetdevName) != 0 {
		// NOTE: For SF representor same external_id is used due to https://github.com/ovn-org/ovn-kubernetes/pull/3054
		// Review this line when upgrade mechanism will be implemented
		iface.ExternalIDs["vf-netdev-name"] = ifInfo.NetdevName
	}

	if ifInfo.NetName != types.DefaultNetworkName {
		iface.ExternalIDs[types.NetworkExternalID] = ifInfo.NetName
		iface.ExternalIDs[types.NADExternalID] = ifInfo.NADName
	}

	if ifInfo.Egress > 0 {
		// Note: bandwidth is configured from the OVS perspective, so
		// pod Egress == OVS ingress

		// ingress_policing_rate is in Kbps
		egressKBPS := int(ifInfo.Egress / 1000)
		iface.IngressPolicingRate = egressKBPS
		// Set the ingress_policing_burst too per recommendation in ovsdb schema, i.e
		// 10% of the rate
		iface.IngressPolicingBurst = egressKBPS / 10
	}

	port := &vswitchdb.Port{
		Name: hostIfaceName,
		OtherConfig: map[string]string{
			"transient": "true",
		},
	}

	if ifInfo.Ingress > 0 {
		// Note: bandwidth is configured from the OVS perspective, so
		// pod Ingress == OVS egress
		qos, err := libovsdbops.CreateQoS(vsClient, sandboxID, ifInfo.Ingress)
		if err != nil {
			return fmt.Errorf("failed to create QoS: %v", err)
		}
		qosUUID := qos.UUID
		port.QOS = &qosUUID
	}

	bridge, err := libovsdbops.FindBridgeByName(vsClient, "br-int")
	if err != nil {
		return fmt.Errorf("failed to find bridge br-int: %v", err)
	}
	if err := libovsdbops.CreateOrUpdatePortAndAddToBridge(vsClient, bridge.UUID, port, iface); err != nil {
		return fmt.Errorf("failed to create interface and port and add to bridge: %v", err)
	}

	if err := waitForPodInterface(vsClient, ctx, ifInfo, hostIfaceName, ifaceID, getter,
		namespace, podName, initialPodUID); err != nil {
		// Ensure the error shows up in node logs, rather than just
		// being reported back to the runtime.
		klog.Warningf("[%s/%s %s] pod uid %s: %v", namespace, podName, sandboxID, initialPodUID, err)
		return err
	}
	return nil
}

// ConfigureInterface sets up the container interface
func (pr *PodRequest) ConfigureInterface(getter PodInfoGetter, ifInfo *PodInterfaceInfo) ([]*current.Interface, error) {
	netns, err := ns.GetNS(pr.Netns)
	if err != nil {
		return nil, fmt.Errorf("failed to open netns %q: %v", pr.Netns, err)
	}
	defer netns.Close()

	var hostIface, contIface *current.Interface

	klog.V(5).Infof("CNI Conf %v", pr.CNIConf)
	if pr.CNIConf.DeviceID != "" {
		// SR-IOV Case
		hostIface, contIface, err = setupSriovInterface(pr.vsClient, netns, pr.SandboxID, pr.IfName, ifInfo, pr.CNIConf.DeviceID)
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
		err = ConfigureOVS(pr.vsClient, pr.ctx, pr.PodNamespace, pr.PodName, hostIface.Name, ifInfo, pr.SandboxID, getter)
		if err != nil {
			pr.deletePorts(pr.vsClient, hostIface.Name, pr.PodNamespace, pr.PodName)
			return nil, err
		}
	}

	// Only configure IPv6 specific stuff and wait for addresses to become usable
	// if there are any IPv6 addresses to assign. v4 doesn't have the concept
	// of tentative addresses so it doesn't need any of this.
	haveV6 := false
	for _, ip := range ifInfo.IPs {
		if ip.IP.To4() == nil {
			haveV6 = true
			break
		}
	}
	if haveV6 {
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
	}

	return []*current.Interface{hostIface, contIface}, nil
}

func (pr *PodRequest) UnconfigureInterface(ifInfo *PodInterfaceInfo) error {
	podDesc := fmt.Sprintf("for pod %s/%s NAD %s", pr.PodNamespace, pr.PodName, pr.nadName)
	klog.V(5).Infof("Tear down interface (%+v) %s", *pr, podDesc)
	if pr.CNIConf.DeviceID == "" && ifInfo.IsDPUHostMode {
		klog.Warningf("Unexpected configuration %s, Device ID must be present for pod request on smart-nic host", podDesc)
		return nil
	}
	// 1. For SRIOV case, we'd need to move the netdevice from container namespace back to the host namespace
	// 2. If it is secondary network and non-dpu mode, then get the container interface index
	//    so that we know the host-side interface name.
	ifnameSuffix := ""
	isSecondary := pr.netName != types.DefaultNetworkName
	if pr.CNIConf.DeviceID != "" || (isSecondary && !ifInfo.IsDPUHostMode) {
		netns, err := ns.GetNS(pr.Netns)
		if err != nil {
			return fmt.Errorf("failed to get container namespace %s: %v", podDesc, err)
		}
		defer netns.Close()

		hostNS, err := ns.GetCurrentNS()
		if err != nil {
			return fmt.Errorf("failed to get host namespace %s: %v", podDesc, err)
		}
		defer hostNS.Close()

		err = netns.Do(func(_ ns.NetNS) error {
			// container side interface deletion
			link, err := util.GetNetLinkOps().LinkByName(pr.IfName)
			if err != nil {
				return fmt.Errorf("failed to get container interface %s %s: %v", pr.IfName, podDesc, err)
			}
			if pr.CNIConf.DeviceID != "" {
				// SR-IOV Case
				err = util.GetNetLinkOps().LinkSetDown(link)
				if err != nil {
					return fmt.Errorf("failed to bring down container interface %s %s: %v", pr.IfName, podDesc, err)
				}
				// rename netdevice to make sure it is unique in the host namespace:
				// if original name of netdevice is empty, sandbox id and a '0' letter prefix is used to make up the unique name.
				oldName := ifInfo.NetdevName
				if oldName == "" {
					id := fmt.Sprintf("_0%d", link.Attrs().Index)
					oldName = pr.SandboxID[:(15-len(id))] + id
				}
				err = util.GetNetLinkOps().LinkSetName(link, oldName)
				if err != nil {
					return fmt.Errorf("failed to rename container interface %s to %s %s: %v",
						pr.IfName, oldName, podDesc, err)
				}
				// move netdevice to host netns
				err = util.GetNetLinkOps().LinkSetNsFd(link, int(hostNS.Fd()))
				if err != nil {
					return fmt.Errorf("failed to move container interface %s back to host namespace %s: %v",
						pr.IfName, podDesc, err)
				}
			}
			if isSecondary && !ifInfo.IsDPUHostMode {
				ifnameSuffix = fmt.Sprintf("_%d", link.Attrs().Index)
			}
			return nil
		})
		if err != nil {
			klog.Errorf(err.Error())
		}
	}

	if ifInfo.IsDPUHostMode {
		// there is nothing else to do in the DPU-Host mode
		return nil
	}

	// host side deletion of OVS port and kernel interface
	ifName := pr.SandboxID[:(15-len(ifnameSuffix))] + ifnameSuffix
	pr.deletePorts(pr.vsClient, ifName, pr.PodNamespace, pr.PodName)

	if err := libovsdbops.ClearPortQoSBySandboxID(pr.vsClient, pr.SandboxID); err != nil {
		klog.Warningf("Failed to clearPodBandwidth sandbox %v %s: %v", pr.SandboxID, podDesc, err)
	}
	pr.deletePodConntrack()
	return nil
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
		err = util.DeleteConntrack(ip.Address.IP.String(), 0, "", netlink.ConntrackReplyAnyIP, nil)
		if err != nil {
			klog.Errorf("Failed to delete Conntrack Entry for %s: %v", ip.Address.IP.String(), err)
			continue
		}
	}
}

func (pr *PodRequest) deletePorts(vsClient client.Client, ifaceName, podNamespace, podName string) {
	podDesc := fmt.Sprintf("%s/%s", podNamespace, podName)

	if err := libovsdbops.DeletePort(vsClient, "br-int", ifaceName); err != nil {
		// DEL should be idempotent; don't return an error just log it
		klog.Warningf("Failed to delete pod %q OVS port %s: %v", podDesc, ifaceName, err)
	}
	// skip deleting representor ports
	if pr.CNIConf.DeviceID == "" {
		if err := util.LinkDelete(ifaceName); err != nil {
			klog.Warningf("Failed to delete pod %q interface %s: %v", podDesc, ifaceName, err)
		}
	}
}

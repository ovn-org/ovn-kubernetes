// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package cni

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/safchain/ethtool"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func addRoute(ipn *net.IPNet, gw net.IP, dev netlink.Link, mtu, table int) error {
	return util.GetNetLinkOps().RouteAdd(&netlink.Route{
		LinkIndex: dev.Attrs().Index,
		Scope:     netlink.SCOPE_UNIVERSE,
		Dst:       ipn,
		Gw:        gw,
		MTU:       mtu,
		Table:     table,
	})
}

func replaceRouteECMP(ipn *net.IPNet, gw net.IP, devs []netlink.Link, mtu int) error {
	ecmpRoute := &netlink.Route{
		Dst: ipn,
		MTU: mtu,
	}
	ecmpRoute.MultiPath = make([]*netlink.NexthopInfo, len(devs))
	for i, dev := range devs {
		ecmpRoute.MultiPath[i] = &netlink.NexthopInfo{
			LinkIndex: dev.Attrs().Index,
			Gw:        gw,
			Hops:      0, // Weight (0 means weight of 1)
		}
	}
	return util.GetNetLinkOps().RouteReplace(ecmpRoute)
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

// safely move the netdev to the pod namespace, making sure to avoid name conflicts
func safeMoveIfToNetns(ifname string, netns ns.NetNS, containerID string) (newNetdeviceName string, err error) {
	newNetdeviceName = ifname
	err = moveIfToNetns(ifname, netns)

	if err != nil {
		if strings.Contains(err.Error(), "file exists") {
			// netdev with the same name exists in the pod
			newNetdeviceName = generateIfName(containerID)
			err = renameLink(ifname, newNetdeviceName)
			if err != nil {
				return ifname, err
			}
			err = moveIfToNetns(newNetdeviceName, netns)
			if err != nil {
				return ifname, err
			}
		} else {
			return ifname, err
		}
	}
	return newNetdeviceName, nil
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
		if err := addRoute(nil, gw, link, ifInfo.RoutableMTU, 0); err != nil {
			return fmt.Errorf("failed to add gateway route to link '%s': %v", link.Attrs().Name, err)
		}
	}

	// all pod links of the same secondary UDN up to the current one
	var links []netlink.Link
	var iptableNum int

	if len(ifInfo.PodIfNamesOfSameNAD) != 0 {
		// this is a pod with multiple interfaces of the same non-primary UDN, we need to configure the following sysctl configuration
		// stop ARP flux and stop RP filter drop from happening
		//   sysctl -w net.ipv4.conf.<linkName>.arp_ignore=1 # only reply to ARP requests if its sent to the IP on this interface
		//   sysctl -w net.ipv4.conf.<linkName>.arp_announce=2 # when sending ARP use the IP that belongs to this interface
		//   sysctl -w net.ipv4.conf.<linkName>.rp_filter=2 # allow reverse path filter check to pass if there is any route to the source
		linkName := link.Attrs().Name
		if err := setSysctl(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/arp_ignore", linkName), 1); err != nil {
			return fmt.Errorf("failed to set arp_ignore for %s: %v", linkName, err)
		}
		if err := setSysctl(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/arp_announce", linkName), 2); err != nil {
			return fmt.Errorf("failed to set arp_announce for %s: %v", linkName, err)
		}
		if err := setSysctl(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", linkName), 2); err != nil {
			return fmt.Errorf("failed to set rp_filter for %s: %v", linkName, err)
		}

		if len(ifInfo.Routes) != 0 {
			// needs ip table for each Pod interface of the same secondary UDN, table number start from 100
			iptableNum = 100 + link.Attrs().Index

			// ECMP routes are needed
			// get all the Pod interface names of the same nadName
			// up to the current link name. This is used for configuring ECMP routes via multiple pod interfaces
			links = make([]netlink.Link, 0, len(ifInfo.PodIfNamesOfSameNAD))
			for _, ifName := range ifInfo.PodIfNamesOfSameNAD {
				if ifName == link.Attrs().Name {
					// loop all pod interfaces of the same secondary UDN until the current pod interface
					links = append(links, link)
					break
				}
				ifLink, err := util.GetNetLinkOps().LinkByName(ifName)
				if err != nil {
					return fmt.Errorf("failed to lookup pod interface %s when setup route for link %s: %v", ifName, link.Attrs().Name, err)
				}
				links = append(links, ifLink)
			}

			// ip rules are needed to force traffic from an ip to egress the correct interface via a route table for that interface:
			//   ip rule add from <IP_on_this_interface> table <iptableNum>
			for _, ip := range ifInfo.IPs {
				rule := netlink.NewRule()
				rule.Src = util.GetIPNetFullMaskFromIP(ip.IP)
				rule.Table = iptableNum
				if err := util.GetNetLinkOps().RuleAdd(rule); err != nil {
					return fmt.Errorf("failed to add IP rule for %s to table %d: %v", ip.IP, iptableNum, err)
				}

				// add scope link route to the specific table
				route := &netlink.Route{
					LinkIndex: link.Attrs().Index,
					Scope:     netlink.SCOPE_LINK,
					Dst:       util.IPsToNetworkIPs(ip)[0],
					Table:     iptableNum,
				}
				if err := util.GetNetLinkOps().RouteAdd(route); err != nil {
					return fmt.Errorf("failed to add link scope route to %v via %v to table %v: %v", ip, linkName, iptableNum, err)
				}
			}
		}
	}

	for _, route := range ifInfo.Routes {
		if len(ifInfo.PodIfNamesOfSameNAD) == 0 {
			// if there is no other interface of the same NAD, just add route directly
			if err := addRoute(route.Dest, route.NextHop, link, ifInfo.RoutableMTU, 0); err != nil {
				return fmt.Errorf("failed to add pod route %v via %v: %v", route.Dest, route.NextHop, err)
			}
		} else {
			if len(links) == 1 {
				// if this is the first pod interface of this NAD, just add route directly
				if err := addRoute(route.Dest, route.NextHop, link, ifInfo.RoutableMTU, 0); err != nil {
					return fmt.Errorf("failed to add pod route %v via %v: %v", route.Dest, route.NextHop, err)
				}
			} else {
				// otherwise replace it with ECMP routes
				if err := replaceRouteECMP(route.Dest, route.NextHop, links, ifInfo.RoutableMTU); err != nil {
					return fmt.Errorf("failed to replace pod route %v via %v through links %v: %v", route.Dest, route.NextHop, links, err)
				}
			}

			// add ECMP route to specific IP route table
			if err := addRoute(route.Dest, route.NextHop, link, ifInfo.RoutableMTU, iptableNum); err != nil {
				return fmt.Errorf("failed to add pod route %v nexthop %v via %v table %v: %v", route.Dest, route.NextHop, link.Attrs().Name, iptableNum, err)
			}
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
		hostVeth, containerVeth, err := ip.SetupVethWithName(ifName, hostIface.Name, ifInfo.MTU, contIface.Mac, hostNS)
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

		// If we have the ipv6 gateway LLA then this is a primary layer2 UDN
		if len(ifInfo.GatewayIPv6LLA) > 0 {
			if err = setupIngressFilter(ifName, ifInfo.GatewayIPv6LLA.String()); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
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

// generate a unique interface name for the temporary netdev that will be moved to pod namespace
func generateIfName(containerID string) string {
	randomId := util.GenerateId(5) // random ID with 5 chars
	// ifname max length is 15
	return containerID[:(15-len(randomId))] + randomId
}

// Setup sriov interface in the pod
func setupSriovInterface(netns ns.NetNS, containerID, ifName string, ifInfo *PodInterfaceInfo, deviceID string, isVFIO bool) (*current.Interface, *current.Interface, error) {
	hostIface := &current.Interface{}
	contIface := &current.Interface{}
	netdevice := ifInfo.NetdevName

	// 0. init contIface for VFIO
	if isVFIO {
		if util.IsAuxDeviceName(deviceID) {
			return nil, nil, fmt.Errorf("VFIO not supported for device %s", deviceID)
		}
		// if the SR-IOV device is bound to VFIO, then there is nothing
		// to do as it will be passed to the KVM VM directly
		contIface.Name = ifName
		contIface.Mac = ifInfo.MAC.String()
		contIface.Sandbox = netns.Path()
	} else {
		// 1. Move netdevice to Container namespace
		if len(netdevice) != 0 {
			// Record the original netdevice name as the link alias before touching
			// the device. It shows which VF backs the renamed pod interface, and
			// CNI DEL renames the VF back from it when the pod annotations are
			// already gone. Only simulated devices depend on it: they are veths
			// identified by name alone, so without the alias they could not be
			// returned to the host (see returnSimulatedNetdevsToHost). Physical
			// VFs are found again through their PCI address whatever their name.
			hostLink, err := util.GetNetLinkOps().LinkByName(netdevice)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to lookup device %s: %v", netdevice, err)
			}
			if err := util.GetNetLinkOps().LinkSetAlias(hostLink, netdevice); err != nil {
				return nil, nil, fmt.Errorf("failed to set alias on device %s: %v", netdevice, err)
			}
			newNetdevName, err := safeMoveIfToNetns(netdevice, netns, containerID)
			if err != nil {
				return nil, nil, err
			}
			err = netns.Do(func(_ ns.NetNS) error {
				contIface.Name = ifName
				err = renameLink(newNetdevName, contIface.Name)
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

				return nil
			})
			if err != nil {
				return nil, nil, err
			}
		}
	}

	if !ifInfo.IsDPUHostMode {
		// 2. get device representor name
		hostRepName, err := util.GetFunctionRepresentorName(deviceID)
		if err != nil {
			return nil, nil, err
		}

		// 3. it's not possible to set mac address within container netns for VFIO case,
		// hence set it through VF representor (PCI device IDs only).
		if util.IsPCIDeviceName(deviceID) {
			if err := util.SetVFHardwreAddress(deviceID, ifInfo.MAC); err != nil {
				return nil, nil, err
			}
		}

		hostIface.Name = hostRepName
		link, err := util.GetNetLinkOps().LinkByName(hostIface.Name)
		if err != nil {
			return nil, nil, err
		}

		hostIface.Mac = link.Attrs().HardwareAddr.String()
		// Do not bring the representor up or set MTU here. In full mode the representor must be
		// added to br-int first, then brought up (and set MTU) in ConfigureOVS after the port
		// is added to br-int. This avoids a race where an old pod's CmdDel could bring down the
		// same VF representor and remove it from br-int after we brought the link up but before
		// we added it to br-int.
	}

	return hostIface, contIface, nil
}

func getPfEncapIP(ovsClient client.Client, deviceID string) (string, error) {
	var mapping string
	if ovsClient != nil {
		ovs, err := ovsops.GetOpenvSwitch(ovsClient)
		if err != nil {
			return "", fmt.Errorf("failed to get Open_vSwitch row: %v", err)
		}
		mapping = ovs.ExternalIDs["ovn-pf-encap-ip-mapping"]
	} else {
		stdout, err := ovsGet("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping")
		if err != nil {
			return "", fmt.Errorf("failed to get ovn-pf-encap-ip-mapping, error: %v", err)
		}
		mapping = stdout
	}
	return parsePfEncapIPMapping(mapping, deviceID)
}

func parsePfEncapIPMapping(mapping, deviceID string) (string, error) {
	if mapping == "" {
		return "", nil
	}
	encapIpMapping := map[string]string{}
	for _, entry := range strings.Split(mapping, ",") {
		tokens := strings.Split(entry, ":")
		if len(tokens) != 2 {
			return "", fmt.Errorf("bad ovn-pf-encap-ip-mapping config: %s", mapping)
		}
		encapIpMapping[tokens[0]] = tokens[1]
	}
	uplinkRepName, err := util.GetUplinkRepresentorName(deviceID)
	if err != nil {
		// FIXME(leih): unlikely to happen, treat this as a valid case and ignore for now.
		klog.Errorf("Failed to get uplink representor for VF PCI address %s: %v",
			deviceID, err)
		return "", nil
	}
	return encapIpMapping[uplinkRepName], nil
}

// getBrIntDatapathType returns the datapath type of br-int. Uses libovsdb when
// an OVS client is available, otherwise falls back to ovs-vsctl.
func getBrIntDatapathType(ovsClient client.Client) (string, error) {
	if ovsClient != nil {
		br, err := ovsops.GetBridge(ovsClient, "br-int")
		if err != nil {
			return "", fmt.Errorf("failed to get bridge br-int: %v", err)
		}
		return br.DatapathType, nil
	}
	return getDatapathType("br-int")
}

// deleteStalePodPorts removes any existing OVS ports on br-int whose
// Interface external_ids[iface-id] == ifaceID, except hostIfaceName itself
// (which may be a legitimate re-add by a restarted ovnkube-node).
func deleteStalePodPorts(ovsClient client.Client, ifaceID, hostIfaceName string) error {
	if ovsClient != nil {
		stale, err := ovsops.FindInterfacesWithPredicate(ovsClient, func(i *vswitchd.Interface) bool {
			return i.ExternalIDs["iface-id"] == ifaceID
		})
		if err != nil {
			return fmt.Errorf("failed to list interfaces with iface-id %s: %v", ifaceID, err)
		}
		for _, s := range stale {
			if s.Name == hostIfaceName {
				continue
			}
			if err := ovsops.DeletePortWithInterfaces(ovsClient, "br-int", s.Name); err != nil {
				klog.Warningf("Failed to delete stale OVS port %q with iface-id %q from br-int: %v",
					s.Name, ifaceID, err)
			}
		}
		return nil
	}
	names, _ := ovsFind("Interface", "name", "external-ids:iface-id="+ifaceID)
	for _, name := range names {
		if name == hostIfaceName {
			continue
		}
		if out, err := ovsExec("--with-iface", "del-port", "br-int", name); err != nil {
			klog.Warningf("Failed to delete stale OVS port %q with iface-id %q from br-int: %v\n %q",
				name, ifaceID, err, out)
		}
	}
	return nil
}

// getExistingIfaceMeta returns the iface-id and effective NAD key recorded in
// the named Interface's external_ids, or (_, _, false) if the interface does
// not exist. An absent NADExternalID is mapped to types.DefaultNetworkName so
// callers can compare directly against ifInfo.NADKey.
func getExistingIfaceMeta(ovsClient client.Client, name string) (string, string, bool, error) {
	if ovsClient != nil {
		existing, err := ovsops.GetOVSInterface(ovsClient, name)
		if err != nil {
			if errors.Is(err, client.ErrNotFound) {
				return "", "", false, nil
			}
			return "", "", false, err
		}
		nadKey := existing.ExternalIDs[types.NADExternalID]
		if nadKey == "" {
			nadKey = types.DefaultNetworkName
		}
		return existing.ExternalIDs["iface-id"], nadKey, true, nil
	}
	extIds, err := ovsFind("Interface", "external_ids", "name="+name)
	if err != nil || len(extIds) != 1 {
		return "", "", false, nil
	}
	nadKey := util.GetExternalIDValByKey(extIds[0], types.NADExternalID)
	if nadKey == "" {
		nadKey = types.DefaultNetworkName
	}
	return util.GetExternalIDValByKey(extIds[0], "iface-id"), nadKey, true, nil
}

// addOrUpdatePodPort creates or updates the OVS port and its single backing
// Interface on br-int for the given pod iface. extIDs becomes the Interface
// external_ids without removing keys owned by other components. On the default
// network, stripDefaultNetIDs=true explicitly removes NetworkExternalID and
// NADExternalID left by a previous owner. If useDPDK is true the Interface
// type is set to dpdk with mtu_request=mtu.
func addOrUpdatePodPort(ovsClient client.Client, hostIfaceName string,
	extIDs map[string]string, useDPDK bool, mtu int, stripDefaultNetIDs bool) error {
	if ovsClient != nil {
		iface := &vswitchd.Interface{ExternalIDs: extIDs}
		if useDPDK {
			m := mtu
			iface.Type = "dpdk"
			iface.MTURequest = &m
		}
		port := &vswitchd.Port{OtherConfig: map[string]string{"transient": "true"}}
		ops, err := ovsops.CreateOrUpdatePodPortOps(ovsClient, nil, "br-int", hostIfaceName, port, iface)
		if err != nil {
			return fmt.Errorf("failed to build operations for plugging pod interface: %v", err)
		}
		if stripDefaultNetIDs {
			ops, err = ovsops.RemoveOVSInterfaceExternalIDsOps(ovsClient, ops, hostIfaceName, types.NetworkExternalID, types.NADExternalID)
			if err != nil {
				return fmt.Errorf("failed to clear default-network external IDs from interface %s: %v", hostIfaceName, err)
			}
		}
		if _, err := ovsops.TransactAndCheck(ovsClient, ops); err != nil {
			return fmt.Errorf("failure in plugging pod interface: %v", err)
		}
		return nil
	}
	args := []string{
		"--may-exist", "add-port", "br-int", hostIfaceName, "other_config:transient=true",
		"--", "set", "interface", hostIfaceName,
	}
	// Walk extIDs in the canonical ovs-vsctl arg order that this CNI has
	// historically used, so the shell call is stable across runs and matches
	// any test expectations.
	for _, k := range []string{
		"attached_mac",
		"iface-id",
		"iface-id-ver",
		"sandbox",
		"pod-if-name",
		"encap-ip",
		"ip_addresses",
		"vf-netdev-name",
		"vf-is-vfio",
		types.NetworkExternalID,
		types.NADExternalID,
	} {
		if v, ok := extIDs[k]; ok {
			args = append(args, fmt.Sprintf("external_ids:%s=%s", k, v))
		}
	}
	if useDPDK {
		args = append(args, "type=dpdk", fmt.Sprintf("mtu_request=%v", mtu))
	}
	if stripDefaultNetIDs {
		args = append(args, "--", "--if-exists", "remove", "interface", hostIfaceName, "external_ids", types.NetworkExternalID)
		args = append(args, "--", "--if-exists", "remove", "interface", hostIfaceName, "external_ids", types.NADExternalID)
	}
	if out, err := ovsExec(args...); err != nil {
		return fmt.Errorf("failure in plugging pod interface: %v\n  %q", err, out)
	}
	return nil
}

// ConfigureOVS performs OVS configurations in order to set up Pod networking.
// When ovsClient is non-nil (ovnkube-node privileged-mode path) it uses
// libovsdb; when nil (unprivileged CNI shim path) it falls back to ovs-vsctl
// shell-outs.
func ConfigureOVS(ctx context.Context, ovsClient client.Client, namespace, podName, podIfName, hostIfaceName string,
	ifInfo *PodInterfaceInfo, sandboxID, deviceID string, isVFIO bool, getter PodInfoGetter) error {

	ifaceID := util.GetIfaceId(namespace, podName)
	if ifInfo.NetName != types.DefaultNetworkName {
		ifaceID = util.GetUDNIfaceId(namespace, podName, ifInfo.NADKey)
	}
	initialPodUID := ifInfo.PodUID
	ipStrs := make([]string, len(ifInfo.IPs))
	for i, ip := range ifInfo.IPs {
		ipStrs[i] = ip.String()
	}

	dpType, err := getBrIntDatapathType(ovsClient)
	if err != nil {
		return err
	}

	klog.Infof("ConfigureOVS: namespace: %s, podName: %s, hostIfaceName: %s, network: %s, NAD %s, SandboxID: %q, PCI device ID: %s, UID: %q, MAC: %s, IPs: %v",
		namespace, podName, hostIfaceName, ifInfo.NetName, ifInfo.NADKey, sandboxID, deviceID, initialPodUID, ifInfo.MAC, ipStrs)

	// Find and remove any existing OVS port with this iface-id. Pods can
	// have multiple sandboxes if some are waiting for garbage collection,
	// but only the latest one should have the iface-id set.
	if err := deleteStalePodPorts(ovsClient, ifaceID, hostIfaceName); err != nil {
		return err
	}

	// if the specified port was created for other Pod/NAD, return error
	if existingIfaceID, existingNADKey, exists, err := getExistingIfaceMeta(ovsClient, hostIfaceName); err != nil {
		return err
	} else if exists {
		if existingIfaceID != ifaceID {
			return fmt.Errorf("OVS port %s was added for iface-id (%s), now readding it for (%s)", hostIfaceName, existingIfaceID, ifaceID)
		}
		if existingNADKey != ifInfo.NADKey {
			return fmt.Errorf("OVS port %s was added for NAD (%s), expect (%s)", hostIfaceName, existingNADKey, ifInfo.NADKey)
		}
	}

	// Add the new sandbox's OVS port, tag the port as transient so stale
	// pod ports are scrubbed on hard reboot
	extIDs := map[string]string{
		"attached_mac": ifInfo.MAC.String(),
		"iface-id":     ifaceID,
		"iface-id-ver": initialPodUID,
		"sandbox":      sandboxID,
	}

	// pod interface name, used to identify CNI request with the same NAD
	if podIfName != "" {
		extIDs["pod-if-name"] = podIfName
	}

	// In case of multi-vtep, host has multipe NICs and each NIC has a VTEP interface, the mapping
	// of VTEP IP to NIC is stored in Open_vSwitch table's `external_ids:ovn-pf-encap-ip-mapping`,
	// the value's format is:
	//   enp1s0f0:<vtep-ip1>,enp193s0f0:<vtep-ip2>,enp197s0f0:<vtep-ip3>
	// Here configure the OVS Interface's encap-ip according to the mapping.
	if deviceID != "" {
		encapIP, err := getPfEncapIP(ovsClient, deviceID)
		if err != nil {
			return err
		}
		if len(encapIP) > 0 {
			extIDs["encap-ip"] = encapIP
		}
	}

	// IPAM is optional for secondary flatL2 networks; thus, the ifaces may not
	// have IP addresses.
	if len(ifInfo.IPs) > 0 {
		extIDs["ip_addresses"] = strings.Join(ipStrs, ",")
	}

	useDPDK := false
	if dpType == types.DatapathUserspace {
		if _, err := util.GetSriovnetOps().GetRepresentorPortFlavour(hostIfaceName); err != nil {
			// The error is not important: the given port is not a switchdev one and won't
			// be used with DPDK. It can happen for legitimate reason. Keep a trace of the
			// event and continue configuring OVS.
			klog.Infof("Port %s cannot be used with DPDK, will use netlink interface in OVS",
				hostIfaceName)
		} else {
			useDPDK = true
		}
	}

	if len(ifInfo.NetdevName) != 0 {
		// NOTE: For SF representor same external_id is used due to https://github.com/ovn-kubernetes/ovn-kubernetes/pull/3054
		// Review this line when upgrade mechanism will be implemented
		extIDs["vf-netdev-name"] = ifInfo.NetdevName
	}
	if isVFIO {
		// VFIO case
		extIDs["vf-is-vfio"] = "true"
	}

	stripDefaultNetIDs := false
	if ifInfo.NetName != types.DefaultNetworkName {
		extIDs[types.NetworkExternalID] = ifInfo.NetName
		extIDs[types.NADExternalID] = ifInfo.NADKey
	} else {
		// On the default network NetworkExternalID/NADExternalID must not
		// be set; for the shell path this requires explicit removes.
		stripDefaultNetIDs = true
	}

	if err := addOrUpdatePodPort(ovsClient, hostIfaceName, extIDs, useDPDK, ifInfo.MTU, stripDefaultNetIDs); err != nil {
		return err
	}

	if err := clearPodBandwidth(ovsClient, sandboxID); err != nil {
		return err
	}

	var link netlink.Link
	if deviceID != "" || (ifInfo.Ingress > 0 || ifInfo.Egress > 0) {
		if link, err = util.GetNetLinkOps().LinkByName(hostIfaceName); err != nil {
			return fmt.Errorf("failed to find interface %s: %v", hostIfaceName, err)
		}
	}

	if deviceID != "" {
		// 4. set MTU on the representor
		if err = util.GetNetLinkOps().LinkSetMTU(link, ifInfo.MTU); err != nil {
			return fmt.Errorf("failed to set MTU on %s: %v", hostIfaceName, err)
		}
		// 5. if the interface is not up, set it to up
		err = util.GetNetLinkOps().LinkSetUp(link)
		if err != nil {
			return fmt.Errorf("failed to set link UP on %s: %v", hostIfaceName, err)
		}
	}

	if ifInfo.Ingress > 0 || ifInfo.Egress > 0 {
		err = netlink.LinkSetTxQLen(link, 1000)
		if err != nil {
			return fmt.Errorf("failed to set host veth txqlen: %v", err)
		}

		if err := setPodBandwidth(sandboxID, hostIfaceName, ifInfo.Ingress, ifInfo.Egress); err != nil {
			return err
		}
	}

	if err := waitForPodInterface(ctx, ovsClient, ifInfo, hostIfaceName, ifaceID, getter,
		namespace, podName, initialPodUID); err != nil {
		// Ensure the error shows up in node logs, rather than just
		// being reported back to the runtime.
		klog.Warningf("[%s/%s %s] pod uid %s: %v", namespace, podName, sandboxID, initialPodUID, err)
		return err
	}
	return nil
}

type PodRequestInterfaceOps interface {
	ConfigureInterface(pr *PodRequest, ovsClient client.Client, getter PodInfoGetter, ifInfo *PodInterfaceInfo) ([]*current.Interface, error)
	UnconfigureInterface(pr *PodRequest, ifInfo *PodInterfaceInfo, podLister corev1listers.PodLister, pod *corev1.Pod) error
}

type defaultPodRequestInterfaceOps struct{}

var podRequestInterfaceOps PodRequestInterfaceOps = &defaultPodRequestInterfaceOps{}

// ConfigureInterface sets up the container interface
func (*defaultPodRequestInterfaceOps) ConfigureInterface(pr *PodRequest, ovsClient client.Client, getter PodInfoGetter, ifInfo *PodInterfaceInfo) ([]*current.Interface, error) {
	netns, err := ns.GetNS(pr.Netns)
	if err != nil {
		return nil, fmt.Errorf("failed to open netns %q: %v", pr.Netns, err)
	}
	defer netns.Close()

	var hostIface, contIface *current.Interface

	klog.V(5).Infof("CNI Conf %v", pr.CNIConf)
	if pr.CNIConf.DeviceID != "" {
		// SR-IOV Case
		hostIface, contIface, err = setupSriovInterface(netns, pr.SandboxID, pr.IfName, ifInfo, pr.CNIConf.DeviceID, pr.IsVFIO)
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
		err = ConfigureOVS(pr.ctx, ovsClient, pr.PodNamespace, pr.PodName, pr.IfName, hostIface.Name, ifInfo, pr.SandboxID, pr.CNIConf.DeviceID, pr.IsVFIO, getter)
		if err != nil {
			pr.deletePort(hostIface.Name, pr.PodNamespace, pr.PodName)
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
	if haveV6 && !pr.IsVFIO {
		err = netns.Do(func(_ ns.NetNS) error {
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

			return ip.SettleAddresses(contIface.Name, 10*time.Second)
		})
		if err != nil {
			klog.Warningf("Failed to settle addresses: %q", err)
		}
	}

	return []*current.Interface{hostIface, contIface}, nil
}

// returnNetdevToHost renames a container netdevice back to oldName and moves
// it to the host network namespace. Must be called from within the container
// network namespace.
func returnNetdevToHost(link netlink.Link, oldName string, hostNS ns.NetNS) error {
	name := link.Attrs().Name
	if err := util.GetNetLinkOps().LinkSetDown(link); err != nil {
		return fmt.Errorf("failed to bring down container interface %s: %v", name, err)
	}
	if name != oldName {
		if err := util.GetNetLinkOps().LinkSetName(link, oldName); err != nil {
			return fmt.Errorf("failed to rename container interface %s to %s: %v", name, oldName, err)
		}
	}
	if err := util.GetNetLinkOps().LinkSetNsFd(link, int(hostNS.Fd())); err != nil {
		return fmt.Errorf("failed to move container interface %s back to host namespace: %v", oldName, err)
	}
	return nil
}

// returnSimulatedNetdevsToHost returns any simulated netdevices still present in the
// pod network namespace to the host during CNI DEL of the default network.
//
// The runtime only issues a DEL for the default network: the primary UDN
// attachment shares the sandbox and no separate DEL ever comes for it, so
// without this it would be destroyed together with the namespace. A simulated
// device is a veth: losing the pod-side end also destroys the DPU-side
// representor peer, permanently breaking any future pod assigned the same VF.
//
// A link is recognized as a simulated netdevice when its name is the primary
// UDN interface name, or its name or alias matches the simulated netdevice
// naming pattern. The restore name is taken from the link alias recorded at
// setup time, or from the link name itself when that already matches the
// pattern and no alias was recorded. A link whose original name cannot be
// determined is left in place, since the simulated allocator resolves devices
// strictly by name and could never find it again anyway. Renaming before the
// move keeps retries idempotent: a link already renamed by an interrupted run
// is recognized again through its name or alias.
func returnSimulatedNetdevsToHost(netns, hostNS ns.NetNS) error {
	return netns.Do(func(_ ns.NetNS) error {
		links, err := util.GetNetLinkOps().LinkList()
		if err != nil {
			return fmt.Errorf("failed to list container interfaces: %v", err)
		}
		var errs []error
		for _, link := range links {
			attrs := link.Attrs()
			if attrs == nil {
				continue
			}
			name, alias := attrs.Name, attrs.Alias
			isCandidate := name == primaryUDNIfName ||
				util.IsSimulatedNetdevName(name) ||
				util.IsSimulatedNetdevName(alias)
			if !isCandidate {
				continue
			}
			var oldName string
			switch {
			case util.IsSimulatedNetdevName(alias):
				oldName = alias
			case util.IsSimulatedNetdevName(name):
				oldName = name
			default:
				klog.Warningf("Cannot determine the original name of simulated netdevice %s, leaving it in the container namespace", name)
				continue
			}
			if err := returnNetdevToHost(link, oldName, hostNS); err != nil {
				errs = append(errs, err)
				continue
			}
			klog.Infof("Returned simulated netdevice %s to host namespace", oldName)
		}
		return errors.Join(errs...)
	})
}

func (*defaultPodRequestInterfaceOps) UnconfigureInterface(pr *PodRequest, ifInfo *PodInterfaceInfo, podLister corev1listers.PodLister, pod *corev1.Pod) error {
	podDesc := fmt.Sprintf("for pod %s/%s NAD %s", pr.PodNamespace, pr.PodName, pr.nadName)
	klog.V(5).Infof("Tear down interface (%+v) %s", *pr, podDesc)
	if ifInfo.IsDPUHostMode {
		if pr.CNIConf.DeviceID == "" {
			klog.Warningf("Unexpected configuration %s, pod request on DPU host. device ID must be provided", podDesc)
			return nil
		}
		// nothing else to do in DPUHostMode for VFIO device
		if pr.IsVFIO {
			return nil
		}
		// in the case of VF, we need to rename the container interface to VF name and move it to host
	}

	ifnameSuffix := ""
	isSecondary := pr.netName != types.DefaultNetworkName
	// nothing needs to be done for the VFIO case in the container namespace
	if !pr.IsVFIO {
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

		// 1. For SRIOV case, we'd need to move device from container namespace back to the host namespace
		// 2. If it is secondary network and not dpu-host mode, then get the container interface index
		//    so that we know the host-side interface name.
		err = netns.Do(func(_ ns.NetNS) error {
			// container side interface deletion
			link, err := util.GetNetLinkOps().LinkByName(pr.IfName)
			if err != nil {
				return fmt.Errorf("failed to get container interface %s %s: %v", pr.IfName, podDesc, err)
			}
			if pr.CNIConf.DeviceID != "" {
				// SR-IOV Case
				// rename the netdevice back to its original name to make sure
				// it is unique in the host namespace. If the original name is
				// unknown, fall back to the link alias recorded at setup time,
				// then to a unique placeholder derived from the sandbox id and
				// the link index.
				oldName := ifInfo.NetdevName
				if oldName == "" {
					oldName = link.Attrs().Alias
				}
				if oldName == "" {
					id := fmt.Sprintf("_0%d", link.Attrs().Index)
					oldName = pr.SandboxID[:(15-len(id))] + id
				}
				if err := returnNetdevToHost(link, oldName, hostNS); err != nil {
					return fmt.Errorf("%v %s", err, podDesc)
				}
			}
			if isSecondary {
				ifnameSuffix = fmt.Sprintf("_%d", link.Attrs().Index)
			}
			return nil
		})
		if err != nil {
			klog.Errorf("Error in UnconfigureInterface: %v", err)
		}

		if ifInfo.IsDPUHostMode && ifInfo.IsSimulatedDPU && !isSecondary {
			// Return any simulated netdevices left in the sandbox (the primary
			// UDN attachment in particular, which gets no DEL of its own) to
			// the host before the namespace is destroyed. Propagate errors so
			// that the runtime retries the DEL rather than proceeding with the
			// namespace teardown.
			if err := returnSimulatedNetdevsToHost(netns, hostNS); err != nil {
				return fmt.Errorf("failed to return simulated netdevices to host namespace %s: %v", podDesc, err)
			}
		}
	}

	if !ifInfo.IsDPUHostMode {
		var err error
		// host side interface deletion
		var hostIfName string
		if !util.IsNetworkSegmentationSupportEnabled() || isSecondary {
			// this is a secondary network (not primary) or segmentation is not enabled
			hostIfName = pr.SandboxID[:(15-len(ifnameSuffix))] + ifnameSuffix
		}
		if pr.CNIConf.DeviceID != "" {
			hostIfName, err = util.GetFunctionRepresentorName(pr.CNIConf.DeviceID)
			if err != nil {
				klog.Errorf("Failed to get the representor name for DeviceID %s for pod %s: %v",
					pr.CNIConf.DeviceID, podDesc, err)
			}
		}
		portList, err := ovsFind("interface", "name", "external-ids:sandbox="+pr.SandboxID)
		if err != nil {
			return fmt.Errorf("failed to list interfaces in OVS during delete for sandbox: %s, err: %w",
				pr.SandboxID, err)
		}
		// hostIfName is not empty if using device ID, a secondary network, or segmentation not enabled
		// delete the port in traditional fashion
		if hostIfName != "" {
			pr.deletePort(hostIfName, pr.PodNamespace, pr.PodName)
		} else {
			// this is a primary interface deletion and segmentation is enabled, delete all ports
			// delete happens in reverse order for attached networks, so this is the final deletion
			// In other words we dont have to worry about accidentally deleting a secondary network interface at
			// this point.
			if len(portList) > 1 {
				klog.V(5).Infof("Removing multiple interfaces for primary network segmentation (%+v) %s: %s",
					*pr, podDesc, strings.Join(portList, ","))
			}
			pr.deletePorts(portList, pr.PodNamespace, pr.PodName)
		}
		err = clearPodBandwidthForPorts(portList, pr.SandboxID)
		if err != nil {
			klog.Errorf("Failed to clearPodBandwidth sandbox %v %s: %v", pr.SandboxID, podDesc, err)
		}
		pr.deletePodConntrack(podLister, pod)
	}
	return nil
}

func (pr *PodRequest) deletePodConntrack(podLister corev1listers.PodLister, pod *corev1.Pod) {
	if pr.CNIConf.PrevResult == nil {
		return
	}
	result, err := current.NewResultFromResult(pr.CNIConf.PrevResult)
	if err != nil {
		klog.Warningf("Could not convert result to current version: %v", err)
		return
	}

	// During KubeVirt live migration the VM IPs are preserved and move to the
	// migration target pod running on another node. Flushing them on this
	// pod's CNI DEL would tear down established connections (e.g. the
	// north/south NodePort/SNAT ingress connection routed to the VM) that must
	// survive the migration. Compute, once per DEL, the set of IPs still owned
	// by another living pod of the VM and skip only those; genuinely released
	// IPs (e.g. the source pod's default-network IP) are still flushed as
	// before.
	preservedIPs := pr.migrationPreservedIPs(podLister, pod)

	for _, ip := range result.IPs {
		// Skip known non-sandbox interfaces
		if ip.Interface != nil {
			intIdx := *ip.Interface
			if intIdx >= 0 &&
				intIdx < len(result.Interfaces) && result.Interfaces[intIdx].Sandbox == "" {
				continue
			}
		}
		if preservedIPs.Has(ip.Address.IP.String()) {
			klog.V(5).Infof("Skipping conntrack flush for pod %s/%s IP %s: IP is preserved on another pod of the live-migrated VM",
				pr.PodNamespace, pr.PodName, ip.Address.IP.String())
			continue
		}
		_, err = util.DeleteConntrack(ip.Address.IP.String(), 0, "", netlink.ConntrackReplyAnyIP, nil)
		if err != nil {
			klog.Errorf("Failed to delete Conntrack Entry for %s: %v", ip.Address.IP.String(), err)
			continue
		}
	}
}

// migrationPreservedIPs returns the set of IP addresses that are preserved on
// another living pod of the same KubeVirt VM as the pod being torn down,
// because of a live migration: the migration target pod when the source is
// deleted, or the source pod when a failed migration target is deleted.
// Only the IPs actually preserved across the migration are returned:
// the cluster default network is skipped, since it allocates per-node subnets
// and its IPs move with the pod, not with the VM; genuinely released IPs keep
// being flushed.
//
// An empty set is returned (flush everything, the previous behavior) when:
//   - podLister or pod is nil (the unprivileged CNI shim has no apiserver
//     access, or the pod is already gone)
//   - the pod does not belong to a live-migratable VM
//   - there is no live migration, or no other living pod of the VM owns IPs
//
// Note: podLister requires cluster-wide pod visibility, since the surviving
// pod of a live migration always runs on a different node. The cniserver's
// podLister is backed by factory.PodInformer(), which watches pods
// on all nodes.
func (pr *PodRequest) migrationPreservedIPs(podLister corev1listers.PodLister, pod *corev1.Pod) sets.Set[string] {
	preservedIPs := sets.New[string]()
	if podLister == nil || pod == nil {
		return preservedIPs
	}
	// DiscoverLiveMigrationStatus already filters non-VM pods. Primary UDN
	// binding plugins can migrate without the legacy pod-bridge annotation.
	status, err := kubevirt.DiscoverLiveMigrationStatus(podLister, pod)
	if err != nil {
		klog.Warningf("Failed to discover live migration status for pod %s/%s: %v", pr.PodNamespace, pr.PodName, err)
		return preservedIPs
	}
	if status == nil {
		return preservedIPs
	}
	for _, vmPod := range []*corev1.Pod{status.SourcePod, status.TargetPod} {
		if vmPod == nil || vmPod.Name == pr.PodName || util.PodCompleted(vmPod) {
			continue
		}
		podNetworks, err := util.UnmarshalPodAnnotationAllNetworks(vmPod.Annotations)
		if err != nil {
			klog.Warningf("Failed to unmarshal OVN pod annotation of live migration pod %s/%s: %v",
				vmPod.Namespace, vmPod.Name, err)
			continue
		}
		for nadName, podNetwork := range podNetworks {
			// The cluster default network allocates per-node subnets, so its
			// IPs are never preserved across a live migration: the surviving
			// pod owns a different IP from its own node's subnet, and the
			// deleted pod's default-network IP is genuinely released.
			if nadName == types.DefaultNetworkName {
				continue
			}
			for _, ipStr := range podNetwork.IPs {
				ip, _, err := net.ParseCIDR(ipStr)
				if err != nil {
					klog.Warningf("Failed to parse IP %q from OVN pod annotation of live migration pod %s/%s: %v",
						ipStr, vmPod.Namespace, vmPod.Name, err)
					continue
				}
				preservedIPs.Insert(ip.String())
			}
		}
	}
	return preservedIPs
}

func (pr *PodRequest) deletePort(ifaceName, podNamespace, podName string) {
	podDesc := fmt.Sprintf("%s/%s", podNamespace, podName)

	var isVFDevice bool
	link, err := util.GetNetLinkOps().LinkByName(ifaceName)
	if err != nil {
		klog.Warningf("Failed to find host-side link %s for pod %q: %v", ifaceName, podDesc, err)
	} else if pr.CNIConf.DeviceID != "" {
		isVFDevice = true
	}

	if isVFDevice {
		// SR-IOV case: bring down the representor before removing from OVS.
		// The device plugin can re-allocate a VF to a new pod before this
		// CmdDel completes, causing the new pod's CmdAdd shim to run
		// concurrently. By doing LinkSetDown before del-port, we eliminate
		// the window where a racing CmdAdd could have its LinkSetUp or
		// ConfigureOVS undone.
		if err = util.GetNetLinkOps().LinkSetDown(link); err != nil {
			klog.Warningf("Failed to bring down pod %q interface %s: %v", podDesc, ifaceName, err)
		}
	}

	out, err := ovsExec("del-port", "br-int", ifaceName)
	if err != nil && !strings.Contains(err.Error(), "no port named") {
		// DEL should be idempotent; don't return an error just log it
		klog.Warningf("Failed to delete pod %q OVS port %s: %v\n  %q", podDesc, ifaceName, err, string(out))
	}

	// skip deleting representor ports
	if link != nil && !isVFDevice {
		if err = util.LinkDelete(ifaceName); err != nil {
			klog.Warningf("Failed to delete pod %q interface %s: %v", podDesc, ifaceName, err)
		}
	}
}

func (pr *PodRequest) deletePorts(ifaces []string, podNamespace, podName string) {
	for _, iface := range ifaces {
		pr.deletePort(iface, podNamespace, podName)
	}
}

// setupIngressFilter sets up an ingress filter using nftables to block
// unwanted ICMPv6 Router Advertisement (RA) packets on a specific device.
// It creates a new nftables table, chain, and rule to drop RA packets
// that do not match the specified gateway link-local address (gwLLA) and
// have a Router Advertisement (RA) lifetime not equal to 0.
//
// The nftables rule created by this function looks like:
// `icmpv6 type nd-router-advert ip6 saddr != <gwLLA> @th,48,16 != 0 drop`
//
// Parameters:
//   - ifaceName: The name of the network interface where the ingress filter
//     should be applied.
//   - gwLLA: The gateway link-local address to match against in the RA packets.
//
// Returns:
// - error: An error if the nftables setup fails, otherwise nil.
func setupIngressFilter(ifaceName, gwLLA string) error {
	const (
		tableName   = "ingress_filter" // Name of the nftables table to be created
		rasLifetime = "@th,48,16"      // Offset for RA lifetime field in ICMPv6 packets
	)

	// Initialize a new nftables table for the ingress filter
	nft, err := knftables.New(knftables.NetDevFamily, tableName)
	if err != nil {
		return fmt.Errorf("failed to initialize table: %w", err)
	}

	// Delegate the setup of the filter with the specified table and lifetime matcher
	return setupIngressFilterWithTableAndLifetimeMatcher(nft, ifaceName, gwLLA, rasLifetime)
}

func setupIngressFilterWithTableAndLifetimeMatcher(nft knftables.Interface, ifaceName, gwLLA, rasLifetime string) error {
	const (
		chainName = "input"
	)

	tx := nft.NewTransaction()

	tx.Add(&knftables.Table{})

	tx.Add(&knftables.Chain{
		Name:     chainName,
		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.IngressHook),
		Priority: knftables.PtrTo(knftables.FilterPriority),
		Device:   knftables.PtrTo(ifaceName),
	})

	tx.Add(&knftables.Rule{
		Chain: chainName,
		Rule: knftables.Concat(
			"icmpv6", "type", "nd-router-advert", "ip6", "saddr", "!=", gwLLA, rasLifetime, "!=", "0", "drop",
		),
	})

	// Execute the transaction
	if err := nft.Run(context.Background(), tx); err != nil {
		return fmt.Errorf("could not update netdev nftables: %v", err)
	}

	return nil

}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package managementport

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/vishvananda/netlink"

	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// bringupManagementPortLink update the management port interface with the expected mac/name/mtu
func bringupManagementPortLink(netName string, link netlink.Link, macAddr *net.HardwareAddr, ifName string, mtu int) error {
	var err error

	// if this is called when deleting the management port, see if its original interface name is saved in the alias
	klog.V(5).Infof("Create management port link %s mac %+v current name %s mtu %v for network %s",
		ifName, macAddr, link.Attrs().Name, mtu, netName)

	setMac := false
	setMTU := false
	oldIfName := link.Attrs().Name
	setName := oldIfName != ifName
	if macAddr != nil {
		setMac = link.Attrs().HardwareAddr.String() != macAddr.String()
	}
	if mtu != 0 {
		setMTU = link.Attrs().MTU != mtu
	}
	if setMac || setName || setMTU {
		err = util.GetNetLinkOps().LinkSetDown(link)
		if err != nil {
			return fmt.Errorf("failed to set link down for management port %s for network %s: %v", oldIfName, netName, err)
		}

		if setMac {
			err = util.GetNetLinkOps().LinkSetHardwareAddr(link, *macAddr)
			if err != nil {
				return fmt.Errorf("failed to set MAC address %s for management port %s for network %s: %v", macAddr.String(), ifName, netName, err)
			}
		}

		if setName {
			err = util.GetNetLinkOps().LinkSetName(link, ifName)
			if err != nil {
				return fmt.Errorf("failed to rename management port name from %s to %s for network %s: %v", oldIfName, ifName, netName, err)
			}
			// when creating the management port, set the old link name as alias, it can then be used to rename the link back.
			err = util.GetNetLinkOps().LinkSetAlias(link, oldIfName)
			if err != nil {
				return fmt.Errorf("failed to set alias %s on the renamed link %s for network %s: %v", oldIfName, ifName, netName, err)
			}
		}

		if setMTU {
			err = util.GetNetLinkOps().LinkSetMTU(link, mtu)
			if err != nil {
				return fmt.Errorf("failed to set MTU %d for management port %s for network %s: %v", mtu, ifName, netName, err)
			}
		}

	}
	// needs to bring the link up if this is to create the management port
	err = util.GetNetLinkOps().LinkSetUp(link)
	if err != nil {
		return fmt.Errorf("failed to set link up for management port %s for network %s: %v", ifName, netName, err)
	}
	return nil
}

// TearDownManagementPortLink bring down the management port interface, and rename back to the original link name if needed
func TearDownManagementPortLink(netName string, link netlink.Link, originalIfName string) error {
	attrs := link.Attrs()
	// check if the interface's original interface name is saved in the alias,
	// it overrides the save name in open-vswitch external-id
	savedName := attrs.Alias
	if savedName == "" {
		if originalIfName != "" {
			savedName = originalIfName
		}
	}
	ifName := attrs.Name
	if savedName != "" && savedName != ifName {
		if _, err := util.GetNetLinkOps().LinkByName(savedName); err == nil {
			// saved name already be taken by other link
			klog.Warningf("Saved management port link name for %s is %s, but is already taken by another link",
				ifName, savedName)
			savedName = ""
		}
	}

	if savedName == "" {
		// rename to "net" + "ddmmyyHHMMSS"
		savedName = time.Now().Format("net010206150405")
		klog.Warningf("No saved management port name for %s, renaming to %s", ifName, savedName)
	}

	klog.V(5).Infof("Tear down management port link %s for network %s", ifName, netName)

	err := util.LinkAddrFlush(link)
	if err != nil {
		klog.Warningf("Failed to flush IP addresses on management port %s for network %s: %v", ifName, netName, err)
	}

	err = util.GetNetLinkOps().LinkSetDown(link)
	if err != nil {
		return fmt.Errorf("failed to set link down for management port %s for network %s: %v", ifName, netName, err)
	}

	if savedName != "" && ifName != savedName {
		klog.V(5).Infof("Restore the original management port name from %s to %s for network %s", ifName, savedName, netName)
		err = util.GetNetLinkOps().LinkSetName(link, savedName)
		if err != nil {
			return fmt.Errorf("failed to rename management port name from %s to %s for network %s: %v", ifName, savedName, netName, err)
		}
	}
	return nil
}

func createManagementPortOVSRepresentor(ovsClient libovsdbclient.Client, netName, ifname, ifaceid string, mtu int, externalIds []string) error {
	if ovsClient == nil {
		return createManagementPortOVSRepresentorWithExec(netName, ifname, ifaceid, mtu, externalIds)
	}

	bridge, err := ops.GetBridge(ovsClient, "br-int")
	if err != nil {
		return fmt.Errorf("failed to get datapath type for bridge br-int: %w", err)
	}
	ifaceExternalIDs := map[string]string{"iface-id": ifaceid}
	for _, value := range externalIds {
		keyValue := strings.SplitN(value, "=", 2)
		if len(keyValue) != 2 {
			return fmt.Errorf("invalid OVS external ID %q", value)
		}
		ifaceExternalIDs[keyValue[0]] = keyValue[1]
	}
	iface := &vswitchd.Interface{ExternalIDs: ifaceExternalIDs}
	if bridge.DatapathType == types.DatapathUserspace {
		iface.Type = "dpdk"
		iface.MTURequest = &mtu
	}

	klog.V(5).Infof("Add OVS representor OVS interface %s to bridge br-int for network %s: ifaceID %s mtu %v externalIDs %v",
		ifname, netName, ifaceid, mtu, externalIds)
	if err := ops.CreateOrUpdatePodPort(ovsClient, "br-int", ifname, &vswitchd.Port{}, iface); err != nil {
		return fmt.Errorf("failed to add port %q to br-int: %w", ifname, err)
	}
	return nil
}

func createManagementPortOVSRepresentorWithExec(netName, ifname, ifaceid string, mtu int, externalIds []string) error {
	br_type, err := util.GetDatapathType("br-int")
	if err != nil {
		return fmt.Errorf("failed to get datapath type for bridge br-int : %v", err)
	}

	ovsArgs := []string{
		"--", "--may-exist", "add-port", "br-int", ifname,
		"--", "set", "interface", ifname,
		"external-ids:iface-id=" + ifaceid,
	}
	for _, v := range externalIds {
		ovsArgs = append(ovsArgs, fmt.Sprintf("external-ids:%s", v))
	}

	if br_type == types.DatapathUserspace {
		dpdkArgs := []string{"type=dpdk"}
		ovsArgs = append(ovsArgs, dpdkArgs...)
		ovsArgs = append(ovsArgs, fmt.Sprintf("mtu_request=%v", mtu))
	}

	klog.V(5).Infof("Add OVS representor OVS interface %s to bridge br-int for network %s: ifaceID %s mtu %v externalIDs %v",
		ifname, netName, ifaceid, mtu, externalIds)
	// Plug management port representor to OVS.
	stdout, stderr, err := util.RunOVSVsctl(ovsArgs...)
	if err != nil {
		klog.Errorf("Failed to add port %q to br-int, stdout: %q, stderr: %q, error: %v",
			ifname, stdout, stderr, err)
		return err
	}
	return nil
}

// deleteManagementPortOVSInterface delete the management port OVS interface from the br-int bridge:
func deleteManagementPortOVSInterface(ovsClient libovsdbclient.Client, network, ovsIfName string) error {
	if ovsClient != nil {
		if err := ops.DeletePortWithInterfaces(ovsClient, "br-int", ovsIfName); err != nil {
			return fmt.Errorf("failed to delete port %s from br-int for network %s: %w", ovsIfName, network, err)
		}
		return nil
	}
	stdout, stderr, err := util.RunOVSVsctl("--if-exists", "del-port", "br-int", ovsIfName)
	if err != nil {
		return fmt.Errorf("failed to delete port %s from br-int for network %s, stdout: %q, stderr: %q, error: %v",
			ovsIfName, network, stdout, stderr, err)
	}
	return nil
}

// DeleteManagementPortInternalOVSInterface delete the management port OVS internal interface:
func DeleteManagementPortInternalOVSInterface(ovsClient libovsdbclient.Client, network, ovsIfName string) error {
	klog.V(5).Infof("Removed OVS management port internal OVS interface %s for network %s", ovsIfName, network)

	err := deleteManagementPortOVSInterface(ovsClient, network, ovsIfName)
	if err != nil {
		return err
	}

	// verify linux device removal - insurance in case something happens with OVS/OVSDB and interface is not removed
	if link, err := netlink.LinkByName(ovsIfName); err == nil {
		klog.Warningf("Management port interface %s still exists after OVS del-port, deleting manually", ovsIfName)
		err = netlink.LinkDel(link)
		if err != nil {
			return fmt.Errorf("failed force remove management port %q, error: %w", ovsIfName, err)
		}
	}
	return nil
}

func DeleteManagementPortRepInterface(ovsClient libovsdbclient.Client, network, repDevice, savedName string) error {
	klog.V(5).Infof("Removed OVS management port Representor OVS interface %s for network %s", repDevice, network)

	err := deleteManagementPortOVSInterface(ovsClient, network, repDevice)
	if err != nil {
		return err
	}

	link, err := util.GetNetLinkOps().LinkByName(repDevice)
	if err != nil {
		if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return fmt.Errorf("failed to lookup management port representor interface %s for network %s: %v", repDevice, network, err)
		}

		klog.V(5).Infof("Management port representor interface %s is already removed for network %s", repDevice, network)

		return nil
	}

	err = TearDownManagementPortLink(network, link, savedName)
	if err != nil {
		return fmt.Errorf("cleanup management port %s for network %s failed: %v", repDevice, network, err)
	}
	return nil
}

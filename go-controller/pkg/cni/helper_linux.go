// +build linux

package cni

import (
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/sirupsen/logrus"

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

func setupInterface(netns ns.NetNS, hostIfaceName, ifName, macAddress, ipAddress, gatewayIP string, mtu int, isDefaultInterface bool) (*current.Interface, *current.Interface, error) {
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

		if ipAddress != "" {
			addr, err := netlink.ParseAddr(ipAddress)
			if err != nil {
				return err
			}
			err = netlink.AddrAdd(link, addr)
			if err != nil {
				return fmt.Errorf("failed to add IP addr %s to %s: %v", ipAddress, contIface.Name, err)
			}
		}

		if gatewayIP != "" {
			gw := net.ParseIP(gatewayIP)
			if gw == nil {
				return fmt.Errorf("parse ip of gateway failed")
			}
			if gw != nil {
				err = ip.AddRoute(nil, gw, link)
				if err != nil {
					return err
				}
			}
		}

		oldHostVethName = hostVeth.Name

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// rename the host end of veth pair
	hostIface.Name = hostIfaceName
	if err := renameLink(oldHostVethName, hostIface.Name); err != nil {
		return nil, nil, fmt.Errorf("failed to rename %s to %s: %v", oldHostVethName, hostIface.Name, err)
	}

	return hostIface, contIface, nil
}

// ConfigureInterface sets up the container interface
func (pr *PodRequest) ConfigureInterface(namespace string, podName string, networkName string, macAddress string, ipAddress string, gatewayIP string, mtu int, ingress, egress int64) ([]*current.Interface, error) {
	netns, err := ns.GetNS(pr.Netns)
	if err != nil {
		return nil, fmt.Errorf("failed to open netns %q: %v", pr.Netns, err)
	}
	defer netns.Close()

	isDefaultInterface := isDefaultInterface(networkName)
	var ifaceID string
	var hostIfaceName string
	if isDefaultInterface {
		ifaceID = fmt.Sprintf("%s_%s", namespace, podName)
		hostIfaceName = pr.SandboxID[:15]
	} else {
		ifaceID = fmt.Sprintf("%s_%s_%s", namespace, podName, networkName)
		hostIfaceName, err = ip.RandomVethName()
		if err != nil {
			return nil, err
		}
	}

	hostIface, contIface, err := setupInterface(netns, hostIfaceName, pr.IfName, macAddress, ipAddress, gatewayIP, mtu, isDefaultInterface)
	if err != nil {
		return nil, err
	}

	ovsArgs := []string{
		"add-port", "br-int", hostIface.Name, "--", "set",
		"interface", hostIface.Name,
		fmt.Sprintf("external_ids:attached_mac=%s", macAddress),
		fmt.Sprintf("external_ids:iface-id=%s", ifaceID),
		fmt.Sprintf("external_ids:sandbox=%s", pr.SandboxID),
	}
	if ipAddress != "" {
		ovsArgs = append(ovsArgs, fmt.Sprintf("external_ids:ip_address=%s", ipAddress))
	}

	if out, err := ovsExec(ovsArgs...); err != nil {
		return nil, fmt.Errorf("failure in plugging pod interface: %v\n  %q", err, out)
	}

	if isDefaultInterface {
		if err := clearPodBandwidth(pr.SandboxID); err != nil {
			return nil, err
		}
		if ingress > 0 || egress > 0 {
			l, err := netlink.LinkByName(hostIface.Name)
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
	}

	return []*current.Interface{hostIface, contIface}, nil
}

// PlatformSpecificCleanup deletes the OVS port
func (pr *PodRequest) PlatformSpecificCleanup() error {
	networkName := pr.CNIConf.Name
	var portList []string
	isDefaultInterface := isDefaultInterface(networkName)
	if isDefaultInterface {
		portList = make([]string, 1)
		portList[0] = pr.SandboxID[:15]
	} else {
		var err error
		ifaceName := fmt.Sprintf("%s_%s_%s", pr.PodNamespace, pr.PodName, networkName)
		portList, err = ovsFind("interface", "name", fmt.Sprintf("external-ids:iface-id=%s", ifaceName))
		if err != nil {
			logrus.Infof("failed to find OVS port with external-ids:iface-id: %s  error: %v", ifaceName, err)
		}
	}

	for _, ifaceName := range portList {
		ovsArgs := []string{
			"del-port", "br-int", ifaceName,
		}
		out, err := exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "no port named") {
			// DEL should be idempotent; don't return an error just log it
			logrus.Warningf("failed to delete OVS port %s: %v\n  %q", ifaceName, err, string(out))
		}
	}

	if isDefaultInterface {
		_ = clearPodBandwidth(pr.SandboxID)
	}

	return nil
}

func isDefaultInterface(networkName string) bool {
	return networkName == "ovn-kubernetes"
}

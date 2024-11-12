package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/vishvananda/netlink"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

const (
	bridgeName = "ovsbr1"
	add        = "add-br"
	del        = "del-br"
	mayExist   = "--may-exist"
	ifExists   = "--if-exists"
)

func setupUnderlay(ovsPods []v1.Pod, portName string, nadConfig networkAttachmentConfig) error {
	for _, ovsPod := range ovsPods {
		if err := addOVSBridge(ovsPod.Name, bridgeName); err != nil {
			return err
		}

		if nadConfig.vlanID > 0 {
			if err := ovsEnableVLANAccessPort(ovsPod.Name, bridgeName, portName, nadConfig.vlanID); err != nil {
				return err
			}
		} else {
			if err := ovsAttachPortToBridge(ovsPod.Name, bridgeName, portName); err != nil {
				return err
			}
		}

		if err := configureBridgeMappings(
			ovsPod.Name,
			defaultNetworkBridgeMapping(),
			bridgeMapping(nadConfig.networkName, bridgeName),
		); err != nil {
			return err
		}
	}
	return nil
}

func teardownUnderlay(ovsPods []v1.Pod) error {
	for _, ovsPod := range ovsPods {
		if err := removeOVSBridge(ovsPod.Name, bridgeName); err != nil {
			return err
		}
	}
	return nil
}

func ovsPods(clientSet clientset.Interface) []v1.Pod {
	const (
		ovsNodeLabel = "app=ovs-node"
	)
	pods, err := clientSet.CoreV1().Pods(ovnNamespace).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: ovsNodeLabel},
	)
	if err != nil {
		return nil
	}
	return pods.Items
}

func addOVSBridge(ovnNodeName string, bridgeName string) error {
	_, err := runCommand(ovsBridgeCommand(ovnNodeName, add, bridgeName)...)
	if err != nil {
		return fmt.Errorf("failed to ADD OVS bridge %s: %v", bridgeName, err)
	}
	return nil
}

func removeOVSBridge(ovnNodeName string, bridgeName string) error {
	_, err := runCommand(ovsBridgeCommand(ovnNodeName, del, bridgeName)...)
	if err != nil {
		return fmt.Errorf("failed to DELETE OVS bridge %s: %v", bridgeName, err)
	}
	return nil
}

func ovsBridgeCommand(ovnNodeName string, addOrDeleteCmd string, bridgeName string) []string {
	return []string{
		"kubectl", "-n", ovnNamespace, "exec", ovnNodeName, "--",
		"ovs-vsctl", mayOrIfExists(addOrDeleteCmd), addOrDeleteCmd, bridgeName,
	}
}

func ovsAttachPortToBridge(ovsNodeName string, bridgeName string, portName string) error {
	cmd := []string{
		"kubectl", "-n", ovnNamespace, "exec", ovsNodeName, "--",
		"ovs-vsctl", mayExist, "add-port", bridgeName, portName,
	}

	if _, err := runCommand(cmd...); err != nil {
		return fmt.Errorf("failed to add port %s to OVS bridge %s: %v", portName, bridgeName, err)
	}

	return nil
}

func ovsEnableVLANAccessPort(ovsNodeName string, bridgeName string, portName string, vlanID int) error {
	cmd := []string{
		"kubectl", "-n", ovnNamespace, "exec", ovsNodeName, "--",
		"ovs-vsctl", mayExist, "add-port", bridgeName, portName, fmt.Sprintf("tag=%d", vlanID), "vlan_mode=access",
	}

	if _, err := runCommand(cmd...); err != nil {
		return fmt.Errorf("failed to add port %s to OVS bridge %s: %v", portName, bridgeName, err)
	}

	return nil
}

type BridgeMapping struct {
	physnet   string
	ovsBridge string
}

func (bm BridgeMapping) String() string {
	return fmt.Sprintf("%s:%s", bm.physnet, bm.ovsBridge)
}

type BridgeMappings []BridgeMapping

func (bms BridgeMappings) String() string {
	return strings.Join(Map(bms, func(bm BridgeMapping) string { return bm.String() }), ",")
}

func Map[T, V any](items []T, fn func(T) V) []V {
	result := make([]V, len(items))
	for i, t := range items {
		result[i] = fn(t)
	}
	return result
}

func configureBridgeMappings(ovnNodeName string, mappings ...BridgeMapping) error {
	mappingsString := fmt.Sprintf("external_ids:ovn-bridge-mappings=%s", BridgeMappings(mappings).String())
	cmd := []string{"kubectl", "-n", ovnNamespace, "exec", ovnNodeName,
		"--", "ovs-vsctl", "set", "open", ".", mappingsString,
	}
	_, err := runCommand(cmd...)
	return err
}

func defaultNetworkBridgeMapping() BridgeMapping {
	return BridgeMapping{
		physnet:   "physnet",
		ovsBridge: "breth0",
	}
}

func bridgeMapping(physnet, ovsBridge string) BridgeMapping {
	return BridgeMapping{
		physnet:   physnet,
		ovsBridge: ovsBridge,
	}
}

type Vlan struct {
	deviceName string
	id         string
	name       string
	ip         *net.IPNet
}

type option func(vlan *Vlan) error

func newVLANIface(deviceName string, vlanID int, opts ...option) (*Vlan, error) {
	vlan := &Vlan{
		deviceName: deviceName,
		id:         fmt.Sprintf("%d", vlanID),
	}
	vlan.name = vlanName(deviceName, vlan.id)
	for _, opt := range opts {
		if err := opt(vlan); err != nil {
			return nil, err
		}
	}
	return vlan, nil
}

func withIP(ipAddress string) option {
	return func(vlan *Vlan) error {
		ip, cidr, err := net.ParseCIDR(ipAddress)
		if err != nil {
			return fmt.Errorf("failed to parse IP address %s: %w", ipAddress, err)
		}
		cidr.IP = ip
		vlan.ip = cidr
		return nil
	}
}

func (v *Vlan) String() string {
	return v.name
}

func (v *Vlan) ensureExistence() error {
	if err := v.ensureVLANEnabled(); err != nil {
		return err
	}

	if v.ip != nil {
		if err := v.ensureVLANHasIP(); err != nil {
			return err
		}
	}

	return nil
}

func (v *Vlan) ensureVLANEnabled() error {
	_, err := netlink.LinkByName(v.name)
	if errors.As(err, &netlink.LinkNotFoundError{}) {
		cmd := exec.Command("sudo", "ip", "link", "add", "link", v.deviceName, "name", v.name, "type", "vlan", "id", v.id)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to create vlan interface %s: %v", v.name, err)
		}
	}
	linkUpCmd := exec.Command("sudo", "ip", "link", "set", "dev", v.name, "up")
	linkUpCmd.Stderr = os.Stderr
	if err := linkUpCmd.Run(); err != nil {
		return fmt.Errorf("failed to enable vlan interface %s: %w", v.name, err)
	}
	return nil
}

func (v *Vlan) ensureVLANHasIP() error {
	vlanIface, err := netlink.LinkByName(v.name)
	if err != nil {
		return fmt.Errorf("failed to find VLAN interface %s: %w", v.name, err)
	}

	addr := netlink.Addr{IPNet: v.ip}
	hasIP, err := isIPInLink(vlanIface, addr)
	if err != nil {
		return err
	}

	if !hasIP {
		cmd := exec.Command("sudo", "ip", "addr", "add", v.ip.String(), "dev", v.name)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to define the vlan interface %q IP Address %s: %w", v.name, *v.ip, err)
		}
	}

	return nil
}

func (v *Vlan) delete() error {
	cmd := exec.Command("sudo", "ip", "link", "del", v.name)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to delete vlan interface %s: %v", v.name, err)
	}
	return nil
}

func vlanName(deviceName string, vlanID string) string {
	// MAX IFSIZE 16; got to truncate it to add the vlan suffix
	if len(deviceName)+len(vlanID)+1 > 16 {
		deviceName = deviceName[:len(deviceName)-len(vlanID)-1]
	}
	return fmt.Sprintf("%s.%s", deviceName, vlanID)
}

func isIPInLink(vlanIface netlink.Link, addr netlink.Addr) (bool, error) {
	vlanAddrs, err := netlink.AddrList(vlanIface, netlink.FAMILY_ALL)
	if err != nil {
		return false, err
	}

	for _, vlanAddr := range vlanAddrs {
		if vlanAddr.Equal(addr) {
			return true, nil
		}
	}
	return false, nil
}

func mayOrIfExists(addOrDeleteCmd string) string {
	if addOrDeleteCmd == del {
		return ifExists
	}
	return mayExist
}

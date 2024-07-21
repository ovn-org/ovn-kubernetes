package node

import (
	"fmt"
	"net"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// ctMarkUDNBase is the conntrack mark base value for user defined networks to use
	// Each network gets its own mark == base + network-id
	ctMarkUDNBase = 3
)

// UserDefinedNetworkGateway contains information
// required to program a UDN at each node's
// gateway.
// NOTE: Currently invoked only for primary networks.
type UserDefinedNetworkGateway struct {
	// network information
	util.NetInfo
	// stores the networkID of this network
	networkID int
	// node that its programming things on
	node *v1.Node
	// masqCTMark holds the mark value for this network
	// which is used for egress traffic in shared gateway mode
	masqCTMark uint
}

func NewUserDefinedNetworkGateway(netInfo util.NetInfo, networkID int, node *v1.Node) *UserDefinedNetworkGateway {
	return &UserDefinedNetworkGateway{
		NetInfo:   netInfo,
		networkID: networkID,
		node:      node,

		// Generate a per network conntrack mark to be used for egress traffic.
		masqCTMark: ctMarkUDNBase + uint(networkID),
	}
}

// AddNetwork will be responsible to create all plumbings
// required by this UDN on the gateway side
func (udng *UserDefinedNetworkGateway) AddNetwork() error {
	return udng.addUDNManagementPort()
}

// DelNetwork will be responsible to remove all plumbings
// used by this UDN on the gateway side
func (udng *UserDefinedNetworkGateway) DelNetwork() error {
	return udng.deleteUDNManagementPort()
}

// addUDNManagementPort does the following:
// STEP1: creates the (netdevice) OVS interface on br-int for the UDN's management port
// STEP2: It saves the MAC address generated on the 1st go as an option on the OVS interface
// so that it persists on reboots
// STEP3: sets up the management port link on the host
// STEP4: adds the management port IP .2 to the mplink
func (udng *UserDefinedNetworkGateway) addUDNManagementPort() error {
	var err error
	interfaceName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.networkID))
	var networkLocalSubnets []*net.IPNet
	if udng.TopologyType() == types.Layer3Topology {
		networkLocalSubnets, err = util.ParseNodeHostSubnetAnnotation(udng.node, udng.GetNetworkName())
		if err != nil {
			return fmt.Errorf("waiting for node %s to start, no annotation found on node for network %s: %w",
				udng.node.Name, udng.GetNetworkName(), err)
		}
	} else if udng.TopologyType() == types.Layer2Topology {
		// NOTE: We don't support L2 networks without subnets as primary UDNs
		globalFlatL2Networks := udng.Subnets()
		for _, globalFlatL2Network := range globalFlatL2Networks {
			networkLocalSubnets = append(networkLocalSubnets, globalFlatL2Network.CIDR)
		}
	}

	// STEP1
	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--may-exist", "add-port", "br-int", interfaceName,
		"--", "set", "interface", interfaceName,
		"type=internal", "mtu_request="+fmt.Sprintf("%d", udng.NetInfo.MTU()),
		"external-ids:iface-id="+udng.GetNetworkScopedK8sMgmtIntfName(udng.node.Name),
	)
	if err != nil {
		return fmt.Errorf("failed to add port to br-int for network %s, stdout: %q, stderr: %q, error: %w",
			udng.GetNetworkName(), stdout, stderr, err)
	}
	klog.V(3).Infof("Added OVS management port interface %s for network %s", interfaceName, udng.GetNetworkName())

	// STEP2
	macAddress, err := util.GetOVSPortMACAddress(interfaceName)
	if err != nil {
		return fmt.Errorf("failed to get management port MAC address for network %s: %v", udng.GetNetworkName(), err)
	}
	// persist the MAC address so that upon node reboot we get back the same mac address.
	_, stderr, err = util.RunOVSVsctl("set", "interface", interfaceName,
		fmt.Sprintf("mac=%s", strings.ReplaceAll(macAddress.String(), ":", "\\:")))
	if err != nil {
		return fmt.Errorf("failed to persist MAC address %q for %q while plumbing network %s: stderr:%s (%v)",
			macAddress.String(), interfaceName, udng.GetNetworkName(), stderr, err)
	}

	// STEP3
	mplink, err := util.LinkSetUp(interfaceName)
	if err != nil {
		return fmt.Errorf("failed to set the link up for interface %s while plumbing network %s, err: %v",
			interfaceName, udng.GetNetworkName(), err)
	}
	klog.V(3).Infof("Setup management port link %s for network %s succeeded", interfaceName, udng.GetNetworkName())

	// STEP4
	for _, subnet := range networkLocalSubnets {
		if config.IPv6Mode && utilnet.IsIPv6CIDR(subnet) || config.IPv4Mode && utilnet.IsIPv4CIDR(subnet) {
			ip := util.GetNodeManagementIfAddr(subnet)
			var err error
			var exists bool
			if exists, err = util.LinkAddrExist(mplink, ip); err == nil && !exists {
				err = util.LinkAddrAdd(mplink, ip, 0, 0, 0)
			}
			if err != nil {
				return fmt.Errorf("failed to add management port IP from subnet %s to netdevice %s for network %s, err: %v",
					subnet, interfaceName, udng.GetNetworkName(), err)
			}
		}
	}
	return nil
}

// deleteUDNManagementPort does the following:
// STEP1: deletes the management port link on the host.
// STEP2: deletes the OVS interface on br-int for the UDN's management port interface
func (udng *UserDefinedNetworkGateway) deleteUDNManagementPort() error {
	var err error
	interfaceName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.networkID))

	// STEP1 (note: doing step2 also guarantees step1; doing it for posterity)
	if err := util.LinkDelete(interfaceName); err != nil {
		return fmt.Errorf("failed to delete link %s for network %s: %v", interfaceName, udng.GetNetworkName(), err)
	}
	klog.V(3).Infof("Removed management port link %s for network %s", interfaceName, udng.GetNetworkName())

	// STEP2
	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--if-exists", "del-port", "br-int", interfaceName,
	)
	if err != nil {
		return fmt.Errorf("failed to delete port from br-int for network %s, stdout: %q, stderr: %q, error: %v",
			udng.GetNetworkName(), stdout, stderr, err)
	}
	klog.V(3).Infof("Removed OVS management port interface %s for network %s", interfaceName, udng.GetNetworkName())
	return nil
}

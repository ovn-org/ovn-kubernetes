package node

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	// VrfDeviceSuffix vrf device suffix associated with every user defined primary network.
	VrfDeviceSuffix = "-vrf"
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
	node          *v1.Node
	nodeAnnotator kube.Annotator
	// vrf manager that creates and manages vrfs for all UDNs
	// used with a lock since its shared between all network controllers
	vrfManager *vrfmanager.Controller
}

func NewUserDefinedNetworkGateway(netInfo util.NetInfo, networkID int, node *v1.Node, nodeAnnotator kube.Annotator,
	vrfManager *vrfmanager.Controller) *UserDefinedNetworkGateway {
	return &UserDefinedNetworkGateway{
		NetInfo:       netInfo,
		networkID:     networkID,
		node:          node,
		nodeAnnotator: nodeAnnotator,
		vrfManager:    vrfManager,
	}
}

func GetVrfDeviceNameForUDN(managementPortName string) string {
	return managementPortName[8:] + VrfDeviceSuffix
}

// AddNetwork will be responsible to create all plumbings
// required by this UDN on the gateway side
func (udng *UserDefinedNetworkGateway) AddNetwork() error {
	err := udng.addUDNManagementPort()
	if err != nil {
		return fmt.Errorf("could not create management port netdevice for network %s: %w", udng.GetNetworkName(), err)
	}
	mgmtPortName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.networkID))
	vrfDeviceName := GetVrfDeviceNameForUDN(mgmtPortName)
	mplink, err := util.GetNetLinkOps().LinkByName(mgmtPortName)
	if err != nil {
		return fmt.Errorf("could not fetch link %s for network %s, err: %v", mgmtPortName, udng.GetNetworkName(), err)
	}
	vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
	// TODO(tssurya): check with peri if we really want a list?
	enslaveInterfaces := make(sets.Set[string])
	enslaveInterfaces.Insert(mgmtPortName)
	err = udng.vrfManager.AddVrf(vrfDeviceName, uint32(vrfTableId), enslaveInterfaces)
	if err != nil {
		return fmt.Errorf("could not add VRF %d for network %s, err: %v", vrfTableId, udng.GetNetworkName(), err)
	}
	return nil
}

// DelNetwork will be responsible to remove all plumbings
// used by this UDN on the gateway side
func (udng *UserDefinedNetworkGateway) DelNetwork() error {
	mgmtPortName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.networkID))
	vrfDeviceName := GetVrfDeviceNameForUDN(mgmtPortName)
	err := udng.vrfManager.DeleteVrf(vrfDeviceName)
	if err != nil {
		return err
	}
	return udng.deleteUDNManagementPort()
}

// addUDNManagementPort does the following:
// STEP1: creates the OVS interface on br-int for the UDN's management port interface
// STEP2: It saves the MAC address generated on the 1st go as an option on the OVS interface
// so that it persists on reboots
// STEP3: creates the management port netdevice on the host
// STEP4: adds the management port IP .2 to the netdevice
func (udng *UserDefinedNetworkGateway) addUDNManagementPort() error {
	var err error
	interfaceName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.networkID))
	var networkLocalSubnets []*net.IPNet
	if udng.TopologyType() == types.Layer3Topology {
		networkLocalSubnets, err = util.ParseNodeHostSubnetAnnotation(udng.node, udng.GetNetworkName())
		if err != nil {
			return fmt.Errorf("waiting for node %s to start, no annotation found on node for network %s: %v",
				udng.node.Name, udng.GetNetworkName(), err)
		}
	} else if udng.TopologyType() == types.Layer2Topology {
		globalFlatL2Networks := udng.Subnets()
		for _, globalFlatL2Network := range globalFlatL2Networks {
			networkLocalSubnets = append(networkLocalSubnets, globalFlatL2Network.CIDR)
		}
	}
	networkMTU := udng.NetInfo.MTU()
	if networkMTU == 0 {
		networkMTU = config.Default.MTU
	}

	// STEP1
	stdout, stderr, err := util.RunOVSVsctl(
		// delete if any port exists just to clean things up
		"--", "--if-exists", "del-port", "br-int", interfaceName,
		"--", "--may-exist", "add-port", "br-int", interfaceName,
		"--", "set", "interface", interfaceName,
		"type=internal", "mtu_request="+fmt.Sprintf("%d", networkMTU),
		"external-ids:iface-id="+udng.GetNetworkScopedK8sMgmtIntfName(udng.node.Name),
	)
	if err != nil {
		return fmt.Errorf("failed to add port to br-int for network %s, stdout: %q, stderr: %q, error: %v",
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
	klog.V(3).Infof("Added management port netdevice %s for network %s", interfaceName, udng.GetNetworkName())

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
				return fmt.Errorf("unable to add management port IP from subnet %s to netdevice %s for network %s, err: %v",
					subnet, interfaceName, udng.GetNetworkName(), err)
			}
		}
	}

	if err := util.UpdateNodeManagementPortMACAddresses(udng.node, udng.nodeAnnotator, macAddress, udng.GetNetworkName()); err != nil {
		return err
	}
	if err := udng.nodeAnnotator.Run(); err != nil {
		return fmt.Errorf("failed to set node %s annotations for network %s: %w", udng.node.Name, udng.GetNetworkName(), err)
	}
	klog.V(3).Infof("Added management port mac address information %s for network %s", interfaceName, udng.GetNetworkName())
	return nil
}

// deleteUDNManagementPort does the following:
// STEP1: deletes the management port link on the host.
// STEP2: deletes the OVS interface on br-int for the UDN's management port interface
func (udng *UserDefinedNetworkGateway) deleteUDNManagementPort() error {
	var err error
	interfaceName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.networkID))

	// STEP1 (note: doing step2 also guarantees step1; doing it for posterity)
	err = util.LinkDelete(interfaceName)
	if err != nil {
		return fmt.Errorf("failed to delete link %s for network %s: %v", interfaceName, udng.GetNetworkName(), err)
	}
	klog.V(3).Infof("Removed management port netdevice %s for network %s", interfaceName, udng.GetNetworkName())

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

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package bridgeconfig

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/egressip"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	nodeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// bridgeMappingsMutex protects the read-modify-write of the shared
// ovn-bridge-mappings external ID. UDN gateways reconcile independently, so
// different networks may add mappings concurrently.
var bridgeMappingsMutex sync.Mutex

// BridgeUDNConfiguration holds the patchport and ctMark
// information for a given network
type BridgeUDNConfiguration struct {
	PatchPort     string
	OfPortPatch   string
	MasqCTMark    string
	PktMark       string
	V4MasqIPs     *udn.MasqueradeIPs
	V6MasqIPs     *udn.MasqueradeIPs
	Subnets       []config.CIDRNetworkEntry
	NodeSubnets   []*net.IPNet
	Advertised    atomic.Bool
	ManagementIPs []*net.IPNet
	// Transport mode for this network ("no-overlay", "evpn", or "" for default network)
	Transport string
	// OutboundSNAT setting for this network ("enabled", "disabled", or "")
	OutboundSNAT string
}

func (netConfig *BridgeUDNConfiguration) ShallowCopy() *BridgeUDNConfiguration {
	copy := &BridgeUDNConfiguration{
		PatchPort:    netConfig.PatchPort,
		OfPortPatch:  netConfig.OfPortPatch,
		MasqCTMark:   netConfig.MasqCTMark,
		PktMark:      netConfig.PktMark,
		V4MasqIPs:    netConfig.V4MasqIPs,
		V6MasqIPs:    netConfig.V6MasqIPs,
		Subnets:      netConfig.Subnets,
		NodeSubnets:  netConfig.NodeSubnets,
		Transport:    netConfig.Transport,
		OutboundSNAT: netConfig.OutboundSNAT,
	}
	copy.Advertised.Store(netConfig.Advertised.Load())
	return copy
}

func (netConfig *BridgeUDNConfiguration) IsDefaultNetwork() bool {
	return netConfig.MasqCTMark == nodetypes.CtMarkOVN
}

func getInterfaceOfPort(ovsClient libovsdbclient.Client, ifaceName string) (string, error) {
	return getInterfaceOfPortIfExists(ovsClient, ifaceName, false)
}

func getInterfaceOfPortIfExists(ovsClient libovsdbclient.Client, ifaceName string, ifExists bool) (string, error) {
	iface, err := ovsops.GetOVSInterface(ovsClient, ifaceName)
	if errors.Is(err, libovsdbclient.ErrNotFound) {
		if ifExists {
			return "", nil
		}
		return "", err
	}
	if err != nil {
		return "", err
	}
	if iface.Ofport == nil || *iface.Ofport == -1 {
		return "", fmt.Errorf("interface %s has no valid ofport", ifaceName)
	}
	return strconv.Itoa(*iface.Ofport), nil
}

func getInterfaceMACAddress(ovsClient libovsdbclient.Client, ifaceName string) (net.HardwareAddr, error) {
	iface, err := ovsops.GetOVSInterface(ovsClient, ifaceName)
	if err != nil {
		return nil, err
	}
	mac := iface.MACInUse
	if mac == nil || *mac == "" {
		// A newly-created bridge's mac_in_use is populated asynchronously by
		// ovs-vswitchd. NicToBridge pins the same address in other_config, which
		// is available immediately in the OVSDB transaction result.
		bridge, err := ovsops.GetBridge(ovsClient, ifaceName)
		if err != nil {
			return nil, fmt.Errorf("failed to get OVS bridge %s while resolving its MAC address: %w", ifaceName, err)
		}
		hwaddr := bridge.OtherConfig["hwaddr"]
		if hwaddr == "" {
			return nil, fmt.Errorf("interface %s has no MAC in use and bridge has no configured hwaddr", ifaceName)
		}
		mac = &hwaddr
	}
	return net.ParseMAC(*mac)
}

func (netConfig *BridgeUDNConfiguration) setOfPatchPort(ovsClient libovsdbclient.Client) error {
	ofportPatch, err := getInterfaceOfPort(ovsClient, netConfig.PatchPort)
	if err != nil {
		return fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and while getting ofport: %v",
			netConfig.PatchPort, err)
	}
	netConfig.OfPortPatch = ofportPatch
	return nil
}

type BridgeConfiguration struct {
	mutex sync.Mutex

	// variables that are only set on creation and never changed
	// don't require mutex lock to read
	ovsClient  libovsdbclient.Client
	nodeName   string
	bridgeName string
	uplinkName string
	gwIface    string
	// gwIfaceRep is the OVS representor port for the gateway MAC when one exists.
	// In DPU mode this is the host representor. In accelerated full mode this is
	// the representor of the configured gateway VF/SF.
	gwIfaceRep  string
	interfaceID string

	// variables that can be updated (read/write access should be done with mutex held)
	ofPortHost string
	ips        []*net.IPNet
	macAddress net.HardwareAddr
	ofPortPhys string
	netConfig  map[string]*BridgeUDNConfiguration
	eipMarkIPs *egressip.MarkIPsCache
	dropGARP   bool
}

// IsLocalnetTopologyPort reports whether port is an OVN patch port for a
// localnet topology. The ovn-localnet-port external ID is also set on gateway
// patch ports, so the key's presence alone is not sufficient.
func IsLocalnetTopologyPort(port *vswitchd.Port) bool {
	if port == nil {
		return false
	}
	logicalPort, ok := port.ExternalIDs["ovn-localnet-port"]
	return ok && (logicalPort == types.OVNLocalnetPort ||
		strings.HasSuffix(logicalPort, "_"+types.OVNLocalnetPort))
}

// hasLocalnetPatchPort returns true when this bridge contains an OVN patch port
// for a localnet topology.
func (b *BridgeConfiguration) hasLocalnetPatchPort() (bool, error) {
	bridge, err := ovsops.GetBridge(b.ovsClient, b.bridgeName)
	if err != nil {
		return false, fmt.Errorf("failed to find OVS bridge %s: %w", b.bridgeName, err)
	}
	bridgePortIDs := make(map[string]struct{}, len(bridge.Ports))
	for _, portID := range bridge.Ports {
		bridgePortIDs[portID] = struct{}{}
	}

	ports, err := ovsops.FindOVSPortsWithPredicate(b.ovsClient, func(port *vswitchd.Port) bool {
		if _, ok := bridgePortIDs[port.UUID]; !ok {
			return false
		}
		return IsLocalnetTopologyPort(port)
	})
	if err != nil {
		return false, fmt.Errorf("failed to list localnet ports on OVS bridge %s: %w", b.bridgeName, err)
	}
	return len(ports) > 0, nil
}

func NewBridgeConfiguration(ovsClient libovsdbclient.Client, intfName, nodeName,
	physicalNetworkName string,
	nodeSubnets, gwIPs []*net.IPNet,
	advertised bool) (*BridgeConfiguration, error) {
	var intfRep string
	var err error
	isGWAcclInterface := false
	gwIntf := intfName

	defaultNetConfig := &BridgeUDNConfiguration{
		MasqCTMark:  nodetypes.CtMarkOVN,
		Subnets:     config.Default.ClusterSubnets,
		NodeSubnets: nodeSubnets,
	}
	for _, subnet := range nodeSubnets {
		defaultNetConfig.ManagementIPs = append(defaultNetConfig.ManagementIPs, util.GetNodeManagementIfAddr(subnet))
	}
	res := BridgeConfiguration{
		ovsClient: ovsClient,
		nodeName:  nodeName,
		netConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: defaultNetConfig,
		},
		eipMarkIPs: egressip.NewMarkIPsCache(),
	}
	res.netConfig[types.DefaultNetworkName].Advertised.Store(advertised)

	// temp workaround for https://issues.redhat.com/browse/FDP-1537
	// we need to ensure we continue dropping GARPs for any new bridge config if the run mode is ovnkube controller + ovnkube node + single zone node
	// FIXME: only add if run mode is ovnkube controller + node in single process
	if config.OVNKubernetesFeature.EnableEgressIP && config.IsModeFull() {
		// drop by default - set to false later when ovnkube controller has sync'd and changes propagated to OVN southbound database
		// we should also match on run mode here to ensure ovnkube controller + ovnkube node are running in the same process
		res.dropGARP = true
	}
	// end temp work around

	if config.Gateway.GatewayAcceleratedInterface != "" {
		// Try to get representor for the specified gateway device.
		// If function succeeds, then it is either a valid switchdev VF or SF, and we can use this accelerated device
		// for node IP, Host Ofport for Openflow etc.
		// If failed - error for improper configuration option
		intfRep, err = getRepresentor(config.Gateway.GatewayAcceleratedInterface)
		if err != nil {
			return nil, fmt.Errorf("gateway accelerated interface %s is not valid: %w", config.Gateway.GatewayAcceleratedInterface, err)
		}
		gwIntf = config.Gateway.GatewayAcceleratedInterface
		isGWAcclInterface = true
		klog.Infof("For gateway accelerated interface %s representor: %s", config.Gateway.GatewayAcceleratedInterface, intfRep)
	} else {
		intfRep, err = getRepresentor(gwIntf)
		if err == nil {
			isGWAcclInterface = true
		}
	}

	if isGWAcclInterface {
		bridge, err := ovsops.GetPortBridge(ovsClient, intfRep)
		if err != nil {
			return nil, fmt.Errorf("failed to find bridge that has port %s: %w", intfRep, err)
		}
		link, err := util.GetNetLinkOps().LinkByName(gwIntf)
		if err != nil {
			return nil, fmt.Errorf("failed to get netdevice link for %s: %w", gwIntf, err)
		}
		uplinkName, err := util.GetNicName(ovsClient, bridge.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to find nic name for bridge %s: %w", bridge.Name, err)
		}
		res.bridgeName = bridge.Name
		res.uplinkName = uplinkName
		res.gwIfaceRep = intfRep
		res.gwIface = gwIntf
		res.macAddress = link.Attrs().HardwareAddr
	} else {
		bridge, err := ovsops.GetPortBridge(ovsClient, intfName)
		if err != nil {
			if !errors.Is(err, libovsdbclient.ErrNotFound) {
				return nil, fmt.Errorf("failed to find bridge that has port %s: %w", intfName, err)
			}
			if _, err := ovsops.GetBridge(ovsClient, intfName); err != nil {
				if !errors.Is(err, libovsdbclient.ErrNotFound) {
					return nil, fmt.Errorf("failed to check whether %s is an OVS bridge: %w", intfName, err)
				}
				// This is not a OVS bridge. We need to create a OVS bridge
				// and add cluster.GatewayIntf as a port of that bridge.
				bridgeName, err := util.NicToBridge(ovsClient, intfName)
				if err != nil {
					return nil, fmt.Errorf("nicToBridge failed for %s: %w", intfName, err)
				}
				if config.Gateway.DPUHostGatewayRepresentorInterface != "" {
					repErr := ovsops.CreateOrUpdatePodPort(ovsClient, bridgeName, config.Gateway.DPUHostGatewayRepresentorInterface,
						&vswitchd.Port{OtherConfig: map[string]string{"transient": "true"}}, &vswitchd.Interface{})
					if repErr != nil {
						return nil, fmt.Errorf("failed to add DPU host gateway representor %s to bridge %s: %w",
							config.Gateway.DPUHostGatewayRepresentorInterface, bridgeName, repErr)
					}
					klog.Infof("Adding host representor interface %s to bridge %s", config.Gateway.DPUHostGatewayRepresentorInterface, bridgeName)
					res.gwIfaceRep = config.Gateway.DPUHostGatewayRepresentorInterface
				}
				res.bridgeName = bridgeName
				res.gwIface = bridgeName
				res.uplinkName = intfName
				gwIntf = bridgeName
			} else {
				// gateway interface is an OVS bridge
				uplinkName, err := getIntfName(ovsClient, intfName)
				if err != nil {
					if config.Gateway.Mode == config.GatewayModeLocal && config.Gateway.AllowNoUplink {
						klog.Infof("Could not find uplink for %s, setup gateway bridge with no uplink port, egress IP and egress GW will not work", intfName)
					} else {
						return nil, fmt.Errorf("failed to find intfName for %s: %w", intfName, err)
					}
				} else {
					res.uplinkName = uplinkName
				}
				res.bridgeName = intfName
				res.gwIface = intfName
			}
		} else {
			// This is an OVS bridge's internal port
			uplinkName, err := util.GetNicName(ovsClient, bridge.Name)
			if err != nil {
				return nil, fmt.Errorf("failed to find nic name for bridge %s: %w", bridge.Name, err)
			}
			res.bridgeName = bridge.Name
			res.gwIface = bridge.Name
			res.uplinkName = uplinkName
			gwIntf = bridge.Name
		}
	}
	// Now, we get IP addresses for the bridge
	if len(gwIPs) > 0 {
		// use gwIPs if provided
		res.ips = gwIPs
	} else {
		// get IP addresses from OVS bridge. If IP does not exist,
		// error out.
		res.ips, err = nodeutil.GetNetworkInterfaceIPAddresses(gwIntf)
		if err != nil {
			return nil, fmt.Errorf("failed to get interface details for %s: %w", gwIntf, err)
		}
	}

	if !isGWAcclInterface { // We do not have an accelerated device for Gateway interface
		res.macAddress, err = getInterfaceMACAddress(ovsClient, gwIntf)
		if err != nil {
			return nil, fmt.Errorf("failed to get MAC address for ovs port %s: %w", gwIntf, err)
		}
	}

	res.interfaceID, err = bridgedGatewayNodeSetup(ovsClient, nodeName, res.bridgeName, physicalNetworkName)
	if err != nil {
		return nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	defaultNetConfig.PatchPort = (&util.DefaultNetInfo{}).GetNetworkScopedPatchPortName(res.bridgeName, nodeName)

	// for DPU we use the host MAC address for the Gateway configuration
	if config.IsModeDPU() {
		if err := res.setDPUHostGatewayConfiguration(nodeName); err != nil {
			return nil, err
		}
	}

	// If gwIface is set, then accelerated GW interface is present and we use it. Else use external bridge instead.
	if res.gwIface == "" {
		res.gwIface = res.bridgeName
	}

	return &res, nil
}

func (b *BridgeConfiguration) setDPUHostGatewayConfiguration(nodeName string) error {
	if b.gwIfaceRep == "" {
		b.gwIfaceRep = config.Gateway.DPUHostGatewayRepresentorInterface
	}
	if b.gwIfaceRep == "" {
		// When the DPU host representor was not provided explicitly, discover
		// it by inspecting the ports attached to the gateway bridge.
		klog.V(5).Infof("No DPU host gateway representor configured, discovering host representor from bridge %s", b.bridgeName)
		hostRep, err := util.GetDPUOps().GetDPUHostRepInterface(b.ovsClient, b.bridgeName)
		if err != nil {
			return err
		}
		b.gwIfaceRep = hostRep
	}
	macAddress, err := util.GetDPUOps().GetHostGatewayMACAddress(b.ovsClient, b.bridgeName, nodeName)
	if err != nil {
		return err
	}
	b.macAddress = macAddress
	return nil
}

// NewUnmanagedBridgeConfiguration creates a bridge configuration for an
// existing OVS bridge. The caller is responsible for provisioning the bridge
// and moving gateway IPs/MACs onto the selected host interface. hostFunction,
// when known, identifies the host PCI function that macAddress belongs to and
// is what a DPU resolves its gateway representor from.
func NewUnmanagedBridgeConfiguration(ovsClient libovsdbclient.Client, bridgeName, hostInterfaceName, nodeName,
	physicalNetworkName string, gwIPs []*net.IPNet, macAddress net.HardwareAddr,
	hostFunction *uplinkv1alpha1.HostFunction) (*BridgeConfiguration, error) {
	bridge, err := ovsops.GetBridge(ovsClient, bridgeName)
	if err != nil {
		return nil, fmt.Errorf("failed to find OVS bridge %s: %w", bridgeName, err)
	}
	if len(gwIPs) == 0 {
		return nil, fmt.Errorf("gateway IP addresses are required for OVS bridge %s", bridgeName)
	}
	if macAddress == nil {
		return nil, fmt.Errorf("gateway MAC address is required for OVS bridge %s", bridgeName)
	}
	uplinkName, err := getIntfName(ovsClient, bridgeName)
	if err != nil {
		return nil, fmt.Errorf("failed to find uplink interface for OVS bridge %s: %w", bridgeName, err)
	}
	interfaceID, err := bridgedGatewayNodeSetup(ovsClient, nodeName, bridgeName, physicalNetworkName)
	if err != nil {
		return nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	gwIface := hostInterfaceName
	var gwIfaceRep string
	if gwIface == "" || config.IsModeDPU() {
		gwIface = bridgeName
		if config.IsModeDPU() {
			gwIfaceRep, err = dpuGatewayRepresentor(ovsClient, bridge, hostFunction, macAddress, nodeName)
			if err != nil {
				return nil, err
			}
		}
	} else {
		gwIfaceRep, err = gatewayHostOVSInterface(bridgeName, gwIface)
		if err != nil {
			return nil, err
		}
	}

	return &BridgeConfiguration{
		ovsClient:   ovsClient,
		nodeName:    nodeName,
		bridgeName:  bridgeName,
		uplinkName:  uplinkName,
		gwIface:     gwIface,
		gwIfaceRep:  gwIfaceRep,
		interfaceID: interfaceID,
		ips:         gwIPs,
		macAddress:  macAddress,
		netConfig:   map[string]*BridgeUDNConfiguration{},
		eipMarkIPs:  egressip.NewMarkIPsCache(),
	}, nil
}

func (b *BridgeConfiguration) GetGatewayIface() string {
	return b.gwIface
}

func (b *BridgeConfiguration) GetGatewayIfaceRep() string {
	return b.gwIfaceRep
}

// GetStaticFDBPort returns the OVS bridge port that owns the gateway MAC.
func (b *BridgeConfiguration) GetStaticFDBPort() string {
	if b.gwIfaceRep != "" {
		return b.gwIfaceRep
	}
	return b.bridgeName
}

// UpdateInterfaceIPAddresses sets and returns the bridge's current ips
func (b *BridgeConfiguration) UpdateInterfaceIPAddresses(node *corev1.Node) ([]*net.IPNet, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	ifAddrs, err := nodeutil.GetNetworkInterfaceIPAddresses(b.GetGatewayIface())
	if err != nil {
		return nil, err
	}

	// For DPU, here we need to use the DPU host's IP address which is the tenant cluster's
	// host internal IP address instead of the DPU's external bridge IP address.
	if config.IsModeDPU() {
		nodeIfAddr, err := util.GetNodePrimaryDPUHostAddrAnnotation(node)
		if err != nil {
			return nil, err
		}
		// For DPU mode, we only support IPv4 for now.
		nodeAddrStr := nodeIfAddr.IPv4

		nodeAddr, _, err := net.ParseCIDR(nodeAddrStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse node IP address. %v", nodeAddrStr)
		}
		ifAddrs, err = nodeutil.GetDPUHostPrimaryIPAddresses(nodeAddr, ifAddrs)
		if err != nil {
			return nil, err
		}
	}

	b.ips = ifAddrs
	return ifAddrs, nil
}

// GetPortConfigurations returns a slice of Network port configurations along with the
// uplinkName and physical port's ofport value
func (b *BridgeConfiguration) GetPortConfigurations() ([]*BridgeUDNConfiguration, string, string) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	var netConfigs []*BridgeUDNConfiguration
	for _, netConfig := range b.netConfig {
		netConfigs = append(netConfigs, netConfig.ShallowCopy())
	}
	return netConfigs, b.uplinkName, b.ofPortPhys
}

// AddNetworkConfig adds the patchport and ctMark value for the provided netInfo into the bridge configuration cache
func (b *BridgeConfiguration) AddNetworkConfig(nInfo util.NetInfo, nodeSubnets, mgmtIPs []*net.IPNet, masqCTMark, pktMark uint, v6MasqIPs, v4MasqIPs *udn.MasqueradeIPs) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	netName := nInfo.GetNetworkName()
	patchPort := nInfo.GetNetworkScopedPatchPortName(b.bridgeName, b.nodeName)

	_, found := b.netConfig[netName]
	if !found {
		netConfig := &BridgeUDNConfiguration{
			PatchPort:     patchPort,
			MasqCTMark:    fmt.Sprintf("0x%x", masqCTMark),
			PktMark:       fmt.Sprintf("0x%x", pktMark),
			V4MasqIPs:     v4MasqIPs,
			V6MasqIPs:     v6MasqIPs,
			ManagementIPs: mgmtIPs,
			Subnets:       nInfo.Subnets(),
			NodeSubnets:   nodeSubnets,
			Transport:     nInfo.Transport(),
			OutboundSNAT:  nInfo.OutboundSNAT(),
		}
		netConfig.Advertised.Store(util.IsPodNetworkAdvertisedAtNode(nInfo, b.nodeName))

		b.netConfig[netName] = netConfig
	} else {
		klog.Warningf("Trying to update bridge config for network %s which already"+
			"exists in cache...networks are not mutable...ignoring update", nInfo.GetNetworkName())
	}
	return nil
}

// DelNetworkConfig deletes the provided netInfo from the bridge configuration cache
func (b *BridgeConfiguration) DelNetworkConfig(nInfo util.NetInfo) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	delete(b.netConfig, nInfo.GetNetworkName())
}

func (b *BridgeConfiguration) GetNetworkConfig(networkName string) *BridgeUDNConfiguration {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.netConfig[networkName]
}

// GetActiveNetworkBridgeConfigCopy returns a shallow copy of the network configuration corresponding to the
// provided netInfo.
//
// NOTE: if the network configuration can't be found or if the network is not patched by OVN
// yet this returns nil.
func (b *BridgeConfiguration) GetActiveNetworkBridgeConfigCopy(networkName string) *BridgeUDNConfiguration {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if netConfig, found := b.netConfig[networkName]; found && netConfig.OfPortPatch != "" {
		return netConfig.ShallowCopy()
	}
	return nil
}

func (b *BridgeConfiguration) HasNetworkConfigs() bool {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return len(b.netConfig) > 0
}

// must be called with mutex held
func (b *BridgeConfiguration) patchedNetConfigs() []*BridgeUDNConfiguration {
	result := make([]*BridgeUDNConfiguration, 0, len(b.netConfig))
	for _, netConfig := range b.netConfig {
		if netConfig.OfPortPatch == "" {
			continue
		}
		result = append(result, netConfig)
	}
	return result
}

// IsGatewayReady checks if patch ports of every netConfig are present.
// used by gateway on newGateway readyFunc
func (b *BridgeConfiguration) IsGatewayReady() bool {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	for _, netConfig := range b.netConfig {
		ready := gatewayReady(b.ovsClient, netConfig.PatchPort)
		if !ready {
			return false
		}
	}
	return true
}

func (b *BridgeConfiguration) ConfigureBridgePorts() error {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	// Get ofport of patchPort
	for _, netConfig := range b.netConfig {
		if err := netConfig.setOfPatchPort(b.ovsClient); err != nil {
			return fmt.Errorf("error setting bridge openflow ports for network with patchport %v: err: %v", netConfig.PatchPort, err)
		}
	}

	if b.uplinkName != "" {
		// Get ofport of physical interface
		ofportPhys, err := getInterfaceOfPort(b.ovsClient, b.uplinkName)
		if err != nil {
			return fmt.Errorf("failed to get ofport of %s: %v", b.uplinkName, err)
		}
		b.ofPortPhys = ofportPhys
	}

	// Get ofport representing the host. That is, the representor when present, ovsLocalPort otherwise.
	b.ofPortHost = nodetypes.OvsLocalPort
	hostOVSInterfaceName := b.bridgeName
	if b.gwIfaceRep != "" {
		ofPortHost, err := getInterfaceOfPort(b.ovsClient, b.gwIfaceRep)
		if err != nil {
			return fmt.Errorf("failed to get ofport of gateway representor %s: %v", b.gwIfaceRep, err)
		}
		b.ofPortHost = ofPortHost
		hostOVSInterfaceName = b.gwIfaceRep
	}

	// Ensure the host port on the bridge carries the configured VLAN tag when requested.
	if hostOVSInterfaceName != "" && config.Gateway.VLANID != 0 {
		iface, err := ovsops.GetOVSInterface(b.ovsClient, hostOVSInterfaceName)
		if err != nil {
			return fmt.Errorf("failed to find interface %s on bridge %s: %v", hostOVSInterfaceName, b.bridgeName, err)
		}
		ports, err := ovsops.FindOVSPortsWithPredicate(b.ovsClient, func(port *vswitchd.Port) bool {
			for _, ifaceUUID := range port.Interfaces {
				if ifaceUUID == iface.UUID {
					return true
				}
			}
			return false
		})
		if err != nil {
			return fmt.Errorf("failed to find port for interface %s on bridge %s: %v", hostOVSInterfaceName, b.bridgeName, err)
		}
		if len(ports) != 1 {
			return fmt.Errorf("failed to determine port for host interface %s on bridge %s", hostOVSInterfaceName, b.bridgeName)
		}
		tag := int(config.Gateway.VLANID)
		portUpdate := &vswitchd.Port{UUID: ports[0].UUID, Tag: &tag}
		ops, err := b.ovsClient.Where(&vswitchd.Port{UUID: ports[0].UUID}).Update(portUpdate, &portUpdate.Tag)
		if err != nil {
			return fmt.Errorf("failed to build VLAN tag update for port %s on bridge %s: %v", ports[0].Name, b.bridgeName, err)
		}
		if _, err = ovsops.TransactAndCheck(b.ovsClient, ops); err != nil {
			return fmt.Errorf("failed to set VLAN tag on port %s for bridge %s: %v", ports[0].Name, b.bridgeName, err)
		}
	}

	return nil
}

func (b *BridgeConfiguration) GetIPs() []*net.IPNet {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.ips
}

func (b *BridgeConfiguration) GetBridgeName() string {
	return b.bridgeName
}

func (b *BridgeConfiguration) GetUplinkName() string {
	return b.uplinkName
}

func (b *BridgeConfiguration) GetMAC() net.HardwareAddr {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.macAddress
}

func (b *BridgeConfiguration) SetMAC(macAddr net.HardwareAddr) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.macAddress = macAddr
}

func (b *BridgeConfiguration) SetNetworkOfPatchPort(netName string) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	netConfig, found := b.netConfig[netName]
	if !found {
		return fmt.Errorf("failed to find network %s configuration on bridge %s", netName, b.bridgeName)
	}
	if err := netConfig.setOfPatchPort(b.ovsClient); err != nil {
		return err
	}

	// Only set no-flood on bridges that also carry the default network.
	// The ARP storm occurs because CUDNs share the node IP with the
	// default GR on the same bridge.
	if netName != types.DefaultNetworkName && util.IsNetworkSegmentationSupportEnabled() {
		if _, isSharedBridge := b.netConfig[types.DefaultNetworkName]; isSharedBridge {
			if err := util.SetPortNoFlood(b.bridgeName, netConfig.OfPortPatch); err != nil {
				return fmt.Errorf("failed to set no-flood on port %s of bridge %s: %w",
					netConfig.PatchPort, b.bridgeName, err)
			}
		}
	}
	return nil
}

// SyncNoFlood ensures OFPPC_NO_FLOOD is set on every non-default patch port
// that shares the bridge with the default network. The flag is OpenFlow
// port config (not OVSDB-persisted), so it must be reapplied after
// ovs-vswitchd restarts or ports are re-created.
//
// To avoid running mod-port for every CUDN patch port on every sync
// cycle, we first query dump-ports-desc once to learn which ports
// already carry the flag and only touch ports that are missing it.
func (b *BridgeConfiguration) SyncNoFlood() error {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return nil
	}
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if _, hasDefault := b.netConfig[types.DefaultNetworkName]; !hasDefault {
		return nil
	}

	alreadyNoFlood, err := util.GetNoFloodPorts(b.bridgeName)
	if err != nil {
		return fmt.Errorf("failed to check no-flood state on bridge %s: %w", b.bridgeName, err)
	}

	for netName, netConfig := range b.netConfig {
		if netName == types.DefaultNetworkName || netConfig.PatchPort == "" || netConfig.OfPortPatch == "" {
			continue
		}
		if alreadyNoFlood[netConfig.OfPortPatch] {
			continue
		}
		if err := util.SetPortNoFlood(b.bridgeName, netConfig.OfPortPatch); err != nil {
			return fmt.Errorf("failed to set no-flood on port %s of bridge %s: %w",
				netConfig.PatchPort, b.bridgeName, err)
		}
	}
	return nil
}

func (b *BridgeConfiguration) GetInterfaceID() string {
	return b.interfaceID
}

func (b *BridgeConfiguration) GetOfPortHost() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.ofPortHost
}

func (b *BridgeConfiguration) GetEIPMarkIPs() *egressip.MarkIPsCache {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.eipMarkIPs
}

func (b *BridgeConfiguration) SetEIPMarkIPs(eipMarkIPs *egressip.MarkIPsCache) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.eipMarkIPs = eipMarkIPs
}

func (b *BridgeConfiguration) SetDropGARP(drop bool) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.dropGARP = drop
}

func gatewayReady(ovsClient libovsdbclient.Client, patchPort string) bool {
	// Get ofport of patchPort
	ofport, err := getInterfaceOfPortIfExists(ovsClient, patchPort, true)
	if err != nil || len(ofport) == 0 {
		return false
	}
	klog.Info("Gateway is ready")
	return true
}

// getIntfName returns the physical uplink interface of the gateway OVS bridge
// gatewayIntf, as derived by util.GetNicName (external-ids:bridge-uplink,
// single system-type port, or the "br<nic>" name convention), and verifies
// that the result is plugged into OVS (has an ofport).
func getIntfName(ovsClient libovsdbclient.Client, gatewayIntf string) (string, error) {
	// The given (or autodetected) interface is an OVS bridge and this could be
	// created by us using util.NicToBridge() or it was pre-created by the user.

	// Is intfName a port of gatewayIntf?
	intfName, err := util.GetNicName(ovsClient, gatewayIntf)
	if err != nil {
		return "", err
	}
	_, err = getInterfaceOfPort(ovsClient, intfName)
	if err != nil {
		return "", fmt.Errorf("failed to get ofport of %s: %v", intfName, err)
	}
	return intfName, nil
}

// bridgedGatewayNodeSetup enables forwarding on bridge interface, sets up the physical network name mappings for the bridge,
// and returns an ifaceID created from the bridge name and the node name
func bridgedGatewayNodeSetup(ovsClient libovsdbclient.Client, nodeName, bridgeName, physicalNetworkName string) (string, error) {
	err := util.SetForwardingModeForInterface(bridgeName)
	if err != nil {
		return "", err
	}

	bridgeMappingsMutex.Lock()
	defer bridgeMappingsMutex.Unlock()

	// ovn-bridge-mappings maps a physical network name to a local ovs bridge
	// that provides connectivity to that network. It is in the form of physnet1:br1,physnet2:br2.
	// Note that there may be multiple ovs bridge mappings, be sure not to override
	// the mappings for the other physical network
	existing := ""
	if ovs, err := ovsops.GetOpenvSwitch(ovsClient); err != nil {
		if !errors.Is(err, libovsdbclient.ErrNotFound) {
			return "", fmt.Errorf("failed to get Open_vSwitch row: %w", err)
		}
		// no row yet => no existing mappings
	} else {
		existing = ovs.ExternalIDs["ovn-bridge-mappings"]
	}

	// skip the existing mapping setting for the specified physicalNetworkName
	mapString := ""
	if existing != "" {
		for _, bridgeMapping := range strings.Split(existing, ",") {
			m := strings.Split(bridgeMapping, ":")
			if network := m[0]; network != physicalNetworkName {
				if len(mapString) != 0 {
					mapString += ","
				}
				mapString += bridgeMapping
			}
		}
	}
	if len(mapString) != 0 {
		mapString += ","
	}
	mapString += physicalNetworkName + ":" + bridgeName

	if err := ovsops.UpdateOpenvSwitchExternalIDs(ovsClient, map[string]string{
		"ovn-bridge-mappings": mapString,
	}); err != nil {
		return "", fmt.Errorf("failed to set ovn-bridge-mappings for ovs bridge %s: %w", bridgeName, err)
	}

	ifaceID := bridgeName + "_" + nodeName
	return ifaceID, nil
}

func getRepresentor(intfName string) (string, error) {
	return util.GetNetdeviceRepresentorName(intfName)
}

// dpuGatewayRepresentor selects the DPU-side representor that carries the
// gateway MAC on bridge. Uplink host interfaces may be backed by a VF or SF,
// not just a PF, so the bridge cannot be assumed to hold a single PF
// representor. The host function published by the DPU-host is the primary key:
// the host owns the MAC of its VFs and SFs, so the DPU eswitch usually reports
// no function MAC for the matching representor and a scan by host MAC cannot
// find it. That scan stays as the fallback for uplinks whose host function
// could not be published.
func dpuGatewayRepresentor(ovsClient libovsdbclient.Client, bridge *vswitchd.Bridge,
	hostFunction *uplinkv1alpha1.HostFunction, macAddress net.HardwareAddr,
	nodeName string) (string, error) {
	var functionErr error
	if hostFunction != nil {
		rep, err := util.FindHostRepresentorByFunction(hostFunction.PFID, hostFunction.VFID, macAddress, nodeName)
		if err == nil {
			err = representorOnBridge(ovsClient, bridge, rep)
		}
		if err == nil {
			klog.Infof("Bridge %s: host function %s resolved gateway representor %s", bridge.Name,
				util.HostFunctionName(hostFunction.PFID, hostFunction.VFID), rep)
			return rep, nil
		}
		functionErr = err
		klog.Warningf("Bridge %s: host function %s did not resolve a gateway representor, "+
			"falling back to the host MAC scan: %v",
			bridge.Name, util.HostFunctionName(hostFunction.PFID, hostFunction.VFID), err)
	}

	rep, err := util.GetDPUOps().FindHostRepresentorByPeerMAC(ovsClient, bridge, macAddress, nodeName)
	if err != nil && functionErr != nil {
		return "", fmt.Errorf("%w; host function %s did not resolve one either: %w", err,
			util.HostFunctionName(hostFunction.PFID, hostFunction.VFID), functionErr)
	}
	return rep, err
}

// representorOnBridge errors unless rep is attached to bridge, either as a
// port of its own or as an interface of another port (e.g. a bond member),
// the same membership the uplink discovery accepts when it resolves the
// bridge from the representor.
func representorOnBridge(ovsClient libovsdbclient.Client, bridge *vswitchd.Bridge, rep string) error {
	ports, err := ovsops.FindPortsForPortOrInterface(ovsClient, rep)
	if err != nil {
		return fmt.Errorf("failed to look up representor %s: %w", rep, err)
	}
	for _, port := range ports {
		if slices.Contains(bridge.Ports, port.UUID) {
			return nil
		}
	}
	return fmt.Errorf("representor %s is not attached to OVS bridge %s", rep, bridge.Name)
}

func gatewayHostOVSInterface(bridgeName, gwIface string) (string, error) {
	if gwIface == "" || gwIface == bridgeName {
		return "", nil
	}

	if bridgeForInterface, _, err := util.RunOVSVsctl("port-to-br", gwIface); err == nil {
		bridgeForInterface = strings.TrimSpace(bridgeForInterface)
		if bridgeForInterface == bridgeName {
			return gwIface, nil
		}
		if bridgeForInterface != "" {
			return "", fmt.Errorf("gateway interface %s belongs to OVS bridge %s, expected %s",
				gwIface, bridgeForInterface, bridgeName)
		}
	}

	gwIfaceRep, err := getRepresentor(gwIface)
	if err != nil {
		return "", fmt.Errorf("gateway interface %s is not the OVS bridge interface, an OVS port on bridge %s, or an accelerated VF/SF netdevice: %w",
			gwIface, bridgeName, err)
	}
	bridgeForRep, stderr, err := util.RunOVSVsctl("port-to-br", gwIfaceRep)
	if err != nil {
		return "", fmt.Errorf("failed to resolve OVS bridge for representor %s of gateway interface %s, stderr: %q, error: %w",
			gwIfaceRep, gwIface, stderr, err)
	}
	bridgeForRep = strings.TrimSpace(bridgeForRep)
	if bridgeForRep != bridgeName {
		return "", fmt.Errorf("representor %s of gateway interface %s belongs to OVS bridge %s, expected %s",
			gwIfaceRep, gwIface, bridgeForRep, bridgeName)
	}
	return gwIfaceRep, nil
}

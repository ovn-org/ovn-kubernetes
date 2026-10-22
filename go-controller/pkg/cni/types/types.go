// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"net"

	cnitypes "github.com/containernetworking/cni/pkg/types"
)

// NetConf is CNI NetConf with DeviceID
type NetConf struct {
	cnitypes.NetConf
	// Role is valid only on L3 / L2 topologies. Not on localnet.
	// It allows for using this network to be either secondary or
	// primary user defined network for the pod.
	// primary user defined networks are used in order to achieve
	// native network isolation.
	// In order to ensure backwards compatibility, if empty the
	// network is considered secondary
	Role string `json:"role,omitempty"`
	// specifies the OVN topology for this network configuration
	// when not specified, by default it is Layer3AttachDefTopoType
	Topology string `json:"topology,omitempty"`
	// captures net-attach-def name in the form of namespace/name
	NADName string `json:"netAttachDefName,omitempty"`
	// Network MTU
	MTU int `json:"mtu,omitempty"`
	// comma-separated subnet cidr
	// for secondary layer3 network, eg. 10.128.0.0/14/23
	// for layer2 and localnet network, eg. 10.1.130.0/24
	Subnets string `json:"subnets,omitempty"`
	// comma-separated list of subnets, to be excluded from being allocated for Pod
	// valid for layer2 and localnet network topology
	// eg. "10.1.130.0/27, 10.1.130.122/32"
	ExcludeSubnets string `json:"excludeSubnets,omitempty"`
	// comma-separated list of subnets, to be reserved for static IP assignment
	// valid for layer2 topology only
	// eg. "10.1.130.0/27, 10.1.130.122/32"
	ReservedSubnets string `json:"reservedSubnets,omitempty"`
	// comma-separated list of subnets, to be reserved for OVN-Kubernetes internal network infrastructure
	// valid for layer2 network topology with primary role only
	// eg. "10.1.130.0/30, 10.1.130.64/30"
	InfrastructureSubnets string `json:"infrastructureSubnets,omitempty"`
	// join subnet cidr is required for supporting
	// services and ingress for user defined networks
	// in case of dualstack cluster, please do a comma-separated list
	// expected format:
	// 1) V4 single stack: "v4CIDR" (eg: "100.65.0.0/16")
	// 2) V6 single stack: "v6CIDR" (eg: "fd99::/64")
	// 3) dualstack: "v4CIDR,v6CIDR" (eg: "100.65.0.0/16,fd99::/64")
	// valid for UDN layer3/layer2 network topology
	// default value: 100.65.0.0/16,fd99::/64 if not provided
	JoinSubnet string `json:"joinSubnet,omitempty"`
	// transit subnet cidr was previously internally set to the default value,
	// but with the recent layer2 topology changes it may overlap with the network Subnet.
	// To avoid that, transit subnet is now configurable. Only used by Primary Layer2 networks.
	// in case of dualstack cluster, please do a comma-separated list
	TransitSubnet string `json:"transitSubnet,omitempty"`
	// comma-separated list of default gateway IPs for layer2 primary networks
	// in case of dualstack cluster, please do a comma-separated list
	// expected format:
	// 1) V4 single stack: "10.128.0.1"
	// 2) V6 single stack: "2001:db8::1"
	// 3) dualstack: "10.128.0.1,2001:db8::1"
	// valid for layer2 primary network topology only
	// when omitted, the .1 address from the subnet is used
	DefaultGatewayIPs string `json:"defaultGatewayIPs,omitempty"`
	// VLANID, valid in localnet topology network only
	VLANID int `json:"vlanID,omitempty"`
	// AllowPersistentIPs is valid on both localnet / layer topologies.
	// It allows for having IP allocations that outlive the pod for which
	// they are originally created - e.g. a KubeVirt VM's migration, or
	// restart.
	AllowPersistentIPs bool `json:"allowPersistentIPs,omitempty"`

	// PhysicalNetworkName indicates the name of the physical network to which
	// the OVN overlay will connect. Only applies to `localnet` topologies.
	// When omitted, the physical network name of the network will be the value
	// of the `name` attribute.
	// This attribute allows multiple overlays to share the same physical
	// network mapping in the hosts.
	PhysicalNetworkName string `json:"physicalNetworkName,omitempty"`

	// MACSecurityMode controls whether MAC spoof protection is enforced on logical
	// switch ports attached to this network.
	// Valid values:
	// - `enabled` (default)
	// - `disabled`
	// When omitted, MAC security mode is enabled (default, preserves current behavior).
	// Valid for `secondary` `layer2` and `localnet` networks when IPAM is disabled.
	MACSecurityMode string `json:"macSecurityMode,omitempty"`

	// Transport describes the transport protocol for east-west traffic.
	// Valid values are "no-overlay" and "evpn".
	// When omitted, the default OVN overlay transport is used.
	Transport string `json:"transport,omitempty"`

	// OutboundSNAT configures SNAT behavior for outbound traffic from pods
	// on user-defined networks in no-overlay mode.
	// Valid values are "enabled" and "disabled".
	// Only valid when Transport is "no-overlay".
	OutboundSNAT string `json:"outboundSNAT,omitempty"`

	// NoOverlayRouting configures whether pod network routing is managed by
	// OVN-Kubernetes or externally when using no-overlay transport.
	// Valid values are "managed" and "unmanaged".
	// Only valid when Transport is "no-overlay".
	NoOverlayRouting string `json:"noOverlayRouting,omitempty"`

	// EVPNConfig contains configuration for EVPN mode.
	// Only valid when Transport is "evpn".
	EVPN *EVPNConfig `json:"evpn,omitempty"`

	// Uplink is the name of the Uplink resource used for this network's
	// external connectivity.
	Uplink string `json:"uplink,omitempty"`

	// PciAddrs in case of using sriov or Auxiliry device name in case of SF
	DeviceID string `json:"deviceID,omitempty"`
	// LogFile to log all the messages from cni shim binary to
	LogFile string `json:"logFile,omitempty"`
	// Level is the logging verbosity level
	LogLevel string `json:"logLevel,omitempty"`
	// LogFileMaxSize is the maximum size in bytes of the logfile
	// before it gets rolled.
	LogFileMaxSize int `json:"logfile-maxsize"`
	// LogFileMaxBackups represents the maximum number of
	// old log files to retain
	LogFileMaxBackups int `json:"logfile-maxbackups"`
	// LogFileMaxAge represents the maximum number
	// of days to retain old log files
	LogFileMaxAge int `json:"logfile-maxage"`
	// Runtime arguments passed by the NPWG implementation (e.g. multus)
	RuntimeConfig struct {
		// see https://github.com/k8snetworkplumbingwg/device-info-spec
		CNIDeviceInfoFile string `json:"CNIDeviceInfoFile,omitempty"`
	} `json:"runtimeConfig,omitempty"`
}

// MarshalJSON overrides the promoted PluginConf.MarshalJSON which only marshals
// base CNI fields and silently drops OVN-specific fields. containernetworking/cni
// v1.3.0 changed types.NetConf from a distinct type to a type alias for PluginConf,
// causing PluginConf.MarshalJSON to be promoted into any struct embedding types.NetConf.
func (n NetConf) MarshalJSON() ([]byte, error) {
	// cniConf is a new type with the same layout as cnitypes.PluginConf but without
	// its MarshalJSON method, so embedding it uses standard struct marshaling.
	type cniConf cnitypes.PluginConf
	type netConf struct {
		cniConf
		Role                  string      `json:"role,omitempty"`
		Topology              string      `json:"topology,omitempty"`
		NADName               string      `json:"netAttachDefName,omitempty"`
		MTU                   int         `json:"mtu,omitempty"`
		Subnets               string      `json:"subnets,omitempty"`
		ExcludeSubnets        string      `json:"excludeSubnets,omitempty"`
		ReservedSubnets       string      `json:"reservedSubnets,omitempty"`
		InfrastructureSubnets string      `json:"infrastructureSubnets,omitempty"`
		JoinSubnet            string      `json:"joinSubnet,omitempty"`
		TransitSubnet         string      `json:"transitSubnet,omitempty"`
		DefaultGatewayIPs     string      `json:"defaultGatewayIPs,omitempty"`
		VLANID                int         `json:"vlanID,omitempty"`
		AllowPersistentIPs    bool        `json:"allowPersistentIPs,omitempty"`
		PhysicalNetworkName   string      `json:"physicalNetworkName,omitempty"`
		MACSecurityMode       string      `json:"macSecurityMode,omitempty"`
		Transport             string      `json:"transport,omitempty"`
		OutboundSNAT          string      `json:"outboundSNAT,omitempty"`
		NoOverlayRouting      string      `json:"noOverlayRouting,omitempty"`
		EVPN                  *EVPNConfig `json:"evpn,omitempty"`
		Uplink                string      `json:"uplink,omitempty"`
		DeviceID              string      `json:"deviceID,omitempty"`
		LogFile               string      `json:"logFile,omitempty"`
		LogLevel              string      `json:"logLevel,omitempty"`
		LogFileMaxSize        int         `json:"logfile-maxsize"`
		LogFileMaxBackups     int         `json:"logfile-maxbackups"`
		LogFileMaxAge         int         `json:"logfile-maxage"`
		RuntimeConfig         struct {
			CNIDeviceInfoFile string `json:"CNIDeviceInfoFile,omitempty"`
		} `json:"runtimeConfig,omitempty"`
	}
	return json.Marshal(netConf{
		cniConf:               cniConf(n.NetConf),
		Role:                  n.Role,
		Topology:              n.Topology,
		NADName:               n.NADName,
		MTU:                   n.MTU,
		Subnets:               n.Subnets,
		ExcludeSubnets:        n.ExcludeSubnets,
		ReservedSubnets:       n.ReservedSubnets,
		InfrastructureSubnets: n.InfrastructureSubnets,
		JoinSubnet:            n.JoinSubnet,
		TransitSubnet:         n.TransitSubnet,
		DefaultGatewayIPs:     n.DefaultGatewayIPs,
		VLANID:                n.VLANID,
		AllowPersistentIPs:    n.AllowPersistentIPs,
		PhysicalNetworkName:   n.PhysicalNetworkName,
		MACSecurityMode:       n.MACSecurityMode,
		Transport:             n.Transport,
		OutboundSNAT:          n.OutboundSNAT,
		NoOverlayRouting:      n.NoOverlayRouting,
		EVPN:                  n.EVPN,
		Uplink:                n.Uplink,
		DeviceID:              n.DeviceID,
		LogFile:               n.LogFile,
		LogLevel:              n.LogLevel,
		LogFileMaxSize:        n.LogFileMaxSize,
		LogFileMaxBackups:     n.LogFileMaxBackups,
		LogFileMaxAge:         n.LogFileMaxAge,
		RuntimeConfig:         n.RuntimeConfig,
	})
}

// EVPNConfig contains EVPN-specific configuration for the network.
type EVPNConfig struct {
	// VTEP is the name of the VTEP CR that defines VTEP IPs for EVPN.
	VTEP string `json:"vtep"`
	// MACVRF contains the MAC-VRF configuration for Layer 2 EVPN.
	MACVRF *VRFConfig `json:"macVRF,omitempty"`
	// IPVRF contains the IP-VRF configuration for Layer 3 EVPN.
	IPVRF *VRFConfig `json:"ipVRF,omitempty"`
}

// VRFConfig contains configuration for a VRF in EVPN.
type VRFConfig struct {
	// VNI is the VXLAN Network Identifier for this VRF.
	VNI int32 `json:"vni"`
	// RouteTarget is the BGP route target for this VRF.
	RouteTarget string `json:"routeTarget,omitempty"`
	// VID is the VLAN ID used for local traffic segmentation on each node.
	// Allocated cluster-wide by the UDN controller, one per VRF.
	VID int `json:"vid,omitempty"`
}

// NetworkSelectionElement represents one element of the JSON format
// Network Attachment Selection Annotation as described in section 4.1.2
// of the CRD specification.
type NetworkSelectionElement struct {
	// Name contains the name of the Network object this element selects
	Name string `json:"name"`
	// Namespace contains the optional namespace that the network referenced
	// by Name exists in
	Namespace string `json:"namespace,omitempty"`
	// MacRequest contains an optional requested MAC address for this
	// network attachment
	MacRequest string `json:"mac,omitempty"`
	// GatewayRequest contains default route IP address for the pod
	GatewayRequest []net.IP `json:"default-route,omitempty"`
}

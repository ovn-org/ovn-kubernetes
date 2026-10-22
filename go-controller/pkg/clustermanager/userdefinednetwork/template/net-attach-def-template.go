// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	OvnK8sCNIOverlay = "ovn-k8s-cni-overlay"

	FinalizerUserDefinedNetwork = "k8s.ovn.org/user-defined-network-protection"
	LabelUserDefinedNetwork     = "k8s.ovn.org/user-defined-network"
)

type SpecGetter interface {
	GetTopology() userdefinednetworkv1.NetworkTopology
	GetLayer3() *userdefinednetworkv1.Layer3Config
	GetLayer2() *userdefinednetworkv1.Layer2Config
	GetLocalnet() *userdefinednetworkv1.LocalnetConfig
	GetTransport() userdefinednetworkv1.TransportOption
	GetEVPN() *userdefinednetworkv1.EVPNConfig
	GetNoOverlay() *userdefinednetworkv1.NoOverlayConfig
}

func RenderNetAttachDefManifest(obj client.Object, targetNamespace string, opts ...RenderOption) (*netv1.NetworkAttachmentDefinition, error) {
	if obj == nil {
		return nil, nil
	}

	if targetNamespace == "" {
		return nil, fmt.Errorf("namespace should not be empty")
	}

	var ownerRef metav1.OwnerReference
	var spec SpecGetter
	var networkName string
	var uplink string
	switch o := obj.(type) {
	case *userdefinednetworkv1.UserDefinedNetwork:
		ownerRef = *metav1.NewControllerRef(obj, userdefinednetworkv1.SchemeGroupVersion.WithKind("UserDefinedNetwork"))
		spec = &o.Spec
		networkName = util.GenerateUDNNetworkName(targetNamespace, obj.GetName())
	case *userdefinednetworkv1.ClusterUserDefinedNetwork:
		ownerRef = *metav1.NewControllerRef(obj, userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork"))
		spec = &o.Spec.Network
		networkName = util.GenerateCUDNNetworkName(obj.GetName())
		if len(o.Spec.Uplinks) > 0 {
			uplink = o.Spec.Uplinks[0]
		}
	default:
		return nil, fmt.Errorf("unknown type %T", obj)
	}

	nadName := util.GetNADName(targetNamespace, obj.GetName())

	nadSpec, err := renderNADSpec(networkName, nadName, spec, uplink, applyOptions(opts))
	if err != nil {
		return nil, err
	}

	return &netv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:            obj.GetName(),
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Labels:          renderNADLabels(obj),
			Annotations:     renderNADAnnotations(obj),
			Finalizers:      []string{FinalizerUserDefinedNetwork},
		},
		Spec: *nadSpec,
	}, nil
}

func renderNADSpec(networkName, nadName string, spec SpecGetter, uplink string, opts *RenderOptions) (*netv1.NetworkAttachmentDefinitionSpec, error) {
	if err := validateTopology(spec); err != nil {
		return nil, fmt.Errorf("invalid topology specified: %w", err)
	}

	cniNetConf, err := renderCNINetworkConfig(networkName, nadName, spec, uplink, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to render CNI network config: %w", err)
	}

	cniNetConfRaw, err := json.Marshal(cniNetConf)
	if err != nil {
		return nil, err
	}

	return &netv1.NetworkAttachmentDefinitionSpec{
		Config: string(cniNetConfRaw),
	}, nil
}

// renderNADLabels copies labels from UDN to help renderNADSpec
// function add those labels to corresponding NAD
func renderNADLabels(obj client.Object) map[string]string {
	labels := make(map[string]string)
	labels[LabelUserDefinedNetwork] = ""
	udnLabels := obj.GetLabels()
	if len(udnLabels) != 0 {
		maps.Copy(labels, udnLabels)
	}
	return labels
}

// renderNADAnnotations copies annotations from UDN to corresponding NAD
func renderNADAnnotations(obj client.Object) map[string]string {
	udnAnnotations := obj.GetAnnotations()
	annotations := make(map[string]string)
	for k, v := range udnAnnotations {
		if !strings.HasPrefix(k, types.OvnK8sPrefix) {
			annotations[k] = v
		}
	}
	if len(annotations) == 0 {
		return nil
	}
	return annotations
}

func validateTopology(spec SpecGetter) error {
	if spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLayer3 && spec.GetLayer3() == nil ||
		spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLayer2 && spec.GetLayer2() == nil ||
		spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLocalnet && spec.GetLocalnet() == nil {
		return config.NewTopologyConfigMismatchError(string(spec.GetTopology()))
	}
	return nil
}

func renderCNINetworkConfig(networkName, nadName string, spec SpecGetter, uplink string, opts *RenderOptions) (map[string]interface{}, error) {
	netConfSpec := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			CNIVersion: config.CNISpecVersion,
			Type:       OvnK8sCNIOverlay,
			Name:       networkName,
		},
		NADName:   nadName,
		Topology:  strings.ToLower(string(spec.GetTopology())),
		Transport: transportFromCRD(spec.GetTransport()),
		Uplink:    uplink,
	}

	switch spec.GetTopology() {
	case userdefinednetworkv1.NetworkTopologyLayer3:
		cfg := spec.GetLayer3()
		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = int(cfg.MTU)
		netConfSpec.Subnets = layer3SubnetsString(cfg.Subnets)
		netConfSpec.JoinSubnet = cidrString(renderJoinSubnets(cfg.Role, cfg.JoinSubnets))
	case userdefinednetworkv1.NetworkTopologyLayer2:
		cfg := spec.GetLayer2()
		if err := validateIPAM(cfg.IPAM); err != nil {
			return nil, err
		}
		if ipamEnabled(cfg.IPAM) && len(cfg.Subnets) == 0 {
			return nil, config.NewSubnetsRequiredError()
		}
		if !ipamEnabled(cfg.IPAM) && len(cfg.Subnets) > 0 {
			return nil, config.NewSubnetsMustBeUnsetError()
		}

		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = int(cfg.MTU)
		netConfSpec.MACSecurityMode = macSecurityModeFromCRD(cfg.MACSecurity)
		netConfSpec.AllowPersistentIPs = cfg.IPAM != nil && cfg.IPAM.Lifecycle == userdefinednetworkv1.IPAMLifecyclePersistent
		netConfSpec.Subnets = cidrString(cfg.Subnets)
		if util.IsPreconfiguredUDNAddressesEnabled() {
			netConfSpec.ReservedSubnets = cidrString(cfg.ReservedSubnets)
			netConfSpec.InfrastructureSubnets = cidrString(cfg.InfrastructureSubnets)
			netConfSpec.DefaultGatewayIPs = ipString(cfg.DefaultGatewayIPs)
		}
		netConfSpec.JoinSubnet = cidrString(renderJoinSubnets(cfg.Role, cfg.JoinSubnets))
		// now generate transit subnet for layer2 topology
		if cfg.Role == userdefinednetworkv1.NetworkRolePrimary {
			err := util.SetTransitSubnets(netConfSpec)
			if err != nil {
				return nil, err
			}
		}
	case userdefinednetworkv1.NetworkTopologyLocalnet:
		cfg := spec.GetLocalnet()
		if err := validateIPAM(cfg.IPAM); err != nil {
			return nil, err
		}
		if ipamEnabled(cfg.IPAM) && len(cfg.Subnets) == 0 {
			return nil, config.NewSubnetsRequiredError()
		}
		if !ipamEnabled(cfg.IPAM) && len(cfg.Subnets) > 0 {
			return nil, config.NewSubnetsMustBeUnsetError()
		}
		if cfg.IPAM != nil && cfg.IPAM.Mode == userdefinednetworkv1.IPAMDHCP {
			netConfSpec.IPAM.Type = types.IPAMTypeDHCP
		}
		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = localnetMTU(cfg.MTU)
		netConfSpec.MACSecurityMode = macSecurityModeFromCRD(cfg.MACSecurity)
		netConfSpec.AllowPersistentIPs = cfg.IPAM != nil && cfg.IPAM.Lifecycle == userdefinednetworkv1.IPAMLifecyclePersistent
		netConfSpec.Subnets = cidrString(cfg.Subnets)
		netConfSpec.ExcludeSubnets = cidrString(cfg.ExcludeSubnets)
		netConfSpec.PhysicalNetworkName = cfg.PhysicalNetworkName

		if cfg.VLAN != nil && cfg.VLAN.Access != nil {
			netConfSpec.VLANID = int(cfg.VLAN.Access.ID)
		}
	}

	if spec.GetTransport() == userdefinednetworkv1.TransportOptionEVPN {
		if !util.IsEVPNEnabled() {
			return nil, fmt.Errorf("EVPN transport requested but EVPN feature is not enabled")
		}
		netConfSpec.EVPN = renderEVPNConfig(spec, opts)
	}

	if spec.GetTransport() == userdefinednetworkv1.TransportOptionNoOverlay {
		noOverlayCfg := spec.GetNoOverlay()
		if noOverlayCfg != nil {
			// Convert CRD SNATOption enum ("Enabled"/"Disabled") to internal format ("enabled"/"disabled")
			netConfSpec.OutboundSNAT = strings.ToLower(string(noOverlayCfg.OutboundSNAT))
			netConfSpec.NoOverlayRouting = noOverlayRoutingFromCRD(noOverlayCfg.Routing)
		}
	}

	if netConfSpec.AllowPersistentIPs && !config.OVNKubernetesFeature.EnablePersistentIPs {
		return nil, fmt.Errorf("allowPersistentIPs is set but persistentIPs is Disabled")
	}

	if err := util.ValidateNetConf(nadName, netConfSpec); err != nil {
		return nil, err
	}

	if _, err := util.NewNetInfo(netConfSpec); err != nil {
		return nil, err
	}

	// Since 'ovncnitypes.NetConf' type and its embedded 'cnitypes.NetConf' type has
	// parameters that defined with 'ommitempty' JSON tag option but not as pointer,
	// they will always present in the marshaed JSON, making the UDN NAD spec config
	// having unexpected fields (e.g.:IPAM, RuntimeConfig).
	// Generating the net-conf JSON string using 'map[string]struct{}' provide the
	// expected result.
	cniNetConf := map[string]interface{}{
		"cniVersion":       config.CNISpecVersion,
		"type":             OvnK8sCNIOverlay,
		"name":             networkName,
		"netAttachDefName": nadName,
		"topology":         netConfSpec.Topology,
		"role":             netConfSpec.Role,
	}
	if mtu := netConfSpec.MTU; mtu > 0 {
		cniNetConf["mtu"] = mtu
	}
	if len(netConfSpec.JoinSubnet) > 0 {
		cniNetConf["joinSubnet"] = netConfSpec.JoinSubnet
	}
	if len(netConfSpec.TransitSubnet) > 0 {
		cniNetConf["transitSubnet"] = netConfSpec.TransitSubnet
	}
	if len(netConfSpec.Subnets) > 0 {
		cniNetConf["subnets"] = netConfSpec.Subnets
	}
	if netConfSpec.AllowPersistentIPs {
		cniNetConf["allowPersistentIPs"] = netConfSpec.AllowPersistentIPs
	}
	if netConfSpec.PhysicalNetworkName != "" {
		cniNetConf["physicalNetworkName"] = netConfSpec.PhysicalNetworkName
	}
	// The Enabled/Disabled modes never set this field as OVN-Kubernetes
	// handles those itself, inferring them from the presence of subnets.
	// An explicit ipam section is emitted only when addressing is
	// delegated to another IPAM plugin, and DHCP is the only such
	// supported mode today.
	if netConfSpec.IPAM.Type != "" {
		cniNetConf["ipam"] = map[string]interface{}{
			"type": netConfSpec.IPAM.Type,
		}
	}
	if len(netConfSpec.ExcludeSubnets) > 0 {
		cniNetConf["excludeSubnets"] = netConfSpec.ExcludeSubnets
	}

	if netConfSpec.VLANID != 0 {
		cniNetConf["vlanID"] = netConfSpec.VLANID
	}
	if util.IsPreconfiguredUDNAddressesEnabled() {
		if len(netConfSpec.ReservedSubnets) > 0 {
			cniNetConf["reservedSubnets"] = netConfSpec.ReservedSubnets
		}
		if len(netConfSpec.InfrastructureSubnets) > 0 {
			cniNetConf["infrastructureSubnets"] = netConfSpec.InfrastructureSubnets
		}
		if len(netConfSpec.DefaultGatewayIPs) > 0 {
			cniNetConf["defaultGatewayIPs"] = netConfSpec.DefaultGatewayIPs
		}
	}

	if netConfSpec.Transport != "" {
		cniNetConf["transport"] = netConfSpec.Transport
	}
	if netConfSpec.OutboundSNAT != "" {
		cniNetConf["outboundSNAT"] = netConfSpec.OutboundSNAT
	}
	if netConfSpec.NoOverlayRouting != "" {
		cniNetConf["noOverlayRouting"] = netConfSpec.NoOverlayRouting
	}
	if netConfSpec.EVPN != nil {
		cniNetConf["evpn"] = netConfSpec.EVPN
	}
	if netConfSpec.Uplink != "" {
		cniNetConf["uplink"] = netConfSpec.Uplink
	}
	if netConfSpec.MACSecurityMode != "" {
		cniNetConf["macSecurityMode"] = netConfSpec.MACSecurityMode
	}

	return cniNetConf, nil
}

// transportFromCRD converts CRD PascalCase format to canonical format.
// CRD format uses PascalCase: "NoOverlay", "EVPN"; empty string means default OVN transport.
// Returns canonical lowercase format: "no-overlay", "evpn", or "" for default.
func transportFromCRD(crdTransport userdefinednetworkv1.TransportOption) string {
	switch crdTransport {
	case userdefinednetworkv1.TransportOptionNoOverlay:
		return types.NetworkTransportNoOverlay
	case userdefinednetworkv1.TransportOptionEVPN:
		return types.NetworkTransportEVPN
	default:
		return "" // empty string means default OVN transport; kubebuilder prevents unknown values
	}
}

// macSecurityModeFromCRD converts the UDN API's MACSecurityConfig mode to NetConf API macSecurityMode values.
func macSecurityModeFromCRD(macSecurity *userdefinednetworkv1.MACSecurityConfig) string {
	if macSecurity == nil {
		return ""
	}

	switch macSecurity.Mode {
	case userdefinednetworkv1.MACSecurityDisabled:
		return types.MACSecurityModeDisabled
	case userdefinednetworkv1.MACSecurityEnabled:
		return types.MACSecurityModeEnabled
	default:
		return ""
	}
}

func noOverlayRoutingFromCRD(crdRouting userdefinednetworkv1.RoutingOption) string {
	switch crdRouting {
	case userdefinednetworkv1.RoutingManaged:
		return config.NoOverlayRoutingManaged
	case userdefinednetworkv1.RoutingUnmanaged:
		return config.NoOverlayRoutingUnmanaged
	default:
		return ""
	}
}

func localnetMTU(desiredMTU int32) int {
	// The MTU for localnet topology should be as the default MTU (1500) because the underlay
	// is not part of the SDN and compensating for the SDN overhead (100) is not required.
	mtu := config.Default.MTU + 100
	if desiredMTU > 0 {
		mtu = int(desiredMTU)
	}

	return mtu
}

func ipamEnabled(ipam *userdefinednetworkv1.IPAMConfig) bool {
	return ipam == nil || ipam.Mode == "" || ipam.Mode == userdefinednetworkv1.IPAMEnabled
}

func validateIPAM(ipam *userdefinednetworkv1.IPAMConfig) error {
	if ipam == nil {
		return nil
	}
	if ipam.Lifecycle == userdefinednetworkv1.IPAMLifecyclePersistent && !ipamEnabled(ipam) {
		return config.NewIPAMLifecycleNotSupportedError()
	}
	return nil
}

func renderJoinSubnets(role userdefinednetworkv1.NetworkRole, joinSubnetes []userdefinednetworkv1.CIDR) []userdefinednetworkv1.CIDR {
	if role != userdefinednetworkv1.NetworkRolePrimary {
		return nil
	}

	if len(joinSubnetes) == 0 {
		return []userdefinednetworkv1.CIDR{types.UserDefinedPrimaryNetworkJoinSubnetV4, types.UserDefinedPrimaryNetworkJoinSubnetV6}
	}

	return joinSubnetes
}

// layer3SubnetsString converts Layer3Subnet slice to comma seperated string
// (e.g.: "10.100.0.0/24/16, 10.200.0.0/24, ...").
// In case a Layer3Subent's HostSubnet is '0' or not specified it will not be
// appended becase it will result in an invalid format (e.g.: "10.200.0.0/24/0").
func layer3SubnetsString(subnets []userdefinednetworkv1.Layer3Subnet) string {
	var cidrs []string
	for _, subnet := range subnets {
		if subnet.HostSubnet > 0 {
			cidrs = append(cidrs, fmt.Sprintf("%s/%d", subnet.CIDR, subnet.HostSubnet))
		} else {
			cidrs = append(cidrs, string(subnet.CIDR))
		}
	}
	return strings.Join(cidrs, ",")
}

type cidr interface {
	userdefinednetworkv1.DualStackCIDRs | []userdefinednetworkv1.CIDR
}

func cidrString[T cidr](subnets T) string {
	var cidrs []string
	for _, subnet := range subnets {
		cidrs = append(cidrs, string(subnet))
	}
	return strings.Join(cidrs, ",")
}

func ipString(ips userdefinednetworkv1.DualStackIPs) string {
	var ipStrings []string
	for _, ip := range ips {
		ipStrings = append(ipStrings, string(ip))
	}
	return strings.Join(ipStrings, ",")
}

// renderEVPNConfig converts the EVPN configuration from the spec into the CNI EVPNConfig format.
// Note: evpnCfg is guaranteed to be non-nil by CEL validation on the CRD.
func renderEVPNConfig(spec SpecGetter, opts *RenderOptions) *ovncnitypes.EVPNConfig {
	evpnCfg := spec.GetEVPN()
	evpnConfig := &ovncnitypes.EVPNConfig{
		VTEP: evpnCfg.VTEP,
	}

	if evpnCfg.MACVRF != nil {
		evpnConfig.MACVRF = &ovncnitypes.VRFConfig{
			VNI:         evpnCfg.MACVRF.VNI,
			RouteTarget: string(evpnCfg.MACVRF.RouteTarget),
		}
		if opts != nil && opts.EVPNVIDs != nil && opts.EVPNVIDs.MACVRFVID > 0 {
			evpnConfig.MACVRF.VID = opts.EVPNVIDs.MACVRFVID
		}
	}
	if evpnCfg.IPVRF != nil {
		evpnConfig.IPVRF = &ovncnitypes.VRFConfig{
			VNI:         evpnCfg.IPVRF.VNI,
			RouteTarget: string(evpnCfg.IPVRF.RouteTarget),
		}
		if opts != nil && opts.EVPNVIDs != nil && opts.EVPNVIDs.IPVRFVID > 0 {
			evpnConfig.IPVRF.VID = opts.EVPNVIDs.IPVRFVID
		}
	}

	return evpnConfig
}

func GetSpec(obj client.Object) SpecGetter {
	switch o := obj.(type) {
	case *userdefinednetworkv1.UserDefinedNetwork:
		return &o.Spec
	case *userdefinednetworkv1.ClusterUserDefinedNetwork:
		return &o.Spec.Network
	default:
		panic(fmt.Sprintf("unknown type %T", obj))
	}
}

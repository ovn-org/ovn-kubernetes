// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	iputils "github.com/containernetworking/plugins/pkg/ip"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"golang.org/x/exp/maps"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	knet "k8s.io/utils/net"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

var (
	ErrorAttachDefNotOvnManaged = errors.New("net-attach-def not managed by OVN")
	ErrorUnsupportedIPAMKey     = errors.New("IPAM key is not supported. Use OVN-K provided IPAM via the `subnets` attribute")
)

// NetInfo exposes read-only information about a network.
type NetInfo interface {
	// static information, not expected to change.
	GetNetworkName() string
	GetNetworkID() int
	GetTunnelKeys() []int
	IsDefault() bool
	IsPrimaryNetwork() bool
	IsUserDefinedNetwork() bool
	TopologyType() string
	MTU() int
	IPMode() (bool, bool)
	Subnets() []config.CIDRNetworkEntry
	ExcludeSubnets() []*net.IPNet
	ReservedSubnets() []*net.IPNet
	InfrastructureSubnets() []*net.IPNet
	JoinSubnetV4() *net.IPNet
	JoinSubnetV6() *net.IPNet
	JoinSubnets() []*net.IPNet
	TransitSubnets() []*net.IPNet
	Vlan() uint
	AllowsPersistentIPs() bool
	PhysicalNetworkName() string
	MACSecurityMode() string
	Transport() string
	OutboundSNAT() string
	EVPNVTEPName() string
	EVPNMACVRFVNI() int32
	EVPNMACVRFRouteTarget() string
	EVPNMACVRFVID() int
	EVPNIPVRFVNI() int32
	EVPNIPVRFRouteTarget() string
	EVPNIPVRFVID() int
	Uplink() string
	GetNodeGatewayIP(hostSubnet *net.IPNet) *net.IPNet
	GetNodeManagementIP(hostSubnet *net.IPNet) *net.IPNet
	// IPAMType returns the ipam.type configured in the NAD. It is "" for
	// both the enabled and disabled IPAM modes, which are inferred from the
	// Subnets presence instead. The only supported value for now is "dhcp"
	// (that delegates IPAM to an external DHCP server).
	IPAMType() string

	// dynamic information, can change over time

	// GetPodNetworkAdvertisedVRFs returns the target VRFs where the pod network
	// is advertised per node, through a map of node names to slice of VRFs.
	GetPodNetworkAdvertisedVRFs() map[string][]string
	// GetPodNetworkAdvertisedOnNodeVRFs returns the target VRFs where the pod
	// network is advertised on the specified node.
	GetPodNetworkAdvertisedOnNodeVRFs(node string) []string
	// GetEgressIPAdvertisedVRFs returns the target VRFs where egress IPs are
	// advertised per node, through a map of node names to slice of VRFs.
	GetEgressIPAdvertisedVRFs() map[string][]string
	// GetEgressIPAdvertisedOnNodeVRFs returns the target VRFs where egress IPs
	// are advertised on the specified node.
	GetEgressIPAdvertisedOnNodeVRFs(node string) []string
	// GetEgressIPAdvertisedNodes return the nodes where egress IP are
	// advertised.
	GetEgressIPAdvertisedNodes() []string

	// derived information.
	GetNADNamespaces() []string
	GetNetworkScopedName(name string) string
	RemoveNetworkScopeFromName(name string) string
	GetNetworkScopedK8sMgmtIntfName(nodeName string) string
	GetNetworkScopedClusterRouterName() string
	GetNetworkScopedGWRouterName(nodeName string) string
	GetNetworkScopedSwitchName(nodeName string) string
	GetNetworkScopedJoinSwitchName() string
	GetNetworkScopedExtSwitchName(nodeName string) string
	GetNetworkScopedPatchPortName(bridgeID, nodeName string) string
	GetNetworkScopedExtPortName(bridgeID, nodeName string) string
	GetNetworkScopedLoadBalancerName(lbName string) string
	GetNetworkScopedLoadBalancerGroupName(lbGroupName string) string
	GetNetworkScopedRouterToSwitchPortName(nodeName string) string
	GetNetworkScopedSwitchToRouterPortName(nodeName string) string

	// GetNetInfo is an identity method used to get the specific NetInfo
	// implementation
	GetNetInfo() NetInfo
}

// DefaultNetInfo is the default network information
type DefaultNetInfo struct {
	mutableNetInfo
}

// MutableNetInfo is a NetInfo where selected information can be changed.
// Intended to be used by network managers that aggregate network information
// from multiple sources that can change over time.
type MutableNetInfo interface {
	NetInfo

	// SetNetworkID sets the network ID before any controller handles the
	// network
	SetNetworkID(id int)
	SetTunnelKeys(keys []int)

	// NADs referencing a network
	SetNADs(nadName ...string)
	AddNADs(nadName ...string)
	DeleteNADs(nadName ...string)

	// VRFs a pod network is being advertised on, also per node
	SetPodNetworkAdvertisedVRFs(podAdvertisements map[string][]string)

	// Nodes advertising Egress IP
	SetEgressIPAdvertisedVRFs(eipAdvertisements map[string][]string)
}

// NewMutableNetInfo builds a copy of netInfo as a MutableNetInfo
func NewMutableNetInfo(netInfo NetInfo) MutableNetInfo {
	if netInfo == nil {
		return nil
	}
	return copyNetInfo(netInfo).(MutableNetInfo)
}

// ReconcilableNetInfo is a NetInfo that can be reconciled
type ReconcilableNetInfo interface {
	NetInfo

	// canReconcile checks if both networks are compatible and thus can be
	// reconciled. Networks are compatible if they are defined by the same
	// static network configuration.
	canReconcile(NetInfo) bool

	// needsReconcile checks if both networks hold differences in their dynamic
	// network configuration that could potentially be reconciled. Note this
	// method does not check for compatibility.
	needsReconcile(NetInfo) bool

	// reconcile copies dynamic network configuration information from the
	// provided network
	reconcile(NetInfo)
}

// NewReconcilableNetInfo builds a copy of netInfo as a ReconcilableNetInfo
func NewReconcilableNetInfo(netInfo NetInfo) ReconcilableNetInfo {
	if netInfo == nil {
		return nil
	}
	return copyNetInfo(netInfo).(ReconcilableNetInfo)
}

// AreNetworksCompatible checks if both networks are compatible and thus can be
// reconciled. Networks are compatible if they are defined by the same
// static network configuration.
func AreNetworksCompatible(l, r NetInfo) bool {
	if l == nil && r == nil {
		return true
	}
	if l == nil || r == nil {
		return false
	}
	return reconcilable(l).canReconcile(r)
}

// DoesNetworkNeedReconciliation checks if both networks hold differences in their dynamic
// network configuration that could potentially be reconciled. Note this
// method does not check for compatibility.
func DoesNetworkNeedReconciliation(l, r NetInfo) bool {
	if l == nil && r == nil {
		return false
	}
	if l == nil || r == nil {
		return true
	}
	return reconcilable(l).needsReconcile(r)
}

// ReconcileNetInfo reconciles the dynamic network configuration
func ReconcileNetInfo(to ReconcilableNetInfo, from NetInfo) error {
	if from == nil || to == nil {
		return fmt.Errorf("can't reconcile a nil network")
	}
	if !AreNetworksCompatible(to, from) {
		return fmt.Errorf("can't reconcile from incompatible network")
	}
	reconcilable(to).reconcile(from)
	return nil
}

func copyNetInfo(netInfo NetInfo) any {
	switch t := netInfo.GetNetInfo().(type) {
	case *DefaultNetInfo:
		return t.copy()
	case *userDefinedNetInfo:
		return t.copy()
	default:
		panic(fmt.Errorf("unrecognized type %T", t))
	}
}

func reconcilable(netInfo NetInfo) ReconcilableNetInfo {
	switch t := netInfo.GetNetInfo().(type) {
	case *DefaultNetInfo:
		return t
	case *userDefinedNetInfo:
		return t
	default:
		panic(fmt.Errorf("unrecognized type %T", t))
	}
}

// mutableNetInfo contains network information that can be changed
type mutableNetInfo struct {
	sync.RWMutex

	// id of the network. It's mutable because is set on day-1 but it can't be
	// changed or reconciled on day-2
	id         int
	tunnelKeys []int

	nads                     sets.Set[string]
	podNetworkAdvertisements map[string][]string
	eipAdvertisements        map[string][]string

	// subnets can be added for Layer3 networks
	subnets []config.CIDRNetworkEntry

	// information generated from previous fields, not used in comparisons
	// namespaces from nads
	namespaces sets.Set[string]
}

func mutable(netInfo NetInfo) *mutableNetInfo {
	switch t := netInfo.GetNetInfo().(type) {
	case *DefaultNetInfo:
		return &t.mutableNetInfo
	case *userDefinedNetInfo:
		return &t.mutableNetInfo
	default:
		panic(fmt.Errorf("unrecognized type %T", t))
	}
}

func (l *mutableNetInfo) needsReconcile(r NetInfo) bool {
	return !mutable(r).equals(l)
}

func (l *mutableNetInfo) reconcile(r NetInfo) {
	l.copyFrom(mutable(r))
}

func (l *mutableNetInfo) equals(r *mutableNetInfo) bool {
	if (l == nil) != (r == nil) {
		return false
	}
	if l == r {
		return true
	}
	l.RLock()
	defer l.RUnlock()
	r.RLock()
	defer r.RUnlock()

	lessCIDR := func(a, b config.CIDRNetworkEntry) bool { return a.String() < b.String() }
	return reflect.DeepEqual(l.id, r.id) &&
		reflect.DeepEqual(l.tunnelKeys, r.tunnelKeys) &&
		reflect.DeepEqual(l.nads, r.nads) &&
		reflect.DeepEqual(l.podNetworkAdvertisements, r.podNetworkAdvertisements) &&
		reflect.DeepEqual(l.eipAdvertisements, r.eipAdvertisements) &&
		cmp.Equal(l.subnets, r.subnets, cmpopts.SortSlices(lessCIDR))
}

func (l *mutableNetInfo) copyFrom(r *mutableNetInfo) {
	if l == r {
		return
	}
	aux := mutableNetInfo{}
	r.RLock()
	aux.id = r.id
	aux.tunnelKeys = slices.Clone(r.tunnelKeys)
	aux.nads = r.nads.Clone()
	aux.setPodNetworkAdvertisedOnVRFs(r.podNetworkAdvertisements)
	aux.setEgressIPAdvertisedAtNodes(r.eipAdvertisements)
	aux.subnets = append([]config.CIDRNetworkEntry(nil), r.subnets...)
	aux.namespaces = r.namespaces.Clone()
	r.RUnlock()
	l.Lock()
	defer l.Unlock()
	l.id = aux.id
	l.tunnelKeys = aux.tunnelKeys
	l.nads = aux.nads
	l.podNetworkAdvertisements = aux.podNetworkAdvertisements
	l.eipAdvertisements = aux.eipAdvertisements
	l.subnets = aux.subnets
	l.namespaces = aux.namespaces
}

func (nInfo *mutableNetInfo) GetNetworkID() int {
	nInfo.RLock()
	defer nInfo.RUnlock()
	return nInfo.id
}

func (nInfo *mutableNetInfo) SetNetworkID(id int) {
	nInfo.Lock()
	defer nInfo.Unlock()
	nInfo.id = id
}

func (nInfo *mutableNetInfo) GetTunnelKeys() []int {
	nInfo.RLock()
	defer nInfo.RUnlock()
	return nInfo.tunnelKeys
}

func (nInfo *mutableNetInfo) SetTunnelKeys(tunnelKeys []int) {
	nInfo.Lock()
	defer nInfo.Unlock()
	nInfo.tunnelKeys = tunnelKeys
}

func (nInfo *mutableNetInfo) SetPodNetworkAdvertisedVRFs(podAdvertisements map[string][]string) {
	nInfo.Lock()
	defer nInfo.Unlock()
	nInfo.setPodNetworkAdvertisedOnVRFs(podAdvertisements)
}

func (nInfo *mutableNetInfo) setPodNetworkAdvertisedOnVRFs(podAdvertisements map[string][]string) {
	nInfo.podNetworkAdvertisements = make(map[string][]string, len(podAdvertisements))
	for node, vrfs := range podAdvertisements {
		nInfo.podNetworkAdvertisements[node] = sets.List(sets.New(vrfs...))
	}
}

func (nInfo *mutableNetInfo) GetPodNetworkAdvertisedVRFs() map[string][]string {
	nInfo.RLock()
	defer nInfo.RUnlock()
	return nInfo.getPodNetworkAdvertisedOnVRFs()
}

func (nInfo *mutableNetInfo) GetPodNetworkAdvertisedOnNodeVRFs(node string) []string {
	nInfo.RLock()
	defer nInfo.RUnlock()
	return nInfo.getPodNetworkAdvertisedOnVRFs()[node]
}

func (nInfo *mutableNetInfo) getPodNetworkAdvertisedOnVRFs() map[string][]string {
	if nInfo.podNetworkAdvertisements == nil {
		return map[string][]string{}
	}
	return nInfo.podNetworkAdvertisements
}

func (nInfo *mutableNetInfo) SetEgressIPAdvertisedVRFs(eipAdvertisements map[string][]string) {
	nInfo.Lock()
	defer nInfo.Unlock()
	nInfo.setEgressIPAdvertisedAtNodes(eipAdvertisements)
}

func (nInfo *mutableNetInfo) setEgressIPAdvertisedAtNodes(eipAdvertisements map[string][]string) {
	nInfo.eipAdvertisements = make(map[string][]string, len(eipAdvertisements))
	for node, vrfs := range eipAdvertisements {
		nInfo.eipAdvertisements[node] = sets.List(sets.New(vrfs...))
	}
}

func (nInfo *mutableNetInfo) GetEgressIPAdvertisedVRFs() map[string][]string {
	nInfo.RLock()
	defer nInfo.RUnlock()
	return nInfo.getEgressIPAdvertisedVRFs()
}

func (nInfo *mutableNetInfo) getEgressIPAdvertisedVRFs() map[string][]string {
	if nInfo.eipAdvertisements == nil {
		return map[string][]string{}
	}
	return nInfo.eipAdvertisements
}

func (nInfo *mutableNetInfo) GetEgressIPAdvertisedOnNodeVRFs(node string) []string {
	nInfo.RLock()
	defer nInfo.RUnlock()
	return nInfo.getEgressIPAdvertisedVRFs()[node]
}

func (nInfo *mutableNetInfo) GetEgressIPAdvertisedNodes() []string {
	nInfo.RLock()
	defer nInfo.RUnlock()
	return maps.Keys(nInfo.eipAdvertisements)
}

// SetNADs replaces the NADs associated with the network
func (nInfo *mutableNetInfo) SetNADs(nadNames ...string) {
	nInfo.Lock()
	defer nInfo.Unlock()
	nInfo.nads = sets.New[string]()
	nInfo.namespaces = sets.New[string]()
	nInfo.addNADs(nadNames...)
}

// AddNADs adds the specified NAD
func (nInfo *mutableNetInfo) AddNADs(nadNames ...string) {
	nInfo.Lock()
	defer nInfo.Unlock()
	nInfo.addNADs(nadNames...)
}

func (nInfo *mutableNetInfo) addNADs(nadNames ...string) {
	for _, name := range nadNames {
		nInfo.getNads().Insert(name)
		nInfo.getNamespaces().Insert(strings.Split(name, "/")[0])
	}
}

// DeleteNADs deletes the specified NAD
func (nInfo *mutableNetInfo) DeleteNADs(nadNames ...string) {
	nInfo.Lock()
	defer nInfo.Unlock()
	ns := sets.New[string]()
	for _, name := range nadNames {
		if !nInfo.getNads().Has(name) {
			continue
		}
		ns.Insert(strings.Split(name, "/")[0])
		nInfo.getNads().Delete(name)
	}
	if ns.Len() == 0 {
		return
	}
	for existing := range nInfo.getNads() {
		ns.Delete(strings.Split(existing, "/")[0])
	}
	nInfo.getNamespaces().Delete(ns.UnsortedList()...)
}

func (nInfo *mutableNetInfo) getNads() sets.Set[string] {
	if nInfo.nads == nil {
		return sets.New[string]()
	}
	return nInfo.nads
}

func (nInfo *mutableNetInfo) getNamespaces() sets.Set[string] {
	if nInfo.namespaces == nil {
		return sets.New[string]()
	}
	return nInfo.namespaces
}

func (nInfo *mutableNetInfo) GetNADNamespaces() []string {
	nInfo.RLock()
	defer nInfo.RUnlock()
	return nInfo.getNamespaces().UnsortedList()
}

func (nInfo *DefaultNetInfo) GetNetInfo() NetInfo {
	return nInfo
}

func (nInfo *DefaultNetInfo) copy() *DefaultNetInfo {
	c := &DefaultNetInfo{}
	c.mutableNetInfo.copyFrom(&nInfo.mutableNetInfo)

	return c
}

// GetNetworkName returns the network name
func (nInfo *DefaultNetInfo) GetNetworkName() string {
	return types.DefaultNetworkName
}

// IsDefault always returns true for default network.
func (nInfo *DefaultNetInfo) IsDefault() bool {
	return true
}

// IsPrimaryNetwork always returns false for default network.
// The boolean indicates if the default network is
// meant to be the primary network for the pod. Since default
// network is never a User Defined Network this is always false.
// This cannot be true if IsUserDefinedNetwork() is not true.
func (nInfo *DefaultNetInfo) IsPrimaryNetwork() bool {
	return false
}

// IsUserDefinedNetwork returns if this network is secondary
func (nInfo *DefaultNetInfo) IsUserDefinedNetwork() bool {
	return false
}

// GetNetworkScopedName returns a network scoped name form the provided one
// appropriate to use globally.
func (nInfo *DefaultNetInfo) GetNetworkScopedName(name string) string {
	// for the default network, names are not scoped
	return name
}

func (nInfo *DefaultNetInfo) RemoveNetworkScopeFromName(name string) string {
	// for the default network, names are not scoped
	return name
}

func (nInfo *DefaultNetInfo) GetNetworkScopedK8sMgmtIntfName(nodeName string) string {
	return GetK8sMgmtIntfName(nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *DefaultNetInfo) GetNetworkScopedClusterRouterName() string {
	return nInfo.GetNetworkScopedName(types.OVNClusterRouter)
}

func (nInfo *DefaultNetInfo) GetNetworkScopedGWRouterName(nodeName string) string {
	return GetGatewayRouterFromNode(nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *DefaultNetInfo) GetNetworkScopedSwitchName(nodeName string) string {
	return nInfo.GetNetworkScopedName(nodeName)
}

func (nInfo *DefaultNetInfo) GetNetworkScopedJoinSwitchName() string {
	return nInfo.GetNetworkScopedName(types.OVNJoinSwitch)
}

func (nInfo *DefaultNetInfo) GetNetworkScopedExtSwitchName(nodeName string) string {
	return GetExtSwitchFromNode(nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *DefaultNetInfo) GetNetworkScopedPatchPortName(bridgeID, nodeName string) string {
	return GetPatchPortName(bridgeID, nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *DefaultNetInfo) GetNetworkScopedExtPortName(bridgeID, nodeName string) string {
	return GetExtPortName(bridgeID, nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *DefaultNetInfo) GetNetworkScopedLoadBalancerName(lbName string) string {
	return nInfo.GetNetworkScopedName(lbName)
}

func (nInfo *DefaultNetInfo) GetNetworkScopedLoadBalancerGroupName(lbGroupName string) string {
	return nInfo.GetNetworkScopedName(lbGroupName)
}

func (nInfo *DefaultNetInfo) GetNetworkScopedRouterToSwitchPortName(nodeName string) string {
	return types.RouterToSwitchPrefix + nInfo.GetNetworkScopedSwitchName(nodeName)
}

func (nInfo *DefaultNetInfo) GetNetworkScopedSwitchToRouterPortName(nodeName string) string {
	return types.SwitchToRouterPrefix + nInfo.GetNetworkScopedSwitchName(nodeName)
}

func (nInfo *DefaultNetInfo) canReconcile(netInfo NetInfo) bool {
	_, ok := netInfo.(*DefaultNetInfo)
	return ok
}

// TopologyType returns the defaultNetConfInfo's topology type which is empty
func (nInfo *DefaultNetInfo) TopologyType() string {
	// TODO(trozet): optimize other checks using this function after changing default network type from "" -> L3
	return types.Layer3Topology
}

// MTU returns the defaultNetConfInfo's MTU value
func (nInfo *DefaultNetInfo) MTU() int {
	return config.Default.MTU
}

// IPMode returns the defaultNetConfInfo's ipv4/ipv6 mode
func (nInfo *DefaultNetInfo) IPMode() (bool, bool) {
	return config.IPv4Mode, config.IPv6Mode
}

// Subnets returns the defaultNetConfInfo's Subnets value
func (nInfo *DefaultNetInfo) Subnets() []config.CIDRNetworkEntry {
	return config.Default.ClusterSubnets
}

// ExcludeSubnets returns the defaultNetConfInfo's ExcludeSubnets value
func (nInfo *DefaultNetInfo) ExcludeSubnets() []*net.IPNet {
	return nil
}

// ReservedSubnets returns the defaultNetConfInfo's ReservedSubnets value
func (nInfo *DefaultNetInfo) ReservedSubnets() []*net.IPNet {
	return nil
}

// InfrastructureSubnets returns the defaultNetConfInfo's InfrastructureSubnets value
func (nInfo *DefaultNetInfo) InfrastructureSubnets() []*net.IPNet {
	return nil
}

// JoinSubnetV4 returns the defaultNetConfInfo's JoinSubnetV4 value
// call when ipv4mode=true
func (nInfo *DefaultNetInfo) JoinSubnetV4() *net.IPNet {
	_, cidr, err := net.ParseCIDR(config.Gateway.V4JoinSubnet)
	if err != nil {
		// Join subnet should have been validated already by config
		panic(fmt.Sprintf("Failed to parse join subnet %q: %v", config.Gateway.V4JoinSubnet, err))
	}
	return cidr
}

// JoinSubnetV6 returns the defaultNetConfInfo's JoinSubnetV6 value
// call when ipv6mode=true
func (nInfo *DefaultNetInfo) JoinSubnetV6() *net.IPNet {
	_, cidr, err := net.ParseCIDR(config.Gateway.V6JoinSubnet)
	if err != nil {
		// Join subnet should have been validated already by config
		panic(fmt.Sprintf("Failed to parse join subnet %q: %v", config.Gateway.V6JoinSubnet, err))
	}
	return cidr
}

// JoinSubnets returns the userDefinedNetInfo's joinsubnet values (both v4&v6)
// used from Equals
func (nInfo *DefaultNetInfo) JoinSubnets() []*net.IPNet {
	var defaultJoinSubnets []*net.IPNet
	_, v4, err := net.ParseCIDR(config.Gateway.V4JoinSubnet)
	if err != nil {
		// Join subnet should have been validated already by config
		panic(fmt.Sprintf("Failed to parse join subnet %q: %v", config.Gateway.V4JoinSubnet, err))
	}
	defaultJoinSubnets = append(defaultJoinSubnets, v4)
	_, v6, err := net.ParseCIDR(config.Gateway.V6JoinSubnet)
	if err != nil {
		// Join subnet should have been validated already by config
		panic(fmt.Sprintf("Failed to parse join subnet %q: %v", config.Gateway.V6JoinSubnet, err))
	}
	defaultJoinSubnets = append(defaultJoinSubnets, v6)
	return defaultJoinSubnets
}

// TransitSubnets should not be used for the default network.
// It will return an empty list since transit networks are not set for this type of network.
func (nInfo *DefaultNetInfo) TransitSubnets() []*net.IPNet {
	return []*net.IPNet{}
}

// Vlan returns the defaultNetConfInfo's Vlan value
func (nInfo *DefaultNetInfo) Vlan() uint {
	return config.Gateway.VLANID
}

// AllowsPersistentIPs returns the defaultNetConfInfo's AllowPersistentIPs value
func (nInfo *DefaultNetInfo) AllowsPersistentIPs() bool {
	return false
}

// PhysicalNetworkName has no impact on defaultNetConfInfo (localnet feature)
func (nInfo *DefaultNetInfo) PhysicalNetworkName() string {
	return ""
}

// MACSecurityMode returns enabled, as MAC spoof protection is always enabled on the default network.
func (nInfo *DefaultNetInfo) MACSecurityMode() string {
	return types.MACSecurityModeEnabled
}

// Transport returns the transport protocol for east-west traffic
func (nInfo *DefaultNetInfo) Transport() string {
	return config.Default.Transport
}

// OutboundSNAT() string returns the outbound SNAT configuration for the default network when using no-overlay transport.
func (nInfo *DefaultNetInfo) OutboundSNAT() string {
	return config.NoOverlay.OutboundSNAT
}

// EVPNVTEPName returns empty as EVPN is not supported on the default network
func (nInfo *DefaultNetInfo) EVPNVTEPName() string {
	return ""
}

// EVPNMACVRFVNI returns 0 as EVPN is not supported on the default network
func (nInfo *DefaultNetInfo) EVPNMACVRFVNI() int32 {
	return 0
}

// EVPNMACVRFRouteTarget returns empty as EVPN is not supported on the default network
func (nInfo *DefaultNetInfo) EVPNMACVRFRouteTarget() string {
	return ""
}

// EVPNIPVRFVNI returns 0 as EVPN is not supported on the default network
func (nInfo *DefaultNetInfo) EVPNIPVRFVNI() int32 {
	return 0
}

// EVPNIPVRFRouteTarget returns empty as EVPN is not supported on the default network
func (nInfo *DefaultNetInfo) EVPNIPVRFRouteTarget() string {
	return ""
}

// EVPNMACVRFVID returns 0 as EVPN is not supported on the default network
func (nInfo *DefaultNetInfo) EVPNMACVRFVID() int {
	return 0
}

// EVPNIPVRFVID returns 0 as EVPN is not supported on the default network
func (nInfo *DefaultNetInfo) EVPNIPVRFVID() int {
	return 0
}

// Uplink returns empty as Uplink is not supported on the default network.
func (nInfo *DefaultNetInfo) Uplink() string {
	return ""
}

func (nInfo *DefaultNetInfo) GetNodeGatewayIP(hostSubnet *net.IPNet) *net.IPNet {
	return GetNodeGatewayIfAddr(hostSubnet)
}

func (nInfo *DefaultNetInfo) GetNodeManagementIP(hostSubnet *net.IPNet) *net.IPNet {
	return GetNodeManagementIfAddr(hostSubnet)
}

// IPAMType returns the ipam.type configured in the NAD. Default network never
// uses an external IPAM plugin, so this is always "".
func (nInfo *DefaultNetInfo) IPAMType() string {
	return ""
}

// userDefinedNetInfo holds the network name information for a User Defined Network if non-nil
type userDefinedNetInfo struct {
	mutableNetInfo

	netName string
	// Should this User Defined Network be used
	// as the pod's primary network?
	primaryNetwork     bool
	topology           string
	mtu                int
	vlan               uint
	allowPersistentIPs bool

	ipv4mode, ipv6mode    bool
	excludeSubnets        []*net.IPNet
	reservedSubnets       []*net.IPNet
	infrastructureSubnets []*net.IPNet
	joinSubnets           []*net.IPNet
	transitSubnets        []*net.IPNet

	physicalNetworkName string
	defaultGatewayIPs   []net.IP
	managementIPs       []net.IP

	macSecurityMode string

	transport    string
	evpn         *ovncnitypes.EVPNConfig
	outboundSNAT string
	uplink       string

	// ipamType is the ipam.type value from the NAD config. It is "" for
	// both the enabled and disabled IPAM modes, which are inferred from the
	// subnets presence instead. The only supported value for now is "dhcp"
	// (that delegates IPAM to an external DHCP server).
	ipamType string
}

func (nInfo *userDefinedNetInfo) GetNetInfo() NetInfo {
	return nInfo
}

// GetNetworkName returns the network name
func (nInfo *userDefinedNetInfo) GetNetworkName() string {
	return nInfo.netName
}

// IsDefault always returns false for all User Defined Networks.
func (nInfo *userDefinedNetInfo) IsDefault() bool {
	return false
}

// IsPrimaryNetwork returns if this User Defined Network
// should be used as the primaryNetwork for the pod
// to achieve native network segmentation
func (nInfo *userDefinedNetInfo) IsPrimaryNetwork() bool {
	return nInfo.primaryNetwork
}

// IsUserDefinedNetwork returns if this network is a User Defined Network
func (nInfo *userDefinedNetInfo) IsUserDefinedNetwork() bool {
	return true
}

// GetNetworkScopedName returns a network scoped name from the provided one
// appropriate to use globally.
func (nInfo *userDefinedNetInfo) GetNetworkScopedName(name string) string {
	return fmt.Sprintf("%s%s", nInfo.getPrefix(), name)
}

// RemoveNetworkScopeFromName removes the name without the network scope added
// by a previous call to GetNetworkScopedName
func (nInfo *userDefinedNetInfo) RemoveNetworkScopeFromName(name string) string {
	// for the default network, names are not scoped
	return strings.TrimPrefix(name, nInfo.getPrefix())
}

func (nInfo *userDefinedNetInfo) GetNetworkScopedK8sMgmtIntfName(nodeName string) string {
	return GetK8sMgmtIntfName(nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *userDefinedNetInfo) GetNetworkScopedClusterRouterName() string {
	if nInfo.TopologyType() == types.Layer2Topology {
		return nInfo.GetNetworkScopedName(types.TransitRouter)
	}
	return nInfo.GetNetworkScopedName(types.OVNClusterRouter)
}

func (nInfo *userDefinedNetInfo) GetNetworkScopedGWRouterName(nodeName string) string {
	return GetGatewayRouterFromNode(nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *userDefinedNetInfo) GetNetworkScopedSwitchName(nodeName string) string {
	// In Layer2Topology there is just one global switch
	if nInfo.TopologyType() == types.Layer2Topology {
		return nInfo.GetNetworkScopedName(types.OVNLayer2Switch)
	}
	return nInfo.GetNetworkScopedName(nodeName)
}

func (nInfo *userDefinedNetInfo) GetNetworkScopedJoinSwitchName() string {
	return nInfo.GetNetworkScopedName(types.OVNJoinSwitch)
}

func (nInfo *userDefinedNetInfo) GetNetworkScopedExtSwitchName(nodeName string) string {
	return GetExtSwitchFromNode(nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *userDefinedNetInfo) GetNetworkScopedPatchPortName(bridgeID, nodeName string) string {
	return GetPatchPortName(bridgeID, nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *userDefinedNetInfo) GetNetworkScopedExtPortName(bridgeID, nodeName string) string {
	return GetExtPortName(bridgeID, nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *userDefinedNetInfo) GetNetworkScopedLoadBalancerName(lbName string) string {
	return nInfo.GetNetworkScopedName(lbName)
}

func (nInfo *userDefinedNetInfo) GetNetworkScopedLoadBalancerGroupName(lbGroupName string) string {
	return nInfo.GetNetworkScopedName(lbGroupName)
}

// GetNetworkScopedRouterToSwitchPortName returns the port name from router to switch.
// For Layer3 and pre-transit router Layer2 topologies, this is the cluster
// router to switch port (rtos-).
// For Layer2 topology using transit router, this is the transit router to
// switch port (trtos-).
// Not Applicable for Localnet topology.
func (nInfo *userDefinedNetInfo) GetNetworkScopedRouterToSwitchPortName(nodeName string) string {
	switchName := nInfo.GetNetworkScopedSwitchName(nodeName)
	if nInfo.TopologyType() == types.Layer2Topology && config.Layer2UsesTransitRouter {
		return types.TransitRouterToSwitchPrefix + switchName
	}
	return types.RouterToSwitchPrefix + switchName
}

// GetNetworkScopedSwitchToRouterPortName returns the port name from switch to router.
// For Layer3 and pre-transit router Layer2 topologies, this is the switch port
// to cluster router (stor-).
// For Layer2 topology using transit router, this is the switch port to transit
// router (stotr-).
// Not Applicable for Localnet topology.
func (nInfo *userDefinedNetInfo) GetNetworkScopedSwitchToRouterPortName(nodeName string) string {
	switchName := nInfo.GetNetworkScopedSwitchName(nodeName)
	if nInfo.TopologyType() == types.Layer2Topology && config.Layer2UsesTransitRouter {
		return types.SwitchToTransitRouterPrefix + switchName
	}
	return types.SwitchToRouterPrefix + switchName
}

// getPrefix returns if the logical entities prefix for this network
func (nInfo *userDefinedNetInfo) getPrefix() string {
	return GetUserDefinedNetworkPrefix(nInfo.netName)
}

// TopologyType returns the topology type
func (nInfo *userDefinedNetInfo) TopologyType() string {
	return nInfo.topology
}

// MTU returns the layer3NetConfInfo's MTU value
func (nInfo *userDefinedNetInfo) MTU() int {
	return nInfo.mtu
}

// Vlan returns the Vlan value
func (nInfo *userDefinedNetInfo) Vlan() uint {
	return nInfo.vlan
}

// AllowsPersistentIPs returns the defaultNetConfInfo's AllowPersistentIPs value
func (nInfo *userDefinedNetInfo) AllowsPersistentIPs() bool {
	return nInfo.allowPersistentIPs
}

// PhysicalNetworkName returns the user provided physical network name value
func (nInfo *userDefinedNetInfo) PhysicalNetworkName() string {
	return nInfo.physicalNetworkName
}

// MACSecurityMode returns MAC security mode to be set for the network LSPs
func (nInfo *userDefinedNetInfo) MACSecurityMode() string {
	if nInfo.macSecurityMode == "" {
		return types.MACSecurityModeEnabled
	}
	return nInfo.macSecurityMode
}

// Transport returns the transport protocol for east-west traffic
func (nInfo *userDefinedNetInfo) Transport() string {
	return nInfo.transport
}

// OutboundSNAT() string returns the outbound SNAT configuration for this network when using no-overlay transport.
func (nInfo *userDefinedNetInfo) OutboundSNAT() string {
	return nInfo.outboundSNAT
}

// EVPNVTEPName returns the name of the VTEP CR for EVPN
func (nInfo *userDefinedNetInfo) EVPNVTEPName() string {
	if nInfo.evpn == nil {
		return ""
	}
	return nInfo.evpn.VTEP
}

// EVPNMACVRFVNI returns the MAC-VRF VNI for EVPN
func (nInfo *userDefinedNetInfo) EVPNMACVRFVNI() int32 {
	if nInfo.evpn == nil || nInfo.evpn.MACVRF == nil {
		return 0
	}
	return nInfo.evpn.MACVRF.VNI
}

// EVPNMACVRFRouteTarget returns the MAC-VRF route target for EVPN
func (nInfo *userDefinedNetInfo) EVPNMACVRFRouteTarget() string {
	if nInfo.evpn == nil || nInfo.evpn.MACVRF == nil {
		return ""
	}
	return nInfo.evpn.MACVRF.RouteTarget
}

// EVPNIPVRFVNI returns the IP-VRF VNI for EVPN
func (nInfo *userDefinedNetInfo) EVPNIPVRFVNI() int32 {
	if nInfo.evpn == nil || nInfo.evpn.IPVRF == nil {
		return 0
	}
	return nInfo.evpn.IPVRF.VNI
}

// EVPNIPVRFRouteTarget returns the IP-VRF route target for EVPN
func (nInfo *userDefinedNetInfo) EVPNIPVRFRouteTarget() string {
	if nInfo.evpn == nil || nInfo.evpn.IPVRF == nil {
		return ""
	}
	return nInfo.evpn.IPVRF.RouteTarget
}

// EVPNMACVRFVID returns the MAC-VRF VID for EVPN
func (nInfo *userDefinedNetInfo) EVPNMACVRFVID() int {
	if nInfo.evpn == nil || nInfo.evpn.MACVRF == nil {
		return 0
	}
	return nInfo.evpn.MACVRF.VID
}

// EVPNIPVRFVID returns the IP-VRF VID for EVPN
func (nInfo *userDefinedNetInfo) EVPNIPVRFVID() int {
	if nInfo.evpn == nil || nInfo.evpn.IPVRF == nil {
		return 0
	}
	return nInfo.evpn.IPVRF.VID
}

// Uplink returns the Uplink resource selected by this network, or "" when the
// Uplink feature is disabled so that uplink-specific handling is skipped.
func (nInfo *userDefinedNetInfo) Uplink() string {
	if !IsUplinkEnabled() {
		return ""
	}
	return nInfo.uplink
}

func (nInfo *userDefinedNetInfo) GetNodeGatewayIP(hostSubnet *net.IPNet) *net.IPNet {
	if IsPreconfiguredUDNAddressesEnabled() && nInfo.TopologyType() == types.Layer2Topology && nInfo.IsPrimaryNetwork() {
		isIPV6 := knet.IsIPv6CIDR(hostSubnet)
		gwIP, _ := MatchFirstIPFamily(isIPV6, nInfo.defaultGatewayIPs)
		return &net.IPNet{
			IP:   gwIP,
			Mask: hostSubnet.Mask,
		}
	}
	return GetNodeGatewayIfAddr(hostSubnet)
}

func (nInfo *userDefinedNetInfo) GetNodeManagementIP(hostSubnet *net.IPNet) *net.IPNet {
	if IsPreconfiguredUDNAddressesEnabled() && nInfo.TopologyType() == types.Layer2Topology && nInfo.IsPrimaryNetwork() {
		isIPV6 := knet.IsIPv6CIDR(hostSubnet)
		mgmtIP, _ := MatchFirstIPFamily(isIPV6, nInfo.managementIPs)
		return &net.IPNet{
			IP:   mgmtIP,
			Mask: hostSubnet.Mask,
		}
	}
	return GetNodeManagementIfAddr(hostSubnet)
}

// IPAMType returns the ipam.type configured in the NAD (e.g. "dhcp" on a
// localnet secondary network when IPAM is delegated to an external DHCP server).
func (nInfo *userDefinedNetInfo) IPAMType() string {
	return nInfo.ipamType
}

// IPMode returns the ipv4/ipv6 mode
func (nInfo *userDefinedNetInfo) IPMode() (bool, bool) {
	return nInfo.ipv4mode, nInfo.ipv6mode
}

// Subnets returns the Subnets value
func (nInfo *userDefinedNetInfo) Subnets() []config.CIDRNetworkEntry {
	nInfo.RLock()
	defer nInfo.RUnlock()
	return append([]config.CIDRNetworkEntry(nil), nInfo.mutableNetInfo.subnets...)
}

// ExcludeSubnets returns the ExcludeSubnets value
func (nInfo *userDefinedNetInfo) ExcludeSubnets() []*net.IPNet {
	return nInfo.excludeSubnets
}

// ReservedSubnets returns the ReservedSubnets value
func (nInfo *userDefinedNetInfo) ReservedSubnets() []*net.IPNet {
	return nInfo.reservedSubnets
}

// InfrastructureSubnets returns the InfrastructureSubnets value
func (nInfo *userDefinedNetInfo) InfrastructureSubnets() []*net.IPNet {
	return nInfo.infrastructureSubnets
}

// JoinSubnetV4 returns the defaultNetConfInfo's JoinSubnetV4 value
// call when ipv4mode=true
func (nInfo *userDefinedNetInfo) JoinSubnetV4() *net.IPNet {
	if len(nInfo.joinSubnets) == 0 {
		return nil // localnet topology
	}
	return nInfo.joinSubnets[0]
}

// JoinSubnetV6 returns the userDefinedNetInfo's JoinSubnetV6 value
// call when ipv6mode=true
func (nInfo *userDefinedNetInfo) JoinSubnetV6() *net.IPNet {
	if len(nInfo.joinSubnets) <= 1 {
		return nil // localnet topology
	}
	return nInfo.joinSubnets[1]
}

// JoinSubnets returns the userDefinedNetInfo's joinsubnet values (both v4&v6)
// used from Equals (since localnet doesn't have joinsubnets to compare nil v/s nil
// we need this util)
func (nInfo *userDefinedNetInfo) JoinSubnets() []*net.IPNet {
	return nInfo.joinSubnets
}

// TransitSubnets returns the userDefinedNetInfo's transit subnet values (both v4&v6)
// For now it is only set for Primary Layer2 UDNs, otherwise is empty
func (nInfo *userDefinedNetInfo) TransitSubnets() []*net.IPNet {
	return nInfo.transitSubnets
}

func (nInfo *userDefinedNetInfo) reconcile(other NetInfo) {
	nInfo.mutableNetInfo.reconcile(other)
}

func (nInfo *userDefinedNetInfo) canReconcile(other NetInfo) bool {
	if (nInfo == nil) != (other == nil) {
		return false
	}
	if nInfo == nil && other == nil {
		return true
	}
	// if network ID has changed, it means the network was re-created, and all controllers
	// should execute delete+create instead of update
	if nInfo.GetNetworkID() != types.InvalidID && other.GetNetworkID() != types.InvalidID && nInfo.GetNetworkID() != other.GetNetworkID() {
		return false
	}
	if nInfo.netName != other.GetNetworkName() {
		return false
	}
	if nInfo.topology != other.TopologyType() {
		return false
	}
	if nInfo.mtu != other.MTU() {
		return false
	}
	if nInfo.vlan != other.Vlan() {
		return false
	}
	if nInfo.allowPersistentIPs != other.AllowsPersistentIPs() {
		return false
	}
	if nInfo.primaryNetwork != other.IsPrimaryNetwork() {
		return false
	}
	if nInfo.physicalNetworkName != other.PhysicalNetworkName() {
		return false
	}
	if nInfo.ipamType != other.IPAMType() {
		return false
	}
	if nInfo.Transport() != other.Transport() {
		return false
	}
	if nInfo.Uplink() != other.Uplink() {
		return false
	}
	if nInfo.EVPNVTEPName() != other.EVPNVTEPName() {
		return false
	}
	if nInfo.EVPNMACVRFVNI() != other.EVPNMACVRFVNI() {
		return false
	}
	if nInfo.EVPNMACVRFRouteTarget() != other.EVPNMACVRFRouteTarget() {
		return false
	}
	if nInfo.EVPNIPVRFVNI() != other.EVPNIPVRFVNI() {
		return false
	}
	if nInfo.EVPNIPVRFRouteTarget() != other.EVPNIPVRFRouteTarget() {
		return false
	}
	if nInfo.OutboundSNAT() != other.OutboundSNAT() {
		return false
	}

	// omitted mode-security mode is equivalent to enabled (preserves current behavior)
	if nInfo.MACSecurityMode() != other.MACSecurityMode() {
		return false
	}

	lessCIDRNetworkEntry := func(a, b config.CIDRNetworkEntry) bool { return a.String() < b.String() }
	if !cmp.Equal(nInfo.Subnets(), other.Subnets(), cmpopts.SortSlices(lessCIDRNetworkEntry)) {
		// For Layer3 topology, adding subnets is considered compatible and reconcilable
		// CRD validation ensures only subnet additions are allowed
		if nInfo.topology != types.Layer3Topology {
			return false
		}
	}

	lessIPNet := func(a, b net.IPNet) bool { return a.String() < b.String() }
	if !cmp.Equal(nInfo.excludeSubnets, other.ExcludeSubnets(), cmpopts.SortSlices(lessIPNet)) {
		return false
	}
	if !cmp.Equal(nInfo.reservedSubnets, other.ReservedSubnets(), cmpopts.SortSlices(lessIPNet)) {
		return false
	}
	if !cmp.Equal(nInfo.infrastructureSubnets, other.InfrastructureSubnets(), cmpopts.SortSlices(lessIPNet)) {
		return false
	}
	if !cmp.Equal(nInfo.joinSubnets, other.JoinSubnets(), cmpopts.SortSlices(lessIPNet)) {
		return false
	}
	return cmp.Equal(nInfo.transitSubnets, other.TransitSubnets(), cmpopts.SortSlices(lessIPNet))
}

func (nInfo *userDefinedNetInfo) copy() *userDefinedNetInfo {
	// everything here is immutable
	c := &userDefinedNetInfo{
		netName:               nInfo.netName,
		primaryNetwork:        nInfo.primaryNetwork,
		topology:              nInfo.topology,
		mtu:                   nInfo.mtu,
		vlan:                  nInfo.vlan,
		allowPersistentIPs:    nInfo.allowPersistentIPs,
		ipv4mode:              nInfo.ipv4mode,
		ipv6mode:              nInfo.ipv6mode,
		excludeSubnets:        nInfo.excludeSubnets,
		reservedSubnets:       nInfo.reservedSubnets,
		infrastructureSubnets: nInfo.infrastructureSubnets,
		joinSubnets:           nInfo.joinSubnets,
		transitSubnets:        nInfo.transitSubnets,
		physicalNetworkName:   nInfo.physicalNetworkName,
		defaultGatewayIPs:     nInfo.defaultGatewayIPs,
		managementIPs:         nInfo.managementIPs,
		transport:             nInfo.transport,
		evpn:                  nInfo.evpn,
		outboundSNAT:          nInfo.outboundSNAT,
		uplink:                nInfo.uplink,
		ipamType:              nInfo.ipamType,
		macSecurityMode:       nInfo.macSecurityMode,
	}
	// copy mutables
	c.mutableNetInfo.copyFrom(&nInfo.mutableNetInfo)

	return c
}

func newLayer3NetConfInfo(netconf *ovncnitypes.NetConf) (MutableNetInfo, error) {
	subnets, err := parseNetworkSubnets(netconf.Subnets, types.Layer3Topology)
	if err != nil {
		return nil, err
	}
	joinSubnets, err := parseJoinSubnet(netconf.JoinSubnet)
	if err != nil {
		return nil, err
	}
	ni := &userDefinedNetInfo{
		netName:        netconf.Name,
		primaryNetwork: netconf.Role == types.NetworkRolePrimary,
		topology:       types.Layer3Topology,
		joinSubnets:    joinSubnets,
		mtu:            netconf.MTU,
		transport:      netconf.Transport,
		evpn:           netconf.EVPN,
		outboundSNAT:   netconf.OutboundSNAT,
		uplink:         netconf.Uplink,
		mutableNetInfo: mutableNetInfo{
			id:      types.InvalidID,
			nads:    sets.Set[string]{},
			subnets: append([]config.CIDRNetworkEntry(nil), subnets...),
		},
	}
	ni.ipv4mode, ni.ipv6mode = getIPMode(subnets)
	return ni, nil
}

func newLayer2NetConfInfo(netconf *ovncnitypes.NetConf) (MutableNetInfo, error) {
	subnets, err := parseNetworkSubnets(netconf.Subnets, types.Layer2Topology)
	if err != nil {
		return nil, fmt.Errorf("invalid network subnets for %s netconf %s: %v", netconf.Topology, netconf.Name, err)
	}

	excludes, err := parseSubnetList(netconf.ExcludeSubnets)
	if err != nil {
		return nil, fmt.Errorf("invalid exclude subnets for %s netconf %s: %v", netconf.Topology, netconf.Name, err)
	}
	if err := validateSubnetContainment(excludes, subnets, config.NewExcludedSubnetNotContainedError); err != nil {
		return nil, err
	}

	var reserved, infra []*net.IPNet
	if IsPreconfiguredUDNAddressesEnabled() {
		reserved, err = parseSubnetList(netconf.ReservedSubnets)
		if err != nil {
			return nil, fmt.Errorf("invalid reserved subnets for %s netconf %s: %v", netconf.Topology, netconf.Name, err)
		}
		if err := validateSubnetContainment(reserved, subnets, config.NewReservedSubnetNotContainedError); err != nil {
			return nil, err
		}

		infra, err = parseSubnetList(netconf.InfrastructureSubnets)
		if err != nil {
			return nil, fmt.Errorf("invalid infrastructure subnets for %s netconf %s: %v", netconf.Topology, netconf.Name, err)
		}
		if err := validateSubnetContainment(infra, subnets, config.NewInfrastructureSubnetNotContainedError); err != nil {
			return nil, err
		}
	}

	joinSubnets, err := parseJoinSubnet(netconf.JoinSubnet)
	if err != nil {
		return nil, err
	}

	transitSubnets, err := parseTransitSubnet(netconf.Role, netconf.TransitSubnet)
	if err != nil {
		return nil, fmt.Errorf("invalid transit subnet for %s netconf %s: %v", netconf.Topology, netconf.Name, err)
	}

	// Allocate infrastructure IPs for primary networks
	var defaultGatewayIPs, managementIPs []net.IP
	if IsPreconfiguredUDNAddressesEnabled() && netconf.Role == types.NetworkRolePrimary {
		defaultGatewayIPs, managementIPs, err = allocateInfrastructureIPs(netconf)
		if err != nil {
			return nil, fmt.Errorf("failed to allocate infrastructure IPs: %v", err)
		}
	}

	ni := &userDefinedNetInfo{
		netName:               netconf.Name,
		primaryNetwork:        netconf.Role == types.NetworkRolePrimary,
		topology:              types.Layer2Topology,
		joinSubnets:           joinSubnets,
		transitSubnets:        transitSubnets,
		excludeSubnets:        excludes,
		reservedSubnets:       reserved,
		infrastructureSubnets: infra,
		mtu:                   netconf.MTU,
		allowPersistentIPs:    netconf.AllowPersistentIPs,
		defaultGatewayIPs:     defaultGatewayIPs,
		managementIPs:         managementIPs,
		transport:             netconf.Transport,
		evpn:                  netconf.EVPN,
		uplink:                netconf.Uplink,
		macSecurityMode:       netconf.MACSecurityMode,
		mutableNetInfo: mutableNetInfo{
			id:      types.InvalidID,
			nads:    sets.Set[string]{},
			subnets: append([]config.CIDRNetworkEntry(nil), subnets...),
		},
	}
	ni.ipv4mode, ni.ipv6mode = getIPMode(subnets)
	return ni, nil
}

func newLocalnetNetConfInfo(netconf *ovncnitypes.NetConf) (MutableNetInfo, error) {
	subnets, err := parseNetworkSubnets(netconf.Subnets, types.LocalnetTopology)
	if err != nil {
		return nil, fmt.Errorf("invalid %s netconf %s: %v", netconf.Topology, netconf.Name, err)
	}

	excludes, err := parseSubnetList(netconf.ExcludeSubnets)
	if err != nil {
		return nil, fmt.Errorf("invalid %s netconf %s: %v", netconf.Topology, netconf.Name, err)
	}

	if err := validateSubnetContainment(excludes, subnets, config.NewExcludedSubnetNotContainedError); err != nil {
		return nil, err
	}

	ni := &userDefinedNetInfo{
		netName:             netconf.Name,
		topology:            types.LocalnetTopology,
		excludeSubnets:      excludes,
		mtu:                 netconf.MTU,
		vlan:                uint(netconf.VLANID),
		allowPersistentIPs:  netconf.AllowPersistentIPs,
		physicalNetworkName: netconf.PhysicalNetworkName,
		uplink:              netconf.Uplink,
		ipamType:            netconf.IPAM.Type,
		macSecurityMode:     netconf.MACSecurityMode,
		mutableNetInfo: mutableNetInfo{
			id:      types.InvalidID,
			nads:    sets.Set[string]{},
			subnets: append([]config.CIDRNetworkEntry(nil), subnets...),
		},
	}
	ni.ipv4mode, ni.ipv6mode = getIPMode(subnets)
	// DHCP mode doesn't set any subnets to derive IP family from. We only
	// support DHCP mode for ipv4.
	if ni.ipamType == types.IPAMTypeDHCP {
		ni.ipv4mode = true
	}
	return ni, nil
}

// parseNetworkSubnets parses network subnets based on the topology, returns nil if subnets is an empty string
func parseNetworkSubnets(subnets, topology string) ([]config.CIDRNetworkEntry, error) {
	if strings.TrimSpace(subnets) == "" {
		return nil, nil
	}

	switch topology {
	case types.Layer3Topology:
		// For L3 topology, subnet is validated
		return config.ParseClusterSubnetEntries(subnets)
	case types.LocalnetTopology, types.Layer2Topology:
		// For L2 topologies, host specific prefix length is ignored (using 0 as prefix length)
		return config.ParseClusterSubnetEntriesWithDefaults(subnets, 0, 0)
	default:
		return nil, fmt.Errorf("unsupported topology: %s", topology)
	}
}

// parseSubnetList parses a list of subnets, returns nil if subnets is an empty string
func parseSubnetList(subnets string) ([]*net.IPNet, error) {
	if strings.TrimSpace(subnets) == "" {
		return nil, nil
	}

	// For subnet lists, host specific prefix length is ignored (using 0 as prefix length)
	entries, err := config.ParseClusterSubnetEntriesWithDefaults(subnets, 0, 0)
	if err != nil {
		return nil, err
	}

	nets := make([]*net.IPNet, 0, len(entries))
	for _, entry := range entries {
		nets = append(nets, entry.CIDR)
	}
	return nets, nil
}

// validateSubnetContainment checks if every subnet in subnets is contained in containerSubnets
// and returns a typed error using the provided error constructor function
func validateSubnetContainment(subnets []*net.IPNet, containerSubnets []config.CIDRNetworkEntry,
	errorConstructor func(interface{}) *config.ValidationError) error {
	for _, subnet := range subnets {
		found := false
		for _, containerSubnet := range containerSubnets {
			if ContainsCIDR(containerSubnet.CIDR, subnet) {
				found = true
				break
			}
		}
		if !found {
			return errorConstructor(subnet)
		}
	}
	return nil
}

func parseJoinSubnet(joinSubnet string) ([]*net.IPNet, error) {
	// assign the default values first
	// if user provided only 1 family; we still populate the default value
	// of the other family from the get-go
	_, v4cidr, err := net.ParseCIDR(types.UserDefinedPrimaryNetworkJoinSubnetV4)
	if err != nil {
		return nil, err
	}
	_, v6cidr, err := net.ParseCIDR(types.UserDefinedPrimaryNetworkJoinSubnetV6)
	if err != nil {
		return nil, err
	}
	joinSubnets := []*net.IPNet{v4cidr, v6cidr}
	if strings.TrimSpace(joinSubnet) == "" {
		// user has not specified a value; pick the default
		return joinSubnets, nil
	}

	// user has provided some value; so let's validate and ensure we can use them
	joinSubnetCIDREntries, err := config.ParseClusterSubnetEntriesWithDefaults(joinSubnet, 0, 0)
	if err != nil {
		return nil, err
	}
	for _, joinSubnetCIDREntry := range joinSubnetCIDREntries {
		if knet.IsIPv4CIDR(joinSubnetCIDREntry.CIDR) {
			joinSubnets[0] = joinSubnetCIDREntry.CIDR
		} else {
			joinSubnets[1] = joinSubnetCIDREntry.CIDR
		}
	}
	return joinSubnets, nil
}

func parseTransitSubnet(netconfRole, transitSubnet string) ([]*net.IPNet, error) {
	transitSubnets := []*net.IPNet{}
	if netconfRole != types.NetworkRolePrimary {
		// only primary networks can have transit subnet
		return transitSubnets, nil
	}
	return parseSubnetList(transitSubnet)
}

func getIPMode(subnets []config.CIDRNetworkEntry) (bool, bool) {
	var ipv6Mode, ipv4Mode bool
	for _, subnet := range subnets {
		if knet.IsIPv6CIDR(subnet.CIDR) {
			ipv6Mode = true
		} else {
			ipv4Mode = true
		}
	}
	return ipv4Mode, ipv6Mode
}

// GetNADName returns key of NetAttachDefInfo.NetAttachDefs map, also used as Pod annotation key
func GetNADName(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

// GetIndexedNADKey returns key of NetAttachDefInfo.NetAttachDefs map for multiple identical NADs, also used as Pod annotation key
// the resulted nadKey example is like "ns/nad", "ns/nad/1", "ns/nad/2" etc...
func GetIndexedNADKey(nadName string, n int) string {
	if n == 0 {
		return nadName
	}
	return fmt.Sprintf("%s/%d", nadName, n)
}

// GetNadFromIndexedNADKey returns NAD name part from a nadName key (with or without index)
func GetNadFromIndexedNADKey(nadKey string) (string, int, error) {
	parts := strings.Split(nadKey, "/")

	if len(parts) == 3 {
		// Attempt to parse the integer part
		num, err := strconv.Atoi(parts[2])
		if err != nil {
			return "", 0, fmt.Errorf("malformed index for NAD key %s: %v", nadKey, err)
		}
		return fmt.Sprintf("%s/%s", parts[0], parts[1]), num, nil
	}

	if len(parts) == 2 {
		return fmt.Sprintf("%s/%s", parts[0], parts[1]), 0, nil
	}

	return "", 0, fmt.Errorf("malformed NAD key %s, expect in the form of namespace/name{/index}", nadKey)
}

// GetUserDefinedNetworkPrefix gets the string used as prefix of the logical entities
// of the User Defined Network of the given network name, in the form of <netName>_.
//
// Note that for port_group and address_set, it does not allow the '-' character,
// which will be replaced with ".". Also replace "/" in the nadName with "."
func GetUserDefinedNetworkPrefix(netName string) string {
	name := strings.ReplaceAll(netName, "-", ".")
	name = strings.ReplaceAll(name, "/", ".")
	return name + "_"
}

func NewNetInfo(netconf *ovncnitypes.NetConf) (NetInfo, error) {
	return newNetInfo(netconf)
}

func newNetInfo(netconf *ovncnitypes.NetConf) (MutableNetInfo, error) {
	if netconf.Name == types.DefaultNetworkName {
		return &DefaultNetInfo{}, nil
	}
	var ni MutableNetInfo
	var err error
	switch netconf.Topology {
	case types.Layer3Topology:
		ni, err = newLayer3NetConfInfo(netconf)
	case types.Layer2Topology:
		ni, err = newLayer2NetConfInfo(netconf)
	case types.LocalnetTopology:
		ni, err = newLocalnetNetConfInfo(netconf)
	default:
		// other topology NAD can be supported later
		return nil, fmt.Errorf("topology %s not supported", netconf.Topology)
	}
	if err != nil {
		return nil, err
	}
	if ni.IsPrimaryNetwork() && ni.IsUserDefinedNetwork() {
		ipv4Mode, ipv6Mode := ni.IPMode()
		if ipv4Mode && !config.IPv4Mode {
			return nil, fmt.Errorf("network %s is attempting to use ipv4 subnets but the cluster does not support ipv4", ni.GetNetworkName())
		}
		if ipv6Mode && !config.IPv6Mode {
			return nil, fmt.Errorf("network %s is attempting to use ipv6 subnets but the cluster does not support ipv6", ni.GetNetworkName())
		}
	}
	return ni, nil
}

// GetAnnotatedNetworkName gets the network name annotated by cluster manager
// nad controller
func GetAnnotatedNetworkName(netattachdef *nettypes.NetworkAttachmentDefinition) string {
	if netattachdef == nil {
		return ""
	}
	if netattachdef.Name == config.Default.ClusterDefaultNetworkNAD.Name && netattachdef.Namespace == config.Default.ClusterDefaultNetworkNAD.Namespace {
		return types.DefaultNetworkName
	}
	return netattachdef.Annotations[types.OvnNetworkNameAnnotation]
}

// GetAnnotatedNetworkID gets the network ID annotated by cluster manager
// nad controller, or types.InvalidID when the NAD carries no network-id
// annotation. It returns an error when the annotation is not a valid integer.
func GetAnnotatedNetworkID(netattachdef *nettypes.NetworkAttachmentDefinition) (int, error) {
	annotation := netattachdef.Annotations[types.OvnNetworkIDAnnotation]
	if annotation == "" {
		return types.InvalidID, nil
	}
	id, err := strconv.Atoi(annotation)
	if err != nil {
		return types.InvalidID, fmt.Errorf("failed to parse annotated network ID %q: %w", annotation, err)
	}
	return id, nil
}

// ParseNADInfo parses config in NAD spec and return a NetAttachDefInfo object for User Defined Networks
func ParseNADInfo(nad *nettypes.NetworkAttachmentDefinition) (NetInfo, error) {
	netconf, err := ParseNetConf(nad)
	if err != nil {
		return nil, err
	}

	nadName := GetNADName(nad.Namespace, nad.Name)
	if err := ValidateNetConf(nadName, netconf); err != nil {
		return nil, err
	}

	id := types.InvalidID
	n, err := newNetInfo(netconf)
	if err != nil {
		return nil, err
	}
	if n.GetNetworkName() == types.DefaultNetworkName {
		id = types.DefaultNetworkID
	}
	annotatedID, err := GetAnnotatedNetworkID(nad)
	if err != nil {
		return nil, err
	}
	if annotatedID != types.InvalidID {
		id = annotatedID
	}
	n.SetNetworkID(id)

	if nad.Annotations[types.OvnNetworkTunnelKeysAnnotation] != "" {
		tunnelKeys, err := ParseTunnelKeysAnnotation(nad.Annotations[types.OvnNetworkTunnelKeysAnnotation])
		if err != nil {
			return nil, fmt.Errorf("failed to parse annotated tunnel keys: %w", err)
		}
		n.SetTunnelKeys(tunnelKeys)
	}
	return n, nil
}

// ParseNetConf parses config in NAD spec for User Defined Networks
func ParseNetConf(netattachdef *nettypes.NetworkAttachmentDefinition) (*ovncnitypes.NetConf, error) {
	netconf, err := config.ParseNetConf([]byte(netattachdef.Spec.Config))
	if err != nil {
		if err.Error() == ErrorAttachDefNotOvnManaged.Error() {
			return nil, err
		}
		return nil, fmt.Errorf("error parsing Network Attachment Definition %s/%s: %v", netattachdef.Namespace, netattachdef.Name, err)
	}

	nadName := GetNADName(netattachdef.Namespace, netattachdef.Name)
	if err := ValidateNetConf(nadName, netconf); err != nil {
		return nil, err
	}

	return netconf, nil
}

func ValidateNetConf(nadName string, netconf *ovncnitypes.NetConf) error {
	if netconf.Name != types.DefaultNetworkName {
		if netconf.NADName != nadName {
			return fmt.Errorf("net-attach-def name (%s) is inconsistent with config (%s)", nadName, netconf.NADName)
		}
	}

	if err := config.ValidateNetConfNameFields(netconf); err != nil {
		return err
	}

	// Persistent IPs preserve OVN-Kubernetes-allocated addresses, so they
	// require OVN-K IPAM with cluster-wide subnets on a non-layer3 topology.
	// The CRD rules reject these at admission, so this covers hand-written NADs.
	if netconf.AllowPersistentIPs {
		if netconf.Topology == types.Layer3Topology {
			return fmt.Errorf("layer3 topology does not allow persistent IPs")
		}
		if netconf.Subnets == "" {
			return fmt.Errorf("error parsing Network Attachment Definition %s: allowPersistentIPs requires "+
				"OVN-Kubernetes-managed IPAM (the subnets attribute must be set)", nadName)
		}
	}

	if netconf.Role != "" && netconf.Role != types.NetworkRoleSecondary && netconf.Topology == types.LocalnetTopology {
		return fmt.Errorf("unexpected network field \"role\" %s for \"localnet\" topology, "+
			"localnet topology does not allow network roles to be set since its always a secondary network", netconf.Role)
	}

	if netconf.Role != "" && netconf.Role != types.NetworkRolePrimary && netconf.Role != types.NetworkRoleSecondary {
		return fmt.Errorf("invalid network role value %s", netconf.Role)
	}

	if err := validateMacSecurity(netconf); err != nil {
		return fmt.Errorf("invalid MAC security mode: %w", err)
	}

	if netconf.IPAM.Type != "" && netconf.IPAM.Type != types.IPAMTypeDHCP {
		return fmt.Errorf("error parsing Network Attachment Definition %s: %w", nadName, ErrorUnsupportedIPAMKey)
	}
	if netconf.IPAM.Type == types.IPAMTypeDHCP && netconf.Topology != types.LocalnetTopology {
		return fmt.Errorf("error parsing Network Attachment Definition %s: ipam.type %q is only supported with localnet topology",
			nadName, netconf.IPAM.Type)
	}
	// subnets enables OVN-K IPAM, which would compete with the external DHCP
	// server for address ownership. The CUDN CEL rules already forbid this
	// combination; enforce it here for hand-written NADs.
	if netconf.IPAM.Type == types.IPAMTypeDHCP && netconf.Subnets != "" {
		return fmt.Errorf("error parsing Network Attachment Definition %s: ipam.type %q cannot be used together with the subnets attribute; "+
			"addresses are assigned by the external DHCP server", nadName, netconf.IPAM.Type)
	}

	// Validate transport if specified
	if netconf.Transport != "" &&
		netconf.Transport != types.NetworkTransportNoOverlay &&
		netconf.Transport != types.NetworkTransportEVPN {
		return fmt.Errorf("invalid transport %q: must be one of %q", netconf.Transport, []string{
			types.NetworkTransportNoOverlay,
			types.NetworkTransportEVPN,
		})
	}
	if netconf.OutboundSNAT != "" {
		if netconf.Transport != types.NetworkTransportNoOverlay {
			return fmt.Errorf("outboundSNAT is only valid when transport is %q", types.NetworkTransportNoOverlay)
		}
		if netconf.OutboundSNAT != types.NoOverlaySNATEnabled &&
			netconf.OutboundSNAT != types.NoOverlaySNATDisabled {
			return fmt.Errorf("invalid outboundSNAT %q: must be one of %q", netconf.OutboundSNAT, []string{
				types.NoOverlaySNATEnabled,
				types.NoOverlaySNATDisabled,
			})
		}
	}
	if netconf.Uplink != "" && config.Gateway.Mode != config.GatewayModeShared {
		return fmt.Errorf("uplink %q is supported only in shared gateway mode", netconf.Uplink)
	}

	if netconf.JoinSubnet != "" && netconf.Topology == types.LocalnetTopology {
		return fmt.Errorf("localnet topology does not allow specifying join-subnet as services are not supported")
	}

	if netconf.Role == types.NetworkRolePrimary && netconf.Subnets == "" && netconf.Topology == types.Layer2Topology {
		return fmt.Errorf("the subnet attribute must be defined for layer2 primary user defined networks")
	}

	if netconf.InfrastructureSubnets != "" && netconf.Topology != types.Layer2Topology {
		return fmt.Errorf("infrastructureSubnets is only supported for layer2 topology")
	}

	if netconf.ReservedSubnets != "" && netconf.Topology != types.Layer2Topology {
		return fmt.Errorf("reservedSubnets is only supported for layer2 topology")
	}

	if netconf.DefaultGatewayIPs != "" && netconf.Topology != types.Layer2Topology {
		return fmt.Errorf("defaultGatewayIPs is only supported for layer2 topology")
	}

	if netconf.TransitSubnet == "" && netconf.Role == types.NetworkRolePrimary && netconf.Topology == types.Layer2Topology {
		klog.Warningf("transitSubnet is not specified for layer2 primary NAD %s, dynamic transit subnet will be used", netconf.Name)
		if err := SetTransitSubnets(netconf); err != nil {
			return fmt.Errorf("failed to set dynamic transit subnet for layer2 primary NAD %s: %v", netconf.Name, err)
		}
	}

	if netconf.Topology != types.LocalnetTopology && netconf.Name != types.DefaultNetworkName {
		if _, _, err := SubnetOverlapCheck(netconf); err != nil {
			return fmt.Errorf("invalid subnet configuration: %w", err)
		}
	}

	return nil
}

func validateMacSecurity(netconf *ovncnitypes.NetConf) error {
	if netconf.MACSecurityMode == "" {
		return nil
	}
	validValues := []string{types.MACSecurityModeEnabled, types.MACSecurityModeDisabled}
	if !slices.Contains(validValues, netconf.MACSecurityMode) {
		return fmt.Errorf("invalid value, must be one of: %v", validValues)
	}

	// Enabled mode preserves current behavior, enforcing MAC spoof protection at the LSP level.
	if netconf.MACSecurityMode == types.MACSecurityModeEnabled {
		return nil
	}

	if netconf.MACSecurityMode == types.MACSecurityModeDisabled {
		if netconf.Role == types.NetworkRolePrimary {
			return fmt.Errorf("only supported on secondary networks")
		}
		supportedTopologies := []string{types.Layer2Topology, types.LocalnetTopology}
		if !slices.Contains(supportedTopologies, netconf.Topology) {
			return fmt.Errorf("unsupported topology, must be one of %v", supportedTopologies)
		}
		if netconf.Subnets != "" {
			return fmt.Errorf("cannot be used when IPAM is enabled (subnets are specified)")
		}
		if netconf.AllowPersistentIPs {
			return fmt.Errorf("cannot be used with allowPersistentIPs, which require IPAM to be enabled")
		}
	}

	return nil
}

// SubnetOverlapCheck validates whether user-configured networks (e.g. POD and join subnet) mentioned in
// a net-attach-def with topology "layer2" and "layer3" overlaps with internal and reserved networks
// (e.g. ClusterSubnets, ServiceCIDRs, join subnet, etc.).
// It also considers excluded subnets mentioned in a net-attach-def.
func SubnetOverlapCheck(netconf *ovncnitypes.NetConf) (*net.IPNet, *net.IPNet, error) {
	allSubnets := config.NewConfigSubnets()
	for _, subnet := range config.Default.ClusterSubnets {
		allSubnets.Append(config.ConfigSubnetCluster, subnet.CIDR)
	}
	for _, subnet := range config.Kubernetes.ServiceCIDRs {
		allSubnets.Append(config.ConfigSubnetService, subnet)
	}
	_, v4JoinCIDR, _ := net.ParseCIDR(config.Gateway.V4JoinSubnet)
	_, v6JoinCIDR, _ := net.ParseCIDR(config.Gateway.V6JoinSubnet)

	allSubnets.Append(config.ConfigSubnetJoin, v4JoinCIDR)
	allSubnets.Append(config.ConfigSubnetJoin, v6JoinCIDR)

	_, v4MasqueradeCIDR, _ := net.ParseCIDR(config.Gateway.V4MasqueradeSubnet)
	_, v6MasqueradeCIDR, _ := net.ParseCIDR(config.Gateway.V6MasqueradeSubnet)

	allSubnets.Append(config.ConfigSubnetMasquerade, v4MasqueradeCIDR)
	allSubnets.Append(config.ConfigSubnetMasquerade, v6MasqueradeCIDR)

	// Layer3 network only uses pre-defined transit subnets
	if netconf.Topology == types.Layer3Topology {
		_, v4TransitCIDR, _ := net.ParseCIDR(config.ClusterManager.V4TransitSubnet)
		_, v6TransitCIDR, _ := net.ParseCIDR(config.ClusterManager.V6TransitSubnet)

		allSubnets.Append(config.ConfigSubnetTransit, v4TransitCIDR)
		allSubnets.Append(config.ConfigSubnetTransit, v6TransitCIDR)
	}

	ni, err := NewNetInfo(netconf)
	if err != nil {
		return nil, nil, fmt.Errorf("error while parsing subnets: %v", err)
	}
	for _, subnet := range ni.Subnets() {
		allSubnets.Append(config.UserDefinedSubnets, subnet.CIDR)
	}
	for _, subnet := range ni.JoinSubnets() {
		allSubnets.Append(config.UserDefinedJoinSubnet, subnet)
	}
	// dynamic transit subnets are only set for primary Layer2 UDNs for now
	for _, subnet := range ni.TransitSubnets() {
		allSubnets.Append(config.ConfigSubnetTransit, subnet)
	}
	if ni.ExcludeSubnets() != nil {
		for i, configSubnet := range allSubnets.Subnets {
			if IsContainedInAnyCIDR(configSubnet.Subnet, ni.ExcludeSubnets()...) {
				allSubnets.Subnets = append(allSubnets.Subnets[:i], allSubnets.Subnets[i+1:]...)
			}
		}
	}
	subnet1, subnet2, err := allSubnets.CheckForOverlaps()
	if err != nil {
		return subnet1, subnet2, fmt.Errorf("pod or join subnet overlaps with already configured internal subnets: %w", err)
	}

	return nil, nil, nil
}

// getPodNADToNetworkMapping sees if the given pod needs to plumb over this given network specified by netconf,
// and return the matching NetworkSelectionElement if any exists.
//
// Return value:
//
//		bool: if this Pod is on this Network; true or false
//		map[string]*nettypes.NetworkSelectionElement: all NetworkSelectionElement that the pod requests
//		    for the specified network, keyed by NAD key. NAD keys are of the form "namespace/name"
//	        for the first attachment of a NAD, and "namespace/name/<idx>" (idx start from 1) for
//	        additional attachments of the same NAD on the same pod. Note multiple NADs of the same
//	        network are allowed on one pod. They can be of different NAD Name or the same NAD Name.
//		error:  error in case of failure
//
// getNetworkNameForNADKey may be nil only for default-network lookups.
// For UDN lookups, it must be provided to validate NAD keys.
func getPodNADToNetworkMapping(
	pod *corev1.Pod,
	nInfo NetInfo,
	getNetworkNameForNADKey func(nadKey string) string,
) (bool, map[string]*nettypes.NetworkSelectionElement, error) {
	if pod.Spec.HostNetwork {
		return false, nil, nil
	}

	networkSelections := map[string]*nettypes.NetworkSelectionElement{}
	podDesc := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if !nInfo.IsUserDefinedNetwork() {
		network, err := GetK8sPodDefaultNetworkSelection(pod)
		if err != nil {
			// multus won't add this Pod if this fails, should never happen
			return false, nil, fmt.Errorf("error getting default-network's network-attachment for pod %s: %v", podDesc, err)
		}
		if network != nil {
			networkSelections[GetNADName(network.Namespace, network.Name)] = network
		}
		return true, networkSelections, nil
	}

	if getNetworkNameForNADKey == nil {
		return false, nil, fmt.Errorf("UDN mapping requires a network resolver")
	}

	return getPodNADToNetworkMappingWithPredicate(pod, nInfo, func(nadName string) bool {
		networkName := getNetworkNameForNADKey(nadName)
		return networkName != "" && networkName == nInfo.GetNetworkName()
	})
}

// GetUDNPodNADToNetworkMapping returns pod network selections for the specified UDN,
// validating NAD keys via the provided resolver.
func GetUDNPodNADToNetworkMapping(
	pod *corev1.Pod,
	nInfo NetInfo,
	getNetworkNameForNADKey func(nadKey string) string,
) (bool, map[string]*nettypes.NetworkSelectionElement, error) {
	if getNetworkNameForNADKey == nil {
		return false, nil, fmt.Errorf("UDN mapping requires a network resolver")
	}
	return getPodNADToNetworkMapping(pod, nInfo, getNetworkNameForNADKey)
}

// GetDefaultPodNADToNetworkMapping returns default-network selections for a pod.
// This should only be used by the default network controller.
func GetDefaultPodNADToNetworkMapping(pod *corev1.Pod) (bool, map[string]*nettypes.NetworkSelectionElement, error) {
	return getPodNADToNetworkMapping(pod, &DefaultNetInfo{}, nil)
}

func getPodNADToNetworkMappingWithPredicate(
	pod *corev1.Pod,
	nInfo NetInfo,
	nadMatches func(nadKey string) bool,
) (bool, map[string]*nettypes.NetworkSelectionElement, error) {
	// For non-default network controller, try to see if its name exists in the Pod's k8s.v1.cni.cncf.io/networks, if no,
	// return false;
	allNetworks, err := GetK8sPodAllNetworkSelections(pod)
	if err != nil {
		return false, nil, err
	}

	networkSelections := map[string]*nettypes.NetworkSelectionElement{}
	// Get map of per-NAD NetworkSelectionElement, if there are multiple NetworkSelectionElements of the same NAD,
	// calculate numbers of network elements of that same NAD.
	nNADs := map[string]int{}
	for _, network := range allNetworks {
		nadNamespace := network.Namespace
		if nadNamespace == "" {
			nadNamespace = pod.Namespace
		}
		nadName := GetNADName(nadNamespace, network.Name)
		if !nadMatches(nadName) {
			continue
		}
		if nInfo.IsPrimaryNetwork() {
			return false, nil, fmt.Errorf("unexpected primary network %q specified with a NetworkSelectionElement %+v", nInfo.GetNetworkName(), network)
		}

		// for multiple NetworkSelectionElements of the same NAD, set its nadName to indexed nadName
		cnt := nNADs[nadName]
		nNADs[nadName] = cnt + 1
		networkSelections[GetIndexedNADKey(nadName, cnt)] = network
	}

	if len(networkSelections) == 0 {
		return false, nil, nil
	}

	return true, networkSelections, nil
}

// overrideActiveNSEWithDefaultNSE overrides the provided active NetworkSelectionElement with the IP and MAC requests from
// the default NetworkSelectionElement after validating its namespace and name.
func overrideActiveNSEWithDefaultNSE(defaultNSE, activeNSE *nettypes.NetworkSelectionElement, topologyType string) error {
	expected := config.Default.ClusterDefaultNetworkNAD
	if defaultNSE.Namespace != expected.Namespace {
		return fmt.Errorf("unexpected default NSE namespace %q, expected %q", defaultNSE.Namespace, expected.Namespace)
	}
	if defaultNSE.Name != expected.Name {
		return fmt.Errorf("unexpected default NSE name %q, expected %q", defaultNSE.Name, expected.Name)
	}
	// Layer2 accepts requested IPs and MACs. Layer3 accepts only a MAC to retain node-local IPAM.
	switch topologyType {
	case types.Layer2Topology:
		// Limit the static ip and mac requests to the layer2 primary UDN
		activeNSE.IPRequest = defaultNSE.IPRequest
		activeNSE.MacRequest = defaultNSE.MacRequest
	case types.Layer3Topology:
		// Limit mac requests to the layer3 primary UDN
		activeNSE.MacRequest = defaultNSE.MacRequest
	}
	return nil
}

// GetPodNADToNetworkMappingWithActiveNetwork resolves the pod's NAD attachments using nInfo (the NAD's NetInfo).
// If activeNetwork is provided and matches nInfo's network, it adds the namespace's active primary NAD selection
// (and any requested default-network IP/MAC details) to the returned mapping.
func GetPodNADToNetworkMappingWithActiveNetwork(
	pod *corev1.Pod,
	nInfo NetInfo,
	activeNetwork NetInfo,
	getNetworkNameForNADKey func(nadKey string) string,
	getPrimaryNADForNamespace func(namespace string) (string, error),
) (bool, map[string]*nettypes.NetworkSelectionElement, error) {
	// scan network selection elements using the resolver to validate attachments
	on, networkSelections, err := getPodNADToNetworkMapping(pod, nInfo, getNetworkNameForNADKey)
	if err != nil {
		return false, nil, err
	}

	if activeNetwork == nil {
		return on, networkSelections, nil
	}

	if activeNetwork.IsDefault() ||
		activeNetwork.GetNetworkName() != nInfo.GetNetworkName() ||
		nInfo.TopologyType() == types.LocalnetTopology {
		return on, networkSelections, nil
	}

	if getPrimaryNADForNamespace == nil {
		return false, nil, fmt.Errorf("missing primary NAD resolver for network %q", nInfo.GetNetworkName())
	}

	primaryNADKey, err := getPrimaryNADForNamespace(pod.Namespace)
	if err != nil {
		return false, nil, fmt.Errorf("failed to get primary NAD for namespace %q: %w", pod.Namespace, err)
	}
	if primaryNADKey == types.DefaultNetworkName {
		return false, nil, fmt.Errorf("no primary NAD found for namespace %q", pod.Namespace)
	}
	if networkName := getNetworkNameForNADKey(primaryNADKey); networkName == "" || networkName != nInfo.GetNetworkName() {
		return false, nil, fmt.Errorf("primary NAD %q does not match network %q for namespace %q", primaryNADKey, nInfo.GetNetworkName(), pod.Namespace)
	}

	nadNamespace, nadName, err := cache.SplitMetaNamespaceKey(primaryNADKey)
	if err != nil {
		return false, nil, fmt.Errorf("failed to split NAD key %q: %w", primaryNADKey, err)
	}

	if len(networkSelections) == 0 {
		networkSelections = map[string]*nettypes.NetworkSelectionElement{}
	}

	activeNSE := &nettypes.NetworkSelectionElement{
		Namespace: nadNamespace,
		Name:      nadName,
	}

	isPersistentIPsPrimaryNetwork := nInfo.IsPrimaryNetwork() && AllowsPersistentIPs(nInfo)
	var defaultNSE *nettypes.NetworkSelectionElement
	if isPersistentIPsPrimaryNetwork || IsPreconfiguredUDNAddressesEnabled() {
		defaultNSE, err = GetK8sPodDefaultNetworkSelection(pod)
		if err != nil {
			return false, nil, fmt.Errorf("failed getting default-network annotation for pod %q: %w", pod.Namespace+"/"+pod.Name, err)
		}
	}

	if isPersistentIPsPrimaryNetwork {
		// 'k8s.ovn.org/primary-udn-ipamclaim' annotation has been deprecated. Maintain backward compatibility by
		// using it as a fallback; when defaultNSE.IPAMClaimReference is set, it takes precedence.
		if ipamClaimName, wasPersistentIPRequested := pod.Annotations[DeprecatedOvnUDNIPAMClaimName]; wasPersistentIPRequested {
			activeNSE.IPAMClaimReference = ipamClaimName
		}
		if defaultNSE != nil && defaultNSE.IPAMClaimReference != "" {
			activeNSE.IPAMClaimReference = defaultNSE.IPAMClaimReference
		}
	}

	// Feature gate integration: EnablePreconfiguredUDNAddresses controls default network IP/MAC transfer to active network,
	// we don't need to explicitly check this is primary UDN since the "active network" concept is exactly that.
	if IsPreconfiguredUDNAddressesEnabled() && defaultNSE != nil {
		if err := overrideActiveNSEWithDefaultNSE(defaultNSE, activeNSE, activeNetwork.TopologyType()); err != nil {
			return false, nil, err
		}
	}

	networkSelections[primaryNADKey] = activeNSE
	return true, networkSelections, nil
}

func IsMultiNetworkPoliciesSupportEnabled() bool {
	return config.OVNKubernetesFeature.EnableMultiNetwork && config.OVNKubernetesFeature.EnableMultiNetworkPolicy
}

func IsNetworkSegmentationSupportEnabled() bool {
	return config.OVNKubernetesFeature.EnableMultiNetwork && config.OVNKubernetesFeature.EnableNetworkSegmentation
}

func IsNetworkConnectEnabled() bool {
	return IsNetworkSegmentationSupportEnabled() && config.OVNKubernetesFeature.EnableNetworkConnect
}

func IsUplinkEnabled() bool {
	return IsNetworkSegmentationSupportEnabled() && config.OVNKubernetesFeature.EnableUplink
}

func IsRouteAdvertisementsEnabled() bool {
	// for now, we require multi-network to be enabled because we rely on NADs,
	// even for the default network
	return config.OVNKubernetesFeature.EnableMultiNetwork && config.OVNKubernetesFeature.EnableRouteAdvertisements
}

func IsEVPNEnabled() bool {
	return IsRouteAdvertisementsEnabled() && config.Gateway.Mode == config.GatewayModeLocal && config.OVNKubernetesFeature.EnableEVPN
}

// IsPreconfiguredUDNAddressesEnabled indicates if user defined IPs / MAC
// addresses can be set in primary UDNs
func IsPreconfiguredUDNAddressesEnabled() bool {
	return IsNetworkSegmentationSupportEnabled() && config.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses
}

func DoesNetworkRequireIPAM(netInfo NetInfo) bool {
	return !((netInfo.TopologyType() == types.Layer2Topology || netInfo.TopologyType() == types.LocalnetTopology) && len(netInfo.Subnets()) == 0)
}

// DoesNetworkHaveDiscoverablePodIPs returns true when pod IPs on this network
// are known to ovnkube ie. allocated by its own IPAM, or DHCP-learned.
func DoesNetworkHaveDiscoverablePodIPs(netInfo NetInfo) bool {
	return DoesNetworkRequireIPAM(netInfo) || netInfo.IPAMType() == types.IPAMTypeDHCP
}

func DoesNetworkRequireTunnelIDs(netInfo NetInfo) bool {
	return netInfo.TopologyType() == types.Layer2Topology
}

func AllowsPersistentIPs(netInfo NetInfo) bool {
	switch {
	case netInfo.IsPrimaryNetwork():
		return netInfo.TopologyType() == types.Layer2Topology && netInfo.AllowsPersistentIPs()

	case netInfo.IsUserDefinedNetwork():
		return (netInfo.TopologyType() == types.Layer2Topology || netInfo.TopologyType() == types.LocalnetTopology) &&
			netInfo.AllowsPersistentIPs()

	default:
		return false
	}
}

// IsPodNetworkAdvertisedAtNode returns true if the pod network is advertised at
// the given node. For networks with NoOverlay or EVPN transport this always
// returns true as those networks should always be advertised.
func IsPodNetworkAdvertisedAtNode(netInfo NetInfo, node string) bool {
	switch netInfo.Transport() {
	case types.NetworkTransportNoOverlay, types.NetworkTransportEVPN:
		return true
	default:
		return len(netInfo.GetPodNetworkAdvertisedOnNodeVRFs(node)) > 0
	}
}

// IsPodNetworkAdvertisedAtNodeDefaultVRF returns true if the pod network is
// advertised at the given node in the default VRF. For networks with EVPN
// transport this always returns false as those networks should not be
// advertised in the default VRF.
func IsPodNetworkAdvertisedAtNodeDefaultVRF(netInfo NetInfo, node string) bool {
	switch netInfo.Transport() {
	case types.NetworkTransportEVPN:
		return false
	default:
		return slices.Contains(netInfo.GetPodNetworkAdvertisedOnNodeVRFs(node), types.DefaultNetworkName)
	}
}

func GetNetworkVRFName(netInfo NetInfo) string {
	if netInfo.GetNetworkName() == types.DefaultNetworkName {
		return types.DefaultNetworkName
	}
	// use the CUDN network name as the VRF name if possible
	udnNamespace, udnName := ParseNetworkName(netInfo.GetNetworkName())
	if udnName != "" && udnNamespace == "" {
		if vrfDeviceName := cudnVRFDeviceName(udnName); vrfDeviceName != "" {
			return vrfDeviceName
		}
	}
	return idDerivedVRFName(netInfo.GetNetworkID())
}

// cudnVRFDeviceName returns the given CUDN name if it can be used verbatim as
// the network's VRF device name, or an empty string when the ID-derived
// fallback name must be used instead: when the name is longer than the maximum
// device name length or is the default network name.
func cudnVRFDeviceName(cudnName string) string {
	if len(cudnName) > types.MaxInterfaceNameLength || cudnName == types.DefaultNetworkName {
		return ""
	}
	return cudnName
}

// GetCUDNVRFName returns the VRF device name for the CUDN with the given name
// and network ID. It returns an empty string when the name can't be derived
// yet, that is when the ID-derived fallback name applies but the network ID is
// not known (InvalidID).
func GetCUDNVRFName(cudnName string, networkID int) string {
	if vrfDeviceName := cudnVRFDeviceName(cudnName); vrfDeviceName != "" {
		return vrfDeviceName
	}
	return GetUDNVRFName(networkID)
}

// GetUDNVRFName returns the VRF device name for a namespaced UDN given its
// network ID, or an empty string when the ID is not known yet (InvalidID):
// the VRF name of a namespaced UDN is always ID-derived.
func GetUDNVRFName(networkID int) string {
	if networkID == types.InvalidID {
		return ""
	}
	return idDerivedVRFName(networkID)
}

// idDerivedVRFName returns the network's ID-derived VRF device name; it is the
// single source of the format, shared by the node-side device creation path
// (GetNetworkVRFName) and the name published on the CUDN status
// (GetCUDNVRFName).
func idDerivedVRFName(networkID int) string {
	return fmt.Sprintf("%s%d%s", types.UDNVRFDevicePrefix, networkID, types.UDNVRFDeviceSuffix)
}

// ParseNetworkIDFromVRFName in the format generated by GetNetworkVRFName.
// Returns InvalidID otherwise.
func ParseNetworkIDFromVRFName(vrf string) int {
	if !strings.HasPrefix(vrf, types.UDNVRFDevicePrefix) {
		return types.InvalidID
	}
	if !strings.HasSuffix(vrf, types.UDNVRFDeviceSuffix) {
		return types.InvalidID
	}
	id, err := strconv.Atoi(vrf[len(types.UDNVRFDevicePrefix) : len(vrf)-len(types.UDNVRFDeviceSuffix)])
	if err != nil {
		return types.InvalidID
	}
	return id
}

// GetNetworkRole returns the role of this controller's
// network for the given pod
// Expected values are:
// (1) "primary" if this network is the primary network of the pod.
//
//	The "default" network is the primary network of any pod usually
//	unless user-defined-network-segmentation feature has been activated.
//	If network segmentation feature is enabled then any user defined
//	network can be the primary network of the pod.
//
// (2) "secondary" if this network is the secondary network of the pod.
//
//	Only user defined networks can be secondary networks for a pod.
//
// (3) "infrastructure-locked" is applicable only to "default" network if
//
//	a user defined network is the "primary" network for this pod. This
//	signifies the "default" network is only used for probing and
//	is otherwise locked for all intents and purposes.
//
// (4) "none" if the pod has no networks on this controller
func GetNetworkRole(
	controllerNetInfo NetInfo,
	getPrimaryNADForNamespace func(namespace string) (string, error),
	getNetworkNameForNADKey func(nadKey string) string,
	pod *corev1.Pod,
) (string, error) {
	if getNetworkNameForNADKey == nil {
		return "", fmt.Errorf("getNetworkNameForNADKey is required")
	}

	// no network segmentation enabled, and is default controller, must be default network
	if !IsNetworkSegmentationSupportEnabled() && controllerNetInfo.IsDefault() {
		return types.NetworkRolePrimary, nil
	}

	var err error
	// controller is serving primary network or is default, we need to get the active network
	if controllerNetInfo.IsPrimaryNetwork() || controllerNetInfo.IsDefault() {
		// check if primary NAD exists
		primaryNAD, err := getPrimaryNADForNamespace(pod.Namespace)
		if err != nil {
			return "", err
		}
		if controllerNetInfo.IsDefault() {
			if primaryNAD == types.DefaultNetworkName {
				return types.NetworkRolePrimary, nil
			}
			return types.NetworkRoleInfrastructure, nil
		}

		if networkName := getNetworkNameForNADKey(primaryNAD); networkName != "" {
			if networkName == controllerNetInfo.GetNetworkName() {
				return types.NetworkRolePrimary, nil
			}
			return types.NetworkRoleNone, nil
		}

		// this is a primary network controller, and it does not have the pod's primary NAD
		return types.NetworkRoleNone, nil
	}

	// at this point the controller must be a secondary network
	allNetworks, err := GetK8sPodAllNetworkSelections(pod)
	if err != nil {
		return "", fmt.Errorf("failed to get pod network mapping: %w", err)
	}
	for _, network := range allNetworks {
		nadNamespace := network.Namespace
		if nadNamespace == "" {
			nadNamespace = pod.Namespace
		}
		nadKey := GetNADName(nadNamespace, network.Name)
		networkName := getNetworkNameForNADKey(nadKey)
		if networkName == "" {
			continue
		}
		if networkName == controllerNetInfo.GetNetworkName() {
			return types.NetworkRoleSecondary, nil
		}
	}
	return types.NetworkRoleNone, nil
}

// (C)UDN network name generation functions must ensure the absence of name conflicts between all (C)UDNs.
// We use underscore as a separator as it is not allowed in k8s namespaces and names.
// Network name is then used by GetUserDefinedNetworkPrefix function to generate db object names.
// GetUserDefinedNetworkPrefix replaces some characters in the network name to ensure correct db object names,
// so the network name must be also unique after these replacements.

func GenerateUDNNetworkName(namespace, name string) string {
	return namespace + "_" + name
}

func GenerateCUDNNetworkName(name string) string {
	return types.CUDNPrefix + name
}

// ParseNetworkName parses the network name into UDN namespace and name OR CUDN name.
// If udnName is empty, then given string is not a (C)UDN-generated network name.
// If udnNamespace is empty, then udnName is a CUDN name.
// As any (C)UDN network can also be just NAD-generated network, there is no guarantee that given network
// is a (C)UDN network. It needs an additional check from the kapi-server.
// This function has a copy in go-controller/observability-lib/sampledecoder/sample_decoder.go
// Please update together with this function.
func ParseNetworkName(networkName string) (udnNamespace, udnName string) {
	if strings.HasPrefix(networkName, types.CUDNPrefix) {
		return "", networkName[len(types.CUDNPrefix):]
	}
	parts := strings.Split(networkName, "_")
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", ""
}

// allocateInfrastructureIPs attempts to allocate gateway and management IPs from infrastructure subnets.
// It searches through infrastructure subnets sequentially for each network subnet, allocating the first
// available IP as gateway IP (if not already provided) and the second available IP as management IP.
// If it isn't able to find the IPs in the infrastructure subnets it defers back to default values.
func allocateInfrastructureIPs(netconf *ovncnitypes.NetConf) ([]net.IP, []net.IP, error) {
	// Parse network subnets
	subnets, err := parseNetworkSubnets(netconf.Subnets, types.Layer2Topology)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse subnets: %w", err)
	}

	// Parse infrastructure subnets
	infra, err := parseSubnetList(netconf.InfrastructureSubnets)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse infrastructure subnets: %w", err)
	}

	// Parse default gateway IPs
	defaultGatewayIPs, err := ParseIPList(netconf.DefaultGatewayIPs)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid default gateway IPs: %w", err)
	}

	var gatewayIPs, managementIPs []net.IP

	for _, netSubnet := range subnets {
		isIPV6 := knet.IsIPv6CIDR(netSubnet.CIDR)
		var gwIP, mgmtIP net.IP

		gwIP, _ = MatchFirstIPFamily(isIPV6, defaultGatewayIPs)
		infraSubnets := MatchAllIPNetFamily(isIPV6, infra)

		// Try to allocate the gateway/management IPs from infra subnets
		// Build set of IPs to exclude (network IP, broadcast IP, and existing gateway IP)
		// NOTE: Even though the network IP is technically allowed for IPv6  we exclude it to be consistent the legacy behavior
		excludeIPs := sets.New(netSubnet.CIDR.IP.String())
		if !isIPV6 {
			// Exclude the broadcast IP for IPv4, there is no broadcast IP for IPv6
			excludeIPs.Insert(SubnetBroadcastIP(*netSubnet.CIDR).String())
		}

		if len(infraSubnets) > 0 {
			if gwIP != nil {
				excludeIPs.Insert(gwIP.String())
			}

			// Find gateway IP if not already set
			if gwIP == nil {
				gwIP = getFirstAvailableIP(infraSubnets, excludeIPs)
				if gwIP != nil {
					excludeIPs.Insert(gwIP.String())
				}
			}

			// Find management IP
			mgmtIP = getFirstAvailableIP(infraSubnets, excludeIPs)
		}

		// fallback to defaults
		if gwIP == nil {
			gwIP = GetNodeGatewayIfAddr(netSubnet.CIDR).IP
		}
		if mgmtIP == nil {
			mgmtIP = GetNodeManagementIfAddr(netSubnet.CIDR).IP
			if mgmtIP.Equal(gwIP) {
				// Corner case: if the default management IP(.2) conflicts with the custom gateway IP,
				// use the .1 address for the management IP.
				mgmtIP = GetNodeGatewayIfAddr(netSubnet.CIDR).IP
			}
		}

		gatewayIPs = append(gatewayIPs, gwIP)
		managementIPs = append(managementIPs, mgmtIP)
	}

	return gatewayIPs, managementIPs, nil
}

// getFirstAvailableIP returns the first available IP in the given subnets that is not in the exclude set.
// Returns nil if no available IP is found.
func getFirstAvailableIP(subnets []*net.IPNet, excludeIPs sets.Set[string]) net.IP {
	for _, subnet := range subnets {
		for currentIP := subnet.IP; subnet.Contains(currentIP); currentIP = iputils.NextIP(currentIP) {
			if !excludeIPs.Has(currentIP.String()) {
				return currentIP
			}
		}
	}
	return nil
}

func ParseTunnelKeysAnnotation(annotation string) ([]int, error) {
	tunnelKeys := []int{}
	if err := json.Unmarshal([]byte(annotation), &tunnelKeys); err != nil {
		return nil, fmt.Errorf("failed to parse annotated network tunnel keys: %w", err)
	}
	return tunnelKeys, nil
}

func FormatTunnelKeysAnnotation(tunnelKeys []int) (string, error) {
	annotationBytes, err := json.Marshal(tunnelKeys)
	if err != nil {
		return "", fmt.Errorf("failed to format tunnel keys annotation: %w", err)
	}
	return string(annotationBytes), nil
}

// SetTransitSubnets generates transit subnet for primary layer2 UDNs and sets for a given netconf.
// It should be called with the final version of netconf to make sure that util.SubnetOverlapCheck(netconf) passes.
func SetTransitSubnets(netconf *ovncnitypes.NetConf) error {
	transitSubnets := []string{}
	for _, subnetStr := range strings.Split(netconf.Subnets, ",") {
		_, subnet, err := net.ParseCIDR(subnetStr)
		if err != nil {
			return fmt.Errorf("can't generate transit subnets: failed to parse CIDR %q: %w", subnetStr, err)
		}
		transitSubnet, err := getTransitSubnet(netconf, knet.IsIPv4CIDR(subnet))
		if err != nil {
			return err
		}
		transitSubnets = append(transitSubnets, transitSubnet)
	}
	netconf.TransitSubnet = strings.Join(transitSubnets, ",")
	return nil
}

func getTransitSubnet(netconf *ovncnitypes.NetConf, isIPv4 bool) (string, error) {
	var transitSubnet *net.IPNet
	var err error
	if isIPv4 {
		_, transitSubnet, err = net.ParseCIDR(config.ClusterManager.V4TransitSubnet)
	} else {
		_, transitSubnet, err = net.ParseCIDR(config.ClusterManager.V6TransitSubnet)
	}
	if err != nil {
		return "", fmt.Errorf("can't generate transit subnets: failed to parse default transit subnet: %w", err)
	}
	// repeat until we find a non-overlapping subnet,
	// but limit the number of iterations to avoid infinite loop
	for i := 0; i < 10; i++ {
		// only add current transit subnet to the netconf for overlap check (for requested ipFamily),
		// final assignment should be done for all ipFamilies outside this function.
		netconf.TransitSubnet = transitSubnet.String()
		// check if there is subnet overlap
		subnet1, subnet2, err := SubnetOverlapCheck(netconf)
		if err == nil {
			return transitSubnet.String(), nil
		}
		if subnet1 == nil || subnet2 == nil || subnet1.String() != transitSubnet.String() && subnet2.String() != transitSubnet.String() {
			// there is another problem with the config
			// or overlap is not with transit subnet
			return "", err
		}
		transitSubnet = getFirstNonOverlappingSubnet(subnet1, subnet2, transitSubnet.Mask)
	}
	// if the previous loop didn't return the result, we failed to find a non-overlapping subnet
	return "", fmt.Errorf("can't generate transit subnets: failed to find non-overlapping transit subnet after 10 attempts")
}

// getFirstNonOverlappingSubnet finds the first subnet with the same netmask as netMask that does not overlap with either subnet1 or subnet2.
// It expects that subnet1 and subnet2 overlap with each other, and that netmask is one of the two subnet's netmask.
// If these conditions are not met, the result won't be correct.
func getFirstNonOverlappingSubnet(subnet1, subnet2 *net.IPNet, netMask net.IPMask) *net.IPNet {
	// find the bigger network, and get the first subnet outside of it with the same netmask as default transit subnet
	subnet1MaskSize, _ := subnet1.Mask.Size()
	subnet2MaskSize, _ := subnet2.Mask.Size()
	// bigger mask size means smaller network
	baseSubnet := subnet1
	if subnet2MaskSize < subnet1MaskSize {
		baseSubnet = subnet2
	}
	// now find the first subnet outside the baseSubnet with the same mask
	baseSubnetLastIP := GetLastIPOfSubnet(baseSubnet, 0)
	nextIP := iputils.NextIP(baseSubnetLastIP.IP)
	return &net.IPNet{IP: nextIP, Mask: netMask}
}

func CheckNetworksOverlap(netInfos []NetInfo) error {
	subnets := make([][]*net.IPNet, len(netInfos))
	for i, ni := range netInfos {
		subnets[i] = GetAllClusterSubnetsFromEntries(ni.Subnets())
	}
	for i, si := range subnets {
		for j := 0; j < i; j++ {
			sj := subnets[j]
			if NetworksOverlap(si, sj) {
				return fmt.Errorf("network %s with subnets %v and network %s with subnets %v",
					netInfos[i].GetNetworkName(), si, netInfos[j].GetNetworkName(), sj)
			}
		}
	}
	return nil
}

// CheckSubnetOverlapWithNetworks checks whether the given subnets overlap with
// any of the given networks' subnets, transit subnets, or join subnets.
// subnetsName is used in the error message to explain where those subnets are coming from.
func CheckSubnetOverlapWithNetworks(subnets []*net.IPNet, subnetsName string, networks []NetInfo) error {
	for _, network := range networks {
		if NetworksOverlap(subnets, GetAllClusterSubnetsFromEntries(network.Subnets())) {
			return fmt.Errorf("network %s with subnets %v and %s with subnets %v",
				network.GetNetworkName(), network.Subnets(), subnetsName, subnets)
		}
		if NetworksOverlap(subnets, network.TransitSubnets()) {
			return fmt.Errorf("network %s with transit subnets %v and %s with subnets %v",
				network.GetNetworkName(), network.TransitSubnets(), subnetsName, subnets)
		}
		if NetworksOverlap(subnets, network.JoinSubnets()) {
			return fmt.Errorf("network %s with join subnets %v and %s with subnets %v",
				network.GetNetworkName(), network.JoinSubnets(), subnetsName, subnets)
		}
	}
	return nil
}

// CheckSubnetOverlapWithClusterSubnets checks whether the given subnet overlaps with
// cluster service CIDRs, or masquerade subnets.
// subnetsName is used in the error message to explain where those subnets are coming from.
func CheckSubnetOverlapWithClusterSubnets(subnets []*net.IPNet, subnetsName string) error {
	if NetworksOverlap(subnets, config.Kubernetes.ServiceCIDRs) {
		return fmt.Errorf("%s with subnets %v and %s with subnets %v",
			config.ConfigSubnetService, config.Kubernetes.ServiceCIDRs,
			subnetsName, subnets)
	}
	_, v4MasqueradeCIDR, _ := net.ParseCIDR(config.Gateway.V4MasqueradeSubnet)
	_, v6MasqueradeCIDR, _ := net.ParseCIDR(config.Gateway.V6MasqueradeSubnet)

	if NetworksOverlap(subnets, []*net.IPNet{v4MasqueradeCIDR, v6MasqueradeCIDR}) {
		return fmt.Errorf("%s with subnets %v and %s with subnets %v",
			config.ConfigSubnetMasquerade, []*net.IPNet{v4MasqueradeCIDR, v6MasqueradeCIDR},
			subnetsName, subnets)
	}
	return nil
}

// GetNetworkScopedSwitchToRouterPortNameFromSwitchName returns the
// switch-to-router port name for the given switch name.
func GetNetworkScopedSwitchToRouterPortNameFromSwitchName(switchName string) string {
	if strings.HasSuffix(switchName, types.OVNLayer2Switch) && config.Layer2UsesTransitRouter {
		return types.SwitchToTransitRouterPrefix + switchName
	}
	return types.SwitchToRouterPrefix + switchName
}

package util

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/exp/maps"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	knet "k8s.io/utils/net"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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
	IsDefault() bool
	IsPrimaryNetwork() bool
	IsSecondary() bool
	TopologyType() string
	MTU() int
	IPMode() (bool, bool)
	Subnets() []config.CIDRNetworkEntry
	ExcludeSubnets() []*net.IPNet
	JoinSubnetV4() *net.IPNet
	JoinSubnetV6() *net.IPNet
	JoinSubnets() []*net.IPNet
	Vlan() uint
	AllowsPersistentIPs() bool
	PhysicalNetworkName() string

	// dynamic information, can change over time
	GetNADs() []string
	HasNAD(nadName string) bool
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
	GetNamespaces() []string
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
	GetNetworkScopedClusterSubnetSNATMatch(nodeName string) string

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
	case *secondaryNetInfo:
		return t.copy()
	default:
		panic(fmt.Errorf("unrecognized type %T", t))
	}
}

func reconcilable(netInfo NetInfo) ReconcilableNetInfo {
	switch t := netInfo.GetNetInfo().(type) {
	case *DefaultNetInfo:
		return t
	case *secondaryNetInfo:
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
	id int

	nads                     sets.Set[string]
	podNetworkAdvertisements map[string][]string
	eipAdvertisements        map[string][]string

	// information generated from previous fields, not used in comparisons

	// namespaces from nads
	namespaces sets.Set[string]
}

func mutable(netInfo NetInfo) *mutableNetInfo {
	switch t := netInfo.GetNetInfo().(type) {
	case *DefaultNetInfo:
		return &t.mutableNetInfo
	case *secondaryNetInfo:
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
	l.RLock()
	defer l.RUnlock()
	r.RLock()
	defer r.RUnlock()
	return reflect.DeepEqual(l.id, r.id) &&
		reflect.DeepEqual(l.nads, r.nads) &&
		reflect.DeepEqual(l.podNetworkAdvertisements, r.podNetworkAdvertisements) &&
		reflect.DeepEqual(l.eipAdvertisements, r.eipAdvertisements)
}

func (l *mutableNetInfo) copyFrom(r *mutableNetInfo) {
	aux := mutableNetInfo{}
	r.RLock()
	aux.id = r.id
	aux.nads = r.nads.Clone()
	aux.setPodNetworkAdvertisedOnVRFs(r.podNetworkAdvertisements)
	aux.setEgressIPAdvertisedAtNodes(r.eipAdvertisements)
	aux.namespaces = r.namespaces.Clone()
	r.RUnlock()
	l.Lock()
	defer l.Unlock()
	l.id = aux.id
	l.nads = aux.nads
	l.podNetworkAdvertisements = aux.podNetworkAdvertisements
	l.eipAdvertisements = aux.eipAdvertisements
	l.namespaces = aux.namespaces
}

func (nInfo *mutableNetInfo) GetNetworkID() int {
	return nInfo.id
}

func (nInfo *mutableNetInfo) SetNetworkID(id int) {
	nInfo.Lock()
	defer nInfo.Unlock()
	nInfo.id = id
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

// GetNADs returns all the NADs associated with this network
func (nInfo *mutableNetInfo) GetNADs() []string {
	nInfo.RLock()
	defer nInfo.RUnlock()
	return nInfo.getNads().UnsortedList()
}

// HasNAD returns true if the given NAD exists, used
// to check if the network needs to be plumbed over
func (nInfo *mutableNetInfo) HasNAD(nadName string) bool {
	nInfo.RLock()
	defer nInfo.RUnlock()
	return nInfo.getNads().Has(nadName)
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

func (nInfo *mutableNetInfo) GetNamespaces() []string {
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
// The boolean indicates if this secondary network is
// meant to be the primary network for the pod. Since default
// network is never a secondary network this is always false.
// This cannot be true if IsSecondary() is not true.
func (nInfo *DefaultNetInfo) IsPrimaryNetwork() bool {
	return false
}

// IsSecondary returns if this network is secondary
func (nInfo *DefaultNetInfo) IsSecondary() bool {
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

func (nInfo *DefaultNetInfo) GetNetworkScopedClusterSubnetSNATMatch(nodeName string) string {
	return ""
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

// JoinSubnets returns the secondaryNetInfo's joinsubnet values (both v4&v6)
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

// SecondaryNetInfo holds the network name information for secondary network if non-nil
type secondaryNetInfo struct {
	mutableNetInfo

	netName string
	// Should this secondary network be used
	// as the pod's primary network?
	primaryNetwork     bool
	topology           string
	mtu                int
	vlan               uint
	allowPersistentIPs bool

	ipv4mode, ipv6mode bool
	subnets            []config.CIDRNetworkEntry
	excludeSubnets     []*net.IPNet
	joinSubnets        []*net.IPNet

	physicalNetworkName string
}

func (nInfo *secondaryNetInfo) GetNetInfo() NetInfo {
	return nInfo
}

// GetNetworkName returns the network name
func (nInfo *secondaryNetInfo) GetNetworkName() string {
	return nInfo.netName
}

// IsDefault always returns false for all secondary networks.
func (nInfo *secondaryNetInfo) IsDefault() bool {
	return false
}

// IsPrimaryNetwork returns if this secondary network
// should be used as the primaryNetwork for the pod
// to achieve native network segmentation
func (nInfo *secondaryNetInfo) IsPrimaryNetwork() bool {
	return nInfo.primaryNetwork
}

// IsSecondary returns if this network is secondary
func (nInfo *secondaryNetInfo) IsSecondary() bool {
	return true
}

// GetNetworkScopedName returns a network scoped name from the provided one
// appropriate to use globally.
func (nInfo *secondaryNetInfo) GetNetworkScopedName(name string) string {
	return fmt.Sprintf("%s%s", nInfo.getPrefix(), name)
}

// RemoveNetworkScopeFromName removes the name without the network scope added
// by a previous call to GetNetworkScopedName
func (nInfo *secondaryNetInfo) RemoveNetworkScopeFromName(name string) string {
	// for the default network, names are not scoped
	return strings.Trim(name, nInfo.getPrefix())
}

func (nInfo *secondaryNetInfo) GetNetworkScopedK8sMgmtIntfName(nodeName string) string {
	return GetK8sMgmtIntfName(nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *secondaryNetInfo) GetNetworkScopedClusterRouterName() string {
	return nInfo.GetNetworkScopedName(types.OVNClusterRouter)
}

func (nInfo *secondaryNetInfo) GetNetworkScopedGWRouterName(nodeName string) string {
	return GetGatewayRouterFromNode(nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *secondaryNetInfo) GetNetworkScopedSwitchName(nodeName string) string {
	// In Layer2Topology there is just one global switch
	if nInfo.TopologyType() == types.Layer2Topology {
		return fmt.Sprintf("%s%s", nInfo.getPrefix(), types.OVNLayer2Switch)
	}
	return nInfo.GetNetworkScopedName(nodeName)
}

func (nInfo *secondaryNetInfo) GetNetworkScopedJoinSwitchName() string {
	return nInfo.GetNetworkScopedName(types.OVNJoinSwitch)
}

func (nInfo *secondaryNetInfo) GetNetworkScopedExtSwitchName(nodeName string) string {
	return GetExtSwitchFromNode(nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *secondaryNetInfo) GetNetworkScopedPatchPortName(bridgeID, nodeName string) string {
	return GetPatchPortName(bridgeID, nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *secondaryNetInfo) GetNetworkScopedExtPortName(bridgeID, nodeName string) string {
	return GetExtPortName(bridgeID, nInfo.GetNetworkScopedName(nodeName))
}

func (nInfo *secondaryNetInfo) GetNetworkScopedLoadBalancerName(lbName string) string {
	return nInfo.GetNetworkScopedName(lbName)
}

func (nInfo *secondaryNetInfo) GetNetworkScopedLoadBalancerGroupName(lbGroupName string) string {
	return nInfo.GetNetworkScopedName(lbGroupName)
}

func (nInfo *secondaryNetInfo) GetNetworkScopedClusterSubnetSNATMatch(nodeName string) string {
	if nInfo.TopologyType() != types.Layer2Topology {
		return ""
	}
	return fmt.Sprintf("outport == %q", types.GWRouterToExtSwitchPrefix+nInfo.GetNetworkScopedGWRouterName(nodeName))
}

// getPrefix returns if the logical entities prefix for this network
func (nInfo *secondaryNetInfo) getPrefix() string {
	return GetSecondaryNetworkPrefix(nInfo.netName)
}

// TopologyType returns the topology type
func (nInfo *secondaryNetInfo) TopologyType() string {
	return nInfo.topology
}

// MTU returns the layer3NetConfInfo's MTU value
func (nInfo *secondaryNetInfo) MTU() int {
	return nInfo.mtu
}

// Vlan returns the Vlan value
func (nInfo *secondaryNetInfo) Vlan() uint {
	return nInfo.vlan
}

// AllowsPersistentIPs returns the defaultNetConfInfo's AllowPersistentIPs value
func (nInfo *secondaryNetInfo) AllowsPersistentIPs() bool {
	return nInfo.allowPersistentIPs
}

// PhysicalNetworkName returns the user provided physical network name value
func (nInfo *secondaryNetInfo) PhysicalNetworkName() string {
	return nInfo.physicalNetworkName
}

// IPMode returns the ipv4/ipv6 mode
func (nInfo *secondaryNetInfo) IPMode() (bool, bool) {
	return nInfo.ipv4mode, nInfo.ipv6mode
}

// Subnets returns the Subnets value
func (nInfo *secondaryNetInfo) Subnets() []config.CIDRNetworkEntry {
	return nInfo.subnets
}

// ExcludeSubnets returns the ExcludeSubnets value
func (nInfo *secondaryNetInfo) ExcludeSubnets() []*net.IPNet {
	return nInfo.excludeSubnets
}

// JoinSubnetV4 returns the defaultNetConfInfo's JoinSubnetV4 value
// call when ipv4mode=true
func (nInfo *secondaryNetInfo) JoinSubnetV4() *net.IPNet {
	if len(nInfo.joinSubnets) == 0 {
		return nil // localnet topology
	}
	return nInfo.joinSubnets[0]
}

// JoinSubnetV6 returns the secondaryNetInfo's JoinSubnetV6 value
// call when ipv6mode=true
func (nInfo *secondaryNetInfo) JoinSubnetV6() *net.IPNet {
	if len(nInfo.joinSubnets) <= 1 {
		return nil // localnet topology
	}
	return nInfo.joinSubnets[1]
}

// JoinSubnets returns the secondaryNetInfo's joinsubnet values (both v4&v6)
// used from Equals (since localnet doesn't have joinsubnets to compare nil v/s nil
// we need this util)
func (nInfo *secondaryNetInfo) JoinSubnets() []*net.IPNet {
	return nInfo.joinSubnets
}

func (nInfo *secondaryNetInfo) canReconcile(other NetInfo) bool {
	if (nInfo == nil) != (other == nil) {
		return false
	}
	if nInfo == nil && other == nil {
		return true
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

	lessCIDRNetworkEntry := func(a, b config.CIDRNetworkEntry) bool { return a.String() < b.String() }
	if !cmp.Equal(nInfo.subnets, other.Subnets(), cmpopts.SortSlices(lessCIDRNetworkEntry)) {
		return false
	}

	lessIPNet := func(a, b net.IPNet) bool { return a.String() < b.String() }
	if !cmp.Equal(nInfo.excludeSubnets, other.ExcludeSubnets(), cmpopts.SortSlices(lessIPNet)) {
		return false
	}
	return cmp.Equal(nInfo.joinSubnets, other.JoinSubnets(), cmpopts.SortSlices(lessIPNet))
}

func (nInfo *secondaryNetInfo) copy() *secondaryNetInfo {
	// everything here is immutable
	c := &secondaryNetInfo{
		netName:             nInfo.netName,
		primaryNetwork:      nInfo.primaryNetwork,
		topology:            nInfo.topology,
		mtu:                 nInfo.mtu,
		vlan:                nInfo.vlan,
		allowPersistentIPs:  nInfo.allowPersistentIPs,
		ipv4mode:            nInfo.ipv4mode,
		ipv6mode:            nInfo.ipv6mode,
		subnets:             nInfo.subnets,
		excludeSubnets:      nInfo.excludeSubnets,
		joinSubnets:         nInfo.joinSubnets,
		physicalNetworkName: nInfo.physicalNetworkName,
	}
	// copy mutables
	c.mutableNetInfo.copyFrom(&nInfo.mutableNetInfo)

	return c
}

func newLayer3NetConfInfo(netconf *ovncnitypes.NetConf) (MutableNetInfo, error) {
	subnets, _, err := parseSubnets(netconf.Subnets, "", types.Layer3Topology)
	if err != nil {
		return nil, err
	}
	joinSubnets, err := parseJoinSubnet(netconf.JoinSubnet)
	if err != nil {
		return nil, err
	}
	ni := &secondaryNetInfo{
		netName:        netconf.Name,
		primaryNetwork: netconf.Role == types.NetworkRolePrimary,
		topology:       types.Layer3Topology,
		subnets:        subnets,
		joinSubnets:    joinSubnets,
		mtu:            netconf.MTU,
		mutableNetInfo: mutableNetInfo{
			id:   InvalidID,
			nads: sets.Set[string]{},
		},
	}
	ni.ipv4mode, ni.ipv6mode = getIPMode(subnets)
	return ni, nil
}

func newLayer2NetConfInfo(netconf *ovncnitypes.NetConf) (MutableNetInfo, error) {
	subnets, excludes, err := parseSubnets(netconf.Subnets, netconf.ExcludeSubnets, types.Layer2Topology)
	if err != nil {
		return nil, fmt.Errorf("invalid %s netconf %s: %v", netconf.Topology, netconf.Name, err)
	}
	joinSubnets, err := parseJoinSubnet(netconf.JoinSubnet)
	if err != nil {
		return nil, err
	}
	ni := &secondaryNetInfo{
		netName:            netconf.Name,
		primaryNetwork:     netconf.Role == types.NetworkRolePrimary,
		topology:           types.Layer2Topology,
		subnets:            subnets,
		joinSubnets:        joinSubnets,
		excludeSubnets:     excludes,
		mtu:                netconf.MTU,
		allowPersistentIPs: netconf.AllowPersistentIPs,
		mutableNetInfo: mutableNetInfo{
			id:   InvalidID,
			nads: sets.Set[string]{},
		},
	}
	ni.ipv4mode, ni.ipv6mode = getIPMode(subnets)
	return ni, nil
}

func newLocalnetNetConfInfo(netconf *ovncnitypes.NetConf) (MutableNetInfo, error) {
	subnets, excludes, err := parseSubnets(netconf.Subnets, netconf.ExcludeSubnets, types.LocalnetTopology)
	if err != nil {
		return nil, fmt.Errorf("invalid %s netconf %s: %v", netconf.Topology, netconf.Name, err)
	}

	ni := &secondaryNetInfo{
		netName:             netconf.Name,
		topology:            types.LocalnetTopology,
		subnets:             subnets,
		excludeSubnets:      excludes,
		mtu:                 netconf.MTU,
		vlan:                uint(netconf.VLANID),
		allowPersistentIPs:  netconf.AllowPersistentIPs,
		physicalNetworkName: netconf.PhysicalNetworkName,
		mutableNetInfo: mutableNetInfo{
			id:   InvalidID,
			nads: sets.Set[string]{},
		},
	}
	ni.ipv4mode, ni.ipv6mode = getIPMode(subnets)
	return ni, nil
}

func parseSubnets(subnetsString, excludeSubnetsString, topology string) ([]config.CIDRNetworkEntry, []*net.IPNet, error) {
	var parseSubnets func(clusterSubnetCmd string) ([]config.CIDRNetworkEntry, error)
	switch topology {
	case types.Layer3Topology:
		// For L3 topology, subnet is validated
		parseSubnets = config.ParseClusterSubnetEntries
	case types.LocalnetTopology, types.Layer2Topology:
		// For L2 topologies, host specific prefix length is ignored (using 0 as
		// prefix length)
		parseSubnets = func(clusterSubnetCmd string) ([]config.CIDRNetworkEntry, error) {
			return config.ParseClusterSubnetEntriesWithDefaults(clusterSubnetCmd, 0, 0)
		}
	}

	var subnets []config.CIDRNetworkEntry
	if strings.TrimSpace(subnetsString) != "" {
		var err error
		subnets, err = parseSubnets(subnetsString)
		if err != nil {
			return nil, nil, err
		}
	}

	var excludeIPNets []*net.IPNet
	if strings.TrimSpace(excludeSubnetsString) != "" {
		// For L2 topologies, host specific prefix length is ignored (using 0 as
		// prefix length)
		excludeSubnets, err := config.ParseClusterSubnetEntriesWithDefaults(excludeSubnetsString, 0, 0)
		if err != nil {
			return nil, nil, err
		}
		excludeIPNets = make([]*net.IPNet, 0, len(excludeSubnets))
		for _, excludeSubnet := range excludeSubnets {
			found := false
			for _, subnet := range subnets {
				if ContainsCIDR(subnet.CIDR, excludeSubnet.CIDR) {
					found = true
					break
				}
			}
			if !found {
				return nil, nil, fmt.Errorf("the provided network subnets %v do not contain exluded subnets %v",
					subnets, excludeSubnet.CIDR)
			}
			excludeIPNets = append(excludeIPNets, excludeSubnet.CIDR)
		}
	}

	return subnets, excludeIPNets, nil
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

// GetSecondaryNetworkPrefix gets the string used as prefix of the logical entities
// of the secondary network of the given network name, in the form of <netName>_.
//
// Note that for port_group and address_set, it does not allow the '-' character,
// which will be replaced with ".". Also replace "/" in the nadName with "."
func GetSecondaryNetworkPrefix(netName string) string {
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
	if ni.IsPrimaryNetwork() && ni.IsSecondary() {
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
	if netattachdef.Name == types.DefaultNetworkName && netattachdef.Namespace == config.Kubernetes.OVNConfigNamespace {
		return types.DefaultNetworkName
	}
	return netattachdef.Annotations[types.OvnNetworkNameAnnotation]
}

// ParseNADInfo parses config in NAD spec and return a NetAttachDefInfo object for secondary networks
func ParseNADInfo(netattachdef *nettypes.NetworkAttachmentDefinition) (NetInfo, error) {
	netconf, err := ParseNetConf(netattachdef)
	if err != nil {
		return nil, err
	}

	nadName := GetNADName(netattachdef.Namespace, netattachdef.Name)
	if err := ValidateNetConf(nadName, netconf); err != nil {
		return nil, err
	}

	return NewNetInfo(netconf)
}

// ParseNetConf parses config in NAD spec for secondary networks
func ParseNetConf(netattachdef *nettypes.NetworkAttachmentDefinition) (*ovncnitypes.NetConf, error) {
	netconf, err := config.ParseNetConf([]byte(netattachdef.Spec.Config))
	if err != nil {
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

	if netconf.AllowPersistentIPs && netconf.Topology == types.Layer3Topology {
		return fmt.Errorf("layer3 topology does not allow persistent IPs")
	}

	if netconf.Role != "" && netconf.Role != types.NetworkRoleSecondary && netconf.Topology == types.LocalnetTopology {
		return fmt.Errorf("unexpected network field \"role\" %s for \"localnet\" topology, "+
			"localnet topology does not allow network roles to be set since its always a secondary network", netconf.Role)
	}

	if netconf.Role != "" && netconf.Role != types.NetworkRolePrimary && netconf.Role != types.NetworkRoleSecondary {
		return fmt.Errorf("invalid network role value %s", netconf.Role)
	}

	if netconf.IPAM.Type != "" {
		return fmt.Errorf("error parsing Network Attachment Definition %s: %w", nadName, ErrorUnsupportedIPAMKey)
	}

	if netconf.JoinSubnet != "" && netconf.Topology == types.LocalnetTopology {
		return fmt.Errorf("localnet topology does not allow specifying join-subnet as services are not supported")
	}

	if netconf.Role == types.NetworkRolePrimary && netconf.Subnets == "" && netconf.Topology == types.Layer2Topology {
		return fmt.Errorf("the subnet attribute must be defined for layer2 primary user defined networks")
	}

	if netconf.Topology != types.LocalnetTopology && netconf.Name != types.DefaultNetworkName {
		if err := subnetOverlapCheck(netconf); err != nil {
			return fmt.Errorf("invalid subnet configuration: %w", err)
		}
	}

	return nil
}

// subnetOverlapCheck validates whether POD and join subnet mentioned in a net-attach-def with
// topology "layer2" and "layer3" does not overlap with ClusterSubnets, ServiceCIDRs, join subnet,
// and masquerade subnet. It also considers excluded subnets mentioned in a net-attach-def.
func subnetOverlapCheck(netconf *ovncnitypes.NetConf) error {
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

	ni, err := NewNetInfo(netconf)
	if err != nil {
		return fmt.Errorf("error while parsing subnets: %v", err)
	}
	for _, subnet := range ni.Subnets() {
		allSubnets.Append(config.UserDefinedSubnets, subnet.CIDR)
	}

	for _, subnet := range ni.JoinSubnets() {
		allSubnets.Append(config.UserDefinedJoinSubnet, subnet)
	}
	if ni.ExcludeSubnets() != nil {
		for i, configSubnet := range allSubnets.Subnets {
			if IsContainedInAnyCIDR(configSubnet.Subnet, ni.ExcludeSubnets()...) {
				allSubnets.Subnets = append(allSubnets.Subnets[:i], allSubnets.Subnets[i+1:]...)
			}
		}
	}
	err = allSubnets.CheckForOverlaps()
	if err != nil {
		return fmt.Errorf("pod or join subnet overlaps with already configured internal subnets: %v", err)
	}

	return nil
}

// GetPodNADToNetworkMapping sees if the given pod needs to plumb over this given network specified by netconf,
// and return the matching NetworkSelectionElement if any exists.
//
// Return value:
//
//	bool: if this Pod is on this Network; true or false
//	map[string]*nettypes.NetworkSelectionElement: all NetworkSelectionElement that pod is requested
//	    for the specified network, key is NADName. Note multiple NADs of the same network are allowed
//	    on one pod, as long as they are of different NADName.
//	error:  error in case of failure
func GetPodNADToNetworkMapping(pod *kapi.Pod, nInfo NetInfo) (bool, map[string]*nettypes.NetworkSelectionElement, error) {
	if pod.Spec.HostNetwork {
		return false, nil, nil
	}

	networkSelections := map[string]*nettypes.NetworkSelectionElement{}
	podDesc := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if !nInfo.IsSecondary() {
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

	// For non-default network controller, try to see if its name exists in the Pod's k8s.v1.cni.cncf.io/networks, if no,
	// return false;
	allNetworks, err := GetK8sPodAllNetworkSelections(pod)
	if err != nil {
		return false, nil, err
	}

	for _, network := range allNetworks {
		nadName := GetNADName(network.Namespace, network.Name)
		if nInfo.HasNAD(nadName) {
			if nInfo.IsPrimaryNetwork() {
				return false, nil, fmt.Errorf("unexpected primary network %q specified with a NetworkSelectionElement %+v", nInfo.GetNetworkName(), network)
			}
			if _, ok := networkSelections[nadName]; ok {
				return false, nil, fmt.Errorf("unexpected error: more than one of the same NAD %s specified for pod %s",
					nadName, podDesc)
			}
			networkSelections[nadName] = network
		}
	}

	if len(networkSelections) == 0 {
		return false, nil, nil
	}

	return true, networkSelections, nil
}

// GetPodNADToNetworkMappingWithActiveNetwork will call `GetPodNADToNetworkMapping` passing "nInfo" which correspond
// to the NetInfo representing the NAD, the resulting NetworkSelectingElements will be decorated with the ones
// from found active network
func GetPodNADToNetworkMappingWithActiveNetwork(pod *kapi.Pod, nInfo NetInfo, activeNetwork NetInfo) (bool, map[string]*nettypes.NetworkSelectionElement, error) {
	on, networkSelections, err := GetPodNADToNetworkMapping(pod, nInfo)
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

	// Add the active network to the NSE map if it is configured
	activeNetworkNADs := activeNetwork.GetNADs()
	if len(activeNetworkNADs) < 1 {
		return false, nil, fmt.Errorf("missing NADs at active network %q for namespace %q", activeNetwork.GetNetworkName(), pod.Namespace)
	}
	activeNetworkNADKey := strings.Split(activeNetworkNADs[0], "/")
	if len(networkSelections) == 0 {
		networkSelections = map[string]*nettypes.NetworkSelectionElement{}
	}
	networkSelections[activeNetworkNADs[0]] = &nettypes.NetworkSelectionElement{
		Namespace: activeNetworkNADKey[0],
		Name:      activeNetworkNADKey[1],
	}

	if nInfo.IsPrimaryNetwork() && AllowsPersistentIPs(nInfo) {
		ipamClaimName, wasPersistentIPRequested := pod.Annotations[OvnUDNIPAMClaimName]
		if wasPersistentIPRequested {
			networkSelections[activeNetworkNADs[0]].IPAMClaimReference = ipamClaimName
		}
	}

	return true, networkSelections, nil
}

func IsMultiNetworkPoliciesSupportEnabled() bool {
	return config.OVNKubernetesFeature.EnableMultiNetwork && config.OVNKubernetesFeature.EnableMultiNetworkPolicy
}

func IsNetworkSegmentationSupportEnabled() bool {
	return config.OVNKubernetesFeature.EnableMultiNetwork && config.OVNKubernetesFeature.EnableNetworkSegmentation
}

func IsRouteAdvertisementsEnabled() bool {
	// for now, we require multi-network to be enabled because we rely on NADs,
	// even for the default network
	return config.OVNKubernetesFeature.EnableMultiNetwork && config.OVNKubernetesFeature.EnableRouteAdvertisements
}

func DoesNetworkRequireIPAM(netInfo NetInfo) bool {
	return !((netInfo.TopologyType() == types.Layer2Topology || netInfo.TopologyType() == types.LocalnetTopology) && len(netInfo.Subnets()) == 0)
}

func DoesNetworkRequireTunnelIDs(netInfo NetInfo) bool {
	// Layer2Topology with IC require that we allocate tunnel IDs for each pod
	return netInfo.TopologyType() == types.Layer2Topology && config.OVNKubernetesFeature.EnableInterconnect
}

func AllowsPersistentIPs(netInfo NetInfo) bool {
	switch {
	case netInfo.IsPrimaryNetwork():
		return netInfo.TopologyType() == types.Layer2Topology && netInfo.AllowsPersistentIPs()

	case netInfo.IsSecondary():
		return (netInfo.TopologyType() == types.Layer2Topology || netInfo.TopologyType() == types.LocalnetTopology) &&
			netInfo.AllowsPersistentIPs()

	default:
		return false
	}
}

func IsPodNetworkAdvertisedAtNode(netInfo NetInfo, node string) bool {
	return len(netInfo.GetPodNetworkAdvertisedOnNodeVRFs(node)) > 0
}

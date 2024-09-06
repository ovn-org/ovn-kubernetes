package util

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

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

// BasicNetInfo is interface which holds basic network information
type BasicNetInfo interface {
	// basic network information
	GetNetworkName() string
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

	// utility methods
	Equals(BasicNetInfo) bool
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
}

// NetInfo correlates which NADs refer to a network in addition to the basic
// network information
type NetInfo interface {
	BasicNetInfo
	GetNADs() []string
	HasNAD(nadName string) bool
	SetNADs(nadName ...string)
	AddNADs(nadName ...string)
	DeleteNADs(nadName ...string)
}

type DefaultNetInfo struct{}

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

// GetNADs returns the NADs associated with the network, no op for default
// network
func (nInfo *DefaultNetInfo) GetNADs() []string {
	panic("unexpected call for default network")
}

// HasNAD returns true if the given NAD exists, already return true for
// default network
func (nInfo *DefaultNetInfo) HasNAD(nadName string) bool {
	panic("unexpected call for default network")
}

// SetNADs replaces the NADs associated with the network, no op for default
// network
func (nInfo *DefaultNetInfo) SetNADs(nadName ...string) {
	panic("unexpected call for default network")
}

// AddNAD adds the specified NAD, no op for default network
func (nInfo *DefaultNetInfo) AddNADs(nadName ...string) {
	panic("unexpected call for default network")
}

// DeleteNAD deletes the specified NAD, no op for default network
func (nInfo *DefaultNetInfo) DeleteNADs(nadName ...string) {
	panic("unexpected call for default network")
}

func (nInfo *DefaultNetInfo) Equals(netBasicInfo BasicNetInfo) bool {
	_, ok := netBasicInfo.(*DefaultNetInfo)
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

// SecondaryNetInfo holds the network name information for secondary network if non-nil
type secondaryNetInfo struct {
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

	// all net-attach-def NAD names for this network, used to determine if a pod needs
	// to be plumbed for this network
	sync.Mutex
	nadNames sets.Set[string]
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

// getPrefix returns if the logical entities prefix for this network
func (nInfo *secondaryNetInfo) getPrefix() string {
	return GetSecondaryNetworkPrefix(nInfo.netName)
}

// GetNADs returns all the NADs associated with this network
func (nInfo *secondaryNetInfo) GetNADs() []string {
	nInfo.Lock()
	defer nInfo.Unlock()
	return nInfo.nadNames.UnsortedList()
}

// HasNAD returns true if the given NAD exists, used
// to check if the network needs to be plumbed over
func (nInfo *secondaryNetInfo) HasNAD(nadName string) bool {
	nInfo.Lock()
	defer nInfo.Unlock()
	return nInfo.nadNames.Has(nadName)
}

// SetNADs replaces the NADs associated with the network
func (nInfo *secondaryNetInfo) SetNADs(nadName ...string) {
	nInfo.Lock()
	defer nInfo.Unlock()
	nInfo.nadNames = sets.New(nadName...)
}

// AddNAD adds the specified NAD
func (nInfo *secondaryNetInfo) AddNADs(nadName ...string) {
	nInfo.Lock()
	defer nInfo.Unlock()
	nInfo.nadNames.Insert(nadName...)
}

// DeleteNAD deletes the specified NAD
func (nInfo *secondaryNetInfo) DeleteNADs(nadName ...string) {
	nInfo.Lock()
	defer nInfo.Unlock()
	nInfo.nadNames.Delete(nadName...)
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

// Equals compares for equality this network information with the other
func (nInfo *secondaryNetInfo) Equals(other BasicNetInfo) bool {
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
	nInfo.Lock()
	defer nInfo.Unlock()

	// everything is immutable except the NADs
	c := &secondaryNetInfo{
		netName:            nInfo.netName,
		primaryNetwork:     nInfo.primaryNetwork,
		topology:           nInfo.topology,
		mtu:                nInfo.mtu,
		vlan:               nInfo.vlan,
		allowPersistentIPs: nInfo.allowPersistentIPs,
		ipv4mode:           nInfo.ipv4mode,
		ipv6mode:           nInfo.ipv6mode,
		subnets:            nInfo.subnets,
		excludeSubnets:     nInfo.excludeSubnets,
		joinSubnets:        nInfo.joinSubnets,
		nadNames:           nInfo.nadNames.Clone(),
	}

	return c
}

func newLayer3NetConfInfo(netconf *ovncnitypes.NetConf) (NetInfo, error) {
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
		nadNames:       sets.Set[string]{},
	}
	ni.ipv4mode, ni.ipv6mode = getIPMode(subnets)
	return ni, nil
}

func newLayer2NetConfInfo(netconf *ovncnitypes.NetConf) (NetInfo, error) {
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
		nadNames:           sets.Set[string]{},
	}
	ni.ipv4mode, ni.ipv6mode = getIPMode(subnets)
	return ni, nil
}

func newLocalnetNetConfInfo(netconf *ovncnitypes.NetConf) (NetInfo, error) {
	subnets, excludes, err := parseSubnets(netconf.Subnets, netconf.ExcludeSubnets, types.LocalnetTopology)
	if err != nil {
		return nil, fmt.Errorf("invalid %s netconf %s: %v", netconf.Topology, netconf.Name, err)
	}

	ni := &secondaryNetInfo{
		netName:            netconf.Name,
		topology:           types.LocalnetTopology,
		subnets:            subnets,
		excludeSubnets:     excludes,
		mtu:                netconf.MTU,
		vlan:               uint(netconf.VLANID),
		allowPersistentIPs: netconf.AllowPersistentIPs,
		nadNames:           sets.Set[string]{},
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
	if netconf.Name == types.DefaultNetworkName {
		return &DefaultNetInfo{}, nil
	}
	switch netconf.Topology {
	case types.Layer3Topology:
		return newLayer3NetConfInfo(netconf)
	case types.Layer2Topology:
		return newLayer2NetConfInfo(netconf)
	case types.LocalnetTopology:
		return newLocalnetNetConfInfo(netconf)
	default:
		// other topology NAD can be supported later
		return nil, fmt.Errorf("topology %s not supported", netconf.Topology)
	}
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
			return fmt.Errorf("invalid subnet cnfiguration: %w", err)
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

func CopyNetInfo(netInfo NetInfo) NetInfo {
	switch t := netInfo.(type) {
	case *DefaultNetInfo:
		// immutable
		return netInfo
	case *secondaryNetInfo:
		return t.copy()
	default:
		panic("program error: unrecognized NetInfo")
	}
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
		return false, nil, fmt.Errorf("missing NADs at active network '%s' for namesapce '%s'", activeNetwork.GetNetworkName(), pod.Namespace)
	}
	activeNetworkNADKey := strings.Split(activeNetworkNADs[0], "/")
	if len(networkSelections) == 0 {
		networkSelections = map[string]*nettypes.NetworkSelectionElement{}
	}
	networkSelections[activeNetworkNADs[0]] = &nettypes.NetworkSelectionElement{
		Namespace: activeNetworkNADKey[0],
		Name:      activeNetworkNADKey[1],
	}

	return true, networkSelections, nil
}
func IsMultiNetworkPoliciesSupportEnabled() bool {
	return config.OVNKubernetesFeature.EnableMultiNetwork && config.OVNKubernetesFeature.EnableMultiNetworkPolicy
}

func IsNetworkSegmentationSupportEnabled() bool {
	return config.OVNKubernetesFeature.EnableMultiNetwork && config.OVNKubernetesFeature.EnableNetworkSegmentation
}

func DoesNetworkRequireIPAM(netInfo NetInfo) bool {
	return !((netInfo.TopologyType() == types.Layer2Topology || netInfo.TopologyType() == types.LocalnetTopology) && len(netInfo.Subnets()) == 0)
}

func DoesNetworkRequireTunnelIDs(netInfo NetInfo) bool {
	// Layer2Topology with IC require that we allocate tunnel IDs for each pod
	return netInfo.TopologyType() == types.Layer2Topology && config.OVNKubernetesFeature.EnableInterconnect
}

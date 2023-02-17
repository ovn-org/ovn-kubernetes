package util

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	kapi "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
)

var ErrorAttachDefNotOvnManaged = errors.New("net-attach-def not managed by OVN")

// NetInfo is interface which holds network name information
// for default network, this is set to nil
type NetInfo interface {
	GetNetworkName() string
	IsSecondary() bool
	GetPrefix() string
	AddNAD(nadName string)
	DeleteNAD(nadName string)
	HasNAD(nadName string) bool
}

type DefaultNetInfo struct{}

// GetNetworkName returns the network name
func (nInfo *DefaultNetInfo) GetNetworkName() string {
	return types.DefaultNetworkName
}

// IsSecondary returns if this network is secondary
func (nInfo *DefaultNetInfo) IsSecondary() bool {
	return false
}

// GetPrefix returns if the logical entities prefix for this network
func (nInfo *DefaultNetInfo) GetPrefix() string {
	return ""
}

// AddNAD adds the specified NAD, no op for default network
func (nInfo *DefaultNetInfo) AddNAD(nadName string) {
	panic("unexpected call for default network")
}

// DeleteNAD deletes the specified NAD, no op for default network
func (nInfo *DefaultNetInfo) DeleteNAD(nadName string) {
	panic("unexpected call for default network")
}

// HasNAD returns true if the given NAD exists, already return true for
// default network
func (nInfo *DefaultNetInfo) HasNAD(nadName string) bool {
	panic("unexpected call for default network")
}

// SecondaryNetInfo holds the network name information for secondary network if non-nil
type SecondaryNetInfo struct {
	// network name
	netName string
	// all net-attach-def NAD names for this network, used to determine if a pod needs
	// to be plumbed for this network
	nadNames *sync.Map
}

// GetNetworkName returns the network name
func (nInfo *SecondaryNetInfo) GetNetworkName() string {
	return nInfo.netName
}

// IsSecondary returns if this network is secondary
func (nInfo *SecondaryNetInfo) IsSecondary() bool {
	return true
}

// GetPrefix returns if the logical entities prefix for this network
func (nInfo *SecondaryNetInfo) GetPrefix() string {
	return GetSecondaryNetworkPrefix(nInfo.netName)
}

// AddNAD adds the specified NAD
func (nInfo *SecondaryNetInfo) AddNAD(nadName string) {
	nInfo.nadNames.Store(nadName, true)
}

// DeleteNAD deletes the specified NAD
func (nInfo *SecondaryNetInfo) DeleteNAD(nadName string) {
	nInfo.nadNames.Delete(nadName)
}

// HasNAD returns true if the given NAD exists, used
// to check if the network needs to be plumbed over
func (nInfo *SecondaryNetInfo) HasNAD(nadName string) bool {
	_, ok := nInfo.nadNames.Load(nadName)
	return ok
}

// NetConfInfo is structure which holds specific per-network configuration
type NetConfInfo interface {
	CompareNetConf(NetConfInfo) bool
	TopologyType() string
	MTU() int
	Subnets() []string
}

// DefaultNetConfInfo is structure which holds specific default network information
type DefaultNetConfInfo struct{}

// CompareNetConf compares the defaultNetConfInfo with the given newNetConfInfo and returns true
// unless the given newNetConfInfo is not the type of DefaultNetConfInfo
func (defaultNetConfInfo *DefaultNetConfInfo) CompareNetConf(newNetConfInfo NetConfInfo) bool {
	_, ok := newNetConfInfo.(*DefaultNetConfInfo)
	if !ok {
		klog.V(5).Infof("New netconf is different, expect default network netconf")
		return false
	}
	return true
}

// TopologyType returns the defaultNetConfInfo's topology type which is empty
func (defaultNetConfInfo *DefaultNetConfInfo) TopologyType() string {
	return ""
}

// MTU returns the defaultNetConfInfo's MTU value
func (defaultNetConfInfo *DefaultNetConfInfo) MTU() int {
	return config.Default.MTU
}

// Subnets returns the defaultNetConfInfo's Subnets value
func (defaultNetConfInfo *DefaultNetConfInfo) Subnets() []string {
	return []string{config.Default.RawClusterSubnets}
}

func isSubnetsStringEqual(subnetsString, newSubnetsString string) bool {
	subnetsStringList := strings.Split(subnetsString, ",")
	newSubnetsStringList := strings.Split(newSubnetsString, ",")
	if len(subnetsStringList) != len(newSubnetsStringList) {
		return false
	}
	for index := range subnetsStringList {
		subnetsStringList[index] = strings.TrimSpace(subnetsStringList[index])
	}
	for index := range newSubnetsStringList {
		newSubnetsStringList[index] = strings.TrimSpace(newSubnetsStringList[index])
	}
	sort.Strings(subnetsStringList)
	sort.Strings(newSubnetsStringList)
	for i, subnetString := range subnetsStringList {
		if subnetString != newSubnetsStringList[i] {
			return false
		}
	}
	return true
}

// parseSubnetsString parses comma-seperated subnet string and returns the list of subnets
func parseSubnetsString(clusterSubnetString string) ([]*net.IPNet, error) {
	var subnetList []*net.IPNet

	if strings.TrimSpace(clusterSubnetString) == "" {
		return subnetList, nil
	}

	subnetStringList := strings.Split(clusterSubnetString, ",")
	for _, subnetString := range subnetStringList {
		subnetString = strings.TrimSpace(subnetString)
		_, subnet, err := net.ParseCIDR(subnetString)
		if err != nil {
			return nil, err
		}

		subnetList = append(subnetList, subnet)
	}
	return subnetList, nil
}

// Layer3NetConfInfo is structure which holds specific secondary layer3 network information
type Layer3NetConfInfo struct {
	subnets        string
	mtu            int
	ClusterSubnets []config.CIDRNetworkEntry
}

// CompareNetConf compares the layer3NetConfInfo with the given newNetConfInfo and returns true
// if they share the same netconf information
func (layer3NetConfInfo *Layer3NetConfInfo) CompareNetConf(newNetConfInfo NetConfInfo) bool {
	var errs []error
	var err error

	newLayer3NetConfInfo, ok := newNetConfInfo.(*Layer3NetConfInfo)
	if !ok {
		klog.V(5).Infof("New netconf topology type is different, expect %s",
			layer3NetConfInfo.TopologyType())
		return false
	}

	if !isSubnetsStringEqual(layer3NetConfInfo.subnets, newLayer3NetConfInfo.subnets) {
		err = fmt.Errorf("new %s netconf subnets %v has changed, expect %v",
			types.Layer3Topology, newLayer3NetConfInfo.subnets, layer3NetConfInfo.subnets)
		errs = append(errs, err)
	}

	if layer3NetConfInfo.mtu != newLayer3NetConfInfo.mtu {
		err = fmt.Errorf("new %s netconf mtu %v has changed, expect %v",
			types.Layer3Topology, newLayer3NetConfInfo.mtu, layer3NetConfInfo.mtu)
		errs = append(errs, err)
	}
	if len(errs) != 0 {
		err = kerrors.NewAggregate(errs)
		klog.V(5).Infof(err.Error())
		return false
	}
	return true
}

func newLayer3NetConfInfo(netconf *ovncnitypes.NetConf) (*Layer3NetConfInfo, error) {
	clusterSubnets, err := config.ParseClusterSubnetEntries(netconf.Subnets)
	if err != nil {
		return nil, fmt.Errorf("cluster subnet %s is invalid: %v", netconf.Subnets, err)
	}

	return &Layer3NetConfInfo{
		subnets:        netconf.Subnets,
		mtu:            netconf.MTU,
		ClusterSubnets: clusterSubnets,
	}, nil
}

// TopologyType returns the layer3NetConfInfo's topology type which is layer3 topology
func (layer3NetConfInfo *Layer3NetConfInfo) TopologyType() string {
	return types.Layer3Topology
}

// MTU returns the layer3NetConfInfo's MTU value
func (layer3NetConfInfo *Layer3NetConfInfo) MTU() int {
	return layer3NetConfInfo.mtu
}

func (layer3NetConfInfo *Layer3NetConfInfo) Subnets() []string {
	return strings.Split(layer3NetConfInfo.subnets, ",")
}

// Layer2NetConfInfo is structure which holds specific secondary layer2 network information
type Layer2NetConfInfo struct {
	subnets        string
	mtu            int
	excludeSubnets string

	ClusterSubnets []*net.IPNet
	ExcludeSubnets []*net.IPNet
}

func (layer2NetConfInfo *Layer2NetConfInfo) CompareNetConf(newNetConfInfo NetConfInfo) bool {
	var errs []error
	var err error
	newLayer2NetConfInfo, ok := newNetConfInfo.(*Layer2NetConfInfo)
	if !ok {
		klog.V(5).Infof("New netconf topology type is different, expect %s",
			layer2NetConfInfo.TopologyType())
		return false
	}
	if !isSubnetsStringEqual(layer2NetConfInfo.subnets, newLayer2NetConfInfo.subnets) {
		err = fmt.Errorf("new %s netconf subnets %v has changed, expect %v",
			types.Layer2Topology, newLayer2NetConfInfo.subnets, layer2NetConfInfo.subnets)
		errs = append(errs, err)
	}
	if layer2NetConfInfo.mtu != newLayer2NetConfInfo.mtu {
		err = fmt.Errorf("new %s netconf mtu %v has changed, expect %v",
			types.Layer2Topology, newLayer2NetConfInfo.mtu, layer2NetConfInfo.mtu)
		errs = append(errs, err)
	}
	if !isSubnetsStringEqual(layer2NetConfInfo.excludeSubnets, newLayer2NetConfInfo.excludeSubnets) {
		err = fmt.Errorf("new %s netconf excludeSubnets %v has changed, expect %v",
			types.Layer2Topology, newLayer2NetConfInfo.excludeSubnets, layer2NetConfInfo.excludeSubnets)
		errs = append(errs, err)
	}
	if len(errs) != 0 {
		err = kerrors.NewAggregate(errs)
		klog.V(5).Infof(err.Error())
		return false
	}
	return true
}

func newLayer2NetConfInfo(netconf *ovncnitypes.NetConf) (*Layer2NetConfInfo, error) {
	clusterSubnets, excludeSubnets, err := verifyExcludeIPs(netconf.Subnets, netconf.ExcludeSubnets)
	if err != nil {
		return nil, fmt.Errorf("invalid %s netconf %s: %v", netconf.Topology, netconf.Name, err)
	}

	return &Layer2NetConfInfo{
		subnets:        netconf.Subnets,
		mtu:            netconf.MTU,
		excludeSubnets: netconf.ExcludeSubnets,
		ClusterSubnets: clusterSubnets,
		ExcludeSubnets: excludeSubnets,
	}, nil
}

func verifyExcludeIPs(subnetsString string, excludeSubnetsString string) ([]*net.IPNet, []*net.IPNet, error) {
	clusterSubnets, err := parseSubnetsString(subnetsString)
	if err != nil {
		return nil, nil, fmt.Errorf("subnets %s is invalid: %v", subnetsString, err)
	}

	excludeSubnets, err := parseSubnetsString(excludeSubnetsString)
	if err != nil {
		return nil, nil, fmt.Errorf("excludeSubnets %s is invalid: %v", excludeSubnetsString, err)
	}

	for _, excludeSubnet := range excludeSubnets {
		found := false
		for _, subnet := range clusterSubnets {
			if ContainsCIDR(subnet, excludeSubnet) {
				found = true
				break
			}
		}
		if !found {
			return nil, nil, fmt.Errorf("the provided network subnets %v does not contain exluded subnets %v",
				clusterSubnets, excludeSubnet)
		}
	}

	return clusterSubnets, excludeSubnets, nil
}

func (layer2NetConfInfo *Layer2NetConfInfo) TopologyType() string {
	return types.Layer2Topology
}

func (layer2NetConfInfo *Layer2NetConfInfo) MTU() int {
	return layer2NetConfInfo.mtu
}

func (layer2NetConfInfo *Layer2NetConfInfo) Subnets() []string {
	subnets := strings.Split(layer2NetConfInfo.subnets, ",")
	if len(subnets) == 1 && strings.TrimSpace(subnets[0]) == "" {
		return nil
	}
	return subnets
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

func newNetConfInfo(netconf *ovncnitypes.NetConf) (NetConfInfo, error) {
	if netconf.Name == types.DefaultNetworkName {
		return &DefaultNetConfInfo{}, nil
	}
	switch netconf.Topology {
	case types.Layer3Topology:
		return newLayer3NetConfInfo(netconf)
	case types.Layer2Topology:
		return newLayer2NetConfInfo(netconf)
	default:
		// other topology NAD can be supported later
		return nil, fmt.Errorf("topology %s not supported", netconf.Topology)
	}
}

// ParseNADInfo parses config in NAD spec and return a NetAttachDefInfo object for secondary networks
func ParseNADInfo(netattachdef *nettypes.NetworkAttachmentDefinition) (NetInfo, NetConfInfo, error) {
	netconf, err := ParseNetConf(netattachdef)
	if err != nil {
		return nil, nil, err
	}

	netconfInfo, err := newNetConfInfo(netconf)
	if err != nil {
		return nil, nil, err
	}
	return NewNetInfo(netconf), netconfInfo, nil
}

// ParseNetConf returns NetInfo for the given netconf
func NewNetInfo(netconf *ovncnitypes.NetConf) NetInfo {
	var nInfo NetInfo
	if netconf.Name == types.DefaultNetworkName {
		nInfo = &DefaultNetInfo{}
	} else {
		nInfo = &SecondaryNetInfo{
			netName:  netconf.Name,
			nadNames: &sync.Map{},
		}
	}
	return nInfo
}

// ParseNetConf parses config in NAD spec for secondary networks
func ParseNetConf(netattachdef *nettypes.NetworkAttachmentDefinition) (*ovncnitypes.NetConf, error) {
	netconf, err := config.ParseNetConf([]byte(netattachdef.Spec.Config))
	if err != nil {
		return nil, fmt.Errorf("error parsing Network Attachment Definition %s/%s: %v", netattachdef.Namespace, netattachdef.Name, err)
	}
	// skip non-OVN NAD
	if netconf.Type != "ovn-k8s-cni-overlay" {
		return nil, ErrorAttachDefNotOvnManaged
	}

	if netconf.Name != types.DefaultNetworkName {
		nadName := GetNADName(netattachdef.Namespace, netattachdef.Name)
		if netconf.NADName != nadName {
			return nil, fmt.Errorf("net-attach-def name (%s) is inconsistent with config (%s)", nadName, netconf.NADName)
		}
	}

	return netconf, nil
}

// GetPodNADToNetworkMapping sees if the given pod needs to plumb over this given network specified by netconf,
// and return the matching NetworkSelectionElement if any exists.
//
// Return value:
//    bool: if this Pod is on this Network; true or false
//    map[string]*nettypes.NetworkSelectionElement: all NetworkSelectionElement that pod is requested
//        for the specified network, key is NADName. Note multiple NADs of the same network are allowed
//        on one pod, as long as they are of different NADName.
//    error:  error in case of failure
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

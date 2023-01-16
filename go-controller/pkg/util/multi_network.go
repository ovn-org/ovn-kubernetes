package util

import (
	"errors"
	"fmt"
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

// Layer3NetConfInfo is structure which holds specific secondary layer3 network information
type Layer3NetConfInfo struct {
	subnets        string
	mtu            int
	ClusterSubnets []config.CIDRNetworkEntry
}

func compareSubnetsString(subnetsString, newSubnetsString string) bool {
	subnetsStringList := strings.Split(subnetsString, ",")
	newSubnetsStringList := strings.Split(newSubnetsString, ",")
	if len(subnetsStringList) != len(newSubnetsStringList) {
		return false
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

	if compareSubnetsString(layer3NetConfInfo.subnets, newLayer3NetConfInfo.subnets) {
		err = fmt.Errorf("new %s netconf subnets %v has changed, expect %v",
			types.Layer3Topology, newLayer3NetConfInfo.subnets, layer3NetConfInfo.subnets)
		errs = append(errs, err)
	}

	if layer3NetConfInfo.mtu != newLayer3NetConfInfo.mtu {
		err = fmt.Errorf("new %s netconf MTU %v has changed, expect %v",
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

	if netconf.Topology == types.Layer3Topology {
		return newLayer3NetConfInfo(netconf)
	}
	// other topology NAD can be supported later
	return nil, fmt.Errorf("topology %s not supported", netconf.Topology)
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

// PodWantsMultiNetwork sees if the given pod needs to plumb over this given network specified by netconf,
// and return the matching NetworkSelectionElement if any exists.
//
// Return value:
//    bool: if this Pod is on this Network; true or false
//    *nettypes.NetworkSelectionElement: all NetworkSelectionElement that pod is requested for the specified network
//    error:  error in case of failure
func PodWantsMultiNetwork(pod *kapi.Pod, nInfo NetInfo) (bool, *nettypes.NetworkSelectionElement, error) {
	var validNADName string

	podDesc := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if !nInfo.IsSecondary() {
		network, err := GetK8sPodDefaultNetwork(pod)
		if err != nil {
			// multus won't add this Pod if this fails, should never happen
			return false, nil, fmt.Errorf("error getting default-network's network-attachment for pod %s: %v", podDesc, err)
		}
		return true, network, nil
	}

	// For non-default network controller, try to see if its name exists in the Pod's k8s.v1.cni.cncf.io/networks, if no,
	// return false;
	allNetworks, err := GetK8sPodAllNetworks(pod)
	if err != nil {
		return false, nil, err
	}

	networkSelections := map[string]*nettypes.NetworkSelectionElement{}
	for _, network := range allNetworks {
		nadName := GetNADName(network.Namespace, network.Name)
		if nInfo.HasNAD(nadName) {
			if _, ok := networkSelections[nadName]; ok {
				return false, nil, fmt.Errorf("unexpected error: more than one of the same NAD %s specified for pod %s",
					nadName, podDesc)
			}
			validNADName = nadName
			networkSelections[nadName] = network
		}
	}
	if len(networkSelections) > 1 {
		return false, nil, fmt.Errorf("unexpected error: more than one NAD of the network %s specified for pod %s",
			nInfo.GetNetworkName(), podDesc)
	} else if len(networkSelections) == 0 {
		return false, nil, nil
	}
	return true, networkSelections[validNADName], nil
}

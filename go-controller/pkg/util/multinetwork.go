package util

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

var ErrorAttachDefNotOvnManaged = errors.New("net-attach-def not managed by OVN")

// NetInfo is interface which holds network name information
// for default network, this is set to nil
type NetInfo interface {
	GetNetworkName() string
	IsSecondary() bool
	GetPrefix() string
	AddNad(nadName string)
	DeleteNad(nadName string)
	IsNadExist(nadName string) bool
}

// NetNameInfo holds the network name information for secondary network if non-nil
type NetNameInfo struct {
	// network name
	netName string
	// all net-attach-def nad names for this network, used to determine if a pod needs
	// to be plumbed for this network
	nadNames *sync.Map
}

// GetNetworkName returns the network name
func (nInfo *NetNameInfo) GetNetworkName() string {
	if nInfo == nil {
		// default network
		return types.DefaultNetworkName
	}
	return nInfo.netName
}

// IsSecondary returns if this network is secondary
func (nInfo *NetNameInfo) IsSecondary() bool {
	return nInfo != nil
}

// GetPrefix returns if the logical entities prefix for this network
func (nInfo *NetNameInfo) GetPrefix() string {
	if nInfo == nil {
		return ""
	}
	return GetSecondaryNetworkPrefix(nInfo.netName)
}

// AddNad adds the specified nad
func (nInfo *NetNameInfo) AddNad(nadName string) {
	if nInfo != nil {
		nInfo.nadNames.Store(nadName, true)
	}
}

// DeleteNad deletes the specified nad and return true if no nads left
func (nInfo *NetNameInfo) DeleteNad(nadName string) {
	if nInfo != nil {
		nInfo.nadNames.Delete(nadName)
	}
}

// IsNadExist returns true if the given nad exists, used
// to check if the network needs to be plumbed over
func (nInfo *NetNameInfo) IsNadExist(nadName string) bool {
	if nInfo == nil {
		// default network always needs to be plumbed over Pod
		return true
	}
	_, ok := nInfo.nadNames.Load(nadName)
	return ok
}

// NetConfInfo is structure which holds specific per-network configuration
type NetConfInfo interface {
	Verify() error
	CompareNetConf(NetConfInfo) bool
	GetTopologyType() string
	GetMtu() int
}

// Layer3NetConfInfo is structure which holds specific secondary layer3 network information
type Layer3NetConfInfo struct {
	NetCidr        string
	MTU            int
	ClusterSubnets []config.CIDRNetworkEntry
}

func (layer3NetConfInfo *Layer3NetConfInfo) CompareNetConf(newNetConfInfo NetConfInfo) bool {
	newLayer3NetConfInfo, ok := newNetConfInfo.(*Layer3NetConfInfo)
	if !ok {
		klog.V(5).Infof("New netconf topology type is different, expect %s",
			layer3NetConfInfo.GetTopologyType())
		return false
	}
	if layer3NetConfInfo.NetCidr != newLayer3NetConfInfo.NetCidr {
		klog.V(5).Infof("New netconf NetCidr %v has changed, expect %v",
			newLayer3NetConfInfo.NetCidr, layer3NetConfInfo.NetCidr)
		return false
	}
	if layer3NetConfInfo.MTU != newLayer3NetConfInfo.MTU {
		klog.V(5).Infof("New netconf MTU %v has changed, expect %v",
			newLayer3NetConfInfo.MTU, layer3NetConfInfo.MTU)
		return false
	}
	return true
}

func (layer3NetConfInfo *Layer3NetConfInfo) Verify() error {
	clusterSubnets, err := config.ParseClusterSubnetEntries(layer3NetConfInfo.NetCidr)
	if err != nil {
		return fmt.Errorf("cluster subnet %s is invalid: %v", layer3NetConfInfo.NetCidr, err)
	}
	layer3NetConfInfo.ClusterSubnets = clusterSubnets
	return nil
}

func (layer3NetConfInfo *Layer3NetConfInfo) GetTopologyType() string {
	return types.Layer3AttachDefTopoType
}

func (layer3NetConfInfo *Layer3NetConfInfo) GetMtu() int {
	return layer3NetConfInfo.MTU
}

// GetNadName returns key of NetAttachDefInfo.NetAttachDefs map, also used as Pod annotation key
func GetNadName(namespace, name string) string {
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
	var netconfInfo NetConfInfo

	if !netconf.IsSecondary {
		return nil, nil
	}

	if netconf.Topology == types.Layer3AttachDefTopoType {
		netconfInfo = &Layer3NetConfInfo{
			NetCidr: netconf.NetCidr,
			MTU:     netconf.MTU,
		}
	} else {
		// other topology nad can be supported later
		return nil, fmt.Errorf("topology %s not supported", netconf.Topology)
	}
	err := netconfInfo.Verify()
	if err != nil {
		return nil, fmt.Errorf("%s netconf verification failed: %v", netconf.Topology, err)
	}
	return netconfInfo, nil
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

func NewNetInfo(netconf *ovncnitypes.NetConf) NetInfo {
	// default network netInfo is nil
	nInfo := (*NetNameInfo)(nil)
	if netconf.IsSecondary {
		nInfo = &NetNameInfo{
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
	// skip non-OVN nad or primary network OVN NAD
	if netconf.Type != "ovn-k8s-cni-overlay" || !netconf.IsSecondary {
		return nil, ErrorAttachDefNotOvnManaged
	}

	nadName := GetNadName(netattachdef.Namespace, netattachdef.Name)
	if netconf.NadName != nadName {
		return nil, fmt.Errorf("net-attach-def name (%s) is inconsistent with config (%s)", nadName, netconf.NadName)
	}

	return netconf, nil
}

// IsNetworkOnPod sees if the given pod needs to plumb over this given network specified by netconf,
// and return the matching NetworkSelectionElement if any exists.
//
// Return value:
//    bool: if this Pod is on this Network; true or false
//    *networkattachmentdefinitionapi.NetworkSelectionElement: all NetworkSelectionElement that pod is requested for the specified network
//    error:  error in case of failure
func IsNetworkOnPod(pod *kapi.Pod, nInfo NetInfo) (bool, *nettypes.NetworkSelectionElement, error) {
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

	nses := make([]*nettypes.NetworkSelectionElement, 0, len(allNetworks))
	for _, network := range allNetworks {
		nadName := GetNadName(network.Namespace, network.Name)
		if nInfo.IsNadExist(nadName) {
			nses = append(nses, network)
		}
	}
	if len(nses) > 1 {
		return false, nil, fmt.Errorf("unexpected error: more than one nad of the network %s specified for pod %s",
			nInfo.GetNetworkName(), podDesc)
	} else if len(nses) == 0 {
		return false, nil, nil
	}
	return true, nses[0], nil
}

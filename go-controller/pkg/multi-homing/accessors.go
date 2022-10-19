package multi_homing

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
)

// GetAnnotationKeyFromNadName for default network, nadName is always "default".
// For everything else, it is the same as the provided nadKeyName.
func GetAnnotationKeyFromNadName(nadName string, isDefault bool) string {
	if isDefault {
		return types.DefaultNetworkName
	}
	return nadName
}

// GetNadName Note that for port_group and address_set, it does not allow the '-' character
func GetNadName(namespace, name string, isDefault bool) string {
	if isDefault {
		return types.DefaultNetworkName
	}
	return GetNadKeyName(namespace, name)
}

// GetNadKeyName key of NetAttachDefInfo.NetAttachDefs map
func GetNadKeyName(namespace, name string) string {
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

// NewNetAttachDefInfo generates a new NetAttachDefInfo struct
func NewNetAttachDefInfo(netconf *ovncnitypes.NetConf) *NetAttachDefInfo {
	netName := netconf.Name
	prefix := ""
	if netconf.IsSecondary {
		prefix = GetSecondaryNetworkPrefix(netName)
	}

	nadInfo := NetAttachDefInfo{
		NetNameInfo: NetNameInfo{NetName: netName, Prefix: prefix, IsSecondary: netconf.IsSecondary},
		NetCidr:     netconf.NetCidr,
		MTU:         netconf.MTU,
	}
	return &nadInfo
}

// ParseNADInfo config in NAD spec and return a NetAttachDefInfo object
func ParseNADInfo(netattachdef *nettypes.NetworkAttachmentDefinition) (*NetAttachDefInfo, error) {
	netconf, err := ParseNetConf(netattachdef)
	if err != nil {
		return nil, err
	}

	nadKey := GetNadKeyName(netattachdef.Namespace, netattachdef.Name)
	if netconf.NadName != nadKey {
		return nil, fmt.Errorf("net-attach-def name (%s) is inconsistent with config (%s)", nadKey, netconf.NadName)
	}

	nadInfo := NewNetAttachDefInfo(netconf)
	return nadInfo, nil
}

// ParseNetConf parses an *ovncnitypes.NetConf from a net attach def
func ParseNetConf(netattachdef *nettypes.NetworkAttachmentDefinition) (*ovncnitypes.NetConf, error) {
	netconf := &ovncnitypes.NetConf{MTU: config.Default.MTU}
	// looking for network attachment definition that use OVN K8S CNI only
	err := json.Unmarshal([]byte(netattachdef.Spec.Config), &netconf)
	if err != nil {
		return nil, fmt.Errorf("error parsing Network Attachment Definition %s/%s: %v", netattachdef.Namespace, netattachdef.Name, err)
	}
	if netconf.Type != "ovn-k8s-cni-overlay" {
		return nil, ErrorAttachDefNotOvnManaged
	}

	if netconf.Name == "" {
		netconf.Name = netattachdef.Name
	}

	if !netconf.IsSecondary {
		netconf.Name = types.DefaultNetworkName
	} else if netconf.Name == types.DefaultNetworkName {
		return nil, fmt.Errorf("netconf name cannot be %s for secondary network net-attach-def", types.DefaultNetworkName)
	}

	if !isValidTopology(netconf.Topology) {
		return nil, fmt.Errorf("invalid topology: %s", netconf.Topology)
	}

	return netconf, nil
}

// GetNADNamesFromMap extracts the names of all the networks attachment definitions in a map
func GetNADNamesFromMap(netAttachDefs *sync.Map) []string {
	nadNames := []string{}
	(*netAttachDefs).Range(func(key, value interface{}) bool {
		nadNames = append(nadNames, key.(string))
		return true
	})
	return nadNames
}

func isValidTopology(topology ovncnitypes.Topology) bool {
	if ovncnitypes.Routed == topology || ovncnitypes.Switched == topology || ovncnitypes.Underlay == topology {
		return true
	}
	return false
}

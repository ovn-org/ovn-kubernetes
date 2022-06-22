package util

import (
	"encoding/json"
	"errors"
	"fmt"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	types "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"strings"
	"sync"
)

var ErrorAttachDefNotOvnManaged = errors.New("net-attach-def not managed by OVN")

// NetNameInfo is structure which holds network name information
type NetNameInfo struct {
	// netconf's name, default for default network
	NetName string
	// Prefix of OVN logical entities for this network
	Prefix      string
	IsSecondary bool
}

// NetAttachDefInfo is structure which holds specific per-network information
type NetAttachDefInfo struct {
	NetNameInfo
	NetCidr string
	MTU     int
	// net-attach-defs shared the same CNI Conf, key is <Namespace>/<Name> of net-attach-def.
	// Note that it means they share the same logical switch (subnet cidr/MTU etc), but they might
	// have different resource requirement (requires or not require VF, or different VF resource set)
	NetAttachDefs sync.Map
}

// for default network, nadName is always "default", otherwide, it is the same as nadKeyName
func GetAnnotationKeyFromNadName(nadName string, isDefault bool) string {
	if isDefault {
		return types.DefaultNetworkName
	}
	return nadName
}

// Note that for port_group and address_set, it does not allow the '-' character
func GetNadName(namespace, name string, isDefault bool) string {
	if isDefault {
		return types.DefaultNetworkName
	}
	return GetNadKeyName(namespace, name)
}

// key of NetAttachDefInfo.NetAttachDefs map
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

// Parse config in NAD spec and return a NetAttachDefInfo object
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

	// validation
	if !netconf.IsSecondary {
		netconf.Name = types.DefaultNetworkName
	} else if netconf.Name == types.DefaultNetworkName {
		return nil, fmt.Errorf("netconf name cannot be %s for secondary network net-attach-def", types.DefaultNetworkName)
	}

	return netconf, nil
}

func GetNADNamesFromMap(netAttachDefs *sync.Map) []string {
	nadNames := []string{}
	(*netAttachDefs).Range(func(key, value interface{}) bool {
		nadNames = append(nadNames, key.(string))
		return true
	})
	return nadNames
}

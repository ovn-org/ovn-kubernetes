package template

import (
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	OvnK8sCNIOverlay = "ovn-k8s-cni-overlay"

	FinalizerUserDefinedNetwork = "k8s.ovn.org/user-defined-network-protection"
	LabelUserDefinedNetwork     = "k8s.ovn.org/user-defined-network"

	cniVersion = "1.0.0"
)

type SpecGetter interface {
	GetTopology() userdefinednetworkv1.NetworkTopology
	GetLayer3() *userdefinednetworkv1.Layer3Config
	GetLayer2() *userdefinednetworkv1.Layer2Config
}

// This function has a copy in go-controller/observability-lib/sampledecoder/sample_decoder.go
// Please update together with this function.
func ParseNetworkName(networkName string) (udnNamespace, udnName string) {
	parts := strings.Split(networkName, ".")
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", ""
}

func RenderNetAttachDefManifest(obj client.Object, targetNamespace string) (*netv1.NetworkAttachmentDefinition, error) {
	if obj == nil {
		return nil, nil
	}

	if targetNamespace == "" {
		return nil, fmt.Errorf("namspace should not be empty")
	}

	var ownerRef metav1.OwnerReference
	var spec SpecGetter
	var networkName string
	switch o := obj.(type) {
	case *userdefinednetworkv1.UserDefinedNetwork:
		ownerRef = *metav1.NewControllerRef(obj, userdefinednetworkv1.SchemeGroupVersion.WithKind("UserDefinedNetwork"))
		spec = &o.Spec
		networkName = targetNamespace + "." + obj.GetName()
	case *userdefinednetworkv1.ClusterUserDefinedNetwork:
		ownerRef = *metav1.NewControllerRef(obj, userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork"))
		spec = &o.Spec.Network
		networkName = "cluster.udn." + obj.GetName()
	default:
		return nil, fmt.Errorf("unknown type %T", obj)
	}

	nadName := util.GetNADName(targetNamespace, obj.GetName())

	nadSpec, err := RenderNADSpec(networkName, nadName, spec)
	if err != nil {
		return nil, err
	}

	return &netv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:            obj.GetName(),
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Labels:          map[string]string{LabelUserDefinedNetwork: ""},
			Finalizers:      []string{FinalizerUserDefinedNetwork},
		},
		Spec: *nadSpec,
	}, nil
}

func RenderNADSpec(networkName, nadName string, spec SpecGetter) (*netv1.NetworkAttachmentDefinitionSpec, error) {
	if err := validateTopology(spec); err != nil {
		return nil, fmt.Errorf("invalid topology specified: %w", err)
	}

	cniNetConf, err := renderCNINetworkConfig(networkName, nadName, spec)
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

func validateTopology(spec SpecGetter) error {
	if spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLayer3 && spec.GetLayer3() == nil ||
		spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLayer2 && spec.GetLayer2() == nil {
		return fmt.Errorf("topology %[1]s is specified but %[1]s config is nil", spec.GetTopology())
	}
	return nil
}

func renderCNINetworkConfig(networkName, nadName string, spec SpecGetter) (map[string]interface{}, error) {
	netConfSpec := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			CNIVersion: cniVersion,
			Type:       OvnK8sCNIOverlay,
			Name:       networkName,
		},
		NADName:  nadName,
		Topology: strings.ToLower(string(spec.GetTopology())),
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
		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = int(cfg.MTU)
		netConfSpec.AllowPersistentIPs = cfg.IPAMLifecycle == userdefinednetworkv1.IPAMLifecyclePersistent
		netConfSpec.Subnets = cidrString(cfg.Subnets)
		netConfSpec.JoinSubnet = cidrString(renderJoinSubnets(cfg.Role, cfg.JoinSubnets))
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
		"cniVersion":       cniVersion,
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
		cniNetConf["joinSubnets"] = netConfSpec.JoinSubnet
	}
	if len(netConfSpec.Subnets) > 0 {
		cniNetConf["subnets"] = netConfSpec.Subnets
	}
	if netConfSpec.AllowPersistentIPs {
		cniNetConf["allowPersistentIPs"] = netConfSpec.AllowPersistentIPs
	}

	return cniNetConf, nil
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

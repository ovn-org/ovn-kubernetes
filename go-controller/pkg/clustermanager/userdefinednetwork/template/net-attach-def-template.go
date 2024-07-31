package template

import (
	"encoding/json"
	"fmt"
	"strings"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	OvnK8sCNIOverlay            = "ovn-k8s-cni-overlay"
	FinalizerUserDefinedNetwork = "k8s.ovn.org/user-defined-network-protection"

	cniVersion              = "1.0.0"
	ovnK8sCNIOverlay        = "ovn-k8s-cni-overlay"
	labelUserDefinedNetwork = "k8s.ovn.org/user-defined-network"
)

var udnKind = userdefinednetworkv1.SchemeGroupVersion.WithKind("UserDefinedNetwork")

// RenderNetAttachDefManifest return NetworkAttachmentDefinition according to the given UserDefinedNetwork spec
func RenderNetAttachDefManifest(udn *userdefinednetworkv1.UserDefinedNetwork) (*netv1.NetworkAttachmentDefinition, error) {
	if udn == nil {
		return nil, nil
	}

	cniNetConf, err := renderCNINetworkConfig(udn)
	if err != nil {
		return nil, fmt.Errorf("failed to render CNI network config: %w", err)
	}
	cniNetConfRaw, err := json.Marshal(cniNetConf)
	if err != nil {
		return nil, err
	}

	return &netv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:            udn.Name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(udn, udnKind)},
			Labels: map[string]string{
				labelUserDefinedNetwork: "",
			},
			Finalizers: []string{FinalizerUserDefinedNetwork},
		},
		Spec: netv1.NetworkAttachmentDefinitionSpec{
			Config: string(cniNetConfRaw),
		},
	}, nil
}

func renderCNINetworkConfig(udn *userdefinednetworkv1.UserDefinedNetwork) (map[string]interface{}, error) {
	networkName := udn.Namespace + "." + udn.Name
	nadName := util.GetNADName(udn.Namespace, udn.Name)
	ipamLifecyclePersistent := udn.Spec.IPAMLifecycle == userdefinednetworkv1.IPAMLifecyclePersistent

	netConfSpec := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			CNIVersion: cniVersion,
			Type:       ovnK8sCNIOverlay,
			Name:       networkName,
		},
		NADName:            nadName,
		Role:               strings.ToLower(string(udn.Spec.Role)),
		Topology:           strings.ToLower(string(udn.Spec.Topology)),
		MTU:                int(udn.Spec.MTU),
		AllowPersistentIPs: ipamLifecyclePersistent,
		JoinSubnet:         strings.Join(udn.Spec.JoinSubnets, ","),
		Subnets:            strings.Join(udn.Spec.Subnets, ","),
		ExcludeSubnets:     strings.Join(udn.Spec.ExcludeSubnets, ","),
	}

	if err := util.ValidateNetConf(nadName, netConfSpec); err != nil {
		return nil, err
	}
	netInfo, err := util.NewNetInfo(netConfSpec)
	if err != nil {
		return nil, err
	}

	cniNetConf := map[string]interface{}{
		"cniVersion":       cniVersion,
		"type":             ovnK8sCNIOverlay,
		"name":             networkName,
		"netAttachDefName": nadName,
		"topology":         strings.ToLower(string(udn.Spec.Topology)),
		"role":             strings.ToLower(string(udn.Spec.Role)),
	}
	if mtu := netInfo.MTU(); mtu > 0 {
		cniNetConf["mtu"] = mtu
	}
	if joinSubnets := netInfo.JoinSubnets(); len(joinSubnets) > 0 {
		cniNetConf["joinSubnets"] = cidrString(joinSubnets)
	}
	if subnets := netInfo.Subnets(); len(subnets) > 0 {
		cniNetConf["subnets"] = cidrString(subnets)
	}
	if excludeSubnets := netInfo.ExcludeSubnets(); len(excludeSubnets) > 0 {
		cniNetConf["excludeSubnets"] = cidrString(excludeSubnets)
	}
	if allowPersistentIPs := netInfo.AllowsPersistentIPs(); allowPersistentIPs {
		cniNetConf["allowPersistentIPs"] = allowPersistentIPs
	}

	return cniNetConf, nil
}

type cidrStringer interface {
	String() string
}

func cidrString[T cidrStringer](ciders []T) string {
	var ciderStrings []string
	for _, s := range ciders {
		ciderStrings = append(ciderStrings, s.String())
	}
	return strings.Join(ciderStrings, ",")
}

package template

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
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
		},
		Spec: netv1.NetworkAttachmentDefinitionSpec{
			Config: string(cniNetConfRaw),
		},
	}, nil
}

func renderCNINetworkConfig(udn *userdefinednetworkv1.UserDefinedNetwork) (map[string]interface{}, error) {
	networkName := udn.Namespace + "." + udn.Name
	nadNamespaceName := util.GetNADName(udn.Namespace, udn.Name)

	netConf := map[string]interface{}{
		"cniVersion":       cniVersion,
		"type":             ovnK8sCNIOverlay,
		"name":             networkName,
		"netAttachDefName": nadNamespaceName,
		"topology":         strings.ToLower(string(udn.Spec.Topology)),
		"role":             strings.ToLower(string(udn.Spec.Role)),
	}

	if udn.Spec.MTU > 0 {
		netConf["mtu"] = udn.Spec.MTU
	}

	primaryNetwork := udn.Spec.Role == userdefinednetworkv1.NetworkRolePrimary

	if primaryNetwork && udn.Spec.Topology == userdefinednetworkv1.NetworkTopologyLocalnet {
		return nil, fmt.Errorf("locannet topology is not supported for primary network")
	}

	if primaryNetwork {
		if len(udn.Spec.JoinSubnets) == 0 {
			netConf["joinSubnets"] = types.UserDefinedPrimaryNetworkJoinSubnetV4 + "," + types.UserDefinedPrimaryNetworkJoinSubnetV6
		} else {
			if err := validateSubnets(udn.Spec.JoinSubnets); err != nil {
				return nil, fmt.Errorf("invalid join subnets: %w", err)
			}
			netConf["joinSubnets"] = strings.Join(udn.Spec.JoinSubnets, ",")
		}
	}

	if udn.Spec.IPAMLifecycle == userdefinednetworkv1.IPAMLifecyclePersistent {
		if udn.Spec.Topology == userdefinednetworkv1.NetworkTopologyLayer2 ||
			udn.Spec.Topology == userdefinednetworkv1.NetworkTopologyLocalnet {
			netConf["allowPersistentIPs"] = true
		} else {
			return nil, fmt.Errorf("IPAMLifyecycle Persistent is supported for Layer2 and Localnet topology")
		}
	}

	if len(udn.Spec.Subnets) > 0 {
		if err := validateSubnets(udn.Spec.Subnets); err != nil {
			return nil, fmt.Errorf("invalid subnets: %w", err)
		}
		netConf["subnets"] = strings.Join(udn.Spec.Subnets, ",")
	}

	if len(udn.Spec.ExcludeSubnets) > 0 {
		if err := validateSubnets(udn.Spec.ExcludeSubnets); err != nil {
			return nil, fmt.Errorf("invalid exclude subnets: %w", err)
		}
		netConf["excludeSubnets"] = strings.Join(udn.Spec.ExcludeSubnets, ",")
	}

	return netConf, nil
}

func validateSubnets(subnets []string) error {
	for _, subent := range subnets {
		if _, _, err := net.ParseCIDR(subent); err != nil {
			return err
		}
	}
	return nil
}

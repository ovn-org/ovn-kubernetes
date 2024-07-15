package template

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kutilnet "k8s.io/utils/net"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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
			if err := validateJoinSubnet(udn.Spec.JoinSubnets); err != nil {
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

func validateJoinSubnet(subnets []string) error {
	if len(subnets) == 0 {
		return nil
	}

	if len(subnets) > 2 {
		return fmt.Errorf("unexpected number of join-subnets: %v", subnets)
	}

	_, defaultJoinSubnetV4, err := net.ParseCIDR(config.Gateway.V4JoinSubnet)
	if err != nil {
		return err
	}
	_, defaultJoinSubnetV6, err := net.ParseCIDR(config.Gateway.V6JoinSubnet)
	if err != nil {
		return err
	}
	cs := config.NewConfigSubnets()
	cs.Append("cluster-default-join-subnet", defaultJoinSubnetV4)
	cs.Append("cluster-default-join-subnet", defaultJoinSubnetV6)

	switch len(subnets) {
	case 1:
		_, cidr, err := net.ParseCIDR(subnets[0])
		if err != nil {
			return fmt.Errorf("invalid join-subnets %v: %w", subnets[0], err)
		}
		cs.Append("user-defined-network-join-subnet", cidr)
		if err := cs.CheckForOverlaps(); err != nil {
			return fmt.Errorf("invalid join-subnets, overlaps with cluster default network join-subnet: %w", err)
		}
	case 2:
		_, cidr1, err := net.ParseCIDR(subnets[0])
		if err != nil {
			return fmt.Errorf("invalid join-subnets %v: %w", subnets[0], err)
		}
		_, cidr2, err := net.ParseCIDR(subnets[1])
		if err != nil {
			return fmt.Errorf("invalid join-subnets %v: %w", subnets[1], err)
		}
		dualStack, err := kutilnet.IsDualStackCIDRs([]*net.IPNet{cidr1, cidr2})
		if err != nil {
			return fmt.Errorf("failed to check dual-stack CIDRs %v: %w", subnets, err)
		}
		if !dualStack {
			return fmt.Errorf("invalid dual-stack join-subnets, expects one IPv4 CIDR & IPv6 CIDR")
		}
		cs.Append("user-defined-network-join-subnet", cidr1)
		cs.Append("user-defined-network-join-subnet", cidr2)
		if err := cs.CheckForOverlaps(); err != nil {
			return fmt.Errorf("invalid join subnets, overlaps with cluster default network join-subnets: %w", err)
		}
	}

	return nil
}

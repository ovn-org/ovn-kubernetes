package e2e

import (
	"context"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

const (
	bridgeName = "ovsbr1"
	add        = "add-br"
	del        = "del-br"
)

func setupUnderlay(ovsPods []v1.Pod, portName string, nadConfig networkAttachmentConfig) error {
	for _, ovsPod := range ovsPods {
		if err := addOVSBridge(ovsPod.Name, bridgeName); err != nil {
			return err
		}

		if nadConfig.vlanID > 0 {
			if err := ovsEnableVLANAccessPort(ovsPod.Name, bridgeName, portName, nadConfig.vlanID); err != nil {
				return err
			}
		} else {
			if err := ovsAttachPortToBridge(ovsPod.Name, bridgeName, portName); err != nil {
				return err
			}
		}

		if err := configureBridgeMappings(
			ovsPod.Name,
			defaultNetworkBridgeMapping(),
			bridgeMapping(nadConfig.networkName, bridgeName),
		); err != nil {
			return err
		}
	}
	return nil
}

func teardownUnderlay(ovsPods []v1.Pod) error {
	for _, ovsPod := range ovsPods {
		if err := removeOVSBridge(ovsPod.Name, bridgeName); err != nil {
			return err
		}
	}
	return nil
}

func ovsPods(clientSet clientset.Interface) []v1.Pod {
	const (
		ovnKubernetesNamespace = "ovn-kubernetes"
		ovsNodeLabel           = "app=ovs-node"
	)
	pods, err := clientSet.CoreV1().Pods(ovnKubernetesNamespace).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: ovsNodeLabel},
	)
	if err != nil {
		return nil
	}
	return pods.Items
}

func addOVSBridge(ovnNodeName string, bridgeName string) error {
	_, err := runCommand(ovsBridgeCommand(ovnNodeName, add, bridgeName)...)
	if err != nil {
		return fmt.Errorf("failed to ADD OVS bridge %s: %v", bridgeName, err)
	}
	return nil
}

func removeOVSBridge(ovnNodeName string, bridgeName string) error {
	_, err := runCommand(ovsBridgeCommand(ovnNodeName, del, bridgeName)...)
	if err != nil {
		return fmt.Errorf("failed to DELETE OVS bridge %s: %v", bridgeName, err)
	}
	return nil
}

func ovsBridgeCommand(ovnNodeName string, addOrDeleteCmd string, bridgeName string) []string {
	return []string{
		"kubectl", "-n", "ovn-kubernetes", "exec", ovnNodeName, "--",
		"ovs-vsctl", addOrDeleteCmd, bridgeName,
	}
}

func ovsAttachPortToBridge(ovsNodeName string, bridgeName string, portName string) error {
	cmd := []string{
		"kubectl", "-n", "ovn-kubernetes", "exec", ovsNodeName, "--",
		"ovs-vsctl", "add-port", bridgeName, portName,
	}

	if _, err := runCommand(cmd...); err != nil {
		return fmt.Errorf("failed to add port %s to OVS bridge %s: %v", portName, bridgeName, err)
	}

	return nil
}

func ovsEnableVLANAccessPort(ovsNodeName string, bridgeName string, portName string, vlanID int) error {
	cmd := []string{
		"kubectl", "-n", "ovn-kubernetes", "exec", ovsNodeName, "--",
		"ovs-vsctl", "add-port", bridgeName, portName, fmt.Sprintf("tag=%d", vlanID), "vlan_mode=access",
	}

	if _, err := runCommand(cmd...); err != nil {
		return fmt.Errorf("failed to add port %s to OVS bridge %s: %v", portName, bridgeName, err)
	}

	return nil
}

type BridgeMapping struct {
	physnet   string
	ovsBridge string
}

func (bm BridgeMapping) String() string {
	return fmt.Sprintf("%s:%s", bm.physnet, bm.ovsBridge)
}

type BridgeMappings []BridgeMapping

func (bms BridgeMappings) String() string {
	return strings.Join(Map(bms, func(bm BridgeMapping) string { return bm.String() }), ",")
}

func Map[T, V any](items []T, fn func(T) V) []V {
	result := make([]V, len(items))
	for i, t := range items {
		result[i] = fn(t)
	}
	return result
}

func configureBridgeMappings(ovnNodeName string, mappings ...BridgeMapping) error {
	mappingsString := fmt.Sprintf("external_ids:ovn-bridge-mappings=%s", BridgeMappings(mappings).String())
	cmd := []string{"kubectl", "-n", "ovn-kubernetes", "exec", ovnNodeName,
		"--", "ovs-vsctl", "set", "open", ".", mappingsString,
	}
	_, err := runCommand(cmd...)
	return err
}

func defaultNetworkBridgeMapping() BridgeMapping {
	return BridgeMapping{
		physnet:   "physnet",
		ovsBridge: "breth0",
	}
}

func bridgeMapping(physnet, ovsBridge string) BridgeMapping {
	return BridgeMapping{
		physnet:   physnet,
		ovsBridge: ovsBridge,
	}
}

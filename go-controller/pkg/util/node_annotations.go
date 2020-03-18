package util

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

// This handles the annotations used by the node to pass information about its local
// network configuration to the master:
//
//   annotations:
//     k8s.ovn.org/l3-gateway-config: |
//       {
//         "default": {
//           "mode": "local",
//           "interface-id": "br-local_ip-10-0-129-64.us-east-2.compute.internal",
//           "mac-address": "f2:20:a0:3c:26:4c",
//           "ip-address": "169.254.33.2/24",
//           "next-hop": "169.254.33.1",
//           "node-port-enable": "true",
//           "vlan-id": "0"
//         }
//       }
//     k8s.ovn.org/node-chassis-id: b1f96182-2bdd-42b6-88f9-9a1fc1c85ece
//     k8s.ovn.org/node-mgmt-port-mac-address: fa:f1:27:f5:54:69

const (
	// OvnNodeL3GatewayConfig is the constant string representing the l3 gateway annotation key
	OvnNodeL3GatewayConfig = "k8s.ovn.org/l3-gateway-config"

	// OvnDefaultNetworkGateway captures L3 gateway config for default OVN network interface
	OvnDefaultNetworkGateway = "default"

	// OvnNodeGatewayMode is the mode of the gateway in the l3 gateway annotation
	OvnNodeGatewayMode = "mode"
	// OvnNodeGatewayVlanID is the vlanid used by the gateway in the l3 gateway annotation
	OvnNodeGatewayVlanID = "vlan-id"
	// OvnNodeGatewayIfaceID is the interfaceID of the gateway in the l3 gateway annotation
	OvnNodeGatewayIfaceID = "interface-id"
	// OvnNodeGatewayMacAddress is the MacAddress of the Gateway interface in the l3 gateway annotation
	OvnNodeGatewayMacAddress = "mac-address"
	// OvnNodeGatewayIP is the IP address of the Gateway in the l3 gateway annotation
	OvnNodeGatewayIP = "ip-address"
	// OvnNodeGatewayNextHop is the Next Hop in the l3 gateway annotation
	OvnNodeGatewayNextHop = "next-hop"
	// OvnNodePortEnable in the l3 gateway annotation captures whether load balancer needs to
	// be created or not
	OvnNodePortEnable = "node-port-enable"

	// OvnNodeManagementPortMacAddress is the constant string representing the annotation key
	OvnNodeManagementPortMacAddress = "k8s.ovn.org/node-mgmt-port-mac-address"

	// OvnNodeChassisID is the systemID of the node needed for creating L3 gateway
	OvnNodeChassisID = "k8s.ovn.org/node-chassis-id"
)

func CreateDisabledL3GatewayConfig() map[string]map[string]string {
	return map[string]map[string]string{
		OvnDefaultNetworkGateway: {
			OvnNodeGatewayMode: string(config.GatewayModeDisabled),
		},
	}
}

func CreateL3GatewayConfig(ifaceID, macAddress, gatewayAddress, nextHop string) map[string]map[string]string {
	return map[string]map[string]string{
		OvnDefaultNetworkGateway: {
			OvnNodeGatewayMode:       string(config.Gateway.Mode),
			OvnNodeGatewayVlanID:     fmt.Sprintf("%d", config.Gateway.VLANID),
			OvnNodeGatewayIfaceID:    ifaceID,
			OvnNodeGatewayMacAddress: macAddress,
			OvnNodeGatewayIP:         gatewayAddress,
			OvnNodeGatewayNextHop:    nextHop,
			OvnNodePortEnable:        fmt.Sprintf("%t", config.Gateway.NodeportEnable),
		},
	}
}

// UnmarshalNodeL3GatewayAnnotation returns the unmarshalled l3-gateway-config annotation
func UnmarshalNodeL3GatewayAnnotation(node *kapi.Node) (map[string]string, error) {
	l3GatewayAnnotation, ok := node.Annotations[OvnNodeL3GatewayConfig]
	if !ok {
		return nil, fmt.Errorf("%s annotation not found for node %q", OvnNodeL3GatewayConfig, node.Name)
	}

	l3GatewayConfigMap := map[string]map[string]string{}
	if err := json.Unmarshal([]byte(l3GatewayAnnotation), &l3GatewayConfigMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal l3 gateway config annotation %s for node %q", l3GatewayAnnotation, node.Name)
	}

	l3GatewayConfig, ok := l3GatewayConfigMap[OvnDefaultNetworkGateway]
	if !ok {
		return nil, fmt.Errorf("%s annotation for %s network not found", OvnNodeL3GatewayConfig, OvnDefaultNetworkGateway)
	}
	return l3GatewayConfig, nil
}

func ParseGatewayIfaceID(l3GatewayConfig map[string]string) (string, error) {
	ifaceID, ok := l3GatewayConfig[OvnNodeGatewayIfaceID]
	if !ok || ifaceID == "" {
		return "", fmt.Errorf("%s annotation not found or invalid", OvnNodeGatewayIfaceID)
	}

	return ifaceID, nil
}

func ParseGatewayMacAddress(l3GatewayConfig map[string]string) (string, error) {
	gatewayMacAddress, ok := l3GatewayConfig[OvnNodeGatewayMacAddress]
	if !ok {
		return "", fmt.Errorf("%s annotation not found", OvnNodeGatewayMacAddress)
	}

	_, err := net.ParseMAC(gatewayMacAddress)
	if err != nil {
		return "", fmt.Errorf("Error %v in parsing node gateway macAddress %v", err, gatewayMacAddress)
	}

	return gatewayMacAddress, nil
}

func ParseGatewayLogicalNetwork(l3GatewayConfig map[string]string) (string, string, error) {
	ipAddress, ok := l3GatewayConfig[OvnNodeGatewayIP]
	if !ok {
		return "", "", fmt.Errorf("%s annotation not found", OvnNodeGatewayIP)
	}

	gwNextHop, ok := l3GatewayConfig[OvnNodeGatewayNextHop]
	if !ok {
		return "", "", fmt.Errorf("%s annotation not found", OvnNodeGatewayNextHop)
	}

	return ipAddress, gwNextHop, nil
}

func ParseGatewayVLANID(l3GatewayConfig map[string]string, ifaceID string) ([]string, error) {
	var lspArgs []string
	vID, ok := l3GatewayConfig[OvnNodeGatewayVlanID]
	if !ok {
		return nil, fmt.Errorf("%s annotation not found", OvnNodeGatewayVlanID)
	}

	vlanID, errVlan := strconv.Atoi(vID)
	if errVlan != nil {
		return nil, fmt.Errorf("%s annotation has an invalid format", OvnNodeGatewayVlanID)
	}
	if vlanID > 0 {
		lspArgs = []string{"--", "set", "logical_switch_port",
			ifaceID, fmt.Sprintf("tag_request=%d", vlanID)}
	}

	return lspArgs, nil
}

func ParseNodeChassisID(node *kapi.Node) (string, error) {
	systemID, ok := node.Annotations[OvnNodeChassisID]
	if !ok {
		return "", fmt.Errorf("%s annotation not found", OvnNodeChassisID)
	}
	return systemID, nil
}

func ParseNodeManagementPortMacAddr(node *kapi.Node) (string, error) {
	macAddress, ok := node.Annotations[OvnNodeManagementPortMacAddress]
	if !ok {
		klog.Errorf("macAddress annotation not found for node %q ", node.Name)
		return "", nil
	}

	_, err := net.ParseMAC(macAddress)
	if err != nil {
		return "", fmt.Errorf("Error %v in parsing node %v macAddress %v", err, node.Name, macAddress)
	}

	return macAddress, nil
}

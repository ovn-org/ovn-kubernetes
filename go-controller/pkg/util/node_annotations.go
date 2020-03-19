package util

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
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
	// ovnNodeL3GatewayConfig is the constant string representing the l3 gateway annotation key
	ovnNodeL3GatewayConfig = "k8s.ovn.org/l3-gateway-config"

	// OvnDefaultNetworkGateway captures L3 gateway config for default OVN network interface
	ovnDefaultNetworkGateway = "default"

	// ovnNodeGatewayMode is the mode of the gateway in the l3 gateway annotation
	ovnNodeGatewayMode = "mode"
	// ovnNodeGatewayVlanID is the vlanid used by the gateway in the l3 gateway annotation
	ovnNodeGatewayVlanID = "vlan-id"
	// ovnNodeGatewayIfaceID is the interfaceID of the gateway in the l3 gateway annotation
	ovnNodeGatewayIfaceID = "interface-id"
	// ovnNodeGatewayMacAddress is the MacAddress of the Gateway interface in the l3 gateway annotation
	ovnNodeGatewayMacAddress = "mac-address"
	// ovnNodeGatewayIP is the IP address of the Gateway in the l3 gateway annotation
	ovnNodeGatewayIP = "ip-address"
	// ovnNodeGatewayNextHop is the Next Hop in the l3 gateway annotation
	ovnNodeGatewayNextHop = "next-hop"
	// ovnNodePortEnable in the l3 gateway annotation captures whether load balancer needs to
	// be created or not
	ovnNodePortEnable = "node-port-enable"

	// ovnNodeManagementPortMacAddress is the constant string representing the annotation key
	ovnNodeManagementPortMacAddress = "k8s.ovn.org/node-mgmt-port-mac-address"

	// ovnNodeChassisID is the systemID of the node needed for creating L3 gateway
	ovnNodeChassisID = "k8s.ovn.org/node-chassis-id"
)

type L3GatewayConfig struct {
	Mode           config.GatewayMode
	ChassisID      string
	InterfaceID    string
	MACAddress     net.HardwareAddr
	IPAddress      *net.IPNet
	NextHop        net.IP
	NodePortEnable bool
	VLANID         *uint
}

func setAnnotations(nodeAnnotator kube.Annotator, l3GatewayConfig map[string]string) error {
	gatewayAnnotation := map[string]map[string]string{ovnDefaultNetworkGateway: l3GatewayConfig}
	if err := nodeAnnotator.Set(ovnNodeL3GatewayConfig, gatewayAnnotation); err != nil {
		return err
	}
	if l3GatewayConfig[ovnNodeGatewayMode] != string(config.GatewayModeDisabled) {
		systemID, err := GetNodeChassisID()
		if err != nil {
			return err
		}
		if err := nodeAnnotator.Set(ovnNodeChassisID, systemID); err != nil {
			return err
		}
	}
	return nil
}

// SetDisabledL3GatewayConfig uses nodeAnnotator set an l3-gateway-config annotation
// indicating the gateway is disabled.
func SetDisabledL3GatewayConfig(nodeAnnotator kube.Annotator) error {
	return setAnnotations(nodeAnnotator, map[string]string{
		ovnNodeGatewayMode: string(config.GatewayModeDisabled),
	})
}

// SetSharedL3GatewayConfig uses nodeAnnotator set an l3-gateway-config annotation
// for the "shared interface" gateway mode.
func SetSharedL3GatewayConfig(nodeAnnotator kube.Annotator,
	ifaceID, macAddress, gatewayAddress, nextHop string, nodePortEnable bool,
	vlanID uint) error {
	return setAnnotations(nodeAnnotator, map[string]string{
		ovnNodeGatewayMode:       string(config.GatewayModeShared),
		ovnNodeGatewayVlanID:     fmt.Sprintf("%d", vlanID),
		ovnNodeGatewayIfaceID:    ifaceID,
		ovnNodeGatewayMacAddress: macAddress,
		ovnNodeGatewayIP:         gatewayAddress,
		ovnNodeGatewayNextHop:    nextHop,
		ovnNodePortEnable:        fmt.Sprintf("%t", nodePortEnable),
	})
}

// SetSharedL3GatewayConfig uses nodeAnnotator set an l3-gateway-config annotation
// for the "localnet" gateway mode.
func SetLocalL3GatewayConfig(nodeAnnotator kube.Annotator,
	ifaceID, macAddress, gatewayAddress, nextHop string, nodePortEnable bool) error {
	return setAnnotations(nodeAnnotator, map[string]string{
		ovnNodeGatewayMode:       string(config.GatewayModeLocal),
		ovnNodeGatewayIfaceID:    ifaceID,
		ovnNodeGatewayMacAddress: macAddress,
		ovnNodeGatewayIP:         gatewayAddress,
		ovnNodeGatewayNextHop:    nextHop,
		ovnNodePortEnable:        fmt.Sprintf("%t", nodePortEnable),
	})
}

// ParseNodeL3GatewayAnnotation returns the parsed l3-gateway-config annotation
func ParseNodeL3GatewayAnnotation(node *kapi.Node) (*L3GatewayConfig, error) {
	l3GatewayAnnotation, ok := node.Annotations[ovnNodeL3GatewayConfig]
	if !ok {
		return nil, fmt.Errorf("%s annotation not found for node %q", ovnNodeL3GatewayConfig, node.Name)
	}

	l3GatewayConfigMap := map[string]map[string]string{}
	if err := json.Unmarshal([]byte(l3GatewayAnnotation), &l3GatewayConfigMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal l3 gateway config annotation %s for node %q", l3GatewayAnnotation, node.Name)
	}

	configRaw, ok := l3GatewayConfigMap[ovnDefaultNetworkGateway]
	if !ok {
		return nil, fmt.Errorf("%s annotation for %s network not found", ovnNodeL3GatewayConfig, ovnDefaultNetworkGateway)
	}

	l3GatewayConfig := &L3GatewayConfig{}
	l3GatewayConfig.Mode = config.GatewayMode(configRaw[ovnNodeGatewayMode])
	if l3GatewayConfig.Mode == config.GatewayModeDisabled {
		return l3GatewayConfig, nil
	} else if l3GatewayConfig.Mode != config.GatewayModeShared && l3GatewayConfig.Mode != config.GatewayModeLocal {
		return nil, fmt.Errorf("bad %q value %q", ovnNodeGatewayMode, l3GatewayConfig.Mode)
	}

	l3GatewayConfig.ChassisID, ok = node.Annotations[ovnNodeChassisID]
	if !ok {
		return nil, fmt.Errorf("%s annotation not found", ovnNodeChassisID)
	}

	l3GatewayConfig.InterfaceID, ok = configRaw[ovnNodeGatewayIfaceID]
	if !ok {
		return nil, fmt.Errorf("%s property not found in %s annotation", ovnNodeGatewayIfaceID, ovnNodeL3GatewayConfig)
	}

	var err error
	l3GatewayConfig.MACAddress, err = net.ParseMAC(configRaw[ovnNodeGatewayMacAddress])
	if err != nil {
		return nil, fmt.Errorf("bad %q value %q (%v)", ovnNodeGatewayMacAddress, configRaw[ovnNodeGatewayMacAddress], err)
	}

	ip, ipNet, err := net.ParseCIDR(configRaw[ovnNodeGatewayIP])
	if err != nil {
		return nil, fmt.Errorf("bad %q value %q (%v)", ovnNodeGatewayIP, configRaw[ovnNodeGatewayIP], err)
	}
	l3GatewayConfig.IPAddress = &net.IPNet{IP: ip, Mask: ipNet.Mask}

	l3GatewayConfig.NextHop = net.ParseIP(configRaw[ovnNodeGatewayNextHop])
	if l3GatewayConfig.NextHop == nil {
		return nil, fmt.Errorf("bad %q value %q", ovnNodeGatewayNextHop, configRaw[ovnNodeGatewayNextHop])
	}

	l3GatewayConfig.NodePortEnable = (configRaw[ovnNodePortEnable] == "true")

	if l3GatewayConfig.Mode == config.GatewayModeShared {
		vlanIDStr, ok := configRaw[ovnNodeGatewayVlanID]
		if ok {
			vlanID64, err := strconv.ParseUint(vlanIDStr, 10, 0)
			if err != nil {
				return nil, fmt.Errorf("bad %q value %q (%v)", ovnNodeGatewayVlanID, vlanIDStr, err)
			}
			vlanID := uint(vlanID64)
			l3GatewayConfig.VLANID = &vlanID
		}
	}

	return l3GatewayConfig, nil
}

func SetNodeManagementPortMacAddr(nodeAnnotator kube.Annotator, macAddress string) error {
	return nodeAnnotator.Set(ovnNodeManagementPortMacAddress, macAddress)
}

func ParseNodeManagementPortMacAddr(node *kapi.Node) (string, error) {
	macAddress, ok := node.Annotations[ovnNodeManagementPortMacAddress]
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

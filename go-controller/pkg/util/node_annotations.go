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

type l3GatewayConfigJSON struct {
	Mode           config.GatewayMode `json:"mode"`
	InterfaceID    string             `json:"interface-id,omitempty"`
	MACAddress     string             `json:"mac-address,omitempty"`
	IPAddress      string             `json:"ip-address,omitempty"`
	NextHop        string             `json:"next-hop,omitempty"`
	NodePortEnable string             `json:"node-port-enable,omitempty"`
	VLANID         string             `json:"vlan-id,omitempty"`
}

func (cfg *L3GatewayConfig) MarshalJSON() ([]byte, error) {
	cfgjson := l3GatewayConfigJSON{
		Mode: cfg.Mode,
	}
	if cfg.Mode == config.GatewayModeDisabled {
		return json.Marshal(&cfgjson)
	}

	cfgjson.InterfaceID = cfg.InterfaceID
	cfgjson.MACAddress = cfg.MACAddress.String()
	cfgjson.NodePortEnable = fmt.Sprintf("%t", cfg.NodePortEnable)
	if cfg.VLANID != nil {
		cfgjson.VLANID = fmt.Sprintf("%d", *cfg.VLANID)
	}
	cfgjson.IPAddress = cfg.IPAddress.String()
	cfgjson.NextHop = cfg.NextHop.String()

	return json.Marshal(&cfgjson)
}

func (cfg *L3GatewayConfig) UnmarshalJSON(bytes []byte) error {
	cfgjson := l3GatewayConfigJSON{}
	if err := json.Unmarshal(bytes, &cfgjson); err != nil {
		return err
	}

	cfg.Mode = cfgjson.Mode
	if cfg.Mode == config.GatewayModeDisabled {
		return nil
	} else if cfg.Mode != config.GatewayModeShared && cfg.Mode != config.GatewayModeLocal {
		return fmt.Errorf("bad 'mode' value %q", cfgjson.Mode)
	}

	cfg.InterfaceID = cfgjson.InterfaceID
	cfg.NodePortEnable = cfgjson.NodePortEnable == "true"
	if cfgjson.VLANID != "" {
		vlanID64, err := strconv.ParseUint(cfgjson.VLANID, 10, 0)
		if err != nil {
			return fmt.Errorf("bad 'vlan-id' value %q: %v", cfgjson.VLANID, err)
		}
		vlanID := uint(vlanID64)
		cfg.VLANID = &vlanID
	}

	var err error
	cfg.MACAddress, err = net.ParseMAC(cfgjson.MACAddress)
	if err != nil {
		return fmt.Errorf("bad 'mac-address' value %q: %v", cfgjson.MACAddress, err)
	}

	ip, ipnet, err := net.ParseCIDR(cfgjson.IPAddress)
	if err != nil {
		return fmt.Errorf("bad 'ip-address' value: %v", err)
	}
	cfg.IPAddress = &net.IPNet{IP: ip, Mask: ipnet.Mask}

	cfg.NextHop = net.ParseIP(cfgjson.NextHop)
	if cfg.NextHop == nil {
		return fmt.Errorf("bad 'next-hop' value %q", cfgjson.NextHop)
	}

	return nil
}

func SetL3GatewayConfig(nodeAnnotator kube.Annotator, cfg *L3GatewayConfig) error {
	gatewayAnnotation := map[string]*L3GatewayConfig{ovnDefaultNetworkGateway: cfg}
	if err := nodeAnnotator.Set(ovnNodeL3GatewayConfig, gatewayAnnotation); err != nil {
		return err
	}
	if cfg.ChassisID != "" {
		if err := nodeAnnotator.Set(ovnNodeChassisID, cfg.ChassisID); err != nil {
			return err
		}
	}
	return nil
}

// ParseNodeL3GatewayAnnotation returns the parsed l3-gateway-config annotation
func ParseNodeL3GatewayAnnotation(node *kapi.Node) (*L3GatewayConfig, error) {
	l3GatewayAnnotation, ok := node.Annotations[ovnNodeL3GatewayConfig]
	if !ok {
		return nil, fmt.Errorf("%s annotation not found for node %q", ovnNodeL3GatewayConfig, node.Name)
	}

	var cfgs map[string]*L3GatewayConfig
	if err := json.Unmarshal([]byte(l3GatewayAnnotation), &cfgs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal l3 gateway config annotation %s for node %q: %v", l3GatewayAnnotation, node.Name, err)
	}

	cfg, ok := cfgs[ovnDefaultNetworkGateway]
	if !ok {
		return nil, fmt.Errorf("%s annotation for %s network not found", ovnNodeL3GatewayConfig, ovnDefaultNetworkGateway)
	}

	if cfg.Mode != config.GatewayModeDisabled {
		cfg.ChassisID, ok = node.Annotations[ovnNodeChassisID]
		if !ok {
			return nil, fmt.Errorf("%s annotation not found", ovnNodeChassisID)
		}
	}

	return cfg, nil
}

func SetNodeManagementPortMACAddress(nodeAnnotator kube.Annotator, macAddress net.HardwareAddr) error {
	return nodeAnnotator.Set(ovnNodeManagementPortMacAddress, macAddress.String())
}

func ParseNodeManagementPortMACAddress(node *kapi.Node) (net.HardwareAddr, error) {
	macAddress, ok := node.Annotations[ovnNodeManagementPortMacAddress]
	if !ok {
		klog.Errorf("macAddress annotation not found for node %q ", node.Name)
		return nil, nil
	}

	return net.ParseMAC(macAddress)
}

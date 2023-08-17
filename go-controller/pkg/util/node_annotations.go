package util

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/netip"
	"strconv"

	"github.com/gaissmai/cidrtree"
	corev1 "k8s.io/api/core/v1"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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
//           "ip-addresses": ["169.254.33.2/24"],
//           "next-hops": ["169.254.33.1"],
//           "node-port-enable": "true",
//           "vlan-id": "0"
//
//           # backward-compat
//           "ip-address": "169.254.33.2/24",
//           "next-hop": "169.254.33.1",
//         }
//       }
//     k8s.ovn.org/node-chassis-id: b1f96182-2bdd-42b6-88f9-9a1fc1c85ece
//     k8s.ovn.org/node-mgmt-port-mac-address: fa:f1:27:f5:54:69
//
// The "ip_address" and "next_hop" fields are deprecated and will eventually go away.
// (And they are not output when "ip_addresses" or "next_hops" contains multiple
// values.)

const (
	// ovnNodeL3GatewayConfig is the constant string representing the l3 gateway annotation key
	ovnNodeL3GatewayConfig = "k8s.ovn.org/l3-gateway-config"

	// ovnNodeGatewayMtuSupport determines if option:gateway_mtu shall be set for GR router ports.
	ovnNodeGatewayMtuSupport = "k8s.ovn.org/gateway-mtu-support"

	// OvnDefaultNetworkGateway captures L3 gateway config for default OVN network interface
	ovnDefaultNetworkGateway = "default"

	// ovnNodeManagementPort is the constant string representing the annotation key
	ovnNodeManagementPort = "k8s.ovn.org/node-mgmt-port"

	// ovnNodeManagementPortMacAddress is the constant string representing the annotation key
	ovnNodeManagementPortMacAddress = "k8s.ovn.org/node-mgmt-port-mac-address"

	// ovnNodeChassisID is the systemID of the node needed for creating L3 gateway
	ovnNodeChassisID = "k8s.ovn.org/node-chassis-id"

	// ovnNodeCIDR is the CIDR form representation of primary network interface's attached IP address (i.e: 192.168.126.31/24 or 0:0:0:0:0:feff:c0a8:8e0c/64)
	ovnNodeIfAddr = "k8s.ovn.org/node-primary-ifaddr"

	// ovnNodeGRLRPAddr is the CIDR form representation of Gate Router LRP IP address to join switch (i.e: 100.64.0.5/24)
	ovnNodeGRLRPAddr = "k8s.ovn.org/node-gateway-router-lrp-ifaddr"

	// OvnNodeEgressLabel is a user assigned node label indicating to ovn-kubernetes that the node is to be used for egress IP assignment
	ovnNodeEgressLabel = "k8s.ovn.org/egress-assignable"

	// ovnNodeHostAddresses is used to track the different host IP addresses on the node
	ovnNodeHostAddresses = "k8s.ovn.org/host-addresses"

	// egressIPConfigAnnotationKey is used to indicate the cloud subnet and
	// capacity for each node. It is set by
	// openshift/cloud-network-config-controller
	cloudEgressIPConfigAnnotationKey = "cloud.network.openshift.io/egress-ipconfig"

	// ovnNodeZoneName is the zone to which the node belongs to. It is set by ovnkube-node.
	// ovnkube-node gets the node's zone from the OVN Southbound database.
	ovnNodeZoneName = "k8s.ovn.org/zone-name"

	/** HACK BEGIN **/
	// TODO(tssurya): Remove this annotation a few months from now (when one or two release jump
	// upgrades are done). This has been added only to minimize disruption for upgrades when
	// moving to interconnect=true.
	// We want the legacy ovnkube-master to wait for remote ovnkube-node to
	// signal it using "k8s.ovn.org/remote-zone-migrated" annotation before
	// considering a node as remote when we upgrade from "global" (1 zone IC)
	// zone to multi-zone. This is so that network disruption for the existing workloads
	// is negligible and until the point where ovnkube-node flips the switch to connect
	// to the new SBDB, it would continue talking to the legacy RAFT ovnkube-sbdb to ensure
	// OVN/OVS flows are intact.
	// ovnNodeMigratedZoneName is the zone to which the node belongs to. It is set by ovnkube-node.
	// ovnkube-node gets the node's zone from the OVN Southbound database.
	ovnNodeMigratedZoneName = "k8s.ovn.org/remote-zone-migrated"
	/** HACK END **/

	// ovnTransitSwitchPortAddr is the annotation to store the node Transit switch port ips.
	// It is set by cluster manager.
	ovnTransitSwitchPortAddr = "k8s.ovn.org/node-transit-switch-port-ifaddr"

	// ovnNodeID is the id (of type integer) of a node. It is set by cluster-manager.
	ovnNodeID = "k8s.ovn.org/node-id"

	// InvalidNodeID indicates an invalid node id
	InvalidNodeID = -1

	// ovnNetworkIDs is the constant string representing the ids allocated for the
	// default network and other layer3 secondary networks by cluster manager.
	ovnNetworkIDs = "k8s.ovn.org/network-ids"

	// invalidNetworkID signifies its an invalid network id
	InvalidNetworkID = -1
)

type L3GatewayConfig struct {
	Mode                config.GatewayMode
	ChassisID           string
	InterfaceID         string
	MACAddress          net.HardwareAddr
	IPAddresses         []*net.IPNet
	EgressGWInterfaceID string
	EgressGWMACAddress  net.HardwareAddr
	EgressGWIPAddresses []*net.IPNet
	NextHops            []net.IP
	NodePortEnable      bool
	VLANID              *uint
}

type l3GatewayConfigJSON struct {
	Mode                config.GatewayMode `json:"mode"`
	InterfaceID         string             `json:"interface-id,omitempty"`
	MACAddress          string             `json:"mac-address,omitempty"`
	IPAddresses         []string           `json:"ip-addresses,omitempty"`
	IPAddress           string             `json:"ip-address,omitempty"`
	EgressGWInterfaceID string             `json:"exgw-interface-id,omitempty"`
	EgressGWMACAddress  string             `json:"exgw-mac-address,omitempty"`
	EgressGWIPAddresses []string           `json:"exgw-ip-addresses,omitempty"`
	EgressGWIPAddress   string             `json:"exgw-ip-address,omitempty"`
	NextHops            []string           `json:"next-hops,omitempty"`
	NextHop             string             `json:"next-hop,omitempty"`
	NodePortEnable      string             `json:"node-port-enable,omitempty"`
	VLANID              string             `json:"vlan-id,omitempty"`
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
	cfgjson.EgressGWInterfaceID = cfg.EgressGWInterfaceID
	cfgjson.EgressGWMACAddress = cfg.EgressGWMACAddress.String()
	cfgjson.NodePortEnable = fmt.Sprintf("%t", cfg.NodePortEnable)
	if cfg.VLANID != nil {
		cfgjson.VLANID = fmt.Sprintf("%d", *cfg.VLANID)
	}

	cfgjson.IPAddresses = make([]string, len(cfg.IPAddresses))
	for i, ip := range cfg.IPAddresses {
		cfgjson.IPAddresses[i] = ip.String()
	}
	if len(cfgjson.IPAddresses) == 1 {
		cfgjson.IPAddress = cfgjson.IPAddresses[0]
	}
	cfgjson.EgressGWIPAddresses = make([]string, len(cfg.EgressGWIPAddresses))
	for i, ip := range cfg.EgressGWIPAddresses {
		cfgjson.EgressGWIPAddresses[i] = ip.String()
	}
	if len(cfgjson.EgressGWIPAddresses) == 1 {
		cfgjson.EgressGWIPAddress = cfgjson.EgressGWIPAddresses[0]
	}
	cfgjson.NextHops = make([]string, len(cfg.NextHops))
	for i, nh := range cfg.NextHops {
		cfgjson.NextHops[i] = nh.String()
	}
	if len(cfgjson.NextHops) == 1 {
		cfgjson.NextHop = cfgjson.NextHops[0]
	}

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
	cfg.EgressGWInterfaceID = cfgjson.EgressGWInterfaceID

	cfg.NodePortEnable = cfgjson.NodePortEnable == "true"
	if cfgjson.VLANID != "" {
		vlanID64, err := strconv.ParseUint(cfgjson.VLANID, 10, 0)
		if err != nil {
			return fmt.Errorf("bad 'vlan-id' value %q: %v", cfgjson.VLANID, err)
		}
		// VLANID is used for specifying TagRequest on the logical switch port
		// connected to the external logical switch, NB DB specifies a maximum
		// value on the TagRequest to 4095, hence validate this:
		//https://github.com/ovn-org/ovn/blob/4b97d6fa88e36206213b9fdc8e1e1a9016cfc736/ovn-nb.ovsschema#L94-L98
		if vlanID64 > 4095 {
			return fmt.Errorf("vlan-id surpasses maximum supported value")
		}
		vlanID := uint(vlanID64)
		cfg.VLANID = &vlanID
	}

	var err error
	cfg.MACAddress, err = net.ParseMAC(cfgjson.MACAddress)
	if err != nil {
		return fmt.Errorf("bad 'mac-address' value %q: %v", cfgjson.MACAddress, err)
	}

	if cfg.EgressGWInterfaceID != "" {
		cfg.EgressGWMACAddress, err = net.ParseMAC(cfgjson.EgressGWMACAddress)
		if err != nil {
			return fmt.Errorf("bad 'egress mac-address' value %q: %v", cfgjson.EgressGWMACAddress, err)
		}
		if len(cfgjson.EgressGWIPAddresses) == 0 {
			cfg.EgressGWIPAddresses = make([]*net.IPNet, 1)
			ip, ipnet, err := net.ParseCIDR(cfgjson.EgressGWIPAddress)
			if err != nil {
				return fmt.Errorf("bad 'ip-address' value %q: %v", cfgjson.EgressGWIPAddress, err)
			}
			cfg.EgressGWIPAddresses[0] = &net.IPNet{IP: ip, Mask: ipnet.Mask}
		} else {
			cfg.EgressGWIPAddresses = make([]*net.IPNet, len(cfgjson.EgressGWIPAddresses))
			for i, ipStr := range cfgjson.EgressGWIPAddresses {
				ip, ipnet, err := net.ParseCIDR(ipStr)
				if err != nil {
					return fmt.Errorf("bad 'ip-addresses' value %q: %v", ipStr, err)
				}
				cfg.EgressGWIPAddresses[i] = &net.IPNet{IP: ip, Mask: ipnet.Mask}
			}
		}
	}

	if len(cfgjson.IPAddresses) == 0 {
		cfg.IPAddresses = make([]*net.IPNet, 1)
		ip, ipnet, err := net.ParseCIDR(cfgjson.IPAddress)
		if err != nil {
			return fmt.Errorf("bad 'ip-address' value %q: %v", cfgjson.IPAddress, err)
		}
		cfg.IPAddresses[0] = &net.IPNet{IP: ip, Mask: ipnet.Mask}
	} else {
		cfg.IPAddresses = make([]*net.IPNet, len(cfgjson.IPAddresses))
		for i, ipStr := range cfgjson.IPAddresses {
			ip, ipnet, err := net.ParseCIDR(ipStr)
			if err != nil {
				return fmt.Errorf("bad 'ip-addresses' value %q: %v", ipStr, err)
			}
			cfg.IPAddresses[i] = &net.IPNet{IP: ip, Mask: ipnet.Mask}
		}
	}

	cfg.NextHops = make([]net.IP, len(cfgjson.NextHops))
	for i, nextHopStr := range cfgjson.NextHops {
		cfg.NextHops[i] = net.ParseIP(nextHopStr)
		if cfg.NextHops[i] == nil {
			return fmt.Errorf("bad 'next-hops' value %q", nextHopStr)
		}
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

// SetGatewayMTUSupport sets annotation "k8s.ovn.org/gateway-mtu-support" to "false" or removes the annotation from
// this node.
func SetGatewayMTUSupport(nodeAnnotator kube.Annotator, set bool) error {
	if set {
		nodeAnnotator.Delete(ovnNodeGatewayMtuSupport)
		return nil
	}
	return nodeAnnotator.Set(ovnNodeGatewayMtuSupport, "false")
}

// ParseNodeGatewayMTUSupport parses annotation "k8s.ovn.org/gateway-mtu-support". The default behavior should be true,
// therefore only an explicit string of "false" will make this function return false.
func ParseNodeGatewayMTUSupport(node *kapi.Node) bool {
	return node.Annotations[ovnNodeGatewayMtuSupport] != "false"
}

// ParseNodeL3GatewayAnnotation returns the parsed l3-gateway-config annotation
func ParseNodeL3GatewayAnnotation(node *kapi.Node) (*L3GatewayConfig, error) {
	l3GatewayAnnotation, ok := node.Annotations[ovnNodeL3GatewayConfig]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", ovnNodeL3GatewayConfig, node.Name)
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

func NodeL3GatewayAnnotationChanged(oldNode, newNode *kapi.Node) bool {
	return oldNode.Annotations[ovnNodeL3GatewayConfig] != newNode.Annotations[ovnNodeL3GatewayConfig]
}

// ParseNodeChassisIDAnnotation returns the node's ovnNodeChassisID annotation
func ParseNodeChassisIDAnnotation(node *kapi.Node) (string, error) {
	chassisID, ok := node.Annotations[ovnNodeChassisID]
	if !ok {
		return "", newAnnotationNotSetError("%s annotation not found for node %s", ovnNodeChassisID, node.Name)
	}

	return chassisID, nil
}

func NodeChassisIDAnnotationChanged(oldNode, newNode *kapi.Node) bool {
	return oldNode.Annotations[ovnNodeChassisID] != newNode.Annotations[ovnNodeChassisID]
}

type ManagementPortDetails struct {
	PfId   int `json:"PfId"`
	FuncId int `json:"FuncId"`
}

func SetNodeManagementPortAnnotation(nodeAnnotator kube.Annotator, PfId int, FuncId int) error {
	mgmtPortDetails := ManagementPortDetails{
		PfId:   PfId,
		FuncId: FuncId,
	}
	bytes, err := json.Marshal(mgmtPortDetails)
	if err != nil {
		return fmt.Errorf("failed to marshal mgmtPortDetails with PfId '%v', FuncId '%v'", PfId, FuncId)
	}
	return nodeAnnotator.Set(ovnNodeManagementPort, string(bytes))
}

// ParseNodeManagementPort returns the parsed host addresses living on a node
func ParseNodeManagementPortAnnotation(node *kapi.Node) (int, int, error) {
	mgmtPortAnnotation, ok := node.Annotations[ovnNodeManagementPort]
	if !ok {
		return -1, -1, newAnnotationNotSetError("%s annotation not found for node %q", ovnNodeManagementPort, node.Name)
	}

	cfg := ManagementPortDetails{}
	if err := json.Unmarshal([]byte(mgmtPortAnnotation), &cfg); err != nil {
		return -1, -1, fmt.Errorf("failed to unmarshal management port annotation %s for node %q: %v",
			mgmtPortAnnotation, node.Name, err)
	}

	return cfg.PfId, cfg.FuncId, nil
}

func SetNodeManagementPortMACAddress(nodeAnnotator kube.Annotator, macAddress net.HardwareAddr) error {
	return nodeAnnotator.Set(ovnNodeManagementPortMacAddress, macAddress.String())
}

func ParseNodeManagementPortMACAddress(node *kapi.Node) (net.HardwareAddr, error) {
	macAddress, ok := node.Annotations[ovnNodeManagementPortMacAddress]
	if !ok {
		return nil, newAnnotationNotSetError("macAddress annotation not found for node %q ", node.Name)
	}

	return net.ParseMAC(macAddress)
}

type primaryIfAddrAnnotation struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

// SetNodePrimaryIfAddr sets the IPv4 / IPv6 values of the node's primary network interface
func SetNodePrimaryIfAddrs(nodeAnnotator kube.Annotator, ifAddrs []*net.IPNet) (err error) {
	nodeIPNetv4, _ := MatchFirstIPNetFamily(false, ifAddrs)
	nodeIPNetv6, _ := MatchFirstIPNetFamily(true, ifAddrs)

	primaryIfAddrAnnotation := primaryIfAddrAnnotation{}
	if nodeIPNetv4 != nil {
		primaryIfAddrAnnotation.IPv4 = nodeIPNetv4.String()
	}
	if nodeIPNetv6 != nil {
		primaryIfAddrAnnotation.IPv6 = nodeIPNetv6.String()
	}
	return nodeAnnotator.Set(ovnNodeIfAddr, primaryIfAddrAnnotation)
}

// createPrimaryIfAddrAnnotation marshals the IPv4 / IPv6 values in the
// primaryIfAddrAnnotation format and stores it in the nodeAnnotation
// map with the provided 'annotationName' as key
func createPrimaryIfAddrAnnotation(annotationName string, nodeAnnotation map[string]interface{}, nodeIPNetv4,
	nodeIPNetv6 *net.IPNet) (map[string]interface{}, error) {
	if nodeAnnotation == nil {
		nodeAnnotation = make(map[string]interface{})
	}
	primaryIfAddrAnnotation := primaryIfAddrAnnotation{}
	if nodeIPNetv4 != nil {
		primaryIfAddrAnnotation.IPv4 = nodeIPNetv4.String()
	}
	if nodeIPNetv6 != nil {
		primaryIfAddrAnnotation.IPv6 = nodeIPNetv6.String()
	}
	bytes, err := json.Marshal(primaryIfAddrAnnotation)
	if err != nil {
		return nil, err
	}
	nodeAnnotation[annotationName] = string(bytes)
	return nodeAnnotation, nil
}

// CreateNodeGatewayRouterLRPAddrAnnotation sets the IPv4 / IPv6 values of the node's Gatewary Router LRP to join switch.
func CreateNodeGatewayRouterLRPAddrAnnotation(nodeAnnotation map[string]interface{}, nodeIPNetv4,
	nodeIPNetv6 *net.IPNet) (map[string]interface{}, error) {
	return createPrimaryIfAddrAnnotation(ovnNodeGRLRPAddr, nodeAnnotation, nodeIPNetv4, nodeIPNetv6)
}

func NodeGatewayRouterLRPAddrAnnotationChanged(oldNode, newNode *corev1.Node) bool {
	return oldNode.Annotations[ovnNodeGRLRPAddr] != newNode.Annotations[ovnNodeGRLRPAddr]
}

// CreateNodeTransitSwitchPortAddrAnnotation creates the node annotation for the node's Transit switch port addresses.
func CreateNodeTransitSwitchPortAddrAnnotation(nodeAnnotation map[string]interface{}, nodeIPNetv4,
	nodeIPNetv6 *net.IPNet) (map[string]interface{}, error) {
	return createPrimaryIfAddrAnnotation(ovnTransitSwitchPortAddr, nodeAnnotation, nodeIPNetv4, nodeIPNetv6)
}

func NodeTransitSwitchPortAddrAnnotationChanged(oldNode, newNode *corev1.Node) bool {
	return oldNode.Annotations[ovnTransitSwitchPortAddr] != newNode.Annotations[ovnTransitSwitchPortAddr]
}

const UnlimitedNodeCapacity = math.MaxInt32

type ifAddr struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

type Capacity struct {
	IPv4 int `json:"ipv4,omitempty"`
	IPv6 int `json:"ipv6,omitempty"`
	IP   int `json:"ip,omitempty"`
}

type nodeEgressIPConfiguration struct {
	Interface string   `json:"interface"`
	IFAddr    ifAddr   `json:"ifaddr"`
	Capacity  Capacity `json:"capacity"`
}

type ParsedIFAddr struct {
	IP  net.IP
	Net *net.IPNet
}

type ParsedNodeEgressIPConfiguration struct {
	V4       ParsedIFAddr
	V6       ParsedIFAddr
	Capacity Capacity
}

func getNodeIfAddrAnnotation(node *kapi.Node) (*primaryIfAddrAnnotation, error) {
	nodeIfAddrAnnotation, ok := node.Annotations[ovnNodeIfAddr]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", ovnNodeIfAddr, node.Name)
	}
	nodeIfAddr := &primaryIfAddrAnnotation{}
	if err := json.Unmarshal([]byte(nodeIfAddrAnnotation), nodeIfAddr); err != nil {
		return nil, fmt.Errorf("failed to unmarshal annotation: %s for node %q, err: %v", ovnNodeIfAddr, node.Name, err)
	}
	if nodeIfAddr.IPv4 == "" && nodeIfAddr.IPv6 == "" {
		return nil, fmt.Errorf("node: %q does not have any IP information set", node.Name)
	}
	return nodeIfAddr, nil
}

// ParseNodePrimaryIfAddr returns the IPv4 / IPv6 values for the node's primary network interface
func ParseNodePrimaryIfAddr(node *kapi.Node) (*ParsedNodeEgressIPConfiguration, error) {
	nodeIfAddr, err := getNodeIfAddrAnnotation(node)
	if err != nil {
		return nil, err
	}
	nodeEgressIPConfig := nodeEgressIPConfiguration{
		IFAddr: ifAddr(*nodeIfAddr),
		Capacity: Capacity{
			IP:   UnlimitedNodeCapacity,
			IPv4: UnlimitedNodeCapacity,
			IPv6: UnlimitedNodeCapacity,
		},
	}
	parsedEgressIPConfig, err := parseNodeEgressIPConfig(&nodeEgressIPConfig)
	if err != nil {
		return nil, err
	}
	return parsedEgressIPConfig, nil
}

// ParseNodeGatewayRouterLRPAddr returns the IPv4 / IPv6 values for the node's gateway router
func ParseNodeGatewayRouterLRPAddr(node *kapi.Node) (net.IP, error) {
	nodeIfAddrAnnotation, ok := node.Annotations[ovnNodeGRLRPAddr]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", ovnNodeGRLRPAddr, node.Name)
	}
	nodeIfAddr := primaryIfAddrAnnotation{}
	if err := json.Unmarshal([]byte(nodeIfAddrAnnotation), &nodeIfAddr); err != nil {
		return nil, fmt.Errorf("failed to unmarshal annotation: %s for node %q, err: %v", ovnNodeGRLRPAddr, node.Name, err)
	}
	if nodeIfAddr.IPv4 == "" && nodeIfAddr.IPv6 == "" {
		return nil, fmt.Errorf("node: %q does not have any IP information set", node.Name)
	}
	ip, _, err := net.ParseCIDR(nodeIfAddr.IPv4)
	if err != nil {
		return nil, fmt.Errorf("failed to parse annotation: %s for node %q, err: %v", ovnNodeGRLRPAddr, node.Name, err)
	}
	return ip, nil
}

// parsePrimaryIfAddrAnnotation unmarshals the IPv4 / IPv6 values in the
// primaryIfAddrAnnotation format from the nodeAnnotation map with the
// provided 'annotationName' as key and returns the addresses.
func parsePrimaryIfAddrAnnotation(node *kapi.Node, annotationName string) ([]*net.IPNet, error) {
	nodeIfAddrAnnotation, ok := node.Annotations[annotationName]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", annotationName, node.Name)
	}
	nodeIfAddr := primaryIfAddrAnnotation{}
	if err := json.Unmarshal([]byte(nodeIfAddrAnnotation), &nodeIfAddr); err != nil {
		return nil, fmt.Errorf("failed to unmarshal annotation: %s for node %q, err: %w", annotationName, node.Name, err)
	}
	if nodeIfAddr.IPv4 == "" && nodeIfAddr.IPv6 == "" {
		return nil, fmt.Errorf("node: %q does not have any IP information set", node.Name)
	}
	var ipAddrs []*net.IPNet
	if nodeIfAddr.IPv4 != "" {
		ip, ipNet, err := net.ParseCIDR(nodeIfAddr.IPv4)
		if err != nil {
			return nil, fmt.Errorf("failed to parse IPv4 address %s from annotation: %s for node %q, err: %w", nodeIfAddr.IPv4, annotationName, node.Name, err)
		}
		ipAddrs = append(ipAddrs, &net.IPNet{IP: ip, Mask: ipNet.Mask})
	}

	if nodeIfAddr.IPv6 != "" {
		ip, ipNet, err := net.ParseCIDR(nodeIfAddr.IPv6)
		if err != nil {
			return nil, fmt.Errorf("failed to parse IPv6 address %s from annotation: %s for node %q, err: %w", nodeIfAddr.IPv6, annotationName, node.Name, err)
		}
		ipAddrs = append(ipAddrs, &net.IPNet{IP: ip, Mask: ipNet.Mask})
	}

	return ipAddrs, nil
}

// ParseNodeGatewayRouterLRPAddrs returns the IPv4 and/or IPv6 addresses for the node's gateway router port
// stored in the 'ovnNodeGRLRPAddr' annotation
func ParseNodeGatewayRouterLRPAddrs(node *kapi.Node) ([]*net.IPNet, error) {
	return parsePrimaryIfAddrAnnotation(node, ovnNodeGRLRPAddr)
}

// ParseNodeTransitSwitchPortAddrs returns the IPv4 and/or IPv6 addresses for the node's transit switch port
// stored in the 'ovnTransitSwitchPortAddr' annotation
func ParseNodeTransitSwitchPortAddrs(node *kapi.Node) ([]*net.IPNet, error) {
	return parsePrimaryIfAddrAnnotation(node, ovnTransitSwitchPortAddr)
}

// ParseCloudEgressIPConfig returns the cloud's information concerning the node's primary network interface
func ParseCloudEgressIPConfig(node *kapi.Node) (*ParsedNodeEgressIPConfiguration, error) {
	egressIPConfigAnnotation, ok := node.Annotations[cloudEgressIPConfigAnnotationKey]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", cloudEgressIPConfigAnnotationKey, node.Name)
	}
	nodeEgressIPConfig := []nodeEgressIPConfiguration{
		{
			Capacity: Capacity{
				IP:   UnlimitedNodeCapacity,
				IPv4: UnlimitedNodeCapacity,
				IPv6: UnlimitedNodeCapacity,
			},
		},
	}
	if err := json.Unmarshal([]byte(egressIPConfigAnnotation), &nodeEgressIPConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal annotation: %s for node %q, err: %v", ovnNodeIfAddr, node.Name, err)
	}
	if len(nodeEgressIPConfig) == 0 {
		return nil, fmt.Errorf("empty annotation: %s for node: %q", cloudEgressIPConfigAnnotationKey, node.Name)
	}

	parsedEgressIPConfig, err := parseNodeEgressIPConfig(&nodeEgressIPConfig[0])
	if err != nil {
		return nil, err
	}

	// ParsedNodeEgressIPConfiguration.V[4|6].IP is used to verify if an egress IP matches node IP to disable its creation
	// use node IP instead of the value assigned from cloud egress CIDR config
	nodeIfAddr, err := getNodeIfAddrAnnotation(node)
	if err != nil {
		return nil, err
	}
	if nodeIfAddr.IPv4 != "" {
		ipv4, _, err := net.ParseCIDR(nodeIfAddr.IPv4)
		if err != nil {
			return nil, err
		}
		parsedEgressIPConfig.V4.IP = ipv4
	}
	if nodeIfAddr.IPv6 != "" {
		ipv6, _, err := net.ParseCIDR(nodeIfAddr.IPv6)
		if err != nil {
			return nil, err
		}
		parsedEgressIPConfig.V6.IP = ipv6
	}

	return parsedEgressIPConfig, nil
}

func parseNodeEgressIPConfig(egressIPConfig *nodeEgressIPConfiguration) (*ParsedNodeEgressIPConfiguration, error) {
	parsedEgressIPConfig := &ParsedNodeEgressIPConfiguration{
		Capacity: egressIPConfig.Capacity,
	}
	if egressIPConfig.IFAddr.IPv4 != "" {
		ipv4, v4Subnet, err := net.ParseCIDR(egressIPConfig.IFAddr.IPv4)
		if err != nil {
			return nil, err
		}
		parsedEgressIPConfig.V4 = ParsedIFAddr{
			IP:  ipv4,
			Net: v4Subnet,
		}
	}
	if egressIPConfig.IFAddr.IPv6 != "" {
		ipv6, v6Subnet, err := net.ParseCIDR(egressIPConfig.IFAddr.IPv6)
		if err != nil {
			return nil, err
		}
		parsedEgressIPConfig.V6 = ParsedIFAddr{
			IP:  ipv6,
			Net: v6Subnet,
		}
	}
	return parsedEgressIPConfig, nil
}

// GetNodeEgressLabel returns label annotation needed for marking nodes as egress assignable
func GetNodeEgressLabel() string {
	return ovnNodeEgressLabel
}

func SetNodeHostAddresses(nodeAnnotator kube.Annotator, addresses sets.Set[string]) error {
	return nodeAnnotator.Set(ovnNodeHostAddresses, sets.List(addresses))
}

func NodeHostAddressesAnnotationChanged(oldNode, newNode *v1.Node) bool {
	return oldNode.Annotations[ovnNodeHostAddresses] != newNode.Annotations[ovnNodeHostAddresses]
}

// ParseNodeHostAddresses returns the parsed host addresses living on a node
func ParseNodeHostAddresses(node *kapi.Node) (sets.Set[string], error) {
	addrAnnotation, ok := node.Annotations[ovnNodeHostAddresses]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", ovnNodeHostAddresses, node.Name)
	}

	var cfg []string
	if err := json.Unmarshal([]byte(addrAnnotation), &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal host addresses annotation %s for node %q: %v",
			addrAnnotation, node.Name, err)
	}

	return sets.New(cfg...), nil
}

// ParseNodeHostAddressesDropNetMask returns the parsed host addresses found on a nodes annotation. Removes the mask.
func ParseNodeHostAddressesDropNetMask(node *kapi.Node) (sets.Set[string], error) {
	addrAnnotation, ok := node.Annotations[ovnNodeHostAddresses]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", ovnNodeHostAddresses, node.Name)
	}

	var cfg []string
	if err := json.Unmarshal([]byte(addrAnnotation), &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal host addresses annotation %s for node %q: %v",
			addrAnnotation, node.Name, err)
	}

	for i, cidr := range cfg {
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil || ip == nil {
			return nil, fmt.Errorf("failed to parse node host address: %v", err)
		}
		cfg[i] = ip.String()
	}
	return sets.New(cfg...), nil
}

func ParseNodeHostAddressesList(node *kapi.Node) ([]string, error) {
	addrAnnotation, ok := node.Annotations[ovnNodeHostAddresses]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", ovnNodeHostAddresses, node.Name)
	}

	var cfg []string
	if err := json.Unmarshal([]byte(addrAnnotation), &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal host addresses annotation %s for node %q: %v",
			addrAnnotation, node.Name, err)
	}
	return cfg, nil
}

// IsNonOVNManagedNetworkContainingIP attempts to find a non OVN managed network that will host the argument IP. If no network is
// found, false is returned
func IsNonOVNManagedNetworkContainingIP(node *v1.Node, ip net.IP) (bool, error) {
	if ip == nil {
		return false, fmt.Errorf("empty IP is not valid")
	}
	if node == nil {
		return false, fmt.Errorf("unable to determine if IP %s is a non OVN managed network because node argument is nil", ip.String())
	}
	network, err := GetNonOVNNetworkContainingIP(node, ip)
	if err != nil {
		return false, fmt.Errorf("failed to determine if IP %s is hosted by a non OVN managed network for node %s: %v",
			ip.String(), node.Name, err)
	}
	if network == "" {
		return false, nil
	}
	return true, nil
}

// GetEgressIPNetwork attempts to retrieve a network that contains EgressIP. It first checks the primary OVN managed network,
// otherwise searches through non-OVN managed networks
func GetEgressIPNetwork(node *v1.Node, eIP net.IP) (string, error) {
	primaryNetworks, err := getNodeIfAddrAnnotation(node)
	if err != nil {
		return "", fmt.Errorf("failed to get node address annotation for node %s: %v", node.Name, err)
	}
	primaryNetwork := primaryNetworks.IPv4
	if utilnet.IsIPv6(eIP) {
		primaryNetwork = primaryNetworks.IPv6
	}
	if primaryNetwork != "" {
		_, primaryNet, err := net.ParseCIDR(primaryNetwork)
		if err != nil {
			return "", fmt.Errorf("failed to parse CIDR %s for node %s: %v", primaryNetwork, node.Name, err)
		}
		if primaryNet.Contains(eIP) {
			return primaryNet.String(), nil
		}
	}
	network, err := GetNonOVNNetworkContainingIP(node, eIP)
	if err != nil {
		return "", fmt.Errorf("failed to get Egress IP %s network for node %s: %v", eIP.String(), node.Name, err)
	}
	return network, nil
}

// IsOVNManagedNetwork attempts to detect if the argument IP can be hosted by a network managed by OVN. Currently, this is
// only the primary OVN network
func IsOVNManagedNetwork(node *v1.Node, ip net.IP) (bool, error) {
	if ip == nil {
		return false, fmt.Errorf("empty IP is not valid")
	}
	if node == nil {
		return false, fmt.Errorf("unable to determine if IP %s is OVN managed because node argument is nil", ip.String())
	}
	isIPV6 := utilnet.IsIPv6(ip)
	primaryNetworks, err := getNodeIfAddrAnnotation(node)
	if err != nil {
		return false, fmt.Errorf("failed to determine if IP %s is OVN managed: %v", ip.String(), err)
	}
	if isIPV6 {
		if primaryNetworks.IPv6 != "" {
			_, primaryIPv6IPNet, err := net.ParseCIDR(primaryNetworks.IPv6)
			if err != nil {
				return false, fmt.Errorf("failed to parse IPv6 IP CIDR %s and therefore unable to detect if "+
					"IP %s is OVN managed: %v", primaryNetworks.IPv6, ip.String(), err)
			}
			if primaryIPv6IPNet.Contains(ip) {
				return true, nil
			} else {
				return false, nil
			}
		}
	} else {
		if primaryNetworks.IPv4 != "" {
			_, primaryIPv4IPNet, err := net.ParseCIDR(primaryNetworks.IPv4)
			if err != nil {
				return false, fmt.Errorf("failed to parse IPv4 IP CIDR %s and therefore unable to detect if "+
					"IP %s is OVN managed: %v", primaryNetworks.IPv4, ip.String(), err)
			}
			if primaryIPv4IPNet.Contains(ip) {
				return true, nil
			} else {
				return false, nil
			}
		}
	}
	return false, fmt.Errorf("unable to determine if IP %s is within the OVN managed network for node %s", ip.String(), node.Name)
}

// GetNonOVNNetworkContainingIP attempts to find a non OVN managed network to host the argument IP
func GetNonOVNNetworkContainingIP(node *v1.Node, ip net.IP) (string, error) {
	networks, err := ParseNodeHostAddressesList(node)
	if err != nil {
		return "", fmt.Errorf("failed to get host-addresses annotation for node %s: %v", node.Name, err)
	}
	cidrs, err := makeCIDRs(networks...)
	if err != nil {
		return "", fmt.Errorf("failed to determine non-OVN managed network for node %s that may host IP %s: %w",
			node.Name, ip.String(), err)
	}
	lpmTree := cidrtree.New(cidrs...)
	addr, err := netip.ParseAddr(ip.String())
	if err != nil {
		return "", fmt.Errorf("failed to parse egress IP %s: %v", ip.String(), err)
	}
	match, found := lpmTree.Lookup(addr)
	if !found {
		return "", nil
	}
	return match.String(), nil
}

// UpdateNodeIDAnnotation updates the ovnNodeID annotation with the node id in the annotations map
// and returns it.
func UpdateNodeIDAnnotation(annotations map[string]interface{}, nodeID int) map[string]interface{} {
	if annotations == nil {
		annotations = make(map[string]interface{})
	}

	annotations[ovnNodeID] = strconv.Itoa(nodeID)
	return annotations
}

// GetNodeID returns the id of the node set in the 'ovnNodeID' node annotation.
// Returns InvalidNodeID (-1) if the 'ovnNodeID' node annotation is not set or if the value is
// not an integer value.
func GetNodeID(node *kapi.Node) int {
	nodeID, ok := node.Annotations[ovnNodeID]
	if !ok {
		return InvalidNodeID
	}

	id, err := strconv.Atoi(nodeID)
	if err != nil {
		return InvalidNodeID
	}
	return id
}

// NodeIDAnnotationChanged returns true if the ovnNodeID in the corev1.Nodes doesn't match
func NodeIDAnnotationChanged(oldNode, newNode *corev1.Node) bool {
	return oldNode.Annotations[ovnNodeID] != newNode.Annotations[ovnNodeID]
}

// SetNodeZone sets the node's zone in the 'ovnNodeZoneName' node annotation.
func SetNodeZone(nodeAnnotator kube.Annotator, zoneName string) error {
	return nodeAnnotator.Set(ovnNodeZoneName, zoneName)
}

/** HACK BEGIN **/
// TODO(tssurya): Remove this a few months from now
// SetNodeZoneMigrated sets the node's zone in the 'ovnNodeMigratedZoneName' node annotation.
func SetNodeZoneMigrated(nodeAnnotator kube.Annotator, zoneName string) error {
	return nodeAnnotator.Set(ovnNodeMigratedZoneName, zoneName)
}

// HasNodeMigratedZone returns true if node has its ovnNodeMigratedZoneName set already
func HasNodeMigratedZone(node *kapi.Node) bool {
	_, ok := node.Annotations[ovnNodeMigratedZoneName]
	return ok
}

// NodeMigratedZoneAnnotationChanged returns true if the ovnNodeMigratedZoneName annotation changed for the node
func NodeMigratedZoneAnnotationChanged(oldNode, newNode *corev1.Node) bool {
	return oldNode.Annotations[ovnNodeMigratedZoneName] != newNode.Annotations[ovnNodeMigratedZoneName]
}

/** HACK END **/

// GetNodeZone returns the zone of the node set in the 'ovnNodeZoneName' node annotation.
// If the annotation is not set, it returns the 'default' zone name.
func GetNodeZone(node *kapi.Node) string {
	zoneName, ok := node.Annotations[ovnNodeZoneName]
	if !ok {
		return types.OvnDefaultZone
	}

	return zoneName
}

// NodeZoneAnnotationChanged returns true if the ovnNodeZoneName in the corev1.Nodes doesn't match
func NodeZoneAnnotationChanged(oldNode, newNode *corev1.Node) bool {
	return oldNode.Annotations[ovnNodeZoneName] != newNode.Annotations[ovnNodeZoneName]
}

func parseNetworkIDsAnnotation(nodeAnnotations map[string]string, annotationName string) (map[string]string, error) {
	annotation, ok := nodeAnnotations[annotationName]
	if !ok {
		return nil, newAnnotationNotSetError("could not find %q annotation", annotationName)
	}

	networkIdsStrMap := map[string]string{}
	networkIds := make(map[string]string)
	if err := json.Unmarshal([]byte(annotation), &networkIds); err != nil {
		return nil, fmt.Errorf("could not parse %q annotation %q : %v",
			annotationName, annotation, err)
	}
	for netName, v := range networkIds {
		networkIdsStrMap[netName] = v
	}

	if len(networkIdsStrMap) == 0 {
		return nil, fmt.Errorf("unexpected empty %s annotation", annotationName)
	}

	return networkIdsStrMap, nil
}

// ParseNetworkIDAnnotation parses the 'ovnNetworkIDs' annotation for the specified
// network in 'netName' and returns the network id.
func ParseNetworkIDAnnotation(node *kapi.Node, netName string) (int, error) {
	networkIDsMap, err := parseNetworkIDsAnnotation(node.Annotations, ovnNetworkIDs)
	if err != nil {
		return InvalidNetworkID, err
	}

	networkID, ok := networkIDsMap[netName]
	if !ok {
		return InvalidNetworkID, newAnnotationNotSetError("node %q has no %q annotation for network %s", node.Name, ovnNetworkIDs, netName)
	}

	return strconv.Atoi(networkID)
}

// updateNetworkIDsAnnotation updates the ovnNetworkIDs annotation in the 'annotations' map
// with the provided network id in 'networkID'.  If 'networkID' is InvalidNetworkID (-1)
// it deletes the ovnNetworkIDs annotation from the map.
func updateNetworkIDsAnnotation(annotations map[string]string, netName string, networkID int) error {
	var bytes []byte

	// First get the all network ids for all existing networks
	networkIDsMap, err := parseNetworkIDsAnnotation(annotations, ovnNetworkIDs)
	if err != nil {
		if !IsAnnotationNotSetError(err) {
			return fmt.Errorf("failed to parse node network id annotation %q: %v",
				annotations, err)
		}
		// in the case that the annotation does not exist
		networkIDsMap = map[string]string{}
	}

	// add or delete network id of the specified network
	if networkID == InvalidNetworkID {
		delete(networkIDsMap, netName)
	} else {
		networkIDsMap[netName] = strconv.Itoa(networkID)
	}

	// if no networks left, just delete the network ids annotation from node annotations.
	if len(networkIDsMap) == 0 {
		delete(annotations, ovnNetworkIDs)
		return nil
	}

	// Marshal all network ids back to annotations.
	networkIdsStrMap := make(map[string]string)
	for n, id := range networkIDsMap {
		networkIdsStrMap[n] = id
	}
	bytes, err = json.Marshal(networkIdsStrMap)
	if err != nil {
		return err
	}
	annotations[ovnNetworkIDs] = string(bytes)
	return nil
}

// UpdateNetworkIDAnnotation updates the ovnNetworkIDs annotation for the network name 'netName' with the network id 'networkID'.
// If 'networkID' is invalid network ID (-1), then it deletes that network from the network ids annotation.
func UpdateNetworkIDAnnotation(annotations map[string]string, netName string, networkID int) (map[string]string, error) {
	if annotations == nil {
		annotations = map[string]string{}
	}
	err := updateNetworkIDsAnnotation(annotations, netName, networkID)
	if err != nil {
		return nil, err
	}
	return annotations, nil
}

// GetNodeNetworkIDsAnnotationNetworkIDs parses the "k8s.ovn.org/network-ids" annotation
// on a node and returns the map of network name and ids.
func GetNodeNetworkIDsAnnotationNetworkIDs(node *kapi.Node) (map[string]int, error) {
	networkIDsStrMap, err := parseNetworkIDsAnnotation(node.Annotations, ovnNetworkIDs)
	if err != nil {
		return nil, err
	}

	networkIDsMap := map[string]int{}
	for netName, v := range networkIDsStrMap {
		id, e := strconv.Atoi(v)
		if e == nil {
			networkIDsMap[netName] = id
		}
	}

	return networkIDsMap, nil
}

// NodeNetworkIDAnnotationChanged returns true if the ovnNetworkIDs annotation in the corev1.Nodes doesn't match
func NodeNetworkIDAnnotationChanged(oldNode, newNode *corev1.Node, netName string) bool {
	oldNodeNetID, _ := ParseNetworkIDAnnotation(oldNode, netName)
	newNodeNetID, _ := ParseNetworkIDAnnotation(newNode, netName)
	return oldNodeNetID != newNodeNetID
}

func makeCIDRs(s ...string) (cidrs []netip.Prefix, err error) {
	for _, cidrString := range s {
		prefix, err := netip.ParsePrefix(cidrString)
		if err != nil {
			return nil, err
		}
		cidrs = append(cidrs, prefix)
	}
	return cidrs, nil
}

package ovn

import (
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/util/retry"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/sirupsen/logrus"
)

const (
	// OvnHostSubnetLegacy is the old constant string representing the node subnet annotation key
	OvnHostSubnetLegacy = "ovn_host_subnet"
	// OvnNodeSubnets is the constant string representing the node subnets annotation key
	OvnNodeSubnets = "k8s.ovn.org/node-subnets"
	// OvnNodeManagementPortMacAddress is the constant string representing the annotation key
	OvnNodeManagementPortMacAddress = "k8s.ovn.org/node-mgmt-port-mac-address"
	// OvnNodeChassisID is the systemID of the node needed for creating L3 gateway
	OvnNodeChassisID = "k8s.ovn.org/node-chassis-id"
	// OvnServiceIdledAt is a constant string representing the Service annotation key
	// whose value indicates the time stamp in RFC3339 format when a Service was idled
	OvnServiceIdledAt = "k8s.ovn.org/idled-at"
	// OvnNodeL3GatewayConfig is the constant string representing the l3 gateway annotation key
	OvnNodeL3GatewayConfig = "k8s.ovn.org/l3-gateway-config"
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
	// OvnDefaultNetworkGateway captures L3 gateway config for default OVN network interface
	OvnDefaultNetworkGateway = "default"
)

// StartClusterMaster runs a subnet IPAM and a controller that watches arrival/departure
// of nodes in the cluster
// On an addition to the cluster (node create), a new subnet is created for it that will translate
// to creation of a logical switch (done by the node, but could be created here at the master process too)
// Upon deletion of a node, the switch will be deleted
//
// TODO: Verify that the cluster was not already called with a different global subnet
//  If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func (oc *Controller) StartClusterMaster(masterNodeName string) error {

	existingNodes, err := oc.kube.GetNodes()
	if err != nil {
		logrus.Errorf("Error in initializing/fetching subnets: %v", err)
		return err
	}
	for _, clusterEntry := range config.Default.ClusterSubnets {
		err := oc.masterSubnetAllocator.AddNetworkRange(clusterEntry.CIDR.String(), clusterEntry.HostBits())
		if err != nil {
			return err
		}
	}
	for _, node := range existingNodes.Items {
		hostsubnet, _ := parseNodeHostSubnet(&node)
		if hostsubnet != nil {
			err := oc.masterSubnetAllocator.MarkAllocatedNetwork(hostsubnet.String())
			if err != nil {
				utilruntime.HandleError(err)
			}
		}
	}

	if _, _, err := util.RunOVNNbctl("--columns=_uuid", "list", "port_group"); err == nil {
		oc.portGroupSupport = true
	}

	if oc.multicastSupport {
		// Multicast support requires portGroupSupport
		if oc.portGroupSupport {
			if _, _, err := util.RunOVNSbctl("--columns=_uuid", "list", "IGMP_Group"); err != nil {
				logrus.Warningf("Multicast support enabled, however version of OVN in use does not support IGMP Group. " +
					"Disabling Multicast Support")
				oc.multicastSupport = false
			}
		} else {
			logrus.Warningf("Multicast support enabled, however version of OVN in use does not support Port Group. " +
				"Disabling Multicast Support")
			oc.multicastSupport = false
		}
		if config.IPv6Mode {
			logrus.Warningf("Multicast support enabled, but can not be used along with IPv6. Disabling Multicast Support")
			oc.multicastSupport = false
		}
	}

	if err := oc.SetupMaster(masterNodeName); err != nil {
		logrus.Errorf("Failed to setup master (%v)", err)
		return err
	}

	return nil
}

// SetupMaster creates the central router and load-balancers for the network
func (oc *Controller) SetupMaster(masterNodeName string) error {
	clusterRouter := util.GetK8sClusterRouter()
	// Create a single common distributed router for the cluster.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "lr-add", clusterRouter,
		"--", "set", "logical_router", clusterRouter, "external_ids:k8s-cluster-router=yes")
	if err != nil {
		logrus.Errorf("Failed to create a single common distributed router for the cluster, "+
			"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// If supported, enable IGMP relay on the router to forward multicast
	// traffic between nodes.
	if oc.multicastSupport {
		stdout, stderr, err = util.RunOVNNbctl("--", "set", "logical_router",
			clusterRouter, "options:mcast_relay=\"true\"")
		if err != nil {
			logrus.Errorf("Failed to enable IGMP relay on the cluster router, "+
				"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}

		// Drop IP multicast globally. Multicast is allowed only if explicitly
		// enabled in a namespace.
		err = createDefaultDenyMulticastPolicy()
		if err != nil {
			logrus.Errorf("Failed to create default deny multicast policy, error: %v",
				err)
			return err
		}
	}

	// Create 2 load-balancers for east-west traffic.  One handles UDP and another handles TCP.
	oc.TCPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes")
	if err != nil {
		logrus.Errorf("Failed to get tcp load-balancer, stderr: %q, error: %v", stderr, err)
		return err
	}

	if oc.TCPLoadBalancerUUID == "" {
		oc.TCPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes", "protocol=tcp")
		if err != nil {
			logrus.Errorf("Failed to create tcp load-balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	oc.UDPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes")
	if err != nil {
		logrus.Errorf("Failed to get udp load-balancer, stderr: %q, error: %v", stderr, err)
		return err
	}
	if oc.UDPLoadBalancerUUID == "" {
		oc.UDPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes", "protocol=udp")
		if err != nil {
			logrus.Errorf("Failed to create udp load-balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	// Create a logical switch called "join" that will be used to connect gateway routers to the distributed router.
	// The "join" switch will be allocated IP addresses in the range 100.64.0.0/16 or fd98::/64.
	var joinSubnet string
	if config.IPv6Mode {
		joinSubnet = "fd98::1/64"
	} else {
		joinSubnet = "100.64.0.1/16"
	}
	joinIP, joinCIDR, _ := net.ParseCIDR(joinSubnet)
	if config.IPv6Mode {
		stdout, stderr, err = util.RunOVNNbctl("--may-exist", "ls-add", "join",
			"--", "set", "logical_switch", "join", fmt.Sprintf("%s=%s", config.OtherConfigSubnet(), joinCIDR.String()))
	} else {
		stdout, stderr, err = util.RunOVNNbctl("--may-exist", "ls-add", "join",
			"--", "set", "logical_switch", "join", fmt.Sprintf("%s=%s", config.OtherConfigSubnet(), joinCIDR.String()),
			"--", "set", "logical_switch", "join", fmt.Sprintf("other-config:exclude_ips=%s", joinIP.String()))
	}
	if err != nil {
		logrus.Errorf("Failed to create logical switch called \"join\", stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Connect the distributed router to "join".
	routerMac, stderr, err := util.RunOVNNbctl("--if-exist", "get", "logical_router_port", "rtoj-"+clusterRouter, "mac")
	if err != nil {
		logrus.Errorf("Failed to get logical router port rtoj-%v, stderr: %q, error: %v", clusterRouter, stderr, err)
		return err
	}
	if routerMac == "" {
		routerMac = util.GenerateMac()
		stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lrp-add", clusterRouter,
			"rtoj-"+clusterRouter, routerMac, joinSubnet)
		if err != nil {
			logrus.Errorf("Failed to add logical router port rtoj-%v, stdout: %q, stderr: %q, error: %v",
				clusterRouter, stdout, stderr, err)
			return err
		}
	}

	// Connect the switch "join" to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", "join", "jtor-"+clusterRouter,
		"--", "set", "logical_switch_port", "jtor-"+clusterRouter, "type=router",
		"options:router-port=rtoj-"+clusterRouter, "addresses="+"\""+routerMac+"\"")
	if err != nil {
		logrus.Errorf("Failed to add router-type logical switch port to join, stdout: %q, stderr: %q, error: %v",
			stdout, stderr, err)
		return err
	}

	return nil
}

func parseNodeManagementPortMacAddr(node *kapi.Node) (string, error) {
	macAddress, ok := node.Annotations[OvnNodeManagementPortMacAddress]
	if !ok {
		logrus.Errorf("macAddress annotation not found for node %q ", node.Name)
		return "", nil
	}

	_, err := net.ParseMAC(macAddress)
	if err != nil {
		return "", fmt.Errorf("Error %v in parsing node %v macAddress %v", err, node.Name, macAddress)
	}

	return macAddress, nil
}

func (oc *Controller) syncNodeManagementPort(node *kapi.Node, subnet *net.IPNet) error {

	macAddress, err := parseNodeManagementPortMacAddr(node)
	if err != nil {
		return err
	}

	if macAddress == "" {
		// When macAddress was removed, delete the switch port
		stdout, stderr, err := util.RunOVNNbctl("--", "--if-exists", "lsp-del", "k8s-"+node.Name)
		if err != nil {
			logrus.Errorf("Failed to delete logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		}

		return nil
	}

	if subnet == nil {
		subnet, err = parseNodeHostSubnet(node)
		if err != nil {
			return err
		}
	}

	_, portIP := util.GetNodeWellKnownAddresses(subnet)

	// Create this node's management logical port on the node switch
	var stdout, stderr string
	if config.IPv6Mode {
		stdout, stderr, err = util.RunOVNNbctl(
			"--", "--may-exist", "lsp-add", node.Name, "k8s-"+node.Name,
			"--", "lsp-set-addresses", "k8s-"+node.Name, macAddress+" "+portIP.IP.String())
	} else {
		stdout, stderr, err = util.RunOVNNbctl(
			"--", "--may-exist", "lsp-add", node.Name, "k8s-"+node.Name,
			"--", "lsp-set-addresses", "k8s-"+node.Name, macAddress+" "+portIP.IP.String(),
			"--", "--if-exists", "remove", "logical_switch", node.Name, "other-config", "exclude_ips")
	}
	if err != nil {
		logrus.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	if err := addAllowACLFromNode(node.Name, portIP.IP); err != nil {
		return err
	}

	return nil
}

// UnmarshalPodAnnotation returns a the unmarshalled pod annotation
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

func parseGatewayIfaceID(l3GatewayConfig map[string]string) (string, error) {
	ifaceID, ok := l3GatewayConfig[OvnNodeGatewayIfaceID]
	if !ok || ifaceID == "" {
		return "", fmt.Errorf("%s annotation not found or invalid", OvnNodeGatewayIfaceID)
	}

	return ifaceID, nil
}

func parseGatewayMacAddress(l3GatewayConfig map[string]string) (string, error) {
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

func parseGatewayLogicalNetwork(l3GatewayConfig map[string]string) (string, string, error) {
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

func parseGatewayVLANID(l3GatewayConfig map[string]string, ifaceID string) ([]string, error) {

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

func parseNodeChassisID(node *kapi.Node) (string, error) {
	systemID, ok := node.Annotations[OvnNodeChassisID]
	if !ok {
		return "", fmt.Errorf("%s annotation not found", OvnNodeChassisID)
	}
	return systemID, nil
}

func (oc *Controller) syncGatewayLogicalNetwork(node *kapi.Node, l3GatewayConfig map[string]string, subnet string) error {
	var err error
	var clusterSubnets []string
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR.String())
	}

	mode := l3GatewayConfig[OvnNodeGatewayMode]
	nodePortEnable := false
	if l3GatewayConfig[OvnNodePortEnable] == "true" {
		nodePortEnable = true
	}
	ifaceID, err := parseGatewayIfaceID(l3GatewayConfig)
	if err != nil {
		return err
	}

	gwMacAddress, err := parseGatewayMacAddress(l3GatewayConfig)
	if err != nil {
		return err
	}

	ipAddress, gwNextHop, err := parseGatewayLogicalNetwork(l3GatewayConfig)
	if err != nil {
		return err
	}

	systemID, err := parseNodeChassisID(node)
	if err != nil {
		return err
	}

	var lspArgs []string
	var lspErr error
	if mode == string(config.GatewayModeShared) {
		lspArgs, lspErr = parseGatewayVLANID(l3GatewayConfig, ifaceID)
		if lspErr != nil {
			return lspErr
		}
	}

	err = util.GatewayInit(clusterSubnets, systemID, node.Name, ifaceID, ipAddress,
		gwMacAddress, gwNextHop, subnet, nodePortEnable, lspArgs)
	if err != nil {
		return fmt.Errorf("failed to init shared interface gateway: %v", err)
	}

	if mode == string(config.GatewayModeShared) {
		// Add static routes to OVN Cluster Router to enable pods on this Node to
		// reach the host IP
		err = addStaticRouteToHost(node, ipAddress)
		if err != nil {
			return err
		}
	}

	if nodePortEnable {
		err = oc.handleNodePortLB(node)
	}

	return err
}

func addStaticRouteToHost(node *kapi.Node, nicIP string) error {
	k8sClusterRouter := util.GetK8sClusterRouter()
	subnet, err := parseNodeHostSubnet(node)
	if err != nil {
		return fmt.Errorf("failed to get interface IP address for %s (%v)",
			util.GetK8sMgmtIntfName(node.Name), err)
	}
	_, secondIP := util.GetNodeWellKnownAddresses(subnet)
	prefix := strings.Split(nicIP, "/")[0] + "/32"
	nexthop := strings.Split(secondIP.String(), "/")[0]
	_, stderr, err := util.RunOVNNbctl("--may-exist", "lr-route-add", k8sClusterRouter, prefix, nexthop)
	if err != nil {
		return fmt.Errorf("failed to add static route '%s via %s' for host %q on %s "+
			"stderr: %q, error: %v", nicIP, secondIP, node.Name, k8sClusterRouter, stderr, err)
	}

	return nil
}

func parseNodeHostSubnet(node *kapi.Node) (*net.IPNet, error) {
	sub, ok := node.Annotations[OvnNodeSubnets]
	if !ok {
		sub, ok = node.Annotations[OvnHostSubnetLegacy]
	} else {
		nodeSubnets := make(map[string]string)
		if err := json.Unmarshal([]byte(sub), &nodeSubnets); err != nil {
			return nil, fmt.Errorf("error parsing node-subnets annotation: %v", err)
		}
		sub, ok = nodeSubnets["default"]
	}
	if !ok {
		return nil, fmt.Errorf("node %q has no subnet annotation", node.Name)
	}

	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		return nil, fmt.Errorf("Error in parsing hostsubnet - %v", err)
	}

	return subnet, nil
}

func (oc *Controller) ensureNodeLogicalNetwork(nodeName string, hostsubnet *net.IPNet) error {

	// Get firstIP for gateway.  Skip the second address of the LogicalSwitch's
	// subnet since we set it aside for the management port on that node.
	firstIP, secondIP := util.GetNodeWellKnownAddresses(hostsubnet)
	nodeLRPMac := util.IPAddrToHWAddr(firstIP.IP)
	clusterRouter := util.GetK8sClusterRouter()

	// Create a router port and provide it the first address on the node's host subnet
	_, stderr, err := util.RunOVNNbctl("--may-exist", "lrp-add", clusterRouter, "rtos-"+nodeName,
		nodeLRPMac, firstIP.String())
	if err != nil {
		logrus.Errorf("Failed to add logical port to router, stderr: %q, error: %v", stderr, err)
		return err
	}

	// Create a logical switch and set its subnet.
	var stdout string
	if config.IPv6Mode {
		stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "ls-add", nodeName,
			"--", "set", "logical_switch", nodeName, config.OtherConfigSubnet()+"="+hostsubnet.String(),
			"external-ids:gateway_ip="+firstIP.String())
	} else {
		stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "ls-add", nodeName,
			"--", "set", "logical_switch", nodeName, config.OtherConfigSubnet()+"="+hostsubnet.String(),
			"other-config:exclude_ips="+secondIP.IP.String(),
			"external-ids:gateway_ip="+firstIP.String())
	}
	if err != nil {
		logrus.Errorf("Failed to create a logical switch %v, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	// If supported, enable IGMP snooping and querier on the node.
	if oc.multicastSupport {
		stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch",
			nodeName, "other-config:mcast_snoop=\"true\"")
		if err != nil {
			logrus.Errorf("Failed to enable IGMP on logical switch %v, stdout: %q, stderr: %q, error: %v",
				nodeName, stdout, stderr, err)
			return err
		}

		// Configure querier only if we have an IPv4 address, otherwise
		// disable querier.
		if firstIP.IP.To4() != nil {
			stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch",
				nodeName, "other-config:mcast_querier=\"true\"",
				"other-config:mcast_eth_src=\""+nodeLRPMac+"\"",
				"other-config:mcast_ip4_src=\""+firstIP.IP.String()+"\"")
			if err != nil {
				logrus.Errorf("Failed to enable IGMP Querier on logical switch %v, stdout: %q, stderr: %q, error: %v",
					nodeName, stdout, stderr, err)
				return err
			}
		} else {
			stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch",
				nodeName, "other-config:mcast_querier=\"false\"")
			if err != nil {
				logrus.Errorf("Failed to disable IGMP Querier on logical switch %v, stdout: %q, stderr: %q, error: %v",
					nodeName, stdout, stderr, err)
				return err
			}
			logrus.Infof("Disabled IGMP Querier on logical switch %v (No IPv4 Source IP available)",
				nodeName)
		}
	}

	// Connect the switch to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", nodeName, "stor-"+nodeName,
		"--", "set", "logical_switch_port", "stor-"+nodeName, "type=router", "options:router-port=rtos-"+nodeName, "addresses="+"\""+nodeLRPMac+"\"")
	if err != nil {
		logrus.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Add our cluster TCP and UDP load balancers to the node switch
	if oc.TCPLoadBalancerUUID == "" {
		return fmt.Errorf("TCP cluster load balancer not created")
	}
	stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch", nodeName, "load_balancer="+oc.TCPLoadBalancerUUID)
	if err != nil {
		logrus.Errorf("Failed to set logical switch %v's loadbalancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	if oc.UDPLoadBalancerUUID == "" {
		return fmt.Errorf("UDP cluster load balancer not created")
	}
	stdout, stderr, err = util.RunOVNNbctl("add", "logical_switch", nodeName, "load_balancer", oc.UDPLoadBalancerUUID)
	if err != nil {
		logrus.Errorf("Failed to add logical switch %v's loadbalancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	return nil
}

// annotate the node with the subnet information assigned to node's logical switch. the
// new format of the annotation is:
//
// k8s.ovn.org/node-subnets: {
//	  "default": "192.168.2.1",
// }
func (oc *Controller) addNodeAnnotations(node *kapi.Node, subnet string) error {
	// nothing to do if the node already has the annotation key
	_, ok := node.Annotations[OvnNodeSubnets]
	if ok {
		return nil
	}

	bytes, err := json.Marshal(map[string]string{
		"default": subnet,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal node %q annotation for subnet %s",
			node.Name, subnet)
	}
	nodeAnnotations := make(map[string]interface{})
	// if legacy annotation key exists, then remove it
	nodeAnnotations[OvnHostSubnetLegacy] = nil
	nodeAnnotations[OvnNodeSubnets] = string(bytes)
	err = oc.kube.SetAnnotationsOnNode(node, nodeAnnotations)
	if err != nil {
		return fmt.Errorf("failed to set node annotation %q on existing node %s to %q: %v",
			OvnNodeSubnets, node.Name, subnet, err)
	}
	return nil
}

func (oc *Controller) addNode(node *kapi.Node) (hostsubnet *net.IPNet, err error) {
	oc.clearInitialNodeNetworkUnavailableCondition(node, nil)

	hostsubnet, _ = parseNodeHostSubnet(node)
	if hostsubnet != nil {
		// Update the node's annotation to use the new annotation key and remove the
		// old annotation key.
		err = oc.addNodeAnnotations(node, hostsubnet.String())
		if err != nil {
			return nil, err
		}
		// Node already has subnet assigned; ensure its logical network is set up
		return hostsubnet, oc.ensureNodeLogicalNetwork(node.Name, hostsubnet)
	}

	// Node doesn't have a subnet assigned; reserve a new one for it
	hostsubnetStr, err := oc.masterSubnetAllocator.AllocateNetwork()
	if err != nil {
		return nil, fmt.Errorf("Error allocating network for node %s: %v", node.Name, err)
	}
	logrus.Infof("Allocated node %s HostSubnet %s", node.Name, hostsubnetStr)

	_, hostsubnet, err = net.ParseCIDR(hostsubnetStr)
	if err != nil {
		return nil, fmt.Errorf("Error in parsing hostsubnet %s - %v", hostsubnetStr, err)
	}

	defer func() {
		// Release the allocation on error
		if err != nil {
			_ = oc.masterSubnetAllocator.ReleaseNetwork(hostsubnetStr)
		}
	}()

	// Ensure that the node's logical network has been created
	err = oc.ensureNodeLogicalNetwork(node.Name, hostsubnet)
	if err != nil {
		return nil, err
	}

	// Set the HostSubnet annotation on the node object to signal
	// to nodes that their logical infrastructure is set up and they can
	// proceed with their initialization
	err = oc.addNodeAnnotations(node, hostsubnet.String())
	if err != nil {
		return nil, err
	}

	return hostsubnet, nil
}

func (oc *Controller) deleteNodeHostSubnet(nodeName string, subnet *net.IPNet) error {
	err := oc.masterSubnetAllocator.ReleaseNetwork(subnet.String())
	if err != nil {
		return fmt.Errorf("Error deleting subnet %v for node %q: %s", subnet, nodeName, err)
	}
	logrus.Infof("Deleted HostSubnet %v for node %s", subnet, nodeName)
	return nil
}

func (oc *Controller) deleteNodeLogicalNetwork(nodeName string) error {
	// Remove the logical switch associated with the node
	if _, stderr, err := util.RunOVNNbctl("--if-exist", "ls-del", nodeName); err != nil {
		return fmt.Errorf("Failed to delete logical switch %s, "+
			"stderr: %q, error: %v", nodeName, stderr, err)
	}

	// Remove the patch port that connects distributed router to node's logical switch
	if _, stderr, err := util.RunOVNNbctl("--if-exist", "lrp-del", "rtos-"+nodeName); err != nil {
		return fmt.Errorf("Failed to delete logical router port rtos-%s, "+
			"stderr: %q, error: %v", nodeName, stderr, err)
	}

	return nil
}

func (oc *Controller) deleteNode(nodeName string, nodeSubnet *net.IPNet) error {
	// Clean up as much as we can but don't hard error
	if nodeSubnet != nil {
		if err := oc.deleteNodeHostSubnet(nodeName, nodeSubnet); err != nil {
			logrus.Errorf("Error deleting node %s HostSubnet: %v", nodeName, err)
		}
	}

	if err := oc.deleteNodeLogicalNetwork(nodeName); err != nil {
		logrus.Errorf("Error deleting node %s logical network: %v", nodeName, err)
	}

	if err := util.GatewayCleanup(nodeName, nodeSubnet); err != nil {
		return fmt.Errorf("Failed to clean up node %s gateway: (%v)", nodeName, err)
	}

	return nil
}

// OVN uses an overlay and doesn't need GCE Routes, we need to
// clear the NetworkUnavailable condition that kubelet adds to initial node
// status when using GCE (done here: https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/cloud/node_controller.go#L237).
// See discussion surrounding this here: https://github.com/kubernetes/kubernetes/pull/34398.
// TODO: make upstream kubelet more flexible with overlays and GCE so this
// condition doesn't get added for network plugins that don't want it, and then
// we can remove this function.
func (oc *Controller) clearInitialNodeNetworkUnavailableCondition(origNode, newNode *kapi.Node) {
	// If it is not a Cloud Provider node, then nothing to do.
	if origNode.Spec.ProviderID == "" {
		return
	}
	// if newNode is not nil, then we are called from UpdateFunc()
	if newNode != nil && reflect.DeepEqual(origNode.Status.Conditions, newNode.Status.Conditions) {
		return
	}

	cleared := false
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error

		oldNode, err := oc.kube.GetNode(origNode.Name)
		if err != nil {
			return err
		}
		// Informer cache should not be mutated, so get a copy of the object
		node := oldNode.DeepCopy()

		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == kapi.NodeNetworkUnavailable {
				condition := &node.Status.Conditions[i]
				if condition.Status != kapi.ConditionFalse && condition.Reason == "NoRouteCreated" {
					condition.Status = kapi.ConditionFalse
					condition.Reason = "RouteCreated"
					condition.Message = "ovn-kube cleared kubelet-set NoRouteCreated"
					condition.LastTransitionTime = metav1.Now()
					if err = oc.kube.UpdateNodeStatus(node); err == nil {
						cleared = true
					}
				}
				break
			}
		}
		return err
	})
	if resultErr != nil {
		logrus.Errorf("status update failed for local node %s: %v", origNode.Name, resultErr)
	} else if cleared {
		logrus.Infof("Cleared node NetworkUnavailable/NoRouteCreated condition for %s", origNode.Name)
	}
}

func (oc *Controller) syncNodes(nodes []interface{}) {
	foundNodes := make(map[string]*kapi.Node)
	for _, tmp := range nodes {
		node, ok := tmp.(*kapi.Node)
		if !ok {
			logrus.Errorf("Spurious object in syncNodes: %v", tmp)
			continue
		}
		foundNodes[node.Name] = node
	}

	// We only deal with cleaning up nodes that shouldn't exist here, since
	// watchNodes() will be called for all existing nodes at startup anyway.
	// Note that this list will include the 'join' cluster switch, which we
	// do not want to delete.
	nodeSwitches, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name,other-config", "find", "logical_switch",
		fmt.Sprintf("%s!=_", config.OtherConfigSubnet()))
	if err != nil {
		logrus.Errorf("Failed to get node logical switches: stderr: %q, error: %v",
			stderr, err)
		return
	}
	for _, result := range strings.Split(nodeSwitches, "\n\n") {
		// Split result into name and other-config
		items := strings.Split(result, "\n")
		if len(items) != 2 || len(items[0]) == 0 {
			continue
		}
		if items[0] == "join" {
			// Don't delete the cluster switch
			continue
		}
		if _, ok := foundNodes[items[0]]; ok {
			// node still exists, no cleanup to do
			continue
		}

		var subnet *net.IPNet
		configs := strings.Fields(items[1])
		for _, config := range configs {
			if strings.HasPrefix(config, "subnet=") {
				subnetStr := strings.TrimPrefix(config, "subnet=")
				_, subnet, _ = net.ParseCIDR(subnetStr)
				break
			}
		}

		if err := oc.deleteNode(items[0], subnet); err != nil {
			logrus.Error(err)
		}
	}
}

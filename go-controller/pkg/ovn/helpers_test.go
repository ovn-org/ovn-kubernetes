package ovn

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// helper functions to make testing easier and remove redundant code
func clusterRouter(policies ...string) *nbdb.LogicalRouter {
	clusterRouter := &nbdb.LogicalRouter{
		UUID:     ovntypes.OVNClusterRouter + "-UUID",
		Name:     ovntypes.OVNClusterRouter,
		Policies: policies,
	}
	return clusterRouter

}

// Please use following subnets for various networks that we have
// 172.16.1.0/24 -- physical network that k8s nodes connect to
// 100.64.0.0/16 -- the join subnet that connects all the L3 gateways with the distributed router
// 169.254.33.0/24 -- the subnet that connects OVN logical network to physical network
// 10.1.0.0/16 -- the overlay subnet that Pods connect to.

type tNode struct {
	Name                 string
	NodeIP               string
	NodeLRPMAC           string
	LrpIP                string
	LrpIPv6              string
	DrLrpIP              string
	PhysicalBridgeMAC    string
	SystemID             string
	NodeSubnet           string
	GWRouter             string
	ClusterIPNet         string
	ClusterCIDR          string
	GatewayRouterIPMask  string
	GatewayRouterIP      string
	GatewayRouterNextHop string
	PhysicalBridgeName   string
	NodeGWIP             string
	NodeMgmtPortIP       string
	NodeMgmtPortMAC      string
	DnatSnatIP           string

	//new variables for generating database components
	StaticRoutes           []string
	Nat                    []string
	Ports                  []string
	LoadBalancerGroupUUIDs []string
}

const (
	// ovnNodeID is the id (of type integer) of a node. It is set by cluster-manager.
	ovnNodeID = "k8s.ovn.org/node-id"

	// ovnNodeGRLRPAddr is the CIDR form representation of Gate Router LRP IP address to join switch (i.e: 100.64.0.5/24)
	ovnNodeGRLRPAddr     = "k8s.ovn.org/node-gateway-router-lrp-ifaddr"
	ovnNodePrimaryIfAddr = "k8s.ovn.org/node-primary-ifaddr"
)

func (n tNode) k8sNode(nodeID string) v1.Node {
	node := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: n.Name,
			Annotations: map[string]string{
				ovnNodeID:             nodeID,
				ovnNodeGRLRPAddr:      "{\"ipv4\": \"100.64.0." + nodeID + "/16\"}",
				util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\"]", fmt.Sprintf("%s/24", n.NodeIP)),
				ovnNodePrimaryIfAddr:  fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", fmt.Sprintf("%s/24", n.NodeIP), ""),
			},
		},
		Status: kapi.NodeStatus{
			Addresses: []kapi.NodeAddress{{Type: kapi.NodeExternalIP, Address: n.NodeIP}},
		},
	}

	return node
}

func (n tNode) ifaceID() string {
	return n.PhysicalBridgeName + "_" + n.Name
}

func (n tNode) gatewayConfig(gatewayMode config.GatewayMode, vlanID uint) *util.L3GatewayConfig {
	return &util.L3GatewayConfig{
		Mode:           gatewayMode,
		ChassisID:      n.SystemID,
		InterfaceID:    n.ifaceID(),
		MACAddress:     ovntest.MustParseMAC(n.PhysicalBridgeMAC),
		IPAddresses:    ovntest.MustParseIPNets(n.GatewayRouterIPMask),
		NextHops:       ovntest.MustParseIPs(n.GatewayRouterNextHop),
		NodePortEnable: true,
		VLANID:         &vlanID,
	}
}

func (n tNode) logicalSwitch() *nbdb.LogicalSwitch {
	return &nbdb.LogicalSwitch{
		UUID:              n.Name + "-UUID",
		Name:              n.Name,
		OtherConfig:       map[string]string{"subnet": n.NodeSubnet},
		LoadBalancerGroup: n.LoadBalancerGroupUUIDs,
		Ports:             n.Ports,
	}
}

func (n tNode) nodeGWRouter() *nbdb.LogicalRouter {
	return &nbdb.LogicalRouter{
		UUID:         fmt.Sprintf("GR_%s-UUID", n.Name),
		Name:         fmt.Sprintf("GR_%s", n.Name),
		StaticRoutes: n.StaticRoutes,
		Nat:          n.Nat,
	}
}

func (n *tNode) generateLogicalSwitchPortAddToNode(pod testPod) (*nbdb.LogicalSwitchPort, error) {
	// make sure we are adding the pod to the right node
	if n.Name != pod.nodeName {
		return nil, fmt.Errorf("pod and node names do not match")
	}

	lspUUID := fmt.Sprintf("%s_%s-UUID", pod.namespace, pod.podName)

	n.Ports = append(n.Ports, lspUUID)
	return &nbdb.LogicalSwitchPort{
		UUID:      lspUUID,
		Addresses: []string{fmt.Sprintf("%s %s", pod.podMAC, pod.podIP)},
		ExternalIDs: map[string]string{
			"pod":       "true",
			"namespace": pod.namespace,
		},
		Name: fmt.Sprintf("%s_%s", pod.namespace, pod.podName),
		Options: map[string]string{
			"iface-id-ver":      pod.podName,
			"requested-chassis": pod.nodeName,
		},
		PortSecurity: []string{fmt.Sprintf("%s %s", pod.podMAC, pod.podIP)},
	}, nil

}

func (n *tNode) deleteLogicalSwitchPortFromNode(pod testPod) {
	lspUUID := fmt.Sprintf("%s_%s-UUID", pod.namespace, pod.podName)

	for index, lspPort := range n.Ports {
		if lspPort == lspUUID {
			n.Ports = append(n.Ports[:index], n.Ports[index+1:]...)
			return
		}
	}

	return

}

func nodeGWRouter(nodeName string, staticRoutes []string) *nbdb.LogicalRouter {
	return &nbdb.LogicalRouter{
		UUID:         fmt.Sprintf("GR_%s-UUID", nodeName),
		Name:         fmt.Sprintf("GR_%s", nodeName),
		StaticRoutes: staticRoutes,
	}
}

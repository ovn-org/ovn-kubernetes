// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	hotypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressqosfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"
	egressservicefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

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
}

const (
	// ovnNodeID is the id (of type integer) of a node. It is set by cluster-manager.
	ovnNodeID            = "k8s.ovn.org/node-id"
	ovnNodePrimaryIfAddr = "k8s.ovn.org/node-primary-ifaddr"
)

func (n tNode) k8sNode(nodeID string) corev1.Node {
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: n.Name,
			Annotations: map[string]string{
				ovnNodeID:                     nodeID,
				util.OVNNodeHostCIDRs:         fmt.Sprintf("[\"%s\"]", fmt.Sprintf("%s/24", n.NodeIP)),
				ovnNodePrimaryIfAddr:          fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", fmt.Sprintf("%s/24", n.NodeIP), ""),
				util.OVNNodeEncapIPs:          fmt.Sprintf("[\"%s\"]", n.NodeIP),
				util.OvnTransitSwitchPortAddr: `{"ipv4":"100.88.0.2/16"}`,
			},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: n.NodeIP}},
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
		BridgeID:       n.PhysicalBridgeName,
		InterfaceID:    n.ifaceID(),
		MACAddress:     ovntest.MustParseMAC(n.PhysicalBridgeMAC),
		IPAddresses:    ovntest.MustParseIPNets(n.GatewayRouterIPMask),
		NextHops:       ovntest.MustParseIPs(n.GatewayRouterNextHop),
		NodePortEnable: true,
		VLANID:         &vlanID,
	}
}

func (n tNode) logicalSwitch(loadBalancerGroupUUIDs []string) *nbdb.LogicalSwitch {
	return &nbdb.LogicalSwitch{
		UUID:              n.Name + "-UUID",
		Name:              n.Name,
		OtherConfig:       map[string]string{"subnet": n.NodeSubnet},
		LoadBalancerGroup: loadBalancerGroupUUIDs,
	}
}

/*
func cleanupGateway(fexec *ovntest.FakeExec, nodeName string, nodeSubnet string, clusterCIDR string, nextHop string) {
	const (
		node1RouteUUID    string = "0cac12cf-3e0f-4682-b028-5ea2e0001962"
		node1mgtRouteUUID string = "0cac12cf-3e0f-4682-b028-5ea2e0001963"
	)

	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --if-exist get logical_router_port " + types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName + " networks",
		Output: "[\"100.64.0.3/16\"]",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=\"100.64.0.3\"",
		Output: node1RouteUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove logical_router " + types.OVNClusterRouter + " static_routes " + node1RouteUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exist lsp-del " + types.JoinSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName,
		"ovn-nbctl --timeout=15 --if-exist lr-del " + types.GWRouterPrefix + nodeName,
		"ovn-nbctl --timeout=15 --if-exist ls-del " + types.ExternalSwitchPrefix + nodeName,
	})
}

func defaultFakeExec(nodeSubnet, nodeName string, sctpSupport bool) *ovntest.FakeExec {
	const (
		mgmtMAC string = "01:02:03:04:05:06"
	)

	fexec := ovntest.NewLooseCompareFakeExec()
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --columns=_uuid list port_group",
		"ovn-sbctl --timeout=15 --columns=_uuid list IGMP_Group",
		"ovn-nbctl --timeout=15 -- --may-exist lr-add ovn_cluster_router -- set logical_router ovn_cluster_router external_ids:k8s-cluster-router=yes external_ids:k8s-ovn-topo-version=" + fmt.Sprintf("%d", types.OvnCurrentTopologyVersion),
	})
	if sctpSupport {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovsdb-client list-columns  --data=bare --no-heading --format=json OVN_Northbound Load_Balancer",
			Output: `{"data":[["_version","uuid"],["health_check",{"key":{"refTable":"Load_Balancer_Health_Check","type":"uuid"},"max":"unlimited","min":0}],["name","string"],["protocol",{"key":{"enum":["set",["sctp","tcp","udp"]],"type":"string"},"min":0}]],"headings":["Column","Type"]}`,
		})
	} else {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovsdb-client list-columns  --data=bare --no-heading --format=json OVN_Northbound Load_Balancer",
			Output: `{"data":[["_version","uuid"],["health_check",{"key":{"refTable":"Load_Balancer_Health_Check","type":"uuid"},"max":"unlimited","min":0}],["name","string"],["protocol",{"key":{"enum":["set",["tcp","udp"]],"type":"string"},"min":0}]],"headings":["Column","Type"]}`,
		})
	}
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 -- set logical_router ovn_cluster_router options:mcast_relay=\"true\"",
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"(ip4.mcast || mldv1 || mldv2 || " + ipv6DynamicMulticastMatch + ")\" action=drop external-ids:default-deny-policy-type=Egress",
		"ovn-nbctl --timeout=15 --id=@acl create acl priority=" + types.DefaultMcastDenyPriority + " direction=" + types.DirectionFromLPort + " log=false match=\"(ip4.mcast || mldv1 || mldv2 || " + ipv6DynamicMulticastMatch + ")\" action=drop external-ids:default-deny-policy-type=Egress -- add port_group  acls @acl",
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"(ip4.mcast || mldv1 || mldv2 || " + ipv6DynamicMulticastMatch + ")\" action=drop external-ids:default-deny-policy-type=Ingress",
		"ovn-nbctl --timeout=15 --id=@acl create acl priority=" + types.DefaultMcastDenyPriority + " direction=" + types.DirectionToLPort + " log=false match=\"(ip4.mcast || mldv1 || mldv2 || " + ipv6DynamicMulticastMatch + ")\" action=drop external-ids:default-deny-policy-type=Ingress -- add port_group  acls @acl",
	})
	drSwitchPort := types.JoinSwitchToGWRouterPrefix + types.OVNClusterRouter
	drRouterPort := types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter
	joinSubnetV4 := ovntest.MustParseIPNet("100.64.0.1/16")
	joinLRPMAC := util.IPAddrToHWAddr(joinSubnetV4.IP)
	joinSubnetV6 := ovntest.MustParseIPNet("fd98::1/64")

	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --may-exist ls-add " + types.OVNJoinSwitch,
		"ovn-nbctl --timeout=15 -- --if-exists lrp-del " + drRouterPort + " -- lrp-add " + types.OVNClusterRouter + " " + drRouterPort +
			" " + joinLRPMAC.String() + " " + joinSubnetV4.String() + " " + joinSubnetV6.String(),
		"ovn-nbctl --timeout=15 --may-exist lsp-add " + types.OVNJoinSwitch + " " + drSwitchPort + " -- set logical_switch_port " +
			drSwitchPort + " type=router options:router-port=" + drRouterPort + " addresses=router",
	})

	// Node-related logical network stuff
	cidr := ovntest.MustParseIPNet(nodeSubnet)
	cidr.IP = util.NextIP(cidr.IP)
	lrpMAC := util.IPAddrToHWAddr(cidr.IP).String()
	gwCIDR := cidr.String()
	gwIP := cidr.IP.String()
	nodeMgmtPortIP := util.NextIP(cidr.IP)
	hybridOverlayIP := util.NextIP(nodeMgmtPortIP)

	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_" + nodeName + " networks",
		"ovn-nbctl --timeout=15 --data=bare --no-heading --format=csv --columns=name,other-config find logical_switch",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists lrp-del " + types.RouterToSwitchPrefix + nodeName + " -- lrp-add ovn_cluster_router " + types.RouterToSwitchPrefix + nodeName + " " + lrpMAC + " " + gwCIDR,
		"ovn-nbctl --timeout=15 --may-exist ls-add " + nodeName + " -- set logical_switch " + nodeName + " other-config:subnet=" + nodeSubnet + " other-config:exclude_ips=" + nodeMgmtPortIP.String() + ".." + hybridOverlayIP.String(),
		"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " other-config:mcast_snoop=\"true\"",
		"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " other-config:mcast_querier=\"true\" other-config:mcast_eth_src=\"" + lrpMAC + "\" other-config:mcast_ip4_src=\"" + gwIP + "\"",
		"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " " + types.SwitchToRouterPrefix + nodeName + " -- lsp-set-type " + types.SwitchToRouterPrefix + nodeName + " router -- lsp-set-options " + types.SwitchToRouterPrefix + nodeName + " router-port=" + types.RouterToSwitchPrefix + nodeName + " -- lsp-set-addresses " + types.SwitchToRouterPrefix + nodeName + " router",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port " + types.SwitchToRouterPrefix + nodeName + " _uuid",
		Output: fakeUUID + "\n",
	})

	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " " + types.K8sPrefix + nodeName + " -- lsp-set-type " + types.K8sPrefix + nodeName + "  -- lsp-set-options " + types.K8sPrefix + nodeName + "  -- lsp-set-addresses " + types.K8sPrefix + nodeName + " " + mgmtMAC + " " + nodeMgmtPortIP.String(),
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port " + types.K8sPrefix + nodeName + " _uuid",
		Output: fakeUUID + "\n",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 lsp-list " + nodeName,
		Output: "29df5ce5-2802-4ee5-891f-4fb27ca776e9 (" + types.K8sPrefix + nodeName + ")",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 -- --if-exists set logical_switch " + nodeName + " other-config:exclude_ips=" + hybridOverlayIP.String(),
	})

	return fexec
}

func addNodeportLBs(fexec *ovntest.FakeExec, nodeName, tcpLBUUID, udpLBUUID, sctpLBUUID string) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + types.GatewayLBTCP + "=" + types.GWRouterPrefix + nodeName,
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + types.GatewayLBUDP + "=" + types.GWRouterPrefix + nodeName,
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + types.GatewayLBSCTP + "=" + types.GWRouterPrefix + nodeName,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + types.GatewayLBTCP + "=" + types.GWRouterPrefix + nodeName + "_local",
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + types.GatewayLBUDP + "=" + types.GWRouterPrefix + nodeName + "_local",
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + types.GatewayLBSCTP + "=" + types.GWRouterPrefix + nodeName + "_local",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + types.WorkerLBTCP + "=" + nodeName,
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + types.WorkerLBUDP + "=" + nodeName,
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + types.WorkerLBSCTP + "=" + nodeName,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 create load_balancer external_ids:" + types.GatewayLBTCP + "=" + types.GWRouterPrefix + nodeName + " protocol=tcp",
		Output: tcpLBUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 create load_balancer external_ids:" + types.GatewayLBTCP + "=" + types.GWRouterPrefix + nodeName + "_local" + " protocol=tcp options:skip_snat=true",
		Output: tcpLBUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 create load_balancer external_ids:" + types.GatewayLBUDP + "=" + types.GWRouterPrefix + nodeName + " protocol=udp",
		Output: udpLBUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 create load_balancer external_ids:" + types.GatewayLBUDP + "=" + types.GWRouterPrefix + nodeName + "_local" + " protocol=udp options:skip_snat=true",
		Output: udpLBUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 create load_balancer external_ids:" + types.GatewayLBSCTP + "=" + types.GWRouterPrefix + nodeName + " protocol=sctp",
		Output: sctpLBUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 create load_balancer external_ids:" + types.GatewayLBSCTP + "=" + types.GWRouterPrefix + nodeName + "_local" + " protocol=sctp options:skip_snat=true",
		Output: sctpLBUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 set logical_router " + types.GWRouterPrefix + nodeName + " load_balancer=" + tcpLBUUID +
			"," + udpLBUUID + "," + tcpLBUUID + "," + udpLBUUID + "," + sctpLBUUID + "," + sctpLBUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 create load_balancer external_ids:" + types.WorkerLBTCP + "=" + nodeName + " protocol=tcp",
		Output: tcpLBUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 create load_balancer external_ids:" + types.WorkerLBUDP + "=" + nodeName + " protocol=udp",
		Output: udpLBUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 create load_balancer external_ids:" + types.WorkerLBSCTP + "=" + nodeName + " protocol=sctp",
		Output: sctpLBUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 get logical_switch " + nodeName + " load_balancer",
		"ovn-nbctl --timeout=15 ls-lb-add " + nodeName + " " + tcpLBUUID,
		"ovn-nbctl --timeout=15 ls-lb-add " + nodeName + " " + udpLBUUID,
		"ovn-nbctl --timeout=15 ls-lb-add " + nodeName + " " + sctpLBUUID,
	})
}
*/

func addNodeLogicalFlows(testData []libovsdbtest.TestData, expectedOVNClusterRouter *nbdb.LogicalRouter,
	expectedNodeSwitch *nbdb.LogicalSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup *nbdb.PortGroup,
	node *tNode) []libovsdbtest.TestData {
	return addNodeLogicalFlowsHelper(testData, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup,
		expectedClusterPortGroup, node, false)
}

// expectedLocalTransitResources returns the default-network interconnect
// resources that ovnkube-controller creates while syncing the local node.
func expectedLocalTransitResources(clusterRouter *nbdb.LogicalRouter) []libovsdbtest.TestData {
	transitRouterPort := &nbdb.LogicalRouterPort{
		UUID:     "rtots-node1-UUID",
		Name:     "rtots-node1",
		MAC:      "0a:58:64:58:00:02",
		Networks: []string{"100.88.0.2/16"},
		Options:  map[string]string{"mcast_flood": "true"},
	}
	transitSwitchPort := &nbdb.LogicalSwitchPort{
		UUID:      "tstor-node1-UUID",
		Name:      "tstor-node1",
		Type:      "router",
		Addresses: []string{"router"},
		ExternalIDs: map[string]string{
			"node": "node1",
		},
		Options: map[string]string{
			"requested-tnl-key": "2",
			"router-port":       "rtots-node1",
		},
	}
	transitSwitch := expectedTransitSwitch()
	transitSwitch.Ports = []string{transitSwitchPort.UUID}
	clusterRouter.Ports = append(clusterRouter.Ports, transitRouterPort.UUID)
	return []libovsdbtest.TestData{transitSwitch, transitRouterPort, transitSwitchPort}
}

func expectedTransitSwitch() *nbdb.LogicalSwitch {
	return &nbdb.LogicalSwitch{
		UUID: "transit_switch-UUID",
		Name: types.TransitSwitch,
		OtherConfig: map[string]string{
			"interconn-ts":              types.TransitSwitch,
			libovsdbops.RequestedTnlKey: "16711683", // BaseTransitSwitchTunnelKey + default network ID (0)
			"mcast_snoop":               "true",
			"mcast_querier":             "false",
			"mcast_flood_unregistered":  "true",
		},
	}
}

func addNodeLogicalFlowsWithServiceController(testData []libovsdbtest.TestData, expectedOVNClusterRouter *nbdb.LogicalRouter,
	expectedNodeSwitch *nbdb.LogicalSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup *nbdb.PortGroup,
	node *tNode, svcTemplateSupport bool) []libovsdbtest.TestData {
	return addNodeLogicalFlowsHelper(testData, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup,
		expectedClusterPortGroup, node, svcTemplateSupport)
}

func addNodeLogicalFlowsHelper(testData []libovsdbtest.TestData, expectedOVNClusterRouter *nbdb.LogicalRouter,
	expectedNodeSwitch *nbdb.LogicalSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup *nbdb.PortGroup,
	node *tNode, serviceControllerEnabled bool) []libovsdbtest.TestData {

	lrpName := types.RouterToSwitchPrefix + node.Name
	chassisName := node.SystemID
	testData = append(testData, &nbdb.GatewayChassis{
		UUID:        chassisName + "-UUID",
		ChassisName: chassisName,
		Name:        lrpName + "-" + chassisName,
		Priority:    1,
	})
	testData = append(testData, &nbdb.LogicalRouterPort{
		Name:           lrpName,
		UUID:           types.RouterToSwitchPrefix + node.Name + "-UUID",
		MAC:            node.NodeLRPMAC,
		Networks:       []string{node.NodeGWIP},
		GatewayChassis: []string{chassisName + "-UUID"},
		Options: map[string]string{
			libovsdbops.GatewayMTU: "1400",
		},
	})
	if serviceControllerEnabled {
		testData = append(testData, &nbdb.ChassisTemplateVar{
			UUID:    chassisName + "ChassisTemplateVar-UUID",
			Chassis: chassisName,
			Variables: map[string]string{
				"NODEIP_IPv4_0": node.NodeIP,
			},
		})
	}

	expectedOVNClusterRouter.Ports = append(expectedOVNClusterRouter.Ports, types.RouterToSwitchPrefix+node.Name+"-UUID")

	testData = append(testData, &nbdb.LogicalSwitchPort{
		Name: types.SwitchToRouterPrefix + node.Name,
		UUID: types.SwitchToRouterPrefix + node.Name + "-UUID",
		Type: "router",
		Options: map[string]string{
			libovsdbops.RouterPort: types.RouterToSwitchPrefix + node.Name,
			"arp_proxy":            kubevirt.ComposeARPProxyLSPOption(),
		},
		Addresses: []string{"router"},
	})
	expectedNodeSwitch.Ports = append(expectedNodeSwitch.Ports, types.SwitchToRouterPrefix+node.Name+"-UUID")

	testData = append(testData, &nbdb.LogicalSwitchPort{
		Name:      types.K8sPrefix + node.Name,
		UUID:      types.K8sPrefix + node.Name + "-UUID",
		Type:      "",
		Options:   nil,
		Addresses: []string{node.NodeMgmtPortMAC + " " + node.NodeMgmtPortIP},
	})
	expectedNodeSwitch.Ports = append(expectedNodeSwitch.Ports, types.K8sPrefix+node.Name+"-UUID")
	expectedClusterPortGroup.Ports = []string{types.K8sPrefix + node.Name + "-UUID"}

	matchStr1 := fmt.Sprintf(`inport == "rtos-%s" && ip4.dst == %s /* %s */`, node.Name, node.GatewayRouterIP, node.Name)
	matchStr2 := fmt.Sprintf(`inport == "rtos-%s" && ip4.dst == %s /* %s */`, node.Name, node.NodeIP, node.Name)
	intPriority, _ := strconv.Atoi(types.NodeSubnetPolicyPriority)
	testData = append(testData, &nbdb.LogicalRouterPolicy{
		UUID:     "policy-based-route-1-UUID",
		Action:   nbdb.LogicalRouterPolicyActionReroute,
		Match:    matchStr1,
		Nexthops: []string{node.NodeMgmtPortIP},
		Priority: intPriority,
	})
	expectedOVNClusterRouter.Policies = append(expectedOVNClusterRouter.Policies, "policy-based-route-1-UUID")

	if config.Gateway.Mode == config.GatewayModeShared {
		testData = append(testData, &nbdb.LogicalRouterPolicy{
			UUID:     "policy-based-route-2-UUID",
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			Match:    matchStr2,
			Nexthops: []string{node.NodeMgmtPortIP},
			Priority: intPriority,
		})
		expectedOVNClusterRouter.Policies = append(expectedOVNClusterRouter.Policies, "policy-based-route-2-UUID")

	}
	testData = append(testData, expectedClusterPortGroup)
	testData = append(testData, expectedClusterRouterPortGroup)
	return testData
}

func startFakeController(oc *DefaultNetworkController, wg *sync.WaitGroup) []*net.IPNet {
	var clusterSubnets []*net.IPNet
	for _, clusterEntry := range config.Default.ClusterSubnets {
		clusterSubnets = append(clusterSubnets, clusterEntry.CIDR)
	}

	startDefaultNodeController(oc)
	gomega.Expect(oc.StartServiceController(wg, false)).To(gomega.Succeed())

	return clusterSubnets
}

func startDefaultNodeController(oc *DefaultNetworkController) {
	gomega.Expect(oc.startNodeReconciliation()).To(gomega.Succeed())
	ginkgo.DeferCleanup(func() {
		if oc.nodeReconciler != nil {
			oc.nodeReconciler.Stop()
		}
	})
}

func expectRecordedClusterSNATInsertWithExemption(recordingClient *libovsdbtest.RecordingClient, logicalIP, exemptedExtIP string) {
	var clusterSNATInsertOps []ovsdb.Operation
	for _, txn := range recordingClient.Transactions {
		for _, op := range txn {
			if op.Op == ovsdb.OperationInsert && op.Table == "NAT" &&
				op.Row["type"] == nbdb.NATTypeSNAT && op.Row["logical_ip"] == logicalIP {
				clusterSNATInsertOps = append(clusterSNATInsertOps, op)
			}
		}
	}
	gomega.Expect(clusterSNATInsertOps).To(gomega.HaveLen(1))
	gomega.Expect(clusterSNATInsertOps[0].Row).To(gomega.HaveKey("exempted_ext_ips"))
	exemptedExtIPs, ok := clusterSNATInsertOps[0].Row["exempted_ext_ips"].(ovsdb.OvsSet)
	gomega.Expect(ok).To(gomega.BeTrue())
	gomega.Expect(exemptedExtIPs.GoSet).To(gomega.ConsistOf(ovsdb.UUID{GoUUID: exemptedExtIP}))
}

func expectRouterClusterSNATWithExemption(routerNATs []*nbdb.NAT, logicalIP, exemptedExtIP string) {
	var clusterSNATs []*nbdb.NAT
	for _, nat := range routerNATs {
		if nat.Type == nbdb.NATTypeSNAT && nat.LogicalIP == logicalIP {
			clusterSNATs = append(clusterSNATs, nat)
		}
	}
	gomega.Expect(clusterSNATs).To(gomega.HaveLen(1))
	gomega.Expect(clusterSNATs[0].ExemptedExtIPs).NotTo(gomega.BeNil())
	gomega.Expect(*clusterSNATs[0].ExemptedExtIPs).To(gomega.Equal(exemptedExtIP))
}

var _ = ginkgo.Describe("Default network controller operations", func() {
	var (
		app      *cli.App
		f        *factory.WatchFactory
		stopChan chan struct{}
		wg       *sync.WaitGroup

		nbClient        libovsdbclient.Client
		sbClient        libovsdbclient.Client
		libovsdbCleanup *libovsdbtest.Context

		dbSetup        libovsdbtest.TestSetup
		node1          tNode
		testNode       corev1.Node
		fakeClient     *util.OVNKubeControllerClientset
		kubeFakeClient *fake.Clientset
		oc             *DefaultNetworkController
		nodeAnnotator  kube.Annotator
		events         []string
		eventsLock     sync.Mutex
		recorder       *record.FakeRecorder

		expectedClusterLBGroup         *nbdb.LoadBalancerGroup
		expectedSwitchLBGroup          *nbdb.LoadBalancerGroup
		expectedRouterLBGroup          *nbdb.LoadBalancerGroup
		expectedNodeSwitch             *nbdb.LogicalSwitch
		expectedOVNClusterRouter       *nbdb.LogicalRouter
		expectedClusterRouterPortGroup *nbdb.PortGroup
		expectedClusterPortGroup       *nbdb.PortGroup
		expectedNBDatabaseState        []libovsdbtest.TestData
		l3GatewayConfig                *util.L3GatewayConfig
		nodeHostCIDRs                  sets.Set[string]
	)

	const (
		clusterIPNet         string = "10.1.0.0"
		clusterCIDR          string = clusterIPNet + "/16"
		clusterv6CIDR        string = "aef0::/48"
		vlanID                      = 1024
		Node1GatewayRouterIP        = "172.16.16.2"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.OVNKubernetesFeature.EnableEgressIP = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}
		events = make([]string, 0)

		libovsdbCleanup = nil

		node1 = tNode{
			Name:                 "node1",
			NodeIP:               "1.2.3.4",
			NodeLRPMAC:           "0a:58:0a:01:01:01",
			LrpIP:                "100.64.0.2",
			LrpIPv6:              "fd98::2",
			DrLrpIP:              "100.64.0.1",
			PhysicalBridgeMAC:    "11:22:33:44:55:66",
			SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
			NodeSubnet:           "10.1.1.0/24",
			GWRouter:             types.GWRouterPrefix + "node1",
			GatewayRouterIPMask:  Node1GatewayRouterIP + "/24",
			GatewayRouterIP:      Node1GatewayRouterIP,
			GatewayRouterNextHop: "172.16.16.1",
			PhysicalBridgeName:   "br-eth0",
			NodeGWIP:             "10.1.1.1/24",
			NodeMgmtPortIP:       "10.1.1.2",
			NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
			DnatSnatIP:           "169.254.0.1",
		}
		_, clusterIPNet, err := net.ParseCIDR(clusterCIDR)
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{
			CIDR:             clusterIPNet,
			HostSubnetLength: 24,
		}}

		expectedClusterLBGroup = newLoadBalancerGroup(types.ClusterLBGroupName)
		expectedSwitchLBGroup = newLoadBalancerGroup(types.ClusterSwitchLBGroupName)
		expectedRouterLBGroup = newLoadBalancerGroup(types.ClusterRouterLBGroupName)
		expectedNodeSwitch = node1.logicalSwitch([]string{expectedClusterLBGroup.UUID, expectedSwitchLBGroup.UUID})
		expectedOVNClusterRouter = newOVNClusterRouter()
		expectedClusterRouterPortGroup = newRouterPortGroup()
		expectedClusterPortGroup = newClusterPortGroup()

		dbSetup = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				newClusterJoinSwitch(),
				expectedNodeSwitch.DeepCopy(),
				newOVNClusterRouter(),
				newRouterPortGroup(),
				newClusterPortGroup(),
				expectedClusterLBGroup.DeepCopy(),
				expectedSwitchLBGroup.DeepCopy(),
				expectedRouterLBGroup.DeepCopy(),
			},
		}
		testNode = node1.k8sNode("2")

		kubeFakeClient = fake.NewSimpleClientset(&corev1.NodeList{
			Items: []corev1.Node{testNode},
		})
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		egressIPFakeClient := &egressipfake.Clientset{}
		egressQoSFakeClient := &egressqosfake.Clientset{}
		egressServiceFakeClient := &egressservicefake.Clientset{}
		fakeClient = &util.OVNKubeControllerClientset{
			KubeClient:           kubeFakeClient,
			EgressIPClient:       egressIPFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
			EgressQoSClient:      egressQoSFakeClient,
			EgressServiceClient:  egressServiceFakeClient,
		}

		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeAnnotator = kube.NewNodeAnnotator(&kube.Kube{KClient: kubeFakeClient}, testNode.Name)
		l3GatewayConfig = node1.gatewayConfig(config.GatewayModeLocal, uint(vlanID))
		err = util.SetL3GatewayConfig(nodeAnnotator, l3GatewayConfig)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		if config.OVNKubernetesFeature.EnableEgressIP {
			egressNodeIPsASv4, egressNodeIPsASv6 := buildEgressIPNodeAddressSets([]string{node1.NodeIP})
			if config.IPv4Mode {
				dbSetup.NBData = append(dbSetup.NBData, egressNodeIPsASv4)
			}
			if config.IPv6Mode {
				dbSetup.NBData = append(dbSetup.NBData, egressNodeIPsASv6)
			}
		}
		err = util.UpdateNodeManagementPortMACAddresses(&testNode, nodeAnnotator,
			ovntest.MustParseMAC(node1.NodeMgmtPortMAC), types.DefaultNetworkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeHostCIDRs = sets.New(fmt.Sprintf("%s/24", node1.NodeIP))
		err = util.SetNodeHostCIDRs(nodeAnnotator, nodeHostCIDRs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = nodeAnnotator.Run()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		nbClient, sbClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		f, err = factory.NewOVNKubeControllerWatchFactory(fakeClient, "test-node")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = f.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		recorder = record.NewFakeRecorder(10)
		netManager := networkmanager.Default()
		addressSetManager := addresssetmanager.NewAddressSetManager(f.PodCoreInformer(), f.NamespaceInformer(),
			f.NodeCoreInformer(), nbClient, netManager.Interface().GetNetworkNameForNADKey)
		oc, err = NewOvnController(
			fakeClient,
			f,
			stopChan,
			nil,
			netManager.Interface(),
			nbClient,
			sbClient,
			recorder,
			wg,
			nil,
			NewPortCache(stopChan),
			addressSetManager,
			node1.Name,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(oc).NotTo(gomega.BeNil())

		setupCOPP := true
		setupClusterController(oc, setupCOPP)

		// record events for testcases to check
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				event, ok := <-recorder.Events
				if !ok {
					return
				}
				eventsLock.Lock()
				events = append(events, event)
				eventsLock.Unlock()
			}
		}()

	})

	ginkgo.AfterEach(func() {
		libovsdbCleanup.Cleanup()
		if oc != nil && oc.nodeReconciler != nil {
			oc.nodeReconciler.Stop()
		}
		close(stopChan)
		f.Shutdown()
		close(recorder.Events)
		wg.Wait()
	})

	ginkgo.It("sets up a local gateway", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			clusterSubnets := startFakeController(oc, wg)

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)

			// Get node with the latest annotations set
			testNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = oc.syncNodeGateway(testNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			skipSnat := false
			expectedNBDatabaseState = addNodeLogicalFlowsWithServiceController(nil, expectedOVNClusterRouter, expectedNodeSwitch,
				expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1, oc.svcTemplateSupport)
			expectedNBDatabaseState = generateGatewayInitExpectedNB(expectedNBDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)},
				skipSnat, node1.NodeMgmtPortIP, "1400", node1.NodeIP)
			expectedNBDatabaseState = append(expectedNBDatabaseState, expectedLocalTransitResources(expectedOVNClusterRouter)...)
			gomega.Eventually(oc.nbClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--gateway-local",
			"--nodeport",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("clears stale ovn_cluster_router routes in local gw", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			clusterSubnets := startFakeController(oc, wg)

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			// add stale route
			badRoute := &nbdb.LogicalRouterStaticRoute{
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				IPPrefix: subnet.String(),
				Nexthop:  "10.244.0.6",
			}
			ginkgo.By("Creating stale route")
			p := func(*nbdb.LogicalRouterStaticRoute) bool { return false }
			err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(nbClient,
				types.OVNClusterRouter, badRoute, p)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.By("Syncing node with OVNK")
			node, err := oc.kube.GetNodeForWindows(testNode.Name)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = oc.syncNodeManagementPortDefault(node, node.Name, []*net.IPNet{subnet})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = oc.syncNodeGateway(node)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.By("Stale route should have been removed")

			skipSnat := false
			expectedNBDatabaseState = addNodeLogicalFlowsWithServiceController(nil, expectedOVNClusterRouter, expectedNodeSwitch,
				expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1, oc.svcTemplateSupport)
			expectedNBDatabaseState = generateGatewayInitExpectedNB(expectedNBDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)},
				skipSnat, node1.NodeMgmtPortIP, "1400", node1.NodeIP)
			expectedNBDatabaseState = append(expectedNBDatabaseState, expectedLocalTransitResources(expectedOVNClusterRouter)...)
			gomega.Eventually(oc.nbClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--gateway-local",
			"--nodeport",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("sets up a shared gateway", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			clusterSubnets := startFakeController(oc, wg)

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			// Get node with the latest annotations set
			testNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = oc.syncNodeGateway(testNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			skipSnat := false
			expectedNBDatabaseState = addNodeLogicalFlowsWithServiceController(nil, expectedOVNClusterRouter, expectedNodeSwitch,
				expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1, oc.svcTemplateSupport)
			expectedNBDatabaseState = generateGatewayInitExpectedNB(expectedNBDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)},
				skipSnat, node1.NodeMgmtPortIP, "1400", node1.NodeIP)
			expectedNBDatabaseState = append(expectedNBDatabaseState, expectedLocalTransitResources(expectedOVNClusterRouter)...)
			gomega.Eventually(oc.nbClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--nodeport",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("syncs dual-stack no-overlay SNAT exemption address set before gateway NATs", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.Default.Transport = types.NetworkTransportNoOverlay
			config.NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled
			oc.addressSetFactory = addressset.NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)
			oc.ovnClusterLRPToJoinIfAddrs, err = oc.getOVNClusterRouterPortToJoinSwitchIfAddrs()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// a no-overlay network is always advertised: create the global
			// advertised-network isolation resources that node sync relies on
			gomega.Expect(ConfigureAdvertisedNetworkIsolation(nbClient)).To(gomega.Succeed())

			dualStackGatewayConfig := node1.gatewayConfig(config.GatewayModeLocal, uint(vlanID))
			dualStackGatewayConfig.IPAddresses = ovntest.MustParseIPNets(node1.GatewayRouterIPMask, "fd99::2/64")
			dualStackGatewayConfig.NextHops = ovntest.MustParseIPs(node1.GatewayRouterNextHop, "fd99::1")
			gomega.Expect(util.SetL3GatewayConfig(nodeAnnotator, dualStackGatewayConfig)).To(gomega.Succeed())
			gomega.Expect(util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet, "aef0:0:0:1::/64"))).To(gomega.Succeed())
			gomega.Expect(util.SetNodeHostCIDRs(nodeAnnotator, sets.New(fmt.Sprintf("%s/24", node1.NodeIP), "fd99::2/64"))).To(gomega.Succeed())
			gomega.Expect(nodeAnnotator.Run()).To(gomega.Succeed())

			recordingClient := libovsdbtest.NewRecordingClient(nbClient)
			oc.nbClient = recordingClient
			node, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = oc.addUpdateLocalNodeEvent(node, &nodeSyncs{
				syncNode:              true,
				syncClusterRouterPort: true,
				syncMgmtPort:          true,
				syncGw:                true,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			as, err := getNoOverlaySNATExemptionAddressSet(oc.addressSetFactory, oc.GetNetInfo(), oc.controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(as).NotTo(gomega.BeNil())
			v4UUID, v6UUID := as.GetASUUID()
			gomega.Expect(v4UUID).NotTo(gomega.BeEmpty())
			gomega.Expect(v6UUID).NotTo(gomega.BeEmpty())

			expectRecordedClusterSNATInsertWithExemption(recordingClient, clusterCIDR, v4UUID)
			expectRecordedClusterSNATInsertWithExemption(recordingClient, clusterv6CIDR, v6UUID)

			routerNATs, err := libovsdbops.GetRouterNATs(nbClient, &nbdb.LogicalRouter{Name: types.GWRouterPrefix + node1.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectRouterClusterSNATWithExemption(routerNATs, clusterCIDR, v4UUID)
			expectRouterClusterSNATWithExemption(routerNATs, clusterv6CIDR, v6UUID)
			_, failed := oc.gatewaysFailed.Load(testNode.Name)
			gomega.Expect(failed).To(gomega.BeFalse())

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR + "," + clusterv6CIDR,
			"-k8s-service-cidr=10.96.0.0/16,fd00:10:96::/112",
			"--init-gateways",
			"--nodeport",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	extraNats := []*nbdb.NAT{
		newNodeSNAT("stale-nodeNAT-UUID-1", "10.1.0.3", Node1GatewayRouterIP),
		newNodeSNAT("stale-nodeNAT-UUID-2", "10.2.0.3", Node1GatewayRouterIP),
		newNodeSNAT("stale-nodeNAT-UUID-3", "10.0.0.3", Node1GatewayRouterIP),
		newNodeSNAT("stale-nodeNAT-UUID-4", "10.0.0.3", "172.16.16.3"),
	}
	extraNatsWithMatch := []*nbdb.NAT{ // used for pod network advertised test
		newNodeSNATWithMatch("stale-nodeNAT-UUID-1", "10.1.0.3", Node1GatewayRouterIP, "ip4.dst == $a1042611113178530741"),
		newNodeSNATWithMatch("stale-nodeNAT-UUID-2", "10.2.0.3", Node1GatewayRouterIP, "ip4.dst == $a1042611113178530741"),
		newNodeSNATWithMatch("stale-nodeNAT-UUID-3", "10.0.0.3", Node1GatewayRouterIP, "ip4.dst == $a1042611113178530741"),
		newNodeSNATWithMatch("stale-nodeNAT-UUID-4", "10.0.0.3", "172.16.16.3", "ip4.dst == $a1042611113178530741"),
	}
	ginkgo.DescribeTable(
		"reconciles pod network SNATs from syncGateway",
		func(condition func(*DefaultNetworkController) error, expectedExtraNATs ...*nbdb.NAT) {
			config.OVNKubernetesFeature.AdvertisedUDNIsolationMode = config.AdvertisedUDNIsolationModeStrict
			app.Action = func(ctx *cli.Context) error {
				// Initialize config from CLI flags (including --init-gateways)
				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// create a pod on this node
				ns := ovntest.NewNamespace("namespace-1")
				pod := *ovntest.NewPodWithLabels(ns.Name, podName, node1.Name, "10.0.0.3", egressPodLabel)
				_, err = fakeClient.KubeClient.CoreV1().Pods(ns.Name).Create(context.TODO(), &pod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// generate specific test conditions (after base config is set)
				err = condition(oc)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				if oc.isPodNetworkAdvertisedAtNode(node1.Name) {
					gomega.Expect(ConfigureAdvertisedNetworkIsolation(nbClient)).To(gomega.Succeed())
				}
				startDefaultNodeController(oc)
				gomega.Eventually(func() error {
					_, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + node1.Name,
					})
					return err
				}).Should(gomega.Succeed())

				// add stale SNATs from pods to nodes on wrong node
				GR := &nbdb.LogicalRouter{
					Name: types.GWRouterPrefix + node1.Name,
				}
				if !oc.isPodNetworkAdvertisedAtNode(node1.Name) {
					err = libovsdbops.CreateOrUpdateNATs(nbClient, GR, extraNats...)
				} else {
					err = libovsdbops.CreateOrUpdateNATs(nbClient, GR, extraNatsWithMatch...)
				}
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// ensure the stale SNAT's are cleaned up
				gomega.Expect(oc.StartServiceController(wg, false)).To(gomega.Succeed())
				subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
				// Get node with the latest annotations set
				testNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = oc.syncNodeGateway(testNode)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				skipSnat := config.Gateway.DisableSNATMultipleGWs && !oc.GetNetInfo().IsPrimaryNetwork()
				var clusterSubnets []*net.IPNet
				for _, clusterSubnet := range config.Default.ClusterSubnets {
					clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
				}
				expectedNBDatabaseState = addNodeLogicalFlowsWithServiceController(nil, expectedOVNClusterRouter, expectedNodeSwitch,
					expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1, oc.svcTemplateSupport)
				if !oc.isPodNetworkAdvertisedAtNode(node1.Name) {
					expectedNBDatabaseState = generateGatewayInitExpectedNB(expectedNBDatabaseState, expectedOVNClusterRouter,
						expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig,
						[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)},
						skipSnat, node1.NodeMgmtPortIP, "1400", node1.NodeIP)
				} else {
					expectedNBDatabaseState = generateGatewayInitExpectedNBWithPodNetworkAdvertised(expectedNBDatabaseState, expectedOVNClusterRouter,
						expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig,
						[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)},
						skipSnat, node1.NodeMgmtPortIP, "1400", true, node1.NodeIP)
					addrSet, err := oc.addressSetFactory.GetAddressSet(GetAdvertisedNetworkSubnetsAddressSetDBIDs())
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					expectedNBDatabaseState = generateAdvertisedUDNIsolationExpectedNB(expectedNBDatabaseState, oc.GetNetworkName(), oc.GetNetworkID(), clusterSubnets, expectedNodeSwitch, addrSet)
				}
				GR = nil
				for _, testObj := range expectedNBDatabaseState {
					if router, ok := testObj.(*nbdb.LogicalRouter); ok && router.UUID == types.GWRouterPrefix+node1.Name+"-UUID" {
						GR = router
						break
					}
				}
				for _, expectedNat := range expectedExtraNATs {
					expectedNBDatabaseState = append(expectedNBDatabaseState, expectedNat)
					GR.Nat = append(GR.Nat, expectedNat.UUID)
				}
				expectedNBDatabaseState = append(expectedNBDatabaseState, expectedLocalTransitResources(expectedOVNClusterRouter)...)
				gomega.Eventually(oc.nbClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"--init-gateways",
				"--nodeport",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry(
			"When DisableSNATMultipleGWs is true",
			func(*DefaultNetworkController) error {
				config.Gateway.DisableSNATMultipleGWs = true
				return nil
			},
			newNodeSNAT("stale-nodeNAT-UUID-3", "10.0.0.3", Node1GatewayRouterIP), // won't be deleted since pod exists on this node
			newNodeSNAT("stale-nodeNAT-UUID-4", "10.0.0.3", "172.16.16.3"),        // won't be deleted on this node but will be deleted on the node whose IP is 172.16.16.3 since this pod belongs to this node
		),
		ginkgo.Entry(
			"When DisableSNATMultipleGWs is false",
			func(*DefaultNetworkController) error {
				config.Gateway.DisableSNATMultipleGWs = false
				return nil
			},
			newNodeSNAT("stale-nodeNAT-UUID-4", "10.0.0.3", "172.16.16.3"), // won't be deleted on this node but will be deleted on the node whose IP is 172.16.16.3 since this pod belongs to this node
		),
		ginkgo.Entry(
			"When pod network is advertised and DisableSNATMultipleGWs is true",
			func(oc *DefaultNetworkController) error {
				config.Gateway.DisableSNATMultipleGWs = true
				mutableNetInfo := util.NewMutableNetInfo(oc.GetNetInfo())
				mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{"node1": {"vrf"}})
				return oc.Reconcile(mutableNetInfo)
			},
			// won't be deleted on this node since this pod belongs to node-1 and is advertised so we keep this SNAT
			newNodeSNATWithMatch("stale-nodeNAT-UUID-3", "10.0.0.3", Node1GatewayRouterIP, "ip4.dst == $a1042611113178530741"),
			// won't be deleted on this node but will be deleted on the node whose IP is 172.16.16.3 since this pod belongs to node-1
			newNodeSNATWithMatch("stale-nodeNAT-UUID-4", "10.0.0.3", "172.16.16.3", "ip4.dst == $a1042611113178530741"),
		),
		ginkgo.Entry(
			"When pod network is advertised and DisableSNATMultipleGWs is false",
			func(oc *DefaultNetworkController) error {
				config.Gateway.DisableSNATMultipleGWs = false
				config.OVNKubernetesFeature.EnableEgressIP = true
				mutableNetInfo := util.NewMutableNetInfo(oc.GetNetInfo())
				mutableNetInfo.SetPodNetworkAdvertisedVRFs(map[string][]string{"node1": {"vrf"}})
				return oc.Reconcile(mutableNetInfo)
			},
			newNodeSNATWithMatch("stale-nodeNAT-UUID-4", "10.0.0.3", "172.16.16.3", "ip4.dst == $a1042611113178530741"), // won't be deleted on this node but will be deleted on the node whose IP is 172.16.16.3 since this pod belongs to this node
		),
	)

	ginkgo.It("does not list node's pods when updating node after successfully adding the node", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			clusterSubnets := startFakeController(oc, wg)

			skipSnat := false
			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			expectedNBDatabaseState = addNodeLogicalFlowsWithServiceController(nil, expectedOVNClusterRouter, expectedNodeSwitch,
				expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1, oc.svcTemplateSupport)
			expectedNBDatabaseState = generateGatewayInitExpectedNB(expectedNBDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)},
				skipSnat, node1.NodeMgmtPortIP, "1400", node1.NodeIP)
			expectedNBDatabaseState = append(expectedNBDatabaseState, expectedLocalTransitResources(expectedOVNClusterRouter)...)
			gomega.Eventually(oc.nbClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))

			ginkgo.By("modifying the node and triggering an update")

			var podsWereListed uint32
			kubeFakeClient.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
				atomic.StoreUint32(&podsWereListed, 1)
				podList := &corev1.PodList{}
				return true, podList, nil
			})

			// modify the node and trigger an update
			err = nodeAnnotator.Set("foobar", "baz")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Consistently(func() uint32 {
				return atomic.LoadUint32(&podsWereListed)
			}, 10).Should(gomega.Equal(uint32(0)))

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--nodeport",
			"--disable-snat-multiple-gws=false",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("use node retry with deleting a node", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			startFakeController(oc, wg)

			// Get node with the latest annotations set
			testNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = oc.syncNodeGateway(testNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// inject transient problem, nbdb is down
			oc.nbClient.Close()
			gomega.Eventually(func() bool {
				return oc.nbClient.Connected()
			}).Should(gomega.BeFalse())

			// Node delete will fail with Failed to delete node node1,
			err = fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), testNode.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// The retry framework serializes work per object: if an add/update
			// reconciliation for the node was already in flight when nbdb went
			// down, it blocks for up to one transaction timeout before the
			// delete can even start, and the delete then needs up to one more
			// transaction timeout to fail. Allow for both plus scheduling.
			gomega.Eventually(func() []string {
				eventsLock.Lock()
				defer eventsLock.Unlock()
				eventsCopy := make([]string, 0, len(events))
				eventsCopy = append(eventsCopy, events...)
				return eventsCopy
			}, 3*config.Default.OVSDBTxnTimeout).Should(gomega.ContainElement(
				gomega.ContainSubstring("error deleting node node1 logical network"),
			))

			ginkgo.By("node resources should still exist while delete reconciliation is failing")
			_, err = libovsdbops.GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: node1.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// reconnect nbdb
			connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
			defer cancel()
			resetNBClient(connCtx, oc.nbClient)
			oc.nodeReconciler.ReconcileNetwork(node1.Name, oc.GetNetworkName())
			gomega.Eventually(func() bool {
				_, err := libovsdbops.GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: node1.Name})
				return errors.Is(err, libovsdbclient.ErrNotFound)
			}).Should(gomega.BeTrue())
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--nodeport",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.DescribeTable("doesn't retry deleting the local node that is missing annotation",
		func(missingAnnotation string) {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				localNode, err := kubeFakeClient.CoreV1().Nodes().Get(context.TODO(), node1.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				delete(localNode.Annotations, missingAnnotation)
				_, err = kubeFakeClient.CoreV1().Nodes().Update(context.TODO(), localNode, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() bool {
					cachedNode, err := f.GetNode(localNode.Name)
					if err != nil {
						return false
					}
					_, found := cachedNode.Annotations[missingAnnotation]
					return !found
				}).Should(gomega.BeTrue())

				startFakeController(oc, wg)
				ginkgo.By("ensuring reconciliation of the partially annotated local node fails")
				failureRecorded := func() bool {
					if missingAnnotation == types.NodeSubnetsAnnotation {
						_, failed := oc.addNodeFailed.Load(localNode.Name)
						return failed
					}
					_, failed := oc.syncZoneICFailed.Load(localNode.Name)
					return failed
				}
				gomega.Eventually(failureRecorded).Should(gomega.BeTrue())

				ginkgo.By("deleting the local node and ensuring its failed state is cleared")
				err = kubeFakeClient.CoreV1().Nodes().Delete(context.TODO(), localNode.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(failureRecorded).Should(gomega.BeFalse())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry(types.NodeSubnetsAnnotation, types.NodeSubnetsAnnotation),
		ginkgo.Entry(ovnNodeID, ovnNodeID),
	)

	ginkgo.It("delete a partially constructed node", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			startFakeController(oc, wg)

			testNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = oc.syncNodeGateway(testNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Delete the node's gateway Logical Router Port to force node delete to handle a
			// partially removed OVN DB
			gatewayRouter := types.GWRouterPrefix + node1.Name
			lr := &nbdb.LogicalRouter{Name: gatewayRouter}
			lrp := &nbdb.LogicalRouterPort{Name: types.GWRouterToJoinSwitchPrefix + gatewayRouter}
			lrp, err = libovsdbops.GetLogicalRouterPort(nbClient, lrp)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = libovsdbops.DeleteLogicalRouterPorts(nbClient, lr, lrp)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = libovsdbops.GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: node1.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			externalSwitch := types.ExternalSwitchPrefix + node1.Name
			_, err = libovsdbops.GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: externalSwitch})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Node delete should not fail
			err = fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), testNode.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() bool {
				_, err := libovsdbops.GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: node1.Name})
				return errors.Is(err, libovsdbclient.ErrNotFound)
			}, 10).Should(gomega.BeTrue())

			gomega.Eventually(func() bool {
				_, err := libovsdbops.GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: externalSwitch})
				return errors.Is(err, libovsdbclient.ErrNotFound)
			}, 10).Should(gomega.BeTrue())

			gomega.Eventually(func() bool {
				_, err = libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: gatewayRouter})
				return errors.Is(err, libovsdbclient.ErrNotFound)
			}, 10).Should(gomega.BeTrue())

			gomega.Eventually(func() bool {
				_, failed := oc.addNodeFailed.Load(testNode.Name)
				return failed
			}).Should(gomega.BeFalse())
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--nodeport",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("use node retry for an unmanaged hybrid overlay node node", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// Don't create clusterManager so that the node has not subnets allocated.
			config.HybridOverlay.Enabled = true

			config.Kubernetes.NoHostSubnetNodes, err = metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
				MatchLabels: nodeNoHostSubnetAnnotation(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			atomic.StoreUint32(&oc.allInitialPodsProcessed, 1)

			badNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			badNode.Annotations = map[string]string{
				util.OvnNodeID: "3",
			}
			_, err = fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), badNode, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// The node controller reads from the informer cache. Wait for it to
			// observe the intentionally incomplete annotations before starting it.
			gomega.Eventually(func() bool {
				cachedNode, err := f.GetNode(badNode.Name)
				if err != nil {
					return false
				}
				_, found := cachedNode.Annotations[types.NodeSubnetsAnnotation]
				return !found
			}).Should(gomega.BeTrue())

			startDefaultNodeController(oc)

			gomega.Eventually(func() bool {
				_, failed := oc.addNodeFailed.Load(badNode.Name)
				return failed
			}).Should(gomega.BeTrue())

			ginkgo.By("labeling the node to a hybrid overlay node")
			badNode, err = fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), badNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			badNode.Labels = nodeNoHostSubnetAnnotation()
			badNode.Annotations[hotypes.HybridOverlayNodeSubnet] = "11.1.0.0/24"
			_, err = fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), badNode, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			oc.nodeReconciler.ReconcileNetwork(badNode.Name, oc.GetNetworkName())

			ginkgo.By("adding the node becomes possible")
			gomega.Eventually(func() bool {
				return oc.lsManager.IsNonHostSubnetSwitch(badNode.Name)
			}).Should(gomega.BeTrue())

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--nodeport",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("reconciles node host subnets after dual-stack to single-stack downgrade", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Expect(nodeAnnotator.Set(util.OVNNodeEncapIPs, []string{node1.NodeIP})).To(gomega.Succeed())
			gomega.Expect(nodeAnnotator.Set(util.OvnTransitSwitchPortAddr,
				map[string]string{"ipv4": "100.88.0.2/16"})).To(gomega.Succeed())
			gomega.Expect(nodeAnnotator.Run()).To(gomega.Succeed())

			startFakeController(oc, wg)

			nodeSubnet := "10.1.1.0/24"
			localNode, err := kubeFakeClient.CoreV1().Nodes().Get(context.TODO(), node1.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			localNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":[\"%s\", \"fd02:0:0:2::2895/64\"]}", nodeSubnet)
			_, err = kubeFakeClient.CoreV1().Nodes().Update(context.TODO(), localNode, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// check that a node event complaining about the mismatch between
			// node subnets and cluster subnets was posted
			gomega.Eventually(func() []string {
				eventsLock.Lock()
				defer eventsLock.Unlock()
				eventsCopy := make([]string, 0, len(events))
				eventsCopy = append(eventsCopy, events...)
				return eventsCopy
			}, 10).Should(gomega.ContainElement(gomega.ContainSubstring("failed to get expected host subnets for node node1; expected v4 true have true, expected v6 false have true")))

			// Simulate the ClusterManager reconciling the node annotations to single-stack
			localNode, err = kubeFakeClient.CoreV1().Nodes().Get(context.TODO(), localNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			localNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":[\"%s\"]}", nodeSubnet)
			_, err = kubeFakeClient.CoreV1().Nodes().Update(context.TODO(), localNode, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Ensure that the node's switch is eventually created once the annotations
			// are reconciled by the network cluster controller
			newNodeLS := &nbdb.LogicalSwitch{Name: localNode.Name}
			gomega.Eventually(func() error {
				_, err := libovsdbops.GetLogicalSwitch(nbClient, newNodeLS)
				return err
			}, 10).ShouldNot(gomega.HaveOccurred())

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--nodeport",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("reconciles options:gateway_mtu on the cluster router port when gateway MTU support changes", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			startFakeController(oc, wg)

			clusterRouterPortOptions := func() (map[string]string, error) {
				lrp, err := libovsdbops.GetLogicalRouterPort(nbClient,
					&nbdb.LogicalRouterPort{Name: types.RouterToSwitchPrefix + node1.Name})
				if err != nil {
					return nil, err
				}
				return lrp.Options, nil
			}

			ginkgo.By("setting options:gateway_mtu while the node reports gateway MTU support")
			gomega.Eventually(clusterRouterPortOptions, 10).
				Should(gomega.HaveKeyWithValue(libovsdbops.GatewayMTU, strconv.Itoa(config.Default.MTU)))

			ginkgo.By("annotating the node as not supporting gateway MTU")
			localNode, err := kubeFakeClient.CoreV1().Nodes().Get(context.TODO(), node1.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			localNode.Annotations[util.OvnNodeGatewayMtuSupport] = "false"
			_, err = kubeFakeClient.CoreV1().Nodes().Update(context.TODO(), localNode, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("removing options:gateway_mtu from the cluster router port")
			gomega.Eventually(clusterRouterPortOptions, 10).ShouldNot(gomega.HaveKey(libovsdbops.GatewayMTU))

			ginkgo.By("annotating the node as supporting gateway MTU again")
			localNode, err = kubeFakeClient.CoreV1().Nodes().Get(context.TODO(), node1.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			localNode.Annotations[util.OvnNodeGatewayMtuSupport] = "true"
			_, err = kubeFakeClient.CoreV1().Nodes().Update(context.TODO(), localNode, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("restoring options:gateway_mtu on the cluster router port")
			gomega.Eventually(clusterRouterPortOptions, 10).
				Should(gomega.HaveKeyWithValue(libovsdbops.GatewayMTU, strconv.Itoa(config.Default.MTU)))

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--nodeport",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("reconciles node host subnets after single-stack to dual-stack upgrade", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Expect(config.IPv4Mode).To(gomega.BeTrue())
			gomega.Expect(config.IPv6Mode).To(gomega.BeTrue())

			gomega.Expect(nodeAnnotator.Set(util.OVNNodeEncapIPs, []string{node1.NodeIP})).To(gomega.Succeed())
			gomega.Expect(nodeAnnotator.Set(util.OvnTransitSwitchPortAddr,
				map[string]string{"ipv4": "100.88.0.2/16", "ipv6": "fd97::2/64"})).To(gomega.Succeed())
			gomega.Expect(nodeAnnotator.Run()).To(gomega.Succeed())

			startFakeController(oc, wg)

			// Simulate the ClusterManager reconciling the node annotations to dual-stack
			nodeIpv4Subnet := "10.1.1.0/24"
			localNode, err := kubeFakeClient.CoreV1().Nodes().Get(context.TODO(), node1.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			newNodeIpv6SubnetPrefix := "aef0:0:0:2::"
			newNodeIpv6Subnet := newNodeIpv6SubnetPrefix + "2895/64"
			localNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", nodeIpv4Subnet, newNodeIpv6Subnet)
			localNode.Annotations["k8s.ovn.org/node-gateway-router-lrp-ifaddr"] = "{\"ipv4\":\"100.64.0.2/16\"}"
			_, err = kubeFakeClient.CoreV1().Nodes().Update(context.TODO(), localNode, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Ensure that the node's switch is eventually created once the annotations
			// are reconciled by the network cluster controller
			gomega.Eventually(func() bool {
				newNodeLS, err := libovsdbops.GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: localNode.Name})
				if err != nil {
					return false
				}
				if newNodeLS.OtherConfig["subnet"] != nodeIpv4Subnet {
					return false
				}
				if newNodeLS.OtherConfig["ipv6_prefix"] != newNodeIpv6SubnetPrefix {
					return false
				}
				return true
			}, 10).Should(gomega.BeTrue())

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR + "," + clusterv6CIDR,
			"-k8s-service-cidr=10.96.0.0/16,fd00:10:96::/112",
			"--init-gateways",
			"--nodeport",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("reconciles a bootstrap local node when only syncZoneICFailed is marked", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			oc.syncZoneICFailed.Store(testNode.Name, true)
			gomega.Expect(oc.lsManager.GetSwitchSubnets(testNode.Name)).To(gomega.BeNil())

			startDefaultNodeController(oc)

			gomega.Eventually(func() []*net.IPNet {
				return oc.lsManager.GetSwitchSubnets(testNode.Name)
			}).ShouldNot(gomega.BeNil())
			gomega.Eventually(func() error {
				_, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{
					Name: types.GWRouterPrefix + node1.Name,
				})
				return err
			}).Should(gomega.Succeed())

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--nodeport",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("retries gateway sync after NBDB outage on node update", func() {
		// Full failure+recovery path: NBDB outage during a node-update gateway
		// sync sets gatewaysFailed; once NBDB recovers, ReconcileNetwork drives
		// gateway sync to consistency and clears the marker.
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("seeding IC node annotations so syncZoneIC succeeds cleanly")
			gomega.Expect(nodeAnnotator.Set(util.OVNNodeEncapIPs, []string{node1.NodeIP})).To(gomega.Succeed())
			gomega.Expect(nodeAnnotator.Set(util.OvnTransitSwitchPortAddr,
				map[string]string{"ipv4": "100.88.0.2/16"})).To(gomega.Succeed())
			gomega.Expect(nodeAnnotator.Run()).To(gomega.Succeed())

			startFakeController(oc, wg)

			ginkgo.By("waiting for the initial bootstrap to converge")
			gomega.Eventually(func() error {
				_, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{
					Name: types.GWRouterPrefix + node1.Name,
				})
				return err
			}).Should(gomega.Succeed())
			gomega.Eventually(func() bool {
				_, failed := oc.gatewaysFailed.Load(node1.Name)
				return failed
			}).Should(gomega.BeFalse())

			ginkgo.By("Bringing down NBDB")
			oc.nbClient.Close()
			gomega.Eventually(func() bool {
				return oc.nbClient.Connected()
			}).Should(gomega.BeFalse())

			ginkgo.By("modifying the node and triggering an update")
			node1.GatewayRouterNextHop = "172.16.16.111"
			l3Config := node1.gatewayConfig(config.GatewayModeShared, uint(vlanID))
			gomega.Expect(util.SetL3GatewayConfig(nodeAnnotator, l3Config)).To(gomega.Succeed())
			gomega.Expect(nodeAnnotator.Run()).To(gomega.Succeed())

			ginkgo.By("waiting for the gateway sync to fail and set the marker")
			// as above, allow for a serialized in-flight reconciliation plus
			// the gateway sync attempt, each up to one transaction timeout
			gomega.Eventually(func() bool {
				_, failed := oc.gatewaysFailed.Load(node1.Name)
				return failed
			}, 3*config.Default.OVSDBTxnTimeout).Should(gomega.BeTrue())

			ginkgo.By("bringing NBDB back up")
			connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
			defer cancel()
			resetNBClient(connCtx, oc.nbClient)
			oc.nodeReconciler.ReconcileNetwork(node1.Name, oc.GetNetworkName())

			ginkgo.By("expecting the gateway-retry path to clear the marker and apply the new next-hop")
			gomega.Eventually(func() bool {
				_, failed := oc.gatewaysFailed.Load(node1.Name)
				return failed
			}, 10*time.Second).Should(gomega.BeFalse())
			const oldNextHop = "172.16.16.1"
			gomega.Eventually(func() (map[string]int, error) {
				routes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(nbClient,
					func(r *nbdb.LogicalRouterStaticRoute) bool {
						return r.Nexthop == node1.GatewayRouterNextHop || r.Nexthop == oldNextHop
					})
				if err != nil {
					return nil, err
				}
				counts := map[string]int{}
				for _, r := range routes {
					counts[r.Nexthop]++
				}
				return counts, nil
			}, 10*time.Second).Should(gomega.And(
				gomega.HaveKey(node1.GatewayRouterNextHop),
				gomega.Not(gomega.HaveKey(oldNextHop)),
			))

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--nodeport",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
})

func nodeNoHostSubnetAnnotation() map[string]string {
	return map[string]string{"leave-alone": "true"}
}

func classBIPAddress(desiredIPAddress string) *net.IPNet {
	return ovntest.MustParseIPNet(desiredIPAddress + "/16")
}

func newClusterJoinSwitch() *nbdb.LogicalSwitch {
	return &nbdb.LogicalSwitch{
		UUID: types.OVNJoinSwitch + "-UUID",
		Name: types.OVNJoinSwitch,
	}
}

func newNetworkClusterPortGroup(netInfo util.NetInfo) *nbdb.PortGroup {
	netControllerName := getNetworkControllerName(netInfo.GetNetworkName())
	fakeController := getFakeController(netControllerName)
	pgIDs := fakeController.getClusterPortGroupDbIDs(types.ClusterPortGroupNameBase)
	pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
	pg.UUID = pgIDs.String()
	return pg
}

func newNetworkRouterPortGroup(netInfo util.NetInfo) *nbdb.PortGroup {
	netControllerName := getNetworkControllerName(netInfo.GetNetworkName())
	fakeController := getFakeController(netControllerName)
	pgIDs := fakeController.getClusterPortGroupDbIDs(types.ClusterRtrPortGroupNameBase)
	pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
	pg.UUID = pgIDs.String()
	return pg
}

func newClusterPortGroup() *nbdb.PortGroup {
	return newNetworkClusterPortGroup(&util.DefaultNetInfo{})
}

func newRouterPortGroup() *nbdb.PortGroup {
	return newNetworkRouterPortGroup(&util.DefaultNetInfo{})
}

func newOVNClusterRouter() *nbdb.LogicalRouter {
	return &nbdb.LogicalRouter{
		UUID: types.OVNClusterRouter + "-UUID",
		Name: types.OVNClusterRouter,
	}
}

func newLoadBalancerGroup(name string) *nbdb.LoadBalancerGroup {
	return &nbdb.LoadBalancerGroup{
		Name: name, UUID: name + "-UUID",
	}
}

func newNodeSNAT(uuid, logicalIP, externalIP string) *nbdb.NAT {
	return &nbdb.NAT{
		UUID:       uuid,
		LogicalIP:  logicalIP,
		ExternalIP: externalIP,
		Type:       nbdb.NATTypeSNAT,
		Options: map[string]string{
			"stateless": "false",
		},
	}
}

func newNodeSNATWithMatch(uuid, logicalIP, externalIP, match string) *nbdb.NAT {
	nat := newNodeSNAT(uuid, logicalIP, externalIP)
	nat.Match = match
	return nat
}

func TestController_syncNodes(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)

	node1Name := "node1"
	nodeRmName := "deleteMeNode"

	transitSwitch := nbdb.LogicalSwitch{
		Name:        types.TransitSwitch,
		UUID:        types.TransitSwitch + "-UUID",
		OtherConfig: map[string]string{"subnet": "1.2.3.4/24"},
	}

	tests := []struct {
		name         string
		initialNBDB  []libovsdbtest.TestData
		expectedNBDB []libovsdbtest.TestData
		initialSBDB  []libovsdbtest.TestData
		expectedSBDB []libovsdbtest.TestData
	}{
		{
			name: "removes node 2, leaves node 1 alone",
			initialNBDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:        node1Name,
					UUID:        node1Name + "-UUID",
					OtherConfig: map[string]string{"subnet": "1.2.3.4/24"},
				},
				&nbdb.LogicalSwitch{
					Name: types.ExternalSwitchPrefix + node1Name,
					UUID: types.ExternalSwitchPrefix + node1Name + "-UUID",
				},
				&nbdb.LogicalSwitch{
					Name: types.EgressGWSwitchPrefix + types.ExternalSwitchPrefix + node1Name,
					UUID: types.EgressGWSwitchPrefix + types.ExternalSwitchPrefix + node1Name + "-UUID",
				},
				&nbdb.LogicalRouter{
					Name: types.GWRouterPrefix + node1Name,
					UUID: types.GWRouterPrefix + node1Name + "-UUID",
				},
				// these should be deleted
				&nbdb.LogicalSwitch{
					Name:        nodeRmName,
					UUID:        nodeRmName + "-UUID",
					OtherConfig: map[string]string{"subnet": "1.2.3.5/24"},
				},
				&nbdb.LogicalSwitch{
					Name: types.ExternalSwitchPrefix + nodeRmName,
					UUID: types.ExternalSwitchPrefix + nodeRmName + "-UUID",
				},
				&nbdb.LogicalSwitch{
					Name: types.EgressGWSwitchPrefix + types.ExternalSwitchPrefix + nodeRmName,
					UUID: types.EgressGWSwitchPrefix + types.ExternalSwitchPrefix + nodeRmName + "-UUID",
				},
				&nbdb.LogicalRouter{
					Name: types.GWRouterPrefix + nodeRmName,
					UUID: types.GWRouterPrefix + nodeRmName + "-UUID",
				},
			},
			expectedNBDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:        node1Name,
					UUID:        node1Name + "-UUID",
					OtherConfig: map[string]string{"subnet": "1.2.3.4/24"},
				},
				&nbdb.LogicalSwitch{
					Name: types.ExternalSwitchPrefix + node1Name,
					UUID: types.ExternalSwitchPrefix + node1Name + "-UUID",
				},
				&nbdb.LogicalSwitch{
					Name: types.EgressGWSwitchPrefix + types.ExternalSwitchPrefix + node1Name,
					UUID: types.EgressGWSwitchPrefix + types.ExternalSwitchPrefix + node1Name + "-UUID",
				},
				&nbdb.LogicalRouter{
					Name: types.GWRouterPrefix + node1Name,
					UUID: types.GWRouterPrefix + node1Name + "-UUID",
				},
			},
		},
		{
			name: "removes node that only had external logical switch left behind",
			initialNBDB: []libovsdbtest.TestData{
				// left-over external logical switch
				&nbdb.LogicalSwitch{
					Name: types.ExternalSwitchPrefix + nodeRmName,
					UUID: types.ExternalSwitchPrefix + nodeRmName + "-UUID",
				},
			},
			expectedNBDB: []libovsdbtest.TestData{},
		},
		{
			name: "removes node that only had external gw logical router left behind",
			initialNBDB: []libovsdbtest.TestData{
				// left-over gateway router
				&nbdb.LogicalRouter{
					Name: types.GWRouterPrefix + nodeRmName,
					UUID: types.GWRouterPrefix + nodeRmName + "-UUID",
				},
			},
			expectedNBDB: []libovsdbtest.TestData{},
		},
		{
			name: "make sure transit switch is never removed",
			initialNBDB: []libovsdbtest.TestData{
				&transitSwitch,
				// left-over external logical switch
				&nbdb.LogicalSwitch{
					Name: types.ExternalSwitchPrefix + nodeRmName,
					UUID: types.ExternalSwitchPrefix + nodeRmName + "-UUID",
				},
			},
			expectedNBDB: []libovsdbtest.TestData{
				&transitSwitch,
			},
		},
		{
			name: "removes stale chassis and chassis private",
			initialSBDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: chassisIDForNode(node1Name), Hostname: node1Name},
				&sbdb.ChassisPrivate{Name: chassisIDForNode(node1Name)},
				&sbdb.Chassis{Name: chassisIDForNode(nodeRmName), Hostname: nodeRmName},
				&sbdb.ChassisPrivate{Name: chassisIDForNode(nodeRmName)},
				&sbdb.ChassisPrivate{Name: chassisIDForNode("node3")},
			},
			expectedSBDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: chassisIDForNode(node1Name), Hostname: node1Name},
				&sbdb.ChassisPrivate{Name: chassisIDForNode(node1Name)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stopChan := make(chan struct{})
			wg := &sync.WaitGroup{}
			defer func() {
				close(stopChan)
				wg.Wait()
			}()

			testNode := corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						"k8s.ovn.org/node-chassis-id": chassisIDForNode(node1Name),
					},
				},
			}

			kubeFakeClient := fake.NewSimpleClientset()
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			fakeClient := &util.OVNKubeControllerClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
			}
			f, err := factory.NewOVNKubeControllerWatchFactory(fakeClient, "test-node")
			if err != nil {
				t.Fatalf("%s: Error creating master watch factory: %v", tt.name, err)
			}
			defer f.Shutdown()

			dbSetup := libovsdbtest.TestSetup{
				NBData: tt.initialNBDB,
				SBData: tt.initialSBDB,
			}
			nbClient, sbClient, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
			if err != nil {
				t.Fatalf("Error creating libovsdb test harness: %v", err)
			}
			t.Cleanup(libovsdbCleanup.Cleanup)

			controller, err := NewOvnController(
				fakeClient,
				f,
				stopChan,
				nil,
				networkmanager.Default().Interface(),
				nbClient,
				sbClient,
				record.NewFakeRecorder(0),
				wg,
				nil,
				NewPortCache(stopChan),
				nil,
				node1Name,
			)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			err = controller.syncNodes([]interface{}{&testNode})
			if err != nil {
				t.Fatalf("%s: Error on syncNodes: %v", tt.name, err)
			}

			if tt.expectedNBDB != nil {
				matcher := libovsdbtest.HaveDataIgnoringUUIDs(tt.expectedNBDB)
				match, err := matcher.Match(nbClient)
				if err != nil {
					t.Fatalf("%s: NB matcher error: %v", tt.name, err)
				}
				if !match {
					t.Fatalf("%s: NB DB state did not match: %s", tt.name, matcher.FailureMessage(sbClient))
				}
			}

			if tt.expectedSBDB != nil {
				matcher := libovsdbtest.HaveDataIgnoringUUIDs(tt.expectedSBDB)
				match, err := matcher.Match(sbClient)
				if err != nil {
					t.Fatalf("%s: SB matcher error: %v", tt.name, err)
				}
				if !match {
					t.Fatalf("%s: SB DB state did not match: %s", tt.name, matcher.FailureMessage(sbClient))
				}
			}
		})
	}
}

func TestController_deleteStaleNodeChassis(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	tests := []struct {
		name         string
		node         corev1.Node
		initialSBDB  []libovsdbtest.TestData
		expectedSBDB []libovsdbtest.TestData
	}{
		{
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						"k8s.ovn.org/node-chassis-id": chassisIDForNode("node1-dpu"),
					},
				},
			},
			name: "removes stale chassis when ovn running on DPU",
			initialSBDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: chassisIDForNode("node1-dpu"), Hostname: "node1"},
				&sbdb.ChassisPrivate{Name: chassisIDForNode("node1-dpu")},
				&sbdb.Chassis{Name: chassisIDForNode("node1"), Hostname: "node1"},
				&sbdb.ChassisPrivate{Name: chassisIDForNode("node1")},
			},
			expectedSBDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: chassisIDForNode("node1-dpu"), Hostname: "node1"},
				&sbdb.ChassisPrivate{Name: chassisIDForNode("node1-dpu")},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stopChan := make(chan struct{})
			wg := &sync.WaitGroup{}
			defer func() {
				close(stopChan)
				wg.Wait()
			}()
			kubeFakeClient := fake.NewSimpleClientset()
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			fakeClient := &util.OVNKubeControllerClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
			}
			f, err := factory.NewOVNKubeControllerWatchFactory(fakeClient, "test-node")
			if err != nil {
				t.Fatalf("%s: Error creating master watch factory: %v", tt.name, err)
			}
			defer f.Shutdown()

			dbSetup := libovsdbtest.TestSetup{
				SBData: tt.initialSBDB,
			}
			nbClient, sbClient, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
			if err != nil {
				t.Fatalf("Error creating libovsdb test harness: %v", err)
			}
			t.Cleanup(libovsdbCleanup.Cleanup)

			controller, err := NewOvnController(
				fakeClient,
				f,
				stopChan,
				nil,
				networkmanager.Default().Interface(),
				nbClient,
				sbClient,
				record.NewFakeRecorder(0),
				wg,
				nil,
				NewPortCache(stopChan),
				nil,
				tt.node.Name,
			)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())

			err = controller.deleteStaleNodeChassis(&tt.node)
			if err != nil {
				t.Fatalf("%s: Error on syncNodes: %v", tt.name, err)
			}

			matcher := libovsdbtest.HaveDataIgnoringUUIDs(tt.expectedSBDB)
			match, err := matcher.Match(sbClient)
			if err != nil {
				t.Fatalf("%s: matcher error: %v", tt.name, err)
			}
			if !match {
				t.Fatalf("%s: DB state did not match: %s", tt.name, matcher.FailureMessage(sbClient))
			}
		})
	}
}

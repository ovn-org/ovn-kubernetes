package ovn

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressqosfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"
	egressservicefake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
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
	ovnNodeID = "k8s.ovn.org/node-id"

	// ovnNodeGRLRPAddr is the CIDR form representation of Gate Router LRP IP address to join switch (i.e: 100.64.0.5/24)
	ovnNodeGRLRPAddr = "k8s.ovn.org/node-gateway-router-lrp-ifaddr"
)

func (n tNode) k8sNode(nodeID string) v1.Node {
	node := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: n.Name,
			Annotations: map[string]string{
				ovnNodeID:        nodeID,
				ovnNodeGRLRPAddr: "{\"ipv4\": \"100.64.0." + nodeID + "/16\"}",
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

func addNodeLogicalFlows(testData []libovsdbtest.TestData, expectedOVNClusterRouter *nbdb.LogicalRouter, expectedNodeSwitch *nbdb.LogicalSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup *nbdb.PortGroup, node *tNode) []libovsdbtest.TestData {

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
	})

	expectedOVNClusterRouter.Ports = append(expectedOVNClusterRouter.Ports, types.RouterToSwitchPrefix+node.Name+"-UUID")

	testData = append(testData, &nbdb.LogicalSwitchPort{
		Name: types.SwitchToRouterPrefix + node.Name,
		UUID: types.SwitchToRouterPrefix + node.Name + "-UUID",
		Type: "router",
		Options: map[string]string{
			"router-port": types.RouterToSwitchPrefix + node.Name,
			"arp_proxy":   kubevirt.ComposeARPProxyLSPOption(),
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
	testData = append(testData, &nbdb.LogicalRouterPolicy{
		UUID:     "policy-based-route-2-UUID",
		Action:   nbdb.LogicalRouterPolicyActionReroute,
		Match:    matchStr2,
		Nexthops: []string{node.NodeMgmtPortIP},
		Priority: intPriority,
	})
	expectedOVNClusterRouter.Policies = append(expectedOVNClusterRouter.Policies, []string{"policy-based-route-1-UUID", "policy-based-route-2-UUID"}...)
	testData = append(testData, expectedClusterPortGroup)
	testData = append(testData, expectedClusterRouterPortGroup)
	return testData
}

/* FIXME for updated local gw

func populatePortAddresses(nodeName, lsp, mac, ips string, ovnClient goovn.Client) {
	cmd, err := ovnClient.LSPAdd(nodeName, lsp)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	err = cmd.Execute()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	addresses := mac + " " + ips
	addresses = strings.TrimSpace(addresses)
	cmd, err = ovnClient.LSPSetDynamicAddresses(lsp, addresses)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	err = cmd.Execute()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

var _ = ginkgo.Describe("Master Operations", func() {
	var (
		app      *cli.App
		f        *factory.WatchFactory
		stopChan chan struct{}
		wg       *sync.WaitGroup
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}
	})

	ginkgo.AfterEach(func() {
		close(stopChan)
		f.Shutdown()
		wg.Wait()
	})

	ginkgo.It("creates logical network elements for a 2-node cluster", func() {
		const (
			clusterIPNet string = "10.1.0.0"
			clusterCIDR  string = clusterIPNet + "/16"
		)

		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName    string = "node1"
				nodeSubnet  string = "10.1.0.0/24"
				clusterCIDR string = "10.1.0.0/16"
				nextHop     string = "10.1.0.2"
				mgmtMAC     string = "01:02:03:04:05:06"
				hybMAC      string = "02:03:04:05:06:07"
				hybIP       string = "10.1.0.3"
			)

			fexec, tcpLBUUID, udpLBUUID, sctpLBUUID := defaultFakeExec(nodeSubnet, nodeName, true)
			cleanupGateway(fexec, nodeName, nodeSubnet, clusterCIDR, nextHop)

			testNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			}}

			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			crdFakeClient := &apiextensionsfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				APIExtensionsClient:  crdFakeClient,
			}

			err := util.SetExec(fexec)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			mockOVNNBClient := ovntest.NewMockOVNClient(goovn.DBNB)
			mockOVNSBClient := ovntest.NewMockOVNClient(goovn.DBSB)
			lsp := "int-" + nodeName
			populatePortAddresses(nodeName, lsp, hybMAC, hybIP, mockOVNNBClient)
			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient, egressIPFakeClient, egressFirewallFakeClient}, testNode.Name)
			err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{Mode: config.GatewayModeDisabled})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeManagementPortMACAddress(nodeAnnotator, ovntest.MustParseMAC(mgmtMAC))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f, err = factory.NewMasterWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			oc := NewOvnController(fakeClient, f, stopChan,
				newFakeAddressSetFactory(),
				mockOVNNBClient,
				mockOVNSBClient, record.NewFakeRecorder(0))

			gomega.Expect(oc).NotTo(gomega.BeNil())
			oc.TCPLoadBalancerUUID = tcpLBUUID
			oc.UDPLoadBalancerUUID = udpLBUUID
			oc.SCTPLoadBalancerUUID = sctpLBUUID

			err = oc.StartClusterMaster("master")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			oc.WatchNodes()

			wg.Add(1)
			go func() {
				defer wg.Done()
				oc.hoMaster.Run(stopChan)
			}()

			gomega.Eventually(fexec.CalledMatchesExpected, 2).Should(gomega.BeTrue(), fexec.ErrorDesc)
			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			subnetsFromAnnotation, err := util.ParseNodeHostSubnetAnnotation(updatedNode, types.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(subnetsFromAnnotation[0].String()).To(gomega.Equal(nodeSubnet))

			macFromAnnotation, err := util.ParseNodeManagementPortMACAddress(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(macFromAnnotation.String()).To(gomega.Equal(mgmtMAC))

			gomega.Eventually(fexec.CalledMatchesExpected, 2).Should(gomega.BeTrue(), fexec.ErrorDesc)
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-enable-multicast",
			"-enable-hybrid-overlay",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("works without SCTP support", func() {
		const (
			clusterIPNet string = "10.1.0.0"
			clusterCIDR  string = clusterIPNet + "/16"
		)

		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName    string = "sctp-test-node"
				nodeSubnet  string = "10.1.0.0/24"
				clusterCIDR string = "10.1.0.0/16"
				nextHop     string = "10.1.0.2"
				mgmtMAC     string = "01:02:03:04:05:06"
				hybMAC      string = "02:03:04:05:06:07"
				hybIP       string = "10.1.0.3"
			)

			fexec, tcpLBUUID, udpLBUUID, _ := defaultFakeExec(nodeSubnet, nodeName, false)
			cleanupGateway(fexec, nodeName, nodeSubnet, clusterCIDR, nextHop)

			testNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			}}

			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			crdFakeClient := &apiextensionsfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				APIExtensionsClient:  crdFakeClient,
			}

			err := util.SetExec(fexec)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			mockOVNNBClient := ovntest.NewMockOVNClient(goovn.DBNB)
			mockOVNSBClient := ovntest.NewMockOVNClient(goovn.DBSB)
			lsp := "int-" + nodeName
			populatePortAddresses(nodeName, lsp, hybMAC, hybIP, mockOVNNBClient)
			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient, egressIPFakeClient, egressFirewallFakeClient}, testNode.Name)
			err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{Mode: config.GatewayModeDisabled})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeManagementPortMACAddress(nodeAnnotator, ovntest.MustParseMAC(mgmtMAC))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f, err = factory.NewMasterWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			oc := NewOvnController(fakeClient, f, stopChan,
				newFakeAddressSetFactory(), mockOVNNBClient,
				mockOVNSBClient, record.NewFakeRecorder(0))

			gomega.Expect(oc).NotTo(gomega.BeNil())
			oc.TCPLoadBalancerUUID = tcpLBUUID
			oc.UDPLoadBalancerUUID = udpLBUUID
			oc.SCTPLoadBalancerUUID = ""

			err = oc.StartClusterMaster("master")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			oc.WatchNodes()

			wg.Add(1)
			go func() {
				defer wg.Done()
				oc.hoMaster.Run(stopChan)
			}()

			gomega.Eventually(fexec.CalledMatchesExpected, 2).Should(gomega.BeTrue(), fexec.ErrorDesc)
			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			subnetsFromAnnotation, err := util.ParseNodeHostSubnetAnnotation(updatedNode, types.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(subnetsFromAnnotation[0].String()).To(gomega.Equal(nodeSubnet))

			macFromAnnotation, err := util.ParseNodeManagementPortMACAddress(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(macFromAnnotation.String()).To(gomega.Equal(mgmtMAC))

			gomega.Eventually(fexec.CalledMatchesExpected, 2).Should(gomega.BeTrue(), fexec.ErrorDesc)
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-enable-multicast",
			"-enable-hybrid-overlay",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("does not allocate a hostsubnet for a node that already has one", func() {
		const (
			clusterIPNet string = "10.1.0.0"
			clusterCIDR  string = clusterIPNet + "/16"
		)

		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName    string = "node1"
				nodeSubnet  string = "10.1.3.0/24"
				clusterCIDR string = "10.1.0.0/16"
				nextHop     string = "10.1.3.2"
				mgmtMAC     string = "01:02:03:04:05:06"
				hybMAC      string = "02:03:04:05:06:07"
				hybIP       string = "10.1.0.3"
			)

			testNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			}}

			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			crdFakeClient := &apiextensionsfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				APIExtensionsClient:  crdFakeClient,
			}

			fexec, tcpLBUUID, udpLBUUID, sctpLBUUID := defaultFakeExec(nodeSubnet, nodeName, true)
			err := util.SetExec(fexec)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			cleanupGateway(fexec, nodeName, nodeSubnet, clusterCIDR, nextHop)

			_, err = config.InitConfig(ctx, fexec, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			mockOVNNBClient := ovntest.NewMockOVNClient(goovn.DBNB)
			mockOVNSBClient := ovntest.NewMockOVNClient(goovn.DBSB)
			lsp := "int-" + nodeName
			populatePortAddresses(nodeName, lsp, hybMAC, hybIP, mockOVNNBClient)
			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient, egressIPFakeClient, egressFirewallFakeClient}, testNode.Name)
			err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{Mode: config.GatewayModeDisabled})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeManagementPortMACAddress(nodeAnnotator, ovntest.MustParseMAC(mgmtMAC))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(nodeSubnet))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f, err = factory.NewMasterWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			oc := NewOvnController(fakeClient, f, stopChan,
				newFakeAddressSetFactory(), mockOVNNBClient, mockOVNSBClient, record.NewFakeRecorder(0))
			gomega.Expect(oc).NotTo(gomega.BeNil())
			oc.TCPLoadBalancerUUID = tcpLBUUID
			oc.UDPLoadBalancerUUID = udpLBUUID
			oc.SCTPLoadBalancerUUID = sctpLBUUID

			err = oc.StartClusterMaster("master")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			oc.WatchNodes()

			wg.Add(1)
			go func() {
				defer wg.Done()
				oc.hoMaster.Run(stopChan)
			}()

			gomega.Eventually(fexec.CalledMatchesExpected, 2).Should(gomega.BeTrue(), fexec.ErrorDesc)
			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			subnetsFromAnnotation, err := util.ParseNodeHostSubnetAnnotation(updatedNode, types.DefaultNetworkName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(subnetsFromAnnotation[0].String()).To(gomega.Equal(nodeSubnet))

			macFromAnnotation, err := util.ParseNodeManagementPortMACAddress(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(macFromAnnotation.String()).To(gomega.Equal(mgmtMAC))

			gomega.Eventually(fexec.CalledMatchesExpected, 2).Should(gomega.BeTrue(), fexec.ErrorDesc)
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-enable-multicast",
			"-enable-hybrid-overlay",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("removes deleted nodes from the OVN database", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				tcpLBUUID         string = "1a3dfc82-2749-4931-9190-c30e7c0ecea3"
				udpLBUUID         string = "6d3142fc-53e8-4ac1-88e6-46094a5a9957"
				sctpLBUUID        string = "0514c521-a120-4756-aec6-883fe5db7139"
				node1Name         string = "openshift-node-1"
				node1Subnet       string = "10.128.0.0/24"
				node1MgmtPortIP   string = "10.128.0.2"
				masterName        string = "openshift-master-node"
				masterSubnet      string = "10.128.2.0/24"
				masterGWCIDR      string = "10.128.2.1/24"
				masterMgmtPortIP  string = "10.128.2.2"
				lrpMAC            string = "0a:58:0a:80:02:01"
				masterMgmtPortMAC string = "00:00:00:55:66:77"
			)

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_" + masterName + " networks",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name,other-config find logical_switch",
				// Return two nodes
				Output: fmt.Sprintf(`%s
subnet=%s

%s
subnet=%s
`, node1Name, node1Subnet, masterName, masterSubnet),
			})

			// Expect the code to delete node1 which no longer exists in Kubernetes API
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exist ls-del " + node1Name,
				"ovn-nbctl --timeout=15 --if-exist lrp-del " + types.RouterToSwitchPrefix + node1Name,
			})
			cleanupGateway(fexec, node1Name, node1Subnet, types.OVNClusterRouter, node1MgmtPortIP)

			// Expect the code to re-add the master (which still exists)
			// when the factory watch begins and enumerates all existing
			// Kubernetes API nodes
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exists lrp-del " + types.RouterToSwitchPrefix + masterName + " -- lrp-add ovn_cluster_router " + types.RouterToSwitchPrefix + masterName + " " + lrpMAC + " " + masterGWCIDR,
				"ovn-nbctl --timeout=15 --may-exist ls-add " + masterName + " -- set logical_switch " + masterName + " other-config:subnet=" + masterSubnet + " other-config:exclude_ips=" + masterMgmtPortIP,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + masterName + " " + types.SwitchToRouterPrefix + masterName + " -- set logical_switch_port " + types.SwitchToRouterPrefix + masterName + " type=router options:router-port=" + types.RouterToSwitchPrefix + masterName + " addresses=\"" + lrpMAC + "\"",
				"ovn-nbctl --timeout=15 set logical_switch " + masterName + " load_balancer=" + tcpLBUUID,
				"ovn-nbctl --timeout=15 add logical_switch " + masterName + " load_balancer " + udpLBUUID,
				"ovn-nbctl --timeout=15 add logical_switch " + masterName + " load_balancer " + sctpLBUUID,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + masterName + " " + types.K8sPrefix + masterName + " -- lsp-set-addresses " + types.K8sPrefix + masterName + " " + masterMgmtPortMAC + " " + masterMgmtPortIP,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port " + types.K8sPrefix + masterName + " _uuid",
				Output: fakeUUID + "\n",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 lsp-list " + masterName,
				Output: "29df5ce5-2802-4ee5-891f-4fb27ca776e9 (" + types.K8sPrefix + masterName + ")",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --if-exists remove logical_switch " + masterName + " other-config exclude_ips",
			})

			cleanupGateway(fexec, masterName, masterSubnet, masterGWCIDR, masterMgmtPortIP)

			masterNode := v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: masterName,
				},
				Spec: v1.NodeSpec{
					ProviderID: "gce://openshift-gce-devel-ci/us-east1-b/ci-op-tbtpp-m-0",
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:               v1.NodeNetworkUnavailable,
							Status:             v1.ConditionTrue,
							Reason:             "NoRouteCreated",
							Message:            "Node created without a route",
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			}
			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{masterNode},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			crdFakeClient := &apiextensionsfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				APIExtensionsClient:  crdFakeClient,
			}

			err := util.SetExec(fexec)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient, egressIPFakeClient, egressFirewallFakeClient}, masterNode.Name)
			err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{Mode: config.GatewayModeDisabled})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeManagementPortMACAddress(nodeAnnotator, ovntest.MustParseMAC(masterMgmtPortMAC))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(masterSubnet))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f, err = factory.NewMasterWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			oc := NewOvnController(fakeClient, f, stopChan,
				newFakeAddressSetFactory(), ovntest.NewMockOVNClient(goovn.DBNB),
				ovntest.NewMockOVNClient(goovn.DBSB), record.NewFakeRecorder(0))
			gomega.Expect(oc).NotTo(gomega.BeNil())
			oc.TCPLoadBalancerUUID = tcpLBUUID
			oc.UDPLoadBalancerUUID = udpLBUUID
			oc.SCTPLoadBalancerUUID = sctpLBUUID
			oc.SCTPSupport = true
			oc.joinSwIPManager, _ = newJoinLogicalSwitchIPManager()
			_, _ = oc.joinSwIPManager.ensureJoinLRPIPs(types.OVNClusterRouter)

			// Let the real code run and ensure OVN database sync
			oc.WatchNodes()

			gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)

			node, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), masterNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(node.Status.Conditions)).To(BeIdenticalTo(1))
			gomega.Expect(node.Status.Conditions[0].Message).To(BeIdenticalTo("ovn-kube cleared kubelet-set NoRouteCreated"))

			return nil
		}

		err := app.Run([]string{app.Name})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
})
*/

func startFakeController(oc *DefaultNetworkController, wg *sync.WaitGroup) []*net.IPNet {
	var clusterSubnets []*net.IPNet
	for _, clusterEntry := range config.Default.ClusterSubnets {
		clusterSubnets = append(clusterSubnets, clusterEntry.CIDR)
	}

	// Let the real code run and ensure OVN database sync
	gomega.Expect(oc.WatchNodes()).To(gomega.Succeed())
	gomega.Expect(oc.StartServiceController(wg, false)).To(gomega.Succeed())

	return clusterSubnets
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
		testNode       v1.Node
		fakeClient     *util.OVNMasterClientset
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
		expectedSBDatabaseState        []libovsdbtest.TestData
		l3GatewayConfig                *util.L3GatewayConfig
		nodeHostAddrs                  sets.Set[string]
	)

	const (
		clusterIPNet  string = "10.1.0.0"
		clusterCIDR   string = clusterIPNet + "/16"
		clusterv6CIDR string = "aef0::/48"
		vlanID               = 1024
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())

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
			GatewayRouterIPMask:  "172.16.16.2/24",
			GatewayRouterIP:      "172.16.16.2",
			GatewayRouterNextHop: "172.16.16.1",
			PhysicalBridgeName:   "br-eth0",
			NodeGWIP:             "10.1.1.1/24",
			NodeMgmtPortIP:       "10.1.1.2",
			NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
			DnatSnatIP:           "169.254.0.1",
		}

		expectedClusterLBGroup = newLoadBalancerGroup(types.ClusterLBGroupName)
		expectedSwitchLBGroup = newLoadBalancerGroup(types.ClusterSwitchLBGroupName)
		expectedRouterLBGroup = newLoadBalancerGroup(types.ClusterRouterLBGroupName)
		expectedNodeSwitch = node1.logicalSwitch([]string{expectedClusterLBGroup.UUID, expectedSwitchLBGroup.UUID})
		expectedOVNClusterRouter = newOVNClusterRouter()
		expectedClusterRouterPortGroup = newRouterPortGroup()
		expectedClusterPortGroup = newClusterPortGroup()

		gr := types.GWRouterPrefix + node1.Name
		datapath := &sbdb.DatapathBinding{
			UUID:        gr + "-UUID",
			ExternalIDs: map[string]string{"logical-router": gr + "-UUID", "name": gr},
		}
		expectedSBDatabaseState = []libovsdbtest.TestData{datapath}
		dbSetup = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				newClusterJoinSwitch(),
				expectedNodeSwitch,
				newOVNClusterRouter(),
				newRouterPortGroup(),
				newClusterPortGroup(),
				expectedClusterLBGroup,
				expectedSwitchLBGroup,
				expectedRouterLBGroup,
			},
			SBData: []libovsdbtest.TestData{
				datapath,
			},
		}
		testNode = node1.k8sNode("2")

		kubeFakeClient = fake.NewSimpleClientset(&v1.NodeList{
			Items: []v1.Node{testNode},
		})
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		egressIPFakeClient := &egressipfake.Clientset{}
		egressQoSFakeClient := &egressqosfake.Clientset{}
		egressServiceFakeClient := &egressservicefake.Clientset{}
		fakeClient = &util.OVNMasterClientset{
			KubeClient:           kubeFakeClient,
			EgressIPClient:       egressIPFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
			EgressQoSClient:      egressQoSFakeClient,
			EgressServiceClient:  egressServiceFakeClient,
		}
		var err error

		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeAnnotator = kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient}, testNode.Name)
		l3GatewayConfig = node1.gatewayConfig(config.GatewayModeLocal, uint(vlanID))
		err = util.SetL3GatewayConfig(nodeAnnotator, l3GatewayConfig)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = util.SetNodeManagementPortMACAddress(nodeAnnotator, ovntest.MustParseMAC(node1.NodeMgmtPortMAC))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nodeHostAddrs = sets.New(node1.NodeIP)
		err = util.SetNodeHostAddresses(nodeAnnotator, nodeHostAddrs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = nodeAnnotator.Run()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		nbClient, sbClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		f, err = factory.NewMasterWatchFactory(fakeClient)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = f.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		recorder = record.NewFakeRecorder(10)
		oc, _ = NewOvnController(fakeClient, f, stopChan, nil, nbClient, sbClient, recorder, wg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		oc.clusterLoadBalancerGroupUUID = expectedClusterLBGroup.UUID
		oc.switchLoadBalancerGroupUUID = expectedSwitchLBGroup.UUID
		oc.routerLoadBalancerGroupUUID = expectedRouterLBGroup.UUID
		gomega.Expect(oc).NotTo(gomega.BeNil())
		oc.defaultCOPPUUID, err = EnsureDefaultCOPP(nbClient)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

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

		oc.SCTPSupport = true

		expectedNBDatabaseState = addNodeLogicalFlows(nil, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1)
	})

	ginkgo.AfterEach(func() {
		libovsdbCleanup.Cleanup()
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
			err = oc.syncGatewayLogicalNetwork(&testNode, l3GatewayConfig, []*net.IPNet{subnet}, nodeHostAddrs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			retry.InitRetryObjWithAdd(testNode, testNode.Name, oc.retryNodes)
			gomega.Expect(retry.RetryObjsLen(oc.retryNodes)).To(gomega.Equal(1))

			retry.CheckRetryObjectMultipleFieldsEventually(
				testNode.Name,
				oc.retryNodes,
				gomega.BeNil(),             // oldObj should be nil
				gomega.Not(gomega.BeNil()), // newObj should not be nil
			)

			retry.DeleteRetryObj(testNode.Name, oc.retryNodes)
			gomega.Expect(retry.CheckRetryObj(testNode.Name, oc.retryNodes)).To(gomega.BeFalse())

			skipSnat := false
			expectedNBDatabaseState = generateGatewayInitExpectedNB(expectedNBDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)},
				skipSnat, node1.NodeMgmtPortIP, "1400")
			gomega.Eventually(oc.nbClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))

			expectedSBDatabaseState := generateGatewayInitExpectedSB(expectedSBDatabaseState, node1.Name)
			gomega.Eventually(oc.sbClient).Should(libovsdbtest.HaveData(expectedSBDatabaseState))
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
			p := func(item *nbdb.LogicalRouterStaticRoute) bool { return false }
			err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(nbClient,
				types.OVNClusterRouter, badRoute, p)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.By("Syncing node with OVNK")
			node, err := oc.kube.GetNode(testNode.Name)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = oc.syncNodeManagementPort(node, []*net.IPNet{subnet})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = oc.syncGatewayLogicalNetwork(node, l3GatewayConfig, []*net.IPNet{subnet}, nodeHostAddrs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.By("Stale route should have been removed")

			skipSnat := false
			expectedNBDatabaseState = generateGatewayInitExpectedNB(expectedNBDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)},
				skipSnat, node1.NodeMgmtPortIP, "1400")
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
			err = oc.syncGatewayLogicalNetwork(&testNode, l3GatewayConfig, []*net.IPNet{subnet}, nodeHostAddrs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			skipSnat := false
			expectedNBDatabaseState = generateGatewayInitExpectedNB(expectedNBDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)},
				skipSnat, node1.NodeMgmtPortIP, "1400")
			gomega.Eventually(oc.nbClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))

			expectedSBDatabaseState := generateGatewayInitExpectedSB(expectedSBDatabaseState, node1.Name)
			gomega.Eventually(oc.sbClient).Should(libovsdbtest.HaveData(expectedSBDatabaseState))
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

	ginkgo.It("clear stale pod SNATs from syncGateway", func() {

		app.Action = func(ctx *cli.Context) error {

			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.Gateway.DisableSNATMultipleGWs = true

			// create a pod on this node
			ns := newNamespace("namespace-1")
			pod := *newPodWithLabels(ns.Name, podName, node1.Name, "10.0.0.3", egressPodLabel)
			_, err = fakeClient.KubeClient.CoreV1().Pods(ns.Name).Create(context.TODO(), &pod, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Let the real code run and ensure OVN database sync
			gomega.Expect(oc.WatchNodes()).To(gomega.Succeed())

			var clusterSubnets []*net.IPNet
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
			}
			externalIP, _, err := net.ParseCIDR(l3GatewayConfig.IPAddresses[0].String())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			skipSnat := config.Gateway.DisableSNATMultipleGWs
			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)

			expectedNBDatabaseState = generateGatewayInitExpectedNB(expectedNBDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)},
				skipSnat, node1.NodeMgmtPortIP, "1400")

			// add stale SNATs from pods to nodes on wrong node
			staleNats := []*nbdb.NAT{
				newNodeSNAT("stale-nodeNAT-UUID-1", "10.1.0.3", externalIP.String()),
				newNodeSNAT("stale-nodeNAT-UUID-2", "10.2.0.3", externalIP.String()),
				newNodeSNAT("stale-nodeNAT-UUID-3", "10.0.0.3", externalIP.String()),
				newNodeSNAT("stale-nodeNAT-UUID-4", "10.0.0.3", "172.16.16.3"),
			}
			GR := &nbdb.LogicalRouter{
				Name: types.GWRouterPrefix + node1.Name,
			}
			err = libovsdbops.CreateOrUpdateNATs(nbClient, GR, staleNats...)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// ensure the stale SNAT's are cleaned up
			gomega.Expect(oc.StartServiceController(wg, false)).To(gomega.Succeed())
			err = oc.syncGatewayLogicalNetwork(&testNode, l3GatewayConfig, []*net.IPNet{subnet}, nodeHostAddrs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedNBDatabaseState = append(expectedNBDatabaseState,
				newNodeSNAT("stale-nodeNAT-UUID-3", "10.0.0.3", externalIP.String()), // won't be deleted since pod exists on this node
				newNodeSNAT("stale-nodeNAT-UUID-4", "10.0.0.3", "172.16.16.3"))       // won't be deleted on this node but will be deleted on the node whose IP is 172.16.16.3 since this pod belongs to this node
			for _, testObj := range expectedNBDatabaseState {
				uuid := reflect.ValueOf(testObj).Elem().FieldByName("UUID").Interface().(string)
				if uuid == types.GWRouterPrefix+node1.Name+"-UUID" {
					GR := testObj.(*nbdb.LogicalRouter)
					GR.Nat = []string{"stale-nodeNAT-UUID-3", "stale-nodeNAT-UUID-4"}
					*testObj.(*nbdb.LogicalRouter) = *GR
					break
				}
			}
			gomega.Eventually(oc.nbClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))

			expectedSBDatabaseState := generateGatewayInitExpectedSB(expectedSBDatabaseState, node1.Name)
			gomega.Eventually(oc.sbClient).Should(libovsdbtest.HaveData(expectedSBDatabaseState))

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

	ginkgo.It("does not list node's pods when updating node after successfully adding the node", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			clusterSubnets := startFakeController(oc, wg)

			skipSnat := false
			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			expectedNBDatabaseState = generateGatewayInitExpectedNB(expectedNBDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)},
				skipSnat, node1.NodeMgmtPortIP, "1400")
			gomega.Eventually(oc.nbClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))

			ginkgo.By("modifying the node and triggering an update")

			var podsWereListed uint32
			kubeFakeClient.PrependReactor("list", "pods", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
				atomic.StoreUint32(&podsWereListed, 1)
				podList := &v1.PodList{}
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

			expectedSBDatabaseState := generateGatewayInitExpectedSB(expectedSBDatabaseState, node1.Name)
			gomega.Eventually(oc.sbClient).Should(libovsdbtest.HaveData(expectedSBDatabaseState))
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

	ginkgo.It("use node retry with updating a node", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			clusterSubnets := startFakeController(oc, wg)

			skipSnat := false
			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			expectedNBDatabaseState = generateGatewayInitExpectedNB(expectedNBDatabaseState, expectedOVNClusterRouter,
				expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig,
				[]*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)},
				skipSnat, node1.NodeMgmtPortIP, "1400")
			gomega.Eventually(oc.nbClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))

			ginkgo.By("Bringing down NBDB")
			// inject transient problem, nbdb is down
			oc.nbClient.Close()
			gomega.Eventually(func() bool {
				return oc.nbClient.Connected()
			}).Should(gomega.BeFalse())

			ginkgo.By("modifying the node and triggering an update")
			// modify the node and trigger an update
			node1.GatewayRouterNextHop = "172.16.16.111"
			l3Config := node1.gatewayConfig(config.GatewayModeShared, uint(vlanID))
			err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.By("update should have failed with a retry being present")
			// check to see if the retry cache has an entry for this node
			// In the retry entry, old obj should be nil, new obj should not be nil
			retry.CheckRetryObjectMultipleFieldsEventually(
				testNode.Name,
				oc.retryNodes,
				gomega.BeNil(),             // oldObj should be nil
				gomega.Not(gomega.BeNil()), // newObj should not be nil
			)
			connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
			defer cancel()
			ginkgo.By("bring up NBDB")
			resetNBClient(connCtx, oc.nbClient)
			retry.SetRetryObjWithNoBackoff(node1.Name, oc.retryNodes)
			oc.retryNodes.RequestRetryObjs() // retry the failed entry
			ginkgo.By("should be no retry entry after update completes")
			retry.CheckRetryObjectEventually(testNode.Name, false, oc.retryNodes)
			for _, data := range expectedNBDatabaseState {
				if route, ok := data.(*nbdb.LogicalRouterStaticRoute); ok {
					if route.Nexthop == "172.16.16.1" {
						route.Nexthop = node1.GatewayRouterNextHop
					}
				}
			}
			gomega.Eventually(oc.nbClient).Should(libovsdbtest.HaveData(expectedNBDatabaseState))

			expectedSBDatabaseState := generateGatewayInitExpectedSB(expectedSBDatabaseState, node1.Name)
			gomega.Eventually(oc.sbClient).Should(libovsdbtest.HaveData(expectedSBDatabaseState))
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

	ginkgo.It("use node retry with deleting a node", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			startFakeController(oc, wg)

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			err = oc.syncGatewayLogicalNetwork(&testNode, l3GatewayConfig, []*net.IPNet{subnet}, nodeHostAddrs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// inject transient problem, nbdb is down
			oc.nbClient.Close()
			gomega.Eventually(func() bool {
				return oc.nbClient.Connected()
			}).Should(gomega.BeFalse())

			// Node delete will fail with Failed to delete node node1,
			err = fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), testNode.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// sleep long enough for TransactWithRetry to fail, causing LS (and other rows related to node) delete to fail
			time.Sleep(types.OVSDBTimeout + time.Second)

			// check the retry entry for this node
			ginkgo.By("retry entry: old obj should not be nil, new obj should be nil")
			retry.CheckRetryObjectMultipleFieldsEventually(
				testNode.Name,
				oc.retryNodes,
				gomega.Not(gomega.BeNil()), // oldObj should not be nil
				gomega.BeNil(),             // newObj should be nil
			)

			// reconnect nbdb
			connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
			defer cancel()
			resetNBClient(connCtx, oc.nbClient)

			// reset backoff for immediate retry
			retry.SetRetryObjWithNoBackoff(node1.Name, oc.retryNodes)
			oc.retryNodes.RequestRetryObjs() // retry the failed entry

			retry.CheckRetryObjectEventually(testNode.Name, false, oc.retryNodes)
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

	ginkgo.It("delete a partially constructed node", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			startFakeController(oc, wg)

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			err = oc.syncGatewayLogicalNetwork(&testNode, l3GatewayConfig, []*net.IPNet{subnet}, nodeHostAddrs)
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

			// check the retry entry for this node
			retry.CheckRetryObjectEventually(testNode.Name, false, oc.retryNodes)
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

	ginkgo.It("use node retry for a node without a host subnet", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// Don't create clusterManager so that the node has not subnets allocated.

			config.Kubernetes.NoHostSubnetNodes = &metav1.LabelSelector{
				MatchLabels: nodeNoHostSubnetAnnotation(),
			}

			gomega.Expect(
				oc.retryNodes.ResourceHandler.AddResource(
					&testNode, false)).To(
				gomega.MatchError(
					"nodeAdd: error adding node \"node1\": could not find \"k8s.ovn.org/node-subnets\" annotation"))

			ginkgo.By("annotating the node with no host subnet")
			testNode.Labels = nodeNoHostSubnetAnnotation()
			_, err = fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &testNode, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("adding the node becomes possible")
			gomega.Expect(oc.retryNodes.ResourceHandler.AddResource(&testNode, false)).To(gomega.Succeed())

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

			newNodeSubnet := "10.1.1.0/24"
			newNode := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "newNode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets":                   fmt.Sprintf("{\"default\":[\"%s\", \"fd02:0:0:2::2895/64\"]}", newNodeSubnet),
						"k8s.ovn.org/node-chassis-id":                "2",
						"k8s.ovn.org/node-gateway-router-lrp-ifaddr": "{\"ipv4\":\"100.64.0.2/16\"}",
					},
				},
			}

			_, err = kubeFakeClient.CoreV1().Nodes().Create(context.TODO(), newNode, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			startFakeController(oc, wg)

			// check that a node event complaining about the mismatch between
			// node subnets and cluster subnets was posted
			gomega.Eventually(func() []string {
				eventsLock.Lock()
				defer eventsLock.Unlock()
				eventsCopy := make([]string, 0, len(events))
				for _, e := range events {
					eventsCopy = append(eventsCopy, e)
				}
				return eventsCopy
			}, 10).Should(gomega.ContainElement(gomega.ContainSubstring("failed to get expected host subnets for node newNode; expected v4 true have true, expected v6 false have true")))

			// Simulate the ClusterManager reconciling the node annotations to single-stack
			newNode, err = kubeFakeClient.CoreV1().Nodes().Get(context.TODO(), newNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			newNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":[\"%s\"]}", newNodeSubnet)
			_, err = kubeFakeClient.CoreV1().Nodes().Update(context.TODO(), newNode, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Ensure that the node's switch is eventually created once the annotations
			// are reconciled by the network cluster controller
			newNodeLS := &nbdb.LogicalSwitch{Name: newNode.Name}
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

	ginkgo.It("reconciles node host subnets after single-stack to dual-stack upgrade", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Expect(config.IPv4Mode).To(gomega.BeTrue())
			gomega.Expect(config.IPv6Mode).To(gomega.BeTrue())

			newNodeIpv4Subnet := "10.1.1.0/24"
			newNode := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "newNode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets":                   fmt.Sprintf("{\"default\":[\"%s\"]}", newNodeIpv4Subnet),
						"k8s.ovn.org/node-chassis-id":                "2",
						"k8s.ovn.org/node-gateway-router-lrp-ifaddr": "{\"ipv4\":\"100.64.0.2/16\"}",
					},
				},
			}

			_, err = kubeFakeClient.CoreV1().Nodes().Create(context.TODO(), newNode, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			startFakeController(oc, wg)

			// Simulate the ClusterManager reconciling the node annotations to dual-stack
			newNode, err = kubeFakeClient.CoreV1().Nodes().Get(context.TODO(), newNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			newNodeIpv6SubnetPrefix := "aef0:0:0:2::"
			newNodeIpv6Subnet := newNodeIpv6SubnetPrefix + "2895/64"
			newNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":[\"%s\", \"%s\"]}", newNodeIpv4Subnet, newNodeIpv6Subnet)
			_, err = kubeFakeClient.CoreV1().Nodes().Update(context.TODO(), newNode, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Ensure that the node's switch is eventually created once the annotations
			// are reconciled by the network cluster controller
			gomega.Eventually(func() bool {
				newNodeLS, err := libovsdbops.GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: newNode.Name})
				if err != nil {
					return false
				}
				if newNodeLS.OtherConfig["subnet"] != newNodeIpv4Subnet {
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

func newClusterPortGroup() *nbdb.PortGroup {
	return &nbdb.PortGroup{
		UUID: types.ClusterPortGroupNameBase + "-UUID",
		Name: types.ClusterPortGroupNameBase,
		ExternalIDs: map[string]string{
			"name": types.ClusterPortGroupNameBase,
		},
	}
}

func newRouterPortGroup() *nbdb.PortGroup {
	return &nbdb.PortGroup{
		UUID: types.ClusterRtrPortGroupNameBase + "-UUID",
		Name: types.ClusterRtrPortGroupNameBase,
		ExternalIDs: map[string]string{
			"name": types.ClusterRtrPortGroupNameBase,
		},
	}
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
				&sbdb.Chassis{Name: "chassis-node1", Hostname: node1Name},
				&sbdb.ChassisPrivate{Name: "chassis-node1"},
				&sbdb.Chassis{Name: "chassis-node2", Hostname: nodeRmName},
				&sbdb.ChassisPrivate{Name: "chassis-node2"},
				&sbdb.ChassisPrivate{Name: "chassis-node3"},
			},
			expectedSBDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: "chassis-node1", Hostname: node1Name},
				&sbdb.ChassisPrivate{Name: "chassis-node1"},
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

			testNode := v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
				},
			}

			kubeFakeClient := fake.NewSimpleClientset()
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			fakeClient := &util.OVNMasterClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
			}
			f, err := factory.NewMasterWatchFactory(fakeClient)
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
				nbClient,
				sbClient,
				record.NewFakeRecorder(0),
				wg)
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
		node         v1.Node
		initialSBDB  []libovsdbtest.TestData
		expectedSBDB []libovsdbtest.TestData
	}{
		{
			node: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						"k8s.ovn.org/node-chassis-id": "chassis-node1-dpu",
					},
				},
			},
			name: "removes stale chassis when ovn running on DPU",
			initialSBDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: "chassis-node1-dpu", Hostname: "node1"},
				&sbdb.ChassisPrivate{Name: "chassis-node1-dpu"},
				&sbdb.Chassis{Name: "chassis-node1", Hostname: "node1"},
				&sbdb.ChassisPrivate{Name: "chassis-node1"},
			},
			expectedSBDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: "chassis-node1-dpu", Hostname: "node1"},
				&sbdb.ChassisPrivate{Name: "chassis-node1-dpu"},
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
			fakeClient := &util.OVNMasterClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
			}
			f, err := factory.NewMasterWatchFactory(fakeClient)
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
				nbClient,
				sbClient,
				record.NewFakeRecorder(0),
				wg)
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

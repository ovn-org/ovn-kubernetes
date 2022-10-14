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

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressqosfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
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
	LrpMAC               string
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
	NodeHostAddress      []string
	NodeGWIP             string
	NodeMgmtPortIP       string
	NodeMgmtPortMAC      string
	DnatSnatIP           string
}

func (n tNode) k8sNode() v1.Node {
	node := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: n.Name,
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

func (n tNode) logicalSwitch(loadBalancerGroupUUID string) *nbdb.LogicalSwitch {
	return &nbdb.LogicalSwitch{
		UUID:              n.Name + "-UUID",
		Name:              n.Name,
		OtherConfig:       map[string]string{"subnet": n.NodeSubnet},
		LoadBalancerGroup: []string{loadBalancerGroupUUID},
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
		"ovn-nbctl --timeout=15 --data=bare --no-heading --format=csv --columns=name,other-config find logical_switch external_ids:network_name{=}[]",
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
		},
		Addresses: []string{"router"},
	})
	expectedNodeSwitch.Ports = append(expectedNodeSwitch.Ports, types.SwitchToRouterPrefix+node.Name+"-UUID")
	expectedClusterRouterPortGroup.Ports = []string{types.SwitchToRouterPrefix + node.Name + "-UUID"}

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
	gomega.Expect(node.NodeHostAddress).To(gomega.HaveLen(1))
	matchStr2 := fmt.Sprintf(`inport == "rtos-%s" && ip4.dst == %s /* %s */`, node.Name, node.NodeHostAddress[0], node.Name)
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
			networkAttchDefClient := &networkattachmentdefinitionfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:            kubeFakeClient,
				EgressIPClient:        egressIPFakeClient,
				EgressFirewallClient:  egressFirewallFakeClient,
				NetworkAttchDefClient: networkAttchDefClient,
				APIExtensionsClient:   crdFakeClient,
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

			clusterController := NewOvnController(fakeClient, f, stopChan,
				newFakeAddressSetFactory(),
				mockOVNNBClient,
				mockOVNSBClient, record.NewFakeRecorder(0))

			gomega.Expect(clusterController).NotTo(gomega.BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID
			clusterController.SCTPLoadBalancerUUID = sctpLBUUID

			err = clusterController.StartClusterMaster("master")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			clusterController.WatchNodes()

			wg.Add(1)
			go func() {
				defer wg.Done()
				clusterController.hoMaster.Run(stopChan)
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
			networkAttchDefClient := &networkattachmentdefinitionfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:            kubeFakeClient,
				EgressIPClient:        egressIPFakeClient,
				EgressFirewallClient:  egressFirewallFakeClient
				NetworkAttchDefClient: networkAttchDefClient,
				APIExtensionsClient:   crdFakeClient,
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

			clusterController := NewOvnController(fakeClient, f, stopChan,
				newFakeAddressSetFactory(), mockOVNNBClient,
				mockOVNSBClient, record.NewFakeRecorder(0))

			gomega.Expect(clusterController).NotTo(gomega.BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID
			clusterController.SCTPLoadBalancerUUID = ""

			err = clusterController.StartClusterMaster("master")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			clusterController.WatchNodes()

			wg.Add(1)
			go func() {
				defer wg.Done()
				clusterController.hoMaster.Run(stopChan)
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
			networkAttchDefClient := &networkattachmentdefinitionfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:            kubeFakeClient,
				EgressIPClient:        egressIPFakeClient,
				EgressFirewallClient:  egressFirewallFakeClient,
				NetworkAttchDefClient: networkAttchDefClient,
				APIExtensionsClient:   crdFakeClient,
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

			clusterController := NewOvnController(fakeClient, f, stopChan,
				newFakeAddressSetFactory(), mockOVNNBClient, mockOVNSBClient, record.NewFakeRecorder(0))
			gomega.Expect(clusterController).NotTo(gomega.BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID
			clusterController.SCTPLoadBalancerUUID = sctpLBUUID

			err = clusterController.StartClusterMaster("master")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			clusterController.WatchNodes()

			wg.Add(1)
			go func() {
				defer wg.Done()
				clusterController.hoMaster.Run(stopChan)
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
				Cmd: "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name,other-config find logical_switch external_ids:network_name{=}[]",
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
			networkAttchDefClient := &networkattachmentdefinitionfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:            kubeFakeClient,
				EgressIPClient:        egressIPFakeClient,
				EgressFirewallClient:  egressFirewallFakeClient,
				NetworkAttchDefClient: networkAttchDefClient,
				APIExtensionsClient:   crdFakeClient,
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

			clusterController := NewOvnController(fakeClient, f, stopChan,
				newFakeAddressSetFactory(), ovntest.NewMockOVNClient(goovn.DBNB),
				ovntest.NewMockOVNClient(goovn.DBSB), record.NewFakeRecorder(0))
			gomega.Expect(clusterController).NotTo(gomega.BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID
			clusterController.SCTPLoadBalancerUUID = sctpLBUUID
			clusterController.SCTPSupport = true
			clusterController.joinSwIPManager, _ = newJoinLogicalSwitchIPManager()
			_, _ = clusterController.joinSwIPManager.ensureJoinLRPIPs(types.OVNClusterRouter)

			// Let the real code run and ensure OVN database sync
			clusterController.WatchNodes()

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

var _ = ginkgo.Describe("Gateway Init Operations", func() {
	var (
		app             *cli.App
		f               *factory.WatchFactory
		stopChan        chan struct{}
		wg              *sync.WaitGroup
		libovsdbCleanup *libovsdbtest.Cleanup

		dbSetup           libovsdbtest.TestSetup
		node1             tNode
		testNode          v1.Node
		fakeClient        *util.OVNClientset
		kubeFakeClient    *fake.Clientset
		clusterController *Controller
		nodeAnnotator     kube.Annotator
	)

	const (
		clusterIPNet string = "10.1.0.0"
		clusterCIDR  string = clusterIPNet + "/16"
		vlanID              = 1024
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.Default.RawClusterSubnets = clusterCIDR

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}

		libovsdbCleanup = nil

		node1 = tNode{
			Name:                 "node1",
			NodeIP:               "1.2.3.4",
			NodeLRPMAC:           "0a:58:0a:01:01:01",
			LrpMAC:               "0a:58:64:40:00:02",
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
			NodeHostAddress:      []string{"9.9.9.9"},
			NodeGWIP:             "10.1.1.1/24",
			NodeMgmtPortIP:       "10.1.1.2",
			NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
			DnatSnatIP:           "169.254.0.1",
		}

		expectedClusterLBGroup := newLoadBalancerGroup()
		expectedNodeSwitch := node1.logicalSwitch(expectedClusterLBGroup.UUID)

		dbSetup = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				newClusterJoinSwitch(),
				expectedNodeSwitch,
				newOVNClusterRouter(),
				newRouterPortGroup(),
				newClusterPortGroup(),
				expectedClusterLBGroup,
			},
		}
		testNode = node1.k8sNode()

		kubeFakeClient = fake.NewSimpleClientset(&v1.NodeList{
			Items: []v1.Node{testNode},
		})
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		egressIPFakeClient := &egressipfake.Clientset{}
		egressQoSFakeClient := &egressqosfake.Clientset{}
		fakeClient = &util.OVNClientset{
			KubeClient:           kubeFakeClient,
			EgressIPClient:       egressIPFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
			EgressQoSClient:      egressQoSFakeClient,
		}
		var err error

		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		config.Kubernetes.HostNetworkNamespace = ""
		nodeAnnotator = kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient, fakeClient.EgressIPClient, fakeClient.EgressFirewallClient, nil}, testNode.Name)
		l3Config := node1.gatewayConfig(config.GatewayModeLocal, uint(vlanID))
		err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
		err = util.SetNodeManagementPortMACAddress(nodeAnnotator, ovntest.MustParseMAC(node1.NodeMgmtPortMAC))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = util.SetNodeHostAddresses(nodeAnnotator, sets.NewString(node1.NodeHostAddress...))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = nodeAnnotator.Run()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
		libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		f, err = factory.NewMasterWatchFactory(fakeClient)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = f.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		cm := NewControllerManager(fakeClient, "", f,
			stopChan, libovsdbOvnNBClient, libovsdbOvnSBClient,
			record.NewFakeRecorder(0), nil)
		clusterController, _ = cm.InitDefaultController(addressset.NewFakeAddressSetFactory())
		clusterController.loadBalancerGroupUUID = expectedClusterLBGroup.UUID
		gomega.Expect(clusterController).NotTo(gomega.BeNil())
		clusterController.defaultGatewayCOPPUUID, err = EnsureDefaultCOPP(libovsdbOvnNBClient)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		clusterController.SCTPSupport = true
		clusterController.joinSwIPManager, _ = lsm.NewJoinLogicalSwitchIPManager(clusterController.nbClient, expectedNodeSwitch.UUID, []string{node1.Name})
		_, _ = clusterController.joinSwIPManager.EnsureJoinLRPIPs(types.OVNClusterRouter)
	})

	ginkgo.AfterEach(func() {
		libovsdbCleanup.Cleanup()
		close(stopChan)
		f.Shutdown()
		wg.Wait()
	})

	ginkgo.It("sets up a local gateway", func() {

		app.Action = func(ctx *cli.Context) error {

			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostAddrs, err := util.ParseNodeHostAddresses(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedClusterLBGroup := newLoadBalancerGroup()
			expectedOVNClusterRouter := newOVNClusterRouter()
			expectedNodeSwitch := node1.logicalSwitch(expectedClusterLBGroup.UUID)
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			expectedDatabaseState := []libovsdbtest.TestData{}
			expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1)

			// Let the real code run and ensure OVN database sync
			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())

			gomega.Expect(clusterController.StartServiceController(wg, false)).To(gomega.Succeed())

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			err = clusterController.syncGatewayLogicalNetwork(updatedNode, l3GatewayConfig, []*net.IPNet{subnet}, hostAddrs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			initRetryObjWithAdd(testNode, testNode.Name, clusterController.retryNodes)
			gomega.Expect(retryObjsLen(clusterController.retryNodes)).To(gomega.Equal(1))
			if retryEntry, found := getRetryObj(testNode.Name, clusterController.retryNodes); found {
				gomega.Expect(retryEntry).ToNot(gomega.BeNil())
				gomega.Expect(retryEntry.newObj).ToNot(gomega.BeNil())
				gomega.Expect(retryEntry.oldObj).To(gomega.BeNil())
			}
			deleteRetryObj(testNode.Name, clusterController.retryNodes)
			gomega.Expect(checkRetryObj(testNode.Name, clusterController.retryNodes)).To(gomega.BeFalse())
			var clusterSubnets []*net.IPNet
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
			}

			skipSnat := false
			expectedDatabaseState = generateGatewayInitExpectedNB(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig, []*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)}, skipSnat, node1.NodeMgmtPortIP)
			gomega.Eventually(clusterController.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

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
			config.Kubernetes.HostNetworkNamespace = ""

			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostAddrs, err := util.ParseNodeHostAddresses(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedClusterLBGroup := newLoadBalancerGroup()
			expectedOVNClusterRouter := newOVNClusterRouter()
			expectedNodeSwitch := node1.logicalSwitch(expectedClusterLBGroup.UUID)
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			expectedDatabaseState := []libovsdbtest.TestData{}
			expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1)

			// Let the real code run and ensure OVN database sync
			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())

			gomega.Expect(clusterController.StartServiceController(wg, false)).To(gomega.Succeed())

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			err = clusterController.syncGatewayLogicalNetwork(updatedNode, l3GatewayConfig, []*net.IPNet{subnet}, hostAddrs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var clusterSubnets []*net.IPNet
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
			}

			skipSnat := false
			expectedDatabaseState = generateGatewayInitExpectedNB(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3GatewayConfig, []*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)}, skipSnat, node1.NodeMgmtPortIP)
			gomega.Eventually(clusterController.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

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
			config.Kubernetes.HostNetworkNamespace = ""

			expectedClusterLBGroup := newLoadBalancerGroup()
			expectedOVNClusterRouter := newOVNClusterRouter()
			expectedNodeSwitch := node1.logicalSwitch(expectedClusterLBGroup.UUID)
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			expectedDatabaseState := []libovsdbtest.TestData{}
			expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1)

			// Let the real code run and ensure OVN database sync
			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())
			// ensure db is consistent
			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)

			var clusterSubnets []*net.IPNet
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
			}

			skipSnat := false
			l3Config := node1.gatewayConfig(config.GatewayModeShared, uint(vlanID))
			expectedDatabaseState = generateGatewayInitExpectedNB(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3Config, []*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)}, skipSnat, node1.NodeMgmtPortIP)
			gomega.Eventually(clusterController.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

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
			config.Kubernetes.HostNetworkNamespace = ""

			expectedClusterLBGroup := newLoadBalancerGroup()
			expectedOVNClusterRouter := newOVNClusterRouter()
			expectedNodeSwitch := node1.logicalSwitch(expectedClusterLBGroup.UUID)
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			expectedDatabaseState := []libovsdbtest.TestData{}
			expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1)

			// Let the real code run and ensure OVN database sync
			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())
			// ensure db is consistent
			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)

			var clusterSubnets []*net.IPNet
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
			}

			skipSnat := false
			l3Config := node1.gatewayConfig(config.GatewayModeLocal, uint(vlanID))
			expectedDatabaseState = generateGatewayInitExpectedNB(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3Config, []*net.IPNet{classBIPAddress(node1.LrpIP)}, []*net.IPNet{classBIPAddress(node1.DrLrpIP)}, skipSnat, node1.NodeMgmtPortIP)
			gomega.Eventually(clusterController.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
			ginkgo.By("Bringing down NBDB")
			// inject transient problem, nbdb is down
			clusterController.nbClient.Close()
			gomega.Eventually(func() bool {
				return clusterController.nbClient.Connected()
			}).Should(gomega.BeFalse())

			ginkgo.By("modifying the node and triggering an update")
			// modify the node and trigger an update
			node1.GatewayRouterNextHop = "172.16.16.111"
			l3Config = node1.gatewayConfig(config.GatewayModeShared, uint(vlanID))
			err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.By("update should have failed with a retry present")
			// check to see if the retry cache has an entry for this node
			checkRetryObjectEventually(testNode.Name, true, clusterController.retryNodes)
			retryEntry, found := getRetryObj(testNode.Name, clusterController.retryNodes)
			gomega.Expect(found).To(gomega.BeTrue())
			ginkgo.By("retry entry new obj should not be nil")
			gomega.Expect(retryEntry.newObj).NotTo(gomega.BeNil())
			ginkgo.By("retry entry old obj should be nil")
			gomega.Expect(retryEntry.oldObj).To(gomega.BeNil())

			connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
			defer cancel()
			ginkgo.By("bring up NBDB")
			resetNBClient(connCtx, clusterController.nbClient)
			setRetryObjWithNoBackoff(node1.Name, clusterController.retryNodes)
			clusterController.retryNodes.RequestRetryObjs() // retry the failed entry
			ginkgo.By("should be no retry entry after update completes")
			checkRetryObjectEventually(testNode.Name, false, clusterController.retryNodes)
			for _, data := range expectedDatabaseState {
				if route, ok := data.(*nbdb.LogicalRouterStaticRoute); ok {
					if route.Nexthop == "172.16.16.1" {
						route.Nexthop = node1.GatewayRouterNextHop
					}
				}
			}
			gomega.Eventually(clusterController.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
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
			config.Kubernetes.HostNetworkNamespace = ""

			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostAddrs, err := util.ParseNodeHostAddresses(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedClusterLBGroup := newLoadBalancerGroup()
			expectedOVNClusterRouter := newOVNClusterRouter()
			expectedNodeSwitch := node1.logicalSwitch(expectedClusterLBGroup.UUID)
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			expectedDatabaseState := []libovsdbtest.TestData{}
			expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1)

			// Let the real code run and ensure OVN database sync
			gomega.Expect(clusterController.WatchNodes()).To(gomega.Succeed())
			gomega.Expect(clusterController.StartServiceController(wg, false)).To(gomega.Succeed())

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			err = clusterController.syncGatewayLogicalNetwork(updatedNode, l3GatewayConfig, []*net.IPNet{subnet}, hostAddrs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Node delete will fail with Failed to delete node node1,
			// error: error deleting node node1 HostSubnet 10.1.1.0/24:
			// error deleting subnet 10.1.1.0/24 for node "node1": network 10.1.1.0/24 does not belong to any known range
			err = fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), testNode.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// check to see if the retry cache has an entry for this node
			checkRetryObjectEventually(testNode.Name, true, clusterController.retryNodes)
			retryEntry, found := getRetryObj(testNode.Name, clusterController.retryNodes)
			gomega.Expect(found).To(gomega.BeTrue())
			ginkgo.By("retry entry new obj should be nil")
			gomega.Expect(retryEntry.newObj).To(gomega.BeNil())
			ginkgo.By("retry entry old obj should not be nil")
			gomega.Expect(retryEntry.oldObj).NotTo(gomega.BeNil())
			// allocate subnet to allow delete to continue
			gomega.Expect(clusterController.masterSubnetAllocator.AddNetworkRange(subnet, 24)).To(gomega.Succeed())
			setRetryObjWithNoBackoff(node1.Name, clusterController.retryNodes)
			clusterController.retryNodes.RequestRetryObjs() // retry the failed entry

			checkRetryObjectEventually(testNode.Name, false, clusterController.retryNodes)
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

			config.Kubernetes.HostNetworkNamespace = ""
			config.Kubernetes.NoHostSubnetNodes = &metav1.LabelSelector{
				MatchLabels: nodeNoHostSubnetAnnotation(),
			}

			expectedClusterLBGroup := newLoadBalancerGroup()
			expectedOVNClusterRouter := newOVNClusterRouter()
			expectedNodeSwitch := node1.logicalSwitch(expectedClusterLBGroup.UUID)
			expectedClusterRouterPortGroup := newRouterPortGroup()
			expectedClusterPortGroup := newClusterPortGroup()

			expectedDatabaseState := []libovsdbtest.TestData{}
			expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, &node1)

			gomega.Expect(
				clusterController.addResource(
					clusterController.retryNodes, &testNode, false)).To(
				gomega.MatchError(
					"nodeAdd: error creating subnet for node node1: error allocating networks for node node1: 1 subnets expected only new 0 subnets allocated"))

			ginkgo.By("annotating the node with no host subnet")
			testNode.Labels = nodeNoHostSubnetAnnotation()
			_, err = fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), &testNode, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("adding the node becomes possible")
			gomega.Expect(clusterController.addResource(clusterController.retryNodes, &testNode, false)).To(gomega.Succeed())

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
	ip, ipNet, _ := net.ParseCIDR(desiredIPAddress + "/16")
	return &net.IPNet{
		IP:   ip,
		Mask: ipNet.Mask,
	}
}

func newClusterJoinSwitch() *nbdb.LogicalSwitch {
	return &nbdb.LogicalSwitch{
		UUID: types.OVNJoinSwitch + "-UUID",
		Name: types.OVNJoinSwitch,
	}
}

func newClusterPortGroup() *nbdb.PortGroup {
	return &nbdb.PortGroup{
		UUID: types.ClusterPortGroupName + "-UUID",
		Name: types.ClusterPortGroupName,
		ExternalIDs: map[string]string{
			"name": types.ClusterPortGroupName,
		},
	}
}

func newRouterPortGroup() *nbdb.PortGroup {
	return &nbdb.PortGroup{
		UUID: types.ClusterRtrPortGroupName + "-UUID",
		Name: types.ClusterRtrPortGroupName,
		ExternalIDs: map[string]string{
			"name": types.ClusterRtrPortGroupName,
		},
	}
}

func newOVNClusterRouter() *nbdb.LogicalRouter {
	return &nbdb.LogicalRouter{
		UUID: types.OVNClusterRouter + "-UUID",
		Name: types.OVNClusterRouter,
	}
}

func newLoadBalancerGroup() *nbdb.LoadBalancerGroup {
	return &nbdb.LoadBalancerGroup{
		Name: types.ClusterLBGroupName,
		UUID: types.ClusterLBGroupName + "-UUID",
	}
}

func TestController_allocateNodeSubnets(t *testing.T) {
	tests := []struct {
		name          string
		networkRanges []string
		networkLen    int
		configIPv4    bool
		configIPv6    bool
		node          *kapi.Node
		// to be converted during the test to []*net.IPNet
		wantStr   []string
		allocated int
		wantErr   bool
	}{
		{
			name:          "new node, IPv4 only cluster",
			networkRanges: []string{"172.16.0.0/16"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    false,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "testnode",
					Annotations: map[string]string{},
				},
			},
			wantStr:   []string{"172.16.0.0/24"},
			allocated: 1,
			wantErr:   false,
		},
		{
			name:          "new node, IPv6 only cluster",
			networkRanges: []string{"2001:db2::/56"},
			networkLen:    64,
			configIPv4:    false,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "testnode",
					Annotations: map[string]string{},
				},
			},
			wantStr:   []string{"2001:db2::/64"},
			allocated: 1,
			wantErr:   false,
		},
		{
			name:          "existing annotated node, IPv4 only cluster",
			networkRanges: []string{"172.16.0.0/16"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    false,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": "172.16.8.0/24"}`,
					},
				},
			},
			wantStr:   []string{"172.16.8.0/24"},
			allocated: 0,
			wantErr:   false,
		},
		{
			name:          "existing annotated node, IPv6 only cluster",
			networkRanges: []string{"2001:db2::/56"},
			networkLen:    64,
			configIPv4:    false,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": "2001:db2:1:2:3:4::/64"}`,
					}},
			},
			wantStr:   []string{"2001:db2:1:2:3:4::/64"},
			allocated: 0,
			wantErr:   false,
		},
		{
			name:          "new node, dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/12"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "testnode",
					Annotations: map[string]string{},
				},
			},
			wantStr:   []string{"172.16.0.0/24", "2000::/24"},
			allocated: 2,
			wantErr:   false,
		},
		{
			name:          "annotated node, dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/12"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": ["172.16.5.0/24","2000:2::/24"]}`,
					},
				},
			},
			wantStr:   []string{"172.16.5.0/24", "2000:2::/24"},
			allocated: 0,
			wantErr:   false,
		},
		{
			name:          "single IPv4 to dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/12"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": "172.16.5.0/24"}`,
					},
				},
			},
			wantStr:   []string{"172.16.5.0/24", "2000::/24"},
			allocated: 1,
			wantErr:   false,
		},
		{
			name:          "single IPv6 to dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000:1::/12"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": "2000:1::/24"}`,
					},
				},
			},
			wantStr:   []string{"2000::/24", "172.16.0.0/24"},
			allocated: 1,
			wantErr:   false,
		},
		{
			name:          "dual stack cluster to single IPv4",
			networkRanges: []string{"172.16.0.0/16"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    false,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": ["172.16.5.0/24","2000:2::/24"]}`,
					},
				},
			},
			wantStr:   []string{"172.16.5.0/24"},
			allocated: 0,
			wantErr:   false,
		},
		{
			name:          "dual stack cluster to single IPv6",
			networkRanges: []string{"2001:db2::/56"},
			networkLen:    64,
			configIPv4:    false,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": ["172.16.5.0/24","2001:db2:1:2:3:4::/64"]}`,
					},
				},
			},
			wantStr:   []string{"2001:db2:1:2:3:4::/64"},
			allocated: 0,
			wantErr:   false,
		},
		{
			name:          "new node, OVN wrong configuration: IPv4 only cluster but IPv6 range",
			networkRanges: []string{"2001:db2::/64"},
			networkLen:    112,
			configIPv4:    true,
			configIPv6:    false,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "testnode",
					Annotations: map[string]string{},
				},
			},
			wantStr:   nil,
			allocated: 0,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// create cluster config
			config.IPv4Mode = tt.configIPv4
			config.IPv6Mode = tt.configIPv6
			// create fake OVN controller
			stopChan := make(chan struct{})
			defer close(stopChan)
			kubeFakeClient := fake.NewSimpleClientset()
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			egressQoSFakeClient := &egressqosfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				EgressQoSClient:      egressQoSFakeClient,
			}
			f, err := factory.NewMasterWatchFactory(fakeClient)
			if err != nil {
				t.Fatalf("Error creating master watch factory: %v", err)
			}
			if err := f.Start(); err != nil {
				t.Fatalf("Error starting master watch factory: %v", err)
			}

			expectedClusterLBGroup := newLoadBalancerGroup()
			dbSetup := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					expectedClusterLBGroup,
				},
			}
			libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
			if err != nil {
				t.Fatalf("Error creating libovsdb test harness %v", err)
			}
			t.Cleanup(libovsdbCleanup.Cleanup)

			cm := NewControllerManager(fakeClient, "", f,
				stopChan, libovsdbOvnNBClient, libovsdbOvnSBClient,
				record.NewFakeRecorder(0), nil)
			clusterController, _ := cm.InitDefaultController(addressset.NewFakeAddressSetFactory())
			clusterController.loadBalancerGroupUUID = expectedClusterLBGroup.UUID

			// configure the cluster allocators
			for _, subnetString := range tt.networkRanges {
				_, subnet, err := net.ParseCIDR(subnetString)
				if err != nil {
					t.Fatalf("Error parsing subnet %s", subnetString)
				}
				clusterController.masterSubnetAllocator.AddNetworkRange(subnet, tt.networkLen)
			}
			// test network allocation works correctly
			got, allocated, err := clusterController.allocateNodeSubnets(tt.node)
			if (err != nil) != tt.wantErr {
				t.Errorf("Controller.addNode() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			var want []*net.IPNet
			for _, netStr := range tt.wantStr {
				_, ipnet, err := net.ParseCIDR(netStr)
				if err != nil {
					t.Fatalf("Error parsing subnet %s", netStr)
				}
				want = append(want, ipnet)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Controller.allocateNodeSubnets() = %v, want %v", got, want)
			}

			if len(allocated) != tt.allocated {
				t.Errorf("Expected %d subnets allocated, received %d", tt.allocated, len(allocated))
			}
		})
	}
}

func TestController_syncNodesRetriable(t *testing.T) {
	tests := []struct {
		name         string
		initialSBDB  []libovsdbtest.TestData
		expectedSBDB []libovsdbtest.TestData
	}{
		{
			name: "removes stale chassis and chassis private",
			initialSBDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: "chassis-node1", Hostname: "node1"},
				&sbdb.ChassisPrivate{Name: "chassis-node1"},
				&sbdb.Chassis{Name: "chassis-node2", Hostname: "node2"},
				&sbdb.ChassisPrivate{Name: "chassis-node2"},
				&sbdb.ChassisPrivate{Name: "chassis-node3"},
			},
			expectedSBDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: "chassis-node1", Hostname: "node1"},
				&sbdb.ChassisPrivate{Name: "chassis-node1"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stopChan := make(chan struct{})
			defer close(stopChan)

			testNode := v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
				},
			}

			kubeFakeClient := fake.NewSimpleClientset()
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
			}
			f, err := factory.NewMasterWatchFactory(fakeClient)
			if err != nil {
				t.Fatalf("%s: Error creating master watch factory: %v", tt.name, err)
			}

			dbSetup := libovsdbtest.TestSetup{
				SBData: tt.initialSBDB,
			}
			nbClient, sbClient, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
			if err != nil {
				t.Fatalf("Error creating libovsdb test harness: %v", err)
			}
			t.Cleanup(libovsdbCleanup.Cleanup)

			cm := NewControllerManager(fakeClient, "", f,
				stopChan, nbClient, sbClient,
				record.NewFakeRecorder(0), nil)
			controller, _ := cm.InitDefaultController(addressset.NewFakeAddressSetFactory())
			controller.joinSwIPManager, err = lsm.NewJoinLogicalSwitchIPManager(nbClient, "", []string{})
			if err != nil {
				t.Fatalf("%s: Error creating joinSwIPManager: %v", tt.name, err)
			}

			err = controller.syncNodesRetriable([]interface{}{&testNode})
			if err != nil {
				t.Fatalf("%s: Error on syncNodesRetriable: %v", tt.name, err)
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

func TestController_deleteStaleNodeChassis(t *testing.T) {
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
			defer close(stopChan)

			kubeFakeClient := fake.NewSimpleClientset()
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
			}
			f, err := factory.NewMasterWatchFactory(fakeClient)
			if err != nil {
				t.Fatalf("%s: Error creating master watch factory: %v", tt.name, err)
			}

			dbSetup := libovsdbtest.TestSetup{
				SBData: tt.initialSBDB,
			}
			nbClient, sbClient, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
			if err != nil {
				t.Fatalf("Error creating libovsdb test harness: %v", err)
			}
			t.Cleanup(libovsdbCleanup.Cleanup)

			cm := NewControllerManager(fakeClient, "", f,
				stopChan, nbClient, sbClient,
				record.NewFakeRecorder(0), nil)
			controller, _ := cm.InitDefaultController(addressset.NewFakeAddressSetFactory())
			controller.joinSwIPManager, err = lsm.NewJoinLogicalSwitchIPManager(nbClient, "", []string{})
			if err != nil {
				t.Fatalf("%s: Error creating joinSwIPManager: %v", tt.name, err)
			}

			err = controller.deleteStaleNodeChassis(&tt.node)
			if err != nil {
				t.Fatalf("%s: Error on syncNodesRetriable: %v", tt.name, err)
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

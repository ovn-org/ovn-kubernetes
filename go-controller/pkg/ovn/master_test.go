package ovn

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	goovn "github.com/ebay/go-ovn"
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
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
	NodeGWIP             string
	NodeMgmtPortIP       string
	NodeMgmtPortMAC      string
	DnatSnatIP           string
}

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

/*
>>>>>>> origin/master
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

func addNodeLogicalFlows(testData []libovsdb.TestData, expectedOVNClusterRouter *nbdb.LogicalRouter, expectedNodeSwitch *nbdb.LogicalSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup *nbdb.PortGroup, fexec *ovntest.FakeExec, node *tNode, clusterCIDR string, enableIPv6 bool) []libovsdb.TestData {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --data=bare --no-heading --format=csv --columns=name,other-config find logical_switch",
	})

	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --may-exist ls-add " + node.Name + " -- set logical_switch " + node.Name + " other-config:subnet=" + node.NodeSubnet + " other-config:exclude_ips=" + node.NodeMgmtPortIP,
	})

	testData = append(testData, &nbdb.LogicalRouterPort{
		Name:     types.RouterToSwitchPrefix + node.Name,
		UUID:     types.RouterToSwitchPrefix + node.Name + "-UUID",
		MAC:      node.NodeLRPMAC,
		Networks: []string{node.NodeGWIP},
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

	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 lsp-list " + node.Name,
		Output: "29df5ce5-2802-4ee5-891f-4fb27ca776e9 (" + types.K8sPrefix + node.Name + ")",
	})

	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 -- --if-exists remove logical_switch " + node.Name + " other-config exclude_ips",
		"ovn-nbctl --timeout=15 --if-exists lrp-del " + types.RouterToSwitchPrefix + node.Name + " -- lrp-add ovn_cluster_router " + types.RouterToSwitchPrefix + node.Name + " " + node.NodeLRPMAC + " " + node.NodeGWIP + " -- lrp-set-gateway-chassis " + types.RouterToSwitchPrefix + node.Name + " " + node.SystemID + " 1",
	})

	matchStr1 := fmt.Sprintf(`inport == "rtos-%s" && ip4.dst == %s /* %s */`, node.Name, node.GatewayRouterIP, node.Name)
	matchStr2 := fmt.Sprintf(`inport == "rtos-%s" && ip4.dst == 9.9.9.9 /* %s */`, node.Name, node.Name)
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

func populatePortAddresses(nodeName, lsp, mac, ips string, ovnClient goovn.Client) {
	cmd, err := ovnClient.LSPAdd(nodeName, "", lsp)
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

/* FIXME for updated local gw
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

			subnetsFromAnnotation, err := util.ParseNodeHostSubnetAnnotation(updatedNode)
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

			subnetsFromAnnotation, err := util.ParseNodeHostSubnetAnnotation(updatedNode)
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

			subnetsFromAnnotation, err := util.ParseNodeHostSubnetAnnotation(updatedNode)
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
		app      *cli.App
		f        *factory.WatchFactory
		stopChan chan struct{}
		wg       *sync.WaitGroup
	)

	const (
		clusterIPNet string = "10.1.0.0"
		clusterCIDR  string = clusterIPNet + "/16"
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

	/* FIXME with update to local gw
	ginkgo.It("sets up a localnet gateway", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName               string = "node1"
				nodeLRPMAC             string = "0a:58:0a:01:01:01"
				lrpMAC                 string = "0a:58:64:40:00:02"
				lrpIP                  string = "100.64.0.2"
				lrpIPv6                string = "fd98::2"
				drLrpIP                string = "100.64.0.1"
				brLocalnetMAC          string = "11:22:33:44:55:66"
				systemID               string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
				tcpLBUUID              string = "d2e858b2-cb5a-441b-a670-ed450f79a91f"
				udpLBUUID              string = "12832f14-eb0f-44d4-b8db-4cccbc73c792"
				sctpLBUUID             string = "0514c521-a120-4756-aec6-883fe5db7139"
				nodeSubnet             string = "10.1.1.0/24"
				nextHop                string = "10.1.1.2"
				gwRouter               string = types.GWRouterPrefix + nodeName
				clusterIPNet           string = "10.1.0.0"
				clusterCIDR            string = clusterIPNet + "/16"
				localnetGatewayIP      string = "169.254.33.2/24"
				localnetGatewayNextHop string = "169.254.33.1"
				localnetBridgeName     string = "br-local"
				masterGWCIDR           string = "10.1.1.1/24"
				masterMgmtPortIP       string = "10.1.1.2"
				node1RouteUUID         string = "0cac12cf-3e0f-4682-b028-5ea2e0001962"
				node1mgtRouteUUID      string = "0cac12cf-3e0f-4682-b028-5ea2e0001963"
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

			fexec := ovntest.NewFakeExec()
			err := util.SetExec(fexec)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient, egressIPFakeClient, egressFirewallFakeClient}, testNode.Name)
			ifaceID := localnetBridgeName + "_" + nodeName
			err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      systemID,
				InterfaceID:    ifaceID,
				MACAddress:     ovntest.MustParseMAC(brLocalnetMAC),
				IPAddresses:    ovntest.MustParseIPNets(localnetGatewayIP),
				NextHops:       ovntest.MustParseIPs(localnetGatewayNextHop),
				NodePortEnable: true,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeManagementPortMACAddress(nodeAnnotator, ovntest.MustParseMAC(brLocalnetMAC))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(nodeSubnet))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_" + nodeName + " networks",
				"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name,other-config find logical_switch",
			})

			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exists lrp-del " + types.RouterToSwitchPrefix + nodeName + " -- lrp-add ovn_cluster_router " + types.RouterToSwitchPrefix + nodeName + " " + nodeLRPMAC + " " + masterGWCIDR,
				"ovn-nbctl --timeout=15 --may-exist ls-add " + nodeName + " -- set logical_switch " + nodeName + " other-config:subnet=" + nodeSubnet + " other-config:exclude_ips=" + masterMgmtPortIP,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " " + types.SwitchToRouterPrefix + nodeName + " -- set logical_switch_port " + types.SwitchToRouterPrefix + nodeName + " type=router options:router-port=" + types.RouterToSwitchPrefix + nodeName + " addresses=\"" + nodeLRPMAC + "\"",
				"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " load_balancer=" + tcpLBUUID,
				"ovn-nbctl --timeout=15 add logical_switch " + nodeName + " load_balancer " + udpLBUUID,
				"ovn-nbctl --timeout=15 add logical_switch " + nodeName + " load_balancer " + sctpLBUUID,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " " + types.K8sPrefix + nodeName + " -- lsp-set-addresses " + types.K8sPrefix + nodeName + " " + brLocalnetMAC + " " + masterMgmtPortIP,
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
				"ovn-nbctl --timeout=15 -- --if-exists remove logical_switch " + nodeName + " other-config exclude_ips",
				"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=169.254.33.2 external_ids:physical_ips=169.254.33.2",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + util.OvnJoinSwitch + " " + types.JoinSwitchToGWRouterPrefix + gwRouter + " -- set logical_switch_port " + types.JoinSwitchToGWRouterPrefix + gwRouter + " type=router options:router-port=" + types.GWRouterToJoinSwitchPrefix + gwRouter + " addresses=router",
				"ovn-nbctl --timeout=15 -- --if-exists lrp-del " + types.GWRouterToJoinSwitchPrefix + gwRouter + " -- lrp-add " + gwRouter + " " + types.GWRouterToJoinSwitchPrefix + gwRouter + " " + lrpMAC + " " + lrpIP + "/16" + " " + lrpIPv6 + "/64",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " " + drLrpIP,
			})
			addNodeportLBs(fexec, nodeName, tcpLBUUID, udpLBUUID, sctpLBUUID)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist ls-add " + types.ExternalSwitchPrefix + nodeName,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + types.ExternalSwitchPrefix + nodeName + " br-local_" + nodeName + " -- lsp-set-addresses br-local_" + nodeName + " unknown -- lsp-set-type br-local_" + nodeName + " localnet -- lsp-set-options br-local_" + nodeName + " network_name=" + types.PhysicalNetworkName,
				"ovn-nbctl --timeout=15 -- --if-exists lrp-del " + types.GWRouterToExtSwitchPrefix + gwRouter + " -- lrp-add " + gwRouter + " " + types.GWRouterToExtSwitchPrefix + gwRouter + " " + brLocalnetMAC + " 169.254.33.2/24 -- set logical_router_port " + types.GWRouterToExtSwitchPrefix + gwRouter + " external-ids:gateway-physical-ip=yes",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + types.ExternalSwitchPrefix + nodeName + " " + types.EXTSwitchToGWRouterPrefix + gwRouter + " -- set logical_switch_port " + types.EXTSwitchToGWRouterPrefix + gwRouter + " type=router options:router-port=" + types.GWRouterToExtSwitchPrefix + gwRouter + " addresses=\"" + brLocalnetMAC + "\"",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 169.254.33.1 " + types.GWRouterToExtSwitchPrefix + gwRouter,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + util.OVNClsuterRouter + " " + lrpIP + " " + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + types.OVNClusterRouter + " " + lrpIPv6 + " " + lrpIPv6,
				"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + types.OVNClusterRouter + " " + nodeSubnet + " " + lrpIP,
				"ovn-nbctl --timeout=15 --if-exists lr-nat-del " + gwRouter + " snat " + clusterCIDR + " -- lr-nat-add " +
					gwRouter + " snat 169.254.33.2 " + clusterCIDR,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_router " + types.GWRouterPrefix + nodeName + " external_ids:physical_ips",
				Output: "169.254.33.2",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=169.254.33.2 external_ids:physical_ips=169.254.33.2",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + types.OVNJoinSwitch + " " + types.JoinSwitchToGWRouterPrefix + gwRouter + " -- set logical_switch_port " + types.JoinSwitchToGWRouterPrefix + gwRouter + " type=router options:router-port=" + types.GWRouterToJoinSwitchPrefix + gwRouter + " addresses=router",
				"ovn-nbctl --timeout=15 -- --if-exists lrp-del " + types.GWRouterToJoinSwitchPrefix + gwRouter + " -- lrp-add " + gwRouter + " " + types.GWRouterToJoinSwitchPrefix + gwRouter + " " + lrpMAC + " " + lrpIP + "/16" + " " + lrpIPv6 + "/64",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " " + drLrpIP,
			})
			addNodeportLBs(fexec, nodeName, tcpLBUUID, udpLBUUID, sctpLBUUID)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist ls-add " + types.ExternalSwitchPrefix + nodeName,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + types.ExternalSwitchPrefix + nodeName + " br-local_" + nodeName + " -- lsp-set-addresses br-local_" + nodeName + " unknown -- lsp-set-type br-local_" + nodeName + " localnet -- lsp-set-options br-local_" + nodeName + " network_name=" + types.PhysicalNetworkName,
				"ovn-nbctl --timeout=15 -- --if-exists lrp-del " + types.GWRouterToExtSwitchPrefix + gwRouter + " -- lrp-add " + gwRouter + " " + types.GWRouterToExtSwitchPrefix + gwRouter + " " + brLocalnetMAC + " 169.254.33.2/24 -- set logical_router_port " + types.GWRouterToExtSwitchPrefix + gwRouter + " external-ids:gateway-physical-ip=yes",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + types.ExternalSwitchPrefix + nodeName + " " + types.EXTSwitchToGWRouterPrefix + gwRouter + " -- set logical_switch_port " + types.EXTSwitchToGWRouterPrefix + gwRouter + " type=router options:router-port=" + types.GWRouterToExtSwitchPrefix + gwRouter + " addresses=\"" + brLocalnetMAC + "\"",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 169.254.33.1 " + types.GWRouterToExtSwitchPrefix + gwRouter,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + types.OVNClusterRouter + " " + lrpIP + " " + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + types.OVNClusterRouter + " " + lrpIPv6 + " " + lrpIPv6,
				"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + types.OVNClusterRouter + " " + nodeSubnet + " " + lrpIP,
				"ovn-nbctl --timeout=15 --if-exists lr-nat-del " + gwRouter + " snat " + clusterCIDR +
					" -- lr-nat-add " + gwRouter + " snat 169.254.33.2 " + clusterCIDR,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_router " + types.GWRouterPrefix + nodeName + " external_ids:physical_ips",
				Output: "169.254.33.2",
			})

			f, err = factory.NewMasterWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			clusterController := NewOvnController(fakeClient, f, stopChan, newFakeAddressSetFactory(),
				ovntest.NewMockOVNClient(goovn.DBNB),
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

			subnet := ovntest.MustParseIPNet(nodeSubnet)
			err = clusterController.syncGatewayLogicalNetwork(updatedNode, l3GatewayConfig, []*net.IPNet{subnet})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
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
	*/

	ginkgo.It("sets up a shared gateway", func() {

		app.Action = func(ctx *cli.Context) error {
			node1 := tNode{
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
				NodeGWIP:             "10.1.1.1/24",
				NodeMgmtPortIP:       "10.1.1.2",
				NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
				DnatSnatIP:           "169.254.0.1",
			}

			testNode := v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: node1.Name,
				},
				Status: kapi.NodeStatus{
					Addresses: []kapi.NodeAddress{
						{
							Type:    kapi.NodeExternalIP,
							Address: node1.NodeIP,
						},
					},
				},
			}

			kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
			}

			fexec := ovntest.NewLooseCompareFakeExec()
			err := util.SetExec(fexec)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.Kubernetes.HostNetworkNamespace = ""
			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{kubeFakeClient, egressIPFakeClient, egressFirewallFakeClient}, testNode.Name)
			ifaceID := node1.PhysicalBridgeName + "_" + node1.Name
			vlanID := uint(1024)
			l3Config := &util.L3GatewayConfig{
				Mode:           config.GatewayModeShared,
				ChassisID:      node1.SystemID,
				InterfaceID:    ifaceID,
				MACAddress:     ovntest.MustParseMAC(node1.PhysicalBridgeMAC),
				IPAddresses:    ovntest.MustParseIPNets(node1.GatewayRouterIPMask),
				NextHops:       ovntest.MustParseIPs(node1.GatewayRouterNextHop),
				NodePortEnable: true,
				VLANID:         &vlanID,
			}
			err = util.SetL3GatewayConfig(nodeAnnotator, l3Config)
			err = util.SetNodeManagementPortMACAddress(nodeAnnotator, ovntest.MustParseMAC(node1.NodeMgmtPortMAC))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeLocalNatAnnotation(nodeAnnotator, []net.IP{ovntest.MustParseIP(node1.DnatSnatIP)})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = util.SetNodeHostAddresses(nodeAnnotator, sets.NewString("9.9.9.9"))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = nodeAnnotator.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), testNode.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostAddrs, err := util.ParseNodeHostAddresses(updatedNode)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			f, err = factory.NewMasterWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = f.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID: types.OVNClusterRouter + "-UUID",
				Name: types.OVNClusterRouter,
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: node1.Name + "-UUID",
				Name: node1.Name,
			}
			expectedClusterRouterPortGroup := &nbdb.PortGroup{
				UUID: types.ClusterRtrPortGroupName + "-UUID",
				Name: types.ClusterRtrPortGroupName,
				ExternalIDs: map[string]string{
					"name": types.ClusterRtrPortGroupName,
				},
			}
			expectedClusterPortGroup := &nbdb.PortGroup{
				UUID: types.ClusterPortGroupName + "-UUID",
				Name: types.ClusterPortGroupName,
				ExternalIDs: map[string]string{
					"name": types.ClusterPortGroupName,
				},
			}
			dbSetup := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					expectedOVNClusterRouter,
					expectedNodeSwitch,
					expectedClusterRouterPortGroup,
					expectedClusterPortGroup,
				},
			}
			libovsdbOvnNBClient, libovsdbOvnSBClient, err := libovsdbtest.NewNBSBTestHarness(dbSetup, stopChan)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedDatabaseState := []libovsdb.TestData{}
			expectedDatabaseState = addNodeLogicalFlows(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, expectedClusterRouterPortGroup, expectedClusterPortGroup, fexec, &node1, clusterCIDR, config.IPv6Mode)

			clusterController := NewOvnController(fakeClient, f, stopChan, addressset.NewFakeAddressSetFactory(),
				ovntest.NewMockOVNClient(goovn.DBNB), ovntest.NewMockOVNClient(goovn.DBSB),
				libovsdbOvnNBClient, libovsdbOvnSBClient,
				record.NewFakeRecorder(0))
			gomega.Expect(clusterController).NotTo(gomega.BeNil())

			clusterController.SCTPSupport = true
			clusterController.joinSwIPManager, _ = lsm.NewJoinLogicalSwitchIPManager(clusterController.nbClient, []string{node1.Name})
			_, _ = clusterController.joinSwIPManager.EnsureJoinLRPIPs(types.OVNClusterRouter)

			clusterController.nodeLocalNatIPv4Allocator, _ = ipallocator.NewCIDRRange(ovntest.MustParseIPNet(types.V4NodeLocalNATSubnet))

			// Let the real code run and ensure OVN database sync
			clusterController.WatchNodes()

			clusterController.StartServiceController(wg, false)

			subnet := ovntest.MustParseIPNet(node1.NodeSubnet)
			err = clusterController.syncGatewayLogicalNetwork(updatedNode, l3GatewayConfig, []*net.IPNet{subnet}, hostAddrs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var clusterSubnets []*net.IPNet
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
			}
			joinLRPIP, joinLRNetwork, _ := net.ParseCIDR(node1.LrpIP + "/16")
			dLRPIP, dLRPNetwork, _ := net.ParseCIDR(node1.DrLrpIP + "/16")

			joinLRPIPs := &net.IPNet{
				IP:   joinLRPIP,
				Mask: joinLRNetwork.Mask,
			}
			dLRPIPs := &net.IPNet{
				IP:   dLRPIP,
				Mask: dLRPNetwork.Mask,
			}

			skipSnat := false
			expectedDatabaseState = generateGatewayInitExpectedNB(expectedDatabaseState, expectedOVNClusterRouter, expectedNodeSwitch, node1.Name, clusterSubnets, []*net.IPNet{subnet}, l3Config, []*net.IPNet{joinLRPIPs}, []*net.IPNet{dLRPIPs}, skipSnat)
			gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

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
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
			}
			f, err := factory.NewMasterWatchFactory(fakeClient)
			if err != nil {
				t.Fatalf("Error creating master watch factory: %v", err)
			}
			if err := f.Start(); err != nil {
				t.Fatalf("Error starting master watch factory: %v", err)
			}

			dbSetup := libovsdbtest.TestSetup{}
			libovsdbOvnNBClient, libovsdbOvnSBClient, err := libovsdbtest.NewNBSBTestHarness(dbSetup, stopChan)
			if err != nil {
				t.Fatalf("Error creating libovsdb test harness %v", err)
			}

			clusterController := NewOvnController(fakeClient, f, stopChan, addressset.NewFakeAddressSetFactory(),
				ovntest.NewMockOVNClient(goovn.DBNB), ovntest.NewMockOVNClient(goovn.DBSB),
				libovsdbOvnNBClient, libovsdbOvnSBClient,
				record.NewFakeRecorder(0))

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

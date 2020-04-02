package ovn

import (
	"fmt"
	"net"

	"github.com/urfave/cli"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kuberuntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// Please use following subnets for various networks that we have
// 172.16.1.0/24 -- physical network that k8s nodes connect to
// 100.64.0.0/16 -- the join subnet that connects all the L3 gateways with the distributed router
// 169.254.33.0/24 -- the subnet that connects OVN logical network to physical network
// 10.1.0.0/16 -- the overlay subnet that Pods connect to.

func cleanupGateway(fexec *ovntest.FakeExec, nodeName string, nodeSubnet string, clusterCIDR string, nextHop string) {
	const (
		clusterRouter     string = util.OvnClusterRouter
		node1RouteUUID    string = "0cac12cf-3e0f-4682-b028-5ea2e0001962"
		node1mgtRouteUUID string = "0cac12cf-3e0f-4682-b028-5ea2e0001963"
	)

	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-" + util.GWRouterPrefix + nodeName + " networks",
		Output: "[\"100.64.0.1/29\"]",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=\"100.64.0.1\"",
		Output: node1RouteUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove logical_router " + clusterRouter + " static_routes " + node1RouteUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=\"" + nextHop + "\"",
		Output: node1mgtRouteUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove logical_router " + clusterRouter + " static_routes " + node1mgtRouteUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exist ls-del " + util.JoinSwitchPrefix + nodeName,
		"ovn-nbctl --timeout=15 --if-exist lr-del " + util.GWRouterPrefix + nodeName,
		"ovn-nbctl --timeout=15 --if-exist ls-del " + util.ExternalSwitchPrefix + nodeName,
		"ovn-nbctl --timeout=15 --if-exist lrp-del dtoj-" + nodeName,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=" + util.GWRouterPrefix + nodeName,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=" + util.GWRouterPrefix + nodeName,
		Output: "",
	})
}

func defaultFakeExec(nodeSubnet, nodeName string) (*ovntest.FakeExec, string, string) {
	const (
		tcpLBUUID string = "1a3dfc82-2749-4931-9190-c30e7c0ecea3"
		udpLBUUID string = "6d3142fc-53e8-4ac1-88e6-46094a5a9957"
		mgmtMAC   string = "01:02:03:04:05:06"
	)

	fexec := ovntest.NewFakeExec()
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --columns=_uuid list port_group",
		"ovn-sbctl --timeout=15 --columns=_uuid list IGMP_Group",
		"ovn-nbctl --timeout=15 -- --may-exist lr-add ovn_cluster_router -- set logical_router ovn_cluster_router external_ids:k8s-cluster-router=yes",
		"ovn-nbctl --timeout=15 -- set logical_router ovn_cluster_router options:mcast_relay=\"true\"",
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find port_group name=mcastPortGroupDeny",
		"ovn-nbctl --timeout=15 create port_group name=mcastPortGroupDeny external-ids:name=mcastPortGroupDeny",
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"inport == @mcastPortGroupDeny && ip4.mcast\" action=drop external-ids:default-deny-policy-type=Egress",
		"ovn-nbctl --timeout=15 --id=@acl create acl priority=1011 direction=from-lport match=\"inport == @mcastPortGroupDeny && ip4.mcast\" action=drop external-ids:default-deny-policy-type=Egress -- add port_group  acls @acl",
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"outport == @mcastPortGroupDeny && ip4.mcast\" action=drop external-ids:default-deny-policy-type=Ingress",
		"ovn-nbctl --timeout=15 --id=@acl create acl priority=1011 direction=to-lport match=\"outport == @mcastPortGroupDeny && ip4.mcast\" action=drop external-ids:default-deny-policy-type=Ingress -- add port_group  acls @acl",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 -- create load_balancer external_ids:k8s-cluster-lb-tcp=yes protocol=tcp",
		Output: tcpLBUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-udp=yes",
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 -- create load_balancer external_ids:k8s-cluster-lb-udp=yes protocol=udp",
		Output: udpLBUUID,
	})

	// Node-related logical network stuff
	ip, cidr, err := net.ParseCIDR(nodeSubnet)
	Expect(err).NotTo(HaveOccurred())
	cidr.IP = util.NextIP(ip)
	lrpMAC := util.IPAddrToHWAddr(cidr.IP)
	gwCIDR := cidr.String()
	gwIP := cidr.IP.String()
	nodeMgmtPortIP := util.NextIP(cidr.IP)
	hybridOverlayIP := util.NextIP(nodeMgmtPortIP)

	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-sbctl --timeout=15 --data=bare --no-heading --columns=name,hostname --format=json list Chassis",
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name,other-config find logical_switch other-config:subnet!=_",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --may-exist lrp-add ovn_cluster_router rtos-" + nodeName + " " + lrpMAC + " " + gwCIDR,
		"ovn-nbctl --timeout=15 -- --may-exist ls-add " + nodeName + " -- set logical_switch " + nodeName + " other-config:subnet=" + nodeSubnet + " other-config:exclude_ips=" + nodeMgmtPortIP.String() + ".." + hybridOverlayIP.String(),
		"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " other-config:mcast_snoop=\"true\"",
		"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " other-config:mcast_querier=\"true\" other-config:mcast_eth_src=\"" + lrpMAC + "\" other-config:mcast_ip4_src=\"" + gwIP + "\"",
		"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " stor-" + nodeName + " -- set logical_switch_port stor-" + nodeName + " type=router options:router-port=rtos-" + nodeName + " addresses=\"" + lrpMAC + "\"",
		"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " load_balancer=" + tcpLBUUID,
		"ovn-nbctl --timeout=15 add logical_switch " + nodeName + " load_balancer " + udpLBUUID,
		"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " k8s-" + nodeName + " -- lsp-set-addresses " + "k8s-" + nodeName + " " + mgmtMAC + " " + nodeMgmtPortIP.String(),
		"ovn-nbctl --timeout=15 --may-exist acl-add " + nodeName + " to-lport 1001 ip4.src==" + nodeMgmtPortIP.String() + " allow-related",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 lsp-list " + nodeName,
		Output: "29df5ce5-2802-4ee5-891f-4fb27ca776e9 (k8s-" + nodeName + ")",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 -- --if-exists set logical_switch " + nodeName + " other-config:exclude_ips=" + hybridOverlayIP.String(),
	})

	return fexec, tcpLBUUID, udpLBUUID
}

func addNodeportLBs(fexec *ovntest.FakeExec, nodeName, tcpLBUUID, udpLBUUID string) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=" + util.GWRouterPrefix + nodeName,
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=" + util.GWRouterPrefix + nodeName,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 -- create load_balancer external_ids:TCP_lb_gateway_router=" + util.GWRouterPrefix + nodeName + " protocol=tcp",
		Output: tcpLBUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 -- create load_balancer external_ids:UDP_lb_gateway_router=" + util.GWRouterPrefix + nodeName + " protocol=udp",
		Output: udpLBUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 set logical_router " + util.GWRouterPrefix + nodeName + " load_balancer=" + tcpLBUUID,
		"ovn-nbctl --timeout=15 add logical_router " + util.GWRouterPrefix + nodeName + " load_balancer " + udpLBUUID,
	})
}

var _ = Describe("Master Operations", func() {
	var app *cli.App

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	It("creates logical network elements for a 2-node cluster", func() {
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
			)

			fexec, tcpLBUUID, udpLBUUID := defaultFakeExec(nodeSubnet, nodeName)
			cleanupGateway(fexec, nodeName, nodeSubnet, clusterCIDR, nextHop)

			testNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			}}

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{fakeClient}, &testNode)
			err = util.SetDisabledL3GatewayConfig(nodeAnnotator)
			Expect(err).NotTo(HaveOccurred())
			err = util.SetNodeManagementPortMacAddr(nodeAnnotator, mgmtMAC)
			Expect(err).NotTo(HaveOccurred())
			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer close(stopChan)

			clusterController := NewOvnController(fakeClient, f, stopChan)
			Expect(clusterController).NotTo(BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID

			err = clusterController.StartClusterMaster("master")
			Expect(err).NotTo(HaveOccurred())

			err = clusterController.WatchNodes()
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
			updatedNode, err := fakeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			subnetFromAnnotation, err := util.ParseNodeHostSubnetAnnotation(updatedNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(subnetFromAnnotation.String()).To(Equal(nodeSubnet))

			macFromAnnotation, err := util.ParseNodeManagementPortMacAddr(updatedNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(macFromAnnotation).To(Equal(mgmtMAC))

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-enable-multicast",
			"-enable-hybrid-overlay",
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("does not allocate a hostsubnet for a node that already has one", func() {
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
			)

			testNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			}}

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})

			fexec, tcpLBUUID, udpLBUUID := defaultFakeExec(nodeSubnet, nodeName)
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			cleanupGateway(fexec, nodeName, nodeSubnet, clusterCIDR, nextHop)

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{fakeClient}, &testNode)
			err = util.SetDisabledL3GatewayConfig(nodeAnnotator)
			Expect(err).NotTo(HaveOccurred())
			err = util.SetNodeManagementPortMacAddr(nodeAnnotator, mgmtMAC)
			Expect(err).NotTo(HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, nodeSubnet)
			Expect(err).NotTo(HaveOccurred())
			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer close(stopChan)

			fakeClient.PrependReactor("patch", "nodes", func(action kubetesting.Action) (bool, kuberuntime.Object, error) {
				// Should not be called again as the node already has a subnet
				Expect(true).To(BeFalse())
				return true, nil, fmt.Errorf("should not be called")
			})

			clusterController := NewOvnController(fakeClient, f, stopChan)
			Expect(clusterController).NotTo(BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID

			err = clusterController.StartClusterMaster("master")
			Expect(err).NotTo(HaveOccurred())

			err = clusterController.WatchNodes()
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
			updatedNode, err := fakeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			subnetFromAnnotation, err := util.ParseNodeHostSubnetAnnotation(updatedNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(subnetFromAnnotation.String()).To(Equal(nodeSubnet))

			macFromAnnotation, err := util.ParseNodeManagementPortMacAddr(updatedNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(macFromAnnotation).To(Equal(mgmtMAC))

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-enable-multicast",
			"-enable-hybrid-overlay",
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("removes deleted nodes from the OVN database", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				tcpLBUUID         string = "1a3dfc82-2749-4931-9190-c30e7c0ecea3"
				udpLBUUID         string = "6d3142fc-53e8-4ac1-88e6-46094a5a9957"
				clusterRouter     string = util.OvnClusterRouter
				node1Name         string = "openshift-node-1"
				node1Subnet       string = "10.128.0.0/24"
				node1RouteUUID    string = "0cac12cf-3e0f-4682-b028-5ea2e0001962"
				node1mgtRouteUUID string = "0cac12cf-3e0f-4682-b028-5ea2e0001963"
				masterName        string = "openshift-master-node"
				masterSubnet      string = "10.128.2.0/24"
				masterGWCIDR      string = "10.128.2.1/24"
				masterMgmtPortIP  string = "10.128.2.2"
				lrpMAC            string = "0A:58:0A:80:02:01"
				masterMgmtPortMAC string = "00:00:00:55:66:77"
			)

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-sbctl --timeout=15 --data=bare --no-heading --columns=name,hostname --format=json list Chassis",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name,other-config find logical_switch other-config:subnet!=_",
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
				"ovn-nbctl --timeout=15 --if-exist lrp-del rtos-" + node1Name,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-" + util.GWRouterPrefix + node1Name + " networks",
				Output: "[\"100.64.0.1/29\"]",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=\"100.64.0.1\"",
				Output: node1RouteUUID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exists remove logical_router " + clusterRouter + " static_routes " + node1RouteUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=\"10.128.0.2\"",
				Output: node1mgtRouteUUID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exists remove logical_router " + clusterRouter + " static_routes " + node1mgtRouteUUID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exist ls-del " + util.JoinSwitchPrefix + node1Name,
				"ovn-nbctl --timeout=15 --if-exist lr-del " + util.GWRouterPrefix + node1Name,
				"ovn-nbctl --timeout=15 --if-exist ls-del " + util.ExternalSwitchPrefix + node1Name,
				"ovn-nbctl --timeout=15 --if-exist lrp-del dtoj-" + node1Name,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=" + util.GWRouterPrefix + node1Name,
				Output: "",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=" + util.GWRouterPrefix + node1Name,
				Output: "",
			})

			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-sbctl --timeout=15 --data=bare --no-heading --columns=name find Chassis hostname=" + node1Name,
			})

			// Expect the code to re-add the master node (which still exists)
			// when the factory watch begins and enumerates all existing
			// Kubernetes API nodes
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lrp-add ovn_cluster_router rtos-" + masterName + " " + lrpMAC + " " + masterGWCIDR,
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + masterName + " -- set logical_switch " + masterName + " other-config:subnet=" + masterSubnet + " other-config:exclude_ips=" + masterMgmtPortIP,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + masterName + " stor-" + masterName + " -- set logical_switch_port stor-" + masterName + " type=router options:router-port=rtos-" + masterName + " addresses=\"" + lrpMAC + "\"",
				"ovn-nbctl --timeout=15 set logical_switch " + masterName + " load_balancer=" + tcpLBUUID,
				"ovn-nbctl --timeout=15 add logical_switch " + masterName + " load_balancer " + udpLBUUID,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + masterName + " k8s-" + masterName + " -- lsp-set-addresses " + "k8s-" + masterName + " " + masterMgmtPortMAC + " " + masterMgmtPortIP,
				"ovn-nbctl --timeout=15 --may-exist acl-add " + masterName + " to-lport 1001 ip4.src==" + masterMgmtPortIP + " allow-related",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 lsp-list " + masterName,
				Output: "29df5ce5-2802-4ee5-891f-4fb27ca776e9 (k8s-" + masterName + ")",
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
			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{masterNode},
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{fakeClient}, &masterNode)
			err = util.SetDisabledL3GatewayConfig(nodeAnnotator)
			Expect(err).NotTo(HaveOccurred())
			err = util.SetNodeManagementPortMacAddr(nodeAnnotator, masterMgmtPortMAC)
			Expect(err).NotTo(HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, masterSubnet)
			Expect(err).NotTo(HaveOccurred())
			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer close(stopChan)

			clusterController := NewOvnController(fakeClient, f, stopChan)
			Expect(clusterController).NotTo(BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID
			_ = clusterController.joinSubnetAllocator.AddNetworkRange("100.64.0.0/16", 3)

			// Let the real code run and ensure OVN database sync
			err = clusterController.WatchNodes()
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)

			node, err := fakeClient.CoreV1().Nodes().Get(masterNode.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(node.Status.Conditions)).To(BeIdenticalTo(1))
			Expect(node.Status.Conditions[0].Message).To(BeIdenticalTo("ovn-kube cleared kubelet-set NoRouteCreated"))

			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("Gateway Init Operations", func() {
	var app *cli.App

	const (
		clusterIPNet string = "10.1.0.0"
		clusterCIDR  string = clusterIPNet + "/16"
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	It("sets up a localnet gateway", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName               string = "node1"
				nodeLRPMAC             string = "0A:58:0A:01:01:01"
				joinSubnet             string = "100.64.0.0/29"
				lrpMAC                 string = "0A:58:64:40:00:01"
				lrpIP                  string = "100.64.0.1"
				drLrpMAC               string = "0A:58:64:40:00:02"
				drLrpIP                string = "100.64.0.2"
				brLocalnetMAC          string = "11:22:33:44:55:66"
				clusterRouter          string = util.OvnClusterRouter
				systemID               string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
				tcpLBUUID              string = "d2e858b2-cb5a-441b-a670-ed450f79a91f"
				udpLBUUID              string = "12832f14-eb0f-44d4-b8db-4cccbc73c792"
				nodeSubnet             string = "10.1.1.0/24"
				nextHop                string = "10.1.1.2"
				gwRouter               string = util.GWRouterPrefix + nodeName
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

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})

			fexec := ovntest.NewFakeExec()
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
				Output: systemID,
			})

			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{fakeClient}, &testNode)
			ifaceID := localnetBridgeName + "_" + nodeName
			err = util.SetLocalL3GatewayConfig(nodeAnnotator, ifaceID,
				brLocalnetMAC, localnetGatewayIP, localnetGatewayNextHop, true)
			Expect(err).NotTo(HaveOccurred())
			err = util.SetNodeManagementPortMacAddr(nodeAnnotator, brLocalnetMAC)
			Expect(err).NotTo(HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, nodeSubnet)
			Expect(err).NotTo(HaveOccurred())
			err = util.SetNodeJoinSubnetAnnotation(nodeAnnotator, joinSubnet)
			Expect(err).NotTo(HaveOccurred())
			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			updatedNode, err := fakeClient.CoreV1().Nodes().Get(testNode.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			Expect(err).NotTo(HaveOccurred())

			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-sbctl --timeout=15 --data=bare --no-heading --columns=name,hostname --format=json list Chassis",
				"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name,other-config find logical_switch other-config:subnet!=_",
			})

			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lrp-add ovn_cluster_router rtos-" + nodeName + " " + nodeLRPMAC + " " + masterGWCIDR,
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + nodeName + " -- set logical_switch " + nodeName + " other-config:subnet=" + nodeSubnet + " other-config:exclude_ips=" + masterMgmtPortIP,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " stor-" + nodeName + " -- set logical_switch_port stor-" + nodeName + " type=router options:router-port=rtos-" + nodeName + " addresses=\"" + nodeLRPMAC + "\"",
				"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " load_balancer=" + tcpLBUUID,
				"ovn-nbctl --timeout=15 add logical_switch " + nodeName + " load_balancer " + udpLBUUID,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " k8s-" + nodeName + " -- lsp-set-addresses " + "k8s-" + nodeName + " " + brLocalnetMAC + " " + masterMgmtPortIP,
				"ovn-nbctl --timeout=15 --may-exist acl-add " + nodeName + " to-lport 1001 ip4.src==" + masterMgmtPortIP + " allow-related",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 lsp-list " + nodeName,
				Output: "29df5ce5-2802-4ee5-891f-4fb27ca776e9 (k8s-" + nodeName + ")",
			})
			joinSwitch := util.JoinSwitchPrefix + nodeName
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --if-exists remove logical_switch " + nodeName + " other-config exclude_ips",
				"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=169.254.33.2",
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + joinSwitch,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + joinSwitch + " jtor-" + gwRouter + " -- set logical_switch_port jtor-" + gwRouter + " type=router options:router-port=rtoj-" + gwRouter + " addresses=router",
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add " + gwRouter + " rtoj-" + gwRouter + " " + lrpMAC + " " + lrpIP + "/29",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + joinSwitch + " jtod-" + nodeName + " -- set logical_switch_port jtod-" + nodeName + " type=router options:router-port=dtoj-" + nodeName + " addresses=router",
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add " + clusterRouter + " dtoj-" + nodeName + " " + drLrpMAC + " " + drLrpIP + "/29",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " " + drLrpIP,
			})
			addNodeportLBs(fexec, nodeName, tcpLBUUID, udpLBUUID)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist ls-add " + util.ExternalSwitchPrefix + nodeName,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + util.ExternalSwitchPrefix + nodeName + " br-local_" + nodeName + " -- lsp-set-addresses br-local_" + nodeName + " unknown -- lsp-set-type br-local_" + nodeName + " localnet -- lsp-set-options br-local_" + nodeName + " network_name=" + util.PhysicalNetworkName,
				"ovn-nbctl --timeout=15 -- --if-exists lrp-del rtoe-" + gwRouter + " -- lrp-add " + gwRouter + " rtoe-" + gwRouter + " " + brLocalnetMAC + " 169.254.33.2/24 -- set logical_router_port rtoe-" + gwRouter + " external-ids:gateway-physical-ip=yes",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + util.ExternalSwitchPrefix + nodeName + " etor-" + gwRouter + " -- set logical_switch_port etor-" + gwRouter + " type=router options:router-port=rtoe-" + gwRouter + " addresses=\"" + brLocalnetMAC + "\"",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 169.254.33.1 rtoe-" + gwRouter,
				"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + clusterRouter + " " + nodeSubnet + " " + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-nat-add " + gwRouter + " snat 169.254.33.2 " + clusterCIDR,
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=" + util.GWRouterPrefix + nodeName,
				Output: tcpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=" + util.GWRouterPrefix + nodeName,
				Output: udpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_router " + util.GWRouterPrefix + nodeName + " external_ids:physical_ip",
				Output: "169.254.33.2",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=169.254.33.2",
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + joinSwitch,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + joinSwitch + " jtor-" + gwRouter + " -- set logical_switch_port jtor-" + gwRouter + " type=router options:router-port=rtoj-" + gwRouter + " addresses=router",
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add " + gwRouter + " rtoj-" + gwRouter + " " + lrpMAC + " " + lrpIP + "/29",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + joinSwitch + " jtod-" + nodeName + " -- set logical_switch_port jtod-" + nodeName + " type=router options:router-port=dtoj-" + nodeName + " addresses=router",
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add " + clusterRouter + " dtoj-" + nodeName + " " + drLrpMAC + " " + drLrpIP + "/29",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " " + drLrpIP,
			})
			addNodeportLBs(fexec, nodeName, tcpLBUUID, udpLBUUID)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist ls-add " + util.ExternalSwitchPrefix + nodeName,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + util.ExternalSwitchPrefix + nodeName + " br-local_" + nodeName + " -- lsp-set-addresses br-local_" + nodeName + " unknown -- lsp-set-type br-local_" + nodeName + " localnet -- lsp-set-options br-local_" + nodeName + " network_name=" + util.PhysicalNetworkName,
				"ovn-nbctl --timeout=15 -- --if-exists lrp-del rtoe-" + gwRouter + " -- lrp-add " + gwRouter + " rtoe-" + gwRouter + " " + brLocalnetMAC + " 169.254.33.2/24 -- set logical_router_port rtoe-" + gwRouter + " external-ids:gateway-physical-ip=yes",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + util.ExternalSwitchPrefix + nodeName + " etor-" + gwRouter + " -- set logical_switch_port etor-" + gwRouter + " type=router options:router-port=rtoe-" + gwRouter + " addresses=\"" + brLocalnetMAC + "\"",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 169.254.33.1 rtoe-" + gwRouter,
				"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + clusterRouter + " " + nodeSubnet + " " + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-nat-add " + gwRouter + " snat 169.254.33.2 " + clusterCIDR,
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=" + util.GWRouterPrefix + nodeName,
				Output: tcpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=" + util.GWRouterPrefix + nodeName,
				Output: udpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_router " + util.GWRouterPrefix + nodeName + " external_ids:physical_ip",
				Output: "169.254.33.2",
			})

			stop := make(chan struct{})
			wf, err := factory.NewWatchFactory(fakeClient, stop)
			Expect(err).NotTo(HaveOccurred())
			defer close(stop)

			clusterController := NewOvnController(fakeClient, wf, stop)
			Expect(clusterController).NotTo(BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID
			_ = clusterController.joinSubnetAllocator.AddNetworkRange("100.64.0.0/16", 3)

			// Let the real code run and ensure OVN database sync
			err = clusterController.WatchNodes()
			Expect(err).NotTo(HaveOccurred())

			_, subnet, err := net.ParseCIDR(nodeSubnet)
			Expect(err).NotTo(HaveOccurred())
			err = clusterController.syncGatewayLogicalNetwork(updatedNode, l3GatewayConfig, subnet.String())
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--gateway-local",
			"--nodeport",
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sets up a shared gateway", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName               string = "node1"
				nodeLRPMAC             string = "0A:58:0A:01:01:01"
				joinSubnet             string = "100.64.0.0/29"
				lrpMAC                 string = "0A:58:64:40:00:01"
				lrpIP                  string = "100.64.0.1"
				drLrpMAC               string = "0A:58:64:40:00:02"
				drLrpIP                string = "100.64.0.2"
				physicalBridgeMAC      string = "11:22:33:44:55:66"
				lrpCIDR                string = lrpIP + "/16"
				clusterRouter          string = util.OvnClusterRouter
				systemID               string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
				tcpLBUUID              string = "d2e858b2-cb5a-441b-a670-ed450f79a91f"
				udpLBUUID              string = "12832f14-eb0f-44d4-b8db-4cccbc73c792"
				nodeSubnet             string = "10.1.1.0/24"
				gwRouter               string = util.GWRouterPrefix + nodeName
				clusterIPNet           string = "10.1.0.0"
				clusterCIDR            string = clusterIPNet + "/16"
				nodeIPMask             string = "172.16.16.2/32"
				physicalGatewayIPMask  string = "172.16.16.2/24"
				physicalGatewayIP      string = "172.16.16.2"
				physicalGatewayNextHop string = "172.16.16.1"
				physicalBridgeName     string = "br-eth0"
				nodeGWIP               string = "10.1.1.1/24"
				nodeMgmtPortIP         string = "10.1.1.2"
				nodeMgmtPortMAC        string = "0A:58:0A:01:01:02"
			)

			testNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			}}

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})

			fexec := ovntest.NewFakeExec()
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
				Output: systemID,
			})

			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{fakeClient}, &testNode)
			ifaceID := physicalBridgeName + "_" + nodeName
			err = util.SetSharedL3GatewayConfig(nodeAnnotator, ifaceID,
				physicalBridgeMAC, physicalGatewayIPMask, physicalGatewayNextHop,
				true, 1024)
			err = util.SetNodeManagementPortMacAddr(nodeAnnotator, nodeMgmtPortMAC)
			Expect(err).NotTo(HaveOccurred())
			err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, nodeSubnet)
			Expect(err).NotTo(HaveOccurred())
			err = util.SetNodeJoinSubnetAnnotation(nodeAnnotator, joinSubnet)
			Expect(err).NotTo(HaveOccurred())
			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			updatedNode, err := fakeClient.CoreV1().Nodes().Get(testNode.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
			Expect(err).NotTo(HaveOccurred())

			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-sbctl --timeout=15 --data=bare --no-heading --columns=name,hostname --format=json list Chassis",
				"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name,other-config find logical_switch other-config:subnet!=_",
			})

			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lrp-add ovn_cluster_router rtos-" + nodeName + " " + nodeLRPMAC + " " + nodeGWIP,
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + nodeName + " -- set logical_switch " + nodeName + " other-config:subnet=" + nodeSubnet + " other-config:exclude_ips=" + nodeMgmtPortIP,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " stor-" + nodeName + " -- set logical_switch_port stor-" + nodeName + " type=router options:router-port=rtos-" + nodeName + " addresses=\"" + nodeLRPMAC + "\"",
				"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " load_balancer=" + tcpLBUUID,
				"ovn-nbctl --timeout=15 add logical_switch " + nodeName + " load_balancer " + udpLBUUID,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " k8s-" + nodeName + " -- lsp-set-addresses " + "k8s-" + nodeName + " " + nodeMgmtPortMAC + " " + nodeMgmtPortIP,
				"ovn-nbctl --timeout=15 --may-exist acl-add " + nodeName + " to-lport 1001 ip4.src==" + nodeMgmtPortIP + " allow-related",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 lsp-list " + nodeName,
				Output: "29df5ce5-2802-4ee5-891f-4fb27ca776e9 (k8s-" + nodeName + ")",
			})
			joinSwitch := util.JoinSwitchPrefix + nodeName
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --if-exists remove logical_switch " + nodeName + " other-config exclude_ips",
				"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=" + physicalGatewayIP,
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + joinSwitch,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + joinSwitch + " jtor-" + gwRouter + " -- set logical_switch_port jtor-" + gwRouter + " type=router options:router-port=rtoj-" + gwRouter + " addresses=router",
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add " + gwRouter + " rtoj-" + gwRouter + " " + lrpMAC + " " + lrpIP + "/29",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + joinSwitch + " jtod-" + nodeName + " -- set logical_switch_port jtod-" + nodeName + " type=router options:router-port=dtoj-" + nodeName + " addresses=router",
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add " + clusterRouter + " dtoj-" + nodeName + " " + drLrpMAC + " " + drLrpIP + "/29",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " " + drLrpIP,
			})
			addNodeportLBs(fexec, nodeName, tcpLBUUID, udpLBUUID)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist ls-add " + util.ExternalSwitchPrefix + nodeName,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + util.ExternalSwitchPrefix + nodeName + " br-eth0_" + nodeName + " -- lsp-set-addresses br-eth0_" + nodeName + " unknown -- lsp-set-type br-eth0_" + nodeName + " localnet -- lsp-set-options br-eth0_" + nodeName + " network_name=" + util.PhysicalNetworkName + " -- set logical_switch_port br-eth0_" + nodeName + " tag_request=" + "1024",
				"ovn-nbctl --timeout=15 -- --if-exists lrp-del rtoe-" + gwRouter + " -- lrp-add " + gwRouter + " rtoe-" + gwRouter + " " + physicalBridgeMAC + " " + physicalGatewayIPMask + " -- set logical_router_port rtoe-" + gwRouter + " external-ids:gateway-physical-ip=yes",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + util.ExternalSwitchPrefix + nodeName + " etor-" + gwRouter + " -- set logical_switch_port etor-" + gwRouter + " type=router options:router-port=rtoe-" + gwRouter + " addresses=\"" + physicalBridgeMAC + "\"",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 " + physicalGatewayNextHop + " rtoe-" + gwRouter,
				"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + clusterRouter + " " + nodeSubnet + " " + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-nat-add " + gwRouter + " snat " + physicalGatewayIP + " " + clusterCIDR,
			})

			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouter + " " + nodeIPMask + " " + nodeMgmtPortIP,
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=" + util.GWRouterPrefix + nodeName,
				Output: tcpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=" + util.GWRouterPrefix + nodeName,
				Output: udpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_router " + util.GWRouterPrefix + nodeName + " external_ids:physical_ip",
				Output: "169.254.33.2",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=" + physicalGatewayIP,
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + joinSwitch,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + joinSwitch + " jtor-" + gwRouter + " -- set logical_switch_port jtor-" + gwRouter + " type=router options:router-port=rtoj-" + gwRouter + " addresses=router",
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add " + gwRouter + " rtoj-" + gwRouter + " " + lrpMAC + " " + lrpIP + "/29",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + joinSwitch + " jtod-" + nodeName + " -- set logical_switch_port jtod-" + nodeName + " type=router options:router-port=dtoj-" + nodeName + " addresses=router",
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add " + clusterRouter + " dtoj-" + nodeName + " " + drLrpMAC + " " + drLrpIP + "/29",
			})

			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " " + drLrpIP,
			})
			addNodeportLBs(fexec, nodeName, tcpLBUUID, udpLBUUID)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist ls-add " + util.ExternalSwitchPrefix + nodeName,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + util.ExternalSwitchPrefix + nodeName + " br-eth0_" + nodeName + " -- lsp-set-addresses br-eth0_" + nodeName + " unknown -- lsp-set-type br-eth0_" + nodeName + " localnet -- lsp-set-options br-eth0_" + nodeName + " network_name=" + util.PhysicalNetworkName + " -- set logical_switch_port br-eth0_" + nodeName + " tag_request=" + "1024",
				"ovn-nbctl --timeout=15 -- --if-exists lrp-del rtoe-" + gwRouter + " -- lrp-add " + gwRouter + " rtoe-" + gwRouter + " " + physicalBridgeMAC + " " + physicalGatewayIPMask + " -- set logical_router_port rtoe-" + gwRouter + " external-ids:gateway-physical-ip=yes",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + util.ExternalSwitchPrefix + nodeName + " etor-" + gwRouter + " -- set logical_switch_port etor-" + gwRouter + " type=router options:router-port=rtoe-" + gwRouter + " addresses=\"" + physicalBridgeMAC + "\"",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 " + physicalGatewayNextHop + " rtoe-" + gwRouter,
				"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + clusterRouter + " " + nodeSubnet + " " + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-nat-add " + gwRouter + " snat " + physicalGatewayIP + " " + clusterCIDR,
			})

			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouter + " " + nodeIPMask + " " + nodeMgmtPortIP,
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=" + util.GWRouterPrefix + nodeName,
				Output: tcpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=" + util.GWRouterPrefix + nodeName,
				Output: udpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_router " + util.GWRouterPrefix + nodeName + " external_ids:physical_ip",
				Output: "169.254.33.2",
			})

			stop := make(chan struct{})
			wf, err := factory.NewWatchFactory(fakeClient, stop)
			Expect(err).NotTo(HaveOccurred())
			defer close(stop)

			clusterController := NewOvnController(fakeClient, wf, stop)
			Expect(clusterController).NotTo(BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID
			_ = clusterController.joinSubnetAllocator.AddNetworkRange("100.64.0.0/16", 3)

			// Let the real code run and ensure OVN database sync
			err = clusterController.WatchNodes()
			Expect(err).NotTo(HaveOccurred())

			_, subnet, err := net.ParseCIDR(nodeSubnet)
			Expect(err).NotTo(HaveOccurred())
			err = clusterController.syncGatewayLogicalNetwork(updatedNode, l3GatewayConfig, subnet.String())
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"--init-gateways",
			"--nodeport",
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

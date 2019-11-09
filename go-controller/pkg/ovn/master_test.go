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
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func cleanupGateway(fexec *ovntest.FakeExec, nodeName string, nodeSubnet string, clusterCIDR string, nextHop string) {
	const (
		lrpMAC                 string = "00:00:00:05:46:c3"
		brLocalnetMAC          string = "11:22:33:44:55:66"
		clusterRouterUUID      string = "5cedba03-679f-41f3-b00e-b8ed7437bc6c"
		systemID               string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
		tcpLBUUID              string = "d2e858b2-cb5a-441b-a670-ed450f79a91f"
		udpLBUUID              string = "12832f14-eb0f-44d4-b8db-4cccbc73c792"
		clusterIPNet           string = "10.1.0.0"
		localnetGatewayIP      string = "169.254.33.2/24"
		localnetGatewayNextHop string = "169.254.33.1"
		localnetBridgeName     string = "br-local"
		masterGWCIDR           string = "10.1.1.1/24"
		masterMgmtPortIP       string = "10.1.1.2"
		node1RouteUUID         string = "0cac12cf-3e0f-4682-b028-5ea2e0001962"
		node1mgtRouteUUID      string = "0cac12cf-3e0f-4682-b028-5ea2e0001963"
	)

	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router external_ids:k8s-cluster-router=yes",
		Output: clusterRouterUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_" + nodeName + " networks",
		Output: "[\"100.64.0.3/16\"]",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route ip_prefix=0.0.0.0/0 nexthop=100.64.0.3",
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=100.64.0.3",
		Output: node1RouteUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove logical_router " + clusterRouterUUID + " static_routes " + node1RouteUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=" + nextHop,
		Output: node1mgtRouteUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove logical_router " + clusterRouterUUID + " static_routes " + node1mgtRouteUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exist lsp-del jtor-GR_" + nodeName,
		"ovn-nbctl --timeout=15 --if-exist lr-del GR_" + nodeName,
		"ovn-nbctl --timeout=15 --if-exist ls-del ext_" + nodeName,
	})
}

func defaultFakeExec(nodeSubnet, nodeName string) (*ovntest.FakeExec, string, string) {
	const (
		tcpLBUUID  string = "1a3dfc82-2749-4931-9190-c30e7c0ecea3"
		udpLBUUID  string = "6d3142fc-53e8-4ac1-88e6-46094a5a9957"
		joinLRPMAC string = "00:00:00:83:25:1C"
		lrpMAC     string = "00:00:00:05:46:C3"
		mgmtMAC    string = "01:02:03:04:05:06"
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
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --may-exist ls-add join -- set logical_switch join other-config:subnet=100.64.0.0/16 -- set logical_switch join other-config:exclude_ips=100.64.0.1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-ovn_cluster_router mac",
		Output: joinLRPMAC,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 -- --may-exist lsp-add join jtor-ovn_cluster_router -- set logical_switch_port jtor-ovn_cluster_router type=router options:router-port=rtoj-ovn_cluster_router addresses=\"" + joinLRPMAC + "\"",
	})

	// Node-related logical network stuff
	ip, cidr, err := net.ParseCIDR(nodeSubnet)
	Expect(err).NotTo(HaveOccurred())
	cidr.IP = util.NextIP(ip)
	gwCIDR := cidr.String()
	gwIP := cidr.IP.String()
	nodeMgmtPortIP := util.NextIP(cidr.IP)
	hybridOverlayIP := util.NextIP(nodeMgmtPortIP)

	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name,other-config find logical_switch other-config:subnet!=_",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtos-" + nodeName + " mac",
		// Return a known MAC; otherwise code autogenerates it
		Output: lrpMAC,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --may-exist lrp-add ovn_cluster_router rtos-" + nodeName + " " + lrpMAC + " " + gwCIDR,
		"ovn-nbctl --timeout=15 -- --may-exist ls-add " + nodeName + " -- set logical_switch " + nodeName + " other-config:subnet=" + nodeSubnet + " other-config:exclude_ips=" + nodeMgmtPortIP.String() + ".." + hybridOverlayIP.String() + " external-ids:gateway_ip=" + gwCIDR,
		"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " other-config:mcast_snoop=\"true\" other-config:mcast_querier=\"true\" other-config:mcast_eth_src=\"" + lrpMAC + "\" other-config:mcast_ip4_src=\"" + gwIP + "\"",
		"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " stor-" + nodeName + " -- set logical_switch_port stor-" + nodeName + " type=router options:router-port=rtos-" + nodeName + " addresses=\"" + lrpMAC + "\"",
		"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " load_balancer=" + tcpLBUUID,
		"ovn-nbctl --timeout=15 add logical_switch " + nodeName + " load_balancer " + udpLBUUID,
		"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " k8s-" + nodeName + " -- lsp-set-addresses " + "k8s-" + nodeName + " " + mgmtMAC + " " + nodeMgmtPortIP.String(),
	})

	return fexec, tcpLBUUID, udpLBUUID
}

func addNodeportLBs(fexec *ovntest.FakeExec, nodeName, tcpLBUUID, udpLBUUID string) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_" + nodeName,
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=GR_" + nodeName,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 -- create load_balancer external_ids:TCP_lb_gateway_router=GR_" + nodeName + " protocol=tcp",
		Output: tcpLBUUID,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 -- create load_balancer external_ids:UDP_lb_gateway_router=GR_" + nodeName + " protocol=udp",
		Output: udpLBUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 set logical_router GR_node1 load_balancer=" + tcpLBUUID,
		"ovn-nbctl --timeout=15 add logical_router GR_node1 load_balancer " + udpLBUUID,
	})
}

var _ = Describe("Master Operations", func() {
	var app *cli.App

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

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
				Annotations: map[string]string{
					OvnNodeManagementPortMacAddress: mgmtMAC,
					OvnHostSubnet:                   nodeSubnet,
				},
			}}

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			clusterController := NewOvnController(fakeClient, f, nil)
			Expect(clusterController).NotTo(BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID

			err = clusterController.StartClusterMaster("master")
			Expect(err).NotTo(HaveOccurred())

			err = clusterController.WatchNodes(nil)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
			updatedNode, err := fakeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations).To(HaveKeyWithValue(OvnHostSubnet, nodeSubnet))
			Expect(updatedNode.Annotations).To(HaveKeyWithValue(OvnNodeManagementPortMacAddress, mgmtMAC))
			Eventually(func() bool { return fexec.CalledMatchesExpected() }, 2).Should(BeTrue())
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
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
				Annotations: map[string]string{
					OvnNodeManagementPortMacAddress: mgmtMAC,
					OvnHostSubnet:                   nodeSubnet,
				},
			}}

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})
			fakeClient.PrependReactor("patch", "nodes", func(action kubetesting.Action) (bool, kuberuntime.Object, error) {
				// Should not be called as the node already has a subnet
				Expect(true).To(BeFalse())
				return true, nil, fmt.Errorf("should not be called")
			})

			fexec, tcpLBUUID, udpLBUUID := defaultFakeExec(nodeSubnet, nodeName)
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			cleanupGateway(fexec, nodeName, nodeSubnet, clusterCIDR, nextHop)

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			clusterController := NewOvnController(fakeClient, f, nil)
			Expect(clusterController).NotTo(BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID

			err = clusterController.StartClusterMaster("master")
			Expect(err).NotTo(HaveOccurred())

			err = clusterController.WatchNodes(nil)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
			updatedNode, err := fakeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations).To(HaveKeyWithValue(OvnHostSubnet, nodeSubnet))
			Expect(updatedNode.Annotations).To(HaveKeyWithValue(OvnNodeManagementPortMacAddress, mgmtMAC))
			Eventually(func() bool { return fexec.CalledMatchesExpected() }, 2).Should(BeTrue())
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("removes deleted nodes from the OVN database", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				tcpLBUUID         string = "1a3dfc82-2749-4931-9190-c30e7c0ecea3"
				udpLBUUID         string = "6d3142fc-53e8-4ac1-88e6-46094a5a9957"
				clusterRouterUUID string = "6d3142fc-53e8-4ac1-88e6-46094a5a9957"
				node1Name         string = "openshift-node-1"
				node1Subnet       string = "10.128.0.0/24"
				node1RouteUUID    string = "0cac12cf-3e0f-4682-b028-5ea2e0001962"
				node1mgtRouteUUID string = "0cac12cf-3e0f-4682-b028-5ea2e0001963"
				masterName        string = "openshift-master-node"
				masterSubnet      string = "10.128.2.0/24"
				masterGWCIDR      string = "10.128.2.1/24"
				masterMgmtPortIP  string = "10.128.2.2"
				masterHOPortIP    string = "10.128.2.3"
				lrpMAC            string = "00:00:00:05:46:C3"
				masterMgmtPortMAC string = "00:00:00:55:66:77"
			)

			fexec := ovntest.NewFakeExec()
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
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router external_ids:k8s-cluster-router=yes",
				Output: clusterRouterUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-GR_openshift-node-1 networks",
				Output: "[\"100.64.0.3/16\"]",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route ip_prefix=0.0.0.0/0 nexthop=100.64.0.3",
				Output: "",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=100.64.0.3",
				Output: node1RouteUUID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exists remove logical_router " + clusterRouterUUID + " static_routes " + node1RouteUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=10.128.0.2",
				Output: node1mgtRouteUUID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exists remove logical_router " + clusterRouterUUID + " static_routes " + node1mgtRouteUUID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exist lsp-del jtor-GR_" + node1Name,
				"ovn-nbctl --timeout=15 --if-exist lr-del GR_" + node1Name,
				"ovn-nbctl --timeout=15 --if-exist ls-del ext_" + node1Name,
			})

			// Expect the code to re-add the master node (which still exists)
			// when the factory watch begins and enumerates all existing
			// Kubernetes API nodes
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtos-" + masterName + " mac",
				// Return a known MAC; otherwise code autogenerates it
				Output: lrpMAC,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lrp-add ovn_cluster_router rtos-" + masterName + " " + lrpMAC + " " + masterGWCIDR,
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + masterName + " -- set logical_switch " + masterName + " other-config:subnet=" + masterSubnet + " other-config:exclude_ips=" + masterMgmtPortIP + ".." + masterHOPortIP + " external-ids:gateway_ip=" + masterGWCIDR,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + masterName + " stor-" + masterName + " -- set logical_switch_port stor-" + masterName + " type=router options:router-port=rtos-" + masterName + " addresses=\"" + lrpMAC + "\"",
				"ovn-nbctl --timeout=15 set logical_switch " + masterName + " load_balancer=" + tcpLBUUID,
				"ovn-nbctl --timeout=15 add logical_switch " + masterName + " load_balancer " + udpLBUUID,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + masterName + " k8s-" + masterName + " -- lsp-set-addresses " + "k8s-" + masterName + " " + masterMgmtPortMAC + " " + masterMgmtPortIP,
			})

			cleanupGateway(fexec, masterName, masterSubnet, masterGWCIDR, masterMgmtPortIP)

			masterNode := v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: masterName,
					Annotations: map[string]string{
						OvnHostSubnet:                   masterSubnet,
						OvnNodeManagementPortMacAddress: masterMgmtPortMAC,
					},
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

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			clusterController := NewOvnController(fakeClient, f, nil)
			Expect(clusterController).NotTo(BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID

			// Let the real code run and ensure OVN database sync
			err = clusterController.WatchNodes(nil)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())

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
	var testNS ns.NetNS

	const (
		clusterIPNet string = "10.1.0.0"
		clusterCIDR  string = clusterIPNet + "/16"
	)

	BeforeEach(func() {
		var err error
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())

		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	AfterEach(func() {
		Expect(testNS.Close()).To(Succeed())
	})

	It("sets up a localnet gateway", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName               string = "node1"
				lrpMAC                 string = "00:00:00:05:46:c3"
				brLocalnetMAC          string = "11:22:33:44:55:66"
				lrpIP                  string = "100.64.0.3"
				lrpCIDR                string = lrpIP + "/16"
				clusterRouterUUID      string = "5cedba03-679f-41f3-b00e-b8ed7437bc6c"
				systemID               string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
				tcpLBUUID              string = "d2e858b2-cb5a-441b-a670-ed450f79a91f"
				udpLBUUID              string = "12832f14-eb0f-44d4-b8db-4cccbc73c792"
				nodeSubnet             string = "10.1.1.0/24"
				nextHop                string = "10.1.1.2"
				gwRouter               string = "GR_" + nodeName
				clusterIPNet           string = "10.1.0.0"
				clusterCIDR            string = clusterIPNet + "/16"
				localnetGatewayIP      string = "169.254.33.2/24"
				localnetGatewayNextHop string = "169.254.33.1"
				localnetBridgeName     string = "br-local"
				masterGWCIDR           string = "10.1.1.1/24"
				masterMgmtPortIP       string = "10.1.1.2"
				masterHOPortIP         string = "10.1.1.3"
				node1RouteUUID         string = "0cac12cf-3e0f-4682-b028-5ea2e0001962"
				node1mgtRouteUUID      string = "0cac12cf-3e0f-4682-b028-5ea2e0001963"
			)

			ifaceID := localnetBridgeName + "_" + nodeName

			testNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					OvnNodeManagementPortMacAddress: brLocalnetMAC,
					OvnHostSubnet:                   nodeSubnet,
					OvnNodeGatewayMode:              string(config.GatewayModeLocal),
					OvnNodeGatewayVlanID:            string(config.Gateway.VLANID),
					OvnNodeGatewayIfaceID:           ifaceID,
					OvnNodeGatewayMacAddress:        brLocalnetMAC,
					OvnNodeGatewayIP:                localnetGatewayIP,
					OvnNodeGatewayNextHop:           localnetGatewayNextHop,
				},
			}}

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})

			fexec := ovntest.NewFakeExec()
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name,other-config find logical_switch other-config:subnet!=_",
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtos-" + nodeName + " mac",
				// Return a known MAC; otherwise code autogenerates it
				Output: lrpMAC,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lrp-add ovn_cluster_router rtos-" + nodeName + " " + lrpMAC + " " + masterGWCIDR,
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + nodeName + " -- set logical_switch " + nodeName + " other-config:subnet=" + nodeSubnet + " other-config:exclude_ips=" + masterMgmtPortIP + ".." + masterHOPortIP + " external-ids:gateway_ip=" + masterGWCIDR,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " stor-" + nodeName + " -- set logical_switch_port stor-" + nodeName + " type=router options:router-port=rtos-" + nodeName + " addresses=\"" + lrpMAC + "\"",
				"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " load_balancer=" + tcpLBUUID,
				"ovn-nbctl --timeout=15 add logical_switch " + nodeName + " load_balancer " + udpLBUUID,

				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " k8s-" + nodeName + " -- lsp-set-addresses " + "k8s-" + nodeName + " " + brLocalnetMAC + " " + masterMgmtPortIP,
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router external_ids:k8s-cluster-router=yes",
				Output: clusterRouterUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-sbctl --timeout=15 --data=bare --no-heading --columns=name find Chassis hostname=" + nodeName,
				Output: systemID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=169.254.33.2",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port jtor-" + gwRouter + " dynamic_addresses",
				Output: "",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --wait=sb --may-exist lsp-add join jtor-GR_" + nodeName + " -- --if-exists clear logical_switch_port jtor-GR_" + nodeName + " dynamic_addresses -- lsp-set-addresses jtor-GR_" + nodeName + " dynamic",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port jtor-" + gwRouter + " dynamic_addresses",
				Output: lrpMAC + " " + lrpIP,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --if-exists get logical_switch join other-config:subnet",
				Output: "\"100.64.0.1/16\"",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add GR_" + nodeName + " rtoj-GR_" + nodeName + " " + lrpMAC + " " + lrpCIDR + " -- set logical_switch_port jtor-GR_" + nodeName + " type=router options:router-port=rtoj-GR_" + nodeName + " addresses=router",
				"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " 100.64.0.1",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=name,options find logical_router options:lb_force_snat_ip!=-",
				Output: fmt.Sprintf(`GR_openshift-node-2      chassis=5befb1e1-b0b1-4277-bb1d-54e8732a39c6 lb_force_snat_ip=%s
GR_openshift-node-1      chassis=0861e85c-5060-42fd-839e-8c463c7da378 lb_force_snat_ip=100.64.0.5
GR_openshift-master-node chassis=6a47b33b-89d3-4d65-ac31-b19b549326c7 lb_force_snat_ip=100.64.0.4`, lrpIP),
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouterUUID + " 0.0.0.0/0 " + lrpIP,
			})
			addNodeportLBs(fexec, nodeName, tcpLBUUID, udpLBUUID)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist ls-add ext_" + nodeName,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " br-local_" + nodeName + " -- lsp-set-addresses br-local_" + nodeName + " unknown -- lsp-set-type br-local_" + nodeName + " localnet -- lsp-set-options br-local_" + nodeName + " network_name=" + util.PhysicalNetworkName,
				"ovn-nbctl --timeout=15 -- --if-exists lrp-del rtoe-" + gwRouter + " -- lrp-add " + gwRouter + " rtoe-" + gwRouter + " " + brLocalnetMAC + " 169.254.33.2/24 -- set logical_router_port rtoe-" + gwRouter + " external-ids:gateway-physical-ip=yes",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " etor-" + gwRouter + " -- set logical_switch_port etor-" + gwRouter + " type=router options:router-port=rtoe-" + gwRouter + " addresses=\"" + brLocalnetMAC + "\"",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 169.254.33.1 rtoe-" + gwRouter,
				"ovn-nbctl --timeout=15 --may-exist lr-nat-add " + gwRouter + " snat 169.254.33.2 " + clusterCIDR,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouterUUID + " " + lrpIP + " " + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + clusterRouterUUID + " " + nodeSubnet + " " + lrpIP,
			})

			//fexec.AddFakeCmdsNoOutputNoError([]string{
			//	"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_" + nodeName,
			//})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_" + nodeName,
				Output: tcpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=GR_" + nodeName,
				Output: udpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_router GR_" + nodeName + " external_ids:physical_ip",
				Output: "169.254.33.2",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router external_ids:k8s-cluster-router=yes",
				Output: clusterRouterUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-sbctl --timeout=15 --data=bare --no-heading --columns=name find Chassis hostname=" + nodeName,
				Output: systemID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=169.254.33.2",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port jtor-" + gwRouter + " dynamic_addresses",
				Output: "",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --wait=sb --may-exist lsp-add join jtor-GR_" + nodeName + " -- --if-exists clear logical_switch_port jtor-GR_" + nodeName + " dynamic_addresses -- lsp-set-addresses jtor-GR_" + nodeName + " dynamic",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port jtor-" + gwRouter + " dynamic_addresses",
				Output: lrpMAC + " " + lrpIP,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --if-exists get logical_switch join other-config:subnet",
				Output: "\"100.64.0.1/16\"",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add GR_" + nodeName + " rtoj-GR_" + nodeName + " " + lrpMAC + " " + lrpCIDR + " -- set logical_switch_port jtor-GR_" + nodeName + " type=router options:router-port=rtoj-GR_" + nodeName + " addresses=router",
				"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " 100.64.0.1",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=name,options find logical_router options:lb_force_snat_ip!=-",
				Output: fmt.Sprintf(`GR_openshift-node-2      chassis=5befb1e1-b0b1-4277-bb1d-54e8732a39c6 lb_force_snat_ip=%s
GR_openshift-node-1      chassis=0861e85c-5060-42fd-839e-8c463c7da378 lb_force_snat_ip=100.64.0.5
GR_openshift-master-node chassis=6a47b33b-89d3-4d65-ac31-b19b549326c7 lb_force_snat_ip=100.64.0.4`, lrpIP),
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouterUUID + " 0.0.0.0/0 " + lrpIP,
			})
			addNodeportLBs(fexec, nodeName, tcpLBUUID, udpLBUUID)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist ls-add ext_" + nodeName,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " br-local_" + nodeName + " -- lsp-set-addresses br-local_" + nodeName + " unknown -- lsp-set-type br-local_" + nodeName + " localnet -- lsp-set-options br-local_" + nodeName + " network_name=" + util.PhysicalNetworkName,
				"ovn-nbctl --timeout=15 -- --if-exists lrp-del rtoe-" + gwRouter + " -- lrp-add " + gwRouter + " rtoe-" + gwRouter + " " + brLocalnetMAC + " 169.254.33.2/24 -- set logical_router_port rtoe-" + gwRouter + " external-ids:gateway-physical-ip=yes",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " etor-" + gwRouter + " -- set logical_switch_port etor-" + gwRouter + " type=router options:router-port=rtoe-" + gwRouter + " addresses=\"" + brLocalnetMAC + "\"",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 169.254.33.1 rtoe-" + gwRouter,
				"ovn-nbctl --timeout=15 --may-exist lr-nat-add " + gwRouter + " snat 169.254.33.2 " + clusterCIDR,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouterUUID + " " + lrpIP + " " + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + clusterRouterUUID + " " + nodeSubnet + " " + lrpIP,
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_" + nodeName,
				Output: tcpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=GR_" + nodeName,
				Output: udpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_router GR_" + nodeName + " external_ids:physical_ip",
				Output: "169.254.33.2",
			})

			stop := make(chan struct{})
			wf, err := factory.NewWatchFactory(fakeClient, stop)
			Expect(err).NotTo(HaveOccurred())
			defer wf.Shutdown()

			clusterController := NewOvnController(fakeClient, wf, nil)
			Expect(clusterController).NotTo(BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID

			// Let the real code run and ensure OVN database sync
			err = clusterController.WatchNodes(nil)
			Expect(err).NotTo(HaveOccurred())

			_, subnet, err := net.ParseCIDR(nodeSubnet)
			Expect(err).NotTo(HaveOccurred())
			err = clusterController.syncGatewayLogicalNetwork(&testNode, string(config.GatewayModeLocal), subnet.String())
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
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
				nodeName          string = "node1"
				lrpMAC            string = "00:00:00:05:46:c3"
				brLocalnetMAC     string = "11:22:33:44:55:66"
				lrpIP             string = "100.64.0.3"
				lrpCIDR           string = lrpIP + "/16"
				clusterRouterUUID string = "5cedba03-679f-41f3-b00e-b8ed7437bc6c"
				systemID          string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
				tcpLBUUID         string = "d2e858b2-cb5a-441b-a670-ed450f79a91f"
				udpLBUUID         string = "12832f14-eb0f-44d4-b8db-4cccbc73c792"
				nodeSubnet        string = "10.1.1.0/24"
				nextHop           string = "10.1.1.2"
				//nextHop                string = "10.64.0.2"
				gwRouter     string = "GR_" + nodeName
				clusterIPNet string = "10.1.0.0"
				clusterCIDR  string = clusterIPNet + "/16"
				//localnetGatewayIP      string = "100.64.0.3/24"
				//localnetGatewayNextHop string = "100.64.0.2"
				localnetGatewayIP      string = "100.64.0.3/24"
				localnetGatewayNextHop string = "100.64.0.1"
				localnetBridgeName     string = "br-local"
				masterGWCIDR           string = "10.1.1.1/24"
				masterMgmtPortIP       string = "10.1.1.2"
				masterHOPortIP         string = "10.1.1.3"
				node1RouteUUID         string = "0cac12cf-3e0f-4682-b028-5ea2e0001962"
				node1mgtRouteUUID      string = "0cac12cf-3e0f-4682-b028-5ea2e0001963"
			)

			ifaceID := localnetBridgeName + "_" + nodeName

			testNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					OvnNodeManagementPortMacAddress: brLocalnetMAC,
					OvnHostSubnet:                   nodeSubnet,
					OvnNodeGatewayMode:              string(config.GatewayModeShared),
					OvnNodeGatewayVlanID:            "1024",
					OvnNodeGatewayIfaceID:           ifaceID,
					OvnNodeGatewayMacAddress:        brLocalnetMAC,
					OvnNodeGatewayIP:                localnetGatewayIP,
					OvnNodeGatewayNextHop:           localnetGatewayNextHop,
				},
			}}

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})

			fexec := ovntest.NewFakeExec()
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name,other-config find logical_switch other-config:subnet!=_",
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtos-" + nodeName + " mac",
				// Return a known MAC; otherwise code autogenerates it
				Output: lrpMAC,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lrp-add ovn_cluster_router rtos-" + nodeName + " " + lrpMAC + " " + masterGWCIDR,
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + nodeName + " -- set logical_switch " + nodeName + " other-config:subnet=" + nodeSubnet + " other-config:exclude_ips=" + masterMgmtPortIP + ".." + masterHOPortIP + " external-ids:gateway_ip=" + masterGWCIDR,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " stor-" + nodeName + " -- set logical_switch_port stor-" + nodeName + " type=router options:router-port=rtos-" + nodeName + " addresses=\"" + lrpMAC + "\"",
				"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " load_balancer=" + tcpLBUUID,
				"ovn-nbctl --timeout=15 add logical_switch " + nodeName + " load_balancer " + udpLBUUID,

				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " k8s-" + nodeName + " -- lsp-set-addresses " + "k8s-" + nodeName + " " + brLocalnetMAC + " " + masterMgmtPortIP,
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router external_ids:k8s-cluster-router=yes",
				Output: clusterRouterUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-sbctl --timeout=15 --data=bare --no-heading --columns=name find Chassis hostname=" + nodeName,
				Output: systemID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=" + lrpIP,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port jtor-" + gwRouter + " dynamic_addresses",
				Output: "",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --wait=sb --may-exist lsp-add join jtor-GR_" + nodeName + " -- --if-exists clear logical_switch_port jtor-GR_" + nodeName + " dynamic_addresses -- lsp-set-addresses jtor-GR_" + nodeName + " dynamic",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port jtor-" + gwRouter + " dynamic_addresses",
				Output: lrpMAC + " " + lrpIP,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --if-exists get logical_switch join other-config:subnet",
				Output: "\"100.64.0.1/16\"",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add GR_" + nodeName + " rtoj-GR_" + nodeName + " " + lrpMAC + " " + lrpCIDR + " -- set logical_switch_port jtor-GR_" + nodeName + " type=router options:router-port=rtoj-GR_" + nodeName + " addresses=router",
				"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " 100.64.0.1",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=name,options find logical_router options:lb_force_snat_ip!=-",
				Output: fmt.Sprintf(`GR_openshift-node-2      chassis=5befb1e1-b0b1-4277-bb1d-54e8732a39c6 lb_force_snat_ip=%s
GR_openshift-node-1      chassis=0861e85c-5060-42fd-839e-8c463c7da378 lb_force_snat_ip=100.64.0.5
GR_openshift-master-node chassis=6a47b33b-89d3-4d65-ac31-b19b549326c7 lb_force_snat_ip=100.64.0.4`, lrpIP),
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouterUUID + " 0.0.0.0/0 " + lrpIP,
			})
			addNodeportLBs(fexec, nodeName, tcpLBUUID, udpLBUUID)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist ls-add ext_" + nodeName,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " br-local_" + nodeName + " -- lsp-set-addresses br-local_" + nodeName + " unknown -- lsp-set-type br-local_" + nodeName + " localnet -- lsp-set-options br-local_" + nodeName + " network_name=" + util.PhysicalNetworkName + " -- set logical_switch_port br-local_" + nodeName + " tag_request=" + "1024",
				"ovn-nbctl --timeout=15 -- --if-exists lrp-del rtoe-" + gwRouter + " -- lrp-add " + gwRouter + " rtoe-" + gwRouter + " " + brLocalnetMAC + " 100.64.0.3/24 -- set logical_router_port rtoe-" + gwRouter + " external-ids:gateway-physical-ip=yes",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " etor-" + gwRouter + " -- set logical_switch_port etor-" + gwRouter + " type=router options:router-port=rtoe-" + gwRouter + " addresses=\"" + brLocalnetMAC + "\"",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 100.64.0.1 rtoe-" + gwRouter,
				"ovn-nbctl --timeout=15 --may-exist lr-nat-add " + gwRouter + " snat 100.64.0.3 " + clusterCIDR,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouterUUID + " " + lrpIP + " " + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + clusterRouterUUID + " " + nodeSubnet + " " + lrpIP,
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router external_ids:k8s-cluster-router=yes",
				Output: clusterRouterUUID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouterUUID + " " + "100.64.0.3/32" + " " + nextHop,
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_" + nodeName,
				Output: tcpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=GR_" + nodeName,
				Output: udpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_router GR_" + nodeName + " external_ids:physical_ip",
				Output: "169.254.33.2",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router external_ids:k8s-cluster-router=yes",
				Output: clusterRouterUUID,
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-sbctl --timeout=15 --data=bare --no-heading --columns=name find Chassis hostname=" + nodeName,
				Output: systemID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=" + lrpIP,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port jtor-" + gwRouter + " dynamic_addresses",
				Output: "",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --wait=sb --may-exist lsp-add join jtor-GR_" + nodeName + " -- --if-exists clear logical_switch_port jtor-GR_" + nodeName + " dynamic_addresses -- lsp-set-addresses jtor-GR_" + nodeName + " dynamic",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port jtor-" + gwRouter + " dynamic_addresses",
				Output: lrpMAC + " " + lrpIP,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --if-exists get logical_switch join other-config:subnet",
				Output: "\"100.64.0.1/16\"",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add GR_" + nodeName + " rtoj-GR_" + nodeName + " " + lrpMAC + " " + lrpCIDR + " -- set logical_switch_port jtor-GR_" + nodeName + " type=router options:router-port=rtoj-GR_" + nodeName + " addresses=router",
				"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " 100.64.0.1",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=name,options find logical_router options:lb_force_snat_ip!=-",
				Output: fmt.Sprintf(`GR_openshift-node-2      chassis=5befb1e1-b0b1-4277-bb1d-54e8732a39c6 lb_force_snat_ip=%s
GR_openshift-node-1      chassis=0861e85c-5060-42fd-839e-8c463c7da378 lb_force_snat_ip=100.64.0.5
GR_openshift-master-node chassis=6a47b33b-89d3-4d65-ac31-b19b549326c7 lb_force_snat_ip=100.64.0.4`, lrpIP),
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouterUUID + " 0.0.0.0/0 " + lrpIP,
			})
			addNodeportLBs(fexec, nodeName, tcpLBUUID, udpLBUUID)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --may-exist ls-add ext_" + nodeName,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " br-local_" + nodeName + " -- lsp-set-addresses br-local_" + nodeName + " unknown -- lsp-set-type br-local_" + nodeName + " localnet -- lsp-set-options br-local_" + nodeName + " network_name=" + util.PhysicalNetworkName,
				"ovn-nbctl --timeout=15 -- --if-exists lrp-del rtoe-" + gwRouter + " -- lrp-add " + gwRouter + " rtoe-" + gwRouter + " " + brLocalnetMAC + " 100.64.0.3/24 -- set logical_router_port rtoe-" + gwRouter + " external-ids:gateway-physical-ip=yes",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " etor-" + gwRouter + " -- set logical_switch_port etor-" + gwRouter + " type=router options:router-port=rtoe-" + gwRouter + " addresses=\"" + brLocalnetMAC + "\"",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 100.64.0.1 rtoe-" + gwRouter,
				"ovn-nbctl --timeout=15 --may-exist lr-nat-add " + gwRouter + " snat 100.64.0.3 " + clusterCIDR,
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouterUUID + " " + lrpIP + " " + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + clusterRouterUUID + " " + nodeSubnet + " " + lrpIP,
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_" + nodeName,
				Output: tcpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=GR_" + nodeName,
				Output: udpLBUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 get logical_router GR_" + nodeName + " external_ids:physical_ip",
				Output: "169.254.33.2",
			})

			stop := make(chan struct{})
			wf, err := factory.NewWatchFactory(fakeClient, stop)
			Expect(err).NotTo(HaveOccurred())
			defer wf.Shutdown()

			clusterController := NewOvnController(fakeClient, wf, nil)
			Expect(clusterController).NotTo(BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID

			// Let the real code run and ensure OVN database sync
			err = clusterController.WatchNodes(nil)
			Expect(err).NotTo(HaveOccurred())

			_, subnet, err := net.ParseCIDR(nodeSubnet)
			Expect(err).NotTo(HaveOccurred())
			err = clusterController.syncGatewayLogicalNetwork(&testNode, string(config.GatewayModeLocal), subnet.String())
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
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

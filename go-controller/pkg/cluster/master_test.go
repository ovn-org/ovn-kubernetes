package cluster

import (
	"fmt"
	"net"

	"github.com/urfave/cli"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

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
		app.Action = func(ctx *cli.Context) error {
			const (
				tcpLBUUID    string = "1a3dfc82-2749-4931-9190-c30e7c0ecea3"
				udpLBUUID    string = "6d3142fc-53e8-4ac1-88e6-46094a5a9957"
				clusterIPNet string = "10.1.0.0"
				clusterCIDR  string = clusterIPNet + "/16"
				joinLRPMAC   string = "00:00:00:83:25:1C"
			)

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --may-exist lr-add ovn_cluster_router -- set logical_router ovn_cluster_router external_ids:k8s-cluster-router=yes",
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
			const (
				nodeName       string = "node1"
				lrpMAC         string = "00:00:00:05:46:C3"
				nodeSubnet     string = "10.1.0.0/24"
				gwCIDR         string = "10.1.0.1/24"
				nodeMgmtPortIP string = "10.1.0.2"
			)

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
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + nodeName + " -- set logical_switch " + nodeName + " other-config:subnet=" + nodeSubnet + " other-config:exclude_ips=" + nodeMgmtPortIP + " external-ids:gateway_ip=" + gwCIDR,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " stor-" + nodeName + " -- set logical_switch_port stor-" + nodeName + " type=router options:router-port=rtos-" + nodeName + " addresses=\"" + lrpMAC + "\"",
				"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " load_balancer=" + tcpLBUUID,
				"ovn-nbctl --timeout=15 add logical_switch " + nodeName + " load_balancer " + udpLBUUID,
			})

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}},
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			clusterController := NewClusterController(fakeClient, f)
			Expect(clusterController).NotTo(BeNil())
			clusterController.TCPLoadBalancerUUID = tcpLBUUID
			clusterController.UDPLoadBalancerUUID = udpLBUUID

			_, ipnet, err := net.ParseCIDR(clusterCIDR)
			Expect(err).NotTo(HaveOccurred())
			clusterController.ClusterIPNet = append(clusterController.ClusterIPNet, CIDRNetworkEntry{
				CIDR:             ipnet,
				HostSubnetLength: 24,
			})

			err = clusterController.StartClusterMaster("master")
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
			return nil
		}

		err := app.Run([]string{app.Name})
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
				masterName        string = "openshift-master-node"
				masterSubnet      string = "10.128.2.0/24"
				masterGWCIDR      string = "10.128.2.1/24"
				masterMgmtPortIP  string = "10.128.2.2"
				lrpMAC            string = "00:00:00:05:46:C3"
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
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=100.64.0.3",
				Output: node1RouteUUID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exists remove logical_router " + clusterRouterUUID + " static_routes " + node1RouteUUID,
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
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + masterName + " -- set logical_switch " + masterName + " other-config:subnet=" + masterSubnet + " other-config:exclude_ips=" + masterMgmtPortIP + " external-ids:gateway_ip=" + masterGWCIDR,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + masterName + " stor-" + masterName + " -- set logical_switch_port stor-" + masterName + " type=router options:router-port=rtos-" + masterName + " addresses=\"" + lrpMAC + "\"",
			})

			masterNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: masterName,
				Annotations: map[string]string{
					OvnHostSubnet: masterSubnet,
				},
			}}
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

			clusterController := NewClusterController(fakeClient, f)
			Expect(clusterController).NotTo(BeNil())

			// Initialize OVS/OVN connection methods
			err = setupOVNMaster(masterNode.Name)
			Expect(err).NotTo(HaveOccurred())

			// Let the real code run and ensure OVN database sync
			err = clusterController.watchNodes()
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})
})

package cluster

import (
	"fmt"
	"net"

	"github.com/urfave/cli"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	fakeexec "k8s.io/utils/exec/testing"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

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

			fakeCmds := ovntest.AddFakeCmdsNoOutputNoError(nil, []string{
				"ovn-nbctl --timeout=15 -- --may-exist lr-add ovn_cluster_router -- set logical_router ovn_cluster_router external_ids:k8s-cluster-router=yes",
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
				Output: "",
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 -- create load_balancer external_ids:k8s-cluster-lb-tcp=yes protocol=tcp",
				Output: tcpLBUUID,
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-udp=yes",
				Output: "",
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 -- create load_balancer external_ids:k8s-cluster-lb-udp=yes protocol=udp",
				Output: udpLBUUID,
			})
			fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
				"ovn-nbctl --timeout=15 --may-exist ls-add join",
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-ovn_cluster_router mac",
				Output: joinLRPMAC,
			})
			fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add join jtor-ovn_cluster_router -- set logical_switch_port jtor-ovn_cluster_router type=router options:router-port=rtoj-ovn_cluster_router addresses=\"" + joinLRPMAC + "\"",
				"ovn-nbctl --timeout=15 -- set nb_global . external-ids:gateway-lock=\"\"",
			})

			var (
				nodeName   string = "node1"
				lrpMAC     string = "00:00:00:05:46:C3"
				nodeSubnet string = "10.1.0.0/24"
				gwCIDR     string = "10.1.0.1/24"
			)

			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtos-" + nodeName + " mac",
				// Return a known MAC; otherwise code autogenerates it
				Output: lrpMAC,
			})
			fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
				"ovn-nbctl --timeout=15 --may-exist lrp-add ovn_cluster_router rtos-" + nodeName + " " + lrpMAC + " " + gwCIDR,
				"ovn-nbctl --timeout=15 -- --may-exist ls-add " + nodeName + " -- set logical_switch " + nodeName + " other-config:subnet=" + nodeSubnet + " external-ids:gateway_ip=" + gwCIDR,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " stor-" + nodeName + " -- set logical_switch_port stor-" + nodeName + " type=router options:router-port=rtos-" + nodeName + " addresses=\"" + lrpMAC + "\"",
				"ovn-nbctl --timeout=15 set logical_switch " + nodeName + " load_balancer=" + tcpLBUUID,
				"ovn-nbctl --timeout=15 add logical_switch " + nodeName + " load_balancer " + udpLBUUID,
			})

			fexec := &fakeexec.FakeExec{
				CommandScript: fakeCmds,
				LookPathFunc: func(file string) (string, error) {
					return fmt.Sprintf("/fake-bin/%s", file), nil
				},
			}

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}},
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			factory, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				close(stopChan)
			}()

			clusterController := NewClusterController(fakeClient, factory)
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

			Expect(fexec.CommandCalls).To(Equal(len(fakeCmds)))
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})
})

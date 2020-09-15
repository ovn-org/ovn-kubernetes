package ovn

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

//func newEgressFirewallMeta(name, namespace string) metav1.ObjectMeta {
func newObjectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       types.UID(namespace),
		Name:      name,
		Namespace: namespace,
	}

}

func newEgressFirewallObject(name, namespace string, egressRules []egressfirewallapi.EgressFirewallRule) *egressfirewallapi.EgressFirewall {

	return &egressfirewallapi.EgressFirewall{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: egressfirewallapi.EgressFirewallSpec{
			Egress: egressRules,
		},
	}
}

var _ = Describe("OVN EgressFirewall Operations", func() {
	var (
		app     *cli.App
		fakeOVN *FakeOVN
		fExec   *ovntest.FakeExec
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fExec = ovntest.NewLooseCompareFakeExec()
		fakeOVN = NewFakeOVN(fExec)

	})

	AfterEach(func() {
		fakeOVN.shutdown()
	})

	Context("on startup", func() {
		It("reconciles an existing egressFirewall", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
				)
				fExec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.dst == 1.2.3.4/23 && ip4.src == $a10481622940199974102\" action=allow external-ids:egressFirewall=namespace1",
					"ovn-nbctl --timeout=15 --id=@acl create acl priority=2000 direction=from-lport match=\"ip4.dst == 1.2.3.4/23 && ip4.src == $a10481622940199974102\" action=allow external-ids:egressFirewall=namespace1 -- add logical_switch join_node1 acls @acl",
				})

				namespace1 := *newNamespace("namespace1")
				egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Allow",
						To: egressfirewallapi.EgressFirewallDestination{
							CIDRSelector: "1.2.3.4/23",
						},
					},
				})

				fakeOVN.fakeEgressClient = egressfirewallfake.NewSimpleClientset([]runtime.Object{
					&egressfirewallapi.EgressFirewallList{
						Items: []egressfirewallapi.EgressFirewall{
							*egressFirewall,
						},
					},
				}...)
				fakeOVN.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						namespace1,
					},
				}, &v1.NodeList{
					Items: []v1.Node{
						{
							Status: v1.NodeStatus{
								Phase: v1.NodeRunning,
							},
							ObjectMeta: newObjectMeta(node1Name, ""),
						},
					},
				})
				fakeOVN.controller.WatchNamespaces()
				fakeOVN.controller.WatchEgressFirewall()

				_, err := fakeOVN.fakeEgressClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())

		})
	})
	Context("during execution", func() {
		It("correctly creates an egressfirewall denying traffic all udp traffic", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
				)
				fExec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.dst == 1.2.3.4/23 && ip4.src == $a10481622940199974102 && ( (udp) )\" action=drop external-ids:egressFirewall=namespace1",
					"ovn-nbctl --timeout=15 --id=@acl create acl priority=2000 direction=from-lport match=\"ip4.dst == 1.2.3.4/23 && ip4.src == $a10481622940199974102 && ( (udp) )\" action=drop external-ids:egressFirewall=namespace1 -- add logical_switch join_node1 acls @acl",
				})
				namespace1 := *newNamespace("namespace1")
				egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Deny",
						Ports: []egressfirewallapi.EgressFirewallPort{
							{
								Protocol: "UDP",
							},
						},
						To: egressfirewallapi.EgressFirewallDestination{
							CIDRSelector: "1.2.3.4/23",
						},
					},
				})
				fakeOVN.fakeEgressClient = egressfirewallfake.NewSimpleClientset([]runtime.Object{
					&egressfirewallapi.EgressFirewallList{
						Items: []egressfirewallapi.EgressFirewall{
							*egressFirewall,
						},
					},
				}...)
				fakeOVN.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						namespace1,
					},
				}, &v1.NodeList{
					Items: []v1.Node{
						{
							Status: v1.NodeStatus{
								Phase: v1.NodeRunning,
							},
							ObjectMeta: newObjectMeta(node1Name, ""),
						},
					},
				})
				fakeOVN.controller.WatchNamespaces()
				_, err := fakeOVN.fakeEgressClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				fakeOVN.controller.WatchEgressFirewall()

				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("correctly deletes an egressfirewall", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
				)
				fExec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.dst == 1.2.3.5/23 && " +
						"ip4.src == $a10481622940199974102 && ( (udp && ( udp.dst == 100 )) || (tcp) )\" action=allow external-ids:egressFirewall=namespace1",
					"ovn-nbctl --timeout=15 --id=@acl create acl priority=2000 direction=from-lport match=\"ip4.dst == 1.2.3.5/23 && " +
						"ip4.src == $a10481622940199974102 && ( (udp && ( udp.dst == 100 )) || (tcp) )\" action=allow external-ids:egressFirewall=namespace1 -- add logical_switch join_node1 acls @acl",
					fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_switch join_node1 acls %s", fakeUUID),
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL external-ids:egressFirewall=namespace1",
					Output: fmt.Sprintf("%s", fakeUUID),
				})

				namespace1 := *newNamespace("namespace1")
				egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Allow",
						Ports: []egressfirewallapi.EgressFirewallPort{
							{
								Protocol: "TCP",
							},
							{
								Protocol: "UDP",
								Port:     100,
							},
						},
						To: egressfirewallapi.EgressFirewallDestination{
							CIDRSelector: "1.2.3.5/23",
						},
					},
				})

				fakeOVN.fakeEgressClient = egressfirewallfake.NewSimpleClientset([]runtime.Object{
					&egressfirewallapi.EgressFirewallList{
						Items: []egressfirewallapi.EgressFirewall{
							*egressFirewall,
						},
					},
				}...)
				fakeOVN.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						namespace1,
					},
				}, &v1.NodeList{
					Items: []v1.Node{
						{
							Status: v1.NodeStatus{
								Phase: v1.NodeRunning,
							},
							ObjectMeta: newObjectMeta(node1Name, ""),
						},
					},
				})
				fakeOVN.controller.WatchNamespaces()
				fakeOVN.controller.WatchEgressFirewall()

				err := fakeOVN.fakeEgressClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())

				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("correctly updates an egressfirewall", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
				)
				fExec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.dst == 1.2.3.4/23 && ip4.src == $a10481622940199974102\" action=allow external-ids:egressFirewall=namespace1",
					"ovn-nbctl --timeout=15 --id=@acl create acl priority=2000 direction=from-lport match=\"ip4.dst == 1.2.3.4/23 && ip4.src == $a10481622940199974102\" action=allow external-ids:egressFirewall=namespace1 -- add logical_switch join_node1 acls @acl",
					fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_switch join_node1 acls %s", fakeUUID),
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.dst == 1.2.3.4/23 && ip4.src == $a10481622940199974102\" action=drop external-ids:egressFirewall=namespace1",
					"ovn-nbctl --timeout=15 --id=@acl create acl priority=2000 direction=from-lport match=\"ip4.dst == 1.2.3.4/23 && ip4.src == $a10481622940199974102\" action=drop external-ids:egressFirewall=namespace1 -- add logical_switch join_node1 acls @acl",
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL external-ids:egressFirewall=namespace1",
					Output: fmt.Sprintf("%s", fakeUUID),
				})

				namespace1 := *newNamespace("namespace1")
				egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Allow",
						To: egressfirewallapi.EgressFirewallDestination{
							CIDRSelector: "1.2.3.4/23",
						},
					},
				})
				egressFirewall1 := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Deny",
						To: egressfirewallapi.EgressFirewallDestination{
							CIDRSelector: "1.2.3.4/23",
						},
					},
				})

				fakeOVN.fakeEgressClient = egressfirewallfake.NewSimpleClientset([]runtime.Object{
					&egressfirewallapi.EgressFirewallList{
						Items: []egressfirewallapi.EgressFirewall{
							*egressFirewall,
						},
					},
				}...)
				fakeOVN.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						namespace1,
					},
				}, &v1.NodeList{
					Items: []v1.Node{
						{
							Status: v1.NodeStatus{
								Phase: v1.NodeRunning,
							},
							ObjectMeta: newObjectMeta(node1Name, ""),
						},
					},
				})
				fakeOVN.controller.WatchNamespaces()
				fakeOVN.controller.WatchEgressFirewall()

				_, err := fakeOVN.fakeEgressClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, err = fakeOVN.fakeEgressClient.K8sV1().EgressFirewalls(egressFirewall1.Namespace).Update(context.TODO(), egressFirewall1, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())

		})
		It("correctly adds an existing egressFirewall to a new node", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					node1Name string = "node1"
				)
				stopChan := make(chan struct{})
				defer close(stopChan)
				fExec.AddFakeCmdsNoOutputNoError([]string{
					//adding the original node commands
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name,other-config find logical_switch",
					"ovn-nbctl --timeout=15 --if-exists lrp-del rtos-node1 -- lrp-add ovn_cluster_router rtos-node1 ",
					"ovn-nbctl --timeout=15 --may-exist ls-add node1 -- set logical_switch node1",
					"ovn-nbctl --timeout=15 set logical_switch node1 other-config:mcast_snoop=\"true\"",
					"ovn-nbctl --timeout=15 set logical_switch node1 other-config:mcast_querier=\"false\"",
					"ovn-nbctl --timeout=15 -- --may-exist lsp-add node1 stor-node1 -- set logical_switch_port stor-node1 type=router options:router-port=rtos-node1 addresses=\"\"",
					"ovn-nbctl --timeout=15 set logical_switch node1 load_balancer=fakeTCPLoadBalancerUUID",
					"ovn-nbctl --timeout=15 add logical_switch node1 load_balancer fakeUDPLoadBalancerUUID",
					//adding the new node
					"ovn-nbctl --timeout=15 --if-exists lrp-del rtos-newNode -- lrp-add ovn_cluster_router rtos-newNode ",
					"ovn-nbctl --timeout=15 --may-exist ls-add newNode -- set logical_switch newNode",
					"ovn-nbctl --timeout=15 set logical_switch newNode other-config:mcast_snoop=\"true\"",
					"ovn-nbctl --timeout=15 set logical_switch newNode other-config:mcast_querier=\"false\"",
					"ovn-nbctl --timeout=15 -- --may-exist lsp-add newNode stor-newNode -- set logical_switch_port stor-newNode type=router options:router-port=rtos-newNode addresses=\"\"",
					"ovn-nbctl --timeout=15 set logical_switch newNode load_balancer=fakeTCPLoadBalancerUUID",
					"ovn-nbctl --timeout=15 add logical_switch newNode load_balancer fakeUDPLoadBalancerUUID",
				})

				fExec.AddFakeCmdsNoOutputNoError([]string{
					//adding the original egressFirewall
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.dst == 1.2.3.4/23 && ip4.src == $a10481622940199974102\" action=allow external-ids:egressFirewall=namespace1",
					"ovn-nbctl --timeout=15 --id=@acl create acl priority=2000 direction=from-lport match=\"ip4.dst == 1.2.3.4/23 && ip4.src == $a10481622940199974102\" action=allow external-ids:egressFirewall=namespace1 -- add logical_switch join_node1 acls @acl",
					"ovn-nbctl --timeout=15 add logical_switch join_newNode acls " + fakeUUID,
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					//query ovn and get the UUID of the original ACL
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"ip4.dst == 1.2.3.4/23 && ip4.src == $a10481622940199974102\" action=allow external-ids:egressFirewall=namespace1",
					Output: fakeUUID,
				})

				namespace1 := *newNamespace("namespace1")
				egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
					{
						Type: "Allow",
						To: egressfirewallapi.EgressFirewallDestination{
							CIDRSelector: "1.2.3.4/23",
						},
					},
				})
				newNode := &v1.Node{
					ObjectMeta: newObjectMeta("newNode", ""),
					Status: v1.NodeStatus{
						Addresses: []v1.NodeAddress{
							{Type: v1.NodeInternalIP, Address: "10.0.0.0"},
						},
					},
				}
				fakeOVN.fakeEgressClient = egressfirewallfake.NewSimpleClientset([]runtime.Object{
					&egressfirewallapi.EgressFirewallList{
						Items: []egressfirewallapi.EgressFirewall{
							*egressFirewall,
						},
					},
				}...)

				fakeOVN.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						namespace1,
					},
				}, &v1.NodeList{
					Items: []v1.Node{
						{
							Status: v1.NodeStatus{
								Phase: v1.NodeRunning,
							},
							ObjectMeta: newObjectMeta(node1Name, ""),
						},
					},
				})
				fakeOVN.controller.TCPLoadBalancerUUID = "fakeTCPLoadBalancerUUID"
				fakeOVN.controller.UDPLoadBalancerUUID = "fakeUDPLoadBalancerUUID"
				fakeOVN.controller.SCTPLoadBalancerUUID = "fakeSTCPLoadBalancerUUID"
				fakeOVN.controller.WatchNodes()
				fakeOVN.controller.WatchNamespaces()
				fakeOVN.controller.WatchEgressFirewall()

				_, err := fakeOVN.fakeEgressClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				_, err = fakeOVN.fakeClient.CoreV1().Nodes().Create(context.TODO(), newNode, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

	})

})

var _ = Describe("OVN test basic functions", func() {

	It("computes correct L4Match", func() {
		type testcase struct {
			ports         []egressfirewallapi.EgressFirewallPort
			expectedMatch string
		}
		testcases := []testcase{
			{
				expectedMatch: "",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
					},
				},
				expectedMatch: "(tcp)",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "UDP",
					},
				},
				expectedMatch: "(udp)",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "SCTP",
					},
				},
				expectedMatch: "(sctp)",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
				},
				expectedMatch: "(tcp && ( tcp.dst == 100 ))",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
					{
						Protocol: "UDP",
					},
				},
				expectedMatch: "(udp) || (tcp && ( tcp.dst == 100 ))",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
					{
						Protocol: "SCTP",
						Port:     13,
					},
					{
						Protocol: "TCP",
						Port:     102,
					},
					{
						Protocol: "UDP",
						Port:     400,
					},
				},
				expectedMatch: "(udp && ( udp.dst == 400 )) || (tcp && ( tcp.dst == 100 || tcp.dst == 102 )) || (sctp && ( sctp.dst == 13 ))",
			},
		}
		for _, test := range testcases {
			l4Match := egressGetL4Match(test.ports)
			Expect(test.expectedMatch).To(Equal(l4Match))
		}
	})
})

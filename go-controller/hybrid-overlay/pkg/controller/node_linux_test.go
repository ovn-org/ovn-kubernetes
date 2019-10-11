package controller

import (
	"fmt"
	"net"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/urfave/cli"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func addNodeSetupCmds(fexec *ovntest.FakeExec, nodeName, hybMAC, hybIP, ovsMAC, nodeSubnet string) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 get logical_switch mynode other-config:subnet",
		Output: nodeSubnet,
	})
	addGetPortAddressesCmds(fexec, nodeName, hybMAC, hybIP)
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovs-vsctl --timeout=15 br-exists br-ext",
		Err: fmt.Errorf("bridge br-ext not found"),
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 --may-exist add-br br-ext -- set Bridge br-ext fail_mode=secure",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface br-ext mac_in_use",
		Output: ovsMAC,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 set bridge br-ext other-config:hwaddr=" + ovsMAC,
		"ip link set br-ext up",
		"ovs-vsctl --timeout=15 --may-exist add-port br-int int -- --may-exist add-port br-ext ext -- set Interface int type=patch options:peer=ext external-ids:iface-id=int-" + nodeName + " -- set Interface ext type=patch options:peer=int ofport_request=11",
		"ovs-ofctl -O openflow13 add-flow br-ext table=0, priority=0, actions=drop",
	})
	hybMACRaw := strings.Replace(hybMAC, ":", "", -1)
	hybIPRaw := getIPAsHexString(net.ParseIP(hybIP))
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-ofctl -O openflow13 add-flow br-ext table=0, priority=100, in_port=11, arp, arp_tpa=" + hybIP + ", actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],mod_dl_src:" + hybMAC + ",load:0x2->NXM_OF_ARP_OP[],move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],load:0x" + hybMACRaw + "->NXM_NX_ARP_SHA[],load:0x" + hybIPRaw + "->NXM_OF_ARP_SPA[],IN_PORT",
		`ovs-vsctl --timeout=15 --may-exist add-port br-ext ext-vxlan -- set interface ext-vxlan ofport_request=1 type=vxlan options:remote_ip="flow" options:key="flow"`,
		"ovs-ofctl -O openflow13 add-flow br-ext table=0, priority=100, in_port=ext-vxlan, ip, nw_dst=" + nodeSubnet + ", dl_dst=" + hybMAC + ", actions=goto_table:10",
		"ovs-ofctl -O openflow13 add-flow br-ext table=10, priority=0, actions=drop",
	})
}

func createNode(name, os, ip string, annotations map[string]string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				v1.LabelOSStable: os,
			},
			Annotations: annotations,
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: ip},
			},
		},
	}
}

func createPod(namespace, name, node, podIP, podMAC string) *v1.Pod {
	annotations := map[string]string{}
	if podIP != "" || podMAC != "" {
		annotations["ovn"] = fmt.Sprintf(`{"ip_address":"%s", "mac_address":"%s"}`, podIP, podMAC)
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        name,
			Annotations: annotations,
		},
		Spec: v1.PodSpec{
			NodeName: node,
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	}
}

var _ = Describe("Hybrid Overlay Node Linux Operations", func() {
	var app *cli.App
	const (
		thisNode string = "mynode"
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	It("does not set up tunnels for non-Windows nodes without annotations", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				node1Name   string = "node1"
				node1Subnet string = "1.2.4.0/24"
				node1IP     string = "10.0.0.2"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					*createNode(node1Name, "linux", "10.0.0.1", nil),
				},
			})

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-ofctl dump-flows br-ext table=0",
				// Assume fresh OVS bridge
				Output: "",
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			n, err := NewNode(fakeClient, thisNode)
			Expect(err).NotTo(HaveOccurred())

			err = n.startNodeWatch(f)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})

	It("does not set up tunnels for non-Windows nodes with subnet annotations", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				node1Name   string = "node1"
				node1Subnet string = "1.2.4.0/24"
				node1IP     string = "10.0.0.2"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					*createNode(node1Name, "linux", node1IP, map[string]string{
						types.HybridOverlayHostSubnet: node1Subnet,
						"ovn_host_subnet":             node1Subnet,
					}),
				},
			})

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-ofctl dump-flows br-ext table=0",
				// Assume fresh OVS bridge
				Output: "",
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			n, err := NewNode(fakeClient, thisNode)
			Expect(err).NotTo(HaveOccurred())

			err = n.startNodeWatch(f)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sets up local node hybrid overlay bridge", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				thisNode   string = "mynode"
				hybMAC     string = "00:00:00:7a:af:04"
				hybIP      string = "1.2.3.3"
				thisSubnet string = "1.2.3.0/24"
				ovsMAC     string = "11:22:33:44:55:66"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					*createNode(thisNode, "linux", "10.0.0.1", nil),
				},
			})

			fexec := ovntest.NewFakeExec()
			// Node setup from initial node sync
			addNodeSetupCmds(fexec, thisNode, hybMAC, hybIP, ovsMAC, thisSubnet)
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-ofctl dump-flows br-ext table=0",
				// Assume fresh OVS bridge
				Output: "",
			})
			// Second check from node watch Add event
			addGetPortAddressesCmds(fexec, thisNode, hybMAC, hybIP)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				// Bridge now exists
				"ovs-vsctl --timeout=15 br-exists br-ext",
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			ipt, err := util.NewFakeWithProtocol(iptables.ProtocolIPv4)
			Expect(err).NotTo(HaveOccurred())
			util.SetIPTablesHelper(iptables.ProtocolIPv4, ipt)

			n, err := NewNode(fakeClient, thisNode)
			Expect(err).NotTo(HaveOccurred())

			err = n.startNodeWatch(f)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())

			expectedTables := map[string]util.FakeTable{
				"filter": {
					"INPUT": []string{"-p udp -m udp --dport 4789 -j ACCEPT"},
				},
				"nat": {},
			}
			Expect(ipt.MatchState(expectedTables)).NotTo(HaveOccurred())

			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sets up tunnels for Windows nodes", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				thisNode    string = "mynode"
				hybMAC      string = "00:00:00:7a:af:04"
				hybIP       string = "1.2.3.3"
				thisSubnet  string = "1.2.3.0/24"
				ovsMAC      string = "11:22:33:44:55:66"
				node1Name   string = "node1"
				node1IP     string = "10.0.0.2"
				node1DrMAC  string = "22:33:44:55:66:77"
				node1Subnet string = "5.6.7.0/24"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					*createNode(node1Name, "windows", node1IP, map[string]string{
						types.HybridOverlayHostSubnet: node1Subnet,
						types.HybridOverlayDrMac:      node1DrMAC,
					}),
				},
			})

			node1DrMACRaw := strings.Replace(node1DrMAC, ":", "", -1)

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovs-ofctl dump-flows br-ext table=0",
				// Adds flows for existing node1
				"ovs-ofctl add-flow br-ext cookie=0xca12f31b,table=0,priority=100,arp,in_port=ext,arp_tpa=" + node1Subnet + " actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],mod_dl_src:" + node1DrMAC + ",load:0x2->NXM_OF_ARP_OP[],move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],load:0x" + node1DrMACRaw + "->NXM_NX_ARP_SHA[],move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],IN_PORT",
				"ovs-ofctl add-flow br-ext cookie=0xca12f31b,table=0,priority=100,ip,nw_dst=5.6.7.0/24,actions=load:4097->NXM_NX_TUN_ID[0..31],set_field:10.0.0.2->tun_dst,set_field:22:33:44:55:66:77->eth_dst,output:1",
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			n, err := NewNode(fakeClient, thisNode)
			Expect(err).NotTo(HaveOccurred())

			err = n.startNodeWatch(f)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())

			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})

	It("removes stale node flows on initial sync", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				thisNode  string = "mynode"
				node1Name string = "node1"
				node1IP   string = "10.0.0.2"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{*createNode(node1Name, "linux", node1IP, nil)},
			})

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-ofctl dump-flows br-ext table=0",
				// Return output for a stale node
				Output: ` cookie=0x0, duration=137014.498s, table=0, n_packets=20, n_bytes=2605, priority=100,ip,in_port="ext-vxlan",dl_dst=00:00:00:4d:d9:c1,nw_dst=10.128.1.0/24 actions=resubmit(,10)
 cookie=0x1f40e27c, duration=61107.432s, table=0, n_packets=0, n_bytes=0, priority=100,arp,in_port=ext,arp_tpa=11.128.0.0/24 actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],mod_dl_src:00:00:00:33:65:d0,load:0x2->NXM_OF_ARP_OP[],move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],load:0x3365d0->NXM_NX_ARP_SHA[],move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],IN_PORT
 cookie=0x1f40e27c, duration=61107.417s, table=0, n_packets=0, n_bytes=0, priority=100,ip,nw_dst=11.128.0.0/24 actions=load:0x1001->NXM_NX_TUN_ID[0..31],load:0xac110003->NXM_NX_TUN_IPV4_DST[],mod_dl_dst:00:00:00:33:65:d0,output:"ext-vxlan"
 cookie=0x0, duration=61107.658s, table=0, n_packets=50, n_bytes=3576, priority=0 actions=drop`,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				// Deletes flows for stale node in OVS
				"ovs-ofctl del-flows br-ext table=0, cookie=0x1f40e27c/0xffffffff",
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			n, err := NewNode(fakeClient, thisNode)
			Expect(err).NotTo(HaveOccurred())

			err = n.startNodeWatch(f)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())

			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})

	It("removes stale pod flows on initial sync", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				thisNode string = "mynode"
			)

			fakeClient := fake.NewSimpleClientset()

			fexec := ovntest.NewFakeExec()
			// Put one live pod and one stale pod into the OVS bridge flows
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-ofctl dump-flows br-ext table=10",
				Output: ` cookie=0xaabbccdd, duration=29398.539s, table=10, n_packets=0, n_bytes=0, priority=100,ip,nw_dst=1.2.3.4 actions=mod_dl_src:ab:cd:ef:ab:cd:ef,mod_dl_dst:ef:cd:ab:ef:cd:ab,output:ext
 cookie=0x0, duration=29398.687s, table=10, n_packets=0, n_bytes=0, priority=0 actions=drop`,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				// Deletes flows for pod in OVS that is not in Kube
				"ovs-ofctl del-flows br-ext table=10, cookie=0xaabbccdd/0xffffffff",
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			n, err := NewNode(fakeClient, thisNode)
			Expect(err).NotTo(HaveOccurred())

			err = n.startPodWatch(f)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())

			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sets up local pod flows", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				thisNode  string = "mynode"
				hybMAC    string = "00:00:00:7a:af:04"
				node1Name string = "node1"
				pod1IP    string = "1.2.3.5"
				pod1CIDR  string = pod1IP + "/24"
				pod1MAC   string = "aa:bb:cc:dd:ee:ff"
			)

			fakeClient := fake.NewSimpleClientset([]runtime.Object{
				createPod("default", "pod1", thisNode, pod1CIDR, pod1MAC),
			}...)

			fexec := ovntest.NewFakeExec()
			// Put one live pod and one stale pod into the OVS bridge flows
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-ofctl dump-flows br-ext table=10",
				Output: ` cookie=0x7fdcde17, duration=29398.539s, table=10, n_packets=0, n_bytes=0, priority=100,ip,nw_dst=` + pod1CIDR + ` actions=mod_dl_src:` + hybMAC + `,mod_dl_dst:` + pod1MAC + `,output:ext
 cookie=0x0, duration=29398.687s, table=10, n_packets=0, n_bytes=0, priority=0 actions=drop`,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				// Refreshes flows for pod that is in OVS and in Kube
				"ovs-ofctl add-flow br-ext table=10, cookie=0x7fdcde17, priority=100, ip, nw_dst=" + pod1CIDR + ", actions=set_field:" + hybMAC + "->eth_src,set_field:" + pod1MAC + "->eth_dst,output:ext",
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			n, err := NewNode(fakeClient, thisNode)
			Expect(err).NotTo(HaveOccurred())
			n.drMAC = hybMAC

			err = n.startPodWatch(f)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())

			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})
})

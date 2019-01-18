// +build linux

package cluster

import (
	"bytes"
	"fmt"
	"net"
	"syscall"

	"github.com/urfave/cli"
	"k8s.io/client-go/kubernetes/fake"
	fakeexec "k8s.io/utils/exec/testing"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func addNodeportLBs(nodeName, tcpLBUUID, udpLBUUID string, fakeCmds []fakeexec.FakeCommandAction) []fakeexec.FakeCommandAction {
	fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_" + nodeName,
	})
	fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 -- create load_balancer external_ids:TCP_lb_gateway_router=GR_" + nodeName,
		Output: tcpLBUUID,
	})
	fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
		"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=GR_" + nodeName,
	})
	fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 -- create load_balancer external_ids:UDP_lb_gateway_router=GR_" + nodeName + " protocol=udp",
		Output: udpLBUUID,
	})
	fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
		"ovn-nbctl --timeout=15 set logical_router GR_node1 load_balancer=" + tcpLBUUID,
		"ovn-nbctl --timeout=15 add logical_router GR_node1 load_balancer " + udpLBUUID,
	})
	return fakeCmds
}

var _ = Describe("Gateway Init Operations", func() {
	var app *cli.App
	var testNS ns.NetNS

	BeforeEach(func() {
		var err error
		testNS, err = ns.NewNS()
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
				nodeName          string = "node1"
				lrpMAC            string = "00:00:00:05:46:C3"
				brLocalnetMAC     string = "11:22:33:44:55:66"
				lrpIP             string = "100.64.2.3"
				lrpCIDR           string = lrpIP + "/24"
				clusterRouterUUID string = "5cedba03-679f-41f3-b00e-b8ed7437bc6c"
				systemID          string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
				tcpLBUUID         string = "d2e858b2-cb5a-441b-a670-ed450f79a91f"
				udpLBUUID         string = "12832f14-eb0f-44d4-b8db-4cccbc73c792"
				nodeSubnet        string = "10.1.1.0/24"
				gwRouter          string = "GR_" + nodeName
				clusterIPNet      string = "10.1.0.0"
				clusterCIDR       string = clusterIPNet + "/16"
			)

			fakeCmds := ovntest.AddFakeCmdsNoOutputNoError(nil, []string{
				"ovs-vsctl --timeout=15 --may-exist add-br br-localnet",
				"ip link set br-localnet up",
				"ovs-vsctl --timeout=15 --may-exist add-port br-localnet br-nexthop -- set interface br-nexthop type=internal",
				"ip link set br-nexthop up",
				"ip addr flush dev br-nexthop",
				"ip addr add 169.254.33.1/24 dev br-nexthop",
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router external_ids:k8s-cluster-router=yes",
				Output: clusterRouterUUID,
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
				Output: systemID,
			})
			fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
				"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=169.254.33.2",
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-" + gwRouter + " mac",
				Output: lrpMAC,
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --if-exists get logical_router_port rtoj-" + gwRouter + " networks",
				Output: "[" + lrpCIDR + "]",
			})
			fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add join jtor-" + gwRouter + " -- set logical_switch_port jtor-" + gwRouter + " type=router options:router-port=rtoj-" + gwRouter + " addresses=\"" + lrpMAC + "\"",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " 100.64.1.1",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouterUUID + " 0.0.0.0/0 100.64.1.2",
			})
			fakeCmds = addNodeportLBs(nodeName, tcpLBUUID, udpLBUUID, fakeCmds)
			fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
				"ovn-nbctl --timeout=15 --may-exist ls-add ext_" + nodeName,
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface br-localnet mac_in_use",
				Output: brLocalnetMAC,
			})
			fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
				"ovs-vsctl --timeout=15 set bridge br-localnet other-config:hwaddr=" + brLocalnetMAC,
				"ovs-vsctl --timeout=15 --may-exist add-port br-localnet k8s-patch-br-localnet-br-int -- set interface k8s-patch-br-localnet-br-int type=patch options:peer=k8s-patch-br-int-br-localnet",
				"ovs-vsctl --timeout=15 --may-exist add-port br-int k8s-patch-br-int-br-localnet -- set interface k8s-patch-br-int-br-localnet type=patch options:peer=k8s-patch-br-localnet-br-int external-ids:iface-id=br-localnet_" + nodeName,
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " br-localnet_" + nodeName + " -- lsp-set-addresses br-localnet_" + nodeName + " unknown",
				"ovn-nbctl --timeout=15 -- --may-exist lrp-add " + gwRouter + " rtoe-" + gwRouter + " " + brLocalnetMAC + " 169.254.33.2/24 -- set logical_router_port rtoe-" + gwRouter + " external-ids:gateway-physical-ip=yes",
				"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " etor-" + gwRouter + " -- set logical_switch_port etor-" + gwRouter + " type=router options:router-port=rtoe-" + gwRouter + " addresses=\"" + brLocalnetMAC + "\"",
				"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 169.254.33.1 rtoe-" + gwRouter,
				"ovn-nbctl --timeout=15 --may-exist lr-nat-add " + gwRouter + " snat 169.254.33.2 " + clusterCIDR,
				"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
				"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + clusterRouterUUID + " " + nodeSubnet + " " + lrpIP,
			})

			fexec := &fakeexec.FakeExec{
				CommandScript: fakeCmds,
				LookPathFunc: func(file string) (string, error) {
					return fmt.Sprintf("/fake-bin/%s", file), nil
				},
			}

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			ipt, err := util.NewFakeWithProtocol(iptables.ProtocolIPv4)
			Expect(err).NotTo(HaveOccurred())
			err = initLocalnetGatewayInternal(nodeName, []string{clusterCIDR}, nodeSubnet, ipt, true)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CommandCalls).To(Equal(len(fakeCmds)))

			expectedTables := map[string]util.FakeTable{
				"filter": {
					"INPUT": []string{
						"-i br-nexthop -m comment --comment from OVN to localhost -j ACCEPT",
					},
					"FORWARD": []string{
						"-i br-nexthop -j ACCEPT",
						"-o br-nexthop -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
					},
				},
				"nat": {
					"POSTROUTING": []string{
						"-s 169.254.33.2/24 -j MASQUERADE",
					},
				},
			}
			Expect(ipt.MatchState(expectedTables)).NotTo(HaveOccurred())
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})

	Context("for NIC-based operations", func() {
		const (
			eth0Name string = "eth0"
			eth0IP   string = "192.168.1.10"
			eth0CIDR string = eth0IP + "/24"
			eth0GWIP string = "192.168.1.1"
		)
		var eth0MAC string

		BeforeEach(func() {
			// Set up a fake eth0
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				err := netlink.LinkAdd(&netlink.Dummy{
					LinkAttrs: netlink.LinkAttrs{
						Name: eth0Name,
					},
				})
				Expect(err).NotTo(HaveOccurred())
				l, err := netlink.LinkByName(eth0Name)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(l)
				Expect(err).NotTo(HaveOccurred())

				// Add an IP address
				addr, err := netlink.ParseAddr(eth0CIDR)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.AddrAdd(l, addr)
				Expect(err).NotTo(HaveOccurred())

				eth0MAC = l.Attrs().HardwareAddr.String()

				// And a default route
				_, ipn, err := net.ParseCIDR("0.0.0.0/0")
				Expect(err).NotTo(HaveOccurred())
				gw := net.ParseIP(eth0GWIP)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.RouteAdd(&netlink.Route{
					LinkIndex: l.Attrs().Index,
					Scope:     netlink.SCOPE_UNIVERSE,
					Dst:       ipn,
					Gw:        gw,
				})
				Expect(err).NotTo(HaveOccurred())

				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("sets up a shared interface gateway", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeName          string = "node1"
					lrpMAC            string = "00:00:00:05:46:C3"
					lrpIP             string = "100.64.2.3"
					lrpCIDR           string = lrpIP + "/24"
					clusterRouterUUID string = "5cedba03-679f-41f3-b00e-b8ed7437bc6c"
					systemID          string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
					tcpLBUUID         string = "d2e858b2-cb5a-441b-a670-ed450f79a91f"
					udpLBUUID         string = "12832f14-eb0f-44d4-b8db-4cccbc73c792"
					nodeSubnet        string = "10.1.1.0/24"
					gwRouter          string = "GR_" + nodeName
					clusterIPNet      string = "10.1.0.0"
					clusterCIDR       string = clusterIPNet + "/16"
				)

				fakeCmds := ovntest.AddFakeCmd(nil, &ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 -- br-exists eth0",
					Err: fmt.Errorf(""),
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 -- --may-exist add-br breth0 -- br-set-external-id breth0 bridge-id breth0 -- set bridge breth0 fail-mode=standalone other_config:hwaddr=" + eth0MAC + " -- --may-exist add-port breth0 eth0 -- set port eth0 other-config:transient=true",
					Action: func() error {
						return testNS.Do(func(ns.NetNS) error {
							defer GinkgoRecover()

							hwaddr, err := net.ParseMAC(eth0MAC)
							Expect(err).NotTo(HaveOccurred())

							// Create breth0 as a dummy link
							err = netlink.LinkAdd(&netlink.Dummy{
								LinkAttrs: netlink.LinkAttrs{
									Name:         "br" + eth0Name,
									HardwareAddr: hwaddr,
								},
							})
							Expect(err).NotTo(HaveOccurred())
							_, err = netlink.LinkByName("br" + eth0Name)
							Expect(err).NotTo(HaveOccurred())
							return nil
						})
					},
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router external_ids:k8s-cluster-router=yes",
					Output: clusterRouterUUID,
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
					Output: systemID,
				})
				fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
					"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=" + eth0IP,
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-" + gwRouter + " mac",
					Output: lrpMAC,
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --if-exists get logical_router_port rtoj-" + gwRouter + " networks",
					Output: "[" + lrpCIDR + "]",
				})
				fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
					"ovn-nbctl --timeout=15 -- --may-exist lsp-add join jtor-" + gwRouter + " -- set logical_switch_port jtor-" + gwRouter + " type=router options:router-port=rtoj-" + gwRouter + " addresses=\"" + lrpMAC + "\"",
					"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " 100.64.1.1",
					"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouterUUID + " 0.0.0.0/0 100.64.1.2",
				})
				fakeCmds = addNodeportLBs(nodeName, tcpLBUUID, udpLBUUID, fakeCmds)
				fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
					"ovn-nbctl --timeout=15 --may-exist ls-add ext_" + nodeName,
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 mac_in_use",
					Output: eth0MAC,
				})
				fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
					"ovs-vsctl --timeout=15 set bridge breth0 other-config:hwaddr=" + eth0MAC,
					"ovs-vsctl --timeout=15 --may-exist add-port breth0 k8s-patch-breth0-br-int -- set interface k8s-patch-breth0-br-int type=patch options:peer=k8s-patch-br-int-breth0",
					"ovs-vsctl --timeout=15 --may-exist add-port br-int k8s-patch-br-int-breth0 -- set interface k8s-patch-br-int-breth0 type=patch options:peer=k8s-patch-breth0-br-int external-ids:iface-id=breth0_" + nodeName,
					"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " breth0_" + nodeName + " -- lsp-set-addresses breth0_" + nodeName + " unknown",
					"ovn-nbctl --timeout=15 -- --may-exist lrp-add " + gwRouter + " rtoe-" + gwRouter + " " + eth0MAC + " " + eth0CIDR + " -- set logical_router_port rtoe-" + gwRouter + " external-ids:gateway-physical-ip=yes",
					"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " etor-" + gwRouter + " -- set logical_switch_port etor-" + gwRouter + " type=router options:router-port=rtoe-" + gwRouter + " addresses=\"" + eth0MAC + "\"",
					"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 " + eth0GWIP + " rtoe-" + gwRouter,
					"ovn-nbctl --timeout=15 --may-exist lr-nat-add " + gwRouter + " snat " + eth0IP + " " + clusterCIDR,
					"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
					"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + clusterRouterUUID + " " + nodeSubnet + " " + lrpIP,
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface k8s-patch-breth0-br-int ofport",
					Output: "5",
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface eth0 ofport",
					Output: "7",
				})
				fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
					"ovs-ofctl add-flow breth0 priority=100, in_port=5, ip, actions=ct(commit, zone=64000), output:7",
					"ovs-ofctl add-flow breth0 priority=50, in_port=7, ip, actions=ct(zone=64000, table=1)",
					"ovs-ofctl add-flow breth0 priority=100, table=1, ct_state=+trk+est, actions=output:5",
					"ovs-ofctl add-flow breth0 priority=100, table=1, ct_state=+trk+rel, actions=output:5",
					"ovs-ofctl add-flow breth0 priority=0, table=1, actions=output:LOCAL",
				})
				// nodePortWatcher()
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface k8s-patch-breth0-br-int ofport",
					Output: "9",
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface eth0 ofport",
					Output: "11",
				})
				// syncServices()
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface eth0 ofport",
					Output: "11",
				})

				// syncServices()
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl dump-flows breth0",
					Output: `cookie=0x0, duration=8366.605s, table=0, n_packets=0, n_bytes=0, priority=100,ip,in_port="k8s-patch-breth" actions=ct(commit,zone=64000),output:eth0
cookie=0x0, duration=8366.603s, table=0, n_packets=10642, n_bytes=10370438, priority=50,ip,in_port=eth0 actions=ct(table=1,zone=64000)
cookie=0x0, duration=8366.705s, table=0, n_packets=11549, n_bytes=1746901, priority=0 actions=NORMAL
cookie=0x0, duration=8366.602s, table=1, n_packets=0, n_bytes=0, priority=100,ct_state=+est+trk actions=output:"k8s-patch-breth"
cookie=0x0, duration=8366.600s, table=1, n_packets=0, n_bytes=0, priority=100,ct_state=+rel+trk actions=output:"k8s-patch-breth"
cookie=0x0, duration=8366.597s, table=1, n_packets=10641, n_bytes=10370087, priority=0 actions=LOCAL
`,
				})

				fexec := &fakeexec.FakeExec{
					CommandScript: fakeCmds,
					LookPathFunc: func(file string) (string, error) {
						return fmt.Sprintf("/fake-bin/%s", file), nil
					},
				}

				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				fakeClient := &fake.Clientset{}
				stop := make(chan struct{})
				wf, err := factory.NewWatchFactory(fakeClient, stop)
				Expect(err).NotTo(HaveOccurred())

				cluster := OvnClusterController{
					watchFactory:     wf,
					GatewayInit:      true,
					GatewayIntf:      eth0Name,
					GatewaySpareIntf: false,
					NodePortEnable:   true,
					LocalnetGateway:  false,
				}

				ipt, err := util.NewFakeWithProtocol(iptables.ProtocolIPv4)
				Expect(err).NotTo(HaveOccurred())

				err = testNS.Do(func(ns.NetNS) error {
					defer GinkgoRecover()

					err = cluster.initGateway(nodeName, []string{clusterCIDR}, nodeSubnet)
					Expect(err).NotTo(HaveOccurred())

					// Verify the code moved eth0's IP address, MAC, and routes
					// over to breth0
					l, err := netlink.LinkByName("breth0")
					Expect(err).NotTo(HaveOccurred())
					addrs, err := netlink.AddrList(l, syscall.AF_INET)
					Expect(err).NotTo(HaveOccurred())
					var found bool
					expectedAddr, err := netlink.ParseAddr(eth0CIDR)
					Expect(err).NotTo(HaveOccurred())
					for _, a := range addrs {
						if a.IP.Equal(expectedAddr.IP) && bytes.Equal(a.Mask, expectedAddr.Mask) {
							found = true
							break
						}
					}
					Expect(found).To(BeTrue())

					Expect(l.Attrs().HardwareAddr.String()).To(Equal(eth0MAC))
					return nil
				})

				Expect(fexec.CommandCalls).To(Equal(len(fakeCmds)))

				expectedTables := map[string]util.FakeTable{
					"filter": {},
					"nat":    {},
				}
				Expect(ipt.MatchState(expectedTables)).NotTo(HaveOccurred())
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("sets up a spare interface gateway", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeName          string = "node1"
					lrpMAC            string = "00:00:00:05:46:C3"
					lrpIP             string = "100.64.2.3"
					lrpCIDR           string = lrpIP + "/24"
					clusterRouterUUID string = "5cedba03-679f-41f3-b00e-b8ed7437bc6c"
					systemID          string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
					tcpLBUUID         string = "d2e858b2-cb5a-441b-a670-ed450f79a91f"
					udpLBUUID         string = "12832f14-eb0f-44d4-b8db-4cccbc73c792"
					nodeSubnet        string = "10.1.1.0/24"
					gwRouter          string = "GR_" + nodeName
					clusterIPNet      string = "10.1.0.0"
					clusterCIDR       string = clusterIPNet + "/16"
				)

				fakeCmds := ovntest.AddFakeCmd(nil, &ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router external_ids:k8s-cluster-router=yes",
					Output: clusterRouterUUID,
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
					Output: systemID,
				})
				fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
					"ovn-nbctl --timeout=15 -- --may-exist lr-add " + gwRouter + " -- set logical_router " + gwRouter + " options:chassis=" + systemID + " external_ids:physical_ip=" + eth0IP,
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --if-exist get logical_router_port rtoj-" + gwRouter + " mac",
					Output: lrpMAC,
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --if-exists get logical_router_port rtoj-" + gwRouter + " networks",
					Output: "[" + lrpCIDR + "]",
				})
				fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
					"ovn-nbctl --timeout=15 -- --may-exist lsp-add join jtor-" + gwRouter + " -- set logical_switch_port jtor-" + gwRouter + " type=router options:router-port=rtoj-" + gwRouter + " addresses=\"" + lrpMAC + "\"",
					"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " " + clusterCIDR + " 100.64.1.1",
					"ovn-nbctl --timeout=15 --may-exist lr-route-add " + clusterRouterUUID + " 0.0.0.0/0 100.64.1.2",
				})
				fakeCmds = addNodeportLBs(nodeName, tcpLBUUID, udpLBUUID, fakeCmds)
				fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
					"ovn-nbctl --timeout=15 --may-exist ls-add ext_" + nodeName,
					"ovs-vsctl --timeout=15 -- --may-exist add-port br-int eth0 -- set interface eth0 external-ids:iface-id=eth0_" + nodeName,
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface eth0 mac_in_use",
					Output: eth0MAC,
				})
				fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
					"ip addr flush dev eth0",
					"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " eth0_" + nodeName + " -- lsp-set-addresses eth0_" + nodeName + " unknown",
					"ovn-nbctl --timeout=15 -- --may-exist lrp-add " + gwRouter + " rtoe-" + gwRouter + " " + eth0MAC + " " + eth0CIDR + " -- set logical_router_port rtoe-" + gwRouter + " external-ids:gateway-physical-ip=yes",
					"ovn-nbctl --timeout=15 -- --may-exist lsp-add ext_" + nodeName + " etor-" + gwRouter + " -- set logical_switch_port etor-" + gwRouter + " type=router options:router-port=rtoe-" + gwRouter + " addresses=\"" + eth0MAC + "\"",
					"ovn-nbctl --timeout=15 --may-exist lr-route-add " + gwRouter + " 0.0.0.0/0 " + eth0GWIP + " rtoe-" + gwRouter,
					"ovn-nbctl --timeout=15 --may-exist lr-nat-add " + gwRouter + " snat " + eth0IP + " " + clusterCIDR,
					"ovn-nbctl --timeout=15 set logical_router " + gwRouter + " options:lb_force_snat_ip=" + lrpIP,
					"ovn-nbctl --timeout=15 --may-exist --policy=src-ip lr-route-add " + clusterRouterUUID + " " + nodeSubnet + " " + lrpIP,
				})

				fexec := &fakeexec.FakeExec{
					CommandScript: fakeCmds,
					LookPathFunc: func(file string) (string, error) {
						return fmt.Sprintf("/fake-bin/%s", file), nil
					},
				}

				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				fakeClient := &fake.Clientset{}
				stop := make(chan struct{})
				wf, err := factory.NewWatchFactory(fakeClient, stop)
				Expect(err).NotTo(HaveOccurred())

				cluster := OvnClusterController{
					watchFactory:     wf,
					GatewayInit:      true,
					GatewayIntf:      eth0Name,
					GatewaySpareIntf: true,
					NodePortEnable:   true,
					LocalnetGateway:  false,
				}

				ipt, err := util.NewFakeWithProtocol(iptables.ProtocolIPv4)
				Expect(err).NotTo(HaveOccurred())

				err = testNS.Do(func(ns.NetNS) error {
					defer GinkgoRecover()

					err = cluster.initGateway(nodeName, []string{clusterCIDR}, nodeSubnet)
					Expect(err).NotTo(HaveOccurred())

					// Verify the code didn't touch eth0's IP and MAC
					l, err := netlink.LinkByName("eth0")
					Expect(err).NotTo(HaveOccurred())
					addrs, err := netlink.AddrList(l, syscall.AF_INET)
					Expect(err).NotTo(HaveOccurred())
					var found bool
					expectedAddr, err := netlink.ParseAddr(eth0CIDR)
					Expect(err).NotTo(HaveOccurred())
					for _, a := range addrs {
						if a.IP.Equal(expectedAddr.IP) && bytes.Equal(a.Mask, expectedAddr.Mask) {
							found = true
							break
						}
					}
					Expect(found).To(BeTrue())
					Expect(l.Attrs().HardwareAddr.String()).To(Equal(eth0MAC))
					return nil
				})

				Expect(fexec.CommandCalls).To(Equal(len(fakeCmds)))

				expectedTables := map[string]util.FakeTable{
					"filter": {},
					"nat":    {},
				}
				Expect(ipt.MatchState(expectedTables)).NotTo(HaveOccurred())
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

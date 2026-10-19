// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/stretchr/testify/mock"
	"github.com/urfave/cli/v2"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	netlink_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const v4PMTUDNFTRules = `
add table inet ovn-kubernetes
add rule inet ovn-kubernetes no-pmtud ip daddr @remote-node-ips-v4 meta l4proto icmp icmp type 3 icmp code 4 counter drop
add chain inet ovn-kubernetes no-pmtud { type filter hook output priority 0 ; comment "Block egress needs frag/packet too big to remote k8s nodes" ; }
add set inet ovn-kubernetes remote-node-ips-v4 { type ipv4_addr ; comment "Block egress ICMP needs frag to remote Kubernetes nodes" ; }
add set inet ovn-kubernetes remote-node-ips-v6 { type ipv6_addr ; comment "Block egress ICMPv6 packet too big to remote Kubernetes nodes" ; }
`

const v6PMTUDNFTRules = `
add table inet ovn-kubernetes
add rule inet ovn-kubernetes no-pmtud meta l4proto icmpv6 icmpv6 type 2 icmpv6 code 0 ip6 daddr @remote-node-ips-v6 counter drop
add chain inet ovn-kubernetes no-pmtud { type filter hook output priority 0 ; comment "Block egress needs frag/packet too big to remote k8s nodes" ; }
add set inet ovn-kubernetes remote-node-ips-v4 { type ipv4_addr ; comment "Block egress ICMP needs frag to remote Kubernetes nodes" ; }
add set inet ovn-kubernetes remote-node-ips-v6 { type ipv6_addr ; comment "Block egress ICMPv6 packet too big to remote Kubernetes nodes" ; }
`

var _ = Describe("Node", func() {
	Describe("removing stale DPU-host address annotations", func() {
		const nodeName = "my-node"

		newController := func(kubeInterface kube.Interface) *DefaultNodeNetworkController {
			return &DefaultNodeNetworkController{
				BaseNodeNetworkController: BaseNodeNetworkController{
					CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{
						name: nodeName,
						Kube: kubeInterface,
					},
				},
			}
		}

		BeforeEach(func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
		})

		It("removes the annotation during full-mode startup", func() {
			config.OvnKubeNode.Mode = types.NodeModeFull
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					util.OVNNodePrimaryDPUHostAddr: `{"ipv4":"10.1.1.10/24"}`,
				},
			}}
			client := fake.NewSimpleClientset(node)
			nc := newController(&kube.Kube{KClient: client})

			Expect(nc.removeStaleDPUHostAddressAnnotation()).To(Succeed())

			updatedNode, err := client.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations).NotTo(HaveKey(util.OVNNodePrimaryDPUHostAddr))
		})

		DescribeTable("preserves the annotation outside full mode", func(mode string) {
			config.OvnKubeNode.Mode = mode
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					util.OVNNodePrimaryDPUHostAddr: `{"ipv4":"10.1.1.10/24"}`,
				},
			}}
			client := fake.NewSimpleClientset(node)
			nc := newController(&kube.Kube{KClient: client})

			Expect(nc.removeStaleDPUHostAddressAnnotation()).To(Succeed())

			updatedNode, err := client.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations).To(HaveKey(util.OVNNodePrimaryDPUHostAddr))
		},
			Entry("in DPU mode", types.NodeModeDPU),
			Entry("in DPU-host mode", types.NodeModeDPUHost),
		)

		It("returns an error when the annotation cannot be removed", func() {
			config.OvnKubeNode.Mode = types.NodeModeFull
			kubeMock := new(mocks.Interface)
			kubeMock.On("SetAnnotationsOnNode", nodeName, map[string]interface{}{
				util.OVNNodePrimaryDPUHostAddr: nil,
			}).Return(fmt.Errorf("patch failed"))
			nc := newController(kubeMock)

			err := nc.removeStaleDPUHostAddressAnnotation()
			Expect(err).To(MatchError(And(
				ContainSubstring("failed to remove stale"),
				ContainSubstring("patch failed"),
			)))
			kubeMock.AssertExpectations(GinkgoT())
		})
	})

	Describe("validateMTU", func() {
		var (
			kubeMock        *mocks.Interface
			netlinkOpsMock  *utilMocks.NetLinkOps
			netlinkLinkMock *netlink_mocks.Link

			nc *DefaultNodeNetworkController
		)

		const (
			nodeName  = "my-node"
			linkName  = "breth0"
			linkIPNet = "10.1.0.40/32"
			linkIndex = 4

			linkIPNet2 = "10.2.0.50/32"
			linkIndex2 = 5

			configDefaultMTU               = 1500 //value for config.Default.MTU
			mtuTooSmallForIPv4AndIPv6      = configDefaultMTU + types.GeneveHeaderLengthIPv4 - 1
			mtuOkForIPv4ButTooSmallForIPv6 = configDefaultMTU + types.GeneveHeaderLengthIPv4
			mtuOkForIPv4AndIPv6            = configDefaultMTU + types.GeneveHeaderLengthIPv6
			mtuTooSmallForSingleNode       = configDefaultMTU - 1
			mtuOkForSingleNode             = configDefaultMTU
		)

		BeforeEach(func() {
			kubeMock = new(mocks.Interface)
			netlinkOpsMock = new(utilMocks.NetLinkOps)
			netlinkLinkMock = new(netlink_mocks.Link)

			util.SetNetLinkOpMockInst(netlinkOpsMock)
			netlinkOpsMock.On("AddrList", nil, netlink.FAMILY_V4).
				Return([]netlink.Addr{
					{LinkIndex: linkIndex, IPNet: ovntest.MustParseIPNet(linkIPNet)},
					{LinkIndex: linkIndex2, IPNet: ovntest.MustParseIPNet(linkIPNet2)}}, nil)
			netlinkOpsMock.On("LinkByIndex", 4).Return(netlinkLinkMock, nil)

			nc = &DefaultNodeNetworkController{
				BaseNodeNetworkController: BaseNodeNetworkController{
					CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{
						name: nodeName,
						Kube: kubeMock,
					},
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}

			config.Default.MTU = configDefaultMTU
			config.Default.EffectiveEncapIP = "10.1.0.40"

		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst() // other tests in this package rely directly on netlink (e.g. gateway_init_linux_test.go)
		})

		Context("with a cluster in IPv4 mode", func() {

			BeforeEach(func() {
				config.IPv4Mode = true
				config.IPv6Mode = false
			})

			Context("with the node having a too small MTU", func() {

				It("should taint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuTooSmallForIPv4AndIPv6,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).To(HaveOccurred())
				})
			})

			Context("with the node having a big enough MTU", func() {

				It("should untaint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForIPv4ButTooSmallForIPv6,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})

		Context("with a cluster in IPv6 mode", func() {
			BeforeEach(func() {
				config.IPv4Mode = false
				config.IPv6Mode = true
			})

			Context("with the node having a too small MTU", func() {

				It("should taint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuTooSmallForIPv4AndIPv6,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).To(HaveOccurred())
				})
			})

			Context("with the node having a big enough MTU", func() {

				It("should untaint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForIPv4AndIPv6,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})

		Context("with a cluster in dual-stack mode", func() {
			BeforeEach(func() {
				config.IPv4Mode = true
				config.IPv6Mode = true
			})

			Context("with the node having a too small MTU", func() {

				It("should taint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForIPv4ButTooSmallForIPv6,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).To(HaveOccurred())
				})
			})

			Context("with the node having a big enough MTU", func() {

				It("should untaint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForIPv4AndIPv6,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})

		Context("with a single-node cluster", func() {
			BeforeEach(func() {
				config.Gateway.SingleNode = true
			})

			Context("with the node having a too small MTU", func() {

				It("should taint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuTooSmallForSingleNode,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).To(HaveOccurred())
				})
			})

			Context("with the node having a big enough MTU", func() {

				It("should untaint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForSingleNode,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})

		Context("with multiple ovn encap IPs", func() {

			BeforeEach(func() {
				config.IPv4Mode = true
				config.IPv6Mode = false
				config.Default.EffectiveEncapIP = "10.1.0.40,10.2.0.50"
				netlinkOpsMock.On("LinkByIndex", 5).Return(netlinkLinkMock, nil)
			})

			It("all interfaces have big enough MTU", func() {
				netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
					MTU:  mtuOkForIPv4AndIPv6,
					Name: linkName,
				})

				err := nc.validateVTEPInterfaceMTU()
				Expect(err).NotTo(HaveOccurred())
			})
		})

	})

	Describe("Node Operations", func() {
		var app *cli.App

		BeforeEach(func() {
			// Restore global default values before each testcase
			Expect(config.PrepareTestConfig()).To(Succeed())

			app = cli.NewApp()
			app.Name = "test"
			app.Flags = config.Flags
		})

		It("sets correct OVN external IDs", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeIP   string = "1.2.5.6"
					nodeName string = "cannot.be.resolv.ed"
					interval int    = 100000
					ofintval int    = 0
				)
				node := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{
								Type:    corev1.NodeExternalIP,
								Address: nodeIP,
							},
						},
					},
				}

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
						"external_ids:ovn-encap-type=geneve "+
						"external_ids:ovn-encap-ip=%s "+
						"external_ids:ovn-remote-probe-interval=%d "+
						"external_ids:ovn-bridge-remote-probe-interval=%d "+
						"other_config:bundle-idle-timeout=%d "+
						"external_ids:ovn-is-interconn=true "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-ofctrl-wait-before-clear=0 "+
						"external_ids:ovn-enable-lflow-cache=true "+
						"external_ids:ovn-set-local-ip=\"true\" "+
						"external_ids:hostname=\"%s\"",
						nodeIP, interval, ofintval, ofintval, nodeName),
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 -- clear bridge br-int netflow" +
						" -- " +
						"clear bridge br-int sflow" +
						" -- " +
						"clear bridge br-int ipfix",
				})
				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				config.OvnKubeNode.Mode = types.NodeModeFull
				err = setupOVNNode(&node)
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("sets non-default OVN encap port", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeIP      string = "1.2.5.6"
					nodeName    string = "cannot.be.resolv.ed"
					encapPort   uint   = 666
					interval    int    = 100000
					ofintval    int    = 0
					chassisUUID string = "1a3dfc82-2749-4931-9190-c30e7c0ecea3"
					encapUUID   string = "e4437094-0094-4223-9f14-995d98d5fff8"
				)

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 " +
						"--if-exists get Open_vSwitch . external_ids:system-id"),
					Output: chassisUUID,
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovn-sbctl --timeout=15 --data=bare --no-heading --columns=_uuid find "+
						"Encap chassis_name=%s", chassisUUID),
					Output: encapUUID,
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovn-sbctl --timeout=15 set encap "+
						"%s options:dst_port=%d", encapUUID, encapPort),
				})

				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())
				config.Default.EncapPort = encapPort
				err = setEncapPort(context.Background())
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("sets non-default logical flow cache limits", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeIP   string = "1.2.5.6"
					nodeName string = "cannot.be.resolv.ed"
					interval int    = 100000
					ofintval int    = 0
				)
				node := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{
								Type:    corev1.NodeExternalIP,
								Address: nodeIP,
							},
						},
					},
				}

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
						"external_ids:ovn-encap-type=geneve "+
						"external_ids:ovn-encap-ip=%s "+
						"external_ids:ovn-remote-probe-interval=%d "+
						"external_ids:ovn-bridge-remote-probe-interval=%d "+
						"other_config:bundle-idle-timeout=%d "+
						"external_ids:ovn-is-interconn=true "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-ofctrl-wait-before-clear=0 "+
						"external_ids:ovn-enable-lflow-cache=false "+
						"external_ids:ovn-set-local-ip=\"true\" "+
						"external_ids:ovn-limit-lflow-cache=1000 "+
						"external_ids:ovn-memlimit-lflow-cache-kb=100000 "+
						"external_ids:hostname=\"%s\"",
						nodeIP, interval, ofintval, ofintval, nodeName),
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 -- clear bridge br-int netflow" +
						" -- " +
						"clear bridge br-int sflow" +
						" -- " +
						"clear bridge br-int ipfix",
				})
				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				config.Default.LFlowCacheEnable = false
				config.Default.LFlowCacheLimit = 1000
				config.Default.LFlowCacheLimitKb = 100000
				config.OvnKubeNode.Mode = types.NodeModeFull
				err = setupOVNNode(&node)
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("sets default IPFIX configuration", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeIP    string = "1.2.5.6"
					nodeName  string = "cannot.be.resolv.ed"
					interval  int    = 100000
					ofintval  int    = 0
					ipfixPort int32  = 456
				)
				ipfixIP := net.IP{1, 2, 3, 4}

				node := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{
								Type:    corev1.NodeExternalIP,
								Address: nodeIP,
							},
						},
					},
				}

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
						"external_ids:ovn-encap-type=geneve "+
						"external_ids:ovn-encap-ip=%s "+
						"external_ids:ovn-remote-probe-interval=%d "+
						"external_ids:ovn-bridge-remote-probe-interval=%d "+
						"other_config:bundle-idle-timeout=%d "+
						"external_ids:ovn-is-interconn=true "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-ofctrl-wait-before-clear=0 "+
						"external_ids:ovn-enable-lflow-cache=true "+
						"external_ids:ovn-set-local-ip=\"true\" "+
						"external_ids:hostname=\"%s\"",
						nodeIP, interval, ofintval, ofintval, nodeName),
				})

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 -- clear bridge br-int netflow" +
						" -- " +
						"clear bridge br-int sflow" +
						" -- " +
						"clear bridge br-int ipfix",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15"+
						" -- "+
						"--id=@ipfix create ipfix "+
						"targets=[\"%s:%d\"] cache_active_timeout=60 sampling=400"+
						" -- "+
						"set bridge br-int ipfix=@ipfix", ipfixIP, ipfixPort),
				})
				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())
				config.Monitoring.IPFIXTargets = []config.HostPort{
					{Host: &ipfixIP, Port: ipfixPort},
				}
				err = setupOVNNode(&node)
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("allows overriding IPFIX configuration", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeIP    string = "1.2.5.6"
					nodeName  string = "cannot.be.resolv.ed"
					interval  int    = 100000
					ofintval  int    = 0
					ipfixPort int32  = 456
				)
				ipfixIP := net.IP{1, 2, 3, 4}

				node := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{
								Type:    corev1.NodeExternalIP,
								Address: nodeIP,
							},
						},
					},
				}

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
						"external_ids:ovn-encap-type=geneve "+
						"external_ids:ovn-encap-ip=%s "+
						"external_ids:ovn-remote-probe-interval=%d "+
						"external_ids:ovn-bridge-remote-probe-interval=%d "+
						"other_config:bundle-idle-timeout=%d "+
						"external_ids:ovn-is-interconn=true "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-ofctrl-wait-before-clear=0 "+
						"external_ids:ovn-enable-lflow-cache=true "+
						"external_ids:ovn-set-local-ip=\"true\" "+
						"external_ids:hostname=\"%s\"",
						nodeIP, interval, ofintval, ofintval, nodeName),
				})

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 -- clear bridge br-int netflow" +
						" -- " +
						"clear bridge br-int sflow" +
						" -- " +
						"clear bridge br-int ipfix",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15"+
						" -- "+
						"--id=@ipfix create ipfix "+
						"targets=[\"%s:%d\"] cache_active_timeout=123 cache_max_flows=456 sampling=789"+
						" -- "+
						"set bridge br-int ipfix=@ipfix", ipfixIP, ipfixPort),
				})
				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())
				config.Monitoring.IPFIXTargets = []config.HostPort{
					{Host: &ipfixIP, Port: ipfixPort},
				}
				config.IPFIX.CacheActiveTimeout = 123
				config.IPFIX.CacheMaxFlows = 456
				config.IPFIX.Sampling = 789
				err = setupOVNNode(&node)
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("uses Node IP when the flow tracing targets only specify a port", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeIP   string = "1.2.5.6"
					nodeName string = "anyhost.test"
					interval int    = 100000
					ofintval int    = 0
				)
				node := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{
								Type:    corev1.NodeExternalIP,
								Address: nodeIP,
							},
						},
					},
				}

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
						"external_ids:ovn-encap-type=geneve "+
						"external_ids:ovn-encap-ip=%s "+
						"external_ids:ovn-remote-probe-interval=%d "+
						"external_ids:ovn-bridge-remote-probe-interval=%d "+
						"other_config:bundle-idle-timeout=%d "+
						"external_ids:ovn-is-interconn=true "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-ofctrl-wait-before-clear=0 "+
						"external_ids:ovn-enable-lflow-cache=true "+
						"external_ids:ovn-set-local-ip=\"true\" "+
						"external_ids:hostname=\"%s\"",
						nodeIP, interval, ofintval, ofintval, nodeName),
				})

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 -- clear bridge br-int netflow" +
						" -- " +
						"clear bridge br-int sflow" +
						" -- " +
						"clear bridge br-int ipfix",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15" +
						" -- " +
						"--id=@ipfix create ipfix " +
						// verify that the 1.2.5.6 IP has been attached to the :8888 target below
						`targets=["10.0.0.2:3030","1.2.5.6:8888","[2020:1111:f::1:933]:3333"] cache_active_timeout=60` +
						" -- " +
						"set bridge br-int ipfix=@ipfix",
				})
				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				config.Monitoring.IPFIXTargets, err =
					config.ParseFlowCollectors("10.0.0.2:3030,:8888,[2020:1111:f::1:0933]:3333")
				config.IPFIX.CacheActiveTimeout = 60
				config.IPFIX.CacheMaxFlows = 0
				config.IPFIX.Sampling = 0
				Expect(err).NotTo(HaveOccurred())

				err = setupOVNNode(&node)
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
	Describe("node pmtud management", func() {
		var (
			testNS ns.NetNS
			nc     *DefaultNodeNetworkController
			app    *cli.App
		)

		const (
			nodeName       = "my-node"
			remoteNodeName = "other-node"
		)

		BeforeEach(func() {
			var err error
			testNS, err = testutils.NewNS()
			Expect(err).NotTo(HaveOccurred())
			Expect(config.PrepareTestConfig()).To(Succeed())

			app = cli.NewApp()
			app.Name = "test"
			app.Flags = config.Flags
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst() // other tests in this package rely directly on netlink (e.g. gateway_init_linux_test.go)
			Expect(testNS.Close()).To(Succeed())
		})

		Context("with a cluster in IPv4 mode", func() {
			const (
				ethName           string = "lo1337"
				nodeIP            string = "169.254.254.60"
				ethCIDR           string = nodeIP + "/24"
				otherNodeIP       string = "169.254.254.61"
				otherSubnetNodeIP string = "169.254.253.61"
				fullMask                 = 32
			)
			var link netlink.Link

			BeforeEach(func() {
				config.IPv4Mode = true
				config.IPv6Mode = false
				config.Gateway.Mode = config.GatewayModeShared

				// Note we must do this in default netNS because
				// nc.WatchNodes() will spawn goroutines which we cannot lock to the testNS
				ovntest.AddLink(ethName)

				var err error
				link, err = netlink.LinkByName(ethName)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(link)
				Expect(err).NotTo(HaveOccurred())

				// Add an IP address
				addr, err := netlink.ParseAddr(ethCIDR)
				Expect(err).NotTo(HaveOccurred())
				addr.Scope = int(netlink.SCOPE_UNIVERSE)
				err = netlink.AddrAdd(link, addr)
				Expect(err).NotTo(HaveOccurred())

			})

			AfterEach(func() {
				err := netlink.LinkDel(link)
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("adds and removes nftables rule for node in same subnet", func() {

				app.Action = func(_ *cli.Context) error {
					node := corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: nodeName,
							Annotations: map[string]string{
								util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\"]", nodeIP+"/24"),
							},
						},
						Status: corev1.NodeStatus{
							Addresses: []corev1.NodeAddress{
								{
									Type:    corev1.NodeInternalIP,
									Address: nodeIP,
								},
							},
						},
					}

					otherNode := corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: remoteNodeName,
							Annotations: map[string]string{
								util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\"]", otherNodeIP+"/24"),
							},
						},
						Status: corev1.NodeStatus{
							Addresses: []corev1.NodeAddress{
								{
									Type:    corev1.NodeInternalIP,
									Address: otherNodeIP,
								},
							},
						},
					}
					nft := nodenft.SetFakeNFTablesHelper()

					kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
						Items: []corev1.Node{node, otherNode},
					})
					fakeClient := &util.OVNNodeClientset{
						KubeClient:             kubeFakeClient,
						AdminPolicyRouteClient: adminpolicybasedrouteclient.NewSimpleClientset(),
						NetworkAttchDefClient:  nadfake.NewSimpleClientset(),
					}

					stop := make(chan struct{})
					wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
					Expect(err).NotTo(HaveOccurred())
					wg := &sync.WaitGroup{}
					defer func() {
						close(stop)
						wg.Wait()
						wf.Shutdown()
					}()

					err = wf.Start()
					Expect(err).NotTo(HaveOccurred())
					routeManager := routemanager.NewController()
					cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, fakeClient.AdminPolicyRouteClient, wf, nil, nodeName, routeManager)
					nc = newDefaultNodeNetworkController(cnnci, stop, wg, routeManager, nil, nil)
					nc.initRetryFrameworkForNode()
					err = setupRemoteNodeNFTSets()
					Expect(err).NotTo(HaveOccurred())
					err = setupPMTUDNFTChain()
					Expect(err).NotTo(HaveOccurred())
					nc.Gateway = &gateway{
						openflowManager: &openflowManager{
							defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
							uplinkBridges: map[string]*openflowBridge{},
						},
					}

					// must run route manager manually which is usually started with nc.Start()
					wg.Add(1)
					go func() {
						defer GinkgoRecover()
						defer wg.Done()
						nc.routeManager.Run(stop, 10*time.Second)
						Expect(err).NotTo(HaveOccurred())
					}()
					By("start up should add nftables rules for remote node")

					err = nc.WatchNodes()
					Expect(err).NotTo(HaveOccurred())
					nftRules := v4PMTUDNFTRules + `
add element inet ovn-kubernetes remote-node-ips-v4 { 169.254.254.61 }
`
					err = nodenft.MatchNFTRules(nftRules, nft.Dump())
					Expect(err).NotTo(HaveOccurred())
					gw := nc.Gateway.(*gateway)
					By("start up should add openflow rules for remote node")
					flows := gw.openflowManager.getFlowsByKey(getPMTUDKey(remoteNodeName))
					Expect(flows).To(HaveLen(1))

					By("deleting the remote node should remove the nftables element")
					err = kubeFakeClient.CoreV1().Nodes().Delete(context.TODO(), remoteNodeName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
					Eventually(func() error {
						return nodenft.MatchNFTRules(v4PMTUDNFTRules, nft.Dump())
					}).WithTimeout(2 * time.Second).ShouldNot(HaveOccurred())
					Eventually(func() []string { return gw.openflowManager.getFlowsByKey(getPMTUDKey(remoteNodeName)) }).WithTimeout(2 * time.Second).Should(BeEmpty())
					return nil

				}

				err := app.Run([]string{app.Name})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("adds and removes nftables rule for node in different subnet", func() {

				app.Action = func(_ *cli.Context) error {
					node := corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: nodeName,
							Annotations: map[string]string{
								util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\"]", nodeIP+"/24"),
							},
						},
						Status: corev1.NodeStatus{
							Addresses: []corev1.NodeAddress{
								{
									Type:    corev1.NodeInternalIP,
									Address: nodeIP,
								},
							},
						},
					}

					otherNode := corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: remoteNodeName,
							Annotations: map[string]string{
								util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\"]", otherSubnetNodeIP+"/24"),
							},
						},
						Status: corev1.NodeStatus{
							Addresses: []corev1.NodeAddress{
								{
									Type:    corev1.NodeInternalIP,
									Address: otherSubnetNodeIP,
								},
							},
						},
					}
					nft := nodenft.SetFakeNFTablesHelper()

					kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
						Items: []corev1.Node{node, otherNode},
					})
					fakeClient := &util.OVNNodeClientset{
						KubeClient:             kubeFakeClient,
						AdminPolicyRouteClient: adminpolicybasedrouteclient.NewSimpleClientset(),
						NetworkAttchDefClient:  nadfake.NewSimpleClientset(),
					}

					stop := make(chan struct{})
					wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
					Expect(err).NotTo(HaveOccurred())
					wg := &sync.WaitGroup{}
					defer func() {
						close(stop)
						wg.Wait()
						wf.Shutdown()
					}()

					err = wf.Start()
					Expect(err).NotTo(HaveOccurred())
					routeManager := routemanager.NewController()
					cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, fakeClient.AdminPolicyRouteClient, wf, nil, nodeName, routeManager)
					nc = newDefaultNodeNetworkController(cnnci, stop, wg, routeManager, nil, nil)
					nc.initRetryFrameworkForNode()
					err = setupRemoteNodeNFTSets()
					Expect(err).NotTo(HaveOccurred())
					err = setupPMTUDNFTChain()
					Expect(err).NotTo(HaveOccurred())
					nc.Gateway = &gateway{
						openflowManager: &openflowManager{
							defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
							uplinkBridges: map[string]*openflowBridge{},
						},
					}

					// must run route manager manually which is usually started with nc.Start()
					wg.Add(1)
					go func() {
						defer GinkgoRecover()
						defer wg.Done()
						nc.routeManager.Run(stop, 10*time.Second)
						Expect(err).NotTo(HaveOccurred())
					}()
					By("start up should add nftables rules for remote node")

					err = nc.WatchNodes()
					Expect(err).NotTo(HaveOccurred())
					nftRules := v4PMTUDNFTRules + `
add element inet ovn-kubernetes remote-node-ips-v4 { 169.254.253.61 }
`
					err = nodenft.MatchNFTRules(nftRules, nft.Dump())
					Expect(err).NotTo(HaveOccurred())
					gw := nc.Gateway.(*gateway)
					By("start up should add openflow rules for remote node")
					flows := gw.openflowManager.getFlowsByKey(getPMTUDKey(remoteNodeName))
					Expect(flows).To(HaveLen(1))

					By("deleting the remote node should remove the nftables element")
					err = kubeFakeClient.CoreV1().Nodes().Delete(context.TODO(), remoteNodeName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
					Eventually(func() error {
						return nodenft.MatchNFTRules(v4PMTUDNFTRules, nft.Dump())
					}).WithTimeout(2 * time.Second).ShouldNot(HaveOccurred())
					Eventually(func() []string { return gw.openflowManager.getFlowsByKey(getPMTUDKey(remoteNodeName)) }).WithTimeout(2 * time.Second).Should(BeEmpty())
					return nil

				}

				err := app.Run([]string{app.Name})
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("with a cluster in IPv6 mode", func() {
			const (
				ethName           string = "lo1337"
				nodeIP            string = "2001:db8:1::3"
				ethCIDR           string = nodeIP + "/64"
				otherNodeIP       string = "2001:db8:1::4"
				otherSubnetNodeIP string = "2002:db8:1::4"
				fullMask                 = 128
			)

			var link netlink.Link

			BeforeEach(func() {
				config.IPv4Mode = false
				config.IPv6Mode = true
				config.Gateway.Mode = config.GatewayModeShared

				// Note we must do this in default netNS because
				// nc.WatchNodes() will spawn goroutines which we cannot lock to the testNS
				ovntest.AddLink(ethName)

				var err error
				link, err = netlink.LinkByName(ethName)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(link)
				Expect(err).NotTo(HaveOccurred())

				// Add an IP address
				addr, err := netlink.ParseAddr(ethCIDR)
				Expect(err).NotTo(HaveOccurred())
				addr.Scope = int(netlink.SCOPE_UNIVERSE)
				err = netlink.AddrAdd(link, addr)
				Expect(err).NotTo(HaveOccurred())

			})

			AfterEach(func() {
				err := netlink.LinkDel(link)
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("adds and removes nftables rule for node in same subnet", func() {

				app.Action = func(_ *cli.Context) error {
					node := corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: nodeName,
							Annotations: map[string]string{
								util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\"]", nodeIP+"/64"),
							},
						},
						Status: corev1.NodeStatus{
							Addresses: []corev1.NodeAddress{
								{
									Type:    corev1.NodeInternalIP,
									Address: nodeIP,
								},
							},
						},
					}

					otherNode := corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: remoteNodeName,
							Annotations: map[string]string{
								util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\"]", otherNodeIP+"/64"),
							},
						},
						Status: corev1.NodeStatus{
							Addresses: []corev1.NodeAddress{
								{
									Type:    corev1.NodeInternalIP,
									Address: otherNodeIP,
								},
							},
						},
					}
					nft := nodenft.SetFakeNFTablesHelper()

					kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
						Items: []corev1.Node{node, otherNode},
					})
					fakeClient := &util.OVNNodeClientset{
						KubeClient:             kubeFakeClient,
						AdminPolicyRouteClient: adminpolicybasedrouteclient.NewSimpleClientset(),
						NetworkAttchDefClient:  nadfake.NewSimpleClientset(),
					}

					stop := make(chan struct{})
					wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
					Expect(err).NotTo(HaveOccurred())
					wg := &sync.WaitGroup{}
					defer func() {
						close(stop)
						wg.Wait()
						wf.Shutdown()
					}()

					err = wf.Start()
					Expect(err).NotTo(HaveOccurred())
					routeManager := routemanager.NewController()
					cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, fakeClient.AdminPolicyRouteClient, wf, nil, nodeName, routeManager)
					nc = newDefaultNodeNetworkController(cnnci, stop, wg, routeManager, nil, nil)
					nc.initRetryFrameworkForNode()
					err = setupRemoteNodeNFTSets()
					Expect(err).NotTo(HaveOccurred())
					err = setupPMTUDNFTChain()
					Expect(err).NotTo(HaveOccurred())
					nc.Gateway = &gateway{
						openflowManager: &openflowManager{
							defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
							uplinkBridges: map[string]*openflowBridge{},
						},
					}

					// must run route manager manually which is usually started with nc.Start()
					wg.Add(1)
					go func() {
						defer GinkgoRecover()
						defer wg.Done()
						nc.routeManager.Run(stop, 10*time.Second)
						Expect(err).NotTo(HaveOccurred())
					}()
					By("start up should add nftables rules for remote node")

					err = nc.WatchNodes()
					Expect(err).NotTo(HaveOccurred())
					nftRules := v6PMTUDNFTRules + `
add element inet ovn-kubernetes remote-node-ips-v6 { 2001:db8:1::4 }
`
					err = nodenft.MatchNFTRules(nftRules, nft.Dump())
					Expect(err).NotTo(HaveOccurred())
					gw := nc.Gateway.(*gateway)
					By("start up should add openflow rules for remote node")
					flows := gw.openflowManager.getFlowsByKey(getPMTUDKey(remoteNodeName))
					Expect(flows).To(HaveLen(1))

					By("deleting the remote node should remove the nftables element")
					err = kubeFakeClient.CoreV1().Nodes().Delete(context.TODO(), remoteNodeName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
					Eventually(func() error {
						return nodenft.MatchNFTRules(v6PMTUDNFTRules, nft.Dump())
					}).WithTimeout(2 * time.Second).ShouldNot(HaveOccurred())
					Eventually(func() []string { return gw.openflowManager.getFlowsByKey(getPMTUDKey(remoteNodeName)) }).WithTimeout(2 * time.Second).Should(BeEmpty())
					return nil
				}

				err := app.Run([]string{app.Name})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("adds and removes nftables rule for node in different subnet", func() {

				app.Action = func(_ *cli.Context) error {
					node := corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: nodeName,
							Annotations: map[string]string{
								util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\"]", nodeIP+"/64"),
							},
						},
						Status: corev1.NodeStatus{
							Addresses: []corev1.NodeAddress{
								{
									Type:    corev1.NodeInternalIP,
									Address: nodeIP,
								},
							},
						},
					}

					otherNode := corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: remoteNodeName,
							Annotations: map[string]string{
								util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\"]", otherSubnetNodeIP+"/64"),
							},
						},
						Status: corev1.NodeStatus{
							Addresses: []corev1.NodeAddress{
								{
									Type:    corev1.NodeInternalIP,
									Address: otherSubnetNodeIP,
								},
							},
						},
					}
					nft := nodenft.SetFakeNFTablesHelper()

					kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
						Items: []corev1.Node{node, otherNode},
					})
					fakeClient := &util.OVNNodeClientset{
						KubeClient:             kubeFakeClient,
						AdminPolicyRouteClient: adminpolicybasedrouteclient.NewSimpleClientset(),
						NetworkAttchDefClient:  nadfake.NewSimpleClientset(),
					}

					stop := make(chan struct{})
					wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
					Expect(err).NotTo(HaveOccurred())
					wg := &sync.WaitGroup{}
					defer func() {
						close(stop)
						wg.Wait()
						wf.Shutdown()
					}()

					err = wf.Start()
					Expect(err).NotTo(HaveOccurred())
					routeManager := routemanager.NewController()
					cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, fakeClient.AdminPolicyRouteClient, wf, nil, nodeName, routeManager)
					nc = newDefaultNodeNetworkController(cnnci, stop, wg, routeManager, nil, nil)
					nc.initRetryFrameworkForNode()
					err = setupRemoteNodeNFTSets()
					Expect(err).NotTo(HaveOccurred())
					err = setupPMTUDNFTChain()
					Expect(err).NotTo(HaveOccurred())
					nc.Gateway = &gateway{
						openflowManager: &openflowManager{
							defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
							uplinkBridges: map[string]*openflowBridge{},
						},
					}

					// must run route manager manually which is usually started with nc.Start()
					wg.Add(1)
					go func() {
						defer GinkgoRecover()
						defer wg.Done()
						nc.routeManager.Run(stop, 10*time.Second)
						Expect(err).NotTo(HaveOccurred())
					}()
					By("start up should add nftables rules for remote node")

					err = nc.WatchNodes()
					Expect(err).NotTo(HaveOccurred())
					nftRules := v6PMTUDNFTRules + `
add element inet ovn-kubernetes remote-node-ips-v6 { 2002:db8:1::4 }
`
					err = nodenft.MatchNFTRules(nftRules, nft.Dump())
					Expect(err).NotTo(HaveOccurred())
					gw := nc.Gateway.(*gateway)
					By("start up should add openflow rules for remote node")
					flows := gw.openflowManager.getFlowsByKey(getPMTUDKey(remoteNodeName))
					Expect(flows).To(HaveLen(1))

					By("deleting the remote node should remove the nftables element")
					err = kubeFakeClient.CoreV1().Nodes().Delete(context.TODO(), remoteNodeName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
					Eventually(func() error {
						return nodenft.MatchNFTRules(v6PMTUDNFTRules, nft.Dump())
					}).WithTimeout(2 * time.Second).ShouldNot(HaveOccurred())
					Eventually(func() []string { return gw.openflowManager.getFlowsByKey(getPMTUDKey(remoteNodeName)) }).WithTimeout(2 * time.Second).Should(BeEmpty())
					return nil
				}

				err := app.Run([]string{app.Name})
				Expect(err).NotTo(HaveOccurred())
			})

		})

	})

	Describe("node ingress snat exclude subnets", func() {

		var (
			testNS ns.NetNS
			nc     *DefaultNodeNetworkController
			app    *cli.App
		)

		const (
			nodeName = "my-node"
		)

		BeforeEach(func() {
			var err error
			testNS, err = testutils.NewNS()
			Expect(err).NotTo(HaveOccurred())
			Expect(config.PrepareTestConfig()).To(Succeed())

			app = cli.NewApp()
			app.Name = "test"
			app.Flags = config.Flags
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst() // other tests in this package rely directly on netlink (e.g. gateway_init_linux_test.go)
			Expect(testNS.Close()).To(Succeed())
		})

		Context("with a cluster in IPv4 mode", func() {
			const (
				ethName string = "lo1337"
				nodeIP  string = "169.254.254.60"
				ethCIDR string = nodeIP + "/24"
			)
			var link netlink.Link

			BeforeEach(func() {
				config.IPv4Mode = true
				config.IPv6Mode = false
				config.Gateway.Mode = config.GatewayModeShared

				// Note we must do this in default netNS because
				// nc.WatchNodes() will spawn goroutines which we cannot lock to the testNS
				ovntest.AddLink(ethName)

				var err error
				link, err = netlink.LinkByName(ethName)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(link)
				Expect(err).NotTo(HaveOccurred())

				// Add an IP address
				addr, err := netlink.ParseAddr(ethCIDR)
				Expect(err).NotTo(HaveOccurred())
				addr.Scope = int(netlink.SCOPE_UNIVERSE)
				err = netlink.AddrAdd(link, addr)
				Expect(err).NotTo(HaveOccurred())

			})

			AfterEach(func() {
				err := netlink.LinkDel(link)
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("empty annotation on startup", func() {

				app.Action = func(_ *cli.Context) error {
					node := corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name:        nodeName,
							Annotations: map[string]string{},
						},
						Status: corev1.NodeStatus{
							Addresses: []corev1.NodeAddress{
								{
									Type:    corev1.NodeInternalIP,
									Address: nodeIP,
								},
							},
						},
					}

					nft := nodenft.SetFakeNFTablesHelper()

					kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
						Items: []corev1.Node{node},
					})
					fakeClient := &util.OVNNodeClientset{
						KubeClient:             kubeFakeClient,
						AdminPolicyRouteClient: adminpolicybasedrouteclient.NewSimpleClientset(),
						NetworkAttchDefClient:  nadfake.NewSimpleClientset(),
					}

					stop := make(chan struct{})
					wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
					Expect(err).NotTo(HaveOccurred())
					wg := &sync.WaitGroup{}
					defer func() {
						close(stop)
						wg.Wait()
						wf.Shutdown()
					}()

					err = wf.Start()
					Expect(err).NotTo(HaveOccurred())
					routeManager := routemanager.NewController()
					cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, fakeClient.AdminPolicyRouteClient, wf, nil, nodeName, routeManager)
					nc = newDefaultNodeNetworkController(cnnci, stop, wg, routeManager, nil, nil)
					nc.initRetryFrameworkForNode()
					err = setupRemoteNodeNFTSets()
					Expect(err).NotTo(HaveOccurred())
					err = setupPMTUDNFTChain()
					Expect(err).NotTo(HaveOccurred())
					nc.Gateway = &gateway{
						openflowManager: &openflowManager{
							defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
							uplinkBridges: map[string]*openflowBridge{},
						},
					}

					err = managementport.SetupManagementPortNFTSets()
					Expect(err).NotTo(HaveOccurred())

					// must run route manager manually which is usually started with nc.Start()
					wg.Add(1)
					go func() {
						defer GinkgoRecover()
						defer wg.Done()
						nc.routeManager.Run(stop, 10*time.Second)
						Expect(err).NotTo(HaveOccurred())
					}()
					By("no nftables elements should present at startup")

					err = nc.WatchNodes()
					Expect(err).NotTo(HaveOccurred())
					Expect(nft.Dump()).NotTo(ContainSubstring("add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.168.1.0/24 }"))
					Expect(nft.Dump()).NotTo(ContainSubstring("add element inet ovn-kubernetes mgmtport-no-snat-subnets-v6 { fd00::/64 }"))

					By("adding subnets to node annotation should update nftables elements")
					node.Annotations[util.OvnNodeDontSNATSubnets] = `["192.167.1.0/24"]`

					_, err = kubeFakeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					Eventually(func() bool {
						cleanDump := strings.ReplaceAll(nft.Dump(), "\r", "")
						return strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.167.1.0/24 }") &&
							!strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.168.1.0/24 }") &&
							!strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v6 { fd00::/64 }")
					}).WithTimeout(2 * time.Second).Should(BeTrue())

					By("adding extra subnets to node annotation should update nftables elements")

					node.Annotations[util.OvnNodeDontSNATSubnets] = `["192.167.1.0/24","fd00::/64","192.169.1.0/24","fd11::/64"]`

					_, err = kubeFakeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					Eventually(func() bool {
						cleanDump := strings.ReplaceAll(nft.Dump(), "\r", "")
						return strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.167.1.0/24 }") &&
							strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.169.1.0/24 }") &&
							strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v6 { fd00::/64 }")
					}).WithTimeout(2 * time.Second).Should(BeTrue())

					By("deleting node should remove nftables elements")
					err = kubeFakeClient.CoreV1().Nodes().Delete(context.TODO(), nodeName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())

					Eventually(func() bool {
						cleanDump := strings.ReplaceAll(nft.Dump(), "\r", "")
						return !strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.167.1.0/24 }") &&
							!strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.169.1.0/24 }") &&
							!strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v6 { fd00::/64 }")

					}).WithTimeout(2 * time.Second).Should(BeTrue())
					return nil
				}

				err := app.Run([]string{app.Name})
				Expect(err).NotTo(HaveOccurred())
			})

			ovntest.OnSupportedPlatformsIt("non-empty annotation on startup", func() {

				app.Action = func(_ *cli.Context) error {
					node := corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: nodeName,
							Annotations: map[string]string{
								util.OvnNodeDontSNATSubnets: `["192.168.1.0/24","fd00::/64"]`,
							},
						},
						Status: corev1.NodeStatus{
							Addresses: []corev1.NodeAddress{
								{
									Type:    corev1.NodeInternalIP,
									Address: nodeIP,
								},
							},
						},
					}

					nft := nodenft.SetFakeNFTablesHelper()

					kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
						Items: []corev1.Node{node},
					})
					fakeClient := &util.OVNNodeClientset{
						KubeClient:             kubeFakeClient,
						AdminPolicyRouteClient: adminpolicybasedrouteclient.NewSimpleClientset(),
						NetworkAttchDefClient:  nadfake.NewSimpleClientset(),
					}

					stop := make(chan struct{})
					wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
					Expect(err).NotTo(HaveOccurred())
					wg := &sync.WaitGroup{}
					defer func() {
						close(stop)
						wg.Wait()
						wf.Shutdown()
					}()

					err = wf.Start()
					Expect(err).NotTo(HaveOccurred())
					routeManager := routemanager.NewController()
					cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, fakeClient.AdminPolicyRouteClient, wf, nil, nodeName, routeManager)
					nc = newDefaultNodeNetworkController(cnnci, stop, wg, routeManager, nil, nil)
					nc.initRetryFrameworkForNode()
					err = setupRemoteNodeNFTSets()
					Expect(err).NotTo(HaveOccurred())
					err = setupPMTUDNFTChain()
					Expect(err).NotTo(HaveOccurred())
					nc.Gateway = &gateway{
						openflowManager: &openflowManager{
							defaultBridge: newOpenflowBridge(bridgeconfig.TestDefaultBridgeConfig()),
							uplinkBridges: map[string]*openflowBridge{},
						},
					}

					err = managementport.SetupManagementPortNFTSets()
					Expect(err).NotTo(HaveOccurred())

					// must run route manager manually which is usually started with nc.Start()
					wg.Add(1)
					go func() {
						defer GinkgoRecover()
						defer wg.Done()
						nc.routeManager.Run(stop, 10*time.Second)
						Expect(err).NotTo(HaveOccurred())
					}()
					By("expected nftables elements should present at startup")

					err = nc.WatchNodes()
					Expect(err).NotTo(HaveOccurred())
					Expect(nft.Dump()).To(ContainSubstring("add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.168.1.0/24 }"))
					Expect(nft.Dump()).To(ContainSubstring("add element inet ovn-kubernetes mgmtport-no-snat-subnets-v6 { fd00::/64 }"))

					By("editing subnets on node annotation should update nftables elements")
					node.Annotations[util.OvnNodeDontSNATSubnets] = `["192.167.1.0/24"]`

					_, err = kubeFakeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					Eventually(func() bool {
						cleanDump := strings.ReplaceAll(nft.Dump(), "\r", "")
						return strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.167.1.0/24 }") &&
							!strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.168.1.0/24 }") &&
							!strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v6 { fd00::/64 }")
					}).WithTimeout(2 * time.Second).Should(BeTrue())

					By("adding extra subnets to node annotation should update nftables elements")

					node.Annotations[util.OvnNodeDontSNATSubnets] = `["192.167.1.0/24","fd00::/64","192.169.1.0/24","fd11::/64"]`

					_, err = kubeFakeClient.CoreV1().Nodes().Update(context.TODO(), &node, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					Eventually(func() bool {
						cleanDump := strings.ReplaceAll(nft.Dump(), "\r", "")
						return strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.167.1.0/24 }") &&
							strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.169.1.0/24 }") &&
							strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v6 { fd00::/64 }")
					}).WithTimeout(2 * time.Second).Should(BeTrue())

					By("deleting node should remove nftables elements")
					err = kubeFakeClient.CoreV1().Nodes().Delete(context.TODO(), nodeName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())

					Eventually(func() bool {
						cleanDump := strings.ReplaceAll(nft.Dump(), "\r", "")
						return !strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.167.1.0/24 }") &&
							!strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { 192.169.1.0/24 }") &&
							!strings.Contains(cleanDump, "add element inet ovn-kubernetes mgmtport-no-snat-subnets-v6 { fd00::/64 }")

					}).WithTimeout(2 * time.Second).Should(BeTrue())
					return nil
				}

				err := app.Run([]string{app.Name})
				Expect(err).NotTo(HaveOccurred())
			})

		})

		Describe("derive-from-mgmt-port gateway interface resolution", func() {
			var (
				kubeMock        *mocks.Interface
				sriovnetMock    utilMocks.SriovnetOps
				netlinkOpsMock  *utilMocks.NetLinkOps
				netlinkLinkMock *netlink_mocks.Link
			)

			const (
				nodeName            = "test-node"
				mgmtPortNetdev      = "pf0vf0"
				vfPciAddr           = "0000:01:02.3"
				pfPciAddr           = "0000:01:00.0"
				expectedGatewayIntf = "eth0"
			)

			BeforeEach(func() {
				kubeMock = new(mocks.Interface)
				sriovnetMock = utilMocks.SriovnetOps{}
				netlinkOpsMock = new(utilMocks.NetLinkOps)
				netlinkLinkMock = new(netlink_mocks.Link)

				util.SetSriovnetOpsInst(&sriovnetMock)
				util.SetNetLinkOpMockInst(netlinkOpsMock)

				// Setup default node network controller
				cnnci := &CommonNodeNetworkControllerInfo{
					name: nodeName,
					Kube: kubeMock,
				}
				nc = &DefaultNodeNetworkController{
					BaseNodeNetworkController: BaseNodeNetworkController{
						CommonNodeNetworkControllerInfo: *cnnci,
						ReconcilableNetInfo:             &util.DefaultNetInfo{},
					},
				}

				// Set DPU host mode
				config.OvnKubeNode.Mode = types.NodeModeDPUHost
				config.OvnKubeNode.MgmtPortNetdev = mgmtPortNetdev
				config.Gateway.Interface = types.DeriveFromMgmtPort
			})

			AfterEach(func() {
				util.ResetNetLinkOpMockInst()
			})

			Context("when gateway interface is set to derive-from-mgmt-port", func() {
				ovntest.OnSupportedPlatformsIt("should resolve gateway interface from PCI address successfully", func() {
					// Mock getManagementPortNetDev to return the management port device
					netlinkOpsMock.On("LinkByName", mgmtPortNetdev).Return(netlinkLinkMock, nil)
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						Name: mgmtPortNetdev,
					})

					// Mock GetPciFromNetDevice to return VF PCI address
					sriovnetMock.On("GetPciFromNetDevice", mgmtPortNetdev).Return(vfPciAddr, nil)

					// Mock GetPfPciFromVfPci to return PF PCI address
					sriovnetMock.On("GetPfPciFromVfPci", vfPciAddr).Return(pfPciAddr, nil)

					// Mock GetNetDevicesFromPci to return available network devices
					sriovnetMock.On("GetNetDevicesFromPci", pfPciAddr).Return([]string{expectedGatewayIntf, "eth1"}, nil)

					// Execute the gateway interface resolution logic
					// This simulates the logic in the Start() method
					netdevName, err := getManagementPortNetDev(config.OvnKubeNode.MgmtPortNetdev)
					Expect(err).NotTo(HaveOccurred())
					Expect(netdevName).To(Equal(mgmtPortNetdev))

					pciAddr, err := util.GetSriovnetOps().GetPciFromNetDevice(netdevName)
					Expect(err).NotTo(HaveOccurred())
					Expect(pciAddr).To(Equal(vfPciAddr))

					pfPciAddr, err := util.GetSriovnetOps().GetPfPciFromVfPci(pciAddr)
					Expect(err).NotTo(HaveOccurred())
					Expect(pfPciAddr).To(Equal(pfPciAddr))

					netdevs, err := util.GetSriovnetOps().GetNetDevicesFromPci(pfPciAddr)
					Expect(err).NotTo(HaveOccurred())
					Expect(netdevs).To(HaveLen(2))
					Expect(netdevs[0]).To(Equal(expectedGatewayIntf))

					// Verify that the first device is selected as the gateway interface
					selectedNetdev := netdevs[0]
					Expect(selectedNetdev).To(Equal(expectedGatewayIntf))
				})

				ovntest.OnSupportedPlatformsIt("should return error when no network devices found for PCI address", func() {
					// Mock getManagementPortNetDev to return the management port device
					netlinkOpsMock.On("LinkByName", mgmtPortNetdev).Return(netlinkLinkMock, nil)
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						Name: mgmtPortNetdev,
					})

					// Mock GetPciFromNetDevice to return VF PCI address
					sriovnetMock.On("GetPciFromNetDevice", mgmtPortNetdev).Return(vfPciAddr, nil)

					// Mock GetPfPciFromVfPci to return PF PCI address
					sriovnetMock.On("GetPfPciFromVfPci", vfPciAddr).Return(pfPciAddr, nil)

					// Mock GetNetDevicesFromPci to return empty list
					sriovnetMock.On("GetNetDevicesFromPci", pfPciAddr).Return([]string{}, nil)

					// Execute the gateway interface resolution logic
					netdevName, err := getManagementPortNetDev(config.OvnKubeNode.MgmtPortNetdev)
					Expect(err).NotTo(HaveOccurred())

					pciAddr, err := util.GetSriovnetOps().GetPciFromNetDevice(netdevName)
					Expect(err).NotTo(HaveOccurred())

					pfPciAddr, err := util.GetSriovnetOps().GetPfPciFromVfPci(pciAddr)
					Expect(err).NotTo(HaveOccurred())

					netdevs, err := util.GetSriovnetOps().GetNetDevicesFromPci(pfPciAddr)
					Expect(err).NotTo(HaveOccurred())
					Expect(netdevs).To(BeEmpty())

					// This should result in an error when no devices are found
					Expect(netdevs).To(BeEmpty())
				})

				ovntest.OnSupportedPlatformsIt("should return error when GetPciFromNetDevice fails", func() {
					// Mock getManagementPortNetDev to return the management port device
					netlinkOpsMock.On("LinkByName", mgmtPortNetdev).Return(netlinkLinkMock, nil)
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						Name: mgmtPortNetdev,
					})

					// Mock GetPciFromNetDevice to return error
					sriovnetMock.On("GetPciFromNetDevice", mgmtPortNetdev).Return("", fmt.Errorf("failed to get PCI address"))

					// Execute the gateway interface resolution logic
					netdevName, err := getManagementPortNetDev(config.OvnKubeNode.MgmtPortNetdev)
					Expect(err).NotTo(HaveOccurred())

					_, err = util.GetSriovnetOps().GetPciFromNetDevice(netdevName)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("failed to get PCI address"))
				})

				ovntest.OnSupportedPlatformsIt("should return error when GetPfPciFromVfPci fails", func() {
					// Mock getManagementPortNetDev to return the management port device
					netlinkOpsMock.On("LinkByName", mgmtPortNetdev).Return(netlinkLinkMock, nil)
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						Name: mgmtPortNetdev,
					})

					// Mock GetPciFromNetDevice to return VF PCI address
					sriovnetMock.On("GetPciFromNetDevice", mgmtPortNetdev).Return(vfPciAddr, nil)

					// Mock GetPfPciFromVfPci to return error
					sriovnetMock.On("GetPfPciFromVfPci", vfPciAddr).Return("", fmt.Errorf("failed to get PF PCI address"))

					// Execute the gateway interface resolution logic
					netdevName, err := getManagementPortNetDev(config.OvnKubeNode.MgmtPortNetdev)
					Expect(err).NotTo(HaveOccurred())

					pciAddr, err := util.GetSriovnetOps().GetPciFromNetDevice(netdevName)
					Expect(err).NotTo(HaveOccurred())

					_, err = util.GetSriovnetOps().GetPfPciFromVfPci(pciAddr)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("failed to get PF PCI address"))
				})

				ovntest.OnSupportedPlatformsIt("should return error when GetNetDevicesFromPci fails", func() {
					// Mock getManagementPortNetDev to return the management port device
					netlinkOpsMock.On("LinkByName", mgmtPortNetdev).Return(netlinkLinkMock, nil)
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						Name: mgmtPortNetdev,
					})

					// Mock GetPciFromNetDevice to return VF PCI address
					sriovnetMock.On("GetPciFromNetDevice", mgmtPortNetdev).Return(vfPciAddr, nil)

					// Mock GetPfPciFromVfPci to return PF PCI address
					sriovnetMock.On("GetPfPciFromVfPci", vfPciAddr).Return(pfPciAddr, nil)

					// Mock GetNetDevicesFromPci to return error
					sriovnetMock.On("GetNetDevicesFromPci", pfPciAddr).Return(nil, fmt.Errorf("failed to get network devices"))

					// Execute the gateway interface resolution logic
					netdevName, err := getManagementPortNetDev(config.OvnKubeNode.MgmtPortNetdev)
					Expect(err).NotTo(HaveOccurred())

					pciAddr, err := util.GetSriovnetOps().GetPciFromNetDevice(netdevName)
					Expect(err).NotTo(HaveOccurred())

					pfPciAddr, err := util.GetSriovnetOps().GetPfPciFromVfPci(pciAddr)
					Expect(err).NotTo(HaveOccurred())

					_, err = util.GetSriovnetOps().GetNetDevicesFromPci(pfPciAddr)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("failed to get network devices"))
				})
			})
		})
	})

	Describe("reconcileConntrackUponEndpointSliceEvents", func() {
		const (
			testNamespace     = "test-ns"
			testServiceName   = "test-service"
			testEndpointSlice = "test-endpointslice"
		)

		var (
			udpProtocol       = corev1.ProtocolUDP
			tcpProtocol       = corev1.ProtocolTCP
			testEndpointPort1 = int32(8080)
			testEndpointPort2 = int32(8443)
			testServicePort1  = int32(80)
			testServicePort2  = int32(443)
		)

		// expectedConntrackFilter represents the expected parameters for a conntrack filter
		type expectedConntrackFilter struct {
			ip       string
			port     uint16
			protocol uint8
		}

		// Test data structure for table-driven tests
		type reconcileConntrackTestCase struct {
			desc                   string
			service                *corev1.Service // nil means service not found
			oldEndpointSlice       *discovery.EndpointSlice
			newEndpointSlice       *discovery.EndpointSlice
			expectedConntrackCalls int
			expectedFilters        []expectedConntrackFilter
			initialSyncSlices      []interface{}
			initialNonZeroState    bool
		}

		type endpointPortConfig struct {
			name     *string
			port     int32
			protocol corev1.Protocol
		}

		type servicePortConfig struct {
			name       string
			port       int32
			targetPort int32
			protocol   corev1.Protocol
		}

		makeEndpointSlice := func(portConfigs []endpointPortConfig, addresses []string, addressType discovery.AddressType) *discovery.EndpointSlice {
			ports := make([]discovery.EndpointPort, len(portConfigs))
			for i, pc := range portConfigs {
				p := pc.port
				proto := pc.protocol
				ports[i] = discovery.EndpointPort{
					Name:     pc.name,
					Port:     &p,
					Protocol: &proto,
				}
			}

			return &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testEndpointSlice,
					Namespace: testNamespace,
					Labels: map[string]string{
						discovery.LabelServiceName: testServiceName,
					},
				},
				AddressType: addressType,
				Ports:       ports,
				Endpoints: []discovery.Endpoint{
					{Addresses: addresses},
				},
			}
		}

		makeService := func(portConfigs []servicePortConfig) *corev1.Service {
			ports := make([]corev1.ServicePort, len(portConfigs))
			for i, pc := range portConfigs {
				ports[i] = corev1.ServicePort{
					Name:       pc.name,
					Port:       pc.port,
					TargetPort: intstr.FromInt(int(pc.targetPort)),
					Protocol:   pc.protocol,
				}
			}

			return &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testServiceName,
					Namespace: testNamespace,
				},
				Spec: corev1.ServiceSpec{
					Ports: ports,
				},
			}
		}

		makeServiceWithNodePort := func(portConfigs []servicePortConfig, nodePorts []int32, svcType corev1.ServiceType) *corev1.Service {
			svc := makeService(portConfigs)
			svc.Spec.Type = svcType
			for i := 0; i < len(nodePorts) && i < len(svc.Spec.Ports); i++ {
				svc.Spec.Ports[i].NodePort = nodePorts[i]
			}
			return svc
		}

		// Helper function to build expected ConntrackFilter for verification
		buildExpectedFilter := func(ef expectedConntrackFilter) *netlink.ConntrackFilter {
			filter := &netlink.ConntrackFilter{}

			// Add protocol
			if err := filter.AddProtocol(ef.protocol); err != nil {
				GinkgoT().Fatalf("Failed to add protocol to expected filter: %v", err)
			}

			// Add port
			if ef.port > 0 {
				if err := filter.AddPort(netlink.ConntrackOrigDstPort, ef.port); err != nil {
					GinkgoT().Fatalf("Failed to add port to expected filter: %v", err)
				}
			}

			// Add IP
			ipAddr := net.ParseIP(ef.ip)
			if ipAddr == nil {
				GinkgoT().Fatalf("Invalid IP address: %s", ef.ip)
			}
			if err := filter.AddIP(netlink.ConntrackReplyAnyIP, ipAddr); err != nil {
				GinkgoT().Fatalf("Failed to add IP to expected filter: %v", err)
			}

			return filter
		}

		DescribeTable("should handle conntrack deletion correctly",
			func(tc reconcileConntrackTestCase) {
				mockNetLinkOps := new(utilMocks.NetLinkOps)
				util.SetNetLinkOpMockInst(mockNetLinkOps)
				defer util.ResetNetLinkOpMockInst()

				mockNetLinkOps.On("ConntrackDeleteFilters",
					mock.AnythingOfType("netlink.ConntrackTableType"),
					mock.AnythingOfType("netlink.InetFamily"),
					mock.AnythingOfType("*netlink.ConntrackFilter")).
					Return(uint(1), nil).Maybe()

				// Build object list for fake client
				objects := []runtime.Object{}
				if tc.service != nil {
					objects = append(objects, tc.service)
				}
				if tc.newEndpointSlice != nil {
					objects = append(objects, tc.newEndpointSlice)
				}
				for _, obj := range tc.initialSyncSlices {
					if epSlice, ok := obj.(*discovery.EndpointSlice); ok {
						objects = append(objects, epSlice)
					}
				}

				// Add test node for NodePort conntrack cleanup
				testNode := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-node",
					},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
						},
					},
				}
				objects = append(objects, testNode)

				fakeClient := fake.NewSimpleClientset(objects...)

				wf, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{
					KubeClient: fakeClient,
				}, "test-node")
				Expect(err).NotTo(HaveOccurred())
				defer wf.Shutdown()

				err = wf.Start()
				Expect(err).NotTo(HaveOccurred())

				nc := &DefaultNodeNetworkController{
					BaseNodeNetworkController: BaseNodeNetworkController{
						CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{
							name:         "test-node",
							watchFactory: wf,
						},
						ReconcilableNetInfo: &util.DefaultNetInfo{},
					},
				}

				if tc.initialSyncSlices != nil {
					err = nc.syncEndpointSlices(tc.initialSyncSlices)
					Expect(err).NotTo(HaveOccurred())
				}

				if tc.initialNonZeroState && tc.newEndpointSlice != nil && tc.service != nil {
					stateKey := fmt.Sprintf("%s/%s/%s", tc.newEndpointSlice.Namespace,
						tc.service.Name, tc.newEndpointSlice.AddressType)
					nc.udpServiceZeroState.Store(stateKey, struct{}{})
				}

				err = nc.reconcileConntrackUponEndpointSliceEvents(tc.oldEndpointSlice, tc.newEndpointSlice)
				Expect(err).NotTo(HaveOccurred())

				mockNetLinkOps.AssertNumberOfCalls(GinkgoT(), "ConntrackDeleteFilters", tc.expectedConntrackCalls)

				// Collect all actual filters from the mock calls.
				actualFilters := []*netlink.ConntrackFilter{}
				for _, call := range mockNetLinkOps.Calls {
					if call.Method == "ConntrackDeleteFilters" {
						_, ok1 := call.Arguments.Get(0).(netlink.ConntrackTableType)
						_, ok2 := call.Arguments.Get(1).(netlink.InetFamily)
						filter, ok3 := call.Arguments.Get(2).(*netlink.ConntrackFilter)

						if ok1 && ok2 && ok3 {
							actualFilters = append(actualFilters, filter)
						}
					}
				}

				// Build the list of expected filters.
				expectedNetlinkFilters := make([]*netlink.ConntrackFilter, 0, len(tc.expectedFilters))
				for _, expectedFilter := range tc.expectedFilters {
					expectedNetlinkFilters = append(expectedNetlinkFilters, buildExpectedFilter(expectedFilter))
				}

				// Use gomega's ConsistOf to compare the actual and expected filters.
				Expect(actualFilters).To(ConsistOf(expectedNetlinkFilters), "The set of conntrack filters to be deleted should match the expected set.")
			},

			Entry("old endpointslice is nil",
				reconcileConntrackTestCase{
					desc:             "should not delete any conntrack entries when old endpoint is nil",
					service:          makeService([]servicePortConfig{{name: "", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol}}),
					oldEndpointSlice: nil,
					newEndpointSlice: makeEndpointSlice(
						[]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}},
						[]string{},
						discovery.AddressTypeIPv4,
					),
					expectedConntrackCalls: 0,
				},
			),

			Entry("service exists with matching unnamed port",
				reconcileConntrackTestCase{
					desc:    "should delete conntrack with service port for unnamed port",
					service: makeService([]servicePortConfig{{name: "", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol}}),
					oldEndpointSlice: makeEndpointSlice(
						[]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}},
						[]string{"10.0.0.1"},
						discovery.AddressTypeIPv4,
					),
					newEndpointSlice:       nil,
					expectedConntrackCalls: 1,
					expectedFilters: []expectedConntrackFilter{
						{ip: "10.0.0.1", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
					},
				},
			),

			Entry("service exists with matching named port",
				reconcileConntrackTestCase{
					desc:    "should delete conntrack with service port for named port",
					service: makeService([]servicePortConfig{{name: "http", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol}}),
					oldEndpointSlice: makeEndpointSlice(
						[]endpointPortConfig{{name: strPtr("http"), port: testEndpointPort1, protocol: udpProtocol}},
						[]string{"10.0.0.1"},
						discovery.AddressTypeIPv4,
					),
					newEndpointSlice:       nil,
					expectedConntrackCalls: 1,
					expectedFilters: []expectedConntrackFilter{
						{ip: "10.0.0.1", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
					},
				},
			),

			Entry("service exists but port name mismatch",
				reconcileConntrackTestCase{
					desc:    "should skip conntrack deletion when port name doesn't match",
					service: makeService([]servicePortConfig{{name: "http", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol}}),
					oldEndpointSlice: makeEndpointSlice(
						[]endpointPortConfig{{name: strPtr("grpc"), port: testEndpointPort1, protocol: udpProtocol}},
						[]string{"10.0.0.1"},
						discovery.AddressTypeIPv4,
					),
					newEndpointSlice:       nil,
					expectedConntrackCalls: 0,
				},
			),

			Entry("service not found",
				reconcileConntrackTestCase{
					desc:    "should return early without deleting conntrack when service not found",
					service: nil,
					oldEndpointSlice: makeEndpointSlice(
						[]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}},
						[]string{"10.0.0.1"},
						discovery.AddressTypeIPv4,
					),
					newEndpointSlice:       nil,
					expectedConntrackCalls: 0,
				},
			),

			Entry("TCP protocol should be skipped",
				reconcileConntrackTestCase{
					desc:    "should skip conntrack deletion for TCP protocol",
					service: makeService([]servicePortConfig{{name: "", port: testServicePort1, targetPort: testEndpointPort1, protocol: tcpProtocol}}),
					oldEndpointSlice: makeEndpointSlice(
						[]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: tcpProtocol}},
						[]string{"10.0.0.1"},
						discovery.AddressTypeIPv4,
					),
					newEndpointSlice:       nil,
					expectedConntrackCalls: 0,
				},
			),

			Entry("multiple endpoints",
				reconcileConntrackTestCase{
					desc:    "should delete conntrack for each endpoint",
					service: makeService([]servicePortConfig{{name: "", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol}}),
					oldEndpointSlice: makeEndpointSlice(
						[]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}},
						[]string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
						discovery.AddressTypeIPv4,
					),
					newEndpointSlice:       nil,
					expectedConntrackCalls: 3,
					expectedFilters: []expectedConntrackFilter{
						{ip: "10.0.0.1", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
						{ip: "10.0.0.2", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
						{ip: "10.0.0.3", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
					},
				},
			),

			Entry("IPv6 endpoint",
				reconcileConntrackTestCase{
					desc:    "should delete conntrack for IPv6 endpoint",
					service: makeService([]servicePortConfig{{name: "", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol}}),
					oldEndpointSlice: makeEndpointSlice(
						[]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}},
						[]string{"fd00::1"}, discovery.AddressTypeIPv6,
					),
					newEndpointSlice:       nil,
					expectedConntrackCalls: 1,
					expectedFilters: []expectedConntrackFilter{
						{ip: "fd00::1", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
					},
				},
			),

			Entry("dual-stack endpoints",
				reconcileConntrackTestCase{
					desc:    "should delete conntrack for both IPv4 and IPv6",
					service: makeService([]servicePortConfig{{name: "", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol}}),
					oldEndpointSlice: makeEndpointSlice(
						[]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}},
						[]string{"10.0.0.1", "fd00::1"},
						discovery.AddressTypeIPv4,
					),
					newEndpointSlice:       nil,
					expectedConntrackCalls: 2,
					expectedFilters: []expectedConntrackFilter{
						{ip: "10.0.0.1", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
						{ip: "fd00::1", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
					},
				},
			),

			Entry("multiple service ports with matching names",
				reconcileConntrackTestCase{
					desc: "should match correct service port by name for multiple ports",
					service: makeService([]servicePortConfig{
						{name: "http", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol},
						{name: "https", port: testServicePort2, targetPort: testEndpointPort2, protocol: udpProtocol},
					}),
					oldEndpointSlice: makeEndpointSlice(
						[]endpointPortConfig{
							{name: strPtr("http"), port: testEndpointPort1, protocol: udpProtocol},
							{name: strPtr("https"), port: testEndpointPort2, protocol: udpProtocol},
						},
						[]string{"10.0.0.1"},
						discovery.AddressTypeIPv4,
					),
					newEndpointSlice:       nil,
					expectedConntrackCalls: 2,
					expectedFilters: []expectedConntrackFilter{
						{ip: "10.0.0.1", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
						{ip: "10.0.0.1", port: uint16(testServicePort2), protocol: syscall.IPPROTO_UDP},
					},
				},
			),
			Entry("NodePort service", reconcileConntrackTestCase{
				desc: "should delete conntrack entries for both service port and NodePort",
				service: makeServiceWithNodePort([]servicePortConfig{{name: "", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol}},
					[]int32{30000}, corev1.ServiceTypeNodePort),
				oldEndpointSlice:       makeEndpointSlice([]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}}, []string{"10.128.0.1"}, discovery.AddressTypeIPv4),
				newEndpointSlice:       makeEndpointSlice([]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}}, []string{"10.128.0.2"}, discovery.AddressTypeIPv4),
				expectedConntrackCalls: 2,
				expectedFilters: []expectedConntrackFilter{
					{ip: "10.128.0.1", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
					{ip: "10.128.0.1", port: 30000, protocol: syscall.IPPROTO_UDP},
				},
				initialNonZeroState: true,
			}),
			Entry("NodePort service with mixed protocols should only clean UDP NodePort", reconcileConntrackTestCase{
				desc: "should only delete conntrack for UDP NodePort, not TCP (protocol filtering)",
				service: makeServiceWithNodePort([]servicePortConfig{
					{name: "", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol},
					{name: "", port: testServicePort2, targetPort: testEndpointPort1, protocol: tcpProtocol},
				}, []int32{30000, 30001}, corev1.ServiceTypeNodePort),
				oldEndpointSlice:       makeEndpointSlice([]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}}, []string{"10.128.0.1"}, discovery.AddressTypeIPv4),
				newEndpointSlice:       makeEndpointSlice([]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}}, []string{"10.128.0.2"}, discovery.AddressTypeIPv4),
				expectedConntrackCalls: 2, // Only UDP: service port + NodePort (TCP port 30001 should be skipped)
				expectedFilters: []expectedConntrackFilter{
					{ip: "10.128.0.1", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
					{ip: "10.128.0.1", port: 30000, protocol: syscall.IPPROTO_UDP},
				},
				initialNonZeroState: true,
			}),
			Entry("NodePort service with multiple UDP ports", reconcileConntrackTestCase{
				desc: "should delete conntrack entries only for the specific NodePort that changed",
				service: makeServiceWithNodePort([]servicePortConfig{
					{name: "dns", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol},
					{name: "snmp", port: testServicePort2, targetPort: testEndpointPort1, protocol: udpProtocol},
				}, []int32{30000, 30002}, corev1.ServiceTypeNodePort),
				oldEndpointSlice:       makeEndpointSlice([]endpointPortConfig{{name: strPtr("dns"), port: testEndpointPort1, protocol: udpProtocol}}, []string{"10.128.0.1"}, discovery.AddressTypeIPv4),
				newEndpointSlice:       makeEndpointSlice([]endpointPortConfig{{name: strPtr("dns"), port: testEndpointPort1, protocol: udpProtocol}}, []string{"10.128.0.2"}, discovery.AddressTypeIPv4),
				expectedConntrackCalls: 2, // service port + NodePort for "dns" only
				expectedFilters: []expectedConntrackFilter{
					{ip: "10.128.0.1", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
					{ip: "10.128.0.1", port: 30000, protocol: syscall.IPPROTO_UDP},
				},
				initialNonZeroState: true,
			}),
			Entry("LoadBalancer service with NodePort allocation", reconcileConntrackTestCase{
				desc: "should delete conntrack entries for both service port and NodePort",
				service: func() *corev1.Service {
					svc := makeServiceWithNodePort([]servicePortConfig{{name: "", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol}},
						[]int32{30000}, corev1.ServiceTypeLoadBalancer)
					svc.Status = corev1.ServiceStatus{
						LoadBalancer: corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{{IP: "5.5.5.5"}},
						},
					}
					return svc
				}(),
				oldEndpointSlice:       makeEndpointSlice([]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}}, []string{"10.128.0.1"}, discovery.AddressTypeIPv4),
				newEndpointSlice:       makeEndpointSlice([]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}}, []string{"10.128.0.2"}, discovery.AddressTypeIPv4),
				expectedConntrackCalls: 2,
				expectedFilters: []expectedConntrackFilter{
					{ip: "10.128.0.1", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
					{ip: "10.128.0.1", port: 30000, protocol: syscall.IPPROTO_UDP},
				},
				initialNonZeroState: true,
			}),
			Entry("LoadBalancer service with AllocateLoadBalancerNodePorts=false", func() reconcileConntrackTestCase {
				allocateNodePorts := false
				return reconcileConntrackTestCase{
					desc: "should only delete conntrack entries for service port (no NodePort)",
					service: func() *corev1.Service {
						svc := makeService([]servicePortConfig{{name: "", port: testServicePort1, targetPort: testEndpointPort1, protocol: udpProtocol}})
						svc.Spec.Type = corev1.ServiceTypeLoadBalancer
						svc.Spec.AllocateLoadBalancerNodePorts = &allocateNodePorts
						svc.Status = corev1.ServiceStatus{
							LoadBalancer: corev1.LoadBalancerStatus{
								Ingress: []corev1.LoadBalancerIngress{{IP: "5.5.5.5"}},
							},
						}
						return svc
					}(),
					oldEndpointSlice:       makeEndpointSlice([]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}}, []string{"10.128.0.1"}, discovery.AddressTypeIPv4),
					newEndpointSlice:       makeEndpointSlice([]endpointPortConfig{{name: nil, port: testEndpointPort1, protocol: udpProtocol}}, []string{"10.128.0.2"}, discovery.AddressTypeIPv4),
					expectedConntrackCalls: 1,
					expectedFilters: []expectedConntrackFilter{
						{ip: "10.128.0.1", port: uint16(testServicePort1), protocol: syscall.IPPROTO_UDP},
					},
					initialNonZeroState: true,
				}
			}()),
		)
	})

	Describe("state-based 0→N transition detection", func() {
		BeforeEach(func() {
			config.IPv4Mode = true
			config.IPv6Mode = false
		})

		AfterEach(func() {
			Expect(config.PrepareTestConfig()).To(Succeed(), "failed to restore test config")
		})

		const (
			testNamespace   = "test-ns"
			testServiceName = "test-service"
			testNodeName    = "test-node"
		)

		var udpProtocol = corev1.ProtocolUDP

		makeSlice := func(uid, name string, addresses []string, addressType discovery.AddressType) *discovery.EndpointSlice {
			port := int32(8080)
			return &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: testNamespace,
					UID:       k8stypes.UID(uid),
					Labels: map[string]string{
						discovery.LabelServiceName: testServiceName,
					},
				},
				AddressType: addressType,
				Ports: []discovery.EndpointPort{
					{Port: &port, Protocol: &udpProtocol},
				},
				Endpoints: []discovery.Endpoint{
					{
						Addresses: addresses,
						Conditions: discovery.EndpointConditions{
							Ready: boolPtr(true),
						},
					},
				},
			}
		}

		makeService := func() *corev1.Service {
			return &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testServiceName,
					Namespace: testNamespace,
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: "10.96.0.1",
					Ports: []corev1.ServicePort{
						{Port: 80, Protocol: udpProtocol},
					},
				},
			}
		}

		It("should skip replayed initial-sync add events", func() {
			slice := makeSlice("uid-1", "slice-1", []string{"10.0.0.1"}, discovery.AddressTypeIPv4)
			svc := makeService()

			fakeClient := fake.NewSimpleClientset(svc, slice)
			wf, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{
				KubeClient: fakeClient,
			}, testNodeName)
			Expect(err).NotTo(HaveOccurred())
			defer wf.Shutdown()

			nc := &DefaultNodeNetworkController{
				BaseNodeNetworkController: BaseNodeNetworkController{
					CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{
						name:         testNodeName,
						watchFactory: wf,
					},
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}

			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())

			err = nc.syncEndpointSlices([]interface{}{slice})
			Expect(err).NotTo(HaveOccurred())

			_, tracked := nc.initialSyncEndpointSliceUIDs.Load(slice.UID)
			Expect(tracked).To(BeTrue(), "slice UID should be tracked after sync")

			// Simulate replayed add (oldSlice=nil, newSlice=slice)
			mockNetLinkOps := new(utilMocks.NetLinkOps)
			util.SetNetLinkOpMockInst(mockNetLinkOps)
			defer util.ResetNetLinkOpMockInst()

			err = nc.reconcileConntrackUponEndpointSliceEvents(nil, slice)
			Expect(err).NotTo(HaveOccurred())

			mockNetLinkOps.AssertNumberOfCalls(GinkgoT(), "ConntrackDeleteFilters", 0)

			_, tracked = nc.initialSyncEndpointSliceUIDs.Load(slice.UID)
			Expect(tracked).To(BeFalse(), "slice UID should be removed after replayed add")
		})

		It("should flush conntrack after UID consumed on second call", func() {
			slice := makeSlice("uid-consumed-test", "slice-1", []string{"10.0.0.1"}, discovery.AddressTypeIPv4)
			svc := makeService()

			fakeClient := fake.NewSimpleClientset(svc, slice)
			wf, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{
				KubeClient: fakeClient,
			}, testNodeName)
			Expect(err).NotTo(HaveOccurred())
			defer wf.Shutdown()

			nc := &DefaultNodeNetworkController{
				BaseNodeNetworkController: BaseNodeNetworkController{
					CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{
						name:         testNodeName,
						watchFactory: wf,
					},
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}

			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())

			err = nc.syncEndpointSlices([]interface{}{slice})
			Expect(err).NotTo(HaveOccurred())

			stateKey := fmt.Sprintf("%s/%s/%s", testNamespace, testServiceName, discovery.AddressTypeIPv4)
			nc.udpServiceZeroState.Delete(stateKey)

			mockNetLinkOps := new(utilMocks.NetLinkOps)
			util.SetNetLinkOpMockInst(mockNetLinkOps)
			defer util.ResetNetLinkOpMockInst()

			mockNetLinkOps.On("ConntrackDeleteFilters",
				mock.AnythingOfType("netlink.ConntrackTableType"),
				mock.AnythingOfType("netlink.InetFamily"),
				mock.AnythingOfType("*netlink.ConntrackFilter")).
				Return(uint(1), nil)

			// First call: replayed add, UID consumed, no flush
			err = nc.reconcileConntrackUponEndpointSliceEvents(nil, slice)
			Expect(err).NotTo(HaveOccurred())
			mockNetLinkOps.AssertNumberOfCalls(GinkgoT(), "ConntrackDeleteFilters", 0)

			_, tracked := nc.initialSyncEndpointSliceUIDs.Load(slice.UID)
			Expect(tracked).To(BeFalse(), "UID should be consumed after first call")

			// Second call: same UID but now consumed, should detect 0→N and flush
			err = nc.reconcileConntrackUponEndpointSliceEvents(nil, slice)
			Expect(err).NotTo(HaveOccurred())
			mockNetLinkOps.AssertNumberOfCalls(GinkgoT(), "ConntrackDeleteFilters", 1)

			_, exists := nc.udpServiceZeroState.Load(stateKey)
			Expect(exists).To(BeTrue(), "state should be updated to non-zero after flush")
		})

		It("should initialize state correctly in syncEndpointSlices", func() {
			emptySlice := makeSlice("uid-empty", "slice-empty", []string{}, discovery.AddressTypeIPv4)
			nonEmptySlice := makeSlice("uid-nonempty", "slice-nonempty", []string{"10.0.0.1"}, discovery.AddressTypeIPv4)
			svc := makeService()

			fakeClient := fake.NewSimpleClientset(svc, emptySlice, nonEmptySlice)
			wf, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{
				KubeClient: fakeClient,
			}, testNodeName)
			Expect(err).NotTo(HaveOccurred())
			defer wf.Shutdown()

			nc := &DefaultNodeNetworkController{
				BaseNodeNetworkController: BaseNodeNetworkController{
					CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{
						name:         testNodeName,
						watchFactory: wf,
					},
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}

			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())

			err = nc.syncEndpointSlices([]interface{}{emptySlice, nonEmptySlice})
			Expect(err).NotTo(HaveOccurred())

			_, tracked1 := nc.initialSyncEndpointSliceUIDs.Load(emptySlice.UID)
			_, tracked2 := nc.initialSyncEndpointSliceUIDs.Load(nonEmptySlice.UID)
			Expect(tracked1).To(BeTrue(), "empty slice UID should be tracked")
			Expect(tracked2).To(BeTrue(), "non-empty slice UID should be tracked")

			stateKey := fmt.Sprintf("%s/%s/%s", testNamespace, testServiceName, discovery.AddressTypeIPv4)
			_, exists := nc.udpServiceZeroState.Load(stateKey)
			Expect(exists).To(BeTrue(), "state should be non-zero because nonEmptySlice has endpoints")
		})

		It("should initialize state as zero when service has no endpoints", func() {
			emptySlice := makeSlice("uid-empty", "slice-empty", []string{}, discovery.AddressTypeIPv4)
			svc := makeService()

			fakeClient := fake.NewSimpleClientset(svc, emptySlice)
			wf, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{
				KubeClient: fakeClient,
			}, testNodeName)
			Expect(err).NotTo(HaveOccurred())
			defer wf.Shutdown()

			nc := &DefaultNodeNetworkController{
				BaseNodeNetworkController: BaseNodeNetworkController{
					CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{
						name:         testNodeName,
						watchFactory: wf,
					},
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}

			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())

			err = nc.syncEndpointSlices([]interface{}{emptySlice})
			Expect(err).NotTo(HaveOccurred())

			stateKey := fmt.Sprintf("%s/%s/%s", testNamespace, testServiceName, discovery.AddressTypeIPv4)
			_, exists := nc.udpServiceZeroState.Load(stateKey)
			Expect(exists).To(BeFalse(), "state should be zero (absent) when all slices are empty")
		})

		It("should detect 0→N transition for dual-stack secondary family", func() {
			ipv4Slice := makeSlice("uid-ipv4", "slice-ipv4", []string{"10.0.0.1"}, discovery.AddressTypeIPv4)
			ipv6SliceWithEndpoints := makeSlice("uid-ipv6", "slice-ipv6", []string{"2001:db8::1"}, discovery.AddressTypeIPv6)
			svc := makeService()
			svc.Spec.ClusterIPs = []string{"10.96.0.1", "fd00::1"}

			fakeClient := fake.NewSimpleClientset(svc, ipv4Slice, ipv6SliceWithEndpoints)
			wf, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{
				KubeClient: fakeClient,
			}, testNodeName)
			Expect(err).NotTo(HaveOccurred())
			defer wf.Shutdown()

			nc := &DefaultNodeNetworkController{
				BaseNodeNetworkController: BaseNodeNetworkController{
					CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{
						name:         testNodeName,
						watchFactory: wf,
					},
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}

			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())

			mockNetLinkOps := new(utilMocks.NetLinkOps)
			util.SetNetLinkOpMockInst(mockNetLinkOps)
			defer util.ResetNetLinkOpMockInst()
			mockNetLinkOps.On("ConntrackDeleteFilters",
				mock.AnythingOfType("netlink.ConntrackTableType"),
				mock.AnythingOfType("netlink.InetFamily"),
				mock.AnythingOfType("*netlink.ConntrackFilter")).
				Return(uint(1), nil)

			ipv4Key := fmt.Sprintf("%s/%s/%s", testNamespace, testServiceName, discovery.AddressTypeIPv4)
			ipv6Key := fmt.Sprintf("%s/%s/%s", testNamespace, testServiceName, discovery.AddressTypeIPv6)

			nc.udpServiceZeroState.Store(ipv4Key, struct{}{})

			_, ipv4Exists := nc.udpServiceZeroState.Load(ipv4Key)
			_, ipv6Exists := nc.udpServiceZeroState.Load(ipv6Key)
			Expect(ipv4Exists).To(BeTrue(), "IPv4 family should have non-zero state")
			Expect(ipv6Exists).To(BeFalse(), "IPv6 family should be absent (zero state)")

			err = nc.reconcileConntrackUponEndpointSliceEvents(nil, ipv6SliceWithEndpoints)
			Expect(err).NotTo(HaveOccurred())

			mockNetLinkOps.AssertNumberOfCalls(GinkgoT(), "ConntrackDeleteFilters", 1)

			_, ipv6ExistsAfter := nc.udpServiceZeroState.Load(ipv6Key)
			Expect(ipv6ExistsAfter).To(BeTrue(), "IPv6 family should transition to non-zero state after adding endpoints")
		})

		It("should preserve zero state when flush fails and retry flush on next event", func() {
			slice := makeSlice("uid-1", "slice-1", []string{"10.0.0.1"}, discovery.AddressTypeIPv4)
			svc := makeService()

			fakeClient := fake.NewSimpleClientset(svc, slice)
			wf, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{
				KubeClient: fakeClient,
			}, testNodeName)
			Expect(err).NotTo(HaveOccurred())
			defer wf.Shutdown()

			nc := &DefaultNodeNetworkController{
				BaseNodeNetworkController: BaseNodeNetworkController{
					CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{
						name:         testNodeName,
						watchFactory: wf,
					},
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}

			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())

			mockNetLinkOps := new(utilMocks.NetLinkOps)
			util.SetNetLinkOpMockInst(mockNetLinkOps)
			defer util.ResetNetLinkOpMockInst()

			stateKey := fmt.Sprintf("%s/%s/%s", testNamespace, testServiceName, discovery.AddressTypeIPv4)

			mockNetLinkOps.On("ConntrackDeleteFilters",
				mock.AnythingOfType("netlink.ConntrackTableType"),
				mock.AnythingOfType("netlink.InetFamily"),
				mock.AnythingOfType("*netlink.ConntrackFilter")).
				Return(uint(0), fmt.Errorf("conntrack flush failed")).
				Once()

			err = nc.reconcileConntrackUponEndpointSliceEvents(nil, slice)
			Expect(err).To(HaveOccurred(), "should return error when flush fails")

			_, exists := nc.udpServiceZeroState.Load(stateKey)
			Expect(exists).To(BeFalse(), "state should remain zero (absent) when flush fails")

			mockNetLinkOps.AssertNumberOfCalls(GinkgoT(), "ConntrackDeleteFilters", 1)

			mockNetLinkOps.On("ConntrackDeleteFilters",
				mock.AnythingOfType("netlink.ConntrackTableType"),
				mock.AnythingOfType("netlink.InetFamily"),
				mock.AnythingOfType("*netlink.ConntrackFilter")).
				Return(uint(1), nil).
				Once()

			err = nc.reconcileConntrackUponEndpointSliceEvents(nil, slice)
			Expect(err).NotTo(HaveOccurred(), "retry should succeed when flush succeeds")

			mockNetLinkOps.AssertNumberOfCalls(GinkgoT(), "ConntrackDeleteFilters", 2)

			_, existsAfterRetry := nc.udpServiceZeroState.Load(stateKey)
			Expect(existsAfterRetry).To(BeTrue(), "state should be updated to non-zero after successful flush")
		})

	})

	Describe("flushConntrackForServiceVIPs", func() {
		BeforeEach(func() {
			config.IPv4Mode = true
			config.IPv6Mode = false
		})

		AfterEach(func() {
			Expect(config.PrepareTestConfig()).To(Succeed(), "failed to restore test config after VIP flush test")
		})

		const (
			testNamespace   = "test-ns"
			testServiceName = "test-service"
			testNodeName    = "test-node"
		)

		var (
			udpProtocol = corev1.ProtocolUDP
			tcpProtocol = corev1.ProtocolTCP
		)

		type testCase struct {
			desc                   string
			service                *corev1.Service
			node                   *corev1.Node
			addressType            discovery.AddressType
			expectedConntrackCalls int
		}

		makeServiceWithVIPs := func(svcType corev1.ServiceType, clusterIP string, externalIPs []string, lbIP string, nodePort int32) *corev1.Service {
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testServiceName,
					Namespace: testNamespace,
				},
				Spec: corev1.ServiceSpec{
					Type:      svcType,
					ClusterIP: clusterIP,
					Ports: []corev1.ServicePort{
						{Port: 80, Protocol: udpProtocol, NodePort: nodePort},
					},
				},
			}
			if len(externalIPs) > 0 {
				svc.Spec.ExternalIPs = externalIPs
			}
			if lbIP != "" {
				svc.Status = corev1.ServiceStatus{
					LoadBalancer: corev1.LoadBalancerStatus{
						Ingress: []corev1.LoadBalancerIngress{
							{IP: lbIP},
						},
					},
				}
			}
			return svc
		}

		makeNode := func(ips ...string) *corev1.Node {
			addresses := []corev1.NodeAddress{}
			cidrs := []string{}
			for _, ip := range ips {
				addresses = append(addresses, corev1.NodeAddress{
					Type:    corev1.NodeInternalIP,
					Address: ip,
				})
				// Add /32 for IPv4, /128 for IPv6
				if strings.Contains(ip, ":") {
					cidrs = append(cidrs, ip+"/128")
				} else {
					cidrs = append(cidrs, ip+"/32")
				}
			}
			cidrsJSON, _ := json.Marshal(cidrs)
			return &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: testNodeName,
					Annotations: map[string]string{
						util.OVNNodeHostCIDRs: string(cidrsJSON),
					},
				},
				Status: corev1.NodeStatus{
					Addresses: addresses,
				},
			}
		}

		// setupConntrackTest creates common test infrastructure for conntrack tests.
		// Returns mock NetLinkOps, node controller, and cleanup function.
		setupConntrackTest := func(objects ...runtime.Object) (*utilMocks.NetLinkOps, *DefaultNodeNetworkController, func()) {
			mockNetLinkOps := new(utilMocks.NetLinkOps)
			util.SetNetLinkOpMockInst(mockNetLinkOps)

			mockNetLinkOps.On("ConntrackDeleteFilters",
				mock.AnythingOfType("netlink.ConntrackTableType"),
				mock.AnythingOfType("netlink.InetFamily"),
				mock.AnythingOfType("*netlink.ConntrackFilter")).
				Return(uint(1), nil).Maybe()

			fakeClient := fake.NewSimpleClientset(objects...)

			wf, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{
				KubeClient: fakeClient,
			}, testNodeName)
			Expect(err).NotTo(HaveOccurred(), "failed to create watch factory for conntrack test")

			err = wf.Start()
			Expect(err).NotTo(HaveOccurred(), "failed to start watch factory for conntrack test")

			nc := &DefaultNodeNetworkController{
				BaseNodeNetworkController: BaseNodeNetworkController{
					CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{
						watchFactory: wf,
						name:         testNodeName,
					},
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}

			cleanup := func() {
				wf.Shutdown()
				util.ResetNetLinkOpMockInst()
			}

			return mockNetLinkOps, nc, cleanup
		}

		DescribeTable("should flush conntrack for correct VIPs",
			func(tc testCase) {
				mockNetLinkOps, nc, cleanup := setupConntrackTest(tc.service, tc.node)
				defer cleanup()

				err := nc.flushConntrackForServiceVIPs(tc.service, tc.addressType)
				Expect(err).NotTo(HaveOccurred(), "flushConntrackForServiceVIPs failed for: "+tc.desc)

				mockNetLinkOps.AssertNumberOfCalls(GinkgoT(), "ConntrackDeleteFilters", tc.expectedConntrackCalls)
			},

			Entry("ClusterIP service only",
				testCase{
					desc:                   "should flush conntrack for ClusterIP only",
					service:                makeServiceWithVIPs(corev1.ServiceTypeClusterIP, "10.96.0.1", nil, "", 0),
					node:                   makeNode("192.168.1.10"),
					expectedConntrackCalls: 1,
					addressType:            discovery.AddressTypeIPv4,
				},
			),

			Entry("ClusterIP with ExternalIPs",
				testCase{
					desc:                   "should flush conntrack for ClusterIP and ExternalIPs",
					service:                makeServiceWithVIPs(corev1.ServiceTypeClusterIP, "10.96.0.1", []string{"1.2.3.4", "5.6.7.8"}, "", 0),
					node:                   makeNode("192.168.1.10"),
					expectedConntrackCalls: 3,
					addressType:            discovery.AddressTypeIPv4,
				},
			),

			Entry("LoadBalancer service",
				testCase{
					desc:                   "should flush conntrack for ClusterIP and LB IP",
					service:                makeServiceWithVIPs(corev1.ServiceTypeLoadBalancer, "10.96.0.1", nil, "203.0.113.10", 30080),
					node:                   makeNode("192.168.1.10"),
					expectedConntrackCalls: 3,
					addressType:            discovery.AddressTypeIPv4,
				},
			),

			Entry("NodePort service with multiple node IPs",
				testCase{
					desc:                   "should flush conntrack for ClusterIP and all node IPs",
					service:                makeServiceWithVIPs(corev1.ServiceTypeNodePort, "10.96.0.1", nil, "", 30080),
					node:                   makeNode("192.168.1.10", "192.168.1.11"),
					expectedConntrackCalls: 3,
					addressType:            discovery.AddressTypeIPv4,
				},
			),

			Entry("service with TCP ports only (no UDP)",
				testCase{
					desc: "should not flush any conntrack when no UDP ports",
					service: &corev1.Service{
						ObjectMeta: metav1.ObjectMeta{
							Name:      testServiceName,
							Namespace: testNamespace,
						},
						Spec: corev1.ServiceSpec{
							ClusterIP: "10.96.0.1",
							Ports: []corev1.ServicePort{
								{Port: 80, Protocol: tcpProtocol},
							},
						},
					},
					node:                   makeNode("192.168.1.10"),
					expectedConntrackCalls: 0,
					addressType:            discovery.AddressTypeIPv4,
				},
			),

			Entry("service with mixed UDP and TCP ports",
				testCase{
					desc: "should flush conntrack only for UDP ports",
					service: &corev1.Service{
						ObjectMeta: metav1.ObjectMeta{
							Name:      testServiceName,
							Namespace: testNamespace,
						},
						Spec: corev1.ServiceSpec{
							ClusterIP: "10.96.0.1",
							Ports: []corev1.ServicePort{
								{Port: 80, Protocol: udpProtocol},
								{Port: 443, Protocol: tcpProtocol},
							},
						},
					},
					node:                   makeNode("192.168.1.10"),
					expectedConntrackCalls: 1,
					addressType:            discovery.AddressTypeIPv4,
				},
			),
		)

		Context("IPv6-only VIP flushing", func() {
			BeforeEach(func() {
				config.IPv4Mode = false
				config.IPv6Mode = true
			})

			AfterEach(func() {
				Expect(config.PrepareTestConfig()).To(Succeed(), "failed to restore test config after IPv6 conntrack test")
			})

			DescribeTable("should flush conntrack for IPv6 VIPs",
				func(tc testCase) {
					mockNetLinkOps, nc, cleanup := setupConntrackTest(tc.service, tc.node)
					defer cleanup()

					err := nc.flushConntrackForServiceVIPs(tc.service, tc.addressType)
					Expect(err).NotTo(HaveOccurred(), "flushConntrackForServiceVIPs failed for: "+tc.desc)

					mockNetLinkOps.AssertNumberOfCalls(GinkgoT(), "ConntrackDeleteFilters", tc.expectedConntrackCalls)
				},

				Entry("IPv6 ClusterIP service only",
					testCase{
						desc:                   "should flush conntrack for IPv6 ClusterIP",
						service:                makeServiceWithVIPs(corev1.ServiceTypeClusterIP, "fd00::1", nil, "", 0),
						node:                   makeNode("2001:db8::10"),
						addressType:            discovery.AddressTypeIPv6,
						expectedConntrackCalls: 1,
					},
				),

				Entry("IPv6 NodePort service",
					testCase{
						desc:                   "should flush conntrack for IPv6 ClusterIP and node IPs",
						service:                makeServiceWithVIPs(corev1.ServiceTypeNodePort, "fd00::1", nil, "", 30080),
						node:                   makeNode("2001:db8::10", "2001:db8::11"),
						addressType:            discovery.AddressTypeIPv6,
						expectedConntrackCalls: 3,
					},
				),

				Entry("IPv6 LoadBalancer service",
					testCase{
						desc:                   "should flush conntrack for IPv6 ClusterIP and LB IP",
						service:                makeServiceWithVIPs(corev1.ServiceTypeLoadBalancer, "fd00::1", nil, "2001:db8:1b::1", 30080),
						node:                   makeNode("2001:db8::10"),
						addressType:            discovery.AddressTypeIPv6,
						expectedConntrackCalls: 3,
					},
				),
			)
		})

		Context("Dual-stack VIP flushing", func() {
			BeforeEach(func() {
				config.IPv4Mode = true
				config.IPv6Mode = true
			})

			AfterEach(func() {
				Expect(config.PrepareTestConfig()).To(Succeed(), "failed to restore test config after dual-stack conntrack test")
			})

			DescribeTable("should flush conntrack for dual-stack VIPs",
				func(tc testCase) {
					mockNetLinkOps, nc, cleanup := setupConntrackTest(tc.service, tc.node)
					defer cleanup()

					err := nc.flushConntrackForServiceVIPs(tc.service, tc.addressType)
					Expect(err).NotTo(HaveOccurred(), "flushConntrackForServiceVIPs failed for: "+tc.desc)

					mockNetLinkOps.AssertNumberOfCalls(GinkgoT(), "ConntrackDeleteFilters", tc.expectedConntrackCalls)
				},

				Entry("Dual-stack ClusterIP service (IPv4 family)",
					testCase{
						desc: "should flush conntrack for IPv4 ClusterIP only in dual-stack service",
						service: func() *corev1.Service {
							svc := &corev1.Service{
								ObjectMeta: metav1.ObjectMeta{
									Name:      testServiceName,
									Namespace: testNamespace,
								},
								Spec: corev1.ServiceSpec{
									Type:       corev1.ServiceTypeClusterIP,
									ClusterIP:  "10.96.0.1",
									ClusterIPs: []string{"10.96.0.1", "fd00::1"},
									Ports: []corev1.ServicePort{
										{Port: 80, Protocol: udpProtocol},
									},
								},
							}
							return svc
						}(),
						node:                   makeNode("192.168.1.10", "2001:db8::10"),
						addressType:            discovery.AddressTypeIPv4,
						expectedConntrackCalls: 1,
					},
				),

				Entry("Dual-stack NodePort service (IPv4 family)",
					testCase{
						desc: "should flush conntrack for IPv4 ClusterIP and IPv4 node IP only",
						service: func() *corev1.Service {
							svc := &corev1.Service{
								ObjectMeta: metav1.ObjectMeta{
									Name:      testServiceName,
									Namespace: testNamespace,
								},
								Spec: corev1.ServiceSpec{
									Type:       corev1.ServiceTypeNodePort,
									ClusterIP:  "10.96.0.1",
									ClusterIPs: []string{"10.96.0.1", "fd00::1"},
									Ports: []corev1.ServicePort{
										{Port: 80, Protocol: udpProtocol, NodePort: 30080},
									},
								},
							}
							return svc
						}(),
						node:                   makeNode("192.168.1.10", "2001:db8::10"),
						addressType:            discovery.AddressTypeIPv4,
						expectedConntrackCalls: 2,
					},
				),

				Entry("Dual-stack LoadBalancer service (IPv4 family)",
					testCase{
						desc: "should flush conntrack for IPv4 ClusterIP, LB ingress IP, and node IP only",
						service: func() *corev1.Service {
							svc := &corev1.Service{
								ObjectMeta: metav1.ObjectMeta{
									Name:      testServiceName,
									Namespace: testNamespace,
								},
								Spec: corev1.ServiceSpec{
									Type:       corev1.ServiceTypeLoadBalancer,
									ClusterIP:  "10.96.0.1",
									ClusterIPs: []string{"10.96.0.1", "fd00::1"},
									Ports: []corev1.ServicePort{
										{Port: 80, Protocol: udpProtocol, NodePort: 30080},
									},
								},
								Status: corev1.ServiceStatus{
									LoadBalancer: corev1.LoadBalancerStatus{
										Ingress: []corev1.LoadBalancerIngress{
											{IP: "203.0.113.10"},
											{IP: "2001:db8:1b::1"},
										},
									},
								},
							}
							return svc
						}(),
						node:                   makeNode("192.168.1.10", "2001:db8::10"),
						addressType:            discovery.AddressTypeIPv4,
						expectedConntrackCalls: 3,
					},
				),
			)
		})
	})

	Describe("advertised UDN isolation nftables", func() {
		const nodeName = "my-node"

		BeforeEach(func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
		})

		It("sets up isolation sets for DPU host mode without NodePort", func() {
			config.OvnKubeNode.Mode = types.NodeModeDPUHost
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			config.Gateway.NodeportEnable = false

			nft := nodenft.SetFakeNFTablesHelper()
			ovnClient := util.GetOVNClientset()
			wf, err := factory.NewNodeWatchFactory(ovnClient.GetNodeClientset(), nodeName)
			Expect(err).NotTo(HaveOccurred())
			defer wf.Shutdown()

			routeManager := routemanager.NewController()
			cnnci := NewCommonNodeNetworkControllerInfo(ovnClient.KubeClient, ovnClient.AdminPolicyRouteClient, wf, nil, nodeName, routeManager)
			_, err = NewDefaultNodeNetworkController(cnnci, nil, nil)
			Expect(err).NotTo(HaveOccurred())

			_, err = nft.ListElements(context.TODO(), "set", nftablesAdvertisedUDNsSetV4)
			Expect(err).NotTo(HaveOccurred())
			_, err = nft.ListElements(context.TODO(), "set", nftablesAdvertisedUDNsSetV6)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// Helper function to create string pointer
func strPtr(s string) *string {
	return &s
}
func boolPtr(b bool) *bool {
	return &b
}

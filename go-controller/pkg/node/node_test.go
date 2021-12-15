package node

import (
	"fmt"

	"github.com/stretchr/testify/mock"
	"github.com/urfave/cli/v2"
	"github.com/vishvananda/netlink"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	netlink_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Node", func() {

	Describe("validateMTU", func() {
		var (
			kubeMock        *mocks.KubeInterface
			netlinkOpsMock  *utilMocks.NetLinkOps
			netlinkLinkMock *netlink_mocks.Link

			ovnNode *OvnNode
		)

		const (
			nodeName  = "my-node"
			linkName  = "breth0"
			linkIPNet = "10.1.0.40/32"
			linkIndex = 4

			configDefaultMTU               = 1500 //value for config.Default.MTU
			mtuTooSmallForIPv4AndIPv6      = configDefaultMTU + types.GeneveHeaderLengthIPv4 - 1
			mtuOkForIPv4ButTooSmallForIPv6 = configDefaultMTU + types.GeneveHeaderLengthIPv4
			mtuOkForIPv4AndIPv6            = configDefaultMTU + types.GeneveHeaderLengthIPv6
		)

		BeforeEach(func() {
			kubeMock = new(mocks.KubeInterface)
			netlinkOpsMock = new(utilMocks.NetLinkOps)
			netlinkLinkMock = new(netlink_mocks.Link)

			util.SetNetLinkOpMockInst(netlinkOpsMock)
			netlinkOpsMock.On("AddrList", nil, netlink.FAMILY_V4).
				Return([]netlink.Addr{{LinkIndex: linkIndex, IPNet: ovntest.MustParseIPNet(linkIPNet)}}, nil)
			netlinkOpsMock.On("LinkByIndex", 4).Return(netlinkLinkMock, nil)

			ovnNode = &OvnNode{
				name: nodeName,
				Kube: kubeMock,
			}

			config.Default.MTU = configDefaultMTU
			config.Default.EncapIP = "10.1.0.40"

			kubeMock.On("SetTaintOnNode", nodeName, mock.AnythingOfType("*v1.Taint")).Return(nil)
			kubeMock.On("RemoveTaintFromNode", nodeName, mock.AnythingOfType("*v1.Taint")).Return(nil)
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

					err := ovnNode.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
					kubeMock.AssertNumberOfCalls(GinkgoT(), "SetTaintOnNode", 1)
					kubeMock.AssertNumberOfCalls(GinkgoT(), "RemoveTaintFromNode", 0)
				})
			})

			Context("with the node having a big enough MTU", func() {

				It("should untaint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForIPv4ButTooSmallForIPv6,
						Name: linkName,
					})

					err := ovnNode.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
					kubeMock.AssertNumberOfCalls(GinkgoT(), "SetTaintOnNode", 0)
					kubeMock.AssertNumberOfCalls(GinkgoT(), "RemoveTaintFromNode", 1)
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

					err := ovnNode.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
					kubeMock.AssertNumberOfCalls(GinkgoT(), "SetTaintOnNode", 1)
					kubeMock.AssertNumberOfCalls(GinkgoT(), "RemoveTaintFromNode", 0)
				})
			})

			Context("with the node having a big enough MTU", func() {

				It("should untaint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForIPv4AndIPv6,
						Name: linkName,
					})

					err := ovnNode.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
					kubeMock.AssertNumberOfCalls(GinkgoT(), "SetTaintOnNode", 0)
					kubeMock.AssertNumberOfCalls(GinkgoT(), "RemoveTaintFromNode", 1)
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

					err := ovnNode.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
					kubeMock.AssertNumberOfCalls(GinkgoT(), "SetTaintOnNode", 1)
					kubeMock.AssertNumberOfCalls(GinkgoT(), "RemoveTaintFromNode", 0)
				})
			})

			Context("with the node having a big enough MTU", func() {

				It("should untaint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForIPv4AndIPv6,
						Name: linkName,
					})

					err := ovnNode.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
					kubeMock.AssertNumberOfCalls(GinkgoT(), "SetTaintOnNode", 0)
					kubeMock.AssertNumberOfCalls(GinkgoT(), "RemoveTaintFromNode", 1)
				})
			})
		})
	})

	Describe("Node Operations", func() {
		var app *cli.App

		BeforeEach(func() {
			// Restore global default values before each testcase
			config.PrepareTestConfig()

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
					ofintval int    = 180
				)
				node := kapi.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: kapi.NodeStatus{
						Addresses: []kapi.NodeAddress{
							{
								Type:    kapi.NodeExternalIP,
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
						"external_ids:ovn-openflow-probe-interval=%d "+
						"external_ids:hostname=\"%s\" "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-enable-lflow-cache=true",
						nodeIP, interval, ofintval, nodeName),
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
					ofintval    int    = 180
					chassisUUID string = "1a3dfc82-2749-4931-9190-c30e7c0ecea3"
					encapUUID   string = "e4437094-0094-4223-9f14-995d98d5fff8"
				)
				node := kapi.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: kapi.NodeStatus{
						Addresses: []kapi.NodeAddress{
							{
								Type:    kapi.NodeExternalIP,
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
						"external_ids:ovn-openflow-probe-interval=%d "+
						"external_ids:hostname=\"%s\" "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-enable-lflow-cache=true",
						nodeIP, interval, ofintval, nodeName),
				})
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
				config.Default.EncapPort = encapPort

				err = setupOVNNode(&node)
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
					ofintval int    = 180
				)
				node := kapi.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: kapi.NodeStatus{
						Addresses: []kapi.NodeAddress{
							{
								Type:    kapi.NodeExternalIP,
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
						"external_ids:ovn-openflow-probe-interval=%d "+
						"external_ids:hostname=\"%s\" "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-enable-lflow-cache=false "+
						"external_ids:ovn-limit-lflow-cache=1000 "+
						"external_ids:ovn-limit-lflow-cache-kb=100000",
						nodeIP, interval, ofintval, nodeName),
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
				err = setupOVNNode(&node)
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

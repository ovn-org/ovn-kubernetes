package node

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	"github.com/vishvananda/netlink"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var primaryLinkName = "primary"
var primaryIP = "192.168.126.11"
var gatewayIP = "100.64.0.2"

func mockAddPrimaryIP(link netlink.Link) {
	nodePrimaryAddrCIDR := "192.168.126.11/24"
	nodeAddr, err := netlink.ParseAddr(nodePrimaryAddrCIDR)
	Expect(err).NotTo(HaveOccurred())
	err = netlink.AddrAdd(link, nodeAddr)
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("EgressIP Operations", func() {

	var (
		app         *cli.App
		fakeOvnNode *FakeOVNNode
		fExec       *ovntest.FakeExec
	)

	BeforeEach(func() {
		config.PrepareTestConfig()

		ovntest.AddLink(primaryLinkName)

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fExec = ovntest.NewFakeExec()
		fakeOvnNode = NewFakeOVNNode(fExec)
	})

	AfterEach(func() {
		ovntest.DelLink(primaryLinkName)
	})

	Context("in local gateway mode on startup in IPv4", func() {

		It("deletes stale egress IPs on current node", func() {
			app.Action = func(ctx *cli.Context) error {

				leftOverEgressIP1 := "192.168.126.102"
				leftOverEgressIP2 := "192.168.126.111"
				leftOverEgressIP3 := "192.168.126.101"

				eIP := egressipv1.EgressIP{
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: leftOverEgressIP3,
								Node:     fakeNodeName,
							},
						},
					},
				}

				fakeOvnNode.start(ctx,
					&egressipv1.EgressIPList{
						Items: []egressipv1.EgressIP{eIP},
					})

				egressIPLocal := &egressIPLocal{
					defaultGatewayIntf: primaryLinkName,
					nodeName:           fakeNodeName,
					gatewayIPv4:        gatewayIP,
				}

				link, err := netlink.LinkByName(primaryLinkName)
				mockAddPrimaryIP(link)

				for _, ip := range []string{leftOverEgressIP1, leftOverEgressIP2, leftOverEgressIP3} {
					ipCIDR := ip + "/32"
					e1, _ := netlink.ParseAddr(ipCIDR)
					e1.Label = primaryLinkName + egressLabel
					err = netlink.AddrAdd(link, e1)
					Expect(err).NotTo(HaveOccurred())
				}

				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("arping -q -A -c 1 -I %s %s", primaryLinkName, leftOverEgressIP3),
						fmt.Sprintf("arping -q -U -c 1 -I %s %s", primaryLinkName, leftOverEgressIP3),
					},
				)

				fakeOvnNode.node.watchEgressIP(egressIPLocal)

				addrs := func() []netlink.Addr {
					addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
					Expect(err).NotTo(HaveOccurred())
					return addrs
				}

				finalAddr := []string{}
				for _, addr := range addrs() {
					finalAddr = append(finalAddr, addr.IP.String())
				}
				Eventually(fakeOvnNode.fakeExec.CalledMatchesExpected, 3).Should(BeTrue(), fakeOvnNode.fakeExec.ErrorDesc)
				Eventually(addrs).Should(HaveLen(2))
				Expect(finalAddr).ToNot(ContainElement(leftOverEgressIP1))
				Expect(finalAddr).ToNot(ContainElement(leftOverEgressIP2))
				Expect(finalAddr).To(ContainElement(primaryIP))
				Expect(finalAddr).To(ContainElement(leftOverEgressIP3))
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("in local gateway mode on ADD in IPv4", func() {

		It("adds IPs with label", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.101"

				eIP := egressipv1.EgressIP{
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP,
								Node:     fakeNodeName,
							},
						},
					},
				}

				fakeOvnNode.start(ctx)

				egressIPLocal := &egressIPLocal{
					defaultGatewayIntf: primaryLinkName,
					nodeName:           fakeNodeName,
					gatewayIPv4:        gatewayIP,
				}

				link, err := netlink.LinkByName(primaryLinkName)
				mockAddPrimaryIP(link)

				fakeOvnNode.node.watchEgressIP(egressIPLocal)

				addrs := func() []netlink.Addr {
					addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
					Expect(err).NotTo(HaveOccurred())
					return addrs
				}

				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("arping -q -A -c 1 -I %s %s", primaryLinkName, egressIP),
						fmt.Sprintf("arping -q -U -c 1 -I %s %s", primaryLinkName, egressIP),
					},
				)
				_, err = fakeOvnNode.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})

				Expect(err).NotTo(HaveOccurred())
				Eventually(addrs).Should(HaveLen(2))
				Eventually(fakeOvnNode.fakeExec.CalledMatchesExpected, 3).Should(BeTrue(), fakeOvnNode.fakeExec.ErrorDesc)

				tmp := addrs()
				Expect(tmp[0].IP.String()).To(Equal(primaryIP))
				Expect(tmp[1].IP.String()).To(Equal(egressIP))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not add IP which equals node IP", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := primaryIP

				eIP := egressipv1.EgressIP{
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP,
								Node:     fakeNodeName,
							},
						},
					},
				}

				fakeOvnNode.start(ctx)

				egressIPLocal := &egressIPLocal{
					defaultGatewayIntf: primaryLinkName,
					nodeName:           fakeNodeName,
					gatewayIPv4:        gatewayIP,
				}

				link, err := netlink.LinkByName(primaryLinkName)
				mockAddPrimaryIP(link)

				fakeOvnNode.node.watchEgressIP(egressIPLocal)

				addrs := func() []netlink.Addr {
					addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
					Expect(err).NotTo(HaveOccurred())
					return addrs
				}

				_, err = fakeOvnNode.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})

				Expect(err).NotTo(HaveOccurred())
				Eventually(addrs).Should(HaveLen(1))
				Eventually(fakeOvnNode.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvnNode.fakeExec.ErrorDesc)

				tmp := addrs()
				Expect(tmp[0].IP.String()).To(Equal(primaryIP))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("in local gateway mode on DELETE in IPv4", func() {

		It("deletes IPs with label", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.101"

				eIP := egressipv1.EgressIP{
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP,
								Node:     fakeNodeName,
							},
						},
					},
				}

				fakeOvnNode.start(ctx)

				egressIPLocal := &egressIPLocal{
					defaultGatewayIntf: primaryLinkName,
					nodeName:           fakeNodeName,
					gatewayIPv4:        gatewayIP,
				}

				link, err := netlink.LinkByName(primaryLinkName)
				mockAddPrimaryIP(link)

				fakeOvnNode.node.watchEgressIP(egressIPLocal)

				addrs := func() []netlink.Addr {
					addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
					Expect(err).NotTo(HaveOccurred())
					return addrs
				}

				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("arping -q -A -c 1 -I %s %s", primaryLinkName, egressIP),
						fmt.Sprintf("arping -q -U -c 1 -I %s %s", primaryLinkName, egressIP),
					},
				)
				_, err = fakeOvnNode.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(addrs).Should(HaveLen(2))
				Eventually(fakeOvnNode.fakeExec.CalledMatchesExpected, 3).Should(BeTrue(), fakeOvnNode.fakeExec.ErrorDesc)

				tmp := addrs()
				Expect(tmp[0].IP.String()).To(Equal(primaryIP))
				Expect(tmp[1].IP.String()).To(Equal(egressIP))
				Expect(tmp[1].Label).To(Equal(primaryLinkName + egressLabel))

				err = fakeOvnNode.fakeEgressIPClient.K8sV1().EgressIPs().Delete(context.TODO(), eIP.Name, *metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(addrs).Should(HaveLen(1))

				tmp = addrs()
				Expect(tmp[0].IP.String()).To(Equal(primaryIP))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("in local gateway mode on UPDATE in IPv4", func() {

		It("adds and deletes IPs with label", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := "192.168.126.101"

				eIP := egressipv1.EgressIP{
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP,
								Node:     fakeNodeName,
							},
						},
					},
				}

				fakeOvnNode.start(ctx)

				egressIPLocal := &egressIPLocal{
					defaultGatewayIntf: primaryLinkName,
					nodeName:           fakeNodeName,
					gatewayIPv4:        gatewayIP,
				}

				link, err := netlink.LinkByName(primaryLinkName)
				mockAddPrimaryIP(link)

				fakeOvnNode.node.watchEgressIP(egressIPLocal)

				addrs := func() []netlink.Addr {
					addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
					Expect(err).NotTo(HaveOccurred())
					return addrs
				}

				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("arping -q -A -c 1 -I %s %s", primaryLinkName, egressIP),
						fmt.Sprintf("arping -q -U -c 1 -I %s %s", primaryLinkName, egressIP),
					},
				)
				_, err = fakeOvnNode.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(addrs).Should(HaveLen(2))
				Eventually(fakeOvnNode.fakeExec.CalledMatchesExpected, 3).Should(BeTrue(), fakeOvnNode.fakeExec.ErrorDesc)

				tmp := addrs()
				Expect(tmp[0].IP.String()).To(Equal(primaryIP))
				Expect(tmp[1].IP.String()).To(Equal(egressIP))
				Expect(tmp[1].Label).To(Equal(primaryLinkName + egressLabel))

				newEgressIP := "192.168.126.125"

				eIPUPdate := egressipv1.EgressIP{
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: newEgressIP,
								Node:     fakeNodeName,
							},
						},
					},
				}

				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("arping -q -A -c 1 -I %s %s", primaryLinkName, newEgressIP),
						fmt.Sprintf("arping -q -U -c 1 -I %s %s", primaryLinkName, newEgressIP),
					},
				)

				_, err = fakeOvnNode.fakeEgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), &eIPUPdate, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fakeOvnNode.fakeExec.CalledMatchesExpected, 3).Should(BeTrue(), fakeOvnNode.fakeExec.ErrorDesc)
				Eventually(addrs).Should(HaveLen(2))

				tmp = addrs()
				Expect(tmp[0].IP.String()).To(Equal(primaryIP))
				Expect(tmp[1].IP.String()).To(Equal(newEgressIP))
				Expect(tmp[1].Label).To(Equal(primaryLinkName + egressLabel))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("in local gateway mode on ADD in IPv4", func() {

		It("adds IPs with label", func() {
			app.Action = func(ctx *cli.Context) error {

				iptV4, _ := util.SetFakeIPTablesHelpers()
				egressIP := "192.168.126.101"

				eIP := egressipv1.EgressIP{
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP,
								Node:     fakeNodeName,
							},
						},
					},
				}

				fakeOvnNode.start(ctx)

				egressIPLocal := &egressIPLocal{
					defaultGatewayIntf: primaryLinkName,
					nodeName:           fakeNodeName,
					gatewayIPv4:        gatewayIP,
				}

				link, err := netlink.LinkByName(primaryLinkName)
				mockAddPrimaryIP(link)

				fakeOvnNode.node.watchEgressIP(egressIPLocal)

				addrs := func() []netlink.Addr {
					addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
					Expect(err).NotTo(HaveOccurred())
					return addrs
				}

				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("arping -q -A -c 1 -I %s %s", primaryLinkName, egressIP),
						fmt.Sprintf("arping -q -U -c 1 -I %s %s", primaryLinkName, egressIP),
					},
				)
				_, err = fakeOvnNode.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})

				Expect(err).NotTo(HaveOccurred())
				Eventually(addrs).Should(HaveLen(2))
				Eventually(fakeOvnNode.fakeExec.CalledMatchesExpected, 3).Should(BeTrue(), fakeOvnNode.fakeExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EGRESSIP": []string{
							fmt.Sprintf("-s %s -m mark --mark %s -j SNAT --to-source %s", gatewayIP, fmt.Sprintf("0x%x", util.IPToUint32(egressIP)), egressIP),
						},
					},
					"filter": {
						"OVN-KUBE-EGRESSIP": []string{
							fmt.Sprintf("-d %s -m conntrack --ctstate NEW -j REJECT", egressIP),
						},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				tmp := addrs()
				Expect(tmp[0].IP.String()).To(Equal(primaryIP))
				Expect(tmp[1].IP.String()).To(Equal(egressIP))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("in local gateway mode on DELETE in IPv4", func() {

		It("deletes IPs with label", func() {
			app.Action = func(ctx *cli.Context) error {

				iptV4, _ := util.SetFakeIPTablesHelpers()
				egressIP := "192.168.126.101"

				eIP := egressipv1.EgressIP{
					Status: egressipv1.EgressIPStatus{
						Items: []egressipv1.EgressIPStatusItem{
							{
								EgressIP: egressIP,
								Node:     fakeNodeName,
							},
						},
					},
				}

				fakeOvnNode.start(ctx)

				egressIPLocal := &egressIPLocal{
					defaultGatewayIntf: primaryLinkName,
					nodeName:           fakeNodeName,
					gatewayIPv4:        gatewayIP,
				}

				link, err := netlink.LinkByName(primaryLinkName)
				mockAddPrimaryIP(link)

				fakeOvnNode.node.watchEgressIP(egressIPLocal)

				addrs := func() []netlink.Addr {
					addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
					Expect(err).NotTo(HaveOccurred())
					return addrs
				}

				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("arping -q -A -c 1 -I %s %s", primaryLinkName, egressIP),
						fmt.Sprintf("arping -q -U -c 1 -I %s %s", primaryLinkName, egressIP),
					},
				)
				_, err = fakeOvnNode.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})

				Expect(err).NotTo(HaveOccurred())
				Eventually(addrs).Should(HaveLen(2))
				Eventually(fakeOvnNode.fakeExec.CalledMatchesExpected, 3).Should(BeTrue(), fakeOvnNode.fakeExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EGRESSIP": []string{
							fmt.Sprintf("-s %s -m mark --mark %s -j SNAT --to-source %s", gatewayIP, fmt.Sprintf("0x%x", util.IPToUint32(egressIP)), egressIP),
						},
					},
					"filter": {
						"OVN-KUBE-EGRESSIP": []string{
							fmt.Sprintf("-d %s -m conntrack --ctstate NEW -j REJECT", egressIP),
						},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				tmp := addrs()
				Expect(tmp[0].IP.String()).To(Equal(primaryIP))
				Expect(tmp[1].IP.String()).To(Equal(egressIP))

				err = fakeOvnNode.fakeEgressIPClient.K8sV1().EgressIPs().Delete(context.TODO(), eIP.Name, *metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(addrs).Should(HaveLen(1))

				tmp = addrs()
				Expect(tmp[0].IP.String()).To(Equal(primaryIP))

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EGRESSIP": []string{},
					},
					"filter": {
						"OVN-KUBE-EGRESSIP": []string{},
					},
				}

				f4 = iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

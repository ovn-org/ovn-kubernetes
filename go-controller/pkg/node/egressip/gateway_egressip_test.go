// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	netlink_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
)

var _ = ginkgo.Describe("Gateway EgressIP", func() {

	const (
		nodeName        = "ovn-worker"
		bridgeName      = "breth0"
		bridgeLinkIndex = 10
		ipV4Addr        = "192.168.1.5"
		ipV4Addr2       = "192.168.1.6"
		ipV4Addr3       = "192.168.1.7"
		mark            = "50000"
		mark2           = "50001"
		mark3           = "50002"
		emptyAnnotation = ""
	)

	var (
		nlMock     *mocks.NetLinkOps
		nlLinkMock *netlink_mocks.Link
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		err := config.PrepareTestConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nlMock = new(mocks.NetLinkOps)
		nlLinkMock = new(netlink_mocks.Link)
		util.SetNetLinkOpMockInst(nlMock)
	})

	ginkgo.AfterEach(func() {
		util.ResetNetLinkOpMockInst()
	})

	ginkgo.Context("add EgressIP", func() {
		ginkgo.It("configures annotation and bridge when EIP assigned to node", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			eip := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			isUpdated, err := addrMgr.AddEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			gomega.Eventually(func() []string {
				node, err := addrMgr.nodeLister.Get(nodeName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
				return parseEIPsFromAnnotation(node)
			}).Should(gomega.ConsistOf(ipV4Addr))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("doesn't configure or fail when annotation mark isn't found", func() {
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			eip := getEIPAssignedToNode(nodeName, "", ipV4Addr)
			isUpdated, err := addrMgr.AddEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeFalse())
			node, err := addrMgr.nodeLister.Get(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).ShouldNot(gomega.ConsistOf(ipV4Addr))
			gomega.Expect(nlMock.AssertNotCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("fails when invalid annotation mark", func() {
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			eip := getEIPAssignedToNode(nodeName, "not-an-integer", ipV4Addr)
			isUpdated, err := addrMgr.AddEgressIP(eip)
			gomega.Expect(err).Should(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeFalse())
			node, err := addrMgr.nodeLister.Get(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).ShouldNot(gomega.ConsistOf(ipV4Addr))
			gomega.Expect(nlMock.AssertNotCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("configures annotations with existing entries", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, generateAnnotFromIPs(ipV4Addr2))
			defer stopFn()
			eip := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			isUpdated, err := addrMgr.AddEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			gomega.Eventually(func() []string {
				node, err := addrMgr.nodeLister.Get(nodeName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
				return parseEIPsFromAnnotation(node)
			}).Should(gomega.ConsistOf(ipV4Addr, ipV4Addr2))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("doesn't configure when EgressIP is on a secondary host network", func() {
			// Setup a node with host-cidrs annotation containing a secondary network subnet
			secondarySubnet := "10.10.10.0/24"
			secondaryIP := "10.10.10.5"

			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(secondaryIP), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManagerWithHostCIDRs(nodeName, bridgeName, emptyAnnotation, []string{secondarySubnet})
			defer stopFn()
			eip := getEIPAssignedToNode(nodeName, mark, secondaryIP)
			isUpdated, err := addrMgr.AddEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should not error for secondary network IP")
			gomega.Expect(isUpdated).Should(gomega.BeFalse(), "should not update for secondary network IP")
			node, err := addrMgr.nodeLister.Get(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).ShouldNot(gomega.ConsistOf(secondaryIP), "secondary IP should not be in annotation")
			gomega.Expect(nlMock.AssertNotCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(secondaryIP), bridgeLinkIndex))).Should(gomega.BeTrue(), "should not add IP to bridge")
		})
	})

	ginkgo.Context("update EgressIP", func() {
		ginkgo.It("configures when EgressIP is not assigned to the node", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			assignedEIP := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			unassignedEIP := getEIPNotAssignedToNode(mark, ipV4Addr)
			isUpdated, err := addrMgr.UpdateEgressIP(unassignedEIP, assignedEIP)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			gomega.Eventually(func() []string {
				node, err := addrMgr.nodeLister.Get(nodeName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
				return parseEIPsFromAnnotation(node)
			}).Should(gomega.ConsistOf(ipV4Addr))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("removes EgressIP previously assigned", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			assignedEIP := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			unassignedEIP := getEIPNotAssignedToNode(mark, ipV4Addr)
			isUpdated, err := addrMgr.UpdateEgressIP(unassignedEIP, assignedEIP)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			isUpdated, err = addrMgr.UpdateEgressIP(assignedEIP, unassignedEIP)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			gomega.Eventually(func() []string {
				node, err := addrMgr.nodeLister.Get(nodeName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
				return parseEIPsFromAnnotation(node)
			}).ShouldNot(gomega.ConsistOf(ipV4Addr))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("reconfigures from an old to a new IP", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr2), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			unassignedEIP := getEIPNotAssignedToNode(mark, ipV4Addr)
			assignedEIP1 := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			assignedEIP2 := getEIPAssignedToNode(nodeName, mark2, ipV4Addr2)
			isUpdated, err := addrMgr.UpdateEgressIP(unassignedEIP, assignedEIP1)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			isUpdated, err = addrMgr.UpdateEgressIP(assignedEIP1, assignedEIP2)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			gomega.Eventually(func() []string {
				node, err := addrMgr.nodeLister.Get(nodeName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
				return parseEIPsFromAnnotation(node)
			}).Should(gomega.ConsistOf(ipV4Addr2))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr2), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})
	})

	ginkgo.Context("delete EgressIP", func() {
		ginkgo.It("removes configuration from annotation and bridge when EIP assigned to node is deleted", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			eip := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			isUpdated, err := addrMgr.AddEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			isUpdated, err = addrMgr.DeleteEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			gomega.Eventually(func() []string {
				node, err := addrMgr.nodeLister.Get(nodeName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
				return parseEIPsFromAnnotation(node)
			}).ShouldNot(gomega.ConsistOf(ipV4Addr))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("does not update when EIP is deleted that wasn't assigned to the node", func() {
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, generateAnnotFromIPs(ipV4Addr2))
			defer stopFn()
			eip := getEIPNotAssignedToNode(mark, ipV4Addr)
			isUpdated, err := addrMgr.DeleteEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeFalse())
			node, err := addrMgr.nodeLister.Get(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).Should(gomega.ConsistOf(ipV4Addr2))
			gomega.Expect(nlMock.AssertNotCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})
	})

	ginkgo.Context("sync EgressIP", func() {
		ginkgo.It("configures multiple EgressIPs assigned to the node", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr2), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			eipAssigned1 := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			eipAssigned2 := getEIPAssignedToNode(nodeName, mark2, ipV4Addr2)
			eipUnassigned3 := getEIPNotAssignedToNode(mark3, ipV4Addr3)
			err := addrMgr.SyncEgressIP([]interface{}{eipAssigned1, eipAssigned2, eipUnassigned3})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process valid EgressIPs")
			gomega.Eventually(func() []string {
				node, err := addrMgr.nodeLister.Get(nodeName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
				return parseEIPsFromAnnotation(node)
			}).Should(gomega.ConsistOf(ipV4Addr, ipV4Addr2))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr2), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("delete previous configuration", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr2), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr3), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, generateAnnotFromIPs(ipV4Addr3)) // previously configured IP
			defer stopFn()
			eipAssigned1 := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			eipAssigned2 := getEIPAssignedToNode(nodeName, mark2, ipV4Addr2)
			err := addrMgr.SyncEgressIP([]interface{}{eipAssigned1, eipAssigned2})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process valid EgressIPs")
			gomega.Eventually(func() []string {
				node, err := addrMgr.nodeLister.Get(nodeName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
				return parseEIPsFromAnnotation(node)
			}).Should(gomega.ConsistOf(ipV4Addr, ipV4Addr2))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr2), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr3), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("no update or failure when mark is not set", func() {
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation) // previously configured IP
			defer stopFn()
			eipAssigned := getEIPAssignedToNode(nodeName, "", ipV4Addr)
			err := addrMgr.SyncEgressIP([]interface{}{eipAssigned})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process valid EgressIPs")
			node, err := addrMgr.nodeLister.Get(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).Should(gomega.BeEmpty())
		})

		ginkgo.It("cleans up mistakenly configured secondary network EgressIP", func() {
			// Setup: secondary network IP that was mistakenly configured by old buggy code
			secondaryIP := "10.10.10.5"
			secondarySubnet := "10.10.10.0/24"

			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(secondaryIP), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrAdd", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)

			// Initialize with host-cidrs that includes the secondary network and mistakenly configured secondary IP
			addrMgr, stopFn := initBridgeEIPAddrManagerWithHostCIDRs(nodeName, bridgeName, generateAnnotFromIPs(secondaryIP), []string{secondarySubnet})
			defer stopFn()

			// Simulate mistaken configuration by old buggy code: IP is in cache
			secondaryEIP := getEIPAssignedToNode(nodeName, mark, secondaryIP)
			pktMark, err := util.ParseEgressIPMark(secondaryEIP.Annotations)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			addrMgr.cache.insertMarkIP(pktMark, net.ParseIP(secondaryIP))

			// Verify mistaken state exists
			gomega.Expect(addrMgr.cache.IsIPPresent(net.ParseIP(secondaryIP))).Should(gomega.BeTrue(), "IP should be in cache (mistaken state)")
			node, err := addrMgr.nodeLister.Get(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			gomega.Expect(parseEIPsFromAnnotation(node)).Should(gomega.ConsistOf(secondaryIP), "IP should be in annotation (mistaken state)")

			// Sync with a valid OVN network EgressIP - should clean up the secondary IP and add the new one
			validEIP := getEIPAssignedToNode(nodeName, mark2, ipV4Addr)
			err = addrMgr.SyncEgressIP([]interface{}{validEIP})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should sync and clean up mistaken secondary network EgressIP")

			// Verify cleanup: secondary IP removed from cache, annotation, and bridge
			gomega.Expect(addrMgr.cache.IsIPPresent(net.ParseIP(secondaryIP))).Should(gomega.BeFalse(), "secondary IP should be removed from cache")
			gomega.Eventually(func() []string {
				node, err := addrMgr.nodeLister.Get(nodeName)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				return parseEIPsFromAnnotation(node)
			}).Should(gomega.ConsistOf(ipV4Addr), "only valid OVN IP should be in annotation")
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(secondaryIP), bridgeLinkIndex))).Should(gomega.BeTrue(), "should delete secondary IP from bridge")
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue(), "should add valid OVN IP to bridge")
		})
	})

	ginkgo.Context("egress-assignable label removal", func() {
		ginkgo.It("should preemptively remove EgressIPs from bridge when label is removed", func() {
			// Setup: Create node with label and EgressIP configured
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						util.GetNodeEgressLabel(): "",
					},
					Annotations: map[string]string{
						util.OVNNodeBridgeEgressIPs: toJSONArrayOfStrings(ipV4Addr, ipV4Addr2),
						util.OvnNodeIfAddr:          `{"ipv4":"192.168.1.10/24"}`,
					},
				},
			}
			client := fake.NewSimpleClientset(node)
			watchFactory, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{KubeClient: client}, nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			gomega.Expect(watchFactory.Start()).Should(gomega.Succeed())
			defer watchFactory.Shutdown()

			linkManager := linkmanager.NewController(nodeName, true, true, nil)

			// Setup netlink mocks
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex})
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)

			// Expect AddrDel to be called for both IPs
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil).Once()
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr2), bridgeLinkIndex)).Return(nil).Once()

			addrMgr := NewBridgeEIPAddrManager(nodeName, bridgeName, linkManager, &kube.Kube{KClient: client},
				watchFactory.EgressIPInformer(), watchFactory.NodeCoreInformer())

			// Add IPs to cache as if they were assigned
			addrMgr.cache.insertMarkIP(util.EgressIPMark(50000), net.ParseIP(ipV4Addr))
			addrMgr.cache.insertMarkIP(util.EgressIPMark(50001), net.ParseIP(ipV4Addr2))

			// Simulate label removal by updating the node
			updatedNode := node.DeepCopy()
			delete(updatedNode.Labels, util.GetNodeEgressLabel())
			_, err = client.CoreV1().Nodes().Update(nil, updatedNode, metav1.UpdateOptions{})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			// Wait for the event handler to process
			gomega.Eventually(func() bool {
				return nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
					egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr), bridgeLinkIndex))
			}, "2s").Should(gomega.BeTrue(), "should remove IPv4 address from bridge")

			gomega.Eventually(func() bool {
				return nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
					egressip.GetNetlinkAddress(net.ParseIP(ipV4Addr2), bridgeLinkIndex))
			}, "2s").Should(gomega.BeTrue(), "should remove second IPv4 address from bridge")
		})

		ginkgo.It("should not remove IPs when label is added", func() {
			// Setup: Node without label
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						util.OvnNodeIfAddr: `{"ipv4":"192.168.1.10/24"}`,
					},
				},
			}
			client := fake.NewSimpleClientset(node)
			watchFactory, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{KubeClient: client}, nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			gomega.Expect(watchFactory.Start()).Should(gomega.Succeed())
			defer watchFactory.Shutdown()

			linkManager := linkmanager.NewController(nodeName, true, true, nil)

			// Setup netlink mocks - AddrDel should NOT be called
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex})
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)

			addrMgr := NewBridgeEIPAddrManager(nodeName, bridgeName, linkManager, &kube.Kube{KClient: client},
				watchFactory.EgressIPInformer(), watchFactory.NodeCoreInformer())

			addrMgr.cache.insertMarkIP(util.EgressIPMark(50000), net.ParseIP(ipV4Addr))

			// Simulate label addition
			updatedNode := node.DeepCopy()
			updatedNode.Labels = map[string]string{util.GetNodeEgressLabel(): ""}
			_, err = client.CoreV1().Nodes().Update(nil, updatedNode, metav1.UpdateOptions{})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			// AddrDel should NOT be called
			gomega.Consistently(func() bool {
				return nlMock.AssertNotCalled(ginkgo.GinkgoT(), "AddrDel")
			}, "1s").Should(gomega.BeTrue(), "should not remove IPs when label is added")
		})

		ginkgo.It("should not remove IPs for different node", func() {
			// Setup: Create two nodes
			node1 := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						util.GetNodeEgressLabel(): "",
					},
					Annotations: map[string]string{
						util.OvnNodeIfAddr: `{"ipv4":"192.168.1.10/24"}`,
					},
				},
			}
			node2 := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "ovn-worker2",
					Labels: map[string]string{
						util.GetNodeEgressLabel(): "",
					},
					Annotations: map[string]string{
						util.OvnNodeIfAddr: `{"ipv4":"192.168.1.11/24"}`,
					},
				},
			}
			client := fake.NewSimpleClientset(node1, node2)
			watchFactory, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{KubeClient: client}, nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			gomega.Expect(watchFactory.Start()).Should(gomega.Succeed())
			defer watchFactory.Shutdown()

			linkManager := linkmanager.NewController(nodeName, true, true, nil)

			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex})
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)

			addrMgr := NewBridgeEIPAddrManager(nodeName, bridgeName, linkManager, &kube.Kube{KClient: client},
				watchFactory.EgressIPInformer(), watchFactory.NodeCoreInformer())

			// Remove label from node2 (different node)
			updatedNode2 := node2.DeepCopy()
			delete(updatedNode2.Labels, util.GetNodeEgressLabel())
			_, err = client.CoreV1().Nodes().Update(nil, updatedNode2, metav1.UpdateOptions{})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			// AddrDel should NOT be called for our node
			gomega.Consistently(func() bool {
				return nlMock.AssertNotCalled(ginkgo.GinkgoT(), "AddrDel")
			}, "1s").Should(gomega.BeTrue(), "should not remove IPs when different node's label is removed")
		})

		ginkgo.It("should handle IPv6 EgressIP removal", func() {
			ipV6Addr := "fd00::1"
			ipV6Addr2 := "fd00::2"

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						util.GetNodeEgressLabel(): "",
					},
					Annotations: map[string]string{
						util.OVNNodeBridgeEgressIPs: toJSONArrayOfStrings(ipV6Addr, ipV6Addr2),
						util.OvnNodeIfAddr:          `{"ipv6":"fd00::10/64"}`,
					},
				},
			}
			client := fake.NewSimpleClientset(node)
			watchFactory, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{KubeClient: client}, nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			gomega.Expect(watchFactory.Start()).Should(gomega.Succeed())
			defer watchFactory.Shutdown()

			linkManager := linkmanager.NewController(nodeName, true, true, nil)

			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex})
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV6Addr), bridgeLinkIndex)).Return(nil).Once()
			nlMock.On("AddrDel", nlLinkMock, egressip.GetNetlinkAddress(net.ParseIP(ipV6Addr2), bridgeLinkIndex)).Return(nil).Once()

			addrMgr := NewBridgeEIPAddrManager(nodeName, bridgeName, linkManager, &kube.Kube{KClient: client},
				watchFactory.EgressIPInformer(), watchFactory.NodeCoreInformer())

			addrMgr.cache.insertMarkIP(util.EgressIPMark(60000), net.ParseIP(ipV6Addr))
			addrMgr.cache.insertMarkIP(util.EgressIPMark(60001), net.ParseIP(ipV6Addr2))

			// Remove label
			updatedNode := node.DeepCopy()
			delete(updatedNode.Labels, util.GetNodeEgressLabel())
			_, err = client.CoreV1().Nodes().Update(nil, updatedNode, metav1.UpdateOptions{})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			gomega.Eventually(func() bool {
				return nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
					egressip.GetNetlinkAddress(net.ParseIP(ipV6Addr), bridgeLinkIndex))
			}, "2s").Should(gomega.BeTrue(), "should remove first IPv6 address from bridge")

			gomega.Eventually(func() bool {
				return nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
					egressip.GetNetlinkAddress(net.ParseIP(ipV6Addr2), bridgeLinkIndex))
			}, "2s").Should(gomega.BeTrue(), "should remove second IPv6 address from bridge")
		})
	})
})

func initBridgeEIPAddrManager(nodeName, bridgeName string, bridgeEIPAnnot string) (*BridgeEIPAddrManager, func()) {
	return initBridgeEIPAddrManagerWithHostCIDRs(nodeName, bridgeName, bridgeEIPAnnot, nil)
}

// initBridgeEIPAddrManagerWithHostCIDRs is a variant of initBridgeEIPAddrManager that sets the host-cidrs annotation
func initBridgeEIPAddrManagerWithHostCIDRs(nodeName, bridgeName string, bridgeEIPAnnot string, hostCIDRs []string) (*BridgeEIPAddrManager, func()) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, Annotations: map[string]string{}},
	}
	if bridgeEIPAnnot != "" {
		node.Annotations[util.OVNNodeBridgeEgressIPs] = bridgeEIPAnnot
	}
	// Add OVN network annotation - required for isOVNNetworkIP to work
	node.Annotations[util.OvnNodeIfAddr] = `{"ipv4":"192.168.1.10/24"}`
	if len(hostCIDRs) > 0 {
		// Add OVN (primary) network to host-cidrs
		hostCIDRs = append(hostCIDRs, "192.168.1.0/24")
		cidrsJSON, err := json.Marshal(hostCIDRs)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		node.Annotations[util.OVNNodeHostCIDRs] = string(cidrsJSON)
	}
	client := fake.NewSimpleClientset(node)
	watchFactory, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{KubeClient: client}, nodeName)
	gomega.Expect(watchFactory.Start()).Should(gomega.Succeed(), "watch factory should start")
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "watch factory creation must succeed")
	linkManager := linkmanager.NewController(nodeName, true, true, nil)
	addrMgr := NewBridgeEIPAddrManager(nodeName, bridgeName, linkManager, &kube.Kube{KClient: client}, watchFactory.EgressIPInformer(), watchFactory.NodeCoreInformer())
	initialAnnotIPs, err := util.ParseNodeBridgeEgressIPsAnnotation(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			initialAnnotIPs = make([]string, 0)
		} else {
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "bridge EgressIP annotation should be parseable")
		}
	}
	addrMgr.annotationIPs = sets.New[string](initialAnnotIPs...)
	return addrMgr, watchFactory.Shutdown
}

func getEIPAssignedToNode(nodeName, mark, assignedIP string) *egressipv1.EgressIP {
	eip := &egressipv1.EgressIP{
		ObjectMeta: metav1.ObjectMeta{Name: "bridge-addr-mgr-test", Annotations: map[string]string{}},
		Spec: egressipv1.EgressIPSpec{
			EgressIPs: []string{assignedIP},
		},
		Status: egressipv1.EgressIPStatus{
			Items: []egressipv1.EgressIPStatusItem{
				{
					Node:     nodeName,
					EgressIP: assignedIP,
				},
			},
		},
	}
	if mark != "" {
		eip.Annotations[util.EgressIPMarkAnnotation] = mark
	}
	return eip
}

func getEIPNotAssignedToNode(mark, ip string) *egressipv1.EgressIP {
	eip := &egressipv1.EgressIP{
		ObjectMeta: metav1.ObjectMeta{Name: "bridge-addr-mgr-test", Annotations: map[string]string{}},
		Spec: egressipv1.EgressIPSpec{
			EgressIPs: []string{ip},
		},
		Status: egressipv1.EgressIPStatus{
			Items: []egressipv1.EgressIPStatusItem{
				{
					Node:     "different-node",
					EgressIP: ip,
				},
			},
		},
	}
	if mark != "" {
		eip.Annotations[util.EgressIPMarkAnnotation] = mark
	}
	return eip
}

func generateAnnotFromIPs(ips ...string) string {
	ipsWithQuotes := make([]string, 0, len(ips))
	for _, ip := range ips {
		if ip == "" {
			continue
		}
		if net.ParseIP(ip) == nil {
			panic("invalid IP")
		}
		ipsWithQuotes = append(ipsWithQuotes, fmt.Sprintf("\"%s\"", ip))
	}
	return fmt.Sprintf("[%s]", strings.Join(ipsWithQuotes, ","))
}

func parseEIPsFromAnnotation(node *corev1.Node) []string {
	ips, err := util.ParseNodeBridgeEgressIPsAnnotation(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			ips = make([]string, 0)
		} else {
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should be able to detect if annotation is or not")
		}
	}
	return ips
}


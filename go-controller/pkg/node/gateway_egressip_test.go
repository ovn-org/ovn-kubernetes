package node

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	netlink_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			eip := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			isUpdated, err := addrMgr.addEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			node, err := addrMgr.kube.GetNode(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).Should(gomega.ConsistOf(ipV4Addr))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("doesn't configure or fail when annotation mark isn't found", func() {
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			eip := getEIPAssignedToNode(nodeName, "", ipV4Addr)
			isUpdated, err := addrMgr.addEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeFalse())
			node, err := addrMgr.kube.GetNode(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).ShouldNot(gomega.ConsistOf(ipV4Addr))
			gomega.Expect(nlMock.AssertNotCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("fails when invalid annotation mark", func() {
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			eip := getEIPAssignedToNode(nodeName, "not-an-integer", ipV4Addr)
			isUpdated, err := addrMgr.addEgressIP(eip)
			gomega.Expect(err).Should(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeFalse())
			node, err := addrMgr.kube.GetNode(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).ShouldNot(gomega.ConsistOf(ipV4Addr))
			gomega.Expect(nlMock.AssertNotCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("configures annotations with existing entries", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, generateAnnotFromIPs(ipV4Addr2))
			defer stopFn()
			eip := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			isUpdated, err := addrMgr.addEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			node, err := addrMgr.kube.GetNode(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).Should(gomega.ConsistOf(ipV4Addr, ipV4Addr2))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})
	})

	ginkgo.Context("update EgressIP", func() {
		ginkgo.It("configures when EgressIP is not assigned to the node", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			assignedEIP := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			unassignedEIP := getEIPNotAssignedToNode(mark, ipV4Addr)
			isUpdated, err := addrMgr.updateEgressIP(unassignedEIP, assignedEIP)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			node, err := addrMgr.kube.GetNode(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).Should(gomega.ConsistOf(ipV4Addr))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("removes EgressIP previously assigned", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrDel", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			assignedEIP := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			unassignedEIP := getEIPNotAssignedToNode(mark, ipV4Addr)
			isUpdated, err := addrMgr.updateEgressIP(unassignedEIP, assignedEIP)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			isUpdated, err = addrMgr.updateEgressIP(assignedEIP, unassignedEIP)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			node, err := addrMgr.kube.GetNode(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).ShouldNot(gomega.ConsistOf(ipV4Addr))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("reconfigures from an old to a new IP", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr2), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrDel", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			unassignedEIP := getEIPNotAssignedToNode(mark, ipV4Addr)
			assignedEIP1 := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			assignedEIP2 := getEIPAssignedToNode(nodeName, mark2, ipV4Addr2)
			isUpdated, err := addrMgr.updateEgressIP(unassignedEIP, assignedEIP1)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			isUpdated, err = addrMgr.updateEgressIP(assignedEIP1, assignedEIP2)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			node, err := addrMgr.kube.GetNode(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).Should(gomega.ConsistOf(ipV4Addr2))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr2), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})
	})

	ginkgo.Context("delete EgressIP", func() {
		ginkgo.It("removes configuration from annotation and bridge when EIP assigned to node is deleted", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrDel", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			eip := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			isUpdated, err := addrMgr.addEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			isUpdated, err = addrMgr.deleteEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeTrue())
			node, err := addrMgr.kube.GetNode(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).ShouldNot(gomega.ConsistOf(ipV4Addr))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("does not update when EIP is deleted that wasn't assigned to the node", func() {
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, generateAnnotFromIPs(ipV4Addr2))
			defer stopFn()
			eip := getEIPNotAssignedToNode(mark, ipV4Addr)
			isUpdated, err := addrMgr.deleteEgressIP(eip)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process a valid EgressIP")
			gomega.Expect(isUpdated).Should(gomega.BeFalse())
			node, err := addrMgr.kube.GetNode(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).Should(gomega.ConsistOf(ipV4Addr2))
			gomega.Expect(nlMock.AssertNotCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
		})
	})

	ginkgo.Context("sync EgressIP", func() {
		ginkgo.It("configures multiple EgressIPs assigned to the node", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr2), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation)
			defer stopFn()
			eipAssigned1 := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			eipAssigned2 := getEIPAssignedToNode(nodeName, mark2, ipV4Addr2)
			eipUnassigned3 := getEIPNotAssignedToNode(mark3, ipV4Addr3)
			err := addrMgr.syncEgressIP([]interface{}{eipAssigned1, eipAssigned2, eipUnassigned3})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process valid EgressIPs")
			node, err := addrMgr.kube.GetNode(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).Should(gomega.ConsistOf(ipV4Addr, ipV4Addr2))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr2), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("delete previous configuration", func() {
			nlLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: bridgeName, Index: bridgeLinkIndex}, nil)
			nlMock.On("LinkByName", bridgeName).Return(nlLinkMock, nil)
			nlMock.On("LinkByIndex", bridgeLinkIndex).Return(nlLinkMock, nil)
			nlMock.On("LinkList").Return([]netlink.Link{nlLinkMock}, nil)
			nlMock.On("AddrList", nlLinkMock, 0).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrAdd", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr2), bridgeLinkIndex)).Return(nil)
			nlMock.On("AddrDel", nlLinkMock, getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr3), bridgeLinkIndex)).Return(nil)
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, generateAnnotFromIPs(ipV4Addr3)) // previously configured IP
			defer stopFn()
			eipAssigned1 := getEIPAssignedToNode(nodeName, mark, ipV4Addr)
			eipAssigned2 := getEIPAssignedToNode(nodeName, mark2, ipV4Addr2)
			err := addrMgr.syncEgressIP([]interface{}{eipAssigned1, eipAssigned2})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process valid EgressIPs")
			node, err := addrMgr.kube.GetNode(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(parseEIPsFromAnnotation(node)).Should(gomega.ConsistOf(ipV4Addr, ipV4Addr2))
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr2), bridgeLinkIndex))).Should(gomega.BeTrue())
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLinkMock,
				getEIPBridgeNetlinkAddressPtr(net.ParseIP(ipV4Addr3), bridgeLinkIndex))).Should(gomega.BeTrue())
		})

		ginkgo.It("no update or failure when mark is not set", func() {
			addrMgr, stopFn := initBridgeEIPAddrManager(nodeName, bridgeName, emptyAnnotation) // previously configured IP
			defer stopFn()
			eipAssigned := getEIPAssignedToNode(nodeName, "", ipV4Addr)
			err := addrMgr.syncEgressIP([]interface{}{eipAssigned})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "should process valid EgressIPs")
			node, err := addrMgr.kube.GetNode(nodeName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "node should be present within kapi")
			gomega.Expect(len(parseEIPsFromAnnotation(node))).Should(gomega.BeZero())
		})
	})
})

func initBridgeEIPAddrManager(nodeName, bridgeName string, bridgeEIPAnnot string) (*bridgeEIPAddrManager, func()) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, Annotations: map[string]string{}},
	}
	if bridgeEIPAnnot != "" {
		node.Annotations[util.OVNNodeBridgeEgressIPs] = bridgeEIPAnnot
	}
	client := fake.NewSimpleClientset(node)
	watchFactory, err := factory.NewNodeWatchFactory(&util.OVNNodeClientset{KubeClient: client}, nodeName)
	gomega.Expect(watchFactory.Start()).Should(gomega.Succeed(), "watch factory should start")
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "watch factory creation must succeed")
	linkManager := linkmanager.NewController(nodeName, true, true, nil)
	return newBridgeEIPAddrManager(nodeName, bridgeName, linkManager, &kube.Kube{KClient: client}, watchFactory.EgressIPInformer(), watchFactory.NodeCoreInformer()),
		watchFactory.Shutdown
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

func getEIPBridgeNetlinkAddressPtr(ip net.IP, ifindex int) *netlink.Addr {
	addr := getEIPBridgeNetlinkAddress(ip, ifindex)
	return &addr
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

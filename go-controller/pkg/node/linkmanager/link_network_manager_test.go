package linkmanager

import (
	"fmt"

	netlink_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

	"github.com/onsi/ginkgo/v2"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
)

var _ = ginkgo.Describe("Link network manager", func() {
	const (
		v4CIDR1   = "10.10.10.4/24"
		v4CIDR2   = "10.10.10.5/24"
		linkName1 = "link1"
		linkName2 = "link2"
		// following consts are to improve readability for table test code args
		noErr            = false
		Err              = true
		v4Enabled        = true
		v4Disable        = false
		v6Enabled        = true
		v6Disabled       = false
		addrAddCalled    = true
		addrAddNotCalled = false
		addrDelCalled    = true
		addrDelNotCalled = false
	)

	var (
		nlMock      *mocks.NetLinkOps
		nlLink1Mock *netlink_mocks.Link
		nlLink2Mock *netlink_mocks.Link
		c           *Controller
	)

	linkNameIndexes := map[string]int{
		linkName1: 1,
		linkName2: 2,
	}

	getLinkIndexFromName := func(linkName string) int {
		linkIndex, ok := linkNameIndexes[linkName]
		if !ok {
			panic(fmt.Sprintf("failed to find index for link name %q", linkName))
		}
		return linkIndex
	}

	newNetlinkAddrWithIndexSet := func(cidr, linkName string) netlink.Addr {
		addr := newNetlinkAddr(cidr)
		addr.LinkIndex = getLinkIndexFromName(linkName)
		return addr
	}

	// filter addresses for a link based on link index
	getLinkAddrs := func(addrs []netlink.Addr, linkName string) []netlink.Addr {
		wantedLinkIndex := getLinkIndexFromName(linkName)
		wantedLinkAddrs := make([]netlink.Addr, 0)
		for _, addr := range addrs {
			if addr.LinkIndex == wantedLinkIndex {
				wantedLinkAddrs = append(wantedLinkAddrs, addr)
			}
		}
		return wantedLinkAddrs
	}

	ginkgo.BeforeEach(func() {
		nlMock = &mocks.NetLinkOps{}
		nlLink1Mock = new(netlink_mocks.Link)
		nlLink2Mock = new(netlink_mocks.Link)
		util.SetNetLinkOpMockInst(nlMock)
	})

	ginkgo.AfterEach(func() {
		util.ResetNetLinkOpMockInst()
	})

	ginkgo.It("returns error when address is added but link doesn't exist", func() {
		linkAddr := newNetlinkAddr(v4CIDR2)
		nlLink1Mock.On("Attrs").Return(&netlink.LinkAttrs{Name: linkName1, Index: getLinkIndexFromName(linkName1)}, nil)
		nlMock.On("LinkByIndex").Return(nil, netlink.LinkNotFoundError{})
		c = NewController("test", v4Enabled, v6Enabled, nil)
		gomega.Expect(c.AddAddress(linkAddr)).Should(gomega.HaveOccurred())
	})

	ginkgo.It("doesnt return error when attempting to delete address but link deleted", func() {
		linkAddr := newNetlinkAddr(v4CIDR2)
		linkAddr.LinkIndex = getLinkIndexFromName(linkName1)
		nlLink1Mock.On("Attrs").Return(&netlink.LinkAttrs{Name: linkName1, Index: getLinkIndexFromName(linkName1)}, nil)
		nlMock.On("LinkList").Return([]netlink.Link{nlLink1Mock}, nil)
		nlMock.On("LinkByIndex", getLinkIndexFromName(linkName1)).Return(nlLink1Mock, nil)
		nlMock.On("AddrList", nlLink1Mock, getIPFamilyInt(v4Enabled, v6Enabled)).Return([]netlink.Addr{}, nil)
		nlMock.On("AddrAdd", nlLink1Mock, &linkAddr).Return(nil)
		c = NewController("test", v4Enabled, v6Enabled, nil)
		gomega.Expect(c.AddAddress(linkAddr)).Should(gomega.Succeed())
		nlMock.Mock.ExpectedCalls = nil
		nlMock.On("LinkByIndex", getLinkIndexFromName(linkName1)).Return(nil, netlink.LinkNotFoundError{})
		nlMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
		gomega.Expect(c.DelAddress(linkAddr)).Should(gomega.Succeed())
	})

	// Test that:
	// 1. Addition of address to store
	// 2. Expected address applied func (AddrAdd) is called
	//
	// There maybe a discrepancy between existingLinkAddr link addresses and existingStore link addresses because a link may
	// have addresses that aren't managed. Link1 is always the target of the new addresses to add.
	ginkgo.DescribeTable("Add address to link1", func(addrToAdd netlink.Addr, existingLinkAddr []netlink.Addr, existingStore map[string][]netlink.Addr,
		v4Enabled, v6Enabled, expectErr, expectAddAddrCalled bool) {

		expectedAddr := addrToAdd
		nlLink1Mock.On("Attrs").Return(&netlink.LinkAttrs{Name: linkName1, Index: getLinkIndexFromName(linkName1)}, nil)
		nlLink2Mock.On("Attrs").Return(&netlink.LinkAttrs{Name: linkName2, Index: getLinkIndexFromName(linkName2)}, nil)
		nlMock.On("LinkList").Return([]netlink.Link{nlLink1Mock, nlLink2Mock}, nil)
		nlMock.On("LinkByIndex", getLinkIndexFromName(linkName1)).Return(nlLink1Mock, nil)
		nlMock.On("LinkByIndex", getLinkIndexFromName(linkName2)).Return(nlLink2Mock, nil)
		nlMock.On("AddrList", nlLink1Mock, getIPFamilyInt(v4Enabled, v6Enabled)).Return(getLinkAddrs(existingLinkAddr, linkName1), nil)
		nlMock.On("AddrList", nlLink2Mock, getIPFamilyInt(v4Enabled, v6Enabled)).Return(getLinkAddrs(existingLinkAddr, linkName2), nil)
		nlMock.On("AddrAdd", nlLink1Mock, &expectedAddr).Return(nil)
		c = NewController("test", v4Enabled, v6Enabled, nil)
		c.store = existingStore
		err := c.AddAddress(addrToAdd)
		expectedResMatcher := gomega.Succeed()
		if expectErr {
			expectedResMatcher = gomega.HaveOccurred()
		}
		gomega.Expect(err).Should(expectedResMatcher)
		if !expectErr {
			gomega.Expect(isAddrInStore(c.store, linkName1, expectedAddr)).Should(gomega.BeTrue())
		}
		if expectAddAddrCalled {
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrAdd", nlLink1Mock, &expectedAddr)).Should(gomega.BeTrue())
		}
	}, ginkgo.Entry("Add valid IPv4 address with empty store",
		newNetlinkAddrWithIndexSet(v4CIDR1, linkName1), []netlink.Addr{}, map[string][]netlink.Addr{}, v4Enabled, v6Disabled, noErr, addrAddCalled),
		ginkgo.Entry("Doesn't add IPv4 address when IPv4 is disabled and IPv6 enabled",
			newNetlinkAddrWithIndexSet(v4CIDR1, linkName1), []netlink.Addr{}, map[string][]netlink.Addr{}, v4Disable, v6Enabled, Err, addrAddNotCalled),
		ginkgo.Entry("Add IPv4 address when it exists in store but not applied",
			newNetlinkAddrWithIndexSet(v4CIDR1, linkName1), []netlink.Addr{},
			map[string][]netlink.Addr{
				linkName1: {newNetlinkAddrWithIndexSet(v4CIDR1, linkName1)},
			},
			v4Enabled, v6Disabled, noErr, addrAddNotCalled),
		ginkgo.Entry("Doesn't attempt to add an IPv4 address when already applied and exists in store",
			newNetlinkAddrWithIndexSet(v4CIDR1, linkName1), []netlink.Addr{newNetlinkAddrWithIndexSet(v4CIDR1, linkName1)},
			map[string][]netlink.Addr{
				linkName1: {newNetlinkAddrWithIndexSet(v4CIDR1, linkName1)},
			},
			v4Enabled, v6Disabled, noErr, addrAddNotCalled),
	)

	// Test that:
	// 1. Deletion of address from store
	// 2. Address deletion func (AddrDel) is called
	//
	// There maybe a discrepancy between existingLinkAddr link addresses and existingStore link addresses because a link may
	// have addresses that aren't managed. Link1 is always the target of the new addresses to delete.
	ginkgo.DescribeTable("Delete address from link1", func(addrToDel netlink.Addr, existingLinkAddr []netlink.Addr, existingStore map[string][]netlink.Addr,
		v4Enabled, v6Enabled, expectErr, expectDelAddrCalled bool) {

		expectedAddr := addrToDel
		nlLink1Mock.On("Attrs").Return(&netlink.LinkAttrs{Name: linkName1, Index: getLinkIndexFromName(linkName1)}, nil)
		nlLink2Mock.On("Attrs").Return(&netlink.LinkAttrs{Name: linkName2, Index: getLinkIndexFromName(linkName2)}, nil)
		nlMock.On("LinkList").Return([]netlink.Link{nlLink1Mock, nlLink2Mock}, nil)
		nlMock.On("LinkByIndex", getLinkIndexFromName(linkName1)).Return(nlLink1Mock, nil)
		nlMock.On("LinkByIndex", getLinkIndexFromName(linkName2)).Return(nlLink2Mock, nil)
		nlMock.On("AddrList", nlLink1Mock, getIPFamilyInt(v4Enabled, v6Enabled)).Return(getLinkAddrs(existingLinkAddr, linkName1), nil)
		nlMock.On("AddrList", nlLink2Mock, getIPFamilyInt(v4Enabled, v6Enabled)).Return(getLinkAddrs(existingLinkAddr, linkName2), nil)
		nlMock.On("AddrDel", nlLink1Mock, &expectedAddr).Return(nil)
		c = NewController("test", v4Enabled, v6Enabled, nil)
		c.store = existingStore
		err := c.DelAddress(addrToDel)
		expectedResMatcher := gomega.Succeed()
		if expectErr {
			expectedResMatcher = gomega.HaveOccurred()
		}
		gomega.Expect(err).Should(expectedResMatcher)
		if !expectErr {
			gomega.Expect(isAddrInStore(c.store, linkName1, expectedAddr)).Should(gomega.BeFalse())
		}
		if expectDelAddrCalled {
			gomega.Expect(nlMock.AssertCalled(ginkgo.GinkgoT(), "AddrDel", nlLink1Mock, &expectedAddr)).Should(gomega.BeTrue())
		}
	}, ginkgo.Entry("Deletes an IPv4 address which exists in store and is applied",
		newNetlinkAddrWithIndexSet(v4CIDR1, linkName1), []netlink.Addr{
			newNetlinkAddrWithIndexSet(v4CIDR1, linkName1),
			newNetlinkAddrWithIndexSet(v4CIDR2, linkName2),
		}, map[string][]netlink.Addr{
			linkName1: {newNetlinkAddrWithIndexSet(v4CIDR1, linkName1)},
			linkName2: {newNetlinkAddrWithIndexSet(v4CIDR2, linkName2)},
		}, v4Enabled, v6Disabled, noErr, addrDelCalled),
		ginkgo.Entry("Doesn't attempt to delete an IPv4 address which exists in store but not applied",
			newNetlinkAddrWithIndexSet(v4CIDR1, linkName1), []netlink.Addr{
				newNetlinkAddrWithIndexSet(v4CIDR1, linkName1), // different address than the one attempted to be deleted
				newNetlinkAddrWithIndexSet(v4CIDR2, linkName2),
			}, map[string][]netlink.Addr{
				linkName1: {newNetlinkAddrWithIndexSet(v4CIDR1, linkName1)},
				linkName2: {newNetlinkAddrWithIndexSet(v4CIDR2, linkName2)},
			}, v4Enabled, v6Disabled, noErr, addrDelNotCalled),
		ginkgo.Entry("Doesn't delete IPv4 address when IPv4 is disabled and IPv6 enabled",
			newNetlinkAddrWithIndexSet(v4CIDR1, linkName1), []netlink.Addr{}, map[string][]netlink.Addr{}, v4Disable, v6Enabled, Err, addrDelNotCalled),
	)
})

func isAddrInStore(store map[string][]netlink.Addr, linkName string, expectedAddr netlink.Addr) bool {
	linkAddrs, ok := store[linkName]
	if !ok {
		return false
	}
	for _, addr := range linkAddrs {
		if addr.Equal(expectedAddr) {
			return true
		}
	}
	return false
}

func newNetlinkAddr(cidr string) netlink.Addr {
	nlAddr, err := netlink.ParseAddr(cidr)
	if err != nil {
		panic(fmt.Sprintf("failed to parse CIDR %q: %v", cidr, err))
	}
	return *nlAddr
}

func getIPFamilyInt(v4, v6 bool) int {
	if v4 && v6 {
		return netlink.FAMILY_ALL
	}
	if v4 {
		return netlink.FAMILY_V4
	}
	return netlink.FAMILY_V6
}

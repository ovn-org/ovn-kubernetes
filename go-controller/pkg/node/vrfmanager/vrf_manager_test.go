package vrfmanager

import (
	"fmt"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	netlink_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
)

var _ = ginkgo.Describe("Vrf manager", func() {

	var (
		vrfLinkName1    = "vrf-blue"
		enslaveLinkName = "dev100"
		vrfLinkName2    = "vrf-red"
		nlMock          *mocks.NetLinkOps
		vrfLinkMock1    *netlink_mocks.Link
		enslaveLinkMock *netlink_mocks.Link
		vrfLinkMock2    *netlink_mocks.Link
	)

	linkIndexes := map[string]int{
		vrfLinkName1:    1,
		enslaveLinkName: 2,
		vrfLinkName2:    3,
	}

	masterIndexes := map[string]int{
		vrfLinkName1:    0,
		enslaveLinkName: 1,
		vrfLinkName2:    0,
	}

	getLinkIndex := func(linkName string) int {
		index, ok := linkIndexes[linkName]
		if !ok {
			panic(fmt.Sprintf("failed to find index for link name %q", linkName))
		}
		return index
	}

	getLinkMasterIndex := func(linkName string) int {
		masterIndex, ok := masterIndexes[linkName]
		if !ok {
			panic(fmt.Sprintf("failed to find index for link name %q", linkName))
		}
		return masterIndex
	}

	ginkgo.BeforeEach(func() {
		nlMock = &mocks.NetLinkOps{}
		vrfLinkMock1 = new(netlink_mocks.Link)
		enslaveLinkMock = new(netlink_mocks.Link)
		vrfLinkMock2 = new(netlink_mocks.Link)
		util.SetNetLinkOpMockInst(nlMock)

		nlMock.On("LinkList").Return([]netlink.Link{vrfLinkMock1, enslaveLinkMock}, nil)
		nlMock.On("LinkByName", vrfLinkName1).Return(vrfLinkMock1, nil)
		nlMock.On("LinkByName", enslaveLinkName).Return(enslaveLinkMock, nil)
		nlMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
		nlMock.On("LinkAdd", mock.Anything).Return(nil)
		nlMock.On("LinkSetUp", mock.Anything).Return(nil)
		nlMock.On("LinkSetMaster", enslaveLinkMock, vrfLinkMock1).Return(nil)
		vrfLinkMock1.On("Type").Return("vrf")
		vrfLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: vrfLinkName1, MasterIndex: getLinkMasterIndex(vrfLinkName1), Index: getLinkIndex(vrfLinkName1)}, nil)
		enslaveLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName, MasterIndex: getLinkIndex(enslaveLinkName), Index: getLinkIndex(enslaveLinkName)}, nil)
		nlMock.On("LinkList").Return([]netlink.Link{vrfLinkMock1, enslaveLinkMock, vrfLinkMock2}, nil)

		nlMock.On("LinkByName", vrfLinkName2).Return(vrfLinkMock2, nil)
		nlMock.On("IsLinkNotFoundError", mock.Anything).Return(false)
		nlMock.On("LinkDelete", vrfLinkMock2).Return(nil)
	})

	ginkgo.AfterEach(func() {
		util.ResetNetLinkOpMockInst()
	})

	ginkgo.Context("Vrfs", func() {
		ginkgo.It("add vrf with an enslave interface", func() {
			c := NewController()
			enslaveInfaces := sets.Set[string]{}
			enslaveInfaces.Insert(enslaveLinkName)
			vrf := &vrf{name: vrfLinkName1, table: 10, interfaces: enslaveInfaces}
			c.vrfs[vrfLinkName1] = vrf
			err := c.addVrf(vrf)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.It("update vrf with removing associated stale enslave interface", func() {
			nlMock.On("LinkSetMaster", enslaveLinkMock, nil).Return(nil)
			enslaveLinkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName, MasterIndex: getLinkMasterIndex(enslaveLinkName), Index: getLinkIndex(enslaveLinkName)}, nil)
			c := NewController()
			vrf := &vrf{name: vrfLinkName1, table: 10, interfaces: sets.Set[string]{}}
			c.vrfs[vrfLinkName1] = vrf
			err := c.addVrf(vrf)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.It("delete vrf", func() {
			c := NewController()
			vrf := &vrf{name: vrfLinkName2, table: 20, interfaces: sets.Set[string]{}, delete: true}
			c.vrfs[vrfLinkName1] = vrf
			err := c.delVrf(vrf)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.It("reconcile vrfs", func() {
			c := NewController()
			enslaveInfaces := sets.Set[string]{}
			enslaveInfaces.Insert(enslaveLinkName)
			c.vrfs[vrfLinkName1] = &vrf{name: vrfLinkName1, table: 10, interfaces: enslaveInfaces}
			c.vrfs[vrfLinkName2] = &vrf{name: vrfLinkName2, table: 20, interfaces: sets.Set[string]{}, delete: true}
			err := c.reconcile()
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})
	})
})

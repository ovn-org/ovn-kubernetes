package vrfmanager

import (
	"fmt"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	netlink_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
)

var _ = ginkgo.Describe("VRF manager", func() {

	var (
		c                *Controller
		vrfLinkName1     = "100-vrf"
		enslaveLinkName1 = "dev100"
		enslaveLinkName2 = "dev101"
		vrfLinkName2     = "200-vrf"
		nlMock           *mocks.NetLinkOps
		vrfLinkMock1     *netlink_mocks.Link
		enslaveLinkMock1 *netlink_mocks.Link
		enslaveLinkMock2 *netlink_mocks.Link
		vrfLinkMock2     *netlink_mocks.Link
	)

	linkIndexes := map[string]int{
		vrfLinkName1:     1,
		enslaveLinkName1: 2,
		enslaveLinkName2: 3,
		vrfLinkName2:     4,
	}

	masterIndexes := map[string]int{
		vrfLinkName1:     0,
		enslaveLinkName1: 1,
		enslaveLinkName2: 1,
		vrfLinkName2:     0,
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
		c = NewController(routemanager.NewController())

		nlMock = &mocks.NetLinkOps{}
		vrfLinkMock1 = new(netlink_mocks.Link)
		enslaveLinkMock1 = new(netlink_mocks.Link)
		enslaveLinkMock2 = new(netlink_mocks.Link)
		vrfLinkMock2 = new(netlink_mocks.Link)
		util.SetNetLinkOpMockInst(nlMock)

		nlMock.On("LinkByName", vrfLinkName1).Return(vrfLinkMock1, nil)
		nlMock.On("LinkByName", enslaveLinkName1).Return(enslaveLinkMock1, nil)
		nlMock.On("LinkByName", enslaveLinkName2).Return(enslaveLinkMock2, nil)
		nlMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
		nlMock.On("LinkAdd", mock.Anything).Return(nil)
		nlMock.On("LinkSetUp", mock.Anything).Return(nil)
		vrfLinkMock1.On("Type").Return("vrf")
		vrfLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: vrfLinkName1, MasterIndex: getLinkMasterIndex(vrfLinkName1), Index: getLinkIndex(vrfLinkName1)}, nil)

		nlMock.On("LinkByName", vrfLinkName2).Return(vrfLinkMock2, nil)
		vrfLinkMock2.On("Type").Return("vrf")
		vrfLinkMock2.On("Attrs").Return(&netlink.LinkAttrs{Name: vrfLinkName2, MasterIndex: getLinkMasterIndex(vrfLinkName2), Index: getLinkIndex(vrfLinkName2), OperState: netlink.OperUp}, nil)
		nlMock.On("LinkDelete", vrfLinkMock2).Return(nil)
	})

	ginkgo.AfterEach(func() {
		util.ResetNetLinkOpMockInst()
	})

	ginkgo.Context("VRFs", func() {
		ginkgo.It("add VRF with a slave interface", func() {
			nlMock.On("LinkList").Return([]netlink.Link{vrfLinkMock1, enslaveLinkMock1}, nil)
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: 0, Index: getLinkIndex(enslaveLinkName1)}, nil)
			nlMock.On("LinkSetMaster", enslaveLinkMock1, vrfLinkMock1).Return(nil)
			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, 10, nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.It("delete VRF", func() {
			err := c.AddVRF(vrfLinkName2, "", 20, nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			err = c.DeleteVRF(vrfLinkName2)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.It("reconcile VRFs", func() {
			nlMock.On("LinkList").Return([]netlink.Link{vrfLinkMock1, vrfLinkMock2, enslaveLinkMock1}, nil)
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkMasterIndex(enslaveLinkName1), Index: getLinkIndex(enslaveLinkName1)}, nil)
			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, 10, nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			err = c.AddVRF(vrfLinkName2, "", 20, nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			err = util.GetNetLinkOps().LinkDelete(vrfLinkMock2)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			enslaveLinkMock1.On("Type").Return("dummy")
			err = c.reconcile()
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.It("repair VRFs", func() {
			nlMock.On("LinkList").Return([]netlink.Link{vrfLinkMock1, vrfLinkMock2, enslaveLinkMock1}, nil)
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkMasterIndex(enslaveLinkName1), Index: getLinkIndex(enslaveLinkName1)}, nil)
			enslaveLinkMock1.On("Type").Return("dummy")
			validVRFs := make(sets.Set[string])
			validVRFs.Insert(vrfLinkName1)
			// Now the Repair call would delete vrfLinkMock2 link.
			err := c.Repair(validVRFs)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})
	})
})

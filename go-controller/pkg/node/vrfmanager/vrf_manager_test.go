package vrfmanager

import (
	"fmt"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	netlink_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
)

var (
	c            *Controller
	vrfLinkName1 = "blue"
	vrfLinkName2 = "mp200-udn-vrf"
	vrfLinkName3 = "red-not-udn"
)

var _ = ginkgo.Describe("VRF manager", func() {

	var (
		enslaveLinkName1 = "dev100"
		enslaveLinkName2 = "dev101"
		nlMock           *mocks.NetLinkOps
		enslaveLinkMock1 *netlink_mocks.Link
		enslaveLinkMock2 *netlink_mocks.Link
	)

	linkIndexes := map[string]int{
		vrfLinkName1:     1,
		enslaveLinkName1: 2,
		enslaveLinkName2: 3,
		vrfLinkName2:     4,
		vrfLinkName3:     5,
	}

	masterIndexes := map[string]int{
		vrfLinkName1:     0,
		enslaveLinkName1: 1,
		enslaveLinkName2: 1,
		vrfLinkName2:     0,
		vrfLinkName3:     0,
	}

	vrfTables := map[string]uint32{
		vrfLinkName1: 1000,
		vrfLinkName2: 2000,
		vrfLinkName3: 999,
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

	getVRFTable := func(linkName string) uint32 {
		table, ok := vrfTables[linkName]
		if !ok {
			panic(fmt.Sprintf("failed to find table for vrf %q", linkName))
		}
		return table
	}

	buildVRF := func(name string) *netlink.Vrf {
		return &netlink.Vrf{
			LinkAttrs: netlink.LinkAttrs{Name: name, MasterIndex: getLinkMasterIndex(name), Index: getLinkIndex(name)},
			Table:     getVRFTable(name),
		}
	}

	ginkgo.BeforeEach(func() {
		c = NewController(routemanager.NewController())

		nlMock = &mocks.NetLinkOps{}

		enslaveLinkMock1 = new(netlink_mocks.Link)
		enslaveLinkMock2 = new(netlink_mocks.Link)
		util.SetNetLinkOpMockInst(nlMock)

		nlMock.On("LinkByName", vrfLinkName1).Return(buildVRF(vrfLinkName1), nil)
		nlMock.On("LinkByName", enslaveLinkName1).Return(enslaveLinkMock1, nil)
		nlMock.On("LinkByName", enslaveLinkName2).Return(enslaveLinkMock2, nil)
		nlMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
		nlMock.On("LinkAdd", mock.Anything).Return(nil)
		nlMock.On("LinkSetUp", mock.Anything).Return(nil)

		nlMock.On("LinkByName", vrfLinkName2).Return(buildVRF(vrfLinkName2), nil)
		nlMock.On("LinkDelete", buildVRF(vrfLinkName2)).Return(nil)

		nlMock.On("LinkByName", vrfLinkName3).Return(buildVRF(vrfLinkName3), nil)
	})

	ginkgo.AfterEach(func() {
		util.ResetNetLinkOpMockInst()
	})

	ginkgo.Context("VRFs", func() {
		ginkgo.It("add VRF with a slave interface", func() {
			nlMock.On("LinkList").Return([]netlink.Link{buildVRF(vrfLinkName1), enslaveLinkMock1}, nil)
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: 0, Index: getLinkIndex(enslaveLinkName1)}, nil)
			nlMock.On("LinkSetMaster", enslaveLinkMock1, buildVRF(vrfLinkName1)).Return(nil)
			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.It("fails if we add a VRF with a long name", func() {
			err := c.AddVRF("this.name.is.too.long", "other", 0, nil)
			gomega.Expect(err).Should(gomega.HaveOccurred())
		})

		ginkgo.It("fails if we add a VRF with a non managed routing table", func() {
			err := c.AddVRF("this.name.is.ok", "other", 999, nil)
			gomega.Expect(err).Should(gomega.HaveOccurred())
		})

		ginkgo.It("fails if we add VRF with same name as existing non-managed VRF", func() {
			nlMock.On("LinkList").Return([]netlink.Link{buildVRF(vrfLinkName3)}, nil)
			err := c.AddVRF(vrfLinkName3, "other", 3000, nil)
			gomega.Expect(err).Should(gomega.HaveOccurred())
		})

		ginkgo.It("delete VRF", func() {
			err := c.AddVRF(vrfLinkName2, "", getVRFTable(vrfLinkName2), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			err = c.DeleteVRF(vrfLinkName2)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.It("reconcile VRFs", func() {
			nlMock.On("LinkList").Return([]netlink.Link{buildVRF(vrfLinkName1), buildVRF(vrfLinkName2), enslaveLinkMock1}, nil)
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkMasterIndex(enslaveLinkName1), Index: getLinkIndex(enslaveLinkName1)}, nil)
			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			err = c.AddVRF(vrfLinkName2, "", getVRFTable(vrfLinkName2), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			err = util.GetNetLinkOps().LinkDelete(buildVRF(vrfLinkName2))
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			enslaveLinkMock1.On("Type").Return("dummy")
			err = c.reconcile()
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			// Invoke reconcile again to ensure both vrf links in sync.
			err = c.reconcile()
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.It("repair VRFs", func() {
			nlMock.On("LinkList").Return([]netlink.Link{buildVRF(vrfLinkName1), buildVRF(vrfLinkName2), buildVRF(vrfLinkName3), enslaveLinkMock1}, nil)
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

var _ = ginkgo.Describe("VRF manager tests with a network namespace", func() {
	var (
		testNS ns.NetNS
		stopCh chan struct{}
		wg     *sync.WaitGroup
	)
	ginkgo.BeforeEach(func() {
		var err error
		testNS, err = testutils.NewNS()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		wg = &sync.WaitGroup{}
		stopCh = make(chan struct{})
		routeManager := routemanager.NewController()
		wg.Add(1)
		go testNS.Do(func(netNS ns.NetNS) error {
			defer wg.Done()
			routeManager.Run(stopCh, 2*time.Minute)
			return nil
		})
		// set vrf manager reconcile period into one second.
		reconcilePeriod = 1 * time.Second
		c = NewController(routeManager)
		wg2 := &sync.WaitGroup{}
		defer func() {
			wg2.Wait()
		}()
		wg2.Add(1)
		go testNS.Do(func(netNS ns.NetNS) error {
			defer func() {
				ginkgo.GinkgoRecover()
				wg2.Done()
			}()
			err = c.Run(stopCh, wg)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return nil
		})
	})
	ginkgo.AfterEach(func() {
		close(stopCh)
		wg.Wait()
		gomega.Expect(testNS.Close()).To(gomega.Succeed())
		gomega.Expect(testutils.UnmountNS(testNS)).To(gomega.Succeed())
		util.ResetRunner()
	})

	checkforVrfLinkExistence := func() error {
		err := testNS.Do(func(ns.NetNS) error {
			if _, err := util.GetNetLinkOps().LinkByName(vrfLinkName1); err != nil {
				return err
			}
			_, err := util.GetNetLinkOps().LinkByName(vrfLinkName2)
			return err
		})
		return err
	}

	ovntest.OnSupportedPlatformsIt("ensure VRF manager is reconciling configured VRF devices correctly", func() {
		err := testNS.Do(func(ns.NetNS) error {
			defer ginkgo.GinkgoRecover()
			err := c.AddVRF(vrfLinkName1, "", 1000, nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			err = c.AddVRF(vrfLinkName2, "", 2000, nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			wg3 := &sync.WaitGroup{}
			wg3.Add(1)
			go func() {
				defer func() {
					ginkgo.GinkgoRecover()
					wg3.Done()
				}()
				// wait enough to reconcile ran for few times.
				time.Sleep(5 * time.Second)
				err = checkforVrfLinkExistence()
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			}()
			wg3.Wait()

			// Invoke reconcile method explicitly few times to ensure it's always working fine.
			err = c.reconcile()
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			err = checkforVrfLinkExistence()
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			err = c.reconcile()
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			err = checkforVrfLinkExistence()
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			return nil
		})
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	})
})

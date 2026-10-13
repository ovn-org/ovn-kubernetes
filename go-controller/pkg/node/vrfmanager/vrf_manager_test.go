// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vrfmanager

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	netlink_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
)

var (
	c            *Controller
	vrfLinkName1 = "blue"
	vrfLinkName2 = "mp200-udn-vrf"
	vrfLinkName3 = "red-not-udn"
	vrfLinkName4 = "green"
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
		vrfLinkName4:     6,
	}

	masterIndexes := map[string]int{
		vrfLinkName1:     0,
		enslaveLinkName1: 1,
		enslaveLinkName2: 1,
		vrfLinkName2:     0,
		vrfLinkName3:     0,
		vrfLinkName4:     0,
	}

	vrfTables := map[string]uint32{
		vrfLinkName1: 1000,
		vrfLinkName2: 2000,
		vrfLinkName3: 999,
		vrfLinkName4: 3000,
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
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		c = NewController(routemanager.NewController())

		nlMock = &mocks.NetLinkOps{}

		enslaveLinkMock1 = new(netlink_mocks.Link)
		enslaveLinkMock2 = new(netlink_mocks.Link)
		util.SetNetLinkOpMockInst(nlMock)

		nlMock.On("LinkByName", vrfLinkName1).Return(buildVRF(vrfLinkName1), nil)
		nlMock.On("LinkByName", enslaveLinkName1).Return(enslaveLinkMock1, nil)
		nlMock.On("LinkByName", enslaveLinkName2).Return(enslaveLinkMock2, nil)
		// A nil error is never a not-found error: the specific expectation
		// is registered first so it wins over the catch-all below.
		nlMock.On("IsLinkNotFoundError", nil).Return(false)
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
			nlMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
			nlMock.On("LinkSetMaster", enslaveLinkMock1, buildVRF(vrfLinkName1)).Return(nil)
			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.It("migrates the slave interface routes into the VRF table on enslavement", func() {
			slaveLinkIndex := getLinkIndex(enslaveLinkName1)
			nlMock.On("LinkList").Return([]netlink.Link{buildVRF(vrfLinkName1), enslaveLinkMock1}, nil)
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: 0, Index: slaveLinkIndex}, nil)
			// The slave interface holds a DHCP default route through an
			// off-subnet gateway (with the kernel's carrier state flag set),
			// the DHCP host route covering that gateway, a kernel generated
			// connected route and a BGP route before the enslavement.
			dhcpDefaultRoute := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Gw:        net.ParseIP("192.168.2.1"),
				Protocol:  unix.RTPROT_DHCP,
				Type:      unix.RTN_UNICAST,
				Flags:     unix.RTNH_F_ONLINK | unix.RTNH_F_LINKDOWN,
				Table:     unix.RT_TABLE_MAIN,
			}
			dhcpGatewayHostRoute := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Dst:       ovntest.MustParseIPNet("192.168.2.1/32"),
				Scope:     netlink.SCOPE_LINK,
				Protocol:  unix.RTPROT_DHCP,
				Type:      unix.RTN_UNICAST,
				Table:     unix.RT_TABLE_MAIN,
			}
			kernelConnectedRoute := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Dst:       ovntest.MustParseIPNet("192.168.1.0/24"),
				Protocol:  unix.RTPROT_KERNEL,
				Type:      unix.RTN_UNICAST,
				Table:     unix.RT_TABLE_MAIN,
			}
			bgpRoute := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Dst:       ovntest.MustParseIPNet("10.10.0.0/16"),
				Gw:        net.ParseIP("192.168.1.253"),
				Protocol:  unix.RTPROT_BGP,
				Type:      unix.RTN_UNICAST,
				Table:     unix.RT_TABLE_MAIN,
			}
			// A static ECMP route through the slave interface: the kernel
			// reports it with a zero top-level link index, its interfaces are
			// in the nexthops.
			ecmpRoute := netlink.Route{
				Dst:      ovntest.MustParseIPNet("172.16.0.0/16"),
				Protocol: unix.RTPROT_STATIC,
				Type:     unix.RTN_UNICAST,
				Table:    unix.RT_TABLE_MAIN,
				MultiPath: []*netlink.NexthopInfo{
					{LinkIndex: slaveLinkIndex, Gw: net.ParseIP("192.168.1.1"), Flags: unix.RTNH_F_LINKDOWN},
					{LinkIndex: slaveLinkIndex, Gw: net.ParseIP("192.168.1.2")},
				},
			}
			// An ECMP route spanning the slave and another interface cannot
			// follow the slave across routing tables and is not migrated.
			mixedInterfacesEcmpRoute := netlink.Route{
				Dst:      ovntest.MustParseIPNet("172.17.0.0/16"),
				Protocol: unix.RTPROT_STATIC,
				Type:     unix.RTN_UNICAST,
				Table:    unix.RT_TABLE_MAIN,
				MultiPath: []*netlink.NexthopInfo{
					{LinkIndex: slaveLinkIndex, Gw: net.ParseIP("192.168.1.1")},
					{LinkIndex: getLinkIndex(enslaveLinkName2), Gw: net.ParseIP("192.168.3.1")},
				},
			}
			nlMock.On("RouteListFiltered", netlink.FAMILY_ALL,
				mock.MatchedBy(func(filter *netlink.Route) bool {
					return filter.Table == unix.RT_TABLE_MAIN
				}),
				uint64(netlink.RT_FILTER_TABLE),
			).Return([]netlink.Route{dhcpDefaultRoute, kernelConnectedRoute, bgpRoute, dhcpGatewayHostRoute, ecmpRoute, mixedInterfacesEcmpRoute}, nil)
			nlMock.On("LinkSetMaster", enslaveLinkMock1, buildVRF(vrfLinkName1)).Return(nil)
			nlMock.On("RouteAdd", mock.Anything).Return(nil)

			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			// The DHCP routes, the kernel connected route and the ECMP route
			// are restored into the VRF table with kernel state flags
			// stripped; the BGP route is left for its daemon and the
			// mixed-interfaces ECMP route stays behind.
			expectedConnectedRoute := kernelConnectedRoute
			expectedConnectedRoute.Table = int(getVRFTable(vrfLinkName1))
			expectedHostRoute := dhcpGatewayHostRoute
			expectedHostRoute.Table = int(getVRFTable(vrfLinkName1))
			expectedDefaultRoute := dhcpDefaultRoute
			expectedDefaultRoute.Table = int(getVRFTable(vrfLinkName1))
			expectedDefaultRoute.Flags = unix.RTNH_F_ONLINK
			expectedEcmpRoute := ecmpRoute
			expectedEcmpRoute.Table = int(getVRFTable(vrfLinkName1))
			expectedEcmpRoute.MultiPath = []*netlink.NexthopInfo{
				{LinkIndex: slaveLinkIndex, Gw: net.ParseIP("192.168.1.1")},
				{LinkIndex: slaveLinkIndex, Gw: net.ParseIP("192.168.1.2")},
			}
			nlMock.AssertCalled(ginkgo.GinkgoT(), "RouteAdd", &expectedConnectedRoute)
			nlMock.AssertCalled(ginkgo.GinkgoT(), "RouteAdd", &expectedHostRoute)
			nlMock.AssertCalled(ginkgo.GinkgoT(), "RouteAdd", &expectedDefaultRoute)
			nlMock.AssertCalled(ginkgo.GinkgoT(), "RouteAdd", &expectedEcmpRoute)
			nlMock.AssertNumberOfCalls(ginkgo.GinkgoT(), "RouteAdd", 4)

			// The gateway-less routes must be restored before the gateway
			// routes that depend on them.
			var restored []*netlink.Route
			for _, call := range nlMock.Calls {
				if call.Method == "RouteAdd" {
					restored = append(restored, call.Arguments.Get(0).(*netlink.Route))
				}
			}
			gomega.Expect(restored).To(gomega.Equal([]*netlink.Route{&expectedConnectedRoute, &expectedHostRoute, &expectedDefaultRoute, &expectedEcmpRoute}),
				"expected the gateway-less routes to be restored before the gateway routes that depend on them")
		})

		ginkgo.It("retries a route restore the kernel transiently rejects", func() {
			// A restored route can fail validation right after a master
			// change, e.g. EINVAL while its IPv6 source address re-runs
			// duplicate address detection; the retry must absorb it.
			route := netlink.Route{
				LinkIndex: getLinkIndex(enslaveLinkName1),
				Dst:       ovntest.MustParseIPNet("2001:db8:1::/64"),
				Table:     unix.RT_TABLE_MAIN,
			}
			nlMock.On("RouteAdd", mock.Anything).Return(unix.EINVAL).Once()
			nlMock.On("RouteAdd", mock.Anything).Return(nil).Once()

			gomega.Expect(addRouteWithRetry(&route)).To(gomega.Succeed())
			nlMock.AssertNumberOfCalls(ginkgo.GinkgoT(), "RouteAdd", 2)
		})

		ginkgo.It("undoes the enslavement when the route restore into the VRF fails", func() {
			slaveLinkIndex := getLinkIndex(enslaveLinkName1)
			nlMock.On("LinkList").Return([]netlink.Link{buildVRF(vrfLinkName1), enslaveLinkMock1}, nil)
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: 0, Index: slaveLinkIndex}, nil)
			dhcpDefaultRoute := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Gw:        net.ParseIP("192.168.1.1"),
				Protocol:  unix.RTPROT_DHCP,
				Type:      unix.RTN_UNICAST,
				Table:     unix.RT_TABLE_MAIN,
			}
			nlMock.On("RouteListFiltered", netlink.FAMILY_ALL,
				mock.MatchedBy(func(filter *netlink.Route) bool {
					return filter.Table == unix.RT_TABLE_MAIN
				}),
				uint64(netlink.RT_FILTER_TABLE),
			).Return([]netlink.Route{dhcpDefaultRoute}, nil)
			nlMock.On("LinkSetMaster", enslaveLinkMock1, buildVRF(vrfLinkName1)).Return(nil)
			restoreErr := fmt.Errorf("route restore failed")
			nlMock.On("IsAlreadyExistsError", restoreErr).Return(false)
			// The restore into the VRF table fails, the one back into the
			// main table succeeds.
			nlMock.On("RouteAdd", mock.Anything).Return(restoreErr).Once()
			nlMock.On("RouteAdd", mock.Anything).Return(nil)
			nlMock.On("LinkSetNoMaster", enslaveLinkMock1).Return(nil)

			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), nil)
			gomega.Expect(err).Should(gomega.MatchError(gomega.ContainSubstring("undid the enslavement")))
			nlMock.AssertCalled(ginkgo.GinkgoT(), "LinkSetNoMaster", enslaveLinkMock1)

			// The captured route went back to the main table.
			expectedRoute := dhcpDefaultRoute
			nlMock.AssertCalled(ginkgo.GinkgoT(), "RouteAdd", &expectedRoute)
			nlMock.AssertNumberOfCalls(ginkgo.GinkgoT(), "RouteAdd", 2)
		})

		ginkgo.It("adds another slave interface to an existing VRF", func() {
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkIndex(vrfLinkName1), Index: getLinkIndex(enslaveLinkName1)}, nil)
			enslaveLinkMock2.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName2, MasterIndex: 0, Index: getLinkIndex(enslaveLinkName2)}, nil)
			nlMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
			nlMock.On("LinkSetMaster", enslaveLinkMock2, buildVRF(vrfLinkName1)).Return(nil)

			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			err = c.AddVRFSlave(vrfLinkName1, enslaveLinkName2)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			nlMock.AssertCalled(ginkgo.GinkgoT(), "LinkSetMaster", enslaveLinkMock2, buildVRF(vrfLinkName1))
		})

		ginkgo.It("rejects assigning a slave interface to two VRFs", func() {
			c.vrfs[getLinkIndex(vrfLinkName1)] = newVRF(vrfLinkName1, getVRFTable(vrfLinkName1), enslaveLinkName1, nil)
			c.vrfs[getLinkIndex(vrfLinkName2)] = newVRF(vrfLinkName2, getVRFTable(vrfLinkName2), "", nil)

			err := c.AddVRFSlave(vrfLinkName2, enslaveLinkName1)
			var conflict *VRFSlaveConflictError
			gomega.Expect(errors.As(err, &conflict)).To(gomega.BeTrue())
			gomega.Expect(conflict).To(gomega.Equal(&VRFSlaveConflictError{
				Interface:    enslaveLinkName1,
				RequestedVRF: vrfLinkName2,
				ExistingVRF:  vrfLinkName1,
			}))
		})

		ginkgo.It("rejects assigning a slave interface already attached to another VRF", func() {
			c.vrfs[getLinkIndex(vrfLinkName2)] = newVRF(vrfLinkName2, getVRFTable(vrfLinkName2), "", nil)
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{
				Name:        enslaveLinkName1,
				MasterIndex: getLinkIndex(vrfLinkName1),
				Index:       getLinkIndex(enslaveLinkName1),
			}, nil)
			nlMock.On("LinkByIndex", getLinkIndex(vrfLinkName1)).Return(buildVRF(vrfLinkName1), nil)

			err := c.AddVRFSlave(vrfLinkName2, enslaveLinkName1)
			var conflict *VRFSlaveConflictError
			gomega.Expect(errors.As(err, &conflict)).To(gomega.BeTrue())
			gomega.Expect(conflict).To(gomega.Equal(&VRFSlaveConflictError{
				Interface:    enslaveLinkName1,
				RequestedVRF: vrfLinkName2,
				ExistingVRF:  vrfLinkName1,
			}))
		})

		ginkgo.It("removes a slave interface from an existing VRF", func() {
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkIndex(vrfLinkName1), Index: getLinkIndex(enslaveLinkName1)}, nil)
			nlMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
			nlMock.On("LinkSetNoMaster", enslaveLinkMock1).Return(nil)

			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			err = c.DeleteVRFSlave(vrfLinkName1, enslaveLinkName1)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			nlMock.AssertCalled(ginkgo.GinkgoT(), "LinkSetNoMaster", enslaveLinkMock1)
		})

		ginkgo.It("migrates the slave interface routes back to the main table on release, except OVN managed ones", func() {
			slaveLinkIndex := getLinkIndex(enslaveLinkName1)
			vrfTable := int(getVRFTable(vrfLinkName1))
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkIndex(vrfLinkName1), Index: slaveLinkIndex}, nil)
			// The VRF table holds routes previously migrated on enslavement,
			// OVN managed routes carrying the OVN-Kubernetes protocol and a
			// BGP route learned in the VRF, all on the slave interface. Note
			// that the kernel dumps default routes with a nil destination.
			migratedDefaultRoute := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Gw:        net.ParseIP("192.168.1.1"),
				Protocol:  unix.RTPROT_DHCP,
				Type:      unix.RTN_UNICAST,
				Table:     vrfTable,
			}
			migratedDefaultRouteV6 := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Gw:        net.ParseIP("2001:db8:1::1"),
				Protocol:  unix.RTPROT_DHCP,
				Type:      unix.RTN_UNICAST,
				Priority:  1024,
				Table:     vrfTable,
			}
			ovnManagedRoute := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Dst:       ovntest.MustParseIPNet("10.96.0.0/16"),
				Gw:        net.ParseIP("169.254.169.4"),
				Protocol:  netlink.RouteProtocol(types.OVNKProtocol),
				Type:      unix.RTN_UNICAST,
				Table:     vrfTable,
			}
			ovnManagedRouteV6 := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Dst:       ovntest.MustParseIPNet("fd00:10:96::/112"),
				Gw:        net.ParseIP("fd69::4"),
				Protocol:  netlink.RouteProtocol(types.OVNKProtocol),
				Type:      unix.RTN_UNICAST,
				Priority:  1024,
				Table:     vrfTable,
			}
			bgpLearnedRoute := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Dst:       ovntest.MustParseIPNet("10.10.0.0/16"),
				Gw:        net.ParseIP("192.168.1.253"),
				Protocol:  unix.RTPROT_BGP,
				Type:      unix.RTN_UNICAST,
				Table:     vrfTable,
			}
			// The kernel derives the local route of an enslaved interface's
			// address in the VRF table itself; on release it maintains it in
			// the local table, so it must not be migrated.
			kernelLocalRoute := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Dst:       ovntest.MustParseIPNet("192.168.1.10/32"),
				Protocol:  unix.RTPROT_KERNEL,
				Scope:     netlink.SCOPE_HOST,
				Type:      unix.RTN_LOCAL,
				Table:     vrfTable,
			}
			nlMock.On("RouteListFiltered", netlink.FAMILY_ALL,
				mock.MatchedBy(func(filter *netlink.Route) bool {
					return filter.Table == vrfTable
				}),
				uint64(netlink.RT_FILTER_TABLE),
			).Return([]netlink.Route{migratedDefaultRoute, migratedDefaultRouteV6, ovnManagedRoute, ovnManagedRouteV6, bgpLearnedRoute, kernelLocalRoute}, nil)
			nlMock.On("LinkSetNoMaster", enslaveLinkMock1).Return(nil)
			nlMock.On("RouteAdd", mock.Anything).Return(nil)

			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			err = c.DeleteVRFSlave(vrfLinkName1, enslaveLinkName1)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			nlMock.AssertCalled(ginkgo.GinkgoT(), "LinkSetNoMaster", enslaveLinkMock1)

			// The migrated routes return to the main table; the OVN managed
			// routes stay with the route manager, the BGP route with its
			// daemon, recognized by their protocol, and the local route with
			// the kernel, recognized by its type.
			expectedRoute := migratedDefaultRoute
			expectedRoute.Table = unix.RT_TABLE_MAIN
			nlMock.AssertCalled(ginkgo.GinkgoT(), "RouteAdd", &expectedRoute)
			expectedRouteV6 := migratedDefaultRouteV6
			expectedRouteV6.Table = unix.RT_TABLE_MAIN
			nlMock.AssertCalled(ginkgo.GinkgoT(), "RouteAdd", &expectedRouteV6)
			nlMock.AssertNumberOfCalls(ginkgo.GinkgoT(), "RouteAdd", 2)
		})

		ginkgo.It("treats a same-key route as equivalent only when interface, gateway and source match", func() {
			// The main-table EEXIST log stays at V(5) only when the route
			// already there is effectively the captured route; a same-key
			// route differing in any forwarding attribute means the captured
			// route is gone from every table.
			existing := netlink.Route{
				LinkIndex: getLinkIndex(enslaveLinkName1),
				Dst:       ovntest.MustParseIPNet("192.168.1.0/24"),
				Src:       net.ParseIP("192.168.1.10"),
				Protocol:  unix.RTPROT_KERNEL,
				Type:      unix.RTN_UNICAST,
				Table:     unix.RT_TABLE_MAIN,
			}
			nlMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return([]netlink.Route{existing}, nil)

			candidate := existing
			gomega.Expect(equivalentRouteExists(candidate)).To(gomega.BeTrue())
			candidate.Src = net.ParseIP("192.168.1.11")
			gomega.Expect(equivalentRouteExists(candidate)).To(gomega.BeFalse(),
				"a different preferred source is not the same route")
			candidate = existing
			candidate.LinkIndex = getLinkIndex(enslaveLinkName2)
			gomega.Expect(equivalentRouteExists(candidate)).To(gomega.BeFalse(),
				"a different output interface is not the same route")
			candidate = existing
			candidate.Gw = net.ParseIP("192.168.1.1")
			gomega.Expect(equivalentRouteExists(candidate)).To(gomega.BeFalse(),
				"a different gateway is not the same route")
		})

		ginkgo.It("releases enslaved interfaces before deleting a VRF, restoring their routes", func() {
			slaveLinkIndex := getLinkIndex(enslaveLinkName1)
			vrfTable := int(getVRFTable(vrfLinkName1))
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkIndex(vrfLinkName1), Index: slaveLinkIndex}, nil)
			nlMock.On("LinkList").Return([]netlink.Link{buildVRF(vrfLinkName1), enslaveLinkMock1}, nil)
			migratedDefaultRoute := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Gw:        net.ParseIP("192.168.1.1"),
				Protocol:  unix.RTPROT_DHCP,
				Type:      unix.RTN_UNICAST,
				Table:     vrfTable,
			}
			nlMock.On("RouteListFiltered", netlink.FAMILY_ALL,
				mock.MatchedBy(func(filter *netlink.Route) bool {
					return filter.Table == vrfTable
				}),
				uint64(netlink.RT_FILTER_TABLE),
			).Return([]netlink.Route{migratedDefaultRoute}, nil)
			nlMock.On("LinkSetNoMaster", enslaveLinkMock1).Return(nil)
			nlMock.On("RouteAdd", mock.Anything).Return(nil)
			nlMock.On("LinkDelete", buildVRF(vrfLinkName1)).Return(nil)

			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			err = c.deleteVRF(buildVRF(vrfLinkName1))
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			nlMock.AssertCalled(ginkgo.GinkgoT(), "LinkSetNoMaster", enslaveLinkMock1)
			expectedRoute := migratedDefaultRoute
			expectedRoute.Table = unix.RT_TABLE_MAIN
			nlMock.AssertCalled(ginkgo.GinkgoT(), "RouteAdd", &expectedRoute)
			nlMock.AssertCalled(ginkgo.GinkgoT(), "LinkDelete", buildVRF(vrfLinkName1))
		})

		ginkgo.It("tags the routes it manages with the OVN-Kubernetes protocol", func() {
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkIndex(vrfLinkName1), Index: getLinkIndex(enslaveLinkName1)}, nil)
			nlMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
			nlMock.On("RouteReplace", mock.Anything).Return(nil)
			managedRoute := netlink.Route{
				LinkIndex: getLinkIndex(enslaveLinkName1),
				Dst:       ovntest.MustParseIPNet("10.96.0.0/16"),
				Gw:        net.ParseIP("169.254.169.4"),
				Table:     int(getVRFTable(vrfLinkName1)),
			}

			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), []netlink.Route{managedRoute})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			// The route entered the kernel and the cache carrying the
			// OVN-Kubernetes protocol, which is what route migration relies
			// on to tell it apart from third-party routes.
			taggedRoute := managedRoute
			taggedRoute.Protocol = netlink.RouteProtocol(types.OVNKProtocol)
			cached := c.vrfs[getLinkIndex(vrfLinkName1)]
			gomega.Expect(cached.routes).To(gomega.ConsistOf(taggedRoute))
			taggedRoute.Priority = 0
			nlMock.AssertCalled(ginkgo.GinkgoT(), "RouteReplace", &taggedRoute)
		})

		ginkgo.It("replaces a tracked route with the same key instead of accumulating it", func() {
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkIndex(vrfLinkName1), Index: getLinkIndex(enslaveLinkName1)}, nil)
			nlMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
			nlMock.On("RouteReplace", mock.Anything).Return(nil)
			defaultVia := func(gateway string) netlink.Route {
				return netlink.Route{
					LinkIndex: getLinkIndex(enslaveLinkName1),
					Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
					Gw:        net.ParseIP(gateway),
					Table:     int(getVRFTable(vrfLinkName1)),
				}
			}
			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), []netlink.Route{defaultVia("192.168.1.1")})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			// Route manager keys by destination, table and metric: the
			// default via the new gateway replaces the tracked one, and
			// re-adding it leaves a single entry.
			for range 2 {
				gomega.Expect(c.AddVRFRoutes(vrfLinkName1, []netlink.Route{defaultVia("192.168.1.2")})).To(gomega.Succeed())
			}
			replacement := defaultVia("192.168.1.2")
			replacement.Protocol = netlink.RouteProtocol(types.OVNKProtocol)
			gomega.Expect(c.vrfs[getLinkIndex(vrfLinkName1)].routes).To(gomega.ConsistOf(replacement))

			// Same-key routes of one batch replace each other as well.
			gomega.Expect(c.AddVRFRoutes(vrfLinkName1, []netlink.Route{defaultVia("192.168.1.3"), defaultVia("192.168.1.4")})).To(gomega.Succeed())
			replacement = defaultVia("192.168.1.4")
			replacement.Protocol = netlink.RouteProtocol(types.OVNKProtocol)
			gomega.Expect(c.vrfs[getLinkIndex(vrfLinkName1)].routes).To(gomega.ConsistOf(replacement))

			// The defaults of the two families are distinct keys; the IPv6
			// one is tracked under the metric the kernel assigns it, so
			// re-adding it with that metric set is not a new route.
			defaultVia6 := func(gateway string, priority int) netlink.Route {
				return netlink.Route{
					LinkIndex: getLinkIndex(enslaveLinkName1),
					Dst:       ovntest.MustParseIPNet("::/0"),
					Gw:        net.ParseIP(gateway),
					Table:     int(getVRFTable(vrfLinkName1)),
					Priority:  priority,
				}
			}
			gomega.Expect(c.AddVRFRoutes(vrfLinkName1, []netlink.Route{defaultVia("192.168.1.4"), defaultVia6("fd00::1", 0)})).To(gomega.Succeed())
			gomega.Expect(c.vrfs[getLinkIndex(vrfLinkName1)].routes).To(gomega.HaveLen(2))
			gomega.Expect(c.AddVRFRoutes(vrfLinkName1, []netlink.Route{defaultVia6("fd00::2", 1024)})).To(gomega.Succeed())
			replacement6 := defaultVia6("fd00::2", 1024)
			replacement6.Protocol = netlink.RouteProtocol(types.OVNKProtocol)
			gomega.Expect(c.vrfs[getLinkIndex(vrfLinkName1)].routes).To(gomega.ConsistOf(replacement, replacement6))
		})

		ginkgo.It("untracks a route deleted by key when the kernel no longer has it", func() {
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkIndex(vrfLinkName1), Index: getLinkIndex(enslaveLinkName1)}, nil)
			nlMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
			nlMock.On("RouteReplace", mock.Anything).Return(nil)
			nlMock.On("RouteDel", mock.Anything).Return(fmt.Errorf("no such process"))
			tracked := netlink.Route{
				LinkIndex: getLinkIndex(enslaveLinkName1),
				Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
				Gw:        net.ParseIP("192.168.1.1"),
				Table:     int(getVRFTable(vrfLinkName1)),
			}
			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), []netlink.Route{tracked})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			// The caller knows the route's key, not the gateway it was
			// installed through, and the kernel reports it as already gone.
			key := tracked
			key.Gw = nil
			gomega.Expect(c.DeleteVRFRoutes(vrfLinkName1, []netlink.Route{key})).To(gomega.Succeed())
			gomega.Expect(c.vrfs[getLinkIndex(vrfLinkName1)].routes).To(gomega.BeEmpty())
		})

		ginkgo.It("keeps the cache intact when syncing a replaced route fails", func() {
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkIndex(vrfLinkName1), Index: getLinkIndex(enslaveLinkName1)}, nil)
			nlMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
			defaultVia := func(gateway string) netlink.Route {
				return netlink.Route{
					LinkIndex: getLinkIndex(enslaveLinkName1),
					Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
					Gw:        net.ParseIP(gateway),
					Table:     int(getVRFTable(vrfLinkName1)),
				}
			}
			// Specific expectations are registered first so they win over
			// the catch-all below.
			nlMock.On("RouteReplace", mock.MatchedBy(func(r *netlink.Route) bool {
				return r.Gw.Equal(net.ParseIP("192.168.1.2"))
			})).Return(fmt.Errorf("replace rejected"))
			nlMock.On("RouteReplace", mock.Anything).Return(nil)
			err := c.AddVRF(vrfLinkName1, enslaveLinkName1, getVRFTable(vrfLinkName1), []netlink.Route{defaultVia("192.168.1.1")})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			gomega.Expect(c.vrfs[getLinkIndex(vrfLinkName1)].routes).To(gomega.HaveLen(1))

			gomega.Expect(c.AddVRFRoutes(vrfLinkName1, []netlink.Route{defaultVia("192.168.1.2")})).NotTo(gomega.Succeed())
			// Only a successful sync writes the entry back.
			previous := defaultVia("192.168.1.1")
			previous.Protocol = netlink.RouteProtocol(types.OVNKProtocol)
			gomega.Expect(c.vrfs[getLinkIndex(vrfLinkName1)].routes).To(gomega.ConsistOf(previous))
		})

		ginkgo.It("does not delete a VRF whose slave release failed", func() {
			slaveLinkIndex := getLinkIndex(enslaveLinkName1)
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkIndex(vrfLinkName1), Index: slaveLinkIndex}, nil)
			nlMock.On("LinkList").Return([]netlink.Link{buildVRF(vrfLinkName1), enslaveLinkMock1}, nil)
			nlMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
			nlMock.On("LinkSetNoMaster", enslaveLinkMock1).Return(fmt.Errorf("operation not permitted"))

			// Deleting the VRF device would purge the routes of a still
			// enslaved interface: a failed release must block the deletion
			// so that a later retry completes the release first.
			err := c.deleteVRF(buildVRF(vrfLinkName1))
			gomega.Expect(err).Should(gomega.MatchError(gomega.ContainSubstring("operation not permitted")))
			nlMock.AssertNotCalled(ginkgo.GinkgoT(), "LinkDelete", mock.Anything)
		})

		ginkgo.It("restores only third-party routes when deleting a VRF unknown to the cache", func() {
			slaveLinkIndex := getLinkIndex(enslaveLinkName1)
			vrfTable := int(getVRFTable(vrfLinkName1))
			enslaveLinkMock1.On("Attrs").Return(&netlink.LinkAttrs{Name: enslaveLinkName1, MasterIndex: getLinkIndex(vrfLinkName1), Index: slaveLinkIndex}, nil)
			nlMock.On("LinkList").Return([]netlink.Link{buildVRF(vrfLinkName1), enslaveLinkMock1}, nil)
			migratedDefaultRoute := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Gw:        net.ParseIP("192.168.1.1"),
				Protocol:  unix.RTPROT_DHCP,
				Type:      unix.RTN_UNICAST,
				Table:     vrfTable,
			}
			ovnManagedRoute := netlink.Route{
				LinkIndex: slaveLinkIndex,
				Dst:       ovntest.MustParseIPNet("10.96.0.0/16"),
				Gw:        net.ParseIP("169.254.169.4"),
				Protocol:  netlink.RouteProtocol(types.OVNKProtocol),
				Type:      unix.RTN_UNICAST,
				Table:     vrfTable,
			}
			nlMock.On("RouteListFiltered", netlink.FAMILY_ALL,
				mock.MatchedBy(func(filter *netlink.Route) bool {
					return filter.Table == vrfTable
				}),
				uint64(netlink.RT_FILTER_TABLE),
			).Return([]netlink.Route{migratedDefaultRoute, ovnManagedRoute}, nil)
			nlMock.On("LinkSetNoMaster", enslaveLinkMock1).Return(nil)
			nlMock.On("RouteAdd", mock.Anything).Return(nil)
			nlMock.On("LinkDelete", buildVRF(vrfLinkName1)).Return(nil)

			// The VRF was never added to the cache (e.g. a stale device found
			// by repair after a restart): the OVN-Kubernetes routes are still
			// recognized by their protocol and are not restored into the main
			// table, while third-party routes are.
			err := c.deleteVRF(buildVRF(vrfLinkName1))
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			nlMock.AssertCalled(ginkgo.GinkgoT(), "LinkSetNoMaster", enslaveLinkMock1)
			expectedRoute := migratedDefaultRoute
			expectedRoute.Table = unix.RT_TABLE_MAIN
			nlMock.AssertCalled(ginkgo.GinkgoT(), "RouteAdd", &expectedRoute)
			nlMock.AssertNumberOfCalls(ginkgo.GinkgoT(), "RouteAdd", 1)
			nlMock.AssertCalled(ginkgo.GinkgoT(), "LinkDelete", buildVRF(vrfLinkName1))
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

		ginkgo.It("uses configured routing table ID start for ownership checks", func() {
			config.OvnKubeNode.RoutingTableIDStart = 2000

			err := c.AddVRF("this.name.is.ok", "other", 1999, nil)
			gomega.Expect(err).Should(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("lower than 2000"))

			err = c.AddVRF(vrfLinkName1, "", 2000, nil)
			gomega.Expect(err).Should(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("not managed by ovn-kubernetes"))

			nlMock.On("LinkList").Return([]netlink.Link{buildVRF(vrfLinkName1), buildVRF(vrfLinkName2)}, nil)
			err = c.Repair(sets.New[string]())
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			nlMock.AssertNotCalled(ginkgo.GinkgoT(), "LinkDelete", buildVRF(vrfLinkName1))
			nlMock.AssertCalled(ginkgo.GinkgoT(), "LinkDelete", buildVRF(vrfLinkName2))
		})

		ginkgo.It("creates the VRF device when it does not exist", func() {
			// AddVRF and sync each look the device up before sync creates it.
			nlMock.On("LinkByName", vrfLinkName4).Return(nil, fmt.Errorf("link not found")).Twice()
			nlMock.On("LinkByName", vrfLinkName4).Return(buildVRF(vrfLinkName4), nil)
			err := c.AddVRF(vrfLinkName4, "", getVRFTable(vrfLinkName4), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			var added []*netlink.Vrf
			for _, call := range nlMock.Calls {
				if call.Method == "LinkAdd" {
					added = append(added, call.Arguments.Get(0).(*netlink.Vrf))
				}
			}
			gomega.Expect(added).To(gomega.HaveLen(1))
			gomega.Expect(added[0].Name).To(gomega.Equal(vrfLinkName4))
			gomega.Expect(added[0].Table).To(gomega.Equal(getVRFTable(vrfLinkName4)))
			gomega.Expect(c.vrfs).To(gomega.HaveKey(getLinkIndex(vrfLinkName4)))
		})

		ginkgo.It("delete VRF", func() {
			nlMock.On("LinkList").Return([]netlink.Link{buildVRF(vrfLinkName2)}, nil)
			err := c.AddVRF(vrfLinkName2, "", getVRFTable(vrfLinkName2), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			err = c.DeleteVRF(vrfLinkName2)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			nlMock.AssertCalled(ginkgo.GinkgoT(), "LinkDelete", buildVRF(vrfLinkName2))
			gomega.Expect(c.vrfs).NotTo(gomega.HaveKey(getLinkIndex(vrfLinkName2)))
		})

		ginkgo.It("deletes requested VRF routes that are not tracked in the cache", func() {
			vrfTable := int(getVRFTable(vrfLinkName1))
			err := c.AddVRF(vrfLinkName1, "", getVRFTable(vrfLinkName1), nil)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

			// Default routes discovered in the kernel, e.g. by their
			// protocol, without a matching entry in the tracked VRF routes,
			// typically after a restart.
			untrackedV4Default := netlink.Route{
				LinkIndex: getLinkIndex(enslaveLinkName1),
				Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
				Gw:        net.ParseIP("192.168.1.1"),
				Table:     vrfTable,
			}
			untrackedV6Default := netlink.Route{
				LinkIndex: getLinkIndex(enslaveLinkName1),
				Dst:       ovntest.MustParseIPNet("::/0"),
				Gw:        net.ParseIP("fd00::1"),
				Table:     vrfTable,
			}
			nlMock.On("RouteDel", mock.Anything).Return(nil)

			err = c.DeleteVRFRoutes(vrfLinkName1, []netlink.Route{untrackedV4Default, untrackedV6Default})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			nlMock.AssertNumberOfCalls(ginkgo.GinkgoT(), "RouteDel", 2)
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
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		var err error
		testNS, err = testutils.NewNS()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		wg = &sync.WaitGroup{}
		stopCh = make(chan struct{})
		routeManager := routemanager.NewController()
		wg.Add(1)
		go func() {
			defer ginkgo.GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				routeManager.Run(stopCh, 2*time.Minute)
				return nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()
		// set vrf manager reconcile period into one second.
		reconcilePeriod = 1 * time.Second
		c = NewController(routeManager)
		wg2 := &sync.WaitGroup{}
		defer func() {
			wg2.Wait()
		}()
		wg2.Add(1)
		go func() {
			defer ginkgo.GinkgoRecover()
			defer wg2.Done()
			err := testNS.Do(func(ns.NetNS) error {
				return c.Run(stopCh, wg)
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()
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

	listDefaultRoutes := func(family, table int) []netlink.Route {
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		var defaultRoutes []netlink.Route
		for _, route := range routes {
			if route.Gw != nil && (route.Dst == nil || route.Dst.IP.IsUnspecified()) {
				defaultRoutes = append(defaultRoutes, route)
			}
		}
		return defaultRoutes
	}
	expectDefaultRoutesIn := func(table int, gateways ...net.IP) {
		for _, gateway := range gateways {
			family := netlink.FAMILY_V4
			if gateway.To4() == nil {
				family = netlink.FAMILY_V6
			}
			defaultRoutes := listDefaultRoutes(family, table)
			gomega.Expect(defaultRoutes).To(gomega.HaveLen(1),
				"expected exactly one default route via %s in table %d, got %v", gateway, table, defaultRoutes)
			gomega.Expect(defaultRoutes[0].Gw.String()).To(gomega.Equal(gateway.String()),
				"expected the default route in table %d to go via %s", table, gateway)
		}
	}

	ovntest.OnSupportedPlatformsIt("preserves slave interface routes across VRF enslavement and release", func() {
		slaveLinkName := "dev100"
		gatewayIP := net.ParseIP("192.168.1.1")
		gatewayIPv6 := net.ParseIP("2001:db8:1::1")
		vrfTable := 1010
		err := testNS.Do(func(ns.NetNS) error {
			defer ginkgo.GinkgoRecover()
			// Set up an interface configured with addresses and default
			// routes of both families, mimicking an interface handed over to
			// us that was configured by another agent, e.g. through DHCP.
			gomega.Expect(netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: slaveLinkName}})).To(gomega.Succeed())
			slaveLink, err := netlink.LinkByName(slaveLinkName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			gomega.Expect(netlink.LinkSetUp(slaveLink)).To(gomega.Succeed())
			// The kernel removes IPv6 addresses on enslavement: mirror the UDN
			// gateway, which preserves them through keep_addr_on_down before
			// enslaving the Uplink gateway interface.
			gomega.Expect(os.WriteFile("/proc/sys/net/ipv6/conf/"+slaveLinkName+"/keep_addr_on_down", []byte("1"), 0644)).To(gomega.Succeed())
			for _, address := range []string{"192.168.1.10/24", "2001:db8:1::10/64"} {
				addr, err := netlink.ParseAddr(address)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				gomega.Expect(netlink.AddrAdd(slaveLink, addr)).To(gomega.Succeed())
			}
			for _, gateway := range []net.IP{gatewayIP, gatewayIPv6} {
				gomega.Expect(netlink.RouteAdd(&netlink.Route{
					LinkIndex: slaveLink.Attrs().Index,
					Gw:        gateway,
				})).To(gomega.Succeed())
			}
			// A static ECMP route through two gateways on the interface: the
			// kernel reports it with a zero top-level link index, so it must
			// be matched through its nexthops.
			ecmpDst := ovntest.MustParseIPNet("172.16.0.0/16")
			ecmpNexthops := []*netlink.NexthopInfo{
				{LinkIndex: slaveLink.Attrs().Index, Gw: net.ParseIP("192.168.1.1")},
				{LinkIndex: slaveLink.Attrs().Index, Gw: net.ParseIP("192.168.1.2")},
			}
			gomega.Expect(netlink.RouteAdd(&netlink.Route{
				Dst:       ecmpDst,
				MultiPath: ecmpNexthops,
			})).To(gomega.Succeed())
			listEcmpRoutes := func(table int) []netlink.Route {
				routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
					&netlink.Route{Table: table, Dst: ecmpDst}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				return routes
			}
			expectEcmpRouteIn := func(table int) {
				routes := listEcmpRoutes(table)
				gomega.Expect(routes).To(gomega.HaveLen(1),
					"expected the ECMP route in table %d, got %v", table, routes)
				gomega.Expect(routes[0].MultiPath).To(gomega.HaveLen(2),
					"expected the ECMP route in table %d to keep both nexthops, got %v", table, routes[0])
			}

			// On enslavement, the default routes and the ECMP route are
			// migrated into the VRF table.
			gomega.Expect(c.AddVRF(vrfLinkName1, slaveLinkName, uint32(vrfTable), nil)).To(gomega.Succeed())
			expectEcmpRouteIn(vrfTable)
			gomega.Expect(listDefaultRoutes(netlink.FAMILY_ALL, unix.RT_TABLE_MAIN)).To(gomega.BeEmpty())
			expectDefaultRoutesIn(vrfTable, gatewayIP, gatewayIPv6)

			// On release, the default routes and the ECMP route are migrated
			// back to the main table.
			gomega.Expect(c.DeleteVRFSlave(vrfLinkName1, slaveLinkName)).To(gomega.Succeed())
			gomega.Expect(listDefaultRoutes(netlink.FAMILY_ALL, vrfTable)).To(gomega.BeEmpty())
			expectDefaultRoutesIn(unix.RT_TABLE_MAIN, gatewayIP, gatewayIPv6)
			gomega.Expect(listEcmpRoutes(vrfTable)).To(gomega.BeEmpty())
			expectEcmpRouteIn(unix.RT_TABLE_MAIN)
			return nil
		})
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	})

	ovntest.OnSupportedPlatformsIt("preserves the prefix routes of noprefixroute addresses across VRF enslavement and release", func() {
		slaveLinkName := "dev200"
		gatewayIP := net.ParseIP("192.168.5.1")
		gatewayIPv6 := net.ParseIP("2001:db8:5::1")
		prefixV4 := ovntest.MustParseIPNet("192.168.5.0/24")
		prefixV6 := ovntest.MustParseIPNet("2001:db8:5::/64")
		vrfTable := 1011
		listPrefixRoutes := func(table int, prefix *net.IPNet) []netlink.Route {
			family := netlink.FAMILY_V4
			if prefix.IP.To4() == nil {
				family = netlink.FAMILY_V6
			}
			routes, err := netlink.RouteListFiltered(family,
				&netlink.Route{Table: table, Dst: prefix}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			return routes
		}
		expectPrefixRouteIn := func(table int, prefix *net.IPNet) {
			routes := listPrefixRoutes(table, prefix)
			gomega.Expect(routes).To(gomega.HaveLen(1),
				"expected the %s prefix route in table %d, got %v", prefix, table, routes)
			gomega.Expect(int(routes[0].Protocol)).To(gomega.Equal(unix.RTPROT_KERNEL),
				"expected the %s prefix route in table %d to keep proto kernel", prefix, table)
		}
		err := testNS.Do(func(ns.NetNS) error {
			defer ginkgo.GinkgoRecover()
			// Mimic an interface managed by NetworkManager: it sets
			// noprefixroute on the addresses it manages and installs their
			// prefix routes itself, tagged proto kernel, so the kernel does
			// not regenerate them after a master change and only the
			// migration can carry them into the VRF table.
			gomega.Expect(netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: slaveLinkName}})).To(gomega.Succeed())
			slaveLink, err := netlink.LinkByName(slaveLinkName)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			gomega.Expect(netlink.LinkSetUp(slaveLink)).To(gomega.Succeed())
			gomega.Expect(os.WriteFile("/proc/sys/net/ipv6/conf/"+slaveLinkName+"/keep_addr_on_down", []byte("1"), 0644)).To(gomega.Succeed())
			for _, address := range []string{"192.168.5.10/24", "2001:db8:5::10/64"} {
				addr, err := netlink.ParseAddr(address)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				addr.Flags = unix.IFA_F_NOPREFIXROUTE
				gomega.Expect(netlink.AddrAdd(slaveLink, addr)).To(gomega.Succeed())
			}
			gomega.Expect(netlink.RouteAdd(&netlink.Route{
				LinkIndex: slaveLink.Attrs().Index,
				Dst:       prefixV4,
				Src:       net.ParseIP("192.168.5.10"),
				Scope:     netlink.SCOPE_LINK,
				Protocol:  unix.RTPROT_KERNEL,
				Type:      unix.RTN_UNICAST,
			})).To(gomega.Succeed())
			gomega.Expect(netlink.RouteAdd(&netlink.Route{
				LinkIndex: slaveLink.Attrs().Index,
				Dst:       prefixV6,
				Protocol:  unix.RTPROT_KERNEL,
				Type:      unix.RTN_UNICAST,
			})).To(gomega.Succeed())
			for _, gateway := range []net.IP{gatewayIP, gatewayIPv6} {
				gomega.Expect(netlink.RouteAdd(&netlink.Route{
					LinkIndex: slaveLink.Attrs().Index,
					Gw:        gateway,
				})).To(gomega.Succeed())
			}

			// On enslavement, the prefix routes migrate into the VRF table
			// along with the default routes. Without them, the default routes
			// would fail their restore against an unreachable gateway and the
			// rollback would undo the enslavement.
			gomega.Expect(c.AddVRF(vrfLinkName1, slaveLinkName, uint32(vrfTable), nil)).To(gomega.Succeed())
			expectPrefixRouteIn(vrfTable, prefixV4)
			expectPrefixRouteIn(vrfTable, prefixV6)
			expectDefaultRoutesIn(vrfTable, gatewayIP, gatewayIPv6)
			gomega.Expect(listPrefixRoutes(unix.RT_TABLE_MAIN, prefixV4)).To(gomega.BeEmpty())
			gomega.Expect(listPrefixRoutes(unix.RT_TABLE_MAIN, prefixV6)).To(gomega.BeEmpty())

			// On release, they all migrate back to the main table.
			gomega.Expect(c.DeleteVRFSlave(vrfLinkName1, slaveLinkName)).To(gomega.Succeed())
			expectPrefixRouteIn(unix.RT_TABLE_MAIN, prefixV4)
			expectPrefixRouteIn(unix.RT_TABLE_MAIN, prefixV6)
			expectDefaultRoutesIn(unix.RT_TABLE_MAIN, gatewayIP, gatewayIPv6)
			gomega.Expect(listPrefixRoutes(vrfTable, prefixV4)).To(gomega.BeEmpty())
			gomega.Expect(listPrefixRoutes(vrfTable, prefixV6)).To(gomega.BeEmpty())

			// The kernel's address-derived entries (local, broadcast, anycast,
			// multicast) lived in the VRF table while the interface was
			// enslaved and belong to the local table afterwards: none of them
			// may be migrated into the main table.
			mainRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL,
				&netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			for _, route := range mainRoutes {
				gomega.Expect(route.Type).To(gomega.Equal(unix.RTN_UNICAST),
					"expected no non-unicast route in the main table after release, got %v", route)
			}
			return nil
		})
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	})

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

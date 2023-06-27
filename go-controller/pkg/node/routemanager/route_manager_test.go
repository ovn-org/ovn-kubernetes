package routemanager

import (
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"
)

var _ = ginkgo.Describe("Route Manager", func() {
	var rm *RouteManager
	var stopCh chan struct{}
	var wg *sync.WaitGroup
	var testNS ns.NetNS
	var loLink netlink.Link
	loMTU := 65520
	loAlternativeMTU := 9000
	loLinkName := "lo"
	loSubnet := &net.IPNet{
		IP:   net.IPv4(127, 1, 0, 0),
		Mask: net.CIDRMask(24, 32),
	}
	altSubnet := &net.IPNet{
		IP:   net.IPv4(10, 10, 0, 0),
		Mask: net.CIDRMask(24, 32),
	}
	loIP := net.IPv4(127, 1, 1, 1)
	loIPDiff := net.IPv4(127, 1, 1, 2)
	loGWIP := net.IPv4(127, 1, 1, 254)

	if os.Getenv("NOROOT") == "TRUE" {
		defer ginkgo.GinkgoRecover()
		ginkgo.Skip("Test requires root privileges")
	}

	ginkgo.BeforeEach(func() {
		var err error
		runtime.LockOSThread()
		testNS, err = testutils.NewNS()
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

		wg = &sync.WaitGroup{}
		stopCh = make(chan struct{})
		syncPeriod := 10 * time.Millisecond
		logAllActivity := true
		rm = NewRouteManager(logAllActivity, syncPeriod)
		err = testNS.Do(func(netNS ns.NetNS) error {
			defer ginkgo.GinkgoRecover()
			loLink, err = netlink.LinkByName(loLinkName)
			if err != nil {
				return err
			}
			if err := netlink.LinkSetUp(loLink); err != nil {
				return err
			}

			loAddr := &netlink.Addr{
				IPNet: loSubnet,
			}
			if err := netlink.AddrAdd(loLink, loAddr); err != nil {
				return err
			}
			route := netlink.Route{LinkIndex: loLink.Attrs().Index, Dst: loSubnet, Src: loIP}
			if err := netlink.RouteAdd(&route); err != nil {
				return err
			}
			return nil
		})

		wg.Add(1)
		go testNS.Do(func(netNS ns.NetNS) error {
			defer wg.Done()
			defer ginkgo.GinkgoRecover()
			rm.Run(stopCh)
			return nil
		})
	})

	ginkgo.AfterEach(func() {
		defer runtime.UnlockOSThread()
		close(stopCh)
		wg.Wait()
		gomega.Expect(testNS.Close()).ShouldNot(gomega.HaveOccurred())
		gomega.Expect(testutils.UnmountNS(testNS)).To(gomega.Succeed())
	})

	ginkgo.Context("del route", func() {
		ginkgo.It("del route", func() {
			r := Route{nil, altSubnet, 0, nil}
			rl := RoutesPerLink{loLink, []Route{r}}
			rm.Add(rl)
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
			rm.Del(rl)
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeFalse())
		})

	})

	ginkgo.Context("add route", func() {
		ginkgo.It("applies route with subnet, gateway IP, src IP, MTU", func() {
			r := Route{loGWIP, loSubnet, loMTU, loIP}
			rl := RoutesPerLink{loLink, []Route{r}}
			rm.Add(rl)
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("applies route with subnets, gateway IP, src IP", func() {
			r := Route{loGWIP, loSubnet, 0, loIP}
			rl := RoutesPerLink{loLink, []Route{r}}
			rm.Add(rl)
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("applies route with subnets, gateway IP", func() {
			r := Route{loGWIP, loSubnet, 0, nil}
			rl := RoutesPerLink{loLink, []Route{r}}
			rm.Add(rl)
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("applies route with subnets", func() {
			r := Route{nil, loSubnet, 0, nil}
			rl := RoutesPerLink{loLink, []Route{r}}
			rm.Add(rl)
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("route exists, has different mtu and is updated", func() {
			// route already exists for default mtu - no need to add it
			r := Route{nil, loSubnet, loAlternativeMTU, nil}
			rl := RoutesPerLink{loLink, []Route{r}}
			rm.Add(rl)
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("route exists, has different src and is updated", func() {
			// route already exists for src ip - no need to add it
			r := Route{nil, loSubnet, 0, loIPDiff}
			rl := RoutesPerLink{loLink, []Route{r}}
			rm.Add(rl)
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
		})
	})

	ginkgo.Context("runtime sync", func() {
		ginkgo.It("reapplies managed route that was removed (gw IP, mtu, src IP)", func() {
			r := Route{loGWIP, loSubnet, loMTU, loIP}
			rm.Add(RoutesPerLink{loLink, []Route{r}})
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
			// clear routes and wait for sync to reapply
			routeList, err := getRouteList(testNS, loLink, netlink.FAMILY_ALL)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			gomega.Expect(len(routeList)).Should(gomega.BeNumerically(">", 0))
			gomega.Expect(deleteRoutes(testNS, routeList...)).ShouldNot(gomega.HaveOccurred())
			// wait for sync to activate since managed routes have been deleted
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("reapplies managed route that was removed (mtu, src IP)", func() {
			r := Route{nil, loSubnet, loMTU, loIP}
			rm.Add(RoutesPerLink{loLink, []Route{r}})
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
			// clear routes and wait for sync to reapply
			routeList, err := getRouteList(testNS, loLink, netlink.FAMILY_ALL)
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
			gomega.Expect(len(routeList)).Should(gomega.BeNumerically(">", 0))
			gomega.Expect(deleteRoutes(testNS, routeList...)).ShouldNot(gomega.HaveOccurred())
			// wait for sync to activate since managed routes have been deleted
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("reapplies managed route that was removed because link is down", func() {
			r := Route{nil, loSubnet, loMTU, loIP}
			rm.Add(RoutesPerLink{loLink, []Route{r}})
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
			gomega.Expect(setLinkDown(testNS, loLink)).ShouldNot(gomega.HaveOccurred())
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeFalse())
			gomega.Expect(setLinkUp(testNS, loLink)).ShouldNot(gomega.HaveOccurred())
			gomega.Eventually(func() bool {
				return doesRouteEntryExist(testNS, loLink, r)
			}, time.Second).Should(gomega.BeTrue())
		})
	})
})

func doesRouteEntryExist(targetNs ns.NetNS, link netlink.Link, reCandidate Route) bool {
	nlRoutesFound, err := getRouteList(targetNs, link, netlink.FAMILY_ALL)
	if err != nil {
		return false
	}

	for _, nlRouteFound := range nlRoutesFound {
		routeFound, err := convertNetlinkRouteToRoutesPerLink(nlRouteFound)
		if err != nil {
			return false
		}
		if len(routeFound.Routes) == 0 {
			return false
		}
		r := routeFound.Routes[0] // always only one RE
		if r.equal(reCandidate) {
			return true
		}
	}
	return false
}

func getRouteList(targetNs ns.NetNS, link netlink.Link, ipFamily int) ([]netlink.Route, error) {
	nlRoutesFound := make([]netlink.Route, 0)
	var err error
	err = targetNs.Do(func(netNS ns.NetNS) error {
		nlRoutesFound, err = netlink.RouteList(link, ipFamily)
		if err != nil {
			return err
		}
		return nil
	})
	return nlRoutesFound, err
}

func deleteRoutes(targetNs ns.NetNS, nlRoutes ...netlink.Route) error {
	var err error
	err = targetNs.Do(func(netNS ns.NetNS) error {
		for _, nlRoute := range nlRoutes {
			if err = netlink.RouteDel(&nlRoute); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

func setLinkUp(targetNS ns.NetNS, link netlink.Link) error {
	return setLink(targetNS, link, netlink.LinkSetUp)
}

func setLinkDown(targetNS ns.NetNS, link netlink.Link) error {
	return setLink(targetNS, link, netlink.LinkSetDown)
}
func setLink(targetNS ns.NetNS, link netlink.Link, nlFunc func(link2 netlink.Link) error) error {
	err := targetNS.Do(func(netNS ns.NetNS) error {
		if err := nlFunc(link); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

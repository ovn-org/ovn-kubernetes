package logicalswitchmanager

import (
	"net"

	"github.com/urfave/cli/v2"
	utilnet "k8s.io/utils/net"

	ipallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

// test function that returns if an IP address is allocated
func (manager *LogicalSwitchManager) isAllocatedIP(switchName, ip string) bool {
	return manager.AllocateIPs(switchName, []*net.IPNet{ovntest.MustParseIPNet(ip)}) == ipallocator.ErrAllocated
}

// AllocateNextIPv4s will allocate the next IPv4 addresses from each of the host subnets
// for a given switch
func (manager *LogicalSwitchManager) AllocateNextIPv4s(switchName string) ([]*net.IPNet, error) {
	ips, err := manager.AllocateNextIPs(switchName)
	if err != nil {
		return nil, err
	}
	var ipv4s []*net.IPNet
	var ipv6s []*net.IPNet
	for _, ip := range ips {
		if utilnet.IsIPv6(ip.IP) {
			ipv6s = append(ipv6s, ip)
		} else {
			ipv4s = append(ipv4s, ip)
		}
	}
	err = manager.ReleaseIPs(switchName, ipv6s)
	if err != nil {
		return nil, err
	}
	return ipv4s, nil
}

type testNodeSubnetData struct {
	switchName string
	subnets    []string //IP subnets in string format e.g. 10.1.1.0/24
}

var _ = ginkgo.Describe("OVN Logical Switch Manager operations", func() {
	var (
		app       *cli.App
		fexec     *ovntest.FakeExec
		lsManager *LogicalSwitchManager
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		lsManager = NewLogicalSwitchManager()
	})

	ginkgo.Context("when adding node", func() {
		ginkgo.It("creates IPAM for each subnet and reserves IPs correctly when HybridOverlay is enabled and address is passed", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				testNode := testNodeSubnetData{
					switchName: "testNode1",
					subnets: []string{
						"10.1.1.0/24",
						"2000::/64",
					},
				}
				err = lsManager.AddOrUpdateSwitch(testNode.switchName, ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				allocatedHybridOverlayDRIP, err := lsManager.AllocateHybridOverlay(testNode.switchName, []string{"10.1.1.53"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(net.ParseIP("10.1.1.53").To4()).To(gomega.Equal(allocatedHybridOverlayDRIP[0].IP))
				gomega.Expect(true).To(gomega.Equal(lsManager.isAllocatedIP(testNode.switchName, "10.1.1.53/32")))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("creates IPAM for each subnet and reserves the .3 address for Hybrid Overlay by default", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				testNode := testNodeSubnetData{
					switchName: "testNode1",
					subnets: []string{
						"10.1.1.0/24",
						"2000::/64",
					},
				}

				err = lsManager.AddOrUpdateSwitch(testNode.switchName, ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				allocatedHybridOverlayDRIP, err := lsManager.AllocateHybridOverlay(testNode.switchName, []string{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(net.ParseIP("10.1.1.3").To4()).To(gomega.Equal(allocatedHybridOverlayDRIP[0].IP))

				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(true).To(gomega.Equal(lsManager.isAllocatedIP(testNode.switchName, "10.1.1.3/32")))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("creates IPAM for each subnet and reserves a non-default IP address for hybrid overlay", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				testNode := testNodeSubnetData{
					switchName: "testNode1",
					subnets: []string{
						"10.1.1.0/24",
						"2000::/64",
					},
				}

				err = lsManager.AddOrUpdateSwitch(testNode.switchName, ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = lsManager.AllocateIPs(testNode.switchName, []*net.IPNet{
					{IP: net.ParseIP("10.1.1.3").To4(), Mask: net.CIDRMask(32, 32)},
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				allocatedHybridOverlayDRIP, err := lsManager.AllocateHybridOverlay(testNode.switchName, []string{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// 10.1.1.4 is the next ip address
				gomega.Expect("10.1.1.4").To(gomega.Equal(allocatedHybridOverlayDRIP[0].IP.String()))

				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(true).To(gomega.Equal(lsManager.isAllocatedIP(testNode.switchName, "10.1.1.3/32")))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})

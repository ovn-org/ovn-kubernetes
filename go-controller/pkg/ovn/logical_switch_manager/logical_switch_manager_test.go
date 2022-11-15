package logicalswitchmanager

import (
	"net"

	"github.com/urfave/cli/v2"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

// test function that returns if an IP address is allocated
func (manager *LogicalSwitchManager) isAllocatedIP(nodeName, ip string) bool {
	manager.RLock()
	defer manager.RUnlock()

	lsi, ok := manager.cache[nodeName]
	if !ok {
		return false
	}
	for _, ipam := range lsi.ipams {
		if ipam.Has(net.ParseIP(ip)) {
			return true
		}
	}
	return false
}

// AllocateNextIPv4s will allocate the next IPv4 addresses from each of the host subnets
// for a given switch
func (manager *LogicalSwitchManager) AllocateNextIPv4s(nodeName string) ([]*net.IPNet, error) {
	ips, err := manager.AllocateNextIPs(nodeName)
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
	err = manager.ReleaseIPs(nodeName, ipv6s)
	if err != nil {
		return nil, err
	}
	return ipv4s, nil
}

type testNodeSubnetData struct {
	nodeName string
	subnets  []string //IP subnets in string format e.g. 10.1.1.0/24
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
		ginkgo.It("creates IPAM for each subnet and reserves IPs correctly", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				testNode := testNodeSubnetData{
					nodeName: "testNode1",
					subnets: []string{
						"10.1.1.0/24",
						"2000::/64",
					},
				}

				expectedIPs := []string{"10.1.1.3", "2000::3"}

				err = lsManager.AddNode(testNode.nodeName, "", ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ips, err := lsManager.AllocateNextIPs(testNode.nodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}

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
					nodeName: "testNode1",
					subnets: []string{
						"10.1.1.0/24",
						"2000::/64",
					},
				}

				err = lsManager.AddNode(testNode.nodeName, "", ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				allocatedHybridOverlayDRIP, err := lsManager.AllocateHybridOverlay(testNode.nodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(net.ParseIP("10.1.1.3").To4()).To(gomega.Equal(allocatedHybridOverlayDRIP[0].IP))

				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(true).To(gomega.Equal(lsManager.isAllocatedIP(testNode.nodeName, "10.1.1.3")))

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
					nodeName: "testNode1",
					subnets: []string{
						"10.1.1.0/24",
						"2000::/64",
					},
				}

				err = lsManager.AddNode(testNode.nodeName, "", ovntest.MustParseIPNets(testNode.subnets...))
				err = lsManager.AllocateIPs(testNode.nodeName, []*net.IPNet{
					{net.ParseIP("10.1.1.3").To4(), net.CIDRMask(32, 32)},
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				allocatedHybridOverlayDRIP, err := lsManager.AllocateHybridOverlay(testNode.nodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// 10.1.1.4 is the next ip address
				gomega.Expect(net.ParseIP("10.1.1.4").To4()).To(gomega.Equal(allocatedHybridOverlayDRIP[0].IP))

				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(true).To(gomega.Equal(lsManager.isAllocatedIP(testNode.nodeName, "10.1.1.3")))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("manages no host subnet nodes correctly", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				testNode := testNodeSubnetData{
					nodeName: "testNode1",
					subnets:  []string{},
				}

				err = lsManager.AddNode(testNode.nodeName, "", ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				noHostSubnet := lsManager.IsNonHostSubnetSwitch(testNode.nodeName)
				gomega.Expect(noHostSubnet).To(gomega.BeTrue())
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("handles updates to the host subnets correctly", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				testNode := testNodeSubnetData{
					nodeName: "testNode1",
					subnets: []string{
						"10.1.1.0/24",
						"2000::/64",
					},
				}

				expectedIPs := []string{"10.1.1.3", "2000::3"}

				err = lsManager.AddNode(testNode.nodeName, "", ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ips, err := lsManager.AllocateNextIPs(testNode.nodeName)
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}
				testNode.subnets = []string{"10.1.2.0/24"}
				expectedIPs = []string{"10.1.2.3"}
				err = lsManager.AddNode(testNode.nodeName, "", ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ips, err = lsManager.AllocateNextIPs(testNode.nodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("when allocating IP addresses", func() {
		ginkgo.It("IPAM for each subnet allocates IPs contiguously", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				testNode := testNodeSubnetData{
					nodeName: "testNode1",
					subnets: []string{
						"10.1.1.0/24",
						"2000::/64",
					},
				}

				expectedIPAllocations := [][]string{
					{"10.1.1.3", "2000::3"},
					{"10.1.1.4", "2000::4"},
				}

				err = lsManager.AddNode(testNode.nodeName, "", ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for _, expectedIPs := range expectedIPAllocations {
					ips, err := lsManager.AllocateNextIPs(testNode.nodeName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					for i, ip := range ips {
						gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
					}
				}
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("IPAM allocates, releases, and reallocates IPs correctly", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				testNode := testNodeSubnetData{
					nodeName: "testNode1",
					subnets: []string{
						"10.1.1.0/24",
					},
				}

				expectedIPAllocations := [][]string{
					{"10.1.1.3"},
					{"10.1.1.4"},
				}

				err = lsManager.AddNode(testNode.nodeName, "", ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for _, expectedIPs := range expectedIPAllocations {
					ips, err := lsManager.AllocateNextIPs(testNode.nodeName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					for i, ip := range ips {
						gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
					}
					err = lsManager.ReleaseIPs(testNode.nodeName, ips)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = lsManager.AllocateIPs(testNode.nodeName, ips)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("releases IPs for other host subnet nodes when any host subnets allocation fails", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				testNode := testNodeSubnetData{
					nodeName: "testNode1",
					subnets: []string{
						"10.1.1.0/24",
						"10.1.2.0/29",
					},
				}
				config.HybridOverlay.Enabled = true
				expectedIPAllocations := [][]string{
					{"10.1.1.3", "10.1.2.3"},
					{"10.1.1.4", "10.1.2.4"},
					{"10.1.1.5", "10.1.2.5"},
				}

				err = lsManager.AddNode(testNode.nodeName, "", ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// exhaust valid ips in second subnet
				for _, expectedIPs := range expectedIPAllocations {
					ips, err := lsManager.AllocateNextIPs(testNode.nodeName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					for i, ip := range ips {
						gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
					}
				}
				ips, err := lsManager.AllocateNextIPv4s(testNode.nodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedIPAllocation := [][]string{
					{"10.1.1.6", "10.1.2.6"},
				}
				for _, expectedIPs := range expectedIPAllocation {
					for i, ip := range ips {
						gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
					}
				}

				// now try one more allocation and expect it to fail
				ips, err = lsManager.AllocateNextIPs(testNode.nodeName)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(len(ips)).To(gomega.Equal(0))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("fails correctly when trying to block a previously allocated IP", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				testNode := testNodeSubnetData{
					nodeName: "testNode1",
					subnets: []string{
						"10.1.1.0/24",
						"2000::/64",
					},
				}

				allocatedIPs := []string{
					"10.1.1.2/24",
					"2000::2/64",
				}
				allocatedIPNets := ovntest.MustParseIPNets(allocatedIPs...)
				err = lsManager.AddNode(testNode.nodeName, "", ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = lsManager.AllocateIPs(testNode.nodeName, allocatedIPNets)
				klog.Errorf("Error: %v", err)
				gomega.Expect(err).To(gomega.HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

})

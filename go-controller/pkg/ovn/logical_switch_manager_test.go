package ovn

import (
	"github.com/urfave/cli/v2"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

type testNodeSubnetData struct {
	nodeName string
	subnets  []string //IP subnets in string format e.g. 10.1.1.0/24
}

var _ = ginkgo.Describe("OVN Logical Switch Manager operations", func() {
	var (
		app       *cli.App
		fexec     *ovntest.FakeExec
		lsManager *logicalSwitchManager
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		lsManager = newLogicalSwitchManager()
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

				err = lsManager.AddNode(testNode.nodeName, ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ips, err := lsManager.AllocateNextIPs(testNode.nodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}

				// run the test for hybrid overlay enabled case
				testHONode := testNodeSubnetData{
					nodeName: "testNode2",
					subnets: []string{
						"10.1.1.0/24",
						"2000::/64",
					},
				}
				config.HybridOverlay.Enabled = true
				expectedIPs = []string{"10.1.1.4", "2000::4"}
				err = lsManager.AddNode(testHONode.nodeName, ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ips, err = lsManager.AllocateNextIPs(testHONode.nodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}

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

				err = lsManager.AddNode(testNode.nodeName, ovntest.MustParseIPNets(testNode.subnets...))
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

				err = lsManager.AddNode(testNode.nodeName, ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ips, err := lsManager.AllocateNextIPs(testNode.nodeName)
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}
				testNode.subnets = []string{"10.1.2.0/24"}
				expectedIPs = []string{"10.1.2.3"}
				err = lsManager.AddNode(testNode.nodeName, ovntest.MustParseIPNets(testNode.subnets...))
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

				err = lsManager.AddNode(testNode.nodeName, ovntest.MustParseIPNets(testNode.subnets...))
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

				err = lsManager.AddNode(testNode.nodeName, ovntest.MustParseIPNets(testNode.subnets...))
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
					{"10.1.1.4", "10.1.2.4"},
					{"10.1.1.5", "10.1.2.5"},
					{"10.1.1.6", "10.1.2.6"},
				}

				err = lsManager.AddNode(testNode.nodeName, ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// exhaust valid ips in second subnet
				for _, expectedIPs := range expectedIPAllocations {
					ips, err := lsManager.AllocateNextIPs(testNode.nodeName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					for i, ip := range ips {
						gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
					}
				}
				// now try one more allocation and expect it to fail
				ips, err := lsManager.AllocateNextIPs(testNode.nodeName)
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
				err = lsManager.AddNode(testNode.nodeName, ovntest.MustParseIPNets(testNode.subnets...))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = lsManager.AllocateIPs(testNode.nodeName, allocatedIPNets)
				klog.Errorf("error: %v", err)
				gomega.Expect(err).To(gomega.HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

})

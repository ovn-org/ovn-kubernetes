package subnet

import (
	"testing"

	ipam "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("Subnet IP allocator operations", func() {
	const subnetName = "subnet1"
	var (
		allocator Allocator
	)

	ginkgo.BeforeEach(func() {
		allocator = NewAllocator()
	})

	ginkgo.Context("when adding subnets", func() {
		ginkgo.It("creates each IPAM and reserves IPs correctly", func() {
			subnets := []string{
				"10.1.1.0/24",
				"2000::/64",
			}

			expectedIPs := []string{"10.1.1.1", "2000::1"}

			err := allocator.AddOrUpdateSubnet(subnetName, ovntest.MustParseIPNets(subnets...))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err := allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for i, ip := range ips {
				gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
			}
		})

		ginkgo.It("handles updates to the subnets correctly", func() {
			subnets := []string{
				"10.1.1.0/24",
				"2000::/64",
			}

			expectedIPs := []string{"10.1.1.1", "2000::1"}

			err := allocator.AddOrUpdateSubnet(subnetName, ovntest.MustParseIPNets(subnets...))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err := allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for i, ip := range ips {
				gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
			}
			subnets = []string{"10.1.2.0/24"}
			expectedIPs = []string{"10.1.2.1"}
			err = allocator.AddOrUpdateSubnet(subnetName, ovntest.MustParseIPNets(subnets...))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err = allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for i, ip := range ips {
				gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
			}
		})

		ginkgo.It("excludes subnets correctly", func() {
			subnets := []string{
				"10.1.1.0/24",
			}
			excludes := []string{
				"10.1.1.0/29",
			}

			expectedIPs := []string{"10.1.1.8"}

			err := allocator.AddOrUpdateSubnet(subnetName, ovntest.MustParseIPNets(subnets...), ovntest.MustParseIPNets(excludes...)...)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err := allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for i, ip := range ips {
				gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
			}
		})

	})

	ginkgo.Context("when allocating IP addresses", func() {
		ginkgo.It("IPAM for each subnet allocates IPs contiguously", func() {
			subnets := []string{
				"10.1.1.0/24",
				"2000::/64",
			}

			expectedIPAllocations := [][]string{
				{"10.1.1.1", "2000::1"},
				{"10.1.1.2", "2000::2"},
			}

			err := allocator.AddOrUpdateSubnet(subnetName, ovntest.MustParseIPNets(subnets...))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, expectedIPs := range expectedIPAllocations {
				ips, err := allocator.AllocateNextIPs(subnetName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}
			}
		})

		ginkgo.It("IPAM allocates, releases, and reallocates IPs correctly", func() {
			subnets := []string{
				"10.1.1.0/24",
			}

			expectedIPAllocations := [][]string{
				{"10.1.1.1"},
				{"10.1.1.2"},
			}

			err := allocator.AddOrUpdateSubnet(subnetName, ovntest.MustParseIPNets(subnets...))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, expectedIPs := range expectedIPAllocations {
				ips, err := allocator.AllocateNextIPs(subnetName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}
				err = allocator.ReleaseIPs(subnetName, ips)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = allocator.AllocateIPPerSubnet(subnetName, ips)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
		})

		ginkgo.It("fails to allocate multiple IPs from the same subnet", func() {
			subnets := []string{"10.1.1.0/24", "2000::/64"}

			gomega.Expect(allocator.AddOrUpdateSubnet(subnetName, ovntest.MustParseIPNets(subnets...))).To(gomega.Succeed())

			ips, err := util.ParseIPNets([]string{"10.1.1.1/24", "10.1.1.2/24"})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(allocator.AllocateIPPerSubnet(subnetName, ips)).To(gomega.MatchError(
				"failed to allocate IP 10.1.1.2 for subnet1: attempted to reserve multiple IPs in the same IPAM instance",
			))
		})

		ginkgo.It("releases IPs for other subnets when any other subnet allocation fails", func() {
			subnets := []string{
				"10.1.1.0/24",
				"10.1.2.0/29",
			}

			expectedIPAllocations := [][]string{
				{"10.1.1.1", "10.1.2.1"},
				{"10.1.1.2", "10.1.2.2"},
				{"10.1.1.3", "10.1.2.3"},
				{"10.1.1.4", "10.1.2.4"},
				{"10.1.1.5", "10.1.2.5"},
			}

			err := allocator.AddOrUpdateSubnet(subnetName, ovntest.MustParseIPNets(subnets...))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// exhaust valid ips in second subnet
			for _, expectedIPs := range expectedIPAllocations {
				ips, err := allocator.AllocateNextIPs(subnetName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}
			}
			ips, err := allocator.AllocateNextIPs(subnetName)
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
			ips, err = allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).To(gomega.MatchError(ipam.ErrFull))
			gomega.Expect(ips).To(gomega.BeEmpty())
		})

		ginkgo.It("fails correctly when trying to block a previously allocated IP", func() {
			subnets := []string{
				"10.1.1.0/24",
				"2000::/64",
			}

			expectedIPs := []string{
				"10.1.1.1/24",
				"2000::1/64",
			}

			err := allocator.AddOrUpdateSubnet(subnetName, ovntest.MustParseIPNets(subnets...))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ips, err := allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for i, ip := range ips {
				gomega.Expect(ip.String()).To(gomega.Equal(expectedIPs[i]))
			}
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = allocator.AllocateIPPerSubnet(subnetName, ovntest.MustParseIPNets(expectedIPs...))
			gomega.Expect(err).To(gomega.MatchError(ipam.ErrAllocated))
		})

	})

})

func TestSubnetIPAllocator(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Subnet IP allocator Operations Suite")
}

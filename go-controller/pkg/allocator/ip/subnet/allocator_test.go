// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package subnet

import (
	"net"
	"sync"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	ipam "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
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

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err := allocator.AllocateNextIPs(subnetName, "test-owner")
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

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err := allocator.AllocateNextIPs(subnetName, "test-owner")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for i, ip := range ips {
				gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
			}
			subnets = []string{"10.1.2.0/24"}
			expectedIPs = []string{"10.1.2.1"}
			err = allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err = allocator.AllocateNextIPs(subnetName, "test-owner")
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

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:           subnetName,
				Subnets:        ovntest.MustParseIPNets(subnets...),
				ExcludeSubnets: ovntest.MustParseIPNets(excludes...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err := allocator.AllocateNextIPs(subnetName, "test-owner")
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

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, expectedIPs := range expectedIPAllocations {
				ips, err := allocator.AllocateNextIPs(subnetName, "test-owner")
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
			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, expectedIPs := range expectedIPAllocations {
				ips, err := allocator.AllocateNextIPs(subnetName, "test-owner")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}
				err = allocator.ReleaseIPs(subnetName, "test-owner", ips)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = allocator.AllocateIPs(subnetName, "test-owner", ips)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
		})

		ginkgo.It("fails to allocate multiple IPs from the same subnet", func() {
			subnets := []string{"10.1.1.0/24", "2000::/64"}

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err := util.ParseIPNets([]string{"10.1.1.1/24", "10.1.1.2/24"})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(allocator.AllocateIPs(subnetName, "test-owner", ips)).To(gomega.MatchError(
				"failed to allocate IP 10.1.1.2 for subnet1: attempted to reserve multiple IPs in the same continuous IPAM instance",
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

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// exhaust valid ips in second subnet
			for _, expectedIPs := range expectedIPAllocations {
				ips, err := allocator.AllocateNextIPs(subnetName, "test-owner")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}
			}
			ips, err := allocator.AllocateNextIPs(subnetName, "test-owner")
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
			ips, err = allocator.AllocateNextIPs(subnetName, "test-owner")
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

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ips, err := allocator.AllocateNextIPs(subnetName, "test-owner")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for i, ip := range ips {
				gomega.Expect(ip.String()).To(gomega.Equal(expectedIPs[i]))
			}
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = allocator.AllocateIPs(subnetName, "test-owner", ovntest.MustParseIPNets(expectedIPs...))
			gomega.Expect(err).To(gomega.MatchError(ipam.ErrAllocated))
		})

		ginkgo.It("fails to allocate IP not contained in any known subnet", func() {
			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets("10.1.1.0/24"),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Try to allocate an IP from a completely different subnet
			outOfRangeIPs := ovntest.MustParseIPNets("10.2.0.50/24")
			err = allocator.AllocateIPs(subnetName, "test-owner", outOfRangeIPs)
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("not contained in any known subnet")))
		})

	})

	// Reserved subnets test cases
	ginkgo.It("reserves subnets correctly and allows specific allocation", func() {
		subnets := []string{
			"10.1.1.0/24",
		}
		reservedSubnets := []string{
			"10.1.1.16/28", // Reserve 10.1.1.16-31
		}
		expectedIP := "10.1.1.1/24"

		err := allocator.AddOrUpdateSubnet(SubnetConfig{
			Name:            subnetName,
			Subnets:         ovntest.MustParseIPNets(subnets...),
			ReservedSubnets: ovntest.MustParseIPNets(reservedSubnets...),
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ips, err := allocator.AllocateNextIPs(subnetName, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ips).To(gomega.HaveLen(1))
		gomega.Expect(ips[0].String()).To(gomega.Equal(expectedIP))

		// Should be able to allocate the first IP from the reserved range
		reservedIPs, err := util.ParseIPNets([]string{"10.1.1.16/24"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = allocator.AllocateIPs(subnetName, "test-owner", reservedIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Should be able to allocate the last IP from the reserved range
		reservedIPs, err = util.ParseIPNets([]string{"10.1.1.31/24"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = allocator.AllocateIPs(subnetName, "test-owner", reservedIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Should not be able to allocate an IP that is already allocated
		err = allocator.AllocateIPs(subnetName, "test-owner", reservedIPs)
		gomega.Expect(err).To(gomega.MatchError(ipam.ErrAllocated))
	})

	ginkgo.It("handles reserved together with exclude subnets correctly", func() {
		subnets := []string{
			"10.1.1.0/24",
		}
		reservedSubnets := []string{
			"10.1.1.0/28",   // Reserve 10.1.1.0-15
			"10.1.1.240/28", // Reserve 10.1.1.240-255
		}
		excludeSubnets := []string{
			"10.1.1.200/29", // Exclude 10.1.1.200-207
		}
		expectedIP := "10.1.1.16/24"

		err := allocator.AddOrUpdateSubnet(SubnetConfig{
			Name:            subnetName,
			Subnets:         ovntest.MustParseIPNets(subnets...),
			ExcludeSubnets:  ovntest.MustParseIPNets(excludeSubnets...),
			ReservedSubnets: ovntest.MustParseIPNets(reservedSubnets...),
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ips, err := allocator.AllocateNextIPs(subnetName, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ips).To(gomega.HaveLen(1))
		gomega.Expect(ips[0].String()).To(gomega.Equal(expectedIP))

		// Should be able to allocate from reserved range but not excluded range
		reservedIPs, err := util.ParseIPNets([]string{"10.1.1.5/24"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = allocator.AllocateIPs(subnetName, "test-owner", reservedIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Should NOT be able to allocate from excluded range
		excludedIPs, err := util.ParseIPNets([]string{"10.1.1.202/24"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = allocator.AllocateIPs(subnetName, "test-owner", excludedIPs)
		gomega.Expect(err).To(gomega.MatchError(ipam.ErrAllocatedByOther))

		// Should not be able to allocate the network IP
		reservedIPs, err = util.ParseIPNets([]string{"10.1.1.0/24"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = allocator.AllocateIPs(subnetName, "test-owner", reservedIPs)
		gomega.Expect(err).To(gomega.HaveOccurred())

		// Should not be able to allocate the broadcast IP
		reservedIPs, err = util.ParseIPNets([]string{"10.1.1.255/24"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = allocator.AllocateIPs(subnetName, "test-owner", reservedIPs)
		gomega.Expect(err).To(gomega.HaveOccurred())
	})

})

func newOwnershipTest(t *testing.T) (*allocator, []*net.IPNet) {
	t.Helper()
	g := gomega.NewWithT(t)
	allocator := NewAllocator()
	g.Expect(allocator.AddOrUpdateSubnet(ownershipSubnetConfig(nil))).To(gomega.Succeed())
	return allocator, ovntest.MustParseIPNets("10.0.0.3/24", "fd00::3/64")
}

func ownershipSubnetConfig(initial map[string][]*net.IPNet) SubnetConfig {
	return SubnetConfig{
		Name:               "node",
		Subnets:            ovntest.MustParseIPNets("10.0.0.0/24", "fd00::/64"),
		ExcludeSubnets:     ovntest.MustParseIPNets("10.0.0.1/32", "10.0.0.2/32", "fd00::1/128", "fd00::2/128"),
		InitialAllocations: initial,
	}
}

func TestIPOwnershipReleaseAndReacquisition(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator, ips := newOwnershipTest(t)
	g.Expect(allocator.AllocateIPs("node", "a", ips)).To(gomega.Succeed())
	alreadyOwned := allocator.AllocateIPs("node", "a", ips)
	g.Expect(alreadyOwned).To(gomega.MatchError(ipam.ErrAllocated))
	g.Expect(alreadyOwned).NotTo(gomega.MatchError(ipam.ErrAllocatedByOther))
	g.Expect(alreadyOwned.Error()).To(gomega.ContainSubstring(`by the same owner "a"`))
	g.Expect(allocator.ReleaseIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.ReleaseIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "b", ips)).To(gomega.Succeed())
	g.Expect(allocator.ReleaseIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("node", "b", ips)).To(gomega.BeTrue())
	err := allocator.AllocateIPs("node", "a", ips)
	g.Expect(err).To(gomega.MatchError(ipam.ErrAllocatedByOther))
	g.Expect(ipam.IsErrAllocated(err)).To(gomega.BeTrue())
	g.Expect(allocator.ReleaseIPs("node", "b", ips)).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.ReleaseIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.cache["node"].owners).To(gomega.BeEmpty())
}

func TestIPOwnershipIsolation(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator, ips := newOwnershipTest(t)
	g.Expect(allocator.AllocateIPs("node", "uid-a/nad-a", ips)).To(gomega.Succeed())
	config := ownershipSubnetConfig(nil)
	config.Name = "other-node"
	g.Expect(allocator.AddOrUpdateSubnet(config)).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("other-node", "uid-b/nad-a", ips)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("node", "uid-a/nad-a", ips)).To(gomega.BeTrue())
	allocator.DeleteSubnet("node")
	g.Expect(allocator.ReleaseIPs("node", "uid-a/nad-a", ips)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("node", "uid-a/nad-a", ips)).To(gomega.BeFalse())
	g.Expect(allocator.OwnsIPs("other-node", "uid-b/nad-a", ips)).To(gomega.BeTrue())
}

func TestIPOwnershipRollback(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator, ips := newOwnershipTest(t)
	g.Expect(allocator.AllocateIPs("node", "b", ips[1:])).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "a", ips)).To(gomega.MatchError(ipam.ErrAllocatedByOther))
	g.Expect(allocator.AllocateIPs("node", "a", ips[:1])).To(gomega.Succeed())
	g.Expect(allocator.ReleaseIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("node", "b", ips[1:])).To(gomega.BeTrue())
	g.Expect(allocator.ReleaseIPs("node", "b", ips)).To(gomega.Succeed())
	badIPs := []*net.IPNet{ips[0], ovntest.MustParseIPNet("fd01::3/64")}
	g.Expect(allocator.AllocateIPs("node", "a", badIPs)).NotTo(gomega.Succeed())
	g.Expect(allocator.cache["node"].owners).To(gomega.BeEmpty())
	g.Expect(allocator.AllocateIPs("node", "b", ips)).To(gomega.Succeed())
}

func TestIPOwnershipConcurrentStaleRelease(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator, _ := newOwnershipTest(t)
	ips, err := allocator.AllocateNextIPs("node", "a")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(allocator.ReleaseIPs("node", "a", ips)).To(gomega.Succeed())
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 20 {
		wg.Go(func() {
			<-start
			for range 50 {
				if err := allocator.ReleaseIPs("node", "a", ips); err != nil {
					t.Errorf("stale release: %v", err)
				}
			}
		})
	}
	close(start)
	g.Expect(allocator.AllocateIPs("node", "b", ips)).To(gomega.Succeed())
	wg.Wait()
	g.Expect(allocator.OwnsIPs("node", "b", ips)).To(gomega.BeTrue())
}

func TestIPOwnershipRejectsPartialReservation(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator, ips := newOwnershipTest(t)
	g.Expect(allocator.AllocateIPs("node", "owner", ips[:1])).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "owner", ips)).NotTo(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("node", "owner", ips[:1])).To(gomega.BeTrue())
	g.Expect(allocator.AllocateIPs("node", "other", ips[1:])).To(gomega.Succeed(), "failed allocation must not reserve the missing family")
}

func TestIPOwnershipOnSubnetInitialization(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator := NewAllocator()
	ips := ovntest.MustParseIPNets("10.0.0.3/24", "fd00::3/64")
	otherIPs := ovntest.MustParseIPNets("10.0.0.4/24", "fd00::4/64")
	initial := map[string][]*net.IPNet{
		"vm-a": append(append([]*net.IPNet{}, ips...), ips...), // Migration pods share a reservation.
		"vm-b": otherIPs,
	}
	for range 2 {
		g.Expect(allocator.AddOrUpdateSubnet(ownershipSubnetConfig(initial))).To(gomega.Succeed())
		g.Expect(allocator.OwnsIPs("node", "vm-a", ips)).To(gomega.BeTrue())
		g.Expect(allocator.OwnsIPs("node", "vm-b", otherIPs)).To(gomega.BeTrue())
		g.Expect(allocator.AllocateIPs("node", "vm-a", ips)).To(gomega.MatchError(ipam.ErrAllocated))
		g.Expect(allocator.AllocateIPs("node", "other", ips)).To(gomega.MatchError(ipam.ErrAllocatedByOther))
		g.Expect(allocator.ReleaseIPs("node", "other", ips)).To(gomega.Succeed())
		allocated, err := allocator.AllocateNextIPs("node", "pod")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(allocated).To(gomega.Equal(ovntest.MustParseIPNets("10.0.0.5/24", "fd00::5/64")))
	}
	g.Expect(allocator.ReleaseIPs("node", "vm-a", ips)).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "other", ips)).To(gomega.Succeed())
	g.Expect(allocator.ReleaseIPs("node", "vm-a", ips)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("node", "other", ips)).To(gomega.BeTrue())

	// Restored allocations must not turn infrastructure exclusions into releasable IPs.
	infrastructure := ovntest.MustParseIPNets("10.0.0.1/24", "fd00::2/64")
	g.Expect(allocator.ReleaseIPs("node", "vm-a", infrastructure)).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "vm-a", infrastructure)).To(gomega.MatchError(ipam.ErrAllocatedByOther))
}

func TestIPOwnershipFailedSubnetInitialization(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial map[string][]*net.IPNet
	}{
		{
			name: "conflicting owners",
			initial: map[string][]*net.IPNet{
				"vm-a": ovntest.MustParseIPNets("10.0.0.3/24", "fd00::3/64"),
				"vm-b": ovntest.MustParseIPNets("10.0.0.4/24", "fd00::3/64"),
			},
		},
		{
			name:    "outside managed subnet",
			initial: map[string][]*net.IPNet{"vm": ovntest.MustParseIPNets("10.0.0.3/24", "fd01::3/64")},
		},
		{
			name:    "excluded IPv4 address",
			initial: map[string][]*net.IPNet{"vm": ovntest.MustParseIPNets("10.0.0.1/24")},
		},
		{
			name:    "excluded IPv6 address",
			initial: map[string][]*net.IPNet{"vm": ovntest.MustParseIPNets("fd00::2/64")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			allocator, ips := newOwnershipTest(t)
			g.Expect(allocator.AllocateIPs("node", "original", ips)).To(gomega.Succeed())
			config := ownershipSubnetConfig(tc.initial)
			err := allocator.AddOrUpdateSubnet(config)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(allocator.OwnsIPs("node", "original", ips)).To(gomega.BeTrue())
			g.Expect(allocator.AllocateIPs("node", "other", ips)).To(gomega.MatchError(ipam.ErrAllocatedByOther))
			g.Expect(allocator.ReleaseIPs("node", "original", ips)).To(gomega.Succeed())
			g.Expect(allocator.AllocateIPs("node", "other", ips)).To(gomega.Succeed())
		})
	}
}

func TestIPOwnershipStaticReservations(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator := NewAllocator()
	ips := ovntest.MustParseIPNets("10.0.0.1/30", "fd00::1/126")
	config := SubnetConfig{
		Name:               "network",
		Subnets:            ovntest.MustParseIPNets("10.0.0.0/30", "fd00::/126"),
		ReservedSubnets:    ovntest.MustParseIPNets("10.0.0.0/30", "fd00::/126"),
		InitialAllocations: map[string][]*net.IPNet{"original": ips},
	}
	g.Expect(allocator.AddOrUpdateSubnet(config)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("network", "original", ips)).To(gomega.BeTrue())
	g.Expect(allocator.AllocateIPs("network", "replacement", ips)).To(gomega.MatchError(ipam.ErrAllocatedByOther))
	g.Expect(allocator.ReleaseIPs("network", "original", ips)).To(gomega.Succeed())
	// Releasing a static reservation must not free the dynamic pool's exclusion.
	_, err := allocator.AllocateNextIPs("network", "dynamic")
	g.Expect(err).To(gomega.MatchError(ipam.ErrFull))
	g.Expect(allocator.AllocateIPs("network", "replacement", ips)).To(gomega.Succeed())
	g.Expect(allocator.ReleaseIPs("network", "original", ips)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("network", "replacement", ips)).To(gomega.BeTrue())
	g.Expect(allocator.ReleaseIPs("network", "replacement", ips)).To(gomega.Succeed())

	badIPs := []*net.IPNet{ips[0], ovntest.MustParseIPNet("fd01::1/126")}
	g.Expect(allocator.AllocateIPs("network", "failed", badIPs)).NotTo(gomega.Succeed())
	g.Expect(allocator.cache["network"].owners).To(gomega.BeEmpty())
	g.Expect(allocator.AllocateIPs("network", "original", ips)).To(gomega.Succeed())
}

func TestIPOwnershipAllocateNextRollback(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator := NewAllocator()
	g.Expect(allocator.AddOrUpdateSubnet(SubnetConfig{
		Name:           "network",
		Subnets:        ovntest.MustParseIPNets("10.0.0.0/30", "fd00::/126"),
		ExcludeSubnets: ovntest.MustParseIPNets("fd00::/126"),
	})).To(gomega.Succeed())
	_, err := allocator.AllocateNextIPs("network", "failed")
	g.Expect(err).To(gomega.MatchError(ipam.ErrFull))
	g.Expect(allocator.cache["network"].owners).To(gomega.BeEmpty())
	// The first family must be available despite failing on the second one.
	for _, ip := range []string{"10.0.0.1/30", "10.0.0.2/30"} {
		g.Expect(allocator.AllocateIPs("network", "other", ovntest.MustParseIPNets(ip))).To(gomega.Succeed())
	}
}

func TestSubnetIPAllocator(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Subnet IP allocator Operations Suite")
}

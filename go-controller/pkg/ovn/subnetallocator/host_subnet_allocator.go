package subnetallocator

import (
	"fmt"
	"net"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
)

type HostSubnetAllocator struct {
	// Don't inherit from BaseSubnetAllocator to ensure users of
	// hostSubnetAllocator can't directly call the underlying methods
	base SubnetAllocator
}

func NewHostSubnetAllocator() *HostSubnetAllocator {
	return &HostSubnetAllocator{
		base: NewSubnetAllocator(),
	}
}

func (sna *HostSubnetAllocator) InitRanges(subnets []config.CIDRNetworkEntry) error {
	for _, entry := range subnets {
		if err := sna.base.AddNetworkRange(entry.CIDR, entry.HostSubnetLength); err != nil {
			return err
		}
		klog.V(5).Infof("Added network range %s to host subnet allocator", entry.CIDR)
	}

	// update metrics for host subnets
	v4count, _, v6count, _ := sna.base.Usage()
	metrics.RecordSubnetCount(float64(v4count), float64(v6count))
	return nil
}

// MarkSubnetsAllocated will mark the given subnets as already allocated by
// the given owner. Marking is all-or-nothing; if marking one of the subnets
// fails then none of them are marked as allocated.
func (sna *HostSubnetAllocator) MarkSubnetsAllocated(nodeName string, subnets ...*net.IPNet) error {
	if err := sna.base.MarkAllocatedNetworks(nodeName, subnets...); err != nil {
		return err
	}
	_, v4used, _, v6used := sna.base.Usage()
	metrics.RecordSubnetUsage(float64(v4used), float64(v6used))
	return nil
}

// AllocateNodeSubnets either validates existing node subnets against the allocators
// ranges, or allocates new subnets if the node doesn't have any yet, or returns an error
func (sna *HostSubnetAllocator) AllocateNodeSubnets(nodeName string, existingSubnets []*net.IPNet, ipv4Mode, ipv6Mode bool) ([]*net.IPNet, []*net.IPNet, error) {
	allocatedSubnets := []*net.IPNet{}

	// OVN can work in single-stack or dual-stack only.
	expectedHostSubnets := 1
	// if dual-stack mode we expect one subnet per each IP family
	if ipv4Mode && ipv6Mode {
		expectedHostSubnets = 2
	}

	klog.Infof("Expected %d subnets on node %s, found %d: %v", expectedHostSubnets, nodeName, len(existingSubnets), existingSubnets)

	// If any existing subnets the node has are valid, mark them as reserved.
	// The node might have invalid or already-reserved subnets, or it might
	// have more subnets than configured in OVN (like for dual-stack to/from
	// single-stack conversion).
	// filter in place slice
	// https://github.com/golang/go/wiki/SliceTricks#filter-in-place
	foundIPv4 := false
	foundIPv6 := false
	n := 0
	for _, subnet := range existingSubnets {
		if (ipv4Mode && utilnet.IsIPv4CIDR(subnet) && !foundIPv4) || (ipv6Mode && utilnet.IsIPv6CIDR(subnet) && !foundIPv6) {
			if err := sna.MarkSubnetsAllocated(nodeName, subnet); err == nil {
				klog.Infof("Valid subnet %v allocated on node %s", subnet, nodeName)
				existingSubnets[n] = subnet
				n++
				if utilnet.IsIPv4CIDR(subnet) {
					foundIPv4 = true
				} else if utilnet.IsIPv6CIDR(subnet) {
					foundIPv6 = true
				}
				continue
			}
		}
		// this subnet is no longer needed; release it
		klog.Infof("Releasing unused or invalid subnet %v on node %s", subnet, nodeName)
		if err := sna.base.ReleaseNetworks(nodeName, subnet); err != nil {
			klog.Warningf("Failed to release subnet %v on node %s: %v", subnet, nodeName, err)
		}
	}
	// recreate existingSubnets with the valid subnets
	existingSubnets = existingSubnets[:n]

	// Node has enough valid subnets already allocated
	if len(existingSubnets) == expectedHostSubnets {
		klog.Infof("Allowed existing subnets %v on node %s", existingSubnets, nodeName)
		return existingSubnets, allocatedSubnets, nil
	}

	// Release allocated subnets on error
	releaseAllocatedSubnets := true
	defer func() {
		if releaseAllocatedSubnets {
			for _, subnet := range allocatedSubnets {
				klog.Warningf("Releasing subnet %v on node %s", subnet, nodeName)
				if errR := sna.base.ReleaseNetworks(nodeName, subnet); errR != nil {
					klog.Warningf("Error releasing subnet %v on node %s: %v", subnet, nodeName, errR)
				}
			}
		}
	}()

	// allocateOneSubnet is a helper to process the result of a subnet allocation
	allocateOneSubnet := func(allocatedHostSubnet *net.IPNet, allocErr error) error {
		if allocErr != nil {
			return fmt.Errorf("error allocating network for node %s: %v", nodeName, allocErr)
		}
		// the allocator returns nil if it can't provide a subnet
		// we should filter them out or they will be appended to the slice
		if allocatedHostSubnet != nil {
			klog.V(5).Infof("Allocating subnet %v on node %s", allocatedHostSubnet, nodeName)
			allocatedSubnets = append(allocatedSubnets, allocatedHostSubnet)
		}
		return nil
	}

	// allocate new subnets if needed
	if ipv4Mode && !foundIPv4 {
		if err := allocateOneSubnet(sna.base.AllocateIPv4Network(nodeName)); err != nil {
			return nil, nil, err
		}
	}
	if ipv6Mode && !foundIPv6 {
		if err := allocateOneSubnet(sna.base.AllocateIPv6Network(nodeName)); err != nil {
			return nil, nil, err
		}
	}

	// check if we were able to allocate the new subnets require
	// this can only happen if OVN is not configured correctly
	// so it will require a reconfiguration and restart.
	wantedSubnets := expectedHostSubnets - len(existingSubnets)
	if wantedSubnets > 0 && len(allocatedSubnets) != wantedSubnets {
		return nil, nil, fmt.Errorf("error allocating networks for node %s: %d subnets expected only new %d subnets allocated",
			nodeName, expectedHostSubnets, len(allocatedSubnets))
	}

	_, v4used, _, v6used := sna.base.Usage()
	metrics.RecordSubnetUsage(float64(v4used), float64(v6used))

	hostSubnets := append(existingSubnets, allocatedSubnets...)
	klog.Infof("Allocated Subnets %v on Node %s", hostSubnets, nodeName)

	// Success; prevent the release-on-error from triggering and return all node subnets
	releaseAllocatedSubnets = false
	return hostSubnets, allocatedSubnets, nil
}

func (sna *HostSubnetAllocator) ReleaseNodeSubnets(nodeName string, subnets ...*net.IPNet) error {
	err := sna.base.ReleaseNetworks(nodeName, subnets...)
	_, v4used, _, v6used := sna.base.Usage()
	metrics.RecordSubnetUsage(float64(v4used), float64(v6used))
	return err
}

func (sna *HostSubnetAllocator) ReleaseAllNodeSubnets(nodeName string) {
	sna.base.ReleaseAllNetworks(nodeName)
	_, v4used, _, v6used := sna.base.Usage()
	metrics.RecordSubnetUsage(float64(v4used), float64(v6used))
}

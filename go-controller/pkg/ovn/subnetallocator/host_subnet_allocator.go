package subnetallocator

import (
	"fmt"
	"net"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
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
	if err := sna.base.MarkAllocatedNetworks(subnets...); err != nil {
		return err
	}
	_, v4used, _, v6used := sna.base.Usage()
	metrics.RecordSubnetUsage(float64(v4used), float64(v6used))
	return nil
}

func (sna *HostSubnetAllocator) AllocateNodeSubnets(node *kapi.Node, ipv4Mode, ipv6Mode bool) ([]*net.IPNet, []*net.IPNet, error) {
	hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
	if err != nil && !util.IsAnnotationNotSetError(err) {
		// Log the error and try to allocate new subnets
		klog.Infof("Failed to get node %s host subnets annotations: %v", node.Name, err)
	}
	allocatedSubnets := []*net.IPNet{}

	// OVN can work in single-stack or dual-stack only.
	currentHostSubnets := len(hostSubnets)
	expectedHostSubnets := 1
	// if dual-stack mode we expect one subnet per each IP family
	if ipv4Mode && ipv6Mode {
		expectedHostSubnets = 2
	}

	// node already has the expected subnets annotated
	// assume IP families match, i.e. no IPv6 config and node annotation IPv4
	if expectedHostSubnets == currentHostSubnets {
		klog.Infof("Allocated Subnets %v on Node %s", hostSubnets, node.Name)
		return hostSubnets, nil, nil
	}

	// Node doesn't have the expected subnets annotated
	// it may happen it has more subnets assigned that configured in OVN
	// like in a dual-stack to single-stack conversion
	// or that it needs to allocate new subnet because it is a new node
	// or has been converted from single-stack to dual-stack
	klog.Infof("Expected %d subnets on node %s, found %d: %v", expectedHostSubnets, node.Name, currentHostSubnets, hostSubnets)
	// release unexpected subnets
	// filter in place slice
	// https://github.com/golang/go/wiki/SliceTricks#filter-in-place
	foundIPv4 := false
	foundIPv6 := false
	n := 0
	for _, subnet := range hostSubnets {
		// if the subnet is not going to be reused release it
		if ipv4Mode && utilnet.IsIPv4CIDR(subnet) && !foundIPv4 {
			klog.V(5).Infof("Valid IPv4 allocated subnet %v on node %s", subnet, node.Name)
			hostSubnets[n] = subnet
			n++
			foundIPv4 = true
			continue
		}
		if ipv6Mode && utilnet.IsIPv6CIDR(subnet) && !foundIPv6 {
			klog.V(5).Infof("Valid IPv6 allocated subnet %v on node %s", subnet, node.Name)
			hostSubnets[n] = subnet
			n++
			foundIPv6 = true
			continue
		}
		// this subnet is no longer needed
		klog.V(5).Infof("Releasing subnet %v on node %s", subnet, node.Name)
		if err := sna.base.ReleaseNetworks(subnet); err != nil {
			klog.Warningf("Error releasing subnet %v on node %s", subnet, node.Name)
		}
	}
	// recreate hostSubnets with the valid subnets
	hostSubnets = hostSubnets[:n]

	// Release allocated subnets on error
	releaseAllocatedSubnets := true
	defer func() {
		if releaseAllocatedSubnets {
			for _, subnet := range allocatedSubnets {
				klog.Warningf("Releasing subnet %v on node %s", subnet, node.Name)
				if errR := sna.base.ReleaseNetworks(subnet); errR != nil {
					klog.Warningf("Error releasing subnet %v on node %s: %v", subnet, node.Name, errR)
				}
			}
		}
	}()

	// allocateOneSubnet is a helper to process the result of a subnet allocation
	allocateOneSubnet := func(allocatedHostSubnet *net.IPNet, allocErr error) error {
		if allocErr != nil {
			return fmt.Errorf("error allocating network for node %s: %v", node.Name, allocErr)
		}
		// the allocator returns nil if it can't provide a subnet
		// we should filter them out or they will be appended to the slice
		if allocatedHostSubnet != nil {
			klog.V(5).Infof("Allocating subnet %v on node %s", allocatedHostSubnet, node.Name)
			allocatedSubnets = append(allocatedSubnets, allocatedHostSubnet)
		}
		return nil
	}

	// allocate new subnets if needed
	if ipv4Mode && !foundIPv4 {
		if err := allocateOneSubnet(sna.base.AllocateIPv4Network()); err != nil {
			return nil, nil, err
		}
	}
	if ipv6Mode && !foundIPv6 {
		if err := allocateOneSubnet(sna.base.AllocateIPv6Network()); err != nil {
			return nil, nil, err
		}
	}

	// check if we were able to allocate the new subnets require
	// this can only happen if OVN is not configured correctly
	// so it will require a reconfiguration and restart.
	wantedSubnets := expectedHostSubnets - currentHostSubnets
	if wantedSubnets > 0 && len(allocatedSubnets) != wantedSubnets {
		return nil, nil, fmt.Errorf("error allocating networks for node %s: %d subnets expected only new %d subnets allocated",
			node.Name, expectedHostSubnets, len(allocatedSubnets))
	}

	_, v4used, _, v6used := sna.base.Usage()
	metrics.RecordSubnetUsage(float64(v4used), float64(v6used))

	hostSubnets = append(hostSubnets, allocatedSubnets...)
	klog.Infof("Allocated Subnets %v on Node %s", hostSubnets, node.Name)

	// Success; prevent the release-on-error from triggering and return all node subnets
	releaseAllocatedSubnets = false
	return hostSubnets, allocatedSubnets, nil
}

func (sna *HostSubnetAllocator) ReleaseNodeSubnets(nodeName string, subnets ...*net.IPNet) error {
	err := sna.base.ReleaseNetworks(subnets...)
	_, v4used, _, v6used := sna.base.Usage()
	metrics.RecordSubnetUsage(float64(v4used), float64(v6used))
	return err
}

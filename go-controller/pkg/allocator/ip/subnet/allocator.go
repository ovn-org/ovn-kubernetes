// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package subnet

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"

	iputils "github.com/containernetworking/plugins/pkg/ip"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	bitmapallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/bitmap"
	ipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// SubnetConfig contains configuration parameters for adding or updating a subnet
type SubnetConfig struct {
	Name            string
	Subnets         []*net.IPNet
	ReservedSubnets []*net.IPNet
	ExcludeSubnets  []*net.IPNet
	// InitialAllocations contains existing IP allocations keyed by owner.
	InitialAllocations map[string][]*net.IPNet
}

// Allocator manages the allocation of IP within specific set of subnets
// identified by a name. Allocator should be threadsafe.
type Allocator interface {
	IPAllocator
	AddOrUpdateSubnet(config SubnetConfig) error
	DeleteSubnet(name string)
	GetSubnets(name string) ([]*net.IPNet, error)
	AllocateUntilFull(name string) error
	OwnsIPs(name, owner string, ips []*net.IPNet) bool
	GetSubnetName(subnets []*net.IPNet) (string, bool)
}

// IPAllocator manages reservations by owner within a named set of subnets.
// AllocateIPs returns ErrAllocated for the owner's existing reservation and
// ErrAllocatedByOther for conflicts. A successful call reserves all supplied IPs.
// Releasing an address held by another owner is a no-op.
type IPAllocator interface {
	AllocateIPs(name, owner string, ips []*net.IPNet) error
	AllocateNextIPs(name, owner string) ([]*net.IPNet, error)
	ReleaseIPs(name, owner string, ips []*net.IPNet) error
}

// ErrSubnetNotFound is used to inform the subnet is not being managed
var ErrSubnetNotFound = errors.New("subnet not found")

// subnetInfo contains information corresponding to the subnet. It holds the
// allocations (v4 and v6) as well as the IPAM allocator instances for each
// of the managed subnets
type subnetInfo struct {
	subnets []*net.IPNet
	owners  map[string]string
	// ipams holds continuous IP allocators for dynamic IP allocation within the managed subnets.
	ipams []ipallocator.ContinuousAllocator
	// staticIPAMs holds static IP allocators for reserved subnets that support static IP allocation (currently only supported for Layer2 primary networks)
	staticIPAMs []ipallocator.StaticAllocator
}

type continuousIPAMFactoryFunc func(*net.IPNet) (ipallocator.ContinuousAllocator, error)
type staticIPAMFactoryFunc func(*net.IPNet) (ipallocator.StaticAllocator, error)

// allocator provides IPAM for different sets of subnets. Each set is
// identified with a subnet name.
type allocator struct {
	cache map[string]subnetInfo
	// A RW mutex which holds subnet information
	sync.RWMutex
	ipamFunc         continuousIPAMFactoryFunc
	reservedIPAMFunc staticIPAMFactoryFunc
}

// newIPAMAllocator provides an ipam interface which can be used for IPAM
// allocations for a given cidr using a contiguous allocation strategy.
// It also pre-allocates certain special subnet IPs such as the .1, .2, and .3
// addresses as reserved.
func newIPAMAllocator(cidr *net.IPNet) (ipallocator.ContinuousAllocator, error) {
	return ipallocator.NewAllocatorCIDRRange(cidr, func(max int, rangeSpec string) (bitmapallocator.Interface, error) {
		return bitmapallocator.NewRoundRobinAllocationMap(max, rangeSpec), nil
	})
}

// newReservedIPAMAllocator provides an ipam interface which can be used for IPAM
// allocations for a given cidr using static IP allocations only. All IPs are available.
func newReservedIPAMAllocator(cidr *net.IPNet) (ipallocator.StaticAllocator, error) {
	return ipallocator.NewAllocatorFullCIDRRange(cidr, func(max int, rangeSpec string) (bitmapallocator.Interface, error) {
		return bitmapallocator.NewRoundRobinAllocationMap(max, rangeSpec), nil
	})
}

// Initializes a new subnet IP allocator
func NewAllocator() *allocator {
	return &allocator{
		cache:            make(map[string]subnetInfo),
		RWMutex:          sync.RWMutex{},
		ipamFunc:         newIPAMAllocator,
		reservedIPAMFunc: newReservedIPAMAllocator,
	}
}

// AddOrUpdateSubnet set to the allocator for IPAM management, or update it.
func (allocator *allocator) AddOrUpdateSubnet(config SubnetConfig) error {
	allocator.Lock()
	defer allocator.Unlock()
	if subnetInfo, ok := allocator.cache[config.Name]; ok && !reflect.DeepEqual(subnetInfo.subnets, config.Subnets) {
		klog.Warningf("Replacing subnets %v with %v for %s", util.StringSlice(subnetInfo.subnets), util.StringSlice(config.Subnets), config.Name)
	}
	var ipams []ipallocator.ContinuousAllocator

	// subnetBoundaryIPs holds network and broadcast addresses for IPv4 subnets.
	// These are automatically excluded from reserved subnet allocators to prevent allocation.
	var subnetBoundaryIPs []net.IP
	for _, subnet := range config.Subnets {
		ipam, err := allocator.ipamFunc(subnet)
		if err != nil {
			return fmt.Errorf("failed to initialize IPAM of subnet %s for %s: %w", subnet, config.Name, err)
		}
		ipams = append(ipams, ipam)

		if utilnet.IsIPv4CIDR(subnet) {
			subnetBoundaryIPs = append(subnetBoundaryIPs, subnet.IP, util.SubnetBroadcastIP(*subnet))
		}
	}

	// reservedSubnets is a subset of subnets, and it should not be used by automatic IPAM
	for _, excludeFromIPAM := range append(config.ReservedSubnets, config.ExcludeSubnets...) {
		var excluded bool
		for i, subnet := range config.Subnets {
			if util.ContainsCIDR(subnet, excludeFromIPAM) {
				err := reserveSubnets(excludeFromIPAM, ipams[i])
				if err != nil {
					return fmt.Errorf("failed to exclude subnet %s for %s: %w", excludeFromIPAM, config.Name, err)
				}
				excluded = true
			}
		}
		if !excluded {
			return fmt.Errorf("failed to exclude subnet %s for %s: not contained in any of the subnets", excludeFromIPAM, config.Name)
		}
	}

	var staticIPAMs []ipallocator.StaticAllocator
	for _, reservedSubnet := range config.ReservedSubnets {
		ipam, err := allocator.reservedIPAMFunc(reservedSubnet)
		if err != nil {
			return fmt.Errorf("failed to initialize IPAM of reserved subnet %s for %s: %w", reservedSubnet, config.Name, err)
		}
		staticIPAMs = append(staticIPAMs, ipam)

		// Exclude network and broadcast addresses from reserved subnet allocators
		for _, excludedSubnetIP := range subnetBoundaryIPs {
			if reservedSubnet.Contains(excludedSubnetIP) {
				if err := ipam.Allocate(excludedSubnetIP); err != nil {
					return fmt.Errorf("failed to exclude %s from reserved subnet allocator %s: %w", excludedSubnetIP, ipam.CIDR(), err)
				}
			}
		}
	}
	info := subnetInfo{
		subnets:     config.Subnets,
		owners:      make(map[string]string),
		ipams:       ipams,
		staticIPAMs: staticIPAMs,
	}
	// Restore allocations before publishing the replacement subnet state.
	for owner, ips := range config.InitialAllocations {
		for _, ip := range ips {
			if err := info.allocateIPs(config.Name, owner, []*net.IPNet{ip}); err != nil &&
				(!ipallocator.IsErrAllocated(err) || errors.Is(err, ipallocator.ErrAllocatedByOther)) {
				return err
			}
		}
	}
	allocator.cache[config.Name] = info
	return nil
}

// DeleteSubnet from the allocator
func (allocator *allocator) DeleteSubnet(name string) {
	allocator.Lock()
	defer allocator.Unlock()
	delete(allocator.cache, name)
}

// GetSubnets of a given subnet set
func (allocator *allocator) GetSubnets(name string) ([]*net.IPNet, error) {
	allocator.RLock()
	defer allocator.RUnlock()
	subnetInfo, ok := allocator.cache[name]
	// make a deep-copy of the underlying slice and return so that there is no
	// resource contention
	if ok {
		subnets := make([]*net.IPNet, len(subnetInfo.subnets))
		for i, subnet := range subnetInfo.subnets {
			subnet := *subnet
			subnets[i] = &subnet
		}
		return subnets, nil
	}
	return nil, ErrSubnetNotFound
}

// AllocateUntilFull used for unit testing only, allocates the rest of the subnet
func (allocator *allocator) AllocateUntilFull(name string) error {
	allocator.Lock()
	defer allocator.Unlock()
	subnetInfo, ok := allocator.cache[name]
	if !ok {
		return fmt.Errorf("failed to allocate IPs for subnet %s: %w", name, ErrSubnetNotFound)
	} else if len(subnetInfo.ipams) == 0 {
		return fmt.Errorf("failed to allocate IPs for subnet %s: has no IPAM", name)
	}
	var err error
	for err != ipallocator.ErrFull {
		for _, ipam := range subnetInfo.ipams {
			_, err = ipam.AllocateNext()
		}
	}
	return nil
}

// AllocateIPs will block off IPs in the ipnets slice as already
// allocated in each of the subnets it manages. ips *must* feature a single IP
// on each of the subnets managed by the allocator.
func (allocator *allocator) AllocateIPs(name, owner string, ips []*net.IPNet) error {
	allocator.Lock()
	defer allocator.Unlock()
	info, ok := allocator.cache[name]
	if !ok {
		return fmt.Errorf("failed to allocate IPs %v for %s: %w", util.StringSlice(ips), name, ErrSubnetNotFound)
	}
	return info.allocateIPs(name, owner, ips)
}

func (subnetInfo *subnetInfo) allocateIPs(name, owner string, ips []*net.IPNet) error {
	if len(ips) == 0 {
		return fmt.Errorf("failed to allocate IPs for %s: no IPs provided", name)
	}
	if len(subnetInfo.ipams) == 0 && len(subnetInfo.staticIPAMs) == 0 {
		return fmt.Errorf("failed to allocate IPs %v for subnet %s: has no IPAM", util.StringSlice(ips), name)
	}
	owned := 0
	for _, ip := range ips {
		current, allocated := subnetInfo.owners[ip.IP.String()]
		if allocated && current != owner {
			return fmt.Errorf("IP %s on subnet %s: %w", ip.IP, name, ipallocator.ErrAllocatedByOther)
		}
		if allocated {
			owned++
		}
	}
	if owned != 0 {
		if owned == len(ips) {
			// Distinguish existing reservations from new ones so callers do not
			// release this owner's existing IPs when rolling back a failed operation.
			return fmt.Errorf("%w %q", ipallocator.ErrAllocated, owner)
		}
		// Mixing old and new reservations would make rollback release both.
		return fmt.Errorf("owner %s already owns only part of requested IPs on subnet %s", owner, name)
	}

	var err error
	allocatedContinuous := make(map[int]*net.IPNet)
	allocatedStatic := make(map[int]*net.IPNet)
	defer func() {
		if err != nil {
			// iterate over range of already allocated indices and release
			// ips allocated before the error occurred.
			for relIdx, relIPNet := range allocatedContinuous {
				subnetInfo.ipams[relIdx].Release(relIPNet.IP)
				if relIPNet.IP != nil {
					klog.Warningf("Continuous IP %s was released for %s", relIPNet.IP, name)
				}
			}
			for relIdx, relIPNet := range allocatedStatic {
				subnetInfo.staticIPAMs[relIdx].Release(relIPNet.IP)
				if relIPNet.IP != nil {
					klog.Warningf("Static IP %s was released for %s", relIPNet.IP, name)
				}
			}
		}
	}()

	for _, ipnet := range ips {
		allocated := false

		// Try static IPAMs first (for reserved subnets)
		for idx, staticIPAM := range subnetInfo.staticIPAMs {
			cidr := staticIPAM.CIDR()
			if cidr.Contains(ipnet.IP) {
				if _, ok := allocatedStatic[idx]; ok {
					err = fmt.Errorf("failed to allocate IP %s for %s: attempted to reserve multiple IPs in the same static IPAM instance", ipnet.IP, name)
					return err
				}

				if err = staticIPAM.Allocate(ipnet.IP); err != nil {
					if ipallocator.IsErrAllocated(err) {
						err = fmt.Errorf("reserved IP %s on subnet %s: %w", ipnet.IP, name, ipallocator.ErrAllocatedByOther)
					}
					return err
				}
				allocatedStatic[idx] = ipnet
				allocated = true
				break
			}
		}

		// If not found in static IPAMs, try continuous IPAMs
		if !allocated {
			for idx, ipam := range subnetInfo.ipams {
				cidr := ipam.CIDR()
				if cidr.Contains(ipnet.IP) {
					if _, ok := allocatedContinuous[idx]; ok {
						err = fmt.Errorf("failed to allocate IP %s for %s: attempted to reserve multiple IPs in the same continuous IPAM instance", ipnet.IP, name)
						return err
					}
					if err = ipam.Allocate(ipnet.IP); err != nil {
						if ipallocator.IsErrAllocated(err) {
							err = fmt.Errorf("reserved IP %s on subnet %s: %w", ipnet.IP, name, ipallocator.ErrAllocatedByOther)
						}
						return err
					}
					allocatedContinuous[idx] = ipnet
					allocated = true
					break
				}
			}
		}

		if !allocated {
			err = fmt.Errorf("failed to allocate IP %s for %s: not contained in any known subnet", ipnet.IP, name)
			return err
		}
	}
	for _, ip := range ips {
		subnetInfo.owners[ip.IP.String()] = owner
	}
	return nil
}

// reserveSubnets reserves subnet IPs
func reserveSubnets(subnet *net.IPNet, ipam ipallocator.ContinuousAllocator) error {
	// FIXME: allocate IP ranges when https://github.com/ovn-kubernetes/ovn-kubernetes/issues/3369 is fixed
	for ip := subnet.IP; subnet.Contains(ip); ip = iputils.NextIP(ip) {
		if ipam.Reserved(ip) {
			continue
		}
		err := ipam.Allocate(ip)
		if err != nil {
			return fmt.Errorf("failed to reserve IP %s: %w", ip, err)
		}
	}
	return nil
}

// AllocateNextIPs allocates IP addresses from the given subnet set
func (allocator *allocator) AllocateNextIPs(name, owner string) ([]*net.IPNet, error) {
	allocator.Lock()
	defer allocator.Unlock()
	var ipnets []*net.IPNet
	var ip net.IP
	var err error
	subnetInfo, ok := allocator.cache[name]

	if !ok {
		return nil, fmt.Errorf("failed to allocate new IPs for %s: %w", name, ErrSubnetNotFound)
	}

	if len(subnetInfo.ipams) == 0 {
		return nil, fmt.Errorf("failed to allocate new IPs for %s: has no IPAM", name)
	}

	if len(subnetInfo.ipams) != len(subnetInfo.subnets) {
		return nil, fmt.Errorf("failed to allocate new IPs for %s: number of subnets %d"+
			" don't match number of ipam instances %d", name, len(subnetInfo.subnets), len(subnetInfo.ipams))
	}

	defer func() {
		if err != nil {
			// iterate over range of already allocated indices and release
			// ips allocated before the error occurred.
			for relIdx, relIPNet := range ipnets {
				subnetInfo.ipams[relIdx].Release(relIPNet.IP)
				if relIPNet.IP != nil {
					klog.Warningf("Reserved IP %s was released for %s", relIPNet.IP, name)
				}
			}
		}
	}()

	for idx, ipam := range subnetInfo.ipams {
		ip, err = ipam.AllocateNext()
		if err != nil {
			if errors.Is(err, ipallocator.ErrFull) {
				err = fmt.Errorf("failed to allocate new IPs for %s: %w", name, ipallocator.ErrFull)
			}
			return nil, err
		}
		ipnet := &net.IPNet{
			IP:   ip,
			Mask: subnetInfo.subnets[idx].Mask,
		}
		ipnets = append(ipnets, ipnet)
	}
	for _, ip := range ipnets {
		subnetInfo.owners[ip.IP.String()] = owner
	}
	return ipnets, nil
}

// ReleaseIPs releases only addresses still held by owner.
func (allocator *allocator) ReleaseIPs(name, owner string, ips []*net.IPNet) error {
	allocator.Lock()
	defer allocator.Unlock()
	subnetInfo, ok := allocator.cache[name]
	if !ok {
		return nil
	}

	for _, ipnet := range ips {
		key := ipnet.IP.String()
		if current, allocated := subnetInfo.owners[key]; !allocated || current != owner {
			continue
		}
		delete(subnetInfo.owners, key)
		released := false
		for _, ipam := range subnetInfo.staticIPAMs {
			cidr := ipam.CIDR()
			if cidr.Contains(ipnet.IP) {
				ipam.Release(ipnet.IP)
				released = true
				break
			}
		}
		// Continue if the IP was released already
		if released {
			continue
		}

		for _, ipam := range subnetInfo.ipams {
			cidr := ipam.CIDR()
			if cidr.Contains(ipnet.IP) {
				ipam.Release(ipnet.IP)
				break
			}
		}
	}
	return nil
}

// OwnsIPs reports whether owner holds every requested address.
func (allocator *allocator) OwnsIPs(name, owner string, ips []*net.IPNet) bool {
	allocator.RLock()
	defer allocator.RUnlock()
	info := allocator.cache[name]
	for _, ip := range ips {
		if current, allocated := info.owners[ip.IP.String()]; !allocated || current != owner {
			return false
		}
	}
	return len(ips) != 0
}

// GetSubnetName will find the switch that contains one of the subnets
// from "subnets" if not it will return "", false
func (allocator *allocator) GetSubnetName(subnets []*net.IPNet) (string, bool) {
	allocator.RLock()
	defer allocator.RUnlock()
	for _, subnet := range subnets {
		for switchName, lsInfo := range allocator.cache {
			for _, ipam := range lsInfo.staticIPAMs {
				ipamCIDR := ipam.CIDR()
				if ipamCIDR.Contains(subnet.IP) {
					return switchName, true
				}
			}

			for _, ipam := range lsInfo.ipams {
				ipamCIDR := ipam.CIDR()
				if ipamCIDR.Contains(subnet.IP) {
					return switchName, true
				}
			}
		}
	}
	return "", false
}

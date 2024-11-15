package subnet

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"

	iputils "github.com/containernetworking/plugins/pkg/ip"
	bitmapallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/bitmap"
	ipallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog/v2"
)

// Allocator manages the allocation of IP within specific set of subnets
// identified by a name. Allocator should be threadsafe.
type Allocator interface {
	AddOrUpdateSubnet(name string, subnets []*net.IPNet, excludeSubnets ...*net.IPNet) error
	DeleteSubnet(name string)
	GetSubnets(name string) ([]*net.IPNet, error)
	AllocateUntilFull(name string) error
	AllocateIPPerSubnet(name string, ips []*net.IPNet) error
	AllocateNextIPs(name string) ([]*net.IPNet, error)
	ReleaseIPs(name string, ips []*net.IPNet) error
	ConditionalIPRelease(name string, ips []*net.IPNet, predicate func() (bool, error)) (bool, error)
	ForSubnet(name string) NamedAllocator
	GetSubnetName(subnets []*net.IPNet) (string, bool)
}

// NamedAllocator manages the allocation of IPs within a specific subnet
type NamedAllocator interface {
	AllocateIPs(ips []*net.IPNet) error
	AllocateNextIPs() ([]*net.IPNet, error)
	ReleaseIPs(ips []*net.IPNet) error
}

// ErrSubnetNotFound is used to inform the subnet is not being managed
var ErrSubnetNotFound = errors.New("subnet not found")

// subnetInfo contains information corresponding to the subnet. It holds the
// allocations (v4 and v6) as well as the IPAM allocator instances for each
// of the managed subnets
type subnetInfo struct {
	subnets []*net.IPNet
	ipams   []ipallocator.Interface
}

type ipamFactoryFunc func(*net.IPNet) (ipallocator.Interface, error)

// allocator provides IPAM for different sets of subnets. Each set is
// identified with a subnet name.
type allocator struct {
	cache map[string]subnetInfo
	// A RW mutex which holds subnet information
	sync.RWMutex
	ipamFunc ipamFactoryFunc
}

// newIPAMAllocator provides an ipam interface which can be used for IPAM
// allocations for a given cidr using a contiguous allocation strategy.
// It also pre-allocates certain special subnet IPs such as the .1, .2, and .3
// addresses as reserved.
func newIPAMAllocator(cidr *net.IPNet) (ipallocator.Interface, error) {
	return ipallocator.NewAllocatorCIDRRange(cidr, func(max int, rangeSpec string) (bitmapallocator.Interface, error) {
		return bitmapallocator.NewRoundRobinAllocationMap(max, rangeSpec), nil
	})
}

// Initializes a new subnet IP allocator
func NewAllocator() *allocator {
	return &allocator{
		cache:    make(map[string]subnetInfo),
		RWMutex:  sync.RWMutex{},
		ipamFunc: newIPAMAllocator,
	}
}

// AddOrUpdateSubnet set to the allocator for IPAM management, or update it.
func (allocator *allocator) AddOrUpdateSubnet(name string, subnets []*net.IPNet, excludeSubnets ...*net.IPNet) error {
	allocator.Lock()
	defer allocator.Unlock()
	if subnetInfo, ok := allocator.cache[name]; ok && !reflect.DeepEqual(subnetInfo.subnets, subnets) {
		klog.Warningf("Replacing subnets %v with %v for %s", util.StringSlice(subnetInfo.subnets), util.StringSlice(subnets), name)
	}
	var ipams []ipallocator.Interface
	for _, subnet := range subnets {
		ipam, err := allocator.ipamFunc(subnet)
		if err != nil {
			return fmt.Errorf("failed to initialize IPAM of subnet %s for %s: %w", subnet, name, err)
		}
		ipams = append(ipams, ipam)
	}
	allocator.cache[name] = subnetInfo{
		subnets: subnets,
		ipams:   ipams,
	}

	for _, excludeSubnet := range excludeSubnets {
		var excluded bool
		for i, subnet := range subnets {
			if util.ContainsCIDR(subnet, excludeSubnet) {
				err := reserveSubnets(excludeSubnet, ipams[i])
				if err != nil {
					return fmt.Errorf("failed to exclude subnet %s for %s: %w", excludeSubnet, name, err)
				}
			}
			excluded = true
		}
		if !excluded {
			return fmt.Errorf("failed to exclude subnet %s for %s: not contained in any of the subnets", excludeSubnet, name)
		}
	}
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
	allocator.RLock()
	defer allocator.RUnlock()
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

// AllocateIPPerSubnet will block off IPs in the ipnets slice as already
// allocated in each of the subnets it manages. ips *must* feature a single IP
// on each of the subnets managed by the allocator.
func (allocator *allocator) AllocateIPPerSubnet(name string, ips []*net.IPNet) error {
	if len(ips) == 0 {
		return fmt.Errorf("failed to allocate IPs for %s: no IPs provided", name)
	}
	allocator.RLock()
	defer allocator.RUnlock()
	subnetInfo, ok := allocator.cache[name]
	if !ok {
		return fmt.Errorf("failed to allocate IPs %v for %s: %w", util.StringSlice(ips), name, ErrSubnetNotFound)
	} else if len(subnetInfo.ipams) == 0 {
		return fmt.Errorf("failed to allocate IPs %v for subnet %s: has no IPAM", util.StringSlice(ips), name)
	}

	var err error
	allocated := make(map[int]*net.IPNet)
	defer func() {
		if err != nil {
			// iterate over range of already allocated indices and release
			// ips allocated before the error occurred.
			for relIdx, relIPNet := range allocated {
				subnetInfo.ipams[relIdx].Release(relIPNet.IP)
				if relIPNet.IP != nil {
					klog.Warningf("Reserved IP %s was released for %s", relIPNet.IP, name)
				}
			}
		}
	}()

	for _, ipnet := range ips {
		for idx, ipam := range subnetInfo.ipams {
			cidr := ipam.CIDR()
			if cidr.Contains(ipnet.IP) {
				if _, ok = allocated[idx]; ok {
					err = fmt.Errorf("failed to allocate IP %s for %s: attempted to reserve multiple IPs in the same IPAM instance", ipnet.IP, name)
					return err
				}
				if err = ipam.Allocate(ipnet.IP); err != nil {
					return err
				}
				allocated[idx] = ipnet
				break
			}
		}
	}
	return nil
}

// reserveSubnets reserves subnet IPs
func reserveSubnets(subnet *net.IPNet, ipam ipallocator.Interface) error {
	// FIXME: allocate IP ranges when https://github.com/ovn-org/ovn-kubernetes/issues/3369 is fixed
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
func (allocator *allocator) AllocateNextIPs(name string) ([]*net.IPNet, error) {
	allocator.RLock()
	defer allocator.RUnlock()
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
			return nil, err
		}
		ipnet := &net.IPNet{
			IP:   ip,
			Mask: subnetInfo.subnets[idx].Mask,
		}
		ipnets = append(ipnets, ipnet)
	}
	return ipnets, nil
}

// ReleaseIPs marks the IPs in ipnets slice as available for allocation by
// releasing them from the IPAM pool of allocated IPs of the given subnet set.
// If there aren't IPs to release the method does not return an error.
func (allocator *allocator) ReleaseIPs(name string, ips []*net.IPNet) error {
	allocator.RLock()
	defer allocator.RUnlock()
	if ips == nil || name == "" {
		return nil
	}
	subnetInfo, ok := allocator.cache[name]
	if !ok {
		return fmt.Errorf("failed to release ips for %s: %w", name, ErrSubnetNotFound)
	}

	for _, ipnet := range ips {
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

// ConditionalIPRelease determines if any IP is available to be released from an IPAM conditionally if func is true.
// It guarantees state of the allocator will not change while executing the predicate function
// TODO(trozet): add unit testing for this function
func (allocator *allocator) ConditionalIPRelease(name string, ips []*net.IPNet, predicate func() (bool, error)) (bool, error) {
	allocator.RLock()
	defer allocator.RUnlock()
	if ips == nil || name == "" {
		return false, nil
	}
	subnetInfo, ok := allocator.cache[name]
	if !ok {
		return false, nil
	}
	if len(subnetInfo.ipams) == 0 {
		return false, nil
	}

	// check if ipam has one of the ip addresses, and then execute the predicate function to determine
	// if this IP should be released or not
	for _, ipnet := range ips {
		for _, ipam := range subnetInfo.ipams {
			cidr := ipam.CIDR()
			if cidr.Contains(ipnet.IP) {
				if ipam.Has(ipnet.IP) {
					return predicate()
				}
			}
		}
	}

	return false, nil
}

// ForSubnet returns an IP allocator for the specified subnet
func (allocator *allocator) ForSubnet(name string) NamedAllocator {
	return &IPAllocator{
		name:      name,
		allocator: allocator,
	}
}

// GetSubnetName will find the switch that contains one of the subnets
// from "subnets" if not it will return "", false
func (allocator *allocator) GetSubnetName(subnets []*net.IPNet) (string, bool) {
	allocator.RLock()
	defer allocator.RUnlock()
	for _, subnet := range subnets {
		for switchName, lsInfo := range allocator.cache {
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

type IPAllocator struct {
	allocator *allocator
	name      string
}

// AllocateIPs allocates the requested IPs
func (ipAllocator *IPAllocator) AllocateIPs(ips []*net.IPNet) error {
	return ipAllocator.allocator.AllocateIPPerSubnet(ipAllocator.name, ips)
}

// AllocateNextIPs allocates the next available IPs
func (ipAllocator *IPAllocator) AllocateNextIPs() ([]*net.IPNet, error) {
	return ipAllocator.allocator.AllocateNextIPs(ipAllocator.name)
}

// ReleaseIPs release the provided IPs
func (ipAllocator *IPAllocator) ReleaseIPs(ips []*net.IPNet) error {
	return ipAllocator.allocator.ReleaseIPs(ipAllocator.name, ips)
}

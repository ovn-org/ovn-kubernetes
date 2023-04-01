package logicalswitchmanager

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"reflect"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator"
	ipam "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator/allocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// SwitchNotFound is used to inform the logical switch was not found in the cache
var SwitchNotFound = errors.New("switch not found")

// logicalSwitchInfo contains information corresponding to the switch. It holds the
// subnet allocations (v4 and v6) as well as the IPAM allocator instances for each
// subnet managed for this switch
type logicalSwitchInfo struct {
	hostSubnets  []*net.IPNet
	ipams        []ipam.Interface
	noHostSubnet bool
	// the uuid of the logicalSwitch described by this struct
	uuid string
}

type ipamFactoryFunc func(*net.IPNet) (ipam.Interface, error)

// LogicalSwitchManager provides switch info management APIs including IPAM for the host subnets
type LogicalSwitchManager struct {
	cache map[string]logicalSwitchInfo
	// A RW mutex for LogicalSwitchManager which holds logicalSwitch information
	sync.RWMutex
	ipamFunc ipamFactoryFunc
}

// GetUUID returns the UUID for the given logical switch name if
func (manager *LogicalSwitchManager) GetUUID(switchName string) (string, bool) {
	manager.RLock()
	defer manager.RUnlock()
	if _, ok := manager.cache[switchName]; !ok {
		return "", ok
	}
	return manager.cache[switchName].uuid, true
}

// NewIPAMAllocator provides an ipam interface which can be used for IPAM
// allocations for a given cidr using a contiguous allocation strategy.
// It also pre-allocates certain special subnet IPs such as the .1, .2, and .3
// addresses as reserved.
func NewIPAMAllocator(cidr *net.IPNet) (ipam.Interface, error) {
	subnetRange, err := ipam.NewAllocatorCIDRRange(cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewRoundRobinAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return nil, err
	}
	if err := reserveIPs(cidr, subnetRange); err != nil {
		klog.Errorf("Failed reserving IPs for subnet %s, err: %v", cidr, err)
		return nil, err
	}
	return subnetRange, nil
}

// Helper function to reserve certain subnet IPs as special
// These are the .1, .2 and .3 addresses in particular
func reserveIPs(subnet *net.IPNet, ipam ipam.Interface) error {
	gwIfAddr := util.GetNodeGatewayIfAddr(subnet)
	err := ipam.Allocate(gwIfAddr.IP)
	if err != nil {
		klog.Errorf("Unable to allocate subnet's gateway IP: %s", gwIfAddr.IP)
		return err
	}
	mgmtIfAddr := util.GetNodeManagementIfAddr(subnet)
	err = ipam.Allocate(mgmtIfAddr.IP)
	if err != nil {
		klog.Errorf("Unable to allocate subnet's management IP: %s", mgmtIfAddr.IP)
		return err
	}
	return nil
}

// Initializes a new logical switch manager
func NewLogicalSwitchManager() *LogicalSwitchManager {
	return &LogicalSwitchManager{
		cache:    make(map[string]logicalSwitchInfo),
		RWMutex:  sync.RWMutex{},
		ipamFunc: NewIPAMAllocator,
	}
}

// AddSwitch adds/updates a switch to the logical switch manager for subnet
// and IPAM management.
func (manager *LogicalSwitchManager) AddSwitch(switchName, uuid string, hostSubnets []*net.IPNet) error {
	manager.Lock()
	defer manager.Unlock()
	if lsi, ok := manager.cache[switchName]; ok && !reflect.DeepEqual(lsi.hostSubnets, hostSubnets) {
		klog.Warningf("Logical switch %s already in cache with subnet %s; replacing with %s", switchName,
			util.JoinIPNets(lsi.hostSubnets, ","), util.JoinIPNets(hostSubnets, ","))
	}
	var ipams []ipam.Interface
	for _, subnet := range hostSubnets {
		ipam, err := manager.ipamFunc(subnet)
		if err != nil {
			klog.Errorf("IPAM for subnet %s was not initialized for switch %q", subnet, switchName)
			return err
		}
		ipams = append(ipams, ipam)
	}
	manager.cache[switchName] = logicalSwitchInfo{
		hostSubnets:  hostSubnets,
		ipams:        ipams,
		noHostSubnet: len(hostSubnets) == 0,
		uuid:         uuid,
	}

	return nil
}

// AddNoHostSubnetSwitch adds/updates a switch without any host subnets
// to the logical switch manager
func (manager *LogicalSwitchManager) AddNoHostSubnetSwitch(switchName string) error {
	// setting the hostSubnets slice argument to nil in the cache means an object
	// exists for the switch but it was not assigned a hostSubnet by ovn-kubernetes
	// this will be true for switches created on nodes that are marked as host-subnet only.
	return manager.AddSwitch(switchName, "", nil)
}

// Remove a switch from the the logical switch manager
func (manager *LogicalSwitchManager) DeleteSwitch(switchName string) {
	manager.Lock()
	defer manager.Unlock()
	delete(manager.cache, switchName)
}

// Given a switch name, checks if the switch is a noHostSubnet switch
func (manager *LogicalSwitchManager) IsNonHostSubnetSwitch(switchName string) bool {
	manager.RLock()
	defer manager.RUnlock()
	lsi, ok := manager.cache[switchName]
	return ok && lsi.noHostSubnet
}

// Given a switch name, get all its host-subnets
func (manager *LogicalSwitchManager) GetSwitchSubnets(switchName string) []*net.IPNet {
	manager.RLock()
	defer manager.RUnlock()
	lsi, ok := manager.cache[switchName]
	// make a deep-copy of the underlying slice and return so that there is no
	// resource contention
	if ok && len(lsi.hostSubnets) > 0 {
		subnets := make([]*net.IPNet, len(lsi.hostSubnets))
		for i, hsn := range lsi.hostSubnets {
			subnet := *hsn
			subnets[i] = &subnet
		}
		return subnets
	}
	return nil
}

// AllocateUntilFull used for unit testing only, allocates the rest of the switch subnet
func (manager *LogicalSwitchManager) AllocateUntilFull(switchName string) error {
	manager.RLock()
	defer manager.RUnlock()
	lsi, ok := manager.cache[switchName]
	if !ok {
		return fmt.Errorf("unable to allocate IPs for switch: %s: %w", switchName, SwitchNotFound)
	} else if len(lsi.ipams) == 0 {
		return fmt.Errorf("unable to allocate IPs for switch: %s because logical switch manager has no IPAM", switchName)
	}
	var err error
	for err != ipam.ErrFull {
		for _, ipam := range lsi.ipams {
			_, err = ipam.AllocateNext()
		}
	}
	return nil
}

// AllocateIPs will block off IPs in the ipnets slice as already allocated
// for a given switch
func (manager *LogicalSwitchManager) AllocateIPs(switchName string, ipnets []*net.IPNet) error {
	if len(ipnets) == 0 {
		return fmt.Errorf("unable to allocate empty IPs")
	}
	manager.RLock()
	defer manager.RUnlock()
	lsi, ok := manager.cache[switchName]
	if !ok {
		return fmt.Errorf("unable to allocate IPs: %v for switch %s: %w", ipnets, switchName, SwitchNotFound)
	} else if len(lsi.ipams) == 0 {
		return fmt.Errorf("unable to allocate IPs: %v for switch: %s: logical switch manager has no IPAM",
			ipnets, switchName)

	}

	var err error
	allocated := make(map[int]*net.IPNet)
	defer func() {
		if err != nil {
			// iterate over range of already allocated indices and release
			// ips allocated before the error occurred.
			for relIdx, relIPNet := range allocated {
				lsi.ipams[relIdx].Release(relIPNet.IP)
				if relIPNet.IP != nil {
					klog.Warningf("Reserved IP: %s was released", relIPNet.IP.String())
				}
			}
		}
	}()

	for _, ipnet := range ipnets {
		for idx, ipam := range lsi.ipams {
			cidr := ipam.CIDR()
			if cidr.Contains(ipnet.IP) {
				if _, ok = allocated[idx]; ok {
					err = fmt.Errorf("error attempting to reserve multiple IPs in the same IPAM instance")
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

// AllocateNextIPs allocates IP addresses from each of the host subnets
// for a given switch
func (manager *LogicalSwitchManager) AllocateNextIPs(switchName string) ([]*net.IPNet, error) {
	manager.RLock()
	defer manager.RUnlock()
	var ipnets []*net.IPNet
	var ip net.IP
	var err error
	lsi, ok := manager.cache[switchName]

	if !ok {
		return nil, fmt.Errorf("failed to allocate IPs for switch %s: %w", switchName, SwitchNotFound)
	}

	if len(lsi.ipams) == 0 {
		return nil, fmt.Errorf("failed to allocate IPs for switch %s because there is no IPAM instance", switchName)
	}

	if len(lsi.ipams) != len(lsi.hostSubnets) {
		return nil, fmt.Errorf("failed to allocate IPs for switch %s because host subnet instances: %d"+
			" don't match ipam instances: %d", switchName, len(lsi.hostSubnets), len(lsi.ipams))
	}

	defer func() {
		if err != nil {
			// iterate over range of already allocated indices and release
			// ips allocated before the error occurred.
			for relIdx, relIPNet := range ipnets {
				lsi.ipams[relIdx].Release(relIPNet.IP)
				if relIPNet.IP != nil {
					klog.Warningf("Reserved IP: %s was released", relIPNet.IP.String())
				}
			}
		}
	}()

	for idx, ipam := range lsi.ipams {
		ip, err = ipam.AllocateNext()
		if err != nil {
			return nil, err
		}
		ipnet := &net.IPNet{
			IP:   ip,
			Mask: lsi.hostSubnets[idx].Mask,
		}
		ipnets = append(ipnets, ipnet)
	}
	return ipnets, nil
}

func (manager *LogicalSwitchManager) AllocateHybridOverlay(switchName string, hybridOverlayAnnotation []string) ([]*net.IPNet, error) {
	if len(hybridOverlayAnnotation) > 0 {
		var allocateAddresses []*net.IPNet
		for _, ip := range hybridOverlayAnnotation {
			allocateAddresses = append(allocateAddresses, &net.IPNet{IP: net.ParseIP(ip).To4(), Mask: net.CIDRMask(32, 32)})
		}
		// attempt to allocate the IP address that is annotated on the node. The only way there would be a collision is if the annotations of podIP or hybridOverlayDRIP
		// where manually edited and we do not support that
		err := manager.AllocateIPs(switchName, allocateAddresses)
		if err != nil && err != ipallocator.ErrAllocated {
			return nil, err
		}
		return allocateAddresses, nil
	}
	// if we are not provided with any addresses
	manager.RLock()
	defer manager.RUnlock()

	lsi, ok := manager.cache[switchName]
	if !ok {
		return nil, fmt.Errorf("unable to allocate hybrid overlay for switch %s: %w", switchName, SwitchNotFound)
	}
	// determine if ipams are ipv4
	var ipv4IPAMS []ipam.Interface
	for _, ipam := range lsi.ipams {
		if utilnet.IsIPv4(ipam.CIDR().IP) {
			ipv4IPAMS = append(ipv4IPAMS, ipam)
		}
	}
	var allocatedAddresses []*net.IPNet
	for _, ipv4IPAM := range ipv4IPAMS {
		hostSubnet := ipv4IPAM.CIDR()
		potentialHybridIFAddress := util.GetNodeHybridOverlayIfAddr(&hostSubnet)
		err := ipv4IPAM.Allocate(potentialHybridIFAddress.IP)
		if err == ipam.ErrAllocated {
			// allocate NextIP
			allocatedipv4, err := ipv4IPAM.AllocateNext()
			if err != nil {
				_ = manager.ReleaseIPs(switchName, allocatedAddresses)
				return nil, fmt.Errorf("cannot allocate hybrid overlay interface address for switch/subnet %s/%s (%+v)", switchName, hostSubnet, err)

			}
			if err == nil {
				allocatedAddresses = append(allocatedAddresses, &net.IPNet{IP: allocatedipv4.To4(), Mask: net.CIDRMask(32, 32)})

			}
		} else if err != nil {
			_ = manager.ReleaseIPs(switchName, allocatedAddresses)
			return nil, fmt.Errorf("cannot allocate hybrid overlay interface address for switch/subnet %s/%s (%+v)", switchName, hostSubnet, err)
		} else {
			allocatedAddresses = append(allocatedAddresses, &net.IPNet{IP: potentialHybridIFAddress.IP.To4(), Mask: net.CIDRMask(32, 32)})
		}
	}
	return allocatedAddresses, nil
}

// Mark the IPs in ipnets slice as available for allocation
// by releasing them from the IPAM pool of allocated IPs.
// If there aren't IPs to release the method does not return an error.
func (manager *LogicalSwitchManager) ReleaseIPs(switchName string, ipnets []*net.IPNet) error {
	manager.RLock()
	defer manager.RUnlock()
	if ipnets == nil || switchName == "" {
		klog.V(5).Infof("Switch name is empty or ip slice to release is nil")
		return nil
	}
	lsi, ok := manager.cache[switchName]
	if !ok {
		return fmt.Errorf("unable to release ips for switch %s: %w", switchName, SwitchNotFound)
	}

	for _, ipnet := range ipnets {
		for _, ipam := range lsi.ipams {
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
func (manager *LogicalSwitchManager) ConditionalIPRelease(switchName string, ipnets []*net.IPNet, predicate func() (bool, error)) (bool, error) {
	manager.RLock()
	defer manager.RUnlock()
	if ipnets == nil || switchName == "" {
		klog.V(5).Infof("Switch name is empty or ip slice to release is nil")
		return false, nil
	}
	lsi, ok := manager.cache[switchName]
	if !ok {
		return false, nil
	}
	if len(lsi.ipams) == 0 {
		return false, nil
	}

	// check if ipam has one of the ip addresses, and then execute the predicate function to determine
	// if this IP should be released or not
	for _, ipnet := range ipnets {
		for _, ipam := range lsi.ipams {
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

// NewL2SwitchManager initializes a new layer2 logical switch manager,
// only manage subnet for the one specified switch
func NewL2SwitchManager() *LogicalSwitchManager {
	return &LogicalSwitchManager{
		cache:    make(map[string]logicalSwitchInfo),
		RWMutex:  sync.RWMutex{},
		ipamFunc: NewL2IPAMAllocator,
	}
}

// NewLayer2IPAMAllocator provides an ipam interface which can be used for layer2 switch IPAM
// allocations for the specified cidr using a contiguous allocation strategy.
func NewL2IPAMAllocator(cidr *net.IPNet) (ipam.Interface, error) {
	subnetRange, err := ipam.NewAllocatorCIDRRange(cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewRoundRobinAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return nil, err
	}
	return subnetRange, nil
}

// GenerateRandMAC generates a random unicast and locally administered MAC address.
// LOOTED FROM https://github.com/cilium/cilium/blob/v1.12.6/pkg/mac/mac.go#L106
func GenerateRandMAC() (net.HardwareAddr, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("unable to retrieve 6 rnd bytes: %s", err)
	}

	// Set locally administered addresses bit and reset multicast bit
	buf[0] = (buf[0] | 0x02) & 0xfe

	return buf, nil
}

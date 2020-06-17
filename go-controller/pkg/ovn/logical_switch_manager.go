package ovn

import (
	"fmt"
	"net"
	"reflect"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ipam "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator/allocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog"
)

// logicalSwitchInfo contains information corresponding to the node. It holds the
// subnet allocations (v4 and v6) as well as the IPAM allocator instances for each
// subnet managed for this node
type logicalSwitchInfo struct {
	hostSubnets  []*net.IPNet
	ipams        []ipam.Interface
	noHostSubnet bool
}

type ipamFactoryFunc func(*net.IPNet) (ipam.Interface, error)

// logicalSwitchManager provides switch info management APIs including IPAM for the host subnets
type logicalSwitchManager struct {
	cache map[string]logicalSwitchInfo
	// A RW mutex for logicalSwitchManager which holds logicalSwitch information
	sync.RWMutex
	ipamFunc ipamFactoryFunc
}

// NewIPAMAllocator provides an ipam interface which can be used for IPAM
// allocations for a given cidr using a contiguous allocation strategy.
// It also pre-allocates certain special subnet IPs such as the .1, .2, and .3
// addresses as reserved.
func NewIPAMAllocator(cidr *net.IPNet) (ipam.Interface, error) {
	subnetRange, err := ipam.NewAllocatorCIDRRange(cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
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
	if config.HybridOverlay.Enabled {
		hybridOverlayIfAddr := util.GetNodeHybridOverlayIfAddr(subnet)

		err = ipam.Allocate(hybridOverlayIfAddr.IP)
		if err != nil {
			klog.Errorf("Unable to allocate subnet's hybrid overlay interface IP: %s", hybridOverlayIfAddr.IP)
			return err
		}
	}

	return nil
}

// Initializes a new logical switch manager
func newLogicalSwitchManager() *logicalSwitchManager {
	return &logicalSwitchManager{
		cache:    make(map[string]logicalSwitchInfo),
		RWMutex:  sync.RWMutex{},
		ipamFunc: NewIPAMAllocator,
	}
}

// AddNode adds/updates a node to the logical switch manager for subnet
// and IPAM management.
func (manager *logicalSwitchManager) AddNode(nodeName string, hostSubnets []*net.IPNet) error {
	manager.Lock()
	defer manager.Unlock()
	if lsi, ok := manager.cache[nodeName]; ok && !reflect.DeepEqual(lsi.hostSubnets, hostSubnets) {
		klog.Warningf("Node %q logical switch already in cache with subnet %s; replacing with %s", nodeName,
			util.JoinIPNets(lsi.hostSubnets, ","), util.JoinIPNets(hostSubnets, ","))
	}
	var ipams []ipam.Interface
	for _, subnet := range hostSubnets {
		ipam, err := manager.ipamFunc(subnet)
		if err != nil {
			klog.Errorf("IPAM for subnet %s was not initialized for node %q", subnet, nodeName)
			return err
		}
		ipams = append(ipams, ipam)
	}
	manager.cache[nodeName] = logicalSwitchInfo{
		hostSubnets:  hostSubnets,
		ipams:        ipams,
		noHostSubnet: len(hostSubnets) == 0,
	}

	return nil
}

// AddNoHostSubnetNode adds/updates a node without any host subnets
// to the logical switch manager
func (manager *logicalSwitchManager) AddNoHostSubnetNode(nodeName string) error {
	// setting the hostSubnets slice argument to nil in the cache means an object
	// exists for the switch but it was not assigned a hostSubnet by ovn-kubernetes
	// this will be true for nodes that are marked as host-subnet only.
	return manager.AddNode(nodeName, nil)
}

// Remove a switch/node from the the logical switch manager
func (manager *logicalSwitchManager) DeleteNode(nodeName string) {
	manager.Lock()
	defer manager.Unlock()
	delete(manager.cache, nodeName)
}

// Given a switch name, checks if the switch is a noHostSubnet switch
func (manager *logicalSwitchManager) IsNonHostSubnetSwitch(nodeName string) bool {
	manager.RLock()
	defer manager.RUnlock()
	lsi, ok := manager.cache[nodeName]
	return ok && lsi.noHostSubnet
}

// Given a switch name, get all its host-subnets
func (manager *logicalSwitchManager) GetSwitchSubnets(nodeName string) []*net.IPNet {
	manager.RLock()
	defer manager.RUnlock()
	lsi, ok := manager.cache[nodeName]
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

// AllocateIPs will block off IPs in the ipnets slice as already allocated
// for a given switch
func (manager *logicalSwitchManager) AllocateIPs(nodeName string, ipnets []*net.IPNet) error {
	manager.RLock()
	defer manager.RUnlock()
	lsi, ok := manager.cache[nodeName]
	if len(ipnets) == 0 || !ok || len(lsi.ipams) == 0 {
		return fmt.Errorf("unable to allocate ips %v for node: %s",
			ipnets, nodeName)

	}
	for _, ipnet := range ipnets {
		for _, ipam := range lsi.ipams {
			cidr := ipam.CIDR()
			if cidr.Contains(ipnet.IP) {
				if err := ipam.Allocate(ipnet.IP); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// AllocateNextIPs allocates IP addresses from each of the host subnets
// for a given switch
func (manager *logicalSwitchManager) AllocateNextIPs(nodeName string) ([]*net.IPNet, error) {
	manager.RLock()
	defer manager.RUnlock()
	var ipnets []*net.IPNet
	var ip net.IP
	var err error
	lsi, ok := manager.cache[nodeName]

	if !ok {
		return nil, fmt.Errorf("node %s not found in the logical switch manager cache", nodeName)
	}

	if len(lsi.ipams) == 0 {
		return nil, fmt.Errorf("failed to allocate IPs for node %s because there is no IPAM instance", nodeName)
	}

	if len(lsi.ipams) != len(lsi.hostSubnets) {
		return nil, fmt.Errorf("failed to allocate IPs for node %s because host subnet instances: %d"+
			" don't match ipam instances: %d", nodeName, len(lsi.hostSubnets), len(lsi.ipams))
	}

	defer func() {
		if err != nil {
			// iterate over range of already allocated indices and release
			// ips allocated before the error occurred.
			for relIdx, relIPNet := range ipnets {
				if relErr := lsi.ipams[relIdx].Release(relIPNet.IP); relErr != nil {
					klog.Errorf("Error while releasing IP: %s, err: %v", relIPNet.IP, relErr)
				}
			}
			klog.Warningf("Allocated IPs: %s were released", util.JoinIPNetIPs(ipnets, " "))
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

// Mark the IPs in ipnets slice as available for allocation
// by releasing them from the IPAM pool of allocated IPs.
func (manager *logicalSwitchManager) ReleaseIPs(nodeName string, ipnets []*net.IPNet) error {
	manager.RLock()
	defer manager.RUnlock()
	if ipnets == nil || nodeName == "" {
		klog.V(5).Infof("Node name is empty or ip slice to release is nil")
		return nil
	}
	lsi, ok := manager.cache[nodeName]
	if !ok {
		return fmt.Errorf("node %s not found in the logical switch manager cache",
			nodeName)
	}
	if len(lsi.ipams) == 0 {
		return fmt.Errorf("failed to release IPs for node %s because there is no IPAM instance", nodeName)
	}
	for _, ipnet := range ipnets {
		for _, ipam := range lsi.ipams {
			cidr := ipam.CIDR()
			if cidr.Contains(ipnet.IP) {
				if err := ipam.Release(ipnet.IP); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

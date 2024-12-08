package logicalswitchmanager

import (
	"fmt"
	"net"

	ipam "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var SwitchNotFound = subnet.ErrSubnetNotFound

// LogicalSwitchManager provides switch info management APIs including IPAM for the host subnets
type LogicalSwitchManager struct {
	allocator  subnet.Allocator
	reserveIPs bool
}

// NewLogicalSwitchManager initializes a new logical switch manager for L3
// networks.
func NewLogicalSwitchManager() *LogicalSwitchManager {
	return &LogicalSwitchManager{
		allocator:  subnet.NewAllocator(),
		reserveIPs: true,
	}
}

// NewL2SwitchManager initializes a new logical switch manager for L2 secondary
// networks.
// In L2, we do not auto-reserve the GW and mp0 IPs on the subnet - the reasons
// are different depending on the topology though:
//   - localnet: it is the user's responsibility to know which IPs to exclude
//     from the physical network
//   - layer2: this is a disconnected network, thus it doesn't have a GW, nor a
//     management port
func NewL2SwitchManager() *LogicalSwitchManager {
	return &LogicalSwitchManager{
		allocator: subnet.NewAllocator(),
	}
}

// NewL2SwitchManagerForUserDefinedPrimaryNetwork initializes a new logical
// switch manager for L2 primary networks.
// A user defined primary network auto-reserves the .1 and .2 IP addresses,
// which are required for egressing the cluster over this user defined network.
func NewL2SwitchManagerForUserDefinedPrimaryNetwork() *LogicalSwitchManager {
	return NewLogicalSwitchManager()
}

// AddOrUpdateSwitch adds/updates a switch to the logical switch manager for subnet
// and IPAM management.
func (manager *LogicalSwitchManager) AddOrUpdateSwitch(switchName string, hostSubnets []*net.IPNet, excludeSubnets ...*net.IPNet) error {
	if manager.reserveIPs {
		for _, hostSubnet := range hostSubnets {
			for _, ip := range []*net.IPNet{util.GetNodeGatewayIfAddr(hostSubnet), util.GetNodeManagementIfAddr(hostSubnet)} {
				excludeSubnets = append(excludeSubnets,
					&net.IPNet{IP: ip.IP, Mask: util.GetIPFullMask(ip.IP)},
				)
			}
		}
	}
	return manager.allocator.AddOrUpdateSubnet(switchName, hostSubnets, excludeSubnets...)
}

// AddNoHostSubnetSwitch adds/updates a switch without any host subnets
// to the logical switch manager
func (manager *LogicalSwitchManager) AddNoHostSubnetSwitch(switchName string) error {
	// setting the hostSubnets slice argument to nil in the cache means an object
	// exists for the switch but it was not assigned a hostSubnet by ovn-kubernetes
	// this will be true for switches created on nodes that are marked as host-subnet only.
	return manager.allocator.AddOrUpdateSubnet(switchName, nil)
}

// Remove a switch from the the logical switch manager
func (manager *LogicalSwitchManager) DeleteSwitch(switchName string) {
	manager.allocator.DeleteSubnet(switchName)
}

// Given a switch name, checks if the switch is a noHostSubnet switch
func (manager *LogicalSwitchManager) IsNonHostSubnetSwitch(switchName string) bool {
	subnets, err := manager.allocator.GetSubnets(switchName)
	return err == nil && len(subnets) == 0
}

// Given a switch name, get all its host-subnets
func (manager *LogicalSwitchManager) GetSwitchSubnets(switchName string) []*net.IPNet {
	subnets, _ := manager.allocator.GetSubnets(switchName)
	return subnets
}

// AllocateUntilFull used for unit testing only, allocates the rest of the switch subnet
func (manager *LogicalSwitchManager) AllocateUntilFull(switchName string) error {
	return manager.allocator.AllocateUntilFull(switchName)
}

// AllocateIPs will block off IPs in the ipnets slice as already allocated
// for a given switch
func (manager *LogicalSwitchManager) AllocateIPs(switchName string, ipnets []*net.IPNet) error {
	return manager.allocator.AllocateIPPerSubnet(switchName, ipnets)
}

// AllocateNextIPs allocates IP addresses from each of the host subnets
// for a given switch
func (manager *LogicalSwitchManager) AllocateNextIPs(switchName string) ([]*net.IPNet, error) {
	return manager.allocator.AllocateNextIPs(switchName)
}

func (manager *LogicalSwitchManager) AllocateHybridOverlay(switchName string, hybridOverlayAnnotation []string) ([]*net.IPNet, error) {
	var err error
	var allocatedAddresses []*net.IPNet

	if len(hybridOverlayAnnotation) > 0 {
		for _, ip := range hybridOverlayAnnotation {
			allocatedAddresses = append(allocatedAddresses, &net.IPNet{IP: net.ParseIP(ip).To4(), Mask: net.CIDRMask(32, 32)})
		}
		// attempt to allocate the IP address that is annotated on the node. The only way there would be a collision is if the annotations of podIP or hybridOverlayDRIP
		// where manually edited and we do not support that
		err = manager.AllocateIPs(switchName, allocatedAddresses)
		if err != nil && err != ipam.ErrAllocated {
			return nil, err
		}
		return allocatedAddresses, nil
	}

	// if we are not provided with any addresses, try to allocate the well known address
	hostSubnets := manager.GetSwitchSubnets(switchName)
	for _, hostSubnet := range hostSubnets {
		allocatedAddresses = append(allocatedAddresses, util.GetNodeHybridOverlayIfAddr(hostSubnet))
	}
	err = manager.AllocateIPs(switchName, allocatedAddresses)
	if err != nil && err != ipam.ErrAllocated {
		return nil, fmt.Errorf("cannot allocate hybrid overlay interface addresses %s for switch %s: %w",
			util.StringSlice(allocatedAddresses),
			switchName,
			err)
	}

	// otherwise try to allocate any IP
	if err == ipam.ErrAllocated {
		allocatedAddresses, err = manager.AllocateNextIPs(switchName)
	}

	if err != nil {
		return nil, fmt.Errorf("cannot allocate new hybrid overlay interface addresses for switch %s: %w", switchName, err)
	}

	return allocatedAddresses, nil
}

// Mark the IPs in ipnets slice as available for allocation
// by releasing them from the IPAM pool of allocated IPs.
// If there aren't IPs to release the method does not return an error.
func (manager *LogicalSwitchManager) ReleaseIPs(switchName string, ipnets []*net.IPNet) error {
	return manager.allocator.ReleaseIPs(switchName, ipnets)
}

// ConditionalIPRelease determines if any IP is available to be released from an IPAM conditionally if func is true.
// It guarantees state of the allocator will not change while executing the predicate function
// TODO(trozet): add unit testing for this function
func (manager *LogicalSwitchManager) ConditionalIPRelease(switchName string, ipnets []*net.IPNet, predicate func() (bool, error)) (bool, error) {
	return manager.allocator.ConditionalIPRelease(switchName, ipnets, predicate)
}

// ForSubnet return an IP allocator for the specified switch
func (manager *LogicalSwitchManager) ForSwitch(switchName string) subnet.NamedAllocator {
	return manager.allocator.ForSubnet(switchName)
}

// GetSubnetName will find the switch that contains one of the subnets
// from "subnets" if not it will return "", false
func (manager *LogicalSwitchManager) GetSubnetName(subnets []*net.IPNet) (string, bool) {
	return manager.allocator.GetSubnetName(subnets)
}

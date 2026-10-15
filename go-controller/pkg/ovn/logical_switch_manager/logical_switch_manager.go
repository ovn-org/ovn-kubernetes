// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package logicalswitchmanager

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	knet "k8s.io/utils/net"

	ipam "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// LogicalSwitchManager provides switch info management APIs including IPAM for the host subnets
type LogicalSwitchManager struct {
	allocator  subnet.Allocator
	gatewayIPs []*net.IPNet
	mgmtIPs    []*net.IPNet
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
// A user defined primary network auto-reserves the gateway and the node management IP addresses,
// which are required for egressing the cluster over this user defined network.
func NewL2SwitchManagerForUserDefinedPrimaryNetwork(gatewayIPs, mgmtIPs []*net.IPNet) *LogicalSwitchManager {
	lsm := NewLogicalSwitchManager()
	lsm.gatewayIPs = gatewayIPs
	lsm.mgmtIPs = mgmtIPs
	return lsm
}

// AddOrUpdateSwitch adds/updates a switch to the logical switch manager for subnet
// and IPAM management, restoring initial allocations with their owners atomically.
func (manager *LogicalSwitchManager) AddOrUpdateSwitch(switchName string, hostSubnets []*net.IPNet, reservedSubnets []*net.IPNet,
	initialAllocations map[string][]*net.IPNet, excludeSubnets ...*net.IPNet,
) error {
	if manager.reserveIPs {
		for _, hostSubnet := range hostSubnets {
			gwIP, _ := util.MatchFirstIPNetFamily(knet.IsIPv6CIDR(hostSubnet), manager.gatewayIPs)
			if gwIP == nil {
				gwIP = util.GetNodeGatewayIfAddr(hostSubnet)
			}

			mgmtIP, _ := util.MatchFirstIPNetFamily(knet.IsIPv6CIDR(hostSubnet), manager.mgmtIPs)
			if mgmtIP == nil {
				mgmtIP = util.GetNodeManagementIfAddr(hostSubnet)
			}

			for _, ip := range []*net.IPNet{gwIP, mgmtIP} {
				excludeIP := &net.IPNet{IP: ip.IP, Mask: util.GetIPFullMask(ip.IP)}
				if !util.IsContainedInAnyCIDR(excludeIP, excludeSubnets...) {
					excludeSubnets = append(excludeSubnets, excludeIP)
				}
			}
		}
	}
	return manager.allocator.AddOrUpdateSubnet(subnet.SubnetConfig{
		Name:               switchName,
		Subnets:            hostSubnets,
		ReservedSubnets:    reservedSubnets,
		ExcludeSubnets:     excludeSubnets,
		InitialAllocations: initialAllocations,
	})
}

// AddNoHostSubnetSwitch adds/updates a switch without any host subnets
// to the logical switch manager
func (manager *LogicalSwitchManager) AddNoHostSubnetSwitch(switchName string) error {
	// setting the hostSubnets slice argument to nil in the cache means an object
	// exists for the switch but it was not assigned a hostSubnet by ovn-kubernetes
	// this will be true for switches created on nodes that are marked as host-subnet only.
	return manager.allocator.AddOrUpdateSubnet(subnet.SubnetConfig{Name: switchName})
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

// FilterIPsForSwitch returns the supplied addresses contained in the switch's subnets.
func (manager *LogicalSwitchManager) FilterIPsForSwitch(switchName string, ips []*net.IPNet) []*net.IPNet {
	var localIPs []*net.IPNet
	subnets := manager.GetSwitchSubnets(switchName)
	for _, ip := range ips {
		if util.IsContainedInAnyCIDR(ip, subnets...) {
			localIPs = append(localIPs, ip)
		}
	}
	return localIPs
}

// AllocateUntilFull used for unit testing only, allocates the rest of the switch subnet
func (manager *LogicalSwitchManager) AllocateUntilFull(switchName string) error {
	return manager.allocator.AllocateUntilFull(switchName)
}

// AllocateIPs reserves addresses for owner on a switch.
func (manager *LogicalSwitchManager) AllocateIPs(switchName, owner string, ipnets []*net.IPNet) error {
	return manager.allocator.AllocateIPs(switchName, owner, ipnets)
}

// AllocateNextIPs allocates addresses and records their owner atomically.
func (manager *LogicalSwitchManager) AllocateNextIPs(switchName, owner string) ([]*net.IPNet, error) {
	return manager.allocator.AllocateNextIPs(switchName, owner)
}

// OwnsIPs reports whether owner holds every requested address.
func (manager *LogicalSwitchManager) OwnsIPs(switchName, owner string, ips []*net.IPNet) bool {
	return manager.allocator.OwnsIPs(switchName, owner, ips)
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
		err = manager.AllocateIPs(switchName, "hybrid-overlay", allocatedAddresses)
		if err != nil && (!ipam.IsErrAllocated(err) || errors.Is(err, ipam.ErrAllocatedByOther)) {
			return nil, err
		}
		return allocatedAddresses, nil
	}

	// if we are not provided with any addresses, try to allocate the well known address
	hostSubnets := manager.GetSwitchSubnets(switchName)
	for _, hostSubnet := range hostSubnets {
		allocatedAddresses = append(allocatedAddresses, util.GetNodeHybridOverlayIfAddr(hostSubnet))
	}
	err = manager.AllocateIPs(switchName, "hybrid-overlay", allocatedAddresses)
	if err != nil && !ipam.IsErrAllocated(err) {
		return nil, fmt.Errorf("cannot allocate hybrid overlay interface addresses %s for switch %s: %w",
			util.StringSlice(allocatedAddresses),
			switchName,
			err)
	}

	// otherwise try to allocate any IP
	if ipam.IsErrAllocated(err) {
		allocatedAddresses, err = manager.AllocateNextIPs(switchName, "hybrid-overlay")
	}

	if err != nil {
		return nil, fmt.Errorf("cannot allocate new hybrid overlay interface addresses for switch %s: %w", switchName, err)
	}

	return allocatedAddresses, nil
}

// ReleaseIPs releases only addresses still held by owner.
func (manager *LogicalSwitchManager) ReleaseIPs(switchName, owner string, ipnets []*net.IPNet) error {
	return manager.allocator.ReleaseIPs(switchName, owner, ipnets)
}

// GetSubnetName will find the switch that contains one of the subnets
// from "subnets" if not it will return "", false
func (manager *LogicalSwitchManager) GetSubnetName(subnets []*net.IPNet) (string, bool) {
	return manager.allocator.GetSubnetName(subnets)
}

// SortPodsForIPAM orders bootstrap reservations so live pods claim their IPs
// before completed pods whose annotations may refer to released addresses.
// Among completed pods, the most recently initialized pod claims first.
func SortPodsForIPAM(objects []interface{}) ([]*corev1.Pod, error) {
	pods := make([]*corev1.Pod, 0, len(objects))
	for _, object := range objects {
		pod, ok := object.(*corev1.Pod)
		if !ok {
			return nil, fmt.Errorf("spurious object in pod IPAM initialization: %v", object)
		}
		pods = append(pods, pod)
	}
	initialized := func(pod *corev1.Pod) time.Time {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodInitialized {
				return condition.LastTransitionTime.Time
			}
		}
		return time.Time{}
	}
	sort.SliceStable(pods, func(i, j int) bool {
		if util.PodCompleted(pods[i]) != util.PodCompleted(pods[j]) {
			return !util.PodCompleted(pods[i])
		}
		return initialized(pods[i]).After(initialized(pods[j]))
	})
	return pods, nil
}

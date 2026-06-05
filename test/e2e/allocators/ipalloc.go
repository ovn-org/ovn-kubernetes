// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// TODO: add IP release functionality to allow reuse of allocated IPs.
// The reserved IPv4 range (.200-.254) only provides 55 addresses per subnet,
// which may become insufficient as more tests adopt this allocator.
// Consider basing this on AllocateInt/DeallocateInt from int.go which already
// provides ConfigMap-backed allocation with deallocation support.
package allocators

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"sync"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Reserved range for E2E test IPs to avoid conflicts with node IPs
const (
	testIPv4Start = 200 // Start from .200 to avoid typical node IPs (.1-.199)
	testIPv4End   = 254 // Last usable before .255 broadcast
	testIPv6Start = 200 // Same for IPv6 (0xC8)
	testIPv6End   = 255 // Last byte of reserved range (0xFF)
)

// primaryIPAllocator is a cluster-wide IP allocator for E2E tests. Callers pass a node
// object when requesting an IP; the provider subnet is looked up from the cached node
// subnets map (populated via GetProviderNodeSubnets at init). The allocator returns an
// unused IP from the reserved range (.200-.254 for IPv4, ::c8-::ff for IPv6) within
// that subnet.
// A separate allocator is maintained per subnet so that nodes on different subnets
// receive IPs from their own subnet's range.
// The nodeClient is used to list existing node IPs and avoid conflicts.
type primaryIPAllocator struct {
	mu          *sync.Mutex
	v4Allocs    map[string]*ipAllocator // keyed by subnet prefix (last byte zeroed)
	v6Allocs    map[string]*ipAllocator
	nodeClient  v1.NodeInterface
	nodeSubnets map[string][]string // keyed by node name
}

type ipAllocator struct {
	net *net.IPNet
	// base is a cached version of the start IP in the CIDR range as a *big.Int
	base *big.Int
	// max is the maximum size of the usable addresses in the range
	max   int
	count int
}

var pia *primaryIPAllocator

// InitPrimaryIPAllocator must be called to init IP allocator(s). Callers must be synchronise.
func InitPrimaryIPAllocator(nodeClient v1.NodeInterface) error {
	nodeSubnets, err := deploymentconfig.Get().GetNodeMachineNetworks()
	if err != nil {
		return fmt.Errorf("failed to get provider node subnets: %v", err)
	}
	if len(nodeSubnets) == 0 {
		return fmt.Errorf("expected at least one node but found zero")
	}
	pia, err = newPrimaryIPAllocator(nodeClient, nodeSubnets)
	return err
}

func NewPrimaryIPv4(nodeName string) (net.IP, error) {
	subnets, ok := pia.nodeSubnets[nodeName]
	if !ok {
		return nil, fmt.Errorf("no provider subnets found for node %q", nodeName)
	}
	for _, subnet := range subnets {
		ip, _, err := net.ParseCIDR(subnet)
		if err != nil {
			continue
		}
		if ip.To4() != nil {
			return pia.allocateNextV4(subnet)
		}
	}
	return nil, fmt.Errorf("no IPv4 subnet found for node %q", nodeName)
}

func NewPrimaryIPv6(nodeName string) (net.IP, error) {
	subnets, ok := pia.nodeSubnets[nodeName]
	if !ok {
		return nil, fmt.Errorf("no provider subnets found for node %q", nodeName)
	}
	for _, subnet := range subnets {
		ip, _, err := net.ParseCIDR(subnet)
		if err != nil {
			continue
		}
		if ip.To4() == nil {
			return pia.allocateNextV6(subnet)
		}
	}
	return nil, fmt.Errorf("no IPv6 subnet found for node %q", nodeName)
}

// newPrimaryIPAllocator initializes a primaryIPAllocator with empty per-subnet maps.
// Subnet allocators are created lazily when allocateNextV4/V6 is called with specific subnet CIDRs.
func newPrimaryIPAllocator(nodeClient v1.NodeInterface, nodeSubnets map[string][]string) (*primaryIPAllocator, error) {
	return &primaryIPAllocator{
		mu:          &sync.Mutex{},
		nodeClient:  nodeClient,
		v4Allocs:    make(map[string]*ipAllocator),
		v6Allocs:    make(map[string]*ipAllocator),
		nodeSubnets: nodeSubnets,
	}, nil
}

// allocateNextV4 allocates the next available IPv4 from the reserved range (.200-.254)
// within the provided subnet CIDR. The subnet string should be a CIDR like "10.0.0.0/17"
// derived from the cached node subnets map.
// Subnets narrower than /24 are rejected because the reserved range (.200-.254)
// requires at least a /24.
func (p *primaryIPAllocator) allocateNextV4(subnet string) (net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ip, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("failed to parse subnet %q: %v", subnet, err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("subnet %q is not an IPv4 CIDR", subnet)
	}
	ones, _ := ipNet.Mask.Size()
	if ones > 24 {
		return nil, fmt.Errorf("subnet %q is narrower than /24", subnet)
	}
	alloc := p.getOrCreateV4(ip4)
	return allocateIP(p.nodeClient, alloc.allocateNextIP, testIPv4Start, testIPv4End)
}

// allocateNextV6 allocates the next available IPv6 from the reserved range (::c8-::ff)
// within the provided subnet CIDR.
// Subnets narrower than /120 are rejected because the reserved range (::c8-::ff)
// requires at least a /120.
func (p *primaryIPAllocator) allocateNextV6(subnet string) (net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ip, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("failed to parse subnet %q: %v", subnet, err)
	}
	if ip.To4() != nil {
		return nil, fmt.Errorf("subnet %q is not an IPv6 CIDR", subnet)
	}
	ones, _ := ipNet.Mask.Size()
	if ones > 120 {
		return nil, fmt.Errorf("subnet %q is narrower than /120", subnet)
	}
	alloc := p.getOrCreateV6(ip.To16())
	return allocateIP(p.nodeClient, alloc.allocateNextIP, testIPv6Start, testIPv6End)
}

// getOrCreateV4 returns or lazily creates a per-subnet IPv4 allocator.
// Uses a fixed /24 mask internally so the allocator produces IPs in the .200-.254 range
// regardless of the subnet CIDR's actual mask. This is intentional because the provider
// may return a broad subnet (e.g. /17) while the allocator only needs the last octet range.
func (p *primaryIPAllocator) getOrCreateV4(ip net.IP) *ipAllocator {
	key := subnetKey(ip)
	if alloc, ok := p.v4Allocs[key]; ok {
		return alloc
	}
	ip4 := ip.To4()
	startIP := make(net.IP, 4)
	copy(startIP, ip4)
	startIP[3] = testIPv4Start - 1 // allocateNextIP increments before returning
	alloc := newIPAllocator(&net.IPNet{IP: startIP, Mask: net.CIDRMask(24, 32)}, testIPv4Start, testIPv4End)
	p.v4Allocs[key] = alloc
	return alloc
}

// getOrCreateV6 returns or lazily creates a per-subnet IPv6 allocator.
// Uses a fixed /120 mask internally so the allocator produces IPs in the ::c8-::ff range.
// See getOrCreateV4 for why the subnet CIDR's actual mask is intentionally ignored.
func (p *primaryIPAllocator) getOrCreateV6(ip net.IP) *ipAllocator {
	key := subnetKey(ip)
	if alloc, ok := p.v6Allocs[key]; ok {
		return alloc
	}
	ip16 := ip.To16()
	startIP := make(net.IP, 16)
	copy(startIP, ip16)
	startIP[15] = testIPv6Start - 1
	alloc := newIPAllocator(&net.IPNet{IP: startIP, Mask: net.CIDRMask(120, 128)}, testIPv6Start, testIPv6End)
	p.v6Allocs[key] = alloc
	return alloc
}

// subnetKey groups IPs by their /24 (IPv4) or /120 (IPv6) prefix. Two nodes in the
// same broad subnet (e.g. /17) but different /24s get separate allocators, which is
// fine since allocateIP rechecks against all node IPs to avoid conflicts.
func subnetKey(ip net.IP) string {
	return string(ip.To16().Mask(net.CIDRMask(120, 128)))
}

type allocNextFn func() (net.IP, error)

// allocateIP allocates the next available IP from the reserved range (.200-.254 for IPv4,
// ::c8-::ff for IPv6) that doesn't conflict with existing node IPs. The last byte of each
// candidate must fall within [startLastByte, endLastByte]; any candidate outside this range
// triggers an exhaustion error.
func allocateIP(nodeClient v1.NodeInterface, allocateFn allocNextFn, startLastByte, endLastByte byte) (net.IP, error) {
	nodeList, err := nodeClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %v", err)
	}
	for {
		nextIP, err := allocateFn()
		if err != nil {
			return nil, fmt.Errorf("failed to allocate next IP address (reserved test range may be exhausted): %v", err)
		}
		lastOctet := nextIP[len(nextIP)-1]
		if lastOctet < startLastByte || lastOctet > endLastByte {
			return nil, fmt.Errorf("reserved IP range exhausted: next candidate %s is outside range [%d, %d]", net.IP(nextIP), startLastByte, endLastByte)
		}
		isConflict, err := isConflictWithExistingHostIPs(nodeList.Items, nextIP)
		if err != nil {
			return nil, fmt.Errorf("failed to determine if IP conflicts with existing IPs: %v", err)
		}
		if !isConflict {
			return nextIP, nil
		}
		// IP conflicts with a node, try next one
	}
}

func isConflictWithExistingHostIPs(nodes []corev1.Node, ip net.IP) (bool, error) {
	ipStr := ip.String()
	for _, node := range nodes {
		nodeIPsSet, err := util.ParseNodeHostCIDRsDropNetMask(&node)
		if err != nil {
			return false, fmt.Errorf("failed to parse node %s primary annotation info: %v", node.Name, err)
		}
		if nodeIPsSet.Has(ipStr) {
			return true, nil
		}
	}
	return false, nil
}

func newIPAllocator(cidr *net.IPNet, start, end byte) *ipAllocator {
	return &ipAllocator{net: cidr, base: getBaseInt(cidr.IP), max: int(end-start) + 1}
}

func (n *ipAllocator) allocateNextIP() (net.IP, error) {
	if n.count >= n.max {
		return net.IP{}, fmt.Errorf("limit of %d reached", n.max)
	}
	n.base.Add(n.base, big.NewInt(1))
	n.count += 1
	b := n.base.Bytes()
	b = append(make([]byte, 16), b...)
	return b[len(b)-16:], nil
}

func getBaseInt(ip net.IP) *big.Int {
	return big.NewInt(0).SetBytes(ip.To16())
}

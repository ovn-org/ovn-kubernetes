// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ipalloc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// primaryIPAllocator attempts to allocate an IP in the same subnet as a nodes primary network
type primaryIPAllocator struct {
	mu         *sync.Mutex
	v4         *ipAllocator
	v6         *ipAllocator
	nodeClient v1.NodeInterface
}

var pia *primaryIPAllocator

var errNoRange = errors.New("no shared primary IP range")

func IsNoRangeError(err error) bool {
	return errors.Is(err, errNoRange)
}

// InitPrimaryIPAllocator must be called to init IP allocator(s). Callers must be synchronise.
func InitPrimaryIPAllocator(nodeClient v1.NodeInterface) error {
	allocator, err := newPrimaryIPAllocator(nodeClient)
	if err != nil && !IsNoRangeError(err) {
		pia = nil
		return err
	}
	pia = allocator
	return err
}

func NewPrimaryIPv4() (net.IP, error) {
	if pia == nil || pia.v4 == nil {
		ginkgo.Skip("primary IPv4 allocation is unavailable on this cluster", 2)
	}
	return pia.AllocateNextV4()
}

func NewPrimaryIPv6() (net.IP, error) {
	if pia == nil || pia.v6 == nil {
		ginkgo.Skip("primary IPv6 allocation is unavailable on this cluster", 2)
	}
	return pia.AllocateNextV6()
}

// newPrimaryIPAllocator gets a Node's primary interface information and checks that the first candidate is in every Node subnet.
func newPrimaryIPAllocator(nodeClient v1.NodeInterface) (*primaryIPAllocator, error) {
	ipa := &primaryIPAllocator{mu: &sync.Mutex{}, nodeClient: nodeClient}
	var noRangeErr error
	nodes, err := nodeClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return ipa, fmt.Errorf("failed to get a list of node(s): %v", err)
	}
	if len(nodes.Items) == 0 {
		return ipa, fmt.Errorf("expected at least one node but found zero")
	}
	nodePrimaryIPs, err := util.ParseNodePrimaryIfAddr(&nodes.Items[0])
	if err != nil {
		return ipa, fmt.Errorf("failed to parse node primary interface address from Node object: %v", err)
	}
	if nodePrimaryIPs.V4.IP != nil {
		prefix, _ := nodePrimaryIPs.V4.Net.Mask.Size()
		// Incrementing the penultimate octet would move a /24 candidate outside its subnet.
		if prefix != 24 {
			nodePrimaryIPs.V4.IP[len(nodePrimaryIPs.V4.IP)-2]++
		}
		ipa.v4 = newIPAllocator(&net.IPNet{IP: nodePrimaryIPs.V4.IP, Mask: nodePrimaryIPs.V4.Net.Mask})
	}
	if nodePrimaryIPs.V6.IP != nil {
		nodePrimaryIPs.V6.IP[len(nodePrimaryIPs.V6.IP)-2]++
		ipa.v6 = newIPAllocator(&net.IPNet{IP: nodePrimaryIPs.V6.IP, Mask: nodePrimaryIPs.V6.Net.Mask})
	}
	// verify the new starting base IP is within all Nodes subnets
	if nodePrimaryIPs.V4.IP != nil {
		ipNets, err := getNodePrimaryProviderIPs(nodes.Items, false)
		if err != nil {
			return ipa, err
		}
		nextIP, err := ipa.v4.AllocateNextIP()
		if err != nil {
			return ipa, err
		}
		if !isIPWithinAllSubnets(ipNets, nextIP) {
			ipa.v4 = nil
			noRangeErr = fmt.Errorf("%w: IP %s is not within all Node subnets", errNoRange, nextIP)
		}
	}
	if nodePrimaryIPs.V6.IP != nil {
		ipNets, err := getNodePrimaryProviderIPs(nodes.Items, true)
		if err != nil {
			return ipa, err
		}
		nextIP, err := ipa.v6.AllocateNextIP()
		if err != nil {
			return ipa, err
		}
		if !isIPWithinAllSubnets(ipNets, nextIP) {
			ipa.v6 = nil
			if noRangeErr == nil {
				noRangeErr = fmt.Errorf("%w: IP %s is not within all Node subnets", errNoRange, nextIP)
			}
		}
	}

	return ipa, noRangeErr
}

func getNodePrimaryProviderIPs(nodes []corev1.Node, isIPv6 bool) ([]*net.IPNet, error) {
	ipNets := make([]*net.IPNet, 0, len(nodes))
	for _, node := range nodes {
		nodePrimaryIPs, err := util.ParseNodePrimaryIfAddr(&node)
		if err != nil {
			return nil, fmt.Errorf("failed to parse node primary interface address from Node %s object: %v", node.Name, err)
		}
		var mask net.IPMask
		var ip net.IP

		if isIPv6 {
			ip = nodePrimaryIPs.V6.IP
			mask = nodePrimaryIPs.V6.Net.Mask
		} else {
			ip = nodePrimaryIPs.V4.IP
			mask = nodePrimaryIPs.V4.Net.Mask
		}
		if len(ip) == 0 || len(mask) == 0 {
			return nil, fmt.Errorf("failed to find Node %s primary Node IP and/or mask", node.Name)
		}
		ipNets = append(ipNets, &net.IPNet{IP: ip, Mask: mask})
	}
	return ipNets, nil
}

func isIPWithinAllSubnets(ipNets []*net.IPNet, ip net.IP) bool {
	if len(ipNets) == 0 {
		return false
	}
	for _, ipNet := range ipNets {
		if !ipNet.Contains(ip) {
			return false
		}
	}
	return true
}

func (pia *primaryIPAllocator) IncrementAndGetNextV4(times int) (net.IP, error) {
	var err error
	for i := 0; i < times; i++ {
		if _, err = pia.AllocateNextV4(); err != nil {
			return nil, err
		}
	}
	return pia.AllocateNextV4()
}

func (pia *primaryIPAllocator) AllocateNextV4() (net.IP, error) {
	if pia.v4 == nil {
		return nil, fmt.Errorf("IPv4 is not enable ")
	}
	if pia.v4.net == nil {
		return nil, fmt.Errorf("IPv4 is not enabled but Allocation request was called")
	}
	pia.mu.Lock()
	defer pia.mu.Unlock()
	return allocateIP(pia.nodeClient, pia.v4.AllocateNextIP)
}

func (pia *primaryIPAllocator) IncrementAndGetNextV6(times int) (net.IP, error) {
	var err error
	for i := 0; i < times; i++ {
		if _, err = pia.AllocateNextV6(); err != nil {
			return nil, err
		}
	}
	return pia.AllocateNextV6()
}

func (pia primaryIPAllocator) AllocateNextV6() (net.IP, error) {
	if pia.v6 == nil {
		return nil, fmt.Errorf("IPv6 is not enabled but Allocation request was called")
	}
	if pia.v6.net == nil {
		return nil, fmt.Errorf("ipv6 network is not set")
	}
	pia.mu.Lock()
	defer pia.mu.Unlock()
	return allocateIP(pia.nodeClient, pia.v6.AllocateNextIP)
}

type allocNextFn func() (net.IP, error)

func allocateIP(nodeClient v1.NodeInterface, allocateFn allocNextFn) (net.IP, error) {
	nodeList, err := nodeClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %v", err)
	}
	for {
		nextIP, err := allocateFn()
		if err != nil {
			return nil, fmt.Errorf("failed to allocated next IP address: %v", err)
		}
		firstOctet := nextIP[len(nextIP)-1]
		// skip 0 and 1
		if firstOctet == 0 || firstOctet == 1 {
			continue
		}
		isConflict, err := isConflictWithExistingHostIPs(nodeList.Items, nextIP)
		if err != nil {
			return nil, fmt.Errorf("failed to determine if IP conflicts with existing IPs: %v", err)
		}
		if !isConflict {
			return nextIP, nil
		}
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

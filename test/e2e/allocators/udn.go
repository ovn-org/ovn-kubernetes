// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package allocators

import (
	"fmt"
	"net"
	"sync"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	"k8s.io/apimachinery/pkg/util/sets"
)

var (
	udnOnce      sync.Once
	udnV4, udnV6 subnetSpec
)

func initSubnetSpecs() {
	udnOnce.Do(func() {
		v4Exclusions, v6Exclusions := infrastructureNetworkExclusions()
		udnV4 = newSubnetSpec(udnSubnets, v4Exclusions)
		udnV6 = newSubnetSpec(udnSubnets6, v6Exclusions)
	})
}

func infrastructureNetworkExclusions() (ipv4, ipv6 []string) {
	v4, v6, err := getMachineNetworkSubnets()
	if err != nil {
		panic(fmt.Errorf("failed to get machine network subnets for exclusion: %v", err))
	}
	infraV4, infraV6 := infraprovider.Get().InfrastructureNetworkExclusions()
	v4 = v4.Union(infraV4)
	v6 = v6.Union(infraV6)
	return v4.UnsortedList(), v6.UnsortedList()
}

// GetFirstUDNSubnets always allocates the first UDN IPv4 and IPv6 subnet
// within the dedicated UDN subnet broader range. Used when overlaps across UDNs
// are not a concern but still prevents overlaps with other subnets.
func GetFirstUDNSubnets() (ipv4, ipv6 string) {
	subnets4, subnets6 := GetNthFirstUDNSubnets(1)
	return subnets4[0], subnets6[0]
}

// GetNthFirstUDNSubnets returns the first n UDN IPv4 and IPv6 subnets within
// the dedicated UDN subnet broader range. Used when overlaps across UDNs are
// not a concern but still prevents overlaps with other subnets.
func GetNthFirstUDNSubnets(n int) (ipv4, ipv6 []string) {
	if n < 1 {
		panic("GetNthFirstUDNSubnets: n must be >= 1")
	}
	initSubnetSpecs()
	if n > udnV4.usable() || n > udnV6.usable() {
		panic("GetNthFirstUDNSubnets: not enough free subnets available")
	}

	ipv4 = make([]string, 0, n)
	ipv6 = make([]string, 0, n)
	for i := 1; i < n+1; i++ {
		udnV4Idx := udnV4.nthFree(i)
		udnV6Idx := udnV6.nthFree(i)
		ipv4 = append(ipv4, udnV4.cidr(udnV4Idx))
		ipv6 = append(ipv6, udnV6.cidr(udnV6Idx))
	}
	return ipv4, ipv6
}

// getMachineNetworkSubnets retrieves the machine network subnets from the
// deployment config's GetProviderNodeSubnets. It returns the unique IPv4 and
// IPv6 CIDR networks found across all nodes.
func getMachineNetworkSubnets() (sets.Set[string], sets.Set[string], error) {
	ipv4 := sets.New[string]()
	ipv6 := sets.New[string]()
	nodeSubnets, err := deploymentconfig.Get().GetNodeMachineNetworks()
	if err != nil {
		return ipv4, ipv6, err
	}
	for _, subnets := range nodeSubnets {
		for _, subnet := range subnets {
			ip, _, err := net.ParseCIDR(subnet)
			if err != nil {
				continue
			}
			if ip.To4() != nil {
				ipv4.Insert(subnet)
			} else {
				ipv6.Insert(subnet)
			}
		}
	}
	return ipv4, ipv6, nil
}

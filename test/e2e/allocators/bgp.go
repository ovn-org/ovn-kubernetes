// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package allocators

import (
	"sync"

	"k8s.io/kubernetes/test/e2e/framework"

	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

const (
	bgpKey = "bgp"

	// VID valid range is 2-4094 (0, 1, 4095 are reserved)
	vidMin = 2
	vidMax = 4094

	// VNI valid range is 1-16777215 (24-bit)
	vniMax = 16777215
	// Even offset so MACVRF VNIs are always even and don't correlate with VIDs.
	vniOffset = vniMax/2 + 1
)

var (
	bgpOnce              sync.Once
	bgpPeer4, bgpPeer6   subnetSpec
	bgpIPVRF4, bgpIPVRF6 subnetSpec
	bgpVTEP4, bgpVTEP6   subnetSpec
	bgpUDN4, bgpUDN6     subnetSpec
)

type BGPAllocation struct {
	UDNSubnet      string
	UDNSubnet6     string
	BGPPeerSubnet  string
	BGPPeerSubnet6 string
	IPVRFSubnet    string
	IPVRFSubnet6   string
	VTEPSubnet     string
	VTEPSubnet6    string
	MACVRFVNI      int
	MACVRFVID      int
	IPVRFVNI       int
	IPVRFVID       int
}

// AllocateBGP allocates non-overlapping UDN subnets, BGP peering subnets,
// IP-VRF subnets, VTEP subnets, VNIs and VIDs for parallel test isolation
// commonly needed in BGP test cases. All resources are derived from a single
// integer allocated with AllocateInt. Deallocation is registered as a cleanup
// function.
func AllocateBGP(f *framework.Framework, cleanup infraapi.ContextCleanUp) (BGPAllocation, error) {
	bgpOnce.Do(func() {
		v4Exclusions, v6Exclusions := infrastructureNetworkExclusions()
		bgpPeer4 = newSubnetSpec(bgpPeerSubnets, v4Exclusions)
		bgpPeer6 = newSubnetSpec(bgpPeerSubnets6, v6Exclusions)
		bgpIPVRF4 = newSubnetSpec(ipvrfSubnets, v4Exclusions)
		bgpIPVRF6 = newSubnetSpec(ipvrfSubnets6, v6Exclusions)
		bgpVTEP4 = newSubnetSpec(vtepSubnets, v4Exclusions)
		bgpVTEP6 = newSubnetSpec(vtepSubnets6, v6Exclusions)
		bgpUDN4 = newSubnetSpec(udnSubnets, v4Exclusions)
		bgpUDN6 = newSubnetSpec(udnSubnets6, v6Exclusions)
	})

	maxUsable := min(
		bgpPeer4.usable(), bgpPeer6.usable(),
		bgpIPVRF4.usable(), bgpIPVRF6.usable(),
		bgpVTEP4.usable(), bgpVTEP6.usable(),
		bgpUDN4.usable(), bgpUDN6.usable(),
		(vidMax-vidMin+1)/2,
		(vniMax-vniOffset+1)/2,
	)

	n, err := AllocateInt(f, bgpKey, maxUsable)
	if err != nil {
		return BGPAllocation{}, err
	}

	cleanup.AddCleanUpFn(func() error {
		return DeallocateInt(f, bgpKey, n)
	})

	bgpV4Idx := bgpPeer4.nthFree(n)
	bgpV6Idx := bgpPeer6.nthFree(n)
	ipvrfV4Idx := bgpIPVRF4.nthFree(n)
	ipvrfV6Idx := bgpIPVRF6.nthFree(n)
	vtepV4Idx := bgpVTEP4.nthFree(n)
	vtepV6Idx := bgpVTEP6.nthFree(n)
	udnV4Idx := bgpUDN4.nthFree(n)
	udnV6Idx := bgpUDN6.nthFree(n)

	// Each allocation consumes two VIDs (MACVRF + IPVRF). MACVRF gets even
	// VIDs and IPVRF gets odd VIDs so different allocations never collide.
	macvrfVID := (n-1)*2 + vidMin
	ipvrfVID := macvrfVID + 1
	// Same stride-by-2 scheme for VNIs: MACVRF gets even, IPVRF gets odd.
	macvrfVNI := (n-1)*2 + vniOffset
	ipvrfVNI := macvrfVNI + 1

	return BGPAllocation{
		UDNSubnet:      bgpUDN4.cidr(udnV4Idx),
		UDNSubnet6:     bgpUDN6.cidr(udnV6Idx),
		BGPPeerSubnet:  bgpPeer4.cidr(bgpV4Idx),
		BGPPeerSubnet6: bgpPeer6.cidr(bgpV6Idx),
		IPVRFSubnet:    bgpIPVRF4.cidr(ipvrfV4Idx),
		IPVRFSubnet6:   bgpIPVRF6.cidr(ipvrfV6Idx),
		VTEPSubnet:     bgpVTEP4.cidr(vtepV4Idx),
		VTEPSubnet6:    bgpVTEP6.cidr(vtepV6Idx),
		MACVRFVNI:      macvrfVNI,
		MACVRFVID:      macvrfVID,
		IPVRFVNI:       ipvrfVNI,
		IPVRFVID:       ipvrfVID,
	}, nil
}

package subnetallocator

import (
	"fmt"
	"net"
	"sync"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilnet "k8s.io/utils/net"
)

var ErrSubnetAllocatorFull = fmt.Errorf("no subnets available.")

type SubnetAllocator interface {
	AddNetworkRange(network *net.IPNet, hostSubnetLen int) error
	MarkAllocatedNetworks(...*net.IPNet) error
	// Usage returns the number of available and used v4 subnets, and
	// the number of available and used v6 subnets
	Usage() (uint64, uint64, uint64, uint64)
	AllocateNetworks() ([]*net.IPNet, error)
	AllocateIPv4Network() (*net.IPNet, error)
	AllocateIPv6Network() (*net.IPNet, error)
	ReleaseNetworks(...*net.IPNet) error
}

type BaseSubnetAllocator struct {
	sync.Mutex

	v4ranges []*subnetAllocatorRange
	v6ranges []*subnetAllocatorRange
}

var _ SubnetAllocator = &BaseSubnetAllocator{}

func NewSubnetAllocator() SubnetAllocator {
	return &BaseSubnetAllocator{}
}

// Usage returns the number of allocated and used IPv4 subnets, and the number
// of allocated and used IPv6 subnets.
func (sna *BaseSubnetAllocator) Usage() (uint64, uint64, uint64, uint64) {
	var v4count, v4used, v6count, v6used uint64
	for _, snr := range sna.v4ranges {
		c, u := snr.usage()
		v4count = v4count + c
		v4used = v4used + u
	}
	for _, snr := range sna.v6ranges {
		c, u := snr.usage()
		v6count = v6count + c
		v6used = v6used + u
	}
	return v4count, v4used, v6count, v6used
}

// AddNetworkRange makes the given range available for allocation and returns
// the number of IPv4 or IPv6 subnets in the range.
func (sna *BaseSubnetAllocator) AddNetworkRange(network *net.IPNet, hostSubnetLen int) error {
	sna.Lock()
	defer sna.Unlock()

	snr, err := newSubnetAllocatorRange(network, hostSubnetLen)
	if err != nil {
		return err
	}

	if utilnet.IsIPv6(snr.network.IP) {
		sna.v6ranges = append(sna.v6ranges, snr)
	} else {
		sna.v4ranges = append(sna.v4ranges, snr)
	}
	return nil
}

func (sna *BaseSubnetAllocator) MarkAllocatedNetworks(subnets ...*net.IPNet) error {
	sna.Lock()
	defer sna.Unlock()

	releaseOnError := make([]*net.IPNet, 0, len(subnets))
	for i := range subnets {
		marked := false
		for _, snr := range sna.v4ranges {
			if snr.markAllocatedNetwork(subnets[i]) {
				releaseOnError = append(releaseOnError, subnets[i])
				marked = true
				break
			}
		}
		if !marked {
			for _, snr := range sna.v6ranges {
				if snr.markAllocatedNetwork(subnets[i]) {
					releaseOnError = append(releaseOnError, subnets[i])
					marked = true
					break
				}
			}
		}
		if !marked {
			_ = sna.releaseNetworks(releaseOnError...)
			return fmt.Errorf("network %s does not belong to any known range", subnets[i].String())
		}
	}
	return nil
}

// AllocateNetworks tries to allocate networks in all the ranges available
func (sna *BaseSubnetAllocator) AllocateNetworks() ([]*net.IPNet, error) {
	var networks []*net.IPNet
	var err error
	ipv4network, err := sna.AllocateIPv4Network()
	if err != nil {
		return nil, err
	}
	if ipv4network != nil {
		networks = append(networks, ipv4network)
	}
	ipv6network, err := sna.AllocateIPv6Network()
	if err != nil {
		// Release already allocated networks on error
		if len(networks) > 0 {
			_ = sna.ReleaseNetworks(networks...)
		}
		return nil, err
	}
	if ipv6network != nil {
		networks = append(networks, ipv6network)
	}
	return networks, nil
}

// AllocateIPv4Network tries to allocate an IPv4 network if there are ranges available
func (sna *BaseSubnetAllocator) AllocateIPv4Network() (*net.IPNet, error) {
	sna.Lock()
	defer sna.Unlock()
	if len(sna.v4ranges) == 0 {
		return nil, nil
	}
	for _, snr := range sna.v4ranges {
		sn := snr.allocateNetwork()
		if sn != nil {
			return sn, nil
		}
	}
	return nil, ErrSubnetAllocatorFull
}

// AllocateIPv6Network tries to allocate an IPv6 network if there are ranges available
func (sna *BaseSubnetAllocator) AllocateIPv6Network() (*net.IPNet, error) {
	sna.Lock()
	defer sna.Unlock()
	if len(sna.v6ranges) == 0 {
		return nil, nil
	}
	for _, snr := range sna.v6ranges {
		sn := snr.allocateNetwork()
		if sn != nil {
			return sn, nil
		}
	}
	return nil, ErrSubnetAllocatorFull
}

func (sna *BaseSubnetAllocator) ReleaseNetworks(subnets ...*net.IPNet) error {
	sna.Lock()
	defer sna.Unlock()
	return sna.releaseNetworks(subnets...)
}

func (sna *BaseSubnetAllocator) releaseNetworks(subnets ...*net.IPNet) error {
	var errorList []error
	for _, subnet := range subnets {
		released := false
		for _, snr := range sna.v4ranges {
			if snr.releaseNetwork(subnet) {
				released = true
				break
			}
		}
		if !released {
			for _, snr := range sna.v6ranges {
				if snr.releaseNetwork(subnet) {
					released = true
					break
				}
			}
		}
		if !released {
			errorList = append(errorList, fmt.Errorf("network %s does not belong to any known range", subnet.String()))
		}
	}

	return utilerrors.NewAggregate(errorList)
}

// subnetAllocatorRange handles allocating subnets out of a single CIDR
type subnetAllocatorRange struct {
	network    *net.IPNet
	hostBits   uint32
	subnetBits uint32
	next       uint32
	allocMap   map[string]bool
	used       uint32

	// IPv4-only address-alignment hackery; see below
	leftShift  uint32
	leftMask   uint32
	rightShift uint32
	rightMask  uint32
}

func newSubnetAllocatorRange(network *net.IPNet, hostSubnetLen int) (*subnetAllocatorRange, error) {
	clusterCIDRLen, addrLen := network.Mask.Size()
	if hostSubnetLen >= addrLen {
		return nil, fmt.Errorf("host capacity cannot be zero.")
	} else if hostSubnetLen < clusterCIDRLen {
		return nil, fmt.Errorf("subnet capacity cannot be larger than number of networks available.")
	}
	hostBits := uint32(addrLen - hostSubnetLen)
	subnetBits := uint32(hostSubnetLen - clusterCIDRLen)

	snr := &subnetAllocatorRange{
		network:    network,
		hostBits:   hostBits,
		subnetBits: subnetBits,
		next:       0,
		allocMap:   make(map[string]bool),
	}

	// In the simple case, the subnet part of the 32-bit IP address is just the subnet
	// number shifted hostBits to the left. However, if hostBits isn't a multiple of
	// 8, then it can be difficult to distinguish the subnet part and the host part
	// visually. (Eg, given network="10.1.0.0/16" and hostBits=6, then "10.1.0.50" and
	// "10.1.0.70" are on different networks.)
	//
	// To try to avoid this confusion, if the subnet extends into the next higher
	// octet, we rotate the bits of the subnet number so that we use the subnets with
	// all 0s in the shared octet first. So again given network="10.1.0.0/16",
	// hostBits=6, we first allocate 10.1.0.0/26, 10.1.1.0/26, etc, through
	// 10.1.255.0/26 (just like we would with /24s in the hostBits=8 case), and only
	// if we use up all of those subnets do we start allocating 10.1.0.64/26,
	// 10.1.1.64/26, etc.
	//
	// (For IPv6 we just don't bother worrying about this.)
	if addrLen == 32 && hostBits%8 != 0 && ((hostBits-1)/8 != (hostBits+subnetBits-1)/8) {
		// leftShift is used to move the subnet id left by the number of bits that
		// the subnet part extends into the overlap octet (which is to say, the
		// number of bits that the host part ISN'T using in that octet). leftMask
		// masks out the bits that get shifted left out of the subnet part
		snr.leftShift = 8 - (hostBits % 8)
		snr.leftMask = 1<<subnetBits - 1
		// rightShift and rightMask are used to copy the shifted-out upper bits of
		// the subnet id back down to the lower bits
		snr.rightShift = subnetBits - snr.leftShift
		snr.rightMask = 1<<snr.leftShift - 1
	}

	return snr, nil
}

// usage returns the number of available subnets and the number of allocated subnets
func (snr *subnetAllocatorRange) usage() (uint64, uint64) {
	var one uint64 = 1
	return one << snr.subnetBits, uint64(snr.used)
}

// markAllocatedNetwork marks network as being in use, if it is part of snr's range.
// It returns whether the network was in snr's range.
func (snr *subnetAllocatorRange) markAllocatedNetwork(network *net.IPNet) bool {
	str := network.String()
	if snr.network.Contains(network.IP) {
		snr.allocMap[str] = true
		snr.used++
	}
	return snr.allocMap[str]
}

// allocateNetwork returns a new subnet, or nil if the range is full
func (snr *subnetAllocatorRange) allocateNetwork() *net.IPNet {
	netMaskSize, addrLen := snr.network.Mask.Size()
	numSubnets := uint32(1) << snr.subnetBits
	if snr.subnetBits > 24 {
		// We need to make sure that the uint32 math below won't overflow. If
		// snr.subnetBits > 32 then numSubnets has already overflowed, but also if
		// numSubnets is between 1<<24 and 1<<32 then "base << (snr.hostBits % 8)"
		// below could overflow if snr.hostBits%8 is non-0. So we cap numSubnets
		// at 1<<24. "16M subnets ought to be enough for anybody."
		numSubnets = 1 << 24
	}

	var i uint32
	for i = 0; i < numSubnets; i++ {
		n := (i + snr.next) % numSubnets
		base := n
		if snr.leftShift != 0 {
			base = ((base << snr.leftShift) & snr.leftMask) | ((base >> snr.rightShift) & snr.rightMask)
		} else if addrLen == 128 && snr.subnetBits >= 16 {
			// Skip the 0 subnet (and other subnets with all 0s in the low word)
			// since the extra 0 word will get compressed out and make the address
			// look different from addresses on other subnets.
			if (base & 0xFFFF) == 0 {
				continue
			}
		}

		genIP := append([]byte{}, []byte(snr.network.IP)...)
		subnetBits := base << (snr.hostBits % 8)
		b := (uint32(addrLen) - snr.hostBits - 1) / 8
		for subnetBits != 0 {
			genIP[b] |= byte(subnetBits)
			subnetBits >>= 8
			b--
		}

		genSubnet := &net.IPNet{IP: genIP, Mask: net.CIDRMask(int(snr.subnetBits)+netMaskSize, addrLen)}
		if !snr.allocMap[genSubnet.String()] {
			snr.allocMap[genSubnet.String()] = true
			snr.next = n + 1
			snr.used++
			return genSubnet
		}
	}

	snr.next = 0
	return nil
}

// releaseNetwork marks network as being not in use, if it is part of snr's range.
// It returns whether the network was in snr's range.
func (snr *subnetAllocatorRange) releaseNetwork(network *net.IPNet) bool {
	if !snr.network.Contains(network.IP) {
		return false
	}

	snr.allocMap[network.String()] = false
	snr.used--
	return true
}

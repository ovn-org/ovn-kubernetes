package allocator

import (
	"fmt"
	"net"
	"testing"
)

func newSubnetAllocator(clusterCIDR string, hostBits uint32) (*SubnetAllocator, error) {
	sna := NewSubnetAllocator()
	err := sna.AddNetworkRange(clusterCIDR, hostBits)
	return sna, err
}

func networkID(n int) string {
	if n == -1 {
		return "network"
	} else {
		return fmt.Sprintf("network %d", n)
	}
}

func allocateOneNetwork(sna *SubnetAllocator) (*net.IPNet, error) {
	sns, err := sna.AllocateNetworks()
	if err != nil {
		return nil, err
	}
	if len(sns) != 1 {
		return nil, fmt.Errorf("unexpectedly got multiple subnets: %v", sns)
	}
	return sns[0], nil
}

func allocateExpected(sna *SubnetAllocator, n int, expected ...string) error {
	// Canonicalize expected; eg "fd01:0:0:0::/64" -> "fd01::/64"
	for i, str := range expected {
		_, cidr, _ := net.ParseCIDR(str)
		expected[i] = cidr.String()
	}

	sns, err := sna.AllocateNetworks()
	if err != nil {
		return fmt.Errorf("failed to allocate %s (%s): %v", networkID(n), expected, err)
	}
	if len(sns) != len(expected) {
		return fmt.Errorf("wrong number of networks for %s: expected %d, got %d", networkID(n), len(expected), len(sns))
	}
	for i := range sns {
		if sns[i].String() != expected[i] {
			return fmt.Errorf("failed to allocate %s: expected %s, got %s", networkID(n), expected[i], sns[i].String())
		}
	}
	return nil
}

func allocateNotExpected(sna *SubnetAllocator, n int) error {
	if sns, err := sna.AllocateNetworks(); err == nil {
		return fmt.Errorf("unexpectedly succeeded in allocating %s (sns=%v)", networkID(n), sns)
	} else if err != ErrSubnetAllocatorFull {
		return fmt.Errorf("returned error was not ErrSubnetAllocatorFull (%v)", err)
	}
	return nil
}

// 10.1.ssssssss.hhhhhhhh
func TestAllocateSubnetIPv4(t *testing.T) {
	sna, err := newSubnetAllocator("10.1.0.0/16", 8)
	if err != nil {
		t.Fatal("Failed to initialize subnet allocator: ", err)
	}

	for n := 0; n < 256; n++ {
		if err := allocateExpected(sna, n, fmt.Sprintf("10.1.%d.0/24", n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := allocateNotExpected(sna, 256); err != nil {
		t.Fatal(err)
	}
}

// fd01:0:0:SSSS:HHHH:HHHH:HHHH:HHHH
func TestAllocateSubnetIPv6(t *testing.T) {
	sna, err := newSubnetAllocator("fd01::/48", 64)
	if err != nil {
		t.Fatal("Failed to initialize subnet allocator: ", err)
	}

	// IPv6 allocation skips the 0 subnet, so we start with n=1
	for n := 1; n < 256; n++ {
		if err := allocateExpected(sna, n, fmt.Sprintf("fd01:0:0:%x::/64", n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := allocateExpected(sna, 256, "fd01:0:0:100::/64"); err != nil {
		t.Fatal(err)
	}

	// We have 16 bits for subnet, after which it will wrap around (and then allocate
	// the next previously-unallocated value, skipping the 0 subnet again).
	sna.v6ranges[0].next = 0xFFFF
	if err := allocateExpected(sna, -1, "fd01:0:0:ffff::/64"); err != nil {
		t.Fatal(err)
	}
	if err := allocateExpected(sna, -1, "fd01:0:0:101::/64"); err != nil {
		t.Fatal(err)
	}
}

// 10.1.sssssshh.hhhhhhhh
func TestAllocateSubnetLargeHostBitsIPv4(t *testing.T) {
	sna, err := newSubnetAllocator("10.1.0.0/16", 10)
	if err != nil {
		t.Fatal("Failed to initialize subnet allocator: ", err)
	}

	for n := 0; n < 64; n++ {
		if err := allocateExpected(sna, n, fmt.Sprintf("10.1.%d.0/22", n*4)); err != nil {
			t.Fatal(err)
		}
	}
	if err := allocateNotExpected(sna, 64); err != nil {
		t.Fatal(err)
	}
}

// fd01:0:0:SSSH:HHHH:HHHH:HHHH:HHHH
func TestAllocateSubnetLargeHostBitsIPv6(t *testing.T) {
	sna, err := newSubnetAllocator("fd01::/48", 68)
	if err != nil {
		t.Fatal("Failed to initialize subnet allocator: ", err)
	}

	// (Because of the small subnet size we won't skip the 0 subnet like in the
	// other IPv6 cases.)
	for n := 0; n < 256; n++ {
		if err := allocateExpected(sna, n, fmt.Sprintf("fd01:0:0:%x::/60", n<<4)); err != nil {
			t.Fatal(err)
		}
	}
}

// 10.1.ssssssss.sshhhhhh
func TestAllocateSubnetLargeSubnetBitsIPv4(t *testing.T) {
	sna, err := newSubnetAllocator("10.1.0.0/16", 6)
	if err != nil {
		t.Fatal("Failed to initialize subnet allocator: ", err)
	}

	// for IPv4, we tweak the allocation order and expect to see all of the ".0"
	// networks before any non-".0" network
	for n := 0; n < 256; n++ {
		if err = allocateExpected(sna, n, fmt.Sprintf("10.1.%d.0/26", n)); err != nil {
			t.Fatal(err)
		}
	}
	for n := 0; n < 256; n++ {
		if err = allocateExpected(sna, n+256, fmt.Sprintf("10.1.%d.64/26", n)); err != nil {
			t.Fatal(err)
		}
	}
	if err = allocateExpected(sna, 512, "10.1.0.128/26"); err != nil {
		t.Fatal(err)
	}

	sna.v4ranges[0].next = 1023
	if err = allocateExpected(sna, -1, "10.1.255.192/26"); err != nil {
		t.Fatal(err)
	}
	// Next allocation should wrap around and get the next unallocated network (513)
	if err = allocateExpected(sna, -1, "10.1.1.128/26"); err != nil {
		t.Fatalf("After wraparound: %v", err)
	}
}

// fd01:0:0:SSSS:SSSS:SHHH:HHHH:HHHH
func TestAllocateSubnetLargeSubnetBitsIPv6(t *testing.T) {
	sna, err := newSubnetAllocator("fd01::/48", 44)
	if err != nil {
		t.Fatal("Failed to initialize subnet allocator: ", err)
	}

	// For IPv6 we expect to see the networks just get allocated in order
	for n := 1; n < 256; n++ {
		// (Many of the IPv6 strings we Sprintf here won't be in canonical
		// format, but allocateExpecting() will fix them for us.)
		if err := allocateExpected(sna, n, fmt.Sprintf("fd01:0:0:0:%x:%x::/84", n>>4, (n<<12)&0xFFFF)); err != nil {
			t.Fatal(err)
		}
	}
	if err := allocateExpected(sna, -1, "fd01:0:0:0:10:0::/84"); err != nil {
		t.Fatal(err)
	}

	// Even though we theoretically have 36 bits of subnets, SubnetAllocator will only
	// use the lower 24 bits before looping around and then allocating the next
	// previously-unallocated subnet.
	sna.v6ranges[0].next = 0x00FFFFFF
	if err := allocateExpected(sna, -1, "fd01:0:0:000f:ffff:f000::/84"); err != nil {
		t.Fatal(err)
	}
	if err := allocateExpected(sna, -1, "fd01:0:0:0:10:1000::/84"); err != nil {
		t.Fatal(err)
	}
}

// 10.000000ss.sssssshh.hhhhhhhh
func TestAllocateSubnetOverlappingIPv4(t *testing.T) {
	sna, err := newSubnetAllocator("10.0.0.0/14", 10)
	if err != nil {
		t.Fatal("Failed to initialize subnet allocator: ", err)
	}

	for n := 0; n < 4; n++ {
		if err = allocateExpected(sna, n, fmt.Sprintf("10.%d.0.0/22", n)); err != nil {
			t.Fatal(err)
		}
	}
	for n := 0; n < 4; n++ {
		if err = allocateExpected(sna, n+4, fmt.Sprintf("10.%d.4.0/22", n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := allocateExpected(sna, 8, "10.0.8.0/22"); err != nil {
		t.Fatal(err)
	}

	sna.v4ranges[0].next = 255
	if err := allocateExpected(sna, -1, "10.3.252.0/22"); err != nil {
		t.Fatal(err)
	}
	if err := allocateExpected(sna, -1, "10.1.8.0/22"); err != nil {
		t.Fatalf("After wraparound: %v", err)
	}
}

// There's no TestAllocateSubnetOverlappingIPv6 because it wouldn't be any different from
// TestAllocateSubnetLargeSubnetBitsIPv6.

// 10.1.hhhhhhhh.hhhhhhhh
func TestAllocateSubnetNoSubnetBitsIPv4(t *testing.T) {
	sna, err := newSubnetAllocator("10.1.0.0/16", 16)
	if err != nil {
		t.Fatal("Failed to initialize subnet allocator: ", err)
	}

	if err := allocateExpected(sna, 0, "10.1.0.0/16"); err != nil {
		t.Fatal(err)
	}
	if err := allocateNotExpected(sna, 1); err != nil {
		t.Fatal(err)
	}
}

func TestAllocateSubnetNoSubnetBitsIPv6(t *testing.T) {
	// fd01:0:0:0:HHHH:HHHH:HHHH:HHHH
	sna, err := newSubnetAllocator("fd01::/64", 64)
	if err != nil {
		t.Fatal("Failed to initialize subnet allocator: ", err)
	}

	if err := allocateExpected(sna, 0, "fd01::/64"); err != nil {
		t.Fatal(err)
	}
	if err := allocateNotExpected(sna, 1); err != nil {
		t.Fatal(err)
	}
}

func TestAllocateSubnetInvalidHostBitsOrCIDR(t *testing.T) {
	_, err := newSubnetAllocator("10.1.0.0/16", 18)
	if err == nil {
		t.Fatal("Unexpectedly succeeded in initializing subnet allocator")
	}

	_, err = newSubnetAllocator("10.1.0.0/16", 0)
	if err == nil {
		t.Fatal("Unexpectedly succeeded in initializing subnet allocator")
	}

	_, err = newSubnetAllocator("10.1.0.0/33", 16)
	if err == nil {
		t.Fatal("Unexpectedly succeeded in initializing subnet allocator")
	}

	_, err = newSubnetAllocator("fd01::/64", 66)
	if err == nil {
		t.Fatal("Unexpectedly succeeded in initializing subnet allocator")
	}

	_, err = newSubnetAllocator("fd01::/64", 0)
	if err == nil {
		t.Fatal("Unexpectedly succeeded in initializing subnet allocator")
	}

	_, err = newSubnetAllocator("fd01::/129", 64)
	if err == nil {
		t.Fatal("Unexpectedly succeeded in initializing subnet allocator")
	}
}

func TestMarkAllocatedNetwork(t *testing.T) {
	sna, err := newSubnetAllocator("10.1.0.0/16", 14)
	if err != nil {
		t.Fatal("Failed to initialize IP allocator: ", err)
	}

	allocSubnets := make([]*net.IPNet, 4)
	for i := 0; i < 4; i++ {
		if allocSubnets[i], err = allocateOneNetwork(sna); err != nil {
			t.Fatal("Failed to allocate network: ", err)
		}
	}

	if sn, err := allocateOneNetwork(sna); err == nil {
		t.Fatalf("Unexpectedly succeeded in allocating network (sn=%s)", sn.String())
	}
	if err := sna.ReleaseNetwork(allocSubnets[2]); err != nil {
		t.Fatalf("Failed to release the subnet (allocSubnets[2]=%s): %v", allocSubnets[2].String(), err)
	}
	for i := 0; i < 2; i++ {
		if err := sna.MarkAllocatedNetwork(allocSubnets[2]); err != nil {
			t.Fatalf("Failed to mark allocated subnet (allocSubnets[2]=%s): %v", allocSubnets[2].String(), err)
		}
	}
	if sn, err := allocateOneNetwork(sna); err == nil {
		t.Fatalf("Unexpectedly succeeded in allocating network (sn=%s)", sn.String())
	}

	// Test subnet that does not belong to network
	_, subnet, _ := net.ParseCIDR("10.2.3.4/24")
	if err := sna.MarkAllocatedNetwork(subnet); err == nil {
		t.Fatalf("Unexpectedly succeeded in marking allocated subnet that doesn't belong to network (sn=%s)", subnet.String())
	}
}

func TestAllocateReleaseSubnet(t *testing.T) {
	sna, err := newSubnetAllocator("10.1.0.0/16", 14)
	if err != nil {
		t.Fatal("Failed to initialize IP allocator: ", err)
	}

	var releaseSn *net.IPNet

	for i := 0; i < 4; i++ {
		sn, err := allocateOneNetwork(sna)
		if err != nil {
			t.Fatal("Failed to allocate network: ", err)
		}
		if sn.String() != fmt.Sprintf("10.1.%d.0/18", i*64) {
			t.Fatalf("Did not get expected subnet (i=%d, sn=%s)", i, sn.String())
		}
		if i == 2 {
			releaseSn = sn
		}
	}

	sn, err := allocateOneNetwork(sna)
	if err == nil {
		t.Fatalf("Unexpectedly succeeded in allocating network (sn=%s)", sn.String())
	}

	if err := sna.ReleaseNetwork(releaseSn); err != nil {
		t.Fatalf("Failed to release the subnet (releaseSn=%s): %v", releaseSn, err)
	}

	sn, err = allocateOneNetwork(sna)
	if err != nil {
		t.Fatal("Failed to allocate network: ", err)
	}
	if sn.String() != releaseSn.String() {
		t.Fatalf("Did not get expected subnet (sn=%s)", sn.String())
	}

	sn, err = allocateOneNetwork(sna)
	if err == nil {
		t.Fatalf("Unexpectedly succeeded in allocating network (sn=%s)", sn.String())
	}
}

func TestMultipleSubnets(t *testing.T) {
	sna, err := newSubnetAllocator("10.1.0.0/16", 14)
	if err != nil {
		t.Fatal("Failed to initialize IP allocator: ", err)
	}
	err = sna.AddNetworkRange("10.2.0.0/16", 14)
	if err != nil {
		t.Fatal("Failed to add network range: ", err)
	}

	for i := 0; i < 4; i++ {
		if err := allocateExpected(sna, i, fmt.Sprintf("10.1.%d.0/18", i*64)); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 4; i++ {
		if err := allocateExpected(sna, i+4, fmt.Sprintf("10.2.%d.0/18", i*64)); err != nil {
			t.Fatal(err)
		}
	}

	if err := allocateNotExpected(sna, 8); err != nil {
		t.Fatal(err)
	}

	_, sn, _ := net.ParseCIDR("10.1.128.0/18")
	if err := sna.ReleaseNetwork(sn); err != nil {
		t.Fatalf("Failed to release the subnet %s: %v", sn.String(), err)
	}
	_, sn, _ = net.ParseCIDR("10.2.128.0/18")
	if err := sna.ReleaseNetwork(sn); err != nil {
		t.Fatalf("Failed to release the subnet %s: %v", sn.String(), err)
	}

	if err := allocateExpected(sna, -1, "10.1.128.0/18"); err != nil {
		t.Fatal(err)
	}
	if err := allocateExpected(sna, -1, "10.2.128.0/18"); err != nil {
		t.Fatal(err)
	}

	if err := allocateNotExpected(sna, -1); err != nil {
		t.Fatal(err)
	}
}

func TestDualStack(t *testing.T) {
	sna, err := newSubnetAllocator("10.1.0.0/16", 14)
	if err != nil {
		t.Fatal("Failed to initialize IP allocator: ", err)
	}
	err = sna.AddNetworkRange("10.2.0.0/16", 14)
	if err != nil {
		t.Fatal("Failed to add network range: ", err)
	}
	err = sna.AddNetworkRange("fd01::/48", 64)
	if err != nil {
		t.Fatal("Failed to add network range: ", err)
	}

	for i := 0; i < 4; i++ {
		if err := allocateExpected(sna, i,
			fmt.Sprintf("10.1.%d.0/18", i*64),
			fmt.Sprintf("fd01:0:0:%x::/64", i+1),
		); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		if err := allocateExpected(sna, i+4,
			fmt.Sprintf("10.2.%d.0/18", i*64),
			fmt.Sprintf("fd01:0:0:%x::/64", i+5),
		); err != nil {
			t.Fatal(err)
		}
	}

	if err := allocateNotExpected(sna, 8); err != nil {
		t.Fatal(err)
	}

	_, sn, _ := net.ParseCIDR("10.1.128.0/18")
	if err := sna.ReleaseNetwork(sn); err != nil {
		t.Fatalf("Failed to release the subnet %s: %v", sn.String(), err)
	}
	_, sn, _ = net.ParseCIDR("fd01:0:0:3::/64")
	if err := sna.ReleaseNetwork(sn); err != nil {
		t.Fatalf("Failed to release the subnet %s: %v", sn.String(), err)
	}
	_, sn, _ = net.ParseCIDR("10.2.128.0/18")
	if err := sna.ReleaseNetwork(sn); err != nil {
		t.Fatalf("Failed to release the subnet %s: %v", sn.String(), err)
	}
	_, sn, _ = net.ParseCIDR("fd01:0:0:7::/64")
	if err := sna.ReleaseNetwork(sn); err != nil {
		t.Fatalf("Failed to release the subnet %s: %v", sn.String(), err)
	}

	// The IPv4 allocator will now reuse the freed subnets (since they're all it has
	// left), but the IPv6 allocator will continue allocating new ones.
	if err := allocateExpected(sna, -1, "10.1.128.0/18", "fd01:0:0:9::/64"); err != nil {
		t.Fatal(err)
	}
	if err := allocateExpected(sna, -1, "10.2.128.0/18", "fd01:0:0:a::/64"); err != nil {
		t.Fatal(err)
	}

	if err := allocateNotExpected(sna, -1); err != nil {
		t.Fatal(err)
	}
}

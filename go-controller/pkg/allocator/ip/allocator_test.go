// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ip

import (
	"fmt"
	"net"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
)

func TestAllocate(t *testing.T) {
	testCases := []struct {
		name             string
		cidr             string
		free             int
		released         string
		outOfRange       []string
		alreadyAllocated string
	}{
		{
			name:     "IPv4",
			cidr:     "192.168.1.0/24",
			free:     254,
			released: "192.168.1.5",
			outOfRange: []string{
				"192.168.0.1",   // not in 192.168.1.0/24
				"192.168.1.0",   // reserved (base address)
				"192.168.1.255", // reserved (broadcast address)
				"192.168.2.2",   // not in 192.168.1.0/24
			},
			alreadyAllocated: "192.168.1.1",
		},
		{
			name:     "IPv6",
			cidr:     "2001:db8:1::/48",
			free:     65535,
			released: "2001:db8:1::5",
			outOfRange: []string{
				"2001:db8::1",   // not in 2001:db8:1::/48
				"2001:db8:1::",  // reserved (base address)
				"2001:db8:2::2", // not in 2001:db8:1::/48
			},
			alreadyAllocated: "2001:db8:1::1",
		},
		{
			name:     "IPv6",
			cidr:     "2605:b100:283:1::/64",
			free:     65535,
			released: "2605:b100:283:1::e",
			outOfRange: []string{
				"2605:b100:283:0::1", // not in 2605:b100:283:1::/64
				"2605:b100:283:1::",  // reserved (base address)
				"2605:b100:284:2::2", // not in 2605:b100:283:1::/64
			},
			alreadyAllocated: "2605:b100:283:1::1",
		},
	}
	for _, tc := range testCases {
		_, cidr, err := net.ParseCIDR(tc.cidr)
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewCIDRRange(cidr)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("base: %v", r.base.Bytes())
		if f := r.Free(); f != tc.free {
			t.Errorf("Test %s unexpected free %d", tc.name, f)
		}

		rCIDR := r.CIDR()
		if rCIDR.String() != tc.cidr {
			t.Errorf("allocator returned a different cidr")
		}

		if f := r.Used(); f != 0 {
			t.Errorf("Test %s unexpected used %d", tc.name, f)
		}
		found := sets.NewString()
		count := 0
		for r.Free() > 0 {
			ip, err := r.AllocateNext()
			if err != nil {
				t.Fatalf("Test %s error @ %d: %v", tc.name, count, err)
			}
			count++
			if !cidr.Contains(ip) {
				t.Fatalf("Test %s allocated %s which is outside of %s", tc.name, ip, cidr)
			}
			if found.Has(ip.String()) {
				t.Fatalf("Test %s allocated %s twice @ %d", tc.name, ip, count)
			}
			found.Insert(ip.String())
		}
		if _, err := r.AllocateNext(); err != ErrFull {
			t.Fatal(err)
		}
		released := net.ParseIP(tc.released)
		r.Release(released)
		if f := r.Free(); f != 1 {
			t.Errorf("Test %s unexpected free %d", tc.name, f)
		}
		if f := r.Used(); f != (tc.free - 1) {
			t.Errorf("Test %s unexpected free %d", tc.name, f)
		}
		ip, err := r.AllocateNext()
		if err != nil {
			t.Fatal(err)
		}
		if !released.Equal(ip) {
			t.Errorf("Test %s unexpected %s : %s", tc.name, ip, released)
		}

		r.Release(released)
		for _, outOfRange := range tc.outOfRange {
			err = r.Allocate(net.ParseIP(outOfRange))
			if _, ok := err.(*ErrNotInRange); !ok {
				t.Fatal(err)
			}
		}

		if err := r.Allocate(net.ParseIP(tc.alreadyAllocated)); err != ErrAllocated {
			t.Fatal(err)
		}
		if f := r.Free(); f != 1 {
			t.Errorf("Test %s unexpected free %d", tc.name, f)
		}
		if f := r.Used(); f != (tc.free - 1) {
			t.Errorf("Test %s unexpected free %d", tc.name, f)
		}
		if err := r.Allocate(released); err != nil {
			t.Fatal(err)
		}
		if f := r.Free(); f != 0 {
			t.Errorf("Test %s unexpected free %d", tc.name, f)
		}
		if f := r.Used(); f != tc.free {
			t.Errorf("Test %s unexpected free %d", tc.name, f)
		}
	}
}

func TestAllocateTiny(t *testing.T) {
	_, cidr, err := net.ParseCIDR("192.168.1.0/32")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewCIDRRange(cidr)
	if err != nil {
		t.Fatal(err)
	}
	if f := r.Free(); f != 0 {
		t.Errorf("free: %d", f)
	}
	if _, err := r.AllocateNext(); err != ErrFull {
		t.Error(err)
	}
}

func TestAllocateSmall(t *testing.T) {
	_, cidr, err := net.ParseCIDR("192.168.1.240/30")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewCIDRRange(cidr)
	if err != nil {
		t.Fatal(err)
	}
	if f := r.Free(); f != 2 {
		t.Errorf("free: %d", f)
	}
	found := sets.NewString()
	for i := 0; i < 2; i++ {
		ip, err := r.AllocateNext()
		if err != nil {
			t.Fatal(err)
		}
		if found.Has(ip.String()) {
			t.Fatalf("already reserved: %s", ip)
		}
		found.Insert(ip.String())
	}
	for s := range found {
		if !r.Has(net.ParseIP(s)) {
			t.Fatalf("missing: %s", s)
		}
		if err := r.Allocate(net.ParseIP(s)); err != ErrAllocated {
			t.Fatal(err)
		}
	}
	for i := 0; i < 100; i++ {
		if _, err := r.AllocateNext(); err != ErrFull {
			t.Fatalf("suddenly became not-full: %#v", r)
		}
	}

	if r.Free() != 0 && r.max != 2 {
		t.Fatalf("unexpected range: %v", r)
	}

	t.Logf("allocated: %v", found)
}

func TestForEach(t *testing.T) {
	_, cidr, err := net.ParseCIDR("192.168.1.0/24")
	if err != nil {
		t.Fatal(err)
	}

	testCases := []sets.Set[string]{
		sets.New[string](),
		sets.New("192.168.1.1"),
		sets.New("192.168.1.1", "192.168.1.254"),
		sets.New("192.168.1.1", "192.168.1.128", "192.168.1.254"),
	}

	for i, tc := range testCases {
		r, err := NewCIDRRange(cidr)
		if err != nil {
			t.Fatal(err)
		}
		for ips := range tc {
			ip := net.ParseIP(ips)
			if err := r.Allocate(ip); err != nil {
				t.Errorf("[%d] error allocating IP %v: %v", i, ip, err)
			}
			if !r.Has(ip) {
				t.Errorf("[%d] expected IP %v allocated", i, ip)
			}
		}
		calls := sets.New[string]()
		r.ForEach(func(ip net.IP) {
			calls.Insert(ip.String())
		})
		if len(calls) != len(tc) {
			t.Errorf("[%d] expected %d calls, got %d", i, len(tc), len(calls))
		}
		if !calls.Equal(tc) {
			t.Errorf("[%d] expected calls to equal testcase: %v vs %v", i, sets.List(calls), sets.List(tc))
		}
	}
}

func TestReserved(t *testing.T) {
	_, cidr, err := net.ParseCIDR("192.168.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewCIDRRange(cidr)
	if err != nil {
		t.Fatal(err)
	}

	if !r.Reserved(net.ParseIP("192.168.1.0")) {
		t.Errorf("should be a reserved address: %s", "192.168.1.0")
	}

	if !r.Reserved(net.ParseIP("192.168.1.255")) {
		t.Errorf("should be a reserved address: %s", "192.168.1.255")
	}

	if r.Reserved(net.ParseIP("192.168.1.1")) {
		t.Errorf("should not be a reserved address: %s", "192.168.1.1")
	}

	if r.Reserved(net.ParseIP("192.168.1.254")) {
		t.Errorf("should not be a reserved address: %s", "192.168.1.254")
	}
}

func TestIPv6StaticAllocationBeyondBitmapCap(t *testing.T) {
	testCases := []struct {
		name  string
		cidr  string
		ips   []string
		notIn []string // IPs truly outside the CIDR
	}{
		{
			name: "/64 subnet",
			cidr: "2001:db8:1::/64",
			ips: []string{
				"2001:db8:1::1:0",       // offset 65536
				"2001:db8:1::2:0",       // offset 131072
				"2001:db8:1::ffff:ffff", // large offset
			},
			notIn: []string{
				"2001:db8:2::1", // different subnet
			},
		},
		{
			name: "/48 subnet",
			cidr: "2001:db8:1::/48",
			ips: []string{
				"2001:db8:1::1:0",     // offset 65536
				"2001:db8:1:1::1",     // offset in different /64 block
				"2001:db8:1:ffff::99", // far into the /48
			},
			notIn: []string{
				"2001:db8:2::1", // different /48
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, cidr, err := net.ParseCIDR(tc.cidr)
			if err != nil {
				t.Fatalf("failed to parse CIDR %q: %v", tc.cidr, err)
			}
			r, err := NewCIDRRange(cidr)
			if err != nil {
				t.Fatalf("failed to create Range for CIDR %q: %v", tc.cidr, err)
			}

			// Allocate IPs beyond bitmap cap
			for _, ipStr := range tc.ips {
				ip := net.ParseIP(ipStr)
				if err := r.Allocate(ip); err != nil {
					t.Errorf("Allocate(%s) should succeed, got: %v", ipStr, err)
				}
				if !r.Has(ip) {
					t.Errorf("Has(%s) should return true after allocation", ipStr)
				}
			}

			// Double allocation must fail with ErrAllocated
			for _, ipStr := range tc.ips {
				ip := net.ParseIP(ipStr)
				if err := r.Allocate(ip); err != ErrAllocated {
					t.Errorf("Allocate(%s) twice should return ErrAllocated, got: %v", ipStr, err)
				}
			}

			// IPs outside the CIDR must still be rejected
			for _, ipStr := range tc.notIn {
				ip := net.ParseIP(ipStr)
				err := r.Allocate(ip)
				if _, ok := err.(*ErrNotInRange); !ok {
					t.Errorf("Allocate(%s) should return ErrNotInRange, got: %v", ipStr, err)
				}
			}

			// Release and re-allocate
			for _, ipStr := range tc.ips {
				ip := net.ParseIP(ipStr)
				r.Release(ip)
				if r.Has(ip) {
					t.Errorf("Has(%s) should return false after release", ipStr)
				}
				if err := r.Allocate(ip); err != nil {
					t.Errorf("re-Allocate(%s) after release should succeed, got: %v", ipStr, err)
				}
			}

			// ForEach must include out-of-range static IPs
			found := sets.New[string]()
			r.ForEach(func(ip net.IP) {
				found.Insert(ip.String())
			})
			for _, ipStr := range tc.ips {
				canonical := net.ParseIP(ipStr).String()
				if !found.Has(canonical) {
					t.Errorf("ForEach should include %s", ipStr)
				}
			}
		})
	}
}

func TestIPv6StaticAndDynamicCoexistence(t *testing.T) {
	_, cidr, err := net.ParseCIDR("2001:db8:1::/64")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewCIDRRange(cidr)
	if err != nil {
		t.Fatal(err)
	}

	// Static allocation within bitmap range
	inRange := net.ParseIP("2001:db8:1::100")
	if err := r.Allocate(inRange); err != nil {
		t.Fatalf("static Allocate within bitmap should succeed: %v", err)
	}

	// Static allocation beyond bitmap range
	outRange := net.ParseIP("2001:db8:1::1:0")
	if err := r.Allocate(outRange); err != nil {
		t.Fatalf("static Allocate beyond bitmap should succeed: %v", err)
	}

	// Dynamic allocation should skip the in-range static IP
	for i := 0; i < 200; i++ {
		ip, err := r.AllocateNext()
		if err != nil {
			t.Fatalf("AllocateNext error at %d: %v", i, err)
		}
		if ip.Equal(inRange) {
			t.Fatalf("AllocateNext returned statically allocated IP %s", inRange)
		}
	}

	// Both IPs should report as allocated
	if !r.Has(inRange) {
		t.Error("Has should be true for in-range static IP")
	}
	if !r.Has(outRange) {
		t.Error("Has should be true for out-of-range static IP")
	}
}

func TestIPv6SmallSubnetUnaffected(t *testing.T) {
	// /112 has exactly 65536 addresses, should work unchanged
	_, cidr, err := net.ParseCIDR("2001:db8:1::/112")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewCIDRRange(cidr)
	if err != nil {
		t.Fatal(err)
	}

	// Last IP in /112 range should be allocatable
	ip := net.ParseIP("2001:db8:1::fffe")
	if err := r.Allocate(ip); err != nil {
		t.Errorf("Allocate last-but-one IP in /112 should succeed: %v", err)
	}

	// IP outside /112 must be rejected
	ipOutside := net.ParseIP("2001:db8:1::1:0")
	err = r.Allocate(ipOutside)
	if _, ok := err.(*ErrNotInRange); !ok {
		t.Errorf("Allocate outside /112 should return ErrNotInRange, got: %v", err)
	}
}

func TestIPv6StaticAllocationConcurrency(t *testing.T) {
	_, cidr, err := net.ParseCIDR("2001:db8:cafe::/64")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewCIDRRange(cidr)
	if err != nil {
		t.Fatal(err)
	}

	const count = 100
	var wg sync.WaitGroup
	errs := make(chan error, count)

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ip := net.ParseIP(fmt.Sprintf("2001:db8:cafe::%x:0", idx+1))
			if err := r.Allocate(ip); err != nil {
				errs <- fmt.Errorf("Allocate(%s): %w", ip, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	for i := 0; i < count; i++ {
		ip := net.ParseIP(fmt.Sprintf("2001:db8:cafe::%x:0", i+1))
		if !r.Has(ip) {
			t.Errorf("Has(%s) should be true after concurrent allocation", ip)
		}
	}
}

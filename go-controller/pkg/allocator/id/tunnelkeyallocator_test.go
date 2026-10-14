// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package id

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestReserveTunnelKeysOutOfRange(t *testing.T) {
	for _, invalid := range []int{-2, 1 << 24, math.MaxInt} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			allocator := NewTunnelKeyAllocator("test")
			transit := allocator.idsOffset + 1
			dynamic := allocator.idsOffset + allocator.preservedRange
			if err := allocator.ReserveKeys("healthy", []int{transit, dynamic}); err != nil {
				t.Fatal(err)
			}
			err := allocator.ReserveKeys("invalid", []int{transit + 1, dynamic + 1, invalid})
			if err == nil || !strings.Contains(err.Error(), "outside allocation range") {
				t.Fatalf("expected bounds error, got %v", err)
			}
			if err := allocator.ReserveKeys("conflict", []int{transit + 2, dynamic}); err == nil {
				t.Fatal("healthy reservation was released")
			}
			if err := allocator.ReserveKeys("replacement", []int{transit + 2, dynamic + 1}); err != nil {
				t.Fatalf("partial reservation leaked: %v", err)
			}
			if _, err := allocator.AllocateKeys("invalid", 2, 2); err != nil {
				t.Fatalf("failed reservation prevented retry: %v", err)
			}
		})
	}
}

func TestReserveTunnelKeysWithRollback(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			allocator := NewTunnelKeyAllocator("test")
			keys := []int{allocator.idsOffset + 1, allocator.idsOffset + allocator.preservedRange}
			if existing {
				if err := allocator.ReserveKeys("network", keys); err != nil {
					t.Fatal(err)
				}
			}
			rollback, err := allocator.ReserveKeysWithRollback("network", keys)
			if err != nil {
				t.Fatal(err)
			}
			if err := allocator.ReserveKeys("other", keys); err == nil {
				t.Fatal("reservation was not held before rollback")
			}
			rollback()
			err = allocator.ReserveKeys("other", keys)
			if existing && err == nil {
				t.Fatal("rollback released an existing reservation")
			}
			if !existing && err != nil {
				t.Fatalf("rollback leaked a new reservation: %v", err)
			}
		})
	}
}

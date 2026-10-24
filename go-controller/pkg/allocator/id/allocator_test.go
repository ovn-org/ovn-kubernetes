// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package id

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
)

func TestIDAllocator_ReleaseID(t *testing.T) {
	t.Run("returns allocated ID when releasing", func(t *testing.T) {
		allocator := NewIDAllocator("test", 10)
		id, err := allocator.AllocateID("resource1")
		if err != nil {
			t.Fatalf("AllocateID() unexpected error: %v", err)
		}

		got := allocator.ReleaseID("resource1")
		if got != id {
			t.Errorf("ReleaseID() = %d, want %d", got, id)
		}
		if allocator.GetID("resource1") != -1 {
			t.Error("GetID() should return -1 after release")
		}
	})

	t.Run("returns -1 when releasing already released resource", func(t *testing.T) {
		allocator := NewIDAllocator("test", 10)
		if _, err := allocator.AllocateID("resource1"); err != nil {
			t.Fatalf("AllocateID() unexpected error: %v", err)
		}
		allocator.ReleaseID("resource1")

		if got := allocator.ReleaseID("resource1"); got != -1 {
			t.Errorf("ReleaseID() = %d, want -1", got)
		}
	})

	t.Run("returns -1 when releasing non-existent resource", func(t *testing.T) {
		allocator := NewIDAllocator("test", 10)
		if got := allocator.ReleaseID("nonexistent"); got != -1 {
			t.Errorf("ReleaseID() = %d, want -1", got)
		}
	})
}

func TestIDsAllocator(t *testing.T) {
	// create allocator with range [3, 8]
	allocator := newIDsAllocator("test", 6, 3)
	ids, err := allocator.AllocateIDs("test1", 0)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expect 0 ids allocated, but got %v", ids)
	}
	// test reserve IDs
	err = allocator.ReserveIDs("test1", []int{4})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// ids: test1 = [4]
	// test offset and multiple IDs allocation skipping allocated ID
	ids, err = allocator.AllocateIDs("test2", 3)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !slices.Equal(ids, []int{3, 5, 6}) {
		t.Errorf("expect ids [3,5,6] allocated, but got %v", ids)
	}
	// ids: test1 = [4]
	// ids: test2 = [3,5,6]
	// try to allocate more ids for test1
	ids, err = allocator.AllocateIDs("test1", 2)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !slices.Equal(ids, []int{4, 7}) {
		t.Errorf("expect ids [4,7] allocated, but got %v", ids)
	}
	// ids: test1 = [4,7]
	// ids: test2 = [3,5,6]
	// request already existing IDs
	ids, err = allocator.AllocateIDs("test1", 2)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !slices.Equal(ids, []int{4, 7}) {
		t.Errorf("expect ids [4,7] allocated, but got %v", ids)
	}
	// ids: test1 = [4,7]
	// ids: test2 = [3,5,6]
	// try to allocate more ids than available
	ids, err = allocator.AllocateIDs("test3", 2)
	if err == nil {
		t.Errorf("expect error allocating id for test3, but got ids %v", ids)
	}
	// try to reserve last available ID
	err = allocator.ReserveIDs("test3", []int{8})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// ids: test1 = [4,7]
	// ids: test2 = [3,5,6]
	// ids: test3 = [8]
	// try to reserve different IDs
	err = allocator.ReserveIDs("test3", []int{7, 8})
	if err == nil {
		t.Errorf("expect error reserving ids for test3")
	}
	// now release IDs for test1
	allocator.ReleaseIDs("test1")
	// ids: test2 = [3,5,6]
	// ids: test3 = [8]
	// try to allocate more ids than available
	ids, err = allocator.AllocateIDs("test3", 4)
	if err == nil {
		t.Errorf("expect error allocating id for test3, but got ids %v", ids)
	}
	ids, err = allocator.AllocateIDs("test3", 3)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !slices.Equal(ids, []int{8, 4, 7}) {
		t.Errorf("expect ids [8,4,7] allocated, but got %v", ids)
	}
	// ids: test2 = [3,5,6]
	// ids: test3 = [8,4,7]
}

func TestTunnelKeysAllocator(t *testing.T) {
	allocator := NewTunnelKeyAllocator("test")
	transitSwitchBase := 16711683
	tunnelKeyBase := 16715779
	// allocate 1 key for networkID 1 (transit switch key is preserved)
	ids, err := allocator.AllocateKeys("net1", 1, 1)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !slices.Equal(ids, []int{transitSwitchBase + 1}) {
		t.Errorf("expect ids %v allocated, but got %v", []int{transitSwitchBase + 1}, ids)
	}
	// now add one more key for networkID 1 (should return the same key)
	ids, err = allocator.AllocateKeys("net1", 1, 2)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !slices.Equal(ids, []int{transitSwitchBase + 1, tunnelKeyBase}) {
		t.Errorf("expect ids %v allocated, but got %v", []int{transitSwitchBase + 1, tunnelKeyBase}, ids)
	}
	// now ask for 1 key again for networkID 1 (reducing the number of requested keys is not expected and should return error)
	ids, err = allocator.AllocateKeys("net1", 1, 1)
	if err == nil {
		t.Errorf("expect error allocating id for net1, but got ids %v", ids)
	}
	// check the 0 also works
	ids, err = allocator.AllocateKeys("net1", 1, 0)
	if err == nil {
		t.Errorf("expect error allocating id for net1, but got ids %v", ids)
	}
	// same for reserve IDs
	err = allocator.ReserveKeys("net1", []int{transitSwitchBase + 1})
	if err == nil {
		t.Errorf("expect error reserving ids for net1")
	}
	// now reserve already allocated ids, should be ok
	err = allocator.ReserveKeys("net1", []int{transitSwitchBase + 1, tunnelKeyBase})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// allocate 3 keys for networkID 2 (transit switch key is preserved + 2 allocated keys)
	ids, err = allocator.AllocateKeys("net2", 2, 3)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !slices.Equal(ids, []int{transitSwitchBase + 2, tunnelKeyBase + 1, tunnelKeyBase + 2}) {
		t.Errorf("expect ids %v allocated, but got %v", []int{transitSwitchBase + 2, tunnelKeyBase + 1, tunnelKeyBase + 2}, ids)
	}
	// reserve next 2 keys for networkID 3
	err = allocator.ReserveKeys("net3", []int{tunnelKeyBase + 3, tunnelKeyBase + 4})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// allocate 2 keys for networkID 4
	ids, err = allocator.AllocateKeys("net4", 4, 2)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !slices.Equal(ids, []int{transitSwitchBase + 4, tunnelKeyBase + 5}) {
		t.Errorf("expect ids %v allocated, but got %v", []int{transitSwitchBase + 4, tunnelKeyBase + 5}, ids)
	}
	// check network ID out of reserved range
	ids, err = allocator.AllocateKeys("net5", 5000, 1)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !slices.Equal(ids, []int{tunnelKeyBase + 6}) {
		t.Errorf("expect ids %v allocated, but got %v", []int{tunnelKeyBase + 6}, ids)
	}

	totalKeys := 61437
	// we have already allocated 7 keys from the free range, request the rest of them + 1
	_, err = allocator.AllocateKeys("net6", 10000, totalKeys-7+1)
	if err == nil {
		t.Errorf("expect error allocating id for net5")
	}
	_, err = allocator.AllocateKeys("net6", 10000, totalKeys-7)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReserveIDOutOfRange(t *testing.T) {
	for _, invalid := range []int{math.MinInt, -2, -1, 10, math.MaxInt} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			allocator := NewIDAllocator("test", 10)
			if err := allocator.ReserveID("node", invalid); err == nil {
				t.Fatal("expected an out-of-range error")
			}
			if got := allocator.GetID("node"); got != invalidID {
				t.Fatalf("invalid reservation stored: %d", got)
			}
			for valid := 0; valid < 10; valid++ {
				if err := allocator.ReserveID(fmt.Sprint(valid), valid); err != nil {
					t.Fatalf("valid reservation failed: %v", err)
				}
			}
			if _, err := allocator.AllocateID("extra"); err == nil {
				t.Fatal("expected full allocator")
			}
		})
	}
}

func TestReserveIDsOutOfRange(t *testing.T) {
	for _, invalid := range []int{-2, 2, 9, math.MaxInt} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			allocator := newIDsAllocator("test", 6, 3)
			if err := allocator.ReserveIDs("existing", []int{3}); err != nil {
				t.Fatal(err)
			}
			err := allocator.ReserveIDs("invalid", []int{4, invalid})
			if err == nil || !strings.Contains(err.Error(), "outside allocation range") {
				t.Fatalf("expected bounds error, got %v", err)
			}
			if _, found := allocator.nameIdsMap.Load("invalid"); found {
				t.Fatal("failed reservation stored")
			}
			if err := allocator.ReserveIDs("other", []int{3}); err == nil {
				t.Fatal("existing allocation was released")
			}
			// Every remaining ID must be available, including the partial reservation.
			if err := allocator.ReserveIDs("replacement", []int{4, 5, 6, 7, 8}); err != nil {
				t.Fatal(err)
			}
			if _, err := allocator.AllocateIDs("overflow", 1); err == nil {
				t.Fatal("expected exhaustion")
			}
		})
	}
}

func TestTunnelKeysAllocatorRollbackPreservesExistingKeys(t *testing.T) {
	allocator := NewTunnelKeyAllocator("test")
	existing := []int{16715790}
	if err := allocator.ReserveKeys("network", existing); err != nil {
		t.Fatal(err)
	}
	keys, rollback, err := allocator.AllocateKeysWithRollback("network", 5000, 2)
	if err != nil {
		t.Fatal(err)
	}
	rollback()
	if err := allocator.ReserveKeys("other", existing); err == nil {
		t.Fatal("rollback released an existing key")
	}
	if err := allocator.ReserveKeys("other", keys[1:]); err != nil {
		t.Fatalf("rollback retained a newly allocated key: %v", err)
	}
	keys, err = allocator.AllocateKeys("network", 5000, 1)
	if err != nil || !slices.Equal(keys, existing) {
		t.Fatalf("existing reservation changed: keys=%v, err=%v", keys, err)
	}
}

// GetKeys returns a copy of the dynamic keys currently owned by name, or nil
// when none are reserved. Transit switch keys derived from network IDs are not
// tracked by the allocator and are excluded from the result.
func (allocator *TunnelKeysAllocator) GetKeys(name string) []int {
	allocator.idsAllocator.nameIdsMap.LockKey(name)
	defer allocator.idsAllocator.nameIdsMap.UnlockKey(name)
	keys, _ := allocator.idsAllocator.nameIdsMap.Load(name)
	return slices.Clone(keys)
}

func TestTunnelKeysAllocatorGetKeys(t *testing.T) {
	allocator := NewTunnelKeyAllocator("test")
	if keys := allocator.GetKeys("missing"); keys != nil {
		t.Fatalf("unexpected allocation: %v", keys)
	}
	expected := []int{16715790}
	if err := allocator.ReserveKeys("network", []int{16711684, expected[0]}); err != nil {
		t.Fatal(err)
	}
	keys := allocator.GetKeys("network")
	if !slices.Equal(keys, expected) {
		t.Fatalf("unexpected dynamic keys: %v", keys)
	}
	keys[0]++
	if keys := allocator.GetKeys("network"); !slices.Equal(keys, expected) {
		t.Fatalf("lookup exposed mutable allocator state: %v", keys)
	}
	if err := allocator.ReserveKeys("other", []int{16715779}); err != nil {
		t.Fatalf("lookup unexpectedly allocated a key: %v", err)
	}
}

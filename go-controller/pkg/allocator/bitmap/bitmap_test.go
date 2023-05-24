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

package bitmap

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
)

func TestAllocate(t *testing.T) {
	max := 10
	m := NewAllocationMap(max, "test")

	if _, ok, _ := m.AllocateNext(); !ok {
		t.Fatalf("unexpected error")
	}
	if m.count != 1 {
		t.Errorf("expect to get %d, but got %d", 1, m.count)
	}
	if f := m.Free(); f != max-1 {
		t.Errorf("expect to get %d, but got %d", max-1, f)
	}
}

func TestAllocateMax(t *testing.T) {
	max := 10
	m := NewAllocationMap(max, "test")
	for i := 0; i < max; i++ {
		if _, ok, _ := m.AllocateNext(); !ok {
			t.Fatalf("unexpected error")
		}
	}

	if _, ok, _ := m.AllocateNext(); ok {
		t.Errorf("unexpected success")
	}
	if f := m.Free(); f != 0 {
		t.Errorf("expect to get %d, but got %d", 0, f)
	}
}

func TestAllocateError(t *testing.T) {
	m := NewAllocationMap(10, "test")
	if ok, _ := m.Allocate(3); !ok {
		t.Errorf("error allocate offset %v", 3)
	}
	if ok, _ := m.Allocate(3); ok {
		t.Errorf("unexpected success")
	}
}

func TestRelease(t *testing.T) {
	offset := 3
	m := NewAllocationMap(10, "test")
	if ok, _ := m.Allocate(offset); !ok {
		t.Errorf("error allocate offset %v", offset)
	}

	if !m.Has(offset) {
		t.Errorf("expect offset %v allocated", offset)
	}

	m.Release(offset)

	if m.Has(offset) {
		t.Errorf("expect offset %v not allocated", offset)
	}
}

func TestForEach(t *testing.T) {
	testCases := []sets.Int{
		sets.NewInt(),
		sets.NewInt(0),
		sets.NewInt(0, 2, 5, 9),
		sets.NewInt(0, 1, 2, 3, 4, 5, 6, 7, 8, 9),
	}

	for i, tc := range testCases {
		m := NewAllocationMap(10, "test")
		for offset := range tc {
			if ok, _ := m.Allocate(offset); !ok {
				t.Errorf("[%d] error allocate offset %v", i, offset)
			}
			if !m.Has(offset) {
				t.Errorf("[%d] expect offset %v allocated", i, offset)
			}
		}
		calls := sets.NewInt()
		m.ForEach(func(i int) {
			calls.Insert(i)
		})
		if len(calls) != len(tc) {
			t.Errorf("[%d] expected %d calls, got %d", i, len(tc), len(calls))
		}
		if !calls.Equal(tc) {
			t.Errorf("[%d] expected calls to equal testcase: %v vs %v", i, calls.List(), tc.List())
		}
	}
}

func TestSnapshotAndRestore(t *testing.T) {
	offset := 3
	m := NewAllocationMap(10, "test")
	if ok, _ := m.Allocate(offset); !ok {
		t.Errorf("error allocate offset %v", offset)
	}
	spec, bytes := m.Snapshot()

	m2 := NewAllocationMap(10, "test")
	err := m2.Restore(spec, bytes)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if m2.count != 1 {
		t.Errorf("expect count to %d, but got %d", 0, m.count)
	}
	if !m2.Has(offset) {
		t.Errorf("expect offset %v allocated", offset)
	}
}

func TestContiguousAllocation(t *testing.T) {
	max := 10
	m := NewContiguousAllocationMap(max, "test")

	for i := 0; i < max; i++ {
		next, ok, _ := m.AllocateNext()
		if !ok {
			t.Fatalf("unexpected error")
		}
		if next != i {
			t.Fatalf("expect next to %d, but got %d", i, next)
		}
	}

	if _, ok, _ := m.AllocateNext(); ok {
		t.Errorf("unexpected success")
	}
}

func TestRoundRobinAllocation(t *testing.T) {
	max := 10
	m := NewRoundRobinAllocationMap(max, "test")

	for i := 0; i < max; i++ {
		next, ok, _ := m.AllocateNext()
		if !ok {
			t.Fatalf("unexpected error")
		}
		if next != i {
			t.Fatalf("expect next to be %d, but got %d", i, next)
		}
	}

	if _, ok, _ := m.AllocateNext(); ok {
		t.Fatalf("unexpected success")
	}
}

func TestRoundRobinAllocationOrdering(t *testing.T) {
	max := 10
	m := NewRoundRobinAllocationMap(max, "test")

	// Pre-allocate 3 entries at the start of the map
	for i := 0; i < 3; i++ {
		if ok, _ := m.Allocate(i); !ok {
			t.Fatalf("error allocating offset %d", i)
		}
	}

	// Next allocation should be after the preallocated entries
	next, ok, _ := m.AllocateNext()
	if !ok {
		t.Fatalf("unexpected error")
	}
	if next != 3 {
		t.Fatalf("expect next to 3, but got %d", next)
	}

	// Release one of the pre-allocated entries
	m.Release(0)

	// Next allocation should be after the most recently allocated entry,
	// not one of the just-released ones
	next, ok, _ = m.AllocateNext()
	if !ok {
		t.Fatalf("unexpected error")
	}
	if next != 4 {
		t.Fatalf("expect next to 4, but got %d", next)
	}
}

func TestRoundRobinAllocate(t *testing.T) {
	max := 10
	m := NewRoundRobinAllocationMap(max, "test")

	if _, ok, _ := m.AllocateNext(); !ok {
		t.Fatalf("unexpected error")
	}
	if m.count != 1 {
		t.Fatalf("expect to get %d, but got %d", 1, m.count)
	}
	if f := m.Free(); f != max-1 {
		t.Fatalf("expect to get %d, but got %d", max-1, f)
	}
}

func TestRoundRobinAllocateMax(t *testing.T) {
	max := 10
	m := NewRoundRobinAllocationMap(max, "test")
	for i := 0; i < max; i++ {
		if _, ok, _ := m.AllocateNext(); !ok {
			t.Fatalf("unexpected error")
		}
	}

	if _, ok, _ := m.AllocateNext(); ok {
		t.Fatalf("unexpected success")
	}
	if f := m.Free(); f != 0 {
		t.Fatalf("expect to get %d, but got %d", 0, f)
	}
}

func TestRoundRobinAllocateError(t *testing.T) {
	m := NewRoundRobinAllocationMap(10, "test")
	if ok, _ := m.Allocate(3); !ok {
		t.Fatalf("error allocate offset %d", 3)
	}
	if ok, _ := m.Allocate(3); ok {
		t.Fatalf("unexpected success")
	}
}

func TestRoundRobinRelease(t *testing.T) {
	offset := 3
	m := NewRoundRobinAllocationMap(10, "test")
	if ok, _ := m.Allocate(offset); !ok {
		t.Fatalf("error allocate offset %d", offset)
	}

	if !m.Has(offset) {
		t.Fatalf("expect offset %d allocated", offset)
	}

	m.Release(offset)

	if m.Has(offset) {
		t.Fatalf("expect offset %d not allocated", offset)
	}
}

func TestRoundRobinWrapAround(t *testing.T) {
	const maxOffsets = 10
	m := NewRoundRobinAllocationMap(maxOffsets, "test")

	// Allocate next offset and release it, expecting that the offset
	// continues to increment
	for i := 0; i < maxOffsets*2; i++ {
		offset, ok, err := m.AllocateNext()
		if !ok {
			t.Fatalf("unexpected AllocateNext error: %v", err)
		}

		if offset != (i % maxOffsets) {
			t.Fatalf("got offset %d but expected offset %d", offset, i)
		}

		m.Release(offset)
	}
}

func TestRoundRobinForEach(t *testing.T) {
	testCases := []sets.Int{
		sets.NewInt(),
		sets.NewInt(0),
		sets.NewInt(0, 2, 5, 9),
		sets.NewInt(0, 1, 2, 3, 4, 5, 6, 7, 8, 9),
	}

	for i, tc := range testCases {
		m := NewRoundRobinAllocationMap(10, "test")
		for offset := range tc {
			if ok, _ := m.Allocate(offset); !ok {
				t.Fatalf("[%d] error allocate offset %d", i, offset)
			}
			if !m.Has(offset) {
				t.Fatalf("[%d] expect offset %d allocated", i, offset)
			}
		}
		calls := sets.NewInt()
		m.ForEach(func(j int) {
			calls.Insert(j)
		})
		if len(calls) != len(tc) {
			t.Fatalf("[%d] expected %d calls, got %d", i, len(tc), len(calls))
		}
		if !calls.Equal(tc) {
			t.Fatalf("[%d] expected calls to equal testcase: %v vs %v", i, calls.List(), tc.List())
		}
	}
}

func TestRoundRobinSnapshotAndRestore(t *testing.T) {
	offset := 3
	m := NewRoundRobinAllocationMap(10, "test")
	if ok, _ := m.Allocate(offset); !ok {
		t.Fatalf("error allocate offset %d", offset)
	}
	spec, bytes := m.Snapshot()

	m2 := NewRoundRobinAllocationMap(10, "test")
	err := m2.Restore(spec, bytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if m2.count != 1 {
		t.Fatalf("expect count to %d, but got %d", 0, m.count)
	}
	if !m2.Has(offset) {
		t.Fatalf("expect offset %d allocated", offset)
	}
}

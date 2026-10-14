// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package id

import (
	"fmt"
	"slices"

	bitmapallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/bitmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
)

const (
	invalidID = -1
)

// Allocator of IDs for a set of resources identified by name
type Allocator interface {
	AllocateID(name string) (int, error)
	ReserveID(name string, id int) error
	ReleaseID(name string) int
	ForName(name string) NamedAllocator
	GetID(name string) int
}

// NamedAllocator of IDs for a specific resource
type NamedAllocator interface {
	AllocateID() (int, error)
	ReserveID(int) error
	ReleaseID() int
}

// idAllocator is used to allocate id for a resource and store the resource - id in a map
type idAllocator struct {
	nameIdMap *syncmap.SyncMap[int]
	idBitmap  *bitmapallocator.AllocationBitmap
}

// NewIDAllocator returns an IDAllocator
func NewIDAllocator(name string, maxIds int) Allocator {
	idBitmap := bitmapallocator.NewRoundRobinAllocationMap(maxIds, name)

	return &idAllocator{
		nameIdMap: syncmap.NewSyncMap[int](),
		idBitmap:  idBitmap,
	}
}

// AllocateID allocates an id for the resource 'name' and returns the id.
// If the id for the resource is already allocated, it returns the cached id.
func (idAllocator *idAllocator) AllocateID(name string) (int, error) {
	idAllocator.nameIdMap.LockKey(name)
	defer idAllocator.nameIdMap.UnlockKey(name)
	// Check the idMap and return the id if its already allocated
	v, ok := idAllocator.nameIdMap.Load(name)
	if ok {
		return v, nil
	}

	id, allocated, _ := idAllocator.idBitmap.AllocateNext()

	if !allocated {
		return invalidID, fmt.Errorf("failed to allocate the id for the resource %s", name)
	}

	idAllocator.nameIdMap.Store(name, id)
	return id, nil
}

// ReserveID reserves the id 'id' for the resource 'name'. It returns an
// error if the 'id' is already reserved by a resource other than 'name'.
// It also returns an error if the resource 'name' has a different 'id'
// already reserved.
func (idAllocator *idAllocator) ReserveID(name string, id int) error {
	idAllocator.nameIdMap.LockKey(name)
	defer idAllocator.nameIdMap.UnlockKey(name)
	v, ok := idAllocator.nameIdMap.Load(name)
	if ok {
		if v == id {
			// All good. The id is already reserved by the same resource name.
			return nil
		}
		return fmt.Errorf("can't reserve id %d for the resource %s. It is already allocated with a different id %d", id, name, v)
	}

	reserved, err := idAllocator.idBitmap.Allocate(id)
	if err != nil {
		return fmt.Errorf("failed to reserve id %d for the resource %s: %w", id, name, err)
	}
	if !reserved {
		return fmt.Errorf("id %d is already reserved by another resource", id)
	}

	idAllocator.nameIdMap.Store(name, id)
	return nil
}

// ReleaseID releases the id allocated for the resource 'name'.
// Returns the released id, or -1 if no id was allocated for that name.
func (idAllocator *idAllocator) ReleaseID(name string) int {
	idAllocator.nameIdMap.LockKey(name)
	defer idAllocator.nameIdMap.UnlockKey(name)
	v, ok := idAllocator.nameIdMap.Load(name)
	if ok {
		idAllocator.idBitmap.Release(v)
		idAllocator.nameIdMap.Delete(name)
		return v
	}
	return invalidID
}

func (idAllocator *idAllocator) ForName(name string) NamedAllocator {
	return &namedAllocator{
		name:      name,
		allocator: idAllocator,
	}
}

func (idAllocator *idAllocator) GetID(name string) int {
	v, ok := idAllocator.nameIdMap.Load(name)
	if !ok {
		return invalidID
	}
	return v
}

type namedAllocator struct {
	name      string
	allocator *idAllocator
}

func (allocator *namedAllocator) AllocateID() (int, error) {
	return allocator.allocator.AllocateID(allocator.name)
}

func (allocator *namedAllocator) ReserveID(id int) error {
	return allocator.allocator.ReserveID(allocator.name, id)
}

func (allocator *namedAllocator) ReleaseID() int {
	return allocator.allocator.ReleaseID(allocator.name)
}

// idsAllocator is used to allocate multiple ids for a resource and store the resource - ids in a map
type idsAllocator struct {
	// idBitmap allocated ids in range [0, maxIds-1]
	idBitmap *bitmapallocator.AllocationBitmap
	// offset can be used to shift the range to [offset, offset+maxIds-1]
	offset int
	// nameIdsMap stores the final allocated ids in range [offset, offset+maxIds-1] for a resource name
	nameIdsMap *syncmap.SyncMap[[]int]
}

// newIDsAllocator returns an idsAllocator.
// If offset is non-zero, the allocated ids will be in the range [offset, offset+maxIds-1)
func newIDsAllocator(name string, maxIds int, offset int) *idsAllocator {
	idBitmap := bitmapallocator.NewRoundRobinAllocationMap(maxIds, name)
	return &idsAllocator{
		nameIdsMap: syncmap.NewSyncMap[[]int](),
		idBitmap:   idBitmap,
		offset:     offset,
	}
}

// AllocateIDs allocates numOfIDs for the resource 'name' and returns the ids.
// If less ids than numOfIDs are already allocated for the resource name, it will allocate the missing amount.
// If more ids than numOfIDs are already allocated for the resource name, it returns an error.
func (idsAllocator *idsAllocator) AllocateIDs(name string, numOfIDs int) ([]int, error) {
	idsAllocator.nameIdsMap.LockKey(name)
	defer idsAllocator.nameIdsMap.UnlockKey(name)
	return idsAllocator.allocateIDs(name, numOfIDs)
}

// allocateIDs requires the caller to hold the resource's key lock.
func (idsAllocator *idsAllocator) allocateIDs(name string, numOfIDs int) ([]int, error) {
	// Check the idMap and return the id if its already allocated
	ids, ok := idsAllocator.nameIdsMap.Load(name)
	if ok {
		if len(ids) == numOfIDs {
			return ids, nil
		}
		if len(ids) > numOfIDs {
			return ids, fmt.Errorf("the resource %s already has more ids allocated %v than requested %v", name, ids, numOfIDs)
		}
	} else {
		ids = make([]int, 0, numOfIDs)
	}
	previouslyAllocated := len(ids)
	for len(ids) < numOfIDs {
		id, allocated, _ := idsAllocator.idBitmap.AllocateNext()
		if !allocated {
			// release newly allocated ids
			for _, id := range ids[previouslyAllocated:] {
				idsAllocator.idBitmap.Release(id - idsAllocator.offset)
			}
			return ids, fmt.Errorf("failed to allocate the id for the resource %s", name)
		}
		ids = append(ids, id+idsAllocator.offset)
	}
	if len(ids) == 0 {
		// don't store empty slice in the map
		return ids, nil
	}
	idsAllocator.nameIdsMap.Store(name, ids)
	return ids, nil
}

// ReserveIDs reserves 'ids' for the resource 'name'. It returns an
// error if one of the 'ids' is already reserved by a resource other than 'name'.
// It also returns an error if the resource 'name' has a different 'ids' slice
// already reserved. Slice elements order is important for comparison.
func (idsAllocator *idsAllocator) ReserveIDs(name string, ids []int) error {
	idsAllocator.nameIdsMap.LockKey(name)
	defer idsAllocator.nameIdsMap.UnlockKey(name)
	return idsAllocator.reserveIDs(name, ids)
}

// reserveIDs requires the caller to hold the resource's key lock.
func (idsAllocator *idsAllocator) reserveIDs(name string, ids []int) error {
	existingIDs, ok := idsAllocator.nameIdsMap.Load(name)
	if ok {
		if slices.Equal(existingIDs, ids) {
			// All good. The ids are already reserved by the same resource name.
			return nil
		}
		return fmt.Errorf("can't reserve ids %v for the resource %s. It is already allocated with different ids %v",
			ids, name, existingIDs)
	}
	allocatedIDs := make([]int, 0, len(ids))
	for _, id := range ids {
		// don't forget to adjust the id with the offset
		reserved, err := idsAllocator.idBitmap.Allocate(id - idsAllocator.offset)
		if err != nil || !reserved {
			// cleanup previously allocated ids
			for _, allocatedID := range allocatedIDs {
				idsAllocator.idBitmap.Release(allocatedID - idsAllocator.offset)
			}
			if err != nil {
				return fmt.Errorf("failed to reserve id %d for resource %s: %w", id, name, err)
			}
			return fmt.Errorf("id %d is already reserved by another resource", id)
		}
		allocatedIDs = append(allocatedIDs, id)
	}
	idsAllocator.nameIdsMap.Store(name, allocatedIDs)
	return nil
}

// ReleaseIDs releases all ids allocated for the resource 'name'
func (idsAllocator *idsAllocator) ReleaseIDs(name string) {
	idsAllocator.nameIdsMap.LockKey(name)
	defer idsAllocator.nameIdsMap.UnlockKey(name)
	existingIDs, ok := idsAllocator.nameIdsMap.Load(name)
	if !ok {
		return
	}
	for _, id := range existingIDs {
		idsAllocator.idBitmap.Release(id - idsAllocator.offset)
	}
	idsAllocator.nameIdsMap.Delete(name)
}

// TrimIDs releases the ids 'name' holds beyond the first numOfIDs, keeping the
// rest allocated to 'name'. It is a no-op if 'name' holds numOfIDs or fewer. Only
// this resource's own ids are touched, under its key lock, so no other resource
// is affected.
func (idsAllocator *idsAllocator) TrimIDs(name string, numOfIDs int) {
	idsAllocator.nameIdsMap.LockKey(name)
	defer idsAllocator.nameIdsMap.UnlockKey(name)
	existingIDs, ok := idsAllocator.nameIdsMap.Load(name)
	if !ok || len(existingIDs) <= numOfIDs {
		return
	}
	for _, id := range existingIDs[numOfIDs:] {
		idsAllocator.idBitmap.Release(id - idsAllocator.offset)
	}
	if numOfIDs == 0 {
		idsAllocator.nameIdsMap.Delete(name)
		return
	}
	idsAllocator.nameIdsMap.Store(name, existingIDs[:numOfIDs])
}

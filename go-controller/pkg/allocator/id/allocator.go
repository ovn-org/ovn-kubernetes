package id

import (
	"fmt"
	"sync"

	bitmapallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/bitmap"
)

const (
	invalidID = -1
)

// Allocator of IDs for a set of resources identified by name
type Allocator interface {
	AllocateID(name string) (int, error)
	ReserveID(name string, id int) error
	ReleaseID(name string)
	ForName(name string) NamedAllocator
}

// NamedAllocator of IDs for a specific resource
type NamedAllocator interface {
	AllocateID() (int, error)
	ReserveID(int) error
	ReleaseID()
}

// idAllocator is used to allocate id for a resource and store the resource - id in a map
type idAllocator struct {
	nameIdMap sync.Map
	idBitmap  *bitmapallocator.AllocationBitmap
}

// NewIDAllocator returns an IDAllocator
func NewIDAllocator(name string, maxIds int) Allocator {
	idBitmap := bitmapallocator.NewRoundRobinAllocationMap(maxIds, name)

	return &idAllocator{
		nameIdMap: sync.Map{},
		idBitmap:  idBitmap,
	}
}

// AllocateID allocates an id for the resource 'name' and returns the id.
// If the id for the resource is already allocated, it returns the cached id.
func (idAllocator *idAllocator) AllocateID(name string) (int, error) {
	// Check the idMap and return the id if its already allocated
	v, ok := idAllocator.nameIdMap.Load(name)
	if ok {
		return v.(int), nil
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
	v, ok := idAllocator.nameIdMap.Load(name)
	if ok {
		if v.(int) == id {
			// All good. The id is already reserved by the same resource name.
			return nil
		}
		return fmt.Errorf("can't reserve id %d for the resource %s. It is already allocated with a different id %d", id, name, v.(int))
	}

	reserved, _ := idAllocator.idBitmap.Allocate(id)
	if !reserved {
		return fmt.Errorf("id %d is already reserved by another resource", id)
	}

	idAllocator.nameIdMap.Store(name, id)
	return nil
}

// ReleaseID releases the id allocated for the resource 'name'
func (idAllocator *idAllocator) ReleaseID(name string) {
	v, ok := idAllocator.nameIdMap.Load(name)
	if ok {
		idAllocator.idBitmap.Release(v.(int))
		idAllocator.nameIdMap.Delete(name)
	}
}

func (idAllocator *idAllocator) ForName(name string) NamedAllocator {
	return &namedAllocator{
		name:      name,
		allocator: idAllocator,
	}
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

func (allocator *namedAllocator) ReleaseID() {
	allocator.allocator.ReleaseID(allocator.name)
}

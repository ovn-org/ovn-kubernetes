package clustermanager

import (
	"fmt"
	"sync"

	bitmapallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ipallocator/allocator"
)

const (
	invalidID = -1
)

// idAllocator is used to allocate id for a resource and store the resource - id in a map
type idAllocator struct {
	nameIdMap sync.Map
	idBitmap  *bitmapallocator.AllocationBitmap
}

// NewIDAllocator returns an IDAllocator
func NewIDAllocator(name string, maxIds int) (*idAllocator, error) {
	idBitmap := bitmapallocator.NewContiguousAllocationMap(maxIds, name)

	return &idAllocator{
		nameIdMap: sync.Map{},
		idBitmap:  idBitmap,
	}, nil
}

// allocateID allocates an id for the resource 'name' and returns the id.
// If the id for the resource is already allocated, it returns the cached id.
func (idAllocator *idAllocator) allocateID(name string) (int, error) {
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

// reserveID reserves the id 'id' for the resource 'name'. It returns an
// error if the 'id' is already reserved by a resource other than 'name'.
// It also returns an error if the resource 'name' has a different 'id'
// already reserved.
func (idAllocator *idAllocator) reserveID(name string, id int) error {
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

// releaseID releases the id allocated for the resource 'name'
func (idAllocator *idAllocator) releaseID(name string) {
	v, ok := idAllocator.nameIdMap.Load(name)
	if ok {
		idAllocator.idBitmap.Release(v.(int))
		idAllocator.nameIdMap.Delete(name)
	}
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package id

import "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"

// TunnelKeysAllocator is used to allocate tunnel Keys for distributed OVN datapaths.
// It preserves the first types.MaxNetworks keys for the already-used transit switch IDs based on the networkID.
type TunnelKeysAllocator struct {
	idsAllocator   *idsAllocator
	preservedRange int
	idsOffset      int
}

// NewTunnelKeyAllocator returns an TunnelKeysAllocator
func NewTunnelKeyAllocator(name string) *TunnelKeysAllocator {
	// OVN-defined constants from
	// https://github.com/ovn-org/ovn/blob/cfaf849c034469502fc97149f20676dec4d76595/lib/ovn-util.h#L159-L164
	// total number of datapath(switches and routers) keys
	maxDPKey := (1 << 24) - 1
	// We have already used some keys for transit switch tunnels, the maximum tunnel key that is already allocated
	// is BaseTransitSwitchTunnelKey + types.MaxNetworks - 1.
	// BaseTransitSwitchTunnelKey = 16711683
	rangeStart := 16711683 + types.MaxNetworks
	// this is how many keys are left for allocation
	freeIDs := maxDPKey - rangeStart + 1

	return &TunnelKeysAllocator{
		idsAllocator:   newIDsAllocator(name, freeIDs, rangeStart),
		preservedRange: types.MaxNetworks,
		idsOffset:      16711683,
	}
}

// AllocateKeys allocates 'numOfKeys' for the resource 'name'.
// Previously allocated keys for 'name' are preserved in case of error.
// If networkID is less than types.MaxNetworks, the first key will come from the preserved range
// based on the networkID.
// If less keys than numOfKeys are already allocated for the resource name, it will allocate the missing amount.
// If more keys than numOfKeys are already allocated for the resource name, it returns an error.
func (allocator *TunnelKeysAllocator) AllocateKeys(name string, networkID, numOfKeys int) ([]int, error) {
	keys, _, err := allocator.AllocateKeysWithRollback(name, networkID, numOfKeys)
	return keys, err
}

// AllocateKeysWithRollback allocates keys and returns a function that releases
// only keys added by this call. Existing keys remain reserved on rollback.
// The caller must serialize operations for this resource until it commits or
// rolls back the allocation.
func (allocator *TunnelKeysAllocator) AllocateKeysWithRollback(name string, networkID, numOfKeys int) ([]int, func(), error) {
	allocator.idsAllocator.nameIdsMap.LockKey(name)
	defer allocator.idsAllocator.nameIdsMap.UnlockKey(name)
	existingIDs, _ := allocator.idsAllocator.nameIdsMap.Load(name)
	previousCount := len(existingIDs)
	allocatedIDs := make([]int, 0, numOfKeys)
	if networkID < allocator.preservedRange && numOfKeys > 0 {
		allocatedIDs = append(allocatedIDs, allocator.idsOffset+networkID)
		numOfKeys--
	}
	newIDs, err := allocator.idsAllocator.allocateIDs(name, numOfKeys)
	if err != nil {
		return nil, nil, err
	}
	rollback := func() {
		allocator.idsAllocator.TrimIDs(name, previousCount)
	}
	return append(allocatedIDs, newIDs...), rollback, nil
}

// ReserveKeys reserves 'tunnelKeys' for the resource 'name'. It returns an
// error if one of the 'tunnelKeys' is already reserved by a resource other than 'name'.
// It also returns an error if the resource 'name' has a different 'tunnelKeys' slice
// already reserved. Slice elements order is important for comparison.
func (allocator *TunnelKeysAllocator) ReserveKeys(name string, tunnelKeys []int) error {
	_, err := allocator.ReserveKeysWithRollback(name, tunnelKeys)
	return err
}

// ReserveKeysWithRollback reserves keys and returns a function that releases only
// reservations added by this call. Existing reservations survive rollback.
// The caller must serialize operations for this resource until it commits or
// rolls back the reservation.
func (allocator *TunnelKeysAllocator) ReserveKeysWithRollback(name string, tunnelKeys []int) (func(), error) {
	allocator.idsAllocator.nameIdsMap.LockKey(name)
	defer allocator.idsAllocator.nameIdsMap.UnlockKey(name)
	existingIDs, _ := allocator.idsAllocator.nameIdsMap.Load(name)
	previousCount := len(existingIDs)
	if len(tunnelKeys) > 0 && tunnelKeys[0] >= allocator.idsOffset && tunnelKeys[0]-allocator.idsOffset < allocator.preservedRange {
		// transit switch tunnel key is not allocated by the allocator
		tunnelKeys = tunnelKeys[1:]
	}
	if err := allocator.idsAllocator.reserveIDs(name, tunnelKeys); err != nil {
		return nil, err
	}
	return func() {
		allocator.idsAllocator.TrimIDs(name, previousCount)
	}, nil
}

// ReleaseKeys releases the tunnelKeys allocated for the resource 'name'
func (allocator *TunnelKeysAllocator) ReleaseKeys(name string) {
	allocator.idsAllocator.ReleaseIDs(name)
}

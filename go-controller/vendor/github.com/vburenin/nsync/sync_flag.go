package nsync

import (
	"sync"
	"sync/atomic"
)

// SyncFlag implements a boolean flag that can be set or unset atomically.
// During set/unset SyncFlag locks the mutex, so if anything needs to prevent
// a flag from being set/unset should acquire a lock.

type SyncFlag struct {
	sync.Mutex
	flag int32
}

// Set locks the mutex, sets the flag and unlocks the mutex.
func (bf *SyncFlag) Set() {
	bf.Lock()
	atomic.StoreInt32(&bf.flag, 1)
	bf.Unlock()
}

// Set locks the mutex, reset the flag and unlocks the mutex.
func (bf *SyncFlag) Unset() {
	bf.Lock()
	atomic.StoreInt32(&bf.flag, 0)
	bf.Unlock()
}

// IsSet atomically checks if flag is set.
func (bf *SyncFlag) IsSet() bool {
	return atomic.LoadInt32(&bf.flag) > 0
}

// IsUnset atomically checks if flag is unset.
func (bf *SyncFlag) IsUnset() bool {
	return atomic.LoadInt32(&bf.flag) == 0
}

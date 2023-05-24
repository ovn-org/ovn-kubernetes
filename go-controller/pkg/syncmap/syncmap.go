package syncmap

import (
	"sync"

	"k8s.io/klog/v2"
)

type keyLock struct {
	mutex      sync.Mutex
	refCounter int
}

func (m *keyLock) addRef() {
	m.refCounter++
}

func (m *keyLock) delRef() {
	m.refCounter--
}

func newKeyLock() *keyLock {
	c := keyLock{
		sync.Mutex{},
		0,
	}
	return &c
}

// SyncMap is a map with lockable keys. It allows to lock the key regardless of whether the entry for
// given key exists. When key is locked other threads can't read/write the key.
type SyncMap[T any] struct {
	// keyLocksMutex needs to be locked for every read/write operation with keyLocks.
	// refCounter should be updated for keyLock before keyLocksMutex lock is released.
	// to avoid deadlocks make sure no other locks are acquired when keyLocksMutex is locked.
	keyLocksMutex sync.Mutex
	// map of key mutexes, should only be accessed with keyLocksMutex lock
	// keyLock exists for a key that was locked with LockKey and until all threads that called LockKey
	// execute UnlockKey
	keyLocks map[string]*keyLock
	// entriesMutex needs to be locked for every read/write operation with entries
	// to avoid deadlocks make sure no other locks are acquired when entriesMutex is locked
	entriesMutex sync.Mutex
	// cache entries
	// should only be accessed with entriesMutex, also
	// read/write for a given key is only allowed with keyLock
	entries map[string]T
}

func NewSyncMap[T any]() *SyncMap[T] {
	c := SyncMap[T]{
		sync.Mutex{},
		make(map[string]*keyLock),
		sync.Mutex{},
		make(map[string]T),
	}
	return &c
}

// UnlockKey unlocks previously locked key. Call it when all the operations with the given key are done.
func (c *SyncMap[T]) UnlockKey(lockedKey string) {
	c.keyLocksMutex.Lock()
	defer c.keyLocksMutex.Unlock()
	kLock, ok := c.keyLocks[lockedKey]
	if !ok {
		// this should never happen, since UnlockKey should only be called when the key is Locked
		// and when the key is Locked, c.keyLocks[key] will always have its keyLock.
		// similar to calling Unlock() on unlocked mutex
		klog.Errorf("Unlocking non-existing key %s", lockedKey)
		return
	}
	kLock.delRef()
	// keyLock can be deleted when the last request is being shut down (that is refCount=0)
	// next load request for this key will create a new keyLock
	if kLock.refCounter == 0 {
		delete(c.keyLocks, lockedKey)
	}
	kLock.mutex.Unlock()
}

// loadOrStoreKeyLock returns the existing value for keyLock if present.
// Otherwise, it stores and returns the given value.
// The loaded result is true if the value was loaded, false if stored.
// loadOrStoreKeyLock will increase refCounter for returned value
func (c *SyncMap[T]) loadOrStoreKeyLock(lockedKey string, value *keyLock) (*keyLock, bool) {
	c.keyLocksMutex.Lock()
	defer c.keyLocksMutex.Unlock()
	if kLock, ok := c.keyLocks[lockedKey]; ok {
		kLock.addRef()
		return kLock, true
	} else {
		c.keyLocks[lockedKey] = value
		value.addRef()
		return value, false
	}
}

// LockKey should be called before reading/writing entry value,
// it guarantees exclusive access to the key.
// Unlock(key) should be called once the work for this key is done to unlock other threads
// After the key is unlocked there are no guarantees for the entry for given key
func (c *SyncMap[T]) LockKey(key string) {
	// if the kLock is not present, we create a new one
	// lock it before adding, to prevent other threads from getting the key lock after we add it
	newKLock := newKeyLock()
	newKLock.mutex.Lock()
	kLock, loaded := c.loadOrStoreKeyLock(key, newKLock)
	// loadOrStoreKeyLock will increase refCounter for the returned kLock,
	// meaning that other threads won't be able to delete this kLock until we decrease refCounter
	// with UnlockKey().
	// if newKLock was stored (!loaded), we already have it locked
	if !loaded {
		return
	}
	// existing kLock was loaded, unlock newKLock since we didn't use it
	newKLock.mutex.Unlock()
	// lock the key
	kLock.mutex.Lock()
}

// Load returns the value stored in the map for a key, or nil if no value is present.
// The loaded result indicates whether value was found in the map.
func (c *SyncMap[T]) Load(lockedKey string) (value T, loaded bool) {
	c.entriesMutex.Lock()
	defer c.entriesMutex.Unlock()
	entry, ok := c.entries[lockedKey]
	return entry, ok
}

// LoadOrStore gets the key value if it's present or creates a new one if it isn't,
// loaded return value signals if the object was present.
func (c *SyncMap[T]) LoadOrStore(lockedKey string, newEntry T) (value T, loaded bool) {
	c.entriesMutex.Lock()
	defer c.entriesMutex.Unlock()
	if entry, ok := c.entries[lockedKey]; ok {
		return entry, true
	} else {
		c.entries[lockedKey] = newEntry
		return newEntry, false
	}
}

// Store sets the value for a key.
// If key-value was already present, it will be over-written
func (c *SyncMap[T]) Store(lockedKey string, newEntry T) {
	c.entriesMutex.Lock()
	defer c.entriesMutex.Unlock()
	c.entries[lockedKey] = newEntry
}

// Delete deletes object from the entries map
func (c *SyncMap[T]) Delete(lockedKey string) {
	c.entriesMutex.Lock()
	defer c.entriesMutex.Unlock()
	delete(c.entries, lockedKey)
}

// GetKeys returns a snapshot of all keys from entries map.
// After this function returns there are no guarantees that the keys in the real entries map are still the same
func (c *SyncMap[T]) GetKeys() []string {
	c.entriesMutex.Lock()
	defer c.entriesMutex.Unlock()
	keys := make([]string, len(c.entries))
	i := 0
	for k := range c.entries {
		keys[i] = k
		i++
	}
	return keys
}

// DoWithLock takes care of locking and unlocking key.
func (c *SyncMap[T]) DoWithLock(key string, f func(key string) error) error {
	c.LockKey(key)
	defer c.UnlockKey(key)
	return f(key)
}

package retry

import (
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"

	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

const RetryObjInterval = 30 * time.Second
const MaxFailedAttempts = 15 // same value used for the services level-driven controller
const initialBackoff = 1
const noBackoff = 0

// retryObjEntry is a generic object caching with retry mechanism
// that resources can use to eventually complete their intended operations.
type retryObjEntry struct {
	// newObj holds k8s resource failed during add operation
	newObj interface{}
	// oldObj holds k8s resource failed during delete operation
	oldObj interface{}
	// config holds feature specific configuration,
	// currently used by network policies and pods.
	config     interface{}
	timeStamp  time.Time
	backoffSec time.Duration
	// number of times this object has been unsuccessfully added/updated/deleted
	failedAttempts uint8
}

type EventHandler interface {
	AddResource(obj interface{}, fromRetryLoop bool) error
	UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error
	DeleteResource(obj, cachedObj interface{}) error
	SyncFunc([]interface{}) error

	// auxiliary functions needed in the retry logic
	GetResourceFromInformerCache(key string) (interface{}, error)
	AreResourcesEqual(obj1, obj2 interface{}) (bool, error)
	GetInternalCacheEntry(obj interface{}) interface{}
	IsResourceScheduled(obj interface{}) bool
	IsObjectInTerminalState(obj interface{}) bool

	// functions related to metrics and events
	RecordAddEvent(obj interface{})
	RecordUpdateEvent(obj interface{})
	RecordDeleteEvent(obj interface{})
	RecordSuccessEvent(obj interface{})
	RecordErrorEvent(obj interface{}, reason string, err error)
	FilterResource(obj interface{}) bool
}

// DefaultEventHandler has the default implementations for some EventHandler
// methods, that are not required for every handler
type DefaultEventHandler struct{}

func (h *DefaultEventHandler) SyncFunc([]interface{}) error { return nil }

func (h *DefaultEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	return false, nil
}

func (h *DefaultEventHandler) GetInternalCacheEntry(obj interface{}) interface{} { return nil }

func (h *DefaultEventHandler) IsResourceScheduled(obj interface{}) bool { return true }

func (h *DefaultEventHandler) IsObjectInTerminalState(obj interface{}) bool { return false }

func (h *DefaultEventHandler) RecordAddEvent(obj interface{}) {}

func (h *DefaultEventHandler) RecordUpdateEvent(obj interface{}) {}

func (h *DefaultEventHandler) RecordDeleteEvent(obj interface{}) {}

func (h *DefaultEventHandler) RecordSuccessEvent(obj interface{}) {}

func (h *DefaultEventHandler) RecordErrorEvent(obj interface{}, reason string, err error) {}

type ResourceHandler struct {
	// HasUpdateFunc is true if an update event for this resource type is implemented as an
	// update action; it is false, if instead it is implemented as a delete on the old obj and
	// an add on the new one.
	HasUpdateFunc          bool
	NeedsUpdateDuringRetry bool
	ObjType                reflect.Type
	EventHandler
}

type RetryFramework struct {
	// cache to hold object needs retry to successfully complete processing
	retryEntries *syncmap.SyncMap[*retryObjEntry]
	// channel to indicate we need to retry objs immediately
	retryChan chan struct{}

	stopChan <-chan struct{}
	doneWg   *sync.WaitGroup

	watchFactory      *factory.WatchFactory
	ResourceHandler   *ResourceHandler
	terminatedObjects sync.Map
}

// NewRetryFramework returns a new RetryFramework instance, essential for the whole retry logic.
// It returns a new struct packed with the desired input parameters and
// with its function attributes pre-filled with default code. Clients of this pkg (ovnk master,
// ovnk node) will have to override the functions in the returned struct with the desired
// per-resource logic.
func NewRetryFramework(
	stopChan <-chan struct{}, doneWg *sync.WaitGroup,
	watchFactory *factory.WatchFactory,
	resourceHandler *ResourceHandler) *RetryFramework {
	return &RetryFramework{
		retryEntries:      syncmap.NewSyncMap[*retryObjEntry](),
		retryChan:         make(chan struct{}, 1),
		watchFactory:      watchFactory,
		stopChan:          stopChan,
		doneWg:            doneWg,
		ResourceHandler:   resourceHandler,
		terminatedObjects: sync.Map{},
	}
}

func (r *RetryFramework) DoWithLock(key string, f func(key string)) {
	r.retryEntries.LockKey(key)
	defer r.retryEntries.UnlockKey(key)
	f(key)
}

func (r *RetryFramework) initRetryObjWithAddBackoff(obj interface{}, lockedKey string, backoff time.Duration) *retryObjEntry {
	// even if the object was loaded and changed before with the same lock, LoadOrStore will return reference to the same object
	entry, _ := r.retryEntries.LoadOrStore(lockedKey, &retryObjEntry{backoffSec: backoff})
	entry.timeStamp = time.Now()
	entry.newObj = obj
	entry.failedAttempts = 0
	entry.backoffSec = backoff
	return entry
}

// initRetryObjWithAdd creates a retry entry for an object that is being added,
// so that, if it fails, the add can be potentially retried later.
func (r *RetryFramework) initRetryObjWithAdd(obj interface{}, lockedKey string) *retryObjEntry {
	return r.initRetryObjWithAddBackoff(obj, lockedKey, initialBackoff)
}

// initRetryObjWithUpdate tracks objects that failed to be updated to potentially retry later
func (r *RetryFramework) initRetryObjWithUpdate(oldObj, newObj interface{}, lockedKey string) *retryObjEntry {
	entry, _ := r.retryEntries.LoadOrStore(lockedKey, &retryObjEntry{config: oldObj, backoffSec: initialBackoff})
	// even if the object was loaded and changed before with the same lock, LoadOrStore will return reference to the same object
	entry.timeStamp = time.Now()
	entry.newObj = newObj
	entry.config = oldObj
	entry.failedAttempts = 0
	return entry
}

// InitRetryObjWithDelete creates a retry entry for an object that is being deleted,
// so that, if it fails, the delete can be potentially retried later.
// When applied to pods, we include the config object as well in case the namespace is removed
// and the object is orphaned from the namespace.
// The noRetryAdd boolean argument is to indicate whether to retry for addition
func (r *RetryFramework) InitRetryObjWithDelete(obj interface{}, lockedKey string, config interface{}, noRetryAdd bool) *retryObjEntry {
	// even if the object was loaded and changed before with the same lock, LoadOrStore will return reference to the same object
	entry, _ := r.retryEntries.LoadOrStore(lockedKey, &retryObjEntry{config: config, backoffSec: initialBackoff})
	entry.timeStamp = time.Now()
	entry.oldObj = obj
	if entry.config == nil {
		entry.config = config
	}
	entry.failedAttempts = 0
	if noRetryAdd {
		// will not be retried for addition
		entry.newObj = nil
	}
	return entry
}

// AddRetryObjWithAddNoBackoff adds an object to be retried immediately for add.
// It will lock the key, create or update retryObject, and unlock the key
func (r *RetryFramework) AddRetryObjWithAddNoBackoff(obj interface{}) error {
	key, err := GetResourceKey(obj)
	if err != nil {
		return fmt.Errorf("could not get the key of %s %v: %v", r.ResourceHandler.ObjType, obj, err)
	}
	r.DoWithLock(key, func(key string) {
		r.initRetryObjWithAddBackoff(obj, key, noBackoff)
	})
	return nil
}

func (r *RetryFramework) getRetryObj(lockedKey string) (value *retryObjEntry, found bool) {
	return r.retryEntries.Load(lockedKey)
}

func (r *RetryFramework) DeleteRetryObj(lockedKey string) {
	r.retryEntries.Delete(lockedKey)
}

// setRetryObjWithNoBackoff sets an object's backoff to be retried
// immediately during the next retry iteration
// Used only for testing right now
func (r *RetryFramework) setRetryObjWithNoBackoff(entry *retryObjEntry) {
	entry.backoffSec = noBackoff
}

// removeDeleteFromRetryObj removes any old object from a retry entry
func (r *RetryFramework) removeDeleteFromRetryObj(entry *retryObjEntry) {
	entry.oldObj = nil
	entry.config = nil
}

// increaseFailedAttemptsCounter increases by one the counter of failed add/update/delete attempts
// for the given key
func (r *RetryFramework) increaseFailedAttemptsCounter(entry *retryObjEntry) {
	entry.failedAttempts++
}

// RequestRetryFramework allows a caller to immediately request to iterate through all objects that
// are in the retry cache. This will ignore any outstanding time wait/backoff state
func (r *RetryFramework) RequestRetryObjs() {
	select {
	case r.retryChan <- struct{}{}:
		klog.V(5).Infof("Iterate retry objects requested (resource %s)", r.ResourceHandler.ObjType)
	default:
		klog.V(5).Infof("Iterate retry objects already requested (resource %s)", r.ResourceHandler.ObjType)
	}
}

// Given an object and its type, it returns the key for this object and an error if the key retrieval failed.
// For all namespaced resources, the key will be namespace/name. For resource types without a namespace,
// the key will be the object name itself. obj must be a pointer to an API type.
func GetResourceKey(obj interface{}) (string, error) {
	res, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		klog.Errorf("GetResourceKey returned error: %v", err)
	}
	return res, err
}

func (r *RetryFramework) resourceRetry(objKey string, now time.Time) {
	r.DoWithLock(objKey, func(key string) {
		entry, loaded := r.getRetryObj(key)
		if !loaded {
			klog.V(5).Infof("%v resource %s was not found in the iterateRetryResources map while retrying resource setup", r.ResourceHandler.ObjType, objKey)
			return
		}

		if entry.failedAttempts >= MaxFailedAttempts {
			klog.Warningf("Dropping retry entry for %s %s: exceeded number of failed attempts",
				r.ResourceHandler.ObjType, objKey)
			r.DeleteRetryObj(key)
			metrics.MetricResourceRetryFailuresCount.Inc()
			if entry.newObj != nil {
				r.ResourceHandler.RecordErrorEvent(entry.newObj, "RetryFailed",
					fmt.Errorf("failed to reconcile and retried %d times for object: %v", MaxFailedAttempts, entry.newObj))
			}
			return
		}
		forceRetry := false
		// check if immediate retry is requested
		if entry.backoffSec == noBackoff {
			entry.backoffSec = initialBackoff
			forceRetry = true
		}
		backoff := (entry.backoffSec * time.Second) + (time.Duration(rand.Intn(500)) * time.Millisecond)
		objTimer := entry.timeStamp.Add(backoff)
		if !forceRetry && now.Before(objTimer) {
			klog.V(5).Infof("Attempting retry of %s %s before timer (time: %s): skip", r.ResourceHandler.ObjType, objKey, objTimer)
			return
		}

		// update backoff for future attempts in case of failure
		entry.backoffSec = entry.backoffSec * 2
		if entry.backoffSec > 60 {
			entry.backoffSec = 60
		}

		// storing original obj for metrics
		var initObj interface{}
		if entry.newObj != nil {
			initObj = entry.newObj
		} else if entry.oldObj != nil {
			initObj = entry.oldObj
		}

		klog.Infof("Retry object setup: %s %s", r.ResourceHandler.ObjType, objKey)

		if entry.newObj != nil {
			// get the latest version of the object from the informer;
			// if it doesn't exist we are not going to create the new object.
			kObj, err := r.ResourceHandler.GetResourceFromInformerCache(objKey)
			if err != nil {
				if kerrors.IsNotFound(err) {
					klog.Infof("%s %s not found in the informers cache,"+
						" not going to retry object create", r.ResourceHandler.ObjType, objKey)
					kObj = nil
				} else {
					klog.Errorf("Failed to look up %s %s in the informers cache,"+
						" will retry later: %v", r.ResourceHandler.ObjType, objKey, err)
					return
				}
			}
			entry.newObj = kObj
		}
		if r.ResourceHandler.NeedsUpdateDuringRetry && entry.config != nil && entry.newObj != nil {
			klog.Infof("%v retry: updating object %s", r.ResourceHandler.ObjType, objKey)
			if err := r.ResourceHandler.UpdateResource(entry.config, entry.newObj, true); err != nil {
				entry.timeStamp = time.Now()
				entry.failedAttempts++
				if entry.failedAttempts >= MaxFailedAttempts {
					klog.Errorf("Retry update failed final attempt for %s %s: error: %v", r.ResourceHandler.ObjType, objKey, err)
				} else {
					klog.Infof("%v retry update failed for %s, will try again later: %v", r.ResourceHandler.ObjType, objKey, err)
				}
				return
			}
			// successfully cleaned up new and old object, remove it from the retry cache
			entry.newObj = nil
			entry.config = nil
		} else {
			// delete old object if needed
			if entry.oldObj != nil {
				klog.Infof("Removing old object: %s %s (failed: %v)",
					r.ResourceHandler.ObjType, objKey, entry.failedAttempts)
				if !r.ResourceHandler.IsResourceScheduled(entry.oldObj) {
					klog.V(5).Infof("Retry: %s %s not scheduled", r.ResourceHandler.ObjType, objKey)
					entry.failedAttempts++
					return
				}
				if err := r.ResourceHandler.DeleteResource(entry.oldObj, entry.config); err != nil {
					entry.timeStamp = time.Now()
					entry.failedAttempts++
					if entry.failedAttempts >= MaxFailedAttempts {
						klog.Errorf("Retry delete failed final attempt for %s %s: error: %v", r.ResourceHandler.ObjType, objKey, err)
					} else {
						klog.Infof("Retry delete failed for %s %s, will try again later: %v",
							r.ResourceHandler.ObjType, objKey, err)
					}

					return
				}
				// successfully cleaned up old object, remove it from the retry cache
				entry.oldObj = nil
			}

			// create new object if needed
			if entry.newObj != nil {
				klog.Infof("Adding new object: %s %s", r.ResourceHandler.ObjType, objKey)
				if !r.ResourceHandler.IsResourceScheduled(entry.newObj) {
					klog.V(5).Infof("Retry: %s %s not scheduled", r.ResourceHandler.ObjType, objKey)
					entry.failedAttempts++
					return
				}
				if err := r.ResourceHandler.AddResource(entry.newObj, true); err != nil {
					entry.timeStamp = time.Now()
					entry.failedAttempts++
					if entry.failedAttempts >= MaxFailedAttempts {
						klog.Errorf("Retry add failed final attempt for %s %s: error: %v", r.ResourceHandler.ObjType, objKey, err)
					} else {
						klog.Infof("Retry add failed for %s %s, will try again later: %v", r.ResourceHandler.ObjType, objKey, err)
					}
					return
				}
				// successfully cleaned up new object, remove it from the retry cache
				entry.newObj = nil
			}
		}

		klog.Infof("Retry successful for %s %s after %d failed attempt(s)", r.ResourceHandler.ObjType, objKey, entry.failedAttempts)
		if initObj != nil {
			r.ResourceHandler.RecordSuccessEvent(initObj)
		}
		r.DeleteRetryObj(key)
	})
}

// iterateRetryResources checks if any outstanding resource objects exist and if so it tries to
// re-add them. updateAll forces all objects to be attempted to be retried regardless.
// iterateRetryResources makes a snapshot of keys present in the r.retryEntries cache, and runs retry only
// for those keys. New changes may be applied to saved keys entries while iterateRetryResources is executed.
// Deleted entries will be ignored, and all the updates will be reflected with key Lock.
// Keys added after the snapshot was done won't be retried during this run.
func (r *RetryFramework) iterateRetryResources() {
	entriesKeys := r.retryEntries.GetKeys()
	if len(entriesKeys) == 0 {
		return
	}
	now := time.Now()
	wg := &sync.WaitGroup{}

	// Process the above list of objects that need retry by holding the lock for each one of them.
	klog.V(5).Infof("Going to retry %v resource setup for %d objects: %s", r.ResourceHandler.ObjType, len(entriesKeys), entriesKeys)

	for _, entryKey := range entriesKeys {
		wg.Add(1)
		go func(entryKey string) {
			defer wg.Done()
			r.resourceRetry(entryKey, now)
		}(entryKey)
	}
	klog.V(5).Infof("Waiting for all the %s retry setup to complete in iterateRetryResources", r.ResourceHandler.ObjType)
	wg.Wait()
	klog.V(5).Infof("Function iterateRetryResources for %s ended (in %v)", r.ResourceHandler.ObjType, time.Since(now))
}

// periodicallyRetryResources tracks RetryFramework and checks if any object needs to be retried for add or delete every
// RetryObjInterval seconds or when requested through retryChan.
func (r *RetryFramework) periodicallyRetryResources() {
	timer := time.NewTicker(RetryObjInterval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			r.iterateRetryResources()

		case <-r.retryChan:
			klog.V(5).Infof("periodicallyRetryResources: Retry channel got triggered: retrying failed objects of type %s", r.ResourceHandler.ObjType)
			r.iterateRetryResources()
			timer.Reset(RetryObjInterval)

		case <-r.stopChan:
			klog.V(5).Infof("Stop channel got triggered: will stop retrying failed objects of type %s", r.ResourceHandler.ObjType)
			return
		}
	}
}

type resourceEvent string

var (
	resourceEventAdd    resourceEvent = "add"
	resourceEventUpdate resourceEvent = "update"
)

// processObjectInTerminalState is executed when an object has been added or updated and is actually in a terminal state
// already. The add or update event is not valid for such object, which we now remove from the cluster in order to
// free its resources. (for now, this applies to completed pods)
// processObjectInTerminalState doesn't unlock key
func (r *RetryFramework) processObjectInTerminalState(obj interface{}, lockedKey string, event resourceEvent) {
	_, loaded := r.terminatedObjects.LoadOrStore(lockedKey, true)
	if loaded {
		// object was already terminated
		klog.Infof("Detected object %s of type %s in terminal state (e.g. completed) will be "+
			"ignored as it has already been processed", lockedKey, r.ResourceHandler.ObjType)
		return
	}

	// The object is in a terminal state: delete it from the cluster, delete its retry entry and return.
	klog.Infof("Detected object %s of type %s in terminal state (e.g. completed)"+
		" during %s event: will remove it", lockedKey, r.ResourceHandler.ObjType, event)
	internalCacheEntry := r.ResourceHandler.GetInternalCacheEntry(obj)
	retryEntry := r.InitRetryObjWithDelete(obj, lockedKey, internalCacheEntry, true) // set up the retry obj for deletion
	if err := r.ResourceHandler.DeleteResource(obj, internalCacheEntry); err != nil {
		klog.Errorf("Failed to delete object %s of type %s in terminal state, during %s event: %v",
			lockedKey, r.ResourceHandler.ObjType, event, err)
		r.ResourceHandler.RecordErrorEvent(obj, "ErrorDeletingResource", err)
		r.increaseFailedAttemptsCounter(retryEntry)
		return
	}
	r.DeleteRetryObj(lockedKey)
}

func (r *RetryFramework) WatchResource() (*factory.Handler, error) {
	return r.WatchResourceFiltered("", nil)
}

// WatchResourceFiltered starts the watching of a resource type, manages its retry entries and calls
// back the appropriate handler logic. It also starts a goroutine that goes over all retry objects
// periodically or when explicitly requested.
// Note: when applying WatchResourceFiltered to a new resource type, the appropriate resource-specific logic must be added to the
// the different methods it calls.
func (r *RetryFramework) WatchResourceFiltered(namespaceForFilteredHandler string, labelSelectorForFilteredHandler labels.Selector) (*factory.Handler, error) {
	addHandlerFunc, err := r.watchFactory.GetResourceHandlerFunc(r.ResourceHandler.ObjType)
	if err != nil {
		return nil, fmt.Errorf("no resource handler function found for resource %v. "+
			"Cannot watch this resource", r.ResourceHandler.ObjType)
	}

	// create the actual watcher
	handler, err := addHandlerFunc(
		namespaceForFilteredHandler,     // filter out objects not in this namespace
		labelSelectorForFilteredHandler, // filter out objects not matching these labels
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if !r.ResourceHandler.FilterResource(obj) {
					return
				}
				r.ResourceHandler.RecordAddEvent(obj)

				key, err := GetResourceKey(obj)
				if err != nil {
					klog.Errorf("Upon add event: %v", err)
					return
				}
				klog.V(5).Infof("Add event received for %s %s", r.ResourceHandler.ObjType, key)

				r.DoWithLock(key, func(key string) {
					// This only applies to pod watchers (pods + dynamic network policy handlers watching pods):
					// if ovnkube-master is restarted, it will get all the add events with completed pods
					if r.ResourceHandler.IsObjectInTerminalState(obj) {
						r.processObjectInTerminalState(obj, key, resourceEventAdd)
						return
					}

					retryObj := r.initRetryObjWithAdd(obj, key)
					// If there is a delete entry with the same key, we got an add event for an object
					// with the same name as a previous object that failed deletion.
					// Destroy the old object before we add the new one.
					if retryObj.oldObj != nil {
						klog.Infof("Detected stale object during new object"+
							" add of type %s with the same key: %s",
							r.ResourceHandler.ObjType, key)
						internalCacheEntry := r.ResourceHandler.GetInternalCacheEntry(obj)
						if err := r.ResourceHandler.DeleteResource(retryObj.oldObj, internalCacheEntry); err != nil {
							klog.Errorf("Failed to delete old object %s of type %s,"+
								" during add event: %v", key, r.ResourceHandler.ObjType, err)
							r.ResourceHandler.RecordErrorEvent(obj, "ErrorDeletingResource", err)
							r.increaseFailedAttemptsCounter(retryObj)
							return
						}
						r.removeDeleteFromRetryObj(retryObj)
					}
					start := time.Now()
					if err := r.ResourceHandler.AddResource(obj, false); err != nil {
						if !ovntypes.IsSuppressedError(err) {
							klog.Errorf("Failed to create %s %s, error: %v", r.ResourceHandler.ObjType, key, err)
							r.ResourceHandler.RecordErrorEvent(obj, "ErrorAddingResource", err)
						} else {
							klog.Infof("Failed to create %s %s, error: %v", r.ResourceHandler.ObjType, key, err)
						}
						r.increaseFailedAttemptsCounter(retryObj)
						return
					}
					klog.V(5).Infof("Creating %s %s took: %v", r.ResourceHandler.ObjType, key, time.Since(start))
					// delete retryObj if handling was successful
					r.DeleteRetryObj(key)
					r.ResourceHandler.RecordSuccessEvent(obj)
				})
			},
			UpdateFunc: func(old, newer interface{}) {
				if !r.ResourceHandler.FilterResource(newer) {
					return
				}
				// skip the whole update if old and newer are equal
				areEqual, err := r.ResourceHandler.AreResourcesEqual(old, newer)
				if err != nil {
					klog.Errorf("Could not compare old and newer resource objects of type %s: %v",
						r.ResourceHandler.ObjType, err)
					return
				}
				klog.V(5).Infof("Update event received for resource %s, old object is equal to new: %t",
					r.ResourceHandler.ObjType, areEqual)
				if areEqual {
					return
				}
				r.ResourceHandler.RecordUpdateEvent(newer)

				// get the object keys for newer and old (expected to be the same)
				newKey, err := GetResourceKey(newer)
				if err != nil {
					klog.Errorf("Update of %s failed when looking up key of new obj: %v",
						r.ResourceHandler.ObjType, err)
					return
				}
				oldKey, err := GetResourceKey(old)
				if err != nil {
					klog.Errorf("Update of %s failed when looking up key of old obj: %v",
						r.ResourceHandler.ObjType, err)
					return
				}
				if newKey != oldKey {
					klog.Errorf("Could not update resource object of type %s: the key was changed from %s to %s",
						r.ResourceHandler.ObjType, oldKey, newKey)
					return
				}

				// skip the whole update if the new object doesn't exist anymore in the API server
				latest, err := r.ResourceHandler.GetResourceFromInformerCache(newKey)
				if err != nil {
					// When processing an object in terminal state there is a chance that it was already removed from
					// the API server. Since delete events for objects in terminal state are skipped delete it here.
					// This only applies to pod watchers (pods + dynamic network policy handlers watching pods).
					if kerrors.IsNotFound(err) {
						if r.ResourceHandler.IsObjectInTerminalState(newer) {
							klog.V(5).Infof("%s %s is in terminal state but no longer exists in informer cache, removing",
								r.ResourceHandler.ObjType, newKey)
							r.DoWithLock(newKey, func(key string) {
								r.processObjectInTerminalState(newer, newKey, resourceEventUpdate)
							})
						} else {
							klog.V(5).Infof("Ignoring update event for %s %s as it was not found in"+
								" informer cache and is not in a terminal state", r.ResourceHandler.ObjType, newKey)
						}
					} else {
						// This should never happen. The cache storage backend type cannot return any error
						// other than not found
						klog.Errorf("Unhandled error while trying to retrieve %s %s from informer cache: %v",
							r.ResourceHandler.ObjType, newKey, err)
					}
					return
				}

				klog.V(5).Infof("Update event received for %s %s", r.ResourceHandler.ObjType, newKey)

				r.DoWithLock(newKey, func(key string) {
					// STEP 1:
					// Delete existing (old) object if:
					// a) it has a retry entry marked for deletion and doesn't use update or
					// b) the resource is in terminal state (e.g. pod is completed) or
					// c) this resource type has no update function, so an update means delete old obj and add new one
					//
					retryEntryOrNil, found := r.getRetryObj(key)
					// retryEntryOrNil may be nil if found=false

					if found && retryEntryOrNil.oldObj != nil {
						// [step 1a] there is a retry entry marked for deletion
						klog.Infof("Found retry entry for %s %s marked for deletion: will delete the object",
							r.ResourceHandler.ObjType, oldKey)
						if err := r.ResourceHandler.DeleteResource(retryEntryOrNil.oldObj,
							retryEntryOrNil.config); err != nil {
							klog.Errorf("Failed to delete stale object %s, during update: %v", oldKey, err)
							r.ResourceHandler.RecordErrorEvent(retryEntryOrNil.oldObj, "ErrorDeletingResource", err)
							retryEntry := r.initRetryObjWithAdd(latest, key)
							r.increaseFailedAttemptsCounter(retryEntry)
							return
						}
						// remove the old object from retry entry since it was correctly deleted
						if found {
							r.removeDeleteFromRetryObj(retryEntryOrNil)
						}
					} else if r.ResourceHandler.IsObjectInTerminalState(latest) { // check the latest status on newer
						// [step 1b] The object is in a terminal state: delete it from the cluster,
						// delete its retry entry and return. This only applies to pod watchers
						// (pods + dynamic network policy handlers watching pods).
						r.processObjectInTerminalState(latest, key, resourceEventUpdate)
						return

					} else if !r.ResourceHandler.HasUpdateFunc {
						// [step 1c] if this resource type has no update function,
						// delete old obj and in step 2 add the new one
						var existingCacheEntry interface{}
						if found {
							existingCacheEntry = retryEntryOrNil.config
						}
						klog.V(5).Infof("Deleting old %s of type %s during update", oldKey, r.ResourceHandler.ObjType)
						if err := r.ResourceHandler.DeleteResource(old, existingCacheEntry); err != nil {
							klog.Errorf("Failed to delete %s %s, during update: %v",
								r.ResourceHandler.ObjType, oldKey, err)
							r.ResourceHandler.RecordErrorEvent(old, "ErrorDeletingResource", err)
							retryEntry := r.InitRetryObjWithDelete(old, key, nil, false)
							r.initRetryObjWithAdd(latest, key)
							r.increaseFailedAttemptsCounter(retryEntry)
							return
						}
						// remove the old object from retry entry since it was correctly deleted
						if found {
							r.removeDeleteFromRetryObj(retryEntryOrNil)
						}
					}
					// STEP 2:
					// Execute the update function for this resource type; resort to add if no update
					// function is available.
					if r.ResourceHandler.HasUpdateFunc {
						// if this resource type has an update func, just call the update function
						if err := r.ResourceHandler.UpdateResource(old, latest, found); err != nil {
							if !ovntypes.IsSuppressedError(err) {
								klog.Errorf("Failed to update %s, old=%s, new=%s, error: %v",
									r.ResourceHandler.ObjType, oldKey, newKey, err)
								r.ResourceHandler.RecordErrorEvent(latest, "ErrorUpdatingResource", err)
							} else {
								klog.V(5).Infof("Failed to update %s, old=%s, new=%s, error: %v",
									r.ResourceHandler.ObjType, oldKey, newKey, err)
							}
							var retryEntry *retryObjEntry
							if r.ResourceHandler.NeedsUpdateDuringRetry {
								retryEntry = r.initRetryObjWithUpdate(old, latest, key)
							} else {
								retryEntry = r.initRetryObjWithAdd(latest, key)
							}
							r.increaseFailedAttemptsCounter(retryEntry)
							return
						}
					} else { // we previously deleted old object, now let's add the new one
						if err := r.ResourceHandler.AddResource(latest, false); err != nil {
							retryEntry := r.initRetryObjWithAdd(latest, key)
							r.increaseFailedAttemptsCounter(retryEntry)
							if !ovntypes.IsSuppressedError(err) {
								klog.Errorf("Failed to add %s %s, during update: %v",
									r.ResourceHandler.ObjType, newKey, err)
								r.ResourceHandler.RecordErrorEvent(latest, "ErrorAddingResource", err)
							} else {
								klog.Infof("Failed to add %s %s, during update: %v",
									r.ResourceHandler.ObjType, newKey, err)
							}
							return
						}
					}
					r.DeleteRetryObj(key)
					r.ResourceHandler.RecordSuccessEvent(latest)
				})
			},
			DeleteFunc: func(obj interface{}) {
				if !r.ResourceHandler.FilterResource(obj) {
					return
				}
				r.ResourceHandler.RecordDeleteEvent(obj)
				key, err := GetResourceKey(obj)
				if err != nil {
					klog.Errorf("Delete of %s failed: %v", r.ResourceHandler.ObjType, err)
					return
				}
				klog.V(5).Infof("Delete event received for %s %s", r.ResourceHandler.ObjType, key)
				// If object is in terminal state, we would have already deleted it during update.
				// No reason to attempt to delete it here again.
				if r.ResourceHandler.IsObjectInTerminalState(obj) {
					// If object is in terminal state, check if we have already processed it in a previous update.
					// We cannot blindly handle multiple delete operations for the same pod currently. There can be races
					// where other pod handlers are removing IP addresses from address sets when they shouldn't be, etc.
					// See: https://github.com/ovn-org/ovn-kubernetes/pull/3318#issuecomment-1349804450
					if _, loaded := r.terminatedObjects.LoadAndDelete(key); loaded {
						// object was already terminated
						klog.Infof("Ignoring delete event for resource in terminal state %s %s",
							r.ResourceHandler.ObjType, key)
						return
					}
				}
				r.DoWithLock(key, func(key string) {
					internalCacheEntry := r.ResourceHandler.GetInternalCacheEntry(obj)
					retryEntry := r.InitRetryObjWithDelete(obj, key, internalCacheEntry, false) // set up the retry obj for deletion
					if err = r.ResourceHandler.DeleteResource(obj, internalCacheEntry); err != nil {
						retryEntry.failedAttempts++
						klog.Errorf("Failed to delete %s %s, error: %v", r.ResourceHandler.ObjType, key, err)
						return
					}
					r.DeleteRetryObj(key)
					r.ResourceHandler.RecordSuccessEvent(obj)
				})
			},
		},
		r.ResourceHandler.SyncFunc) // processes all existing objects at startup

	if err != nil {
		return nil, fmt.Errorf("watchResource for resource %v. "+
			"Failed addHandlerFunc: %v", r.ResourceHandler.ObjType, err)
	}

	// track the retry entries and every 30 seconds (or upon explicit request) check if any objects
	// need to be retried
	r.doneWg.Add(1)
	go func() {
		defer r.doneWg.Done()
		r.periodicallyRetryResources()
	}()

	return handler, nil
}

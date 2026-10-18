// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package retry

import (
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const RetryObjInterval = 30 * time.Second
const MaxFailedAttempts = 15 // same value used for the services level-driven controller
const initialBackoff = 1 * time.Second
const noBackoff = 0
const maxBackoff = 60 * time.Second

// retryObjEntry is a generic object caching with retry mechanism
// that resources can use to eventually complete their intended operations.
type retryObjEntry struct {
	// newObj holds k8s resource failed during add operation
	newObj interface{}
	// oldObj holds k8s resource failed during delete operation
	oldObj interface{}
	// config holds feature specific configuration,
	// currently used by network policies and pods.
	config    interface{}
	timeStamp time.Time
	backoff   time.Duration
	// number of times this object has been unsuccessfully added/updated/deleted
	failedAttempts uint8
	// infiniteRetry indicates whether this object should be retried indefinitely, regardless of the number of failed attempts
	// Used for pods only right now
	infiniteRetry bool
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
	FilterOutResource(obj interface{}) bool
}

// deleteResourceFilter allows handlers to use delete-specific filtering when
// ownership can no longer be determined from the live object state.
type deleteResourceFilter interface {
	FilterOutDeleteResource(obj interface{}) bool
}

// DefaultEventHandler has the default implementations for some EventHandler
// methods, that are not required for every handler
type DefaultEventHandler struct{}

func (h *DefaultEventHandler) SyncFunc([]interface{}) error { return nil }

func (h *DefaultEventHandler) AreResourcesEqual(_, _ interface{}) (bool, error) {
	return false, nil
}

func (h *DefaultEventHandler) GetInternalCacheEntry(_ interface{}) interface{} { return nil }

func (h *DefaultEventHandler) IsResourceScheduled(_ interface{}) bool { return true }

func (h *DefaultEventHandler) IsObjectInTerminalState(_ interface{}) bool { return false }

func (h *DefaultEventHandler) RecordAddEvent(_ interface{}) {}

func (h *DefaultEventHandler) RecordUpdateEvent(_ interface{}) {}

func (h *DefaultEventHandler) RecordDeleteEvent(_ interface{}) {}

func (h *DefaultEventHandler) RecordSuccessEvent(_ interface{}) {}

func (h *DefaultEventHandler) RecordErrorEvent(_ interface{}, _ string, _ error) {}

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
	name string
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
// with its function attributes pre-filled with default code. Clients of this pkg (ovnkube controller,
// ovnkube node) will have to override the functions in the returned struct with the desired
// per-resource logic.
func NewRetryFramework(name string,
	stopChan <-chan struct{}, doneWg *sync.WaitGroup,
	watchFactory *factory.WatchFactory,
	resourceHandler *ResourceHandler) *RetryFramework {
	return &RetryFramework{
		name:              name,
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
	entry, _ := r.retryEntries.LoadOrStore(lockedKey, &retryObjEntry{backoff: backoff})
	entry.timeStamp = time.Now()
	entry.newObj = obj
	if _, isPod := obj.(*corev1.Pod); isPod {
		// for pods we want to retry indefinitely
		entry.infiniteRetry = true
	}
	entry.failedAttempts = 0
	entry.backoff = backoff
	return entry
}

// initRetryObjWithAdd creates a retry entry for an object that is being added,
// so that, if it fails, the add can be potentially retried later.
func (r *RetryFramework) initRetryObjWithAdd(obj interface{}, lockedKey string) *retryObjEntry {
	return r.initRetryObjWithAddBackoff(obj, lockedKey, initialBackoff)
}

// initRetryObjWithUpdate tracks objects that failed to be updated to potentially retry later
func (r *RetryFramework) initRetryObjWithUpdate(oldObj, newObj interface{}, lockedKey string) *retryObjEntry {
	entry, _ := r.retryEntries.LoadOrStore(lockedKey, &retryObjEntry{config: oldObj, backoff: initialBackoff})
	// even if the object was loaded and changed before with the same lock, LoadOrStore will return reference to the same object
	entry.timeStamp = time.Now()
	entry.newObj = newObj
	if _, isPod := newObj.(*corev1.Pod); isPod {
		// for pods we want to retry indefinitely
		entry.infiniteRetry = true
	}
	entry.config = oldObj
	entry.failedAttempts = 0
	return entry
}

// initRetryObjWithDelete creates a retry entry for an object that is being deleted,
// so that, if it fails, the delete can be potentially retried later.
// When applied to pods, we include the config object as well in case the namespace is removed
// and the object is orphaned from the namespace.
// The noRetryAdd boolean argument is to indicate whether to retry for addition
func (r *RetryFramework) initRetryObjWithDelete(obj interface{}, lockedKey string, config interface{}, noRetryAdd bool) *retryObjEntry {
	return r.initRetryObjWithDeleteBackoff(obj, lockedKey, config, noRetryAdd, initialBackoff)
}

func (r *RetryFramework) initRetryObjWithDeleteBackoff(obj interface{}, lockedKey string, config interface{}, noRetryAdd bool, backoff time.Duration) *retryObjEntry {
	// even if the object was loaded and changed before with the same lock, LoadOrStore will return reference to the same object
	entry, _ := r.retryEntries.LoadOrStore(lockedKey, &retryObjEntry{config: config, backoff: initialBackoff})
	entry.timeStamp = time.Now()
	entry.oldObj = obj
	if _, isPod := obj.(*corev1.Pod); isPod {
		// for pods we want to retry indefinitely
		entry.infiniteRetry = true
	}
	if entry.config == nil {
		entry.config = config
	}
	entry.failedAttempts = 0
	if noRetryAdd {
		// will not be retried for addition
		entry.newObj = nil
	}
	entry.backoff = backoff
	return entry
}

func (r *RetryFramework) AddRetryObjWithDeleteNoBackoff(obj interface{}) error {
	key, err := GetResourceKey(obj)
	if err != nil {
		return fmt.Errorf("could not get the key of %s %v: %v", r.ResourceHandler.ObjType, obj, err)
	}
	r.DoWithLock(key, func(key string) {
		r.initRetryObjWithDeleteBackoff(obj, key, nil, true, noBackoff)
	})
	return nil
}

// AddRetryObjWithAddNoBackoff adds an object to be retried immediately for add.
// It will lock the key, create or update retryObject, and unlock the key
func (r *RetryFramework) AddRetryObjWithAddNoBackoff(obj interface{}) error {
	key, err := GetResourceKey(obj)
	if err != nil {
		return fmt.Errorf("%s: could not get the key of %s %v: %v", r.name, r.ResourceHandler.ObjType, obj, err)
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
	entry.backoff = noBackoff
}

// removeDeleteFromRetryObj removes any old object from a retry entry
func (r *RetryFramework) removeDeleteFromRetryObj(entry *retryObjEntry) {
	entry.oldObj = nil
	entry.config = nil
}

// increaseFailedAttemptsCounter increases by one the counter of failed add/update/delete attempts
// for the given key
func (r *RetryFramework) increaseFailedAttemptsCounter(entry *retryObjEntry) {
	// avoid overflowing the counter for infinite retries
	if entry.failedAttempts < 255 {
		entry.failedAttempts++
	}
}

// RequestRetryFramework allows a caller to immediately request to iterate through all objects that
// are in the retry cache. This will ignore any outstanding time wait/backoff state
func (r *RetryFramework) RequestRetryObjs() {
	select {
	case r.retryChan <- struct{}{}:
		klog.V(5).Infof("%s: iterate retry objects requested (resource %s)", r.name, r.ResourceHandler.ObjType)
	default:
		klog.V(5).Infof("%s: iterate retry objects already requested (resource %s)", r.name, r.ResourceHandler.ObjType)
	}
}

// RequestRetryObjWithNoBackoff requests immediate retry for the given object key
// if it is already present in the retry cache. It returns true when a retry entry
// was found and requested, false otherwise.
func (r *RetryFramework) RequestRetryObjWithNoBackoff(obj interface{}) (bool, error) {
	key, err := GetResourceKey(obj)
	if err != nil {
		return false, fmt.Errorf("could not get the key of %s %v: %v", r.ResourceHandler.ObjType, obj, err)
	}
	requested := false
	r.DoWithLock(key, func(key string) {
		entry, loaded := r.getRetryObj(key)
		if !loaded {
			return
		}
		r.setRetryObjWithNoBackoff(entry)
		requested = true
	})
	if requested {
		r.RequestRetryObjs()
	}
	return requested, nil
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
			klog.V(5).Infof("%s: %v resource %s was not found in the iterateRetryResources map while retrying resource setup", r.name, r.ResourceHandler.ObjType, objKey)
			return
		}

		if entry.failedAttempts >= MaxFailedAttempts && !entry.infiniteRetry {
			klog.Warningf("%s: dropping retry entry for %s %s: exceeded number of failed attempts",
				r.name, r.ResourceHandler.ObjType, objKey)
			r.DeleteRetryObj(key)
			metrics.MetricResourceRetryFailuresCount.Inc()
			if entry.newObj != nil {
				r.ResourceHandler.RecordErrorEvent(entry.newObj, "RetryFailed",
					fmt.Errorf("%s: failed to reconcile and retried %d times for object: %v", r.name, MaxFailedAttempts, entry.newObj))
			}
			return
		}
		forceRetry := false
		// check if immediate retry is requested
		if entry.backoff == noBackoff {
			entry.backoff = initialBackoff
			forceRetry = true
		}
		backoff := entry.backoff + (time.Duration(rand.Intn(500)) * time.Millisecond)
		objTimer := entry.timeStamp.Add(backoff)
		if !forceRetry && now.Before(objTimer) {
			klog.V(5).Infof("%s: attempting retry of %s %s before timer (time: %s): skip", r.name, r.ResourceHandler.ObjType, objKey, objTimer)
			return
		}

		// update backoff for future attempts in case of failure
		entry.backoff = entry.backoff * 2
		if entry.backoff > maxBackoff {
			entry.backoff = maxBackoff
		}

		// storing original obj for metrics
		var initObj interface{}
		if entry.newObj != nil {
			initObj = entry.newObj
		} else if entry.oldObj != nil {
			initObj = entry.oldObj
		}

		klog.V(5).Infof("%s: retry object setup: %s %s (attempt %d, backoff %s)", r.name, r.ResourceHandler.ObjType, objKey, entry.failedAttempts, entry.backoff)

		if entry.newObj != nil {
			// get the latest version of the object from the informer;
			// if it doesn't exist we are not going to create the new object.
			kObj, err := r.ResourceHandler.GetResourceFromInformerCache(objKey)
			if err != nil {
				if apierrors.IsNotFound(err) {
					klog.Infof("%s: %s %s not found in the informers cache,"+
						" not going to retry object create", r.name, r.ResourceHandler.ObjType, objKey)
					kObj = nil
				} else {
					klog.Errorf("%s: failed to look up %s %s in the informers cache,"+
						" will retry later: %v", r.name, r.ResourceHandler.ObjType, objKey, err)
					return
				}
			}
			entry.newObj = kObj
		}
		if r.ResourceHandler.NeedsUpdateDuringRetry && entry.config != nil && entry.newObj != nil {
			klog.Infof("%s: %v retry: updating object %s", r.name, r.ResourceHandler.ObjType, objKey)
			if !r.ResourceHandler.IsResourceScheduled(entry.newObj) {
				// unscheduled resources (pods) will be retried again later we do not track these as failures, and should not retry.
				// we should avoid queuing objects to the retry handler that are not scheduled. Thus treat this as an error.
				klog.Errorf("%s: %v retry: cannot update object that is not scheduled: %s", r.name, r.ResourceHandler.ObjType, objKey)
			} else if err := r.ResourceHandler.UpdateResource(entry.config, entry.newObj, true); err != nil {
				entry.timeStamp = time.Now()
				if !ovntypes.IsSuppressedError(err) {
					r.increaseFailedAttemptsCounter(entry)
					if entry.failedAttempts >= MaxFailedAttempts && !entry.infiniteRetry {
						klog.Errorf("%s: retry update failed final attempt for %s %s: error: %v", r.name, r.ResourceHandler.ObjType, objKey, err)
					} else {
						klog.Infof("%s: %v retry update failed for %s, will try again later: %v", r.name, r.ResourceHandler.ObjType, objKey, err)
					}
				} else {
					klog.V(5).Infof("%s: %v retry update suppressed for %s: %v", r.name, r.ResourceHandler.ObjType, objKey, err)
				}
				return
			}
			// successfully cleaned up new and old object, remove it from the retry cache
			entry.newObj = nil
			entry.config = nil
		} else {
			// delete old object if needed
			if entry.oldObj != nil {
				klog.Infof("%s: removing old object: %s %s (failed: %v)", r.name, r.ResourceHandler.ObjType, objKey, entry.failedAttempts)
				if !r.ResourceHandler.IsResourceScheduled(entry.oldObj) {
					// unscheduled resources (pods) will be retried again later we do not track these as failures, and should not retry.
					// we should avoid queuing objects to the retry handler that are not scheduled. Thus treat this as an error.
					klog.Errorf("%s: %v retry: cannot delete object that was not scheduled %s", r.name, r.ResourceHandler.ObjType, objKey)
				} else if err := r.ResourceHandler.DeleteResource(entry.oldObj, entry.config); err != nil {
					entry.timeStamp = time.Now()
					r.increaseFailedAttemptsCounter(entry)
					if entry.failedAttempts >= MaxFailedAttempts && !entry.infiniteRetry {
						klog.Errorf("%s: retry delete failed final attempt for %s %s: error: %v", r.name, r.ResourceHandler.ObjType, objKey, err)
					} else {
						klog.Infof("%s: retry delete failed for %s %s, will try again later: %v",
							r.name, r.ResourceHandler.ObjType, objKey, err)
					}

					return
				}
				// successfully cleaned up old object, remove it from the retry cache
				entry.oldObj = nil
			}

			// create new object if needed
			if entry.newObj != nil {
				klog.V(5).Infof("%s: adding new object: %s %s", r.name, r.ResourceHandler.ObjType, objKey)
				if !r.ResourceHandler.IsResourceScheduled(entry.newObj) {
					// unscheduled resources (pods) will be retried again later we do not track these as failures, and should not retry.
					// we should avoid queuing objects to the retry handler that are not scheduled. Thus treat this as an error.
					klog.Errorf("%s: %v retry: cannot create object that is not scheduled %s", r.name, r.ResourceHandler.ObjType, objKey)
				} else if err := r.ResourceHandler.AddResource(entry.newObj, true); err != nil {
					entry.timeStamp = time.Now()
					if !ovntypes.IsSuppressedError(err) {
						r.increaseFailedAttemptsCounter(entry)
						if entry.failedAttempts >= MaxFailedAttempts && !entry.infiniteRetry {
							klog.Errorf("%s: retry add failed final attempt for %s %s: error: %v", r.name, r.ResourceHandler.ObjType, objKey, err)
						} else {
							klog.Infof("%s: retry add failed for %s %s, will try again later: %v", r.name, r.ResourceHandler.ObjType, objKey, err)
						}
					} else {
						klog.V(5).Infof("%s: %v retry add suppressed for %s: %v", r.name, r.ResourceHandler.ObjType, objKey, err)
					}
					return
				}
				// successfully cleaned up new object, remove it from the retry cache
				entry.newObj = nil
			}
		}

		if entry.failedAttempts > 0 {
			klog.Infof("%s: retry successful for %s %s after %d failed attempt(s)", r.name, r.ResourceHandler.ObjType, objKey, entry.failedAttempts)
		}
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
	klog.V(5).Infof("%s: going to retry %v resource setup for %d objects: %s", r.name, r.ResourceHandler.ObjType, len(entriesKeys), entriesKeys)

	for _, entryKey := range entriesKeys {
		wg.Add(1)
		go func(entryKey string) {
			defer wg.Done()
			r.resourceRetry(entryKey, now)
		}(entryKey)
	}
	klog.V(5).Infof("%s: waiting for all the %s retry setup to complete in iterateRetryResources", r.name, r.ResourceHandler.ObjType)
	wg.Wait()
	klog.V(5).Infof("%s: function iterateRetryResources for %s ended (in %v)", r.name, r.ResourceHandler.ObjType, time.Since(now))
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
			klog.V(5).Infof("%s: periodicallyRetryResources: Retry channel got triggered: retrying failed objects of type %s", r.name, r.ResourceHandler.ObjType)
			r.iterateRetryResources()
			timer.Reset(RetryObjInterval)

		case <-r.stopChan:
			klog.V(5).Infof("%s: stop channel got triggered: will stop retrying failed objects of type %s", r.name, r.ResourceHandler.ObjType)
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
		klog.V(5).Infof("%s: detected object %s of type %s in terminal state (e.g. completed) will be "+
			"ignored as it has already been processed", r.name, lockedKey, r.ResourceHandler.ObjType)
		return
	}

	// The object is in a terminal state: delete it from the cluster, delete its retry entry and return.
	klog.V(5).Infof("%s: detected object %s of type %s in terminal state (e.g. completed)"+
		" during %s event: will remove it", r.name, lockedKey, r.ResourceHandler.ObjType, event)
	internalCacheEntry := r.ResourceHandler.GetInternalCacheEntry(obj)
	retryEntry := r.initRetryObjWithDelete(obj, lockedKey, internalCacheEntry, true) // set up the retry obj for deletion
	if err := r.ResourceHandler.DeleteResource(obj, internalCacheEntry); err != nil {
		klog.Errorf("%s: failed to delete object %s of type %s in terminal state, during %s event: %v",
			r.name, lockedKey, r.ResourceHandler.ObjType, event, err)
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
		return nil, fmt.Errorf("%s: no resource handler function found for resource %v. "+
			"Cannot watch this resource", r.name, r.ResourceHandler.ObjType)
	}

	// create the actual watcher
	handler, err := addHandlerFunc(
		namespaceForFilteredHandler,     // filter out objects not in this namespace
		labelSelectorForFilteredHandler, // filter out objects not matching these labels
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if r.ResourceHandler.FilterOutResource(obj) {
					return
				}
				r.ResourceHandler.RecordAddEvent(obj)

				key, err := GetResourceKey(obj)
				if err != nil {
					klog.Errorf("%s: upon add event for %s: %v", r.name, r.ResourceHandler.ObjType, err)
					return
				}
				klog.V(5).Infof("%s: add event received for %s %s", r.name, r.ResourceHandler.ObjType, key)

				r.DoWithLock(key, func(key string) {
					// This only applies to pod watchers (pods + dynamic network policy handlers watching pods):
					// if the watching controller restarts, it will get all the add events with completed pods
					if r.ResourceHandler.IsObjectInTerminalState(obj) {
						r.processObjectInTerminalState(obj, key, resourceEventAdd)
						return
					}

					retryObj := r.initRetryObjWithAdd(obj, key)
					// If there is a delete entry with the same key, we got an add event for an object
					// with the same name as a previous object that failed deletion.
					// Destroy the old object before we add the new one.
					if retryObj.oldObj != nil {
						klog.Infof("%s: detected stale object during new object"+
							" add of type %s with the same key: %s", r.name,
							r.ResourceHandler.ObjType, key)
						internalCacheEntry := r.ResourceHandler.GetInternalCacheEntry(obj)
						if err := r.ResourceHandler.DeleteResource(retryObj.oldObj, internalCacheEntry); err != nil {
							klog.Errorf("%s: failed to delete old object %s of type %s,"+
								" during add event: %v", r.name, key, r.ResourceHandler.ObjType, err)
							r.ResourceHandler.RecordErrorEvent(obj, "ErrorDeletingResource", err)
							r.increaseFailedAttemptsCounter(retryObj)
							return
						}
						r.removeDeleteFromRetryObj(retryObj)
					}
					start := time.Now()
					if err := r.ResourceHandler.AddResource(obj, false); err != nil {
						if !ovntypes.IsSuppressedError(err) {
							klog.Errorf("%s: failed to create %s %s, error: %v", r.name, r.ResourceHandler.ObjType, key, err)
							r.ResourceHandler.RecordErrorEvent(obj, "ErrorAddingResource", err)
							r.increaseFailedAttemptsCounter(retryObj)
						} else {
							klog.Infof("%s: failed to create %s %s, error: %v", r.name, r.ResourceHandler.ObjType, key, err)
						}
						return
					}
					klog.V(5).Infof("%s: creating %s %s took: %v", r.name, r.ResourceHandler.ObjType, key, time.Since(start))
					// delete retryObj if handling was successful
					r.DeleteRetryObj(key)
					r.ResourceHandler.RecordSuccessEvent(obj)
				})
			},
			UpdateFunc: func(old, newer interface{}) {
				if r.ResourceHandler.FilterOutResource(old) && r.ResourceHandler.FilterOutResource(newer) {
					return
				}
				// skip the whole update if old and newer are equal
				areEqual, err := r.ResourceHandler.AreResourcesEqual(old, newer)
				if err != nil {
					klog.Errorf("%s: could not compare old and newer resource objects of type %s: %v",
						r.name, r.ResourceHandler.ObjType, err)
					return
				}
				if areEqual {
					return
				}
				r.ResourceHandler.RecordUpdateEvent(newer)

				// get the object keys for newer and old (expected to be the same)
				newKey, err := GetResourceKey(newer)
				if err != nil {
					klog.Errorf("%s: update of %s failed when looking up key of new obj: %v",
						r.name, r.ResourceHandler.ObjType, err)
					return
				}
				oldKey, err := GetResourceKey(old)
				if err != nil {
					klog.Errorf("%s: update of %s failed when looking up key of old obj: %v",
						r.name, r.ResourceHandler.ObjType, err)
					return
				}
				if newKey != oldKey {
					klog.Errorf("%s: could not update resource object of type %s: the key was changed from %s to %s",
						r.name, r.ResourceHandler.ObjType, oldKey, newKey)
					return
				}

				// skip the whole update if the new object doesn't exist anymore in the API server
				latest, err := r.ResourceHandler.GetResourceFromInformerCache(newKey)
				if err != nil {
					// When processing an object in terminal state there is a chance that it was already removed from
					// the API server. Since delete events for objects in terminal state are skipped delete it here.
					// This only applies to pod watchers (pods + dynamic network policy handlers watching pods).
					if apierrors.IsNotFound(err) {
						if r.ResourceHandler.IsObjectInTerminalState(newer) {
							klog.V(5).Infof("%s: %s %s is in terminal state but no longer exists in informer cache, removing",
								r.name, r.ResourceHandler.ObjType, newKey)
							r.DoWithLock(newKey, func(_ string) {
								r.processObjectInTerminalState(newer, newKey, resourceEventUpdate)
							})
						} else {
							klog.V(5).Infof("%s: ignoring update event for %s %s as it was not found in"+
								" informer cache and is not in a terminal state", r.name, r.ResourceHandler.ObjType, newKey)
						}
					} else {
						// This should never happen. The cache storage backend type cannot return any error
						// other than not found
						klog.Errorf("%s: unhandled error while trying to retrieve %s %s from informer cache: %v",
							r.name, r.ResourceHandler.ObjType, newKey, err)
					}
					return
				}

				klog.V(5).Infof("%s: update event received for %s %s", r.name, r.ResourceHandler.ObjType, newKey)
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
						klog.Infof("%s: found retry entry for %s %s marked for deletion: will delete the object",
							r.name, r.ResourceHandler.ObjType, oldKey)
						if err := r.ResourceHandler.DeleteResource(retryEntryOrNil.oldObj,
							retryEntryOrNil.config); err != nil {
							klog.Errorf("%s: failed to delete stale object %s, during update: %v", r.name, oldKey, err)
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
						klog.V(5).Infof("%s: deleting old %s of type %s during update", r.name, oldKey, r.ResourceHandler.ObjType)
						if err := r.ResourceHandler.DeleteResource(old, existingCacheEntry); err != nil {
							klog.Errorf("%s: failed to delete %s %s, during update: %v",
								r.name, r.ResourceHandler.ObjType, oldKey, err)
							r.ResourceHandler.RecordErrorEvent(old, "ErrorDeletingResource", err)
							retryEntry := r.initRetryObjWithDelete(old, key, nil, false)
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
								klog.Errorf("%s: failed to update %s, old=%s, new=%s, error: %v",
									r.name, r.ResourceHandler.ObjType, oldKey, newKey, err)
								r.ResourceHandler.RecordErrorEvent(latest, "ErrorUpdatingResource", err)
							} else {
								klog.V(5).Infof("%s: failed to update %s, old=%s, new=%s, error: %v",
									r.name, r.ResourceHandler.ObjType, oldKey, newKey, err)
							}
							var retryEntry *retryObjEntry
							if r.ResourceHandler.NeedsUpdateDuringRetry {
								retryEntry = r.initRetryObjWithUpdate(old, latest, key)
							} else {
								retryEntry = r.initRetryObjWithAdd(latest, key)
							}
							if !ovntypes.IsSuppressedError(err) {
								r.increaseFailedAttemptsCounter(retryEntry)
							}
							return
						}
					} else { // we previously deleted old object, now let's add the new one
						if err := r.ResourceHandler.AddResource(latest, false); err != nil {
							retryEntry := r.initRetryObjWithAdd(latest, key)
							if !ovntypes.IsSuppressedError(err) {
								r.increaseFailedAttemptsCounter(retryEntry)
								klog.Errorf("%s: failed to add %s %s, during update: %v",
									r.name, r.ResourceHandler.ObjType, newKey, err)
								r.ResourceHandler.RecordErrorEvent(latest, "ErrorAddingResource", err)
							} else {
								klog.Infof("%s: failed to add %s %s, during update: %v",
									r.name, r.ResourceHandler.ObjType, newKey, err)
							}
							return
						}
					}
					r.DeleteRetryObj(key)
					r.ResourceHandler.RecordSuccessEvent(latest)
				})
			},
			DeleteFunc: func(obj interface{}) {
				var filterOut bool
				if handler, ok := r.ResourceHandler.EventHandler.(deleteResourceFilter); ok {
					filterOut = handler.FilterOutDeleteResource(obj)
				} else {
					filterOut = r.ResourceHandler.FilterOutResource(obj)
				}
				if filterOut {
					return
				}
				r.ResourceHandler.RecordDeleteEvent(obj)
				key, err := GetResourceKey(obj)
				if err != nil {
					klog.Errorf("%s: delete of %s failed: %v", r.name, r.ResourceHandler.ObjType, err)
					return
				}
				klog.V(5).Infof("%s: delete event received for %s %s", r.name, r.ResourceHandler.ObjType, key)
				// If object is in terminal state, we would have already deleted it during update.
				// No reason to attempt to delete it here again.
				if r.ResourceHandler.IsObjectInTerminalState(obj) {
					// If object is in terminal state, check if we have already processed it in a previous update.
					// We cannot blindly handle multiple delete operations for the same pod currently. There can be races
					// where other pod handlers are removing IP addresses from address sets when they shouldn't be, etc.
					// See: https://github.com/ovn-kubernetes/ovn-kubernetes/pull/3318#issuecomment-1349804450
					if _, loaded := r.terminatedObjects.LoadAndDelete(key); loaded {
						// object was already terminated
						klog.V(5).Infof("%s: ignoring delete event for resource in terminal state %s %s",
							r.name, r.ResourceHandler.ObjType, key)
						return
					}
				}
				r.DoWithLock(key, func(key string) {
					internalCacheEntry := r.ResourceHandler.GetInternalCacheEntry(obj)
					retryEntry := r.initRetryObjWithDelete(obj, key, internalCacheEntry, false) // set up the retry obj for deletion
					if err = r.ResourceHandler.DeleteResource(obj, internalCacheEntry); err != nil {
						r.increaseFailedAttemptsCounter(retryEntry)
						klog.Errorf("%s: failed to delete %s %s, error: %v", r.name, r.ResourceHandler.ObjType, key, err)
						return
					}
					r.DeleteRetryObj(key)
					r.ResourceHandler.RecordSuccessEvent(obj)
				})
			},
		},
		r.ResourceHandler.SyncFunc) // processes all existing objects at startup

	if err != nil {
		return nil, fmt.Errorf("%s: watchResource for resource %v. "+
			"Failed addHandlerFunc: %v", r.name, r.ResourceHandler.ObjType, err)
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

// RequeuePendingPods enqueues all Pending pods into the retryPods associated with netInfo.
func RequeuePendingPods(wf *factory.WatchFactory, netInfo util.NetInfo, retryPods *RetryFramework) error {
	var errs []error

	// NOTE: A pod may reference a NAD from a different namespace, so check all pending pods.
	allPods, err := wf.GetAllPods()
	if err != nil {
		return fmt.Errorf("failed to get all pods: %w", err)
	}

	podsAdded := false
	for _, pod := range allPods {
		pod := *pod
		if !util.PodScheduled(&pod) {
			continue
		}
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		klog.V(5).Infof("Adding pending pod %s/%s to retryPods for network %s", pod.Namespace, pod.Name, netInfo.GetNetworkName())
		err := retryPods.AddRetryObjWithAddNoBackoff(&pod)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		podsAdded = true
	}
	if podsAdded {
		retryPods.RequestRetryObjs()
	}
	return utilerrors.Join(errs...)
}

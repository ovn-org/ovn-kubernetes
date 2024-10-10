// Package informer provides a wrapper around a client-go SharedIndexInformer
// for event handling. It removes a lot of boilerplate code that is required
// when using workqueues
package informer

import (
	"fmt"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// EventHandler is an event handler that responds to
// Add/Update/Delete events from the informer cache
// Add and Updates use the provided add function
// Deletes use the provided delete function
type EventHandler interface {
	Run(threadiness int, stopChan <-chan struct{}) error
	GetIndexer() cache.Indexer
	Synced() bool
}

type eventHandler struct {
	// name of the handler. used in log messages
	name string
	// informer is the underlying SharedIndexInformer that we're wrapping
	informer cache.SharedIndexInformer
	// deletedIndexer stores copies of objects that have been deleted from
	// the cache. This is needed as the OVN controller Delete functions expect
	// to have a copy of the object that needs deleting
	deletedIndexer cache.Indexer
	// workqueue is the queue we use to store work
	workqueue workqueue.TypedRateLimitingInterface[string]
	// add is the handler function that gets called when something has been added/updated
	add func(obj interface{}) error
	// delete is handler function that gets called when something has been deleted
	delete func(obj interface{}) error
	// updateFilter is the function that we use to evaluate whether an update
	// should be enqueued or not. This is required to avoid an unrelated annotation
	// change triggering hundreds of OVN commands being run
	updateFilter UpdateFilterFunction
}

// UpdateFilterFunction returns true if the update is interesting
type UpdateFilterFunction func(old, new interface{}) bool

// ReceiveAllUpdates always returns true
// meaning that all updates will be enqueued
func ReceiveAllUpdates(old, new interface{}) bool {
	return true
}

// DiscardAllUpdates always returns false, discarding updates
func DiscardAllUpdates(old, new interface{}) bool {
	return false
}

// EventHandlerCreateFunction is function that creates new event handlers
type EventHandlerCreateFunction func(
	name string,
	informer cache.SharedIndexInformer,
	addFunc, deleteFunc func(obj interface{}) error,
	updateFilterFunc UpdateFilterFunction,
) (EventHandler, error)

// NewDefaultEventHandler returns a new default event handler
// The default event handler
// - Enqueue Adds to the workqueue
// - Enqueue Updates to the workqueue if they match the predicate function
// - Enqueue Deletes to the workqueue, and places the deleted object on the deletedIndexer
//
// As the workqueue doesn't carry type information, adds/updates/deletes for a key are all colllapsed.
// We can only differentiate between add/delete by whether an item exists in the cache or not.
// Using this implementation, it's not possible to differentiate between adds and updates
func NewDefaultEventHandler(
	name string,
	informer cache.SharedIndexInformer,
	addFunc, deleteFunc func(obj interface{}) error,
	updateFilterFunc UpdateFilterFunction,
) (EventHandler, error) {
	e := &eventHandler{
		name:           name,
		informer:       informer,
		deletedIndexer: cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, cache.Indexers{}),
		workqueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: name},
		),
		add:          addFunc,
		delete:       deleteFunc,
		updateFilter: updateFilterFunc,
	}
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// always enqueue adds
			e.enqueue(obj)
		},
		UpdateFunc: func(old, new interface{}) {
			oldObj := old.(metav1.Object)
			newObj := new.(metav1.Object)
			if oldObj.GetUID() != newObj.GetUID() {
				// This occurs not so often, so log this occurance.
				klog.Infof("Object %s/%s is replaced, invoking delete followed by add handler", newObj.GetNamespace(), newObj.GetName())
				e.enqueueDelete(oldObj)
				e.enqueue(newObj)
				return
			}
			// Make sure object was actually changed.
			if oldObj.GetResourceVersion() == newObj.GetResourceVersion() {
				return
			}
			// check the update aginst the predicate functions
			if e.updateFilter(old, new) {
				// enqueue if it matches
				e.enqueue(newObj)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// dispatch to enqueueDelete to ensure deleted items are handled properly
			e.enqueueDelete(obj)
		},
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// NewTestEventHandler returns an event handler similar to NewEventHandler.
// The only difference is that it ignores the ResourceVersion check.
func NewTestEventHandler(
	name string,
	informer cache.SharedIndexInformer,
	addFunc, deleteFunc func(obj interface{}) error,
	updateFilterFunc UpdateFilterFunction,
) (EventHandler, error) {
	e := &eventHandler{
		name:           name,
		informer:       informer,
		deletedIndexer: cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, cache.Indexers{}),
		workqueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: name},
		),
		add:          addFunc,
		delete:       deleteFunc,
		updateFilter: updateFilterFunc,
	}
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// always enqueue adds
			e.enqueue(obj)
		},
		UpdateFunc: func(old, new interface{}) {
			oldObj := old.(metav1.Object)
			newObj := new.(metav1.Object)
			if oldObj.GetUID() != newObj.GetUID() {
				// This occurs not so often, so log this occurance.
				klog.Infof("Object %s/%s is replaced, invoking delete followed by add handler", newObj.GetNamespace(), newObj.GetName())
				e.enqueueDelete(oldObj)
				e.enqueue(newObj)
				return
			}
			// check the update aginst the predicate functions
			if e.updateFilter(old, new) {
				// enqueue if it matches
				e.enqueue(newObj)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// dispatch to enqueueDelete to ensure deleted items are handled properly
			e.enqueueDelete(obj)
		},
	})
	if err != nil {
		return nil, err
	}
	return e, nil

}

// GetIndexer returns the indexer that is associated with
// the SharedInformer. This is required for a consumer of
// this wrapper to create Listers
func (e *eventHandler) GetIndexer() cache.Indexer {
	return e.informer.GetIndexer()
}

func (e *eventHandler) Synced() bool {
	return e.informer.HasSynced()
}

// Run starts event processing for the eventHandler.
// It waits for the informer cache to sync before starting the provided number of threads.
// It will block until stopCh is closed, at which point it will shutdown
// the workqueue and wait for workers to finish processing their current work items.
func (e *eventHandler) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting %s informer queue", e.name)

	klog.Infof("Waiting for %s informer caches to sync", e.name)
	// wait for caches to be in sync before we start the workers
	if ok := WaitForCacheSyncWithTimeout(stopCh, e.informer.HasSynced); !ok {
		return fmt.Errorf("failed to wait for %s caches to sync", e.name)
	}

	klog.Infof("Starting %d %s queue workers", threadiness, e.name)
	// start our worker threads
	wg := &sync.WaitGroup{}
	for j := 0; j < threadiness; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(e.runWorker, time.Second, stopCh)
		}()
	}

	klog.Infof("Started %s queue workers", e.name)
	// wait until the channel is closed
	<-stopCh

	klog.Infof("Shutting down %s queue workers", e.name)
	e.workqueue.ShutDown()
	wg.Wait()
	klog.Infof("Shut down %s queue workers", e.name)

	return nil
}

// enqueue adds an item to the workqueue
func (e *eventHandler) enqueue(obj interface{}) {
	// ignore objects that are already set for deletion
	if !obj.(metav1.Object).GetDeletionTimestamp().IsZero() {
		return
	}
	var key string
	var err error
	// get the key for our object
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	// add the key to the workqueue
	e.workqueue.Add(key)
}

// enqueueDelete is a special case of enqueue, where we must
// add the deleted item to the deletedIndexer before we enqueue
func (e *eventHandler) enqueueDelete(obj interface{}) {
	var key string
	var err error
	// get the key for our object, which may have been deleted already
	if key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	// add the object to the deleted indexer
	err = e.deletedIndexer.Add(obj)
	if err != nil {
		utilruntime.HandleError(err)
	}
	// add the key to the workqueue
	e.workqueue.Add(key)
}

// runWorker is invoked by our worker threads
// it executes processNextWorkItem until the queue is shutdown
func (e *eventHandler) runWorker() {
	for e.processNextWorkItem() {
	}
}

// processNextWorkItem processes work items from the queue
func (e *eventHandler) processNextWorkItem() bool {
	// get item from the queue
	key, shutdown := e.workqueue.Get()

	// if we have to shutdown, return now
	if shutdown {
		return false
	}

	// process the item
	err := func(key string) error {
		// make sure we call Done on the object once we've finshed processing
		defer e.workqueue.Done(key)

		// Run the syncHandler, passing it the namespace/name string of the
		// resource to be synced.
		if err := e.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors
			// if it hasn't already been requeued more times than our MaxRetries
			if e.workqueue.NumRequeues(key) < MaxRetries {
				e.workqueue.AddRateLimited(key)
				return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
			}
			// if we've exceeded MaxRetries, remove the item from the queue
			e.workqueue.Forget(key)
			return fmt.Errorf("dropping %s from %s queue as it has failed more than %d times", key, e.name, MaxRetries)
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		e.workqueue.Forget(key)
		klog.Infof("Successfully synced '%s'", key)

		return nil
	}(key)

	// handle any errors that occurred
	if err != nil {
		utilruntime.HandleError(err)
	}

	// work complete!
	return true
}

// syncHandler fetches an item from the informer's cache
// and dispatches to the relevant handler function
func (e *eventHandler) syncHandler(key string) error {
	// get the object from the informer cache
	obj, exists, err := e.informer.GetIndexer().GetByKey(key)
	if err != nil {
		return fmt.Errorf("error fetching object with key %s from store: %v", key, err)
	}
	// if it doesn't exist, it's been deleted
	if !exists {
		// get the deleted object from the deletedIndexer
		// this shouldn't error, or fail to exist but we handle these cases to be thorough
		obj, exists, err := e.deletedIndexer.GetByKey(key)
		if err != nil {
			return fmt.Errorf("error getting object with key %s from deletedIndexer: %v", key, err)
		}
		if !exists {
			return fmt.Errorf("key %s doesn't exist in deletedIndexer: %v", key, err)
		}
		// call the eventHandler's delete function for this object
		if err := e.delete(obj); err != nil {
			return err
		}
		// finally, remove the deleted object from the deletedIndexer
		return e.deletedIndexer.Delete(obj)
	}
	// call the eventHandler's add function in the case of add/update
	return e.add(obj)
}

func WaitForCacheSyncWithTimeout(stopCh <-chan struct{}, cacheSyncs ...cache.InformerSynced) bool {
	return cache.WaitForCacheSync(util.GetChildStopChanWithTimeout(stopCh, types.InformerSyncTimeout), cacheSyncs...)
}

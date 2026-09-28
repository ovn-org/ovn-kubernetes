// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	DefaultMaxAttempts = 15
	InfiniteAttempts   = math.MaxInt
)

// DefaultRateLimiter returns the default rate limiter for controller work
// queues. It uses per-item exponential backoff starting at 5ms and capped at 1
// minute. The delay doubles on each retry (5ms, 10ms, 20ms, 40ms, ...) until
// reaching the cap.
//
// This is similar to the upstream DefaultTypedControllerRateLimiter but with a
// lower max delay (1m vs ~17m) which is more appropriate for networking
// controllers, and without the global token-bucket limiter since we already
// rely on per-item backoff.
func DefaultRateLimiter[T comparable]() workqueue.TypedRateLimiter[T] {
	return workqueue.NewTypedItemExponentialFailureRateLimiter[T](5*time.Millisecond, 1*time.Minute)
}

// Reconciler is a basic level-driven controller that is fed externally of items
// to reconcile through its Reconcile method
type Reconciler interface {
	Reconcile(key string)
	ReconcileRateLimited(key string)
	ReconcileAfter(key string, duration time.Duration)
	addHandler() error
	startWorkers() error
	stop()
}

// Controller is a level-driven controller that is fed of items to reconcile
// through a provided informer
type Controller interface {
	Reconciler
	ReconcileAll()
}

type ReconcilerConfig struct {
	RateLimiter workqueue.TypedRateLimiter[string]
	Reconcile   func(context.Context, string) error
	// How many workers should be started for this reconciler.
	Threadiness int
	// MaxAttempts is the number of times a key will be reconciled before giving
	// up on error. If 0, will use DefaultMaxAttempts. Set to InfiniteAttempts
	// to retry forever.
	MaxAttempts int
	// HandleError when MaxRetries has been reached
	HandleError func(ctx context.Context, key string, err error) error
}

type ControllerConfig[T any] struct {
	RateLimiter workqueue.TypedRateLimiter[string]
	Reconcile   func(context.Context, string) error
	// How many workers should be started for this controller.
	Threadiness int
	// MaxAttempts is the number of times a key will be reconciled before giving
	// up on error. If 0, will use DefaultMaxAttempts. Set to InfiniteAttempts to
	// retry forever.
	MaxAttempts int
	// HandleError when MaxRetries has been reached
	HandleError func(ctx context.Context, key string, err error) error

	Informer cache.SharedIndexInformer
	Lister   func(selector labels.Selector) (ret []*T, err error)
	// ObjNeedsUpdate tells if object should be reconciled.
	// May be called with oldObj = nil on Add, won't be called on Delete.
	ObjNeedsUpdate func(oldObj, newObj *T) bool
	// OnDelete, if non-nil, is invoked synchronously from the informer's delete
	// handler with the last-known object (unwrapped from a
	// cache.DeletedFinalStateUnknown tombstone when the delete was missed and
	// detected on relist). It runs in informer-delivery order, before the
	// object's key is enqueued for Reconcile. This is the only place a consumer
	// can observe a deleted object's content: Reconcile receives just a key, by
	// which point the object is gone from the lister. Use it to react to a
	// deletion whose effect depends on the deleted object's own fields (e.g. to
	// re-queue a related object identified only by a field on the deleted one).
	// It must be fast and non-blocking since it runs on the informer's handler
	// goroutine. Optional; nil preserves existing behavior.
	OnDelete func(obj *T)
}

// controller has the basic functionality, and may have some wrappers to provide
// different Start() method options.
type controller[T any] struct {
	name         string
	config       *ControllerConfig[T]
	eventHandler cache.ResourceEventHandlerRegistration

	queue    workqueue.TypedRateLimitingInterface[string]
	stopChan chan struct{}
	wg       *sync.WaitGroup
}

// NewReconciler creates a new basic level-driven controller. It should be
// started and stopped using Start/StartWithInitialSync/Stop functions.
func NewReconciler(name string, config *ReconcilerConfig) Reconciler {
	controllerConfig := &ControllerConfig[string]{
		RateLimiter: config.RateLimiter,
		Reconcile:   config.Reconcile,
		Threadiness: config.Threadiness,
		MaxAttempts: config.MaxAttempts,
		HandleError: config.HandleError,
	}
	return NewController(name, controllerConfig)
}

// NewController creates a new level-driven controller. It should be started and
// stopped using Start/StartWithInitialSync/Stop functions.
func NewController[T any](name string, config *ControllerConfig[T]) Controller {
	rateLimiter := config.RateLimiter
	if rateLimiter == nil {
		rateLimiter = DefaultRateLimiter[string]()
	}
	c := &controller[T]{
		name:   name,
		config: config,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			rateLimiter,
			workqueue.TypedRateLimitingQueueConfig[string]{
				Name: name,
			},
		),
		stopChan: make(chan struct{}),
		wg:       &sync.WaitGroup{},
	}
	if c.config.MaxAttempts == 0 {
		c.config.MaxAttempts = DefaultMaxAttempts
	}
	if c.config.HandleError == nil {
		c.config.HandleError = func(_ context.Context, _ string, err error) error {
			utilruntime.HandleError(err)
			return nil
		}
	}
	return c
}

func (c *controller[T]) addHandler() error {
	if c.config.Informer == nil {
		return nil
	}

	klog.Infof("Adding controller %v event handlers", c.name)

	var err error
	c.eventHandler, err = c.config.Informer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onAdd,
			UpdateFunc: c.onUpdate,
			DeleteFunc: c.onDelete,
		}))
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	if !util.WaitForInformerCacheSyncWithTimeout(c.name, c.stopChan, c.config.Informer.HasSynced) {
		return fmt.Errorf("timed out waiting for %s informer cache to sync", c.name)
	}
	return nil
}

func (c *controller[T]) startWorkers() error {
	if c.config.Threadiness < 1 {
		return fmt.Errorf("failed to start controller %s: threadiness should be > 0", c.name)
	}

	klog.Infof("Starting controller %v with %v workers", c.name, c.config.Threadiness)

	for i := 0; i < c.config.Threadiness; i++ {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			// this loop will exit once the queue is shut down, because processNextQueueItem will return false
			for c.processNextQueueItem() {
			}
		}()
	}
	return nil
}

func (c *controller[T]) stop() {
	// we assign stopChan to nil to signal that controller was already stopped.
	if c.stopChan == nil {
		return
	}
	close(c.stopChan)
	c.stopChan = nil
	c.cleanup()
	c.wg.Wait()
}

func (c *controller[T]) cleanup() {
	c.queue.ShutDown()
	if c.eventHandler != nil {
		if err := c.config.Informer.RemoveEventHandler(c.eventHandler); err != nil {
			klog.Errorf("Failed to remove event handler for controller %s: %v", c.name, err)
		}
	}
}

func (c *controller[T]) onAdd(objInterface interface{}) {
	newObj, ok := objInterface.(*T)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("controller %s: expecting %T but received %T", c.name, *new(T), newObj))
		return
	}
	if !c.config.ObjNeedsUpdate(nil, newObj) {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("controller %s: couldn't get key for object %+v: %v", c.name, newObj, err))
		return
	}
	c.queue.Add(key)
}

func (c *controller[T]) onUpdate(oldObjInterface, newObjInterface interface{}) {
	oldObj, ok := oldObjInterface.(*T)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("controller %s: expecting %T but received %T", c.name, *new(T), oldObj))
		return
	}
	newObj, ok := newObjInterface.(*T)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("controller %s: expecting %T but received %T", c.name, *new(T), newObj))
		return
	}

	if !c.config.ObjNeedsUpdate(oldObj, newObj) {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("controller %s: couldn't get key for object %+v: %v", c.name, newObj, err))
		return
	}
	c.queue.Add(key)
}

func (c *controller[T]) onDelete(objInterface interface{}) {
	if c.config.OnDelete != nil {
		// OnDelete is an auxiliary, best-effort hook. If the object can't be
		// extracted (deletedObj logged and returned nil), we intentionally do
		// not return: enqueueing the key below is the primary, level-driven
		// behavior and must still run so the deletion gets reconciled.
		if obj := c.deletedObj(objInterface); obj != nil {
			c.config.OnDelete(obj)
		}
	}

	// A deletion that the watch missed (detected later by a relist) is
	// delivered as a DeletedFinalStateUnknown wrapper, not as the object
	// itself. This key func handles both; the plain one errors on the
	// wrapper, which would silently drop the deletion.
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(objInterface)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("controller %s: couldn't get key for object %+v: %v", c.name, objInterface, err))
		return
	}
	c.queue.Add(key)
}

// deletedObj extracts the typed object from a delete event, unwrapping a
// cache.DeletedFinalStateUnknown tombstone (delivered when a delete was missed
// and detected later on relist) to its last-known object. It logs and returns
// nil if the (unwrapped) object is not of the expected type.
func (c *controller[T]) deletedObj(objInterface interface{}) *T {
	obj := objInterface
	if tombstone, isTombstone := objInterface.(cache.DeletedFinalStateUnknown); isTombstone {
		obj = tombstone.Obj
	}
	typed, ok := obj.(*T)
	if !ok {
		// Log and return nil rather than making this fatal: onDelete treats a
		// nil result as "skip the OnDelete hook" but still enqueues the key so
		// the deletion gets reconciled. This branch is defense-in-depth; a
		// correctly-wired typed informer only ever delivers *T (or a tombstone
		// wrapping *T).
		utilruntime.HandleError(fmt.Errorf("controller %s: OnDelete received unexpected type %T", c.name, obj))
		return nil
	}
	return typed
}

// processNextQueueItem returns false when the queue is shutdown and the
// handling should be stopped. Otherwise, it handles the next item from the
// queue and always returns true.
func (c *controller[T]) processNextQueueItem() bool {
	key, shutdown := c.queue.Get()

	if shutdown {
		return false
	}

	defer c.queue.Done(key)

	ctx := context.Background()
	err := c.config.Reconcile(ctx, key)
	if err != nil {
		retry := c.config.MaxAttempts == InfiniteAttempts || c.queue.NumRequeues(key) < c.config.MaxAttempts
		if retry {
			klog.Errorf("Controller %s: error found while processing %s: %v", c.name, key, err)
			c.queue.AddRateLimited(key)
			return true
		}
		klog.Warningf("Controller %s: dropping %s out of the queue: %v", c.name, key, err)
		err = c.config.HandleError(ctx, key, err)
		if err != nil {
			utilruntime.HandleError(err)
		}
	}
	c.queue.Forget(key)
	return true
}

func (c *controller[T]) Reconcile(key string) {
	c.queue.Add(key)
}

func (c *controller[T]) ReconcileRateLimited(key string) {
	c.queue.AddRateLimited(key)
}

func (c *controller[T]) ReconcileAfter(key string, duration time.Duration) {
	c.queue.AddAfter(key, duration)
}

func (c *controller[T]) ReconcileAll() {
	if c.config.Lister == nil {
		panic("ReconcileAll needs a Lister that was not provided")
	}

	klog.Infof("Controller %s: full reconcile", c.name)
	objs, err := c.config.Lister(labels.Everything())
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("controller %s: couldn't list objects of type %T on ReconcileAll: %v", c.name, *new(T), err))
		return
	}
	for _, obj := range objs {
		key, err := cache.MetaNamespaceKeyFunc(obj)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("controller %s: couldn't get key for object %+v: %v", c.name, obj, err))
			return
		}
		c.queue.Add(key)
	}
}

// Start starts one or multiple controllers. If initial sync is required, use
// StartWithInitialSync
func Start(controllers ...Reconciler) (err error) {
	return StartWithInitialSync(nil, controllers...)
}

// StartWithInitialSync starts one or multiple controllers that share
// initialSync function. initialSync will be called after all event handlers for
// given controllers were added to the informer (if any), but before queue
// handlers are started. It ensures no events will be missed. A user of this
// package that manages multiple controllers would most likely do a single call
// to this function if it needs to use this sync to take an initial action (i.e.
// building up its own cache) correlating the information from all the
// initialized informers. If initial sync is not required, use Start.
func StartWithInitialSync(initialSync func() error, controllers ...Reconciler) (err error) {
	defer func() {
		if err != nil {
			Stop(controllers...)
		}
	}()

	g := new(errgroup.Group)
	for _, controller := range controllers {
		controller := controller
		g.Go(func() error {
			return controller.addHandler()
		})
	}
	if err = g.Wait(); err != nil {
		return err
	}

	// now we have already started receiving events and putting keys in the
	// queue. If initial sync is needed, we can do it now, by listing all
	// existing objects. Since we are receiving all events already, we know
	// every change that happens after the following List call, will be handled.
	if initialSync != nil {
		if err = initialSync(); err != nil {
			return fmt.Errorf("initial sync failed: %w", err)
		}
	}
	for _, controller := range controllers {
		if err = controller.startWorkers(); err != nil {
			return err
		}
	}
	return nil
}

func Stop(controllers ...Reconciler) {
	for _, subcontroller := range controllers {
		subcontroller.stop()
	}
}

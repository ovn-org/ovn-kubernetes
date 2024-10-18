package factory

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cryptorand"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"

	ipamclaimslister "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/listers/ipamclaims/v1alpha1"
	multinetworkpolicylister "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/listers/k8s.cni.cncf.io/v1beta1"
	networkattachmentdefinitionlister "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	egressfirewalllister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	egressqoslister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/listers/egressqos/v1"
	egressservicelister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/listers/egressservice/v1"

	cloudprivateipconfiglister "github.com/openshift/client-go/cloudnetwork/listers/cloudnetwork/v1"
	egressiplister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	networkqoslister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1/apis/listers/networkqos/v1"
	anplister "sigs.k8s.io/network-policy-api/pkg/client/listers/apis/v1alpha1"

	userdefinednetworklister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	listers "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	netlisters "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// Handler represents an event handler and is private to the factory module
type Handler struct {
	base cache.FilteringResourceEventHandler

	id uint64
	// tombstone is used to track the handler's lifetime. handlerAlive
	// indicates the handler can be called, while handlerDead indicates
	// it has been scheduled for removal and should not be called.
	// tombstone should only be set using atomic operations since it is
	// used from multiple goroutines.
	tombstone uint32
	// priority is used to track the handler's priority of being invoked.
	// example: a handler with priority 0 will process the received event first
	// before a handler with priority 1.
	priority int
}

func (h *Handler) OnAdd(obj interface{}, isInInitialList bool) {
	if atomic.LoadUint32(&h.tombstone) == handlerAlive {
		h.base.OnAdd(obj, isInInitialList)
	}
}

func (h *Handler) OnUpdate(oldObj, newObj interface{}) {
	if atomic.LoadUint32(&h.tombstone) == handlerAlive {
		h.base.OnUpdate(oldObj, newObj)
	}
}

func (h *Handler) OnDelete(obj interface{}) {
	if atomic.LoadUint32(&h.tombstone) == handlerAlive {
		h.base.OnDelete(obj)
	}
}

func (h *Handler) kill() bool {
	return atomic.CompareAndSwapUint32(&h.tombstone, handlerAlive, handlerDead)
}

type event struct {
	obj     interface{}
	oldObj  interface{}
	process func(*event)
}

type listerInterface interface{}

type initialAddFn func(*Handler, []interface{})

type queueMap struct {
	sync.Mutex
	entries  map[ktypes.NamespacedName]*queueMapEntry
	queues   []chan *event
	wg       *sync.WaitGroup
	stopChan chan struct{}
}

type queueMapEntry struct {
	queue    uint32
	refcount int32
}

type informer struct {
	sync.RWMutex
	oType reflect.Type
	inf   cache.SharedIndexInformer
	// keyed by priority - used to track the handler's priority of being invoked.
	// example: a handler with priority 0 will process the received event first
	// before a handler with priority 1, 0 being the higest priority.
	// NOTE: we can have multiple handlers with the same priority hence the value
	// is a map of handlers keyed by its unique id.
	handlers map[int]map[uint64]*Handler
	lister   listerInterface
	// initialAddFunc will be called to deliver the initial list of objects
	// when a handler is added
	initialAddFunc initialAddFn
	shutdownWg     sync.WaitGroup

	// queueMap handles distributing events across a queued handler's queues
	queueMap *queueMap
}

func (i *informer) forEachQueuedHandler(f func(h *Handler)) {
	i.RLock()
	defer i.RUnlock()

	for priority := 0; priority <= minHandlerPriority; priority++ { // loop over priority higest to lowest
		for _, handler := range i.handlers[priority] {
			f(handler)
		}
	}
}

func (i *informer) forEachQueuedHandlerReversed(f func(h *Handler)) {
	i.RLock()
	defer i.RUnlock()

	for priority := minHandlerPriority; priority >= 0; priority-- { // loop over priority lowest to highest
		for _, handler := range i.handlers[priority] {
			f(handler)
		}
	}
}

func (i *informer) forEachHandler(obj interface{}, f func(h *Handler)) {
	i.RLock()
	defer i.RUnlock()

	objType := reflect.TypeOf(obj)
	if objType != i.oType {
		klog.Errorf("Object type %v did not match expected %v", objType, i.oType)
		return
	}

	for priority := 0; priority <= minHandlerPriority; priority++ { // loop over priority higest to lowest
		for _, handler := range i.handlers[priority] {
			f(handler)
		}
	}
}

func (i *informer) forEachHandlerReversed(obj interface{}, f func(h *Handler)) {
	i.RLock()
	defer i.RUnlock()

	objType := reflect.TypeOf(obj)
	if objType != i.oType {
		klog.Errorf("Object type %v did not match expected %v", objType, i.oType)
		return
	}

	for priority := minHandlerPriority; priority >= 0; priority-- { // loop over priority lowest to highest
		for _, handler := range i.handlers[priority] {
			f(handler)
		}
	}
}

func (i *informer) addHandler(id uint64, priority int, filterFunc func(obj interface{}) bool, funcs cache.ResourceEventHandler, existingItems []interface{}) *Handler {
	handler := &Handler{
		cache.FilteringResourceEventHandler{
			FilterFunc: filterFunc,
			Handler:    funcs,
		},
		id,
		handlerAlive,
		priority,
	}

	// Send existing items to the handler's add function; informers usually
	// do this but since we share informers, it's long-since happened so
	// we must emulate that here
	i.initialAddFunc(handler, existingItems)

	_, ok := i.handlers[priority]
	if !ok {
		i.handlers[priority] = make(map[uint64]*Handler)
	}
	i.handlers[priority][id] = handler

	return handler
}

func (i *informer) removeHandler(handler *Handler) {
	if !handler.kill() {
		klog.Errorf("Removing already-removed %v event handler %d", i.oType, handler.id)
		return
	}

	klog.V(5).Infof("Sending %v event handler %d for removal", i.oType, handler.id)

	go func() {
		i.Lock()
		defer i.Unlock()
		removed := 0
		for priority := range i.handlers { // loop over priority
			if _, ok := i.handlers[priority]; !ok {
				continue // protection against nil map as value
			}
			if _, ok := i.handlers[priority][handler.id]; ok {
				// Remove the handler
				delete(i.handlers[priority], handler.id)
				removed = 1
				klog.V(5).Infof("Removed %v event handler %d", i.oType, handler.id)
			}
		}
		if removed == 0 {
			klog.Warningf("Tried to remove unknown object type %v event handler %d", i.oType, handler.id)
		}
	}()
}

func newQueueMap(numEventQueues uint32, wg *sync.WaitGroup, stopChan chan struct{}) *queueMap {
	qm := &queueMap{
		entries:  make(map[ktypes.NamespacedName]*queueMapEntry),
		queues:   make([]chan *event, numEventQueues),
		wg:       wg,
		stopChan: stopChan,
	}
	for j := 0; j < int(numEventQueues); j++ {
		qm.queues[j] = make(chan *event, 10)
	}
	return qm
}

func (qm *queueMap) processEvents(queue chan *event) {
	defer qm.wg.Done()
	for {
		select {
		case e, ok := <-queue:
			if !ok {
				return
			}
			e.process(e)
		case <-qm.stopChan:
			return
		}
	}
}

func (qm *queueMap) start() {
	qm.wg.Add(len(qm.queues))
	for _, q := range qm.queues {
		go qm.processEvents(q)
	}
}

func (qm *queueMap) shutdown() {
	// Close all the event channels
	for _, q := range qm.queues {
		close(q)
	}
}

// getNewQueueNum finds and returns the index of the queue with the lowest
// number of items
func (qm *queueMap) getNewQueueNum() uint32 {
	var j, startIdx, queueIdx uint32
	numEventQueues := uint32(len(qm.queues))
	startIdx = uint32(cryptorand.Intn(int64(numEventQueues - 1)))
	queueIdx = startIdx
	lowestNum := len(qm.queues[startIdx])
	for j = 0; j < numEventQueues; j++ {
		tryQueue := (startIdx + j) % numEventQueues
		num := len(qm.queues[tryQueue])
		if num < lowestNum {
			lowestNum = num
			queueIdx = tryQueue
		}
	}
	return queueIdx
}

// getQueueMapEntry creates or returns an existing entry for the given object's
// NamespacedName. This entry tracks the queue number for that NamespacedName
// so that all objects with the NamespacedName are serialized into the same
// queue slot. This prevents parallel processing of events for the same object
// that might happen out-of-order.
//
// If there is no entry for the NamespacedName a new one is created and assigned
// a queue slot with the least number of items (to attempt to balance queue
// length).
//
// If an existing entry exists it will be returned and the already-assigned
// queue slot will be used to ensure serialization.
func (qm *queueMap) getQueueMapEntry(oType reflect.Type, obj interface{}) (ktypes.NamespacedName, *queueMapEntry) {
	meta, err := getObjectMeta(oType, obj)
	if err != nil {
		klog.Errorf("Object has no meta: %v", err)
		return ktypes.NamespacedName{}, nil
	}

	namespacedName := ktypes.NamespacedName{Namespace: meta.Namespace, Name: meta.Name}

	qm.Lock()
	defer qm.Unlock()

	entry, ok := qm.entries[namespacedName]
	if ok {
		if atomic.AddInt32(&entry.refcount, 1) == 1 {
			// Entry is unused because add/update operations completed
			// but we haven't seen a delete yet. Assign new queue to
			// ensure queue balance.
			entry.queue = qm.getNewQueueNum()
		}
	} else {
		// no entry found, assign new queue
		entry = &queueMapEntry{
			refcount: 1,
			queue:    qm.getNewQueueNum(),
		}
		qm.entries[namespacedName] = entry
	}
	return namespacedName, entry
}

// releaseQueueMapEntry is called when an event has finished processing. It
// decreases the reference count on the queue map entry and if that entry
// is less-than-or-equal-to-zero (meaning there are no in-flight events for the
// object) removes it from the entries map. The next event for the given
// NamespacedName will be rebalanced to a new queue slot.
func (qm *queueMap) releaseQueueMapEntry(key ktypes.NamespacedName, entry *queueMapEntry, del bool) {
	if entry == nil {
		return
	}

	// To reduce lock contention don't bother grabbing the lock for
	// add/update operations which are quite frequent. We'll eventually
	// get a delete for the object and remove it from the queue map.
	if !del {
		atomic.AddInt32(&entry.refcount, -1)
		return
	}

	qm.Lock()
	defer qm.Unlock()
	if atomic.AddInt32(&entry.refcount, -1) <= 0 {
		delete(qm.entries, key)
	}
}

// enqueueEvent adds an event to the appropriate queue for the object
func (qm *queueMap) enqueueEvent(oldObj, obj interface{}, oType reflect.Type, isDel bool, processFunc func(*event)) {
	key, entry := qm.getQueueMapEntry(oType, obj)
	event := &event{
		obj:    obj,
		oldObj: oldObj,
		process: func(e *event) {
			processFunc(e)
			qm.releaseQueueMapEntry(key, entry, isDel)
		},
	}
	select {
	case qm.queues[entry.queue] <- event:
	case <-qm.stopChan:
		return
	}
}

func ensureObjectOnDelete(obj interface{}, expectedType reflect.Type) (interface{}, error) {
	if expectedType == reflect.TypeOf(obj) {
		return obj, nil
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil, fmt.Errorf("couldn't get object from tombstone: %+v", obj)
	}
	obj = tombstone.Obj
	objType := reflect.TypeOf(obj)
	if expectedType != objType {
		return nil, fmt.Errorf("expected tombstone object resource type %v but got %v", expectedType, objType)
	}
	return obj, nil
}

func (i *informer) newFederatedQueuedHandler(numEventQueues uint32) cache.ResourceEventHandlerFuncs {
	name := i.oType.Elem().Name()
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			i.queueMap.enqueueEvent(nil, obj, i.oType, false, func(e *event) {
				metrics.MetricResourceUpdateCount.WithLabelValues(name, "add").Inc()
				start := time.Now()
				i.forEachQueuedHandler(func(h *Handler) {
					h.OnAdd(e.obj, false)
				})
				metrics.MetricResourceAddLatency.Observe(time.Since(start).Seconds())
			})
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			i.queueMap.enqueueEvent(oldObj, newObj, i.oType, false, func(e *event) {
				metrics.MetricResourceUpdateCount.WithLabelValues(name, "update").Inc()
				start := time.Now()
				i.forEachQueuedHandler(func(h *Handler) {
					old := oldObj.(metav1.Object)
					new := newObj.(metav1.Object)
					if old.GetUID() != new.GetUID() {
						// This occurs not so often, so log this occurance.
						klog.Infof("Object %s/%s is replaced, invoking delete followed by add handler", new.GetNamespace(), new.GetName())
						h.OnDelete(e.oldObj)
						h.OnAdd(e.obj, false)
					} else {
						h.OnUpdate(e.oldObj, e.obj)
					}
				})
				metrics.MetricResourceUpdateLatency.Observe(time.Since(start).Seconds())
			})
		},
		DeleteFunc: func(obj interface{}) {
			realObj, err := ensureObjectOnDelete(obj, i.oType)
			if err != nil {
				klog.Errorf(err.Error())
				return
			}
			i.queueMap.enqueueEvent(nil, realObj, i.oType, true, func(e *event) {
				metrics.MetricResourceUpdateCount.WithLabelValues(name, "delete").Inc()
				start := time.Now()
				i.forEachQueuedHandlerReversed(func(h *Handler) {
					h.OnDelete(e.obj)
				})
				metrics.MetricResourceDeleteLatency.Observe(time.Since(start).Seconds())
			})
		},
	}
}

func (i *informer) newFederatedHandler() cache.ResourceEventHandlerFuncs {
	name := i.oType.Elem().Name()
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			metrics.MetricResourceUpdateCount.WithLabelValues(name, "add").Inc()
			start := time.Now()
			i.forEachHandler(obj, func(h *Handler) {
				h.OnAdd(obj, false)
			})
			metrics.MetricResourceAddLatency.Observe(time.Since(start).Seconds())
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			metrics.MetricResourceUpdateCount.WithLabelValues(name, "update").Inc()
			start := time.Now()
			i.forEachHandler(newObj, func(h *Handler) {
				old := oldObj.(metav1.Object)
				new := newObj.(metav1.Object)
				if old.GetUID() != new.GetUID() {
					// This occurs not so often, so log this occurance.
					klog.Infof("Object %s/%s is replaced, invoking delete followed by add handler", new.GetNamespace(), new.GetName())
					h.OnDelete(oldObj)
					h.OnAdd(newObj, false)
				} else {
					h.OnUpdate(oldObj, newObj)
				}
			})
			metrics.MetricResourceUpdateLatency.Observe(time.Since(start).Seconds())
		},
		DeleteFunc: func(obj interface{}) {
			realObj, err := ensureObjectOnDelete(obj, i.oType)
			if err != nil {
				klog.Errorf(err.Error())
				return
			}
			metrics.MetricResourceUpdateCount.WithLabelValues(name, "delete").Inc()
			start := time.Now()
			i.forEachHandlerReversed(realObj, func(h *Handler) {
				h.OnDelete(realObj)
			})
			metrics.MetricResourceDeleteLatency.Observe(time.Since(start).Seconds())
		},
	}
}

func (i *informer) removeAllHandlers() {
	i.Lock()
	defer i.Unlock()
	for _, handlers := range i.handlers {
		for _, handler := range handlers {
			i.removeHandler(handler)
		}
	}
}

func (i *informer) shutdown() {
	i.removeAllHandlers()

	// Wait for all event processors to finish
	i.shutdownWg.Wait()
}

func newInformerLister(oType reflect.Type, sharedInformer cache.SharedIndexInformer) (listerInterface, error) {
	switch oType {
	case PodType:
		return listers.NewPodLister(sharedInformer.GetIndexer()), nil
	case ServiceType:
		return listers.NewServiceLister(sharedInformer.GetIndexer()), nil
	case NamespaceType:
		return listers.NewNamespaceLister(sharedInformer.GetIndexer()), nil
	case NodeType:
		return listers.NewNodeLister(sharedInformer.GetIndexer()), nil
	case PolicyType:
		return netlisters.NewNetworkPolicyLister(sharedInformer.GetIndexer()), nil
	case EgressFirewallType:
		return egressfirewalllister.NewEgressFirewallLister(sharedInformer.GetIndexer()), nil
	case AdminNetworkPolicyType:
		return anplister.NewAdminNetworkPolicyLister(sharedInformer.GetIndexer()), nil
	case BaselineAdminNetworkPolicyType:
		return anplister.NewBaselineAdminNetworkPolicyLister(sharedInformer.GetIndexer()), nil
	case EgressIPType:
		return egressiplister.NewEgressIPLister(sharedInformer.GetIndexer()), nil
	case CloudPrivateIPConfigType:
		return cloudprivateipconfiglister.NewCloudPrivateIPConfigLister(sharedInformer.GetIndexer()), nil
	case EndpointSliceType:
		return discoverylisters.NewEndpointSliceLister(sharedInformer.GetIndexer()), nil
	case EgressQoSType:
		return egressqoslister.NewEgressQoSLister(sharedInformer.GetIndexer()), nil
	case NetworkAttachmentDefinitionType:
		return networkattachmentdefinitionlister.NewNetworkAttachmentDefinitionLister(sharedInformer.GetIndexer()), nil
	case MultiNetworkPolicyType:
		return multinetworkpolicylister.NewMultiNetworkPolicyLister(sharedInformer.GetIndexer()), nil
	case EgressServiceType:
		return egressservicelister.NewEgressServiceLister(sharedInformer.GetIndexer()), nil
	case IPAMClaimsType:
		return ipamclaimslister.NewIPAMClaimLister(sharedInformer.GetIndexer()), nil
	case UserDefinedNetworkType:
		return userdefinednetworklister.NewUserDefinedNetworkLister(sharedInformer.GetIndexer()), nil
	case NetworkQoSType:
		return networkqoslister.NewNetworkQoSLister(sharedInformer.GetIndexer()), nil
	}

	return nil, fmt.Errorf("cannot create lister from type %v", oType)
}

func newBaseInformer(oType reflect.Type, sharedInformer cache.SharedIndexInformer) (*informer, error) {
	lister, err := newInformerLister(oType, sharedInformer)
	if err != nil {
		klog.Errorf(err.Error())
		return nil, err
	}

	return &informer{
		oType:    oType,
		inf:      sharedInformer,
		lister:   lister,
		handlers: make(map[int]map[uint64]*Handler),
	}, nil
}

func newInformer(oType reflect.Type, sharedInformer cache.SharedIndexInformer) (*informer, error) {
	i, err := newBaseInformer(oType, sharedInformer)
	if err != nil {
		return nil, err
	}
	i.initialAddFunc = func(h *Handler, items []interface{}) {
		for _, item := range items {
			h.OnAdd(item, false)
		}
	}
	_, err = i.inf.AddEventHandler(i.newFederatedHandler())
	if err != nil {
		return nil, err
	}
	return i, nil

}

func newQueuedInformer(oType reflect.Type, sharedInformer cache.SharedIndexInformer,
	stopChan chan struct{}, numEventQueues uint32) (*informer, error) {
	i, err := newBaseInformer(oType, sharedInformer)
	if err != nil {
		return nil, err
	}
	i.queueMap = newQueueMap(numEventQueues, &i.shutdownWg, stopChan)
	i.queueMap.start()

	i.initialAddFunc = func(h *Handler, items []interface{}) {
		// Make a handler-specific channel array across which the
		// initial add events will be distributed. When a new handler
		// is added, only that handler should receive events for all
		// existing objects.
		addsWg := &sync.WaitGroup{}
		addsMap := newQueueMap(numEventQueues, addsWg, stopChan)
		addsMap.start()

		// Distribute the existing items into the handler-specific
		// channel array.
		for _, obj := range items {
			addsMap.enqueueEvent(nil, obj, i.oType, false, func(e *event) {
				h.OnAdd(e.obj, false)
			})
		}

		// Wait until all the object additions have been processed
		addsMap.shutdown()
		addsWg.Wait()
	}

	_, err = i.inf.AddEventHandler(i.newFederatedQueuedHandler(numEventQueues))
	if err != nil {
		return nil, err
	}

	return i, nil

}

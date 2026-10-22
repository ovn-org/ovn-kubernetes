// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	ipamclaimslister "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/listers/ipamclaims/v1alpha1"
	multinetworkpolicylister "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/listers/k8s.cni.cncf.io/v1beta1"
	networkattachmentdefinitionlister "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	cloudprivateipconfiglister "github.com/openshift/client-go/cloudnetwork/listers/cloudnetwork/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	listers "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	netlisters "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	anplister "sigs.k8s.io/network-policy-api/pkg/client/listers/apis/v1alpha1"

	networkconnectlister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/listers/clusternetworkconnect/v1"
	egressfirewalllister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	egressiplister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	egressqoslister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/listers/egressqos/v1"
	egressservicelister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/listers/egressservice/v1"
	networkqoslister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/listers/networkqos/v1alpha1"
	uplinklister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	userdefinednetworklister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
)

// Use a pool of internal informers to allow multiplexing of events
// between multiple internal informers.  This reduces lock contention
// when adding/removing event handlers by distributing them between
// internal informers.
const internalInformerPoolSize int = 201

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

	// indicates which informer.internalInformers index to use
	// clients are distributed between internal informers
	internalInformerIndex int
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

func (h *Handler) FilterFunc(obj interface{}) bool {
	return h.base.FilterFunc(obj)
}

func (h *Handler) kill() bool {
	return atomic.CompareAndSwapUint32(&h.tombstone, handlerAlive, handlerDead)
}

// event preserves the objects and callback needed to deliver a queued informer notification.
type event struct {
	obj              interface{}
	oldObj           interface{}
	process          func(*event)
	eventType        eventType
	filterTransition bool
}

type eventType uint8

const (
	eventTypeAdd eventType = iota
	eventTypeUpdate
	eventTypeDelete
)

type listerInterface interface{}

type initialAddFn func(*Handler, []interface{})

// queueMap dispatches callbacks by resource key while preserving per-key ordering.
type queueMap struct {
	sync.Mutex
	entries    map[ktypes.NamespacedName]*queueMapEntry
	queue      workqueue.TypedInterface[ktypes.NamespacedName]
	numWorkers int
	stopped    bool
	wg         *sync.WaitGroup
	stopChan   chan struct{}
}

// queueMapEntry holds callbacks for one object key until its worker delivers them.
type queueMapEntry struct {
	pending []*event
}

type internalInformer struct {
	sync.RWMutex
	oType reflect.Type
	// keyed by priority - used to track the handler's priority of being invoked.
	// example: a handler with priority 0 will process the received event first
	// before a handler with priority 1, 0 being the highest priority.
	// NOTE: we can have multiple handlers with the same priority hence the value
	// is a map of handlers keyed by its unique id.
	handlers map[int]map[uint64]*Handler
	// coalescingFilters tracks handlers while they are registered or being
	// removed. Its lock is held through update enqueueing so membership checks
	// cannot be separated from their queued events.
	coalescingFiltersMu sync.RWMutex
	coalescingFilters   map[uint64]func(interface{}) bool
	// queueMap serializes and dispatches queued events by object key.
	queueMap *queueMap
	// hasHandlers is an atomic used to determine if this internal informer actually has handlers attached to it or not
	hasHandlers uint32
}

type informer struct {
	oType  reflect.Type
	inf    cache.SharedIndexInformer
	lister listerInterface
	// initialAddFunc will be called to deliver the initial list of objects
	// when a handler is added
	initialAddFunc initialAddFn
	shutdownWg     sync.WaitGroup

	internalInformers []*internalInformer
}

// forEachQueuedHandler invokes f in ascending priority order while holding the read lock.
func (inf *internalInformer) forEachQueuedHandler(f func(h *Handler)) {
	inf.RLock()
	defer inf.RUnlock()
	for priority := 0; priority <= minHandlerPriority; priority++ { // loop over priority highest to lowest
		for _, handler := range inf.handlers[priority] {
			f(handler)
		}
	}
}

// hasFilterTransition reports whether an update changes membership for a live or registering handler.
// The caller must hold coalescingFiltersMu.RLock through queueing the event.
func (inf *internalInformer) hasFilterTransition(oldObj, newObj interface{}) bool {
	for _, filter := range inf.coalescingFilters {
		if filter(oldObj) != filter(newObj) {
			return true
		}
	}
	return false
}

// addCoalescingFilter registers a handler's filter before its initial store snapshot is taken.
func (inf *internalInformer) addCoalescingFilter(id uint64, filter func(interface{}) bool) {
	inf.coalescingFiltersMu.Lock()
	defer inf.coalescingFiltersMu.Unlock()
	if inf.coalescingFilters == nil {
		inf.coalescingFilters = make(map[uint64]func(interface{}) bool)
	}
	inf.coalescingFilters[id] = filter
}

// removeCoalescingFilter removes a filter after its handler is removed from dispatch.
func (inf *internalInformer) removeCoalescingFilter(id uint64) {
	inf.coalescingFiltersMu.Lock()
	defer inf.coalescingFiltersMu.Unlock()
	delete(inf.coalescingFilters, id)
}

func (inf *internalInformer) forEachQueuedHandlerReversed(f func(h *Handler)) {
	inf.RLock()
	defer inf.RUnlock()

	for priority := minHandlerPriority; priority >= 0; priority-- { // loop over priority lowest to highest
		for _, handler := range inf.handlers[priority] {
			f(handler)
		}
	}
}

// addHandler delivers existing items and then registers the handler for queued events.
// The caller must hold the selected internal informer's write lock.
func (i *informer) addHandler(internalInformerIndex int, id uint64, priority int, filterFunc func(obj interface{}) bool, funcs cache.ResourceEventHandler, existingItems []interface{}) *Handler {
	handler := &Handler{
		cache.FilteringResourceEventHandler{
			FilterFunc: filterFunc,
			Handler:    funcs,
		},
		id,
		handlerAlive,
		priority,
		internalInformerIndex,
	}

	// Send existing items to the handler's add function; informers usually
	// do this but since we share informers, it's long-since happened so
	// we must emulate that here
	i.initialAddFunc(handler, existingItems)

	intInf := i.internalInformers[internalInformerIndex]

	_, ok := intInf.handlers[priority]
	if !ok {
		intInf.handlers[priority] = make(map[uint64]*Handler)
	}
	intInf.handlers[priority][id] = handler

	return handler
}

// removeHandler tombstones a handler immediately, then removes it from the map asynchronously.
func (i *informer) removeHandler(handler *Handler) {
	if !handler.kill() {
		klog.Errorf("Removing already-removed %v event handler %d", i.oType, handler.id)
		return
	}

	klog.V(5).Infof("Sending %v event handler %d for removal", i.oType, handler.id)

	go func() {
		intInf := i.internalInformers[handler.internalInformerIndex]

		intInf.Lock()
		defer intInf.Unlock()
		removed := false
		// track overall how many handlers this internal informer has
		numHandlers := 0
		for priority := range intInf.handlers { // loop over priority
			if _, ok := intInf.handlers[priority]; !ok {
				continue // protection against nil map as value
			}
			if _, ok := intInf.handlers[priority][handler.id]; ok {
				// Remove the handler
				delete(intInf.handlers[priority], handler.id)
				removed = true
				klog.V(5).Infof("Removed %v event handler %d", i.oType, handler.id)
			}
			numHandlers += len(intInf.handlers[priority])
		}
		if removed {
			intInf.removeCoalescingFilter(handler.id)
		}

		// if this internal informer has no handlers, update the atomic
		if numHandlers == 0 {
			atomic.StoreUint32(&intInf.hasHandlers, hasNoHandler)
		}

		if !removed {
			klog.Warningf("Tried to remove unknown object type %v event handler %d", i.oType, handler.id)
		}
	}()
}

// newQueueMap creates a keyed workqueue with the configured callback parallelism.
func newQueueMap(_ uint32, numWorkers uint32, wg *sync.WaitGroup, stopChan chan struct{}) *queueMap {
	if numWorkers == 0 {
		numWorkers = 1
	}
	return &queueMap{
		entries:    make(map[ktypes.NamespacedName]*queueMapEntry),
		queue:      workqueue.NewTyped[ktypes.NamespacedName](),
		numWorkers: int(numWorkers),
		wg:         wg,
		stopChan:   stopChan,
	}
}

// processEvents delivers one callback for a key per queue turn. The workqueue
// prevents another worker from handling that key until Done, while requeueing
// dirty keys at the tail lets other resources make progress.
func (qm *queueMap) processEvents() {
	defer qm.wg.Done()
	for {
		key, shutdown := qm.queue.Get()
		if shutdown {
			return
		}

		for {
			qm.Lock()
			entry := qm.entries[key]
			if entry == nil || len(entry.pending) == 0 {
				delete(qm.entries, key)
				qm.Unlock()
				qm.queue.Done(key)
				break
			}
			event := entry.pending[0]
			entry.pending[0] = nil
			entry.pending = entry.pending[1:]
			qm.Unlock()

			event.process(event)

			qm.Lock()
			entry = qm.entries[key]
			if entry == nil || len(entry.pending) == 0 {
				delete(qm.entries, key)
				qm.Unlock()
				qm.queue.Done(key)
				break
			}
			if !qm.stopped {
				qm.queue.Add(key)
				qm.Unlock()
				qm.queue.Done(key)
				break
			}
			// Shutdown rejects new events before it shuts down the workqueue.
			// Drain this key here so accepted callbacks are not lost in between.
			qm.Unlock()
		}
	}
}

// start launches enough workers to preserve the watch factory's callback parallelism.
func (qm *queueMap) start() {
	qm.wg.Add(qm.numWorkers)
	for i := 0; i < qm.numWorkers; i++ {
		go qm.processEvents()
	}
}

// shutdown rejects new events and drains events accepted before shutdown.
func (qm *queueMap) shutdown() {
	qm.Lock()
	qm.stopped = true
	qm.Unlock()
	qm.queue.ShutDown()
}

// getQueueMapEntry returns the NamespacedName for the given object after
// validating that its metadata can be read.
func (qm *queueMap) getQueueMapEntry(oType reflect.Type, obj interface{}) (ktypes.NamespacedName, bool) {
	meta, err := getObjectMeta(oType, obj)
	if err != nil {
		klog.Errorf("Object has no meta: %v", err)
		return ktypes.NamespacedName{}, false
	}

	namespacedName := ktypes.NamespacedName{Namespace: meta.Namespace, Name: meta.Name}
	return namespacedName, true
}

// isUIDReplacement reports whether an update changes object identity.
func isUIDReplacement(e *event) bool {
	oldObject, oldOK := e.oldObj.(metav1.Object)
	newObject, newOK := e.obj.(metav1.Object)
	return !oldOK || !newOK || oldObject.GetUID() != newObject.GetUID()
}

// coalesceEvent merges only consecutive updates with no filter boundary or UID replacement.
// Lifecycle events stay in the pending sequence so their callbacks cannot be lost.
func coalesceEvent(entry *queueMapEntry, incoming *event) bool {
	if len(entry.pending) == 0 {
		return false
	}

	last := entry.pending[len(entry.pending)-1]
	if last.eventType != eventTypeUpdate || incoming.eventType != eventTypeUpdate ||
		last.filterTransition || incoming.filterTransition || isUIDReplacement(incoming) {
		return false
	}
	last.obj = incoming.obj
	return true
}

// forgetDeletedObject removes an idle deleted object's queue mapping even when
// a slot has no subscribers. The workqueue still serializes any recreated key
// behind a callback already in progress.
func (qm *queueMap) forgetDeletedObject(oType reflect.Type, obj interface{}) {
	meta, err := getObjectMeta(oType, obj)
	if err != nil {
		klog.Errorf("Object has no meta: %v", err)
		return
	}
	key := ktypes.NamespacedName{Namespace: meta.Namespace, Name: meta.Name}
	qm.Lock()
	defer qm.Unlock()
	if entry := qm.entries[key]; entry != nil && len(entry.pending) == 0 {
		delete(qm.entries, key)
	}
}

// enqueueEvent adds or coalesces an event on the object's key queue.
func (qm *queueMap) enqueueEvent(oldObj, obj interface{}, oType reflect.Type, isDel bool, processFunc func(*event)) {
	qm.enqueueEventWithFilterTransition(oldObj, obj, oType, isDel, nil, processFunc)
}

// enqueueEventWithFilterTransition queues an event while preserving filter-boundary updates from coalescing.
// filterTransitionFunc is called under qm.Lock only when adjacent updates could otherwise coalesce. Callers must
// hold any lock that protects the filter state until this method returns.
func (qm *queueMap) enqueueEventWithFilterTransition(oldObj, obj interface{}, oType reflect.Type, isDel bool, filterTransitionFunc func(interface{}, interface{}) bool, processFunc func(*event)) bool {
	select {
	case <-qm.stopChan:
		return false
	default:
	}

	key, ok := qm.getQueueMapEntry(oType, obj)
	if !ok {
		return false
	}
	eventType := eventTypeUpdate
	if isDel {
		eventType = eventTypeDelete
	} else if oldObj == nil {
		eventType = eventTypeAdd
	}
	event := &event{
		obj:       obj,
		oldObj:    oldObj,
		process:   processFunc,
		eventType: eventType,
	}

	qm.Lock()
	defer qm.Unlock()
	if qm.stopped {
		return false
	}
	select {
	case <-qm.stopChan:
		return false
	default:
	}

	entry, ok := qm.entries[key]
	if !ok {
		entry = &queueMapEntry{}
		qm.entries[key] = entry
	}
	if len(entry.pending) != 0 {
		last := entry.pending[len(entry.pending)-1]
		if filterTransitionFunc != nil && last.eventType == eventTypeUpdate && event.eventType == eventTypeUpdate &&
			!last.filterTransition && !isUIDReplacement(event) {
			last.filterTransition = filterTransitionFunc(last.oldObj, last.obj)
			if !last.filterTransition {
				event.filterTransition = filterTransitionFunc(event.oldObj, event.obj)
			}
		}
	}
	coalesced := coalesceEvent(entry, event)
	if !coalesced {
		entry.pending = append(entry.pending, event)
	}
	qm.queue.Add(key)
	return coalesced
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

// newFederatedQueuedHandler routes informer notifications through the internal informer's key queues.
func (i *informer) newFederatedQueuedHandler(internalInformerIndex int) cache.ResourceEventHandlerFuncs {
	name := i.oType.Elem().Name()
	intInf := i.internalInformers[internalInformerIndex]
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// do not enqueue events to internal informer that has no handlers for better performance
			if atomic.LoadUint32(&intInf.hasHandlers) == hasNoHandler {
				return
			}
			intInf.queueMap.enqueueEvent(nil, obj, i.oType, false, func(e *event) {
				metrics.MetricResourceUpdateCount.WithLabelValues(name, "add").Inc()
				start := time.Now()
				intInf.forEachQueuedHandler(func(h *Handler) {
					h.OnAdd(e.obj, false)
				})
				metrics.MetricResourceAddLatency.Observe(time.Since(start).Seconds())
			})
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			// do not enqueue events to internal informer that has no handlers for better performance
			if atomic.LoadUint32(&intInf.hasHandlers) == hasNoHandler {
				return
			}
			coalesced := func() bool {
				intInf.coalescingFiltersMu.RLock()
				defer intInf.coalescingFiltersMu.RUnlock()
				return intInf.queueMap.enqueueEventWithFilterTransition(oldObj, newObj, i.oType, false, intInf.hasFilterTransition, func(e *event) {
					metrics.MetricResourceUpdateCount.WithLabelValues(name, "update").Inc()
					start := time.Now()
					intInf.forEachQueuedHandler(func(h *Handler) {
						old := e.oldObj.(metav1.Object)
						new := e.obj.(metav1.Object)
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
			}()
			if coalesced {
				metrics.MetricResourceUpdateCoalescedCount.WithLabelValues(name).Inc()
			}
		},
		DeleteFunc: func(obj interface{}) {
			realObj, err := ensureObjectOnDelete(obj, i.oType)
			if err != nil {
				klog.Errorf("Error in DeleteFunc: %v", err)
				return
			}
			// do not enqueue events to internal informer that has no handlers for better performance
			if atomic.LoadUint32(&intInf.hasHandlers) == hasNoHandler {
				intInf.queueMap.forgetDeletedObject(i.oType, realObj)
				return
			}
			intInf.queueMap.enqueueEvent(nil, realObj, i.oType, true, func(e *event) {
				metrics.MetricResourceUpdateCount.WithLabelValues(name, "delete").Inc()
				start := time.Now()
				intInf.forEachQueuedHandlerReversed(func(h *Handler) {
					h.OnDelete(e.obj)
				})
				metrics.MetricResourceDeleteLatency.Observe(time.Since(start).Seconds())
			})
		},
	}
}

func (inf *informer) removeAllHandlers() {
	for _, intInf := range inf.internalInformers {
		intInf.Lock()
		for _, handlers := range intInf.handlers {
			for _, handler := range handlers {
				inf.removeHandler(handler)
			}
		}
		intInf.Unlock()
	}
}

// shutdown removes handlers, stops queued workers, and waits for event processing to finish.
func (i *informer) shutdown() {
	i.removeAllHandlers()
	for _, intInf := range i.internalInformers {
		if intInf.queueMap != nil {
			intInf.queueMap.shutdown()
		}
	}

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
	case ClusterUserDefinedNetworkType:
		return userdefinednetworklister.NewClusterUserDefinedNetworkLister(sharedInformer.GetIndexer()), nil
	case UplinkType:
		return uplinklister.NewUplinkLister(sharedInformer.GetIndexer()), nil
	case UplinkStateType:
		return uplinklister.NewUplinkStateLister(sharedInformer.GetIndexer()), nil
	case ClusterNetworkConnectType:
		return networkconnectlister.NewClusterNetworkConnectLister(sharedInformer.GetIndexer()), nil
	case NetworkQoSType:
		return networkqoslister.NewNetworkQoSLister(sharedInformer.GetIndexer()), nil
	}

	return nil, fmt.Errorf("cannot create lister from type %v", oType)
}

func newBaseInformer(oType reflect.Type, sharedInformer cache.SharedIndexInformer) (*informer, error) {
	lister, err := newInformerLister(oType, sharedInformer)
	if err != nil {
		return nil, err
	}

	internalInformers := make([]*internalInformer, 0, internalInformerPoolSize)
	for i := 0; i < internalInformerPoolSize; i++ {
		internalInformers = append(internalInformers, &internalInformer{
			oType:             oType,
			handlers:          make(map[int]map[uint64]*Handler),
			coalescingFilters: make(map[uint64]func(interface{}) bool),
		})
	}

	return &informer{
		oType:             oType,
		inf:               sharedInformer,
		lister:            lister,
		internalInformers: internalInformers,
	}, nil
}

// newQueuedInformer wires an informer to serialized, key-deduplicated event queues.
func newQueuedInformer(queueSize uint32, oType reflect.Type, sharedInformer cache.SharedIndexInformer,
	stopChan chan struct{}, numEventQueues uint32) (*informer, error) {
	informer, err := newBaseInformer(oType, sharedInformer)
	if err != nil {
		return nil, err
	}

	informer.initialAddFunc = func(h *Handler, items []interface{}) {
		// Make a handler-specific channel array across which the
		// initial add events will be distributed. When a new handler
		// is added, only that handler should receive events for all
		// existing objects.
		addsWg := &sync.WaitGroup{}

		addsMap := newQueueMap(queueSize, numEventQueues, addsWg, stopChan)
		addsMap.start()

		// Distribute the existing items into the handler-specific
		// channel array.
		for _, obj := range items {
			addsMap.enqueueEvent(nil, obj, informer.oType, false, func(e *event) {
				h.OnAdd(e.obj, false)
			})
		}

		// Wait until all the object additions have been processed
		addsMap.shutdown()
		addsWg.Wait()
	}

	for i := 0; i < internalInformerPoolSize; i++ {
		informer.internalInformers[i].queueMap = newQueueMap(queueSize, numEventQueues, &informer.shutdownWg, stopChan)
		informer.internalInformers[i].queueMap.start()

		_, err = informer.inf.AddEventHandler(informer.newFederatedQueuedHandler(i))
		if err != nil {
			return nil, err
		}
	}

	return informer, nil

}

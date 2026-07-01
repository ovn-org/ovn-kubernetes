// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package pod

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	apitypes "k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// NetworkHandler reconciles pod state for a single network.
type NetworkHandler interface {
	GetNetworkName() string
	// SyncPods performs the initial full-network sync before per-pod
	// reconciliation is queued for the handler.
	SyncPods(pods []*corev1.Pod) error
	// PodExpectedOnNetwork returns whether the pod should exist on the network.
	PodExpectedOnNetwork(pod *corev1.Pod) (bool, error)
	// ReconcilePod is the single level-driven entry point. newPod == nil means
	// teardown, with lastState carrying the state returned by the last
	// successful reconcile of the pod, if any. On success it returns the state
	// to cache for the eventual teardown.
	ReconcilePod(oldPod, newPod *corev1.Pod, lastState interface{}) (interface{}, error)
}

// ErrorRecorder records reconcile failures for handlers that expose
// user-visible Kubernetes events.
type ErrorRecorder interface {
	RecordPodError(pod *corev1.Pod, reason string, err error)
}

// RelatedPodHandler requeues pods that share external state with the current pod.
type RelatedPodHandler interface {
	RelatedPodKeys(pod *corev1.Pod) ([]string, error)
}

// podEntry is the level-driven record for a pod on a network. All fields are
// guarded by Controller.stateMu. Pod objects are shared informer references
// and MUST NOT be modified.
type podEntry struct {
	// applied is the last successfully reconciled pod, nil while no reconcile
	// has succeeded yet.
	applied *corev1.Pod
	// lastSeen is the newest informer object observed for the pod, recorded on
	// every reconcile attempt and informer update so teardown always has a pod
	// object even when every reconcile failed.
	lastSeen *corev1.Pod
	// lastSeenSequence orders informer callbacks and lister snapshots without
	// relying on ResourceVersion formatting or comparison semantics.
	lastSeenSequence uint64
	// lastSeenDeleted distinguishes a bootstrap seed from an informer tombstone.
	// A late old-UID tombstone must replace a listed same-name replacement while
	// the network is still initializing.
	lastSeenDeleted bool
	// state is the handler state returned by the last successful reconcile.
	state interface{}
	// A successful teardown must not be replayed if only related-pod
	// queueing fails afterward: the handler may already have released IPAM.
	teardownComplete bool
	// lastErr dedups error events to one per failure streak.
	lastErr string
}

// podEntryUID returns the UID the entry tracks, preferring the last
// successfully applied pod and falling back to the last seen one.
func podEntryUID(entry *podEntry) apitypes.UID {
	if entry.applied != nil {
		return entry.applied.UID
	}
	if entry.lastSeen != nil {
		return entry.lastSeen.UID
	}
	return ""
}

// activeNetwork tracks a registered handler and its lifecycle: in-flight
// reconciles for deregistration draining, and the initial-listing barrier.
type activeNetwork struct {
	handler NetworkHandler

	// mu gates bootstrap and protects closed, pending, and done. Registration
	// publishes the network while holding mu so scoped workers can discover the
	// handler without running it before SyncPods and state seeding complete.
	mu       sync.Mutex
	closed   bool
	wg       sync.WaitGroup
	doneOnce sync.Once
	// initializing is guarded by Controller.networksMu. It lets delete callbacks
	// preserve tombstones during the small window before the bootstrap listing is
	// seeded into entries.
	initializing bool
	// pending tracks initial-listing pods not yet attempted; done is closed
	// once every one of them has been reconciled at least once, successfully
	// or not. Failed pods stay on the retry queue like any other.
	pending map[string]struct{}
	done    chan struct{}
}

// enter reserves an in-flight reconcile slot; it fails once the network is
// being deregistered.
func (n *activeNetwork) enter() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return false
	}
	n.wg.Add(1)
	return true
}

func (n *activeNetwork) leave() {
	n.wg.Done()
}

// markAttempted records that a pod from the initial listing has been
// reconciled at least once and closes the barrier when the listing is done.
func (n *activeNetwork) markAttempted(podKey string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pending == nil {
		return
	}
	delete(n.pending, podKey)
	if len(n.pending) == 0 {
		n.closeDoneLocked()
	}
}

func (n *activeNetwork) closeDoneLocked() {
	n.pending = nil
	n.doneOnce.Do(func() { close(n.done) })
}

// close marks the network deregistered and releases any barrier waiters.
func (n *activeNetwork) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	n.closeDoneLocked()
}

// Controller reconciles pod state for all registered networks.
type Controller struct {
	name string

	podController controller.Controller
	podLister     corev1listers.PodLister

	// podLocks serializes reconciles per pod across networks: a pod's network
	// controllers all patch the same pod-networks annotation.
	podLocks *syncmap.SyncMap[struct{}]
	// networkLocks serializes one network name's full register/deregister
	// lifecycle. An old generation must finish clearing state before a new
	// handler with the same name can publish and seed its state.
	networkLocks *syncmap.SyncMap[struct{}]

	// networksMu guards networks.
	networksMu sync.RWMutex
	networks   map[string]*activeNetwork

	// stateMu guards entries and every podEntry.
	stateMu sync.RWMutex
	// entries is keyed by network name, then pod key.
	entries map[string]map[string]*podEntry
	// terminalPodUID dedups completed-pod teardown while the terminal pod
	// remains visible in the informer and later delete events arrive.
	terminalPodUID map[string]map[string]apitypes.UID
	// observationSequence provides a process-local total order for informer
	// callbacks and lister reads. It is intentionally independent of Kubernetes
	// ResourceVersion semantics.
	observationSequence atomic.Uint64

	startMu sync.Mutex
	started bool
	stopped bool
}

const scopedPodQueueKeySeparator = "|"

// NewController builds a shared pod controller.
func NewController(wf *factory.WatchFactory, name string) *Controller {
	podInformer := wf.PodCoreInformer()
	c := &Controller{
		name:           name,
		podLister:      podInformer.Lister(),
		podLocks:       syncmap.NewSyncMap[struct{}](),
		networkLocks:   syncmap.NewSyncMap[struct{}](),
		networks:       map[string]*activeNetwork{},
		entries:        map[string]map[string]*podEntry{},
		terminalPodUID: map[string]map[string]apitypes.UID{},
	}

	podControllerConfig := &controller.ControllerConfig[corev1.Pod]{
		Informer:       podInformer.Informer(),
		Lister:         podInformer.Lister().List,
		MaxAttempts:    controller.InfiniteAttempts,
		ObjNeedsUpdate: c.podNeedsUpdate,
		OnDelete:       c.rememberDeletedPod,
		Reconcile:      c.reconcilePod,
		Threadiness:    15,
	}
	c.podController = controller.NewController(c.name+"-pod", podControllerConfig)

	return c
}

// NewPodController builds a controller that handles pod events for all networks.
func NewPodController(wf *factory.WatchFactory) *Controller {
	return NewController(wf, "pod-network")
}

// Start starts the pod workers.
func (c *Controller) Start() error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.stopped {
		return fmt.Errorf("%s: pod controller cannot be restarted after Stop", c.name)
	}
	if c.started {
		return nil
	}

	if err := controller.Start(c.podController); err != nil {
		return err
	}

	c.started = true
	return nil
}

// Stop stops the pod workers.
func (c *Controller) Stop() {
	c.startMu.Lock()
	if c.stopped {
		c.startMu.Unlock()
		return
	}
	c.stopped = true
	c.started = false

	// Remove registrations before releasing the lifecycle lock so a concurrent
	// registration cannot publish after the stop snapshot. Closing each network
	// releases initial-attempt waiters and prevents new in-flight reservations.
	c.networksMu.Lock()
	networks := make([]*activeNetwork, 0, len(c.networks))
	for netName, net := range c.networks {
		networks = append(networks, net)
		delete(c.networks, netName)
	}
	c.networksMu.Unlock()
	c.startMu.Unlock()

	for _, net := range networks {
		net.close()
	}
	controller.Stop(c.podController)
	for _, net := range networks {
		net.wg.Wait()
	}

	c.stateMu.Lock()
	c.entries = map[string]map[string]*podEntry{}
	c.terminalPodUID = map[string]map[string]apitypes.UID{}
	c.stateMu.Unlock()
}

// ReconcileNetwork queues reconciliation for a single pod/network pair.
func (c *Controller) ReconcileNetwork(podKey, netName string) {
	c.podController.Reconcile(scopedPodQueueKey(podKey, netName))
}

// ReconcileNetworkWithInvalidation invalidates handler-owned observations and
// queues a scoped reconcile while serialized with every reconcile for this pod.
// This ordering matters when an external dependency such as node topology is
// rebuilt: an older in-flight reconcile must not restore the observation after
// it has been invalidated and make the queued reconcile look like a no-op.
func (c *Controller) ReconcileNetworkWithInvalidation(podKey, netName string, invalidate func()) {
	c.podLocks.LockKey(podKey)
	defer c.podLocks.UnlockKey(podKey)

	if invalidate != nil {
		invalidate()
	}
	c.ReconcileNetwork(podKey, netName)
}

// ReconcilePendingPods queues pending pods for a single network.
func (c *Controller) ReconcilePendingPods(netName string) error {
	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list all pods: %w", err)
	}
	for _, pod := range pods {
		if !util.PodScheduled(pod) || pod.Status.Phase != corev1.PodPending {
			continue
		}
		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			return fmt.Errorf("failed to get pod key for %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		c.ReconcileNetwork(key, netName)
	}
	return nil
}

func (c *Controller) podNeedsUpdate(_, newPod *corev1.Pod) bool {
	// Keep same-UID teardown using the latest informer state while still
	// enqueueing every pod update.
	c.refreshLastSeen(newPod)
	return true
}

// RegisterNetworkController registers a per-network pod handler. It publishes
// a bootstrap-gated handler, syncs it against a full pod listing, and queues
// every listed pod for reconciliation. Publishing first closes the event gap:
// events concurrent with SyncPods can discover the network, while scoped work
// waits for bootstrap to finish. The returned channel closes once every listed
// pod has been attempted at least once, successfully or not.
func (c *Controller) RegisterNetworkController(handler NetworkHandler) (<-chan struct{}, error) {
	if handler == nil {
		return nil, fmt.Errorf("%s: nil pod handler registration", c.name)
	}
	netName := handler.GetNetworkName()
	c.networkLocks.LockKey(netName)
	defer c.networkLocks.UnlockKey(netName)

	net := &activeNetwork{
		handler:      handler,
		initializing: true,
		pending:      map[string]struct{}{},
		done:         make(chan struct{}),
	}
	// Hold the network gate until listing, state seeding, and SyncPods complete.
	// A scoped worker that observes the published network blocks in enter.
	net.mu.Lock()

	c.startMu.Lock()
	if c.stopped {
		c.startMu.Unlock()
		net.mu.Unlock()
		return nil, fmt.Errorf("%s: cannot register network %s after pod controller Stop", c.name, netName)
	}
	c.networksMu.Lock()
	if _, ok := c.networks[netName]; ok {
		c.networksMu.Unlock()
		c.startMu.Unlock()
		net.mu.Unlock()
		panic(fmt.Sprintf("%s: duplicate pod handler registration for network %q", c.name, netName))
	}
	c.networks[netName] = net
	c.networksMu.Unlock()
	c.startMu.Unlock()

	abort := func(err error) (<-chan struct{}, error) {
		c.networksMu.Lock()
		net.initializing = false
		if c.networks[netName] == net {
			delete(c.networks, netName)
		}
		c.networksMu.Unlock()

		net.closed = true
		net.closeDoneLocked()

		c.stateMu.Lock()
		delete(c.entries, netName)
		delete(c.terminalPodUID, netName)
		c.stateMu.Unlock()

		net.mu.Unlock()
		return nil, err
	}

	bootstrapSequence := c.nextObservationSequence()
	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		return abort(fmt.Errorf("%s: failed to list pods to bootstrap network %s: %w", c.name, netName, err))
	}

	podKeys := make([]string, 0, len(pods))
	for _, pod := range pods {
		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			return abort(fmt.Errorf("%s: failed to get pod key for %s/%s: %w", c.name, pod.Namespace, pod.Name, err))
		}
		podKeys = append(podKeys, key)
		net.pending[key] = struct{}{}
	}

	// SyncPods may retain durable state for any listed pod. Seed an unapplied
	// entry first so deletion or replacement before the first scoped attempt can
	// still drive teardown with the bootstrap pod object.
	c.seedBootstrapEntries(netName, pods, bootstrapSequence)
	if err := handler.SyncPods(pods); err != nil {
		return abort(fmt.Errorf("%s: failed to bootstrap network %s: %w", c.name, netName, err))
	}

	// Deregistration or Stop may have removed this network while SyncPods was
	// running. Do not resurrect it or report a successful registration.
	c.networksMu.Lock()
	if c.networks[netName] != net {
		c.networksMu.Unlock()
		return abort(fmt.Errorf("%s: network %s was deregistered during pod bootstrap", c.name, netName))
	}
	net.initializing = false
	c.networksMu.Unlock()

	if len(net.pending) == 0 {
		net.closeDoneLocked()
	}
	net.mu.Unlock()

	for _, key := range podKeys {
		c.ReconcileNetwork(key, netName)
	}
	return net.done, nil
}

// DeregisterNetworkController removes a per-network pod handler, waits for its
// in-flight reconciles to drain and clears associated pod state tracking.
func (c *Controller) DeregisterNetworkController(netName string) {
	c.networkLocks.LockKey(netName)
	defer c.networkLocks.UnlockKey(netName)

	c.networksMu.Lock()
	net := c.networks[netName]
	delete(c.networks, netName)
	c.networksMu.Unlock()
	if net == nil {
		return
	}
	net.close()
	net.wg.Wait()

	c.stateMu.Lock()
	delete(c.entries, netName)
	delete(c.terminalPodUID, netName)
	c.stateMu.Unlock()
}

func (c *Controller) getNetwork(netName string) *activeNetwork {
	c.networksMu.RLock()
	defer c.networksMu.RUnlock()
	return c.networks[netName]
}

func (c *Controller) networkNames() []string {
	c.networksMu.RLock()
	defer c.networksMu.RUnlock()
	names := make([]string, 0, len(c.networks))
	for name := range c.networks {
		names = append(names, name)
	}
	return names
}

func (c *Controller) reconcilePod(key string) error {
	podKey, netName := parseScopedPodQueueKey(key)
	if netName == "" {
		for _, networkName := range c.networkNames() {
			c.ReconcileNetwork(podKey, networkName)
		}
		return c.reconcileRelatedPods(podKey)
	}

	net := c.getNetwork(netName)
	if net == nil || !net.enter() {
		return nil
	}
	defer net.leave()
	defer net.markAttempted(podKey)

	// A pod's network controllers all patch the same pod-networks annotation.
	// Serialize by pod key so per-network reconciles cannot clobber each other.
	c.podLocks.LockKey(podKey)
	defer c.podLocks.UnlockKey(podKey)
	return c.reconcilePodForNetwork(net.handler, podKey, netName)
}

func (c *Controller) reconcileRelatedPods(podKey string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(podKey)
	if err != nil {
		return fmt.Errorf("failed to split pod key %s: %w", podKey, err)
	}
	pod, err := c.podLister.Pods(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: failed to get pod %s for related-pod reconciliation: %w", c.name, podKey, err)
	}

	for _, networkName := range c.networkNames() {
		net := c.getNetwork(networkName)
		if net == nil || !net.enter() {
			continue
		}
		err := c.reconcileRelatedPodsForNetwork(net.handler, networkName, podKey, pod)
		net.leave()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) reconcileRelatedPodsForNetwork(handler NetworkHandler, netName, podKey string, pod *corev1.Pod) error {
	relatedHandler, ok := handler.(RelatedPodHandler)
	if !ok {
		return nil
	}
	relatedPodKeys, err := relatedHandler.RelatedPodKeys(pod)
	if err != nil {
		return fmt.Errorf("%s: failed to get related pod keys for pod %s on network %s: %w", c.name, podKey, netName, err)
	}
	for _, relatedPodKey := range relatedPodKeys {
		if relatedPodKey == "" || relatedPodKey == podKey {
			continue
		}
		c.ReconcileNetwork(relatedPodKey, netName)
	}
	return nil
}

func (c *Controller) reconcilePodForNetwork(handler NetworkHandler, podKey, netName string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(podKey)
	if err != nil {
		return fmt.Errorf("failed to split pod key %s: %w", podKey, err)
	}

	// Take the observation sequence before reading the lister. Any informer
	// callback that begins after this point receives a larger sequence and must
	// not be overwritten when this snapshot is recorded below.
	observationSequence := c.nextObservationSequence()
	pod, err := c.podLister.Pods(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("%s: failed to get pod %s while reconciling network %s: %w", c.name, podKey, netName, err)
	}
	podFound := err == nil

	if !podFound {
		return c.teardown(handler, podKey, netName, nil, observationSequence)
	}

	if uid := c.entryUID(netName, podKey); uid != "" && uid != pod.UID {
		// The pod was recreated under the same name; tear the old instance
		// down before evaluating the replacement. In particular, an error while
		// deciding replacement eligibility must never overwrite the only old-UID
		// tombstone before it is deleted.
		if err := c.teardown(handler, podKey, netName, nil, observationSequence); err != nil {
			return err
		}
	}

	expected, err := handler.PodExpectedOnNetwork(pod)
	if err != nil {
		c.markSeen(netName, podKey, pod, observationSequence)
		c.recordPodError(handler, netName, podKey, pod, "ErrorUpdatingResource", err)
		return err
	}

	if !expected {
		// Tear down whatever the entry knows about; completed pods without an
		// entry still get a best-effort teardown with the informer object.
		var fallbackPod *corev1.Pod
		if util.PodCompleted(pod) {
			if c.terminalPodProcessed(netName, podKey, pod.UID) {
				return nil
			}
			fallbackPod = pod
		}
		if err := c.teardown(handler, podKey, netName, fallbackPod, observationSequence); err != nil {
			return err
		}
		if fallbackPod != nil {
			c.markTerminalPodProcessed(netName, podKey, fallbackPod.UID)
		}
		return nil
	}

	applied, state := c.markSeen(netName, podKey, pod, observationSequence)
	newState, err := handler.ReconcilePod(applied, pod, state)
	if err != nil {
		reason := "ErrorAddingResource"
		if applied != nil {
			reason = "ErrorUpdatingResource"
		}
		c.recordPodError(handler, netName, podKey, pod, reason, err)
		return err
	}
	c.markApplied(netName, podKey, pod, newState)
	return nil
}

// teardown reconciles the pod as absent using the entry's best available pod
// object, or fallbackPod when no entry exists. On success it requeues related
// pods and clears the entry.
func (c *Controller) teardown(handler NetworkHandler, podKey, netName string, fallbackPod *corev1.Pod, observationSequence uint64) error {
	if fallbackPod != nil {
		c.rememberTeardownFallback(netName, podKey, fallbackPod, observationSequence)
	}
	deletePod, state := c.teardownPod(netName, podKey)
	if deletePod != nil {
		c.stateMu.RLock()
		complete := c.entries[netName][podKey].teardownComplete
		c.stateMu.RUnlock()
		if !complete {
			if _, err := handler.ReconcilePod(deletePod, nil, state); err != nil {
				c.recordPodError(handler, netName, podKey, deletePod, "ErrorDeletingResource", err)
				return err
			}
			c.stateMu.Lock()
			c.ensureEntryLocked(netName, podKey).teardownComplete = true
			c.stateMu.Unlock()
		}
		// Requeue pods sharing external state with the deleted pod so state
		// that referenced it (e.g. VM routes flipped during live migration)
		// is restored promptly.
		if err := c.requeueRelatedPods(handler, netName, podKey, deletePod); err != nil {
			c.recordPodError(handler, netName, podKey, deletePod, "ErrorDeletingResource", err)
			return err
		}
	}
	c.deleteEntry(netName, podKey)
	return nil
}

// requeueRelatedPods requeues pods that share external state with a deleted
// pod on the handler's network.
func (c *Controller) requeueRelatedPods(handler NetworkHandler, netName, podKey string, pod *corev1.Pod) error {
	relatedHandler, ok := handler.(RelatedPodHandler)
	if !ok {
		return nil
	}
	relatedPodKeys, err := relatedHandler.RelatedPodKeys(pod)
	if err != nil {
		return fmt.Errorf("%s: failed to get related pod keys for deleted pod %s on network %s: %w", c.name, podKey, netName, err)
	}
	for _, relatedPodKey := range relatedPodKeys {
		if relatedPodKey == "" || relatedPodKey == podKey {
			continue
		}
		c.ReconcileNetwork(relatedPodKey, netName)
	}
	return nil
}

// recordPodError records a reconcile failure once per failure streak:
// reconciles retry indefinitely and must not flood the recorder with
// duplicate events.
func (c *Controller) recordPodError(handler NetworkHandler, netName, podKey string, pod *corev1.Pod, reason string, err error) {
	if pod == nil || err == nil {
		return
	}
	recorder, ok := handler.(ErrorRecorder)
	if !ok {
		return
	}
	errMsg := reason + ": " + err.Error()
	c.stateMu.Lock()
	entry := c.ensureEntryLocked(netName, podKey)
	if entry.lastErr == errMsg {
		c.stateMu.Unlock()
		return
	}
	entry.lastErr = errMsg
	c.stateMu.Unlock()
	recorder.RecordPodError(pod, reason, err)
}

// ensureEntryLocked returns the entry for the pod on the network, creating it
// when needed. Callers must hold stateMu.
func (c *Controller) ensureEntryLocked(netName, podKey string) *podEntry {
	pods := c.entries[netName]
	if pods == nil {
		pods = map[string]*podEntry{}
		c.entries[netName] = pods
	}
	entry := pods[podKey]
	if entry == nil {
		entry = &podEntry{}
		pods[podKey] = entry
	}
	return entry
}

// seedBootstrapEntries records the pod objects whose durable state SyncPods is
// about to inspect. These are deliberately not marked applied: only a
// successful per-pod reconcile can advance applied state. A delete tombstone
// captured while registration was listing wins over the listed object.
func (c *Controller) seedBootstrapEntries(netName string, pods []*corev1.Pod, sequence uint64) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	for _, pod := range pods {
		podKey, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			// Registration already validated every key before calling this helper.
			continue
		}
		entry := c.ensureEntryLocked(netName, podKey)
		// Any delete observation captured while this network was initializing
		// wins over the listed object, even when its UID differs. It is the old
		// incarnation that must be deleted before the listed replacement.
		if entry.lastSeenDeleted {
			continue
		}
		c.observeLastSeenLocked(entry, pod, sequence, false)
	}
}

// rememberTeardownFallback persists an informer pod before best-effort
// teardown. If teardown fails and the pod subsequently leaves the informer,
// the retry must still have an object with which to clean up partial state.
func (c *Controller) rememberTeardownFallback(netName, podKey string, pod *corev1.Pod, sequence uint64) {
	if pod == nil {
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	entry := c.ensureEntryLocked(netName, podKey)
	uid := podEntryUID(entry)
	if uid == "" || uid == pod.UID {
		c.observeLastSeenLocked(entry, pod, sequence, false)
	}
}

func (c *Controller) nextObservationSequence() uint64 {
	return c.observationSequence.Add(1)
}

// observeLastSeenLocked advances lastSeen only when this observation began no
// earlier than the one already stored. Callers must hold stateMu.
func (c *Controller) observeLastSeenLocked(entry *podEntry, pod *corev1.Pod, sequence uint64, deleted bool) bool {
	if entry.lastSeen != nil && sequence < entry.lastSeenSequence {
		return false
	}
	entry.lastSeen = pod
	entry.lastSeenSequence = sequence
	entry.lastSeenDeleted = deleted
	return true
}

// entryUID returns the UID tracked for the pod on the network, empty when
// nothing is tracked.
func (c *Controller) entryUID(netName, podKey string) apitypes.UID {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	entry := c.entries[netName][podKey]
	if entry == nil {
		return ""
	}
	return podEntryUID(entry)
}

// markSeen records the pod as the latest observed object before a reconcile
// attempt, so a later teardown always has a pod object even when every
// reconcile failed. It returns the last applied pod and state for the attempt.
func (c *Controller) markSeen(netName, podKey string, pod *corev1.Pod, sequence uint64) (*corev1.Pod, interface{}) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	entry := c.ensureEntryLocked(netName, podKey)
	c.observeLastSeenLocked(entry, pod, sequence, false)
	c.clearTerminalPodProcessedLocked(netName, podKey)
	return entry.applied, entry.state
}

// markApplied records a successful reconcile and resets error-event dedup.
func (c *Controller) markApplied(netName, podKey string, pod *corev1.Pod, state interface{}) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	entry := c.ensureEntryLocked(netName, podKey)
	entry.applied = pod
	// An informer callback may have recorded a newer same-UID object while the
	// handler was running. Never downgrade it to the attempt's older object.
	if entry.lastSeen == nil {
		entry.lastSeen = pod
		entry.lastSeenDeleted = false
	}
	entry.state = state
	entry.teardownComplete = false
	entry.lastErr = ""
	c.clearTerminalPodProcessedLocked(netName, podKey)
}

// teardownPod returns the best pod object and state to tear down with: the
// latest observed object when it matches the applied pod's UID, otherwise the
// applied pod itself, otherwise the last attempted object with no state.
func (c *Controller) teardownPod(netName, podKey string) (*corev1.Pod, interface{}) {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	entry := c.entries[netName][podKey]
	if entry == nil {
		return nil, nil
	}
	if entry.applied == nil {
		return entry.lastSeen, nil
	}
	if entry.lastSeen != nil && entry.lastSeen.UID == entry.applied.UID {
		return entry.lastSeen, entry.state
	}
	return entry.applied, entry.state
}

func (c *Controller) deleteEntry(netName, podKey string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.clearTerminalPodProcessedLocked(netName, podKey)
	pods := c.entries[netName]
	if pods == nil {
		return
	}
	delete(pods, podKey)
	if len(pods) == 0 {
		delete(c.entries, netName)
	}
}

func (c *Controller) terminalPodProcessed(netName, podKey string, uid apitypes.UID) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.terminalPodUID[netName][podKey] == uid
}

func (c *Controller) markTerminalPodProcessed(netName, podKey string, uid apitypes.UID) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	pods := c.terminalPodUID[netName]
	if pods == nil {
		pods = map[string]apitypes.UID{}
		c.terminalPodUID[netName] = pods
	}
	pods[podKey] = uid
}

func (c *Controller) clearTerminalPodProcessedLocked(netName, podKey string) {
	pods := c.terminalPodUID[netName]
	if pods == nil {
		return
	}
	delete(pods, podKey)
	if len(pods) == 0 {
		delete(c.terminalPodUID, netName)
	}
}

// refreshLastSeen keeps existing entries tracking the pod's current UID pointed
// at the newest informer object.
func (c *Controller) refreshLastSeen(pod *corev1.Pod) {
	if pod == nil {
		return
	}
	sequence := c.nextObservationSequence()
	podKey, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	for _, pods := range c.entries {
		entry := pods[podKey]
		if entry == nil || podEntryUID(entry) != pod.UID {
			continue
		}
		c.observeLastSeenLocked(entry, pod, sequence, false)
	}
}

// rememberDeletedPod preserves delete metadata for existing entries and for
// networks still bootstrapping. The latter closes the list-to-seed window: a
// pod retained by SyncPods can disappear before its bootstrap entry is seeded.
func (c *Controller) rememberDeletedPod(pod *corev1.Pod) {
	if pod == nil {
		return
	}
	sequence := c.nextObservationSequence()
	podKey, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		return
	}

	// Hold networksMu while creating initializing-network entries so Stop or
	// deregistration cannot remove the network and clear its state immediately
	// before this callback recreates it.
	c.networksMu.RLock()
	c.stateMu.Lock()
	for _, pods := range c.entries {
		entry := pods[podKey]
		if entry == nil || podEntryUID(entry) != pod.UID {
			continue
		}
		c.observeLastSeenLocked(entry, pod, sequence, true)
	}
	for netName, net := range c.networks {
		if !net.initializing {
			continue
		}
		entry := c.ensureEntryLocked(netName, podKey)
		if entry.applied != nil {
			if entry.applied.UID == pod.UID {
				c.observeLastSeenLocked(entry, pod, sequence, true)
			}
			continue
		}
		uid := podEntryUID(entry)
		switch {
		case uid == "" || uid == pod.UID:
			c.observeLastSeenLocked(entry, pod, sequence, true)
		case !entry.lastSeenDeleted:
			// A listed replacement may already have been seeded. During
			// initialization, a different-UID tombstone is the old generation
			// whose durable state SyncPods may have retained; it must win even
			// when its callback began before the list snapshot was recorded.
			entry.lastSeen = pod
			entry.lastSeenSequence = sequence
			entry.lastSeenDeleted = true
		case sequence >= entry.lastSeenSequence:
			// Multiple rapid same-name generations can produce more than one
			// tombstone. Retain the latest explicitly observed deletion.
			entry.lastSeen = pod
			entry.lastSeenSequence = sequence
			entry.lastSeenDeleted = true
		}
	}
	c.stateMu.Unlock()
	c.networksMu.RUnlock()
}

func scopedPodQueueKey(podKey, netName string) string {
	if netName == "" {
		return podKey
	}
	return podKey + scopedPodQueueKeySeparator + netName
}

func parseScopedPodQueueKey(key string) (podKey, netName string) {
	parts := strings.SplitN(key, scopedPodQueueKeySeparator, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return key, ""
	}
	return parts[0], parts[1]
}

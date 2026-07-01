// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package pod

import (
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
)

type fakePodHandler struct {
	netName string
	expect  map[string]bool
	state   map[string]interface{}

	syncCalls      int
	reconcileCalls int
	deleteCalls    int
	events         []string
	lastOldPod     *corev1.Pod
	lastDeletePod  *corev1.Pod
	lastState      interface{}
	reconcileErr   error
	recordedErrors []string

	syncStarted       chan struct{}
	syncContinue      chan struct{}
	syncErr           error
	reconcileStarted  chan struct{}
	reconcileContinue chan struct{}
	reconcileOnce     sync.Once
	expectedStarted   chan struct{}
	expectedContinue  chan struct{}
	expectedOnce      sync.Once
	expectedErr       error
	relatedStarted    chan struct{}
	relatedContinue   chan struct{}
	relatedOnce       sync.Once
	relatedErr        error
	relatedCalls      int
	relatedKeys       []string
}

func (f *fakePodHandler) GetNetworkName() string {
	return f.netName
}

func (f *fakePodHandler) SyncPods(_ []*corev1.Pod) error {
	f.syncCalls++
	if f.syncStarted != nil {
		close(f.syncStarted)
	}
	if f.syncContinue != nil {
		<-f.syncContinue
	}
	return f.syncErr
}

func (f *fakePodHandler) PodExpectedOnNetwork(pod *corev1.Pod) (bool, error) {
	if f.expectedStarted != nil {
		f.expectedOnce.Do(func() { close(f.expectedStarted) })
		if f.expectedContinue != nil {
			<-f.expectedContinue
		}
	}
	if f.expectedErr != nil {
		return false, f.expectedErr
	}
	key := pod.Namespace + "/" + pod.Name
	return f.expect[key], nil
}

func (f *fakePodHandler) ReconcilePod(oldPod, newPod *corev1.Pod, lastState interface{}) (interface{}, error) {
	if newPod != nil && f.reconcileStarted != nil {
		f.reconcileOnce.Do(func() { close(f.reconcileStarted) })
		if f.reconcileContinue != nil {
			<-f.reconcileContinue
		}
	}
	if f.reconcileErr != nil {
		return nil, f.reconcileErr
	}
	if newPod == nil {
		f.deleteCalls++
		f.events = append(f.events, "delete:"+string(oldPod.UID))
		f.lastDeletePod = oldPod
		f.lastState = lastState
		return nil, nil
	}
	f.reconcileCalls++
	f.events = append(f.events, "reconcile:"+string(newPod.UID))
	f.lastOldPod = oldPod
	key := newPod.Namespace + "/" + newPod.Name
	if f.state != nil {
		return f.state[key], nil
	}
	return string(newPod.UID), nil
}

func (f *fakePodHandler) RelatedPodKeys(_ *corev1.Pod) ([]string, error) {
	f.relatedCalls++
	if f.relatedStarted != nil {
		f.relatedOnce.Do(func() { close(f.relatedStarted) })
		if f.relatedContinue != nil {
			<-f.relatedContinue
		}
	}
	if f.relatedErr != nil {
		return nil, f.relatedErr
	}
	return f.relatedKeys, nil
}

func (f *fakePodHandler) RecordPodError(pod *corev1.Pod, reason string, err error) {
	f.recordedErrors = append(f.recordedErrors, reason+":"+string(pod.UID)+":"+err.Error())
}

func newPod(namespace, name, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			UID:       types.UID(uid),
		},
	}
}

func newPodLister(t *testing.T, pods ...*corev1.Pod) corelisters.PodLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, pod := range pods {
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("failed to add pod to indexer: %v", err)
		}
	}
	return corelisters.NewPodLister(indexer)
}

type blockingPodLister struct {
	corelisters.PodLister
	listStarted  chan struct{}
	listContinue chan struct{}
	listOnce     sync.Once
}

func (l *blockingPodLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	l.listOnce.Do(func() { close(l.listStarted) })
	<-l.listContinue
	return l.PodLister.List(selector)
}

func newPodControllerForTest(t *testing.T, pods ...*corev1.Pod) *Controller {
	t.Helper()
	return &Controller{
		name:      "pod-test",
		podLister: newPodLister(t, pods...),
		podController: controller.NewController("pod-test-controller", &controller.ControllerConfig[corev1.Pod]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:   func(string) error { return nil },
			Threadiness: 1,
			Lister: func(labels.Selector) ([]*corev1.Pod, error) {
				return nil, nil
			},
			ObjNeedsUpdate: func(_, _ *corev1.Pod) bool { return true },
		}),
		podLocks:       syncmap.NewSyncMap[struct{}](),
		networkLocks:   syncmap.NewSyncMap[struct{}](),
		networks:       map[string]*activeNetwork{},
		entries:        map[string]map[string]*podEntry{},
		terminalPodUID: map[string]map[string]types.UID{},
	}
}

func (c *Controller) entryForTest(netName, podKey string) *podEntry {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.entries[netName][podKey]
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestRegisterNetworkControllerQueuesAndSignalsInitialReconcile(t *testing.T) {
	podA := newPod("ns", "a", "uid-a")
	podB := newPod("ns", "b", "uid-b")
	c := newPodControllerForTest(t, podA, podB)
	queueRecorder := &controller.FakeController{}
	c.podController = queueRecorder
	handler := &fakePodHandler{
		netName: "net-a",
		expect: map[string]bool{
			"ns/a": true,
			"ns/b": false,
		},
	}

	done, err := c.RegisterNetworkController(handler)
	if err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	if handler.syncCalls != 1 {
		t.Fatalf("expected one sync call, got %d", handler.syncCalls)
	}
	// Registration only queues work; nothing is reconciled inline.
	if handler.reconcileCalls != 0 {
		t.Fatalf("expected no inline reconcile during registration, got %d", handler.reconcileCalls)
	}
	if isClosed(done) {
		t.Fatal("expected initial reconcile barrier to be open")
	}
	queueRecorder.Lock()
	queuedReconciles := append([]string(nil), queueRecorder.Reconciles...)
	queueRecorder.Unlock()
	queuedCounts := map[string]int{}
	for _, queuedReconcile := range queuedReconciles {
		queuedCounts[queuedReconcile]++
	}
	if len(queuedReconciles) != 2 ||
		queuedCounts["Reconcile:ns/a|net-a"] != 1 ||
		queuedCounts["Reconcile:ns/b|net-a"] != 1 {
		t.Fatalf("expected registration to enqueue both scoped pod keys once, got %#v", queuedReconciles)
	}

	// Exercise the reconciles directly so the initial-attempt barrier can be
	// inspected after each key; enqueueing itself is asserted above.
	if err := c.reconcilePod("ns/a|net-a"); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if isClosed(done) {
		t.Fatal("expected barrier to stay open until every pod is attempted")
	}
	if err := c.reconcilePod("ns/b|net-a"); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if !isClosed(done) {
		t.Fatal("expected barrier to close after every pod was attempted")
	}

	if handler.reconcileCalls != 1 {
		t.Fatalf("expected only the expected pod to be reconciled, got %d", handler.reconcileCalls)
	}
	if entry := c.entryForTest("net-a", "ns/a"); entry == nil || entry.applied == nil || entry.state != "uid-a" {
		t.Fatalf("expected applied entry with state for ns/a, got %#v", entry)
	}
	if entry := c.entryForTest("net-a", "ns/b"); entry != nil {
		t.Fatalf("expected no entry for unexpected pod, got %#v", entry)
	}
}

func TestRegisterNetworkControllerBarrierClosesOnFailedAttempts(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{"ns/pod": true},
		reconcileErr: errors.New("boom"),
	}

	done, err := c.RegisterNetworkController(handler)
	if err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	if err := c.reconcilePod("ns/pod|net-a"); err == nil {
		t.Fatal("expected reconcile error")
	}
	// A failed first attempt still counts: a wedged pod must not block
	// dependent controllers forever; it stays on the retry queue.
	if !isClosed(done) {
		t.Fatal("expected barrier to close after failed attempt")
	}
}

func TestRegisterNetworkControllerPublishesBeforeBlockedSync(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:          "net-a",
		expect:           map[string]bool{"ns/pod": true},
		syncStarted:      make(chan struct{}),
		syncContinue:     make(chan struct{}),
		reconcileStarted: make(chan struct{}),
	}

	registerErr := make(chan error, 1)
	go func() {
		_, err := c.RegisterNetworkController(handler)
		registerErr <- err
	}()
	<-handler.syncStarted

	if c.getNetwork(handler.netName) == nil {
		t.Fatal("expected network to be published while SyncPods is blocked")
	}
	reconcileErr := make(chan error, 1)
	go func() {
		reconcileErr <- c.reconcilePod("ns/pod|net-a")
	}()
	select {
	case <-handler.reconcileStarted:
		t.Fatal("scoped reconcile ran before network bootstrap completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(handler.syncContinue)
	if err := <-registerErr; err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	select {
	case <-handler.reconcileStarted:
	case <-time.After(time.Second):
		t.Fatal("scoped reconcile did not resume after bootstrap")
	}
	if err := <-reconcileErr; err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
}

func TestReconcileNetworkWithInvalidationWaitsForInFlightPodReconcile(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:           "net-a",
		expect:            map[string]bool{"ns/pod": true},
		reconcileStarted:  make(chan struct{}),
		reconcileContinue: make(chan struct{}),
	}
	if _, err := c.RegisterNetworkController(handler); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}

	reconcileErr := make(chan error, 1)
	go func() {
		reconcileErr <- c.reconcilePod("ns/pod|net-a")
	}()
	<-handler.reconcileStarted

	invalidationAttempted := make(chan struct{})
	invalidated := make(chan struct{})
	invalidationDone := make(chan struct{})
	go func() {
		close(invalidationAttempted)
		c.ReconcileNetworkWithInvalidation("ns/pod", handler.netName, func() {
			close(invalidated)
		})
		close(invalidationDone)
	}()
	<-invalidationAttempted
	select {
	case <-invalidated:
		t.Fatal("invalidation ran while an older pod reconcile could still restore stale state")
	case <-time.After(50 * time.Millisecond):
	}

	close(handler.reconcileContinue)
	if err := <-reconcileErr; err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	select {
	case <-invalidated:
	case <-time.After(time.Second):
		t.Fatal("invalidation did not run after the in-flight reconcile finished")
	}
	select {
	case <-invalidationDone:
	case <-time.After(time.Second):
		t.Fatal("serialized invalidation did not finish")
	}
}

func TestBootstrapDeleteBeforeFirstAttemptUsesLatestDeleteObject(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	deletedPod := pod.DeepCopy()
	deletedPod.Status.Phase = corev1.PodSucceeded
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:      "net-a",
		syncStarted:  make(chan struct{}),
		syncContinue: make(chan struct{}),
	}

	registerErr := make(chan error, 1)
	go func() {
		_, err := c.RegisterNetworkController(handler)
		registerErr <- err
	}()
	<-handler.syncStarted
	c.rememberDeletedPod(deletedPod)
	c.podLister = newPodLister(t)
	close(handler.syncContinue)
	if err := <-registerErr; err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}

	if err := c.reconcilePod("ns/pod|net-a"); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.deleteCalls != 1 {
		t.Fatalf("expected one bootstrap teardown, got %d", handler.deleteCalls)
	}
	if handler.lastDeletePod == nil || handler.lastDeletePod.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("expected teardown to use latest delete object, got %#v", handler.lastDeletePod)
	}
}

func TestBootstrapReplacementDeletesListedUIDBeforeReconcilingCurrentUID(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	newPod := newPod("ns", "pod", "new")
	c := newPodControllerForTest(t, oldPod)
	handler := &fakePodHandler{
		netName: "net-a",
		expect:  map[string]bool{"ns/pod": true},
	}

	if _, err := c.RegisterNetworkController(handler); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	c.podLister = newPodLister(t, newPod)
	if err := c.reconcilePod("ns/pod|net-a"); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if got := handler.events; len(got) != 2 || got[0] != "delete:old" || got[1] != "reconcile:new" {
		t.Fatalf("unexpected bootstrap replacement order: %#v", got)
	}
}

func TestBootstrapDeleteTombstoneWinsOverListedReplacement(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	newPod := newPod("ns", "pod", "new")
	c := newPodControllerForTest(t, newPod)
	listStarted := make(chan struct{})
	listContinue := make(chan struct{})
	c.podLister = &blockingPodLister{
		PodLister:    c.podLister,
		listStarted:  listStarted,
		listContinue: listContinue,
	}
	handler := &fakePodHandler{
		netName: "net-a",
		expect:  map[string]bool{"ns/pod": true},
	}

	registerErr := make(chan error, 1)
	go func() {
		_, err := c.RegisterNetworkController(handler)
		registerErr <- err
	}()
	<-listStarted
	// The old pod's delete can arrive after publication but before the
	// bootstrap list is seeded. It must not be overwritten by the listed
	// replacement, otherwise old durable state is never torn down.
	c.rememberDeletedPod(oldPod)
	close(listContinue)
	if err := <-registerErr; err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}

	if err := c.reconcilePod("ns/pod|net-a"); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if got := handler.events; len(got) != 2 || got[0] != "delete:old" || got[1] != "reconcile:new" {
		t.Fatalf("unexpected tombstone/replacement order: %#v", got)
	}
}

func TestUIDReplacementTeardownPrecedesExpectedError(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	newPod := newPod("ns", "pod", "new")
	c := newPodControllerForTest(t, oldPod)
	handler := &fakePodHandler{
		netName:     "net-a",
		expectedErr: errors.New("cannot resolve replacement eligibility"),
	}
	if _, err := c.RegisterNetworkController(handler); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}

	c.podLister = newPodLister(t, newPod)
	err := c.reconcilePod("ns/pod|net-a")
	if err == nil {
		t.Fatal("expected replacement eligibility error")
	}
	if got := handler.events; len(got) != 1 || got[0] != "delete:old" {
		t.Fatalf("expected old UID teardown before eligibility error, got %#v", got)
	}
	entry := c.entryForTest(handler.netName, "ns/pod")
	if entry == nil || entry.lastSeen == nil || entry.lastSeen.UID != newPod.UID {
		t.Fatalf("expected replacement to be retained for retry after old teardown, got %#v", entry)
	}
}

func TestLateBootstrapDeleteTombstoneReplacesListedReplacementSeed(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	newPod := newPod("ns", "pod", "new")
	c := newPodControllerForTest(t, newPod)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{"ns/pod": true},
		syncStarted:  make(chan struct{}),
		syncContinue: make(chan struct{}),
	}

	registerErr := make(chan error, 1)
	go func() {
		_, err := c.RegisterNetworkController(handler)
		registerErr <- err
	}()
	<-handler.syncStarted // the replacement has already been seeded
	c.rememberDeletedPod(oldPod)
	close(handler.syncContinue)
	if err := <-registerErr; err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}

	if err := c.reconcilePod("ns/pod|net-a"); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if got := handler.events; len(got) != 2 || got[0] != "delete:old" || got[1] != "reconcile:new" {
		t.Fatalf("unexpected late-tombstone replacement order: %#v", got)
	}
}

func TestReconcileRetryAfterFailedAddStillLooksLikeAdd(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{"ns/pod": true},
		reconcileErr: errors.New("boom"),
	}

	if _, err := c.RegisterNetworkController(handler); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	if err := c.reconcilePod("ns/pod|net-a"); err == nil {
		t.Fatal("expected reconcile error")
	}

	handler.reconcileErr = nil
	if err := c.reconcilePod("ns/pod|net-a"); err != nil {
		t.Fatalf("unexpected retry error: %v", err)
	}
	if handler.lastOldPod != nil {
		t.Fatalf("expected retry before first success to pass nil oldPod, got %#v", handler.lastOldPod)
	}
	if entry := c.entryForTest("net-a", "ns/pod"); entry == nil || entry.applied == nil {
		t.Fatalf("expected retry to apply pod, got %#v", entry)
	}
}

func TestDeregisterNetworkControllerClearsState(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{"ns/pod": true}}

	if _, err := c.RegisterNetworkController(handler); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	if err := c.reconcilePod("ns/pod|net-a"); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	c.DeregisterNetworkController("net-a")
	if c.getNetwork("net-a") != nil {
		t.Fatal("expected network to be deregistered")
	}
	if entry := c.entryForTest("net-a", "ns/pod"); entry != nil {
		t.Fatalf("expected entries to be cleared, got %#v", entry)
	}
	// Reconciles for a deregistered network are silent no-ops.
	if err := c.reconcilePod("ns/pod|net-a"); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.reconcileCalls != 1 {
		t.Fatalf("expected no reconcile after deregistration, got %d", handler.reconcileCalls)
	}
}

func TestDeregisterCleanupFinishesBeforeSameNameRegistration(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	newPod := newPod("ns", "pod", "new")
	c := newPodControllerForTest(t, oldPod)
	oldHandler := &fakePodHandler{
		netName: "net-a",
		expect:  map[string]bool{"ns/pod": true},
	}
	if _, err := c.RegisterNetworkController(oldHandler); err != nil {
		t.Fatalf("unexpected old-generation register error: %v", err)
	}
	if err := c.reconcilePod("ns/pod|net-a"); err != nil {
		t.Fatalf("unexpected old-generation reconcile error: %v", err)
	}

	oldNetwork := c.getNetwork(oldHandler.netName)
	if oldNetwork == nil || !oldNetwork.enter() {
		t.Fatal("failed to reserve an old-generation in-flight operation")
	}
	deregisterDone := make(chan struct{})
	go func() {
		c.DeregisterNetworkController(oldHandler.netName)
		close(deregisterDone)
	}()
	deadline := time.After(time.Second)
	for c.getNetwork(oldHandler.netName) != nil {
		select {
		case <-deadline:
			oldNetwork.leave()
			t.Fatal("deregistration did not remove the old network")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	c.podLister = newPodLister(t, newPod)
	newHandler := &fakePodHandler{
		netName:     oldHandler.netName,
		syncStarted: make(chan struct{}),
	}
	registerErr := make(chan error, 1)
	go func() {
		_, err := c.RegisterNetworkController(newHandler)
		registerErr <- err
	}()
	select {
	case <-newHandler.syncStarted:
		oldNetwork.leave()
		t.Fatal("same-name registration started before old-generation cleanup")
	case <-time.After(50 * time.Millisecond):
	}

	oldNetwork.leave()
	select {
	case <-deregisterDone:
	case <-time.After(time.Second):
		t.Fatal("old-generation deregistration did not finish")
	}
	select {
	case <-newHandler.syncStarted:
	case <-time.After(time.Second):
		t.Fatal("new generation did not start after deregistration cleanup")
	}
	if err := <-registerErr; err != nil {
		t.Fatalf("unexpected new-generation register error: %v", err)
	}
	if c.getNetwork(newHandler.netName) == nil {
		t.Fatal("old-generation cleanup removed the new network")
	}
	entry := c.entryForTest(newHandler.netName, "ns/pod")
	if entry == nil || entry.lastSeen == nil || entry.lastSeen.UID != newPod.UID {
		t.Fatalf("old-generation cleanup erased new state: %#v", entry)
	}
}

func TestFailedRegistrationCleanupFinishesBeforeSameNameRegistration(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	newPod := newPod("ns", "pod", "new")
	c := newPodControllerForTest(t, oldPod)
	oldHandler := &fakePodHandler{
		netName:      "net-a",
		syncStarted:  make(chan struct{}),
		syncContinue: make(chan struct{}),
		syncErr:      errors.New("bootstrap failed"),
	}
	oldRegisterErr := make(chan error, 1)
	go func() {
		_, err := c.RegisterNetworkController(oldHandler)
		oldRegisterErr <- err
	}()
	<-oldHandler.syncStarted

	c.podLister = newPodLister(t, newPod)
	newHandler := &fakePodHandler{
		netName:     oldHandler.netName,
		syncStarted: make(chan struct{}),
	}
	newRegisterErr := make(chan error, 1)
	go func() {
		_, err := c.RegisterNetworkController(newHandler)
		newRegisterErr <- err
	}()
	select {
	case <-newHandler.syncStarted:
		close(oldHandler.syncContinue)
		t.Fatal("same-name registration started before failed-generation cleanup")
	case <-time.After(50 * time.Millisecond):
	}

	close(oldHandler.syncContinue)
	if err := <-oldRegisterErr; err == nil {
		t.Fatal("expected old-generation bootstrap failure")
	}
	select {
	case <-newHandler.syncStarted:
	case <-time.After(time.Second):
		t.Fatal("new generation did not start after failed registration cleanup")
	}
	if err := <-newRegisterErr; err != nil {
		t.Fatalf("unexpected new-generation register error: %v", err)
	}
	if c.getNetwork(newHandler.netName) == nil {
		t.Fatal("failed-generation cleanup removed the new network")
	}
	entry := c.entryForTest(newHandler.netName, "ns/pod")
	if entry == nil || entry.lastSeen == nil || entry.lastSeen.UID != newPod.UID {
		t.Fatalf("failed-generation cleanup erased new state: %#v", entry)
	}
}

func TestReconcilePodUIDChangeDeletesBeforeReconcile(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	newerPod := newPod("ns", "pod", "new")
	c := newPodControllerForTest(t, newerPod)
	handler := &fakePodHandler{
		netName: "net-a",
		expect:  map[string]bool{"ns/pod": true},
	}
	c.markApplied(handler.netName, "ns/pod", oldPod, "old-state")

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if handler.deleteCalls != 1 || handler.reconcileCalls != 1 {
		t.Fatalf("expected one delete and one reconcile, got deletes=%d reconciles=%d", handler.deleteCalls, handler.reconcileCalls)
	}
	if got := handler.events; len(got) != 2 || got[0] != "delete:old" || got[1] != "reconcile:new" {
		t.Fatalf("unexpected event order: %#v", got)
	}
	if handler.lastState != "old-state" {
		t.Fatalf("expected old cached state, got %#v", handler.lastState)
	}
	entry := c.entryForTest(handler.netName, "ns/pod")
	if entry == nil || entry.applied == nil || entry.applied.UID != newerPod.UID {
		t.Fatalf("expected applied entry updated to new UID, got %#v", entry)
	}
}

func TestReconcilePodDeleteClearsEntry(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a"}
	c.markApplied(handler.netName, "ns/pod", oldPod, "old-state")

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if handler.deleteCalls != 1 {
		t.Fatalf("expected one delete, got %d", handler.deleteCalls)
	}
	if handler.lastDeletePod.UID != oldPod.UID || handler.lastState != "old-state" {
		t.Fatalf("unexpected delete input pod=%#v state=%#v", handler.lastDeletePod, handler.lastState)
	}
	if entry := c.entryForTest(handler.netName, "ns/pod"); entry != nil {
		t.Fatal("expected entry to be cleared")
	}
}

func TestReconcilePodRecordsUpdateError(t *testing.T) {
	oldPod := newPod("ns", "pod", "uid")
	currentPod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, currentPod)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{"ns/pod": true},
		reconcileErr: errors.New("boom"),
	}
	c.markApplied(handler.netName, "ns/pod", oldPod, "old-state")

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
		t.Fatal("expected reconcile error")
	}

	expected := "ErrorUpdatingResource:uid:boom"
	if len(handler.recordedErrors) != 1 || handler.recordedErrors[0] != expected {
		t.Fatalf("expected recorded error %q, got %#v", expected, handler.recordedErrors)
	}
}

func TestReconcilePodRecordsErrorOncePerFailureStreak(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{"ns/pod": true},
		reconcileErr: errors.New("boom"),
	}

	// Reconciles retry indefinitely; repeated identical failures must not
	// flood the recorder with duplicate events.
	for i := 0; i < 3; i++ {
		if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
			t.Fatal("expected reconcile error")
		}
	}
	if len(handler.recordedErrors) != 1 {
		t.Fatalf("expected one recorded error for repeated failure, got %#v", handler.recordedErrors)
	}

	// A different error is recorded again.
	handler.reconcileErr = errors.New("bang")
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
		t.Fatal("expected reconcile error")
	}
	if len(handler.recordedErrors) != 2 {
		t.Fatalf("expected second recorded error after error change, got %#v", handler.recordedErrors)
	}

	// After a successful reconcile the next failure starts a new streak.
	handler.reconcileErr = nil
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	handler.reconcileErr = errors.New("bang")
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
		t.Fatal("expected reconcile error")
	}
	if len(handler.recordedErrors) != 3 {
		t.Fatalf("expected recorded error after new failure streak, got %#v", handler.recordedErrors)
	}
}

func TestReconcilePodRecordsDeleteError(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{},
		reconcileErr: errors.New("boom"),
	}
	c.markApplied(handler.netName, "ns/pod", oldPod, "old-state")

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
		t.Fatal("expected reconcile error")
	}

	expected := "ErrorDeletingResource:old:boom"
	if len(handler.recordedErrors) != 1 || handler.recordedErrors[0] != expected {
		t.Fatalf("expected recorded error %q, got %#v", expected, handler.recordedErrors)
	}
}

func TestReconcilePodAbsentWithoutEntryDoesNotLeak(t *testing.T) {
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a"}

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.deleteCalls != 0 || len(c.entries) != 0 {
		t.Fatalf("expected no delete and no entries, got deletes=%d entries=%#v", handler.deleteCalls, c.entries)
	}
}

func TestReconcilePodDeleteAfterFailedAddRunsTeardown(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{"ns/pod": true},
		reconcileErr: errors.New("boom"),
	}

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
		t.Fatal("expected reconcile error")
	}

	// The pod is deleted before a retry succeeds. Partially applied state
	// from the failed add must still be torn down.
	c.podLister = newPodLister(t)
	handler.reconcileErr = nil

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.deleteCalls != 1 {
		t.Fatalf("expected teardown after failed add, got %d deletes", handler.deleteCalls)
	}
	if handler.lastDeletePod.UID != pod.UID || handler.lastState != nil {
		t.Fatalf("unexpected delete input pod=%#v state=%#v", handler.lastDeletePod, handler.lastState)
	}
	if entry := c.entryForTest(handler.netName, "ns/pod"); entry != nil {
		t.Fatal("expected entry to be cleared")
	}
}

func TestReconcileCompletedUnexpectedPodWithoutEntryRunsDelete(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	pod.Status.Phase = corev1.PodFailed
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{"ns/pod": false}}

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if handler.deleteCalls != 1 {
		t.Fatalf("expected one delete, got %d", handler.deleteCalls)
	}
	if handler.lastDeletePod.UID != pod.UID || handler.lastState != nil {
		t.Fatalf("unexpected delete input pod=%#v state=%#v", handler.lastDeletePod, handler.lastState)
	}
	if entry := c.entryForTest(handler.netName, "ns/pod"); entry != nil {
		t.Fatal("expected no entry after fallback delete")
	}
}

func TestFallbackDeleteFailureRetainsPodAfterInformerDeletion(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	pod.Status.Phase = corev1.PodFailed
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:      "net-a",
		expect:       map[string]bool{"ns/pod": false},
		reconcileErr: errors.New("delete failed"),
	}

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
		t.Fatal("expected first fallback delete to fail")
	}
	entry := c.entryForTest(handler.netName, "ns/pod")
	if entry == nil || entry.lastSeen == nil || entry.lastSeen.UID != pod.UID {
		t.Fatalf("expected failed fallback delete to retain pod, got %#v", entry)
	}

	c.podLister = newPodLister(t)
	handler.reconcileErr = nil
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected retry error: %v", err)
	}
	if handler.deleteCalls != 1 || handler.lastDeletePod == nil || handler.lastDeletePod.UID != pod.UID {
		t.Fatalf("expected retry to delete retained pod, calls=%d pod=%#v", handler.deleteCalls, handler.lastDeletePod)
	}
	if entry := c.entryForTest(handler.netName, "ns/pod"); entry != nil {
		t.Fatalf("expected successful retry to clear entry, got %#v", entry)
	}
}

func TestReconcileCompletedUnexpectedPodDeleteDedupedByUID(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	pod.Status.Phase = corev1.PodSucceeded
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{netName: "net-a", expect: map[string]bool{"ns/pod": false}}

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.deleteCalls != 1 {
		t.Fatalf("expected first completed reconcile to delete, got %d", handler.deleteCalls)
	}

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.deleteCalls != 1 {
		t.Fatalf("expected repeated completed reconcile to be ignored, got %d deletes", handler.deleteCalls)
	}

	c.podLister = newPodLister(t)
	c.refreshLastSeen(pod)
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.deleteCalls != 1 {
		t.Fatalf("expected delete event for completed pod to be ignored, got %d deletes", handler.deleteCalls)
	}

	recreatedPod := newPod("ns", "pod", "new")
	c.podLister = newPodLister(t, recreatedPod)
	handler.expect["ns/pod"] = true
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if handler.reconcileCalls != 1 {
		t.Fatalf("expected same-name recreate to reconcile, got %d", handler.reconcileCalls)
	}
}

func TestPodNeedsUpdateRefreshesLastSeenForSameUID(t *testing.T) {
	oldPod := newPod("ns", "pod", "uid")
	oldPod.Status.Phase = corev1.PodRunning
	latestPod := oldPod.DeepCopy()
	latestPod.Status.Phase = corev1.PodSucceeded
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a"}
	c.markApplied(handler.netName, "ns/pod", oldPod, "old-state")

	if !c.podNeedsUpdate(oldPod, latestPod) {
		t.Fatal("expected podNeedsUpdate to be true")
	}

	deletePod, state := c.teardownPod(handler.netName, "ns/pod")
	if deletePod == nil || deletePod.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("expected teardown to use latest informer pod, got %#v", deletePod)
	}
	if state != "old-state" {
		t.Fatalf("expected applied state to survive refresh, got %#v", state)
	}
}

func TestPodNeedsUpdateDoesNotRefreshLastSeenForDifferentUID(t *testing.T) {
	oldPod := newPod("ns", "pod", "old")
	replacementPod := newPod("ns", "pod", "new")
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a"}
	c.markApplied(handler.netName, "ns/pod", oldPod, "old-state")

	if !c.podNeedsUpdate(oldPod, replacementPod) {
		t.Fatal("expected podNeedsUpdate to be true")
	}

	deletePod, _ := c.teardownPod(handler.netName, "ns/pod")
	if deletePod == nil || deletePod.UID != oldPod.UID {
		t.Fatalf("expected teardown to keep the applied pod for a different UID, got %#v", deletePod)
	}
}

func TestPodDeletedRefreshesLastSeenForSameUID(t *testing.T) {
	appliedPod := newPod("ns", "pod", "uid")
	appliedPod.Status.Phase = corev1.PodRunning
	deletedPod := appliedPod.DeepCopy()
	deletedPod.Status.Phase = corev1.PodSucceeded
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{netName: "net-a"}
	c.markApplied(handler.netName, "ns/pod", appliedPod, "old-state")

	c.refreshLastSeen(deletedPod)

	deletePod, state := c.teardownPod(handler.netName, "ns/pod")
	if deletePod == nil || deletePod.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("expected teardown to use deleted pod object, got %#v", deletePod)
	}
	if state != "old-state" {
		t.Fatalf("expected applied state to survive delete refresh, got %#v", state)
	}
}

func TestSuccessfulReconcileDoesNotOverwriteNewerTerminalLastSeen(t *testing.T) {
	runningPod := newPod("ns", "pod", "uid")
	runningPod.Status.Phase = corev1.PodRunning
	terminalPod := runningPod.DeepCopy()
	terminalPod.Status.Phase = corev1.PodSucceeded
	c := newPodControllerForTest(t, runningPod)
	handler := &fakePodHandler{
		netName:           "net-a",
		expect:            map[string]bool{"ns/pod": true},
		reconcileStarted:  make(chan struct{}),
		reconcileContinue: make(chan struct{}),
		state:             map[string]interface{}{"ns/pod": "applied-state"},
	}

	reconcileErr := make(chan error, 1)
	go func() {
		reconcileErr <- c.reconcilePodForNetwork(handler, "ns/pod", handler.netName)
	}()
	<-handler.reconcileStarted
	c.refreshLastSeen(terminalPod)
	close(handler.reconcileContinue)
	if err := <-reconcileErr; err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	entry := c.entryForTest(handler.netName, "ns/pod")
	if entry == nil || entry.lastSeen == nil || entry.lastSeen.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("successful reconcile overwrote newer terminal state: %#v", entry)
	}

	c.podLister = newPodLister(t)
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected teardown error: %v", err)
	}
	if handler.lastDeletePod == nil || handler.lastDeletePod.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("expected teardown to use terminal object, got %#v", handler.lastDeletePod)
	}
	if handler.lastState != "applied-state" {
		t.Fatalf("expected teardown to retain applied state, got %#v", handler.lastState)
	}
}

func TestListerSnapshotCannotOverwriteLaterSameUIDObservationAtMarkSeen(t *testing.T) {
	appliedPod := newPod("ns", "pod", "uid")
	listedPod := appliedPod.DeepCopy()
	listedPod.Annotations = map[string]string{"observation": "listed"}
	newerPod := appliedPod.DeepCopy()
	newerPod.Annotations = map[string]string{"observation": "callback"}
	c := newPodControllerForTest(t, listedPod)
	handler := &fakePodHandler{
		netName:          "net-a",
		expect:           map[string]bool{"ns/pod": true},
		expectedStarted:  make(chan struct{}),
		expectedContinue: make(chan struct{}),
	}
	c.markApplied(handler.netName, "ns/pod", appliedPod, "old-state")

	reconcileErr := make(chan error, 1)
	go func() {
		reconcileErr <- c.reconcilePodForNetwork(handler, "ns/pod", handler.netName)
	}()
	<-handler.expectedStarted // lister read has completed
	c.refreshLastSeen(newerPod)
	close(handler.expectedContinue)
	if err := <-reconcileErr; err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	entry := c.entryForTest(handler.netName, "ns/pod")
	if entry == nil || entry.lastSeen == nil || entry.lastSeen.Annotations["observation"] != "callback" {
		t.Fatalf("markSeen overwrote the later informer observation: %#v", entry)
	}
}

func TestFallbackSnapshotCannotOverwriteLaterSameUIDDeleteObservation(t *testing.T) {
	appliedPod := newPod("ns", "pod", "uid")
	listedPod := appliedPod.DeepCopy()
	listedPod.Status.Phase = corev1.PodFailed
	listedPod.Annotations = map[string]string{"observation": "listed"}
	newerDeletePod := appliedPod.DeepCopy()
	newerDeletePod.Status.Phase = corev1.PodSucceeded
	newerDeletePod.Annotations = map[string]string{"observation": "callback"}
	c := newPodControllerForTest(t, listedPod)
	handler := &fakePodHandler{
		netName:          "net-a",
		expect:           map[string]bool{"ns/pod": false},
		expectedStarted:  make(chan struct{}),
		expectedContinue: make(chan struct{}),
	}
	c.markApplied(handler.netName, "ns/pod", appliedPod, "old-state")

	reconcileErr := make(chan error, 1)
	go func() {
		reconcileErr <- c.reconcilePodForNetwork(handler, "ns/pod", handler.netName)
	}()
	<-handler.expectedStarted // lister read has completed
	c.rememberDeletedPod(newerDeletePod)
	close(handler.expectedContinue)
	if err := <-reconcileErr; err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if handler.lastDeletePod == nil || handler.lastDeletePod.Annotations["observation"] != "callback" {
		t.Fatalf("fallback snapshot overwrote later delete observation: %#v", handler.lastDeletePod)
	}
	if handler.lastState != "old-state" {
		t.Fatalf("expected applied state during delete, got %#v", handler.lastState)
	}
}

func TestDeregisterWaitsForInFlightRelatedPodLookup(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:         "net-a",
		relatedStarted:  make(chan struct{}),
		relatedContinue: make(chan struct{}),
	}
	if _, err := c.RegisterNetworkController(handler); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}

	relatedDone := make(chan error, 1)
	go func() {
		relatedDone <- c.reconcileRelatedPods("ns/pod")
	}()
	<-handler.relatedStarted

	deregisterDone := make(chan struct{})
	go func() {
		c.DeregisterNetworkController(handler.netName)
		close(deregisterDone)
	}()
	select {
	case <-deregisterDone:
		t.Fatal("deregistration returned while RelatedPodKeys was in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(handler.relatedContinue)
	if err := <-relatedDone; err != nil {
		t.Fatalf("unexpected related-pod reconcile error: %v", err)
	}
	select {
	case <-deregisterDone:
	case <-time.After(time.Second):
		t.Fatal("deregistration did not finish after RelatedPodKeys returned")
	}
}

func TestDeleteRelatedPodLookupFailureRetainsEntryAndRetries(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t)
	handler := &fakePodHandler{
		netName:    "net-a",
		relatedErr: errors.New("related lookup failed"),
	}
	c.markApplied(handler.netName, "ns/pod", pod, "old-state")

	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err == nil {
		t.Fatal("expected related-pod lookup failure")
	}
	if entry := c.entryForTest(handler.netName, "ns/pod"); entry == nil {
		t.Fatal("expected failed related-pod lookup to retain delete state")
	}
	if handler.deleteCalls != 1 {
		t.Fatalf("expected first idempotent delete, got %d", handler.deleteCalls)
	}

	handler.relatedErr = nil
	handler.relatedKeys = []string{"ns/related"}
	if err := c.reconcilePodForNetwork(handler, "ns/pod", handler.netName); err != nil {
		t.Fatalf("unexpected retry error: %v", err)
	}
	if handler.deleteCalls != 1 {
		t.Fatalf("successful teardown must not be replayed after related lookup failure, got %d calls", handler.deleteCalls)
	}
	if handler.relatedCalls != 2 {
		t.Fatalf("expected related lookup to be retried, got %d calls", handler.relatedCalls)
	}
	if entry := c.entryForTest(handler.netName, "ns/pod"); entry != nil {
		t.Fatalf("expected successful related lookup to clear entry, got %#v", entry)
	}
}

func TestStopClosesInitialBarrierAndPreventsRestart(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{netName: "net-a"}
	done, err := c.RegisterNetworkController(handler)
	if err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	if isClosed(done) {
		t.Fatal("expected initial barrier to remain open before first attempt")
	}

	c.Stop()
	if !isClosed(done) {
		t.Fatal("expected Stop to close initial-attempt barrier")
	}
	if c.getNetwork(handler.netName) != nil {
		t.Fatal("expected Stop to remove registered network")
	}
	if err := c.Start(); err == nil {
		t.Fatal("expected Start after Stop to fail for one-shot controller")
	}
	if _, err := c.RegisterNetworkController(&fakePodHandler{netName: "net-b"}); err == nil {
		t.Fatal("expected registration after Stop to fail")
	}

	// Stop remains idempotent.
	c.Stop()
}

func TestStopWaitsForBlockedNetworkBootstrap(t *testing.T) {
	pod := newPod("ns", "pod", "uid")
	c := newPodControllerForTest(t, pod)
	handler := &fakePodHandler{
		netName:      "net-a",
		syncStarted:  make(chan struct{}),
		syncContinue: make(chan struct{}),
	}

	registerErr := make(chan error, 1)
	go func() {
		_, err := c.RegisterNetworkController(handler)
		registerErr <- err
	}()
	<-handler.syncStarted

	stopDone := make(chan struct{})
	go func() {
		c.Stop()
		close(stopDone)
	}()
	// Stop removes the registration before waiting on the bootstrap gate.
	deadline := time.After(time.Second)
	for c.getNetwork(handler.netName) != nil {
		select {
		case <-deadline:
			t.Fatal("Stop did not begin closing the bootstrapping network")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case <-stopDone:
		t.Fatal("Stop returned while SyncPods was still in flight")
	default:
	}

	close(handler.syncContinue)
	if err := <-registerErr; err == nil {
		t.Fatal("expected registration canceled by Stop to fail")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after SyncPods completed")
	}
}

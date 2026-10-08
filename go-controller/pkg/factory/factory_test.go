// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	ipamclaimsapifake "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/clientset/versioned/fake"
	nadsfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	ocpconfigapi "github.com/openshift/api/config/v1"
	ocpcloudnetworkclientsetfake "github.com/openshift/client-go/cloudnetwork/clientset/versioned/fake"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpapifake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressfirewall "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressip "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressqos "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressqosfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"
	egressservice "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressservicefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	networkqos "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	networkqosfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned/fake"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestFactory(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Watch Factory Suite")
}

// shutdownQueueMap stops queue workers and fails if they do not exit promptly.
func shutdownQueueMap(t *testing.T, queueMap *queueMap) {
	t.Helper()
	queueMap.shutdown()
	done := make(chan struct{})
	go func() {
		queueMap.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queue worker did not shut down")
	}
}

// blockingQueueMapQueue pauses queue acceptance so tests can control shutdown ordering.
type blockingQueueMapQueue struct {
	workqueue.TypedInterface[types.NamespacedName]
	addStarted     chan struct{}
	allowAdd       chan struct{}
	shutdownCalled chan struct{}
	shutdownOnce   sync.Once
}

// Add pauses queue acceptance so the test can race an enqueue with shutdown.
func (q *blockingQueueMapQueue) Add(item types.NamespacedName) {
	close(q.addStarted)
	<-q.allowAdd
	q.TypedInterface.Add(item)
}

// ShutDown signals the blocked Add and shuts down the wrapped queue.
func (q *blockingQueueMapQueue) ShutDown() {
	q.shutdownOnce.Do(func() {
		close(q.shutdownCalled)
	})
	q.TypedInterface.ShutDown()
}

// shutdownRaceQueueMapQueue pauses shutdown and ShuttingDown so tests can expose their ordering.
type shutdownRaceQueueMapQueue struct {
	workqueue.TypedInterface[types.NamespacedName]
	shutdownStarted       chan struct{}
	allowShutdown         chan struct{}
	shuttingDownChecked   chan struct{}
	allowShuttingDownRead chan struct{}
	shutdownOnce          sync.Once
	shuttingDownOnce      sync.Once
}

// ShutDown pauses queue shutdown until the test releases it.
func (q *shutdownRaceQueueMapQueue) ShutDown() {
	q.shutdownOnce.Do(func() { close(q.shutdownStarted) })
	<-q.allowShutdown
	q.TypedInterface.ShutDown()
}

// ShuttingDown pauses the state check to expose shutdown ordering races.
func (q *shutdownRaceQueueMapQueue) ShuttingDown() bool {
	shuttingDown := q.TypedInterface.ShuttingDown()
	q.shuttingDownOnce.Do(func() { close(q.shuttingDownChecked) })
	<-q.allowShuttingDownRead
	return shuttingDown
}

// queueMapGetGate pauses the first Get so tests can queue events before delivery starts.
type queueMapGetGate struct {
	workqueue.TypedInterface[types.NamespacedName]
	firstGet   chan struct{}
	releaseGet chan struct{}
	getOnce    sync.Once
}

// Get blocks the first read until the test releases queued events.
func (q *queueMapGetGate) Get() (types.NamespacedName, bool) {
	q.getOnce.Do(func() {
		close(q.firstGet)
		<-q.releaseGet
	})
	return q.TypedInterface.Get()
}

// TestQueueMapCoalescesRapidUpdates checks that a burst keeps the first old object and latest new object.
func TestQueueMapCoalescesRapidUpdates(t *testing.T) {
	const unrelatedAnnotation = "argocd.argoproj.io/managed-by"
	queueMap := newQueueMap(1, 1, &sync.WaitGroup{}, make(chan struct{}))
	oldNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test",
			UID:  types.UID("uid"),
			Annotations: map[string]string{
				util.AclLoggingAnnotation: "{\"deny\":\"alert\"}",
			},
		},
	}
	queueMap.enqueueEvent(nil, oldNamespace, NamespaceType, false, func(*event) {})
	firstNewNamespace := oldNamespace.DeepCopy()
	firstNewNamespace.ResourceVersion = "1"
	firstNewNamespace.Annotations[unrelatedAnnotation] = "revision-1"
	queueMap.enqueueEvent(oldNamespace, firstNewNamespace, NamespaceType, false, func(*event) {})

	for resourceVersion := 2; resourceVersion <= 1001; resourceVersion++ {
		newNamespace := firstNewNamespace.DeepCopy()
		newNamespace.ResourceVersion = strconv.Itoa(resourceVersion)
		newNamespace.Annotations[unrelatedAnnotation] = fmt.Sprintf("revision-%d", resourceVersion)
		queueMap.enqueueEvent(firstNewNamespace, newNamespace, NamespaceType, false, func(*event) {})
		firstNewNamespace = newNamespace
	}
	finalNamespace := firstNewNamespace.DeepCopy()
	finalNamespace.ResourceVersion = "1002"
	finalNamespace.Annotations[util.AclLoggingAnnotation] = "{\"deny\":\"warning\"}"
	queueMap.enqueueEvent(firstNewNamespace, finalNamespace, NamespaceType, false, func(*event) {})

	if got := queueMap.queue.Len(); got != 1 {
		t.Fatalf("queue length = %d, want 1", got)
	}

	key := types.NamespacedName{Name: "test"}
	entry := queueMap.entries[key]
	if entry == nil {
		t.Fatal("queue map entry was not created")
	}
	if got := len(entry.pending); got != 2 {
		t.Fatalf("pending event count = %d, want add plus one coalesced update", got)
	}
	if got := entry.pending[0].eventType; got != eventTypeAdd {
		t.Fatalf("first pending event type = %v, want add", got)
	}
	if got := entry.pending[1].oldObj.(*corev1.Namespace).ResourceVersion; got != "" {
		t.Errorf("coalesced event old ResourceVersion = %q, want empty", got)
	}
	if got := entry.pending[1].obj.(*corev1.Namespace).ResourceVersion; got != "1002" {
		t.Errorf("coalesced event ResourceVersion = %q, want 1002", got)
	}
	if got := entry.pending[1].oldObj.(*corev1.Namespace).Annotations[util.AclLoggingAnnotation]; got != "{\"deny\":\"alert\"}" {
		t.Errorf("coalesced event old ACL logging annotation = %q, want alert", got)
	}
	if got := entry.pending[1].obj.(*corev1.Namespace).Annotations[util.AclLoggingAnnotation]; got != "{\"deny\":\"warning\"}" {
		t.Errorf("coalesced event new ACL logging annotation = %q, want warning", got)
	}
	if got := entry.pending[1].obj.(*corev1.Namespace).Annotations[unrelatedAnnotation]; got != "revision-1001" {
		t.Errorf("coalesced event unrelated annotation = %q, want latest revision", got)
	}
}

// TestQueueMapDefersFilterChecksUntilUpdatesCanCoalesce avoids filter work when an update cannot coalesce.
func TestQueueMapDefersFilterChecksUntilUpdatesCanCoalesce(t *testing.T) {
	queueMap := newQueueMap(1, 1, &sync.WaitGroup{}, make(chan struct{}))
	transitionChecks := 0
	filterTransition := func(_, _ interface{}) bool {
		transitionChecks++
		return false
	}
	newNamespace := func(name, resourceVersion string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			UID:             types.UID("uid"),
			ResourceVersion: resourceVersion,
		}}
	}
	first := newNamespace("coalesce", "1")
	second := newNamespace("coalesce", "2")
	third := newNamespace("coalesce", "3")

	if coalesced := queueMap.enqueueEventWithFilterTransition(first, second, NamespaceType, false, filterTransition, func(*event) {}); coalesced {
		t.Fatal("first update coalesced without a pending update")
	}
	if transitionChecks != 0 {
		t.Fatalf("filter transitions checked %d times with an empty queue, want 0", transitionChecks)
	}
	if coalesced := queueMap.enqueueEventWithFilterTransition(second, third, NamespaceType, false, filterTransition, func(*event) {}); !coalesced {
		t.Fatal("second consecutive update did not coalesce")
	}
	if transitionChecks != 2 {
		t.Fatalf("filter transitions checked %d times for a coalescible pair, want 2", transitionChecks)
	}
	entry := queueMap.entries[types.NamespacedName{Name: "coalesce"}]
	if entry == nil {
		t.Fatal("queue map entry was not created")
	}
	if got := len(entry.pending); got != 1 {
		t.Fatalf("pending update count = %d, want 1", got)
	}
	if got := entry.pending[0].obj.(*corev1.Namespace).ResourceVersion; got != "3" {
		t.Errorf("coalesced update ResourceVersion = %q, want 3", got)
	}

	added := newNamespace("add-tail", "1")
	queueMap.enqueueEvent(nil, added, NamespaceType, false, func(*event) {})
	updated := newNamespace("add-tail", "2")
	queueMap.enqueueEventWithFilterTransition(added, updated, NamespaceType, false, filterTransition, func(*event) {})
	if transitionChecks != 2 {
		t.Fatalf("filter transitions checked %d times after an add tail, want 2", transitionChecks)
	}
	deleted := newNamespace("delete-tail", "1")
	queueMap.enqueueEvent(nil, deleted, NamespaceType, true, func(*event) {})
	updatedAfterDelete := newNamespace("delete-tail", "2")
	queueMap.enqueueEventWithFilterTransition(deleted, updatedAfterDelete, NamespaceType, false, filterTransition, func(*event) {})
	if transitionChecks != 2 {
		t.Fatalf("filter transitions checked %d times after a delete tail, want 2", transitionChecks)
	}
}

// TestQueueMapPreservesFilteredExitCallbacks covers filter exits adjacent to adds and deletes.
func TestQueueMapPreservesFilteredExitCallbacks(t *testing.T) {
	type callback struct {
		kind            string
		resourceVersion string
	}
	testCases := []struct {
		name    string
		enqueue func(*queueMap, *corev1.Namespace, *corev1.Namespace, func(*event))
		want    []callback
	}{
		{
			name: "add followed by filter-exit update",
			enqueue: func(qm *queueMap, matching, notMatching *corev1.Namespace, process func(*event)) {
				qm.enqueueEvent(nil, matching, NamespaceType, false, process)
				qm.enqueueEvent(matching, notMatching, NamespaceType, false, process)
			},
			want: []callback{{kind: "add", resourceVersion: "1"}, {kind: "delete", resourceVersion: "1"}},
		},
		{
			name: "filter-exit update followed by delete",
			enqueue: func(qm *queueMap, matching, notMatching *corev1.Namespace, process func(*event)) {
				qm.enqueueEvent(matching, notMatching, NamespaceType, false, process)
				qm.enqueueEvent(nil, notMatching, NamespaceType, true, process)
			},
			want: []callback{{kind: "delete", resourceVersion: "1"}},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			queueMap := newQueueMap(1, 1, &sync.WaitGroup{}, make(chan struct{}))
			stop := stopQueueMapOnCleanup(t, queueMap, nil)
			callbacks := make(chan callback, 2)
			processed := make(chan struct{}, 2)
			filtered := cache.FilteringResourceEventHandler{
				FilterFunc: func(obj interface{}) bool {
					return obj.(*corev1.Namespace).Labels["selected"] == "yes"
				},
				Handler: cache.ResourceEventHandlerFuncs{
					AddFunc: func(obj interface{}) {
						callbacks <- callback{kind: "add", resourceVersion: obj.(*corev1.Namespace).ResourceVersion}
					},
					UpdateFunc: func(_, obj interface{}) {
						callbacks <- callback{kind: "update", resourceVersion: obj.(*corev1.Namespace).ResourceVersion}
					},
					DeleteFunc: func(obj interface{}) {
						callbacks <- callback{kind: "delete", resourceVersion: obj.(*corev1.Namespace).ResourceVersion}
					},
				},
			}
			process := func(e *event) {
				switch e.eventType {
				case eventTypeAdd:
					filtered.OnAdd(e.obj, false)
				case eventTypeUpdate:
					filtered.OnUpdate(e.oldObj, e.obj)
				case eventTypeDelete:
					filtered.OnDelete(e.obj)
				}
				processed <- struct{}{}
			}

			matching := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: "test", UID: types.UID("uid"), ResourceVersion: "1",
				Labels: map[string]string{"selected": "yes"},
			}}
			notMatching := matching.DeepCopy()
			notMatching.ResourceVersion = "2"
			notMatching.Labels["selected"] = "no"
			testCase.enqueue(queueMap, matching, notMatching, process)
			queueMap.start()

			for i := 0; i < 2; i++ {
				select {
				case <-processed:
				case <-time.After(time.Second):
					t.Fatalf("timed out waiting for queued event %d", i+1)
				}
			}
			stop()

			if got := len(callbacks); got != len(testCase.want) {
				t.Fatalf("filtered callback count = %d, want %d", got, len(testCase.want))
			}
			for i, want := range testCase.want {
				if got := <-callbacks; got != want {
					t.Errorf("filtered callback %d = %+v, want %+v", i, got, want)
				}
			}
		})
	}
}

// TestFederatedHandlerPreservesFilterExitAndReentry keeps matching-to-nonmatching-to-matching events distinct.
func TestFederatedHandlerPreservesFilterExitAndReentry(t *testing.T) {
	type callback struct {
		kind            string
		resourceVersion string
	}

	var wg sync.WaitGroup
	queueMap := newQueueMap(1, 1, &wg, make(chan struct{}))
	queueGate := &queueMapGetGate{
		TypedInterface: queueMap.queue,
		firstGet:       make(chan struct{}),
		releaseGet:     make(chan struct{}),
	}
	queueMap.queue = queueGate
	intInf := &internalInformer{queueMap: queueMap}
	callbacks := make(chan callback, 2)
	handler := &Handler{
		base: cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				return obj.(*corev1.Namespace).Labels["selected"] == "yes"
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					callbacks <- callback{kind: "add", resourceVersion: obj.(*corev1.Namespace).ResourceVersion}
				},
				UpdateFunc: func(_, obj interface{}) {
					callbacks <- callback{kind: "update", resourceVersion: obj.(*corev1.Namespace).ResourceVersion}
				},
				DeleteFunc: func(obj interface{}) {
					callbacks <- callback{kind: "delete", resourceVersion: obj.(*corev1.Namespace).ResourceVersion}
				},
			},
		},
		tombstone: handlerAlive,
	}
	intInf.Lock()
	intInf.handlers = map[int]map[uint64]*Handler{0: {1: handler}}
	intInf.Unlock()
	intInf.addCoalescingFilter(handler.id, handler.FilterFunc)
	atomic.StoreUint32(&intInf.hasHandlers, hasHandler)
	informer := &informer{oType: NamespaceType, internalInformers: []*internalInformer{intInf}}

	var releaseOnce sync.Once
	releaseQueueGet := func() {
		releaseOnce.Do(func() { close(queueGate.releaseGet) })
	}
	queueMap.start()
	stop := stopQueueMapOnCleanup(t, queueMap, releaseQueueGet)
	select {
	case <-queueGate.firstGet:
	case <-time.After(time.Second):
		t.Fatal("queue worker did not reach the blocked get")
	}

	matching := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "test", UID: types.UID("uid"), ResourceVersion: "1",
		Labels: map[string]string{"selected": "yes"},
	}}
	notMatching := matching.DeepCopy()
	notMatching.ResourceVersion = "2"
	notMatching.Labels["selected"] = "no"
	matchingAgain := notMatching.DeepCopy()
	matchingAgain.ResourceVersion = "3"
	matchingAgain.Labels["selected"] = "yes"
	queuedHandler := informer.newFederatedQueuedHandler(0)
	queuedHandler.OnUpdate(matching, notMatching)
	queuedHandler.OnUpdate(notMatching, matchingAgain)
	releaseQueueGet()

	for i, want := range []callback{{kind: "delete", resourceVersion: "1"}, {kind: "add", resourceVersion: "3"}} {
		select {
		case got := <-callbacks:
			if got != want {
				t.Errorf("filtered callback %d = %+v, want %+v", i, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for filtered callback %d (%s)", i, want.kind)
		}
	}
	stop()
}

// TestHandlerRegistrationDoesNotCoalesceSnapshotFilterExit verifies that queued
// filter transitions run after the initial snapshot and leave no stale state.
func TestHandlerRegistrationDoesNotCoalesceSnapshotFilterExit(t *testing.T) {
	var wg sync.WaitGroup
	queueMap := newQueueMap(1, 1, &wg, make(chan struct{}))
	queueGate := &queueMapGetGate{
		TypedInterface: queueMap.queue,
		firstGet:       make(chan struct{}),
		releaseGet:     make(chan struct{}),
	}
	queueMap.queue = queueGate
	intInf := &internalInformer{handlers: make(map[int]map[uint64]*Handler), queueMap: queueMap}
	notMatching := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "test", UID: types.UID("uid"), ResourceVersion: "1",
		Labels: map[string]string{"selected": "no"},
	}}
	sharedInformer := cache.NewSharedIndexInformer(nil, &corev1.Namespace{}, 0, cache.Indexers{})
	if err := sharedInformer.GetStore().Add(notMatching); err != nil {
		t.Fatalf("failed to seed informer store: %v", err)
	}
	testInformer := &informer{
		oType: NamespaceType,
		inf:   sharedInformer,
		initialAddFunc: func(handler *Handler, items []interface{}) {
			for _, item := range items {
				handler.OnAdd(item, false)
			}
		},
		internalInformers: []*internalInformer{intInf},
	}
	wf := &WatchFactory{
		handlerCounter:        &handlerCounter{},
		informers:             map[reflect.Type]*informer{NamespaceType: testInformer},
		internalInformerIndex: 0,
	}
	callbacks := make(chan string, 4)
	var stateMu sync.Mutex
	ownedState := make(map[string]*corev1.Namespace)
	handlerFuncs := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			namespace := obj.(*corev1.Namespace)
			stateMu.Lock()
			ownedState[namespace.Name] = namespace.DeepCopy()
			stateMu.Unlock()
			callbacks <- "add:" + namespace.ResourceVersion
		},
		UpdateFunc: func(_, obj interface{}) {
			namespace := obj.(*corev1.Namespace)
			stateMu.Lock()
			ownedState[namespace.Name] = namespace.DeepCopy()
			stateMu.Unlock()
			callbacks <- "update:" + namespace.ResourceVersion
		},
		DeleteFunc: func(obj interface{}) {
			namespace := obj.(*corev1.Namespace)
			stateMu.Lock()
			delete(ownedState, namespace.Name)
			stateMu.Unlock()
			callbacks <- "delete:" + namespace.ResourceVersion
		},
	}
	queueMap.start()
	var releaseQueueOnce sync.Once
	releaseQueue := func() { releaseQueueOnce.Do(func() { close(queueGate.releaseGet) }) }
	stop := stopQueueMapOnCleanup(t, queueMap, releaseQueue)
	select {
	case <-queueGate.firstGet:
	case <-time.After(time.Second):
		t.Fatal("queue worker did not reach the blocked get")
	}

	snapshotStarted := make(chan []interface{}, 1)
	releaseSnapshot := make(chan struct{})
	var releaseSnapshotOnce sync.Once
	finishSnapshot := func() { releaseSnapshotOnce.Do(func() { close(releaseSnapshot) }) }
	t.Cleanup(finishSnapshot)
	registrationDone := make(chan error, 1)
	go func() {
		_, err := wf.addHandler(NamespaceType, "", labels.Set{"selected": "yes"}.AsSelector(), handlerFuncs,
			func(items []interface{}) error {
				snapshotStarted <- items
				<-releaseSnapshot
				return nil
			}, defaultHandlerPriority)
		registrationDone <- err
	}()
	var snapshot []interface{}
	select {
	case snapshot = <-snapshotStarted:
	case <-time.After(time.Second):
		t.Fatal("handler registration did not reach the blocked initial snapshot")
	}
	if len(snapshot) != 0 {
		t.Fatalf("initial matching snapshot contains %d objects, want none", len(snapshot))
	}
	queuedHandler := testInformer.newFederatedQueuedHandler(0)
	matching := notMatching.DeepCopy()
	matching.ResourceVersion = "2"
	matching.Labels["selected"] = "yes"
	notMatchingAgain := matching.DeepCopy()
	notMatchingAgain.ResourceVersion = "3"
	notMatchingAgain.Labels["selected"] = "no"
	notMatchingAgain2 := notMatchingAgain.DeepCopy()
	notMatchingAgain2.ResourceVersion = "4"
	notMatchingAgain3 := notMatchingAgain.DeepCopy()
	notMatchingAgain3.ResourceVersion = "5"

	// addHandler holds intInf's write lock while this callback is blocked. Its
	// filter has already been registered, so these membership transitions must
	// stay distinct even though later same-membership updates can be coalesced.
	for _, update := range [][2]*corev1.Namespace{
		{notMatching, matching},
		{matching, notMatchingAgain},
		{notMatchingAgain, notMatchingAgain2},
		{notMatchingAgain2, notMatchingAgain3},
	} {
		if err := sharedInformer.GetStore().Update(update[1]); err != nil {
			t.Fatalf("failed to update informer store to rv=%s: %v", update[1].ResourceVersion, err)
		}
		queuedHandler.OnUpdate(update[0], update[1])
	}

	key := types.NamespacedName{Name: notMatching.Name}
	queueMap.Lock()
	entry := queueMap.entries[key]
	if entry == nil {
		queueMap.Unlock()
		t.Fatal("registration updates were not queued")
	}
	pending := append([]*event(nil), entry.pending...)
	queueMap.Unlock()
	if len(pending) != 3 {
		t.Fatalf("pending event count = %d, want two filter transitions and one coalesced update", len(pending))
	}
	if !pending[0].filterTransition || !pending[1].filterTransition {
		t.Fatal("filter membership transitions were not marked for coalescing protection")
	}
	if got := pending[2].obj.(*corev1.Namespace).ResourceVersion; got != "5" {
		t.Errorf("coalesced non-matching update resourceVersion = %q, want latest rv=5", got)
	}

	finishSnapshot()
	select {
	case err := <-registrationDone:
		if err != nil {
			t.Fatalf("handler registration failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handler registration did not complete after releasing the snapshot")
	}
	releaseQueue()
	for i, want := range []string{"add:2", "delete:2"} {
		select {
		case got := <-callbacks:
			if got != want {
				t.Errorf("callback %d = %q, want %q", i+1, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for callback %d (%s)", i+1, want)
		}
	}
	stop()

	stateMu.Lock()
	defer stateMu.Unlock()
	if len(ownedState) != 0 {
		t.Fatalf("handler retained stale state for %d namespaces after filter exit: %+v", len(ownedState), ownedState)
	}
}

// TestQueueMapDoesNotCoalesceAddsForDifferentUIDs retains separate adds for reused names.
func TestQueueMapDoesNotCoalesceAddsForDifferentUIDs(t *testing.T) {
	queueMap := newQueueMap(1, 1, &sync.WaitGroup{}, make(chan struct{}))
	oldNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test",
			UID:  types.UID("old-uid"),
		},
	}
	newNamespace := oldNamespace.DeepCopy()
	newNamespace.UID = types.UID("new-uid")

	queueMap.enqueueEvent(nil, oldNamespace, NamespaceType, false, func(*event) {})
	queueMap.enqueueEvent(nil, newNamespace, NamespaceType, false, func(*event) {})

	entry := queueMap.entries[types.NamespacedName{Name: "test"}]
	if entry == nil {
		t.Fatal("queue map entry was not created")
	}
	if got := len(entry.pending); got != 2 {
		t.Fatalf("pending event count = %d, want 2", got)
	}
	if got := entry.pending[0].obj.(*corev1.Namespace).UID; got != types.UID("old-uid") {
		t.Errorf("first pending UID = %q, want old-uid", got)
	}
	if got := entry.pending[1].obj.(*corev1.Namespace).UID; got != types.UID("new-uid") {
		t.Errorf("second pending UID = %q, want new-uid", got)
	}
}

// TestQueueMapUpdateDoesNotSuppressOtherKeyLifecycleEvents checks that
// coalescing one key leaves another key's lifecycle events intact.
func TestQueueMapUpdateDoesNotSuppressOtherKeyLifecycleEvents(t *testing.T) {
	var wg sync.WaitGroup
	queueMap := newQueueMap(1, 2, &wg, make(chan struct{}))
	first := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: types.UID("uid-a")}}
	callbacks := make(chan string, 4)
	process := func(e *event) {
		name := e.obj.(*corev1.Namespace).Name
		switch e.eventType {
		case eventTypeAdd:
			callbacks <- "add:" + name
		case eventTypeUpdate:
			callbacks <- "update:" + name + ":" + e.obj.(*corev1.Namespace).ResourceVersion
		case eventTypeDelete:
			callbacks <- "delete:" + name
		}
	}

	for rv := 1; rv <= 100; rv++ {
		updated := first.DeepCopy()
		updated.ResourceVersion = strconv.Itoa(rv)
		queueMap.enqueueEvent(first, updated, NamespaceType, false, process)
		first = updated
	}
	other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "b", UID: types.UID("uid-b")}}
	queueMap.enqueueEvent(nil, other, NamespaceType, false, process)
	queueMap.enqueueEvent(nil, other, NamespaceType, true, process)

	queueMap.start()
	stop := stopQueueMapOnCleanup(t, queueMap, nil)
	stop()

	want := map[string]int{
		"update:a:100": 1,
		"add:b":        1,
		"delete:b":     1,
	}
	gotOrder := make([]string, 0, len(want))
	for len(callbacks) > 0 {
		got := <-callbacks
		gotOrder = append(gotOrder, got)
		if _, ok := want[got]; !ok {
			t.Errorf("unexpected callback %q", got)
			continue
		}
		want[got]--
	}
	for callback, count := range want {
		if count != 0 {
			t.Errorf("callback %q delivered %d times, want once", callback, 1-count)
		}
	}
	addIndex, deleteIndex := -1, -1
	for i, callback := range gotOrder {
		switch callback {
		case "add:b":
			addIndex = i
		case "delete:b":
			deleteIndex = i
		}
	}
	if addIndex < 0 || deleteIndex < 0 || addIndex > deleteIndex {
		t.Errorf("key b lifecycle callbacks out of order: %v", gotOrder)
	}
}

// TestQueueMapPreservesUIDReplacementUpdates delivers each replacement in delete/add order.
func TestQueueMapPreservesUIDReplacementUpdates(t *testing.T) {
	var wg sync.WaitGroup
	queueMap := newQueueMap(1, 1, &wg, make(chan struct{}))
	oldNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test",
			UID:  types.UID("old-uid"),
		},
	}
	firstReplacement := oldNamespace.DeepCopy()
	firstReplacement.UID = types.UID("first-replacement-uid")
	secondReplacement := firstReplacement.DeepCopy()
	secondReplacement.UID = types.UID("second-replacement-uid")

	callbacks := make(chan string, 4)
	process := func(e *event) {
		oldUID := e.oldObj.(metav1.Object).GetUID()
		newUID := e.obj.(metav1.Object).GetUID()
		if oldUID != newUID {
			callbacks <- "delete:" + string(oldUID)
			callbacks <- "add:" + string(newUID)
		}
	}
	queueMap.enqueueEvent(oldNamespace, firstReplacement, NamespaceType, false, process)
	queueMap.enqueueEvent(firstReplacement, secondReplacement, NamespaceType, false, process)

	entry := queueMap.entries[types.NamespacedName{Name: "test"}]
	if entry == nil {
		t.Fatal("queue map entry was not created")
	}
	if got := len(entry.pending); got != 2 {
		t.Fatalf("pending event count = %d, want 2", got)
	}
	if got := entry.pending[0].oldObj.(*corev1.Namespace).UID; got != types.UID("old-uid") {
		t.Errorf("first replacement old UID = %q, want old-uid", got)
	}
	if got := entry.pending[0].obj.(*corev1.Namespace).UID; got != types.UID("first-replacement-uid") {
		t.Errorf("first replacement new UID = %q, want first-replacement-uid", got)
	}
	if got := entry.pending[1].oldObj.(*corev1.Namespace).UID; got != types.UID("first-replacement-uid") {
		t.Errorf("second replacement old UID = %q, want first-replacement-uid", got)
	}
	if got := entry.pending[1].obj.(*corev1.Namespace).UID; got != types.UID("second-replacement-uid") {
		t.Errorf("second replacement new UID = %q, want second-replacement-uid", got)
	}
	queueMap.start()
	stop := stopQueueMapOnCleanup(t, queueMap, nil)
	for _, want := range []string{
		"delete:old-uid",
		"add:first-replacement-uid",
		"delete:first-replacement-uid",
		"add:second-replacement-uid",
	} {
		select {
		case got := <-callbacks:
			if got != want {
				t.Errorf("replacement callback = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for replacement callback %q", want)
		}
	}
	stop()
}

// TestQueueMapPreservesAddDeleteAndReplacementOrdering covers lifecycle barriers around object recreation.
func TestQueueMapPreservesAddDeleteAndReplacementOrdering(t *testing.T) {
	testCases := []struct {
		name     string
		enqueue  func(*queueMap, *corev1.Namespace, *corev1.Namespace, func(*event))
		expected []string
	}{
		{
			name: "add-delete-add",
			enqueue: func(qm *queueMap, oldObject, newObject *corev1.Namespace, process func(*event)) {
				qm.enqueueEvent(nil, oldObject, NamespaceType, false, process)
				qm.enqueueEvent(nil, oldObject, NamespaceType, true, process)
				qm.enqueueEvent(nil, newObject, NamespaceType, false, process)
			},
			expected: []string{"add:old", "delete:old", "add:new"},
		},
		{
			name: "add-updates-delete-add",
			enqueue: func(qm *queueMap, oldObject, newObject *corev1.Namespace, process func(*event)) {
				firstUpdate := oldObject.DeepCopy()
				firstUpdate.ResourceVersion = "2"
				lastUpdate := firstUpdate.DeepCopy()
				lastUpdate.ResourceVersion = "3"
				qm.enqueueEvent(nil, oldObject, NamespaceType, false, process)
				qm.enqueueEvent(oldObject, firstUpdate, NamespaceType, false, process)
				qm.enqueueEvent(firstUpdate, lastUpdate, NamespaceType, false, process)
				qm.enqueueEvent(nil, lastUpdate, NamespaceType, true, process)
				qm.enqueueEvent(nil, newObject, NamespaceType, false, process)
			},
			expected: []string{"add:old", "update:old:3", "delete:old", "add:new"},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var wg sync.WaitGroup
			queueMap := newQueueMap(1, 1, &wg, make(chan struct{}))
			oldObject := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test", UID: types.UID("old"), ResourceVersion: "1"}}
			newObject := oldObject.DeepCopy()
			newObject.UID = types.UID("new")
			callbacks := make(chan string, 4)
			process := func(e *event) {
				switch e.eventType {
				case eventTypeAdd:
					callbacks <- "add:" + string(e.obj.(metav1.Object).GetUID())
				case eventTypeUpdate:
					callbacks <- "update:" + string(e.obj.(metav1.Object).GetUID()) + ":" + e.obj.(*corev1.Namespace).ResourceVersion
				case eventTypeDelete:
					callbacks <- "delete:" + string(e.obj.(metav1.Object).GetUID())
				}
			}
			testCase.enqueue(queueMap, oldObject, newObject, process)
			queueMap.start()
			stop := stopQueueMapOnCleanup(t, queueMap, nil)
			for i, want := range testCase.expected {
				select {
				case got := <-callbacks:
					if got != want {
						t.Errorf("callback %d = %q, want %q", i+1, got, want)
					}
				case <-time.After(time.Second):
					t.Fatalf("timed out waiting for callback %d (%s)", i+1, want)
				}
			}
			stop()
		})
	}
}

// TestQueueMapPreservesUIDReplacementBeforeDelete ensures shutdown drains the update before its following delete.
func TestQueueMapPreservesUIDReplacementBeforeDelete(t *testing.T) {
	var wg sync.WaitGroup
	queueMap := newQueueMap(1, 1, &wg, make(chan struct{}))
	oldNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test",
			UID:  types.UID("old-uid"),
		},
	}
	newNamespace := oldNamespace.DeepCopy()
	newNamespace.UID = types.UID("new-uid")
	processed := make(chan eventType, 2)
	shutdownStarted := make(chan struct{})
	process := func(e *event) {
		if e.eventType == eventTypeUpdate {
			// Shut down while the first event is being processed so the second
			// event cannot depend on queue.Add after queue.Done.
			queueMap.shutdown()
			close(shutdownStarted)
		}
		processed <- e.eventType
	}

	queueMap.enqueueEvent(oldNamespace, newNamespace, NamespaceType, false, process)
	queueMap.enqueueEvent(nil, newNamespace, NamespaceType, true, process)

	entry := queueMap.entries[types.NamespacedName{Name: "test"}]
	if entry == nil {
		t.Fatal("queue map entry was not created")
	}
	if got := len(entry.pending); got != 2 {
		t.Fatalf("pending event count = %d, want 2", got)
	}
	if got := entry.pending[0].oldObj.(*corev1.Namespace).UID; got != types.UID("old-uid") {
		t.Errorf("replacement old UID = %q, want old-uid", got)
	}
	if got := entry.pending[0].obj.(*corev1.Namespace).UID; got != types.UID("new-uid") {
		t.Errorf("replacement new UID = %q, want new-uid", got)
	}
	if got := entry.pending[1].eventType; got != eventTypeDelete {
		t.Errorf("second event type = %v, want delete", got)
	}

	queueMap.start()
	stop := stopQueueMapOnCleanup(t, queueMap, nil)
	select {
	case <-shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("replacement update was not processed")
	}
	stop()
	for i, want := range []eventType{eventTypeUpdate, eventTypeDelete} {
		select {
		case got := <-processed:
			if got != want {
				t.Errorf("processed event %d = %v, want %v", i, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for processed event %d (%v)", i, want)
		}
	}
}

// TestQueueMapSerializesEnqueueAndShutdown blocks Add to verify shutdown preserves accepted work.
func TestQueueMapSerializesEnqueueAndShutdown(t *testing.T) {
	var wg sync.WaitGroup
	stopChan := make(chan struct{})
	queueMap := newQueueMap(1, 1, &wg, stopChan)
	queue := &blockingQueueMapQueue{
		TypedInterface: queueMap.queue,
		addStarted:     make(chan struct{}),
		allowAdd:       make(chan struct{}),
		shutdownCalled: make(chan struct{}),
	}
	queueMap.queue = queue
	queueMap.start()

	var releaseAddOnce sync.Once
	releaseAdd := func() {
		releaseAddOnce.Do(func() { close(queue.allowAdd) })
	}
	defer func() {
		releaseAdd()
		shutdownQueueMap(t, queueMap)
	}()

	processed := make(chan struct{})
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test", UID: types.UID("uid")}}
	enqueueDone := make(chan struct{})
	go func() {
		queueMap.enqueueEvent(nil, namespace, NamespaceType, false, func(*event) {
			close(processed)
		})
		close(enqueueDone)
	}()

	select {
	case <-queue.addStarted:
	case <-time.After(time.Second):
		t.Fatal("enqueue did not reach queue.Add")
	}
	close(stopChan)

	shutdownDone := make(chan struct{})
	go func() {
		queueMap.shutdown()
		close(shutdownDone)
	}()

	select {
	case <-queue.shutdownCalled:
		t.Fatal("shutdown closed the queue before the pending event was accepted")
	case <-time.After(time.Second):
	}

	releaseAdd()
	select {
	case <-processed:
	case <-time.After(time.Second):
		t.Fatal("accepted event was not processed after concurrent shutdown")
	}
	select {
	case <-enqueueDone:
	case <-time.After(time.Second):
		t.Fatal("enqueue did not finish after queue acceptance")
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after queue acceptance")
	}
}

// TestQueueMapDrainsPendingEventsWhenShutdownRacesRequeue protects the gap
// between marking the queue map stopped and shutting down its workqueue.
func TestQueueMapDrainsPendingEventsWhenShutdownRacesRequeue(t *testing.T) {
	var wg sync.WaitGroup
	queueMap := newQueueMap(1, 1, &wg, make(chan struct{}))
	queue := &shutdownRaceQueueMapQueue{
		TypedInterface:        queueMap.queue,
		shutdownStarted:       make(chan struct{}),
		allowShutdown:         make(chan struct{}),
		shuttingDownChecked:   make(chan struct{}),
		allowShuttingDownRead: make(chan struct{}),
	}
	queueMap.queue = queue
	updateStarted := make(chan struct{})
	releaseUpdate := make(chan struct{})
	deleteProcessed := make(chan struct{})
	var releaseUpdateOnce sync.Once
	releaseUpdateCallback := func() {
		releaseUpdateOnce.Do(func() { close(releaseUpdate) })
	}
	var releaseShutdownOnce sync.Once
	releaseQueueShutdown := func() {
		releaseShutdownOnce.Do(func() { close(queue.allowShutdown) })
	}
	var releaseShuttingDownReadOnce sync.Once
	releaseShuttingDownRead := func() {
		releaseShuttingDownReadOnce.Do(func() { close(queue.allowShuttingDownRead) })
	}
	queueMap.start()
	stop := stopQueueMapOnCleanup(t, queueMap, func() {
		releaseUpdateCallback()
		releaseQueueShutdown()
		releaseShuttingDownRead()
	})

	oldNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test", UID: types.UID("uid"), ResourceVersion: "1"}}
	newNamespace := oldNamespace.DeepCopy()
	newNamespace.ResourceVersion = "2"
	process := func(e *event) {
		switch e.eventType {
		case eventTypeUpdate:
			close(updateStarted)
			<-releaseUpdate
		case eventTypeDelete:
			close(deleteProcessed)
		}
	}
	queueMap.enqueueEvent(oldNamespace, newNamespace, NamespaceType, false, process)
	select {
	case <-updateStarted:
	case <-time.After(time.Second):
		t.Fatal("update callback did not start")
	}
	queueMap.enqueueEvent(nil, newNamespace, NamespaceType, true, process)

	shutdownDone := make(chan struct{})
	go func() {
		queueMap.shutdown()
		close(shutdownDone)
	}()
	select {
	case <-queue.shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("queue map did not begin workqueue shutdown")
	}
	releaseUpdateCallback()

	select {
	case <-deleteProcessed:
		// The worker observed queueMap.stopped and drained the pending delete.
	case <-queue.shuttingDownChecked:
		// Force the old check-then-Add race: shut down the workqueue after the
		// worker observed false but before its Add can requeue the key.
		releaseQueueShutdown()
		select {
		case <-shutdownDone:
		case <-time.After(time.Second):
			t.Fatal("workqueue shutdown did not complete")
		}
		releaseShuttingDownRead()
		select {
		case <-deleteProcessed:
		case <-time.After(time.Second):
			t.Fatal("pending delete was lost while shutdown raced with requeue")
		}
	case <-time.After(time.Second):
		t.Fatal("pending delete was not processed during shutdown")
	}
	releaseQueueShutdown()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("workqueue shutdown did not complete")
	}
	stop()
}

// TestQueueMapProcessesAndShutsDown checks that a callback accepted before stop is drained.
func TestQueueMapProcessesAndShutsDown(t *testing.T) {
	var wg sync.WaitGroup
	queueMap := newQueueMap(1, 1, &wg, make(chan struct{}))
	queueMap.start()
	stop := stopQueueMapOnCleanup(t, queueMap, nil)

	processed := make(chan struct{})
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test", UID: types.UID("uid")}}
	queueMap.enqueueEvent(nil, namespace, NamespaceType, false, func(*event) {
		close(processed)
	})

	select {
	case <-processed:
	case <-time.After(time.Second):
		t.Fatal("queued event was not processed")
	}

	stop()
}

// TestQueueMapCoalescesWhileEventIsProcessing ensures a busy key yields so others are not starved.
func TestQueueMapCoalescesWhileEventIsProcessing(t *testing.T) {
	var wg sync.WaitGroup
	queueMap := newQueueMap(1, 1, &wg, make(chan struct{}))
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	queueMap.start()
	stop := stopQueueMapOnCleanup(t, queueMap, func() {
		releaseOnce.Do(func() { close(release) })
	})
	processed := make(chan string, 4)
	var process func(*event)
	process = func(e *event) {
		switch e.eventType {
		case eventTypeAdd:
			close(entered)
			<-release
			processed <- "hot add"
		case eventTypeUpdate:
			namespace := e.obj.(*corev1.Namespace)
			if namespace.ResourceVersion == "1000" {
				nextNamespace := namespace.DeepCopy()
				nextNamespace.ResourceVersion = "1001"
				queueMap.enqueueEvent(namespace, nextNamespace, NamespaceType, false, process)
			}
			processed <- "hot update " + namespace.ResourceVersion
		}
	}

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test", UID: types.UID("uid")}}
	queueMap.enqueueEvent(nil, namespace, NamespaceType, false, process)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("queued event was not picked up by the worker")
	}

	for resourceVersion := 1; resourceVersion <= 1000; resourceVersion++ {
		newNamespace := namespace.DeepCopy()
		newNamespace.ResourceVersion = strconv.Itoa(resourceVersion)
		queueMap.enqueueEvent(namespace, newNamespace, NamespaceType, false, process)
		namespace = newNamespace
	}

	queueMap.Lock()
	pendingEvents := len(queueMap.entries[types.NamespacedName{Name: "test"}].pending)
	queueMap.Unlock()
	if pendingEvents != 1 {
		t.Fatalf("pending events while handler is blocked = %d, want 1", pendingEvents)
	}
	other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: types.UID("other-uid")}}
	queueMap.enqueueEvent(nil, other, NamespaceType, false, func(*event) { processed <- "other add" })

	releaseOnce.Do(func() { close(release) })
	want := []string{"hot add", "other add", "hot update 1000", "hot update 1001"}
	for i, expected := range want {
		select {
		case got := <-processed:
			if got != expected {
				t.Errorf("callback %d = %q, want %q", i+1, got, expected)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for callback %d: expected %q", i+1, expected)
		}
	}

	stop()
}

func newObjectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		UID:       types.UID(name),
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newPod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "containerName",
					Image: "containerImage",
				},
			},
			NodeName: "mynode",
		},
	}
}

func newNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		Status: corev1.NamespaceStatus{
			Phase: corev1.NamespaceActive,
		},
		ObjectMeta: newObjectMeta(name, name),
	}
}

func newNode(name string) *corev1.Node {
	return &corev1.Node{
		Status: corev1.NodeStatus{
			Phase: corev1.NodeRunning,
		},
		ObjectMeta: newObjectMeta(name, ""),
	}
}

func newPolicy(name, namespace string) *knet.NetworkPolicy {
	return &knet.NetworkPolicy{
		ObjectMeta: newObjectMeta(name, namespace),
	}
}

func newService(name, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			UID:       types.UID(name),
			Namespace: namespace,
			Labels: map[string]string{
				"name": name,
			},
		},
	}
}

func newEndpointSlice(name, namespace, service string) *discovery.EndpointSlice {
	return &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			UID:       types.UID(name),
			Namespace: namespace,
			Labels: map[string]string{
				discovery.LabelServiceName: service,
			},
		},
	}
}

func newEgressFirewall(name, namespace string) *egressfirewall.EgressFirewall {
	return &egressfirewall.EgressFirewall{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: egressfirewall.EgressFirewallSpec{
			Egress: []egressfirewall.EgressFirewallRule{
				{
					Type: egressfirewall.EgressFirewallRuleAllow,
					To: egressfirewall.EgressFirewallDestination{
						CIDRSelector: "1.2.3.4/32",
					},
				},
			},
		},
	}
}

func newEgressIP(name, namespace string) *egressip.EgressIP {
	return &egressip.EgressIP{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: egressip.EgressIPSpec{
			EgressIPs: []string{
				"192.168.126.10",
			},
		},
	}

}

func newCloudPrivateIPConfig(name string) *ocpcloudnetworkapi.CloudPrivateIPConfig {
	return &ocpcloudnetworkapi.CloudPrivateIPConfig{
		ObjectMeta: newObjectMeta(name, ""),
		Spec: ocpcloudnetworkapi.CloudPrivateIPConfigSpec{
			Node: "test-node",
		},
	}
}

func newEgressQoS(name, namespace string) *egressqos.EgressQoS {
	return &egressqos.EgressQoS{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: egressqos.EgressQoSSpec{
			Egress: []egressqos.EgressQoSRule{
				{
					DSCP:    50,
					DstCIDR: ptr.To("1.2.3.4/32"),
				},
			},
		},
	}
}

func newEgressService(name, namespace string) *egressservice.EgressService {
	return &egressservice.EgressService{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: egressservice.EgressServiceSpec{
			SourceIPBy: egressservice.SourceIPLoadBalancer,
			NodeSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"kubernetes.io/hostname": "node",
				},
			},
		},
	}
}

func newAdminNetworkPolicy(name string, priority int32) *anpapi.AdminNetworkPolicy {
	return &anpapi.AdminNetworkPolicy{
		ObjectMeta: newObjectMeta(name, ""),
		Spec: anpapi.AdminNetworkPolicySpec{
			Priority: priority,
			Subject: anpapi.AdminNetworkPolicySubject{
				Namespaces: &metav1.LabelSelector{},
			},
		},
	}
}

func newBaselineAdminNetworkPolicy(name string) *anpapi.BaselineAdminNetworkPolicy {
	return &anpapi.BaselineAdminNetworkPolicy{
		ObjectMeta: newObjectMeta(name, ""),
		Spec: anpapi.BaselineAdminNetworkPolicySpec{
			Subject: anpapi.AdminNetworkPolicySubject{
				Namespaces: &metav1.LabelSelector{},
			},
		},
	}
}

func newIPAMClaim(name string) *ipamclaimsapi.IPAMClaim {
	return &ipamclaimsapi.IPAMClaim{
		ObjectMeta: newObjectMeta(name, ""),
		Spec:       ipamclaimsapi.IPAMClaimSpec{},
	}
}

func newNetworkQoS(name, namespace string) *networkqos.NetworkQoS {
	return &networkqos.NetworkQoS{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: networkqos.Spec{
			NetworkSelectors: []crdtypes.NetworkSelector{
				{
					NetworkSelectionType: crdtypes.NetworkAttachmentDefinitions,
					NetworkAttachmentDefinitionSelector: &crdtypes.NetworkAttachmentDefinitionSelector{
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": "stream",
							},
						},
					},
				},
			},
			Priority: 100,
			Egress: []networkqos.Rule{
				{
					DSCP: 50,
					Classifier: networkqos.Classifier{
						To: []networkqos.Destination{
							{
								IPBlock: &knet.IPBlock{
									CIDR: "1.2.3.4/32",
								},
							},
						},
					},
					Bandwidth: networkqos.Bandwidth{
						Rate:  20000,
						Burst: 10,
					},
				},
			},
		},
	}
}

func objSetup(c *fake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func egressFirewallObjSetup(c *egressfirewallfake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func egressIPObjSetup(c *egressipfake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func cloudPrivateIPConfigObjSetup(c *ocpcloudnetworkclientsetfake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func egressQoSObjSetup(c *egressqosfake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func egressServiceObjSetup(c *egressservicefake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func adminNetworkPolicyObjSetup(c *anpapifake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func ipamClaimsObjSetup(c *ipamclaimsapifake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

func networkQoSObjSetup(c *networkqosfake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

type handlerCalls struct {
	added   int32
	updated int32
	deleted int32
}

func (c *handlerCalls) getAdded() int {
	return int(atomic.LoadInt32(&c.added))
}

func (c *handlerCalls) getUpdated() int {
	return int(atomic.LoadInt32(&c.updated))
}

func (c *handlerCalls) getDeleted() int {
	return int(atomic.LoadInt32(&c.deleted))
}

var _ = Describe("Watch Factory Operations", func() {
	var (
		ovnClientset                        *util.OVNKubeControllerClientset
		ovnCMClientset                      *util.OVNClusterManagerClientset
		ovnNodeClientset                    *util.OVNNodeClientset
		fakeClient                          *fake.Clientset
		egressIPFakeClient                  *egressipfake.Clientset
		egressFirewallFakeClient            *egressfirewallfake.Clientset
		cloudNetworkFakeClient              *ocpcloudnetworkclientsetfake.Clientset
		egressQoSFakeClient                 *egressqosfake.Clientset
		egressServiceFakeClient             *egressservicefake.Clientset
		adminNetworkPolicyFakeClient        *anpapifake.Clientset
		ipamClaimsFakeClient                *ipamclaimsapifake.Clientset
		nadsFakeClient                      *nadsfake.Clientset
		networkQoSFakeClient                *networkqosfake.Clientset
		podWatch, namespaceWatch, nodeWatch *watch.FakeWatcher
		policyWatch, serviceWatch           *watch.FakeWatcher
		endpointSliceWatch                  *watch.FakeWatcher
		egressFirewallWatch                 *watch.FakeWatcher
		egressIPWatch                       *watch.FakeWatcher
		cloudPrivateIPConfigWatch           *watch.FakeWatcher
		egressQoSWatch                      *watch.FakeWatcher
		egressServiceWatch                  *watch.FakeWatcher
		adminNetPolWatch                    *watch.FakeWatcher
		baselineAdminNetPolWatch            *watch.FakeWatcher
		ipamClaimsWatch                     *watch.FakeWatcher
		networkQoSWatch                     *watch.FakeWatcher
		pods                                []*corev1.Pod
		namespaces                          []*corev1.Namespace
		nodes                               []*corev1.Node
		policies                            []*knet.NetworkPolicy
		endpointSlices                      []*discovery.EndpointSlice
		services                            []*corev1.Service
		egressIPs                           []*egressip.EgressIP
		cloudPrivateIPConfigs               []*ocpcloudnetworkapi.CloudPrivateIPConfig
		wf                                  *WatchFactory
		egressFirewalls                     []*egressfirewall.EgressFirewall
		egressQoSes                         []*egressqos.EgressQoS
		egressServices                      []*egressservice.EgressService
		adminNetworkPolicies                []*anpapi.AdminNetworkPolicy
		baselineAdminNetworkPolicies        []*anpapi.BaselineAdminNetworkPolicy
		ipamClaims                          []*ipamclaimsapi.IPAMClaim
		networkQoSes                        []*networkqos.NetworkQoS
		err                                 error
		shutdown                            bool
	)

	const (
		nodeName string = "node1"
	)

	BeforeEach(func() {

		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableEgressIP = true
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		config.OVNKubernetesFeature.EnableEgressQoS = true
		config.OVNKubernetesFeature.EnableEgressService = true
		config.OVNKubernetesFeature.EnableAdminNetworkPolicy = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnablePersistentIPs = true
		config.OVNKubernetesFeature.EnableNetworkQoS = true
		config.Kubernetes.PlatformType = string(ocpconfigapi.AWSPlatformType)

		fakeClient = &fake.Clientset{}
		egressFirewallFakeClient = &egressfirewallfake.Clientset{}
		egressIPFakeClient = &egressipfake.Clientset{}
		cloudNetworkFakeClient = &ocpcloudnetworkclientsetfake.Clientset{}
		egressQoSFakeClient = &egressqosfake.Clientset{}
		egressServiceFakeClient = &egressservicefake.Clientset{}
		adminNetworkPolicyFakeClient = &anpapifake.Clientset{}
		ipamClaimsFakeClient = &ipamclaimsapifake.Clientset{}
		nadsFakeClient = &nadsfake.Clientset{}
		networkQoSFakeClient = &networkqosfake.Clientset{}

		ovnClientset = &util.OVNKubeControllerClientset{
			KubeClient:            fakeClient,
			ANPClient:             adminNetworkPolicyFakeClient,
			EgressIPClient:        egressIPFakeClient,
			EgressFirewallClient:  egressFirewallFakeClient,
			EgressQoSClient:       egressQoSFakeClient,
			EgressServiceClient:   egressServiceFakeClient,
			IPAMClaimsClient:      ipamClaimsFakeClient,
			NetworkAttchDefClient: nadsFakeClient,
			NetworkQoSClient:      networkQoSFakeClient,
		}
		ovnCMClientset = &util.OVNClusterManagerClientset{
			KubeClient:            fakeClient,
			EgressIPClient:        egressIPFakeClient,
			CloudNetworkClient:    cloudNetworkFakeClient,
			EgressServiceClient:   egressServiceFakeClient,
			EgressFirewallClient:  egressFirewallFakeClient,
			IPAMClaimsClient:      ipamClaimsFakeClient,
			NetworkAttchDefClient: nadsFakeClient,
		}
		ovnNodeClientset = &util.OVNNodeClientset{
			KubeClient:            fakeClient,
			EgressIPClient:        egressIPFakeClient,
			EgressServiceClient:   egressServiceFakeClient,
			NetworkAttchDefClient: nadsFakeClient,
		}

		pods = make([]*corev1.Pod, 0)
		podWatch = objSetup(fakeClient, "pods", func(core.Action) (bool, runtime.Object, error) {
			obj := &corev1.PodList{}
			for _, p := range pods {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		namespaces = make([]*corev1.Namespace, 0)
		namespaceWatch = objSetup(fakeClient, "namespaces", func(core.Action) (bool, runtime.Object, error) {
			obj := &corev1.NamespaceList{}
			for _, p := range namespaces {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		nodes = make([]*corev1.Node, 0)
		nodeWatch = objSetup(fakeClient, "nodes", func(core.Action) (bool, runtime.Object, error) {
			obj := &corev1.NodeList{}
			for _, p := range nodes {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		policies = make([]*knet.NetworkPolicy, 0)
		policyWatch = objSetup(fakeClient, "networkpolicies", func(core.Action) (bool, runtime.Object, error) {
			obj := &knet.NetworkPolicyList{}
			for _, p := range policies {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		services = make([]*corev1.Service, 0)
		serviceWatch = objSetup(fakeClient, "services", func(core.Action) (bool, runtime.Object, error) {
			obj := &corev1.ServiceList{}
			for _, p := range services {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		endpointSlices = make([]*discovery.EndpointSlice, 0)
		endpointSliceWatch = objSetup(fakeClient, "endpointslices", func(core.Action) (bool, runtime.Object, error) {
			obj := &discovery.EndpointSliceList{}
			for _, p := range endpointSlices {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		egressFirewalls = make([]*egressfirewall.EgressFirewall, 0)
		egressFirewallWatch = egressFirewallObjSetup(egressFirewallFakeClient, "egressfirewalls", func(core.Action) (bool, runtime.Object, error) {
			obj := &egressfirewall.EgressFirewallList{}
			for _, p := range egressFirewalls {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		egressIPs = make([]*egressip.EgressIP, 0)
		egressIPWatch = egressIPObjSetup(egressIPFakeClient, "egressips", func(core.Action) (bool, runtime.Object, error) {
			obj := &egressip.EgressIPList{}
			for _, p := range egressIPs {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		cloudPrivateIPConfigs = make([]*ocpcloudnetworkapi.CloudPrivateIPConfig, 0)
		cloudPrivateIPConfigWatch = cloudPrivateIPConfigObjSetup(cloudNetworkFakeClient, "cloudprivateipconfigs", func(core.Action) (bool, runtime.Object, error) {
			obj := &ocpcloudnetworkapi.CloudPrivateIPConfigList{}
			for _, p := range cloudPrivateIPConfigs {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		egressQoSes = make([]*egressqos.EgressQoS, 0)
		egressQoSWatch = egressQoSObjSetup(egressQoSFakeClient, "egressqoses", func(core.Action) (bool, runtime.Object, error) {
			obj := &egressqos.EgressQoSList{}
			for _, p := range egressQoSes {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		egressServices = make([]*egressservice.EgressService, 0)
		egressServiceWatch = egressServiceObjSetup(egressServiceFakeClient, "egressservices", func(core.Action) (bool, runtime.Object, error) {
			obj := &egressservice.EgressServiceList{}
			for _, p := range egressServices {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		adminNetworkPolicies = make([]*anpapi.AdminNetworkPolicy, 0)
		adminNetPolWatch = adminNetworkPolicyObjSetup(adminNetworkPolicyFakeClient, "adminnetworkpolicies", func(core.Action) (bool, runtime.Object, error) {
			obj := &anpapi.AdminNetworkPolicyList{}
			for _, p := range adminNetworkPolicies {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		baselineAdminNetworkPolicies = make([]*anpapi.BaselineAdminNetworkPolicy, 0)
		baselineAdminNetPolWatch = adminNetworkPolicyObjSetup(adminNetworkPolicyFakeClient, "baselineadminnetworkpolicies", func(core.Action) (bool, runtime.Object, error) {
			obj := &anpapi.BaselineAdminNetworkPolicyList{}
			for _, p := range baselineAdminNetworkPolicies {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		ipamClaims = make([]*ipamclaimsapi.IPAMClaim, 0)
		ipamClaimsWatch = ipamClaimsObjSetup(ipamClaimsFakeClient, "ipamclaims", func(core.Action) (bool, runtime.Object, error) {
			obj := &ipamclaimsapi.IPAMClaimList{}
			for _, p := range ipamClaims {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		networkQoSes = make([]*networkqos.NetworkQoS, 0)
		networkQoSWatch = networkQoSObjSetup(networkQoSFakeClient, "networkqoses", func(core.Action) (bool, runtime.Object, error) {
			obj := &networkqos.NetworkQoSList{}
			for _, p := range networkQoSes {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		shutdown = false
	})

	AfterEach(func() {
		if !shutdown {
			wf.Shutdown()
		}
	})

	Context("when a processExisting is given", func() {
		testExisting := func(objType reflect.Type, namespace string, sel labels.Selector, priority int) {
			if objType == EndpointSliceType {
				wf, err = NewNodeWatchFactory(ovnNodeClientset, nodeName)
			} else if objType == CloudPrivateIPConfigType || objType == IPAMClaimsType {
				wf, err = NewClusterManagerWatchFactory(ovnCMClientset)
			} else {
				wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
			}
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			h, err := wf.addHandler(objType, namespace, sel,
				cache.ResourceEventHandlerFuncs{},
				func(objs []interface{}) error {
					defer GinkgoRecover()
					Expect(objs).To(HaveLen(1))
					return nil
				}, wf.GetHandlerPriority(objType))
			Expect(h).NotTo(BeNil())
			Expect(err).NotTo(HaveOccurred())
			Expect(h.priority).To(Equal(priority))
			wf.removeHandler(objType, h)
		}

		testExistingFilteredHandler := func(objType reflect.Type, realObj reflect.Type, namespace string, sel labels.Selector, priority int) {
			if objType == EndpointSliceType {
				wf, err = NewNodeWatchFactory(ovnNodeClientset, nodeName)
			} else if objType == CloudPrivateIPConfigType || objType == IPAMClaimsType {
				wf, err = NewClusterManagerWatchFactory(ovnCMClientset)
			} else {
				wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
			}
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			h, err := wf.AddFilteredPodHandler(namespace, sel,
				cache.ResourceEventHandlerFuncs{},
				func(objs []interface{}) error {
					defer GinkgoRecover()
					Expect(objs).To(HaveLen(1))
					return nil
				}, wf.GetHandlerPriority(realObj))
			Expect(h).NotTo(BeNil())
			Expect(err).NotTo(HaveOccurred())
			Expect(h.priority).To(Equal(priority))
			wf.removeHandler(objType, h)
		}

		It("is called for each existing pod", func() {
			pods = append(pods, newPod("pod1", "default"))
			testExisting(PodType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing namespace", func() {
			namespaces = append(namespaces, newNamespace("default"))
			testExisting(NamespaceType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing node", func() {
			nodes = append(nodes, newNode("default"))
			testExisting(NodeType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing policy", func() {
			policies = append(policies, newPolicy("denyall", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExisting(PolicyType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing policy: LocalPodSelectorType", func() {
			policies = append(policies, newPolicy("denyall", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExistingFilteredHandler(PodType, LocalPodSelectorType, "default", nil, 3)
		})

		It("is called for each existing endpointSlice", func() {
			endpointSlices = append(endpointSlices, newEndpointSlice("myEndpointSlice", "default", "myService"))
			testExisting(EndpointSliceType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing service", func() {
			services = append(services, newService("myservice", "default"))
			testExisting(ServiceType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing egressFirewall", func() {
			egressFirewalls = append(egressFirewalls, newEgressFirewall("myEgressFirewall", "default"))
			testExisting(EgressFirewallType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing egressIP", func() {
			egressIPs = append(egressIPs, newEgressIP("myEgressIP", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExisting(EgressIPType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing egressIP: EgressIPPodType", func() {
			egressIPs = append(egressIPs, newEgressIP("myEgressIP", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExistingFilteredHandler(PodType, EgressIPPodType, "default", nil, 1)
		})

		It("is called for each existing egressIP: EgressIPNamespaceType", func() {
			egressIPs = append(egressIPs, newEgressIP("myEgressIP", "default"))
			pods = append(pods, newPod("pod1", "default"))
			testExistingFilteredHandler(NamespaceType, EgressIPNamespaceType, "default", nil, 1)
		})

		It("is called for each existing cloudPrivateIPConfig", func() {
			cloudPrivateIPConfigs = append(cloudPrivateIPConfigs, newCloudPrivateIPConfig("192.168.176.25"))
			testExisting(CloudPrivateIPConfigType, "", nil, defaultHandlerPriority)
		})
		It("is called for each existing egressQoS", func() {
			egressQoSes = append(egressQoSes, newEgressQoS("myEgressQoS", "default"))
			testExisting(EgressQoSType, "", nil, defaultHandlerPriority)
		})
		It("is called for each existing egressService", func() {
			egressServices = append(egressServices, newEgressService("myEgressService", "default"))
			testExisting(EgressServiceType, "", nil, defaultHandlerPriority)
		})
		It("is called for each existing admin network policy", func() {
			adminNetworkPolicies = append(adminNetworkPolicies, newAdminNetworkPolicy("myANP", 3))
			testExisting(AdminNetworkPolicyType, "", nil, defaultHandlerPriority)
		})
		It("is called for each existing baseline admin network policy", func() {
			baselineAdminNetworkPolicies = append(baselineAdminNetworkPolicies, newBaselineAdminNetworkPolicy("myBANP"))
			testExisting(BaselineAdminNetworkPolicyType, "", nil, defaultHandlerPriority)
		})
		It("is called for each existing IPAMClaim", func() {
			ipamClaims = append(ipamClaims, newIPAMClaim("claim!"))
			testExisting(IPAMClaimsType, "", nil, defaultHandlerPriority)
		})
		It("is called for each existing networkQoS", func() {
			networkQoSes = append(networkQoSes, newNetworkQoS("myNetworkQoS", "default"))
			testExisting(NetworkQoSType, "", nil, defaultHandlerPriority)
		})

		It("is called for each existing pod that matches a given namespace and label", func() {
			pod := newPod("pod1", "default")
			pod.ObjectMeta.Labels["blah"] = "foobar"
			pods = append(pods, pod)

			sel, err := metav1.LabelSelectorAsSelector(
				&metav1.LabelSelector{
					MatchLabels: map[string]string{"blah": "foobar"},
				},
			)
			Expect(err).NotTo(HaveOccurred())

			testExisting(PodType, "default", sel, defaultHandlerPriority)
		})
	})

	Context("when existing items are known to the informer", func() {
		testExisting := func(objType reflect.Type) {
			if objType == EndpointSliceType {
				wf, err = NewNodeWatchFactory(ovnNodeClientset, nodeName)
			} else if objType == CloudPrivateIPConfigType {
				wf, err = NewClusterManagerWatchFactory(ovnCMClientset)
			} else {
				wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
			}
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			var addCalls int32
			h, err := wf.addHandler(objType, "", nil,
				cache.ResourceEventHandlerFuncs{
					AddFunc: func(interface{}) {
						atomic.AddInt32(&addCalls, 1)
					},
					UpdateFunc: func(interface{}, interface{}) {},
					DeleteFunc: func(interface{}) {},
				}, nil, wf.GetHandlerPriority(objType))
			Expect(int(addCalls)).To(Equal(2))
			Expect(err).NotTo(HaveOccurred())
			wf.removeHandler(objType, h)
		}

		It("calls ADD for each existing pod", func() {
			pods = append(pods, newPod("pod1", "default"))
			pods = append(pods, newPod("pod2", "default"))
			testExisting(PodType)
		})

		It("calls ADD for each existing namespace", func() {
			namespaces = append(namespaces, newNamespace("default"))
			namespaces = append(namespaces, newNamespace("default2"))
			testExisting(NamespaceType)
		})

		It("calls ADD for each existing node", func() {
			nodes = append(nodes, newNode("default"))
			nodes = append(nodes, newNode("default2"))
			testExisting(NodeType)
		})

		It("calls ADD for each existing policy", func() {
			policies = append(policies, newPolicy("denyall", "default"))
			policies = append(policies, newPolicy("denyall2", "default"))
			testExisting(PolicyType)
		})

		It("calls ADD for each existing endpointSlices", func() {
			endpointSlices = append(endpointSlices, newEndpointSlice("myEndpointSlice", "default", "myService"))
			endpointSlices = append(endpointSlices, newEndpointSlice("myEndpointSlice2", "default", "myService"))
			testExisting(EndpointSliceType)
		})

		It("calls ADD for each existing service", func() {
			services = append(services, newService("myservice", "default"))
			services = append(services, newService("myservice2", "default"))
			testExisting(ServiceType)
		})

		It("calls ADD for each existing egressFirewall", func() {
			egressFirewalls = append(egressFirewalls, newEgressFirewall("myFirewall", "default"))
			egressFirewalls = append(egressFirewalls, newEgressFirewall("myFirewall1", "default"))
			testExisting(EgressFirewallType)
		})
		It("calls ADD for each existing egressIP", func() {
			egressIPs = append(egressIPs, newEgressIP("myEgressIP", "default"))
			egressIPs = append(egressIPs, newEgressIP("myEgressIP1", "default"))
			testExisting(EgressIPType)
		})
		It("calls ADD for each existing cloudPrivateIPConfig", func() {
			cloudPrivateIPConfigs = append(cloudPrivateIPConfigs, newCloudPrivateIPConfig("192.168.126.25"))
			cloudPrivateIPConfigs = append(cloudPrivateIPConfigs, newCloudPrivateIPConfig("192.168.126.26"))
			testExisting(CloudPrivateIPConfigType)
		})
		It("calls ADD for each existing egressQoS", func() {
			egressQoSes = append(egressQoSes, newEgressQoS("myEgressQoS", "default"))
			egressQoSes = append(egressQoSes, newEgressQoS("myEgressQoS1", "default"))
			testExisting(EgressQoSType)
		})
		It("calls ADD for each existing egressService", func() {
			egressServices = append(egressServices, newEgressService("myEgressService", "default"))
			egressServices = append(egressServices, newEgressService("myEgressService1", "default"))
			testExisting(EgressServiceType)
		})
		It("calls ADD for each existing Admin Network Policy", func() {
			adminNetworkPolicies = append(adminNetworkPolicies, newAdminNetworkPolicy("myANP1", 10))
			adminNetworkPolicies = append(adminNetworkPolicies, newAdminNetworkPolicy("myANP2", 20))
			testExisting(AdminNetworkPolicyType)
		})
		It("calls ADD for each existing Baseline Admin Network Policy", func() {
			baselineAdminNetworkPolicies = append(baselineAdminNetworkPolicies, newBaselineAdminNetworkPolicy("myBANP"))
			baselineAdminNetworkPolicies = append(baselineAdminNetworkPolicies, newBaselineAdminNetworkPolicy("myBANP2"))
			testExisting(BaselineAdminNetworkPolicyType)
		})
		It("calls ADD for each existing networkQoS", func() {
			networkQoSes = append(networkQoSes, newNetworkQoS("myNetworkQoS", "default"))
			networkQoSes = append(networkQoSes, newNetworkQoS("myNetworkQoS1", "default"))
			testExisting(NetworkQoSType)
		})

		It("doesn't deadlock when factory is shutdown", func() {
			// every queue has length 10, but some events may be handled before the stop channel event is selected,
			// so multiply by 15 instead of 10 to ensure overflow
			for i := uint32(1); i <= defaultNumEventQueues*15; i++ {
				pods = append(pods, newPod(fmt.Sprintf("pod%d", i), "default"))
			}
			wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			wf.Shutdown()
			shutdown = true
			h, err := wf.addHandler(PodType, "", nil,
				cache.ResourceEventHandlerFuncs{
					AddFunc:    func(interface{}) {},
					UpdateFunc: func(interface{}, interface{}) {},
					DeleteFunc: func(interface{}) {},
				}, nil, wf.GetHandlerPriority(PodType))
			Expect(err).NotTo(HaveOccurred())
			wf.removeHandler(PodType, h)
		})
	})

	Context("when EgressIP is disabled", func() {
		testExisting := func(objType reflect.Type) {
			wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.informers).NotTo(HaveKey(objType))
		}
		It("does not contain Egress IP informer", func() {
			config.OVNKubernetesFeature.EnableEgressIP = false
			testExisting(EgressIPType)
		})
	})
	Context("when EgressFirewall is disabled", func() {
		testExisting := func(objType reflect.Type) {
			wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.informers).NotTo(HaveKey(objType))
		}
		It("does not contain EgressFirewall informer", func() {
			config.OVNKubernetesFeature.EnableEgressFirewall = false
			testExisting(EgressFirewallType)
		})
	})
	Context("when EgressQoS is disabled", func() {
		testExisting := func(objType reflect.Type) {
			wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.informers).NotTo(HaveKey(objType))
		}
		It("does not contain EgressQoS informer", func() {
			config.OVNKubernetesFeature.EnableEgressQoS = false
			testExisting(EgressQoSType)
		})
	})
	Context("when EgressService is disabled", func() {
		testExisting := func(objType reflect.Type) {
			wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.informers).NotTo(HaveKey(objType))
		}
		It("does not contain EgressService informer", func() {
			config.OVNKubernetesFeature.EnableEgressService = false
			testExisting(EgressServiceType)
		})
	})
	Context("when Admin Network Policy is disabled", func() {
		testExisting := func(objType reflect.Type) {
			wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.informers).NotTo(HaveKey(objType))
		}
		It("does not contain Admin Network Policy informer", func() {
			config.OVNKubernetesFeature.EnableAdminNetworkPolicy = false
			testExisting(AdminNetworkPolicyType)
		})
		It("does not contain Baseline Admin Network Policy informer", func() {
			config.OVNKubernetesFeature.EnableAdminNetworkPolicy = false
			testExisting(BaselineAdminNetworkPolicyType)
		})
	})

	Context("when Persistent IPs feature is disabled", func() {
		testExisting := func(objType reflect.Type) {
			wf, err = NewClusterManagerWatchFactory(ovnCMClientset)
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.informers).NotTo(HaveKey(objType))
		}
		It("does not contain IPAMClaims informer", func() {
			config.OVNKubernetesFeature.EnablePersistentIPs = false
			testExisting(IPAMClaimsType)
		})
	})

	Context("when NetworkQoS is disabled", func() {
		testExisting := func(objType reflect.Type) {
			wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
			Expect(err).NotTo(HaveOccurred())
			err = wf.Start()
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.informers).NotTo(HaveKey(objType))
		}
		It("does not contain NetworkQoS informer", func() {
			config.OVNKubernetesFeature.EnableNetworkQoS = false
			testExisting(NetworkQoSType)
		})
	})

	addFilteredHandler := func(wf *WatchFactory, objType reflect.Type, realObjType reflect.Type, namespace string, sel labels.Selector, funcs cache.ResourceEventHandlerFuncs) (*Handler, *handlerCalls) {
		calls := handlerCalls{}
		h, err := wf.addHandler(objType, namespace, sel, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				atomic.AddInt32(&calls.added, 1)
				funcs.AddFunc(obj)
			},
			UpdateFunc: func(old, new interface{}) {
				defer GinkgoRecover()
				atomic.AddInt32(&calls.updated, 1)
				funcs.UpdateFunc(old, new)
			},
			DeleteFunc: func(obj interface{}) {
				defer GinkgoRecover()
				atomic.AddInt32(&calls.deleted, 1)
				funcs.DeleteFunc(obj)
			},
		}, nil, wf.GetHandlerPriority(realObjType))
		Expect(h).NotTo(BeNil())
		Expect(err).NotTo(HaveOccurred())
		return h, &calls
	}

	addHandler := func(wf *WatchFactory, objType reflect.Type, funcs cache.ResourceEventHandlerFuncs) (*Handler, *handlerCalls) {
		return addFilteredHandler(wf, objType, objType, "", nil, funcs)
	}

	addPriorityHandler := func(wf *WatchFactory, objType reflect.Type, realObjType reflect.Type, funcs cache.ResourceEventHandlerFuncs) (*Handler, *handlerCalls) {
		return addFilteredHandler(wf, objType, realObjType, "", nil, funcs)
	}

	It("responds to pod add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newPod("pod1", "default")
		h, c := addHandler(wf, PodType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*corev1.Pod)
				Expect(reflect.DeepEqual(pod, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newPod := new.(*corev1.Pod)
				Expect(reflect.DeepEqual(newPod, added)).To(BeTrue())
				Expect(newPod.Spec.NodeName).To(Equal("foobar"))
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*corev1.Pod)
				Expect(reflect.DeepEqual(pod, added)).To(BeTrue())
			},
		})

		pods = append(pods, added)
		podWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.NodeName = "foobar"
		podWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		pods = pods[:0]
		podWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemovePodHandler(h)
	})

	It("responds to pod replace with create/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newPod("pod1", "default")
		added.UID = "mybar"
		h, c := addHandler(wf, PodType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*corev1.Pod)
				Expect(pod.Spec.NodeName).To(Equal("mynode"))
			},
			UpdateFunc: func(_, new interface{}) {
				newPod := new.(*corev1.Pod)
				Expect(newPod.UID).To(Equal(types.UID("mybar")))
				Expect(newPod.Spec.NodeName).To(Equal("foobar"))
			},
			DeleteFunc: func(interface{}) {
			},
		})

		pods = append(pods, added)
		podWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		podCopy := added.DeepCopy()
		podCopy.Spec.NodeName = "foobar"
		podWatch.Modify(podCopy)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		podCopy = added.DeepCopy()
		podCopy.UID = "foobar"
		podCopy.Spec.NodeName = "mynode"
		podWatch.Modify(podCopy)
		Eventually(c.getDeleted, 2).Should(Equal(1))
		Eventually(c.getAdded, 2).Should(Equal(2))
		Eventually(c.getUpdated, 2).Should(Equal(1))
		pods = pods[:0]
		podWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(2))

		wf.RemovePodHandler(h)
	})

	It("responds to multiple pod add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		const nodeName string = "mynode"
		type opTest struct {
			mu      sync.Mutex
			pod     *corev1.Pod
			added   int
			updated int
			deleted int
		}
		testPods := make(map[string]*opTest)

		for i := 0; i < 5; i++ {
			name := fmt.Sprintf("mypod-%d", i)
			pod := newPod(name, fmt.Sprintf("namespace-%d", i))
			testPods[name] = &opTest{pod: pod}
		}
		waitFor := func(ot *opTest, operation string, expected int, count func(*opTest) int) {
			Eventually(func() int {
				ot.mu.Lock()
				defer ot.mu.Unlock()
				return count(ot)
			}, 2).Should(Equal(expected), "pod %q %s callback count did not reach %d", ot.pod.Name, operation, expected)
		}

		h, c := addHandler(wf, PodType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*corev1.Pod)
				ot, ok := testPods[pod.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(BeNumerically("<", 2))
				ot.added++
			},
			UpdateFunc: func(_, new interface{}) {
				newPod := new.(*corev1.Pod)
				ot, ok := testPods[newPod.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.updated).To(BeNumerically("<", 2))
				ot.updated++
				Expect(newPod.Spec.NodeName).To(Equal(nodeName))
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*corev1.Pod)
				ot, ok := testPods[pod.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.deleted).To(BeNumerically("<", 2))
				ot.deleted++
			},
		})

		// Add/Update/Delete each pod twice
		for i := 0; i < 2; i++ {
			for _, ot := range testPods {
				pods = append(pods, ot.pod)
				podWatch.Add(ot.pod)
				waitFor(ot, "add", i+1, func(ot *opTest) int { return ot.added })
				ot.mu.Lock()
				ot.pod.Spec.NodeName = nodeName
				ot.mu.Unlock()
				podWatch.Modify(ot.pod)
				waitFor(ot, "update", i+1, func(ot *opTest) int { return ot.updated })
				pods = pods[:0]
				podWatch.Delete(ot.pod)
				waitFor(ot, "delete", i+1, func(ot *opTest) int { return ot.deleted })
			}
		}

		// Ensure total number of each operation is 10; and each
		// node's individual operation count is 2
		Eventually(c.getAdded, 2).Should(Equal(10))
		Eventually(c.getUpdated, 2).Should(Equal(10))
		Eventually(c.getDeleted, 2).Should(Equal(10))
		for _, ot := range testPods {
			ot.mu.Lock()
			Expect(ot.added).Should(Equal(2))
			Expect(ot.updated).Should(Equal(2))
			Expect(ot.deleted).Should(Equal(2))
			ot.mu.Unlock()
		}

		wf.RemovePodHandler(h)
	})

	It("responds to namespace add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newNamespace("default")
		h, c := addHandler(wf, NamespaceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				ns := obj.(*corev1.Namespace)
				Expect(reflect.DeepEqual(ns, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newNS := new.(*corev1.Namespace)
				Expect(reflect.DeepEqual(newNS, added)).To(BeTrue())
				Expect(newNS.Status.Phase).To(Equal(corev1.NamespaceTerminating))
			},
			DeleteFunc: func(obj interface{}) {
				ns := obj.(*corev1.Namespace)
				Expect(reflect.DeepEqual(ns, added)).To(BeTrue())
			},
		})

		namespaces = append(namespaces, added)
		namespaceWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Status.Phase = corev1.NamespaceTerminating
		namespaceWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		namespaces = namespaces[:0]
		namespaceWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveNamespaceHandler(h)
	})

	It("responds to node add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newNode("mynode")
		h, c := addHandler(wf, NodeType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				node := obj.(*corev1.Node)
				Expect(reflect.DeepEqual(node, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newNode := new.(*corev1.Node)
				Expect(reflect.DeepEqual(newNode, added)).To(BeTrue())
				Expect(newNode.Status.Phase).To(Equal(corev1.NodeTerminated))
			},
			DeleteFunc: func(obj interface{}) {
				node := obj.(*corev1.Node)
				Expect(reflect.DeepEqual(node, added)).To(BeTrue())
			},
		})

		nodes = append(nodes, added)
		nodeWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Status.Phase = corev1.NodeTerminated
		nodeWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		nodes = nodes[:0]
		nodeWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveNodeHandler(h)
	})

	It("responds to multiple node add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		type opTest struct {
			mu      sync.Mutex
			node    *corev1.Node
			added   int
			updated int
			deleted int
		}
		testNodes := make(map[string]*opTest)

		for i := 0; i < 5; i++ {
			name := fmt.Sprintf("mynode-%d", i)
			node := newNode(name)
			testNodes[name] = &opTest{node: node}
		}
		waitFor := func(ot *opTest, operation string, expected int, count func(*opTest) int) {
			Eventually(func() int {
				ot.mu.Lock()
				defer ot.mu.Unlock()
				return count(ot)
			}, 2).Should(Equal(expected), "node %q %s callback count did not reach %d", ot.node.Name, operation, expected)
		}

		h, c := addHandler(wf, NodeType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				node := obj.(*corev1.Node)
				ot, ok := testNodes[node.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(BeNumerically("<", 2))
				ot.added++
			},
			UpdateFunc: func(_, new interface{}) {
				newNode := new.(*corev1.Node)
				ot, ok := testNodes[newNode.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.updated).To(BeNumerically("<", 2))
				ot.updated++
				Expect(newNode.Status.Phase).To(Equal(corev1.NodeTerminated))
			},
			DeleteFunc: func(obj interface{}) {
				node := obj.(*corev1.Node)
				ot, ok := testNodes[node.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				Expect(ot.deleted).To(BeNumerically("<", 2))
				ot.deleted++
				ot.mu.Unlock()
			},
		})

		// Add/Update/Delete each node twice
		for i := 0; i < 2; i++ {
			for _, ot := range testNodes {
				nodes = append(nodes, ot.node)
				nodeWatch.Add(ot.node)
				waitFor(ot, "add", i+1, func(ot *opTest) int { return ot.added })
				ot.mu.Lock()
				ot.node.Status.Phase = corev1.NodeTerminated
				ot.mu.Unlock()
				nodeWatch.Modify(ot.node)
				waitFor(ot, "update", i+1, func(ot *opTest) int { return ot.updated })
				nodes = nodes[:0]
				nodeWatch.Delete(ot.node)
				waitFor(ot, "delete", i+1, func(ot *opTest) int { return ot.deleted })
			}
		}

		// Ensure total number of each operation is 10; and each
		// node's individual operation count is 2
		Eventually(c.getAdded, 2).Should(Equal(10))
		Eventually(c.getUpdated, 2).Should(Equal(10))
		Eventually(c.getDeleted, 2).Should(Equal(10))
		for _, ot := range testNodes {
			ot.mu.Lock()
			Expect(ot.added).Should(Equal(2))
			Expect(ot.updated).Should(Equal(2))
			Expect(ot.deleted).Should(Equal(2))
			ot.mu.Unlock()
		}

		wf.RemoveNodeHandler(h)
	})

	It("correctly orders queued informer initial add events and subsequent update events", func() {
		type opTest struct {
			mu      sync.Mutex
			node    *corev1.Node
			added   int
			updated int
		}
		testNodes := make(map[string]*opTest)

		for i := 0; i < 600; i++ {
			name := fmt.Sprintf("mynode-%d", i)
			node := newNode(name)
			testNodes[name] = &opTest{node: node}
			// Add all nodes to the initial list
			nodes = append(nodes, node)
		}

		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		h, c := addHandler(wf, NodeType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				node := obj.(*corev1.Node)
				ot, ok := testNodes[node.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(0), "add for node %s already run", node.Name)
				ot.added++
			},
			UpdateFunc: func(_, new interface{}) {
				defer GinkgoRecover()
				newNode := new.(*corev1.Node)
				ot, ok := testNodes[newNode.Name]
				Expect(ok).To(BeTrue())
				// Expect updates to be processed after Add
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(1), "update for node %s processed before initial add!", newNode.Name)
				Expect(ot.updated).To(Equal(0))
				ot.updated++
				Expect(newNode.Status.Phase).To(Equal(corev1.NodeTerminated))
			},
			DeleteFunc: func(interface{}) {},
		})

		done := make(chan bool)
		go func() {
			// Send an update event for each node
			for _, n := range nodes {
				n.Status.Phase = corev1.NodeTerminated
				nodeWatch.Modify(n)
			}
			done <- true
		}()

		// Adds are done synchronously at handler addition time
		for _, ot := range testNodes {
			ot.mu.Lock()
			Expect(ot.added).To(Equal(1), "missing add for node %s", ot.node.Name)
			ot.mu.Unlock()
		}
		Expect(c.getAdded()).To(Equal(len(testNodes)))

		<-done
		// Updates are async and may take a bit longer to finish
		Eventually(c.getUpdated, 10).Should(Equal(len(testNodes)))
		for _, ot := range testNodes {
			ot.mu.Lock()
			Expect(ot.updated).To(Equal(1), "missing update for node %s", ot.node.Name)
			ot.mu.Unlock()
		}

		wf.RemoveNodeHandler(h)
	})

	It("correctly orders serialized informer initial add events and subsequent update events", func() {
		type opTest struct {
			mu        sync.Mutex
			namespace *corev1.Namespace
			added     int
			updated   int
		}
		testNamespaces := make(map[string]*opTest)

		for i := 0; i < 598; i++ {
			name := fmt.Sprintf("mynamespace-%d", i)
			namespace := newNamespace(name)
			testNamespaces[name] = &opTest{namespace: namespace}
			// Add all namespaces to the initial list
			namespaces = append(namespaces, namespace)
		}

		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		startWg := sync.WaitGroup{}
		startWg.Add(1)
		doneWg := sync.WaitGroup{}
		doneWg.Add(1)
		go func() {
			startWg.Done()
			// Send an update event for each namespace
			for _, n := range namespaces {
				n.Status.Phase = corev1.NamespaceTerminating
				namespaceWatch.Modify(n)
			}
			doneWg.Done()
		}()
		startWg.Wait()

		h, c := addHandler(wf, NamespaceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				namespace := obj.(*corev1.Namespace)
				ot, ok := testNamespaces[namespace.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(0))
				ot.added++
			},
			UpdateFunc: func(_, new interface{}) {
				defer GinkgoRecover()
				newNamespace := new.(*corev1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Expect updates to be processed after Add
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(1), "update for namespace %s processed before initial add!", newNamespace.Name)
				Expect(ot.updated).To(Equal(0))
				ot.updated++
				Expect(newNamespace.Status.Phase).To(Equal(corev1.NamespaceTerminating))
			},
			DeleteFunc: func(interface{}) {},
		})
		doneWg.Wait()

		// Adds are done synchronously at handler addition time
		for _, ot := range testNamespaces {
			ot.mu.Lock()
			Expect(ot.added).To(Equal(1), "missing add for namespace %s", ot.namespace.Name)
			ot.mu.Unlock()
		}
		Expect(c.getAdded()).To(Equal(len(testNamespaces)))

		// Updates are async and may take a bit longer to finish
		Eventually(c.getUpdated, 10).Should(Equal(len(testNamespaces)))
		for _, ot := range testNamespaces {
			ot.mu.Lock()
			Expect(ot.updated).To(Equal(1), "missing update for namespace %s", ot.namespace.Name)
			ot.mu.Unlock()
		}

		wf.RemoveNamespaceHandler(h)
	})

	It("correctly orders add events across prioritized handlers sharing the same object type", func() {
		type opTest struct {
			mu        sync.Mutex
			namespace *corev1.Namespace
			added     int
			updated   int
			deleted   int
		}
		testNamespaces := make(map[string]*opTest)

		for i := 0; i < 998; i++ {
			name := fmt.Sprintf("mynamespace-%d", i)
			namespace := newNamespace(name)
			namespace.Status.Phase = ""
			testNamespaces[name] = &opTest{namespace: namespace}
			// Add all namespaces to the initial list
			namespaces = append(namespaces, namespace)
		}

		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		nsh, c1 := addPriorityHandler(wf, NamespaceType, NamespaceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				namespace := obj.(*corev1.Namespace)
				ot, ok := testNamespaces[namespace.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(0))
				ot.added++
				Expect(namespace.Status.Phase).To(BeEmpty())
			},
			UpdateFunc: func(_, new interface{}) {
				defer GinkgoRecover()
				newNamespace := new.(*corev1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Expect updates to be processed after Add
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(10), "update for EIP namespace %s processed before add was processed in all handlers!", newNamespace.Name)
				Expect(ot.updated).To(Equal(0))
				ot.updated++
				Expect(newNamespace.Status.Phase).To(Equal(corev1.NamespaceActive))
			},
			DeleteFunc: func(obj interface{}) {
				defer GinkgoRecover()
				newNamespace := obj.(*corev1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Verify that deletes were processed after the updates and adds
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(10), "delete for EIP namespace %s processed before add was processed in all handlers!", newNamespace.Name)
				Expect(ot.updated).To(Equal(10), "delete for EIP namespace %s processed before update was processed in all handlers!", newNamespace.Name)
				Expect(ot.deleted).To(Equal(1))
				ot.deleted = ot.deleted * 10
				Expect(newNamespace.Status.Phase).To(Equal(corev1.NamespaceTerminating))
			},
		})

		eipnsh, c2 := addPriorityHandler(wf, NamespaceType, EgressIPNamespaceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				namespace := obj.(*corev1.Namespace)
				ot, ok := testNamespaces[namespace.Name]
				Expect(ok).To(BeTrue())
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(1), "add for EIP namespace %s processed before initial namespace add!", namespace.Name)
				ot.added = ot.added * 10
				Expect(namespace.Status.Phase).To(BeEmpty())
			},
			UpdateFunc: func(_, new interface{}) {
				defer GinkgoRecover()
				newNamespace := new.(*corev1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Expect updates to be processed after Add
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(10), "update for EIP namespace %s processed before add was processed in all handlers!", newNamespace.Name)
				Expect(ot.updated).To(Equal(1), "update for EIP namespace %s processed before initial namespace update!", newNamespace.Name)
				ot.updated = ot.updated * 10
				Expect(newNamespace.Status.Phase).To(Equal(corev1.NamespaceActive))
			},
			DeleteFunc: func(obj interface{}) {
				defer GinkgoRecover()
				newNamespace := obj.(*corev1.Namespace)
				ot, ok := testNamespaces[newNamespace.Name]
				Expect(ok).To(BeTrue())
				// Verify that deletes were processed after the updates and adds
				ot.mu.Lock()
				defer ot.mu.Unlock()
				Expect(ot.added).To(Equal(10), "delete for EIP namespace %s processed before add was processed in all handlers!", newNamespace.Name)
				Expect(ot.updated).To(Equal(10), "delete for EIP namespace %s processed before update was processed in all handlers!", newNamespace.Name)
				Expect(ot.deleted).To(Equal(0))
				ot.deleted++
				Expect(newNamespace.Status.Phase).To(Equal(corev1.NamespaceTerminating))
			},
		})
		done := make(chan bool)
		go func() {
			// Send an update event for each namespace
			for _, n := range namespaces {
				n.Status.Phase = corev1.NamespaceActive
				namespaceWatch.Modify(n)
			}
			done <- true
		}()

		// Adds are done synchronously at handler addition time
		for _, ot := range testNamespaces {
			ot.mu.Lock()
			// ((0 + 1) * 10) = 10
			Expect(ot.added).To(Equal(10), "missing add for namespace %s", ot.namespace.Name)
			ot.mu.Unlock()
		}
		Expect(c1.getAdded()).To(Equal(len(testNamespaces)))
		Expect(c2.getAdded()).To(Equal(len(testNamespaces)))
		<-done
		// Updates are async and may take a bit longer to finish
		Eventually(c1.getUpdated, 10).Should(Equal(len(testNamespaces)))
		Eventually(c2.getUpdated, 10).Should(Equal(len(testNamespaces)))

		for _, ot := range testNamespaces {
			ot.mu.Lock()
			// ((0 + 1) * 10) = 10
			Expect(ot.updated).To(Equal(10), "missing update for namespace %s", ot.namespace.Name)
			ot.mu.Unlock()
		}

		go func() {
			// Send a delete event for each namespace
			for _, n := range namespaces {
				n.Status.Phase = corev1.NamespaceTerminating
				namespaceWatch.Delete(n)
			}
			done <- true
		}()
		<-done
		// Deletes are async and may take a bit longer to finish
		Eventually(c1.getDeleted, 10).Should(Equal(len(testNamespaces)))
		Eventually(c2.getDeleted, 10).Should(Equal(len(testNamespaces)))

		for _, ot := range testNamespaces {
			ot.mu.Lock()
			// ((0 + 1) * 10) = 10
			Expect(ot.deleted).To(Equal(10), "missing delete for namespace %s", ot.namespace.Name)
			ot.mu.Unlock()
		}

		wf.RemoveNamespaceHandler(nsh)
		wf.RemoveNamespaceHandler(eipnsh)
	})

	It("responds to policy add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newPolicy("mypolicy", "default")
		h, c := addHandler(wf, PolicyType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				np := obj.(*knet.NetworkPolicy)
				Expect(reflect.DeepEqual(np, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newNP := new.(*knet.NetworkPolicy)
				Expect(reflect.DeepEqual(newNP, added)).To(BeTrue())
				Expect(newNP.Spec.PolicyTypes).To(Equal([]knet.PolicyType{knet.PolicyTypeIngress}))
			},
			DeleteFunc: func(obj interface{}) {
				np := obj.(*knet.NetworkPolicy)
				Expect(reflect.DeepEqual(np, added)).To(BeTrue())
			},
		})

		policies = append(policies, added)
		policyWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.PolicyTypes = []knet.PolicyType{knet.PolicyTypeIngress}
		policyWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		policies = policies[:0]
		policyWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemovePolicyHandler(h)
	})

	It("responds to endpointslices add/update/delete events", func() {
		wf, err = NewNodeWatchFactory(ovnNodeClientset, nodeName)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newEndpointSlice("myEndpointSlice", "default", "myService")
		h, c := addHandler(wf, EndpointSliceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				epSlice := obj.(*discovery.EndpointSlice)
				Expect(reflect.DeepEqual(epSlice, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newEpSlice := new.(*discovery.EndpointSlice)
				Expect(reflect.DeepEqual(newEpSlice, added)).To(BeTrue())
				Expect(newEpSlice.Endpoints).To(HaveLen(1))
			},
			DeleteFunc: func(obj interface{}) {
				epSlice := obj.(*discovery.EndpointSlice)
				Expect(reflect.DeepEqual(epSlice, added)).To(BeTrue())
			},
		})

		endpointSlices = append(endpointSlices, added)
		endpointSliceWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Endpoints = append(added.Endpoints, discovery.Endpoint{
			Addresses: []string{"1.1.1.1"},
		})
		endpointSliceWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		endpointSlices = endpointSlices[:0]
		endpointSliceWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveEndpointSliceHandler(h)
	})

	It("responds to service add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newService("myservice", "default")
		h, c := addHandler(wf, ServiceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				service := obj.(*corev1.Service)
				Expect(reflect.DeepEqual(service, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newService := new.(*corev1.Service)
				Expect(reflect.DeepEqual(newService, added)).To(BeTrue())
				Expect(newService.Spec.ClusterIP).To(Equal("1.1.1.1"))
			},
			DeleteFunc: func(obj interface{}) {
				service := obj.(*corev1.Service)
				Expect(reflect.DeepEqual(service, added)).To(BeTrue())
			},
		})

		services = append(services, added)
		serviceWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.ClusterIP = "1.1.1.1"
		serviceWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		services = services[:0]
		serviceWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveServiceHandler(h)
	})

	It("responds to egressFirewall add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newEgressFirewall("myEgressFirewall", "default")
		h, c := addHandler(wf, EgressFirewallType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				egressFirewall := obj.(*egressfirewall.EgressFirewall)
				Expect(reflect.DeepEqual(egressFirewall, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newEgressFirewall := new.(*egressfirewall.EgressFirewall)
				Expect(reflect.DeepEqual(newEgressFirewall, added)).To(BeTrue())
				Expect(newEgressFirewall.Spec.Egress[0].Type).To(Equal(egressfirewall.EgressFirewallRuleDeny))
			},
			DeleteFunc: func(obj interface{}) {
				egressFirewall := obj.(*egressfirewall.EgressFirewall)
				Expect(reflect.DeepEqual(egressFirewall, added)).To(BeTrue())
			},
		})

		egressFirewalls = append(egressFirewalls, added)
		egressFirewallWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.Egress[0].Type = egressfirewall.EgressFirewallRuleDeny
		egressFirewallWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		egressFirewalls = egressFirewalls[:0]
		egressFirewallWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveEgressFirewallHandler(h)
	})
	It("responds to egressIP add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newEgressIP("myEgressIP", "default")
		h, c := addHandler(wf, EgressIPType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				egressIP := obj.(*egressip.EgressIP)
				Expect(reflect.DeepEqual(egressIP, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newEgressIP := new.(*egressip.EgressIP)
				Expect(reflect.DeepEqual(newEgressIP, added)).To(BeTrue())
				Expect(newEgressIP.Spec.EgressIPs).To(Equal([]string{"192.168.126.10"}))
			},
			DeleteFunc: func(obj interface{}) {
				egressIP := obj.(*egressip.EgressIP)
				Expect(reflect.DeepEqual(egressIP, added)).To(BeTrue())
			},
		})

		egressIPs = append(egressIPs, added)
		egressIPWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.EgressIPs = []string{"192.168.126.10"}
		egressIPWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		egressIPs = egressIPs[:0]
		egressIPWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveEgressIPHandler(h)
	})
	It("responds to cloudPrivateIPConfig add/update/delete events", func() {
		wf, err = NewClusterManagerWatchFactory(ovnCMClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newCloudPrivateIPConfig("192.168.126.25")
		h, c := addHandler(wf, CloudPrivateIPConfigType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
				Expect(reflect.DeepEqual(cloudPrivateIPConfig, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newCloudPrivateIPConfig := new.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
				Expect(reflect.DeepEqual(newCloudPrivateIPConfig, added)).To(BeTrue())
				Expect(newCloudPrivateIPConfig.Name).To(Equal("192.168.126.25"))
			},
			DeleteFunc: func(obj interface{}) {
				cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
				Expect(reflect.DeepEqual(cloudPrivateIPConfig, added)).To(BeTrue())
			},
		})

		cloudPrivateIPConfigs = append(cloudPrivateIPConfigs, added)
		cloudPrivateIPConfigWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.Node = "nodeA"
		cloudPrivateIPConfigWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		cloudPrivateIPConfigs = cloudPrivateIPConfigs[:0]
		cloudPrivateIPConfigWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveCloudPrivateIPConfigHandler(h)
	})
	It("responds to egressQoS add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newEgressQoS("myEgressQoS", "default")
		h, c := addHandler(wf, EgressQoSType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				egressQoS := obj.(*egressqos.EgressQoS)
				Expect(reflect.DeepEqual(egressQoS, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newEgressQoS := new.(*egressqos.EgressQoS)
				Expect(reflect.DeepEqual(newEgressQoS, added)).To(BeTrue())
				Expect(newEgressQoS.Spec.Egress[0].DSCP).To(Equal(40))
			},
			DeleteFunc: func(obj interface{}) {
				egressQoS := obj.(*egressqos.EgressQoS)
				Expect(reflect.DeepEqual(egressQoS, added)).To(BeTrue())
			},
		})

		egressQoSes = append(egressQoSes, added)
		egressQoSWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.Egress[0].DSCP = 40
		egressQoSWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		egressQoSes = egressQoSes[:0]
		egressQoSWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveEgressQoSHandler(h)
	})
	It("responds to egressService add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newEgressService("myEgressService", "default")
		h, c := addHandler(wf, EgressServiceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				egressService := obj.(*egressservice.EgressService)
				Expect(reflect.DeepEqual(egressService, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newEgressService := new.(*egressservice.EgressService)
				Expect(reflect.DeepEqual(newEgressService, added)).To(BeTrue())
				Expect(newEgressService.Spec.NodeSelector).To(Equal(metav1.LabelSelector{
					MatchLabels: map[string]string{
						"kubernetes.io/hostname": "node2",
					},
				}))
			},
			DeleteFunc: func(obj interface{}) {
				egressService := obj.(*egressservice.EgressService)
				Expect(reflect.DeepEqual(egressService, added)).To(BeTrue())
			},
		})

		egressServices = append(egressServices, added)
		egressServiceWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.NodeSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{
				"kubernetes.io/hostname": "node2",
			},
		}
		egressServiceWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		egressServices = egressServices[:0]
		egressServiceWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveEgressServiceHandler(h)
	})
	It("responds to admin network policy add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newAdminNetworkPolicy("myANP", 2)
		h, c := addHandler(wf, AdminNetworkPolicyType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				anp := obj.(*anpapi.AdminNetworkPolicy)
				Expect(reflect.DeepEqual(anp, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newANP := new.(*anpapi.AdminNetworkPolicy)
				Expect(reflect.DeepEqual(newANP, added)).To(BeTrue())
				Expect(newANP.Spec.Priority).To(Equal(int32(3)))
			},
			DeleteFunc: func(obj interface{}) {
				anp := obj.(*anpapi.AdminNetworkPolicy)
				Expect(reflect.DeepEqual(anp, added)).To(BeTrue())
			},
		})

		adminNetworkPolicies = append(adminNetworkPolicies, added)
		adminNetPolWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.Priority = 3
		adminNetPolWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		adminNetworkPolicies = adminNetworkPolicies[:0]
		adminNetPolWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveAdminNetworkPolicyHandler(h)
	})
	It("responds to baseline admin network policy add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newBaselineAdminNetworkPolicy("myBANP")
		h, c := addHandler(wf, BaselineAdminNetworkPolicyType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				banp := obj.(*anpapi.BaselineAdminNetworkPolicy)
				Expect(reflect.DeepEqual(banp, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newBANP := new.(*anpapi.BaselineAdminNetworkPolicy)
				Expect(reflect.DeepEqual(newBANP, added)).To(BeTrue())
				labelSelect := &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": "default",
					},
				}
				Expect(newBANP.Spec.Subject.Namespaces).To(Equal(labelSelect))
			},
			DeleteFunc: func(obj interface{}) {
				anp := obj.(*anpapi.BaselineAdminNetworkPolicy)
				Expect(reflect.DeepEqual(anp, added)).To(BeTrue())
			},
		})

		baselineAdminNetworkPolicies = append(baselineAdminNetworkPolicies, added)
		baselineAdminNetPolWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.Subject.Namespaces = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"kubernetes.io/metadata.name": "default",
			},
		}
		baselineAdminNetPolWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		baselineAdminNetworkPolicies = baselineAdminNetworkPolicies[:0]
		baselineAdminNetPolWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveBaselineAdminNetworkPolicyHandler(h)
	})
	It("responds to IPAMClaims add/update/delete events", func() {
		wf, err = NewClusterManagerWatchFactory(ovnCMClientset)
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newIPAMClaim("claiM!")
		h, c := addHandler(wf, IPAMClaimsType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				claim := obj.(*ipamclaimsapi.IPAMClaim)
				Expect(reflect.DeepEqual(claim, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newClaim := new.(*ipamclaimsapi.IPAMClaim)
				Expect(reflect.DeepEqual(newClaim, added)).To(BeTrue())
			},
			DeleteFunc: func(obj interface{}) {
				claim := obj.(*ipamclaimsapi.IPAMClaim)
				Expect(reflect.DeepEqual(claim, added)).To(BeTrue())
			},
		})

		ipamClaims = append(ipamClaims, added)
		ipamClaimsWatch.Add(added)

		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Status.IPs = []string{"10.10.10.10/24"}
		ipamClaimsWatch.Modify(added)

		Eventually(c.getUpdated, 2).Should(Equal(1))

		ipamClaims = ipamClaims[:0]
		ipamClaimsWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveIPAMClaimsHandler(h)
	})

	It("responds to networkQoS add/update/delete events", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newNetworkQoS("myNetworkQoS", "default")
		h, c := addHandler(wf, NetworkQoSType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				networkQoS := obj.(*networkqos.NetworkQoS)
				Expect(reflect.DeepEqual(networkQoS, added)).To(BeTrue())
			},
			UpdateFunc: func(_, new interface{}) {
				newNetworkQoS := new.(*networkqos.NetworkQoS)
				Expect(reflect.DeepEqual(newNetworkQoS, added)).To(BeTrue())
				Expect(newNetworkQoS.Spec.Egress[0].DSCP).To(Equal(42))
			},
			DeleteFunc: func(obj interface{}) {
				networkQoS := obj.(*networkqos.NetworkQoS)
				Expect(reflect.DeepEqual(networkQoS, added)).To(BeTrue())
			},
		})

		networkQoSes = append(networkQoSes, added)
		networkQoSWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		added.Spec.Egress[0].DSCP = 42
		networkQoSWatch.Modify(added)
		Eventually(c.getUpdated, 2).Should(Equal(1))
		networkQoSes = networkQoSes[:0]
		networkQoSWatch.Delete(added)
		Eventually(c.getDeleted, 2).Should(Equal(1))

		wf.RemoveNetworkQoSHandler(h)
	})

	It("stops processing events after the handler is removed", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		added := newNamespace("default")
		h, c := addHandler(wf, NamespaceType, cache.ResourceEventHandlerFuncs{
			AddFunc:    func(interface{}) {},
			UpdateFunc: func(interface{}, interface{}) {},
			DeleteFunc: func(interface{}) {},
		})

		namespaces = append(namespaces, added)
		namespaceWatch.Add(added)
		Eventually(c.getAdded, 2).Should(Equal(1))
		wf.RemoveNamespaceHandler(h)

		added2 := newNamespace("other")
		namespaces = append(namespaces, added2)
		namespaceWatch.Add(added2)
		Consistently(c.getAdded, 2).Should(Equal(1))

		added2.Status.Phase = corev1.NamespaceTerminating
		namespaceWatch.Modify(added2)
		Consistently(c.getUpdated, 2).Should(Equal(0))
		namespaces = []*corev1.Namespace{added}
		namespaceWatch.Delete(added2)
		Consistently(c.getDeleted, 2).Should(Equal(0))
	})

	It("filters correctly by label and namespace", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		passesFilter := newPod("pod1", "default")
		passesFilter.ObjectMeta.Labels["blah"] = "foobar"
		failsFilter := newPod("pod2", "default")
		failsFilter.ObjectMeta.Labels["blah"] = "baz"
		failsFilter2 := newPod("pod3", "otherns")
		failsFilter2.ObjectMeta.Labels["blah"] = "foobar"

		sel, err := metav1.LabelSelectorAsSelector(
			&metav1.LabelSelector{
				MatchLabels: map[string]string{"blah": "foobar"},
			},
		)
		Expect(err).NotTo(HaveOccurred())

		_, c := addFilteredHandler(wf,
			PodType,
			PodType,
			"default",
			sel,
			cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					pod := obj.(*corev1.Pod)
					Expect(reflect.DeepEqual(pod, passesFilter)).To(BeTrue())
				},
				UpdateFunc: func(_, new interface{}) {
					newPod := new.(*corev1.Pod)
					Expect(reflect.DeepEqual(newPod, passesFilter)).To(BeTrue())
				},
				DeleteFunc: func(obj interface{}) {
					pod := obj.(*corev1.Pod)
					Expect(reflect.DeepEqual(pod, passesFilter)).To(BeTrue())
				},
			})

		pods = append(pods, passesFilter)
		podWatch.Add(passesFilter)
		Eventually(c.getAdded, 2).Should(Equal(1))

		// numAdded should remain 1
		pods = append(pods, failsFilter)
		podWatch.Add(failsFilter)
		Consistently(c.getAdded, 2).Should(Equal(1))

		// numAdded should remain 1
		pods = append(pods, failsFilter2)
		podWatch.Add(failsFilter2)
		Consistently(c.getAdded, 2).Should(Equal(1))

		passesFilter.Status.Phase = corev1.PodFailed
		podWatch.Modify(passesFilter)
		Eventually(c.getUpdated, 2).Should(Equal(1))

		// numAdded should remain 1
		failsFilter.Status.Phase = corev1.PodFailed
		podWatch.Modify(failsFilter)
		Consistently(c.getUpdated, 2).Should(Equal(1))

		failsFilter2.Status.Phase = corev1.PodFailed
		podWatch.Modify(failsFilter2)
		Consistently(c.getUpdated, 2).Should(Equal(1))

		pods = []*corev1.Pod{failsFilter, failsFilter2}
		podWatch.Delete(passesFilter)
		Eventually(c.getDeleted, 2).Should(Equal(1))
	})

	It("correctly handles object updates that cause filter changes", func() {
		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "test-node")
		Expect(err).NotTo(HaveOccurred())
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		pod := newPod("pod1", "default")
		pod.ObjectMeta.Labels["blah"] = "baz"

		sel, err := metav1.LabelSelectorAsSelector(
			&metav1.LabelSelector{
				MatchLabels: map[string]string{"blah": "foobar"},
			},
		)
		Expect(err).NotTo(HaveOccurred())

		equalPod := pod
		h, c := addFilteredHandler(wf,
			PodType,
			PodType,
			"default",
			sel,
			cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					p := obj.(*corev1.Pod)
					Expect(reflect.DeepEqual(p, equalPod)).To(BeTrue())
				},
				UpdateFunc: func(_, _ interface{}) {},
				DeleteFunc: func(obj interface{}) {
					p := obj.(*corev1.Pod)
					Expect(reflect.DeepEqual(p, equalPod)).To(BeTrue())
				},
			})

		pods = append(pods, pod)
		podCopy := pod.DeepCopy()
		podCopy2 := pod.DeepCopy()

		// Pod doesn't pass filter; shouldn't be added
		podWatch.Add(pod)
		Consistently(c.getAdded, 2).Should(Equal(0))

		// Update pod to pass filter; should be treated as add.  Need
		// to deep-copy pod when modifying because it's a pointer all
		// the way through when using FakeClient
		podCopy.ObjectMeta.Labels["blah"] = "foobar"
		pods = []*corev1.Pod{podCopy}
		equalPod = podCopy
		podWatch.Modify(podCopy)
		Eventually(c.getAdded, 2).Should(Equal(1))

		// Update pod to fail filter; should be treated as delete
		podCopy2.ObjectMeta.Labels["blah"] = "baz"
		podWatch.Modify(podCopy2)
		Eventually(c.getDeleted, 2).Should(Equal(1))
		Consistently(c.getAdded, 2).Should(Equal(1))
		Consistently(c.getUpdated, 2).Should(Equal(0))

		wf.RemovePodHandler(h)
	})
})

var _ = Describe("informerObjectTrim", func() {
	It("strips unnecessary node fields", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-node",
				Labels: map[string]string{
					"kubernetes.io/hostname": "test-node",
				},
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": `{"default":"10.128.0.0/23"}`,
				},
				ManagedFields: []metav1.ManagedFieldsEntry{
					{Manager: "kubelet"},
				},
				OwnerReferences: []metav1.OwnerReference{
					{Name: "owner"},
				},
				Finalizers: []string{"ovn-kubernetes.io/node-cleanup"},
			},
			Status: corev1.NodeStatus{
				Images: []corev1.ContainerImage{
					{Names: []string{"registry.io/image:latest"}, SizeBytes: 100000},
				},
				VolumesAttached: []corev1.AttachedVolume{
					{Name: "vol1", DevicePath: "/dev/sda"},
				},
				VolumesInUse: []corev1.UniqueVolumeName{"vol1"},
				Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				},
				Conditions: []corev1.NodeCondition{
					{
						Type:    corev1.NodeReady,
						Status:  corev1.ConditionTrue,
						Reason:  "KubeletReady",
						Message: "kubelet is posting ready status",
					},
				},
				Capacity: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("4"),
				},
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("3"),
				},
				DaemonEndpoints: corev1.NodeDaemonEndpoints{
					KubeletEndpoint: corev1.DaemonEndpoint{Port: 10250},
				},
				NodeInfo: corev1.NodeSystemInfo{
					KernelVersion: "5.14.0",
				},
				Config: &corev1.NodeConfigStatus{
					Active: &corev1.NodeConfigSource{
						ConfigMap: &corev1.ConfigMapNodeConfigSource{
							Name:      "kubelet-config",
							Namespace: "kube-system",
						},
					},
				},
				RuntimeHandlers: []corev1.NodeRuntimeHandler{
					{Name: "runc", Features: &corev1.NodeRuntimeHandlerFeatures{}},
				},
				Features: &corev1.NodeFeatures{},
			},
			Spec: corev1.NodeSpec{
				PodCIDR:  "10.128.0.0/23",
				PodCIDRs: []string{"10.128.0.0/23"},
				Taints: []corev1.Taint{
					{Key: "node-role.kubernetes.io/master", Effect: corev1.TaintEffectNoSchedule},
				},
			},
		}

		trimmed, err := informerObjectTrim(node)
		Expect(err).NotTo(HaveOccurred())
		n := trimmed.(*corev1.Node)

		// Verify fields that SHOULD be cleared
		Expect(n.Status.Images).To(BeNil(), "Status.Images should be cleared")
		Expect(n.Status.VolumesAttached).To(BeNil(), "Status.VolumesAttached should be cleared")
		Expect(n.Status.VolumesInUse).To(BeNil(), "Status.VolumesInUse should be cleared")
		Expect(n.OwnerReferences).To(BeNil(), "OwnerReferences should be cleared")
		Expect(n.ManagedFields).To(BeNil(), "ManagedFields should be cleared")
		Expect(n.Finalizers).To(BeNil(), "Finalizers should be cleared")
		Expect(n.Status.DaemonEndpoints).To(Equal(corev1.NodeDaemonEndpoints{}), "Status.DaemonEndpoints should be cleared")
		Expect(n.Status.NodeInfo).To(Equal(corev1.NodeSystemInfo{KernelVersion: "5.14.0"}), "Status.NodeInfo must be preserved")
		Expect(n.Status.Capacity).To(BeNil(), "Status.Capacity should be cleared")
		Expect(n.Status.Allocatable).To(BeNil(), "Status.Allocatable should be cleared")
		Expect(n.Status.Config).To(BeNil(), "Status.Config should be cleared")
		Expect(n.Status.RuntimeHandlers).To(BeNil(), "Status.RuntimeHandlers should be cleared")
		Expect(n.Status.Features).To(BeNil(), "Status.Features should be cleared")
		Expect(n.Spec.Taints).To(BeNil(), "Spec.Taints should be cleared")
		Expect(n.Spec.PodCIDRs).To(BeNil(), "Spec.PodCIDRs should be cleared")

		// Verify condition subfields: Type/Status/Reason/Message preserved
		Expect(n.Status.Conditions).To(HaveLen(1))
		Expect(n.Status.Conditions[0].Reason).To(Equal("KubeletReady"), "Condition.Reason must be preserved")
		Expect(n.Status.Conditions[0].Message).To(Equal("kubelet is posting ready status"), "Condition.Message must be preserved")

		// Verify fields that MUST be preserved
		Expect(n.Name).To(Equal("test-node"))
		Expect(n.Labels).To(HaveKey("kubernetes.io/hostname"))
		Expect(n.Annotations).To(HaveKey("k8s.ovn.org/node-subnets"))
		Expect(n.Status.Addresses).To(HaveLen(1))
		Expect(n.Spec.PodCIDR).To(Equal("10.128.0.0/23"))
	})
})

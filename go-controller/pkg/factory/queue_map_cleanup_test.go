// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

// stopQueueMapOnCleanup registers an idempotent queue shutdown and returns it for explicit use.
func stopQueueMapOnCleanup(t *testing.T, qm *queueMap, beforeStop func()) func() {
	t.Helper()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			if beforeStop != nil {
				beforeStop()
			}
			shutdownQueueMap(t, qm)
		})
	}
	t.Cleanup(stop)
	return stop
}

// TestInactiveSlotForgetsDeletedPod ensures idle queue entries are removed for object and tombstone deletes.
func TestInactiveSlotForgetsDeletedPod(t *testing.T) {
	for _, tombstone := range []bool{false, true} {
		t.Run(strconv.FormatBool(tombstone), func(t *testing.T) {
			qm := newQueueMap(1, 1, &sync.WaitGroup{}, make(chan struct{}))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"}}
			key := types.NamespacedName{Namespace: "ns", Name: "pod"}
			qm.entries[key] = &queueMapEntry{}

			inf := &informer{oType: PodType, internalInformers: []*internalInformer{{queueMap: qm}}}
			var deleted interface{} = pod
			if tombstone {
				deleted = cache.DeletedFinalStateUnknown{Key: "ns/pod", Obj: pod}
			}
			inf.newFederatedQueuedHandler(0).OnDelete(deleted)

			if len(qm.entries) != 0 {
				t.Fatal("inactive slot retained deleted Pod key")
			}
			if got := qm.queue.Len(); got != 0 {
				t.Fatalf("inactive slot queued %d callbacks, want none", got)
			}
		})
	}
}

// TestQueueMapSerializesRecreatedKeyAfterForget keeps a recreated key behind its in-flight callback.
func TestQueueMapSerializesRecreatedKeyAfterForget(t *testing.T) {
	var wg sync.WaitGroup
	qm := newQueueMap(1, 2, &wg, make(chan struct{}))
	qm.start()

	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: types.UID("old")}}
	newPod := oldPod.DeepCopy()
	newPod.UID = types.UID("new")
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	var releaseOldOnce sync.Once
	callbacks := make(chan string, 4)
	stop := stopQueueMapOnCleanup(t, qm, func() { releaseOldOnce.Do(func() { close(releaseOld) }) })

	qm.enqueueEvent(nil, oldPod, PodType, false, func(*event) {
		close(oldStarted)
		<-releaseOld
		callbacks <- "old complete"
	})
	select {
	case <-oldStarted:
	case <-time.After(time.Second):
		t.Fatal("old object callback did not start")
	}

	probeStarted := make(chan struct{})
	probeRelease := make(chan struct{})
	qm.enqueueEvent(nil, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "ns"}}, PodType, false, func(*event) {
		close(probeStarted)
		<-probeRelease
	})
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("unrelated key did not progress while the old key was blocked")
	}
	close(probeRelease)

	qm.forgetDeletedObject(PodType, oldPod)
	newStarted := make(chan struct{})
	qm.enqueueEvent(nil, newPod, PodType, false, func(*event) {
		close(newStarted)
		callbacks <- "new complete"
	})
	barrierProcessed := make(chan struct{})
	qm.enqueueEvent(nil, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "barrier", Namespace: "ns"}}, PodType, false, func(*event) {
		close(barrierProcessed)
	})
	select {
	case <-barrierProcessed:
	case <-time.After(time.Second):
		t.Fatal("second worker did not process the unrelated barrier key")
	}
	select {
	case <-newStarted:
		t.Fatal("replacement callback ran before the old callback completed")
	default:
	}

	releaseOldOnce.Do(func() { close(releaseOld) })
	for _, want := range []string{"old complete", "new complete"} {
		select {
		case got := <-callbacks:
			if got != want {
				t.Fatalf("callback = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for callback %q", want)
		}
	}
	stop()

	qm.Lock()
	defer qm.Unlock()
	if len(qm.entries) != 0 {
		t.Fatalf("queue map retained %d idle keys after shutdown", len(qm.entries))
	}
}

// TestQueueMapConcurrentProducersAndConsumers checks latest-state delivery with concurrent producers and workers.
func TestQueueMapConcurrentProducersAndConsumers(t *testing.T) {
	const (
		producerCount = 16
		updatesPerKey = 100
	)
	var workerWG sync.WaitGroup
	qm := newQueueMap(1, 4, &workerWG, make(chan struct{}))
	qm.start()
	finalKeys := make(chan string, producerCount)
	var producerWG sync.WaitGroup
	for i := 0; i < producerCount; i++ {
		name := "namespace-" + strconv.Itoa(i)
		producerWG.Add(1)
		go func() {
			defer producerWG.Done()
			oldNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name)}}
			process := func(e *event) {
				if e.obj.(*corev1.Namespace).ResourceVersion == strconv.Itoa(updatesPerKey) {
					finalKeys <- name
				}
			}
			for rv := 1; rv <= updatesPerKey; rv++ {
				newNamespace := oldNamespace.DeepCopy()
				newNamespace.ResourceVersion = strconv.Itoa(rv)
				qm.enqueueEvent(oldNamespace, newNamespace, NamespaceType, false, process)
				oldNamespace = newNamespace
			}
		}()
	}
	producerWG.Wait()
	stop := stopQueueMapOnCleanup(t, qm, nil)
	stop()

	seen := make(map[string]struct{}, producerCount)
	for i := 0; i < producerCount; i++ {
		select {
		case key := <-finalKeys:
			if _, ok := seen[key]; ok {
				t.Fatalf("final state for %s was delivered more than once", key)
			}
			seen[key] = struct{}{}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for latest state from every producer")
		}
	}
	if len(seen) != producerCount {
		t.Fatalf("final states delivered for %d keys, want %d", len(seen), producerCount)
	}
}

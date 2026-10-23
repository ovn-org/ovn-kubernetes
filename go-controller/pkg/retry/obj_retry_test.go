// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package retry

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type terminalPodEventHandler struct {
	DefaultEventHandler
	pod        *corev1.Pod
	adds       int
	updates    int
	deletePod  func(*corev1.Pod, interface{}) error
	cacheEntry interface{}
}

func (h *terminalPodEventHandler) AddResource(interface{}, bool) error {
	h.adds++
	return nil
}

func (h *terminalPodEventHandler) UpdateResource(interface{}, interface{}, bool) error {
	h.updates++
	return nil
}

func (h *terminalPodEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	return h.deletePod(obj.(*corev1.Pod), cachedObj)
}

func (h *terminalPodEventHandler) GetResourceFromInformerCache(string) (interface{}, error) {
	return h.pod, nil
}

func (h *terminalPodEventHandler) GetInternalCacheEntry(interface{}) interface{} {
	return h.cacheEntry
}

func (h *terminalPodEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	return util.PodCompleted(obj.(*corev1.Pod))
}

func (h *terminalPodEventHandler) FilterOutResource(interface{}) bool { return false }

func TestResourceRetryCompletedPod(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		completedBeforeQueue bool
		alreadyProcessed     bool
		pendingDeletion      bool
		replacement          bool
		failDeleteOnce       bool
		updateRetry          bool
	}{
		{name: "completed before enqueue", completedBeforeQueue: true},
		{name: "completed before retry"},
		{name: "already cleaned up", alreadyProcessed: true},
		{name: "terminal cleanup fails", failDeleteOnce: true},
		{name: "terminal cleanup still pending", alreadyProcessed: true, pendingDeletion: true, failDeleteOnce: true},
		{name: "replacement waits for old cleanup", pendingDeletion: true, replacement: true, failDeleteOnce: true},
		{name: "update retry becomes cleanup", updateRetry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "namespace", Name: "pod", UID: "current"},
				Spec:       corev1.PodSpec{NodeName: "node"},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			}
			completed := pod.DeepCopy()
			completed.Status.Phase = corev1.PodSucceeded
			handler := &terminalPodEventHandler{pod: pod, cacheEntry: "current cached state"}
			r := NewRetryFramework("test", nil, nil, nil, &ResourceHandler{
				ObjType:                factory.PodType,
				NeedsUpdateDuringRetry: tc.updateRetry,
				EventHandler:           handler,
			})
			key, err := GetResourceKey(pod)
			require.NoError(t, err)
			if tc.alreadyProcessed {
				r.terminatedObjects.Store(key, true)
			}
			oldPod := pod.DeepCopy()
			if tc.replacement {
				oldPod.UID = "old"
			}
			if tc.pendingDeletion {
				r.initRetryObjWithDelete(oldPod, key, "pending cached state", true)
			}
			if tc.completedBeforeQueue {
				handler.pod = completed
			}
			// A delayed retry request can enqueue a running snapshot after completion.
			require.NoError(t, r.AddRetryObjWithAddNoBackoff(pod))
			if tc.updateRetry {
				r.initRetryObjWithUpdate(pod, pod, key)
			}
			handler.pod = completed

			var deleted []types.UID
			failDelete := tc.failDeleteOnce
			handler.deletePod = func(deleting *corev1.Pod, cachedObj interface{}) error {
				if tc.pendingDeletion && len(deleted) == 0 {
					require.Equal(t, oldPod.UID, deleting.UID)
					require.Equal(t, "pending cached state", cachedObj)
				} else {
					require.Equal(t, completed, deleting)
					require.Equal(t, handler.cacheEntry, cachedObj)
				}
				if failDelete {
					failDelete = false
					return errors.New("cleanup failed")
				}
				deleted = append(deleted, deleting.UID)
				return nil
			}

			r.resourceRetry(key, time.Now())
			if tc.failDeleteOnce {
				entry, found := r.getRetryObj(key)
				require.True(t, found, "failed cleanup must remain retryable")
				require.NotNil(t, entry.oldObj)
				require.EqualValues(t, 1, entry.failedAttempts)
				r.setRetryObjWithNoBackoff(entry)
				r.resourceRetry(key, time.Now())
			}
			var expected []types.UID
			if tc.pendingDeletion {
				expected = append(expected, oldPod.UID)
			}
			if !tc.alreadyProcessed {
				expected = append(expected, pod.UID)
			}
			require.Equal(t, expected, deleted)
			require.Zero(t, handler.adds)
			require.Zero(t, handler.updates)
			_, found := r.getRetryObj(key)
			require.False(t, found)
		})
	}
}

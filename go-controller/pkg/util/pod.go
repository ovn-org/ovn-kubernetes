// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
)

const (
	podAnnotationCachePollInterval = 20 * time.Millisecond
	podAnnotationCacheWaitTimeout  = 500 * time.Millisecond
)

// AllocateToPodWithRollbackFunc is a function used to allocate a resource to a
// pod that depends on the current state of the pod, and possibly updating it.
// To be used with UpdatePodWithAllocationOrRollback. Implementations can return
// a nil pod if no update is warranted. Implementations can also return a
// rollback function that will be invoked if the pod update fails.
type AllocateToPodWithRollbackFunc func(pod *corev1.Pod) (*corev1.Pod, func(), error)

// IsPodAnnotationUpdateRetryable returns true for errors that should trigger a
// relist-and-retry when updating pod annotations.
//
// JSON Patch "test" failures from the apiserver are surfaced as Invalid rather
// than Conflict, so this retry path needs to handle both.
func IsPodAnnotationUpdateRetryable(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsInvalid(err)
}

// UpdatePodWithRetryOrRollback updates pod annotations with the result of the
// allocate function. If the pod update fails, it applies the rollback provided by
// the allocate function.
// If the informer observes a different UID, the original pod is treated as not found.
func UpdatePodWithRetryOrRollback(ctx context.Context, podLister listers.PodLister, kube kube.Interface, pod *corev1.Pod, allocate AllocateToPodWithRollbackFunc) error {
	start := time.Now()
	defer func() {
		klog.V(5).Infof("[%s/%s] pod update took %v", pod.Namespace, pod.Name, time.Since(start))
	}()
	var failedResourceVersion string
	var failedPatchErr error
	err := retry.OnError(OvnConflictBackoff, IsPodAnnotationUpdateRetryable, func() error {
		var oldPod *corev1.Pod
		var err error
		if failedResourceVersion == "" {
			oldPod, err = podLister.Pods(pod.Namespace).Get(pod.Name)
		} else {
			oldPod, err = waitForPodCacheUpdate(
				podLister,
				pod.Namespace,
				pod.Name,
				failedResourceVersion,
			)
			if errors.Is(err, context.DeadlineExceeded) {
				// Repeating a patch built from the same stale snapshot can only
				// fail its compare-and-swap test again. Let the caller requeue
				// instead of adding another request to an overloaded apiserver.
				return failedPatchErr
			}
		}
		if err != nil {
			return err
		}
		if pod.UID != "" && oldPod.UID != pod.UID {
			return fmt.Errorf("pod was replaced: expected UID %q, found %q: %w", pod.UID, oldPod.UID,
				apierrors.NewNotFound(corev1.Resource("pods"), pod.Name))
		}

		// Informer cache should not be mutated, so copy the object
		currentPod := oldPod.DeepCopy()
		updatedPod, rollback, err := allocate(currentPod)
		if err != nil {
			return err
		}

		if updatedPod == nil {
			return nil
		}

		err = kube.PatchPodStatusAnnotations(ctx, oldPod, updatedPod)
		if err != nil && rollback != nil {
			rollback()
		}
		if IsPodAnnotationUpdateRetryable(err) && oldPod.ResourceVersion != "" {
			failedResourceVersion = oldPod.ResourceVersion
			failedPatchErr = err
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to update pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

func waitForPodCacheUpdate(podLister listers.PodLister, namespace, name,
	oldResourceVersion string) (*corev1.Pod, error) {
	var pod *corev1.Pod
	err := wait.PollUntilContextTimeout(
		context.Background(),
		podAnnotationCachePollInterval,
		podAnnotationCacheWaitTimeout,
		true,
		func(_ context.Context) (bool, error) {
			var err error
			pod, err = podLister.Pods(namespace).Get(name)
			if err != nil {
				return false, err
			}
			return pod.ResourceVersion != oldResourceVersion, nil
		},
	)
	return pod, err
}

package util

import (
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
)

// AllocateToPodWithRollbackFunc is a function used to allocate a resource to a
// pod that depends on the current state of the pod, and possibly updating it.
// To be used with UpdatePodWithAllocationOrRollback. Implementations can return
// a nil pod if no update is warranted. Implementations can also return a
// rollback function that will be invoked if the pod update fails.
type AllocateToPodWithRollbackFunc func(pod *v1.Pod) (*v1.Pod, func(), error)

// UpdatePodWithRetryOrRollback updates the pod with the result of the
// allocate function. If the pod update fails, it applies the rollback provided by
// the allocate function.
func UpdatePodWithRetryOrRollback(podLister listers.PodLister, kube kube.Interface, pod *v1.Pod, allocate AllocateToPodWithRollbackFunc) error {
	start := time.Now()
	var updated bool

	err := retry.RetryOnConflict(OvnConflictBackoff, func() error {
		pod, err := podLister.Pods(pod.Namespace).Get(pod.Name)
		if err != nil {
			return err
		}

		// Informer cache should not be mutated, so copy the object
		pod = pod.DeepCopy()
		pod, rollback, err := allocate(pod)
		if err != nil {
			return err
		}

		if pod == nil {
			return nil
		}

		updated = true
		// It is possible to update the pod annotations using status subresource
		// because changes to metadata via status subresource are not restricted pods.
		err = kube.UpdatePodStatus(pod)
		if err != nil && rollback != nil {
			rollback()
		}

		return err
	})

	if err != nil {
		return fmt.Errorf("failed to update pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	if updated {
		klog.Infof("[%s/%s] pod update took %v", pod.Namespace, pod.Name, time.Since(start))
	}

	return nil
}

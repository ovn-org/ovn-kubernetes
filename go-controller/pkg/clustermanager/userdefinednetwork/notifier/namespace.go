package notifier

import (
	"errors"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	corev1informer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
)

type NamespaceReconciler interface {
	ReconcileNamespace(key string) error
}

// NamespaceNotifier watches Namespaces objects and notify subscribers upon change.
// It enqueues the reconciled object keys in the subscribing controllers workqueue.
type NamespaceNotifier struct {
	Controller controller.Controller

	subscribers []NamespaceReconciler
}

func NewNamespaceNotifier(nsInformer corev1informer.NamespaceInformer, subscribers ...NamespaceReconciler) *NamespaceNotifier {
	c := &NamespaceNotifier{
		subscribers: subscribers,
	}

	nsLister := nsInformer.Lister()
	cfg := &controller.ControllerConfig[corev1.Namespace]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		ObjNeedsUpdate: c.needUpdate,
		Threadiness:    1,
		Informer:       nsInformer.Informer(),
		Lister:         nsLister.List,
	}
	c.Controller = controller.NewController[corev1.Namespace]("udn-namespace-controller", cfg)

	return c
}

// needUpdate return true when the namespace has been deleted or created.
func (c *NamespaceNotifier) needUpdate(old, new *corev1.Namespace) bool {
	nsCreated := old == nil && new != nil
	nsDeleted := old != nil && new == nil
	nsLabelsChanged := old != nil && new != nil &&
		!reflect.DeepEqual(old.Labels, new.Labels)

	return nsCreated || nsDeleted || nsLabelsChanged
}

// reconcile notify subscribers with the request namespace key following namespace events.
func (c *NamespaceNotifier) reconcile(key string) error {
	var errs []error
	for _, subscriber := range c.subscribers {
		if subscriber != nil {
			// enqueue the reconciled NAD key in the subscribers workqueue to
			// enable the subscriber act on NAD changes (e.g.: reflect NAD state is status)
			if err := subscriber.ReconcileNamespace(key); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}

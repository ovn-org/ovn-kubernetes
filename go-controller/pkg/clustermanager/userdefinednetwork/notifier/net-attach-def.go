package notifier

import (
	"k8s.io/client-go/util/workqueue"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netv1infomer "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
)

type NetAttachDefReconciler interface {
	ReconcileNetAttachDef(key string) error
}

// NetAttachDefNotifier watches NetworkAttachmentDefinition objects and notify subscribers upon change.
// It enqueues the reconciled object keys in the subscribing controllers workqueue.
type NetAttachDefNotifier struct {
	Controller controller.Controller

	subscribers []NetAttachDefReconciler
}

func NewNetAttachDefNotifier(nadInfomer netv1infomer.NetworkAttachmentDefinitionInformer, subscribers ...NetAttachDefReconciler) *NetAttachDefNotifier {
	c := &NetAttachDefNotifier{
		subscribers: subscribers,
	}

	nadLister := nadInfomer.Lister()
	cfg := &controller.ControllerConfig[netv1.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		ObjNeedsUpdate: c.needUpdate,
		Threadiness:    1,
		Informer:       nadInfomer.Informer(),
		Lister:         nadLister.List,
	}
	c.Controller = controller.NewController[netv1.NetworkAttachmentDefinition]("udn-nad-controller", cfg)

	return c
}

func (c *NetAttachDefNotifier) needUpdate(_, _ *netv1.NetworkAttachmentDefinition) bool {
	return true
}

func (c *NetAttachDefNotifier) reconcile(key string) error {
	for _, subscriber := range c.subscribers {
		if subscriber != nil {
			// enqueue the reconciled NAD key in the subscribers workqueue to
			// enable the subscriber act on NAD changes (e.g.: reflect NAD state is status)
			if err := subscriber.ReconcileNetAttachDef(key); err != nil {
				return err
			}
		}
	}

	return nil
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package notifier

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/informers/externalversions/vtep/v1"
)

// VTEPReconciler is the interface for controllers that need to react to VTEP events.
type VTEPReconciler interface {
	ReconcileVTEP(key string) error
}

// VTEPNotifier watches VTEP objects and notifies subscribers upon change.
// It enqueues the reconciled object keys in the subscribing controllers workqueue.
type VTEPNotifier struct {
	Controller controller.Controller

	subscribers []VTEPReconciler
}

// NewVTEPNotifier creates a new VTEPNotifier that watches VTEP CRs and notifies subscribers.
func NewVTEPNotifier(vtepInformer vtepinformer.VTEPInformer, subscribers ...VTEPReconciler) *VTEPNotifier {
	c := &VTEPNotifier{
		subscribers: subscribers,
	}

	vtepLister := vtepInformer.Lister()
	cfg := &controller.ControllerConfig[vtepv1.VTEP]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		ObjNeedsUpdate: c.needUpdate,
		Threadiness:    1,
		Informer:       vtepInformer.Informer(),
		Lister:         vtepLister.List,
	}
	c.Controller = controller.NewController("udn-vtep-controller", cfg)

	return c
}

// needUpdate returns true when the VTEP has been created, deleted, or its
// Accepted condition has changed. CUDNs referencing this VTEP must be
// re-queued so that NAD creation is gated on Accepted=True.
// IMPORTANT: Before adding further update triggers, verify that all subscribers
// can handle increased event frequency.
func (c *VTEPNotifier) needUpdate(old, new *vtepv1.VTEP) bool {
	if old == nil || new == nil {
		return true
	}

	return IsVTEPAccepted(old.Status.Conditions) != IsVTEPAccepted(new.Status.Conditions)
}

// IsVTEPAccepted checks if a VTEP CR has Accepted=True condition.
func IsVTEPAccepted(conditions []metav1.Condition) bool {
	condition := meta.FindStatusCondition(conditions, "Accepted")
	return condition != nil && condition.Status == metav1.ConditionTrue
}

// reconcile notifies subscribers with the VTEP key following VTEP events.
func (c *VTEPNotifier) reconcile(_ context.Context, key string) error {
	var errs []error
	for _, subscriber := range c.subscribers {
		if subscriber != nil {
			// enqueue the reconciled VTEP key in the subscribers workqueue to
			// enable the subscriber to act on VTEP changes
			if err := subscriber.ReconcileVTEP(key); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}

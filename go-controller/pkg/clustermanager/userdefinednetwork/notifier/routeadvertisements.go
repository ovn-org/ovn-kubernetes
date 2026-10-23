// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package notifier

import (
	"context"
	"errors"
	"reflect"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	rainformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/informers/externalversions/routeadvertisements/v1"
)

// RouteAdvertisementsReconciler is the interface for controllers that need to react to RouteAdvertisements events.
type RouteAdvertisementsReconciler interface {
	ReconcileRouteAdvertisements(key string) error
}

// RouteAdvertisementsNotifier watches RouteAdvertisements objects and notifies subscribers upon change.
// It enqueues the reconciled object keys in the subscribing controllers workqueue.
type RouteAdvertisementsNotifier struct {
	Controller controller.Controller

	subscribers []RouteAdvertisementsReconciler
}

// NewRouteAdvertisementsNotifier creates a new RouteAdvertisementsNotifier that watches RouteAdvertisements CRs and notifies subscribers.
func NewRouteAdvertisementsNotifier(raInformer rainformer.RouteAdvertisementsInformer, subscribers ...RouteAdvertisementsReconciler) *RouteAdvertisementsNotifier {
	c := &RouteAdvertisementsNotifier{
		subscribers: subscribers,
	}

	raLister := raInformer.Lister()
	cfg := &controller.ControllerConfig[ratypes.RouteAdvertisements]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		ObjNeedsUpdate: c.needUpdate,
		Threadiness:    1,
		Informer:       raInformer.Informer(),
		Lister:         raLister.List,
	}
	c.Controller = controller.NewController("udn-routeadvertisements-controller", cfg)

	return c
}

// needUpdate returns true when the RouteAdvertisements CR has been created, deleted, or fields relevant
// to CUDN transport validation and Uplink VRF placement have changed.
//
// Per OKEP, CUDN validation checks:
// 1. Whether the RA selects the CUDN (via NetworkSelectors)
// 2. Whether it advertises PodNetwork (via Advertisements)
// 3. Whether the RA is Accepted (via Status.Conditions)
//
// Therefore, we only notify on changes to these specific fields.
func (c *RouteAdvertisementsNotifier) needUpdate(old, new *ratypes.RouteAdvertisements) bool {
	// Notify on create or delete
	if old == nil || new == nil {
		return true
	}

	// Check if NetworkSelectors changed - determines which CUDNs are selected
	if !reflect.DeepEqual(old.Spec.NetworkSelectors, new.Spec.NetworkSelectors) {
		return true
	}

	// Check if Advertisements changed - determines if PodNetwork is advertised
	if !reflect.DeepEqual(old.Spec.Advertisements, new.Spec.Advertisements) {
		return true
	}

	// Check if TargetVRF changed - determines whether an Uplink bridge must
	// be kept in the default VRF or enslaved to the selected UDN VRF.
	if old.Spec.TargetVRF != new.Spec.TargetVRF {
		return true
	}

	// Check if Accepted condition changed - determines if RA configuration is valid
	if IsRAAccepted(old.Status.Conditions) != IsRAAccepted(new.Status.Conditions) {
		return true
	}

	return false
}

// IsRAAccepted checks if a RouteAdvertisements CR has Accepted=True condition
func IsRAAccepted(conditions []metav1.Condition) bool {
	condition := meta.FindStatusCondition(conditions, ratypes.RouteAdvertisementsAccepted)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

// reconcile notifies subscribers with the RouteAdvertisements key following RouteAdvertisements events.
func (c *RouteAdvertisementsNotifier) reconcile(_ context.Context, key string) error {
	var errs []error
	for _, subscriber := range c.subscribers {
		if subscriber != nil {
			// enqueue the reconciled RouteAdvertisements key in the subscribers workqueue to
			// enable the subscriber to act on RouteAdvertisements changes
			if err := subscriber.ReconcileRouteAdvertisements(key); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}

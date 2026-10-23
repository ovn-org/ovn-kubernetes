// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package status_manager

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	adminpolicybasedrouteapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/applyconfiguration/adminpolicybasedroute/v1"
	adminpolicybasedrouteclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	adminpolicybasedroutelisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

type apbRouteManager struct {
	lister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister
	client adminpolicybasedrouteclientset.Interface
}

func newAPBRouteManager(lister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister, client adminpolicybasedrouteclientset.Interface) *apbRouteManager {
	return &apbRouteManager{
		lister: lister,
		client: client,
	}
}

//lint:ignore U1000 generic interfaces throw false-positives https://github.com/dominikh/go-tools/issues/1440
func (m *apbRouteManager) get(_, name string) (*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, error) {
	return m.lister.Get(name)
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *apbRouteManager) getMessages(route *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) []string {
	return route.Status.Messages
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *apbRouteManager) getManagedFields(route *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) []metav1.ManagedFieldsEntry {
	return route.ManagedFields
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *apbRouteManager) updateStatus(ctx context.Context, route *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, applyOpts *metav1.ApplyOptions,
	applyEmptyOrFailed bool) error {
	if route == nil {
		return nil
	}
	newStatus := adminpolicybasedrouteapi.SuccessStatus
	for _, message := range route.Status.Messages {
		if strings.Contains(message, types.APBRouteErrorMsg) {
			newStatus = adminpolicybasedrouteapi.FailStatus
			break
		}
	}
	if applyEmptyOrFailed && newStatus != adminpolicybasedrouteapi.FailStatus {
		newStatus = ""
	}

	if route.Status.Status == newStatus {
		// already set to the same value
		return nil
	}

	applyStatus := adminpolicybasedrouteapply.AdminPolicyBasedRouteStatus()

	if newStatus != "" {
		applyStatus.WithStatus(newStatus)
	}

	applyObj := adminpolicybasedrouteapply.AdminPolicyBasedExternalRoute(route.Name).
		WithStatus(applyStatus)

	_, err := m.client.K8sV1().AdminPolicyBasedExternalRoutes().ApplyStatus(ctx, applyObj, *applyOpts)
	return err
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *apbRouteManager) cleanupStatus(ctx context.Context, route *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, applyOpts *metav1.ApplyOptions) error {
	applyObj := adminpolicybasedrouteapply.AdminPolicyBasedExternalRoute(route.Name)
	_, err := m.client.K8sV1().AdminPolicyBasedExternalRoutes().ApplyStatus(ctx, applyObj, *applyOpts)
	return err
}

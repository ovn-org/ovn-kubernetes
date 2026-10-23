// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package statusmetrics

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"

	adminpolicybasedrouteapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/applyconfiguration/adminpolicybasedroute/v1"
	adminpolicybasedrouteclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	adminpolicybasedroutelisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const fullMetricAPBExternalRoute = types.MetricOvnkubeNamespace + "_" + metrics.MetricAdminPolicyBasedExternalRouteSyncSucceeded

type apbRouteRoller struct {
	lister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister
	client adminpolicybasedrouteclientset.Interface
}

// NewAPBExternalRouteRoller creates a Prometheus-backed rollup for APBExternalRoute.
func NewAPBExternalRouteRoller(
	lister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister,
	client adminpolicybasedrouteclientset.Interface,
) ResourceRoller {
	return &apbRouteRoller{lister: lister, client: client}
}

func (r *apbRouteRoller) Name() string       { return "adminpolicybasedexternalroute" }
func (r *apbRouteRoller) MetricName() string { return fullMetricAPBExternalRoute }

func (r *apbRouteRoller) ListKeys() ([]ResourceKey, error) {
	objs, err := r.lister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	keys := make([]ResourceKey, 0, len(objs))
	for _, o := range objs {
		keys = append(keys, ResourceKey{Name: o.Name})
	}
	return keys, nil
}

func (r *apbRouteRoller) RelevantNodes(_ ResourceKey, zones sets.Set[string]) (sets.Set[string], error) {
	return zones.Clone(), nil
}

func (r *apbRouteRoller) PatchSummary(key ResourceKey, outcome AggregateOutcome, _ []string) error {
	route, err := r.lister.Get(key.Name)
	if err != nil {
		return err
	}

	// APB status.status is Success/Fail (enum); failing node names are not
	// stored on this field — Prometheus remains the source for the node list.
	var newStatus adminpolicybasedrouteapi.StatusType
	switch outcome {
	case OutcomeSuccess:
		newStatus = adminpolicybasedrouteapi.SuccessStatus
	case OutcomeFailure:
		newStatus = adminpolicybasedrouteapi.FailStatus
	case OutcomeIncomplete:
		if route.Status.Status == adminpolicybasedrouteapi.FailStatus {
			newStatus = adminpolicybasedrouteapi.FailStatus
		} else {
			newStatus = ""
		}
	}

	if route.Status.Status == newStatus {
		return nil
	}

	applyStatus := adminpolicybasedrouteapply.AdminPolicyBasedRouteStatus()
	if newStatus != "" {
		applyStatus.WithStatus(newStatus)
	}
	applyObj := adminpolicybasedrouteapply.AdminPolicyBasedExternalRoute(route.Name).WithStatus(applyStatus)
	_, err = r.client.K8sV1().AdminPolicyBasedExternalRoutes().ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{
		Force:        true,
		FieldManager: clusterManagerName,
	})
	return err
}

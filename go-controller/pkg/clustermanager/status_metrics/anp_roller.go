// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package statusmetrics

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	anpapiapply "sigs.k8s.io/network-policy-api/pkg/client/applyconfiguration/apis/v1alpha1"
	anpclientset "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned"
	anplisters "sigs.k8s.io/network-policy-api/pkg/client/listers/apis/v1alpha1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const (
	aggregateReadyConditionType = "Ready"
	aggregateReadyReason        = "MetricsRollup"
	fullMetricANP               = types.MetricOvnkubeNamespace + "_" + metrics.MetricAdminNetworkPolicySyncSucceeded
	fullMetricBANP              = types.MetricOvnkubeNamespace + "_" + metrics.MetricBaselineAdminNetworkPolicySyncSucceeded
)

type anpRoller struct {
	lister anplisters.AdminNetworkPolicyLister
	client anpclientset.Interface
}

// NewAdminNetworkPolicyRoller creates a Prometheus-backed rollup for ANP.
func NewAdminNetworkPolicyRoller(lister anplisters.AdminNetworkPolicyLister, client anpclientset.Interface) ResourceRoller {
	return &anpRoller{lister: lister, client: client}
}

func (r *anpRoller) Name() string       { return "adminnetworkpolicy" }
func (r *anpRoller) MetricName() string { return fullMetricANP }

func (r *anpRoller) ListKeys() ([]ResourceKey, error) {
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

func (r *anpRoller) RelevantNodes(_ ResourceKey, zones sets.Set[string]) (sets.Set[string], error) {
	return zones.Clone(), nil
}

func (r *anpRoller) PatchSummary(key ResourceKey, outcome AggregateOutcome, failingNodes []string) error {
	anp, err := r.lister.Get(key.Name)
	if err != nil {
		return err
	}
	cond, skip := aggregateReadyCondition(outcome, failingNodes)
	if skip {
		return nil
	}
	existing := meta.FindStatusCondition(anp.Status.Conditions, aggregateReadyConditionType)
	if existing != nil && existing.Status == cond.Status && existing.Reason == cond.Reason && existing.Message == cond.Message {
		return nil
	}
	applyObj := anpapiapply.AdminNetworkPolicy(key.Name).
		WithStatus(anpapiapply.AdminNetworkPolicyStatus().WithConditions(cond))
	_, err = r.client.PolicyV1alpha1().AdminNetworkPolicies().
		ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: clusterManagerName, Force: true})
	return err
}

type banpRoller struct {
	lister anplisters.BaselineAdminNetworkPolicyLister
	client anpclientset.Interface
}

// NewBaselineAdminNetworkPolicyRoller creates a Prometheus-backed rollup for BANP.
func NewBaselineAdminNetworkPolicyRoller(lister anplisters.BaselineAdminNetworkPolicyLister, client anpclientset.Interface) ResourceRoller {
	return &banpRoller{lister: lister, client: client}
}

func (r *banpRoller) Name() string       { return "baselineadminnetworkpolicy" }
func (r *banpRoller) MetricName() string { return fullMetricBANP }

func (r *banpRoller) ListKeys() ([]ResourceKey, error) {
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

func (r *banpRoller) RelevantNodes(_ ResourceKey, zones sets.Set[string]) (sets.Set[string], error) {
	return zones.Clone(), nil
}

func (r *banpRoller) PatchSummary(key ResourceKey, outcome AggregateOutcome, failingNodes []string) error {
	banp, err := r.lister.Get(key.Name)
	if err != nil {
		return err
	}
	cond, skip := aggregateReadyCondition(outcome, failingNodes)
	if skip {
		return nil
	}
	existing := meta.FindStatusCondition(banp.Status.Conditions, aggregateReadyConditionType)
	if existing != nil && existing.Status == cond.Status && existing.Reason == cond.Reason && existing.Message == cond.Message {
		return nil
	}
	applyObj := anpapiapply.BaselineAdminNetworkPolicy(key.Name).
		WithStatus(anpapiapply.BaselineAdminNetworkPolicyStatus().WithConditions(cond))
	_, err = r.client.PolicyV1alpha1().BaselineAdminNetworkPolicies().
		ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: clusterManagerName, Force: true})
	return err
}

func aggregateReadyCondition(outcome AggregateOutcome, failingNodes []string) (metav1.Condition, bool) {
	cond := metav1.Condition{
		Type:               aggregateReadyConditionType,
		Reason:             aggregateReadyReason,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}
	switch outcome {
	case OutcomeSuccess:
		cond.Status = metav1.ConditionTrue
		cond.Message = "All relevant nodes synced successfully"
	case OutcomeFailure:
		cond.Status = metav1.ConditionFalse
		cond.Message = "One or more nodes failed to sync"
		if msg := FormatFailingNodesMessage(failingNodes); msg != "" {
			cond.Message = fmt.Sprintf("%s (%s)", cond.Message, msg)
		}
	case OutcomeIncomplete:
		// Do not patch a success outcome while incomplete.
		return metav1.Condition{}, true
	}
	return cond, false
}

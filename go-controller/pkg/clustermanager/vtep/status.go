// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metaapply "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/klog/v2"

	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/applyconfiguration/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
)

const (
	fieldManager = "clustermanager-vtep-controller"

	conditionTypeAccepted = "Accepted"

	reasonManagedModeNotSupported = "ManagedModeNotSupported"
	reasonAllocated               = "Allocated"
	reasonAllocationFailed        = "AllocationFailed"
	reasonCIDROverlap             = "CIDROverlap"
	reasonEVPNIPv6NotSupported    = "EVPNIPv6NotSupported"
)

// updateStatusCondition applies the status condition to the VTEP resource.
// The API update is skipped if the condition already matches.
func (c *Controller) updateStatusCondition(ctx context.Context, vtep *vtepv1.VTEP, conditionType string, status metav1.ConditionStatus, reason, message string) error {
	const maxMessageLen = 32768
	if len(message) >= maxMessageLen {
		message = message[:maxMessageLen-1]
	}

	existingCondition := meta.FindStatusCondition(vtep.Status.Conditions, conditionType)
	if existingCondition != nil &&
		existingCondition.Status == status &&
		existingCondition.Reason == reason &&
		existingCondition.Message == message {
		// Record the metric from the existing API-confirmed condition so it is
		// populated after controller restarts, where the informer fires synthetic
		// creates for all VTEPs but the condition hasn't changed.
		metrics.RecordVTEPCondition(vtep.Name, conditionType, status)
		return nil
	}

	condition := metaapply.Condition().
		WithType(conditionType).
		WithStatus(status).
		WithReason(reason).
		WithMessage(message)

	now := metav1.NewTime(time.Now())
	if existingCondition != nil && existingCondition.Status == status {
		now = existingCondition.LastTransitionTime
	}
	condition = condition.WithLastTransitionTime(now)

	// NOTE:SSA merges conditions by the "type" key (+listMapKey=type). Since this
	// controller only sets a single condition type per field manager, applying
	// one condition at a time is safe. If a second condition type is ever added
	// under the same field manager, all owned conditions must be included in
	// every apply call to avoid wiping the others.
	_, err := c.vtepClient.K8sV1().VTEPs().ApplyStatus(
		ctx,
		vtepapply.VTEP(vtep.Name).WithStatus(
			vtepapply.VTEPStatus().WithConditions(condition),
		),
		metav1.ApplyOptions{
			FieldManager: fieldManager,
			Force:        true,
		},
	)
	if err != nil {
		klog.Errorf("Failed to update status condition %q for VTEP %s: %v", conditionType, vtep.Name, err)
		return fmt.Errorf("failed to update status condition %q for VTEP %s: %w", conditionType, vtep.Name, err)
	}
	metrics.RecordVTEPCondition(vtep.Name, conditionType, status)
	return nil
}

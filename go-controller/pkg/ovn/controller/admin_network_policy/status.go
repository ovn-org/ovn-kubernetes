// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package adminnetworkpolicy

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	anpapiapply "sigs.k8s.io/network-policy-api/pkg/client/applyconfiguration/apis/v1alpha1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
)

// Defined status.type fields for Admin Network Policy - This is prefixed with the zone name thus
// creating one row per zone in the metav1.Condition array
// NOTE: On every update of ANP, related pods and namespaces - if anything goes wrong this
// this status type flaps between true and false. Users can use this to narrow down the malfunctioning zone
// (DANGER): If this feature is used at 500-1000 node scale, then that many status rows will be created
// and a 1000 controllers will be updating the same policy CRD  ¯\_(ツ)_/¯. However we are using server-side apply
// which should ease the scale issues. We have tested at 120 nodes and status works fine at that scale.
/* Sample Output ~~~~~~~~~~
Status:
  Conditions:
    Last Transition Time:  2023-06-11T12:07:51Z
    Message:               Setting up OVN DB plumbing was successful
    Reason:                SetupSucceeded
    Status:                True
    Type:                  Ready-In-Zone-ovn-worker
    Last Transition Time:  2023-06-11T12:07:51Z
    Message:               Setting up OVN DB plumbing was successful
    Reason:                SetupSucceeded
    Status:                True
    Type:                  Ready-In-Zone-ovn-control-plane
    Last Transition Time:  2023-06-11T12:07:51Z
    Message:               Setting up OVN DB plumbing was successful
    Reason:                SetupSucceeded
    Status:                True
    Type:                  Ready-In-Zone-ovn-worker2
Events:                    <none>
*/
const (
	// conditions.type can have max 316 characters (zone names are max 273 so keep this under allowed range)
	policyReadyStatusType = "Ready-In-Zone-"
	// Defined status.reason fields for (Baseline)Admin Network Policy
	policyReadyReason    = "SetupSucceeded"
	policyNotReadyReason = "SetupFailed"
)

// doesStatusNeedAnUpdate compares the existing condition with the new condition
// and returns true if an update is needed, false if the status is already in the desired state.
// This helps avoid unnecessary API server calls when the status hasn't changed.
func doesStatusNeedAnUpdate(existingCondition *metav1.Condition, newCondition metav1.Condition) bool {
	if existingCondition == nil {
		return true // condition doesn't exist yet, needs to be created
	}
	// Check if Status, Reason, and Message are all the same - if so, no update needed
	if existingCondition.Status == newCondition.Status &&
		existingCondition.Reason == newCondition.Reason &&
		existingCondition.Message == newCondition.Message {
		return false
	}
	return true
}

// updateANPStatusToReady updates the status of the policy to reflect that it is ready
// Each zone's ovnkube-controller will call this, hence let's update status using server-side-apply
func (c *Controller) updateANPStatusToReady(anpName string) error {
	readyCondition := metav1.Condition{
		Type:    policyReadyStatusType + c.nodeName,
		Status:  metav1.ConditionTrue,
		Reason:  policyReadyReason,
		Message: "Setting up OVN DB plumbing was successful",
	}
	err := c.updateANPZoneStatusCondition(readyCondition, anpName)
	if err != nil {
		return fmt.Errorf("unable to update the status of ANP %s, err: %v", anpName, err)
	}
	return nil
}

// updateANPStatusToNotReady updates the status of the policy to reflect that it is not ready
// Each zone's ovnkube-controller will call this, hence let's update status using server-side-apply
// status.message must be less than 32768 characters and is usually the error that occurred which is passed
// to this function. Message is particularly useful as it can tell which zone's setup has not finished for
// this ANP instead of having to manually check logs across zones
func (c *Controller) updateANPStatusToNotReady(anpName, message string) error {
	if len(message) >= 32767 { // max length of message can be 32768
		message = message[:32766]
	}
	notReadyCondition := metav1.Condition{
		Type:    policyReadyStatusType + c.nodeName,
		Status:  metav1.ConditionFalse,
		Reason:  policyNotReadyReason,
		Message: message,
	}
	err := c.updateANPZoneStatusCondition(notReadyCondition, anpName)
	if err != nil {
		return fmt.Errorf("unable update the status of ANP %s, err: %v", anpName, err)
	}
	return nil
}

func (c *Controller) updateANPZoneStatusCondition(newCondition metav1.Condition, anpName string) error {
	if config.OVNKubernetesFeature.EnableStatusMetrics {
		metrics.SetAdminNetworkPolicySyncSucceeded(c.nodeName, anpName, newCondition.Status == metav1.ConditionTrue)
		return nil
	}

	anp, err := c.anpLister.Get(anpName)
	if err != nil {
		return err
	}
	existingCondition := meta.FindStatusCondition(anp.Status.Conditions, newCondition.Type)
	if !doesStatusNeedAnUpdate(existingCondition, newCondition) {
		// status is already in the desired state, skip the update to reduce API server load
		return nil
	}
	if existingCondition == nil {
		newCondition.LastTransitionTime = metav1.NewTime(time.Now())
	} else {
		if existingCondition.Status != newCondition.Status {
			existingCondition.Status = newCondition.Status
			existingCondition.LastTransitionTime = metav1.NewTime(time.Now())
		}
		existingCondition.Reason = newCondition.Reason
		existingCondition.Message = newCondition.Message
		newCondition = *existingCondition
	}
	applyObj := anpapiapply.AdminNetworkPolicy(anpName).
		WithStatus(anpapiapply.AdminNetworkPolicyStatus().WithConditions(newCondition))
	_, err = c.anpClientSet.PolicyV1alpha1().AdminNetworkPolicies().
		ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: c.nodeName, Force: true})
	if err == nil {
		klog.V(5).Infof("Patched the status of ANP %s with condition type %s/%s, reason %s, message: %s",
			anpName, newCondition.Type, newCondition.Status, newCondition.Reason, newCondition.Message)
	}
	return err
}

// updateBANPStatusToReady updates the status of the policy to reflect that it is ready
// Each zone's ovnkube-controller will call this, hence let's update status using server-side-apply
func (c *Controller) updateBANPStatusToReady(banpName string) error {
	readyCondition := metav1.Condition{
		Type:    policyReadyStatusType + c.nodeName,
		Status:  metav1.ConditionTrue,
		Reason:  policyReadyReason,
		Message: "Setting up OVN DB plumbing was successful",
	}
	err := c.updateBANPZoneStatusCondition(readyCondition, banpName)
	if err != nil {
		return fmt.Errorf("unable to update the status of BANP %s, err: %v", banpName, err)
	}
	return nil
}

// updateBANPStatusToNotReady updates the status of the policy to reflect that it is not ready
// Each zone's ovnkube-controller will call this, hence let's update status using server-side-apply
// status.message must be less than 32768 characters and is usually the error that occurred which is passed
// to this function. Message is particularly useful as it can tell which zone's setup has not finished for
// this ANP instead of having to manually check logs across zones
func (c *Controller) updateBANPStatusToNotReady(banpName, message string) error {
	notReadyCondition := metav1.Condition{
		Type:    policyReadyStatusType + c.nodeName,
		Status:  metav1.ConditionFalse,
		Reason:  policyNotReadyReason,
		Message: message,
	}
	err := c.updateBANPZoneStatusCondition(notReadyCondition, banpName)
	if err != nil {
		return fmt.Errorf("unable update the status of BANP %s, err: %v", banpName, err)
	}
	return nil
}

func (c *Controller) updateBANPZoneStatusCondition(newCondition metav1.Condition, banpName string) error {
	if config.OVNKubernetesFeature.EnableStatusMetrics {
		metrics.SetBaselineAdminNetworkPolicySyncSucceeded(c.nodeName, banpName, newCondition.Status == metav1.ConditionTrue)
		return nil
	}

	banp, err := c.banpLister.Get(banpName)
	if err != nil {
		return err
	}
	existingCondition := meta.FindStatusCondition(banp.Status.Conditions, newCondition.Type)
	if !doesStatusNeedAnUpdate(existingCondition, newCondition) {
		// status is already in the desired state, skip the update to reduce API server load
		return nil
	}
	if existingCondition == nil {
		newCondition.LastTransitionTime = metav1.NewTime(time.Now())
	} else {
		if existingCondition.Status != newCondition.Status {
			existingCondition.Status = newCondition.Status
			existingCondition.LastTransitionTime = metav1.NewTime(time.Now())
		}
		existingCondition.Reason = newCondition.Reason
		existingCondition.Message = newCondition.Message
		newCondition = *existingCondition
	}
	applyObj := anpapiapply.BaselineAdminNetworkPolicy(banpName).
		WithStatus(anpapiapply.BaselineAdminNetworkPolicyStatus().WithConditions(newCondition))
	_, err = c.anpClientSet.PolicyV1alpha1().BaselineAdminNetworkPolicies().
		ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: c.nodeName, Force: true})
	if err == nil {
		klog.V(5).Infof("Patched the status of BANP %s with condition type %s/%s, reason %s, message: %s",
			banpName, newCondition.Type, newCondition.Status, newCondition.Reason, newCondition.Message)
	}
	return err
}

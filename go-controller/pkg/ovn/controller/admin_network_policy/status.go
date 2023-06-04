package admin_network_policy

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

// Defined status.type fields for Admin Network Policy
const (
	AdminNetworkPolicyReadyStatusType         = "AdminNetworkPolicyReady"
	BaselineAdminNetworkPolicyReadyStatusType = "BaselineAdminNetworkPolicyReady"
)

// Defined status.reason fields for Admin Network Policy
const (
	PolicyReadyReason    = "SetupSucceeded"
	PolicyNotReadyReason = "SetupFailed"
)

// Defined error strings for status.message
// Must be less than 32768 characters
const (
	ANPWithSamePriorityExists         = "Another ANP with same priority exists. Please use another priority."
	PolicyAlreadyExistsInCache        = "Something went wrong, entry already in cache when it shouldn't be."
	PolicyBuildACLFailed              = "Building ACLs for this policy failed. Please check logs."
	PolicyCreateUpdateACLFailed       = "Creating ACL ops for this policy failed. Please check logs."
	PolicyBuildPortGroupFailed        = "Building PortGroups for this policy failed. Please check logs."
	PolicyCreateUpdatePortGroupFailed = "Creating PortGroup ops for this policy failed. Please check logs."
	PolicyTransactFailed              = "Creating PortGroup with ports and acls for this policy failed. Please check logs."
)

func (c *Controller) updateANPStatusToReady(anp *anpapi.AdminNetworkPolicy) error {
	meta.SetStatusCondition(&anp.Status.Conditions, metav1.Condition{
		Type:    AdminNetworkPolicyReadyStatusType,
		Status:  metav1.ConditionTrue,
		Reason:  PolicyReadyReason,
		Message: "Setting up OVN DB plumbing was successful",
	})
	_, err := c.anpClientSet.PolicyV1alpha1().AdminNetworkPolicies().UpdateStatus(context.TODO(), anp, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	klog.Infof("Successfully patched the status of ANP %v with condition type %v/%v",
		anp.Name, AdminNetworkPolicyReadyStatusType, metav1.ConditionTrue)
	return nil
}

func (c *Controller) updateANPStatusToNotReady(anp *anpapi.AdminNetworkPolicy, reason string) error {
	meta.SetStatusCondition(&anp.Status.Conditions, metav1.Condition{
		Type:   AdminNetworkPolicyReadyStatusType,
		Status: metav1.ConditionFalse,
		Reason: reason,
	})
	_, err := c.anpClientSet.PolicyV1alpha1().AdminNetworkPolicies().Update(context.TODO(), anp, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	klog.Infof("Successfully patched the status of ANP %v with condition type %v/%v and reason %s",
		anp.Name, AdminNetworkPolicyReadyStatusType, metav1.ConditionFalse, reason)
	return nil
}

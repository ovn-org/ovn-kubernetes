package adminnetworkpolicy

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

// Defined status.type fields for Admin Network Policy - This is prefixed with the zone name thus
// creating one row per zone in the metav1.Condition array
// NOTE: On every update of ANP, related pods and namespaces - if anything goes wrong this
// this status type flaps between true and false. Users can use this to narrow down the malfunctioning zone
// (DANGER): If this feature is used at 500-1000 node scale, then that many status rows will be created
// and a 1000 controllers will be updating the same policy CRD  ¯\_(ツ)_/¯ -TODO: Test at least at 120 nodes.
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
	policyReadyStatusType = "Ready-In-Zone-"
	// Defined status.reason fields for (Baseline)Admin Network Policy
	policyReadyReason    = "SetupSucceeded"
	policyNotReadyReason = "SetupFailed"
)

// updateANPStatusToReady updates the status of the policy to reflect that it is ready
// Each zone's ovnkube-controller will call this, hence let's update status using retryWithConflict
func (c *Controller) updateANPStatusToReady(anp *anpapi.AdminNetworkPolicy, zone string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestANP, err := c.anpLister.Get(anp.Name)
		if err != nil {
			return fmt.Errorf("unable to fetch ANP %s, err: %v", anp.Name, err)
		}
		canp := latestANP.DeepCopy()
		meta.SetStatusCondition(&canp.Status.Conditions, metav1.Condition{
			Type:    policyReadyStatusType + zone,
			Status:  metav1.ConditionTrue,
			Reason:  policyReadyReason,
			Message: "Setting up OVN DB plumbing was successful",
		})
		_, err = c.anpClientSet.PolicyV1alpha1().AdminNetworkPolicies().UpdateStatus(context.TODO(), canp, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("unable to update the status of ANP %s, err: %v", anp.Name, err)
	}
	klog.V(5).Infof("Patched the status of ANP %v with condition type %v/%v",
		anp.Name, policyReadyStatusType+zone, metav1.ConditionTrue)
	return nil
}

// updateANPStatusToNotReady updates the status of the policy to reflect that it is not ready
// Each zone's ovnkube-controller will call this, hence let's update status using retryWithConflict
// status.message must be less than 32768 characters and is usually the error that occurred which is passed
// to this function. Message is particularly useful as it can tell which zone's setup has not finished for
// this ANP instead of having to manually check logs across zones
func (c *Controller) updateANPStatusToNotReady(anp *anpapi.AdminNetworkPolicy, zone, message string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestANP, err := c.anpLister.Get(anp.Name)
		if err != nil {
			return fmt.Errorf("unable to fetch ANP %s, err: %v", anp.Name, err)
		}
		canp := latestANP.DeepCopy()
		meta.SetStatusCondition(&canp.Status.Conditions, metav1.Condition{
			Type:    policyReadyStatusType + zone,
			Status:  metav1.ConditionFalse,
			Reason:  policyNotReadyReason,
			Message: message,
		})
		_, err = c.anpClientSet.PolicyV1alpha1().AdminNetworkPolicies().UpdateStatus(context.TODO(), canp, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("unable update the status of ANP %s, err: %v", anp.Name, err)
	}
	klog.V(3).Infof("Patched the status of ANP %v with condition type %v/%v and reason %s/%s",
		anp.Name, policyReadyStatusType+zone, metav1.ConditionFalse, policyNotReadyReason, message)
	return nil
}

// updateBANPStatusToReady updates the status of the policy to reflect that it is ready
// Each zone's ovnkube-controller will call this, hence let's update status using retryWithConflict
func (c *Controller) updateBANPStatusToReady(banp *anpapi.BaselineAdminNetworkPolicy, zone string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestBANP, err := c.banpLister.Get(banp.Name)
		if err != nil {
			return fmt.Errorf("unable to fetch BANP %s, err: %v", banp.Name, err)
		}
		cbanp := latestBANP.DeepCopy()
		meta.SetStatusCondition(&cbanp.Status.Conditions, metav1.Condition{
			Type:    policyReadyStatusType + zone,
			Status:  metav1.ConditionTrue,
			Reason:  policyReadyReason,
			Message: "Setting up OVN DB plumbing was successful",
		})
		_, err = c.anpClientSet.PolicyV1alpha1().BaselineAdminNetworkPolicies().UpdateStatus(context.TODO(), cbanp, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("unable to update the status of BANP %s, err: %v", banp.Name, err)
	}
	klog.V(5).Infof("Patched the status of BANP %v with condition type %v/%v",
		banp.Name, policyReadyStatusType+zone, metav1.ConditionTrue)
	return nil
}

// updateBANPStatusToNotReady updates the status of the policy to reflect that it is not ready
// Each zone's ovnkube-controller will call this, hence let's update status using retryWithConflict
// status.message must be less than 32768 characters and is usually the error that occurred which is passed
// to this function. Message is particularly useful as it can tell which zone's setup has not finished for
// this ANP instead of having to manually check logs across zones
func (c *Controller) updateBANPStatusToNotReady(banp *anpapi.BaselineAdminNetworkPolicy, zone, message string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestBANP, err := c.banpLister.Get(banp.Name)
		if err != nil {
			return fmt.Errorf("unable to fetch BANP %s, err: %v", banp.Name, err)
		}
		cbanp := latestBANP.DeepCopy()
		meta.SetStatusCondition(&cbanp.Status.Conditions, metav1.Condition{
			Type:    policyReadyStatusType + zone,
			Status:  metav1.ConditionFalse,
			Reason:  policyNotReadyReason,
			Message: message,
		})
		_, err = c.anpClientSet.PolicyV1alpha1().BaselineAdminNetworkPolicies().UpdateStatus(context.TODO(), cbanp, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("unable update the status of BANP %s, err: %v", banp.Name, err)
	}
	klog.V(3).Infof("Patched the status of BANP %v with condition type %v/%v and reason %s",
		banp.Name, policyReadyStatusType+zone, metav1.ConditionFalse, policyNotReadyReason)
	return nil
}

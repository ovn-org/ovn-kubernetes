package status_manager

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/pager"
	"k8s.io/klog/v2"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpapiapply "sigs.k8s.io/network-policy-api/pkg/client/applyconfiguration/apis/v1alpha1"
	anpclientset "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned"
)

// anpZoneDeleteCleanupManager is NOT like other status managers
// It only takes care of deleting statuses from zones as part of
// zone deletion
type anpZoneDeleteCleanupManager struct {
	client anpclientset.Interface
}

func newANPManager(client anpclientset.Interface) *anpZoneDeleteCleanupManager {
	return &anpZoneDeleteCleanupManager{
		client: client,
	}
}

// GetANPs returns the list of all AdminNetworkPolicy objects from kubernetes API Server
func (m *anpZoneDeleteCleanupManager) GetANPs() ([]*anpapi.AdminNetworkPolicy, error) {
	list := []*anpapi.AdminNetworkPolicy{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return m.client.PolicyV1alpha1().AdminNetworkPolicies().List(ctx, opts)
	}).EachListItem(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
	}, func(obj runtime.Object) error {
		list = append(list, obj.(*anpapi.AdminNetworkPolicy))
		return nil
	})
	return list, err
}

// GetBANPs returns the list of all BaselineAdminNetworkPolicy objects from kubernetes API Server
func (m *anpZoneDeleteCleanupManager) GetBANPs() ([]*anpapi.BaselineAdminNetworkPolicy, error) {
	list := []*anpapi.BaselineAdminNetworkPolicy{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return m.client.PolicyV1alpha1().BaselineAdminNetworkPolicies().List(ctx, opts)
	}).EachListItem(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
	}, func(obj runtime.Object) error {
		list = append(list, obj.(*anpapi.BaselineAdminNetworkPolicy))
		return nil
	})
	return list, err
}

// removeZoneStatusFromAllANPs removes the condition managed by zone
// in the conditions status of all ANPs and BANP in the cluster
// This is best effort, so errors are silently ignored by emitting
// warning messages.
func (m *anpZoneDeleteCleanupManager) removeZoneStatusFromAllANPs(existingANPs []*anpapi.AdminNetworkPolicy, existingBANPs []*anpapi.BaselineAdminNetworkPolicy, zone string) {
	klog.Infof("Deleting status for zone %s from existing admin network policies", zone)
	for _, existingANP := range existingANPs {
		applyObj := anpapiapply.AdminNetworkPolicy(existingANP.Name).
			WithStatus(anpapiapply.AdminNetworkPolicyStatus())
		_, err := m.client.PolicyV1alpha1().AdminNetworkPolicies().
			ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: zone, Force: true})
		if err != nil {
			klog.Warningf("Unable to remove zone %s's status from ANP %s: %v", zone, existingANP.Name, err)
		}
	}
	for _, existingBANP := range existingBANPs {
		applyObj := anpapiapply.BaselineAdminNetworkPolicy(existingBANP.Name).
			WithStatus(anpapiapply.BaselineAdminNetworkPolicyStatus())
		_, err := m.client.PolicyV1alpha1().BaselineAdminNetworkPolicies().
			ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: zone, Force: true})
		if err != nil {
			klog.Warningf("Unable to remove zone %s's status from BANP %s: %v", zone, existingBANP.Name, err)
		}
	}
}

// cleanupDeletedZoneStatuses loops through the provided zones and cleans the statuses of those
// zones from existing ANPs and BANPs
func (m *anpZoneDeleteCleanupManager) cleanupDeletedZoneStatuses(deletedZones sets.Set[string]) {
	// let us try to fetch all the ANPs/BANPs in one go so that we don't query API server for each zone
	existingANPs, err := m.GetANPs()
	if err != nil {
		klog.Warningf("Unable to fetch ANPs: %v", err)
	}
	existingBANPs, err := m.GetBANPs()
	if err != nil {
		klog.Warningf("Unable to fetch BANPs: %v", err)
	}
	if len(existingANPs) > 0 || len(existingBANPs) > 0 {
		for _, zone := range deletedZones.UnsortedList() {
			m.removeZoneStatusFromAllANPs(existingANPs, existingBANPs, zone)
		}
	}
}

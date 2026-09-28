// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package status_manager

import (
	"context"
	"fmt"

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
func (m *anpZoneDeleteCleanupManager) GetANPs(ctx context.Context) ([]*anpapi.AdminNetworkPolicy, error) {
	list := []*anpapi.AdminNetworkPolicy{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return m.client.PolicyV1alpha1().AdminNetworkPolicies().List(ctx, opts)
	}).EachListItem(ctx, metav1.ListOptions{
		ResourceVersion: "0",
	}, func(obj runtime.Object) error {
		list = append(list, obj.(*anpapi.AdminNetworkPolicy))
		return nil
	})
	return list, err
}

// GetBANPs returns the list of all BaselineAdminNetworkPolicy objects from kubernetes API Server
func (m *anpZoneDeleteCleanupManager) GetBANPs(ctx context.Context) ([]*anpapi.BaselineAdminNetworkPolicy, error) {
	list := []*anpapi.BaselineAdminNetworkPolicy{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return m.client.PolicyV1alpha1().BaselineAdminNetworkPolicies().List(ctx, opts)
	}).EachListItem(ctx, metav1.ListOptions{
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
func (m *anpZoneDeleteCleanupManager) removeZoneStatusFromAllANPs(ctx context.Context, existingANPs []*anpapi.AdminNetworkPolicy, existingBANPs []*anpapi.BaselineAdminNetworkPolicy, zone string) {
	klog.Infof("Deleting status for zone %s from existing admin network policies", zone)
	for _, existingANP := range existingANPs {
		applyObj := anpapiapply.AdminNetworkPolicy(existingANP.Name)
		_, err := m.client.PolicyV1alpha1().AdminNetworkPolicies().
			ApplyStatus(ctx, applyObj, metav1.ApplyOptions{FieldManager: zone, Force: true})
		if err != nil {
			klog.Warningf("Unable to remove zone %s's status from ANP %s: %v", zone, existingANP.Name, err)
		}
	}
	for _, existingBANP := range existingBANPs {
		applyObj := anpapiapply.BaselineAdminNetworkPolicy(existingBANP.Name)
		_, err := m.client.PolicyV1alpha1().BaselineAdminNetworkPolicies().
			ApplyStatus(ctx, applyObj, metav1.ApplyOptions{FieldManager: zone, Force: true})
		if err != nil {
			klog.Warningf("Unable to remove zone %s's status from BANP %s: %v", zone, existingBANP.Name, err)
		}
	}
}

// cleanupDeletedZoneStatuses loops through the provided zones and cleans the statuses of those
// zones from existing ANPs and BANPs
func (m *anpZoneDeleteCleanupManager) cleanupDeletedZoneStatuses(ctx context.Context, deletedZones sets.Set[string]) {
	// let us try to fetch all the ANPs/BANPs in one go so that we don't query API server for each zone
	existingANPs, err := m.GetANPs(ctx)
	if err != nil {
		klog.Warningf("Unable to fetch ANPs: %v", err)
	}
	existingBANPs, err := m.GetBANPs(ctx)
	if err != nil {
		klog.Warningf("Unable to fetch BANPs: %v", err)
	}
	if len(existingANPs) > 0 || len(existingBANPs) > 0 {
		for _, zone := range deletedZones.UnsortedList() {
			m.removeZoneStatusFromAllANPs(ctx, existingANPs, existingBANPs, zone)
		}
	}
}

// doStartupCleanup performs a one-time cleanup of stale ANP/BANP managedFields at startup.
// This is similar to the cleanup done in cleanupDeletedZoneStatuses when zones are deleted at runtime.
// It detects stale zones by checking for managedFields from zones that no longer exist.
func (m *anpZoneDeleteCleanupManager) doStartupCleanup(ctx context.Context, currentZones sets.Set[string]) error {
	klog.Infof("StatusManager: performing one-time startup cleanup for ANP/BANP managedFields")

	existingANPs, err := m.GetANPs(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch ANPs for startup cleanup: %w", err)
	}
	existingBANPs, err := m.GetBANPs(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch BANPs for startup cleanup: %w", err)
	}

	if len(existingANPs) == 0 && len(existingBANPs) == 0 {
		klog.V(5).Infof("StatusManager: no ANPs or BANPs found, skipping startup cleanup")
		return nil
	}

	// Find stale zones by checking managedFields on ANPs/BANPs
	staleZones := sets.New[string]()
	for _, anp := range existingANPs {
		for _, mf := range anp.ManagedFields {
			if mf.Subresource == "status" && !currentZones.Has(mf.Manager) {
				staleZones.Insert(mf.Manager)
			}
		}
	}
	for _, banp := range existingBANPs {
		for _, mf := range banp.ManagedFields {
			if mf.Subresource == "status" && !currentZones.Has(mf.Manager) {
				staleZones.Insert(mf.Manager)
			}
		}
	}

	if len(staleZones) > 0 {
		klog.Infof("StatusManager: found stale zones in ANP/BANP managedFields: %v", staleZones.UnsortedList())
		for _, zone := range staleZones.UnsortedList() {
			m.removeZoneStatusFromAllANPs(ctx, existingANPs, existingBANPs, zone)
		}
	}

	klog.Infof("StatusManager: ANP/BANP startup cleanup complete")
	return nil
}

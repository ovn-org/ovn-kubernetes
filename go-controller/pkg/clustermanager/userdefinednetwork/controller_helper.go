// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package userdefinednetwork

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utiludn "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/udn"
)

const nadFieldManager = "user-defined-network-controller"

func (c *Controller) updateNAD(ctx context.Context, obj client.Object, namespace string) (_ *netv1.NetworkAttachmentDefinition, err error) {
	start := time.Now()
	defer func() {
		if err != nil || !config.Metrics.EnableScaleMetrics {
			return
		}
		duration := time.Since(start)
		// Record workflow phase metric for NAD sync
		// Use network-scoped name to avoid collisions between same-name UDNs in different namespaces
		networkName := obj.GetName()
		switch o := obj.(type) {
		case *userdefinednetworkv1.UserDefinedNetwork:
			networkName = util.GenerateUDNNetworkName(o.Namespace, o.Name)
		case *userdefinednetworkv1.ClusterUserDefinedNetwork:
			networkName = util.GenerateCUDNNetworkName(o.Name)
		}
		metrics.RecordUDNNADSyncDuration(networkName, duration.Seconds())
	}()
	if utiludn.IsPrimaryNetwork(template.GetSpec(obj)) {
		// check if required UDN label is on namespace
		ns, err := c.namespaceInformer.Lister().Get(namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to get namespace %q: %w", namespace, err)
		}

		if _, exists := ns.Labels[types.RequiredUDNNamespaceLabel]; !exists {
			// No Required label set on namespace while trying to render NAD for primary network on this namespace
			return nil, util.NewInvalidPrimaryNetworkError(namespace)
		}
	}

	existingNAD, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(obj.GetName())
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get NetworkAttachmentDefinition %s/%s from cache: %v", namespace, obj.GetName(), err)
	}

	renderOpts, err := c.allocateEVPNIDsIfNeeded(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate EVPN IDs: %w", err)
	}

	desiredNAD, err := c.renderNadFn(obj, namespace, renderOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to generate NetworkAttachmentDefinition: %w", err)
	}

	nadCopy := existingNAD.DeepCopy()

	if nadCopy == nil {
		nadCopy, err = c.createNAD(ctx, obj, namespace, desiredNAD)
		if err != nil {
			return nil, err
		}
		klog.Infof("Created NetworkAttachmentDefinition [%s/%s]", nadCopy.Namespace, nadCopy.Name)
	}

	if !metav1.IsControlledBy(nadCopy, obj) {
		return nil, fmt.Errorf("foreign NetworkAttachmentDefinition with the desired name already exist [%s/%s]", nadCopy.Namespace, nadCopy.Name)
	}

	if nadCopy.Spec.Config != desiredNAD.Spec.Config || !reflect.DeepEqual(nadCopy.Labels, desiredNAD.Labels) {
		nadCopy.Spec.Config = desiredNAD.Spec.Config
		nadCopy.Labels = desiredNAD.Labels
		nadCopy, err = c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).
			Update(ctx, nadCopy, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to update NetworkAttachmentDefinition: %w", err)
		}
	}

	return c.applyNADAnnotations(ctx, nadCopy, desiredNAD.Annotations)
}

func (c *Controller) createNAD(ctx context.Context, obj client.Object, namespace string, desiredNAD *netv1.NetworkAttachmentDefinition) (*netv1.NetworkAttachmentDefinition, error) {
	// Serialize the primary-network check and creation with other NAD creators.
	c.createNetworkLock.Lock()
	defer c.createNetworkLock.Unlock()

	if utiludn.IsPrimaryNetwork(template.GetSpec(obj)) {
		actualNads, err := c.nadLister.NetworkAttachmentDefinitions(namespace).List(labels.Everything())
		if err != nil {
			return nil, fmt.Errorf("failed to list NetworkAttachmentDefinition: %w", err)
		}
		// The informer cache makes this a best-effort check for an existing primary NAD.
		if err := PrimaryNetAttachDefNotExist(actualNads); err != nil {
			return nil, err
		}
	}

	newNAD, err := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(ctx, desiredNAD, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create NetworkAttachmentDefinition: %w", err)
	}

	return newNAD, nil
}

func (c *Controller) applyNADAnnotations(ctx context.Context, nad *netv1.NetworkAttachmentDefinition, annotations map[string]string) (*netv1.NetworkAttachmentDefinition, error) {
	// An empty parent needs an apply only to release previously applied keys.
	if len(annotations) == 0 && !hasNADAnnotationApplyManager(nad) {
		return nad, nil
	}

	nad, err := c.migrateNADAnnotationOwnership(ctx, nad, annotations)
	if err != nil {
		return nil, err
	}

	// Apply only parent annotations. Including NAD-local keys would claim them.
	// The UID prevents updating a replacement NAD with a different owner.
	annotationIntent := map[string]any{
		"apiVersion": netv1.SchemeGroupVersion.String(),
		"kind":       "NetworkAttachmentDefinition",
		"metadata": metav1.ObjectMeta{
			Name:            nad.Name,
			Namespace:       nad.Namespace,
			UID:             nad.UID,
			ResourceVersion: nad.ResourceVersion,
			Annotations:     annotations,
		},
	}
	data, err := json.Marshal(annotationIntent)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal NetworkAttachmentDefinition: %w", err)
	}

	updatedNAD, err := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Patch(
		ctx, nad.Name, k8stypes.ApplyPatchType, data,
		metav1.PatchOptions{FieldManager: nadFieldManager, Force: ptr.To(true)})
	if err != nil {
		return nil, fmt.Errorf("failed to update NetworkAttachmentDefinition: %w", err)
	}
	klog.Infof("Updated NetworkAttachmentDefinition [%s/%s]", updatedNAD.Namespace, updatedNAD.Name)

	return updatedNAD, nil
}

func hasNADAnnotationApplyManager(nad *netv1.NetworkAttachmentDefinition) bool {
	for _, entry := range nad.ManagedFields {
		if entry.Manager == nadFieldManager && entry.Operation == metav1.ManagedFieldsOperationApply && entry.Subresource == "" {
			return true
		}
	}
	return false
}

// migrateNADAnnotationOwnership is also needed for new NADs because Create
// writes parent annotations with Update ownership. It cannot be removed until
// creation establishes annotation ownership through SSA and supported upgrades
// no longer need to migrate legacy NADs.
func (c *Controller) migrateNADAnnotationOwnership(ctx context.Context, nad *netv1.NetworkAttachmentDefinition, annotations map[string]string) (*netv1.NetworkAttachmentDefinition, error) {
	if hasNADAnnotationApplyManager(nad) {
		return nad, nil
	}

	// An unchanged first apply shares ownership with old Update writers, which
	// would prevent later removal. Release only current parent keys from Update
	// records; preserve local keys and deliberate ownership by other appliers.
	parentFields := fieldpath.NewSet()
	for key := range annotations {
		parentFields.Insert(fieldpath.MakePathOrDie("metadata", "annotations", key))
	}
	entries := append([]metav1.ManagedFieldsEntry(nil), nad.ManagedFields...)
	changed := false
	for i := range entries {
		entry := &entries[i]
		if entry.Operation != metav1.ManagedFieldsOperationUpdate || entry.Subresource != "" || entry.FieldsV1 == nil {
			continue
		}
		fields := fieldpath.NewSet()
		if err := fields.FromJSON(entry.FieldsV1.GetRawReader()); err != nil {
			return nil, fmt.Errorf("failed to read NAD annotation ownership: %w", err)
		}
		remaining := fields.Difference(parentFields)
		if fields.Equals(remaining) {
			continue
		}
		raw, err := remaining.ToJSON()
		if err != nil {
			return nil, fmt.Errorf("failed to encode NAD annotation ownership: %w", err)
		}
		entry.FieldsV1 = &metav1.FieldsV1{Raw: raw}
		changed = true
	}
	if !changed {
		return nad, nil
	}

	adoptedFields := fieldpath.NewSet()
	for key := range annotations {
		if _, exists := nad.Annotations[key]; exists {
			adoptedFields.Insert(fieldpath.MakePathOrDie("metadata", "annotations", key))
		}
	}
	raw, err := adoptedFields.ToJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to encode adopted NAD annotation ownership: %w", err)
	}
	entries = append(entries, metav1.ManagedFieldsEntry{
		Manager:    nadFieldManager,
		Operation:  metav1.ManagedFieldsOperationApply,
		APIVersion: netv1.SchemeGroupVersion.String(),
		// NAD update filtering requires a timestamp on each ownership record.
		Time:       ptr.To(metav1.Now()),
		FieldsType: "FieldsV1",
		FieldsV1:   &metav1.FieldsV1{Raw: raw},
	})

	// Transfer ownership atomically without changing values. If the subsequent
	// apply fails, its ownership record still lets a retry remove annotations
	// deleted from the parent in the meantime. Both writes use resourceVersion
	// so a concurrent writer cannot acquire ownership unnoticed between them.
	data, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": nad.ResourceVersion,
		"managedFields":   entries,
	}})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal NAD ownership migration: %w", err)
	}
	updated, err := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).
		Patch(ctx, nad.Name, k8stypes.MergePatchType, data, metav1.PatchOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to migrate NAD annotation ownership: %w", err)
	}
	return updated, nil
}

func (c *Controller) deleteNAD(ctx context.Context, obj client.Object, namespace string) error {
	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(obj.GetName())
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get NetworkAttachmentDefinition %s/%s from cache: %v", namespace, obj.GetName(), err)
		}
		// Informer caches may lag object creation/deletion. Confirm a cache miss against the API
		// before treating the NAD as absent and allowing cleanup bookkeeping to proceed.
		nad, err = c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("failed to get NetworkAttachmentDefinition %s/%s from api: %v", namespace, obj.GetName(), err)
		}
	}
	nadCopy := nad.DeepCopy()

	if nadCopy == nil ||
		!metav1.IsControlledBy(nadCopy, obj) ||
		!controllerutil.ContainsFinalizer(nadCopy, template.FinalizerUserDefinedNetwork) {
		return nil
	}

	pods, err := c.podInformer.Lister().Pods(nadCopy.Namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods at target namespace %q: %w", nadCopy.Namespace, err)
	}
	// This is best-effort check no pod using the subject NAD,
	// noting prevent a from being pod creation right after this check.
	if err := NetAttachDefNotInUse(nadCopy, pods); err != nil {
		return &networkInUseError{err: err}
	}

	controllerutil.RemoveFinalizer(nadCopy, template.FinalizerUserDefinedNetwork)
	updatedNAD, err := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nadCopy.Namespace).Update(ctx, nadCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove NetworkAttachmentDefinition finalizer: %w", err)
	}
	klog.Infof("Finalizer removed from NetworkAttachmentDefinition [%s/%s]", updatedNAD.Namespace, updatedNAD.Name)

	err = c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(updatedNAD.Namespace).Delete(ctx, updatedNAD.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	klog.Infof("Deleted NetworkAttachmetDefinition [%s/%s]", updatedNAD.Namespace, updatedNAD.Name)

	return nil
}

// allocateEVPNIDsIfNeeded checks if the object is an EVPN network and allocates VIDs and reserves VNIs if needed.
// Returns render options containing the allocated VIDs, or empty options for non-EVPN networks.
// Returns an error if EVPN transport is requested but the feature flag is disabled.
//
// This function relies on the idempotency of AllocateID: if a VID was already allocated for a key
// (either during recovery or a previous reconciliation), AllocateID returns the same VID.
// This means VIDs are stable across reconciliations without needing to parse the existing NAD.
func (c *Controller) allocateEVPNIDsIfNeeded(obj client.Object) ([]template.RenderOption, error) {
	spec := template.GetSpec(obj)
	if spec.GetTransport() != userdefinednetworkv1.TransportOptionEVPN {
		return nil, nil
	}

	// EVPN transport is requested - ensure the feature is enabled.
	if !util.IsEVPNEnabled() {
		return nil, fmt.Errorf("EVPN transport requested but EVPN feature is not enabled")
	}

	evpnCfg := spec.GetEVPN()
	if evpnCfg == nil {
		return nil, nil
	}

	networkName := obj.GetName()

	// ptr.Deref yields zero-value VRFConfig for nil VRFs, reserveVNIs skips VNI 0
	if err := c.reserveVNIs(networkName, evpnCfg.VTEP, ptr.Deref(evpnCfg.MACVRF, userdefinednetworkv1.VRFConfig{}).VNI, ptr.Deref(evpnCfg.IPVRF, userdefinednetworkv1.VRFConfig{}).VNI); err != nil {
		return nil, fmt.Errorf("failed to reserve VNIs: %w", err)
	}

	var macVRFVID, ipVRFVID int
	// Allocate VID for MAC-VRF if present
	if evpnCfg.MACVRF != nil {
		vid, err := c.vidAllocator.AllocateID(macVRFKey(networkName))
		if err != nil {
			return nil, fmt.Errorf("failed to allocate VID for MAC-VRF: %w", err)
		}
		macVRFVID = vid
		klog.V(4).InfoS("Allocated VID for MAC-VRF", "network", networkName, "vid", vid)
	}

	// Allocate VID for IP-VRF if present
	if evpnCfg.IPVRF != nil {
		vid, err := c.vidAllocator.AllocateID(ipVRFKey(networkName))
		if err != nil {
			return nil, fmt.Errorf("failed to allocate VID for IP-VRF: %w", err)
		}
		ipVRFVID = vid
		klog.V(4).InfoS("Allocated VID for IP-VRF", "network", networkName, "vid", vid)
	}

	// Return render options with allocated VIDs.
	// Note: API validation ensures at least one of macVRF or ipVRF is specified,
	// so at least one VID will be allocated if we reach here.
	return []template.RenderOption{template.WithEVPNVIDs(macVRFVID, ipVRFVID)}, nil
}

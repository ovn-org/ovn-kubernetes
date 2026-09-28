// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	k8scontrollerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/applyconfiguration/vtep/v1"
	vtepclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned"
	vtepscheme "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/scheme"
	vteplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/listers/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	finalizerVTEP = "k8s.ovn.org/vtep-protection"
)

// Controller manages VTEP resources in the cluster manager.
type Controller struct {
	vtepClient     vtepclientset.Interface
	vtepLister     vteplisters.VTEPLister
	cudnLister     udnlisters.ClusterUserDefinedNetworkLister
	nodeLister     corelisters.NodeLister
	vtepController controllerutil.Controller
	cudnController controllerutil.Controller
	nodeController controllerutil.Controller
	eventRecorder  record.EventRecorder
}

// NewController creates a new VTEP controller.
func NewController(
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
	recorder record.EventRecorder,
) *Controller {
	vtepLister := wf.VTEPInformer().Lister()
	c := &Controller{
		vtepClient:    ovnClient.VTEPClient,
		vtepLister:    vtepLister,
		cudnLister:    wf.ClusterUserDefinedNetworkInformer().Lister(),
		nodeLister:    wf.NodeCoreInformer().Lister(),
		eventRecorder: recorder,
	}

	vtepCfg := &controllerutil.ControllerConfig[vtepv1.VTEP]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.VTEPInformer().Informer(),
		Lister:         vtepLister.List,
		Reconcile:      c.reconcileVTEP,
		ObjNeedsUpdate: vtepNeedsUpdate,
		Threadiness:    1,
	}
	c.vtepController = controllerutil.NewController(
		"clustermanager-vtep-controller",
		vtepCfg,
	)

	cudnLister := wf.ClusterUserDefinedNetworkInformer().Lister()
	cudnCfg := &controllerutil.ControllerConfig[udnv1.ClusterUserDefinedNetwork]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.ClusterUserDefinedNetworkInformer().Informer(),
		Lister:         cudnLister.List,
		Reconcile:      c.reconcileCUDN,
		ObjNeedsUpdate: cudnNeedsUpdate,
		OnDelete:       c.onCUDNDeleted,
		Threadiness:    1,
	}
	c.cudnController = controllerutil.NewController(
		"clustermanager-vtep-cudn-controller",
		cudnCfg,
	)

	nodeLister := wf.NodeCoreInformer().Lister()
	nodeCfg := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         nodeLister.List,
		Reconcile:      c.reconcileNode,
		ObjNeedsUpdate: nodeNeedsUpdate,
		Threadiness:    1,
	}
	c.nodeController = controllerutil.NewController(
		"clustermanager-vtep-node-controller",
		nodeCfg,
	)

	return c
}

// Start begins the VTEP controller.
func (c *Controller) Start() error {
	defer klog.Infof("Cluster manager VTEP controller started")
	return controllerutil.Start(
		c.vtepController,
		c.cudnController,
		c.nodeController,
	)
}

// Stop shuts down the VTEP controller.
func (c *Controller) Stop() {
	controllerutil.Stop(c.vtepController, c.cudnController, c.nodeController)
}

func (c *Controller) reconcileVTEP(ctx context.Context, key string) error {
	startTime := time.Now()
	vtepName := key
	klog.V(5).Infof("Reconciling VTEP %s", vtepName)
	defer func() {
		klog.V(5).Infof("Reconciling VTEP %s took %v", vtepName, time.Since(startTime))
	}()

	vtep, err := c.vtepLister.Get(vtepName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.requeueConflictingVTEPs(vtepName)
			return nil
		}
		return fmt.Errorf("failed to get VTEP %s: %w", vtepName, err)
	}

	if !vtep.DeletionTimestamp.IsZero() {
		return c.handleVTEPDeletion(ctx, vtep)
	}

	if err := c.ensureFinalizer(ctx, vtep); err != nil {
		return err
	}

	if vtep.Spec.Mode == vtepv1.VTEPModeManaged {
		return c.handleManagedModeNotSupported(ctx, vtep)
	}

	if err := c.validateCIDRsAcrossVTEPs(vtep); err != nil {
		existingCond := meta.FindStatusCondition(vtep.Status.Conditions, conditionTypeAccepted)
		if existingCond == nil || existingCond.Status != metav1.ConditionFalse || existingCond.Reason != reasonCIDROverlap || existingCond.Message != err.Error() {
			vtepRef, refErr := reference.GetReference(vtepscheme.Scheme, vtep)
			if refErr != nil {
				return fmt.Errorf("failed to get object reference for VTEP %s: %w", vtep.Name, refErr)
			}
			c.eventRecorder.Event(vtepRef, corev1.EventTypeWarning, reasonCIDROverlap, err.Error())
		}
		return c.updateStatusCondition(ctx, vtep, conditionTypeAccepted, metav1.ConditionFalse,
			reasonCIDROverlap, err.Error())
	}

	if err := c.validateNoIPv6VTEPsForEVPN(vtep); err != nil {
		existingCond := meta.FindStatusCondition(vtep.Status.Conditions, conditionTypeAccepted)
		if existingCond == nil || existingCond.Status != metav1.ConditionFalse || existingCond.Reason != reasonEVPNIPv6NotSupported || existingCond.Message != err.Error() {
			vtepRef, refErr := reference.GetReference(vtepscheme.Scheme, vtep)
			if refErr != nil {
				return fmt.Errorf("failed to get object reference for VTEP %s: %w", vtep.Name, refErr)
			}
			c.eventRecorder.Event(vtepRef, corev1.EventTypeWarning, reasonEVPNIPv6NotSupported, err.Error())
		}
		return c.updateStatusCondition(ctx, vtep, conditionTypeAccepted, metav1.ConditionFalse,
			reasonEVPNIPv6NotSupported, err.Error())
	}

	if err := c.validateNodeVTEPIPs(vtep); err != nil {
		existingCond := meta.FindStatusCondition(vtep.Status.Conditions, conditionTypeAccepted)
		if existingCond == nil || existingCond.Status != metav1.ConditionFalse || existingCond.Reason != reasonAllocationFailed || existingCond.Message != err.Error() {
			vtepRef, refErr := reference.GetReference(vtepscheme.Scheme, vtep)
			if refErr != nil {
				return fmt.Errorf("failed to get object reference for VTEP %s: %w", vtep.Name, refErr)
			}
			c.eventRecorder.Event(vtepRef, corev1.EventTypeWarning, reasonAllocationFailed, err.Error())
		}
		// Don't retry: the node controller watches for k8s.ovn.org/vteps
		// annotation changes and re-queues all VTEPs when annotations appear.
		return c.updateStatusCondition(ctx, vtep, conditionTypeAccepted, metav1.ConditionFalse,
			reasonAllocationFailed, err.Error())
	}

	return c.updateStatusCondition(ctx, vtep, conditionTypeAccepted, metav1.ConditionTrue,
		reasonAllocated, "VTEP allocation succeeded")
}

// ensureFinalizer adds the VTEP protection finalizer unconditionally on every
// non-deleted VTEP. We always add it rather than waiting for a CUDN (or future
// consumer) to reference the VTEP, to avoid a race where the VTEP is deleted
// between the time a consumer starts referencing it and the controller notices.
// The actual deletion policy (checking CUDN references) is enforced in
// handleVTEPDeletion when the finalizer gates the delete.
func (c *Controller) ensureFinalizer(ctx context.Context, vtep *vtepv1.VTEP) error {
	if !k8scontrollerutil.ContainsFinalizer(vtep, finalizerVTEP) {
		_, err := c.vtepClient.K8sV1().VTEPs().Apply(
			ctx,
			vtepapply.VTEP(vtep.Name).WithFinalizers(finalizerVTEP),
			metav1.ApplyOptions{FieldManager: fieldManager, Force: true},
		)
		if err != nil {
			return fmt.Errorf("failed to add finalizer to VTEP %s: %w", vtep.Name, err)
		}
		klog.Infof("Added finalizer to VTEP %s", vtep.Name)
	}
	return nil
}

func (c *Controller) handleVTEPDeletion(ctx context.Context, vtep *vtepv1.VTEP) error {
	if !k8scontrollerutil.ContainsFinalizer(vtep, finalizerVTEP) {
		return nil
	}

	referencingCUDNs, err := c.getCUDNsReferencingVTEP(vtep.Name)
	if err != nil {
		return fmt.Errorf("failed to check CUDN references for VTEP %s: %w", vtep.Name, err)
	}

	if len(referencingCUDNs) > 0 {
		// we don't retry here since when a CUDN is deleted, this controller will be notified and will re-queue the VTEP
		klog.Infof("VTEP %s is still referenced by CUDNs [%s], blocking deletion", vtep.Name, strings.Join(referencingCUDNs, ", "))
		return nil
	}

	_, err = c.vtepClient.K8sV1().VTEPs().Apply(
		ctx,
		vtepapply.VTEP(vtep.Name),
		metav1.ApplyOptions{FieldManager: fieldManager, Force: true},
	)
	if err != nil {
		return fmt.Errorf("failed to remove finalizer from VTEP %s: %w", vtep.Name, err)
	}
	klog.Infof("Removed finalizer from VTEP %s, deletion unblocked", vtep.Name)
	metrics.DeleteVTEPCondition(vtep.Name)
	return nil
}

// getCUDNsReferencingVTEP returns the names of CUDNs that reference the given VTEP.
func (c *Controller) getCUDNsReferencingVTEP(vtepName string) ([]string, error) {
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list CUDNs: %w", err)
	}
	var referencing []string
	for _, cudn := range cudns {
		if cudnReferencesVTEP(cudn, vtepName) {
			referencing = append(referencing, cudn.Name)
		}
	}
	return referencing, nil
}

func cudnReferencesVTEP(cudn *udnv1.ClusterUserDefinedNetwork, vtepName string) bool {
	if cudn.Spec.Network.EVPN != nil && cudn.Spec.Network.EVPN.VTEP == vtepName {
		return true
	}
	return false
}

// validateNoIPv6ForEVPN rejects VTEPs that contain IPv6 CIDRs when they are
// referenced by an EVPN CUDN. FRR does not support IPv6 VTEPs for EVPN
// transport. VTEPs not referenced by any EVPN CUDN are unaffected.
func (c *Controller) validateNoIPv6VTEPsForEVPN(vtep *vtepv1.VTEP) error {
	referencingCUDNs, err := c.getCUDNsReferencingVTEP(vtep.Name)
	if err != nil {
		return fmt.Errorf("failed to check CUDN references for VTEP %s: %w", vtep.Name, err)
	}
	if len(referencingCUDNs) == 0 {
		return nil
	}

	var errs []error
	for _, cidr := range vtep.Spec.CIDRs {
		_, ipNet, err := net.ParseCIDR(string(cidr))
		if err != nil {
			// shouldn't happen since CIDRs are validated by CEL
			errs = append(errs, fmt.Errorf("invalid CIDR %q in VTEP %s: %w", cidr, vtep.Name, err))
			continue
		}
		if ipNet.IP.To4() == nil {
			errs = append(errs, fmt.Errorf("VTEP %s contains IPv6 CIDR %s but is referenced by EVPN CUDNs [%s]; "+
				"IPv6 VTEPs are not supported for EVPN transport", vtep.Name, cidr, strings.Join(referencingCUDNs, ", ")))
		}
	}
	return errors.Join(errs...)
}

// validateCIDRsAcrossVTEPs checks that none of this VTEP's CIDRs overlap with
// any other VTEP's CIDRs. All overlapping VTEPs are re-queued so both sides
// of a conflict discover it and set Accepted=False.
//
// NOTE: this parses every VTEP's CIDRs on each reconciliation. If the number
// of VTEPs grows large we may need to cache parsed CIDRs to avoid repeated
// parsing overhead.
func (c *Controller) validateCIDRsAcrossVTEPs(vtep *vtepv1.VTEP) error {
	currentCIDRs, err := parseVTEPCIDRs(vtep)
	if err != nil {
		return fmt.Errorf("failed to parse CIDRs for VTEP %s: %w", vtep.Name, err)
	}

	allVTEPs, err := c.vtepLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list VTEPs: %w", err)
	}

	var conflicting []string
	var errs []error
	for _, other := range allVTEPs {
		if other.Name == vtep.Name {
			continue
		}
		// A VTEP with DeletionTimestamp and no finalizers is being garbage
		// collected by the API server. The informer cache may briefly lag
		// behind, so skip it to avoid false overlap with a dying VTEP.
		if !other.DeletionTimestamp.IsZero() && len(other.Finalizers) == 0 {
			continue
		}
		otherCIDRs, err := parseVTEPCIDRs(other)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to parse CIDRs for VTEP %s: %w", other.Name, err))
			continue
		}
		otherCond := meta.FindStatusCondition(other.Status.Conditions, conditionTypeAccepted)
		if util.NetworksOverlap(currentCIDRs, otherCIDRs) {
			conflicting = append(conflicting, other.Name)
			if otherCond == nil || otherCond.Status != metav1.ConditionFalse || otherCond.Reason != reasonCIDROverlap ||
				!vtepNameInMessage(otherCond.Message, vtep.Name) {
				c.vtepController.Reconcile(other.Name)
			}
		} else if otherCond != nil && otherCond.Status == metav1.ConditionFalse && otherCond.Reason == reasonCIDROverlap &&
			vtepNameInMessage(otherCond.Message, vtep.Name) {
			c.vtepController.Reconcile(other.Name)
		}
	}
	if len(conflicting) > 0 {
		sort.Strings(conflicting)
		errs = append(errs, fmt.Errorf("CIDRs overlap with VTEPs: [%s]",
			strings.Join(conflicting, ", ")))
	}
	return errors.Join(errs...)
}

// requeueConflictingVTEPs re-queues VTEPs whose Accepted=False/CIDROverlap
// condition message mentions the deleted VTEP by name, so they can
// re-evaluate whether their conflicts are resolved.
func (c *Controller) requeueConflictingVTEPs(deletedVTEPName string) {
	allVTEPs, err := c.vtepLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list VTEPs during conflict requeue: %v", err)
		return
	}
	for _, other := range allVTEPs {
		cond := meta.FindStatusCondition(other.Status.Conditions, conditionTypeAccepted)
		if cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == reasonCIDROverlap &&
			vtepNameInMessage(cond.Message, deletedVTEPName) {
			klog.V(4).Infof("Re-queuing VTEP %s after deletion of conflicting VTEP %s", other.Name, deletedVTEPName)
			c.vtepController.Reconcile(other.Name)
		}
	}
}

// vtepNameInMessage checks whether name appears as an exact entry in the
// bracketed, comma-separated list embedded in the condition message.
// Message format: "CIDRs overlap with VTEPs: [vtep-a, vtep-b]"
func vtepNameInMessage(message, name string) bool {
	start := strings.Index(message, "[")
	end := strings.LastIndex(message, "]")
	if start == -1 || end == -1 || end <= start {
		return false
	}
	for _, entry := range strings.Split(message[start+1:end], ", ") {
		if entry == name {
			return true
		}
	}
	return false
}

func parseVTEPCIDRs(vtep *vtepv1.VTEP) ([]*net.IPNet, error) {
	cidrs := make([]*net.IPNet, 0, len(vtep.Spec.CIDRs))
	for _, c := range vtep.Spec.CIDRs {
		_, ipNet, err := net.ParseCIDR(string(c))
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", c, err)
		}
		cidrs = append(cidrs, ipNet)
	}
	return cidrs, nil
}

// validateNodeVTEPIPs validates that every node has a VTEP IP entry in the
// k8s.ovn.org/vteps annotation for this VTEP. For unmanaged mode, ovnkube-node
// discovers IPs and writes the annotation; this controller only reads and
// validates. The RA controller reads the same annotation when generating FRR
// configurations.
func (c *Controller) validateNodeVTEPIPs(vtep *vtepv1.VTEP) error {
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	var errs []error
	for _, node := range nodes {
		vteps, err := util.ParseNodeVTEPs(node)
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				errs = append(errs, fmt.Errorf("node %s is missing the %s annotation", node.Name, util.OVNNodeVTEPs))
				continue
			}
			errs = append(errs, fmt.Errorf("failed to parse %s annotation on node %s: %w", util.OVNNodeVTEPs, node.Name, err))
			continue
		}
		entry, ok := vteps[vtep.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("node %s has no entry for VTEP %s in %s annotation", node.Name, vtep.Name, util.OVNNodeVTEPs))
			continue
		}
		if len(entry.IPs) == 0 {
			errs = append(errs, fmt.Errorf("node %s has an empty IP list for VTEP %s", node.Name, vtep.Name))
			continue
		}
	}

	return errors.Join(errs...)
}

func (c *Controller) handleManagedModeNotSupported(ctx context.Context, vtep *vtepv1.VTEP) error {
	existingCond := meta.FindStatusCondition(vtep.Status.Conditions, conditionTypeAccepted)
	if existingCond == nil || existingCond.Status != metav1.ConditionFalse || existingCond.Reason != reasonManagedModeNotSupported {
		vtepRef, err := reference.GetReference(vtepscheme.Scheme, vtep)
		if err != nil {
			return fmt.Errorf("failed to get object reference for VTEP %s: %w", vtep.Name, err)
		}
		c.eventRecorder.Event(vtepRef, corev1.EventTypeWarning, reasonManagedModeNotSupported,
			"Managed VTEP mode is not yet implemented; only Unmanaged mode is currently supported")
	}

	return c.updateStatusCondition(ctx, vtep, conditionTypeAccepted, metav1.ConditionFalse,
		reasonManagedModeNotSupported,
		"Managed VTEP mode is not yet implemented; only Unmanaged mode is currently supported")
}

// reconcileCUDN handles CUDN create events relevant to VTEP management. It
// re-queues the referenced VTEP so validations like IPv6 rejection can fire.
// Deletions are not handled here (the object is already gone from the lister
// by the time the key is reconciled, so we couldn't tell which VTEP to
// re-queue); onCUDNDeleted handles them from the informer's delete handler
// where the deleted object is still available.
func (c *Controller) reconcileCUDN(_ context.Context, key string) error {
	cudn, err := c.cudnLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Handled by onCUDNDeleted, which has the deleted object.
			return nil
		}
		return fmt.Errorf("failed to get CUDN %s: %w", key, err)
	}

	if cudn.Spec.Network.EVPN != nil && cudn.Spec.Network.EVPN.VTEP != "" {
		klog.V(5).Infof("EVPN CUDN %s created, re-queuing VTEP %s", key, cudn.Spec.Network.EVPN.VTEP)
		c.vtepController.Reconcile(cudn.Spec.Network.EVPN.VTEP)
	}
	return nil
}

// onCUDNDeleted runs synchronously from the CUDN informer's delete handler with
// the deleted object in hand, so it can read EVPN.VTEP directly and re-queue
// exactly that VTEP.
//
// A CUDN deletion changes the set of CUDNs referencing the VTEP, which two VTEP
// behaviours depend on (both evaluated via getCUDNsReferencingVTEP in
// reconcileVTEP):
//   - finalizer removal: if the VTEP is being deleted but was blocked because
//     this was the last CUDN referencing it, deletion can now be unblocked
//     (handleVTEPDeletion).
//   - IPv6-for-EVPN recovery: if the VTEP has an IPv6 CIDR and was set
//     Accepted=False/EVPNIPv6NotSupported because this was the last EVPN CUDN
//     referencing it, it can now recover to Accepted=True
//     (validateNoIPv6VTEPsForEVPN).
//
// Only EVPN CUDNs with a VTEP set can ever reference a VTEP (see
// cudnReferencesVTEP), so non-EVPN deletions cannot affect either behaviour and
// are ignored here.
//
// Handling deletes here (rather than in reconcileCUDN) avoids relying on
// remembered state: the decision is made from the deleted object itself, so it
// cannot be lost if a CUDN's create and delete coalesce in the workqueue.
func (c *Controller) onCUDNDeleted(cudn *udnv1.ClusterUserDefinedNetwork) {
	if cudn.Spec.Network.EVPN == nil || cudn.Spec.Network.EVPN.VTEP == "" {
		return
	}
	klog.V(5).Infof("EVPN CUDN %s deleted, re-queuing VTEP %s", cudn.Name, cudn.Spec.Network.EVPN.VTEP)
	c.vtepController.Reconcile(cudn.Spec.Network.EVPN.VTEP)
}

// cudnNeedsUpdate determines if a CUDN event is relevant for VTEP management.
// We allow creates of EVPN-enabled CUDNs through so reconcileCUDN can re-queue
// the referenced VTEP. The spec is immutable so updates never matter.
// Deletions bypass ObjNeedsUpdate entirely and are handled by onCUDNDeleted.
func cudnNeedsUpdate(oldObj, newObj *udnv1.ClusterUserDefinedNetwork) bool {
	if oldObj == nil && newObj != nil {
		return newObj.Spec.Network.EVPN != nil
	}
	return false
}

// reconcileNode is called when a node's k8s.ovn.org/vteps annotation changes
// (or on node create/delete). It re-queues all VTEPs so validateNodeVTEPIPs
// can re-validate VTEP IPs for the affected node.
//
// NOTE: currently we re-queue all VTEPs because every node participates in
// every VTEP (FRR runs on all nodes). If partial VTEP participation is
// supported in the future, we could compare the node's IPs against each
// VTEP's CIDRs and only re-queue overlapping VTEPs.
func (c *Controller) reconcileNode(_ context.Context, _ string) error {
	c.vtepController.ReconcileAll()
	return nil
}

// nodeNeedsUpdate triggers VTEP reconciliation only when the k8s.ovn.org/vteps
// annotation changes. Creates (oldObj==nil) are ignored because a fresh node
// won't have the VTEP annotation yet; the annotation-change event will handle
// it once set. On restart, the informer fires synthetic creates for all
// existing nodes (which may already carry the annotation), but the VTEP
// informer also fires creates for all VTEPs, and each VTEP reconciliation
// reads node annotations via the lister, so skipping node creates is safe.
// Deletes (newObj==nil) bypass ObjNeedsUpdate in the controller framework,
// so that branch is unreachable here.
func nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	if oldObj == nil || newObj == nil {
		return false
	}
	return util.NodeVTEPsAnnotationChanged(oldObj, newObj)
}

func vtepNeedsUpdate(oldObj, newObj *vtepv1.VTEP) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	if !reflect.DeepEqual(oldObj.Spec, newObj.Spec) {
		return true
	}
	// Delete comes as an update event with a non-zero DeletionTimestamp.
	// Always reconcile deleting VTEPs so finalizer removal can be retried.
	if !newObj.DeletionTimestamp.IsZero() {
		return true
	}
	return false
}

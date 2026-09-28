// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkmanager

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressiplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// EgressIPTrackerController tracks which NADs must be present on which nodes
// due to EgressIP assignments.
type EgressIPTrackerController struct {
	cacheMutex sync.Mutex // guards cache
	name       string
	// node -> nad that has EIP
	cache map[string]map[string]struct{}

	onNetworkRefChange func(nodeName, nadName string, present bool)

	primaryNADForNamespace func(namespace string) (string, error)

	nsLister      v1.NamespaceLister
	eipLister     egressiplisters.EgressIPLister
	nadLister     nadlisters.NetworkAttachmentDefinitionLister
	eipController controller.Controller
	nsController  controller.Controller
	nadReconciler controller.Reconciler
}

func NewEgressIPTrackerController(
	name string, wf watchFactory,
	onNetworkRefChange func(nodeName, nadName string, present bool),
	primaryNADForNamespace func(namespace string) (string, error),
) *EgressIPTrackerController {
	t := &EgressIPTrackerController{
		name:                   name,
		cache:                  make(map[string]map[string]struct{}),
		onNetworkRefChange:     onNetworkRefChange,
		nsLister:               wf.NamespaceInformer().Lister(),
		eipLister:              wf.EgressIPInformer().Lister(),
		nadLister:              wf.NADInformer().Lister(),
		primaryNADForNamespace: primaryNADForNamespace,
	}

	if t.primaryNADForNamespace == nil {
		t.primaryNADForNamespace = t.getPrimaryNADForNamespaceFromLister
	}

	cfg := &controller.ControllerConfig[egressipv1.EgressIP]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      t.reconcileEgressIP,
		ObjNeedsUpdate: t.egressIPNeedsUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       wf.EgressIPInformer().Informer(),
		Lister:         wf.EgressIPInformer().Lister().List,
	}
	t.eipController = controller.NewController[egressipv1.EgressIP]("egressip-tracker", cfg)

	ncfg := &controller.ControllerConfig[corev1.Namespace]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      t.reconcileNamespace,
		ObjNeedsUpdate: t.namespaceNeedsUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       wf.NamespaceInformer().Informer(),
		Lister:         wf.NamespaceInformer().Lister().List,
	}
	t.nsController = controller.NewController[corev1.Namespace]("egressip-namespace-tracker", ncfg)

	t.nadReconciler = controller.NewReconciler(
		fmt.Sprintf("%s-nad-reconciler", name),
		&controller.ReconcilerConfig{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:   t.reconcileNAD,
			Threadiness: 1,
			MaxAttempts: controller.InfiniteAttempts,
		},
	)

	return t
}

func (t *EgressIPTrackerController) Start() error {
	return controller.StartWithInitialSync(t.syncAll, t.eipController, t.nsController, t.nadReconciler)
}

func (t *EgressIPTrackerController) Stop() {
	controller.Stop(t.eipController, t.nsController, t.nadReconciler)
}

func (t *EgressIPTrackerController) NodeHasNAD(node, nad string) bool {
	t.cacheMutex.Lock()
	defer t.cacheMutex.Unlock()
	if _, ok := t.cache[node]; !ok {
		return false
	}
	if _, ok := t.cache[node][nad]; !ok {
		return false
	}
	return true
}

func (t *EgressIPTrackerController) NADReconciler() controller.Reconciler {
	return t.nadReconciler
}

func (t *EgressIPTrackerController) egressIPNeedsUpdate(oldObj, newObj *egressipv1.EgressIP) bool {
	if newObj == nil {
		return false
	}
	if oldObj == nil {
		return true // this is an Add
	}

	if !reflect.DeepEqual(oldObj.Spec.NamespaceSelector, newObj.Spec.NamespaceSelector) {
		return true
	}

	if !reflect.DeepEqual(oldObj.Status.Items, newObj.Status.Items) {
		return true
	}

	return false
}

func (t *EgressIPTrackerController) namespaceNeedsUpdate(oldObj, newObj *corev1.Namespace) bool {
	if newObj == nil {
		return false
	}
	if oldObj == nil {
		return true // this is an Add
	}

	// Only trigger reconcile if the labels (used by EgressIP selectors) change
	return !reflect.DeepEqual(oldObj.Labels, newObj.Labels)
}

// reconcileNAD determines if a NAD needs to reconcile, then triggers reconciliation
// via the namespace controller
func (t *EgressIPTrackerController) reconcileNAD(_ context.Context, key string) error {
	klog.V(5).Infof("%s - reconciling NAD key: %q", t.name, key)
	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid NAD key %q: %v", key, err)
	}

	// All NAD changes are funneled through namespace reconciliation to keep the cache consistent.
	t.nsController.Reconcile(namespace)
	return nil
}

// reconcileEgressIP determines if an egress IP needs to reconcile, then triggers reconciliation
// via the namespace controller
func (t *EgressIPTrackerController) reconcileEgressIP(_ context.Context, key string) error {
	klog.V(5).Infof("%s - reconciling egress IP key: %q", t.name, key)

	eip, err := t.eipLister.Get(key)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get EgressIP %q from cache: %v", key, err)
	}
	if apierrors.IsNotFound(err) {
		// EgressIP deleted → reconcile every namespace that had an active NAD cached
		namespacesToReconcile := make(map[string]struct{})
		t.cacheMutex.Lock()
		for _, nads := range t.cache {
			for nad := range nads {
				nsName, _, err := cache.SplitMetaNamespaceKey(nad)
				if err != nil {
					klog.Errorf("%s - Invalid NAD key in cache %q: %v", t.name, nad, err)
					continue
				}
				namespacesToReconcile[nsName] = struct{}{}
			}
		}
		t.cacheMutex.Unlock()

		for nsName := range namespacesToReconcile {
			t.nsController.Reconcile(nsName)
		}
		return nil
	}

	nsSelector, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
	if err != nil {
		return fmt.Errorf("invalid namespaceSelector in EIP %s: %w", key, err)
	}
	nsList, err := t.nsLister.List(nsSelector)
	if err != nil {
		return fmt.Errorf("failed to list namespaces for EIP %s: %w", key, err)
	}

	for _, ns := range nsList {
		t.nsController.Reconcile(ns.Name)
	}

	return nil
}

func (t *EgressIPTrackerController) reconcileNamespace(_ context.Context, key string) error {
	var refChanges []refChange
	klog.V(5).Infof("%s - reconciling namespace key: %q", t.name, key)
	ns, err := t.nsLister.Get(key)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) {
		// Namespace deleted → drop any cache
		t.cacheMutex.Lock()
		for node, nads := range t.cache {
			for nad := range nads {
				nadNamespace, _, err := cache.SplitMetaNamespaceKey(nad)
				if err != nil {
					klog.Errorf("%s - Invalid NAD key in cache %q: %v", t.name, nad, err)
					delete(nads, nad)
				} else if nadNamespace == key {
					delete(nads, nad)
					refChanges = append(refChanges, refChange{node, nad, false})
				}
			}
			if len(nads) == 0 {
				delete(t.cache, node)
			}
		}
		t.cacheMutex.Unlock()
		if t.onNetworkRefChange != nil {
			for _, callback := range refChanges {
				t.onNetworkRefChange(callback.node, callback.nad, callback.active)
			}
		}
		return nil
	}

	primaryNAD, err := t.primaryNADForNamespace(ns.Name)
	if err != nil {
		if util.IsInvalidPrimaryNetworkError(err) {
			// Namespace requires a primary network but none exists yet; NAD controller will requeue.
			return nil
		}
		return fmt.Errorf("failed to get primary NAD for namespace %q: %w", ns.Name, err)
	}

	if primaryNAD == types.DefaultNetworkName {
		primaryNAD = ""
	}

	// Gather the new set of (node,nad) pairs implied by this namespace's EIPs. Each namespace can
	// have at most one primary NAD; if present we pin that NAD to every node that currently serves
	// the namespace via an EgressIP assignment.
	newActive := map[string]string{} // node -> nad
	if primaryNAD != "" {
		eips, err := t.eipLister.List(labels.Everything())
		if err != nil {
			return fmt.Errorf("failed to list EgressIPs: %w", err)
		}
		for _, eip := range eips {
			sel, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
			if err != nil {
				return fmt.Errorf("invalid namespaceSelector in EIP %s: %w", eip.Name, err)
			}
			if sel.Matches(labels.Set(ns.Labels)) {
				for _, st := range eip.Status.Items {
					newActive[st.Node] = primaryNAD
				}
			}
		}
	}

	// Diff against cache
	t.cacheMutex.Lock()

	// Removals first
	for node, nads := range t.cache {
		for nad := range nads {
			nsName, _, err := cache.SplitMetaNamespaceKey(nad)
			if err != nil {
				klog.Errorf("%s - Invalid NAD key in cache %q: %v", t.name, nad, err)
				delete(nads, nad)
			}
			if nsName == ns.Name {
				if newActive[node] != nad {
					delete(nads, nad)
					refChanges = append(refChanges, refChange{node, nad, false})
				}
			}
		}
		if len(nads) == 0 {
			delete(t.cache, node)
		}
	}

	// Additions second
	for node, nad := range newActive {
		if _, ok := t.cache[node]; !ok {
			t.cache[node] = map[string]struct{}{}
		}
		if _, exists := t.cache[node][nad]; !exists {
			t.cache[node][nad] = struct{}{}
			refChanges = append(refChanges, refChange{node, nad, true})
		}
	}
	t.cacheMutex.Unlock()
	if t.onNetworkRefChange != nil {
		for _, callback := range refChanges {
			t.onNetworkRefChange(callback.node, callback.nad, callback.active)
		}
	}
	return nil
}

// syncAll builds the cache on initial controller start
// This is required because workers are started asynchronously and consumers of the tracker
// rely on the cache to be populated during start up
func (t *EgressIPTrackerController) syncAll() error {
	ctx := context.Background()
	start := time.Now()
	defer func() {
		klog.V(5).Infof("%s - syncAll took %v", t.name, time.Since(start))
	}()

	// handling all namespaces will handle setting up the egress IP tracker
	namespaces, err := t.nsLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("syncAll: list Namespaces: %v", err)
	}

	for _, ns := range namespaces {
		nsName := ns.Name
		if err := t.reconcileNamespace(ctx, nsName); err != nil {
			klog.Errorf("%s - Failed to sync namespace %q: %v", t.name, nsName, err)
			continue
		}
	}

	return nil
}

// getPrimaryNADForNamespaceFromLister is a fallback resolver used in tests when no resolver is injected.
func (t *EgressIPTrackerController) getPrimaryNADForNamespaceFromLister(namespace string) (string, error) {
	ns, err := t.nsLister.Get(namespace)
	if err != nil {
		return "", fmt.Errorf("failed to get namespace %q: %w", namespace, err)
	}
	if _, exists := ns.Labels[types.RequiredUDNNamespaceLabel]; !exists {
		return types.DefaultNetworkName, nil
	}

	nads, err := t.nadLister.NetworkAttachmentDefinitions(namespace).List(labels.Everything())
	if err != nil {
		return "", fmt.Errorf("failed to list network attachment definitions: %w", err)
	}
	for _, nad := range nads {
		if nad.Name == types.DefaultNetworkName {
			continue
		}
		nadInfo, err := util.ParseNADInfo(nad)
		if err != nil {
			klog.Warningf("%s - Failed to parse network attachment definition %q: %v", t.name, nad.Name, err)
			continue
		}
		if nadInfo.IsPrimaryNetwork() {
			return util.GetNADName(nad.Namespace, nad.Name), nil
		}
	}

	// The namespace declared it needs a primary UDN but none exists yet.
	return "", util.NewInvalidPrimaryNetworkError(namespace)
}

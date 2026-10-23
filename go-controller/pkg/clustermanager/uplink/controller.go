// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	k8scontrollerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/applyconfiguration/uplink/v1alpha1"
	uplinkclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/applyconfiguration/userdefinednetwork/v1"
	udnclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	udnlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	fieldManager    = "clustermanager-uplink-controller"
	finalizerUplink = "k8s.ovn.org/uplink-protection"

	reasonInvalidSpec              = "InvalidSpec"
	reasonNodeSelectorOverlap      = "NodeSelectorOverlap"
	reasonNoMatchingNodes          = "NoMatchingNodes"
	reasonMissingUplinkState       = "MissingUplinkState"
	reasonUplinkStateNotResolved   = "UplinkStateNotResolved"
	reasonUplinkStateIdentityError = "UplinkStateIdentityError"
	reasonReady                    = "Ready"

	obsoleteUplinkConditionDegraded   = "Degraded"
	obsoleteUplinkConditionReferenced = "Referenced"

	conditionTypeUplinksReady            = "UplinksReady"
	reasonUplinksReady                   = "UplinksReady"
	reasonUplinksNotReady                = "UplinksNotReady"
	reasonUplinkUnsupportedGatewayMode   = "UplinkUnsupportedGatewayMode"
	reasonUplinkUnsupportedTransport     = "UplinkUnsupportedTransport"
	reasonUplinkNotFound                 = "UplinkNotFound"
	reasonUplinkOverlapOnNode            = "UplinkOverlapOnNode"
	reasonUplinkNotFoundForNode          = "UplinkNotFoundForNode"
	reasonUplinkNotResolvedForNode       = "UplinkNotResolvedForNode"
	reasonUplinkTerminating              = "UplinkTerminating"
	reasonNoActiveCUDNs                  = "NoActiveCUDNs"
	reasonUplinkVRFAttachmentFailed      = "UplinkVRFAttachmentFailed"
	reasonUplinkBridgeMappingFailed      = "UplinkBridgeMappingFailed"
	reasonUplinkGatewayProgrammingFailed = "UplinkGatewayProgrammingFailed"
	reasonUplinkConfigurationConflict    = "UplinkConfigurationConflict"

	networkRefSeparator = "\x00"
)

// Controller reconciles Uplink API objects in cluster-manager. It resolves
// per-node Uplink nodeConfigs, deletes stale UplinkStates, protects referenced
// Uplinks with a finalizer, aggregates UplinkState readiness into Uplink
// status, and updates CUDN UplinksReady status from the states for the nodes
// selected by that CUDN.
type Controller struct {
	uplinkClient      uplinkclientset.Interface
	udnClient         udnclientset.Interface
	uplinkLister      uplinklisters.UplinkLister
	uplinkStateLister uplinklisters.UplinkStateLister
	cudnLister        udnlisters.ClusterUserDefinedNetworkLister
	nodeLister        corelisters.NodeLister
	networkManager    networkmanager.Interface

	uplinkController      controllerutil.Controller
	uplinkStateController controllerutil.Controller
	cudnController        controllerutil.Controller
	nodeController        controllerutil.Controller
	networkRefController  controllerutil.Reconciler
	networkRefReconciler  networkmanager.NetworkRefReconciler
	networkRefID          uint64

	cudnsByUplinkMu sync.RWMutex
	cudnsByUplink   map[string]sets.Set[string]

	uplinkStateIdentityMu sync.RWMutex
	uplinkByStateName     map[string]string
}

type cudnUplinkFailure struct {
	uplink string
	reason string
	node   string
}

type uplinkNodeFailure struct {
	node   string
	reason string
	detail string
}

// NewController creates a new cluster-manager Uplink controller.
func NewController(
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
	networkManager networkmanager.Interface,
) *Controller {
	c := &Controller{
		uplinkClient:      ovnClient.UplinkClient,
		udnClient:         ovnClient.UserDefinedNetworkClient,
		uplinkLister:      wf.UplinkInformer().Lister(),
		uplinkStateLister: wf.UplinkStateInformer().Lister(),
		cudnLister:        wf.ClusterUserDefinedNetworkInformer().Lister(),
		nodeLister:        wf.NodeCoreInformer().Lister(),
		networkManager:    networkManager,
		cudnsByUplink:     make(map[string]sets.Set[string]),
		uplinkByStateName: make(map[string]string),
	}

	uplinkCfg := &controllerutil.ControllerConfig[uplinkv1alpha1.Uplink]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		MaxAttempts:    controllerutil.InfiniteAttempts,
		Informer:       wf.UplinkInformer().Informer(),
		Lister:         c.uplinkLister.List,
		Reconcile:      c.reconcileUplink,
		ObjNeedsUpdate: uplinkNeedsUpdate,
		Threadiness:    1,
	}
	c.uplinkController = controllerutil.NewController(
		"clustermanager-uplink-controller",
		uplinkCfg,
	)

	uplinkStateCfg := &controllerutil.ControllerConfig[uplinkv1alpha1.UplinkState]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		MaxAttempts:    controllerutil.InfiniteAttempts,
		Informer:       wf.UplinkStateInformer().Informer(),
		Lister:         c.uplinkStateLister.List,
		Reconcile:      c.reconcileUplinkState,
		ObjNeedsUpdate: uplinkStateNeedsUpdate,
		Threadiness:    1,
	}
	c.uplinkStateController = controllerutil.NewController(
		"clustermanager-uplink-state-controller",
		uplinkStateCfg,
	)

	cudnCfg := &controllerutil.ControllerConfig[udnv1.ClusterUserDefinedNetwork]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		MaxAttempts:    controllerutil.InfiniteAttempts,
		Informer:       wf.ClusterUserDefinedNetworkInformer().Informer(),
		Lister:         c.cudnLister.List,
		Reconcile:      c.reconcileCUDN,
		ObjNeedsUpdate: cudnNeedsUpdate,
		Threadiness:    1,
	}
	c.cudnController = controllerutil.NewController(
		"clustermanager-uplink-cudn-controller",
		cudnCfg,
	)

	nodeCfg := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		MaxAttempts:    controllerutil.InfiniteAttempts,
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         c.nodeLister.List,
		Reconcile:      c.reconcileNode,
		ObjNeedsUpdate: nodeNeedsUpdate,
		Threadiness:    1,
	}
	c.nodeController = controllerutil.NewController(
		"clustermanager-uplink-node-controller",
		nodeCfg,
	)

	networkRefCfg := &controllerutil.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		MaxAttempts: controllerutil.InfiniteAttempts,
		Reconcile:   c.reconcileNetworkRef,
		Threadiness: 1,
	}
	c.networkRefController = controllerutil.NewReconciler(
		"clustermanager-uplink-network-ref-controller",
		networkRefCfg,
	)
	networkRefController := c.networkRefController
	c.networkRefReconciler = networkRefReconcilerFunc(
		func(nodeName, networkName string) {
			if nodeName == "" || networkName == "" {
				return
			}
			networkRefController.Reconcile(networkRefKey(nodeName, networkName))
		},
	)

	return c
}

// Start registers for network-reference updates, repairs stale UplinkStates,
// and starts the Uplink, UplinkState, CUDN, node, and network-reference
// reconcilers.
func (c *Controller) Start() error {
	c.networkRefID = c.networkManager.RegisterNetworkRefReconciler(c.networkRefReconciler)
	ctx := context.Background()
	err := controllerutil.StartWithInitialSync(
		func() error { return c.initialSync(ctx) },
		c.uplinkController,
		c.uplinkStateController,
		c.cudnController,
		c.nodeController,
		c.networkRefController,
	)
	if err != nil {
		c.networkManager.DeRegisterNetworkRefReconciler(c.networkRefID)
		c.networkRefID = 0
		return err
	}
	klog.Infof("Cluster manager Uplink controller started")
	return nil
}

func (c *Controller) initialSync(ctx context.Context) error {
	states, err := c.uplinkStateLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list UplinkStates for initial sync: %w", err)
	}

	var errs []error
	for _, state := range states {
		uplinkName, nodeName := c.rememberUplinkStateIdentity(state)
		stale, reason, err := c.staleUplinkState(state, uplinkName, nodeName)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !stale {
			continue
		}
		klog.Infof("Deleting stale UplinkState %s during initial sync: %s",
			state.Name, reason)
		if err := c.deleteUplinkState(ctx, state.Name); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.rebuildCUDNUplinkReferences(); err != nil {
		errs = append(errs, err)
	} else if err := c.syncAllUplinkFinalizers(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Stop shuts down the Uplink controller.
func (c *Controller) Stop() {
	if c.networkRefID != 0 {
		c.networkManager.DeRegisterNetworkRefReconciler(c.networkRefID)
		c.networkRefID = 0
	}
	controllerutil.Stop(
		c.uplinkController,
		c.uplinkStateController,
		c.cudnController,
		c.nodeController,
		c.networkRefController,
	)
}

func (c *Controller) reconcileUplink(ctx context.Context, key string) error {
	uplink, err := c.uplinkLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.deleteUplinkStatesFor(ctx, key)
		}
		return fmt.Errorf("failed to get Uplink %s: %w", key, err)
	}

	referencingCUDNs := c.getCUDNsReferencingUplink(uplink.Name)

	if !uplink.DeletionTimestamp.IsZero() {
		if err := c.updateStatus(ctx, uplink,
			metav1.ConditionFalse, reasonUplinkTerminating,
			"Uplink is terminating and cannot accept new CUDN references"); err != nil {
			return err
		}
		if len(referencingCUDNs) > 0 {
			klog.Infof("Uplink %s is terminating and still referenced by CUDNs [%s]",
				uplink.Name, strings.Join(referencingCUDNs, ", "))
		}
		c.reconcileCUDNNames(referencingCUDNs)
		return nil
	}

	selected, conflicts, validationErr := c.resolveSelectedNodeConfigs(uplink)
	if validationErr != nil {
		err := c.updateStatus(ctx, uplink,
			metav1.ConditionFalse, reasonInvalidSpec,
			"Uplink is not ready because its spec is not accepted")
		c.reconcileCUDNNames(referencingCUDNs)
		if err != nil {
			return errors.Join(validationErr, err)
		}
		// A rejected spec is not retryable: the cluster admin must fix the
		// Uplink CR, and that update triggers a new reconcile.
		return nil
	}

	readyStatus, readyReason, readyMessage := c.readyCondition(
		uplink,
		selected,
		conflicts,
	)
	if err := c.updateStatus(ctx, uplink,
		readyStatus, readyReason, readyMessage); err != nil {
		return err
	}
	c.reconcileCUDNNames(referencingCUDNs)
	// A not-ready Uplink needs no retry: readiness is recomputed whenever
	// an UplinkState, node or CUDN changes.
	return nil
}

func (c *Controller) reconcileUplinkState(_ context.Context, key string) error {
	uplinkState, err := c.uplinkStateLister.Get(key)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get UplinkState %s: %w", key, err)
	}
	if err == nil {
		if uplinkName, _ := c.rememberUplinkStateIdentity(uplinkState); uplinkName != "" {
			c.uplinkController.Reconcile(uplinkName)
			return nil
		}

		c.uplinkController.ReconcileAll()
		return nil
	}

	uplinkName, _ := c.forgetUplinkStateIdentity(key)
	if uplinkName == "" {
		var ok bool
		uplinkName, ok, err = c.uplinkNameForStateName(key)
		if err != nil {
			return err
		}
		if !ok {
			c.uplinkController.ReconcileAll()
			return nil
		}
	}

	c.uplinkController.Reconcile(uplinkName)
	return nil
}

func (c *Controller) reconcileNode(_ context.Context, _ string) error {
	c.uplinkController.ReconcileAll()
	return nil
}

func (c *Controller) reconcileCUDN(ctx context.Context, key string) error {
	cudn, err := c.cudnLister.Get(key)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get CUDN %s: %w", key, err)
		}
		c.deleteCUDNUplinkReferences(key)
		return c.syncAllUplinkFinalizers(ctx)
	}

	c.setCUDNUplinkReferences(cudn.Name, cudn.Spec.Uplinks)
	if err := c.syncAllUplinkFinalizers(ctx); err != nil {
		return err
	}
	return c.updateCUDNUplinksReady(ctx, cudn)
}

func (c *Controller) reconcileNetworkRef(_ context.Context, key string) error {
	_, networkName, err := parseNetworkRefKey(key)
	if err != nil {
		klog.Warningf("Skipping invalid Uplink network-ref key %q: %v", key, err)
		return nil
	}
	udnNamespace, cudnName := util.ParseNetworkName(networkName)
	if cudnName == "" || udnNamespace != "" {
		return nil
	}
	c.cudnController.Reconcile(cudnName)
	return nil
}

func (c *Controller) ensureFinalizer(ctx context.Context, uplink *uplinkv1alpha1.Uplink) error {
	uplinkCopy := uplink.DeepCopy()
	if !k8scontrollerutil.AddFinalizer(uplinkCopy, finalizerUplink) {
		return nil
	}
	if err := c.applyUplinkFinalizers(ctx, uplinkCopy.Name, uplinkCopy.Finalizers); err != nil {
		return fmt.Errorf("failed to add finalizer to Uplink %s: %w", uplink.Name, err)
	}
	klog.Infof("Added finalizer to Uplink %s", uplink.Name)
	return nil
}

func (c *Controller) removeFinalizer(ctx context.Context, uplink *uplinkv1alpha1.Uplink) error {
	uplinkCopy := uplink.DeepCopy()
	if !k8scontrollerutil.RemoveFinalizer(uplinkCopy, finalizerUplink) {
		return nil
	}
	if err := c.applyUplinkFinalizers(ctx, uplinkCopy.Name, uplinkCopy.Finalizers); err != nil {
		return fmt.Errorf("failed to remove finalizer from Uplink %s: %w", uplink.Name, err)
	}
	klog.Infof("Removed finalizer from Uplink %s", uplink.Name)
	return nil
}

func (c *Controller) applyUplinkFinalizers(ctx context.Context, uplinkName string, finalizers []string) error {
	apply := uplinkapply.Uplink(uplinkName)
	if len(finalizers) > 0 {
		apply = apply.WithFinalizers(finalizers...)
	}
	_, err := c.uplinkClient.K8sV1alpha1().Uplinks().Apply(
		ctx,
		apply,
		metav1.ApplyOptions{FieldManager: fieldManager, Force: true},
	)
	return err
}

func (c *Controller) syncAllUplinkFinalizers(ctx context.Context) error {
	uplinks, err := c.uplinkLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list Uplinks: %w", err)
	}

	var errs []error
	for _, uplink := range uplinks {
		referencingCUDNs := c.getCUDNsReferencingUplink(uplink.Name)
		if err := c.syncUplinkFinalizer(ctx, uplink, referencingCUDNs); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) syncUplinkFinalizer(ctx context.Context, uplink *uplinkv1alpha1.Uplink, referencingCUDNs []string) error {
	if len(referencingCUDNs) > 0 {
		if !uplink.DeletionTimestamp.IsZero() {
			return nil
		}
		return c.ensureFinalizer(ctx, uplink)
	}
	if !uplink.DeletionTimestamp.IsZero() {
		if err := c.deleteUplinkStatesFor(ctx, uplink.Name); err != nil {
			return err
		}
	}
	return c.removeFinalizer(ctx, uplink)
}

func (c *Controller) rebuildCUDNUplinkReferences() error {
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list CUDNs for Uplink reference cache: %w", err)
	}

	cudnsByUplink := make(map[string]sets.Set[string])
	for _, cudn := range cudns {
		for _, uplinkName := range cudn.Spec.Uplinks {
			cudnNames := cudnsByUplink[uplinkName]
			if cudnNames == nil {
				cudnNames = sets.New[string]()
				cudnsByUplink[uplinkName] = cudnNames
			}
			cudnNames.Insert(cudn.Name)
		}
	}

	c.cudnsByUplinkMu.Lock()
	c.cudnsByUplink = cudnsByUplink
	c.cudnsByUplinkMu.Unlock()
	return nil
}

func (c *Controller) getCUDNsReferencingUplink(uplinkName string) []string {
	c.cudnsByUplinkMu.RLock()
	defer c.cudnsByUplinkMu.RUnlock()
	return sets.List(c.cudnsByUplink[uplinkName])
}

func (c *Controller) setCUDNUplinkReferences(cudnName string, uplinkNames []string) {
	c.cudnsByUplinkMu.Lock()
	defer c.cudnsByUplinkMu.Unlock()

	desiredUplinks := sets.New(uplinkNames...)
	for uplinkName, cudnNames := range c.cudnsByUplink {
		if !cudnNames.Has(cudnName) {
			continue
		}
		if desiredUplinks.Has(uplinkName) {
			desiredUplinks.Delete(uplinkName)
			continue
		}
		cudnNames.Delete(cudnName)
		if len(cudnNames) == 0 {
			delete(c.cudnsByUplink, uplinkName)
		}
	}

	for uplinkName := range desiredUplinks {
		cudnNames := c.cudnsByUplink[uplinkName]
		if cudnNames == nil {
			cudnNames = sets.New[string]()
			c.cudnsByUplink[uplinkName] = cudnNames
		}
		cudnNames.Insert(cudnName)
	}
}

func (c *Controller) deleteCUDNUplinkReferences(cudnName string) {
	c.cudnsByUplinkMu.Lock()
	defer c.cudnsByUplinkMu.Unlock()

	for uplinkName, cudnNames := range c.cudnsByUplink {
		if !cudnNames.Has(cudnName) {
			continue
		}
		cudnNames.Delete(cudnName)
		if len(cudnNames) == 0 {
			delete(c.cudnsByUplink, uplinkName)
		}
	}
}

func (c *Controller) reconcileCUDNNames(cudnNames []string) {
	for _, cudnName := range cudnNames {
		c.cudnController.Reconcile(cudnName)
	}
}

func (c *Controller) rememberUplinkStateIdentity(state *uplinkv1alpha1.UplinkState) (uplinkName, nodeName string) {
	uplinkName, nodeName = uplinkutil.StateIdentity(state)
	c.uplinkStateIdentityMu.Lock()
	defer c.uplinkStateIdentityMu.Unlock()
	c.uplinkByStateName[state.Name] = uplinkName
	return uplinkName, nodeName
}

func (c *Controller) forgetUplinkStateIdentity(name string) (string, bool) {
	c.uplinkStateIdentityMu.Lock()
	defer c.uplinkStateIdentityMu.Unlock()
	uplinkName, ok := c.uplinkByStateName[name]
	delete(c.uplinkByStateName, name)
	return uplinkName, ok
}

func (c *Controller) uplinkNameForStateName(stateName string) (string, bool, error) {
	uplinks, err := c.uplinkLister.List(labels.Everything())
	if err != nil {
		return "", false, fmt.Errorf("failed to list Uplinks for UplinkState %s: %w", stateName, err)
	}
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return "", false, fmt.Errorf("failed to list Nodes for UplinkState %s: %w", stateName, err)
	}
	nodeNames := make([]string, 0, len(nodes))
	for _, node := range nodes {
		nodeNames = append(nodeNames, node.Name)
	}

	uplink, found := uplinkutil.UplinkForState(uplinks, nodeNames, stateName)
	if !found {
		return "", false, nil
	}
	return uplink.Name, true, nil
}

// Node controllers normally remove their own UplinkStates, but the responsible
// controller may no longer be running when its Node is deleted or Uplink
// selection changes. Cluster-manager removes those stale states before they
// affect aggregate Uplink and CUDN readiness.
func (c *Controller) staleUplinkState(
	state *uplinkv1alpha1.UplinkState,
	uplinkName, nodeName string,
) (bool, string, error) {
	if uplinkName == "" || nodeName == "" {
		return true, "missing UplinkState identity", nil
	}

	uplink, err := c.uplinkLister.Get(uplinkName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, fmt.Sprintf("Uplink %s is missing", uplinkName), nil
		}
		return false, "", fmt.Errorf("failed to get Uplink %s for UplinkState %s: %w",
			uplinkName, state.Name, err)
	}

	node, err := c.nodeLister.Get(nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, fmt.Sprintf("Node %s is missing", nodeName), nil
		}
		return false, "", fmt.Errorf("failed to get Node %s for UplinkState %s: %w",
			nodeName, state.Name, err)
	}

	selected, err := uplinkSelectsNode(uplink, node)
	if err != nil {
		return false, "", fmt.Errorf("failed to resolve Uplink %s selectors for UplinkState %s: %w",
			uplinkName, state.Name, err)
	}
	if !selected {
		return true, fmt.Sprintf("Uplink %s no longer selects Node %s",
			uplinkName, nodeName), nil
	}
	return false, "", nil
}

func uplinkSelectsNode(uplink *uplinkv1alpha1.Uplink, node *corev1.Node) (bool, error) {
	selected, overlapping, err := uplinkutil.SelectNodeConfig(uplink, node)
	if err != nil {
		return false, err
	}
	return selected != nil || overlapping, nil
}

func (c *Controller) resolveSelectedNodeConfigs(
	uplink *uplinkv1alpha1.Uplink,
) (map[string]uplinkv1alpha1.UplinkNodeConfig, []string, error) {
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	return uplinkutil.SelectNodeConfigs(uplink, nodes)
}

func (c *Controller) deleteUplinkStatesFor(ctx context.Context, uplinkName string) error {
	states, err := c.uplinkStateLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list UplinkStates: %w", err)
	}
	for _, state := range states {
		if state.Spec.UplinkName != uplinkName {
			continue
		}
		if err := c.deleteUplinkState(ctx, state.Name); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) deleteUplinkState(ctx context.Context, name string) error {
	err := c.uplinkClient.K8sV1alpha1().UplinkStates().Delete(
		ctx,
		name,
		metav1.DeleteOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete UplinkState %s: %w", name, err)
	}
	return nil
}

func (c *Controller) readyCondition(
	uplink *uplinkv1alpha1.Uplink,
	selected map[string]uplinkv1alpha1.UplinkNodeConfig,
	conflicts []string,
) (metav1.ConditionStatus, string, string) {
	selectedNodes := len(selected) + len(conflicts)
	if selectedNodes == 0 {
		return metav1.ConditionFalse, reasonNoMatchingNodes,
			"no nodes match any Uplink nodeConfig"
	}

	nodeNames := make([]string, 0, len(selected))
	for nodeName := range selected {
		nodeNames = append(nodeNames, nodeName)
	}
	sort.Strings(nodeNames)

	failures := make([]uplinkNodeFailure, 0, len(conflicts))
	for _, nodeName := range conflicts {
		failures = append(failures, uplinkNodeFailure{
			node:   nodeName,
			reason: reasonNodeSelectorOverlap,
			detail: reasonNodeSelectorOverlap,
		})
	}
	for _, nodeName := range nodeNames {
		entry := selected[nodeName]
		stateName := uplinkutil.StateName(uplink.Name, nodeName)
		state, err := c.uplinkStateLister.Get(stateName)
		if err != nil {
			detail := reasonMissingUplinkState
			if !apierrors.IsNotFound(err) {
				detail = fmt.Sprintf("UplinkStateLookupFailed: %v", err)
			}
			failures = append(failures, uplinkNodeFailure{
				node:   nodeName,
				reason: reasonMissingUplinkState,
				detail: detail,
			})
			continue
		}

		stateUplink, stateNode := uplinkutil.StateIdentity(state)
		if stateUplink != uplink.Name {
			failures = append(failures, uplinkNodeFailure{
				node:   nodeName,
				reason: reasonUplinkStateIdentityError,
				detail: fmt.Sprintf("UplinkStateIdentityError: uplinkName=%q", stateUplink),
			})
			continue
		}
		if stateNode != nodeName {
			failures = append(failures, uplinkNodeFailure{
				node:   nodeName,
				reason: reasonUplinkStateIdentityError,
				detail: fmt.Sprintf("UplinkStateIdentityError: nodeName=%q", stateNode),
			})
			continue
		}

		if detail := uplinkStateNotResolvedReason(state, entry); detail != "" {
			failures = append(failures, uplinkNodeFailure{
				node:   nodeName,
				reason: reasonUplinkStateNotResolved,
				detail: detail,
			})
		}
	}
	if len(failures) > 0 {
		return metav1.ConditionFalse, aggregateUplinkFailureReason(failures),
			uplinkNodeFailureMessage(failures, selectedNodes)
	}
	return metav1.ConditionTrue, reasonReady,
		fmt.Sprintf("all %d selected node(s) are resolved", selectedNodes)
}

func uplinkStateNotResolvedReason(
	state *uplinkv1alpha1.UplinkState,
	nodeConfig uplinkv1alpha1.UplinkNodeConfig,
) string {
	cond := meta.FindStatusCondition(
		state.Status.Conditions,
		uplinkv1alpha1.UplinkStateConditionResolved,
	)
	if cond == nil {
		return hostDataFailureReason(state, "ResolvedConditionMissing")
	}
	if cond.Status != metav1.ConditionTrue {
		if cond.Reason == uplinkv1alpha1.UplinkStateReasonWaitingForDPUHost {
			return hostDataFailureReason(state, cond.Reason)
		}
		if cond.Reason != "" {
			return cond.Reason
		}
		return reasonUplinkStateNotResolved
	}
	if state.Status.Type != nodeConfig.Type || state.Status.HostInterfaceName != nodeConfig.HostInterfaceName {
		return "NodeConfigMismatch"
	}
	return ""
}

// hostDataFailureReason surfaces the DPU-host-side root cause when the DPU
// side is blocked on host data: the aggregate message samples only reasons,
// and WaitingForDPUHost alone would hide why the host has not published.
func hostDataFailureReason(state *uplinkv1alpha1.UplinkState, fallback string) string {
	cond := meta.FindStatusCondition(
		state.Status.Conditions,
		uplinkv1alpha1.UplinkStateConditionHostDataReady,
	)
	if cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason != "" {
		return cond.Reason
	}
	return fallback
}

func uplinkStateResolved(state *uplinkv1alpha1.UplinkState, nodeConfig uplinkv1alpha1.UplinkNodeConfig) bool {
	return uplinkStateNotResolvedReason(state, nodeConfig) == ""
}

func aggregateUplinkFailureReason(failures []uplinkNodeFailure) string {
	reasons := sets.New[string]()
	for _, failure := range failures {
		reasons.Insert(failure.reason)
	}
	for _, reason := range []string{
		reasonNodeSelectorOverlap,
		reasonMissingUplinkState,
		reasonUplinkStateNotResolved,
		reasonUplinkStateIdentityError,
	} {
		if reasons.Has(reason) {
			return reason
		}
	}
	return reasonUplinkStateNotResolved
}

func uplinkNodeFailureMessage(failures []uplinkNodeFailure, selectedNodes int) string {
	examples := make([]string, 0, len(failures))
	for _, failure := range failures {
		examples = append(examples, fmt.Sprintf("%s=%s", failure.node, failure.detail))
	}
	sort.Strings(examples)
	const maxFailureExamples = 3
	if len(examples) > maxFailureExamples {
		examples = examples[:maxFailureExamples]
	}
	return fmt.Sprintf("%d of %d selected node(s) have readiness failures; examples: %s",
		len(failures), selectedNodes, strings.Join(examples, ", "))
}

func cudnUplinkStateGatewayNotReadyReason(state *uplinkv1alpha1.UplinkState) string {
	if reason := cudnUplinkStateGatewayConditionNotReadyReason(
		state, uplinkv1alpha1.UplinkStateConditionGatewayReady); reason != "" {
		return reason
	}
	// In split-DPU deployments, recognizable by the DPU-host-owned
	// HostDataReady condition, host-side gateway programming is reported
	// separately through HostGatewayReady and must be ready as well.
	if meta.FindStatusCondition(state.Status.Conditions, uplinkv1alpha1.UplinkStateConditionHostDataReady) != nil {
		return cudnUplinkStateGatewayConditionNotReadyReason(
			state, uplinkv1alpha1.UplinkStateConditionHostGatewayReady)
	}
	return ""
}

func cudnUplinkStateGatewayConditionNotReadyReason(state *uplinkv1alpha1.UplinkState, conditionType string) string {
	condition := meta.FindStatusCondition(
		state.Status.Conditions,
		conditionType,
	)
	if condition == nil {
		return reasonUplinksNotReady
	}
	if condition.Status == metav1.ConditionTrue {
		if condition.Reason == uplinkv1alpha1.UplinkStateReasonNoActiveCUDNs {
			return reasonNoActiveCUDNs
		}
		return ""
	}
	switch condition.Reason {
	case uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed:
		return reasonUplinkVRFAttachmentFailed
	case uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed:
		return reasonUplinkBridgeMappingFailed
	case uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed:
		return reasonUplinkGatewayProgrammingFailed
	case uplinkv1alpha1.UplinkStateReasonConfigurationConflict:
		return reasonUplinkConfigurationConflict
	default:
		return reasonUplinksNotReady
	}
}

func (c *Controller) updateCUDNUplinksReady(ctx context.Context, cudn *udnv1.ClusterUserDefinedNetwork) error {
	condition, changed := util.MergeStatusCondition(
		cudn.Status.Conditions,
		c.cudnUplinksReadyCondition(cudn),
	)
	if !changed {
		return nil
	}

	_, err := c.udnClient.K8sV1().ClusterUserDefinedNetworks().ApplyStatus(
		ctx,
		udnapply.ClusterUserDefinedNetwork(cudn.Name).WithStatus(
			udnapply.ClusterUserDefinedNetworkStatus().
				WithConditions(util.ConditionToApply(condition)),
		),
		metav1.ApplyOptions{
			FieldManager: fieldManager,
			Force:        true,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to update CUDN %s UplinksReady condition: %w",
			cudn.Name, err)
	}
	return nil
}

func (c *Controller) cudnUplinksReadyCondition(cudn *udnv1.ClusterUserDefinedNetwork) metav1.Condition {
	status, reason, message := c.cudnUplinksReadyStatus(cudn)
	return metav1.Condition{
		Type:    conditionTypeUplinksReady,
		Status:  status,
		Reason:  reason,
		Message: message,
	}
}

func (c *Controller) cudnUplinksReadyStatus(
	cudn *udnv1.ClusterUserDefinedNetwork,
) (metav1.ConditionStatus, string, string) {
	if len(cudn.Spec.Uplinks) == 0 {
		return metav1.ConditionTrue, reasonUplinksReady,
			"CUDN does not reference an Uplink"
	}
	if config.Gateway.Mode != config.GatewayModeShared {
		return metav1.ConditionFalse, reasonUplinkUnsupportedGatewayMode,
			fmt.Sprintf("Uplink requires shared gateway mode, got %s",
				config.Gateway.Mode)
	}
	if cudn.Spec.Network.Transport == udnv1.TransportOptionEVPN {
		return metav1.ConditionFalse, reasonUplinkUnsupportedTransport,
			"Uplink is not supported with EVPN transport"
	}

	activeNodes, err := c.activeCUDNNodes(cudn)
	if err != nil {
		return metav1.ConditionFalse, reasonUplinkNotResolvedForNode,
			fmt.Sprintf("failed to list active nodes for CUDN %s: %v",
				cudn.Name, err)
	}
	failures := make([]cudnUplinkFailure, 0)

	for _, uplinkName := range cudn.Spec.Uplinks {
		uplink, err := c.uplinkLister.Get(uplinkName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return metav1.ConditionFalse, reasonUplinkNotFound,
					fmt.Sprintf("Uplink %s is missing", uplinkName)
			}
			return metav1.ConditionFalse, reasonUplinkNotFound,
				fmt.Sprintf("failed to get Uplink %s: %v", uplinkName, err)
		}
		if !uplink.DeletionTimestamp.IsZero() {
			failures = append(failures, cudnUplinkFailure{
				uplink: uplinkName,
				reason: reasonUplinkTerminating,
			})
			continue
		}

		// Resolve all nodes selected by the Uplink so overlap detection matches
		// Uplink status, but only report CUDN failures for active CUDN nodes.
		selected, conflicts, err := c.resolveSelectedNodeConfigs(uplink)
		if err != nil {
			return metav1.ConditionFalse, reasonUplinkNotResolvedForNode,
				fmt.Sprintf("Uplink %s has invalid nodeConfigs: %v",
					uplinkName, err)
		}
		conflictSet := sets.New[string](conflicts...)

		for _, node := range activeNodes {
			if conflictSet.Has(node.Name) {
				failures = append(failures, cudnUplinkFailure{
					uplink: uplinkName,
					reason: reasonUplinkOverlapOnNode,
					node:   node.Name,
				})
				continue
			}
			nodeConfig, ok := selected[node.Name]
			if !ok {
				failures = append(failures, cudnUplinkFailure{
					uplink: uplinkName,
					reason: reasonUplinkNotFoundForNode,
					node:   node.Name,
				})
				continue
			}
			state, err := uplinkutil.GetState(c.uplinkStateLister, uplinkName, node.Name)
			if err != nil {
				failures = append(failures, cudnUplinkFailure{
					uplink: uplinkName,
					reason: reasonUplinkNotResolvedForNode,
					node:   node.Name,
				})
				continue
			}
			if !uplinkStateResolved(state, nodeConfig) {
				failures = append(failures, cudnUplinkFailure{
					uplink: uplinkName,
					reason: reasonUplinkNotResolvedForNode,
					node:   node.Name,
				})
				continue
			}
			if reason := cudnUplinkStateGatewayNotReadyReason(state); reason != "" {
				failures = append(failures, cudnUplinkFailure{
					uplink: uplinkName,
					reason: reason,
					node:   node.Name,
				})
			}
		}
	}
	if len(failures) > 0 {
		reason, message := summarizeCUDNUplinkFailures(failures)
		return metav1.ConditionFalse, reason, message
	}
	return metav1.ConditionTrue, reasonUplinksReady,
		"Uplinks are ready for all active CUDN nodes"
}

func summarizeCUDNUplinkFailures(failures []cudnUplinkFailure) (string, string) {
	selectedReason := failures[0].reason
	mixedReasons := false
	examples := make([]string, 0, len(failures))
	for _, failure := range failures {
		if failure.reason != selectedReason {
			mixedReasons = true
		}
		if failure.uplink != "" && failure.node != "" {
			examples = append(examples, fmt.Sprintf("%s/%s=%s",
				failure.uplink, failure.node, failure.reason))
			continue
		}
		if failure.uplink != "" {
			examples = append(examples, fmt.Sprintf("%s=%s",
				failure.uplink, failure.reason))
			continue
		}
		examples = append(examples, failure.reason)
	}
	if mixedReasons {
		selectedReason = reasonUplinksNotReady
	}

	sort.Strings(examples)
	const maxFailureExamples = 3
	if len(examples) > maxFailureExamples {
		examples = examples[:maxFailureExamples]
	}

	return selectedReason, fmt.Sprintf(
		"%d active node/uplink readiness check(s) failed; examples: %s",
		len(failures),
		strings.Join(examples, ", "),
	)
}

func (c *Controller) activeCUDNNodes(cudn *udnv1.ClusterUserDefinedNetwork) ([]*corev1.Node, error) {
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	networkName := util.GenerateCUDNNetworkName(cudn.Name)
	activeNodes := make([]*corev1.Node, 0, len(nodes))
	for _, node := range nodes {
		if c.networkManager.NodeHasNetwork(node.Name, networkName) {
			activeNodes = append(activeNodes, node)
		}
	}
	sort.Slice(activeNodes, func(i, j int) bool {
		return activeNodes[i].Name < activeNodes[j].Name
	})
	return activeNodes, nil
}

func (c *Controller) updateStatus(
	ctx context.Context,
	uplink *uplinkv1alpha1.Uplink,
	readyStatus metav1.ConditionStatus,
	readyReason string,
	readyMessage string,
) error {
	ready, changed := util.MergeStatusCondition(
		uplink.Status.Conditions,
		metav1.Condition{
			Type:    uplinkv1alpha1.UplinkConditionReady,
			Status:  readyStatus,
			Reason:  readyReason,
			Message: readyMessage,
		},
	)

	if !changed &&
		meta.FindStatusCondition(uplink.Status.Conditions, obsoleteUplinkConditionDegraded) == nil &&
		meta.FindStatusCondition(uplink.Status.Conditions, obsoleteUplinkConditionReferenced) == nil {
		return nil
	}

	_, err := c.uplinkClient.K8sV1alpha1().Uplinks().ApplyStatus(
		ctx,
		uplinkapply.Uplink(uplink.Name).WithStatus(
			uplinkapply.UplinkStatus().WithConditions(util.ConditionToApply(ready)),
		),
		metav1.ApplyOptions{
			FieldManager: fieldManager,
			Force:        true,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to update Uplink %s status: %w", uplink.Name, err)
	}
	return nil
}

type networkRefReconcilerFunc func(nodeName, networkName string)

func (f networkRefReconcilerFunc) Reconcile(nodeName, networkName string) {
	f(nodeName, networkName)
}

func networkRefKey(nodeName, networkName string) string {
	return nodeName + networkRefSeparator + networkName
}

func parseNetworkRefKey(key string) (nodeName, networkName string, err error) {
	nodeName, networkName, found := strings.Cut(key, networkRefSeparator)
	if !found || nodeName == "" || networkName == "" {
		return "", "", fmt.Errorf("invalid network-ref key %q", key)
	}
	return nodeName, networkName, nil
}

func uplinkNeedsUpdate(oldObj, newObj *uplinkv1alpha1.Uplink) bool {
	if oldObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec, newObj.Spec) ||
		oldObj.DeletionTimestamp.IsZero() != newObj.DeletionTimestamp.IsZero()
}

func uplinkStateNeedsUpdate(oldObj, newObj *uplinkv1alpha1.UplinkState) bool {
	if oldObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec, newObj.Spec) ||
		!reflect.DeepEqual(oldObj.Status, newObj.Status)
}

func cudnNeedsUpdate(oldObj, newObj *udnv1.ClusterUserDefinedNetwork) bool {
	if oldObj == nil {
		return newObj != nil
	}
	if newObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Spec.Uplinks, newObj.Spec.Uplinks) ||
		oldObj.DeletionTimestamp.IsZero() != newObj.DeletionTimestamp.IsZero()
}

func nodeNeedsUpdate(oldObj, newObj *corev1.Node) bool {
	if oldObj == nil {
		return true
	}
	return !reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		oldObj.DeletionTimestamp.IsZero() != newObj.DeletionTimestamp.IsZero()
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package managedbgp

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	frrclientset "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	frrlisters "github.com/metallb/frr-k8s/pkg/client/listers/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	ralisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/listers/routeadvertisements/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	cudnclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	cudnlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// controllerName is the name of the managed BGP controller
	controllerName = "managed-bgp-controller"
	// fieldManager identifies this controller as the server-side author of owned resources.
	// Used with FieldManager in Create/Update options so that isOwnUpdate can
	// filter out informer events that result from our own writes.
	fieldManager = "clustermanager-managed-bgp-controller"
	// managedNetworkLabel is the label used to identify managed resources by network name.
	// It is set on managed RouteAdvertisements, FRRConfigurations, and CUDNs.
	managedNetworkLabel = "k8s.ovn.org/managed-bgp"
	// managedNamePrefix is the name for the managed base FRRConfiguration and prefix for RouteAdvertisements
	managedNamePrefix = "ovnk-managed-bgp"
)

// Controller manages the BGP topology for no-overlay networks with managed routing
type Controller struct {
	frrClient        frrclientset.Interface
	frrLister        frrlisters.FRRConfigurationLister
	raClient         raclientset.Interface
	raLister         ralisters.RouteAdvertisementsLister
	cudnClient       cudnclientset.Interface
	cudnLister       cudnlisters.ClusterUserDefinedNetworkLister
	nodeEventHandler cache.ResourceEventHandlerRegistration
	raEventHandler   cache.ResourceEventHandlerRegistration
	frrEventHandler  cache.ResourceEventHandlerRegistration
	cudnController   controllerutil.Controller
	configReconciler controllerutil.Reconciler
	wf               *factory.WatchFactory
}

// NewController creates a new managed BGP controller
func NewController(
	wf *factory.WatchFactory,
	frrClient frrclientset.Interface,
	raClient raclientset.Interface,
	cudnClient cudnclientset.Interface,
) (*Controller, error) {
	c := &Controller{
		frrClient:  frrClient,
		frrLister:  wf.FRRConfigurationsInformer().Lister(),
		raClient:   raClient,
		raLister:   wf.RouteAdvertisementsInformer().Lister(),
		cudnClient: cudnClient,
		wf:         wf,
	}

	configReconcilerCfg := &controllerutil.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.ensureManagedConfiguration,
		Threadiness: 1,
	}
	c.configReconciler = controllerutil.NewReconciler(controllerName+"-config", configReconcilerCfg)

	var err error
	c.nodeEventHandler, err = wf.NodeCoreInformer().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { c.onNodeUpdate(nil, obj) },
			UpdateFunc: c.onNodeUpdate,
			DeleteFunc: func(obj interface{}) { c.onNodeUpdate(obj, nil) },
		})
	if err != nil {
		return nil, fmt.Errorf("failed to add node event handler: %w", err)
	}

	c.raEventHandler, err = wf.RouteAdvertisementsInformer().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { c.onRAUpdate(nil, obj) },
			UpdateFunc: c.onRAUpdate,
			DeleteFunc: func(obj interface{}) { c.onRAUpdate(obj, nil) },
		})
	if err != nil {
		return nil, fmt.Errorf("failed to add RouteAdvertisement event handler: %w", err)
	}

	c.frrEventHandler, err = wf.FRRConfigurationsInformer().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { c.onFRRUpdate(nil, obj) },
			UpdateFunc: c.onFRRUpdate,
			DeleteFunc: func(obj interface{}) { c.onFRRUpdate(obj, nil) },
		})
	if err != nil {
		return nil, fmt.Errorf("failed to add FRRConfiguration event handler: %w", err)
	}

	if util.IsNetworkSegmentationSupportEnabled() {
		c.cudnLister = wf.ClusterUserDefinedNetworkInformer().Lister()
		cudnConfig := &controllerutil.ControllerConfig[userdefinednetworkv1.ClusterUserDefinedNetwork]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      c.reconcileCUDN,
			Threadiness:    1,
			Informer:       wf.ClusterUserDefinedNetworkInformer().Informer(),
			Lister:         wf.ClusterUserDefinedNetworkInformer().Lister().List,
			ObjNeedsUpdate: c.cudnNeedsUpdate,
		}
		c.cudnController = controllerutil.NewController(controllerName+"-cudn", cudnConfig)
	}

	return c, nil
}

// Start starts the managed BGP controller.
func (c *Controller) Start() error {
	klog.Infof("Starting managed BGP controller")

	controllers := []controllerutil.Reconciler{
		c.configReconciler,
	}
	if c.cudnController != nil {
		controllers = append(controllers, c.cudnController)
	}

	if err := controllerutil.Start(controllers...); err != nil {
		return err
	}

	if isCDNManaged() {
		// Trigger initial reconciliation for the CDN managed RouteAdvertisement so
		// it is created on startup even before any RA informer events arrive.
		c.configReconciler.Reconcile("")
	}
	return nil
}

// Stop stops the managed BGP controller
func (c *Controller) Stop() {
	klog.Infof("Stopping managed BGP controller")

	// Remove event handlers
	if c.nodeEventHandler != nil {
		if err := c.wf.NodeCoreInformer().Informer().RemoveEventHandler(c.nodeEventHandler); err != nil {
			klog.Errorf("Failed to remove node event handler: %v", err)
		}
	}
	if c.raEventHandler != nil {
		if err := c.wf.RouteAdvertisementsInformer().Informer().RemoveEventHandler(c.raEventHandler); err != nil {
			klog.Errorf("Failed to remove route advertisement event handler: %v", err)
		}
	}
	if c.frrEventHandler != nil {
		if err := c.wf.FRRConfigurationsInformer().Informer().RemoveEventHandler(c.frrEventHandler); err != nil {
			klog.Errorf("Failed to remove frr event handler: %v", err)
		}
	}

	controllers := []controllerutil.Reconciler{
		c.configReconciler,
	}
	if c.cudnController != nil {
		controllers = append(controllers, c.cudnController)
	}
	controllerutil.Stop(controllers...)
}

func (c *Controller) onNodeUpdate(oldObj, newObj interface{}) {
	oldNode, _ := oldObj.(*corev1.Node)
	newNode, _ := newObj.(*corev1.Node)
	if c.nodeNeedsUpdate(oldNode, newNode) {
		c.configReconciler.Reconcile("")
	}
}

func (c *Controller) onRAUpdate(oldObj, newObj interface{}) {
	oldRA, _ := oldObj.(*ratypes.RouteAdvertisements)
	newRA, _ := newObj.(*ratypes.RouteAdvertisements)
	if c.raNeedsUpdate(oldRA, newRA) {
		c.configReconciler.Reconcile("")
	}
}

func (c *Controller) onFRRUpdate(oldObj, newObj interface{}) {
	oldFRR, _ := oldObj.(*frrtypes.FRRConfiguration)
	newFRR, _ := newObj.(*frrtypes.FRRConfiguration)
	if c.frrNeedsUpdate(oldFRR, newFRR) {
		c.configReconciler.Reconcile("")
	}
}

// isOwnUpdate checks if an object was last updated by this controller,
// as indicated by its managed fields. Used to avoid reconciling events
// that result from our own writes.
func isOwnUpdate(oldManagedFields, newManagedFields []metav1.ManagedFieldsEntry) bool {
	return util.IsLastUpdatedByManager(fieldManager, oldManagedFields, newManagedFields)
}

// isManagedRA returns true if RA name has ovnk-managed-bgp prefix
func isManagedRA(ra *ratypes.RouteAdvertisements) bool {
	if ra == nil {
		return false
	}
	return strings.HasPrefix(ra.Name, managedNamePrefix+"-")
}

// isManagedBaseFRRConfiguration returns true if FRRConfiguration name is ovnk-managed-bgp
func isManagedBaseFRRConfiguration(cfg *frrtypes.FRRConfiguration) bool {
	if cfg == nil {
		return false
	}
	return cfg.Name == managedNamePrefix
}

// isCDNManaged returns true if the cluster default network is configured for
// no-overlay mode with managed routing.
func isCDNManaged() bool {
	return config.Default.Transport == types.NetworkTransportNoOverlay &&
		config.NoOverlay.Routing == config.NoOverlayRoutingManaged
}

// isCUDNManaged returns true if the given CUDN is configured for
// no-overlay transport with managed routing.
func isCUDNManaged(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) bool {
	if cudn == nil {
		return false
	}
	return cudn.Spec.Network.Transport == userdefinednetworkv1.TransportOptionNoOverlay &&
		cudn.Spec.Network.NoOverlay != nil &&
		cudn.Spec.Network.NoOverlay.Routing == userdefinednetworkv1.RoutingManaged
}

// managedRAName returns the fixed RA name for a given network.
// CDN gets "ovnk-managed-bgp-default", CUDNs share "ovnk-managed-bgp-cudn"
func managedRAName(networkName string) string {
	if networkName == types.DefaultNetworkName {
		return managedNamePrefix + "-default"
	}
	return managedNamePrefix + "-cudn"
}

// getManagedCUDNs returns all CUDNs that are configured for no-overlay with managed routing.
func (c *Controller) getManagedCUDNs() ([]*userdefinednetworkv1.ClusterUserDefinedNetwork, error) {
	if c.cudnLister == nil {
		return nil, nil
	}
	allCUDNs, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list CUDNs: %w", err)
	}
	var managed []*userdefinednetworkv1.ClusterUserDefinedNetwork
	for _, cudn := range allCUDNs {
		if isCUDNManaged(cudn) {
			managed = append(managed, cudn)
		}
	}
	return managed, nil
}

func (c *Controller) cudnNeedsUpdate(oldCUDN, newCUDN *userdefinednetworkv1.ClusterUserDefinedNetwork) bool {
	if newCUDN == nil {
		// Delete events bypass ObjNeedsUpdate and are always reconciled.
		return false
	}
	if oldCUDN == nil {
		return isCUDNManaged(newCUDN)
	}

	if !isCUDNManaged(oldCUDN) && !isCUDNManaged(newCUDN) {
		return false
	}
	// Ignore events from our own writes (e.g. ensureManagedNetworkLabel)
	if isOwnUpdate(oldCUDN.ManagedFields, newCUDN.ManagedFields) {
		return false
	}

	_, oldHasLabel := oldCUDN.Labels[managedNetworkLabel]
	newVal, newHasLabel := newCUDN.Labels[managedNetworkLabel]
	return oldHasLabel != newHasLabel || (newHasLabel && newVal != "")
}

func (c *Controller) nodeNeedsUpdate(oldNode, newNode *corev1.Node) bool {
	if oldNode == nil || newNode == nil {
		return true
	}
	// We care about node IP changes
	oldV4, oldV6 := util.GetNodeInternalAddrs(oldNode)
	newV4, newV6 := util.GetNodeInternalAddrs(newNode)
	return !reflect.DeepEqual(oldV4, newV4) || !reflect.DeepEqual(oldV6, newV6)
}

func (c *Controller) raNeedsUpdate(oldRA, newRA *ratypes.RouteAdvertisements) bool {
	if !isManagedRA(oldRA) && !isManagedRA(newRA) {
		return false
	}
	if newRA == nil {
		return true
	}
	var oldManagedFields []metav1.ManagedFieldsEntry
	if oldRA != nil {
		oldManagedFields = oldRA.ManagedFields
	}
	if isOwnUpdate(oldManagedFields, newRA.ManagedFields) {
		return false
	}
	if oldRA == nil {
		return true
	}
	return !reflect.DeepEqual(oldRA.Spec, newRA.Spec)
}

func (c *Controller) frrNeedsUpdate(oldFRR, newFRR *frrtypes.FRRConfiguration) bool {
	if !isManagedBaseFRRConfiguration(oldFRR) && !isManagedBaseFRRConfiguration(newFRR) {
		return false
	}
	if newFRR == nil {
		return true
	}
	var oldManagedFields []metav1.ManagedFieldsEntry
	if oldFRR != nil {
		oldManagedFields = oldFRR.ManagedFields
	}
	if isOwnUpdate(oldManagedFields, newFRR.ManagedFields) {
		return false
	}
	if oldFRR == nil {
		return true
	}
	oldValue, oldHasLabel := oldFRR.Labels[managedNetworkLabel]
	newValue, newHasLabel := newFRR.Labels[managedNetworkLabel]
	return !reflect.DeepEqual(oldFRR.Spec, newFRR.Spec) ||
		oldHasLabel != newHasLabel || oldValue != newValue
}

// reconcileCUDN handles CUDN events by ensuring the label is set and then
// triggering a reconciliation of the managed configuration.
func (c *Controller) reconcileCUDN(key string) error {
	cudn, err := c.cudnLister.Get(key)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get CUDN %s: %w", key, err)
	}

	if cudn != nil && isCUDNManaged(cudn) {
		// CUDNs are immutable and we won't ever have to make sure that the label does not exist.
		if err := c.ensureManagedNetworkLabel(cudn); err != nil {
			return err
		}
	}

	// Make sure that managed FRRConfiguration and RouteAdvertisement exist
	c.configReconciler.Reconcile("")
	return nil
}

// ensureManagedConfiguration configures:
// - FRRConfiguration if default network or any CUDN transport is set to NoOverlay, deletes it if not
// - Default network RA if transport is set to NoOverlay, deletes it if not
// - CUDN RA if any CUDN transport is set to NoOverlay, deletes it if none are NoOverlay
func (c *Controller) ensureManagedConfiguration(_ string) error {
	cdnManaged := isCDNManaged()
	managedCUDNs, err := c.getManagedCUDNs()
	if err != nil {
		return fmt.Errorf("failed to get managed CUDNs: %w", err)
	}

	hasManaged := cdnManaged || len(managedCUDNs) > 0

	// Handle FRRConfiguration
	if hasManaged {
		if err := c.ensureBaseFRRConfiguration(); err != nil {
			return fmt.Errorf("failed to ensure base FRRConfiguration: %w", err)
		}
	} else {
		if err := c.cleanupBaseFRRConfigurations(); err != nil {
			return fmt.Errorf("failed to cleanup base FRRConfiguration: %w", err)
		}
	}

	// Handle CDN RouteAdvertisement
	if cdnManaged {
		if err := c.ensureManagedRouteAdvertisement(true); err != nil {
			return fmt.Errorf("failed to ensure CDN RouteAdvertisement: %w", err)
		}
	} else {
		if err := c.deleteManagedRA(true); err != nil {
			return fmt.Errorf("failed to delete CDN RouteAdvertisement: %w", err)
		}
	}

	// Handle CUDN RouteAdvertisement
	if len(managedCUDNs) > 0 {
		if err := c.ensureManagedRouteAdvertisement(false); err != nil {
			return fmt.Errorf("failed to ensure CUDN RouteAdvertisement: %w", err)
		}
	} else {
		if err := c.deleteManagedRA(false); err != nil {
			return fmt.Errorf("failed to delete CUDN RouteAdvertisement: %w", err)
		}
	}

	return nil
}

func (c *Controller) ensureBaseFRRConfiguration() error {
	if config.ManagedBGP.Topology != config.ManagedBGPTopologyFullMesh {
		return fmt.Errorf("unsupported managed BGP topology %q, only %q is supported", config.ManagedBGP.Topology, config.ManagedBGPTopologyFullMesh)
	}

	allNodes, err := c.wf.GetNodes()
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}
	neighbors := []frrtypes.Neighbor{}
	for _, node := range allNodes {
		v4, v6 := util.GetNodeInternalAddrs(node)
		if v4 != nil {
			neighbors = append(neighbors, frrtypes.Neighbor{
				Address: v4.String(),
				ASN:     config.ManagedBGP.ASNumber,
			})
		}
		if v6 != nil {
			neighbors = append(neighbors, frrtypes.Neighbor{
				Address: v6.String(),
				ASN:     config.ManagedBGP.ASNumber,
			})
		}
	}

	sort.Slice(neighbors, func(i, j int) bool {
		return neighbors[i].Address < neighbors[j].Address
	})

	desiredSpec := frrtypes.FRRConfigurationSpec{
		// Empty NodeSelector means it applies as a base for all nodes by RouteAdvertisements controller
		NodeSelector: metav1.LabelSelector{},
		BGP: frrtypes.BGPConfig{
			Routers: []frrtypes.Router{
				{
					ASN:       config.ManagedBGP.ASNumber,
					Neighbors: neighbors,
				},
			},
		},
	}

	frrConfigName := managedNamePrefix
	existing, err := c.frrLister.FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(frrConfigName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		frrConfig := &frrtypes.FRRConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      frrConfigName,
				Namespace: config.ManagedBGP.FRRNamespace,
				Labels: map[string]string{
					managedNetworkLabel: "",
				},
			},
			Spec: desiredSpec,
		}
		klog.Infof("Creating base FRRConfiguration")
		_, err = c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Create(context.TODO(), frrConfig, metav1.CreateOptions{FieldManager: fieldManager})
		if err != nil {
			return fmt.Errorf("failed to create base FRRConfiguration: %w", err)
		}
	} else {
		_, hasLabel := existing.Labels[managedNetworkLabel]
		needsUpdate := !reflect.DeepEqual(existing.Spec, desiredSpec) || !hasLabel
		if needsUpdate {
			klog.Infof("Updating base FRRConfiguration %s", existing.Name)
			updated := existing.DeepCopy()
			if updated.Labels == nil {
				updated.Labels = map[string]string{}
			}
			updated.Labels[managedNetworkLabel] = ""
			updated.Spec = desiredSpec
			_, err = c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Update(context.TODO(), updated, metav1.UpdateOptions{FieldManager: fieldManager})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// ensureManagedNetworkLabel sets managedNetworkLabel="" on the CUDN so that
// the shared managed RA's ClusterUserDefinedNetworkSelector can match all managed CUDNs.
// All managed CUDNs get the same label with empty value for unified selection.
func (c *Controller) ensureManagedNetworkLabel(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) error {
	if val, exists := cudn.Labels[managedNetworkLabel]; exists && val == "" {
		return nil
	}
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, managedNetworkLabel, "")
	_, err := c.cudnClient.K8sV1().ClusterUserDefinedNetworks().Patch(
		context.TODO(), cudn.Name, k8stypes.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: fieldManager})
	if err != nil {
		return fmt.Errorf("failed to set %s label on CUDN %s: %w", managedNetworkLabel, cudn.Name, err)
	}
	klog.Infof("Set %s=\"\" label on CUDN %s", managedNetworkLabel, cudn.Name)
	return nil
}

func (c *Controller) ensureManagedRouteAdvertisement(isCDN bool) error {
	var raName string
	var netSelector apitypes.NetworkSelectors
	var networkType string

	if isCDN {
		raName = managedRAName(types.DefaultNetworkName)
		networkType = "CDN"
		netSelector = apitypes.NetworkSelectors{{
			NetworkSelectionType: apitypes.DefaultNetwork,
		}}
	} else {
		raName = managedRAName("cudn")
		networkType = "CUDN"
		netSelector = apitypes.NetworkSelectors{{
			NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
			ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
				NetworkSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						managedNetworkLabel: "",
					},
				},
			},
		}}
	}

	ra := &ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: raName,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			NetworkSelectors: netSelector,
			Advertisements: []ratypes.AdvertisementType{
				ratypes.PodNetwork,
			},
			FRRConfigurationSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					managedNetworkLabel: "",
				},
			},
			NodeSelector: metav1.LabelSelector{},
		},
	}

	existing, err := c.raLister.Get(raName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		klog.Infof("Creating managed RouteAdvertisement %s for %s", raName, networkType)
		_, err = c.raClient.K8sV1().RouteAdvertisements().Create(
			context.TODO(), ra, metav1.CreateOptions{FieldManager: fieldManager})
		if err != nil {
			return err
		}
		return nil
	}

	needsUpdate := !reflect.DeepEqual(existing.Spec, ra.Spec)
	if needsUpdate {
		klog.Infof("Updating managed RouteAdvertisement %s for %s", raName, networkType)
		updated := existing.DeepCopy()
		updated.Spec = ra.Spec
		_, err = c.raClient.K8sV1().RouteAdvertisements().Update(
			context.TODO(), updated, metav1.UpdateOptions{FieldManager: fieldManager})
		return err
	}

	return nil
}

func (c *Controller) deleteManagedRA(isCDN bool) error {
	var raName, networkType string
	if isCDN {
		raName = managedRAName(types.DefaultNetworkName)
		networkType = "CDN"
	} else {
		raName = managedRAName("cudn")
		networkType = "CUDN"
	}

	klog.Infof("Deleting managed RouteAdvertisement %s for %s", raName, networkType)
	err := c.raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), raName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete managed RouteAdvertisement %s for %s: %w", raName, networkType, err)
	}
	return nil
}

// cleanupBaseFRRConfigurations deletes the managed base FRRConfiguration.
func (c *Controller) cleanupBaseFRRConfigurations() error {
	klog.Infof("Deleting managed base FRRConfiguration %s", managedNamePrefix)
	if err := c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Delete(
		context.TODO(), managedNamePrefix, metav1.DeleteOptions{},
	); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete base FRRConfiguration %s: %w", managedNamePrefix, err)
	}
	return nil
}

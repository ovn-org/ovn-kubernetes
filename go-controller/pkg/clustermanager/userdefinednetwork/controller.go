package userdefinednetwork

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	corev1informer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netv1clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	netv1infomer "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	netv1lister "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnapplyconfkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/applyconfiguration/userdefinednetwork/v1"
	userdefinednetworkclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	userdefinednetworkscheme "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/scheme"
	userdefinednetworkinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/informers/externalversions/userdefinednetwork/v1"
	userdefinednetworklister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/notifier"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const conditionTypeNetworkCreated = "NetworkCreated"

type RenderNetAttachDefManifest func(obj client.Object, targetNamespace string) (*netv1.NetworkAttachmentDefinition, error)

type networkInUseError struct {
	err error
}

func (n *networkInUseError) Error() string {
	return n.err.Error()
}

type Controller struct {
	// cudnController manage ClusterUserDefinedNetwork CRs.
	cudnController controller.Controller
	// udnController manage UserDefinedNetwork CRs.
	udnController controller.Controller
	// nadNotifier notifies subscribing controllers about NetworkAttachmentDefinition events.
	nadNotifier *notifier.NetAttachDefNotifier
	// namespaceInformer notifies subscribing controllers about Namespace events.
	namespaceNotifier *notifier.NamespaceNotifier
	// namespaceTracker tracks each CUDN CRs affected namespaces, enable finding stale NADs.
	// Keys are CR name, value is affected namespace names slice.
	namespaceTracker     map[string]sets.Set[string]
	namespaceTrackerLock sync.RWMutex
	// renderNadFn render NAD manifest from given object, enable replacing in tests.
	renderNadFn RenderNetAttachDefManifest
	// createNetworkLock lock should be held when NAD is created to avoid having two components
	// trying to create an object with the same name.
	createNetworkLock sync.Mutex

	udnClient         userdefinednetworkclientset.Interface
	udnLister         userdefinednetworklister.UserDefinedNetworkLister
	cudnLister        userdefinednetworklister.ClusterUserDefinedNetworkLister
	nadClient         netv1clientset.Interface
	nadLister         netv1lister.NetworkAttachmentDefinitionLister
	podInformer       corev1informer.PodInformer
	namespaceInformer corev1informer.NamespaceInformer

	networkInUseRequeueInterval time.Duration
	eventRecorder               record.EventRecorder
}

const defaultNetworkInUseCheckInterval = 1 * time.Minute

func New(
	nadClient netv1clientset.Interface,
	nadInfomer netv1infomer.NetworkAttachmentDefinitionInformer,
	udnClient userdefinednetworkclientset.Interface,
	udnInformer userdefinednetworkinformer.UserDefinedNetworkInformer,
	cudnInformer userdefinednetworkinformer.ClusterUserDefinedNetworkInformer,
	renderNadFn RenderNetAttachDefManifest,
	podInformer corev1informer.PodInformer,
	namespaceInformer corev1informer.NamespaceInformer,
	eventRecorder record.EventRecorder,
) *Controller {
	udnLister := udnInformer.Lister()
	cudnLister := cudnInformer.Lister()
	c := &Controller{
		nadClient:                   nadClient,
		nadLister:                   nadInfomer.Lister(),
		udnClient:                   udnClient,
		udnLister:                   udnLister,
		cudnLister:                  cudnLister,
		renderNadFn:                 renderNadFn,
		podInformer:                 podInformer,
		namespaceInformer:           namespaceInformer,
		networkInUseRequeueInterval: defaultNetworkInUseCheckInterval,
		namespaceTracker:            map[string]sets.Set[string]{},
		eventRecorder:               eventRecorder,
	}
	udnCfg := &controller.ControllerConfig[userdefinednetworkv1.UserDefinedNetwork]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileUDN,
		ObjNeedsUpdate: c.udnNeedUpdate,
		Threadiness:    1,
		Informer:       udnInformer.Informer(),
		Lister:         udnLister.List,
	}
	c.udnController = controller.NewController[userdefinednetworkv1.UserDefinedNetwork]("user-defined-network-controller", udnCfg)

	cudnCfg := &controller.ControllerConfig[userdefinednetworkv1.ClusterUserDefinedNetwork]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileCUDN,
		ObjNeedsUpdate: c.cudnNeedUpdate,
		Threadiness:    1,
		Informer:       cudnInformer.Informer(),
		Lister:         cudnLister.List,
	}
	c.cudnController = controller.NewController[userdefinednetworkv1.ClusterUserDefinedNetwork]("cluster-user-defined-network-controller", cudnCfg)

	c.nadNotifier = notifier.NewNetAttachDefNotifier(nadInfomer, c)
	c.namespaceNotifier = notifier.NewNamespaceNotifier(namespaceInformer, c)

	return c
}

func (c *Controller) Run() error {
	klog.Infof("Starting user-defined network controllers")
	if err := controller.StartWithInitialSync(
		c.initializeNamespaceTracker,
		c.cudnController,
		c.udnController,
		c.nadNotifier.Controller,
		c.namespaceNotifier.Controller,
	); err != nil {
		return fmt.Errorf("unable to start user-defined network controller: %v", err)
	}

	return nil
}

// initializeNamespaceTracker populates the namespace-tracker with NAD namespaces who owned by the controller.
func (c *Controller) initializeNamespaceTracker() error {
	cudns, err := c.cudnLister.List(labels.Everything())
	if err != nil {
		return err
	}
	if len(cudns) == 0 {
		return nil
	}

	nads, err := c.nadLister.List(labels.Everything())
	if err != nil {
		return err
	}
	if len(nads) == 0 {
		return nil
	}
	indexedNADs := map[string]netv1.NetworkAttachmentDefinition{}
	for _, nad := range nads {
		if nad != nil {
			indexedNADs[nad.Namespace+"/"+nad.Name] = *nad.DeepCopy()
		}
	}

	for _, cudn := range cudns {
		c.namespaceTracker[cudn.Name] = sets.New[string]()

		for nadKey, nad := range indexedNADs {
			if !metav1.IsControlledBy(&nad, cudn) {
				continue
			}
			c.namespaceTracker[cudn.Name].Insert(nad.Namespace)

			// Usually we don't want to mutate an iterated map, in this case
			// the processed entry is removed because it shouldn't be processed
			// again and not expected to be visited again, i.e.: the NAD should
			// be recorded by the namespaceTracker once.
			delete(indexedNADs, nadKey)
		}
	}

	return nil
}

func (c *Controller) Shutdown() {
	controller.Stop(
		c.cudnController,
		c.udnController,
		c.nadNotifier.Controller,
		c.namespaceNotifier.Controller,
	)
}

// ReconcileNetAttachDef enqueue NAD requests following NAD events.
func (c *Controller) ReconcileNetAttachDef(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split meta namespace key %q: %v", key, err)
	}
	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get NetworkAttachmentDefinition %q from cache: %v", key, err)
	}
	ownerRef := metav1.GetControllerOf(nad)
	if ownerRef == nil {
		return nil
	}

	switch ownerRef.Kind {
	case "ClusterUserDefinedNetwork":
		owner, err := c.cudnLister.Get(ownerRef.Name)
		if err != nil {
			return fmt.Errorf("failed to get ClusterUserDefinedNetwork %q from cache: %v", ownerRef.Name, err)
		}
		ownerKey, err := cache.MetaNamespaceKeyFunc(owner)
		if err != nil {
			return fmt.Errorf("failed to generate meta namespace key for CUDN: %v", err)
		}
		c.cudnController.Reconcile(ownerKey)
	case "UserDefinedNetwork":
		owner, err := c.udnLister.UserDefinedNetworks(nad.Namespace).Get(ownerRef.Name)
		if err != nil {
			return fmt.Errorf("failed to get UserDefinedNetwork %q from cache: %v", ownerRef.Name, err)
		}
		ownerKey, err := cache.MetaNamespaceKeyFunc(owner)
		if err != nil {
			return fmt.Errorf("failed to generate meta namespace key for UDN: %v", err)
		}
		c.udnController.Reconcile(ownerKey)
	default:
		return nil
	}
	return nil
}

// ReconcileNamespace enqueue relevant Cluster UDN CR requests following namespace events.
func (c *Controller) ReconcileNamespace(key string) error {
	c.namespaceTrackerLock.RLock()
	defer c.namespaceTrackerLock.RUnlock()

	namespace, err := c.namespaceInformer.Lister().Get(key)
	if err != nil {
		return fmt.Errorf("failed to get namespace %q from cahce: %w", key, err)
	}
	namespaceLabels := labels.Set(namespace.Labels)

	for cudnName, affectedNamespaces := range c.namespaceTracker {
		affectedNamespace := affectedNamespaces.Has(key)

		selectedNamespace := false

		if !affectedNamespace {
			cudn, err := c.cudnLister.Get(cudnName)
			if err != nil {
				return fmt.Errorf("faild to get CUDN %q from cache: %w", cudnName, err)
			}
			cudnSelector, err := metav1.LabelSelectorAsSelector(&cudn.Spec.NamespaceSelector)
			if err != nil {
				return fmt.Errorf("failed to convert CUDN namespace selector: %w", err)
			}
			selectedNamespace = cudnSelector.Matches(namespaceLabels)
		}

		if affectedNamespace || selectedNamespace {
			klog.Infof("Enqueue ClusterUDN %q following namespace %q event", cudnName, key)
			c.cudnController.Reconcile(cudnName)
		}
	}

	return nil
}

// UpdateSubsystemCondition may be used by other controllers handling UDN/NAD/network setup to report conditions that
// may affect UDN functionality.
// FieldManager should be unique for every subsystem.
// If given network is not managed by a UDN, no condition will be reported and no error will be returned.
// Events may be used to report additional information about the condition to avoid overloading the condition message.
// When condition should not change, but new events should be reported, pass condition = nil.
func (c *Controller) UpdateSubsystemCondition(
	networkName string,
	fieldManager string,
	condition *metav1.Condition,
	events ...*util.EventDetails,
) error {
	// try to find udn using network name
	udnNamespace, udnName := template.ParseNetworkName(networkName)
	if udnName == "" {
		return nil
	}
	udn, err := c.udnLister.UserDefinedNetworks(udnNamespace).Get(udnName)
	if err != nil {
		return nil
	}

	udnRef, err := reference.GetReference(userdefinednetworkscheme.Scheme, udn)
	if err != nil {
		return fmt.Errorf("failed to get object reference for UserDefinedNetwork %s/%s: %w", udnNamespace, udnName, err)
	}
	for _, event := range events {
		c.eventRecorder.Event(udnRef, event.EventType, event.Reason, event.Note)
	}

	if condition == nil {
		return nil
	}

	applyCondition := &metaapplyv1.ConditionApplyConfiguration{
		Type:               &condition.Type,
		Status:             &condition.Status,
		LastTransitionTime: &condition.LastTransitionTime,
		Reason:             &condition.Reason,
		Message:            &condition.Message,
	}

	udnStatus := udnapplyconfkv1.UserDefinedNetworkStatus().WithConditions(applyCondition)

	applyUDN := udnapplyconfkv1.UserDefinedNetwork(udnName, udnNamespace).WithStatus(udnStatus)
	opts := metav1.ApplyOptions{
		FieldManager: fieldManager,
		Force:        true,
	}
	_, err = c.udnClient.K8sV1().UserDefinedNetworks(udnNamespace).ApplyStatus(context.Background(), applyUDN, opts)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to update UserDefinedNetwork %s/%s status: %w", udnNamespace, udnName, err)
	}
	return nil
}

func (c *Controller) udnNeedUpdate(_, _ *userdefinednetworkv1.UserDefinedNetwork) bool {
	return true
}

// reconcileUDN get UserDefinedNetwork CR key and reconcile it according to spec.
// It creates NAD according to spec at the namespace the CR resides.
// The NAD objects are created with the same key as the request CR, having both kinds have the same key enable
// the controller to act on NAD changes as well and reconciles NAD objects (e.g: in case NAD is deleted it will be re-created).
func (c *Controller) reconcileUDN(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	udn, err := c.udnLister.UserDefinedNetworks(namespace).Get(name)
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("failed to get UserDefinedNetwork %q from cache: %v", key, err)
	}

	udnCopy := udn.DeepCopy()

	nadCopy, syncErr := c.syncUserDefinedNetwork(udnCopy)

	updateStatusErr := c.updateUserDefinedNetworkStatus(udnCopy, nadCopy, syncErr)

	var networkInUse *networkInUseError
	if errors.As(syncErr, &networkInUse) {
		c.udnController.ReconcileAfter(key, c.networkInUseRequeueInterval)
		return updateStatusErr
	}

	return errors.Join(syncErr, updateStatusErr)
}

func (c *Controller) syncUserDefinedNetwork(udn *userdefinednetworkv1.UserDefinedNetwork) (*netv1.NetworkAttachmentDefinition, error) {
	if udn == nil {
		return nil, nil
	}

	if !udn.DeletionTimestamp.IsZero() { // udn is being  deleted
		if controllerutil.ContainsFinalizer(udn, template.FinalizerUserDefinedNetwork) {
			if err := c.deleteNAD(udn, udn.Namespace); err != nil {
				return nil, fmt.Errorf("failed to delete NetworkAttachmentDefinition [%s/%s]: %w", udn.Namespace, udn.Name, err)
			}

			controllerutil.RemoveFinalizer(udn, template.FinalizerUserDefinedNetwork)
			udn, err := c.udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Update(context.Background(), udn, metav1.UpdateOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to remove finalizer to UserDefinedNetwork: %w", err)
			}
			klog.Infof("Finalizer removed from UserDefinedNetworks [%s/%s]", udn.Namespace, udn.Name)
		}

		return nil, nil
	}

	if finalizerAdded := controllerutil.AddFinalizer(udn, template.FinalizerUserDefinedNetwork); finalizerAdded {
		udn, err := c.udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Update(context.Background(), udn, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to add finalizer to UserDefinedNetwork: %w", err)
		}
		klog.Infof("Added Finalizer to UserDefinedNetwork [%s/%s]", udn.Namespace, udn.Name)
	}

	return c.updateNAD(udn, udn.Namespace)
}

func (c *Controller) updateUserDefinedNetworkStatus(udn *userdefinednetworkv1.UserDefinedNetwork, nad *netv1.NetworkAttachmentDefinition, syncError error) error {
	if udn == nil {
		return nil
	}

	networkCreatedCondition := newNetworkCreatedCondition(nad, syncError)

	conditions, updated := updateCondition(udn.Status.Conditions, networkCreatedCondition)

	if updated {
		var err error
		conditionsApply := make([]*metaapplyv1.ConditionApplyConfiguration, len(conditions))
		for i := range conditions {
			conditionsApply[i] = &metaapplyv1.ConditionApplyConfiguration{
				Type:               &conditions[i].Type,
				Status:             &conditions[i].Status,
				LastTransitionTime: &conditions[i].LastTransitionTime,
				Reason:             &conditions[i].Reason,
				Message:            &conditions[i].Message,
			}
		}
		udnApplyConf := udnapplyconfkv1.UserDefinedNetwork(udn.Name, udn.Namespace).
			WithStatus(udnapplyconfkv1.UserDefinedNetworkStatus().
				WithConditions(conditionsApply...))
		opts := metav1.ApplyOptions{FieldManager: "user-defined-network-controller"}
		udn, err = c.udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).ApplyStatus(context.Background(), udnApplyConf, opts)
		if err != nil {
			if kerrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("failed to update UserDefinedNetwork status: %w", err)
		}
		klog.Infof("Updated status UserDefinedNetwork [%s/%s]", udn.Namespace, udn.Name)
	}

	return nil
}

func newNetworkCreatedCondition(nad *netv1.NetworkAttachmentDefinition, syncError error) *metav1.Condition {
	now := metav1.Now()
	networkCreatedCondition := &metav1.Condition{
		Type:               conditionTypeNetworkCreated,
		Status:             metav1.ConditionTrue,
		Reason:             "NetworkAttachmentDefinitionCreated",
		Message:            "NetworkAttachmentDefinition has been created",
		LastTransitionTime: now,
	}

	if nad != nil && !nad.DeletionTimestamp.IsZero() {
		networkCreatedCondition.Status = metav1.ConditionFalse
		networkCreatedCondition.Reason = "NetworkAttachmentDefinitionDeleted"
		networkCreatedCondition.Message = "NetworkAttachmentDefinition is being deleted"
	}
	if syncError != nil {
		networkCreatedCondition.Status = metav1.ConditionFalse
		networkCreatedCondition.Reason = "SyncError"
		networkCreatedCondition.Message = syncError.Error()
	}

	return networkCreatedCondition
}

func updateCondition(conditions []metav1.Condition, cond *metav1.Condition) ([]metav1.Condition, bool) {
	if len(conditions) == 0 {
		return append(conditions, *cond), true
	}

	idx := slices.IndexFunc(conditions, func(c metav1.Condition) bool {
		return (c.Type == cond.Type) &&
			(c.Status != cond.Status || c.Reason != cond.Reason || c.Message != cond.Message)
	})
	if idx != -1 {
		return slices.Replace(conditions, idx, idx+1, *cond), true
	}
	return conditions, false
}

func (c *Controller) cudnNeedUpdate(_ *userdefinednetworkv1.ClusterUserDefinedNetwork, _ *userdefinednetworkv1.ClusterUserDefinedNetwork) bool {
	return true
}

// reconcileUDN get ClusterUserDefinedNetwork CR key and reconcile it according to spec.
// It creates NADs according to spec at the spesified selected namespaces.
// The NAD objects are created with the same key as the request CR, having both kinds have the same key enable
// the controller to act on NAD changes as well and reconciles NAD objects (e.g: in case NAD is deleted it will be re-created).
func (c *Controller) reconcileCUDN(key string) error {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	cudn, err := c.cudnLister.Get(name)
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("failed to get ClusterUserDefinedNetwork %q from cache: %v", key, err)
	}

	cudnCopy := cudn.DeepCopy()

	nads, syncErr := c.syncClusterUDN(cudnCopy)

	updateStatusErr := c.updateClusterUDNStatus(cudnCopy, nads, syncErr)

	var networkInUse *networkInUseError
	if errors.As(syncErr, &networkInUse) {
		c.cudnController.ReconcileAfter(key, c.networkInUseRequeueInterval)
		return updateStatusErr
	}

	return errors.Join(syncErr, updateStatusErr)
}

func (c *Controller) syncClusterUDN(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) ([]netv1.NetworkAttachmentDefinition, error) {
	c.namespaceTrackerLock.Lock()
	defer c.namespaceTrackerLock.Unlock()

	if cudn == nil {
		return nil, nil
	}

	cudnName := cudn.Name
	affectedNamespaces := c.namespaceTracker[cudnName]

	if !cudn.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(cudn, template.FinalizerUserDefinedNetwork) {
			var errs []error
			for nsToDelete := range affectedNamespaces {
				if err := c.deleteNAD(cudn, nsToDelete); err != nil {
					errs = append(errs, fmt.Errorf("failed to delete NetworkAttachmentDefinition [%s/%s]: %w",
						nsToDelete, cudnName, err))
				} else {
					c.namespaceTracker[cudnName].Delete(nsToDelete)
				}
			}

			if len(errs) > 0 {
				return nil, errors.Join(errs...)
			}

			var err error
			controllerutil.RemoveFinalizer(cudn, template.FinalizerUserDefinedNetwork)
			cudn, err = c.udnClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudn, metav1.UpdateOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to remove finalizer from ClusterUserDefinedNetwork %q: %w",
					cudnName, err)
			}
			klog.Infof("Finalizer removed from ClusterUserDefinedNetwork %q", cudn.Name)
			delete(c.namespaceTracker, cudnName)
		}

		return nil, nil
	}

	if _, exist := c.namespaceTracker[cudnName]; !exist {
		// start tracking CR
		c.namespaceTracker[cudnName] = sets.Set[string]{}
	}

	if finalizerAdded := controllerutil.AddFinalizer(cudn, template.FinalizerUserDefinedNetwork); finalizerAdded {
		var err error
		cudn, err = c.udnClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudn, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to add finalizer to ClusterUserDefinedNetwork %q: %w", cudnName, err)
		}
		klog.Infof("Added Finalizer to ClusterUserDefinedNetwork %q", cudnName)
	}

	selectedNamespaces, err := c.getSelectedNamespaces(cudn.Spec.NamespaceSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to get selected namespaces: %w", err)
	}

	var errs []error
	for nsToDelete := range affectedNamespaces.Difference(selectedNamespaces) {
		if err := c.deleteNAD(cudn, nsToDelete); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete NetworkAttachmentDefinition [%s/%s]: %w",
				nsToDelete, cudnName, err))
		} else {
			c.namespaceTracker[cudnName].Delete(nsToDelete)
		}
	}

	var nads []netv1.NetworkAttachmentDefinition
	for nsToUpdate := range selectedNamespaces {
		nad, err := c.updateNAD(cudn, nsToUpdate)
		if err != nil {
			errs = append(errs, err)
		} else {
			c.namespaceTracker[cudn.Name].Insert(nsToUpdate)
			nads = append(nads, *nad)
		}
	}

	return nads, errors.Join(errs...)
}

// getSelectedNamespaces list all selected namespaces according to given selector and create
// a set of the selected namespaces keys.
func (c *Controller) getSelectedNamespaces(sel metav1.LabelSelector) (sets.Set[string], error) {
	selectedNamespaces := sets.Set[string]{}
	labelSelector, err := metav1.LabelSelectorAsSelector(&sel)
	if err != nil {
		return nil, fmt.Errorf("failed to create label-selector: %w", err)
	}
	selectedNamespacesList, err := c.namespaceInformer.Lister().List(labelSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}
	for _, selectedNs := range selectedNamespacesList {
		selectedNamespaces.Insert(selectedNs.Name)
	}
	return selectedNamespaces, nil
}

func (c *Controller) updateClusterUDNStatus(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, nads []netv1.NetworkAttachmentDefinition, syncError error) error {
	if cudn == nil {
		return nil
	}

	// sort NADs by namespace names to avoid redundant updated due to inconsistent ordering
	slices.SortFunc(nads, func(a, b netv1.NetworkAttachmentDefinition) int {
		return strings.Compare(a.Namespace, b.Namespace)
	})

	networkCreatedCondition := newClusterNetworCreatedCondition(nads, syncError)

	conditions, updated := updateCondition(cudn.Status.Conditions, networkCreatedCondition)
	if !updated {
		return nil
	}
	conditionsApply := make([]*metaapplyv1.ConditionApplyConfiguration, len(conditions))
	for i := range conditions {
		conditionsApply[i] = &metaapplyv1.ConditionApplyConfiguration{
			Type:               &conditions[i].Type,
			Status:             &conditions[i].Status,
			LastTransitionTime: &conditions[i].LastTransitionTime,
			Reason:             &conditions[i].Reason,
			Message:            &conditions[i].Message,
		}
	}
	var err error
	applyConf := udnapplyconfkv1.ClusterUserDefinedNetwork(cudn.Name).
		WithStatus(udnapplyconfkv1.ClusterUserDefinedNetworkStatus().
			WithConditions(conditionsApply...))
	opts := metav1.ApplyOptions{FieldManager: "user-defined-network-controller"}
	cudnName := cudn.Name
	cudn, err = c.udnClient.K8sV1().ClusterUserDefinedNetworks().ApplyStatus(context.Background(), applyConf, opts)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to update ClusterUserDefinedNetwork status %q: %w", cudnName, err)
	}
	klog.Infof("Updated status ClusterUserDefinedNetwork %q", cudn.Name)

	return nil
}

func newClusterNetworCreatedCondition(nads []netv1.NetworkAttachmentDefinition, syncError error) *metav1.Condition {
	var namespaces []string
	for _, nad := range nads {
		namespaces = append(namespaces, nad.Namespace)
	}
	affectedNamespaces := strings.Join(namespaces, ", ")

	now := metav1.Now()
	condition := &metav1.Condition{
		Type:               conditionTypeNetworkCreated,
		Status:             metav1.ConditionTrue,
		Reason:             "NetworkAttachmentDefinitionCreated",
		Message:            fmt.Sprintf("NetworkAttachmentDefinition has been created in following namespaces: [%s]", affectedNamespaces),
		LastTransitionTime: now,
	}

	var deletedNadKeys []string
	for _, nad := range nads {
		if nad.DeletionTimestamp != nil {
			deletedNadKeys = append(deletedNadKeys, nad.Namespace+"/"+nad.Name)
		}
	}
	if len(deletedNadKeys) > 0 {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "NetworkAttachmentDefinitionDeleted"
		condition.Message = fmt.Sprintf("NetworkAttachmentDefinition are being deleted: %v", deletedNadKeys)
	}

	if syncError != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "NetworkAttachmentDefinitionSyncError"
		condition.Message = syncError.Error()
	}

	return condition
}

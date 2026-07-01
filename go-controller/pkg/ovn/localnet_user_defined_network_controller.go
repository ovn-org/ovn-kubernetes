// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/pod"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	podcontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/pod"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics/recorders"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	lsm "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type LocalnetUserDefinedNetworkControllerEventHandler struct {
	baseHandler  baseNetworkControllerEventHandler
	watchFactory *factory.WatchFactory
	objType      reflect.Type
	oc           *LocalnetUserDefinedNetworkController
	syncFunc     func([]interface{}) error
}

func (h *LocalnetUserDefinedNetworkControllerEventHandler) FilterOutResource(obj interface{}) bool {
	return h.oc.FilterOutResource(h.objType, obj)
}

// FilterOutDeleteResource applies delete-specific filtering for the configured resource type.
func (h *LocalnetUserDefinedNetworkControllerEventHandler) FilterOutDeleteResource(obj interface{}) bool {
	return h.oc.FilterOutDeleteResource(h.objType, obj)
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *LocalnetUserDefinedNetworkControllerEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	return h.baseHandler.areResourcesEqual(h.objType, obj1, obj2)
}

// GetInternalCacheEntry returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (h *LocalnetUserDefinedNetworkControllerEventHandler) GetInternalCacheEntry(obj interface{}) interface{} {
	return h.oc.GetInternalCacheEntryForUserDefinedNetwork(h.objType, obj)
}

// GetResourceFromInformerCache returns the latest state of the object, given an object key and its type.
// from the informers cache.
func (h *LocalnetUserDefinedNetworkControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	return h.baseHandler.getResourceFromInformerCache(h.objType, h.watchFactory, key)
}

// RecordAddEvent records the add event on this given object.
func (h *LocalnetUserDefinedNetworkControllerEventHandler) RecordAddEvent(obj interface{}) {
	switch h.objType {
	case factory.MultiNetworkPolicyType:
		mnp := obj.(*mnpapi.MultiNetworkPolicy)
		klog.V(5).Infof("Recording add event on multinetwork policy %s/%s", mnp.Namespace, mnp.Name)
		recorders.GetConfigDurationRecorder().Start("multinetworkpolicy", mnp.Namespace, mnp.Name)
	}
}

// RecordUpdateEvent records the udpate event on this given object.
func (h *LocalnetUserDefinedNetworkControllerEventHandler) RecordUpdateEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordDeleteEvent records the delete event on this given object.
func (h *LocalnetUserDefinedNetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordSuccessEvent records the success event on this given object.
func (h *LocalnetUserDefinedNetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordErrorEvent records the error event on this given object.
func (h *LocalnetUserDefinedNetworkControllerEventHandler) RecordErrorEvent(_ interface{}, _ string, _ error) {
}

// IsResourceScheduled lets the retry framework process unscheduled UDN pods.
// Their reconcile path performs identity-based policy cleanup without trying
// to create an LSP; skipping them would silently discard cleanup failures.
func (h *LocalnetUserDefinedNetworkControllerEventHandler) IsResourceScheduled(obj interface{}) bool {
	if h.objType == factory.PodType {
		return true
	}
	return h.baseHandler.isResourceScheduled(h.objType, obj)
}

// AddResource adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
// Given an object to add and a boolean specifying if the function was executed from iterateRetryResources
func (h *LocalnetUserDefinedNetworkControllerEventHandler) AddResource(obj interface{}, _ bool) error {
	return h.oc.AddUserDefinedNetworkResourceCommon(h.objType, obj)
}

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (h *LocalnetUserDefinedNetworkControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	return h.oc.UpdateUserDefinedNetworkResourceCommon(h.objType, oldObj, newObj, inRetryCache)
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *LocalnetUserDefinedNetworkControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	return h.oc.DeleteUserDefinedNetworkResourceCommon(h.objType, obj, cachedObj)
}

func (h *LocalnetUserDefinedNetworkControllerEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.NamespaceType:
			syncFunc = h.oc.syncNamespaces

		case factory.MultiNetworkPolicyType:
			syncFunc = h.oc.syncMultiNetworkPolicies

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

// IsObjectInTerminalState returns true if the given object is a in terminal state.
// This is used now for pods that are either in a PodSucceeded or in a PodFailed state.
func (h *LocalnetUserDefinedNetworkControllerEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	return h.baseHandler.isObjectInTerminalState(h.objType, obj)
}

// LocalnetUserDefinedNetworkController is created for logical network infrastructure and policy
// for a localnet user-defined network
type LocalnetUserDefinedNetworkController struct {
	BaseLayer2UserDefinedNetworkController
}

// NewLocalnetUserDefinedNetworkController create a new OVN controller for the given localnet NAD
func NewLocalnetUserDefinedNetworkController(
	cnci *CommonNetworkControllerInfo,
	netInfo util.NetInfo,
	networkManager networkmanager.Interface,
	addressSetManager *addresssetmanager.AddressSetManager,
	nodeReconciler *nodecontroller.NodeController,
	podReconciler *podcontroller.Controller,
) *LocalnetUserDefinedNetworkController {

	stopChan := make(chan struct{})
	nodeAnnotationCache := nodecontroller.NewNodeAnnotationCache()
	if nodeReconciler != nil {
		nodeAnnotationCache = nodeReconciler.AnnotationCache()
	}

	ipv4Mode, ipv6Mode := netInfo.IPMode()
	addressSetFactory := addressset.NewOvnAddressSetFactory(cnci.nbClient, ipv4Mode, ipv6Mode)
	oc := &LocalnetUserDefinedNetworkController{
		BaseLayer2UserDefinedNetworkController{
			BaseUserDefinedNetworkController: BaseUserDefinedNetworkController{
				BaseNetworkController: BaseNetworkController{
					CommonNetworkControllerInfo:  *cnci,
					controllerName:               getNetworkControllerName(netInfo.GetNetworkName()),
					ReconcilableNetInfo:          util.NewReconcilableNetInfo(netInfo),
					lsManager:                    lsm.NewL2SwitchManager(),
					logicalPortCache:             NewPortCache(stopChan),
					namespaces:                   make(map[string]*namespaceInfo),
					namespacesMutex:              sync.Mutex{},
					addressSetFactory:            addressSetFactory,
					networkPolicies:              syncmap.NewSyncMap[*networkPolicy](),
					networkPolicyKeysByNamespace: syncmap.NewSyncMap[sets.Set[string]](),
					sharedNetpolPortGroups:       syncmap.NewSyncMap[*defaultDenyPortGroups](),
					stopChan:                     stopChan,
					wg:                           &sync.WaitGroup{},
					cancelableCtx:                util.NewCancelableContext(),
					networkManager:               networkManager,
					addressSetManager:            addressSetManager,
					nodeReconciler:               nodeReconciler,
					nodeAnnotationCache:          nodeAnnotationCache,
					podReconciler:                podReconciler,
				},
			},
		},
	}
	oc.podHandler = &oc.BaseUserDefinedNetworkController

	if oc.allocatesPodAnnotation() {
		oc.podAnnotationAllocator = pod.NewPodAnnotationAllocator(
			netInfo,
			cnci.watchFactory.PodCoreInformer().Lister(),
			cnci.kube,
			nil)
	}

	// disable multicast support for UDNs
	// TBD: changes needs to be made to support multicast in UDNs
	oc.multicastSupport = false

	oc.initRetryFramework()
	return oc
}

// Start starts the localnet UDN controller, handles all events and creates all needed logical entities
func (oc *LocalnetUserDefinedNetworkController) Start(_ context.Context) error {
	klog.Infof("Starting controller for UDN %s", oc.GetNetworkName())

	start := time.Now()
	defer func() {
		klog.Infof("Starting controller for UDN %s took %v", oc.GetNetworkName(), time.Since(start))
	}()

	if err := oc.init(); err != nil {
		return err
	}
	if err := oc.run(); err != nil {
		oc.DeregisterPodHandler()
		oc.DeregisterNodeHandler()
		return err
	}
	return nil
}

func (oc *LocalnetUserDefinedNetworkController) run() error {
	return oc.BaseLayer2UserDefinedNetworkController.run()
}

// Cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *LocalnetUserDefinedNetworkController) Cleanup() error {
	return oc.BaseLayer2UserDefinedNetworkController.cleanup()
}

func (oc *LocalnetUserDefinedNetworkController) init() error {
	switchName := oc.GetNetworkScopedSwitchName(types.OVNLocalnetSwitch)

	logicalSwitch, err := oc.initializeLogicalSwitch(switchName, oc.Subnets(), oc.ExcludeSubnets(), oc.ReservedSubnets(), "", "")
	if err != nil {
		return err
	}

	// Add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      oc.GetNetworkScopedName(types.OVNLocalnetPort),
		Addresses: []string{"unknown"},
		Type:      "localnet",
		Options:   oc.localnetPortNetworkNameOptions(),
	}
	intVlanID := int(oc.Vlan())
	if intVlanID != 0 {
		logicalSwitchPort.TagRequest = &intVlanID
	}

	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, logicalSwitch, &logicalSwitchPort)
	if err != nil {
		klog.Errorf("Failed to add logical port %+v to switch %s: %v", logicalSwitchPort, switchName, err)
		return err
	}

	return nil
}

func (oc *LocalnetUserDefinedNetworkController) Stop() {
	klog.Infof("Stoping controller for UDN %s", oc.GetNetworkName())
	oc.BaseLayer2UserDefinedNetworkController.stop()
}

func (oc *LocalnetUserDefinedNetworkController) Reconcile(netInfo util.NetInfo) error {
	return oc.BaseNetworkController.reconcile(
		netInfo,
		func(_ string) {},
	)
}

func (oc *LocalnetUserDefinedNetworkController) initRetryFramework() {
	// For secondary networks, we don't have to watch namespace events if
	// multi-network policy support is not enabled. We don't support
	// multi-network policy for IPAM-less secondary networks either.
	if util.IsMultiNetworkPoliciesSupportEnabled() {
		oc.retryNamespaces = oc.newRetryFramework(factory.NamespaceType)
		oc.retryMultiNetworkPolicies = oc.newRetryFramework(factory.MultiNetworkPolicyType)
	}
}

// newRetryFramework builds and returns a retry framework for the input resource type;
func (oc *LocalnetUserDefinedNetworkController) newRetryFramework(
	objectType reflect.Type) *retry.RetryFramework {
	eventHandler := &LocalnetUserDefinedNetworkControllerEventHandler{
		baseHandler:  baseNetworkControllerEventHandler{},
		objType:      objectType,
		watchFactory: oc.watchFactory,
		oc:           oc,
		syncFunc:     nil,
	}
	resourceHandler := &retry.ResourceHandler{
		HasUpdateFunc:          hasResourceAnUpdateFunc(objectType),
		NeedsUpdateDuringRetry: needsUpdateDuringRetry(objectType),
		ObjType:                objectType,
		EventHandler:           eventHandler,
	}
	return retry.NewRetryFramework(
		oc.GetNetworkName()+"/networkController",
		oc.stopChan,
		oc.wg,
		oc.watchFactory,
		resourceHandler,
	)
}

func (oc *LocalnetUserDefinedNetworkController) localnetPortNetworkNameOptions() map[string]string {
	localnetLSPOptions := map[string]string{
		"network_name": oc.GetNetworkName(),
	}
	if oc.PhysicalNetworkName() != "" {
		localnetLSPOptions["network_name"] = oc.PhysicalNetworkName()
	}
	return localnetLSPOptions
}

package ovn

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	networkAttachDefController "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/persistentips"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type secondaryLocalnetNetworkControllerEventHandler struct {
	baseHandler  baseNetworkControllerEventHandler
	watchFactory *factory.WatchFactory
	objType      reflect.Type
	oc           *SecondaryLocalnetNetworkController
	syncFunc     func([]interface{}) error
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *secondaryLocalnetNetworkControllerEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	return h.baseHandler.areResourcesEqual(h.objType, obj1, obj2)
}

// GetInternalCacheEntry returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (h *secondaryLocalnetNetworkControllerEventHandler) GetInternalCacheEntry(obj interface{}) interface{} {
	return h.oc.GetInternalCacheEntryForSecondaryNetwork(h.objType, obj)
}

// GetResourceFromInformerCache returns the latest state of the object, given an object key and its type.
// from the informers cache.
func (h *secondaryLocalnetNetworkControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	return h.baseHandler.getResourceFromInformerCache(h.objType, h.watchFactory, key)
}

// RecordAddEvent records the add event on this given object.
func (h *secondaryLocalnetNetworkControllerEventHandler) RecordAddEvent(obj interface{}) {
	switch h.objType {
	case factory.MultiNetworkPolicyType:
		mnp := obj.(*mnpapi.MultiNetworkPolicy)
		klog.V(5).Infof("Recording add event on multinetwork policy %s/%s", mnp.Namespace, mnp.Name)
		metrics.GetConfigDurationRecorder().Start("multinetworkpolicy", mnp.Namespace, mnp.Name)
	}
}

// RecordUpdateEvent records the udpate event on this given object.
func (h *secondaryLocalnetNetworkControllerEventHandler) RecordUpdateEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordDeleteEvent records the delete event on this given object.
func (h *secondaryLocalnetNetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordSuccessEvent records the success event on this given object.
func (h *secondaryLocalnetNetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordErrorEvent records the error event on this given object.
func (h *secondaryLocalnetNetworkControllerEventHandler) RecordErrorEvent(obj interface{}, reason string, err error) {
}

// IsResourceScheduled returns true if the given object has been scheduled.
// Only applied to pods for now. Returns true for all other types.
func (h *secondaryLocalnetNetworkControllerEventHandler) IsResourceScheduled(obj interface{}) bool {
	return h.baseHandler.isResourceScheduled(h.objType, obj)
}

// AddResource adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
// Given an object to add and a boolean specifying if the function was executed from iterateRetryResources
func (h *secondaryLocalnetNetworkControllerEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	switch h.objType {
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to Node", obj)
		}
		return h.oc.addUpdateNodeEvent(node)
	default:
		return h.oc.AddSecondaryNetworkResourceCommon(h.objType, obj)
	}
}

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (h *secondaryLocalnetNetworkControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	switch h.objType {
	case factory.NodeType:
		node, ok := newObj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to Node", newObj)
		}
		return h.oc.addUpdateNodeEvent(node)
	default:
		return h.oc.UpdateSecondaryNetworkResourceCommon(h.objType, oldObj, newObj, inRetryCache)
	}
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *secondaryLocalnetNetworkControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	switch h.objType {
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to Node", obj)
		}
		return h.oc.deleteNodeEvent(node)
	default:
		return h.oc.DeleteSecondaryNetworkResourceCommon(h.objType, obj, cachedObj)
	}
}

func (h *secondaryLocalnetNetworkControllerEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.NodeType:
			syncFunc = h.oc.syncNodes

		case factory.PodType:
			syncFunc = h.oc.syncPodsForSecondaryNetwork

		case factory.NamespaceType:
			syncFunc = h.oc.syncNamespaces

		case factory.MultiNetworkPolicyType:
			syncFunc = h.oc.syncMultiNetworkPolicies

		case factory.IPAMClaimsType:
			syncFunc = h.oc.syncIPAMClaims

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
func (h *secondaryLocalnetNetworkControllerEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	return h.baseHandler.isObjectInTerminalState(h.objType, obj)
}

// SecondaryLocalnetNetworkController is created for logical network infrastructure and policy
// for a secondary localnet network
type SecondaryLocalnetNetworkController struct {
	BaseSecondaryLayer2NetworkController
}

// NewSecondaryLocalnetNetworkController create a new OVN controller for the given secondary localnet NAD
func NewSecondaryLocalnetNetworkController(cnci *CommonNetworkControllerInfo, netInfo util.NetInfo,
	nadController networkAttachDefController.NADController) *SecondaryLocalnetNetworkController {

	stopChan := make(chan struct{})

	ipv4Mode, ipv6Mode := netInfo.IPMode()
	addressSetFactory := addressset.NewOvnAddressSetFactory(cnci.nbClient, ipv4Mode, ipv6Mode)
	oc := &SecondaryLocalnetNetworkController{
		BaseSecondaryLayer2NetworkController{
			BaseSecondaryNetworkController: BaseSecondaryNetworkController{
				BaseNetworkController: BaseNetworkController{
					CommonNetworkControllerInfo: *cnci,
					controllerName:              getNetworkControllerName(netInfo.GetNetworkName()),
					NetInfo:                     netInfo,
					lsManager:                   lsm.NewL2SwitchManager(),
					logicalPortCache:            newPortCache(stopChan),
					namespaces:                  make(map[string]*namespaceInfo),
					namespacesMutex:             sync.Mutex{},
					addressSetFactory:           addressSetFactory,
					networkPolicies:             syncmap.NewSyncMap[*networkPolicy](),
					sharedNetpolPortGroups:      syncmap.NewSyncMap[*defaultDenyPortGroups](),
					podSelectorAddressSets:      syncmap.NewSyncMap[*PodSelectorAddressSet](),
					stopChan:                    stopChan,
					wg:                          &sync.WaitGroup{},
					cancelableCtx:               util.NewCancelableContext(),
					localZoneNodes:              &sync.Map{},
					nadController:               nadController,
				},
			},
		},
	}

	if oc.allocatesPodAnnotation() {
		var claimsReconciler persistentips.PersistentAllocations
		if oc.allowPersistentIPs() {
			ipamClaimsReconciler := persistentips.NewIPAMClaimReconciler(
				oc.kube,
				oc.NetInfo,
				oc.watchFactory.IPAMClaimsInformer().Lister(),
			)
			oc.ipamClaimsReconciler = ipamClaimsReconciler
			claimsReconciler = ipamClaimsReconciler
		}
		oc.podAnnotationAllocator = pod.NewPodAnnotationAllocator(
			netInfo,
			cnci.watchFactory.PodCoreInformer().Lister(),
			cnci.kube,
			claimsReconciler)
	}

	// disable multicast support for secondary networks
	// TBD: changes needs to be made to support multicast in secondary networks
	oc.multicastSupport = false

	oc.initRetryFramework()
	return oc
}

// Start starts the secondary localnet controller, handles all events and creates all needed logical entities
func (oc *SecondaryLocalnetNetworkController) Start(ctx context.Context) error {
	klog.Infof("Starting controller for secondary network network %s", oc.GetNetworkName())

	start := time.Now()
	defer func() {
		klog.Infof("Starting controller for secondary network network %s took %v", oc.GetNetworkName(), time.Since(start))
	}()

	if err := oc.Init(); err != nil {
		return err
	}

	return oc.run(ctx)
}

func (oc *SecondaryLocalnetNetworkController) run(ctx context.Context) error {
	return oc.BaseSecondaryLayer2NetworkController.run()
}

// Cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *SecondaryLocalnetNetworkController) Cleanup() error {
	return oc.BaseSecondaryLayer2NetworkController.cleanup()
}

func (oc *SecondaryLocalnetNetworkController) Init() error {
	switchName := oc.GetNetworkScopedSwitchName(types.OVNLocalnetSwitch)

	logicalSwitch, err := oc.initializeLogicalSwitch(switchName, oc.Subnets(), oc.ExcludeSubnets(), "", "")
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

func (oc *SecondaryLocalnetNetworkController) Stop() {
	klog.Infof("Stoping controller for secondary network %s", oc.GetNetworkName())
	oc.BaseSecondaryLayer2NetworkController.stop()
}

func (oc *SecondaryLocalnetNetworkController) initRetryFramework() {
	oc.retryNodes = oc.newRetryFramework(factory.NodeType)
	oc.retryPods = oc.newRetryFramework(factory.PodType)
	if oc.allocatesPodAnnotation() && oc.NetInfo.AllowsPersistentIPs() {
		oc.retryIPAMClaims = oc.newRetryFramework(factory.IPAMClaimsType)
	}

	// For secondary networks, we don't have to watch namespace events if
	// multi-network policy support is not enabled. We don't support
	// multi-network policy for IPAM-less secondary networks either.
	if util.IsMultiNetworkPoliciesSupportEnabled() {
		oc.retryNamespaces = oc.newRetryFramework(factory.NamespaceType)
		oc.retryMultiNetworkPolicies = oc.newRetryFramework(factory.MultiNetworkPolicyType)
	}
}

// newRetryFramework builds and returns a retry framework for the input resource type;
func (oc *SecondaryLocalnetNetworkController) newRetryFramework(
	objectType reflect.Type) *retry.RetryFramework {
	eventHandler := &secondaryLocalnetNetworkControllerEventHandler{
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
		oc.stopChan,
		oc.wg,
		oc.watchFactory,
		resourceHandler,
	)
}

func (oc *SecondaryLocalnetNetworkController) localnetPortNetworkNameOptions() map[string]string {
	localnetLSPOptions := map[string]string{
		"network_name": oc.GetNetworkName(),
	}
	if oc.PhysicalNetworkName() != "" {
		localnetLSPOptions["network_name"] = oc.PhysicalNetworkName()
	}
	return localnetLSPOptions
}

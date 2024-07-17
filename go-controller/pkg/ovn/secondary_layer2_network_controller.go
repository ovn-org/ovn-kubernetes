package ovn

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	zoneinterconnect "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/persistentips"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	corev1 "k8s.io/api/core/v1"
	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// method/structure shared by all layer 2 network controller, including localnet and layer2 network controllres.

type secondaryLayer2NetworkControllerEventHandler struct {
	baseHandler  baseNetworkControllerEventHandler
	watchFactory *factory.WatchFactory
	objType      reflect.Type
	oc           *SecondaryLayer2NetworkController
	syncFunc     func([]interface{}) error
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *secondaryLayer2NetworkControllerEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	return h.baseHandler.areResourcesEqual(h.objType, obj1, obj2)
}

// GetInternalCacheEntry returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (h *secondaryLayer2NetworkControllerEventHandler) GetInternalCacheEntry(obj interface{}) interface{} {
	return h.oc.GetInternalCacheEntryForSecondaryNetwork(h.objType, obj)
}

// GetResourceFromInformerCache returns the latest state of the object, given an object key and its type.
// from the informers cache.
func (h *secondaryLayer2NetworkControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	return h.baseHandler.getResourceFromInformerCache(h.objType, h.watchFactory, key)
}

// RecordAddEvent records the add event on this given object.
func (h *secondaryLayer2NetworkControllerEventHandler) RecordAddEvent(obj interface{}) {
	switch h.objType {
	case factory.MultiNetworkPolicyType:
		mnp := obj.(*mnpapi.MultiNetworkPolicy)
		klog.V(5).Infof("Recording add event on multinetwork policy %s/%s", mnp.Namespace, mnp.Name)
		metrics.GetConfigDurationRecorder().Start("multinetworkpolicy", mnp.Namespace, mnp.Name)
	}
}

// RecordUpdateEvent records the udpate event on this given object.
func (h *secondaryLayer2NetworkControllerEventHandler) RecordUpdateEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordDeleteEvent records the delete event on this given object.
func (h *secondaryLayer2NetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordSuccessEvent records the success event on this given object.
func (h *secondaryLayer2NetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordErrorEvent records the error event on this given object.
func (h *secondaryLayer2NetworkControllerEventHandler) RecordErrorEvent(obj interface{}, reason string, err error) {
}

// IsResourceScheduled returns true if the given object has been scheduled.
// Only applied to pods for now. Returns true for all other types.
func (h *secondaryLayer2NetworkControllerEventHandler) IsResourceScheduled(obj interface{}) bool {
	return h.baseHandler.isResourceScheduled(h.objType, obj)
}

// AddResource adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
// Given an object to add and a boolean specifying if the function was executed from iterateRetryResources
func (h *secondaryLayer2NetworkControllerEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	switch h.objType {
	case factory.NodeType:
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to Node", obj)
		}
		if h.oc.isLocalZoneNode(node) {
			var nodeParams *nodeSyncs
			if fromRetryLoop {
				_, syncMgmtPort := h.oc.mgmtPortFailed.Load(node.Name)
				nodeParams = &nodeSyncs{syncMgmtPort: syncMgmtPort}
			} else {
				nodeParams = &nodeSyncs{syncMgmtPort: true}
			}
			return h.oc.addUpdateLocalNodeEvent(node, nodeParams)
		}
		return h.oc.addUpdateRemoteNodeEvent(node)
	default:
		return h.oc.AddSecondaryNetworkResourceCommon(h.objType, obj)
	}
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *secondaryLayer2NetworkControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
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

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (h *secondaryLayer2NetworkControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	switch h.objType {
	case factory.NodeType:
		newNode, ok := newObj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to Node", newObj)
		}
		oldNode, ok := oldObj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast oldObj of type %T to *kapi.Node", oldObj)
		}
		newNodeIsLocalZoneNode := h.oc.isLocalZoneNode(newNode)
		nodeSubnetChanged := nodeSubnetChanged(oldNode, newNode)
		if newNodeIsLocalZoneNode {
			var nodeSyncsParam *nodeSyncs
			if h.oc.isLocalZoneNode(oldNode) {
				// determine what actually changed in this update
				syncMgmtPort := macAddressChanged(oldNode, newNode) || nodeSubnetChanged
				nodeSyncsParam = &nodeSyncs{syncMgmtPort: syncMgmtPort}
			} else {
				klog.Infof("Node %s moved from the remote zone %s to local zone %s.",
					newNode.Name, util.GetNodeZone(oldNode), util.GetNodeZone(newNode))
				// The node is now a local zone node. Trigger a full node sync.
				nodeSyncsParam = &nodeSyncs{syncMgmtPort: true}
			}

			return h.oc.addUpdateLocalNodeEvent(newNode, nodeSyncsParam)
		} else {
			return h.oc.addUpdateRemoteNodeEvent(newNode)
		}
	default:
		return h.oc.UpdateSecondaryNetworkResourceCommon(h.objType, oldObj, newObj, inRetryCache)
	}
}

func (h *secondaryLayer2NetworkControllerEventHandler) SyncFunc(objs []interface{}) error {
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
func (h *secondaryLayer2NetworkControllerEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	return h.baseHandler.isObjectInTerminalState(h.objType, obj)
}

// SecondaryLayer2NetworkController is created for logical network infrastructure and policy
// for a secondary layer2 network
type SecondaryLayer2NetworkController struct {
	BaseSecondaryLayer2NetworkController

	// Node-specific syncMaps used by node event handler
	mgmtPortFailed sync.Map
}

// NewSecondaryLayer2NetworkController create a new OVN controller for the given secondary layer2 nad
func NewSecondaryLayer2NetworkController(cnci *CommonNetworkControllerInfo, netInfo util.NetInfo) *SecondaryLayer2NetworkController {

	stopChan := make(chan struct{})

	ipv4Mode, ipv6Mode := netInfo.IPMode()
	addressSetFactory := addressset.NewOvnAddressSetFactory(cnci.nbClient, ipv4Mode, ipv6Mode)

	lsManagerFactoryFn := lsm.NewL2SwitchManager
	if netInfo.IsPrimaryNetwork() {
		lsManagerFactoryFn = lsm.NewL2SwitchManagerForUserDefinedPrimaryNetwork
	}
	oc := &SecondaryLayer2NetworkController{
		BaseSecondaryLayer2NetworkController: BaseSecondaryLayer2NetworkController{

			BaseSecondaryNetworkController: BaseSecondaryNetworkController{
				BaseNetworkController: BaseNetworkController{
					CommonNetworkControllerInfo: *cnci,
					controllerName:              getNetworkControllerName(netInfo.GetNetworkName()),
					NetInfo:                     netInfo,
					lsManager:                   lsManagerFactoryFn(),
					logicalPortCache:            newPortCache(stopChan),
					namespaces:                  make(map[string]*namespaceInfo),
					namespacesMutex:             sync.Mutex{},
					addressSetFactory:           addressSetFactory,
					networkPolicies:             syncmap.NewSyncMap[*networkPolicy](),
					sharedNetpolPortGroups:      syncmap.NewSyncMap[*defaultDenyPortGroups](),
					podSelectorAddressSets:      syncmap.NewSyncMap[*PodSelectorAddressSet](),
					stopChan:                    stopChan,
					wg:                          &sync.WaitGroup{},
					localZoneNodes:              &sync.Map{},
					cancelableCtx:               util.NewCancelableContext(),
				},
			},
		},
		mgmtPortFailed: sync.Map{},
	}

	if config.OVNKubernetesFeature.EnableInterconnect {
		oc.zoneICHandler = zoneinterconnect.NewZoneInterconnectHandler(oc.NetInfo, oc.nbClient, oc.sbClient, oc.watchFactory)
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

// Start starts the secondary layer2 controller, handles all events and creates all needed logical entities
func (oc *SecondaryLayer2NetworkController) Start(ctx context.Context) error {
	klog.Infof("Starting controller for secondary network %s", oc.GetNetworkName())

	start := time.Now()
	defer func() {
		klog.Infof("Starting controller for secondary network %s took %v", oc.GetNetworkName(), time.Since(start))
	}()

	if err := oc.BaseNetworkController.init(); err != nil {
		return err
	}

	if err := oc.Init(); err != nil {
		return err
	}

	return oc.run(ctx)
}

func (oc *SecondaryLayer2NetworkController) run(ctx context.Context) error {
	return oc.BaseSecondaryLayer2NetworkController.run()
}

// Cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *SecondaryLayer2NetworkController) Cleanup() error {
	return oc.BaseSecondaryLayer2NetworkController.cleanup()
}

func (oc *SecondaryLayer2NetworkController) Init() error {
	_, err := oc.initializeLogicalSwitch(oc.GetNetworkScopedSwitchName(types.OVNLayer2Switch), oc.Subnets(), oc.ExcludeSubnets())
	return err
}

func (oc *SecondaryLayer2NetworkController) Stop() {
	klog.Infof("Stoping controller for secondary network %s", oc.GetNetworkName())
	oc.BaseSecondaryLayer2NetworkController.stop()
}

func (oc *SecondaryLayer2NetworkController) initRetryFramework() {
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
		oc.retryNetworkPolicies = oc.newRetryFramework(factory.MultiNetworkPolicyType)
	}
}

// newRetryFramework builds and returns a retry framework for the input resource type;
func (oc *SecondaryLayer2NetworkController) newRetryFramework(
	objectType reflect.Type) *retry.RetryFramework {
	eventHandler := &secondaryLayer2NetworkControllerEventHandler{
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

func (oc *SecondaryLayer2NetworkController) addUpdateLocalNodeEvent(node *corev1.Node, nSyncs *nodeSyncs) error {
	var errs []error

	if util.IsNetworkSegmentationSupportEnabled() && oc.IsPrimaryNetwork() {
		if nSyncs.syncMgmtPort {
			// Layer 2 networks have a single, large subnet, that's the one
			// associated to the controller.  Take the management port IP from
			// there.
			subnets := oc.Subnets()
			hostSubnets := make([]*net.IPNet, 0, len(subnets))
			for _, subnet := range oc.Subnets() {
				hostSubnets = append(hostSubnets, subnet.CIDR)
			}
			if _, err := oc.syncNodeManagementPortNoRouteHostSubnets(node, oc.GetNetworkScopedSwitchName(types.OVNLayer2Switch), hostSubnets); err != nil {
				errs = append(errs, err)
				oc.mgmtPortFailed.Store(node.Name, true)
			} else {
				oc.mgmtPortFailed.Delete(node.Name)
			}
		}
	}

	errs = append(errs, oc.BaseSecondaryLayer2NetworkController.addUpdateLocalNodeEvent(node))

	err := utilerrors.Join(errs...)
	if err != nil {
		oc.recordNodeErrorEvent(node, err)
	}
	return err
}

func (oc *SecondaryLayer2NetworkController) deleteNodeEvent(node *corev1.Node) error {
	oc.localZoneNodes.Delete(node.Name)
	oc.mgmtPortFailed.Delete(node.Name)
	return nil
}

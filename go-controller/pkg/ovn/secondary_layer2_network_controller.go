package ovn

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	zoneinterconnect "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
)

// SecondaryLayer2NetworkController is created for logical network infrastructure and policy
// for a secondary layer2 network
type SecondaryLayer2NetworkController struct {
	BaseSecondaryLayer2NetworkController
}

// NewSecondaryLayer2NetworkController create a new OVN controller for the given secondary layer2 nad
func NewSecondaryLayer2NetworkController(cnci *CommonNetworkControllerInfo, netInfo util.NetInfo) *SecondaryLayer2NetworkController {

	stopChan := make(chan struct{})

	ipv4Mode, ipv6Mode := netInfo.IPMode()
	addressSetFactory := addressset.NewOvnAddressSetFactory(cnci.nbClient, ipv4Mode, ipv6Mode)

	oc := &SecondaryLayer2NetworkController{
		BaseSecondaryLayer2NetworkController{
			BaseSecondaryNetworkController: BaseSecondaryNetworkController{
				BaseNetworkController: BaseNetworkController{
					CommonNetworkControllerInfo: *cnci,
					controllerName:              netInfo.GetNetworkName() + "-network-controller",
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
					localZoneNodes:              &sync.Map{},
					cancelableCtx:               util.NewCancelableContext(),
				},
			},
		},
	}

	if config.OVNKubernetesFeature.EnableInterconnect {
		oc.zoneICHandler = zoneinterconnect.NewZoneInterconnectHandler(oc.NetInfo, oc.nbClient, oc.sbClient, oc.watchFactory)
	}

	if oc.allocatesPodAnnotation() {
		podAnnotationAllocator := pod.NewPodAnnotationAllocator(
			netInfo,
			cnci.watchFactory.PodCoreInformer().Lister(),
			cnci.kube)
		oc.podAnnotationAllocator = podAnnotationAllocator
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

	if err := oc.Init(); err != nil {
		return err
	}

	return oc.run(ctx)
}

func (oc *SecondaryLayer2NetworkController) run(ctx context.Context) error {
	err := oc.WatchNodes()
	if err != nil {
		return err
	}

	return oc.BaseSecondaryLayer2NetworkController.run()
}

// Cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *SecondaryLayer2NetworkController) Cleanup(netName string) error {
	klog.Infof("Delete OVN logical entities for network %s", netName)
	return oc.BaseSecondaryLayer2NetworkController.cleanup(types.Layer2Topology, netName)
}

func (oc *SecondaryLayer2NetworkController) Init() error {
	switchName := oc.GetNetworkScopedName(types.OVNLayer2Switch)

	_, err := oc.initializeLogicalSwitch(switchName, oc.Subnets(), oc.ExcludeSubnets())
	return err
}

func (oc *SecondaryLayer2NetworkController) Stop() {
	klog.Infof("Stoping controller for secondary network %s", oc.GetNetworkName())
	oc.BaseSecondaryLayer2NetworkController.stop()

	if oc.nodeHandler != nil {
		oc.watchFactory.RemoveNodeHandler(oc.nodeHandler)
	}
}

func (oc *SecondaryLayer2NetworkController) initRetryFramework() {
	oc.BaseSecondaryLayer2NetworkController.initRetryFramework()
	oc.retryNodes = oc.newRetryFramework(factory.NodeType)
}

// newRetryFramework builds and returns a retry framework for the input resource type;
func (oc *SecondaryLayer2NetworkController) newRetryFramework(objectType reflect.Type) *retry.RetryFramework {
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

func (oc *SecondaryLayer2NetworkController) addUpdateNodeEvent(node *corev1.Node) error {
	if oc.isLocalZoneNode(node) {
		return oc.addUpdateLocalNodeEvent(node)
	}
	return oc.addUpdateRemoteNodeEvent(node)
}

func (oc *SecondaryLayer2NetworkController) addUpdateLocalNodeEvent(node *corev1.Node) error {
	_, present := oc.localZoneNodes.LoadOrStore(node.Name, true)

	if !present {
		// process all pods so they are reconfigured as local
		errs := oc.addAllPodsOnNode(node.Name)
		if errs != nil {
			err := kerrors.NewAggregate(errs)
			return err
		}
	}

	return nil
}

func (oc *SecondaryLayer2NetworkController) addUpdateRemoteNodeEvent(node *corev1.Node) error {
	_, present := oc.localZoneNodes.Load(node.Name)

	if present {
		err := oc.deleteNodeEvent(node)
		if err != nil {
			return err
		}

		// process all pods so they are reconfigured as remote
		errs := oc.addAllPodsOnNode(node.Name)
		if errs != nil {
			err = kerrors.NewAggregate(errs)
			return err
		}
	}

	return nil
}

func (oc *SecondaryLayer2NetworkController) deleteNodeEvent(node *corev1.Node) error {
	oc.localZoneNodes.Delete(node.Name)
	return nil
}

type secondaryLayer2NetworkControllerEventHandler struct {
	baseHandler  baseNetworkControllerEventHandler
	watchFactory *factory.WatchFactory
	objType      reflect.Type
	oc           *SecondaryLayer2NetworkController
	syncFunc     func([]interface{}) error
}

func (h *secondaryLayer2NetworkControllerEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
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

func (h *secondaryLayer2NetworkControllerEventHandler) UpdateResource(oldObj interface{}, newObj interface{}, inRetryCache bool) error {
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

func (h *secondaryLayer2NetworkControllerEventHandler) DeleteResource(obj interface{}, cachedObj interface{}) error {
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

func (h *secondaryLayer2NetworkControllerEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.NodeType:
			// no need to sync anything
		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

func (h *secondaryLayer2NetworkControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	return h.baseHandler.getResourceFromInformerCache(h.objType, h.watchFactory, key)
}

func (h *secondaryLayer2NetworkControllerEventHandler) AreResourcesEqual(obj1 interface{}, obj2 interface{}) (bool, error) {
	return h.baseHandler.areResourcesEqual(h.objType, obj1, obj2)
}

func (h *secondaryLayer2NetworkControllerEventHandler) GetInternalCacheEntry(obj interface{}) interface{} {
	return h.oc.GetInternalCacheEntryForSecondaryNetwork(h.objType, obj)
}

func (h *secondaryLayer2NetworkControllerEventHandler) IsResourceScheduled(obj interface{}) bool {
	return h.baseHandler.isResourceScheduled(h.objType, obj)
}

func (h *secondaryLayer2NetworkControllerEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	return h.baseHandler.isObjectInTerminalState(h.objType, obj)
}

// functions related to metrics and events
func (h *secondaryLayer2NetworkControllerEventHandler) RecordAddEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

func (h *secondaryLayer2NetworkControllerEventHandler) RecordUpdateEvent(obj interface{}) {
	h.baseHandler.recordUpdateEvent(h.objType, obj)
}

func (h *secondaryLayer2NetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
	h.baseHandler.recordDeleteEvent(h.objType, obj)
}

func (h *secondaryLayer2NetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
	h.baseHandler.recordSuccessEvent(h.objType, obj)
}

func (h *secondaryLayer2NetworkControllerEventHandler) RecordErrorEvent(obj interface{}, reason string, err error) {
	h.baseHandler.recordSuccessEvent(h.objType, obj)
}

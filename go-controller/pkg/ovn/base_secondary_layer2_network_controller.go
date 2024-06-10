package ovn

import (
	"fmt"
	"net"
	"reflect"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta2"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// method/structure shared by all layer 2 network controller, including localnet and layer2 network controllres.

type baseSecondaryLayer2NetworkControllerEventHandler struct {
	baseHandler  baseNetworkControllerEventHandler
	watchFactory *factory.WatchFactory
	objType      reflect.Type
	oc           *BaseSecondaryLayer2NetworkController
	syncFunc     func([]interface{}) error
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *baseSecondaryLayer2NetworkControllerEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	return h.baseHandler.areResourcesEqual(h.objType, obj1, obj2)
}

// GetInternalCacheEntry returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (h *baseSecondaryLayer2NetworkControllerEventHandler) GetInternalCacheEntry(obj interface{}) interface{} {
	return h.oc.GetInternalCacheEntryForSecondaryNetwork(h.objType, obj)
}

// GetResourceFromInformerCache returns the latest state of the object, given an object key and its type.
// from the informers cache.
func (h *baseSecondaryLayer2NetworkControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	return h.baseHandler.getResourceFromInformerCache(h.objType, h.watchFactory, key)
}

// RecordAddEvent records the add event on this given object.
func (h *baseSecondaryLayer2NetworkControllerEventHandler) RecordAddEvent(obj interface{}) {
	switch h.objType {
	case factory.MultiNetworkPolicyType:
		mnp := obj.(*mnpapi.MultiNetworkPolicy)
		klog.V(5).Infof("Recording add event on multinetwork policy %s/%s", mnp.Namespace, mnp.Name)
		metrics.GetConfigDurationRecorder().Start("multinetworkpolicy", mnp.Namespace, mnp.Name)
	}
}

// RecordUpdateEvent records the udpate event on this given object.
func (h *baseSecondaryLayer2NetworkControllerEventHandler) RecordUpdateEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordDeleteEvent records the delete event on this given object.
func (h *baseSecondaryLayer2NetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordSuccessEvent records the success event on this given object.
func (h *baseSecondaryLayer2NetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
	h.baseHandler.recordAddEvent(h.objType, obj)
}

// RecordErrorEvent records the error event on this given object.
func (h *baseSecondaryLayer2NetworkControllerEventHandler) RecordErrorEvent(obj interface{}, reason string, err error) {
}

// IsResourceScheduled returns true if the given object has been scheduled.
// Only applied to pods for now. Returns true for all other types.
func (h *baseSecondaryLayer2NetworkControllerEventHandler) IsResourceScheduled(obj interface{}) bool {
	return h.baseHandler.isResourceScheduled(h.objType, obj)
}

// AddResource adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
// Given an object to add and a boolean specifying if the function was executed from iterateRetryResources
func (h *baseSecondaryLayer2NetworkControllerEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
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
func (h *baseSecondaryLayer2NetworkControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
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
func (h *baseSecondaryLayer2NetworkControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
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

func (h *baseSecondaryLayer2NetworkControllerEventHandler) SyncFunc(objs []interface{}) error {
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
func (h *baseSecondaryLayer2NetworkControllerEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	return h.baseHandler.isObjectInTerminalState(h.objType, obj)
}

// BaseSecondaryLayer2NetworkController structure holds per-network fields and network specific
// configuration for secondary layer2/localnet network controller
type BaseSecondaryLayer2NetworkController struct {
	BaseSecondaryNetworkController
}

func (oc *BaseSecondaryLayer2NetworkController) initRetryFramework() {
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
func (oc *BaseSecondaryLayer2NetworkController) newRetryFramework(
	objectType reflect.Type) *retry.RetryFramework {
	eventHandler := &baseSecondaryLayer2NetworkControllerEventHandler{
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

// stop gracefully stops the controller, and delete all logical entities for this network if requested
func (oc *BaseSecondaryLayer2NetworkController) stop() {
	klog.Infof("Stop secondary %s network controller of network %s", oc.TopologyType(), oc.GetNetworkName())
	close(oc.stopChan)
	oc.cancelableCtx.Cancel()
	oc.wg.Wait()

	if oc.ipamClaimsHandler != nil {
		oc.watchFactory.RemoveIPAMClaimsHandler(oc.ipamClaimsHandler)
	}
	if oc.policyHandler != nil {
		oc.watchFactory.RemoveMultiNetworkPolicyHandler(oc.policyHandler)
	}
	if oc.podHandler != nil {
		oc.watchFactory.RemovePodHandler(oc.podHandler)
	}
	if oc.nodeHandler != nil {
		oc.watchFactory.RemoveNodeHandler(oc.nodeHandler)
	}
	if oc.namespaceHandler != nil {
		oc.watchFactory.RemoveNamespaceHandler(oc.namespaceHandler)
	}
}

// cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *BaseSecondaryLayer2NetworkController) cleanup() error {
	netName := oc.GetNetworkName()
	klog.Infof("Delete OVN logical entities for network %s", netName)
	// delete layer 2 logical switches
	ops, err := libovsdbops.DeleteLogicalSwitchesWithPredicateOps(oc.nbClient, nil,
		func(item *nbdb.LogicalSwitch) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting switches of network %s: %v", netName, err)
	}

	ops, err = cleanupPolicyLogicalEntities(oc.nbClient, ops, oc.controllerName)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to deleting switches of network %s: %v", netName, err)
	}

	return nil
}

func (oc *BaseSecondaryLayer2NetworkController) run() error {
	// WatchNamespaces() should be started first because it has no other
	// dependencies, and WatchNodes() depends on it
	if err := oc.WatchNamespaces(); err != nil {
		return err
	}

	if err := oc.WatchNodes(); err != nil {
		return err
	}

	// when on IC, it will be the NetworkController that returns the IPAMClaims
	// IPs back to the pool
	if oc.allocatesPodAnnotation() && oc.allowPersistentIPs() {
		// WatchIPAMClaims should be started before WatchPods to prevent OVN-K
		// master assigning IPs to pods without taking into account the persistent
		// IPs set aside for the IPAMClaims
		if err := oc.WatchIPAMClaims(); err != nil {
			return err
		}
	}

	if err := oc.WatchPods(); err != nil {
		return err
	}

	// WatchMultiNetworkPolicy depends on WatchPods and WatchNamespaces
	if err := oc.WatchMultiNetworkPolicy(); err != nil {
		return err
	}

	return nil
}

func (oc *BaseSecondaryLayer2NetworkController) initializeLogicalSwitch(switchName string, clusterSubnets []config.CIDRNetworkEntry,
	excludeSubnets []*net.IPNet) (*nbdb.LogicalSwitch, error) {
	logicalSwitch := nbdb.LogicalSwitch{
		Name:        switchName,
		ExternalIDs: map[string]string{},
	}
	logicalSwitch.ExternalIDs[types.NetworkExternalID] = oc.GetNetworkName()
	logicalSwitch.ExternalIDs[types.TopologyExternalID] = oc.TopologyType()

	hostSubnets := make([]*net.IPNet, 0, len(clusterSubnets))
	for _, clusterSubnet := range clusterSubnets {
		subnet := clusterSubnet.CIDR
		hostSubnets = append(hostSubnets, subnet)
		if utilnet.IsIPv6CIDR(subnet) {
			logicalSwitch.OtherConfig = map[string]string{"ipv6_prefix": subnet.IP.String()}
		} else {
			logicalSwitch.OtherConfig = map[string]string{"subnet": subnet.String()}
		}
	}

	if oc.isLayer2Interconnect() {
		err := oc.zoneICHandler.AddTransitSwitchConfig(&logicalSwitch)
		if err != nil {
			return nil, err
		}
	}

	err := libovsdbops.CreateOrUpdateLogicalSwitch(oc.nbClient, &logicalSwitch)
	if err != nil {
		return nil, fmt.Errorf("failed to create logical switch %+v: %v", logicalSwitch, err)
	}

	if err = oc.lsManager.AddOrUpdateSwitch(switchName, hostSubnets, excludeSubnets...); err != nil {
		return nil, err
	}

	return &logicalSwitch, nil
}

func (oc *BaseSecondaryLayer2NetworkController) addUpdateNodeEvent(node *corev1.Node) error {
	if oc.isLocalZoneNode(node) {
		return oc.addUpdateLocalNodeEvent(node)
	}
	return oc.addUpdateRemoteNodeEvent(node)
}

func (oc *BaseSecondaryLayer2NetworkController) addUpdateLocalNodeEvent(node *corev1.Node) error {
	_, present := oc.localZoneNodes.LoadOrStore(node.Name, true)

	if !present {
		// process all pods so they are reconfigured as local
		errs := oc.addAllPodsOnNode(node.Name)
		if errs != nil {
			err := utilerrors.Join(errs...)
			return err
		}
	}

	return nil
}

func (oc *BaseSecondaryLayer2NetworkController) addUpdateRemoteNodeEvent(node *corev1.Node) error {
	_, present := oc.localZoneNodes.Load(node.Name)

	if present {
		err := oc.deleteNodeEvent(node)
		if err != nil {
			return err
		}

		// process all pods so they are reconfigured as remote
		errs := oc.addAllPodsOnNode(node.Name)
		if errs != nil {
			err = utilerrors.Join(errs...)
			return err
		}
	}

	return nil
}

func (oc *BaseSecondaryLayer2NetworkController) deleteNodeEvent(node *corev1.Node) error {
	oc.localZoneNodes.Delete(node.Name)
	return nil
}

func (oc *BaseSecondaryLayer2NetworkController) syncNodes(nodes []interface{}) error {
	for _, tmp := range nodes {
		node, ok := tmp.(*corev1.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", tmp)
		}

		// Add the node to the foundNodes only if it belongs to the local zone.
		if oc.isLocalZoneNode(node) {
			oc.localZoneNodes.Store(node.Name, true)
		}
	}

	return nil
}

func (oc *BaseSecondaryLayer2NetworkController) syncIPAMClaims(ipamClaims []interface{}) error {
	switchName, err := oc.getExpectedSwitchName(dummyPod())
	if err != nil {
		return err
	}
	return oc.ipamClaimsReconciler.Sync(ipamClaims, oc.lsManager.ForSwitch(switchName))
}

func dummyPod() *corev1.Pod {
	return &corev1.Pod{Spec: corev1.PodSpec{NodeName: ""}}
}

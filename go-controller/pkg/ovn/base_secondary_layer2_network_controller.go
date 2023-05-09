package ovn

import (
	"fmt"
	"net"
	"reflect"
	"strconv"
	"time"

	iputils "github.com/containernetworking/plugins/pkg/ip"
	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// method/structure shared by all layer 2 network controller, including localnet and layer2 network controllres.

type secondaryLayer2NetworkControllerEventHandler struct {
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
	switch h.objType {
	case factory.MultiNetworkPolicyType:
		mnp := obj.(*mnpapi.MultiNetworkPolicy)
		klog.V(5).Infof("Recording update event on multinetwork policy %s/%s", mnp.Namespace, mnp.Name)
		metrics.GetConfigDurationRecorder().Start("multinetworkpolicy", mnp.Namespace, mnp.Name)
	}
}

// RecordDeleteEvent records the delete event on this given object.
func (h *secondaryLayer2NetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
	switch h.objType {
	case factory.MultiNetworkPolicyType:
		mnp := obj.(*mnpapi.MultiNetworkPolicy)
		klog.V(5).Infof("Recording delete event on multinetwork policy %s/%s", mnp.Namespace, mnp.Name)
		metrics.GetConfigDurationRecorder().Start("multinetworkpolicy", mnp.Namespace, mnp.Name)
	}
}

// RecordSuccessEvent records the success event on this given object.
func (h *secondaryLayer2NetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
	switch h.objType {
	case factory.MultiNetworkPolicyType:
		mnp := obj.(*mnpapi.MultiNetworkPolicy)
		klog.V(5).Infof("Recording success event on multinetwork policy %s/%s", mnp.Namespace, mnp.Name)
		metrics.GetConfigDurationRecorder().End("multinetworkpolicy", mnp.Namespace, mnp.Name)
	}
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
	return h.oc.AddSecondaryNetworkResourceCommon(h.objType, obj)
}

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (h *secondaryLayer2NetworkControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	return h.oc.UpdateSecondaryNetworkResourceCommon(h.objType, oldObj, newObj, inRetryCache)
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *secondaryLayer2NetworkControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	return h.oc.DeleteSecondaryNetworkResourceCommon(h.objType, obj, cachedObj)
}

func (h *secondaryLayer2NetworkControllerEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.PodType:
			syncFunc = h.oc.syncPodsForSecondaryNetwork

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
func (h *secondaryLayer2NetworkControllerEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	return h.baseHandler.isObjectInTerminalState(h.objType, obj)
}

// BaseSecondaryLayer2NetworkController structure holds per-network fields and network specific
// configuration for secondary layer2/localnet network controller
type BaseSecondaryLayer2NetworkController struct {
	BaseSecondaryNetworkController
}

func (oc *BaseSecondaryLayer2NetworkController) initRetryFramework() {
	oc.retryPods = oc.newRetryFramework(factory.PodType)
	if oc.doesNetworkRequireIPAM() {
		oc.retryNamespaces = oc.newRetryFramework(factory.NamespaceType)
		oc.retryNetworkPolicies = oc.newRetryFramework(factory.MultiNetworkPolicyType)
	}
}

// newRetryFramework builds and returns a retry framework for the input resource type;
func (oc *BaseSecondaryLayer2NetworkController) newRetryFramework(
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

// Stop gracefully stops the controller, and delete all logical entities for this network if requested
func (oc *BaseSecondaryLayer2NetworkController) Stop() {
	klog.Infof("Stop secondary %s network controller of network %s", oc.TopologyType(), oc.GetNetworkName())
	close(oc.stopChan)
	oc.wg.Wait()

	if oc.policyHandler != nil {
		oc.watchFactory.RemoveMultiNetworkPolicyHandler(oc.policyHandler)
	}
	if oc.podHandler != nil {
		oc.watchFactory.RemovePodHandler(oc.podHandler)
	}
	if oc.namespaceHandler != nil {
		oc.watchFactory.RemoveNamespaceHandler(oc.namespaceHandler)
	}
}

// cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *BaseSecondaryLayer2NetworkController) cleanup(topotype, netName string) error {
	klog.Infof("Delete OVN logical entities for %s network controller of network %s", topotype, netName)

	// delete layer 2 logical switches
	ops, err := libovsdbops.DeleteLogicalSwitchesWithPredicateOps(oc.nbClient, nil,
		func(item *nbdb.LogicalSwitch) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting switches of network %s: %v", netName, err)
	}

	ops, err = cleanupPolicyLogicalEntities(oc.nbClient, ops, netName)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to deleting switches of network %s: %v", netName, err)
	}

	return nil
}

func (oc *BaseSecondaryLayer2NetworkController) Run() error {
	klog.Infof("Starting all the Watchers for network %s ...", oc.GetNetworkName())
	start := time.Now()

	// WatchNamespaces() should be started first because it has no other
	// dependencies, and WatchNodes() depends on it
	if err := oc.WatchNamespaces(); err != nil {
		return err
	}

	if err := oc.WatchPods(); err != nil {
		return err
	}

	// WatchMultiNetworkPolicy depends on WatchPods and WatchNamespaces
	if err := oc.WatchMultiNetworkPolicy(); err != nil {
		return err
	}

	klog.Infof("Completing all the Watchers for network %s took %v", oc.GetNetworkName(), time.Since(start))

	// controller is fully running and resource handlers have synced, update Topology version in OVN
	if err := oc.updateL2TopologyVersion(); err != nil {
		return fmt.Errorf("failed to update topology version for network %s: %v", oc.GetNetworkName(), err)
	}

	return nil
}

func (oc *BaseSecondaryLayer2NetworkController) InitializeLogicalSwitch(switchName string, clusterSubnets []config.CIDRNetworkEntry,
	excludeSubnets []*net.IPNet) (*nbdb.LogicalSwitch, error) {
	logicalSwitch := nbdb.LogicalSwitch{
		Name:        switchName,
		ExternalIDs: map[string]string{},
	}
	logicalSwitch.ExternalIDs[types.NetworkExternalID] = oc.GetNetworkName()
	logicalSwitch.ExternalIDs[types.TopologyExternalID] = oc.TopologyType()
	logicalSwitch.ExternalIDs[types.TopologyVersionExternalID] = strconv.Itoa(oc.topologyVersion)

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

	err := libovsdbops.CreateOrUpdateLogicalSwitch(oc.nbClient, &logicalSwitch, &logicalSwitch.OtherConfig, &logicalSwitch.ExternalIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to create logical switch %+v: %v", logicalSwitch, err)
	}

	if err = oc.lsManager.AddSwitch(switchName, logicalSwitch.UUID, hostSubnets); err != nil {
		return nil, err
	}

	// FIXME: allocate IP ranges when https://github.com/ovn-org/ovn-kubernetes/issues/3369 is fixed
	for _, excludeSubnet := range excludeSubnets {
		for excludeIP := excludeSubnet.IP; excludeSubnet.Contains(excludeIP); excludeIP = iputils.NextIP(excludeIP) {
			var ipMask net.IPMask
			if excludeIP.To4() != nil {
				ipMask = net.CIDRMask(32, 32)
			} else {
				ipMask = net.CIDRMask(128, 128)
			}
			_ = oc.lsManager.AllocateIPs(switchName, []*net.IPNet{{IP: excludeIP, Mask: ipMask}})
		}
	}

	return &logicalSwitch, nil
}

package ovn

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

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
}

// RecordUpdateEvent records the udpate event on this given object.
func (h *secondaryLayer2NetworkControllerEventHandler) RecordUpdateEvent(obj interface{}) {
}

// RecordDeleteEvent records the delete event on this given object.
func (h *secondaryLayer2NetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
}

// RecordSuccessEvent records the success event on this given object.
func (h *secondaryLayer2NetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
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
		if h.oc.IsExpose() {
			node, ok := obj.(*kapi.Node)
			if !ok {
				return fmt.Errorf("failed casting %T object to *kapi.Node", obj)
			}
			if err := h.oc.routeTenantSubnetToJoinRouter(node.Name); err != nil {
				return err
			}
			if err := h.oc.masqueradeTenantSubnet(node.Name); err != nil {
				return err
			}
		}
	case factory.PodType:
		if err := h.oc.AddSecondaryNetworkResourceCommon(h.objType, obj); err != nil {
			return err
		}
		if h.oc.IsExpose() {
			if err := h.oc.ensureRerouteToGwPolicy(obj); err != nil {
				return err
			}
		}
	}
	return nil
}

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (h *secondaryLayer2NetworkControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	switch h.objType {
	case factory.PodType:
		pod, ok := newObj.(*kapi.Pod)
		if !ok {
			return fmt.Errorf("failed casting %T object to *kapi.Pod", newObj)
		}
		isLiveMigrationLeftover, err := kubevirt.PodIsLiveMigrationLeftOver(h.oc.client, pod)
		if err != nil {
			return err
		}
		// If this pod is a kubevirt live migration leftover we should do nothing
		// or we may end up overwriting lsp requested-chassis and enroute policy
		if isLiveMigrationLeftover {
			return nil
		}
		if err := h.oc.UpdateSecondaryNetworkResourceCommon(h.objType, oldObj, newObj, inRetryCache); err != nil {
			return err
		}
		if h.oc.IsExpose() {
			if err := h.oc.ensureRerouteToGwPolicy(newObj); err != nil {
				return err
			}
		}
	}
	return nil
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *secondaryLayer2NetworkControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	pod, ok := obj.(*kapi.Pod)
	if !ok {
		return fmt.Errorf("failed casting %T object to *kapi.Pod", obj)
	}
	isLiveMigrationLeftOver, err := kubevirt.PodIsLiveMigrationLeftOver(h.oc.client, pod)
	if err != nil {
		return err
	}

	// Keep the LSP if this pod is what remains from a kubevirt live migration
	if isLiveMigrationLeftOver {
		return nil
	}

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

		case factory.NodeType:
			syncFunc = h.oc.syncNodes

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
	BaseSecondaryNetworkController

	// waitGroup per-Controller
	wg *sync.WaitGroup
}

// NewSecondaryLayer2NetworkController create a new OVN controller for the given secondary layer2 nad
func NewSecondaryLayer2NetworkController(cnci *CommonNetworkControllerInfo, netInfo util.NetInfo,
	netconfInfo util.NetConfInfo) *SecondaryLayer2NetworkController {
	stopChan := make(chan struct{})

	oc := &SecondaryLayer2NetworkController{
		BaseSecondaryNetworkController: BaseSecondaryNetworkController{
			BaseNetworkController: BaseNetworkController{
				CommonNetworkControllerInfo: *cnci,
				NetConfInfo:                 netconfInfo,
				NetInfo:                     netInfo,
				lsManager:                   lsm.NewL2SwitchManager(),
				logicalPortCache:            newPortCache(stopChan),
				namespaces:                  make(map[string]*namespaceInfo),
				namespacesMutex:             sync.Mutex{},
				addressSetFactory:           addressset.NewOvnAddressSetFactory(cnci.nbClient),
				stopChan:                    stopChan,
			},
		},
		wg: &sync.WaitGroup{},
	}

	// disable multicast support for secondary networks
	oc.multicastSupport = false

	oc.initRetryFramework()
	return oc
}

func (oc *SecondaryLayer2NetworkController) initRetryFramework() {
	oc.retryPods = oc.newRetryFramework(factory.PodType)
	oc.retryNodes = oc.newRetryFramework(factory.NodeType)
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

// Start starts the secondary layer2 controller, handles all events and creates all needed logical entities
func (oc *SecondaryLayer2NetworkController) Start(ctx context.Context) error {
	klog.Infof("Start secondary %s network controller of network %s", oc.TopologyType(), oc.GetNetworkName())
	if err := oc.Init(); err != nil {
		return err
	}

	return oc.Run()
}

// Stop gracefully stops the controller, and delete all logical entities for this network if requested
func (oc *SecondaryLayer2NetworkController) Stop() {
	klog.Infof("Stop secondary %s network controller of network %s", oc.TopologyType(), oc.GetNetworkName())
	close(oc.stopChan)
	oc.wg.Wait()

	if oc.nodeHandler != nil {
		oc.watchFactory.RemoveNodeHandler(oc.nodeHandler)
	}

	if oc.podHandler != nil {
		oc.watchFactory.RemovePodHandler(oc.podHandler)
	}
}

// Cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *SecondaryLayer2NetworkController) Cleanup(netName string) error {
	klog.Infof("Delete OVN logical entities for %s network controller of network %s", types.Layer3Topology, netName)

	// delete layer 2 logical switches
	ops, err := libovsdbops.DeleteLogicalSwitchesWithPredicateOps(oc.nbClient, nil,
		func(item *nbdb.LogicalSwitch) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting switches of network %s", netName)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to deleting switches of network %s", netName)
	}

	return nil
}

func (oc *SecondaryLayer2NetworkController) Run() error {
	klog.Infof("Starting all the Watchers for network %s ...", oc.GetNetworkName())
	start := time.Now()

	if err := oc.WatchNodes(); err != nil {
		return err
	}

	if err := oc.WatchPods(); err != nil {
		return err
	}

	klog.Infof("Completing all the Watchers for network %s took %v", oc.GetNetworkName(), time.Since(start))

	// controller is fully running and resource handlers have synced, update Topology version in OVN
	if err := oc.updateL2TopologyVersion(); err != nil {
		return fmt.Errorf("failed to update topology version for network %s: %v", oc.GetNetworkName(), err)
	}

	return nil
}

// WatchNodes starts the watching of node resource and calls
// back the appropriate handler logic
func (oc *SecondaryLayer2NetworkController) WatchNodes() error {
	if oc.nodeHandler != nil {
		return nil
	}
	handler, err := oc.retryNodes.WatchResource()
	if err == nil {
		oc.nodeHandler = handler
	}
	return err
}

func (oc *SecondaryLayer2NetworkController) Init() error {
	switchName := oc.GetNetworkScopedName(types.OVNLayer2Switch)
	logicalSwitch := nbdb.LogicalSwitch{
		Name:        switchName,
		ExternalIDs: map[string]string{},
	}
	if oc.IsSecondary() {
		logicalSwitch.ExternalIDs[types.NetworkExternalID] = oc.GetNetworkName()
		logicalSwitch.ExternalIDs[types.TopologyExternalID] = oc.TopologyType()
	}

	layer2NetConfInfo := oc.NetConfInfo.(*util.Layer2NetConfInfo)

	hostSubnets := make([]*net.IPNet, 0, len(layer2NetConfInfo.ClusterSubnets))
	for _, subnet := range layer2NetConfInfo.ClusterSubnets {
		hostSubnet := subnet.CIDR
		hostSubnets = append(hostSubnets, hostSubnet)
		if utilnet.IsIPv6CIDR(hostSubnet) {
			logicalSwitch.OtherConfig = map[string]string{"ipv6_prefix": hostSubnet.IP.String()}
		} else {
			logicalSwitch.OtherConfig = map[string]string{"subnet": hostSubnet.String()}
		}
	}

	err := libovsdbops.CreateOrUpdateLogicalSwitch(oc.nbClient, &logicalSwitch, &logicalSwitch.OtherConfig)
	if err != nil {
		return fmt.Errorf("failed to create logical switch %+v: %v", logicalSwitch, err)
	}

	err = oc.lsManager.AddSwitch(switchName, logicalSwitch.UUID, hostSubnets)
	if err != nil {
		return err
	}
	lsSubnet := logicalSwitch.OtherConfig["subnet"]
	_, switchSubnet, err := net.ParseCIDR(lsSubnet)
	gwIfAddr := util.GetNodeGatewayIfAddr(switchSubnet)
	if err != nil {
		return err
	}

	// Exclude the local router if the subnet is expose
	if oc.IsExpose() {
		layer2NetConfInfo.ExcludeIPs = append(layer2NetConfInfo.ExcludeIPs, gwIfAddr.IP)
	}
	for _, excludeIP := range layer2NetConfInfo.ExcludeIPs {
		var ipMask net.IPMask
		if excludeIP.To4() != nil {
			ipMask = net.CIDRMask(32, 32)
		} else {
			ipMask = net.CIDRMask(128, 128)
		}

		_ = oc.lsManager.AllocateIPs(switchName, []*net.IPNet{{IP: excludeIP, Mask: ipMask}})
	}

	// Attach to the distribuged router
	if oc.IsExpose() {
		// Create the port at the distributed router to attach the switch to
		lrpMAC := util.IPAddrToHWAddr(gwIfAddr.IP)

		logicalRouterPort := nbdb.LogicalRouterPort{
			Name:     types.RouterToSwitchPrefix + logicalSwitch.Name,
			MAC:      lrpMAC.String(),
			Networks: []string{gwIfAddr.String()},
		}
		logicalRouter := nbdb.LogicalRouter{Name: types.OVNClusterRouter}
		if err := libovsdbops.CreateOrUpdateLogicalRouterPort(oc.nbClient, &logicalRouter, &logicalRouterPort,
			nil, &logicalRouterPort.MAC, &logicalRouterPort.Networks); err != nil {
			return err
		}

		// Create the LSP
		logicalSwitchPort := nbdb.LogicalSwitchPort{
			Name:      types.SwitchToRouterPrefix + logicalSwitch.Name,
			Type:      "router",
			Addresses: []string{"router"},
			Options:   map[string]string{"router-port": logicalRouterPort.Name},
		}
		if err := libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &logicalSwitch, &logicalSwitchPort); err != nil {
			return nil
		}

		if err := oc.ensureDummyRoute(lsSubnet, gwIfAddr.IP.String()); err != nil {
			return err
		}

		if err := oc.ensureKeepInternalTrafficNextHopPolicy(lsSubnet); err != nil {
			return err
		}
	}

	return nil
}

func (oc *SecondaryLayer2NetworkController) syncNodes(nodes []interface{}) error {
	return nil
}

func (oc *SecondaryLayer2NetworkController) ensureDummyRoute(subnet, router string) error {
	// Add a dummy route to match the tenant cluster so we can continue implementing
	// routing with policies (if there is no match policies are not run).
	dummyRoute := nbdb.LogicalRouterStaticRoute{
		IPPrefix: subnet,
		Nexthop:  router,
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
	}

	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.Policy != nil && *item.Policy == *dummyRoute.Policy && item.Nexthop == dummyRoute.Nexthop && item.IPPrefix == dummyRoute.IPPrefix
	}
	if err := libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicate(oc.nbClient, types.OVNClusterRouter, &dummyRoute, p); err != nil {
		return fmt.Errorf("failed ensuring dummy route: %v", err)
	}
	return nil
}

func (oc *SecondaryLayer2NetworkController) ensureKeepInternalTrafficNextHopPolicy(subnet string) error {
	// Add a allow policy with higher priority to keep nexthop for e/s traffic
	// TODO: Read the internal subnets from the system
	podCIDR := "10.244.0.0/16"
	internalCIDR := "100.64.0.0/16"
	policy := nbdb.LogicalRouterPolicy{
		Match:    fmt.Sprintf("ip4.src == %s && ip4.dst == { %s, %s }", subnet, podCIDR, internalCIDR),
		Action:   nbdb.LogicalRouterPolicyActionAllow,
		Priority: 2,
	}

	predicate := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == policy.Priority && item.Match == policy.Match && item.Action == policy.Action
	}

	if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(oc.nbClient, types.OVNClusterRouter, &policy, predicate); err != nil {
		return fmt.Errorf("failed ensuring policy at cluster router to keep e/w nexthop: %v", err)
	}
	return nil
}

func (oc *SecondaryLayer2NetworkController) ensureRerouteToGwPolicy(obj interface{}) error {
	pod, ok := obj.(*kapi.Pod)
	if !ok {
		return fmt.Errorf("failed casting %T object to *knet.Pod", obj)
	}

	_, network, _ := util.PodWantsMultiNetwork(pod, oc.NetInfo)
	if network == nil {
		return fmt.Errorf("failed retrieving network")
	}
	nadName := util.GetNADName(network.Namespace, network.Name)
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadName)
	if err != nil {
		return err
	}

	nodeLRP := &nbdb.LogicalRouterPort{
		Name: types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + pod.Spec.NodeName,
	}

	nodeLRP, err = libovsdbops.GetLogicalRouterPort(oc.nbClient, nodeLRP)
	if err != nil {
		return err
	}
	nodeLRPIP, _, err := net.ParseCIDR(nodeLRP.Networks[0])
	if err != nil {
		return err
	}
	nodeGwAddress := nodeLRPIP.String()
	if nodeGwAddress == "" {
		return fmt.Errorf("missing node gw router port address")
	}

	for _, podIP := range podAnnotation.IPs {
		// Add a reroute policy to route VM n/s traffic to the node where the VM
		// is running
		policy := nbdb.LogicalRouterPolicy{
			Match:    fmt.Sprintf("ip4.src == %s", podIP.IP.String()),
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			Nexthops: []string{nodeGwAddress},
			Priority: 1,
		}

		predicate := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Priority == policy.Priority && item.Match == policy.Match && item.Action == policy.Action && item.Nexthops != nil && item.Nexthop == policy.Nexthop
		}

		if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(oc.nbClient, types.OVNClusterRouter, &policy, predicate); err != nil {
			return fmt.Errorf("failed ensuring policy to reroute to n/s traffic: %v", err)
		}
	}
	return nil
}

func (oc *SecondaryLayer2NetworkController) routeTenantSubnetToJoinRouter(nodeName string) error {
	joinGwPort := &nbdb.LogicalRouterPort{
		Name: types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter,
	}

	joinGwPort, err := libovsdbops.GetLogicalRouterPort(oc.nbClient, joinGwPort)
	if err != nil {
		return fmt.Errorf("failed getting current join logical router port %s: %v", joinGwPort.Name, err)
	}

	joinGwPortIP, _, err := net.ParseCIDR(joinGwPort.Networks[0])
	if err != nil {
		return err
	}

	layer2NetConfInfo := oc.NetConfInfo.(*util.Layer2NetConfInfo)
	for _, subnet := range layer2NetConfInfo.ClusterSubnets {
		cidr := subnet.CIDR.String()
		route := nbdb.LogicalRouterStaticRoute{
			IPPrefix: cidr,
			Nexthop:  joinGwPortIP.String(),
		}

		predicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.Nexthop == route.Nexthop && item.IPPrefix == route.IPPrefix
		}

		if err := libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicate(oc.nbClient, types.GWRouterPrefix+nodeName, &route, predicate); err != nil {
			return fmt.Errorf("failed ensuring route to join router at gw: %v", err)
		}
	}

	return nil
}

func (oc *SecondaryLayer2NetworkController) masqueradeTenantSubnet(nodeName string) error {
	currentGwLR := &nbdb.LogicalRouter{
		Name: types.GWRouterPrefix + nodeName,
	}

	currentGwLR, err := libovsdbops.GetLogicalRouter(oc.nbClient, currentGwLR)
	if err != nil {
		return fmt.Errorf("failed getting current gw logical router %s: %v", currentGwLR.Name, err)
	}

	currentGwLRP := &nbdb.LogicalRouterPort{
		Name: types.GWRouterToExtSwitchPrefix + currentGwLR.Name,
	}
	if err := oc.nbClient.Get(context.Background(), currentGwLRP); err != nil {
		return fmt.Errorf("failed getting current gw logical router port %s: %v", currentGwLRP.Name, err)
	}

	currentGwLRPIP, _, err := net.ParseCIDR(currentGwLRP.Networks[0])
	if err != nil {
		return err
	}

	if err := oc.nbClient.Get(context.Background(), currentGwLR); err != nil {
		return fmt.Errorf("failed getting current gw logical router %s: %v", currentGwLR.Name, err)
	}

	layer2NetConfInfo := oc.NetConfInfo.(*util.Layer2NetConfInfo)
	for _, subnet := range layer2NetConfInfo.ClusterSubnets {
		cidr := subnet.CIDR.String()
		masqueradeNAT := &nbdb.NAT{
			ExternalIP: currentGwLRPIP.String(),
			LogicalIP:  cidr,
			Type:       nbdb.NATTypeSNAT,
			Options: map[string]string{
				"stateless": "false",
			},
		}
		if err := libovsdbops.CreateOrUpdateNATs(oc.nbClient, currentGwLR, masqueradeNAT); err != nil {
			return fmt.Errorf("failed ensuring tenant subnet masquerade: %s")
		}
	}
	return nil
}

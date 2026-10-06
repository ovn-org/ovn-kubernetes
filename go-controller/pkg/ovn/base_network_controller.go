// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	nadinformerv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics/recorders"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/observability"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	nqoscontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/network_qos"
	lsm "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/routeimport"
	zoneic "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	ovnretry "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// CommonNetworkControllerInfo structure is place holder for all fields shared among controllers.
type CommonNetworkControllerInfo struct {
	client       clientset.Interface
	kube         *kube.KubeOVN
	watchFactory *factory.WatchFactory
	podRecorder  *metrics.PodRecorder

	// event recorder used to post events to k8s
	recorder record.EventRecorder

	// libovsdb northbound client interface
	nbClient libovsdbclient.Client

	// libovsdb southbound client interface
	sbClient libovsdbclient.Client

	// has multicast support; set to false for secondary networks.
	// TBD: Changes need to be made to support multicast for secondary networks
	multicastSupport bool

	// Supports OVN Template Load Balancers?
	svcTemplateSupport bool

	// Kubernetes node managed by this controller.
	nodeName string
}

// BaseNetworkController structure holds per-network fields and network specific configuration
// Note that all the methods with NetworkControllerInfo pointer receivers will be called
// by more than one type of network controllers.
type BaseNetworkController struct {
	CommonNetworkControllerInfo
	// controllerName should be used to identify objects owned by given controller in the db
	controllerName string

	// network information
	util.ReconcilableNetInfo
	nadKeysLock sync.Mutex
	lastNADKeys sets.Set[string]

	// retry framework for pods
	retryPods *ovnretry.RetryFramework
	// retry framework for namespaces
	retryNamespaces *ovnretry.RetryFramework
	// retry framework for network policies
	retryNetworkPolicies *ovnretry.RetryFramework
	// retry framework for network policies
	retryMultiNetworkPolicies *ovnretry.RetryFramework

	// nodeReconciler is the shared node controller used by controllers that
	// reconcile node topology through pkg/controllers/node.
	nodeReconciler *nodecontroller.NodeController
	// node annotation cache for shared node controllers (optional)
	nodeAnnotationCache *nodecontroller.NodeAnnotationCache

	// pod events factory handler
	podHandler *factory.Handler
	// namespace events factory Handler
	namespaceHandler *factory.Handler

	// A cache of all logical switches seen by the watcher and their subnets
	lsManager *lsm.LogicalSwitchManager

	// An utility to allocate the PodAnnotation to pods
	podAnnotationAllocator *pod.PodAnnotationAllocator

	// A cache of all logical ports known to the controller
	logicalPortCache *PortCache
	// optional callback for consumers that need to react when a pod's logical
	// port info is inserted/refreshed in logicalPortCache.
	onLogicalPortCacheAdd func(pod *corev1.Pod, nadKey string)

	// Info about known namespaces. You must use oc.getNamespaceLocked() or
	// oc.waitForNamespaceLocked() to read this map, and oc.createNamespaceLocked()
	// or oc.deleteNamespaceLocked() to modify it. namespacesMutex is only held
	// from inside those functions.
	namespaces      map[string]*namespaceInfo
	namespacesMutex sync.Mutex

	// An address set factory that creates address sets
	addressSetFactory addressset.AddressSetFactory

	// network policies map, key should be retrieved with getPolicyKey(policy *knet.NetworkPolicy).
	// network policies that failed to be created will also be added here, and can be retried or cleaned up later.
	// network policy is only deleted from this map after successful cleanup.
	// Allowed order of locking is namespace Lock -> oc.networkPolicies key Lock -> networkPolicy.Lock
	// Don't take namespace Lock while holding networkPolicy key lock to avoid deadlock.
	networkPolicies *syncmap.SyncMap[*networkPolicy]

	// map of existing shared port groups for network policies
	// port group exists in the db if and only if port group key is present in this map
	// key is namespace
	// allowed locking order is namespace Lock -> networkPolicy.Lock -> sharedNetpolPortGroups key Lock
	// make sure to keep this order to avoid deadlocks
	sharedNetpolPortGroups *syncmap.SyncMap[*defaultDenyPortGroups]

	addressSetManager *addresssetmanager.AddressSetManager

	// stopChan per controller
	stopChan chan struct{}
	// waitGroup per-Controller
	wg *sync.WaitGroup

	// some downstream components need to stop on their own or when the network
	// controller is stopped
	// use a chain of cancelable contexts for this
	cancelableCtx util.CancelableContext

	// zoneICHandler creates the interconnect resources for local nodes and remote nodes.
	// Interconnect resources are Transit switch and logical ports connecting this transit switch
	// to the cluster router. Please see zone_interconnect/interconnect_handler.go for more details.
	zoneICHandler *zoneic.ZoneInterconnectHandler

	// networkManager used for getting network information
	networkManager networkmanager.Interface

	// IP addresses of OVN Cluster logical router port ("GwRouterToJoinSwitchPrefix + OVNClusterRouter")
	// connecting to the join switch
	ovnClusterLRPToJoinIfAddrs []*net.IPNet

	observManager *observability.Manager

	routeImportManager routeimport.Manager

	// Controller used for programming OVN for Network QoS
	nqosController *nqoscontroller.Controller
}

func (oc *BaseNetworkController) reconcile(netInfo util.NetInfo, setNodeFailed func(string)) error {
	// gather some information first
	reconcileLocalNode := false
	subnetsChanged := clusterSubnetsChanged(oc, netInfo)
	wasAdvertised := util.IsPodNetworkAdvertisedAtNode(oc, oc.nodeName)
	isAdvertised := util.IsPodNetworkAdvertisedAtNode(netInfo, oc.nodeName)
	reconcileSubnetChange := subnetsChanged && (isAdvertised || config.OVNKubernetesFeature.EnableEgressIP)
	if wasAdvertised != isAdvertised || reconcileSubnetChange {
		reconcileLocalNode = true
	}
	reconcileRoutes := oc.routeImportManager != nil && oc.routeImportManager.NeedsReconciliation(netInfo)
	nadKeys := oc.networkManager.GetNADKeysForNetwork(netInfo.GetNetworkName())
	reconcilePendingPods := !oc.IsDefault() && oc.updateNADKeysChanged(nadKeys)
	reconcileNamespaces := sets.NewString()
	if oc.IsPrimaryNetwork() {
		// since shouldFilterNamespace filters out namespace events for namespaces unknown
		// to be served by this primary network, we need to reconcile namespaces once
		// the network is reconfigured to serve a namespace.
		reconcileNamespaces = sets.NewString(netInfo.GetNADNamespaces()...).Difference(
			sets.NewString(oc.GetNADNamespaces()...))
	}

	// set the new NetInfo, point of no return
	err := util.ReconcileNetInfo(oc.ReconcilableNetInfo, netInfo)
	if err != nil {
		return fmt.Errorf("failed to reconcile network information for network %s: %v", oc.GetNetworkName(), err)
	}
	return oc.doReconcile(reconcileRoutes, reconcilePendingPods, reconcileLocalNode, setNodeFailed, reconcileNamespaces.List())
}

func clusterSubnetsChanged(old, new util.NetInfo) bool {
	oldSubnets := sets.New[string]()
	for _, subnet := range old.Subnets() {
		oldSubnets.Insert(subnet.CIDR.String())
	}

	newSubnets := sets.New[string]()
	for _, subnet := range new.Subnets() {
		newSubnets.Insert(subnet.CIDR.String())
	}

	return !oldSubnets.Equal(newSubnets)
}

func (oc *BaseNetworkController) updateNADKeysChanged(nadKeys []string) bool {
	oc.nadKeysLock.Lock()
	defer oc.nadKeysLock.Unlock()

	next := sets.New(nadKeys...)
	changed := oc.lastNADKeys == nil || !next.Equal(oc.lastNADKeys)
	oc.lastNADKeys = next
	return changed
}

// doReconcile performs the reconciliation after the controller NetInfo has already being
// updated with the changes. What needs to be reconciled should already be known and
// provided on the arguments of the method. This method returns no error and logs them
// instead since once the controller NetInfo has been updated there is no point in retrying.
func (oc *BaseNetworkController) doReconcile(reconcileRoutes, reconcilePendingPods bool,
	reconcileLocalNode bool, setNodeFailed func(string), reconcileNamespaces []string,
) error {
	if reconcileRoutes {
		err := oc.routeImportManager.ReconcileNetwork(oc.GetNetworkName())
		if err != nil {
			klog.Errorf("Failed to reconcile network %s on route import controller: %v", oc.GetNetworkName(), err)
		}
	}

	if reconcileLocalNode {
		setNodeFailed(oc.nodeName)
		oc.nodeReconciler.ReconcileNetwork(oc.nodeName, oc.GetNetworkName())
	}

	if reconcilePendingPods {
		if err := ovnretry.RequeuePendingPods(oc.watchFactory, oc.GetNetInfo(), oc.retryPods); err != nil {
			klog.Errorf("Failed to requeue pending pods for network %s: %v", oc.GetNetworkName(), err)
		}
	}

	// reconciles namespaces that were added to the network, this will trigger namespace add event and
	// network controller creates the address set for the namespace.
	// To update gress policy ACLs with peer namespace address set, invoke requeuePeerNamespace method after
	// address set is created for the namespace.
	namespaceAdded := false
	for _, ns := range reconcileNamespaces {
		namespace, err := oc.watchFactory.GetNamespace(ns)
		if err != nil {
			klog.Infof("Failed to get namespace %s for reconciling network %s: %v", ns, oc.GetNetworkName(), err)
			continue
		}
		err = oc.retryNamespaces.AddRetryObjWithAddNoBackoff(namespace)
		if err != nil {
			klog.Infof("Failed to retry namespace %s for network %s: %v", ns, oc.GetNetworkName(), err)
			continue
		}
		namespaceAdded = true
		if err := oc.addRemotePodsInNamespace(ns); err != nil {
			klog.Errorf("Failed to requeue remote pods for namespace %s on network %s: %v", ns, oc.GetNetworkName(), err)
		}
	}
	if namespaceAdded {
		oc.retryNamespaces.RequestRetryObjs()
	}
	return nil
}

// DeregisterNodeHandler removes this controller from the shared node controller.
func (oc *BaseNetworkController) DeregisterNodeHandler() {
	oc.nodeReconciler.DeregisterNetworkController(oc.GetNetworkName())
}

// BaseUserDefinedNetworkController structure holds per-network fields and network specific
// configuration for UDN controller
type BaseUserDefinedNetworkController struct {
	BaseNetworkController

	// network policy events factory handler
	netPolicyHandler *factory.Handler
	// multi-network policy events factory handler
	multiNetPolicyHandler *factory.Handler
}

func (oc *BaseUserDefinedNetworkController) FilterOutResource(objType reflect.Type, obj interface{}) bool {
	switch objType {
	case factory.NamespaceType:
		ns, ok := obj.(*corev1.Namespace)
		if !ok {
			klog.Errorf("Failed to cast the provided object to a namespace")
			return false
		}
		return oc.shouldFilterNamespace(ns.Name)
	case factory.PodType:
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			klog.Errorf("Failed to cast the provided object to a pod")
			return false
		}
		return oc.shouldFilterNamespace(pod.GetNamespace())
	default:
		return false
	}
}

// FilterOutDeleteResource keeps pod delete events that still have resources
// owned by this controller, even if the live primary-NAD mapping is gone.
func (oc *BaseUserDefinedNetworkController) FilterOutDeleteResource(objType reflect.Type, obj interface{}) bool {
	filterOut := oc.FilterOutResource(objType, obj)
	if objType != factory.PodType || !filterOut {
		return filterOut
	}

	pod, ok := obj.(*corev1.Pod)
	if !ok {
		klog.Errorf("Failed to cast the provided object to a pod")
		return false
	}
	expectedSwitchName, err := oc.getExpectedSwitchName(pod)
	if err != nil {
		klog.Errorf("Failed to determine the expected switch for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return true
	}
	for _, portInfo := range oc.getPortInfoForUserDefinedNetwork(pod) {
		if portInfo != nil && portInfo.logicalSwitch == expectedSwitchName {
			return false
		}
	}
	return true
}

// shouldFilterNamespace reports whether namespace's events should be filtered out because
// it doesn't belong to this network. For primary networks, this asks the network manager
// for the namespace's actual primary NAD rather than checking this controller's own NetInfo,
// which under Dynamic UDN can diverge from it.
func (bnc *BaseNetworkController) shouldFilterNamespace(namespace string) bool {
	// Default network handles all namespaces
	// Secondary networks can handle pods from different namespaces
	if !bnc.IsPrimaryNetwork() {
		return false
	}
	if bnc.networkManager == nil {
		return !bnc.hasNADNamespace(namespace)
	}

	nadKey, err := bnc.networkManager.GetPrimaryNADForNamespace(namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false
		}
		return util.IsInvalidPrimaryNetworkError(err)
	}
	if nadKey == types.DefaultNetworkName {
		return true
	}

	networkName := bnc.networkManager.GetNetworkNameForNADKey(nadKey)
	if networkName == "" {
		return !bnc.hasNADNamespace(namespace)
	}
	return networkName != bnc.GetNetworkName()
}

func (bnc *BaseNetworkController) hasNADNamespace(namespace string) bool {
	return slices.Contains(bnc.GetNADNamespaces(), namespace)
}

func getNetworkControllerName(netName string) string {
	return netName + "-network-controller"
}

func (bnc *BaseNetworkController) getNetworkNameForNADKeyFunc() func(nadKey string) string {
	if bnc.networkManager == nil || !bnc.GetNetInfo().IsUserDefinedNetwork() {
		return nil
	}
	return bnc.networkManager.GetNetworkNameForNADKey
}

// NewCommonNetworkControllerInfo creates CommonNetworkControllerInfo shared by controllers
func NewCommonNetworkControllerInfo(client clientset.Interface, kube *kube.KubeOVN, wf *factory.WatchFactory,
	recorder record.EventRecorder, nbClient libovsdbclient.Client, sbClient libovsdbclient.Client,
	podRecorder *metrics.PodRecorder, multicastSupport, svcTemplateSupport bool, nodeName string,
) *CommonNetworkControllerInfo {
	return &CommonNetworkControllerInfo{
		client:             client,
		kube:               kube,
		watchFactory:       wf,
		recorder:           recorder,
		nbClient:           nbClient,
		sbClient:           sbClient,
		podRecorder:        podRecorder,
		multicastSupport:   multicastSupport,
		svcTemplateSupport: svcTemplateSupport,
		nodeName:           nodeName,
	}
}

func (bnc *BaseNetworkController) GetLogicalPortName(pod *corev1.Pod, nadKey string) string {
	if !bnc.IsUserDefinedNetwork() {
		return util.GetLogicalPortName(pod.Namespace, pod.Name)
	} else {
		return util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, nadKey)
	}
}

func (bnc *BaseNetworkController) AddConfigDurationRecord(kind, namespace, name string) (
	[]ovsdb.Operation, func(), time.Time, error,
) {
	if !bnc.IsUserDefinedNetwork() {
		return recorders.GetConfigDurationRecorder().AddOVN(bnc.nbClient, kind, namespace, name)
	}
	// TBD: no-op for UDN for now
	return []ovsdb.Operation{}, func() {}, time.Time{}, nil
}

// getOVNClusterRouterPortToJoinSwitchIPs returns the IP addresses for the
// logical router port "GwRouterToJoinSwitchPrefix + OVNClusterRouter" from the
// config.Gateway.V4JoinSubnet and  config.Gateway.V6JoinSubnet. This will
// always be the first IP from these subnets.
func (bnc *BaseNetworkController) getOVNClusterRouterPortToJoinSwitchIfAddrs() (gwLRPIPs []*net.IPNet, err error) {
	joinSubnetsConfig := []*net.IPNet{}
	if config.IPv4Mode {
		joinSubnetsConfig = append(joinSubnetsConfig, bnc.JoinSubnetV4())
	}
	if config.IPv6Mode {
		joinSubnetsConfig = append(joinSubnetsConfig, bnc.JoinSubnetV6())
	}
	for _, joinSubnet := range joinSubnetsConfig {
		joinSubnetBaseIP := utilnet.BigForIP(joinSubnet.IP)
		ipnet := &net.IPNet{
			IP:   utilnet.AddIPOffset(joinSubnetBaseIP, 1),
			Mask: joinSubnet.Mask,
		}
		gwLRPIPs = append(gwLRPIPs, ipnet)
	}

	return gwLRPIPs, nil
}

// syncNodeClusterRouterPort ensures a node's LS to the cluster router's LRP is created.
// NOTE: We could have created the router port in createNodeLogicalSwitch() instead of here,
// but chassis ID is not available at that moment. We need the chassis ID to set the
// gateway-chassis, which in effect pins the logical switch to the current node in OVN.
// Otherwise, ovn-controller will flood-fill unrelated datapaths unnecessarily, causing scale
// problems.
func (bnc *BaseNetworkController) syncNodeClusterRouterPort(node *corev1.Node, hostSubnets []*net.IPNet) error {
	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		return err
	}

	if len(hostSubnets) == 0 {
		hostSubnets, err = util.ParseNodeHostSubnetAnnotation(node, bnc.GetNetworkName())
		if err != nil {
			return err
		}
	}

	// logical router port MAC is based on IPv4 subnet if there is one, else IPv6
	var nodeLRPMAC net.HardwareAddr
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := bnc.GetNodeGatewayIP(hostSubnet)
		nodeLRPMAC = util.IPAddrToHWAddr(gwIfAddr.IP)
		if !utilnet.IsIPv6CIDR(hostSubnet) {
			break
		}
	}

	logicalRouterName := bnc.GetNetworkScopedClusterRouterName()
	lrpName := bnc.GetNetworkScopedRouterToSwitchPortName(node.Name)
	lrpNetworks := []string{}
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := bnc.GetNodeGatewayIP(hostSubnet)
		lrpNetworks = append(lrpNetworks, gwIfAddr.String())
	}

	var lrpOptions map[string]string
	enableGatewayMTU := util.ParseNodeGatewayMTUSupport(node)
	if enableGatewayMTU {
		lrpOptions = map[string]string{
			libovsdbops.GatewayMTU: strconv.Itoa(config.Default.MTU),
		}
	}
	if bnc.TopologyType() == types.Layer2Topology {
		// In layer2 topology transit router is a distributed router, so even local ports need to have a tunnel key.
		// we reserve the same tunnel key for all transit router to l2 switch ports on all nodes.
		if lrpOptions == nil {
			lrpOptions = make(map[string]string)
		}
		lrpOptions[libovsdbops.RequestedTnlKey] = strconv.Itoa(transitRouterToSwitchTunnelKey)
	}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     lrpName,
		MAC:      nodeLRPMAC.String(),
		Networks: lrpNetworks,
		Options:  lrpOptions,
	}
	logicalRouter := nbdb.LogicalRouter{Name: logicalRouterName}
	gatewayChassis := nbdb.GatewayChassis{
		Name:        lrpName + "-" + chassisID,
		ChassisName: chassisID,
		Priority:    1,
	}
	_, isNetIPv6 := bnc.IPMode()
	if bnc.TopologyType() == types.Layer2Topology &&
		isNetIPv6 &&
		util.IsNetworkSegmentationSupportEnabled() &&
		bnc.IsPrimaryNetwork() {
		logicalRouterPort.Ipv6RaConfigs = map[string]string{
			"address_mode":      "dhcpv6_stateful",
			"send_periodic":     "true",
			"max_interval":      "900", // 15 minutes
			"min_interval":      "300", // 5 minutes
			"router_preference": "LOW", // The static gateway configured by CNI is MEDIUM, so make this LOW so it has less effect for pods
		}
		if bnc.MTU() > 0 {
			logicalRouterPort.Ipv6RaConfigs["mtu"] = fmt.Sprintf("%d", bnc.MTU())
		}
	}

	err = libovsdbops.CreateOrUpdateLogicalRouterPort(bnc.nbClient, &logicalRouter, &logicalRouterPort,
		&gatewayChassis, &logicalRouterPort.MAC, &logicalRouterPort.Networks, &logicalRouterPort.Options)
	if err != nil {
		klog.Errorf("Failed to add gateway chassis %s to logical router port %s, error: %v", chassisID, lrpName, err)
		return err
	}

	return nil
}

func (bnc *BaseNetworkController) createNodeLogicalSwitch(nodeName string, hostSubnets []*net.IPNet,
	clusterLoadBalancerGroupUUID, switchLoadBalancerGroupUUID string,
) error {
	// logical router port MAC is based on IPv4 subnet if there is one, else IPv6
	var nodeLRPMAC net.HardwareAddr
	switchName := bnc.GetNetworkScopedSwitchName(nodeName)
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := bnc.GetNodeGatewayIP(hostSubnet)
		nodeLRPMAC = util.IPAddrToHWAddr(gwIfAddr.IP)
		if !utilnet.IsIPv6CIDR(hostSubnet) {
			break
		}
	}

	logicalSwitch := nbdb.LogicalSwitch{
		Name: switchName,
	}

	logicalSwitch.ExternalIDs = util.GenerateExternalIDsForSwitchOrRouter(bnc.GetNetInfo())
	var v4Gateway, v6Gateway net.IP
	logicalSwitch.OtherConfig = map[string]string{}
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := bnc.GetNodeGatewayIP(hostSubnet)
		mgmtIfAddr := bnc.GetNodeManagementIP(hostSubnet)

		if utilnet.IsIPv6CIDR(hostSubnet) {
			v6Gateway = gwIfAddr.IP

			logicalSwitch.OtherConfig["ipv6_prefix"] = hostSubnet.IP.String()
		} else {
			v4Gateway = gwIfAddr.IP
			excludeIPs := mgmtIfAddr.IP.String()
			if config.HybridOverlay.Enabled {
				hybridOverlayIfAddr := util.GetNodeHybridOverlayIfAddr(hostSubnet)
				excludeIPs += ".." + hybridOverlayIfAddr.IP.String()
			}
			logicalSwitch.OtherConfig["subnet"] = hostSubnet.String()
			logicalSwitch.OtherConfig["exclude_ips"] = excludeIPs
		}
	}

	if clusterLoadBalancerGroupUUID != "" && switchLoadBalancerGroupUUID != "" {
		logicalSwitch.LoadBalancerGroup = []string{clusterLoadBalancerGroupUUID, switchLoadBalancerGroupUUID}
	}

	// If supported, enable IGMP/MLD snooping and querier on the node.
	if bnc.multicastSupport {
		logicalSwitch.OtherConfig["mcast_snoop"] = "true"

		// Configure IGMP/MLD querier if the gateway IP address is known.
		// Otherwise disable it.
		if v4Gateway != nil || v6Gateway != nil {
			logicalSwitch.OtherConfig["mcast_querier"] = "true"
			logicalSwitch.OtherConfig["mcast_eth_src"] = nodeLRPMAC.String()
			if v4Gateway != nil {
				logicalSwitch.OtherConfig["mcast_ip4_src"] = v4Gateway.String()
			}
			if v6Gateway != nil {
				logicalSwitch.OtherConfig["mcast_ip6_src"] = util.HWAddrToIPv6LLA(nodeLRPMAC).String()
			}
		} else {
			logicalSwitch.OtherConfig["mcast_querier"] = "false"
		}
	}

	err := libovsdbops.CreateOrUpdateLogicalSwitch(bnc.nbClient, &logicalSwitch, &logicalSwitch.OtherConfig,
		&logicalSwitch.LoadBalancerGroup, &logicalSwitch.ExternalIDs)
	if err != nil {
		return fmt.Errorf("failed to add logical switch %+v: %v", logicalSwitch, err)
	}

	// Connect the switch to the router.
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      bnc.GetNetworkScopedSwitchToRouterPortName(nodeName),
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			libovsdbops.RouterPort: bnc.GetNetworkScopedRouterToSwitchPortName(nodeName),
		},
	}
	if bnc.IsDefault() {
		logicalSwitchPort.Options["arp_proxy"] = kubevirt.ComposeARPProxyLSPOption()
	}
	sw := nbdb.LogicalSwitch{Name: switchName}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(bnc.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		klog.Errorf("Failed to add logical port %+v to switch %s: %v", logicalSwitchPort, switchName, err)
		return err
	}

	if bnc.multicastSupport {
		err = libovsdbops.AddPortsToPortGroup(bnc.nbClient, bnc.getClusterPortGroupName(types.ClusterRtrPortGroupNameBase), logicalSwitchPort.UUID)
		if err != nil {
			return fmt.Errorf("failed adding port to portgroup for multicast: %v", err)
		}
	}
	// Add the switch to the logical switch cache
	migratableIPsByOwner, err := bnc.findMigratablePodIPsForSubnets(hostSubnets)
	if err != nil {
		return fmt.Errorf("failed finding migratable pod IPs belonging to %s: %v", nodeName, err)
	}

	return bnc.lsManager.AddOrUpdateSwitch(logicalSwitch.Name, hostSubnets, nil, migratableIPsByOwner)
}

// deleteNodeLogicalNetwork removes the logical switch and logical router port associated with the node
func (bnc *BaseNetworkController) deleteNodeLogicalNetwork(nodeName string) error {
	switchName := bnc.GetNetworkScopedName(nodeName)

	// Remove the logical switch associated with the node
	err := libovsdbops.DeleteLogicalSwitch(bnc.nbClient, switchName)
	if err != nil {
		return fmt.Errorf("failed to delete logical switch %s: %v", switchName, err)
	}

	logicalRouterName := bnc.GetNetworkScopedClusterRouterName()
	logicalRouter := nbdb.LogicalRouter{Name: logicalRouterName}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name: bnc.GetNetworkScopedRouterToSwitchPortName(nodeName),
	}
	err = libovsdbops.DeleteLogicalRouterPorts(bnc.nbClient, &logicalRouter, &logicalRouterPort)
	if err != nil {
		return fmt.Errorf("failed to delete router port %s: %w", logicalRouterPort.Name, err)
	}

	return nil
}

func (bnc *BaseNetworkController) addAllPodsOnNode(nodeName string) []error {
	errs := []error{}
	pods, err := bnc.watchFactory.GetAllPods()
	if err != nil {
		errs = append(errs, err)
		klog.Errorf("Unable to list existing pods for synchronizing node: %s, existing pods on this node may not function",
			nodeName)
	} else {
		klog.V(5).Infof("When adding node %s for network %s, found %d pods to add to retryPods", nodeName, bnc.GetNetworkName(), len(pods))
		for _, pod := range pods {
			pod := *pod
			if util.PodCompleted(&pod) {
				continue
			}
			if pod.Spec.NodeName != nodeName {
				continue
			}
			klog.V(5).Infof("Adding pod %s/%s to retryPods for network %s", pod.Namespace, pod.Name, bnc.GetNetworkName())
			err = bnc.retryPods.AddRetryObjWithAddNoBackoff(&pod)
			if err != nil {
				errs = append(errs, err)
				klog.Errorf("Failed to add pod %s/%s to retryPods for network %s: %v", pod.Namespace, pod.Name, bnc.GetNetworkName(), err)
			}
		}
	}
	bnc.retryPods.RequestRetryObjs()
	return errs
}

// addRemotePodsInNamespace requeues non-terminal remote pods in a namespace once it
// becomes served by this network. Their Add may have been filtered by shouldFilterNamespace
// while GetPrimaryNADForNamespace returned InvalidPrimaryNetworkError, and by then they can
// be past Pending, so RequeuePendingPods misses them. Local pods are skipped: they can't get
// past Pending without this controller's processing them.
func (bnc *BaseNetworkController) addRemotePodsInNamespace(namespace string) error {
	pods, err := bnc.watchFactory.GetPods(namespace)
	if err != nil {
		return fmt.Errorf("unable to list pods in namespace %s: %w", namespace, err)
	}

	var errs []error
	podsAdded := false
	for _, pod := range pods {
		pod := *pod
		if util.PodCompleted(&pod) || util.PodWantsHostNetwork(&pod) || bnc.isPodScheduledOnLocalNode(&pod) {
			continue
		}
		klog.V(5).Infof("Adding remote running pod %s/%s to retryPods for network %s",
			pod.Namespace, pod.Name, bnc.GetNetworkName())
		if err := bnc.retryPods.AddRetryObjWithAddNoBackoff(&pod); err != nil {
			errs = append(errs, fmt.Errorf("failed to add pod %s/%s to retryPods: %w", pod.Namespace, pod.Name, err))
			continue
		}
		podsAdded = true
	}
	if podsAdded {
		bnc.retryPods.RequestRetryObjs()
	}
	return utilerrors.Join(errs...)
}

// getNamespaceLocked locks namespacesMutex, looks up ns, and (if found), returns it with
// its mutex locked. If ns is not known, nil will be returned
func (bnc *BaseNetworkController) getNamespaceLocked(ns string, readOnly bool) (*namespaceInfo, func()) {
	// Only hold namespacesMutex while reading/modifying oc.namespaces. In particular,
	// we drop namespacesMutex while trying to claim nsInfo.Mutex, because something
	// else might have locked the nsInfo and be doing something slow with it, and we
	// don't want to block all access to oc.namespaces while that's happening.
	bnc.namespacesMutex.Lock()
	nsInfo := bnc.namespaces[ns]
	bnc.namespacesMutex.Unlock()

	if nsInfo == nil {
		return nil, nil
	}
	var unlockFunc func()
	if readOnly {
		unlockFunc = func() { nsInfo.RUnlock() }
		nsInfo.RLock()
	} else {
		unlockFunc = func() { nsInfo.Unlock() }
		nsInfo.Lock()
	}
	// Check that the namespace wasn't deleted while we were waiting for the lock
	bnc.namespacesMutex.Lock()
	defer bnc.namespacesMutex.Unlock()
	if nsInfo != bnc.namespaces[ns] {
		unlockFunc()
		return nil, nil
	}
	return nsInfo, unlockFunc
}

// deleteNamespaceLocked locks namespacesMutex, finds and deletes ns, and returns the
// namespace, locked. If error != nil, namespaceInfo is nil.
func (bnc *BaseNetworkController) deleteNamespaceLocked(ns string) (*namespaceInfo, error) {
	// The locking here is the same as in getNamespaceLocked

	bnc.namespacesMutex.Lock()
	nsInfo := bnc.namespaces[ns]
	bnc.namespacesMutex.Unlock()

	if nsInfo == nil {
		return nil, nil
	}
	nsInfo.Lock()

	bnc.namespacesMutex.Lock()
	defer bnc.namespacesMutex.Unlock()
	if nsInfo != bnc.namespaces[ns] {
		nsInfo.Unlock()
		return nil, nil
	}
	if nsInfo.portGroupName != "" {
		err := libovsdbops.DeletePortGroups(bnc.nbClient, nsInfo.portGroupName)
		if err != nil {
			nsInfo.Unlock()
			return nil, err
		}
	}
	delete(bnc.namespaces, ns)

	return nsInfo, nil
}

func (bnc *BaseNetworkController) syncNodeManagementPort(node *corev1.Node, switchName, routerName string, hostSubnets []*net.IPNet) ([]net.IP, error) {
	// get mac address from node only for legacy reasons, if it doesn't exist, then calculate it from subnets
	var macAddress net.HardwareAddr
	var err error
	// find suitable MAC address

	if bnc.IsDefault() {
		// check node annotation first for default network, to ensure we are not picking a new MAC when one was already configured
		if macAddress, err = util.ParseNodeManagementPortMACAddresses(node, bnc.GetNetworkName()); err != nil && !util.IsAnnotationNotSetError(err) {
			return nil, err
		}
	}
	if len(macAddress) == 0 {
		// calculate mac
		if len(hostSubnets) == 0 {
			return nil, fmt.Errorf("unable to generate MAC address, no subnets provided for network: %s", bnc.GetNetworkName())
		}
		macAddress = util.IPAddrToHWAddr(bnc.GetNodeManagementIP(hostSubnets[0]).IP)
	}

	var v4Subnet *net.IPNet
	addresses := macAddress.String()
	mgmtPortIPs := []net.IP{}
	for _, hostSubnet := range hostSubnets {
		mgmtIfAddr := bnc.GetNodeManagementIP(hostSubnet)
		addresses += " " + mgmtIfAddr.IP.String()
		mgmtPortIPs = append(mgmtPortIPs, mgmtIfAddr.IP)

		if err := bnc.addAllowACLFromNode(switchName, mgmtIfAddr.IP); err != nil {
			return nil, err
		}

		if !utilnet.IsIPv6CIDR(hostSubnet) {
			v4Subnet = hostSubnet
		}
		if config.Gateway.Mode == config.GatewayModeLocal {
			lrsr := nbdb.LogicalRouterStaticRoute{
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				IPPrefix: hostSubnet.String(),
				Nexthop:  mgmtIfAddr.IP.String(),
			}
			if bnc.IsUserDefinedNetwork() {
				lrsr.ExternalIDs = map[string]string{
					types.NetworkExternalID:  bnc.GetNetworkName(),
					types.TopologyExternalID: bnc.TopologyType(),
				}
			}
			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == lrsr.IPPrefix && libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
			}
			err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(bnc.nbClient, routerName,
				&lrsr, p, &lrsr.Nexthop)
			if err != nil {
				return nil, fmt.Errorf("error creating static route %+v on router %s: %v", lrsr, routerName, err)
			}
		}
	}

	// Create this node's management logical port on the node switch
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      bnc.GetNetworkScopedK8sMgmtIntfName(node.Name),
		Addresses: []string{addresses},
	}
	sw := nbdb.LogicalSwitch{Name: switchName}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(bnc.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return nil, err
	}

	clusterPortGroupName := bnc.getClusterPortGroupName(types.ClusterPortGroupNameBase)
	if err = libovsdbops.AddPortsToPortGroup(bnc.nbClient, clusterPortGroupName, logicalSwitchPort.UUID); err != nil {
		err1 := fmt.Errorf("failed to add port %s to cluster port group %s (%s): %w",
			logicalSwitchPort.Name, types.ClusterPortGroupNameBase, clusterPortGroupName, err)
		klog.Error(err1)
		return nil, err1
	}

	if v4Subnet != nil {
		if err := libovsdbutil.UpdateNodeSwitchExcludeIPs(bnc.nbClient, bnc.GetNetworkScopedK8sMgmtIntfName(node.Name), bnc.GetNetworkScopedSwitchName(node.Name), node.Name, v4Subnet, bnc.GetNodeManagementIP(v4Subnet)); err != nil {
			return nil, err
		}
	}

	return mgmtPortIPs, nil
}

func (bnc *BaseNetworkController) recordNodeErrorEvent(node *corev1.Node, nodeErr error) {
	if bnc.IsUserDefinedNetwork() {
		// TBD, noop for UDN for now
		return
	}
	if types.IsSuppressedError(nodeErr) {
		return
	}
	nodeRef, err := ref.GetReference(scheme.Scheme, node)
	if err != nil {
		klog.Errorf("Couldn't get a reference to node %s to post an event: %v", node.Name, err)
		return
	}

	klog.V(5).Infof("Posting %s event for Node %s: %v", corev1.EventTypeWarning, node.Name, nodeErr)
	bnc.recorder.Eventf(nodeRef, corev1.EventTypeWarning, "ErrorReconcilingNode", "%s", nodeErr.Error())
}

func (bnc *BaseNetworkController) recordPodErrorEvent(pod *corev1.Pod, podErr error) {
	podRef, err := ref.GetReference(scheme.Scheme, pod)
	if err != nil {
		klog.Errorf("Couldn't get a reference to pod %s/%s to post an event: '%v'",
			pod.Namespace, pod.Name, err)
	} else {
		klog.V(5).Infof("Posting a %s event for Pod %s/%s", corev1.EventTypeWarning, pod.Namespace, pod.Name)
		bnc.recorder.Eventf(podRef, corev1.EventTypeWarning, "ErrorReconcilingPod", "%s", podErr.Error())
	}
}

func (bnc *BaseNetworkController) doesNetworkRequireIPAM() bool {
	return util.DoesNetworkRequireIPAM(bnc.GetNetInfo())
}

func (bnc *BaseNetworkController) doesNetworkHaveDiscoverablePodIPs() bool {
	return util.DoesNetworkHaveDiscoverablePodIPs(bnc.GetNetInfo())
}

func (bnc *BaseNetworkController) getPodNADKeys(pod *corev1.Pod) []string {
	if !bnc.IsUserDefinedNetwork() {
		return []string{types.DefaultNetworkName}
	}
	podNADKeys, _ := util.PodNADKeys(pod, bnc.GetNetInfo(), bnc.networkManager.GetNetworkNameForNADKey)
	return podNADKeys
}

func (bnc *BaseNetworkController) getClusterPortGroupDbIDs(base string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupCluster, bnc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: base,
		})
}

// getClusterPortGroupName gets network scoped port group hash name; base is either
// ClusterPortGroupNameBase or ClusterRtrPortGroupNameBase.
func (bnc *BaseNetworkController) getClusterPortGroupName(base string) string {
	return libovsdbutil.GetPortGroupName(bnc.getClusterPortGroupDbIDs(base))
}

// GetLocalNode returns the node managed by this controller.
func (bnc *BaseNetworkController) GetLocalNode() (*corev1.Node, error) {
	node, err := bnc.watchFactory.GetNode(bnc.nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get local node %q: %w", bnc.nodeName, err)
	}
	return node, nil
}

// isLocalNode returns true if the node is local to this controller.
func (cnci *CommonNetworkControllerInfo) isLocalNode(node *corev1.Node) bool {
	return node.Name == cnci.nodeName
}

// GetNetworkRole returns the role of this controller's network for the given pod
func (bnc *BaseNetworkController) GetNetworkRole(pod *corev1.Pod) (string, error) {
	role, err := util.GetNetworkRole(
		bnc.GetNetInfo(),
		bnc.networkManager.GetPrimaryNADForNamespace,
		bnc.networkManager.GetNetworkNameForNADKey,
		pod,
	)
	if err != nil {
		if util.IsInvalidPrimaryNetworkError(err) {
			bnc.recordPodErrorEvent(pod, err)
		}
		return "", err
	}

	return role, nil
}

// hasInterconnectTransport returns whether the network East/West traffic goes
// through the interconnect overlay or not. This is not the case for networks
// that have no overlay or that use EVPN, and this method would typically be
// used to inhibit the configuration of interconnect resources in those cases.
func (bnc *BaseNetworkController) hasInterconnectTransport() bool {
	switch bnc.Transport() {
	case types.NetworkTransportEVPN, types.NetworkTransportNoOverlay:
		return false
	default:
		return true
	}
}

// isLayer2WithInterconnectTransport returns whether this is Layer 2 network
// that has East/West interconnect traffic. See hasInterconnectTransport.
func (bnc *BaseNetworkController) isLayer2WithInterconnectTransport() bool {
	return bnc.hasInterconnectTransport() && bnc.TopologyType() == types.Layer2Topology
}

// HandleNetworkRefChange enqueues node reconciliation when a NAD reference becomes active/inactive.
func (bnc *BaseNetworkController) HandleNetworkRefChange(nodeName string, _ bool) {
	bnc.nodeReconciler.ReconcileNetwork(nodeName, bnc.GetNetworkName())
}

func (bnc *BaseNetworkController) nodeZoneClusterChanged(oldNode, newNode *corev1.Node) bool {
	// Check if the annotations have changed. Use network topology and local params to skip unnecessary checks

	// NodeIDAnnotationChanged and NodeTransitSwitchPortAddrAnnotationChanged affects local and remote nodes
	if util.NodeIDAnnotationChanged(oldNode, newNode) {
		return true
	}

	if util.NodeTransitSwitchPortAddrAnnotationChanged(oldNode, newNode) {
		return true
	}
	return false
}

func (bnc *BaseNetworkController) findMigratablePodIPsForSubnets(subnets []*net.IPNet) (map[string][]*net.IPNet, error) {
	// live migration is not supported in combination with UDNs
	if bnc.IsUserDefinedNetwork() {
		return nil, nil
	}

	ipsByOwner := map[string][]*net.IPNet{}
	liveMigratablePods, err := kubevirt.FindLiveMigratablePods(bnc.watchFactory)
	if err != nil {
		return nil, err
	}

	for _, liveMigratablePod := range liveMigratablePods {
		isMigratedSourcePodStale, err := kubevirt.IsMigratedSourcePodStale(bnc.watchFactory, liveMigratablePod)
		if err != nil {
			return nil, err
		}
		if isMigratedSourcePodStale {
			continue
		}
		podAnnotation, err := util.UnmarshalPodAnnotation(liveMigratablePod.Annotations, bnc.GetNetworkName())
		if err != nil {
			// even though it can be normal to not have an annotation now, live
			// migration is a sensible process that might be used when draining
			// nodes on upgrades, so log a warning in every case to have the
			// information available
			klog.Warningf("Could not get pod annotation of pod %s/%s for network %s: %v",
				liveMigratablePod.Namespace,
				liveMigratablePod.Name,
				bnc.GetNetworkName(),
				err)
			continue
		}
		owner := bnc.podIPOwner(liveMigratablePod, bnc.GetNetworkName())
		for _, podIP := range podAnnotation.IPs {
			if util.IsContainedInAnyCIDR(podIP, subnets...) {
				ipsByOwner[owner] = append(ipsByOwner[owner], podIP)
			}
		}
	}
	return ipsByOwner, nil
}

func (bnc *BaseNetworkController) AddResourceCommon(objType reflect.Type, obj interface{}) error {
	switch objType {
	case factory.PolicyType:
		np, ok := obj.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *knet.NetworkPolicy", obj)
		}
		foundNamespaceNAD, err := bnc.networkManager.GetPrimaryNADForNamespace(np.Namespace)
		if err != nil {
			// If this is a UDN namespace that hasn't been processed yet, the default
			// controller should skip it while UDN controllers should retry.
			if bnc.GetNetworkName() == types.DefaultNetworkName && util.IsInvalidPrimaryNetworkError(err) {
				return nil
			}
			// Retry until the NAD controller has processed the primary NAD for this namespace.
			return fmt.Errorf("could not get primary network NAD for namespace %s: %v", np.Namespace, err)
		}
		if foundNamespaceNAD == types.DefaultNetworkName {
			// Only the default network controller should handle policies in default namespaces.
			if bnc.GetNetworkName() != types.DefaultNetworkName {
				return nil
			}
		} else {
			networkName := bnc.networkManager.GetNetworkNameForNADKey(foundNamespaceNAD)
			if networkName == "" {
				return fmt.Errorf("no primary network found for namespace %s", np.Namespace)
			}
			if bnc.GetNetworkName() != networkName {
				return nil
			}
		}
		netInfo, err := bnc.networkManager.GetActiveNetworkForNamespace(np.Namespace)
		if err != nil {
			return fmt.Errorf("could not get active network for namespace %s: %v", np.Namespace, err)
		}
		if netInfo == nil {
			// no active network, nothing to do
			return nil
		}
		if bnc.GetNetworkName() != netInfo.GetNetworkName() {
			return nil
		}
		if err := bnc.addNetworkPolicy(np); err != nil {
			klog.Infof("Network Policy add failed for %s/%s, will try again later: %v",
				np.Namespace, np.Name, err)
			return err
		}
	default:
		klog.Errorf("Can not process add resource event, object type %s is not supported", objType)
	}
	return nil
}

func (bnc *BaseNetworkController) DeleteResourceCommon(objType reflect.Type, obj interface{}) error {
	switch objType {
	case factory.PolicyType:
		knp, ok := obj.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.NetworkPolicy", obj)
		}
		return bnc.deleteNetworkPolicy(knp)
	default:
		klog.Errorf("Can not process delete resource event, object type %s is not supported", objType)
	}
	return nil
}

func (bnc *BaseNetworkController) newNetworkQoSController() error {
	var err error
	var nadInformer nadinformerv1.NetworkAttachmentDefinitionInformer

	if config.OVNKubernetesFeature.EnableMultiNetwork {
		nadInformer = bnc.watchFactory.NADInformer()
	}
	bnc.nqosController, err = nqoscontroller.NewController(
		bnc.controllerName,
		bnc.ReconcilableNetInfo.GetNetInfo(),
		bnc.nbClient,
		bnc.recorder,
		bnc.kube.NetworkQoSClient,
		bnc.watchFactory.NetworkQoSInformer(),
		bnc.watchFactory.NamespaceCoreInformer(),
		bnc.watchFactory.PodCoreInformer(),
		nadInformer,
		bnc.networkManager,
		bnc.addressSetFactory,
		bnc.nodeName,
	)
	return err
}

func initLoadBalancerGroups(nbClient libovsdbclient.Client, netInfo util.NetInfo) (
	clusterLoadBalancerGroupUUID, switchLoadBalancerGroupUUID, routerLoadBalancerGroupUUID string, err error,
) {
	loadBalancerGroupName := netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterLBGroupName)
	clusterLBGroup := nbdb.LoadBalancerGroup{Name: loadBalancerGroupName}
	ops, err := libovsdbops.CreateOrUpdateLoadBalancerGroupOps(nbClient, nil, &clusterLBGroup)
	if err != nil {
		klog.Errorf("Error creating operation for cluster-wide load balancer group %s: %v", loadBalancerGroupName, err)
		return
	}

	loadBalancerGroupName = netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterSwitchLBGroupName)
	clusterSwitchLBGroup := nbdb.LoadBalancerGroup{Name: loadBalancerGroupName}
	ops, err = libovsdbops.CreateOrUpdateLoadBalancerGroupOps(nbClient, ops, &clusterSwitchLBGroup)
	if err != nil {
		klog.Errorf("Error creating operation for cluster-wide switch load balancer group %s: %v", loadBalancerGroupName, err)
		return
	}

	loadBalancerGroupName = netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterRouterLBGroupName)
	clusterRouterLBGroup := nbdb.LoadBalancerGroup{Name: loadBalancerGroupName}
	ops, err = libovsdbops.CreateOrUpdateLoadBalancerGroupOps(nbClient, ops, &clusterRouterLBGroup)
	if err != nil {
		klog.Errorf("Error creating operation for cluster-wide router load balancer group %s: %v", loadBalancerGroupName, err)
		return
	}

	lbs := []*nbdb.LoadBalancerGroup{&clusterLBGroup, &clusterSwitchLBGroup, &clusterRouterLBGroup}
	if _, err = libovsdbops.TransactAndCheckAndSetUUIDs(nbClient, lbs, ops); err != nil {
		klog.Errorf("Error creating cluster-wide router load balancer group %s: %v", loadBalancerGroupName, err)
		return
	}

	clusterLoadBalancerGroupUUID = clusterLBGroup.UUID
	switchLoadBalancerGroupUUID = clusterSwitchLBGroup.UUID
	routerLoadBalancerGroupUUID = clusterRouterLBGroup.UUID

	return
}

func (bnc *BaseNetworkController) setupClusterPortGroups() error {
	pgIDs := bnc.getClusterPortGroupDbIDs(types.ClusterPortGroupNameBase)
	pg := &nbdb.PortGroup{
		Name: libovsdbutil.GetPortGroupName(pgIDs),
	}
	pg, err := libovsdbops.GetPortGroup(bnc.nbClient, pg)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to query cluster port group for network %s: %w", bnc.GetNetworkName(), err)
	}
	if pg == nil {
		// we didn't find an existing clusterPG, let's create a new empty PG (fresh cluster install)
		// Create a cluster-wide port group that all logical switch ports are part of
		pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
		err = libovsdbops.CreateOrUpdatePortGroups(bnc.nbClient, pg)
		if err != nil {
			return fmt.Errorf("failed to create cluster port group for network %s: %w", bnc.GetNetworkName(), err)
		}
	}

	pgIDs = bnc.getClusterPortGroupDbIDs(types.ClusterRtrPortGroupNameBase)
	pg = &nbdb.PortGroup{
		Name: libovsdbutil.GetPortGroupName(pgIDs),
	}
	pg, err = libovsdbops.GetPortGroup(bnc.nbClient, pg)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to query cluster router port group for network %s: %w", bnc.GetNetworkName(), err)
	}
	if pg == nil {
		// we didn't find an existing clusterRtrPG, let's create a new empty PG (fresh cluster install)
		// Create a cluster-wide port group with all node-to-cluster router
		// logical switch ports. Currently the only user is multicast but it might
		// be used for other features in the future.
		pg = libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
		err = libovsdbops.CreateOrUpdatePortGroups(bnc.nbClient, pg)
		if err != nil {
			return fmt.Errorf("failed to create cluster router port group for network %s: %w", bnc.GetNetworkName(), err)
		}
	}
	return nil
}

func (bnc *BaseNetworkController) GetSamplingConfig() *libovsdbops.SamplingConfig {
	if bnc.observManager != nil {
		return bnc.observManager.SamplingConfig()
	}
	return nil
}

func (bnc *BaseNetworkController) ensureDHCP(pod *corev1.Pod, podAnnotation *util.PodAnnotation, lsp *nbdb.LogicalSwitchPort) error {
	opts := []kubevirt.DHCPConfigsOpt{}

	ipv4DNSServer, ipv6DNSServer, err := kubevirt.RetrieveDNSServiceClusterIPs(bnc.watchFactory)
	if err != nil {
		return err
	}

	ipv4Gateway, _ := util.MatchFirstIPFamily(false /*ipv4*/, podAnnotation.Gateways)
	if ipv4Gateway != nil {
		opts = append(opts, kubevirt.WithIPv4Router(ipv4Gateway.String()))
	}

	if bnc.MTU() > 0 {
		opts = append(opts, kubevirt.WithIPv4MTU(bnc.MTU()))
	}

	opts = append(opts, kubevirt.WithIPv4DNSServer(ipv4DNSServer), kubevirt.WithIPv6DNSServer(ipv6DNSServer))

	return kubevirt.EnsureDHCPOptionsForLSP(bnc.controllerName, bnc.nbClient, pod, podAnnotation.IPs, lsp, opts...)
}

package ovn

import (
	"context"
	"fmt"
	"math"
	"net"
	"strconv"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovnretry "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
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

	// has SCTP support
	SCTPSupport bool

	// has multicast support; set to false for secondary networks.
	// TBD: Changes need to be made to support multicast for secondary networks
	multicastSupport bool

	// Supports OVN Template Load Balancers?
	svcTemplateSupport bool

	// Northbound database zone name to which this Controller is connected to - aka local zone
	zone string
}

// BaseNetworkController structure holds per-network fields and network specific configuration
// Note that all the methods with NetworkControllerInfo pointer receivers will be called
// by more than one type of network controllers.
type BaseNetworkController struct {
	CommonNetworkControllerInfo
	// controllerName should be used to identify objects owned by given controller in the db
	controllerName string

	// network information
	util.NetInfo

	// retry framework for pods
	retryPods *ovnretry.RetryFramework
	// retry framework for nodes
	retryNodes *ovnretry.RetryFramework
	// retry framework for namespaces
	retryNamespaces *ovnretry.RetryFramework
	// retry framework for network policies
	retryNetworkPolicies *ovnretry.RetryFramework

	// pod events factory handler
	podHandler *factory.Handler
	// node events factory handler
	nodeHandler *factory.Handler
	// namespace events factory Handler
	namespaceHandler *factory.Handler

	// A cache of all logical switches seen by the watcher and their subnets
	lsManager *lsm.LogicalSwitchManager

	// An utility to allocate the PodAnnotation to pods
	podAnnotationAllocator *pod.PodAnnotationAllocator

	// A cache of all logical ports known to the controller
	logicalPortCache *portCache

	// Info about known namespaces. You must use oc.getNamespaceLocked() or
	// oc.waitForNamespaceLocked() to read this map, and oc.createNamespaceLocked()
	// or oc.deleteNamespaceLocked() to modify it. namespacesMutex is only held
	// from inside those functions.
	namespaces      map[string]*namespaceInfo
	namespacesMutex sync.Mutex

	// An address set factory that creates address sets
	addressSetFactory addressset.AddressSetFactory

	// topology version of this network. It is first retrieved from network logical entities,
	// and will eventually updated to latest version once topology upgrade is done.
	topologyVersion int

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

	podSelectorAddressSets *syncmap.SyncMap[*PodSelectorAddressSet]

	// stopChan per controller
	stopChan chan struct{}
	// waitGroup per-Controller
	wg *sync.WaitGroup

	// List of nodes which belong to the local zone (stored as a sync map)
	// If the map is nil, it means the controller is not tracking the node events
	// and all the nodes are considered as local zone nodes.
	localZoneNodes *sync.Map
}

// BaseSecondaryNetworkController structure holds per-network fields and network specific
// configuration for secondary network controller
type BaseSecondaryNetworkController struct {
	BaseNetworkController
	// multi-network policy events factory handler
	policyHandler *factory.Handler
}

// NewCommonNetworkControllerInfo creates CommonNetworkControllerInfo shared by controllers
func NewCommonNetworkControllerInfo(client clientset.Interface, kube *kube.KubeOVN, wf *factory.WatchFactory,
	recorder record.EventRecorder, nbClient libovsdbclient.Client, sbClient libovsdbclient.Client,
	podRecorder *metrics.PodRecorder, SCTPSupport, multicastSupport, svcTemplateSupport bool) (*CommonNetworkControllerInfo, error) {
	zone, err := util.GetNBZone(nbClient)
	if err != nil {
		return nil, fmt.Errorf("error getting NB zone name : err - %w", err)
	}
	return &CommonNetworkControllerInfo{
		client:             client,
		kube:               kube,
		watchFactory:       wf,
		recorder:           recorder,
		nbClient:           nbClient,
		sbClient:           sbClient,
		podRecorder:        podRecorder,
		SCTPSupport:        SCTPSupport,
		multicastSupport:   multicastSupport,
		svcTemplateSupport: svcTemplateSupport,
		zone:               zone,
	}, nil
}

func (bnc *BaseNetworkController) GetLogicalPortName(pod *kapi.Pod, nadName string) string {
	if !bnc.IsSecondary() {
		return util.GetLogicalPortName(pod.Namespace, pod.Name)
	} else {
		return util.GetSecondaryNetworkLogicalPortName(pod.Namespace, pod.Name, nadName)
	}
}

func (bnc *BaseNetworkController) AddConfigDurationRecord(kind, namespace, name string) (
	[]ovsdb.Operation, func(), time.Time, error) {
	if !bnc.IsSecondary() {
		return metrics.GetConfigDurationRecorder().AddOVN(bnc.nbClient, kind, namespace, name)
	}
	// TBD: no op for secondary network for now
	return []ovsdb.Operation{}, func() {}, time.Time{}, nil
}

// createOvnClusterRouter creates the central router for the network
func (bnc *BaseNetworkController) createOvnClusterRouter() (*nbdb.LogicalRouter, error) {
	// Create default Control Plane Protection (COPP) entry for routers
	defaultCOPPUUID, err := EnsureDefaultCOPP(bnc.nbClient)
	if err != nil {
		return nil, fmt.Errorf("unable to create router control plane protection: %w", err)
	}

	// Create a single common distributed router for the cluster.
	logicalRouterName := bnc.GetNetworkScopedName(types.OVNClusterRouter)
	logicalRouter := nbdb.LogicalRouter{
		Name: logicalRouterName,
		ExternalIDs: map[string]string{
			"k8s-cluster-router":            "yes",
			types.TopologyVersionExternalID: strconv.Itoa(bnc.topologyVersion),
		},
		Options: map[string]string{
			"always_learn_from_arp_request": "false",
		},
		Copp: &defaultCOPPUUID,
	}
	if bnc.IsSecondary() {
		logicalRouter.ExternalIDs[types.NetworkExternalID] = bnc.GetNetworkName()
		logicalRouter.ExternalIDs[types.TopologyExternalID] = bnc.TopologyType()
	}
	if bnc.multicastSupport {
		logicalRouter.Options = map[string]string{
			"mcast_relay": "true",
		}
	}

	err = libovsdbops.CreateOrUpdateLogicalRouter(bnc.nbClient, &logicalRouter, &logicalRouter.Options,
		&logicalRouter.ExternalIDs, &logicalRouter.Copp)
	if err != nil {
		return nil, fmt.Errorf("failed to create distributed router %s, error: %v",
			logicalRouterName, err)
	}

	return &logicalRouter, nil
}

// syncNodeClusterRouterPort ensures a node's LS to the cluster router's LRP is created.
// NOTE: We could have created the router port in ensureNodeLogicalNetwork() instead of here,
// but chassis ID is not available at that moment. We need the chassis ID to set the
// gateway-chassis, which in effect pins the logical switch to the current node in OVN.
// Otherwise, ovn-controller will flood-fill unrelated datapaths unnecessarily, causing scale
// problems.
func (bnc *BaseNetworkController) syncNodeClusterRouterPort(node *kapi.Node, hostSubnets []*net.IPNet) error {
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
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		nodeLRPMAC = util.IPAddrToHWAddr(gwIfAddr.IP)
		if !utilnet.IsIPv6CIDR(hostSubnet) {
			break
		}
	}

	switchName := bnc.GetNetworkScopedName(node.Name)
	logicalRouterName := bnc.GetNetworkScopedName(types.OVNClusterRouter)
	lrpName := types.RouterToSwitchPrefix + switchName
	lrpNetworks := []string{}
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		lrpNetworks = append(lrpNetworks, gwIfAddr.String())
	}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     lrpName,
		MAC:      nodeLRPMAC.String(),
		Networks: lrpNetworks,
	}
	logicalRouter := nbdb.LogicalRouter{Name: logicalRouterName}
	gatewayChassis := nbdb.GatewayChassis{
		Name:        lrpName + "-" + chassisID,
		ChassisName: chassisID,
		Priority:    1,
	}

	err = libovsdbops.CreateOrUpdateLogicalRouterPort(bnc.nbClient, &logicalRouter, &logicalRouterPort,
		&gatewayChassis, &logicalRouterPort.MAC, &logicalRouterPort.Networks)
	if err != nil {
		klog.Errorf("Failed to add gateway chassis %s to logical router port %s, error: %v", chassisID, lrpName, err)
		return err
	}

	return nil
}

func (bnc *BaseNetworkController) createNodeLogicalSwitch(nodeName string, hostSubnets []*net.IPNet,
	clusterLoadBalancerGroupUUID, switchLoadBalancerGroupUUID string) error {
	// logical router port MAC is based on IPv4 subnet if there is one, else IPv6
	var nodeLRPMAC net.HardwareAddr
	switchName := bnc.GetNetworkScopedName(nodeName)
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		nodeLRPMAC = util.IPAddrToHWAddr(gwIfAddr.IP)
		if !utilnet.IsIPv6CIDR(hostSubnet) {
			break
		}
	}

	logicalSwitch := nbdb.LogicalSwitch{
		Name: switchName,
	}
	if bnc.IsSecondary() {
		logicalSwitch.ExternalIDs = map[string]string{
			types.NetworkExternalID:  bnc.GetNetworkName(),
			types.TopologyExternalID: bnc.TopologyType(),
		}
	}

	var v4Gateway, v6Gateway net.IP
	logicalSwitch.OtherConfig = map[string]string{}
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)

		if utilnet.IsIPv6CIDR(hostSubnet) {
			v6Gateway = gwIfAddr.IP

			logicalSwitch.OtherConfig["ipv6_prefix"] =
				hostSubnet.IP.String()
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
		&logicalSwitch.LoadBalancerGroup)
	if err != nil {
		return fmt.Errorf("failed to add logical switch %+v: %v", logicalSwitch, err)
	}

	// Connect the switch to the router.
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      types.SwitchToRouterPrefix + switchName,
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			"router-port": types.RouterToSwitchPrefix + switchName,
			"arp_proxy":   kubevirt.ComposeARPProxyLSPOption(),
		},
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
			klog.Errorf(err.Error())
			return err
		}
	}

	// Add the switch to the logical switch cache
	return bnc.lsManager.AddOrUpdateSwitch(logicalSwitch.Name, hostSubnets)
}

// UpdateNodeAnnotationWithRetry update node's annotation with the given node annotations.
func (cnci *CommonNetworkControllerInfo) UpdateNodeAnnotationWithRetry(nodeName string,
	nodeAnnotations map[string]string) error {
	// Retry if it fails because of potential conflict which is transient. Return error in the
	// case of other errors (say temporary API server down), and it will be taken care of by the
	// retry mechanism.
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		node, err := cnci.watchFactory.GetNode(nodeName)
		if err != nil {
			return err
		}

		cnode := node.DeepCopy()
		for k, v := range nodeAnnotations {
			cnode.Annotations[k] = v
		}
		return cnci.kube.UpdateNode(cnode)
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update node %s annotation", nodeName)
	}
	return nil
}

// deleteNodeLogicalNetwork removes the logical switch and logical router port associated with the node
func (bnc *BaseNetworkController) deleteNodeLogicalNetwork(nodeName string) error {
	switchName := bnc.GetNetworkScopedName(nodeName)

	// Remove the logical switch associated with the node
	err := libovsdbops.DeleteLogicalSwitch(bnc.nbClient, switchName)
	if err != nil {
		return fmt.Errorf("failed to delete logical switch %s: %v", switchName, err)
	}

	logicalRouterName := bnc.GetNetworkScopedName(types.OVNClusterRouter)
	logicalRouter := nbdb.LogicalRouter{Name: logicalRouterName}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name: types.RouterToSwitchPrefix + switchName,
	}
	err = libovsdbops.DeleteLogicalRouterPorts(bnc.nbClient, &logicalRouter, &logicalRouterPort)
	if err != nil {
		return fmt.Errorf("failed to delete router port %s: %w", logicalRouterPort.Name, err)
	}

	return nil
}

func (bnc *BaseNetworkController) addAllPodsOnNode(nodeName string) []error {
	errs := []error{}
	options := metav1.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
		ResourceVersion: "0",
	}
	pods, err := bnc.client.CoreV1().Pods(metav1.NamespaceAll).List(context.TODO(), options)
	if err != nil {
		errs = append(errs, err)
		klog.Errorf("Unable to list existing pods on node: %s, existing pods on this node may not function",
			nodeName)
	} else {
		klog.V(5).Infof("When adding node %s for network %s, found %d pods to add to retryPods", nodeName, bnc.GetNetworkName(), len(pods.Items))
		for _, pod := range pods.Items {
			pod := pod
			if util.PodCompleted(&pod) {
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

func (bnc *BaseNetworkController) updateL3TopologyVersion() error {
	currentTopologyVersion := strconv.Itoa(types.OvnCurrentTopologyVersion)
	clusterRouterName := bnc.GetNetworkScopedName(types.OVNClusterRouter)
	logicalRouter := nbdb.LogicalRouter{
		Name:        clusterRouterName,
		ExternalIDs: map[string]string{types.TopologyVersionExternalID: currentTopologyVersion},
	}
	err := libovsdbops.UpdateLogicalRouterSetExternalIDs(bnc.nbClient, &logicalRouter)
	if err != nil {
		return fmt.Errorf("failed to generate set topology version, err: %v", err)
	}
	bnc.topologyVersion = types.OvnCurrentTopologyVersion
	klog.Infof("Updated Logical_Router %s topology version to %s", clusterRouterName, currentTopologyVersion)
	return nil
}

func (bnc *BaseNetworkController) updateL2TopologyVersion() error {
	var switchName string

	currentTopologyVersion := strconv.Itoa(types.OvnCurrentTopologyVersion)
	topoType := bnc.TopologyType()
	switch topoType {
	case types.Layer2Topology:
		switchName = bnc.GetNetworkScopedName(types.OVNLayer2Switch)
	case types.LocalnetTopology:
		switchName = bnc.GetNetworkScopedName(types.OVNLocalnetSwitch)
	default:
		return fmt.Errorf("topology type %s is not supported", topoType)
	}
	logicalSwitch := nbdb.LogicalSwitch{
		Name:        switchName,
		ExternalIDs: map[string]string{types.TopologyVersionExternalID: currentTopologyVersion},
	}
	err := libovsdbops.UpdateLogicalSwitchSetExternalIDs(bnc.nbClient, &logicalSwitch)
	if err != nil {
		return fmt.Errorf("failed to generate set topology version, err: %v", err)
	}
	bnc.topologyVersion = types.OvnCurrentTopologyVersion
	klog.Infof("Updated Logical_Switch %s topology version to %s", switchName, currentTopologyVersion)
	return nil
}

// determineOVNTopoVersionFromOVN determines what OVN Topology version is being used.
// If TopologyVersionExternalID key in external_ids column does not exist, it is prior to OVN topology versioning
// and therefore set version number to OvnCurrentTopologyVersion
func (bnc *BaseNetworkController) determineOVNTopoVersionFromOVN() error {
	var topologyVersion int
	var err error

	if !bnc.IsSecondary() {
		topologyVersion, err = bnc.getOVNTopoVersionFromLogicalRouter(types.OVNClusterRouter)
	} else {
		topoType := bnc.TopologyType()
		switch topoType {
		case types.Layer3Topology:
			topologyVersion, err = bnc.getOVNTopoVersionFromLogicalRouter(bnc.GetNetworkScopedName(types.OVNClusterRouter))
		case types.Layer2Topology:
			topologyVersion, err = bnc.getOVNTopoVersionFromLogicalSwitch(bnc.GetNetworkScopedName(types.OVNLayer2Switch))
		case types.LocalnetTopology:
			topologyVersion, err = bnc.getOVNTopoVersionFromLogicalSwitch(bnc.GetNetworkScopedName(types.OVNLocalnetSwitch))
		default:
			return fmt.Errorf("topology type %s not supported", topoType)
		}
	}
	bnc.topologyVersion = topologyVersion
	return err
}

func (bnc *BaseNetworkController) getOVNTopoVersionFromLogicalRouter(clusterRouterName string) (int, error) {
	logicalRouter := &nbdb.LogicalRouter{Name: clusterRouterName}
	logicalRouter, err := libovsdbops.GetLogicalRouter(bnc.nbClient, logicalRouter)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return 0, fmt.Errorf("error getting router %s: %v", clusterRouterName, err)
	}
	if err == libovsdbclient.ErrNotFound {
		// no OVNClusterRouter exists, DB is empty, nothing to upgrade
		return math.MaxInt32, nil
	}
	v, exists := logicalRouter.ExternalIDs[types.TopologyVersionExternalID]
	if !exists {
		klog.Infof("No version string found. The OVN topology is before versioning is introduced. Upgrade needed")
		return 0, nil
	}
	ver, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid OVN topology version string for network %s, err: %v", bnc.GetNetworkName(), err)
	}
	return ver, nil
}

func (bnc *BaseNetworkController) getOVNTopoVersionFromLogicalSwitch(switchName string) (int, error) {
	logicalSwitch := &nbdb.LogicalSwitch{Name: switchName}
	logicalSwitch, err := libovsdbops.GetLogicalSwitch(bnc.nbClient, logicalSwitch)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return 0, fmt.Errorf("error getting switch %s: %v", switchName, err)
	}
	if err == libovsdbclient.ErrNotFound {
		// no switch exists, DB is empty, nothing to upgrade
		return math.MaxInt32, nil
	}
	v := logicalSwitch.ExternalIDs[types.TopologyVersionExternalID]
	ver, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid OVN topology version string for network %s, err: %v", bnc.GetNetworkName(), err)
	}
	return ver, nil
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
// namespace, locked.
func (bnc *BaseNetworkController) deleteNamespaceLocked(ns string) *namespaceInfo {
	// The locking here is the same as in getNamespaceLocked

	bnc.namespacesMutex.Lock()
	nsInfo := bnc.namespaces[ns]
	bnc.namespacesMutex.Unlock()

	if nsInfo == nil {
		return nil
	}
	nsInfo.Lock()

	bnc.namespacesMutex.Lock()
	defer bnc.namespacesMutex.Unlock()
	if nsInfo != bnc.namespaces[ns] {
		nsInfo.Unlock()
		return nil
	}
	if nsInfo.addressSet != nil {
		// Empty the address set, then delete it after an interval.
		if err := nsInfo.addressSet.SetIPs(nil); err != nil {
			klog.Errorf("Warning: failed to empty address set for deleted NS %s: %v", ns, err)
		}

		// Delete the address set after a short delay.
		// This is so NetworkPolicy handlers can converge and stop referencing it.
		addressSet := nsInfo.addressSet
		go func() {
			select {
			case <-bnc.stopChan:
				return
			case <-time.After(20 * time.Second):
				// Check to see if the NS was re-added in the meanwhile. If so,
				// only delete if the new NS's AddressSet shouldn't exist.
				nsInfo, nsUnlock := bnc.getNamespaceLocked(ns, true)
				if nsInfo != nil {
					defer nsUnlock()
					if nsInfo.addressSet != nil {
						klog.V(5).Infof("Skipping deferred deletion of AddressSet for NS %s: re-created", ns)
						return
					}
				}

				klog.V(5).Infof("Finishing deferred deletion of AddressSet for NS %s", ns)
				if err := addressSet.Destroy(); err != nil {
					klog.Errorf("Failed to delete AddressSet for NS %s: %v", ns, err.Error())
				}
			}
		}()
	}
	delete(bnc.namespaces, ns)

	return nsInfo
}

// WatchNodes starts the watching of the nodes resource and calls back the appropriate handler logic
func (bnc *BaseNetworkController) WatchNodes() error {
	if bnc.nodeHandler != nil {
		return nil
	}

	handler, err := bnc.retryNodes.WatchResource()
	if err == nil {
		bnc.nodeHandler = handler
	}
	return err
}

func (bnc *BaseNetworkController) recordNodeErrorEvent(node *kapi.Node, nodeErr error) {
	if bnc.IsSecondary() {
		// TBD, no op for secondary network for now
		return
	}
	nodeRef, err := ref.GetReference(scheme.Scheme, node)
	if err != nil {
		klog.Errorf("Couldn't get a reference to node %s to post an event: %v", node.Name, err)
		return
	}

	klog.V(5).Infof("Posting %s event for Node %s: %v", kapi.EventTypeWarning, node.Name, nodeErr)
	bnc.recorder.Eventf(nodeRef, kapi.EventTypeWarning, "ErrorReconcilingNode", nodeErr.Error())
}

func (bnc *BaseNetworkController) doesNetworkRequireIPAM() bool {
	return util.DoesNetworkRequireIPAM(bnc.NetInfo)
}

func (bnc *BaseNetworkController) buildPortGroup(hashName, name string, ports []*nbdb.LogicalSwitchPort, acls []*nbdb.ACL) *nbdb.PortGroup {
	externalIds := map[string]string{"name": name}
	if bnc.IsSecondary() {
		externalIds[types.NetworkExternalID] = bnc.GetNetworkName()
	}
	return libovsdbops.BuildPortGroup(hashName, ports, acls, externalIds)
}

func (bnc *BaseNetworkController) getPodNADNames(pod *kapi.Pod) []string {
	if !bnc.IsSecondary() {
		return []string{types.DefaultNetworkName}
	}
	podNadNames, _ := util.PodNadNames(pod, bnc.NetInfo)
	return podNadNames
}

// getClusterPortGroupName gets network scoped port group hash name; base is either
// ClusterPortGroupNameBase or ClusterRtrPortGroupNameBase.
func (bnc *BaseNetworkController) getClusterPortGroupName(base string) string {
	if bnc.IsSecondary() {
		return hashedPortGroup(bnc.GetNetworkName()) + "_" + base
	}
	return base
}

// GetLocalZoneNodes returns the list of local zone nodes
// A node is considered a local zone node if the zone name
// set in the node's annotation matches with the zone name
// set in the OVN Northbound database (to which this controller is connected to).
func (bnc *BaseNetworkController) GetLocalZoneNodes() ([]*kapi.Node, error) {
	nodes, err := bnc.watchFactory.GetNodes()
	if err != nil {
		return nil, fmt.Errorf("failed to get nodes: %v", err)
	}

	var zoneNodes []*kapi.Node
	for _, n := range nodes {
		if bnc.isLocalZoneNode(n) {
			zoneNodes = append(zoneNodes, n)
		}
	}

	return zoneNodes, nil
}

// isLocalZoneNode returns true if the node is part of the local zone.
func (bnc *BaseNetworkController) isLocalZoneNode(node *kapi.Node) bool {
	/** HACK BEGIN **/
	// TODO(tssurya): Remove this HACK a few months from now. This has been added only to
	// minimize disruption for upgrades when moving to interconnect=true.
	// We want the legacy ovnkube-master to wait for remote ovnkube-node to
	// signal it using "k8s.ovn.org/remote-zone-migrated" annotation before
	// considering a node as remote when we upgrade from "global" (1 zone IC)
	// zone to multi-zone. This is so that network disruption for the existing workloads
	// is negligible and until the point where ovnkube-node flips the switch to connect
	// to the new SBDB, it would continue talking to the legacy RAFT ovnkube-sbdb to ensure
	// OVN/OVS flows are intact.
	if bnc.zone == types.OvnDefaultZone {
		return !util.HasNodeMigratedZone(node)
	}
	/** HACK END **/
	return util.GetNodeZone(node) == bnc.zone
}

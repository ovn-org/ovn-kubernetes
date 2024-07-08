package ovn

import (
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/observability"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	nqoscontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/network_qos"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	zoneic "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/persistentips"
	ovnretry "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
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
	// retry framework for network policies
	retryMultiNetworkPolicies *ovnretry.RetryFramework
	// retry framework for IPAMClaims
	retryIPAMClaims *ovnretry.RetryFramework

	// pod events factory handler
	podHandler *factory.Handler
	// node events factory handler
	nodeHandler *factory.Handler
	// namespace events factory Handler
	namespaceHandler *factory.Handler
	// ipam claims events factory Handler
	ipamClaimsHandler *factory.Handler

	// A cache of all logical switches seen by the watcher and their subnets
	lsManager *lsm.LogicalSwitchManager

	// An utility to allocate the PodAnnotation to pods
	podAnnotationAllocator *pod.PodAnnotationAllocator

	ipamClaimsReconciler *persistentips.IPAMClaimReconciler

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

	// some downstream components need to stop on their own or when the network
	// controller is stopped
	// use a chain of cancelable contexts for this
	cancelableCtx util.CancelableContext

	// List of nodes which belong to the local zone (stored as a sync map)
	// If the map is nil, it means the controller is not tracking the node events
	// and all the nodes are considered as local zone nodes.
	localZoneNodes *sync.Map

	// zoneICHandler creates the interconnect resources for local nodes and remote nodes.
	// Interconnect resources are Transit switch and logical ports connecting this transit switch
	// to the cluster router. Please see zone_interconnect/interconnect_handler.go for more details.
	zoneICHandler *zoneic.ZoneInterconnectHandler

	// nadController used for getting network information for UDNs
	nadController nad.NADController
	// releasedPodsBeforeStartup tracks pods per NAD (map of NADs to pods UIDs)
	// might have been already be released on startup
	releasedPodsBeforeStartup  map[string]sets.Set[string]
	releasedPodsOnStartupMutex sync.Mutex

	// IP addresses of OVN Cluster logical router port ("GwRouterToJoinSwitchPrefix + OVNClusterRouter")
	// connecting to the join switch
	ovnClusterLRPToJoinIfAddrs []*net.IPNet

	observManager *observability.Manager

	// Controller used for programming OVN for Network QoS
	nqosController *nqoscontroller.Controller
}

// BaseSecondaryNetworkController structure holds per-network fields and network specific
// configuration for secondary network controller
type BaseSecondaryNetworkController struct {
	BaseNetworkController

	networkID *int

	// network policy events factory handler
	netPolicyHandler *factory.Handler
	// multi-network policy events factory handler
	multiNetPolicyHandler *factory.Handler
}

func getNetworkControllerName(netName string) string {
	return netName + "-network-controller"
}

// NewCommonNetworkControllerInfo creates CommonNetworkControllerInfo shared by controllers
func NewCommonNetworkControllerInfo(client clientset.Interface, kube *kube.KubeOVN, wf *factory.WatchFactory,
	recorder record.EventRecorder, nbClient libovsdbclient.Client, sbClient libovsdbclient.Client,
	podRecorder *metrics.PodRecorder, SCTPSupport, multicastSupport, svcTemplateSupport bool) (*CommonNetworkControllerInfo, error) {
	zone, err := libovsdbutil.GetNBZone(nbClient)
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
	logicalRouterName := bnc.GetNetworkScopedClusterRouterName()
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

	if util.IsNetworkSegmentationSupportEnabled() &&
		bnc.IsPrimaryNetwork() && !config.OVNKubernetesFeature.EnableInterconnect &&
		bnc.TopologyType() == types.Layer3Topology {
		// since in nonIC the ovn_cluster_router is distributed, we must specify the gatewayPort for the
		// conditional SNATs to signal OVN which gatewayport should be chosen if there are mutiple distributed
		// gateway ports. Now that the LRP is created, let's update the NATs to reflect that.
		lrp := nbdb.LogicalRouterPort{
			Name: lrpName,
		}
		logicalRouterPort, err := libovsdbops.GetLogicalRouterPort(bnc.nbClient, &lrp)
		if err != nil {
			return fmt.Errorf("failed to fetch gatewayport %s for network %q on node %q, err: %w",
				lrpName, bnc.GetNetworkName(), node.Name, err)
		}
		gatewayPort := logicalRouterPort.UUID
		p := func(item *nbdb.NAT) bool {
			return item.ExternalIDs[types.NetworkExternalID] == bnc.GetNetworkName() &&
				item.LogicalPort != nil && *item.LogicalPort == lrpName && item.Match != ""
		}
		nonICConditonalSNATs, err := libovsdbops.FindNATsWithPredicate(bnc.nbClient, p)
		if err != nil {
			return fmt.Errorf("failed to fetch conditional NATs %s for network %q on node %q, err: %w",
				lrpName, bnc.GetNetworkName(), node.Name, err)
		}
		for _, nat := range nonICConditonalSNATs {
			nat.GatewayPort = &gatewayPort
		}
		if err := libovsdbops.CreateOrUpdateNATs(bnc.nbClient, &logicalRouter, nonICConditonalSNATs...); err != nil {
			return fmt.Errorf("failed to fetch conditional NATs %s for network %q on node %q, err: %w",
				lrpName, bnc.GetNetworkName(), node.Name, err)
		}
	}
	return nil
}

func (bnc *BaseNetworkController) createNodeLogicalSwitch(nodeName string, hostSubnets []*net.IPNet,
	clusterLoadBalancerGroupUUID, switchLoadBalancerGroupUUID string) error {
	// logical router port MAC is based on IPv4 subnet if there is one, else IPv6
	var nodeLRPMAC net.HardwareAddr
	switchName := bnc.GetNetworkScopedSwitchName(nodeName)
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

	logicalSwitch.ExternalIDs = util.GenerateExternalIDsForSwitchOrRouter(bnc.NetInfo)
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
			klog.Errorf(err.Error())
			return fmt.Errorf("failed adding port to portgroup for multicast: %v", err)
		}
	}
	// Add the switch to the logical switch cache
	migratableIPsByPod, err := bnc.findMigratablePodIPsForSubnets(hostSubnets)
	if err != nil {
		return fmt.Errorf("failed finding migratable pod IPs belonging to %s: %v", nodeName, err)
	}

	return bnc.lsManager.AddOrUpdateSwitch(logicalSwitch.Name, hostSubnets, migratableIPsByPod...)
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
	pods, err := bnc.kube.GetPods(metav1.NamespaceAll, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
	})
	if err != nil {
		errs = append(errs, err)
		klog.Errorf("Unable to list existing pods on node: %s, existing pods on this node may not function",
			nodeName)
	} else {
		klog.V(5).Infof("When adding node %s for network %s, found %d pods to add to retryPods", nodeName, bnc.GetNetworkName(), len(pods))
		for _, pod := range pods {
			pod := *pod
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
	if nsInfo.addressSet != nil {
		// Empty the address set, then delete it after an interval.
		if err := nsInfo.addressSet.SetAddresses(nil); err != nil {
			klog.Errorf("Warning: failed to empty address set for deleted NS %s: %v", ns, err)
		}

		// Delete the address set after a short delay.
		// This is to avoid OVN warnings while the address set is still
		// referenced from NBDB ACLs until the NetworkPolicy handlers remove
		// them.
		addressSet := nsInfo.addressSet
		go func() {
			select {
			case <-bnc.stopChan:
				return
			case <-time.After(20 * time.Second):
				maybeDeleteAddressSet := func() bool {
					bnc.namespacesMutex.Lock()
					nsInfo := bnc.namespaces[ns]
					if nsInfo == nil {
						defer bnc.namespacesMutex.Unlock()
					} else {
						bnc.namespacesMutex.Unlock()
						nsInfo.Lock()
						defer nsInfo.Unlock()
						bnc.namespacesMutex.Lock()
						defer bnc.namespacesMutex.Unlock()
						if nsInfo != bnc.namespaces[ns] {
							// somebody deleted the namespace while waiting for
							// its lock, check again in case it was added back
							return false
						}
						// if somebody recreated the namespace during the delay,
						// check if it has an address set
						if nsInfo.addressSet != nil {
							klog.V(5).Infof("Skipping deferred deletion of AddressSet for NS %s: recreated", ns)
							return true
						}
					}
					klog.V(5).Infof("Finishing deferred deletion of AddressSet for NS %s", ns)
					if err := addressSet.Destroy(); err != nil {
						klog.Errorf("Failed to delete AddressSet for NS %s: %v", ns, err.Error())
					}
					return true
				}
				for {
					done := maybeDeleteAddressSet()
					if done {
						break
					}
				}
			}
		}()
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

func (bnc *BaseNetworkController) syncNodeManagementPort(node *kapi.Node, switchName, routerName string, hostSubnets []*net.IPNet) ([]net.IP, error) {
	macAddress, err := util.ParseNodeManagementPortMACAddresses(node, bnc.GetNetworkName())
	if err != nil {
		return nil, err
	}

	var v4Subnet *net.IPNet
	addresses := macAddress.String()
	mgmtPortIPs := []net.IP{}
	for _, hostSubnet := range hostSubnets {
		mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
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
			if bnc.IsSecondary() {
				lrsr.ExternalIDs = map[string]string{
					ovntypes.NetworkExternalID:  bnc.GetNetworkName(),
					ovntypes.TopologyExternalID: bnc.TopologyType(),
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

	// TODO(dceara): The cluster port group must be per network. So for now skip adding management port to the cluster port
	// group for secondary network's because the cluster port group is not yet created for secondary networks.
	if bnc.IsDefault() {
		if err = libovsdbops.AddPortsToPortGroup(bnc.nbClient, bnc.getClusterPortGroupName(types.ClusterPortGroupNameBase), logicalSwitchPort.UUID); err != nil {
			klog.Errorf(err.Error())
			return nil, err
		}
	}

	if v4Subnet != nil {
		if err := libovsdbutil.UpdateNodeSwitchExcludeIPs(bnc.nbClient, bnc.GetNetworkScopedK8sMgmtIntfName(node.Name), bnc.GetNetworkScopedSwitchName(node.Name), node.Name, v4Subnet); err != nil {
			return nil, err
		}
	}

	return mgmtPortIPs, nil
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

func (bnc *BaseNetworkController) recordPodErrorEvent(pod *kapi.Pod, podErr error) {
	podRef, err := ref.GetReference(scheme.Scheme, pod)
	if err != nil {
		klog.Errorf("Couldn't get a reference to pod %s/%s to post an event: '%v'",
			pod.Namespace, pod.Name, err)
	} else {
		klog.V(5).Infof("Posting a %s event for Pod %s/%s", kapi.EventTypeWarning, pod.Namespace, pod.Name)
		bnc.recorder.Eventf(podRef, kapi.EventTypeWarning, "ErrorReconcilingPod", podErr.Error())
	}
}

func (bnc *BaseNetworkController) doesNetworkRequireIPAM() bool {
	return util.DoesNetworkRequireIPAM(bnc.NetInfo)
}

func (bnc *BaseNetworkController) getPodNADNames(pod *kapi.Pod) []string {
	if !bnc.IsSecondary() {
		return []string{types.DefaultNetworkName}
	}
	podNadNames, _ := util.PodNadNames(pod, bnc.NetInfo)
	return podNadNames
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

// and is a wrapper around GetActiveNetworkForNamespace
func (bnc *BaseNetworkController) getActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return bnc.nadController.GetActiveNetworkForNamespace(namespace)
}

// GetNetworkRole returns the role of this controller's
// network for the given pod
// Expected values are:
// (1) "primary" if this network is the primary network of the pod.
//
//	The "default" network is the primary network of any pod usually
//	unless user-defined-network-segmentation feature has been activated.
//	If network segmentation feature is enabled then any user defined
//	network can be the primary network of the pod.
//
// (2) "secondary" if this network is the secondary network of the pod.
//
//	Only user defined networks can be secondary networks for a pod.
//
// (3) "infrastructure-locked" is applicable only to "default" network if
//
//	a user defined network is the "primary" network for this pod. This
//	signifies the "default" network is only used for probing and
//	is otherwise locked for all intents and purposes.
//
// NOTE: Like in other places, expectation is this function is always called
// from controller's that have some relation to the given pod, unrelated
// networks are treated as secondary networks so caller has to be careful
func (bnc *BaseNetworkController) GetNetworkRole(pod *kapi.Pod) (string, error) {
	if !util.IsNetworkSegmentationSupportEnabled() {
		// if user defined network segmentation is not enabled
		// then we know pod's primary network is "default" and
		// pod's secondary network is not its NOT primary network
		if bnc.IsDefault() {
			return types.NetworkRolePrimary, nil
		}
		return types.NetworkRoleSecondary, nil
	}
	activeNetwork, err := bnc.getActiveNetworkForNamespace(pod.Namespace)
	if err != nil {
		if util.IsUnprocessedActiveNetworkError(err) {
			bnc.recordPodErrorEvent(pod, err)
		}
		return "", err
	}
	if activeNetwork.GetNetworkName() == bnc.GetNetworkName() {
		return types.NetworkRolePrimary, nil
	}
	if bnc.IsDefault() {
		// if default network was not the primary network,
		// then when UDN is turned on, default network is the
		// infrastructure-locked network forthis pod
		return types.NetworkRoleInfrastructure, nil
	}
	return types.NetworkRoleSecondary, nil
}

func (bnc *BaseNetworkController) isLayer2Interconnect() bool {
	return config.OVNKubernetesFeature.EnableInterconnect && bnc.NetInfo.TopologyType() == types.Layer2Topology
}

func (bnc *BaseNetworkController) nodeZoneClusterChanged(oldNode, newNode *kapi.Node, newNodeIsLocalZone bool, netName string) bool {
	// Check if the annotations have changed. Use network topology and local params to skip unnecessary checks

	// NodeIDAnnotationChanged and NodeTransitSwitchPortAddrAnnotationChanged affects local and remote nodes
	if util.NodeIDAnnotationChanged(oldNode, newNode) {
		return true
	}

	if util.NodeTransitSwitchPortAddrAnnotationChanged(oldNode, newNode) {
		return true
	}

	// NodeGatewayRouterLRPAddrsAnnotationChanged would not affect local, nor localnet secondary network
	if !newNodeIsLocalZone && bnc.NetInfo.TopologyType() != types.LocalnetTopology && joinCIDRChanged(oldNode, newNode, netName) {
		return true
	}

	return false
}

func (bnc *BaseNetworkController) findMigratablePodIPsForSubnets(subnets []*net.IPNet) ([]*net.IPNet, error) {
	// live migration is not supported in combination with secondary networks
	if bnc.IsSecondary() {
		return nil, nil
	}

	ipSet := sets.New[string]()
	ipList := []*net.IPNet{}
	liveMigratablePods, err := kubevirt.FindLiveMigratablePods(bnc.watchFactory)
	if err != nil {
		return nil, err
	}

	for _, liveMigratablePod := range liveMigratablePods {
		if util.PodCompleted(liveMigratablePod) {
			continue
		}
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
		for _, podIP := range podAnnotation.IPs {
			if util.IsContainedInAnyCIDR(podIP, subnets...) {
				podIPString := podIP.String()
				// Skip duplicate IPs
				if !ipSet.Has(podIPString) {
					ipSet = ipSet.Insert(podIPString)
					ipList = append(ipList, &net.IPNet{
						IP:   podIP.IP,
						Mask: util.GetIPFullMask(podIP.IP),
					})
				}
			}
		}
	}
	return ipList, nil
}

func (bnc *BaseNetworkController) AddResourceCommon(objType reflect.Type, obj interface{}) error {
	switch objType {
	case factory.PolicyType:
		np, ok := obj.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *knet.NetworkPolicy", obj)
		}
		netinfo, err := bnc.getActiveNetworkForNamespace(np.Namespace)
		if err != nil {
			return fmt.Errorf("could not get active network for namespace %s: %v", np.Namespace, err)
		}
		if bnc.GetNetworkName() != netinfo.GetNetworkName() {
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
		netinfo, err := bnc.getActiveNetworkForNamespace(knp.Namespace)
		if err != nil {
			return fmt.Errorf("could not get active network for namespace %s: %v", knp.Namespace, err)
		}
		if bnc.GetNetworkName() != netinfo.GetNetworkName() {
			return nil
		}
		return bnc.deleteNetworkPolicy(knp)
	default:
		klog.Errorf("Can not process delete resource event, object type %s is not supported", objType)
	}
	return nil
}

func (bnc *BaseNetworkController) newNetworkQoSController() error {
	var err error
	bnc.nqosController, err = nqoscontroller.NewController(
		bnc.controllerName,
		bnc.NetInfo,
		bnc.nbClient,
		bnc.recorder,
		bnc.kube.NetworkQoSClient,
		bnc.watchFactory.NetworkQoSInformer(),
		bnc.watchFactory.NamespaceCoreInformer(),
		bnc.watchFactory.PodCoreInformer(),
		bnc.watchFactory.NodeCoreInformer(),
		bnc.addressSetFactory,
		bnc.isPodScheduledinLocalZone,
		bnc.zone,
	)
	return err
}

func initLoadBalancerGroups(nbClient libovsdbclient.Client, netInfo util.NetInfo) (
	clusterLoadBalancerGroupUUID, switchLoadBalancerGroupUUID, routerLoadBalancerGroupUUID string, err error) {

	loadBalancerGroupName := netInfo.GetNetworkScopedLoadBalancerGroupName(ovntypes.ClusterLBGroupName)
	clusterLBGroup := nbdb.LoadBalancerGroup{Name: loadBalancerGroupName}
	ops, err := libovsdbops.CreateOrUpdateLoadBalancerGroupOps(nbClient, nil, &clusterLBGroup)
	if err != nil {
		klog.Errorf("Error creating operation for cluster-wide load balancer group %s: %v", loadBalancerGroupName, err)
		return
	}

	loadBalancerGroupName = netInfo.GetNetworkScopedLoadBalancerGroupName(ovntypes.ClusterSwitchLBGroupName)
	clusterSwitchLBGroup := nbdb.LoadBalancerGroup{Name: loadBalancerGroupName}
	ops, err = libovsdbops.CreateOrUpdateLoadBalancerGroupOps(nbClient, ops, &clusterSwitchLBGroup)
	if err != nil {
		klog.Errorf("Error creating operation for cluster-wide switch load balancer group %s: %v", loadBalancerGroupName, err)
		return
	}

	loadBalancerGroupName = netInfo.GetNetworkScopedLoadBalancerGroupName(ovntypes.ClusterRouterLBGroupName)
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

func (bnc *BaseNetworkController) GetSamplingConfig() *libovsdbops.SamplingConfig {
	if bnc.observManager != nil {
		return bnc.observManager.SamplingConfig()
	}
	return nil
}

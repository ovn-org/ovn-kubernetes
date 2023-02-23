package ovn

import (
	"fmt"
	"net"
	"strings"
	"time"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// IdledServiceAnnotationSuffix is a constant string representing the suffix of the Service annotation key
	// whose value indicates the time stamp in RFC3339 format when a Service was idled
	IdledServiceAnnotationSuffix   = "idled-at"
	OvnNodeAnnotationRetryInterval = 100 * time.Millisecond
	OvnNodeAnnotationRetryTimeout  = 1 * time.Second
)

// cleanup obsolete *gressDefaultDeny port groups
func (oc *DefaultNetworkController) upgradeToNamespacedDenyPGOVNTopology(existingNodeList *kapi.NodeList) error {
	err := libovsdbops.DeletePortGroups(oc.nbClient, "ingressDefaultDeny", "egressDefaultDeny")
	if err != nil {
		klog.Errorf("%v", err)
	}
	return nil
}

// delete obsoleted logical OVN entities that are specific for Multiple join switches OVN topology. Also cleanup
// OVN entities for deleted nodes (similar to syncNodes() but for obsoleted Multiple join switches OVN topology)
func (oc *DefaultNetworkController) upgradeToSingleSwitchOVNTopology(existingNodeList *kapi.NodeList) error {
	existingNodes := make(map[string]bool)
	for _, node := range existingNodeList.Items {
		existingNodes[node.Name] = true

		// delete the obsoleted node-join-subnets annotation
		err := oc.kube.SetAnnotationsOnNode(node.Name, map[string]interface{}{"k8s.ovn.org/node-join-subnets": nil})
		if err != nil {
			klog.Errorf("Failed to remove node-join-subnets annotation for node %s", node.Name)
		}
	}

	p := func(item *nbdb.LogicalSwitch) bool {
		return strings.HasPrefix(item.Name, types.JoinSwitchPrefix)
	}
	legacyJoinSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(oc.nbClient, p)
	if err != nil {
		klog.Errorf("Failed to remove any legacy per node join switches")
	}

	for _, legacyJoinSwitch := range legacyJoinSwitches {
		// if the node was deleted when ovn-master was down, delete its per-node switch
		nodeName := strings.TrimPrefix(legacyJoinSwitch.Name, types.JoinSwitchPrefix)
		upgradeOnly := true
		if _, ok := existingNodes[nodeName]; !ok {
			_ = oc.deleteNodeLogicalNetwork(nodeName)
			upgradeOnly = false
		}

		// for all nodes include the ones that were deleted, delete its gateway entities.
		// See comments above the multiJoinSwitchGatewayCleanup() function for details.
		err := oc.multiJoinSwitchGatewayCleanup(nodeName, upgradeOnly)
		if err != nil {
			return err
		}
	}
	return nil
}

func (oc *DefaultNetworkController) upgradeOVNTopology(existingNodes *kapi.NodeList) error {
	err := oc.determineOVNTopoVersionFromOVN()
	if err != nil {
		return err
	}

	ver := oc.topologyVersion
	// If current DB version is greater than OvnSingleJoinSwitchTopoVersion, no need to upgrade to single switch topology
	if ver < types.OvnSingleJoinSwitchTopoVersion {
		klog.Infof("Upgrading to Single Switch OVN Topology")
		err = oc.upgradeToSingleSwitchOVNTopology(existingNodes)
	}
	if err == nil && ver < types.OvnNamespacedDenyPGTopoVersion {
		klog.Infof("Upgrading to Namespace Deny PortGroup OVN Topology")
		err = oc.upgradeToNamespacedDenyPGOVNTopology(existingNodes)
	}
	// If version is less than Host -> Service with OpenFlow, we need to remove and cleanup DGP
	if err == nil && ((ver < types.OvnHostToSvcOFTopoVersion && config.Gateway.Mode == config.GatewayModeShared) ||
		(ver < types.OvnRoutingViaHostTopoVersion)) {
		err = oc.cleanupDGP(existingNodes)
	}
	return err
}

// SetupMaster creates the central router and load-balancers for the network
func (oc *DefaultNetworkController) SetupMaster(existingNodeNames []string) error {
	// Create default Control Plane Protection (COPP) entry for routers
	logicalRouter, err := oc.createOvnClusterRouter()
	if err != nil {
		return err
	}
	oc.defaultCOPPUUID = *(logicalRouter.Copp)

	// Create a cluster-wide port group that all logical switch ports are part of
	pg := libovsdbops.BuildPortGroup(types.ClusterPortGroupName, types.ClusterPortGroupName, nil, nil)
	err = libovsdbops.CreateOrUpdatePortGroups(oc.nbClient, pg)
	if err != nil {
		klog.Errorf("Failed to create cluster port group: %v", err)
		return err
	}

	// Create a cluster-wide port group with all node-to-cluster router
	// logical switch ports.  Currently the only user is multicast but it might
	// be used for other features in the future.
	pg = libovsdbops.BuildPortGroup(types.ClusterRtrPortGroupName, types.ClusterRtrPortGroupName, nil, nil)
	err = libovsdbops.CreateOrUpdatePortGroups(oc.nbClient, pg)
	if err != nil {
		klog.Errorf("Failed to create cluster port group: %v", err)
		return err
	}

	// If supported, enable IGMP relay on the router to forward multicast
	// traffic between nodes.
	if oc.multicastSupport {
		// Drop IP multicast globally. Multicast is allowed only if explicitly
		// enabled in a namespace.
		if err := oc.createDefaultDenyMulticastPolicy(); err != nil {
			klog.Errorf("Failed to create default deny multicast policy, error: %v", err)
			return err
		}

		// Allow IP multicast from node switch to cluster router and from
		// cluster router to node switch.
		if err := oc.createDefaultAllowMulticastPolicy(); err != nil {
			klog.Errorf("Failed to create default deny multicast policy, error: %v", err)
			return err
		}
	}

	// Create OVNJoinSwitch that will be used to connect gateway routers to the distributed router.
	logicalSwitch := nbdb.LogicalSwitch{
		Name: types.OVNJoinSwitch,
	}
	// nothing is updated here, so no reason to pass fields
	err = libovsdbops.CreateOrUpdateLogicalSwitch(oc.nbClient, &logicalSwitch)
	if err != nil {
		return fmt.Errorf("failed to create logical switch %+v: %v", logicalSwitch, err)
	}

	// Initialize the OVNJoinSwitch switch IP manager
	// The OVNJoinSwitch will be allocated IP addresses in the range 100.64.0.0/16 or fd98::/64.
	oc.joinSwIPManager, err = lsm.NewJoinLogicalSwitchIPManager(oc.nbClient, logicalSwitch.UUID, existingNodeNames)
	if err != nil {
		return err
	}

	// Allocate IPs for logical router port "GwRouterToJoinSwitchPrefix + OVNClusterRouter". This should always
	// allocate the first IPs in the join switch subnets
	gwLRPIfAddrs, err := oc.joinSwIPManager.EnsureJoinLRPIPs(types.OVNClusterRouter)
	if err != nil {
		return fmt.Errorf("failed to allocate join switch IP address connected to %s: %v", types.OVNClusterRouter, err)
	}

	// Connect the distributed router to OVNJoinSwitch.
	drSwitchPort := types.JoinSwitchToGWRouterPrefix + types.OVNClusterRouter
	drRouterPort := types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter

	gwLRPMAC := util.IPAddrToHWAddr(gwLRPIfAddrs[0].IP)
	gwLRPNetworks := []string{}
	for _, gwLRPIfAddr := range gwLRPIfAddrs {
		gwLRPNetworks = append(gwLRPNetworks, gwLRPIfAddr.String())
	}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     drRouterPort,
		MAC:      gwLRPMAC.String(),
		Networks: gwLRPNetworks,
	}

	err = libovsdbops.CreateOrUpdateLogicalRouterPort(oc.nbClient, logicalRouter,
		&logicalRouterPort, nil, &logicalRouterPort.MAC, &logicalRouterPort.Networks)
	if err != nil {
		return fmt.Errorf("failed to add logical router port %+v on router %s: %v", logicalRouterPort, logicalRouter.Name, err)
	}

	// Create OVNJoinSwitch that will be used to connect gateway routers to the
	// distributed router and connect it to said dsitributed router.
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name: drSwitchPort,
		Type: "router",
		Options: map[string]string{
			"router-port": drRouterPort,
		},
		Addresses: []string{"router"},
	}
	sw := nbdb.LogicalSwitch{Name: types.OVNJoinSwitch}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return fmt.Errorf("failed to create logical switch port %+v and switch %s: %v", logicalSwitchPort, types.OVNJoinSwitch, err)
	}

	return nil
}

func (oc *DefaultNetworkController) syncNodeManagementPort(node *kapi.Node, hostSubnets []*net.IPNet) error {
	macAddress, err := util.ParseNodeManagementPortMACAddress(node)
	if err != nil {
		return err
	}

	if hostSubnets == nil {
		hostSubnets, err = util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
		if err != nil {
			return err
		}
	}

	var v4Subnet *net.IPNet
	addresses := macAddress.String()
	for _, hostSubnet := range hostSubnets {
		mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
		addresses += " " + mgmtIfAddr.IP.String()

		if err := addAllowACLFromNode(node.Name, mgmtIfAddr.IP, oc.nbClient); err != nil {
			return err
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
			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == lrsr.IPPrefix && libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
			}
			err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(oc.nbClient, types.OVNClusterRouter,
				&lrsr, p, &lrsr.Nexthop)
			if err != nil {
				return fmt.Errorf("error creating static route %+v on router %s: %v", lrsr, types.OVNClusterRouter, err)
			}
		}
	}

	// Create this node's management logical port on the node switch
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      types.K8sPrefix + node.Name,
		Addresses: []string{addresses},
	}
	sw := nbdb.LogicalSwitch{Name: node.Name}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return err
	}

	err = libovsdbops.AddPortsToPortGroup(oc.nbClient, types.ClusterPortGroupName, logicalSwitchPort.UUID)
	if err != nil {
		klog.Errorf(err.Error())
		return err
	}

	if v4Subnet != nil {
		if err := util.UpdateNodeSwitchExcludeIPs(oc.nbClient, node.Name, v4Subnet); err != nil {
			return err
		}
	}

	return nil
}

func (oc *DefaultNetworkController) syncGatewayLogicalNetwork(node *kapi.Node, l3GatewayConfig *util.L3GatewayConfig,
	hostSubnets []*net.IPNet, hostAddrs sets.String) error {
	var err error
	var gwLRPIPs, clusterSubnets []*net.IPNet
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
	}

	gwLRPIPs, err = oc.joinSwIPManager.EnsureJoinLRPIPs(node.Name)
	if err != nil {
		return fmt.Errorf("failed to allocate join switch port IP address for node %s: %v", node.Name, err)
	}

	drLRPIPs, _ := oc.joinSwIPManager.EnsureJoinLRPIPs(types.OVNClusterRouter)

	enableGatewayMTU := util.ParseNodeGatewayMTUSupport(node)

	err = oc.gatewayInit(node.Name, clusterSubnets, hostSubnets, l3GatewayConfig, oc.SCTPSupport, gwLRPIPs, drLRPIPs,
		enableGatewayMTU)
	if err != nil {
		return fmt.Errorf("failed to init shared interface gateway: %v", err)
	}

	for _, subnet := range hostSubnets {
		hostIfAddr := util.GetNodeManagementIfAddr(subnet)
		l3GatewayConfigIP, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6(hostIfAddr.IP), l3GatewayConfig.IPAddresses)
		if err != nil {
			return err
		}
		relevantHostIPs, err := util.MatchAllIPStringFamily(utilnet.IsIPv6(hostIfAddr.IP), hostAddrs.List())
		if err != nil && err != util.NoIPError {
			return err
		}
		if err := oc.addPolicyBasedRoutes(node.Name, hostIfAddr.IP.String(), l3GatewayConfigIP, relevantHostIPs); err != nil {
			return err
		}
	}

	return err
}

func (oc *DefaultNetworkController) ensureNodeLogicalNetwork(node *kapi.Node, hostSubnets []*net.IPNet) error {
	var hostNetworkPolicyIPs []net.IP

	switchName := node.Name
	for _, hostSubnet := range hostSubnets {
		mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
		hostNetworkPolicyIPs = append(hostNetworkPolicyIPs, mgmtIfAddr.IP)
	}

	// also add the join switch IPs for this node - needed in shared gateway mode
	lrpIPs, err := oc.joinSwIPManager.EnsureJoinLRPIPs(switchName)
	if err != nil {
		return fmt.Errorf("failed to get join switch port IP address for switch %s: %v", switchName, err)
	}

	for _, lrpIP := range lrpIPs {
		hostNetworkPolicyIPs = append(hostNetworkPolicyIPs, lrpIP.IP)
	}

	// add the host network IPs for this node to host network namespace's address set
	if err = func() error {
		hostNetworkNamespace := config.Kubernetes.HostNetworkNamespace
		if hostNetworkNamespace != "" {
			nsInfo, nsUnlock, err := oc.ensureNamespaceLocked(hostNetworkNamespace, true, nil)
			if err != nil {
				return fmt.Errorf("failed to ensure namespace locked: %v", err)
			}
			defer nsUnlock()
			if err = nsInfo.addressSet.AddIPs(hostNetworkPolicyIPs); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		return err
	}

	return oc.createNodeLogicalSwitch(node.Name, hostSubnets, oc.loadBalancerGroupUUID)
}

func (oc *DefaultNetworkController) addNode(node *kapi.Node) ([]*net.IPNet, error) {
	gwLRPIPs, err := oc.joinSwIPManager.EnsureJoinLRPIPs(node.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate join switch port IP address for node %s: %v", node.Name, err)
	}
	var v4Addr, v6Addr *net.IPNet
	for _, ip := range gwLRPIPs {
		if ip.IP.To4() != nil {
			v4Addr = ip
		} else if ip.IP.To16() != nil {
			v6Addr = ip
		}
	}
	updatedNodeAnnotation, err := util.CreateNodeGatewayRouterLRPAddrAnnotation(nil, v4Addr, v6Addr)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal node %q annotation for Gateway LRP IP %v",
			node.Name, gwLRPIPs)
	}

	hostSubnets, err := oc.allocateNodeSubnets(node, oc.masterSubnetAllocator)
	if err != nil {
		return nil, err
	}

	hostSubnetsMap := map[string][]*net.IPNet{oc.GetNetworkName(): hostSubnets}
	err = oc.UpdateNodeHostSubnetAnnotationWithRetry(node.Name, hostSubnetsMap, updatedNodeAnnotation)
	if err != nil {
		return nil, err
	}

	// delete stale chassis in SBDB if any
	if err = oc.deleteStaleNodeChassis(node); err != nil {
		return nil, err
	}

	// Ensure that the node's logical network has been created. Note that if the
	// subsequent operation in addNode() fails, oc.lsManager.DeleteNode(node.Name)
	// needs to be done, otherwise, this node's IPAM will be overwritten and the
	// same IP could be allocated to multiple Pods scheduled on this node.
	err = oc.ensureNodeLogicalNetwork(node, hostSubnets)
	if err != nil {
		return nil, err
	}

	return hostSubnets, nil
}

// check if any existing chassis entries in the SBDB mismatches with node's chassisID annotation
func (oc *DefaultNetworkController) checkNodeChassisMismatch(node *kapi.Node) (string, error) {
	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		return "", nil
	}

	chassisList, err := libovsdbops.ListChassis(oc.sbClient)
	if err != nil {
		return "", fmt.Errorf("failed to get chassis list for node %s: error: %v", node.Name, err)
	}

	for _, chassis := range chassisList {
		if chassis.Hostname == node.Name && chassis.Name != chassisID {
			return chassis.Name, nil
		}
	}
	return "", nil
}

// delete stale chassis in SBDB if system-id of the specific node has changed.
func (oc *DefaultNetworkController) deleteStaleNodeChassis(node *kapi.Node) error {
	staleChassis, err := oc.checkNodeChassisMismatch(node)
	if err != nil {
		return fmt.Errorf("failed to check if there is any stale chassis for node %s in SBDB: %v", node.Name, err)
	} else if staleChassis != "" {
		klog.V(5).Infof("Node %s now has a new chassis ID, delete its stale chassis %s in SBDB", node.Name, staleChassis)
		p := func(item *sbdb.Chassis) bool {
			return item.Name == staleChassis
		}
		if err = libovsdbops.DeleteChassisWithPredicate(oc.sbClient, p); err != nil {
			// Send an event and Log on failure
			oc.recorder.Eventf(node, kapi.EventTypeWarning, "ErrorMismatchChassis",
				"Node %s is now with a new chassis ID. Its stale chassis entry is still in the SBDB",
				node.Name)
			return fmt.Errorf("node %s is now with a new chassis ID. Its stale chassis entry is still in the SBDB", node.Name)
		}
	}
	return nil
}

func (oc *DefaultNetworkController) deleteNode(nodeName string) error {
	oc.masterSubnetAllocator.ReleaseAllNodeSubnets(nodeName)

	if err := oc.deleteNodeLogicalNetwork(nodeName); err != nil {
		return fmt.Errorf("error deleting node %s logical network: %v", nodeName, err)
	}

	if err := oc.gatewayCleanup(nodeName); err != nil {
		return fmt.Errorf("failed to clean up node %s gateway: (%v)", nodeName, err)
	}

	if err := oc.joinSwIPManager.ReleaseJoinLRPIPs(nodeName); err != nil {
		return fmt.Errorf("failed to clean up GR LRP IPs for node %s: %v", nodeName, err)
	}

	p := func(item *sbdb.Chassis) bool {
		return item.Hostname == nodeName
	}
	if err := libovsdbops.DeleteChassisWithPredicate(oc.sbClient, p); err != nil {
		return fmt.Errorf("failed to remove the chassis associated with node %s in the OVN SB Chassis table: %v", nodeName, err)
	}
	return nil
}

// OVN uses an overlay and doesn't need GCE Routes, we need to
// clear the NetworkUnavailable condition that kubelet adds to initial node
// status when using GCE (done here: https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/cloud/node_controller.go#L237).
// See discussion surrounding this here: https://github.com/kubernetes/kubernetes/pull/34398.
// TODO: make upstream kubelet more flexible with overlays and GCE so this
// condition doesn't get added for network plugins that don't want it, and then
// we can remove this function.
func (oc *DefaultNetworkController) clearInitialNodeNetworkUnavailableCondition(origNode *kapi.Node) {
	// If it is not a Cloud Provider node, then nothing to do.
	if origNode.Spec.ProviderID == "" {
		return
	}

	cleared := false
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error

		oldNode, err := oc.watchFactory.GetNode(origNode.Name)
		if err != nil {
			return err
		}
		// Informer cache should not be mutated, so get a copy of the object
		node := oldNode.DeepCopy()

		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == kapi.NodeNetworkUnavailable {
				condition := &node.Status.Conditions[i]
				if condition.Status != kapi.ConditionFalse && condition.Reason == "NoRouteCreated" {
					condition.Status = kapi.ConditionFalse
					condition.Reason = "RouteCreated"
					condition.Message = "ovn-kube cleared kubelet-set NoRouteCreated"
					condition.LastTransitionTime = metav1.Now()
					if err = oc.kube.UpdateNodeStatus(node); err == nil {
						cleared = true
					}
				}
				break
			}
		}
		return err
	})
	if resultErr != nil {
		klog.Errorf("Status update failed for local node %s: %v", origNode.Name, resultErr)
	} else if cleared {
		klog.Infof("Cleared node NetworkUnavailable/NoRouteCreated condition for %s", origNode.Name)
	}
}

// this is the worker function that does the periodic sync of nodes from kube API
// and sbdb and deletes chassis that are stale
func (oc *DefaultNetworkController) syncNodesPeriodic() {
	//node names is a slice of all node names
	nodes, err := oc.kube.GetNodes()
	if err != nil {
		klog.Errorf("Error getting existing nodes from kube API: %v", err)
		return
	}

	nodeNames := make([]string, 0, len(nodes.Items))

	for _, node := range nodes.Items {
		nodeNames = append(nodeNames, node.Name)
	}

	chassisList, err := libovsdbops.ListChassis(oc.sbClient)
	if err != nil {
		klog.Errorf("Failed to get chassis list: error: %v", err)
		return
	}

	chassisHostNameMap := map[string]*sbdb.Chassis{}
	for _, chassis := range chassisList {
		chassisHostNameMap[chassis.Hostname] = chassis
	}

	//delete existing nodes from the chassis map.
	for _, nodeName := range nodeNames {
		delete(chassisHostNameMap, nodeName)
	}

	staleChassis := []*sbdb.Chassis{}
	for _, v := range chassisHostNameMap {
		staleChassis = append(staleChassis, v)
	}

	if err = libovsdbops.DeleteChassis(oc.sbClient, staleChassis...); err != nil {
		klog.Errorf("Failed Deleting chassis %v error: %v", chassisHostNameMap, err)
		return
	}
}

// We only deal with cleaning up nodes that shouldn't exist here, since
// watchNodes() will be called for all existing nodes at startup anyway.
// Note that this list will include the 'join' cluster switch, which we
// do not want to delete.
func (oc *DefaultNetworkController) syncNodes(nodes []interface{}) error {
	foundNodes := sets.NewString()
	for _, tmp := range nodes {
		node, ok := tmp.(*kapi.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", tmp)
		}
		hostSubnets := oc.updateNodesManageHostSubnets(node, oc.masterSubnetAllocator, foundNodes)
		if config.HybridOverlay.Enabled && len(hostSubnets) == 0 && houtil.IsHybridOverlayNode(node) {
			// this is a hybrid overlay node so mark as allocated from the hybrid overlay subnet allocator
			hostSubnet, err := houtil.ParseHybridOverlayHostSubnet(node)
			if err != nil {
				klog.Warning(err.Error())
			} else if hostSubnet != nil {
				klog.V(5).Infof("Node %s contains subnets: %v", node.Name, hostSubnet)
				if err := oc.hybridOverlaySubnetAllocator.MarkSubnetsAllocated(node.Name, hostSubnet); err != nil {
					utilruntime.HandleError(err)
				}
			}
			// there is nothing left to be done if this is a hybrid overlay node
			continue
		}
		// For each existing node, reserve its joinSwitch LRP IPs if they already exist.
		if _, err := oc.joinSwIPManager.EnsureJoinLRPIPs(node.Name); err != nil {
			// TODO (flaviof): keep going even if EnsureJoinLRPIPs returned an error. Maybe we should not.
			klog.Errorf("Failed to get join switch port IP address for node %s: %v", node.Name, err)
		}
	}

	defaultNetworkPredicate := func(item *nbdb.LogicalSwitch) bool {
		_, ok := item.ExternalIDs[types.NetworkExternalID]
		return len(item.OtherConfig) > 0 && !ok
	}
	nodeSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(oc.nbClient, defaultNetworkPredicate)
	if err != nil {
		return fmt.Errorf("failed to get node logical switches which have other-config set: %v", err)
	}
	for _, nodeSwitch := range nodeSwitches {
		if !foundNodes.Has(nodeSwitch.Name) {
			if err := oc.deleteNode(nodeSwitch.Name); err != nil {
				return fmt.Errorf("failed to delete node:%s, err:%v", nodeSwitch.Name, err)
			}
		}
	}

	// cleanup stale chassis with no corresponding nodes
	chassisList, err := libovsdbops.ListChassis(oc.sbClient)
	if err != nil {
		return fmt.Errorf("failed to get chassis list: %v", err)
	}

	knownChassisNames := sets.NewString()
	chassisDeleteList := []*sbdb.Chassis{}
	for _, chassis := range chassisList {
		knownChassisNames.Insert(chassis.Name)
		// skip chassis that have a corresponding node
		if foundNodes.Has(chassis.Hostname) {
			continue
		}
		chassisDeleteList = append(chassisDeleteList, chassis)
	}

	// cleanup stale chassis private with no corresponding chassis
	chassisPrivateList, err := libovsdbops.ListChassisPrivate(oc.sbClient)
	if err != nil {
		return fmt.Errorf("failed to get chassis private list: %v", err)
	}

	for _, chassis := range chassisPrivateList {
		// skip chassis private that have a corresponding chassis
		if knownChassisNames.Has(chassis.Name) {
			continue
		}
		// we add to the list what would be the corresponding Chassis. Even if
		// the Chassis does not exist in SBDB, DeleteChassis will remove the
		// ChassisPrivate.
		chassisDeleteList = append(chassisDeleteList, &sbdb.Chassis{Name: chassis.Name})
	}

	// Delete stale chassis and associated chassis private
	if err := libovsdbops.DeleteChassis(oc.sbClient, chassisDeleteList...); err != nil {
		return fmt.Errorf("failed deleting chassis %v: %v", chassisDeleteList, err)
	}
	return nil
}

// nodeSyncs structure contains flags for the different failures
// so the retry logic can control what need to retry based
type nodeSyncs struct {
	syncNode              bool
	syncClusterRouterPort bool
	syncMgmtPort          bool
	syncGw                bool
	syncHo                bool
}

func (oc *DefaultNetworkController) addUpdateNodeEvent(node *kapi.Node, nSyncs *nodeSyncs) error {
	var hostSubnets []*net.IPNet
	var errs []error
	var err error

	if noHostSubnet := noHostSubnet(node); noHostSubnet {
		err := oc.lsManager.AddNoHostSubnetSwitch(node.Name)
		if err != nil {
			return fmt.Errorf("nodeAdd: error adding noHost subnet for switch %s: %w", node.Name, err)
		}
		if config.HybridOverlay.Enabled && houtil.IsHybridOverlayNode(node) {
			annotator := kube.NewNodeAnnotator(oc.kube, node.Name)
			if _, err := oc.hybridOverlayNodeEnsureSubnet(node, annotator); err != nil {
				return fmt.Errorf("failed to update node %s hybrid overlay subnet annotation: %v", node.Name, err)
			}
			if err := annotator.Run(); err != nil {
				oc.releaseHybridOverlayNodeSubnet(node.Name)
				return fmt.Errorf(" failed to set hybrid overlay annotations for node %s: %v", node.Name, err)
			}
		}
		oc.clearInitialNodeNetworkUnavailableCondition(node)
		return nil
	}

	klog.Infof("Adding or Updating Node %q", node.Name)
	if nSyncs.syncNode {
		if hostSubnets, err = oc.addNode(node); err != nil {
			oc.addNodeFailed.Store(node.Name, true)
			oc.nodeClusterRouterPortFailed.Store(node.Name, true)
			oc.mgmtPortFailed.Store(node.Name, true)
			oc.gatewaysFailed.Store(node.Name, true)
			oc.hybridOverlayFailed.Store(node.Name, config.HybridOverlay.Enabled)
			err = fmt.Errorf("nodeAdd: error adding node %q: %w", node.Name, err)
			oc.recordNodeErrorEvent(node, err)
			return err
		}
		oc.addNodeFailed.Delete(node.Name)
	}

	// since the nodeSync objects are created knowing if hybridOverlay is enabled this should work
	if nSyncs.syncHo {
		if err = oc.allocateHybridOverlayDRIP(node); err != nil {
			errs = append(errs, err)
			oc.hybridOverlayFailed.Store(node.Name, true)
		} else {
			oc.hybridOverlayFailed.Delete(node.Name)
		}
	}

	if nSyncs.syncClusterRouterPort {
		if err = oc.syncNodeClusterRouterPort(node, nil); err != nil {
			errs = append(errs, err)
			oc.nodeClusterRouterPortFailed.Store(node.Name, true)
		} else {
			oc.nodeClusterRouterPortFailed.Delete(node.Name)
		}
	}

	if nSyncs.syncMgmtPort {
		err := oc.syncNodeManagementPort(node, hostSubnets)
		if err != nil {
			errs = append(errs, err)
			oc.mgmtPortFailed.Store(node.Name, true)
		} else {
			oc.mgmtPortFailed.Delete(node.Name)
		}
	}

	// delete stale chassis in SBDB if any
	if err := oc.deleteStaleNodeChassis(node); err != nil {
		errs = append(errs, err)
	}

	annotator := kube.NewNodeAnnotator(oc.kube, node.Name)
	if config.HybridOverlay.Enabled {
		if err := oc.handleHybridOverlayPort(node, annotator); err != nil {
			errs = append(errs, fmt.Errorf("failed to set up hybrid overlay logical switch port for %s: %v", node.Name, err))
		}
	} else {
		// the node needs to cleanup Hybrid overlay annotations if it has them and hybrid overlay is not enabled
		if _, exist := node.Annotations[hotypes.HybridOverlayDRMAC]; exist {
			annotator.Delete(hotypes.HybridOverlayDRMAC)
		}
		if _, exist := node.Annotations[hotypes.HybridOverlayDRIP]; exist {
			annotator.Delete(hotypes.HybridOverlayDRIP)
		}
	}
	if err := annotator.Run(); err != nil {
		errs = append(errs, fmt.Errorf("failed to set hybrid overlay annotations for node %s: %v", node.Name, err))
	}

	oc.clearInitialNodeNetworkUnavailableCondition(node)

	if nSyncs.syncGw {
		err := oc.syncNodeGateway(node, nil)
		if err != nil {
			errs = append(errs, err)
			oc.gatewaysFailed.Store(node.Name, true)
		} else {
			oc.gatewaysFailed.Delete(node.Name)
		}
	}

	// ensure pods that already exist on this node have their logical ports created
	// if per pod SNAT is being used, then l3 gateway config is required to be able to add pods
	if _, gwFailed := oc.gatewaysFailed.Load(node.Name); !gwFailed || !config.Gateway.DisableSNATMultipleGWs {
		if nSyncs.syncNode || nSyncs.syncGw { // do this only if it is a new node add or a gateway sync happened
			errors := oc.addAllPodsOnNode(node.Name)
			errs = append(errs, errors...)
		}
	}

	err = kerrors.NewAggregate(errs)
	if err != nil {
		oc.recordNodeErrorEvent(node, err)
	}
	return err
}

func (oc *DefaultNetworkController) deleteNodeEvent(node *kapi.Node) error {
	klog.V(5).Infof("Deleting Node %q. Removing the node from "+
		"various caches", node.Name)

	if config.HybridOverlay.Enabled {
		if noHostSubnet := noHostSubnet(node); noHostSubnet {
			// noHostSubnet nodes are different, only remove the switch and delete the hybrid overlay subnet
			oc.lsManager.DeleteSwitch(node.Name)
			oc.releaseHybridOverlayNodeSubnet(node.Name)
			return nil
		}
		if _, ok := node.Annotations[hotypes.HybridOverlayDRMAC]; ok && !houtil.IsHybridOverlayNode(node) {
			oc.deleteHybridOverlayPort(node)
		}
		if err := oc.removeHybridLRPolicySharedGW(node.Name); err != nil {
			return err
		}
	}
	if err := oc.deleteNode(node.Name); err != nil {
		return err
	}
	oc.lsManager.DeleteSwitch(node.Name)
	oc.addNodeFailed.Delete(node.Name)
	oc.mgmtPortFailed.Delete(node.Name)
	oc.gatewaysFailed.Delete(node.Name)
	oc.nodeClusterRouterPortFailed.Delete(node.Name)
	return nil
}

func (oc *DefaultNetworkController) createACLLoggingMeter() error {
	band := &nbdb.MeterBand{
		Action: types.MeterAction,
		Rate:   config.Logging.ACLLoggingRateLimit,
	}
	ops, err := libovsdbops.CreateMeterBandOps(oc.nbClient, nil, band)
	if err != nil {
		return fmt.Errorf("can't create meter band %v: %v", band, err)
	}

	meterFairness := true
	meter := &nbdb.Meter{
		Name: types.OvnACLLoggingMeter,
		Fair: &meterFairness,
		Unit: types.PacketsPerSecond,
	}
	ops, err = libovsdbops.CreateOrUpdateMeterOps(oc.nbClient, ops, meter, []*nbdb.MeterBand{band},
		&meter.Bands, &meter.Fair, &meter.Unit)
	if err != nil {
		return fmt.Errorf("can't create meter %v: %v", meter, err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("can't transact ACL logging meter: %v", err)
	}

	return nil
}

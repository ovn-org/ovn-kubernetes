package ovn

import (
	"fmt"
	"net"
	"strings"
	"time"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/pkg/errors"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
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

	pg := &nbdb.PortGroup{
		Name: types.ClusterPortGroupNameBase,
	}
	pg, err = libovsdbops.GetPortGroup(oc.nbClient, pg)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return err
	}
	if pg == nil {
		// we didn't find an existing clusterPG, let's create a new empty PG (fresh cluster install)
		// Create a cluster-wide port group that all logical switch ports are part of
		pg := oc.buildPortGroup(types.ClusterPortGroupNameBase, types.ClusterPortGroupNameBase, nil, nil)
		err = libovsdbops.CreateOrUpdatePortGroups(oc.nbClient, pg)
		if err != nil {
			klog.Errorf("Failed to create cluster port group: %v", err)
			return err
		}
	}

	pg = &nbdb.PortGroup{
		Name: types.ClusterRtrPortGroupNameBase,
	}
	pg, err = libovsdbops.GetPortGroup(oc.nbClient, pg)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return err
	}
	if pg == nil {
		// we didn't find an existing clusterRtrPG, let's create a new empty PG (fresh cluster install)
		// Create a cluster-wide port group with all node-to-cluster router
		// logical switch ports. Currently the only user is multicast but it might
		// be used for other features in the future.
		pg = oc.buildPortGroup(types.ClusterRtrPortGroupNameBase, types.ClusterRtrPortGroupNameBase, nil, nil)
		err = libovsdbops.CreateOrUpdatePortGroups(oc.nbClient, pg)
		if err != nil {
			klog.Errorf("Failed to create cluster port group: %v", err)
			return err
		}
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
	} else {
		if err = oc.disableMulticast(); err != nil {
			return fmt.Errorf("failed to delete default multicast policy, error: %v", err)
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

	// Connect the distributed router to OVNJoinSwitch.
	drSwitchPort := types.JoinSwitchToGWRouterPrefix + types.OVNClusterRouter
	drRouterPort := types.GWRouterToJoinSwitchPrefix + types.OVNClusterRouter

	gwLRPMAC := util.IPAddrToHWAddr(oc.ovnClusterLRPToJoinIfAddrs[0].IP)
	gwLRPNetworks := []string{}
	for _, gwLRPIfAddr := range oc.ovnClusterLRPToJoinIfAddrs {
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

		if err := oc.addAllowACLFromNode(node.Name, mgmtIfAddr.IP); err != nil {
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

	err = libovsdbops.AddPortsToPortGroup(oc.nbClient, types.ClusterPortGroupNameBase, logicalSwitchPort.UUID)
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
	hostSubnets []*net.IPNet, hostAddrs sets.Set[string]) error {
	var err error
	var gwLRPIPs, clusterSubnets []*net.IPNet
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
	}

	gwLRPIPs, err = util.ParseNodeGatewayRouterLRPAddrs(node)
	if err != nil {
		return fmt.Errorf("failed to get join switch port IP address for node %s: %v", node.Name, err)
	}

	enableGatewayMTU := util.ParseNodeGatewayMTUSupport(node)

	err = oc.gatewayInit(node.Name, clusterSubnets, hostSubnets, l3GatewayConfig, oc.SCTPSupport, gwLRPIPs, oc.ovnClusterLRPToJoinIfAddrs,
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
		relevantHostIPs, err := util.MatchAllIPStringFamily(utilnet.IsIPv6(hostIfAddr.IP), sets.List(hostAddrs))
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

	for _, hostSubnet := range hostSubnets {
		mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
		hostNetworkPolicyIPs = append(hostNetworkPolicyIPs, mgmtIfAddr.IP)
	}

	// also add the join switch IPs for this node - needed in shared gateway mode
	// Note: join switch IPs for each node are generated by cluster manager and
	// stored in the node annotation
	lrpIPs, err := util.ParseNodeGatewayRouterLRPAddrs(node)
	if err != nil {
		return fmt.Errorf("failed to get join switch port IP address for node %s: %v", node.Name, err)
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

	return oc.createNodeLogicalSwitch(node.Name, hostSubnets, oc.clusterLoadBalancerGroupUUID, oc.switchLoadBalancerGroupUUID)
}

func (oc *DefaultNetworkController) addNode(node *kapi.Node) ([]*net.IPNet, error) {
	// Node subnet for the default network is allocated by cluster manager.
	// Make sure that the node is allocated with the subnet before proceeding
	// to create OVN Northbound resources.
	hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
	if err != nil {
		return nil, err
	}

	// We expect one subnet per configured ClusterNetwork IP family.
	var haveV4, haveV6 bool
	for _, net := range hostSubnets {
		if !haveV4 {
			haveV4 = net.IP.To4() != nil
		}
		if !haveV6 {
			haveV6 = net.IP.To4() == nil
		}
	}
	if haveV4 != config.IPv4Mode || haveV6 != config.IPv6Mode {
		return nil, fmt.Errorf("failed to get expected host subnets for node %s; expected v4 %v have %v, expected v6 %v have %v",
			node.Name, config.IPv4Mode, haveV4, config.IPv6Mode, haveV6)
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
		if err = libovsdbops.DeleteChassisTemplateVar(oc.nbClient, &nbdb.ChassisTemplateVar{Chassis: staleChassis}); err != nil {
			// Send an event and Log on failure
			oc.recorder.Eventf(node, kapi.EventTypeWarning, "ErrorMismatchChassis",
				"Node %s is now with a new chassis ID. Its stale chassis template vars are still in the NBDB",
				node.Name)
			return fmt.Errorf("node %s is now with a new chassis ID. Its stale chassis template vars are still in the NBDB", node.Name)
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

// cleanupNodeResources deletes the node resources from the OVN Northbound database
func (oc *DefaultNetworkController) cleanupNodeResources(nodeName string) error {
	if err := oc.deleteNodeLogicalNetwork(nodeName); err != nil {
		return fmt.Errorf("error deleting node %s logical network: %v", nodeName, err)
	}

	if err := oc.gatewayCleanup(nodeName); err != nil {
		return fmt.Errorf("failed to clean up node %s gateway: (%w)", nodeName, err)
	}

	chassisTemplateVars := make([]*nbdb.ChassisTemplateVar, 0)
	p := func(item *sbdb.Chassis) bool {
		if item.Hostname == nodeName {
			chassisTemplateVars = append(chassisTemplateVars, &nbdb.ChassisTemplateVar{Chassis: item.Name})
			return true
		}
		return false
	}
	if err := libovsdbops.DeleteChassisWithPredicate(oc.sbClient, p); err != nil {
		return fmt.Errorf("failed to remove the chassis associated with node %s in the OVN SB Chassis table: %v", nodeName, err)
	}
	if err := libovsdbops.DeleteChassisTemplateVar(oc.nbClient, chassisTemplateVars...); err != nil {
		return fmt.Errorf("failed deleting chassis template variables for %s: %v", nodeName, err)
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
	kNodes, err := oc.watchFactory.GetNodes()
	if err != nil {
		klog.Errorf("Error getting existing nodes from kube API: %v", err)
		return
	}

	localZoneNodeNames := make([]string, 0, len(kNodes))
	remoteZoneNodeNames := make([]string, 0, len(kNodes))
	for i := range kNodes {
		if oc.isLocalZoneNode(kNodes[i]) {
			localZoneNodeNames = append(localZoneNodeNames, kNodes[i].Name)
		} else {
			remoteZoneNodeNames = append(remoteZoneNodeNames, kNodes[i].Name)
		}
	}

	if err := oc.syncChassis(localZoneNodeNames, remoteZoneNodeNames); err != nil {
		klog.Errorf("Failed to sync chassis: error: %v", err)
	}
}

// We only deal with cleaning up nodes that shouldn't exist here, since
// watchNodes() will be called for all existing nodes at startup anyway.
// Note that this list will include the 'join' cluster switch, which we
// do not want to delete.
func (oc *DefaultNetworkController) syncNodes(kNodes []interface{}) error {
	foundNodes := sets.New[string]()
	localZoneNodeNames := make([]string, 0, len(kNodes))
	remoteZoneKNodeNames := make([]string, 0, len(kNodes))
	for _, tmp := range kNodes {
		node, ok := tmp.(*kapi.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", tmp)
		}

		if config.HybridOverlay.Enabled && houtil.IsHybridOverlayNode(node) {
			continue
		}

		// Add the node to the foundNodes only if it belongs to the local zone.
		if oc.isLocalZoneNode(node) {
			foundNodes.Insert(node.Name)
			oc.localZoneNodes.Store(node.Name, true)
			localZoneNodeNames = append(localZoneNodeNames, node.Name)
		} else {
			remoteZoneKNodeNames = append(remoteZoneKNodeNames, node.Name)
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

	staleNodes := sets.NewString()
	for _, nodeSwitch := range nodeSwitches {
		if nodeSwitch.Name != types.TransitSwitch && !foundNodes.Has(nodeSwitch.Name) {
			staleNodes.Insert(nodeSwitch.Name)
		}
	}

	// Find stale external logical switches, based on well known prefix and node name
	lookupExtSwFunction := func(item *nbdb.LogicalSwitch) bool {
		nodeName := strings.TrimPrefix(item.Name, types.ExternalSwitchPrefix)
		if nodeName != item.Name && len(nodeName) > 0 && !foundNodes.Has(nodeName) {
			staleNodes.Insert(nodeName)
			return true
		}
		return false
	}
	_, err = libovsdbops.FindLogicalSwitchesWithPredicate(oc.nbClient, lookupExtSwFunction)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Warning("Failed trying to find stale external logical switches")
	}

	// Find stale gateway routers, based on well known prefix and node name
	lookupGwRouterFunction := func(item *nbdb.LogicalRouter) bool {
		nodeName := strings.TrimPrefix(item.Name, types.GWRouterPrefix)
		if nodeName != item.Name && len(nodeName) > 0 && !foundNodes.Has(nodeName) {
			staleNodes.Insert(nodeName)
			return true
		}
		return false
	}
	_, err = libovsdbops.FindLogicalRoutersWithPredicate(oc.nbClient, lookupGwRouterFunction)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Warning("Failed trying to find stale gateway routers")
	}

	// Cleanup stale nodes (including gateway routers and external logical switches)
	for _, staleNode := range staleNodes.UnsortedList() {
		if err := oc.cleanupNodeResources(staleNode); err != nil {
			return fmt.Errorf("failed to cleanup node resources:%s, err:%w", staleNode, err)
		}
	}

	if err := oc.syncChassis(localZoneNodeNames, remoteZoneKNodeNames); err != nil {
		return fmt.Errorf("failed to sync chassis: error: %v", err)
	}

	if config.OVNKubernetesFeature.EnableInterconnect {
		if err := oc.zoneChassisHandler.SyncNodes(kNodes); err != nil {
			return fmt.Errorf("zoneChassisHandler failed to sync nodes: error: %w", err)
		}

		if err := oc.zoneICHandler.SyncNodes(kNodes); err != nil {
			return fmt.Errorf("zoneICHandler failed to sync nodes: error: %w", err)
		}
	}

	return nil
}

// Cleanup stale chassis and chassis template variables with no
// corresponding nodes.
func (oc *DefaultNetworkController) syncChassis(localZoneNodeNames, remoteZoneNodeNames []string) error {
	chassisList, err := libovsdbops.ListChassis(oc.sbClient)
	if err != nil {
		return fmt.Errorf("failed to get chassis list: error: %v", err)
	}

	// Cleanup stale chassis private with no corresponding chassis
	chassisPrivateList, err := libovsdbops.ListChassisPrivate(oc.sbClient)
	if err != nil {
		return fmt.Errorf("failed to get chassis private list: %v", err)
	}

	templateVarList := []*nbdb.ChassisTemplateVar{}

	if oc.svcTemplateSupport {
		templateVarList, err = libovsdbops.ListTemplateVar(oc.nbClient)
		if err != nil {
			return fmt.Errorf("failed to get template var list: error: %w", err)
		}
	}

	chassisHostNameMap := map[string]*sbdb.Chassis{}
	chassisNameMap := map[string]*sbdb.Chassis{}
	for _, chassis := range chassisList {
		chassisHostNameMap[chassis.Hostname] = chassis
		chassisNameMap[chassis.Name] = chassis
	}

	for _, chassisPrivate := range chassisPrivateList {
		// Skip chassis private that have a corresponding chassis
		if _, ok := chassisNameMap[chassisPrivate.Name]; ok {
			continue
		}
		// We add to the map what would be the corresponding Chassis. Even if
		// the Chassis does not exist in SBDB, DeleteChassis will remove the
		// ChassisPrivate.
		chassisNameMap[chassisPrivate.Name] = &sbdb.Chassis{Name: chassisPrivate.Name}
	}

	templateChassisMap := map[string]*nbdb.ChassisTemplateVar{}
	for _, templateVar := range templateVarList {
		templateChassisMap[templateVar.Chassis] = templateVar
	}

	// Delete existing nodes from the chassis map.
	// Also delete existing templateVars from the template map.
	for _, nodeName := range localZoneNodeNames {
		if chassis, ok := chassisHostNameMap[nodeName]; ok {
			delete(chassisNameMap, chassis.Name)
			delete(chassisHostNameMap, chassis.Hostname)
			delete(templateChassisMap, chassis.Name)
		}
	}

	// Delete existing remote zone nodes from the chassis map, but not from the templateVars
	// as we need to cleanup chassisTemplateVars for the remote zone nodes
	for _, nodeName := range remoteZoneNodeNames {
		if chassis, ok := chassisHostNameMap[nodeName]; ok {
			delete(chassisNameMap, chassis.Name)
			delete(chassisHostNameMap, chassis.Hostname)
		}
	}

	staleChassis := make([]*sbdb.Chassis, 0, len(chassisHostNameMap))
	for _, chassis := range chassisNameMap {
		staleChassis = append(staleChassis, chassis)
	}

	staleChassisTemplateVars := make([]*nbdb.ChassisTemplateVar, 0, len(templateChassisMap))
	for _, template := range templateChassisMap {
		staleChassisTemplateVars = append(staleChassisTemplateVars, template)
	}

	if err := libovsdbops.DeleteChassis(oc.sbClient, staleChassis...); err != nil {
		return fmt.Errorf("failed Deleting chassis %v error: %v", chassisHostNameMap, err)
	}

	if err := libovsdbops.DeleteChassisTemplateVar(oc.nbClient, staleChassisTemplateVars...); err != nil {
		return fmt.Errorf("failed Deleting chassis template vars %v error: %v", chassisHostNameMap, err)
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
	syncZoneIC            bool
	syncMigratablePods    bool
}

func (oc *DefaultNetworkController) addUpdateLocalNodeEvent(node *kapi.Node, nSyncs *nodeSyncs) error {
	var hostSubnets []*net.IPNet
	var errs []error
	var err error

	_, _ = oc.localZoneNodes.LoadOrStore(node.Name, true)

	if noHostSubnet := util.NoHostSubnet(node); noHostSubnet {
		err := oc.lsManager.AddNoHostSubnetSwitch(node.Name)
		if err != nil {
			return fmt.Errorf("nodeAdd: error adding noHost subnet for switch %s: %w", node.Name, err)
		}
		if config.HybridOverlay.Enabled && houtil.IsHybridOverlayNode(node) {
			// Parse the hybrid overlay host subnet for the node to
			// make sure that cluster manager has allocated the subnet.
			if _, err := houtil.ParseHybridOverlayHostSubnet(node); err != nil {
				return err
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
			if nSyncs.syncZoneIC {
				oc.syncZoneICFailed.Store(node.Name, true)
			}
			return fmt.Errorf("nodeAdd: error adding node %q: %w", node.Name, err)
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

	if nSyncs.syncZoneIC && config.OVNKubernetesFeature.EnableInterconnect {
		// Call zone chassis handler's AddLocalZoneNode function to mark
		// this node's chassis record in Southbound db as a local zone chassis.
		// This is required when a node moves from a remote zone to local zone
		if err := oc.zoneChassisHandler.AddLocalZoneNode(node); err != nil {
			errs = append(errs, err)
			oc.syncZoneICFailed.Store(node.Name, true)
		} else {
			// Call zone IC handler's AddLocalZoneNode function to create
			// interconnect resources in the OVN Northbound db for this local zone node.
			if err := oc.zoneICHandler.AddLocalZoneNode(node); err != nil {
				errs = append(errs, err)
				oc.syncZoneICFailed.Store(node.Name, true)
			} else {
				oc.syncZoneICFailed.Delete(node.Name)
			}
		}
	}

	if nSyncs.syncMigratablePods {
		if err := oc.allocateSyncMigratablePodIPs(node); err != nil {
			errs = append(errs, err)
			oc.syncMigratablePodsFailed.Store(node.Name, true)
		} else {
			oc.syncMigratablePodsFailed.Delete(node.Name)
		}
	}
	return kerrors.NewAggregate(errs)
}

func (oc *DefaultNetworkController) addUpdateRemoteNodeEvent(node *kapi.Node, syncZoneIC bool) error {
	start := time.Now()
	// Check if the remote node is present in the local zone nodes.  If its present
	// it means it moved from this controller zone to other remote zone. Cleanup the node
	// from the local zone cache.
	_, present := oc.localZoneNodes.Load(node.Name)

	if present {
		klog.Infof("Node %q moved from the local zone %s to a remote zone %s. Cleaning the node resources", node.Name, oc.zone, util.GetNodeZone(node))
		if err := oc.cleanupNodeResources(node.Name); err != nil {
			return fmt.Errorf("error cleaning up the local resources for the remote node %s, err : %w", node.Name, err)
		}
		oc.localZoneNodes.Delete(node.Name)
	}

	var err error
	if syncZoneIC && config.OVNKubernetesFeature.EnableInterconnect {
		// Call zone chassis handler's AddRemoteZoneNode function to creates
		// the remote chassis for the remote zone node in the SB DB or mark
		// the entry as remote if it was local chassis earlier
		if err = oc.zoneChassisHandler.AddRemoteZoneNode(node); err != nil {
			err = fmt.Errorf("adding or updating remote node chassis %s failed, err - %w", node.Name, err)
			oc.syncZoneICFailed.Store(node.Name, true)
			return err
		}

		// Call zone IC handler's AddRemoteZoneNode function to create
		// interconnect resources in the OVN NBDB for this remote zone node.
		// Also, create the remote port binding in SBDB
		if err = oc.zoneICHandler.AddRemoteZoneNode(node); err != nil {
			err = fmt.Errorf("adding or updating remote node IC resources %s failed, err - %w", node.Name, err)
			oc.syncZoneICFailed.Store(node.Name, true)
		} else {
			oc.syncZoneICFailed.Delete(node.Name)
		}
	}
	klog.Infof("Creating Interconnect resources for node %v took: %s", node.Name, time.Since(start))
	return err
}

func (oc *DefaultNetworkController) deleteNodeEvent(node *kapi.Node) error {
	klog.V(5).Infof("Deleting Node %q. Removing the node from "+
		"various caches", node.Name)

	if config.HybridOverlay.Enabled {
		if noHostSubnet := util.NoHostSubnet(node); noHostSubnet {
			// noHostSubnet nodes are different, only remove the switch
			oc.lsManager.DeleteSwitch(node.Name)
			return nil
		}
		if _, ok := node.Annotations[hotypes.HybridOverlayDRMAC]; ok && !houtil.IsHybridOverlayNode(node) {
			oc.deleteHybridOverlayPort(node)
		}
		if err := oc.removeHybridLRPolicySharedGW(node.Name); err != nil {
			return err
		}
	}

	if err := oc.cleanupNodeResources(node.Name); err != nil {
		return err
	}

	if config.OVNKubernetesFeature.EnableInterconnect {
		if err := oc.zoneICHandler.DeleteNode(node); err != nil {
			return err
		}
		if !oc.isLocalZoneNode(node) {
			if err := oc.zoneChassisHandler.DeleteRemoteZoneNode(node); err != nil {
				return err
			}
		}
		oc.syncZoneICFailed.Delete(node.Name)
	}

	oc.lsManager.DeleteSwitch(node.Name)
	oc.addNodeFailed.Delete(node.Name)
	oc.mgmtPortFailed.Delete(node.Name)
	oc.gatewaysFailed.Delete(node.Name)
	oc.nodeClusterRouterPortFailed.Delete(node.Name)
	oc.localZoneNodes.Delete(node.Name)

	return nil
}

// getOVNClusterRouterPortToJoinSwitchIPs returns the IP addresses for the
// logical router port "GwRouterToJoinSwitchPrefix + OVNClusterRouter" from the
// config.Gateway.V4JoinSubnet and  config.Gateway.V6JoinSubnet. This will
// always be the first IP from these subnets.
func (oc *DefaultNetworkController) getOVNClusterRouterPortToJoinSwitchIfAddrs() (gwLRPIPs []*net.IPNet, err error) {
	joinSubnetsConfig := []string{}
	if config.IPv4Mode {
		joinSubnetsConfig = append(joinSubnetsConfig, config.Gateway.V4JoinSubnet)
	}
	if config.IPv6Mode {
		joinSubnetsConfig = append(joinSubnetsConfig, config.Gateway.V6JoinSubnet)
	}
	for _, joinSubnetString := range joinSubnetsConfig {
		_, joinSubnet, err := net.ParseCIDR(joinSubnetString)
		if err != nil {
			return nil, fmt.Errorf("error parsing join subnet string %s: %v", joinSubnetString, err)
		}
		joinSubnetBaseIP := utilnet.BigForIP(joinSubnet.IP)
		ipnet := &net.IPNet{
			IP:   utilnet.AddIPOffset(joinSubnetBaseIP, 1),
			Mask: joinSubnet.Mask,
		}
		gwLRPIPs = append(gwLRPIPs, ipnet)
	}

	return gwLRPIPs, nil
}
func (oc *DefaultNetworkController) allocateSyncMigratablePodIPs(node *kapi.Node) error {
	allocatePodIPsOnSwitchWrapFn := func(liveMigratablePod *kapi.Pod, liveMigratablePodAnnotation *util.PodAnnotation, switchName, nadName string) (string, error) {
		return oc.allocatePodIPsOnSwitch(liveMigratablePod, liveMigratablePodAnnotation, switchName, nadName)
	}
	return kubevirt.AllocateSyncMigratablePodsIPsOnNode(oc.watchFactory, oc.lsManager, node.Name, ovntypes.DefaultNetworkName, allocatePodIPsOnSwitchWrapFn)
}

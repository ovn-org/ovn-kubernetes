// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package zoneinterconnect

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	lportTypeRouter     = "router"
	lportTypeRouterAddr = "router"
	lportTypeRemote     = "remote"

	BaseTransitSwitchTunnelKey = 16711683
)

/*
 * ZoneInterconnectHandler manages OVN resources required for interconnecting
 * multiple zones. This handler exposes functions which a network controller
 * (default and UDN) is expected to call on different events.

 * For routed topologies:
 *
 * AddLocalZoneNode(node) should be called if the node 'node' is a local zone node.
 * AddRemoteZoneNode(node) should be called if the node 'node' is a remote zone node.
 * Zone Interconnect Handler first creates a transit switch with the name - <network_name>+ "_" + types.TransitSwitch
 * if it is still not present.
 *
 * Local zone node handling
 * ------------------------
 * When network controller calls AddLocalZoneNode(ovn-worker)
 *    -  A logical switch port - router port pair is created connecting the ovn_cluster_router
 *       to the transit switch.
 *    -  Node annotation - k8s.ovn.org/ovn-node-transit-switch-port-ifaddr value is used
 *       as the logical router port address
 *
 * When network controller calls AddRemoteZoneNode(ovn-worker3)
 *    - A logical switch port of type "remote" is created in OVN Northbound transit_switch
 *      for the node ovn-worker3
 *    - A static route {IPPrefix: "ovn-worker3_subnet", Nexthop: "ovn-worker3_transit_port_ip"} is
 *      added in the ovn_cluster_router.
 *    - For the default network, additional static route
 *      {IPPrefix: "ovn-worker3_gw_router_port_host_ip", Nexthop: "ovn-worker3_transit_port_ip"} is
 *      added in the ovn_cluster_router
 *    - The corresponding port binding row in OVN Southbound DB for this logical port
 *      is manually bound to the remote OVN Southbound DB Chassis "ovn-worker3"
 *
 * -----------------------------------------------------------------------------------------------------
 * $ ovn-nbctl show ovn_cluster_router (on ovn-worker zone DB)
 *   router ovn_cluster_router
 *   ...
 *   port rtots-ovn-worker
 *      mac: "0a:58:a8:fe:00:08"
 *      networks: ["100.88.0.8/16", "fd97::8/64"]
 *
 * $ ovn-nbctl show transit_switch
 *     port tstor-ovn-worker
 *        type: router
 *        router-port: rtots-ovn-worker
 *     port tstor-ovn-worker3
 *        type: remote
 *        addresses: ["0a:58:a8:fe:00:02 100.88.0.2/16 fd97::2/64"]
 *
 * $ ovn-nbctl lr-route-list ovn_cluster_router
 *    IPv4 Routes
 *    Route Table <main>:
 *    ...
 *    ...
 *    10.244.0.0/24 (ovn-worker3 subnet)            100.88.0.2 (ovn-worker3 transit switch port ip) dst-ip
 *    100.64.0.2/32 (ovn-worker3 gw router port ip) 100.88.0.2 dst-ip
 *    ...
 *    IPv6 Routes
 *    Route Table <main>:
 *    ...
 *    ...
 *    fd00:10:244:1::/64 (ovn-worker3 subnet)       fd97::2 (ovn-worker3 transit switch port ip) dst-ip
 *    fd98::2 (ovn-worker3 gw router port ip)       fd97::2 dst-ip
 *    ...
 *
 * $ ovn-sbctl show
 *     ...
 *     Chassis "c391c626-e1f0-4b1e-af0b-66f0807f9495"
 *     hostname: ovn-worker3 (Its a remote chassis entry on which tstor-ovn-worker3 is bound)
 *     Encap geneve
 *         ip: "10.89.0.26"
 *         options: {csum="true"}
 *     Port_Binding tstor-ovn-worker3
 *
 * -----------------------------------------------------------------------------------------------------
 *
 *
 * For single switch flat topologies that require transit accross nodes:
 *
 * AddTransitSwitchConfig will add to the switch the specific transit config
 * AddTransitPortConfig will add to the local or remote port the specific transit config
 * BindTransitRemotePort will bind the remote port to the remote chassis
 *
 *
 * Note that the Chassis entry for each remote zone node is created by ZoneChassisHandler
 *
 */

// ZoneInterconnectHandler creates the OVN resources required for interconnecting
// multiple zones for a network (default or layer 3) UDN
type ZoneInterconnectHandler struct {
	watchFactory *factory.WatchFactory
	// network which is inter-connected
	util.NetInfo
	nbClient libovsdbclient.Client
	sbClient libovsdbclient.Client
	// ovn_cluster_router name for the network
	networkClusterRouterName string
	// transit switch name for the network
	networkTransitSwitchName string
}

// NewZoneInterconnectHandler returns a new ZoneInterconnectHandler object
func NewZoneInterconnectHandler(nInfo util.NetInfo, nbClient, sbClient libovsdbclient.Client, watchFactory *factory.WatchFactory) *ZoneInterconnectHandler {
	zic := &ZoneInterconnectHandler{
		NetInfo:      nInfo,
		nbClient:     nbClient,
		sbClient:     sbClient,
		watchFactory: watchFactory,
	}

	zic.networkClusterRouterName = zic.GetNetworkScopedName(types.OVNClusterRouter)
	zic.networkTransitSwitchName = getTransitSwitchName(nInfo)
	return zic
}

func getTransitSwitchName(nInfo util.NetInfo) string {
	switch nInfo.TopologyType() {
	case types.Layer2Topology:
		return nInfo.GetNetworkScopedName(types.OVNLayer2Switch)
	default:
		return nInfo.GetNetworkScopedName(types.TransitSwitch)
	}
}

func (zic *ZoneInterconnectHandler) createOrUpdateTransitSwitch(networkID int) error {
	externalIDs := make(map[string]string)
	if zic.IsUserDefinedNetwork() {
		externalIDs = getUserDefinedNetTransitSwitchExtIDs(zic.GetNetworkName(), zic.TopologyType(), zic.IsPrimaryNetwork())
	}
	ts := &nbdb.LogicalSwitch{
		Name:        zic.networkTransitSwitchName,
		ExternalIDs: externalIDs,
	}
	zic.addTransitSwitchConfig(ts, BaseTransitSwitchTunnelKey+networkID)
	// Create transit switch if it doesn't exist
	if err := libovsdbops.CreateOrUpdateLogicalSwitch(zic.nbClient, ts); err != nil {
		return fmt.Errorf("failed to create/update transit switch %s: %w", zic.networkTransitSwitchName, err)
	}
	return nil
}

// ensureTransitSwitch sets up the global transit switch required for interoperability with other zones
// Must wait for network id to be annotated to any node by cluster manager
func (zic *ZoneInterconnectHandler) ensureTransitSwitch() error {
	start := time.Now()

	// Get the transit switch. If its not present no cleanup to do
	ts := &nbdb.LogicalSwitch{
		Name: zic.networkTransitSwitchName,
	}

	_, err := libovsdbops.GetLogicalSwitch(zic.nbClient, ts)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return err
	}

	// Create the transit switch if it doesn't exist
	if err := zic.createOrUpdateTransitSwitch(zic.GetNetworkID()); err != nil {
		return err
	}

	klog.Infof("Time taken to create transit switch: %s", time.Since(start))

	return nil
}

// AddLocalZoneNode creates the interconnect resources in OVN NB DB for the local zone node.
// See createLocalZoneNodeResources() below for more details.
func (zic *ZoneInterconnectHandler) AddLocalZoneNode(node *corev1.Node) error {
	klog.Infof("Creating interconnect resources for local zone node %s for the network %s", node.Name, zic.GetNetworkName())
	nodeID, _ := util.GetNodeID(node)
	if nodeID == -1 {
		// Don't consider this node as cluster-manager has not allocated node id yet.
		return fmt.Errorf("failed to get node id for node - %s", node.Name)
	}

	if err := zic.ensureTransitSwitch(); err != nil {
		return fmt.Errorf("ensuring transit switch for local zone node %s for the network %s failed : err - %w", node.Name, zic.GetNetworkName(), err)
	}

	if err := zic.createLocalZoneNodeResources(node, nodeID); err != nil {
		return fmt.Errorf("creating interconnect resources for local zone node %s for the network %s failed : err - %w", node.Name, zic.GetNetworkName(), err)
	}

	return nil
}

// AddRemoteZoneNode creates the interconnect resources in OVN NBDB and SBDB for the remote zone node.
// // See createRemoteZoneNodeResources() below for more details.
func (zic *ZoneInterconnectHandler) AddRemoteZoneNode(node *corev1.Node) error {
	start := time.Now()

	nodeID, _ := util.GetNodeID(node)
	if nodeID == -1 {
		// Don't consider this node as cluster-manager has not allocated node id yet.
		return fmt.Errorf("failed to get node id for node - %s", node.Name)
	}

	nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node, zic.GetNetworkName())
	if err != nil {
		err = fmt.Errorf("failed to parse node %s subnets annotation %w", node.Name, err)
		if util.IsAnnotationNotSetError(err) {
			// remote node may not have the annotation yet, suppress it
			return types.NewSuppressedError(err)
		}
		return err
	}

	nodeTransitSwitchPortIPs, err := util.ParseNodeTransitSwitchPortAddrs(node)
	if err != nil || len(nodeTransitSwitchPortIPs) == 0 {
		err = fmt.Errorf("failed to get the node transit switch port IP addresses : %w", err)
		if util.IsAnnotationNotSetError(err) {
			return types.NewSuppressedError(err)
		}
		return err
	}

	var nodeGRPIPs []*net.IPNet
	// only primary networks have cluster router connected to join switch+GR
	// used for adding routes to GR
	if !zic.IsUserDefinedNetwork() || (util.IsNetworkSegmentationSupportEnabled() && zic.IsPrimaryNetwork()) {
		nodeGRPIPs, err = udn.GetGWRouterIPs(node, zic.GetNetInfo())
		if err != nil {
			return fmt.Errorf("failed to get the gateway router port IP addresses for node %s: %w", node.Name, err)
		}
	}

	if err := zic.ensureTransitSwitch(); err != nil {
		return fmt.Errorf("ensuring transit switch for remote zone node %s for the network %s failed : err - %w", node.Name, zic.GetNetworkName(), err)
	}

	klog.Infof("Creating interconnect resources for remote zone node %s for the network %s", node.Name, zic.GetNetworkName())

	if err := zic.createRemoteZoneNodeResources(node, nodeID, nodeTransitSwitchPortIPs, nodeSubnets, nodeGRPIPs); err != nil {
		return fmt.Errorf("creating interconnect resources for remote zone node %s for the network %s failed : err - %w", node.Name, zic.GetNetworkName(), err)
	}
	klog.Infof("Creating Interconnect resources for node %q on network %q took: %s", node.Name, zic.GetNetworkName(), time.Since(start))
	return nil
}

// DeleteNode deletes the local zone node or remote zone node resources
func (zic *ZoneInterconnectHandler) DeleteNode(node *corev1.Node) error {
	klog.Infof("Deleting interconnect resources for the node %s for the network %s", node.Name, zic.GetNetworkName())

	return zic.cleanupNode(node.Name)
}

// CleanupStaleNodes cleans up the interconnect resources for stale nodes.
func (zic *ZoneInterconnectHandler) CleanupStaleNodes(objs []interface{}) error {
	// Build set of current node names
	foundNodeNames := sets.New[string]()
	for _, obj := range objs {
		node, ok := obj.(*corev1.Node)
		if !ok {
			return fmt.Errorf("spurious object in CleanupStaleNodes: %v", obj)
		}
		foundNodeNames.Insert(node.Name)
	}
	staleNodeNames := sets.New[string]()

	// Get the transit switch
	ts := &nbdb.LogicalSwitch{
		Name: zic.networkTransitSwitchName,
	}
	ts, err := libovsdbops.GetLogicalSwitch(zic.nbClient, ts)
	if err == nil {
		// Transit switch exists - find stale nodes by checking transit switch ports
		for _, p := range ts.Ports {
			lp := &nbdb.LogicalSwitchPort{
				UUID: p,
			}

			lp, err := libovsdbops.GetLogicalSwitchPort(zic.nbClient, lp)
			if err != nil {
				continue
			}

			if lp.ExternalIDs == nil {
				continue
			}

			lportNode := lp.ExternalIDs["node"]
			if lportNode != "" && !foundNodeNames.Has(lportNode) {
				staleNodeNames.Insert(lportNode)
			}
		}
	} else if errors.Is(err, libovsdbclient.ErrNotFound) {
		// Transit switch doesn't exist - discover nodes from cluster router resources
		lr := &nbdb.LogicalRouter{Name: zic.networkClusterRouterName}
		lr, err = libovsdbops.GetLogicalRouter(zic.nbClient, lr)
		if err != nil {
			if !errors.Is(err, libovsdbclient.ErrNotFound) {
				return fmt.Errorf("failed to get cluster router: %w", err)
			}
			// Router doesn't exist, nothing to cleanup
			return nil
		}

		// Discover remote zone nodes from static routes with ic-node external ID
		p := func(route *nbdb.LogicalRouterStaticRoute) bool {
			return route.ExternalIDs != nil && route.ExternalIDs["ic-node"] != ""
		}
		routes, err := libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(zic.nbClient, lr, p)
		if err != nil {
			return fmt.Errorf("failed to get static routes for cluster router: %w", err)
		}

		for _, route := range routes {
			nodeName := route.ExternalIDs["ic-node"]
			if nodeName != "" && !foundNodeNames.Has(nodeName) {
				staleNodeNames.Insert(nodeName)
			}
		}

		// Discover local zone nodes from router ports connecting to transit switch
		routerPortPrefix := zic.GetNetworkScopedName(types.RouterToTransitSwitchPrefix)
		for _, portUUID := range lr.Ports {
			lrp, err := libovsdbops.GetLogicalRouterPort(zic.nbClient, &nbdb.LogicalRouterPort{UUID: portUUID})
			if err != nil {
				continue
			}
			// Extract node name from port name (e.g., "rtots-node1" -> "node1")
			if nodeName, found := strings.CutPrefix(lrp.Name, routerPortPrefix); found {
				if nodeName != "" && !foundNodeNames.Has(nodeName) {
					staleNodeNames.Insert(nodeName)
				}
			}
		}
	} else {
		// Unexpected error
		return fmt.Errorf("unexpected error while getting transit switch: %w", err)
	}

	// Cleanup stale interconnect resources
	for _, staleNodeName := range staleNodeNames.UnsortedList() {
		if err := zic.cleanupNode(staleNodeName); err != nil {
			klog.Errorf("Failed to cleanup the interconnect resources from OVN Northbound db for the stale node %s: %v", staleNodeName, err)
		}
	}

	return nil
}

// Cleanup deletes all interconnect resources for the network, including all node resources
// (ports, router ports, static routes) and the transit switch itself. This method is idempotent
// and safe to call multiple times.
func (zic *ZoneInterconnectHandler) Cleanup() error {
	klog.Infof("Cleaning up all interconnect resources for network %s", zic.GetNetworkName())

	// First cleanup all node resources (ports, routes, etc.)
	// Passing nil removes all nodes from the transit switch
	if err := zic.CleanupStaleNodes(nil); err != nil {
		return fmt.Errorf("failed to cleanup node resources: %w", err)
	}

	// Then delete the transit switch
	klog.Infof("Deleting the transit switch %s for the network %s", zic.networkTransitSwitchName, zic.GetNetworkName())
	if err := libovsdbops.DeleteLogicalSwitch(zic.nbClient, zic.networkTransitSwitchName); err != nil &&
		!errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete transit switch: %w", err)
	}
	return nil
}

// AddTransitSwitchConfig is only used by the layer2 network controller
func (zic *ZoneInterconnectHandler) AddTransitSwitchConfig(sw *nbdb.LogicalSwitch, tunnelKey int) error {
	if zic.TopologyType() != types.Layer2Topology {
		return nil
	}

	zic.addTransitSwitchConfig(sw, tunnelKey)
	return nil
}

func (zic *ZoneInterconnectHandler) AddTransitPortConfig(remote bool, podAnnotation *util.PodAnnotation, port *nbdb.LogicalSwitchPort) error {
	if zic.TopologyType() != types.Layer2Topology {
		return nil
	}

	// make sure we have a good ID
	if podAnnotation.TunnelID == 0 {
		return fmt.Errorf("invalid id %d for port %s", podAnnotation.TunnelID, port.Name)
	}

	if port.Options == nil {
		port.Options = map[string]string{}
	}
	port.Options[libovsdbops.RequestedTnlKey] = strconv.Itoa(podAnnotation.TunnelID)

	if remote {
		port.Type = lportTypeRemote
	}

	return nil
}

func (zic *ZoneInterconnectHandler) addTransitSwitchConfig(sw *nbdb.LogicalSwitch, tunnelKey int) {
	if sw.OtherConfig == nil {
		sw.OtherConfig = map[string]string{}
	}

	sw.OtherConfig["interconn-ts"] = sw.Name
	sw.OtherConfig[libovsdbops.RequestedTnlKey] = strconv.Itoa(tunnelKey)
	sw.OtherConfig["mcast_snoop"] = "true"
	sw.OtherConfig["mcast_querier"] = "false"
	sw.OtherConfig["mcast_flood_unregistered"] = "true"
}

// createLocalZoneNodeResources creates the local zone node resources for interconnect
//   - creates a logical switch port of type "router" in the transit switch with the name as - <network_name>.tstor-<node_name>
//     Eg. if the node name is ovn-worker and the network is default, the name would be - tstor-ovn-worker
//     if the node name is ovn-worker and the network name is blue, the logical port name would be - blue.tstor-ovn-worker
//   - creates a logical router port in the ovn_cluster_router with the name - <network_name>.rtots-<node_name> and connects
//     to the node logical switch port in the transit switch
//   - remove any stale static routes in the ovn_cluster_router for the node
func (zic *ZoneInterconnectHandler) createLocalZoneNodeResources(node *corev1.Node, nodeID int) error {
	nodeTransitSwitchPortIPs, err := util.ParseNodeTransitSwitchPortAddrs(node)
	if err != nil || len(nodeTransitSwitchPortIPs) == 0 {
		return fmt.Errorf("failed to get the node transit switch port ips for node %s: %w", node.Name, err)
	}

	transitRouterPortMac := util.IPAddrToHWAddr(nodeTransitSwitchPortIPs[0].IP)
	var transitRouterPortNetworks []string
	for _, ip := range nodeTransitSwitchPortIPs {
		transitRouterPortNetworks = append(transitRouterPortNetworks, ip.String())
	}

	// Connect transit switch to the cluster router by creating a pair of logical switch port - logical router port
	logicalRouterPortName := zic.GetNetworkScopedName(types.RouterToTransitSwitchPrefix + node.Name)
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     logicalRouterPortName,
		MAC:      transitRouterPortMac.String(),
		Networks: transitRouterPortNetworks,
		Options: map[string]string{
			"mcast_flood": "true",
		},
	}
	logicalRouter := nbdb.LogicalRouter{
		Name: zic.networkClusterRouterName,
	}

	if err := libovsdbops.CreateOrUpdateLogicalRouterPort(zic.nbClient, &logicalRouter, &logicalRouterPort, nil); err != nil {
		return fmt.Errorf("failed to create/update cluster router %s to add transit switch port %s for the node %s: %w", zic.networkClusterRouterName, logicalRouterPortName, node.Name, err)
	}

	lspOptions := map[string]string{
		libovsdbops.RouterPort:      logicalRouterPortName,
		libovsdbops.RequestedTnlKey: strconv.Itoa(nodeID),
	}

	// Store the node name in the external_ids column for book keeping
	externalIDs := map[string]string{
		"node": node.Name,
	}
	err = zic.addNodeLogicalSwitchPort(zic.networkTransitSwitchName, zic.GetNetworkScopedName(types.TransitSwitchToRouterPrefix+node.Name),
		lportTypeRouter, []string{lportTypeRouterAddr}, lspOptions, externalIDs)
	if err != nil {
		return err
	}

	// Its possible that node is moved from a remote zone to the local zone. Check and delete the remote zone routes
	// for this node as it's no longer needed.
	return zic.deleteLocalNodeStaticRoutes(node, nodeTransitSwitchPortIPs)
}

// createRemoteZoneNodeResources creates the remote zone node resources
//   - creates a logical port of type "remote" in the transit switch with the name as - <network_name>.tstor.<node_name>
//     Eg. if the node name is ovn-worker and the network is default, the name would be - tstor.ovn-worker
//     if the node name is ovn-worker and the network name is blue, the logical port name would be - blue.tstor.ovn-worker
//   - binds the remote port to the node remote chassis in SBDB
//   - adds static routes for the remote node via the remote port ip in the ovn_cluster_router
func (zic *ZoneInterconnectHandler) createRemoteZoneNodeResources(node *corev1.Node, nodeID int, nodeTransitSwitchPortIPs, nodeSubnets, nodeGRPIPs []*net.IPNet) error {
	transitRouterPortMac := util.IPAddrToHWAddr(nodeTransitSwitchPortIPs[0].IP)
	var transitRouterPortNetworks []string
	for _, ip := range nodeTransitSwitchPortIPs {
		transitRouterPortNetworks = append(transitRouterPortNetworks, ip.String())
	}

	remotePortAddr := transitRouterPortMac.String()
	for _, tsNetwork := range transitRouterPortNetworks {
		remotePortAddr = remotePortAddr + " " + tsNetwork
	}

	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// remote node may not have the annotation yet, suppress it
			return types.NewSuppressedError(err)
		}
		return fmt.Errorf("failed to parse node chassis-id for node %s: %w", node.Name, err)
	}

	lspOptions := map[string]string{
		libovsdbops.RequestedTnlKey:  strconv.Itoa(nodeID),
		libovsdbops.RequestedChassis: chassisID,
	}
	// Store the node name in the external_ids column for book keeping
	externalIDs := map[string]string{
		"node": node.Name,
	}

	remotePortName := zic.GetNetworkScopedName(types.TransitSwitchToRouterPrefix + node.Name)
	if err := zic.addNodeLogicalSwitchPort(zic.networkTransitSwitchName, remotePortName, lportTypeRemote, []string{remotePortAddr}, lspOptions, externalIDs); err != nil {
		return err
	}

	if err := zic.deleteStaleStaticRoutes(node, nodeTransitSwitchPortIPs, nodeSubnets, nodeGRPIPs); err != nil {
		return err
	}

	if err := zic.addRemoteNodeStaticRoutes(node, nodeTransitSwitchPortIPs, nodeSubnets, nodeGRPIPs); err != nil {
		return err
	}

	// Cleanup the logical router port connecting to the transit switch for the remote node (if present)
	// Cleanup would be required when a local zone node moves to a remote zone.
	return zic.cleanupNodeClusterRouterPort(node.Name)
}

func (zic *ZoneInterconnectHandler) addNodeLogicalSwitchPort(logicalSwitchName, portName, portType string, addresses []string, options, externalIDs map[string]string) error {
	logicalSwitch := nbdb.LogicalSwitch{
		Name: logicalSwitchName,
	}

	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:        portName,
		Type:        portType,
		Options:     options,
		Addresses:   addresses,
		ExternalIDs: externalIDs,
	}
	if err := libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(zic.nbClient, &logicalSwitch, &logicalSwitchPort); err != nil {
		return fmt.Errorf("failed to add logical port %s to switch %s, error: %w", portName, logicalSwitch.Name, err)
	}
	return nil
}

// cleanupNode cleansup the local zone node or remote zone node resources
func (zic *ZoneInterconnectHandler) cleanupNode(nodeName string) error {
	klog.Infof("Cleaning up interconnect resources for the node %s for the network %s", nodeName, zic.GetNetworkName())

	// Cleanup the logical router port in the cluster router for the node
	// if it exists.
	if err := zic.cleanupNodeClusterRouterPort(nodeName); err != nil {
		return err
	}

	// Cleanup the logical switch port in the transit switch for the node
	// if it exists.
	if err := zic.cleanupNodeTransitSwitchPort(nodeName); err != nil {
		return err
	}

	// Delete any static routes in the cluster router for this node.
	// skip types.NetworkExternalID check in the predicate function as this static route may be deleted
	// before types.NetworkExternalID external-ids is set correctly during upgrade.
	p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
		return lrsr.ExternalIDs["ic-node"] == nodeName
	}
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(zic.nbClient, zic.networkClusterRouterName, p); err != nil {
		return fmt.Errorf("failed to cleanup static routes for the node %s: %w", nodeName, err)
	}

	return nil
}

func (zic *ZoneInterconnectHandler) cleanupNodeClusterRouterPort(nodeName string) error {
	lrp := nbdb.LogicalRouterPort{
		Name: zic.GetNetworkScopedName(types.RouterToTransitSwitchPrefix + nodeName),
	}
	logicalRouterPort, err := libovsdbops.GetLogicalRouterPort(zic.nbClient, &lrp)
	if err != nil {
		// logical router port doesn't exist. So nothing to cleanup.
		return nil
	}

	logicalRouter := nbdb.LogicalRouter{
		Name: zic.networkClusterRouterName,
	}

	if err := libovsdbops.DeleteLogicalRouterPorts(zic.nbClient, &logicalRouter, logicalRouterPort); err != nil {
		return fmt.Errorf("failed to delete logical router port %s from router %s for the node %s, error: %w", logicalRouterPort.Name, zic.networkClusterRouterName, nodeName, err)
	}

	return nil
}

func (zic *ZoneInterconnectHandler) cleanupNodeTransitSwitchPort(nodeName string) error {
	logicalSwitch := &nbdb.LogicalSwitch{
		Name: zic.networkTransitSwitchName,
	}
	logicalSwitchPort := &nbdb.LogicalSwitchPort{
		Name: zic.GetNetworkScopedName(types.TransitSwitchToRouterPrefix + nodeName),
	}

	if err := libovsdbops.DeleteLogicalSwitchPorts(zic.nbClient, logicalSwitch, logicalSwitchPort); err != nil {
		return fmt.Errorf("failed to delete logical switch port %s from transit switch %s for the node %s, error: %w", logicalSwitchPort.Name, zic.networkTransitSwitchName, nodeName, err)
	}
	return nil
}

// addRemoteNodeStaticRoutes adds static routes in ovn_cluster_router to reach the remote node via the
// remote node transit switch port.
// Eg. if node ovn-worker2 is a remote node
// ovn-worker2 - { node_subnet = 10.244.0.0/24,  node id = 2,  transit switch port ip = 100.88.0.2/16,  join ip connecting to GR_ovn-worker = 100.64.0.2/16}
// Then the below static routes are added
// ip4.dst == 10.244.0.0/24 , nexthop = 100.88.0.2
// ip4.dst == 100.64.0.2/16 , nexthop = 100.88.0.2  (only for default primary network)
func (zic *ZoneInterconnectHandler) addRemoteNodeStaticRoutes(node *corev1.Node, nodeTransitSwitchPortIPs, nodeSubnets, nodeGRPIPs []*net.IPNet) error {
	ops := make([]ovsdb.Operation, 0, 2)
	addRoute := func(prefix, nexthop string) error {
		logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
			ExternalIDs: map[string]string{
				"ic-node":               node.Name,
				types.NetworkExternalID: zic.GetNetworkName(),
			},
			Nexthop:  nexthop,
			IPPrefix: prefix,
		}
		// Note that because logical router static routes were originally created without types.NetworkExternalID
		// external-ids, skip types.NetworkExternalID check in the predicate function to replace existing static route
		// with correct external-ids on an upgrade scenario.
		p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
			return lrsr.IPPrefix == prefix
		}
		var err error
		ops, err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicateOps(zic.nbClient, ops, zic.networkClusterRouterName, &logicalRouterStaticRoute, p)
		if err != nil {
			return fmt.Errorf("failed to create static route ops: %w", err)
		}
		return nil
	}

	nodeSubnetStaticRoutes := zic.getStaticRoutes(nodeSubnets, nodeTransitSwitchPortIPs, false)
	for _, staticRoute := range nodeSubnetStaticRoutes {
		if err := addRoute(staticRoute.prefix, staticRoute.nexthop); err != nil {
			return fmt.Errorf("error adding static route %s - %s to the router %s : %w", staticRoute.prefix, staticRoute.nexthop, zic.networkClusterRouterName, err)
		}
	}

	if len(nodeGRPIPs) > 0 {
		nodeGRPIPStaticRoutes := zic.getStaticRoutes(nodeGRPIPs, nodeTransitSwitchPortIPs, true)
		for _, staticRoute := range nodeGRPIPStaticRoutes {
			if err := addRoute(staticRoute.prefix, staticRoute.nexthop); err != nil {
				return fmt.Errorf("error adding static route %s - %s to the router %s : %w", staticRoute.prefix, staticRoute.nexthop, zic.networkClusterRouterName, err)
			}
		}
	}

	_, err := libovsdbops.TransactAndCheck(zic.nbClient, ops)
	return err
}

// deleteLocalNodeStaticRoutes deletes the static routes added by the function addRemoteNodeStaticRoutes
func (zic *ZoneInterconnectHandler) deleteLocalNodeStaticRoutes(node *corev1.Node, nodeTransitSwitchPortIPs []*net.IPNet) error {
	// skip types.NetworkExternalID check in the predicate function as this static route may be deleted
	// before types.NetworkExternalID external-ids is set correctly during upgrade.
	deleteRoute := func(prefix, nexthop string) error {
		p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
			return lrsr.IPPrefix == prefix &&
				lrsr.Nexthop == nexthop &&
				lrsr.ExternalIDs["ic-node"] == node.Name
		}
		if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(zic.nbClient, zic.networkClusterRouterName, p); err != nil {
			return fmt.Errorf("failed to delete static route: %w", err)
		}
		return nil
	}

	nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node, zic.GetNetworkName())
	if err != nil {
		return fmt.Errorf("failed to parse node %s subnets annotation %w", node.Name, err)
	}

	nodeSubnetStaticRoutes := zic.getStaticRoutes(nodeSubnets, nodeTransitSwitchPortIPs, false)
	for _, staticRoute := range nodeSubnetStaticRoutes {
		// Possible optimization: Add all the routes in one transaction
		if err := deleteRoute(staticRoute.prefix, staticRoute.nexthop); err != nil {
			return fmt.Errorf("error deleting static route %s - %s from the router %s : %w", staticRoute.prefix, staticRoute.nexthop, zic.networkClusterRouterName, err)
		}
	}

	if zic.IsUserDefinedNetwork() {
		// UDN cluster router doesn't connect to a join switch
		// or to a Gateway router.
		return nil
	}

	// Clear the routes connecting to the GW Router for the default network
	nodeGRPIPs, err := udn.GetGWRouterIPs(node, zic.GetNetInfo())
	if err != nil {
		// Without node-id we can't reconstruct the GR join IPs; delete by owner.
		p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
			return lrsr.ExternalIDs["ic-node"] == node.Name
		}
		if opserr := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(zic.nbClient, zic.networkClusterRouterName, p); opserr != nil {
			return fmt.Errorf("failed to delete gateway router static routes for node %s: %w", node.Name, opserr)
		}
		return fmt.Errorf("failed to get the gateway router port IP addresses for node %s: %w", node.Name, err)
	}

	nodenodeGRPIPStaticRoutes := zic.getStaticRoutes(nodeGRPIPs, nodeTransitSwitchPortIPs, true)
	for _, staticRoute := range nodenodeGRPIPStaticRoutes {
		// Possible optimization: Add all the routes in one transaction
		if err := deleteRoute(staticRoute.prefix, staticRoute.nexthop); err != nil {
			return fmt.Errorf("error deleting static route %s - %s from the router %s : %w", staticRoute.prefix, staticRoute.nexthop, zic.networkClusterRouterName, err)
		}
	}

	return nil
}

// interconnectStaticRoute represents a static route
type interconnectStaticRoute struct {
	prefix  string
	nexthop string
}

// getStaticRoutes returns a list of static routes from the provided ipPrefix'es and nexthops
// Eg. If ipPrefixes - [10.0.0.4/24, aef0::4/64] and nexthops - [100.88.0.4/16, bef0::4/64] and fullMask is true
//
// It will return [interconnectStaticRoute { prefix : 10.0.0.4/32, nexthop : 100.88.0.4},
// -               interconnectStaticRoute { prefix : aef0::4/128, nexthop : bef0::4}}
//
// If fullMask is false, it will return
// [interconnectStaticRoute { prefix : 10.0.0.4/24, nexthop : 100.88.0.4},
// -               interconnectStaticRoute { prefix : aef0::4/64, nexthop : bef0::4}}
func (zic *ZoneInterconnectHandler) getStaticRoutes(ipPrefixes []*net.IPNet, nexthops []*net.IPNet, fullMask bool) []*interconnectStaticRoute {
	var staticRoutes []*interconnectStaticRoute

	for _, prefix := range ipPrefixes {
		for _, nexthop := range nexthops {
			if utilnet.IPFamilyOfCIDR(prefix) != utilnet.IPFamilyOfCIDR(nexthop) {
				continue
			}
			p := ""
			if fullMask {
				p = prefix.IP.String() + util.GetIPFullMaskString(prefix.IP.String())
			} else {
				p = prefix.String()
			}

			staticRoute := &interconnectStaticRoute{
				prefix:  p,
				nexthop: nexthop.IP.String(),
			}
			staticRoutes = append(staticRoutes, staticRoute)
		}
	}

	return staticRoutes
}

// deleteStaleStaticRoutes removes the static route no longer match the ones
// addRemoteNodeStaticRoutes would create for it.Those are left behind when the
// node transit switch IP or the node gateway router join IP change as part of a
// day 2 operation, since the routes are keyed by IPPrefix and a changed IPPrefix
// ends up creating a new route instead of replacing the old one.
func (zic *ZoneInterconnectHandler) deleteStaleStaticRoutes(node *corev1.Node, nodeTransitSwitchPortIPs, nodeSubnets, nodeGRPIPs []*net.IPNet) error {
	routeKey := func(prefix, nexthop string) string {
		return prefix + " " + nexthop
	}

	expectedRoutes := sets.New[string]()
	for _, staticRoute := range zic.getStaticRoutes(nodeSubnets, nodeTransitSwitchPortIPs, false) {
		expectedRoutes.Insert(routeKey(staticRoute.prefix, staticRoute.nexthop))
	}
	for _, staticRoute := range zic.getStaticRoutes(nodeGRPIPs, nodeTransitSwitchPortIPs, true) {
		expectedRoutes.Insert(routeKey(staticRoute.prefix, staticRoute.nexthop))
	}

	p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
		if lrsr.ExternalIDs["ic-node"] != node.Name {
			return false
		}
		if networkName, isSet := lrsr.ExternalIDs[types.NetworkExternalID]; isSet && networkName != zic.GetNetworkName() {
			return false
		}
		if !expectedRoutes.Has(routeKey(lrsr.IPPrefix, lrsr.Nexthop)) {
			klog.Infof("Removing stale static route %s via %s from router %s for node %s network %s",
				lrsr.IPPrefix, lrsr.Nexthop, zic.networkClusterRouterName, node.Name, zic.GetNetworkName())
			return true
		}
		return false
	}
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(zic.nbClient, zic.networkClusterRouterName, p); err != nil {
		return fmt.Errorf("failed to delete stale static routes of node %s from router %s: %w", node.Name, zic.networkClusterRouterName, err)
	}
	return nil
}

func getUserDefinedNetTransitSwitchExtIDs(networkName, topology string, isPrimaryUDN bool) map[string]string {
	return map[string]string{
		types.NetworkExternalID:     networkName,
		types.NetworkRoleExternalID: util.GetUserDefinedNetworkRole(isPrimaryUDN),
		types.TopologyExternalID:    topology,
	}
}

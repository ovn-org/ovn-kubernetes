package ovn

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"

	utilnet "k8s.io/utils/net"
)

const (
	transitSwitchTunnelKey = "16711683"
)

func (oc *Controller) interconnectAddUpdateLocalNode(node *kapi.Node) error {
	nodeId := util.GetNodeId(node)
	if nodeId == -1 {
		// Don't consider this node as cluster-manager has not allocated node id yet.
		return fmt.Errorf("failed to get node id for node - %s", node.Name)
	}

	// Get the chassis id.
	chassisId, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		return fmt.Errorf("failed to parse node chassis-id for node - %s, error: %v", node.Name, err)
	}

	// Update the node chassis to local.
	if err := libovsdbops.UpdateChassisToLocal(oc.sbClient, node.Name, chassisId); err != nil {
		return fmt.Errorf("failed to create update remote chassis into local %s, error: %v", node.Name, err)
	}

	return oc.interconnectCreateLocalNodeResources(node, nodeId)
}

func (oc *Controller) interconnectCreateLocalNodeResources(node *kapi.Node, nodeId int) error {
	nodeTransitSwitchPortIps, err := util.ParseNodeTransitSwitchPortAddresses(node)
	if err != nil || len(nodeTransitSwitchPortIps) == 0 {
		return fmt.Errorf("failed to get the node transit switch port Ips : %v", err)
	}

	var transitRouterPortMac net.HardwareAddr
	var transitRouterPortNetworks []string
	var transitRouterPortIps []string
	foundIPv4 := false
	for _, ip := range nodeTransitSwitchPortIps {
		if utilnet.IsIPv4CIDR(ip) {
			foundIPv4 = true
			transitRouterPortMac = util.IPAddrToHWAddr(ip.IP)
		} else {
			if !foundIPv4 {
				transitRouterPortMac = util.IPAddrToHWAddr(ip.IP)
			}
		}
		transitRouterPortNetworks = append(transitRouterPortNetworks, ip.String())
		transitRouterPortIps = append(transitRouterPortIps, ip.IP.String())
	}

	logicalSwitch := nbdb.LogicalSwitch{
		Name: types.TransitSwitch,
		OtherConfig: map[string]string{
			"interconn-ts":             types.TransitSwitch,
			"requested-tnl-key":        transitSwitchTunnelKey,
			"mcast_snoop":              "true",
			"mcast_flood_unregistered": "true",
		},
	}
	if err := libovsdbops.CreateOrUpdateLogicalSwitch(oc.nbClient, &logicalSwitch); err != nil {
		return fmt.Errorf("failed to create/update logical transit switch: %v", err)
	}

	logicalRouterPortName := types.RouterToTransitSwitchPrefix + node.Name
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     logicalRouterPortName,
		MAC:      transitRouterPortMac.String(),
		Networks: transitRouterPortNetworks,
		Options: map[string]string{
			"mcast_flood": "true",
		},
	}
	logicalRouter := nbdb.LogicalRouter{
		Name: types.OVNClusterRouter,
	}

	if err := libovsdbops.CreateOrUpdateLogicalRouterPorts(oc.nbClient, &logicalRouter,
		[]*nbdb.LogicalRouterPort{&logicalRouterPort}); err != nil {
		return fmt.Errorf("failed to create/update cluster router to transit switch ports: %v", err)
	}

	lspOptions := map[string]string{
		"router-port":       types.RouterToTransitSwitchPrefix + node.Name,
		"requested-tnl-key": strconv.Itoa(nodeId),
	}

	err = oc.addNodeLogicalSwitchPort(types.TransitSwitch, types.TransitSwitchToRouterPrefix+node.Name,
		"router", []string{"router"}, lspOptions)
	if err != nil {
		return err
	}

	err = oc.deleteLocalNodeStaticRoutes(node, nodeId, transitRouterPortIps)
	return err
}

func (oc *Controller) addNodeLogicalSwitchPort(logicalSwitchName, portName, portType string, addresses []string, options map[string]string) error {
	logicalSwitch := nbdb.LogicalSwitch{
		Name: types.TransitSwitch,
	}

	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      portName,
		Type:      portType,
		Options:   options,
		Addresses: addresses,
	}
	if err := libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &logicalSwitch, &logicalSwitchPort); err != nil {
		return fmt.Errorf("failed to add logical port %s to switch %s, error: %v", portName, logicalSwitch.Name, err)
	}
	return nil
}

func (oc *Controller) interconnectAddUpdateRemoteNode(node *kapi.Node) error {
	nodeId := util.GetNodeId(node)
	if nodeId == -1 {
		// Don't consider this node as cluster-manager has not allocated node id yet.
		return fmt.Errorf("failed to get node id for node - %s", node.Name)
	}

	// Get the chassis id.
	chassisId, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		return fmt.Errorf("failed to parse node chassis-id for node - %s, error: %v", node.Name, err)
	}

	nodePrimaryIp, err := util.GetNodePrimaryIP(node)
	if err != nil {
		return fmt.Errorf("failed to parse node %s primary IP %v", node.Name, err)
	}

	if err := libovsdbops.CreateOrUpdateRemoteChassis(oc.sbClient, node.Name, chassisId, nodePrimaryIp); err != nil {
		return fmt.Errorf("failed to create encaps and remote chassis %s, error: %v", node.Name, err)
	}

	return oc.interconnectCreateRemoteNodeResources(node, nodeId, chassisId)
}

func (oc *Controller) interconnectCreateRemoteNodeResources(node *kapi.Node, nodeId int, chassisId string) error {
	nodeTransitSwitchPortIps, err := util.ParseNodeTransitSwitchPortAddresses(node)
	if err != nil || len(nodeTransitSwitchPortIps) == 0 {
		return fmt.Errorf("failed to get the node transit switch port Ips : %v", err)
	}

	var transitRouterPortMac net.HardwareAddr
	var transitRouterPortNetworks []string
	var transitRouterPortIps []string
	foundIPv4 := false
	for _, ip := range nodeTransitSwitchPortIps {
		if utilnet.IsIPv4CIDR(ip) {
			foundIPv4 = true
			transitRouterPortMac = util.IPAddrToHWAddr(ip.IP)
		} else {
			if !foundIPv4 {
				transitRouterPortMac = util.IPAddrToHWAddr(ip.IP)
			}
		}
		transitRouterPortNetworks = append(transitRouterPortNetworks, ip.String())
		transitRouterPortIps = append(transitRouterPortIps, ip.IP.String())
	}

	remotePortAddr := transitRouterPortMac.String()
	for _, tsNetwork := range transitRouterPortNetworks {
		remotePortAddr = remotePortAddr + " " + tsNetwork
	}

	lspOptions := map[string]string{
		"requested-tnl-key": strconv.Itoa(nodeId),
	}
	if err := oc.addNodeLogicalSwitchPort(types.TransitSwitch, types.TransitSwitchToRouterPrefix+node.Name, "remote", []string{remotePortAddr}, lspOptions); err != nil {
		return err
	}
	// Set the port binding chassis.
	if err := oc.setRemotePortBindingChassis(node.Name, types.TransitSwitchToRouterPrefix+node.Name, chassisId); err != nil {
		return err
	}

	if err := oc.addRemoteNodeStaticRoutes(node, nodeId, transitRouterPortIps); err != nil {
		return err
	}

	logicalRouterPort := nbdb.LogicalRouterPort{
		Name: types.RouterToTransitSwitchPrefix + node.Name,
	}
	ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
	defer cancel()
	if err := oc.nbClient.Get(ctx, logicalRouterPort); err != nil {
		return nil
	}

	logicalRouter := nbdb.LogicalRouter{
		Name: types.OVNClusterRouter,
	}
	if err := libovsdbops.DeleteLogicalRouterPorts(oc.nbClient, &logicalRouter, &logicalRouterPort); err != nil {
		return fmt.Errorf("failed to delete logical router port %s from router %s, error: %v", logicalRouterPort.Name, logicalRouter.Name, err)
	}

	return nil
}

func (oc *Controller) setRemotePortBindingChassis(nodeName, portName, chassisId string) error {
	remotePort := sbdb.PortBinding{
		LogicalPort: portName,
	}
	chassis := sbdb.Chassis{
		Hostname: nodeName,
		Name:     chassisId,
	}

	if err := libovsdbops.UpdatePortBindingSetChassis(oc.sbClient, &remotePort, &chassis); err != nil {
		return fmt.Errorf("failed to update chassis for remote port, error: %v", portName)
	}

	return nil
}

func (ic *Controller) addRemoteNodeStaticRoutes(node *kapi.Node, nodeId int, tsIps []string) error {
	addRoute := func(subnet *net.IPNet, tsIp string) error {
		logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
			ExternalIDs: map[string]string{
				"ic-node": node.Name,
			},
			Nexthop:  tsIp,
			IPPrefix: subnet.String(),
		}
		p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
			return lrsr.IPPrefix == subnet.String() &&
				lrsr.Nexthop == tsIp &&
				lrsr.ExternalIDs["ic-node"] == node.Name
		}
		//FIXME: All routes should be added in a single transaction instead.
		if err := libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicate(ic.nbClient, types.OVNClusterRouter, &logicalRouterStaticRoute, p); err != nil {
			return fmt.Errorf("failed to create static route: %v", err)
		}
		return nil
	}

	nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node)
	if err != nil {
		return fmt.Errorf("failed to parse node %s subnets annotation %v", node.Name, err)
	}

	//FIXME: No consistency on transaction failure.
	for _, subnet := range nodeSubnets {
		for _, tsIp := range tsIps {
			if err := addRoute(subnet, tsIp); err != nil {
				return fmt.Errorf("unable to create static routes: %v", err)
			}
		}
	}

	joinSubnets, err := util.ParseZoneJoinSubnetsAnnotation(node)
	if err != nil {
		return fmt.Errorf("failed to parse node %s join subnets annotation %v", node.Name, err)
	}

	for _, subnet := range joinSubnets {
		for _, tsIp := range tsIps {
			if err := addRoute(subnet, tsIp); err != nil {
				return fmt.Errorf("unable to create static routes: %v", err)
			}
		}
	}

	nodeGRIPs, err := util.ParseNodeGRIPsAnnotation(node)
	if err != nil {
		return fmt.Errorf("failed to parse node %s GR IPs annotation %v", node.Name, err)
	}

	for _, subnet := range nodeGRIPs {
		for _, tsIp := range tsIps {
			if err := addRoute(subnet, tsIp); err != nil {
				return fmt.Errorf("unable to create static routes: %v", err)
			}
		}
	}

	return nil
}

func (ic *Controller) deleteLocalNodeStaticRoutes(node *kapi.Node, nodeId int, tsIps []string) error {
	deleteRoute := func(subnet *net.IPNet, tsIp string) error {
		p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
			return lrsr.IPPrefix == subnet.String() &&
				lrsr.Nexthop == tsIp &&
				lrsr.ExternalIDs["ic-node"] == node.Name
		}
		//FIXME: All routes should be added in a single transaction instead.
		if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(ic.nbClient, types.OVNClusterRouter, p); err != nil {
			return fmt.Errorf("failed to create static route: %v", err)
		}
		return nil
	}

	nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node)
	if err != nil {
		return fmt.Errorf("failed to parse node %s subnets annotation %v", node.Name, err)
	}

	//FIXME: No consistency on transaction failure.
	for _, subnet := range nodeSubnets {
		for _, tsIp := range tsIps {
			if err := deleteRoute(subnet, tsIp); err != nil {
				return fmt.Errorf("unable to create static routes: %v", err)
			}
		}
	}

	joinSubnets, err := util.ParseZoneJoinSubnetsAnnotation(node)
	if err != nil {
		return fmt.Errorf("failed to parse node %s join subnets annotation %v", node.Name, err)
	}

	for _, subnet := range joinSubnets {
		for _, tsIp := range tsIps {
			if err := deleteRoute(subnet, tsIp); err != nil {
				return fmt.Errorf("unable to create static routes: %v", err)
			}
		}
	}

	nodeGRIPs, err := util.ParseNodeGRIPsAnnotation(node)
	if err != nil {
		return fmt.Errorf("failed to parse node %s GR IPs annotation %v", node.Name, err)
	}

	for _, subnet := range nodeGRIPs {
		for _, tsIp := range tsIps {
			if err := deleteRoute(subnet, tsIp); err != nil {
				return fmt.Errorf("unable to create static routes: %v", err)
			}
		}
	}

	return nil
}

func (oc *Controller) interconnectRemoveNode(node *kapi.Node) error {
	//TODO
	return nil
}

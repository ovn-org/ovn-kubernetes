package topology

import (
	"fmt"
	"net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type GatewayTopologyFactory struct {
	nbClient libovsdbclient.Client
}

func NewGatewayTopologyFactory(ovnNBClient libovsdbclient.Client) *GatewayTopologyFactory {
	return &GatewayTopologyFactory{
		nbClient: ovnNBClient,
	}
}

func (gtf *GatewayTopologyFactory) NewClusterRouter(
	clusterRouterName string,
	netInfo util.NetInfo,
	coopUUID string,
) (*nbdb.LogicalRouter, error) {
	routerOptions := map[string]string{"always_learn_from_arp_request": "false"}
	return gtf.newClusterRouter(clusterRouterName, netInfo, coopUUID, routerOptions)
}

func (gtf *GatewayTopologyFactory) NewClusterRouterWithMulticastSupport(
	clusterRouterName string,
	netInfo util.NetInfo,
	coopUUID string,
) (*nbdb.LogicalRouter, error) {
	routerOptions := map[string]string{"mcast_relay": "true"}
	return gtf.newClusterRouter(clusterRouterName, netInfo, coopUUID, routerOptions)
}

func (gtf *GatewayTopologyFactory) newClusterRouter(
	clusterRouterName string,
	netInfo util.NetInfo,
	coopUUID string,
	routerOptions map[string]string,
) (*nbdb.LogicalRouter, error) {
	// Create a single common distributed router for the cluster.
	logicalRouter := nbdb.LogicalRouter{
		Name: clusterRouterName,
		ExternalIDs: map[string]string{
			"k8s-cluster-router": "yes",
		},
		Options: routerOptions,
		Copp:    &coopUUID,
	}
	if netInfo.IsSecondary() {
		logicalRouter.ExternalIDs[types.NetworkExternalID] = netInfo.GetNetworkName()
		logicalRouter.ExternalIDs[types.TopologyExternalID] = netInfo.TopologyType()
	}

	if err := libovsdbops.CreateOrUpdateLogicalRouter(
		gtf.nbClient,
		&logicalRouter,
		&logicalRouter.Options,
		&logicalRouter.ExternalIDs,
		&logicalRouter.Copp,
	); err != nil {
		return nil, fmt.Errorf("failed to create distributed router %s, error: %v",
			clusterRouterName, err)
	}

	return &logicalRouter, nil
}

// NewJoinSwitch creates a join switch to connect gateway routers to the distributed router.
func (gtf *GatewayTopologyFactory) NewJoinSwitch(
	clusterRouter *nbdb.LogicalRouter,
	netInfo util.NetInfo,
	reservedIPs []*net.IPNet,
) error {
	joinSwitchName := netInfo.GetNetworkScopedJoinSwitchName()
	logicalSwitch := nbdb.LogicalSwitch{
		Name: joinSwitchName,
	}
	if netInfo.IsSecondary() {
		logicalSwitch.ExternalIDs = map[string]string{
			types.NetworkExternalID:  netInfo.GetNetworkName(),
			types.TopologyExternalID: netInfo.TopologyType(),
		}
	}

	// nothing is updated here, so no reason to pass fields
	err := libovsdbops.CreateOrUpdateLogicalSwitch(gtf.nbClient, &logicalSwitch)
	if err != nil {
		return fmt.Errorf("failed to create logical switch %+v: %v", logicalSwitch, err)
	}

	// Connect the distributed router to OVNJoinSwitch.
	drSwitchPort := types.JoinSwitchToGWRouterPrefix + clusterRouter.Name
	drRouterPort := types.GWRouterToJoinSwitchPrefix + clusterRouter.Name

	gwLRPMAC := util.IPAddrToHWAddr(reservedIPs[0].IP)
	gwLRPNetworks := []string{}
	for _, gwLRPIfAddr := range reservedIPs {
		gwLRPNetworks = append(gwLRPNetworks, gwLRPIfAddr.String())
	}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     drRouterPort,
		MAC:      gwLRPMAC.String(),
		Networks: gwLRPNetworks,
	}
	if netInfo.IsSecondary() {
		logicalRouterPort.ExternalIDs = map[string]string{
			types.NetworkExternalID:  netInfo.GetNetworkName(),
			types.TopologyExternalID: netInfo.TopologyType(),
		}
	}

	err = libovsdbops.CreateOrUpdateLogicalRouterPort(gtf.nbClient, clusterRouter,
		&logicalRouterPort, nil, &logicalRouterPort.MAC, &logicalRouterPort.Networks)
	if err != nil {
		return fmt.Errorf("failed to add logical router port %+v on router %s: %v", logicalRouterPort, clusterRouter.Name, err)
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
	sw := nbdb.LogicalSwitch{Name: joinSwitchName}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(gtf.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return fmt.Errorf("failed to create logical switch port %+v and switch %s: %v", logicalSwitchPort, joinSwitchName, err)
	}

	return nil
}

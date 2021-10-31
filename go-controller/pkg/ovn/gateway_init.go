package ovn

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// gatewayInit creates a gateway router for the local chassis.
func (oc *Controller) gatewayInit(nodeName string, clusterIPSubnet []*net.IPNet, hostSubnets []*net.IPNet,
	l3GatewayConfig *util.L3GatewayConfig, sctpSupport bool, gwLRPIfAddrs, drLRPIfAddrs []*net.IPNet) error {

	gwLRPIPs := make([]net.IP, 0)
	for _, gwLRPIfAddr := range gwLRPIfAddrs {
		gwLRPIPs = append(gwLRPIPs, gwLRPIfAddr.IP)
	}

	// Create a gateway router.
	gatewayRouter := types.GWRouterPrefix + nodeName
	physicalIPs := make([]string, len(l3GatewayConfig.IPAddresses))
	for i, ip := range l3GatewayConfig.IPAddresses {
		physicalIPs[i] = ip.IP.String()
	}

	logicalRouterOptions := map[string]string{
		"always_learn_from_arp_request": "false",
		"dynamic_neigh_routers":         "true",
		"chassis":                       l3GatewayConfig.ChassisID,
		"lb_force_snat_ip":              "router_ip",
		"snat-ct-zone":                  "0",
	}
	logicalRouterExternalIDs := map[string]string{
		"physical_ip":  physicalIPs[0],
		"physical_ips": strings.Join(physicalIPs, ","),
	}

	logicalRouter := nbdb.LogicalRouter{
		Name:        gatewayRouter,
		Options:     logicalRouterOptions,
		ExternalIDs: logicalRouterExternalIDs,
	}
	opModels := []libovsdbops.OperationModel{
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
			OnModelUpdates: []interface{}{
				&logicalRouter.Options,
				&logicalRouter.ExternalIDs,
			},
		},
	}

	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to create logical router %v, err: %v", gatewayRouter, err)
	}

	gwSwitchPort := types.JoinSwitchToGWRouterPrefix + gatewayRouter
	gwRouterPort := types.GWRouterToJoinSwitchPrefix + gatewayRouter

	logicalSwitch := nbdb.LogicalSwitch{}
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      gwSwitchPort,
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			"router-port": gwRouterPort,
		},
	}

	opModels = []libovsdbops.OperationModel{
		{
			Model: &logicalSwitchPort,
			DoAfter: func() {
				logicalSwitch.Ports = []string{logicalSwitchPort.UUID}
			},
		},
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == types.OVNJoinSwitch },
			OnModelMutations: []interface{}{
				&logicalSwitch.Ports,
			},
			ErrNotFound: true,
		},
	}

	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add port %q to logical switch %q, err: %v", gwSwitchPort, types.OVNJoinSwitch, err)
	}

	gwLRPMAC := util.IPAddrToHWAddr(gwLRPIPs[0])
	gwLRPNetworks := []string{}
	for _, gwLRPIfAddr := range gwLRPIfAddrs {
		gwLRPNetworks = append(gwLRPNetworks, gwLRPIfAddr.String())
	}

	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     gwRouterPort,
		MAC:      gwLRPMAC.String(),
		Networks: gwLRPNetworks,
	}
	opModels = []libovsdbops.OperationModel{
		{
			Model: &logicalRouterPort,
			OnModelUpdates: []interface{}{
				&logicalRouterPort.MAC,
				&logicalRouterPort.Networks,
			},
			DoAfter: func() {
				logicalRouter.Ports = []string{logicalRouterPort.UUID}
			},
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Ports,
			},
			ErrNotFound: true,
		},
	}

	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical router port %q for gateway router %s, err: %v", gwRouterPort, gatewayRouter, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()

	for _, entry := range clusterIPSubnet {
		drLRPIfAddr, err := util.MatchIPNetFamily(utilnet.IsIPv6CIDR(entry), drLRPIfAddrs)
		if err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with distributed "+
				"router as the nexthop: %v",
				gatewayRouter, err)
		}

		logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
			IPPrefix: entry.String(),
			Nexthop:  drLRPIfAddr.IP.String(),
		}

		tmpRouters := []nbdb.LogicalRouter{}
		if err := oc.nbClient.WhereCache(func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter }).List(ctx, &tmpRouters); err != nil {
			return fmt.Errorf("unable to list logical router: %s, err: %v", gatewayRouter, err)
		}
		if len(tmpRouters) != 1 {
			return fmt.Errorf("unable to retrieve unique logical router: %s, found: %+v", gatewayRouter, tmpRouters)
		}

		opModels = []libovsdbops.OperationModel{
			{
				Model: &logicalRouterStaticRoute,
				ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
					return lrsr.IPPrefix == entry.String() && lrsr.Nexthop == drLRPIfAddr.IP.String() && util.SliceHasStringItem(tmpRouters[0].StaticRoutes, lrsr.UUID)
				},
				OnModelUpdates: []interface{}{
					&logicalRouterStaticRoute.IPPrefix,
					&logicalRouterStaticRoute.Nexthop,
				},
				DoAfter: func() {
					if logicalRouterStaticRoute.UUID != "" {
						logicalRouter.StaticRoutes = []string{logicalRouterStaticRoute.UUID}
					}
				},
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
				OnModelMutations: []interface{}{
					&logicalRouter.StaticRoutes,
				},
				ErrNotFound: true,
			},
		}
		if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with distributed router as the nexthop, err: %v", gatewayRouter, err)
		}
	}

	if err := oc.addExternalSwitch("",
		l3GatewayConfig.InterfaceID,
		nodeName,
		gatewayRouter,
		l3GatewayConfig.MACAddress.String(),
		types.PhysicalNetworkName,
		l3GatewayConfig.IPAddresses,
		l3GatewayConfig.VLANID); err != nil {
		return err
	}

	if l3GatewayConfig.EgressGWInterfaceID != "" {
		if err := oc.addExternalSwitch(types.EgressGWSwitchPrefix,
			l3GatewayConfig.EgressGWInterfaceID,
			nodeName,
			gatewayRouter,
			l3GatewayConfig.EgressGWMACAddress.String(),
			types.PhysicalNetworkExGwName,
			l3GatewayConfig.EgressGWIPAddresses,
			nil); err != nil {
			return err
		}
	}

	externalRouterPort := types.GWRouterToExtSwitchPrefix + gatewayRouter

	// Add static routes in GR with gateway router as the default next hop.
	for _, nextHop := range l3GatewayConfig.NextHops {
		var allIPs string
		if utilnet.IsIPv6(nextHop) {
			allIPs = "::/0"
		} else {
			allIPs = "0.0.0.0/0"
		}

		logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
			IPPrefix:   allIPs,
			Nexthop:    nextHop.String(),
			OutputPort: &externalRouterPort,
		}
		opModels = []libovsdbops.OperationModel{
			{
				Model: &logicalRouterStaticRoute,
				ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
					return lrsr.OutputPort != nil && *lrsr.OutputPort == externalRouterPort && lrsr.Nexthop == nextHop.String()
				},
				OnModelUpdates: []interface{}{
					&logicalRouterStaticRoute.Nexthop,
				},
				DoAfter: func() {
					if logicalRouterStaticRoute.UUID != "" {
						logicalRouter.StaticRoutes = []string{logicalRouterStaticRoute.UUID}
					}
				},
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
				OnModelMutations: []interface{}{
					&logicalRouter.StaticRoutes,
				},
				ErrNotFound: true,
			},
		}
		if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with physical gateway as the default next hop, err: %v", gatewayRouter, err)
		}
	}

	// We need to add a route to the Gateway router's IP, on the
	// cluster router, to ensure that the return traffic goes back
	// to the same gateway router
	//
	// This can be removed once https://bugzilla.redhat.com/show_bug.cgi?id=1891516 is fixed.
	for _, gwLRPIP := range gwLRPIPs {

		logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
			IPPrefix: gwLRPIP.String(),
			Nexthop:  gwLRPIP.String(),
		}
		opModels = []libovsdbops.OperationModel{
			{
				Model: &logicalRouterStaticRoute,
				ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
					return lrsr.Nexthop == gwLRPIP.String() && lrsr.IPPrefix == gwLRPIP.String()
				},
				OnModelUpdates: []interface{}{
					&logicalRouterStaticRoute.Nexthop,
					&logicalRouterStaticRoute.IPPrefix,
				},
				DoAfter: func() {
					if logicalRouterStaticRoute.UUID != "" {
						logicalRouter.StaticRoutes = []string{logicalRouterStaticRoute.UUID}
					}
				},
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
				OnModelMutations: []interface{}{
					&logicalRouter.StaticRoutes,
				},
				ErrNotFound: true,
			},
		}
		if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with physical gateway as the default next hop, err: %v", gatewayRouter, err)
		}
	}

	// Add source IP address based routes in distributed router
	// for this gateway router.
	for _, hostSubnet := range hostSubnets {
		gwLRPIP, err := util.MatchIPFamily(utilnet.IsIPv6CIDR(hostSubnet), gwLRPIPs)
		if err != nil {
			return fmt.Errorf("failed to add source IP address based "+
				"routes in distributed router %s: %v",
				types.OVNClusterRouter, err)
		}

		if config.Gateway.Mode != config.GatewayModeLocal {

			logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				IPPrefix: hostSubnet.String(),
				Nexthop:  gwLRPIP[0].String(),
			}
			opModels = []libovsdbops.OperationModel{
				{
					Model: &logicalRouterStaticRoute,
					ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
						return lrsr.Nexthop == gwLRPIP[0].String() && lrsr.IPPrefix == hostSubnet.String()
					},
					OnModelUpdates: []interface{}{
						&logicalRouterStaticRoute.Nexthop,
						&logicalRouterStaticRoute.IPPrefix,
					},
					DoAfter: func() {
						if logicalRouterStaticRoute.UUID != "" {
							logicalRouter.StaticRoutes = []string{logicalRouterStaticRoute.UUID}
						}
					},
				},
				{
					Model:          &logicalRouter,
					ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
					OnModelMutations: []interface{}{
						&logicalRouter.StaticRoutes,
					},
					ErrNotFound: true,
				},
			}
			if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
				return fmt.Errorf("failed to add a static route in GR %s with physical gateway as the default next hop, err: %v", gatewayRouter, err)
			}
		}
	}

	// if config.Gateway.DisabledSNATMultipleGWs is not set (by default it is not),
	// the NAT rules for pods not having annotations to route through either external
	// gws or pod CNFs will be added within pods.go addLogicalPort
	nats := make([]*nbdb.NAT, 0, len(clusterIPSubnet))
	var nat *nbdb.NAT
	if !config.Gateway.DisableSNATMultipleGWs && config.Gateway.Mode != config.GatewayModeLocal {
		// Default SNAT rules.
		externalIPs := make([]net.IP, len(l3GatewayConfig.IPAddresses))
		for i, ip := range l3GatewayConfig.IPAddresses {
			externalIPs[i] = ip.IP
		}
		for _, entry := range clusterIPSubnet {
			externalIP, err := util.MatchIPFamily(utilnet.IsIPv6CIDR(entry), externalIPs)
			if err != nil {
				return fmt.Errorf("failed to create default SNAT rules for gateway router %s: %v",
					gatewayRouter, err)
			}
			nat = libovsdbops.BuildRouterSNAT(&externalIP[0], entry, "", nil)
			nats = append(nats, nat)
		}
		err := libovsdbops.AddOrUpdateNatsToRouter(oc.nbClient, gatewayRouter, nats...)
		if err != nil {
			return fmt.Errorf("failed to update SNAT rule for pod on router %s error: %v", gatewayRouter, err)
		}
	} else {
		// ensure we do not have any leftover SNAT entries after an upgrade
		for _, logicalSubnet := range clusterIPSubnet {
			nat = libovsdbops.BuildRouterSNAT(nil, logicalSubnet, "", nil)
			nats = append(nats, nat)
		}
		err := libovsdbops.DeleteNatsFromRouter(oc.nbClient, gatewayRouter, nats...)
		if err != nil {
			return fmt.Errorf("failed to delete GW SNAT rule for pod on router %s error: %v", gatewayRouter, err)
		}
	}

	return nil
}

// This DistributedGWPort guarantees to always have both IPv4 and IPv6 regardless of dual-stack
func (oc *Controller) addDistributedGWPort() error {
	masterChassisID, err := util.GetNodeChassisID()
	if err != nil {
		return fmt.Errorf("failed to get master's chassis ID error: %v", err)
	}

	// the distributed gateway port is always dual-stack and uses the IPv4 address to generate its mac
	dgpName := types.RouterToSwitchPrefix + types.NodeLocalSwitch

	dgpMac := util.IPAddrToHWAddr(net.ParseIP(types.V4NodeLocalDistributedGWPortIP)).String()
	dgpNetworkV4 := fmt.Sprintf("%s/%d", types.V4NodeLocalDistributedGWPortIP, types.V4NodeLocalNATSubnetPrefix)
	dgpNetworkV6 := fmt.Sprintf("%s/%d", types.V6NodeLocalDistributedGWPortIP, types.V6NodeLocalNATSubnetPrefix)

	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     dgpName,
		MAC:      dgpMac,
		Networks: []string{dgpNetworkV4, dgpNetworkV6},
	}
	opModels := []libovsdbops.OperationModel{
		{
			Model: &logicalRouterPort,
			OnModelUpdates: []interface{}{
				&logicalRouterPort.MAC,
				&logicalRouterPort.Networks,
			},
			DoAfter: func() {
				logicalRouter.Ports = []string{logicalRouterPort.UUID}
			},
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Ports,
			},
			ErrNotFound: true,
		},
	}
	_, err = oc.modelClient.CreateOrUpdate(opModels...)
	if err != nil {
		return fmt.Errorf("unable to add distributed GW port: %s to logical router: %s, err: %v", dgpName, types.OVNClusterRouter, err)
	}
	// The DPG port will be used to create the gateway chassis below. It can't
	// have a named UUID, because it won't be found, hence: swap out the named
	// UUID to the real one if we are performing a create operation.
	//if libovsdbops.IsNamedUUID(logicalRouterPort.UUID) {
	//	logicalRouterPort.UUID = res[0].UUID.GoUUID
	//}

	gatewayChassis := nbdb.GatewayChassis{
		ChassisName: masterChassisID,
		ExternalIDs: map[string]string{
			"dpg_name": dgpName,
		},
		Name:     fmt.Sprintf("%s_%s", dgpName, masterChassisID),
		Priority: 100,
	}
	opModels = []libovsdbops.OperationModel{
		{
			Model: &gatewayChassis,
			OnModelUpdates: []interface{}{
				&gatewayChassis.ChassisName,
				&gatewayChassis.Name,
			},
			DoAfter: func() {
				logicalRouterPort.GatewayChassis = []string{gatewayChassis.UUID}
			},
		},
		{
			Model: &logicalRouterPort,
			OnModelMutations: []interface{}{
				&logicalRouterPort.GatewayChassis,
			},
			ErrNotFound: true,
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("unable to add gateway chassis: %s to logical router port: %s, err: %v", masterChassisID, dgpName, err)
	}

	// connect the distributed gateway port to logical switch configured with localnet port
	logicalSwitch := nbdb.LogicalSwitch{
		Name: types.NodeLocalSwitch,
	}
	opModels = []libovsdbops.OperationModel{
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == types.NodeLocalSwitch },
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("unable to create the logical switch for the localnet port, err: %v", err)
	}

	// add localnet port to the logical switch
	lclNetPortname := "lnet-" + types.NodeLocalSwitch

	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      lclNetPortname,
		Addresses: []string{"unknown"},
		Type:      "localnet",
		Options: map[string]string{
			"network_name": types.LocalNetworkName,
		},
	}
	opModels = []libovsdbops.OperationModel{
		{
			Model: &logicalSwitchPort,
			DoAfter: func() {
				logicalSwitch.Ports = []string{logicalSwitchPort.UUID}
			},
		},
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == types.NodeLocalSwitch },
			OnModelMutations: []interface{}{
				&logicalSwitch.Ports,
			},
			ErrNotFound: true,
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("unable to add the localnet port to the logical switch, err: %v", err)
	}

	// connect the switch to the distributed router
	lspName := types.SwitchToRouterPrefix + types.NodeLocalSwitch

	logicalSwitchPort = nbdb.LogicalSwitchPort{
		Name:      lspName,
		Addresses: []string{"router"},
		Type:      "router",
		Options: map[string]string{
			"nat-addresses": "router",
			"router-port":   dgpName,
		},
	}
	opModels = []libovsdbops.OperationModel{
		{
			Model: &logicalSwitchPort,
			DoAfter: func() {
				logicalSwitch.Ports = []string{logicalSwitchPort.UUID}
			},
		},
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == types.NodeLocalSwitch },
			OnModelMutations: []interface{}{
				&logicalSwitch.Ports,
			},
			ErrNotFound: true,
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("unable to add the localnet port to the logical switch, err: %v", err)
	}

	// finally add an entry to the OVN SB MAC_Binding table, if not present, to capture the
	// MAC-IP binding of types.V4NodeLocalNATSubnetNextHop address or
	// types.V6NodeLocalNATSubnetNextHop address to its MAC. Normally, this will
	// be learnt and added by the chassis to which the distributed gateway port (DGP) is
	// bound. However, in our case we don't send any traffic out with the DGP port's IP
	// as source IP, so that binding will never be learnt and we need to seed it.

	var nodeLocalNatSubnetNextHop string
	dnatSnatNextHopMac := util.IPAddrToHWAddr(net.ParseIP(types.V4NodeLocalNATSubnetNextHop))
	nodeLocalNatSubnetNextHop = types.V4NodeLocalNATSubnetNextHop + " " + types.V6NodeLocalNATSubnetNextHop

	macResult := []sbdb.MACBinding{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	if err := oc.sbClient.WhereCache(func(mb *sbdb.MACBinding) bool {
		return mb.LogicalPort == dgpName && mb.MAC == dnatSnatNextHopMac.String()
	}).List(ctx, &macResult); err != nil {
		return fmt.Errorf("failed to check existence of MAC_Binding entry of (%s, %s) for distributed router port %s "+
			"error: %v", nodeLocalNatSubnetNextHop, dnatSnatNextHopMac, dgpName, err)
	}

	if len(macResult) > 0 {
		klog.Infof("The MAC_Binding entry of (%s, %s) exists on distributed router port %s", nodeLocalNatSubnetNextHop, dnatSnatNextHopMac, dgpName)
		return nil
	}

	for _, ip := range []string{types.V4NodeLocalNATSubnetNextHop, types.V6NodeLocalNATSubnetNextHop} {
		nextHop := net.ParseIP(ip)
		if err := util.CreateMACBinding(dgpName, types.OVNClusterRouter, dnatSnatNextHopMac, nextHop); err != nil {
			return fmt.Errorf("unable to create mac binding for DGP: %v", err)
		}
	}

	return nil
}

// addExternalSwitch creates a switch connected to the external bridge and connects it to
// the gateway router
func (oc *Controller) addExternalSwitch(prefix, interfaceID, nodeName, gatewayRouter, macAddress, physNetworkName string, ipAddresses []*net.IPNet, vlanID *uint) error {
	// Create the external switch for the physical interface to connect to.
	externalSwitch := fmt.Sprintf("%s%s%s", prefix, types.ExternalSwitchPrefix, nodeName)

	externalLogicalSwitch := nbdb.LogicalSwitch{
		Name: externalSwitch,
	}
	opModels := []libovsdbops.OperationModel{
		{
			Model:          &externalLogicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == externalSwitch },
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to create logical switch %s, err: %v", externalSwitch, err)
	}

	// Add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	externalLogicalSwitchPort := nbdb.LogicalSwitchPort{
		Addresses: []string{"unknown"},
		Type:      "localnet",
		Options: map[string]string{
			"network_name": physNetworkName,
		},
		Name: interfaceID,
	}
	if vlanID != nil {
		intVlanID := int(*vlanID)
		externalLogicalSwitchPort.TagRequest = &intVlanID
	}

	opModels = []libovsdbops.OperationModel{
		{
			Model: &externalLogicalSwitchPort,
			OnModelUpdates: []interface{}{
				&externalLogicalSwitchPort.Addresses,
				&externalLogicalSwitchPort.Type,
				&externalLogicalSwitchPort.TagRequest,
				&externalLogicalSwitchPort.Options,
			},
			DoAfter: func() {
				externalLogicalSwitch.Ports = []string{externalLogicalSwitchPort.UUID}
			},
		},
		{
			Model:          &externalLogicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == externalSwitch },
			OnModelMutations: []interface{}{
				&externalLogicalSwitch.Ports,
			},
			ErrNotFound: true,
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical switch port: %s to switch %s, err: %v", interfaceID, externalSwitch, err)
	}

	// Connect GR to external_switch with mac address of external interface
	// and that IP address. In the case of `local` gateway mode, whenever ovnkube-node container
	// restarts a new br-local bridge will be created with a new `nicMacAddress`.
	externalRouterPort := types.GWRouterToExtSwitchPrefix + gatewayRouter

	externalRouterPortNetworks := []string{}
	for _, ip := range ipAddresses {
		externalRouterPortNetworks = append(externalRouterPortNetworks, ip.String())
	}
	externalLogicalRouterPort := nbdb.LogicalRouterPort{
		MAC: macAddress,
		ExternalIDs: map[string]string{
			"gateway-physical-ip": "yes",
		},
		Networks: externalRouterPortNetworks,
		Name:     externalRouterPort,
	}

	logicalRouter := nbdb.LogicalRouter{}
	opModels = []libovsdbops.OperationModel{
		{
			Model: &externalLogicalRouterPort,
			OnModelUpdates: []interface{}{
				&externalLogicalRouterPort.MAC,
				&externalLogicalRouterPort.Networks,
			},
			DoAfter: func() {
				logicalRouter.Ports = []string{externalLogicalRouterPort.UUID}
			},
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Ports,
			},
			ErrNotFound: true,
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical router port: %s to router %s, err: %v", externalRouterPort, gatewayRouter, err)
	}

	// Connect the external_switch to the router.
	externalSwitchPortToRouter := types.EXTSwitchToGWRouterPrefix + gatewayRouter

	externalLogicalSwitchPortToRouter := nbdb.LogicalSwitchPort{
		Name: externalSwitchPortToRouter,
		Type: "router",
		Options: map[string]string{
			"router-port": externalRouterPort,
		},
		Addresses: []string{macAddress},
	}
	opModels = []libovsdbops.OperationModel{
		{
			Model: &externalLogicalSwitchPortToRouter,
			OnModelUpdates: []interface{}{
				&externalLogicalSwitchPortToRouter.Addresses,
				&externalLogicalSwitchPortToRouter.Type,
				&externalLogicalSwitchPortToRouter.Options,
			},
			DoAfter: func() {
				externalLogicalSwitch.Ports = []string{externalLogicalSwitchPortToRouter.UUID}
			},
		},
		{
			Model:          &externalLogicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == externalSwitch },
			OnModelMutations: []interface{}{
				&externalLogicalSwitch.Ports,
			},
			ErrNotFound: true,
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical switch port: %s to switch %s, err: %v", externalSwitchPortToRouter, externalSwitch, err)
	}
	return nil
}

func (oc *Controller) addPolicyBasedRoutes(nodeName, mgmtPortIP string, hostIfAddr *net.IPNet, otherHostAddrs []string) error {
	var l3Prefix string
	var natSubnetNextHop string
	if utilnet.IsIPv6(hostIfAddr.IP) {
		l3Prefix = "ip6"
		natSubnetNextHop = types.V6NodeLocalNATSubnetNextHop
	} else {
		l3Prefix = "ip4"
		natSubnetNextHop = types.V4NodeLocalNATSubnetNextHop
	}

	matches := sets.NewString()
	for _, hostIP := range append(otherHostAddrs, hostIfAddr.IP.String()) {
		// embed nodeName as comment so that it is easier to delete these rules later on.
		// logical router policy doesn't support external_ids to stash metadata
		matchStr := fmt.Sprintf(`inport == "%s%s" && %s.dst == %s /* %s */`,
			types.RouterToSwitchPrefix, nodeName, l3Prefix, hostIP, nodeName)
		matches = matches.Insert(matchStr)
	}
	if err := oc.syncPolicyBasedRoutes(nodeName, matches, types.NodeSubnetPolicyPriority, mgmtPortIP); err != nil {
		return fmt.Errorf("unable to sync node subnet policies, err: %v", err)
	}

	if config.Gateway.Mode == config.GatewayModeLocal {

		// policy to allow host -> service -> hairpin back to host
		matchStr := fmt.Sprintf("%s.src == %s && %s.dst == %s /* %s */",
			l3Prefix, mgmtPortIP, l3Prefix, hostIfAddr.IP.String(), nodeName)
		if err := oc.syncPolicyBasedRoutes(nodeName, sets.NewString(matchStr), types.MGMTPortPolicyPriority, natSubnetNextHop); err != nil {
			return fmt.Errorf("unable to sync management port policies, err: %v", err)
		}

		var matchDst string
		// Local gw mode needs to use DGP to do hostA -> service -> hostB
		var clusterL3Prefix string
		for _, clusterSubnet := range config.Default.ClusterSubnets {
			if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
				clusterL3Prefix = "ip6"
			} else {
				clusterL3Prefix = "ip4"
			}
			if l3Prefix != clusterL3Prefix {
				continue
			}
			matchDst += fmt.Sprintf(" && %s.dst != %s", clusterL3Prefix, clusterSubnet.CIDR)
		}
		matchStr = fmt.Sprintf("%s.src == %s %s /* inter-%s */",
			l3Prefix, mgmtPortIP, matchDst, nodeName)
		if err := oc.syncPolicyBasedRoutes(nodeName, sets.NewString(matchStr), types.InterNodePolicyPriority, natSubnetNextHop); err != nil {
			return fmt.Errorf("unable to sync inter-node policies, err: %v", err)
		}
	}

	return nil
}

// This function syncs logical router policies given various criteria
// This function compares the following ovn-nbctl output:

// either

// 		72db5e49-0949-4d00-93e3-fe94442dd861,ip4.src == 10.244.0.2 && ip4.dst == 172.18.0.2 /* ovn-worker2 */,169.254.0.1
// 		6465e223-053c-4c74-a5f0-5f058c9f7a3e,ip4.src == 10.244.2.2 && ip4.dst == 172.18.0.3 /* ovn-worker */,169.254.0.1
// 		7debdcc6-ad5e-4825-9978-74bfbf8b7c27,ip4.src == 10.244.1.2 && ip4.dst == 172.18.0.4 /* ovn-control-plane */,169.254.0.1

// or

// 		c20ac671-704a-428a-a32b-44da2eec8456,"inport == ""rtos-ovn-worker2"" && ip4.dst == 172.18.0.2 /* ovn-worker2 */",10.244.0.2
// 		be7c8b53-f8ac-4051-b8f1-bfdb007d0956,"inport == ""rtos-ovn-worker"" && ip4.dst == 172.18.0.3 /* ovn-worker */",10.244.2.2
// 		fa8cf55d-a96c-4a53-9bf2-1c1fb1bc7a42,"inport == ""rtos-ovn-control-plane"" && ip4.dst == 172.18.0.4 /* ovn-control-plane */",10.244.1.2

// or

// 		822ab242-cce5-47b2-9c6f-f025f47e766a,ip4.src == 10.244.2.2  && ip4.dst != 10.244.0.0/16 /* inter-ovn-worker */,169.254.0.1
// 		a1b876f6-5ed4-4f88-b09c-7b4beed3b75f,ip4.src == 10.244.1.2  && ip4.dst != 10.244.0.0/16 /* inter-ovn-control-plane */,169.254.0.1
// 		0f5af297-74c8-4551-b10e-afe3b74bb000,ip4.src == 10.244.0.2  && ip4.dst != 10.244.0.0/16 /* inter-ovn-worker2 */,169.254.0.1

// The function checks to see if the mgmtPort IP has changed, or if match criteria has changed
// and removes stale policies for a node for the NodeSubnetPolicy in SGW.
// TODO: Fix the MGMTPortPolicy's and InterNodePolicy's ip4.src fields if the mgmtPort IP has changed in LGW.
// It also adds new policies for a node at a specific priority.
// This is ugly (since the node is encoded as a comment in the match),
// but a necessary evil as any change to this would break upgrades and
// possible downgrades. We could make sure any upgrade encodes the node in
// the external_id, but since ovn-kubernetes isn't versioned, we won't ever
// know which version someone is running of this and when the switch to version
// N+2 is fully made.
func (oc *Controller) syncPolicyBasedRoutes(nodeName string, matches sets.String, priority, nexthop string) error {
	// create a map to track matches found
	matchTracker := sets.NewString(matches.List()...)

	if priority == types.NodeSubnetPolicyPriority {
		policies, err := oc.findPolicyBasedRoutes(priority)
		if err != nil {
			return fmt.Errorf("unable to list policies, err: %v", err)
		}

		// sync and remove unknown policies for this node/priority
		// also flag if desired policies are already found
		for _, policy := range policies {
			if strings.Contains(policy.Match, fmt.Sprintf("%s\"", nodeName)) {
				// if the policy is for this node and has the wrong mgmtPortIP as nexthop, remove it
				// FIXME we currently assume that foundNexthops is a single ip, this may
				// change in the future.

				if policy.Nexthops != nil && utilnet.IsIPv6String(policy.Nexthops[0]) != utilnet.IsIPv6String(nexthop) {
					continue
				}
				if policy.Nexthops[0] != nexthop {
					if err := oc.deletePolicyBasedRoutes(policy.UUID, priority); err != nil {
						return fmt.Errorf("failed to delete policy route '%s' for host %q on %s "+
							"error: %v", policy.UUID, nodeName, types.OVNClusterRouter, err)
					}
					continue
				}
				desiredMatchFound := false
				for match := range matchTracker {
					if strings.Contains(policy.Match, match) {
						desiredMatchFound = true
						break
					}
				}
				// if the policy is for this node/priority and does not contain a valid match, remove it
				if !desiredMatchFound {
					if err := oc.deletePolicyBasedRoutes(policy.UUID, priority); err != nil {
						return fmt.Errorf("failed to delete policy route '%s' for host %q on %s "+
							"error: %v", policy.UUID, nodeName, types.OVNClusterRouter, err)
					}
					continue
				}
				// now check if the existing policy matches, remove it
				matchTracker.Delete(policy.Match)
			}
		}
	}

	// cycle through all of the not found match criteria and create new policies
	for match := range matchTracker {
		if err := oc.createPolicyBasedRoutes(match, priority, nexthop); err != nil {
			return fmt.Errorf("failed to add policy route '%s' for host %q on %s "+
				"error: %v", match, nodeName, types.OVNClusterRouter, err)
		}
	}
	return nil
}

func (oc *Controller) findPolicyBasedRoutes(priority string) ([]nbdb.LogicalRouterPolicy, error) {
	intPriority, _ := strconv.Atoi(priority)
	logicalRouterPolicyResult := []nbdb.LogicalRouterPolicy{}
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	err := oc.nbClient.WhereCache(func(lrp *nbdb.LogicalRouterPolicy) bool {
		return lrp.Priority == intPriority
	}).List(ctx, &logicalRouterPolicyResult)
	if err != nil {
		return nil, fmt.Errorf("unable to find logical router policy, err: %v", err)
	}
	return logicalRouterPolicyResult, nil
}

func (oc *Controller) createPolicyBasedRoutes(match, priority, nexthops string) error {
	intPriority, _ := strconv.Atoi(priority)

	logicalRouterPolicy := nbdb.LogicalRouterPolicy{
		Priority: intPriority,
		Match:    match,
		Nexthops: []string{nexthops},
		Action:   nbdb.LogicalRouterPolicyActionReroute,
	}
	logicalRouter := nbdb.LogicalRouter{}
	opModels := []libovsdbops.OperationModel{
		{
			Model: &logicalRouterPolicy,
			ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
				return lrp.Priority == intPriority && lrp.Match == match
			},
			OnModelUpdates: []interface{}{
				&logicalRouterPolicy.Nexthops,
				&logicalRouterPolicy.Match,
			},
			DoAfter: func() {
				if logicalRouterPolicy.UUID != "" {
					logicalRouter.Policies = []string{logicalRouterPolicy.UUID}
				}
			},
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Policies,
			},
			ErrNotFound: true,
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("unable to create policy based routes, err: %v", err)
	}
	return nil
}

func (oc *Controller) deletePolicyBasedRoutes(policyID, priority string) error {
	logicalRouterPolicy := nbdb.LogicalRouterPolicy{
		UUID: policyID,
	}
	logicalRouter := nbdb.LogicalRouter{
		Policies: []string{policyID},
	}
	opModels := []libovsdbops.OperationModel{
		{
			Model: &logicalRouterPolicy,
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: []interface{}{
				&logicalRouter.Policies,
			},
		},
	}
	if err := oc.modelClient.Delete(opModels...); err != nil {
		return fmt.Errorf("unable to delete logical router policy, err: %v", err)
	}
	return nil
}

func (oc *Controller) addNodeLocalNatEntries(node *kapi.Node, mgmtPortMAC string, mgmtPortIfAddr *net.IPNet) error {
	var externalIP net.IP

	isIPv6 := utilnet.IsIPv6CIDR(mgmtPortIfAddr)
	annotationPresent := false
	externalIPs, err := util.ParseNodeLocalNatIPAnnotation(node)
	if err == nil {
		for _, ip := range externalIPs {
			if isIPv6 == utilnet.IsIPv6(ip) {
				klog.V(5).Infof("Found node local NAT IP %s in %v for the node %s, so reusing it", ip, externalIPs, node.Name)
				externalIP = ip
				annotationPresent = true
				break
			}
		}
	}
	if !annotationPresent {
		if isIPv6 {
			externalIP, err = oc.nodeLocalNatIPv6Allocator.AllocateNext()
		} else {
			externalIP, err = oc.nodeLocalNatIPv4Allocator.AllocateNext()
		}
		if err != nil {
			return fmt.Errorf("error allocating node local NAT IP for node %s: %v", node.Name, err)
		}
		externalIPs = append(externalIPs, externalIP)
		defer func() {
			// Release the allocation on error
			if err != nil {
				if isIPv6 {
					_ = oc.nodeLocalNatIPv6Allocator.Release(externalIP)
				} else {
					_ = oc.nodeLocalNatIPv4Allocator.Release(externalIP)
				}
			}
		}()
	}

	mgmtPortName := types.K8sPrefix + node.Name
	nat := libovsdbops.BuildRouterDNATAndSNAT(&externalIP, mgmtPortIfAddr, mgmtPortName, mgmtPortMAC, nil)
	err = libovsdbops.AddOrUpdateNatsToRouter(oc.nbClient, types.OVNClusterRouter, nat)
	if err != nil {
		return fmt.Errorf("failed to add dnat_and_snat entry for the management port on node %s, "+
			"error: %v", node.Name, err)
	}

	if annotationPresent {
		return nil
	}
	// capture the node local NAT IP as a node annotation so that we can re-create it on onvkube-restart
	nodeAnnotations, err := util.CreateNodeLocalNatAnnotation(externalIPs)
	if err != nil {
		return fmt.Errorf("failed to marshal node %q annotation for node local NAT IP %s",
			node.Name, externalIP.String())
	}
	err = oc.kube.SetAnnotationsOnNode(node.Name, nodeAnnotations)
	if err != nil {
		return fmt.Errorf("failed to set node local NAT IP annotation on node %s: %v",
			node.Name, err)
	}
	return nil
}

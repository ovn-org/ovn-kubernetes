package ovn

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/pkg/errors"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/gateway"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// cleanupStalePodSNATs removes SNATs against nodeIP for the given node if the SNAT.logicalIP isn't an active podIP on this node
// We don't have to worry about missing SNATs that should be added because addLogicalPort takes care of this for all pods
// when RequestRetryObjs is called for each node add.
// This is executed only when disableSNATMultipleGateways = true
//
// NOTE: On startup libovsdb adds back all the pods and this should normally update all existing SNATs
// accordingly. Due to a stale egressIP cache bug https://issues.redhat.com/browse/OCPBUGS-1520 we ended up adding
// wrong pod->nodeSNATs which won't get cleared up unless explicitly deleted.
// NOTE2: egressIP SNATs are synced in EIP controller.
// TODO (tssurya): Add support cleaning up even if disableSNATMultipleGWs=false, we'd need to remove the perPod
// SNATs in case someone switches between these modes. See https://github.com/ovn-org/ovn-kubernetes/issues/3232
func (oc *DefaultNetworkController) cleanupStalePodSNATs(nodeName string, nodeIPs []*net.IPNet) error {
	if !config.Gateway.DisableSNATMultipleGWs {
		return nil
	}
	options := metav1.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
		ResourceVersion: "0",
	}
	pods, err := oc.client.CoreV1().Pods(metav1.NamespaceAll).List(context.TODO(), options)
	if err != nil {
		return fmt.Errorf("unable to list existing pods on node: %s, %w",
			nodeName, err)
	}
	gatewayRouter := nbdb.LogicalRouter{
		Name: types.GWRouterPrefix + nodeName,
	}
	routerNats, err := libovsdbops.GetRouterNATs(oc.nbClient, &gatewayRouter)
	if err != nil && errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("unable to get NAT entries for router on node %s: %w", nodeName, err)
	}
	podIPsOnNode := sets.NewString() // collects all podIPs on node
	for _, pod := range pods.Items {
		pod := pod
		podIPs, err := util.GetPodIPsOfNetwork(&pod, oc.NetInfo)
		if err != nil {
			return fmt.Errorf("unable to fetch podIPs for pod %s/%s", pod.Namespace, pod.Name)
		}
		for _, podIP := range podIPs {
			podIPsOnNode.Insert(podIP.String())
		}
	}
	natsToDelete := []*nbdb.NAT{}
	for _, routerNat := range routerNats {
		routerNat := routerNat
		if routerNat.Type != nbdb.NATTypeSNAT {
			continue
		}
		for _, nodeIP := range nodeIPs {
			if routerNat.ExternalIP == nodeIP.IP.String() && !podIPsOnNode.Has(routerNat.LogicalIP) {
				natsToDelete = append(natsToDelete, routerNat)
			}
		}
	}
	if len(natsToDelete) > 0 {
		err := libovsdbops.DeleteNATs(oc.nbClient, &gatewayRouter, natsToDelete...)
		if err != nil {
			return fmt.Errorf("unable to delete NATs %+v from node %s", natsToDelete, nodeName)
		}
	}
	return nil
}

// gatewayInit creates a gateway router for the local chassis.
// enableGatewayMTU enables options:gateway_mtu for gateway routers.
func (oc *DefaultNetworkController) gatewayInit(nodeName string, clusterIPSubnet []*net.IPNet, hostSubnets []*net.IPNet,
	l3GatewayConfig *util.L3GatewayConfig, sctpSupport bool, gwLRPIfAddrs, drLRPIfAddrs []*net.IPNet,
	enableGatewayMTU bool) error {

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
		Copp:        &oc.defaultCOPPUUID,
	}

	if oc.loadBalancerGroupUUID != "" {
		logicalRouter.LoadBalancerGroup = []string{oc.loadBalancerGroupUUID}
	}

	// If l3gatewayAnnotation.IPAddresses changed, we need to update the perPodSNATs,
	// so let's save the old value before we update the router for later use
	var oldExtIPs []net.IP
	oldLogicalRouter, err := libovsdbops.GetLogicalRouter(oc.nbClient, &logicalRouter)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return fmt.Errorf("failed in retrieving %s, error: %v", gatewayRouter, err)
	}

	if oldLogicalRouter != nil && oldLogicalRouter.ExternalIDs != nil {
		if physicalIPs, ok := oldLogicalRouter.ExternalIDs["physical_ips"]; ok {
			oldExternalIPs := strings.Split(physicalIPs, ",")
			oldExtIPs = make([]net.IP, len(oldExternalIPs))
			for i, oldExternalIP := range oldExternalIPs {
				cidr := oldExternalIP + util.GetIPFullMask(oldExternalIP)
				ip, _, err := net.ParseCIDR(cidr)
				if err != nil {
					return fmt.Errorf("invalid cidr:%s error: %v", cidr, err)
				}
				oldExtIPs[i] = ip
			}
		}
	}

	err = libovsdbops.CreateOrUpdateLogicalRouter(oc.nbClient, &logicalRouter, &logicalRouter.Options,
		&logicalRouter.ExternalIDs, &logicalRouter.LoadBalancerGroup, &logicalRouter.Copp)
	if err != nil {
		return fmt.Errorf("failed to create logical router %+v: %v", logicalRouter, err)
	}

	gwSwitchPort := types.JoinSwitchToGWRouterPrefix + gatewayRouter
	gwRouterPort := types.GWRouterToJoinSwitchPrefix + gatewayRouter

	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      gwSwitchPort,
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			"router-port": gwRouterPort,
		},
	}
	sw := nbdb.LogicalSwitch{Name: types.OVNJoinSwitch}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return fmt.Errorf("failed to create port %v on logical switch %q: %v", gwSwitchPort, types.OVNJoinSwitch, err)
	}

	gwLRPMAC := util.IPAddrToHWAddr(gwLRPIPs[0])
	gwLRPNetworks := []string{}
	for _, gwLRPIfAddr := range gwLRPIfAddrs {
		gwLRPNetworks = append(gwLRPNetworks, gwLRPIfAddr.String())
	}

	var options map[string]string
	if enableGatewayMTU {
		options = map[string]string{
			"gateway_mtu": strconv.Itoa(config.Default.MTU),
		}
	}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     gwRouterPort,
		MAC:      gwLRPMAC.String(),
		Networks: gwLRPNetworks,
		Options:  options,
	}

	err = libovsdbops.CreateOrUpdateLogicalRouterPort(oc.nbClient, &logicalRouter,
		&logicalRouterPort, nil, &logicalRouterPort.MAC, &logicalRouterPort.Networks,
		&logicalRouterPort.Options)
	if err != nil {
		return fmt.Errorf("failed to create port %+v on router %+v: %v", logicalRouterPort, logicalRouter, err)
	}

	for _, entry := range clusterIPSubnet {
		drLRPIfAddr, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(entry), drLRPIfAddrs)
		if err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with distributed "+
				"router as the nexthop: %v",
				gatewayRouter, err)
		}

		// TODO There has to be a better way to do this. It seems like the
		// whole purpose is to update the appropriate route in case it already
		// exists *only* in the context of this router. But then it does not
		// make sense to refresh it on every loop, unless it is also way to
		// check for duplicate cluster IP subnets for which there would also be
		// a better way to do it. Adding support for indirection in ModelClients
		// opModel (being able to operate on thins pointed to from another model)
		// would be agreat way to simplify this.
		updatedLogicalRouter, err := libovsdbops.GetLogicalRouter(oc.nbClient, &logicalRouter)
		if err != nil {
			return fmt.Errorf("unable to retrieve logical router %+v: %v", logicalRouter, err)
		}

		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix: entry.String(),
			Nexthop:  drLRPIfAddr.IP.String(),
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.IPPrefix == lrsr.IPPrefix && libovsdbops.PolicyEqualPredicate(item.Policy, lrsr.Policy) &&
				util.SliceHasStringItem(updatedLogicalRouter.StaticRoutes, item.UUID)
		}
		err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(oc.nbClient, gatewayRouter, &lrsr, p,
			&lrsr.Nexthop)
		if err != nil {
			return fmt.Errorf("failed to add a static route %+v in GR %s with distributed router as the nexthop, err: %v", lrsr, gatewayRouter, err)
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

	nextHops := l3GatewayConfig.NextHops

	if err := gateway.CreateDummyGWMacBindings(oc.sbClient, nodeName); err != nil {
		return err
	}

	for _, nextHop := range node.DummyNextHopIPs() {
		// Add return service route for OVN back to host
		prefix := types.V4MasqueradeSubnet
		if utilnet.IsIPv6(nextHop) {
			prefix = types.V6MasqueradeSubnet
		}
		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix:   prefix,
			Nexthop:    nextHop.String(),
			OutputPort: &externalRouterPort,
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.OutputPort != nil && *item.OutputPort == *lrsr.OutputPort && item.IPPrefix == lrsr.IPPrefix &&
				libovsdbops.PolicyEqualPredicate(item.Policy, lrsr.Policy)
		}
		err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(oc.nbClient, gatewayRouter, &lrsr, p,
			&lrsr.Nexthop)
		if err != nil {
			return fmt.Errorf("error creating service static route %+v in GR %s: %v", lrsr, gatewayRouter, err)
		}
	}

	// Add default gateway routes in GR
	for _, nextHop := range nextHops {
		var allIPs string
		if utilnet.IsIPv6(nextHop) {
			allIPs = "::/0"
		} else {
			allIPs = "0.0.0.0/0"
		}

		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix:   allIPs,
			Nexthop:    nextHop.String(),
			OutputPort: &externalRouterPort,
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.OutputPort != nil && *item.OutputPort == *lrsr.OutputPort && item.IPPrefix == lrsr.IPPrefix &&
				libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
		}
		err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(oc.nbClient, gatewayRouter, &lrsr,
			p, &lrsr.Nexthop)
		if err != nil {
			return fmt.Errorf("error creating static route %+v in GR %s: %v", lrsr, gatewayRouter, err)
		}
	}

	// We need to add a route to the Gateway router's IP, on the
	// cluster router, to ensure that the return traffic goes back
	// to the same gateway router
	//
	// This can be removed once https://bugzilla.redhat.com/show_bug.cgi?id=1891516 is fixed.
	for _, gwLRPIP := range gwLRPIPs {
		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix: gwLRPIP.String(),
			Nexthop:  gwLRPIP.String(),
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.IPPrefix == lrsr.IPPrefix &&
				libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
		}
		err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(oc.nbClient,
			types.OVNClusterRouter, &lrsr, p, &lrsr.Nexthop)
		if err != nil {
			return fmt.Errorf("error creating static route %+v in %s: %v", lrsr, types.OVNClusterRouter, err)
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

		lrsr := nbdb.LogicalRouterStaticRoute{
			Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
			IPPrefix: hostSubnet.String(),
			Nexthop:  gwLRPIP[0].String(),
		}

		if config.Gateway.Mode != config.GatewayModeLocal {
			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == lrsr.IPPrefix && libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
			}
			// If migrating from local to shared gateway, let's remove the static routes towards
			// management port interface for the hostSubnet prefix before adding the routes
			// towards join switch.
			mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
			oc.staticRouteCleanup([]net.IP{mgmtIfAddr.IP})

			err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(oc.nbClient, types.OVNClusterRouter,
				&lrsr, p, &lrsr.Nexthop)
			if err != nil {
				return fmt.Errorf("error creating static route %+v in GR %s: %v", lrsr, types.OVNClusterRouter, err)
			}
		} else if config.Gateway.Mode == config.GatewayModeLocal {
			// If migrating from shared to local gateway, let's remove the static routes towards
			// join switch for the hostSubnet prefix
			// Note syncManagementPort happens before gateway sync so only remove things pointing to join subnet

			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == lrsr.IPPrefix && item.Policy != nil && *item.Policy == *lrsr.Policy &&
					config.ContainsJoinIP(net.ParseIP(item.Nexthop))
			}
			err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(oc.nbClient, types.OVNClusterRouter, p)
			if err != nil {
				return fmt.Errorf("error deleting static route %+v in GR %s: %v", lrsr, types.OVNClusterRouter, err)
			}
		}
	}

	// if config.Gateway.DisabledSNATMultipleGWs is not set (by default it is not),
	// the NAT rules for pods not having annotations to route through either external
	// gws or pod CNFs will be added within pods.go addLogicalPort
	externalIPs := make([]net.IP, len(l3GatewayConfig.IPAddresses))
	for i, ip := range l3GatewayConfig.IPAddresses {
		externalIPs[i] = ip.IP
	}
	var natsToUpdate []*nbdb.NAT
	// If l3gatewayAnnotation.IPAddresses changed, we need to update the SNATs on the GR
	if len(oldExtIPs) > 0 {
		for _, externalIP := range externalIPs {
			oldExternalIP, err := util.MatchIPFamily(utilnet.IsIPv6(externalIP), oldExtIPs)
			if err != nil {
				return fmt.Errorf("failed to update GW SNAT rule for pods on router %s error: %v", gatewayRouter, err)
			}
			if externalIP.String() != oldExternalIP[0].String() {
				predicate := func(item *nbdb.NAT) bool {
					return item.ExternalIP == oldExternalIP[0].String() && item.Type == nbdb.NATTypeSNAT
				}
				natsToUpdate, err = libovsdbops.FindNATsWithPredicate(oc.nbClient, predicate)
				if err != nil {
					return fmt.Errorf("failed to update GW SNAT rule for pods on router %s error: %v", gatewayRouter, err)
				}
				for i := 0; i < len(natsToUpdate); i++ {
					natsToUpdate[i].ExternalIP = externalIP.String()
				}
			}
		}
		err := libovsdbops.CreateOrUpdateNATs(oc.nbClient, &logicalRouter, natsToUpdate...)
		if err != nil {
			return fmt.Errorf("failed to update GW SNAT rule for pod on router %s error: %v", gatewayRouter, err)
		}
	}
	nats := make([]*nbdb.NAT, 0, len(clusterIPSubnet))
	var nat *nbdb.NAT
	if !config.Gateway.DisableSNATMultipleGWs {
		// Default SNAT rules. DisableSNATMultipleGWs=false in LGW (traffic egresses via mp0) always.
		// We are not checking for gateway mode to be shared explicitly to reduce topology differences.
		for _, entry := range clusterIPSubnet {
			externalIP, err := util.MatchIPFamily(utilnet.IsIPv6CIDR(entry), externalIPs)
			if err != nil {
				return fmt.Errorf("failed to create default SNAT rules for gateway router %s: %v",
					gatewayRouter, err)
			}
			nat = libovsdbops.BuildSNAT(&externalIP[0], entry, "", nil)
			nats = append(nats, nat)
		}
		err := libovsdbops.CreateOrUpdateNATs(oc.nbClient, &logicalRouter, nats...)
		if err != nil {
			return fmt.Errorf("failed to update SNAT rule for pod on router %s error: %v", gatewayRouter, err)
		}
	} else {
		// ensure we do not have any leftover SNAT entries after an upgrade
		for _, logicalSubnet := range clusterIPSubnet {
			nat = libovsdbops.BuildSNAT(nil, logicalSubnet, "", nil)
			nats = append(nats, nat)
		}
		err := libovsdbops.DeleteNATs(oc.nbClient, &logicalRouter, nats...)
		if err != nil {
			return fmt.Errorf("failed to delete GW SNAT rule for pod on router %s error: %v", gatewayRouter, err)
		}
	}

	if err := oc.cleanupStalePodSNATs(nodeName, l3GatewayConfig.IPAddresses); err != nil {
		return fmt.Errorf("failed to sync stale SNATs on node %s: %v", nodeName, err)
	}

	// recording gateway mode metrics here after gateway setup is done
	metrics.RecordEgressRoutingViaHost()

	return nil
}

// addExternalSwitch creates a switch connected to the external bridge and connects it to
// the gateway router
func (oc *DefaultNetworkController) addExternalSwitch(prefix, interfaceID, nodeName, gatewayRouter, macAddress, physNetworkName string, ipAddresses []*net.IPNet, vlanID *uint) error {
	// Create the GR port that connects to external_switch with mac address of
	// external interface and that IP address. In the case of `local` gateway
	// mode, whenever ovnkube-node container restarts a new br-local bridge will
	// be created with a new `nicMacAddress`.
	externalRouterPort := prefix + types.GWRouterToExtSwitchPrefix + gatewayRouter

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
	logicalRouter := nbdb.LogicalRouter{Name: gatewayRouter}

	err := libovsdbops.CreateOrUpdateLogicalRouterPort(oc.nbClient, &logicalRouter,
		&externalLogicalRouterPort, nil, &externalLogicalRouterPort.MAC,
		&externalLogicalRouterPort.Networks, &externalLogicalRouterPort.ExternalIDs)
	if err != nil {
		return fmt.Errorf("failed to add logical router port %+v to router %s: %v", externalLogicalRouterPort, gatewayRouter, err)
	}

	// Create the external switch for the physical interface to connect to
	// and add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	externalSwitch := externalSwitchName(prefix, nodeName)
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

	// Also add the port to connect the external_switch to the router.
	externalSwitchPortToRouter := prefix + types.EXTSwitchToGWRouterPrefix + gatewayRouter
	externalLogicalSwitchPortToRouter := nbdb.LogicalSwitchPort{
		Name: externalSwitchPortToRouter,
		Type: "router",
		Options: map[string]string{
			"router-port": externalRouterPort,
		},
		Addresses: []string{macAddress},
	}
	sw := nbdb.LogicalSwitch{Name: externalSwitch}

	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsAndSwitch(oc.nbClient, &sw, &externalLogicalSwitchPort, &externalLogicalSwitchPortToRouter)
	if err != nil {
		return fmt.Errorf("failed to create logical switch ports %+v, %+v, and switch %s: %v",
			externalLogicalSwitchPort, externalLogicalSwitchPortToRouter, externalSwitch, err)
	}

	return nil
}

func (oc *DefaultNetworkController) addPolicyBasedRoutes(nodeName, mgmtPortIP string, hostIfAddr *net.IPNet, otherHostAddrs []string) error {
	var l3Prefix string
	if utilnet.IsIPv6(hostIfAddr.IP) {
		l3Prefix = "ip6"
	} else {
		l3Prefix = "ip4"
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
func (oc *DefaultNetworkController) syncPolicyBasedRoutes(nodeName string, matches sets.String, priority, nexthop string) error {
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

func (oc *DefaultNetworkController) findPolicyBasedRoutes(priority string) ([]*nbdb.LogicalRouterPolicy, error) {
	intPriority, _ := strconv.Atoi(priority)
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == intPriority
	}
	logicalRouterStaticPolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(oc.nbClient, p)
	if err != nil {
		return nil, fmt.Errorf("unable to find logical router policy: %v", err)
	}

	return logicalRouterStaticPolicies, nil
}

func (oc *DefaultNetworkController) createPolicyBasedRoutes(match, priority, nexthops string) error {
	intPriority, _ := strconv.Atoi(priority)
	lrp := nbdb.LogicalRouterPolicy{
		Priority: intPriority,
		Match:    match,
		Nexthops: []string{nexthops},
		Action:   nbdb.LogicalRouterPolicyActionReroute,
	}

	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == lrp.Priority && item.Match == lrp.Match
	}

	err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(oc.nbClient, types.OVNClusterRouter, &lrp, p,
		&lrp.Nexthops, &lrp.Action)
	if err != nil {
		return fmt.Errorf("error creating policy %+v on router %s: %v", lrp, types.OVNClusterRouter, err)
	}

	return nil
}

func (oc *DefaultNetworkController) deletePolicyBasedRoutes(policyID, priority string) error {
	lrp := nbdb.LogicalRouterPolicy{UUID: policyID}
	err := libovsdbops.DeleteLogicalRouterPolicies(oc.nbClient, types.OVNClusterRouter, &lrp)
	if err != nil {
		return fmt.Errorf("error deleting policy %s: %v", policyID, err)
	}

	return nil
}

func externalSwitchName(prefix string, nodeName string) string {
	return fmt.Sprintf("%s%s%s", prefix, types.ExternalSwitchPrefix, nodeName)
}

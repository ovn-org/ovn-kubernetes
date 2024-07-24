package ovn

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/gateway"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/gatewayrouter"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type GatewayManager struct {
	nodeName          string
	clusterRouterName string
	gwRouterName      string
	extSwitchName     string
	transitSwitchName string
	coopUUID          string
	kube              kube.InterfaceOVN
	nbClient          libovsdbclient.Client
	netInfo           util.NetInfo
	watchFactory      *factory.WatchFactory

	// Cluster wide Load_Balancer_Group UUID.
	// Includes all node switches and node gateway routers.
	clusterLoadBalancerGroupUUID string

	// Cluster wide switch Load_Balancer_Group UUID.
	// Includes all node switches.
	switchLoadBalancerGroupUUID string

	// Cluster wide router Load_Balancer_Group UUID.
	// Includes all node gateway routers.
	routerLoadBalancerGroupUUID string
}

type GatewayOption func(*GatewayManager)

func NewGatewayManager(
	nodeName, clusterRouter, gwRouter, extSwitch, transitSwitch string,
	coopUUID string,
	kube kube.InterfaceOVN,
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	opts ...GatewayOption,
) *GatewayManager {
	gwManager := &GatewayManager{
		nodeName:          nodeName,
		clusterRouterName: clusterRouter,
		gwRouterName:      gwRouter,
		extSwitchName:     extSwitch,
		transitSwitchName: transitSwitch,
		coopUUID:          coopUUID,
		kube:              kube,
		nbClient:          nbClient,
		netInfo:           netInfo,
	}

	for _, opt := range opts {
		opt(gwManager)
	}

	return gwManager
}

func WithLoadBalancerGroups(routerLBGroup, clusterLBGroup, switchLBGroup string) GatewayOption {
	return func(manager *GatewayManager) {
		manager.routerLoadBalancerGroupUUID = routerLBGroup
		manager.clusterLoadBalancerGroupUUID = clusterLBGroup
		manager.switchLoadBalancerGroupUUID = switchLBGroup
	}
}

// cleanupStalePodSNATs removes SNATs against nodeIP for the given node if the SNAT.logicalIP isn't an active podIP on this node
// We don't have to worry about missing SNATs that should be added because addLogicalPort takes care of this for all pods
// when RequestRetryObjs is called for each node add.
// This is executed only when disableSNATMultipleGateways = true
// SNATs configured for the join subnet are ignored.
//
// NOTE: On startup libovsdb adds back all the pods and this should normally update all existing SNATs
// accordingly. Due to a stale egressIP cache bug https://issues.redhat.com/browse/OCPBUGS-1520 we ended up adding
// wrong pod->nodeSNATs which won't get cleared up unless explicitly deleted.
// NOTE2: egressIP SNATs are synced in EIP controller.
// TODO (tssurya): Add support cleaning up even if disableSNATMultipleGWs=false, we'd need to remove the perPod
// SNATs in case someone switches between these modes. See https://github.com/ovn-org/ovn-kubernetes/issues/3232
func (gw *GatewayManager) cleanupStalePodSNATs(nodeName string, nodeIPs []*net.IPNet) error {
	if !config.Gateway.DisableSNATMultipleGWs {
		return nil
	}
	pods, err := gw.kube.GetPods(metav1.NamespaceAll, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
	})
	if err != nil {
		return fmt.Errorf("unable to list existing pods on node: %s, %w",
			nodeName, err)
	}
	gatewayRouter := nbdb.LogicalRouter{
		Name: gw.gwRouterName,
	}
	routerNats, err := libovsdbops.GetRouterNATs(gw.nbClient, &gatewayRouter)
	if err != nil && errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("unable to get NAT entries for router %s on node %s: %w", gatewayRouter.Name, nodeName, err)
	}
	podIPsOnNode := sets.NewString() // collects all podIPs on node
	for _, pod := range pods {
		pod := *pod
		if !util.PodScheduled(&pod) { //if the pod is not scheduled we should not remove the nat
			continue
		}
		if util.PodCompleted(&pod) {
			collidingPod, err := findPodWithIPAddresses(gw.watchFactory, gw.netInfo, []net.IP{utilnet.ParseIPSloppy(pod.Status.PodIP)}, "") //even if a pod is completed we should still delete the nat if the ip is not in use anymore
			if err != nil {
				return fmt.Errorf("lookup for pods with same ip as %s %s failed: %w", pod.Namespace, pod.Name, err)
			}
			if collidingPod != nil { //if the ip is in use we should not remove the nat
				continue
			}
		}
		podIPs, err := util.GetPodIPsOfNetwork(&pod, gw.netInfo)
		if err != nil && errors.Is(err, util.ErrNoPodIPFound) {
			// It is possible that the pod is scheduled during this time, but the LSP add or
			// IP Allocation has not happened and it is waiting for the WatchPods to start
			// after WatchNodes completes (This function is called during syncNodes). So since
			// the pod doesn't have any IPs, there is no SNAT here to keep for this pod so we skip
			// this pod from processing and move onto the next one.
			klog.Warningf("Unable to fetch podIPs for pod %s/%s: %v", pod.Namespace, pod.Name, err)
			continue // no-op
		} else if err != nil {
			return fmt.Errorf("unable to fetch podIPs for pod %s/%s: %w", pod.Namespace, pod.Name, err)
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
			logicalIP := net.ParseIP(routerNat.LogicalIP)
			if routerNat.ExternalIP == nodeIP.IP.String() && !config.ContainsJoinIP(logicalIP) && !podIPsOnNode.Has(routerNat.LogicalIP) {
				natsToDelete = append(natsToDelete, routerNat)
			}
		}
	}
	if len(natsToDelete) > 0 {
		err := libovsdbops.DeleteNATs(gw.nbClient, &gatewayRouter, natsToDelete...)
		if err != nil {
			return fmt.Errorf("unable to delete NATs %+v from node %s: %w", natsToDelete, nodeName, err)
		}
	}
	return nil
}

// GatewayInit creates a gateway router for the local chassis.
// enableGatewayMTU enables options:gateway_mtu for gateway routers.
func (gw *GatewayManager) GatewayInit(
	nodeName string,
	clusterIPSubnet []*net.IPNet,
	hostSubnets []*net.IPNet,
	l3GatewayConfig *util.L3GatewayConfig,
	sctpSupport bool,
	gwLRPIfAddrs, drLRPIfAddrs []*net.IPNet,
	externalIPs []net.IP,
	enableGatewayMTU bool,
) error {

	gwLRPIPs := make([]net.IP, 0)
	for _, gwLRPIfAddr := range gwLRPIfAddrs {
		gwLRPIPs = append(gwLRPIPs, gwLRPIfAddr.IP)
	}

	// Create a gateway router.
	gatewayRouter := gw.gwRouterName
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
		"mac_binding_age_threshold":     types.GRMACBindingAgeThreshold,
	}
	logicalRouterExternalIDs := map[string]string{
		"physical_ip":  physicalIPs[0],
		"physical_ips": strings.Join(physicalIPs, ","),
	}

	if gw.netInfo.IsSecondary() {
		networkName := gw.netInfo.GetNetworkName()
		topologyType := gw.netInfo.TopologyType()
		logicalRouterExternalIDs[types.NetworkExternalID] = networkName
		logicalRouterExternalIDs[types.TopologyExternalID] = topologyType
	}

	logicalRouter := nbdb.LogicalRouter{
		Name:        gatewayRouter,
		Options:     logicalRouterOptions,
		ExternalIDs: logicalRouterExternalIDs,
		Copp:        &gw.coopUUID,
	}

	if gw.clusterLoadBalancerGroupUUID != "" {
		logicalRouter.LoadBalancerGroup = []string{gw.clusterLoadBalancerGroupUUID}
		if l3GatewayConfig.NodePortEnable && gw.routerLoadBalancerGroupUUID != "" {
			// add routerLoadBalancerGroupUUID to the gateway router only if nodePort is enabled
			logicalRouter.LoadBalancerGroup = append(logicalRouter.LoadBalancerGroup, gw.routerLoadBalancerGroupUUID)
		}
	}

	// If l3gatewayAnnotation.IPAddresses changed, we need to update the perPodSNATs,
	// so let's save the old value before we update the router for later use
	var oldExtIPs []net.IP
	oldLogicalRouter, err := libovsdbops.GetLogicalRouter(gw.nbClient, &logicalRouter)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed in retrieving %s, error: %v", gatewayRouter, err)
	}

	if oldLogicalRouter != nil && oldLogicalRouter.ExternalIDs != nil {
		if physicalIPs, ok := oldLogicalRouter.ExternalIDs["physical_ips"]; ok {
			oldExternalIPs := strings.Split(physicalIPs, ",")
			oldExtIPs = make([]net.IP, len(oldExternalIPs))
			for i, oldExternalIP := range oldExternalIPs {
				cidr := oldExternalIP + util.GetIPFullMaskString(oldExternalIP)
				ip, _, err := net.ParseCIDR(cidr)
				if err != nil {
					return fmt.Errorf("invalid cidr:%s error: %v", cidr, err)
				}
				oldExtIPs[i] = ip
			}
		}
	}

	err = libovsdbops.CreateOrUpdateLogicalRouter(gw.nbClient, &logicalRouter, &logicalRouter.Options,
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
	if gw.netInfo.IsSecondary() {
		logicalSwitchPort.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}
	sw := nbdb.LogicalSwitch{Name: gw.transitSwitchName}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(gw.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return fmt.Errorf("failed to create port %v on logical switch %q: %v", gwSwitchPort, sw.Name, err)
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
	if gw.netInfo.IsSecondary() {
		logicalRouterPort.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}

	err = libovsdbops.CreateOrUpdateLogicalRouterPort(gw.nbClient, &logicalRouter,
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
		// would be a great way to simplify this.
		updatedLogicalRouter, err := libovsdbops.GetLogicalRouter(gw.nbClient, &logicalRouter)
		if err != nil {
			return fmt.Errorf("unable to retrieve logical router %+v: %v", logicalRouter, err)
		}

		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix: entry.String(),
			Nexthop:  drLRPIfAddr.IP.String(),
		}
		if gw.netInfo.IsSecondary() {
			lrsr.ExternalIDs = map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			}
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.IPPrefix == lrsr.IPPrefix && libovsdbops.PolicyEqualPredicate(item.Policy, lrsr.Policy) &&
				util.SliceHasStringItem(updatedLogicalRouter.StaticRoutes, item.UUID)
		}
		err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient, gatewayRouter, &lrsr, p,
			&lrsr.Nexthop)
		if err != nil {
			return fmt.Errorf("failed to add a static route %+v in GR %s with distributed router as the nexthop, err: %v", lrsr, gatewayRouter, err)
		}
	}

	if err := gw.addExternalSwitch("",
		l3GatewayConfig.InterfaceID,
		nodeName,
		gatewayRouter,
		l3GatewayConfig.MACAddress.String(),
		physNetName(gw.netInfo),
		l3GatewayConfig.IPAddresses,
		l3GatewayConfig.VLANID); err != nil {
		return err
	}

	if l3GatewayConfig.EgressGWInterfaceID != "" {
		if err := gw.addExternalSwitch(types.EgressGWSwitchPrefix,
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

	if err := gateway.CreateDummyGWMacBindings(gw.nbClient, gatewayRouter); err != nil {
		return err
	}

	for _, nextHop := range node.DummyNextHopIPs() {
		// Add return service route for OVN back to host
		prefix := config.Gateway.V4MasqueradeSubnet
		if utilnet.IsIPv6(nextHop) {
			prefix = config.Gateway.V6MasqueradeSubnet
		}
		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix:   prefix,
			Nexthop:    nextHop.String(),
			OutputPort: &externalRouterPort,
		}
		if gw.netInfo.IsSecondary() {
			lrsr.ExternalIDs = map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			}
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.OutputPort != nil && *item.OutputPort == *lrsr.OutputPort && item.IPPrefix == lrsr.IPPrefix &&
				// TODO: maybe this needs to be network scoped
				libovsdbops.PolicyEqualPredicate(item.Policy, lrsr.Policy)
		}
		err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient, gatewayRouter, &lrsr, p,
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
		if gw.netInfo.IsSecondary() {
			lrsr.ExternalIDs = map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			}
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.OutputPort != nil && *item.OutputPort == *lrsr.OutputPort && item.IPPrefix == lrsr.IPPrefix &&
				libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
		}
		err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient, gatewayRouter, &lrsr,
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
	// FIXME(trozet): if LRP IP is changed, we do not remove stale instances of these routes
	for _, gwLRPIP := range gwLRPIPs {
		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix: gwLRPIP.String(),
			Nexthop:  gwLRPIP.String(),
		}
		if gw.netInfo.IsSecondary() {
			lrsr.ExternalIDs = map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			}
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.IPPrefix == lrsr.IPPrefix &&
				libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
		}

		if gw.clusterRouterName != "" {
			err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient,
				gw.clusterRouterName, &lrsr, p, &lrsr.Nexthop)
			if err != nil {
				return fmt.Errorf("error creating static route %+v in %s: %v", lrsr, gw.clusterRouterName, err)
			}
		}
	}

	// Add source IP address based routes in distributed router
	// for this gateway router.
	for _, hostSubnet := range hostSubnets {
		if gw.clusterRouterName == "" {
			break
		}
		gwLRPIP, err := util.MatchIPFamily(utilnet.IsIPv6CIDR(hostSubnet), gwLRPIPs)
		if err != nil {
			return fmt.Errorf("failed to add source IP address based "+
				"routes in distributed router %s: %v",
				gw.clusterRouterName, err)
		}

		lrsr := nbdb.LogicalRouterStaticRoute{
			Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
			IPPrefix: hostSubnet.String(),
			Nexthop:  gwLRPIP[0].String(),
		}

		if config.Gateway.Mode != config.GatewayModeLocal {
			if gw.netInfo.IsSecondary() {
				lrsr.ExternalIDs = map[string]string{
					types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
					types.TopologyExternalID: gw.netInfo.TopologyType(),
				}
			}
			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == lrsr.IPPrefix && libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
			}
			// If migrating from local to shared gateway, let's remove the static routes towards
			// management port interface for the hostSubnet prefix before adding the routes
			// towards join switch.
			mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
			gw.staticRouteCleanup([]net.IP{mgmtIfAddr.IP})

			if gw.clusterRouterName != "" {
				if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(
					gw.nbClient,
					gw.clusterRouterName,
					&lrsr,
					p,
					&lrsr.Nexthop,
				); err != nil {
					return fmt.Errorf("error creating static route %+v in GR %s: %v", lrsr, gw.clusterRouterName, err)
				}
			}
		} else if config.Gateway.Mode == config.GatewayModeLocal {
			// If migrating from shared to local gateway, let's remove the static routes towards
			// join switch for the hostSubnet prefix
			// Note syncManagementPort happens before gateway sync so only remove things pointing to join subnet
			if gw.clusterRouterName != "" {
				p := func(item *nbdb.LogicalRouterStaticRoute) bool {
					return item.IPPrefix == lrsr.IPPrefix && item.Policy != nil && *item.Policy == *lrsr.Policy &&
						config.ContainsJoinIP(net.ParseIP(item.Nexthop))
				}
				err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(gw.nbClient, gw.clusterRouterName, p)
				if err != nil {
					return fmt.Errorf("error deleting static route %+v in GR %s: %v", lrsr, gw.clusterRouterName, err)
				}
			}
		}
	}

	// if config.Gateway.DisabledSNATMultipleGWs is not set (by default it is not),
	// the NAT rules for pods not having annotations to route through either external
	// gws or pod CNFs will be added within pods.go addLogicalPort
	var natsToUpdate []*nbdb.NAT
	// If l3gatewayAnnotation.IPAddresses changed, we need to update the SNATs on the GR
	oldNATs := []*nbdb.NAT{}
	if oldLogicalRouter != nil {
		oldNATs, err = libovsdbops.GetRouterNATs(gw.nbClient, oldLogicalRouter)
		if err != nil && errors.Is(err, libovsdbclient.ErrNotFound) {
			return fmt.Errorf("unable to get NAT entries for router on node %s: %w", nodeName, err)
		}
	}

	for _, nat := range oldNATs {
		nat := nat
		natModified := false

		// if not type snat, we don't need to update as we only configure snat types
		if nat.Type != nbdb.NATTypeSNAT {
			continue
		}
		// check external ip changed
		for _, externalIP := range externalIPs {
			oldExternalIP, err := util.MatchFirstIPFamily(utilnet.IsIPv6(externalIP), oldExtIPs)
			if err != nil {
				return fmt.Errorf("failed to update GW SNAT rule for pods on router %s error: %v", gatewayRouter, err)
			}
			if externalIP.String() == oldExternalIP.String() {
				// no external ip change, skip
				continue
			}
			if nat.ExternalIP == oldExternalIP.String() {
				// needs to be updated
				natModified = true
				nat.ExternalIP = externalIP.String()
			}

		}
		// note, nat.LogicalIP may be a CIDR or IP, we don't care unless it's an IP
		parsedLogicalIP := net.ParseIP(nat.LogicalIP)
		// check if join ip changed
		if config.ContainsJoinIP(parsedLogicalIP) {
			// is a join SNAT, check if IP needs updating
			joinIP, err := util.MatchFirstIPFamily(utilnet.IsIPv6(parsedLogicalIP), gwLRPIPs)
			if err != nil {
				return fmt.Errorf("failed to find valid IP family match for join subnet IP: %s on "+
					"gateway router: %s, provided IPs: %#v", parsedLogicalIP, gatewayRouter, gwLRPIPs)
			}
			if nat.LogicalIP != joinIP.String() {
				// needs to be updated
				natModified = true
				nat.LogicalIP = joinIP.String()
			}
		}
		if natModified {
			natsToUpdate = append(natsToUpdate, nat)
		}
	}

	if len(natsToUpdate) > 0 {
		err = libovsdbops.CreateOrUpdateNATs(gw.nbClient, &logicalRouter, natsToUpdate...)
		if err != nil {
			return fmt.Errorf("failed to update GW SNAT rule for pod on router %s error: %v", gatewayRouter, err)
		}
	}

	// REMOVEME(trozet) workaround - create join subnet SNAT to handle ICMP needs frag return
	var extIDs map[string]string
	if gw.netInfo.IsSecondary() {
		extIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}
	joinNATs := make([]*nbdb.NAT, 0, len(gwLRPIPs))
	for _, gwLRPIP := range gwLRPIPs {
		externalIP, err := util.MatchIPFamily(utilnet.IsIPv6(gwLRPIP), externalIPs)
		if err != nil {
			return fmt.Errorf("failed to find valid external IP family match for join subnet IP: %s on "+
				"gateway router: %s", gwLRPIP, gatewayRouter)
		}
		joinIPNet, err := util.GetIPNetFullMask(gwLRPIP.String())
		if err != nil {
			return fmt.Errorf("failed to parse full CIDR mask for join subnet IP: %s", gwLRPIP)
		}
		nat := libovsdbops.BuildSNAT(&externalIP[0], joinIPNet, "", extIDs)
		joinNATs = append(joinNATs, nat)
	}
	err = libovsdbops.CreateOrUpdateNATs(gw.nbClient, &logicalRouter, joinNATs...)
	if err != nil {
		return fmt.Errorf("failed to create SNAT rule for join subnet on router %s error: %v", gatewayRouter, err)
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
		err := libovsdbops.CreateOrUpdateNATs(gw.nbClient, &logicalRouter, nats...)
		if err != nil {
			return fmt.Errorf("failed to update SNAT rule for pod on router %s error: %v", gatewayRouter, err)
		}
	} else {
		// ensure we do not have any leftover SNAT entries after an upgrade
		for _, logicalSubnet := range clusterIPSubnet {
			nat = libovsdbops.BuildSNAT(nil, logicalSubnet, "", nil)
			nats = append(nats, nat)
		}
		err := libovsdbops.DeleteNATs(gw.nbClient, &logicalRouter, nats...)
		if err != nil {
			return fmt.Errorf("failed to delete GW SNAT rule for pod on router %s error: %v", gatewayRouter, err)
		}
	}

	if err := gw.cleanupStalePodSNATs(nodeName, l3GatewayConfig.IPAddresses); err != nil {
		return fmt.Errorf("failed to sync stale SNATs on node %s: %v", nodeName, err)
	}

	// recording gateway mode metrics here after gateway setup is done
	metrics.RecordEgressRoutingViaHost()

	return nil
}

// addExternalSwitch creates a switch connected to the external bridge and connects it to
// the gateway router
func (gw *GatewayManager) addExternalSwitch(prefix, interfaceID, nodeName, gatewayRouter, macAddress, physNetworkName string, ipAddresses []*net.IPNet, vlanID *uint) error {
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
	if gw.netInfo.IsSecondary() {
		externalLogicalRouterPort.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}
	logicalRouter := nbdb.LogicalRouter{Name: gatewayRouter}

	err := libovsdbops.CreateOrUpdateLogicalRouterPort(gw.nbClient, &logicalRouter,
		&externalLogicalRouterPort, nil, &externalLogicalRouterPort.MAC,
		&externalLogicalRouterPort.Networks, &externalLogicalRouterPort.ExternalIDs,
		&externalLogicalRouterPort.Options)
	if err != nil {
		return fmt.Errorf("failed to add logical router port %+v to router %s: %v", externalLogicalRouterPort, gatewayRouter, err)
	}

	// Create the external switch for the physical interface to connect to
	// and add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	externalSwitch := prefix + gw.extSwitchName
	externalLogicalSwitchPort := nbdb.LogicalSwitchPort{
		Addresses: []string{"unknown"},
		Type:      "localnet",
		Options: map[string]string{
			"network_name": physNetworkName,
		},
		Name: interfaceID,
	}
	if gw.netInfo.IsSecondary() {
		externalLogicalSwitchPort.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}

	if vlanID != nil && int(*vlanID) != 0 {
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

			// This option will program OVN to start sending GARPs for all external IPS
			// that the logical switch port has been configured to use. This is
			// necessary for egress IP because if an egress IP is moved between two
			// nodes, the nodes need to actively update the ARP cache of all neighbors
			// as to notify them the change. If this is not the case: packets will
			// continue to be routed to the old node which hosted the egress IP before
			// it was moved, and the connections will fail.
			"nat-addresses": "router",

			// Setting nat-addresses to router will send out GARPs for all externalIPs and LB VIPs
			// hosted on the GR. Setting exclude-lb-vips-from-garp to true will make sure GARPs for
			// LB VIPs are not sent, thereby preventing GARP overload.
			"exclude-lb-vips-from-garp": "true",
		},
		Addresses: []string{macAddress},
	}

	if gw.netInfo.IsSecondary() {
		externalLogicalSwitchPortToRouter.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}
	sw := nbdb.LogicalSwitch{Name: externalSwitch}
	if gw.netInfo.IsSecondary() {
		sw.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}

	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsAndSwitch(gw.nbClient, &sw, &externalLogicalSwitchPort, &externalLogicalSwitchPortToRouter)
	if err != nil {
		return fmt.Errorf("failed to create logical switch ports %+v, %+v, and switch %s: %v",
			externalLogicalSwitchPort, externalLogicalSwitchPortToRouter, externalSwitch, err)
	}

	return nil
}

// Cleanup removes all the NB DB objects created for a node's gateway
func (gw *GatewayManager) Cleanup() error {
	// Get the gateway router port's IP address (connected to join switch)
	var nextHops []net.IP

	gwIPAddrs, err := libovsdbutil.GetLRPAddrs(gw.nbClient, types.GWRouterToJoinSwitchPrefix+gw.gwRouterName)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return err
	}

	for _, gwIPAddr := range gwIPAddrs {
		nextHops = append(nextHops, gwIPAddr.IP)
	}
	gw.staticRouteCleanup(nextHops)
	gw.policyRouteCleanup(nextHops)

	// Remove the patch port that connects join switch to gateway router
	portName := types.JoinSwitchToGWRouterPrefix + gw.gwRouterName
	lsp := nbdb.LogicalSwitchPort{Name: portName}
	sw := nbdb.LogicalSwitch{Name: gw.transitSwitchName}
	err = libovsdbops.DeleteLogicalSwitchPorts(gw.nbClient, &sw, &lsp)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete logical switch port %s from switch %s: %w", portName, sw.Name, err)
	}

	// Remove the logical router port on the gateway router that connects to the join switch
	logicalRouter := nbdb.LogicalRouter{Name: gw.gwRouterName}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name: types.GWRouterToJoinSwitchPrefix + gw.gwRouterName,
	}
	err = libovsdbops.DeleteLogicalRouterPorts(gw.nbClient, &logicalRouter, &logicalRouterPort)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete port %s on router %s: %w", logicalRouterPort.Name, gw.gwRouterName, err)
	}

	// Remove the static mac bindings of the gateway router
	err = gateway.DeleteDummyGWMacBindings(gw.nbClient, gw.gwRouterName)
	if err != nil {
		return fmt.Errorf("failed to delete GR dummy mac bindings for node %s: %w", gw.nodeName, err)
	}

	// Remove the gateway router associated with nodeName
	err = libovsdbops.DeleteLogicalRouter(gw.nbClient, &logicalRouter)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete gateway router %s: %w", gw.gwRouterName, err)
	}

	// Remove external switch
	err = libovsdbops.DeleteLogicalSwitch(gw.nbClient, gw.extSwitchName)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete external switch %s: %w", gw.extSwitchName, err)
	}

	exGWexternalSwitch := types.EgressGWSwitchPrefix + gw.extSwitchName
	err = libovsdbops.DeleteLogicalSwitch(gw.nbClient, exGWexternalSwitch)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete external switch %s: %w", exGWexternalSwitch, err)
	}

	// This will cleanup the NodeSubnetPolicy in local and shared gateway modes. It will be a no-op for any other mode.
	gw.delPbrAndNatRules(gw.nodeName, nil)
	return nil
}

func (gw *GatewayManager) delPbrAndNatRules(nodeName string, lrpTypes []string) {
	// delete the dnat_and_snat entry that we added for the management port IP
	// Note: we don't need to delete any MAC bindings that are dynamically learned from OVN SB DB
	// because there will be none since this NAT is only for outbound traffic and not for inbound
	mgmtPortName := util.GetK8sMgmtIntfName(gw.netInfo.GetNetworkScopedName(nodeName))
	nat := libovsdbops.BuildDNATAndSNAT(nil, nil, mgmtPortName, "", nil)
	logicalRouter := nbdb.LogicalRouter{
		Name: gw.clusterRouterName,
	}
	err := libovsdbops.DeleteNATs(gw.nbClient, &logicalRouter, nat)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Errorf("Failed to delete the dnat_and_snat associated with the management port %s: %v", mgmtPortName, err)
	}

	// delete all logical router policies on ovn_cluster_router
	gw.removeLRPolicies(nodeName, lrpTypes)
}

func (gw *GatewayManager) staticRouteCleanup(nextHops []net.IP) {
	ips := sets.Set[string]{}
	for _, nextHop := range nextHops {
		ips.Insert(nextHop.String())
	}
	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		networkName, isSecondaryNetwork := item.ExternalIDs[types.NetworkExternalID]
		if !isSecondaryNetwork {
			networkName = types.DefaultNetworkName
		}
		if networkName != gw.netInfo.GetNetworkName() {
			return false
		}
		return ips.Has(item.Nexthop)
	}
	err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(gw.nbClient, gw.clusterRouterName, p)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Errorf("Failed to delete static route for nexthops %+v: %v", ips.UnsortedList(), err)
	}
}

// policyRouteCleanup cleansup all policies on cluster router that have a nextHop
// in the provided list.
// - if the LRP exists and has the len(nexthops) > 1: it removes
// the specified gatewayRouterIP from nexthops
// - if the LRP exists and has the len(nexthops) == 1: it removes
// the LRP completely
func (gw *GatewayManager) policyRouteCleanup(nextHops []net.IP) {
	for _, nextHop := range nextHops {
		gwIP := nextHop.String()
		policyPred := func(item *nbdb.LogicalRouterPolicy) bool {
			networkName, isSecondaryNetwork := item.ExternalIDs[types.NetworkExternalID]
			if !isSecondaryNetwork {
				networkName = types.DefaultNetworkName
			}
			if networkName != gw.netInfo.GetNetworkName() {
				return false
			}
			for _, nexthop := range item.Nexthops {
				if nexthop == gwIP {
					return true
				}
			}
			return false
		}
		err := libovsdbops.DeleteNextHopFromLogicalRouterPoliciesWithPredicate(gw.nbClient, gw.clusterRouterName, policyPred, gwIP)
		if err != nil && err != libovsdbclient.ErrNotFound {
			klog.Errorf("Failed to delete policy route from router %q for nexthop %+v: %v", gw.clusterRouterName, nextHop, err)
		}
	}
}

// remove Logical Router Policy on ovn_cluster_router for a specific node.
// Specify priorities to only delete specific types
func (gw *GatewayManager) removeLRPolicies(nodeName string, priorities []string) {
	if len(priorities) == 0 {
		priorities = []string{types.NodeSubnetPolicyPriority}
	}

	intPriorities := sets.Set[int]{}
	for _, priority := range priorities {
		intPriority, _ := strconv.Atoi(priority)
		intPriorities.Insert(intPriority)
	}

	managedNetworkName := gw.netInfo.GetNetworkName()
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		networkName, isSecondaryNetwork := item.ExternalIDs[types.NetworkExternalID]
		if !isSecondaryNetwork {
			networkName = types.DefaultNetworkName
		}
		if networkName != managedNetworkName {
			return false
		}
		return strings.Contains(item.Match, fmt.Sprintf("%s ", nodeName)) && intPriorities.Has(item.Priority)
	}
	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(gw.nbClient, gw.clusterRouterName, p)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Errorf("Error deleting policies for network %q with priorities %v associated with the node %s: %v", gw.netInfo.GetNetworkName(), priorities, nodeName, err)
	}
}

func (gw *GatewayManager) syncGatewayLogicalNetwork(
	node *kapi.Node,
	l3GatewayConfig *util.L3GatewayConfig,
	hostSubnets []*net.IPNet,
	hostAddrs []string,
	clusterSubnets []*net.IPNet,
	gwLRPIPs []*net.IPNet,
	isSCTPSupported bool,
	ovnClusterLRPToJoinIfAddrs []*net.IPNet,
	externalIPs []net.IP,
) error {
	var err error

	enableGatewayMTU := util.ParseNodeGatewayMTUSupport(node)

	err = gw.GatewayInit(
		node.Name,
		clusterSubnets,
		hostSubnets,
		l3GatewayConfig,
		isSCTPSupported,
		gwLRPIPs,
		ovnClusterLRPToJoinIfAddrs,
		externalIPs,
		enableGatewayMTU,
	)
	if err != nil {
		return fmt.Errorf("failed to init shared interface gateway: %v", err)
	}

	for _, subnet := range hostSubnets {
		hostIfAddr := util.GetNodeManagementIfAddr(subnet)
		if hostIfAddr == nil {
			return fmt.Errorf("host interface address not found for subnet %q on network %q", subnet, gw.netInfo.GetNetworkName())
		}
		l3GatewayConfigIP, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6(hostIfAddr.IP), l3GatewayConfig.IPAddresses)
		if err != nil {
			return err
		}
		relevantHostIPs, err := util.MatchAllIPStringFamily(utilnet.IsIPv6(hostIfAddr.IP), hostAddrs)
		if err != nil && err != util.ErrorNoIP {
			return err
		}
		pbrMngr := gatewayrouter.NewPolicyBasedRoutesManager(gw.nbClient, gw.clusterRouterName, gw.netInfo)
		if err := pbrMngr.Add(node.Name, hostIfAddr.IP.String(), l3GatewayConfigIP, relevantHostIPs); err != nil {
			return err
		}
	}

	return err
}

// syncNodeGateway ensures a node's gateway router is configured according to the L3 config and host subnets
func (gw *GatewayManager) syncNodeGateway(
	node *kapi.Node,
	l3GatewayConfig *util.L3GatewayConfig,
	hostSubnets []*net.IPNet,
	hostAddrs []string,
	clusterSubnets, gwLRPIPs []*net.IPNet,
	isSCTPSupported bool,
	joinSwitchIPs []*net.IPNet,
	externalIPs []net.IP,
) error {
	if l3GatewayConfig.Mode == config.GatewayModeDisabled {
		if err := gw.Cleanup(); err != nil {
			return fmt.Errorf("error cleaning up gateway for node %s: %v", node.Name, err)
		}
	} else if hostSubnets != nil {
		if err := gw.syncGatewayLogicalNetwork(
			node,
			l3GatewayConfig,
			hostSubnets,
			hostAddrs,
			clusterSubnets,
			gwLRPIPs,
			isSCTPSupported,
			joinSwitchIPs,
			externalIPs,
		); err != nil {
			return fmt.Errorf("error creating gateway for node %s: %v", node.Name, err)
		}
	}
	return nil
}

func physNetName(netInfo util.NetInfo) string {
	physnetName := types.PhysicalNetworkName
	if netInfo.IsSecondary() {
		physnetName = netInfo.GetNetworkName()
	}
	return physnetName
}

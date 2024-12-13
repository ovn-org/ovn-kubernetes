package ovn

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"golang.org/x/exp/maps"
	kapi "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
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
	joinSwitchName    string
	coppUUID          string
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

func NewGatewayManagerForLayer2Topology(
	nodeName string,
	coopUUID string,
	kube kube.InterfaceOVN,
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	watchFactory *factory.WatchFactory,
	opts ...GatewayOption,
) *GatewayManager {
	return newGWManager(
		nodeName,
		"",
		netInfo.GetNetworkScopedGWRouterName(nodeName),
		netInfo.GetNetworkScopedExtSwitchName(nodeName),
		netInfo.GetNetworkScopedName(types.OVNLayer2Switch),
		coopUUID,
		kube,
		nbClient,
		netInfo,
		watchFactory,
		opts...,
	)
}

func NewGatewayManager(
	nodeName string,
	coopUUID string,
	kube kube.InterfaceOVN,
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	watchFactory *factory.WatchFactory,
	opts ...GatewayOption,
) *GatewayManager {
	return newGWManager(
		nodeName,
		netInfo.GetNetworkScopedClusterRouterName(),
		netInfo.GetNetworkScopedGWRouterName(nodeName),
		netInfo.GetNetworkScopedExtSwitchName(nodeName),
		netInfo.GetNetworkScopedJoinSwitchName(),
		coopUUID,
		kube,
		nbClient,
		netInfo,
		watchFactory,
		opts...,
	)
}

func newGWManager(
	nodeName, clusterRouterName, gwRouterName, extSwitchName, joinSwitchName string,
	coopUUID string,
	kube kube.InterfaceOVN,
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	watchFactory *factory.WatchFactory,
	opts ...GatewayOption) *GatewayManager {
	gwManager := &GatewayManager{
		nodeName:          nodeName,
		clusterRouterName: clusterRouterName,
		gwRouterName:      gwRouterName,
		extSwitchName:     extSwitchName,
		joinSwitchName:    joinSwitchName,
		coppUUID:          coopUUID,
		kube:              kube,
		nbClient:          nbClient,
		netInfo:           netInfo,
		watchFactory:      watchFactory,
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

// cleanupStalePodSNATs removes pod SNATs against nodeIP for the given node if
// the SNAT.logicalIP isn't an active podIP, the pod network is being advertised
// on this node or disableSNATMultipleGWs=false. We don't have to worry about
// missing SNATs that should be added because addLogicalPort takes care of this
// for all pods when RequestRetryObjs is called for each node add.
// Other non-pod SNATs like join subnet SNATs are ignored.
// NOTE: On startup libovsdb adds back all the pods and this should normally
// update all existing SNATs accordingly. Due to a stale egressIP cache bug
// https://issues.redhat.com/browse/OCPBUGS-1520 we ended up adding wrong
// pod->nodeSNATs which won't get cleared up unless explicitly deleted.
// NOTE2: egressIP SNATs are synced in EIP controller.
func (gw *GatewayManager) cleanupStalePodSNATs(nodeName string, nodeIPs []*net.IPNet, gwLRPIPs []net.IP) error {
	// collect all the pod IPs for which we should be doing the SNAT; if the pod
	// network is advertised or DisableSNATMultipleGWs==false we consider all
	// the SNATs stale
	podIPsWithSNAT := sets.New[string]()
	if !gw.isRoutingAdvertised(nodeName) && config.Gateway.DisableSNATMultipleGWs {
		pods, err := gw.kube.GetPods(metav1.NamespaceAll, metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
		})
		if err != nil {
			return fmt.Errorf("unable to list existing pods on node: %s, %w",
				nodeName, err)
		}
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
				podIPsWithSNAT.Insert(podIP.String())
			}
		}
	}

	gatewayRouter := &nbdb.LogicalRouter{
		Name: gw.gwRouterName,
	}
	routerNATs, err := libovsdbops.GetRouterNATs(gw.nbClient, gatewayRouter)
	if err != nil && errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("unable to get NAT entries for router %s on node %s: %w", gatewayRouter.Name, nodeName, err)
	}

	nodeIPset := sets.New(util.IPNetsIPToStringSlice(nodeIPs)...)
	gwLRPIPset := sets.New(util.StringSlice(gwLRPIPs)...)
	natsToDelete := []*nbdb.NAT{}
	for _, routerNat := range routerNATs {
		routerNat := routerNat
		if routerNat.Type != nbdb.NATTypeSNAT {
			continue
		}
		if !nodeIPset.Has(routerNat.ExternalIP) {
			continue
		}
		if podIPsWithSNAT.Has(routerNat.LogicalIP) {
			continue
		}
		if gwLRPIPset.Has(routerNat.LogicalIP) {
			continue
		}
		logicalIP := net.ParseIP(routerNat.LogicalIP)
		if logicalIP == nil {
			// this is probably a CIDR so not a pod IP
			continue
		}
		natsToDelete = append(natsToDelete, routerNat)
	}

	if len(natsToDelete) > 0 {
		err := libovsdbops.DeleteNATs(gw.nbClient, gatewayRouter, natsToDelete...)
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
	gwLRPJoinIPs, drLRPIfAddrs []*net.IPNet,
	externalIPs []net.IP,
	enableGatewayMTU bool,
) error {

	gwLRPIPs := make([]net.IP, 0)
	for _, gwLRPJoinIP := range gwLRPJoinIPs {
		gwLRPIPs = append(gwLRPIPs, gwLRPJoinIP.IP)
	}
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		// At layer2 GR LRP acts as the layer3 ovn_cluster_router so we need
		// to configure here the .1 address, this will work only for IC with
		// one node per zone, since ARPs for .1 will not go beyond local switch.
		// This is being done to add the ICMP SNATs for .1 podSubnet that OVN GR generates
		for _, subnet := range hostSubnets {
			gwLRPIPs = append(gwLRPIPs, util.GetNodeGatewayIfAddr(subnet).IP)
		}
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
		"mac_binding_age_threshold":     types.GRMACBindingAgeThreshold,
	}
	// set the snat-ct-zone only for the default network
	// for UDN's OVN will pick a random one
	if gw.netInfo.GetNetworkName() == types.DefaultNetworkName {
		logicalRouterOptions["snat-ct-zone"] = "0"
	}
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		// When multiple networks are set of the same logical-router-port
		// the networks get lexicographically sorted; thus there is no
		// ordering or telling on which IP will be chosen as the router-ip
		// when it comes to SNATing traffic after load balancing.
		// Hence for Layer2 UDPNs let's set the snat-ip explicitly to the
		// joinsubnetIP
		joinIPDualStack := make([]string, len(gwLRPJoinIPs))
		for i, gwLRPJoinIP := range gwLRPJoinIPs {
			joinIPDualStack[i] = gwLRPJoinIP.IP.String()
		}
		logicalRouterOptions["lb_force_snat_ip"] = strings.Join(joinIPDualStack, " ")
	}
	logicalRouterExternalIDs := map[string]string{
		"physical_ip":  physicalIPs[0],
		"physical_ips": strings.Join(physicalIPs, ","),
	}

	if gw.netInfo.IsSecondary() {
		maps.Copy(logicalRouterExternalIDs, util.GenerateExternalIDsForSwitchOrRouter(gw.netInfo))
	}

	logicalRouter := nbdb.LogicalRouter{
		Name:        gatewayRouter,
		Options:     logicalRouterOptions,
		ExternalIDs: logicalRouterExternalIDs,
		Copp:        &gw.coppUUID,
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

	// In Layer2 networks there is no join switch and the gw.joinSwitchName points to the cluster switch.
	// Ensure that the ports are named appropriately, this is important for the logical router policies
	// created for local node access.
	// TODO(kyrtapz): Clean this up for clarity as part of https://github.com/ovn-org/ovn-kubernetes/issues/4689
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		gwSwitchPort = types.SwitchToRouterPrefix + gw.joinSwitchName
		gwRouterPort = types.RouterToSwitchPrefix + gw.joinSwitchName
	}

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
		if gw.netInfo.TopologyType() == types.Layer2Topology {
			node, err := gw.watchFactory.GetNode(nodeName)
			if err != nil {
				return fmt.Errorf("failed to fetch node %s from watch factory %w", node, err)
			}
			tunnelID, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(node, gw.netInfo.GetNetworkName())
			if err != nil {
				if util.IsAnnotationNotSetError(err) {
					// remote node may not have the annotation yet, suppress it
					return types.NewSuppressedError(err)
				}
				// Don't consider this node as cluster-manager has not allocated node id yet.
				return fmt.Errorf("failed to fetch tunnelID annotation from the node %s for network %s, err: %w",
					nodeName, gw.netInfo.GetNetworkName(), err)
			}
			logicalSwitchPort.Options["requested-tnl-key"] = strconv.Itoa(tunnelID)
		}
	}
	sw := nbdb.LogicalSwitch{Name: gw.joinSwitchName}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(gw.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return fmt.Errorf("failed to create port %v on logical switch %q: %v", gwSwitchPort, sw.Name, err)
	}

	gwLRPMAC := util.IPAddrToHWAddr(gwLRPIPs[0])
	gwLRPNetworks := []string{}
	for _, gwLRPJoinIP := range gwLRPJoinIPs {
		gwLRPNetworks = append(gwLRPNetworks, gwLRPJoinIP.String())
	}
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		// At layer2 GR LRP acts as the layer3 ovn_cluster_router so we need
		// to configure here the .1 address, this will work only for IC with
		// one node per zone, since ARPs for .1 will not go beyond local switch.
		for _, subnet := range hostSubnets {
			gwLRPNetworks = append(gwLRPNetworks, util.GetNodeGatewayIfAddr(subnet).String())
		}
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
		_, isNetIPv6 := gw.netInfo.IPMode()
		if gw.netInfo.TopologyType() == types.Layer2Topology && isNetIPv6 && config.IPv6Mode {
			logicalRouterPort.Ipv6RaConfigs = map[string]string{
				"address_mode":      "dhcpv6_stateful",
				"send_periodic":     "true",
				"max_interval":      "900", // 15 minutes
				"min_interval":      "300", // 5 minutes
				"router_preference": "LOW", // The static gateway configured by CNI is MEDIUM, so make this SLOW so it has less effect for pods
			}
			if gw.netInfo.MTU() > 0 {
				logicalRouterPort.Ipv6RaConfigs["mtu"] = fmt.Sprintf("%d", gw.netInfo.MTU())
			}
		}
	}

	err = libovsdbops.CreateOrUpdateLogicalRouterPort(gw.nbClient, &logicalRouter,
		&logicalRouterPort, nil, &logicalRouterPort.MAC, &logicalRouterPort.Networks,
		&logicalRouterPort.Options)
	if err != nil {
		return fmt.Errorf("failed to create port %+v on router %+v: %v", logicalRouterPort, logicalRouter, err)
	}
	if len(drLRPIfAddrs) > 0 {
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

	// Remove stale OVN resources with any old masquerade IP
	if err := deleteStaleMasqueradeResources(gw.nbClient, gatewayRouter, nodeName, gw.watchFactory); err != nil {
		return fmt.Errorf("failed to remove stale masquerade resources from northbound database: %w", err)
	}

	if err := gateway.CreateDummyGWMacBindings(gw.nbClient, gatewayRouter, gw.netInfo); err != nil {
		return err
	}

	for _, nextHop := range node.DummyNextHopIPs() {
		// Add return service route for OVN back to host
		prefix := config.Gateway.V4MasqueradeSubnet
		if utilnet.IsIPv6(nextHop) {
			prefix = config.Gateway.V6MasqueradeSubnet
		}
		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix:    prefix,
			Nexthop:     nextHop.String(),
			OutputPort:  &externalRouterPort,
			ExternalIDs: map[string]string{util.OvnNodeMasqCIDR: ""},
		}
		if gw.netInfo.IsSecondary() {
			lrsr.ExternalIDs = map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			}
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.OutputPort != nil && *item.OutputPort == *lrsr.OutputPort && item.IPPrefix == lrsr.IPPrefix &&
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
			gw.staticRouteCleanup([]net.IP{mgmtIfAddr.IP}, hostSubnet)

			if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(
				gw.nbClient,
				gw.clusterRouterName,
				&lrsr,
				p,
				&lrsr.Nexthop,
			); err != nil {
				return fmt.Errorf("error creating static route %+v in GR %s: %v", lrsr, gw.clusterRouterName, err)
			}
		} else if config.Gateway.Mode == config.GatewayModeLocal {
			// If migrating from shared to local gateway, let's remove the static routes towards
			// join switch for the hostSubnet prefix and any potential routes for UDN enabled services.
			// Note syncManagementPort happens before gateway sync so only remove things pointing to join subnet
			if gw.clusterRouterName != "" {
				p := func(item *nbdb.LogicalRouterStaticRoute) bool {
					if _, ok := item.ExternalIDs[types.UDNEnabledServiceExternalID]; ok {
						return true
					}
					return item.IPPrefix == lrsr.IPPrefix && item.Policy != nil && *item.Policy == *lrsr.Policy &&
						gw.containsJoinIP(net.ParseIP(item.Nexthop))
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
		if gw.containsJoinIP(parsedLogicalIP) {
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
	if !config.Gateway.DisableSNATMultipleGWs && !gw.isRoutingAdvertised(nodeName) {
		// Default SNAT rules. DisableSNATMultipleGWs=false in LGW (traffic egresses via mp0) always.
		// We are not checking for gateway mode to be shared explicitly to reduce topology differences.
		for _, entry := range clusterIPSubnet {
			externalIP, err := util.MatchIPFamily(utilnet.IsIPv6CIDR(entry), externalIPs)
			if err != nil {
				return fmt.Errorf("failed to create default SNAT rules for gateway router %s: %v",
					gatewayRouter, err)
			}

			nat = libovsdbops.BuildSNATWithMatch(&externalIP[0], entry, "", extIDs, gw.netInfo.GetNetworkScopedClusterSubnetSNATMatch(nodeName))
			nats = append(nats, nat)
		}
		err := libovsdbops.CreateOrUpdateNATs(gw.nbClient, &logicalRouter, nats...)
		if err != nil {
			return fmt.Errorf("failed to update SNAT rule for pod on router %s error: %v", gatewayRouter, err)
		}
	} else {
		// ensure we do not have any leftover SNAT entries after an upgrade
		for _, logicalSubnet := range clusterIPSubnet {
			nat = libovsdbops.BuildSNATWithMatch(nil, logicalSubnet, "", extIDs, gw.netInfo.GetNetworkScopedClusterSubnetSNATMatch(nodeName))
			nats = append(nats, nat)
		}
		err := libovsdbops.DeleteNATs(gw.nbClient, &logicalRouter, nats...)
		if err != nil {
			return fmt.Errorf("failed to delete GW SNAT rule for pod on router %s error: %v", gatewayRouter, err)
		}
	}

	if err := gw.cleanupStalePodSNATs(nodeName, l3GatewayConfig.IPAddresses, gwLRPIPs); err != nil {
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
		sw.ExternalIDs = util.GenerateExternalIDsForSwitchOrRouter(gw.netInfo)
	}

	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsAndSwitch(gw.nbClient, &sw, &externalLogicalSwitchPort, &externalLogicalSwitchPortToRouter)
	if err != nil {
		return fmt.Errorf("failed to create logical switch ports %+v, %+v, and switch %s: %v",
			externalLogicalSwitchPort, externalLogicalSwitchPortToRouter, externalSwitch, err)
	}

	return nil
}

// cleanupStaleMasqueradeData removes following from northbound database
//   - LogicalRouterStaticRoute for rtoe-<GW_router> OutputPort anf IPPrefix is same as v4 or v6
//     StaleMasqueradeSubnet
//   - StaticMACBinding for rtoe-<GW_router> LogicalPort and referencing old DummyNextHopMasqueradeIP
func deleteStaleMasqueradeResources(nbClient libovsdbclient.Client, routerName, nodeName string, wf *factory.WatchFactory) error {
	var staleMasqueradeIPs config.MasqueradeIPsConfig
	var nextHops []net.IP
	logicalport := types.GWRouterToExtSwitchPrefix + routerName

	// we first examine the kapi node to see if we can determine if there is a stale masquerade subnet
	// if the masquerade subnet on the kapi node matches whats configured for this controller, it
	// doesn't necessarily mean there is not still a stale masquerade config in nbdb because
	// node could have already updated before this process runs.
	// As a backup, if we don't find the positive match in kapi, we execute a negative lookup in NBDB

	node, err := wf.GetNode(nodeName)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// node doesn't exist for some reason, assume we should still try to clean up with auto-detection
			if err := deleteStaleMasqueradeRouteAndMACBinding(nbClient, routerName, nextHops); err != nil {
				return fmt.Errorf("failed to remove stale MAC binding and static route for logical port %s: %w", logicalport, err)
			}
			return nil
		}
		return err
	}

	subnets, err := util.ParseNodeMasqueradeSubnet(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// no annotation set, must be initial bring up, nothing to clean
			return nil
		}
		return err
	}

	var v4ConfiguredMasqueradeNet, v6ConfiguredMasqueradeNet *net.IPNet

	for _, subnet := range subnets {
		if utilnet.IsIPv6CIDR(subnet) {
			v6ConfiguredMasqueradeNet = subnet
		} else if utilnet.IsIPv4CIDR(subnet) {
			v4ConfiguredMasqueradeNet = subnet
		} else {
			return fmt.Errorf("invalid subnet for masquerade annotation: %s", subnet)
		}
	}

	// Check for KAPI telling us there is a stale masquerade
	if v4ConfiguredMasqueradeNet != nil && config.Gateway.V4MasqueradeSubnet != v4ConfiguredMasqueradeNet.String() {
		if err := config.AllocateV4MasqueradeIPs(v4ConfiguredMasqueradeNet.IP, &staleMasqueradeIPs); err != nil {
			return fmt.Errorf("unable to determine stale V4MasqueradeIPs: %s", err)
		}
		nextHops = append(nextHops, staleMasqueradeIPs.V4DummyNextHopMasqueradeIP)
	}

	if v6ConfiguredMasqueradeNet != nil && config.Gateway.V6MasqueradeSubnet != v6ConfiguredMasqueradeNet.String() {
		if err := config.AllocateV6MasqueradeIPs(v6ConfiguredMasqueradeNet.IP, &staleMasqueradeIPs); err != nil {
			return fmt.Errorf("unable to determine stale V6MasqueradeIPs: %s", err)
		}
		nextHops = append(nextHops, staleMasqueradeIPs.V6DummyNextHopMasqueradeIP)
	}

	if err := deleteStaleMasqueradeRouteAndMACBinding(nbClient, routerName, nextHops); err != nil {
		return fmt.Errorf("failed to remove stale MAC binding and static route for logical port %s: %w", logicalport, err)
	}

	return nil
}

// deleteStaleMasqueradeRouteAndMACBinding will attempt to remove the corresponding routes and MAC bindings given the
// list of nextHopIPs. If nextHopIPs is empty, then an attempt will be made to detect the stale route and MAC bindings
func deleteStaleMasqueradeRouteAndMACBinding(nbClient libovsdbclient.Client, routerName string, nextHopIPs []net.IP) error {
	logicalport := types.GWRouterToExtSwitchPrefix + routerName
	if len(nextHopIPs) == 0 {
		// build valid values
		validNextHops := []net.IP{config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP, config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP}
		// lookup routes for external id that dont match currently configured masquerade subnets
		for _, validNextHop := range validNextHops {
			staticRoutePredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
				if item.OutputPort != nil && *item.OutputPort == logicalport &&
					item.Nexthop != validNextHop.String() && utilnet.IPFamilyOfString(item.Nexthop) == utilnet.IPFamilyOf(validNextHop) {
					if _, ok := item.ExternalIDs[util.OvnNodeMasqCIDR]; ok {
						return true
					}
				}
				return false
			}

			staleRoutes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(nbClient, staticRoutePredicate)
			if err != nil {
				return fmt.Errorf("failed to search for stale masquerade routes: %w", err)
			}

			for _, staleRoute := range staleRoutes {
				klog.Infof("Stale masquerade route found: %#v", *staleRoute)
				// found stale routes, derive nexthop and flush the route and mac binding if it exists
				staleNextHop := staleRoute.Nexthop

				macBindingPredicate := func(item *nbdb.StaticMACBinding) bool {
					return item.LogicalPort == logicalport && item.IP == staleNextHop &&
						utilnet.IPFamilyOfString(item.IP) == utilnet.IPFamilyOfString(staleNextHop)
				}
				if err := libovsdbops.DeleteStaticMACBindingWithPredicate(nbClient, macBindingPredicate); err != nil {
					return fmt.Errorf("failed to delete static MAC binding for logical port %s: %v", logicalport, err)
				}
			}
			if err := libovsdbops.DeleteLogicalRouterStaticRoutes(nbClient, routerName, staleRoutes...); err != nil {
				return err
			}
		}
		return nil
	}

	for _, nextHop := range nextHopIPs {
		staticRoutePredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
			if item.OutputPort != nil && *item.OutputPort == logicalport &&
				item.Nexthop == nextHop.String() && utilnet.IPFamilyOfString(item.Nexthop) == utilnet.IPFamilyOf(nextHop) {
				if _, ok := item.ExternalIDs[util.OvnNodeMasqCIDR]; ok {
					return true
				}
			}
			return false
		}
		if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(nbClient, routerName, staticRoutePredicate); err != nil {
			return fmt.Errorf("failed to delete static route from gateway router %s: %v", routerName, err)
		}

		macBindingPredicate := func(item *nbdb.StaticMACBinding) bool {
			return item.LogicalPort == logicalport && item.IP == nextHop.String() &&
				utilnet.IPFamilyOfString(item.IP) == utilnet.IPFamilyOf(nextHop)
		}
		if err := libovsdbops.DeleteStaticMACBindingWithPredicate(nbClient, macBindingPredicate); err != nil {
			return fmt.Errorf("failed to delete static MAC binding for logical port %s: %v", logicalport, err)
		}
	}
	return nil
}

// Cleanup removes all the NB DB objects created for a node's gateway
func (gw *GatewayManager) Cleanup() error {
	// Get the gateway router port's IP address (connected to join switch)
	var nextHops []net.IP

	gwRouterToJoinSwitchPortName := types.GWRouterToJoinSwitchPrefix + gw.gwRouterName
	portName := types.JoinSwitchToGWRouterPrefix + gw.gwRouterName

	// In Layer2 networks there is no join switch and the gw.joinSwitchName points to the cluster switch.
	// Ensure that the ports are named appropriately, this is important for the logical router policies
	// created for local node access.
	// TODO(kyrtapz): Clean this up for clarity as part of https://github.com/ovn-org/ovn-kubernetes/issues/4689
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		gwRouterToJoinSwitchPortName = types.RouterToSwitchPrefix + gw.joinSwitchName
		portName = types.SwitchToRouterPrefix + gw.joinSwitchName
	}

	gwIPAddrs, err := libovsdbutil.GetLRPAddrs(gw.nbClient, gwRouterToJoinSwitchPortName)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf(
			"failed to get gateway IPs for network %q from LRP %s: %v",
			gw.netInfo.GetNetworkName(),
			gwRouterToJoinSwitchPortName,
			err,
		)
	}

	for _, gwIPAddr := range gwIPAddrs {
		nextHops = append(nextHops, gwIPAddr.IP)
	}
	gw.staticRouteCleanup(nextHops, nil)
	gw.policyRouteCleanup(nextHops)

	// Remove the patch port that connects join switch to gateway router
	lsp := nbdb.LogicalSwitchPort{Name: portName}
	sw := nbdb.LogicalSwitch{Name: gw.joinSwitchName}
	err = libovsdbops.DeleteLogicalSwitchPorts(gw.nbClient, &sw, &lsp)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete logical switch port %s from switch %s: %w", portName, sw.Name, err)
	}

	// Remove the logical router port on the gateway router that connects to the join switch
	logicalRouter := nbdb.LogicalRouter{Name: gw.gwRouterName}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name: gwRouterToJoinSwitchPortName,
	}
	err = libovsdbops.DeleteLogicalRouterPorts(gw.nbClient, &logicalRouter, &logicalRouterPort)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete port %s on router %s: %w", logicalRouterPort.Name, gw.gwRouterName, err)
	}

	// Remove the static mac bindings of the gateway router
	err = gateway.DeleteDummyGWMacBindings(gw.nbClient, gw.gwRouterName, gw.netInfo)
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
	gw.delPbrAndNatRules(gw.nodeName)
	return nil
}

func (gw *GatewayManager) delPbrAndNatRules(nodeName string) {
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
	gw.removeLRPolicies(nodeName)
}

func (gw *GatewayManager) staticRouteCleanup(nextHops []net.IP, ipPrefix *net.IPNet) {
	if len(nextHops) == 0 {
		return // if we do not have next hops, we do not have any routes to cleanup
	}
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
		if ipPrefix != nil && item.IPPrefix != ipPrefix.String() {
			return false
		}
		return ips.Has(item.Nexthop)
	}
	err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(gw.nbClient, gw.clusterRouterName, p)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		klog.Errorf("Failed to delete static route for nexthops %+v: %v", ips.UnsortedList(), err)
	}
}

// policyRouteCleanup cleans up all policies on cluster router that have a nextHop
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
func (gw *GatewayManager) removeLRPolicies(nodeName string) {
	priorities := []string{types.NodeSubnetPolicyPriority}

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

func (gw *GatewayManager) containsJoinIP(ip net.IP) bool {
	ipNet := &net.IPNet{
		IP:   ip,
		Mask: util.GetIPFullMask(ip),
	}
	return util.IsContainedInAnyCIDR(ipNet, gw.netInfo.JoinSubnets()...)
}

func (gw *GatewayManager) isRoutingAdvertised(node string) bool {
	if gw.netInfo.IsSecondary() {
		return false
	}
	return util.IsPodNetworkAdvertisedAtNode(gw.netInfo, node)
}

func (gw *GatewayManager) syncGatewayLogicalNetwork(
	node *kapi.Node,
	l3GatewayConfig *util.L3GatewayConfig,
	hostSubnets []*net.IPNet,
	hostAddrs []string,
	clusterSubnets []*net.IPNet,
	grLRPJoinIPs []*net.IPNet,
	isSCTPSupported bool,
	ovnClusterLRPToJoinIfAddrs []*net.IPNet,
	externalIPs []net.IP,
) error {
	enableGatewayMTU := util.ParseNodeGatewayMTUSupport(node)

	err := gw.GatewayInit(
		node.Name,
		clusterSubnets,
		hostSubnets,
		l3GatewayConfig,
		isSCTPSupported,
		grLRPJoinIPs, // the joinIP allocated to this node's GR for this controller's network
		ovnClusterLRPToJoinIfAddrs,
		externalIPs,
		enableGatewayMTU,
	)
	if err != nil {
		return fmt.Errorf("failed to init gateway for network %q: %v", gw.netInfo.GetNetworkName(), err)
	}

	routerName := gw.clusterRouterName
	if gw.clusterRouterName == "" {
		routerName = gw.gwRouterName
	}
	for _, subnet := range hostSubnets {
		mgmtIfAddr := util.GetNodeManagementIfAddr(subnet)
		if mgmtIfAddr == nil {
			return fmt.Errorf("management interface address not found for subnet %q on network %q", subnet, gw.netInfo.GetNetworkName())
		}
		l3GatewayConfigIP, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6(mgmtIfAddr.IP), l3GatewayConfig.IPAddresses)
		if err != nil {
			return fmt.Errorf("failed to extract the gateway IP addr for network %q: %v", gw.netInfo.GetNetworkName(), err)
		}
		relevantHostIPs, err := util.MatchAllIPStringFamily(utilnet.IsIPv6(mgmtIfAddr.IP), hostAddrs)
		if err != nil && err != util.ErrorNoIP {
			return fmt.Errorf("failed to extract the host IP addrs for network %q: %v", gw.netInfo.GetNetworkName(), err)
		}
		pbrMngr := gatewayrouter.NewPolicyBasedRoutesManager(gw.nbClient, routerName, gw.netInfo)
		if err := pbrMngr.AddSameNodeIPPolicy(node.Name, mgmtIfAddr.IP.String(), l3GatewayConfigIP, relevantHostIPs); err != nil {
			return fmt.Errorf("failed to configure the policy based routes for network %q: %v", gw.netInfo.GetNetworkName(), err)
		}
		if gw.netInfo.TopologyType() == types.Layer2Topology && config.Gateway.Mode == config.GatewayModeLocal {
			if err := pbrMngr.AddHostCIDRPolicy(node, mgmtIfAddr.IP.String(), subnet.String()); err != nil {
				return fmt.Errorf("failed to configure the hostCIDR policy for L2 network %q on local gateway: %v",
					gw.netInfo.GetNetworkName(), err)
			}
		}
	}

	return nil
}

// syncNodeGateway ensures a node's gateway router is configured according to the L3 config and host subnets
func (gw *GatewayManager) syncNodeGateway(
	node *kapi.Node,
	l3GatewayConfig *util.L3GatewayConfig,
	hostSubnets []*net.IPNet,
	hostAddrs []string,
	clusterSubnets, grLRPJoinIPs []*net.IPNet,
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
			grLRPJoinIPs, // the joinIP allocated to this node for this controller's network
			isSCTPSupported,
			joinSwitchIPs, // the .1 of this controller's global joinSubnet
			externalIPs,
		); err != nil {
			return fmt.Errorf("error creating gateway for node %s: %v", node.Name, err)
		}
	}
	return nil
}

func physNetName(netInfo util.NetInfo) string {
	if netInfo.IsDefault() || netInfo.IsPrimaryNetwork() {
		return types.PhysicalNetworkName
	}
	return netInfo.GetNetworkName()
}

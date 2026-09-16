// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/gateway"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/gatewayrouter"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type GatewayManager struct {
	nodeName                string
	clusterRouterName       string
	gwRouterName            string
	extSwitchName           string
	joinSwitchName          string
	coppUUID                string
	kube                    kube.InterfaceOVN
	nbClient                libovsdbclient.Client
	netInfo                 util.NetInfo
	watchFactory            *factory.WatchFactory
	getNetworkNameForNADKey func(nadKey string) string
	nodeAnnotationCache     *nodecontroller.NodeAnnotationCache
	addressSetManager       *addresssetmanager.AddressSetManager
	// Cluster wide Load_Balancer_Group UUID.
	// Includes all node switches and node gateway routers.
	clusterLoadBalancerGroupUUID string

	// Cluster wide switch Load_Balancer_Group UUID.
	// Includes all node switches.
	switchLoadBalancerGroupUUID string

	// Cluster wide router Load_Balancer_Group UUID.
	// Includes all node gateway routers.
	routerLoadBalancerGroupUUID string

	transitRouterInfo *transitRouterInfo
}

type GatewayOption func(*GatewayManager)

func NewGatewayManagerForLayer2Topology(
	nodeName string,
	coopUUID string,
	kube kube.InterfaceOVN,
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	watchFactory *factory.WatchFactory,
	nodeAnnotationCache *nodecontroller.NodeAnnotationCache,
	addressSetManager *addresssetmanager.AddressSetManager,
	useTransitRouter bool,
	opts ...GatewayOption,
) *GatewayManager {
	routerName := ""
	if useTransitRouter {
		routerName = netInfo.GetNetworkScopedClusterRouterName()
	}
	return newGWManager(
		nodeName,
		routerName,
		netInfo.GetNetworkScopedExtSwitchName(nodeName),
		netInfo.GetNetworkScopedSwitchName(""),
		coopUUID,
		kube,
		nbClient,
		netInfo,
		watchFactory,
		nodeAnnotationCache,
		addressSetManager,
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
	nodeAnnotationCache *nodecontroller.NodeAnnotationCache,
	addressSetManager *addresssetmanager.AddressSetManager,
	opts ...GatewayOption,
) *GatewayManager {
	return newGWManager(
		nodeName,
		netInfo.GetNetworkScopedClusterRouterName(),
		netInfo.GetNetworkScopedExtSwitchName(nodeName),
		netInfo.GetNetworkScopedJoinSwitchName(),
		coopUUID,
		kube,
		nbClient,
		netInfo,
		watchFactory,
		nodeAnnotationCache,
		addressSetManager,
		opts...,
	)
}

func newGWManager(
	nodeName, clusterRouterName, extSwitchName, joinSwitchName string,
	coopUUID string,
	kube kube.InterfaceOVN,
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	watchFactory *factory.WatchFactory,
	nodeAnnotationCache *nodecontroller.NodeAnnotationCache,
	addressSetManager *addresssetmanager.AddressSetManager,
	opts ...GatewayOption) *GatewayManager {
	gwManager := &GatewayManager{
		nodeName:            nodeName,
		clusterRouterName:   clusterRouterName,
		gwRouterName:        netInfo.GetNetworkScopedGWRouterName(nodeName),
		extSwitchName:       extSwitchName,
		joinSwitchName:      joinSwitchName,
		coppUUID:            coopUUID,
		kube:                kube,
		nbClient:            nbClient,
		netInfo:             netInfo,
		watchFactory:        watchFactory,
		nodeAnnotationCache: nodeAnnotationCache,
		addressSetManager:   addressSetManager,
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

func WithNetworkNameForNADKeyResolver(getNetworkNameForNADKey func(nadKey string) string) GatewayOption {
	return func(manager *GatewayManager) {
		manager.getNetworkNameForNADKey = getNetworkNameForNADKey
	}
}

// cleanupStalePodSNATs removes pod SNATs against nodeIP for the given node if
// the SNAT.logicalIP isn't an active podIP, or disableSNATMultipleGWs=false.
// We don't have to worry about
// missing SNATs that should be added because addLogicalPort takes care of this
// for all pods when RequestRetryObjs is called for each node add.
// Other non-pod SNATs like join subnet SNATs are ignored.
// NOTE: On startup libovsdb adds back all the pods and this should normally
// update all existing SNATs accordingly. Due to a stale egressIP cache bug
// https://issues.redhat.com/browse/OCPBUGS-1520 we ended up adding wrong
// pod->nodeSNATs which won't get cleared up unless explicitly deleted.
// NOTE2: egressIP SNATs are synced in EIP controller.
func (gw *GatewayManager) cleanupStalePodSNATs(nodeName string, nodeIPs []*net.IPNet, gwLRPIPs []net.IP) error {
	if gw.netInfo.IsUserDefinedNetwork() && gw.getNetworkNameForNADKey == nil {
		return fmt.Errorf("missing NAD resolver for network %q", gw.netInfo.GetNetworkName())
	}

	gatewayRouter := &nbdb.LogicalRouter{
		Name: gw.gwRouterName,
	}
	routerNATs, err := libovsdbops.GetRouterNATs(gw.nbClient, gatewayRouter)
	if err != nil {
		return fmt.Errorf("unable to get NAT entries for router %s on node %s: %w", gatewayRouter.Name, nodeName, err)
	}
	if len(routerNATs) == 0 {
		// no nats, nothing to clean up
		return nil
	}

	// collect all the pod IPs for which we should be doing the SNAT;
	// if DisableSNATMultipleGWs==false we consider all
	// the SNATs stale
	podIPsWithSNAT := sets.New[string]()
	if config.Gateway.DisableSNATMultipleGWs {
		// For UDNs, only scan namespaces attached to this network.
		// For the default network NADNamespaces is empty; GetPods("")
		// falls back to listing all pods (ListAllByNamespace on "").
		nadNamespaces := gw.netInfo.GetNADNamespaces()
		if len(nadNamespaces) == 0 {
			nadNamespaces = []string{""}
		}
		for _, ns := range nadNamespaces {
			pods, err := gw.watchFactory.GetPods(ns)
			if err != nil {
				return fmt.Errorf("unable to list existing pods in namespace %q: %w", ns, err)
			}
			for _, pod := range pods {
				if !util.PodScheduled(pod) {
					continue
				}
				if pod.Spec.NodeName != nodeName {
					continue
				}
				if util.PodCompleted(pod) {
					collidingPod, err := findPodWithIPAddresses(gw.watchFactory, gw.netInfo, []net.IP{utilnet.ParseIPSloppy(pod.Status.PodIP)}, "", gw.getNetworkNameForNADKey, "")
					if err != nil {
						return fmt.Errorf("lookup for pods with same ip as %s %s failed: %w", pod.Namespace, pod.Name, err)
					}
					if collidingPod != nil {
						continue
					}
				}
				podIPs, err := util.GetPodIPsOfNetwork(pod, gw.netInfo, gw.getNetworkNameForNADKey)
				if err != nil && errors.Is(err, util.ErrNoPodIPFound) {
					klog.Warningf("Unable to fetch podIPs for pod %s/%s: %v", pod.Namespace, pod.Name, err)
					continue
				} else if err != nil {
					return fmt.Errorf("unable to fetch podIPs for pod %s/%s: %w", pod.Namespace, pod.Name, err)
				}
				for _, podIP := range podIPs {
					podIPsWithSNAT.Insert(podIP.String())
				}
			}
		}
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

func (gw *GatewayManager) createGWRouter(gwConfig *GatewayConfig) (*nbdb.LogicalRouter, error) {
	// Create a gateway router.
	logicalRouterOptions := map[string]string{
		"always_learn_from_arp_request": "false",
		"dynamic_neigh_routers":         "false",
		"chassis":                       gwConfig.annoConfig.ChassisID,
		"lb_force_snat_ip":              "router_ip",
		"mac_binding_age_threshold":     types.GRMACBindingAgeThreshold,
	}
	// set the snat-ct-zone only for the default network
	// for UDN's OVN will pick a random one
	if gw.netInfo.GetNetworkName() == types.DefaultNetworkName {
		logicalRouterOptions["snat-ct-zone"] = "0"
	}
	// Defensive: since nat-addresses=router isn't set on UDN external
	// switch ports, there is no extra benefit from setting this to true
	// today. However, the default (false) would allow GARP/RARP if
	// nat-addresses were ever added back; keeping it true ensures that
	// any future change enabling GARPs on UDN GRs must explicitly
	// justify which traffic flow requires it.
	if gw.netInfo.IsUserDefinedNetwork() {
		logicalRouterOptions["disable_garp_rarp"] = "true"
	}
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		// When multiple networks are set of the same logical-router-port
		// the networks get lexicographically sorted; thus there is no
		// ordering or telling on which IP will be chosen as the router-ip
		// when it comes to SNATing traffic after load balancing.
		// Hence for Layer2 UDPNs let's set the snat-ip explicitly to the
		// joinsubnetIP
		logicalRouterOptions["lb_force_snat_ip"] = strings.Join(util.IPNetsIPToStringSlice(gwConfig.gwRouterJoinCIDRs), " ")
	}
	physicalIPs := make([]string, len(gwConfig.annoConfig.IPAddresses))
	for i, ip := range gwConfig.annoConfig.IPAddresses {
		physicalIPs[i] = ip.IP.String()
	}
	logicalRouterExternalIDs := map[string]string{
		"physical_ip":  physicalIPs[0],
		"physical_ips": strings.Join(physicalIPs, ","),
	}

	if gw.netInfo.IsUserDefinedNetwork() {
		maps.Copy(logicalRouterExternalIDs, util.GenerateExternalIDsForSwitchOrRouter(gw.netInfo))
	}

	gwRouter := nbdb.LogicalRouter{
		Name:        gw.gwRouterName,
		Options:     logicalRouterOptions,
		ExternalIDs: logicalRouterExternalIDs,
		Copp:        &gw.coppUUID,
	}

	if gw.clusterLoadBalancerGroupUUID != "" {
		gwRouter.LoadBalancerGroup = []string{gw.clusterLoadBalancerGroupUUID}
		if gwConfig.annoConfig.NodePortEnable && gw.routerLoadBalancerGroupUUID != "" {
			// add routerLoadBalancerGroupUUID to the gateway router only if nodePort is enabled
			gwRouter.LoadBalancerGroup = append(gwRouter.LoadBalancerGroup, gw.routerLoadBalancerGroupUUID)
		}
	}

	err := libovsdbops.CreateOrUpdateLogicalRouter(gw.nbClient, &gwRouter, &gwRouter.Options,
		&gwRouter.ExternalIDs, &gwRouter.LoadBalancerGroup, &gwRouter.Copp)
	if err != nil {
		return nil, fmt.Errorf("failed to create logical router %+v: %v", gwRouter, err)
	}
	return &gwRouter, nil
}

func (gw *GatewayManager) getGWRouterPeerRouterPortName() string {
	return types.TransitRouterToRouterPrefix + gw.gwRouterName
}

func (gw *GatewayManager) getGWRouterPeerSwitchPortName() string {
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		return types.SwitchToRouterPrefix + gw.joinSwitchName
	}
	return types.JoinSwitchToGWRouterPrefix + gw.gwRouterName
}

func (gw *GatewayManager) getGWRouterPortName() string {
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		if gw.transitRouterInfo != nil {
			return types.RouterToTransitRouterPrefix + gw.gwRouterName
		}
		return types.RouterToSwitchPrefix + gw.joinSwitchName
	}
	return types.GWRouterToJoinSwitchPrefix + gw.gwRouterName
}

func (gw *GatewayManager) createGWRouterPeerSwitchPort(nodeName string) error {
	gwSwitchPort := gw.getGWRouterPeerSwitchPortName()
	gwRouterPortName := gw.getGWRouterPortName()

	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      gwSwitchPort,
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			libovsdbops.RouterPort: gwRouterPortName,
		},
	}
	if gw.netInfo.IsUserDefinedNetwork() {
		logicalSwitchPort.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}
	if gw.netInfo.TopologyType() == types.Layer2Topology {
		node, err := gw.watchFactory.GetNode(nodeName)
		if err != nil {
			return fmt.Errorf("failed to fetch node %s from watch factory %w", node.Name, err)
		}
		tunnelID, err := gw.nodeAnnotationCache.ParseUDNLayer2NodeGRLRPTunnelIDCached(node, gw.netInfo.GetNetworkName())
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				// remote node may not have the annotation yet, suppress it
				return types.NewSuppressedError(err)
			}
			// Don't consider this node as cluster-manager has not allocated node id yet.
			return fmt.Errorf("failed to fetch tunnelID annotation from the node %s for network %s, err: %w",
				nodeName, gw.netInfo.GetNetworkName(), err)
		}
		logicalSwitchPort.Options[libovsdbops.RequestedTnlKey] = strconv.Itoa(tunnelID)
	}
	sw := nbdb.LogicalSwitch{Name: gw.joinSwitchName}
	err := libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(gw.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		return fmt.Errorf("failed to create port %v on logical switch %q: %v", gwSwitchPort, sw.Name, err)
	}
	return err
}

func (gw *GatewayManager) deleteGWRouterPeerSwitchPort() error {
	// Remove the patch port that connects join switch to gateway router
	lsp := nbdb.LogicalSwitchPort{Name: gw.getGWRouterPeerSwitchPortName()}
	sw := nbdb.LogicalSwitch{Name: gw.joinSwitchName}
	err := libovsdbops.DeleteLogicalSwitchPorts(gw.nbClient, &sw, &lsp)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete logical switch port %s from switch %s: %w", lsp.Name, sw.Name, err)
	}
	return nil
}

func (gw *GatewayManager) createGWRouterPeerRouterPort() error {
	gwPeerPortName := gw.getGWRouterPeerRouterPortName()
	gwRouterPortName := gw.getGWRouterPortName()

	ovnClusterRouterToGWRouterPort := nbdb.LogicalRouterPort{
		Name:     gwPeerPortName,
		MAC:      util.IPAddrToHWAddr(gw.transitRouterInfo.transitRouterNets[0].IP).String(),
		Networks: util.IPNetsToStringSlice(gw.transitRouterInfo.transitRouterNets),
		Options: map[string]string{
			libovsdbops.RequestedTnlKey: getTransitRouterPortTunnelKey(gw.transitRouterInfo.nodeID),
		},
		Peer: ptr.To(gwRouterPortName),
		ExternalIDs: map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		},
	}

	ovnClusterRouter := nbdb.LogicalRouter{Name: gw.clusterRouterName}
	err := libovsdbops.CreateOrUpdateLogicalRouterPort(gw.nbClient, &ovnClusterRouter,
		&ovnClusterRouterToGWRouterPort, nil, &ovnClusterRouterToGWRouterPort.MAC, &ovnClusterRouterToGWRouterPort.Networks,
		&ovnClusterRouterToGWRouterPort.Options, &ovnClusterRouterToGWRouterPort.Peer, &ovnClusterRouterToGWRouterPort.ExternalIDs)
	if err != nil {
		return fmt.Errorf("failed to create port %+v on router %+v: %v", ovnClusterRouterToGWRouterPort, ovnClusterRouter, err)
	}
	return nil
}

func (gw *GatewayManager) deleteGWRouterPeerRouterPort() error {
	ovnClusterRouterToGWRouterPort := nbdb.LogicalRouterPort{Name: gw.getGWRouterPeerRouterPortName()}
	ovnClusterRouter := nbdb.LogicalRouter{Name: gw.clusterRouterName}
	err := libovsdbops.DeleteLogicalRouterPorts(gw.nbClient, &ovnClusterRouter, &ovnClusterRouterToGWRouterPort)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to delete router port %s from router %s: %w", ovnClusterRouterToGWRouterPort.Name, ovnClusterRouter.Name, err)
	}
	return nil
}

func (gw *GatewayManager) createGWRouterPort(gwConfig *GatewayConfig,
	enableGatewayMTU bool, gwRouter *nbdb.LogicalRouter) error {
	gwLRPNetworks := []string{}
	for _, gwRouterJoinNet := range gwConfig.gwRouterJoinCIDRs {
		gwLRPNetworks = append(gwLRPNetworks, gwRouterJoinNet.String())
	}
	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo == nil {
		// At layer2 GR LRP acts as the layer3 ovn_cluster_router so we need
		// to configure here the .1 address, this will work only for IC with
		// one node per zone, since ARPs for .1 will not go beyond local switch.
		// This is being done to add the ICMP SNATs for .1 podSubnet that OVN GR generates
		for _, subnet := range gwConfig.hostSubnets {
			gwLRPNetworks = append(gwLRPNetworks, gw.netInfo.GetNodeGatewayIP(subnet).String())
		}
	}
	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo != nil {
		for _, gatewayRouterTransitNetwork := range gw.transitRouterInfo.gatewayRouterNets {
			gwLRPNetworks = append(gwLRPNetworks, gatewayRouterTransitNetwork.String())
		}
	}
	gwLRPMAC := util.IPAddrToHWAddr(gwConfig.gwRouterJoinCIDRs[0].IP)

	var options map[string]string
	if enableGatewayMTU {
		options = map[string]string{
			libovsdbops.GatewayMTU: strconv.Itoa(config.Default.MTU),
		}
	}

	gwRouterPort := nbdb.LogicalRouterPort{
		Name:     gw.getGWRouterPortName(),
		MAC:      gwLRPMAC.String(),
		Networks: gwLRPNetworks,
		Options:  options,
	}
	if gw.netInfo.IsUserDefinedNetwork() {
		gwRouterPort.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
		if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo != nil {
			gwRouterPort.Peer = ptr.To(gw.getGWRouterPeerRouterPortName())
		}

		_, isNetIPv6 := gw.netInfo.IPMode()
		if gw.netInfo.TopologyType() == types.Layer2Topology && isNetIPv6 && config.IPv6Mode && gw.transitRouterInfo == nil {
			gwRouterPort.Ipv6RaConfigs = map[string]string{
				"address_mode":      "dhcpv6_stateful",
				"send_periodic":     "true",
				"max_interval":      "900", // 15 minutes
				"min_interval":      "300", // 5 minutes
				"router_preference": "LOW", // The static gateway configured by CNI is MEDIUM, so make this SLOW so it has less effect for pods
			}
			if gw.netInfo.MTU() > 0 {
				gwRouterPort.Ipv6RaConfigs["mtu"] = fmt.Sprintf("%d", gw.netInfo.MTU())
			}
		}
	}

	err := libovsdbops.CreateOrUpdateLogicalRouterPort(gw.nbClient, gwRouter,
		&gwRouterPort, nil, &gwRouterPort.MAC, &gwRouterPort.Networks,
		&gwRouterPort.Options)
	if err != nil {
		return fmt.Errorf("failed to create port %+v on router %+v: %v", gwRouterPort, gwRouter, err)
	}
	return nil
}

func (gw *GatewayManager) updateGWRouterStaticRoutes(gwConfig *GatewayConfig, externalRouterPort string,
	gwRouter *nbdb.LogicalRouter) error {
	if len(gwConfig.ovnClusterLRPToJoinIfAddrs) > 0 {
		// this is only the case for layer3 topology
		//
		// In overlay mode the gateway router routes the whole cluster subnet(s)
		// towards the ovn_cluster_router so it can reach pods on every node
		// through the distributed router (over the transit switch).
		//
		// In no-overlay mode there is no transit switch: traffic destined to pods
		// on other nodes must leave through the default route to the physical
		// network instead of being forwarded to the ovn_cluster_router. So the
		// gateway router should only route this node's local host subnet(s)
		// towards the ovn_cluster_router (to reach pods local to this node).
		drRoutedSubnets := gwConfig.clusterSubnets
		if gw.netInfo.Transport() == types.NetworkTransportNoOverlay {
			drRoutedSubnets = gwConfig.hostSubnets
		}
		for _, entry := range drRoutedSubnets {
			drLRPIfAddr, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(entry), gwConfig.ovnClusterLRPToJoinIfAddrs)
			if err != nil {
				return fmt.Errorf("failed to add a static route in GR %s with distributed "+
					"router as the nexthop: %v",
					gw.gwRouterName, err)
			}

			// TODO There has to be a better way to do this. It seems like the
			// whole purpose is to update the appropriate route in case it already
			// exists *only* in the context of this router. But then it does not
			// make sense to refresh it on every loop, unless it is also way to
			// check for duplicate cluster IP subnets for which there would also be
			// a better way to do it. Adding support for indirection in ModelClients
			// opModel (being able to operate on thins pointed to from another model)
			// would be a great way to simplify this.
			updatedGWRouter, err := libovsdbops.GetLogicalRouter(gw.nbClient, gwRouter)
			if err != nil {
				return fmt.Errorf("unable to retrieve logical router %+v: %v", gwRouter, err)
			}

			lrsr := nbdb.LogicalRouterStaticRoute{
				IPPrefix: entry.String(),
				Nexthop:  drLRPIfAddr.IP.String(),
			}
			if gw.netInfo.IsUserDefinedNetwork() {
				lrsr.ExternalIDs = map[string]string{
					types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
					types.TopologyExternalID: gw.netInfo.TopologyType(),
				}
			}
			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == lrsr.IPPrefix && libovsdbops.PolicyEqualPredicate(item.Policy, lrsr.Policy) &&
					util.SliceHasStringItem(updatedGWRouter.StaticRoutes, item.UUID)
			}
			err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient, gw.gwRouterName, &lrsr, p,
				&lrsr.Nexthop)
			if err != nil {
				return fmt.Errorf("failed to add a static route %+v in GR %s with distributed router as the nexthop, err: %v", lrsr, gw.gwRouterName, err)
			}
		}
		// The set of subnets routed towards the ovn_cluster_router differs
		// between overlay and no-overlay mode (see above), so on a transition
		// between the two transports (e.g. an upgrade) the gateway router can be
		// left with stale routes from the previous mode. Remove them so the
		// gateway router converges to the correct set of distributed-router
		// routes for the current transport.
		if gw.netInfo.Transport() == types.NetworkTransportNoOverlay {
			// We now only route this node's host subnet(s) towards the
			// ovn_cluster_router. Remove any stale full cluster-subnet route
			// pointing at the distributed router (programmed previously in
			// overlay mode), so the gateway router does not forward off-node pod
			// traffic to the ovn_cluster_router.
			hostSubnetSet := sets.New[string]()
			for _, hostSubnet := range gwConfig.hostSubnets {
				hostSubnetSet.Insert(hostSubnet.String())
			}
			if err := gw.deleteStaleDRRoutes(gwConfig, gwConfig.clusterSubnets, hostSubnetSet); err != nil {
				return err
			}
		} else {
			// We now route the whole cluster subnet(s) towards the
			// ovn_cluster_router. Remove any stale per-host subnet route pointing
			// at the distributed router (programmed previously in no-overlay
			// mode), so it does not shadow the cluster-subnet route.
			clusterSubnetSet := sets.New[string]()
			for _, clusterSubnet := range gwConfig.clusterSubnets {
				clusterSubnetSet.Insert(clusterSubnet.String())
			}
			if err := gw.deleteStaleDRRoutes(gwConfig, gwConfig.hostSubnets, clusterSubnetSet); err != nil {
				return err
			}
		}
	}
	// for layer2 topology with transit router, add pod subnet routes via transit router, like so:
	// 10.10.0.0/24                100.88.0.8 dst-ip rtotr-GR_<network_name>
	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo != nil {
		for _, subnet := range gwConfig.hostSubnets {
			nexthop, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6(subnet.IP), gw.transitRouterInfo.transitRouterNets)
			if err != nil {
				return err
			}
			subnetRoute := nbdb.LogicalRouterStaticRoute{
				IPPrefix:   subnet.String(),
				Nexthop:    nexthop.IP.String(),
				OutputPort: ptr.To(gw.getGWRouterPortName()),
			}
			subnetRoute.ExternalIDs = map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			}
			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.OutputPort != nil && *item.OutputPort == *subnetRoute.OutputPort && item.IPPrefix == subnetRoute.IPPrefix &&
					libovsdbops.PolicyEqualPredicate(subnetRoute.Policy, item.Policy)
			}
			if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient, gw.gwRouterName, &subnetRoute,
				p, &subnetRoute.Nexthop); err != nil {
				return fmt.Errorf("error creating static route %+v in GW router %s: %v", subnetRoute, gw.gwRouterName, err)
			}
		}
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
		if gw.netInfo.IsUserDefinedNetwork() {
			lrsr.ExternalIDs = map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			}
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.OutputPort != nil && *item.OutputPort == *lrsr.OutputPort && item.IPPrefix == lrsr.IPPrefix &&
				libovsdbops.PolicyEqualPredicate(item.Policy, lrsr.Policy)
		}
		err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient, gw.gwRouterName, &lrsr, p,
			&lrsr.Nexthop)
		if err != nil {
			return fmt.Errorf("error creating service static route %+v in GR %s: %v", lrsr, gw.gwRouterName, err)
		}
	}

	nextHops := gwConfig.annoConfig.NextHops
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
		if gw.netInfo.IsUserDefinedNetwork() {
			lrsr.ExternalIDs = map[string]string{
				types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
				types.TopologyExternalID: gw.netInfo.TopologyType(),
			}
		}
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.OutputPort != nil && *item.OutputPort == *lrsr.OutputPort && item.IPPrefix == lrsr.IPPrefix &&
				libovsdbops.PolicyEqualPredicate(lrsr.Policy, item.Policy)
		}
		err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient, gw.gwRouterName, &lrsr,
			p, &lrsr.Nexthop)
		if err != nil {
			return fmt.Errorf("error creating static route %+v in GR %s: %v", lrsr, gw.gwRouterName, err)
		}
	}
	return nil
}

// deleteStaleDRRoutes removes routes on the gateway router that point at the
// distributed router (ovn_cluster_router) for any subnet in staleSubnets,
// skipping subnets present in keepSubnets. It is used to clean up routes left
// over from the previous network transport when transitioning between overlay
// and no-overlay mode (the two modes route a different set of subnets towards
// the distributed router).
func (gw *GatewayManager) deleteStaleDRRoutes(gwConfig *GatewayConfig, staleSubnets []*net.IPNet, keepSubnets sets.Set[string]) error {
	for _, subnet := range staleSubnets {
		if keepSubnets.Has(subnet.String()) {
			continue
		}
		drLRPIfAddr, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(subnet), gwConfig.ovnClusterLRPToJoinIfAddrs)
		if err != nil {
			continue
		}
		prefix := subnet.String()
		nexthop := drLRPIfAddr.IP.String()
		p := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.IPPrefix == prefix && item.Nexthop == nexthop && item.OutputPort == nil
		}
		if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(gw.nbClient, gw.gwRouterName, p); err != nil {
			return fmt.Errorf("failed to delete stale route %s via %s in GR %s: %v",
				prefix, nexthop, gw.gwRouterName, err)
		}
	}
	return nil
}

func (gw *GatewayManager) updateClusterRouterStaticRoutes(gwConfig *GatewayConfig, gwRouterIPs []net.IP) error {
	// We need to add a route to the Gateway router's IP, on the
	// cluster router, to ensure that the return traffic goes back
	// to the same gateway router
	//
	// This can be removed once https://bugzilla.redhat.com/show_bug.cgi?id=1891516 is fixed.
	// FIXME(trozet): if LRP IP is changed, we do not remove stale instances of these routes
	nextHops := gwRouterIPs
	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo != nil {
		nextHops = util.IPNetsToIPs(gw.transitRouterInfo.gatewayRouterNets)
	}

	for _, gwRouterIP := range gwRouterIPs {
		nextHop, err := util.MatchIPFamily(utilnet.IsIPv6(gwRouterIP), nextHops)
		if err != nil {
			if gw.transitRouterInfo != nil {
				// for layer2 networks with transit router it is not an error.
				// JoinIPs are allocated for both IP families always, but transit router IPs and routes
				// are only created for the actual IP families of the network
				continue
			}
			return fmt.Errorf("failed to add source IP address based "+
				"routes in distributed router %s: %v",
				gw.clusterRouterName, err)
		}

		lrsr := nbdb.LogicalRouterStaticRoute{
			IPPrefix: gwRouterIP.String(),
			Nexthop:  nextHop[0].String(),
		}
		if gw.netInfo.IsUserDefinedNetwork() {
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
			err = libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(gw.nbClient,
				gw.clusterRouterName, &lrsr, p, &lrsr.Nexthop)
			if err != nil {
				return fmt.Errorf("error creating static route %+v in %s: %v", lrsr, gw.clusterRouterName, err)
			}
		}
	}
	if gw.clusterRouterName == "" {
		return nil
	}

	// Add source IP address based routes in distributed router
	// for this gateway router.
	for _, hostSubnet := range gwConfig.hostSubnets {
		nextHop, err := util.MatchIPFamily(utilnet.IsIPv6CIDR(hostSubnet), nextHops)
		if err != nil {
			return fmt.Errorf("failed to add source IP address based "+
				"routes in distributed router %s: %v",
				gw.clusterRouterName, err)
		}

		lrsr := nbdb.LogicalRouterStaticRoute{
			Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
			IPPrefix: hostSubnet.String(),
			Nexthop:  nextHop[0].String(),
		}

		if config.Gateway.Mode != config.GatewayModeLocal {
			if gw.netInfo.IsUserDefinedNetwork() {
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
			mgmtIfAddr := gw.netInfo.GetNodeManagementIP(hostSubnet)
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
	return nil
}

// syncNATsForGRIPChange updates the SNAT rules on the gateway router that are created outside the GatewayManager.
// Multiple handlers, like
// - DefaultNetworkController.addLogicalPort
// - DefaultNetworkController.updateNamespace
// - EgressIPController.addExternalGWPodSNATOps
// - EgressIPController.addPodEgressIPAssignment
// - Layer2UserDefinedNetworkController.buildUDNEgressSNAT
// - Layer3UserDefinedNetworkController.addUDNNodeSubnetEgressSNAT
// use gateway config parameters to create SNAT rules on the gateway router, but some of them (not all) don't watch
// gateway config changes and rely on the GatewayManager to update their SNAT rules.
// Is it racy? Yes!
// This function also updates SNAT created by `updateGWRouterNAT`, because NATs don't use ExternalIDs,
// and their fields are used to find equivalent NATs. That means on gateway IPs change, instead of updating
// the old NAT, we would create a new one. FIXME: add externalIDs to NATs
func (gw *GatewayManager) syncNATsForGRIPChange(gwConfig *GatewayConfig, oldExtIPs, gwRouterIPs []net.IP,
	gwRouter, oldGWRouter *nbdb.LogicalRouter) error {
	// if config.Gateway.DisabledSNATMultipleGWs is not set (by default it is not),
	// the NAT rules for pods not having annotations to route through either external
	// gws or pod CNFs will be added within pods.go addLogicalPort
	var natsToUpdate []*nbdb.NAT
	// If l3gatewayAnnotation.IPAddresses changed, we need to update the SNATs on the GR
	oldNATs := []*nbdb.NAT{}
	var err error
	if oldGWRouter != nil {
		oldNATs, err = libovsdbops.GetRouterNATs(gw.nbClient, oldGWRouter)
		if err != nil && errors.Is(err, libovsdbclient.ErrNotFound) {
			return fmt.Errorf("unable to get NAT entries for router %s: %w", oldGWRouter.Name, err)
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
		for _, externalIP := range gwConfig.externalIPs {
			oldExternalIP, err := util.MatchFirstIPFamily(utilnet.IsIPv6(externalIP), oldExtIPs)
			if err != nil {
				return fmt.Errorf("failed to update GW SNAT rule for pods on router %s error: %v", gw.gwRouterName, err)
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
			joinIP, err := util.MatchFirstIPFamily(utilnet.IsIPv6(parsedLogicalIP), gwRouterIPs)
			if err != nil {
				return fmt.Errorf("failed to find valid IP family match for join subnet IP: %s on "+
					"gateway router: %s, provided IPs: %#v", parsedLogicalIP, gw.gwRouterName, gwRouterIPs)
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
		err = libovsdbops.CreateOrUpdateNATs(gw.nbClient, gwRouter, natsToUpdate...)
		if err != nil {
			return fmt.Errorf("failed to update GW SNAT rule for pod on router %s error: %v", gw.gwRouterName, err)
		}
	}
	return nil
}

func (gw *GatewayManager) updateGWRouterNAT(nodeName string, gwConfig *GatewayConfig, gwLRPIPs []net.IP, gwRouter *nbdb.LogicalRouter) error {
	// REMOVEME(trozet) workaround - create join subnet SNAT to handle ICMP needs frag return
	var extIDs map[string]string
	if gw.netInfo.IsUserDefinedNetwork() {
		extIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}
	joinNATs := make([]*nbdb.NAT, 0, len(gwLRPIPs))
	for _, gwLRPIP := range gwLRPIPs {
		externalIP, err := util.MatchIPFamily(utilnet.IsIPv6(gwLRPIP), gwConfig.externalIPs)
		if err != nil {
			return fmt.Errorf("failed to find valid external IP family match for join subnet IP: %s on "+
				"gateway router: %s", gwLRPIP, gw.gwRouterName)
		}
		joinIPNet, err := util.GetIPNetFullMask(gwLRPIP.String())
		if err != nil {
			return fmt.Errorf("failed to parse full CIDR mask for join subnet IP: %s", gwLRPIP)
		}
		nat := libovsdbops.BuildSNAT(&externalIP[0], joinIPNet, "", extIDs)
		joinNATs = append(joinNATs, nat)
	}
	err := libovsdbops.CreateOrUpdateNATs(gw.nbClient, gwRouter, joinNATs...)
	if err != nil {
		return fmt.Errorf("failed to create SNAT rule for join subnet on router %s error: %v", gw.gwRouterName, err)
	}

	nats := make([]*nbdb.NAT, 0, len(gwConfig.clusterSubnets))
	var nat *nbdb.NAT
	// DisableSNATMultipleGWs is only applicable to cluster default network and not to user defined networks.
	// For user defined networks, we always add SNAT rules regardless of whether the network is advertised or not.
	if !config.Gateway.DisableSNATMultipleGWs || gw.netInfo.IsPrimaryNetwork() {
		var v4UUID, v6UUID string
		var err error
		snatExemptionNeeded := util.IsNoOverlaySNATExemptionNeeded(gw.netInfo)
		controllerName := getNetworkControllerName(gw.netInfo.GetNetworkName())
		if snatExemptionNeeded {
			// Get the no-overlay SNAT exemption address set UUIDs
			addressSetFactory := addressset.NewOvnAddressSetFactory(gw.nbClient, config.IPv4Mode, config.IPv6Mode)
			v4UUID, v6UUID, err = getNoOverlaySNATExemptionAsUUID(addressSetFactory, gw.netInfo, controllerName)
			if err != nil {
				return fmt.Errorf("failed to get no-overlay SNAT exemption address set UUID: %w", err)
			}
		}

		isNetworkAdvertised := gw.isRoutingAdvertised(nodeName)
		var clusterNodeIPsAddrSetDbIDs *libovsdbops.DbObjectIDs
		if isNetworkAdvertised && !snatExemptionNeeded {
			clusterNodeIPsAddrSetDbIDs, err = gw.addressSetManager.EnsureClusterNodeIPsAddressSet(addresssetmanager.ClusterNodeIPsRouteAdvertisementsBackRef)
			if err != nil {
				return fmt.Errorf("failed to ensure cluster node IP address set for route advertisements: %w", err)
			}
		}

		// Default SNAT rules. DisableSNATMultipleGWs=false in LGW (traffic egresses via mp0) always.
		// We are not checking for gateway mode to be shared explicitly to reduce topology differences.
		for _, entry := range gwConfig.clusterSubnets {
			externalIP, err := util.MatchIPFamily(utilnet.IsIPv6CIDR(entry), gwConfig.externalIPs)
			if err != nil {
				return fmt.Errorf("failed to create default SNAT rules for gateway router %s: %v",
					gw.gwRouterName, err)
			}

			// Get the match for this specific subnet's IP family
			ipFamily := utilnet.IPv4
			if utilnet.IsIPv6CIDR(entry) {
				ipFamily = utilnet.IPv6
			}
			snatMatch, err := GetNetworkScopedClusterSubnetSNATMatch(gw.nbClient, gw.netInfo, nodeName,
				isNetworkAdvertised, ipFamily, clusterNodeIPsAddrSetDbIDs)
			if err != nil {
				return fmt.Errorf("failed to get SNAT match for node %s for network %s: %w", nodeName, gw.netInfo.GetNetworkName(), err)
			}

			exemptedExtIPs := ""
			if ipFamily == utilnet.IPv6 {
				exemptedExtIPs = v6UUID
			} else {
				exemptedExtIPs = v4UUID
			}

			natExtIDs := extIDs
			if snatExemptionNeeded {
				// Mark the SNAT with DB IDs so that it can be told apart from
				// cluster-subnet SNATs created without exemptions.
				natExtIDs = getNoOverlayClusterSubnetSNATDbIDs(controllerName, gw.netInfo.GetNetworkName(), nodeName, entry).GetExternalIDs()
				maps.Copy(natExtIDs, extIDs)
			}

			nat = libovsdbops.BuildSNATWithExemptedExtIPs(&externalIP[0], entry, "", natExtIDs, snatMatch, exemptedExtIPs)
			nats = append(nats, nat)
		}
		natOps, err := libovsdbops.CreateOrUpdateNATsOps(gw.nbClient, nil, gwRouter, nats...)
		if err != nil {
			return fmt.Errorf("failed to build SNAT ops for pod on router %s error: %w", gw.gwRouterName, err)
		}
		if snatExemptionNeeded {
			// A cluster-subnet SNAT without our DB IDs is not matched by the ops
			// above and would keep SNATing exempted traffic: delete such stale
			// rows in the same transaction, so the exempted SNAT never coexists
			// with a stale twin. Stale rows are left behind by versions that
			// predate the DB IDs or by a transport switch from overlay to
			// no-overlay.
			natOps, err = gw.deleteStaleNoOverlayClusterSNATsOps(natOps, gwRouter, controllerName, nats)
			if err != nil {
				return fmt.Errorf("failed to build stale no-overlay SNAT cleanup ops on router %s error: %w", gw.gwRouterName, err)
			}
		}
		if _, err := libovsdbops.TransactAndCheck(gw.nbClient, natOps); err != nil {
			return fmt.Errorf("failed to update SNAT rule for pod on router %s error: %w", gw.gwRouterName, err)
		}
	} else {
		// ensure we do not have any leftover SNAT entries after an upgrade
		for _, logicalSubnet := range gwConfig.clusterSubnets {
			nat = libovsdbops.BuildSNAT(nil, logicalSubnet, "", extIDs)
			nats = append(nats, nat)
		}
		err = libovsdbops.DeleteNATs(gw.nbClient, gwRouter, nats...)
		if err != nil {
			return fmt.Errorf("failed to delete GW SNAT rule for pod on router %s error: %v", gw.gwRouterName, err)
		}
	}

	if err = gw.cleanupStalePodSNATs(nodeName, gwConfig.annoConfig.IPAddresses, gwLRPIPs); err != nil {
		return fmt.Errorf("failed to sync stale SNATs on node %s: %v", nodeName, err)
	}
	return nil
}

// getNoOverlayClusterSubnetSNATDbIDs returns the DB IDs identifying the SNAT
// that a gateway router applies to a cluster subnet, with no-overlay SNAT
// exemptions, on the network scoped by controllerName and networkName.
func getNoOverlayClusterSubnetSNATDbIDs(controllerName, networkName, nodeName string, clusterSubnet *net.IPNet) *libovsdbops.DbObjectIDs {
	// Use the same ip-family value ("ip4"/"ip6") as the other NAT owners so the
	// rows can be looked up consistently.
	ipFamily := string(IPFamilyValueV4)
	if utilnet.IsIPv6CIDR(clusterSubnet) {
		ipFamily = string(IPFamilyValueV6)
	}
	return libovsdbops.NewDbObjectIDs(libovsdbops.NATNoOverlayClusterSubnetSNAT, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: nodeName,
			libovsdbops.CIDRKey:       libovsdbops.BuildCIDRKey(clusterSubnet.String()),
			libovsdbops.IPFamilyKey:   ipFamily,
			libovsdbops.NetworkKey:    networkName,
		})
}

// deleteStaleNoOverlayClusterSNATsOps appends operations that delete stale
// cluster-subnet SNATs from the gateway router. desiredNATs are the DB-ID-marked
// SNATs that the router should have. A stale SNAT is either a marked row whose
// DB IDs no longer match any desired SNAT, e.g. for a cluster subnet that was
// removed, or an unmarked SNAT for the same logical IP as a desired SNAT, left
// behind by a version that predates the DB IDs or by a default network
// transport switch from overlay to no-overlay. Either would keep SNATing
// traffic that must be exempted: deleting them in the same transaction that
// creates or updates the desired SNATs guarantees the exempted SNAT never
// coexists with a stale twin.
func (gw *GatewayManager) deleteStaleNoOverlayClusterSNATsOps(ops []ovsdb.Operation, gwRouter *nbdb.LogicalRouter, controllerName string, desiredNATs []*nbdb.NAT) ([]ovsdb.Operation, error) {
	// A desired row is identified by the primary ID of its DB IDs, which this
	// function computes on its own: identifying it by row UUID would depend on
	// CreateOrUpdateNATsOps having already resolved the row UUIDs into
	// desiredNATs, that is, on how the enclosing transaction is built.
	primaryIDKey := libovsdbops.PrimaryIDKey.String()
	desiredPrimaryIDs := sets.Set[string]{}
	for _, desiredNAT := range desiredNATs {
		desiredPrimaryIDs.Insert(desiredNAT.ExternalIDs[primaryIDKey])
	}

	routerNATs, err := libovsdbops.GetRouterNATs(gw.nbClient, gwRouter)
	if err != nil {
		return ops, fmt.Errorf("unable to get NAT entries for router %+v: %w", gwRouter, err)
	}

	isMarkedClusterSubnetSNAT := libovsdbops.GetPredicate[*nbdb.NAT](
		libovsdbops.NewDbObjectIDs(libovsdbops.NATNoOverlayClusterSubnetSNAT, controllerName, nil), nil)

	staleNATUUIDs := sets.Set[string]{}
	for _, routerNAT := range routerNATs {
		if isMarkedClusterSubnetSNAT(routerNAT) {
			// Keep the marked rows that are still desired.
			if !desiredPrimaryIDs.Has(routerNAT.ExternalIDs[primaryIDKey]) {
				staleNATUUIDs.Insert(routerNAT.UUID)
			}
			continue
		}
		// An unmarked SNAT for the same cluster subnet as a desired SNAT,
		// created without exemptions before they were enabled on this network.
		// The logical IP alone identifies it: the gateway router holds at most
		// one cluster-subnet SNAT per subnet, so any twin, even one carrying a
		// stale external IP, is stale.
		for _, desiredNAT := range desiredNATs {
			if routerNAT.Type == nbdb.NATTypeSNAT &&
				routerNAT.LogicalIP == desiredNAT.LogicalIP &&
				ptr.Equal(routerNAT.LogicalPort, desiredNAT.LogicalPort) {
				staleNATUUIDs.Insert(routerNAT.UUID)
				break
			}
		}
	}

	if staleNATUUIDs.Len() == 0 {
		return ops, nil
	}

	// Delete by exact row UUID: a model-based delete falls back to NAT
	// equivalence, which ignores exempted_ext_ips and matches external IDs as a
	// subset, so an unmarked twin's model would also match its marked desired
	// row and delete it.
	ops, err = libovsdbops.DeleteNATsWithPredicateOps(gw.nbClient, ops, func(nat *nbdb.NAT) bool {
		return staleNATUUIDs.Has(nat.UUID)
	})
	if err != nil {
		return ops, fmt.Errorf("unable to build stale no-overlay SNAT cleanup operations for router %s: %w", gwRouter.Name, err)
	}
	return ops, nil
}

// gatewayInit creates a gateway router for the local chassis.
// enableGatewayMTU enables options:gateway_mtu for gateway routers.
func (gw *GatewayManager) gatewayInit(
	nodeName string,
	gwConfig *GatewayConfig,
	enableGatewayMTU bool,
) error {

	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.clusterRouterName != "" {
		// layer2 network uses transit router, so we need to set the transit router info
		// in all the other operations we can use both `gw.clusterRouterName == ""` and `gw.transitRouterInfo == nil`
		// as an indicator of the old topology.
		err := gw.setTransitRouterInfo(nodeName)
		if err != nil {
			return fmt.Errorf("failed to initialize layer2 info for gateway on node %s: %v", nodeName, err)
		}
	}
	// If l3gatewayAnnotation.IPAddresses changed, we need to update the perPodSNATs,
	// so let's save the old value before we update the router for later use
	var oldExtIPs []net.IP
	oldLogicalRouter, err := libovsdbops.GetLogicalRouter(gw.nbClient,
		&nbdb.LogicalRouter{
			Name: gw.gwRouterName,
		})
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed in retrieving %s, error: %v", gw.gwRouterName, err)
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

	gwRouter, err := gw.createGWRouter(gwConfig)
	if err != nil {
		return err
	}

	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo != nil {
		err = gw.createGWRouterPeerRouterPort()
	} else {
		err = gw.createGWRouterPeerSwitchPort(nodeName)
	}
	if err != nil {
		return err
	}

	err = gw.createGWRouterPort(gwConfig, enableGatewayMTU, gwRouter)
	if err != nil {
		return err
	}

	if err := gw.addExternalSwitch("",
		gwConfig.annoConfig.InterfaceID,
		gw.gwRouterName,
		gwConfig.annoConfig.MACAddress.String(),
		physNetName(gw.netInfo),
		gwConfig.annoConfig.IPAddresses,
		gwConfig.annoConfig.VLANID); err != nil {
		return err
	}

	if gwConfig.annoConfig.EgressGWInterfaceID != "" {
		if err := gw.addExternalSwitch(types.EgressGWSwitchPrefix,
			gwConfig.annoConfig.EgressGWInterfaceID,
			gw.gwRouterName,
			gwConfig.annoConfig.EgressGWMACAddress.String(),
			types.PhysicalNetworkExGwName,
			gwConfig.annoConfig.EgressGWIPAddresses,
			nil); err != nil {
			return err
		}
	}

	// Remove stale OVN resources with any old masquerade IP
	if err := deleteStaleMasqueradeResources(gw.nbClient, gw.gwRouterName, nodeName, gw.watchFactory); err != nil {
		return fmt.Errorf("failed to remove stale masquerade resources from northbound database: %w", err)
	}

	if err := gateway.CreateDummyGWMacBindings(gw.nbClient, gw.gwRouterName, gw.netInfo); err != nil {
		return err
	}

	externalRouterPort := types.GWRouterToExtSwitchPrefix + gw.gwRouterName
	if err = gw.updateGWRouterStaticRoutes(gwConfig, externalRouterPort, gwRouter); err != nil {
		return err
	}

	gwRouterIPs := util.IPNetsToIPs(gwConfig.gwRouterJoinCIDRs)
	if err = gw.updateClusterRouterStaticRoutes(gwConfig, gwRouterIPs); err != nil {
		return err
	}

	if err = gw.syncNATsForGRIPChange(gwConfig, oldExtIPs, gwRouterIPs, gwRouter, oldLogicalRouter); err != nil {
		return err
	}

	if err = gw.updateGWRouterNAT(nodeName, gwConfig, gwRouterIPs, gwRouter); err != nil {
		return err
	}

	// recording gateway mode metrics here after gateway setup is done
	metrics.RecordEgressRoutingViaHost()

	return nil
}

// GetNetworkScopedClusterSubnetSNATMatch returns the match for the SNAT rule for the cluster default network
// and the match for the SNAT rule for L3/L2 user defined networks.
// Short-circuit(no-overlay mode):
// - If the network is advertised and outbound SNAT is enabled, the match is empty for all topologies.
// If the network is not advertised:
// - For Layer2 topology, the match is the output port of the GR to the join switch since in L2 there is only 1 router but two cSNATs.
// - For Layer3 topology, the match is empty.
// If the network is advertised:
// - For Layer2 topology, the match is the output port of the GR to the join switch and the destination must be a nodeIP in the cluster.
// - For Layer3 topology, the match is the destination must be a nodeIP in the cluster.
func GetNetworkScopedClusterSubnetSNATMatch(nbClient libovsdbclient.Client, netInfo util.NetInfo, nodeName string,
	isNetworkAdvertised bool, ipFamily utilnet.IPFamily, clusterNodeIPsAddrSetDbIDs *libovsdbops.DbObjectIDs) (string, error) {
	layer2OldTopo := netInfo.TopologyType() == types.Layer2Topology && !config.Layer2UsesTransitRouter
	if !isNetworkAdvertised {
		if !layer2OldTopo {
			return "", nil
		}
		return fmt.Sprintf("outport == %q", types.GWRouterToExtSwitchPrefix+netInfo.GetNetworkScopedGWRouterName(nodeName)), nil
	} else if util.IsNoOverlaySNATExemptionNeeded(netInfo) {
		return "", nil
	}

	// if the network is advertised, we need to ensure that the SNAT exists with the correct conditional destination match
	if clusterNodeIPsAddrSetDbIDs == nil {
		return "", fmt.Errorf("cluster node IP address set DB IDs are required for advertised network %s", netInfo.GetNetworkName())
	}
	addressSetFactory := addressset.NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)
	addrSet, err := addressSetFactory.GetAddressSet(clusterNodeIPsAddrSetDbIDs)
	if err != nil {
		return "", fmt.Errorf("cannot ensure that addressSet %v exists: %w", clusterNodeIPsAddrSetDbIDs, err)
	}
	destinationMatch := getClusterNodesDestinationBasedSNATMatch(ipFamily, addrSet)
	if destinationMatch == "" {
		return "", fmt.Errorf("could not build a destination based SNAT match because no addressSet %v exists for IP family %v", clusterNodeIPsAddrSetDbIDs, ipFamily)
	}
	if !layer2OldTopo {
		return destinationMatch, nil
	}
	return fmt.Sprintf("outport == %q && %s", types.GWRouterToExtSwitchPrefix+netInfo.GetNetworkScopedGWRouterName(nodeName), destinationMatch), nil
}

// addExternalSwitch creates a switch connected to the external bridge and connects it to
// the gateway router
func (gw *GatewayManager) addExternalSwitch(prefix, interfaceID, gatewayRouter, macAddress, physNetworkName string, ipAddresses []*net.IPNet, vlanID *uint) error {
	// The stale localnet port cleanup below removes every localnet port not
	// named interfaceID: an empty ID would drop the switch's only one and
	// insert a nameless port in its place.
	if interfaceID == "" {
		return fmt.Errorf("failed to add external switch %s%s: empty interface ID", prefix, gw.extSwitchName)
	}
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
	if gw.netInfo.IsUserDefinedNetwork() {
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
	if gw.netInfo.IsUserDefinedNetwork() {
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
	etorOptions := map[string]string{
		libovsdbops.RouterPort: externalRouterPort,
	}
	if !gw.netInfo.IsUserDefinedNetwork() {
		// nat-addresses=router programs OVN to send GARPs for all external IPs
		// that the logical switch port has been configured to use. This is
		// necessary for egress IP because if an egress IP is moved between two
		// nodes, the nodes need to actively update the ARP cache of all neighbors
		// as to notify them the change. If this is not the case: packets will
		// continue to be routed to the old node which hosted the egress IP before
		// it was moved, and the connections will fail.
		// For UDNs this is not set: UDN GRs only carry masquerade IPs which are
		// link-local and identical across nodes, so GARPs for them cause an ARP
		// storm on the physical network. EgressIP for UDNs has no SNATs on the
		// GR, so nat-addresses=router is not needed.
		etorOptions["nat-addresses"] = "router"

		// Setting nat-addresses to router will send out GARPs for all externalIPs and LB VIPs
		// hosted on the GR. Setting exclude-lb-vips-from-garp to true will make sure GARPs for
		// LB VIPs are not sent, thereby preventing GARP overload.
		etorOptions["exclude-lb-vips-from-garp"] = "true"
	}
	externalLogicalSwitchPortToRouter := nbdb.LogicalSwitchPort{
		Name:      externalSwitchPortToRouter,
		Type:      "router",
		Options:   etorOptions,
		Addresses: []string{macAddress},
	}

	if gw.netInfo.IsUserDefinedNetwork() {
		externalLogicalSwitchPortToRouter.ExternalIDs = map[string]string{
			types.NetworkExternalID:  gw.netInfo.GetNetworkName(),
			types.TopologyExternalID: gw.netInfo.TopologyType(),
		}
	}
	sw := nbdb.LogicalSwitch{Name: externalSwitch}
	if gw.netInfo.IsUserDefinedNetwork() {
		sw.ExternalIDs = util.GenerateExternalIDsForSwitchOrRouter(gw.netInfo)
	}

	// gatewayInit creates exactly one localnet port per external switch. Its
	// name embeds the bridge name, so a bridge change (Uplink nodeConfig) yields
	// a new port name on the same switch. ovn-controller forwards through the
	// first localnet port it bound, so a leftover old port keeps egress on its
	// patch port. Delete it in the same transaction.
	ops, err := libovsdbops.DeleteLogicalSwitchPortsWithPredicateOps(gw.nbClient, nil, &sw,
		func(lsp *nbdb.LogicalSwitchPort) bool {
			return lsp.Type == "localnet" && lsp.Name != interfaceID
		})
	if err != nil {
		return fmt.Errorf("failed to build ops removing the stale localnet port from switch %s: %w", externalSwitch, err)
	}
	ops, err = libovsdbops.CreateOrUpdateLogicalSwitchPortsAndSwitchOps(gw.nbClient, ops, &sw,
		&externalLogicalSwitchPort, &externalLogicalSwitchPortToRouter)
	if err != nil {
		return fmt.Errorf("failed to build ops creating localnet port %s and router port %s on switch %s: %w",
			interfaceID, externalSwitchPortToRouter, externalSwitch, err)
	}
	if _, err = libovsdbops.TransactAndCheck(gw.nbClient, ops); err != nil {
		return fmt.Errorf("failed to replace localnet port %s and update router port %s on switch %s: %w",
			interfaceID, externalSwitchPortToRouter, externalSwitch, err)
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
		if apierrors.IsNotFound(err) {
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

	var matchNextHop func(nexthop string) bool
	if len(nextHopIPs) == 0 {
		configuredNextHops := sets.New(
			config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP.String(),
			config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP.String(),
		)
		matchNextHop = func(nexthop string) bool {
			return !configuredNextHops.Has(nexthop)
		}
	} else {
		targetNextHops := sets.New[string]()
		for _, ip := range nextHopIPs {
			targetNextHops.Insert(ip.String())
		}
		matchNextHop = func(nexthop string) bool {
			return targetNextHops.Has(nexthop)
		}
	}

	var staleNextHops []string
	staticRoutePredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
		if item.OutputPort == nil || *item.OutputPort != logicalport {
			return false
		}
		if _, ok := item.ExternalIDs[util.OvnNodeMasqCIDR]; !ok {
			return false
		}
		if matchNextHop(item.Nexthop) {
			staleNextHops = append(staleNextHops, item.Nexthop)
			return true
		}
		return false
	}

	var ops []ovsdb.Operation
	ops, err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(nbClient, ops, routerName, staticRoutePredicate)
	if err != nil {
		return fmt.Errorf("failed to build ops to delete stale masquerade routes from router %s: %w", routerName, err)
	}

	for _, staleNextHop := range staleNextHops {
		klog.Infof("Stale masquerade route found on router %s with nexthop %s", routerName, staleNextHop)
		macBindingPredicate := func(item *nbdb.StaticMACBinding) bool {
			return item.LogicalPort == logicalport && item.IP == staleNextHop
		}
		ops, err = libovsdbops.DeleteStaticMACBindingWithPredicateOps(nbClient, ops, macBindingPredicate)
		if err != nil {
			return fmt.Errorf("failed to build ops to delete static MAC binding for logical port %s: %w", logicalport, err)
		}
	}

	if _, err := libovsdbops.TransactAndCheck(nbClient, ops); err != nil {
		return fmt.Errorf("failed to delete stale masquerade routes and MAC bindings from router %s: %w", routerName, err)
	}
	return nil
}

// Cleanup removes all the NB DB objects created for a node's gateway
func (gw *GatewayManager) Cleanup() error {
	// Get the gateway router port's IP address (connected to join switch)
	var nextHops []net.IP

	gwRouterPortName := gw.getGWRouterPortName()

	gwIPAddrs, err := libovsdbutil.GetLRPAddrs(gw.nbClient, gwRouterPortName)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf(
			"failed to get gateway IPs for network %q from LRP %s: %v",
			gw.netInfo.GetNetworkName(),
			gwRouterPortName,
			err,
		)
	}

	for _, gwIPAddr := range gwIPAddrs {
		nextHops = append(nextHops, gwIPAddr.IP)
	}
	gw.staticRouteCleanup(nextHops, nil)
	gw.policyRouteCleanup(nextHops)

	if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo != nil {
		err = gw.deleteGWRouterPeerRouterPort()
	} else {
		err = gw.deleteGWRouterPeerSwitchPort()
	}
	if err != nil {
		return err
	}

	// Remove the static mac bindings of the gateway router
	err = gateway.DeleteDummyGWMacBindings(gw.nbClient, gw.gwRouterName, gw.netInfo)
	if err != nil {
		return fmt.Errorf("failed to delete GR dummy mac bindings for node %s: %w", gw.nodeName, err)
	}

	// Remove the gateway router associated with nodeName
	logicalRouter := nbdb.LogicalRouter{Name: gw.gwRouterName}
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

// NewGatewayManagerForCleanup returns a minimal GatewayManager used only for Cleanup(). Used when
// discovering gateway routers from the DB (e.g. stale cleanup when nodes are gone). layer2UseTransitRouter
// selects the peer port cleanup path (transit router LRP vs join switch LSP).
//
// NOTE: transitRouterInfo is set to an empty struct (not nil) when layer2UseTransitRouter is true.
// This is safe because Cleanup() only checks (transitRouterInfo != nil) to choose between
// deleteGWRouterPeerRouterPort and deleteGWRouterPeerSwitchPort — neither of which accesses
// transitRouterInfo fields. If Cleanup() is ever changed to dereference transitRouterInfo fields,
// this constructor must be updated accordingly.
func NewGatewayManagerForCleanup(
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	clusterRouterName, joinSwitchName, gwRouterName, nodeName string,
	layer2UseTransitRouter bool,
) *GatewayManager {
	var tri *transitRouterInfo
	if layer2UseTransitRouter {
		tri = &transitRouterInfo{}
	}
	return &GatewayManager{
		nodeName:          nodeName,
		clusterRouterName: clusterRouterName,
		gwRouterName:      gwRouterName,
		extSwitchName:     netInfo.GetNetworkScopedExtSwitchName(nodeName),
		joinSwitchName:    joinSwitchName,
		nbClient:          nbClient,
		netInfo:           netInfo,
		transitRouterInfo: tri,
	}
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
		networkName, isUserDefinedNetwork := item.ExternalIDs[types.NetworkExternalID]
		if !isUserDefinedNetwork {
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
			networkName, isUserDefinedNetwork := item.ExternalIDs[types.NetworkExternalID]
			if !isUserDefinedNetwork {
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
		networkName, isUserDefinedNetwork := item.ExternalIDs[types.NetworkExternalID]
		if !isUserDefinedNetwork {
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
	return util.IsPodNetworkAdvertisedAtNode(gw.netInfo, node)
}

// SyncGateway ensures a node's gateway router is configured according to the L3 config and host subnets
func (gw *GatewayManager) SyncGateway(
	node *corev1.Node,
	gwConfig *GatewayConfig,
) error {
	if gwConfig.annoConfig.Mode == config.GatewayModeDisabled {
		if err := gw.Cleanup(); err != nil {
			return fmt.Errorf("error cleaning up gateway for node %s: %v", node.Name, err)
		}
		return nil
	}
	if gwConfig.hostSubnets == nil {
		return nil
	}

	enableGatewayMTU := util.ParseNodeGatewayMTUSupport(node)

	err := gw.gatewayInit(
		node.Name,
		gwConfig,
		enableGatewayMTU,
	)
	if err != nil {
		return fmt.Errorf("failed to init gateway for network %q: %v", gw.netInfo.GetNetworkName(), err)
	}

	routerName := gw.clusterRouterName
	if gw.clusterRouterName == "" {
		routerName = gw.gwRouterName
	}
	for _, subnet := range gwConfig.hostSubnets {
		mgmtIfAddr := gw.netInfo.GetNodeManagementIP(subnet)
		if mgmtIfAddr == nil {
			return fmt.Errorf("management interface address not found for subnet %q on network %q", subnet, gw.netInfo.GetNetworkName())
		}
		l3GatewayConfigIP, err := util.MatchFirstIPNetFamily(utilnet.IsIPv6(mgmtIfAddr.IP), gwConfig.annoConfig.IPAddresses)
		if err != nil {
			return fmt.Errorf("failed to extract the gateway IP addr for network %q: %v", gw.netInfo.GetNetworkName(), err)
		}
		relevantHostIPs, err := util.MatchAllIPStringFamily(utilnet.IsIPv6(mgmtIfAddr.IP), gwConfig.hostAddrs)
		if err != nil && err != util.ErrorNoIP {
			return fmt.Errorf("failed to extract the host IP addrs for network %q: %v", gw.netInfo.GetNetworkName(), err)
		}
		pbrMngr := gatewayrouter.NewPolicyBasedRoutesManager(gw.nbClient, routerName, gw.netInfo)
		if err := pbrMngr.AddSameNodeIPPolicy(node.Name, mgmtIfAddr.IP.String(), l3GatewayConfigIP, relevantHostIPs); err != nil {
			return fmt.Errorf("failed to configure the policy based routes for network %q: %v", gw.netInfo.GetNetworkName(), err)
		}
		if gw.netInfo.TopologyType() == types.Layer2Topology && gw.transitRouterInfo == nil && config.Gateway.Mode == config.GatewayModeLocal {
			if err := pbrMngr.AddHostCIDRPolicy(node, mgmtIfAddr.IP.String(), subnet.String()); err != nil {
				return fmt.Errorf("failed to configure the hostCIDR policy for L2 network %q on local gateway: %v",
					gw.netInfo.GetNetworkName(), err)
			}
		}
	}

	return nil
}

func physNetName(netInfo util.NetInfo) string {
	if netInfo.IsDefault() || (netInfo.IsPrimaryNetwork() && netInfo.Uplink() == "") {
		return types.PhysicalNetworkName
	}
	return netInfo.GetNetworkName()
}

func (gw *GatewayManager) setTransitRouterInfo(nodeName string) error {
	node, err := gw.watchFactory.GetNode(nodeName)
	if err != nil {
		return err
	}
	gw.transitRouterInfo, err = getTransitRouterInfo(gw.netInfo, node)
	if err != nil {
		return err
	}
	return nil
}

// AddPodSNATOps adds operations to create or update SNAT rules for a pod's IP addresses.
// This function handles the different SNAT behaviors based on transport mode (overlay vs no-overlay)
// and network advertisement status.
//
// In no-overlay mode with outboundSNAT=enable, SNAT rules are created with exempted external IPs
// to prevent SNATing pod-to-pod traffic while still SNATing pod-to-external traffic.
//
// Parameters:
//   - nodeName: Node name to create SNAT rules for
//   - podIPs: Pod IP addresses to create SNAT rules for
//
// Returns:
//   - []ovsdb.Operation: SNAT operations for the pod
//   - error: Any error encountered during operation creation
func (oc *DefaultNetworkController) AddPodSNATOps(
	nodeName string,
	podIPs []*net.IPNet,
) ([]ovsdb.Operation, error) {
	var v4UUID, v6UUID string
	// Get external IPs for the node's gateway router
	extIPs, err := getExternalIPsGR(oc.watchFactory, nodeName)
	if err != nil {
		return nil, err
	}

	isNetworkAdvertised := oc.isPodNetworkAdvertisedAtNode(nodeName)
	gwRouterName := oc.GetNetworkScopedGWRouterName(nodeName)
	var clusterNodeIPsAddrSetDbIDs *libovsdbops.DbObjectIDs

	if util.IsNoOverlaySNATExemptionNeeded(oc.GetNetInfo()) {
		// Get the no-overlay SNAT exemption address set UUIDs
		v4UUID, v6UUID, err = getNoOverlaySNATExemptionAsUUID(oc.addressSetFactory, oc.GetNetInfo(), oc.controllerName)
		if err != nil {
			return nil, fmt.Errorf("failed to get no-overlay SNAT exemption address set UUID: %w", err)
		}
	} else if isNetworkAdvertised {
		clusterNodeIPsAddrSetDbIDs, err = oc.addressSetManager.EnsureClusterNodeIPsAddressSet(addresssetmanager.ClusterNodeIPsRouteAdvertisementsBackRef)
		if err != nil {
			return nil, fmt.Errorf("failed to ensure cluster node IP address set for route advertisements: %w", err)
		}
	}

	var ops []ovsdb.Operation
	// Handle each pod IP individually since each IP family needs its own SNAT match
	for _, podIP := range podIPs {
		// Determine the IP family and appropriate exempted address set
		var exemptedExtIPs string
		var ipFamily utilnet.IPFamily
		if utilnet.IsIPv6CIDR(podIP) {
			ipFamily = utilnet.IPv6
			exemptedExtIPs = v6UUID
		} else {
			ipFamily = utilnet.IPv4
			exemptedExtIPs = v4UUID
		}

		snatMatch, err := GetNetworkScopedClusterSubnetSNATMatch(oc.nbClient, oc.GetNetInfo(), nodeName, isNetworkAdvertised, ipFamily, clusterNodeIPsAddrSetDbIDs)
		if err != nil {
			return nil, fmt.Errorf("failed to get SNAT match for node %s for network %s: %w", nodeName, oc.GetNetInfo().GetNetworkName(), err)
		}
		ops, err = addOrUpdatePodSNATOps(oc.nbClient, gwRouterName, extIPs, []*net.IPNet{podIP}, snatMatch, exemptedExtIPs, ops)
		if err != nil {
			return nil, err
		}
	}

	return ops, nil
}

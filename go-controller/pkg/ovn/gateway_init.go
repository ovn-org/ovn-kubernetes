package ovn

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
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
	}
	logicalRouterExternalIDs := map[string]string{
		"physical_ip":  physicalIPs[0],
		"physical_ips": strings.Join(physicalIPs, ","),
	}
	// Local gateway mode does not need SNAT or routes on GR because GR is only used for multiple external gws
	// without SNAT. For normal N/S traffic, ingress/egress is mp0 on node switches
	if config.Gateway.Mode != config.GatewayModeLocal {
		// When there are multiple gateway routers (which would be the likely
		// default for any sane deployment), we need to SNAT traffic
		// heading to the logical space with the Gateway router's IP so that
		// return traffic comes back to the same gateway router.
		logicalRouterOptions["lb_force_snat_ip"] = "router_ip"
		logicalRouterOptions["snat-ct-zone"] = "0"
	}

	logicalRouter := nbdb.LogicalRouter{
		Name:        gatewayRouter,
		Options:     logicalRouterOptions,
		ExternalIDs: logicalRouterExternalIDs,
	}
	opModels := []util.OperationModel{
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
			OnModelUpdates: []interface{}{
				&logicalRouter.Options,
				&logicalRouter.ExternalIDs,
			},
			ExistingResult: &[]nbdb.LogicalRouter{},
		},
	}

	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to create logical router %v, err: %v", gatewayRouter, err)
	}

	gwSwitchPort := types.JoinSwitchToGWRouterPrefix + gatewayRouter
	gwSwitchPortUUID := util.GenerateNamedUUID()
	gwRouterPort := types.GWRouterToJoinSwitchPrefix + gatewayRouter
	gwRouterPortUUID := util.GenerateNamedUUID()

	logicalSwitch := nbdb.LogicalSwitch{}
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      gwSwitchPort,
		UUID:      gwSwitchPortUUID,
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			"router-port": gwRouterPort,
		},
	}

	logicalSwitchPortRes := []nbdb.LogicalSwitchPort{}
	opModels = []util.OperationModel{
		{
			Model:          &logicalSwitchPort,
			ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == gwSwitchPort },
			ExistingResult: &logicalSwitchPortRes,
		},
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == types.OVNJoinSwitch },
			OnModelMutations: func() []model.Mutation {
				if len(logicalSwitchPortRes) > 0 {
					return nil
				}
				return []model.Mutation{
					{
						Field:   &logicalSwitch.Ports,
						Mutator: ovsdb.MutateOperationInsert,
						Value:   []string{gwSwitchPortUUID},
					},
				}
			},
			ExistingResult: &[]nbdb.LogicalSwitch{},
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
		UUID:     gwRouterPortUUID,
		MAC:      gwLRPMAC.String(),
		Networks: gwLRPNetworks,
	}
	logicalRouterPortRes := []nbdb.LogicalRouterPort{}
	opModels = []util.OperationModel{
		{
			Model:          &logicalRouterPort,
			ModelPredicate: func(lrp *nbdb.LogicalRouterPort) bool { return lrp.Name == gwRouterPort },
			OnModelUpdates: []interface{}{
				&logicalRouterPort.MAC,
				&logicalRouterPort.Networks,
			},
			ExistingResult: &logicalRouterPortRes,
		},
		{

			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
			OnModelMutations: func() []model.Mutation {
				if len(logicalRouterPortRes) > 0 {
					return nil
				}
				return []model.Mutation{
					{
						Field:   &logicalRouter.Ports,
						Mutator: ovsdb.MutateOperationInsert,
						Value:   []string{gwRouterPortUUID},
					},
				}
			},
			ExistingResult: &[]nbdb.LogicalRouter{},
		},
	}

	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical router port %q for gateway router %s, err: %v", gwRouterPort, gatewayRouter, err)
	}

	for i, entry := range clusterIPSubnet {
		drLRPIfAddr, err := util.MatchIPNetFamily(utilnet.IsIPv6CIDR(entry), drLRPIfAddrs)
		if err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with distributed "+
				"router as the nexthop: %v",
				gatewayRouter, err)
		}

		namedEntry := fmt.Sprintf("entry%v", i)
		logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
			UUID:     namedEntry,
			IPPrefix: entry.String(),
			Nexthop:  drLRPIfAddr.IP.String(),
		}

		tmpRouters := []nbdb.LogicalRouter{}
		oc.nbClient.WhereCache(func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter }).List(&tmpRouters)
		if len(tmpRouters) != 1 {
			return fmt.Errorf("unable to retrieve logical router: %s, found: %+v", gatewayRouter, tmpRouters)
		}

		logicalRouterStaticRouteRes := []nbdb.LogicalRouterStaticRoute{}
		opModels = []util.OperationModel{
			{
				Model: &logicalRouterStaticRoute,
				ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
					return lrsr.IPPrefix == entry.String() && lrsr.Nexthop == drLRPIfAddr.IP.String() && util.SliceHasStringItem(tmpRouters[0].StaticRoutes, lrsr.UUID)
				},
				OnModelUpdates: []interface{}{
					&logicalRouterStaticRoute.IPPrefix,
					&logicalRouterStaticRoute.Nexthop,
				},
				ExistingResult: &logicalRouterStaticRouteRes,
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
				OnModelMutations: func() []model.Mutation {
					if len(logicalRouterStaticRouteRes) > 0 {
						return nil
					}
					return []model.Mutation{
						{
							Field:   &logicalRouter.StaticRoutes,
							Mutator: ovsdb.MutateOperationInsert,
							Value:   []string{namedEntry},
						},
					}
				},
				ExistingResult: &[]nbdb.LogicalRouter{},
			},
		}
		if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with distributed router as the nexthop, err: %v", gatewayRouter, err)
		}
	}

	k8sNSLbTCP, k8sNSLbUDP, k8sNSLbSCTP, err := getGatewayLoadBalancers(gatewayRouter)
	if err != nil {
		return err
	}
	gatewayProtoLBMap := map[kapi.Protocol]string{
		kapi.ProtocolTCP:  k8sNSLbTCP,
		kapi.ProtocolUDP:  k8sNSLbUDP,
		kapi.ProtocolSCTP: k8sNSLbSCTP,
	}
	workerK8sNSLbTCP, workerK8sNSLbUDP, workerK8sNSLbSCTP, err := loadbalancer.GetWorkerLoadBalancers(nodeName)
	if err != nil {
		return err
	}
	workerProtoLBMap := map[kapi.Protocol]string{
		kapi.ProtocolTCP:  workerK8sNSLbTCP,
		kapi.ProtocolUDP:  workerK8sNSLbUDP,
		kapi.ProtocolSCTP: workerK8sNSLbSCTP,
	}
	enabledProtos := []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolUDP}
	if sctpSupport {
		enabledProtos = append(enabledProtos, kapi.ProtocolSCTP)
	}

	if l3GatewayConfig.NodePortEnable {
		// Create 3 load-balancers for north-south traffic for each gateway
		// router: UDP, TCP, SCTP
		for _, proto := range enabledProtos {
			if gatewayProtoLBMap[proto] == "" {
				opModels = []util.OperationModel{
					{
						Model: &nbdb.LoadBalancer{
							ExternalIDs: map[string]string{
								fmt.Sprintf("%s_lb_gateway_router", proto): gatewayRouter,
							},
							Protocol: []string{strings.ToLower(string(proto))},
						},
						ModelPredicate: func(lb *nbdb.LoadBalancer) bool {
							return lb.ExternalIDs[fmt.Sprintf("%s_lb_gateway_router", proto)] == gatewayRouter && util.SliceHasStringItem(lb.Protocol, strings.ToLower(string(proto)))
						},
						ExistingResult: &[]nbdb.LoadBalancer{},
					},
				}
				res, err := oc.modelClient.CreateOrUpdate(opModels...)
				if err != nil {
					return fmt.Errorf("failed to create load balancer for gateway router %s for protocol %s, err: %v", gatewayRouter, proto, err)
				}
				gatewayProtoLBMap[proto] = res[0].UUID.GoUUID
			}
		}

		// Local gateway mode does not use GR for ingress node port traffic, it uses mp0 instead
		if config.Gateway.Mode != config.GatewayModeLocal {
			// Add north-south load-balancers to the gateway router.
			lbs := []string{gatewayProtoLBMap[kapi.ProtocolTCP], gatewayProtoLBMap[kapi.ProtocolUDP]}
			if sctpSupport {
				lbs = append(lbs, gatewayProtoLBMap[kapi.ProtocolSCTP])
			}

			logicalRouter := nbdb.LogicalRouter{
				LoadBalancer: lbs,
			}
			opModels = []util.OperationModel{
				{
					Model:          &logicalRouter,
					ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
					OnModelUpdates: []interface{}{
						&logicalRouter.LoadBalancer,
					},
					ExistingResult: &[]nbdb.LogicalRouter{},
				},
			}
			if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
				return fmt.Errorf("failed to set north-south load-balancers to the gateway router %s, err: %v", gatewayRouter, err)
			}
		} else {
			// Also add north-south load-balancers to local switches for pod -> nodePort traffic
			for _, proto := range enabledProtos {
				// TODO: think about if this is really right or not. Not sure about multiple conditions in the predicate
				opModels = []util.OperationModel{
					{
						Model: &logicalSwitch,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool {
							return !util.SliceHasStringItem(ls.LoadBalancer, gatewayProtoLBMap[proto]) && ls.Name == nodeName
						},
						OnModelMutations: func() []model.Mutation {
							return []model.Mutation{
								{
									Field:   &logicalSwitch.LoadBalancer,
									Mutator: ovsdb.MutateOperationInsert,
									Value:   []string{gatewayProtoLBMap[proto]},
								},
							}
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
				if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
					return fmt.Errorf("failed to add north-south load-balancer %s to the node switch %s, err: %v", gatewayProtoLBMap[proto], nodeName, err)
				}
			}
		}
	}

	// Create load balancers for workers (to be applied to GR and node switch)
	for _, proto := range enabledProtos {
		if workerProtoLBMap[proto] == "" {
			opModels = []util.OperationModel{
				{
					Model: &nbdb.LoadBalancer{
						ExternalIDs: map[string]string{
							fmt.Sprintf("%s-%s", types.WorkerLBPrefix, strings.ToLower(string(proto))): nodeName,
						},
						Protocol: []string{strings.ToLower(string(proto))},
					},
					ModelPredicate: func(lb *nbdb.LoadBalancer) bool {
						return lb.ExternalIDs[fmt.Sprintf("%s-%s", types.WorkerLBPrefix, strings.ToLower(string(proto)))] == nodeName && util.SliceHasStringItem(lb.Protocol, strings.ToLower(string(proto)))
					},
					ExistingResult: &[]nbdb.LoadBalancer{},
				},
			}
			res, err := oc.modelClient.CreateOrUpdate(opModels...)
			if err != nil {
				return fmt.Errorf("failed to create load balancer for worker node %s for protocol %s, err: %v", nodeName, proto, err)
			}
			workerProtoLBMap[proto] = res[0].UUID.GoUUID
		}
	}

	if config.Gateway.Mode != config.GatewayModeLocal {
		// Ensure north-south load-balancers are not on local switches for pod -> nodePort traffic
		// For upgrade path REMOVEME later
		for _, proto := range enabledProtos {
			opModels = []util.OperationModel{
				{
					Model: &logicalSwitch,
					ModelPredicate: func(ls *nbdb.LogicalSwitch) bool {
						return ls.Name == nodeName && util.SliceHasStringItem(ls.LoadBalancer, gatewayProtoLBMap[proto])
					},
					OnModelMutations: func() []model.Mutation {
						return []model.Mutation{
							{
								Field:   &logicalSwitch.LoadBalancer,
								Mutator: ovsdb.MutateOperationDelete,
								Value:   []string{gatewayProtoLBMap[proto]},
							},
						}
					},
					ExistingResult: &[]nbdb.LogicalSwitch{},
				},
			}
			if err := oc.modelClient.Delete(opModels...); err != nil {
				return fmt.Errorf("failed to remove north-south load-balancer %s to the node switch %s, err: %v", gatewayProtoLBMap[proto], nodeName, err)
			}
		}

		ls := nbdb.LogicalSwitch{}
		// Add per worker switch specific load-balancers
		for _, proto := range enabledProtos {
			opModels = []util.OperationModel{
				{
					Model: &ls,
					ModelPredicate: func(ls *nbdb.LogicalSwitch) bool {
						return ls.Name == nodeName && !util.SliceHasStringItem(ls.LoadBalancer, workerProtoLBMap[proto])
					},
					OnModelMutations: func() []model.Mutation {
						return []model.Mutation{
							{
								Field:   &ls.LoadBalancer,
								Mutator: ovsdb.MutateOperationInsert,
								Value:   []string{workerProtoLBMap[proto]},
							},
						}
					},
					ExistingResult: &[]nbdb.LogicalSwitch{},
				},
			}
			if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
				return fmt.Errorf("failed to add worker load-balancer %s to the node switch %s, err: %v", workerProtoLBMap[proto], nodeName, err)
			}
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
	for i, nextHop := range l3GatewayConfig.NextHops {
		var allIPs string
		if utilnet.IsIPv6(nextHop) {
			allIPs = "::/0"
		} else {
			allIPs = "0.0.0.0/0"
		}

		id := fmt.Sprintf("staticroute%v", i)
		logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
			UUID:       id,
			IPPrefix:   allIPs,
			Nexthop:    nextHop.String(),
			OutputPort: []string{externalRouterPort},
		}
		logicalRouterStaticRouteRes := []nbdb.LogicalRouterStaticRoute{}
		opModels = []util.OperationModel{
			{
				Model: &logicalRouterStaticRoute,
				ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
					return util.SliceHasStringItem(lrsr.OutputPort, externalRouterPort) && lrsr.Nexthop == nextHop.String()
				},
				OnModelUpdates: []interface{}{
					&logicalRouterStaticRoute.Nexthop,
				},
				ExistingResult: &logicalRouterStaticRouteRes,
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
				OnModelMutations: func() []model.Mutation {
					if len(logicalRouterStaticRouteRes) > 0 {
						return nil
					}
					return []model.Mutation{
						{
							Field:   &logicalRouter.StaticRoutes,
							Mutator: ovsdb.MutateOperationInsert,
							Value:   []string{id},
						},
					}
				},
				ExistingResult: &[]nbdb.LogicalRouter{},
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
	for i, gwLRPIP := range gwLRPIPs {
		id := fmt.Sprintf("staticroute%v2", i)
		logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
			UUID:     id,
			IPPrefix: gwLRPIP.String(),
			Nexthop:  gwLRPIP.String(),
		}
		logicalRouterStaticRouteRes := []nbdb.LogicalRouterStaticRoute{}
		opModels = []util.OperationModel{
			{
				Model: &logicalRouterStaticRoute,
				ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
					return lrsr.Nexthop == gwLRPIP.String() && lrsr.IPPrefix == gwLRPIP.String()
				},
				OnModelUpdates: []interface{}{
					&logicalRouterStaticRoute.Nexthop,
					&logicalRouterStaticRoute.IPPrefix,
				},
				ExistingResult: &logicalRouterStaticRouteRes,
			},
			{
				Model:          &logicalRouter,
				ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
				OnModelMutations: func() []model.Mutation {
					if len(logicalRouterStaticRouteRes) > 0 {
						return nil
					}
					return []model.Mutation{
						{
							Field:   &logicalRouter.StaticRoutes,
							Mutator: ovsdb.MutateOperationInsert,
							Value:   []string{id},
						},
					}
				},
				ExistingResult: &[]nbdb.LogicalRouter{},
			},
		}
		if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("failed to add a static route in GR %s with physical gateway as the default next hop, err: %v", gatewayRouter, err)
		}
	}

	// Add source IP address based routes in distributed router
	// for this gateway router.
	for i, hostSubnet := range hostSubnets {
		gwLRPIP, err := util.MatchIPFamily(utilnet.IsIPv6CIDR(hostSubnet), gwLRPIPs)
		if err != nil {
			return fmt.Errorf("failed to add source IP address based "+
				"routes in distributed router %s: %v",
				types.OVNClusterRouter, err)
		}

		if config.Gateway.Mode != config.GatewayModeLocal {
			id := fmt.Sprintf("staticroute%v3", i)
			logicalRouterStaticRoute := nbdb.LogicalRouterStaticRoute{
				UUID:     id,
				Policy:   []string{"src-ip"},
				IPPrefix: hostSubnet.String(),
				Nexthop:  gwLRPIP[0].String(),
			}
			logicalRouterStaticRouteRes := []nbdb.LogicalRouterStaticRoute{}
			opModels = []util.OperationModel{
				{
					Model: &logicalRouterStaticRoute,
					ModelPredicate: func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
						return lrsr.Nexthop == gwLRPIP[0].String() && lrsr.IPPrefix == hostSubnet.String()
					},
					OnModelUpdates: []interface{}{
						&logicalRouterStaticRoute.Nexthop,
						&logicalRouterStaticRoute.IPPrefix,
					},
					ExistingResult: &logicalRouterStaticRouteRes,
				},
				{
					Model:          &logicalRouter,
					ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
					OnModelMutations: func() []model.Mutation {
						if len(logicalRouterStaticRouteRes) > 0 {
							return nil
						}
						return []model.Mutation{
							{
								Field:   &logicalRouter.StaticRoutes,
								Mutator: ovsdb.MutateOperationInsert,
								Value:   []string{id},
							},
						}
					},
					ExistingResult: &[]nbdb.LogicalRouter{},
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
			if err := util.UpdateRouterSNAT(gatewayRouter, externalIP[0], entry); err != nil {
				return fmt.Errorf("failed to update NAT entry for pod subnet: %s, GR: %s, error: %v",
					entry.String(), gatewayRouter, err)
			}
		}
	} else {
		// ensure we do not have any leftover SNAT entries after an upgrade
		for _, logicalSubnet := range clusterIPSubnet {
			nats := []nbdb.NAT{}
			opModels = []util.OperationModel{
				{
					ModelPredicate: func(nat *nbdb.NAT) bool {
						return nat.Type == "snat" && nat.LogicalIP == logicalSubnet.String()
					},
					ExistingResult: &nats,
				},
				{
					Model:          &logicalRouter,
					ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
					OnModelMutations: func() []model.Mutation {
						if len(nats) == 1 {
							return []model.Mutation{
								{
									Field:   &logicalRouter.Nat,
									Mutator: ovsdb.MutateOperationDelete,
									Value:   []string{nats[0].UUID},
								},
							}
						}
						return nil
					},
					ExistingResult: &[]nbdb.LogicalRouter{},
				},
			}
			if err := oc.modelClient.Delete(opModels...); err != nil {
				return fmt.Errorf("failed to delete GW SNAT rule for pod on router %s, for subnet: %s, err: %v", gatewayRouter, logicalSubnet, err)
			}
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
	namedUUID := util.GenerateNamedUUID()
	dgpMac := util.IPAddrToHWAddr(net.ParseIP(types.V4NodeLocalDistributedGWPortIP)).String()
	dgpNetworkV4 := fmt.Sprintf("%s/%d", types.V4NodeLocalDistributedGWPortIP, types.V4NodeLocalNATSubnetPrefix)
	dgpNetworkV6 := fmt.Sprintf("%s/%d", types.V6NodeLocalDistributedGWPortIP, types.V6NodeLocalNATSubnetPrefix)

	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterPort := nbdb.LogicalRouterPort{
		UUID:     namedUUID,
		Name:     dgpName,
		MAC:      dgpMac,
		Networks: []string{dgpNetworkV4, dgpNetworkV6},
	}
	result := []nbdb.LogicalRouterPort{}
	opModels := []util.OperationModel{
		{
			Model: &logicalRouterPort,
			ModelPredicate: func(lrp *nbdb.LogicalRouterPort) bool {
				return lrp.Name == dgpName
			},
			OnModelUpdates: []interface{}{
				&logicalRouterPort.MAC,
				&logicalRouterPort.Networks,
			},
			ExistingResult: &result,
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: func() []model.Mutation {
				if len(result) > 0 {
					return nil
				}
				return []model.Mutation{
					{
						Field:   &logicalRouter.Ports,
						Mutator: ovsdb.MutateOperationInsert,
						Value:   []string{namedUUID},
					},
				}
			},
			ExistingResult: &[]nbdb.LogicalRouter{},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("unable to add distributed GW port: %s to logical router: %s, err: %v", dgpName, types.OVNClusterRouter, err)
	}

	namedUUID = "gw"
	gatewayChassis := nbdb.GatewayChassis{
		ChassisName: masterChassisID,
		ExternalIDs: map[string]string{
			"dpg_name": dgpName,
		},
		Name:     fmt.Sprintf("%s_%s", dgpName, masterChassisID),
		Priority: 100,
		UUID:     namedUUID,
	}
	chassisResult := []nbdb.GatewayChassis{}
	opModels = []util.OperationModel{
		{
			Model:          &gatewayChassis,
			ModelPredicate: func(g *nbdb.GatewayChassis) bool { return g.ChassisName == masterChassisID },
			OnModelUpdates: []interface{}{
				&gatewayChassis.ChassisName,
				&gatewayChassis.Name,
			},
			ExistingResult: &chassisResult,
		},
		{
			Model:          &logicalRouterPort,
			ModelPredicate: func(lrp *nbdb.LogicalRouterPort) bool { return lrp.Name == dgpName },
			OnModelMutations: func() []model.Mutation {
				if len(chassisResult) > 0 {
					return nil
				}
				return []model.Mutation{
					{
						Field:   &logicalRouterPort.GatewayChassis,
						Mutator: ovsdb.MutateOperationInsert,
						Value:   []string{namedUUID},
					},
				}
			},
			ExistingResult: &[]nbdb.LogicalRouterPort{},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("unable to add gateway chassis: %s to logical router port: %s, err: %v", masterChassisID, dgpName, err)
	}

	// connect the distributed gateway port to logical switch configured with localnet port
	logicalSwitch := nbdb.LogicalSwitch{
		Name: types.NodeLocalSwitch,
	}
	opModels = []util.OperationModel{
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == types.NodeLocalSwitch },
			ExistingResult: &[]nbdb.LogicalSwitch{},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("unable to create the logical switch for the localnet port, err: %v", err)
	}

	// add localnet port to the logical switch
	lclNetPortname := "lnet-" + types.NodeLocalSwitch
	namedUUID = util.GenerateNamedUUID()
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      lclNetPortname,
		UUID:      namedUUID,
		Addresses: []string{"unknown"},
		Type:      "localnet",
		Options: map[string]string{
			"network_name": types.LocalNetworkName,
		},
	}
	logicalSwitchPortRes := []nbdb.LogicalSwitchPort{}
	opModels = []util.OperationModel{
		{
			Model:          &logicalSwitchPort,
			ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == lclNetPortname },
			ExistingResult: &logicalSwitchPortRes,
		},
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == types.NodeLocalSwitch },
			OnModelMutations: func() []model.Mutation {
				if len(logicalSwitchPortRes) > 0 {
					return nil
				}
				return []model.Mutation{
					{
						Field:   &logicalSwitch.Ports,
						Mutator: ovsdb.MutateOperationInsert,
						Value:   []string{namedUUID},
					},
				}
			},
			ExistingResult: &[]nbdb.LogicalSwitch{},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("unable to add the localnet port to the logical switch, err: %v", err)
	}

	// connect the switch to the distributed router
	lspName := types.SwitchToRouterPrefix + types.NodeLocalSwitch
	namedUUID = util.GenerateNamedUUID()
	logicalSwitchPort = nbdb.LogicalSwitchPort{
		Name:      lspName,
		UUID:      namedUUID,
		Addresses: []string{"router"},
		Type:      "router",
		Options: map[string]string{
			"nat-addresses": "router",
			"router-port":   dgpName,
		},
	}
	logicalSwitchPortRes = []nbdb.LogicalSwitchPort{}
	opModels = []util.OperationModel{
		{
			Model:          &logicalSwitchPort,
			ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == lspName },
			ExistingResult: &logicalSwitchPortRes,
		},
		{
			Model:          &logicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == types.NodeLocalSwitch },
			OnModelMutations: func() []model.Mutation {
				if len(logicalSwitchPortRes) > 0 {
					return nil
				}
				return []model.Mutation{
					{
						Field:   &logicalSwitch.Ports,
						Mutator: ovsdb.MutateOperationInsert,
						Value:   []string{namedUUID},
					},
				}
			},
			ExistingResult: &[]nbdb.LogicalSwitch{},
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
	if err := oc.sbClient.WhereCache(func(mb *sbdb.MACBinding) bool {
		return mb.LogicalPort == dgpName && mb.MAC == dnatSnatNextHopMac.String()
	}).List(&macResult); err != nil {
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
	opModels := []util.OperationModel{
		{
			Model:          &externalLogicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == externalSwitch },
			ExistingResult: &[]nbdb.LogicalSwitch{},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to create logical switch %s, err: %v", externalSwitch, err)
	}

	externalSwitchPortUUID := util.GenerateNamedUUID()
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
		UUID: externalSwitchPortUUID,
	}
	if vlanID != nil {
		externalLogicalSwitchPort.TagRequest = []int{int(*vlanID)}
	}

	logicalSwitchPortRes := []nbdb.LogicalSwitchPort{}
	opModels = []util.OperationModel{
		{
			Model:          &externalLogicalSwitchPort,
			ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == interfaceID },
			OnModelUpdates: []interface{}{
				&externalLogicalSwitchPort.Addresses,
				&externalLogicalSwitchPort.Type,
				&externalLogicalSwitchPort.TagRequest,
				&externalLogicalSwitchPort.Options,
			},
			ExistingResult: &logicalSwitchPortRes,
		},
		{
			Model:          &externalLogicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == externalSwitch },
			OnModelMutations: func() []model.Mutation {
				if len(logicalSwitchPortRes) > 0 {
					return nil
				}
				return []model.Mutation{
					{
						Field:   &externalLogicalSwitch.Ports,
						Mutator: ovsdb.MutateOperationInsert,
						Value:   []string{externalSwitchPortUUID},
					},
				}
			},
			ExistingResult: &[]nbdb.LogicalSwitch{},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical switch port: %s to switch %s, err: %v", interfaceID, externalSwitch, err)
	}

	// Connect GR to external_switch with mac address of external interface
	// and that IP address. In the case of `local` gateway mode, whenever ovnkube-node container
	// restarts a new br-local bridge will be created with a new `nicMacAddress`.
	externalRouterPort := types.GWRouterToExtSwitchPrefix + gatewayRouter
	externalRouterPortUUID := util.GenerateNamedUUID()

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
		UUID:     externalRouterPortUUID,
	}

	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterPortRes := []nbdb.LogicalRouterPort{}
	opModels = []util.OperationModel{
		{
			Model:          &externalLogicalRouterPort,
			ModelPredicate: func(lrp *nbdb.LogicalRouterPort) bool { return lrp.Name == externalRouterPort },
			OnModelUpdates: []interface{}{
				&externalLogicalRouterPort.MAC,
				&externalLogicalRouterPort.Networks,
			},
			ExistingResult: &logicalRouterPortRes,
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == gatewayRouter },
			OnModelMutations: func() []model.Mutation {
				if len(logicalRouterPortRes) > 0 {
					return nil
				}
				return []model.Mutation{
					{
						Field:   &logicalRouter.Ports,
						Mutator: ovsdb.MutateOperationInsert,
						Value:   []string{externalRouterPortUUID},
					},
				}
			},
			ExistingResult: &[]nbdb.LogicalRouter{},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add logical router port: %s to router %s, err: %v", externalRouterPort, gatewayRouter, err)
	}

	// Connect the external_switch to the router.
	externalSwitchPortToRouter := types.EXTSwitchToGWRouterPrefix + gatewayRouter
	externalSwitchPortToRouterUUID := util.GenerateNamedUUID()

	externalLogicalSwitchPortToRouter := nbdb.LogicalSwitchPort{
		Name: externalSwitchPortToRouter,
		UUID: externalSwitchPortToRouterUUID,
		Type: "router",
		Options: map[string]string{
			"router-port": externalRouterPort,
		},
		Addresses: []string{macAddress},
	}
	logicalSwitchPortRes = []nbdb.LogicalSwitchPort{}
	opModels = []util.OperationModel{
		{
			Model:          &externalLogicalSwitchPortToRouter,
			ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == externalSwitchPortToRouter },
			OnModelUpdates: []interface{}{
				&externalLogicalSwitchPortToRouter.Addresses,
				&externalLogicalSwitchPortToRouter.Type,
				&externalLogicalSwitchPortToRouter.Options,
			},
			ExistingResult: &logicalSwitchPortRes,
		},
		{
			Model:          &externalLogicalSwitch,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == externalSwitch },
			OnModelMutations: func() []model.Mutation {
				if len(logicalSwitchPortRes) > 0 {
					return nil
				}
				return []model.Mutation{
					{
						Field:   &externalLogicalSwitch.Ports,
						Mutator: ovsdb.MutateOperationInsert,
						Value:   []string{externalSwitchPortToRouterUUID},
					},
				}
			},
			ExistingResult: &[]nbdb.LogicalSwitch{},
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

	for _, hostIP := range append(otherHostAddrs, hostIfAddr.IP.String()) {
		// embed nodeName as comment so that it is easier to delete these rules later on.
		// logical router policy doesn't support external_ids to stash metadata
		matchStr := fmt.Sprintf(`inport == "%s%s" && %s.dst == %s /* %s */`,
			types.RouterToSwitchPrefix, nodeName, l3Prefix, hostIP, nodeName)
		if err := oc.syncPolicyBasedRoutes(nodeName, matchStr, types.NodeSubnetPolicyPriority, mgmtPortIP); err != nil {
			return fmt.Errorf("unable to sync node subnet policies, err: %v", err)
		}
	}

	if config.Gateway.Mode == config.GatewayModeLocal {

		// policy to allow host -> service -> hairpin back to host
		matchStr := fmt.Sprintf("%s.src == %s && %s.dst == %s /* %s */",
			l3Prefix, mgmtPortIP, l3Prefix, hostIfAddr.IP.String(), nodeName)
		if err := oc.syncPolicyBasedRoutes(nodeName, matchStr, types.MGMTPortPolicyPriority, natSubnetNextHop); err != nil {
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
		if err := oc.syncPolicyBasedRoutes(nodeName, matchStr, types.InterNodePolicyPriority, natSubnetNextHop); err != nil {
			return fmt.Errorf("unable to sync inter-node policies, err: %v", err)
		}
	} else if config.Gateway.Mode == config.GatewayModeShared {
		// if we are upgrading from Local to Shared gateway mode, we need to ensure the inter-node LRP is removed
		oc.removeLRPolicies(nodeName, []string{types.InterNodePolicyPriority})
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
// and removes stale policies for a node. It also adds new policies for a node at a specific priority.
// This is ugly (since the node is encoded as a comment in the match),
// but a necessary evil as any change to this would break upgrades and
// possible downgrades. We could make sure any upgrade encodes the node in
// the external_id, but since ovn-kubernetes isn't versioned, we won't ever
// know which version someone is running of this and when the switch to version
// N+2 is fully made.
// TODO: @aconstan - this function is insane, it's a really intricate and
// obfuscated way of just doing "create if exists or update".
func (oc *Controller) syncPolicyBasedRoutes(nodeName string, match, priority, nexthop string) error {
	intPriority, _ := strconv.Atoi(priority)
	namedUUID := util.GenerateNamedUUID()

	logicalRouter := nbdb.LogicalRouter{}
	logicalRouterPolicy := nbdb.LogicalRouterPolicy{
		Nexthops: []string{nexthop},
		Priority: intPriority,
		Match:    match,
		Action:   nbdb.LogicalRouterPolicyActionReroute,
		UUID:     namedUUID,
	}
	result := []nbdb.LogicalRouterPolicy{}
	opModels := []util.OperationModel{
		{
			Model: &logicalRouterPolicy,
			ModelPredicate: func(lrp *nbdb.LogicalRouterPolicy) bool {
				return strings.Contains(lrp.Match, fmt.Sprintf("%s ", nodeName)) && lrp.Priority == intPriority
			},
			OnModelUpdates: []interface{}{
				&logicalRouterPolicy.Match,
				&logicalRouterPolicy.Nexthops,
			},
			ExistingResult: &result,
		},
		{
			Model:          &logicalRouter,
			ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == types.OVNClusterRouter },
			OnModelMutations: func() []model.Mutation {
				if len(result) > 0 {
					return nil
				}
				return []model.Mutation{
					{
						Field:   &logicalRouter.Policies,
						Mutator: ovsdb.MutateOperationInsert,
						Value:   []string{namedUUID},
					},
				}
			},
			ExistingResult: &[]nbdb.LogicalRouter{},
		},
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to add policy route '%s' for host %q on %s "+
			"error: %v", match, nodeName, types.OVNClusterRouter, err)
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
	stdout, stderr, err := util.RunOVNNbctl("--if-exists", "lr-nat-del", types.OVNClusterRouter,
		"dnat_and_snat", externalIP.String())
	if err != nil {
		return fmt.Errorf("failed to delete dnat_and_snat entry for the management port on node %s, "+
			"stdout: %s, stderr: %q, error: %v", node.Name, stdout, stderr, err)
	}
	stdout, stderr, err = util.RunOVNNbctl("lr-nat-add", types.OVNClusterRouter, "dnat_and_snat",
		externalIP.String(), mgmtPortIfAddr.IP.String(), mgmtPortName, mgmtPortMAC)
	if err != nil {
		return fmt.Errorf("failed to add dnat_and_snat entry for the management port on node %s, "+
			"stdout: %s, stderr: %q, error: %v", node.Name, stdout, stderr, err)
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
	err = oc.kube.SetAnnotationsOnNode(node, nodeAnnotations)
	if err != nil {
		return fmt.Errorf("failed to set node local NAT IP annotation on node %s: %v",
			node.Name, err)
	}
	return nil
}

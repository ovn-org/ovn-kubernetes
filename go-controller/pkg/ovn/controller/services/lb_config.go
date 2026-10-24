// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/apis/core"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/unidling"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// magic string used in vips to indicate that the node's physical
// ips should be substituted in
const placeholderNodeIPs = "node"

// lbConfig is the abstract desired load balancer configuration.
// vips and endpoints are mixed families.
type lbConfig struct {
	vips     []string        // just ip or the special value "node" for the node's physical IPs (i.e. NodePort)
	protocol corev1.Protocol // TCP, UDP, or SCTP
	inport   int32           // the incoming (virtual) port number

	clusterEndpoints util.LBEndpoints            // addresses of cluster-wide endpoints
	nodeEndpoints    map[string]util.LBEndpoints // node -> addresses of local endpoints

	// if true, then vips added on the router are in "local" mode
	// that means, skipSNAT, and remove any non-local endpoints.
	// (see below)
	externalTrafficLocal bool
	// if true, then vips added on the switch are in "local" mode
	// that means, remove any non-local endpoints.
	internalTrafficLocal bool
	// indicates if this LB is configuring service of type NodePort.
	hasNodePort bool

	// trafficDistribution: PreferSameZone/PreferClose support
	preferSameZone    bool
	zoneHintEndpoints map[string]util.LBEndpoints // zone name -> endpoints from Hints.ForZones

	// trafficDistribution: PreferSameNode support
	preferSameNode    bool
	nodeHintEndpoints map[string]util.LBEndpoints // node name -> endpoints from Hints.ForNodes
}

func makeNodeSwitchTargetIPs(node string, clusterEntry util.LBEndpointEntry, c *lbConfig) (targetIPsV4, targetIPsV6 []string, v4Changed, v6Changed bool) {
	targetIPsV4 = clusterEntry.V4IPs
	targetIPsV6 = clusterEntry.V6IPs

	if c.externalTrafficLocal || c.internalTrafficLocal {
		// For ExternalTrafficPolicy=Local, remove non-local endpoints from the router/switch targets
		// NOTE: on the switches, filtered eps are used only by masqueradeVIP
		// for InternalTrafficPolicy=Local, remove non-local endpoints from the switch targets only
		localIPsV4 := []string{}
		localIPsV6 := []string{}
		if localEndpoints, ok := c.nodeEndpoints[node]; ok {
			localEntry := localEndpoints.GetEntryByPort(clusterEntry.Port)
			localIPsV4 = localEntry.V4IPs
			localIPsV6 = localEntry.V6IPs
		}
		targetIPsV4 = localIPsV4
		targetIPsV6 = localIPsV6
	}

	// Local endpoints are a subset of cluster endpoints, so it is enough to compare their length
	v4Changed = len(targetIPsV4) != len(clusterEntry.V4IPs)
	v6Changed = len(targetIPsV6) != len(clusterEntry.V6IPs)

	return
}

func makeNodeRouterTargetIPs(node *nodeInfo, clusterEntry util.LBEndpointEntry, c *lbConfig, hostMasqueradeIPV4, hostMasqueradeIPV6 string) (targetIPsV4, targetIPsV6 []string, v4Changed, v6Changed bool) {
	targetIPsV4 = clusterEntry.V4IPs
	targetIPsV6 = clusterEntry.V6IPs

	if c.externalTrafficLocal {
		// For ExternalTrafficPolicy=Local, remove non-local endpoints from the router/switch targets
		// NOTE: on the switches, filtered eps are used only by masqueradeVIP
		localIPsV4 := []string{}
		localIPsV6 := []string{}
		if localEndpoints, ok := c.nodeEndpoints[node.name]; ok {
			localEntry := localEndpoints.GetEntryByPort(clusterEntry.Port)
			localIPsV4 = localEntry.V4IPs
			localIPsV6 = localEntry.V6IPs
		}
		targetIPsV4 = localIPsV4
		targetIPsV6 = localIPsV6
	}

	// TODO: For all scenarios the lbAddress should be set to hostAddressesStr but this is breaking CI needs more investigation
	lbAddresses := node.hostAddressesStr()
	if config.IsModeFull() {
		lbAddresses = node.l3gatewayAddressesStr()
	}

	// Any targets local to the node need to have a special
	// harpin IP added, but only for the router LB
	targetIPsV4, v4Updated := util.UpdateIPsSlice(targetIPsV4, lbAddresses, []string{hostMasqueradeIPV4})
	targetIPsV6, v6Updated := util.UpdateIPsSlice(targetIPsV6, lbAddresses, []string{hostMasqueradeIPV6})

	// Local endpoints are a subset of cluster endpoints, so it is enough to compare their length
	v4Changed = len(targetIPsV4) != len(clusterEntry.V4IPs) || v4Updated
	v6Changed = len(targetIPsV6) != len(clusterEntry.V6IPs) || v6Updated

	return
}

// just used for consistent ordering
var protos = []corev1.Protocol{
	corev1.ProtocolTCP,
	corev1.ProtocolUDP,
	corev1.ProtocolSCTP,
}

// buildServiceLBConfigs generates the abstract load balancer(s) configurations for each service. The abstract configurations
// are then expanded in buildClusterLBs and buildPerNodeLBs to the full list of OVN LBs desired.
//
// It creates three lists of configurations:
// - the per-node configs, which are load balancers that, for some reason must be expanded per-node. (see below for why)
// - the template configs, which are template load balancers that are similar across the whole cluster.
// - the cluster-wide configs, which are load balancers that can be the same across the whole cluster.
//
// For a "standard" ClusterIP service (possibly with ExternalIPS or external LoadBalancer Status IPs),
// a single cluster-wide LB will be created.
//
// Per-node LBs will be created for
// - services with NodePort set
// - services with host-network endpoints
// - services with ExternalTrafficPolicy=Local
// - services with InternalTrafficPolicy=Local
//
// Template LBs will be created for
//   - services with NodePort set but *without* ExternalTrafficPolicy=Local or
//     affinity timeout set.
func buildServiceLBConfigs(service *corev1.Service, endpointSlices []*discovery.EndpointSlice, nodeInfos []nodeInfo,
	useLBGroup, useTemplates bool, netInfo util.NetInfo) (perNodeConfigs, templateConfigs, clusterConfigs []lbConfig) {

	needsAffinityTimeout := hasSessionAffinityTimeOut(service)

	nodes := sets.New[string]()
	for _, n := range nodeInfos {
		nodes.Insert(n.name)
	}
	// get all the endpoints classified by port and by port,node
	needsLocalEndpoints := util.ServiceExternalTrafficPolicyLocal(service) || util.ServiceInternalTrafficPolicyLocal(service)
	portToClusterEndpoints, portToNodeToEndpoints, err := util.GetEndpointsForService(endpointSlices, service, nodes, true, needsLocalEndpoints)
	if err != nil {
		klog.Warningf("Failed to get endpoints for service %s/%s during LB config build: %v",
			service.Namespace, service.Name, err)
	}

	preferSameZone := util.ServiceHasTrafficDistributionPreferSameZone(service)
	preferSameNode := util.ServiceHasTrafficDistributionPreferSameNode(service)
	var portToZoneHintEndpoints util.PortToZoneToLBEndpoints
	var portToNodeHintEndpoints util.PortToNodeToLBEndpoints
	if preferSameZone {
		portToZoneHintEndpoints = util.GetZoneHintEndpointsForService(endpointSlices, service)
	}
	if preferSameNode {
		portToNodeHintEndpoints = util.GetNodeHintEndpointsForService(endpointSlices, service, nodes)
	}

	for _, svcPort := range service.Spec.Ports {
		svcPortKey := util.GetServicePortKey(svcPort.Protocol, svcPort.Name)
		clusterEndpoints := portToClusterEndpoints[svcPortKey]
		nodeEndpoints := portToNodeToEndpoints[svcPortKey]
		if nodeEndpoints == nil {
			nodeEndpoints = make(map[string]util.LBEndpoints)
		}
		// if ExternalTrafficPolicy or InternalTrafficPolicy is local, then we need to do things a bit differently
		externalTrafficLocal := util.ServiceExternalTrafficPolicyLocal(service)
		internalTrafficLocal := util.ServiceInternalTrafficPolicyLocal(service)

		// NodePort services get a per-node load balancer, but with the node's physical IP as the vip
		// Thus, the vip "node" will be expanded later.
		// This is NEVER influenced by InternalTrafficPolicy
		if svcPort.NodePort != 0 {
			nodePortLBConfig := lbConfig{
				protocol:             svcPort.Protocol,
				inport:               svcPort.NodePort,
				vips:                 []string{placeholderNodeIPs}, // shortcut for all-physical-ips
				clusterEndpoints:     clusterEndpoints,
				nodeEndpoints:        nodeEndpoints,
				externalTrafficLocal: externalTrafficLocal,
				internalTrafficLocal: false, // always false for non-ClusterIPs
				hasNodePort:          true,
			}
			// Only "plain" NodePort services (no ETP, no affinity timeout)
			// can use load balancer templates.
			if !useLBGroup || !useTemplates || externalTrafficLocal || needsAffinityTimeout {
				perNodeConfigs = append(perNodeConfigs, nodePortLBConfig)
			} else {
				templateConfigs = append(templateConfigs, nodePortLBConfig)
			}
		}

		// Build up list of vips and externalVips
		vips := util.GetClusterIPs(service)
		externalVips := util.GetExternalAndLBIPs(service)

		// if ETP=Local, then treat ExternalIPs and LoadBalancer IPs specially
		// otherwise, they're just cluster IPs
		// This is NEVER influenced by InternalTrafficPolicy
		if externalTrafficLocal && len(externalVips) > 0 {
			externalIPConfig := lbConfig{
				protocol:             svcPort.Protocol,
				inport:               svcPort.Port,
				vips:                 externalVips,
				clusterEndpoints:     clusterEndpoints,
				nodeEndpoints:        nodeEndpoints,
				externalTrafficLocal: true,
				internalTrafficLocal: false, // always false for non-ClusterIPs
				hasNodePort:          false,
			}
			perNodeConfigs = append(perNodeConfigs, externalIPConfig)
		} else {
			vips = append(vips, externalVips...)
		}

		// Build the clusterIP config
		// This is NEVER influenced by ExternalTrafficPolicy
		zoneHintEndpoints := portToZoneHintEndpoints[svcPortKey]
		nodeHintEndpoints := portToNodeHintEndpoints[svcPortKey]

		clusterIPConfig := lbConfig{
			protocol:             svcPort.Protocol,
			inport:               svcPort.Port,
			vips:                 vips,
			clusterEndpoints:     clusterEndpoints,
			nodeEndpoints:        nodeEndpoints,
			externalTrafficLocal: false, // always false for ClusterIPs
			internalTrafficLocal: internalTrafficLocal,
			hasNodePort:          false,
			preferSameZone:       preferSameZone,
			zoneHintEndpoints:    zoneHintEndpoints,
			preferSameNode:       preferSameNode,
			nodeHintEndpoints:    nodeHintEndpoints,
		}

		// Normally, the ClusterIP LB is global (on all node switches and routers),
		// unless any of the following are true:
		// - Any of the endpoints are host-network
		// - ETP=local service backed by non-local-host-networked endpoints
		// - trafficDistribution is set (PreferSameZone, PreferSameNode, or PreferClose)
		//
		// In that case, we need to create per-node LBs.
		ips := []string{}
		for _, ep := range clusterEndpoints {
			ips = append(ips, ep.V4IPs...)
			ips = append(ips, ep.V6IPs...)
		}
		if hasHostEndpoints(ips, netInfo) || internalTrafficLocal || preferSameZone || preferSameNode {
			perNodeConfigs = append(perNodeConfigs, clusterIPConfig)
		} else {
			clusterConfigs = append(clusterConfigs, clusterIPConfig)
		}
	}

	return
}

func makeLBNameForNetwork(service *corev1.Service, proto corev1.Protocol, scope string, netInfo util.NetInfo) string {
	return netInfo.GetNetworkScopedLoadBalancerName(makeLBName(service, proto, scope))
}

// makeLBName creates the load balancer name - used to minimize churn
func makeLBName(service *corev1.Service, proto corev1.Protocol, scope string) string {
	return fmt.Sprintf("Service_%s/%s_%s_%s",
		service.Namespace, service.Name,
		proto, scope)
}

// buildClusterLBs takes a list of lbConfigs and aggregates them
// in to one ovn LB per protocol.
//
// It takes a list of (proto:[vips]:port -> [endpoints]) configs and re-aggregates
// them to a list of (proto:[vip:port -> [endpoint:port]])
// This load balancer is attached to all node switches. In shared-GW mode, it is also on all routers
// The input netInfo is needed to get the right LB groups and network IDs for the specified network.
func buildClusterLBs(service *corev1.Service, configs []lbConfig, nodeInfos []nodeInfo, useLBGroup bool, netInfo util.NetInfo) []LB {
	var nodeSwitches []string
	var nodeRouters []string
	var groups []string
	if useLBGroup {
		nodeSwitches = make([]string, 0)
		nodeRouters = make([]string, 0)
		groups = []string{netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterLBGroupName)}
	} else {
		nodeSwitches = make([]string, 0, len(nodeInfos))
		nodeRouters = make([]string, 0, len(nodeInfos))
		groups = make([]string, 0)

		for _, node := range nodeInfos {
			nodeSwitches = append(nodeSwitches, node.switchName)
			// For shared gateway, add to the node's GWR as well.
			// The node may not have a gateway router - it might be waiting initialization, or
			// might have disabled GWR creation via the k8s.ovn.org/l3-gateway-config annotation
			if node.gatewayRouterName != "" {
				nodeRouters = append(nodeRouters, node.gatewayRouterName)
			}
		}
	}

	cbp := configsByProto(configs)

	out := []LB{}
	for _, proto := range protos {
		cfgs, ok := cbp[proto]
		if !ok {
			continue
		}
		lb := LB{
			Name:        makeLBNameForNetwork(service, proto, "cluster", netInfo),
			Protocol:    string(proto),
			ExternalIDs: getExternalIDsForLoadBalancer(service, netInfo),
			Opts:        lbOpts(service),

			Switches: nodeSwitches,
			Routers:  nodeRouters,
			Groups:   groups,
		}

		for _, config := range cfgs {
			if config.externalTrafficLocal {
				klog.Errorf("BUG: service %s/%s has routerLocalMode=true for cluster-wide lbConfig",
					service.Namespace, service.Name)
			}

			v4targets := []Addr{}
			v6targets := []Addr{}
			for _, entry := range config.clusterEndpoints {
				for _, targetIP := range entry.V4IPs {
					v4targets = append(v4targets, Addr{
						IP:   targetIP,
						Port: entry.Port,
					})
				}

				for _, targetIP := range entry.V6IPs {
					v6targets = append(v6targets, Addr{
						IP:   targetIP,
						Port: entry.Port,
					})
				}
			}

			rules := make([]LBRule, 0, len(config.vips))
			for _, vip := range config.vips {
				if vip == placeholderNodeIPs {
					klog.Errorf("BUG: service %s/%s has a \"node\" vip for a cluster-wide lbConfig",
						service.Namespace, service.Name)
					continue
				}
				targets := v4targets
				if utilnet.IsIPv6String(vip) {
					targets = v6targets
				}

				rules = append(rules, LBRule{
					Source: Addr{
						IP:   vip,
						Port: config.inport,
					},
					Targets: targets,
				})
			}
			lb.Rules = append(lb.Rules, rules...)
		}

		out = append(out, lb)
	}
	return out
}

// buildTemplateLBs takes a list of lbConfigs and expands them to one template
// LB per protocol (per address family).
//
// Template LBs are created for nodeport services and are attached to each
// node's gateway router + switch via load balancer groups.  Their vips and
// backends are OVN chassis template variables that expand to the chassis'
// node IP and set of backends.
//
// Note:
// NodePort services with ETP=local or affinity timeout set still need
// non-template per-node LBs.
//
// The input netInfo is needed to get the right LB groups and network IDs for the specified network.
func buildTemplateLBs(service *corev1.Service, configs []lbConfig, nodes []nodeInfo,
	nodeIPv4Templates, nodeIPv6Templates *NodeIPsTemplates, netInfo util.NetInfo) []LB {

	cbp := configsByProto(configs)
	eids := getExternalIDsForLoadBalancer(service, netInfo)
	out := make([]LB, 0, len(configs))

	for _, proto := range protos {
		configs, ok := cbp[proto]
		if !ok {
			continue
		}

		switchV4Rules := make([]LBRule, 0, len(configs))
		switchV6Rules := make([]LBRule, 0, len(configs))
		routerV4Rules := make([]LBRule, 0, len(configs))
		routerV6Rules := make([]LBRule, 0, len(configs))

		optsV4 := lbTemplateOpts(service, corev1.IPv4Protocol)
		optsV6 := lbTemplateOpts(service, corev1.IPv6Protocol)

		for _, cfg := range configs {
			switchV4TemplateTarget :=
				makeTemplate(
					makeLBTargetTemplateName(
						service, proto, cfg.inport,
						optsV4.AddressFamily, "node_switch_template", netInfo))
			switchV6TemplateTarget :=
				makeTemplate(
					makeLBTargetTemplateName(
						service, proto, cfg.inport,
						optsV6.AddressFamily, "node_switch_template", netInfo))

			routerV4TemplateTarget :=
				makeTemplate(
					makeLBTargetTemplateName(
						service, proto, cfg.inport,
						optsV4.AddressFamily, "node_router_template", netInfo))
			routerV6TemplateTarget :=
				makeTemplate(
					makeLBTargetTemplateName(
						service, proto, cfg.inport,
						optsV6.AddressFamily, "node_router_template", netInfo))

			// Phase 1: Accumulate template values and needsTemplate flags
			// across all target port numbers. When multiple target ports
			// coexist (e.g. during a rolling update), each port's targets
			// must be appended into the template rather than overwriting.
			switchV4TargetNeedsTemplate := false
			switchV6TargetNeedsTemplate := false
			routerV4TargetNeedsTemplate := false
			routerV6TargetNeedsTemplate := false
			allSharedV4Targets := []Addr{}
			allSharedV6Targets := []Addr{}

			for _, entry := range cfg.clusterEndpoints {
				allSharedV4Targets = append(allSharedV4Targets, joinHostsPort(entry.V4IPs, entry.Port)...)
				allSharedV6Targets = append(allSharedV6Targets, joinHostsPort(entry.V6IPs, entry.Port)...)

				for _, node := range nodes {
					switchV4TargetIPs, switchV6TargetIPs, v4Changed, v6Changed := makeNodeSwitchTargetIPs(node.name, entry, &cfg)
					if v4Changed {
						switchV4TargetNeedsTemplate = true
					}
					if v6Changed {
						switchV6TargetNeedsTemplate = true
					}

					routerV4TargetIPs, routerV6TargetIPs, v4Changed, v6Changed := makeNodeRouterTargetIPs(
						&node,
						entry,
						&cfg,
						config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String(),
						config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String())

					if v4Changed {
						routerV4TargetNeedsTemplate = true
					}
					if v6Changed {
						routerV6TargetNeedsTemplate = true
					}

					appendTemplateValue(switchV4TemplateTarget, node.chassisID,
						addrsToString(joinHostsPort(switchV4TargetIPs, entry.Port)))
					appendTemplateValue(switchV6TemplateTarget, node.chassisID,
						addrsToString(joinHostsPort(switchV6TargetIPs, entry.Port)))
					appendTemplateValue(routerV4TemplateTarget, node.chassisID,
						addrsToString(joinHostsPort(routerV4TargetIPs, entry.Port)))
					appendTemplateValue(routerV6TemplateTarget, node.chassisID,
						addrsToString(joinHostsPort(routerV6TargetIPs, entry.Port)))
				}
			}

			// Phase 2: Create rules using fully-populated template values.
			// If all targets have exactly the same IPs on all nodes there's
			// no need to use a template, just use the same list of explicit
			// targets on all nodes.
			sharedV4Targets := []Addr{}
			sharedV6Targets := []Addr{}
			if !switchV4TargetNeedsTemplate || !routerV4TargetNeedsTemplate {
				sharedV4Targets = allSharedV4Targets
			}
			if !switchV6TargetNeedsTemplate || !routerV6TargetNeedsTemplate {
				sharedV6Targets = allSharedV6Targets
			}

			for range cfg.vips {
				klog.V(5).Infof("buildTemplateLBs() service %s/%s adding rules for network=%s",
					service.Namespace, service.Name, netInfo.GetNetworkName())

				for _, nodeIPv4Template := range nodeIPv4Templates.AsTemplates() {

					if switchV4TargetNeedsTemplate {
						switchV4Rules = append(switchV4Rules, LBRule{
							Source:  Addr{Template: nodeIPv4Template, Port: cfg.inport},
							Targets: []Addr{{Template: switchV4TemplateTarget}},
						})
					} else {
						switchV4Rules = append(switchV4Rules, LBRule{
							Source:  Addr{Template: nodeIPv4Template, Port: cfg.inport},
							Targets: sharedV4Targets,
						})
					}

					if routerV4TargetNeedsTemplate {
						routerV4Rules = append(routerV4Rules, LBRule{
							Source:  Addr{Template: nodeIPv4Template, Port: cfg.inport},
							Targets: []Addr{{Template: routerV4TemplateTarget}},
						})
					} else {
						routerV4Rules = append(routerV4Rules, LBRule{
							Source:  Addr{Template: nodeIPv4Template, Port: cfg.inport},
							Targets: sharedV4Targets,
						})
					}
				}

				for _, nodeIPv6Template := range nodeIPv6Templates.AsTemplates() {

					if switchV6TargetNeedsTemplate {
						switchV6Rules = append(switchV6Rules, LBRule{
							Source:  Addr{Template: nodeIPv6Template, Port: cfg.inport},
							Targets: []Addr{{Template: switchV6TemplateTarget}},
						})
					} else {
						switchV6Rules = append(switchV6Rules, LBRule{
							Source:  Addr{Template: nodeIPv6Template, Port: cfg.inport},
							Targets: sharedV6Targets,
						})
					}

					if routerV6TargetNeedsTemplate {
						routerV6Rules = append(routerV6Rules, LBRule{
							Source:  Addr{Template: nodeIPv6Template, Port: cfg.inport},
							Targets: []Addr{{Template: routerV6TemplateTarget}},
						})
					} else {
						routerV6Rules = append(routerV6Rules, LBRule{
							Source:  Addr{Template: nodeIPv6Template, Port: cfg.inport},
							Targets: sharedV6Targets,
						})
					}
				}
			}
		}

		if nodeIPv4Templates.Len() > 0 {
			if len(switchV4Rules) > 0 {
				out = append(out, LB{
					Name:        makeLBNameForNetwork(service, proto, "node_switch_template_IPv4", netInfo),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        optsV4,
					Groups:      []string{netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterSwitchLBGroupName)},
					Rules:       switchV4Rules,
					Templates:   getTemplatesFromRulesTargets(switchV4Rules),
				})
			}
			if len(routerV4Rules) > 0 {
				out = append(out, LB{
					Name:        makeLBNameForNetwork(service, proto, "node_router_template_IPv4", netInfo),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        optsV4,
					Groups:      []string{netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterRouterLBGroupName)},
					Rules:       routerV4Rules,
					Templates:   getTemplatesFromRulesTargets(routerV4Rules),
				})
			}
		}

		if nodeIPv6Templates.Len() > 0 {
			if len(switchV6Rules) > 0 {
				out = append(out, LB{
					Name:        makeLBNameForNetwork(service, proto, "node_switch_template_IPv6", netInfo),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        optsV6,
					Groups:      []string{netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterSwitchLBGroupName)},
					Rules:       switchV6Rules,
					Templates:   getTemplatesFromRulesTargets(switchV6Rules),
				})
			}
			if len(routerV6Rules) > 0 {
				out = append(out, LB{
					Name:        makeLBNameForNetwork(service, proto, "node_router_template_IPv6", netInfo),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        optsV6,
					Groups:      []string{netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterRouterLBGroupName)},
					Rules:       routerV6Rules,
					Templates:   getTemplatesFromRulesTargets(routerV6Rules),
				})
			}
		}
	}

	merged := mergeLBs(out)
	if len(merged) != len(out) {
		klog.V(5).Infof("Service %s/%s merged %d LBs to %d for network=%s",
			service.Namespace, service.Name,
			len(out), len(merged), netInfo.GetNetworkName())
	}

	return merged
}

// buildPerNodeLBs takes a list of lbConfigs and expands them to one LB per protocol per node
//
// Per-node lbs are created for
// - clusterip services with host-network endpoints, which are attached to each node's gateway router + switch
// - nodeport services are attached to each node's gateway router + switch, vips are the node's physical IPs (except if etp=local+ovnk backend pods)
// - any services with host-network endpoints
// - services with external IPs / LoadBalancer Status IPs
//
// HOWEVER, we need to replace, on each nodes gateway router only, any host-network endpoints with a special loopback address
// see https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/design/host_to_services_OpenFlow.md
// This is for host -> serviceip -> host hairpin
//
// For ExternalTrafficPolicy=local, all "External" IPs (NodePort, ExternalIPs, Loadbalancer Status) have:
// - targets filtered to only local targets
// - SkipSNAT enabled
// - NodePort LB on the switch will have masqueradeIP as the vip to handle etp=local for LGW case.
// This results in the creation of an additional load balancer on the GatewayRouters and NodeSwitches.
//
// The input netInfo is needed to get the right network IDs for the specified network.
func buildPerNodeLBs(service *corev1.Service, configs []lbConfig, nodes []nodeInfo, netInfo util.NetInfo) []LB {
	cbp := configsByProto(configs)
	eids := getExternalIDsForLoadBalancer(service, netInfo)

	out := make([]LB, 0, len(nodes)*len(configs))

	// output is one LB per node per protocol with one rule per vip
	for _, node := range nodes {
		for _, proto := range protos {
			configs, ok := cbp[proto]
			if !ok {
				continue
			}

			// attach to router & switch,
			// rules may or may not be different
			routerRules := make([]LBRule, 0, len(configs))
			noSNATRouterRules := make([]LBRule, 0)
			switchRules := make([]LBRule, 0, len(configs))

			for _, cfg := range configs {

				// Accumulate targets across all port numbers. When
				// clusterEndpoints is empty (e.g. pod deleted during
				// cycling), these remain empty and we still create
				// rules with empty targets so OVN cleans up conntrack.
				switchV4targets := []Addr{}
				switchV6targets := []Addr{}
				routerV4targets := []Addr{}
				routerV6targets := []Addr{}
				switchV4LocalTargets := []Addr{}
				switchV6LocalTargets := []Addr{}

				for _, entry := range cfg.clusterEndpoints {
					switchV4TargetIPs, switchV6TargetIPs, _, _ := makeNodeSwitchTargetIPs(node.name, entry, &cfg)

					routerV4TargetIPs, routerV6TargetIPs, _, _ := makeNodeRouterTargetIPs(
						&node,
						entry,
						&cfg,
						config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String(),
						config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String())

					routerV4targets = append(routerV4targets, joinHostsPort(routerV4TargetIPs, entry.Port)...)
					routerV6targets = append(routerV6targets, joinHostsPort(routerV6TargetIPs, entry.Port)...)

					if cfg.preferSameZone && node.topologyZone != "" {
						if zoneEps, ok := cfg.zoneHintEndpoints[node.topologyZone]; ok {
							zoneEntry := zoneEps.GetEntryByPort(entry.Port)
							if len(zoneEntry.V4IPs) > 0 {
								switchV4targets = append(switchV4targets, joinHostsPort(zoneEntry.V4IPs, entry.Port)...)
								switchV6targets = append(switchV6targets, joinHostsPort(zoneEntry.V6IPs, entry.Port)...)
							} else {
								switchV4targets = append(switchV4targets, joinHostsPort(entry.V4IPs, entry.Port)...)
								switchV6targets = append(switchV6targets, joinHostsPort(entry.V6IPs, entry.Port)...)
							}
						} else {
							switchV4targets = append(switchV4targets, joinHostsPort(entry.V4IPs, entry.Port)...)
							switchV6targets = append(switchV6targets, joinHostsPort(entry.V6IPs, entry.Port)...)
						}
					} else if cfg.preferSameNode {
						if nodeEps, ok := cfg.nodeHintEndpoints[node.name]; ok {
							nodeEntry := nodeEps.GetEntryByPort(entry.Port)
							if len(nodeEntry.V4IPs) > 0 {
								switchV4targets = append(switchV4targets, joinHostsPort(nodeEntry.V4IPs, entry.Port)...)
								switchV6targets = append(switchV6targets, joinHostsPort(nodeEntry.V6IPs, entry.Port)...)
							} else {
								switchV4targets = append(switchV4targets, joinHostsPort(entry.V4IPs, entry.Port)...)
								switchV6targets = append(switchV6targets, joinHostsPort(entry.V6IPs, entry.Port)...)
							}
						} else {
							switchV4targets = append(switchV4targets, joinHostsPort(entry.V4IPs, entry.Port)...)
							switchV6targets = append(switchV6targets, joinHostsPort(entry.V6IPs, entry.Port)...)
						}
					} else {
						switchV4targets = append(switchV4targets, joinHostsPort(entry.V4IPs, entry.Port)...)
						switchV6targets = append(switchV6targets, joinHostsPort(entry.V6IPs, entry.Port)...)
					}

					switchV4LocalTargets = append(switchV4LocalTargets, joinHostsPort(switchV4TargetIPs, entry.Port)...)
					switchV6LocalTargets = append(switchV6LocalTargets, joinHostsPort(switchV6TargetIPs, entry.Port)...)
				}

				// Substitute the special vip "node" for the node's physical ips
				// This is used for nodeport
				vips := make([]string, 0, len(cfg.vips))
				for _, vip := range cfg.vips {
					if vip == placeholderNodeIPs {
						if !node.nodePortDisabled {
							vips = append(vips, node.hostAddressesStr()...)
						}
					} else {
						vips = append(vips, vip)
					}
				}

				for _, vip := range vips {
					isv6 := utilnet.IsIPv6String((vip))
					// build switch rules
					targets := switchV4targets
					if isv6 {
						targets = switchV6targets
					}

					if cfg.externalTrafficLocal && cfg.hasNodePort {
						// add special masqueradeIP as a vip if its nodePort svc with ETP=local
						mvip := config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String()
						targetsETP := switchV4LocalTargets
						if isv6 {
							mvip = config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String()
							targetsETP = switchV6LocalTargets
						}
						switchRules = append(switchRules, LBRule{
							Source:  Addr{IP: mvip, Port: cfg.inport},
							Targets: targetsETP,
						})
					}
					if cfg.internalTrafficLocal && util.IsClusterIP(vip) { // ITP only applicable to CIP
						targetsITP := switchV4LocalTargets
						if isv6 {
							targetsITP = switchV6LocalTargets
						}
						switchRules = append(switchRules, LBRule{
							Source:  Addr{IP: vip, Port: cfg.inport},
							Targets: targetsITP,
						})
					} else {
						switchRules = append(switchRules, LBRule{
							Source:  Addr{IP: vip, Port: cfg.inport},
							Targets: targets,
						})
					}

					// There is also a per-router rule
					// with targets that *may* be different
					targets = routerV4targets
					if isv6 {
						targets = routerV6targets
					}
					rule := LBRule{
						Source:  Addr{IP: vip, Port: cfg.inport},
						Targets: targets,
					}

					// in other words, is this ExternalTrafficPolicy=local?
					// if so, this gets a separate load balancer with SNAT disabled
					// (but there's no need to do this if the list of targets is empty)
					if cfg.externalTrafficLocal && len(targets) > 0 {
						noSNATRouterRules = append(noSNATRouterRules, rule)
					} else {
						routerRules = append(routerRules, rule)
					}
				}
			}

			// If switch and router rules are identical, coalesce
			if reflect.DeepEqual(switchRules, routerRules) && len(switchRules) > 0 && node.gatewayRouterName != "" {
				out = append(out, LB{
					Name:        makeLBNameForNetwork(service, proto, "node_router+switch_"+node.name, netInfo),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        lbOpts(service),
					Routers:     []string{node.gatewayRouterName},
					Switches:    []string{node.switchName},
					Rules:       routerRules,
				})
			} else {
				if len(routerRules) > 0 && node.gatewayRouterName != "" {
					out = append(out, LB{
						Name:        makeLBNameForNetwork(service, proto, "node_router_"+node.name, netInfo),
						Protocol:    string(proto),
						ExternalIDs: eids,
						Opts:        lbOpts(service),
						Routers:     []string{node.gatewayRouterName},
						Rules:       routerRules,
					})
				}
				if len(noSNATRouterRules) > 0 && node.gatewayRouterName != "" {
					lb := LB{
						Name:        makeLBNameForNetwork(service, proto, "node_local_router_"+node.name, netInfo),
						Protocol:    string(proto),
						ExternalIDs: eids,
						Opts:        lbOpts(service),
						Routers:     []string{node.gatewayRouterName},
						Rules:       noSNATRouterRules,
					}
					lb.Opts.SkipSNAT = true
					out = append(out, lb)
				}

				if len(switchRules) > 0 {
					out = append(out, LB{
						Name:        makeLBNameForNetwork(service, proto, "node_switch_"+node.name, netInfo),
						Protocol:    string(proto),
						ExternalIDs: eids,
						Opts:        lbOpts(service),
						Switches:    []string{node.switchName},
						Rules:       switchRules,
					})
				}
			}
		}
	}

	merged := mergeLBs(out)
	if len(merged) != len(out) {
		klog.V(5).Infof("Service %s/%s merged %d LBs to %d for network=%s",
			service.Namespace, service.Name,
			len(out), len(merged), netInfo.GetNetworkName())
	}

	return merged
}

// configsByProto buckets a list of configs by protocol (tcp, udp, sctp)
func configsByProto(configs []lbConfig) map[corev1.Protocol][]lbConfig {
	out := map[corev1.Protocol][]lbConfig{}
	for _, config := range configs {
		out[config.protocol] = append(out[config.protocol], config)
	}
	return out
}

func getSessionAffinityTimeOut(service *corev1.Service) int32 {
	// NOTE: This if condition is actually not needed, present only for protection against nil value as good coding practice,
	// The API always puts the default value of 10800 whenever sessionAffinity == ClientIP if timeout is not explicitly set
	// There is no ClientIP session affinity without a timeout set.
	if service.Spec.SessionAffinityConfig == nil ||
		service.Spec.SessionAffinityConfig.ClientIP == nil ||
		service.Spec.SessionAffinityConfig.ClientIP.TimeoutSeconds == nil {
		return core.DefaultClientIPServiceAffinitySeconds // default value
	}
	return *service.Spec.SessionAffinityConfig.ClientIP.TimeoutSeconds
}

func hasSessionAffinityTimeOut(service *corev1.Service) bool {
	return service.Spec.SessionAffinity == corev1.ServiceAffinityClientIP &&
		getSessionAffinityTimeOut(service) != core.MaxClientIPServiceAffinitySeconds
}

// lbOpts generates the OVN load balancer options from the kubernetes Service.
func lbOpts(service *corev1.Service) LBOpts {
	affinity := service.Spec.SessionAffinity == corev1.ServiceAffinityClientIP
	lbOptions := LBOpts{
		SkipSNAT: false, // never service-wide, ExternalTrafficPolicy-specific
	}

	lbOptions.Reject = true
	lbOptions.EmptyLBEvents = false

	if config.Kubernetes.OVNEmptyLbEvents {
		if unidling.HasIdleAt(service) {
			lbOptions.Reject = false
			lbOptions.EmptyLBEvents = true
		}

		if unidling.IsOnGracePeriod(service) {
			lbOptions.Reject = false

			// Setting to true even if we don't need empty_lb_events from OVN during grace period
			// because OVN does not support having
			// <no_backends> event=false reject=false
			// Remove the following line when https://bugzilla.redhat.com/show_bug.cgi?id=2177173
			// is fixed
			lbOptions.EmptyLBEvents = true
		}
	}

	if affinity {
		lbOptions.AffinityTimeOut = getSessionAffinityTimeOut(service)
	}
	return lbOptions
}

func lbTemplateOpts(service *corev1.Service, addressFamily corev1.IPFamily) LBOpts {
	lbOptions := lbOpts(service)

	// Only template LBs need an explicit address family.
	lbOptions.AddressFamily = addressFamily
	lbOptions.Template = true
	return lbOptions
}

// mergeLBs joins two LBs together if it is safe to do so.
//
// an LB can be merged if the protocol, rules, and options are the same,
// and only the switches and routers are different.
func mergeLBs(lbs []LB) []LB {
	if len(lbs) == 1 {
		return lbs
	}
	out := make([]LB, 0, len(lbs))

outer:
	for _, lb := range lbs {
		for i := range out {
			// If mergeable, rather than inserting lb to out, just add switches, routers, groups
			// and drop
			if canMergeLB(lb, out[i]) {
				out[i].Switches = append(out[i].Switches, lb.Switches...)
				out[i].Routers = append(out[i].Routers, lb.Routers...)
				out[i].Groups = append(out[i].Groups, lb.Groups...)

				if !strings.HasSuffix(out[i].Name, "_merged") {
					out[i].Name += "_merged"
				}
				continue outer
			}
		}
		out = append(out, lb)
	}

	return out
}

// canMergeLB returns true if two LBs are mergeable.
// We know that the ExternalIDs will be the same, so we don't need to compare them.
// All that matters is the protocol and rules are the same.
func canMergeLB(a, b LB) bool {
	if a.Protocol != b.Protocol {
		return false
	}

	if !reflect.DeepEqual(a.Opts, b.Opts) {
		return false
	}

	// While rules are actually a set, we generate all our lbConfigs from a single source
	// so the ordering will be the same. Thus, we can cheat and just reflect.DeepEqual
	return reflect.DeepEqual(a.Rules, b.Rules)
}

// joinHostsPort takes a list of IPs and a port and converts it to a list of Addrs
func joinHostsPort(ips []string, port int32) []Addr {
	out := make([]Addr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, Addr{IP: ip, Port: port})
	}
	return out
}

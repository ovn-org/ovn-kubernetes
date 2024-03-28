package services

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	conf "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/unidling"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/apis/core"
	utilnet "k8s.io/utils/net"
)

// magic string used in vips to indicate that the node's physical
// ips should be substituted in
const placeholderNodeIPs = "node"

// lbConfig is the abstract desired load balancer configuration.
// vips and endpoints are mixed families.
type lbConfig struct {
	vips     []string    // just ip or the special value "node" for the node's physical IPs (i.e. NodePort)
	protocol v1.Protocol // TCP, UDP, or SCTP
	inport   int32       // the incoming (virtual) port number

	clusterEndpoints lbEndpoints            // addresses of cluster-wide endpoints
	nodeEndpoints    map[string]lbEndpoints // node -> addresses of local endpoints

	// if true, then vips added on the router are in "local" mode
	// that means, skipSNAT, and remove any non-local endpoints.
	// (see below)
	externalTrafficLocal bool
	// if true, then vips added on the switch are in "local" mode
	// that means, remove any non-local endpoints.
	internalTrafficLocal bool
	// indicates if this LB is configuring service of type NodePort.
	hasNodePort bool
}

func (c *lbConfig) makeNodeSwitchTargetIPs(service *v1.Service, node *nodeInfo, clusterEndpoints []string, localEndpoints []string) (
	targetIPs []string, changed bool) {

	targetIPs = clusterEndpoints
	changed = false
	if c.externalTrafficLocal || c.internalTrafficLocal {
		// for ExternalTrafficPolicy=Local, remove non-local endpoints from the router/switch targets
		// NOTE: on the switches, filtered eps are used only by masqueradeVIP
		// for InternalTrafficPolicy=Local, remove non-local endpoints from the switch targets only
		targetIPs = localEndpoints
	}

	// We potentially only removed stuff from the original slice, so just
	// comparing lengths is enough.
	if len(targetIPs) != len(clusterEndpoints) {
		changed = true
	}
	return
}

func (c *lbConfig) makeNodeRouterTargetIPs(service *v1.Service, node *nodeInfo, clusterEndpoints []string, localEndpoints []string, hostMasqueradeIP string) (targetIPs []string, changed bool) {
	targetIPs = clusterEndpoints
	changed = false

	if c.externalTrafficLocal {
		// for ExternalTrafficPolicy=Local, remove non-local endpoints from the router/switch targets
		// NOTE: on the switches, filtered eps are used only by masqueradeVIP
		targetIPs = localEndpoints
	}

	// any targets local to the node need to have a special
	// harpin IP added, but only for the router LB
	targetIPs, updated := util.UpdateIPsSlice(targetIPs, node.l3gatewayAddressesStr(), []string{hostMasqueradeIP})

	// We either only removed stuff from the original slice, or updated some IPs.
	if len(targetIPs) != len(clusterEndpoints) || updated {
		changed = true
	}
	return
}

// just used for consistent ordering
var protos = []v1.Protocol{
	v1.ProtocolTCP,
	v1.ProtocolUDP,
	v1.ProtocolSCTP,
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
func buildServiceLBConfigs(service *v1.Service, endpointSlices []*discovery.EndpointSlice, nodeInfos []nodeInfo, useLBGroup, useTemplates bool) (perNodeConfigs, templateConfigs, clusterConfigs []lbConfig) {
	needsAffinityTimeout := hasSessionAffinityTimeOut(service)

	nodes := []string{}
	for _, n := range nodeInfos {
		nodes = append(nodes, n.name)
	}
	// For each svcPort, determine if it will be applied per-node or cluster-wide
	portToClusterEndpoints, portToNodeToEndpoints := getEndpointsForService(endpointSlices, service, nodes)

	for _, svcPort := range service.Spec.Ports {

		clusterEndpoints := portToClusterEndpoints[svcPort]
		nodeEndpoints := portToNodeToEndpoints[svcPort]
		if nodeEndpoints == nil {
			nodeEndpoints = make(map[string]lbEndpoints)
		}
		// if ExternalTrafficPolicy or InternalTrafficPolicy is local, then we need to do things a bit differently
		externalTrafficLocal := (service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal)
		internalTrafficLocal := (service.Spec.InternalTrafficPolicy != nil) && (*service.Spec.InternalTrafficPolicy == v1.ServiceInternalTrafficPolicyLocal)

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
		clusterIPConfig := lbConfig{
			protocol:             svcPort.Protocol,
			inport:               svcPort.Port,
			vips:                 vips,
			clusterEndpoints:     clusterEndpoints,
			nodeEndpoints:        nodeEndpoints,
			externalTrafficLocal: false, // always false for ClusterIPs
			internalTrafficLocal: internalTrafficLocal,
			hasNodePort:          false,
		}

		// Normally, the ClusterIP LB is global (on all node switches and routers),
		// unless any of the following are true:
		// - Any of the endpoints are host-network
		// - ETP=local service backed by non-local-host-networked endpoints
		//
		// In that case, we need to create per-node LBs.
		if hasHostEndpoints(clusterEndpoints.V4IPs) || hasHostEndpoints(clusterEndpoints.V6IPs) || internalTrafficLocal {
			perNodeConfigs = append(perNodeConfigs, clusterIPConfig)
		} else {
			clusterConfigs = append(clusterConfigs, clusterIPConfig)
		}
	}

	return
}

// makeLBName creates the load balancer name - used to minimize churn
func makeLBName(service *v1.Service, proto v1.Protocol, scope string) string {
	return fmt.Sprintf("Service_%s/%s_%s_%s",
		service.Namespace, service.Name,
		proto, scope,
	)
}

// buildClusterLBs takes a list of lbConfigs and aggregates them
// in to one ovn LB per protocol.
//
// It takes a list of (proto:[vips]:port -> [endpoints]) configs and re-aggregates
// them to a list of (proto:[vip:port -> [endpoint:port]])
// This load balancer is attached to all node switches. In shared-GW mode, it is also on all routers
func buildClusterLBs(service *v1.Service, configs []lbConfig, nodeInfos []nodeInfo, useLBGroup bool) []LB {
	var nodeSwitches []string
	var nodeRouters []string
	var groups []string
	if useLBGroup {
		nodeSwitches = make([]string, 0)
		nodeRouters = make([]string, 0)
		groups = []string{types.ClusterLBGroupName}
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
			Name:        makeLBName(service, proto, "cluster"),
			Protocol:    string(proto),
			ExternalIDs: util.ExternalIDsForObject(service),
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

			v4targets := make([]Addr, 0, len(config.clusterEndpoints.V4IPs))
			for _, targetIP := range config.clusterEndpoints.V4IPs {
				v4targets = append(v4targets, Addr{
					IP:   targetIP,
					Port: config.clusterEndpoints.Port,
				})
			}

			v6targets := make([]Addr, 0, len(config.clusterEndpoints.V6IPs))
			for _, targetIP := range config.clusterEndpoints.V6IPs {
				v6targets = append(v6targets, Addr{
					IP:   targetIP,
					Port: config.clusterEndpoints.Port,
				})
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
func buildTemplateLBs(service *v1.Service, configs []lbConfig, nodes []nodeInfo,
	nodeIPv4Templates, nodeIPv6Templates *NodeIPsTemplates) []LB {

	cbp := configsByProto(configs)
	eids := util.ExternalIDsForObject(service)
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

		optsV4 := lbTemplateOpts(service, v1.IPv4Protocol)
		optsV6 := lbTemplateOpts(service, v1.IPv6Protocol)

		for _, config := range configs {
			switchV4TemplateTarget :=
				makeTemplate(
					makeLBTargetTemplateName(
						service, proto, config.inport,
						optsV4.AddressFamily, "node_switch_template"))
			switchV6TemplateTarget :=
				makeTemplate(
					makeLBTargetTemplateName(
						service, proto, config.inport,
						optsV6.AddressFamily, "node_switch_template"))

			routerV4TemplateTarget :=
				makeTemplate(
					makeLBTargetTemplateName(
						service, proto, config.inport,
						optsV4.AddressFamily, "node_router_template"))
			routerV6TemplateTarget :=
				makeTemplate(
					makeLBTargetTemplateName(
						service, proto, config.inport,
						optsV6.AddressFamily, "node_router_template"))

			allV4TargetIPs := config.clusterEndpoints.V4IPs
			allV6TargetIPs := config.clusterEndpoints.V6IPs

			for range config.vips {
				klog.V(5).Infof("buildTemplateLBs() service %s/%s adding rules", service.Namespace, service.Name)

				// If all targets have exactly the same IPs on all nodes there's
				// no need to use a template, just use the same list of explicit
				// targets on all nodes.
				switchV4TargetNeedsTemplate := false
				switchV6TargetNeedsTemplate := false
				routerV4TargetNeedsTemplate := false
				routerV6TargetNeedsTemplate := false

				for _, node := range nodes {
					localEndpointsV4 := []string{}
					localEndpointsV6 := []string{}
					if localEndpoints, ok := config.nodeEndpoints[node.name]; ok {
						localEndpointsV4 = localEndpoints.V4IPs
						localEndpointsV6 = localEndpoints.V6IPs
					}

					switchV4targetips, changed := config.makeNodeSwitchTargetIPs(
						service, &node, allV4TargetIPs, localEndpointsV4)
					if !switchV4TargetNeedsTemplate && changed {
						switchV4TargetNeedsTemplate = true
					}
					switchV6targetips, changed := config.makeNodeSwitchTargetIPs(
						service, &node, allV6TargetIPs, localEndpointsV6)
					if !switchV6TargetNeedsTemplate && changed {
						switchV6TargetNeedsTemplate = true
					}

					routerV4targetips, changed := config.makeNodeRouterTargetIPs(
						service, &node, allV4TargetIPs, localEndpointsV4,
						conf.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String())
					if !routerV4TargetNeedsTemplate && changed {
						routerV4TargetNeedsTemplate = true
					}
					routerV6targetips, changed := config.makeNodeRouterTargetIPs(
						service, &node, allV6TargetIPs, localEndpointsV6,
						conf.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String())
					if !routerV6TargetNeedsTemplate && changed {
						routerV6TargetNeedsTemplate = true
					}

					switchV4TemplateTarget.Value[node.chassisID] = addrsToString(
						joinHostsPort(switchV4targetips, config.clusterEndpoints.Port))
					switchV6TemplateTarget.Value[node.chassisID] = addrsToString(
						joinHostsPort(switchV6targetips, config.clusterEndpoints.Port))

					routerV4TemplateTarget.Value[node.chassisID] = addrsToString(
						joinHostsPort(routerV4targetips, config.clusterEndpoints.Port))
					routerV6TemplateTarget.Value[node.chassisID] = addrsToString(
						joinHostsPort(routerV6targetips, config.clusterEndpoints.Port))
				}

				sharedV4Targets := []Addr{}
				sharedV6Targets := []Addr{}
				if !switchV4TargetNeedsTemplate || !routerV4TargetNeedsTemplate {
					sharedV4Targets = joinHostsPort(allV4TargetIPs, config.clusterEndpoints.Port)
				}
				if !switchV6TargetNeedsTemplate || !routerV6TargetNeedsTemplate {
					sharedV6Targets = joinHostsPort(allV6TargetIPs, config.clusterEndpoints.Port)
				}

				for _, nodeIPv4Template := range nodeIPv4Templates.AsTemplates() {

					if switchV4TargetNeedsTemplate {
						switchV4Rules = append(switchV4Rules, LBRule{
							Source:  Addr{Template: nodeIPv4Template, Port: config.inport},
							Targets: []Addr{{Template: switchV4TemplateTarget}},
						})
					} else {
						switchV4Rules = append(switchV4Rules, LBRule{
							Source:  Addr{Template: nodeIPv4Template, Port: config.inport},
							Targets: sharedV4Targets,
						})
					}

					if routerV4TargetNeedsTemplate {
						routerV4Rules = append(routerV4Rules, LBRule{
							Source:  Addr{Template: nodeIPv4Template, Port: config.inport},
							Targets: []Addr{{Template: routerV4TemplateTarget}},
						})
					} else {
						routerV4Rules = append(routerV4Rules, LBRule{
							Source:  Addr{Template: nodeIPv4Template, Port: config.inport},
							Targets: sharedV4Targets,
						})
					}
				}

				for _, nodeIPv6Template := range nodeIPv6Templates.AsTemplates() {

					if switchV6TargetNeedsTemplate {
						switchV6Rules = append(switchV6Rules, LBRule{
							Source:  Addr{Template: nodeIPv6Template, Port: config.inport},
							Targets: []Addr{{Template: switchV6TemplateTarget}},
						})
					} else {
						switchV6Rules = append(switchV6Rules, LBRule{
							Source:  Addr{Template: nodeIPv6Template, Port: config.inport},
							Targets: sharedV6Targets,
						})
					}

					if routerV6TargetNeedsTemplate {
						routerV6Rules = append(routerV6Rules, LBRule{
							Source:  Addr{Template: nodeIPv6Template, Port: config.inport},
							Targets: []Addr{{Template: routerV6TemplateTarget}},
						})
					} else {
						routerV6Rules = append(routerV6Rules, LBRule{
							Source:  Addr{Template: nodeIPv6Template, Port: config.inport},
							Targets: sharedV6Targets,
						})
					}
				}
			}
		}

		if nodeIPv4Templates.Len() > 0 {
			if len(switchV4Rules) > 0 {
				out = append(out, LB{
					Name:        makeLBName(service, proto, "node_switch_template_IPv4"),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        optsV4,
					Groups:      []string{types.ClusterSwitchLBGroupName},
					Rules:       switchV4Rules,
					Templates:   getTemplatesFromRulesTargets(switchV4Rules),
				})
			}
			if len(routerV4Rules) > 0 {
				out = append(out, LB{
					Name:        makeLBName(service, proto, "node_router_template_IPv4"),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        optsV4,
					Groups:      []string{types.ClusterRouterLBGroupName},
					Rules:       routerV4Rules,
					Templates:   getTemplatesFromRulesTargets(routerV4Rules),
				})
			}
		}

		if nodeIPv6Templates.Len() > 0 {
			if len(switchV6Rules) > 0 {
				out = append(out, LB{
					Name:        makeLBName(service, proto, "node_switch_template_IPv6"),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        optsV6,
					Groups:      []string{types.ClusterSwitchLBGroupName},
					Rules:       switchV6Rules,
					Templates:   getTemplatesFromRulesTargets(switchV6Rules),
				})
			}
			if len(routerV6Rules) > 0 {
				out = append(out, LB{
					Name:        makeLBName(service, proto, "node_router_template_IPv6"),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        optsV6,
					Groups:      []string{types.ClusterRouterLBGroupName},
					Rules:       routerV6Rules,
					Templates:   getTemplatesFromRulesTargets(routerV6Rules),
				})
			}
		}
	}

	merged := mergeLBs(out)
	if len(merged) != len(out) {
		klog.V(5).Infof("Service %s/%s merged %d LBs to %d",
			service.Namespace, service.Name,
			len(out), len(merged))
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
// see https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/design/host_to_services_OpenFlow.md
// This is for host -> serviceip -> host hairpin
//
// For ExternalTrafficPolicy=local, all "External" IPs (NodePort, ExternalIPs, Loadbalancer Status) have:
// - targets filtered to only local targets
// - SkipSNAT enabled
// - NodePort LB on the switch will have masqueradeIP as the vip to handle etp=local for LGW case.
// This results in the creation of an additional load balancer on the GatewayRouters and NodeSwitches.
func buildPerNodeLBs(service *v1.Service, configs []lbConfig, nodes []nodeInfo) []LB {
	cbp := configsByProto(configs)
	eids := util.ExternalIDsForObject(service)

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

			for _, config := range configs {

				allV4TargetIPs := config.clusterEndpoints.V4IPs
				allV6TargetIPs := config.clusterEndpoints.V6IPs

				localEndpointsV4 := []string{}
				localEndpointsV6 := []string{}
				if localEndpoints, ok := config.nodeEndpoints[node.name]; ok {
					localEndpointsV4 = localEndpoints.V4IPs
					localEndpointsV6 = localEndpoints.V6IPs
				}

				switchV4targetips, _ := config.makeNodeSwitchTargetIPs(service, &node, allV4TargetIPs, localEndpointsV4)
				switchV6targetips, _ := config.makeNodeSwitchTargetIPs(service, &node, allV6TargetIPs, localEndpointsV6)

				routerV4targetips, _ := config.makeNodeRouterTargetIPs(
					service, &node, allV4TargetIPs, localEndpointsV4, conf.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String())
				routerV6targetips, _ := config.makeNodeRouterTargetIPs(
					service, &node, allV6TargetIPs, localEndpointsV6, conf.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String())

				routerV4targets := joinHostsPort(routerV4targetips, config.clusterEndpoints.Port)
				routerV6targets := joinHostsPort(routerV6targetips, config.clusterEndpoints.Port)

				switchV4targets := joinHostsPort(allV4TargetIPs, config.clusterEndpoints.Port)
				switchV6targets := joinHostsPort(allV6TargetIPs, config.clusterEndpoints.Port)

				// Substitute the special vip "node" for the node's physical ips
				// This is used for nodeport
				vips := make([]string, 0, len(config.vips))
				for _, vip := range config.vips {
					if vip == placeholderNodeIPs {
						vips = append(vips, node.hostAddressesStr()...)
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

					if config.externalTrafficLocal && config.hasNodePort {
						// add special masqueradeIP as a vip if its nodePort svc with ETP=local
						mvip := conf.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String()
						targetsETP := joinHostsPort(switchV4targetips, config.clusterEndpoints.Port)
						if isv6 {
							mvip = conf.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String()
							targetsETP = joinHostsPort(switchV6targetips, config.clusterEndpoints.Port)
						}
						switchRules = append(switchRules, LBRule{
							Source:  Addr{IP: mvip, Port: config.inport},
							Targets: targetsETP,
						})
					}
					if config.internalTrafficLocal && util.IsClusterIP(vip) { // ITP only applicable to CIP
						targetsITP := joinHostsPort(switchV4targetips, config.clusterEndpoints.Port)
						if isv6 {
							targetsITP = joinHostsPort(switchV6targetips, config.clusterEndpoints.Port)
						}
						switchRules = append(switchRules, LBRule{
							Source:  Addr{IP: vip, Port: config.inport},
							Targets: targetsITP,
						})
					} else {
						switchRules = append(switchRules, LBRule{
							Source:  Addr{IP: vip, Port: config.inport},
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
						Source:  Addr{IP: vip, Port: config.inport},
						Targets: targets,
					}

					// in other words, is this ExternalTrafficPolicy=local?
					// if so, this gets a separate load balancer with SNAT disabled
					// (but there's no need to do this if the list of targets is empty)
					if config.externalTrafficLocal && len(targets) > 0 {
						noSNATRouterRules = append(noSNATRouterRules, rule)
					} else {
						routerRules = append(routerRules, rule)
					}
				}
			}

			// If switch and router rules are identical, coalesce
			if reflect.DeepEqual(switchRules, routerRules) && len(switchRules) > 0 && node.gatewayRouterName != "" {
				out = append(out, LB{
					Name:        makeLBName(service, proto, "node_router+switch_"+node.name),
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
						Name:        makeLBName(service, proto, "node_router_"+node.name),
						Protocol:    string(proto),
						ExternalIDs: eids,
						Opts:        lbOpts(service),
						Routers:     []string{node.gatewayRouterName},
						Rules:       routerRules,
					})
				}
				if len(noSNATRouterRules) > 0 && node.gatewayRouterName != "" {
					lb := LB{
						Name:        makeLBName(service, proto, "node_local_router_"+node.name),
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
						Name:        makeLBName(service, proto, "node_switch_"+node.name),
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
		klog.V(5).Infof("Service %s/%s merged %d LBs to %d",
			service.Namespace, service.Name,
			len(out), len(merged))
	}

	return merged
}

// configsByProto buckets a list of configs by protocol (tcp, udp, sctp)
func configsByProto(configs []lbConfig) map[v1.Protocol][]lbConfig {
	out := map[v1.Protocol][]lbConfig{}
	for _, config := range configs {
		out[config.protocol] = append(out[config.protocol], config)
	}
	return out
}

func getSessionAffinityTimeOut(service *v1.Service) int32 {
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

func hasSessionAffinityTimeOut(service *v1.Service) bool {
	return service.Spec.SessionAffinity == v1.ServiceAffinityClientIP &&
		getSessionAffinityTimeOut(service) != core.MaxClientIPServiceAffinitySeconds
}

// lbOpts generates the OVN load balancer options from the kubernetes Service.
func lbOpts(service *v1.Service) LBOpts {
	affinity := service.Spec.SessionAffinity == v1.ServiceAffinityClientIP
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

func lbTemplateOpts(service *v1.Service, addressFamily v1.IPFamily) LBOpts {
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

type lbEndpoints struct {
	Port  int32
	V4IPs []string
	V6IPs []string
}

type lbEndpointsConfig struct {
	port        int32
	v4Endpoints []discovery.Endpoint
	v6Endpoints []discovery.Endpoint
}

func doesServicePortMatchEndpointPort(svcPort v1.ServicePort, endpointPort discovery.EndpointPort) bool {
	// Service targetPort is a selector for the endpoints/endpointslices
	// controller to create the endpoints based on that container port name.
	// It is not meant to be used in the Service implementation.
	// The relation is ServicePort.Name - EndpointPort.Name, however,
	// ServicePort.Name is only required for multiple ports and it may be empty.
	// If the endpoint matches the service (through labels) and there is no service port name,
	// that means that it is a single port service and there is only one endpoint.
	// https://github.com/ovn-org/ovn-kubernetes/commit/46bbcc8659ab7222cee08a1b12adecb3a5b65aa8

	// If Service port name is set, it must match the name field in the endpoint
	// If Service port name is not set, we just use the endpoint port as is
	if svcPort.Name != "" && svcPort.Name != *endpointPort.Name {
		return false
	}
	return svcPort.Protocol == *endpointPort.Protocol
}

func getLocalEndpoints(endpoints []discovery.Endpoint, nodeName string) []discovery.Endpoint {
	localEndpoints := []discovery.Endpoint{}
	for _, endpoint := range endpoints {
		if endpoint.NodeName != nil && *endpoint.NodeName == nodeName {
			localEndpoints = append(localEndpoints, endpoint)
		}
	}
	return localEndpoints
}

func portToAddressesAsString(portToAddresses map[v1.ServicePort]lbEndpoints) string {
	res := ""
	for port, lbEndpoints := range portToAddresses {
		res = fmt.Sprintf("%s port=%s, LbEndpoints=%+v ;", res, port.String(), lbEndpoints)
	}
	return res
}

func portToNodeToAddressesAsString(portToNodeToAddresses map[v1.ServicePort]map[string]lbEndpoints) string {
	res := ""
	for port, nodeToAddresses := range portToNodeToAddresses {
		res = fmt.Sprintf("%s port=%s :", res, port.String())
		for node, lbEndpoints := range nodeToAddresses {
			res = fmt.Sprintf("%s node=%s, lbEndpoints=%+v ;", res, node, lbEndpoints)
		}
	}
	return res
}

// GetEndpointsForService takes a service, all its slices and a list of nodes in the cluster and returns two maps that hold all
// the endpoint addresses for the service: one for cluster-wide endpoints, one for per-node endpoints.
func getEndpointsForService(slices []*discovery.EndpointSlice, service *v1.Service, nodes []string) (map[v1.ServicePort]lbEndpoints, map[v1.ServicePort]map[string]lbEndpoints) {
	// intermediate maps holding candidate endpoints (ready or terminating & serving)
	portToClusterEndpoints := make(map[v1.ServicePort]lbEndpointsConfig)           // svc port -> candidate cluster lbEndpoints
	portToNodeToEndpoints := make(map[v1.ServicePort]map[string]lbEndpointsConfig) // svc port -> node -> candidate local lbEndpoints

	// final maps holding endpoint addresses
	portToClusterEndpointAddresses := make(map[v1.ServicePort]lbEndpoints)           // svc port -> addresses of cluster-wide endpoints
	portToNodeToEndpointAddresses := make(map[v1.ServicePort]map[string]lbEndpoints) // svc port -> node -> addresses of local endpoints

	// return empty maps so the caller doesn't have to check for nil and can use it as an iterator
	if len(slices) == 0 {
		return portToClusterEndpointAddresses, portToNodeToEndpointAddresses
	}

	includeAllEndpoints := service != nil && service.Spec.PublishNotReadyAddresses

	// Accumulate endpoints across all slices and store them by port
	for _, slice := range slices {

		var v4EndpointsTmp []discovery.Endpoint
		var v6EndpointsTmp []discovery.Endpoint

		var endpointsPointer *[]discovery.Endpoint
		switch slice.AddressType {
		case discovery.AddressTypeFQDN:
			klog.V(5).Infof("Skipping FQDN slice %s/%s for service %s/%s",
				slice.Namespace, slice.Name, service.Namespace, service.Name)
			continue
		case discovery.AddressTypeIPv4:
			endpointsPointer = &v4EndpointsTmp
		case discovery.AddressTypeIPv6:
			endpointsPointer = &v6EndpointsTmp
		}

		if endpointsPointer == nil {
			continue
		}

		for _, ep := range slice.Endpoints {
			// readiness selection: include endpoints that are either ready
			// or terminating and serving.
			if !(includeAllEndpoints || util.IsEndpointReady(ep) || util.IsEndpointServing(ep)) {
				continue
			}

			// add to v4 or v6 candidate endpoints
			if len(ep.Addresses) > 0 {
				*endpointsPointer = append(*endpointsPointer, ep)
			}
		}

		// Store v4 or v6 endpoints by svc port in the two maps
		for _, slicePort := range slice.Ports {
			for _, svcPort := range service.Spec.Ports {

				if !doesServicePortMatchEndpointPort(svcPort, slicePort) {
					continue
				}

				// fill in port -> cluster endpoints map
				if _, ok := portToClusterEndpoints[svcPort]; !ok {
					portToClusterEndpoints[svcPort] = lbEndpointsConfig{port: *slicePort.Port}
				}
				if entry, ok := portToClusterEndpoints[svcPort]; ok {
					if len(v4EndpointsTmp) > 0 {
						entry.v4Endpoints = append(entry.v4Endpoints, v4EndpointsTmp...)
					} else if len(v6EndpointsTmp) > 0 {
						entry.v6Endpoints = append(entry.v6Endpoints, v6EndpointsTmp...)
					}
					portToClusterEndpoints[svcPort] = entry
				}

				// fill in port -> nodes -> per-node endpoints map
				if _, ok := portToNodeToEndpoints[svcPort]; !ok {
					portToNodeToEndpoints[svcPort] = make(map[string]lbEndpointsConfig)
				}
				if nodeToEndpoints, ok := portToNodeToEndpoints[svcPort]; ok {
					for _, node := range nodes {
						if _, ok := nodeToEndpoints[node]; !ok {
							nodeToEndpoints[node] = lbEndpointsConfig{port: *slicePort.Port}
						}
						if lbConfig, ok := nodeToEndpoints[node]; ok {
							if len(v4EndpointsTmp) > 0 {
								if localEndpoints := getLocalEndpoints(v4EndpointsTmp, node); len(localEndpoints) > 0 {
									lbConfig.v4Endpoints = append(lbConfig.v4Endpoints, localEndpoints...)
								}
							} else if len(v6EndpointsTmp) > 0 {
								if localEndpoints := getLocalEndpoints(v6EndpointsTmp, node); len(localEndpoints) > 0 {
									lbConfig.v6Endpoints = append(lbConfig.v6Endpoints, localEndpoints...)
								}
							}
							nodeToEndpoints[node] = lbConfig
						}
					}
					portToNodeToEndpoints[svcPort] = nodeToEndpoints
				}
				break // slice port has been matched with service port, move to the next slice port
			}
		}
	}

	// Now we have all candidate endpoints, local and cluster-wide, for each port of the service:
	// apply readiness selection and get all endpoint addresses
	for svcPort, lbEndpointsInstance := range portToClusterEndpoints {
		clusterEndpointsConfig := lbEndpoints{}
		if len(lbEndpointsInstance.v4Endpoints) > 0 {
			addresses := util.GetEligibleEndpointAddresses(lbEndpointsInstance.v4Endpoints, service)
			if len(addresses) > 0 {
				clusterEndpointsConfig.V4IPs = addresses
			}
		}
		if len(lbEndpointsInstance.v6Endpoints) > 0 {
			addresses := util.GetEligibleEndpointAddresses(lbEndpointsInstance.v6Endpoints, service)
			if len(addresses) > 0 {
				clusterEndpointsConfig.V6IPs = addresses
			}
		}
		if len(clusterEndpointsConfig.V4IPs) > 0 || len(clusterEndpointsConfig.V6IPs) > 0 {
			clusterEndpointsConfig.Port = lbEndpointsInstance.port
			portToClusterEndpointAddresses[svcPort] = clusterEndpointsConfig
		}
	}
	svcPortsToPop := []v1.ServicePort{}
	for svcPort, nodeToEndpoints := range portToNodeToEndpoints {

		if _, ok := portToNodeToEndpointAddresses[svcPort]; !ok {
			portToNodeToEndpointAddresses[svcPort] = make(map[string]lbEndpoints)
		}

		if nodeToEndpointAddresses, ok := portToNodeToEndpointAddresses[svcPort]; ok {
			nodesToPop := []string{}
			for node, lbEndpointsInstance := range nodeToEndpoints {
				nodeEndpointsConfig := lbEndpoints{}
				if len(lbEndpointsInstance.v4Endpoints) > 0 {
					addresses := util.GetLocalEligibleEndpointAddresses(lbEndpointsInstance.v4Endpoints, service, node)
					if len(addresses) > 0 {
						nodeEndpointsConfig.V4IPs = addresses
					}
				}
				if len(lbEndpointsInstance.v6Endpoints) > 0 {
					addresses := util.GetLocalEligibleEndpointAddresses(lbEndpointsInstance.v6Endpoints, service, node)
					if len(addresses) > 0 {
						nodeEndpointsConfig.V6IPs = addresses
					}
				}
				if len(nodeEndpointsConfig.V4IPs) > 0 || len(nodeEndpointsConfig.V6IPs) > 0 {
					nodeEndpointsConfig.Port = lbEndpointsInstance.port
					nodeToEndpointAddresses[node] = nodeEndpointsConfig

				} else {
					nodesToPop = append(nodesToPop, node) // no endpoints to add: will remove node key
				}

			}
			for _, node := range nodesToPop {
				delete(nodeToEndpoints, node)
			}
			if len(nodeToEndpointAddresses) > 0 {
				portToNodeToEndpointAddresses[svcPort] = nodeToEndpointAddresses
			} else {
				svcPortsToPop = append(svcPortsToPop, svcPort) // no service port to add: will remove svc port key
			}
		}
	}

	for _, svcPort := range svcPortsToPop {
		delete(portToNodeToEndpointAddresses, svcPort)
	}

	klog.V(5).Infof("Cluster endpoints for %s/%s  are: %s",
		service.Namespace, service.Name, portToAddressesAsString(portToClusterEndpointAddresses))

	klog.V(5).Infof("Local endpoints for %s/%s  are: %s",
		service.Namespace, service.Name, portToNodeToAddressesAsString(portToNodeToEndpointAddresses))

	return portToClusterEndpointAddresses, portToNodeToEndpointAddresses
}

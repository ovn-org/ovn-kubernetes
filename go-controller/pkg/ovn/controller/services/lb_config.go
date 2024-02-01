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
	"k8s.io/apimachinery/pkg/util/sets"
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

type lbEndpoints struct {
	Port  int32
	V4IPs []string
	V6IPs []string
}

func makeNodeSwitchTargetIPs(node string, c *lbConfig) (targetIPsV4, targetIPsV6 []string, v4Changed, v6Changed bool) {
	targetIPsV4 = c.clusterEndpoints.V4IPs
	targetIPsV6 = c.clusterEndpoints.V6IPs

	if c.externalTrafficLocal || c.internalTrafficLocal {
		// For ExternalTrafficPolicy=Local, remove non-local endpoints from the router/switch targets
		// NOTE: on the switches, filtered eps are used only by masqueradeVIP
		// for InternalTrafficPolicy=Local, remove non-local endpoints from the switch targets only
		localIPsV4 := []string{}
		localIPsV6 := []string{}
		if localEndpoints, ok := c.nodeEndpoints[node]; ok {
			localIPsV4 = localEndpoints.V4IPs
			localIPsV6 = localEndpoints.V6IPs
		}
		targetIPsV4 = localIPsV4
		targetIPsV6 = localIPsV6
	}

	// Local endpoints are a subset of cluster endpoints, so it is enough to compare their length
	v4Changed = len(targetIPsV4) != len(c.clusterEndpoints.V4IPs)
	v6Changed = len(targetIPsV6) != len(c.clusterEndpoints.V6IPs)

	return
}

func makeNodeRouterTargetIPs(node *nodeInfo, c *lbConfig, hostMasqueradeIPV4, hostMasqueradeIPV6 string) (targetIPsV4, targetIPsV6 []string, v4Changed, v6Changed bool) {
	targetIPsV4 = c.clusterEndpoints.V4IPs
	targetIPsV6 = c.clusterEndpoints.V6IPs

	if c.externalTrafficLocal {
		// For ExternalTrafficPolicy=Local, remove non-local endpoints from the router/switch targets
		// NOTE: on the switches, filtered eps are used only by masqueradeVIP
		localIPsV4 := []string{}
		localIPsV6 := []string{}
		if localEndpoints, ok := c.nodeEndpoints[node.name]; ok {
			localIPsV4 = localEndpoints.V4IPs
			localIPsV6 = localEndpoints.V6IPs
		}
		targetIPsV4 = localIPsV4
		targetIPsV6 = localIPsV6
	}

	// Any targets local to the node need to have a special
	// harpin IP added, but only for the router LB
	targetIPsV4, v4Updated := util.UpdateIPsSlice(targetIPsV4, node.l3gatewayAddressesStr(), []string{hostMasqueradeIPV4})
	targetIPsV6, v6Updated := util.UpdateIPsSlice(targetIPsV6, node.l3gatewayAddressesStr(), []string{hostMasqueradeIPV6})

	// Local endpoints are a subset of cluster endpoints, so it is enough to compare their length
	v4Changed = len(targetIPsV4) != len(c.clusterEndpoints.V4IPs) || v4Updated
	v6Changed = len(targetIPsV6) != len(c.clusterEndpoints.V6IPs) || v6Updated

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

	nodes := sets.New[string]()
	for _, n := range nodeInfos {
		nodes.Insert(n.name)
	}
	// get all the endpoints classified by port and by port,node
	portToClusterEndpoints, portToNodeToEndpoints := getEndpointsForService(endpointSlices, service, nodes)
	for _, svcPort := range service.Spec.Ports {
		svcPortKey := getServicePortKey(svcPort.Protocol, svcPort.Name)
		clusterEndpoints := portToClusterEndpoints[svcPortKey]
		nodeEndpoints := portToNodeToEndpoints[svcPortKey]
		if nodeEndpoints == nil {
			nodeEndpoints = make(map[string]lbEndpoints)
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

					switchV4TargetIPs, switchV6TargetIPs, v4Changed, v6Changed := makeNodeSwitchTargetIPs(node.name, &config)
					if !switchV4TargetNeedsTemplate && v4Changed {
						switchV4TargetNeedsTemplate = true
					}
					if !switchV6TargetNeedsTemplate && v6Changed {
						switchV6TargetNeedsTemplate = true
					}

					routerV4TargetIPs, routerV6TargetIPs, v4Changed, v6Changed := makeNodeRouterTargetIPs(
						&node,
						&config,
						conf.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String(),
						conf.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String())

					if !routerV4TargetNeedsTemplate && v4Changed {
						routerV4TargetNeedsTemplate = true
					}
					if !routerV6TargetNeedsTemplate && v6Changed {
						routerV6TargetNeedsTemplate = true
					}

					switchV4TemplateTarget.Value[node.chassisID] = addrsToString(
						joinHostsPort(switchV4TargetIPs, config.clusterEndpoints.Port))
					switchV6TemplateTarget.Value[node.chassisID] = addrsToString(
						joinHostsPort(switchV6TargetIPs, config.clusterEndpoints.Port))

					routerV4TemplateTarget.Value[node.chassisID] = addrsToString(
						joinHostsPort(routerV4TargetIPs, config.clusterEndpoints.Port))
					routerV6TemplateTarget.Value[node.chassisID] = addrsToString(
						joinHostsPort(routerV6TargetIPs, config.clusterEndpoints.Port))
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

				switchV4TargetIPs, switchV6TargetIPs, _, _ := makeNodeSwitchTargetIPs(node.name, &config)

				routerV4TargetIPs, routerV6TargetIPs, _, _ := makeNodeRouterTargetIPs(
					&node,
					&config,
					conf.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String(),
					conf.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String())

				routerV4targets := joinHostsPort(routerV4TargetIPs, config.clusterEndpoints.Port)
				routerV6targets := joinHostsPort(routerV6TargetIPs, config.clusterEndpoints.Port)

				switchV4targets := joinHostsPort(config.clusterEndpoints.V4IPs, config.clusterEndpoints.Port)
				switchV6targets := joinHostsPort(config.clusterEndpoints.V6IPs, config.clusterEndpoints.Port)

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
						targetsETP := joinHostsPort(switchV4TargetIPs, config.clusterEndpoints.Port)
						if isv6 {
							mvip = conf.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String()
							targetsETP = joinHostsPort(switchV6TargetIPs, config.clusterEndpoints.Port)
						}
						switchRules = append(switchRules, LBRule{
							Source:  Addr{IP: mvip, Port: config.inport},
							Targets: targetsETP,
						})
					}
					if config.internalTrafficLocal && util.IsClusterIP(vip) { // ITP only applicable to CIP
						targetsITP := joinHostsPort(switchV4TargetIPs, config.clusterEndpoints.Port)
						if isv6 {
							targetsITP = joinHostsPort(switchV6TargetIPs, config.clusterEndpoints.Port)
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

func getServicePortKey(protocol v1.Protocol, name string) string {
	return fmt.Sprintf("%s/%s", protocol, name)
}

// GetEndpointsForService takes a service, all its slices and the list of nodes in the OVN zone
// and returns two maps that hold all the endpoint addresses for the service:
// one classified by port, one classified by port,node. This second map is only filled in
// when the service needs local (per-node) endpoints, that is when ETP=local or ITP=local.
// The node list helps to keep the resulting map small, since we're only interested in local endpoints.
func getEndpointsForService(slices []*discovery.EndpointSlice, service *v1.Service, nodes sets.Set[string]) (map[string]lbEndpoints, map[string]map[string]lbEndpoints) {
	// classify endpoints
	ports := map[string]int32{}
	portToEndpoints := map[string][]discovery.Endpoint{}
	portToNodeToEndpoints := map[string]map[string][]discovery.Endpoint{}
	requiresLocalEndpoints := util.ServiceExternalTrafficPolicyLocal(service) || util.ServiceInternalTrafficPolicyLocal(service)

	for _, port := range service.Spec.Ports {
		name := getServicePortKey(port.Protocol, port.Name)
		ports[name] = 0
	}

	for _, slice := range slices {

		if slice.AddressType == discovery.AddressTypeFQDN {
			continue // consider only v4 and v6, discard FQDN
		}

		slicePorts := make([]string, 0, len(slice.Ports))

		for _, port := range slice.Ports {
			// check if there's a service port matching the slice protocol/name
			slicePortName := ""
			if port.Name != nil {
				slicePortName = *port.Name
			}
			name := getServicePortKey(*port.Protocol, slicePortName)
			if _, hasPort := ports[name]; hasPort {
				slicePorts = append(slicePorts, name)
				ports[name] = *port.Port
				continue
			}
			// service port name might be empty: check against slice protocol/""
			noName := getServicePortKey(*port.Protocol, "")
			if _, hasPort := ports[noName]; hasPort {
				slicePorts = append(slicePorts, name)
				ports[noName] = *port.Port
			}
		}
		for _, endpoint := range slice.Endpoints {
			for _, port := range slicePorts {

				portToEndpoints[port] = append(portToEndpoints[port], endpoint)

				// won't add items to portToNodeToEndpoints if  the service doesn't need it,
				// the endpoint is not assigned to a node yet or the endpoint is not local to the OVN zone
				if !requiresLocalEndpoints || endpoint.NodeName == nil || !nodes.Has(*endpoint.NodeName) {
					continue
				}
				if portToNodeToEndpoints[port] == nil {
					portToNodeToEndpoints[port] = make(map[string][]discovery.Endpoint, len(nodes))
				}

				if portToNodeToEndpoints[port][*endpoint.NodeName] == nil {
					portToNodeToEndpoints[port][*endpoint.NodeName] = []discovery.Endpoint{}
				}
				portToNodeToEndpoints[port][*endpoint.NodeName] = append(portToNodeToEndpoints[port][*endpoint.NodeName], endpoint)
			}
		}
	}

	// get eligible endpoint addresses
	portToLBEndpoints := make(map[string]lbEndpoints, len(portToEndpoints))
	portToNodeToLBEndpoints := make(map[string]map[string]lbEndpoints, len(portToEndpoints))

	for port, endpoints := range portToEndpoints {
		addresses := util.GetEligibleEndpointAddresses(endpoints, service)
		v4IPs, _ := util.MatchAllIPStringFamily(false, addresses)
		v6IPs, _ := util.MatchAllIPStringFamily(true, addresses)
		if len(v4IPs) > 0 || len(v6IPs) > 0 {
			portToLBEndpoints[port] = lbEndpoints{
				V4IPs: v4IPs,
				V6IPs: v6IPs,
				Port:  ports[port],
			}
		}
	}
	klog.V(5).Infof("Cluster endpoints for %s/%s are: %v", service.Namespace, service.Name, portToLBEndpoints)

	for port, nodeToEndpoints := range portToNodeToEndpoints {
		for node, endpoints := range nodeToEndpoints {
			addresses := util.GetEligibleEndpointAddresses(endpoints, service)
			v4IPs, _ := util.MatchAllIPStringFamily(false, addresses)
			v6IPs, _ := util.MatchAllIPStringFamily(true, addresses)
			if len(v4IPs) > 0 || len(v6IPs) > 0 {
				if portToNodeToLBEndpoints[port] == nil {
					portToNodeToLBEndpoints[port] = make(map[string]lbEndpoints, len(nodes))
				}

				portToNodeToLBEndpoints[port][node] = lbEndpoints{
					V4IPs: v4IPs,
					V6IPs: v6IPs,
					Port:  ports[port],
				}
			}
		}
	}

	if requiresLocalEndpoints {
		klog.V(5).Infof("Local endpoints for %s/%s are: %v", service.Namespace, service.Name, portToNodeToLBEndpoints)
	}

	return portToLBEndpoints, portToNodeToLBEndpoints
}

package services

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/unidling"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/klog/v2"
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
	eps      util.LbEndpoints

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

// just used for consistent ordering
var protos = []v1.Protocol{
	v1.ProtocolTCP,
	v1.ProtocolUDP,
	v1.ProtocolSCTP,
}

// buildServiceLBConfigs generates the abstract load balancer(s) configurations for each service. The abstract configurations
// are then expanded in buildClusterLBs and buildPerNodeLBs to the full list of OVN LBs desired.
//
// It creates two lists of configurations:
// - the per-node configs, which are load balancers that, for some reason must be expanded per-node. (see below for why)
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
func buildServiceLBConfigs(service *v1.Service, endpointSlices []*discovery.EndpointSlice) (perNodeConfigs []lbConfig, clusterConfigs []lbConfig) {
	// For each svcPort, determine if it will be applied per-node or cluster-wide
	for _, svcPort := range service.Spec.Ports {
		eps := util.GetLbEndpoints(endpointSlices, svcPort)

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
				eps:                  eps,
				externalTrafficLocal: externalTrafficLocal,
				internalTrafficLocal: false, // always false for non-ClusterIPs
				hasNodePort:          true,
			}
			perNodeConfigs = append(perNodeConfigs, nodePortLBConfig)
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
				eps:                  eps,
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
			eps:                  eps,
			externalTrafficLocal: false, // always false for ClusterIPs
			internalTrafficLocal: internalTrafficLocal,
			hasNodePort:          false,
		}

		// Normally, the ClusterIP LB is global (on all node switches and routers),
		// unless any of the following are true:
		// - Any of the endpoints are host-network
		// - ETP=local service backed by non-local-host-networked endpoints
		// - OCP only HACK: It's an openshift-dns:default-dns service
		//
		// In that case, we need to create per-node LBs.
		if hasHostEndpoints(eps.V4IPs) || hasHostEndpoints(eps.V6IPs) || internalTrafficLocal ||
			// OCP only hack begin
			(service.Namespace == "openshift-dns" && service.Name == "dns-default") {
			// OCP only hack end
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

			v4targets := make([]Addr, 0, len(config.eps.V4IPs))
			for _, tgt := range config.eps.V4IPs {
				v4targets = append(v4targets, Addr{
					IP:   tgt,
					Port: config.eps.Port,
				})
			}

			v6targets := make([]Addr, 0, len(config.eps.V6IPs))
			for _, tgt := range config.eps.V6IPs {
				v6targets = append(v6targets, Addr{
					IP:   tgt,
					Port: config.eps.Port,
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

// buildPerNodeLBs takes a list of lbConfigs and expands them to one LB per protocol per node
//
// Per-node lbs are created for
// - clusterip services with host-network endpoints are attached to each node's gateway router + switch
// - nodeport services are attached to each node's gateway router + switch, vips are node's physical IPs (except if etp=local+ovnk backend pods)
// - any services with host-network endpoints
// - services with external IPs / LoadBalancer Status IPs
//
// HOWEVER, we need to replace, on each nodes gateway router only, any host-network endpoints with a special loopback address
// see https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/design/host_to_services_OpenFlow.md
// This is for host -> serviceip -> host hairpin
//
// For ExternalTrafficPolicy, all "External" IPs (NodePort, ExternalIPs, Loadbalancer Status) have:
// - targets filtered to only local targets
// - SkipSNAT enabled
// - NP LB on the switch will have masqueradeIP as the vip to handle etp=local for LGW case.
// This results in the creation of an additional load balancer on the GatewayRouters and NodeSwitches.
func buildPerNodeLBs(service *v1.Service, configs []lbConfig, nodes []nodeInfo) []LB {
	cbp := configsByProto(configs)
	eids := util.ExternalIDsForObject(service)

	out := make([]LB, 0, len(nodes)*len(configs))

	// output is one LB per node per protocol
	// with one rule per vip
	for _, node := range nodes {
		for _, proto := range protos {
			configs, ok := cbp[proto]
			if !ok {
				continue
			}

			// attach to router & switch,
			// rules may or may not be different
			// localRouterRules are rules with no snat
			routerRules := make([]LBRule, 0, len(configs))
			noSNATRouterRules := make([]LBRule, 0)
			switchRules := make([]LBRule, 0, len(configs))

			for _, config := range configs {
				vips := config.vips

				routerV4targetips := config.eps.V4IPs
				routerV6targetips := config.eps.V6IPs
				switchV4targetips := config.eps.V4IPs
				switchV6targetips := config.eps.V6IPs

				if config.externalTrafficLocal {
					// for ExternalTrafficPolicy=Local, remove non-local endpoints from the router/switch targets
					// NOTE: on the switches, filtered eps are used only by masqueradeVIP
					routerV4targetips = util.FilterIPsSlice(routerV4targetips, node.nodeSubnets(), true)
					routerV6targetips = util.FilterIPsSlice(routerV6targetips, node.nodeSubnets(), true)
					switchV4targetips = util.FilterIPsSlice(switchV4targetips, node.nodeSubnets(), true)
					switchV6targetips = util.FilterIPsSlice(switchV6targetips, node.nodeSubnets(), true)
				}
				if config.internalTrafficLocal {
					// for InternalTrafficPolicy=Local, remove non-local endpoints from the switch targets only
					switchV4targetips = util.FilterIPsSlice(switchV4targetips, node.nodeSubnets(), true)
					switchV6targetips = util.FilterIPsSlice(switchV6targetips, node.nodeSubnets(), true)
				}
				// at this point, the targets may be empty

				// any targets local to the node need to have a special
				// harpin IP added, but only for the router LB
				routerV4targetips = util.UpdateIPsSlice(routerV4targetips, node.nodeIPs, []string{types.V4HostMasqueradeIP})
				routerV6targetips = util.UpdateIPsSlice(routerV6targetips, node.nodeIPs, []string{types.V6HostMasqueradeIP})

				routerV4targets := joinHostsPort(routerV4targetips, config.eps.Port)
				routerV6targets := joinHostsPort(routerV6targetips, config.eps.Port)

				switchV4Targets := joinHostsPort(config.eps.V4IPs, config.eps.Port)
				switchV6Targets := joinHostsPort(config.eps.V6IPs, config.eps.Port)

				// OCP HACK begin
				// TODO: Remove this hack once we add support for ITP:preferLocal and DNS operator starts using it.
				if service.Namespace == "openshift-dns" && service.Name == "dns-default" {
					// Filter out endpoints that are local to this node.
					switchV4targetDNSips := util.FilterIPsSlice(config.eps.V4IPs, node.podSubnets, true)
					switchV6targetDNSips := util.FilterIPsSlice(config.eps.V6IPs, node.podSubnets, true)
					// If no local endpoints were found add all the endpoints as targets.
					if len(switchV4targetDNSips) == 0 {
						switchV4targetDNSips = config.eps.V4IPs
					}
					if len(switchV6targetDNSips) == 0 {
						switchV6targetDNSips = config.eps.V6IPs
					}
					switchV4Targets = ovnlb.JoinHostsPort(switchV4targetDNSips, config.eps.Port)
					switchV6Targets = ovnlb.JoinHostsPort(switchV6targetDNSips, config.eps.Port)
				}
				// OCP HACK end

				// Substitute the special vip "node" for the node's physical ips
				// This is used for nodeport
				vips = make([]string, 0, len(vips))
				for _, vip := range config.vips {
					if vip == placeholderNodeIPs {
						vips = append(vips, node.nodeIPs...)
					} else {
						vips = append(vips, vip)
					}
				}

				for _, vip := range vips {
					isv6 := utilnet.IsIPv6String((vip))
					// build switch rules
					targets := switchV4Targets
					if isv6 {
						targets = switchV6Targets
					}

					if config.externalTrafficLocal && config.hasNodePort {
						// add special masqueradeIP as a vip if its nodePort svc with ETP=local
						mvip := types.V4HostETPLocalMasqueradeIP
						targetsETP := joinHostsPort(switchV4targetips, config.eps.Port)
						if isv6 {
							mvip = types.V6HostETPLocalMasqueradeIP
							targetsETP = joinHostsPort(switchV6targetips, config.eps.Port)
						}
						switchRules = append(switchRules, LBRule{
							Source:  Addr{IP: mvip, Port: config.inport},
							Targets: targetsETP,
						})
					}
					if config.internalTrafficLocal && util.IsClusterIP(vip) { // ITP only applicable to CIP
						targetsITP := joinHostsPort(switchV4targetips, config.eps.Port)
						if isv6 {
							targetsITP = joinHostsPort(switchV6targetips, config.eps.Port)
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
		return 10800 // default value
	}
	return *service.Spec.SessionAffinityConfig.ClientIP.TimeoutSeconds
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
		}
	}

	if affinity {
		lbOptions.AffinityTimeOut = getSessionAffinityTimeOut(service)
	}
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
			// If mergeable, rather than inserting lb to out, just add switches and routers
			// and drop
			if canMergeLB(lb, out[i]) {
				out[i].Switches = append(out[i].Switches, lb.Switches...)
				out[i].Routers = append(out[i].Routers, lb.Routers...)

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

package services

import (
	"fmt"
	"reflect"
	"strings"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
	"k8s.io/klog/v2"
)

// lbConfig is the abstract desired load balancer configuration.
// vips and endpoints are mixed families.
type lbConfig struct {
	vips                     []string // just ip or the special value "node" for the node's physical IPs (i.e. NodePort)
	protocol                 v1.Protocol
	inport                   int32
	eps                      util.LbEndpoints
	routerLocalEndpointsOnly bool
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
// - services with host-network endpoints (for shared gateway mode)
// - services with ExternalTrafficPolicy = Local and ExternalIPs or LoadBalancer IPs
func buildServiceLBConfigs(service *v1.Service, endpointSlices []*discovery.EndpointSlice) (perNodeConfigs []lbConfig, clusterConfigs []lbConfig) {
	// For each svcPort, determine if it will be applied per-node or cluster-wide
	for _, svcPort := range service.Spec.Ports {
		eps := util.GetLbEndpoints(endpointSlices, svcPort)

		// NodePort services get a per-node load balancer, but with the node's physical IP as the vip
		// Thus, the vip "node" will be expanded later.
		if svcPort.NodePort != 0 {
			perNodeConfigs = append(perNodeConfigs, lbConfig{
				protocol:                 svcPort.Protocol,
				inport:                   svcPort.NodePort,
				vips:                     []string{"node"},
				eps:                      eps,
				routerLocalEndpointsOnly: (service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal),
			})
		}

		// Build up list of vips
		clusterVips := append([]string{}, service.Spec.ClusterIPs...)
		// Handle old clusters w/o v6 support
		if len(clusterVips) == 0 {
			clusterVips = []string{service.Spec.ClusterIP}
		}

		// ExternalIPs (and LoadBalancer status IPs) are treated as standard cluster IPs,
		// unless ExternalTrafficPolicy is Local
		if service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
			perNodeVips := append([]string{}, service.Spec.ExternalIPs...)
			// LoadBalancer status
			for _, ingress := range service.Status.LoadBalancer.Ingress {
				if ingress.IP != "" {
					perNodeVips = append(perNodeVips, ingress.IP)
				}
			}
			if len(perNodeVips) > 0 {
				perNodeConfigs = append(perNodeConfigs, lbConfig{
					protocol:                 svcPort.Protocol,
					inport:                   svcPort.Port,
					vips:                     perNodeVips,
					eps:                      eps,
					routerLocalEndpointsOnly: true,
				})
			}
		} else {
			// ExternalIP
			clusterVips = append(clusterVips, service.Spec.ExternalIPs...)
			// LoadBalancer status
			for _, ingress := range service.Status.LoadBalancer.Ingress {
				if ingress.IP != "" {
					clusterVips = append(clusterVips, ingress.IP)
				}
			}
		}

		// Normally, the ClusterIP LB is global (on all node switches and routers),
		// unless both of the following are true:
		// - We're in shared gateway mode, and
		// - Any of the endpoints are host-network
		//
		// In that case, we need to create per-node LBs.
		if globalconfig.Gateway.Mode == globalconfig.GatewayModeShared &&
			(hasHostEndpoints(eps.V4IPs) || hasHostEndpoints(eps.V6IPs)) {
			perNodeConfigs = append(perNodeConfigs, lbConfig{
				protocol: svcPort.Protocol,
				inport:   svcPort.Port,
				vips:     clusterVips,
				eps:      eps,
			})
		} else {
			clusterConfigs = append(clusterConfigs, lbConfig{
				protocol: svcPort.Protocol,
				inport:   svcPort.Port,
				vips:     clusterVips,
				eps:      eps,
			})
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
func buildClusterLBs(service *v1.Service, configs []lbConfig, nodeInfos []nodeInfo) []ovnlb.LB {
	nodeSwitches := make([]string, 0, len(nodeInfos))
	nodeRouters := make([]string, 0, len(nodeInfos))
	for _, node := range nodeInfos {
		nodeSwitches = append(nodeSwitches, node.switchName)
		if node.gatewayRouterName != "" {
			nodeRouters = append(nodeRouters, node.gatewayRouterName)
		}
	}

	cbp := configsByProto(configs)

	out := []ovnlb.LB{}
	for _, proto := range protos {
		cfgs, ok := cbp[proto]
		if !ok {
			continue
		}
		lb := ovnlb.LB{
			Name:        makeLBName(service, proto, "cluster"),
			Protocol:    string(proto),
			ExternalIDs: util.ExternalIDsForObject(service),
			Opts:        lbOpts(service),

			Switches: nodeSwitches,
			Routers:  nodeRouters, // Technically only necessary for services with external-ip, but doesn't hurt.
		}

		for _, config := range cfgs {
			if config.routerLocalEndpointsOnly {
				panic("coding error: nodeLocalTraffic on cluster-wide LB")
			}

			v4targets := make([]ovnlb.Addr, 0, len(config.eps.V4IPs))
			for _, tgt := range config.eps.V4IPs {
				v4targets = append(v4targets, ovnlb.Addr{
					IP:   tgt,
					Port: config.eps.Port,
				})
			}

			v6targets := make([]ovnlb.Addr, 0, len(config.eps.V6IPs))
			for _, tgt := range config.eps.V6IPs {
				v6targets = append(v6targets, ovnlb.Addr{
					IP:   tgt,
					Port: config.eps.Port,
				})
			}

			rules := make([]ovnlb.LBRule, 0, len(config.vips))
			for _, vip := range config.vips {
				if vip == "node" {
					panic("coding error: node IP for cluster LB")
				}
				targets := v4targets
				if strings.Contains(vip, ":") {
					targets = v6targets
				}

				rules = append(rules, ovnlb.LBRule{
					Source: ovnlb.Addr{
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
// This works differently based on whether or not we're in shared or local gateway mode.
//
// For all modes, servies with ExternalTrafficPolicy=Local have a LB attached to each node's
// gateway router, with only endpoints on that node, but only ExternalIP and nodePort vips (whew).
//
// For local gateway, per-node lbs are created for:
// - nodePort services, attached to each node's gateway router, vips are node's physical IPs
//
// For shared gateway, per-node lbs are created for
// - clusterip services with host-network services are attached to each node's gateway router + switch
// - nodeport services are attached to each node's gateway router + switch, vips are node's physical IPs
// - any services with host-network endpoints
//
// HOWEVER, we need to replace, on each nodes gateway router only, any host-network endpoints with a special loopback address
// see https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/design/host_to_services_OpenFlow.md
// This is for host -> serviceip -> host hairpin
func buildPerNodeLBs(service *v1.Service, configs []lbConfig, nodes []nodeInfo) []ovnlb.LB {
	cbp := configsByProto(configs)
	eids := util.ExternalIDsForObject(service)

	out := make([]ovnlb.LB, 0, len(nodes)*len(configs))

	// output is one LB per node per protocol
	// with one rule per vip
	for _, node := range nodes {
		for _, proto := range protos {
			configs, ok := cbp[proto]
			if !ok {
				continue
			}

			// local gateway mode - attach to router only
			// shared gateay mode - attach to router & switch,
			// rules may or may not be different
			routerRules := make([]ovnlb.LBRule, 0, len(configs))
			switchRules := make([]ovnlb.LBRule, 0, len(configs))

			// routerLocalOnlyRules are used for ExternalTrafficPolicy rules, which
			// need different options (namely, snat options)
			routerLocalOnlyRules := make([]ovnlb.LBRule, 0, len(configs))

			for _, config := range configs {
				vips := config.vips

				routerV4targetips := config.eps.V4IPs
				routerV6targetips := config.eps.V6IPs

				// If routerLocalEndpointsOnly is true, then filter all non-local endpoints from the router targets
				// (this is because the switch-LB services pod traffic, and the router LB services external traffic)
				if config.routerLocalEndpointsOnly {
					routerV4targetips = util.FilterIPsSlice(routerV4targetips, node.nodeSubnets(), true)
					routerV6targetips = util.FilterIPsSlice(routerV6targetips, node.nodeSubnets(), true)
				}

				// For shared gateway mode, any targets local to the node need to have a special
				// harpin IP added, but only for the router LB
				if globalconfig.Gateway.Mode == "shared" {
					routerV4targetips = util.UpdateIPsSlice(routerV4targetips, node.nodeIPs, []string{types.V4HostMasqueradeIP})
					routerV6targetips = util.UpdateIPsSlice(routerV6targetips, node.nodeIPs, []string{types.V6HostMasqueradeIP})
				}

				routerV4targets := ovnlb.JoinHostsPort(routerV4targetips, config.eps.Port)
				routerV6targets := ovnlb.JoinHostsPort(routerV6targetips, config.eps.Port)

				// Switch targets are never touched
				switchV4Targets := ovnlb.JoinHostsPort(config.eps.V4IPs, config.eps.Port)
				switchV6Targets := ovnlb.JoinHostsPort(config.eps.V6IPs, config.eps.Port)

				// Substitute the special vip "node" for the node's physical ips
				// This is used for nodeport
				vips = make([]string, 0, len(vips))
				for _, vip := range config.vips {
					if vip == "node" {
						vips = append(vips, node.nodeIPs...)
					} else {
						vips = append(vips, vip)
					}
				}

				for _, vip := range vips {
					// build router rules
					targets := routerV4targets
					if strings.Contains(vip, ":") {
						targets = routerV6targets
					}

					if config.routerLocalEndpointsOnly {
						routerLocalOnlyRules = append(routerRules, ovnlb.LBRule{
							Source:  ovnlb.Addr{IP: vip, Port: config.inport},
							Targets: targets,
						})

					} else {
						routerRules = append(routerRules, ovnlb.LBRule{
							Source:  ovnlb.Addr{IP: vip, Port: config.inport},
							Targets: targets,
						})
					}

					// For shared gateway or ExternalTrafficPolicy = local, there is also a per-switch rule
					if globalconfig.Gateway.Mode == "shared" || config.routerLocalEndpointsOnly {
						targets := switchV4Targets
						if strings.Contains(vip, ":") {
							targets = switchV6Targets
						}

						switchRules = append(switchRules, ovnlb.LBRule{
							Source:  ovnlb.Addr{IP: vip, Port: config.inport},
							Targets: targets,
						})
					}
				}
			}

			// If switch and router rules are identical, coalesce
			if reflect.DeepEqual(switchRules, routerRules) && len(switchRules) > 0 && node.gatewayRouterName != "" {
				out = append(out, ovnlb.LB{
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
					out = append(out, ovnlb.LB{
						Name:        makeLBName(service, proto, "node_router_"+node.name),
						Protocol:    string(proto),
						ExternalIDs: eids,
						Opts:        lbOpts(service),
						Routers:     []string{node.gatewayRouterName},
						Rules:       routerRules,
					})
				}
				if len(switchRules) > 0 {
					out = append(out, ovnlb.LB{
						Name:        makeLBName(service, proto, "node_switch_"+node.name),
						Protocol:    string(proto),
						ExternalIDs: eids,
						Opts:        lbOpts(service),
						Switches:    []string{node.switchName},
						Rules:       switchRules,
					})
				}
			}

			if len(routerLocalOnlyRules) > 0 {
				opts := lbOpts(service)
				// Skip SNAT, so that the client IP is preserved.
				// This works because we're not going to go out over the overlay.
				opts.SkipSNAT = true

				out = append(out, ovnlb.LB{
					Name:        makeLBName(service, proto, "node_router_local_"+node.name),
					Protocol:    string(proto),
					ExternalIDs: eids,
					Opts:        opts,
					Routers:     []string{node.gatewayRouterName},
					Rules:       routerLocalOnlyRules,
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

func configsByProto(configs []lbConfig) map[v1.Protocol][]lbConfig {
	out := map[v1.Protocol][]lbConfig{}
	for _, config := range configs {
		out[config.protocol] = append(out[config.protocol], config)
	}
	return out
}

func lbOpts(service *v1.Service) ovnlb.LBOpts {
	return ovnlb.LBOpts{
		Unidling: svcNeedsIdling(service.GetAnnotations()),
		Affinity: service.Spec.SessionAffinity == v1.ServiceAffinityClientIP,
	}
}

// mergeLBs joins two LBs together if it is safe to do so.
//
// an LB can be merged if the protocol, rules, and options are the same,
// and only the targets (switches and routers) are different.
func mergeLBs(lbs []ovnlb.LB) []ovnlb.LB {
	if len(lbs) == 1 {
		return lbs
	}
	out := make([]ovnlb.LB, 0, len(lbs))

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
func canMergeLB(a, b ovnlb.LB) bool {
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

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package node

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// nftables chain names
const (
	nftablesLocalGatewayMasqChain = "ovn-kube-local-gw-masq"
	nftablesPodSubnetMasqChain    = "ovn-kube-pod-subnet-masq"
	nftablesUDNMasqChain          = "ovn-kube-udn-masq"

	nftablesNodePortsV4    = "nodeports-v4"
	nftablesNodePortsV6    = "nodeports-v6"
	nftablesETPNodePortsV4 = "nodeports-etp-local-v4"
	nftablesETPNodePortsV6 = "nodeports-etp-local-v6"

	nftablesNodePortAllowedV4 = "nodeport-allowed-v4"
	nftablesNodePortAllowedV6 = "nodeport-allowed-v6"

	nftablesExternalIPsV4    = "external-ips-v4"
	nftablesExternalIPsV6    = "external-ips-v6"
	nftablesETPExternalIPsV4 = "external-ips-etp-local-v4"
	nftablesETPExternalIPsV6 = "external-ips-etp-local-v6"

	nftablesETPNoNodePortChain = "services-etp-no-nodeport"

	nftablesITPMarkSetV4     = "itp-services-to-mark-v4"
	nftablesITPMarkSetV6     = "itp-services-to-mark-v6"
	nftablesITPServicesMapV4 = "itp-services-to-redirect-v4"
	nftablesITPServicesMapV6 = "itp-services-to-redirect-v6"
)

// nodePortAddressesRestricted reports whether NodePort traffic is limited to
// specific local node IP addresses.
func nodePortAddressesRestricted() bool {
	return config.Gateway.NodePortAddresses != nil && config.Gateway.NodePortAddresses.Restricted()
}

// addNodePortAllowedAddressSets creates nftables sets that hold the local node
// IPs allowed to receive NodePort traffic.
func addNodePortAllowedAddressSets(tx *knftables.Transaction) {
	tx.Add(&knftables.Set{
		Name:    nftablesNodePortAllowedV4,
		Type:    "ipv4_addr",
		Comment: knftables.PtrTo("Local node IPs allowed to receive NodePort traffic"),
	})
	tx.Flush(&knftables.Set{Name: nftablesNodePortAllowedV4})
	tx.Add(&knftables.Set{
		Name:    nftablesNodePortAllowedV6,
		Type:    "ipv6_addr",
		Comment: knftables.PtrTo("Local node IPs allowed to receive NodePort traffic"),
	})
	tx.Flush(&knftables.Set{Name: nftablesNodePortAllowedV6})
}

// nodePortLocalDestMatch returns the nftables destination match for NodePort
// traffic, optionally restricted to allowed local addresses.
func nodePortLocalDestMatch(isIPv6 bool) []string {
	if !nodePortAddressesRestricted() {
		return []string{"fib daddr type local"}
	}
	if isIPv6 {
		return []string{"fib daddr type local", "ip6 daddr", "@", nftablesNodePortAllowedV6}
	}
	return []string{"fib daddr type local", "ip daddr", "@", nftablesNodePortAllowedV4}
}

// nftConcat builds an nftables concatenation expression from string parts.
func nftConcat(parts ...string) string {
	args := make([]interface{}, len(parts))
	for i, part := range parts {
		args[i] = part
	}
	return knftables.Concat(args...)
}

// SyncNodePortAllowedAddresses updates the nftables sets that restrict which
// local node IPs may receive NodePort traffic.
func SyncNodePortAllowedAddresses(hostAddresses, primaryAddresses []net.IP) error {
	if !nodePortAddressesRestricted() {
		return nil
	}

	filtered := config.Gateway.NodePortAddresses.FilterHostAddresses(hostAddresses, primaryAddresses)
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("get nftables helper for NodePort allowed addresses: %w", err)
	}

	tx := nft.NewTransaction()
	tx.Flush(&knftables.Set{Name: nftablesNodePortAllowedV4})
	tx.Flush(&knftables.Set{Name: nftablesNodePortAllowedV6})
	for _, ip := range filtered {
		if ip == nil {
			continue
		}
		if utilnet.IsIPv6(ip) {
			tx.Add(&knftables.Element{Set: nftablesNodePortAllowedV6, Key: []string{ip.String()}})
		} else {
			tx.Add(&knftables.Element{Set: nftablesNodePortAllowedV4, Key: []string{ip.String()}})
		}
	}
	if err := nft.Run(context.TODO(), tx); err != nil {
		return fmt.Errorf("sync NodePort allowed addresses: %w", err)
	}
	return nil
}

// initGatewayNFTables initializes chains/sets/maps used for Service proxying rules.
//
// FIXME: This function is only called if config.NodeportEnable is true, but it and
// getGatewayNFTRules are also used for `internalTrafficPolicy: Local` handling, which
// should not be gated behind config.NodeportEnable.
func initGatewayNFTables() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()

	tx.Add(&knftables.Chain{
		Name:    "services",
		Comment: knftables.PtrTo("DNAT for ordinary NodePort/ExternalIP/LB traffic"),
	})
	tx.Flush(&knftables.Chain{
		Name: "services",
	})
	tx.Add(&knftables.Chain{
		Name:    "services-etp",
		Comment: knftables.PtrTo("Special DNAT for NodePort/ExternalIP/LB traffic with ExternalTrafficPolicy: Local"),
	})
	tx.Flush(&knftables.Chain{
		Name: "services-etp",
	})
	tx.Add(&knftables.Chain{
		Name:    nftablesETPNoNodePortChain,
		Comment: knftables.PtrTo("Special DNAT for ExternalIP/LB traffic with ExternalTrafficPolicy: Local and no NodePorts"),
	})
	tx.Flush(&knftables.Chain{
		Name: nftablesETPNoNodePortChain,
	})
	tx.Add(&knftables.Chain{
		Name:    "services-itp",
		Comment: knftables.PtrTo("Redirects for traffic with InternalTrafficPolicy: Local"),
	})
	tx.Flush(&knftables.Chain{
		Name: "services-itp",
	})

	// Create chains that hook to netfilter and invoke our service chains as appropriate.
	tx.Add(&knftables.Chain{
		Name:     "services-prerouting",
		Type:     knftables.PtrTo(knftables.NATType),
		Hook:     knftables.PtrTo(knftables.PreroutingHook),
		Priority: knftables.PtrTo(knftables.DNATPriority),
	})
	tx.Flush(&knftables.Chain{
		Name: "services-prerouting",
	})
	tx.Add(&knftables.Rule{
		Chain: "services-prerouting",
		Rule:  "jump services-etp",
	})
	tx.Add(&knftables.Rule{
		Chain: "services-prerouting",
		Rule: knftables.Concat(
			"jump", nftablesETPNoNodePortChain,
		),
	})
	tx.Add(&knftables.Rule{
		Chain: "services-prerouting",
		Rule:  "jump services",
	})
	tx.Add(&knftables.Chain{
		Name:     "services-output",
		Type:     knftables.PtrTo(knftables.NATType),
		Hook:     knftables.PtrTo(knftables.OutputHook),
		Priority: knftables.PtrTo(knftables.DNATPriority),
	})
	tx.Flush(&knftables.Chain{
		Name: "services-output",
	})
	tx.Add(&knftables.Rule{
		Chain: "services-output",
		Rule:  "jump services-itp",
	})
	tx.Add(&knftables.Rule{
		Chain: "services-output",
		Rule:  "jump services",
	})

	// Create the maps and rules for ETP:Local services
	tx.Add(&knftables.Map{
		Name:    nftablesETPExternalIPsV4,
		Type:    "ipv4_addr . inet_proto . inet_service : ipv4_addr . inet_service",
		Comment: knftables.PtrTo("DNAT mappings for IPv4 ExternalIP/LB traffic with ExternalTrafficPolicy: Local"),
	})
	tx.Add(&knftables.Map{
		Name:    nftablesETPExternalIPsV6,
		Type:    "ipv6_addr . inet_proto . inet_service : ipv6_addr . inet_service",
		Comment: knftables.PtrTo("DNAT mappings for IPv6 ExternalIP/LB traffic with ExternalTrafficPolicy: Local"),
	})
	tx.Add(&knftables.Map{
		Name:    nftablesETPNodePortsV4,
		Type:    "inet_proto . inet_service : ipv4_addr . inet_service",
		Comment: knftables.PtrTo("DNAT mappings for IPv4 NodePort traffic with ExternalTrafficPolicy: Local"),
	})
	tx.Add(&knftables.Map{
		Name:    nftablesETPNodePortsV6,
		Type:    "inet_proto . inet_service : ipv6_addr . inet_service",
		Comment: knftables.PtrTo("DNAT mappings for IPv6 NodePort traffic with ExternalTrafficPolicy: Local"),
	})
	if nodePortAddressesRestricted() {
		addNodePortAllowedAddressSets(tx)
	}
	tx.Add(&knftables.Rule{
		Chain: "services-etp",
		Rule: knftables.Concat(
			"dnat ip addr . port to ",
			"ip daddr . meta l4proto . th dport map", "@", nftablesETPExternalIPsV4,
		),
	})
	tx.Add(&knftables.Rule{
		Chain: "services-etp",
		Rule: knftables.Concat(
			"dnat ip6 addr . port to ",
			"ip6 daddr . meta l4proto . th dport map", "@", nftablesETPExternalIPsV6,
		),
	})
	tx.Add(&knftables.Rule{
		Chain: "services-etp",
		Rule: nftConcat(append(nodePortLocalDestMatch(false),
			"dnat ip addr . port to",
			"meta l4proto . th dport map", "@", nftablesETPNodePortsV4)...),
	})
	tx.Add(&knftables.Rule{
		Chain: "services-etp",
		Rule: nftConcat(append(nodePortLocalDestMatch(true),
			"dnat ip6 addr . port to",
			"meta l4proto . th dport map", "@", nftablesETPNodePortsV6)...),
	})

	// Create the maps and rules for ordinary (ETP:Cluster) services
	tx.Add(&knftables.Map{
		Name:    nftablesExternalIPsV4,
		Type:    "ipv4_addr . inet_proto . inet_service : ipv4_addr . inet_service",
		Comment: knftables.PtrTo("DNAT mappings for ordinary IPv4 ExternalIP/LB traffic"),
	})
	tx.Add(&knftables.Map{
		Name:    nftablesExternalIPsV6,
		Type:    "ipv6_addr . inet_proto . inet_service : ipv6_addr . inet_service",
		Comment: knftables.PtrTo("DNAT mappings for ordinary IPv6 ExternalIP/LB traffic"),
	})
	tx.Add(&knftables.Map{
		Name:    nftablesNodePortsV4,
		Type:    "inet_proto . inet_service : ipv4_addr . inet_service",
		Comment: knftables.PtrTo("DNAT mappings for ordinary IPv4 NodePort traffic"),
	})
	tx.Add(&knftables.Map{
		Name:    nftablesNodePortsV6,
		Type:    "inet_proto . inet_service : ipv6_addr . inet_service",
		Comment: knftables.PtrTo("DNAT mappings for ordinary IPv6 NodePort traffic"),
	})

	tx.Add(&knftables.Rule{
		Chain: "services",
		Rule: knftables.Concat(
			"dnat ip to ",
			"ip daddr . meta l4proto . th dport map", "@", nftablesExternalIPsV4,
		),
	})
	tx.Add(&knftables.Rule{
		Chain: "services",
		Rule: knftables.Concat(
			"dnat ip6 to ",
			"ip6 daddr . meta l4proto . th dport map", "@", nftablesExternalIPsV6,
		),
	})
	tx.Add(&knftables.Rule{
		Chain: "services",
		Rule: nftConcat(append(nodePortLocalDestMatch(false),
			"dnat ip addr . port to",
			"meta l4proto . th dport map", "@", nftablesNodePortsV4)...),
	})
	tx.Add(&knftables.Rule{
		Chain: "services",
		Rule: nftConcat(append(nodePortLocalDestMatch(true),
			"dnat ip6 addr . port to",
			"meta l4proto . th dport map", "@", nftablesNodePortsV6)...),
	})

	// Add the remaining chains/sets/maps for InternalTrafficPolicy: Local.
	tx.Add(&knftables.Set{
		Name:    nftablesITPMarkSetV4,
		Type:    "ipv4_addr . inet_proto . inet_service",
		Comment: knftables.PtrTo("InternalTrafficPolicy: Local traffic to mark for special routing"),
	})
	tx.Add(&knftables.Set{
		Name:    nftablesITPMarkSetV6,
		Type:    "ipv6_addr . inet_proto . inet_service",
		Comment: knftables.PtrTo("InternalTrafficPolicy: Local traffic to mark for special routing"),
	})
	tx.Add(&knftables.Map{
		Name:    nftablesITPServicesMapV4,
		Type:    "ipv4_addr . inet_proto . inet_service : inet_service",
		Comment: knftables.PtrTo("Port redirections for ordinary InternalTrafficPolicy: Local traffic"),
	})
	tx.Add(&knftables.Map{
		Name:    nftablesITPServicesMapV6,
		Type:    "ipv6_addr . inet_proto . inet_service : inet_service",
		Comment: knftables.PtrTo("Port redirections for ordinary InternalTrafficPolicy: Local traffic"),
	})
	tx.Add(&knftables.Rule{
		Chain: "services-itp",
		Rule: knftables.Concat(
			"meta l4proto { tcp, udp, sctp } redirect to ip daddr . meta l4proto . th dport map", "@", nftablesITPServicesMapV4,
		),
	})
	tx.Add(&knftables.Rule{
		Chain: "services-itp",
		Rule: knftables.Concat(
			"meta l4proto { tcp, udp, sctp } redirect to ip6 daddr . meta l4proto . th dport map", "@", nftablesITPServicesMapV6,
		),
	})

	tx.Add(&knftables.Chain{
		Name:     "services-itp-mark",
		Type:     knftables.PtrTo(knftables.RouteType),
		Hook:     knftables.PtrTo(knftables.OutputHook),
		Priority: knftables.PtrTo(knftables.ManglePriority),
		Comment:  knftables.PtrTo("Chain to mark InternalTrafficPolicy: Local traffic for special routing"),
	})
	tx.Flush(&knftables.Chain{
		Name: "services-itp-mark",
	})
	tx.Add(&knftables.Rule{
		Chain: "services-itp-mark",
		Rule: knftables.Concat(
			"ip daddr . meta l4proto . th dport", "@", nftablesITPMarkSetV4,
			"mark set", types.OVNKubeITPMark,
		),
	})
	tx.Add(&knftables.Rule{
		Chain: "services-itp-mark",
		Rule: knftables.Concat(
			"ip6 daddr . meta l4proto . th dport", "@", nftablesITPMarkSetV6,
			"mark set", types.OVNKubeITPMark,
		),
	})

	if err := nft.Run(context.TODO(), tx); err != nil {
		return fmt.Errorf("could not set up service nftables rules: %w", err)
	}

	// If there are legacy IPTables rules left around, try to clean them up.
	cleanupGatewayIPTables()

	return nil
}

// getNodePortNFTRules returns the nftables DNAT rules for a service of type nodePort.
// `svcPort` corresponds to port details for this service as specified in the service object.
// `targetIP` is the clusterIP towards which the DNAT of nodePort service is to be added.
// `targetPort` is the port towards which the DNAT of the nodePort service is to be added
//
//	case1: if svcHasLocalHostNetEndPnt=false + isETPLocal=true targetIP=config.masqueradeIP["HostETPLocalMasqueradeIP"] and targetPort=svcPort.NodePort
//	case2: default: targetIP=clusterIP and targetPort=svcPort.Port
//
// `svcHasLocalHostNetEndPnt` is true if this service has at least one host-networked endpoint that is local to this node
// `isETPLocal` is true if the svc.Spec.ExternalTrafficPolicy=Local
func getNodePortNFTRules(svcPort corev1.ServicePort, targetIP string, targetPort int32, svcHasLocalHostNetEndPnt, isETPLocal bool) []knftables.Object {
	var mapName string
	if !svcHasLocalHostNetEndPnt && isETPLocal {
		// DNAT it to the masqueradeIP:nodePort instead of clusterIP:targetPort
		targetIP = getMasqueradeVIP(targetIP)
		if utilnet.IsIPv4String(targetIP) {
			mapName = nftablesETPNodePortsV4
		} else {
			mapName = nftablesETPNodePortsV6
		}
	} else {
		if utilnet.IsIPv4String(targetIP) {
			mapName = nftablesNodePortsV4
		} else {
			mapName = nftablesNodePortsV6
		}
	}

	return []knftables.Object{
		&knftables.Element{
			Map: mapName,
			Key: []string{
				strings.ToLower(string(svcPort.Protocol)),
				fmt.Sprintf("%d", svcPort.NodePort),
			},
			Value: []string{
				targetIP,
				fmt.Sprintf("%d", targetPort),
			},
		},
	}
}

func generateNFTRulesForLoadBalancersWithoutNodePorts(service *corev1.Service, svcPort corev1.ServicePort, externalIP string, localEndpoints util.PortToLBEndpoints) []knftables.Object {
	// Get the endpoints for the port key.
	// svcPortKey is of format e.g. "TCP/my-port-name" or "TCP/" if name is empty
	// (is the case when only a single ServicePort is defined on this service).
	svcPortKey := util.GetServicePortKey(svcPort.Protocol, svcPort.Name)
	lbEndpoints := localEndpoints[svcPortKey]

	// Get IPv4 or IPv6 IPs, depending on the type of the service's external IP.
	destinations := lbEndpoints.GetV4Destinations()
	if utilnet.IsIPv6String(externalIP) {
		destinations = lbEndpoints.GetV6Destinations()
	}
	numLocalEndpoints := len(destinations)
	if numLocalEndpoints == 0 {
		return nil
	}

	ipX := "ip"
	if utilnet.IsIPv6String(externalIP) {
		ipX = "ip6"
	}
	comment := service.Namespace + "/" + service.Name + ":" + svcPort.Name

	// Build comma-separated list of "NUMBER : IP" mappings
	mappings := make([]string, 0, numLocalEndpoints*2)
	for i, destination := range destinations {
		if i != 0 {
			mappings = append(mappings, ",")
		}
		mappings = append(mappings, fmt.Sprintf("%d : %s . %d", i, destination.IP, destination.Port))
	}

	// There's no good way to do this with a single static rule and only add and
	// remove set/map elements like we do for everything else. In particular, you
	// can't have nested maps, so we'd have to have the "ip . protocol . port" and the
	// random endpoint number be in the same map. (So we'd have to have one map and
	// rule for single-endpoint services, another map and rule for 2-endpoint
	// services, etc.)
	//
	// If the O(n)-ness here turns out to be a problem, an alternative implementation
	// would be to have a vmap from "ip . protocol . port" to a jump rule, and then
	// have each Service have its own chain with a single "dnat to random map entry"
	// rule. That would be slightly more complicated to manage, but it would make it
	// O(1)...
	return []knftables.Object{
		&knftables.Rule{
			Chain: nftablesETPNoNodePortChain,
			Rule: knftables.Concat(
				ipX, "daddr", externalIP,
				"meta l4proto", strings.ToLower(string(svcPort.Protocol)),
				"th dport", svcPort.Port,
				"dnat", ipX, "addr . port to",
				"numgen random mod", numLocalEndpoints,
				"map {", mappings, "}",
			),
			Comment: &comment,
		},
	}
}

// getExternalIPNFTRules returns the nftables DNAT rules for a service of type LB or ExternalIP
// `svcPort` corresponds to port details for this service as specified in the service object
// `externalIP` can either be the externalIP or LB.status.ingressIP
// `dstIP` corresponds to the IP to which the provided externalIP needs to be DNAT-ed to
//
//	case1: if svcHasLocalHostNetEndPnt=false + isETPLocal=true, dstIP=config.MasqueradeIP["HostETPLocalMasqueradeIP"]
//	case2: default: dstIP=clusterIP
//
// `svcHasLocalHostNetEndPnt` is true if this service has at least one host-networked endpoint that is local to this node
// `isETPLocal` is true if the svc.Spec.ExternalTrafficPolicy=Local
func getExternalIPNFTRules(svcPort corev1.ServicePort, externalIP, dstIP string, svcHasLocalHostNetEndPnt, isETPLocal bool) []knftables.Object {
	var mapName string
	targetPort := svcPort.Port
	if !svcHasLocalHostNetEndPnt && isETPLocal {
		// DNAT it to the masqueradeIP:nodePort instead of clusterIP:targetPort
		dstIP = getMasqueradeVIP(externalIP)
		targetPort = svcPort.NodePort
		if utilnet.IsIPv4String(dstIP) {
			mapName = nftablesETPExternalIPsV4
		} else {
			mapName = nftablesETPExternalIPsV6
		}
	} else {
		if utilnet.IsIPv4String(dstIP) {
			mapName = nftablesExternalIPsV4
		} else {
			mapName = nftablesExternalIPsV6
		}
	}

	return []knftables.Object{
		&knftables.Element{
			Map: mapName,
			Key: []string{
				externalIP,
				strings.ToLower(string(svcPort.Protocol)),
				fmt.Sprintf("%d", svcPort.Port),
			},
			Value: []string{
				dstIP,
				fmt.Sprintf("%d", targetPort),
			},
		},
	}
}

// getITPLocalNFTRules returns the `InternalTrafficPolicy: Local`-related rules for the provided service
// `svcPort` corresponds to port details for this service as specified in the service object
// `clusterIP` is clusterIP is the VIP of the service to match on
// `svcHasLocalHostNetEndPnt` is true if this service has at least one host-networked endpoint that is local to this node
func getITPLocalNFTRules(svcPort corev1.ServicePort, clusterIP string, svcHasLocalHostNetEndPnt bool) []knftables.Object {
	// If the service has a local host-network endpoint, add a rule that will cause
	// traffic to the ITP service from this host to be redirected to that endpoint.
	if svcHasLocalHostNetEndPnt {
		var mapName string
		if utilnet.IsIPv4String(clusterIP) {
			mapName = nftablesITPServicesMapV4
		} else {
			mapName = nftablesITPServicesMapV6
		}
		return []knftables.Object{
			&knftables.Element{
				Map: mapName,
				Key: []string{
					clusterIP,
					strings.ToLower(string(svcPort.Protocol)),
					fmt.Sprintf("%v", svcPort.Port),
				},
				Value: []string{
					fmt.Sprintf("%v", int32(svcPort.TargetPort.IntValue())),
				},
			},
		}
	}

	// Otherwise, add a rule that will cause traffic to the ITP service from this host
	// to be marked (which will then cause it to be steered to ovn-k8s-mp0).
	var setName string
	if utilnet.IsIPv4String(clusterIP) {
		setName = nftablesITPMarkSetV4
	} else {
		setName = nftablesITPMarkSetV6
	}
	return []knftables.Object{
		&knftables.Element{
			Set: setName,
			Key: []string{
				clusterIP,
				strings.ToLower(string(svcPort.Protocol)),
				fmt.Sprintf("%v", svcPort.Port),
			},
		},
	}
}

// getNoSNATNodePortRules returns elements to add to the "mgmtport-no-snat-nodeports"
// set to prevent SNAT of sourceIP when passing through the management port, for an
// `externalTrafficPolicy: Local` service with NodePorts.
func getNoSNATNodePortRules(svcPort corev1.ServicePort) []knftables.Object {
	return []knftables.Object{
		&knftables.Element{
			Set: types.NFTMgmtPortNoSNATNodePorts,
			Key: []string{
				strings.ToLower(string(svcPort.Protocol)),
				fmt.Sprintf("%d", svcPort.NodePort),
			},
		},
	}
}

// getNoSNATLoadBalancerIPRules returns elements to add to the
// "mgmtport-no-snat-services-v4" and "mgmtport-no-snat-services-v6" sets to prevent SNAT
// of sourceIP when passing through the management port, for an `externalTrafficPolicy:
// Local` service *without* NodePorts.
func getNoSNATLoadBalancerIPRules(svcPort corev1.ServicePort, localEndpoints util.PortToLBEndpoints) []knftables.Object {
	var nftRules []knftables.Object
	protocol := strings.ToLower(string(svcPort.Protocol))

	// Get the endpoints for the port key.
	// svcPortKey is of format e.g. "TCP/my-port-name" or "TCP/" if name is empty
	// (is the case when only a single ServicePort is defined on this service).
	svcPortKey := util.GetServicePortKey(svcPort.Protocol, svcPort.Name)
	lbEndpoints := localEndpoints[svcPortKey]

	for _, destination := range lbEndpoints.GetV4Destinations() {
		nftRules = append(nftRules,
			&knftables.Element{
				Set: types.NFTMgmtPortNoSNATServicesV4,
				Key: []string{destination.IP, protocol, fmt.Sprintf("%d", destination.Port)},
			},
		)
	}

	for _, destination := range lbEndpoints.GetV6Destinations() {
		nftRules = append(nftRules,
			&knftables.Element{
				Set: types.NFTMgmtPortNoSNATServicesV6,
				Key: []string{destination.IP, protocol, fmt.Sprintf("%d", destination.Port)},
			},
		)
	}

	return nftRules
}

// getUDNNodePortMarkNFTRule returns a verdict map element (nftablesUDNMarkNodePortsMap)
// with a key composed of the svcPort protocol and port.
// The value is a jump to the UDN chain mark if netInfo is provided, or nil that is useful for map entry removal.
func getUDNNodePortMarkNFTRule(svcPort corev1.ServicePort, netInfo *bridgeconfig.BridgeUDNConfiguration) *knftables.Element {
	var val []string
	if netInfo != nil {
		val = []string{fmt.Sprintf("jump %s", GetUDNMarkChain(netInfo.PktMark))}
	}
	return &knftables.Element{
		Map:   nftablesUDNMarkNodePortsMap,
		Key:   []string{strings.ToLower(string(svcPort.Protocol)), fmt.Sprintf("%v", svcPort.NodePort)},
		Value: val,
	}

}

// getUDNExternalIPsMarkNFTRules returns a verdict map elements (nftablesUDNMarkExternalIPsV4Map or nftablesUDNMarkExternalIPsV6Map)
// with a key composed of the external IP, svcPort protocol and port.
// The value is a jump to the UDN chain mark if netInfo is provided,  or nil that is useful for map entry removal.
func getUDNExternalIPsMarkNFTRules(svcPort corev1.ServicePort, externalIPs []string, netInfo *bridgeconfig.BridgeUDNConfiguration) []knftables.Object {
	var nftRules []knftables.Object
	var val []string

	if netInfo != nil {
		val = []string{fmt.Sprintf("jump %s", GetUDNMarkChain(netInfo.PktMark))}
	}
	for _, externalIP := range externalIPs {
		mapName := nftablesUDNMarkExternalIPsV4Map
		if utilnet.IsIPv6String(externalIP) {
			mapName = nftablesUDNMarkExternalIPsV6Map
		}
		nftRules = append(nftRules,
			&knftables.Element{
				Map:   mapName,
				Key:   []string{externalIP, strings.ToLower(string(svcPort.Protocol)), fmt.Sprintf("%v", svcPort.Port)},
				Value: val,
			},
		)

	}
	return nftRules
}

// getGatewayNFTRules returns nftables rules for service.
//
// FIXME: This function is only called if config.NodeportEnable is true, but it is also
// used for `internalTrafficPolicy: Local` handling, which should not be gated behind
// config.NodeportEnable.
func getGatewayNFTRules(service *corev1.Service, localEndpoints util.PortToLBEndpoints, svcHasLocalHostNetEndPnt bool) []knftables.Object {
	rules := make([]knftables.Object, 0)
	clusterIPs := util.GetClusterIPs(service)
	svcTypeIsETPLocal := util.ServiceExternalTrafficPolicyLocal(service)
	svcTypeIsITPLocal := util.ServiceInternalTrafficPolicyLocal(service)
	for _, svcPort := range service.Spec.Ports {
		if util.ServiceTypeHasNodePort(service) {
			err := util.ValidatePort(svcPort.Protocol, svcPort.NodePort)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service NodePort: %v", svcPort.Name, err)
				continue
			}
			err = util.ValidatePort(svcPort.Protocol, svcPort.Port)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service port %v", svcPort.Name, err)
				continue
			}
			for _, clusterIP := range clusterIPs {
				if svcTypeIsETPLocal && !svcHasLocalHostNetEndPnt {
					// case1 (see function description for details)
					// A DNAT rule to masqueradeIP is added that takes priority over DNAT to clusterIP.
					if config.Gateway.Mode == config.GatewayModeLocal {
						rules = append(rules, getNodePortNFTRules(svcPort, clusterIP, svcPort.NodePort, svcHasLocalHostNetEndPnt, svcTypeIsETPLocal)...)
					}
					// add a skip SNAT rule to preserve sourceIP for etp=local traffic
					rules = append(rules, getNoSNATNodePortRules(svcPort)...)
				}
				// case2 (see function description for details)
				rules = append(rules, getNodePortNFTRules(svcPort, clusterIP, svcPort.Port, svcHasLocalHostNetEndPnt, false)...)
			}
		}

		snatRulesCreated := false
		externalIPs := util.GetExternalAndLBIPs(service)
		for _, externalIP := range externalIPs {
			err := util.ValidatePort(svcPort.Protocol, svcPort.Port)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service port %v", svcPort.Name, err)
				continue
			}
			if clusterIP, err := util.MatchIPStringFamily(utilnet.IsIPv6String(externalIP), clusterIPs); err == nil {
				if svcTypeIsETPLocal && !svcHasLocalHostNetEndPnt {
					// case1 (see function description for details)
					// DNAT traffic to masqueradeIP:nodePort instead of clusterIP:Port. We are leveraging the existing rules for NODEPORT
					// service so no need to add a rule to skip SNAT since the corresponding nodePort svc would have one.
					if !util.ServiceTypeHasNodePort(service) {
						rules = append(rules, generateNFTRulesForLoadBalancersWithoutNodePorts(service, svcPort, externalIP, localEndpoints)...)
						// The SNAT rules are per endpoint and should only be created one time per endpoint and port combination
						if !snatRulesCreated {
							rules = append(rules, getNoSNATLoadBalancerIPRules(svcPort, localEndpoints)...)
							snatRulesCreated = true
						}
					} else {
						rules = append(rules, getExternalIPNFTRules(svcPort, externalIP, "", svcHasLocalHostNetEndPnt, svcTypeIsETPLocal)...)
					}
				}
				// case2 (see function description for details)
				rules = append(rules, getExternalIPNFTRules(svcPort, externalIP, clusterIP, svcHasLocalHostNetEndPnt, false)...)
			}
		}

		if svcTypeIsITPLocal {
			for _, clusterIP := range clusterIPs {
				rules = append(rules, getITPLocalNFTRules(svcPort, clusterIP, svcHasLocalHostNetEndPnt)...)
			}
		}
	}
	return rules
}

// getGatewayNFTContainerObjects returns all of the "container" objects (Sets/Maps/Chains)
// used by getGatewayNFTRules. This is used (possibly along with
// getUDNNFTContainerObjects) to determine the sets/maps/chains whose contents will be
// synchronized by nodePortWatcher.SyncServices().
func getGatewayNFTContainerObjects() []knftables.Object {
	return []knftables.Object{
		&knftables.Set{
			Name: types.NFTMgmtPortNoSNATNodePorts,
		},
		&knftables.Set{
			Name: types.NFTMgmtPortNoSNATServicesV4,
		},
		&knftables.Set{
			Name: types.NFTMgmtPortNoSNATServicesV6,
		},
		&knftables.Map{
			Name: nftablesNodePortsV4,
		},
		&knftables.Map{
			Name: nftablesNodePortsV6,
		},
		&knftables.Map{
			Name: nftablesETPNodePortsV4,
		},
		&knftables.Map{
			Name: nftablesETPNodePortsV6,
		},
		&knftables.Map{
			Name: nftablesExternalIPsV4,
		},
		&knftables.Map{
			Name: nftablesExternalIPsV6,
		},
		&knftables.Map{
			Name: nftablesETPExternalIPsV4,
		},
		&knftables.Map{
			Name: nftablesETPExternalIPsV6,
		},
		&knftables.Chain{
			Name: nftablesETPNoNodePortChain,
		},
		&knftables.Set{
			Name: nftablesITPMarkSetV4,
		},
		&knftables.Set{
			Name: nftablesITPMarkSetV6,
		},
		&knftables.Map{
			Name: nftablesITPServicesMapV4,
		},
		&knftables.Map{
			Name: nftablesITPServicesMapV6,
		},
	}
}

// getUDNNFTRules generates nftables rules for a UDN service.
// If netConfig is nil, the resulting map elements will have empty values,
// suitable only for entry removal.
func getUDNNFTRules(service *corev1.Service, netConfig *bridgeconfig.BridgeUDNConfiguration) []knftables.Object {
	rules := make([]knftables.Object, 0)
	for _, svcPort := range service.Spec.Ports {
		if util.ServiceTypeHasNodePort(service) {
			rules = append(rules, getUDNNodePortMarkNFTRule(svcPort, netConfig))
		}
		rules = append(rules, getUDNExternalIPsMarkNFTRules(svcPort, util.GetExternalAndLBIPs(service), netConfig)...)
	}
	return rules
}

func shouldAddUDNServiceMarkRules(netInfo util.NetInfo, nodeName string) bool {
	if !netInfo.IsPrimaryNetwork() {
		return false
	}
	advertisedVRFs := netInfo.GetPodNetworkAdvertisedOnNodeVRFs(nodeName)
	if len(advertisedVRFs) == 0 {
		return true
	}
	// The default network name represents the default VRF target here, not the
	// default pod network. Only install host service mark rules when the UDN is
	// reachable through the default VRF path; VRF-lite Uplink targets must not
	// be reachable from the default gateway interface.
	return slices.Contains(advertisedVRFs, types.DefaultNetworkName)
}

// getUDNNFTContainerObjects returns all of the "container" objects (Sets/Maps/Chains)
// used by getUDNNFTRules. This is used (possibly along with
// getGatewayNFTContainerObjects) to determine the sets/maps/chains whose contents will be
// synchronized by nodePortWatcher.SyncServices().
func getUDNNFTContainerObjects() []knftables.Object {
	return []knftables.Object{
		&knftables.Map{
			Name: nftablesUDNMarkNodePortsMap,
		},
		&knftables.Map{
			Name: nftablesUDNMarkExternalIPsV4Map,
		},
		&knftables.Map{
			Name: nftablesUDNMarkExternalIPsV6Map,
		},
	}
}

// getLocalGatewayPodSubnetMasqueradeNFTRule creates rules for masquerading traffic from the pod subnet CIDR
// in local gateway node in a separate chain which is then called from local gateway masquerade chain.
//
//	chain ovn-kube-pod-subnet-masq {
//		ip saddr 10.244.0.0/24 masquerade
//		ip6 saddr fd00:10:244:1::/64 masquerade
//	}
//
// If isAdvertisedNetwork is true, masquerade only when destination matches remote node IPs.
// Rules look like:
// ip saddr 10.244.0.0/24 ip daddr @remote-node-ips-v4 masquerade
// ip6 saddr fd00:10:244:1::/64 ip6 daddr @remote-node-ips-v6 masquerade
//
// For no-overlay mode with outboundSNAT enabled, returns two rules:
// 1. A return rule to exempt traffic to cluster CIDRs + local node IPs from SNAT
// 2. A masquerade rule for all other traffic
// Example for outboundSNAT=enabled:
//
//	chain ovn-kube-pod-subnet-masq {
//		ip saddr 10.128.0.0/24 ip daddr @no-overlay-snat-exempt-v4 return
//		ip saddr 10.128.0.0/24 masquerade
//		ip6 saddr fd00:10:244:1::/64 ip6 daddr @no-overlay-snat-exempt-v6 return
//		ip6 saddr fd00:10:244:1::/64 masquerade
//	}
func getLocalGatewayPodSubnetMasqueradeNFTRule(cidr *net.IPNet, isAdvertisedNetwork bool) ([]*knftables.Rule, error) {
	// Create the rule for masquerading traffic from the CIDR
	var ipPrefix string
	var remoteNodeSetName string
	if utilnet.IsIPv6CIDR(cidr) {
		ipPrefix = "ip6"
		remoteNodeSetName = types.NFTRemoteNodeIPsv6
	} else {
		ipPrefix = "ip"
		remoteNodeSetName = types.NFTRemoteNodeIPsv4
	}

	var rules []*knftables.Rule

	// For no-overlay mode with outboundSNAT enabled, use simple logic:
	// 1. Return rule to skip SNAT for traffic to exempted destinations (cluster CIDRs + local node IPs)
	// 2. Unconditional masquerade for everything else (pod-to-external and pod-to-remote-node)
	if config.Default.Transport == types.NetworkTransportNoOverlay && config.NoOverlay.OutboundSNAT == types.NoOverlaySNATEnabled {
		var snatExemptSetName string
		if utilnet.IsIPv6CIDR(cidr) {
			snatExemptSetName = types.NFTNoOverlaySNATExemptV6
		} else {
			snatExemptSetName = types.NFTNoOverlaySNATExemptV4
		}

		// Add return rule to skip SNAT for traffic to exempted destinations
		// The set contains cluster CIDRs + local node IPs
		returnRule := &knftables.Rule{
			Rule: knftables.Concat(
				ipPrefix, "saddr", cidr,
				ipPrefix, "daddr", "@", snatExemptSetName,
				"return",
			),
			Chain: nftablesPodSubnetMasqChain,
		}
		rules = append(rules, returnRule)

		// Add unconditional masquerade rule for all other traffic
		masqRule := &knftables.Rule{
			Rule: knftables.Concat(
				ipPrefix, "saddr", cidr,
				"masquerade",
			),
			Chain: nftablesPodSubnetMasqChain,
		}
		rules = append(rules, masqRule)
		return rules, nil
	}

	// For BGP advertised networks (without outboundSNAT enabled):
	// Only masquerade traffic to remote node IPs (for NodePort services)
	// All other traffic (pod-to-pod and pod-to-external) is not SNATed
	var optionalDestRules []string
	if isAdvertisedNetwork {
		optionalDestRules = []string{ipPrefix, "daddr", "@", remoteNodeSetName}
	}
	masqRule := &knftables.Rule{
		Rule: knftables.Concat(
			ipPrefix, "saddr", cidr,
			optionalDestRules,
			"masquerade",
		),
		Chain: nftablesPodSubnetMasqChain,
	}
	rules = append(rules, masqRule)

	return rules, nil
}

// getLocalGatewayNATNFTRules returns the nftables rules for local gateway NAT including masquerade IP rule,
// pod subnet rules, and UDN masquerade rules (if network segmentation is enabled).
// This function supports dual-stack by accepting multiple CIDRs and generating rules for all IP families.
//
//	chain ovn-kube-local-gw-masq {
//		comment "OVN local gateway masquerade"
//		type nat hook postrouting priority srcnat; policy accept;
//		ip saddr 169.254.0.1 masquerade
//		ip6 saddr fd69::1 masquerade
//		jump ovn-kube-pod-subnet-masq
//		jump ovn-kube-udn-masq
//	}
func getLocalGatewayNATNFTRules(cidrs ...*net.IPNet) ([]*knftables.Rule, error) {
	var rules []*knftables.Rule

	// Process each CIDR to support dual-stack
	for _, cidr := range cidrs {
		// Determine IP version and masquerade IP
		isIPv6 := utilnet.IsIPv6CIDR(cidr)
		var masqueradeIP net.IP
		var ipPrefix string
		if isIPv6 {
			masqueradeIP = config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP
			ipPrefix = "ip6"
		} else {
			masqueradeIP = config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP
			ipPrefix = "ip"
		}

		// Rule1: Masquerade IP rule for the main chain
		masqRule := &knftables.Rule{
			Chain: nftablesLocalGatewayMasqChain,
			Rule: knftables.Concat(
				ipPrefix, "saddr", masqueradeIP,
				"masquerade",
			),
		}
		rules = append(rules, masqRule)

		// Rule2: Pod subnet NAT rule for the pod subnet chain
		podSubnetRules, err := getLocalGatewayPodSubnetMasqueradeNFTRule(cidr, false)
		if err != nil {
			return nil, fmt.Errorf("failed to create pod subnet masquerade rule: %w", err)
		}
		rules = append(rules, podSubnetRules...)
	}

	// Rule 3: UDN masquerade rules (if network segmentation is enabled)
	if util.IsNetworkSegmentationSupportEnabled() {
		if config.IPv4Mode {
			udnRules, err := getUDNMasqueradeNFTRules(utilnet.IPv4)
			if err != nil {
				return nil, fmt.Errorf("failed to create IPv4 UDN masquerade rules: %w", err)
			}
			rules = append(rules, udnRules...)
		}
		if config.IPv6Mode {
			udnRules, err := getUDNMasqueradeNFTRules(utilnet.IPv6)
			if err != nil {
				return nil, fmt.Errorf("failed to create IPv6 UDN masquerade rules: %w", err)
			}
			rules = append(rules, udnRules...)
		}
	}

	return rules, nil
}

// getUDNMasqueradeNFTRules returns the nftables rules for UDN masquerade.
// Chain creation is handled separately by setupLocalGatewayNATNFTRules.
//
//	chain ovn-kube-udn-masq {
//		comment "OVN UDN masquerade"
//		ip saddr != 169.254.0.0/29 ip daddr != 10.96.0.0/16 ip saddr 169.254.0.0/17 masquerade
//		ip6 saddr != fd69::/125 ip daddr != fd00:10:96::/112 ip6 saddr fd69::/112 masquerade
//	}
func getUDNMasqueradeNFTRules(ipFamily utilnet.IPFamily) ([]*knftables.Rule, error) {
	var rules []*knftables.Rule

	// Determine subnet and IP family
	srcUDNMasqueradePrefix := config.Gateway.V4MasqueradeSubnet
	ipPrefix := "ip"
	if ipFamily == utilnet.IPv6 {
		srcUDNMasqueradePrefix = config.Gateway.V6MasqueradeSubnet
		ipPrefix = "ip6"
	}

	// Calculate reserved masquerade prefix (first 8 IPs)
	_, ipnet, err := net.ParseCIDR(srcUDNMasqueradePrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to parse UDN masquerade subnet: %w", err)
	}
	_, prefixLen := ipnet.Mask.Size()
	defaultNetworkReservedMasqueradePrefix := fmt.Sprintf("%s/%d", ipnet.IP.String(), prefixLen-3)

	// Rule: RETURN for reserved masquerade prefix and service CIDRs
	// rest of the traffic is masqueraded

	for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
		if utilnet.IPFamilyOfCIDR(svcCIDR) != ipFamily {
			continue
		}
		masqueradeRule := &knftables.Rule{
			Chain: nftablesUDNMasqChain,
			Rule: knftables.Concat(
				ipPrefix, "saddr", "!=", defaultNetworkReservedMasqueradePrefix, // this guarantees we don't SNAT default network masqueradeIPs
				ipPrefix, "daddr", "!=", svcCIDR, // this guarantees we don't SNAT service traffic
				ipPrefix, "saddr", srcUDNMasqueradePrefix, // this guarantees we SNAT all UDN MasqueradeIPs traffic leaving the node
				"masquerade",
			),
		}
		rules = append(rules, masqueradeRule)
	}

	return rules, nil
}

// initLocalGatewayNFTNATRules sets up nftables rules for local gateway NAT functionality
// This function supports dual-stack by accepting multiple CIDRs and generating rules for all IP families
func initLocalGatewayNFTNATRules(cidrs ...*net.IPNet) error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	// Create transaction and apply all chains and rules
	tx := nft.NewTransaction()

	// Create main local gateway masquerade chain
	// TODO: now that EgressIP uses nftables maps instead of iptables, this
	// priority bump to 101 may no longer be necessary. Investigate using
	// the standard SNATPriority (100) instead.
	// Use priority 101 instead of defaultknftables.SNATPriority (100) to ensure
	// iptables egress IP rules in OVN-KUBE-EGRESS-IP-MULTI-NIC chain run first
	// this also ensure for egress-services, the
	// 	chain egress-services {
	//	type nat hook postrouting priority srcnat; policy accept;
	// is called before the local gateway masquerade chain
	localGwMasqChain := &knftables.Chain{
		Name:     nftablesLocalGatewayMasqChain,
		Comment:  knftables.PtrTo("OVN local gateway masquerade"),
		Type:     knftables.PtrTo(knftables.NATType),
		Hook:     knftables.PtrTo(knftables.PostroutingHook),
		Priority: knftables.PtrTo(knftables.BaseChainPriority("101")),
	}
	tx.Add(localGwMasqChain)

	// Create dedicated pod subnet masquerade chain
	podSubnetMasqChain := &knftables.Chain{
		Name: nftablesPodSubnetMasqChain,
	}
	tx.Add(podSubnetMasqChain)

	// Create UDN masquerade chain only if network segmentation is enabled
	var udnMasqChain *knftables.Chain
	if util.IsNetworkSegmentationSupportEnabled() {
		udnMasqChain = &knftables.Chain{
			Name:    nftablesUDNMasqChain,
			Comment: knftables.PtrTo("OVN UDN masquerade"),
		}
		tx.Add(udnMasqChain)
	}

	// Flush existing chains to ensure clean state
	tx.Flush(localGwMasqChain)
	tx.Flush(podSubnetMasqChain)
	if util.IsNetworkSegmentationSupportEnabled() {
		tx.Flush(udnMasqChain)
	}

	// Get the existing local gateway NAT rules
	localGwRules, err := getLocalGatewayNATNFTRules(cidrs...)
	if err != nil {
		return fmt.Errorf("failed to get local gateway NAT rules: %w", err)
	}

	// Add the main local gateway NAT rules
	for _, rule := range localGwRules {
		tx.Add(rule)
	}

	// Add jump rule from main chain to pod subnet chain
	jumpToPodSubnetRule := &knftables.Rule{
		Chain: nftablesLocalGatewayMasqChain,
		Rule: knftables.Concat(
			"jump", nftablesPodSubnetMasqChain,
		),
	}
	tx.Add(jumpToPodSubnetRule)

	// Add jump rule to UDN chain only if network segmentation is enabled
	if util.IsNetworkSegmentationSupportEnabled() {
		jumpToUDNRule := &knftables.Rule{
			Chain: nftablesLocalGatewayMasqChain,
			Rule: knftables.Concat(
				"jump", nftablesUDNMasqChain,
			),
		}
		tx.Add(jumpToUDNRule)
	}

	err = nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("failed to setup local gateway NAT nftables rules: %w", err)
	}

	return nil
}

// addOrUpdateLocalGatewayPodSubnetNFTRules adds nftables rules for pod subnet masquerading for multiple CIDRs
// These rules are added to the dedicated pod subnet masquerade chain.
// If the rules already exist, they are updated.
// If isAdvertisedNetwork is true, the masquerade rules also get a destination match
// that matches the remote node IP set.
func addOrUpdateLocalGatewayPodSubnetNFTRules(isAdvertisedNetwork bool, cidrs ...*net.IPNet) error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	tx := nft.NewTransaction()

	// Ensure the pod subnet chain exists
	podSubnetChain := &knftables.Chain{
		Name: nftablesPodSubnetMasqChain,
	}
	tx.Add(podSubnetChain)

	// Flush the chain to remove all existing rules
	// if network toggles between advertised and non-advertised, we need to flush the chain and re-add correct rules
	tx.Flush(podSubnetChain)

	// Add the new rules for each CIDR
	for _, cidr := range cidrs {
		ruleList, err := getLocalGatewayPodSubnetMasqueradeNFTRule(cidr, isAdvertisedNetwork)
		if err != nil {
			return fmt.Errorf("failed to create nftables rules for CIDR %s: %w", cidr.String(), err)
		}

		// Add all the rules for this CIDR
		for _, rule := range ruleList {
			tx.Add(rule)
		}
	}

	if err := nft.Run(context.TODO(), tx); err != nil {
		return fmt.Errorf("failed to add pod subnet NAT rules: %w", err)
	}

	return nil
}

// delLocalGatewayPodSubnetNFTRules removes nftables rules for pod subnet masquerading for multiple CIDRs
// Since we use a separate chain, we can simply flush it to remove all pod subnet rules.
func delLocalGatewayPodSubnetNFTRules() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %w", err)
	}

	tx := nft.NewTransaction()

	// In shared gateway mode, this chain might not exist if its
	// not migration from local gateway mode. In that case, let's
	// use the idiomatic way of adding the chain before trying to flush it.
	// I anyways also have the knftables.IsNotFound() check in the caller later.
	tx.Add(&knftables.Chain{
		Name: nftablesPodSubnetMasqChain,
	})

	// Simply flush the dedicated pod subnet masquerade chain
	// This removes all pod subnet masquerade rules at once
	tx.Flush(&knftables.Chain{Name: nftablesPodSubnetMasqChain})

	if err := nft.Run(context.TODO(), tx); err != nil && !knftables.IsNotFound(err) {
		return fmt.Errorf("failed to delete pod subnet NAT rules: %w", err)
	}

	return nil
}

// getMasqueradeVIP returns the .3 masquerade VIP based on the protocol (v4/v6) of provided IP string
func getMasqueradeVIP(ip string) string {
	if utilnet.IsIPv6String(ip) {
		return config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP.String()
	}
	return config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String()
}

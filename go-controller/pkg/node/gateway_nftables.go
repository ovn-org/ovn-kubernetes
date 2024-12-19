//go:build linux
// +build linux

package node

import (
	"fmt"
	"strings"

	kapi "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// gateway_nftables.go contains code for dealing with nftables rules; it is used in
// conjunction with gateway_iptables.go.
//
// For the most part, using a mix of iptables and nftables rules does not matter, since
// both of them are handled by netfilter. However, in cases where there is a close
// ordering dependency between two rules (especially, in any case where it's necessary to
// use an "accept" rule to override a later "drop" rule), then those rules will need to
// either both be iptables or both be nftables.

// getNoSNATNodePortRules returns elements to add to the "mgmtport-no-snat-nodeports"
// set to prevent SNAT of sourceIP when passing through the management port, for an
// `externalTrafficPolicy: Local` service with NodePorts.
func getNoSNATNodePortRules(svcPort kapi.ServicePort) []knftables.Object {
	return []knftables.Object{
		&knftables.Element{
			Set: nftablesMgmtPortNoSNATNodePorts,
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
func getNoSNATLoadBalancerIPRules(svcPort kapi.ServicePort, localEndpoints []string) []knftables.Object {
	var nftRules []knftables.Object
	protocol := strings.ToLower(string(svcPort.Protocol))
	port := fmt.Sprintf("%v", svcPort.TargetPort.IntValue())
	for _, ip := range localEndpoints {
		setName := nftablesMgmtPortNoSNATServicesV4
		if utilnet.IsIPv6String(ip) {
			setName = nftablesMgmtPortNoSNATServicesV6
		}

		nftRules = append(nftRules,
			&knftables.Element{
				Set: setName,
				Key: []string{ip, protocol, port},
			},
		)
	}
	return nftRules
}

// getUDNNodePortMarkNFTRule returns a verdict map element (nftablesUDNMarkNodePortsMap)
// with a key composed of the svcPort protocol and port.
// The value is a jump to the UDN chain mark if netInfo is provided, or nil that is useful for map entry removal.
func getUDNNodePortMarkNFTRule(svcPort kapi.ServicePort, netInfo *bridgeUDNConfiguration) *knftables.Element {
	var val []string
	if netInfo != nil {
		val = []string{fmt.Sprintf("jump %s", GetUDNMarkChain(netInfo.pktMark))}
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
func getUDNExternalIPsMarkNFTRules(svcPort kapi.ServicePort, externalIPs []string, netInfo *bridgeUDNConfiguration) []knftables.Object {
	var nftRules []knftables.Object
	var val []string

	if netInfo != nil {
		val = []string{fmt.Sprintf("jump %s", GetUDNMarkChain(netInfo.pktMark))}
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

// getGatewayNFTRules returns nftables rules for service. This must be used in conjunction
// with getGatewayIPTRules.
func getGatewayNFTRules(service *kapi.Service, localEndpoints []string, svcHasLocalHostNetEndPnt bool) []knftables.Object {
	rules := make([]knftables.Object, 0)
	svcTypeIsETPLocal := util.ServiceExternalTrafficPolicyLocal(service)
	for _, svcPort := range service.Spec.Ports {
		if svcTypeIsETPLocal && !svcHasLocalHostNetEndPnt {
			// For `externalTrafficPolicy: Local` services with pod-network
			// endpoints, we need to add rules to prevent them from being SNATted
			// when entering the management port, to preserve the client IP.
			if util.ServiceTypeHasNodePort(service) {
				rules = append(rules, getNoSNATNodePortRules(svcPort)...)
			} else if len(util.GetExternalAndLBIPs(service)) > 0 {
				rules = append(rules, getNoSNATLoadBalancerIPRules(svcPort, localEndpoints)...)
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
			Name: nftablesMgmtPortNoSNATNodePorts,
		},
		&knftables.Set{
			Name: nftablesMgmtPortNoSNATServicesV4,
		},
		&knftables.Set{
			Name: nftablesMgmtPortNoSNATServicesV6,
		},
	}
}

// getUDNNFTRules generates nftables rules for a UDN service.
// If netConfig is nil, the resulting map elements will have empty values,
// suitable only for entry removal.
func getUDNNFTRules(service *kapi.Service, netConfig *bridgeUDNConfiguration) []knftables.Object {
	rules := make([]knftables.Object, 0)
	for _, svcPort := range service.Spec.Ports {
		if util.ServiceTypeHasNodePort(service) {
			rules = append(rules, getUDNNodePortMarkNFTRule(svcPort, netConfig))
		}
		rules = append(rules, getUDNExternalIPsMarkNFTRules(svcPort, util.GetExternalAndLBIPs(service), netConfig)...)
	}
	return rules
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

//go:build linux
// +build linux

package node

import (
	"context"
	"fmt"
	"strings"

	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"
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
func getNoSNATNodePortRules(svcPort kapi.ServicePort) []*knftables.Element {
	return []*knftables.Element{
		{
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
func getNoSNATLoadBalancerIPRules(svcPort kapi.ServicePort, localEndpoints []string) []*knftables.Element {
	var nftRules []*knftables.Element
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

func recreateNFTSet(setName string, keepNFTElems []*knftables.Element) error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()
	tx.Flush(&knftables.Set{
		Name: setName,
	})
	for _, elem := range keepNFTElems {
		if elem.Set == setName {
			tx.Add(elem)
		}
	}
	return nft.Run(context.TODO(), tx)
}

// getGatewayNFTRules returns nftables rules for service. This must be used in conjunction
// with getGatewayIPTRules.
func getGatewayNFTRules(service *kapi.Service, localEndpoints []string, svcHasLocalHostNetEndPnt bool) []*knftables.Element {
	rules := make([]*knftables.Element, 0)
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

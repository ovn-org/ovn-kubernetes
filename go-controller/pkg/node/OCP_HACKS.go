// +build linux

package node

import (
	"fmt"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
)

// OCP HACK: Block MCS Access. https://github.com/openshift/ovn-kubernetes/pull/170
func generateBlockMCSRules(rules *[]iptRule, protocol iptables.Protocol) {
	*rules = append(*rules, iptRule{
		table:    "filter",
		chain:    "FORWARD",
		args:     []string{"-p", "tcp", "-m", "tcp", "--dport", "22623", "-j", "REJECT"},
		protocol: protocol,
	})
	*rules = append(*rules, iptRule{
		table:    "filter",
		chain:    "FORWARD",
		args:     []string{"-p", "tcp", "-m", "tcp", "--dport", "22624", "-j", "REJECT"},
		protocol: protocol,
	})
	*rules = append(*rules, iptRule{
		table:    "filter",
		chain:    "OUTPUT",
		args:     []string{"-p", "tcp", "-m", "tcp", "--dport", "22623", "-j", "REJECT"},
		protocol: protocol,
	})
	*rules = append(*rules, iptRule{
		table:    "filter",
		chain:    "OUTPUT",
		args:     []string{"-p", "tcp", "-m", "tcp", "--dport", "22624", "-j", "REJECT"},
		protocol: protocol,
	})
}

// END OCP HACK

// OCP HACK: Fix Azure/GCP LoadBalancers. https://github.com/openshift/ovn-kubernetes/pull/112
func getLoadBalancerIPTRules(svc *kapi.Service, svcPort kapi.ServicePort, gatewayIP string, targetPort int32) []iptRule {
	var rules []iptRule
	ingPort := fmt.Sprintf("%d", svcPort.Port)
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP == "" {
			continue
		}
		rules = append(rules, iptRule{
			table: "nat",
			chain: iptableNodePortChain,
			args: []string{
				"-d", ing.IP,
				"-p", string(svcPort.Protocol), "--dport", ingPort,
				"-j", "DNAT", "--to-destination", util.JoinHostPortInt32(gatewayIP, targetPort),
			},
		})
		rules = append(rules, iptRule{
			table: "filter",
			chain: iptableNodePortChain,
			args: []string{
				"-d", ing.IP,
				"-p", string(svcPort.Protocol), "--dport", ingPort,
				"-j", "ACCEPT",
			},
		})
	}
	return rules
}

// END OCP HACK

// +build linux

package node

import (
	"github.com/coreos/go-iptables/iptables"
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

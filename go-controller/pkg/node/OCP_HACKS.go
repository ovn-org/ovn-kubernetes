// +build linux

package node

import (
	"github.com/coreos/go-iptables/iptables"
)

// Block MCS Access. https://github.com/openshift/ovn-kubernetes/pull/170
func generateBlockMCSRules(rules *[]iptRule, protocol iptables.Protocol) {
	var delRules []iptRule

	for _, chain := range []string{"FORWARD", "OUTPUT"} {
		for _, port := range []string{"22623", "22624"} {
			*rules = append(*rules, iptRule{
				table:    "filter",
				chain:    chain,
				args:     []string{"-p", "tcp", "-m", "tcp", "--dport", port, "--syn", "-j", "REJECT"},
				protocol: protocol,
			})
			// Delete the old "--syn"-less rules on upgrade
			delRules = append(delRules, iptRule{
				table:    "filter",
				chain:    chain,
				args:     []string{"-p", "tcp", "-m", "tcp", "--dport", port, "-j", "REJECT"},
				protocol: protocol,
			})
		}
	}

	_ = delIptRules(delRules)
}

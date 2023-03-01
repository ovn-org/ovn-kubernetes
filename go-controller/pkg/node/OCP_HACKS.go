//go:build linux
// +build linux

package node

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"

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

// insertMCSBlockIptRules inserts iptables rules to block local Machine Config Service
// ports. See https://github.com/openshift/ovn-kubernetes/pull/170
func insertMCSBlockIptRules() error {
	rules := []iptRule{}
	if config.IPv4Mode {
		generateBlockMCSRules(&rules, iptables.ProtocolIPv4)
	}
	if config.IPv6Mode {
		generateBlockMCSRules(&rules, iptables.ProtocolIPv6)
	}
	if err := insertIptRules(rules); err != nil {
		return fmt.Errorf("failed to setup MCS-blocking rules: %w", err)
	}
	return nil
}

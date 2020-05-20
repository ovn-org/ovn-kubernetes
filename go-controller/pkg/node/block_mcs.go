// +build linux

package node

func generateBlockMCSRules(rules *[]iptRule) {
	*rules = append(*rules, iptRule{
		table: "filter",
		chain: "FORWARD",
		args:  []string{"-p", "tcp", "-m", "tcp", "--dport", "22623", "-j", "REJECT"},
	})
	*rules = append(*rules, iptRule{
		table: "filter",
		chain: "FORWARD",
		args:  []string{"-p", "tcp", "-m", "tcp", "--dport", "22624", "-j", "REJECT"},
	})
	*rules = append(*rules, iptRule{
		table: "filter",
		chain: "OUTPUT",
		args:  []string{"-p", "tcp", "-m", "tcp", "--dport", "22623", "-j", "REJECT"},
	})
	*rules = append(*rules, iptRule{
		table: "filter",
		chain: "OUTPUT",
		args:  []string{"-p", "tcp", "-m", "tcp", "--dport", "22624", "-j", "REJECT"},
	})
}

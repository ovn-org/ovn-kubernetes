// +build linux

package cni

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"

	"k8s.io/apimachinery/pkg/util/wait"
	utilnet "k8s.io/utils/net"
)

// OCP HACK: block access to MCS/metadata; https://github.com/openshift/ovn-kubernetes/pull/19
var iptablesCommands = [][]string{
	// Block MCS
	{"-A", "OUTPUT", "-p", "tcp", "-m", "tcp", "--dport", "22623", "-j", "REJECT"},
	{"-A", "OUTPUT", "-p", "tcp", "-m", "tcp", "--dport", "22624", "-j", "REJECT"},
	{"-A", "FORWARD", "-p", "tcp", "-m", "tcp", "--dport", "22623", "-j", "REJECT"},
	{"-A", "FORWARD", "-p", "tcp", "-m", "tcp", "--dport", "22624", "-j", "REJECT"},
}

var iptables4OnlyCommands = [][]string{
	// Block cloud provider metadata IP except DNS
	{"-A", "OUTPUT", "-p", "tcp", "-m", "tcp", "-d", "169.254.169.254", "!", "--dport", "53", "-j", "REJECT"},
	{"-A", "OUTPUT", "-p", "udp", "-m", "udp", "-d", "169.254.169.254", "!", "--dport", "53", "-j", "REJECT"},
	{"-A", "FORWARD", "-p", "tcp", "-m", "tcp", "-d", "169.254.169.254", "!", "--dport", "53", "-j", "REJECT"},
	{"-A", "FORWARD", "-p", "udp", "-m", "udp", "-d", "169.254.169.254", "!", "--dport", "53", "-j", "REJECT"},
}

func setupIPTablesBlocks(netns ns.NetNS, ifInfo *PodInterfaceInfo) error {
	return netns.Do(func(hostNS ns.NetNS) error {
		var hasIPv4, hasIPv6 bool
		for _, ip := range ifInfo.IPs {
			if utilnet.IsIPv6CIDR(ip) {
				hasIPv6 = true
			} else {
				hasIPv4 = true
			}
		}

		for _, args := range iptablesCommands {
			if hasIPv4 {
				out, err := exec.Command("iptables", args...).CombinedOutput()
				if err != nil {
					return fmt.Errorf("could not set up pod iptables rules: %s", string(out))
				}
			}
			if hasIPv6 {
				out, err := exec.Command("ip6tables", args...).CombinedOutput()
				if err != nil {
					return fmt.Errorf("could not set up pod iptables rules: %s", string(out))
				}
			}
		}
		if hasIPv4 {
			for _, args := range iptables4OnlyCommands {
				out, err := exec.Command("iptables", args...).CombinedOutput()
				if err != nil {
					return fmt.Errorf("could not set up pod iptables rules: %s", string(out))
				}
			}
		}

		return nil
	})
}
// END OCP HACK

// OCP HACK: wait for OVN to fully process the new pod
func ofctlExec(args ...string) (string, error) {
	args = append([]string{"--timeout=30"}, args...)
	output, err := runner.Command("ovs-ofctl", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run 'ovs-ofctl %s': %v\n  %q", strings.Join(args, " "), err, string(output))
	}

	outStr := string(output)
	trimmed := strings.TrimSpace(outStr)
	// If output is a single line, strip the trailing newline
	if strings.Count(trimmed, "\n") == 0 {
		outStr = trimmed
	}

	return outStr, nil
}

func waitForBrIntFlows(ip string) error {
	return wait.PollImmediate(100*time.Millisecond, 20*time.Second, func() (bool, error) {
		stdout, err := ofctlExec("dump-flows", "br-int")
		if err != nil {
			return false, nil
		}
		return strings.Contains(stdout, ip), nil
	})
}
// END OCP HACK

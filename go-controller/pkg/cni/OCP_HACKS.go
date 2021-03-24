// +build linux

package cni

import (
	"fmt"
	"github.com/containernetworking/plugins/pkg/ns"
	"os/exec"

	utilnet "k8s.io/utils/net"
)

// OCP HACK: block access to MCS/metadata; https://github.com/openshift/ovn-kubernetes/pull/19
var iptablesCommands = [][]string{
	// Block MCS
	{"-A", "OUTPUT", "-p", "tcp", "-m", "tcp", "--dport", "22623", "--syn", "-j", "REJECT"},
	{"-A", "OUTPUT", "-p", "tcp", "-m", "tcp", "--dport", "22624", "--syn", "-j", "REJECT"},
	{"-A", "FORWARD", "-p", "tcp", "-m", "tcp", "--dport", "22623", "--syn", "-j", "REJECT"},
	{"-A", "FORWARD", "-p", "tcp", "-m", "tcp", "--dport", "22624", "--syn", "-j", "REJECT"},
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
			args = append([]string{"-w 5"}, args...)
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
				args = append([]string{"-w 5"}, args...)
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

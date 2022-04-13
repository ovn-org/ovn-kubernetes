//go:build linux
// +build linux

package cni

import (
	"fmt"
	"os/exec"

	"github.com/containernetworking/plugins/pkg/ns"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"

	utilnet "k8s.io/utils/net"
)

// OCP HACK: block access to MCS/metadata; https://github.com/openshift/ovn-kubernetes/pull/19
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

		var iptablesCommands = [][]string{
			// Block MCS
			{"-A", "OUTPUT", "-p", "tcp", "-m", "tcp", "--dport", "22623", "--syn", "-j", "REJECT"},
			{"-A", "OUTPUT", "-p", "tcp", "-m", "tcp", "--dport", "22624", "--syn", "-j", "REJECT"},
			{"-A", "FORWARD", "-p", "tcp", "-m", "tcp", "--dport", "22623", "--syn", "-j", "REJECT"},
			{"-A", "FORWARD", "-p", "tcp", "-m", "tcp", "--dport", "22624", "--syn", "-j", "REJECT"},
		}
		metadataServiceIP := "169.254.169.254"
		if config.Kubernetes.PlatformType == string(configv1.AlibabaCloudPlatformType) {
			metadataServiceIP = "100.100.100.200"
		}
		var iptables4OnlyCommands = [][]string{
			// Block cloud provider metadata IP except DNS
			{"-A", "OUTPUT", "-p", "tcp", "-m", "tcp", "-d", metadataServiceIP, "!", "--dport", "53", "-j", "REJECT"},
			{"-A", "OUTPUT", "-p", "udp", "-m", "udp", "-d", metadataServiceIP, "!", "--dport", "53", "-j", "REJECT"},
			{"-A", "FORWARD", "-p", "tcp", "-m", "tcp", "-d", metadataServiceIP, "!", "--dport", "53", "-j", "REJECT"},
			{"-A", "FORWARD", "-p", "udp", "-m", "udp", "-d", metadataServiceIP, "!", "--dport", "53", "-j", "REJECT"},
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

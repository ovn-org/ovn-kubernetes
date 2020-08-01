// +build linux

package cni

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"k8s.io/klog"

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
	args = append([]string{"--timeout=30", "--no-stats", "--strict"}, args...)
	var stdout, stderr bytes.Buffer
	cmd := runner.Command("ovs-ofctl", args...)
	cmd.SetStdout(&stdout)
	cmd.SetStderr(&stderr)

	cmdStr := strings.Join(args, " ")
	klog.V(5).Infof("exec: ovs-ofctl %s", cmdStr)

	err := cmd.Run()
	stdoutStr := string(stdout.Bytes())
	stderrStr := string(stderr.Bytes())
	if err != nil {
		klog.V(5).Infof("exec: stderr: %q", stderrStr)
		return "", fmt.Errorf("failed to run 'ovs-ofctl %s': %v\n  %q", cmdStr, err, string(stderr.Bytes()))
	}
	klog.V(5).Infof("exec: stdout: %q", stdoutStr)

	trimmed := strings.TrimSpace(stdoutStr)
	// If output is a single line, strip the trailing newline
	if strings.Count(trimmed, "\n") == 0 {
		stdoutStr = trimmed
	}
	return stdoutStr, nil
}

func waitForPodFlows(mac string) error {
	return wait.PollImmediate(200*time.Millisecond, 20*time.Second, func() (bool, error) {
		//Query the flows by mac address and count the number of flows.
		query := fmt.Sprintf("table=9,dl_src=%s",mac)
		//ovs-ofctl dumps error on stderr, so stdout will only dump flow data if matches the query.
		stdout, err := ofctlExec("dump-flows", "br-int", query)
		if err != nil {
			return false, nil
		}
		if len(stdout) == 0 {
			return false, nil
		}
		return true, nil
	})
}

// END OCP HACK

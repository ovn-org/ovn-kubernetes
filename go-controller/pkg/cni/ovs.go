package cni

import (
	"bytes"
	"fmt"
	utilnet "k8s.io/utils/net"
	"net"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	kexec "k8s.io/utils/exec"
)

var runner kexec.Interface
var vsctlPath string

func setExec(r kexec.Interface) error {
	runner = r
	var err error
	vsctlPath, err = r.LookPath("ovs-vsctl")
	return err
}

func ovsExec(args ...string) (string, error) {
	if runner == nil {
		if err := setExec(kexec.New()); err != nil {
			return "", err
		}
	}

	args = append([]string{"--timeout=30"}, args...)
	output, err := runner.Command(vsctlPath, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run 'ovs-vsctl %s': %v\n  %q", strings.Join(args, " "), err, string(output))
	}

	return strings.TrimSuffix(string(output), "\n"), nil
}

func ovsCreate(table string, values ...string) (string, error) {
	args := append([]string{"create", table}, values...)
	return ovsExec(args...)
}

func ovsDestroy(table, record string) error {
	_, err := ovsExec("--if-exists", "destroy", table, record)
	return err
}

func ovsSet(table, record string, values ...string) error {
	args := append([]string{"set", table, record}, values...)
	_, err := ovsExec(args...)
	return err
}

// Returns the given column of records that match the condition
func ovsFind(table, column, condition string) ([]string, error) {
	output, err := ovsExec("--no-heading", "--format=csv", "--data=bare", "--columns="+column, "find", table, condition)
	if err != nil {
		return nil, err
	}
	if output == "" {
		return nil, nil
	}
	return strings.Split(output, "\n"), nil
}

func ovsClear(table, record string, columns ...string) error {
	args := append([]string{"--if-exists", "clear", table, record}, columns...)
	_, err := ovsExec(args...)
	return err
}

func ofctlExec(args ...string) (string, error) {
	args = append([]string{"--timeout=10", "--no-stats", "--strict"}, args...)
	var stdout, stderr bytes.Buffer
	cmd := runner.Command("ovs-ofctl", args...)
	cmd.SetStdout(&stdout)
	cmd.SetStderr(&stderr)

	cmdStr := strings.Join(args, " ")
	klog.V(5).Infof("exec: ovs-ofctl %s", cmdStr)

	err := cmd.Run()
	if err != nil {
		stderrStr := stderr.String()
		klog.Errorf("exec: ovs-ofctl %s : stderr: %q", cmdStr, stderrStr)
		return "", fmt.Errorf("failed to run 'ovs-ofctl %s': %v\n  %q", cmdStr, err, stderrStr)
	}
	stdoutStr := stdout.String()
	klog.V(5).Infof("exec: ovs-ofctl %s: stdout: %q", cmdStr, stdoutStr)

	trimmed := strings.TrimSpace(stdoutStr)
	// If output is a single line, strip the trailing newline
	if strings.Count(trimmed, "\n") == 0 {
		stdoutStr = trimmed
	}
	return stdoutStr, nil
}

func waitForPodFlows(mac string, ifAddrs []*net.IPNet) error {
	// query represents the match criteria, and different OF tables that this query may match on
	type query struct {
		match  string
		tables []int
	}

	return wait.PollImmediate(200*time.Millisecond, 20*time.Second, func() (bool, error) {
		// Function checks for OpenFlow flows to know the pod is ready
		// TODO(trozet): in the future use a more stable mechanism provided by OVN:
		// https://bugzilla.redhat.com/show_bug.cgi?id=1839102

		// Query the flows by mac address for in_port_security
		queries := []query{
			{
				match:  "dl_src=" + mac,
				tables: []int{9},
			},
		}
		for _, ifAddr := range ifAddrs {
			var ipMatch string
			if !utilnet.IsIPv6(ifAddr.IP) {
				ipMatch = "ip,ip_dst"
			} else {
				ipMatch = "ipv6,ipv6_dst"
			}
			// add queries for out_port_security
			// note we need to support table 48 for 20.06 OVN backwards compatibility. Table 49 is now
			// where out_port_security lives
			queries = append(queries,
				query{fmt.Sprintf("%s=%s", ipMatch, ifAddr.IP), []int{48, 49}},
			)
		}
		for _, query := range queries {
			found := false
			// Look for a match in any table to be considered success
			for _, table := range query.tables {
				queryStr := fmt.Sprintf("table=%d,%s", table, query.match)
				// ovs-ofctl dumps error on stderr, so stdout will only dump flow data if matches the query.
				stdout, err := ofctlExec("dump-flows", "br-int", queryStr)
				if err == nil && len(stdout) > 0 {
					found = true
					break
				}
			}
			if !found {
				return false, nil
			}
		}
		return true, nil
	})
}

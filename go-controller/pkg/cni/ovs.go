package cni

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"
	utilnet "k8s.io/utils/net"
)

var runner kexec.Interface
var vsctlPath string
var ofctlPath string

func SetExec(r kexec.Interface) error {
	runner = r
	var err error
	vsctlPath, err = r.LookPath("ovs-vsctl")
	if err != nil {
		return err
	}
	ofctlPath, err = r.LookPath("ovs-ofctl")
	return err
}

// ResetRunner used by unit-tests to reset runner to its initial (un-initialized) value
func ResetRunner() {
	runner = nil
}

func ovsExec(args ...string) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("OVS exec runner not initialized")
	}

	args = append([]string{"--timeout=30"}, args...)
	output, err := runner.Command(vsctlPath, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run 'ovs-vsctl %s': %v\n  %q", strings.Join(args, " "), err, string(output))
	}

	return strings.TrimSuffix(string(output), "\n"), nil
}

// ovsGetMultiOutput allows running get command with multiple columns
// returns a slice of requested fields
// if row doesn't exist command output will be a slice with an empty string
// empty ovsdb value [] will be replaced with an empty string
func ovsGetMultiOutput(table, record string, columns []string) ([]string, error) {
	args := []string{"--if-exists", "get", table, record}
	args = append(args, columns...)
	output, err := ovsExec(args...)
	var result []string
	// columns are separated with \n
	// remove \" as with --data=bare formatting
	for _, column := range strings.Split(strings.ReplaceAll(output, "\"", ""), "\n") {
		if column == "[]" {
			result = append(result, "")
		} else {
			result = append(result, column)
		}
	}
	return result, err
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

func ovsGet(table, record, column, key string) (string, error) {
	args := []string{"--if-exists", "get", table, record}
	if key != "" {
		args = append(args, fmt.Sprintf("%s:%s", column, key))
	} else {
		args = append(args, column)
	}
	output, err := ovsExec(args...)
	return strings.Trim(strings.TrimSpace(string(output)), "\""), err
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
	if runner == nil {
		return "", fmt.Errorf("OVS exec runner not initialized")
	}

	args = append([]string{"--timeout=10", "--no-stats", "--strict"}, args...)
	var stdout, stderr bytes.Buffer
	cmd := runner.Command(ofctlPath, args...)
	cmd.SetStdout(&stdout)
	cmd.SetStderr(&stderr)

	cmdStr := strings.Join(args, " ")
	klog.V(5).Infof("Exec: %s %s", ofctlPath, cmdStr)

	err := cmd.Run()
	if err != nil {
		stderrStr := stderr.String()
		klog.Errorf("Exec: %s %s : stderr: %q", ofctlPath, cmdStr, stderrStr)
		return "", fmt.Errorf("failed to run '%s %s': %v\n  %q", ofctlPath, cmdStr, err, stderrStr)
	}
	stdoutStr := stdout.String()
	klog.V(5).Infof("Exec: %s %s: stdout: %q", ofctlPath, cmdStr, stdoutStr)

	trimmed := strings.TrimSpace(stdoutStr)
	// If output is a single line, strip the trailing newline
	if strings.Count(trimmed, "\n") == 0 {
		stdoutStr = trimmed
	}
	return stdoutStr, nil
}

// getIfaceOFPort returns the of port number for an interface
func getIfaceOFPort(ifaceName string) (int, error) {
	port, err := ovsGet("Interface", ifaceName, "ofport", "")
	if err == nil && port == "" {
		return -1, fmt.Errorf("cannot find OpenFlow port for OVS interface: %s, error: %v ", ifaceName, err)
	}

	iPort, err := strconv.Atoi(port)
	if err != nil {
		return -1, fmt.Errorf("unable to parse OpenFlow port %s, error: %v", port, err)
	}
	return iPort, nil
}

func doPodFlowsExist(mac string, ifAddrs []*net.IPNet, ofPort int) bool {
	// Function checks for OpenFlow flows to know the pod is ready
	// Legacy way to check pod readiness for versions that don't support ovn-installed

	// query represents the match criteria, and different OF tables that this query may match on
	type query struct {
		match  string
		tables []int
	}

	// Query the flows by mac address for in_port_security and OF port
	queries := []query{
		{
			match:  "dl_src=" + mac,
			tables: []int{9},
		},
		{
			match:  fmt.Sprintf("in_port=%d", ofPort),
			tables: []int{0},
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

	// Must find the right flows in all queries to succeed
	for _, query := range queries {
		found := false
		// Look for a match in any table of this query to be considered success
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
			return false
		}
	}

	return true
}

// checkCancelSandbox checks that this sandbox is still valid for the current
// instance of the pod in the apiserver. Sandbox requests and pod instances
// have a 1:1 relationship determined by pod UID. If we detect that the pod
// has changed either UID or MAC terminate this sandbox request early instead
// of waiting for OVN to set up flows that will never exist.
func checkCancelSandbox(mac string, getter PodInfoGetter, namespace, name, nadName, initialPodUID string) error {
	// Not all node CNI modes may have access to kube api, those will pass nil as getter.
	if getter == nil {
		return nil
	}
	pod, err := getter.getPod(namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("pod deleted")
		}
		klog.Warningf("[%s/%s] failed to get pod while waiting for OVS port binding: %v", namespace, name, err)
		return nil
	}

	if string(pod.UID) != initialPodUID {
		// Pod UID changed and this sandbox should be canceled
		// so the new pod sandbox can run
		return fmt.Errorf("canceled old pod sandbox")
	}

	ovnAnnot, err := util.UnmarshalPodAnnotation(pod.Annotations, nadName)
	if err != nil {
		return fmt.Errorf("pod OVN annotations deleted or invalid")
	}

	// Pod OVN annotation changed and this sandbox should
	// be canceled so the new pod sandbox can run with the
	// updated MAC/IP
	if mac != ovnAnnot.MAC.String() {
		return fmt.Errorf("pod OVN annotations changed")
	}

	return nil
}

func waitForPodInterface(ctx context.Context, ifInfo *PodInterfaceInfo,
	ifaceName, ifaceID string, getter PodInfoGetter,
	namespace, name, initialPodUID string) error {
	var detail string
	var ofPort int
	var err error

	mac := ifInfo.MAC.String()
	ifAddrs := ifInfo.IPs
	checkExternalIDs := ifInfo.CheckExtIDs
	if checkExternalIDs {
		detail = " (ovn-installed)"
	} else {
		ofPort, err = getIfaceOFPort(ifaceName)
		if err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			errDetail := "timed out"
			if ctx.Err() == context.Canceled {
				errDetail = "canceled while"
			}
			return fmt.Errorf("%s waiting for OVS port binding%s for %s %v", errDetail, detail, mac, ifAddrs)
		default:
			columns := []string{"external-ids:iface-id"}
			if checkExternalIDs {
				// get ovn-installed flag in the same request
				columns = append(columns, "external-ids:ovn-installed")
			}
			output, err := ovsGetMultiOutput("Interface", ifaceName, columns)
			// check to see if the interface has its external id set, which indicates if it is active
			// It may have been cleared by a subsequent CNI ADD and if so, there's no need to keep checking for flows
			if err == nil && len(output) > 0 && output[0] != ifaceID {
				return fmt.Errorf("OVS sandbox port %s is no longer active (probably due to a subsequent "+
					"CNI ADD)", ifaceName)
			}
			if checkExternalIDs {
				if err == nil && len(output) == 2 && output[1] == "true" {
					klog.V(5).Infof("Interface %s has ovn-installed=true", ifaceName)
					return nil
				}
				klog.V(5).Infof("Still waiting for OVS port %s to have ovn-installed=true", ifaceName)
			} else {
				if doPodFlowsExist(mac, ifAddrs, ofPort) {
					// success
					return nil
				}
			}

			if err := checkCancelSandbox(mac, getter, namespace, name, ifInfo.NADName, initialPodUID); err != nil {
				return fmt.Errorf("%v waiting for OVS port binding for %s %v", err, mac, ifAddrs)
			}

			// try again later
			time.Sleep(200 * time.Millisecond)
		}
	}
}

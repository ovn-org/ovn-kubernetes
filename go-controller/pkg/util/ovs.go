// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/afero"

	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
)

const (
	// On Windows we need an increased timeout on OVS commands, because
	// adding internal ports on a non Hyper-V enabled host will call
	// external Powershell commandlets.
	// TODO: Decrease the timeout once port adding is improved on Windows
	ovsCommandTimeout  = 15
	ovsOfctlCommand    = "ovs-ofctl"
	ovsAppctlCommand   = "ovs-appctl"
	ovnNbctlCommand    = "ovn-nbctl"
	ovnSbctlCommand    = "ovn-sbctl"
	ovnAppctlCommand   = "ovn-appctl"
	ovsdbClientCommand = "ovsdb-client"
	ipCommand          = "ip"
	powershellCommand  = "powershell"
	netshCommand       = "netsh"
	routeCommand       = "route"
	sysctlCommand      = "sysctl"
	osRelease          = "/etc/os-release"
	rhel               = "RHEL"
	ubuntu             = "Ubuntu"
	windowsOS          = "windows"
)

const (
	nbdbCtlFileName = "ovnnb_db.ctl"
	sbdbCtlFileName = "ovnsb_db.ctl"
	FloodAction     = "FLOOD"
	NormalAction    = "NORMAL"
)

var (
	// These are variables (not constants) so that testcases can modify them
	ovsRunDir string = config.OvsPaths.RunDir
	ovnRunDir string = config.OvnSouth.RunDir
)

var ovnCmdRetryInterval = 2 * time.Second
var ovnCmdRetryCount = 200
var AppFs = afero.NewOsFs()

// PrepareTestConfig restores default config values. Used by testcases to
// provide a pristine environment between tests.
func PrepareTestConfig() {
	ovsRunDir = config.OvsPaths.RunDir
	ovnRunDir = config.OvnSouth.RunDir
}

// SetupMockOVSPidFile setup mock filesystem for ovs-vswitchd.pid file.
func SetupMockOVSPidFile() error {
	err := AppFs.MkdirAll("/var/run/openvswitch/", 0o755)
	if err != nil {
		return err
	}
	return afero.WriteFile(AppFs, "/var/run/openvswitch/ovs-vswitchd.pid", []byte("1234"), 0o644)
}

func runningPlatform() (string, error) {
	if runtime.GOOS == windowsOS {
		return windowsOS, nil
	}
	fileContents, err := afero.ReadFile(AppFs, osRelease)
	if err != nil {
		return "", fmt.Errorf("failed to parse file %s (%v)", osRelease, err)
	}

	var platform string
	ss := strings.Split(string(fileContents), "\n")
	for _, pair := range ss {
		keyValue := strings.Split(pair, "=")
		if len(keyValue) == 2 {
			if keyValue[0] == "Name" || keyValue[0] == "NAME" {
				platform = keyValue[1]
				break
			}
		}
	}

	if platform == "" {
		return "", fmt.Errorf("failed to find the platform name")
	}

	if strings.Contains(platform, "Fedora") ||
		strings.Contains(platform, "Red Hat") || strings.Contains(platform, "CentOS") {
		return rhel, nil
	} else if strings.Contains(platform, "Debian") ||
		strings.Contains(platform, ubuntu) {
		return ubuntu, nil
	} else if strings.Contains(platform, "VMware") {
		return "Photon", nil
	}
	return "", fmt.Errorf("unknown platform")
}

// Exec runs various OVN and OVS utilities
type execHelper struct {
	exec            kexec.Interface
	ofctlPath       string
	appctlPath      string
	ovnappctlPath   string
	nbctlPath       string
	sbctlPath       string
	ovnctlPath      string
	ovsdbClientPath string
	ovnRunDir       string
	ipPath          string
	powershellPath  string
	netshPath       string
	routePath       string
	sysctlPath      string
}

var runner *execHelper

type ExecRunner interface {
	RunCmd(cmd kexec.Cmd, cmdPath string, envVars []string, args ...string) (*bytes.Buffer, *bytes.Buffer, error)
}

// defaultExecRunner implements the methods defined in the ExecRunner interface
type defaultExecRunner struct {
}

// RunCmd invokes the methods of the Cmd interfaces defined in k8s.io/utils/exec to execute commands
// Note: the cmdPath and args parameter are used only for logging and is not processed
func (runsvc *defaultExecRunner) RunCmd(cmd kexec.Cmd, cmdPath string, envVars []string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	if cmd == nil {
		return &bytes.Buffer{}, &bytes.Buffer{}, fmt.Errorf("cmd object cannot be nil")
	}
	if len(envVars) != 0 {
		cmd.SetEnv(envVars)
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	cmd.SetStdout(stdout)
	cmd.SetStderr(stderr)

	counter := atomic.AddUint64(&runCounter, 1)
	logCmd := fmt.Sprintf("%s %s", cmdPath, strings.Join(args, " "))
	klog.V(5).Infof("Exec(%d): %s", counter, logCmd)

	err := cmd.Run()
	klog.V(5).Infof("Exec(%d): stdout: %q", counter, stdout)
	klog.V(5).Infof("Exec(%d): stderr: %q", counter, stderr)
	if err != nil {
		klog.V(5).Infof("Exec(%d): err: %v", counter, err)
	}
	return stdout, stderr, err
}

var RunCmdExecRunner ExecRunner = &defaultExecRunner{}

// SetExec validates executable paths and saves the given exec interface
// to be used for running various OVS and OVN utilites
func SetExec(exec kexec.Interface) error {
	err := SetExecWithoutOVS(exec)
	if err != nil {
		return err
	}

	runner.ofctlPath, err = exec.LookPath(ovsOfctlCommand)
	if err != nil {
		return err
	}
	runner.appctlPath, err = exec.LookPath(ovsAppctlCommand)
	if err != nil {
		return err
	}

	runner.ovnappctlPath, err = exec.LookPath(ovnAppctlCommand)
	if err != nil {
		// If ovn-appctl command is not available then fall back to
		// ovs-appctl. It also means OVN is using the rundir of
		// openvswitch.
		runner.ovnappctlPath = runner.appctlPath
		runner.ovnctlPath = "/usr/share/openvswitch/scripts/ovn-ctl"
		runner.ovnRunDir = config.OvsPaths.RunDir
	} else {
		// If ovn-appctl command is available, it means OVN
		// has its own separate rundir, logdir, sharedir.
		runner.ovnctlPath = "/usr/share/ovn/scripts/ovn-ctl"

		// OvnNorth.RunDir should be the same as OvnSouth.RunDir.
		runner.ovnRunDir = config.OvnNorth.RunDir
	}

	runner.nbctlPath, err = exec.LookPath(ovnNbctlCommand)
	if err != nil {
		return err
	}
	runner.sbctlPath, err = exec.LookPath(ovnSbctlCommand)
	if err != nil {
		return err
	}
	runner.ovsdbClientPath, err = exec.LookPath(ovsdbClientCommand)
	if err != nil {
		return err
	}
	return nil
}

// SetExecWithoutOVS validates executable paths excluding OVS/OVN binaries and
// saves the given exec interface to be used for running various utilites
func SetExecWithoutOVS(exec kexec.Interface) error {
	var err error

	runner = &execHelper{exec: exec}
	if runtime.GOOS == windowsOS {
		runner.powershellPath, err = exec.LookPath(powershellCommand)
		if err != nil {
			return err
		}
		runner.netshPath, err = exec.LookPath(netshCommand)
		if err != nil {
			return err
		}
		runner.routePath, err = exec.LookPath(routeCommand)
		if err != nil {
			return err
		}
	} else {
		runner.ipPath, err = exec.LookPath(ipCommand)
		if err != nil {
			return err
		}
		runner.sysctlPath, err = exec.LookPath(sysctlCommand)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetExec returns the exec interface which can be used for running commands directly.
// Only use for passing an exec interface into pkg/config which cannot call this
// function directly because this module imports pkg/config already.
func GetExec() kexec.Interface {
	return runner.exec
}

// ResetRunner used by unit-tests to reset runner to its initial (un-initialized) value
func ResetRunner() {
	runner = nil
}

var runCounter uint64

func runCmd(cmd kexec.Cmd, cmdPath string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	return RunCmdExecRunner.RunCmd(cmd, cmdPath, []string{}, args...)
}

func run(cmdPath string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	cmd := runner.exec.Command(cmdPath, args...)
	return RunCmdExecRunner.RunCmd(cmd, cmdPath, []string{}, args...)
}

func runWithEnvVars(cmdPath string, envVars []string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	cmd := runner.exec.Command(cmdPath, args...)
	return RunCmdExecRunner.RunCmd(cmd, cmdPath, envVars, args...)
}

// RunOVSOfctl runs a command via ovs-ofctl.
func RunOVSOfctl(args ...string) (string, string, error) {
	stdout, stderr, err := run(runner.ofctlPath, args...)
	return strings.Trim(stdout.String(), "\" \n"), stderr.String(), err
}

// SetPortNoFlood sets the OFPPC_NO_FLOOD bit on the given OVS port via
// "ovs-ofctl mod-port", preventing NORMAL/FLOOD actions from sending
// packets to that port. The no-flood property is an OpenFlow port
// config flag and cannot be set through the OVSDB Port table.
// The caller must resolve the ofport number (e.g. via libovsdb)
// because ovs-ofctl uses OpenFlow to map names to numbers and OpenFlow
// limits port names to 15 bytes (see ovs-ofctl(8) --names documentation).
func SetPortNoFlood(bridgeName, ofport string) error {
	_, stderr, err := RunOVSOfctl("mod-port", bridgeName, ofport, "no-flood")
	if err != nil {
		return fmt.Errorf("failed to set no-flood on ofport %s of bridge %s: stderr: %s, error: %w",
			ofport, bridgeName, stderr, err)
	}
	return nil
}

// GetNoFloodPorts returns the set of ofport numbers on bridgeName that
// already have OFPPC_NO_FLOOD set, by parsing "ovs-ofctl dump-ports-desc".
// The returned map keys are ofport number strings (e.g. "42").
func GetNoFloodPorts(bridgeName string) (map[string]bool, error) {
	stdout, stderr, err := RunOVSOfctl("dump-ports-desc", bridgeName)
	if err != nil {
		return nil, fmt.Errorf("dump-ports-desc on %s failed: stderr: %s, error: %w",
			bridgeName, stderr, err)
	}
	// Output format (one block per port):
	//  1(eth0): addr:...
	//      config:     NO_FLOOD
	//      state:      LIVE
	result := make(map[string]bool)
	var currentOfport string
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		// Port header, e.g.:
		//  1(eth0): addr:...
		if idx := strings.IndexByte(trimmed, '('); idx > 0 {
			port := strings.TrimSpace(trimmed[:idx])
			if _, err := strconv.ParseUint(port, 10, 32); err == nil {
				currentOfport = port
			}
			continue
		}
		// Port configuration, e.g.:
		//  config:     NO_FLOOD
		if currentOfport != "" && strings.HasPrefix(trimmed, "config:") {
			for _, flag := range strings.Fields(strings.TrimPrefix(trimmed, "config:")) {
				if flag == "NO_FLOOD" {
					result[currentOfport] = true
					break
				}
			}
		}
	}
	return result, nil
}

// RunOVSAppctlWithTimeout runs a command via ovs-appctl.
func RunOVSAppctlWithTimeout(timeout int, args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", timeout)}
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := run(runner.appctlPath, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNAppctlWithTimeout runs a command via ovn-appctl. If ovn-appctl is not present, then it
// falls back to using ovs-appctl.
func RunOVNAppctlWithTimeout(timeout int, args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", timeout)}
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := run(runner.ovnappctlPath, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// Run the ovn-ctl command and retry if "Connection refused"
// poll waitng for service to become available
// FIXME: Remove when https://github.com/ovn-kubernetes/libovsdb/issues/235 is fixed
func runOVNretry(cmdPath string, envVars []string, extraArgsFunc func() ([]string, error), args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	retriesLeft := ovnCmdRetryCount
	for {
		var cmdArgs []string
		if extraArgsFunc != nil {
			extraArgs, err := extraArgsFunc()
			if err != nil {
				return nil, nil, err
			}
			cmdArgs = append(cmdArgs, extraArgs...)
		}
		cmdArgs = append(cmdArgs, args...)

		stdout, stderr, err := runWithEnvVars(cmdPath, envVars, cmdArgs...)
		if err == nil {
			return stdout, stderr, err
		}

		// Connection refused
		// OVN DB may not be up so keep trying
		if strings.Contains(stderr.String(), "Connection refused") {
			if retriesLeft == 0 {
				return stdout, stderr, err
			}
			klog.V(4).Infof("Command '%s %s' failed due to 'Connection refused', retrying %d more time(s)", cmdPath, strings.Join(cmdArgs, " "), retriesLeft)
			retriesLeft--
			time.Sleep(ovnCmdRetryInterval)
		} else {
			// Some other problem for caller to handle
			return stdout, stderr, fmt.Errorf("OVN command '%s %s' failed: %s", cmdPath, strings.Join(cmdArgs, " "), err)
		}
	}
}

func getNbctlArgsAndEnv(timeout int, args ...string) ([]string, []string) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", timeout)}
	cmdArgs = append(cmdArgs, args...)
	return cmdArgs, []string{}
}

// RunOVNNbctlWithTimeout runs command via ovn-nbctl with a specific timeout
// FIXME: Remove when https://github.com/ovn-kubernetes/libovsdb/issues/235 is fixed
func RunOVNNbctlWithTimeout(timeout int, args ...string) (string, string, error) {
	stdout, stderr, err := RunOVNNbctlRawOutput(timeout, args...)
	return strings.Trim(strings.TrimSpace(stdout), "\""), stderr, err
}

// RunOVNNbctlRawOutput returns the output with no trimming or other string manipulation
// FIXME: Remove when https://github.com/ovn-kubernetes/libovsdb/issues/235 is fixed
func RunOVNNbctlRawOutput(timeout int, args ...string) (string, string, error) {
	cmdArgs, envVars := getNbctlArgsAndEnv(timeout, args...)
	stdout, stderr, err := runOVNretry(runner.nbctlPath, envVars, nil, cmdArgs...)
	return stdout.String(), stderr.String(), err
}

// RunOVNNbctl runs a command via ovn-nbctl.
// FIXME: Remove when https://github.com/ovn-kubernetes/libovsdb/issues/235 is fixed
func RunOVNNbctl(args ...string) (string, string, error) {
	return RunOVNNbctlWithTimeout(ovsCommandTimeout, args...)
}

// RunOVNSbctlWithTimeout runs command via ovn-sbctl with a specific timeout
// FIXME: Remove when https://github.com/ovn-kubernetes/libovsdb/issues/235 is fixed
func RunOVNSbctlWithTimeout(timeout int, args ...string) (string, string,
	error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", timeout)}
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := runOVNretry(runner.sbctlPath, nil, nil, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVSDBClient runs an 'ovsdb-client [OPTIONS] COMMAND [ARG...] command'.
func RunOVSDBClient(args ...string) (string, string, error) {
	stdout, stderr, err := runOVNretry(runner.ovsdbClientPath, nil, nil, args...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNSbctl runs a command via ovn-sbctl.
// FIXME: Remove when https://github.com/ovn-kubernetes/libovsdb/issues/235 is fixed
func RunOVNSbctl(args ...string) (string, string, error) {
	return RunOVNSbctlWithTimeout(ovsCommandTimeout, args...)
}

// RunOVNNBAppCtlWithTimeout runs an ovn-appctl command with a timeout to nbdb
func RunOVNNBAppCtlWithTimeout(timeout int, args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", timeout)}
	cmdArgs = append(cmdArgs, args...)
	return RunOVNNBAppCtl(cmdArgs...)
}

// RunOVNNBAppCtl runs an 'ovn-appctl -t nbdbCtlFileName command'.
func RunOVNNBAppCtl(args ...string) (string, string, error) {
	getSocketPath := func() ([]string, error) {
		return []string{
			"-t",
			runner.ovnRunDir + nbdbCtlFileName,
		}, nil
	}
	stdout, stderr, err := runOVNretry(runner.ovnappctlPath, nil, getSocketPath, args...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNSBAppCtlWithTimeout runs an ovn-appctl command with a timeout to sbdb
func RunOVNSBAppCtlWithTimeout(timeout int, args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", timeout)}
	cmdArgs = append(cmdArgs, args...)
	return RunOVNSBAppCtl(cmdArgs...)
}

// RunOVNSBAppCtl runs an 'ovn-appctl -t sbdbCtlFileName command'.
func RunOVNSBAppCtl(args ...string) (string, string, error) {
	getSocketPath := func() ([]string, error) {
		return []string{
			"-t",
			runner.ovnRunDir + sbdbCtlFileName,
		}, nil
	}
	stdout, stderr, err := runOVNretry(runner.ovnappctlPath, nil, getSocketPath, args...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNNorthAppCtlWithTimeout runs an 'ovs-appctl -t ovn-northd command' with a
// --timeout so the subprocess self-terminates instead of blocking indefinitely on
// a busy daemon.
func RunOVNNorthAppCtlWithTimeout(timeout int, args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", timeout)}
	cmdArgs = append(cmdArgs, args...)
	return RunOVNNorthAppCtl(cmdArgs...)
}

// RunOVNNorthAppCtl runs an 'ovs-appctl -t ovn-northd command'.
// TODO: Currently no module is invoking this function, will need to consider adding an unit test when actively used
func RunOVNNorthAppCtl(args ...string) (string, string, error) {
	getSocketPath := func() ([]string, error) {
		pid, err := afero.ReadFile(AppFs, runner.ovnRunDir+"ovn-northd.pid")
		if err != nil {
			return nil, fmt.Errorf("failed to run the command since failed to get ovn-northd's pid: %v", err)
		}
		return []string{
			"-t",
			runner.ovnRunDir + fmt.Sprintf("ovn-northd.%s.ctl", strings.TrimSpace(string(pid))),
		}, nil
	}
	stdout, stderr, err := runOVNretry(runner.ovnappctlPath, nil, getSocketPath, args...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNControllerAppCtlWithTimeout runs an 'ovs-appctl -t ovn-controller.pid.ctl
// command' with a --timeout so the subprocess self-terminates instead of blocking
// indefinitely on a busy daemon.
func RunOVNControllerAppCtlWithTimeout(timeout int, args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", timeout)}
	cmdArgs = append(cmdArgs, args...)
	return RunOVNControllerAppCtl(cmdArgs...)
}

// RunOVNControllerAppCtl runs an 'ovs-appctl -t ovn-controller.pid.ctl command'.
func RunOVNControllerAppCtl(args ...string) (string, string, error) {
	getSocketPath := func() ([]string, error) {
		pid, err := afero.ReadFile(AppFs, runner.ovnRunDir+"ovn-controller.pid")
		if err != nil {
			return nil, fmt.Errorf("failed to get ovn-controller pid : %v", err)
		}
		return []string{
			"-t",
			runner.ovnRunDir + fmt.Sprintf("ovn-controller.%s.ctl", strings.TrimSpace(string(pid))),
		}, nil
	}
	stdout, stderr, err := runOVNretry(runner.ovnappctlPath, nil, getSocketPath, args...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOvsVswitchdAppCtlWithTimeout runs an 'ovs-appctl -t ovs-vswitchd.pid.ctl command'
// with a --timeout so the subprocess self-terminates instead of blocking indefinitely
// on a busy daemon.
func RunOvsVswitchdAppCtlWithTimeout(timeout int, args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", timeout)}
	cmdArgs = append(cmdArgs, args...)
	return RunOvsVswitchdAppCtl(cmdArgs...)
}

// RunOvsVswitchdAppCtl runs an 'ovs-appctl -t /var/run/openvsiwthc/ovs-vswitchd.pid.ctl command'
func RunOvsVswitchdAppCtl(args ...string) (string, string, error) {
	getSocketPath := func() ([]string, error) {
		pid, err := GetOvsVSwitchdPID()
		if err != nil {
			return nil, err
		}
		return []string{
			"-t",
			filepath.Join(config.OvsPaths.RunDir, fmt.Sprintf("ovs-vswitchd.%s.ctl", pid)),
		}, nil
	}
	stdout, stderr, err := runOVNretry(runner.appctlPath, nil, getSocketPath, args...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// GetOvsVSwitchdPID retrieves the Process IDentifier for ovs-vswitchd daemon.
func GetOvsVSwitchdPID() (string, error) {
	pid, err := afero.ReadFile(AppFs, filepath.Join(config.OvsPaths.RunDir, "ovs-vswitchd.pid"))
	if err != nil {
		return "", fmt.Errorf("failed to get ovs-vswitch pid : %v", err)
	}

	return strings.TrimSpace(string(pid)), nil
}

// GetOvsDBServerPID retrieves the Process IDentifier for ovs-vswitchd daemon.
func GetOvsDBServerPID() (string, error) {
	pid, err := afero.ReadFile(AppFs, filepath.Join(config.OvsPaths.RunDir, "ovsdb-server.pid"))
	if err != nil {
		return "", fmt.Errorf("failed to get ovsdb-server pid : %v", err)
	}

	return strings.TrimSpace(string(pid)), nil
}

// RunIP runs a command via the iproute2 "ip" utility
func RunIP(args ...string) (string, string, error) {
	stdout, stderr, err := run(runner.ipPath, args...)
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

// RunSysctl runs a command via the procps "sysctl" utility
func RunSysctl(args ...string) (string, string, error) {
	stdout, stderr, err := run(runner.sysctlPath, args...)
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

// RunPowershell runs a command via the Windows powershell utility
func RunPowershell(args ...string) (string, string, error) {
	stdout, stderr, err := run(runner.powershellPath, args...)
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

// RunNetsh runs a command via the Windows netsh utility
func RunNetsh(args ...string) (string, string, error) {
	stdout, stderr, err := run(runner.netshPath, args...)
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

// RunRoute runs a command via the Windows route utility
func RunRoute(args ...string) (string, string, error) {
	stdout, stderr, err := run(runner.routePath, args...)
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

// AddOFFlowWithSpecificAction replaces flows in the bridge by a single flow with a
// specified action
func AddOFFlowWithSpecificAction(bridgeName, action string) (string, string, error) {
	args := []string{"-O", "OpenFlow13", "replace-flows", bridgeName, "-"}

	stdin := &bytes.Buffer{}
	stdin.Write([]byte(fmt.Sprintf("table=0,priority=0,actions=%s\n", action)))

	cmd := runner.exec.Command(runner.ofctlPath, args...)
	cmd.SetStdin(stdin)
	stdout, stderr, err := runCmd(cmd, runner.ofctlPath, args...)
	return strings.Trim(stdout.String(), "\" \n"), stderr.String(), err
}

// openFlowStdinReader incrementally renders a flow slice as a newline-delimited
// stream for ovs-ofctl stdin without constructing one large joined string.
type openFlowStdinReader struct {
	flows      []string
	flowIndex  int
	flowOffset int
	needEOL    bool
}

// Read implements io.Reader over r.flows, producing output equivalent to
// strings.Join(flows, "\n"), but in small chunks to reduce peak allocations.
func (r *openFlowStdinReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Fast path: no flows left and no pending delimiter.
	if r.flowIndex >= len(r.flows) && !r.needEOL {
		return 0, io.EOF
	}

	total := 0
	for total < len(p) {
		if r.needEOL {
			// Emit exactly one '\n' between flows.
			p[total] = '\n'
			total++
			r.needEOL = false
			if total == len(p) {
				return total, nil
			}
			continue
		}

		if r.flowIndex >= len(r.flows) {
			break
		}

		flow := r.flows[r.flowIndex]
		if r.flowOffset >= len(flow) {
			// Current flow was fully consumed; advance and schedule delimiter if
			// there is another flow.
			r.flowIndex++
			r.flowOffset = 0
			r.needEOL = r.flowIndex < len(r.flows)
			continue
		}

		// Copy as much of the current flow as fits in caller's buffer.
		copied := copy(p[total:], flow[r.flowOffset:])
		total += copied
		r.flowOffset += copied
	}

	if total == 0 {
		return 0, io.EOF
	}
	return total, nil
}

// ReplaceOFFlows replaces flows in the bridge with a slice of flows
func ReplaceOFFlows(bridgeName string, flows []string) (string, string, error) {
	args := []string{"-O", "OpenFlow13", "--bundle", "replace-flows", bridgeName, "-"}
	stdin := &openFlowStdinReader{flows: flows}

	cmd := runner.exec.Command(runner.ofctlPath, args...)
	cmd.SetStdin(stdin)
	stdout, stderr, err := runCmd(cmd, runner.ofctlPath, args...)
	return strings.Trim(stdout.String(), "\" \n"), stderr.String(), err
}

// AddOrModOFGroup creates or modifies an OpenFlow group on the bridge.
func AddOrModOFGroup(bridgeName, group string) (string, string, error) {
	return RunOVSOfctl("-O", "OpenFlow13", "--may-create", "mod-group", bridgeName, group)
}

// DeleteOFGroup deletes an OpenFlow group from the bridge by group ID.
func DeleteOFGroup(bridgeName, groupID string) (string, string, error) {
	return RunOVSOfctl("-O", "OpenFlow13", "del-groups", bridgeName, fmt.Sprintf("group_id=%s", groupID))
}

// GetOFFlows gets all the flows from a bridge
func GetOFFlows(bridgeName string) ([]string, error) {
	stdout, stderr, err := RunOVSOfctl("dump-flows", bridgeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get flows on bridge %q:, stderr: %q, error: %v",
			bridgeName, stderr, err)
	}

	var flows []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "cookie=") {
			flows = append(flows, strings.TrimSpace(line))
		}
	}

	return flows, nil
}

// GetOpenFlowPorts names or numbers for a given bridge
func GetOpenFlowPorts(bridgeName string, namedPorts bool) ([]string, error) {
	stdout, stderr, err := RunOVSOfctl("show", bridgeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get list of ports on bridge %q:, stderr: %q, error: %v",
			bridgeName, stderr, err)
	}

	index := 0
	if namedPorts {
		index = 1
	}
	var ports []string
	re := regexp.MustCompile("[(|)]")
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "addr:") {
			port := strings.TrimSpace(
				re.Split(line, -1)[index],
			)
			ports = append(ports, port)
		}
	}
	return ports, nil
}

// GetOvnRunDir returns the OVN's rundir.
func GetOvnRunDir() string {
	return runner.ovnRunDir
}

// DetectCheckPktLengthSupport checks if OVN supports check packet length action in OVS kernel datapath
func DetectCheckPktLengthSupport(bridge string) (bool, error) {
	stdout, stderr, err := RunOvsVswitchdAppCtl("dpif/show-dp-features", bridge)
	if err != nil {
		klog.Errorf("Failed to query OVS for check packet length support, "+
			"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return false, err
	}

	re := regexp.MustCompile(`(?i)yes|(?i)true`)

	for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		if strings.Contains(line, "Check pkt length action") && re.MatchString(line) {
			return true, nil
		}
	}

	return false, nil
}

// SetStaticFDBEntry programs a static MAC entry into the OVS FIB and disables MAC learning for this entry.
func SetStaticFDBEntry(bridge, port string, mac net.HardwareAddr, vlanID uint) error {
	vlan := fmt.Sprintf("%d", vlanID)
	stdout, stderr, err := RunOvsVswitchdAppCtl("fdb/add", bridge, port, vlan, mac.String())
	if err != nil {
		return fmt.Errorf("failed to add FDB entry to OVS for port %s, "+
			"stdout: %q, stderr: %q, error: %v", port, stdout, stderr, err)
	}
	return nil
}

type OvsDbProperties struct {
	AppCtl  func(timeout int, args ...string) (string, string, error)
	DbAlias string
	DbName  string
}

// GetOvsDbProperties inits OvsDbProperties based on db file path given to it.
// Now it only works with ovn dbs (nbdb and sbdb)
func GetOvsDbProperties(db string) (*OvsDbProperties, error) {
	if strings.Contains(db, "ovnnb") {
		return &OvsDbProperties{
			AppCtl:  RunOVNNBAppCtlWithTimeout,
			DbName:  "OVN_Northbound",
			DbAlias: db,
		}, nil
	} else if strings.Contains(db, "ovnsb") {
		return &OvsDbProperties{
			AppCtl:  RunOVNSBAppCtlWithTimeout,
			DbName:  "OVN_Southbound",
			DbAlias: db,
		}, nil
	} else {
		return nil, fmt.Errorf("failed to parse ovn db type Northbound/Southbound from the path %s", db)
	}
}

// GetExternalIDValByKey returns the value of the specified key in a space separated string (each in the form of k=v)
func GetExternalIDValByKey(keyValString, key string) string {
	keyVals := strings.Fields(keyValString)
	for _, keyVal := range keyVals {
		if strings.HasPrefix(keyVal, key+"=") {
			return strings.TrimPrefix(keyVal, key+"=")
		}
	}
	return ""
}

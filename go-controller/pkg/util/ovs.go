package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/afero"

	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"
)

const (
	// On Windows we need an increased timeout on OVS commands, because
	// adding internal ports on a non Hyper-V enabled host will call
	// external Powershell commandlets.
	// TODO: Decrease the timeout once port adding is improved on Windows
	ovsCommandTimeout  = 15
	ovsVsctlCommand    = "ovs-vsctl"
	ovsOfctlCommand    = "ovs-ofctl"
	ovsAppctlCommand   = "ovs-appctl"
	ovnNbctlCommand    = "ovn-nbctl"
	ovnSbctlCommand    = "ovn-sbctl"
	ovnAppctlCommand   = "ovn-appctl"
	ovsdbClientCommand = "ovsdb-client"
	ovsdbToolCommand   = "ovsdb-tool"
	ipCommand          = "ip"
	powershellCommand  = "powershell"
	netshCommand       = "netsh"
	routeCommand       = "route"
	osRelease          = "/etc/os-release"
	rhel               = "RHEL"
	ubuntu             = "Ubuntu"
	windowsOS          = "windows"
	defaultOSMaxArgs   = 262144
	minOSArgs          = 1000
)

const (
	nbdbCtlSock     = "ovnnb_db.ctl"
	sbdbCtlSock     = "ovnsb_db.ctl"
	OvnNbdbLocation = "/etc/ovn/ovnnb_db.db"
	OvnSbdbLocation = "/etc/ovn/ovnsb_db.db"
	FloodAction     = "FLOOD"
	NormalAction    = "NORMAL"
)

var (
	// These are variables (not constants) so that testcases can modify them
	ovsRunDir string = "/var/run/openvswitch/"
	ovnRunDir string = "/var/run/ovn/"

	savedOVSRunDir = ovsRunDir
	savedOVNRunDir = ovnRunDir
)

var ovnCmdRetryCount = 200
var AppFs = afero.NewOsFs()

var MaxArgsError = errors.New("requested transaction exceeds maximum arguments")

// PrepareTestConfig restores default config values. Used by testcases to
// provide a pristine environment between tests.
func PrepareTestConfig() {
	ovsRunDir = savedOVSRunDir
	ovnRunDir = savedOVNRunDir
}

// this metric is set only for the ovnkube in master mode since 99.9% of
// all the ovn-nbctl/ovn-sbctl calls occur on the master
var MetricOvnCliLatency *prometheus.HistogramVec

var maxArgs int

func init() {
	maxArgs = findMaxArgsUsable(defaultOSMaxArgs)
	klog.Infof("Maximum command line arguments set to: %d", maxArgs)
}

// findMaxArgsUsable finds the maximum amount of usable args on the system, which may be
// different than what the kernel returns for ARG_MAX
func findMaxArgsUsable(estimatedMax int) int {
	backoff := .9
	args := make([]string, estimatedMax)
	for i := range args {
		args[i] = "a"
	}

	for estimatedMax > minOSArgs {
		if _, err := exec.Command("/bin/true", args...).Output(); err == nil {
			break
		}
		estimatedMax = int(float64(estimatedMax) * backoff)
		args = args[:estimatedMax]
	}

	if estimatedMax < minOSArgs {
		estimatedMax = minOSArgs
	}
	return estimatedMax
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
	vsctlPath       string
	appctlPath      string
	ovnappctlPath   string
	nbctlPath       string
	sbctlPath       string
	ovnctlPath      string
	ovsdbClientPath string
	ovsdbToolPath   string
	ovnRunDir       string
	ipPath          string
	powershellPath  string
	netshPath       string
	routePath       string
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

var runCmdExecRunner ExecRunner = &defaultExecRunner{}

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
	runner.vsctlPath, err = exec.LookPath(ovsVsctlCommand)
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
		runner.ovnRunDir = ovsRunDir
	} else {
		// If ovn-appctl command is available, it means OVN
		// has its own separate rundir, logdir, sharedir.
		runner.ovnctlPath = "/usr/share/ovn/scripts/ovn-ctl"
		runner.ovnRunDir = ovnRunDir
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
	runner.ovsdbToolPath, err = exec.LookPath(ovsdbToolCommand)
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
	}
	return nil
}

// SetSpecificExec validates executable paths for selected commands. It also saves the given
// exec interface to be used for running selected commands
func SetSpecificExec(exec kexec.Interface, commands ...string) error {
	var err error

	runner = &execHelper{exec: exec}
	for _, command := range commands {
		switch command {
		case ovsVsctlCommand:
			runner.vsctlPath, err = exec.LookPath(ovsVsctlCommand)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown command: %q", command)
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
	return runCmdExecRunner.RunCmd(cmd, cmdPath, []string{}, args...)
}

func run(cmdPath string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	cmd := runner.exec.Command(cmdPath, args...)
	return runCmdExecRunner.RunCmd(cmd, cmdPath, []string{}, args...)
}

func runWithEnvVars(cmdPath string, envVars []string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	cmd := runner.exec.Command(cmdPath, args...)
	return runCmdExecRunner.RunCmd(cmd, cmdPath, envVars, args...)
}

// RunOVSOfctl runs a command via ovs-ofctl.
func RunOVSOfctl(args ...string) (string, string, error) {
	stdout, stderr, err := run(runner.ofctlPath, args...)
	return strings.Trim(stdout.String(), "\" \n"), stderr.String(), err
}

// RunOVSVsctl runs a command via ovs-vsctl.
func RunOVSVsctl(args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", ovsCommandTimeout)}
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := run(runner.vsctlPath, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// GetOVSOfPort runs get ofport via ovs-vsctl and handle special return strings.
func GetOVSOfPort(args ...string) (string, string, error) {
	stdout, stderr, err := RunOVSVsctl(args...)
	if stdout == "[]" || stdout == "-1" {
		err = fmt.Errorf("%s return invalid result %s err %s", args, stdout, err)
	}
	return stdout, stderr, err
}

// RunOVSAppctlWithTimeout runs a command via ovs-appctl.
func RunOVSAppctlWithTimeout(timeout int, args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", timeout)}
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := run(runner.appctlPath, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVSAppctl runs a command via ovs-appctl.
func RunOVSAppctl(args ...string) (string, string, error) {
	return RunOVSAppctlWithTimeout(ovsCommandTimeout, args...)
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
// FIXME: Remove when https://github.com/ovn-org/libovsdb/issues/235 is fixed
func runOVNretry(cmdPath string, envVars []string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {

	retriesLeft := ovnCmdRetryCount
	for {
		stdout, stderr, err := runWithEnvVars(cmdPath, envVars, args...)
		if err == nil {
			return stdout, stderr, err
		}

		// Connection refused
		// Master may not be up so keep trying
		if strings.Contains(stderr.String(), "Connection refused") {
			if retriesLeft == 0 {
				return stdout, stderr, err
			}
			retriesLeft--
			time.Sleep(2 * time.Second)
		} else {
			// Some other problem for caller to handle
			return stdout, stderr, fmt.Errorf("OVN command '%s %s' failed: %s", cmdPath, strings.Join(args, " "), err)
		}
	}
}

func getNbctlArgsAndEnv(timeout int, args ...string) ([]string, []string) {
	var cmdArgs []string

	if config.OvnNorth.Scheme == config.OvnDBSchemeSSL {
		cmdArgs = append(cmdArgs,
			fmt.Sprintf("--private-key=%s", config.OvnNorth.PrivKey),
			fmt.Sprintf("--certificate=%s", config.OvnNorth.Cert),
			fmt.Sprintf("--bootstrap-ca-cert=%s", config.OvnNorth.CACert),
			fmt.Sprintf("--db=%s", config.OvnNorth.GetURL()))
	} else if config.OvnNorth.Scheme == config.OvnDBSchemeTCP {
		cmdArgs = append(cmdArgs, fmt.Sprintf("--db=%s", config.OvnNorth.GetURL()))
	}
	cmdArgs = append(cmdArgs, fmt.Sprintf("--timeout=%d", timeout))
	cmdArgs = append(cmdArgs, args...)
	return cmdArgs, []string{}
}

func getNbOVSDBArgs(command string, args ...string) []string {
	var cmdArgs []string
	if config.OvnNorth.Scheme == config.OvnDBSchemeSSL {
		cmdArgs = append(cmdArgs,
			fmt.Sprintf("--private-key=%s", config.OvnNorth.PrivKey),
			fmt.Sprintf("--certificate=%s", config.OvnNorth.Cert),
			fmt.Sprintf("--bootstrap-ca-cert=%s", config.OvnNorth.CACert))
	}
	cmdArgs = append(cmdArgs, command)
	cmdArgs = append(cmdArgs, config.OvnNorth.GetURL())
	cmdArgs = append(cmdArgs, args...)
	return cmdArgs
}

// RunOVNNbctlWithTimeout runs command via ovn-nbctl with a specific timeout
// FIXME: Remove when https://github.com/ovn-org/libovsdb/issues/235 is fixed
func RunOVNNbctlWithTimeout(timeout int, args ...string) (string, string, error) {
	stdout, stderr, err := RunOVNNbctlRawOutput(timeout, args...)
	return strings.Trim(strings.TrimSpace(stdout), "\""), stderr, err
}

// RunOVNNbctlRawOutput returns the output with no trimming or other string manipulation
// FIXME: Remove when https://github.com/ovn-org/libovsdb/issues/235 is fixed
func RunOVNNbctlRawOutput(timeout int, args ...string) (string, string, error) {
	cmdArgs, envVars := getNbctlArgsAndEnv(timeout, args...)
	start := time.Now()
	stdout, stderr, err := runOVNretry(runner.nbctlPath, envVars, cmdArgs...)
	if MetricOvnCliLatency != nil {
		MetricOvnCliLatency.WithLabelValues("ovn-nbctl").Observe(time.Since(start).Seconds())
	}
	return stdout.String(), stderr.String(), err
}

// RunOVNNbctl runs a command via ovn-nbctl.
// FIXME: Remove when https://github.com/ovn-org/libovsdb/issues/235 is fixed
func RunOVNNbctl(args ...string) (string, string, error) {
	return RunOVNNbctlWithTimeout(ovsCommandTimeout, args...)
}

// RunOVNSbctlWithTimeout runs command via ovn-sbctl with a specific timeout
// FIXME: Remove when https://github.com/ovn-org/libovsdb/issues/235 is fixed
func RunOVNSbctlWithTimeout(timeout int, args ...string) (string, string,
	error) {
	var cmdArgs []string
	if config.OvnSouth.Scheme == config.OvnDBSchemeSSL {
		cmdArgs = []string{
			fmt.Sprintf("--private-key=%s", config.OvnSouth.PrivKey),
			fmt.Sprintf("--certificate=%s", config.OvnSouth.Cert),
			fmt.Sprintf("--bootstrap-ca-cert=%s", config.OvnSouth.CACert),
			fmt.Sprintf("--db=%s", config.OvnSouth.GetURL()),
		}
	} else if config.OvnSouth.Scheme == config.OvnDBSchemeTCP {
		cmdArgs = []string{
			fmt.Sprintf("--db=%s", config.OvnSouth.GetURL()),
		}
	}

	cmdArgs = append(cmdArgs, fmt.Sprintf("--timeout=%d", timeout))
	cmdArgs = append(cmdArgs, "--no-leader-only")
	cmdArgs = append(cmdArgs, args...)
	start := time.Now()
	stdout, stderr, err := runOVNretry(runner.sbctlPath, nil, cmdArgs...)
	if MetricOvnCliLatency != nil {
		MetricOvnCliLatency.WithLabelValues("ovn-sbctl").Observe(time.Since(start).Seconds())
	}
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVSDBClient runs an 'ovsdb-client [OPTIONS] COMMAND [ARG...] command'.
func RunOVSDBClient(args ...string) (string, string, error) {
	stdout, stderr, err := runOVNretry(runner.ovsdbClientPath, nil, args...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVSDBTool runs an 'ovsdb-tool [OPTIONS] COMMAND [ARG...] command'.
func RunOVSDBTool(args ...string) (string, string, error) {
	stdout, stderr, err := run(runner.ovsdbToolPath, args...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVSDBClientOVN runs an 'ovsdb-client [OPTIONS] COMMAND [SERVER] [ARG...] command' against OVN NB database.
func RunOVSDBClientOVNNB(command string, args ...string) (string, string, error) {
	cmdArgs := getNbOVSDBArgs(command, args...)
	stdout, stderr, err := runOVNretry(runner.ovsdbClientPath, nil, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNSbctl runs a command via ovn-sbctl.
// FIXME: Remove when https://github.com/ovn-org/libovsdb/issues/235 is fixed
func RunOVNSbctl(args ...string) (string, string, error) {
	return RunOVNSbctlWithTimeout(ovsCommandTimeout, args...)
}

// RunOVNNBAppCtlWithTimeout runs an ovn-appctl command with a timeout to nbdb
func RunOVNNBAppCtlWithTimeout(timeout int, args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", timeout)}
	cmdArgs = append(cmdArgs, args...)
	return RunOVNNBAppCtl(cmdArgs...)
}

// RunOVNNBAppCtl runs an 'ovn-appctl -t nbdbCtlSockPath command'.
func RunOVNNBAppCtl(args ...string) (string, string, error) {
	var cmdArgs []string
	cmdArgs = []string{
		"-t",
		runner.ovnRunDir + nbdbCtlSock,
	}
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := runOVNretry(runner.ovnappctlPath, nil, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNSBAppCtlWithTimeout runs an ovn-appctl command with a timeout to sbdb
func RunOVNSBAppCtlWithTimeout(timeout int, args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", timeout)}
	cmdArgs = append(cmdArgs, args...)
	return RunOVNSBAppCtl(cmdArgs...)
}

// RunOVNSBAppCtl runs an 'ovn-appctl -t sbdbCtlSockPath command'.
func RunOVNSBAppCtl(args ...string) (string, string, error) {
	var cmdArgs []string
	cmdArgs = []string{
		"-t",
		runner.ovnRunDir + sbdbCtlSock,
	}
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := runOVNretry(runner.ovnappctlPath, nil, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNNorthAppCtl runs an 'ovs-appctl -t ovn-northd command'.
// TODO: Currently no module is invoking this function, will need to consider adding an unit test when actively used
func RunOVNNorthAppCtl(args ...string) (string, string, error) {
	var cmdArgs []string

	pid, err := afero.ReadFile(AppFs, runner.ovnRunDir+"ovn-northd.pid")
	if err != nil {
		return "", "", fmt.Errorf("failed to run the command since failed to get ovn-northd's pid: %v", err)
	}

	cmdArgs = []string{
		"-t",
		runner.ovnRunDir + fmt.Sprintf("ovn-northd.%s.ctl", strings.TrimSpace(string(pid))),
	}
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := runOVNretry(runner.ovnappctlPath, nil, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNControllerAppCtl runs an 'ovs-appctl -t ovn-controller.pid.ctl command'.
func RunOVNControllerAppCtl(args ...string) (string, string, error) {
	var cmdArgs []string
	pid, err := afero.ReadFile(AppFs, runner.ovnRunDir+"ovn-controller.pid")
	if err != nil {
		return "", "", fmt.Errorf("failed to get ovn-controller pid : %v", err)
	}
	cmdArgs = []string{
		"-t",
		runner.ovnRunDir + fmt.Sprintf("ovn-controller.%s.ctl", strings.TrimSpace(string(pid))),
	}
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := runOVNretry(runner.ovnappctlPath, nil, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOvsVswitchdAppCtl runs an 'ovs-appctl -t /var/run/openvsiwthc/ovs-vswitchd.pid.ctl command'
func RunOvsVswitchdAppCtl(args ...string) (string, string, error) {
	var cmdArgs []string
	pid, err := afero.ReadFile(AppFs, savedOVSRunDir+"ovs-vswitchd.pid")
	if err != nil {
		return "", "", fmt.Errorf("failed to get ovs-vswitch pid : %v", err)
	}
	cmdArgs = []string{
		"-t",
		savedOVSRunDir + fmt.Sprintf("ovs-vswitchd.%s.ctl", strings.TrimSpace(string(pid))),
	}
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := runOVNretry(runner.appctlPath, nil, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunIP runs a command via the iproute2 "ip" utility
func RunIP(args ...string) (string, string, error) {
	stdout, stderr, err := run(runner.ipPath, args...)
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

// ReplaceOFFlows replaces flows in the bridge with a slice of flows
func ReplaceOFFlows(bridgeName string, flows []string) (string, string, error) {
	args := []string{"-O", "OpenFlow13", "--bundle", "replace-flows", bridgeName, "-"}
	stdin := &bytes.Buffer{}
	stdin.Write([]byte(strings.Join(flows, "\n")))

	cmd := runner.exec.Command(runner.ofctlPath, args...)
	cmd.SetStdin(stdin)
	stdout, stderr, err := runCmd(cmd, runner.ofctlPath, args...)
	return strings.Trim(stdout.String(), "\" \n"), stderr.String(), err
}

// Get OpenFlow Port names or numbers for a given bridge
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

// ovsdb-server(5) says a clustered database is connected if the server
// is in contact with a majority of its cluster.
type OVNDBServerStatus struct {
	Connected bool
	Leader    bool
	Index     int
}

// Internal structure that holds the un-marshaled json output from the
// ovsdb-client query command. The Index can hold ["set": []] when it is
// not populated yet, so we need to use `interface{}` type. However, we
// don't want our callers to worry about all this and we want them to see the
// Index as an integer and hence we use an exported OVNDBServerStatus for that
type dbRow struct {
	Connected bool        `json:"connected"`
	Leader    bool        `json:"leader"`
	Index     interface{} `json:"index"`
}

type queryResult struct {
	Rows []dbRow `json:"rows"`
}

func GetOVNDBServerInfo(timeout int, direction, database string) (*OVNDBServerStatus, error) {
	sockPath := fmt.Sprintf("unix:/var/run/openvswitch/ovn%s_db.sock", direction)
	transact := fmt.Sprintf(`["_Server", {"op":"select", "table":"Database", "where":[["name", "==", "%s"]], `+
		`"columns": ["connected", "leader", "index"]}]`, database)

	stdout, stderr, err := RunOVSDBClient(fmt.Sprintf("--timeout=%d", timeout), "query", sockPath, transact)
	if err != nil {
		return nil, fmt.Errorf("failed to get %q ovsdb-server status: stderr(%s), err(%v)",
			direction, stderr, err)
	}

	var result []queryResult
	err = json.Unmarshal([]byte(stdout), &result)
	if err != nil {
		return nil, fmt.Errorf("failed to parse the json output(%s) from ovsdb-client command for database %q: %v",
			stdout, database, err)
	}
	if len(result) != 1 || len(result[0].Rows) != 1 {
		return nil, fmt.Errorf("parsed json output for %q ovsdb-server has incorrect status information",
			direction)
	}
	serverStatus := &OVNDBServerStatus{}
	serverStatus.Connected = result[0].Rows[0].Connected
	serverStatus.Leader = result[0].Rows[0].Leader
	if index, ok := result[0].Rows[0].Index.(float64); ok {
		serverStatus.Index = int(index)
	} else {
		serverStatus.Index = 0
	}

	return serverStatus, nil
}

// DetectSCTPSupport checks if OVN supports SCTP for load balancer
func DetectSCTPSupport() (bool, error) {
	stdout, stderr, err := RunOVSDBClientOVNNB("list-columns", "--data=bare", "--no-heading",
		"--format=json", "OVN_Northbound", "Load_Balancer")
	if err != nil {
		klog.Errorf("Failed to query OVN NB DB for SCTP support, "+
			"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return false, err
	}
	type OvsdbData struct {
		Data [][]interface{}
	}
	var lbData OvsdbData
	err = json.Unmarshal([]byte(stdout), &lbData)
	if err != nil {
		return false, err
	}
	for _, entry := range lbData.Data {
		if entry[0].(string) == "protocol" && strings.Contains(fmt.Sprintf("%v", entry[1]), "sctp") {
			return true, nil
		}
	}
	return false, nil
}

// NBTxn hold parts of an ovn-nbctl transaction request
type NBTxn struct {
	args    []string
	txnArgs []string
	env     []string
}

// NewNBTxn returns a new ovn-nbctl transaction request object
func NewNBTxn() *NBTxn {
	args, env := getNbctlArgsAndEnv(ovsCommandTimeout, []string{}...)
	return &NBTxn{
		args: args,
		env:  env,
	}
}

// Add adds a new request to the transaction
func (t *NBTxn) add(args ...string) {
	if len(t.txnArgs) > 0 {
		t.txnArgs = append(t.txnArgs, "--")
	}
	t.txnArgs = append(t.txnArgs, args...)
}

// Commit commits all parts of the transaction and returns output and errors
func (t *NBTxn) Commit() (string, string, error) {
	if len(t.txnArgs) == 0 {
		return "", "", nil
	}
	allArgs := append(t.args, t.txnArgs...)
	stdout, stderr, err := runOVNretry(runner.nbctlPath, t.env, allArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// AddOrCommit adds a slice of requests to a transaction
// If the incoming slice to be added would be greater than the maximum
// number of arguments for a transaction; the transaction is committed
// and the current transactions arguments are reset to the slice
// Note: This method should be called once with a slice of dependent args
// For example, using create --id=@acl with dependent add to switch cmds
// should all be added in a single AddOrCommit call
// The caller should take care not to overload the call with a too large
// slice exceeding max args, or the command can never be committed
func (t *NBTxn) AddOrCommit(args []string) (string, string, error) {
	if len(args) > maxArgs {
		return "", "", MaxArgsError
	}
	// assume a 10 argument buffer for other arguments by default added to the nbctl call
	buffer := 10
	incomingLength := len(args)
	if len(t.txnArgs) > 0 {
		// increment for --
		incomingLength += 1
	}

	klog.V(5).Infof("Number of args: %d, txnArgs: %d, incomingLen: %d, buffer: %d", len(t.args),
		len(t.txnArgs), incomingLength, buffer)

	// case where we are going to exceed max arguments
	// also check entire line length is going be over 100k
	// maximum bash command seems to be a combination of max args and length of each argument
	// maximum length is PAGE_SIZE * 32 which we can assume to be 4k page, and equals 131072
	if len(t.args)+len(t.txnArgs)+incomingLength+buffer > maxArgs || len(strings.Join(t.args, " "))+
		len(strings.Join(t.txnArgs, " "))+len(strings.Join(args, " ")) > 100000 {
		klog.Info("Requested transaction add is too large, committing...")
		if stdout, stderr, err := t.Commit(); err != nil {
			return stdout, stderr, err
		}
		// reset txnArgs
		t.txnArgs = []string{}
	}

	t.add(args...)
	return "", "", nil
}

// DetectCheckPktLengthSupport checks if OVN supports check packet length action in OVS kernel datapath
func DetectCheckPktLengthSupport(bridge string) (bool, error) {
	stdout, stderr, err := RunOVSAppctl("dpif/show-dp-features", bridge)
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

type OvsDbProperties struct {
	AppCtl        func(timeout int, args ...string) (string, string, error)
	DbAlias       string
	DbName        string
	ElectionTimer int
}

// GetOvsDbProperties inits OvsDbProperties based on db file path given to it.
// Now it only works with ovn dbs (nbdb and sbdb)
func GetOvsDbProperties(db string) (*OvsDbProperties, error) {
	if strings.Contains(db, "ovnnb") {
		return &OvsDbProperties{
			ElectionTimer: int(config.OvnNorth.ElectionTimer) * 1000,
			AppCtl:        RunOVNNBAppCtlWithTimeout,
			DbName:        "OVN_Northbound",
			DbAlias:       db,
		}, nil
	} else if strings.Contains(db, "ovnsb") {
		return &OvsDbProperties{
			ElectionTimer: int(config.OvnSouth.ElectionTimer) * 1000,
			AppCtl:        RunOVNSBAppCtlWithTimeout,
			DbName:        "OVN_Southbound",
			DbAlias:       db,
		}, nil
	} else {
		return nil, fmt.Errorf("failed to parse ovn db type Northbound/Southbound from the path %s", db)
	}
}

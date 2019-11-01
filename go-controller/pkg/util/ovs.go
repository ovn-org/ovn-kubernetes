package util

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/sirupsen/logrus"
	kexec "k8s.io/utils/exec"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

const (
	// On Windows we need an increased timeout on OVS commands, because
	// adding internal ports on a non Hyper-V enabled host will call
	// external Powershell commandlets.
	// TODO: Decrease the timeout once port adding is improved on Windows
	ovsCommandTimeout = 15
	ovsVsctlCommand   = "ovs-vsctl"
	ovsOfctlCommand   = "ovs-ofctl"
	ovsAppctlCommand  = "ovs-appctl"
	ovnNbctlCommand   = "ovn-nbctl"
	ovnSbctlCommand   = "ovn-sbctl"
	ipCommand         = "ip"
	powershellCommand = "powershell"
	netshCommand      = "netsh"
	routeCommand      = "route"
	osRelease         = "/etc/os-release"
	rhel              = "RHEL"
	ubuntu            = "Ubuntu"
	windowsOS         = "windows"
)

func runningPlatform() (string, error) {
	if runtime.GOOS == windowsOS {
		return windowsOS, nil
	}
	fileContents, err := ioutil.ReadFile(osRelease)
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
	return "", fmt.Errorf("Unknown platform")
}

// Exec runs various OVN and OVS utilities
type execHelper struct {
	exec           kexec.Interface
	ofctlPath      string
	vsctlPath      string
	appctlPath     string
	nbctlPath      string
	sbctlPath      string
	ipPath         string
	powershellPath string
	netshPath      string
	routePath      string
}

var runner *execHelper

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
	runner.nbctlPath, err = exec.LookPath(ovnNbctlCommand)
	if err != nil {
		return err
	}
	runner.sbctlPath, err = exec.LookPath(ovnSbctlCommand)
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

// GetExec returns the exec interface which can be used for running commands directly.
// Only use for passing an exec interface into pkg/config which cannot call this
// function directly because this module imports pkg/config already.
func GetExec() kexec.Interface {
	return runner.exec
}

var runCounter uint64

func runCmd(cmd kexec.Cmd, cmdPath string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	cmd.SetStdout(stdout)
	cmd.SetStderr(stderr)

	counter := atomic.AddUint64(&runCounter, 1)
	logCmd := fmt.Sprintf("%s %s", cmdPath, strings.Join(args, " "))
	logrus.Debugf("exec(%d): %s", counter, logCmd)

	err := cmd.Run()
	logrus.Debugf("exec(%d): stdout: %q", counter, stdout)
	logrus.Debugf("exec(%d): stderr: %q", counter, stderr)
	if err != nil {
		logrus.Debugf("exec(%d): err: %v", counter, err)
	}
	return stdout, stderr, err
}

func run(cmdPath string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	cmd := runner.exec.Command(cmdPath, args...)
	return runCmd(cmd, cmdPath, args...)
}

func runWithEnvVars(cmdPath string, envVars []string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	cmd := runner.exec.Command(cmdPath, args...)
	cmd.SetEnv(envVars)
	return runCmd(cmd, cmdPath, args...)
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

// RunOVSAppctl runs a command via ovs-appctl.
func RunOVSAppctl(args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", ovsCommandTimeout)}
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := run(runner.appctlPath, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// Run the ovn-ctl command and retry if "Connection refused"
// poll waitng for service to become available
func runOVNretry(cmdPath string, envVars []string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {

	retriesLeft := 200
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
	var cmdArgs, envVars []string

	if config.NbctlDaemonMode {
		// when ovn-nbctl is running in a "daemon mode", the user first starts
		// ovn-nbctl running in the background and afterward uses the daemon to execute
		// operations. The client needs to use the control socket and set the path to the
		// control socket in environment variable OVN_NB_DAEMON
		pid, err := ioutil.ReadFile("/var/run/openvswitch/ovn-nbctl.pid")
		if err == nil {
			envVars = append(envVars,
				fmt.Sprintf("OVN_NB_DAEMON=/var/run/openvswitch/ovn-nbctl.%s.ctl",
					strings.Trim(string(pid), " \n")))
			logrus.Debugf("using ovn-nbctl daemon mode at %s", envVars)
			cmdArgs = append(cmdArgs, fmt.Sprintf("--timeout=%d", timeout))
			cmdArgs = append(cmdArgs, args...)
			return cmdArgs, envVars
		}
		logrus.Warningf("failed to retrieve ovn-nbctl daemon's control socket " +
			"so resorting to non-daemon mode")
	}

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
	return cmdArgs, envVars
}

// RunOVNNbctlUnix runs command via ovn-nbctl, with ovn-nbctl using the unix
// domain sockets to connect to the ovsdb-server backing the OVN NB database.
func RunOVNNbctlUnix(args ...string) (string, string, error) {
	cmdArgs, envVars := getNbctlArgsAndEnv(ovsCommandTimeout, args...)
	stdout, stderr, err := runOVNretry(runner.nbctlPath, envVars, cmdArgs...)
	return strings.Trim(strings.TrimFunc(stdout.String(), unicode.IsSpace), "\""),
		stderr.String(), err
}

// RunOVNNbctlWithTimeout runs command via ovn-nbctl with a specific timeout
func RunOVNNbctlWithTimeout(timeout int, args ...string) (string, string, error) {
	cmdArgs, envVars := getNbctlArgsAndEnv(timeout, args...)
	stdout, stderr, err := runOVNretry(runner.nbctlPath, envVars, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNNbctl runs a command via ovn-nbctl.
func RunOVNNbctl(args ...string) (string, string, error) {
	return RunOVNNbctlWithTimeout(ovsCommandTimeout, args...)
}

// RunOVNSbctlUnix runs command via ovn-sbctl, with ovn-sbctl using the unix
// domain sockets to connect to the ovsdb-server backing the OVN NB database.
func RunOVNSbctlUnix(args ...string) (string, string, error) {
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", ovsCommandTimeout)}
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := runOVNretry(runner.sbctlPath, nil, cmdArgs...)
	return strings.Trim(strings.TrimFunc(stdout.String(), unicode.IsSpace), "\""),
		stderr.String(), err
}

// RunOVNSbctlWithTimeout runs command via ovn-sbctl with a specific timeout
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
	cmdArgs = append(cmdArgs, args...)
	stdout, stderr, err := runOVNretry(runner.sbctlPath, nil, cmdArgs...)
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNSbctl runs a command via ovn-sbctl.
func RunOVNSbctl(args ...string) (string, string, error) {
	return RunOVNSbctlWithTimeout(ovsCommandTimeout, args...)
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

// RawExec runs the given command via the exec interface. Should only be used
// for early calls before configuration is read.
func RawExec(cmdPath string, args ...string) (string, string, error) {
	// If the command is not a path to a binary, try finding it
	if filepath.Base(cmdPath) == cmdPath {
		var err error
		cmdPath, err = runner.exec.LookPath(cmdPath)
		if err != nil {
			return "", "", err
		}
	}
	stdout, stderr, err := run(cmdPath, args...)
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

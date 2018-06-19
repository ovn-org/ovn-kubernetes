package util

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"unicode"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
)

const (
	ovsCommandTimeout = 15
	ovsVsctlCommand   = "ovs-vsctl"
	ovsOfctlCommand   = "ovs-ofctl"
	ovnNbctlCommand   = "ovn-nbctl"
	osRelease         = "/etc/os-release"
	rhel              = "RHEL"
	ubuntu            = "Ubuntu"
	windowsOS         = "windows"
)

// PathExist checks the path exist or not.
func PathExist(path string) bool {
	_, err := os.Stat(path)
	if err != nil && os.IsNotExist(err) {
		return false
	}
	return true
}

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

// RunOVSOfctl runs a command via ovs-ofctl.
func RunOVSOfctl(args ...string) (string, string, error) {
	cmdPath, err := exec.LookPath(ovsOfctlCommand)
	if err != nil {
		return "", "", err
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd := exec.Command(cmdPath, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()
	return strings.Trim(stdout.String(), "\" \n"), stderr.String(), err
}

// RunOVSVsctl runs a command via ovs-vsctl.
func RunOVSVsctl(args ...string) (string, string, error) {
	cmdPath, err := exec.LookPath(ovsVsctlCommand)
	if err != nil {
		return "", "", err
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", ovsCommandTimeout)}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command(cmdPath, cmdArgs...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNNbctlUnix runs command via ovn-nbctl, with ovn-nbctl using the unix
// domain sockets to connect to the ovsdb-server backing the OVN NB database.
func RunOVNNbctlUnix(args ...string) (string, string, error) {
	cmdPath, err := exec.LookPath(ovnNbctlCommand)
	if err != nil {
		return "", "", err
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	cmdArgs := []string{fmt.Sprintf("--timeout=%d", ovsCommandTimeout)}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command(cmdPath, cmdArgs...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()
	return strings.Trim(strings.TrimFunc(stdout.String(), unicode.IsSpace), "\""),
		stderr.String(), err
}

// RunOVNNbctlWithTimeout runs command via ovn-nbctl with a specific timeout
func RunOVNNbctlWithTimeout(timeout int, args ...string) (string, string,
	error) {
	cmdPath, err := exec.LookPath(ovnNbctlCommand)
	if err != nil {
		return "", "", err
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	var cmd *exec.Cmd
	var cmdArgs []string
	if config.OvnNorth.ClientAuth.Scheme == config.OvnDBSchemeSSL {
		cmdArgs = []string{
			fmt.Sprintf("--private-key=%s", config.OvnNorth.ClientAuth.PrivKey),
			fmt.Sprintf("--certificate=%s", config.OvnNorth.ClientAuth.Cert),
			fmt.Sprintf("--bootstrap-ca-cert=%s", config.OvnNorth.ClientAuth.CACert),
			fmt.Sprintf("--db=%s", config.OvnNorth.ClientAuth.GetURL()),
		}
	} else if config.OvnNorth.ClientAuth.Scheme == config.OvnDBSchemeTCP {
		cmdArgs = []string{
			fmt.Sprintf("--db=%s", config.OvnNorth.ClientAuth.GetURL()),
		}
	}

	cmdArgs = append(cmdArgs, fmt.Sprintf("--timeout=%d", timeout))
	cmdArgs = append(cmdArgs, args...)
	cmd = exec.Command(cmdPath, cmdArgs...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNNbctl runs a command via ovn-nbctl.
func RunOVNNbctl(args ...string) (string, string, error) {
	return RunOVNNbctlWithTimeout(ovsCommandTimeout, args...)
}

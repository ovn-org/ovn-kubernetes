package util

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
)

const (
	ovsCommandTimeout = 5
	ovsVsctlCommand   = "ovs-vsctl"
	ovnNbctlCommand   = "ovn-nbctl"
)

// PathExist checks the path exist or not.
func PathExist(path string) bool {
	_, err := os.Stat(path)
	if err != nil && os.IsNotExist(err) {
		return false
	}
	return true
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
	return strings.Trim(stdout.String(), "\" \n"), stderr.String(), err
}

// RunOVNNbctlWithTimeout runs command via ovs-nbctl with a specific timeout
func RunOVNNbctlWithTimeout(timeout int, args ...string) (string, string,
	error) {
	cmdPath, err := exec.LookPath(ovnNbctlCommand)
	if err != nil {
		return "", "", err
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	var cmd *exec.Cmd
	if config.Scheme == "ssl" {
		cmdArgs := []string{
			fmt.Sprintf("--private-key=%s", config.NbctlPrivateKey),
			fmt.Sprintf("--certificate=%s", config.NbctlCertificate),
			fmt.Sprintf("--bootstrap-ca-cert=%s", config.NbctlCACert),
			fmt.Sprintf("--db=%s", config.OvnNB),
			fmt.Sprintf("--timeout=%d", timeout),
		}
		cmdArgs = append(cmdArgs, args...)
		cmd = exec.Command(cmdPath, cmdArgs...)
	} else {
		cmdArgs := []string{
			fmt.Sprintf("--db=%s", config.OvnNB),
			fmt.Sprintf("--timeout=%d", timeout),
		}
		cmdArgs = append(cmdArgs, args...)
		cmd = exec.Command(cmdPath, cmdArgs...)
	}

	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()
	return strings.Trim(stdout.String(), "\" \n"), stderr.String(), err
}

// RunOVNNbctl runs a command via ovs-nbctl.
func RunOVNNbctl(args ...string) (string, string, error) {
	return RunOVNNbctlWithTimeout(ovsCommandTimeout, args...)
}

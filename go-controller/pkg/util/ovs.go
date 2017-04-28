package util

import (
	"bytes"
	"fmt"
	"os/exec"
)

const (
	ovsCommandTimeout = 5
	ovsVsctlCommand   = "ovs-vsctl"
)

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
	return stdout.String(), stderr.String(), err
}

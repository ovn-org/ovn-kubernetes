package cni

import (
	"fmt"
	"strings"

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

	outStr := string(output)
	trimmed := strings.TrimSpace(outStr)
	// If output is a single line, strip the trailing newline
	if strings.Count(trimmed, "\n") == 0 {
		outStr = trimmed
	}

	return outStr, nil
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
	return strings.Split(strings.TrimSpace(output), "\n"), nil
}

func ovsClear(table, record string, columns ...string) error {
	args := append([]string{"--if-exists", "clear", table, record}, columns...)
	_, err := ovsExec(args...)
	return err
}

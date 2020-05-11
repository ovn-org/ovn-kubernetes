package util

import kexec "k8s.io/utils/exec"

type KexecCmdInterface interface {
	SetExec() error
	SetExecWithoutOVS() error
	SetSpecificExec(commands ...string) error
	RunCmd(cmd kexec.Cmd, cmdPath string, envVars []string, args ...string)
}

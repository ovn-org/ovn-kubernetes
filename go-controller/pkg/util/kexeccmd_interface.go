package util

import kexec "k8s.io/utils/exec"

type KexecCmdInterface interface {
	SetExec() error
	SetExecWithoutOVS() error
	SetSpecificExec( commands ...string) error
	//RunCmd( cmdPath string, args ...string)
	RunCmd( cmd kexec.Cmd, cmdPath string, envVars []string, args ...string)
	//Run(cmdPath string, args ...string) (*bytes.Buffer, *bytes.Buffer, error)
	//RunWithEnvVars(cmdPath string, envVars []string, args ...string) (*bytes.Buffer, *bytes.Buffer, error)
}

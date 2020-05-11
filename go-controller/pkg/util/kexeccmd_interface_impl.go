package util

import (
	"bytes"
	"fmt"
	"k8s.io/klog"
	kexec "k8s.io/utils/exec"
	"runtime"
	"strings"
	"sync/atomic"
)

type kexecUtil struct {
	kexecDotInterface kexec.Interface
}

var kexecUtilsInstance *kexecUtil

func GetKexecUtilsInstance() *kexecUtil {
	iface := kexec.New()
	kexecUtilsInstance = &kexecUtil{iface}
	return kexecUtilsInstance
}

// SetExec validates executable paths and saves the given exec interface
// to be used for running various OVS and OVN utilites
func (cie *kexecUtil) SetExec() error {
	err := cie.SetExecWithoutOVS()
	if err != nil {
		return err
	}

	runner.ofctlPath, err = cie.kexecDotInterface.LookPath(ovsOfctlCommand)
	if err != nil {
		return err
	}
	runner.vsctlPath, err = cie.kexecDotInterface.LookPath(ovsVsctlCommand)
	if err != nil {
		return err
	}
	runner.appctlPath, err = cie.kexecDotInterface.LookPath(ovsAppctlCommand)
	if err != nil {
		return err
	}

	runner.ovnappctlPath, err = cie.kexecDotInterface.LookPath(ovnAppctlCommand)
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

	runner.nbctlPath, err = cie.kexecDotInterface.LookPath(ovnNbctlCommand)
	if err != nil {
		return err
	}
	runner.sbctlPath, err = cie.kexecDotInterface.LookPath(ovnSbctlCommand)
	if err != nil {
		return err
	}
	runner.ovsdbClientPath, err = cie.kexecDotInterface.LookPath(ovsdbClientCommand)
	if err != nil {
		return err
	}

	return nil
}

// SetExecWithoutOVS validates executable paths excluding OVS/OVN binaries and
// saves the given exec interface to be used for running various utilites
func (cie *kexecUtil) SetExecWithoutOVS() error {
	var err error

	runner = &execHelper{exec: cie.kexecDotInterface}

	if runtime.GOOS == windowsOS {
		runner.powershellPath, err = cie.kexecDotInterface.LookPath(powershellCommand)
		if err != nil {
			return err
		}
		runner.netshPath, err = cie.kexecDotInterface.LookPath(netshCommand)
		if err != nil {
			return err
		}
		runner.routePath, err = cie.kexecDotInterface.LookPath(routeCommand)
		if err != nil {
			return err
		}
	} else {
		runner.ipPath, err = cie.kexecDotInterface.LookPath(ipCommand)
		if err != nil {
			fmt.Println(err)
			return err
		}
	}
	return nil
}

// SetSpecificExec validates executable paths for selected commands. It also saves the given
// exec interface to be used for running selected commands
func (cie *kexecUtil) SetSpecificExec(commands ...string) error {
	var err error

	runner = &execHelper{exec: cie.kexecDotInterface}
	for _, command := range commands {
		switch command {
		case ovsVsctlCommand:
			runner.vsctlPath, err = cie.kexecDotInterface.LookPath(ovsVsctlCommand)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown command: %q", command)
		}
	}
	return nil
}

func (cie *kexecUtil) RunCmd(cmd kexec.Cmd, cmdPath string, envVars []string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	if cmd == nil {
		cmd = cie.kexecDotInterface.Command(cmdPath, args...)
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
	klog.V(5).Infof("exec(%d): %s", counter, logCmd)

	err := cmd.Run()
	cmd.Output()
	klog.V(5).Infof("exec(%d): stdout: %q", counter, stdout)
	klog.V(5).Infof("exec(%d): stderr: %q", counter, stderr)
	if err != nil {
		klog.V(5).Infof("exec(%d): err: %v", counter, err)
	}
	return stdout, stderr, err
}

/*
func (cie *kexecUtil) RunCmd( cmdPath string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	if cie.kexecDotCmd == nil {
		cie.kexecDotCmd = cie.kexecDotInterface.Command(cmdPath, args...)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	cie.kexecDotCmd.SetStdout(stdout)
	cie.kexecDotCmd.SetStderr(stderr)

	counter := atomic.AddUint64(&runCounter, 1)
	logCmd := fmt.Sprintf("%s %s", cmdPath, strings.Join(args, " "))
	klog.V(5).Infof("exec(%d): %s", counter, logCmd)

	err := cie.kexecDotCmd.Run()
	cie.kexecDotCmd.Output()
	klog.V(5).Infof("exec(%d): stdout: %q", counter, stdout)
	klog.V(5).Infof("exec(%d): stderr: %q", counter, stderr)
	if err != nil {
		klog.V(5).Infof("exec(%d): err: %v", counter, err)
	}
	return stdout, stderr, err
}

func (cie *kexecUtil) Run(cmdPath string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	//cmd := cie.kexecDotInterface.Command(cmdPath, args...)
	//return runCmd(cmd, cmdPath, args...)
	cie.kexecDotCmd = cie.kexecDotInterface.Command(cmdPath, args...)
	return cie.RunCmd(cmdPath, args...)
}

func (cie *kexecUtil) RunWithEnvVars(cmdPath string, envVars []string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	//cmd := cie.kexecDotInterface.Command(cmdPath, args...)
	//cmd.SetEnv(envVars)
	//return runCmd(cmd, cmdPath, args...)
	cie.kexecDotCmd = cie.kexecDotInterface.Command(cmdPath, args...)
	cie.kexecDotCmd.SetEnv(envVars)
	return cie.RunCmd(cmdPath, args...)
}
*/

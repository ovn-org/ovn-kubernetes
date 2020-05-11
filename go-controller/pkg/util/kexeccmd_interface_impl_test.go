package util

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	kexec "k8s.io/utils/exec"
	"testing"
)

type onCallReturnArgs struct {
	onCallMethodName    string
	onCallMethodArgType string
	retArgList          []interface{}
}

func TestKexecUtil_SetExec(t *testing.T) {
	// kexecUtilsInstance is defined in kexeccmd_interface_impl.go
	kexecUtilsInstance = &kexecUtil{&mocks.Interface{}}
	tests := []struct {
		desc        string
		expectedErr error
		onRetArgs   *onCallReturnArgs
		callTimes   int
	}{
		{
			desc:        "positive, SetExecWithoutOVS succeeds",
			expectedErr: nil,
			onRetArgs:   &onCallReturnArgs{"LookPath", "[]string", []interface{}{"ip", nil}},
			callTimes:   8,
		},
		{
			desc:        "negative, SetExecWithoutOVS returns error",
			expectedErr: fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`),
			onRetArgs:   &onCallReturnArgs{"LookPath", "[]string", []interface{}{"", fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`)}},
			callTimes:   1,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := kexecUtilsInstance.kexecDotInterface.(*mocks.Interface).On(tc.onRetArgs.onCallMethodName, mock.Anything)
			for _, elem := range tc.onRetArgs.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, elem)
			}
			//call.Once()
			call.Times(tc.callTimes)
			e := kexecUtilsInstance.SetExec()
			fmt.Println(e)
			assert.Equal(t, e, tc.expectedErr)
		})
	}
}

func TestKexecUtil_SetExecWithoutOVS(t *testing.T) {
	// kexecUtilsInstance is defined in kexeccmd_interface_impl.go
	kexecUtilsInstance = &kexecUtil{&mocks.Interface{}}
	tests := []struct {
		desc        string
		expectedErr error
		onRetArgs   *onCallReturnArgs
	}{
		{
			desc:        "positive, ip path found",
			expectedErr: nil,
			onRetArgs:   &onCallReturnArgs{"LookPath", "[]string", []interface{}{"ip", nil}},
		},
		{
			desc:        "negative, ip path not found",
			expectedErr: fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`),
			onRetArgs:   &onCallReturnArgs{"LookPath", "[]string", []interface{}{"", fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`)}},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := kexecUtilsInstance.kexecDotInterface.(*mocks.Interface).On(tc.onRetArgs.onCallMethodName, mock.Anything)
			for _, elem := range tc.onRetArgs.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, elem)
			}
			call.Once()
			e := kexecUtilsInstance.SetExecWithoutOVS()
			fmt.Println(e)
			assert.Equal(t, e, tc.expectedErr)
		})

	}
}

func TestKexecUtil_SetSpecificExec(t *testing.T) {
	// kexecUtilsInstance is defined in kexeccmd_interface_impl.go
	kexecUtilsInstance = &kexecUtil{&mocks.Interface{}}
	tests := []struct {
		desc        string
		expectedErr error
		fnArg       string
		onRetArgs   *onCallReturnArgs
	}{
		{
			desc:        "positive: ovs-vsctl path found",
			expectedErr: nil,
			fnArg:       "ovs-vsctl",
			onRetArgs:   &onCallReturnArgs{"LookPath", "[]string", []interface{}{"ovs-vsctl", nil}},
		},
		{
			desc:        "negative: ovs-vsctl path not found",
			expectedErr: fmt.Errorf(`exec: \"ovs-vsctl:\" executable file not found in $PATH`),
			fnArg:       "ovs-vsctl",
			onRetArgs:   &onCallReturnArgs{"LookPath", "[]string", []interface{}{"", fmt.Errorf(`exec: \"ovs-vsctl:\" executable file not found in $PATH`)}},
		},
		{
			desc:        "negative: unknown command",
			expectedErr: fmt.Errorf(`unknown command: "ovs-appctl"`),
			fnArg:       "ovs-appctl",
			onRetArgs:   &onCallReturnArgs{"LookPath", "[]string", []interface{}{"", nil}},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := kexecUtilsInstance.kexecDotInterface.(*mocks.Interface).On(tc.onRetArgs.onCallMethodName, mock.Anything)
			for _, elem := range tc.onRetArgs.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, elem)
			}
			call.Once()
			e := kexecUtilsInstance.SetSpecificExec(tc.fnArg)
			assert.Equal(t, e, tc.expectedErr)
		})

	}
}

func TestKexecUtil_RunCmd(t *testing.T) {
	tests := []struct {
		desc        string
		expectedErr error
		cmd         kexec.Cmd
		cmdPath     string
		cmdArg      string
		envArgs     []string
	}{
		{
			desc:        "positive: run `ip addr` command without envVars",
			expectedErr: nil,
			cmd:         nil,
			cmdPath:     "ip",
			cmdArg:      "a",
			envArgs:     nil,
		},
		{
			desc:        "positive: run `ip addr` command with envVars",
			expectedErr: nil,
			cmd:         nil,
			cmdPath:     "ip",
			cmdArg:      "a",
			envArgs:     []string{"OVN_NB_DAEMON=/some/blah/path"},
		},
		{
			desc:        "negative: run `ip addr` command",
			expectedErr: fmt.Errorf("executable file not found in $PATH"),
			cmd:         nil,
			cmdPath:     "ips",
			cmdArg:      "addr",
			envArgs:     nil,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			_, _, e := GetKexecUtilsInstance().RunCmd(tc.cmd, tc.cmdPath, tc.envArgs, tc.cmdArg)
			assert.Equal(t, e, tc.expectedErr)
		})

	}
}

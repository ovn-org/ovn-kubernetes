package util

import (
	"bytes"
	"fmt"
	"testing"

	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	kexec "k8s.io/utils/exec"
)

type onCallReturnArgsRepetitive struct {
	onCallMethodName                    string
	onCallMethodsArgsStrTypeAppendCount int
	onCallMethodArgType                 []string
	retArgList                          []interface{}
}

type onCallReturnArgs struct {
	onCallMethodName    string
	onCallMethodArgType []string
	retArgList          []interface{}
}

func TestDefaultExecRunner_RunCmd(t *testing.T) {
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// tests in other files in the package would set runCmdExecRunner to mocks.ExecRunner,
	// for this test we want to ensure the non-mock instance is used
	runCmdExecRunner = &defaultExecRunner{}

	tests := []struct {
		desc             string
		expectedErr      error
		cmd              kexec.Cmd
		cmdPath          string
		cmdArg           string
		envVars          []string
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:        "negative: set cmd parameter to be nil",
			expectedErr: fmt.Errorf("cmd object cannot be nil"),
			cmd:         nil,
		},
		{
			desc:        "set envars and ensure cmd.SetEnv is invoked",
			expectedErr: nil,
			cmd:         mockCmd,
			envVars:     []string{"OVN_NB_DAEMON=/some/blah/path"},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:        "cmd.Run returns error test",
			expectedErr: fmt.Errorf("mock error"),
			cmd:         mockCmd,
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("mock error")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.cmd != nil {
				for _, item := range tc.onRetArgsCmdList {
					cmdCall := tc.cmd.(*mock_k8s_io_utils_exec.Cmd).On(item.onCallMethodName)
					for _, arg := range item.onCallMethodArgType {
						cmdCall.Arguments = append(cmdCall.Arguments, mock.AnythingOfType(arg))
					}

					for _, e := range item.retArgList {
						cmdCall.ReturnArguments = append(cmdCall.ReturnArguments, e)
					}
					cmdCall.Once()
				}
			}
			_, _, e := runCmdExecRunner.RunCmd(tc.cmd, tc.cmdPath, tc.envVars, tc.cmdArg)

			assert.Equal(t, e, tc.expectedErr)
			mockCmd.AssertExpectations(t)
		})
	}
}

func TestSetExec(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	tests := []struct {
		desc         string
		expectedErr  error
		onRetArgs    *onCallReturnArgs
		fnCallTimes  int
		setRunnerNil bool
	}{
		{
			desc:         "positive, test when 'runner' is nil",
			expectedErr:  nil,
			onRetArgs:    &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"ip", nil, "arping", nil}},
			fnCallTimes:  11,
			setRunnerNil: true,
		},
		{
			desc:         "positive, test when 'runner' is not nil",
			expectedErr:  nil,
			onRetArgs:    &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"", nil, "", nil}},
			fnCallTimes:  11,
			setRunnerNil: false,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockKexecIface.On(tc.onRetArgs.onCallMethodName)
			for _, arg := range tc.onRetArgs.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, elem := range tc.onRetArgs.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, elem)
			}
			call.Times(tc.fnCallTimes)
			if tc.setRunnerNil == false {
				// note runner is defined in ovs.go file
				runner = &execHelper{exec: mockKexecIface}
			}
			e := SetExec(mockKexecIface)
			assert.Equal(t, e, tc.expectedErr)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestSetExecWithoutOVS(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	tests := []struct {
		desc        string
		expectedErr error
		onRetArgs   *onCallReturnArgs
		fnCallTimes int
	}{
		{
			desc:        "positive, ip and arping path found",
			expectedErr: nil,
			fnCallTimes: 2,
			onRetArgs:   &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"ip", nil, "arping", nil}},
		},
		{
			desc:        "negative, ip path not found",
			expectedErr: fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`),
			fnCallTimes: 1,
			onRetArgs:   &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"", fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`), "arping", nil}},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockKexecIface.On(tc.onRetArgs.onCallMethodName)
			for _, arg := range tc.onRetArgs.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, elem := range tc.onRetArgs.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, elem)
			}
			call.Times(tc.fnCallTimes)
			e := SetExecWithoutOVS(mockKexecIface)
			assert.Equal(t, e, tc.expectedErr)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestSetSpecificExec(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	tests := []struct {
		desc        string
		expectedErr error
		fnArg       string
		onRetArgs   *onCallReturnArgs
		fnCallTimes int
	}{
		{
			desc:        "positive: ovs-vsctl path found",
			expectedErr: nil,
			fnArg:       "ovs-vsctl",
			onRetArgs:   &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"ovs-vsctl", nil}},
			fnCallTimes: 1,
		},
		{
			desc:        "negative: ovs-vsctl path not found",
			expectedErr: fmt.Errorf(`exec: \"ovs-vsctl:\" executable file not found in $PATH`),
			fnArg:       "ovs-vsctl",
			onRetArgs:   &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"", fmt.Errorf(`exec: \"ovs-vsctl:\" executable file not found in $PATH`)}},
			fnCallTimes: 1,
		},
		{
			desc:        "negative: unknown command",
			expectedErr: fmt.Errorf(`unknown command: "ovs-appctl"`),
			fnArg:       "ovs-appctl",
			onRetArgs:   &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"ovs-appctl", nil}},
			fnCallTimes: 0,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockKexecIface.On(tc.onRetArgs.onCallMethodName)
			for _, arg := range tc.onRetArgs.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, elem := range tc.onRetArgs.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, elem)
			}
			call.Times(tc.fnCallTimes)
			e := SetSpecificExec(mockKexecIface, tc.fnArg)
			assert.Equal(t, e, tc.expectedErr)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunCmd(t *testing.T) {
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc             string
		expectedErr      error
		cmdPath          string
		cmdArg           string
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:        "positive: run `ip addr` command",
			expectedErr: nil,
			cmdPath:     "ip",
			cmdArg:      "a",
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
			},
		},
		{
			desc:        "negative: run `ip addr` command",
			expectedErr: fmt.Errorf("executable file not found in $PATH"),
			cmdPath:     "ips",
			cmdArg:      "addr",
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsCmdList {
				cmdCall := mockCmd.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					cmdCall.Arguments = append(cmdCall.Arguments, mock.AnythingOfType(arg))
				}

				for _, e := range item.retArgList {
					cmdCall.ReturnArguments = append(cmdCall.ReturnArguments, e)
				}
				cmdCall.Once()
			}
			_, _, e := runCmd(mockCmd, tc.cmdPath, tc.cmdArg)

			assert.Equal(t, e, tc.expectedErr)
			mockCmd.AssertExpectations(t)
		})
	}
}

func TestRun(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      error
		cmdPath          string
		cmdArg           string
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "negative: run `ip addr` command",
			expectedErr:    fmt.Errorf("executable file not found in $PATH"),
			cmdPath:        "ips",
			cmdArg:         "addr",
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
			},
		},
		{
			desc:           "positive: run `ip addr`",
			expectedErr:    nil,
			cmdPath:        "ip",
			cmdArg:         "a",
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsCmdList {
				cmdCall := mockCmd.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					cmdCall.Arguments = append(cmdCall.Arguments, mock.AnythingOfType(arg))
				}

				for _, e := range item.retArgList {
					cmdCall.ReturnArguments = append(cmdCall.ReturnArguments, e)
				}
				cmdCall.Once()
			}

			ifaceCall := mockKexecIface.On(tc.onRetArgsIface.onCallMethodName, mock.Anything)
			for _, arg := range tc.onRetArgsIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, item := range tc.onRetArgsIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, item)
			}
			ifaceCall.Once()

			_, _, e := run(tc.cmdPath, tc.cmdArg)

			assert.Equal(t, e, tc.expectedErr)
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunWithEnvVars(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      error
		cmdPath          string
		cmdArg           string
		envArgs          []string
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ip addr` command with empty envVars",
			expectedErr:    nil,
			cmdPath:        "ip",
			cmdArg:         "a",
			envArgs:        []string{},
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
			},
		},
		{
			desc:           "positive: run `ip addr` command with envVars",
			expectedErr:    nil,
			cmdPath:        "ip",
			cmdArg:         "a",
			envArgs:        []string{"OVN_NB_DAEMON=/some/blah/path"},
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ip addr` command",
			expectedErr:    fmt.Errorf("executable file not found in $PATH"),
			cmdPath:        "ips",
			cmdArg:         "addr",
			envArgs:        nil,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsCmdList {
				cmdCall := mockCmd.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					cmdCall.Arguments = append(cmdCall.Arguments, mock.AnythingOfType(arg))
				}

				for _, e := range item.retArgList {
					cmdCall.ReturnArguments = append(cmdCall.ReturnArguments, e)
				}
				cmdCall.Once()
			}

			ifaceCall := mockKexecIface.On(tc.onRetArgsIface.onCallMethodName, mock.Anything)
			for _, arg := range tc.onRetArgsIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, item := range tc.onRetArgsIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, item)
			}
			ifaceCall.Once()

			_, _, e := runWithEnvVars(tc.cmdPath, tc.envArgs, tc.cmdArg)

			assert.Equal(t, e, tc.expectedErr)
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSOfctl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovs-ofctl` command",
			expectedErr:             fmt.Errorf("executable file not found in $PATH"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string"}, []interface{}{nil, nil, fmt.Errorf("executable file not found in $PATH")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovs-ofctl` ",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVSOfctl()

			assert.Equal(t, e, tc.expectedErr)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSDpctl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovs-dpctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovs-dpctl command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovs-dpctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovs-dpctl` ",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVSDpctl()

			assert.Equal(t, e, tc.expectedErr)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSVsctl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovs-vsctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovs-vsctl command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovs-vsctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovs-vsctl` ",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVSVsctl()

			assert.Equal(t, e, tc.expectedErr)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSAppctlWithTimeout(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		timeout                 int
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovs-appctl` command with timeout",
			expectedErr:             fmt.Errorf("failed to execute ovs-appctl command"),
			timeout:                 5,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovs-appctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovs-appctl` command with timeout",
			expectedErr:             nil,
			timeout:                 5,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()

			_, _, e := RunOVSAppctlWithTimeout(tc.timeout)

			assert.Equal(t, e, tc.expectedErr)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSAppctl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovs-appctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovs-appctl command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovs-appctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovs-appctl` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()

			_, _, e := RunOVSAppctl()

			assert.Equal(t, e, tc.expectedErr)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNAppctlWithTimeout(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		timeout                 int
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovn-appctl` command with timeout",
			expectedErr:             fmt.Errorf("failed to execute ovn-appctl command"),
			timeout:                 5,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-appctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-appctl` command with timeout",
			expectedErr:             nil,
			timeout:                 15,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVNAppctlWithTimeout(tc.timeout)

			assert.Equal(t, e, tc.expectedErr)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNNbctlUnix(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovn-nbctl` command with no env vars generated",
			expectedErr:             fmt.Errorf("failed to execute ovn-nbctl command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-nbctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-nbctl` command with no env vars generated",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVNNbctlUnix()

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNNbctlWithTimeout(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		timeout                 int
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovn-nbctl` command with timeout",
			expectedErr:             fmt.Errorf("failed to execute ovn-nbctl command"),
			timeout:                 5,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-nbctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-nbctl` command with timeout",
			expectedErr:             nil,
			timeout:                 15,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVNNbctlWithTimeout(tc.timeout)

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNNbctl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovn-nbctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovn-nbctl command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-nbctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-nbctl` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()
			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVNNbctl()

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNSbctlUnix(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovn-sbctl` command with no env vars generated",
			expectedErr:             fmt.Errorf("failed to execute ovn-sbctl command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-sbctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-sbctl` command with no env vars generated",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()
			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVNSbctlUnix()

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNSbctlWithTimeout(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		timeout                 int
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovn-sbctl` command with timeout",
			expectedErr:             fmt.Errorf("failed to execute ovn-sbctl command"),
			timeout:                 5,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-sbctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-sbctl` command with timeout",
			expectedErr:             nil,
			timeout:                 15,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVNSbctlWithTimeout(tc.timeout)

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNSbctl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovn-sbctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovn-sbctl command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-sbctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-sbctl` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVNSbctl()

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSDBClient(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovsdb-client` command",
			expectedErr:             fmt.Errorf("failed to execute ovsdb-client command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovsdb-client command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovsdb-client` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVSDBClient()

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSDBClientOVNNB(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovsdb-client` command against OVN NB database",
			expectedErr:             fmt.Errorf("failed to execute ovsdb-client command against OVN NB database"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovsdb-client command against OVN NB database")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovsdb-client` command against OVN NB database",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVSDBClientOVNNB("list-dbs")

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNCtl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovn-ctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovn-ctl command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-ctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-ctl` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVNCtl()

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNNBAppCtl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovn-appctl -t nbdbCtlSockPath` command",
			expectedErr:             fmt.Errorf("failed to execute ovn-appctl -t nbdbCtlSockPath command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-appctl -t nbdbCtlSockPath command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-appctl -t nbdbCtlSockPath` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVNNBAppCtl()

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNSBAppCtl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovn-appctl -t sbdbCtlSockPath` command",
			expectedErr:             fmt.Errorf("failed to execute ovn-appctl -t sbdbCtlSockPath command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-appctl -t sbdbCtlSockPath command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-appctl -t sbdbCtlSockPath` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunOVNSBAppCtl()

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunIP(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "positive: run IP ",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()
			_, _, e := RunIP()

			assert.Equal(t, e, tc.expectedErr)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestAddNormalActionOFFlow(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	mockExecRunner := new(mocks.ExecRunner)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
		onRetArgsCmdList        *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovs-ofctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovs-ofctl command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovs-ofctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:        &onCallReturnArgs{"SetStdin", []string{"*bytes.Buffer"}, nil},
		},
		{
			desc:                    "positive: run `ovs-ofctl` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), fmt.Errorf("executable file not found in $PATH")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:        &onCallReturnArgs{"SetStdin", []string{"*bytes.Buffer"}, nil},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()

			mockCall := mockCmd.On(tc.onRetArgsCmdList.onCallMethodName)
			for _, arg := range tc.onRetArgsCmdList.onCallMethodArgType {
				mockCall.Arguments = append(mockCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsCmdList.retArgList {
				mockCall.ReturnArguments = append(mockCall.ReturnArguments, ret)
			}
			mockCall.Once()

			_, _, e := AddNormalActionOFFlow("somename")

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestReplaceOFFlows(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	mockExecRunner := new(mocks.ExecRunner)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
		onRetArgsCmdList        *onCallReturnArgs
	}{
		{
			desc:                    "negative: run `ovs-ofctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovs-ofctl command"),
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, []interface{}{nil, nil, fmt.Errorf("failed to execute ovs-ofctl command")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:        &onCallReturnArgs{"SetStdin", []string{"*bytes.Buffer"}, nil},
		},
		{
			desc:                    "positive: run `ovs-ofctl` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:        &onCallReturnArgs{"SetStdin", []string{"*bytes.Buffer"}, nil},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()

			mockCall := mockCmd.On(tc.onRetArgsCmdList.onCallMethodName)
			for _, arg := range tc.onRetArgsCmdList.onCallMethodArgType {
				mockCall.Arguments = append(mockCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsCmdList.retArgList {
				mockCall.ReturnArguments = append(mockCall.ReturnArguments, ret)
			}
			mockCall.Once()

			_, _, e := ReplaceOFFlows("somename", []string{})

			if tc.expectedErr != nil {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestGetOVNDBServerInfo(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             bool
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: executable not found",
			expectedErr:             true,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, []interface{}{nil, nil, fmt.Errorf("executable file not found in $PATH")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "negative: malformed json output",
			expectedErr:             true,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte(`["rows":[{"connected":true,"index":["set",[]],"leader":true}]}]`)), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "negative: zero rows returned",
			expectedErr:             true,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte(`[{"rows":[]}]`)), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovsdb-client` command successfully",
			expectedErr:             false,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte(`[{"rows":[{"connected":true,"index":["set",[]],"leader":true}]}]`)), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()

			_, e := GetOVNDBServerInfo(15, "nb", "OVN_Northbound")

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestDetectSCTPSupport(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc                    string
		expectedErr             bool
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		{
			desc:                    "negative: fails to query OVN NB DB for SCTP support",
			expectedErr:             true,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{nil, nil, fmt.Errorf("fails to query OVN NB DB")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:        "negative: json unmarshal error",
			expectedErr: true,
			onRetArgsExecUtilsIface: &onCallReturnArgs{
				"RunCmd",
				[]string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string"},
				[]interface{}{ // below three rows are stdout, stderr and error respectively returned by runWithEnvVars method
					bytes.NewBuffer([]byte(`"data":"headings":["Column","Type"]}`)), //stdout value: mocks malformed json returned
					bytes.NewBuffer([]byte("")),
					nil,
				},
			},
			onRetArgsKexecIface: &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:        "positive: SCTP present in protocol list",
			expectedErr: false,
			onRetArgsExecUtilsIface: &onCallReturnArgs{
				"RunCmd",
				[]string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string"},
				[]interface{}{ // below three rows are stdout, stderr and error respectively returned by runWithEnvVars method
					// below is snippet of valid stdout returned and is truncated for unit testing and readability
					bytes.NewBuffer([]byte(`{"data":[["protocol",{"key":{"enum":["set",["sctp","tcp","udp"]],"type":"string"},"min":0}]],"headings":["Column","Type"]}`)),
					bytes.NewBuffer([]byte("")),
					nil,
				},
			},
			onRetArgsKexecIface: &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:        "negative: SCTP not present in protocol list",
			expectedErr: false,
			onRetArgsExecUtilsIface: &onCallReturnArgs{
				"RunCmd",
				[]string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string"},
				[]interface{}{ // below three rows are stdout, stderr and error respectively returned by runWithEnvVars method
					// below is snippet of valid stdout returned and is truncated for unit testing and readability
					bytes.NewBuffer([]byte(`{"data":[["protocol",{"key":{"enum":["set",["tcp","udp"]],"type":"string"},"min":0}]],"headings":["Column","Type"]}`)),
					bytes.NewBuffer([]byte("")),
					nil,
				},
			},
			onRetArgsKexecIface: &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()

			_, e := DetectSCTPSupport()

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

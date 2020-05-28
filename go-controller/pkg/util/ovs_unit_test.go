package util

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"testing"
)

type onCallReturnArgs struct {
	onCallMethodName    string
	onCallMethodArgType []string
	retArgList          []interface{}
}

func TestSetExec(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	tests := []struct {
		desc        string
		expectedErr error
		onRetArgs   *onCallReturnArgs
		fnCallTimes int
	}{
		{
			desc:        "positive, SetExecWithoutOVS succeeds",
			expectedErr: nil,
			onRetArgs:   &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"ip", nil}},
			fnCallTimes: 8,
		},
		{
			desc:        "negative, SetExecWithoutOVS returns error",
			expectedErr: fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`),
			onRetArgs:   &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"", fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`)}},
			fnCallTimes: 1,
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
			e := SetExec(mockKexecIface)
			assert.Equal(t, e, tc.expectedErr)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestSetExecWithoutOVS(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	tests := []struct {
		desc        string
		expectedErr error
		onRetArgs   *onCallReturnArgs
	}{
		{
			desc:        "positive, ip path found",
			expectedErr: nil,
			onRetArgs:   &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"ip", nil}},
		},
		{
			desc:        "negative, ip path not found",
			expectedErr: fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`),
			onRetArgs:   &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"", fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`)}},
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
			call.Once()
			e := SetExecWithoutOVS(mockKexecIface)
			assert.Equal(t, e, tc.expectedErr)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestSetSpecificExec(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
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
	mockCmd := new(mocks.Cmd)

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
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
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
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
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
				{"SetEnv", []string{"[]string"}, nil},
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
				{"SetEnv", []string{"[]string"}, nil},
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
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      error
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "negative: run `ovs-ofctl` command",
			expectedErr:    fmt.Errorf("executable file not found in $PATH"),
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
			},
		},
		{
			desc:           "positive: run `ovs-ofctl` ",
			expectedErr:    nil,
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

			_, _, e := RunOVSOfctl()

			assert.Equal(t, e, tc.expectedErr)
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSVsctl(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      error
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "negative: run `ovs-vsctl` command",
			expectedErr:    fmt.Errorf("executable file not found in $PATH"),
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
			},
		},
		{
			desc:           "positive: run `ovs-vsctl` ",
			expectedErr:    nil,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
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

			_, _, e := RunOVSVsctl()

			assert.Equal(t, e, tc.expectedErr)
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSAppctlWithTimeout(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      error
		cmdArg           int
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "negative: run `ovs-appctl` command with timeout",
			expectedErr:    fmt.Errorf("executable file not found in $PATH"),
			cmdArg:         5,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
			},
		},
		{
			desc:           "positive: run `ovs-appctl` command with timeout",
			expectedErr:    nil,
			cmdArg:         5,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
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

			_, _, e := RunOVSAppctlWithTimeout(tc.cmdArg)

			assert.Equal(t, e, tc.expectedErr)
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSAppctl(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      error
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "negative: run `ovs-appctl` command ",
			expectedErr:    fmt.Errorf("executable file not found in $PATH"),
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
			},
		},
		{
			desc:           "positive: run `ovs-appctl` command ",
			expectedErr:    nil,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
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

			_, _, e := RunOVSAppctl()

			assert.Equal(t, e, tc.expectedErr)
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNAppctlWithTimeout(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      error
		cmdArg           int
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "negative: run `ovn-appctl` command with timeout",
			expectedErr:    fmt.Errorf("executable file not found in $PATH"),
			cmdArg:         5,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
			},
		},
		{
			desc:           "positive: run `ovn-appctl` command with timeout",
			expectedErr:    nil,
			cmdArg:         5,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
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

			_, _, e := RunOVNAppctlWithTimeout(tc.cmdArg)

			assert.Equal(t, e, tc.expectedErr)
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNNbctlUnix(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovn-nbctl` command",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovn-nbctl` command",
			expectedErr:    true,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, _, e := RunOVNNbctlUnix()

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNNbctlWithTimeout(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		timeout          int
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovn-nbctl` command with timeout",
			expectedErr:    false,
			timeout:        5,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovn-nbctl` command with timeout",
			expectedErr:    true,
			timeout:        5,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, _, e := RunOVNNbctlWithTimeout(tc.timeout)

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNNbctl(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		timeout          int
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovn-nbctl` command",
			expectedErr:    false,
			timeout:        5,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovn-nbctl` command",
			expectedErr:    true,
			timeout:        5,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, _, e := RunOVNNbctl()

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNSbctlUnix(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovn-sbctl` command",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovn-sbctl` command",
			expectedErr:    true,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, _, e := RunOVNSbctlUnix()

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNSbctlWithTimeout(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		timeout          int
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovn-sbctl` command with timeout",
			expectedErr:    false,
			timeout:        5,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovn-sbctl` command with timeout",
			expectedErr:    true,
			timeout:        5,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, _, e := RunOVNSbctlWithTimeout(tc.timeout)

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNSbctl(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovn-sbctl` command",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovn-sbctl` command",
			expectedErr:    true,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, _, e := RunOVNSbctl()

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSDBClient(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovsdb-client` command",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovsdb-client` command",
			expectedErr:    true,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, _, e := RunOVSDBClient()

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSDBClientOVNNB(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovsdb-client` command against OVN NB database",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovsdb-client` command against OVN NB database",
			expectedErr:    true,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, _, e := RunOVSDBClientOVNNB("list-dbs")

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNCtl(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovn-ctl` command",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovn-ctl` command",
			expectedErr:    true,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, _, e := RunOVNCtl()

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNNBAppCtl(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovn-appctl -t nbdbCtlSockPath` command",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovn-appctl -t nbdbCtlSockPath` command",
			expectedErr:    true,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, _, e := RunOVNNBAppCtl()

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNSBAppCtl(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovn-appctl -t sbdbCtlSockPath` command",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovn-appctl -t sbdbCtlSockPath` command",
			expectedErr:    true,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, _, e := RunOVNSBAppCtl()

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

/* NOTE: maybe integration test candidate instead of unit test candidate?
func TestRunOVNNorthAppCtl(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &ExecHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovs-appctl -t pidfile/socket` command",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovn-appctl -t pidfile/socket` command",
			expectedErr:    true,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, _, e := RunOVNNorthAppCtl()

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}
*/

func TestAddNormalActionOFFlow(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
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
			desc:           "negative: run `ovs-ofctl` command",
			expectedErr:    fmt.Errorf("executable file not found in $PATH"),
			cmdPath:        "ips",
			cmdArg:         "addr",
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetStdin", []string{"*bytes.Buffer"}, nil},
			},
		},
		{
			desc:           "positive: run `ovs-ofctl` command",
			expectedErr:    nil,
			cmdPath:        "ip",
			cmdArg:         "a",
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetStdin", []string{"*bytes.Buffer"}, nil},
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

			_, _, e := AddNormalActionOFFlow("somename")

			assert.Equal(t, e, tc.expectedErr)
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestGetOVNDBServerInfo(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		{
			desc:           "positive: run `ovsdb-client` command",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},
		{
			desc:           "negative: run `ovsdb-client` command",
			expectedErr:    true,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, e := GetOVNDBServerInfo(10, "direction", "dbstring")

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestDetectSCTPSupport(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      bool
		onRetArgsIface   *onCallReturnArgs
		onRetArgsCmdList []onCallReturnArgs
	}{
		/* NOTE/TODO : Positive test case will remain commented out until we are able to group the run, runCmd, runCmdWithEnvVars into
		a separate interface and mocks are generated.
		{
			desc:           "positive: run `ovsdb-client` command against OVN NB database",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{nil}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
			},
		},*/
		{
			desc:           "negative: run `ovsdb-client` command against OVN NB database",
			expectedErr:    true,
			onRetArgsIface: &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", []string{}, []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", []string{"*bytes.Buffer"}, nil},
				{"SetStderr", []string{"*bytes.Buffer"}, nil},
				{"SetEnv", []string{"[]string"}, nil},
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

			_, e := DetectSCTPSupport()

			if tc.expectedErr {
				assert.Error(t, e)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

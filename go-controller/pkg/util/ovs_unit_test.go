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
			onRetArgs:   &onCallReturnArgs{"LookPath", "string", []interface{}{"ip", nil}},
			fnCallTimes: 8,
		},
		{
			desc:        "negative, SetExecWithoutOVS returns error",
			expectedErr: fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`),
			onRetArgs:   &onCallReturnArgs{"LookPath", "string", []interface{}{"", fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`)}},
			fnCallTimes: 1,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockKexecIface.On(tc.onRetArgs.onCallMethodName, mock.AnythingOfType(tc.onRetArgs.onCallMethodArgType))
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
			onRetArgs:   &onCallReturnArgs{"LookPath", "string", []interface{}{"ip", nil}},
		},
		{
			desc:        "negative, ip path not found",
			expectedErr: fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`),
			onRetArgs:   &onCallReturnArgs{"LookPath", "string", []interface{}{"", fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`)}},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockKexecIface.On(tc.onRetArgs.onCallMethodName, mock.Anything)
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
			onRetArgs:   &onCallReturnArgs{"LookPath", "string", []interface{}{"ovs-vsctl", nil}},
			fnCallTimes: 1,
		},
		{
			desc:        "negative: ovs-vsctl path not found",
			expectedErr: fmt.Errorf(`exec: \"ovs-vsctl:\" executable file not found in $PATH`),
			fnArg:       "ovs-vsctl",
			onRetArgs:   &onCallReturnArgs{"LookPath", "string", []interface{}{"", fmt.Errorf(`exec: \"ovs-vsctl:\" executable file not found in $PATH`)}},
			fnCallTimes: 1,
		},
		{
			desc:        "negative: unknown command",
			expectedErr: fmt.Errorf(`unknown command: "ovs-appctl"`),
			fnArg:       "ovs-appctl",
			onRetArgs:   &onCallReturnArgs{"LookPath", "[]string", []interface{}{"ovs-appctl", nil}},
			fnCallTimes: 0,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockKexecIface.On(tc.onRetArgs.onCallMethodName, mock.Anything)
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
	tests := []struct {
		desc        string
		expectedErr error
		cmdPath     string
		cmdArg      string
	}{
		{
			desc:        "positive: run `ip addr` command without envVars",
			expectedErr: nil,
			cmdPath:     "ip",
			cmdArg:      "a",
		},
		{
			desc:        "positive: run `ip addr` command with envVars",
			expectedErr: nil,
			cmdPath:     "ip",
			cmdArg:      "a",
		},
		{
			desc:        "negative: run `ip addr` command",
			expectedErr: fmt.Errorf("executable file not found in $PATH"),
			cmdPath:     "ips",
			cmdArg:      "addr",
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			cmd := kexec.New().Command(tc.cmdPath, tc.cmdArg)
			_, _, e := RunCmd(cmd, tc.cmdPath, tc.cmdArg)
			assert.Equal(t, e, tc.expectedErr)
		})
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		desc        string
		expectedErr error
		cmdPath     string
		cmdArg      string
	}{
		{
			desc:        "positive: run `ip addr` ",
			expectedErr: nil,
			cmdPath:     "ip",
			cmdArg:      "a",
		},
		{
			desc:        "negative: run `ip addr` command",
			expectedErr: fmt.Errorf("executable file not found in $PATH"),
			cmdPath:     "ips",
			cmdArg:      "addr",
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			// note runner is defined in ovs.go file
			runner = &ExecHelper{exec: kexec.New()}
			_, _, e := Run(tc.cmdPath, tc.cmdArg)
			assert.Equal(t, e, tc.expectedErr)
		})
	}
}

func TestRunWithEnvVars(t *testing.T) {
	tests := []struct {
		desc        string
		expectedErr error
		cmdPath     string
		cmdArg      string
		envArgs     []string
	}{
		{
			desc:        "positive: run `ip addr` command with empty envVars",
			expectedErr: nil,
			cmdPath:     "ip",
			cmdArg:      "a",
			envArgs:     []string{},
		},
		{
			desc:        "positive: run `ip addr` command with envVars",
			expectedErr: nil,
			cmdPath:     "ip",
			cmdArg:      "a",
			envArgs:     []string{"OVN_NB_DAEMON=/some/blah/path"},
		},
		{
			desc:        "negative: run `ip addr` command",
			expectedErr: fmt.Errorf("executable file not found in $PATH"),
			cmdPath:     "ips",
			cmdArg:      "addr",
			envArgs:     nil,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			// note runner is defined in ovs.go file
			runner = &ExecHelper{exec: kexec.New()}
			_, _, e := RunWithEnvVars(tc.cmdPath, tc.envArgs, tc.cmdArg)
			assert.Equal(t, e, tc.expectedErr)
		})
	}
}


func TestRunOVSOfctl(t *testing.T) {
	mockKexecIface := new(mocks.Interface)
	mockCmd := new(mocks.Cmd)
	// note runner is defined in ovs.go file
	runner = &ExecHelper{exec: mockKexecIface}

	tests := []struct {
		desc        		string
		expectedErr 		error
		cmdArg      		string
		onRetArgsIface  	*onCallReturnArgs
		onRetArgsCmdList	[]onCallReturnArgs
	}{
		{
			desc:        "negative: run `ovs-ofctl` command",
			expectedErr: fmt.Errorf("executable file not found in $PATH"),
			cmdArg:      "",
			onRetArgsIface:	&onCallReturnArgs{"Command", "string", []interface{}{mockCmd}},
			onRetArgsCmdList: []onCallReturnArgs{
				{"Run", "", []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{"SetStdout", "*bytes.Buffer", nil},
				{"SetStderr", "*bytes.Buffer", nil},
			},
		},
		{
			desc:        			"positive: run `ovs-ofctl` ",
			expectedErr: 			nil,
			cmdArg:		 			"",
			onRetArgsIface:			&onCallReturnArgs{ "Command", "string",[]interface{}{mockCmd}},
			onRetArgsCmdList: 		[]onCallReturnArgs{
				{"Run", "", []interface{}{ nil}},
				{"SetStdout", "*bytes.Buffer", nil},
				{ "SetStderr", "*bytes.Buffer", nil},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsCmdList {
				cmdCall := mockCmd.On(item.onCallMethodName, mock.Anything)
				for _, e := range item.retArgList {
					cmdCall.ReturnArguments = append(cmdCall.ReturnArguments, e)
				}
				cmdCall.Once()
			}

			ifaceCall := mockKexecIface.On(tc.onRetArgsIface.onCallMethodName, mock.Anything)
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

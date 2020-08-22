package cni

import (
	"fmt"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	kexec "k8s.io/utils/exec"
)

func TestSetExec(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	tests := []struct {
		desc        string
		expectedErr error
		onRetArgs   *ovntest.TestifyMockHelper
	}{
		{
			desc:        "positive, ovs-vsctl found",
			expectedErr: nil,
			onRetArgs:   &ovntest.TestifyMockHelper{"LookPath", []string{"string"}, []interface{}{"", nil}},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockKexecIface.On(tc.onRetArgs.OnCallMethodName)
			for _, arg := range tc.onRetArgs.OnCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, elem := range tc.onRetArgs.RetArgList {
				call.ReturnArguments = append(call.ReturnArguments, elem)
			}
			call.Once()

			e := setExec(mockKexecIface)
			assert.Equal(t, e, tc.expectedErr)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestOvsExec(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         error
		onRetArgsKexecIface *ovntest.TestifyMockHelper
		onRetArgsCmdList    *ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
	}{
		{
			desc:           "Test codepath when runner is nil and returns error",
			expectedErr:    fmt.Errorf("failed to run ovs-vsctl"),
			runnerInstance: nil,
		},
		{
			desc:                "Test codepath when runner is non nil and returns error",
			expectedErr:         fmt.Errorf("failed to run ovs-vsctl"),
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{"CombinedOutput", []string{}, []interface{}{nil, fmt.Errorf("failed to run 'ovs-vsctl")}},
			runnerInstance:      mockKexecIface,
		},
		{
			desc:                "Test codepath when runner is not nil and does not return an error",
			expectedErr:         nil,
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{"Command", []string{"string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{"CombinedOutput", []string{}, []interface{}{nil, nil}},
			runnerInstance:      mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.OnCallMethodName)
				for _, arg := range tc.onRetArgsKexecIface.OnCallMethodArgType {
					ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsKexecIface.RetArgList {
					ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
				}
				ifaceCall.Once()
			}
			if tc.onRetArgsCmdList != nil {
				mockCall := mockCmd.On(tc.onRetArgsCmdList.OnCallMethodName)
				for _, arg := range tc.onRetArgsCmdList.OnCallMethodArgType {
					mockCall.Arguments = append(mockCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsCmdList.RetArgList {
					mockCall.ReturnArguments = append(mockCall.ReturnArguments, ret)
				}
				mockCall.Once()
			}

			runner = tc.runnerInstance

			_, e := ovsExec()

			if tc.expectedErr != nil {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestOvsCreate(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         error
		onRetArgsKexecIface *ovntest.TestifyMockHelper
		onRetArgsCmdList    *ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
	}{
		{
			desc:                "Positive test codepath for ovsCreate",
			expectedErr:         nil,
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{"Command", []string{"string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{"CombinedOutput", []string{}, []interface{}{nil, nil}},
			runnerInstance:      mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.OnCallMethodName)
				for _, arg := range tc.onRetArgsKexecIface.OnCallMethodArgType {
					ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsKexecIface.RetArgList {
					ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
				}
				ifaceCall.Once()
			}
			if tc.onRetArgsCmdList != nil {
				mockCall := mockCmd.On(tc.onRetArgsCmdList.OnCallMethodName)
				for _, arg := range tc.onRetArgsCmdList.OnCallMethodArgType {
					mockCall.Arguments = append(mockCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsCmdList.RetArgList {
					mockCall.ReturnArguments = append(mockCall.ReturnArguments, ret)
				}
				mockCall.Once()
			}

			runner = tc.runnerInstance

			_, e := ovsCreate("blah")

			if tc.expectedErr != nil {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestOvsDestroy(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         error
		onRetArgsKexecIface *ovntest.TestifyMockHelper
		onRetArgsCmdList    *ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
	}{
		{
			desc:                "Positive test codepath for ovsDestroy",
			expectedErr:         nil,
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{"Command", []string{"string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{"CombinedOutput", []string{}, []interface{}{nil, nil}},
			runnerInstance:      mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.OnCallMethodName)
				for _, arg := range tc.onRetArgsKexecIface.OnCallMethodArgType {
					ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsKexecIface.RetArgList {
					ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
				}
				ifaceCall.Once()
			}
			if tc.onRetArgsCmdList != nil {
				mockCall := mockCmd.On(tc.onRetArgsCmdList.OnCallMethodName)
				for _, arg := range tc.onRetArgsCmdList.OnCallMethodArgType {
					mockCall.Arguments = append(mockCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsCmdList.RetArgList {
					mockCall.ReturnArguments = append(mockCall.ReturnArguments, ret)
				}
				mockCall.Once()
			}

			runner = tc.runnerInstance

			e := ovsDestroy("table", "record")

			if tc.expectedErr != nil {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestOvsSet(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         error
		onRetArgsKexecIface *ovntest.TestifyMockHelper
		onRetArgsCmdList    *ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
	}{
		{
			desc:                "Positive test codepath for ovsSet",
			expectedErr:         nil,
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{"Command", []string{"string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{"CombinedOutput", []string{}, []interface{}{nil, nil}},
			runnerInstance:      mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.OnCallMethodName)
				for _, arg := range tc.onRetArgsKexecIface.OnCallMethodArgType {
					ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsKexecIface.RetArgList {
					ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
				}
				ifaceCall.Once()
			}
			if tc.onRetArgsCmdList != nil {
				mockCall := mockCmd.On(tc.onRetArgsCmdList.OnCallMethodName)
				for _, arg := range tc.onRetArgsCmdList.OnCallMethodArgType {
					mockCall.Arguments = append(mockCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsCmdList.RetArgList {
					mockCall.ReturnArguments = append(mockCall.ReturnArguments, ret)
				}
				mockCall.Once()
			}

			runner = tc.runnerInstance

			e := ovsSet("table", "record")

			if tc.expectedErr != nil {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestOvsFind(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         error
		onRetArgsKexecIface *ovntest.TestifyMockHelper
		onRetArgsCmdList    *ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
	}{
		{
			desc:                "Test codepath when ovsExec returns an error",
			expectedErr:         fmt.Errorf("failed to run ovsFind"),
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{"CombinedOutput", []string{}, []interface{}{nil, fmt.Errorf("failed to run ovsFind")}},
			runnerInstance:      mockKexecIface,
		},
		{
			desc:                "Test codepath when ovsExec output is nil",
			expectedErr:         nil,
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{"CombinedOutput", []string{}, []interface{}{nil, nil}},
			runnerInstance:      mockKexecIface,
		},
		{
			desc:                "Positive test codepath for ovsFind; ovsExec output is not nil",
			expectedErr:         nil,
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{"CombinedOutput", []string{}, []interface{}{[]byte{}, nil}},
			runnerInstance:      mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.OnCallMethodName)
				for _, arg := range tc.onRetArgsKexecIface.OnCallMethodArgType {
					ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsKexecIface.RetArgList {
					ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
				}
				ifaceCall.Once()
			}
			if tc.onRetArgsCmdList != nil {
				mockCall := mockCmd.On(tc.onRetArgsCmdList.OnCallMethodName)
				for _, arg := range tc.onRetArgsCmdList.OnCallMethodArgType {
					mockCall.Arguments = append(mockCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsCmdList.RetArgList {
					mockCall.ReturnArguments = append(mockCall.ReturnArguments, ret)
				}
				mockCall.Once()
			}

			runner = tc.runnerInstance

			_, e := ovsFind("table", "record", "condition")

			if tc.expectedErr != nil {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestOvsClear(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         error
		onRetArgsKexecIface *ovntest.TestifyMockHelper
		onRetArgsCmdList    *ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
	}{
		{
			desc:                "Positive test codepath for ovsClear",
			expectedErr:         nil,
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{"Command", []string{"string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{"CombinedOutput", []string{}, []interface{}{nil, nil}},
			runnerInstance:      mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.OnCallMethodName)
				for _, arg := range tc.onRetArgsKexecIface.OnCallMethodArgType {
					ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsKexecIface.RetArgList {
					ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
				}
				ifaceCall.Once()
			}
			if tc.onRetArgsCmdList != nil {
				mockCall := mockCmd.On(tc.onRetArgsCmdList.OnCallMethodName)
				for _, arg := range tc.onRetArgsCmdList.OnCallMethodArgType {
					mockCall.Arguments = append(mockCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsCmdList.RetArgList {
					mockCall.ReturnArguments = append(mockCall.ReturnArguments, ret)
				}
				mockCall.Once()
			}

			runner = tc.runnerInstance

			e := ovsClear("table", "record", "columns")

			if tc.expectedErr != nil {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestOfctlExec(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         error
		onRetArgsKexecIface *ovntest.TestifyMockHelper
		onRetArgsCmdList    []ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
	}{
		{
			desc:                "Positive test codepath for ofctlExec",
			expectedErr:         nil,
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{"Command", []string{"string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{"SetStdout", []string{"*bytes.Buffer"}, []interface{}{nil}},
				{"SetStderr", []string{"*bytes.Buffer"}, []interface{}{nil}},
				{"Run", []string{}, []interface{}{nil}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:                "Negative test codepath for ofctlExec",
			expectedErr:         fmt.Errorf("failed to run ovs-ofctl"),
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{"Command", []string{"string", "string", "string", "string"}, []interface{}{mockCmd}},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{"SetStdout", []string{"*bytes.Buffer"}, []interface{}{nil}},
				{"SetStderr", []string{"*bytes.Buffer"}, []interface{}{nil}},
				{"Run", []string{}, []interface{}{fmt.Errorf("failed to run 'ovs-ofctl'")}},
			},
			runnerInstance: mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.OnCallMethodName)
				for _, arg := range tc.onRetArgsKexecIface.OnCallMethodArgType {
					ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsKexecIface.RetArgList {
					ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
				}
				ifaceCall.Once()
			}
			for _, item := range tc.onRetArgsCmdList {
				if tc.onRetArgsCmdList != nil {
					mockCall := mockCmd.On(item.OnCallMethodName)
					for _, arg := range item.OnCallMethodArgType {
						mockCall.Arguments = append(mockCall.Arguments, mock.AnythingOfType(arg))
					}
					for _, ret := range item.RetArgList {
						mockCall.ReturnArguments = append(mockCall.ReturnArguments, ret)
					}
					mockCall.Once()
				}
			}
			runner = tc.runnerInstance

			_, e := ofctlExec()

			if tc.expectedErr != nil {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockKexecIface.AssertExpectations(t)
			mockCmd.AssertExpectations(t)
		})
	}
}

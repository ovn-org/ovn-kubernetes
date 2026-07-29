// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kexec "k8s.io/utils/exec"

	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	mock_k8s_io_utils_exec "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
)

func TestSetExec(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	tests := []struct {
		desc        string
		expectedErr error
		onRetArgs   []*ovntest.TestifyMockHelper
	}{
		{
			desc:        "positive, ovs-vsctl found",
			expectedErr: nil,
			onRetArgs: []*ovntest.TestifyMockHelper{
				// mock ovs-vsctl path lookup
				{OnCallMethodName: "LookPath", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"", nil}},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			for _, mockHelper := range tc.onRetArgs {
				ovntest.ProcessMockFn(&mockKexecIface.Mock, *mockHelper)
			}

			e := SetExec(mockKexecIface)
			assert.Equal(t, tc.expectedErr, e)
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
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("failed to run 'ovs-vsctl")}},
			runnerInstance:      mockKexecIface,
		},
		{
			desc:                "Test codepath when runner is not nil and does not return an error",
			expectedErr:         nil,
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			runnerInstance:      mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			}
			if tc.onRetArgsCmdList != nil {
				ovntest.ProcessMockFn(&mockCmd.Mock, *tc.onRetArgsCmdList)
			}

			runner = tc.runnerInstance

			_, e := ovsExec()

			if tc.expectedErr != nil {
				require.Error(t, e)
			} else {
				require.NoError(t, e)
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
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			runnerInstance:      mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			ovntest.ProcessMockFn(&mockCmd.Mock, *tc.onRetArgsCmdList)

			runner = tc.runnerInstance

			_, e := ovsCreate("blah")

			if tc.expectedErr != nil {
				require.Error(t, e)
			} else {
				require.NoError(t, e)
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
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			runnerInstance:      mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			ovntest.ProcessMockFn(&mockCmd.Mock, *tc.onRetArgsCmdList)

			runner = tc.runnerInstance

			e := ovsDestroy("table", "record")

			if tc.expectedErr != nil {
				require.Error(t, e)
			} else {
				require.NoError(t, e)
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
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			runnerInstance:      mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			ovntest.ProcessMockFn(&mockCmd.Mock, *tc.onRetArgsCmdList)

			runner = tc.runnerInstance

			e := ovsSet("table", "record")

			if tc.expectedErr != nil {
				require.Error(t, e)
			} else {
				require.NoError(t, e)
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
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("failed to run ovsFind")}},
			runnerInstance:      mockKexecIface,
		},
		{
			desc:                "Test codepath when ovsExec output is nil",
			expectedErr:         nil,
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			runnerInstance:      mockKexecIface,
		},
		{
			desc:                "Positive test codepath for ovsFind; ovsExec output is not nil",
			expectedErr:         nil,
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{}, nil}},
			runnerInstance:      mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			ovntest.ProcessMockFn(&mockCmd.Mock, *tc.onRetArgsCmdList)

			runner = tc.runnerInstance

			_, e := ovsFind("table", "record", "condition")

			if tc.expectedErr != nil {
				require.Error(t, e)
			} else {
				require.NoError(t, e)
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
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:    &ovntest.TestifyMockHelper{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			runnerInstance:      mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			ovntest.ProcessMockFn(&mockCmd.Mock, *tc.onRetArgsCmdList)

			runner = tc.runnerInstance

			e := ovsClear("table", "record", "columns")

			if tc.expectedErr != nil {
				require.Error(t, e)
			} else {
				require.NoError(t, e)
			}

			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

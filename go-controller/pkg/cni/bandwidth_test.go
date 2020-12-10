package cni

import (
	"fmt"
	"github.com/stretchr/testify/mock"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/stretchr/testify/assert"
	kexec "k8s.io/utils/exec"
)

func TestClearPodBandwidth(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         bool
		onRetArgsKexecIface []ovntest.TestifyMockHelper
		onRetArgsCmdList    []ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
	}{
		{
			desc:        "Test error code path when ovsFind attempts to retrieve interfaces",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsFind")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:        "Test code path when ovsClear returns an error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsClear")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:        "Test error code path when ovsFind attempts to retrieve qos instances",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsFind")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:        "Test code path when ovsDestroy returns an error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsDestroy")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc: "Positive test code path",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance: mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				ovntest.ProcessMockFnList(&mockKexecIface.Mock, tc.onRetArgsKexecIface)
			}
			if tc.onRetArgsCmdList != nil {
				ovntest.ProcessMockFnList(&mockCmd.Mock, tc.onRetArgsCmdList)
			}
			// note runner is defined in pkg/cni/ovs.go file
			runner = tc.runnerInstance

			e := clearPodBandwidth("sandboxID")

			if tc.expectedErr {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestSetPodBandwidth(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         bool
		onRetArgsKexecIface []ovntest.TestifyMockHelper
		onRetArgsCmdList    []ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
		egressBPS           int64
	}{
		{
			desc:        "Test code path when both ingressBPS is greater than zero and ovsCreate returns an error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}}},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsCreate")}}},
			runnerInstance: mockKexecIface,
			egressBPS:      0,
		},
		{
			desc:        "Test code path when inressBPS is greater than zero and ovsSet returns an error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
			egressBPS:      0,
		},
		{
			desc: "Positive test code path when ingressBPS is greater than zero",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance: mockKexecIface,
			egressBPS:      0,
		},
		{
			desc:        "Negative test code path when setting ingress_policing_rate",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
			egressBPS:      3,
		},
		{
			desc:        "Negative test code path when setting ingress_policing_burst",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
			egressBPS:      3,
		},
		{
			desc: "Positive test code path when both ingressBPS and egressBPS are greater than zero",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance: mockKexecIface,
			egressBPS:      3,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				ovntest.ProcessMockFnList(&mockKexecIface.Mock, tc.onRetArgsKexecIface)
			}

			if tc.onRetArgsCmdList != nil {
				ovntest.ProcessMockFnList(&mockCmd.Mock, tc.onRetArgsCmdList)
			}
			// note runner is defined in pkg/cni/ovs.go file
			runner = tc.runnerInstance

			e := setPodBandwidth("sandboxID", "ifname", 1, tc.egressBPS)

			if tc.expectedErr {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestGetPodBandwidth(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc                string
		expectedErr         bool
		onRetArgsKexecIface []ovntest.TestifyMockHelper
		onRetArgsCmdList    []ovntest.TestifyMockHelper
		runnerInstance      kexec.Interface
		egressBPS           int64
		igressBPS           int64
	}{
		{
			desc: "Positive test code path when ingressBPS and egressBPS are correctly set",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("\"10000000\""), nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("10000"), nil}},
			},
			runnerInstance: mockKexecIface,
			igressBPS:      10000000,
			egressBPS:      10000000,
		},
		{
			desc: "Positive test code path when ingressBPS and egressBPS are not set",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("0"), nil}},
			},
			runnerInstance: mockKexecIface,
			igressBPS:      -1,
			egressBPS:      0,
		},
		{
			desc: "Positive test code path when ingressBPS is not set (no port) and egressBPS is set",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("10000"), nil}},
			},
			runnerInstance: mockKexecIface,
			igressBPS:      -1,
			egressBPS:      10000000,
		},
		{
			desc: "Positive test code path when ingressBPS is not set (no max-rate) and egressBPS is set",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("10000"), nil}},
			},
			runnerInstance: mockKexecIface,
			igressBPS:      -1,
			egressBPS:      10000000,
		},
		{
			desc: "Positive test code path when ingressBPS is set but egressBPS isn't",
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("\"10000000\""), nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance: mockKexecIface,
			igressBPS:      10000000,
			egressBPS:      -1,
		},
		{
			desc:        "Negative test code path when ovsGet 'port' returns error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:        "Negative test code path when ovsGet 'qos' returns error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:        "Negative test code path when max-rate value cannot be transfer to integer",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("test"), nil}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:        "Negative test code path when ovsGet 'interface' returns error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("\"10000000\""), nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:        "Negative test code path when ovsGet 'interface' returns error",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("\"10000000\""), nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("mock: failed to run ovsSet")}},
			},
			runnerInstance: mockKexecIface,
		},
		{ // cannot happen
			desc:        "Negative test code path when ingress_policing_rate cannot be transfer to integer ",
			expectedErr: true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte{1}, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("\"10000000\""), nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{[]byte("test"), nil}},
			},
			runnerInstance: mockKexecIface,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsKexecIface != nil {
				for _, item := range tc.onRetArgsKexecIface {
					ifaceCall := mockKexecIface.On(item.OnCallMethodName)
					for _, arg := range item.OnCallMethodArgType {
						ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
					}
					for _, ret := range item.RetArgList {
						ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
					}
					ifaceCall.Once()
				}
			}

			if tc.onRetArgsCmdList != nil {
				for _, item := range tc.onRetArgsCmdList {
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
			// note runner is defined in pkg/cni/ovs.go file
			runner = tc.runnerInstance
			igress, egress, e := getPodBandwidth("ifname")

			if tc.expectedErr {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
				assert.Equal(t, igress, tc.igressBPS)
				assert.Equal(t, egress, tc.egressBPS)
			}
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

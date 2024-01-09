//go:build linux
// +build linux

package util

import (
	"fmt"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
)

func TestGetDeviceIDFromNetdevice(t *testing.T) {
	mockFsOps := mocks.NewFileSystemOps(t)
	SetFileSystemOps(mockFsOps)

	mockErr := fmt.Errorf("mock error")
	tests := []struct {
		desc        string
		netdev      string
		expErr      error
		expVal      string
		fsOpsHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:   "Correct netdevice",
			netdev: "eth0",
			expErr: nil,
			expVal: "0000:12:34.0",
			fsOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Readlink", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"../../0000:12:34.0", nil}},
			},
		},
		{
			desc:   "",
			netdev: "eth0",
			expErr: mockErr,
			expVal: "",
			fsOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Readlink", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"", mockErr}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockFsOps.Mock, tc.fsOpsHelper)

			ret, err := GetDeviceIDFromNetdevice(tc.netdev)
			if tc.expVal != ret {
				t.Errorf("Expected - '%v', got - '%v' for '%s'", tc.expVal, ret, tc.netdev)
			}
			if tc.expErr == nil && err != nil {
				t.Errorf("Expected not to fail for '%s', got error: %v", tc.netdev, err)
			} else if tc.expErr != nil && err == nil {
				t.Errorf("Expected to fail for '%s' with: %v", tc.netdev, tc.expErr)
			} else if tc.expErr != err {
				t.Errorf("Expected - '%v', got - '%v' for '%s'", tc.expErr, err, tc.netdev)
			}

			mockFsOps.AssertExpectations(t)
		})
	}
}

func TestGetExternalPortRange(t *testing.T) {
	mockFsOps := mocks.NewFileSystemOps(t)
	SetFileSystemOps(mockFsOps)

	mockErr := fmt.Errorf("mock error")
	tests := []struct {
		desc        string
		expErr      error
		expVal      string
		fsOpsHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:   "Correct output",
			expErr: nil,
			expVal: "32000-65535",
			fsOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ReadFile", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"32000\t65535", nil}},
			},
		},
		{
			desc:   "File read error",
			expErr: fmt.Errorf("failed to read file: %s", mockErr.Error()),
			expVal: "",
			fsOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ReadFile", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"", mockErr}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockFsOps.Mock, tc.fsOpsHelper)

			ret, err := GetExternalPortRange()
			if tc.expVal != ret {
				t.Errorf("Expected - '%v', got - '%v'", tc.expVal, ret)
			}
			if tc.expErr == nil && err != nil {
				t.Errorf("Expected not to fail, got error: %v", err)
			} else if tc.expErr != nil && err == nil {
				t.Errorf("Expected to fail with: %v", tc.expErr)
			} else if tc.expErr == nil && err == nil {
				// all good
			} else if tc.expErr.Error() != err.Error() {
				t.Errorf("Expected - '%v', got - '%v'", tc.expErr, err)
			}

			mockFsOps.AssertExpectations(t)
		})
	}
}

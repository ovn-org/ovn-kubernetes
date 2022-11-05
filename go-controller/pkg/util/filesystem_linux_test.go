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

//go:build linux
// +build linux

package util

import (
	"fmt"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
)

func TestIsPCIDeviceName(t *testing.T) {
	tests := []struct {
		desc    string
		address string
		expect  bool
	}{
		// PCI address = <domain>:<bus>:<device>.<function>
		{
			desc:    "valid PCI address",
			address: "1234:00:00.1",
			expect:  true,
		},
		{
			desc:    "invalid PCI domain",
			address: "BEEF:00:00.0",
			expect:  false,
		},
		{
			desc:    "invalid PCI bus",
			address: "0000:jj:00.0",
			expect:  false,
		},
		{
			desc:    "invalid PCI device",
			address: "0000:00:23.0",
			expect:  false,
		},
		{
			desc:    "invalid PCI function",
			address: "BEEF:00:00.f",
			expect:  false,
		},
		{
			desc:    "not a PCI address",
			address: "not an address",
			expect:  false,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ret := IsPCIDeviceName(tc.address)
			if tc.expect != ret {
				t.Errorf("Expected - '%v', got - '%v' for '%s'", tc.expect, ret, tc.address)
			}
		})
	}
}

func TestIsAuxDeviceName(t *testing.T) {
	tests := []struct {
		desc   string
		aux    string
		expect bool
	}{
		// auxiliary device = <driver>.<device_type>.<id>
		{
			desc:   "valid auxiliary device name",
			aux:    "foo.bar.15",
			expect: true,
		},
		{
			desc:   "invalid driver name",
			aux:    "not-a.driver.1",
			expect: false,
		},
		{
			desc:   "invalid device type",
			aux:    "wrong.device-type.1",
			expect: false,
		},
		{
			desc:   "invalid device id",
			aux:    "wrong.device.id",
			expect: false,
		},
		{
			desc:   "not an auxiliary device name",
			aux:    "not an auxiliary device name",
			expect: false,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ret := IsAuxDeviceName(tc.aux)
			if tc.expect != ret {
				t.Errorf("Expected - '%v', got - '%v' for '%s'", tc.expect, ret, tc.aux)
			}
		})
	}
}

func TestGetFunctionRepresentorName(t *testing.T) {
	mockSriovnetOps := mocks.NewSriovnetOps(t)
	SetSriovnetOpsInst(mockSriovnetOps)

	mockUplErr := fmt.Errorf("mock failed to get uplink representor")
	mockIdxErr := fmt.Errorf("mock failed to get index")
	mockRepErr := fmt.Errorf("mock failed to get representor")
	tests := []struct {
		desc           string
		deviceID       string
		expRep         string
		expUpl         string
		expErr         error
		sriovOpsHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:     "PCI: success",
			deviceID: "0000:00:00.1",
			expRep:   "eno1",
			expUpl:   "ens0",
			expErr:   nil,
			sriovOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"ens0", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{1, nil}},
				{OnCallMethodName: "GetVfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"eno1", nil}},
			},
		},
		{
			desc:     "PCI: GetUplinkRepresentor failure",
			deviceID: "0000:00:00.2",
			expRep:   "",
			expUpl:   "",
			expErr:   mockUplErr,
			sriovOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"", mockUplErr}},
			},
		},
		{
			desc:     "PCI: GetVfIndexByPciAddress failure",
			deviceID: "0000:00:00.3",
			expRep:   "",
			expUpl:   "",
			expErr:   mockIdxErr,
			sriovOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"ens0", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{-1, mockIdxErr}},
			},
		},
		{
			desc:     "PCI: GetVfRepresentor failure",
			deviceID: "0000:00:00.4",
			expRep:   "",
			expUpl:   "",
			expErr:   mockRepErr,
			sriovOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"ens0", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{1, nil}},
				{OnCallMethodName: "GetVfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"", mockRepErr}},
			},
		},
		{
			desc:     "Auxiliary: success",
			deviceID: "foo.bar.5",
			expRep:   "eno1",
			expUpl:   "ens0",
			expErr:   nil,
			sriovOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentorFromAux", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"ens0", nil}},
				{OnCallMethodName: "GetSfIndexByAuxDev", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{1, nil}},
				{OnCallMethodName: "GetSfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"eno1", nil}},
			},
		},
		{
			desc:     "Auxiliary: GetUplinkRepresentorFromAux failure",
			deviceID: "foo.bar.6",
			expRep:   "",
			expUpl:   "",
			expErr:   mockUplErr,
			sriovOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentorFromAux", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"", mockUplErr}},
			},
		},
		{
			desc:     "Auxiliary: GetSfIndexByAuxDev failure",
			deviceID: "foo.bar.7",
			expRep:   "",
			expUpl:   "",
			expErr:   mockIdxErr,
			sriovOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentorFromAux", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"ens0", nil}},
				{OnCallMethodName: "GetSfIndexByAuxDev", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{-1, mockIdxErr}},
			},
		},
		{
			desc:     "Auxiliary: GetSfRepresentor failure",
			deviceID: "foo.bar.8",
			expRep:   "",
			expUpl:   "",
			expErr:   mockRepErr,
			sriovOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentorFromAux", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"ens0", nil}},
				{OnCallMethodName: "GetSfIndexByAuxDev", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{1, nil}},
				{OnCallMethodName: "GetSfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"", mockRepErr}},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockSriovnetOps.Mock, tc.sriovOpsHelper)

			rep, upl, err := GetFunctionRepresentorName(tc.deviceID)
			if tc.expRep != rep {
				t.Errorf("Expected representor - '%v', got - '%v' for '%s'", tc.expRep, rep, tc.deviceID)
			}
			if tc.expUpl != upl {
				t.Errorf("Expected uplink - '%v', got - '%v' for '%s'", tc.expUpl, upl, tc.deviceID)
			}
			if tc.expErr == nil && err != nil {
				t.Errorf("Expected not to fail for '%s', got error: %v", tc.deviceID, err)
			} else if tc.expErr != nil && err == nil {
				t.Errorf("Expected to fail for '%s' with: %v", tc.deviceID, tc.expErr)
			} else if tc.expErr != err {
				t.Errorf("Expected - '%v', got - '%v' for '%s'", tc.expErr, err, tc.deviceID)
			}

			mockSriovnetOps.AssertExpectations(t)
		})
	}
}

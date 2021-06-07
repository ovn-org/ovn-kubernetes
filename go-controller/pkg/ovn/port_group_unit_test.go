package ovn

import (
	"errors"
	"fmt"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	goovn_mock "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/ebay/go-ovn"

	goovn "github.com/ebay/go-ovn"
	"github.com/stretchr/testify/assert"
)

const (
	pgName     = "TestPortGroupName"
	pgHashName = "TestPortGroupHashName"
	pgUUUID    = "0d3b392e-15a0-46be-9b37-f9cb0dd4f9ec"
	lspUUID    = "e72eb0bb-a1ce-45d5-b8a0-3200aed7e5e9"
	lspName    = "TestLSPName"
)

var (
	execError  = errors.New("transaction Failed due to an error")
	otherError = errors.New("other error")
)

func TestCreatePortGroup(t *testing.T) {
	mockGoOvnNBClient := new(goovn_mock.Client)

	tests := []struct {
		desc                      string
		name                      string
		hashName                  string
		errMatch                  error
		onRetArgMockGoOvnNBClient []ovntest.TestifyMockHelper
	}{
		{
			desc:     "positive test case",
			name:     pgName,
			hashName: pgHashName,
			errMatch: nil,
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupAdd", OnCallMethodArgType: []string{"string", "[]string", "map[string]string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
				},
				{
					OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{nil},
				},
				{
					OnCallMethodName: "PortGroupGet", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&goovn.PortGroup{UUID: pgUUUID, Name: pgName}, nil},
				},
			},
		},
		{
			desc:     "port group already exists",
			name:     pgName,
			hashName: pgHashName,
			errMatch: nil,
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupAdd", OnCallMethodArgType: []string{"string", "[]string", "map[string]string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, goovn.ErrorExist},
				},
				{
					OnCallMethodName: "PortGroupGet", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&goovn.PortGroup{UUID: pgUUUID, Name: pgName}, nil},
				},
			},
		},
		{
			desc:     "other PortGroupAdd Error",
			name:     pgName,
			hashName: pgHashName,
			errMatch: fmt.Errorf("add error for port group: %s, %v", pgName, otherError),
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupAdd", OnCallMethodArgType: []string{"string", "[]string", "map[string]string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, otherError},
				},
			},
		},
		{
			desc:     "execute error",
			name:     pgName,
			hashName: pgHashName,
			errMatch: fmt.Errorf("execute error for add port group: %s, %v", pgName, execError),
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupAdd", OnCallMethodArgType: []string{"string", "[]string", "map[string]string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
				},
				{
					OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{execError},
				},
			},
		},
		{
			desc:     "PortGroupGet error",
			name:     pgName,
			hashName: pgHashName,
			errMatch: fmt.Errorf("failed to get port group UUID: %s, %v", pgName, otherError),
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupAdd", OnCallMethodArgType: []string{"string", "[]string", "map[string]string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
				},
				{
					OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{nil},
				},
				{
					OnCallMethodName: "PortGroupGet", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&goovn.PortGroup{UUID: pgUUUID, Name: pgName}, otherError},
				},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockGoOvnNBClient.Mock, tc.onRetArgMockGoOvnNBClient)

			uuid, err := createPortGroup(mockGoOvnNBClient, tc.name, tc.hashName)

			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				assert.Nil(t, err)
				assert.Equal(t, pgUUUID, uuid)
			}
			mockGoOvnNBClient.AssertExpectations(t)
		})
	}
}

func TestDeletePortGroup(t *testing.T) {
	mockGoOvnNBClient := new(goovn_mock.Client)

	tests := []struct {
		desc                      string
		hashName                  string
		errMatch                  error
		onRetArgMockGoOvnNBClient []ovntest.TestifyMockHelper
	}{
		{
			desc:     "positive test case",
			hashName: pgHashName,
			errMatch: nil,
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupDel", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
				},
				{
					OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{nil},
				},
			},
		},
		{
			desc:     "port group not found",
			hashName: pgHashName,
			errMatch: nil,
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupDel", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, goovn.ErrorNotFound},
				},
			},
		},
		{
			desc:     "other PortGroupDel Error",
			hashName: pgHashName,
			errMatch: fmt.Errorf("delete error for port group: %s, %v", pgHashName, otherError),
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupDel", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, otherError},
				},
			},
		},
		{
			desc:     "execute error",
			hashName: pgHashName,
			errMatch: fmt.Errorf("execute error for delete port group: %s, %v", pgHashName, execError),
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupDel", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
				},
				{
					OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{execError},
				},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockGoOvnNBClient.Mock, tc.onRetArgMockGoOvnNBClient)

			err := deletePortGroup(mockGoOvnNBClient, tc.hashName)

			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				assert.Nil(t, err)
			}
			mockGoOvnNBClient.AssertExpectations(t)
		})
	}
}

func TestAddToPortGroup(t *testing.T) {
	mockGoOvnNBClient := new(goovn_mock.Client)

	tests := []struct {
		desc                      string
		name                      string
		portInfo                  *lpInfo
		errMatch                  error
		onRetArgMockGoOvnNBClient []ovntest.TestifyMockHelper
	}{
		{
			desc:     "positive test case",
			name:     pgName,
			portInfo: &lpInfo{name: lspName, uuid: lspUUID},
			errMatch: nil,
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupAddPort", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
				},
				{
					OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{nil},
				},
			},
		},
		{
			desc:     "positive idempotency",
			name:     pgName,
			portInfo: &lpInfo{name: lspName, uuid: lspUUID},
			errMatch: nil,
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupAddPort", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
				},
				{
					OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{nil},
				},
			},
		},
		{
			desc:     "port group not found",
			name:     pgName,
			portInfo: &lpInfo{name: lspName, uuid: lspUUID},
			errMatch: fmt.Errorf("error preparing adding port %s to port group %s (%v)", lspName, pgName, goovn.ErrorNotFound),
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupAddPort", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, goovn.ErrorNotFound},
				},
			},
		},
		{
			desc:     "other PortGroupAddPort error",
			name:     pgName,
			portInfo: &lpInfo{name: lspName, uuid: lspUUID},
			errMatch: fmt.Errorf("error preparing adding port %s to port group %s (%v)", lspName, pgName, otherError),
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupAddPort", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, otherError},
				},
			},
		},
		{
			desc:     "execute error",
			name:     pgName,
			portInfo: &lpInfo{name: lspName, uuid: lspUUID},
			errMatch: fmt.Errorf("error committing adding ports (%s) to port group %s (%v)", lspName, pgName, execError),
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupAddPort", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
				},
				{
					OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{execError},
				},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockGoOvnNBClient.Mock, tc.onRetArgMockGoOvnNBClient)

			err := addToPortGroup(mockGoOvnNBClient, tc.name, tc.portInfo)

			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				assert.Nil(t, err)
			}
			mockGoOvnNBClient.AssertExpectations(t)
		})
	}
}

func TestDeleteFromPortGroup(t *testing.T) {
	mockGoOvnNBClient := new(goovn_mock.Client)

	tests := []struct {
		desc                      string
		name                      string
		portInfo                  *lpInfo
		errMatch                  error
		onRetArgMockGoOvnNBClient []ovntest.TestifyMockHelper
	}{
		{
			desc:     "positive test case",
			name:     pgName,
			portInfo: &lpInfo{name: lspName, uuid: lspUUID},
			errMatch: nil,
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupRemovePort", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
				},
				{
					OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{nil},
				},
			},
		},
		{
			desc:     "port group or port not found",
			name:     pgName,
			portInfo: &lpInfo{name: lspName, uuid: lspUUID},
			errMatch: nil,
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupRemovePort", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, goovn.ErrorNotFound},
				},
			},
		},
		{
			desc:     "other PortGroupAddPort error",
			name:     pgName,
			portInfo: &lpInfo{name: lspName, uuid: lspUUID},
			errMatch: fmt.Errorf("error preparing removing port %s from port group %s (%v)", lspName, pgName, otherError),
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupRemovePort", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, otherError},
				},
			},
		},
		{
			desc:     "execute error",
			name:     pgName,
			portInfo: &lpInfo{name: lspName, uuid: lspUUID},
			errMatch: fmt.Errorf("error committing removing ports (%s) from port group %s (%v)", lspName, pgName, execError),
			onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "PortGroupRemovePort", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
				},
				{
					OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{execError},
				},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockGoOvnNBClient.Mock, tc.onRetArgMockGoOvnNBClient)

			err := deleteFromPortGroup(mockGoOvnNBClient, tc.name, tc.portInfo)

			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				assert.Nil(t, err)
			}
			mockGoOvnNBClient.AssertExpectations(t)
		})
	}
}

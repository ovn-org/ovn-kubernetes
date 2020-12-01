package util

import (
	"fmt"
	goovn "github.com/ebay/go-ovn"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	go_ovn_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/ebay/go-ovn"
	"github.com/stretchr/testify/assert"
)

func TestOvnNBLSPDel(t *testing.T) {
	mockNbClient := new(go_ovn_mocks.Client)
	inpLogicalPort := "blahPort"

	tests := []struct {
		desc            string
		errMatch        error
		errExp          bool
		goovnMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:     "test path when 'nbClient.Execute(cmd)' returns error",
			errMatch: fmt.Errorf("error while deleting logical port"),
			goovnMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LSPDel", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil}},
				{OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		{
			desc:   "test path when 'err != goovn.ErrorNotFound'",
			errExp: true,
			goovnMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LSPDel", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:   "test path when 'err == goovn.ErrorNotFound'",
			errExp: false,
			goovnMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LSPDel", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, goovn.ErrorNotFound}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNbClient.Mock, tc.goovnMockHelper)

			err := OvnNBLSPDel(mockNbClient, inpLogicalPort)
			t.Log(err)
			if tc.errExp {
				assert.Error(t, err)
			} else if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				assert.Nil(t, err)
			}

			mockNbClient.AssertExpectations(t)
		})
	}
}

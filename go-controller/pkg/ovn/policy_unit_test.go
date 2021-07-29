package ovn

import (
	"fmt"
	"net"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	. "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/stretchr/testify/assert"
)

func TestAddAllowACLFromNode(t *testing.T) {
	tests := []struct {
		desc      string
		inpSwName string
		inpMgmtIp net.IP
		errExp    bool
		errMatch  error
		//onRetArgMockGoOvnNBClient []ovntest.TestifyMockHelper
	}{
		{
			desc:      "test error when ovnNBClient.ACLAdd() fails",
			inpSwName: "testSW",
			inpMgmtIp: ovntest.MustParseIP("192.168.10.10"),
			errMatch:  fmt.Errorf("ACLAdd() error when creating node acl for logical switch"),
			/*
				onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
					{
						OnCallMethodName: "ACLAdd", OnCallMethodArgType: []string{"string", "string", "string", "string", "int", "map[string]string", "bool", "string", "string"}, RetArgList: []interface{}{nil, goovn.ErrorSchema},
					},
				},
			*/
		},
		{
			desc:      "test when ACL already exists and confirm no error returned",
			inpSwName: "testSW",
			inpMgmtIp: ovntest.MustParseIP("192.168.10.10"),
			/*
				onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
					{
						OnCallMethodName: "ACLAdd", OnCallMethodArgType: []string{"string", "string", "string", "string", "int", "map[string]string", "bool", "string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, goovn.ErrorExist},
					},
				},
			*/
		},
		{
			desc:      "test when ovnNBClient.Execute() fails",
			inpSwName: "testSW",
			inpMgmtIp: ovntest.MustParseIP("192.168.10.10"),
			errMatch:  fmt.Errorf("failed to create the node acl for logical_switch"),
			/*
				onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
					{
						OnCallMethodName: "ACLAdd", OnCallMethodArgType: []string{"string", "string", "string", "string", "int", "map[string]string", "bool", "string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
					},
					{
						OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{goovn.ErrorOption},
					},
				},
			*/
		},
		{
			desc:      "positive: test ip4 managment ip",
			inpSwName: "testSW",
			inpMgmtIp: ovntest.MustParseIP("192.168.10.10"),
			/*
				onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
					{
						OnCallMethodName: "ACLAdd", OnCallMethodArgType: []string{"string", "string", "string", "string", "int", "map[string]string", "bool", "string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
					},
					{
						OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{nil},
					},
				},
			*/
		},
		{
			desc:      "positive: test ip6 management ip",
			inpSwName: "testSW",
			inpMgmtIp: ovntest.MustParseIP("fd01::1234"),
			/*
				onRetArgMockGoOvnNBClient: []ovntest.TestifyMockHelper{
					{
						OnCallMethodName: "ACLAdd", OnCallMethodArgType: []string{"string", "string", "string", "string", "int", "map[string]string", "bool", "string", "string"}, RetArgList: []interface{}{&goovn.OvnCommand{}, nil},
					},
					{
						OnCallMethodName: "Execute", OnCallMethodArgType: []string{"*goovn.OvnCommand"}, RetArgList: []interface{}{nil},
					},
				},
			*/
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			stopChan := make(chan struct{})
			defer close(stopChan)
			nbClient, err := NewNBTestHarness(TestSetup{}, stopChan)
			if err != nil {
				t.Fatalf("Error creating northbound ovsdb test harness: %v", err)
			}

			err = addAllowACLFromNode(nbClient, tc.inpSwName, tc.inpMgmtIp)
			// TODO execute ops and assert state
			if tc.errExp {
				assert.Error(t, err)
			} else if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				assert.Nil(t, err)
			}
		})
	}
}

package util

import (
	"bytes"
	"fmt"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	netlink_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink"

	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

	"github.com/stretchr/testify/assert"
)

func TestGetNicName(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                string
		errMatch            error
		outputExp           string
		inpBrName           string
		onRetArgsExecIface  []ovntest.TestifyMockHelper
		onRetArgsKexecIface []ovntest.TestifyMockHelper
	}{
		{
			desc:      "empty string as inputBrName",
			errMatch:  fmt.Errorf("failed to get list of ports on bridge"),
			outputExp: "",
			inpBrName: "",
			onRetArgsExecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("test error")}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
		},
		{
			desc:      "test code path when `ovs-vsctl get Port port Interfaces` returns error",
			errMatch:  fmt.Errorf("failed to get port"),
			outputExp: "",
			inpBrName: "testPortName",
			onRetArgsExecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("port1\nport2")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
		},
		{
			desc:      "test code path when `ovs-vsctl get Interface ifaceId Type` returns error",
			errMatch:  fmt.Errorf("failed to get Interface"),
			outputExp: "",
			inpBrName: "testPortName",
			onRetArgsExecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("port1")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
		},
		{
			desc:      "test code path when `ovs-vsctl get Interface ifaceId Type` returns `system`",
			outputExp: "port1",
			inpBrName: "testPortName",
			onRetArgsExecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("port1")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("system")), bytes.NewBuffer([]byte("")), nil}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
		},
		{
			desc:      "test code path when `ovs-vsctl br-get-external-id` returns error",
			errMatch:  fmt.Errorf("failed to get the bridge-uplink for the bridge"),
			outputExp: "",
			inpBrName: "testPortName",
			onRetArgsExecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("port1")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("internal")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
		},
		{
			desc:      "test code path when input bridge name has prefix `br` and stdout is empty",
			outputExp: "Name",
			inpBrName: "brName",
			onRetArgsExecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("port1")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("internal")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
		},
		{
			desc:      "test code path when input bridge name has NO prefix `br`",
			outputExp: "someoutput",
			inpBrName: "testName",
			onRetArgsExecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("port1")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("internal")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("someoutput")), bytes.NewBuffer([]byte("")), nil}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockExecRunner.Mock, tc.onRetArgsExecIface)
			ovntest.ProcessMockFnList(&mockKexecIface.Mock, tc.onRetArgsKexecIface)

			res, err := GetNicName(tc.inpBrName)

			t.Log(res, err)

			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				assert.Equal(t, res, tc.outputExp)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestSaveIPAddress(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps
	tests := []struct {
		desc                     string
		inpOldLink               netlink.Link
		inpNewLink               netlink.Link
		inpAddrs                 []netlink.Addr
		errExp                   bool
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:       "empty address list, LinkSetup(newLink) succeeds",
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpAddrs:   []netlink.Addr{},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:       "deleting address from old link errors out",
			errExp:     true,
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpAddrs:   []netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:       "adding address to new link errors out",
			errExp:     true,
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpAddrs:   []netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:       "saving IP address to new link succeeds",
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpAddrs:   []netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			err := saveIPAddress(tc.inpNewLink, tc.inpOldLink, tc.inpAddrs, false)
			t.Log(err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestDelAddRoute(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		errExp                   bool
		inpOldLink               netlink.Link
		inpNewLink               netlink.Link
		inpRoute                 netlink.Route
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:       "test path where RouteDel() fails",
			errExp:     true,
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpRoute:   netlink.Route{Dst: ovntest.MustParseIPNet("192.168.1.0/24")},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:       "test path where RouteAdd() fails",
			errExp:     true,
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpRoute:   netlink.Route{Dst: ovntest.MustParseIPNet("192.168.1.0/24")},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Index: 1}}},
			},
		},
		{
			desc:       "test success path",
			inpOldLink: mockLink,
			inpNewLink: mockLink,
			inpRoute:   netlink.Route{Dst: ovntest.MustParseIPNet("192.168.1.0/24")},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Index: 1}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			err := delAddRoute(tc.inpOldLink, tc.inpNewLink, tc.inpRoute)
			t.Log(err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestSaveRoute(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		errExp                   bool
		inpOldLink               netlink.Link
		inpNewLink               netlink.Link
		inpRoutes                []netlink.Route
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc: "providing empty routes should return no error",
		},
		{
			desc:       "test error path for GCE special case",
			errExp:     true,
			inpNewLink: mockLink,
			inpOldLink: mockLink,
			inpRoutes: []netlink.Route{
				{Dst: ovntest.MustParseIPNet("192.168.1.0/24"), Gw: ovntest.MustParseIP("10.10.10.1")},
				{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:       "test error path for when adding default gateway",
			errExp:     true,
			inpNewLink: mockLink,
			inpOldLink: mockLink,
			inpRoutes: []netlink.Route{
				{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:       "test success path",
			inpNewLink: mockLink,
			inpOldLink: mockLink,
			inpRoutes: []netlink.Route{
				{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			err := saveRoute(tc.inpOldLink, tc.inpNewLink, tc.inpRoutes)
			t.Log(err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestNicToBridge(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps
	tests := []struct {
		desc                     string
		inpIface                 string
		outBridge                string
		errExp                   bool
		onRetArgsExecUtilsIface  *ovntest.TestifyMockHelper
		onRetArgsKexecIface      *ovntest.TestifyMockHelper
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:     "invalid interface name fails to return a link",
			inpIface: "",
			errExp:   true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:                    "RunOVSVsctl fails to create OVS bridge",
			inpIface:                "eth0",
			errExp:                  true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 31, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("RunOVSVsctl error")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 32, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:                    "Retrieving link by bridge name fails",
			errExp:                  true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 31, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 32, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:                    "IP address retrieval for link fails",
			inpIface:                "eth0",
			errExp:                  true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 31, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 32, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:                    "Route retrieval for link fails",
			inpIface:                "eth0",
			errExp:                  true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 31, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 32, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:                    "Saving IP address to bridge fails",
			errExp:                  true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 31, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 32, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{}, nil}},
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{}, nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:                    "Saving routes to bridge fails",
			errExp:                  true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 31, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 32, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{}, nil}},
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:                    "IP address and Routes of interface to OVS bridge succeeds",
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 31, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 32, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{}, nil}},
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsExecUtilsIface != nil {
				ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			}
			if tc.onRetArgsKexecIface != nil {
				ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			}
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			res, err := NicToBridge(tc.inpIface)
			t.Log(res, err)
			if tc.errExp {
				assert.Error(t, err)
				assert.Equal(t, len(res), 0)
			} else {
				assert.Nil(t, err)
				assert.Greater(t, len(res), 0)
			}
			mockKexecIface.AssertExpectations(t)
			mockExecRunner.AssertExpectations(t)
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestBridgeToNic(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps
	tests := []struct {
		desc                     string
		inpBridge                string
		errExp                   bool
		onRetArgsExecUtilsIface  []ovntest.TestifyMockHelper
		onRetArgsKexecIface      []ovntest.TestifyMockHelper
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:      "invalid bridge name fails to return a link",
			inpBridge: "brinvalid",
			errExp:    true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:      "IP address retrieval for link fails",
			inpBridge: "breth0",
			errExp:    true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:      "Route retrieval for link fails",
			inpBridge: "breth0",
			errExp:    true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				// Below row entry is for mocking the `routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:      "Nic Name retrieval using bridge name fails",
			inpBridge: "breth0",
			errExp:    true,
			// Below two entries are for mocking the `nicName, err := GetNicName(bridge)` code path
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				//Below entry is for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("eth0")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				//Below entry is for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				// Below row entry is for mocking the `routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, nil}},
			},
		},
		{
			desc:      "retrieving interface link using nic name fails",
			inpBridge: "breth0",
			errExp:    true,
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("eth0")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("system")), bytes.NewBuffer([]byte("")), nil}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				// Below row entry is for mocking the `routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, nil}},
				// Below row entry is for mocking the `ifaceLink, err := netlink.LinkByName(nicName)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:      "saving IP address to iface fails",
			inpBridge: "breth0",
			errExp:    true,
			// Below two entries are for mocking the `nicName, err := GetNicName(bridge)` code path
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("eth0")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("system")), bytes.NewBuffer([]byte("")), nil}}},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				// Below row entry is for mocking the `routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, nil}},
				// Below row entry is for mocking the `ifaceLink, err := netlink.LinkByName(nicName)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below three rows are to invoke the save ip adddress function
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:      "saving routes to iface fails",
			inpBridge: "breth0",
			errExp:    true,
			// Below two entries are for mocking the `nicName, err := GetNicName(bridge)` code path
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("eth0")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("system")), bytes.NewBuffer([]byte("")), nil}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				// Below row entry is for mocking the `routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
				// Below row entry is for mocking the `ifaceLink, err := netlink.LinkByName(nicName)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below three row entries are for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path.
				//The delAddRoute function is indirectly invoked and needs mocking of Attrs() function.
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:      "list-ifaces (via RunOVSVsctl) for given bridge errors out",
			inpBridge: "breth0",
			errExp:    true,
			// Below two entries are for mocking the `nicName, err := GetNicName(bridge)` code path
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("eth0")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("system")), bytes.NewBuffer([]byte("")), nil}},
				//Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 3, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				// Below row entry is for mocking the `routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
				// Below row entry is for mocking the `ifaceLink, err := netlink.LinkByName(nicName)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below three row entries are for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				// Below entry is for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path.
				//The delAddRoute function is indirectly invoked and needs mocking of Attrs() function.
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:      "determining iface type fails",
			inpBridge: "breth0",
			errExp:    true,
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("eth0")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("system")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 3, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("9d2bc35689fa975")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "type")` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below rows are for mocking the three RunOVSVsctl executed inside the for loop that processes the interface list returned
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				// Below row entry is for mocking the `routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
				// Below row entry is for mocking the `ifaceLink, err := netlink.LinkByName(nicName)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below three row entries are for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path.
				//The delAddRoute function is indirectly invoked and needs mocking of Attrs() function.
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:      "test code path where the interface type IS NOT patch",
			inpBridge: "breth0",
			errExp:    true,
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("eth0")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("system")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 3, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("9d2bc35689fa975")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "type")` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("internal")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below rows are for mocking the three RunOVSVsctl executed inside the for loop that processes the interface list returned
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				// Below row entry is for mocking the `routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
				// Below row entry is for mocking the `ifaceLink, err := netlink.LinkByName(nicName)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below three row entries are for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path.
				//The delAddRoute function is indirectly invoked and needs mocking of Attrs() function.
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:      "fails to get peer port for patch interface",
			inpBridge: "breth0",
			errExp:    true,
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("eth0")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("system")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 3, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("ovn-3e22f4-0")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "type")` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("patch")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "options:peer")` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below rows are for mocking the three RunOVSVsctl executed inside the for loop that processes the interface list returned
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "options:peer")` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				// Below row entry is for mocking the `routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
				// Below row entry is for mocking the `ifaceLink, err := netlink.LinkByName(nicName)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below three row entries are for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path.
				//The delAddRoute function is indirectly invoked and needs mocking of Attrs() function.
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:      "failed to delete peer patch port on br-int",
			inpBridge: "breth0",
			errExp:    true,
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("eth0")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("system")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 3, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("ovn-3e22f4-0")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "type")` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("patch")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "options:peer")` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `_, stderr, err = RunOVSVsctl("--if-exists", "del-port", "br-int", peer)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below rows are for mocking the three RunOVSVsctl executed inside the for loop that processes the interface list returned
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "options:peer")` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `_, stderr, err = RunOVSVsctl("--if-exists", "del-port", "br-int", peer)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				// Below row entry is for mocking the `routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
				// Below row entry is for mocking the `ifaceLink, err := netlink.LinkByName(nicName)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below three row entries are for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path.
				//The delAddRoute function is indirectly invoked and needs mocking of Attrs() function.
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:      "deleting the bridge fails",
			inpBridge: "breth0",
			errExp:    true,
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("eth0")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("system")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 3, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("ovn-3e22f4-0")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "type")` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("patch")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "options:peer")` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `_, stderr, err = RunOVSVsctl("--if-exists", "del-port", "br-int", peer)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("mock error")}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below rows are for mocking the three RunOVSVsctl executed inside the for loop that processes the interface list returned
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "options:peer")` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `_, stderr, err = RunOVSVsctl("--if-exists", "del-port", "br-int", peer)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				// Below row entry is for mocking the `routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
				// Below row entry is for mocking the `ifaceLink, err := netlink.LinkByName(nicName)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below three row entries are for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path.
				//The delAddRoute function is indirectly invoked and needs mocking of Attrs() function.
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:      "delete bridge successfully",
			inpBridge: "breth0",
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("eth0")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[080841d8-4e12-4003-b0ac-36fefd63bae1]")), bytes.NewBuffer([]byte("")), nil}},
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("system")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 3, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("ovn-3e22f4-0")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "type")` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("patch")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "options:peer")` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `_, stderr, err = RunOVSVsctl("--if-exists", "del-port", "br-int", peer)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)` code path
				{OnCallMethodName: "RunCmd", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				//Below three entries are for mocking the `nicName, err := GetNicName(bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 5, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err := RunOVSVsctl("list-ifaces", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 4, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below rows are for mocking the three RunOVSVsctl executed inside the for loop that processes the interface list returned
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("get", "interface", iface, "options:peer")` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `_, stderr, err = RunOVSVsctl("--if-exists", "del-port", "br-int", peer)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
				// Below row is for mocking the `stdout, stderr, err = RunOVSVsctl("--", "--if-exists", "del-br", bridge)` code path
				{OnCallMethodName: "Command", OnCallMethodsArgsStrTypeAppendCount: 6, OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `bridgeLink, err := netlink.LinkByName(bridge)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below row entry is for mocking the `addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{{IPNet: ovntest.MustParseIPNet("192.168.1.15/24")}}, nil}},
				// Below row entry is for mocking the `routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)` code path
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{{Gw: ovntest.MustParseIP("10.10.10.1"), LinkIndex: 1}}, nil}},
				// Below row entry is for mocking the `ifaceLink, err := netlink.LinkByName(nicName)` code path
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				// Below three row entries are for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path
				{OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				// Below row entry is for mocking the `if err = saveIPAddress(bridgeLink, ifaceLink, addrs); err != nil` code path
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				// Below two row entries are for mocking the `if err = saveRoute(bridgeLink, ifaceLink, routes); err != nil` code path.
				//The delAddRoute function is indirectly invoked and needs mocking of Attrs() function.
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockExecRunner.Mock, tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFnList(&mockKexecIface.Mock, tc.onRetArgsKexecIface)
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			err := BridgeToNic(tc.inpBridge)
			t.Log(err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockKexecIface.AssertExpectations(t)
			mockExecRunner.AssertExpectations(t)
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

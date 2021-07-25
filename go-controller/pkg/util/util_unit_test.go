package util

import (
	"bytes"
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"net"
	"reflect"
	"testing"

	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/assert"
)

func TestGetLegacyK8sMgmtIntfName(t *testing.T) {
	tests := []struct {
		desc        string
		inpNodeName string
		expRetStr   string
	}{
		{
			desc:        "node name less than 11 characters",
			inpNodeName: "lesseleven",
			expRetStr:   "k8s-lesseleven",
		},
		{
			desc:        "node name more than 11 characters",
			inpNodeName: "morethaneleven",
			expRetStr:   "k8s-morethanele",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ret := GetLegacyK8sMgmtIntfName(tc.inpNodeName)
			if tc.expRetStr != ret {
				t.Fail()
			}
		})
	}
}

func TestGetNodeChassisID(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc                    string
		errExpected             bool
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "ovs-vsctl command returns error",
			errExpected:             true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("test error")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "ovs-vsctl command returns empty chassisID along with error",
			errExpected:             true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("test error")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "ovs-vsctl command returns empty chassisID with NO error",
			errExpected:             true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "ovs-vsctl command returns valid chassisID",
			errExpected:             false,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("4e98c281-f12b-4601-ab5a-a3d759fcb493")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			ret, e := GetNodeChassisID()
			if tc.errExpected {
				assert.Error(t, e)
			} else {
				assert.Greater(t, len(ret), 0)
			}
			mockExecRunner.AssertExpectations(t)
			mockCmd.AssertExpectations(t)
		})
	}
}

func TestUpdateNodeSwitchExcludeIPs(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc                    string
		inpNodeName             string
		inpSubnetStr            string
		errExpected             bool
		setCfgHybridOvlyEnabled bool
		onRetArgsExecUtilsIface []ovntest.TestifyMockHelper
		onRetArgsKexecIface     []ovntest.TestifyMockHelper
	}{
		{
			desc:         "IPv4 CIDR, ovn-nbctl fails to list logical switch ports",
			errExpected:  true,
			inpNodeName:  "ovn-control-plane",
			inpSubnetStr: "192.168.1.0/24",
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("RunOVNNbctl error")}},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
		},
		{
			desc:         "IPv6 CIDR, never excludes",
			errExpected:  false,
			inpNodeName:  "ovn-control-plane",
			inpSubnetStr: "fd04:3e42:4a4e:3381::/64",
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=true, sets haveMangementPort=true, ovn-nbctl command excludeIPs list empty",
			errExpected:             false,
			inpNodeName:             "ovn-control-plane",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: true,
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "RunCmd",
					OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string"},
					RetArgList: []interface{}{
						// below is output from command --> ovn-nbctl lsp-list ovn-control-plane
						bytes.NewBuffer([]byte("7dc3d98a-660a-477b-a6bc-d42904ed59e7 (k8s-ovn-control-plane)\nd23162b4-87b1-4ff8-b5a5-5cb731d822ed (kube-system_coredns-6955765f44-l9jxq)\n1e8cd861-c584-4e38-8c50-7a71a6ae26bb (local-path-storage_local-path-provisioner-85445b74d4-w5ghw)\n8f1b3173-aa43-4014-adcb-36eae52f7502 (stor-ovn-control-plane)")),
						bytes.NewBuffer([]byte("")),
						nil,
					},
				},
				{
					OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil},
				},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=true, sets haveHybridOverlayPort=false, ovn-nbctl command excludeIPs list populated",
			errExpected:             false,
			inpNodeName:             "ovn-control-plane",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: true,
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "RunCmd",
					OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string"},
					RetArgList: []interface{}{
						// below is output from command --> ovn-nbctl lsp-list ovn-control-plane
						bytes.NewBuffer([]byte("7dc3d98a-660a-477b-a6bc-d42904ed59e7 (int-ovn-control-plane)\nd23162b4-87b1-4ff8-b5a5-5cb731d822ed (kube-system_coredns-6955765f44-l9jxq)\n1e8cd861-c584-4e38-8c50-7a71a6ae26bb (local-path-storage_local-path-provisioner-85445b74d4-w5ghw)\n8f1b3173-aa43-4014-adcb-36eae52f7502 (stor-ovn-control-plane)")),
						bytes.NewBuffer([]byte("")),
						nil,
					},
				},
				{
					OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil},
				},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
		},
		{
			desc:         "IPv4 CIDR, haveMangementPort=false, ovn-nbctl command with excludeIPs list populated, returns error ",
			errExpected:  false,
			inpNodeName:  "ovn-control-plane",
			inpSubnetStr: "192.168.1.0/24",
			onRetArgsExecUtilsIface: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "RunCmd",
					OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string"},
					RetArgList: []interface{}{
						// below is output from command --> ovn-nbctl lsp-list ovn-control-plane
						bytes.NewBuffer([]byte("d23162b4-87b1-4ff8-b5a5-5cb731d822ed (kube-system_coredns-6955765f44-l9jxq)\n1e8cd861-c584-4e38-8c50-7a71a6ae26bb (local-path-storage_local-path-provisioner-85445b74d4-w5ghw)\n8f1b3173-aa43-4014-adcb-36eae52f7502 (stor-ovn-control-plane)")),
						bytes.NewBuffer([]byte("")),
						nil,
					},
				},
				{
					OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("test error")},
				},
			},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockExecRunner.Mock, tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFnList(&mockKexecIface.Mock, tc.onRetArgsKexecIface)

			_, ipnet, err := net.ParseCIDR(tc.inpSubnetStr)
			if err != nil {
				t.Fail()
			}
			var e error
			if tc.setCfgHybridOvlyEnabled {
				config.HybridOverlay.Enabled = true
				e = UpdateNodeSwitchExcludeIPs(tc.inpNodeName, ipnet)
				config.HybridOverlay.Enabled = false
			} else {
				e = UpdateNodeSwitchExcludeIPs(tc.inpNodeName, ipnet)
			}

			if tc.errExpected {
				assert.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestUpdateIPsSlice(t *testing.T) {
	var tests = []struct {
		name              string
		s, oldIPs, newIPs []string
		want              []string
	}{
		{
			"Tests no matching IPs to remove",
			[]string{"192.168.1.1", "10.0.0.1", "127.0.0.2"},
			[]string{"1.1.1.1"},
			[]string{"9.9.9.9", "fe99::1"},
			[]string{"192.168.1.1", "10.0.0.1", "127.0.0.2"},
		},
		{
			"Tests some matching IPs to replace",
			[]string{"192.168.1.1", "10.0.0.1", "127.0.0.2"},
			[]string{"10.0.0.1"},
			[]string{"9.9.9.9", "fe99::1"},
			[]string{"192.168.1.1", "9.9.9.9", "127.0.0.2"},
		},
		{
			"Tests matching IPv6 to replace",
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			[]string{"3dfd::99ac"},
			[]string{"9.9.9.9", "fe99::1"},
			[]string{"fed9::5", "ab13::1e15", "fe99::1"},
		},
		{
			"Tests match but nothing to replace with",
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			[]string{"3dfd::99ac"},
			[]string{"9.9.9.9"},
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
		},
		{
			"Tests with no newIPs",
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			[]string{"3dfd::99ac"},
			[]string{},
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
		},
		{
			"Tests with no newIPs or oldIPs",
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			[]string{},
			[]string{},
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ans := UpdateIPsSlice(tt.s, tt.oldIPs, tt.newIPs)
			if !reflect.DeepEqual(ans, tt.want) {
				t.Errorf("got %v, want %v", ans, tt.want)
			}
		})
	}
}

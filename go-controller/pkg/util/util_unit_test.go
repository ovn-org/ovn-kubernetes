package util

import (
	"fmt"
	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"net"
	"testing"
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
	mockExecUtilsSvc := new(mocks.ExecUtilRunSvc)
	// below is defined in ovs.go
	RunCmdExecSvcInst = mockExecUtilsSvc
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc                    string
		errExpected             bool
		onRetArgsExecUtilsIface *onCallReturnArgs
	}{
		{
			desc:                    "ovs-vsctl command returns error",
			errExpected:             true,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"Run", []string{"string", "string", "string", "string", "string", "string", "string"}, []interface{}{"", "", fmt.Errorf("test error")}},
		},
		{
			desc:                    "ovs-vsctl command returns empty chassisID along with error",
			errExpected:             true,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"Run", []string{"string", "string", "string", "string", "string", "string", "string"}, []interface{}{"", "", fmt.Errorf("test error")}},
		},
		{
			desc:                    "ovs-vsctl command returns empty chassisID with NO error",
			errExpected:             true,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"Run", []string{"string", "string", "string", "string", "string", "string", "string"}, []interface{}{"", "", nil}},
		},
		{
			desc:                    "ovs-vsctl command returns valid chassisID",
			errExpected:             false,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"Run", []string{"string", "string", "string", "string", "string", "string", "string"}, []interface{}{"4e98c281-f12b-4601-ab5a-a3d759fcb493", "", nil}},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			call := mockExecUtilsSvc.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			ret, e := GetNodeChassisID()
			if tc.errExpected {
				assert.Error(t, e)
			} else {
				assert.Greater(t, len(ret), 0)
			}
		})
	}
}

func TestUpdateNodeSwitchExcludeIPs(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecUtilsSvc := new(mocks.ExecUtilRunSvc)
	// below is defined in ovs.go
	RunCmdExecSvcInst = mockExecUtilsSvc
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc                    string
		inpNodeName             string
		inpSubnetStr            string
		errExpected             bool
		onRetArgsExecUtilsIface []onCallReturnArgs
	}{
		{
			desc:         "IPv4 CIDR, ovs-vsctl fails to list logical switch ports",
			errExpected:  true,
			inpNodeName:  "ovn-control-plane",
			inpSubnetStr: "192.168.1.12/24",
			onRetArgsExecUtilsIface: []onCallReturnArgs{
				{"RunWithEnvVars", []string{"string", "[]string", "string", "string", "string"}, []interface{}{"", "", fmt.Errorf("RunOVNNbctl error")}},
			},
		},
		{
			desc:         "IPv6 CIDR, ovs-vsctl fails to list logical switch ports",
			errExpected:  false,
			inpNodeName:  "ovn-control-plane",
			inpSubnetStr: "fd04:3e42:4a4e:3381::/64",
		},
		{
			desc:         "IPv4 CIDR, haveMangementPort=true, ovs-nbctl command excludeIPs list empty",
			errExpected:  false,
			inpNodeName:  "ovn-control-plane",
			inpSubnetStr: "192.168.1.12/24",
			onRetArgsExecUtilsIface: []onCallReturnArgs{
				{
					"RunWithEnvVars",
					[]string{"string", "[]string", "string", "string", "string"},
					[]interface{}{
						// below is output from command --> ovn-nbctl lsp-list ovn-control-plane
						"7dc3d98a-660a-477b-a6bc-d42904ed59e7 (k8s-ovn-control-plane)\nd23162b4-87b1-4ff8-b5a5-5cb731d822ed (kube-system_coredns-6955765f44-l9jxq)\n1e8cd861-c584-4e38-8c50-7a71a6ae26bb (local-path-storage_local-path-provisioner-85445b74d4-w5ghw)\n8f1b3173-aa43-4014-adcb-36eae52f7502 (stor-ovn-control-plane)",
						"",
						nil,
					},
				},
				{
					"RunWithEnvVars", []string{"string", "[]string", "string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{"", "", nil},
				},
			},
		},
		{
			desc:         "IPv4 CIDR, haveMangementPort=false, ovs-nbctl command with excludeIPs list populated, returns error ",
			errExpected:  false,
			inpNodeName:  "ovn-control-plane",
			inpSubnetStr: "192.168.1.12/24",
			onRetArgsExecUtilsIface: []onCallReturnArgs{
				{
					"RunWithEnvVars",
					[]string{"string", "[]string", "string", "string", "string"},
					[]interface{}{
						// below is output from command --> ovn-nbctl lsp-list ovn-control-plane
						"d23162b4-87b1-4ff8-b5a5-5cb731d822ed (kube-system_coredns-6955765f44-l9jxq)\n1e8cd861-c584-4e38-8c50-7a71a6ae26bb (local-path-storage_local-path-provisioner-85445b74d4-w5ghw)\n8f1b3173-aa43-4014-adcb-36eae52f7502 (stor-ovn-control-plane)",
						"",
						nil,
					},
				},
				{
					"RunWithEnvVars", []string{"string", "[]string", "string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{"", "", fmt.Errorf("test error")},
				},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsExecUtilsIface != nil {
				for _, item := range tc.onRetArgsExecUtilsIface {
					call := mockExecUtilsSvc.On(item.onCallMethodName)
					for _, arg := range item.onCallMethodArgType {
						call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
					}
					for _, elem := range item.retArgList {
						call.ReturnArguments = append(call.ReturnArguments, elem)
					}
					call.Once()
				}
			}

			_, ipnet, err := net.ParseCIDR(tc.inpSubnetStr)
			if err != nil {
				t.Fail()
			}
			e := UpdateNodeSwitchExcludeIPs(tc.inpNodeName, ipnet)
			if tc.errExpected {
				assert.Error(t, e)
			}
			mockExecUtilsSvc.AssertExpectations(t)
		})
	}
}

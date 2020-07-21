package util

import (
	"bytes"
	"fmt"
	"testing"

	goovn "github.com/ebay/go-ovn"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	goovn_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/ebay/go-ovn"
	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestNextIP(t *testing.T) {
	tests := []struct {
		desc      string
		input     string
		expOutput string
	}{
		// Note: test was not successful when providing input of 0.0.0.0
		{
			desc:      "test increment of fourth octet",
			input:     "255.255.255.254",
			expOutput: "255.255.255.255",
		},
		{
			desc:      "test increment of third octet",
			input:     "255.255.254.255",
			expOutput: "255.255.255.0",
		},
		{
			desc:      "test increment of second octet",
			input:     "255.254.255.255",
			expOutput: "255.255.0.0",
		},
		{
			desc:      "test increment of first octet",
			input:     "254.255.255.255",
			expOutput: "255.0.0.0",
		},
		{
			desc:      "IPv6: test increment of eight hextet",
			input:     "2001:db8::ffff",
			expOutput: "2001:db8::1:0",
		},
		{
			desc:      "IPv6: test increment of first hextet",
			input:     "fffe:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
			expOutput: "ffff::",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := NextIP(ovntest.MustParseIP(tc.input))
			t.Log(res.String())
			assert.Equal(t, tc.expOutput, res.String())
		})
	}
}

func TestGetPortAddresses(t *testing.T) {
	mockOvnNBClient := new(goovn_mocks.Client)
	tests := []struct {
		desc                 string
		inpPort              string
		errAssert            bool
		errMatch             error
		onRetArgsOvnNBClient *onCallReturnArgs
	}{
		{
			desc:                 "test path where LSPGet() returns goovn.ErrorSchema/goovn.ErrorNotFound",
			inpPort:              "TEST_PORT",
			onRetArgsOvnNBClient: &onCallReturnArgs{"LSPGet", []string{"string"}, []interface{}{nil, goovn.ErrorSchema}},
		},
		{
			desc:                 "test path where LSPGet() returns an error other than goovn.ErrorSchema/goovn.ErrorNotFound",
			inpPort:              "TEST_PORT",
			errAssert:            true,
			onRetArgsOvnNBClient: &onCallReturnArgs{"LSPGet", []string{"string"}, []interface{}{nil, goovn.ErrorOption}},
		},
		{
			desc:                 "test path where LSPGet() returns nil",
			inpPort:              "TEST_PORT",
			onRetArgsOvnNBClient: &onCallReturnArgs{"LSPGet", []string{"string"}, []interface{}{nil, nil}},
		},
		{
			desc:                 "test path where lsp.DynamicAddresses is a zero length string and len(addresses)==0",
			inpPort:              "TEST_PORT",
			onRetArgsOvnNBClient: &onCallReturnArgs{"LSPGet", []string{"string"}, []interface{}{&goovn.LogicalSwitchPort{DynamicAddresses: ""}, nil}},
		},
		{
			desc:                 "test path where lsp.DynamicAddresses is non-zero length string and value of first address in addresses list is set to dynamic",
			inpPort:              "TEST_PORT",
			onRetArgsOvnNBClient: &onCallReturnArgs{"LSPGet", []string{"string"}, []interface{}{&goovn.LogicalSwitchPort{DynamicAddresses: "06:c6:d4:fb:fb:ba 10.244.2.2"}, nil}},
		},
		{
			desc:                 "test code path where addresses list count is less than 2",
			inpPort:              "TEST_PORT",
			errMatch:             fmt.Errorf("error while obtaining addresses for"),
			onRetArgsOvnNBClient: &onCallReturnArgs{"LSPGet", []string{"string"}, []interface{}{&goovn.LogicalSwitchPort{DynamicAddresses: "06:c6:d4:fb:fb:ba"}, nil}},
		},
		{
			desc:                 "test the code path where ParseMAC fails",
			inpPort:              "TEST_PORT",
			errMatch:             fmt.Errorf("failed to parse logical switch port"),
			onRetArgsOvnNBClient: &onCallReturnArgs{"LSPGet", []string{"string"}, []interface{}{&goovn.LogicalSwitchPort{Addresses: []string{"192.168.1.3", "0a:00:00:00:00:01"}}, nil}},
		},
		{
			desc:                 "test code path where IP address parsing fails",
			inpPort:              "TEST_PORT",
			errMatch:             fmt.Errorf("failed to parse logical switch port"),
			onRetArgsOvnNBClient: &onCallReturnArgs{"LSPGet", []string{"string"}, []interface{}{&goovn.LogicalSwitchPort{Addresses: []string{"192.168.1.3", "0a:00:00:00:00:01"}}, nil}},
		},
		{
			desc:                 "test success path where MAC, IPs are returned",
			inpPort:              "TEST_PORT",
			onRetArgsOvnNBClient: &onCallReturnArgs{"LSPGet", []string{"string"}, []interface{}{&goovn.LogicalSwitchPort{Addresses: []string{"0a:00:00:00:00:01", "192.168.1.3"}}, nil}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockOvnNBClient.On(tc.onRetArgsOvnNBClient.onCallMethodName)
			for _, arg := range tc.onRetArgsOvnNBClient.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsOvnNBClient.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()

			hwAddr, ipList, err := GetPortAddresses(tc.inpPort, mockOvnNBClient)
			t.Log(hwAddr.String(), ipList, err)
			if tc.errAssert {
				assert.Error(t, err)
			} else if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			}
		})
	}
}

func TestGetOVSPortMACAddress(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc                    string
		input                   string
		errExpected             bool
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		// NOTE: May need a test to validate (e.g; zero length string ) portName parameter that is passed to function
		{
			desc:                    "tests code path when RunOVSVsctl returns error",
			input:                   "TEST_PORT",
			errExpected:             true,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, []interface{}{nil, nil, fmt.Errorf("executable file not found in $PATH")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "tests code path when MAC address returned is []",
			input:                   "TEST_PORT",
			errExpected:             true,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("[]")), nil, nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "tests code path when Valid MAC address is returned",
			input:                   "TEST_PORT",
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("00:00:a9:fe:21:01")), nil, nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			call := mockExecRunner.On(tc.onRetArgsExecUtilsIface.onCallMethodName)
			for _, arg := range tc.onRetArgsExecUtilsIface.onCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsExecUtilsIface.retArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()
			ifaceCall := mockKexecIface.On(tc.onRetArgsKexecIface.onCallMethodName)
			for _, arg := range tc.onRetArgsKexecIface.onCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range tc.onRetArgsKexecIface.retArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, ret)
			}
			ifaceCall.Once()

			res, err := GetOVSPortMACAddress(tc.input)
			t.Log(res, err)
			if tc.errExpected {
				assert.Error(t, err)
			}
			mockKexecIface.AssertExpectations(t)
			mockExecRunner.AssertExpectations(t)
		})
	}
}

func TestIPFamilyName(t *testing.T) {
	tests := []struct {
		desc   string
		input  bool
		expOut string
	}{
		{
			desc:   "verify bool value `true` maps to IPv6",
			input:  true,
			expOut: "IPv6",
		},
		{
			desc:   "verify bool value `false` maps to IPv4",
			input:  false,
			expOut: "IPv4",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := IPFamilyName(tc.input)
			t.Log(res)
			assert.Equal(t, res, tc.expOut)
		})
	}
}

func TestMatchIPFamily(t *testing.T) {
	tests := []struct {
		desc        string
		inpIPFamily bool // true matches with IPv6 and false with IPv4
		inpSubnets  []string
		expErr      bool
	}{
		{
			desc:        "negative: attempt to get IPv6 from an empty IP list",
			inpIPFamily: true,
			inpSubnets:  []string{},
			expErr:      true,
		},
		{
			desc:        "negative: attempt to get IPv4 from an empty IP list",
			inpIPFamily: false,
			inpSubnets:  []string{},
			expErr:      true,
		},
		{
			desc:        "positive: retrieve IPv6 valid address",
			inpIPFamily: true,
			inpSubnets:  []string{"192.168.1.0/24", "fd01::1/24"},
		},
		{
			desc:        "positive: retrieve IPv4 valid address",
			inpIPFamily: false,
			inpSubnets:  []string{"fd01::1/24", "192.168.1.0/24"},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, err := MatchIPFamily(tc.inpIPFamily, ovntest.MustParseIPNets(tc.inpSubnets...))
			t.Log(res, err)
			if !tc.expErr {
				assert.NotNil(t, res)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

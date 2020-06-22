package util

import (
	"bytes"
	"fmt"
	"testing"

	goovn "github.com/ebay/go-ovn"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
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
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	mockOVNNBClient := ovntest.NewMockOVNClient(goovn.DBNB)

	tests := []struct {
		desc                    string
		input                   string
		errExpected             bool
		onRetArgsExecUtilsIface *onCallReturnArgs
		onRetArgsKexecIface     *onCallReturnArgs
	}{
		// NOTE: May need a test to validate portName parameter that is passed to function
		{
			desc:                    "tests code path when RunOVNNbctl returns error",
			input:                   "TEST_PORT",
			errExpected:             false,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{nil, nil, fmt.Errorf("executable file not found in $PATH")}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "test code path when RunOVNNbctl returns valid output",
			input:                   "TEST_PORT",
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("22:e9:ac:f4:00:04 10.244.0.3\n[dynamic]")), nil, nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "test code path where IP address parsing fails",
			input:                   "TEST_PORT",
			errExpected:             false,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("22:e9:ac:f4:00:04 10.244.0.\n[dynamic]")), nil, nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
		{
			desc:                    "test code path where addresses list count is less than 2",
			input:                   "TEST_PORT",
			errExpected:             false,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{nil, nil, nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		},
		// {
		// 	desc:                    "test code path when RunOVNNbctl returns only static address",
		// 	input:                   "TEST_PORT",
		// 	onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("[]\n[\"06:c6:d4:fb:fb:ba 10.244.2.2\"]")), nil, nil}},
		// 	onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		// },
		// {
		// 	desc:                    "test the code path when RunOVNNbctl returns `[]\\n[dynamic]`",
		// 	input:                   "TEST_PORT",
		// 	onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("[]\n[dynamic]")), nil, nil}},
		// 	onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
		// },
		{
			desc:                    "test the code path where ParseMAC fails",
			input:                   "TEST_PORT",
			errExpected:             false,
			onRetArgsExecUtilsIface: &onCallReturnArgs{"RunCmd", []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{bytes.NewBuffer([]byte("\"22:e9:ac::00:04 10.244.0.3\\n[dynamic]\"")), nil, nil}},
			onRetArgsKexecIface:     &onCallReturnArgs{"Command", []string{"string", "string", "string", "string", "string", "string", "string", "string"}, []interface{}{mockCmd}},
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

			hwAddr, ipList, err := GetPortAddresses(tc.input, mockOVNNBClient)
			t.Log(hwAddr.String(), ipList, err)
			if tc.errExpected {
				assert.Error(t, err)
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

package util

import (
	"bytes"
	"fmt"
	"net"
	"testing"

	nbdb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/assert"
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

func stringPtr(str string) *string {
	return &str
}

const (
	hwAddr    string = "06:c6:d4:fb:fb:ba"
	badHWAddr string = "NotMAC"
	badIPAddr string = "NOTIP"
	ipAddr    string = "10.244.2.2"
	portName  string = "test-pod"
)

func TestExtractPortAddresses(t *testing.T) {
	tests := []struct {
		desc       string
		lsp        *nbdb.LogicalSwitchPort
		errMatch   error
		isNotFound bool
		hasNoIP    bool
	}{
		{
			desc: "test path where lsp.DynamicAddresses is a zero length string and len(addresses)==0",
			lsp: &nbdb.LogicalSwitchPort{
				Name:             "test-pod",
				DynamicAddresses: stringPtr(hwAddr + " " + ipAddr),
			},
		},
		{
			desc: "test path where lsp.DynamicAddresses is non-zero length string and value of first address in addresses list is set to dynamic",
			lsp: &nbdb.LogicalSwitchPort{
				Name:             portName,
				DynamicAddresses: stringPtr(hwAddr + " " + ipAddr),
				Addresses:        []string{"dynamic"},
			},
		},
		{
			desc: "test code path where port has MAC but no IPs",
			lsp: &nbdb.LogicalSwitchPort{
				Name:             "test-pod",
				DynamicAddresses: stringPtr(hwAddr),
			},
			hasNoIP: true,
		},
		{
			desc: "test the code path where ParseMAC fails",
			lsp: &nbdb.LogicalSwitchPort{
				Name:             portName,
				DynamicAddresses: stringPtr(badHWAddr),
			},
			errMatch: fmt.Errorf("failed to parse logical switch port \"%s\" MAC \"%s\": address %s: invalid MAC address", portName, badHWAddr, badHWAddr),
		},
		{
			desc: "test code path where IP address parsing fails",
			lsp: &nbdb.LogicalSwitchPort{
				Name:      portName,
				Addresses: []string{fmt.Sprintf("%s %s", hwAddr, badIPAddr)},
			},
			errMatch: fmt.Errorf("failed to parse logical switch port \"%s\" IP \"%s\" is not a valid ip address", portName, badIPAddr),
		},
		{
			desc: "test success path with len(lsp.Addresses) > 0 and lsp.DynamicAddresses = nil",
			lsp: &nbdb.LogicalSwitchPort{
				Name:      portName,
				Addresses: []string{fmt.Sprintf("%s %s", hwAddr, ipAddr)},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			hardwareAddr, ips, err := ExtractPortAddresses(tc.lsp)
			if tc.isNotFound {
				assert.Nil(t, hardwareAddr)
				assert.Nil(t, ips)
				assert.Nil(t, err)

			} else if tc.hasNoIP {
				assert.Equal(t, hardwareAddr.String(), hwAddr)
				assert.Nil(t, ips)
			} else if tc.errMatch != nil {
				assert.Equal(t, err, tc.errMatch)
			} else {
				assert.Equal(t, hardwareAddr.String(), hwAddr)
				assert.Equal(t, len(ips), 1)
				assert.Equal(t, ips[0].String(), ipAddr)
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
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		// NOTE: May need a test to validate (e.g; zero length string ) portName parameter that is passed to function
		{
			desc:                    "tests code path when RunOVSVsctl returns error",
			input:                   "TEST_PORT",
			errExpected:             true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("executable file not found in $PATH")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "tests code path when MAC address returned is []",
			input:                   "TEST_PORT",
			errExpected:             true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("[]")), nil, nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "tests code path when Valid MAC address is returned",
			input:                   "TEST_PORT",
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("00:00:a9:fe:21:01")), nil, nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

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
		desc      string
		inpIsIPv6 bool // true matches with IPv6 and false with IPv4
		inpIPs    []string
		expected  string
	}{
		{
			desc:      "negative: attempt to get IPv6 from an empty IP list",
			inpIsIPv6: true,
			inpIPs:    []string{},
			expected:  "",
		},
		{
			desc:      "negative: attempt to get IPv4 from an empty IP list",
			inpIsIPv6: false,
			inpIPs:    []string{},
			expected:  "",
		},
		{
			desc:      "negative: attempt to get IPv4 from IPv6-only list",
			inpIsIPv6: false,
			inpIPs:    []string{"fd01::1"},
			expected:  "",
		},
		{
			desc:      "positive: retrieve IPv6 valid address",
			inpIsIPv6: true,
			inpIPs:    []string{"192.168.1.0", "fd01::1"},
			expected:  "fd01::1",
		},
		{
			desc:      "positive: retrieve IPv4 valid address",
			inpIsIPv6: false,
			inpIPs:    []string{"192.168.1.0", "fd01::1"},
			expected:  "192.168.1.0",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, err := MatchIPFamily(tc.inpIsIPv6, ovntest.MustParseIPs(tc.inpIPs...))
			t.Log(res, err)
			if tc.expected == "" {
				assert.Error(t, err)
			} else {
				assert.Equal(t, res[0], ovntest.MustParseIP(tc.expected))
			}
		})
	}
}

func TestMatchFirstIPNetFamily(t *testing.T) {
	tests := []struct {
		desc       string
		inpIsIPv6  bool // true matches with IPv6 and false with IPv4
		inpSubnets []string
		expected   string
	}{
		{
			desc:       "negative: attempt to get IPv6 from an empty IP list",
			inpIsIPv6:  true,
			inpSubnets: []string{},
			expected:   "",
		},
		{
			desc:       "negative: attempt to get IPv4 from an empty IP list",
			inpIsIPv6:  false,
			inpSubnets: []string{},
			expected:   "",
		},
		{
			desc:       "negative: attempt to get IPv4 from IPv6-only list",
			inpIsIPv6:  false,
			inpSubnets: []string{"fd01::1/64"},
			expected:   "",
		},
		{
			desc:       "positive: retrieve IPv6 valid address",
			inpIsIPv6:  true,
			inpSubnets: []string{"192.168.1.0/24", "fd01::1/64"},
			expected:   "fd01::1/64",
		},
		{
			desc:       "positive: retrieve IPv4 valid address",
			inpIsIPv6:  false,
			inpSubnets: []string{"192.168.1.0/24", "fd01::1/64"},
			expected:   "192.168.1.0/24",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, err := MatchFirstIPNetFamily(tc.inpIsIPv6, ovntest.MustParseIPNets(tc.inpSubnets...))
			t.Log(res, err)
			if tc.expected == "" {
				assert.Error(t, err)
			} else {
				assert.Equal(t, res, ovntest.MustParseIPNet(tc.expected))
			}
		})
	}
}

func TestJoinHostPortInt32(t *testing.T) {
	tests := []struct {
		desc    string
		inpHost string
		inpPort int32
		outExp  string
	}{
		{
			desc:    "empty string for host name and a negative port number",
			inpPort: -25,
			outExp:  ":-25",
		},
		{
			desc:    "valid non-zero length string for host name",
			inpHost: "hostname",
			inpPort: 22,
			outExp:  "hostname:22",
		},
		{
			desc:    "valid IPv4 address as host name",
			inpHost: "192.168.1.15",
			inpPort: 22,
			outExp:  "192.168.1.15:22",
		},
		{
			desc:    "valid IPv6 address as host name",
			inpHost: "fd01::1234",
			inpPort: 22,
			outExp:  "[fd01::1234]:22",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := JoinHostPortInt32(tc.inpHost, tc.inpPort)
			t.Log(res)
			assert.Equal(t, res, tc.outExp)
		})
	}
}

func TestIPAddrToHWAddr(t *testing.T) {
	tests := []struct {
		desc   string
		inpIP  net.IP
		outExp net.HardwareAddr
	}{
		{
			desc:   "test IPv4 instance of net.IP",
			inpIP:  ovntest.MustParseIP("192.168.1.5"),
			outExp: ovntest.MustParseMAC("0a:58:c0:a8:01:05"),
		},
		{
			desc:  "test IPv6 instance of net.IP",
			inpIP: ovntest.MustParseIP("fd01::1234"),
			// 0a:58:ee:33:fc:1a generated from util.IPAddrToHWAddr(net.ParseIP(""fd01::1234")).String()
			outExp: ovntest.MustParseMAC("0a:58:11:37:a6:26"),
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := IPAddrToHWAddr(tc.inpIP)
			t.Log(res)
			assert.Equal(t, res, tc.outExp)
		})
	}
}

func TestJoinIPs(t *testing.T) {
	tests := []struct {
		desc         string
		inpIPList    []net.IP
		inpSeparator string
		outExp       string
	}{
		{
			desc:         "an empty net.IPNet list with ',' separator",
			inpSeparator: ",",
			outExp:       "",
		},
		{
			desc:         "single item in net.IPNet list with `;` separator",
			inpIPList:    ovntest.MustParseIPs("192.168.1.5"),
			inpSeparator: ";",
			outExp:       "192.168.1.5",
		},
		{
			desc:         "two items in net.IPNet list with `;` separator",
			inpIPList:    ovntest.MustParseIPs("192.168.1.5", "192.168.1.6"),
			inpSeparator: ";",
			outExp:       "192.168.1.5;192.168.1.6",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := JoinIPs(tc.inpIPList, tc.inpSeparator)
			t.Log(res)
			assert.Equal(t, res, tc.outExp)
		})
	}
}

func TestJoinIPNets(t *testing.T) {
	tests := []struct {
		desc         string
		inpIPNetList []*net.IPNet
		inpSeparator string
		outExp       string
	}{
		{
			desc:         "an empty net.IPNet list with ',' separator",
			inpSeparator: ",",
			outExp:       "",
		},
		{
			desc:         "single item in net.IPNet list with `;` separator",
			inpIPNetList: ovntest.MustParseIPNets("192.168.1.5/24"),
			inpSeparator: ";",
			outExp:       "192.168.1.5/24",
		},
		{
			desc:         "two items in net.IPNet list with `;` separator",
			inpIPNetList: ovntest.MustParseIPNets("192.168.1.5/24", "192.168.1.6/24"),
			inpSeparator: ";",
			outExp:       "192.168.1.5/24;192.168.1.6/24",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := JoinIPNets(tc.inpIPNetList, tc.inpSeparator)
			t.Log(res)
			assert.Equal(t, res, tc.outExp)
		})
	}
}

func TestJoinIPNetIPs(t *testing.T) {
	tests := []struct {
		desc         string
		inpIPNetList []*net.IPNet
		inpSeparator string
		outExp       string
	}{
		{
			desc:         "an empty net.IPNet list with ',' separator",
			inpSeparator: ",",
			outExp:       "",
		},
		{
			desc:         "single item in net.IPNet list with `;` separator",
			inpIPNetList: ovntest.MustParseIPNets("192.168.1.5/24"),
			inpSeparator: ";",
			outExp:       "192.168.1.5",
		},
		{
			desc:         "two items in net.IPNet list with `;` separator",
			inpIPNetList: ovntest.MustParseIPNets("192.168.1.5/24", "192.168.1.6/24"),
			inpSeparator: ";",
			outExp:       "192.168.1.5;192.168.1.6",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := JoinIPNetIPs(tc.inpIPNetList, tc.inpSeparator)
			t.Log(res)
			assert.Equal(t, res, tc.outExp)
		})
	}
}

func TestContainsCIDR(t *testing.T) {
	tests := []struct {
		desc      string
		inpIPNet1 *net.IPNet
		inpIPNet2 *net.IPNet
		outExp    bool
	}{
		{
			desc:      "positive case, ipnet1 contains ipnet2",
			inpIPNet1: ovntest.MustParseIPNet("192.168.1.1/24"),
			inpIPNet2: ovntest.MustParseIPNet("192.168.1.16/28"),
			outExp:    true,
		},
		{
			desc:      "positive case, ipnet1 is the same as ipnet2",
			inpIPNet1: ovntest.MustParseIPNet("192.168.1.1/24"),
			inpIPNet2: ovntest.MustParseIPNet("192.168.1.1/24"),
			outExp:    true,
		},
		{
			desc:      "negative case, ipnet1 does not contain ipnet2",
			inpIPNet1: ovntest.MustParseIPNet("192.168.1.1/25"),
			inpIPNet2: ovntest.MustParseIPNet("192.168.1.1/24"),
			outExp:    false,
		},
		{
			desc:      "negative case, ipnet1 and ipnet2 does not overlap",
			inpIPNet1: ovntest.MustParseIPNet("192.168.1.1/24"),
			inpIPNet2: ovntest.MustParseIPNet("192.168.2.1/24"),
			outExp:    false,
		},
		{
			desc:      "positive case, ipnet1 contains ipnet2, IPv6 case",
			inpIPNet1: ovntest.MustParseIPNet("2001:db8:3c4d::/48"),
			inpIPNet2: ovntest.MustParseIPNet("2001:db8:3c4d:15::/64"),
			outExp:    true,
		},
		{
			desc:      "negative case, ipv4 compare with ipv6",
			inpIPNet1: ovntest.MustParseIPNet("2001:db8:3c4d::/48"),
			inpIPNet2: ovntest.MustParseIPNet("192.168.2.1/24"),
			outExp:    false,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := ContainsCIDR(tc.inpIPNet1, tc.inpIPNet2)
			t.Log(res)
			assert.Equal(t, res, tc.outExp)
		})
	}
}

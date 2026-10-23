// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"net"
	"testing"

	iputils "github.com/containernetworking/plugins/pkg/ip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
)

func TestCalculateRouteTableID(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	t.Cleanup(func() {
		require.NoError(t, config.PrepareTestConfig())
	})

	assert.Equal(t, 1005, CalculateRouteTableID(5))

	config.OvnKubeNode.RoutingTableIDStart = 2000
	assert.Equal(t, 2005, CalculateRouteTableID(5))
}

func TestNextSloppyIP(t *testing.T) {
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
			desc:      "IPv6: test increment of leading zeros",
			input:     "10:100:200:2::",
			expOutput: "10:100:200:2::1",
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
			res := iputils.NextIP(ovntest.MustParseIP(tc.input))
			t.Log(res.String())
			assert.Equal(t, tc.expOutput, res.String())
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
			assert.Equal(t, tc.expOut, res)
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
			assert.Equal(t, tc.outExp, res)
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
			assert.Equal(t, tc.outExp, res)
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
			assert.Equal(t, tc.outExp, res)
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
			assert.Equal(t, tc.outExp, res)
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
			assert.Equal(t, tc.outExp, res)
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
			assert.Equal(t, tc.outExp, res)
		})
	}
}

func TestIsUsableEthernetMAC(t *testing.T) {
	tests := []struct {
		name     string
		mac      net.HardwareAddr
		expected bool
	}{
		{name: "unicast Ethernet", mac: net.HardwareAddr{0x02, 0x42, 0xac, 0x12, 0x00, 0x03}, expected: true},
		{name: "nil", mac: nil, expected: false},
		{name: "all-zero", mac: net.HardwareAddr{0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, expected: false},
		{name: "multicast", mac: net.HardwareAddr{0x01, 0x00, 0x5e, 0x00, 0x00, 0x01}, expected: false},
		{name: "broadcast", mac: net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, expected: false},
		{name: "EUI-64 length", mac: net.HardwareAddr{0x02, 0x42, 0xac, 0x12, 0x00, 0x03, 0x00, 0x01}, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUsableEthernetMAC(tt.mac); got != tt.expected {
				t.Errorf("IsUsableEthernetMAC(%v) = %v, expected %v", tt.mac, got, tt.expected)
			}
		})
	}
}

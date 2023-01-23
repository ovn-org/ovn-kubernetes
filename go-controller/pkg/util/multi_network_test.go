package util

import (
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

func parseIPNets(ipNetStrs ...string) []*net.IPNet {
	ipNets := make([]*net.IPNet, len(ipNetStrs))
	for i := range ipNetStrs {
		_, ipNet, err := net.ParseCIDR(ipNetStrs[i])
		if err != nil {
			panic(fmt.Sprintf("Could not parse %q as a CIDR: %v", ipNetStrs[i], err))
		}
		ipNets[i] = ipNet
	}
	return ipNets
}

func TestParseSubnetsString(t *testing.T) {
	tests := []struct {
		desc      string
		input     string
		expOutput []*net.IPNet
	}{
		{
			desc:      "positive, single IPv4 subnet",
			input:     "192.168.1.1/24",
			expOutput: parseIPNets("192.168.1.1/24"),
		},
		{
			desc:      "positive, multiple IPv4 subnet",
			input:     "192.168.1.1/24, 192.168.2.1/24",
			expOutput: parseIPNets("192.168.1.1/24", "192.168.2.1/24"),
		},
		{
			desc:      "positive, empty string",
			input:     " ",
			expOutput: []*net.IPNet{},
		},
		{
			desc:      "positive, single IPv6 subnet",
			input:     "2001:db8:3c4d::/48",
			expOutput: parseIPNets("2001:db8:3c4d::/48"),
		},
		{
			desc:      "positive, multiple IPv6 subnets",
			input:     "2001:db8:3c4d::/48, 2001:db8::1:0/64",
			expOutput: parseIPNets("2001:db8:3c4d::/48", "2001:db8::0:0/64"),
		},
		{
			desc:      "negative, incorrect subnets case 1",
			input:     "192.168.1.1, 192.168.1.3/24",
			expOutput: nil,
		},
		{
			desc:      "negative, incorrect subnets case 2",
			input:     "abcde, 192.168.1.1/24",
			expOutput: nil,
		},
		{
			desc:      "negative, incorrect subnets case 2",
			input:     "192.168.1.1/24; 192.168.1.2/24",
			expOutput: nil,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			resIPNets, err := parseSubnetsString(tc.input)
			t.Log(resIPNets, err)
			if tc.expOutput == nil {
				assert.Error(t, err)
			} else {
				assert.Equal(t, len(resIPNets), len(tc.expOutput))
				for i := range resIPNets {
					assert.Equal(t, *(resIPNets[i]), *(tc.expOutput[i]))
				}
			}
		})
	}
}

package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
)

func TestConvertK8sProtocolToOVNProtocol(t *testing.T) {
	testcases := []struct {
		desc     string
		protocol v1.Protocol
		expected string
	}{
		{
			"unsupported protocol",
			"kube",
			"",
		},
		{
			"tcp protocol",
			"TCP",
			"tcp",
		},
		{
			"sctp protocol",
			"SCTP",
			"sctp",
		},
		{
			"udp protocol",
			"UDP",
			"udp",
		},
	}
	for _, tc := range testcases {
		protocol := ConvertK8sProtocolToOVNProtocol(v1.Protocol(tc.protocol))
		if tc.expected == "" {
			assert.Equal(t, len(protocol), 0)
			continue
		}
		assert.Equal(t, protocol, tc.expected)
	}
}

func TestGetL4Match(t *testing.T) {
	testcases := []struct {
		desc        string
		protocol    string
		portPolices []*NetworkPolicyPort
		expected    string
	}{
		{
			"empty port policies",
			"tcp",
			[]*NetworkPolicyPort{},
			"",
		},
		{
			"single tcp port",
			"tcp",
			[]*NetworkPolicyPort{
				{
					Protocol: "tcp",
					Port:     800,
				},
			},
			"tcp && tcp.dst==800",
		},
		{
			"valid individual tcp ports",
			"tcp",
			[]*NetworkPolicyPort{
				{
					Protocol: "tcp",
					Port:     800,
				},
				{
					Protocol: "tcp",
					Port:     900,
				},
				{
					Protocol: "tcp",
					Port:     1900,
				},
			},
			"tcp && tcp.dst=={800,900,1900}",
		},
		{
			"single udp port range",
			"udp",
			[]*NetworkPolicyPort{
				{
					Protocol: "udp",
					Port:     800,
					EndPort:  850,
				},
			},
			"udp && 800<=udp.dst<=850",
		},
		{
			"valid tcp port ranges only",
			"tcp",
			[]*NetworkPolicyPort{
				{
					Protocol: "tcp",
					Port:     800,
					EndPort:  850,
				},
				{
					Protocol: "tcp",
					Port:     900,
					EndPort:  950,
				},
				{
					Protocol: "tcp",
					Port:     1900,
					EndPort:  2000,
				},
			},
			"tcp && (800<=tcp.dst<=850 || 900<=tcp.dst<=950 || 1900<=tcp.dst<=2000)",
		},
		{
			"valid udp ports and ranges",
			"udp",
			[]*NetworkPolicyPort{
				{
					Protocol: "udp",
					Port:     800,
				},
				{
					Protocol: "udp",
					Port:     900,
				},
				{
					Protocol: "udp",
					Port:     1900,
					EndPort:  2000,
				},
				{
					Protocol: "udp",
					Port:     4900,
					EndPort:  5000,
				},
			},
			"udp && (udp.dst=={800,900} || 1900<=udp.dst<=2000 || 4900<=udp.dst<=5000)",
		},
		{
			"just sctp",
			"sctp",
			[]*NetworkPolicyPort{
				{
					Protocol: "sctp",
				},
			},
			"sctp",
		},
	}

	for _, tc := range testcases {
		protocolPortsMap := getProtocolPortsMap(tc.portPolices)
		if tc.expected == "" {
			assert.Equal(t, len(protocolPortsMap), 0)
			continue
		}
		assert.Equal(t, len(protocolPortsMap), 1)
		assert.Contains(t, protocolPortsMap, tc.protocol)
		l4Match := getL4Match(tc.protocol, protocolPortsMap[tc.protocol])
		assert.Equal(t, tc.expected, l4Match)
	}
}

func TestGetL3L4MatchesFromNamedPorts(t *testing.T) {
	testcases := []struct {
		desc     string
		protocol []string
		ports    map[string][]NamedNetworkPolicyPort
		expected []string // per protocol
	}{
		{
			"nil namedPort policies",
			[]string{"tcp"},
			nil,
			[]string{""},
		},
		{
			"empty map port policies",
			[]string{"tcp"},
			make(map[string][]NamedNetworkPolicyPort, 0),
			[]string{""},
		},
		{
			"empty NamedPorts map",
			[]string{"sctp"},
			map[string][]NamedNetworkPolicyPort{},
			[]string{""},
		},
		{
			"empty NamedPorts map with no matching pods",
			[]string{"sctp"},
			map[string][]NamedNetworkPolicyPort{"un-matched-pods": {}},
			[]string{""},
		},
		{
			"empty NamedPorts map with no matching pods for multiple ports",
			[]string{"sctp"},
			map[string][]NamedNetworkPolicyPort{
				"un-matched-port-too-bad": {},
				"un-matched-port-sad":     {},
			},
			[]string{""},
		},
		{
			"single namedPort and single v4 namedPort representation",
			[]string{"sctp"},
			map[string][]NamedNetworkPolicyPort{
				"web": {{L4PodPort: "35356", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.4", L4Protocol: "sctp"}},
			},
			[]string{"sctp && ((ip4.dst == 10.128.1.4 && sctp.dst == 35356))"},
		},
		{
			"single namedPort and multiple v4 namedPort representations",
			[]string{"tcp"},
			map[string][]NamedNetworkPolicyPort{
				"web": {
					{L4PodPort: "35356", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.4", L4Protocol: "tcp"},
					{L4PodPort: "35354", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.5", L4Protocol: "tcp"},
				},
			},
			[]string{"tcp && ((ip4.dst == 10.128.1.4 && tcp.dst == 35356) || (ip4.dst == 10.128.1.5 && tcp.dst == 35354))"},
		},
		{
			"single namedPort and multiple v4 & v6 namedPort representations",
			[]string{"tcp", "sctp"},
			map[string][]NamedNetworkPolicyPort{
				"web": {
					{L4PodPort: "35356", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.4", L4Protocol: "tcp"},
					{L4PodPort: "35356", L3PodIPFamily: "ip6", L3PodIP: "fe00:10:128:1::4", L4Protocol: "tcp"},
					{L4PodPort: "35354", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.5", L4Protocol: "sctp"},
					{L4PodPort: "35354", L3PodIPFamily: "ip6", L3PodIP: "fe00:10:128:1::5", L4Protocol: "sctp"},
				},
			},
			[]string{"tcp && ((ip4.dst == 10.128.1.4 && tcp.dst == 35356) || (ip6.dst == fe00:10:128:1::4 && tcp.dst == 35356))",
				"sctp && ((ip4.dst == 10.128.1.5 && sctp.dst == 35354) || (ip6.dst == fe00:10:128:1::5 && sctp.dst == 35354))"},
		},
		{
			"multiple namedPorts and multiple v4 namedPort representations per port; same protocol",
			[]string{"tcp"},
			map[string][]NamedNetworkPolicyPort{
				"web": {
					{L4PodPort: "35356", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.4", L4Protocol: "tcp"},
					{L4PodPort: "35354", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.5", L4Protocol: "tcp"},
				},
				"web123": {
					{L4PodPort: "9999", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.3", L4Protocol: "tcp"},
					{L4PodPort: "9998", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.5", L4Protocol: "tcp"},
				},
			},
			[]string{"tcp && ((ip4.dst == 10.128.1.4 && tcp.dst == 35356) || (ip4.dst == 10.128.1.5 && tcp.dst == 35354) || (ip4.dst == 10.128.1.3 && tcp.dst == 9999) || (ip4.dst == 10.128.1.5 && tcp.dst == 9998))"},
		},
		{
			"multiple namedPorts and multiple v4 & v6 namedPort representations per port; all protocols",
			[]string{"tcp", "sctp", "udp"},
			map[string][]NamedNetworkPolicyPort{
				"web": {
					{L4PodPort: "35356", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.4", L4Protocol: "tcp"},
					{L4PodPort: "35356", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.5", L4Protocol: "tcp"},
					{L4PodPort: "3535", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.5", L4Protocol: "sctp"},
					{L4PodPort: "3535", L3PodIPFamily: "ip6", L3PodIP: "fe00:10:128:1::5", L4Protocol: "sctp"},
					{L4PodPort: "53", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.4", L4Protocol: "udp"},
					{L4PodPort: "53", L3PodIPFamily: "ip6", L3PodIP: "fe00:10:128:1::4", L4Protocol: "udp"},
				},
				"web123": {
					{L4PodPort: "9999", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.3", L4Protocol: "tcp"},
					{L4PodPort: "9999", L3PodIPFamily: "ip6", L3PodIP: "fe00:10:128:1::3", L4Protocol: "tcp"},
					{L4PodPort: "35354", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.5", L4Protocol: "sctp"},
					{L4PodPort: "35354", L3PodIPFamily: "ip6", L3PodIP: "fe00:10:128:1::5", L4Protocol: "sctp"},
					{L4PodPort: "5353", L3PodIPFamily: "ip4", L3PodIP: "10.128.1.4", L4Protocol: "udp"},
					{L4PodPort: "5353", L3PodIPFamily: "ip6", L3PodIP: "fe00:10:128:1::4", L4Protocol: "udp"},
				},
			},
			[]string{"tcp && ((ip4.dst == 10.128.1.4 && tcp.dst == 35356) || (ip4.dst == 10.128.1.5 && tcp.dst == 35356) || (ip4.dst == 10.128.1.3 && tcp.dst == 9999) || (ip6.dst == fe00:10:128:1::3 && tcp.dst == 9999))",
				"sctp && ((ip4.dst == 10.128.1.5 && sctp.dst == 3535) || (ip6.dst == fe00:10:128:1::5 && sctp.dst == 3535) || (ip4.dst == 10.128.1.5 && sctp.dst == 35354) || (ip6.dst == fe00:10:128:1::5 && sctp.dst == 35354))",
				"udp && ((ip4.dst == 10.128.1.4 && udp.dst == 53) || (ip6.dst == fe00:10:128:1::4 && udp.dst == 53) || (ip4.dst == 10.128.1.4 && udp.dst == 5353) || (ip6.dst == fe00:10:128:1::4 && udp.dst == 5353))"},
		},
	}

	for _, tc := range testcases {
		l3l4MatchPerProtocol := GetL3L4MatchesFromNamedPorts(tc.ports)
		for i, expected := range tc.expected {
			if expected == "" {
				assert.Equal(t, len(l3l4MatchPerProtocol), 0)
				continue
			}
			assert.Contains(t, l3l4MatchPerProtocol, tc.protocol[i])
			assert.Equal(t, expected, l3l4MatchPerProtocol[tc.protocol[i]])
		}
	}
}

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
		protocol := convertK8sProtocolToOVNProtocol(v1.Protocol(tc.protocol))
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

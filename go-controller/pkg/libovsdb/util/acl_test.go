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

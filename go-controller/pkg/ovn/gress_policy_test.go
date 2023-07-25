package ovn

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/stretchr/testify/assert"

	"testing"

	knet "k8s.io/api/networking/v1"
)

func TestGetMatchFromIPBlock(t *testing.T) {
	testcases := []struct {
		desc       string
		ipBlocks   []*knet.IPBlock
		lportMatch string
		l4Match    string
		expected   []string
	}{
		{
			desc: "IPv4 only no except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR: "0.0.0.0/0",
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected:   []string{"ip4.src == 0.0.0.0/0 && input && fake"},
		},
		{
			desc: "multiple IPv4 only no except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR: "0.0.0.0/0",
				},
				{
					CIDR: "10.1.0.0/16",
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected: []string{"ip4.src == 0.0.0.0/0 && input && fake",
				"ip4.src == 10.1.0.0/16 && input && fake"},
		},
		{
			desc: "IPv6 only no except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR: "fd00:10:244:3::49/32",
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected:   []string{"ip6.src == fd00:10:244:3::49/32 && input && fake"},
		},
		{
			desc: "mixed IPv4 and IPv6  no except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR: "::/0",
				},
				{
					CIDR: "0.0.0.0/0",
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected: []string{"ip6.src == ::/0 && input && fake",
				"ip4.src == 0.0.0.0/0 && input && fake"},
		},
		{
			desc: "IPv4 only with except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR:   "0.0.0.0/0",
					Except: []string{"10.1.0.0/16"},
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected:   []string{"ip4.src == 0.0.0.0/0 && ip4.src != {10.1.0.0/16} && input && fake"},
		},
		{
			desc: "multiple IPv4 with except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR:   "0.0.0.0/0",
					Except: []string{"10.1.0.0/16"},
				},
				{
					CIDR: "10.1.0.0/16",
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected: []string{"ip4.src == 0.0.0.0/0 && ip4.src != {10.1.0.0/16} && input && fake",
				"ip4.src == 10.1.0.0/16 && input && fake"},
		},
		{
			desc: "IPv4 with IPv4 except",
			ipBlocks: []*knet.IPBlock{
				{
					CIDR:   "0.0.0.0/0",
					Except: []string{"10.1.0.0/16"},
				},
			},
			lportMatch: "fake",
			l4Match:    "input",
			expected:   []string{"ip4.src == 0.0.0.0/0 && ip4.src != {10.1.0.0/16} && input && fake"},
		},
	}

	for _, tc := range testcases {
		gressPolicy := newGressPolicy(knet.PolicyTypeIngress, 5, "testing", "test",
			DefaultNetworkControllerName, false, &util.DefaultNetInfo{})
		for _, ipBlock := range tc.ipBlocks {
			gressPolicy.addIPBlock(ipBlock)
		}
		output := gressPolicy.getMatchFromIPBlock(tc.lportMatch, tc.l4Match)
		assert.Equal(t, tc.expected, output)
	}
}

func TestGetL4Match(t *testing.T) {
	testcases := []struct {
		desc        string
		protocol    string
		portPolices []*portPolicy
		expected    string
	}{
		{
			"unsupported protocol",
			"kube",
			[]*portPolicy{
				{
					protocol: "kube",
					port:     0,
					endPort:  0,
				},
			},
			"",
		},
		{
			"empty port policies",
			"tcp",
			[]*portPolicy{},
			"",
		},
		{
			"single tcp port",
			"tcp",
			[]*portPolicy{
				{
					protocol: "TCP",
					port:     800,
				},
			},
			"tcp && tcp.dst==800",
		},
		{
			"valid individual tcp ports",
			"tcp",
			[]*portPolicy{
				{
					protocol: "TCP",
					port:     800,
				},
				{
					protocol: "TCP",
					port:     900,
				},
				{
					protocol: "TCP",
					port:     1900,
				},
			},
			"tcp && tcp.dst=={800,900,1900}",
		},
		{
			"single udp port range",
			"udp",
			[]*portPolicy{
				{
					protocol: "UDP",
					port:     800,
					endPort:  850,
				},
			},
			"udp && 800<=udp.dst<=850",
		},
		{
			"valid tcp port ranges only",
			"tcp",
			[]*portPolicy{
				{
					protocol: "TCP",
					port:     800,
					endPort:  850,
				},
				{
					protocol: "TCP",
					port:     900,
					endPort:  950,
				},
				{
					protocol: "TCP",
					port:     1900,
					endPort:  2000,
				},
			},
			"tcp && (800<=tcp.dst<=850 || 900<=tcp.dst<=950 || 1900<=tcp.dst<=2000)",
		},
		{
			"valid udp ports and ranges",
			"udp",
			[]*portPolicy{
				{
					protocol: "UDP",
					port:     800,
				},
				{
					protocol: "UDP",
					port:     900,
				},
				{
					protocol: "UDP",
					port:     1900,
					endPort:  2000,
				},
				{
					protocol: "UDP",
					port:     4900,
					endPort:  5000,
				},
			},
			"udp && (udp.dst=={800,900} || 1900<=udp.dst<=2000 || 4900<=udp.dst<=5000)",
		},
		{
			"just sctp",
			"sctp",
			[]*portPolicy{
				{
					protocol: "SCTP",
				},
			},
			"sctp",
		},
	}

	for _, tc := range testcases {
		gp := &gressPolicy{
			policyNamespace: "default",
			policyName:      "test-policy",
			policyType:      "Ingress",
			portPolicies:    tc.portPolices,
		}
		protocolPortsMap := gp.getProtocolPortsMap()
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

package util

import (
	"net"
	"testing"

	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TestParseSubnets(t *testing.T) {
	tests := []struct {
		desc             string
		topology         string
		subnets          string
		excludes         string
		expectedSubnets  []config.CIDRNetworkEntry
		expectedExcludes []*net.IPNet
		expectError      bool
	}{
		{
			desc:     "multiple subnets layer 3 topology",
			topology: types.Layer3Topology,
			subnets:  "192.168.1.1/26/28, fda6::/48",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR:             ovntest.MustParseIPNet("192.168.1.0/26"),
					HostSubnetLength: 28,
				},
				{
					CIDR:             ovntest.MustParseIPNet("fda6::/48"),
					HostSubnetLength: 64,
				},
			},
		},
		{
			desc:     "empty subnets layer 3 topology",
			topology: types.Layer3Topology,
		},
		{
			desc:     "multiple subnets and excludes layer 2 topology",
			topology: types.Layer2Topology,
			subnets:  "192.168.1.1/26, fda6::/48",
			excludes: "192.168.1.38/32, fda6::38/128",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR: ovntest.MustParseIPNet("192.168.1.0/26"),
				},
				{
					CIDR: ovntest.MustParseIPNet("fda6::/48"),
				},
			},
			expectedExcludes: ovntest.MustParseIPNets("192.168.1.38/32", "fda6::38/128"),
		},
		{
			desc:     "empty subnets layer 2 topology",
			topology: types.Layer2Topology,
		},
		{
			desc:        "invalid formatted excludes layer 2 topology",
			topology:    types.Layer2Topology,
			subnets:     "192.168.1.1/26",
			excludes:    "192.168.1.1/26/32",
			expectError: true,
		},
		{
			desc:        "invalid not contained excludes layer 2 topology",
			topology:    types.Layer2Topology,
			subnets:     "fda6::/48",
			excludes:    "fda7::38/128",
			expectError: true,
		},
		{
			desc:     "multiple subnets and excludes localnet topology",
			topology: types.LocalnetTopology,
			subnets:  "192.168.1.1/26, fda6::/48",
			excludes: "192.168.1.38/32, fda6::38/128",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR: ovntest.MustParseIPNet("192.168.1.0/26"),
				},
				{
					CIDR: ovntest.MustParseIPNet("fda6::/48"),
				},
			},
			expectedExcludes: ovntest.MustParseIPNets("192.168.1.38/32", "fda6::38/128"),
		},
		{
			desc:     "empty subnets localnet topology",
			topology: types.LocalnetTopology,
		},
		{
			desc:        "invalid formatted excludes localnet topology",
			topology:    types.LocalnetTopology,
			subnets:     "fda6::/48",
			excludes:    "fda6::1/48/128",
			expectError: true,
		},
		{
			desc:        "invalid not contained excludes localnet topology",
			topology:    types.LocalnetTopology,
			subnets:     "192.168.1.1/26",
			excludes:    "192.168.2.38/32",
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			subnets, excludes, err := parseSubnets(tc.subnets, tc.excludes, tc.topology)
			if tc.expectError {
				g.Expect(err).To(gomega.HaveOccurred())
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(subnets).To(gomega.ConsistOf(tc.expectedSubnets))
			g.Expect(excludes).To(gomega.ConsistOf(tc.expectedExcludes))
		})
	}
}

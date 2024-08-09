package config

import (
	"net"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
)

func TestParseClusterSubnetEntries(t *testing.T) {
	tests := []struct {
		name                        string
		cmdLineArg                  string
		clusterNetworks             []CIDRNetworkEntry
		withDefaultHostSubnetLength bool
		defaultIPv4HostSubnetLength int
		defaultIPv6HostSubnetLength int
		expectedErr                 bool
	}{
		{
			name:            "Single CIDR correctly formatted",
			cmdLineArg:      "10.132.0.0/26/28",
			clusterNetworks: []CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("10.132.0.0/26"), HostSubnetLength: 28}},
			expectedErr:     false,
		},
		{
			name:       "Two CIDRs correctly formatted",
			cmdLineArg: "10.132.0.0/26/28,10.133.0.0/26/28",
			clusterNetworks: []CIDRNetworkEntry{
				{CIDR: ovntest.MustParseIPNet("10.132.0.0/26"), HostSubnetLength: 28},
				{CIDR: ovntest.MustParseIPNet("10.133.0.0/26"), HostSubnetLength: 28},
			},
			expectedErr: false,
		},
		{
			name:            "Test that defaulting to hostsubnetlength 8 still works",
			cmdLineArg:      "10.128.0.0/14",
			clusterNetworks: []CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("10.128.0.0/14"), HostSubnetLength: 24}},
			expectedErr:     false,
		},
		{
			name:            "empty cmdLineArg",
			cmdLineArg:      "",
			clusterNetworks: nil,
			expectedErr:     true,
		},
		{
			name:            "improperly formated CIDR",
			cmdLineArg:      "10.132.0.-/26/28",
			clusterNetworks: nil,
			expectedErr:     true,
		},
		{
			name:            "negative HostsubnetLength",
			cmdLineArg:      "10.132.0.0/26/-22",
			clusterNetworks: nil,
			expectedErr:     true,
		},
		{
			name:            "HostSubnetLength smaller than cluster subnet",
			cmdLineArg:      "10.132.0.0/26/24",
			clusterNetworks: nil,
			expectedErr:     true,
		},
		{
			name:            "Cluster Subnet length too large",
			cmdLineArg:      "10.132.0.0/25",
			clusterNetworks: nil,
			expectedErr:     true,
		},
		{
			name:            "Cluster Subnet length same as host subnet length",
			cmdLineArg:      "10.132.0.0/24/24",
			clusterNetworks: nil,
			expectedErr:     true,
		},
		{
			name:            "IPv4 host Subnet invalid",
			cmdLineArg:      "10.132.0.0/24/33",
			clusterNetworks: nil,
			expectedErr:     true,
		},
		{
			name:            "Test that defaulting to hostsubnetlength with 24 bit cluster prefix fails",
			cmdLineArg:      "10.128.0.0/24",
			clusterNetworks: nil,
			expectedErr:     true,
		},
		{
			name:            "IPv6",
			cmdLineArg:      "fda6::/48/64",
			clusterNetworks: []CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("fda6::/48"), HostSubnetLength: 64}},
			expectedErr:     false,
		},
		{
			name:            "IPv6 defaults to /64 hostsubnets",
			cmdLineArg:      "fda6::/48",
			clusterNetworks: []CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("fda6::/48"), HostSubnetLength: 64}},
			expectedErr:     false,
		},
		{
			name:            "IPv6 doesn't allow longer than /64 hostsubnet",
			cmdLineArg:      "fda6::/48/56",
			clusterNetworks: nil,
			expectedErr:     true,
		},
		{
			name:            "IPv6 doesn't allow shorter than /64 hostsubnet",
			cmdLineArg:      "fda6::/48/72",
			clusterNetworks: nil,
			expectedErr:     true,
		},
		{
			name:            "IPv6 can't use /64 cluster net",
			cmdLineArg:      "fda6::/64",
			clusterNetworks: nil,
			expectedErr:     true,
		},
		{
			name:       "Two CIDRs correctly formatted with spaces",
			cmdLineArg: "10.132.0.0/26/28, 10.133.0.0/26/28",
			clusterNetworks: []CIDRNetworkEntry{
				{CIDR: ovntest.MustParseIPNet("10.132.0.0/26"), HostSubnetLength: 28},
				{CIDR: ovntest.MustParseIPNet("10.133.0.0/26"), HostSubnetLength: 28},
			},
			expectedErr: false,
		},
		{
			name:                        "Single IPv4 CIDR with default host subnet length",
			cmdLineArg:                  "10.132.0.0/26",
			withDefaultHostSubnetLength: true,
			defaultIPv4HostSubnetLength: 28,
			clusterNetworks:             []CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("10.132.0.0/26"), HostSubnetLength: 28}},
			expectedErr:                 false,
		},
		{
			name:                        "Single IPv4 CIDR with invalid default host subnet length",
			cmdLineArg:                  "10.132.0.0/26",
			withDefaultHostSubnetLength: true,
			defaultIPv4HostSubnetLength: 26,
			expectedErr:                 true,
		},
		{
			name:                        "Single IPv4 CIDR no host subnet length allowed or validated",
			cmdLineArg:                  "10.132.0.1/32",
			withDefaultHostSubnetLength: true,
			defaultIPv4HostSubnetLength: 0,
			clusterNetworks:             []CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("10.132.0.1/32")}},
			expectedErr:                 false,
		},
		{
			name:                        "Single IPv4 CIDR no host subnet length allowed",
			cmdLineArg:                  "10.132.0.0/26/28",
			withDefaultHostSubnetLength: true,
			defaultIPv4HostSubnetLength: 0,
			expectedErr:                 true,
		},
		{
			name:                        "Single IPv6 CIDR with default host subnet length",
			cmdLineArg:                  "fda6::/48",
			withDefaultHostSubnetLength: true,
			defaultIPv6HostSubnetLength: 64,
			clusterNetworks:             []CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("fda6::/48"), HostSubnetLength: 64}},
			expectedErr:                 false,
		},
		{
			name:                        "Single IPv6 CIDR with invalid default host subnet length",
			cmdLineArg:                  "fda6::/64",
			withDefaultHostSubnetLength: true,
			defaultIPv6HostSubnetLength: 48,
			expectedErr:                 true,
		},
		{
			name:                        "Single IPv6 CIDR no host subnet length allowed or validated",
			cmdLineArg:                  "fda6::1/128",
			withDefaultHostSubnetLength: true,
			defaultIPv6HostSubnetLength: 0,
			clusterNetworks:             []CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("fda6::1/128")}},
			expectedErr:                 false,
		},
		{
			name:                        "Single IPv6 CIDR no host subnet length allowed",
			cmdLineArg:                  "fda6::/48/64",
			withDefaultHostSubnetLength: true,
			defaultIPv6HostSubnetLength: 0,
			expectedErr:                 true,
		},
	}

	for _, tc := range tests {
		var err error
		var parsedList []CIDRNetworkEntry
		if tc.withDefaultHostSubnetLength {
			parsedList, err = ParseClusterSubnetEntriesWithDefaults(tc.cmdLineArg, tc.defaultIPv4HostSubnetLength, tc.defaultIPv6HostSubnetLength)
		} else {
			parsedList, err = ParseClusterSubnetEntries(tc.cmdLineArg)
		}
		if err != nil && !tc.expectedErr {
			t.Errorf("Test case \"%s\" expected no errors, got %v", tc.name, err)
		}
		if len(tc.clusterNetworks) != len(parsedList) {
			t.Errorf("Test case \"%s\" expected to output the same number of entries as parseClusterSubnetEntries", tc.name)
		} else {
			for index, entry := range parsedList {
				if entry.CIDR.String() != tc.clusterNetworks[index].CIDR.String() {
					t.Errorf("Test case \"%s\" expected entry[%d].CIDR: %s to equal tc.clusterNetworks[%d].CIDR: %s", tc.name, index, entry.CIDR.String(), index, tc.clusterNetworks[index].CIDR.String())
				}
				if entry.HostSubnetLength != tc.clusterNetworks[index].HostSubnetLength {
					t.Errorf("Test case \"%s\" expected entry[%d].HostSubnetLength: %d to equal tc.clusterNetworks[%d].HostSubnetLength: %d", tc.name, index, entry.HostSubnetLength, index, tc.clusterNetworks[index].HostSubnetLength)
				}
			}
		}
	}
}

func Test_checkForOverlap(t *testing.T) {
	tests := []struct {
		name               string
		cidrList           []*net.IPNet
		joinSubnetCIDRList []*net.IPNet
		shouldError        bool
	}{
		{
			name:        "empty cidrList",
			cidrList:    nil,
			shouldError: false,
		},
		{
			name:        "non-overlapping",
			cidrList:    ovntest.MustParseIPNets("10.132.0.0/26", "10.133.0.0/26"),
			shouldError: false,
		},
		{
			name:        "duplicate entry",
			cidrList:    ovntest.MustParseIPNets("10.132.0.0/26", "10.132.0.0/26"),
			shouldError: true,
		},
		{
			name:        "proper subset",
			cidrList:    ovntest.MustParseIPNets("10.132.0.0/26", "10.0.0.0/8"),
			shouldError: true,
		},
		{
			name:        "proper superset",
			cidrList:    ovntest.MustParseIPNets("10.0.0.0/8", "10.133.0.0/26"),
			shouldError: true,
		},
		{
			name:        "overlap",
			cidrList:    ovntest.MustParseIPNets("10.1.0.0/14", "10.0.0.0/15"),
			shouldError: true,
		},
		{
			name:        "non-overlapping V4 ipnet entries",
			cidrList:    ovntest.MustParseIPNets("10.132.0.0/26", "192.168.0.0/16", "172.16.0.0/16"),
			shouldError: false,
		},
		{
			name:        "non-overlapping V6 ipnet entries",
			cidrList:    ovntest.MustParseIPNets("fd98:8000::/65", "fd99::/64", "fd98:1::/64"),
			shouldError: false,
		},

		{
			name:               "ipnet entry equal to V4 joinSubnet value",
			cidrList:           ovntest.MustParseIPNets("100.64.0.0/16"),
			joinSubnetCIDRList: ovntest.MustParseIPNets("100.64.0.0/16", "fd98::/64"),
			shouldError:        true,
		},
		{
			name:               "ipnet entry equal to V6 joinSubnet value",
			cidrList:           ovntest.MustParseIPNets("fd90::/48"),
			joinSubnetCIDRList: ovntest.MustParseIPNets("100.64.0.0/16", "fd90::/48"),
			shouldError:        true,
		},
		{
			name:               "ipnet entry overlapping with V4 joinSubnet value",
			cidrList:           ovntest.MustParseIPNets("100.63.128.0/17"),
			joinSubnetCIDRList: ovntest.MustParseIPNets("100.63.0.0/16", "fd98::/64"),
			shouldError:        true,
		},
		{
			name:               "ipnet entry overlapping with V6 joinSubnet value",
			cidrList:           ovntest.MustParseIPNets("fd99::/68"),
			joinSubnetCIDRList: ovntest.MustParseIPNets("100.64.0.0/16", "fd99::/64"),
			shouldError:        true,
		},
		{
			name:               "ipnet entry encompassing the V4 joinSubnet value",
			cidrList:           ovntest.MustParseIPNets("100.0.0.0/8"),
			joinSubnetCIDRList: ovntest.MustParseIPNets("100.64.0.0/16", "fd98::/64"),
			shouldError:        true,
		},
		{
			name:               "ipnet entry encompassing the V6 joinSubnet value",
			cidrList:           ovntest.MustParseIPNets("fd98::/63"),
			joinSubnetCIDRList: ovntest.MustParseIPNets("100.64.0.0/16", "fd98::/64"),
			shouldError:        true,
		},
	}

	for _, tc := range tests {
		allSubnets := NewConfigSubnets()
		for _, joinSubnet := range tc.joinSubnetCIDRList {
			allSubnets.Append(ConfigSubnetJoin, joinSubnet)
		}
		for _, subnet := range tc.cidrList {
			allSubnets.Append(ConfigSubnetCluster, subnet)
		}

		err := allSubnets.CheckForOverlaps()
		if err == nil && tc.shouldError {
			t.Errorf("testcase \"%s\" failed to find overlap", tc.name)
		} else if err != nil && !tc.shouldError {
			t.Errorf("testcase \"%s\" erroneously found overlap: %v", tc.name, err)
		}
	}
}

func TestParseFlowCollectors(t *testing.T) {
	hp, err := ParseFlowCollectors("10.0.0.2:3030,:8888,[2020:1111:f::1:0933]:3333,10.0.0.3:3031")
	if err != nil {
		t.Error("can't parse flowCollectors", err)
	}
	if len(hp) != 4 ||
		hp[0].Host.String() != "10.0.0.2" || hp[0].Port != 3030 ||
		hp[1].Host != nil || hp[1].Port != 8888 ||
		hp[2].Host.String() != "2020:1111:f::1:933" || hp[2].Port != 3333 ||
		hp[3].Host.String() != "10.0.0.3" || hp[3].Port != 3031 {
		t.Errorf("parsed hostPorts returned unexpected results: %+v", hp)
	}
}

func TestParseFlowCollectors_DeduplicateEntries(t *testing.T) {
	hp, err := ParseFlowCollectors("10.0.0.2:3030,[1::1]:3333,10.0.0.2:3030,[1::1]:3333")
	if err != nil {
		t.Error("can't parse flowCollectors", err)
	}
	if len(hp) != 2 ||
		hp[0].Host.String() != "10.0.0.2" || hp[0].Port != 3030 ||
		hp[1].Host.String() != "1::1" || hp[1].Port != 3333 {
		t.Errorf("parsed hostPorts returned unexpected results: %+v", hp)
	}
}

func TestParseFlowCollectors_DeduplicateEquivalentEntries(t *testing.T) {
	hp, err := ParseFlowCollectors(
		"[fd00:1101:0000:0001:0000:0000:0000:0002]:1234,[fd00:1101:0000:0001::0002]:1234")
	if err != nil {
		t.Error("can't parse flowCollectors", err)
	}
	if len(hp) != 1 ||
		hp[0].Host.String() != "fd00:1101:0:1::2" || hp[0].Port != 1234 {
		t.Errorf("parsed hostPorts returned unexpected results: %+v", hp)
	}
}

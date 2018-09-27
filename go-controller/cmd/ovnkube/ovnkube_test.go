package main

import (
	ovncluster "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/cluster"
	"net"
	"testing"
)

func returnIPNetPointers(input string) *net.IPNet {
	_, ipNet, _ := net.ParseCIDR(input)
	return ipNet
}

func TestParseClusterSubnetEntries(t *testing.T) {
	tests := []struct {
		name            string
		cmdLineArg      string
		clusterNetworks []ovncluster.CIDRNetworkEntry
		expectedErr     bool
	}{
		{
			name:            "Single CIDR correctly formatted",
			cmdLineArg:      "10.132.0.0/26/28",
			clusterNetworks: []ovncluster.CIDRNetworkEntry{{CIDR: returnIPNetPointers("10.132.0.0/26"), HostSubnetLength: 28}},
			expectedErr:     false,
		},
		{
			name:       "Two CIDRs correctly formatted",
			cmdLineArg: "10.132.0.0/26/28,10.133.0.0/26/28",
			clusterNetworks: []ovncluster.CIDRNetworkEntry{
				{CIDR: returnIPNetPointers("10.132.0.0/26"), HostSubnetLength: 28},
				{CIDR: returnIPNetPointers("10.133.0.0/26"), HostSubnetLength: 28},
			},
			expectedErr: false,
		},
		{
			name:            "Test that defaulting to hostsubnetlength 8 still works",
			cmdLineArg:      "10.128.0.0/14",
			clusterNetworks: []ovncluster.CIDRNetworkEntry{{CIDR: returnIPNetPointers("10.128.0.0/14"), HostSubnetLength: 24}},
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
	}

	for _, tc := range tests {

		parsedList, err := parseClusterSubnetEntries(tc.cmdLineArg)
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

func TestCidrsOverlap(t *testing.T) {
	tests := []struct {
		name           string
		cidr           *net.IPNet
		cidrList       []ovncluster.CIDRNetworkEntry
		expectedOutput bool
	}{
		{
			name:           "empty cidrList",
			cidr:           returnIPNetPointers("10.132.0.0/26"),
			cidrList:       nil,
			expectedOutput: false,
		},
		{
			name: "non-overlapping",
			cidr: returnIPNetPointers("10.132.0.0/26"),
			cidrList: []ovncluster.CIDRNetworkEntry{
				{CIDR: returnIPNetPointers("10.133.0.0/26")},
			},
			expectedOutput: false,
		},
		{
			name: "cidr equal to an entry in the list",
			cidr: returnIPNetPointers("10.132.0.0/26"),
			cidrList: []ovncluster.CIDRNetworkEntry{
				{CIDR: returnIPNetPointers("10.132.0.0/26")},
			},
			expectedOutput: true,
		},
		{
			name: "proposed cidr is a proper subset of a list entry",
			cidr: returnIPNetPointers("10.132.0.0/26"),
			cidrList: []ovncluster.CIDRNetworkEntry{
				{CIDR: returnIPNetPointers("10.0.0.0/8")},
			},
			expectedOutput: true,
		},
		{
			name: "proposed cidr is a proper superset of a list entry",
			cidr: returnIPNetPointers("10.0.0.0/8"),
			cidrList: []ovncluster.CIDRNetworkEntry{
				{CIDR: returnIPNetPointers("10.133.0.0/26")},
			},
			expectedOutput: true,
		},
		{
			name: "proposed cidr overlaps a list entry",
			cidr: returnIPNetPointers("10.1.0.0/14"),
			cidrList: []ovncluster.CIDRNetworkEntry{
				{CIDR: returnIPNetPointers("10.0.0.0/15")},
			},
			expectedOutput: true,
		},
	}

	for _, tc := range tests {

		if result := cidrsOverlap(tc.cidr, tc.cidrList); result != tc.expectedOutput {
			t.Errorf("testcase \"%s\" expected output %t cidrsOverlap returns %t", tc.name, tc.expectedOutput, result)
		}
	}
}

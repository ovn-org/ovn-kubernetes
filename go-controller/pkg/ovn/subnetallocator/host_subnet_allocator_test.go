package subnetallocator

import (
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
)

func rangesFromStrings(ranges []string, networkLens []int) ([]config.CIDRNetworkEntry, error) {
	entries := make([]config.CIDRNetworkEntry, 0, len(ranges))
	for i, subnetString := range ranges {
		_, subnet, err := net.ParseCIDR(subnetString)
		if err != nil {
			return nil, fmt.Errorf("error parsing subnet %s", subnetString)
		}
		entries = append(entries, config.CIDRNetworkEntry{
			CIDR:             subnet,
			HostSubnetLength: networkLens[i],
		})
	}
	return entries, nil
}

type existingAllocation struct {
	subnet string
	owner  string
}

func TestController_allocateNodeSubnets(t *testing.T) {
	tests := []struct {
		name          string
		networkRanges []string
		networkLens   []int
		configIPv4    bool
		configIPv6    bool
		existingNets  []*net.IPNet
		alreadyOwned  *existingAllocation
		// to be converted during the test to []*net.IPNet
		wantStr   []string
		allocated int
		wantErr   bool
	}{
		{
			name:          "new node, IPv4 only cluster",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  nil,
			wantStr:       []string{"172.16.0.0/24"},
			allocated:     1,
			wantErr:       false,
		},
		{
			name:          "new node, IPv6 only cluster",
			networkRanges: []string{"2001:db2::/56"},
			networkLens:   []int{64},
			configIPv4:    false,
			configIPv6:    true,
			existingNets:  nil,
			wantStr:       []string{"2001:db2::/64"},
			allocated:     1,
			wantErr:       false,
		},
		{
			name:          "existing annotated node, IPv4 only cluster",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  ovntest.MustParseIPNets("172.16.8.0/24"),
			wantStr:       []string{"172.16.8.0/24"},
			allocated:     0,
			wantErr:       false,
		},
		{
			name:          "existing annotated node, IPv6 only cluster",
			networkRanges: []string{"2001:db2::/32"},
			networkLens:   []int{64},
			configIPv4:    false,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("2001:db2:1::/64"),
			wantStr:       []string{"2001:db2:1::/64"},
			allocated:     0,
			wantErr:       false,
		},
		{
			name:          "new node, dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/12"},
			networkLens:   []int{24, 24},
			configIPv4:    true,
			configIPv6:    true,
			existingNets:  nil,
			wantStr:       []string{"172.16.0.0/24", "2000::/24"},
			allocated:     2,
			wantErr:       false,
		},
		{
			name:          "existing annotated node, dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/12"},
			networkLens:   []int{24, 24},
			configIPv4:    true,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("172.16.5.0/24", "2000::/24"),
			wantStr:       []string{"172.16.5.0/24", "2000::/24"},
			allocated:     0,
			wantErr:       false,
		},
		{
			name:          "single IPv4 to dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/12"},
			networkLens:   []int{24, 24},
			configIPv4:    true,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("172.16.5.0/24"),
			wantStr:       []string{"172.16.5.0/24", "2000::/24"},
			allocated:     1,
			wantErr:       false,
		},
		{
			name:          "single IPv6 to dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/16"},
			networkLens:   []int{24, 32},
			configIPv4:    true,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("2000:1::/32"),
			wantStr:       []string{"2000:1::/32", "172.16.0.0/24"},
			allocated:     1,
			wantErr:       false,
		},
		{
			name:          "dual stack cluster to single IPv4",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  ovntest.MustParseIPNets("172.16.5.0/24", "2000:2::/24"),
			wantStr:       []string{"172.16.5.0/24"},
			allocated:     0,
			wantErr:       false,
		},
		{
			name:          "dual stack cluster to single IPv6",
			networkRanges: []string{"2001:db2:1::/56"},
			networkLens:   []int{64},
			configIPv4:    false,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("172.16.5.0/24", "2001:db2:1:2::/64"),
			wantStr:       []string{"2001:db2:1:2::/64"},
			allocated:     0,
			wantErr:       false,
		},
		{
			name:          "new node, OVN wrong configuration: IPv4 only cluster but IPv6 range",
			networkRanges: []string{"2001:db2::/64"},
			networkLens:   []int{112},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  nil,
			wantStr:       nil,
			allocated:     0,
			wantErr:       true,
		},
		{
			name:          "existing annotated node outside cluster CIDR",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  ovntest.MustParseIPNets("10.1.0.0/24"),
			wantStr:       []string{"172.16.0.0/24"},
			allocated:     1,
		},
		{
			name:          "existing annotated node with too many subnets",
			networkRanges: []string{"172.16.0.0/16", "2001:db2:1::/56"},
			networkLens:   []int{24, 64},
			configIPv4:    true,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("172.16.0.0/24", "172.16.1.0/24", "2001:db2:1:2::/64", "2001:db2:1:3::/64"),
			wantStr:       []string{"172.16.0.0/24", "2001:db2:1:2::/64"},
			allocated:     0,
		},
		{
			name:          "existing annotated node with too many subnets, one of which is already owned",
			networkRanges: []string{"172.16.0.0/16", "2001:db2:1::/56"},
			networkLens:   []int{24, 64},
			configIPv4:    true,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("172.16.0.0/24", "172.16.1.0/24", "2001:db2:1:2::/64", "2001:db2:1:3::/64"),
			alreadyOwned: &existingAllocation{
				owner:  "another-node",
				subnet: "172.16.1.0/24",
			},
			wantStr:   []string{"172.16.0.0/24", "2001:db2:1:2::/64"},
			allocated: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sna := NewHostSubnetAllocator()

			ranges, err := rangesFromStrings(tt.networkRanges, tt.networkLens)
			if err != nil {
				t.Fatal(err)
			}
			if err := sna.InitRanges(ranges); err != nil {
				t.Fatalf("Failed to initialize network ranges: %v", err)
			}

			if tt.alreadyOwned != nil {
				err = sna.MarkSubnetsAllocated(tt.alreadyOwned.owner, ovntest.MustParseIPNets(tt.alreadyOwned.subnet)...)
				if err != nil {
					t.Fatal(err)
				}
			}

			// test network allocation works correctly
			got, allocated, err := sna.AllocateNodeSubnets("testnode", tt.existingNets, tt.configIPv4, tt.configIPv6)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Controller.addNode() error = %v, wantErr %v", err, tt.wantErr)
			}

			var want []*net.IPNet
			for _, netStr := range tt.wantStr {
				_, ipnet, err := net.ParseCIDR(netStr)
				if err != nil {
					t.Fatalf("Error parsing subnet %s", netStr)
				}
				want = append(want, ipnet)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Controller.allocateNodeSubnets() = %v, want %v", got, want)
			}

			if len(allocated) != tt.allocated {
				t.Fatalf("Expected %d subnets allocated, received %d", tt.allocated, len(allocated))
			}

			// Ensure an already owned subnet isn't touched
			if tt.alreadyOwned != nil {
				err = sna.MarkSubnetsAllocated("blahblah", ovntest.MustParseIPNets(tt.alreadyOwned.subnet)...)
				if err == nil {
					t.Fatal("Expected subnet to already be allocated by a different node")
				}
			}
		})
	}
}

func TestController_allocateNodeSubnets_ReleaseOnError(t *testing.T) {
	ranges, err := rangesFromStrings([]string{"172.16.0.0/16", "2000::/127"}, []int{24, 127})
	if err != nil {
		t.Fatal(err)
	}
	sna := NewHostSubnetAllocator()
	if err := sna.InitRanges(ranges); err != nil {
		t.Fatalf("Failed to initialize network ranges: %v", err)
	}

	// Mark all v6 subnets already allocated to force an error in AllocateNodeSubnets()
	if err := sna.MarkSubnetsAllocated("blah", ovntest.MustParseIPNet("2000::/127")); err != nil {
		t.Fatalf("MarkSubnetsAllocated() expected no error but got: %v", err)
	}

	// test network allocation works correctly
	_, v4usedBefore, _, v6usedBefore := sna.base.Usage()
	got, allocated, err := sna.AllocateNodeSubnets("testNode", nil, true, true)
	if err == nil {
		t.Fatalf("AllocateNodeSubnets() expected error but got success")
	}
	if got != nil {
		t.Fatalf("AllocateNodeSubnets() expected no existing host subnets, got %v", got)
	}
	if allocated != nil {
		t.Fatalf("AllocateNodeSubnets() expected no allocated subnets, got %v", allocated)
	}

	_, v4usedAfter, _, v6usedAfter := sna.base.Usage()
	if v4usedAfter != v4usedBefore {
		t.Fatalf("Expected %d v4 allocated subnets, but got %d", v4usedBefore, v4usedAfter)
	}
	if v6usedAfter != v6usedBefore {
		t.Fatalf("Expected %d v6 allocated subnets, but got %d", v6usedBefore, v6usedAfter)
	}
}

func ipnetStringsToSlice(strings []string) ([]*net.IPNet, error) {
	slice := make([]*net.IPNet, 0, len(strings))
	for _, s := range strings {
		_, subnet, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("error parsing subnet %s", s)
		}
		slice = append(slice, subnet)
	}
	return slice, nil
}

func TestController_markSubnetsAllocated(t *testing.T) {
	tests := []struct {
		name          string
		networkRanges []string
		networkLens   []int
		markedSubnets []string
		secondSubnets []string
		wantErr       bool
	}{
		{
			name:          "IPv4 no conflict",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			markedSubnets: []string{"172.16.0.0/24"},
			secondSubnets: []string{"172.16.1.0/24"},
			wantErr:       false,
		},
		{
			name:          "IPv4 conflict",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			markedSubnets: []string{"172.16.0.0/24"},
			secondSubnets: []string{"172.16.0.0/24"},
			wantErr:       true,
		},
		{
			name:          "IPv6 no conflict",
			networkRanges: []string{"2001:db2::/56"},
			networkLens:   []int{64},
			markedSubnets: []string{"2001:db2:0:1::/64"},
			secondSubnets: []string{"2001:db2:0:2::/64"},
			wantErr:       false,
		},
		{
			name:          "IPv6 conflict",
			networkRanges: []string{"2001:db2::/56"},
			networkLens:   []int{64},
			markedSubnets: []string{"2001:db2::/64"},
			secondSubnets: []string{"2001:db2::/64"},
			wantErr:       true,
		},
		{
			name:          "dual-stack no conflict",
			networkRanges: []string{"2001:db2::/56", "172.16.0.0/16"},
			networkLens:   []int{64, 24},
			markedSubnets: []string{"2001:db2:0:1::/64", "172.16.0.0/24"},
			secondSubnets: []string{"2001:db2:0:2::/64", "172.16.1.0/24"},
			wantErr:       false,
		},
		{
			name:          "dual-stack v4 conflict",
			networkRanges: []string{"2001:db2::/56", "172.16.0.0/16"},
			networkLens:   []int{64, 24},
			markedSubnets: []string{"2001:db2:0:1::/64", "172.16.0.0/24"},
			secondSubnets: []string{"2001:db2:0:2::/64", "172.16.0.0/24"},
			wantErr:       true,
		},
		{
			name:          "dual-stack v6 conflict",
			networkRanges: []string{"2001:db2::/56", "172.16.0.0/16"},
			networkLens:   []int{64, 24},
			markedSubnets: []string{"2001:db2:0:1::/64", "172.16.0.0/24"},
			secondSubnets: []string{"2001:db2:0:1::/64", "172.16.1.0/24"},
			wantErr:       true,
		},
		{
			name:          "dual-stack both conflict",
			networkRanges: []string{"2001:db2::/56", "172.16.0.0/16"},
			networkLens:   []int{64, 24},
			markedSubnets: []string{"2001:db2:0:1::/64", "172.16.0.0/24"},
			secondSubnets: []string{"2001:db2:0:1::/64", "172.16.0.0/24"},
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sna := NewHostSubnetAllocator()

			ranges, err := rangesFromStrings(tt.networkRanges, tt.networkLens)
			if err != nil {
				t.Fatal(err)
			}
			if err := sna.InitRanges(ranges); err != nil {
				t.Fatalf("Failed to initialize network ranges: %v", err)
			}

			subnets, err := ipnetStringsToSlice(tt.markedSubnets)
			if err != nil {
				t.Fatal(err)
			}
			if err := sna.MarkSubnetsAllocated("node1", subnets...); err != nil {
				t.Fatalf("Failed to mark allocated subnets: %v", err)
			}

			subnets, err = ipnetStringsToSlice(tt.secondSubnets)
			if err != nil {
				t.Fatal(err)
			}
			err = sna.MarkSubnetsAllocated("node2", subnets...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Mark second subnets allocated error %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

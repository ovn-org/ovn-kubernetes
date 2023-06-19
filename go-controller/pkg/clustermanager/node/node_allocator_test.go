package node

import (
	"fmt"
	"net"
	"reflect"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
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
			ranges, err := rangesFromStrings(tt.networkRanges, tt.networkLens)
			if err != nil {
				t.Fatal(err)
			}
			config.Default.ClusterSubnets = ranges

			netInfo, err := util.NewNetInfo(
				&ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName},
				},
			)
			if err != nil {
				t.Fatal(err)
			}

			na := &NodeAllocator{
				netInfo:                netInfo,
				clusterSubnetAllocator: NewSubnetAllocator(),
			}

			if err := na.Init(); err != nil {
				t.Fatalf("Failed to initialize node allocator: %v", err)
			}

			if tt.alreadyOwned != nil {
				err := na.clusterSubnetAllocator.MarkAllocatedNetworks(tt.alreadyOwned.owner, ovntest.MustParseIPNets(tt.alreadyOwned.subnet)...)
				if err != nil {
					t.Fatal(err)
				}
			}

			// test network allocation works correctly
			got, allocated, err := na.allocateNodeSubnets(na.clusterSubnetAllocator, "testnode", tt.existingNets, tt.configIPv4, tt.configIPv6)
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
				err = na.clusterSubnetAllocator.MarkAllocatedNetworks("blahblah", ovntest.MustParseIPNets(tt.alreadyOwned.subnet)...)
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
	config.Default.ClusterSubnets = ranges

	netInfo, err := util.NewNetInfo(
		&ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	na := &NodeAllocator{
		netInfo:                netInfo,
		clusterSubnetAllocator: NewSubnetAllocator(),
	}

	if err := na.Init(); err != nil {
		t.Fatalf("Failed to initialize node allocator: %v", err)
	}

	// Mark all v6 subnets already allocated to force an error in AllocateNodeSubnets()
	if err := na.clusterSubnetAllocator.MarkAllocatedNetworks("blah", ovntest.MustParseIPNet("2000::/127")); err != nil {
		t.Fatalf("MarkAllocatedNetworks() expected no error but got: %v", err)
	}

	// test network allocation works correctly
	v4usedBefore, v6usedBefore := na.clusterSubnetAllocator.Usage()
	got, allocated, err := na.allocateNodeSubnets(na.clusterSubnetAllocator, "testNode", nil, true, true)
	if err == nil {
		t.Fatalf("allocateNodeSubnets() expected error but got success")
	}
	if got != nil {
		t.Fatalf("allocateNodeSubnets() expected no existing host subnets, got %v", got)
	}
	if allocated != nil {
		t.Fatalf("allocateNodeSubnets() expected no allocated subnets, got %v", allocated)
	}

	v4usedAfter, v6usedAfter := na.clusterSubnetAllocator.Usage()
	if v4usedAfter != v4usedBefore {
		t.Fatalf("Expected %d v4 allocated subnets, but got %d", v4usedBefore, v4usedAfter)
	}
	if v6usedAfter != v6usedBefore {
		t.Fatalf("Expected %d v6 allocated subnets, but got %d", v6usedBefore, v6usedAfter)
	}
}

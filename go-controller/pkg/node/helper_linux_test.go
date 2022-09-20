package node

import (
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestFilterRoutesByIfIndex(t *testing.T) {
	tcs := []struct {
		routesUnfiltered []netlink.Route
		gwIfIdx          int
		routesFiltered   []netlink.Route
	}{
		// tc0 - baseline.
		{
			routesUnfiltered: []netlink.Route{},
			gwIfIdx:          0,
			routesFiltered:   []netlink.Route{},
		},
		// tc1 - do not filter.
		{
			routesUnfiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 2,
					Gw:        []byte{2, 2, 2, 2},
				},
				{
					LinkIndex: 1,
					Gw:        []byte{3, 3, 3, 3},
				},
			},
			gwIfIdx: 0,
			routesFiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 2,
					Gw:        []byte{2, 2, 2, 2},
				},
				{
					LinkIndex: 1,
					Gw:        []byte{3, 3, 3, 3},
				},
			},
		},
		// tc2 - filter by link index.
		{
			routesUnfiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 2,
					Gw:        []byte{2, 2, 2, 2},
				},
				{
					LinkIndex: 1,
					Gw:        []byte{3, 3, 3, 3},
				},
			},
			gwIfIdx: 1,
			routesFiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 1,
					Gw:        []byte{3, 3, 3, 3},
				},
			},
		},
		// tc3 - filter by MultiPath index.
		{
			routesUnfiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 2,
					Gw:        []byte{2, 2, 2, 2},
				},
				{
					LinkIndex: 0,
					MultiPath: []*netlink.NexthopInfo{
						{
							LinkIndex: 1,
							Hops:      0,
							Gw:        []byte{3, 3, 3, 3},
						},
						{
							LinkIndex: 1,
							Hops:      0,
							Gw:        []byte{4, 4, 4, 4},
						},
					},
				},
			},
			gwIfIdx: 1,
			routesFiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 0,
					MultiPath: []*netlink.NexthopInfo{
						{
							LinkIndex: 1,
							Hops:      0,
							Gw:        []byte{3, 3, 3, 3},
						},
						{
							LinkIndex: 1,
							Hops:      0,
							Gw:        []byte{4, 4, 4, 4},
						},
					},
				},
			},
		},
		// tc4 - filter by MultiPath index - MultiPath with multiple interfaces.
		{
			routesUnfiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 2,
					Gw:        []byte{2, 2, 2, 2},
				},
				{
					LinkIndex: 0,
					MultiPath: []*netlink.NexthopInfo{
						{
							LinkIndex: 1,
							Hops:      0,
							Gw:        []byte{3, 3, 3, 3},
						},
						{
							LinkIndex: 2,
							Hops:      0,
							Gw:        []byte{4, 4, 4, 4},
						},
					},
				},
			},
			gwIfIdx: 1,
			routesFiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
			},
		},
	}

	for i, tc := range tcs {
		routesFiltered := filterRoutesByIfIndex(tc.routesUnfiltered, tc.gwIfIdx)
		if !reflect.DeepEqual(tc.routesFiltered, routesFiltered) {
			t.Fatalf("TestFilterRoutesByIfIndex(%d): Filtering '%v' by index '%d' should have yielded '%v' but got '%v'",
				i, tc.routesUnfiltered, tc.gwIfIdx, tc.routesFiltered, routesFiltered)
		}
	}
}

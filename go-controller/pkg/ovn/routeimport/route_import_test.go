// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeimport

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntesting "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
)

type routeStream struct {
	util.NetLinkOps
	routes []netlink.Route
	err    error
}

func (s routeStream) RouteListFilteredIter(family int, filter *netlink.Route, mask uint64, visit func(netlink.Route) bool) error {
	if family != netlink.FAMILY_ALL || filter.Table != 1000 || filter.Protocol != unix.RTPROT_BGP || mask != netlink.RT_FILTER_TABLE|netlink.RT_FILTER_PROTOCOL {
		return errors.New("unexpected route filter")
	}
	for _, route := range s.routes {
		if !visit(route) {
			break
		}
	}
	return s.err
}

func TestBGPRoutesDiscardsInterruptedDump(t *testing.T) {
	_, dst, _ := net.ParseCIDR("192.0.2.0/24")
	for _, dumpErr := range []error{netlink.ErrDumpInterrupted, errors.New("receive failed")} {
		c := &controller{log: logr.Discard(), netlink: routeStream{
			routes: []netlink.Route{{Dst: dst, Gw: net.ParseIP("192.0.2.1")}}, err: dumpErr,
		}}
		got, err := c.getBGPRoutes(1000, nil, 0)
		if !errors.Is(err, dumpErr) || got != nil {
			t.Fatalf("partial dump must not become desired state: routes=%v err=%v", got, err)
		}
	}
}

func TestBGPRoutesStreamsBothFamilies(t *testing.T) {
	_, v4, _ := net.ParseCIDR("192.0.2.0/24")
	_, v6, _ := net.ParseCIDR("2001:db8::/64")
	c := &controller{log: logr.Discard(), netlink: routeStream{routes: []netlink.Route{
		{Dst: v4, Gw: net.ParseIP("192.0.2.1"), LinkIndex: 2},
		{Dst: v6, MultiPath: []*netlink.NexthopInfo{
			{Gw: net.ParseIP("fe80::1"), LinkIndex: 2},
			{Gw: net.ParseIP("fe80::2"), LinkIndex: 3},
		}},
	}}}
	got, err := c.getBGPRoutes(1000, nil, 2)
	if err != nil || len(got) != 2 || !got.Has(route{dst: v4.String(), gw: "192.0.2.1"}) || !got.Has(route{dst: v6.String(), gw: "fe80::1"}) {
		t.Fatalf("unexpected dual-stack/multipath routes: %v, %v", got, err)
	}
	got, err = c.getBGPRoutes(1000, []*net.IPNet{v4}, 2)
	if err != nil || len(got) != 1 || !got.Has(route{dst: v6.String(), gw: "fe80::1"}) {
		t.Fatalf("unexpected subnet-filtered routes: %v, %v", got, err)
	}
}

func BenchmarkBGPRoutesStreaming(b *testing.B) {
	routes := make([]netlink.Route, 10000)
	for i := range routes {
		routes[i] = netlink.Route{
			Dst: &net.IPNet{IP: net.IPv4(10, byte(i>>8), byte(i), 0), Mask: net.CIDRMask(24, 32)},
			Gw:  net.ParseIP("192.0.2.1"),
		}
	}
	c := &controller{log: logr.Discard(), netlink: routeStream{routes: routes}}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		got, err := c.getBGPRoutes(1000, nil, 0)
		if err != nil || len(got) != len(routes) {
			b.Fatalf("routes=%d error=%v", len(got), err)
		}
	}
}

func Test_controller_syncNetwork(t *testing.T) {
	node := "testnode"

	// Capture original global config values and restore after test
	origClusterSubnets := config.Default.ClusterSubnets
	t.Cleanup(func() {
		config.Default.ClusterSubnets = origClusterSubnets
	})

	defaultNetwork := &util.DefaultNetInfo{}
	defaultNetworkRouter := defaultNetwork.GetNetworkScopedGWRouterName(node)
	defaultNetworkRouterPort := types.GWRouterToExtSwitchPrefix + defaultNetworkRouter

	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{
			CIDR: &net.IPNet{
				IP:   net.IPv4(10, 128, 0, 0),
				Mask: net.CIDRMask(16, 32),
			},
			HostSubnetLength: 24,
		},
	}

	udn := &multinetworkmocks.NetInfo{}
	udn.On("IsDefault").Return(false)
	udn.On("GetNetworkName").Return("udn")
	udn.On("GetNetworkID").Return(1)
	udn.On("Subnets").Return(nil)
	udn.On("GetNetworkScopedGWRouterName", node).Return("router")
	udn.On("Transport").Return("")
	udn.On("Uplink").Return("")

	cudn := &multinetworkmocks.NetInfo{}
	cudn.On("IsDefault").Return(false)
	cudn.On("GetNetworkName").Return(types.CUDNPrefix + "cudn")
	cudn.On("GetNetworkID").Return(2)
	cudn.On("Subnets").Return(nil)
	cudn.On("GetNetworkScopedGWRouterName", node).Return("router")
	cudn.On("Transport").Return("")
	cudn.On("Uplink").Return("")

	// Create CUDN with subnets for overlay mode testing
	cudnOverlay := &multinetworkmocks.NetInfo{}
	cudnOverlay.On("IsDefault").Return(false)
	cudnOverlay.On("GetNetworkName").Return(types.CUDNPrefix + "cudn-overlay")
	cudnOverlay.On("GetNetworkID").Return(3)
	cudnOverlay.On("Subnets").Return([]config.CIDRNetworkEntry{
		{
			CIDR: &net.IPNet{
				IP:   net.IPv4(192, 168, 0, 0),
				Mask: net.CIDRMask(16, 32),
			},
			HostSubnetLength: 24,
		},
	})
	cudnOverlay.On("GetNetworkScopedGWRouterName", node).Return("cudn-overlay-router")
	cudnOverlay.On("Transport").Return("") // Empty means overlay (geneve)
	cudnOverlay.On("Uplink").Return("")
	cudnOverlayRouter := cudnOverlay.GetNetworkScopedGWRouterName(node)
	cudnOverlayRouterPort := types.GWRouterToExtSwitchPrefix + cudnOverlayRouter

	// Create CUDN with subnets for no-overlay mode testing
	cudnNoOverlay := &multinetworkmocks.NetInfo{}
	cudnNoOverlay.On("IsDefault").Return(false)
	cudnNoOverlay.On("GetNetworkName").Return(types.CUDNPrefix + "cudn-nooverlay")
	cudnNoOverlay.On("GetNetworkID").Return(4)
	cudnNoOverlay.On("Subnets").Return([]config.CIDRNetworkEntry{
		{
			CIDR: &net.IPNet{
				IP:   net.IPv4(192, 168, 0, 0),
				Mask: net.CIDRMask(16, 32),
			},
			HostSubnetLength: 24,
		},
	})
	cudnNoOverlay.On("GetNetworkScopedGWRouterName", node).Return("cudn-nooverlay-router")
	cudnNoOverlay.On("Transport").Return(types.NetworkTransportNoOverlay)
	cudnNoOverlay.On("Uplink").Return("")
	cudnNoOverlayRouter := cudnNoOverlay.GetNetworkScopedGWRouterName(node)
	cudnNoOverlayRouterPort := types.GWRouterToExtSwitchPrefix + cudnNoOverlayRouter

	primaryUDNMultiSubnet := &multinetworkmocks.NetInfo{}
	primaryUDNMultiSubnet.On("IsDefault").Return(false)
	primaryUDNMultiSubnet.On("GetNetworkName").Return("primary-udn")
	primaryUDNMultiSubnet.On("GetNetworkID").Return(5)
	primaryUDNMultiSubnet.On("Subnets").Return([]config.CIDRNetworkEntry{
		{
			CIDR: &net.IPNet{
				IP:   net.IPv4(192, 168, 0, 0),
				Mask: net.CIDRMask(16, 32),
			},
			HostSubnetLength: 24,
		},
		{
			CIDR: &net.IPNet{
				IP:   net.IPv4(192, 169, 0, 0),
				Mask: net.CIDRMask(16, 32),
			},
			HostSubnetLength: 24,
		},
	})
	primaryUDNMultiSubnet.On("GetNetworkScopedGWRouterName", node).Return("primary-udn-router")
	primaryUDNMultiSubnet.On("Transport").Return("")
	primaryUDNMultiSubnet.On("Uplink").Return("")
	primaryUDNMultiSubnetRouter := primaryUDNMultiSubnet.GetNetworkScopedGWRouterName(node)
	primaryUDNMultiSubnetRouterPort := types.GWRouterToExtSwitchPrefix + primaryUDNMultiSubnetRouter

	primaryUDNMultiSubnetNoOverlay := &multinetworkmocks.NetInfo{}
	primaryUDNMultiSubnetNoOverlay.On("IsDefault").Return(false)
	primaryUDNMultiSubnetNoOverlay.On("GetNetworkName").Return("primary-udn-nooverlay")
	primaryUDNMultiSubnetNoOverlay.On("GetNetworkID").Return(6)
	primaryUDNMultiSubnetNoOverlay.On("Subnets").Return([]config.CIDRNetworkEntry{
		{
			CIDR: &net.IPNet{
				IP:   net.IPv4(192, 168, 0, 0),
				Mask: net.CIDRMask(16, 32),
			},
			HostSubnetLength: 24,
		},
		{
			CIDR: &net.IPNet{
				IP:   net.IPv4(192, 169, 0, 0),
				Mask: net.CIDRMask(16, 32),
			},
			HostSubnetLength: 24,
		},
	})
	primaryUDNMultiSubnetNoOverlay.On("GetNetworkScopedGWRouterName", node).Return("primary-udn-nooverlay-router")
	primaryUDNMultiSubnetNoOverlay.On("Transport").Return(types.NetworkTransportNoOverlay)
	primaryUDNMultiSubnetNoOverlay.On("Uplink").Return("")
	primaryUDNMultiSubnetNoOverlayRouter := primaryUDNMultiSubnetNoOverlay.GetNetworkScopedGWRouterName(node)
	primaryUDNMultiSubnetNoOverlayRouterPort := types.GWRouterToExtSwitchPrefix + primaryUDNMultiSubnetNoOverlayRouter

	uplinkCUDN := &multinetworkmocks.NetInfo{}
	uplinkCUDN.On("IsDefault").Return(false)
	uplinkCUDN.On("GetNetworkName").Return(types.CUDNPrefix + "blue")
	uplinkCUDN.On("GetNetworkID").Return(7)
	uplinkCUDN.On("Subnets").Return(nil)
	uplinkCUDN.On("GetNetworkScopedGWRouterName", node).Return("blue-router")
	uplinkCUDN.On("Transport").Return("")
	uplinkCUDN.On("Uplink").Return("uplink1")
	uplinkCUDNRouter := uplinkCUDN.GetNetworkScopedGWRouterName(node)
	uplinkCUDNRouterPort := types.GWRouterToExtSwitchPrefix + uplinkCUDNRouter

	type fields struct {
		networkIDs map[int]string
		networks   map[string]util.NetInfo
	}
	type args struct {
		network string
	}
	tests := []struct {
		name              string
		fields            fields
		args              args
		initial           []libovsdb.TestData
		expected          []libovsdb.TestData
		routes            []netlink.Route
		link              netlink.Link
		uplinkStateLister uplinklisters.UplinkStateLister
		uplinkBridgeLink  netlink.Link
		noOverlayEnabled  bool
		linkErr           bool
		routesErr         bool
		wantErr           bool
	}{
		{
			name: "ignored if network not known",
			args: args{"default"},
		},
		{
			name: "ignored if vrf not known",
			args: args{"udn"},
			fields: fields{
				networkIDs: map[int]string{1: "udn"},
				networks:   map[string]util.NetInfo{"udn": udn},
			},
		},
		{
			name: "fails if vrf link cannot be fetched",
			args: args{"udn"},
			fields: fields{
				networkIDs: map[int]string{1: "udn"},
				networks:   map[string]util.NetInfo{"udn": udn},
			},
			linkErr: true,
			wantErr: true,
		},
		{
			name: "fails if kernel routes cannot be fetched",
			args: args{"default"},
			fields: fields{
				networkIDs: map[int]string{0: "default"},
				networks:   map[string]util.NetInfo{"default": defaultNetwork},
			},
			routesErr: true,
			wantErr:   true,
		},
		{
			name: "fails if OVN routes cannot be fetched (i.e. router does not exist)",
			args: args{"default"},
			link: &netlink.Vrf{Table: unix.RT_TABLE_MAIN},
			fields: fields{
				networkIDs: map[int]string{0: "default"},
				networks:   map[string]util.NetInfo{"default": defaultNetwork},
			},
			wantErr: true,
		},
		{
			name: "imports routes for a UDN",
			args: args{"udn"},
			link: &netlink.Vrf{Table: 1000},
			fields: fields{
				networkIDs: map[int]string{1: "udn"},
				networks:   map[string]util.NetInfo{"udn": udn},
			},
			initial: []libovsdb.TestData{
				&nbdb.LogicalRouter{Name: "router"},
			},
			expected: []libovsdb.TestData{
				&nbdb.LogicalRouter{UUID: "router", Name: "router"},
			},
		},
		{
			name: "imports routes for a CUDN",
			args: args{"cudn"},
			link: &netlink.Vrf{Table: 10001},
			fields: fields{
				networkIDs: map[int]string{1: "cudn"},
				networks:   map[string]util.NetInfo{"cudn": cudn},
			},
			initial: []libovsdb.TestData{
				&nbdb.LogicalRouter{Name: "router"},
			},
			expected: []libovsdb.TestData{
				&nbdb.LogicalRouter{UUID: "router", Name: "router"},
			},
		},
		{
			name: "adds and removes routes as necessary",
			args: args{"default"},
			fields: fields{
				networkIDs: map[int]string{0: "default"},
				networks:   map[string]util.NetInfo{"default": defaultNetwork},
			},
			link: &netlink.Vrf{Table: unix.RT_TABLE_MAIN},
			initial: []libovsdb.TestData{
				&nbdb.LogicalRouter{Name: defaultNetworkRouter, StaticRoutes: []string{"keep-1", "keep-2", "remove"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &defaultNetworkRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-2", IPPrefix: "5.5.5.0/24", Nexthop: "5.5.5.1"},
				&nbdb.LogicalRouterStaticRoute{UUID: "remove", IPPrefix: "6.6.6.0/24", Nexthop: "6.6.6.1", OutputPort: &defaultNetworkRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
				&nbdb.LogicalRouter{UUID: "otherRouter", Name: "otherRouter", StaticRoutes: []string{"untouched-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "untouched-1", IPPrefix: "3.3.3.0/24", Nexthop: "3.3.3.2", ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
			routes: []netlink.Route{
				{Dst: ovntesting.MustParseIPNet("1.1.1.0/24"), Gw: ovntesting.MustParseIP("1.1.1.1")},
				{Dst: ovntesting.MustParseIPNet("2.2.2.0/24"), Gw: ovntesting.MustParseIP("2.2.2.1")},
				{Dst: ovntesting.MustParseIPNet("3.3.3.0/24"), MultiPath: []*netlink.NexthopInfo{{Gw: ovntesting.MustParseIP("3.3.3.1")}, {Gw: ovntesting.MustParseIP("3.3.3.2")}}},
			},
			expected: []libovsdb.TestData{
				&nbdb.LogicalRouter{UUID: "router", Name: defaultNetworkRouter, StaticRoutes: []string{"keep-1", "keep-2", "add-1", "add-2", "add-3"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &defaultNetworkRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-2", IPPrefix: "5.5.5.0/24", Nexthop: "5.5.5.1"},
				&nbdb.LogicalRouterStaticRoute{UUID: "add-1", IPPrefix: "2.2.2.0/24", Nexthop: "2.2.2.1", OutputPort: &defaultNetworkRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
				&nbdb.LogicalRouterStaticRoute{UUID: "add-2", IPPrefix: "3.3.3.0/24", Nexthop: "3.3.3.1", OutputPort: &defaultNetworkRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
				&nbdb.LogicalRouterStaticRoute{UUID: "add-3", IPPrefix: "3.3.3.0/24", Nexthop: "3.3.3.2", OutputPort: &defaultNetworkRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
				&nbdb.LogicalRouter{UUID: "otherRouter", Name: "otherRouter", StaticRoutes: []string{"untouched-1"}},
				// this route should not be updated as it belongs to a different network
				&nbdb.LogicalRouterStaticRoute{UUID: "untouched-1", IPPrefix: "3.3.3.0/24", Nexthop: "3.3.3.2", ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
		},
		{
			name: "ignores host subnet routes as necessary in overlay mode",
			args: args{"default"},
			fields: fields{
				networkIDs: map[int]string{0: "default"},
				networks:   map[string]util.NetInfo{"default": defaultNetwork},
			},
			link: &netlink.Vrf{Table: unix.RT_TABLE_MAIN},
			initial: []libovsdb.TestData{
				&nbdb.LogicalRouter{Name: defaultNetwork.GetNetworkScopedGWRouterName(node), StaticRoutes: []string{"keep-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &defaultNetworkRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
			routes: []netlink.Route{
				{Dst: ovntesting.MustParseIPNet("1.1.1.0/24"), Gw: ovntesting.MustParseIP("1.1.1.1")},
				{Dst: ovntesting.MustParseIPNet("10.128.1.0/24"), Gw: ovntesting.MustParseIP("2.2.2.1")},
			},
			expected: []libovsdb.TestData{
				&nbdb.LogicalRouter{UUID: "router", Name: defaultNetwork.GetNetworkScopedGWRouterName(node), StaticRoutes: []string{"keep-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &defaultNetworkRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
		},
		{
			name:             "adds host subnet routes as necessary in no-overlay mode",
			noOverlayEnabled: true,
			args:             args{"default"},
			fields: fields{
				networkIDs: map[int]string{0: "default"},
				networks:   map[string]util.NetInfo{"default": defaultNetwork},
			},
			link: &netlink.Vrf{Table: unix.RT_TABLE_MAIN},
			initial: []libovsdb.TestData{
				&nbdb.LogicalRouter{Name: defaultNetwork.GetNetworkScopedGWRouterName(node), StaticRoutes: []string{"keep-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &defaultNetworkRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
			routes: []netlink.Route{
				{Dst: ovntesting.MustParseIPNet("1.1.1.0/24"), Gw: ovntesting.MustParseIP("1.1.1.1")},
				{Dst: ovntesting.MustParseIPNet("10.128.1.0/24"), Gw: ovntesting.MustParseIP("2.2.2.1")},
			},
			expected: []libovsdb.TestData{
				&nbdb.LogicalRouter{UUID: "router", Name: defaultNetwork.GetNetworkScopedGWRouterName(node), StaticRoutes: []string{"keep-1", "add-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &defaultNetworkRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
				&nbdb.LogicalRouterStaticRoute{UUID: "add-1", IPPrefix: "10.128.1.0/24", Nexthop: "2.2.2.1", OutputPort: &defaultNetworkRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
		},
		{
			name: "ignores CUDN pod subnet routes in overlay mode",
			args: args{types.CUDNPrefix + "cudn-overlay"},
			fields: fields{
				networkIDs: map[int]string{3: types.CUDNPrefix + "cudn-overlay"},
				networks:   map[string]util.NetInfo{types.CUDNPrefix + "cudn-overlay": cudnOverlay},
			},
			link: &netlink.Vrf{Table: 3},
			initial: []libovsdb.TestData{
				&nbdb.LogicalRouter{Name: cudnOverlayRouter, StaticRoutes: []string{"keep-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &cudnOverlayRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
			routes: []netlink.Route{
				{Dst: ovntesting.MustParseIPNet("1.1.1.0/24"), Gw: ovntesting.MustParseIP("1.1.1.1")},
				{Dst: ovntesting.MustParseIPNet("192.168.1.0/24"), Gw: ovntesting.MustParseIP("2.2.2.1")},
			},
			expected: []libovsdb.TestData{
				&nbdb.LogicalRouter{UUID: "router", Name: cudnOverlayRouter, StaticRoutes: []string{"keep-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &cudnOverlayRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
		},
		{
			name: "ignores primary UDN pod subnet routes from additional subnet in overlay mode",
			args: args{"primary-udn"},
			fields: fields{
				networkIDs: map[int]string{5: "primary-udn"},
				networks:   map[string]util.NetInfo{"primary-udn": primaryUDNMultiSubnet},
			},
			link: &netlink.Vrf{Table: 5},
			initial: []libovsdb.TestData{
				&nbdb.LogicalRouter{Name: primaryUDNMultiSubnetRouter, StaticRoutes: []string{"keep-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &primaryUDNMultiSubnetRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
			routes: []netlink.Route{
				{Dst: ovntesting.MustParseIPNet("1.1.1.0/24"), Gw: ovntesting.MustParseIP("1.1.1.1")},
				{Dst: ovntesting.MustParseIPNet("192.169.1.0/24"), Gw: ovntesting.MustParseIP("2.2.2.1")},
			},
			expected: []libovsdb.TestData{
				&nbdb.LogicalRouter{UUID: "router", Name: primaryUDNMultiSubnetRouter, StaticRoutes: []string{"keep-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &primaryUDNMultiSubnetRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
		},
		{
			name: "adds primary UDN pod subnet routes from additional subnet in no-overlay mode",
			args: args{"primary-udn-nooverlay"},
			fields: fields{
				networkIDs: map[int]string{6: "primary-udn-nooverlay"},
				networks:   map[string]util.NetInfo{"primary-udn-nooverlay": primaryUDNMultiSubnetNoOverlay},
			},
			link: &netlink.Vrf{Table: 6},
			initial: []libovsdb.TestData{
				&nbdb.LogicalRouter{Name: primaryUDNMultiSubnetNoOverlayRouter, StaticRoutes: []string{"keep-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &primaryUDNMultiSubnetNoOverlayRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
			routes: []netlink.Route{
				{Dst: ovntesting.MustParseIPNet("1.1.1.0/24"), Gw: ovntesting.MustParseIP("1.1.1.1")},
				{Dst: ovntesting.MustParseIPNet("192.169.1.0/24"), Gw: ovntesting.MustParseIP("2.2.2.1")},
			},
			expected: []libovsdb.TestData{
				&nbdb.LogicalRouter{UUID: "router", Name: primaryUDNMultiSubnetNoOverlayRouter, StaticRoutes: []string{"keep-1", "add-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &primaryUDNMultiSubnetNoOverlayRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
				&nbdb.LogicalRouterStaticRoute{UUID: "add-1", IPPrefix: "192.169.1.0/24", Nexthop: "2.2.2.1", OutputPort: &primaryUDNMultiSubnetNoOverlayRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
		},
		{
			name: "adds CUDN pod subnet routes in no-overlay mode",
			args: args{types.CUDNPrefix + "cudn-nooverlay"},
			fields: fields{
				networkIDs: map[int]string{4: types.CUDNPrefix + "cudn-nooverlay"},
				networks:   map[string]util.NetInfo{types.CUDNPrefix + "cudn-nooverlay": cudnNoOverlay},
			},
			link: &netlink.Vrf{Table: 4},
			initial: []libovsdb.TestData{
				&nbdb.LogicalRouter{Name: cudnNoOverlayRouter, StaticRoutes: []string{"keep-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &cudnNoOverlayRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
			routes: []netlink.Route{
				{Dst: ovntesting.MustParseIPNet("1.1.1.0/24"), Gw: ovntesting.MustParseIP("1.1.1.1")},
				{Dst: ovntesting.MustParseIPNet("192.168.1.0/24"), Gw: ovntesting.MustParseIP("2.2.2.1")},
			},
			expected: []libovsdb.TestData{
				&nbdb.LogicalRouter{UUID: "router", Name: cudnNoOverlayRouter, StaticRoutes: []string{"keep-1", "add-1"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", OutputPort: &cudnNoOverlayRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
				&nbdb.LogicalRouterStaticRoute{UUID: "add-1", IPPrefix: "192.168.1.0/24", Nexthop: "2.2.2.1", OutputPort: &cudnNoOverlayRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
		},
		{
			name: "filters routes for an Uplink-backed CUDN by Uplink bridge",
			args: args{types.CUDNPrefix + "blue"},
			fields: fields{
				networkIDs: map[int]string{7: types.CUDNPrefix + "blue"},
				networks:   map[string]util.NetInfo{types.CUDNPrefix + "blue": uplinkCUDN},
			},
			link: &netlink.Vrf{Table: 7},
			uplinkStateLister: newRouteImportUplinkStateLister(
				t,
				"uplink1",
				node,
				"ovsbr1",
			),
			uplinkBridgeLink: &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "ovsbr1",
					Index: 10,
				},
			},
			initial: []libovsdb.TestData{
				&nbdb.LogicalRouter{Name: uplinkCUDNRouter},
			},
			routes: []netlink.Route{
				{
					Dst: ovntesting.MustParseIPNet("172.26.0.0/16"),
					MultiPath: []*netlink.NexthopInfo{
						{LinkIndex: 2, Gw: ovntesting.MustParseIP("172.18.0.5")},
						{LinkIndex: 10, Gw: ovntesting.MustParseIP("172.28.0.5")},
					},
				},
				{
					LinkIndex: 2,
					Dst:       ovntesting.MustParseIPNet("172.27.0.0/16"),
					Gw:        ovntesting.MustParseIP("172.18.0.5"),
				},
				{
					LinkIndex: 10,
					Dst:       ovntesting.MustParseIPNet("172.29.0.0/16"),
					Gw:        ovntesting.MustParseIP("172.28.0.5"),
				},
			},
			expected: []libovsdb.TestData{
				&nbdb.LogicalRouter{UUID: "router", Name: uplinkCUDNRouter, StaticRoutes: []string{"add-1", "add-2"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "add-1", IPPrefix: "172.26.0.0/16", Nexthop: "172.28.0.5", OutputPort: &uplinkCUDNRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
				&nbdb.LogicalRouterStaticRoute{UUID: "add-2", IPPrefix: "172.29.0.0/16", Nexthop: "172.28.0.5", OutputPort: &uplinkCUDNRouterPort, ExternalIDs: map[string]string{controllerExternalIDKey: controllerName}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			// Capture and restore global config value for this subtest
			origTransport := config.Default.Transport
			t.Cleanup(func() {
				config.Default.Transport = origTransport
			})

			testError := errors.New("test forced error or incorrect test arguments")
			network := tt.fields.networks[tt.args.network]

			nlmock := &mocks.NetLinkOps{}
			nlmock.On("IsLinkNotFoundError", mock.Anything).Return(tt.link == nil && !tt.linkErr)
			switch {
			case network == nil || tt.linkErr:
				nlmock.On("LinkByName", mock.Anything).Return(nil, testError)
			default:
				nlmock.On("LinkByName", util.GetNetworkVRFName(network)).Return(tt.link, nil)
				if tt.uplinkBridgeLink != nil {
					nlmock.On("LinkByName", tt.uplinkBridgeLink.Attrs().Name).Return(tt.uplinkBridgeLink, nil)
				}
			}

			switch {
			case tt.link == nil || tt.link.Type() != "vrf" || tt.routesErr:
				nlmock.On("RouteListFilteredIter", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(testError)
			default:
				vrf := tt.link.(*netlink.Vrf)
				matchFilter := func(r *netlink.Route) bool {
					return r != nil && r.Equal(netlink.Route{Protocol: unix.RTPROT_BGP, Table: int(vrf.Table)})
				}
				nlmock.On("RouteListFilteredIter", netlink.FAMILY_ALL, mock.MatchedBy(matchFilter), netlink.RT_FILTER_PROTOCOL|netlink.RT_FILTER_TABLE, mock.Anything).
					Run(func(args mock.Arguments) {
						for _, route := range tt.routes {
							if !args.Get(3).(func(netlink.Route) bool)(route) {
								break
							}
						}
					}).Return(nil)
			}

			client, ctx, err := libovsdb.NewNBTestHarness(libovsdb.TestSetup{NBData: tt.initial}, nil)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			t.Cleanup(ctx.Cleanup)

			c := &controller{
				nbClient:          client,
				node:              node,
				log:               testr.New(t),
				networkIDs:        tt.fields.networkIDs,
				networks:          tt.fields.networks,
				tables:            map[int]int{},
				netlink:           nlmock,
				uplinkStateLister: tt.uplinkStateLister,
			}

			if tt.noOverlayEnabled {
				config.Default.Transport = types.NetworkTransportNoOverlay
			}

			err = c.syncNetwork(context.Background(), tt.args.network)
			if tt.wantErr {
				g.Expect(err).To(gomega.HaveOccurred())
				return
			}

			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(client).To(libovsdb.HaveData(tt.expected...))
		})
	}
}

func newRouteImportUplinkStateLister(
	t *testing.T,
	uplinkName, nodeName, bridgeName string,
) uplinklisters.UplinkStateLister {
	t.Helper()

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	err := indexer.Add(&uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{
			Name: uplinkutil.StateName(uplinkName, nodeName),
		},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: uplinkName,
			NodeName:   nodeName,
		},
		Status: uplinkv1alpha1.UplinkStateStatus{
			Type: uplinkv1alpha1.UplinkTypeOVSBridge,
			OVSBridge: &uplinkv1alpha1.OVSBridgeStatus{
				Name: bridgeName,
			},
			Conditions: []metav1.Condition{
				{
					Type:   uplinkv1alpha1.UplinkStateConditionResolved,
					Status: metav1.ConditionTrue,
				},
			},
		},
	})
	gomega.NewWithT(t).Expect(err).ToNot(gomega.HaveOccurred())

	return uplinklisters.NewUplinkStateLister(indexer)
}

func Test_controller_syncRouteUpdate(t *testing.T) {
	defaultNetwork := &util.DefaultNetInfo{}
	type fields struct {
		networkIDs map[int]string
		networks   map[string]util.NetInfo
		tables     map[int]int
	}
	type args struct {
		update *netlink.RouteUpdate
	}
	tests := []struct {
		name     string
		fields   fields
		args     args
		expected []string
	}{
		{
			name: "ignores route updates with protocol != BGP",
			args: args{&netlink.RouteUpdate{Route: netlink.Route{Protocol: unix.RTPROT_STATIC}}},
		},
		{
			name: "ignores route updates for unknown tables",
			args: args{&netlink.RouteUpdate{Route: netlink.Route{Protocol: unix.RTPROT_BGP, Table: unix.RT_TABLE_UNSPEC}}},
		},
		{
			name:   "ignores route updates for unknown networks",
			fields: fields{tables: map[int]int{unix.RT_TABLE_MAIN: 0}},
			args:   args{&netlink.RouteUpdate{Route: netlink.Route{Protocol: unix.RTPROT_BGP, Table: unix.RT_TABLE_MAIN}}},
		},
		{
			name: "processes route updates",
			fields: fields{
				networkIDs: map[int]string{0: "default"},
				networks:   map[string]util.NetInfo{"default": defaultNetwork},
				tables:     map[int]int{unix.RT_TABLE_MAIN: 0},
			},
			args:     args{&netlink.RouteUpdate{Route: netlink.Route{Protocol: unix.RTPROT_BGP, Table: unix.RT_TABLE_MAIN}}},
			expected: []string{"default"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			var reconciled []string
			var m sync.Mutex
			reconcile := func(_ context.Context, key string) error {
				m.Lock()
				defer m.Unlock()
				reconciled = append(reconciled, key)
				return nil
			}
			matchReconcile := func(g gomega.Gomega, expected []string) {
				m.Lock()
				defer m.Unlock()
				g.Expect(reconciled).To(gomega.Equal(expected))
			}
			r := controllerutil.NewReconciler(
				"test",
				&controllerutil.ReconcilerConfig{Reconcile: reconcile, Threadiness: 1, RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](0, 0)})
			c := &controller{
				log:        testr.New(t),
				networkIDs: tt.fields.networkIDs,
				networks:   tt.fields.networks,
				tables:     tt.fields.tables,
				reconciler: r,
			}
			err := controllerutil.Start(r)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			c.syncRouteUpdate(tt.args.update)

			g.Eventually(matchReconcile).WithArguments(tt.expected).Should(gomega.Succeed())
			g.Consistently(matchReconcile).WithArguments(tt.expected).Should(gomega.Succeed())
		})
	}
}

func Test_controller_syncLinkUpdate(t *testing.T) {
	udn := &multinetworkmocks.NetInfo{}
	type fields struct {
		networkIDs map[int]string
		networks   map[string]util.NetInfo
		tables     map[int]int
	}
	type args struct {
		update *netlink.LinkUpdate
	}
	tests := []struct {
		name             string
		fields           fields
		args             args
		expectTables     map[int]int
		expectReconciles []string
	}{
		{
			name: "ignores link updates with type != VRF",
			args: args{&netlink.LinkUpdate{Link: &netlink.Dummy{}}},
		},
		{
			name: "ignores link updates with incorrect prefix",
			args: args{&netlink.LinkUpdate{Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "something10" + types.UDNVRFDeviceSuffix}}}},
		},
		{
			name: "ignores link updates with incorrect suffix",
			args: args{&netlink.LinkUpdate{Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "10-something"}}}},
		},
		{
			name: "ignores link updates with incorrect format",
			args: args{&netlink.LinkUpdate{Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "something" + types.UDNVRFDeviceSuffix}}}},
		},
		{
			name: "ignores link updates of unknown UDN networks",
			args: args{&netlink.LinkUpdate{Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "10" + types.UDNVRFDeviceSuffix}}}},
		},
		{
			name: "ignores link updates of unknown CUDN networks",
			args: args{&netlink.LinkUpdate{Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "cudn"}}}},
		},
		{
			name: "ignores link delete event types",
			fields: fields{
				networkIDs: map[int]string{1: "udn"},
				networks:   map[string]util.NetInfo{"udn": udn},
				tables:     map[int]int{1000: 1},
			},
			args: args{&netlink.LinkUpdate{
				Header: unix.NlMsghdr{Type: unix.RTM_DELLINK},
				Link:   &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "1" + types.UDNVRFDeviceSuffix}, Table: 1000}},
			},
			expectTables: map[int]int{1000: 1},
		},
		{
			name: "does not reconcile on link updates with no actual changes",
			fields: fields{
				networkIDs: map[int]string{1: "udn"},
				networks:   map[string]util.NetInfo{"udn": udn},
				tables:     map[int]int{1000: 1},
			},
			args: args{&netlink.LinkUpdate{
				Header: unix.NlMsghdr{Type: unix.RTM_NEWLINK},
				Link:   &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "1" + types.UDNVRFDeviceSuffix}, Table: 1000}},
			},
			expectTables: map[int]int{1000: 1},
		},
		{
			name: "does reconcile on link updates with actual changes for generated VRF names",
			fields: fields{
				networkIDs: map[int]string{1: "udn"},
				networks:   map[string]util.NetInfo{"udn": udn},
				tables:     map[int]int{1000: 1},
			},
			args: args{&netlink.LinkUpdate{
				Header: unix.NlMsghdr{Type: unix.RTM_NEWLINK},
				Link:   &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "1" + types.UDNVRFDeviceSuffix}, Table: 1001}},
			},
			expectTables:     map[int]int{1001: 1},
			expectReconciles: []string{"udn"},
		},
		{
			name: "does reconcile on link updates with actual changes for network VRF names",
			fields: fields{
				networkIDs: map[int]string{1: types.CUDNPrefix + "udn"},
				networks:   map[string]util.NetInfo{types.CUDNPrefix + "udn": udn},
				tables:     map[int]int{1000: 1},
			},
			args: args{&netlink.LinkUpdate{
				Header: unix.NlMsghdr{Type: unix.RTM_NEWLINK},
				Link:   &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "udn"}, Table: 1001}},
			},
			expectTables:     map[int]int{1001: 1},
			expectReconciles: []string{types.CUDNPrefix + "udn"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			var reconciled []string
			var m sync.Mutex
			reconcile := func(_ context.Context, key string) error {
				m.Lock()
				defer m.Unlock()
				reconciled = append(reconciled, key)
				return nil
			}
			matchReconcile := func(g gomega.Gomega, expected []string) {
				m.Lock()
				defer m.Unlock()
				g.Expect(reconciled).To(gomega.Equal(expected))
			}
			r := controllerutil.NewReconciler(
				"test",
				&controllerutil.ReconcilerConfig{Reconcile: reconcile, Threadiness: 1, RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](0, 0)},
			)
			for id, network := range tt.fields.networkIDs {
				netInfo := &multinetworkmocks.NetInfo{}
				netInfo.On("GetNetworkName").Return(network)
				netInfo.On("GetNetworkID").Return(id)
				tt.fields.networks[network] = netInfo
			}
			c := &controller{
				log:        testr.New(t),
				networkIDs: tt.fields.networkIDs,
				networks:   tt.fields.networks,
				tables:     tt.fields.tables,
				reconciler: r,
			}
			err := controllerutil.Start(r)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			c.syncLinkUpdate(tt.args.update)

			g.Expect(c.tables).To(gomega.Equal(tt.expectTables))
			g.Eventually(matchReconcile).WithArguments(tt.expectReconciles).Should(gomega.Succeed())
			g.Consistently(matchReconcile).WithArguments(tt.expectReconciles).Should(gomega.Succeed())
		})
	}
}

func Test_controller_subscribe(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	var m sync.Mutex
	var routeEventCh chan<- netlink.RouteUpdate
	var linkEventCh chan<- netlink.LinkUpdate
	setRouteEventCh := func(ch chan<- netlink.RouteUpdate) {
		m.Lock()
		defer m.Unlock()
		routeEventCh = ch
	}
	setLinkEventCh := func(ch chan<- netlink.LinkUpdate) {
		m.Lock()
		defer m.Unlock()
		linkEventCh = ch
	}
	isRouteEventChSet := func(g gomega.Gomega) {
		m.Lock()
		defer m.Unlock()
		g.Expect(routeEventCh).ToNot(gomega.BeNil())
	}
	isLinkEventChSet := func(g gomega.Gomega) {
		m.Lock()
		defer m.Unlock()
		g.Expect(linkEventCh).ToNot(gomega.BeNil())
	}

	matchOptions := func(options any) bool {
		switch o := options.(type) {
		case netlink.RouteSubscribeOptions:
			return o.ListExisting == true
		case netlink.LinkSubscribeOptions:
			return o.ListExisting == true
		}
		return false
	}

	var stopArg <-chan struct{} = stop
	nlmock := &mocks.NetLinkOps{}
	nlmock.On("RouteSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.RouteUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Run(func(args mock.Arguments) { setRouteEventCh(args.Get(0).(chan<- netlink.RouteUpdate)) }).
		Return(nil).Twice()
	nlmock.On("RouteSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.RouteUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Return(errors.New("test error")).Twice()
	nlmock.On("RouteSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.RouteUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Run(func(args mock.Arguments) { setRouteEventCh(args.Get(0).(chan<- netlink.RouteUpdate)) }).
		Return(nil).Once()

	nlmock.On("LinkSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.LinkUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Run(func(args mock.Arguments) { setLinkEventCh(args.Get(0).(chan<- netlink.LinkUpdate)) }).
		Return(nil).Twice()
	nlmock.On("LinkSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.LinkUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Return(errors.New("test error")).Twice()
	nlmock.On("LinkSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.LinkUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Run(func(args mock.Arguments) { setLinkEventCh(args.Get(0).(chan<- netlink.LinkUpdate)) }).
		Return(nil).Once()
	nlmock.On("LinkSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.LinkUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Run(func(args mock.Arguments) { setLinkEventCh(args.Get(0).(chan<- netlink.LinkUpdate)) }).
		Return(errors.New("test error"))

	c := &controller{
		log:     testr.New(t),
		netlink: nlmock,
		tables:  map[int]int{1: 1},
	}

	c.subscribe(stop)

	g := gomega.NewWithT(t)

	g.Eventually(isRouteEventChSet).Should(gomega.Succeed())
	g.Eventually(isLinkEventChSet).Should(gomega.Succeed())

	rch := routeEventCh
	routeEventCh = nil
	close(rch)

	g.Eventually(isRouteEventChSet).WithTimeout(subscribePeriod * 3).Should(gomega.Succeed())

	rch = routeEventCh
	routeEventCh = nil
	close(rch)

	g.Eventually(isRouteEventChSet).WithTimeout(subscribePeriod * 3).Should(gomega.Succeed())

	lch := linkEventCh
	linkEventCh = nil
	close(lch)

	g.Eventually(isLinkEventChSet).WithTimeout(subscribePeriod * 3).Should(gomega.Succeed())

	lch = linkEventCh
	linkEventCh = nil
	close(lch)

	g.Eventually(isLinkEventChSet).WithTimeout(subscribePeriod * 3).Should(gomega.Succeed())

	lch = linkEventCh
	linkEventCh = nil
	close(lch)

	g.Eventually(isLinkEventChSet).WithTimeout(subscribePeriod * 3).Should(gomega.Succeed())
	g.Expect(c.tables).To(gomega.BeEmpty())
}

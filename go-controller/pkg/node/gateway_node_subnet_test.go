// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// TestNodeSubnetRouteSelection covers that node subnet routes are derived from
// the bridge selected by the openflowManager - the Uplink bridge for an Uplink
// network, otherwise the shared gateway bridge - and that an Uplink enslaved
// into its own VRF (advertised, not to the default VRF) gets none, because the
// kernel owns its connected route. It also covers that the bridge is read live
// rather than cached (jcaamano concerns: uplink parity, no bridge cache,
// VRF-Lite skip).
func TestNodeSubnetRouteSelection(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.IPv4Mode, config.IPv6Mode = true, true
	config.Gateway.Mode = config.GatewayModeShared
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableUplink = true
	for _, tc := range []struct {
		name, uplink           string
		advertised, defaultVRF bool
		want                   int
	}{
		{name: "ordinary", want: 2},
		{name: "uplink unadvertised", uplink: "uplink1", want: 2},
		{name: "uplink default VRF", uplink: "uplink1", advertised: true, defaultVRF: true, want: 2},
		{name: "uplink dedicated VRF", uplink: "uplink1", advertised: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			nad := generateUplinkNAD("red", "rednad", "test", types.Layer3Topology,
				"100.128.0.0/16/24,ae70::/60/64", types.NetworkRolePrimary, tc.uplink)
			netInfo, err := util.ParseNADInfo(nad)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(netInfo.Uplink()).To(gomega.Equal(tc.uplink))
			ofm := getDummyOpenflowManager(ovntest.MustParseIPNets("192.0.2.5/24", "2001:db8:1::5/64")...)
			bridge := ofm.defaultBridge.BridgeConfiguration
			wantSrc := "192.0.2.5"
			if tc.uplink != "" {
				// The Uplink network's routes must come from the Uplink bridge
				// registered in the openflowManager, not the default bridge.
				bridge = bridgeconfig.TestBridgeConfig("br-uplink")
				bridge.SetIPs(ovntest.MustParseIPNets("198.51.100.5/24", "2001:db8:2::5/64"))
				g.Expect(ofm.addNetwork("br-uplink", bridge, netInfo, nil, nil, 1, 1, nil, nil)).To(gomega.Succeed())
				wantSrc = "198.51.100.5"
			}
			gw := &UserDefinedNetworkGateway{NetInfo: netInfo, gateway: &gateway{openflowManager: ofm},
				gwInterfaceIndex: 77, vrfTableId: 1007, isNetworkAdvertised: tc.advertised,
				isNetworkAdvertisedToDefaultVRF: tc.defaultVRF}
			routes := gw.computeNodeSubnetRoutes()
			g.Expect(routes).To(gomega.HaveLen(tc.want))
			if tc.want == 0 {
				return
			}
			g.Expect(routes[0].Src.String()).To(gomega.Equal(wantSrc))
			g.Expect(routes[0].LinkIndex).To(gomega.Equal(77))
			g.Expect(routes[0].Table).To(gomega.Equal(1007))
			// Read live configuration rather than a construction-time snapshot.
			bridge.SetIPs(ovntest.MustParseIPNets("203.0.113.5/24"))
			routes = gw.computeNodeSubnetRoutes()
			g.Expect(routes).To(gomega.HaveLen(1))
			g.Expect(routes[0].Dst.String()).To(gomega.Equal("203.0.113.0/24"))
		})
	}
}

// TestNodeSubnetRouteHostPrefixes covers that host prefixes (/32, /128), nil and
// empty addresses and unconfigured IP families are skipped, that IPv6 routes use
// universe scope to match the kernel, and that a missing openflowManager yields
// nothing (jcaamano concern: /32 node address).
func TestNodeSubnetRouteHostPrefixes(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.IPv4Mode, config.IPv6Mode = true, true
	ips := ovntest.MustParseIPNets("192.0.2.5/24", "2001:db8:1::5/64", "198.51.100.1/32", "2001:db8::1/128")
	ofm := getDummyOpenflowManager(append(ips, nil, &net.IPNet{})...)
	gw := &UserDefinedNetworkGateway{NetInfo: &util.DefaultNetInfo{}, gateway: &gateway{openflowManager: ofm},
		gwInterfaceIndex: 7, vrfTableId: 1007}
	byDst := map[string]netlink.Route{}
	for _, r := range gw.computeNodeSubnetRoutes() {
		byDst[r.Dst.String()] = r
	}
	g.Expect(byDst).To(gomega.HaveLen(2), "host prefixes, nil and empty addresses must be skipped")
	g.Expect(byDst).To(gomega.HaveKey("192.0.2.0/24"))
	g.Expect(byDst["192.0.2.0/24"].Scope).To(gomega.Equal(netlink.SCOPE_LINK))
	g.Expect(byDst).To(gomega.HaveKey("2001:db8:1::/64"))
	g.Expect(byDst["2001:db8:1::/64"].Scope).To(gomega.Equal(netlink.SCOPE_UNIVERSE))

	// Only configured IP families are programmed.
	config.IPv6Mode = false
	g.Expect(gw.computeNodeSubnetRoutes()).To(gomega.HaveLen(1))

	// No openflowManager (e.g. DPU mode) yields nothing.
	gw.openflowManager = nil
	g.Expect(gw.computeNodeSubnetRoutes()).To(gomega.BeEmpty())
}

// TestNodeSubnetRouteOwnership covers that isNodeSubnetRoute recognizes only the
// connected routes this controller owns, so reconcileNodeSubnetRoutes can use the
// kernel dump as the source of truth without clobbering other routes (jcaamano
// concern: use netlink as source of truth, like removeManagedDefaultRoutesFromVRF).
func TestNodeSubnetRouteOwnership(t *testing.T) {
	g := gomega.NewWithT(t)
	owned := netlink.Route{Dst: ovntest.MustParseIPNet("192.0.2.0/24"), Src: net.ParseIP("192.0.2.5"),
		Scope: netlink.SCOPE_LINK, Protocol: netlink.RouteProtocol(types.OVNKProtocol)}
	g.Expect(isNodeSubnetRoute(owned)).To(gomega.BeTrue())
	ownedV6 := netlink.Route{Dst: ovntest.MustParseIPNet("2001:db8::/64"), Src: net.ParseIP("2001:db8::5"),
		Scope: netlink.SCOPE_UNIVERSE, Protocol: netlink.RouteProtocol(types.OVNKProtocol)}
	g.Expect(isNodeSubnetRoute(ownedV6)).To(gomega.BeTrue(), "IPv6 connected route with universe scope")
	for _, tc := range []struct {
		name   string
		change func(*netlink.Route)
	}{
		{"kernel", func(r *netlink.Route) { r.Protocol = unix.RTPROT_KERNEL }},
		{"service", func(r *netlink.Route) { r.Gw = net.ParseIP("169.254.169.4") }},
		{"next hop", func(r *netlink.Route) { r.Src = nil }},
		{"universe scope IPv4", func(r *netlink.Route) { r.Scope = netlink.SCOPE_UNIVERSE }},
		{"other source", func(r *netlink.Route) { r.Src = net.ParseIP("198.51.100.5") }},
		{"local", func(r *netlink.Route) { r.Type = unix.RTN_LOCAL }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			route := owned
			tc.change(&route)
			g.Expect(isNodeSubnetRoute(route)).To(gomega.BeFalse(), "unrelated route: %v", route)
		})
	}
}

// TestUDNAddressChangeCallbackLifecycle covers that registering a reconciler does
// not itself add an address-change handler (the gateway's existing handler fans
// out via notifyUDNAddressChange), that an unregistered network is not notified,
// and that register/unregister/notify are safe to interleave (jcaamano concerns:
// no sync.Once, reuse the existing handler).
func TestUDNAddressChangeCallbackLifecycle(t *testing.T) {
	g := gomega.NewWithT(t)
	gw := &gateway{nodeIPManager: &addressManager{}}
	var calls atomic.Int32
	gw.registerUDNAddressChangeReconciler("red", func() {
		calls.Add(1)
		// A callback must be able to mutate the registry: notify releases the
		// lock before invoking callbacks.
		gw.unregisterUDNAddressChangeReconciler("red")
	})
	g.Expect(gw.nodeIPManager.onAddressesChangedHandlers).To(gomega.BeEmpty(),
		"registration must not add its own handler")
	gw.notifyUDNAddressChange()
	gw.notifyUDNAddressChange()
	g.Expect(calls.Load()).To(gomega.Equal(int32(1)), "deleted network must not be notified")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			gw.registerUDNAddressChangeReconciler("blue", func() {})
			gw.unregisterUDNAddressChangeReconciler("blue")
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			gw.notifyUDNAddressChange()
		}
	}()
	wg.Wait()
}

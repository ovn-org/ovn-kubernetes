// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"

	"k8s.io/utils/ptr"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestGatewayRoutesAfterSubnetChange(t *testing.T) {
	for _, networkName := range []string{"default", "blue"} {
		for _, sourceRoute := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/source-route=%t", networkName, sourceRoute), func(t *testing.T) {
				g := gomega.NewWithT(t)
				g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
				t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed()) })
				config.Gateway.Mode = config.GatewayModeShared
				config.Default.Transport = types.NetworkTransportNoOverlay
				config.IPv4Mode, config.IPv6Mode = true, true
				netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
					NetConf:  cnitypes.NetConf{Name: networkName},
					Topology: types.Layer3Topology, Role: types.NetworkRolePrimary,
					Subnets: "172.18.0.0/16/24,fd00::/16/64", Transport: types.NetworkTransportNoOverlay,
				})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				gwRouter := &nbdb.LogicalRouter{Name: netInfo.GetNetworkScopedGWRouterName("node1"), UUID: "gr-UUID"}
				clusterRouter := &nbdb.LogicalRouter{Name: netInfo.GetNetworkScopedClusterRouterName(), UUID: "cr-UUID"}
				router := gwRouter
				var policy *string
				v4NextHop, v6NextHop := "100.65.0.1", "fd98::1"
				if sourceRoute {
					router = clusterRouter
					policy = &nbdb.LogicalRouterStaticRoutePolicySrcIP
					v4NextHop, v6NextHop = "100.65.0.2", "fd98::2"
				}
				otherPolicy := &nbdb.LogicalRouterStaticRoutePolicySrcIP
				if sourceRoute {
					otherPolicy = nil
				}
				staleV4 := &nbdb.LogicalRouterStaticRoute{UUID: "old-v4-UUID", IPPrefix: "172.18.1.0/24", Nexthop: v4NextHop, Policy: policy}
				staleV6 := &nbdb.LogicalRouterStaticRoute{UUID: "old-v6-UUID", IPPrefix: "fd00:1::/64", Nexthop: v6NextHop, Policy: policy}
				// These routes share some attributes with the stale ones, but are not
				// this gateway's default-table subnet routes.
				preserved := []*nbdb.LogicalRouterStaticRoute{
					{UUID: "other-nexthop-UUID", IPPrefix: staleV4.IPPrefix, Nexthop: "100.65.0.9", Policy: policy},
					{UUID: "named-table-UUID", IPPrefix: staleV4.IPPrefix, Nexthop: v4NextHop, Policy: policy, RouteTable: "custom"},
					{UUID: "explicit-output-UUID", IPPrefix: staleV4.IPPrefix, Nexthop: v4NextHop, Policy: policy, OutputPort: ptr.To("custom-port")},
					{UUID: "imported-UUID", IPPrefix: staleV4.IPPrefix, Nexthop: v4NextHop, Policy: policy, ExternalIDs: map[string]string{libovsdbops.OwnerControllerKey.String(): "RouteImport"}},
					{UUID: "other-policy-UUID", IPPrefix: staleV4.IPPrefix, Nexthop: v4NextHop, Policy: otherPolicy},
					{UUID: "other-network-UUID", IPPrefix: staleV4.IPPrefix, Nexthop: v4NextHop, Policy: policy, ExternalIDs: map[string]string{types.NetworkExternalID: "other-network"}},
					{UUID: "external-prefix-UUID", IPPrefix: "192.0.2.0/24", Nexthop: v4NextHop, Policy: policy},
				}
				if !sourceRoute && !netInfo.IsUserDefinedNetwork() {
					preserved = append(preserved, &nbdb.LogicalRouterStaticRoute{
						UUID: "hybrid-overlay-UUID", IPPrefix: staleV4.IPPrefix, Nexthop: v4NextHop,
						ExternalIDs: map[string]string{"name": types.HybridSubnetPrefix + "node1" + types.HybridOverlayGRSubfix},
					})
				}
				if netInfo.IsUserDefinedNetwork() {
					for _, route := range append([]*nbdb.LogicalRouterStaticRoute{staleV4, staleV6}, preserved...) {
						if route.ExternalIDs == nil {
							route.ExternalIDs = map[string]string{}
						}
						if route.ExternalIDs[types.NetworkExternalID] == "" {
							route.ExternalIDs[types.NetworkExternalID] = networkName
							route.ExternalIDs[types.TopologyExternalID] = netInfo.TopologyType()
						}
					}
				}
				data := []libovsdbtest.TestData{gwRouter, clusterRouter, staleV4, staleV6}
				router.StaticRoutes = []string{staleV4.UUID, staleV6.UUID}
				for _, route := range preserved {
					data = append(data, route)
					router.StaticRoutes = append(router.StaticRoutes, route.UUID)
				}
				nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				t.Cleanup(cleanup.Cleanup)
				gw := &GatewayManager{nbClient: nbClient, netInfo: netInfo, gwRouterName: gwRouter.Name, clusterRouterName: clusterRouter.Name}
				cfg := &GatewayConfig{
					annoConfig:                 &util.L3GatewayConfig{},
					hostSubnets:                ovntest.MustParseIPNets("172.18.2.0/24", "fd00:2::/64"),
					clusterSubnets:             ovntest.MustParseIPNets("172.18.0.0/16", "fd00::/16"),
					ovnClusterLRPToJoinIfAddrs: ovntest.MustParseIPNets("100.65.0.1/16", "fd98::1/64"),
				}
				for range 2 {
					if sourceRoute {
						err = gw.updateClusterRouterStaticRoutes(cfg, []net.IP{net.ParseIP(v4NextHop), net.ParseIP(v6NextHop)})
					} else {
						err = gw.updateGWRouterStaticRoutes(cfg, "rtoe-GR_node1", gwRouter)
					}
					g.Expect(err).NotTo(gomega.HaveOccurred())
					g.Eventually(func(g gomega.Gomega) {
						routes, err := libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, router, func(*nbdb.LogicalRouterStaticRoute) bool { return true })
						g.Expect(err).NotTo(gomega.HaveOccurred())
						var prefixes []string
						for _, route := range routes {
							prefixes = append(prefixes, route.IPPrefix)
						}
						wanted := []string{"172.18.2.0/24", "fd00:2::/64"}
						for _, route := range preserved {
							wanted = append(wanted, route.IPPrefix)
							g.Expect(routes).To(gomega.ContainElement(gomega.And(
								gomega.HaveField("IPPrefix", route.IPPrefix),
								gomega.HaveField("Nexthop", route.Nexthop),
								gomega.HaveField("Policy", route.Policy),
								gomega.HaveField("OutputPort", route.OutputPort),
								gomega.HaveField("RouteTable", route.RouteTable),
								gomega.HaveField("ExternalIDs", route.ExternalIDs),
							)))
						}
						if sourceRoute {
							wanted = append(wanted, v4NextHop, v6NextHop)
						} else {
							wanted = append(wanted, config.Gateway.V4MasqueradeSubnet, config.Gateway.V6MasqueradeSubnet)
						}
						g.Expect(prefixes).To(gomega.ConsistOf(wanted), "obsolete subnet routes must disappear while unrelated routes survive")
					}, 2*time.Second).Should(gomega.Succeed())
				}
			})
		}
	}
}

func TestGatewayPreservesEgressCatchAllRoutes(t *testing.T) {
	for _, tc := range []struct {
		name           string
		gatewayMode    config.GatewayMode
		clusterSubnets []string
	}{
		{"shared", config.GatewayModeShared, []string{"10.128.0.0/14", "fd00::/48"}},
		{"local", config.GatewayModeLocal, []string{"10.128.0.0/14", "fd00::/48"}},
		{"shared after cluster subnet expansion", config.GatewayModeShared, []string{"10.128.0.0/13", "fd00::/47"}},
		{"local after cluster subnet expansion", config.GatewayModeLocal, []string{"10.128.0.0/13", "fd00::/47"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed()) })
			config.Gateway.Mode = tc.gatewayMode
			config.IPv4Mode, config.IPv6Mode = true, true
			router := &nbdb.LogicalRouter{Name: types.OVNClusterRouter, UUID: "cluster-router-UUID"}
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{router},
			}, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(cleanup.Cleanup)
			clusterSubnets := ovntest.MustParseIPNets("10.128.0.0/14", "fd00::/48")
			joinIPs := ovntest.MustParseIPNets("100.64.0.2/16", "fd98::2/64")
			entries := []config.CIDRNetworkEntry{
				{CIDR: clusterSubnets[0], HostSubnetLength: 24},
				{CIDR: clusterSubnets[1], HostSubnetLength: 64},
			}
			// Use the shared producer so this test follows the routes used by
			// EgressIP, EgressService and VM live migration.
			g.Expect(libovsdbutil.CreateDefaultRouteToExternal(nbClient, router.Name, "GR_node1", entries, joinIPs)).To(gomega.Succeed())
			catchAllRoutes := func() ([]*nbdb.LogicalRouterStaticRoute, error) {
				return libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, router, func(route *nbdb.LogicalRouterStaticRoute) bool {
					return route.Policy != nil && *route.Policy == nbdb.LogicalRouterStaticRoutePolicySrcIP &&
						(route.IPPrefix == clusterSubnets[0].String() || route.IPPrefix == clusterSubnets[1].String())
				})
			}
			g.Eventually(catchAllRoutes).Should(gomega.HaveLen(2))
			gw := &GatewayManager{nbClient: nbClient, netInfo: &util.DefaultNetInfo{}, gwRouterName: "GR_node1", clusterRouterName: router.Name}
			cfg := &GatewayConfig{
				clusterSubnets: ovntest.MustParseIPNets(tc.clusterSubnets...),
				hostSubnets:    ovntest.MustParseIPNets("10.128.1.0/24", "fd00:0:0:1::/64"),
			}
			for range 2 {
				g.Expect(gw.updateClusterRouterStaticRoutes(cfg, util.IPNetsToIPs(joinIPs))).To(gomega.Succeed())
			}
			g.Consistently(catchAllRoutes, 2*time.Second, 20*time.Millisecond).Should(gomega.HaveLen(2),
				"gateway reconciliation must retain the shared catch-all source routes")
		})
	}
}

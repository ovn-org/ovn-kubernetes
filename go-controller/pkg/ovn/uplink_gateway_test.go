// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
)

func TestSyncUplinkDefaultRoutes(t *testing.T) {
	for _, dualStack := range []bool{false, true} {
		t.Run(map[bool]string{false: "IPv4", true: "dual stack"}[dualStack], func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo := multinetworkmocks.NewNetInfo(t)
			netInfo.On("GetNetworkName").Return("blue")
			netInfo.On("TopologyType").Return(types.Layer3Topology)
			netInfo.On("IPMode").Return(true, dualStack)
			port, otherPort := "rtoe-GR_blue", "other-port"
			preserved := []*nbdb.LogicalRouterStaticRoute{
				{UUID: "imported", IPPrefix: "0.0.0.0/0", Nexthop: "192.0.2.1", OutputPort: &port,
					ExternalIDs: map[string]string{string(libovsdbops.OwnerControllerKey): "RouteImport", types.NetworkExternalID: "blue"}},
				{UUID: "other-table", IPPrefix: "0.0.0.0/0", Nexthop: "192.0.2.99", OutputPort: &port, RouteTable: "other",
					ExternalIDs: map[string]string{types.NetworkExternalID: "blue"}},
				{UUID: "other-port", IPPrefix: "0.0.0.0/0", Nexthop: "192.0.2.99", OutputPort: &otherPort,
					ExternalIDs: map[string]string{types.NetworkExternalID: "blue"}},
				// The masquerade return route of the network shares the tags
				// and the port of the default routes and must not be touched.
				{UUID: "masquerade", IPPrefix: "169.254.169.0/29", Nexthop: "169.254.169.4", OutputPort: &port,
					ExternalIDs: map[string]string{types.NetworkExternalID: "blue", types.TopologyExternalID: types.Layer3Topology}},
			}
			router := &nbdb.LogicalRouter{UUID: "router", Name: "GR_blue"}
			data := []libovsdbtest.TestData{router}
			for _, route := range preserved {
				router.StaticRoutes = append(router.StaticRoutes, route.UUID)
				data = append(data, route)
			}
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(cleanup.Cleanup)
			// Capture the assigned UUIDs as well as the contents, so updates
			// must preserve these exact rows.
			allRoutes := func(*nbdb.LogicalRouterStaticRoute) bool { return true }
			preserved, err = libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, router, allRoutes)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(preserved).To(gomega.HaveLen(4))
			gw := &GatewayManager{nbClient: nbClient, netInfo: netInfo, gwRouterName: router.Name}
			var many []string
			for i := 1; i <= 256; i++ {
				if dualStack && i > 128 {
					many = append(many, fmt.Sprintf("2001:db8::%x", i-128))
				} else {
					many = append(many, fmt.Sprintf("192.0.%d.%d", i/256, i%256))
				}
			}
			for _, nextHops := range [][]string{
				{"192.0.2.1", "192.0.2.2", "2001:db8::1", "192.0.2.1"},
				{"192.0.2.2", "192.0.2.1", "2001:db8::1"},
				{"192.0.2.2", "2001:db8::2"},
				many,
				nil,
			} {
				var ips []net.IP
				// next hop -> expected default prefix of its family
				expected := map[string]string{}
				for _, nextHop := range nextHops {
					ip := net.ParseIP(nextHop)
					ips = append(ips, ip)
					if ip.To4() != nil {
						expected[nextHop] = "0.0.0.0/0"
					} else if dualStack {
						expected[nextHop] = "::/0"
					}
				}
				g.Expect(gw.syncUplinkDefaultRoutes(ips, port)).To(gomega.Succeed())
				routes, err := libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, router, allRoutes)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				// A repeated sync with the same next hops changes nothing.
				g.Expect(gw.syncUplinkDefaultRoutes(ips, port)).To(gomega.Succeed())
				again, err := libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, router, allRoutes)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(again).To(gomega.ConsistOf(routes))
				g.Expect(routes).To(gomega.HaveLen(len(preserved) + len(expected)))
				for _, route := range preserved {
					g.Expect(routes).To(gomega.ContainElement(route))
				}
				for nextHop, prefix := range expected {
					g.Expect(routes).To(gomega.ContainElement(gomega.And(
						gomega.HaveField("Nexthop", nextHop),
						gomega.HaveField("IPPrefix", prefix),
						gomega.HaveField("OutputPort", gomega.HaveValue(gomega.Equal(port))),
						gomega.HaveField("Policy", gomega.BeNil()),
						gomega.HaveField("RouteTable", ""),
						gomega.HaveField("ExternalIDs", gomega.Equal(map[string]string{
							types.NetworkExternalID: "blue", types.TopologyExternalID: types.Layer3Topology,
						})),
					)))
				}
			}
		})
	}
}

func TestResolvedUplinkL3GatewayConfig(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.Gateway.Mode = config.GatewayModeShared
	config.Gateway.NodeportEnable = true

	gwConfig, err := resolvedUplinkL3GatewayConfig(testUplinkState(metav1.ConditionTrue), "node-a", "chassis-a")

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(gwConfig.Mode).To(gomega.Equal(config.GatewayModeShared))
	g.Expect(gwConfig.ChassisID).To(gomega.Equal("chassis-a"))
	g.Expect(gwConfig.BridgeID).To(gomega.Equal("breth0"))
	g.Expect(gwConfig.InterfaceID).To(gomega.Equal("breth0_node-a"))
	g.Expect(gwConfig.MACAddress.String()).To(gomega.Equal("02:42:ac:12:00:02"))
	g.Expect(gwConfig.IPAddresses).To(gomega.HaveLen(1))
	g.Expect(gwConfig.IPAddresses[0].String()).To(gomega.Equal("192.0.2.10/24"))
	g.Expect(gwConfig.NextHops).To(gomega.HaveLen(1))
	g.Expect(gwConfig.NextHops[0].String()).To(gomega.Equal("192.0.2.1"))
	g.Expect(gwConfig.NodePortEnable).To(gomega.BeTrue())
}

func TestUplinkStateResolved(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(uplinkutil.StateResolved(testUplinkState(metav1.ConditionTrue))).To(gomega.BeTrue())
	g.Expect(uplinkutil.StateResolved(testUplinkState(metav1.ConditionFalse))).To(gomega.BeFalse())
}

func testUplinkState(resolvedStatus metav1.ConditionStatus) *uplinkv1alpha1.UplinkState {
	return &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{Name: "blue-node-a"},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: "blue",
			NodeName:   "node-a",
		},
		Status: uplinkv1alpha1.UplinkStateStatus{
			Type:              uplinkv1alpha1.UplinkTypeOVSBridge,
			HostInterfaceName: "breth0",
			OVSBridge: &uplinkv1alpha1.OVSBridgeStatus{
				Name: "breth0",
			},
			MACAddress:      "02:42:ac:12:00:02",
			IPAddresses:     []uplinkv1alpha1.IPAddressCIDR{"192.0.2.10/24"},
			DefaultGateways: []uplinkv1alpha1.IPAddress{"192.0.2.1"},
			Conditions: []metav1.Condition{
				{
					Type:               uplinkv1alpha1.UplinkStateConditionResolved,
					Status:             resolvedStatus,
					Reason:             uplinkv1alpha1.UplinkStateReasonResolved,
					LastTransitionTime: metav1.NewTime(time.Now()),
				},
			},
		},
	}
}

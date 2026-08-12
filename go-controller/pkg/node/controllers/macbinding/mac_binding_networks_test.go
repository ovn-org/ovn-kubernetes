// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"fmt"
	"maps"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/util/sets"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// --- allocation engine ---------------------------------------------------

// primaryUDN builds a primary Layer2 UDN NetInfo for allocation tests. A
// non-empty uplink is reported by NetInfo.Uplink() only when the uplink feature
// is enabled (see enableUplinkFeature), so callers exercising uplink groups must
// enable it first.
func primaryUDN(g gomega.Gomega, name, nadKey, uplink string) util.NetInfo {
	ni, err := util.NewNetInfo(&ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: name, Type: "ovn-k8s-cni-overlay"},
		Role:     types.NetworkRolePrimary,
		Topology: types.Layer2Topology,
		NADName:  nadKey,
		Subnets:  "192.168.0.0/16",
		MTU:      1400,
		Uplink:   uplink,
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return ni
}

// TestReconcileNetworks drives the network-reconcile stack through its real entry
// point, reconcileNetwork: the NAD-event dispatch and, via the "" key, the gather
// + source-resolution + port-validation pipeline that feeds the allocation engine.
// It asserts the resulting follower/port state and the keys enqueued on the
// mac-binding and network reconcilers. TestUpdateFollowers covers the
// allocation engine's relocation scenarios in isolation.
func TestReconcileNetworks(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableUplink = true
	config.Gateway.DisableUDNARPNDPFlood = true

	cdnPort := cdnPortFor("node1")

	// Default-group primary UDN (shares breth0, Uplink() == "").
	udn := primaryUDN(g, "tenantred", "ns1/nad1", "")
	udnPort := util.GetNetworkScopedGWRouterExtPortName(udn.GetNetworkName(), "node1")
	// Two CUDNs sharing a dedicated uplink; cudnA is the designated source.
	cudnA := primaryUDN(g, "cudnA", "nsA/nadA", "uplinkA")
	cudnB := primaryUDN(g, "cudnB", "nsB/nadB", "uplinkA")
	portA := util.GetNetworkScopedGWRouterExtPortName(cudnA.GetNetworkName(), "node1")
	portB := util.GetNetworkScopedGWRouterExtPortName(cudnB.GetNetworkName(), "node1")

	tests := []struct {
		name string
		// primaryNetworks are the networks the fake network manager tracks
		// (namespace -> NetInfo); the CDN is always gathered implicitly.
		primaryNetworks map[string]util.NetInfo
		// nadNetworks are resolved by NAD key (nadKey -> NetInfo); needed when
		// reconcileKey is a NAD namespaced name (the dispatch path).
		nadNetworks map[string]util.NetInfo
		// uplinkSources is what the uplink source provider designates
		// (uplink -> source GR external port).
		uplinkSources map[string]string
		// sbPorts are the GR external ports that have a PortBinding in the SB DB.
		sbPorts []string
		// preFollowers pre-registers source -> followers as already known.
		preFollowers map[string][]string
		// reconcileKey is handed to reconcileNetwork: "" reconciles all networks,
		// a NAD namespaced name exercises the event-routing dispatch.
		reconcileKey string
		// wantMbEnqueued is the set of keys enqueued on the mac-binding reconciler
		// for a catch-up.
		wantMbEnqueued []string
		// wantNetEnqueued is the set of keys re-enqueued on the network reconciler
		// (the dispatch early-outs enqueue "" for a full reconcile).
		wantNetEnqueued []string
		// wantTrackedPorts is the full set of tracked ports (sources + followers)
		// after the reconcile; nil skips the assertion.
		wantTrackedPorts []string
		// wantFollowers is the expected source -> followers after the reconcile
		// (only sources with followers need to be listed).
		wantFollowers map[string][]string
		// dirtyUplinkSource leaves the uplink->source cache invalidated (nil)
		// going into the reconcile, as ReconcileUplinkSource leaves it after a
		// re-designation. By default the cache is warmed first, modelling steady
		// state where the designation is already computed.
		dirtyUplinkSource bool
	}{
		{
			name:             "NAD event for an already-tracked network is a no-op",
			primaryNetworks:  map[string]util.NetInfo{"ns1": udn},
			nadNetworks:      map[string]util.NetInfo{"ns1/nad1": udn},
			sbPorts:          []string{cdnPort, udnPort},
			preFollowers:     map[string][]string{cdnPort: {udnPort}},
			reconcileKey:     "ns1/nad1",
			wantTrackedPorts: []string{cdnPort, udnPort},
			wantFollowers:    map[string][]string{cdnPort: {udnPort}},
		},
		{
			name:            "NAD event while a follower's source is unknown triggers a full reconcile",
			nadNetworks:     map[string]util.NetInfo{"ns1/nad1": udn},
			preFollowers:    map[string][]string{"unknown": {"rtoe-GR_other_node1"}},
			reconcileKey:    "ns1/nad1",
			wantNetEnqueued: []string{""},
		},
		{
			name:               "full reconcile mirrors the CDN onto a primary UDN follower",
			primaryNetworks:    map[string]util.NetInfo{"ns1": udn},
			sbPorts:            []string{cdnPort, udnPort},
			reconcileKey:     "",
			wantMbEnqueued:   []string{udnPort},
			wantTrackedPorts: []string{cdnPort, udnPort},
			wantFollowers:    map[string][]string{cdnPort: {udnPort}},
		},
		{
			name:               "full reconcile cleans up a follower whose port left the SB",
			primaryNetworks:    map[string]util.NetInfo{"ns1": udn},
			sbPorts:          []string{cdnPort}, // udnPort's PortBinding is gone
			preFollowers:     map[string][]string{cdnPort: {udnPort}},
			reconcileKey:     "",
			wantTrackedPorts: []string{cdnPort},
		},
		{
			// The full-reconcile removal-by-difference path: udn was deleted, so
			// the network manager no longer returns it and the per-network loop
			// never inspects its port. Its port is parked under "unknown" (source
			// never resolved) with no portToUplink entry, so without removing it by
			// difference it would stay wedged there, keeping hasUnknownSource true
			// forever and every later event on a full reconcile.
			name:               "full reconcile removes a stale unknown-bucket port for a deleted network",
			primaryNetworks:    map[string]util.NetInfo{}, // udn deleted
			sbPorts:            []string{cdnPort},         // udnPort's PortBinding is gone
			preFollowers:     map[string][]string{cdnPort: {}, "unknown": {udnPort}},
			reconcileKey:     "",
			wantTrackedPorts: []string{cdnPort}, // unknown bucket cleared
			wantFollowers:    map[string][]string{cdnPort: nil},
		},
		{
			name:               "full reconcile mirrors the designated uplink source onto the other member",
			primaryNetworks:    map[string]util.NetInfo{"nsA": cudnA, "nsB": cudnB},
			uplinkSources:      map[string]string{"uplinkA": portA},
			sbPorts:            []string{cdnPort, portA, portB},
			reconcileKey:     "",
			wantMbEnqueued:   []string{portB},
			wantTrackedPorts: []string{cdnPort, portA, portB},
			wantFollowers:    map[string][]string{portA: {portB}},
		},
		{
			// The trickiest branch driven end to end: the uplink's designated
			// source changes from portA to portB, the state ReconcileUplinkSource
			// leaves behind (uplink->source cache invalidated, so hasUnknownSource
			// trips the skip guard despite no membership change). The old source is
			// relocated as a follower under the new one.
			name:               "full reconcile relocates followers when the uplink source is re-designated",
			primaryNetworks:    map[string]util.NetInfo{"nsA": cudnA, "nsB": cudnB},
			uplinkSources:      map[string]string{"uplinkA": portB}, // portB newly designated
			sbPorts:            []string{cdnPort, portA, portB},
			preFollowers:       map[string][]string{cdnPort: {}, portA: {portB}},
			reconcileKey:       "",
			dirtyUplinkSource: true,
			wantMbEnqueued:    []string{portA}, // old source becomes a follower under portB
			wantTrackedPorts:  []string{cdnPort, portA, portB},
			wantFollowers:     map[string][]string{portB: {portA}},
		},
		{
			name:             "full reconcile skips a network whose port is absent from the SB",
			primaryNetworks:  map[string]util.NetInfo{"ns1": udn},
			sbPorts:          []string{cdnPort}, // udnPort has no PortBinding
			reconcileKey:     "",
			wantTrackedPorts: []string{cdnPort}, // only the CDN is realized
		},
		{
			name:             "full reconcile is a no-op when every gathered port is already tracked",
			primaryNetworks:  map[string]util.NetInfo{"ns1": udn},
			sbPorts:          []string{cdnPort, udnPort},
			preFollowers:     map[string][]string{cdnPort: {udnPort}},
			reconcileKey:     "",
			wantTrackedPorts: []string{cdnPort, udnPort},
			wantFollowers:    map[string][]string{cdnPort: {udnPort}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			setup := libovsdbtest.TestSetup{IgnoreConstraints: true}
			for i, port := range tt.sbPorts {
				dp := fmt.Sprintf("dp-%d", i)
				setup.SBData = append(setup.SBData,
					&sbdb.DatapathBinding{UUID: dp},
					&sbdb.PortBinding{UUID: fmt.Sprintf("pb-%d", i), LogicalPort: port, Datapath: dp},
				)
			}
			sbClient, cleanup, err := libovsdbtest.NewSBTestHarness(setup, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			defer cleanup.Cleanup()

			mbRec, mbR := startRecorder(g, "mb")
			defer controller.Stop(mbR)
			netRec, netR := startRecorder(g, "network")
			defer controller.Stop(netR)

			// A non-nil (possibly empty) sources map is required: a nil map makes
			// the provider skip the default "" -> CDN mapping.
			sources := maps.Clone(tt.uplinkSources)
			if sources == nil {
				sources = map[string]string{}
			}

			c := newTestController()
			c.sbClient = sbClient
			c.networkManager = &networkmanager.FakeNetworkManager{
				PrimaryNetworks: tt.primaryNetworks,
				NADNetworks:     tt.nadNetworks,
			}
			c.cdnGatewayPort = cdnPort
			c.dynamicMacBindingReconciler = mbR
			c.networkReconciler = netR
			c.uplinkSourceProvider = &fakeUplinkSourceProvider{sources: sources}
			for source, followers := range tt.preFollowers {
				c.followers[source] = sets.New(followers...)
			}
			if !tt.dirtyUplinkSource {
				// Steady state: the uplink->source designation is already computed
				// (warm cache), so only real membership changes or a non-empty
				// "unknown" bucket drive a full reconcile — not the dirty-cache signal.
				c.getMacBindingSourceForUplinks()
			}

			g.Expect(c.reconcileNetwork(tt.reconcileKey)).To(gomega.Succeed())

			// Keys enqueued on the mac-binding reconciler for a catch-up.
			if len(tt.wantMbEnqueued) == 0 {
				g.Consistently(mbRec.got).Should(gomega.BeEmpty())
			} else {
				g.Eventually(mbRec.got).Should(gomega.ConsistOf(tt.wantMbEnqueued))
			}
			// Keys re-enqueued on the network reconciler (dispatch early-outs).
			if len(tt.wantNetEnqueued) == 0 {
				g.Consistently(netRec.got).Should(gomega.BeEmpty())
			} else {
				g.Eventually(netRec.got).Should(gomega.ConsistOf(tt.wantNetEnqueued))
			}
			// Resulting follower/port state.
			for source, followers := range tt.wantFollowers {
				g.Expect(c.getFollowers(source)).To(gomega.ConsistOf(followers))
			}
			if tt.wantTrackedPorts != nil {
				g.Expect(c.getAllPorts().UnsortedList()).To(gomega.ConsistOf(tt.wantTrackedPorts))
			}
		})
	}
}

// TestUpdateFollowers exercises the in-memory follower allocation engine
// directly: given a starting followers/ports state and a set of port additions,
// removals and the resolved uplink/source topology, it asserts the recomputed
// follower map and the set of ports that became new followers (which the caller
// enqueues for a mac binding catch-up).
func TestUpdateFollowers(t *testing.T) {
	cdnPort := cdnPortFor("node1")
	const (
		udnPort   = "rtoe-GR_tenantred_node1"
		oldSource = "rtoe-GR_cudnA_node1"
		newSource = "rtoe-GR_cudnB_node1"
	)

	tests := []struct {
		name string
		// starting controller state
		followers map[string][]string // source -> followers

		// updateFollowers arguments
		addPorts       []string
		removePorts    []string
		portToUplink   map[string]string
		uplinkToSource map[string]string

		// expectations
		wantNewFollowers []string
		wantFollowers    map[string][]string // source -> expected followers
		wantTrackedPorts []string            // ports expected to remain tracked
	}{
		{
			// A follower whose port disappeared is dropped, and its now-empty
			// source is removed.
			name:           "removes a dropped follower and prunes its empty source",
			followers:      map[string][]string{cdnPort: {udnPort}},
			removePorts:    []string{udnPort},
			portToUplink:   map[string]string{cdnPort: ""},
			uplinkToSource: map[string]string{"": cdnPort},
			wantFollowers:  map[string][]string{cdnPort: nil},
		},
		{
			// A port on an uplink with no known source is parked in the
			// "unknown" bucket rather than dropped, and is still tracked.
			name:             "parks a port with no known source as unknown",
			addPorts:         []string{udnPort},
			portToUplink:     map[string]string{udnPort: "uplinkA"}, // uplinkA has no source
			uplinkToSource:   map[string]string{"": cdnPort},        // only default group
			wantFollowers:    map[string][]string{"unknown": {udnPort}},
			wantTrackedPorts: []string{udnPort},
		},
		{
			// When the designated source of an uplink group changes, the old
			// source's followers are relocated under the new source and the old
			// source itself becomes a follower.
			name:             "relocates followers when the source is re-designated",
			followers:        map[string][]string{oldSource: {newSource}},
			portToUplink:     map[string]string{oldSource: "uplinkA", newSource: "uplinkA"},
			uplinkToSource:   map[string]string{"uplinkA": newSource, "": cdnPort},
			wantNewFollowers: []string{oldSource},
			wantFollowers: map[string][]string{
				newSource: {oldSource},
				oldSource: nil,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			c := newTestController()
			c.cdnGatewayPort = cdnPort
			for source, followers := range tt.followers {
				c.followers[source] = sets.New(followers...)
			}

			newFollowers := c.updateFollowers(
				sets.New(tt.addPorts...),
				sets.New(tt.removePorts...),
				tt.portToUplink,
				tt.uplinkToSource,
			)

			g.Expect(newFollowers.UnsortedList()).To(gomega.ConsistOf(tt.wantNewFollowers))
			for source, want := range tt.wantFollowers {
				g.Expect(c.getFollowers(source)).To(gomega.ConsistOf(want))
			}

			for _, port := range tt.wantTrackedPorts {
				g.Expect(c.getAllPorts().Has(port)).To(gomega.BeTrue())
			}
		})
	}
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"context"
	"testing"

	"github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// TestReconcileMacBindingsForIPFromSource verifies the mirror read path: given a
// designated source MAC_Binding and a follower Gateway Router port,
// syncDynamicMacBinding resolves the follower datapath and mirrors the (ip, mac)
// pair onto it. A missing source binding is a no-op: followers are left to age
// out rather than being cleared.
func TestReconcileMacBindingsForIPFromSource(t *testing.T) {
	// ipv4 enabled, ipv6 disabled
	gomega.NewWithT(t).Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.Gateway.DisableUDNARPNDPFlood = true
	const (
		cdnPort    = "rtoe-GR_node1"
		targetPort = "rtoe-GR_udn_node1"
		srcIP      = "10.0.0.5"
		srcMAC     = "0a:00:00:00:00:05"
	)

	tests := []struct {
		name string
		// seedBinding, when true, seeds the source MAC_Binding row for srcIP.
		seedBinding bool
		reconcileIP string
		wantMirror  bool
	}{
		{
			name:        "mirrors an existing source binding onto the follower",
			seedBinding: true,
			reconcileIP: srcIP,
			wantMirror:  true,
		},
		{
			name:        "is a no-op when the source binding is missing",
			reconcileIP: "10.0.0.9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			setup := libovsdbtest.TestSetup{
				IgnoreConstraints: true,
				SBData: []libovsdbtest.TestData{
					&sbdb.DatapathBinding{UUID: "src-dp"},
					&sbdb.DatapathBinding{UUID: "tgt-dp"},
					&sbdb.PortBinding{UUID: "cdn-pb", LogicalPort: cdnPort, Datapath: "src-dp"},
					&sbdb.PortBinding{UUID: "tgt-pb", LogicalPort: targetPort, Datapath: "tgt-dp"},
				},
			}
			if tt.seedBinding {
				setup.SBData = append(setup.SBData,
					&sbdb.MACBinding{UUID: "src-mb", LogicalPort: cdnPort, IP: srcIP, MAC: srcMAC, Datapath: "src-dp", Timestamp: 100})
			}
			sbClient, cleanup, err := libovsdbtest.NewSBTestHarness(setup, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			defer cleanup.Cleanup()

			c := newTestController()
			c.sbClient = sbClient
			c.cdnGatewayPort = cdnPort
			c.followers[cdnPort] = sets.New(targetPort)

			tgtPB, err := ops.GetPortBinding(sbClient, &sbdb.PortBinding{LogicalPort: targetPort})
			g.Expect(err).NotTo(gomega.HaveOccurred())

			// The write is synchronous: TransactAndCheck only returns once the
			// server has broadcast the update and the client has applied it to
			// its cache, so the cache read below needs no Eventually.
			g.Expect(c.syncDynamicMacBinding(cdnPort, tt.reconcileIP, addWarnDelayThreshold)).To(gomega.Succeed())

			if !tt.wantMirror {
				g.Expect(macBindingsFor(g, sbClient, targetPort)).To(gomega.BeEmpty())
				return
			}

			// The (ip, mac) pair is mirrored onto the follower's datapath.
			mbs := macBindingsFor(g, sbClient, targetPort)
			g.Expect(mbs).To(gomega.HaveLen(1))
			g.Expect(mbs[0].IP).To(gomega.Equal(srcIP))
			g.Expect(mbs[0].MAC).To(gomega.Equal(srcMAC))
			g.Expect(mbs[0].Datapath).To(gomega.Equal(tgtPB.Datapath))
		})
	}
}

// TestReconcileMacBindingsForFollower verifies the follower catch-up path: a
// newly added follower gets every current source binding for an enabled IP
// family mirrored onto it, while bindings for a disabled family are skipped.
func TestReconcileMacBindingsForFollower(t *testing.T) {
	g := gomega.NewWithT(t)
	// ipv4 enabled, ipv6 disabled
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.Gateway.DisableUDNARPNDPFlood = true

	const (
		cdnPort    = "rtoe-GR_node1"
		targetPort = "rtoe-GR_udn_node1"
	)

	setup := libovsdbtest.TestSetup{
		IgnoreConstraints: true,
		SBData: []libovsdbtest.TestData{
			&sbdb.DatapathBinding{UUID: "src-dp"},
			&sbdb.DatapathBinding{UUID: "tgt-dp"},
			&sbdb.PortBinding{UUID: "cdn-pb", LogicalPort: cdnPort, Datapath: "src-dp"},
			&sbdb.PortBinding{UUID: "tgt-pb", LogicalPort: targetPort, Datapath: "tgt-dp"},
			&sbdb.MACBinding{UUID: "mb1", LogicalPort: cdnPort, IP: "10.0.0.5", MAC: "0a:00:00:00:00:05", Datapath: "src-dp", Timestamp: 100},
			&sbdb.MACBinding{UUID: "mb2", LogicalPort: cdnPort, IP: "10.0.0.6", MAC: "0a:00:00:00:00:06", Datapath: "src-dp", Timestamp: 100},
			// IPv6 binding: ignored because the controller has IPv6 disabled.
			&sbdb.MACBinding{UUID: "mb3", LogicalPort: cdnPort, IP: "fd00::5", MAC: "0a:00:00:00:00:07", Datapath: "src-dp", Timestamp: 100},
		},
	}
	sbClient, cleanup, err := libovsdbtest.NewSBTestHarness(setup, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer cleanup.Cleanup()

	c := newTestController()
	c.sbClient = sbClient
	c.cdnGatewayPort = cdnPort
	c.followers[cdnPort] = sets.New(targetPort)

	tgtPB, err := ops.GetPortBinding(sbClient, &sbdb.PortBinding{LogicalPort: targetPort})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	g.Expect(c.syncDynamicMacBindingsToFollower(targetPort)).To(gomega.Succeed())

	// Only the enabled-family (IPv4) source bindings are mirrored onto the new
	// follower; the IPv6 binding is skipped.
	got := map[string]string{}
	for _, mb := range macBindingsFor(g, sbClient, targetPort) {
		got[mb.IP] = mb.MAC
		g.Expect(mb.Datapath).To(gomega.Equal(tgtPB.Datapath))
	}
	g.Expect(got).To(gomega.Equal(map[string]string{
		"10.0.0.5": "0a:00:00:00:00:05",
		"10.0.0.6": "0a:00:00:00:00:06",
	}))
}

// TestSetMacBindings verifies the actual SB write path: setDynamicMacBindings
// mirrors every (port, ip) pair in a single write, creating a row that does not
// exist, always writing a changed MAC, updating an unchanged row only when the
// mirrored timestamp is newer, and leaving an unchanged row whose timestamp is not
// newer untouched.
func TestSetMacBindings(t *testing.T) {
	config.Gateway.DisableUDNARPNDPFlood = true
	const (
		portA     = "rtoe-GR_udnA_node1"
		portB     = "rtoe-GR_udnB_node1"
		ip        = "10.0.0.7"
		otherIP   = "10.0.0.8"
		firstMAC  = "0a:00:00:00:00:07"
		otherMAC  = "0a:00:00:00:00:08"
		secondMAC = "0a:00:00:00:00:99"
	)

	// wantData holds the mac and timestamp of a mac binding
	type wantData struct {
		mac       string
		timestamp int
	}

	tests := []struct {
		name string
		// initial mac bindings
		initial          map[string]string
		initialTimestamp int
		// mac bindings to set
		set          map[string]string
		setTimestamp int
		// ports to set the mac bindings for
		targets []string
		// expect port->ip->mac,timestamp.
		want map[string]map[string]wantData
	}{
		{
			name:         "creates a row that does not yet exist",
			set:          map[string]string{ip: secondMAC},
			setTimestamp: 200,
			targets:      []string{portA},
			want:         map[string]map[string]wantData{portA: {ip: {secondMAC, 200}}},
		},
		{
			name:             "refreshes an unchanged row's timestamp when newer",
			initial:          map[string]string{ip: firstMAC},
			initialTimestamp: 200,
			set:              map[string]string{ip: firstMAC},
			setTimestamp:     201,
			targets:          []string{portA},
			want:             map[string]map[string]wantData{portA: {ip: {firstMAC, 201}}},
		},
		{
			name:             "skips a refresh whose timestamp is not newer",
			initial:          map[string]string{ip: firstMAC},
			initialTimestamp: 200,
			set:              map[string]string{ip: firstMAC},
			setTimestamp:     200,
			targets:          []string{portA},
			want:             map[string]map[string]wantData{portA: {ip: {firstMAC, 200}}},
		},
		{
			// The MAC comparison runs before the timestamp check, so a changed MAC
			// is written even when the timestamp is not newer (contrast the skip
			// case above, which is identical but for the unchanged MAC).
			name:             "writes a changed MAC even when the timestamp is not newer",
			initial:          map[string]string{ip: firstMAC},
			initialTimestamp: 200,
			set:              map[string]string{ip: secondMAC},
			setTimestamp:     200,
			targets:          []string{portA},
			want:             map[string]map[string]wantData{portA: {ip: {secondMAC, 200}}},
		},
		{
			name:         "mirrors every ip onto every port in one write",
			set:          map[string]string{ip: firstMAC, otherIP: otherMAC},
			setTimestamp: 200,
			targets:      []string{portA, portB},
			want: map[string]map[string]wantData{
				portA: {ip: {firstMAC, 200}, otherIP: {otherMAC, 200}},
				portB: {ip: {firstMAC, 200}, otherIP: {otherMAC, 200}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			// Seed any pre-existing rows through SBData rather than via
			// setDynamicMacBindings, so the update/skip path is exercised against a
			// precondition established independently of the method under test.
			// The seeded Datapath is never asserted; it just references the shared
			// datapath so the rows are well-formed.
			setup := libovsdbtest.TestSetup{
				IgnoreConstraints: true,
				SBData: []libovsdbtest.TestData{
					&sbdb.DatapathBinding{UUID: "tgt-dp"},
				},
			}
			for _, port := range tt.targets {
				for ip, mac := range tt.initial {
					setup.SBData = append(setup.SBData, &sbdb.MACBinding{
						LogicalPort: port,
						IP:          ip,
						MAC:         mac,
						Datapath:    "tgt-dp",
						Timestamp:   tt.initialTimestamp,
					})
				}
			}
			sbClient, cleanup, err := libovsdbtest.NewSBTestHarness(setup, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			defer cleanup.Cleanup()

			// The production SB client monitors the full MAC_Binding table, so
			// written rows reach the cache without the test installing its own
			// monitor (a second overlapping MAC_Binding monitor would make the
			// cache inconsistent).

			// Resolve the real datapath UUID assigned by the server; all ports
			// share it.
			var dps []*sbdb.DatapathBinding
			g.Expect(sbClient.List(context.Background(), &dps)).To(gomega.Succeed())
			g.Expect(dps).To(gomega.HaveLen(1))
			dpUUID := dps[0].UUID
			portToDatapath := map[string]string{}
			for _, port := range tt.targets {
				portToDatapath[port] = dpUUID
			}

			c := newTestController()
			c.sbClient = sbClient

			// setDynamicMacBindings is synchronous: on return the cache already
			// reflects the write (or, for the not-newer skip case, the lack of one).
			set := map[string]macTimestamp{}
			for ip, mac := range tt.set {
				set[ip] = macTimestamp{mac: mac, timestamp: tt.setTimestamp}
			}
			g.Expect(c.setDynamicMacBindings(set, portToDatapath)).To(gomega.Succeed())

			for port, want := range tt.want {
				got := map[string]wantData{}
				for _, mb := range macBindingsFor(g, sbClient, port) {
					got[mb.IP] = wantData{mb.MAC, mb.Timestamp}
				}
				g.Expect(got).To(gomega.Equal(want))
			}
		})
	}
}

// --- south-bound event handlers -----------------------------------------

// TestRegisterSouthBoundEventHandlers verifies the SB cache event handlers
// translate row changes into the right reconcile enqueue, and filter out rows
// the controller does not care about (a disabled IP family, an untracked source,
// or port bindings that are not tracked Gateway Router external ports).
func TestRegisterSouthBoundEventHandlers(t *testing.T) {
	// ipv4 enabled, ipv6 disabled
	gomega.NewWithT(t).Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.Gateway.DisableUDNARPNDPFlood = true
	cdnPort := cdnPortFor("node1")
	const (
		udnPort       = "rtoe-GR_tenantred_node1"
		trackedSource = "rtoe-GR_tracked_node1"
	)
	mbKey := cdnPort + keySep + "10.0.0.5"

	createMACBinding := func(g gomega.Gomega, sbClient libovsdbclient.Client) {
		createRow(g, sbClient, &sbdb.MACBinding{
			LogicalPort: cdnPort, IP: "10.0.0.5", MAC: "0a:00:00:00:00:05",
		})
	}
	createUDNPortBinding := func(g gomega.Gomega, sbClient libovsdbclient.Client) {
		createRow(g, sbClient, &sbdb.PortBinding{
			LogicalPort: udnPort,
			ExternalIDs: map[string]string{
				types.NetworkExternalID:  "tenantred",
				types.TopologyExternalID: types.Layer2Topology,
			},
		})
	}

	tests := []struct {
		name string
		// followers seeds c.followers (source -> followers) so the relevant
		// sources are tracked.
		followers map[string][]string
		// action performs the triggering SB mutation(s).
		action func(g gomega.Gomega, sbClient libovsdbclient.Client)
		// want* are keys expected on each reconciler (ContainElements); an empty
		// slice asserts the reconciler stays empty.
		wantNetwork []string
		wantMb      []string
		wantRefresh []string
		// assert, when set, replaces the default want-based assertions.
		assert func(g gomega.Gomega, network, mb, refresh *recorder)
	}{
		{
			name:        "PortBinding for a primary L2 GR external port enqueues its network",
			action:      createUDNPortBinding,
			wantNetwork: []string{"tenantred"},
		},
		{
			name:      "new MAC_Binding on a tracked source enqueues an update",
			followers: map[string][]string{cdnPort: {udnPort}},
			action:    createMACBinding,
			wantMb:    []string{mbKey},
		},
		{
			name:      "timestamp-only MAC_Binding change enqueues a refresh",
			followers: map[string][]string{cdnPort: {udnPort}},
			action: func(g gomega.Gomega, sbClient libovsdbclient.Client) {
				createMACBinding(g, sbClient)
				mb := &sbdb.MACBinding{LogicalPort: cdnPort, IP: "10.0.0.5"}
				g.Expect(sbClient.Get(context.Background(), mb)).To(gomega.Succeed())
				mb.Timestamp = 100
				updOps, err := sbClient.Where(mb).Update(mb, &mb.Timestamp)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = ops.TransactAndCheck(sbClient, updOps)
				g.Expect(err).NotTo(gomega.HaveOccurred())
			},
			// the create enqueues an update (a MAC not previously mirrored), the
			// timestamp bump a refresh.
			wantMb:      []string{mbKey},
			wantRefresh: []string{mbKey},
		},
		{
			name:      "PortBinding delete re-enqueues its network",
			followers: map[string][]string{cdnPort: {udnPort}},
			action: func(g gomega.Gomega, sbClient libovsdbclient.Client) {
				createUDNPortBinding(g, sbClient)
				delOps, err := sbClient.WhereCache(func(pb *sbdb.PortBinding) bool {
					return pb.LogicalPort == udnPort
				}).Delete()
				g.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = ops.TransactAndCheck(sbClient, delOps)
				g.Expect(err).NotTo(gomega.HaveOccurred())
			},
			// both the add and the delete enqueue "tenantred", so require the
			// network to be enqueued at least twice (the delete re-enqueue).
			assert: func(g gomega.Gomega, network, mb, refresh *recorder) {
				g.Eventually(func() int {
					n := 0
					for _, k := range network.got() {
						if k == "tenantred" {
							n++
						}
					}
					return n
				}).Should(gomega.BeNumerically(">=", 2))
				g.Consistently(mb.got, "200ms", "50ms").Should(gomega.BeEmpty())
				g.Consistently(refresh.got, "200ms", "50ms").Should(gomega.BeEmpty())
			},
		},
		{
			name:      "MAC_Binding on an untracked source is ignored",
			followers: map[string][]string{trackedSource: {"rtoe-GR_follower_node1"}},
			action: func(g gomega.Gomega, sbClient libovsdbclient.Client) {
				createRow(g, sbClient, &sbdb.MACBinding{
					LogicalPort: "rtoe-GR_untracked_node1", IP: "10.0.0.5", MAC: "0a:00:00:00:00:05",
				})
			},
		},
		{
			name:      "MAC_Binding for a disabled IP family is ignored",
			followers: map[string][]string{trackedSource: {"rtoe-GR_follower_node1"}},
			action: func(g gomega.Gomega, sbClient libovsdbclient.Client) {
				createRow(g, sbClient, &sbdb.MACBinding{
					LogicalPort: trackedSource, IP: "fd00::5", MAC: "0a:00:00:00:00:06",
				})
			},
		},
		{
			name: "PortBinding without the GR external port prefix is ignored",
			action: func(g gomega.Gomega, sbClient libovsdbclient.Client) {
				createRow(g, sbClient, &sbdb.PortBinding{
					LogicalPort: "sw-port", TunnelKey: 1,
					ExternalIDs: map[string]string{
						types.NetworkExternalID:  "tenantred",
						types.TopologyExternalID: types.Layer2Topology,
					},
				})
			},
		},
		{
			name: "PortBinding with a non-L2/L3 topology is ignored",
			action: func(g gomega.Gomega, sbClient libovsdbclient.Client) {
				createRow(g, sbClient, &sbdb.PortBinding{
					LogicalPort: "rtoe-GR_localnet_node1", TunnelKey: 2,
					ExternalIDs: map[string]string{
						types.NetworkExternalID:  "localnetnet",
						types.TopologyExternalID: "localnet",
					},
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			sbClient, cleanup, err := libovsdbtest.NewSBTestHarness(libovsdbtest.TestSetup{IgnoreConstraints: true}, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			defer cleanup.Cleanup()

			networkRec, networkR := startRecorder(g, "network")
			defer controller.Stop(networkR)
			mbRec, mbR := startRecorder(g, "mb")
			defer controller.Stop(mbR)
			refreshRec, refreshR := startRecorder(g, "refresh")
			defer controller.Stop(refreshR)

			c := newTestController()
			c.sbClient = sbClient
			c.cdnGatewayPort = cdnPort
			c.networkReconciler = networkR
			c.dynamicMacBindingReconciler = mbR
			c.dynamicMacBindingRefreshReconciler = refreshR
			for source, followers := range tt.followers {
				c.followers[source] = sets.New(followers...)
			}

			c.registerSouthBoundEventHandlers()

			tt.action(g, sbClient)

			if tt.assert != nil {
				tt.assert(g, networkRec, mbRec, refreshRec)
				return
			}

			expects := []struct {
				rec  *recorder
				want []string
			}{
				{networkRec, tt.wantNetwork},
				{mbRec, tt.wantMb},
				{refreshRec, tt.wantRefresh},
			}
			// First wait for the expected enqueues, then confirm the untouched
			// reconcilers stay empty.
			for _, e := range expects {
				if len(e.want) > 0 {
					g.Eventually(e.rec.got).Should(gomega.ContainElements(e.want))
				}
			}
			for _, e := range expects {
				if len(e.want) == 0 {
					g.Consistently(e.rec.got, "200ms", "50ms").Should(gomega.BeEmpty())
				}
			}
		})
	}
}

// --- small helpers -------------------------------------------------------

// macBindingsFor returns the MAC_Binding rows mirrored onto a logical port,
// read from the SB client cache.
func macBindingsFor(g gomega.Gomega, sbClient libovsdbclient.Client, port string) []*sbdb.MACBinding {
	var all []*sbdb.MACBinding
	g.Expect(sbClient.List(context.Background(), &all)).To(gomega.Succeed())
	var out []*sbdb.MACBinding
	for _, mb := range all {
		if mb.LogicalPort == port {
			out = append(out, mb)
		}
	}
	return out
}

// createRow inserts a single row into the SB database.
func createRow(g gomega.Gomega, sbClient libovsdbclient.Client, m model.Model) {
	createOps, err := sbClient.Create(m)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = ops.TransactAndCheck(sbClient, createOps)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

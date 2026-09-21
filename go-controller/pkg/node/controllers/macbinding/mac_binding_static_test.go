// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"testing"

	"github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

const (
	// nodeIPSource is the CDN GR external port, the designated source of the
	// default ("") uplink whose followers carry the node-IP static bindings.
	nodeIPSource = "rtoe-GR_node1"
)

// smb builds a static MAC binding row as the controller writes it
// (override_dynamic_mac always set). Expected rows carry no UUID and are matched
// with HaveDataIgnoringUUIDs.
func smb(port, ip, mac string) *nbdb.StaticMACBinding {
	return &nbdb.StaticMACBinding{LogicalPort: port, IP: ip, MAC: mac, OverrideDynamicMAC: true}
}

// TestSyncStaticMacBindingsToFollower verifies a follower is written the whole
// cached node-IP set for its source's uplink, and that a source with no known
// uplink writes nothing.
func TestSyncStaticMacBindingsToFollower(t *testing.T) {
	g := gomega.NewWithT(t)
	config.Gateway.DisableUDNARPNDPFlood = true
	const follower = "rtoe-GR_udnA_node1"

	nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer nbCleanup.Cleanup()

	c := newTestController()
	c.nbClient = nbClient
	c.cdnGatewayPort = nodeIPSource
	c.nodeIPs[""] = map[string]string{"10.0.0.10": nodeIPMAC1, "10.0.0.11": nodeIPMAC2}

	// The follower catches up on the whole set for its source's ("") uplink.
	g.Expect(c.syncStaticMacBindingsToFollower(nodeIPSource, follower)).To(gomega.Succeed())
	g.Eventually(c.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(
		smb(follower, "10.0.0.10", nodeIPMAC1),
		smb(follower, "10.0.0.11", nodeIPMAC2),
	))

	// A source that is not designated for any uplink is a no-op (no new rows).
	g.Expect(c.syncStaticMacBindingsToFollower("rtoe-GR_notasource_node1", "rtoe-GR_udnB_node1")).To(gomega.Succeed())
	g.Consistently(c.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(
		smb(follower, "10.0.0.10", nodeIPMAC1),
		smb(follower, "10.0.0.11", nodeIPMAC2),
	))
}

// TestDeleteStaticMacBindingsForFollower verifies that a port which stopped being
// a follower has every static binding reaped except the gateway masquerade IPs:
// a port that is no longer a follower owns nothing, so the node-IP cache is
// irrelevant and only the gateway-owned rows survive. The reap is scoped to the
// port, so other ports' bindings are untouched.
func TestDeleteStaticMacBindingsForFollower(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.Gateway.DisableUDNARPNDPFlood = true
	const (
		port    = "rtoe-GR_udnA_node1"
		nodeIP1 = "10.0.0.10"     // a node IP still in the cache
		nodeIP2 = "10.0.0.11"     // a node IP no longer in the cache
		otherIP = "10.1.0.10"     // an owned binding for a stale IP (reaped)
		masqIP  = "169.254.169.2" // the gateway's host masquerade IP (allow-listed)
		// other is a different port, whose binding the port-scoped reap must not touch.
		other       = "rtoe-GR_udnB_node1"
		otherPortIP = "10.0.0.20"
	)

	nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
		&nbdb.StaticMACBinding{UUID: "u1", LogicalPort: port, IP: nodeIP1, MAC: nodeIPMAC1, OverrideDynamicMAC: true},
		&nbdb.StaticMACBinding{UUID: "u2", LogicalPort: port, IP: nodeIP2, MAC: nodeIPMAC2, OverrideDynamicMAC: true},
		&nbdb.StaticMACBinding{UUID: "u3", LogicalPort: port, IP: otherIP, MAC: nodeIPMAC3, OverrideDynamicMAC: true},
		&nbdb.StaticMACBinding{UUID: "u4", LogicalPort: port, IP: masqIP, MAC: nodeIPMAC3, OverrideDynamicMAC: true},
		&nbdb.StaticMACBinding{UUID: "u5", LogicalPort: other, IP: otherPortIP, MAC: nodeIPMAC1, OverrideDynamicMAC: true},
	}}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer nbCleanup.Cleanup()

	c := newTestController()
	c.nbClient = nbClient
	// the cache is irrelevant for a removed follower: it owns nothing, so the
	// whole set except the gateway masquerade IP is reaped.
	c.nodeIPs[""] = map[string]string{nodeIP1: nodeIPMAC1}

	g.Expect(c.deleteStaticMacBindingsForFollower(port)).To(gomega.Succeed())
	// only the gateway masquerade row on port survives; the other port is untouched.
	g.Eventually(c.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(
		smb(port, masqIP, nodeIPMAC3),
		smb(other, otherPortIP, nodeIPMAC1),
	))
}

// TestRepairStaticMacBindings verifies the one-shot startup sweep reaps only
// controller-owned bindings on gateway external ports absent from the known
// (SBDB-validated) port set — i.e. networks that no longer exist — leaving
// bindings on known ports, the gateway's own masquerade IPs, and non-gateway
// ports untouched.
func TestRepairStaticMacBindings(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.Gateway.DisableUDNARPNDPFlood = true

	cdnPort := cdnPortFor("node1")
	const (
		udnPort    = "rtoe-GR_tenantred_node1" // a known network's port
		orphanPort = "rtoe-GR_gone_node1"      // a network deleted while we were down
		nonGWPort  = "some-lsp"                // not a gateway external port
	)
	masqIP := config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()

	nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
		// owned bindings on known networks' ports survive
		&nbdb.StaticMACBinding{UUID: "u1", LogicalPort: cdnPort, IP: "10.0.0.10", MAC: nodeIPMAC1, OverrideDynamicMAC: true},
		&nbdb.StaticMACBinding{UUID: "u2", LogicalPort: udnPort, IP: "10.0.0.11", MAC: nodeIPMAC2, OverrideDynamicMAC: true},
		// an owned binding on an orphan gateway port is reaped
		&nbdb.StaticMACBinding{UUID: "u3", LogicalPort: orphanPort, IP: "10.0.0.12", MAC: nodeIPMAC3, OverrideDynamicMAC: true},
		// the gateway's masquerade IP is not owned, even on the orphan port
		&nbdb.StaticMACBinding{UUID: "u4", LogicalPort: orphanPort, IP: masqIP, MAC: nodeIPMAC3, OverrideDynamicMAC: true},
		// a non-gateway port is not owned
		&nbdb.StaticMACBinding{UUID: "u5", LogicalPort: nonGWPort, IP: "10.0.0.13", MAC: nodeIPMAC1, OverrideDynamicMAC: true},
	}}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer nbCleanup.Cleanup()

	c := newTestController()
	c.nbClient = nbClient

	// the known ports the first full reconcile validated against SBDB.
	c.repairStaticMacBindings(sets.New(cdnPort, udnPort))

	// only the orphan network's owned binding is reaped.
	g.Eventually(c.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(
		smb(cdnPort, "10.0.0.10", nodeIPMAC1),
		smb(udnPort, "10.0.0.11", nodeIPMAC2),
		smb(orphanPort, masqIP, nodeIPMAC3),
		smb(nonGWPort, "10.0.0.13", nodeIPMAC1),
	))
}

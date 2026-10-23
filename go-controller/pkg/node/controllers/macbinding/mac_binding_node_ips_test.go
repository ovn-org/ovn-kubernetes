// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"fmt"
	"testing"

	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	factorymocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory/mocks"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	nodeIPMAC1 = "0a:00:00:00:00:01"
	nodeIPMAC2 = "0a:00:00:00:00:02"
	nodeIPMAC3 = "0a:00:00:00:00:03"
)

// nodeIPTestNode builds a fake node carrying the internal IPs and the
// l3-gateway-config gateway MAC the recompute reads. An empty mac leaves the node
// unannotated (nothing to bind); subnets sets the l3-gateway ip-addresses that,
// for the local node, bound which other nodes' IPs are kept.
func nodeIPTestNode(name, mac string, subnets []string, ips ...string) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, ip := range ips {
		node.Status.Addresses = append(node.Status.Addresses,
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
	}
	if mac != "" {
		subnetsJSON := ""
		for i, s := range subnets {
			if i > 0 {
				subnetsJSON += ","
			}
			subnetsJSON += fmt.Sprintf("%q", s)
		}
		node.Annotations = map[string]string{
			util.OvnNodeL3GatewayConfig: fmt.Sprintf(
				`{"default":{"mode":"shared","mac-address":%q,"ip-addresses":[%s]}}`, mac, subnetsJSON),
			util.OvnNodeChassisID: name + "-chassis",
		}
	}
	return node
}

// nodeIPWatchFactoryMock returns a NodeWatchFactory mock whose GetNode("node1")
// yields local (the source of the local uplink subnets) and whose GetNodes yields
// all.
func nodeIPWatchFactoryMock(local *corev1.Node, all ...*corev1.Node) *factorymocks.NodeWatchFactory {
	wf := &factorymocks.NodeWatchFactory{}
	wf.On("GetNode", "node1").Return(local, nil)
	wf.On("GetNodes").Return(all, nil)
	return wf
}

// TestSyncNodeIPsForUplink drives the node-event side of the node-IP statics end
// to end: the recompute walks the nodes and builds the uplink's ip->mac map
// (on-subnet IPs only, smallest MAC on a collision), then the per-IP applies
// (re)apply cached IPs on the followers and delete a vanished one. The two run
// together because the recompute writes nothing itself.
func TestSyncNodeIPsForUplink(t *testing.T) {
	g := gomega.NewWithT(t)
	const follower = "rtoe-GR_udnA_node1"

	local := nodeIPTestNode("node1", nodeIPMAC1, []string{"10.0.0.10/24"}, "10.0.0.10")
	node2 := nodeIPTestNode("node2", nodeIPMAC2, nil, "10.0.0.11", "fd00::11")  // v6 filtered (ipv4-only)
	offSubnet := nodeIPTestNode("node3", nodeIPMAC3, nil, "192.168.1.5")        // off-subnet, filtered
	unannotated := nodeIPTestNode("node4", "", nil, "10.0.0.12")                // no MAC, skipped
	collision := nodeIPTestNode("node5", "0a:00:00:00:00:99", nil, "10.0.0.11") // loses to node2's smaller MAC

	nbClient, nbCleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer nbCleanup.Cleanup()

	c := newTestController()
	c.nbClient = nbClient
	c.cdnGatewayPort = nodeIPSource
	// syncNodeIPsForUplink fans out per-IP reconciles onto the static reconciler;
	// the recorder is just a non-nil sink so that enqueue does not panic (this test
	// applies the IPs itself).
	_, r := startRecorder(g, "static")
	defer controller.Stop(r)
	c.staticMacBindingReconciler = r

	c.followers[nodeIPSource] = sets.New(follower)
	c.watchFactory = nodeIPWatchFactoryMock(local, local, node2, offSubnet, unannotated, collision)

	// Recompute: only the two on-subnet IPv4 IPs survive, node2 winning the
	// collision on 10.0.0.11 with the smaller MAC.
	g.Expect(c.syncNodeIPsForUplink("")).To(gomega.Succeed())
	g.Expect(c.getNodeIPMACs("")).To(gomega.Equal(map[string]string{
		"10.0.0.10": nodeIPMAC1,
		"10.0.0.11": nodeIPMAC2,
	}))

	// Apply the recomputed IPs onto the follower.
	for ip := range c.getNodeIPMACs("") {
		g.Expect(c.syncStaticMacBinding("", ip)).To(gomega.Succeed())
	}
	g.Eventually(c.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(
		smb(follower, "10.0.0.10", nodeIPMAC1),
		smb(follower, "10.0.0.11", nodeIPMAC2),
	))

	// node2 leaves the cluster: the recompute drops its IP, and applying that now
	// uncached IP deletes the follower's stale binding.
	c.watchFactory = nodeIPWatchFactoryMock(local, local)
	g.Expect(c.syncNodeIPsForUplink("")).To(gomega.Succeed())
	g.Expect(c.getNodeIPMACs("")).To(gomega.Equal(map[string]string{"10.0.0.10": nodeIPMAC1}))
	g.Expect(c.syncStaticMacBinding("", "10.0.0.11")).To(gomega.Succeed())
	g.Eventually(c.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(
		smb(follower, "10.0.0.10", nodeIPMAC1),
	))
}

// TestOnNodeIPsChange verifies the node-event filter that gates the recompute: a
// change to a node's internal IPs or its gateway MAC enqueues a recompute, while
// an unrelated node update does not.
func TestOnNodeIPsChange(t *testing.T) {
	subnets := []string{"10.0.0.10/24"}
	base := nodeIPTestNode("node1", nodeIPMAC1, subnets, "10.0.0.10")

	tests := []struct {
		name        string
		old         *corev1.Node
		new         *corev1.Node
		wantEnqueue bool
	}{
		{
			name:        "internal IP change enqueues a recompute",
			old:         base,
			new:         nodeIPTestNode("node1", nodeIPMAC1, subnets, "10.0.0.10", "10.0.0.99"),
			wantEnqueue: true,
		},
		{
			name:        "gateway MAC change enqueues a recompute",
			old:         base,
			new:         nodeIPTestNode("node1", nodeIPMAC2, subnets, "10.0.0.10"),
			wantEnqueue: true,
		},
		{
			// The local node's l3-gateway ip-addresses define the local subnets
			// that filter which node IPs get a binding, so a change must recompute
			// even when the status IPs and gateway MAC are unchanged.
			name:        "local node l3-gateway subnets change enqueues a recompute",
			old:         base,
			new:         nodeIPTestNode("node1", nodeIPMAC1, []string{"10.0.1.10/24"}, "10.0.0.10"),
			wantEnqueue: true,
		},
		{
			// A remote node's l3-gateway ip-addresses do not affect the local
			// subnets, so with unchanged status IPs and MAC it is ignored.
			name:        "remote node l3-gateway subnets change is ignored",
			old:         nodeIPTestNode("node2", nodeIPMAC2, subnets, "10.0.0.11"),
			new:         nodeIPTestNode("node2", nodeIPMAC2, []string{"10.0.1.10/24"}, "10.0.0.11"),
			wantEnqueue: false,
		},
		{
			name:        "an unrelated update is ignored",
			old:         base,
			new:         nodeIPTestNode("node1", nodeIPMAC1, subnets, "10.0.0.10"),
			wantEnqueue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			c := newTestController()
			rec, r := startRecorder(g, "node-ips")
			defer controller.Stop(r)
			c.nodeIPsReconciler = r

			c.onNodeIPsChange(tt.old, tt.new)

			if tt.wantEnqueue {
				// the recompute is enqueued under the "" (default uplink) key
				g.Eventually(rec.got).Should(gomega.ConsistOf(""))
			} else {
				g.Consistently(rec.got).Should(gomega.BeEmpty())
			}
		})
	}
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	ipgenerator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestSyncNodeIDsOutOfRange(t *testing.T) {
	for _, invalid := range []int{-2, types.MaxNodeIDs + 1, math.MaxInt} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			config.IPv4Mode = true
			invalidNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: "invalid", Annotations: map[string]string{util.OvnNodeID: strconv.Itoa(invalid)},
			}}
			healthyNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: "healthy", Annotations: map[string]string{util.OvnNodeID: "10"},
			}}
			client := util.GetOVNClientset(invalidNode, healthyNode).GetClusterManagerClientset()
			wf, err := factory.NewClusterManagerWatchFactory(client)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(wf.Shutdown)
			g.Expect(wf.Start()).To(gomega.Succeed())
			nac, err := newNodeAllocationController(client, wf)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(nac.Stop)
			g.Expect(nac.Start(context.Background())).To(gomega.Succeed())
			allocator := nac.nodeIDAllocator
			// Wait for the informer-driven handlers to persist both annotations.
			g.Eventually(func(g gomega.Gomega) {
				for _, name := range []string{"invalid", "healthy"} {
					node, getErr := client.KubeClient.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
					g.Expect(getErr).NotTo(gomega.HaveOccurred())
					nodeID, parseErr := util.GetNodeID(node)
					g.Expect(parseErr).NotTo(gomega.HaveOccurred())
					g.Expect(nodeID).To(gomega.BeNumerically(">=", 2))
					g.Expect(nodeID).To(gomega.BeNumerically("<=", types.MaxNodeIDs))
					g.Expect(nodeID).To(gomega.Equal(allocator.GetID(name)))
					g.Expect(node.Annotations[util.OvnTransitSwitchPortAddr]).NotTo(gomega.BeEmpty())
					if name == "healthy" {
						g.Expect(nodeID).To(gomega.Equal(10))
					}
				}
			}, 5*time.Second, 10*time.Millisecond).Should(gomega.Succeed())
			nodes := []interface{}{invalidNode, healthyNode}
			replacement := allocator.GetID("invalid")
			if replacement < 2 || replacement > types.MaxNodeIDs || replacement == 10 {
				t.Fatalf("invalid replacement: %d", replacement)
			}
			if allocator.GetID("healthy") != 10 {
				t.Fatal("healthy node ID changed")
			}
			if err = nac.syncNodeIDs(nodes); err != nil {
				t.Fatal(err)
			}
			if allocator.GetID("invalid") != replacement {
				t.Fatal("replacement changed on resync")
			}
			next, err := allocator.AllocateID("another")
			if err != nil || next == replacement || next == 10 {
				t.Fatalf("subsequent allocation: %d, %v", next, err)
			}
		})
	}
}

func TestNeedsNodeAllocation(t *testing.T) {
	oldHybridOverlay := config.HybridOverlay.Enabled
	config.HybridOverlay.Enabled = false
	t.Cleanup(func() { config.HybridOverlay.Enabled = oldHybridOverlay })
	for _, tc := range []struct {
		name, nodeID                    string
		missingID, missingTransit, want bool
	}{
		{name: "missing ID", missingID: true, want: true},
		{name: "empty ID", want: true},
		{name: "malformed ID", nodeID: "invalid", want: true},
		{name: "negative ID", nodeID: "-2", want: true},
		{name: "reserved zero ID", nodeID: "0", want: true},
		{name: "reserved one ID", nodeID: "1", want: true},
		{name: "ID above limit", nodeID: strconv.Itoa(types.MaxNodeIDs + 1), want: true},
		{name: "oversized ID", nodeID: "1099511627776", want: true},
		{name: "minimum valid ID", nodeID: strconv.Itoa(types.MinNodeID)},
		{name: "maximum valid ID", nodeID: strconv.Itoa(types.MaxNodeIDs)},
		{name: "missing transit address", nodeID: "2", missingTransit: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				util.OvnNodeID: tc.nodeID, util.OvnTransitSwitchPortAddr: "existing",
			}}}
			if tc.missingID {
				delete(node.Annotations, util.OvnNodeID)
			}
			if tc.missingTransit {
				delete(node.Annotations, util.OvnTransitSwitchPortAddr)
			}
			if got := needsNodeAllocation(node); got != tc.want {
				t.Fatalf("needsNodeAllocation() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestNodeAllocationUpdateRepairsInvalidID(t *testing.T) {
	oldV4, oldV6, oldHybridOverlay := config.IPv4Mode, config.IPv6Mode, config.HybridOverlay.Enabled
	config.IPv4Mode, config.IPv6Mode, config.HybridOverlay.Enabled = true, true, false
	t.Cleanup(func() {
		config.IPv4Mode, config.IPv6Mode, config.HybridOverlay.Enabled = oldV4, oldV6, oldHybridOverlay
	})
	for _, invalidID := range []string{"", "invalid", "-2", "0", "1", strconv.Itoa(types.MaxNodeIDs + 1), "1099511627776"} {
		t.Run(invalidID, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", Annotations: map[string]string{
				util.OvnNodeID: invalidID,
				// Both annotations must exist to reproduce the update being skipped.
				util.OvnTransitSwitchPortAddr: `{"ipv4":"100.88.0.99/16","ipv6":"fd97::99/64"}`,
			}}}
			client := fake.NewClientset(node)
			allocator := id.NewIDAllocator("NodeIDs", types.MaxNodeIDs+1)
			for name, reserved := range map[string]int{"zero": 0, "one": 1} {
				if err := allocator.ReserveID(name, reserved); err != nil {
					t.Fatalf("reserve system node ID %d: %v", reserved, err)
				}
			}
			v4, err := ipgenerator.NewIPGenerator("100.88.0.0/16")
			if err != nil {
				t.Fatalf("create IPv4 generator: %v", err)
			}
			v6, err := ipgenerator.NewIPGenerator("fd97::/64")
			if err != nil {
				t.Fatalf("create IPv6 generator: %v", err)
			}
			nac := &nodeAllocationController{kube: &kube.Kube{KClient: client}, nodeIDAllocator: allocator, transitSwitchIPv4Generator: v4, transitSwitchIPv6Generator: v6}
			handler := &nodeAllocationControllerEventHandler{objType: factory.NodeType, nac: nac}
			if err := nac.syncNodeIDs([]interface{}{node}); err != nil {
				t.Fatalf("restore node ID allocations: %v", err)
			}
			if err := handler.UpdateResource(node, node, false); err != nil {
				t.Fatalf("reconcile invalid node ID update: %v", err)
			}
			updated, err := client.CoreV1().Nodes().Get(context.Background(), node.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get reconciled node: %v", err)
			}
			assigned, err := util.GetNodeID(updated)
			if err != nil || assigned < types.MinNodeID || assigned > types.MaxNodeIDs {
				t.Fatalf("invalid replacement node ID %d: %v", assigned, err)
			}
			if got := allocator.GetID(node.Name); got != assigned {
				t.Fatalf("reserved node ID %d differs from annotation %d", got, assigned)
			}
			addresses, err := util.ParseNodeTransitSwitchPortAddrs(updated)
			if err != nil || len(addresses) != 2 {
				t.Fatalf("expected dual-stack transit addresses, got %v: %v", addresses, err)
			}
			for _, generator := range []*ipgenerator.IPGenerator{v4, v6} {
				expected, err := generator.GenerateIP(assigned)
				if err != nil {
					t.Fatalf("derive transit address from replacement ID: %v", err)
				}
				found := false
				for _, address := range addresses {
					if address.String() == expected.String() {
						found = true
					}
				}
				if !found {
					t.Fatalf("transit addresses %v do not contain replacement address %s", addresses, expected)
				}
			}
		})
	}
}

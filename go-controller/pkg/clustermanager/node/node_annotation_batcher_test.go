// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var (
	subnet1 = &net.IPNet{IP: net.ParseIP("10.1.0.0"), Mask: net.CIDRMask(24, 32)}
	subnet2 = &net.IPNet{IP: net.ParseIP("10.2.0.0"), Mask: net.CIDRMask(24, 32)}
	subnet3 = &net.IPNet{IP: net.ParseIP("10.3.0.0"), Mask: net.CIDRMask(24, 32)}
	subnet4 = &net.IPNet{IP: net.ParseIP("10.4.0.0"), Mask: net.CIDRMask(24, 32)}
	subnet5 = &net.IPNet{IP: net.ParseIP("10.5.0.0"), Mask: net.CIDRMask(24, 32)}
)

func newTestNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{},
		},
	}
}

func setupBatcherTest(t *testing.T, nodes ...*corev1.Node) (*NodeAnnotationBatcher, *fake.Clientset) {
	t.Helper()
	fakeClient := fake.NewSimpleClientset()

	for _, node := range nodes {
		_, err := fakeClient.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create node %s: %v", node.Name, err)
		}
	}

	kubeInterface := &kube.Kube{KClient: fakeClient}
	batcher := NewNodeAnnotationBatcher(kubeInterface)

	return batcher, fakeClient
}

func startBatcher(t *testing.T, batcher *NodeAnnotationBatcher) {
	t.Helper()
	if err := controller.Start(batcher.Reconciler()); err != nil {
		t.Fatalf("Failed to start batcher: %v", err)
	}
}

func stopBatcher(batcher *NodeAnnotationBatcher) {
	controller.Stop(batcher.Reconciler())
}

func TestNodeAnnotationBatcher_BasicBatching(t *testing.T) {
	// This test reproduces the race condition where successive reconcile
	// cycles for the same node read stale state and overwrite
	// annotations set by the previous reconcile.
	node1 := newTestNode("node1")
	node2 := newTestNode("node2")
	node3 := newTestNode("node3")

	batcher, fakeClient := setupBatcherTest(t, node1, node2, node3)
	startBatcher(t, batcher)
	defer stopBatcher(batcher)

	// Phase 1: first batch of updates — one network per node
	batcher.EnqueueUpdate("node1", "network1", []*net.IPNet{subnet1}, true, 1, types.NoTunnelID)
	batcher.EnqueueUpdate("node2", "network1", []*net.IPNet{subnet3}, true, 1, types.NoTunnelID)
	batcher.EnqueueUpdate("node3", "network1", []*net.IPNet{subnet5}, true, 1, types.NoTunnelID)

	// Phase 2: enqueue a second network for node1 and node2.
	// This triggers a new reconcile that must merge with the phase 1 annotations.
	batcher.EnqueueUpdate("node1", "network2", []*net.IPNet{subnet2}, true, 2, types.NoTunnelID)
	batcher.EnqueueUpdate("node2", "network3", []*net.IPNet{subnet4}, true, 3, types.NoTunnelID)

	g := gomega.NewWithT(t)
	g.Eventually(func() error {
		n1, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
		if err != nil {
			return err
		}
		subnets, err := util.ParseNodeHostSubnetsAnnotation(n1)
		if err != nil || len(subnets) != 2 {
			return fmt.Errorf("node1: waiting for 2 networks, got %d, err %v", len(subnets), err)
		}
		n2, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node2", metav1.GetOptions{})
		if err != nil {
			return err
		}
		subnets, err = util.ParseNodeHostSubnetsAnnotation(n2)
		if err != nil || len(subnets) != 2 {
			return fmt.Errorf("node2: waiting for 2 networks, got %d, err %v", len(subnets), err)
		}
		n3, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node3", metav1.GetOptions{})
		if err != nil {
			return err
		}
		subnets, err = util.ParseNodeHostSubnetsAnnotation(n3)
		if err != nil || len(subnets) != 1 {
			return fmt.Errorf("node3: waiting for 1 network, got %d, err %v", len(subnets), err)
		}
		return nil
	}, 2*time.Second, 10*time.Millisecond).Should(gomega.Succeed())

	// Verify node1 has both networks (network1 from phase 1 + network2 from phase 2)
	updatedNode1, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get node1: %v", err)
	}
	subnets1, err := util.ParseNodeHostSubnetsAnnotation(updatedNode1)
	if err != nil {
		t.Fatalf("Failed to parse node1 subnet annotation: %v", err)
	}
	if len(subnets1) != 2 {
		t.Fatalf("node1: expected 2 networks, got %d (phase 1 annotations likely overwritten by stale lister state)", len(subnets1))
	}
	if _, ok := subnets1["network1"]; !ok {
		t.Error("node1: network1 subnet not found — phase 1 annotation was overwritten")
	}
	if _, ok := subnets1["network2"]; !ok {
		t.Error("node1: network2 subnet not found")
	}

	networkIDs1, err := util.GetNodeNetworkIDsAnnotationNetworkIDs(updatedNode1)
	if err != nil {
		t.Fatalf("Failed to parse node1 network IDs: %v", err)
	}
	if networkIDs1["network1"] != 1 {
		t.Errorf("node1: expected network1 ID 1, got %d", networkIDs1["network1"])
	}
	if networkIDs1["network2"] != 2 {
		t.Errorf("node1: expected network2 ID 2, got %d", networkIDs1["network2"])
	}

	// Verify node2 has both networks (network1 from phase 1 + network3 from phase 2)
	updatedNode2, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node2", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get node2: %v", err)
	}
	subnets2, err := util.ParseNodeHostSubnetsAnnotation(updatedNode2)
	if err != nil {
		t.Fatalf("Failed to parse node2 subnet annotation: %v", err)
	}
	if len(subnets2) != 2 {
		t.Fatalf("node2: expected 2 networks, got %d (phase 1 annotations likely overwritten by stale lister state)", len(subnets2))
	}
	if _, ok := subnets2["network1"]; !ok {
		t.Error("node2: network1 subnet not found — phase 1 annotation was overwritten")
	}
	if _, ok := subnets2["network3"]; !ok {
		t.Error("node2: network3 subnet not found")
	}

	networkIDs2, err := util.GetNodeNetworkIDsAnnotationNetworkIDs(updatedNode2)
	if err != nil {
		t.Fatalf("Failed to parse node2 network IDs: %v", err)
	}
	if networkIDs2["network1"] != 1 {
		t.Errorf("node2: expected network1 ID 1, got %d", networkIDs2["network1"])
	}
	if networkIDs2["network3"] != 3 {
		t.Errorf("node2: expected network3 ID 3, got %d", networkIDs2["network3"])
	}

	// Verify node3 still has network1 from phase 1 (no phase 2 update)
	updatedNode3, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node3", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get node3: %v", err)
	}
	subnets3, err := util.ParseNodeHostSubnetsAnnotation(updatedNode3)
	if err != nil {
		t.Fatalf("Failed to parse node3 subnet annotation: %v", err)
	}
	if len(subnets3) != 1 {
		t.Fatalf("node3: expected 1 network, got %d", len(subnets3))
	}
	if _, ok := subnets3["network1"]; !ok {
		t.Error("node3: network1 subnet not found")
	}
}

func TestNodeAnnotationBatcher_UpdateMerging(t *testing.T) {
	node1 := newTestNode("node1")
	node2 := newTestNode("node2")

	batcher, fakeClient := setupBatcherTest(t, node1, node2)
	startBatcher(t, batcher)
	defer stopBatcher(batcher)

	batcher.EnqueueUpdate("node1", "blue", []*net.IPNet{subnet1}, true, 1, types.NoTunnelID)
	batcher.EnqueueUpdate("node1", "blue", []*net.IPNet{subnet2}, true, 2, types.NoTunnelID)
	batcher.EnqueueUpdate("node2", "blue", []*net.IPNet{subnet3}, true, 3, types.NoTunnelID)
	batcher.EnqueueUpdate("node2", "blue", []*net.IPNet{subnet4}, true, 4, types.NoTunnelID)

	g := gomega.NewWithT(t)
	g.Eventually(func() error {
		n1, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
		if err != nil {
			return err
		}
		ids, err := util.GetNodeNetworkIDsAnnotationNetworkIDs(n1)
		if err != nil || ids["blue"] != 2 {
			return fmt.Errorf("node1: waiting for blue network ID 2, got %d, err %v", ids["blue"], err)
		}
		n2, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node2", metav1.GetOptions{})
		if err != nil {
			return err
		}
		ids, err = util.GetNodeNetworkIDsAnnotationNetworkIDs(n2)
		if err != nil || ids["blue"] != 4 {
			return fmt.Errorf("node2: waiting for blue network ID 4, got %d, err %v", ids["blue"], err)
		}
		return nil
	}, 2*time.Second, 10*time.Millisecond).Should(gomega.Succeed())

	updatedNode1, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get node1: %v", err)
	}
	subnets1, err := util.ParseNodeHostSubnetsAnnotation(updatedNode1)
	if err != nil {
		t.Fatalf("Failed to parse node1 subnet annotation: %v", err)
	}
	blueSubnets1 := subnets1["blue"]
	if len(blueSubnets1) != 1 {
		t.Fatalf("node1: expected 1 subnet for blue, got %d", len(blueSubnets1))
	}
	if blueSubnets1[0].String() != subnet2.String() {
		t.Errorf("node1: expected subnet %s, got %s", subnet2.String(), blueSubnets1[0].String())
	}
	networkIDs1, err := util.GetNodeNetworkIDsAnnotationNetworkIDs(updatedNode1)
	if err != nil {
		t.Fatalf("Failed to parse node1 network IDs: %v", err)
	}
	if networkIDs1["blue"] != 2 {
		t.Errorf("node1: expected blue network ID 2, got %d", networkIDs1["blue"])
	}

	updatedNode2, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node2", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get node2: %v", err)
	}
	subnets2, err := util.ParseNodeHostSubnetsAnnotation(updatedNode2)
	if err != nil {
		t.Fatalf("Failed to parse node2 subnet annotation: %v", err)
	}
	blueSubnets2 := subnets2["blue"]
	if len(blueSubnets2) != 1 {
		t.Fatalf("node2: expected 1 subnet for blue, got %d", len(blueSubnets2))
	}
	if blueSubnets2[0].String() != subnet4.String() {
		t.Errorf("node2: expected subnet %s, got %s", subnet4.String(), blueSubnets2[0].String())
	}
	networkIDs2, err := util.GetNodeNetworkIDsAnnotationNetworkIDs(updatedNode2)
	if err != nil {
		t.Fatalf("Failed to parse node2 network IDs: %v", err)
	}
	if networkIDs2["blue"] != 4 {
		t.Errorf("node2: expected blue network ID 4, got %d", networkIDs2["blue"])
	}
}

func TestNodeAnnotationBatcher_StopProcessesPendingUpdates(t *testing.T) {
	node1 := newTestNode("node1")
	node2 := newTestNode("node2")

	batcher, fakeClient := setupBatcherTest(t, node1, node2)
	startBatcher(t, batcher)

	batcher.EnqueueUpdate("node1", "blue", []*net.IPNet{subnet1}, true, 1, types.NoTunnelID)
	batcher.EnqueueUpdate("node2", "red", []*net.IPNet{subnet2}, true, 2, types.NoTunnelID)

	// Stop drains the workqueue, processing pending items
	stopBatcher(batcher)

	updatedNode1, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get node1: %v", err)
	}
	subnets1, err := util.ParseNodeHostSubnetsAnnotation(updatedNode1)
	if err != nil {
		t.Fatalf("Failed to parse node1 subnet annotation: %v", err)
	}
	if _, ok := subnets1["blue"]; !ok {
		t.Error("node1: expected blue network subnet to be present after stop")
	}

	updatedNode2, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node2", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get node2: %v", err)
	}
	subnets2, err := util.ParseNodeHostSubnetsAnnotation(updatedNode2)
	if err != nil {
		t.Fatalf("Failed to parse node2 subnet annotation: %v", err)
	}
	if _, ok := subnets2["red"]; !ok {
		t.Error("node2: expected red network subnet to be present after stop")
	}
}

func TestNodeAnnotationBatcher_TunnelIDAnnotation(t *testing.T) {
	node1 := newTestNode("node1")
	node2 := newTestNode("node2")

	batcher, fakeClient := setupBatcherTest(t, node1, node2)
	startBatcher(t, batcher)
	defer stopBatcher(batcher)

	batcher.EnqueueUpdate("node1", "blue", []*net.IPNet{subnet1}, true, 1, 100)
	batcher.EnqueueUpdate("node1", "red", []*net.IPNet{subnet2}, true, 2, 200)
	batcher.EnqueueUpdate("node2", "blue", []*net.IPNet{subnet3}, true, 1, 300)

	g := gomega.NewWithT(t)
	g.Eventually(func() error {
		n1, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if id, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(n1, "blue"); err != nil || id != 100 {
			return fmt.Errorf("node1: waiting for blue tunnel ID 100, got %d, err %v", id, err)
		}
		if id, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(n1, "red"); err != nil || id != 200 {
			return fmt.Errorf("node1: waiting for red tunnel ID 200, got %d, err %v", id, err)
		}
		n2, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node2", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if id, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(n2, "blue"); err != nil || id != 300 {
			return fmt.Errorf("node2: waiting for blue tunnel ID 300, got %d, err %v", id, err)
		}
		return nil
	}, 2*time.Second, 10*time.Millisecond).Should(gomega.Succeed())

	updatedNode1, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get node1: %v", err)
	}
	tunnelID1Blue, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(updatedNode1, "blue")
	if err != nil {
		t.Fatalf("Failed to parse node1 blue tunnel ID: %v", err)
	}
	if tunnelID1Blue != 100 {
		t.Errorf("node1: expected blue tunnel ID 100, got %d", tunnelID1Blue)
	}
	tunnelID1Red, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(updatedNode1, "red")
	if err != nil {
		t.Fatalf("Failed to parse node1 red tunnel ID: %v", err)
	}
	if tunnelID1Red != 200 {
		t.Errorf("node1: expected red tunnel ID 200, got %d", tunnelID1Red)
	}

	updatedNode2, err := fakeClient.CoreV1().Nodes().Get(context.Background(), "node2", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get node2: %v", err)
	}
	tunnelID2Blue, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(updatedNode2, "blue")
	if err != nil {
		t.Fatalf("Failed to parse node2 blue tunnel ID: %v", err)
	}
	if tunnelID2Blue != 300 {
		t.Errorf("node2: expected blue tunnel ID 300, got %d", tunnelID2Blue)
	}
}

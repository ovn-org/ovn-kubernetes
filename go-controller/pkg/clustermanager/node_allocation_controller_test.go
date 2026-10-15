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

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestSyncNodeIDsOutOfRange(t *testing.T) {
	for _, invalid := range []int{-2, maxNodeIDs + 1, math.MaxInt} {
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
					g.Expect(nodeID).To(gomega.BeNumerically("<=", maxNodeIDs))
					g.Expect(nodeID).To(gomega.Equal(allocator.GetID(name)))
					g.Expect(node.Annotations[util.OvnTransitSwitchPortAddr]).NotTo(gomega.BeEmpty())
					if name == "healthy" {
						g.Expect(nodeID).To(gomega.Equal(10))
					}
				}
			}, 5*time.Second, 10*time.Millisecond).Should(gomega.Succeed())
			nodes := []interface{}{invalidNode, healthyNode}
			replacement := allocator.GetID("invalid")
			if replacement < 2 || replacement > maxNodeIDs || replacement == 10 {
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

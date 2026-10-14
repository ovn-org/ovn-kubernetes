// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"

	hotypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
)

var _ = ginkgo.Describe("Hybrid-overlay subnet claims", ginkgo.Label("HybridOverlay"), func() {
	f := wrappedTestFramework("hybrid-overlay-subnet")

	ginkgo.It("rejects a remote node claiming an arbitrary subnet", func() {
		if os.Getenv("OVN_HYBRID_OVERLAY_ENABLE") != "true" {
			ginkgo.Skip("hybrid overlay is not enabled")
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		const ipv4ClaimedSubnet = "198.19.0.0/24"
		const ipv6ClaimedSubnet = "2001:db8:198:19::/64"
		claimedSubnets := []string{ipv4ClaimedSubnet, ipv6ClaimedSubnet}
		remoteNodeName := "hybrid-overlay-subnet-" + framework.RandomSuffix()

		config := rest.CopyConfig(f.ClientConfig())
		config.Impersonate = rest.ImpersonationConfig{
			UserName: "system:node:" + remoteNodeName,
			Groups:   []string{"system:nodes", "system:authenticated"},
		}
		remoteNodeClient, err := kubernetes.NewForConfig(config)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, subnet := range claimedSubnets {
			maliciousNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: remoteNodeName,
				Annotations: map[string]string{
					hotypes.HybridOverlayNodeSubnet: subnet,
				},
			}}
			_, err = remoteNodeClient.CoreV1().Nodes().Create(ctx, maliciousNode, metav1.CreateOptions{})
			if err == nil {
				gomega.Expect(f.ClientSet.CoreV1().Nodes().Delete(ctx, remoteNodeName,
					metav1.DeleteOptions{})).To(gomega.Succeed())
			}
			gomega.Expect(apierrors.IsForbidden(err)).To(gomega.BeTrue(),
				"node subnet claim %s during node creation was not forbidden: %v", subnet, err)
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				"not allowed to modify the following controller-owned annotations")))
		}

		remoteNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: remoteNodeName,
			Labels: map[string]string{
				corev1.LabelOSStable: "windows",
			},
		}}
		_, err = f.ClientSet.CoreV1().Nodes().Create(ctx, remoteNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			gomega.Expect(f.ClientSet.CoreV1().Nodes().Delete(cleanupCtx, remoteNodeName,
				metav1.DeleteOptions{})).To(gomega.Succeed())
		})

		for _, subnet := range claimedSubnets {
			annotationPatch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`,
				hotypes.HybridOverlayNodeSubnet, subnet))
			for _, subresource := range []string{"", "status"} {
				_, err = remoteNodeClient.CoreV1().Nodes().Patch(ctx, remoteNodeName,
					types.MergePatchType, annotationPatch, metav1.PatchOptions{}, subresource)
				gomega.Expect(apierrors.IsForbidden(err)).To(gomega.BeTrue(),
					"node subnet claim %s through nodes/%s was not forbidden: %v", subnet, subresource, err)
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
					"not allowed to modify the following controller-owned annotations")))
			}
		}
	})
})

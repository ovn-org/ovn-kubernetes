// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
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
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"

	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("Node subnet claims", func() {
	f := wrappedTestFramework("node-subnet-claim")

	ginkgo.It("rejects a node claiming a sub-prefix of another node's subnet", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 1)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(nodes.Items).NotTo(gomega.BeEmpty())

		victimSubnets, err := util.ParseNodeHostSubnetAnnotation(&nodes.Items[0], ovntypes.DefaultNetworkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(victimSubnets).NotTo(gomega.BeEmpty())

		victimSubnet := victimSubnets[0]
		prefixLen, addressLen := victimSubnet.Mask.Size()
		gomega.Expect(prefixLen).To(gomega.BeNumerically("<", addressLen))
		claimedSubnet := &net.IPNet{
			IP:   victimSubnet.IP,
			Mask: net.CIDRMask(prefixLen+1, addressLen),
		}
		annotations, err := util.UpdateNodeHostSubnetAnnotation(nil, []*net.IPNet{claimedSubnet}, ovntypes.DefaultNetworkName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		remoteNodeName := "node-subnet-claim-" + framework.RandomSuffix()
		config := rest.CopyConfig(f.ClientConfig())
		config.Impersonate = rest.ImpersonationConfig{
			UserName: "system:node:" + remoteNodeName,
			Groups:   []string{"system:nodes", "system:authenticated"},
		}
		remoteNodeClient, err := kubernetes.NewForConfig(config)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		maliciousNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:        remoteNodeName,
			Annotations: annotations,
		}}
		_, err = remoteNodeClient.CoreV1().Nodes().Create(ctx, maliciousNode, metav1.CreateOptions{})
		if err == nil {
			gomega.Expect(f.ClientSet.CoreV1().Nodes().Delete(ctx, remoteNodeName,
				metav1.DeleteOptions{})).To(gomega.Succeed())
		}
		gomega.Expect(apierrors.IsForbidden(err)).To(gomega.BeTrue(),
			"node subnet claim during node creation was not forbidden: %v", err)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
			"not allowed to modify the following controller-owned annotations")))

		_, err = f.ClientSet.CoreV1().Nodes().Create(ctx, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: remoteNodeName},
		}, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			gomega.Expect(f.ClientSet.CoreV1().Nodes().Delete(cleanupCtx, remoteNodeName,
				metav1.DeleteOptions{})).To(gomega.Succeed())
		})

		annotationPatch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`,
			ovntypes.NodeSubnetsAnnotation, annotations[ovntypes.NodeSubnetsAnnotation]))
		for _, subresource := range []string{"", "status"} {
			_, err = remoteNodeClient.CoreV1().Nodes().Patch(ctx, remoteNodeName,
				types.MergePatchType, annotationPatch, metav1.PatchOptions{}, subresource)
			gomega.Expect(apierrors.IsForbidden(err)).To(gomega.BeTrue(),
				"node subnet claim through nodes/%s was not forbidden: %v", subresource, err)
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				"not allowed to modify the following controller-owned annotations")))
		}
	})
})

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
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

// With dynamic UDN, EgressFirewall metrics/CM rollup should only
// consider nodes where the primary UDN is rendered (pods scheduled there).
var _ = ginkgo.Describe("Status metrics mode dynamic UDN", feature.Metrics, feature.EgressFirewall, feature.NetworkSegmentation, func() {
	const (
		svcname                = "status-metrics-ef-udn"
		egressFirewallYamlFile = "egress-fw-status-metrics-udn.yml"
		udnName                = "primary-udn"
	)

	f := wrappedTestFramework(svcname)

	ginkgo.BeforeEach(func() {
		if !statusMetricsEnabled() {
			ginkgo.Skip("Status metrics mode is disabled (OVN_ENABLE_STATUS_METRICS != true)")
		}
		if !isNetworkSegmentationEnabled() {
			ginkgo.Skip("Network segmentation is disabled")
		}
		if !isDynamicUDNEnabled() {
			ginkgo.Skip("Dynamic UDN allocation is disabled")
		}
	})

	ginkgo.AfterEach(func() {
		_ = os.Remove(egressFirewallYamlFile)
		_, _ = e2ekubectl.RunKubectl(f.Namespace.Name, "delete", "egressfirewall", "default", "--ignore-not-found=true")
		_, _ = e2ekubectl.RunKubectl(f.Namespace.Name, "delete", "userdefinednetwork", udnName, "--ignore-not-found=true")
	})

	ginkgo.It("EgressFirewall on primary UDN has no per-node shards and summary when Prometheus is available", func() {
		nodes, err := e2enode.GetReadySchedulableNodes(context.TODO(), f.ClientSet)
		framework.ExpectNoError(err)
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 1), "need at least 2 nodes")

		// Label namespace for required UDN primary network attachment.
		ns, err := f.ClientSet.CoreV1().Namespaces().Get(context.TODO(), f.Namespace.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		ns.Labels[RequiredUDNNamespaceLabel] = ""
		_, err = f.ClientSet.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
		framework.ExpectNoError(err)

		cleanup, err := createManifest(f.Namespace.Name, newPrimaryUserDefinedNetworkManifest(f.ClientSet, udnName))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(cleanup)

		gomega.Eventually(func() string {
			out, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", "userdefinednetwork", udnName,
				"-o", "jsonpath={.status.conditions[?(@.type==\"NetworkCreated\")].status}")
			if err != nil {
				return ""
			}
			return out
		}, 2*time.Minute, 2*time.Second).Should(gomega.Equal("True"))

		// Schedule a pod on only one node so the UDN is rendered on a subset.
		targetNode := nodes.Items[0].Name
		pod := e2epod.NewAgnhostPod(f.Namespace.Name, "udn-client", nil, nil, nil)
		pod.Spec.NodeName = targetNode
		pod.Spec.Containers[0].Command = []string{"/agnhost", "pause"}
		_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		framework.ExpectNoError(e2epod.WaitTimeoutForPodRunningInNamespace(context.TODO(), f.ClientSet, pod.Name, f.Namespace.Name, 2*time.Minute))

		applyDefaultEgressFirewall(f.Namespace.Name, egressFirewallYamlFile)

		gomega.Consistently(func() error {
			return assertNoPerNodeEgressFirewallShards(f.Namespace.Name, "default")
		}, 10*time.Second, 500*time.Millisecond).Should(gomega.Succeed())

		if os.Getenv("OVN_STATUS_METRICS_PROMETHEUS_URL") == "" {
			framework.Logf("Prometheus URL not set; skipping coarse summary assertion")
			return
		}
		gomega.Eventually(func() string {
			output, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", "egressfirewall", "default",
				"-o", "jsonpath={.status.status}")
			if err != nil {
				return ""
			}
			return output
		}, 2*time.Minute, 5*time.Second).Should(gomega.Or(
			gomega.ContainSubstring("EgressFirewall Rules applied"),
			gomega.ContainSubstring("EgressFirewall Rules not correctly applied"),
		), fmt.Sprintf("expected coarse summary reflecting rendered-node set (pod on %s)", targetNode))
	})
})

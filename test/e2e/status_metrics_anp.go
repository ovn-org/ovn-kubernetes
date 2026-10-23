// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
)

var _ = ginkgo.Describe("Status metrics mode AdminNetworkPolicy", feature.Metrics, feature.AdminNetworkPolicy, func() {
	const (
		svcname     = "status-metrics-anp"
		anpYamlFile = "anp-status-metrics.yml"
		anpName     = "default"
	)
	f := wrappedTestFramework(svcname)

	ginkgo.BeforeEach(func() {
		if !statusMetricsEnabled() {
			ginkgo.Skip("Status metrics mode is disabled (OVN_ENABLE_STATUS_METRICS != true)")
		}
	})
	ginkgo.AfterEach(func() {
		_ = os.Remove(anpYamlFile)
		_, _ = e2ekubectl.RunKubectl("", "delete", "adminnetworkpolicy", anpName, "--ignore-not-found=true")
	})

	ginkgo.It("should not write Ready-In-Zone conditions", func() {
		cfg := fmt.Sprintf(`apiVersion: policy.networking.k8s.io/v1alpha1
kind: AdminNetworkPolicy
metadata:
  name: %s
spec:
  priority: 10
  subject:
    namespaces:
      matchLabels:
        kubernetes: %s
  ingress:
  - action: Allow
    from:
    - namespaces: {}
`, anpName, f.Namespace.Name)
		gomega.Expect(os.WriteFile(anpYamlFile, []byte(cfg), 0644)).To(gomega.Succeed())
		e2ekubectl.RunKubectlOrDie("", "apply", "-f", anpYamlFile)

		gomega.Consistently(func() error {
			return assertNoReadyInZoneConditions("", "adminnetworkpolicy", anpName)
		}, 10*time.Second, 500*time.Millisecond).Should(gomega.Succeed())
	})
})

var _ = ginkgo.Describe("Status metrics mode BaselineAdminNetworkPolicy", feature.Metrics, feature.BaselineNetworkPolicy, func() {
	const (
		svcname      = "status-metrics-banp"
		banpYamlFile = "banp-status-metrics.yml"
		banpName     = "default"
	)
	f := wrappedTestFramework(svcname)

	ginkgo.BeforeEach(func() {
		if !statusMetricsEnabled() {
			ginkgo.Skip("Status metrics mode is disabled (OVN_ENABLE_STATUS_METRICS != true)")
		}
	})
	ginkgo.AfterEach(func() {
		_ = os.Remove(banpYamlFile)
		_, _ = e2ekubectl.RunKubectl("", "delete", "baselineadminnetworkpolicy", banpName, "--ignore-not-found=true")
	})

	ginkgo.It("should not write Ready-In-Zone conditions", func() {
		_ = f // namespace reserved for future subject matchers
		cfg := fmt.Sprintf(`apiVersion: policy.networking.k8s.io/v1alpha1
kind: BaselineAdminNetworkPolicy
metadata:
  name: %s
spec:
  subject:
    namespaces: {}
  ingress:
  - action: Allow
    from:
    - namespaces: {}
`, banpName)
		gomega.Expect(os.WriteFile(banpYamlFile, []byte(cfg), 0644)).To(gomega.Succeed())
		e2ekubectl.RunKubectlOrDie("", "apply", "-f", banpYamlFile)

		gomega.Consistently(func() error {
			return assertNoReadyInZoneConditions("", "baselineadminnetworkpolicy", banpName)
		}, 10*time.Second, 500*time.Millisecond).Should(gomega.Succeed())
	})
})

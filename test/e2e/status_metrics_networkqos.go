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

var _ = ginkgo.Describe("Status metrics mode NetworkQoS", feature.Metrics, feature.NetworkQos, func() {
	const (
		svcname    = "status-metrics-nq"
		nqYamlFile = "network-qos-status-metrics.yml"
		nqName     = "default"
	)
	f := wrappedTestFramework(svcname)

	ginkgo.BeforeEach(func() {
		if !statusMetricsEnabled() {
			ginkgo.Skip("Status metrics mode is disabled (OVN_ENABLE_STATUS_METRICS != true)")
		}
		if os.Getenv("OVN_NETWORK_QOS_ENABLE") != "true" {
			ginkgo.Skip("NetworkQoS feature is disabled")
		}
	})
	ginkgo.AfterEach(func() {
		_ = os.Remove(nqYamlFile)
		_, _ = e2ekubectl.RunKubectl(f.Namespace.Name, "delete", "networkqos", nqName, "--ignore-not-found=true")
	})

	ginkgo.It("should not write Ready-In-Zone conditions", func() {
		cfg := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata:
  name: %s
  namespace: %s
spec:
  networkSelectors:
  - networkSelectionType: DefaultNetwork
  egress:
  - dscp: 50
`, nqName, f.Namespace.Name)
		gomega.Expect(os.WriteFile(nqYamlFile, []byte(cfg), 0644)).To(gomega.Succeed())
		e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "apply", "-f", nqYamlFile)

		gomega.Consistently(func() error {
			return assertNoReadyInZoneConditions(f.Namespace.Name, "networkqos", nqName)
		}, 10*time.Second, 500*time.Millisecond).Should(gomega.Succeed())
	})
})

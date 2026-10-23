// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
)

func statusMetricsEnabled() bool {
	return os.Getenv("OVN_ENABLE_STATUS_METRICS") == "true"
}

var _ = ginkgo.Describe("Status metrics mode", feature.Metrics, feature.EgressFirewall, func() {
	const (
		svcname                string = "status-metrics-ef"
		egressFirewallYamlFile string = "egress-fw-status-metrics.yml"
	)

	f := wrappedTestFramework(svcname)

	ginkgo.BeforeEach(func() {
		if !statusMetricsEnabled() {
			ginkgo.Skip("Status metrics mode is disabled (OVN_ENABLE_STATUS_METRICS != true)")
		}
	})

	ginkgo.AfterEach(func() {
		_ = os.Remove(egressFirewallYamlFile)
		_, _ = e2ekubectl.RunKubectl(f.Namespace.Name, "delete", "egressfirewall", "default", "--ignore-not-found=true")
	})

	ginkgo.It("EgressFirewall should not write per-node status shards", func() {
		applyDefaultEgressFirewall(f.Namespace.Name, egressFirewallYamlFile)

		gomega.Consistently(func() error {
			return assertNoPerNodeEgressFirewallShards(f.Namespace.Name, "default")
		}, 10*time.Second, 500*time.Millisecond).Should(gomega.Succeed())
	})

	ginkgo.It("EgressFirewall should get a coarse CM summary when Prometheus is available", func() {
		if os.Getenv("OVN_STATUS_METRICS_PROMETHEUS_URL") == "" {
			ginkgo.Skip("Prometheus URL not configured (OVN_STATUS_METRICS_PROMETHEUS_URL empty)")
		}
		applyDefaultEgressFirewall(f.Namespace.Name, egressFirewallYamlFile)

		gomega.Eventually(func() string {
			output, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", "egressfirewall", "default",
				"-o", "jsonpath={.status.status}")
			if err != nil {
				return ""
			}
			return output
		}, 2*time.Minute, 5*time.Second).Should(gomega.ContainSubstring("EgressFirewall Rules applied"))
	})
})

func applyDefaultEgressFirewall(namespace, yamlFile string) {
	config := fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: 1.2.3.4/24
`, namespace)
	gomega.Expect(os.WriteFile(yamlFile, []byte(config), 0644)).To(gomega.Succeed())
	e2ekubectl.RunKubectlOrDie(namespace, "apply", "-f", yamlFile)
}

func assertNoPerNodeEgressFirewallShards(namespace, name string) error {
	raw, err := e2ekubectl.RunKubectl(namespace, "get", "egressfirewall", name, "-o", "json")
	if err != nil {
		return err
	}
	var obj struct {
		Status struct {
			Messages []string `json:"messages"`
		} `json:"status"`
		ManagedFields []metav1.ManagedFieldsEntry `json:"managedFields"`
	}
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return err
	}
	if len(obj.Status.Messages) > 0 {
		return fmt.Errorf("expected no per-node messages[], got %v", obj.Status.Messages)
	}
	nodesRaw, err := e2ekubectl.RunKubectl("", "get", "nodes", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return err
	}
	nodeNames := strings.Fields(nodesRaw)
	for _, mf := range obj.ManagedFields {
		if mf.Subresource != "status" {
			continue
		}
		for _, node := range nodeNames {
			if mf.Manager == node {
				return fmt.Errorf("found per-node status managedFields for node %q", node)
			}
		}
	}
	return nil
}

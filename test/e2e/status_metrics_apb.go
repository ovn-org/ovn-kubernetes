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

var _ = ginkgo.Describe("Status metrics mode APBExternalRoute", feature.Metrics, feature.ExternalGateway, func() {
	const (
		svcname     = "status-metrics-apb"
		apbYamlFile = "apb-status-metrics.yml"
		apbName     = "default"
	)
	f := wrappedTestFramework(svcname)

	ginkgo.BeforeEach(func() {
		if !statusMetricsEnabled() {
			ginkgo.Skip("Status metrics mode is disabled (OVN_ENABLE_STATUS_METRICS != true)")
		}
	})
	ginkgo.AfterEach(func() {
		_ = os.Remove(apbYamlFile)
		_, _ = e2ekubectl.RunKubectl("", "delete", "adminpolicybasedexternalroute", apbName, "--ignore-not-found=true")
	})

	ginkgo.It("should not write per-node status messages", func() {
		cfg := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: AdminPolicyBasedExternalRoute
metadata:
  name: %s
spec:
  from:
    namespaceSelector:
      matchLabels:
        kubernetes: %s
  nextHops:
    static:
    - ip: "172.18.0.1"
`, apbName, f.Namespace.Name)
		gomega.Expect(os.WriteFile(apbYamlFile, []byte(cfg), 0644)).To(gomega.Succeed())
		e2ekubectl.RunKubectlOrDie("", "apply", "-f", apbYamlFile)

		gomega.Consistently(func() error {
			return assertNoPerNodeAPBMessages(apbName)
		}, 10*time.Second, 500*time.Millisecond).Should(gomega.Succeed())
	})
})

func assertNoPerNodeAPBMessages(name string) error {
	raw, err := e2ekubectl.RunKubectl("", "get", "adminpolicybasedexternalroute", name, "-o", "json")
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
	for _, mf := range obj.ManagedFields {
		if mf.Subresource != "status" {
			continue
		}
		for _, node := range strings.Fields(nodesRaw) {
			if mf.Manager == node {
				return fmt.Errorf("found per-node status managedFields for node %q", node)
			}
		}
	}
	return nil
}

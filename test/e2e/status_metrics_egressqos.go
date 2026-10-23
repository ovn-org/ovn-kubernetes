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

var _ = ginkgo.Describe("Status metrics mode EgressQoS", feature.Metrics, feature.EgressQos, func() {
	const (
		svcname     = "status-metrics-eq"
		eqYamlFile  = "egress-qos-status-metrics.yml"
		defaultName = "default"
	)
	f := wrappedTestFramework(svcname)

	ginkgo.BeforeEach(func() {
		if !statusMetricsEnabled() {
			ginkgo.Skip("Status metrics mode is disabled (OVN_ENABLE_STATUS_METRICS != true)")
		}
	})
	ginkgo.AfterEach(func() {
		_ = os.Remove(eqYamlFile)
		_, _ = e2ekubectl.RunKubectl(f.Namespace.Name, "delete", "egressqos", defaultName, "--ignore-not-found=true")
	})

	ginkgo.It("should not write Ready-In-Zone conditions", func() {
		cfg := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: EgressQoS
metadata:
  name: %s
  namespace: %s
spec:
  egress:
  - dscp: 40
`, defaultName, f.Namespace.Name)
		gomega.Expect(os.WriteFile(eqYamlFile, []byte(cfg), 0644)).To(gomega.Succeed())
		e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "apply", "-f", eqYamlFile)

		gomega.Consistently(func() error {
			return assertNoReadyInZoneConditions(f.Namespace.Name, "egressqos", defaultName)
		}, 10*time.Second, 500*time.Millisecond).Should(gomega.Succeed())
	})
})

func assertNoReadyInZoneConditions(namespace, resource, name string) error {
	raw, err := e2ekubectl.RunKubectl(namespace, "get", resource, name, "-o", "json")
	if err != nil {
		return err
	}
	var obj struct {
		Status struct {
			Conditions []metav1.Condition `json:"conditions"`
		} `json:"status"`
		ManagedFields []metav1.ManagedFieldsEntry `json:"managedFields"`
	}
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return err
	}
	for _, c := range obj.Status.Conditions {
		if strings.HasPrefix(c.Type, "Ready-In-Zone-") {
			return fmt.Errorf("found per-node condition %q", c.Type)
		}
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

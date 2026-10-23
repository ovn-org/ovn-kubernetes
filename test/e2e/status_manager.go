// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
)

var _ = ginkgo.Describe("Status manager validation", feature.EgressFirewall, func() {
	const (
		svcname string = "status-manager"
	)

	f := wrappedTestFramework(svcname)

	ginkgo.BeforeEach(func() {
		// create EgressFirewall
		var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: 1.2.3.4/24
`, f.Namespace.Name)
		framework.Logf("Applying EgressFirewall configuration: %s", egressFirewallConfig)
		// apply the egress firewall configuration
		e2ekubectl.RunKubectlOrDieInput(f.Namespace.Name, egressFirewallConfig, "apply", "-f", "-")
		// check status
		checkEgressFirewallStatus(f.Namespace.Name, false, true, true)
	})

	ginkgo.It("Should validate the egress firewall status when adding a new node zone", func() {
		newNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "new-node",
			},
		}
		_, err := f.ClientSet.CoreV1().Nodes().Create(context.TODO(), newNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			err := f.ClientSet.CoreV1().Nodes().Delete(context.TODO(), newNode.Name, metav1.DeleteOptions{})
			gomega.Expect(err == nil || apierrors.IsNotFound(err)).To(gomega.BeTrue())
		}()
		// check status is unset
		checkEgressFirewallStatus(f.Namespace.Name, true, false, true)
		// delete node
		err = f.ClientSet.CoreV1().Nodes().Delete(context.TODO(), newNode.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// check status is back
		checkEgressFirewallStatus(f.Namespace.Name, false, true, true)
	})
})

func checkEgressFirewallStatus(namespace string, empty bool, success bool, eventually bool) {
	checkStatus := func() bool {
		output, err := e2ekubectl.RunKubectl(namespace, "get", "egressfirewall", "default", "-o", "jsonpath={.status.status}")
		if err != nil {
			framework.Failf("could not get the egressfirewall default in namespace: %s", namespace)
		}
		return empty && output == "" || success && strings.Contains(output, "EgressFirewall Rules applied")
	}
	if eventually {
		gomega.Eventually(checkStatus, 5*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
	} else {
		gomega.Consistently(checkStatus, 5*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
	}
}

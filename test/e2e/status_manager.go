package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
)

var _ = ginkgo.Describe("Status manager validation", func() {
	const (
		svcname                string = "status-manager"
		egressFirewallYamlFile string = "egress-fw.yml"
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
		// write the config to a file for application and defer the removal
		if err := os.WriteFile(egressFirewallYamlFile, []byte(egressFirewallConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		// create the CRD config parameters
		applyArgs := []string{
			"apply",
			fmt.Sprintf("--namespace=%s", f.Namespace.Name),
			"-f",
			egressFirewallYamlFile,
		}
		framework.Logf("Applying EgressFirewall configuration: %s ", applyArgs)
		// apply the egress firewall configuration
		e2ekubectl.RunKubectlOrDie(f.Namespace.Name, applyArgs...)
		// check status
		checkEgressFirewallStatus(f.Namespace.Name, false, true, true)
	})

	ginkgo.AfterEach(func() {
		if err := os.Remove(egressFirewallYamlFile); err != nil {
			framework.Logf("Unable to remove the CRD config from disk: %v", err)
		}
	})

	ginkgo.It("Should validate the egress firewall status when adding an unknown zone", func() {
		// add unknown node
		newNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "new-node",
			},
		}
		_, err := f.ClientSet.CoreV1().Nodes().Create(context.TODO(), newNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			err := f.ClientSet.CoreV1().Nodes().Delete(context.TODO(), newNode.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()
		// check status not changed
		checkEgressFirewallStatus(f.Namespace.Name, false, true, false)
	})

	ginkgo.It("Should validate the egress firewall status when adding a new zone", func() {
		// add node with new zone
		newNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "new-node",
				Annotations: map[string]string{"k8s.ovn.org/zone-name": "new-zone"},
			},
		}
		_, err := f.ClientSet.CoreV1().Nodes().Create(context.TODO(), newNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			if err != nil {
				err := f.ClientSet.CoreV1().Nodes().Delete(context.TODO(), newNode.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
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
		gomega.Eventually(checkStatus, 1*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
	} else {
		gomega.Consistently(checkStatus, 1*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
	}
}

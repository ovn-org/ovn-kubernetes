package e2e

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("OVS CPU affinity pinning", func() {

	f := wrappedTestFramework("ovspinning")

	ginkgo.It("can be enabled on specific nodes by creating enable_dynamic_cpu_affinity file", func() {

		nodeWithEnabledOvsAffinityPinning := "ovn-worker2"

		_, err := runCommand(containerRuntime, "exec", nodeWithEnabledOvsAffinityPinning, "bash", "-c", "echo 1 > /etc/openvswitch/enable_dynamic_cpu_affinity")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		restartOVNKubeNodePodsInParallel(f.ClientSet, ovnNamespace, "ovn-worker", "ovn-worker2")

		enabledNodeLogs, err := getOVNKubePodLogsFiltered(f.ClientSet, ovnNamespace, "ovn-worker2", ".*ovspinning_linux.go.*$")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		gomega.Expect(enabledNodeLogs).To(gomega.ContainSubstring("Starting OVS daemon CPU pinning"))

		disabledNodeLogs, err := getOVNKubePodLogsFiltered(f.ClientSet, ovnNamespace, "ovn-worker", ".*ovspinning_linux.go.*$")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())
		gomega.Expect(disabledNodeLogs).To(gomega.ContainSubstring("OVS CPU affinity pinning disabled"))
	})
})

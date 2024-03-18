package diagnostics

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
)

func (d *Diagnostics) OVSFlowsDumpingDaemonSet(iface string) {
	if !d.ovsflows {
		return
	}
	By("Creating OVS flows dumping daemonsets")
	daemonSets := []appsv1.DaemonSet{}
	daemonSetName := fmt.Sprintf("dump-ovs-flows-%s", iface)
	cmd := composePeriodicCmd("ovs-ofctl dump-flows "+iface, 10)
	daemonSets = append(daemonSets, d.composeDiagnosticsDaemonSet(daemonSetName, cmd, "ovs-flows"))
	Expect(d.runDaemonSets(daemonSets)).To(Succeed())
}

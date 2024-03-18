package diagnostics

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
)

func (d *Diagnostics) ConntrackDumpingDaemonSet() {
	if !d.conntrack {
		return
	}
	By("Creating conntrack dumping daemonsets")
	daemonSets := []appsv1.DaemonSet{}
	daemonSetName := fmt.Sprintf("dump-conntrack")
	cmd := composePeriodicCmd("conntrack -L", 10)
	daemonSets = append(daemonSets, d.composeDiagnosticsDaemonSet(daemonSetName, cmd, "conntrack"))
	Expect(d.runDaemonSets(daemonSets)).To(Succeed())
}

package diagnostics

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
)

func (d *Diagnostics) TCPDumpDaemonSet(ifaces []string, expression string) {
	if !d.tcpdump {
		return
	}
	By("Creating tcpdump daemonsets")
	daemonSets := []appsv1.DaemonSet{}
	for _, iface := range ifaces {
		daemonSetName := "node-tcpdump-" + strings.ReplaceAll(iface, "_", "-")
		cmd := fmt.Sprintf("tcpdump -vvv -nne -i %s %s", iface, expression)
		daemonSets = append(daemonSets, d.composeDiagnosticsDaemonSet(daemonSetName, cmd, "node-tcpdump"))
	}
	Expect(d.runDaemonSets(daemonSets)).To(Succeed())
}

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
)

// These smoke tests guard what the metrics unit tests structurally cannot: that
// the OVN/OVS metric endpoints actually serve their expected series against real
// daemons (catching parser-vs-real-output drift) and that the metrics servers
// are wired up in a real deployment. They deliberately assert only presence of a
// representative subset, not exact values, to stay stable across environments.
var _ = ginkgo.Describe("e2e OVN/OVS metrics", feature.Metrics, func() {
	const (
		ovnMetricsPort = 9476 // --ovn-metrics-bind-address, see dist/images/ovnkube.sh
		ovsMetricsPort = 9310 // ovs-metrics exporter
	)

	f := wrappedTestFramework("metrics")

	var (
		ovnNamespace string
		nodePod      corev1.Pod
		ovnMetrics   = []string{
			"ovn_northd_status",
			"ovn_northd_nb_connection_status",
			"ovn_northd_sb_connection_status",
			"ovn_controller_integration_bridge_openflow_total",
			"ovn_controller_integration_bridge_patch_ports",
			"ovn_controller_integration_bridge_geneve_ports",
			"ovn_controller_southbound_database_connected",
			"ovn_db_db_size_bytes",
		}
		ovsMetrics = []string{
			"ovs_build_info",
			"ovs_vswitchd_dp_flows_total",
		}
	)

	selectNodePod := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
		})
		framework.ExpectNoError(err, "listing ovnkube-node pods")
		gomega.Expect(pods.Items).NotTo(gomega.BeEmpty(), "no ovnkube-node pods found")
		nodePod = pods.Items[0]
		gomega.Expect(nodePod.Status.PodIP).NotTo(gomega.BeEmpty(), "ovnkube-node pod has no IP")
	}

	ginkgo.BeforeEach(func() {
		ovnNamespace = deploymentconfig.Get().OVNKubernetesNamespace()
		selectNodePod()
	})

	// scrapeNoError curls a metrics endpoint and returns the response body or error,
	// allowing caller to decide whether to assert or retry. Used by Eventually().
	scrapeNoError := func(container string, port int) (string, error) {
		url := "http://" + net.JoinHostPort(nodePod.Status.PodIP, strconv.Itoa(port)) + "/metrics"
		stdout, stderr, err := ExecCommandInContainerWithFullOutput(
			f, ovnNamespace, nodePod.Name, container,
			"curl", "-sS", "--max-time", "10", url,
		)
		if err != nil {
			return "", fmt.Errorf("curl %s failed: %s", url, stderr)
		}
		if stdout == "" {
			return "", fmt.Errorf("empty metrics response from %s", url)
		}
		return stdout, nil
	}

	// scrape is a convenience wrapper that asserts success, for non-retrying contexts.
	scrape := func(container string, port int) string {
		body, err := scrapeNoError(container, port)
		framework.ExpectNoError(err)
		return body
	}

	assertPresent := func(body string, names ...string) {
		for _, name := range names {
			// Match the metric name at the start of a sample line, with or
			// without a label set, e.g. `name 1` or `name{db_name="..."} 1`.
			present := false
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"{") {
					present = true
					break
				}
			}
			gomega.Expect(present).To(gomega.BeTrue(), "expected metric %q missing from endpoint", name)
		}
	}

	ginkgo.It("delivers OVN and OVS metrics with periodic collection", func() {
		assertPresent(scrape("ovnkube-controller", ovnMetricsPort), ovnMetrics...)
		assertPresent(scrape("ovs-metrics-exporter", ovsMetricsPort), ovsMetrics...)
	})

	ginkgo.It("delivers the same metrics after restarting ovnkube-node with on-demand collection", ginkgo.Serial, func() {
		const intervalEnv = "OVN_METRICS_COLLECTION_INTERVAL"
		containers := []string{getNodeContainerName(), "ovs-metrics-exporter"}

		setCollectionInterval := func(values map[string]string, forceRestart bool) {
			needsUpdate := forceRestart
			if !needsUpdate {
				for _, container := range containers {
					if getTemplateContainerEnv(ovnNamespace, "daemonset/ovnkube-node", container, intervalEnv) != values[container] {
						needsUpdate = true
						break
					}
				}
			}
			if !needsUpdate {
				return
			}

			err := updateAndWaitForRollout(f.ClientSet, ovnNamespace, "daemonset/ovnkube-node", 0, rolloutTimeout, func() {
				for _, container := range containers {
					value := values[container]
					setting := intervalEnv + "-"
					if value != "" {
						setting = intervalEnv + "=" + value
					}
					e2ekubectl.RunKubectlOrDie(
						ovnNamespace,
						"set", "env", "daemonset/ovnkube-node",
						"-c", container, setting,
					)
				}
				if forceRestart {
					e2ekubectl.RunKubectlOrDie(
						ovnNamespace,
						"rollout", "restart", "daemonset/ovnkube-node",
					)
				}
			})
			framework.ExpectNoError(err, "updating metrics collection interval")
		}

		ginkgo.By("Getting the original collection interval values")
		originalValues := make(map[string]string, len(containers))
		for _, container := range containers {
			originalValues[container] = getTemplateContainerEnv(
				ovnNamespace, "daemonset/ovnkube-node", container, intervalEnv,
			)
		}
		ginkgo.DeferCleanup(func() {
			setCollectionInterval(originalValues, false)
		})

		ginkgo.By(fmt.Sprintf("Setting the metrics collection interval to 0 for all containers in Pod: %s", nodePod.Name))
		setCollectionInterval(map[string]string{
			getNodeContainerName(): "0",
			"ovs-metrics-exporter": "0",
		}, true)
		selectNodePod()

		ginkgo.By("Verifying that containers are using on-demand metrics collection")
		for _, container := range containers {
			stdout, stderr, err := ExecCommandInContainerWithFullOutput(
				f, ovnNamespace, nodePod.Name, container,
				"sh", "-c", `for file in /proc/[0-9]*/cmdline; do tr '\000' ' ' < "${file}" 2>/dev/null; echo; done`,
			)
			framework.ExpectNoError(err, "reading process command lines from container %s: %s", container, stderr)
			gomega.Expect(stdout).To(gomega.ContainSubstring("--metrics-collection-interval=0"),
				"container %s is not using on-demand metrics collection", container)
		}

		ginkgo.By("Verifying metrics endpoints still serve the expected metrics after restart")
		assertPresent(scrape(getNodeContainerName(), ovnMetricsPort), ovnMetrics...)
		assertPresent(scrape("ovs-metrics-exporter", ovsMetricsPort), ovsMetrics...)
	})

	ginkgo.It("keeps the endpoint responsive to repeated scrapes", func() {
		// The scrape handler only serializes the in-memory registry (extraction
		// runs on its own loop), so repeated back-to-back scrapes must all return
		// promptly and non-empty regardless of daemon load.
		for i := 0; i < 5; i++ {
			gomega.Eventually(func(g gomega.Gomega) {
				body, err := scrapeNoError("ovnkube-controller", ovnMetricsPort)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(body).To(gomega.ContainSubstring("ovn_northd_status"))
			}).Within(20 * time.Second).ProbeEvery(time.Second).Should(gomega.Succeed())
		}
	})
})

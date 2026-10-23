// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	. "github.com/onsi/gomega"
)

func TestResourceSyncStatusMetrics(t *testing.T) {
	g := NewWithT(t)
	RegisterResourceSyncStatusMetrics()

	SetEgressFirewallSyncSucceeded("node1", "ns1", "ef1", true)
	SetEgressFirewallSyncSucceeded("node1", "ns1", "ef1", false)
	metric, err := metricEgressFirewallSyncSucceeded.GetMetricWithLabelValues("node1", "ns1", "ef1")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gaugeValue(metric)).To(Equal(0.0))

	DeleteEgressFirewallSyncSucceeded("node1", "ns1", "ef1")
	_, err = metricEgressFirewallSyncSucceeded.GetMetricWithLabelValues("node1", "ns1", "ef1")
	// After delete, a new series may be created on Get; ensure DeleteLabelValues does not panic
	g.Expect(err).NotTo(HaveOccurred())

	SetAdminNetworkPolicySyncSucceeded("node1", "anp1", true)
	m, err := metricAdminNetworkPolicySyncSucceeded.GetMetricWithLabelValues("node1", "anp1")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gaugeValue(m)).To(Equal(1.0))
	DeleteAdminNetworkPolicySyncSucceeded("node1", "anp1")
}

func gaugeValue(g prometheus.Gauge) float64 {
	var m dto.Metric
	_ = g.Write(&m)
	return m.GetGauge().GetValue()
}

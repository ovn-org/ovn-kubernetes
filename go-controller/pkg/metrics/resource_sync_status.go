// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// Metric names for per-node resource sync outcomes.
// Full Prometheus names are ovnkube_<name> (namespace ovnkube, no subsystem).
const (
	MetricEgressFirewallSyncSucceeded                = "egressfirewall_sync_succeeded"
	MetricAdminNetworkPolicySyncSucceeded            = "adminnetworkpolicy_sync_succeeded"
	MetricBaselineAdminNetworkPolicySyncSucceeded    = "baselineadminnetworkpolicy_sync_succeeded"
	MetricEgressQoSSyncSucceeded                     = "egressqos_sync_succeeded"
	MetricNetworkQoSSyncSucceeded                    = "networkqos_sync_succeeded"
	MetricAdminPolicyBasedExternalRouteSyncSucceeded = "adminpolicybasedexternalroute_sync_succeeded"
)

var (
	registerResourceSyncStatusOnce sync.Once

	metricEgressFirewallSyncSucceeded = newNamespacedSyncGauge(
		MetricEgressFirewallSyncSucceeded,
		"Whether the EgressFirewall was successfully synced on this node (1=success, 0=failure).",
		true,
	)
	metricAdminNetworkPolicySyncSucceeded = newNamespacedSyncGauge(
		MetricAdminNetworkPolicySyncSucceeded,
		"Whether the AdminNetworkPolicy was successfully synced on this node (1=success, 0=failure).",
		false,
	)
	metricBaselineAdminNetworkPolicySyncSucceeded = newNamespacedSyncGauge(
		MetricBaselineAdminNetworkPolicySyncSucceeded,
		"Whether the BaselineAdminNetworkPolicy was successfully synced on this node (1=success, 0=failure).",
		false,
	)
	metricEgressQoSSyncSucceeded = newNamespacedSyncGauge(
		MetricEgressQoSSyncSucceeded,
		"Whether the EgressQoS was successfully synced on this node (1=success, 0=failure).",
		true,
	)
	metricNetworkQoSSyncSucceeded = newNamespacedSyncGauge(
		MetricNetworkQoSSyncSucceeded,
		"Whether the NetworkQoS was successfully synced on this node (1=success, 0=failure).",
		true,
	)
	metricAPBExternalRouteSyncSucceeded = newNamespacedSyncGauge(
		MetricAdminPolicyBasedExternalRouteSyncSucceeded,
		"Whether the AdminPolicyBasedExternalRoute was successfully synced on this node (1=success, 0=failure).",
		false,
	)
)

func newNamespacedSyncGauge(name, help string, namespaced bool) *prometheus.GaugeVec {
	labels := []string{"node", "name"}
	if namespaced {
		labels = []string{"node", "namespace", "name"}
	}
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: types.MetricOvnkubeNamespace,
		Name:      name,
		Help:      help,
	}, labels)
}

// RegisterResourceSyncStatusMetrics registers the per-node sync-succeeded gauges.
// Safe to call multiple times.
func RegisterResourceSyncStatusMetrics() {
	registerResourceSyncStatusOnce.Do(func() {
		prometheus.MustRegister(
			metricEgressFirewallSyncSucceeded,
			metricAdminNetworkPolicySyncSucceeded,
			metricBaselineAdminNetworkPolicySyncSucceeded,
			metricEgressQoSSyncSucceeded,
			metricNetworkQoSSyncSucceeded,
			metricAPBExternalRouteSyncSucceeded,
		)
	})
}

func setNamespacedSync(vec *prometheus.GaugeVec, node, namespace, name string, succeeded bool) {
	val := 0.0
	if succeeded {
		val = 1.0
	}
	vec.WithLabelValues(node, namespace, name).Set(val)
}

func deleteNamespacedSync(vec *prometheus.GaugeVec, node, namespace, name string) {
	vec.DeleteLabelValues(node, namespace, name)
}

func setClusterScopedSync(vec *prometheus.GaugeVec, node, name string, succeeded bool) {
	val := 0.0
	if succeeded {
		val = 1.0
	}
	vec.WithLabelValues(node, name).Set(val)
}

func deleteClusterScopedSync(vec *prometheus.GaugeVec, node, name string) {
	vec.DeleteLabelValues(node, name)
}

// SetEgressFirewallSyncSucceeded sets the EgressFirewall sync gauge for a node.
func SetEgressFirewallSyncSucceeded(node, namespace, name string, succeeded bool) {
	setNamespacedSync(metricEgressFirewallSyncSucceeded, node, namespace, name, succeeded)
}

// DeleteEgressFirewallSyncSucceeded removes the EgressFirewall sync gauge label set.
func DeleteEgressFirewallSyncSucceeded(node, namespace, name string) {
	deleteNamespacedSync(metricEgressFirewallSyncSucceeded, node, namespace, name)
}

// SetAdminNetworkPolicySyncSucceeded sets the AdminNetworkPolicy sync gauge for a node.
func SetAdminNetworkPolicySyncSucceeded(node, name string, succeeded bool) {
	setClusterScopedSync(metricAdminNetworkPolicySyncSucceeded, node, name, succeeded)
}

// DeleteAdminNetworkPolicySyncSucceeded removes the AdminNetworkPolicy sync gauge label set.
func DeleteAdminNetworkPolicySyncSucceeded(node, name string) {
	deleteClusterScopedSync(metricAdminNetworkPolicySyncSucceeded, node, name)
}

// SetBaselineAdminNetworkPolicySyncSucceeded sets the BANP sync gauge for a node.
func SetBaselineAdminNetworkPolicySyncSucceeded(node, name string, succeeded bool) {
	setClusterScopedSync(metricBaselineAdminNetworkPolicySyncSucceeded, node, name, succeeded)
}

// DeleteBaselineAdminNetworkPolicySyncSucceeded removes the BANP sync gauge label set.
func DeleteBaselineAdminNetworkPolicySyncSucceeded(node, name string) {
	deleteClusterScopedSync(metricBaselineAdminNetworkPolicySyncSucceeded, node, name)
}

// SetEgressQoSSyncSucceeded sets the EgressQoS sync gauge for a node.
func SetEgressQoSSyncSucceeded(node, namespace, name string, succeeded bool) {
	setNamespacedSync(metricEgressQoSSyncSucceeded, node, namespace, name, succeeded)
}

// DeleteEgressQoSSyncSucceeded removes the EgressQoS sync gauge label set.
func DeleteEgressQoSSyncSucceeded(node, namespace, name string) {
	deleteNamespacedSync(metricEgressQoSSyncSucceeded, node, namespace, name)
}

// SetNetworkQoSSyncSucceeded sets the NetworkQoS sync gauge for a node.
func SetNetworkQoSSyncSucceeded(node, namespace, name string, succeeded bool) {
	setNamespacedSync(metricNetworkQoSSyncSucceeded, node, namespace, name, succeeded)
}

// DeleteNetworkQoSSyncSucceeded removes the NetworkQoS sync gauge label set.
func DeleteNetworkQoSSyncSucceeded(node, namespace, name string) {
	deleteNamespacedSync(metricNetworkQoSSyncSucceeded, node, namespace, name)
}

// SetAPBExternalRouteSyncSucceeded sets the APBExternalRoute sync gauge for a node.
func SetAPBExternalRouteSyncSucceeded(node, name string, succeeded bool) {
	setClusterScopedSync(metricAPBExternalRouteSyncSucceeded, node, name, succeeded)
}

// DeleteAPBExternalRouteSyncSucceeded removes the APBExternalRoute sync gauge label set.
func DeleteAPBExternalRouteSyncSucceeded(node, name string) {
	deleteClusterScopedSync(metricAPBExternalRouteSyncSucceeded, node, name)
}

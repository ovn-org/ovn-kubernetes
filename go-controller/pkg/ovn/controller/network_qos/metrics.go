// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

var (
	nqosCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "num_network_qoses",
			Help:      "The total number of network qoses in the cluster",
		},
	)

	nqosOvnOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "nqos_ovn_operation_duration_ms",
			Help:      "Time spent on reconciling a NetworkQoS event",
			Buckets:   prometheus.ExponentialBuckets(.1, 2, 8),
		},
		[]string{"operation"},
	)

	nqosReconcileDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "nqos_creation_duration_ms",
			Help:      "Time spent on reconciling a NetworkQoS event",
			Buckets:   prometheus.ExponentialBuckets(.1, 2, 8),
		},
	)

	nqosDeletionDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "nqos_deletion_duration_ms",
			Help:      "Time spent on removing a NetworkQoS from a network",
			Buckets:   prometheus.ExponentialBuckets(.1, 2, 8),
		},
	)

	nqosNamespaceReconcileDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "nqos_ns_reconcile_duration_ms",
			Help:      "Time spent on reconciling a Namespace change for the NetworkQoSes it affects",
			Buckets:   prometheus.ExponentialBuckets(.1, 2, 8),
		},
	)

	nqosStatusPatchDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "nqos_status_patch_duration_ms",
			Help:      "Time spent on patching the status of a NetworkQoS",
		},
	)
)

func init() {
	prometheus.MustRegister(
		nqosCount,
		nqosOvnOperationDuration,
		nqosReconcileDuration,
		nqosDeletionDuration,
		nqosNamespaceReconcileDuration,
		nqosStatusPatchDuration,
	)
}

// Count the NetworkQoSes applied on at least one network.
func (c *Controller) updateNetworkQoSCount() {
	c.countMutex.Lock()
	defer c.countMutex.Unlock()
	count := 0
	for _, key := range c.cache.GetKeys() {
		entry, loaded := c.cache.Load(key)
		if loaded && appliedNetworks(entry).Len() > 0 {
			count++
		}
	}
	nqosCount.Set(float64(count))
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func recordNetworkQoSReconcileDuration(duration time.Duration) {
	nqosReconcileDuration.Observe(milliseconds(duration))
}

func recordNetworkQoSDeletionDuration(duration time.Duration) {
	nqosDeletionDuration.Observe(milliseconds(duration))
}

func recordNamespaceReconcileDuration(duration time.Duration) {
	nqosNamespaceReconcileDuration.Observe(milliseconds(duration))
}

func recordOvnOperationDuration(operationType string, duration time.Duration) {
	nqosOvnOperationDuration.WithLabelValues(operationType).Observe(milliseconds(duration))
}

func recordStatusPatchDuration(duration time.Duration) {
	nqosStatusPatchDuration.Observe(milliseconds(duration))
}

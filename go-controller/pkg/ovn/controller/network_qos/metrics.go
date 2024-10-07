package networkqos

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// Descriptors used by the NQOSControllerCollector below.
var (
	nqosCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.MetricOvnkubeNamespace,
			Subsystem: metrics.MetricOvnkubeSubsystemController,
			Name:      "num_network_qoses",
			Help:      "The total number of network qoses in the cluster",
		},
		[]string{"network"},
	)

	nqosOvnOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metrics.MetricOvnkubeNamespace,
			Subsystem: metrics.MetricOvnkubeSubsystemController,
			Name:      "nqos_ovn_operation_duration_ms",
			Help:      "Time spent on reconciling a NetworkQoS event",
			Buckets:   prometheus.ExponentialBuckets(.1, 2, 15),
		},
		[]string{"operation"},
	)

	nqosReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metrics.MetricOvnkubeNamespace,
			Subsystem: metrics.MetricOvnkubeSubsystemController,
			Name:      "nqos_creation_duration_ms",
			Help:      "Time spent on reconciling a NetworkQoS event",
			Buckets:   prometheus.ExponentialBuckets(.1, 2, 15),
		},
		[]string{"network"},
	)

	nqosPodReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metrics.MetricOvnkubeNamespace,
			Subsystem: metrics.MetricOvnkubeSubsystemController,
			Name:      "nqos_deletion_duration_ms",
			Help:      "Time spent on reconciling a Pod event",
			Buckets:   prometheus.ExponentialBuckets(.1, 2, 15),
		},
		[]string{"network"},
	)

	nqosNamespaceReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metrics.MetricOvnkubeNamespace,
			Subsystem: metrics.MetricOvnkubeSubsystemController,
			Name:      "nqos_ns_reconcile_duration_ms",
			Help:      "Time spent on reconciling Namespace change for all Pods related to NetworkQoSes",
			Buckets:   prometheus.ExponentialBuckets(.1, 2, 15),
		},
		[]string{"network"},
	)
)

func init() {
	prometheus.MustRegister(
		nqosCount,
		nqosOvnOperationDuration,
		nqosReconcileDuration,
		nqosPodReconcileDuration,
		nqosNamespaceReconcileDuration,
	)
}

func (c *Controller) teardownMetricsCollector() {
	prometheus.Unregister(nqosCount)
}

// UpdateEgressFirewallRuleCount records the number of Egress firewall rules.
func updateNetworkQoSCount(network string, count int) {
	nqosCount.WithLabelValues(network).Set(float64(count))
}

func recordNetworkQoSReconcileDuration(network string, duration int64) {
	nqosReconcileDuration.WithLabelValues(network).Observe(float64(duration))
}

func recordPodReconcileDuration(network string, duration int64) {
	nqosPodReconcileDuration.WithLabelValues(network).Observe(float64(duration))
}

func recordNamespaceReconcileDuration(network string, duration int64) {
	nqosNamespaceReconcileDuration.WithLabelValues(network).Observe(float64(duration))
}

func recordOvnOperationDuration(operationType string, duration int64) {
	nqosOvnOperationDuration.WithLabelValues(operationType).Observe(float64(duration))
}

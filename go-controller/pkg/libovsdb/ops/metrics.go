// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const (
	metricSubsystemLibovsdb = "libovsdb"

	txnResultSuccess = "success"
	txnResultError   = "error"
)

// These metrics are defined here rather than in pkg/metrics because pkg/metrics
// imports this package, so the dependency cannot go the other way.
var (
	metricTxnDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: types.MetricOvnkubeNamespace,
		Subsystem: metricSubsystemLibovsdb,
		Name:      "txn_duration_seconds",
		Help: "Time taken by an OVSDB transaction, including any reconnect retries, " +
			"labeled by database and outcome.",
		Buckets: prometheus.ExponentialBuckets(.001, 2, 16), // 1ms to ~32s
	}, []string{"db", "result"})

	metricTxnOps = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: types.MetricOvnkubeNamespace,
		Subsystem: metricSubsystemLibovsdb,
		Name:      "txn_ops",
		Help:      "Number of operations submitted in a single OVSDB transaction, labeled by database.",
		Buckets:   prometheus.ExponentialBuckets(1, 2, 15), // 1 to ~16k
	}, []string{"db"})

	registerTransactMetricsOnce sync.Once
)

// RegisterTransactMetrics registers the OVSDB transaction metrics with the
// default Prometheus registry. It is safe to call from more than one component
// in the same process.
func RegisterTransactMetrics() {
	registerTransactMetricsOnce.Do(func() {
		prometheus.MustRegister(metricTxnDuration)
		prometheus.MustRegister(metricTxnOps)
	})
}

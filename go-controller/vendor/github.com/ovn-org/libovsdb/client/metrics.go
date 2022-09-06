package client

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const libovsdbName = "libovsdb"

type metrics struct {
	numUpdates      *prometheus.CounterVec
	numTableUpdates *prometheus.CounterVec
	numDisconnects  prometheus.Counter
	numMonitors     prometheus.Gauge
	registerOnce    sync.Once
}

func (m *metrics) init(modelName string, namespace, subsystem string) {
	// labels that are the same across all metrics
	constLabels := prometheus.Labels{"primary_model": modelName}

	if namespace == "" {
		namespace = libovsdbName
		subsystem = ""
	}

	m.numUpdates = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Subsystem:   subsystem,
			Name:        "update_messages_total",
			Help:        "Count of libovsdb monitor update messages processed, partitioned by database",
			ConstLabels: constLabels,
		},
		[]string{"database"},
	)

	m.numTableUpdates = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Subsystem:   subsystem,
			Name:        "table_updates_total",
			Help:        "Count of libovsdb monitor update messages per table",
			ConstLabels: constLabels,
		},
		[]string{"database", "table"},
	)

	m.numDisconnects = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Subsystem:   subsystem,
			Name:        "disconnects_total",
			Help:        "Count of libovsdb disconnects encountered",
			ConstLabels: constLabels,
		},
	)

	m.numMonitors = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Subsystem:   subsystem,
			Name:        "monitors",
			Help:        "Number of running libovsdb ovsdb monitors",
			ConstLabels: constLabels,
		},
	)
}

func (m *metrics) register(r prometheus.Registerer) {
	m.registerOnce.Do(func() {
		r.MustRegister(
			m.numUpdates,
			m.numTableUpdates,
			m.numDisconnects,
			m.numMonitors,
		)
	})
}

func (o *ovsdbClient) registerMetrics() {
	if !o.options.shouldRegisterMetrics || o.options.registry == nil {
		return
	}
	o.metrics.register(o.options.registry)
	o.options.shouldRegisterMetrics = false
}

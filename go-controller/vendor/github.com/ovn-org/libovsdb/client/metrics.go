package client

import "github.com/prometheus/client_golang/prometheus"

const namespace = "libovsdb"

type metrics struct {
	numUpdates      *prometheus.CounterVec
	numTableUpdates *prometheus.CounterVec
	numDisconnects  prometheus.Counter
	numMonitors     prometheus.Gauge
}

func (m *metrics) init(modelName string) {
	// labels that are the same across all metrics
	constLabels := prometheus.Labels{"primary_model": modelName}

	m.numUpdates = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "update_messages_total",
			Help:        "Count of monitor update messages processed, partitioned by database",
			ConstLabels: constLabels,
		},
		[]string{"database"},
	)

	m.numTableUpdates = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "table_updates_total",
			Help:        "Count of monitor update messages per table",
			ConstLabels: constLabels,
		},
		[]string{"database", "table"},
	)

	m.numDisconnects = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "disconnects_total",
			Help:        "Count of disconnects encountered",
			ConstLabels: constLabels,
		},
	)

	m.numMonitors = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "monitors",
			Help:        "Number of running ovsdb monitors",
			ConstLabels: constLabels,
		},
	)

}

func (m *metrics) register(r prometheus.Registerer) {
	r.MustRegister(
		m.numUpdates,
		m.numTableUpdates,
		m.numDisconnects,
		m.numMonitors,
	)
}

func (o *ovsdbClient) registerMetrics() {
	if !o.options.shouldRegisterMetrics || o.options.registry == nil {
		return
	}
	o.metrics.register(o.options.registry)
	o.options.shouldRegisterMetrics = false
}

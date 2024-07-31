package networkqos

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// Descriptors used by the NQOSControllerCollector below.
var (
	nqosCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: metrics.MetricOvnkubeNamespace,
		Subsystem: metrics.MetricOvnkubeSubsystemController,
		Name:      "num_network_qoses",
		Help:      "The total number of network qoses in the cluster"},
	)
)

func (c *Controller) setupMetricsCollector() {
	prometheus.MustRegister(nqosCount)
}

func (c *Controller) teardownMetricsCollector() {
	prometheus.Unregister(nqosCount)
}

// UpdateEgressFirewallRuleCount records the number of Egress firewall rules.
func UpdateNetworkQoSCount(count float64) {
	nqosCount.Add(count)
}

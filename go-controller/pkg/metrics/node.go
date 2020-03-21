package metrics

import (
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog"
)

// MetricCNIRequestDuration is a prometheus metric that tracks the duration
// of CNI requests
var MetricCNIRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemNode,
	Name:      "cni_request_duration_seconds",
	Help:      "The duration of CNI server requests",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
	//labels
	[]string{"command", "err"},
)

var MetricNodeReadyDuration = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemNode,
	Name:      "ready_duration_seconds",
	Help:      "The duration for the node to get to ready state",
})

var registerNodeMetricsOnce sync.Once

func RegisterNodeMetrics() {
	registerNodeMetricsOnce.Do(func() {
		prometheus.MustRegister(MetricCNIRequestDuration)
		prometheus.MustRegister(MetricNodeReadyDuration)
		prometheus.MustRegister(prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Namespace: MetricOvnkubeNamespace,
				Subsystem: MetricOvnkubeSubsystemNode,
				Name:      "integration_bridge_openflow_total",
				Help:      "The total number of OpenFlow flows in the integration bridge",
			}, func() float64 {
				stdout, stderr, err := util.RunOVSOfctl("-t", "5", "dump-aggregate", "br-int")
				if err != nil {
					klog.Errorf("failed to get flow count for br-int, stderr(%s): (%v)",
						stderr, err)
					return 0
				}
				for _, kvPair := range strings.Fields(stdout) {
					if strings.HasPrefix(kvPair, "flow_count=") {
						count, _ := strconv.ParseFloat(strings.Split(kvPair, "=")[1], 64)
						return count
					}
				}
				return 0
			}))
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: MetricOvnkubeNamespace,
				Subsystem: MetricOvnkubeSubsystemNode,
				Name:      "build_info",
				Help: "A metric with a constant '1' value labeled by version, revision, branch, " +
					"and go version from which ovnkube was built and when and who built it",
				ConstLabels: prometheus.Labels{
					"version":    "0.0",
					"revision":   Commit,
					"branch":     Branch,
					"build_user": BuildUser,
					"build_date": BuildDate,
					"goversion":  runtime.Version(),
				},
			},
			func() float64 { return 1 },
		))
	})
}

package metrics

import (
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"time"

	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog"
)

// metricE2ETimestamp is a timestamp value we have persisted to nbdb. We will
// also export a metric with the same column in sbdb. We will also bump this
// every 30 seconds, so we can detect a hung northd.
var metricE2ETimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "nb_e2e_timestamp",
	Help:      "The current e2e-timestamp value as written to the northbound database"})

// metricPodCreationLatency is the time between a pod being scheduled and the
// ovn controller setting the network annotations.
var metricPodCreationLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "pod_creation_latency_seconds",
	Help:      "The latency between pod creation and setting the OVN annotations",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15),
})

// metricPodCreationLatency is the time between a pod being scheduled and the
// ovn controller setting the network annotations.
var metricOvnCliLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "ovn_cli_latency_seconds",
	Help:      "The latency of various OVN commands. Currently, ovn-nbctl and ovn-sbctl",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
	// labels
	[]string{"command"},
)

var MetricMasterReadyDuration = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "ready_duration_seconds",
	Help:      "The duration for the master to get to ready state",
})

var registerMasterMetricsOnce sync.Once
var startMasterUpdaterOnce sync.Once

// RegisterMasterMetrics registers some ovnkube master metrics with the Prometheus
// registry
func RegisterMasterMetrics() {
	registerMasterMetricsOnce.Do(func() {
		prometheus.MustRegister(metricE2ETimestamp)
		// following go routine is directly responsible for collecting the metric above.
		startMasterMetricsUpdater()

		prometheus.MustRegister(metricPodCreationLatency)
		prometheus.MustRegister(prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Namespace: MetricOvnkubeNamespace,
				Subsystem: MetricOvnkubeSubsystemMaster,
				Name:      "sb_e2e_timestamp",
				Help:      "The current e2e-timestamp value as observed in the southbound database",
			}, scrapeOvnTimestamp))
		prometheus.MustRegister(prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Namespace: MetricOvnkubeNamespace,
				Subsystem: MetricOvnkubeSubsystemMaster,
				Name:      "skipped_nbctl_daemon_total",
				Help:      "The number of times we skipped using ovn-nbctl daemon and directly interacted with OVN NB DB",
			}, func() float64 {
				return float64(util.SkippedNbctlDaemonCounter)
			}))
		prometheus.MustRegister(MetricMasterReadyDuration)
		prometheus.MustRegister(metricOvnCliLatency)
		// this is to not to create circular import between metrics and util package
		util.MetricOvnCliLatency = metricOvnCliLatency
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: MetricOvnkubeNamespace,
				Subsystem: MetricOvnkubeSubsystemMaster,
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

func scrapeOvnTimestamp() float64 {
	output, stderr, err := util.RunOVNSbctl("--if-exists",
		"get", "SB_Global", ".", "options:e2e_timestamp")
	if err != nil {
		klog.Errorf("failed to scrape timestamp: %s (%v)", stderr, err)
		return 0
	}

	out, err := strconv.ParseFloat(output, 64)
	if err != nil {
		klog.Errorf("failed to parse timestamp %s: %v", output, err)
		return 0
	}
	return out
}

// startMasterMetricsUpdater adds a goroutine that updates a "timestamp" value in the
// nbdb every 30 seconds. This is so we can determine freshness of the database
func startMasterMetricsUpdater() {
	startMasterUpdaterOnce.Do(func() {
		go func() {
			for {
				t := time.Now().Unix()
				_, stderr, err := util.RunOVNNbctl("set", "NB_Global", ".",
					fmt.Sprintf(`options:e2e_timestamp="%d"`, t))
				if err != nil {
					klog.Errorf("failed to bump timestamp: %s (%v)", stderr, err)
				} else {
					metricE2ETimestamp.Set(float64(t))
				}
				time.Sleep(30 * time.Second)
			}
		}()
	})
}

// RecordPodCreated extracts the scheduled timestamp and records how long it took
// us to notice this and set up the pod's scheduling.
func RecordPodCreated(pod *kapi.Pod) {
	t := time.Now()

	// Find the scheduled timestamp
	for _, cond := range pod.Status.Conditions {
		if cond.Type != kapi.PodScheduled {
			continue
		}
		if cond.Status != kapi.ConditionTrue {
			return
		}
		creationLatency := t.Sub(cond.LastTransitionTime.Time).Seconds()
		metricPodCreationLatency.Observe(creationLatency)
		return
	}
}

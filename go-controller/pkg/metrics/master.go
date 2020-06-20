package metrics

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/prometheus/client_golang/prometheus"

	kapi "k8s.io/api/core/v1"
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

// MetricResourceUpdateCount is the number of times a particular resource's UpdateFunc has been called.
var MetricResourceUpdateCount = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "resource_update_total",
	Help:      "A metric that captures the number of times a particular resource's UpdateFunc has been called"},
	[]string{
		"name",
	},
)

// MetricResourceUpdateLatency is the time taken to complete resource update by an handler.
// This measures the latency for all of the handlers for a given resource.
var MetricResourceUpdateLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "resource_update_latency_seconds",
	Help:      "The latency of various update handlers for a given resource",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
	[]string{"name"},
)

var MetricMasterReadyDuration = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "ready_duration_seconds",
	Help:      "The duration for the master to get to ready state",
})

// MetricMasterLeader identifies whether this instance of ovnkube-master is a leader or not
var MetricMasterLeader = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "leader",
	Help:      "Identifies whether the instance of ovnkube-master is a leader(1) or not(0).",
})

var registerMasterMetricsOnce sync.Once
var startE2ETimeStampUpdaterOnce sync.Once

var ovnNorthdCoverageShowMetricsMap = map[string]*metricDetails{
	"pstream_open": {
		help: "Specifies the number of time passive connections " +
			"were opened for the remote peer to connect.",
	},
	"stream_open": {
		help: "Specifies the number of attempts to connect " +
			"to a remote peer (active connection).",
	},
	"txn_success": {
		help: "Specifies the number of times the OVSDB " +
			"transaction has successfully completed.",
	},
	"txn_error": {
		help: "Specifies the number of times the OVSDB " +
			"transaction has errored out.",
	},
	"txn_uncommitted": {
		help: "Specifies the number of times the OVSDB " +
			"transaction were uncommitted.",
	},
	"txn_unchanged": {
		help: "Specifies the number of times the OVSDB transaction " +
			"resulted in no change to the database.",
	},
	"txn_incomplete": {
		help: "Specifies the number of times the OVSDB transaction " +
			"did not complete and the client had to re-try.",
	},
	"txn_aborted": {
		help: "Specifies the number of times the OVSDB " +
			" transaction has been aborted.",
	},
	"txn_try_again": {
		help: "Specifies the number of times the OVSDB " +
			"transaction failed and the client had to re-try.",
	},
}

// RegisterMasterMetrics registers some ovnkube master metrics with the Prometheus
// registry
func RegisterMasterMetrics() {
	registerMasterMetricsOnce.Do(func() {
		// ovnkube-master metrics
		prometheus.MustRegister(metricE2ETimestamp)
		prometheus.MustRegister(MetricMasterLeader)
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
		prometheus.MustRegister(MetricResourceUpdateCount)
		prometheus.MustRegister(MetricResourceUpdateLatency)
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

		// ovn-northd metrics
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: MetricOvnNamespace,
				Subsystem: MetricOvnSubsystemNorthd,
				Name:      "probe_interval",
				Help: "The maximum number of milliseconds of idle time on connection to the OVN SB " +
					"and NB DB before sending an inactivity probe message",
			}, func() float64 {
				stdout, stderr, err := util.RunOVNNbctlWithTimeout(5, "get", "NB_Global", ".",
					"options:northd_probe_interval")
				if err != nil {
					klog.Errorf("Failed to get northd_probe_interval value "+
						"stderr(%s) :(%v)", stderr, err)
					return 0
				}
				return parseMetricToFloat(MetricOvnSubsystemNorthd, "probe_interval", stdout)
			},
		))
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: MetricOvnNamespace,
				Subsystem: MetricOvnSubsystemNorthd,
				Name:      "status",
				Help:      "Specifies whether this instance of ovn-northd is standby(0) or active(1) or paused(2).",
			}, func() float64 {
				stdout, stderr, err := util.RunOVNNorthAppCtl("status")
				if err != nil {
					klog.Errorf("Failed to get ovn-northd status "+
						"stderr(%s) :(%v)", stderr, err)
					return -1
				}
				northdStatusMap := map[string]float64{
					"standby": 0,
					"active":  1,
					"paused":  2,
				}
				if strings.HasPrefix(stdout, "Status:") {
					output := strings.TrimSpace(strings.Split(stdout, ":")[1])
					if value, ok := northdStatusMap[output]; ok {
						return value
					}
				}
				return -1
			},
		))

		// Register the ovn-northd coverage/show metrics with prometheus
		componentCoverageShowMetricsMap[ovnNorthd] = ovnNorthdCoverageShowMetricsMap
		registerCoverageShowMetrics(ovnNorthd, MetricOvnNamespace, MetricOvnSubsystemNorthd)
		go coverageShowMetricsUpdater(ovnNorthd)
	})
}

func scrapeOvnTimestamp() float64 {
	output, stderr, err := util.RunOVNSbctl("--if-exists",
		"get", "SB_Global", ".", "options:e2e_timestamp")
	if err != nil {
		klog.Errorf("Failed to scrape timestamp: %s (%v)", stderr, err)
		return 0
	}
	return parseMetricToFloat(MetricOvnkubeSubsystemMaster, "sb_e2e_timestamp", output)
}

// StartE2ETimeStampMetricUpdater adds a goroutine that updates a "timestamp" value in the
// nbdb every 30 seconds. This is so we can determine freshness of the database
func StartE2ETimeStampMetricUpdater() {
	startE2ETimeStampUpdaterOnce.Do(func() {
		go func() {
			for {
				t := time.Now().Unix()
				_, stderr, err := util.RunOVNNbctl("set", "NB_Global", ".",
					fmt.Sprintf(`options:e2e_timestamp="%d"`, t))
				if err != nil {
					klog.Errorf("Failed to bump timestamp: %s (%v)", stderr, err)
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

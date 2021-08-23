package metrics

import (
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"time"

	goovn "github.com/ebay/go-ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/prometheus/client_golang/prometheus"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
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
	Help:      "The number of times a given resource event (add, update, or delete) has been handled"},
	[]string{
		"name",
		"event",
	},
)

// MetricResourceAddLatency is the time taken to complete resource update by an handler.
// This measures the latency for all of the handlers for a given resource.
var MetricResourceAddLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "resource_add_latency_seconds",
	Help:      "The duration to process all handlers for a given resource event - add.",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
)

// MetricResourceUpdateLatency is the time taken to complete resource update by an handler.
// This measures the latency for all of the handlers for a given resource.
var MetricResourceUpdateLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "resource_update_latency_seconds",
	Help:      "The duration to process all handlers for a given resource event - update.",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
)

// MetricResourceDeleteLatency is the time taken to complete resource update by an handler.
// This measures the latency for all of the handlers for a given resource.
var MetricResourceDeleteLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "resource_delete_latency_seconds",
	Help:      "The duration to process all handlers for a given resource event - delete.",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
)

// MetricRequeueServiceCount is the number of times a particular service has been requeued.
var MetricRequeueServiceCount = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "requeue_service_total",
	Help:      "A metric that captures the number of times a service is requeued after failing to sync with OVN"},
)

// MetricSyncServiceCount is the number of times a particular service has been synced.
var MetricSyncServiceCount = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "sync_service_total",
	Help:      "A metric that captures the number of times a service is synced with OVN load balancers"},
)

// MetricSyncServiceLatency is the time taken to sync a service with the OVN load balancers.
var MetricSyncServiceLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "sync_service_latency_seconds",
	Help:      "The latency of syncing a service with the OVN load balancers",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
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

var metricV4HostSubnetCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "num_v4_host_subnets",
	Help:      "The total number of v4 host subnets possible",
})

var metricV6HostSubnetCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "num_v6_host_subnets",
	Help:      "The total number of v6 host subnets possible",
})

var metricV4AllocatedHostSubnetCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "allocated_v4_host_subnets",
	Help:      "The total number of v4 host subnets currently allocated",
})

var metricV6AllocatedHostSubnetCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "allocated_v6_host_subnets",
	Help:      "The total number of v6 host subnets currently allocated",
})

// The nb_cfg metrics. Currently, nb_cfg is a random serial number that we tick up every 30 seconds.
// northd copies this to sbdb, then ovn-controlle reads this and writes it to a chassis-specific
// table in sbdb as well

// metricNbcfgNbdb is the value of nb_cfg we write to nbdb
// There is an equivalent metric for the value in sbdb
var metricNbcfgNbdb = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "nb_cfg_nbdb",
	Help:      "The current nb_cfg as written to the nbdb",
})

var metricNbcfgChassis = prometheus.NewDesc(
	prometheus.BuildFQName(MetricOvnkubeNamespace, MetricOvnkubeSubsystemMaster, "chassis_nb_cfg"),
	"The value of nb_cfg as reported by each ovn-controller",
	[]string{"node"}, nil,
)

var registerMasterMetricsOnce sync.Once
var startE2ETimeStampUpdaterOnce sync.Once

// RegisterMasterMetrics registers some ovnkube master metrics with the Prometheus
// registry
func RegisterMasterMetrics(nbClient, sbClient goovn.Client) {
	registerMasterMetricsOnce.Do(func() {
		// ovnkube-master metrics
		// the updater for this metric is activated
		// after leader election
		prometheus.MustRegister(metricE2ETimestamp)
		prometheus.MustRegister(metricNbcfgNbdb)
		prometheus.MustRegister(MetricMasterLeader)
		prometheus.MustRegister(metricPodCreationLatency)
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: MetricOvnkubeNamespace,
				Subsystem: MetricOvnkubeSubsystemMaster,
				Name:      "sb_e2e_timestamp",
				Help:      "The current e2e-timestamp as reported in the southbound database",
			}, func() float64 { return scrapeE2ETimestampSbdb(sbClient) }))

		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: MetricOvnkubeNamespace,
				Subsystem: MetricOvnkubeSubsystemMaster,
				Name:      "nb_cfg_sbdb",
				Help:      "The current nb_cfg as written to the sbdb",
			},
			scrapeNbcfgSbdb,
		))

		prometheus.MustRegister(prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Namespace: MetricOvnkubeNamespace,
				Subsystem: MetricOvnkubeSubsystemMaster,
				Name:      "skipped_nbctl_daemon_total",
				Help:      "The number of times we skipped using ovn-nbctl daemon and directly interacted with OVN NB DB",
			}, func() float64 {
				return float64(util.SkippedNbctlDaemonCounter)
			}))

		prometheus.MustRegister(&chassisE2EScraper{sbClient: sbClient})
		prometheus.MustRegister(MetricMasterReadyDuration)
		prometheus.MustRegister(metricOvnCliLatency)
		// this is to not to create circular import between metrics and util package
		util.MetricOvnCliLatency = metricOvnCliLatency
		prometheus.MustRegister(MetricResourceUpdateCount)
		prometheus.MustRegister(MetricResourceAddLatency)
		prometheus.MustRegister(MetricResourceUpdateLatency)
		prometheus.MustRegister(MetricResourceDeleteLatency)
		prometheus.MustRegister(MetricRequeueServiceCount)
		prometheus.MustRegister(MetricSyncServiceCount)
		prometheus.MustRegister(MetricSyncServiceLatency)
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: MetricOvnkubeNamespace,
				Subsystem: MetricOvnkubeSubsystemMaster,
				Name:      "build_info",
				Help: "A metric with a constant '1' value labeled by version, revision, branch, " +
					"and go version from which ovnkube was built and when and who built it",
				ConstLabels: prometheus.Labels{
					"version":    "0.0",
					"revision":   config.Commit,
					"branch":     config.Branch,
					"build_user": config.BuildUser,
					"build_date": config.BuildDate,
					"goversion":  runtime.Version(),
				},
			},
			func() float64 { return 1 },
		))
		prometheus.MustRegister(metricV4HostSubnetCount)
		prometheus.MustRegister(metricV6HostSubnetCount)
		prometheus.MustRegister(metricV4AllocatedHostSubnetCount)
		prometheus.MustRegister(metricV6AllocatedHostSubnetCount)
		registerWorkqueueMetrics(MetricOvnkubeNamespace, MetricOvnkubeSubsystemMaster)
	})
}

func scrapeNbcfgSbdb() float64 {
	ts, _, err := util.RunOVNSbctl("get", "SB_Global", ".", "nb_cfg")
	if err != nil {
		klog.Errorf("Failed to get SB_global nb_cfg: %v", err)
		return 0
	}
	return parseMetricToFloat(MetricOvnkubeSubsystemMaster, "sb_e2e_timestamp", ts)
}

func scrapeE2ETimestampSbdb(sbClient goovn.Client) float64 {
	options, err := sbClient.SBGlobalGetOptions()
	if err != nil {
		klog.Errorf("Failed to get global options for the SB_Global table")
		return 0
	}
	if val, ok := options["e2e_timestamp"]; ok {
		return parseMetricToFloat(MetricOvnkubeSubsystemMaster, "sb_e2e_timestamp", val)
	}
	return 0
}

var nbCfgValue int

// StartE2ETimeStampMetricUpdater adds a goroutine that updates the nb_cfg "timestamp" value in the
// nbdb every 30 seconds. This is so we can determine freshness of the database as well as
// ovn-controller's status.
// northd will propagate this down in to the sbdb. ovn-controller will update this value
// in the sbdb when it synchronizes.
func StartE2ETimeStampMetricUpdater(stopChan <-chan struct{}, ovnNBClient goovn.Client) {
	startE2ETimeStampUpdaterOnce.Do(func() {
		go func() {
			klog.Infof("Starting periodic nb_cfg updater...")
			tsUpdateTicker := time.NewTicker(30 * time.Second)
			defer tsUpdateTicker.Stop()

			tick := func() {
				// set unix timestamp to e2e_timestamp in NB_Global Options
				options, err := ovnNBClient.NBGlobalGetOptions()
				if err != nil {
					klog.Errorf("Can't get existing NB Global Options for updating timestamps")
					return
				}
				t := time.Now().Unix()
				options["e2e_timestamp"] = fmt.Sprintf("%d", t)
				cmd, err := ovnNBClient.NBGlobalSetOptions(options)
				if err != nil {
					klog.Errorf("Failed to bump timestamp: %v", err)
				} else {
					err = cmd.Execute()
					if err != nil {
						klog.Errorf("Failed to set timestamp: %v", err)
					} else {
						metricE2ETimestamp.Set(float64(t))
					}
				}

				// Separately, set nb_cfg to arbitrary serial number
				nbCfgValue += 1
				_, _, err = util.RunOVNNbctl("set", "NB_Global", ".", "nb_cfg="+strconv.Itoa(nbCfgValue))
				if err != nil {
					klog.Errorf("Failed to bump NB_Global nb_cfg value: %v", err)
				} else {
					klog.V(7).Infof("Set nb_cfg to %d", nbCfgValue)
					metricNbcfgNbdb.Set(float64(nbCfgValue))
				}
			}

			tick() // set the timestamp immediately at first

			for {
				select {
				case <-tsUpdateTicker.C:
					tick()
				case <-stopChan:
					return
				}
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

// RecordSubnetUsage records the number of subnets allocated for nodes
func RecordSubnetUsage(v4SubnetsAllocated, v6SubnetsAllocated float64) {
	metricV4AllocatedHostSubnetCount.Set(v4SubnetsAllocated)
	metricV6AllocatedHostSubnetCount.Set(v6SubnetsAllocated)
}

// RecordSubnetCount records the number of available subnets per configuration
// for ovn-kubernetes
func RecordSubnetCount(v4SubnetCount, v6SubnetCount float64) {
	metricV4HostSubnetCount.Set(v4SubnetCount)
	metricV6HostSubnetCount.Set(v6SubnetCount)
}

// chassisE2EScraper implements the Collector interface so we can
// generate chassis metrics on-the-fly per scrape
type chassisE2EScraper struct {
	sbClient goovn.Client
}

func (c *chassisE2EScraper) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, ch)
}

// Collect scrapes the Chassis and Chassis_Priv tables to determine the
func (c *chassisE2EScraper) Collect(ch chan<- prometheus.Metric) {
	// the nb_cfg value is in Chassis_Private, but the
	// hostname is in Chassis. So we need to list both and join
	allChassis, err := c.sbClient.ChassisList()
	if err != nil {
		klog.Errorf("Failed to list sbdb Chassis table: %v", err)
		return
	}

	// map of chassis name to hostname
	hostnames := make(map[string]string, len(allChassis))
	for _, chassis := range allChassis {
		hostnames[chassis.Name] = chassis.Hostname
	}

	allChassisPriv, err := c.sbClient.ChassisPrivateList()
	if err != nil {
		klog.Errorf("Failed to list sbdb Chassis_Private table: %v", err)
	}
	for _, chassis := range allChassisPriv {
		hostname := hostnames[chassis.Name]
		if hostname == "" {
			continue
		}
		ch <- prometheus.MustNewConstMetric(
			metricNbcfgChassis,
			prometheus.GaugeValue,
			float64(chassis.NbCfg),
			hostname,
		)
	}
}

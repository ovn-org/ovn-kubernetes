package metrics

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/ovn-org/libovsdb/cache"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/prometheus/client_golang/prometheus"
	kapi "k8s.io/api/core/v1"
	kapimtypes "k8s.io/apimachinery/pkg/types"
	klog "k8s.io/klog/v2"
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

// metricOvnCliLatency is the time between a pod being scheduled and the
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

var metricEgressIPCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "num_egress_ips",
	Help:      "The number of defined egress IP addresses",
})

var metricEgressFirewallRuleCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "num_egress_firewall_rules",
	Help:      "The number of egress firewall rules defined"},
)

var metricIPsecEnabled = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "ipsec_enabled",
	Help:      "Specifies whether IPSec is enabled for this cluster(1) or not enabled for this cluster(0)",
})

var metricEgressFirewallCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "num_egress_firewalls",
	Help:      "The number of egress firewall policies",
})

// metricFirstSeenLSPLatency is the time between a pod first seen in OVN-Kubernetes and its Logical Switch Port is created
var metricFirstSeenLSPLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "pod_first_seen_lsp_created_duration_seconds",
	Help:      "The duration between a pod first observed in OVN-Kubernetes and Logical Switch Port created",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 15),
})

var metricLSPPortBindingLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "pod_lsp_created_port_binding_duration_seconds",
	Help:      "The duration between a pods Logical Switch Port created and port binding observed in cache",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 15),
})

var metricPortBindingChassisLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "pod_port_binding_port_binding_chassis_duration_seconds",
	Help:      "The duration between a pods port binding observed and port binding chassis update observed in cache",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 15),
})

var metricPortBindingUpLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemMaster,
	Name:      "pod_port_binding_chassis_port_binding_up_duration_seconds",
	Help:      "The duration between a pods port binding chassis update and port binding up observed in cache",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 15),
})

var registerMasterMetricsOnce sync.Once
var startMasterMetricUpdaterOnce sync.Once

// RegisterMasterMetrics registers some ovnkube master metrics with the Prometheus
// registry
func RegisterMasterMetrics(sbClient client.Client) {
	registerMasterMetricsOnce.Do(func() {
		// ovnkube-master metrics
		// the updater for this metric is activated
		// after leader election
		prometheus.MustRegister(metricE2ETimestamp)
		prometheus.MustRegister(MetricMasterLeader)
		prometheus.MustRegister(metricPodCreationLatency)

		scrapeOvnTimestamp := func() float64 {
			sbGlobal, err := libovsdbops.FindSBGlobal(sbClient)
			if err != nil {
				klog.Errorf("Failed to get global options for the SB_Global table err: %v", err)
				return 0
			}
			if val, ok := sbGlobal.Options["e2e_timestamp"]; ok {
				return parseMetricToFloat(MetricOvnkubeSubsystemMaster, "sb_e2e_timestamp", val)
			}
			return 0
		}
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
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
		prometheus.MustRegister(metricEgressIPCount)
		prometheus.MustRegister(metricEgressFirewallRuleCount)
		prometheus.MustRegister(metricIPsecEnabled)
		prometheus.MustRegister(metricEgressFirewallCount)
		registerControlPlaneRecorderMetrics()
		registerWorkqueueMetrics(MetricOvnkubeNamespace, MetricOvnkubeSubsystemMaster)
	})
}

// StartMasterMetricUpdater adds a goroutine that updates a "timestamp" value in the
// nbdb every 30 seconds. This is so we can determine freshness of the database.
// Also, update IPsec enabled or disable metric.
func StartMasterMetricUpdater(stopChan <-chan struct{}, nbClient client.Client) {
	startMasterMetricUpdaterOnce.Do(func() {
		addIPSecMetricHandler(nbClient)
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					updateE2ETimestampMetric(nbClient)
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

// RecordEgressIPCount records the total number of Egress IPs.
// This total may include multiple Egress IPs per EgressIP CR.
func RecordEgressIPCount(count float64) {
	metricEgressIPCount.Set(count)
}

// UpdateEgressFirewallRuleCount records the number of Egress firewall rules.
func UpdateEgressFirewallRuleCount(count float64) {
	metricEgressFirewallRuleCount.Add(count)
}

func updateE2ETimestampMetric(ovnNBClient client.Client) {
	currentTime := time.Now().Unix()
	// assumption that only first row is relevant in NB_Global table
	if err := libovsdbops.UpdateNBGlobalOptions(ovnNBClient, map[string]string{"e2e_timestamp": fmt.Sprintf("%d", currentTime)}); err != nil {
		klog.Errorf("Unable to update E2E timestamp metric err: %v", err)
		return
	}

	metricE2ETimestamp.Set(float64(currentTime))
}

func addIPSecMetricHandler(ovnNBClient client.Client) {
	ovnNBClient.Cache().AddEventHandler(&cache.EventHandlerFuncs{
		AddFunc: func(table string, model model.Model) {
			ipsecMetricHandler(table, model)
		},
		UpdateFunc: func(table string, _, new model.Model) {
			ipsecMetricHandler(table, new)
		},
		DeleteFunc: func(table string, model model.Model) {
			ipsecMetricHandler(table, model)
		},
	})
}

func ipsecMetricHandler(table string, model model.Model) {
	if table != "NB_Global" {
		return
	}
	entry := model.(*nbdb.NBGlobal)
	if entry.Ipsec {
		metricIPsecEnabled.Set(1)
	} else {
		metricIPsecEnabled.Set(0)
	}
}

// IncrementEgressFirewallCount increments the number of Egress firewalls
func IncrementEgressFirewallCount() {
	metricEgressFirewallCount.Inc()
}

// DecrementEgressFirewallCount decrements the number of Egress firewalls
func DecrementEgressFirewallCount() {
	metricEgressFirewallCount.Dec()
}

func registerControlPlaneRecorderMetrics() {
	prometheus.MustRegister(metricFirstSeenLSPLatency)
	prometheus.MustRegister(metricLSPPortBindingLatency)
	prometheus.MustRegister(metricPortBindingUpLatency)
	prometheus.MustRegister(metricPortBindingChassisLatency)
}

type timestampType int

const (
	// pod event first handled by OVN-Kubernetes control plane
	firstSeen timestampType = iota
	// OVN-Kubernetes control plane created Logical Switch Port in northbound database
	logicalSwitchPort
	// port binding seen in OVN-Kubernetes control plane southbound database libovsdb cache
	portBinding
	// port binding with updated chassis seen in OVN-Kubernetes  control plane southbound database libovsdb cache
	portBindingChassis
	portBindingTable = "Port_Binding"
)

type record struct {
	timestamp time.Time
	timestampType
}

type ControlPlaneRecorder struct {
	sync.Mutex
	podRecords map[kapimtypes.UID]*record
}

func NewControlPlaneRecorder(sbClient client.Client) *ControlPlaneRecorder {
	recorder := ControlPlaneRecorder{sync.Mutex{}, make(map[kapimtypes.UID]*record)}
	sbClient.Cache().AddEventHandler(&cache.EventHandlerFuncs{
		AddFunc: func(table string, model model.Model) {
			go recorder.AddPortBindingEvent(table, model)
		},
		UpdateFunc: func(table string, old model.Model, new model.Model) {
			go recorder.UpdatePortBindingEvent(table, old, new)
		},
		DeleteFunc: func(table string, model model.Model) {
		},
	})
	return &recorder
}

func (ps *ControlPlaneRecorder) AddPodEvent(podUID kapimtypes.UID) {
	ps.Lock()
	ps.podRecords[podUID] = &record{timestamp: time.Now(), timestampType: firstSeen}
	ps.Unlock()
}

func (ps *ControlPlaneRecorder) CleanPodRecord(podUID kapimtypes.UID) {
	ps.Lock()
	delete(ps.podRecords, podUID)
	ps.Unlock()
}

func (ps *ControlPlaneRecorder) AddLSPEvent(podUID kapimtypes.UID) {
	now := time.Now()
	ps.Lock()
	defer ps.Unlock()
	var r *record
	if r = ps.getRecord(podUID); r == nil {
		klog.Errorf("Metrics: add Logical Switch Port event expected pod with UID %q in cache", podUID)
		return
	}
	if r.timestampType != firstSeen {
		klog.Errorf("Metrics: unexpected last event type (%d) in cache for pod with UID %q", r.timestampType, podUID)
		return
	}
	metricFirstSeenLSPLatency.Observe(now.Sub(r.timestamp).Seconds())
	r.timestamp = now
	r.timestampType = logicalSwitchPort
}

func (ps *ControlPlaneRecorder) AddPortBindingEvent(table string, m model.Model) {
	if table != portBindingTable {
		return
	}
	var r *record
	now := time.Now()
	row := m.(*sbdb.PortBinding)
	podUID := getPodUIDFromPortBinding(row)
	if podUID == "" {
		return
	}
	ps.Lock()
	defer ps.Unlock()
	if r = ps.getRecord(podUID); r == nil {
		klog.Errorf("Metrics: add port binding event expected pod with UID %q in cache", podUID)
		return
	}
	if r.timestampType != logicalSwitchPort {
		klog.Errorf("Metrics: unexpected last event entry (%d) in cache for pod with UID %q", r.timestampType, podUID)
		return
	}
	metricLSPPortBindingLatency.Observe(now.Sub(r.timestamp).Seconds())
	r.timestamp = now
	r.timestampType = portBinding
}

func (ps *ControlPlaneRecorder) UpdatePortBindingEvent(table string, old, new model.Model) {
	if table != portBindingTable {
		return
	}
	var r *record
	oldRow := old.(*sbdb.PortBinding)
	newRow := new.(*sbdb.PortBinding)
	now := time.Now()
	podUID := getPodUIDFromPortBinding(newRow)
	if podUID == "" {
		return
	}
	ps.Lock()
	defer ps.Unlock()
	if r = ps.getRecord(podUID); r == nil {
		klog.Errorf("Metrics: port binding update expected pod with UID %q in cache", podUID)
		return
	}
	if oldRow.Chassis == nil && newRow.Chassis != nil && r.timestampType == portBinding {
		metricPortBindingChassisLatency.Observe(now.Sub(r.timestamp).Seconds())
		r.timestamp = now
		r.timestampType = portBindingChassis
	}
	if oldRow.Up != nil && !*oldRow.Up && newRow.Up != nil && *newRow.Up && r.timestampType == portBindingChassis {
		metricPortBindingUpLatency.Observe(now.Sub(r.timestamp).Seconds())
	}
}

// getRecord assumes lock is held by caller and returns record from map with func argument as the key
func (ps *ControlPlaneRecorder) getRecord(podUID kapimtypes.UID) *record {
	r, ok := ps.podRecords[podUID]
	if !ok {
		klog.Errorf("Metrics: cache entry expected pod with UID %q but failed to find it", podUID)
		return nil
	}
	return r
}

func getPodUIDFromPortBinding(row *sbdb.PortBinding) kapimtypes.UID {
	if isPod, ok := row.ExternalIDs["pod"]; !ok || isPod != "true" {
		return ""
	}
	podUID, ok := row.Options["iface-id-ver"]
	if !ok {
		return ""
	}
	return kapimtypes.UID(podUID)
}

package metrics

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/ovn-org/libovsdb/cache"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/prometheus/client_golang/prometheus"
	kapi "k8s.io/api/core/v1"
	kapimtypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	klog "k8s.io/klog/v2"
)

// metricNbE2eTimestamp is the UNIX timestamp value set to NB DB. Northd will eventually copy this
// timestamp from NB DB to SB DB. The metric 'sb_e2e_timestamp' stores the timestamp that is
// read from SB DB. This is registered within func RunTimestamp in order to allow gathering this
// metric on the fly when metrics are scraped.
var metricNbE2eTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "nb_e2e_timestamp",
	Help:      "The current e2e-timestamp value as written to the northbound database"},
)

// metricDbTimestamp is the UNIX timestamp seen in NB and SB DBs.
var metricDbTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
	Name:      "e2e_timestamp",
	Help:      "The current e2e-timestamp value as observed in this instance of the database"},
	[]string{
		"db_name",
	},
)

// metricPodCreationLatency is the time between a pod being scheduled and
// completing its logical switch port configuration.
var metricPodCreationLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "pod_creation_latency_seconds",
	Help:      "The duration between a pod being scheduled and completing its logical switch port configuration",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15),
})

// MetricResourceUpdateCount is the number of times a particular resource's UpdateFunc has been called.
var MetricResourceUpdateCount = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
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
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "resource_add_latency_seconds",
	Help:      "The duration to process all handlers for a given resource event - add.",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
)

// MetricResourceUpdateLatency is the time taken to complete resource update by an handler.
// This measures the latency for all of the handlers for a given resource.
var MetricResourceUpdateLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "resource_update_latency_seconds",
	Help:      "The duration to process all handlers for a given resource event - update.",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
)

// MetricResourceDeleteLatency is the time taken to complete resource update by an handler.
// This measures the latency for all of the handlers for a given resource.
var MetricResourceDeleteLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "resource_delete_latency_seconds",
	Help:      "The duration to process all handlers for a given resource event - delete.",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
)

// MetricRequeueServiceCount is the number of times a particular service has been requeued.
var MetricRequeueServiceCount = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "requeue_service_total",
	Help:      "A metric that captures the number of times a service is requeued after failing to sync with OVN"},
)

// MetricSyncServiceCount is the number of times a particular service has been synced.
var MetricSyncServiceCount = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "sync_service_total",
	Help:      "A metric that captures the number of times a service is synced with OVN load balancers"},
)

// MetricSyncServiceLatency is the time taken to sync a service with the OVN load balancers.
var MetricSyncServiceLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "sync_service_latency_seconds",
	Help:      "The latency of syncing a service with the OVN load balancers",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
)

var MetricOVNKubeControllerReadyDuration = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "ready_duration_seconds",
	Help:      "The duration for the ovnkube-controller to get to ready state",
})

// MetricOVNKubeControllerSyncDuration is the time taken to complete initial Watch for different resource.
// Resource name is in the label.
var MetricOVNKubeControllerSyncDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "sync_duration_seconds",
	Help:      "The duration to sync and setup all handlers for a given resource"},
	[]string{
		"resource_name",
	})

// MetricOVNKubeControllerLeader identifies whether this instance of ovnkube-controller is a leader or not
var MetricOVNKubeControllerLeader = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "leader",
	Help:      "Identifies whether the instance of ovnkube-controller is a leader(1) or not(0).",
})

var metricOvnKubeControllerLogFileSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "logfile_size_bytes",
	Help:      "The size of ovnkube-controller log file."},
	[]string{
		"logfile_name",
	},
)

var metricEgressIPAssignLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "egress_ips_assign_latency_seconds",
	Help:      "The latency of egress IP assignment to ovn nb database",
	Buckets:   prometheus.ExponentialBuckets(.001, 2, 15),
})

var metricEgressIPUnassignLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "egress_ips_unassign_latency_seconds",
	Help:      "The latency of egress IP unassignment from ovn nb database",
	Buckets:   prometheus.ExponentialBuckets(.001, 2, 15),
})

var metricNetpolEventLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "network_policy_event_latency_seconds",
	Help:      "The latency of full network policy event handling (create, delete)",
	Buckets:   prometheus.ExponentialBuckets(.004, 2, 15)},
	[]string{
		"event",
	})

var metricNetpolLocalPodEventLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "network_policy_local_pod_event_latency_seconds",
	Help:      "The latency of local pod events handling (add, delete)",
	Buckets:   prometheus.ExponentialBuckets(.002, 2, 15)},
	[]string{
		"event",
	})

var metricNetpolPeerNamespaceEventLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "network_policy_peer_namespace_event_latency_seconds",
	Help:      "The latency of peer namespace events handling (add, delete)",
	Buckets:   prometheus.ExponentialBuckets(.002, 2, 15)},
	[]string{
		"event",
	})

var metricPodSelectorAddrSetPodEventLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "pod_selector_address_set_pod_event_latency_seconds",
	Help:      "The latency of peer pod events handling (add, delete)",
	Buckets:   prometheus.ExponentialBuckets(.002, 2, 15)},
	[]string{
		"event",
	})

var metricPodSelectorAddrSetNamespaceEventLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "pod_selector_address_set_namespace_event_latency_seconds",
	Help:      "The latency of peer namespace events handling (add, delete)",
	Buckets:   prometheus.ExponentialBuckets(.002, 2, 15)},
	[]string{
		"event",
	})

var metricPodEventLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "pod_event_latency_seconds",
	Help:      "The latency of pod events handling (add, update, delete)",
	Buckets:   prometheus.ExponentialBuckets(.002, 2, 15)},
	[]string{
		"event",
	})

var metricEgressFirewallRuleCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "num_egress_firewall_rules",
	Help:      "The number of egress firewall rules defined"},
)

var metricIPsecEnabled = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "ipsec_enabled",
	Help:      "Specifies whether IPSec is enabled for this cluster(1) or not enabled for this cluster(0)",
})

var metricEgressRoutingViaHost = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "egress_routing_via_host",
	Help:      "Specifies whether egress gateway mode is via host networking stack(1) or not(0)",
})

var metricEgressFirewallCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "num_egress_firewalls",
	Help:      "The number of egress firewall policies",
})

/** AdminNetworkPolicyMetrics Begin**/
var metricANPCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "admin_network_policies",
	Help:      "The total number of admin network policies in the cluster",
})

var metricBANPCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "baseline_admin_network_policies",
	Help:      "The total number of baseline admin network policies in the cluster",
})

var metricANPDBObjects = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "admin_network_policies_db_objects",
	Help:      "The total number of OVN NBDB objects (table_name) owned by AdminNetworkPolicy controller in the cluster"},
	[]string{
		"table_name",
	},
)

var metricBANPDBObjects = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "baseline_admin_network_policies_db_objects",
	Help:      "The total number of OVN NBDB objects (table_name) owned by BaselineAdminNetworkPolicy controller in the cluster"},
	[]string{
		"table_name",
	},
)

/** AdminNetworkPolicyMetrics End**/

// metricFirstSeenLSPLatency is the time between a pod first seen in OVN-Kubernetes and its Logical Switch Port is created
var metricFirstSeenLSPLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "pod_first_seen_lsp_created_duration_seconds",
	Help:      "The duration between a pod first observed in OVN-Kubernetes and Logical Switch Port created",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 15),
})

var metricLSPPortBindingLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "pod_lsp_created_port_binding_duration_seconds",
	Help:      "The duration between a pods Logical Switch Port created and port binding observed in cache",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 15),
})

var metricPortBindingChassisLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "pod_port_binding_port_binding_chassis_duration_seconds",
	Help:      "The duration between a pods port binding observed and port binding chassis update observed in cache",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 15),
})

var metricPortBindingUpLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "pod_port_binding_chassis_port_binding_up_duration_seconds",
	Help:      "The duration between a pods port binding chassis update and port binding up observed in cache",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 15),
})

var metricNetworkProgramming prometheus.ObserverVec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "network_programming_duration_seconds",
	Help: "The duration to apply network configuration for a kind (e.g. pod, service, networkpolicy). " +
		"Configuration includes add, update and delete events for each kind.",
	Buckets: merge(
		prometheus.LinearBuckets(0.25, 0.25, 2), // 0.25s, 0.50s
		prometheus.LinearBuckets(1, 1, 59),      // 1s, 2s, 3s, ... 59s
		prometheus.LinearBuckets(60, 5, 12),     // 60s, 65s, 70s, ... 115s
		prometheus.LinearBuckets(120, 30, 11))}, // 2min, 2.5min, 3min, ..., 7min
	[]string{
		"kind",
	})

var metricNetworkProgrammingOVN = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: MetricOvnkubeNamespace,
	Subsystem: MetricOvnkubeSubsystemController,
	Name:      "network_programming_ovn_duration_seconds",
	Help:      "The duration for OVN to apply network configuration",
	Buckets: merge(
		prometheus.LinearBuckets(0.25, 0.25, 2), // 0.25s, 0.50s
		prometheus.LinearBuckets(1, 1, 59),      // 1s, 2s, 3s, ... 59s
		prometheus.LinearBuckets(60, 5, 12),     // 60s, 65s, 70s, ... 115s
		prometheus.LinearBuckets(120, 30, 11))}, // 2min, 2.5min, 3min, ..., 7min
)

const (
	globalOptionsTimestampField     = "e2e_timestamp"
	globalOptionsProbeIntervalField = "northd_probe_interval"
)

// RegisterOVNKubeControllerBase registers ovnkube controller base metrics with the Prometheus registry.
// This function should only be called once.
func RegisterOVNKubeControllerBase() {
	prometheus.MustRegister(MetricOVNKubeControllerLeader)
	prometheus.MustRegister(MetricOVNKubeControllerReadyDuration)
	prometheus.MustRegister(MetricOVNKubeControllerSyncDuration)
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: MetricOvnkubeNamespace,
			Subsystem: MetricOvnkubeSubsystemController,
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
}

// RegisterOVNKubeControllerPerformance registers metrics that help us understand ovnkube-controller performance. Call once after LE is won.
func RegisterOVNKubeControllerPerformance(nbClient libovsdbclient.Client) {
	// No need to unregister because process exits when leadership is lost.
	prometheus.MustRegister(metricPodCreationLatency)
	prometheus.MustRegister(MetricResourceUpdateCount)
	prometheus.MustRegister(MetricResourceAddLatency)
	prometheus.MustRegister(MetricResourceUpdateLatency)
	prometheus.MustRegister(MetricResourceDeleteLatency)
	prometheus.MustRegister(MetricRequeueServiceCount)
	prometheus.MustRegister(MetricSyncServiceCount)
	prometheus.MustRegister(MetricSyncServiceLatency)
	registerWorkqueueMetrics(MetricOvnkubeNamespace, MetricOvnkubeSubsystemController)
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: MetricOvnNamespace,
			Subsystem: MetricOvnSubsystemNorthd,
			Name:      "northd_probe_interval",
			Help: "The maximum number of milliseconds of idle time on connection to the OVN SB " +
				"and NB DB before sending an inactivity probe message",
		}, func() float64 {
			return getGlobalOptionsValue(nbClient, globalOptionsProbeIntervalField)
		},
	))
}

// RegisterOVNKubeControllerFunctional is a collection of metrics that help us understand ovnkube-controller functions. Call once after
// LE is won.
func RegisterOVNKubeControllerFunctional(stopChan <-chan struct{}) {
	// No need to unregister because process exits when leadership is lost.
	if config.Metrics.EnableScaleMetrics {
		klog.Infof("Scale metrics are enabled")
		prometheus.MustRegister(metricEgressIPAssignLatency)
		prometheus.MustRegister(metricEgressIPUnassignLatency)
		prometheus.MustRegister(metricNetpolEventLatency)
		prometheus.MustRegister(metricNetpolLocalPodEventLatency)
		prometheus.MustRegister(metricNetpolPeerNamespaceEventLatency)
		prometheus.MustRegister(metricPodSelectorAddrSetPodEventLatency)
		prometheus.MustRegister(metricPodSelectorAddrSetNamespaceEventLatency)
		prometheus.MustRegister(metricPodEventLatency)
	}
	prometheus.MustRegister(metricEgressFirewallRuleCount)
	prometheus.MustRegister(metricEgressFirewallCount)
	prometheus.MustRegister(metricEgressRoutingViaHost)
	prometheus.MustRegister(metricANPCount)
	prometheus.MustRegister(metricBANPCount)
	if err := prometheus.Register(MetricResourceRetryFailuresCount); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			panic(err)
		}
	}
	// ovnkube-controller logfile size metric
	prometheus.MustRegister(metricOvnKubeControllerLogFileSize)
	go ovnKubeLogFileSizeMetricsUpdater(metricOvnKubeControllerLogFileSize, stopChan)
}

func registerOVNKubeFeatureDBObjectsMetrics() {
	prometheus.MustRegister(metricANPDBObjects)
	prometheus.MustRegister(metricBANPDBObjects)
}

func RunOVNKubeFeatureDBObjectsMetricsUpdater(ovnNBClient libovsdbclient.Client, controllerName string, tickPeriod time.Duration, stopChan <-chan struct{}) {
	registerOVNKubeFeatureDBObjectsMetrics()
	go func() {
		ticker := time.NewTicker(tickPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				updateOVNKubeFeatureNBDBObjectMetrics(ovnNBClient, controllerName)
			case <-stopChan:
				return
			}
		}
	}()
}

func updateOVNKubeFeatureNBDBObjectMetrics(ovnNBClient libovsdbclient.Client, controllerName string) {
	if config.OVNKubernetesFeature.EnableAdminNetworkPolicy {
		// ANP Feature
		// 1. ACL 2. AddressSet (TODO: Add PG once indexing is done)
		aclCount := libovsdbutil.GetACLCount(ovnNBClient, libovsdbops.ACLAdminNetworkPolicy, controllerName)
		metricANPDBObjects.WithLabelValues(nbdb.ACLTable).Set(float64(aclCount))
		addressSetCount := libovsdbutil.GetAddressSetCount(ovnNBClient, libovsdbops.AddressSetAdminNetworkPolicy, controllerName)
		metricANPDBObjects.WithLabelValues(nbdb.AddressSetTable).Set(float64(addressSetCount))

		// BANP Feature
		// 1. ACL 2. AddressSet (TODO: Add PG once indexing is done)
		aclCount = libovsdbutil.GetACLCount(ovnNBClient, libovsdbops.ACLBaselineAdminNetworkPolicy, controllerName)
		metricBANPDBObjects.WithLabelValues(nbdb.ACLTable).Set(float64(aclCount))
		addressSetCount = libovsdbutil.GetAddressSetCount(ovnNBClient, libovsdbops.AddressSetBaselineAdminNetworkPolicy, controllerName)
		metricBANPDBObjects.WithLabelValues(nbdb.AddressSetTable).Set(float64(addressSetCount))
	}
}

// RunTimestamp adds a goroutine that registers and updates timestamp metrics.
// This is so we can determine 'freshness' of the components NB/SB DB and northd.
// Function must be called once.
func RunTimestamp(stopChan <-chan struct{}, sbClient, nbClient libovsdbclient.Client) {
	// Metric named nb_e2e_timestamp is the UNIX timestamp this instance wrote to NB DB. Updated every 30s with the
	// current timestamp.
	prometheus.MustRegister(metricNbE2eTimestamp)

	// Metric named sb_e2e_timestamp is the UNIX timestamp observed in SB DB. The value is read from the SB DB
	// cache when metrics HTTP endpoint is scraped.
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: MetricOvnkubeNamespace,
			Subsystem: MetricOvnkubeSubsystemController,
			Name:      "sb_e2e_timestamp",
			Help:      "The current e2e-timestamp value as observed in the southbound database",
		}, func() float64 {
			return getGlobalOptionsValue(sbClient, globalOptionsTimestampField)
		}))

	// Metric named e2e_timestamp is the UNIX timestamp observed in NB and SB DBs cache with the DB name
	// (OVN_Northbound|OVN_Southbound) set as a label. Updated every 30s.
	prometheus.MustRegister(metricDbTimestamp)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				currentTime := time.Now().Unix()
				if setNbE2eTimestamp(nbClient, currentTime) {
					metricNbE2eTimestamp.Set(float64(currentTime))
				} else {
					metricNbE2eTimestamp.Set(0)
				}

				metricDbTimestamp.WithLabelValues(nbClient.Schema().Name).Set(getGlobalOptionsValue(nbClient, globalOptionsTimestampField))
				metricDbTimestamp.WithLabelValues(sbClient.Schema().Name).Set(getGlobalOptionsValue(sbClient, globalOptionsTimestampField))
			case <-stopChan:
				return
			}
		}
	}()
}

// RecordPodCreated extracts the scheduled timestamp and records how long it took
// us to notice this and set up the pod's scheduling.
func RecordPodCreated(pod *kapi.Pod, netInfo util.NetInfo) {
	if netInfo.IsSecondary() {
		// TBD: no op for secondary network for now, TBD
		return
	}
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

// RecordEgressIPAssign records how long it took EgressIP to configure OVN.
func RecordEgressIPAssign(duration time.Duration) {
	metricEgressIPAssignLatency.Observe(duration.Seconds())
}

// RecordEgressIPUnassign records how long it took EgressIP to unconfigure OVN.
func RecordEgressIPUnassign(duration time.Duration) {
	metricEgressIPUnassignLatency.Observe(duration.Seconds())
}

func RecordNetpolEvent(eventName string, duration time.Duration) {
	metricNetpolEventLatency.WithLabelValues(eventName).Observe(duration.Seconds())
}

func RecordNetpolLocalPodEvent(eventName string, duration time.Duration) {
	metricNetpolLocalPodEventLatency.WithLabelValues(eventName).Observe(duration.Seconds())
}

func RecordNetpolPeerNamespaceEvent(eventName string, duration time.Duration) {
	metricNetpolPeerNamespaceEventLatency.WithLabelValues(eventName).Observe(duration.Seconds())
}

func RecordPodSelectorAddrSetPodEvent(eventName string, duration time.Duration) {
	metricPodSelectorAddrSetPodEventLatency.WithLabelValues(eventName).Observe(duration.Seconds())
}

func RecordPodSelectorAddrSetNamespaceEvent(eventName string, duration time.Duration) {
	metricPodSelectorAddrSetNamespaceEventLatency.WithLabelValues(eventName).Observe(duration.Seconds())
}

func RecordPodEvent(eventName string, duration time.Duration) {
	metricPodEventLatency.WithLabelValues(eventName).Observe(duration.Seconds())
}

// UpdateEgressFirewallRuleCount records the number of Egress firewall rules.
func UpdateEgressFirewallRuleCount(count float64) {
	metricEgressFirewallRuleCount.Add(count)
}

// RecordEgressRoutingViaHost records the egress gateway mode of the cluster
// The values are:
// 0: If it is shared gateway mode
// 1: If it is local gateway mode
// 2: invalid mode
func RecordEgressRoutingViaHost() {
	if config.Gateway.Mode == config.GatewayModeLocal {
		// routingViaHost is enabled
		metricEgressRoutingViaHost.Set(1)
	} else if config.Gateway.Mode == config.GatewayModeShared {
		// routingViaOVN is enabled
		metricEgressRoutingViaHost.Set(0)
	} else {
		// invalid mode
		metricEgressRoutingViaHost.Set(2)
	}
}

// MonitorIPSec will register a metric to determine if IPSec is enabled/disabled. It will also add a handler
// to NB libovsdb cache to update the IPSec metric.
// This function should only be called once.
func MonitorIPSec(ovnNBClient libovsdbclient.Client) {
	prometheus.MustRegister(metricIPsecEnabled)
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

// IncrementANPCount increments the number of Admin Network Policies
func IncrementANPCount() {
	metricANPCount.Inc()
}

// DecrementANPCount decrements the number of Admin Network Policies
func DecrementANPCount() {
	metricANPCount.Dec()
}

// IncrementBANPCount increments the number of Baseline Admin Network Policies
func IncrementBANPCount() {
	metricBANPCount.Inc()
}

// DecrementBANPCount decrements the number of Baseline Admin Network Policies
func DecrementBANPCount() {
	metricBANPCount.Dec()
}

type (
	timestampType int
	operation     int
)

const (
	// pod event first handled by OVN-Kubernetes control plane
	firstSeen timestampType = iota
	// OVN-Kubernetes control plane created Logical Switch Port in northbound database
	logicalSwitchPort
	// port binding seen in OVN-Kubernetes control plane southbound database libovsdb cache
	portBinding
	// port binding with updated chassis seen in OVN-Kubernetes control plane southbound database libovsdb cache
	portBindingChassis
	// queue operations
	addPortBinding operation = iota
	updatePortBinding
	addPod
	cleanPod
	addLogicalSwitchPort
	queueCheckPeriod = time.Millisecond * 50
	// prevent OOM by limiting queue size
	queueLimit       = 10000
	portBindingTable = "Port_Binding"
)

type record struct {
	timestamp time.Time
	timestampType
}

type item struct {
	op        operation
	timestamp time.Time
	old       model.Model
	new       model.Model
	uid       kapimtypes.UID
}

type PodRecorder struct {
	records map[kapimtypes.UID]*record
	queue   workqueue.TypedInterface[*item]
}

func NewPodRecorder() PodRecorder {
	return PodRecorder{}
}

var podRecorderRegOnce sync.Once

// Run monitors pod setup latency
func (pr *PodRecorder) Run(sbClient libovsdbclient.Client, stop <-chan struct{}) {
	podRecorderRegOnce.Do(func() {
		prometheus.MustRegister(metricFirstSeenLSPLatency)
		prometheus.MustRegister(metricLSPPortBindingLatency)
		prometheus.MustRegister(metricPortBindingUpLatency)
		prometheus.MustRegister(metricPortBindingChassisLatency)
	})

	pr.queue = workqueue.NewTyped[*item]()
	pr.records = make(map[kapimtypes.UID]*record)

	sbClient.Cache().AddEventHandler(&cache.EventHandlerFuncs{
		AddFunc: func(table string, model model.Model) {
			if table != portBindingTable {
				return
			}
			if !pr.queueFull() {
				pr.queue.Add(&item{op: addPortBinding, old: model, timestamp: time.Now()})
			}
		},
		UpdateFunc: func(table string, old model.Model, new model.Model) {
			if table != portBindingTable {
				return
			}
			oldRow := old.(*sbdb.PortBinding)
			newRow := new.(*sbdb.PortBinding)
			// chassis assigned
			if oldRow.Chassis == nil && newRow.Chassis != nil {
				if !pr.queueFull() {
					pr.queue.Add(&item{op: updatePortBinding, old: old, new: new, timestamp: time.Now()})
				}
				// port binding up
			} else if oldRow.Up != nil && !*oldRow.Up && newRow.Up != nil && *newRow.Up {
				if !pr.queueFull() {
					pr.queue.Add(&item{op: updatePortBinding, old: old, new: new, timestamp: time.Now()})
				}
			}
		},
		DeleteFunc: func(table string, model model.Model) {
		},
	})

	go func() {
		wait.Until(pr.runWorker, queueCheckPeriod, stop)
		pr.queue.ShutDown()
	}()
}

func (pr *PodRecorder) AddPod(podUID kapimtypes.UID) {
	if pr.queue != nil && !pr.queueFull() {
		pr.queue.Add(&item{op: addPod, uid: podUID, timestamp: time.Now()})
	}
}

func (pr *PodRecorder) CleanPod(podUID kapimtypes.UID) {
	if pr.queue != nil && !pr.queueFull() {
		pr.queue.Add(&item{op: cleanPod, uid: podUID})
	}
}

func (pr *PodRecorder) AddLSP(podUID kapimtypes.UID, netInfo util.NetInfo) {
	if netInfo.IsSecondary() {
		// TBD: no op for secondary network for now, TBD
		return
	}
	if pr.queue != nil && !pr.queueFull() {
		pr.queue.Add(&item{op: addLogicalSwitchPort, uid: podUID, timestamp: time.Now()})
	}
}

func (pr *PodRecorder) addLSP(podUID kapimtypes.UID, t time.Time) {
	var r *record
	if r = pr.getRecord(podUID); r == nil {
		klog.V(5).Infof("Add Logical Switch Port event expected pod with UID %q in cache", podUID)
		return
	}
	if r.timestampType != firstSeen {
		klog.V(5).Infof("Unexpected last event type (%d) in cache for pod with UID %q", r.timestampType, podUID)
		return
	}
	metricFirstSeenLSPLatency.Observe(t.Sub(r.timestamp).Seconds())
	r.timestamp = t
	r.timestampType = logicalSwitchPort
}

func (pr *PodRecorder) addPortBinding(m model.Model, t time.Time) {
	var r *record
	row := m.(*sbdb.PortBinding)
	podUID := getPodUIDFromPortBinding(row)
	if podUID == "" {
		return
	}
	if r = pr.getRecord(podUID); r == nil {
		klog.V(5).Infof("Add port binding event expected pod with UID %q in cache", podUID)
		return
	}
	if r.timestampType != logicalSwitchPort {
		klog.V(5).Infof("Unexpected last event entry (%d) in cache for pod with UID %q", r.timestampType, podUID)
		return
	}
	metricLSPPortBindingLatency.Observe(t.Sub(r.timestamp).Seconds())
	r.timestamp = t
	r.timestampType = portBinding
}

func (pr *PodRecorder) updatePortBinding(old, new model.Model, t time.Time) {
	var r *record
	oldRow := old.(*sbdb.PortBinding)
	newRow := new.(*sbdb.PortBinding)
	podUID := getPodUIDFromPortBinding(newRow)
	if podUID == "" {
		return
	}
	if r = pr.getRecord(podUID); r == nil {
		klog.V(5).Infof("Port binding update expected pod with UID %q in cache", podUID)
		return
	}

	if oldRow.Chassis == nil && newRow.Chassis != nil && r.timestampType == portBinding {
		metricPortBindingChassisLatency.Observe(t.Sub(r.timestamp).Seconds())
		r.timestamp = t
		r.timestampType = portBindingChassis

	}

	if oldRow.Up != nil && !*oldRow.Up && newRow.Up != nil && *newRow.Up && r.timestampType == portBindingChassis {
		metricPortBindingUpLatency.Observe(t.Sub(r.timestamp).Seconds())
		delete(pr.records, podUID)
	}
}

func (pr *PodRecorder) queueFull() bool {
	return pr.queue.Len() >= queueLimit
}

func (pr *PodRecorder) runWorker() {
	for pr.processNextItem() {
	}
}

func (pr *PodRecorder) processNextItem() bool {
	i, term := pr.queue.Get()
	if term {
		return false
	}
	pr.processItem(i)
	pr.queue.Done(i)
	return true
}

func (pr *PodRecorder) processItem(i *item) {
	switch i.op {
	case addPortBinding:
		pr.addPortBinding(i.old, i.timestamp)
	case updatePortBinding:
		pr.updatePortBinding(i.old, i.new, i.timestamp)
	case addPod:
		pr.records[i.uid] = &record{timestamp: i.timestamp, timestampType: firstSeen}
	case cleanPod:
		delete(pr.records, i.uid)
	case addLogicalSwitchPort:
		pr.addLSP(i.uid, i.timestamp)
	}
}

// getRecord returns record from map with func argument as the key
func (pr *PodRecorder) getRecord(podUID kapimtypes.UID) *record {
	r, ok := pr.records[podUID]
	if !ok {
		klog.V(5).Infof("Cache entry expected pod with UID %q but failed to find it", podUID)
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

const (
	updateOVNMeasurementChSize = 500
	deleteOVNMeasurementChSize = 50
	processChSize              = 1000
	nbGlobalTable              = "NB_Global"
	//fixme: remove when bug is fixed in OVN (Red Hat bugzilla bug number 2074019). Also, handle overflow event.
	maxNbCfg               = math.MaxUint32 - 1000
	maxMeasurementLifetime = 20 * time.Minute
)

type ovnMeasurement struct {
	// time just before ovsdb tx is called
	startTimestamp time.Time
	// time when the nbCfg value and its associated configuration is applied to all nodes
	endTimestamp time.Time
	// OVN measurement complete - start and end timestamps are valid
	complete bool
	// nb_cfg value that started the measurement
	nbCfg int
}

// measurement stores a measurement attempt through OVN-Kubernetes controller and optionally OVN
type measurement struct {
	// kubernetes kind e.g. pod or service
	kind string
	// time when Add is executed
	startTimestamp time.Time
	// time when End is executed
	endTimestamp time.Time
	// if true endTimestamp is valid
	end bool
	// time when this measurement expires. Set during Add
	expiresAt time.Time
	// OVN measurement(s) via AddOVN
	ovnMeasurements []ovnMeasurement
}

// hvCfgUpdate holds the information received from OVN Northbound event handler
type hvCfgUpdate struct {
	// timestamp is in milliseconds
	timestamp int
	hvCfg     int
}

type ConfigDurationRecorder struct {
	// rate at which measurements are allowed. Probabilistically, 1 in every measurementRate
	measurementRate uint64
	measurements    map[string]measurement
	// controls RW access to measurements map
	measurementsMu sync.RWMutex
	// channel to trigger processing a measurement following call to End func. Channel string is kind/namespace/name
	triggerProcessCh chan string
	enabled          bool
}

// global variable is needed because this functionality is accessed in many functions
var cdr *ConfigDurationRecorder

// lock for accessing the cdr global variable
var cdrMutex sync.Mutex

func GetConfigDurationRecorder() *ConfigDurationRecorder {
	cdrMutex.Lock()
	defer cdrMutex.Unlock()
	if cdr == nil {
		cdr = &ConfigDurationRecorder{}
	}
	return cdr
}

var configDurationRegOnce sync.Once

// Run monitors the config duration for OVN-Kube master to configure k8 kinds. A measurement maybe allowed and this is
// related to the number of k8 nodes, N [1] and by argument k [2] where there is a probability that 1 out of N*k
// measurement attempts are allowed. If k=0, all measurements are allowed. mUpdatePeriod determines the period to
// process and publish metrics
// [1] 1<N<inf, [2] 0<=k<inf
func (cr *ConfigDurationRecorder) Run(nbClient libovsdbclient.Client, kube kube.Interface, k float64,
	workerLoopPeriod time.Duration, stop <-chan struct{}) {
	// ** configuration duration recorder - intro **
	// We measure the duration to configure whatever k8 kind (pod, services, etc.) object and optionally its application
	// to all nodes. Metrics record this duration. This will give a rough upper bound of how long it takes OVN-Kubernetes
	// controller container (CMS) and optionally (if AddOVN is called), OVN to configure all nodes under its control.
	// Not every attempt to record will result in a measurement if argument k > 0. The measurement rate is proportional to
	// the number of nodes, N and argument k. 1 out of every N*k attempted measurements will succeed.

	// For the optional OVN measurement by calling AddOVN, when the CMS is about to make a transaction to configure
	// whatever kind, a call to AddOVN function allows the caller to measure OVN duration.
	// An ovsdb operation is returned to the caller of AddOVN, which they can bundle with their existing transactions
	// sent to OVN which will tell OVN to measure how long it takes to configure all nodes with the config in the transaction.
	// Config duration then waits for OVN to configure all nodes and calculates the time delta.

	// ** configuration duration recorder - caveats **
	// For the optional OVN recording, it does not give you an exact time duration for how long it takes to configure your
	// k8 kind. When you are recording how long it takes OVN to complete your configuration to all nodes, other
	// transactions may have occurred which may increases the overall time. You may also get longer processing times if one
	// or more nodes are unavailable because we are measuring how long the functionality takes to apply to ALL nodes.

	// ** configuration duration recorder - How the duration of the config is measured within OVN **
	// We increment the nb_cfg integer value in the NB_Global table.
	// ovn-northd notices the nb_cfg change and copies the nb_cfg value to SB_Global table field nb_cfg along with any
	// other configuration that is changed in OVN Northbound database.
	// All ovn-controllers detect nb_cfg value change and generate a 'barrier' on the openflow connection to the
	// nodes ovs-vswitchd. Once ovn-controllers receive the 'barrier processed' reply from ovs-vswitchd which
	// indicates that all relevant openflow operations associated with NB_Globals nb_cfg value have been
	// propagated to the nodes OVS, it copies the SB_Global nb_cfg value to its Chassis_Private table nb_cfg record.
	// ovn-northd detects changes to the Chassis_Private startRecords and computes the minimum nb_cfg for all Chassis_Private
	// nb_cfg and stores this in NB_Global hv_cfg field along with a timestamp to field hv_cfg_timestamp which
	// reflects the time when the slowest chassis catches up with the northbound configuration.
	configDurationRegOnce.Do(func() {
		prometheus.MustRegister(metricNetworkProgramming)
		prometheus.MustRegister(metricNetworkProgrammingOVN)
	})

	cr.measurements = make(map[string]measurement)
	// watch node count and adjust measurement rate if node count changes
	cr.runMeasurementRateAdjuster(kube, k, time.Hour, stop)
	// we currently do not clean the following channels up upon exit
	cr.triggerProcessCh = make(chan string, processChSize)
	updateOVNMeasurementCh := make(chan hvCfgUpdate, updateOVNMeasurementChSize)
	deleteOVNMeasurementCh := make(chan int, deleteOVNMeasurementChSize)
	go cr.processMeasurements(workerLoopPeriod, updateOVNMeasurementCh, deleteOVNMeasurementCh, stop)

	nbClient.Cache().AddEventHandler(&cache.EventHandlerFuncs{
		UpdateFunc: func(table string, old model.Model, new model.Model) {
			if table != nbGlobalTable {
				return
			}
			oldRow := old.(*nbdb.NBGlobal)
			newRow := new.(*nbdb.NBGlobal)

			if oldRow.HvCfg != newRow.HvCfg && oldRow.HvCfgTimestamp != newRow.HvCfgTimestamp && newRow.HvCfgTimestamp > 0 {
				select {
				case updateOVNMeasurementCh <- hvCfgUpdate{hvCfg: newRow.HvCfg, timestamp: newRow.HvCfgTimestamp}:
				default:
					klog.Warning("Config duration recorder: unable to update OVN measurement")
					select {
					case deleteOVNMeasurementCh <- newRow.HvCfg:
					default:
					}
				}
			}
		},
	})
	cr.enabled = true
}

// Start allows the caller to attempt measurement of a control plane configuration duration, as a metric,
// the duration between functions Start and End. Optionally, if you wish to record OVN config duration,
// call AddOVN which will add the duration for OVN to apply the configuration to all nodes.
// The caller must pass kind,namespace,name which will be used to determine if the object
// is allowed to record. To allow no locking, each go routine that calls this function, can determine itself
// if it is allowed to measure.
// There is a mandatory two-step process to complete a measurement.
// Step 1) Call Start when you wish to begin a measurement - ideally when processing for the object starts
// Step 2) Call End which will complete a measurement
// Optionally, call AddOVN when you are making a transaction to OVN in order to add on the OVN duration to an existing
// measurement. This must be called between Start and End. Not every call to Start will result in a measurement
// and the rate of measurements depends on the number of nodes and function Run arg k.
// Only one measurement for a kind/namespace/name is allowed until the current measurement is Ended (via End) and
// processed. This is guaranteed by workqueues (even with multiple workers) and informer event handlers.
func (cr *ConfigDurationRecorder) Start(kind, namespace, name string) (time.Time, bool) {
	if !cr.enabled {
		return time.Time{}, false
	}
	kindNamespaceName := fmt.Sprintf("%s/%s/%s", kind, namespace, name)
	if !cr.allowedToMeasure(kindNamespaceName) {
		return time.Time{}, false
	}
	measurementTimestamp := time.Now()
	cr.measurementsMu.Lock()
	_, found := cr.measurements[kindNamespaceName]
	// we only record for measurements that aren't in-progress
	if !found {
		cr.measurements[kindNamespaceName] = measurement{kind: kind, startTimestamp: measurementTimestamp,
			expiresAt: measurementTimestamp.Add(maxMeasurementLifetime)}
	}
	cr.measurementsMu.Unlock()
	return measurementTimestamp, !found
}

// allowedToMeasure determines if we are allowed to measure or not. To avoid the cost of synchronisation by using locks,
// we use probability. For a value of kindNamespaceName that returns true, it will always return true.
func (cr *ConfigDurationRecorder) allowedToMeasure(kindNamespaceName string) bool {
	if cr.measurementRate == 0 {
		return true
	}
	// 1 in measurementRate chance of true
	if hashToNumber(kindNamespaceName)%cr.measurementRate == 0 {
		return true
	}
	return false
}

func (cr *ConfigDurationRecorder) End(kind, namespace, name string) time.Time {
	if !cr.enabled {
		return time.Time{}
	}
	kindNamespaceName := fmt.Sprintf("%s/%s/%s", kind, namespace, name)
	if !cr.allowedToMeasure(kindNamespaceName) {
		return time.Time{}
	}
	measurementTimestamp := time.Now()
	cr.measurementsMu.Lock()
	if m, ok := cr.measurements[kindNamespaceName]; ok {
		if !m.end {
			m.end = true
			m.endTimestamp = measurementTimestamp
			cr.measurements[kindNamespaceName] = m
			// if there are no OVN measurements, trigger immediate processing
			if len(m.ovnMeasurements) == 0 {
				select {
				case cr.triggerProcessCh <- kindNamespaceName:
				default:
					// doesn't matter if channel is full because the measurement will be processed later anyway
				}
			}
		}
	} else {
		// This can happen if Start was rejected for a resource because a measurement was in-progress for this
		// kind/namespace/name, but during execution of this resource, the measurement was completed and now no record
		// is found.
		measurementTimestamp = time.Time{}
	}
	cr.measurementsMu.Unlock()
	return measurementTimestamp
}

// AddOVN adds OVN config duration to an existing recording - previously started by calling function Start
// It will return ovsdb operations which a user can add to existing operations they wish to track.
// Upon successful transaction of the operations to the ovsdb server, the user of this function must call a call-back
// function to lock-in the request to measure and report. Failure to call the call-back function, will result in no OVN
// measurement and no metrics are reported. AddOVN will result in a no-op if Start isn't called previously for the same
// kind/namespace/name.
// If multiple AddOVN is called between Start and End for the same kind/namespace/name, then the
// OVN durations will be summed and added to the total. There is an assumption that processing of kind/namespace/name is
// sequential
func (cr *ConfigDurationRecorder) AddOVN(nbClient libovsdbclient.Client, kind, namespace, name string) (
	[]ovsdb.Operation, func(), time.Time, error) {
	if !cr.enabled {
		return []ovsdb.Operation{}, func() {}, time.Time{}, nil
	}
	kindNamespaceName := fmt.Sprintf("%s/%s/%s", kind, namespace, name)
	if !cr.allowedToMeasure(kindNamespaceName) {
		return []ovsdb.Operation{}, func() {}, time.Time{}, nil
	}
	cr.measurementsMu.RLock()
	m, ok := cr.measurements[kindNamespaceName]
	cr.measurementsMu.RUnlock()
	if !ok {
		// no measurement found, therefore no-op
		return []ovsdb.Operation{}, func() {}, time.Time{}, nil
	}
	if m.end {
		// existing measurement in-progress and not processed yet, therefore no-op
		return []ovsdb.Operation{}, func() {}, time.Time{}, nil
	}
	nbGlobal := &nbdb.NBGlobal{}
	nbGlobal, err := libovsdbops.GetNBGlobal(nbClient, nbGlobal)
	if err != nil {
		return []ovsdb.Operation{}, func() {}, time.Time{}, fmt.Errorf("failed to find OVN Northbound NB_Global table"+
			" entry: %v", err)
	}
	if nbGlobal.NbCfg < 0 {
		return []ovsdb.Operation{}, func() {}, time.Time{}, fmt.Errorf("nb_cfg is negative, failed to add OVN measurement")
	}
	//stop recording if we are close to overflow
	if nbGlobal.NbCfg > maxNbCfg {
		return []ovsdb.Operation{}, func() {}, time.Time{}, fmt.Errorf("unable to measure OVN due to nb_cfg being close to overflow")
	}
	ops, err := nbClient.Where(nbGlobal).Mutate(nbGlobal, model.Mutation{
		Field:   &nbGlobal.NbCfg,
		Mutator: ovsdb.MutateOperationAdd,
		Value:   1,
	})
	if err != nil {
		return []ovsdb.Operation{}, func() {}, time.Time{}, fmt.Errorf("failed to create update operation: %v", err)
	}
	ovnStartTimestamp := time.Now()

	return ops, func() {
		// there can be a race condition here where we queue the wrong nbCfg value, but it is ok as long as it is
		// less than or equal the hv_cfg value we see and this is the case because of atomic increments for nb_cfg
		cr.measurementsMu.Lock()
		m, ok = cr.measurements[kindNamespaceName]
		if !ok {
			klog.Errorf("Config duration recorder: expected a measurement entry. Call Start before AddOVN"+
				" for %s", kindNamespaceName)
			cr.measurementsMu.Unlock()
			return
		}
		m.ovnMeasurements = append(m.ovnMeasurements, ovnMeasurement{startTimestamp: ovnStartTimestamp,
			nbCfg: nbGlobal.NbCfg + 1})
		cr.measurements[kindNamespaceName] = m
		cr.measurementsMu.Unlock()
	}, ovnStartTimestamp, nil
}

// runMeasurementRateAdjuster will adjust the rate of measurements based on the number of nodes in the cluster and arg k
func (cr *ConfigDurationRecorder) runMeasurementRateAdjuster(kube kube.Interface, k float64, nodeCheckPeriod time.Duration,
	stop <-chan struct{}) {
	var currentMeasurementRate, newMeasurementRate uint64

	updateMeasurementRate := func() {
		if nodeCount, err := getNodeCount(kube); err != nil {
			klog.Errorf("Config duration recorder: failed to update ticker duration considering node count: %v", err)
		} else {
			newMeasurementRate = uint64(math.Round(k * float64(nodeCount)))
			if newMeasurementRate != currentMeasurementRate {
				if newMeasurementRate > 0 {
					currentMeasurementRate = newMeasurementRate
					cr.measurementRate = newMeasurementRate
				}
				klog.V(5).Infof("Config duration recorder: updated measurement rate to approx 1 in"+
					" every %d requests", newMeasurementRate)
			}
		}
	}

	// initial measurement rate adjustment
	updateMeasurementRate()

	go func() {
		nodeCheckTicker := time.NewTicker(nodeCheckPeriod)
		for {
			select {
			case <-nodeCheckTicker.C:
				updateMeasurementRate()
			case <-stop:
				nodeCheckTicker.Stop()
				return
			}
		}
	}()
}

// processMeasurements manages the measurements map. It calculates metrics and cleans up finished or stale measurements
func (cr *ConfigDurationRecorder) processMeasurements(period time.Duration, updateOVNMeasurementCh chan hvCfgUpdate,
	deleteOVNMeasurementCh chan int, stop <-chan struct{}) {
	ticker := time.NewTicker(period)
	var ovnKDelta, ovnDelta float64

	for {
		select {
		case <-stop:
			ticker.Stop()
			return
		// remove measurements if channel updateOVNMeasurementCh overflows, therefore we cannot trust existing measurements
		case hvCfg := <-deleteOVNMeasurementCh:
			cr.measurementsMu.Lock()
			removeOVNMeasurements(cr.measurements, hvCfg)
			cr.measurementsMu.Unlock()
		case h := <-updateOVNMeasurementCh:
			cr.measurementsMu.Lock()
			cr.addHvCfg(h.hvCfg, h.timestamp)
			cr.measurementsMu.Unlock()
		// used for processing measurements that didn't require OVN measurement. Helps to keep measurement map small
		case kindNamespaceName := <-cr.triggerProcessCh:
			cr.measurementsMu.Lock()
			m, ok := cr.measurements[kindNamespaceName]
			if !ok {
				klog.Errorf("Config duration recorder: expected measurement, but not found")
				cr.measurementsMu.Unlock()
				continue
			}
			if !m.end {
				cr.measurementsMu.Unlock()
				continue
			}
			if len(m.ovnMeasurements) != 0 {
				cr.measurementsMu.Unlock()
				continue
			}
			ovnKDelta = m.endTimestamp.Sub(m.startTimestamp).Seconds()
			metricNetworkProgramming.With(prometheus.Labels{"kind": m.kind}).Observe(ovnKDelta)
			klog.V(5).Infof("Config duration recorder: kind/namespace/name %s. OVN-Kubernetes controller took %v"+
				" seconds. No OVN measurement.", kindNamespaceName, ovnKDelta)
			delete(cr.measurements, kindNamespaceName)
			cr.measurementsMu.Unlock()
		// used for processing measurements that require OVN measurement or do not or are expired.
		case <-ticker.C:
			start := time.Now()
			cr.measurementsMu.Lock()
			// process and clean up measurements
			for kindNamespaceName, m := range cr.measurements {
				if start.After(m.expiresAt) {
					// measurement may expire if OVN is degraded or End wasn't called
					klog.Warningf("Config duration recorder: measurement expired for %s", kindNamespaceName)
					delete(cr.measurements, kindNamespaceName)
					continue
				}
				if !m.end {
					// measurement didn't end yet, process later
					continue
				}
				// for when no ovn measurements requested
				if len(m.ovnMeasurements) == 0 {
					ovnKDelta = m.endTimestamp.Sub(m.startTimestamp).Seconds()
					metricNetworkProgramming.With(prometheus.Labels{"kind": m.kind}).Observe(ovnKDelta)
					klog.V(5).Infof("Config duration recorder: kind/namespace/name %s. OVN-Kubernetes controller"+
						" took %v seconds. No OVN measurement.", kindNamespaceName, ovnKDelta)
					delete(cr.measurements, kindNamespaceName)
					continue
				}
				// for each kind/namespace/name, there can be multiple calls to AddOVN between start and end
				// we sum all the OVN durations and add it to the start and end duration
				// first lets make sure all OVN measurements are finished
				if complete := allOVNMeasurementsComplete(m.ovnMeasurements); !complete {
					continue
				}

				ovnKDelta = m.endTimestamp.Sub(m.startTimestamp).Seconds()
				ovnDelta = calculateOVNDuration(m.ovnMeasurements)
				metricNetworkProgramming.With(prometheus.Labels{"kind": m.kind}).Observe(ovnKDelta + ovnDelta)
				metricNetworkProgrammingOVN.Observe(ovnDelta)
				klog.V(5).Infof("Config duration recorder: kind/namespace/name %s. OVN-Kubernetes controller took"+
					" %v seconds. OVN took %v seconds. Total took %v seconds", kindNamespaceName, ovnKDelta,
					ovnDelta, ovnDelta+ovnKDelta)
				delete(cr.measurements, kindNamespaceName)
			}
			cr.measurementsMu.Unlock()
		}
	}
}

func (cr *ConfigDurationRecorder) addHvCfg(hvCfg, hvCfgTimestamp int) {
	var altered bool
	for i, m := range cr.measurements {
		altered = false
		for iOvnM, ovnM := range m.ovnMeasurements {
			if ovnM.complete {
				continue
			}
			if ovnM.nbCfg <= hvCfg {
				ovnM.endTimestamp = time.UnixMilli(int64(hvCfgTimestamp))
				ovnM.complete = true
				m.ovnMeasurements[iOvnM] = ovnM
				altered = true
			}
		}
		if altered {
			cr.measurements[i] = m
		}
	}
}

// removeOVNMeasurements remove any OVN measurements less than or equal argument hvCfg
func removeOVNMeasurements(measurements map[string]measurement, hvCfg int) {
	for kindNamespaceName, m := range measurements {
		var indexToDelete []int
		for i, ovnM := range m.ovnMeasurements {
			if ovnM.nbCfg <= hvCfg {
				indexToDelete = append(indexToDelete, i)
			}
		}
		if len(indexToDelete) == 0 {
			continue
		}
		if len(indexToDelete) == len(m.ovnMeasurements) {
			delete(measurements, kindNamespaceName)
		}
		for _, iDel := range indexToDelete {
			m.ovnMeasurements = removeOVNMeasurement(m.ovnMeasurements, iDel)
		}
		measurements[kindNamespaceName] = m
	}
}

func removeOVNMeasurement(oM []ovnMeasurement, i int) []ovnMeasurement {
	oM[i] = oM[len(oM)-1]
	return oM[:len(oM)-1]
}
func hashToNumber(s string) uint64 {
	h := fnv.New64()
	h.Write([]byte(s))
	return h.Sum64()
}

func calculateOVNDuration(ovnMeasurements []ovnMeasurement) float64 {
	var totalDuration float64
	for _, oM := range ovnMeasurements {
		if !oM.complete {
			continue
		}
		totalDuration += oM.endTimestamp.Sub(oM.startTimestamp).Seconds()
	}
	return totalDuration
}

func allOVNMeasurementsComplete(ovnMeasurements []ovnMeasurement) bool {
	for _, oM := range ovnMeasurements {
		if !oM.complete {
			return false
		}
	}
	return true
}

// merge direct copy from k8 pkg/proxy/metrics/metrics.go
func merge(slices ...[]float64) []float64 {
	result := make([]float64, 1)
	for _, s := range slices {
		result = append(result, s...)
	}
	return result
}

func getNodeCount(kube kube.Interface) (int, error) {
	nodes, err := kube.GetNodes()
	if err != nil {
		return 0, fmt.Errorf("unable to retrieve node list: %v", err)
	}
	return len(nodes), nil
}

// setNbE2eTimestamp return true if setting timestamp to NB global options is successful
func setNbE2eTimestamp(ovnNBClient libovsdbclient.Client, timestamp int64) bool {
	// assumption that only first row is relevant in NB_Global table
	nbGlobal := nbdb.NBGlobal{
		Options: map[string]string{globalOptionsTimestampField: fmt.Sprintf("%d", timestamp)},
	}
	if err := libovsdbops.UpdateNBGlobalSetOptions(ovnNBClient, &nbGlobal); err != nil {
		klog.Errorf("Unable to update NB global options E2E timestamp metric err: %v", err)
		return false
	}
	return true
}

func getGlobalOptionsValue(client libovsdbclient.Client, field string) float64 {
	var options map[string]string
	dbName := client.Schema().Name
	nbGlobal := nbdb.NBGlobal{}
	sbGlobal := sbdb.SBGlobal{}

	if dbName == "OVN_Northbound" {
		if nbGlobal, err := libovsdbops.GetNBGlobal(client, &nbGlobal); err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
			klog.Errorf("Failed to get NB_Global table err: %v", err)
			return 0
		} else {
			options = nbGlobal.Options
		}
	}

	if dbName == "OVN_Southbound" {
		if sbGlobal, err := libovsdbops.GetSBGlobal(client, &sbGlobal); err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
			klog.Errorf("Failed to get SB_Global table err: %v", err)
			return 0
		} else {
			options = sbGlobal.Options
		}
	}

	if v, ok := options[field]; !ok {
		klog.V(5).Infof("Failed to find %q from %s options. This may occur at startup.", field, dbName)
		return 0
	} else {
		if value, err := strconv.ParseFloat(v, 64); err != nil {
			klog.Errorf("Failed to parse %q value to float64 err: %v", field, err)
			return 0
		} else {
			return value
		}
	}
}
